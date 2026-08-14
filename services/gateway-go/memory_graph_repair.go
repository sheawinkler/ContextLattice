package main

// This file contains the bounded graph identity repair lane.  It is deliberately
// separate from the ordinary edge API: repair is an operator workflow over an
// immutable current-state/index snapshot, while normal graph writes are a hot
// path.  The repair lane never treats the historical edge log as a document
// authority and never rewrites that log.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	memoryGraphRepairSchemaID         = "contextlattice_memory_graph_repair.v1"
	memoryGraphRepairCheckpointID     = "contextlattice_memory_graph_repair_checkpoint.v1"
	memoryGraphRepairReceiptID        = "contextlattice_memory_graph_repair_receipt.v1"
	memoryGraphRepairPlanReceiptID    = "contextlattice_memory_graph_repair_plan_receipt.v1"
	memoryGraphRepairMaxEdgeLines     = 200000
	memoryGraphRepairMaxEdgeBytes     = memoryEdgeLogMaxRecoveryBytes
	memoryGraphRepairMaxArtifactBytes = memoryEdgeLogMaxRecoveryBytes
	memoryGraphRepairMaxDocs          = 200000
	memoryGraphRepairMaxLockActions   = 64
	memoryGraphRepairDefaultChunk     = memoryGraphRepairMaxLockActions
	memoryGraphRepairDefaultStale     = 30 * 24 * time.Hour
	memoryGraphRepairStoreKind        = "memory_graph_repair"
	memoryGraphRepairSnapshotSchemaID = "contextlattice_memory_graph_repair_snapshot.v1"
	memoryGraphRepairEvidenceSchemaID = "contextlattice_memory_graph_repair_evidence.v1"
)

func readMemoryGraphRepairArtifact(path string) ([]byte, error) {
	return readOwnerOnlyBoundedFile(path, memoryGraphRepairMaxArtifactBytes)
}

func validateMemoryGraphRepairArtifactBytes(raw []byte) error {
	persistedBytes := int64(len(raw)) + 1 // every repair artifact is newline terminated
	if persistedBytes > memoryGraphRepairMaxArtifactBytes {
		return fmt.Errorf("%w: repair artifact bytes=%d cap=%d", errMemoryEdgeLogOversized, persistedBytes, memoryGraphRepairMaxArtifactBytes)
	}
	return nil
}

type memoryGraphRepairSnapshotArtifact struct {
	SchemaID string                    `json:"schema_id"`
	Version  int                       `json:"version"`
	Snapshot memoryGraphRepairSnapshot `json:"snapshot"`
}

type memoryGraphRepairEvidenceArtifact struct {
	SchemaID       string `json:"schema_id"`
	Version        int    `json:"version"`
	Project        string `json:"project"`
	SnapshotDigest string `json:"snapshot_digest"`
	// Rows is retained only for bounded migration of v1 artifacts. Version 2
	// persists the authenticated projection below and leaves Rows empty so a
	// restart does not replay the historical edge log through evidence.record.
	Rows                     []memoryGraphRepairEdgeRow            `json:"rows"`
	ProjectionDigest         string                                `json:"projection_digest,omitempty"`
	Latest                   map[string]memoryEdgeEntry            `json:"latest,omitempty"`
	RetirementSeen           map[string]bool                       `json:"retirement_seen,omitempty"`
	RepairRuns               map[string]map[string]struct{}        `json:"repair_runs,omitempty"`
	RepairActionRows         map[string]map[string]memoryEdgeEntry `json:"repair_action_rows,omitempty"`
	RollbackActionRows       map[string]map[string]memoryEdgeEntry `json:"rollback_action_rows,omitempty"`
	Digest                   string                                `json:"digest"`
	DigestAlgorithm          string                                `json:"digest_algorithm,omitempty"`
	DigestChain              string                                `json:"digest_chain,omitempty"`
	DigestChainCount         int                                   `json:"digest_chain_count,omitempty"`
	Complete                 bool                                  `json:"complete"`
	ScannedLines             int                                   `json:"scanned_lines"`
	DuplicateCount           int                                   `json:"duplicate_count"`
	InvalidCount             int                                   `json:"invalid_count"`
	UnboundCount             int                                   `json:"unbound_count"`
	BoundCount               int                                   `json:"bound_count"`
	UnboundExplicit          int                                   `json:"unbound_explicit"`
	UnboundInferred          int                                   `json:"unbound_inferred"`
	LogGeneration            uint64                                `json:"log_generation"`
	LogDigest                string                                `json:"log_digest"`
	LogContentDigest         string                                `json:"log_content_digest"`
	LogContentHashState      string                                `json:"log_content_hash_state,omitempty"`
	LogContentHashedBytes    int64                                 `json:"log_content_hashed_bytes,omitempty"`
	LogFileSize              int64                                 `json:"log_file_size"`
	LogFileIdentity          string                                `json:"log_file_identity"`
	ScannedBytes             int64                                 `json:"scanned_bytes"`
	ProjectIndex             string                                `json:"project_index,omitempty"`
	ProjectSeenEdgeIDs       map[string]struct{}                   `json:"project_seen_edge_ids,omitempty"`
	ProjectRows              int                                   `json:"project_rows,omitempty"`
	ProjectDuplicateCount    int                                   `json:"project_duplicate_count,omitempty"`
	ProjectBindingByReason   map[string]int                        `json:"project_binding_by_reason,omitempty"`
	ProjectBindingByRelation map[string]map[string]int             `json:"project_binding_by_relation,omitempty"`
	ProjectBindingByProject  map[string]map[string]int             `json:"project_binding_by_project,omitempty"`
	ProjectBoundNodeRefs     map[string]int                        `json:"project_bound_node_refs,omitempty"`
	ProjectUnboundExplicit   int                                   `json:"project_unbound_explicit,omitempty"`
	ProjectUnboundInferred   int                                   `json:"project_unbound_inferred,omitempty"`
	ProjectPriorRows         map[string]map[string]memoryEdgeEntry `json:"project_prior_rows,omitempty"`
}

func memoryGraphRepairEvidenceProjectionDigest(artifact memoryGraphRepairEvidenceArtifact) string {
	artifact.Rows = nil
	artifact.ProjectionDigest = ""
	return "sha256:" + sha256Hex(string(mustJSON(artifact)))
}

func memoryGraphRepairSnapshotArtifactPath(m *memoryStore, project, snapshotDigest string) string {
	seed := strings.ToLower(strings.TrimSpace(project)) + "\x00" + strings.TrimSpace(snapshotDigest)
	return filepath.Join(m.policy.rootPath, "_contextlattice", "memory_graph_repair_snapshot_"+sha256Hex(seed)[:32]+".json")
}

func memoryGraphRepairEvidenceArtifactPath(m *memoryStore, project, snapshotDigest string) string {
	seed := strings.ToLower(strings.TrimSpace(project)) + "\x00" + strings.TrimSpace(snapshotDigest)
	return filepath.Join(m.policy.rootPath, "_contextlattice", "memory_graph_repair_evidence_"+sha256Hex(seed)[:32]+".json")
}

func (m *memoryStore) persistMemoryGraphRepairSnapshotArtifact(snapshot memoryGraphRepairSnapshot) error {
	if m == nil || strings.TrimSpace(snapshot.Project) == "" || strings.TrimSpace(snapshot.SnapshotDigest) == "" || !snapshot.Complete {
		return errors.New("memory graph repair snapshot artifact is incomplete")
	}
	payload, err := json.Marshal(memoryGraphRepairSnapshotArtifact{SchemaID: memoryGraphRepairSnapshotSchemaID, Version: 1, Snapshot: snapshot})
	if err != nil {
		return err
	}
	if err := validateMemoryGraphRepairArtifactBytes(payload); err != nil {
		return err
	}
	return writeOwnerOnlyDurableAtomicFile(memoryGraphRepairSnapshotArtifactPath(m, snapshot.Project, snapshot.SnapshotDigest), append(payload, '\n'), true)
}

func (m *memoryStore) loadMemoryGraphRepairSnapshotArtifact(project, snapshotDigest string) (memoryGraphRepairSnapshot, bool, error) {
	if m == nil || strings.TrimSpace(project) == "" || strings.TrimSpace(snapshotDigest) == "" {
		return memoryGraphRepairSnapshot{}, false, nil
	}
	raw, err := readMemoryGraphRepairArtifact(memoryGraphRepairSnapshotArtifactPath(m, project, snapshotDigest))
	if errors.Is(err, os.ErrNotExist) {
		return memoryGraphRepairSnapshot{}, false, nil
	}
	if err != nil {
		return memoryGraphRepairSnapshot{}, false, err
	}
	var artifact memoryGraphRepairSnapshotArtifact
	if err := json.Unmarshal(raw, &artifact); err != nil {
		return memoryGraphRepairSnapshot{}, false, fmt.Errorf("decode repair snapshot artifact: %w", err)
	}
	if artifact.SchemaID != memoryGraphRepairSnapshotSchemaID || artifact.Version != 1 || !artifact.Snapshot.Complete || !strings.EqualFold(artifact.Snapshot.Project, project) || artifact.Snapshot.SnapshotDigest != snapshotDigest {
		return memoryGraphRepairSnapshot{}, false, errors.New("repair snapshot artifact contract is invalid")
	}
	computed := memoryGraphRepairSnapshotDigest(artifact.Snapshot.Docs, artifact.Snapshot.KeyGeneration, artifact.Snapshot.TopicGeneration)
	if computed != artifact.Snapshot.SnapshotDigest {
		return memoryGraphRepairSnapshot{}, false, errors.New("repair snapshot artifact digest mismatch")
	}
	if err := indexMemoryGraphRepairSnapshotDocs(&artifact.Snapshot); err != nil {
		return memoryGraphRepairSnapshot{}, false, err
	}
	return artifact.Snapshot, true, nil
}

func (m *memoryStore) persistMemoryGraphRepairEvidenceArtifact(snapshot memoryGraphRepairSnapshot, evidence memoryGraphRepairEdgeEvidence) error {
	if m == nil || strings.TrimSpace(snapshot.Project) == "" || strings.TrimSpace(snapshot.SnapshotDigest) == "" || !evidence.Complete || evidence.Project != snapshot.Project {
		return errors.New("memory graph repair evidence artifact is incomplete")
	}
	if evidence.DigestIndex == nil || evidence.DigestIndex.digest() != evidence.Digest || evidence.DigestIndex.chain == "" || evidence.DigestIndex.chainCount < 0 {
		return errors.New("memory graph repair evidence digest midstate is unavailable")
	}
	if evidence.LogContentHashState == "" || evidence.LogContentHashedBytes != evidence.LogFileSize {
		return errors.New("memory graph repair evidence content-hash midstate is unavailable")
	}
	if _, err := memoryEdgeLogHashFromState(memoryEdgeLogState{
		ContentDigest: evidence.LogContentDigest, ContentHashState: evidence.LogContentHashState,
		ContentHashedBytes: evidence.LogContentHashedBytes, FileSize: evidence.LogFileSize,
	}); err != nil {
		return fmt.Errorf("validate repair evidence content-hash midstate: %w", err)
	}
	artifact := memoryGraphRepairEvidenceArtifact{
		SchemaID: memoryGraphRepairEvidenceSchemaID, Version: 2, Project: evidence.Project, SnapshotDigest: snapshot.SnapshotDigest,
		// Version 2 is a compact authenticated projection. Historical Rows are
		// deliberately omitted; v1 rows are consumed only by the one-time legacy
		// migration path in the loader below.
		Latest: evidence.Latest, RetirementSeen: evidence.RetirementSeen, RepairRuns: evidence.RepairRuns,
		RepairActionRows: evidence.RepairActionRows, RollbackActionRows: evidence.RollbackActionRows,
		Digest: evidence.Digest, DigestAlgorithm: memoryGraphRepairDigestAlgorithm, DigestChain: evidence.DigestIndex.chain, DigestChainCount: evidence.DigestIndex.chainCount,
		Complete: evidence.Complete, ScannedLines: evidence.ScannedLines,
		DuplicateCount: evidence.DuplicateCount, InvalidCount: evidence.InvalidCount, UnboundCount: evidence.UnboundCount,
		BoundCount: evidence.BoundCount, UnboundExplicit: evidence.UnboundExplicit, UnboundInferred: evidence.UnboundInferred,
		LogGeneration: evidence.LogGeneration, LogDigest: evidence.LogDigest, LogContentDigest: evidence.LogContentDigest,
		LogContentHashState: evidence.LogContentHashState, LogContentHashedBytes: evidence.LogContentHashedBytes,
		LogFileSize: evidence.LogFileSize, LogFileIdentity: evidence.LogFileIdentity, ScannedBytes: evidence.ScannedBytes,
		ProjectIndex: evidence.Project, ProjectSeenEdgeIDs: evidence.ProjectSeenEdgeIDs, ProjectRows: evidence.ProjectRows,
		ProjectDuplicateCount: evidence.ProjectDuplicateCount, ProjectBindingByReason: evidence.ProjectBindingByReason,
		ProjectBindingByRelation: evidence.ProjectBindingByRelation, ProjectBindingByProject: evidence.ProjectBindingByProject,
		ProjectBoundNodeRefs: evidence.ProjectBoundNodeRefs, ProjectUnboundExplicit: evidence.ProjectUnboundExplicit,
		ProjectUnboundInferred: evidence.ProjectUnboundInferred, ProjectPriorRows: evidence.ProjectPriorRows,
	}
	artifact.ProjectionDigest = memoryGraphRepairEvidenceProjectionDigest(artifact)
	payload, err := json.Marshal(artifact)
	if err != nil {
		return err
	}
	if err := validateMemoryGraphRepairArtifactBytes(payload); err != nil {
		return err
	}
	return writeOwnerOnlyDurableAtomicFile(memoryGraphRepairEvidenceArtifactPath(m, snapshot.Project, snapshot.SnapshotDigest), append(payload, '\n'), true)
}

func (m *memoryStore) loadMemoryGraphRepairEvidenceArtifact(ctx context.Context, snapshot memoryGraphRepairSnapshot) (memoryGraphRepairEdgeEvidence, bool, error) {
	if m == nil || strings.TrimSpace(snapshot.Project) == "" || strings.TrimSpace(snapshot.SnapshotDigest) == "" {
		return memoryGraphRepairEdgeEvidence{}, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	raw, err := readMemoryGraphRepairArtifact(memoryGraphRepairEvidenceArtifactPath(m, snapshot.Project, snapshot.SnapshotDigest))
	if errors.Is(err, os.ErrNotExist) {
		return memoryGraphRepairEdgeEvidence{}, false, nil
	}
	if err != nil {
		return memoryGraphRepairEdgeEvidence{}, false, err
	}
	var artifact memoryGraphRepairEvidenceArtifact
	if err := json.Unmarshal(raw, &artifact); err != nil {
		return memoryGraphRepairEdgeEvidence{}, false, fmt.Errorf("decode repair evidence artifact: %w", err)
	}
	if artifact.SchemaID != memoryGraphRepairEvidenceSchemaID || (artifact.Version != 1 && artifact.Version != 2) || !artifact.Complete || !strings.EqualFold(artifact.Project, snapshot.Project) || artifact.SnapshotDigest != snapshot.SnapshotDigest || len(artifact.Rows) > memoryGraphRepairMaxEdgeLines || artifact.ScannedLines < len(artifact.Rows) || artifact.ScannedBytes < 0 || artifact.ScannedBytes > memoryGraphRepairMaxEdgeBytes || artifact.LogFileSize < 0 || artifact.LogFileSize > memoryGraphRepairMaxEdgeBytes {
		return memoryGraphRepairEdgeEvidence{}, false, errors.New("repair evidence artifact contract is invalid")
	}
	if artifact.Version == 2 {
		if len(artifact.Rows) != 0 || !memoryGraphRepairValidDigest(artifact.ProjectionDigest) || artifact.ProjectionDigest != memoryGraphRepairEvidenceProjectionDigest(artifact) || (artifact.ProjectIndex != "" && !strings.EqualFold(artifact.ProjectIndex, snapshot.Project)) || artifact.DigestAlgorithm != memoryGraphRepairDigestAlgorithm || strings.TrimSpace(artifact.DigestChain) == "" || artifact.DigestChainCount < 0 || artifact.LogContentHashState == "" || artifact.LogContentHashedBytes != artifact.LogFileSize {
			return memoryGraphRepairEdgeEvidence{}, false, errors.New("repair evidence compact midstate contract is invalid")
		}
		if _, hashErr := memoryEdgeLogHashFromState(memoryEdgeLogState{
			ContentDigest: artifact.LogContentDigest, ContentHashState: artifact.LogContentHashState,
			ContentHashedBytes: artifact.LogContentHashedBytes, FileSize: artifact.LogFileSize,
		}); hashErr != nil {
			return memoryGraphRepairEdgeEvidence{}, false, fmt.Errorf("repair evidence content-hash midstate is invalid: %w", hashErr)
		}
		evidence := memoryGraphRepairEdgeEvidence{
			Latest: artifact.Latest, RetirementSeen: artifact.RetirementSeen, RepairRuns: artifact.RepairRuns,
			RepairActionRows: artifact.RepairActionRows, RollbackActionRows: artifact.RollbackActionRows,
			Complete: true, Project: snapshot.Project,
			ProjectAliasIndex:  &memoryGraphRepairAliasIndex{Aliases: snapshot.Aliases, AmbiguousAliases: snapshot.AmbiguousAliases},
			DigestIndex:        &memoryGraphRepairDigestIndex{chain: artifact.DigestChain, chainCount: artifact.DigestChainCount, chainAvailable: true},
			ProjectSeenEdgeIDs: artifact.ProjectSeenEdgeIDs, ProjectRows: artifact.ProjectRows, ProjectDuplicateCount: artifact.ProjectDuplicateCount,
			ProjectBindingByReason: artifact.ProjectBindingByReason, ProjectBindingByRelation: artifact.ProjectBindingByRelation,
			ProjectBindingByProject: artifact.ProjectBindingByProject, ProjectBoundNodeRefs: artifact.ProjectBoundNodeRefs,
			ProjectUnboundExplicit: artifact.ProjectUnboundExplicit, ProjectUnboundInferred: artifact.ProjectUnboundInferred,
			ProjectPriorRows: artifact.ProjectPriorRows,
			ScannedLines:     artifact.ScannedLines, DuplicateCount: artifact.DuplicateCount, InvalidCount: artifact.InvalidCount,
			UnboundCount: artifact.UnboundCount, BoundCount: artifact.BoundCount, UnboundExplicit: artifact.UnboundExplicit,
			UnboundInferred: artifact.UnboundInferred, LogGeneration: artifact.LogGeneration, LogDigest: artifact.LogDigest,
			LogContentDigest: artifact.LogContentDigest, LogContentHashState: artifact.LogContentHashState,
			LogContentHashedBytes: artifact.LogContentHashedBytes, LogFileSize: artifact.LogFileSize,
			LogFileIdentity: artifact.LogFileIdentity, ScannedBytes: artifact.ScannedBytes,
		}
		evidence.ensureProjectIndexes()
		if evidence.projectDigest() != artifact.Digest || evidence.DigestIndex.digest() != artifact.Digest || artifact.DigestChain != artifact.Digest {
			return memoryGraphRepairEdgeEvidence{}, false, errors.New("repair evidence compact digest midstate mismatch")
		}
		return evidence, true, nil
	}
	evidence := memoryGraphRepairEdgeEvidence{
		Latest: map[string]memoryEdgeEntry{}, RetirementSeen: map[string]bool{}, RepairRuns: map[string]map[string]struct{}{}, RepairActionRows: map[string]map[string]memoryEdgeEntry{}, RollbackActionRows: map[string]map[string]memoryEdgeEntry{}, Complete: true,
		Project: snapshot.Project, ProjectAliasIndex: &memoryGraphRepairAliasIndex{Aliases: snapshot.Aliases, AmbiguousAliases: snapshot.AmbiguousAliases}, DigestIndex: &memoryGraphRepairDigestIndex{},
	}
	if m.memoryEdgeLogObserveIO != nil {
		m.memoryEdgeLogObserveIO("repair_evidence_legacy_replay", int64(len(artifact.Rows)))
	}
	for _, row := range artifact.Rows {
		select {
		case <-ctx.Done():
			return memoryGraphRepairEdgeEvidence{}, false, ctx.Err()
		default:
		}
		evidence.record(row)
	}
	computedDuplicateCount := evidence.DuplicateCount
	computedInvalidCount := evidence.InvalidCount
	computedUnboundCount := evidence.UnboundCount
	computedBoundCount := evidence.BoundCount
	computedUnboundExplicit := evidence.UnboundExplicit
	computedUnboundInferred := evidence.UnboundInferred
	if artifact.DuplicateCount != computedDuplicateCount || artifact.InvalidCount != computedInvalidCount || artifact.UnboundCount != computedUnboundCount || artifact.BoundCount != computedBoundCount || artifact.UnboundExplicit != computedUnboundExplicit || artifact.UnboundInferred != computedUnboundInferred {
		return memoryGraphRepairEdgeEvidence{}, false, errors.New("repair evidence artifact counters mismatch")
	}
	evidence.ScannedLines = artifact.ScannedLines
	evidence.DuplicateCount = artifact.DuplicateCount
	evidence.InvalidCount = artifact.InvalidCount
	evidence.UnboundCount = artifact.UnboundCount
	evidence.BoundCount = artifact.BoundCount
	evidence.UnboundExplicit = artifact.UnboundExplicit
	evidence.UnboundInferred = artifact.UnboundInferred
	evidence.LogGeneration = artifact.LogGeneration
	evidence.LogDigest = artifact.LogDigest
	evidence.LogContentDigest = artifact.LogContentDigest
	evidence.LogContentHashState = artifact.LogContentHashState
	evidence.LogContentHashedBytes = artifact.LogContentHashedBytes
	evidence.LogFileSize = artifact.LogFileSize
	evidence.LogFileIdentity = artifact.LogFileIdentity
	evidence.ScannedBytes = artifact.ScannedBytes
	evidence.Digest = evidence.projectDigest()
	if evidence.Digest != artifact.Digest {
		return memoryGraphRepairEdgeEvidence{}, false, errors.New("repair evidence artifact digest mismatch")
	}
	// A v1 artifact is bounded legacy input. Upgrade it immediately after the
	// single replay so every subsequent restart uses the compact authenticated
	// projection. When v1 omitted the content-hash state, recover the current
	// sidecar while the caller still holds the edge-log fence.
	if evidence.LogContentHashState == "" || evidence.LogContentHashedBytes != evidence.LogFileSize {
		if state, stateErr := m.currentMemoryEdgeLogStateLocked(memoryGraphRepairMaxEdgeBytes); stateErr == nil && state.FileSize == evidence.LogFileSize && state.ContentDigest == evidence.LogContentDigest {
			evidence.LogContentHashState = state.ContentHashState
			evidence.LogContentHashedBytes = state.ContentHashedBytes
		}
	}
	if evidence.LogContentHashState != "" && evidence.LogContentHashedBytes == evidence.LogFileSize {
		if upgradeErr := m.persistMemoryGraphRepairEvidenceArtifact(snapshot, evidence); upgradeErr != nil {
			return memoryGraphRepairEdgeEvidence{}, false, fmt.Errorf("upgrade legacy repair evidence artifact: %w", upgradeErr)
		}
	}
	return evidence, true, nil
}

type memoryGraphRepairRequest struct {
	Project              string
	DryRun               bool
	Apply                bool
	Rollback             bool
	IncludeCold          bool
	IncludeEphemeral     bool
	IncludeInferred      bool
	RetireStaleInferred  bool
	StaleAfter           time.Time
	MinConfidence        float64
	MaxCandidates        int
	ChunkSize            int
	TopicPeerLimit       int
	InferredPeerLimit    int
	InferredScanLimit    int
	InferredMinScore     float64
	InferredMinShared    int
	InferredMaxPostings  int
	CheckpointID         string
	PlanReceiptRef       string
	PlanReceiptDigest    string
	ConfirmProject       string
	OperatorConfirmed    bool
	ActorAuthority       string
	ActorPrincipalDigest string
	ActorScopeDigest     string
	ActorWorkspaceDigest string
	ActorInstallDigest   string
	ActorAuthorityDigest string
	ActorCustodyDigest   string
	PlanApplicable       bool
	ObservationAt        time.Time
}

type memoryGraphRepairError struct {
	Code string
	Err  error
}

func (e *memoryGraphRepairError) Error() string {
	if e == nil || e.Err == nil {
		return "memory graph repair failed"
	}
	return e.Err.Error()
}

func (e *memoryGraphRepairError) Unwrap() error { return e.Err }

func memoryGraphRepairErr(code string, err error) error {
	return &memoryGraphRepairError{Code: code, Err: err}
}

func memoryGraphRepairIOErr(code string, err error) error {
	var typed *memoryGraphRepairError
	if errors.As(err, &typed) {
		return err
	}
	return memoryGraphRepairErr(code, err)
}

func memoryGraphRepairPublicError(err error) (string, string) {
	var typed *memoryGraphRepairError
	if errors.As(err, &typed) && typed != nil && typed.Code != "" {
		return "memory graph repair " + strings.ReplaceAll(typed.Code, "_", " "), typed.Code
	}
	return "memory graph repair failed", "repair_failed"
}

func memoryGraphRepairHTTPStatus(code string) int {
	switch strings.TrimSpace(code) {
	case "plan_receipt_required":
		return http.StatusPreconditionRequired
	case "plan_receipt_invalid", "plan_receipt_mismatch", "plan_receipt_expired", "checkpoint_invalid", "bounded_limit", "incomplete_snapshot":
		return http.StatusUnprocessableEntity
	case "plan_receipt_conflict", "receipt_conflict", "plan_receipt_drift", "checkpoint_resume_required", "checkpoint_mismatch", "snapshot_drift", "repair_busy":
		return http.StatusConflict
	case "custody_mismatch":
		return http.StatusForbidden
	case "rollback_superseded", "rollback_checkpoint_mismatch":
		return http.StatusConflict
	case "edge_log_io", "repair_lock_io", "checkpoint_io", "receipt_io", "plan_receipt_io", "rollback_io", "provenance_invalid":
		return http.StatusInternalServerError
	default:
		return http.StatusBadGateway
	}
}

func memoryGraphRepairActorDigests(principal, project, scope string) (string, string) {
	principal = strings.TrimSpace(principal)
	if principal == "" {
		return "", ""
	}
	principalDigest := "sha256:" + sha256Hex("memory-graph-repair-principal\x00"+principal)
	if strings.TrimSpace(scope) == "" {
		scope = strings.ToLower(strings.TrimSpace(project))
	}
	scopeDigest := "sha256:" + sha256Hex("memory-graph-repair-scope\x00"+scope+"\x00"+principalDigest)
	return principalDigest, scopeDigest
}

func memoryGraphRepairAuthorityCustody(project, workspace, installation, subject, authority string) (string, string, string, string, string) {
	principal, scope := memoryGraphRepairActorDigests(subject, project, workspace+"\x00"+installation)
	workspaceDigest := "sha256:" + sha256Hex("memory-graph-repair-workspace\x00"+strings.TrimSpace(workspace))
	installationDigest := "sha256:" + sha256Hex("memory-graph-repair-installation\x00"+strings.TrimSpace(installation))
	authorityDigest := "sha256:" + sha256Hex("memory-graph-repair-authority\x00"+strings.TrimSpace(authority))
	custodyDigest := "sha256:" + sha256Hex(strings.ToLower(strings.TrimSpace(project))+"\x00"+workspaceDigest+"\x00"+installationDigest+"\x00"+principal+"\x00"+scope+"\x00"+authorityDigest)
	return principal, scope, workspaceDigest, installationDigest, custodyDigest
}

func normalizeMemoryGraphRepairRequest(payload map[string]any, policy memoryStorePolicy) (memoryGraphRepairRequest, error) {
	backfill, err := normalizeMemoryEdgeBackfillRequest(payload, policy)
	if err != nil {
		return memoryGraphRepairRequest{}, err
	}
	req := memoryGraphRepairRequest{
		Project:             backfill.Project,
		DryRun:              true,
		IncludeCold:         true,
		IncludeInferred:     true,
		RetireStaleInferred: true,
		MinConfidence:       backfill.MinConfidence,
		MaxCandidates:       backfill.MaxCandidates,
		ChunkSize:           memoryGraphRepairDefaultChunk,
		TopicPeerLimit:      backfill.TopicPeerLimit,
		InferredPeerLimit:   backfill.InferredPeerLimit,
		InferredScanLimit:   backfill.InferredScanLimit,
		InferredMinScore:    backfill.InferredMinScore,
		InferredMinShared:   backfill.InferredMinShared,
		InferredMaxPostings: backfill.InferredMaxPostings,
	}
	if req.Project == "" {
		return req, errors.New("project is required for graph repair")
	}
	if value, ok := payload["include_cold"]; ok {
		req.IncludeCold = anyToBool(value)
	}
	if value, ok := payload["include_ephemeral"]; ok {
		req.IncludeEphemeral = anyToBool(value)
	}
	if value, ok := payload["include_inferred"]; ok {
		req.IncludeInferred = anyToBool(value)
	}
	if value, ok := payload["retire_stale_inferred"]; ok {
		req.RetireStaleInferred = anyToBool(value)
	}
	if value, ok := payload["max_candidates"]; ok {
		req.MaxCandidates = clampInt(anyToInt(value, req.MaxCandidates), 1, 200000)
	}
	if value, ok := payload["chunk_size"]; ok {
		req.ChunkSize = clampInt(anyToInt(value, req.ChunkSize), 1, memoryGraphRepairMaxLockActions)
	}
	if value, ok := payload["checkpoint_id"]; ok {
		req.CheckpointID = strings.TrimSpace(anyToString(value))
	}
	if len(req.CheckpointID) > 160 {
		return req, errors.New("checkpoint_id is too long")
	}
	req.PlanReceiptRef = strings.TrimSpace(firstNonEmptyStrings(
		anyToString(payload["plan_receipt_ref"]), anyToString(payload["dry_run_receipt"]),
	))
	req.PlanReceiptDigest = strings.TrimSpace(firstNonEmptyStrings(
		anyToString(payload["plan_receipt_digest"]), anyToString(payload["dry_run_digest"]),
	))
	if len(req.PlanReceiptRef) > 160 || len(req.PlanReceiptDigest) > 160 {
		return req, errors.New("plan receipt reference or digest is too long")
	}
	req.ConfirmProject = strings.TrimSpace(firstNonEmptyStrings(
		anyToString(payload["confirm_project"]),
		anyToString(payload["operator_project"]),
	))
	req.OperatorConfirmed = anyToBool(payload["operator_confirmed"]) ||
		anyToBool(payload["operator_approved"]) || anyToBool(payload["confirmed"])
	if rawStale := firstNonEmptyStrings(anyToString(payload["stale_after"]), anyToString(payload["stale_before"])); rawStale != "" {
		parsed, ok := parseTimeBestEffort(rawStale)
		if !ok {
			return req, errors.New("stale_after must be an RFC3339 timestamp")
		}
		req.StaleAfter = parsed
	}
	if rawMode := strings.TrimSpace(strings.ToLower(anyToString(payload["mode"]))); rawMode != "" {
		switch rawMode {
		case "dry_run", "dry-run", "preview":
			req.DryRun, req.Apply = true, false
		case "apply":
			req.DryRun, req.Apply = false, true
		case "rollback":
			req.DryRun, req.Apply, req.Rollback = false, false, true
		default:
			return req, errors.New("mode must be dry_run, apply, or rollback")
		}
	} else if value, ok := payload["apply"]; ok && anyToBool(value) {
		req.DryRun, req.Apply = false, true
	} else if value, ok := payload["dry_run"]; ok {
		req.DryRun = anyToBool(value)
		req.Apply = !req.DryRun
	}
	return req, nil
}

type memoryGraphRepairDoc struct {
	Key         string
	MemoryID    string
	Project     string
	FileName    string
	TopicPath   string
	Summary     string
	EventID     string
	ObjectID    string
	ContentHash string
	ContentRef  string
	CreatedAt   string
	LastAccess  string
	AgentID     string
	SessionID   string
	Lifecycle   string
	StorageTier string
	References  []memoryStructuredReference
	UpdatedAt   time.Time
	LastTouch   time.Time
}

type memoryGraphRepairSnapshot struct {
	Project            string
	Docs               []memoryGraphRepairDoc
	Aliases            map[string]string
	AmbiguousAliases   map[string]struct{}
	SnapshotDigest     string
	CurrentStateDigest string
	KeyGeneration      uint64
	TopicGeneration    uint64
	IndexedCount       int
	ExcludedCount      int
	Complete           bool
	docsByID           map[string]memoryGraphRepairDoc
}

func indexMemoryGraphRepairSnapshotDocs(snapshot *memoryGraphRepairSnapshot) error {
	if snapshot == nil {
		return errors.New("memory graph repair snapshot is unavailable")
	}
	index := make(map[string]memoryGraphRepairDoc, len(snapshot.Docs))
	for _, doc := range snapshot.Docs {
		key := strings.ToLower(strings.TrimSpace(doc.MemoryID))
		if key == "" {
			return errors.New("memory graph repair snapshot contains an empty document identity")
		}
		if _, duplicate := index[key]; duplicate {
			return fmt.Errorf("memory graph repair snapshot contains duplicate document %q", doc.MemoryID)
		}
		doc.References = append([]memoryStructuredReference(nil), doc.References...)
		index[key] = doc
	}
	snapshot.docsByID = index
	return nil
}

func memoryGraphRepairSnapshotDoc(snapshot memoryGraphRepairSnapshot, memoryID string) (memoryGraphRepairDoc, bool) {
	key := strings.ToLower(strings.TrimSpace(memoryID))
	if snapshot.docsByID != nil {
		doc, ok := snapshot.docsByID[key]
		return doc, ok
	}
	for _, doc := range snapshot.Docs {
		if strings.EqualFold(doc.MemoryID, memoryID) {
			return doc, true
		}
	}
	return memoryGraphRepairDoc{}, false
}

type memoryGraphRepairAliasIndex struct {
	Aliases          map[string]string
	AmbiguousAliases map[string]struct{}
}

func memoryGraphRepairAlias(raw string) string {
	token := strings.TrimSpace(raw)
	for _, prefix := range []string{"memory://", "memory:", "memory/"} {
		if strings.HasPrefix(strings.ToLower(token), prefix) {
			token = strings.TrimSpace(token[len(prefix):])
			break
		}
	}
	token = strings.ReplaceAll(token, "\\", "/")
	return strings.ToLower(strings.TrimSpace(token))
}

func memoryGraphRepairCanonicalID(raw string) (string, string, string, string, error) {
	token := strings.TrimSpace(raw)
	for _, prefix := range []string{"memory://", "memory:", "memory/"} {
		if strings.HasPrefix(strings.ToLower(token), prefix) {
			token = strings.TrimSpace(token[len(prefix):])
			break
		}
	}
	token = strings.ReplaceAll(token, "\\", "/")
	project, fileName, canonical, key, err := canonicalMemoryID(token)
	if err != nil {
		return "", "", "", "", err
	}
	return project, fileName, canonical, key, nil
}

func (a *memoryGraphRepairAliasIndex) add(alias string, canonical string) {
	if a == nil || strings.TrimSpace(alias) == "" || strings.TrimSpace(canonical) == "" {
		return
	}
	alias = memoryGraphRepairAlias(alias)
	if alias == "" {
		return
	}
	if _, ambiguous := a.AmbiguousAliases[alias]; ambiguous {
		return
	}
	if previous, exists := a.Aliases[alias]; exists && previous != canonical {
		delete(a.Aliases, alias)
		a.AmbiguousAliases[alias] = struct{}{}
		return
	}
	a.Aliases[alias] = canonical
}

func (a *memoryGraphRepairAliasIndex) resolve(raw string) (string, string) {
	if a == nil {
		return "", "unknown"
	}
	alias := memoryGraphRepairAlias(raw)
	if alias == "" {
		return "", "unknown"
	}
	if _, ambiguous := a.AmbiguousAliases[alias]; ambiguous {
		return "", "ambiguous"
	}
	if canonical := a.Aliases[alias]; canonical != "" {
		return canonical, "bound"
	}
	if _, _, canonical, _, err := memoryGraphRepairCanonicalID(raw); err == nil {
		return canonical, "unbound"
	}
	return "", "unknown"
}

func memoryGraphRepairSnapshotDigest(docs []memoryGraphRepairDoc, generations ...uint64) string {
	rows := make([]map[string]any, 0, len(docs))
	for _, doc := range docs {
		row := map[string]any{
			"key":          doc.Key,
			"memory_id":    doc.MemoryID,
			"event_id":     doc.EventID,
			"object_id":    doc.ObjectID,
			"content_hash": doc.ContentHash,
			"summary_hash": sha256Hex(doc.Summary),
			"topic_path":   doc.TopicPath,
			"created_at":   doc.CreatedAt,
			"last_access":  doc.LastAccess,
			"agent_id":     doc.AgentID,
			"session_id":   doc.SessionID,
			"lifecycle":    doc.Lifecycle,
			"storage_tier": doc.StorageTier,
		}
		if len(doc.References) > 0 {
			row["references"] = doc.References
		}
		rows = append(rows, row)
	}
	digestInput := map[string]any{"docs": rows}
	if len(generations) > 0 {
		digestInput["key_generation"] = generations[0]
	}
	if len(generations) > 1 {
		digestInput["topic_generation"] = generations[1]
	}
	encoded, _ := json.Marshal(digestInput)
	return "sha256:" + sha256Hex(string(encoded))
}

func (m *memoryStore) captureMemoryGraphRepairSnapshot(ctx context.Context, req memoryGraphRepairRequest) (memoryGraphRepairSnapshot, error) {
	if m == nil || !m.isEnabled() {
		return memoryGraphRepairSnapshot{}, errors.New("go memory store is disabled")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	project, err := sanitizeMemoryProject(req.Project)
	if err != nil {
		return memoryGraphRepairSnapshot{}, err
	}
	projectKey := normalizeCurrentKeyIndexProject(project)
	snapshot := memoryGraphRepairSnapshot{
		Project:          project,
		Aliases:          map[string]string{},
		AmbiguousAliases: map[string]struct{}{},
	}
	// Test fixtures and legacy in-process callers may construct currentState
	// without the persisted cache maps. Materialize that bounded migration once
	// before taking the read snapshot; normal production stores arrive here
	// already indexed from startup.
	m.mu.Lock()
	m.ensureCurrentStateDigestIndexesLocked()
	m.mu.Unlock()
	type capturedRow struct {
		key           string
		state         memoryCurrentState
		topic         string
		topicContains bool
	}
	rows := []capturedRow{}
	m.mu.RLock()
	if m.currentKeysByProject == nil || m.currentKeyCountsByProject == nil || m.currentKeysByProjectTopic == nil || m.currentTopicKeyCountsByProject == nil || m.currentKeyIndexGeneration == nil || m.currentTopicIndexGeneration == nil || m.currentState == nil || m.latestTopic == nil {
		m.mu.RUnlock()
		return snapshot, memoryGraphRepairErr("incomplete_snapshot", errors.New("current-state/index projections are not fully materialized"))
	}
	keys, keysPresent := m.currentKeysByProject[projectKey]
	expected, countPresent := m.currentKeyCountsByProject[projectKey]
	topicKeys := m.currentKeysByProjectTopic[projectKey]
	topicExpected, topicCountPresent := m.currentTopicKeyCountsByProject[projectKey]
	keyGeneration, keyGenerationPresent := m.currentKeyIndexGeneration[projectKey]
	topicGeneration, topicGenerationPresent := m.currentTopicIndexGeneration[projectKey]
	currentStateDigest := m.currentStateDigestReadLocked(projectKey)
	if !keysPresent && !countPresent && !topicCountPresent && !keyGenerationPresent && !topicGenerationPresent {
		currentProjectRows := 0
		for key := range m.currentState {
			p, _, ok := parseMemoryStoreKeyToken(key)
			if ok && strings.EqualFold(p, project) {
				currentProjectRows++
			}
		}
		m.mu.RUnlock()
		if currentProjectRows > 0 {
			return snapshot, memoryGraphRepairErr("incomplete_snapshot", errors.New("project has current-state rows but no index projection"))
		}
		snapshot.SnapshotDigest, snapshot.Complete = memoryGraphRepairSnapshotDigest(nil, 0, 0), true
		snapshot.CurrentStateDigest = currentStateDigest
		return snapshot, nil
	}
	if expected > memoryGraphRepairMaxDocs {
		m.mu.RUnlock()
		return snapshot, memoryGraphRepairErr("bounded_limit", fmt.Errorf("current-state project index exceeds bounded repair document cap: %d", memoryGraphRepairMaxDocs))
	}
	if !keysPresent || !countPresent || !topicCountPresent || !keyGenerationPresent || !topicGenerationPresent || expected < 0 || topicExpected < 0 || len(keys) != expected || topicExpected != expected || keyGeneration != topicGeneration || (expected > 0 && len(topicKeys) == 0) {
		m.mu.RUnlock()
		return snapshot, memoryGraphRepairErr("incomplete_snapshot", errors.New("current-state/index count or generation mismatch"))
	}
	rows = make([]capturedRow, 0, len(keys))
	for key := range keys {
		state := m.currentState[key]
		topic := m.latestTopic[key]
		_, topicContains := topicKeys[currentStateTopicBucket(topic)][key]
		rows = append(rows, capturedRow{key: key, state: state, topic: topic, topicContains: topicContains})
	}
	m.mu.RUnlock()
	if m.memoryGraphRepairSnapshotCopied != nil {
		m.memoryGraphRepairSnapshotCopied()
	}

	aliases := memoryGraphRepairAliasIndex{Aliases: snapshot.Aliases, AmbiguousAliases: snapshot.AmbiguousAliases}
	for _, captured := range rows {
		select {
		case <-ctx.Done():
			return snapshot, ctx.Err()
		default:
		}
		key := captured.key
		idxProject, idxFile, ok := parseMemoryStoreKeyToken(key)
		if !ok || !strings.EqualFold(idxProject, project) {
			return snapshot, memoryGraphRepairErr("incomplete_snapshot", fmt.Errorf("invalid project index key %q", key))
		}
		canonicalProject, canonicalFile, canonicalID, canonicalKey, canonicalErr := memoryGraphRepairCanonicalID(idxProject + "::" + idxFile)
		if canonicalErr != nil || canonicalKey != key || !strings.EqualFold(canonicalProject, project) {
			return snapshot, memoryGraphRepairErr("incomplete_snapshot", fmt.Errorf("index key is not canonical: %q", key))
		}
		state, topic := captured.state, captured.topic
		if state.Entry.Project == "" || state.Tombstone || strings.TrimSpace(topic) == "" {
			return snapshot, memoryGraphRepairErr("incomplete_snapshot", fmt.Errorf("current-state row missing for %q", key))
		}
		entry := state.Entry
		entryProject, entryFile, _, entryKey, entryErr := memoryGraphRepairCanonicalID(entry.Project + "::" + entry.FileName)
		if entryErr != nil || entryKey != key || !strings.EqualFold(entryProject, project) || !strings.EqualFold(entryFile, canonicalFile) {
			return snapshot, memoryGraphRepairErr("incomplete_snapshot", fmt.Errorf("current-state identity mismatch for %q", key))
		}
		if !captured.topicContains {
			return snapshot, memoryGraphRepairErr("incomplete_snapshot", fmt.Errorf("topic index does not contain %q", key))
		}
		lifecycle := normalizeMemoryLifecycle(entry.Lifecycle)
		storageTier := normalizeMemoryStorageTier(entry.StorageTier)
		lastTouch := time.Time{}
		if parsed, ok := parseTimeBestEffort(entry.LastAccess); ok {
			lastTouch = parsed
		}
		if lastTouch.IsZero() {
			lastTouch, _ = parseTimeBestEffort(entry.CreatedAt)
		}
		if excluded, _ := m.memoryGraphArtifactExcluded(canonicalProject, canonicalFile, sanitizeTopicPath(topic, canonicalFile)); excluded ||
			!shouldSurfaceMemoryLifecycle(lifecycle, req.IncludeEphemeral) ||
			(!req.IncludeCold && (storageTier == "deep" || storageTier == "retired")) {
			snapshot.ExcludedCount++
			continue
		}
		doc := memoryGraphRepairDoc{
			Key: key, MemoryID: canonicalID, Project: canonicalProject, FileName: canonicalFile,
			TopicPath: sanitizeTopicPath(topic, canonicalFile), Summary: strings.TrimSpace(entry.Summary),
			EventID: strings.TrimSpace(entry.EventID), ObjectID: strings.TrimSpace(entry.ObjectID),
			ContentHash: strings.TrimSpace(entry.ContentHash), ContentRef: strings.TrimSpace(entry.ContentRef), CreatedAt: strings.TrimSpace(entry.CreatedAt),
			LastAccess: strings.TrimSpace(entry.LastAccess), AgentID: strings.TrimSpace(entry.AgentID),
			SessionID: strings.TrimSpace(entry.SessionID), Lifecycle: lifecycle, StorageTier: storageTier,
			References: append([]memoryStructuredReference(nil), entry.References...),
			LastTouch:  lastTouch,
		}
		if parsed, ok := parseTimeBestEffort(entry.CreatedAt); ok {
			doc.UpdatedAt = parsed
		}
		snapshot.Docs = append(snapshot.Docs, doc)
		aliases.add(canonicalID, canonicalID)
		aliases.add(key, canonicalID)
		aliases.add(canonicalProject+"/"+canonicalFile, canonicalID)
		aliases.add(entry.Project+"::"+entry.FileName, canonicalID)
		aliases.add(entry.Project+"/"+entry.FileName, canonicalID)
		aliases.add(entry.EventID, canonicalID)
		aliases.add(entry.ObjectID, canonicalID)
	}
	snapshot.KeyGeneration = keyGeneration
	snapshot.TopicGeneration = topicGeneration
	snapshot.IndexedCount = expected
	snapshot.Complete = true
	m.mu.RLock()
	currentKeyGeneration, currentKeyOK := m.currentKeyIndexGeneration[projectKey]
	currentTopicGeneration, currentTopicOK := m.currentTopicIndexGeneration[projectKey]
	currentStateDigestAfter := m.currentStateDigestReadLocked(projectKey)
	m.mu.RUnlock()
	if !currentKeyOK || !currentTopicOK || currentKeyGeneration != keyGeneration || currentTopicGeneration != topicGeneration || currentStateDigestAfter != currentStateDigest {
		return snapshot, memoryGraphRepairErr("snapshot_drift", errors.New("current-state/index generation changed during snapshot traversal"))
	}
	sort.Slice(snapshot.Docs, func(i, j int) bool {
		return strings.ToLower(snapshot.Docs[i].MemoryID) < strings.ToLower(snapshot.Docs[j].MemoryID)
	})
	if err := indexMemoryGraphRepairSnapshotDocs(&snapshot); err != nil {
		return snapshot, memoryGraphRepairErr("incomplete_snapshot", err)
	}
	snapshot.SnapshotDigest = memoryGraphRepairSnapshotDigest(snapshot.Docs, snapshot.KeyGeneration, snapshot.TopicGeneration)
	snapshot.CurrentStateDigest = currentStateDigest
	return snapshot, nil
}

type memoryGraphRepairEdgeRow struct {
	Edge       memoryEdgeEntry
	RawDigest  string
	Bound      bool
	Ambiguous  bool
	Invalid    bool
	FromMemory bool
}

type memoryGraphRepairEdgeEvidence struct {
	Rows             []memoryGraphRepairEdgeRow
	Latest           map[string]memoryEdgeEntry
	RetirementSeen   map[string]bool
	RepairRuns       map[string]map[string]struct{}
	RepairActionRows map[string]map[string]memoryEdgeEntry
	// RollbackActionRows is the incremental receipt index for rollback
	// continuations.  Recovery must resolve one action by identity; scanning
	// the complete historical edge log for every reverse action would turn a
	// 200k-action rollback into O(actions*rows).
	RollbackActionRows    map[string]map[string]memoryEdgeEntry
	Digest                string
	Complete              bool
	ScannedLines          int
	DuplicateCount        int
	InvalidCount          int
	UnboundCount          int
	BoundCount            int
	UnboundExplicit       int
	UnboundInferred       int
	LogGeneration         uint64
	LogDigest             string
	LogContentDigest      string
	LogContentHashState   string
	LogContentHashedBytes int64
	LogFileSize           int64
	LogFileIdentity       string
	ScannedBytes          int64
	// Project and the digest/binding indexes are the validated continuation
	// midstate.  They are populated only for evidence captured for one repair
	// project; keeping them on the evidence object lets a 64-action chunk copy
	// the current projection without sorting or rescanning the prefix.
	Project                  string
	DigestIndex              *memoryGraphRepairDigestIndex
	ProjectAliasIndex        *memoryGraphRepairAliasIndex
	ProjectSeenEdgeIDs       map[string]struct{}
	ProjectRows              int
	ProjectDuplicateCount    int
	ProjectBindingByReason   map[string]int
	ProjectBindingByRelation map[string]map[string]int
	ProjectBindingByProject  map[string]map[string]int
	ProjectBoundNodeRefs     map[string]int
	ProjectUnboundExplicit   int
	ProjectUnboundInferred   int
	// ProjectPriorRows is the authenticated predecessor index for repair
	// action rows. It lets crash-replay overlays remove a bounded pending
	// chunk without rebuilding the entire historical Rows slice.
	ProjectPriorRows map[string]map[string]memoryEdgeEntry
	// Continuation marks an authenticated checkpoint midstate.  Such evidence
	// contains only action identities and the suffix overlay; Rows is
	// intentionally nil so a restart cannot accidentally re-enter the full-log
	// reconstruction path.
	Continuation bool
}

func (e *memoryGraphRepairEdgeEvidence) ensureProjectIndexes() {
	if e == nil || strings.TrimSpace(e.Project) == "" {
		return
	}
	if e.Latest == nil {
		e.Latest = map[string]memoryEdgeEntry{}
	}
	if e.RetirementSeen == nil {
		e.RetirementSeen = map[string]bool{}
	}
	if e.RepairRuns == nil {
		e.RepairRuns = map[string]map[string]struct{}{}
	}
	if e.RepairActionRows == nil {
		e.RepairActionRows = map[string]map[string]memoryEdgeEntry{}
	}
	if e.RollbackActionRows == nil {
		e.RollbackActionRows = map[string]map[string]memoryEdgeEntry{}
	}
	if e.DigestIndex == nil {
		e.DigestIndex = &memoryGraphRepairDigestIndex{}
	}
	if e.ProjectSeenEdgeIDs == nil {
		e.ProjectSeenEdgeIDs = map[string]struct{}{}
	}
	if e.ProjectBindingByReason == nil {
		e.ProjectBindingByReason = memoryGraphRepairBindingBucket()
	}
	if e.ProjectBindingByRelation == nil {
		e.ProjectBindingByRelation = map[string]map[string]int{}
	}
	if e.ProjectBindingByProject == nil {
		e.ProjectBindingByProject = map[string]map[string]int{}
	}
	if e.ProjectBoundNodeRefs == nil {
		e.ProjectBoundNodeRefs = map[string]int{}
	}
	if e.ProjectPriorRows == nil {
		e.ProjectPriorRows = map[string]map[string]memoryEdgeEntry{}
	}
}

func memoryGraphRepairAdjustBindingBucket(bucket map[string]int, reason string, delta int) {
	if bucket == nil {
		return
	}
	bucket["unbound"] += delta
	if reason == "bound_current_state" {
		bucket["bound"] += delta
		bucket["unbound"] -= delta
		return
	}
	if reason == "ambiguous_alias" {
		bucket["ambiguous"] += delta
	}
	if reason == "unknown_endpoint" {
		bucket["unknown"] += delta
	}
}

func (e *memoryGraphRepairEdgeEvidence) adjustProjectBinding(edge memoryEdgeEntry, delta int) {
	if e == nil || e.ProjectAliasIndex == nil || memoryGraphRepairEdgeRetired(edge) || !memoryGraphRepairEdgeInProject(edge, e.Project) {
		return
	}
	_, sourceStatus := e.ProjectAliasIndex.resolve(edge.SourceID)
	_, targetStatus := e.ProjectAliasIndex.resolve(edge.TargetID)
	reason := memoryGraphRepairBindingReason(sourceStatus, targetStatus)
	memoryGraphRepairAdjustBindingBucket(e.ProjectBindingByReason, reason, delta)
	relation := strings.TrimSpace(edge.Relation)
	if relation == "" {
		relation = "unknown"
	}
	if e.ProjectBindingByRelation[relation] == nil {
		e.ProjectBindingByRelation[relation] = memoryGraphRepairBindingBucket()
	}
	memoryGraphRepairAdjustBindingBucket(e.ProjectBindingByRelation[relation], reason, delta)
	projectKey := strings.ToLower(strings.TrimSpace(edge.Project))
	if projectKey == "" {
		projectKey = "unknown"
	}
	if e.ProjectBindingByProject[projectKey] == nil {
		e.ProjectBindingByProject[projectKey] = memoryGraphRepairBindingBucket()
	}
	memoryGraphRepairAdjustBindingBucket(e.ProjectBindingByProject[projectKey], reason, delta)
	if reason != "bound_current_state" {
		if memoryGraphRepairEdgeIsInferred(edge) {
			e.ProjectUnboundInferred += delta
		} else {
			e.ProjectUnboundExplicit += delta
		}
	}
	if reason == "bound_current_state" {
		source, _ := e.ProjectAliasIndex.resolve(edge.SourceID)
		target, _ := e.ProjectAliasIndex.resolve(edge.TargetID)
		for _, node := range []string{source, target} {
			if strings.TrimSpace(node) == "" {
				continue
			}
			e.ProjectBoundNodeRefs[node] += delta
			if e.ProjectBoundNodeRefs[node] <= 0 {
				delete(e.ProjectBoundNodeRefs, node)
			}
		}
	}
}

func (e memoryGraphRepairEdgeEvidence) projectDigest() string {
	if e.DigestIndex != nil && strings.TrimSpace(e.Project) != "" {
		return e.DigestIndex.digest()
	}
	return memoryGraphRepairProjectEdgeDigest(e.Rows, e.Project)
}

func memoryGraphRepairEdgeIsInferred(edge memoryEdgeEntry) bool {
	if strings.EqualFold(strings.TrimSpace(edge.Relation), "inferred_related") {
		return true
	}
	if anyToBool(edge.Metadata["inferred"]) {
		return true
	}
	kind := strings.ToLower(anyToString(edge.Provenance["kind"]))
	return strings.Contains(kind, "inferred")
}

func memoryGraphRepairEdgeRetired(edge memoryEdgeEntry) bool {
	return strings.EqualFold(normalizeMemoryLifecycle(edge.Lifecycle), "retired")
}

func memoryGraphRepairEdgeMetadataRepair(edge memoryEdgeEntry, key string) string {
	return anyToString(edge.Metadata[key])
}

const (
	memoryGraphRepairSourceCorpus       = "gateway_current_state_index"
	memoryGraphRepairHistoricalEvidence = "memory_edges_historical_evidence"
	memoryGraphRepairOriginCurrentState = "generated_current_state"
	memoryGraphRepairOriginHistorical   = "preserved_historical_evidence"
)

// memoryGraphRepairClosedProvenance is attached to every append made by the
// repair lane.  The edge log is append-only, so this is the immutable custody
// boundary that says which current-state/index snapshot justified the new
// record and whether it is generated topology or preserved historical truth.
func memoryGraphRepairClosedProvenance(snapshot memoryGraphRepairSnapshot, origin string, prior memoryEdgeEntry) map[string]any {
	if origin != memoryGraphRepairOriginHistorical {
		origin = memoryGraphRepairOriginCurrentState
	}
	sourceEvidence := memoryGraphRepairSourceCorpus
	if origin == memoryGraphRepairOriginHistorical {
		sourceEvidence = memoryGraphRepairHistoricalEvidence
	}
	provenance := map[string]any{
		"repair_schema_id":       memoryGraphRepairSchemaID,
		"repair_origin":          origin,
		"source_corpus":          memoryGraphRepairSourceCorpus,
		"source_evidence":        sourceEvidence,
		"snapshot_digest":        snapshot.SnapshotDigest,
		"source_snapshot_digest": snapshot.SnapshotDigest,
		"document_set_digest":    snapshot.SnapshotDigest,
		"key_generation":         snapshot.KeyGeneration,
		"topic_generation":       snapshot.TopicGeneration,
		"closed":                 true,
	}
	if origin == memoryGraphRepairOriginHistorical && prior.EdgeID != "" {
		provenance["preserved_edge_digest"] = "sha256:" + sha256Hex(string(mustJSON(prior)))
	}
	return provenance
}

func memoryGraphRepairProvenanceClosed(edge memoryEdgeEntry, snapshot memoryGraphRepairSnapshot, kind string) bool {
	provenance := edge.Provenance
	if provenance == nil || !anyToBool(provenance["closed"]) || anyToString(provenance["repair_schema_id"]) != memoryGraphRepairSchemaID {
		return false
	}
	origin := memoryGraphRepairOriginCurrentState
	if kind == "retire" {
		origin = memoryGraphRepairOriginHistorical
	}
	if anyToString(provenance["repair_origin"]) != origin || anyToString(provenance["source_corpus"]) != memoryGraphRepairSourceCorpus || anyToString(provenance["snapshot_digest"]) != snapshot.SnapshotDigest || anyToString(provenance["source_snapshot_digest"]) != snapshot.SnapshotDigest || anyToString(provenance["document_set_digest"]) != snapshot.SnapshotDigest || anyToInt(provenance["key_generation"], -1) != int(snapshot.KeyGeneration) || anyToInt(provenance["topic_generation"], -1) != int(snapshot.TopicGeneration) {
		return false
	}
	if origin == memoryGraphRepairOriginHistorical && strings.TrimSpace(anyToString(provenance["preserved_edge_digest"])) == "" {
		return false
	}
	return true
}

func memoryGraphRepairBindingMatchesSnapshot(snapshot memoryGraphRepairSnapshot, edge memoryEdgeEntry) bool {
	if !memoryGraphEdgeRequiresBinding(edge) {
		return true
	}
	if !memoryReferenceBindingValid(edge.Binding) {
		return false
	}
	alias := &memoryGraphRepairAliasIndex{Aliases: snapshot.Aliases, AmbiguousAliases: snapshot.AmbiguousAliases}
	sourceID, sourceStatus := alias.resolve(edge.SourceID)
	targetID, targetStatus := alias.resolve(edge.TargetID)
	if sourceStatus != "bound" || targetStatus != "bound" {
		return false
	}
	sourceDoc, sourceOK := memoryGraphRepairSnapshotDoc(snapshot, sourceID)
	targetDoc, targetOK := memoryGraphRepairSnapshotDoc(snapshot, targetID)
	if !sourceOK || !targetOK {
		return false
	}
	source := memoryStoreEntry{
		EventID: sourceDoc.EventID, Project: sourceDoc.Project, FileName: sourceDoc.FileName, TopicPath: sourceDoc.TopicPath,
		ContentHash: sourceDoc.ContentHash, AgentID: sourceDoc.AgentID, SessionID: sourceDoc.SessionID,
		Lifecycle: sourceDoc.Lifecycle, References: append([]memoryStructuredReference(nil), sourceDoc.References...),
	}
	target := memoryStoreEntry{
		EventID: targetDoc.EventID, Project: targetDoc.Project, FileName: targetDoc.FileName, TopicPath: targetDoc.TopicPath,
		ContentHash: targetDoc.ContentHash, AgentID: targetDoc.AgentID, SessionID: targetDoc.SessionID, Lifecycle: targetDoc.Lifecycle,
	}
	sourceTopic := sanitizeTopicPath(source.TopicPath, source.FileName)
	targetTopic := sanitizeTopicPath(target.TopicPath, target.FileName)
	if edge.Binding.RelationSemantic != edge.Relation ||
		edge.Binding.SourceEventID != source.EventID || edge.Binding.TargetEventID != target.EventID ||
		!strings.EqualFold(strings.TrimPrefix(edge.Binding.SourceContentHash, "sha256:"), strings.TrimPrefix(source.ContentHash, "sha256:")) ||
		!strings.EqualFold(strings.TrimPrefix(edge.Binding.TargetContentHash, "sha256:"), strings.TrimPrefix(target.ContentHash, "sha256:")) ||
		!strings.EqualFold(edge.Binding.SourceTopicPath, sourceTopic) || !strings.EqualFold(edge.Binding.TargetTopicPath, targetTopic) ||
		edge.Binding.SourceSessionID != source.SessionID || edge.Binding.TargetSessionID != target.SessionID ||
		!strings.EqualFold(edge.Binding.SourceAgentID, source.AgentID) || !strings.EqualFold(edge.Binding.TargetAgentID, target.AgentID) ||
		edge.Binding.SourceLifecycle != normalizeMemoryLifecycle(source.Lifecycle) || edge.Binding.TargetLifecycle != normalizeMemoryLifecycle(target.Lifecycle) ||
		edge.Binding.SemanticDigest != memoryGraphBindingSemanticDigest(edge.Relation, source, target) {
		return false
	}
	if anyToString(edge.Metadata["claim_kind"]) == "structured_write" && !memoryReferenceClaimPresent(source, edge) {
		return false
	}
	return true
}

func memoryGraphRepairCanonicalizeEdge(edge memoryEdgeEntry, snapshot memoryGraphRepairSnapshot) (memoryGraphRepairEdgeRow, error) {
	row := memoryGraphRepairEdgeRow{RawDigest: "sha256:" + sha256Hex(string(mustJSON(edge)))}
	source, sourceStatus := (&memoryGraphRepairAliasIndex{Aliases: snapshot.Aliases, AmbiguousAliases: snapshot.AmbiguousAliases}).resolve(edge.SourceID)
	target, targetStatus := (&memoryGraphRepairAliasIndex{Aliases: snapshot.Aliases, AmbiguousAliases: snapshot.AmbiguousAliases}).resolve(edge.TargetID)
	if sourceStatus == "ambiguous" || targetStatus == "ambiguous" {
		row.Ambiguous = true
	}
	canonicalInput := edge
	if source != "" {
		canonicalInput.SourceID = source
	}
	if target != "" {
		canonicalInput.TargetID = target
	}
	normalized, err := canonicalInput.normalized()
	if err != nil {
		row.Invalid = true
		return row, err
	}
	if canonical, normErr := normalized.normalized(); normErr == nil {
		normalized = canonical
	}
	row.Edge = normalized
	if sourceStatus == "bound" && targetStatus == "bound" && memoryGraphRepairBindingMatchesSnapshot(snapshot, normalized) {
		row.Bound = true
	}
	return row, nil
}

// memoryGraphRepairDigestIndex keeps the bounded in-process AVL projection for
// full captures and an authenticated append midstate for continuation.  The
// append midstate is deliberately persisted in checkpoints: after restart a
// continuation can authenticate only the action rows and newly appended bytes
// instead of rebuilding the historical ordered index.
type memoryGraphRepairDigestIndex struct {
	root           *memoryGraphRepairDigestNode
	chain          string
	chainCount     int
	chainAvailable bool
}

type memoryGraphRepairDigestNode struct {
	key         string
	count       int
	height      int
	left, right *memoryGraphRepairDigestNode
	hash        string
}

const memoryGraphRepairDigestIndexSchema = "contextlattice_memory_graph_repair_edge_digest.append_chain.v1"

const memoryGraphRepairDigestAlgorithm = memoryGraphRepairDigestIndexSchema

func memoryGraphRepairDigestIndexKey(row memoryGraphRepairEdgeRow) string {
	item := map[string]any{
		"raw_digest": row.RawDigest, "invalid": row.Invalid, "ambiguous": row.Ambiguous,
		"bound": row.Bound, "edge": row.Edge,
	}
	raw, _ := json.Marshal(item)
	return string(raw)
}

func memoryGraphRepairDigestNodeHeight(node *memoryGraphRepairDigestNode) int {
	if node == nil {
		return 0
	}
	return node.height
}

func memoryGraphRepairDigestNodeHash(node *memoryGraphRepairDigestNode) string {
	if node == nil {
		return ""
	}
	return node.hash
}

func memoryGraphRepairDigestNodeRefresh(node *memoryGraphRepairDigestNode) {
	if node == nil {
		return
	}
	leftHash := memoryGraphRepairDigestNodeHash(node.left)
	rightHash := memoryGraphRepairDigestNodeHash(node.right)
	node.height = 1 + maxInt(memoryGraphRepairDigestNodeHeight(node.left), memoryGraphRepairDigestNodeHeight(node.right))
	node.hash = "sha256:" + sha256Hex(memoryGraphRepairDigestIndexSchema+"\x00"+leftHash+"\x00"+node.key+"\x00"+strconv.Itoa(node.count)+"\x00"+rightHash)
}

func memoryGraphRepairDigestNodeRotateRight(node *memoryGraphRepairDigestNode) *memoryGraphRepairDigestNode {
	pivot := node.left
	node.left = pivot.right
	pivot.right = node
	memoryGraphRepairDigestNodeRefresh(node)
	memoryGraphRepairDigestNodeRefresh(pivot)
	return pivot
}

func memoryGraphRepairDigestNodeRotateLeft(node *memoryGraphRepairDigestNode) *memoryGraphRepairDigestNode {
	pivot := node.right
	node.right = pivot.left
	pivot.left = node
	memoryGraphRepairDigestNodeRefresh(node)
	memoryGraphRepairDigestNodeRefresh(pivot)
	return pivot
}

func memoryGraphRepairDigestNodeBalance(node *memoryGraphRepairDigestNode) int {
	if node == nil {
		return 0
	}
	return memoryGraphRepairDigestNodeHeight(node.left) - memoryGraphRepairDigestNodeHeight(node.right)
}

func memoryGraphRepairDigestNodeInsert(node *memoryGraphRepairDigestNode, key string) *memoryGraphRepairDigestNode {
	if node == nil {
		node = &memoryGraphRepairDigestNode{key: key, count: 1, height: 1}
		memoryGraphRepairDigestNodeRefresh(node)
		return node
	}
	if key < node.key {
		node.left = memoryGraphRepairDigestNodeInsert(node.left, key)
	} else if key > node.key {
		node.right = memoryGraphRepairDigestNodeInsert(node.right, key)
	} else {
		node.count++
	}
	memoryGraphRepairDigestNodeRefresh(node)
	balance := memoryGraphRepairDigestNodeBalance(node)
	if balance > 1 && key < node.left.key {
		return memoryGraphRepairDigestNodeRotateRight(node)
	}
	if balance < -1 && key > node.right.key {
		return memoryGraphRepairDigestNodeRotateLeft(node)
	}
	if balance > 1 && key > node.left.key {
		node.left = memoryGraphRepairDigestNodeRotateLeft(node.left)
		return memoryGraphRepairDigestNodeRotateRight(node)
	}
	if balance < -1 && key < node.right.key {
		node.right = memoryGraphRepairDigestNodeRotateRight(node.right)
		return memoryGraphRepairDigestNodeRotateLeft(node)
	}
	return node
}

func (index *memoryGraphRepairDigestIndex) add(row memoryGraphRepairEdgeRow) {
	if index == nil {
		return
	}
	key := memoryGraphRepairDigestIndexKey(row)
	index.root = memoryGraphRepairDigestNodeInsert(index.root, key)
	if !index.chainAvailable {
		index.chain = "sha256:" + sha256Hex(memoryGraphRepairDigestIndexSchema+"\x00empty")
		index.chainAvailable = true
	}
	index.chain = "sha256:" + sha256Hex(memoryGraphRepairDigestIndexSchema+"\x00"+index.chain+"\x00"+key)
	index.chainCount++
}

func (index *memoryGraphRepairDigestIndex) digest() string {
	if index == nil {
		return "sha256:" + sha256Hex(memoryGraphRepairDigestIndexSchema+"\x00empty")
	}
	if index.chainAvailable {
		return index.chain
	}
	if index.root == nil {
		index.chain = "sha256:" + sha256Hex(memoryGraphRepairDigestIndexSchema+"\x00empty")
		index.chainAvailable = true
		return index.chain
	}
	return index.root.hash
}

func memoryGraphRepairEdgeDigest(rows []memoryGraphRepairEdgeRow) string {
	index := &memoryGraphRepairDigestIndex{}
	for _, row := range rows {
		index.add(row)
	}
	return index.digest()
}

func (m *memoryStore) cachedMemoryGraphRepairEvidence(snapshotDigest, project string, state memoryEdgeLogState) (*memoryGraphRepairEdgeEvidence, bool) {
	if m == nil {
		return nil, false
	}
	m.memoryGraphRepairEvidenceCacheMu.Lock()
	defer m.memoryGraphRepairEvidenceCacheMu.Unlock()
	if m.memoryGraphRepairEvidenceCache == nil || m.memoryGraphRepairEvidenceCacheProject != project || m.memoryGraphRepairEvidenceCacheSnapshot != snapshotDigest {
		return nil, false
	}
	cached := m.memoryGraphRepairEvidenceCache
	// A generation is the authenticated content lineage.  Older generations
	// must never be reused merely because a replacement happened to retain the
	// same byte length; the replacement path deliberately advances generation
	// even when identity/modtime hooks make the stamp look unchanged.
	if !cached.Complete || cached.LogFileSize > state.FileSize {
		return nil, false
	}
	emptyPrefixIdentityTransition := cached.LogFileSize == 0 && cached.LogFileIdentity == "absent" && state.ParentFileIdentity == "absent"
	if strings.TrimSpace(cached.LogFileIdentity) == "" || (cached.LogFileIdentity != state.FileIdentity && !emptyPrefixIdentityTransition) {
		return nil, false
	}
	if cached.LogGeneration == state.Generation {
		if cached.LogDigest != state.Digest || cached.LogContentDigest != state.ContentDigest {
			return nil, false
		}
		return cached, true
	}
	// A single generation step is reusable only when the sidecar proves that
	// the current file is an append to the exact cached prefix.  The parent
	// content digest/size/identity is written by every descriptor-bound append
	// and replacement.  This rejects same-size replacement followed by append,
	// even when an adversarial hook preserves inode and timestamp metadata.
	if state.Generation != cached.LogGeneration+1 || state.FileSize <= cached.LogFileSize || state.ParentFileSize != cached.LogFileSize || state.ParentContentDigest != cached.LogContentDigest || state.ParentFileIdentity != cached.LogFileIdentity {
		return nil, false
	}
	return cached, true
}

func (m *memoryStore) cacheMemoryGraphRepairEvidence(snapshotDigest, project string, evidence memoryGraphRepairEdgeEvidence) {
	if m == nil {
		return
	}
	m.memoryGraphRepairEvidenceCacheMu.Lock()
	m.memoryGraphRepairEvidenceCacheProject = project
	m.memoryGraphRepairEvidenceCacheSnapshot = snapshotDigest
	m.memoryGraphRepairEvidenceCache = &evidence
	m.memoryGraphRepairEvidenceCacheMu.Unlock()
}

func (m *memoryStore) captureMemoryGraphRepairEdges(snapshot memoryGraphRepairSnapshot) (memoryGraphRepairEdgeEvidence, error) {
	return m.captureMemoryGraphRepairEdgesContext(context.Background(), snapshot)
}

func (m *memoryStore) captureMemoryGraphRepairEdgesContext(ctx context.Context, snapshot memoryGraphRepairSnapshot) (memoryGraphRepairEdgeEvidence, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return memoryGraphRepairEdgeEvidence{}, err
	}
	if m.memoryGraphRepairCaptureHook != nil {
		m.memoryGraphRepairCaptureHook()
	}
	maxBytes := m.policy.historyStartupTailMaxBytes
	if maxBytes < 1 || maxBytes > memoryGraphRepairMaxEdgeBytes {
		maxBytes = memoryGraphRepairMaxEdgeBytes
	}
	fence, err := m.acquireMemoryEdgeLogFenceContext(ctx)
	if err != nil {
		return memoryGraphRepairEdgeEvidence{}, memoryGraphRepairIOErr("edge_log_io", err)
	}
	defer fence.release()
	appender, err := m.newMemoryEdgeLogAppenderFastWithFenceContextLocked(ctx, true, fence)
	if err != nil {
		return memoryGraphRepairEdgeEvidence{}, memoryGraphRepairIOErr("edge_log_io", err)
	}
	if cached, ok := m.cachedMemoryGraphRepairEvidence(snapshot.SnapshotDigest, snapshot.Project, appender.state); ok {
		if appender.state.FileSize == cached.LogFileSize {
			return *cached, nil
		}
		if cached.LogFileSize >= 0 && appender.state.FileSize > cached.LogFileSize && appender.state.FileSize <= maxBytes {
			expectedStamp := memoryEdgeLogFileStamp{Exists: true, Size: appender.state.FileSize, Identity: appender.state.FileIdentity, ModTimeNanos: appender.state.FileModTimeNanos, ChangeToken: appender.state.FileChangeToken}
			suffix, suffixErr := m.readMemoryEdgeLogSuffixLocked(ctx, cached.LogFileSize, appender.state.FileSize, maxBytes, expectedStamp)
			if suffixErr != nil {
				return memoryGraphRepairEdgeEvidence{}, memoryGraphRepairIOErr("edge_log_io", suffixErr)
			}
			evidence := *cached
			if err := m.extendMemoryGraphRepairEvidenceFromLogSuffix(ctx, snapshot, &evidence, suffix, appender.state); err != nil {
				return evidence, err
			}
			if err := m.persistMemoryGraphRepairEvidenceArtifact(snapshot, evidence); err != nil {
				return memoryGraphRepairEdgeEvidence{}, memoryGraphRepairIOErr("edge_log_io", err)
			}
			m.cacheMemoryGraphRepairEvidence(snapshot.SnapshotDigest, snapshot.Project, evidence)
			return evidence, nil
		}
	}
	// The process-local cache is an optimization only. A restart or cache loss
	// must recover the authenticated row/index midstate from this bounded,
	// snapshot-bound artifact before considering a full edge-log scan.
	if durable, durableOK, durableErr := m.loadMemoryGraphRepairEvidenceArtifact(ctx, snapshot); durableErr != nil {
		return memoryGraphRepairEdgeEvidence{}, memoryGraphRepairIOErr("edge_log_io", durableErr)
	} else if durableOK {
		m.cacheMemoryGraphRepairEvidence(snapshot.SnapshotDigest, snapshot.Project, durable)
		if reusable, reusableOK := m.cachedMemoryGraphRepairEvidence(snapshot.SnapshotDigest, snapshot.Project, appender.state); reusableOK {
			if appender.state.FileSize == reusable.LogFileSize {
				return *reusable, nil
			}
			if appender.state.FileSize > reusable.LogFileSize && appender.state.FileSize <= maxBytes {
				expectedStamp := memoryEdgeLogFileStamp{Exists: true, Size: appender.state.FileSize, Identity: appender.state.FileIdentity, ModTimeNanos: appender.state.FileModTimeNanos, ChangeToken: appender.state.FileChangeToken}
				suffix, suffixErr := m.readMemoryEdgeLogSuffixLocked(ctx, reusable.LogFileSize, appender.state.FileSize, maxBytes, expectedStamp)
				if suffixErr != nil {
					return memoryGraphRepairEdgeEvidence{}, memoryGraphRepairIOErr("edge_log_io", suffixErr)
				}
				evidence := *reusable
				if err := m.extendMemoryGraphRepairEvidenceFromLogSuffix(ctx, snapshot, &evidence, suffix, appender.state); err != nil {
					return evidence, err
				}
				if err := m.persistMemoryGraphRepairEvidenceArtifact(snapshot, evidence); err != nil {
					return memoryGraphRepairEdgeEvidence{}, memoryGraphRepairIOErr("edge_log_io", err)
				}
				m.cacheMemoryGraphRepairEvidence(snapshot.SnapshotDigest, snapshot.Project, evidence)
				return evidence, nil
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return memoryGraphRepairEdgeEvidence{}, err
	}
	logSnapshot, err := m.snapshotMemoryEdgeLogContextLocked(ctx, maxBytes)
	if err != nil {
		return memoryGraphRepairEdgeEvidence{}, memoryGraphRepairIOErr("edge_log_io", err)
	}
	evidence, err := m.memoryGraphRepairEvidenceFromLogSnapshotContext(ctx, snapshot, logSnapshot)
	if err != nil {
		return evidence, err
	}
	if err := m.persistMemoryGraphRepairEvidenceArtifact(snapshot, evidence); err != nil {
		return memoryGraphRepairEdgeEvidence{}, memoryGraphRepairIOErr("edge_log_io", err)
	}
	m.cacheMemoryGraphRepairEvidence(snapshot.SnapshotDigest, snapshot.Project, evidence)
	return evidence, nil
}

func (m *memoryStore) memoryGraphRepairEvidenceFromLogSnapshot(snapshot memoryGraphRepairSnapshot, logSnapshot memoryEdgeLogSnapshot) (memoryGraphRepairEdgeEvidence, error) {
	return m.memoryGraphRepairEvidenceFromLogSnapshotContext(context.Background(), snapshot, logSnapshot)
}

func (m *memoryStore) memoryGraphRepairEvidenceFromLogSnapshotContext(ctx context.Context, snapshot memoryGraphRepairSnapshot, logSnapshot memoryEdgeLogSnapshot) (memoryGraphRepairEdgeEvidence, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	evidence := memoryGraphRepairEdgeEvidence{
		Latest: map[string]memoryEdgeEntry{}, RetirementSeen: map[string]bool{}, RepairRuns: map[string]map[string]struct{}{}, RepairActionRows: map[string]map[string]memoryEdgeEntry{}, RollbackActionRows: map[string]map[string]memoryEdgeEntry{}, Complete: true,
		Project: snapshot.Project, ProjectAliasIndex: &memoryGraphRepairAliasIndex{Aliases: snapshot.Aliases, AmbiguousAliases: snapshot.AmbiguousAliases}, DigestIndex: &memoryGraphRepairDigestIndex{},
	}
	maxLines := m.policy.edgeStartupMaxLines
	if maxLines < 1 || maxLines > memoryGraphRepairMaxEdgeLines {
		maxLines = memoryGraphRepairMaxEdgeLines
	}
	evidence.LogGeneration = logSnapshot.Generation
	evidence.LogDigest = logSnapshot.Digest
	evidence.LogContentDigest = logSnapshot.ContentDigest
	evidence.LogContentHashState = logSnapshot.ContentHashState
	evidence.LogContentHashedBytes = logSnapshot.FileSize
	evidence.LogFileSize = logSnapshot.FileSize
	evidence.LogFileIdentity = logSnapshot.FileStamp.Identity
	evidence.ScannedBytes = int64(len(logSnapshot.Bytes))
	if m.memoryEdgeLogObserveIO != nil {
		m.memoryEdgeLogObserveIO("repair_evidence_full_scan", int64(len(logSnapshot.Bytes)))
	}
	scanner := bufio.NewScanner(bytes.NewReader(logSnapshot.Bytes))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lines := make([]string, 0, minInt(maxLines, 4096))
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return evidence, ctx.Err()
		default:
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lines = append(lines, line)
		if len(lines) > maxLines {
			evidence.Complete = false
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return evidence, err
	}
	evidence.ScannedLines = len(lines)
	if len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	for _, line := range lines {
		var edge memoryEdgeEntry
		if err := json.Unmarshal([]byte(line), &edge); err != nil {
			evidence.InvalidCount++
			evidence.Complete = false
			evidence.Rows = append(evidence.Rows, memoryGraphRepairEdgeRow{RawDigest: "sha256:" + sha256Hex(line), Invalid: true})
			continue
		}
		row, normErr := memoryGraphRepairCanonicalizeEdge(edge, snapshot)
		row.RawDigest = "sha256:" + sha256Hex(line)
		if normErr != nil {
			evidence.InvalidCount++
			evidence.Complete = false
		} else {
			evidence.record(row)
		}
	}
	// The durable edge log tail is the only edge authority.  In particular, do
	// not walk m.edges here: that hot projection can contain an unbounded number
	// of unrelated projects and is not a restart-stable snapshot.  Appends made
	// by this lane are fsynced to the same bounded source before the next chunk.
	evidence.Digest = evidence.projectDigest()
	if !evidence.Complete {
		return evidence, memoryGraphRepairErr("incomplete_snapshot", errors.New("edge evidence is incomplete, invalid, or exceeded its bounded capture cap"))
	}
	return evidence, nil
}

func (m *memoryStore) extendMemoryGraphRepairEvidenceFromLogSuffix(ctx context.Context, snapshot memoryGraphRepairSnapshot, evidence *memoryGraphRepairEdgeEvidence, suffix []byte, state memoryEdgeLogState) error {
	if evidence == nil {
		return errors.New("memory graph repair evidence index is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	maxLines := m.policy.edgeStartupMaxLines
	if maxLines < 1 || maxLines > memoryGraphRepairMaxEdgeLines {
		maxLines = memoryGraphRepairMaxEdgeLines
	}
	evidence.LogGeneration = state.Generation
	evidence.LogDigest = state.Digest
	evidence.LogContentDigest = state.ContentDigest
	evidence.LogContentHashState = state.ContentHashState
	evidence.LogContentHashedBytes = state.ContentHashedBytes
	evidence.LogFileSize = state.FileSize
	evidence.LogFileIdentity = state.FileIdentity
	evidence.ScannedBytes += int64(len(suffix))
	if m.memoryEdgeLogObserveIO != nil {
		m.memoryEdgeLogObserveIO("repair_evidence_incremental", int64(len(suffix)))
	}
	scanner := bufio.NewScanner(bytes.NewReader(suffix))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		evidence.ScannedLines++
		if evidence.ScannedLines > maxLines {
			evidence.Complete = false
			break
		}
		var edge memoryEdgeEntry
		if err := json.Unmarshal([]byte(line), &edge); err != nil {
			evidence.InvalidCount++
			evidence.Complete = false
			evidence.Rows = append(evidence.Rows, memoryGraphRepairEdgeRow{RawDigest: "sha256:" + sha256Hex(line), Invalid: true})
			continue
		}
		row, normErr := memoryGraphRepairCanonicalizeEdge(edge, snapshot)
		row.RawDigest = "sha256:" + sha256Hex(line)
		if normErr != nil {
			evidence.InvalidCount++
			evidence.Complete = false
		} else {
			evidence.record(row)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	evidence.Digest = evidence.projectDigest()
	if !evidence.Complete {
		return memoryGraphRepairErr("incomplete_snapshot", errors.New("edge evidence is incomplete, invalid, or exceeded its bounded capture cap"))
	}
	return nil
}

func (e *memoryGraphRepairEdgeEvidence) record(row memoryGraphRepairEdgeRow) {
	if e == nil {
		return
	}
	if row.Invalid {
		e.Rows = append(e.Rows, row)
		return
	}
	priorLatest, priorLatestExists := e.Latest[row.Edge.EdgeID]
	if priorLatestExists {
		e.DuplicateCount++
	}
	projectRow := strings.TrimSpace(e.Project) != "" && memoryGraphRepairEdgeInProject(row.Edge, e.Project)
	if projectRow {
		e.ensureProjectIndexes()
		e.DigestIndex.add(row)
		e.ProjectRows++
		if _, seen := e.ProjectSeenEdgeIDs[row.Edge.EdgeID]; seen {
			e.ProjectDuplicateCount++
		} else {
			e.ProjectSeenEdgeIDs[row.Edge.EdgeID] = struct{}{}
		}
		if priorLatestExists && memoryGraphRepairEdgeInProject(priorLatest, e.Project) {
			e.adjustProjectBinding(priorLatest, -1)
		}
	}
	e.Rows = append(e.Rows, row)
	e.Latest[row.Edge.EdgeID] = row.Edge
	if projectRow {
		e.adjustProjectBinding(row.Edge, 1)
	}
	if memoryGraphRepairEdgeRetired(row.Edge) {
		e.RetirementSeen[row.Edge.EdgeID] = true
	}
	// Rollback rows can validly restore an earlier repair row and therefore
	// inherit its repair_run_id/action_id. Keep the immutable original apply row
	// as the action authority; rollback evidence has its own identity fields.
	if runID := memoryGraphRepairEdgeMetadataRepair(row.Edge, "repair_run_id"); runID != "" &&
		memoryGraphRepairEdgeMetadataRepair(row.Edge, "rollback_run_id") == "" && memoryGraphRepairServerAppendMarkerValid("repair", row.Edge) {
		actionID := memoryGraphRepairEdgeMetadataRepair(row.Edge, "repair_action_id")
		if actionID != "" {
			e.ensureProjectIndexes()
			if e.ProjectPriorRows[runID] == nil {
				e.ProjectPriorRows[runID] = map[string]memoryEdgeEntry{}
			}
			// Capture the exact latest row before this authenticated append. A
			// zero entry is an authenticated "no predecessor" marker; omitting
			// the action would force a legacy continuation back through Rows.
			if priorLatestExists {
				e.ProjectPriorRows[runID][actionID] = priorLatest
			} else {
				e.ProjectPriorRows[runID][actionID] = memoryEdgeEntry{}
			}
		}
		if e.RepairRuns == nil {
			e.RepairRuns = map[string]map[string]struct{}{}
		}
		if e.RepairRuns[runID] == nil {
			e.RepairRuns[runID] = map[string]struct{}{}
		}
		e.RepairRuns[runID][actionID] = struct{}{}
		if actionID != "" {
			if e.RepairActionRows == nil {
				e.RepairActionRows = map[string]map[string]memoryEdgeEntry{}
			}
			if e.RepairActionRows[runID] == nil {
				e.RepairActionRows[runID] = map[string]memoryEdgeEntry{}
			}
			e.RepairActionRows[runID][actionID] = row.Edge
		}
	}
	if rollbackRunID := memoryGraphRepairEdgeMetadataRepair(row.Edge, "rollback_run_id"); rollbackRunID != "" && memoryGraphRepairServerAppendMarkerValid("rollback", row.Edge) {
		actionID := memoryGraphRepairEdgeMetadataRepair(row.Edge, "rollback_action_id")
		if actionID != "" {
			if e.RollbackActionRows == nil {
				e.RollbackActionRows = map[string]map[string]memoryEdgeEntry{}
			}
			if e.RollbackActionRows[rollbackRunID] == nil {
				e.RollbackActionRows[rollbackRunID] = map[string]memoryEdgeEntry{}
			}
			e.RollbackActionRows[rollbackRunID][actionID] = row.Edge
		}
	}
	if row.Bound {
		e.BoundCount++
	} else {
		e.UnboundCount++
		if memoryGraphRepairEdgeIsInferred(row.Edge) {
			e.UnboundInferred++
		} else {
			e.UnboundExplicit++
		}
	}
}

func memoryGraphRepairEdgeInProject(edge memoryEdgeEntry, project string) bool {
	if !strings.EqualFold(strings.TrimSpace(edge.Project), strings.TrimSpace(project)) {
		return false
	}
	for _, memoryID := range []string{edge.SourceID, edge.TargetID} {
		p, _, _, _, err := memoryGraphRepairCanonicalID(memoryID)
		if err == nil && !strings.EqualFold(p, project) {
			return false
		}
	}
	return true
}

func memoryGraphRepairProjectEdgeDigest(rows []memoryGraphRepairEdgeRow, project string) string {
	index := &memoryGraphRepairDigestIndex{}
	for _, row := range rows {
		// Invalid rows remain counted and preserved in the evidence report, but
		// have no trustworthy project binding and therefore cannot affect a
		// project-scoped digest or checkpoint.
		if !row.Invalid && memoryGraphRepairEdgeInProject(row.Edge, project) {
			index.add(row)
		}
	}
	return index.digest()
}

func memoryGraphRepairProjectEvidenceCounts(evidence memoryGraphRepairEdgeEvidence, project string) (rows int, duplicates int) {
	if evidence.Project == project && evidence.ProjectSeenEdgeIDs != nil && evidence.DigestIndex != nil {
		return evidence.ProjectRows, evidence.ProjectDuplicateCount
	}
	seen := map[string]struct{}{}
	for _, row := range evidence.Rows {
		if row.Invalid || !memoryGraphRepairEdgeInProject(row.Edge, project) {
			continue
		}
		rows++
		if _, exists := seen[row.Edge.EdgeID]; exists {
			duplicates++
		}
		seen[row.Edge.EdgeID] = struct{}{}
	}
	return rows, duplicates
}

func memoryGraphRepairBindingReason(sourceStatus, targetStatus string) string {
	if sourceStatus == "bound" && targetStatus == "bound" {
		return "bound_current_state"
	}
	if sourceStatus == "ambiguous" || targetStatus == "ambiguous" {
		return "ambiguous_alias"
	}
	if sourceStatus == "unknown" || targetStatus == "unknown" {
		return "unknown_endpoint"
	}
	return "unbound_current_state"
}

func memoryGraphRepairBindingFacts(snapshot memoryGraphRepairSnapshot, edge memoryEdgeEntry) (string, string, string) {
	alias := &memoryGraphRepairAliasIndex{Aliases: snapshot.Aliases, AmbiguousAliases: snapshot.AmbiguousAliases}
	_, sourceStatus := alias.resolve(edge.SourceID)
	_, targetStatus := alias.resolve(edge.TargetID)
	reason := memoryGraphRepairBindingReason(sourceStatus, targetStatus)
	if reason == "bound_current_state" && !memoryGraphRepairBindingMatchesSnapshot(snapshot, edge) {
		reason = "stale_current_state_binding"
	}
	return sourceStatus, targetStatus, reason
}

func memoryGraphRepairBindingBucket() map[string]int {
	return map[string]int{"bound": 0, "unbound": 0, "ambiguous": 0, "unknown": 0}
}

func memoryGraphRepairIncrementBindingBucket(bucket map[string]int, reason string) {
	if bucket == nil {
		return
	}
	bucket["unbound"]++
	switch reason {
	case "bound_current_state":
		bucket["bound"]++
		bucket["unbound"]--
	case "ambiguous_alias":
		bucket["ambiguous"]++
	case "unknown_endpoint":
		bucket["unknown"]++
	}
}

func cloneMemoryGraphRepairBindingBucket(bucket map[string]int) map[string]int {
	copy := memoryGraphRepairBindingBucket()
	for key, value := range bucket {
		copy[key] = value
	}
	return copy
}

func cloneMemoryGraphRepairBindingBuckets(buckets map[string]map[string]int) map[string]map[string]int {
	copy := map[string]map[string]int{}
	for key, bucket := range buckets {
		copy[key] = cloneMemoryGraphRepairBindingBucket(bucket)
	}
	return copy
}

func memoryGraphRepairActiveBindingCount(bucket map[string]int) int {
	if bucket == nil {
		return 0
	}
	return bucket["bound"] + bucket["unbound"]
}

// memoryGraphRepairBindingSummary is deliberately based on the latest active
// edge identity per edge ID. It reports the endpoint binding truth used for
// connectedness, with relation/project/reason breakdowns for operator review.
func memoryGraphRepairBindingSummary(snapshot memoryGraphRepairSnapshot, evidence memoryGraphRepairEdgeEvidence, project string) map[string]any {
	if evidence.Project == project && evidence.ProjectAliasIndex != nil && evidence.ProjectBindingByReason != nil {
		byReason := cloneMemoryGraphRepairBindingBucket(evidence.ProjectBindingByReason)
		byRelation := cloneMemoryGraphRepairBindingBuckets(evidence.ProjectBindingByRelation)
		byProject := cloneMemoryGraphRepairBindingBuckets(evidence.ProjectBindingByProject)
		return map[string]any{
			"active_edges": memoryGraphRepairActiveBindingCount(byReason), "bound_edges": byReason["bound"], "unbound_edges": byReason["unbound"],
			"by_reason": byReason, "by_relation": byRelation, "by_project": byProject,
			"resolution_basis": "current_state_index_alias_intersection",
		}
	}
	byReason := memoryGraphRepairBindingBucket()
	byRelation := map[string]map[string]int{}
	byProject := map[string]map[string]int{}
	active := 0
	for _, edge := range evidence.Latest {
		if !memoryGraphRepairEdgeInProject(edge, project) || memoryGraphRepairEdgeRetired(edge) {
			continue
		}
		_, sourceStatus := (&memoryGraphRepairAliasIndex{Aliases: snapshot.Aliases, AmbiguousAliases: snapshot.AmbiguousAliases}).resolve(edge.SourceID)
		_, targetStatus := (&memoryGraphRepairAliasIndex{Aliases: snapshot.Aliases, AmbiguousAliases: snapshot.AmbiguousAliases}).resolve(edge.TargetID)
		reason := memoryGraphRepairBindingReason(sourceStatus, targetStatus)
		memoryGraphRepairIncrementBindingBucket(byReason, reason)
		relation := strings.TrimSpace(edge.Relation)
		if relation == "" {
			relation = "unknown"
		}
		if byRelation[relation] == nil {
			byRelation[relation] = memoryGraphRepairBindingBucket()
		}
		memoryGraphRepairIncrementBindingBucket(byRelation[relation], reason)
		projectKey := strings.ToLower(strings.TrimSpace(edge.Project))
		if projectKey == "" {
			projectKey = "unknown"
		}
		if byProject[projectKey] == nil {
			byProject[projectKey] = memoryGraphRepairBindingBucket()
		}
		memoryGraphRepairIncrementBindingBucket(byProject[projectKey], reason)
		active++
	}
	return map[string]any{
		"active_edges":     active,
		"bound_edges":      byReason["bound"],
		"unbound_edges":    byReason["unbound"],
		"by_reason":        byReason,
		"by_relation":      byRelation,
		"by_project":       byProject,
		"resolution_basis": "current_state_index_alias_intersection",
	}
}

func memoryGraphRepairProjectedEvidence(snapshot memoryGraphRepairSnapshot, evidence memoryGraphRepairEdgeEvidence, actions []memoryGraphRepairAction, project string) memoryGraphRepairEdgeEvidence {
	projected := memoryGraphRepairEdgeEvidence{
		Latest: map[string]memoryEdgeEntry{}, RetirementSeen: map[string]bool{},
		RepairRuns: map[string]map[string]struct{}{}, RepairActionRows: map[string]map[string]memoryEdgeEntry{}, RollbackActionRows: map[string]map[string]memoryEdgeEntry{},
		Complete: evidence.Complete, Project: project,
		ProjectAliasIndex: &memoryGraphRepairAliasIndex{Aliases: snapshot.Aliases, AmbiguousAliases: snapshot.AmbiguousAliases}, DigestIndex: &memoryGraphRepairDigestIndex{},
	}
	for _, row := range evidence.Rows {
		projected.record(row)
	}
	for _, action := range actions {
		row, err := memoryGraphRepairCanonicalizeEdge(action.Edge, snapshot)
		if err != nil {
			continue
		}
		row.FromMemory = true
		projected.record(row)
	}
	projected.Digest = projected.projectDigest()
	return projected
}

func memoryGraphRepairEvidenceWithoutPendingRun(evidence memoryGraphRepairEdgeEvidence, project, runID string, pending map[string]struct{}) memoryGraphRepairEdgeEvidence {
	if !memoryGraphRepairEvidenceHasPendingRows(evidence, runID, pending) {
		return evidence
	}
	// This compatibility projection is used only when an older full evidence
	// artifact is replayed.  Its authenticated digest/index stay shared and the
	// mutable binding projection removes at most the pending action rows.  A
	// missing predecessor index fails closed instead of rebuilding Rows.
	trimmed := evidence
	trimmed.Project = project
	trimmed.Latest = evidence.Latest
	trimmed.ProjectBindingByReason = cloneMemoryGraphRepairBindingBucket(evidence.ProjectBindingByReason)
	trimmed.ProjectBindingByRelation = map[string]map[string]int{}
	trimmed.ProjectBindingByProject = map[string]map[string]int{}
	trimmed.ProjectBoundNodeRefs = map[string]int{}
	prepareBindingBuckets := func(edge memoryEdgeEntry) {
		relation := strings.TrimSpace(edge.Relation)
		if relation == "" {
			relation = "unknown"
		}
		if _, exists := trimmed.ProjectBindingByRelation[relation]; !exists {
			trimmed.ProjectBindingByRelation[relation] = cloneMemoryGraphRepairBindingBucket(evidence.ProjectBindingByRelation[relation])
		}
		projectKey := strings.ToLower(strings.TrimSpace(edge.Project))
		if projectKey == "" {
			projectKey = "unknown"
		}
		if _, exists := trimmed.ProjectBindingByProject[projectKey]; !exists {
			trimmed.ProjectBindingByProject[projectKey] = cloneMemoryGraphRepairBindingBucket(evidence.ProjectBindingByProject[projectKey])
		}
	}
	rows := evidence.RepairActionRows[runID]
	priorRows := evidence.ProjectPriorRows[runID]
	latestOverlay := map[string]memoryEdgeEntry{}
	latestPresent := map[string]bool{}
	actionIDs := make([]string, 0, len(pending))
	for actionID := range pending {
		if _, exists := rows[actionID]; exists {
			actionIDs = append(actionIDs, actionID)
		}
	}
	sort.Slice(actionIDs, func(i, j int) bool {
		left, right := rows[actionIDs[i]], rows[actionIDs[j]]
		leftCursor := anyToInt(left.Metadata["repair_cursor"], -1)
		rightCursor := anyToInt(right.Metadata["repair_cursor"], -1)
		if leftCursor != rightCursor {
			return leftCursor > rightCursor
		}
		return actionIDs[i] > actionIDs[j]
	})
	for _, actionID := range actionIDs {
		row, exists := rows[actionID]
		prior, priorKnown := priorRows[actionID]
		if !exists || !priorKnown {
			// The row index is authenticated but cannot prove the pre-append
			// state. Returning the original projection makes the caller reject
			// the digest/binding mismatch rather than guessing.
			return evidence
		}
		latest, latestExists := latestOverlay[row.EdgeID]
		if !latestPresent[row.EdgeID] {
			latest, latestExists = evidence.Latest[row.EdgeID]
		}
		if !latestExists || memoryGraphRepairOptionalEdgeDigest(latest) != memoryGraphRepairOptionalEdgeDigest(row) {
			continue
		}
		prepareBindingBuckets(latest)
		trimmed.adjustProjectBinding(latest, -1)
		if prior.EdgeID == "" {
			latestPresent[row.EdgeID] = true
			delete(latestOverlay, row.EdgeID)
			continue
		}
		prepareBindingBuckets(prior)
		trimmed.adjustProjectBinding(prior, 1)
		latestOverlay[row.EdgeID] = prior
		latestPresent[row.EdgeID] = true
	}
	return trimmed
}

func memoryGraphRepairEvidenceHasPendingRows(evidence memoryGraphRepairEdgeEvidence, runID string, pending map[string]struct{}) bool {
	if strings.TrimSpace(runID) == "" || len(pending) == 0 {
		return false
	}
	rows := evidence.RepairActionRows[runID]
	for actionID := range pending {
		if _, exists := rows[actionID]; exists {
			return true
		}
	}
	return false
}

func memoryGraphRepairActionResolution(snapshot memoryGraphRepairSnapshot, actions []memoryGraphRepairAction, evidence *memoryGraphRepairEdgeEvidence, runID string) map[string]any {
	proofRows := make([]map[string]any, 0, len(actions))
	writeCount, retireCount := 0, 0
	writeBound, writeUnresolved := 0, 0
	retireBound, retireUnbound := 0, 0
	durableActions, durableWriteBound, durableWriteUnresolved := 0, 0, 0
	provenanceClosed, durableProvenanceClosed, durableProvenanceInvalid := 0, 0, 0
	unresolvedIDs := []string{}
	unresolvedRefs := []string{}
	for _, action := range actions {
		sourceStatus, targetStatus, reason := action.SourceStatus, action.TargetStatus, action.BindingReason
		if sourceStatus == "" || targetStatus == "" || reason == "" {
			sourceStatus, targetStatus, reason = memoryGraphRepairBindingFacts(snapshot, action.Edge)
		}
		bound := reason == "bound_current_state"
		if action.Kind == "write" {
			writeCount++
			if bound {
				writeBound++
			} else {
				writeUnresolved++
				unresolvedIDs = append(unresolvedIDs, action.ActionID)
				if len(unresolvedRefs) < 64 {
					unresolvedRefs = append(unresolvedRefs, "action_"+sha256Hex(action.ActionID)[:20])
				}
			}
		} else {
			retireCount++
			if bound {
				retireBound++
			} else {
				retireUnbound++
			}
		}
		if memoryGraphRepairProvenanceClosed(action.Edge, snapshot, action.Kind) {
			provenanceClosed++
		}
		durable := false
		durableBound := bound
		if evidence != nil && runID != "" {
			if rows := evidence.RepairActionRows[runID]; rows != nil {
				if durableEdge, ok := rows[action.ActionID]; ok {
					latest, latestOK := evidence.Latest[durableEdge.EdgeID]
					durable = latestOK && sha256Hex(string(mustJSON(latest))) == sha256Hex(string(mustJSON(durableEdge)))
					if durable {
						if memoryGraphRepairProvenanceClosed(durableEdge, snapshot, action.Kind) {
							durableProvenanceClosed++
						} else {
							durableProvenanceInvalid++
						}
						if action.BindingReason != "" && len(snapshot.Aliases) == 0 && len(snapshot.AmbiguousAliases) == 0 {
							durableBound = action.BindingReason == "bound_current_state"
						} else {
							_, durableSourceStatus := (&memoryGraphRepairAliasIndex{Aliases: snapshot.Aliases, AmbiguousAliases: snapshot.AmbiguousAliases}).resolve(durableEdge.SourceID)
							_, durableTargetStatus := (&memoryGraphRepairAliasIndex{Aliases: snapshot.Aliases, AmbiguousAliases: snapshot.AmbiguousAliases}).resolve(durableEdge.TargetID)
							durableBound = durableSourceStatus == "bound" && durableTargetStatus == "bound"
						}
					}
				}
			}
		}
		if durable {
			durableActions++
			if action.Kind == "write" {
				if durableBound {
					durableWriteBound++
				} else {
					durableWriteUnresolved++
				}
			}
		}
		proofRows = append(proofRows, map[string]any{"action_id": action.ActionID, "kind": action.Kind, "source_status": sourceStatus, "target_status": targetStatus, "reason": reason, "bound": bound, "durable": durable, "provenance_closed": memoryGraphRepairProvenanceClosed(action.Edge, snapshot, action.Kind)})
	}
	return map[string]any{
		"action_count": len(actions), "write_action_count": writeCount, "retire_action_count": retireCount,
		"write_bound_count": writeBound, "write_unresolved_count": writeUnresolved,
		"retire_bound_count": retireBound, "retire_unbound_count": retireUnbound,
		"durable_action_count": durableActions, "durable_write_bound_count": durableWriteBound, "durable_write_unresolved_count": durableWriteUnresolved,
		"provenance_closed_count": provenanceClosed, "durable_provenance_closed_count": durableProvenanceClosed, "durable_provenance_invalid_count": durableProvenanceInvalid,
		"unresolved_write_action_count": len(unresolvedIDs), "unresolved_write_action_digest": "sha256:" + sha256Hex(string(mustJSON(unresolvedIDs))), "unresolved_write_action_refs": unresolvedRefs, "all_repaired_writes_bound": writeUnresolved == 0,
		"all_repair_provenance_closed": provenanceClosed == len(actions) && durableProvenanceInvalid == 0,
		"resolution_digest":            "sha256:" + sha256Hex(string(mustJSON(proofRows))),
		"resolution_basis":             "canonical_current_state_index_ids",
	}
}

func mustJSON(value any) []byte {
	raw, _ := json.Marshal(value)
	return raw
}

func (m *memoryStore) currentMemoryGraphRepairGeneration(project string) (uint64, uint64, error) {
	projectKey := normalizeCurrentKeyIndexProject(project)
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.currentKeyIndexGeneration == nil || m.currentTopicIndexGeneration == nil {
		return 0, 0, errors.New("current-state/index generations are unavailable")
	}
	key, keyOK := m.currentKeyIndexGeneration[projectKey]
	topic, topicOK := m.currentTopicIndexGeneration[projectKey]
	if !keyOK && !topicOK {
		return 0, 0, nil
	}
	if !keyOK || !topicOK {
		return 0, 0, errors.New("current-state/index generation is unavailable")
	}
	return key, topic, nil
}

func memoryGraphRepairBackfillDocs(snapshot memoryGraphRepairSnapshot) []memoryEdgeBackfillDoc {
	docs := make([]memoryEdgeBackfillDoc, 0, len(snapshot.Docs))
	for _, doc := range snapshot.Docs {
		docs = append(docs, memoryEdgeBackfillDoc{
			Project: doc.Project, FileName: doc.FileName, MemoryID: doc.MemoryID, TopicPath: doc.TopicPath,
			Summary: doc.Summary, EventID: doc.EventID, AgentID: doc.AgentID, SessionID: doc.SessionID,
			UpdatedAt: doc.UpdatedAt, LastTouch: doc.LastTouch, Lifecycle: doc.Lifecycle,
			ContentHash: doc.ContentHash, ContentRef: doc.ContentRef,
			References: append([]memoryStructuredReference(nil), doc.References...),
		})
	}
	return docs
}

func (m *memoryStore) generateCurrentStateSequenceEdges(ctx context.Context, snapshot memoryGraphRepairSnapshot, req memoryGraphRepairRequest) []memoryEdgeBackfillCandidate {
	docs := memoryGraphRepairBackfillDocs(snapshot)
	generator := &memoryEdgeBackfillGenerator{
		store: m, request: memoryEdgeBackfillRequest{
			Project: req.Project, IncludeEphemeral: req.IncludeEphemeral, MinConfidence: req.MinConfidence,
			MaxCandidates: req.MaxCandidates, TopicPeerLimit: req.TopicPeerLimit,
			AllowedRelation: map[string]struct{}{},
		}, docs: docs, knownIDs: map[string]memoryEdgeBackfillDoc{}, candidates: []memoryEdgeBackfillCandidate{}, stats: map[string]*memoryEdgeBackfillRelationStats{},
	}
	for _, doc := range docs {
		generator.knownIDs[strings.ToLower(doc.MemoryID)] = doc
	}
	generator.generateCurrentStateSequenceEdges(ctx, docs, "same_session", 0.98)
	generator.generateCurrentStateSequenceEdges(ctx, docs, "same_agent", 0.82)
	return generator.candidates
}

func memoryGraphRepairPolicyDigest(req memoryGraphRepairRequest) string {
	policy := map[string]any{
		"project": req.Project, "include_cold": req.IncludeCold, "include_ephemeral": req.IncludeEphemeral,
		"include_inferred": req.IncludeInferred, "retire_stale_inferred": req.RetireStaleInferred,
		"stale_after": req.StaleAfter.UTC().Format(time.RFC3339Nano), "min_confidence": req.MinConfidence,
		"max_candidates": req.MaxCandidates, "topic_peer_limit": req.TopicPeerLimit,
		"inferred_peer_limit": req.InferredPeerLimit, "inferred_scan_limit": req.InferredScanLimit,
		"inferred_min_score": req.InferredMinScore, "inferred_min_shared": req.InferredMinShared,
		"inferred_max_postings": req.InferredMaxPostings,
	}
	return "sha256:" + sha256Hex(string(mustJSON(policy)))
}

func memoryGraphRepairActionDigest(actions []memoryGraphRepairAction) string {
	return "sha256:" + sha256Hex(string(mustJSON(actions)))
}

type memoryGraphRepairPlanReceipt struct {
	SchemaID                  string                    `json:"schema_id"`
	Version                   int                       `json:"version"`
	ReceiptRef                string                    `json:"receipt_ref"`
	Project                   string                    `json:"project"`
	SnapshotDigest            string                    `json:"snapshot_digest"`
	CurrentStateDigest        string                    `json:"current_state_digest"`
	KeyGeneration             uint64                    `json:"key_generation"`
	TopicGeneration           uint64                    `json:"topic_generation"`
	EdgeDigest                string                    `json:"edge_digest"`
	EdgeDigestAlgorithm       string                    `json:"edge_digest_algorithm,omitempty"`
	PolicyDigest              string                    `json:"policy_digest"`
	ActionDigest              string                    `json:"action_digest"`
	ActionCount               int                       `json:"action_count"`
	Actions                   []memoryGraphRepairAction `json:"actions"`
	BindingBefore             map[string]any            `json:"binding_before"`
	BindingProjectedAfter     map[string]any            `json:"binding_projected_after"`
	ResolutionProof           map[string]any            `json:"resolution_proof"`
	SnapshotIndexedCount      int                       `json:"snapshot_indexed_count"`
	SnapshotEligibleCount     int                       `json:"snapshot_eligible_count"`
	SnapshotExcludedCount     int                       `json:"snapshot_excluded_count"`
	SnapshotConnectedDocCount int                       `json:"snapshot_connected_doc_count"`
	SnapshotIsolatedDocCount  int                       `json:"snapshot_isolated_doc_count"`
	EdgeScannedLines          int                       `json:"edge_scanned_lines"`
	EdgeProjectRowCount       int                       `json:"edge_project_row_count"`
	EdgeDuplicateCount        int                       `json:"edge_duplicate_count"`
	EdgeInvalidCount          int                       `json:"edge_invalid_count"`
	EdgeUnboundExplicit       int                       `json:"edge_unbound_explicit_count"`
	EdgeUnboundInferred       int                       `json:"edge_unbound_inferred_count"`
	StaleAfter                string                    `json:"stale_after"`
	ObservedAt                string                    `json:"observed_at"`
	CreatedAt                 string                    `json:"created_at"`
	ExpiresAt                 string                    `json:"expires_at"`
	ActorAuthority            string                    `json:"actor_authority"`
	ActorPrincipalDigest      string                    `json:"actor_principal_digest,omitempty"`
	ActorScopeDigest          string                    `json:"actor_scope_digest,omitempty"`
	ActorWorkspaceDigest      string                    `json:"actor_workspace_digest,omitempty"`
	ActorInstallDigest        string                    `json:"actor_installation_digest,omitempty"`
	ActorAuthorityDigest      string                    `json:"actor_authority_digest,omitempty"`
	ActorCustodyDigest        string                    `json:"actor_custody_digest,omitempty"`
	Applicable                bool                      `json:"applicable"`
	EdgeLogGeneration         uint64                    `json:"edge_log_generation"`
	EdgeLogDigest             string                    `json:"edge_log_digest"`
	EdgeLogContentDigest      string                    `json:"edge_log_content_digest"`
	EdgeLogFileSize           int64                     `json:"edge_log_file_size"`
	EdgeLogFileIdentity       string                    `json:"edge_log_file_identity,omitempty"`
	EdgeLogContentHashState   string                    `json:"edge_log_content_hash_state,omitempty"`
	EdgeLogContentHashedBytes int64                     `json:"edge_log_content_hashed_bytes"`
	Custody                   map[string]any            `json:"custody"`
}

func memoryGraphRepairPlanReceiptRef(project, snapshotDigest, edgeDigest, actionDigest string, identity ...string) string {
	return "plan_" + sha256Hex(strings.ToLower(project) + "\x00" + snapshotDigest + "\x00" + edgeDigest + "\x00" + actionDigest + "\x00" + strings.Join(identity, "\x00"))[:24]
}

func memoryGraphRepairPlanReceiptPath(m *memoryStore, ref string) string {
	return filepath.Join(m.policy.rootPath, "_contextlattice", "memory_graph_repair_plan_"+strings.TrimPrefix(ref, "plan_")+".json")
}

func memoryGraphRepairPlanReceiptDigest(receipt memoryGraphRepairPlanReceipt) string {
	raw, _ := json.Marshal(receipt)
	return "sha256:" + sha256Hex(string(raw))
}

func (m *memoryStore) persistMemoryGraphRepairPlanReceipt(receipt memoryGraphRepairPlanReceipt) (memoryGraphRepairPlanReceipt, string, error) {
	if receipt.ReceiptRef == "" {
		receipt.ReceiptRef = memoryGraphRepairPlanReceiptRef(receipt.Project, receipt.SnapshotDigest, receipt.EdgeDigest, receipt.ActionDigest, receipt.PolicyDigest, receipt.ActorCustodyDigest, receipt.ObservedAt)
	}
	if receipt.CreatedAt == "" {
		receipt.CreatedAt = nowUTCISO()
	}
	if receipt.ExpiresAt == "" {
		receipt.ExpiresAt = time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339Nano)
	}
	receipt.SchemaID, receipt.Version = memoryGraphRepairPlanReceiptID, 1
	receipt.Custody = memoryGraphRepairCustody(m)
	if receipt.ActorPrincipalDigest != "" {
		receipt.Custody["actor_principal_digest"] = receipt.ActorPrincipalDigest
	}
	if receipt.ActorScopeDigest != "" {
		receipt.Custody["actor_scope_digest"] = receipt.ActorScopeDigest
	}
	for key, value := range map[string]string{"actor_workspace_digest": receipt.ActorWorkspaceDigest, "actor_installation_digest": receipt.ActorInstallDigest, "actor_authority_digest": receipt.ActorAuthorityDigest, "actor_custody_digest": receipt.ActorCustodyDigest} {
		if value != "" {
			receipt.Custody[key] = value
		}
	}
	path := memoryGraphRepairPlanReceiptPath(m, receipt.ReceiptRef)
	if existingRaw, err := readMemoryGraphRepairArtifact(path); err == nil {
		var existing memoryGraphRepairPlanReceipt
		if json.Unmarshal(existingRaw, &existing) != nil || memoryGraphRepairPlanReceiptDigest(existing) != memoryGraphRepairPlanReceiptDigest(receipt) {
			return receipt, "", memoryGraphRepairErr("plan_receipt_conflict", errors.New("immutable dry-run plan receipt already exists with different content"))
		}
		return existing, memoryGraphRepairPlanReceiptDigest(existing), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return receipt, "", err
	}
	raw, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return receipt, "", err
	}
	if err := validateMemoryGraphRepairArtifactBytes(raw); err != nil {
		return receipt, "", err
	}
	if err := writeOwnerOnlyDurableAtomicFile(path, append(raw, '\n'), true); err != nil {
		return receipt, "", err
	}
	return receipt, memoryGraphRepairPlanReceiptDigest(receipt), nil
}

func (m *memoryStore) loadMemoryGraphRepairPlanReceipt(req memoryGraphRepairRequest, allowExpired bool) (memoryGraphRepairPlanReceipt, string, error) {
	if req.PlanReceiptRef == "" || req.PlanReceiptDigest == "" {
		return memoryGraphRepairPlanReceipt{}, "", memoryGraphRepairErr("plan_receipt_required", errors.New("apply requires an immutable dry-run plan receipt and digest"))
	}
	if !strings.HasPrefix(req.PlanReceiptRef, "plan_") || len(req.PlanReceiptRef) != len("plan_")+24 || strings.Trim(req.PlanReceiptRef[len("plan_"):], "0123456789abcdef") != "" {
		return memoryGraphRepairPlanReceipt{}, "", memoryGraphRepairErr("plan_receipt_invalid", errors.New("plan receipt reference is invalid"))
	}
	raw, err := readMemoryGraphRepairArtifact(memoryGraphRepairPlanReceiptPath(m, req.PlanReceiptRef))
	if errors.Is(err, os.ErrNotExist) {
		return memoryGraphRepairPlanReceipt{}, "", memoryGraphRepairErr("plan_receipt_required", errors.New("dry-run plan receipt is unavailable"))
	}
	if err != nil {
		return memoryGraphRepairPlanReceipt{}, "", memoryGraphRepairErr("plan_receipt_io", err)
	}
	var receipt memoryGraphRepairPlanReceipt
	if json.Unmarshal(raw, &receipt) != nil || receipt.SchemaID != memoryGraphRepairPlanReceiptID || receipt.Version != 1 {
		return receipt, "", memoryGraphRepairErr("plan_receipt_invalid", errors.New("dry-run plan receipt is invalid"))
	}
	if err := memoryGraphRepairCustodyMatches(m, receipt.Custody); err != nil {
		return receipt, "", memoryGraphRepairErr("custody_mismatch", err)
	}
	digest := memoryGraphRepairPlanReceiptDigest(receipt)
	if digest != req.PlanReceiptDigest || receipt.ReceiptRef != req.PlanReceiptRef || !strings.EqualFold(receipt.Project, req.Project) {
		return receipt, "", memoryGraphRepairErr("plan_receipt_mismatch", errors.New("dry-run plan receipt digest or project does not match"))
	}
	if receipt.ActionCount != len(receipt.Actions) || receipt.ActionDigest != memoryGraphRepairActionDigest(receipt.Actions) {
		return receipt, "", memoryGraphRepairErr("plan_receipt_invalid", errors.New("dry-run plan receipt action content is invalid"))
	}
	if receipt.EdgeLogContentHashState != "" || receipt.EdgeLogContentHashedBytes != 0 || receipt.EdgeLogFileSize != 0 {
		if !memoryGraphRepairPlanHasCompactEdgeBoundary(receipt) {
			return receipt, "", memoryGraphRepairErr("plan_receipt_invalid", errors.New("dry-run plan receipt edge-log boundary is invalid"))
		}
	}
	if (req.Apply || req.Rollback) && !receipt.Applicable {
		return receipt, "", memoryGraphRepairErr("custody_mismatch", errors.New("preview plan is not authorized for mutation"))
	}
	if receipt.Applicable && (req.Apply || req.Rollback || req.PlanApplicable) && (req.ActorCustodyDigest == "" || receipt.ActorCustodyDigest != req.ActorCustodyDigest || receipt.ActorPrincipalDigest != req.ActorPrincipalDigest || receipt.ActorScopeDigest != req.ActorScopeDigest || receipt.ActorWorkspaceDigest != req.ActorWorkspaceDigest || receipt.ActorInstallDigest != req.ActorInstallDigest || receipt.ActorAuthorityDigest != req.ActorAuthorityDigest) {
		return receipt, "", memoryGraphRepairErr("custody_mismatch", errors.New("dry-run plan receipt custody does not match the authenticated principal"))
	}
	if expires, ok := parseTimeBestEffort(receipt.ExpiresAt); !ok || (!allowExpired && !time.Now().UTC().Before(expires)) {
		return receipt, "", memoryGraphRepairErr("plan_receipt_expired", errors.New("dry-run plan receipt has expired"))
	}
	return receipt, digest, nil
}

func (g *memoryEdgeBackfillGenerator) generateCurrentStateSequenceEdges(ctx context.Context, docs []memoryEdgeBackfillDoc, relation string, confidence float64) {
	type sequenceEntry struct {
		doc     memoryEdgeBackfillDoc
		created time.Time
	}
	groups := map[string][]sequenceEntry{}
	for _, doc := range docs {
		if g.request.Project != "" && !strings.EqualFold(g.request.Project, doc.Project) {
			continue
		}
		group := ""
		switch relation {
		case "same_session":
			if strings.TrimSpace(doc.SessionID) == "" {
				continue
			}
			group = "session:" + strings.ToLower(doc.Project) + ":" + strings.TrimSpace(doc.SessionID)
		case "same_agent":
			if strings.TrimSpace(doc.AgentID) == "" {
				continue
			}
			group = "agent:" + strings.ToLower(doc.Project) + ":" + strings.ToLower(strings.TrimSpace(doc.AgentID)) + ":" + strings.Trim(strings.ToLower(doc.TopicPath), "/")
		default:
			continue
		}
		created := doc.UpdatedAt
		if created.IsZero() {
			created = doc.LastTouch
		}
		groups[group] = append(groups[group], sequenceEntry{doc: doc, created: created})
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		items := groups[key]
		sort.Slice(items, func(i, j int) bool {
			if !items[i].created.Equal(items[j].created) {
				return items[i].created.Before(items[j].created)
			}
			if items[i].doc.EventID != items[j].doc.EventID {
				return items[i].doc.EventID < items[j].doc.EventID
			}
			return items[i].doc.MemoryID < items[j].doc.MemoryID
		})
		for i := 1; i < len(items); i++ {
			candidate, err := memoryEdgeBackfillCandidateEdge(items[i-1].doc.MemoryID, items[i].doc.MemoryID, relation, confidence, items[i].doc.TopicPath, "current_state_sequence", key)
			if err == nil {
				g.add(ctx, candidate)
			}
			if g.ctxErr != nil || g.truncated {
				return
			}
		}
	}
}

func (m *memoryStore) buildMemoryGraphRepairPlan(ctx context.Context, snapshot memoryGraphRepairSnapshot, evidence memoryGraphRepairEdgeEvidence, req memoryGraphRepairRequest) ([]memoryGraphRepairAction, map[string]int, error) {
	if !snapshot.Complete || !evidence.Complete {
		return nil, nil, memoryGraphRepairErr("incomplete_snapshot", errors.New("repair requires complete current-state and edge evidence snapshots"))
	}
	docs := memoryGraphRepairBackfillDocs(snapshot)
	observedAt := req.ObservationAt
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	repairCreatedAt := observedAt.UTC().Format(time.RFC3339Nano)
	generator := &memoryEdgeBackfillGenerator{
		store:    m,
		snapshot: m.memoryReferenceSnapshotFromGraphRepair(snapshot, repairCreatedAt),
		request:  memoryEdgeBackfillRequest{Project: req.Project, IncludeCold: req.IncludeCold, IncludeEphemeral: req.IncludeEphemeral, MinConfidence: req.MinConfidence, MaxCandidates: req.MaxCandidates, TopicPeerLimit: req.TopicPeerLimit, IncludeInferred: req.IncludeInferred, InferredRelation: "inferred_related", InferredPeerLimit: req.InferredPeerLimit, InferredScanLimit: req.InferredScanLimit, InferredMinScore: req.InferredMinScore, InferredMinShared: req.InferredMinShared, InferredMaxPostings: req.InferredMaxPostings, AllowedRelation: map[string]struct{}{}},
		docs:     docs, knownIDs: map[string]memoryEdgeBackfillDoc{}, candidates: []memoryEdgeBackfillCandidate{}, stats: map[string]*memoryEdgeBackfillRelationStats{},
	}
	for _, doc := range docs {
		generator.knownIDs[strings.ToLower(doc.MemoryID)] = doc
	}
	generator.generateReferenceEdges(ctx)
	generator.generateCurrentStateSequenceEdges(ctx, docs, "same_session", 0.98)
	generator.generateTopicEdges(ctx)
	if req.IncludeInferred {
		generator.generateInferredRelatedEdges(ctx)
	}
	generator.generateCurrentStateSequenceEdges(ctx, docs, "same_agent", 0.82)
	if generator.ctxErr != nil {
		return nil, nil, generator.ctxErr
	}
	if generator.truncated {
		return nil, nil, memoryGraphRepairErr("bounded_limit", errors.New("repair candidate generation reached max_candidates"))
	}
	candidates := map[string]memoryEdgeBackfillCandidate{}
	for _, candidate := range generator.candidates {
		candidate.Edge.CreatedAt = repairCreatedAt
		if candidate.Edge.Confidence < req.MinConfidence {
			continue
		}
		if !strings.EqualFold(candidate.Edge.Project, req.Project) {
			continue
		}
		if _, ok := snapshot.Aliases[strings.ToLower(candidate.Edge.SourceID)]; !ok {
			continue
		}
		if _, ok := snapshot.Aliases[strings.ToLower(candidate.Edge.TargetID)]; !ok {
			continue
		}
		candidates[candidate.Edge.EdgeID] = candidate
	}
	actions := make([]memoryGraphRepairAction, 0, len(candidates))
	retireIDs := map[string]struct{}{}
	if req.RetireStaleInferred {
		// Retirement decisions are made from the exact latest durable record per
		// edge identity.  Looking at RetirementSeen or an older row would make a
		// later append-only restoration appear permanently retired.
		latestIDs := make([]string, 0, len(evidence.Latest))
		for edgeID := range evidence.Latest {
			latestIDs = append(latestIDs, edgeID)
		}
		sort.Strings(latestIDs)
		for _, edgeID := range latestIDs {
			row, canonicalErr := memoryGraphRepairCanonicalizeEdge(evidence.Latest[edgeID], snapshot)
			if canonicalErr != nil {
				continue
			}
			if !memoryGraphRepairEdgeInProject(row.Edge, req.Project) {
				continue
			}
			if !memoryGraphRepairEdgeIsInferred(row.Edge) || memoryGraphRepairEdgeRetired(row.Edge) {
				continue
			}
			created, createdOK := parseTimeBestEffort(row.Edge.CreatedAt)
			stale := !createdOK || created.Before(req.StaleAfter) || !row.Bound
			if !stale {
				continue
			}
			if _, exists := retireIDs[row.Edge.EdgeID]; exists {
				continue
			}
			retireIDs[row.Edge.EdgeID] = struct{}{}
			retired := row.Edge
			retired.Lifecycle = "retired"
			retired.Metadata = cloneJSONMap(retired.Metadata)
			if retired.Metadata == nil {
				retired.Metadata = map[string]any{}
			}
			retired.Metadata["repair_action"] = "retire_stale_inferred"
			retired.Metadata["repair_previous_lifecycle"] = normalizeMemoryLifecycle(row.Edge.Lifecycle)
			retired.Metadata["repair_snapshot_digest"] = snapshot.SnapshotDigest
			retired.Metadata["repair_original_edge_id"] = row.Edge.EdgeID
			retired.Provenance = cloneJSONMap(row.Edge.Provenance)
			if retired.Provenance == nil {
				retired.Provenance = map[string]any{}
			}
			for key, value := range memoryGraphRepairClosedProvenance(snapshot, memoryGraphRepairOriginHistorical, row.Edge) {
				retired.Provenance[key] = value
			}
			actions = append(actions, memoryGraphRepairAction{ActionID: "0:" + row.Edge.EdgeID, Kind: "retire", Edge: retired, Previous: row.Edge, Reason: "stale_or_unbound_inferred_evidence"})
		}
	}
	for _, candidate := range candidates {
		edge := candidate.Edge
		if existing, exists := evidence.Latest[edge.EdgeID]; exists && !memoryGraphRepairEdgeRetired(existing) {
			canonical, canonicalErr := memoryGraphRepairCanonicalizeEdge(existing, snapshot)
			created, createdOK := parseTimeBestEffort(existing.CreatedAt)
			bindingCurrent := !memoryGraphEdgeRequiresBinding(existing) || memoryReferenceBindingsSameCurrentState(existing.Binding, edge.Binding)
			if canonicalErr == nil && canonical.Bound && bindingCurrent && (!memoryGraphRepairEdgeIsInferred(existing) || (createdOK && !created.Before(req.StaleAfter))) {
				continue
			}
		}
		edge.Provenance = cloneJSONMap(edge.Provenance)
		edge.Metadata = cloneJSONMap(edge.Metadata)
		if edge.Provenance == nil {
			edge.Provenance = map[string]any{}
		}
		if edge.Metadata == nil {
			edge.Metadata = map[string]any{}
		}
		for key, value := range memoryGraphRepairClosedProvenance(snapshot, memoryGraphRepairOriginCurrentState, memoryEdgeEntry{}) {
			edge.Provenance[key] = value
		}
		edge.Provenance["repair_observed_at"] = repairCreatedAt
		edge.Provenance["source_document_latest_at"] = latestMemoryGraphRepairDocumentTime(snapshot.Docs)
		edge.Metadata["repair_action"] = "canonical_current_state_backfill"
		edge.Metadata["repair_snapshot_digest"] = snapshot.SnapshotDigest
		previous := memoryEdgeEntry{}
		if existing, exists := evidence.Latest[edge.EdgeID]; exists {
			previous = existing
		}
		previousActionID := ""
		if _, retiring := retireIDs[edge.EdgeID]; retiring {
			previousActionID = "0:" + edge.EdgeID
		}
		actions = append(actions, memoryGraphRepairAction{ActionID: "1:" + edge.EdgeID, Kind: "write", Edge: edge, Previous: previous, PreviousActionID: previousActionID, Candidate: candidate, Reason: candidate.Reason})
	}
	for index := range actions {
		action := &actions[index]
		action.SourceStatus, action.TargetStatus, action.BindingReason = memoryGraphRepairBindingFacts(snapshot, action.Edge)
		if action.Previous.EdgeID != "" {
			_, _, action.PreviousBindingReason = memoryGraphRepairBindingFacts(snapshot, action.Previous)
		}
	}
	sort.Slice(actions, func(i, j int) bool { return actions[i].ActionID < actions[j].ActionID })
	counts := map[string]int{"retire": 0, "write": 0, "actions": len(actions)}
	for _, action := range actions {
		counts[action.Kind]++
	}
	return actions, counts, nil
}

func latestMemoryGraphRepairDocumentTime(docs []memoryGraphRepairDoc) string {
	latest := time.Time{}
	for _, doc := range docs {
		if doc.UpdatedAt.After(latest) {
			latest = doc.UpdatedAt
		}
	}
	if latest.IsZero() {
		return ""
	}
	return latest.UTC().Format(time.RFC3339Nano)
}

type memoryGraphRepairAction struct {
	ActionID         string                      `json:"action_id"`
	Kind             string                      `json:"kind"`
	Edge             memoryEdgeEntry             `json:"edge"`
	Previous         memoryEdgeEntry             `json:"previous,omitempty"`
	PreviousActionID string                      `json:"previous_action_id,omitempty"`
	Candidate        memoryEdgeBackfillCandidate `json:"candidate,omitempty"`
	Reason           string                      `json:"reason"`
	// These frozen endpoint facts let a continuation report and authenticate a
	// chunk without reloading the full current-state alias index.
	SourceStatus          string `json:"source_status,omitempty"`
	TargetStatus          string `json:"target_status,omitempty"`
	BindingReason         string `json:"binding_reason,omitempty"`
	PreviousBindingReason string `json:"previous_binding_reason,omitempty"`
}

// memoryGraphRepairImmutableEdgeTuple is the action-owned portion of an edge
// append. Server repair metadata and provenance are intentionally excluded:
// those fields are added while committing the action and carry the append
// marker. Every normalized identity/content field is retained so an append
// with a copied EdgeID but different endpoints or relation cannot recover an
// immutable action.
type memoryGraphRepairImmutableEdgeTuple struct {
	EdgeID   string
	SourceID string
	TargetID string
	Relation string
	Project  string
}

func memoryGraphRepairImmutableEdgeTupleOf(edge memoryEdgeEntry) (memoryGraphRepairImmutableEdgeTuple, bool) {
	normalized, err := edge.normalized()
	if err != nil || strings.TrimSpace(edge.EdgeID) == "" || edge.EdgeID != normalized.EdgeID {
		return memoryGraphRepairImmutableEdgeTuple{}, false
	}
	return memoryGraphRepairImmutableEdgeTuple{
		EdgeID: normalized.EdgeID, SourceID: normalized.SourceID, TargetID: normalized.TargetID,
		Relation: normalized.Relation, Project: normalized.Project,
	}, true
}

func memoryGraphRepairImmutableEdgeTupleMatches(expected, retained memoryEdgeEntry) bool {
	expectedTuple, expectedOK := memoryGraphRepairImmutableEdgeTupleOf(expected)
	retainedTuple, retainedOK := memoryGraphRepairImmutableEdgeTupleOf(retained)
	return expectedOK && retainedOK && expectedTuple == retainedTuple
}

func memoryGraphRepairActionApplied(action memoryGraphRepairAction, evidence memoryGraphRepairEdgeEvidence, snapshotDigest string, staleAfter time.Time) bool {
	if action.Kind == "retire" {
		latest, exists := evidence.Latest[action.Edge.EdgeID]
		if !exists || !memoryGraphRepairEdgeRetired(latest) {
			return false
		}
		// A retirement is idempotent only when the latest durable record is the
		// requested retirement (or an equivalent already-retired record).  The
		// historical RetirementSeen bit is deliberately not authoritative.
		return memoryGraphRepairEdgeMetadataRepair(latest, "repair_snapshot_digest") == snapshotDigest &&
			memoryGraphRepairEdgeMetadataRepair(latest, "repair_action_id") == action.ActionID
	}
	existing, exists := evidence.Latest[action.Edge.EdgeID]
	if !exists || memoryGraphRepairEdgeRetired(existing) {
		return false
	}
	if memoryGraphRepairEdgeMetadataRepair(existing, "repair_snapshot_digest") == snapshotDigest {
		return true
	}
	// An existing active edge with the same canonical identity is valid evidence;
	// only a stale inferred edge is forced through retirement/replacement.
	if !memoryGraphRepairEdgeIsInferred(existing) {
		return true
	}
	created, createdOK := parseTimeBestEffort(existing.CreatedAt)
	return createdOK && created.After(staleAfter)
}

func memoryGraphRepairActionRecovered(action memoryGraphRepairAction, evidence memoryGraphRepairEdgeEvidence, runID, planRef, planDigest, actionDigest, custodyDigest string) (memoryEdgeEntry, bool) {
	if strings.TrimSpace(runID) == "" {
		return memoryEdgeEntry{}, false
	}
	if rows := evidence.RepairActionRows[runID]; rows != nil {
		if edge, ok := rows[action.ActionID]; ok {
			// The marker authenticates the append provenance, not the action's
			// immutable target identity. A forged row carrying the right run and
			// action marker must never recover a different edge.
			if !memoryGraphRepairImmutableEdgeTupleMatches(action.Edge, edge) {
				return memoryEdgeEntry{}, false
			}
			if !memoryGraphRepairServerAppendMarkerValid("repair", edge) {
				return memoryEdgeEntry{}, false
			}
			if memoryGraphRepairEdgeMetadataRepair(edge, "repair_plan_ref") != planRef ||
				memoryGraphRepairEdgeMetadataRepair(edge, "repair_plan_digest") != planDigest ||
				memoryGraphRepairEdgeMetadataRepair(edge, "repair_action_digest") != actionDigest ||
				memoryGraphRepairEdgeMetadataRepair(edge, "repair_custody_digest") != custodyDigest {
				return memoryEdgeEntry{}, false
			}
			latest, latestOK := evidence.Latest[edge.EdgeID]
			if !latestOK {
				return memoryEdgeEntry{}, false
			}
			if sha256Hex(string(mustJSON(latest))) != sha256Hex(string(mustJSON(edge))) {
				// A later append-only restoration supersedes the crashed repair
				// record unless it is a later action from the same immutable run.
				if memoryGraphRepairEdgeMetadataRepair(latest, "repair_run_id") != runID || anyToInt(latest.Metadata["repair_cursor"], -1) <= anyToInt(edge.Metadata["repair_cursor"], -1) {
					return memoryEdgeEntry{}, false
				}
			}
			if memoryGraphRepairEdgeMetadataRepair(edge, "repair_run_id") != runID || memoryGraphRepairEdgeMetadataRepair(edge, "repair_action_id") != action.ActionID {
				return memoryEdgeEntry{}, false
			}
			if action.Kind == "retire" && !memoryGraphRepairEdgeRetired(edge) {
				return memoryEdgeEntry{}, false
			}
			return edge, true
		}
	}
	return memoryEdgeEntry{}, false
}

func memoryGraphRepairServerAppendMarker(domain string, edge memoryEdgeEntry) string {
	copy := edge
	if normalized, err := copy.normalized(); err == nil {
		copy = normalized
	}
	copy.Metadata = cloneJSONMap(edge.Metadata)
	if copy.Metadata == nil {
		copy.Metadata = map[string]any{}
	}
	delete(copy.Metadata, strings.TrimSpace(domain)+"_server_append_marker")
	return "sha256:" + sha256Hex("contextlattice-memory-graph-"+strings.TrimSpace(domain)+"-append\x00"+string(mustJSON(copy)))
}

func memoryGraphRepairServerAppendMarkerValid(domain string, edge memoryEdgeEntry) bool {
	key := strings.TrimSpace(domain) + "_server_append_marker"
	return memoryGraphRepairEdgeMetadataRepair(edge, key) == memoryGraphRepairServerAppendMarker(domain, edge)
}

func memoryGraphRepairAppendBefore(edge memoryEdgeEntry) (uint64, string, string, bool) {
	generationValue := anyToInt(edge.Metadata["repair_edge_log_generation_before"], 0)
	digest := memoryGraphRepairEdgeMetadataRepair(edge, "repair_edge_log_digest_before")
	contentDigest := memoryGraphRepairEdgeMetadataRepair(edge, "repair_edge_log_content_digest_before")
	validDigest := func(value string) bool {
		return strings.HasPrefix(value, "sha256:") && isHexDigest(strings.TrimPrefix(value, "sha256:"))
	}
	if generationValue < 1 || !validDigest(digest) || !validDigest(contentDigest) {
		return 0, "", "", false
	}
	return uint64(generationValue), digest, contentDigest, true
}

func memoryGraphRepairCheckpointPath(m *memoryStore, project string) string {
	return filepath.Join(m.policy.rootPath, "_contextlattice", "memory_graph_repair_active_"+sha256Hex(strings.ToLower(strings.TrimSpace(project)))[:24]+".json")
}

func memoryGraphRepairPlanCheckpointPath(m *memoryStore, planRef string) string {
	return filepath.Join(m.policy.rootPath, "_contextlattice", "memory_graph_repair_checkpoint_"+sha256Hex(strings.TrimSpace(planRef))[:24]+".json")
}

func memoryGraphRepairLockPath(m *memoryStore, project string) string {
	return filepath.Join(m.policy.rootPath, "_contextlattice", "memory_graph_repair_lock_"+sha256Hex(strings.ToLower(strings.TrimSpace(project)))[:24]+".lock")
}

func memoryGraphRepairActionRowsDigest(rows map[string]memoryEdgeEntry) string {
	if rows == nil {
		rows = map[string]memoryEdgeEntry{}
	}
	return "sha256:" + sha256Hex(string(mustJSON(rows)))
}

func memoryGraphRepairCheckpointDigest(checkpoint memoryGraphRepairCheckpoint) string {
	checkpoint.CheckpointDigest = ""
	checkpoint.Custody = nil
	checkpoint.UpdatedAt = ""
	return "sha256:" + sha256Hex(string(mustJSON(checkpoint)))
}

func memoryGraphRepairRollbackCheckpointDigest(checkpoint memoryGraphRepairRollbackCheckpoint) string {
	checkpoint.CheckpointDigest = ""
	checkpoint.Custody = nil
	checkpoint.UpdatedAt = ""
	return "sha256:" + sha256Hex(string(mustJSON(checkpoint)))
}

func memoryGraphRepairValidDigest(value string) bool {
	value = strings.TrimSpace(strings.ToLower(value))
	return strings.HasPrefix(value, "sha256:") && len(strings.TrimPrefix(value, "sha256:")) == 64 && isHexDigest(strings.TrimPrefix(value, "sha256:"))
}

func memoryGraphRepairActionDigestRow(action memoryGraphRepairAction, edge memoryEdgeEntry) memoryGraphRepairEdgeRow {
	return memoryGraphRepairEdgeRow{
		Edge: edge, RawDigest: memoryGraphRepairOptionalEdgeDigest(edge),
		Bound: action.BindingReason == "bound_current_state",
	}
}

// memoryGraphRepairExpectedCheckpointDigest recomputes the append-chain
// checkpoint digest from the immutable plan prefix and the bounded durable
// action rows. EdgeDigestAfter is an output of this calculation, never a
// trusted seed supplied by a checkpoint artifact.
func memoryGraphRepairExpectedCheckpointDigest(plan memoryGraphRepairPlanReceipt, checkpoint memoryGraphRepairCheckpoint) (string, error) {
	if plan.EdgeDigestAlgorithm != memoryGraphRepairDigestAlgorithm || checkpoint.EdgeDigestAlgorithm != memoryGraphRepairDigestAlgorithm || !memoryGraphRepairValidDigest(plan.EdgeDigest) {
		return "", errors.New("repair checkpoint digest algorithm or plan prefix is invalid")
	}
	if checkpoint.Cursor < 0 || checkpoint.Cursor > len(plan.Actions) {
		return "", errors.New("repair checkpoint cursor is outside the immutable action plan")
	}
	if checkpoint.ActionRows == nil {
		checkpoint.ActionRows = map[string]memoryEdgeEntry{}
	}
	if len(checkpoint.ActionRows) != checkpoint.Cursor {
		return "", errors.New("repair checkpoint action index is incomplete")
	}
	index := &memoryGraphRepairDigestIndex{chain: plan.EdgeDigest, chainAvailable: true}
	for position := 0; position < checkpoint.Cursor; position++ {
		action := plan.Actions[position]
		edge, ok := checkpoint.ActionRows[action.ActionID]
		if !ok || !memoryGraphRepairImmutableEdgeTupleMatches(action.Edge, edge) {
			return "", fmt.Errorf("repair checkpoint action %s does not identify its immutable edge", action.ActionID)
		}
		if memoryGraphRepairEdgeMetadataRepair(edge, "repair_action_id") != action.ActionID || !memoryGraphRepairServerAppendMarkerValid("repair", edge) {
			return "", fmt.Errorf("repair checkpoint action %s is not an authenticated repair append", action.ActionID)
		}
		index.add(memoryGraphRepairActionDigestRow(action, edge))
	}
	expected := index.digest()
	if !strings.EqualFold(strings.TrimSpace(checkpoint.EdgeDigestAfter), expected) {
		return "", fmt.Errorf("repair checkpoint edge digest does not match its authenticated action prefix: got=%s expected=%s cursor=%d", checkpoint.EdgeDigestAfter, expected, checkpoint.Cursor)
	}
	return expected, nil
}

func memoryGraphRepairExpectedRollbackDigest(plan memoryGraphRepairPlanReceipt, apply memoryGraphRepairCheckpoint, rollback memoryGraphRepairRollbackCheckpoint) (string, error) {
	base, err := memoryGraphRepairExpectedCheckpointDigest(plan, apply)
	if err != nil {
		return "", err
	}
	if rollback.EdgeDigestAlgorithm != memoryGraphRepairDigestAlgorithm || rollback.Cursor < 0 || rollback.Cursor > rollback.AppliedActionCount || rollback.AppliedActionCount > len(plan.Actions) {
		return "", errors.New("rollback checkpoint digest algorithm or cursor is invalid")
	}
	if rollback.RollbackRows == nil {
		rollback.RollbackRows = map[string]memoryEdgeEntry{}
	}
	if len(rollback.RollbackRows) != rollback.Cursor {
		return "", errors.New("rollback checkpoint action index is incomplete")
	}
	index := &memoryGraphRepairDigestIndex{chain: base, chainAvailable: true}
	for offset := 0; offset < rollback.Cursor; offset++ {
		action := plan.Actions[rollback.AppliedActionCount-1-offset]
		edge, ok := rollback.RollbackRows[action.ActionID]
		if !ok || !memoryGraphRepairImmutableEdgeTupleMatches(action.Edge, edge) {
			return "", fmt.Errorf("rollback checkpoint action %s does not identify its immutable edge", action.ActionID)
		}
		if memoryGraphRepairEdgeMetadataRepair(edge, "rollback_action_id") != action.ActionID || !memoryGraphRepairServerAppendMarkerValid("rollback", edge) {
			return "", fmt.Errorf("rollback checkpoint action %s is not an authenticated rollback append", action.ActionID)
		}
		index.add(memoryGraphRepairActionDigestRow(action, edge))
	}
	expected := index.digest()
	if !strings.EqualFold(strings.TrimSpace(rollback.EdgeDigestAfter), expected) {
		return "", fmt.Errorf("rollback checkpoint edge digest does not match its authenticated action prefix: got=%s expected=%s cursor=%d", rollback.EdgeDigestAfter, expected, rollback.Cursor)
	}
	return expected, nil
}

func memoryGraphRepairPlanHasCompactEdgeBoundary(plan memoryGraphRepairPlanReceipt) bool {
	if plan.EdgeDigestAlgorithm != memoryGraphRepairDigestAlgorithm || plan.EdgeLogFileSize < 0 || plan.EdgeLogFileSize > memoryGraphRepairMaxEdgeBytes ||
		!memoryGraphRepairValidDigest(plan.CurrentStateDigest) ||
		strings.TrimSpace(plan.EdgeLogContentDigest) == "" ||
		strings.TrimSpace(plan.EdgeLogContentHashState) == "" ||
		plan.EdgeLogContentHashedBytes != plan.EdgeLogFileSize {
		return false
	}
	_, err := memoryEdgeLogHashFromState(memoryEdgeLogState{
		ContentDigest: plan.EdgeLogContentDigest, ContentHashState: plan.EdgeLogContentHashState,
		ContentHashedBytes: plan.EdgeLogContentHashedBytes, FileSize: plan.EdgeLogFileSize,
	})
	return err == nil
}

func memoryGraphRepairSnapshotFromPlan(plan memoryGraphRepairPlanReceipt) memoryGraphRepairSnapshot {
	return memoryGraphRepairSnapshot{
		Project: plan.Project, SnapshotDigest: plan.SnapshotDigest,
		CurrentStateDigest: plan.CurrentStateDigest,
		KeyGeneration:      plan.KeyGeneration, TopicGeneration: plan.TopicGeneration,
		IndexedCount: plan.SnapshotIndexedCount, ExcludedCount: plan.SnapshotExcludedCount,
		Complete: true,
	}
}

func memoryGraphRepairPlanBoundaryCheckpoint(plan memoryGraphRepairPlanReceipt, planDigest string) memoryGraphRepairCheckpoint {
	seed := strings.ToLower(plan.Project) + ":" + plan.ReceiptRef + ":" + planDigest
	return memoryGraphRepairCheckpoint{
		CheckpointID: "repair_" + sha256Hex(seed)[:24], RunID: "run_" + sha256Hex(seed)[:32],
		Project: plan.Project, PlanReceiptRef: plan.ReceiptRef, PlanReceiptDigest: planDigest,
		SnapshotDigest: plan.SnapshotDigest, PolicyDigest: plan.PolicyDigest, ActionDigest: plan.ActionDigest,
		StaleAfter: plan.StaleAfter, ObservedAt: plan.ObservedAt, EdgeDigestAfter: plan.EdgeDigest, EdgeDigestAlgorithm: plan.EdgeDigestAlgorithm,
		KeyGeneration: plan.KeyGeneration, TopicGeneration: plan.TopicGeneration,
		EdgeLogGeneration: plan.EdgeLogGeneration, EdgeLogDigest: plan.EdgeLogDigest,
		EdgeLogContentDigest: plan.EdgeLogContentDigest, EdgeLogContentHashState: plan.EdgeLogContentHashState,
		EdgeLogContentHashedBytes: plan.EdgeLogContentHashedBytes, EdgeLogFileSize: plan.EdgeLogFileSize,
		EdgeLogFileIdentity: plan.EdgeLogFileIdentity, ActionRows: map[string]memoryEdgeEntry{},
		BindingState: cloneJSONMap(plan.BindingBefore), ResolutionState: cloneJSONMap(plan.ResolutionProof),
	}
}

func memoryGraphRepairContinuationEvidence(plan memoryGraphRepairPlanReceipt, checkpoint memoryGraphRepairCheckpoint, rollback *memoryGraphRepairRollbackCheckpoint) (memoryGraphRepairEdgeEvidence, error) {
	chain := plan.EdgeDigest
	if rollback == nil {
		var err error
		chain, err = memoryGraphRepairExpectedCheckpointDigest(plan, checkpoint)
		if err != nil {
			return memoryGraphRepairEdgeEvidence{}, err
		}
	} else {
		applyCheckpoint := checkpoint
		var err error
		chain, err = memoryGraphRepairExpectedRollbackDigest(plan, applyCheckpoint, *rollback)
		if err != nil {
			return memoryGraphRepairEdgeEvidence{}, err
		}
	}
	evidence := memoryGraphRepairEdgeEvidence{
		Latest: map[string]memoryEdgeEntry{}, RetirementSeen: map[string]bool{},
		RepairRuns: map[string]map[string]struct{}{}, RepairActionRows: map[string]map[string]memoryEdgeEntry{},
		RollbackActionRows: map[string]map[string]memoryEdgeEntry{}, Complete: true,
		Project: plan.Project, DigestIndex: &memoryGraphRepairDigestIndex{chain: chain, chainAvailable: chain != ""},
		Continuation: true, LogGeneration: checkpoint.EdgeLogGeneration, LogDigest: checkpoint.EdgeLogDigest,
		LogContentDigest: checkpoint.EdgeLogContentDigest, LogFileSize: checkpoint.EdgeLogFileSize, LogFileIdentity: checkpoint.EdgeLogFileIdentity,
		LogContentHashState: checkpoint.EdgeLogContentHashState, LogContentHashedBytes: checkpoint.EdgeLogContentHashedBytes,
	}
	if evidence.DigestIndex.chain == "" {
		evidence.DigestIndex.chain = "sha256:" + sha256Hex(memoryGraphRepairDigestIndexSchema+"\x00empty")
		evidence.DigestIndex.chainAvailable = true
	}
	if checkpoint.ActionRows != nil {
		for actionID, edge := range checkpoint.ActionRows {
			if edge.EdgeID == "" {
				return memoryGraphRepairEdgeEvidence{}, fmt.Errorf("repair checkpoint action %s has an empty edge identity", actionID)
			}
			if current, exists := evidence.Latest[edge.EdgeID]; !exists || anyToInt(edge.Metadata["repair_cursor"], -1) >= anyToInt(current.Metadata["repair_cursor"], -1) {
				evidence.Latest[edge.EdgeID] = edge
			}
			runID := memoryGraphRepairEdgeMetadataRepair(edge, "repair_run_id")
			if runID != "" {
				if evidence.RepairActionRows[runID] == nil {
					evidence.RepairActionRows[runID] = map[string]memoryEdgeEntry{}
				}
				evidence.RepairActionRows[runID][actionID] = edge
			}
		}
	}
	if rollback != nil {
		if rollback.RollbackRows != nil {
			if evidence.RollbackActionRows[rollback.RunID] == nil {
				evidence.RollbackActionRows[rollback.RunID] = map[string]memoryEdgeEntry{}
			}
			for actionID, edge := range rollback.RollbackRows {
				evidence.RollbackActionRows[rollback.RunID][actionID] = edge
				if current, exists := evidence.Latest[edge.EdgeID]; !exists || anyToInt(edge.Metadata["rollback_cursor"], -1) >= anyToInt(current.Metadata["rollback_cursor"], -1) {
					evidence.Latest[edge.EdgeID] = edge
				}
			}
		}
		evidence.LogGeneration = rollback.EdgeLogGeneration
		evidence.LogDigest = rollback.EdgeLogDigest
		evidence.LogContentDigest = rollback.EdgeLogContentDigest
		evidence.LogContentHashState = rollback.EdgeLogContentHashState
		evidence.LogContentHashedBytes = rollback.EdgeLogContentHashedBytes
		evidence.LogFileSize = rollback.EdgeLogFileSize
		evidence.LogFileIdentity = rollback.EdgeLogFileIdentity
	}
	// Actions not yet represented by a durable row still have a frozen
	// predecessor in the immutable plan.  Seeding those identities lets the
	// bounded CAS check inspect only the action set.
	for _, action := range plan.Actions {
		if _, exists := evidence.Latest[action.Edge.EdgeID]; exists {
			continue
		}
		if action.Previous.EdgeID != "" {
			evidence.Latest[action.Previous.EdgeID] = action.Previous
		}
	}
	evidence.Digest = evidence.DigestIndex.digest()
	return evidence, nil
}

// refreshMemoryGraphRepairContinuationEvidence authenticates only the bytes
// appended after the durable checkpoint.  A project row is accepted from the
// suffix only when it is a server-marked action in the current bounded chunk;
// any other project mutation fails closed.  Unrelated-project bytes are still
// covered by the edge-log generation/content CAS but need not be parsed into
// the project index.
func (m *memoryStore) refreshMemoryGraphRepairContinuationEvidence(ctx context.Context, evidence *memoryGraphRepairEdgeEvidence, plan memoryGraphRepairPlanReceipt, checkpoint memoryGraphRepairCheckpoint, rollback *memoryGraphRepairRollbackCheckpoint, state memoryEdgeLogState, pending map[string]struct{}) error {
	if evidence == nil || !evidence.Continuation {
		return errors.New("repair continuation evidence is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if state.FileSize < evidence.LogFileSize || evidence.LogFileSize < 0 || state.FileSize > memoryGraphRepairMaxEdgeBytes {
		return errMemoryEdgeLogChangedDuringRead
	}
	if evidence.LogContentHashState != "" {
		if evidence.LogContentHashedBytes != evidence.LogFileSize {
			return errMemoryEdgeLogChangedDuringRead
		}
		if _, hashErr := memoryEdgeLogHashFromState(memoryEdgeLogState{
			ContentDigest: evidence.LogContentDigest, ContentHashState: evidence.LogContentHashState,
			ContentHashedBytes: evidence.LogContentHashedBytes, FileSize: evidence.LogFileSize,
		}); hashErr != nil {
			return errMemoryEdgeLogChangedDuringRead
		}
	}
	if state.FileSize == evidence.LogFileSize {
		if state.Generation != evidence.LogGeneration || state.Digest != evidence.LogDigest || state.ContentDigest != evidence.LogContentDigest || (evidence.LogFileIdentity != "" && state.FileIdentity != evidence.LogFileIdentity) {
			return errMemoryEdgeLogChangedDuringRead
		}
		evidence.LogFileIdentity = state.FileIdentity
		return nil
	}
	stamp := memoryEdgeLogFileStamp{Exists: true, Size: state.FileSize, Identity: state.FileIdentity, ModTimeNanos: state.FileModTimeNanos, ChangeToken: state.FileChangeToken}
	suffix, err := m.readMemoryEdgeLogSuffixLocked(ctx, evidence.LogFileSize, state.FileSize, memoryGraphRepairMaxEdgeBytes, stamp)
	if err != nil {
		return err
	}
	if evidence.LogContentHashState != "" && evidence.LogContentHashedBytes == evidence.LogFileSize {
		prefixHash, hashErr := memoryEdgeLogHashFromState(memoryEdgeLogState{ContentDigest: evidence.LogContentDigest, ContentHashState: evidence.LogContentHashState, ContentHashedBytes: evidence.LogContentHashedBytes, FileSize: evidence.LogFileSize})
		if hashErr != nil {
			return errMemoryEdgeLogChangedDuringRead
		}
		if _, writeErr := prefixHash.Write(suffix); writeErr != nil {
			return writeErr
		}
		if got := "sha256:" + fmt.Sprintf("%x", prefixHash.Sum(nil)); got != state.ContentDigest {
			return errMemoryEdgeLogChangedDuringRead
		}
	} else if state.ParentFileSize != evidence.LogFileSize || state.ParentContentDigest != evidence.LogContentDigest || (evidence.LogFileIdentity != "" && state.ParentFileIdentity != evidence.LogFileIdentity) {
		// Legacy checkpoints without a hash midstate are safe only for one
		// append generation; newer checkpoints take the O(new-bytes) hash path
		// above and support multiple crash-prefix appends.
		return errMemoryEdgeLogChangedDuringRead
	}
	if pending == nil {
		pending = map[string]struct{}{}
	}
	touchedIDs := make(map[string]struct{}, len(plan.Actions))
	expectedActions := make(map[string]memoryGraphRepairAction, len(plan.Actions))
	expectedActionBound := make(map[string]bool, len(plan.Actions))
	for _, action := range plan.Actions {
		touchedIDs[action.Edge.EdgeID] = struct{}{}
		expectedActions[action.ActionID] = action
		expectedActionBound[action.ActionID] = action.BindingReason == "bound_current_state"
	}
	scanner := bufio.NewScanner(bytes.NewReader(suffix))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var edge memoryEdgeEntry
		if err := json.Unmarshal([]byte(line), &edge); err != nil {
			return memoryGraphRepairErr("snapshot_drift", errors.New("repair continuation suffix contains invalid edge data"))
		}
		if !memoryGraphRepairEdgeInProject(edge, plan.Project) {
			continue
		}
		_, touchedAction := touchedIDs[edge.EdgeID]
		actionID := ""
		validAction := false
		if rollback != nil {
			runID := memoryGraphRepairEdgeMetadataRepair(edge, "rollback_run_id")
			actionID = memoryGraphRepairEdgeMetadataRepair(edge, "rollback_action_id")
			_, pendingAction := pending[actionID]
			expectedAction, expectedActionOK := expectedActions[actionID]
			tupleMatches := expectedActionOK && memoryGraphRepairImmutableEdgeTupleMatches(expectedAction.Edge, edge)
			if runID == rollback.RunID && actionID != "" && !tupleMatches {
				return memoryGraphRepairErr("snapshot_drift", fmt.Errorf("rollback continuation action %s carries an edge tuple different from the immutable plan", actionID))
			}
			validAction = runID == rollback.RunID && actionID != "" && pendingAction && tupleMatches && memoryGraphRepairServerAppendMarkerValid("rollback", edge)
			if validAction {
				if evidence.RollbackActionRows[rollback.RunID] == nil {
					evidence.RollbackActionRows[rollback.RunID] = map[string]memoryEdgeEntry{}
				}
				evidence.RollbackActionRows[rollback.RunID][actionID] = edge
			}
		} else {
			runID := memoryGraphRepairEdgeMetadataRepair(edge, "repair_run_id")
			actionID = memoryGraphRepairEdgeMetadataRepair(edge, "repair_action_id")
			_, pendingAction := pending[actionID]
			expectedAction, expectedActionOK := expectedActions[actionID]
			tupleMatches := expectedActionOK && memoryGraphRepairImmutableEdgeTupleMatches(expectedAction.Edge, edge)
			if runID == checkpoint.RunID && actionID != "" && !tupleMatches {
				return memoryGraphRepairErr("snapshot_drift", fmt.Errorf("repair continuation action %s carries an edge tuple different from the immutable plan", actionID))
			}
			validAction = runID == checkpoint.RunID && actionID != "" && pendingAction && tupleMatches && memoryGraphRepairServerAppendMarkerValid("repair", edge)
			if validAction {
				if evidence.RepairActionRows[checkpoint.RunID] == nil {
					evidence.RepairActionRows[checkpoint.RunID] = map[string]memoryEdgeEntry{}
				}
				evidence.RepairActionRows[checkpoint.RunID][actionID] = edge
			}
		}
		if !validAction {
			if !touchedAction {
				return memoryGraphRepairErr("snapshot_drift", fmt.Errorf("unexpected project edge appended during repair continuation: %s", edge.EdgeID))
			}
			// A touched edge with no server marker is retained as the latest
			// candidate so the action/rollback CAS layer emits its precise
			// superseding-write refusal.  It never enters the authenticated digest
			// midstate or becomes a repair action row.
			evidence.Latest[edge.EdgeID] = edge
			continue
		}
		row := memoryGraphRepairEdgeRow{Edge: edge, RawDigest: "sha256:" + sha256Hex(line), Bound: expectedActionBound[actionID], FromMemory: true}
		// Pending crash rows are part of the next committed chunk, so include
		// each suffix row in the append midstate exactly once.  The apply/rollback
		// loops use the indexed row and do not record it a second time.
		evidence.DigestIndex.add(row)
		evidence.Latest[edge.EdgeID] = edge
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	evidence.LogGeneration = state.Generation
	evidence.LogDigest = state.Digest
	evidence.LogContentDigest = state.ContentDigest
	evidence.LogFileSize = state.FileSize
	evidence.LogFileIdentity = state.FileIdentity
	evidence.ScannedBytes += int64(len(suffix))
	evidence.ScannedLines += bytes.Count(suffix, []byte{'\n'})
	evidence.Digest = evidence.projectDigest()
	return nil
}

type memoryGraphRepairCheckpoint struct {
	SchemaID                  string `json:"schema_id"`
	Version                   int    `json:"version"`
	CheckpointID              string `json:"checkpoint_id"`
	RunID                     string `json:"run_id"`
	Project                   string `json:"project"`
	PlanReceiptRef            string `json:"plan_receipt_ref"`
	PlanReceiptDigest         string `json:"plan_receipt_digest"`
	SnapshotDigest            string `json:"snapshot_digest"`
	PolicyDigest              string `json:"policy_digest"`
	ActionDigest              string `json:"action_digest"`
	StaleAfter                string `json:"stale_after"`
	ObservedAt                string `json:"observed_at"`
	EdgeDigestBefore          string `json:"edge_digest_before"`
	EdgeDigestAfter           string `json:"edge_digest_after"`
	EdgeDigestAlgorithm       string `json:"edge_digest_algorithm,omitempty"`
	KeyGeneration             uint64 `json:"key_generation"`
	TopicGeneration           uint64 `json:"topic_generation"`
	EdgeLogGeneration         uint64 `json:"edge_log_generation"`
	EdgeLogDigest             string `json:"edge_log_digest"`
	EdgeLogContentDigest      string `json:"edge_log_content_digest"`
	EdgeLogContentHashState   string `json:"edge_log_content_hash_state,omitempty"`
	EdgeLogContentHashedBytes int64  `json:"edge_log_content_hashed_bytes,omitempty"`
	EdgeLogFileSize           int64  `json:"edge_log_file_size"`
	EdgeLogFileIdentity       string `json:"edge_log_file_identity,omitempty"`
	// ActionRows is the compact authenticated repair-row index.  It is bounded
	// by the immutable action plan and is used for crash recovery/rollback
	// without scanning the historical edge log.
	ActionRows           map[string]memoryEdgeEntry `json:"action_rows,omitempty"`
	ActionRowsDigest     string                     `json:"action_rows_digest,omitempty"`
	CheckpointDigest     string                     `json:"checkpoint_digest,omitempty"`
	BindingState         map[string]any             `json:"binding_state,omitempty"`
	ResolutionState      map[string]any             `json:"resolution_state,omitempty"`
	Cursor               int                        `json:"cursor"`
	TotalActions         int                        `json:"total_actions"`
	Counts               map[string]int             `json:"counts"`
	Status               string                     `json:"status"`
	LastReceiptDigest    string                     `json:"last_receipt_digest,omitempty"`
	ActorPrincipalDigest string                     `json:"actor_principal_digest,omitempty"`
	ActorScopeDigest     string                     `json:"actor_scope_digest,omitempty"`
	ActorCustodyDigest   string                     `json:"actor_custody_digest,omitempty"`
	UpdatedAt            string                     `json:"updated_at"`
	Custody              map[string]any             `json:"custody"`
}

type memoryGraphRepairActivePointer struct {
	SchemaID     string `json:"schema_id"`
	Version      int    `json:"version"`
	Project      string `json:"project"`
	PlanRef      string `json:"plan_ref"`
	CheckpointID string `json:"checkpoint_id"`
	Status       string `json:"status"`
	UpdatedAt    string `json:"updated_at"`
}

func (m *memoryStore) loadMemoryGraphRepairCheckpoint(project string, planRefs ...string) (memoryGraphRepairCheckpoint, bool, error) {
	planRef := ""
	if len(planRefs) > 0 {
		planRef = planRefs[0]
	}
	if planRef == "" {
		pointer, exists, err := m.loadMemoryGraphRepairActivePointer(project)
		if err != nil || !exists {
			return memoryGraphRepairCheckpoint{}, false, err
		}
		planRef = pointer.PlanRef
	}
	path := memoryGraphRepairPlanCheckpointPath(m, planRef)
	raw, err := readMemoryGraphRepairArtifact(path)
	if errors.Is(err, os.ErrNotExist) {
		return memoryGraphRepairCheckpoint{}, false, nil
	}
	if err != nil {
		return memoryGraphRepairCheckpoint{}, false, memoryGraphRepairErr("checkpoint_io", err)
	}
	var checkpoint memoryGraphRepairCheckpoint
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		return checkpoint, false, errors.New("memory graph repair checkpoint is invalid")
	}
	if checkpoint.SchemaID != memoryGraphRepairCheckpointID || checkpoint.Version != 1 || checkpoint.Project != project || checkpoint.PlanReceiptRef != planRef || checkpoint.CheckpointID == "" {
		return checkpoint, false, errors.New("memory graph repair checkpoint schema or project mismatch")
	}
	if err := memoryGraphRepairCustodyMatches(m, checkpoint.Custody); err != nil {
		return checkpoint, false, memoryGraphRepairErr("custody_mismatch", err)
	}
	if !memoryGraphRepairValidDigest(checkpoint.CheckpointDigest) || checkpoint.CheckpointDigest != memoryGraphRepairCheckpointDigest(checkpoint) {
		return checkpoint, false, errors.New("memory graph repair checkpoint digest mismatch")
	}
	if checkpoint.ActionRowsDigest != "" && checkpoint.ActionRowsDigest != memoryGraphRepairActionRowsDigest(checkpoint.ActionRows) {
		return checkpoint, false, errors.New("memory graph repair checkpoint action index digest mismatch")
	}
	return checkpoint, true, nil
}

func memoryGraphRepairCustody(m *memoryStore) map[string]any {
	root := ""
	if m != nil {
		root, _ = ownerOnlyRootIdentity(m.policy.rootPath)
	}
	return map[string]any{"store_ref": ownerOnlyStoreRef(memoryGraphRepairStoreKind), "root_identity": root, "source_index": "gateway_current_state_index", "raw_store_scanned": false}
}

func memoryGraphRepairCustodyMatches(m *memoryStore, persisted map[string]any) error {
	if m == nil {
		return errors.New("memory graph repair store is unavailable")
	}
	if persisted == nil {
		return errors.New("memory graph repair artifact custody is missing")
	}
	expected := memoryGraphRepairCustody(m)
	if anyToString(persisted["store_ref"]) != anyToString(expected["store_ref"]) || anyToString(persisted["root_identity"]) != anyToString(expected["root_identity"]) {
		return errors.New("memory graph repair artifact custody does not match this store root")
	}
	return nil
}

func memoryGraphRepairCustodyForRequest(m *memoryStore, req memoryGraphRepairRequest) map[string]any {
	custody := memoryGraphRepairCustody(m)
	if req.ActorPrincipalDigest != "" {
		custody["actor_principal_digest"] = req.ActorPrincipalDigest
	}
	if req.ActorScopeDigest != "" {
		custody["actor_scope_digest"] = req.ActorScopeDigest
	}
	for key, value := range map[string]string{"actor_workspace_digest": req.ActorWorkspaceDigest, "actor_installation_digest": req.ActorInstallDigest, "actor_authority_digest": req.ActorAuthorityDigest, "actor_custody_digest": req.ActorCustodyDigest} {
		if value != "" {
			custody[key] = value
		}
	}
	return custody
}

func (m *memoryStore) writeMemoryGraphRepairCheckpoint(checkpoint memoryGraphRepairCheckpoint) error {
	checkpoint.SchemaID, checkpoint.Version = memoryGraphRepairCheckpointID, 1
	checkpoint.UpdatedAt = nowUTCISO()
	if checkpoint.EdgeDigestAlgorithm == "" {
		checkpoint.EdgeDigestAlgorithm = memoryGraphRepairDigestAlgorithm
	}
	if checkpoint.ActionRows != nil {
		checkpoint.ActionRowsDigest = memoryGraphRepairActionRowsDigest(checkpoint.ActionRows)
	}
	checkpoint.CheckpointDigest = memoryGraphRepairCheckpointDigest(checkpoint)
	checkpoint.Custody = memoryGraphRepairCustody(m)
	if checkpoint.ActorPrincipalDigest != "" {
		checkpoint.Custody["actor_principal_digest"] = checkpoint.ActorPrincipalDigest
	}
	if checkpoint.ActorScopeDigest != "" {
		checkpoint.Custody["actor_scope_digest"] = checkpoint.ActorScopeDigest
	}
	if checkpoint.ActorCustodyDigest != "" {
		checkpoint.Custody["actor_custody_digest"] = checkpoint.ActorCustodyDigest
	}
	raw, err := json.MarshalIndent(checkpoint, "", "  ")
	if err != nil {
		return err
	}
	if err := validateMemoryGraphRepairArtifactBytes(raw); err != nil {
		return err
	}
	if err := writeOwnerOnlyDurableAtomicFile(memoryGraphRepairPlanCheckpointPath(m, checkpoint.PlanReceiptRef), append(raw, '\n'), true); err != nil {
		return err
	}
	pointer := memoryGraphRepairActivePointer{SchemaID: "contextlattice_memory_graph_repair_active.v1", Version: 1, Project: checkpoint.Project, PlanRef: checkpoint.PlanReceiptRef, CheckpointID: checkpoint.CheckpointID, Status: checkpoint.Status, UpdatedAt: checkpoint.UpdatedAt}
	pointerRaw, err := json.MarshalIndent(pointer, "", "  ")
	if err != nil {
		return err
	}
	if err := validateMemoryGraphRepairArtifactBytes(pointerRaw); err != nil {
		return err
	}
	return writeOwnerOnlyDurableAtomicFile(memoryGraphRepairCheckpointPath(m, checkpoint.Project), append(pointerRaw, '\n'), true)
}

func memoryGraphRepairReceiptPath(m *memoryStore, runID string, cursor int, digest string) string {
	seed := strings.ToLower(strings.TrimSpace(runID)) + ":" + fmt.Sprintf("%d", cursor) + ":" + digest
	return filepath.Join(m.policy.rootPath, "_contextlattice", "memory_graph_repair_receipt_"+sha256Hex(seed)[:24]+".json")
}

func (m *memoryStore) appendMemoryGraphRepairEdge(edge memoryEdgeEntry) error {
	fence, err := m.acquireMemoryEdgeLogFence()
	if err != nil {
		return err
	}
	defer fence.release()
	_, err = m.appendMemoryGraphRepairEdgeWithFenceLocked(edge, fence)
	return err
}

func (m *memoryStore) appendMemoryGraphRepairEdgeLocked(edge memoryEdgeEntry) (memoryEdgeEntry, error) {
	return memoryEdgeEntry{}, errMemoryEdgeLogWriterFenceRequired
}

func (m *memoryStore) appendMemoryGraphRepairEdgeWithFenceLocked(edge memoryEdgeEntry, fence *memoryEdgeLogFenceToken) (memoryEdgeEntry, error) {
	if err := requireMemoryEdgeLogFence(m, fence); err != nil {
		return memoryEdgeEntry{}, err
	}
	appender, err := m.newMemoryEdgeLogAppenderFastWithFenceLocked(true, fence)
	if err != nil {
		return memoryEdgeEntry{}, err
	}
	return m.appendMemoryGraphRepairEdgeWithAppenderFenceLocked(edge, appender, fence)
}

func (m *memoryStore) appendMemoryGraphRepairEdgeWithAppenderLocked(edge memoryEdgeEntry, appender *memoryEdgeLogAppender) (memoryEdgeEntry, error) {
	return memoryEdgeEntry{}, errMemoryEdgeLogWriterFenceRequired
}

func (m *memoryStore) appendMemoryGraphRepairEdgeWithAppenderFenceLocked(edge memoryEdgeEntry, appender *memoryEdgeLogAppender, fence *memoryEdgeLogFenceToken) (memoryEdgeEntry, error) {
	if err := requireMemoryEdgeLogFence(m, fence); err != nil {
		return memoryEdgeEntry{}, err
	}
	if m == nil || !m.isEnabled() {
		return memoryEdgeEntry{}, errors.New("go memory store is disabled")
	}
	normalized, err := edge.normalized()
	if err != nil {
		return memoryEdgeEntry{}, err
	}
	if excluded, reason := m.memoryGraphEdgeExcluded(normalized); excluded {
		return memoryEdgeEntry{}, fmt.Errorf("memory edge rejected by graph artifact policy: %s", reason)
	}
	exactStatePaths, err := m.exactStatePathsSnapshotWithFenceChecked(fence)
	if err != nil {
		return memoryEdgeEntry{}, err
	}
	if edgeReferencesExactStatePaths(exactStatePaths, normalized) {
		return memoryEdgeEntry{}, errors.New("memory edge rejected because exact state is not graph-addressable")
	}
	if memoryGraphEdgeRequiresBinding(normalized) && (!memoryReferenceBindingValid(normalized.Binding) || !m.referenceEdgeCurrentWithFence(normalized, fence)) {
		return memoryEdgeEntry{}, errors.New("memory graph repair edge binding is not current")
	}
	if appender == nil || appender.store != m || appender.fence != fence {
		return memoryEdgeEntry{}, errors.New("memory graph repair edge appender is unavailable")
	}
	appended, _, err := appender.append(normalized)
	if err != nil {
		return memoryEdgeEntry{}, err
	}
	m.mu.Lock()
	m.recordEdgeLocked(appended)
	m.mu.Unlock()
	return appended, nil
}

type memoryGraphRepairReceipt struct {
	SchemaID                  string           `json:"schema_id"`
	Version                   int              `json:"version"`
	ReceiptID                 string           `json:"receipt_id"`
	RunID                     string           `json:"run_id"`
	CheckpointID              string           `json:"checkpoint_id"`
	Project                   string           `json:"project"`
	PlanReceiptRef            string           `json:"plan_receipt_ref"`
	PlanReceiptDigest         string           `json:"plan_receipt_digest"`
	ActionDigest              string           `json:"action_digest"`
	SnapshotDigest            string           `json:"snapshot_digest"`
	EdgeDigestBefore          string           `json:"edge_digest_before"`
	EdgeDigestAfter           string           `json:"edge_digest_after"`
	EdgeDigestAlgorithm       string           `json:"edge_digest_algorithm,omitempty"`
	EdgeLogGenerationBefore   uint64           `json:"edge_log_generation_before"`
	EdgeLogGenerationAfter    uint64           `json:"edge_log_generation_after"`
	EdgeLogDigestBefore       string           `json:"edge_log_digest_before"`
	EdgeLogDigestAfter        string           `json:"edge_log_digest_after"`
	EdgeLogContentDigestAfter string           `json:"edge_log_content_digest_after"`
	CursorBefore              int              `json:"cursor_before"`
	CursorAfter               int              `json:"cursor_after"`
	Applied                   int              `json:"applied"`
	Skipped                   int              `json:"skipped"`
	Retired                   int              `json:"retired"`
	Written                   int              `json:"written"`
	BindingBefore             map[string]any   `json:"binding_before"`
	BindingAfter              map[string]any   `json:"binding_after"`
	ResolutionProof           map[string]any   `json:"resolution_proof"`
	Actions                   []map[string]any `json:"actions"`
	RollbackReceipts          []map[string]any `json:"rollback_receipts"`
	ActorPrincipalDigest      string           `json:"actor_principal_digest,omitempty"`
	ActorScopeDigest          string           `json:"actor_scope_digest,omitempty"`
	ActorCustodyDigest        string           `json:"actor_custody_digest,omitempty"`
	Custody                   map[string]any   `json:"custody"`
	CreatedAt                 string           `json:"created_at"`
}

func memoryGraphRepairReceiptSemanticDigest(receipt memoryGraphRepairReceipt) string {
	receipt.ReceiptID = ""
	receipt.Custody = nil
	receipt.CreatedAt = ""
	raw := mustJSON(receipt)
	// Receipt actions are maps after a restart but may contain typed structs
	// before the first write. Round-trip through any so both representations
	// receive the same deterministic, map-key-sorted encoding.
	var canonical any
	if err := json.Unmarshal(raw, &canonical); err == nil {
		raw = mustJSON(canonical)
	}
	return "sha256:" + sha256Hex(string(raw))
}

func (m *memoryStore) writeMemoryGraphRepairReceipt(receipt memoryGraphRepairReceipt) (string, string, error) {
	receipt.CreatedAt = nowUTCISO()
	receipt.Custody = memoryGraphRepairCustody(m)
	if receipt.ActorPrincipalDigest != "" {
		receipt.Custody["actor_principal_digest"] = receipt.ActorPrincipalDigest
	}
	if receipt.ActorScopeDigest != "" {
		receipt.Custody["actor_scope_digest"] = receipt.ActorScopeDigest
	}
	receipt.ReceiptID = "receipt_" + sha256Hex(receipt.RunID + ":" + fmt.Sprintf("%d", receipt.CursorAfter) + ":" + receipt.EdgeDigestAfter)[:24]
	raw, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return "", "", err
	}
	if err := validateMemoryGraphRepairArtifactBytes(raw); err != nil {
		return "", "", err
	}
	path := memoryGraphRepairReceiptPath(m, receipt.RunID, receipt.CursorAfter, receipt.EdgeDigestAfter)
	if existingRaw, readErr := readMemoryGraphRepairArtifact(path); readErr == nil {
		var existing memoryGraphRepairReceipt
		if json.Unmarshal(existingRaw, &existing) != nil || memoryGraphRepairReceiptSemanticDigest(existing) != memoryGraphRepairReceiptSemanticDigest(receipt) {
			return "", "", memoryGraphRepairErr("receipt_conflict", errors.New("immutable repair receipt already exists with different content"))
		}
		return existing.ReceiptID, "sha256:" + sha256Hex(string(existingRaw)), nil
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return "", "", readErr
	}
	persisted := append(raw, '\n')
	if err := writeOwnerOnlyDurableAtomicFile(path, persisted, true); err != nil {
		return "", "", err
	}
	return receipt.ReceiptID, "sha256:" + sha256Hex(string(persisted)), nil
}

func memoryGraphRepairRollbackReceipt(action memoryGraphRepairAction, runID string) map[string]any {
	restore := "append_explicit_retirement_for_repair_created_edge"
	if action.Previous.EdgeID != "" {
		restore = "append_valid_prior_edge_if_repair_row_is_still_latest"
	}
	return map[string]any{
		"action_id": action.ActionID, "edge_id": action.Edge.EdgeID,
		"rollback": restore, "run_id": runID,
		"prior_edge_digest":  memoryGraphRepairOptionalEdgeDigest(action.Previous),
		"evidence_preserved": true, "deletion_performed": false,
	}
}

func memoryGraphRepairReceiptAction(action memoryGraphRepairAction, edge memoryEdgeEntry) map[string]any {
	return map[string]any{"action_id": action.ActionID, "kind": action.Kind, "edge_id": edge.EdgeID, "reason": action.Reason, "appended_row_digest": memoryGraphRepairOptionalEdgeDigest(edge), "provenance_preserved": true}
}

func memoryGraphRepairOptionalEdgeDigest(edge memoryEdgeEntry) string {
	if strings.TrimSpace(edge.EdgeID) == "" {
		return ""
	}
	return "sha256:" + sha256Hex(string(mustJSON(edge)))
}

func (m *memoryStore) loadMemoryGraphRepairActivePointer(project string) (memoryGraphRepairActivePointer, bool, error) {
	raw, err := readMemoryGraphRepairArtifact(memoryGraphRepairCheckpointPath(m, project))
	if errors.Is(err, os.ErrNotExist) {
		return memoryGraphRepairActivePointer{}, false, nil
	}
	if err != nil {
		return memoryGraphRepairActivePointer{}, false, memoryGraphRepairErr("checkpoint_io", err)
	}
	var pointer memoryGraphRepairActivePointer
	if json.Unmarshal(raw, &pointer) != nil || pointer.SchemaID != "contextlattice_memory_graph_repair_active.v1" || pointer.Version != 1 || pointer.Project != project || pointer.PlanRef == "" {
		return pointer, false, errors.New("memory graph repair active pointer is invalid")
	}
	return pointer, true, nil
}

func (m *memoryStore) executeMemoryGraphRepair(ctx context.Context, snapshot memoryGraphRepairSnapshot, evidence memoryGraphRepairEdgeEvidence, plan memoryGraphRepairPlanReceipt, planDigest string, req memoryGraphRepairRequest, validateRepairLock func() error) (map[string]any, error) {
	if req.DryRun || !req.Apply {
		return nil, errors.New("repair apply is not enabled")
	}
	if validateRepairLock != nil {
		if err := validateRepairLock(); err != nil {
			return nil, memoryGraphRepairErr("repair_lock_io", err)
		}
	}
	actions := plan.Actions
	keyGeneration, topicGeneration, err := m.currentMemoryGraphRepairGeneration(req.Project)
	if err != nil {
		return nil, memoryGraphRepairErr("snapshot_drift", err)
	}
	checkpoint, exists, err := m.loadMemoryGraphRepairCheckpoint(req.Project, plan.ReceiptRef)
	if err != nil {
		return nil, memoryGraphRepairIOErr("checkpoint_invalid", err)
	}
	if exists {
		if req.CheckpointID == "" && checkpoint.Status != "complete" {
			return nil, memoryGraphRepairErr("checkpoint_resume_required", errors.New("incomplete checkpoint requires checkpoint_id"))
		}
		if req.CheckpointID != "" && req.CheckpointID != checkpoint.CheckpointID {
			return nil, memoryGraphRepairErr("checkpoint_mismatch", errors.New("checkpoint_id does not match project checkpoint"))
		}
		if checkpoint.SnapshotDigest != snapshot.SnapshotDigest || checkpoint.PolicyDigest != plan.PolicyDigest || checkpoint.ActionDigest != plan.ActionDigest || checkpoint.PlanReceiptRef != plan.ReceiptRef || checkpoint.PlanReceiptDigest != planDigest || checkpoint.KeyGeneration != keyGeneration || checkpoint.TopicGeneration != topicGeneration || checkpoint.ActorCustodyDigest != req.ActorCustodyDigest || checkpoint.ObservedAt != plan.ObservedAt {
			return nil, memoryGraphRepairErr("snapshot_drift", errors.New("current-state snapshot changed since checkpoint"))
		}
		if checkpoint.Cursor < 0 || checkpoint.Cursor > len(actions) || checkpoint.TotalActions != len(actions) || (checkpoint.Status != "running" && checkpoint.Status != "complete") || (checkpoint.Status == "complete" && checkpoint.Cursor != len(actions)) {
			return nil, memoryGraphRepairErr("checkpoint_invalid", errors.New("repair checkpoint cursor or action count is invalid"))
		}
		if checkpoint.Status == "complete" {
			return map[string]any{"ok": true, "dry_run": false, "apply": true, "idempotent": true, "status": "complete", "project": req.Project, "checkpoint_id": checkpoint.CheckpointID, "cursor": checkpoint.Cursor, "total_actions": checkpoint.TotalActions, "counts": checkpoint.Counts, "snapshot_digest": snapshot.SnapshotDigest, "edge_digest": evidence.Digest, "custody": memoryGraphRepairCustodyForRequest(m, req), "connectedness_claim": "bound_current_state_intersection_only"}, nil
		}
		if evidence.Digest != checkpoint.EdgeDigestAfter {
			// A full historical artifact has no reversible authenticated digest
			// midstate.  Compact continuation evidence starts at the checkpoint
			// chain digest and is refreshed from the bounded suffix below; any
			// legacy mismatch fails closed instead of scanning Rows.
			if !evidence.Continuation {
				return nil, memoryGraphRepairErr("snapshot_drift", errors.New("edge evidence changed since checkpoint"))
			}
		}
	} else {
		if req.CheckpointID != "" {
			return nil, memoryGraphRepairErr("checkpoint_mismatch", errors.New("checkpoint_id does not identify an existing project checkpoint"))
		}
		if active, activeExists, activeErr := m.loadMemoryGraphRepairActivePointer(req.Project); activeErr != nil {
			return nil, memoryGraphRepairIOErr("checkpoint_invalid", activeErr)
		} else if activeExists && active.Status == "running" && active.PlanRef != plan.ReceiptRef {
			return nil, memoryGraphRepairErr("checkpoint_resume_required", errors.New("another immutable repair plan is active for this project"))
		}
		seed := strings.ToLower(req.Project) + ":" + plan.ReceiptRef + ":" + planDigest
		checkpoint = memoryGraphRepairCheckpoint{SchemaID: memoryGraphRepairCheckpointID, Version: 1, CheckpointID: "repair_" + sha256Hex(seed)[:24], RunID: "run_" + sha256Hex(seed)[:32], Project: req.Project, PlanReceiptRef: plan.ReceiptRef, PlanReceiptDigest: planDigest, SnapshotDigest: snapshot.SnapshotDigest, PolicyDigest: plan.PolicyDigest, ActionDigest: plan.ActionDigest, StaleAfter: plan.StaleAfter, ObservedAt: plan.ObservedAt, EdgeDigestBefore: evidence.Digest, EdgeDigestAfter: evidence.Digest, EdgeDigestAlgorithm: memoryGraphRepairDigestAlgorithm, KeyGeneration: keyGeneration, TopicGeneration: topicGeneration, EdgeLogGeneration: evidence.LogGeneration, EdgeLogDigest: evidence.LogDigest, EdgeLogContentDigest: evidence.LogContentDigest, EdgeLogContentHashState: evidence.LogContentHashState, EdgeLogContentHashedBytes: evidence.LogContentHashedBytes, EdgeLogFileSize: evidence.LogFileSize, EdgeLogFileIdentity: evidence.LogFileIdentity, ActionRows: map[string]memoryEdgeEntry{}, BindingState: cloneJSONMap(plan.BindingBefore), ResolutionState: cloneJSONMap(plan.ResolutionProof), Cursor: 0, TotalActions: len(actions), Counts: map[string]int{"plan_actions": len(actions), "plan_retire": 0, "plan_write": 0, "applied_total": 0, "skipped_total": 0, "retired_total": 0, "written_total": 0, "chunks": 0}, Status: "running", ActorPrincipalDigest: req.ActorPrincipalDigest, ActorScopeDigest: req.ActorScopeDigest, ActorCustodyDigest: req.ActorCustodyDigest}
		for _, action := range actions {
			if action.Kind == "retire" {
				checkpoint.Counts["plan_retire"]++
			} else if action.Kind == "write" {
				checkpoint.Counts["plan_write"]++
			}
		}
	}
	if checkpoint.Cursor < 0 || checkpoint.Cursor > len(actions) || checkpoint.TotalActions != len(actions) || (checkpoint.Status != "running" && checkpoint.Status != "complete") || (checkpoint.Status == "complete" && checkpoint.Cursor != len(actions)) {
		return nil, memoryGraphRepairErr("checkpoint_invalid", errors.New("repair checkpoint cursor or action count is invalid"))
	}
	start := checkpoint.Cursor
	chunkSize := clampInt(req.ChunkSize, 1, memoryGraphRepairMaxLockActions)
	end := start + chunkSize
	if end > len(actions) {
		end = len(actions)
	}
	currentEvidence := evidence
	pendingActionIDs := map[string]struct{}{}
	for index := start; index < end; index++ {
		pendingActionIDs[actions[index].ActionID] = struct{}{}
	}
	if repairRows := currentEvidence.RepairActionRows[checkpoint.RunID]; repairRows != nil {
		for actionID := range pendingActionIDs {
			if edge, exists := repairRows[actionID]; exists {
				for _, action := range actions[start:end] {
					if action.ActionID == actionID && !memoryGraphRepairImmutableEdgeTupleMatches(action.Edge, edge) {
						return nil, memoryGraphRepairErr("snapshot_drift", fmt.Errorf("repair action %s carries an edge tuple different from the immutable plan", actionID))
					}
				}
			}
		}
	}
	bindingBefore := map[string]any{}
	if currentEvidence.Continuation {
		if checkpoint.BindingState != nil {
			bindingBefore = cloneJSONMap(checkpoint.BindingState)
		} else {
			bindingBefore = cloneJSONMap(plan.BindingBefore)
		}
	} else {
		preChunkEvidence := memoryGraphRepairEvidenceWithoutPendingRun(currentEvidence, req.Project, checkpoint.RunID, pendingActionIDs)
		bindingBefore = memoryGraphRepairBindingSummary(snapshot, preChunkEvidence, req.Project)
	}
	applied, skipped, retired, written := 0, 0, 0, 0
	receiptActions := []map[string]any{}
	rollback := []map[string]any{}
	appliedActions := []memoryGraphRepairAction{}
	fence, err := m.acquireMemoryEdgeLogFenceContext(ctx)
	if err != nil {
		return nil, memoryGraphRepairErr("edge_log_io", err)
	}
	defer fence.release()
	pathKeys := make([]string, 0, 1+(end-start)*2)
	pathKeys = append(pathKeys, "memory-graph-repair:"+strings.ToLower(strings.TrimSpace(req.Project)))
	for index := start; index < end; index++ {
		for _, memoryID := range []string{actions[index].Edge.SourceID, actions[index].Edge.TargetID} {
			if _, _, _, key, keyErr := canonicalMemoryID(memoryID); keyErr == nil {
				pathKeys = append(pathKeys, key)
			}
		}
	}
	pathUnlock, pathErr := m.lockMemoryPathsContext(ctx, pathKeys...)
	if pathErr != nil {
		return nil, memoryGraphRepairErr("repair_lock_io", pathErr)
	}
	defer pathUnlock()
	if validateRepairLock != nil {
		if err := validateRepairLock(); err != nil {
			return nil, memoryGraphRepairErr("repair_lock_io", err)
		}
	}
	if !exists {
		// Both initial callers may have observed an absent checkpoint before
		// contending on the in-process/cross-process fences. Re-read the durable
		// pointer after acquiring those fences so only one caller can create the
		// first checkpoint; the loser must resume explicitly.
		_, freshExists, freshErr := m.loadMemoryGraphRepairCheckpoint(req.Project, plan.ReceiptRef)
		if freshErr != nil {
			return nil, memoryGraphRepairIOErr("checkpoint_invalid", freshErr)
		}
		if freshExists {
			return nil, memoryGraphRepairErr("checkpoint_resume_required", errors.New("repair checkpoint was created by a concurrent initial apply"))
		}
	}
	appender, err := m.newMemoryEdgeLogAppenderFastWithFenceContextLocked(ctx, true, fence)
	if err != nil {
		return nil, memoryGraphRepairErr("edge_log_io", err)
	}
	if currentEvidence.Continuation {
		if err := m.refreshMemoryGraphRepairContinuationEvidence(ctx, &currentEvidence, plan, checkpoint, nil, appender.state, pendingActionIDs); err != nil {
			return nil, memoryGraphRepairErr("snapshot_drift", err)
		}
	}
	if appender.state.Generation != currentEvidence.LogGeneration || appender.state.Digest != currentEvidence.LogDigest || appender.state.ContentDigest != currentEvidence.LogContentDigest {
		return nil, memoryGraphRepairErr("snapshot_drift", errors.New("edge log generation or digest changed before repair commit"))
	}
	if !exists {
		if !currentEvidence.Continuation && (currentEvidence.Digest != plan.EdgeDigest || currentEvidence.LogGeneration != plan.EdgeLogGeneration || currentEvidence.LogDigest != plan.EdgeLogDigest || currentEvidence.LogContentDigest != plan.EdgeLogContentDigest) {
			return nil, memoryGraphRepairErr("plan_receipt_drift", errors.New("edge log no longer matches the immutable plan"))
		}
		if err := m.writeMemoryGraphRepairCheckpoint(checkpoint); err != nil {
			return nil, memoryGraphRepairIOErr("checkpoint_io", err)
		}
	}
	receiptLogGenerationBefore := appender.state.Generation
	receiptLogDigestBefore := appender.state.Digest
	receiptLogBeforeBound := false
	for index := start; index < end; index++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		action := actions[index]
		if recoveredEdge, recovered := memoryGraphRepairActionRecovered(action, currentEvidence, checkpoint.RunID, plan.ReceiptRef, planDigest, plan.ActionDigest, req.ActorCustodyDigest); recovered {
			if checkpoint.ActionRows == nil {
				checkpoint.ActionRows = map[string]memoryEdgeEntry{}
			}
			checkpoint.ActionRows[action.ActionID] = recoveredEdge
			if !receiptLogBeforeBound {
				generation, digest, _, valid := memoryGraphRepairAppendBefore(recoveredEdge)
				if !valid {
					return nil, memoryGraphRepairErr("provenance_invalid", errors.New("recovered repair row is missing exact pre-append log state"))
				}
				receiptLogGenerationBefore, receiptLogDigestBefore, receiptLogBeforeBound = generation, digest, true
			}
			// The append was durable before the process crashed. Reconstruct the
			// receipt as an applied action so cumulative counts and rollback
			// coverage remain exact rather than misclassifying recovery as a skip.
			appliedActions = append(appliedActions, action)
			applied++
			if action.Kind == "retire" {
				retired++
			} else {
				written++
			}
			receiptActions = append(receiptActions, memoryGraphRepairReceiptAction(action, recoveredEdge))
			rollback = append(rollback, memoryGraphRepairRollbackReceipt(action, checkpoint.RunID))
			continue
		}
		latest, latestExists := currentEvidence.Latest[action.Edge.EdgeID]
		if action.PreviousActionID != "" {
			priorRow, priorExists := currentEvidence.RepairActionRows[checkpoint.RunID][action.PreviousActionID]
			if !priorExists || !latestExists || memoryGraphRepairOptionalEdgeDigest(latest) != memoryGraphRepairOptionalEdgeDigest(priorRow) ||
				memoryGraphRepairEdgeMetadataRepair(priorRow, "repair_run_id") != checkpoint.RunID || memoryGraphRepairEdgeMetadataRepair(priorRow, "repair_action_id") != action.PreviousActionID {
				return nil, memoryGraphRepairErr("snapshot_drift", fmt.Errorf("repair action %s no longer follows its exact durable predecessor", action.ActionID))
			}
		} else if action.Previous.EdgeID == "" {
			if latestExists {
				return nil, memoryGraphRepairErr("snapshot_drift", fmt.Errorf("repair action %s expected no prior durable edge", action.ActionID))
			}
		} else if !latestExists || memoryGraphRepairOptionalEdgeDigest(latest) != memoryGraphRepairOptionalEdgeDigest(action.Previous) {
			return nil, memoryGraphRepairErr("snapshot_drift", fmt.Errorf("repair action %s prior durable edge changed", action.ActionID))
		}
		appliedActions = append(appliedActions, action)
		edge := action.Edge
		edge.Metadata = cloneJSONMap(edge.Metadata)
		if edge.Metadata == nil {
			edge.Metadata = map[string]any{}
		}
		edge.Provenance = cloneJSONMap(edge.Provenance)
		if edge.Provenance == nil {
			edge.Provenance = map[string]any{}
		}
		edge.Metadata["repair_run_id"] = checkpoint.RunID
		edge.Metadata["repair_action_id"] = action.ActionID
		edge.Metadata["repair_cursor"] = index
		edge.Metadata["repair_snapshot_digest"] = snapshot.SnapshotDigest
		edge.Metadata["repair_plan_ref"] = plan.ReceiptRef
		edge.Metadata["repair_plan_digest"] = planDigest
		edge.Metadata["repair_action_digest"] = plan.ActionDigest
		edge.Metadata["repair_custody_digest"] = req.ActorCustodyDigest
		edge.Metadata["repair_prior_row_digest"] = memoryGraphRepairOptionalEdgeDigest(latest)
		edge.Metadata["repair_edge_log_generation_before"] = appender.state.Generation
		edge.Metadata["repair_edge_log_digest_before"] = appender.state.Digest
		edge.Metadata["repair_edge_log_content_digest_before"] = appender.state.ContentDigest
		if !receiptLogBeforeBound {
			receiptLogGenerationBefore, receiptLogDigestBefore, receiptLogBeforeBound = appender.state.Generation, appender.state.Digest, true
		}
		origin := memoryGraphRepairOriginCurrentState
		if action.Kind == "retire" {
			origin = memoryGraphRepairOriginHistorical
		}
		for key, value := range memoryGraphRepairClosedProvenance(snapshot, origin, action.Previous) {
			edge.Provenance[key] = value
		}
		edge.Metadata["repair_server_append_marker"] = memoryGraphRepairServerAppendMarker("repair", edge)
		if action.Kind == "retire" {
			edge.Metadata["rollback_contract"] = "authenticated_cas_append"
			// rollback_contract is part of the exact server-owned marker.
			edge.Metadata["repair_server_append_marker"] = memoryGraphRepairServerAppendMarker("repair", edge)
			if edge, err = m.appendMemoryGraphRepairEdgeWithAppenderFenceLocked(edge, appender, fence); err != nil {
				return nil, memoryGraphRepairIOErr("edge_log_io", err)
			}
			retired++
		} else {
			if edge, err = m.appendMemoryGraphRepairEdgeWithAppenderFenceLocked(edge, appender, fence); err != nil {
				return nil, memoryGraphRepairIOErr("edge_log_io", err)
			}
			written++
		}
		applied++
		receiptActions = append(receiptActions, memoryGraphRepairReceiptAction(action, edge))
		rollback = append(rollback, memoryGraphRepairRollbackReceipt(action, checkpoint.RunID))
		row, rowErr := memoryGraphRepairCanonicalizeEdge(edge, snapshot)
		if rowErr != nil {
			return nil, rowErr
		}
		row.Bound = action.BindingReason == "bound_current_state"
		row.FromMemory = true
		row.RawDigest = "sha256:" + sha256Hex(string(mustJSON(edge)))
		currentEvidence.record(row)
		currentEvidence.ScannedLines++
		if encoded, encodeErr := json.Marshal(edge); encodeErr == nil {
			currentEvidence.ScannedBytes += int64(len(encoded) + 1)
		}
		if checkpoint.ActionRows == nil {
			checkpoint.ActionRows = map[string]memoryEdgeEntry{}
		}
		checkpoint.ActionRows[action.ActionID] = edge
	}
	currentEvidence.Digest = currentEvidence.projectDigest()
	committedLog := memoryEdgeLogSnapshot{Generation: appender.state.Generation, Digest: appender.state.Digest, ContentDigest: appender.state.ContentDigest, FileSize: appender.state.FileSize, FileStamp: memoryEdgeLogFileStamp{Exists: true, Size: appender.state.FileSize, Identity: appender.state.FileIdentity, ModTimeNanos: appender.state.FileModTimeNanos, ChangeToken: appender.state.FileChangeToken}}
	currentEvidence.LogGeneration = committedLog.Generation
	currentEvidence.LogDigest = committedLog.Digest
	currentEvidence.LogContentDigest = committedLog.ContentDigest
	currentEvidence.LogContentHashState = appender.state.ContentHashState
	currentEvidence.LogContentHashedBytes = appender.state.ContentHashedBytes
	currentEvidence.LogFileSize = committedLog.FileSize
	if !currentEvidence.Continuation {
		if err := m.persistMemoryGraphRepairEvidenceArtifact(snapshot, currentEvidence); err != nil {
			return nil, memoryGraphRepairIOErr("edge_log_io", err)
		}
		m.cacheMemoryGraphRepairEvidence(snapshot.SnapshotDigest, snapshot.Project, currentEvidence)
	}
	// The checkpoint's previous edge digest is the authoritative digest before
	// this chunk.  Keep it before advancing the in-memory checkpoint so a
	// receipt remains accurate and a replay after a receipt-only crash uses the
	// same before-digest.
	receiptEdgeDigestBefore := checkpoint.EdgeDigestAfter
	if receiptEdgeDigestBefore == "" {
		receiptEdgeDigestBefore = evidence.Digest
	}
	bindingAfter := memoryGraphRepairBindingSummary(snapshot, currentEvidence, req.Project)
	if currentEvidence.Continuation {
		bindingAfter = memoryGraphRepairContinuationBindingAfter(bindingBefore, actions[start:end])
	}
	resolutionProof := memoryGraphRepairActionResolution(snapshot, appliedActions, &currentEvidence, checkpoint.RunID)
	if !anyToBool(resolutionProof["all_repair_provenance_closed"]) {
		return nil, memoryGraphRepairErr("provenance_invalid", errors.New("durable repair evidence is missing closed snapshot provenance"))
	}
	if end >= len(actions) {
		checkpoint.Status = "complete"
	}
	checkpoint.Cursor = end
	checkpoint.EdgeDigestAfter = currentEvidence.Digest
	checkpoint.EdgeDigestAlgorithm = memoryGraphRepairDigestAlgorithm
	checkpoint.EdgeLogGeneration = committedLog.Generation
	checkpoint.EdgeLogDigest = committedLog.Digest
	checkpoint.EdgeLogContentDigest = committedLog.ContentDigest
	checkpoint.EdgeLogContentHashState = appender.state.ContentHashState
	checkpoint.EdgeLogContentHashedBytes = appender.state.ContentHashedBytes
	checkpoint.EdgeLogFileSize = committedLog.FileSize
	checkpoint.EdgeLogFileIdentity = committedLog.FileStamp.Identity
	checkpoint.BindingState = cloneJSONMap(bindingAfter)
	checkpoint.ResolutionState = cloneJSONMap(resolutionProof)
	checkpoint.TotalActions = len(actions)
	if checkpoint.Counts == nil {
		checkpoint.Counts = map[string]int{}
	}
	checkpoint.Counts["plan_actions"] = len(actions)
	checkpoint.Counts["applied_total"] += applied
	checkpoint.Counts["skipped_total"] += skipped
	checkpoint.Counts["retired_total"] += retired
	checkpoint.Counts["written_total"] += written
	checkpoint.Counts["chunks"]++
	receipt := memoryGraphRepairReceipt{SchemaID: memoryGraphRepairReceiptID, Version: 1, RunID: checkpoint.RunID, CheckpointID: checkpoint.CheckpointID, Project: req.Project, PlanReceiptRef: plan.ReceiptRef, PlanReceiptDigest: planDigest, ActionDigest: plan.ActionDigest, SnapshotDigest: snapshot.SnapshotDigest, EdgeDigestBefore: receiptEdgeDigestBefore, EdgeDigestAfter: currentEvidence.Digest, EdgeDigestAlgorithm: memoryGraphRepairDigestAlgorithm, EdgeLogGenerationBefore: receiptLogGenerationBefore, EdgeLogGenerationAfter: committedLog.Generation, EdgeLogDigestBefore: receiptLogDigestBefore, EdgeLogDigestAfter: committedLog.Digest, EdgeLogContentDigestAfter: committedLog.ContentDigest, CursorBefore: start, CursorAfter: end, Applied: applied, Skipped: skipped, Retired: retired, Written: written, BindingBefore: bindingBefore, BindingAfter: bindingAfter, ResolutionProof: resolutionProof, Actions: receiptActions, RollbackReceipts: rollback, ActorPrincipalDigest: req.ActorPrincipalDigest, ActorScopeDigest: req.ActorScopeDigest, ActorCustodyDigest: req.ActorCustodyDigest}
	receiptRef, receiptDigest, err := m.writeMemoryGraphRepairReceipt(receipt)
	if err != nil {
		return nil, memoryGraphRepairIOErr("receipt_io", err)
	}
	checkpoint.LastReceiptDigest = receiptDigest
	if m.memoryGraphRepairBeforeCheckpoint != nil {
		if err := m.memoryGraphRepairBeforeCheckpoint(); err != nil {
			return nil, err
		}
	}
	if err := m.writeMemoryGraphRepairCheckpoint(checkpoint); err != nil {
		return nil, memoryGraphRepairIOErr("checkpoint_io", err)
	}
	return map[string]any{"ok": true, "dry_run": false, "apply": true, "status": checkpoint.Status, "project": req.Project, "checkpoint_id": checkpoint.CheckpointID, "run_id": checkpoint.RunID, "cursor": checkpoint.Cursor, "next_cursor": checkpoint.Cursor, "remaining": maxInt(0, len(actions)-checkpoint.Cursor), "total_actions": len(actions), "counts": checkpoint.Counts, "snapshot_digest": snapshot.SnapshotDigest, "edge_digest": currentEvidence.Digest, "edge_log_generation": committedLog.Generation, "edge_log_digest": committedLog.Digest, "edge_digest_before": receiptEdgeDigestBefore, "binding_before": bindingBefore, "binding_after": bindingAfter, "resolution_proof": resolutionProof, "receipt_ref": receiptRef, "receipt_digest": receiptDigest, "checkpoint_ref": checkpoint.CheckpointID, "custody": memoryGraphRepairCustodyForRequest(m, req), "raw_store_scanned": false, "connectedness_claim": "bound_current_state_intersection_only", "pending_actions": memoryGraphRepairPendingActions(plan, checkpoint.Cursor)}, nil
}

func memoryGraphRepairPendingActions(plan memoryGraphRepairPlanReceipt, cursor int) map[string]any {
	actions := plan.Actions
	if cursor < 0 {
		cursor = 0
	}
	if cursor > len(actions) {
		cursor = len(actions)
	}
	pageEnd := minInt(len(actions), cursor+64)
	refs := make([]string, 0, pageEnd-cursor)
	for _, action := range actions[cursor:pageEnd] {
		refs = append(refs, "action_"+sha256Hex(plan.ActionDigest + "\x00" + action.ActionID)[:20])
	}
	result := map[string]any{"plan_ref": plan.ReceiptRef, "action_digest": plan.ActionDigest, "cursor": cursor, "remaining": len(actions) - cursor, "page_size": len(refs), "action_refs": refs}
	if pageEnd < len(actions) {
		result["next_page_ref"] = "page_" + sha256Hex(plan.ReceiptRef + "\x00" + fmt.Sprintf("%d", pageEnd))[:24]
	}
	return result
}

const (
	memoryGraphRepairRollbackCheckpointID = "contextlattice_memory_graph_repair_rollback_checkpoint.v1"
	memoryGraphRepairRollbackReceiptID    = "contextlattice_memory_graph_repair_rollback_receipt.v1"
)

type memoryGraphRepairRollbackCheckpoint struct {
	SchemaID                  string                     `json:"schema_id"`
	Version                   int                        `json:"version"`
	CheckpointID              string                     `json:"checkpoint_id"`
	RunID                     string                     `json:"run_id"`
	Project                   string                     `json:"project"`
	PlanReceiptRef            string                     `json:"plan_receipt_ref"`
	PlanReceiptDigest         string                     `json:"plan_receipt_digest"`
	SnapshotDigest            string                     `json:"snapshot_digest"`
	PolicyDigest              string                     `json:"policy_digest"`
	ActionDigest              string                     `json:"action_digest"`
	ObservedAt                string                     `json:"observed_at"`
	ApplyCheckpointID         string                     `json:"apply_checkpoint_id"`
	ApplyRunID                string                     `json:"apply_run_id"`
	AppliedActionCount        int                        `json:"applied_action_count"`
	Cursor                    int                        `json:"cursor"`
	Status                    string                     `json:"status"`
	Counts                    map[string]int             `json:"counts"`
	EdgeDigestAfter           string                     `json:"edge_digest_after"`
	EdgeDigestAlgorithm       string                     `json:"edge_digest_algorithm,omitempty"`
	EdgeLogGeneration         uint64                     `json:"edge_log_generation"`
	EdgeLogDigest             string                     `json:"edge_log_digest"`
	EdgeLogContentDigest      string                     `json:"edge_log_content_digest"`
	EdgeLogContentHashState   string                     `json:"edge_log_content_hash_state,omitempty"`
	EdgeLogContentHashedBytes int64                      `json:"edge_log_content_hashed_bytes,omitempty"`
	EdgeLogFileSize           int64                      `json:"edge_log_file_size"`
	EdgeLogFileIdentity       string                     `json:"edge_log_file_identity,omitempty"`
	RollbackRows              map[string]memoryEdgeEntry `json:"rollback_rows,omitempty"`
	RollbackRowsDigest        string                     `json:"rollback_rows_digest,omitempty"`
	CheckpointDigest          string                     `json:"checkpoint_digest,omitempty"`
	ActorCustodyDigest        string                     `json:"actor_custody_digest"`
	LastReceiptDigest         string                     `json:"last_receipt_digest,omitempty"`
	UpdatedAt                 string                     `json:"updated_at"`
	Custody                   map[string]any             `json:"custody"`
}

type memoryGraphRepairRollbackChunkReceipt struct {
	SchemaID                  string           `json:"schema_id"`
	Version                   int              `json:"version"`
	ReceiptID                 string           `json:"receipt_id"`
	RunID                     string           `json:"run_id"`
	CheckpointID              string           `json:"checkpoint_id"`
	Project                   string           `json:"project"`
	PlanReceiptRef            string           `json:"plan_receipt_ref"`
	PlanReceiptDigest         string           `json:"plan_receipt_digest"`
	SnapshotDigest            string           `json:"snapshot_digest"`
	PolicyDigest              string           `json:"policy_digest"`
	ActionDigest              string           `json:"action_digest"`
	ObservedAt                string           `json:"observed_at"`
	CursorBefore              int              `json:"cursor_before"`
	CursorAfter               int              `json:"cursor_after"`
	EdgeDigestAfter           string           `json:"edge_digest_after"`
	EdgeDigestAlgorithm       string           `json:"edge_digest_algorithm,omitempty"`
	EdgeLogGeneration         uint64           `json:"edge_log_generation"`
	EdgeLogDigest             string           `json:"edge_log_digest"`
	EdgeLogContentDigest      string           `json:"edge_log_content_digest"`
	EdgeLogContentHashState   string           `json:"edge_log_content_hash_state,omitempty"`
	EdgeLogContentHashedBytes int64            `json:"edge_log_content_hashed_bytes,omitempty"`
	Actions                   []map[string]any `json:"actions"`
	ActorCustodyDigest        string           `json:"actor_custody_digest"`
	CreatedAt                 string           `json:"created_at"`
	Custody                   map[string]any   `json:"custody"`
}

func memoryGraphRepairRollbackCheckpointPath(m *memoryStore, planRef string) string {
	return filepath.Join(m.policy.rootPath, "_contextlattice", "memory_graph_repair_rollback_checkpoint_"+sha256Hex(planRef)[:24]+".json")
}

func (m *memoryStore) loadMemoryGraphRepairRollbackCheckpoint(planRef string) (memoryGraphRepairRollbackCheckpoint, bool, error) {
	raw, err := readMemoryGraphRepairArtifact(memoryGraphRepairRollbackCheckpointPath(m, planRef))
	if errors.Is(err, os.ErrNotExist) {
		return memoryGraphRepairRollbackCheckpoint{}, false, nil
	}
	if err != nil {
		return memoryGraphRepairRollbackCheckpoint{}, false, memoryGraphRepairErr("rollback_io", err)
	}
	var checkpoint memoryGraphRepairRollbackCheckpoint
	if json.Unmarshal(raw, &checkpoint) != nil || checkpoint.SchemaID != memoryGraphRepairRollbackCheckpointID || checkpoint.Version != 1 || checkpoint.PlanReceiptRef != planRef || checkpoint.CheckpointID == "" {
		return checkpoint, false, errors.New("memory graph repair rollback checkpoint is invalid")
	}
	if err := memoryGraphRepairCustodyMatches(m, checkpoint.Custody); err != nil {
		return checkpoint, false, memoryGraphRepairErr("custody_mismatch", err)
	}
	if !memoryGraphRepairValidDigest(checkpoint.CheckpointDigest) || checkpoint.CheckpointDigest != memoryGraphRepairRollbackCheckpointDigest(checkpoint) {
		return checkpoint, false, errors.New("memory graph repair rollback checkpoint digest mismatch")
	}
	if checkpoint.RollbackRowsDigest != "" && checkpoint.RollbackRowsDigest != memoryGraphRepairActionRowsDigest(checkpoint.RollbackRows) {
		return checkpoint, false, errors.New("memory graph repair rollback action index digest mismatch")
	}
	return checkpoint, true, nil
}

func (m *memoryStore) writeMemoryGraphRepairRollbackCheckpoint(checkpoint memoryGraphRepairRollbackCheckpoint) error {
	checkpoint.SchemaID, checkpoint.Version, checkpoint.UpdatedAt = memoryGraphRepairRollbackCheckpointID, 1, nowUTCISO()
	if checkpoint.EdgeDigestAlgorithm == "" {
		checkpoint.EdgeDigestAlgorithm = memoryGraphRepairDigestAlgorithm
	}
	if checkpoint.RollbackRows != nil {
		checkpoint.RollbackRowsDigest = memoryGraphRepairActionRowsDigest(checkpoint.RollbackRows)
	}
	checkpoint.CheckpointDigest = memoryGraphRepairRollbackCheckpointDigest(checkpoint)
	checkpoint.Custody = memoryGraphRepairCustody(m)
	checkpoint.Custody["actor_custody_digest"] = checkpoint.ActorCustodyDigest
	raw, err := json.MarshalIndent(checkpoint, "", "  ")
	if err != nil {
		return err
	}
	if err := validateMemoryGraphRepairArtifactBytes(raw); err != nil {
		return err
	}
	return writeOwnerOnlyDurableAtomicFile(memoryGraphRepairRollbackCheckpointPath(m, checkpoint.PlanReceiptRef), append(raw, '\n'), true)
}

func memoryGraphRepairRollbackReceiptPath(m *memoryStore, runID string, cursor int) string {
	return filepath.Join(m.policy.rootPath, "_contextlattice", "memory_graph_repair_rollback_receipt_"+sha256Hex(runID + "\x00" + fmt.Sprintf("%d", cursor))[:24]+".json")
}

func memoryGraphRepairRollbackReceiptDigest(receipt memoryGraphRepairRollbackChunkReceipt) string {
	receipt.ReceiptID, receipt.CreatedAt = "", ""
	receipt.Custody = nil
	return "sha256:" + sha256Hex(string(mustJSON(receipt)))
}

func (m *memoryStore) writeMemoryGraphRepairRollbackReceipt(receipt memoryGraphRepairRollbackChunkReceipt) (string, string, error) {
	receipt.SchemaID, receipt.Version, receipt.CreatedAt = memoryGraphRepairRollbackReceiptID, 1, nowUTCISO()
	receipt.ReceiptID = "rollback_receipt_" + sha256Hex(receipt.RunID + "\x00" + fmt.Sprintf("%d", receipt.CursorAfter) + "\x00" + receipt.EdgeLogDigest)[:24]
	receipt.Custody = memoryGraphRepairCustody(m)
	receipt.Custody["actor_custody_digest"] = receipt.ActorCustodyDigest
	raw, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return "", "", err
	}
	if err := validateMemoryGraphRepairArtifactBytes(raw); err != nil {
		return "", "", err
	}
	path := memoryGraphRepairRollbackReceiptPath(m, receipt.RunID, receipt.CursorAfter)
	if existingRaw, readErr := readMemoryGraphRepairArtifact(path); readErr == nil {
		var existing memoryGraphRepairRollbackChunkReceipt
		if json.Unmarshal(existingRaw, &existing) != nil || memoryGraphRepairRollbackReceiptDigest(existing) != memoryGraphRepairRollbackReceiptDigest(receipt) {
			return "", "", memoryGraphRepairErr("receipt_conflict", errors.New("immutable rollback receipt already exists with different content"))
		}
		return existing.ReceiptID, "sha256:" + sha256Hex(string(existingRaw)), nil
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return "", "", readErr
	}
	persisted := append(raw, '\n')
	if err := writeOwnerOnlyDurableAtomicFile(path, persisted, true); err != nil {
		return "", "", err
	}
	return receipt.ReceiptID, "sha256:" + sha256Hex(string(persisted)), nil
}

func memoryGraphRepairExactAppliedRow(action memoryGraphRepairAction, evidence memoryGraphRepairEdgeEvidence, applyRunID string, plan memoryGraphRepairPlanReceipt, planDigest, custodyDigest string) (memoryEdgeEntry, bool) {
	rows := evidence.RepairActionRows[applyRunID]
	applied, ok := rows[action.ActionID]
	if !ok || !memoryGraphRepairImmutableEdgeTupleMatches(action.Edge, applied) {
		return memoryEdgeEntry{}, false
	}
	if !memoryGraphRepairServerAppendMarkerValid("repair", applied) {
		return memoryEdgeEntry{}, false
	}
	if memoryGraphRepairEdgeMetadataRepair(applied, "repair_run_id") != applyRunID ||
		memoryGraphRepairEdgeMetadataRepair(applied, "repair_action_id") != action.ActionID ||
		memoryGraphRepairEdgeMetadataRepair(applied, "repair_plan_ref") != plan.ReceiptRef ||
		memoryGraphRepairEdgeMetadataRepair(applied, "repair_plan_digest") != planDigest ||
		memoryGraphRepairEdgeMetadataRepair(applied, "repair_action_digest") != plan.ActionDigest ||
		memoryGraphRepairEdgeMetadataRepair(applied, "repair_custody_digest") != custodyDigest {
		return memoryEdgeEntry{}, false
	}
	return applied, true
}

func memoryGraphRepairRollbackRowIdentity(edge memoryEdgeEntry, rollbackRunID string, plan memoryGraphRepairPlanReceipt, planDigest, custodyDigest string) bool {
	return memoryGraphRepairEdgeMetadataRepair(edge, "rollback_run_id") == rollbackRunID &&
		memoryGraphRepairEdgeMetadataRepair(edge, "rollback_plan_ref") == plan.ReceiptRef &&
		memoryGraphRepairEdgeMetadataRepair(edge, "rollback_plan_digest") == planDigest &&
		memoryGraphRepairEdgeMetadataRepair(edge, "rollback_custody_digest") == custodyDigest &&
		memoryGraphRepairServerAppendMarkerValid("rollback", edge)
}

func memoryGraphRepairRollbackActionRecovered(action memoryGraphRepairAction, evidence memoryGraphRepairEdgeEvidence, rollbackRunID string, rollbackCursor int, plan memoryGraphRepairPlanReceipt, planDigest, custodyDigest string) (memoryEdgeEntry, bool) {
	latest, ok := evidence.Latest[action.Edge.EdgeID]
	return memoryGraphRepairRollbackActionRecoveredWithLatest(action, evidence, rollbackRunID, rollbackCursor, plan, planDigest, custodyDigest, latest, ok)
}

func memoryGraphRepairRollbackActionRecoveredWithLatest(action memoryGraphRepairAction, evidence memoryGraphRepairEdgeEvidence, rollbackRunID string, rollbackCursor int, plan memoryGraphRepairPlanReceipt, planDigest, custodyDigest string, latest memoryEdgeEntry, latestOK bool) (memoryEdgeEntry, bool) {
	rows := evidence.RollbackActionRows[rollbackRunID]
	recovered, ok := rows[action.ActionID]
	if !ok || !memoryGraphRepairImmutableEdgeTupleMatches(action.Edge, recovered) || !memoryGraphRepairRollbackRowIdentity(recovered, rollbackRunID, plan, planDigest, custodyDigest) || anyToInt(recovered.Metadata["rollback_cursor"], -1) != rollbackCursor {
		return memoryEdgeEntry{}, false
	}
	if !latestOK {
		return memoryEdgeEntry{}, false
	}
	if memoryGraphRepairOptionalEdgeDigest(latest) == memoryGraphRepairOptionalEdgeDigest(recovered) {
		return recovered, true
	}
	// Multiple reverse actions can target one edge in the same rollback chunk.
	// A later exact rollback row is valid proof that this earlier action was
	// already durably appended before a receipt/checkpoint crash.
	if memoryGraphRepairRollbackRowIdentity(latest, rollbackRunID, plan, planDigest, custodyDigest) && anyToInt(latest.Metadata["rollback_cursor"], -1) > rollbackCursor {
		return recovered, true
	}
	return memoryEdgeEntry{}, false
}

func memoryGraphRepairRollbackRestore(action memoryGraphRepairAction, evidence memoryGraphRepairEdgeEvidence, applyRunID string, applied, current memoryEdgeEntry, rollbackRunID string) (memoryEdgeEntry, error) {
	restore := action.Previous
	if action.PreviousActionID != "" {
		rows := evidence.RepairActionRows[applyRunID]
		var ok bool
		restore, ok = rows[action.PreviousActionID]
		if !ok {
			return memoryEdgeEntry{}, errors.New("rollback predecessor row is unavailable")
		}
	}
	if restore.EdgeID == "" {
		if memoryGraphRepairEdgeMetadataRepair(applied, "repair_prior_row_digest") != "" {
			return memoryEdgeEntry{}, errors.New("rollback repair-created edge unexpectedly binds a prior row")
		}
		restore = current
		restore.Lifecycle = "retired"
	} else if expected := memoryGraphRepairEdgeMetadataRepair(applied, "repair_prior_row_digest"); expected == "" || memoryGraphRepairOptionalEdgeDigest(restore) != expected {
		return memoryEdgeEntry{}, errors.New("rollback prior row digest does not match the exact repair append")
	}
	restore.Metadata = cloneJSONMap(restore.Metadata)
	if restore.Metadata == nil {
		restore.Metadata = map[string]any{}
	}
	for _, key := range []string{"repair_prior_edge", "rollback_prior_edge"} {
		delete(restore.Metadata, key)
	}
	restore.Metadata["rollback_run_id"] = rollbackRunID
	restore.Metadata["rollback_action_id"] = action.ActionID
	restore.Metadata["rollback_restores_action_id"] = action.PreviousActionID
	restore.Metadata["rollback_of_row_digest"] = memoryGraphRepairOptionalEdgeDigest(current)
	restore.Metadata["rollback_mode"] = "authenticated_cas_append"
	return restore, nil
}

func (m *memoryStore) executeMemoryGraphRepairRollback(ctx context.Context, snapshot memoryGraphRepairSnapshot, evidence memoryGraphRepairEdgeEvidence, plan memoryGraphRepairPlanReceipt, planDigest string, req memoryGraphRepairRequest, validateRepairLock func() error) (map[string]any, error) {
	if validateRepairLock != nil {
		if err := validateRepairLock(); err != nil {
			return nil, memoryGraphRepairErr("repair_lock_io", err)
		}
	}
	applyCheckpoint, exists, err := m.loadMemoryGraphRepairCheckpoint(req.Project, plan.ReceiptRef)
	if err != nil {
		return nil, memoryGraphRepairIOErr("checkpoint_invalid", err)
	}
	if !exists {
		return nil, memoryGraphRepairErr("rollback_checkpoint_mismatch", errors.New("applied repair checkpoint is unavailable"))
	}
	if applyCheckpoint.Cursor < 0 || applyCheckpoint.Cursor > len(plan.Actions) || applyCheckpoint.TotalActions != len(plan.Actions) || (applyCheckpoint.Status != "running" && applyCheckpoint.Status != "complete") || (applyCheckpoint.Status == "complete" && applyCheckpoint.Cursor != len(plan.Actions)) {
		return nil, memoryGraphRepairErr("checkpoint_invalid", errors.New("applied repair checkpoint cursor or action count is invalid"))
	}
	if applyCheckpoint.ActorCustodyDigest != req.ActorCustodyDigest || applyCheckpoint.PlanReceiptDigest != planDigest {
		return nil, memoryGraphRepairErr("custody_mismatch", errors.New("rollback custody does not match the applied plan"))
	}
	rollback, rollbackExists, err := m.loadMemoryGraphRepairRollbackCheckpoint(plan.ReceiptRef)
	if err != nil {
		return nil, memoryGraphRepairIOErr("checkpoint_invalid", err)
	}
	if rollbackExists {
		if req.CheckpointID != rollback.CheckpointID && req.CheckpointID != applyCheckpoint.CheckpointID {
			return nil, memoryGraphRepairErr("rollback_checkpoint_mismatch", errors.New("rollback checkpoint_id does not match"))
		}
		if rollback.ActorCustodyDigest != req.ActorCustodyDigest {
			return nil, memoryGraphRepairErr("custody_mismatch", errors.New("rollback continuation custody does not match"))
		}
		if rollback.Project != req.Project || rollback.PlanReceiptDigest != planDigest || rollback.ApplyRunID != applyCheckpoint.RunID || rollback.ApplyCheckpointID != applyCheckpoint.CheckpointID || rollback.AppliedActionCount != applyCheckpoint.Cursor || rollback.Cursor < 0 || rollback.Cursor > rollback.AppliedActionCount || rollback.Counts == nil || (rollback.Status != "running" && rollback.Status != "complete") || (rollback.Status == "complete" && rollback.Cursor != rollback.AppliedActionCount) || rollback.SnapshotDigest != plan.SnapshotDigest || rollback.PolicyDigest != plan.PolicyDigest || rollback.ActionDigest != plan.ActionDigest || rollback.ObservedAt != plan.ObservedAt {
			return nil, memoryGraphRepairErr("rollback_checkpoint_mismatch", errors.New("rollback checkpoint identity or cursor does not match the immutable plan"))
		}
		if rollback.Status == "complete" {
			return map[string]any{"ok": true, "mode": "rollback", "rollback": true, "idempotent": true, "status": "complete", "rollback_checkpoint_id": rollback.CheckpointID, "cursor": rollback.Cursor, "total_actions": rollback.AppliedActionCount, "counts": rollback.Counts, "custody": memoryGraphRepairCustodyForRequest(m, req)}, nil
		}
	} else {
		if req.CheckpointID != applyCheckpoint.CheckpointID {
			return nil, memoryGraphRepairErr("rollback_checkpoint_mismatch", errors.New("initial rollback requires the exact apply checkpoint_id"))
		}
		seed := plan.ReceiptRef + "\x00" + applyCheckpoint.RunID + "\x00" + req.ActorCustodyDigest
		rollback = memoryGraphRepairRollbackCheckpoint{CheckpointID: "rollback_" + sha256Hex(seed)[:24], RunID: "rollback_run_" + sha256Hex(seed)[:32], Project: req.Project, PlanReceiptRef: plan.ReceiptRef, PlanReceiptDigest: planDigest, SnapshotDigest: plan.SnapshotDigest, PolicyDigest: plan.PolicyDigest, ActionDigest: plan.ActionDigest, ObservedAt: plan.ObservedAt, ApplyCheckpointID: applyCheckpoint.CheckpointID, ApplyRunID: applyCheckpoint.RunID, AppliedActionCount: applyCheckpoint.Cursor, Status: "running", Counts: map[string]int{"restored_total": 0, "retired_created_total": 0, "chunks": 0}, EdgeDigestAfter: evidence.Digest, EdgeDigestAlgorithm: memoryGraphRepairDigestAlgorithm, EdgeLogGeneration: evidence.LogGeneration, EdgeLogDigest: evidence.LogDigest, EdgeLogContentDigest: evidence.LogContentDigest, EdgeLogContentHashState: evidence.LogContentHashState, EdgeLogContentHashedBytes: evidence.LogContentHashedBytes, EdgeLogFileSize: evidence.LogFileSize, EdgeLogFileIdentity: evidence.LogFileIdentity, RollbackRows: map[string]memoryEdgeEntry{}, ActorCustodyDigest: req.ActorCustodyDigest}
	}
	if rollback.Counts == nil {
		return nil, memoryGraphRepairErr("checkpoint_invalid", errors.New("rollback checkpoint counts are invalid"))
	}
	start := rollback.Cursor
	chunkSize := clampInt(req.ChunkSize, 1, memoryGraphRepairMaxLockActions)
	end := minInt(rollback.AppliedActionCount, start+chunkSize)
	fence, err := m.acquireMemoryEdgeLogFenceContext(ctx)
	if err != nil {
		return nil, memoryGraphRepairErr("rollback_io", err)
	}
	defer fence.release()
	pathUnlock, pathErr := m.lockMemoryPathContext(ctx, "memory-graph-repair:"+strings.ToLower(strings.TrimSpace(req.Project)))
	if pathErr != nil {
		return nil, memoryGraphRepairErr("repair_lock_io", pathErr)
	}
	defer pathUnlock()
	if validateRepairLock != nil {
		if err := validateRepairLock(); err != nil {
			return nil, memoryGraphRepairErr("repair_lock_io", err)
		}
	}
	appender, err := m.newMemoryEdgeLogAppenderFastWithFenceContextLocked(ctx, true, fence)
	if err != nil {
		return nil, memoryGraphRepairErr("rollback_io", err)
	}
	if evidence.Continuation {
		pending := map[string]struct{}{}
		for offset := start; offset < end; offset++ {
			pending[plan.Actions[rollback.AppliedActionCount-1-offset].ActionID] = struct{}{}
		}
		if err := m.refreshMemoryGraphRepairContinuationEvidence(ctx, &evidence, plan, applyCheckpoint, &rollback, appender.state, pending); err != nil {
			return nil, memoryGraphRepairErr("snapshot_drift", err)
		}
	}
	if appender.state.Generation != evidence.LogGeneration || appender.state.Digest != evidence.LogDigest || appender.state.ContentDigest != evidence.LogContentDigest {
		return nil, memoryGraphRepairErr("snapshot_drift", errors.New("edge log changed before rollback commit"))
	}
	// Keep only the edge identities touched by this rollback chunk. Copying the
	// complete latest projection here made each 64-action continuation scan the
	// entire historical prefix; the overlay preserves rollback CAS semantics
	// without turning O(actions+new bytes) into O(actions*rows).
	latestOverlay := map[string]memoryEdgeEntry{}
	lookupLatest := func(edgeID string) (memoryEdgeEntry, bool) {
		if edge, ok := latestOverlay[edgeID]; ok {
			return edge, true
		}
		edge, ok := evidence.Latest[edgeID]
		return edge, ok
	}
	type preparedRollback struct {
		action  memoryGraphRepairAction
		restore memoryEdgeEntry
		recover bool
	}
	prepared := make([]preparedRollback, 0, end-start)
	for offset := start; offset < end; offset++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		action := plan.Actions[rollback.AppliedActionCount-1-offset]
		appliedRow, appliedOK := memoryGraphRepairExactAppliedRow(action, evidence, applyCheckpoint.RunID, plan, planDigest, req.ActorCustodyDigest)
		if !appliedOK {
			return nil, memoryGraphRepairErr("rollback_superseded", fmt.Errorf("rollback action %s has no exact immutable repair append", action.ActionID))
		}
		latest, latestOK := lookupLatest(action.Edge.EdgeID)
		if recovered, ok := memoryGraphRepairRollbackActionRecoveredWithLatest(action, evidence, rollback.RunID, offset, plan, planDigest, req.ActorCustodyDigest, latest, latestOK); ok {
			prepared = append(prepared, preparedRollback{action: action, restore: recovered, recover: true})
			continue
		}
		current, ok := latest, latestOK
		if !ok {
			return nil, memoryGraphRepairErr("rollback_superseded", fmt.Errorf("rollback action %s has no latest durable row", action.ActionID))
		}
		exactRepairRow := memoryGraphRepairOptionalEdgeDigest(current) == memoryGraphRepairOptionalEdgeDigest(appliedRow)
		exactPriorRollback := memoryGraphRepairRollbackRowIdentity(current, rollback.RunID, plan, planDigest, req.ActorCustodyDigest) &&
			memoryGraphRepairEdgeMetadataRepair(current, "rollback_restores_action_id") == action.ActionID && anyToInt(current.Metadata["rollback_cursor"], -1) < offset
		if !exactRepairRow && !exactPriorRollback {
			return nil, memoryGraphRepairErr("rollback_superseded", fmt.Errorf("rollback action %s refuses a superseding durable write", action.ActionID))
		}
		restore, restoreErr := memoryGraphRepairRollbackRestore(action, evidence, applyCheckpoint.RunID, appliedRow, current, rollback.RunID)
		if restoreErr != nil {
			return nil, memoryGraphRepairErr("rollback_superseded", restoreErr)
		}
		restore.Metadata["rollback_cursor"] = offset
		restore.Metadata["rollback_plan_ref"] = plan.ReceiptRef
		restore.Metadata["rollback_plan_digest"] = planDigest
		restore.Metadata["rollback_apply_run_id"] = applyCheckpoint.RunID
		restore.Metadata["rollback_custody_digest"] = req.ActorCustodyDigest
		restore.Metadata["rollback_server_append_marker"] = memoryGraphRepairServerAppendMarker("rollback", restore)
		latestOverlay[action.Edge.EdgeID] = restore
		prepared = append(prepared, preparedRollback{action: action, restore: restore})
	}
	receiptActions := []map[string]any{}
	for _, item := range prepared {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		appended := item.restore
		if rollback.RollbackRows == nil {
			rollback.RollbackRows = map[string]memoryEdgeEntry{}
		}
		if !item.recover {
			appended, err = m.appendMemoryGraphRepairEdgeWithAppenderFenceLocked(item.restore, appender, fence)
			if err != nil {
				return nil, memoryGraphRepairErr("rollback_io", err)
			}
			row, rowErr := memoryGraphRepairCanonicalizeEdge(appended, snapshot)
			if rowErr != nil {
				return nil, rowErr
			}
			row.Bound = item.action.BindingReason == "bound_current_state"
			row.RawDigest = "sha256:" + sha256Hex(string(mustJSON(appended)))
			evidence.record(row)
			evidence.ScannedLines++
			if encoded, encodeErr := json.Marshal(appended); encodeErr == nil {
				evidence.ScannedBytes += int64(len(encoded) + 1)
			}
		}
		rollback.RollbackRows[item.action.ActionID] = appended
		receiptActions = append(receiptActions, map[string]any{"action_id": item.action.ActionID, "edge_id": appended.EdgeID, "restored_row_digest": memoryGraphRepairOptionalEdgeDigest(appended), "durable": true})
		rollback.Counts["restored_total"]++
		if item.action.Previous.EdgeID == "" && item.action.PreviousActionID == "" {
			rollback.Counts["retired_created_total"]++
		}
	}
	evidence.Digest = evidence.projectDigest()
	committed := memoryEdgeLogSnapshot{Generation: appender.state.Generation, Digest: appender.state.Digest, ContentDigest: appender.state.ContentDigest, FileSize: appender.state.FileSize, FileStamp: memoryEdgeLogFileStamp{Exists: true, Size: appender.state.FileSize, Identity: appender.state.FileIdentity, ModTimeNanos: appender.state.FileModTimeNanos, ChangeToken: appender.state.FileChangeToken}}
	evidence.LogGeneration = committed.Generation
	evidence.LogDigest = committed.Digest
	evidence.LogContentDigest = committed.ContentDigest
	evidence.LogContentHashState = appender.state.ContentHashState
	evidence.LogContentHashedBytes = appender.state.ContentHashedBytes
	evidence.LogFileSize = committed.FileSize
	if !evidence.Continuation {
		if err := m.persistMemoryGraphRepairEvidenceArtifact(snapshot, evidence); err != nil {
			return nil, memoryGraphRepairIOErr("rollback_io", err)
		}
		m.cacheMemoryGraphRepairEvidence(snapshot.SnapshotDigest, snapshot.Project, evidence)
	}
	rollback.Cursor = end
	rollback.Counts["chunks"]++
	rollback.EdgeDigestAfter, rollback.EdgeLogGeneration, rollback.EdgeLogDigest, rollback.EdgeLogContentDigest = evidence.Digest, committed.Generation, committed.Digest, committed.ContentDigest
	rollback.EdgeLogFileSize, rollback.EdgeLogFileIdentity = committed.FileSize, committed.FileStamp.Identity
	rollback.EdgeLogContentHashState, rollback.EdgeLogContentHashedBytes = appender.state.ContentHashState, appender.state.ContentHashedBytes
	rollback.EdgeDigestAlgorithm = memoryGraphRepairDigestAlgorithm
	if end == rollback.AppliedActionCount {
		rollback.Status = "complete"
	}
	receipt := memoryGraphRepairRollbackChunkReceipt{RunID: rollback.RunID, CheckpointID: rollback.CheckpointID, Project: req.Project, PlanReceiptRef: plan.ReceiptRef, PlanReceiptDigest: planDigest, SnapshotDigest: plan.SnapshotDigest, PolicyDigest: plan.PolicyDigest, ActionDigest: plan.ActionDigest, ObservedAt: plan.ObservedAt, CursorBefore: start, CursorAfter: end, EdgeDigestAfter: evidence.Digest, EdgeDigestAlgorithm: memoryGraphRepairDigestAlgorithm, EdgeLogGeneration: committed.Generation, EdgeLogDigest: committed.Digest, EdgeLogContentDigest: committed.ContentDigest, Actions: receiptActions, ActorCustodyDigest: req.ActorCustodyDigest}
	receiptRef, receiptDigest, err := m.writeMemoryGraphRepairRollbackReceipt(receipt)
	if err != nil {
		return nil, memoryGraphRepairIOErr("rollback_io", err)
	}
	rollback.LastReceiptDigest = receiptDigest
	if m.memoryGraphRepairBeforeRollbackCheckpoint != nil {
		if err := m.memoryGraphRepairBeforeRollbackCheckpoint(); err != nil {
			return nil, err
		}
	}
	if err := m.writeMemoryGraphRepairRollbackCheckpoint(rollback); err != nil {
		return nil, memoryGraphRepairIOErr("rollback_io", err)
	}
	return map[string]any{"ok": true, "mode": "rollback", "rollback": true, "status": rollback.Status, "rollback_checkpoint_id": rollback.CheckpointID, "apply_checkpoint_id": applyCheckpoint.CheckpointID, "run_id": rollback.RunID, "cursor": rollback.Cursor, "remaining": rollback.AppliedActionCount - rollback.Cursor, "total_actions": rollback.AppliedActionCount, "counts": rollback.Counts, "edge_digest": rollback.EdgeDigestAfter, "edge_log_generation": committed.Generation, "edge_log_digest": committed.Digest, "receipt_ref": receiptRef, "receipt_digest": receiptDigest, "custody": memoryGraphRepairCustodyForRequest(m, req)}, nil
}

func memoryGraphRepairReport(snapshot memoryGraphRepairSnapshot, evidence memoryGraphRepairEdgeEvidence, actions []memoryGraphRepairAction, req memoryGraphRepairRequest, planCounts map[string]int, plan *memoryGraphRepairPlanReceipt) map[string]any {
	projectEdgeRows, projectDuplicateCount := memoryGraphRepairProjectEvidenceCounts(evidence, req.Project)
	bindingBefore := memoryGraphRepairBindingSummary(snapshot, evidence, req.Project)
	var bindingProjectedAfter map[string]any
	var resolutionProof map[string]any
	if plan != nil {
		// Mutation and replay requests already carry the immutable projection and
		// proof from the plan receipt.  Rebuilding them from every historical row
		// and every action on each 64-action continuation was the unbounded path
		// this lane explicitly forbids.
		bindingProjectedAfter = cloneJSONMap(plan.BindingProjectedAfter)
		resolutionProof = cloneJSONMap(plan.ResolutionProof)
	} else {
		projectedEvidence := memoryGraphRepairProjectedEvidence(snapshot, evidence, actions, req.Project)
		bindingProjectedAfter = memoryGraphRepairBindingSummary(snapshot, projectedEvidence, req.Project)
		resolutionProof = memoryGraphRepairActionResolution(snapshot, actions, nil, "")
	}
	boundNodes := map[string]struct{}{}
	activeBound := anyToInt(bindingBefore["bound_edges"], 0)
	activeUnbound := anyToInt(bindingBefore["unbound_edges"], 0)
	unboundExplicit := 0
	unboundInferred := 0
	if evidence.Project == req.Project && evidence.ProjectAliasIndex != nil && evidence.ProjectBoundNodeRefs != nil {
		for node := range evidence.ProjectBoundNodeRefs {
			boundNodes[node] = struct{}{}
		}
		unboundExplicit = evidence.ProjectUnboundExplicit
		unboundInferred = evidence.ProjectUnboundInferred
	} else {
		for _, row := range evidence.Latest {
			if !memoryGraphRepairEdgeInProject(row, req.Project) || memoryGraphRepairEdgeRetired(row) {
				continue
			}
			sourceID, sourceStatus := (&memoryGraphRepairAliasIndex{Aliases: snapshot.Aliases, AmbiguousAliases: snapshot.AmbiguousAliases}).resolve(row.SourceID)
			targetID, targetStatus := (&memoryGraphRepairAliasIndex{Aliases: snapshot.Aliases, AmbiguousAliases: snapshot.AmbiguousAliases}).resolve(row.TargetID)
			bound := sourceStatus == "bound" && targetStatus == "bound"
			if bound {
				boundNodes[sourceID] = struct{}{}
				boundNodes[targetID] = struct{}{}
			} else if memoryGraphRepairEdgeIsInferred(row) {
				unboundInferred++
			} else {
				unboundExplicit++
			}
		}
	}
	mode := "dry_run"
	if req.Apply {
		mode = "apply"
	} else if req.Rollback {
		mode = "rollback"
	}
	return map[string]any{
		"ok": true, "dry_run": req.DryRun, "apply": req.Apply, "rollback": req.Rollback, "mode": mode,
		"schema_id": memoryGraphRepairSchemaID, "project": req.Project, "snapshot_complete": snapshot.Complete, "edge_evidence_complete": evidence.Complete,
		"snapshot_digest": snapshot.SnapshotDigest, "edge_digest": evidence.Digest, "key_generation": snapshot.KeyGeneration, "topic_generation": snapshot.TopicGeneration,
		"indexed_doc_count": snapshot.IndexedCount, "eligible_doc_count": len(snapshot.Docs), "excluded_doc_count": snapshot.ExcludedCount,
		"scanned_edge_lines": evidence.ScannedLines, "project_edge_row_count": projectEdgeRows, "duplicate_edge_count": projectDuplicateCount, "invalid_edge_count": 0,
		"bound_edge_count": activeBound, "unbound_edge_count": activeUnbound, "unbound_explicit_count": unboundExplicit, "unbound_inferred_count": unboundInferred,
		"connected_doc_count": len(boundNodes), "isolated_doc_count": maxInt(0, len(snapshot.Docs)-len(boundNodes)),
		"connectedness_claim": "bound_current_state_intersection_only", "qdrant_authoritative": false,
		"raw_store_scanned": false, "source_index": "gateway_current_state_index", "plan_counts": planCounts,
		"binding_before": bindingBefore, "binding_projected_after": bindingProjectedAfter, "repair_resolution_proof": resolutionProof,
		"actor_principal_digest": req.ActorPrincipalDigest, "actor_scope_digest": req.ActorScopeDigest,
		"would_apply": len(actions), "actions": len(actions),
		"telemetry": map[string]any{"bound_edge_count": activeBound, "unbound_edge_count": activeUnbound, "connected_doc_count": len(boundNodes), "isolated_doc_count": maxInt(0, len(snapshot.Docs)-len(boundNodes)), "claim": "bound_current_state_intersection_only"},
	}
}

// memoryGraphRepairContinuationReport is the mutation/replay report boundary.
// It consumes only the immutable plan summaries and the bounded checkpoint
// suffix overlay; it never walks snapshot.Docs or historical evidence.Rows.
func memoryGraphRepairContinuationReport(snapshot memoryGraphRepairSnapshot, evidence memoryGraphRepairEdgeEvidence, actions []memoryGraphRepairAction, req memoryGraphRepairRequest, planCounts map[string]int, plan *memoryGraphRepairPlanReceipt) map[string]any {
	bindingBefore := map[string]any{}
	bindingProjectedAfter := map[string]any{}
	resolutionProof := map[string]any{}
	if plan != nil {
		bindingBefore = cloneJSONMap(plan.BindingBefore)
		bindingProjectedAfter = cloneJSONMap(plan.BindingProjectedAfter)
		resolutionProof = cloneJSONMap(plan.ResolutionProof)
	}
	activeBound := anyToInt(bindingBefore["bound_edges"], 0)
	activeUnbound := anyToInt(bindingBefore["unbound_edges"], 0)
	eligibleDocs := planSnapshotEligibleCount(plan, snapshot)
	connectedDocs, isolatedDocs := 0, eligibleDocs
	if plan != nil {
		connectedDocs, isolatedDocs = plan.SnapshotConnectedDocCount, plan.SnapshotIsolatedDocCount
	}
	return map[string]any{
		"ok": true, "dry_run": req.DryRun, "apply": req.Apply, "rollback": req.Rollback,
		"mode": func() string {
			if req.Rollback {
				return "rollback"
			}
			if req.Apply {
				return "apply"
			}
			return "dry_run"
		}(),
		"schema_id": memoryGraphRepairSchemaID, "project": req.Project, "snapshot_complete": snapshot.Complete, "edge_evidence_complete": evidence.Complete,
		"snapshot_digest": snapshot.SnapshotDigest, "edge_digest": evidence.Digest, "key_generation": snapshot.KeyGeneration, "topic_generation": snapshot.TopicGeneration,
		"indexed_doc_count": snapshot.IndexedCount, "eligible_doc_count": eligibleDocs, "excluded_doc_count": snapshot.ExcludedCount,
		"scanned_edge_lines":     planInt(plan, func(receipt memoryGraphRepairPlanReceipt) int { return receipt.EdgeScannedLines }) + evidence.ScannedLines,
		"project_edge_row_count": planInt(plan, func(receipt memoryGraphRepairPlanReceipt) int { return receipt.EdgeProjectRowCount }),
		"duplicate_edge_count":   planInt(plan, func(receipt memoryGraphRepairPlanReceipt) int { return receipt.EdgeDuplicateCount }),
		"invalid_edge_count":     planInt(plan, func(receipt memoryGraphRepairPlanReceipt) int { return receipt.EdgeInvalidCount }),
		"bound_edge_count":       activeBound, "unbound_edge_count": activeUnbound,
		"unbound_explicit_count": planInt(plan, func(receipt memoryGraphRepairPlanReceipt) int { return receipt.EdgeUnboundExplicit }),
		"unbound_inferred_count": planInt(plan, func(receipt memoryGraphRepairPlanReceipt) int { return receipt.EdgeUnboundInferred }),
		"connected_doc_count":    connectedDocs, "isolated_doc_count": isolatedDocs,
		"connectedness_claim": "bound_current_state_intersection_only", "qdrant_authoritative": false,
		"raw_store_scanned": false, "source_index": "gateway_current_state_index", "plan_counts": planCounts,
		"binding_before": bindingBefore, "binding_projected_after": bindingProjectedAfter, "repair_resolution_proof": resolutionProof,
		"actor_principal_digest": req.ActorPrincipalDigest, "actor_scope_digest": req.ActorScopeDigest,
		"would_apply": len(actions), "actions": len(actions), "continuation_state": "durable_checkpoint_index",
		"continuation_scan_bytes": evidence.ScannedBytes,
		"telemetry":               map[string]any{"bound_edge_count": activeBound, "unbound_edge_count": activeUnbound, "connected_doc_count": connectedDocs, "isolated_doc_count": isolatedDocs, "claim": "bound_current_state_intersection_only"},
	}
}

func memoryGraphRepairBindingBucketFromAny(raw any) map[string]int {
	bucket := memoryGraphRepairBindingBucket()
	switch typed := raw.(type) {
	case map[string]int:
		for key, value := range typed {
			bucket[key] = value
		}
	case map[string]any:
		for key, value := range typed {
			bucket[key] = anyToInt(value, 0)
		}
	}
	return bucket
}

func memoryGraphRepairBindingBucketsFromAny(raw any) map[string]map[string]int {
	result := map[string]map[string]int{}
	switch typed := raw.(type) {
	case map[string]map[string]int:
		for key, value := range typed {
			result[key] = cloneMemoryGraphRepairBindingBucket(value)
		}
	case map[string]any:
		for key, value := range typed {
			result[key] = memoryGraphRepairBindingBucketFromAny(value)
		}
	}
	return result
}

func memoryGraphRepairContinuationBindingDelta(summary map[string]any, edge memoryEdgeEntry, reason string, delta int) {
	if summary == nil || edge.EdgeID == "" {
		return
	}
	if reason == "" {
		reason = "unknown_endpoint"
	}
	byReason := memoryGraphRepairBindingBucketFromAny(summary["by_reason"])
	byRelation := memoryGraphRepairBindingBucketsFromAny(summary["by_relation"])
	byProject := memoryGraphRepairBindingBucketsFromAny(summary["by_project"])
	memoryGraphRepairAdjustBindingBucket(byReason, reason, delta)
	relation := strings.TrimSpace(edge.Relation)
	if relation == "" {
		relation = "unknown"
	}
	if byRelation[relation] == nil {
		byRelation[relation] = memoryGraphRepairBindingBucket()
	}
	memoryGraphRepairAdjustBindingBucket(byRelation[relation], reason, delta)
	project := strings.ToLower(strings.TrimSpace(edge.Project))
	if project == "" {
		project = "unknown"
	}
	if byProject[project] == nil {
		byProject[project] = memoryGraphRepairBindingBucket()
	}
	memoryGraphRepairAdjustBindingBucket(byProject[project], reason, delta)
	summary["by_reason"], summary["by_relation"], summary["by_project"] = byReason, byRelation, byProject
	summary["bound_edges"] = byReason["bound"]
	summary["unbound_edges"] = byReason["unbound"]
	summary["active_edges"] = byReason["bound"] + byReason["unbound"]
}

func memoryGraphRepairContinuationBindingAfter(before map[string]any, actions []memoryGraphRepairAction) map[string]any {
	after := cloneJSONMap(before)
	for _, action := range actions {
		if action.Kind == "retire" {
			prior := action.Previous
			if prior.EdgeID == "" {
				prior = action.Edge
			}
			reason := action.PreviousBindingReason
			if reason == "" {
				reason = action.BindingReason
			}
			memoryGraphRepairContinuationBindingDelta(after, prior, reason, -1)
			continue
		}
		if action.PreviousActionID == "" && action.Previous.EdgeID != "" {
			memoryGraphRepairContinuationBindingDelta(after, action.Previous, action.PreviousBindingReason, -1)
		}
		memoryGraphRepairContinuationBindingDelta(after, action.Edge, action.BindingReason, 1)
	}
	return after
}

func planSnapshotEligibleCount(plan *memoryGraphRepairPlanReceipt, snapshot memoryGraphRepairSnapshot) int {
	if plan != nil && plan.SnapshotEligibleCount > 0 {
		return plan.SnapshotEligibleCount
	}
	return len(snapshot.Docs)
}

func planInt(plan *memoryGraphRepairPlanReceipt, pick func(memoryGraphRepairPlanReceipt) int) int {
	if plan == nil || pick == nil {
		return 0
	}
	return pick(*plan)
}

func (m *memoryStore) memoryGraphRepair(ctx context.Context, req memoryGraphRepairRequest) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req.ObservationAt.IsZero() {
		req.ObservationAt = time.Now().UTC()
	}
	if req.StaleAfter.IsZero() {
		req.StaleAfter = req.ObservationAt.Add(-memoryGraphRepairDefaultStale)
	}
	if !req.Apply && !req.Rollback && strings.TrimSpace(req.ActorAuthority) == "" {
		req.ActorAuthority = "anonymous_preview"
	}
	var validateRepairLock func() error
	if req.Apply || req.Rollback {
		repairLease, lockErr := lockOwnerOnlyFileContextWithValidation(ctx, memoryGraphRepairLockPath(m, req.Project))
		if lockErr != nil {
			return nil, memoryGraphRepairErr("repair_lock_io", lockErr)
		}
		validateRepairLock = repairLease.validate
		defer repairLease.unlock()
	}

	var planReceipt memoryGraphRepairPlanReceipt
	var planReceiptDigest string
	continuation := req.CheckpointID != ""
	replayPlan := !req.Apply && !req.Rollback && req.PlanReceiptRef != "" && req.PlanReceiptDigest != ""
	if req.Apply || req.Rollback || replayPlan {
		loaded, digest, err := m.loadMemoryGraphRepairPlanReceipt(req, continuation)
		if err != nil {
			return nil, memoryGraphRepairIOErr("plan_receipt_io", err)
		}
		planReceipt, planReceiptDigest = loaded, digest
		frozen, staleOK := parseTimeBestEffort(planReceipt.StaleAfter)
		observed, observedOK := parseTimeBestEffort(planReceipt.ObservedAt)
		if !staleOK || !observedOK {
			return nil, memoryGraphRepairErr("plan_receipt_invalid", errors.New("dry-run plan receipt time boundary is invalid"))
		}
		req.StaleAfter, req.ObservationAt = frozen, observed
	}

	var snapshot memoryGraphRepairSnapshot
	var evidence memoryGraphRepairEdgeEvidence
	var err error
	fastContinuation := (req.Apply || req.Rollback) && continuation
	fastPlanBoundary := (req.Apply || req.Rollback) && !continuation && memoryGraphRepairPlanHasCompactEdgeBoundary(planReceipt)
	fastMutation := fastContinuation || fastPlanBoundary
	if fastMutation {
		keyGeneration, topicGeneration, currentStateDigest, generationErr := m.durableCurrentStateGeneration(req.Project)
		if generationErr != nil {
			return nil, memoryGraphRepairErr("plan_receipt_drift", fmt.Errorf("immutable plan current-state generation is unavailable: %w", generationErr))
		}
		if keyGeneration != planReceipt.KeyGeneration || topicGeneration != planReceipt.TopicGeneration || currentStateDigest != planReceipt.CurrentStateDigest {
			return nil, memoryGraphRepairErr("plan_receipt_drift", errors.New("current-state/index generation changed since the immutable plan"))
		}
	}
	if fastContinuation {
		applyCheckpoint, checkpointExists, checkpointErr := m.loadMemoryGraphRepairCheckpoint(req.Project, planReceipt.ReceiptRef)
		if checkpointErr != nil {
			return nil, memoryGraphRepairIOErr("checkpoint_io", checkpointErr)
		}
		if !checkpointExists {
			return nil, memoryGraphRepairErr("checkpoint_mismatch", errors.New("continuation checkpoint is unavailable"))
		}
		var rollbackCheckpoint *memoryGraphRepairRollbackCheckpoint
		if req.Rollback {
			if loadedRollback, rollbackExists, rollbackErr := m.loadMemoryGraphRepairRollbackCheckpoint(planReceipt.ReceiptRef); rollbackErr != nil {
				return nil, memoryGraphRepairIOErr("checkpoint_io", rollbackErr)
			} else if rollbackExists {
				rollbackCheckpoint = &loadedRollback
			}
		}
		snapshot = memoryGraphRepairSnapshotFromPlan(planReceipt)
		evidence, err = memoryGraphRepairContinuationEvidence(planReceipt, applyCheckpoint, rollbackCheckpoint)
		if err != nil {
			return nil, memoryGraphRepairErr("checkpoint_invalid", err)
		}
	} else if fastPlanBoundary {
		if _, checkpointExists, checkpointErr := m.loadMemoryGraphRepairCheckpoint(req.Project, planReceipt.ReceiptRef); checkpointErr != nil {
			return nil, memoryGraphRepairIOErr("checkpoint_io", checkpointErr)
		} else if checkpointExists {
			return nil, memoryGraphRepairErr("checkpoint_resume_required", errors.New("a durable repair checkpoint already exists; resume with checkpoint_id"))
		}
		snapshot = memoryGraphRepairSnapshotFromPlan(planReceipt)
		boundary := memoryGraphRepairPlanBoundaryCheckpoint(planReceipt, planReceiptDigest)
		evidence, err = memoryGraphRepairContinuationEvidence(planReceipt, boundary, nil)
		if err != nil {
			return nil, memoryGraphRepairErr("plan_receipt_invalid", err)
		}
	} else if (req.Apply || req.Rollback || replayPlan) && strings.TrimSpace(planReceipt.SnapshotDigest) != "" {
		if cachedSnapshot, cachedOK, snapshotErr := m.loadMemoryGraphRepairSnapshotArtifact(req.Project, planReceipt.SnapshotDigest); snapshotErr != nil {
			return nil, memoryGraphRepairIOErr("checkpoint_io", snapshotErr)
		} else if cachedOK {
			keyGeneration, topicGeneration, generationErr := m.currentMemoryGraphRepairGeneration(req.Project)
			if generationErr != nil {
				return nil, memoryGraphRepairErr("snapshot_drift", generationErr)
			}
			if keyGeneration == cachedSnapshot.KeyGeneration && topicGeneration == cachedSnapshot.TopicGeneration {
				snapshot = cachedSnapshot
			}
		}
	}
	if !snapshot.Complete && !fastMutation {
		var captureErr error
		snapshot, captureErr = m.captureMemoryGraphRepairSnapshot(ctx, req)
		if captureErr != nil {
			return nil, captureErr
		}
	}
	if !fastMutation {
		if err := m.persistMemoryGraphRepairSnapshotArtifact(snapshot); err != nil {
			return nil, memoryGraphRepairIOErr("checkpoint_io", err)
		}
	}
	if !fastMutation {
		evidence, err = m.captureMemoryGraphRepairEdgesContext(ctx, snapshot)
		if err != nil {
			return nil, err
		}
	}
	actions := planReceipt.Actions
	counts := map[string]int{"retire": 0, "write": 0, "actions": len(actions)}
	if !req.Apply && !req.Rollback && !replayPlan {
		actions, counts, err = m.buildMemoryGraphRepairPlan(ctx, snapshot, evidence, req)
		if err != nil {
			return nil, err
		}
	} else {
		for _, action := range actions {
			counts[action.Kind]++
		}
	}
	policyDigest := memoryGraphRepairPolicyDigest(req)
	actionDigest := memoryGraphRepairActionDigest(actions)
	var reportPlan *memoryGraphRepairPlanReceipt
	if req.Apply || req.Rollback || replayPlan {
		reportPlan = &planReceipt
	}
	var report map[string]any
	if fastMutation {
		report = memoryGraphRepairContinuationReport(snapshot, evidence, actions, req, counts, reportPlan)
	} else {
		report = memoryGraphRepairReport(snapshot, evidence, actions, req, counts, reportPlan)
	}
	report["actor_authority"] = req.ActorAuthority
	report["custody"] = memoryGraphRepairCustodyForRequest(m, req)

	if req.Apply || req.Rollback || replayPlan {
		edgeMatches := planReceipt.EdgeDigest == evidence.Digest || continuation || fastPlanBoundary || req.Rollback
		bindingMatches := continuation || fastPlanBoundary || req.Rollback || (sha256Hex(string(mustJSON(planReceipt.BindingBefore))) == sha256Hex(string(mustJSON(report["binding_before"]))) && sha256Hex(string(mustJSON(planReceipt.BindingProjectedAfter))) == sha256Hex(string(mustJSON(report["binding_projected_after"]))) && sha256Hex(string(mustJSON(planReceipt.ResolutionProof))) == sha256Hex(string(mustJSON(report["repair_resolution_proof"]))))
		if planReceipt.SnapshotDigest != snapshot.SnapshotDigest || planReceipt.KeyGeneration != snapshot.KeyGeneration || planReceipt.TopicGeneration != snapshot.TopicGeneration || !edgeMatches || !bindingMatches || planReceipt.PolicyDigest != policyDigest || planReceipt.ActionDigest != actionDigest || planReceipt.ActionCount != len(actions) {
			return nil, memoryGraphRepairErr("plan_receipt_drift", errors.New("current bounded repair state differs from the immutable plan receipt"))
		}
	} else {
		planReceipt, planReceiptDigest, err = m.persistMemoryGraphRepairPlanReceipt(memoryGraphRepairPlanReceipt{
			Project: req.Project, SnapshotDigest: snapshot.SnapshotDigest, EdgeDigest: evidence.Digest, EdgeDigestAlgorithm: memoryGraphRepairDigestAlgorithm,
			CurrentStateDigest: snapshot.CurrentStateDigest,
			KeyGeneration:      snapshot.KeyGeneration, TopicGeneration: snapshot.TopicGeneration,
			PolicyDigest: policyDigest, ActionDigest: actionDigest, ActionCount: len(actions), Actions: actions,
			BindingBefore: report["binding_before"].(map[string]any), BindingProjectedAfter: report["binding_projected_after"].(map[string]any), ResolutionProof: report["repair_resolution_proof"].(map[string]any),
			SnapshotIndexedCount: snapshot.IndexedCount, SnapshotEligibleCount: len(snapshot.Docs), SnapshotExcludedCount: snapshot.ExcludedCount,
			SnapshotConnectedDocCount: anyToInt(report["connected_doc_count"], 0), SnapshotIsolatedDocCount: anyToInt(report["isolated_doc_count"], 0),
			EdgeScannedLines: anyToInt(report["scanned_edge_lines"], 0), EdgeProjectRowCount: anyToInt(report["project_edge_row_count"], 0),
			EdgeDuplicateCount: anyToInt(report["duplicate_edge_count"], 0), EdgeInvalidCount: anyToInt(report["invalid_edge_count"], 0),
			EdgeUnboundExplicit: anyToInt(report["unbound_explicit_count"], 0), EdgeUnboundInferred: anyToInt(report["unbound_inferred_count"], 0),
			StaleAfter: req.StaleAfter.UTC().Format(time.RFC3339Nano), ObservedAt: req.ObservationAt.UTC().Format(time.RFC3339Nano), ActorAuthority: req.ActorAuthority,
			ActorPrincipalDigest: req.ActorPrincipalDigest, ActorScopeDigest: req.ActorScopeDigest, ActorWorkspaceDigest: req.ActorWorkspaceDigest, ActorInstallDigest: req.ActorInstallDigest, ActorAuthorityDigest: req.ActorAuthorityDigest, ActorCustodyDigest: req.ActorCustodyDigest, Applicable: req.PlanApplicable,
			EdgeLogGeneration: evidence.LogGeneration, EdgeLogDigest: evidence.LogDigest, EdgeLogContentDigest: evidence.LogContentDigest,
			EdgeLogFileSize: evidence.LogFileSize, EdgeLogFileIdentity: evidence.LogFileIdentity,
			EdgeLogContentHashState: evidence.LogContentHashState, EdgeLogContentHashedBytes: evidence.LogContentHashedBytes,
		})
		if err != nil {
			return nil, memoryGraphRepairIOErr("plan_receipt_io", err)
		}
	}
	report["plan_receipt_ref"] = planReceipt.ReceiptRef
	report["plan_receipt_digest"] = planReceiptDigest
	report["next_checkpoint"] = "repair_" + sha256Hex(strings.ToLower(req.Project) + ":" + planReceipt.ReceiptRef + ":" + planReceiptDigest)[:24]
	report["plan_action_digest"] = actionDigest
	report["plan_policy_digest"] = policyDigest
	report["stale_after"] = req.StaleAfter.UTC().Format(time.RFC3339Nano)
	report["observed_at"] = req.ObservationAt.UTC().Format(time.RFC3339Nano)
	report["plan_expires_at"] = planReceipt.ExpiresAt
	report["plan_applicable"] = planReceipt.Applicable
	if replayPlan {
		report["idempotent"] = true
		report["plan_replayed"] = true
	}
	if req.DryRun {
		return report, nil
	}
	var mutation map[string]any
	if req.Rollback {
		mutation, err = m.executeMemoryGraphRepairRollback(ctx, snapshot, evidence, planReceipt, planReceiptDigest, req, validateRepairLock)
	} else {
		mutation, err = m.executeMemoryGraphRepair(ctx, snapshot, evidence, planReceipt, planReceiptDigest, req, validateRepairLock)
	}
	if err != nil {
		return nil, err
	}
	for key, value := range mutation {
		report[key] = value
	}
	return report, nil
}

func (s *server) authorizeMemoryGraphRepairApply(w http.ResponseWriter, r *http.Request, req *memoryGraphRepairRequest) (string, bool) {
	return optionalMemoryGraphRepairApplyAuthorization(s, w, r, req)
}

func (s *server) memoryV1EdgesRepair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	providedKey, explicitKey := requestAPIKey(r)
	_, ok := s.prepareAuthorizedHeaders(w, r)
	if !ok {
		return
	}
	if s.memoryStore == nil || !s.memoryStore.isEnabled() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "go memory store is disabled"})
		return
	}
	body, err := readRequestBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "failed to read request body"})
		return
	}
	payload, err := parseJSONMap(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid json", "detail": err.Error()})
		return
	}
	req, err := normalizeMemoryGraphRepairRequest(payload, s.memoryStore.policy)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if req.Apply || req.Rollback || (req.OperatorConfirmed && strings.EqualFold(req.ConfirmProject, req.Project)) {
		if !s.writeAuthorizedRequest(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "explicit authenticated operator authorization is required"})
			return
		}
		authority, authorized := s.authorizeMemoryGraphRepairApply(w, r, &req)
		if !authorized {
			return
		}
		req.ActorAuthority = authority
		if !req.OperatorConfirmed || req.ConfirmProject == "" || !strings.EqualFold(req.ConfirmProject, req.Project) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "operator_confirmed and confirm_project must exactly authorize the project"})
			return
		}
	} else {
		req.ActorAuthority = "anonymous_preview_non_applicable"
		if explicitKey {
			principal, scope := memoryGraphRepairActorDigests(providedKey, req.Project, "preview")
			req.ActorPrincipalDigest, req.ActorScopeDigest = principal, scope
			req.ActorAuthority = "authenticated_preview_non_applicable:" + principal
		}
		req.PlanApplicable = false
	}
	report, err := s.memoryStore.memoryGraphRepair(r.Context(), req)
	if err != nil {
		status := http.StatusBadGateway
		var typed *memoryGraphRepairError
		if errors.As(err, &typed) {
			status = memoryGraphRepairHTTPStatus(typed.Code)
		}
		publicError, code := memoryGraphRepairPublicError(err)
		writeJSON(w, status, map[string]any{"ok": false, "error": publicError, "code": code})
		return
	}
	writeJSON(w, http.StatusOK, report)
}
