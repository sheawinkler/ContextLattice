package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	skillDraftContractID       = "skill_draft.v1"
	skillEvaluationContractID  = "skill_evaluation.v1"
	skillExportContractID      = "skill_export.v1"
	skillRetirementContractID  = "skill_retirement.v1"
	skillFoundryStatusSchemaID = "skill_foundry_status.v1"
	skillFoundryTransactionID  = "skill_foundry_transaction.v1"
)

var skillFoundryNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,63}$`)

var skillFoundryStatusRank = map[string]int{"draft": 0, "evaluated": 1, "exported": 2, "retired": 3}

type skillFoundryStore struct {
	mu              sync.RWMutex
	ioMu            sync.Mutex
	enabled         bool
	path            string
	maxBytes        int64
	maxEntries      int
	fsync           bool
	drafts          map[string]map[string]any
	evaluations     []map[string]any
	exports         []map[string]any
	retirements     []map[string]any
	transactions    map[string]map[string]any
	usageReceipts   map[string]map[string]any
	efficacyReviews map[string]map[string]any
	logEntries      int
	parseErrors     int
	compactionCount int
	lastPersistedAt string
	lastError       string
}

func newSkillFoundryStoreFromEnv() (*skillFoundryStore, error) {
	store := &skillFoundryStore{
		enabled:         envBool("CONTEXTLATTICE_SKILL_FOUNDRY_ENABLED", true),
		path:            resolveStoragePath("CONTEXTLATTICE_SKILL_FOUNDRY_PATH", filepath.Join(".data", "orchestrator", "skill_foundry.ndjson")),
		maxBytes:        int64(clampInt(envInt("CONTEXTLATTICE_SKILL_FOUNDRY_MAX_BYTES", 4*1024*1024), 64*1024, 64*1024*1024)),
		maxEntries:      clampInt(envInt("CONTEXTLATTICE_SKILL_FOUNDRY_MAX_ENTRIES", 2000), 20, 20000),
		fsync:           envBool("CONTEXTLATTICE_SKILL_FOUNDRY_FSYNC", true),
		drafts:          map[string]map[string]any{},
		evaluations:     make([]map[string]any, 0, 100),
		exports:         make([]map[string]any, 0, 100),
		retirements:     make([]map[string]any, 0, 100),
		transactions:    map[string]map[string]any{},
		usageReceipts:   map[string]map[string]any{},
		efficacyReviews: map[string]map[string]any{},
	}
	if !store.enabled || strings.TrimSpace(store.path) == "" {
		store.enabled = false
		return store, nil
	}
	if err := prepareOwnerOnlyFile(store.path, strings.TrimSpace(os.Getenv("CONTEXTLATTICE_SKILL_FOUNDRY_PATH")) == ""); err != nil {
		return store, err
	}
	if err := store.load(); err != nil {
		return store, err
	}
	return store, nil
}

func (s *skillFoundryStore) load() error {
	file, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open skill foundry ledger: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			s.parseErrors++
			continue
		}
		if anyToString(row["schema_id"]) == skillFoundryTransactionID {
			if s.loadTransactionLocked(row) {
				s.logEntries++
			}
			continue
		}
		s.applyRowLocked(row)
		s.logEntries++
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan skill foundry ledger: %w", err)
	}
	s.reconcileRetirementsLocked()
	s.trimLocked()
	return nil
}

// A retirement row is written before its terminal draft snapshot. Reconcile it
// on load so a process interruption between those rows cannot revive a draft.
func (s *skillFoundryStore) reconcileRetirementsLocked() {
	for _, retirement := range s.retirements {
		draftID := anyToString(retirement["draft_id"])
		draft := s.drafts[draftID]
		if len(draft) == 0 || anyToString(retirement["draft_fingerprint"]) != anyToString(draft["draft_fingerprint"]) {
			continue
		}
		retiredAt := anyToString(retirement["retired_at"])
		draft["status"] = "retired"
		draft["updated_at"] = retiredAt
		draft["activation"] = map[string]any{
			"state": "inactive", "automatic": false,
			"reason": "Retired drafts cannot be exported or activated.",
		}
		draft["retirement"] = map[string]any{
			"state": "retired", "automatic": false,
			"retirement_id": retirement["retirement_id"], "reason": retirement["reason"],
			"operator": retirement["operator"], "retired_at": retiredAt,
			"deletion_performed": false,
		}
		s.drafts[draftID] = draft
	}
}

func (s *skillFoundryStore) trimLocked() {
	s.evaluations = retainLatestGroupedRows(s.evaluations, "draft_id", s.maxEntries)
	s.exports = retainLatestGroupedRows(s.exports, "draft_id", s.maxEntries)
	s.retirements = retainLatestGroupedRows(s.retirements, "draft_id", s.maxEntries)
	s.trimSkillEfficacyLocked()
	if len(s.drafts) <= s.maxEntries {
		s.trimTransactionsLocked()
		return
	}
	type draftAge struct{ id, updated string }
	ages := make([]draftAge, 0, len(s.drafts))
	for id, draft := range s.drafts {
		ages = append(ages, draftAge{id: id, updated: anyToString(draft["updated_at"])})
	}
	sort.Slice(ages, func(i, j int) bool { return ages[i].updated > ages[j].updated })
	for _, item := range ages[s.maxEntries:] {
		delete(s.drafts, item.id)
	}
	s.trimTransactionsLocked()
}

func (s *skillFoundryStore) trimTransactionsLocked() {
	if len(s.transactions) <= s.maxEntries {
		return
	}
	type transactionAge struct{ id, created string }
	ages := make([]transactionAge, 0, len(s.transactions))
	for id, transaction := range s.transactions {
		ages = append(ages, transactionAge{id: id, created: anyToString(transaction["created_at"])})
	}
	sort.Slice(ages, func(i, j int) bool {
		if ages[i].created == ages[j].created {
			return ages[i].id > ages[j].id
		}
		return ages[i].created > ages[j].created
	})
	for _, item := range ages[s.maxEntries:] {
		delete(s.transactions, item.id)
	}
}

func (s *skillFoundryStore) applyRowLocked(row map[string]any) map[string]any {
	row = cloneMap(row)
	switch anyToString(row["schema_id"]) {
	case skillDraftContractID:
		if id := anyToString(row["draft_id"]); id != "" {
			if existing := s.drafts[id]; len(existing) > 0 && skillFoundryStatusRank[anyToString(existing["status"])] > skillFoundryStatusRank[anyToString(row["status"])] {
				row["status"] = existing["status"]
				row["created_at"] = firstNonEmptyStrings(anyToString(existing["created_at"]), anyToString(row["created_at"]))
				if approval := anyMap(existing["approval"]); len(approval) > 0 {
					row["approval"] = cloneMap(approval)
				}
				if anyToString(existing["status"]) == "retired" {
					row["updated_at"] = existing["updated_at"]
					row["retirement"] = cloneMap(anyMap(existing["retirement"]))
					row["activation"] = cloneMap(anyMap(existing["activation"]))
				}
			}
			s.drafts[id] = cloneMap(row)
		}
	case skillEvaluationContractID:
		s.evaluations = append(s.evaluations, cloneMap(row))
	case skillExportContractID:
		s.exports = append(s.exports, cloneMap(row))
	case skillRetirementContractID:
		s.retirements = append(s.retirements, cloneMap(row))
	case skillUsageReceiptContractID:
		if id := anyToString(row["usage_id"]); id != "" {
			if s.usageReceipts == nil {
				s.usageReceipts = map[string]map[string]any{}
			}
			existing := s.usageReceipts[id]
			if len(existing) == 0 || anyToInt(row["revision"], 0) >= anyToInt(existing["revision"], 0) {
				s.usageReceipts[id] = cloneMap(row)
			}
		}
	case skillEfficacyReviewContractID:
		if id := anyToString(row["review_id"]); id != "" {
			if s.efficacyReviews == nil {
				s.efficacyReviews = map[string]map[string]any{}
			}
			s.efficacyReviews[id] = cloneMap(row)
		}
	}
	return row
}

func (s *skillFoundryStore) loadTransactionLocked(transaction map[string]any) bool {
	id := strings.TrimSpace(anyToString(transaction["transaction_id"]))
	digest := strings.TrimSpace(anyToString(transaction["transaction_digest"]))
	if id == "" || digest == "" {
		s.parseErrors++
		return false
	}
	if existing := s.transactions[id]; len(existing) > 0 {
		if anyToString(existing["transaction_digest"]) != digest {
			s.parseErrors++
		}
		return false
	}
	rows := contextPackAnyList(transaction["rows"])
	if len(rows) > 0 {
		if skillFoundryTransactionDigest(anyToString(transaction["kind"]), anyMap(transaction["metadata"]), rows) != digest {
			s.parseErrors++
			return false
		}
		for _, raw := range rows {
			s.applyRowLocked(anyMap(raw))
		}
	}
	s.transactions[id] = cloneMap(transaction)
	return true
}

func (s *skillFoundryStore) draft(id string) map[string]any {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneMap(s.drafts[strings.TrimSpace(id)])
}

func (s *skillFoundryStore) latestEvaluation(draftID string) map[string]any {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := len(s.evaluations) - 1; i >= 0; i-- {
		if anyToString(s.evaluations[i]["draft_id"]) == draftID {
			return cloneMap(s.evaluations[i])
		}
	}
	return nil
}

func (s *skillFoundryStore) latestRetirement(draftID string) map[string]any {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := len(s.retirements) - 1; i >= 0; i-- {
		if anyToString(s.retirements[i]["draft_id"]) == draftID {
			return cloneMap(s.retirements[i])
		}
	}
	return nil
}

func (s *skillFoundryStore) record(rows ...map[string]any) error {
	if s == nil || !s.enabled {
		return errors.New("skill foundry store disabled")
	}
	s.mu.Lock()
	for index, row := range rows {
		merged := s.applyRowLocked(row)
		rows[index] = merged
		replaceMapContents(row, merged)
	}
	s.trimLocked()
	s.mu.Unlock()
	return s.appendRows(rows...)
}

func skillFoundryTransactionDigest(kind string, metadata map[string]any, rows []any) string {
	raw, err := json.Marshal(map[string]any{"kind": kind, "metadata": metadata, "rows": rows})
	if err != nil {
		return ""
	}
	return "sha256:" + sha256Hex(string(raw))
}

func (s *skillFoundryStore) transaction(id string) map[string]any {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneMap(s.transactions[strings.TrimSpace(id)])
}

// recordTransaction persists the complete lifecycle transition as one NDJSON
// record. A torn append is ignored on reload, while a committed replay returns
// the original receipt without adding duplicate evaluations.
func (s *skillFoundryStore) recordTransaction(transactionID, kind string, metadata map[string]any, rows ...map[string]any) (map[string]any, bool, error) {
	if s == nil || !s.enabled {
		return nil, false, errors.New("skill foundry store disabled")
	}
	transactionID = strings.TrimSpace(transactionID)
	kind = strings.TrimSpace(kind)
	if transactionID == "" || len(transactionID) > 160 || kind == "" || len(kind) > 80 || len(rows) == 0 || len(rows) > 8 {
		return nil, false, errors.New("invalid bounded skill Foundry transaction")
	}
	rowValues := make([]any, 0, len(rows))
	for _, row := range rows {
		if len(row) == 0 {
			return nil, false, errors.New("skill Foundry transaction contains an empty row")
		}
		rowValues = append(rowValues, cloneMap(row))
	}
	metadata = cloneMap(metadata)
	digest := skillFoundryTransactionDigest(kind, metadata, rowValues)
	if digest == "" {
		return nil, false, errors.New("skill Foundry transaction digest failed")
	}

	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	s.mu.RLock()
	existing := cloneMap(s.transactions[transactionID])
	s.mu.RUnlock()
	if len(existing) > 0 {
		if anyToString(existing["transaction_digest"]) != digest {
			return existing, true, errors.New("skill Foundry transaction id conflicts with existing material")
		}
		return existing, true, nil
	}
	now := nowUTCISO()
	transaction := map[string]any{
		"schema_id": skillFoundryTransactionID, "version": 1,
		"transaction_id": transactionID, "transaction_digest": digest,
		"kind": kind, "created_at": now, "metadata": metadata, "rows": rowValues,
	}
	file, err := openOwnerOnlyAppend(s.path, false)
	if err != nil {
		s.setError(err)
		return nil, false, err
	}
	if err := json.NewEncoder(file).Encode(transaction); err != nil {
		_ = file.Close()
		s.setError(err)
		return nil, false, err
	}
	if s.fsync {
		if err := file.Sync(); err != nil {
			_ = file.Close()
			s.setError(err)
			return nil, false, err
		}
	}
	if err := file.Close(); err != nil {
		s.setError(err)
		return nil, false, err
	}

	s.mu.Lock()
	for _, raw := range rowValues {
		s.applyRowLocked(anyMap(raw))
	}
	s.transactions[transactionID] = cloneMap(transaction)
	s.trimLocked()
	s.logEntries++
	s.lastPersistedAt = now
	s.lastError = ""
	s.mu.Unlock()
	if info, statErr := os.Stat(s.path); statErr == nil && info.Size() > s.maxBytes {
		if compactErr := s.compactLockedIO(); compactErr != nil {
			return transaction, false, compactErr
		}
	}
	return cloneMap(transaction), false, nil
}

func (s *skillFoundryStore) appendRows(rows ...map[string]any) error {
	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	file, err := openOwnerOnlyAppend(s.path, false)
	if err != nil {
		s.setError(err)
		return err
	}
	encoder := json.NewEncoder(file)
	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		if err := encoder.Encode(row); err != nil {
			_ = file.Close()
			s.setError(err)
			return err
		}
	}
	if s.fsync {
		if err := file.Sync(); err != nil {
			_ = file.Close()
			s.setError(err)
			return err
		}
	}
	if err := file.Close(); err != nil {
		s.setError(err)
		return err
	}
	s.mu.Lock()
	s.logEntries += len(rows)
	s.lastPersistedAt = nowUTCISO()
	s.lastError = ""
	s.mu.Unlock()
	if info, err := os.Stat(s.path); err == nil && info.Size() > s.maxBytes {
		return s.compactLockedIO()
	}
	return nil
}

func (s *skillFoundryStore) compact() error {
	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	return s.compactLockedIO()
}

func (s *skillFoundryStore) compactLockedIO() error {
	s.mu.RLock()
	drafts := make([]map[string]any, 0, len(s.drafts))
	for _, draft := range s.drafts {
		drafts = append(drafts, cloneMap(draft))
	}
	evaluations := append([]map[string]any{}, s.evaluations...)
	exports := append([]map[string]any{}, s.exports...)
	retirements := append([]map[string]any{}, s.retirements...)
	usageReceipts := make([]map[string]any, 0, len(s.usageReceipts))
	for _, receipt := range s.usageReceipts {
		usageReceipts = append(usageReceipts, cloneMap(receipt))
	}
	efficacyReviews := make([]map[string]any, 0, len(s.efficacyReviews))
	for _, review := range s.efficacyReviews {
		efficacyReviews = append(efficacyReviews, cloneMap(review))
	}
	transactions := make([]map[string]any, 0, len(s.transactions))
	for _, transaction := range s.transactions {
		receipt := cloneMap(transaction)
		// Usage chains are revisioned snapshots. Their transaction row is the
		// only exact replay material for an earlier stage once the latest
		// receipt supersedes it, so retain that bounded row during compaction.
		switch anyToString(receipt["kind"]) {
		case "skill_efficacy_usage", "skill_efficacy_review":
		default:
			delete(receipt, "rows")
		}
		receipt["compacted"] = true
		transactions = append(transactions, receipt)
	}
	s.mu.RUnlock()
	sort.Slice(drafts, func(i, j int) bool { return anyToString(drafts[i]["draft_id"]) < anyToString(drafts[j]["draft_id"]) })
	sort.Slice(transactions, func(i, j int) bool {
		return anyToString(transactions[i]["transaction_id"]) < anyToString(transactions[j]["transaction_id"])
	})
	sort.Slice(usageReceipts, func(i, j int) bool {
		return anyToString(usageReceipts[i]["usage_id"]) < anyToString(usageReceipts[j]["usage_id"])
	})
	sort.Slice(efficacyReviews, func(i, j int) bool {
		return anyToString(efficacyReviews[i]["review_id"]) < anyToString(efficacyReviews[j]["review_id"])
	})
	history := append(evaluations, exports...)
	history = append(history, retirements...)
	rows := append(drafts, history...)
	rows = append(rows, usageReceipts...)
	rows = append(rows, efficacyReviews...)
	rows = append(rows, transactions...)
	tmp := s.path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	for _, row := range rows {
		if err := encoder.Encode(row); err != nil {
			_ = file.Close()
			_ = os.Remove(tmp)
			return err
		}
	}
	if s.fsync {
		if err := file.Sync(); err != nil {
			_ = file.Close()
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	s.mu.Lock()
	s.compactionCount++
	s.logEntries = len(rows)
	s.lastPersistedAt = nowUTCISO()
	s.lastError = ""
	s.mu.Unlock()
	return nil
}

func (s *skillFoundryStore) setError(err error) {
	if s == nil || err == nil {
		return
	}
	s.mu.Lock()
	s.lastError = clipText(err.Error(), 500)
	s.mu.Unlock()
}

func (s *skillFoundryStore) snapshot() map[string]any {
	if s == nil {
		return map[string]any{"schema_id": skillFoundryStatusSchemaID, "enabled": false, "draft_count": 0, "drafts": []any{}}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	statusCounts := map[string]int{}
	drafts := make([]map[string]any, 0, len(s.drafts))
	for _, draft := range s.drafts {
		statusCounts[anyToString(draft["status"])]++
		drafts = append(drafts, cloneMap(draft))
	}
	sort.Slice(drafts, func(i, j int) bool {
		return anyToString(drafts[i]["updated_at"]) > anyToString(drafts[j]["updated_at"])
	})
	items := make([]any, 0, minInt(len(drafts), 20))
	for _, draft := range drafts[:minInt(len(drafts), 20)] {
		copyDraft := cloneMap(draft)
		delete(copyDraft, "skill_markdown")
		items = append(items, copyDraft)
	}
	reviews := make([]map[string]any, 0, len(s.efficacyReviews))
	for _, review := range s.efficacyReviews {
		reviews = append(reviews, cloneMap(review))
	}
	sort.Slice(reviews, func(i, j int) bool {
		return anyToString(reviews[i]["created_at"]) > anyToString(reviews[j]["created_at"])
	})
	reviewItems := make([]any, 0, minInt(len(reviews), 20))
	for _, review := range reviews[:minInt(len(reviews), 20)] {
		proposal := anyMap(review["proposal"])
		artifact := anyMap(review["artifact"])
		reviewItems = append(reviewItems, map[string]any{
			"review_id": review["review_id"], "review_digest": review["review_digest"],
			"project": review["project"], "skill_id": review["skill_id"], "name": review["name"],
			"status": review["status"], "decision": review["decision"], "created_at": review["created_at"],
			"proposal": map[string]any{
				"kind": proposal["kind"], "summary": proposal["summary"],
				"delivery": proposal["delivery"], "content_digest": proposal["content_digest"],
			},
			"artifact": map[string]any{"mode": artifact["mode"], "state": artifact["state"]},
		})
	}
	return map[string]any{
		"schema_id": skillFoundryStatusSchemaID, "version": 1, "enabled": s.enabled,
		"activation_state": "inactive", "automatic_activation": false,
		"draft_count": len(s.drafts), "evaluation_count": len(s.evaluations), "export_count": len(s.exports), "retirement_count": len(s.retirements),
		"transaction_count": len(s.transactions), "skill_usage_count": len(s.usageReceipts), "efficacy_review_count": len(s.efficacyReviews),
		"status_counts": statusCounts, "drafts": items, "efficacy_reviews": reviewItems,
		"storage":    map[string]any{"enabled": s.enabled, "max_bytes": s.maxBytes, "max_entries": s.maxEntries, "log_entries": s.logEntries, "parse_errors": s.parseErrors, "compaction_count": s.compactionCount, "last_persisted_at": s.lastPersistedAt, "last_error": s.lastError},
		"updated_at": nowUTCISO(),
	}
}

func normalizeFoundryStrings(raw any, limit int, maxBytes int) []string {
	values := []string{}
	seen := map[string]struct{}{}
	for _, item := range contextPackAnyList(raw) {
		value := clipText(strings.Join(strings.Fields(anyToString(item)), " "), maxBytes)
		if value == "" {
			value = clipText(strings.Join(strings.Fields(anyToString(anyMap(item)["text"])), " "), maxBytes)
		}
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		values = append(values, value)
		if len(values) >= limit {
			break
		}
	}
	return values
}

func foundryWorkflowSignature(steps, checks []string) string {
	encoded, _ := json.Marshal(map[string]any{"steps": steps, "checks": checks})
	return sha256Hex(string(encoded))
}

func (s *server) buildSkillDraft(payload map[string]any) (map[string]any, error) {
	project, err := sanitizeMemoryProject(firstNonEmptyStrings(anyToString(payload["project"]), "contextlattice"))
	if err != nil {
		return nil, err
	}
	name := strings.ToLower(strings.TrimSpace(anyToString(payload["name"])))
	name = strings.ReplaceAll(name, "_", "-")
	if !skillFoundryNamePattern.MatchString(name) {
		return nil, errors.New("name must be 2-64 lowercase letters, digits, or hyphens")
	}
	description := clipText(strings.Join(strings.Fields(anyToString(payload["description"])), " "), 500)
	if description == "" {
		return nil, errors.New("description is required")
	}
	minimumRuns := clampInt(anyToInt(payload["minimum_verified_runs"], 3), 3, 20)
	skillVersion := clampInt(anyToInt(payload["skill_version"], 1), 1, 100000)
	supersedes := clipText(strings.TrimSpace(anyToString(payload["supersedes"])), 160)
	sourceCandidateID := clipText(strings.TrimSpace(anyToString(payload["source_candidate_id"])), 160)
	sourceWorkflowSignature := strings.ToLower(strings.TrimSpace(anyToString(payload["source_workflow_signature"])))
	if sourceWorkflowSignature != "" && !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(sourceWorkflowSignature) {
		return nil, errors.New("source_workflow_signature must be sha256:<64-hex>")
	}
	prerequisites := normalizeFoundryStrings(payload["prerequisites"], 16, 400)
	rollback := normalizeFoundryStrings(payload["rollback"], 16, 400)
	sideEffects := normalizeFoundryStrings(payload["side_effects"], 16, 400)
	platformConstraints := normalizeFoundryStrings(payload["platform_constraints"], 16, 400)
	verificationCommands, err := normalizeFoundryVerificationCommands(payload["verification_commands"])
	if err != nil {
		return nil, err
	}
	workflowMaterial := map[string]any{
		"source_candidate_id": sourceCandidateID, "source_workflow_signature": sourceWorkflowSignature,
		"prerequisites": stringSliceAny(prerequisites), "rollback": stringSliceAny(rollback),
		"side_effects": stringSliceAny(sideEffects), "platform_constraints": stringSliceAny(platformConstraints),
		"verification_commands": verificationCommands,
	}
	workflowMaterialPresent := sourceCandidateID != "" || sourceWorkflowSignature != "" || len(prerequisites) > 0 || len(rollback) > 0 || len(sideEffects) > 0 || len(platformConstraints) > 0 || len(verificationCommands) > 0
	type runGroup struct {
		steps, checks []string
		runs          []map[string]any
	}
	groups := map[string]*runGroup{}
	for _, raw := range contextPackAnyList(firstNonNil(payload["workflow_runs"], payload["runs"])) {
		run := anyMap(raw)
		if !anyToBool(firstPresentAny(run["verified"], run["verification_passed"])) || !anyToBool(firstPresentAny(run["success"], run["passed"])) {
			continue
		}
		steps := normalizeFoundryStrings(firstPresentAny(run["steps"], run["workflow"]), 24, 500)
		checks := normalizeFoundryStrings(run["checks"], 16, 400)
		refs := normalizeFoundryStrings(firstPresentAny(run["evidence_refs"], run["provenance"]), 12, 500)
		if len(steps) == 0 || len(checks) == 0 || len(refs) == 0 {
			continue
		}
		run["foundry_evidence_refs"] = stringSliceAny(refs)
		signature := foundryWorkflowSignature(steps, checks)
		group := groups[signature]
		if group == nil {
			group = &runGroup{steps: steps, checks: checks}
			groups[signature] = group
		}
		group.runs = append(group.runs, run)
	}
	var dominant *runGroup
	dominantSignature := ""
	for signature, group := range groups {
		if dominant == nil || len(group.runs) > len(dominant.runs) || (len(group.runs) == len(dominant.runs) && signature < dominantSignature) {
			dominant, dominantSignature = group, signature
		}
	}
	if dominant == nil || len(dominant.runs) < minimumRuns {
		return nil, fmt.Errorf("at least %d verified successful runs with the same bounded workflow are required", minimumRuns)
	}
	trainingRunIDs := []any{}
	provenance := []any{}
	seenRuns := map[string]struct{}{}
	for index, run := range dominant.runs {
		id := clipText(strings.TrimSpace(firstNonEmptyStrings(anyToString(run["run_id"]), anyToString(run["id"]))), 160)
		if id == "" {
			id = fmt.Sprintf("run_%s_%d", dominantSignature[:12], index+1)
		}
		if _, ok := seenRuns[id]; ok {
			continue
		}
		seenRuns[id] = struct{}{}
		trainingRunIDs = append(trainingRunIDs, id)
		refs := normalizeFoundryStrings(run["foundry_evidence_refs"], 12, 500)
		provenance = append(provenance, map[string]any{"run_id": id, "evidence_refs": refs})
	}
	if len(trainingRunIDs) < minimumRuns {
		return nil, errors.New("verified workflow runs must have distinct run identities")
	}
	collision := foundrySkillCollision(name)
	now := nowUTCISO()
	draftSeed, _ := json.Marshal(map[string]any{
		"project": project, "name": name, "description": description, "skill_version": skillVersion,
		"supersedes": supersedes, "workflow_signature": dominantSignature,
		"steps": dominant.steps, "checks": dominant.checks, "workflow_material": workflowMaterial,
	})
	draftFingerprint := sha256Hex(string(draftSeed))
	draftID := "skilldraft_" + draftFingerprint[:24]
	createdAt := now
	existing := s.skillFoundry.draft(draftID)
	if len(existing) > 0 {
		createdAt = firstNonEmptyStrings(anyToString(existing["created_at"]), now)
	}
	draft := map[string]any{
		"schema_id": skillDraftContractID, "version": 1, "draft_id": draftID, "project": project,
		"name": name, "description": description, "status": "draft", "created_at": createdAt, "updated_at": now,
		"skill_version": skillVersion, "supersedes": supersedes,
		"draft_fingerprint":  draftFingerprint,
		"workflow_signature": dominantSignature, "steps": stringSliceAny(dominant.steps), "checks": stringSliceAny(dominant.checks),
		"minimum_verified_runs": minimumRuns, "verified_run_count": len(trainingRunIDs), "training_run_ids": trainingRunIDs,
		"provenance": provenance, "existing_skill_collision": collision,
		"evaluation": map[string]any{"required": true, "minimum_holdouts": 3, "training_holdout_separation": true},
		"activation": map[string]any{"state": "inactive", "automatic": false, "reason": "Drafts require independent holdouts and explicit human approval before export."},
		"retirement": map[string]any{"automatic": false, "superseded_skill": supersedes, "reason": "Retirement remains an explicit Skills Index action."},
	}
	if workflowMaterialPresent {
		draft["workflow_material"] = workflowMaterial
	}
	draft["skill_markdown"] = renderFoundrySkillMarkdown(draft)
	if len(existing) > 0 && skillFoundryStatusRank[anyToString(existing["status"])] > skillFoundryStatusRank[anyToString(draft["status"])] {
		draft["status"] = existing["status"]
		if approval := anyMap(existing["approval"]); len(approval) > 0 {
			draft["approval"] = cloneMap(approval)
		}
		if anyToString(existing["status"]) == "retired" {
			draft["updated_at"] = existing["updated_at"]
			draft["retirement"] = cloneMap(anyMap(existing["retirement"]))
			draft["activation"] = cloneMap(anyMap(existing["activation"]))
		}
	}
	return draft, nil
}

func stringSliceAny(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

func foundrySkillCollision(name string) map[string]any {
	payload := nativeSkillsIndexSearch(skillsQuarantineSearchRequest{Query: name, Limit: 100, JSON: true})
	matches := []any{}
	for _, raw := range contextPackAnyList(payload["results"]) {
		row := anyMap(raw)
		if strings.EqualFold(anyToString(row["name"]), name) {
			matches = append(matches, map[string]any{"name": row["name"], "source": row["source"]})
		}
	}
	return map[string]any{"detected": len(matches) > 0, "matches": matches, "index": "native_active_skills"}
}

func renderFoundrySkillMarkdown(draft map[string]any) string {
	name := anyToString(draft["name"])
	description := strings.ReplaceAll(anyToString(draft["description"]), "\n", " ")
	lines := []string{"---", "name: " + name, "description: " + strconv.Quote(description), "version: " + anyToString(draft["skill_version"]), "---", "", "# " + name, "", "## Trigger", "", description, "", "## Workflow", ""}
	for index, raw := range contextPackAnyList(draft["steps"]) {
		lines = append(lines, fmt.Sprintf("%d. %s", index+1, anyToString(raw)))
	}
	lines = append(lines, "", "## Verification", "")
	checks := contextPackAnyList(draft["checks"])
	if len(checks) == 0 {
		lines = append(lines, "- Verify the workflow outcome with deterministic evidence.")
	} else {
		for _, raw := range checks {
			lines = append(lines, "- "+anyToString(raw))
		}
	}
	lines = append(lines, "", "## Boundaries", "", "- Preserve user work and stop on contradictory evidence.", "- Do not claim completion without matching verification.", "")
	material := anyMap(draft["workflow_material"])
	appendSection := func(title string, raw any) {
		values := contextPackAnyList(raw)
		if len(values) == 0 {
			return
		}
		lines = append(lines, "## "+title, "")
		for _, value := range values {
			lines = append(lines, "- "+anyToString(value))
		}
		lines = append(lines, "")
	}
	appendSection("Prerequisites", material["prerequisites"])
	appendSection("Rollback", material["rollback"])
	appendSection("Side Effects", material["side_effects"])
	appendSection("Platform Constraints", material["platform_constraints"])
	commands := contextPackAnyList(material["verification_commands"])
	if len(commands) > 0 {
		lines = append(lines, "## Verification Commands", "")
		for _, raw := range commands {
			command := anyMap(raw)
			lines = append(lines, "- `"+anyToString(command["command"])+"` ("+anyToString(command["digest"])+")")
		}
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

func normalizeFoundryVerificationCommands(raw any) ([]any, error) {
	values := make([]any, 0, 16)
	seen := map[string]struct{}{}
	for _, item := range contextPackAnyList(raw) {
		row := anyMap(item)
		command := ""
		digest := ""
		if len(row) > 0 {
			command = clipText(strings.Join(strings.Fields(anyToString(row["command"])), " "), 1000)
			digest = strings.ToLower(strings.TrimSpace(anyToString(row["digest"])))
		} else {
			command = clipText(strings.Join(strings.Fields(anyToString(item)), " "), 1000)
		}
		if command == "" {
			continue
		}
		expected := "sha256:" + sha256Hex(command)
		if digest == "" {
			digest = expected
		}
		if digest != expected {
			return nil, errors.New("verification command digest does not match its bounded command")
		}
		if _, exists := seen[digest]; exists {
			continue
		}
		seen[digest] = struct{}{}
		values = append(values, map[string]any{"command": command, "digest": digest})
		if len(values) >= 16 {
			break
		}
	}
	return values, nil
}

func (s *server) evaluateSkillDraft(payload map[string]any) (map[string]any, map[string]any, error) {
	draftID := strings.TrimSpace(anyToString(payload["draft_id"]))
	if draftID == "" {
		return nil, nil, errors.New("draft_id is required")
	}
	draft := s.skillFoundry.draft(draftID)
	if len(draft) == 0 {
		return nil, nil, errors.New("draft not found")
	}
	return s.evaluateSkillDraftRecord(draft, payload)
}

func (s *server) evaluateSkillDraftRecord(draft map[string]any, payload map[string]any) (map[string]any, map[string]any, error) {
	draft = cloneMap(draft)
	draftID := strings.TrimSpace(anyToString(draft["draft_id"]))
	if draftID == "" {
		return nil, nil, errors.New("draft_id is required")
	}
	if anyToString(draft["status"]) == "exported" {
		return nil, draft, errors.New("exported drafts are immutable; create a new version for further evaluation")
	}
	if anyToString(draft["status"]) == "retired" {
		return nil, draft, errors.New("retired drafts are terminal and cannot be evaluated")
	}
	training := map[string]struct{}{}
	for _, raw := range contextPackAnyList(draft["training_run_ids"]) {
		training[anyToString(raw)] = struct{}{}
	}
	expectedSignature := anyToString(draft["workflow_signature"])
	minimum := clampInt(anyToInt(payload["minimum_holdouts"], 3), 3, 20)
	holdouts := contextPackAnyList(payload["holdouts"])
	if len(holdouts) < minimum {
		return nil, draft, fmt.Errorf("at least %d independent holdouts are required", minimum)
	}
	results := []any{}
	passed := 0
	seen := map[string]struct{}{}
	for index, raw := range holdouts {
		row := anyMap(raw)
		id := clipText(strings.TrimSpace(firstNonEmptyStrings(anyToString(row["holdout_id"]), anyToString(row["run_id"]), anyToString(row["id"]))), 160)
		if id == "" {
			return nil, draft, fmt.Errorf("holdout %d requires an explicit holdout_id or run_id", index+1)
		}
		if _, leaked := training[id]; leaked {
			return nil, draft, fmt.Errorf("holdout %s overlaps training evidence", id)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, draft, fmt.Errorf("holdout %s is duplicated", id)
		}
		seen[id] = struct{}{}
		steps := normalizeFoundryStrings(firstPresentAny(row["steps"], row["actual_steps"]), 24, 500)
		checks := normalizeFoundryStrings(row["checks"], 16, 400)
		evidenceRefs := normalizeFoundryStrings(firstPresentAny(row["evidence_refs"], row["provenance"]), 12, 500)
		workflowMatch := len(steps) > 0 && len(checks) > 0 && foundryWorkflowSignature(steps, checks) == expectedSignature
		verified := anyToBool(firstPresentAny(row["verified"], row["verification_passed"]))
		success := anyToBool(firstPresentAny(row["success"], row["passed"]))
		checksPassed := anyToBool(firstPresentAny(row["checks_passed"], row["acceptance_passed"]))
		if len(evidenceRefs) == 0 {
			return nil, draft, fmt.Errorf("holdout %s requires bounded evidence_refs", id)
		}
		rowPassed := verified && success && checksPassed && workflowMatch
		if rowPassed {
			passed++
		}
		results = append(results, map[string]any{"holdout_id": id, "passed": rowPassed, "verified": verified, "success": success, "checks_passed": checksPassed, "workflow_match": workflowMatch, "evidence_refs": stringSliceAny(evidenceRefs)})
	}
	passRate := roundFloat(float64(passed)/float64(len(holdouts)), 4)
	evaluationPassed := passed == len(holdouts) && len(holdouts) >= minimum
	recommendation := "revise"
	if evaluationPassed {
		recommendation = "human_review"
		draft["status"] = "evaluated"
	}
	draft["updated_at"] = nowUTCISO()
	evaluation := map[string]any{
		"schema_id": skillEvaluationContractID, "version": 1,
		"evaluation_id": "skilleval_" + sha256Hex(draftID + "\x00" + nowUTCISO() + "\x00" + anyToString(results))[:24],
		"draft_id":      draftID, "draft_fingerprint": draft["draft_fingerprint"], "project": draft["project"], "evaluated_at": nowUTCISO(),
		"passed": evaluationPassed, "pass_rate": passRate, "holdout_count": len(holdouts), "minimum_holdouts": minimum,
		"training_holdout_overlap": false, "holdout_evidence_required": true, "results": results, "recommendation": recommendation,
		"activation_state": "inactive", "human_approval_required": true,
	}
	return evaluation, draft, nil
}

func (s *server) exportSkillDraft(payload map[string]any) (map[string]any, map[string]any, error) {
	draftID := strings.TrimSpace(anyToString(payload["draft_id"]))
	if draftID == "" {
		return nil, nil, errors.New("draft_id is required")
	}
	draft := s.skillFoundry.draft(draftID)
	if len(draft) == 0 {
		return nil, nil, errors.New("draft not found")
	}
	if anyToString(draft["status"]) == "retired" {
		return nil, draft, errors.New("retired drafts are terminal and cannot be exported")
	}
	evaluation := s.skillFoundry.latestEvaluation(draftID)
	if !anyToBool(evaluation["passed"]) {
		return nil, draft, errors.New("a passing independent holdout evaluation is required")
	}
	if anyToString(evaluation["draft_fingerprint"]) != anyToString(draft["draft_fingerprint"]) {
		return nil, draft, errors.New("the passing evaluation does not match the current draft fingerprint")
	}
	collision := foundrySkillCollision(anyToString(draft["name"]))
	draft["existing_skill_collision"] = collision
	if anyToBool(collision["detected"]) && !foundryCollisionSuperseded(collision, anyToString(draft["supersedes"])) {
		return nil, draft, errors.New("an existing skill with this name was detected; supersedes must name that skill before export")
	}
	if !anyToBool(payload["human_approved"]) {
		return nil, draft, errors.New("human_approved=true is required")
	}
	approver := clipText(strings.TrimSpace(anyToString(payload["approver"])), 160)
	if approver == "" {
		return nil, draft, errors.New("approver is required")
	}
	now := nowUTCISO()
	draft["status"] = "exported"
	draft["updated_at"] = now
	draft["approval"] = map[string]any{"human_approved": true, "approver": approver, "approved_at": now}
	content := anyToString(draft["skill_markdown"])
	export := map[string]any{
		"schema_id": skillExportContractID, "version": 1,
		"export_id": "skillexport_" + sha256Hex(draftID + "\x00" + approver + "\x00" + now)[:24],
		"draft_id":  draftID, "draft_fingerprint": draft["draft_fingerprint"], "project": draft["project"], "name": draft["name"], "exported_at": now,
		"skill_version": draft["skill_version"], "supersedes": draft["supersedes"],
		"human_approved": true, "approver": approver, "skill_markdown": content,
		"content_sha256": sha256Hex(content), "suggested_relative_path": anyToString(draft["name"]) + "/SKILL.md",
		"activation_state": "inactive", "automatic_activation": false,
		"retirement":        draft["retirement"],
		"installation_note": "Review and install this exported artifact through the normal Skills Index workflow.",
	}
	return export, draft, nil
}

func (s *server) retireSkillDraft(payload map[string]any) (map[string]any, map[string]any, bool, error) {
	draftID := strings.TrimSpace(anyToString(payload["draft_id"]))
	if draftID == "" {
		return nil, nil, false, errors.New("draft_id is required")
	}
	draft := s.skillFoundry.draft(draftID)
	if len(draft) == 0 {
		return nil, nil, false, errors.New("draft not found")
	}
	reason := clipText(strings.TrimSpace(anyToString(payload["reason"])), 500)
	if reason == "" {
		return nil, draft, false, errors.New("reason is required")
	}
	operator := clipText(strings.TrimSpace(anyToString(payload["operator"])), 160)
	if operator == "" {
		return nil, draft, false, errors.New("operator is required")
	}
	if strings.EqualFold(anyToString(anyMap(draft["activation"])["state"]), "active") {
		return nil, draft, false, errors.New("active skills must be deactivated before their Foundry draft can be retired")
	}
	if existing := s.skillFoundry.latestRetirement(draftID); len(existing) > 0 {
		if anyToString(existing["reason"]) != reason || anyToString(existing["operator"]) != operator {
			return nil, draft, false, errors.New("draft is already retired with immutable reason and operator evidence")
		}
		return existing, draft, false, nil
	}
	now := nowUTCISO()
	retirementID := "skillretire_" + sha256Hex(draftID + "\x00" + anyToString(draft["draft_fingerprint"]) + "\x00" + operator + "\x00" + reason)[:24]
	retirement := map[string]any{
		"schema_id": skillRetirementContractID, "version": 1,
		"retirement_id": retirementID, "draft_id": draftID, "draft_fingerprint": draft["draft_fingerprint"],
		"project": draft["project"], "name": draft["name"], "status": "retired", "reason": reason,
		"operator": operator, "retired_at": now, "automatic": false, "deletion_performed": false,
		"runtime_mutation": false, "activation_state": "inactive",
	}
	draft["status"] = "retired"
	draft["updated_at"] = now
	draft["activation"] = map[string]any{"state": "inactive", "automatic": false, "reason": "Retired drafts cannot be exported or activated."}
	draft["retirement"] = map[string]any{
		"state": "retired", "automatic": false, "retirement_id": retirementID,
		"reason": reason, "operator": operator, "retired_at": now, "deletion_performed": false,
	}
	return retirement, draft, true, nil
}

func foundryCollisionSuperseded(collision map[string]any, supersedes string) bool {
	target := strings.ToLower(strings.TrimSpace(strings.SplitN(supersedes, "@", 2)[0]))
	if target == "" {
		return false
	}
	for _, raw := range contextPackAnyList(collision["matches"]) {
		if strings.EqualFold(anyToString(anyMap(raw)["name"]), target) {
			return true
		}
	}
	return false
}

func (s *server) memorySkillFoundryDraft(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	s.handleSkillFoundryDraft(w, r, false)
}
func (s *server) toolsSkillFoundryDraft(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareToolHeaders(w, r, "/tools/skill_foundry_draft"); !ok {
		return
	}
	s.handleSkillFoundryDraft(w, r, true)
}
func (s *server) handleSkillFoundryDraft(w http.ResponseWriter, r *http.Request, tool bool) {
	payload, err := readOptionalJSONBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
		return
	}
	s.skillLifecycleMu.Lock()
	defer s.skillLifecycleMu.Unlock()
	draft, err := s.buildSkillDraft(payload)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "skill_draft_failed", "detail": err.Error()})
		return
	}
	if err := s.skillFoundry.record(draft); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "skill_draft_persist_failed", "detail": err.Error()})
		return
	}
	response := map[string]any{"ok": true, "schema_id": skillDraftContractID, "draft": draft, "recorded": true}
	if tool {
		response["tool"] = "skill_foundry_draft"
	}
	writeJSON(w, http.StatusOK, attachPayloadFormatContract(skillDraftContractID, response, anyToString(payload["agent_id"]), "skill_draft", r.URL.Path))
}

func (s *server) memorySkillFoundryEvaluate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	s.handleSkillFoundryEvaluate(w, r, false)
}
func (s *server) toolsSkillFoundryEvaluate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareToolHeaders(w, r, "/tools/skill_foundry_evaluate"); !ok {
		return
	}
	s.handleSkillFoundryEvaluate(w, r, true)
}
func (s *server) handleSkillFoundryEvaluate(w http.ResponseWriter, r *http.Request, tool bool) {
	payload, err := readOptionalJSONBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
		return
	}
	s.skillLifecycleMu.Lock()
	defer s.skillLifecycleMu.Unlock()
	evaluation, draft, err := s.evaluateSkillDraft(payload)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "skill_evaluation_failed", "detail": err.Error()})
		return
	}
	if err := s.skillFoundry.record(evaluation, draft); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "skill_evaluation_persist_failed", "detail": err.Error()})
		return
	}
	response := map[string]any{"ok": true, "schema_id": skillEvaluationContractID, "evaluation": evaluation, "draft": draft, "recorded": true}
	if tool {
		response["tool"] = "skill_foundry_evaluate"
	}
	writeJSON(w, http.StatusOK, attachPayloadFormatContract(skillEvaluationContractID, response, anyToString(payload["agent_id"]), "skill_evaluation", r.URL.Path))
}

func (s *server) memorySkillFoundryExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	s.handleSkillFoundryExport(w, r, false)
}

func (s *server) memorySkillFoundryRetire(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	payload, err := readOptionalJSONBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
		return
	}
	s.skillLifecycleMu.Lock()
	defer s.skillLifecycleMu.Unlock()
	retirement, draft, created, err := s.retireSkillDraft(payload)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "skill_retirement_failed", "detail": err.Error()})
		return
	}
	if created {
		if err := s.skillFoundry.record(retirement, draft); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "skill_retirement_persist_failed", "detail": err.Error()})
			return
		}
	}
	response := map[string]any{"ok": true, "schema_id": skillRetirementContractID, "retirement": retirement, "draft": draft, "recorded": created, "idempotent": !created}
	writeJSON(w, http.StatusOK, attachPayloadFormatContract(skillRetirementContractID, response, anyToString(payload["agent_id"]), "skill_retirement", r.URL.Path))
}
func (s *server) toolsSkillFoundryExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareToolHeaders(w, r, "/tools/skill_foundry_export"); !ok {
		return
	}
	s.handleSkillFoundryExport(w, r, true)
}
func (s *server) handleSkillFoundryExport(w http.ResponseWriter, r *http.Request, tool bool) {
	payload, err := readOptionalJSONBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
		return
	}
	s.skillLifecycleMu.Lock()
	defer s.skillLifecycleMu.Unlock()
	exported, draft, err := s.exportSkillDraft(payload)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "skill_export_failed", "detail": err.Error()})
		return
	}
	if err := s.skillFoundry.record(exported, draft); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "skill_export_persist_failed", "detail": err.Error()})
		return
	}
	response := map[string]any{"ok": true, "schema_id": skillExportContractID, "export": exported, "draft": draft, "recorded": true}
	if tool {
		response["tool"] = "skill_foundry_export"
	}
	writeJSON(w, http.StatusOK, attachPayloadFormatContract(skillExportContractID, response, anyToString(payload["agent_id"]), "skill_export", r.URL.Path))
}

func (s *server) telemetrySkillFoundry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.skillFoundry.snapshot())
}
