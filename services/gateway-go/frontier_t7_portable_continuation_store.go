package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	frontierT7GrantRevocationSchemaID = "collaborative_context_grant_revocation.v1"
	frontierT7StatusSchemaID          = "portable_continuation_status.v1"
	frontierT7StateSchemaID           = "portable_continuation_state.v1"

	frontierT7EnabledEnv   = "CONTEXTLATTICE_FRONTIER_T7_PORTABLE_CONTINUATION_ENABLED"
	frontierT7StatePathEnv = "CONTEXTLATTICE_FRONTIER_T7_PORTABLE_CONTINUATION_STATE_PATH"

	frontierT7DefaultStatePath = ".data/orchestrator/frontier_t7_portable_continuation.json"
	frontierT7DefaultMaxBytes  = 8 * 1024 * 1024
	frontierT7MaximumMaxBytes  = 64 * 1024 * 1024
)

type frontierT7StoreLimits struct {
	MaxBytes       int
	MaxGrants      int
	MaxRevocations int
	MaxPlans       int
	MaxReceipts    int
	MaxManifests   int
	MaxReplay      int
}

type frontierT7GrantRevocation struct {
	SchemaID                       string                   `json:"schema_id"`
	Version                        int                      `json:"version"`
	RevocationID                   string                   `json:"revocation_id"`
	GrantID                        string                   `json:"grant_id"`
	GrantDigest                    string                   `json:"grant_digest"`
	Project                        string                   `json:"project"`
	SubjectSnapshotDigest          string                   `json:"subject_snapshot_digest"`
	KeyEpoch                       int                      `json:"key_epoch"`
	RevokedAt                      string                   `json:"revoked_at"`
	ReasonDigest                   string                   `json:"reason_digest"`
	Issuer                         contextPassportIssuer    `json:"issuer"`
	RevocationDigest               string                   `json:"revocation_digest"`
	Signature                      contextPassportSignature `json:"signature"`
	TombstoneOnly                  bool                     `json:"tombstone_only"`
	TransportOwnedByContextLattice bool                     `json:"transport_owned_by_contextlattice"`
	TransportExecuted              bool                     `json:"transport_executed"`
	PrivateKeyExported             bool                     `json:"private_key_exported"`
	OrdinaryMemoryMutated          bool                     `json:"ordinary_memory_mutated"`
	NetworkCalls                   int                      `json:"network_calls"`
}

type frontierT7GrantRevocationUnsigned struct {
	SchemaID                       string                `json:"schema_id"`
	Version                        int                   `json:"version"`
	RevocationID                   string                `json:"revocation_id"`
	GrantID                        string                `json:"grant_id"`
	GrantDigest                    string                `json:"grant_digest"`
	Project                        string                `json:"project"`
	SubjectSnapshotDigest          string                `json:"subject_snapshot_digest"`
	KeyEpoch                       int                   `json:"key_epoch"`
	RevokedAt                      string                `json:"revoked_at"`
	ReasonDigest                   string                `json:"reason_digest"`
	Issuer                         contextPassportIssuer `json:"issuer"`
	TombstoneOnly                  bool                  `json:"tombstone_only"`
	TransportOwnedByContextLattice bool                  `json:"transport_owned_by_contextlattice"`
	TransportExecuted              bool                  `json:"transport_executed"`
	PrivateKeyExported             bool                  `json:"private_key_exported"`
	OrdinaryMemoryMutated          bool                  `json:"ordinary_memory_mutated"`
	NetworkCalls                   int                   `json:"network_calls"`
}

func frontierT7GrantRevocationUnsignedValue(value frontierT7GrantRevocation) frontierT7GrantRevocationUnsigned {
	return frontierT7GrantRevocationUnsigned{
		SchemaID: value.SchemaID, Version: value.Version, RevocationID: value.RevocationID, GrantID: value.GrantID,
		GrantDigest: value.GrantDigest, Project: value.Project, SubjectSnapshotDigest: value.SubjectSnapshotDigest,
		KeyEpoch: value.KeyEpoch, ReasonDigest: value.ReasonDigest, RevokedAt: value.RevokedAt, Issuer: value.Issuer,
		TombstoneOnly: value.TombstoneOnly, TransportOwnedByContextLattice: value.TransportOwnedByContextLattice,
		TransportExecuted: value.TransportExecuted, PrivateKeyExported: value.PrivateKeyExported,
		OrdinaryMemoryMutated: value.OrdinaryMemoryMutated, NetworkCalls: value.NetworkCalls,
	}
}

func frontierT7VerifyGrantRevocation(value frontierT7GrantRevocation) bool {
	unsigned := frontierT7GrantRevocationUnsignedValue(value)
	revokedAt, revokedAtErr := time.Parse(time.RFC3339Nano, value.RevokedAt)
	return value.SchemaID == frontierT7GrantRevocationSchemaID && value.Version == 1 && value.RevocationID != "" && value.GrantID != "" && value.Project != "" &&
		frontierT7ValidDigest(value.GrantDigest) && frontierT7ValidDigest(value.SubjectSnapshotDigest) && frontierT7ValidDigest(value.ReasonDigest) && value.KeyEpoch > 0 &&
		revokedAtErr == nil && !revokedAt.IsZero() && value.TombstoneOnly && !value.TransportOwnedByContextLattice && !value.TransportExecuted && !value.PrivateKeyExported && !value.OrdinaryMemoryMutated && value.NetworkCalls == 0 &&
		value.RevocationDigest == frontierT7Digest(unsigned) && verifySignedBytes(struct {
		RevocationDigest string                            `json:"revocation_digest"`
		Revocation       frontierT7GrantRevocationUnsigned `json:"revocation"`
	}{RevocationDigest: value.RevocationDigest, Revocation: unsigned}, value.Signature, value.Issuer)
}

type frontierT7ManifestRecord struct {
	ManifestID     string `json:"manifest_id"`
	ManifestDigest string `json:"manifest_digest"`
	Project        string `json:"project"`
	RecipientKeyID string `json:"recipient_key_id"`
	Direction      string `json:"direction"`
	CreatedAt      string `json:"created_at"`
	ExpiresAt      string `json:"expires_at"`
}

type frontierT7ReplayRecord struct {
	ManifestDigest string `json:"manifest_digest"`
	RecordedAt     string `json:"recorded_at"`
	ExpiresAt      string `json:"expires_at"`
}

type frontierT7PortableState struct {
	SchemaID       string                                  `json:"schema_id"`
	Version        int                                     `json:"version"`
	Grants         map[string]frontierT7CollaborativeGrant `json:"grants"`
	GrantUsage     map[string]int                          `json:"grant_usage"`
	Revocations    map[string]frontierT7GrantRevocation    `json:"revocations"`
	ImportPlans    map[string]frontierT7ImportPlan         `json:"import_plans"`
	ImportReceipts map[string]frontierT7ImportReceipt      `json:"import_receipts"`
	Manifests      map[string]frontierT7ManifestRecord     `json:"manifests"`
	Replay         map[string]frontierT7ReplayRecord       `json:"replay"`
	UpdatedAt      string                                  `json:"updated_at"`
	StateHash      string                                  `json:"state_hash"`
}

type frontierT7PortableStore struct {
	mu              sync.RWMutex
	enabled         bool
	path            string
	dedicatedParent bool
	limits          frontierT7StoreLimits
	identity        *contextIdentityKeys
	state           frontierT7PortableState
	fileBytes       int64
	lastErrorCode   string
	unlock          func()
}

func frontierT7DefaultStoreLimits() frontierT7StoreLimits {
	return frontierT7StoreLimits{MaxBytes: frontierT7DefaultMaxBytes, MaxGrants: 256, MaxRevocations: 512, MaxPlans: 256, MaxReceipts: 1024, MaxManifests: 1024, MaxReplay: 1024}
}

func frontierT7NormalizeStoreLimits(value frontierT7StoreLimits) frontierT7StoreLimits {
	defaults := frontierT7DefaultStoreLimits()
	if value.MaxBytes <= 0 {
		value.MaxBytes = defaults.MaxBytes
	}
	if value.MaxGrants <= 0 {
		value.MaxGrants = defaults.MaxGrants
	}
	if value.MaxRevocations <= 0 {
		value.MaxRevocations = defaults.MaxRevocations
	}
	if value.MaxPlans <= 0 {
		value.MaxPlans = defaults.MaxPlans
	}
	if value.MaxReceipts <= 0 {
		value.MaxReceipts = defaults.MaxReceipts
	}
	if value.MaxManifests <= 0 {
		value.MaxManifests = defaults.MaxManifests
	}
	if value.MaxReplay <= 0 {
		value.MaxReplay = defaults.MaxReplay
	}
	value.MaxBytes = clampInt(value.MaxBytes, 64*1024, frontierT7MaximumMaxBytes)
	value.MaxGrants = clampInt(value.MaxGrants, 1, 4096)
	value.MaxRevocations = clampInt(value.MaxRevocations, 1, 8192)
	value.MaxPlans = clampInt(value.MaxPlans, 1, 4096)
	value.MaxReceipts = clampInt(value.MaxReceipts, 1, 16384)
	value.MaxManifests = clampInt(value.MaxManifests, 1, 16384)
	value.MaxReplay = clampInt(value.MaxReplay, 1, 16384)
	return value
}

func emptyFrontierT7PortableState() frontierT7PortableState {
	return frontierT7PortableState{
		SchemaID: frontierT7StateSchemaID, Version: 1,
		Grants: map[string]frontierT7CollaborativeGrant{}, GrantUsage: map[string]int{},
		Revocations: map[string]frontierT7GrantRevocation{}, ImportPlans: map[string]frontierT7ImportPlan{},
		ImportReceipts: map[string]frontierT7ImportReceipt{}, Manifests: map[string]frontierT7ManifestRecord{},
		Replay: map[string]frontierT7ReplayRecord{},
	}
}

func frontierT7StateHash(value frontierT7PortableState) string {
	value.StateHash = ""
	return frontierT7Digest(value)
}

func frontierT7PortableStatePath() string {
	if configured := strings.TrimSpace(os.Getenv(frontierT7StatePathEnv)); configured != "" {
		return filepath.Clean(configured)
	}
	if root := strings.TrimSpace(os.Getenv("GO_MEMORY_STORE_ROOT")); root != "" {
		return filepath.Join(root, "_contextlattice", "frontier_t7_portable_continuation.json")
	}
	return frontierT7DefaultStatePath
}

func newFrontierT7PortableStore(path string, limits frontierT7StoreLimits, identity *contextIdentityKeys) (*frontierT7PortableStore, error) {
	return newFrontierT7PortableStoreWithParent(path, limits, identity, false)
}

func newFrontierT7PortableStoreWithParent(path string, limits frontierT7StoreLimits, identity *contextIdentityKeys, dedicatedParent bool) (*frontierT7PortableStore, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" || identity == nil {
		return nil, errors.New("portable continuation state path and signing identity are required")
	}
	store := &frontierT7PortableStore{enabled: true, path: path, dedicatedParent: dedicatedParent, limits: frontierT7NormalizeStoreLimits(limits), identity: identity, state: emptyFrontierT7PortableState()}
	if err := prepareOwnerOnlyFile(store.path, dedicatedParent); err != nil {
		return nil, fmt.Errorf("prepare portable continuation state: %w", err)
	}
	unlock, err := lockOwnerOnlyMigration(store.path + ".lock")
	if err != nil {
		return nil, fmt.Errorf("lock portable continuation state: %w", err)
	}
	store.unlock = unlock
	if err := store.load(); err != nil {
		store.close()
		return nil, err
	}
	return store, nil
}

func newFrontierT7PortableStoreFromEnv(identity *contextIdentityKeys) (*frontierT7PortableStore, error) {
	if !envBool(frontierT7EnabledEnv, true) || identity == nil {
		return &frontierT7PortableStore{enabled: false, identity: identity, limits: frontierT7DefaultStoreLimits(), state: emptyFrontierT7PortableState()}, nil
	}
	limits := frontierT7StoreLimits{
		MaxBytes:       envInt("CONTEXTLATTICE_FRONTIER_T7_PORTABLE_CONTINUATION_MAX_BYTES", frontierT7DefaultMaxBytes),
		MaxGrants:      envInt("CONTEXTLATTICE_FRONTIER_T7_PORTABLE_CONTINUATION_MAX_GRANTS", 256),
		MaxRevocations: envInt("CONTEXTLATTICE_FRONTIER_T7_PORTABLE_CONTINUATION_MAX_REVOCATIONS", 512),
		MaxPlans:       envInt("CONTEXTLATTICE_FRONTIER_T7_PORTABLE_CONTINUATION_MAX_PLANS", 256),
		MaxReceipts:    envInt("CONTEXTLATTICE_FRONTIER_T7_PORTABLE_CONTINUATION_MAX_RECEIPTS", 1024),
		MaxManifests:   envInt("CONTEXTLATTICE_FRONTIER_T7_PORTABLE_CONTINUATION_MAX_MANIFESTS", 1024),
		MaxReplay:      envInt("CONTEXTLATTICE_FRONTIER_T7_PORTABLE_CONTINUATION_MAX_REPLAY", 1024),
	}
	return newFrontierT7PortableStoreWithParent(frontierT7PortableStatePath(), limits, identity, strings.TrimSpace(os.Getenv(frontierT7StatePathEnv)) == "")
}

func (s *frontierT7PortableStore) close() {
	if s != nil && s.unlock != nil {
		s.unlock()
		s.unlock = nil
	}
}

func (s *frontierT7PortableStore) load() error {
	info, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s.saveLocked(time.Now().UTC())
	}
	if err != nil {
		return fmt.Errorf("stat portable continuation state: %w", err)
	}
	if info.Size() == 0 {
		return s.saveLocked(time.Now().UTC())
	}
	if info.Size() > int64(s.limits.MaxBytes) {
		return errors.New("portable continuation state exceeds the bounded maximum")
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("read portable continuation state: %w", err)
	}
	state := emptyFrontierT7PortableState()
	if err := strictJSONDecode(raw, &state); err != nil {
		return fmt.Errorf("decode portable continuation state: %w", err)
	}
	if err := frontierT7ValidatePortableState(state, s.limits); err != nil {
		return err
	}
	s.state, s.fileBytes = state, info.Size()
	return nil
}

func frontierT7ValidatePortableState(state frontierT7PortableState, limits frontierT7StoreLimits) error {
	if state.SchemaID != frontierT7StateSchemaID || state.Version != 1 || state.Grants == nil || state.GrantUsage == nil || state.Revocations == nil || state.ImportPlans == nil || state.ImportReceipts == nil || state.Manifests == nil || state.Replay == nil {
		return errors.New("portable continuation state schema is invalid")
	}
	if len(state.Grants) > limits.MaxGrants || len(state.Revocations) > limits.MaxRevocations || len(state.ImportPlans) > limits.MaxPlans || len(state.ImportReceipts) > limits.MaxReceipts || len(state.Manifests) > limits.MaxManifests || len(state.Replay) > limits.MaxReplay {
		return errors.New("portable continuation state exceeds configured entry bounds")
	}
	for id, grant := range state.Grants {
		if id != grant.GrantID || len(frontierT7VerifyGrant(grant)) > 0 || state.GrantUsage[id] < 0 || state.GrantUsage[id] > grant.UsageLimit {
			return errors.New("portable continuation grant state is invalid")
		}
	}
	for id := range state.GrantUsage {
		if _, exists := state.Grants[id]; !exists {
			return errors.New("portable continuation usage references an unknown grant")
		}
	}
	for id, revocation := range state.Revocations {
		grant, exists := state.Grants[id]
		if !exists || id != revocation.GrantID || !frontierT7VerifyGrantRevocation(revocation) ||
			revocation.GrantDigest != grant.GrantDigest || revocation.Project != grant.Project ||
			revocation.SubjectSnapshotDigest != grant.Subject.SnapshotDigest || revocation.KeyEpoch != grant.KeyEpoch {
			return errors.New("portable continuation revocation state is invalid")
		}
	}
	for id, plan := range state.ImportPlans {
		if id != plan.PlanID || frontierT7ValidateImportPlan(plan) != nil {
			return errors.New("portable continuation import plan state is invalid")
		}
	}
	for key, receipt := range state.ImportReceipts {
		plan, exists := state.ImportPlans[receipt.PlanID]
		if !exists || key != frontierT7ReceiptKey(receipt.PlanID, receipt.BatchIndex) || !frontierT7ValidImportReceipt(receipt, plan, receipt.BatchIndex) {
			return errors.New("portable continuation import receipt state is invalid")
		}
	}
	for key, record := range state.Manifests {
		if record.ManifestID == "" || !frontierT7ValidDigest(record.ManifestDigest) || record.Project == "" || record.RecipientKeyID == "" ||
			(record.Direction != "created" && record.Direction != "reconciled") || key != record.Direction+":"+record.ManifestID ||
			mustParseFrontierT7Time(record.CreatedAt).IsZero() || mustParseFrontierT7Time(record.ExpiresAt).IsZero() {
			return errors.New("portable continuation manifest state is invalid")
		}
	}
	for id, record := range state.Replay {
		if id == "" || !frontierT7ValidDigest(record.ManifestDigest) || mustParseFrontierT7Time(record.RecordedAt).IsZero() || mustParseFrontierT7Time(record.ExpiresAt).IsZero() {
			return errors.New("portable continuation replay state is invalid")
		}
	}
	if frontierT7StateHash(state) != state.StateHash {
		return errors.New("portable continuation state integrity is invalid")
	}
	return nil
}

func (s *frontierT7PortableStore) saveLocked(now time.Time) error {
	s.trimExpiredLocked(now)
	s.state.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
	s.state.StateHash = frontierT7StateHash(s.state)
	raw, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	if len(raw)+1 > s.limits.MaxBytes {
		return errors.New("portable continuation state exceeds the bounded maximum")
	}
	content := append(raw, '\n')
	if err := writeOwnerOnlyDurableAtomicFile(s.path, content, s.dedicatedParent); err != nil {
		return err
	}
	s.fileBytes, s.lastErrorCode = int64(len(content)), ""
	return nil
}

func (s *frontierT7PortableStore) trimExpiredLocked(now time.Time) {
	for id, record := range s.state.Manifests {
		if expires := mustParseFrontierT7Time(record.ExpiresAt); !expires.IsZero() && !now.Before(expires) {
			delete(s.state.Manifests, id)
		}
	}
	for id, record := range s.state.Replay {
		if expires := mustParseFrontierT7Time(record.ExpiresAt); !expires.IsZero() && !now.Before(expires) {
			delete(s.state.Replay, id)
		}
	}
}

func (s *frontierT7PortableStore) mutate(now time.Time, apply func() error) error {
	return s.mutateMaybe(now, func() (bool, error) {
		if err := apply(); err != nil {
			return false, err
		}
		return true, nil
	})
}

func (s *frontierT7PortableStore) mutateMaybe(now time.Time, apply func() (bool, error)) error {
	if s == nil || !s.enabled {
		return errors.New("portable continuation store is disabled")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	before, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	changed, err := apply()
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if err := s.saveLocked(now); err != nil {
		if ownerOnlyAtomicWriteCommitted(err) {
			s.enabled = false
			s.lastErrorCode = "commit_unknown"
			return fmt.Errorf("portable continuation commit outcome is unknown: %w", err)
		}
		_ = json.Unmarshal(before, &s.state)
		s.lastErrorCode = "storage_unavailable"
		return err
	}
	return nil
}

func frontierT7RevokedSet(values map[string]frontierT7GrantRevocation) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for id := range values {
		out[id] = struct{}{}
	}
	return out
}

func (s *frontierT7PortableStore) getGrant(grantID string) (frontierT7CollaborativeGrant, bool) {
	if s == nil || !s.enabled {
		return frontierT7CollaborativeGrant{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	grant, exists := s.state.Grants[strings.TrimSpace(grantID)]
	return grant, exists
}

func (s *frontierT7PortableStore) createGrant(request frontierT7GrantCreateRequest, now time.Time) (frontierT7CollaborativeGrant, error) {
	var created frontierT7CollaborativeGrant
	err := s.mutate(now, func() error {
		if len(s.state.Grants) >= s.limits.MaxGrants {
			return errors.New("portable continuation grant capacity reached")
		}
		if request.Parent != nil {
			parent, exists := s.state.Grants[request.Parent.GrantID]
			if !exists || parent.GrantDigest != request.Parent.GrantDigest {
				return errors.New("parent grant is not present in the local signed store")
			}
			if _, revoked := s.state.Revocations[parent.GrantID]; revoked {
				return errors.New("parent grant is revoked")
			}
			request.Parent = &parent
		}
		grant, err := frontierT7CreateCollaborativeGrant(s.identity, request, now)
		if err != nil {
			return err
		}
		s.state.Grants[grant.GrantID] = grant
		s.state.GrantUsage[grant.GrantID] = 0
		created = grant
		return nil
	})
	return created, err
}

func (s *frontierT7PortableStore) authorize(grantID string, request frontierT7GrantUseRequest, consume bool, now time.Time) (frontierT7GrantDecision, error) {
	var decision frontierT7GrantDecision
	apply := func() error {
		grant, exists := s.state.Grants[grantID]
		if !exists {
			return errors.New("grant not found")
		}
		request.UsageCount, request.RevokedGrantIDs, request.Now = s.state.GrantUsage[grantID], frontierT7RevokedSet(s.state.Revocations), now
		decision = frontierT7AuthorizeGrant(grant, request)
		if consume && decision.Allowed {
			s.state.GrantUsage[grantID]++
			decision.RemainingUses = maxInt(0, grant.UsageLimit-s.state.GrantUsage[grantID])
		}
		return nil
	}
	if consume {
		return decision, s.mutateMaybe(now, func() (bool, error) {
			if err := apply(); err != nil {
				return false, err
			}
			return decision.Allowed, nil
		})
	}
	if s == nil || !s.enabled {
		return decision, errors.New("portable continuation store is disabled")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	err := apply()
	return decision, err
}

func (s *frontierT7PortableStore) revokeGrant(grantID, reason string, now time.Time) (frontierT7GrantRevocation, error) {
	var revocation frontierT7GrantRevocation
	err := s.mutate(now, func() error {
		if existing, ok := s.state.Revocations[grantID]; ok {
			revocation = existing
			return nil
		}
		if len(s.state.Revocations) >= s.limits.MaxRevocations {
			return errors.New("portable continuation revocation capacity reached")
		}
		grant, exists := s.state.Grants[grantID]
		if !exists {
			return errors.New("grant not found")
		}
		if _, err := frontierT7SafeID(reason, "reason", 500); err != nil {
			return err
		}
		issuer := contextPassportIssuer{InstanceID: s.identity.InstanceID, SigningKeyID: s.identity.SigningKeyID, SigningPublicKey: s.identity.SigningPublicKey}
		reasonDigest := frontierT7Digest(map[string]any{"reason": reason})
		revokedAt := now.UTC().Format(time.RFC3339Nano)
		revocation = frontierT7GrantRevocation{
			SchemaID: frontierT7GrantRevocationSchemaID, Version: 1,
			RevocationID: "ctxrevoke_" + strings.TrimPrefix(frontierT7Digest(map[string]any{"grant": grant.GrantDigest, "reason": reasonDigest, "revoked_at": revokedAt}), "sha256:")[:24],
			GrantID:      grant.GrantID, GrantDigest: grant.GrantDigest, Project: grant.Project,
			SubjectSnapshotDigest: grant.Subject.SnapshotDigest, KeyEpoch: grant.KeyEpoch,
			ReasonDigest: reasonDigest, RevokedAt: revokedAt, Issuer: issuer, TombstoneOnly: true,
			TransportOwnedByContextLattice: false, TransportExecuted: false, PrivateKeyExported: false,
			OrdinaryMemoryMutated: false, NetworkCalls: 0,
		}
		unsigned := frontierT7GrantRevocationUnsignedValue(revocation)
		revocation.RevocationDigest = frontierT7Digest(unsigned)
		signature, err := signBytesWithIdentity(struct {
			RevocationDigest string                            `json:"revocation_digest"`
			Revocation       frontierT7GrantRevocationUnsigned `json:"revocation"`
		}{RevocationDigest: revocation.RevocationDigest, Revocation: unsigned}, s.identity)
		if err != nil {
			return err
		}
		revocation.Signature = signature
		s.state.Revocations[grantID] = revocation
		return nil
	})
	return revocation, err
}

func (s *frontierT7PortableStore) buildImportPlan(project string, records []frontierT7ImportRecord, batchSize int, now time.Time) (frontierT7ImportPlan, error) {
	var plan frontierT7ImportPlan
	err := s.mutateMaybe(now, func() (bool, error) {
		existing := map[string]string{}
		for _, receipt := range s.state.ImportReceipts {
			for _, mapping := range receipt.Mappings {
				existing[mapping.SourceKey] = mapping.ContentDigest
			}
		}
		created, err := frontierT7BuildImportPlan(project, records, existing, batchSize)
		if err != nil {
			return false, err
		}
		if prior, ok := s.state.ImportPlans[created.PlanID]; ok {
			plan = prior
			return false, nil
		}
		if len(s.state.ImportPlans) >= s.limits.MaxPlans {
			return false, errors.New("portable continuation import plan capacity reached")
		}
		s.state.ImportPlans[created.PlanID], plan = created, created
		return true, nil
	})
	return plan, err
}

func frontierT7ReceiptKey(planID string, batchIndex int) string {
	return fmt.Sprintf("%s:%08d", planID, batchIndex)
}

func (s *frontierT7PortableStore) commitImport(planID string, batchIndex int, executionDigest string, now time.Time) (frontierT7ImportReceipt, error) {
	var receipt frontierT7ImportReceipt
	err := s.mutateMaybe(now, func() (bool, error) {
		plan, exists := s.state.ImportPlans[planID]
		if !exists {
			return false, errors.New("import plan not found")
		}
		prior := map[int]frontierT7ImportReceipt{}
		for _, item := range s.state.ImportReceipts {
			if item.PlanID == planID {
				prior[item.BatchIndex] = item
			}
		}
		created, err := frontierT7CommitImportBatch(plan, batchIndex, prior, executionDigest, now)
		if err != nil {
			return false, err
		}
		key := frontierT7ReceiptKey(planID, batchIndex)
		if _, exists := s.state.ImportReceipts[key]; exists {
			receipt = created
			return false, nil
		}
		if len(s.state.ImportReceipts) >= s.limits.MaxReceipts {
			return false, errors.New("portable continuation import receipt capacity reached")
		}
		s.state.ImportReceipts[key], receipt = created, created
		return true, nil
	})
	return receipt, err
}

func (s *frontierT7PortableStore) createManifest(request frontierT7ContinuationRequest, now time.Time) (frontierT7ContinuationManifest, error) {
	manifest, err := s.prepareManifest(request, now)
	if err != nil {
		return frontierT7ContinuationManifest{}, err
	}
	if err := s.recordCreatedManifest(manifest, now); err != nil {
		return frontierT7ContinuationManifest{}, err
	}
	return manifest, nil
}

func (s *frontierT7PortableStore) prepareManifest(request frontierT7ContinuationRequest, now time.Time) (frontierT7ContinuationManifest, error) {
	if s == nil || !s.enabled {
		return frontierT7ContinuationManifest{}, errors.New("portable continuation store is disabled")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	grant, exists := s.state.Grants[request.Grant.GrantID]
	if !exists || grant.GrantDigest != request.Grant.GrantDigest {
		return frontierT7ContinuationManifest{}, errors.New("continuation grant not found")
	}
	if _, revoked := s.state.Revocations[grant.GrantID]; revoked {
		return frontierT7ContinuationManifest{}, errors.New("continuation grant is revoked")
	}
	request.Grant = grant
	return frontierT7CreateContinuationManifest(s.identity, request, now)
}

func (s *frontierT7PortableStore) recordCreatedManifest(manifest frontierT7ContinuationManifest, now time.Time) error {
	return s.mutateMaybe(now, func() (bool, error) {
		grant, exists := s.state.Grants[manifest.GrantID]
		if !exists || grant.GrantDigest != manifest.GrantDigest {
			return false, errors.New("continuation grant not found")
		}
		if _, revoked := s.state.Revocations[grant.GrantID]; revoked {
			return false, errors.New("continuation grant is revoked")
		}
		if err := frontierT7ValidateContinuationArtifacts(manifest, grant, now); err != nil {
			return false, err
		}
		key := "created:" + manifest.ManifestID
		if existing, exists := s.state.Manifests[key]; exists {
			if existing.ManifestDigest != manifest.ManifestDigest {
				return false, errors.New("continuation manifest id collision")
			}
			return false, nil
		}
		if len(s.state.Manifests) >= s.limits.MaxManifests {
			return false, errors.New("portable continuation manifest capacity reached")
		}
		s.state.Manifests[key] = frontierT7ManifestRecord{ManifestID: manifest.ManifestID, ManifestDigest: manifest.ManifestDigest, Project: manifest.Project, RecipientKeyID: manifest.RecipientKeyID, Direction: "created", CreatedAt: manifest.CreatedAt, ExpiresAt: manifest.ExpiresAt}
		return true, nil
	})
}

type frontierT7LockedReplayGuard struct {
	replay    map[string]frontierT7ReplayRecord
	now       time.Time
	expiresAt string
}

func (g *frontierT7LockedReplayGuard) checkAndRecord(manifestID, manifestDigest string) (string, bool, error) {
	if g == nil || g.replay == nil {
		return "", false, errors.New("replay guard unavailable")
	}
	prior, exists := g.replay[manifestID]
	if !exists {
		g.replay[manifestID] = frontierT7ReplayRecord{ManifestDigest: manifestDigest, RecordedAt: g.now.UTC().Format(time.RFC3339Nano), ExpiresAt: g.expiresAt}
	}
	return prior.ManifestDigest, exists, nil
}

func frontierT7CloneReplay(values map[string]frontierT7ReplayRecord) map[string]frontierT7ReplayRecord {
	cloned := make(map[string]frontierT7ReplayRecord, len(values)+1)
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func (s *frontierT7PortableStore) reconcileManifest(manifest frontierT7ContinuationManifest, grant frontierT7CollaborativeGrant, expectedLineage string, authorization frontierT7ContinuationAuthorization, now time.Time) (frontierT7ContinuationReconciliation, error) {
	var result frontierT7ContinuationReconciliation
	err := s.mutateMaybe(now, func() (bool, error) {
		if existing, exists := s.state.Grants[grant.GrantID]; exists {
			if existing.GrantDigest != grant.GrantDigest {
				return false, errors.New("continuation grant id collision")
			}
		}
		authorization.UsageCount = s.state.GrantUsage[grant.GrantID]
		authorization.RevokedGrantIDs = frontierT7RevokedSet(s.state.Revocations)
		stagedReplay := frontierT7CloneReplay(s.state.Replay)
		guard := &frontierT7LockedReplayGuard{replay: stagedReplay, now: now, expiresAt: manifest.ExpiresAt}
		result = frontierT7ReconcileContinuation(manifest, grant, now, s.identity.MeshKeyID, expectedLineage, authorization, guard)
		if !result.Accepted {
			return false, nil
		}
		if _, exists := s.state.Grants[grant.GrantID]; !exists && len(s.state.Grants) >= s.limits.MaxGrants {
			return false, errors.New("portable continuation grant capacity reached")
		}
		if _, exists := s.state.Replay[manifest.ManifestID]; !exists && len(s.state.Replay) >= s.limits.MaxReplay {
			return false, errors.New("portable continuation replay capacity reached")
		}
		key := "reconciled:" + manifest.ManifestID
		if _, exists := s.state.Manifests[key]; !exists && len(s.state.Manifests) >= s.limits.MaxManifests {
			return false, errors.New("portable continuation manifest capacity reached")
		}
		if _, exists := s.state.Grants[grant.GrantID]; !exists {
			s.state.Grants[grant.GrantID] = grant
			s.state.GrantUsage[grant.GrantID] = 0
		}
		s.state.Replay = stagedReplay
		s.state.GrantUsage[grant.GrantID]++
		s.state.Manifests[key] = frontierT7ManifestRecord{ManifestID: manifest.ManifestID, ManifestDigest: manifest.ManifestDigest, Project: manifest.Project, RecipientKeyID: manifest.RecipientKeyID, Direction: "reconciled", CreatedAt: manifest.CreatedAt, ExpiresAt: manifest.ExpiresAt}
		return true, nil
	})
	return result, err
}

func (s *frontierT7PortableStore) snapshot(now time.Time) map[string]any {
	if s == nil {
		return map[string]any{"ok": false, "schema_id": frontierT7StatusSchemaID, "enabled": false}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	active, exhausted := 0, 0
	reconciliations := 0
	for id, grant := range s.state.Grants {
		if _, revoked := s.state.Revocations[id]; revoked {
			continue
		}
		if expiry := mustParseFrontierT7Time(grant.ExpiresAt); !expiry.IsZero() && now.Before(expiry) {
			if s.state.GrantUsage[id] >= grant.UsageLimit {
				exhausted++
			} else {
				active++
			}
		}
	}
	for _, manifest := range s.state.Manifests {
		if manifest.Direction == "reconciled" {
			reconciliations++
		}
	}
	return map[string]any{
		"ok": s.enabled, "schema_id": frontierT7StatusSchemaID, "version": 1, "enabled": s.enabled,
		"grants": len(s.state.Grants), "revocations": len(s.state.Revocations),
		"import_plans": len(s.state.ImportPlans), "import_receipts": len(s.state.ImportReceipts),
		"manifests": len(s.state.Manifests), "reconciliations": reconciliations,
		"updated_at": s.state.UpdatedAt, "state_hash": s.state.StateHash,
		"limits":        map[string]any{"max_bytes": s.limits.MaxBytes, "max_grants": s.limits.MaxGrants, "max_revocations": s.limits.MaxRevocations, "max_plans": s.limits.MaxPlans, "max_receipts": s.limits.MaxReceipts, "max_manifests": s.limits.MaxManifests, "max_replay": s.limits.MaxReplay},
		"ownership":     map[string]any{"import_execution_owner": "external_import_worker", "transport_owned_by_contextlattice": false},
		"data_handling": map[string]any{"bounded": true, "redacted": true, "content_references_digest_only": true, "raw_payloads_included": false},
		"safety":        map[string]any{"gateway_execution_performed": false, "model_execution_performed": false, "subprocess_execution_performed": false, "ordinary_memory_mutated": false, "private_key_exported": false},
		"grant_status":  map[string]any{"active": active, "exhausted": exhausted},
		"storage":       map[string]any{"file_bytes": s.fileBytes, "last_error_code": s.lastErrorCode, "replay_records": len(s.state.Replay)},
		"network_calls": 0,
	}
}
