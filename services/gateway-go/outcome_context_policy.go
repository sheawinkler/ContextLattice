package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	contextPolicyCandidateContractID  = "context_policy_candidate.v1"
	contextPolicyEvaluationContractID = "context_policy_evaluation.v1"
	contextPolicyStatusSchemaID       = "context_policy_status.v1"
)

var contextPolicyPhases = map[string]int{
	"candidate":   0,
	"shadow":      1,
	"canary":      2,
	"promoted":    3,
	"rolled_back": 3,
}

var errContextPolicyTransitionConflict = errors.New("context policy transition conflict")

type contextPolicyStore struct {
	mu              sync.RWMutex
	ioMu            sync.Mutex
	enabled         bool
	path            string
	maxBytes        int64
	maxEntries      int
	fsync           bool
	candidates      map[string]map[string]any
	evaluations     []map[string]any
	logEntries      int
	parseErrors     int
	compactionCount int
	lastPersistedAt string
	lastError       string
}

func newContextPolicyStoreFromEnv() (*contextPolicyStore, error) {
	store := &contextPolicyStore{
		enabled:     envBool("CONTEXTLATTICE_CONTEXT_POLICY_ENABLED", true),
		path:        resolveStoragePath("CONTEXTLATTICE_CONTEXT_POLICY_PATH", filepath.Join(".data", "orchestrator", "context_policy.ndjson")),
		maxBytes:    int64(clampInt(envInt("CONTEXTLATTICE_CONTEXT_POLICY_MAX_BYTES", 2*1024*1024), 64*1024, 64*1024*1024)),
		maxEntries:  clampInt(envInt("CONTEXTLATTICE_CONTEXT_POLICY_MAX_ENTRIES", 1000), 20, 20000),
		fsync:       envBool("CONTEXTLATTICE_CONTEXT_POLICY_FSYNC", true),
		candidates:  map[string]map[string]any{},
		evaluations: make([]map[string]any, 0, 100),
	}
	if !store.enabled || strings.TrimSpace(store.path) == "" {
		store.enabled = false
		return store, nil
	}
	if err := prepareOwnerOnlyFile(store.path, strings.TrimSpace(os.Getenv("CONTEXTLATTICE_CONTEXT_POLICY_PATH")) == ""); err != nil {
		return store, err
	}
	if err := store.load(); err != nil {
		return store, err
	}
	return store, nil
}

func (s *contextPolicyStore) load() error {
	file, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open context policy ledger: %w", err)
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
		switch anyToString(row["schema_id"]) {
		case contextPolicyCandidateContractID:
			if id := anyToString(row["candidate_id"]); id != "" {
				s.candidates[id] = cloneMap(row)
			}
		case contextPolicyEvaluationContractID:
			s.evaluations = append(s.evaluations, cloneMap(row))
		}
		s.logEntries++
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan context policy ledger: %w", err)
	}
	s.trimLocked()
	return nil
}

func (s *contextPolicyStore) trimLocked() {
	s.evaluations = retainLatestGroupedRows(s.evaluations, "candidate_id", s.maxEntries)
	if len(s.candidates) <= s.maxEntries {
		return
	}
	type candidateAge struct {
		id      string
		updated string
	}
	ages := make([]candidateAge, 0, len(s.candidates))
	for id, candidate := range s.candidates {
		ages = append(ages, candidateAge{id: id, updated: anyToString(candidate["updated_at"])})
	}
	sort.Slice(ages, func(i, j int) bool { return ages[i].updated > ages[j].updated })
	for _, item := range ages[s.maxEntries:] {
		delete(s.candidates, item.id)
	}
}

// Keep the newest row per entity before spending the remaining budget on history.
// This prevents compaction from discarding the evidence needed to resume a state.
func retainLatestGroupedRows(rows []map[string]any, groupKey string, limit int) []map[string]any {
	if limit <= 0 || len(rows) == 0 {
		return nil
	}
	if len(rows) <= limit {
		return rows
	}
	selected := map[int]struct{}{}
	seenGroups := map[string]struct{}{}
	for index := len(rows) - 1; index >= 0 && len(selected) < limit; index-- {
		group := anyToString(rows[index][groupKey])
		if group == "" {
			continue
		}
		if _, seen := seenGroups[group]; seen {
			continue
		}
		seenGroups[group] = struct{}{}
		selected[index] = struct{}{}
	}
	for index := len(rows) - 1; index >= 0 && len(selected) < limit; index-- {
		if _, exists := selected[index]; !exists {
			selected[index] = struct{}{}
		}
	}
	indices := make([]int, 0, len(selected))
	for index := range selected {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	retained := make([]map[string]any, 0, len(indices))
	for _, index := range indices {
		retained = append(retained, rows[index])
	}
	return retained
}

func (s *contextPolicyStore) candidate(id string) map[string]any {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneMap(s.candidates[strings.TrimSpace(id)])
}

func (s *contextPolicyStore) recordCandidate(candidate map[string]any) error {
	if s == nil || !s.enabled {
		return errors.New("context policy store disabled")
	}
	id := strings.TrimSpace(anyToString(candidate["candidate_id"]))
	if id == "" {
		return errors.New("candidate_id is required")
	}
	s.mu.Lock()
	merged := cloneMap(candidate)
	if existing := s.candidates[id]; len(existing) > 0 {
		if _, valid := contextPolicyPhases[anyToString(existing["status"])]; valid {
			merged["status"] = existing["status"]
			merged["lifecycle"] = cloneMap(anyMap(existing["lifecycle"]))
			merged["created_at"] = firstNonEmptyStrings(anyToString(existing["created_at"]), anyToString(merged["created_at"]))
		}
	}
	s.candidates[id] = cloneMap(merged)
	s.trimLocked()
	s.mu.Unlock()
	replaceMapContents(candidate, merged)
	return s.appendRows(merged)
}

func (s *contextPolicyStore) recordEvaluation(evaluation map[string]any, candidate map[string]any) error {
	if s == nil || !s.enabled {
		return errors.New("context policy store disabled")
	}
	s.mu.Lock()
	id := anyToString(candidate["candidate_id"])
	current := cloneMap(s.candidates[id])
	transitionApplied := anyToBool(evaluation["transition_applied"])
	if transitionApplied && len(current) > 0 && anyToString(current["status"]) != anyToString(evaluation["previous_phase"]) {
		s.mu.Unlock()
		return fmt.Errorf("%w: candidate moved from %s to %s before this evaluation was recorded", errContextPolicyTransitionConflict, anyToString(evaluation["previous_phase"]), anyToString(current["status"]))
	}
	storedCandidate := current
	if transitionApplied || len(storedCandidate) == 0 {
		storedCandidate = cloneMap(candidate)
	}
	s.evaluations = append(s.evaluations, cloneMap(evaluation))
	if id != "" {
		s.candidates[id] = cloneMap(storedCandidate)
	}
	s.trimLocked()
	s.mu.Unlock()
	replaceMapContents(candidate, storedCandidate)
	return s.appendRows(evaluation, storedCandidate)
}

func replaceMapContents(destination, source map[string]any) {
	for key := range destination {
		delete(destination, key)
	}
	for key, value := range source {
		destination[key] = value
	}
}

func (s *contextPolicyStore) appendRows(rows ...map[string]any) error {
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

func (s *contextPolicyStore) compact() error {
	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	return s.compactLockedIO()
}

func (s *contextPolicyStore) compactLockedIO() error {
	s.mu.RLock()
	candidates := make([]map[string]any, 0, len(s.candidates))
	for _, candidate := range s.candidates {
		candidates = append(candidates, cloneMap(candidate))
	}
	evaluations := append([]map[string]any{}, s.evaluations...)
	s.mu.RUnlock()
	sort.Slice(candidates, func(i, j int) bool {
		return anyToString(candidates[i]["candidate_id"]) < anyToString(candidates[j]["candidate_id"])
	})
	rows := append(candidates, evaluations...)
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

func (s *contextPolicyStore) setError(err error) {
	if s == nil || err == nil {
		return
	}
	s.mu.Lock()
	s.lastError = clipText(err.Error(), 500)
	s.mu.Unlock()
}

func (s *contextPolicyStore) snapshot() map[string]any {
	if s == nil {
		return map[string]any{"schema_id": contextPolicyStatusSchemaID, "enabled": false, "candidate_count": 0, "evaluation_count": 0, "candidates": []any{}}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	phaseCounts := map[string]int{}
	candidates := make([]map[string]any, 0, len(s.candidates))
	for _, candidate := range s.candidates {
		phaseCounts[anyToString(candidate["status"])]++
		candidates = append(candidates, cloneMap(candidate))
	}
	sort.Slice(candidates, func(i, j int) bool {
		return anyToString(candidates[i]["updated_at"]) > anyToString(candidates[j]["updated_at"])
	})
	items := make([]any, 0, minInt(len(candidates), 20))
	for _, candidate := range candidates[:minInt(len(candidates), 20)] {
		items = append(items, candidate)
	}
	return map[string]any{
		"schema_id":        contextPolicyStatusSchemaID,
		"version":          1,
		"enabled":          s.enabled,
		"mode":             "advisor_only",
		"runtime_mutation": false,
		"candidate_count":  len(s.candidates),
		"evaluation_count": len(s.evaluations),
		"phase_counts":     phaseCounts,
		"candidates":       items,
		"storage": map[string]any{
			"enabled": s.enabled, "max_bytes": s.maxBytes, "max_entries": s.maxEntries,
			"log_entries": s.logEntries, "parse_errors": s.parseErrors,
			"compaction_count": s.compactionCount, "last_persisted_at": s.lastPersistedAt,
			"last_error": s.lastError,
		},
		"updated_at": nowUTCISO(),
	}
}

func (s *contextPolicyStore) aggregateSignalSufficientStatistics() map[string]any {
	if s == nil {
		return map[string]any{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	promoted := 0
	for _, candidate := range s.candidates {
		if anyToString(candidate["status"]) == "promoted" {
			promoted++
		}
	}
	promotionRate := 0.0
	if len(s.candidates) > 0 {
		promotionRate = float64(promoted) / float64(len(s.candidates))
	}
	return map[string]any{
		"policy_candidate_count":  len(s.candidates),
		"policy_evaluation_count": len(s.evaluations),
		"policy_promotion_rate":   roundFloat(promotionRate, 6),
	}
}

func (s *contextPolicyStore) advisoryCandidate(project string) map[string]any {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var selected map[string]any
	for _, candidate := range s.candidates {
		if project != "" && anyToString(candidate["project"]) != project {
			continue
		}
		status := anyToString(candidate["status"])
		if status == "rolled_back" || status == "insufficient_evidence" {
			continue
		}
		if selected == nil || anyToString(candidate["updated_at"]) > anyToString(selected["updated_at"]) {
			selected = candidate
		}
	}
	if selected == nil {
		return nil
	}
	return map[string]any{
		"candidate_id": selected["candidate_id"],
		"status":       selected["status"],
		"policy":       cloneMap(anyMap(selected["policy"])),
		"updated_at":   selected["updated_at"],
		"applied":      false,
		"reason":       "The public planner exposes learned advice but does not mutate live retrieval policy.",
	}
}

func (s *server) contextPolicyObservationRows() ([]map[string]any, []map[string]any) {
	if s == nil || s.contextPackQuality == nil {
		return nil, nil
	}
	t := s.contextPackQuality
	t.mu.Lock()
	defer t.mu.Unlock()
	samples := make([]map[string]any, 0, len(t.samples))
	for _, row := range t.samples {
		samples = append(samples, cloneMap(row))
	}
	outcomes := make([]map[string]any, 0, len(t.outcomes))
	for _, row := range t.outcomes {
		outcomes = append(outcomes, cloneMap(row))
	}
	return samples, outcomes
}

func (s *server) buildContextPolicyCandidate(payload map[string]any) (map[string]any, bool, error) {
	project, err := sanitizeMemoryProject(firstNonEmptyStrings(anyToString(payload["project"]), "contextlattice"))
	if err != nil {
		return nil, false, err
	}
	taskClass := strings.ToLower(strings.TrimSpace(anyToString(payload["task_class"])))
	retrievalIntent := strings.ToLower(strings.TrimSpace(anyToString(payload["retrieval_intent"])))
	minimum := clampInt(anyToInt(payload["minimum_outcomes"], envInt("CONTEXTLATTICE_CONTEXT_POLICY_MIN_OUTCOMES", 20)), 10, 100)
	samples, outcomes := s.contextPolicyObservationRows()
	sampleByID := map[string]map[string]any{}
	for _, sample := range samples {
		sampleProject := anyToString(sample["project"])
		if sampleProject != "" && sampleProject != project {
			continue
		}
		if sampleProject == "" && project != "contextlattice" {
			continue
		}
		if id := anyToString(sample["sample_id"]); id != "" {
			sampleByID[id] = sample
		}
	}
	eligible := make([]map[string]any, 0, len(outcomes))
	for _, outcome := range outcomes {
		if !anyToBool(outcome["calibration_eligible"]) {
			continue
		}
		outcomeProject := anyToString(outcome["project"])
		if outcomeProject != "" && outcomeProject != project {
			continue
		}
		sample := sampleByID[anyToString(outcome["sample_id"])]
		if outcomeProject == "" && sample == nil {
			continue
		}
		outcomeTaskClass := strings.ToLower(strings.TrimSpace(firstNonEmptyStrings(anyToString(outcome["task_class"]), anyToString(sample["task_class"]))))
		if taskClass != "" && outcomeTaskClass != taskClass {
			continue
		}
		outcomeRetrievalIntent := strings.ToLower(strings.TrimSpace(firstNonEmptyStrings(anyToString(outcome["retrieval_intent"]), anyToString(sample["retrieval_intent"]))))
		if retrievalIntent != "" && outcomeRetrievalIntent != retrievalIntent {
			continue
		}
		if sample != nil {
			outcome["context_sample"] = sample
		}
		eligible = append(eligible, outcome)
	}
	metrics := contextPolicyMetrics(eligible)
	policy := deriveContextPolicy(eligible)
	status := "insufficient_evidence"
	recorded := len(eligible) >= minimum
	if recorded {
		status = "candidate"
	}
	evidenceKeys := make([]string, 0, len(eligible))
	displayOutcomeIDs := make([]string, 0, len(eligible))
	for _, outcome := range eligible {
		if id := anyToString(outcome["outcome_id"]); id != "" {
			evidenceKeys = append(evidenceKeys, "id:"+id)
			displayOutcomeIDs = append(displayOutcomeIDs, id)
			continue
		}
		encoded, _ := json.Marshal(outcome)
		evidenceKeys = append(evidenceKeys, "row:"+sha256Hex(string(encoded)))
	}
	sort.Strings(evidenceKeys)
	sort.Strings(displayOutcomeIDs)
	evidenceSHA := sha256Hex(strings.Join(evidenceKeys, "\x00"))
	seed, _ := json.Marshal(map[string]any{
		"project": project, "task_class": taskClass, "retrieval_intent": retrievalIntent,
		"minimum_outcomes": minimum, "metrics": metrics, "policy": policy, "evidence_sha256": evidenceSHA,
	})
	candidateID := "ctxpol_" + sha256Hex(string(seed))[:24]
	outcomeIDs := make([]any, 0, minInt(len(displayOutcomeIDs), 64))
	for _, id := range displayOutcomeIDs[:minInt(len(displayOutcomeIDs), 64)] {
		outcomeIDs = append(outcomeIDs, id)
	}
	now := nowUTCISO()
	createdAt := now
	existing := s.contextPolicy.candidate(candidateID)
	if len(existing) > 0 {
		createdAt = firstNonEmptyStrings(anyToString(existing["created_at"]), now)
		if _, valid := contextPolicyPhases[anyToString(existing["status"])]; valid {
			status = anyToString(existing["status"])
			recorded = true
		}
	}
	candidate := map[string]any{
		"schema_id":         contextPolicyCandidateContractID,
		"version":           1,
		"candidate_id":      candidateID,
		"project":           project,
		"task_class":        taskClass,
		"retrieval_intent":  retrievalIntent,
		"status":            status,
		"mode":              "advisor_only",
		"created_at":        createdAt,
		"updated_at":        now,
		"minimum_outcomes":  minimum,
		"eligible_outcomes": len(eligible),
		"evidence_sha256":   evidenceSHA,
		"policy":            policy,
		"evidence": map[string]any{
			"metrics":                   metrics,
			"outcome_ids":               outcomeIDs,
			"calibration_eligible_only": true,
			"scope_filter": map[string]any{
				"task_class": taskClass, "retrieval_intent": retrievalIntent, "exact_match_required": taskClass != "" || retrievalIntent != "",
			},
			"counterfactual_limit": "Historical outcomes seed a candidate; controlled canary outcomes are required before promotion.",
		},
		"lifecycle": map[string]any{
			"current_phase":       status,
			"allowed_transitions": []any{"candidate", "shadow", "canary", "promoted", "rolled_back"},
			"one_step_only":       true,
		},
		"activation": map[string]any{
			"allowed":          false,
			"runtime_mutation": false,
			"reason":           "The public core records advisory policy evidence; runtime activation is an entitled operator control.",
		},
	}
	return candidate, recorded, nil
}

func deriveContextPolicy(outcomes []map[string]any) map[string]any {
	successful := make([]map[string]any, 0, len(outcomes))
	graphSuccess, graphTotal, plainSuccess, plainTotal := 0, 0, 0, 0
	contextTokens := []int{}
	sourceCounts := []int{}
	for _, outcome := range outcomes {
		sample := anyMap(outcome["context_sample"])
		firstPass := anyToBool(outcome["first_pass_success"]) && !anyToBool(outcome["repair_required"])
		if firstPass {
			successful = append(successful, outcome)
		}
		if anyToBool(sample["graph_context_used"]) {
			graphTotal++
			if firstPass {
				graphSuccess++
			}
		} else {
			plainTotal++
			if firstPass {
				plainSuccess++
			}
		}
		if firstPass {
			if value := anyToInt(sample["model_call_token_basis"], 0); value > 0 {
				contextTokens = append(contextTokens, value)
			}
			if value := anyToInt(sample["returned_source_count"], 0); value > 0 {
				sourceCounts = append(sourceCounts, value)
			}
		}
	}
	targetTokens := medianInt(contextTokens, 4000)
	targetTokens = clampInt(targetTokens, 512, 12000)
	minSources := clampInt(medianInt(sourceCounts, 2), 1, 7)
	preferGraph := false
	if graphTotal >= 5 && plainTotal >= 5 {
		preferGraph = float64(graphSuccess)/float64(graphTotal) >= float64(plainSuccess)/float64(plainTotal)+0.03
	}
	return map[string]any{
		"target_context_tokens":    targetTokens,
		"minimum_source_diversity": minSources,
		"prefer_graph_context":     preferGraph,
		"temporal_claim_expansion": true,
		"require_proof_support":    true,
		"max_omitted_high_value":   0,
		"max_retrieval_rounds":     3,
		"successful_training_rows": len(successful),
		"selection_strategy":       "impact_per_estimated_token_with_provenance_diversity",
	}
}

func medianInt(values []int, fallback int) int {
	if len(values) == 0 {
		return fallback
	}
	copyValues := append([]int{}, values...)
	sort.Ints(copyValues)
	middle := len(copyValues) / 2
	if len(copyValues)%2 == 0 {
		return (copyValues[middle-1] + copyValues[middle]) / 2
	}
	return copyValues[middle]
}

func contextPolicyMetrics(rows []map[string]any) map[string]any {
	metrics := map[string]any{
		"sample_count": len(rows), "first_pass_success_rate": nil, "repair_rate": nil,
		"average_retry_count": nil, "average_followup_tokens": nil, "average_provider_total_tokens": nil,
	}
	if len(rows) == 0 {
		return metrics
	}
	firstPass, repair, retries, followup, providerTotal, providerCount := 0, 0, 0, 0, 0, 0
	for _, row := range rows {
		if anyToBool(row["first_pass_success"]) {
			firstPass++
		}
		if anyToBool(row["repair_required"]) {
			repair++
		}
		retries += anyToInt(row["retry_count"], 0)
		followup += anyToInt(row["observed_followup_tokens"], 0)
		if value := anyToInt(row["provider_total_tokens"], 0); value > 0 {
			providerTotal += value
			providerCount++
		}
	}
	metrics["first_pass_success_rate"] = roundFloat(float64(firstPass)/float64(len(rows)), 4)
	metrics["repair_rate"] = roundFloat(float64(repair)/float64(len(rows)), 4)
	metrics["average_retry_count"] = roundFloat(float64(retries)/float64(len(rows)), 4)
	metrics["average_followup_tokens"] = roundFloat(float64(followup)/float64(len(rows)), 3)
	if providerCount > 0 {
		metrics["average_provider_total_tokens"] = roundFloat(float64(providerTotal)/float64(providerCount), 3)
	}
	return metrics
}

func (s *server) contextPolicyEvaluation(payload map[string]any) (map[string]any, map[string]any, error) {
	id := strings.TrimSpace(anyToString(payload["candidate_id"]))
	if id == "" {
		return nil, nil, errors.New("candidate_id is required")
	}
	candidate := s.contextPolicy.candidate(id)
	if len(candidate) == 0 {
		return nil, nil, errors.New("candidate not found")
	}
	phase := anyToString(candidate["status"])
	if _, ok := contextPolicyPhases[phase]; !ok || phase == "insufficient_evidence" {
		return nil, candidate, errors.New("candidate is not eligible for evaluation")
	}
	if phase == "promoted" || phase == "rolled_back" {
		return nil, candidate, errors.New("candidate lifecycle is terminal; create a new evidence-bound candidate")
	}
	control := anyMap(payload["control"])
	canary := anyMap(payload["canary"])
	evidenceSource := "operator_supplied_metrics"
	hasControl, hasCanary := len(control) > 0, len(canary) > 0
	if hasControl != hasCanary {
		return nil, candidate, errors.New("control and canary metrics must both be supplied or both be omitted")
	}
	if !hasControl {
		evidenceSource = "persisted_outcomes"
		_, outcomes := s.contextPolicyObservationRows()
		controlRows, canaryRows := []map[string]any{}, []map[string]any{}
		for _, row := range outcomes {
			if !anyToBool(row["calibration_eligible"]) {
				continue
			}
			if anyToString(row["policy_id"]) != id {
				continue
			}
			if project := anyToString(candidate["project"]); project != "" && anyToString(row["project"]) != project {
				continue
			}
			if anyToString(row["policy_phase"]) != phase {
				continue
			}
			switch strings.ToLower(anyToString(row["policy_arm"])) {
			case "control":
				controlRows = append(controlRows, row)
			case phase:
				if phase == "shadow" || phase == "canary" {
					canaryRows = append(canaryRows, row)
				}
			}
		}
		control = contextPolicyMetrics(controlRows)
		canary = contextPolicyMetrics(canaryRows)
	}
	if phase == "shadow" || phase == "canary" {
		if err := validateContextPolicyArm("control", control); err != nil {
			return nil, candidate, err
		}
		if err := validateContextPolicyArm("canary", canary); err != nil {
			return nil, candidate, err
		}
	}
	minimum := 0
	if phase == "shadow" {
		minimum = clampInt(anyToInt(payload["minimum_arm_samples"], 10), 5, 1000)
	}
	if phase == "canary" {
		minimum = clampInt(anyToInt(payload["minimum_arm_samples"], 20), 10, 2000)
	}
	guardrails, beneficial, hardRegression := evaluateContextPolicyArms(control, canary, minimum)
	nextPhase, decision, reason := phase, "hold", "More controlled evidence is required."
	if phase == "candidate" {
		nextPhase, decision, reason = "shadow", "advance", "Candidate has enough calibration-eligible historical outcomes for shadow observation."
	} else if hardRegression {
		nextPhase, decision, reason = "rolled_back", "rollback", "A quality, repair, follow-up-token, or provider-token guardrail regressed materially."
	} else if contextPolicyGuardrailsPass(guardrails) && beneficial {
		if phase == "shadow" {
			nextPhase, decision, reason = "canary", "advance", "Controlled shadow evidence passes every guardrail and shows measurable benefit."
		}
		if phase == "canary" {
			nextPhase, decision, reason = "promoted", "advance", "Controlled canary evidence passes every guardrail and shows measurable benefit."
		}
	}
	apply := anyToBool(payload["apply_transition"])
	previousPhase := phase
	if apply && nextPhase != phase {
		candidate["status"] = nextPhase
		candidate["updated_at"] = nowUTCISO()
		candidate["lifecycle"] = map[string]any{"current_phase": nextPhase, "previous_phase": previousPhase, "one_step_only": true}
	}
	evaluationID := "ctxeval_" + sha256Hex(strings.Join([]string{id, phase, nextPhase, nowUTCISO(), anyToString(control), anyToString(canary)}, "\x00"))[:24]
	evaluation := map[string]any{
		"schema_id":           contextPolicyEvaluationContractID,
		"version":             1,
		"evaluation_id":       evaluationID,
		"candidate_id":        id,
		"project":             candidate["project"],
		"evaluated_at":        nowUTCISO(),
		"previous_phase":      previousPhase,
		"recommended_phase":   nextPhase,
		"recorded_phase":      candidate["status"],
		"decision":            decision,
		"reason":              reason,
		"transition_applied":  apply && nextPhase != previousPhase,
		"one_step_only":       true,
		"control":             control,
		"canary":              canary,
		"minimum_arm_samples": minimum,
		"guardrails":          guardrails,
		"beneficial":          beneficial,
		"evidence_source":     evidenceSource,
		"runtime_activation":  false,
		"activation_boundary": "Advisory promotion never mutates live retrieval in the public core.",
	}
	return evaluation, candidate, nil
}

func contextPolicyNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, !math.IsNaN(typed) && !math.IsInf(typed, 0)
	case float32:
		parsed := float64(typed)
		return parsed, !math.IsNaN(parsed) && !math.IsInf(parsed, 0)
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0)
	default:
		return 0, false
	}
}

func validateContextPolicyArm(name string, arm map[string]any) error {
	countValue, ok := contextPolicyNumber(arm["sample_count"])
	if !ok || countValue < 0 || countValue != math.Trunc(countValue) {
		return fmt.Errorf("%s.sample_count must be a non-negative integer", name)
	}
	if countValue == 0 {
		return nil
	}
	for _, field := range []string{"first_pass_success_rate", "repair_rate"} {
		value, valid := contextPolicyNumber(arm[field])
		if !valid || value < 0 || value > 1 {
			return fmt.Errorf("%s.%s must be a number between 0 and 1", name, field)
		}
	}
	for _, field := range []string{"average_followup_tokens"} {
		value, valid := contextPolicyNumber(arm[field])
		if !valid || value < 0 {
			return fmt.Errorf("%s.%s must be a non-negative number", name, field)
		}
	}
	if raw, exists := arm["average_retry_count"]; exists && raw != nil {
		value, valid := contextPolicyNumber(raw)
		if !valid || value < 0 {
			return fmt.Errorf("%s.average_retry_count must be a non-negative number when present", name)
		}
	}
	if raw, exists := arm["average_provider_total_tokens"]; exists && raw != nil {
		value, valid := contextPolicyNumber(raw)
		if !valid || value < 0 {
			return fmt.Errorf("%s.average_provider_total_tokens must be a non-negative number when present", name)
		}
	}
	return nil
}

func evaluateContextPolicyArms(control, canary map[string]any, minimum int) ([]any, bool, bool) {
	controlCount, canaryCount := anyToInt(control["sample_count"], 0), anyToInt(canary["sample_count"], 0)
	guards := []any{}
	appendGuard := func(name string, passed bool, actual any, limit any) {
		guards = append(guards, map[string]any{"name": name, "passed": passed, "actual": actual, "limit": limit})
	}
	if minimum > 0 {
		appendGuard("control_sample_floor", controlCount >= minimum, controlCount, minimum)
		appendGuard("canary_sample_floor", canaryCount >= minimum, canaryCount, minimum)
	}
	if controlCount == 0 || canaryCount == 0 {
		return guards, false, false
	}
	controlSuccess, canarySuccess := anyToFloat(control["first_pass_success_rate"]), anyToFloat(canary["first_pass_success_rate"])
	controlRepair, canaryRepair := anyToFloat(control["repair_rate"]), anyToFloat(canary["repair_rate"])
	controlFollowup, canaryFollowup := anyToFloat(control["average_followup_tokens"]), anyToFloat(canary["average_followup_tokens"])
	controlProvider, canaryProvider := anyToFloat(control["average_provider_total_tokens"]), anyToFloat(canary["average_provider_total_tokens"])
	appendGuard("first_pass_success", canarySuccess+0.02 >= controlSuccess, canarySuccess, roundFloat(controlSuccess-0.02, 4))
	appendGuard("repair_rate", canaryRepair <= controlRepair+0.02, canaryRepair, roundFloat(controlRepair+0.02, 4))
	appendGuard("followup_tokens", canaryFollowup <= controlFollowup*1.05+16, canaryFollowup, roundFloat(controlFollowup*1.05+16, 3))
	if controlProvider > 0 && canaryProvider > 0 {
		appendGuard("provider_total_tokens", canaryProvider <= controlProvider*1.05+32, canaryProvider, roundFloat(controlProvider*1.05+32, 3))
	}
	beneficial := canarySuccess >= controlSuccess+0.02 || (controlFollowup > 0 && canaryFollowup <= controlFollowup*0.95) || (controlProvider > 0 && canaryProvider > 0 && canaryProvider <= controlProvider*0.95)
	hardRegression := canarySuccess+0.08 < controlSuccess || canaryRepair > controlRepair+0.08 || (controlFollowup > 0 && canaryFollowup > controlFollowup*1.2+32) || (controlProvider > 0 && canaryProvider > controlProvider*1.2+64)
	return guards, beneficial, hardRegression
}

func contextPolicyGuardrailsPass(raw []any) bool {
	if len(raw) == 0 {
		return false
	}
	for _, item := range raw {
		if !anyToBool(anyMap(item)["passed"]) {
			return false
		}
	}
	return true
}

func (s *server) memoryContextPolicyCandidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	s.handleContextPolicyCandidate(w, r, false)
}

func (s *server) toolsContextPolicyCandidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareToolHeaders(w, r, "/tools/context_policy_candidate"); !ok {
		return
	}
	s.handleContextPolicyCandidate(w, r, true)
}

func (s *server) handleContextPolicyCandidate(w http.ResponseWriter, r *http.Request, tool bool) {
	payload, err := readOptionalJSONBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
		return
	}
	candidate, recorded, err := s.buildContextPolicyCandidate(payload)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "invalid_policy_candidate", "detail": err.Error()})
		return
	}
	if recorded {
		if err := s.contextPolicy.recordCandidate(candidate); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "policy_candidate_persist_failed", "detail": err.Error()})
			return
		}
	}
	response := map[string]any{"ok": true, "schema_id": contextPolicyCandidateContractID, "candidate": candidate, "recorded": recorded, "status": candidate["status"]}
	if tool {
		response["tool"] = "context_policy_candidate"
	}
	writeJSON(w, http.StatusOK, attachPayloadFormatContract(contextPolicyCandidateContractID, response, anyToString(payload["agent_id"]), "context_policy_candidate", r.URL.Path))
}

func (s *server) memoryContextPolicyEvaluate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	s.handleContextPolicyEvaluate(w, r, false)
}

func (s *server) toolsContextPolicyEvaluate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareToolHeaders(w, r, "/tools/context_policy_evaluate"); !ok {
		return
	}
	s.handleContextPolicyEvaluate(w, r, true)
}

func (s *server) handleContextPolicyEvaluate(w http.ResponseWriter, r *http.Request, tool bool) {
	payload, err := readOptionalJSONBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
		return
	}
	evaluation, candidate, err := s.contextPolicyEvaluation(payload)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "policy_evaluation_failed", "detail": err.Error()})
		return
	}
	if err := s.contextPolicy.recordEvaluation(evaluation, candidate); err != nil {
		if errors.Is(err, errContextPolicyTransitionConflict) {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "policy_transition_conflict", "detail": err.Error()})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "policy_evaluation_persist_failed", "detail": err.Error()})
		return
	}
	response := map[string]any{"ok": true, "schema_id": contextPolicyEvaluationContractID, "evaluation": evaluation, "candidate": candidate, "recorded": true}
	if tool {
		response["tool"] = "context_policy_evaluate"
	}
	writeJSON(w, http.StatusOK, attachPayloadFormatContract(contextPolicyEvaluationContractID, response, anyToString(payload["agent_id"]), "context_policy_evaluation", r.URL.Path))
}

func (s *server) telemetryContextPolicy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.contextPolicy.snapshot())
}
