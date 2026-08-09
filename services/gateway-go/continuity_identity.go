package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"
)

const (
	continuityLedgerEntrySchemaID                 = "continuity_ledger_entry.v1"
	taskIdentityReconciliationContractID          = "task_identity_reconciliation.v1"
	taskIdentityReceiptContractID                 = "task_identity_receipt.v1"
	objectiveTransitionContractID                 = "objective_transition.v1"
	objectiveGraphContractID                      = "objective_graph.v1"
	decisionChangeContractID                      = "decision_change.v1"
	decisionChangeQueryContractID                 = "decision_change_query.v1"
	continuityDecisionBundleSchemaID              = "continuity_decision_bundle.v1"
	continuityLedgerKindTaskIdentity              = "task_identity_receipt"
	continuityLedgerKindObjectiveTransition       = "objective_transition"
	continuityLedgerKindDecisionChange            = "decision_change"
	continuityLedgerKindDecisionBundle            = "decision_change_bundle"
	defaultContinuityLedgerPathRel                = ".data/orchestrator/continuity_ledger.ndjson"
	defaultContinuityLedgerMaxBytes         int64 = 64 * 1024 * 1024
	defaultContinuityLedgerMaxEntries             = 100000
	continuityLedgerMaxLineBytes                  = 512 * 1024
	continuitySemanticThreshold                   = 0.72
	continuitySemanticMargin                      = 0.12
)

var continuityIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,160}$`)
var errContinuityIdentityScope = errors.New("task_identity_id cannot cross project or repo scope")
var errContinuityIdentityMissing = errors.New("task_identity_id does not exist")
var errContinuityUnavailable = errors.New("continuity identity validation is unavailable")
var errContinuityLedgerLocked = errors.New("continuity ledger already has an active writer")
var errContinuitySelectionStale = errors.New("continuity selection anchor is stale")

type continuityCommitUnknownError struct {
	Operation string
	Err       error
}

func (e *continuityCommitUnknownError) Error() string {
	if e == nil {
		return "continuity ledger commit outcome is unknown"
	}
	return fmt.Sprintf("%s: %v; continuity ledger commit outcome is unknown and writes are disabled until restart and verification", e.Operation, e.Err)
}

func (e *continuityCommitUnknownError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func continuityCommitUnknown(err error) bool {
	var commitErr *continuityCommitUnknownError
	return errors.As(err, &commitErr)
}

type continuityLedgerEntry struct {
	SchemaID     string         `json:"schema_id"`
	Sequence     uint64         `json:"sequence"`
	EntryID      string         `json:"entry_id"`
	Kind         string         `json:"kind"`
	RecordedAt   string         `json:"recorded_at"`
	PreviousHash string         `json:"previous_hash"`
	EntryHash    string         `json:"entry_hash"`
	Payload      map[string]any `json:"payload"`
}

type continuityLedgerAppend struct {
	kind    string
	payload any
}

type continuityDecisionBundle struct {
	SchemaID            string              `json:"schema_id"`
	DecisionChange      decisionChange      `json:"decision_change"`
	ObjectiveTransition objectiveTransition `json:"objective_transition"`
}

type taskIdentityReceipt struct {
	SchemaID              string   `json:"schema_id"`
	ReceiptID             string   `json:"receipt_id"`
	Operation             string   `json:"operation"`
	TaskIdentityID        string   `json:"task_identity_id"`
	SourceTaskIdentityIDs []string `json:"source_task_identity_ids"`
	ParentTaskIdentityID  string   `json:"parent_task_identity_id,omitempty"`
	Project               string   `json:"project"`
	Repo                  string   `json:"repo,omitempty"`
	Objective             string   `json:"objective"`
	NormalizedObjective   string   `json:"normalized_objective"`
	ExternalTaskIDs       []string `json:"external_task_ids"`
	ExecutionLaneID       string   `json:"execution_lane_id,omitempty"`
	SessionID             string   `json:"session_id,omitempty"`
	Actor                 string   `json:"actor"`
	Reason                string   `json:"reason"`
	MatchMode             string   `json:"match_mode"`
	WorkspaceID           string   `json:"workspace_id,omitempty"`
	FeatureID             string   `json:"feature_id,omitempty"`
	PolicyID              string   `json:"policy_id,omitempty"`
	TopScore              float64  `json:"top_score,omitempty"`
	ScoreMargin           float64  `json:"score_margin,omitempty"`
	IdempotencyKey        string   `json:"idempotency_key"`
	CreatedAt             string   `json:"created_at"`
}

type taskIdentityRecord struct {
	TaskIdentityID       string   `json:"task_identity_id"`
	Project              string   `json:"project"`
	Repo                 string   `json:"repo,omitempty"`
	Objective            string   `json:"objective"`
	NormalizedObjective  string   `json:"normalized_objective"`
	ExternalTaskIDs      []string `json:"external_task_ids"`
	ParentTaskIdentityID string   `json:"parent_task_identity_id,omitempty"`
	Status               string   `json:"status"`
	MergedInto           string   `json:"merged_into,omitempty"`
	CreatedAt            string   `json:"created_at"`
	UpdatedAt            string   `json:"updated_at"`
	semanticTokens       []string
}

func taskIdentityReceiptOutput(receipt taskIdentityReceipt) map[string]any {
	receipt.SourceTaskIdentityIDs = append([]string{}, receipt.SourceTaskIdentityIDs...)
	receipt.ExternalTaskIDs = append([]string{}, receipt.ExternalTaskIDs...)
	payload, err := normalizeAgentContractJSONObject(receipt)
	if err != nil || payload == nil {
		return map[string]any{}
	}
	return attachPayloadFormatContract(taskIdentityReceiptContractID, payload, receipt.Actor, "task_identity_receipt", "task_identity_receipt")
}

type taskIdentityCandidate struct {
	Identity taskIdentityRecord `json:"identity"`
	Score    float64            `json:"score"`
}

func continuityCandidateBetter(left taskIdentityCandidate, right taskIdentityCandidate) bool {
	if left.Score != right.Score {
		return left.Score > right.Score
	}
	return left.Identity.TaskIdentityID < right.Identity.TaskIdentityID
}

func insertContinuityCandidate(candidates []taskIdentityCandidate, candidate taskIdentityCandidate, limit int) []taskIdentityCandidate {
	position := 0
	for position < len(candidates) && !continuityCandidateBetter(candidate, candidates[position]) {
		position++
	}
	if position >= limit {
		return candidates
	}
	if len(candidates) < limit {
		candidates = append(candidates, taskIdentityCandidate{})
	}
	copy(candidates[position+1:], candidates[position:len(candidates)-1])
	candidates[position] = candidate
	return candidates
}

type continuityStore struct {
	mu                       sync.RWMutex
	enabled                  bool
	path                     string
	maxBytes                 int64
	maxEntries               int
	fsync                    bool
	unlock                   func()
	fileBytes                int64
	entries                  []continuityLedgerEntry
	proofRevision            uint64
	lastHash                 string
	proofIdentityIndex       map[string][]int
	taskIdentities           map[string]taskIdentityRecord
	taskAliases              map[string]string
	taskObjectiveIndex       map[string]map[string]struct{}
	taskOperationIdempotency map[string]taskIdentityReceipt
	objectiveTransitions     []objectiveTransition
	objectiveTransitionIDs   map[string]struct{}
	objectiveTransitionIndex map[string][]int
	objectiveProjectIndex    map[string][]int
	objectiveRelationIndex   map[string][]objectiveGraphRelationRef
	objectiveIdempotency     map[string]int
	decisionChanges          []decisionChange
	decisionChangeIDs        map[string]struct{}
	decisionChangeIndex      map[string]int
	decisionProjectIndex     map[string][]int
	decisionIdempotency      map[string]int
	decisionTransitionIndex  map[string]int
	lastPersistedAt          string
	lastCompactedAt          string
	compactionCount          int
	tailRecoveryCount        int
	tailRecoveryBytes        int64
	lastError                string
	afterPersistHook         func() error
	beforeGovernedMutation   func(string)
}

func continuityLedgerPath() string {
	return resolveStoragePath("CONTEXTLATTICE_CONTINUITY_LEDGER_PATH", defaultContinuityLedgerPathRel)
}

func continuityLedgerMaxBytes() int64 {
	raw := int64(envInt("CONTEXTLATTICE_CONTINUITY_LEDGER_MAX_BYTES", int(defaultContinuityLedgerMaxBytes)))
	if raw < 1024*1024 {
		return 1024 * 1024
	}
	if raw > 1024*1024*1024 {
		return 1024 * 1024 * 1024
	}
	return raw
}

func newContinuityStoreFromEnv() (*continuityStore, error) {
	store := &continuityStore{
		enabled:                  envBool("CONTEXTLATTICE_CONTINUITY_ENABLED", true),
		path:                     continuityLedgerPath(),
		maxBytes:                 continuityLedgerMaxBytes(),
		maxEntries:               clampInt(envInt("CONTEXTLATTICE_CONTINUITY_LEDGER_MAX_ENTRIES", defaultContinuityLedgerMaxEntries), 128, 1000000),
		fsync:                    envBool("CONTEXTLATTICE_CONTINUITY_LEDGER_FSYNC", true),
		entries:                  []continuityLedgerEntry{},
		proofIdentityIndex:       map[string][]int{},
		taskIdentities:           map[string]taskIdentityRecord{},
		taskAliases:              map[string]string{},
		taskObjectiveIndex:       map[string]map[string]struct{}{},
		taskOperationIdempotency: map[string]taskIdentityReceipt{},
		objectiveTransitionIDs:   map[string]struct{}{},
		objectiveTransitionIndex: map[string][]int{},
		objectiveProjectIndex:    map[string][]int{},
		objectiveRelationIndex:   map[string][]objectiveGraphRelationRef{},
		objectiveIdempotency:     map[string]int{},
		decisionChangeIDs:        map[string]struct{}{},
		decisionChangeIndex:      map[string]int{},
		decisionProjectIndex:     map[string][]int{},
		decisionIdempotency:      map[string]int{},
		decisionTransitionIndex:  map[string]int{},
	}
	if !store.enabled || strings.TrimSpace(store.path) == "" {
		store.enabled = false
		return store, nil
	}
	dedicatedParent := strings.TrimSpace(os.Getenv("CONTEXTLATTICE_CONTINUITY_LEDGER_PATH")) == ""
	if err := prepareOwnerOnlyFile(store.path, dedicatedParent); err != nil {
		store.lastError = err.Error()
		store.enabled = false
		return store, fmt.Errorf("prepare continuity ledger: %w", err)
	}
	unlock, err := lockOwnerOnlyMigration(store.path + ".lock")
	if err != nil {
		store.lastError = err.Error()
		store.enabled = false
		if errors.Is(err, errOwnerOnlyMigrationLocked) {
			return store, errContinuityLedgerLocked
		}
		return store, fmt.Errorf("lock continuity ledger: %w", err)
	}
	store.unlock = unlock
	if err := store.load(); err != nil {
		store.lastError = err.Error()
		store.enabled = false
		store.close()
		return store, err
	}
	return store, nil
}

func (s *continuityStore) close() {
	if s == nil || s.unlock == nil {
		return
	}
	s.unlock()
	s.unlock = nil
}

func continuityLedgerDedicatedParent() bool {
	return strings.TrimSpace(os.Getenv("CONTEXTLATTICE_CONTINUITY_LEDGER_PATH")) == ""
}

func (s *continuityStore) writeCanonicalLedgerBytes(content []byte) error {
	if int64(len(content)) > s.maxBytes {
		return errors.New("canonical continuity ledger exceeds max bytes")
	}
	if s.fsync {
		return writeOwnerOnlyDurableAtomicFile(s.path, content, continuityLedgerDedicatedParent())
	}
	return writeOwnerOnlyAtomicFile(s.path, content, continuityLedgerDedicatedParent())
}

func (s *continuityStore) ensureIndexesLocked() {
	if s.proofIdentityIndex == nil {
		s.proofIdentityIndex = map[string][]int{}
	}
	if s.taskIdentities == nil {
		s.taskIdentities = map[string]taskIdentityRecord{}
	}
	if s.taskAliases == nil {
		s.taskAliases = map[string]string{}
	}
	if s.taskObjectiveIndex == nil {
		s.taskObjectiveIndex = map[string]map[string]struct{}{}
	}
	if s.taskOperationIdempotency == nil {
		s.taskOperationIdempotency = map[string]taskIdentityReceipt{}
	}
	if s.objectiveTransitionIDs == nil {
		s.objectiveTransitionIDs = map[string]struct{}{}
	}
	if s.objectiveTransitionIndex == nil {
		s.objectiveTransitionIndex = map[string][]int{}
	}
	if s.objectiveProjectIndex == nil {
		s.objectiveProjectIndex = map[string][]int{}
	}
	if s.objectiveRelationIndex == nil {
		s.objectiveRelationIndex = map[string][]objectiveGraphRelationRef{}
	}
	if s.objectiveIdempotency == nil {
		s.objectiveIdempotency = map[string]int{}
	}
	if s.decisionChangeIDs == nil {
		s.decisionChangeIDs = map[string]struct{}{}
	}
	if s.decisionChangeIndex == nil {
		s.decisionChangeIndex = map[string]int{}
	}
	if s.decisionProjectIndex == nil {
		s.decisionProjectIndex = map[string][]int{}
	}
	if s.decisionIdempotency == nil {
		s.decisionIdempotency = map[string]int{}
	}
	if s.decisionTransitionIndex == nil {
		s.decisionTransitionIndex = map[string]int{}
	}
}

func (s *continuityStore) load() error {
	if s == nil {
		return errors.New("continuity store unavailable")
	}
	info, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat continuity ledger: %w", err)
	}
	if info.Size() > s.maxBytes {
		return fmt.Errorf("continuity ledger exceeds max bytes: %d > %d", info.Size(), s.maxBytes)
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("read continuity ledger: %w", err)
	}
	s.ensureIndexesLocked()
	originalBytes := len(raw)
	canonicalRewrite := false
	var expectedSequence uint64 = 1
	previousHash := ""
	for cursor := 0; cursor < len(raw); {
		relativeNewline := bytes.IndexByte(raw[cursor:], '\n')
		end := len(raw)
		hasNewline := false
		if relativeNewline >= 0 {
			end = cursor + relativeNewline
			hasNewline = true
		}
		line := bytes.TrimSpace(raw[cursor:end])
		next := end
		if hasNewline {
			next++
		}
		if len(line) == 0 {
			if !hasNewline {
				s.tailRecoveryCount++
				s.tailRecoveryBytes += int64(len(raw) - cursor)
				raw = raw[:cursor]
				canonicalRewrite = true
				break
			}
			return fmt.Errorf("continuity ledger contains an empty committed line at sequence %d", expectedSequence)
		}
		if len(line) > continuityLedgerMaxLineBytes {
			return fmt.Errorf("continuity ledger line exceeds max bytes at sequence %d", expectedSequence)
		}
		if len(s.entries) >= s.maxEntries {
			return errors.New("continuity ledger exceeds max entries")
		}
		var entry continuityLedgerEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			if !hasNewline {
				s.tailRecoveryCount++
				s.tailRecoveryBytes += int64(len(raw) - cursor)
				raw = raw[:cursor]
				canonicalRewrite = true
				break
			}
			return fmt.Errorf("decode continuity ledger sequence %d: %w", expectedSequence, err)
		}
		if entry.SchemaID != continuityLedgerEntrySchemaID || entry.Sequence != expectedSequence {
			return fmt.Errorf("continuity ledger sequence or schema mismatch at %d", expectedSequence)
		}
		if entry.PreviousHash != previousHash {
			return fmt.Errorf("continuity ledger hash chain mismatch at %d", expectedSequence)
		}
		hash, err := continuityEntryHash(entry)
		if err != nil || entry.EntryHash != hash {
			return fmt.Errorf("continuity ledger entry hash mismatch at %d", expectedSequence)
		}
		if err := s.applyEntryLocked(entry); err != nil {
			return fmt.Errorf("apply continuity ledger sequence %d: %w", expectedSequence, err)
		}
		s.entries = append(s.entries, entry)
		s.proofRevision = nextProofTimelineRevision(s.proofRevision)
		s.indexProofTimelineEntryLocked(entry, len(s.entries)-1)
		previousHash = entry.EntryHash
		expectedSequence++
		cursor = next
	}
	if !canonicalRewrite && len(raw) > 0 && raw[len(raw)-1] != '\n' {
		if int64(len(raw)+1) > s.maxBytes {
			return errors.New("continuity ledger final newline would exceed max bytes")
		}
		raw = append(raw, '\n')
		canonicalRewrite = true
		s.tailRecoveryCount++
	}
	if canonicalRewrite {
		if err := s.writeCanonicalLedgerBytes(raw); err != nil {
			return fmt.Errorf("repair continuity ledger tail: %w", err)
		}
	}
	s.fileBytes = int64(len(raw))
	s.lastHash = previousHash
	if len(s.entries) > 0 {
		s.lastPersistedAt = s.entries[len(s.entries)-1].RecordedAt
	}
	if originalBytes > 0 && len(raw) == 0 {
		s.lastPersistedAt = ""
	}
	return nil
}

func continuityEntryHash(entry continuityLedgerEntry) (string, error) {
	entry.EntryHash = ""
	raw, err := json.Marshal(entry)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func continuityPayloadMap(value any) (map[string]any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(raw) > continuityLedgerMaxLineBytes/2 {
		return nil, errors.New("continuity payload exceeds bounded entry size")
	}
	payload := map[string]any{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func continuityLedgerEntryProject(entry continuityLedgerEntry) string {
	if project := strings.TrimSpace(anyToString(entry.Payload["project"])); project != "" {
		return project
	}
	for _, field := range []string{"decision_change", "objective_transition"} {
		if project := strings.TrimSpace(anyToString(anyMap(entry.Payload[field])["project"])); project != "" {
			return project
		}
	}
	return ""
}

func (s *continuityStore) projectCoreAnchorLocked(project string) (uint64, string) {
	for index := len(s.entries) - 1; index >= 0; index-- {
		entry := s.entries[index]
		if strings.EqualFold(continuityLedgerEntryProject(entry), project) {
			return entry.Sequence, entry.EntryHash
		}
	}
	return 0, ""
}

func (s *continuityStore) projectCoreAnchor(project string) (uint64, string) {
	if s == nil {
		return 0, ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.projectCoreAnchorLocked(project)
}

func objectiveTransitionEquivalent(left objectiveTransition, right objectiveTransition) bool {
	if !right.occurredAtExplicit {
		right.OccurredAt = left.OccurredAt
	}
	left.RecordedAt, right.RecordedAt = "", ""
	left.ledgerSequence, right.ledgerSequence = 0, 0
	left.idempotentReplay, right.idempotentReplay = false, false
	left.occurredAtExplicit, right.occurredAtExplicit = false, false
	return reflect.DeepEqual(left, right)
}

func decisionChangeEquivalent(left decisionChange, right decisionChange) bool {
	if !right.occurredAtExplicit {
		right.OccurredAt = left.OccurredAt
	}
	left.RecordedAt, right.RecordedAt = "", ""
	left.PageCursor, right.PageCursor = "", ""
	left.ledgerSequence, right.ledgerSequence = 0, 0
	left.idempotentReplay, right.idempotentReplay = false, false
	left.occurredAtExplicit, right.occurredAtExplicit = false, false
	return reflect.DeepEqual(left, right)
}

func (s *continuityStore) appendLocked(rows []continuityLedgerAppend) ([]continuityLedgerEntry, error) {
	if s == nil || !s.enabled {
		return nil, errors.New("continuity ledger is unavailable")
	}
	if len(rows) == 0 {
		return []continuityLedgerEntry{}, nil
	}
	if len(s.entries)+len(rows) > s.maxEntries {
		return nil, errors.New("continuity ledger entry capacity reached; no data was deleted")
	}
	entries := make([]continuityLedgerEntry, 0, len(rows))
	previousHash := s.lastHash
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	for index, row := range rows {
		payload, err := continuityPayloadMap(row.payload)
		if err != nil {
			return nil, err
		}
		entry := continuityLedgerEntry{
			SchemaID:     continuityLedgerEntrySchemaID,
			Sequence:     uint64(len(s.entries) + index + 1),
			EntryID:      "continuity_" + randomHex(12),
			Kind:         row.kind,
			RecordedAt:   nowUTCISO(),
			PreviousHash: previousHash,
			Payload:      payload,
		}
		entry.EntryHash, err = continuityEntryHash(entry)
		if err != nil {
			return nil, err
		}
		before := buffer.Len()
		if err := encoder.Encode(entry); err != nil {
			return nil, err
		}
		if buffer.Len()-before > continuityLedgerMaxLineBytes {
			return nil, errors.New("continuity ledger entry exceeds max line bytes")
		}
		entries = append(entries, entry)
		previousHash = entry.EntryHash
	}
	if s.fileBytes+int64(buffer.Len()) > s.maxBytes {
		return nil, errors.New("continuity ledger byte capacity reached; no data was deleted")
	}
	ledgerCreated := false
	if _, statErr := os.Lstat(s.path); errors.Is(statErr, os.ErrNotExist) {
		ledgerCreated = true
	} else if statErr != nil {
		return nil, s.disableAfterPersistenceFailure("stat continuity ledger before append", statErr)
	}
	file, err := openOwnerOnlyAppend(s.path, false)
	if err != nil {
		return nil, s.disableAfterPersistenceFailure("open continuity ledger for append", err)
	}
	wroteAny := false
	for buffer.Len() > 0 {
		written, writeErr := file.Write(buffer.Bytes())
		if written > 0 {
			wroteAny = true
			buffer.Next(written)
			s.fileBytes += int64(written)
		}
		if writeErr != nil {
			_ = file.Close()
			if wroteAny {
				return nil, s.disableAfterCommitUnknown("append continuity ledger", writeErr)
			}
			return nil, s.disableAfterPersistenceFailure("append continuity ledger", writeErr)
		}
		if written == 0 {
			_ = file.Close()
			if wroteAny {
				return nil, s.disableAfterCommitUnknown("append continuity ledger", io.ErrShortWrite)
			}
			return nil, s.disableAfterPersistenceFailure("append continuity ledger", io.ErrShortWrite)
		}
	}
	if s.fsync {
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return nil, s.disableAfterCommitUnknown("sync continuity ledger", err)
		}
		if ledgerCreated {
			if err := syncOwnerOnlyDirectory(filepath.Dir(s.path)); err != nil {
				_ = file.Close()
				return nil, s.disableAfterCommitUnknown("sync continuity ledger directory", err)
			}
		}
	}
	if err := file.Close(); err != nil {
		return nil, s.disableAfterCommitUnknown("close continuity ledger", err)
	}
	if s.afterPersistHook != nil {
		if err := s.afterPersistHook(); err != nil {
			return nil, s.disableAfterCommitUnknown("confirm continuity ledger append", err)
		}
	}
	for _, entry := range entries {
		if err := s.applyEntryLocked(entry); err != nil {
			return nil, s.disableAfterCommitUnknown("apply persisted continuity ledger entry", err)
		}
		s.entries = append(s.entries, entry)
		s.proofRevision = nextProofTimelineRevision(s.proofRevision)
		s.indexProofTimelineEntryLocked(entry, len(s.entries)-1)
		s.lastHash = entry.EntryHash
	}
	s.lastPersistedAt = nowUTCISO()
	return entries, nil
}

func (s *continuityStore) compact() (map[string]any, error) {
	if s == nil {
		return nil, errors.New("continuity ledger is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled {
		return nil, errors.New("continuity ledger is unavailable")
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	previousHash := ""
	for index, entry := range s.entries {
		if entry.SchemaID != continuityLedgerEntrySchemaID || entry.Sequence != uint64(index+1) || entry.PreviousHash != previousHash {
			return nil, fmt.Errorf("continuity ledger cannot compact invalid sequence %d", index+1)
		}
		hash, err := continuityEntryHash(entry)
		if err != nil || entry.EntryHash != hash {
			return nil, fmt.Errorf("continuity ledger cannot compact invalid hash at sequence %d", index+1)
		}
		before := buffer.Len()
		if err := encoder.Encode(entry); err != nil {
			return nil, err
		}
		if buffer.Len()-before > continuityLedgerMaxLineBytes {
			return nil, errors.New("continuity ledger entry exceeds max line bytes during compaction")
		}
		previousHash = entry.EntryHash
	}
	if int64(buffer.Len()) > s.maxBytes {
		return nil, errors.New("compacted continuity ledger exceeds max bytes")
	}
	beforeBytes := s.fileBytes
	if err := s.writeCanonicalLedgerBytes(buffer.Bytes()); err != nil {
		return nil, s.disableAfterPersistenceFailure("compact continuity ledger", err)
	}
	s.fileBytes = int64(buffer.Len())
	s.compactionCount++
	s.lastCompactedAt = nowUTCISO()
	return map[string]any{
		"schema_id": "continuity_ledger_compaction.v1", "ok": true, "lossless": true,
		"entry_count": len(s.entries), "before_bytes": beforeBytes, "after_bytes": s.fileBytes,
		"last_hash": s.lastHash, "performed_at": s.lastCompactedAt,
	}, nil
}

func (s *continuityStore) disableAfterPersistenceFailure(operation string, cause error) error {
	message := fmt.Sprintf("%s: %v; continuity ledger disabled until restart and verification", operation, cause)
	s.enabled = false
	s.lastError = message
	return errors.New(message)
}

func (s *continuityStore) disableAfterCommitUnknown(operation string, cause error) error {
	err := &continuityCommitUnknownError{Operation: operation, Err: cause}
	s.enabled = false
	s.lastError = err.Error()
	return err
}

func (s *continuityStore) validateObjectiveTransitionLocked(transition objectiveTransition) error {
	if transition.SchemaID != objectiveTransitionContractID || transition.TransitionID == "" || transition.ObjectiveID == "" {
		return errors.New("invalid objective transition")
	}
	if _, exists := s.objectiveTransitionIDs[transition.TransitionID]; exists {
		return errors.New("duplicate objective transition id")
	}
	if transition.IdempotencyKey == "" {
		return errors.New("objective transition idempotency key is required")
	}
	if _, exists := s.objectiveIdempotency[continuityIdempotencyIndexKey(transition.Project, transition.IdempotencyKey)]; exists {
		return errors.New("duplicate objective transition idempotency key")
	}
	return nil
}

func (s *continuityStore) validateDecisionChangeLocked(change decisionChange) error {
	if change.SchemaID != decisionChangeContractID || change.DecisionChangeID == "" || change.ObjectiveID == "" {
		return errors.New("invalid decision change")
	}
	if _, exists := s.decisionChangeIDs[change.DecisionChangeID]; exists {
		return errors.New("duplicate decision change id")
	}
	if change.IdempotencyKey == "" {
		return errors.New("decision change idempotency key is required")
	}
	if _, exists := s.decisionIdempotency[continuityIdempotencyIndexKey(change.Project, change.IdempotencyKey)]; exists {
		return errors.New("duplicate decision change idempotency key")
	}
	return nil
}

func (s *continuityStore) applyObjectiveTransitionValidatedLocked(transition objectiveTransition) {
	index := len(s.objectiveTransitions)
	s.objectiveTransitions = append(s.objectiveTransitions, transition)
	s.objectiveTransitionIDs[transition.TransitionID] = struct{}{}
	objectiveKey := continuityScopedIndexKey(transition.Project, transition.ObjectiveID)
	s.objectiveTransitionIndex[objectiveKey] = append(s.objectiveTransitionIndex[objectiveKey], index)
	projectKey := strings.ToLower(strings.TrimSpace(transition.Project))
	s.objectiveProjectIndex[projectKey] = append(s.objectiveProjectIndex[projectKey], index)
	s.objectiveIdempotency[continuityIdempotencyIndexKey(transition.Project, transition.IdempotencyKey)] = index
	if transition.DecisionChangeID != "" {
		s.decisionTransitionIndex[continuityScopedIndexKey(transition.Project, transition.DecisionChangeID)] = index
	}
	relatedIDs := mergeContinuityStrings(
		[]string{transition.ParentObjectiveID},
		append(append([]string{}, transition.DependsOn...), transition.Supersedes...),
		129,
	)
	for _, relatedID := range relatedIDs {
		if relatedID == "" || relatedID == transition.ObjectiveID {
			continue
		}
		currentKey := continuityScopedIndexKey(transition.Project, transition.ObjectiveID)
		relatedKey := continuityScopedIndexKey(transition.Project, relatedID)
		s.objectiveRelationIndex[currentKey] = append(
			s.objectiveRelationIndex[currentKey],
			objectiveGraphRelationRef{RelatedObjectiveID: relatedID, TransitionIndex: index},
		)
		s.objectiveRelationIndex[relatedKey] = append(
			s.objectiveRelationIndex[relatedKey],
			objectiveGraphRelationRef{RelatedObjectiveID: transition.ObjectiveID, TransitionIndex: index},
		)
	}
}

func (s *continuityStore) applyDecisionChangeValidatedLocked(change decisionChange) {
	index := len(s.decisionChanges)
	s.decisionChanges = append(s.decisionChanges, change)
	s.decisionChangeIDs[change.DecisionChangeID] = struct{}{}
	s.decisionChangeIndex[continuityScopedIndexKey(change.Project, change.DecisionChangeID)] = index
	projectKey := strings.ToLower(strings.TrimSpace(change.Project))
	projectIndexes := s.decisionProjectIndex[projectKey]
	position := sort.Search(len(projectIndexes), func(position int) bool {
		existingIndex := projectIndexes[position]
		return existingIndex < 0 || existingIndex >= len(s.decisionChanges) ||
			!decisionChangeChronologyOlder(s.decisionChanges[existingIndex], change)
	})
	projectIndexes = append(projectIndexes, 0)
	copy(projectIndexes[position+1:], projectIndexes[position:])
	projectIndexes[position] = index
	s.decisionProjectIndex[projectKey] = projectIndexes
	s.decisionIdempotency[continuityIdempotencyIndexKey(change.Project, change.IdempotencyKey)] = index
}

func (s *continuityStore) applyEntryLocked(entry continuityLedgerEntry) error {
	s.ensureIndexesLocked()
	switch entry.Kind {
	case continuityLedgerKindTaskIdentity:
		var receipt taskIdentityReceipt
		if err := decodeContinuityPayload(entry.Payload, &receipt); err != nil {
			return err
		}
		return s.applyTaskIdentityReceiptLocked(receipt)
	case continuityLedgerKindObjectiveTransition:
		var transition objectiveTransition
		if err := decodeContinuityPayload(entry.Payload, &transition); err != nil {
			return err
		}
		transition.RecordedAt = entry.RecordedAt
		transition.ledgerSequence = entry.Sequence
		if err := s.validateObjectiveTransitionLocked(transition); err != nil {
			return err
		}
		s.applyObjectiveTransitionValidatedLocked(transition)
		return nil
	case continuityLedgerKindDecisionChange:
		var change decisionChange
		if err := decodeContinuityPayload(entry.Payload, &change); err != nil {
			return err
		}
		change.RecordedAt = entry.RecordedAt
		change.ledgerSequence = entry.Sequence
		if err := s.validateDecisionChangeLocked(change); err != nil {
			return err
		}
		s.applyDecisionChangeValidatedLocked(change)
		return nil
	case continuityLedgerKindDecisionBundle:
		var bundle continuityDecisionBundle
		if err := decodeContinuityPayload(entry.Payload, &bundle); err != nil {
			return err
		}
		if bundle.SchemaID != continuityDecisionBundleSchemaID {
			return errors.New("invalid decision bundle")
		}
		bundle.DecisionChange.RecordedAt = entry.RecordedAt
		bundle.ObjectiveTransition.RecordedAt = entry.RecordedAt
		bundle.DecisionChange.ledgerSequence = entry.Sequence
		bundle.ObjectiveTransition.ledgerSequence = entry.Sequence
		if bundle.ObjectiveTransition.DecisionChangeID != bundle.DecisionChange.DecisionChangeID ||
			bundle.ObjectiveTransition.ObjectiveID != bundle.DecisionChange.ObjectiveID ||
			!strings.EqualFold(bundle.ObjectiveTransition.Project, bundle.DecisionChange.Project) ||
			bundle.ObjectiveTransition.OccurredAt != bundle.DecisionChange.OccurredAt {
			return errors.New("decision bundle linkage mismatch")
		}
		if err := s.validateDecisionChangeLocked(bundle.DecisionChange); err != nil {
			return err
		}
		if err := s.validateObjectiveTransitionLocked(bundle.ObjectiveTransition); err != nil {
			return err
		}
		s.applyDecisionChangeValidatedLocked(bundle.DecisionChange)
		s.applyObjectiveTransitionValidatedLocked(bundle.ObjectiveTransition)
		return nil
	default:
		return fmt.Errorf("unsupported continuity ledger kind %q", entry.Kind)
	}
}

func decodeContinuityPayload(payload map[string]any, target any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

func (s *continuityStore) applyTaskIdentityReceiptLocked(receipt taskIdentityReceipt) error {
	if receipt.SchemaID != taskIdentityReceiptContractID || receipt.TaskIdentityID == "" || receipt.IdempotencyKey == "" {
		return errors.New("invalid task identity receipt")
	}
	idempotencyIndexKey := continuityTaskOperationIdempotencyIndexKey(receipt.Project, receipt.Operation, receipt.IdempotencyKey)
	if _, exists := s.taskOperationIdempotency[idempotencyIndexKey]; exists {
		return errors.New("duplicate task identity operation idempotency key")
	}
	switch receipt.Operation {
	case "create", "split":
		if _, exists := s.taskIdentities[receipt.TaskIdentityID]; exists {
			return errors.New("task identity already exists")
		}
		record := taskIdentityRecord{
			TaskIdentityID:       receipt.TaskIdentityID,
			Project:              receipt.Project,
			Repo:                 receipt.Repo,
			Objective:            receipt.Objective,
			NormalizedObjective:  receipt.NormalizedObjective,
			ExternalTaskIDs:      append([]string{}, receipt.ExternalTaskIDs...),
			ParentTaskIdentityID: receipt.ParentTaskIdentityID,
			Status:               "active",
			CreatedAt:            receipt.CreatedAt,
			UpdatedAt:            receipt.CreatedAt,
			semanticTokens:       continuityObjectiveTokenSlice(receipt.NormalizedObjective),
		}
		s.taskIdentities[record.TaskIdentityID] = record
		s.addTaskObjectiveIndexLocked(record)
		for _, alias := range record.ExternalTaskIDs {
			s.taskAliases[continuityAliasKey(record.Project, record.Repo, alias)] = record.TaskIdentityID
		}
	case "merge":
		target, exists := s.taskIdentities[receipt.TaskIdentityID]
		if !exists {
			return errors.New("merge target task identity is missing")
		}
		for _, sourceID := range receipt.SourceTaskIdentityIDs {
			source, ok := s.taskIdentities[sourceID]
			if !ok {
				return errors.New("merge source task identity is missing")
			}
			source.Status = "merged"
			source.MergedInto = target.TaskIdentityID
			source.UpdatedAt = receipt.CreatedAt
			s.taskIdentities[sourceID] = source
			s.redirectAllTaskObjectiveIndexesLocked(source.TaskIdentityID, target.TaskIdentityID)
			for _, alias := range source.ExternalTaskIDs {
				s.taskAliases[continuityAliasKey(source.Project, source.Repo, alias)] = target.TaskIdentityID
			}
			target.ExternalTaskIDs = mergeContinuityStrings(target.ExternalTaskIDs, source.ExternalTaskIDs, 64)
		}
		target.ExternalTaskIDs = mergeContinuityStrings(target.ExternalTaskIDs, receipt.ExternalTaskIDs, 64)
		target.UpdatedAt = receipt.CreatedAt
		s.taskIdentities[target.TaskIdentityID] = target
	case "link":
		target, exists := s.taskIdentities[receipt.TaskIdentityID]
		if !exists || target.Status != "active" {
			return errors.New("link target task identity is missing or inactive")
		}
		if !strings.EqualFold(target.Project, receipt.Project) || !strings.EqualFold(target.Repo, receipt.Repo) {
			return errors.New("task identity link cannot cross project or repo scope")
		}
		for _, alias := range receipt.ExternalTaskIDs {
			aliasKey := continuityAliasKey(target.Project, target.Repo, alias)
			if existingID := s.taskAliases[aliasKey]; existingID != "" && existingID != target.TaskIdentityID {
				return fmt.Errorf("task identity alias %q already belongs to another identity", alias)
			}
			s.taskAliases[aliasKey] = target.TaskIdentityID
		}
		target.ExternalTaskIDs = mergeContinuityStrings(target.ExternalTaskIDs, receipt.ExternalTaskIDs, 64)
		target.UpdatedAt = receipt.CreatedAt
		s.taskIdentities[target.TaskIdentityID] = target
	default:
		return fmt.Errorf("unsupported task identity operation %q", receipt.Operation)
	}
	s.taskOperationIdempotency[idempotencyIndexKey] = receipt
	return nil
}

func continuityAliasKey(project string, repo string, alias string) string {
	return strings.ToLower(strings.TrimSpace(project)) + "\x00" + strings.ToLower(strings.TrimSpace(repo)) + "\x00" + strings.ToLower(strings.TrimSpace(alias))
}

func continuityObjectiveIndexKey(project string, repo string, objective string) string {
	return strings.ToLower(strings.TrimSpace(project)) + "\x00" + strings.ToLower(strings.TrimSpace(repo)) + "\x00" + objective
}

func continuityScopedIndexKey(project string, id string) string {
	return strings.ToLower(strings.TrimSpace(project)) + "\x00" + strings.TrimSpace(id)
}

func continuityIdempotencyIndexKey(project string, key string) string {
	return strings.ToLower(strings.TrimSpace(project)) + "\x00" + strings.ToLower(strings.TrimSpace(key))
}

func continuityTaskOperationIdempotencyIndexKey(project string, operation string, key string) string {
	return strings.ToLower(strings.TrimSpace(project)) + "\x00" + strings.ToLower(strings.TrimSpace(operation)) + "\x00" + strings.ToLower(strings.TrimSpace(key))
}

func continuityTaskOperationKey(payload map[string]any, operation string, fallbackSeed string) (string, error) {
	key := strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["idempotency_key"]), anyToString(payload["idempotencyKey"])))
	if key == "" {
		key = strings.ToLower(strings.TrimSpace(operation)) + "_" + sha256Hex(fallbackSeed)[:32]
	}
	return normalizeContinuityIdempotencyKey(key, strings.ToLower(strings.TrimSpace(operation))+"_")
}

func taskIdentityReceiptEquivalent(left taskIdentityReceipt, right taskIdentityReceipt) bool {
	left.ReceiptID, right.ReceiptID = "", ""
	left.CreatedAt, right.CreatedAt = "", ""
	return reflect.DeepEqual(left, right)
}

func continuityContainsSecret(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if portableSecretKey(key) || continuityContainsSecret(item) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if continuityContainsSecret(item) {
				return true
			}
		}
	case string:
		return portableBearerPattern.MatchString(typed) || portableTokenPattern.MatchString(typed)
	}
	return false
}

func rejectContinuitySecrets(payload map[string]any) error {
	if continuityContainsSecret(payload) {
		return errors.New("secret-bearing keys or token-like values are forbidden in continuity records")
	}
	return nil
}

func (s *continuityStore) addTaskObjectiveIndexLocked(record taskIdentityRecord) {
	if record.NormalizedObjective == "" || record.Status != "active" {
		return
	}
	if s.taskObjectiveIndex == nil {
		s.taskObjectiveIndex = map[string]map[string]struct{}{}
	}
	key := continuityObjectiveIndexKey(record.Project, record.Repo, record.NormalizedObjective)
	ids := s.taskObjectiveIndex[key]
	if ids == nil {
		ids = map[string]struct{}{}
		s.taskObjectiveIndex[key] = ids
	}
	ids[record.TaskIdentityID] = struct{}{}
}

func (s *continuityStore) removeTaskObjectiveIndexLocked(record taskIdentityRecord) {
	key := continuityObjectiveIndexKey(record.Project, record.Repo, record.NormalizedObjective)
	ids := s.taskObjectiveIndex[key]
	delete(ids, record.TaskIdentityID)
	if len(ids) == 0 {
		delete(s.taskObjectiveIndex, key)
	}
}

func (s *continuityStore) redirectAllTaskObjectiveIndexesLocked(sourceID string, targetID string) {
	for key, ids := range s.taskObjectiveIndex {
		if _, exists := ids[sourceID]; !exists {
			continue
		}
		delete(ids, sourceID)
		ids[targetID] = struct{}{}
		s.taskObjectiveIndex[key] = ids
	}
}

func mergeContinuityStrings(left []string, right []string, limit int) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, minInt(len(left)+len(right), limit))
	for _, value := range append(append([]string{}, left...), right...) {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, clipText(value, 160))
		if len(out) >= limit {
			break
		}
	}
	sort.Strings(out)
	return out
}

func normalizeContinuityObjective(raw string) string {
	raw = strings.TrimSpace(strings.ToLower(raw))
	var builder strings.Builder
	space := false
	for _, char := range raw {
		if unicode.IsLetter(char) || unicode.IsDigit(char) {
			builder.WriteRune(char)
			space = false
			continue
		}
		if !space && builder.Len() > 0 {
			builder.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(builder.String())
}

var continuityStopWords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "are": {}, "as": {}, "at": {}, "be": {}, "by": {}, "for": {},
	"from": {}, "in": {}, "is": {}, "it": {}, "of": {}, "on": {}, "or": {}, "that": {}, "the": {},
	"this": {}, "to": {}, "with": {},
}

func continuityObjectiveTokenSlice(value string) []string {
	seen := map[string]struct{}{}
	for _, token := range strings.Fields(normalizeContinuityObjective(value)) {
		if len(token) < 2 {
			continue
		}
		if _, skip := continuityStopWords[token]; skip {
			continue
		}
		seen[token] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for token := range seen {
		out = append(out, token)
	}
	sort.Strings(out)
	return out
}

func continuitySemanticTokenScore(leftTokens []string, rightTokens []string) float64 {
	if len(leftTokens) == 0 || len(rightTokens) == 0 {
		return 0
	}
	leftIndex, rightIndex, intersection := 0, 0, 0
	for leftIndex < len(leftTokens) && rightIndex < len(rightTokens) {
		switch strings.Compare(leftTokens[leftIndex], rightTokens[rightIndex]) {
		case -1:
			leftIndex++
		case 1:
			rightIndex++
		default:
			intersection++
			leftIndex++
			rightIndex++
		}
	}
	return float64(intersection) / float64(len(leftTokens)+len(rightTokens)-intersection)
}

func continuityExecutionLaneID(payload map[string]any, project string, repo string) (string, error) {
	if explicit := strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["execution_lane_id"]), anyToString(payload["executionLaneId"]))); explicit != "" {
		if continuityIDPattern.MatchString(explicit) {
			return explicit, nil
		}
		return "", errors.New("execution_lane_id contains unsupported characters")
	}
	parts := []string{
		strings.ToLower(strings.TrimSpace(project)), strings.ToLower(strings.TrimSpace(repo)),
		strings.ToLower(strings.TrimSpace(anyToString(payload["branch"]))),
		strings.TrimSpace(anyToString(payload["worktree"])), strings.TrimSpace(anyToString(payload["cwd"])),
		strings.ToLower(strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["agent_id"]), anyToString(payload["agent"])))),
	}
	meaningful := false
	for _, value := range parts[2:] {
		if value != "" {
			meaningful = true
			break
		}
	}
	if !meaningful {
		return "", nil
	}
	return "lane_" + sha256Hex(strings.Join(parts, "\x00"))[:24], nil
}

func continuityTaskIdentityID(project string, repo string, normalizedObjective string, externalTaskID string) string {
	seed := normalizedObjective
	if seed == "" {
		seed = strings.ToLower(strings.TrimSpace(externalTaskID))
	}
	return "task_" + sha256Hex(strings.ToLower(project) + "\x00" + strings.ToLower(repo) + "\x00" + seed)[:32]
}

func (s *continuityStore) resolveMergedIdentityLocked(record taskIdentityRecord) taskIdentityRecord {
	seen := map[string]struct{}{}
	for record.Status == "merged" && record.MergedInto != "" {
		if _, exists := seen[record.TaskIdentityID]; exists {
			break
		}
		seen[record.TaskIdentityID] = struct{}{}
		next, ok := s.taskIdentities[record.MergedInto]
		if !ok {
			break
		}
		record = next
	}
	return record
}

func (s *continuityStore) resolveTaskIdentityLinkLocked(identityID string, project string) (string, error) {
	identityID = strings.TrimSpace(identityID)
	if identityID == "" {
		return "", nil
	}
	record, ok := s.taskIdentities[identityID]
	if !ok {
		return "", errors.New("task_identity_id does not exist")
	}
	record = s.resolveMergedIdentityLocked(record)
	if !strings.EqualFold(record.Project, project) {
		return "", errors.New("task_identity_id cannot cross project scope")
	}
	return record.TaskIdentityID, nil
}

func (s *continuityStore) reconcile(payload map[string]any, createIfMissing bool) (map[string]any, error) {
	if s == nil || !s.enabled {
		return nil, errors.New("continuity ledger is unavailable")
	}
	if err := rejectContinuitySecrets(payload); err != nil {
		return nil, err
	}
	project, err := sanitizeMemoryProject(firstNonEmptyStrings(anyToString(payload["project"]), anyToString(payload["project_name"])))
	if err != nil {
		return nil, fmt.Errorf("project: %w", err)
	}
	repo := clipText(strings.TrimSpace(anyToString(payload["repo"])), 320)
	objective := clipText(strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["objective"]), anyToString(payload["title"]), anyToString(payload["query"]), anyToString(payload["task_title"]))), 2000)
	externalTaskID := clipText(strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["task_id"]), anyToString(payload["taskId"]))), 160)
	if objective == "" && externalTaskID == "" && strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["task_identity_id"]), anyToString(payload["taskIdentityId"]))) == "" {
		return nil, errors.New("objective, task_id, or task_identity_id is required")
	}
	normalized := normalizeContinuityObjective(objective)
	laneID, err := continuityExecutionLaneID(payload, project, repo)
	if err != nil {
		return nil, err
	}
	explicitID := strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["task_identity_id"]), anyToString(payload["taskIdentityId"])))
	if explicitID != "" && !continuityIDPattern.MatchString(explicitID) {
		return nil, errors.New("task_identity_id contains unsupported characters")
	}
	if value, ok := payload["create_if_missing"]; ok {
		createIfMissing = anyToBool(value)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := map[string]any{
		"ok": true, "schema_id": taskIdentityReconciliationContractID,
		"project": project, "repo": repo, "objective": objective,
		"normalized_objective": normalized, "execution_lane_id": laneID,
		"threshold": continuitySemanticThreshold, "margin": continuitySemanticMargin,
		"exact_first": true, "semantic_auto_merge": false, "requires_confirmation": false,
		"abstained": false, "candidates": []any{}, "receipt": map[string]any{},
	}
	selectionSequence, selectionHash := s.projectCoreAnchorLocked(project)
	result["selection_anchor"] = map[string]any{"project": project, "sequence": selectionSequence, "entry_hash": selectionHash}
	if explicitID != "" {
		if record, ok := s.taskIdentities[explicitID]; ok {
			record = s.resolveMergedIdentityLocked(record)
			if !strings.EqualFold(record.Project, project) || (repo != "" && !strings.EqualFold(record.Repo, repo)) {
				return nil, errContinuityIdentityScope
			}
			result["match_mode"] = "exact_id"
			result["task_identity"] = record
			result["task_identity_id"] = record.TaskIdentityID
			return result, nil
		}
		return nil, errContinuityIdentityMissing
	}
	if externalTaskID != "" {
		if identityID := s.taskAliases[continuityAliasKey(project, repo, externalTaskID)]; identityID != "" {
			record := s.resolveMergedIdentityLocked(s.taskIdentities[identityID])
			result["match_mode"] = "exact_task_id"
			result["task_identity"] = record
			result["task_identity_id"] = record.TaskIdentityID
			return result, nil
		}
	}
	exact := []taskIdentityRecord{}
	if normalized != "" {
		for identityID := range s.taskObjectiveIndex[continuityObjectiveIndexKey(project, repo, normalized)] {
			if record, ok := s.taskIdentities[identityID]; ok && record.Status == "active" {
				exact = append(exact, record)
			}
		}
	}
	if len(exact) == 1 {
		result["match_mode"] = "exact_objective"
		result["task_identity"] = exact[0]
		result["task_identity_id"] = exact[0].TaskIdentityID
		return result, nil
	}
	if len(exact) > 1 {
		sort.Slice(exact, func(i, j int) bool { return exact[i].TaskIdentityID < exact[j].TaskIdentityID })
		candidates := make([]any, 0, len(exact))
		for _, record := range exact {
			candidates = append(candidates, taskIdentityCandidate{Identity: record, Score: 1})
		}
		result["match_mode"] = "ambiguous_exact"
		result["abstained"] = true
		result["requires_confirmation"] = true
		result["candidates"] = candidates
		return result, nil
	}
	candidates := make([]taskIdentityCandidate, 0, 5)
	if normalized != "" {
		queryTokens := continuityObjectiveTokenSlice(normalized)
		for _, record := range s.taskIdentities {
			if record.Status != "active" || !strings.EqualFold(record.Project, project) || !strings.EqualFold(record.Repo, repo) {
				continue
			}
			score := continuitySemanticTokenScore(queryTokens, record.semanticTokens)
			if score > 0 {
				candidates = insertContinuityCandidate(candidates, taskIdentityCandidate{Identity: record, Score: score}, 5)
			}
		}
	}
	publicCandidates := make([]any, 0, len(candidates))
	for _, candidate := range candidates {
		publicCandidates = append(publicCandidates, candidate)
	}
	result["candidates"] = publicCandidates
	if len(candidates) > 0 && candidates[0].Score >= continuitySemanticThreshold {
		ambiguous := len(candidates) > 1 && candidates[0].Score-candidates[1].Score < continuitySemanticMargin
		result["match_mode"] = map[bool]string{true: "ambiguous_semantic", false: "semantic_candidate"}[ambiguous]
		result["abstained"] = true
		result["requires_confirmation"] = true
		result["top_score"] = candidates[0].Score
		if !ambiguous {
			result["candidate_task_identity_id"] = candidates[0].Identity.TaskIdentityID
		}
		return result, nil
	}
	if !createIfMissing {
		result["match_mode"] = "none"
		result["abstained"] = true
		return result, nil
	}
	identityID := continuityTaskIdentityID(project, repo, normalized, externalTaskID)
	if record, exists := s.taskIdentities[identityID]; exists {
		record = s.resolveMergedIdentityLocked(record)
		result["match_mode"] = "exact_derived_id"
		result["task_identity"] = record
		result["task_identity_id"] = record.TaskIdentityID
		return result, nil
	}
	now := nowUTCISO()
	idempotencyKey := "create_" + sha256Hex(identityID)[:32]
	receipt := taskIdentityReceipt{
		SchemaID: taskIdentityReceiptContractID, ReceiptID: "tir_" + sha256Hex(project + "\x00create\x00" + idempotencyKey)[:24], Operation: "create",
		TaskIdentityID: identityID, SourceTaskIdentityIDs: []string{}, Project: project, Repo: repo, Objective: objective,
		NormalizedObjective: normalized, ExternalTaskIDs: mergeContinuityStrings(nil, []string{externalTaskID}, 64),
		ExecutionLaneID: laneID, SessionID: clipText(strings.TrimSpace(anyToString(payload["session_id"])), 160),
		Actor:  clipText(firstNonEmptyStrings(anyToString(payload["actor"]), anyToString(payload["agent_id"]), "local_user"), 160),
		Reason: "no exact or unambiguous semantic identity existed", MatchMode: "created", IdempotencyKey: idempotencyKey, CreatedAt: now,
	}
	if _, err := s.appendLocked([]continuityLedgerAppend{{kind: continuityLedgerKindTaskIdentity, payload: receipt}}); err != nil {
		return nil, err
	}
	record := s.taskIdentities[identityID]
	result["match_mode"] = "created"
	result["abstained"] = false
	result["task_identity"] = record
	result["task_identity_id"] = identityID
	result["receipt"] = taskIdentityReceiptOutput(receipt)
	return result, nil
}

func (s *continuityStore) mergeTaskIdentities(payload map[string]any) (map[string]any, error) {
	if s == nil || !s.enabled {
		return nil, errors.New("continuity ledger is unavailable")
	}
	if err := rejectContinuitySecrets(payload); err != nil {
		return nil, err
	}
	targetID := strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["target_task_identity_id"]), anyToString(payload["task_identity_id"])))
	sourceIDs := normalizeContinuityIDs(firstNonEmptyAny(payload["source_task_identity_ids"], payload["source_ids"]), targetID, 32)
	actor := clipText(strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["actor"]), anyToString(payload["agent_id"]))), 160)
	reason := clipText(strings.TrimSpace(anyToString(payload["reason"])), 1000)
	if targetID == "" || len(sourceIDs) == 0 || actor == "" || reason == "" {
		return nil, errors.New("target_task_identity_id, source_task_identity_ids, actor, and reason are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	target, ok := s.taskIdentities[targetID]
	if !ok || target.Status != "active" {
		return nil, errors.New("merge target must be an active task identity")
	}
	laneID, err := continuityExecutionLaneID(payload, target.Project, target.Repo)
	if err != nil {
		return nil, err
	}
	idempotencyKey, err := continuityTaskOperationKey(
		payload,
		"merge",
		strings.ToLower(target.Project)+"\x00"+targetID+"\x00"+strings.Join(sourceIDs, "\x00"),
	)
	if err != nil {
		return nil, err
	}
	receipt := taskIdentityReceipt{
		SchemaID: taskIdentityReceiptContractID, ReceiptID: "tir_" + sha256Hex(target.Project + "\x00merge\x00" + idempotencyKey)[:24], Operation: "merge",
		TaskIdentityID: targetID, SourceTaskIdentityIDs: sourceIDs, Project: target.Project, Repo: target.Repo,
		Objective: target.Objective, NormalizedObjective: target.NormalizedObjective,
		ExecutionLaneID: laneID,
		SessionID:       clipText(strings.TrimSpace(anyToString(payload["session_id"])), 160),
		Actor:           actor, Reason: reason, MatchMode: "manual_merge", IdempotencyKey: idempotencyKey,
	}
	idempotencyIndexKey := continuityTaskOperationIdempotencyIndexKey(target.Project, receipt.Operation, idempotencyKey)
	if existing, exists := s.taskOperationIdempotency[idempotencyIndexKey]; exists {
		if !taskIdentityReceiptEquivalent(existing, receipt) {
			return nil, errors.New("idempotency_key already exists with a different task identity merge")
		}
		return map[string]any{
			"ok": true, "schema_id": taskIdentityReconciliationContractID, "operation": "merge",
			"match_mode": "manual_merge", "task_identity_id": existing.TaskIdentityID, "task_identity": s.taskIdentities[existing.TaskIdentityID],
			"source_task_identity_ids": existing.SourceTaskIdentityIDs, "receipt": taskIdentityReceiptOutput(existing), "abstained": false,
			"exact_first": true, "semantic_auto_merge": false, "requires_confirmation": false,
			"recorded": false, "idempotent_replay": true,
		}, nil
	}
	for _, sourceID := range sourceIDs {
		source, exists := s.taskIdentities[sourceID]
		if !exists || source.Status != "active" {
			return nil, fmt.Errorf("merge source %s must be an active task identity", sourceID)
		}
		if !strings.EqualFold(source.Project, target.Project) || !strings.EqualFold(source.Repo, target.Repo) {
			return nil, errors.New("task identity merge cannot cross project or repo scope")
		}
	}
	receipt.CreatedAt = nowUTCISO()
	if _, err := s.appendLocked([]continuityLedgerAppend{{kind: continuityLedgerKindTaskIdentity, payload: receipt}}); err != nil {
		return nil, err
	}
	return map[string]any{
		"ok": true, "schema_id": taskIdentityReconciliationContractID, "operation": "merge",
		"match_mode": "manual_merge", "task_identity_id": targetID, "task_identity": s.taskIdentities[targetID],
		"source_task_identity_ids": sourceIDs, "receipt": taskIdentityReceiptOutput(receipt), "abstained": false,
		"exact_first": true, "semantic_auto_merge": false, "requires_confirmation": false,
		"recorded": true, "idempotent_replay": false,
	}, nil
}

func (s *continuityStore) splitTaskIdentity(payload map[string]any) (map[string]any, error) {
	if s == nil || !s.enabled {
		return nil, errors.New("continuity ledger is unavailable")
	}
	if err := rejectContinuitySecrets(payload); err != nil {
		return nil, err
	}
	sourceID := strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["source_task_identity_id"]), anyToString(payload["parent_task_identity_id"])))
	actor := clipText(strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["actor"]), anyToString(payload["agent_id"]))), 160)
	reason := clipText(strings.TrimSpace(anyToString(payload["reason"])), 1000)
	if sourceID == "" || actor == "" || reason == "" {
		return nil, errors.New("source_task_identity_id, actor, and reason are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	source, ok := s.taskIdentities[sourceID]
	if !ok || source.Status != "active" {
		return nil, errors.New("split source must be an active task identity")
	}
	objective := clipText(strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["objective"]), source.Objective)), 2000)
	normalized := normalizeContinuityObjective(objective)
	externalIDs := mergeContinuityStrings(nil, anyToStringList(firstNonEmptyAny(payload["external_task_ids"], payload["task_ids"]), 64), 64)
	if taskID := strings.TrimSpace(anyToString(payload["task_id"])); taskID != "" {
		externalIDs = mergeContinuityStrings(externalIDs, []string{taskID}, 64)
	}
	laneID, err := continuityExecutionLaneID(payload, source.Project, source.Repo)
	if err != nil {
		return nil, err
	}
	identityID := strings.TrimSpace(anyToString(payload["task_identity_id"]))
	idempotencyKey, err := continuityTaskOperationKey(
		payload,
		"split",
		strings.Join([]string{
			strings.ToLower(source.Project), strings.ToLower(source.Repo), sourceID, identityID,
			normalized, strings.Join(externalIDs, "\x00"), actor, reason,
		}, "\x00"),
	)
	if err != nil {
		return nil, err
	}
	if identityID == "" {
		identityID = "task_" + sha256Hex(strings.ToLower(source.Project) + "\x00" + sourceID + "\x00" + idempotencyKey)[:32]
	}
	if !continuityIDPattern.MatchString(identityID) {
		return nil, errors.New("task_identity_id contains unsupported characters")
	}
	receipt := taskIdentityReceipt{
		SchemaID: taskIdentityReceiptContractID, ReceiptID: "tir_" + sha256Hex(source.Project + "\x00split\x00" + idempotencyKey)[:24], Operation: "split",
		TaskIdentityID: identityID, SourceTaskIdentityIDs: []string{sourceID}, ParentTaskIdentityID: sourceID,
		Project: source.Project, Repo: source.Repo, Objective: objective, NormalizedObjective: normalized,
		ExternalTaskIDs: externalIDs, ExecutionLaneID: laneID,
		SessionID: clipText(strings.TrimSpace(anyToString(payload["session_id"])), 160),
		Actor:     actor, Reason: reason, MatchMode: "manual_split", IdempotencyKey: idempotencyKey,
	}
	idempotencyIndexKey := continuityTaskOperationIdempotencyIndexKey(source.Project, receipt.Operation, idempotencyKey)
	if existing, exists := s.taskOperationIdempotency[idempotencyIndexKey]; exists {
		if !taskIdentityReceiptEquivalent(existing, receipt) {
			return nil, errors.New("idempotency_key already exists with a different task identity split")
		}
		return map[string]any{
			"ok": true, "schema_id": taskIdentityReconciliationContractID, "operation": "split",
			"match_mode": "manual_split", "task_identity_id": existing.TaskIdentityID, "task_identity": s.taskIdentities[existing.TaskIdentityID],
			"source_task_identity_ids": existing.SourceTaskIdentityIDs, "receipt": taskIdentityReceiptOutput(existing), "abstained": false,
			"exact_first": true, "semantic_auto_merge": false, "requires_confirmation": false,
			"recorded": false, "idempotent_replay": true,
		}, nil
	}
	if _, exists := s.taskIdentities[identityID]; exists {
		return nil, errors.New("split target task identity already exists")
	}
	for _, alias := range externalIDs {
		if existingID := s.taskAliases[continuityAliasKey(source.Project, source.Repo, alias)]; existingID != "" {
			return nil, fmt.Errorf("split external task id %q already belongs to task identity %s", alias, existingID)
		}
	}
	receipt.CreatedAt = nowUTCISO()
	if _, err := s.appendLocked([]continuityLedgerAppend{{kind: continuityLedgerKindTaskIdentity, payload: receipt}}); err != nil {
		return nil, err
	}
	return map[string]any{
		"ok": true, "schema_id": taskIdentityReconciliationContractID, "operation": "split",
		"match_mode": "manual_split", "task_identity_id": identityID, "task_identity": s.taskIdentities[identityID],
		"source_task_identity_ids": []string{sourceID}, "receipt": taskIdentityReceiptOutput(receipt), "abstained": false,
		"exact_first": true, "semantic_auto_merge": false, "requires_confirmation": false,
		"recorded": true, "idempotent_replay": false,
	}, nil
}

func normalizeContinuityIDs(raw any, self string, limit int) []string {
	out := []string{}
	seen := map[string]struct{}{}
	for _, value := range anyToStringList(raw, limit*2) {
		value = strings.TrimSpace(value)
		if value == "" || value == self || !continuityIDPattern.MatchString(value) {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
		if len(out) >= limit {
			break
		}
	}
	sort.Strings(out)
	return out
}

func (s *continuityStore) snapshot() map[string]any {
	if s == nil {
		return map[string]any{"schema_id": "continuity_ledger_status.v1", "enabled": false, "ready": false}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	active, merged := 0, 0
	for _, record := range s.taskIdentities {
		if record.Status == "merged" {
			merged++
		} else {
			active++
		}
	}
	return map[string]any{
		"schema_id": "continuity_ledger_status.v1", "enabled": s.enabled, "ready": s.enabled && s.lastError == "",
		"store_ref": ownerOnlyStoreRef("continuity_ledger"), "entry_count": len(s.entries), "max_entries": s.maxEntries,
		"bytes": s.fileBytes, "max_bytes": s.maxBytes, "last_hash": s.lastHash, "last_persisted_at": s.lastPersistedAt,
		"compaction_count": s.compactionCount, "last_compacted_at": s.lastCompactedAt,
		"tail_recovery_count": s.tailRecoveryCount, "tail_recovery_bytes": s.tailRecoveryBytes,
		"task_identity_count": len(s.taskIdentities), "active_task_identities": active, "merged_task_identities": merged,
		"objective_transition_count": len(s.objectiveTransitions), "decision_change_count": len(s.decisionChanges),
		"semantic_threshold": continuitySemanticThreshold, "semantic_margin": continuitySemanticMargin,
		"semantic_auto_merge": false, "last_error": s.lastError,
	}
}

func (s *server) enrichAgentSessionContinuity(payload map[string]any) (map[string]any, error) {
	if payload == nil {
		return map[string]any{}, nil
	}
	project := strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["project"]), anyToString(payload["project_name"])))
	repo := strings.TrimSpace(anyToString(payload["repo"]))
	laneID, laneErr := continuityExecutionLaneID(payload, project, repo)
	if laneErr != nil {
		return nil, laneErr
	}
	if laneID != "" {
		payload["execution_lane_id"] = laneID
	}
	explicitIdentityID := strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["task_identity_id"]), anyToString(payload["taskIdentityId"])))
	if explicitIdentityID != "" && project == "" {
		return nil, errors.New("project is required when task_identity_id is supplied")
	}
	if s == nil || s.continuity == nil || !s.continuity.enabled {
		delete(payload, "task_identity_id")
		delete(payload, "taskIdentityId")
		metadata := anyMap(payload["metadata"])
		status := "disabled"
		detail := "continuity identity is intentionally disabled"
		if s == nil || s.continuity == nil || strings.TrimSpace(s.continuity.lastError) != "" {
			status = "unavailable"
			detail = "continuity identity validation failed initialization or persistence verification"
			if s != nil && s.continuity != nil && strings.TrimSpace(s.continuity.lastError) != "" {
				detail = clipText(s.continuity.lastError, 500)
			}
		}
		metadata["continuity"] = map[string]any{"status": status, "detail": detail, "semantic_auto_merge": false}
		payload["metadata"] = metadata
		if explicitIdentityID != "" && status == "unavailable" {
			return nil, fmt.Errorf("%w: %s", errContinuityUnavailable, detail)
		}
		return map[string]any{"status": status, "detail": detail}, nil
	}
	if project == "" {
		return map[string]any{}, nil
	}
	if strings.TrimSpace(firstNonEmptyStrings(
		explicitIdentityID, anyToString(payload["task_id"]), anyToString(payload["taskId"]),
		anyToString(payload["objective"]), anyToString(payload["title"]),
	)) == "" {
		return map[string]any{}, nil
	}
	result, err := s.continuity.reconcile(payload, true)
	metadata := anyMap(payload["metadata"])
	if err != nil {
		delete(payload, "task_identity_id")
		delete(payload, "taskIdentityId")
		if errors.Is(err, errContinuityIdentityScope) || errors.Is(err, errContinuityIdentityMissing) {
			return nil, err
		}
		metadata["continuity"] = map[string]any{
			"status": "unavailable", "error": clipText(err.Error(), 500), "semantic_auto_merge": false,
		}
		payload["metadata"] = metadata
		if explicitIdentityID != "" {
			return nil, fmt.Errorf("%w: %s", errContinuityUnavailable, clipText(err.Error(), 500))
		}
		return map[string]any{"status": "unavailable", "error": clipText(err.Error(), 500)}, nil
	}
	if identityID := strings.TrimSpace(anyToString(result["task_identity_id"])); identityID != "" {
		payload["task_identity_id"] = identityID
	} else {
		delete(payload, "task_identity_id")
		delete(payload, "taskIdentityId")
	}
	advisory := map[string]any{}
	for _, key := range []string{
		"schema_id", "match_mode", "task_identity_id", "candidate_task_identity_id", "execution_lane_id",
		"top_score", "threshold", "margin", "abstained", "requires_confirmation", "exact_first", "semantic_auto_merge",
	} {
		if value, ok := result[key]; ok {
			advisory[key] = value
		}
	}
	if candidates := contextPackAnyList(result["candidates"]); len(candidates) > 0 {
		advisory["candidates"] = compactAgentSessionValue(candidates, 2)
	}
	metadata["continuity"] = advisory
	payload["metadata"] = metadata
	payload["continuity"] = advisory
	return advisory, nil
}

func (s *server) memoryContinuityReconcile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	if s.continuity == nil || !s.continuity.enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "continuity_ledger_unavailable", "status": s.continuity.snapshot()})
		return
	}
	payload, err := readOptionalJSONBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
		return
	}
	if !s.enforceOptionalFrontierT1ProjectBoundary(w, r, "continuity") {
		return
	}
	operation := strings.TrimSpace(strings.ToLower(firstNonEmptyStrings(anyToString(payload["operation"]), "reconcile")))
	var response map[string]any
	switch operation {
	case "reconcile":
		response, err = s.continuity.reconcile(payload, true)
	case "merge":
		response, err = s.continuity.mergeTaskIdentities(payload)
	case "split":
		response, err = s.continuity.splitTaskIdentity(payload)
	case "compact":
		actor := clipText(strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["actor"]), anyToString(payload["agent_id"]))), 160)
		reason := clipText(strings.TrimSpace(anyToString(payload["reason"])), 1000)
		if actor == "" || reason == "" {
			err = errors.New("actor and reason are required for lossless compaction")
			break
		}
		var compaction map[string]any
		compaction, err = s.continuity.compact()
		if err == nil {
			response = map[string]any{
				"ok": true, "schema_id": taskIdentityReconciliationContractID,
				"operation": "compact", "match_mode": "lossless_compaction",
				"exact_first": true, "semantic_auto_merge": false, "requires_confirmation": false,
				"abstained": false, "receipt": map[string]any{}, "compaction": compaction,
				"actor": actor, "reason": reason,
			}
		}
	default:
		err = errors.New("operation must be reconcile, merge, split, or compact")
	}
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "invalid_task_identity_reconciliation", "detail": err.Error()})
		return
	}
	response["ledger_status"] = s.continuity.snapshot()
	writeJSON(w, http.StatusOK, attachPayloadFormatContract(taskIdentityReconciliationContractID, response, anyToString(payload["agent_id"]), "task_identity_reconciliation", r.URL.Path))
}
