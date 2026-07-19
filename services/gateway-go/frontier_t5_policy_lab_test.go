package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func newFrontierT5PolicyLabTestServer(t *testing.T) *server {
	t.Helper()
	root := t.TempDir()
	memoryRoot := filepath.Join(root, "memory")
	if err := os.MkdirAll(memoryRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	memory := &memoryStore{
		policy: memoryStorePolicy{
			enabled: true, rootPath: memoryRoot,
			historyPath:           filepath.Join(root, "memory-history.ndjson"),
			accessLogPath:         filepath.Join(root, "memory-access.ndjson"),
			edgePath:              filepath.Join(root, "memory-edges.ndjson"),
			exactStateIndexPath:   filepath.Join(root, "exact-state.ndjson"),
			rollupUseHistoryIndex: true, maxRecent: 256, maxSummaryChars: 4000,
			hotIndexMaxAgeDays: 30, confidencePriorAlpha: 1, confidencePriorBeta: 1,
			confidenceWriteWeight: 1, confidenceReadWeight: 0.1,
		},
		recent: []memoryStoreEntry{}, latestTopic: map[string]string{}, latestHash: map[string]string{},
		latestHorizon: map[string]int{}, latestLifecycle: map[string]string{}, latestStorageTier: map[string]string{},
		lastAccess: map[string]time.Time{}, confidence: map[string]confidenceState{},
		rollupCache: map[string]topicRollupCacheEntry{}, edges: map[string]memoryEdgeEntry{},
		edgeOrder: []string{}, edgeOrdinal: map[string]int64{}, edgeAdjacency: map[string]map[string]struct{}{},
		exactStatePaths: map[string]struct{}{}, pathLocks: map[string]*memoryPathLock{},
	}
	memory.ready.Store(true)

	ledger, err := newFrontierT5LedgerForTest(filepath.Join(root, "frontier-t5.ndjson"))
	if err != nil {
		t.Fatalf("new T5 ledger: %v", err)
	}
	claims := &temporalClaimStore{
		enabled: true, path: filepath.Join(root, "claims.ndjson"), maxClaims: 256, compactEvery: 32,
		claims: map[string]temporalClaim{}, proofSessionIndex: map[string][]string{},
	}
	policy := &contextPolicyStore{
		enabled: true, path: filepath.Join(root, "context-policy.ndjson"), maxBytes: 1 << 20,
		maxEntries: 256, candidates: map[string]map[string]any{}, evaluations: []map[string]any{},
	}
	return &server{
		memoryStore: memory, frontierT5: ledger, temporalClaims: claims,
		contextPackQuality: newContextPackQualityTelemetry(512), contextPolicy: policy,
	}
}

func putFrontierT5TestMemory(t *testing.T, s *server, project, file, content, lifecycle, tier string, tags []string) memoryStoreEntry {
	t.Helper()
	entry, _, err := s.memoryStore.put(normalizedWrite{
		project: project, fileName: file, content: content, topicPath: "frontier/t5",
		agentID: "frontier-t5-test", sessionID: "frontier-t5-session", tags: tags,
		lifecycle: lifecycle, storageTier: tier,
	})
	if err != nil {
		t.Fatalf("put memory %s/%s: %v", project, file, err)
	}
	return entry
}

func seedFrontierT5PolicyOutcomes(t testing.TB, s *server, project string, count int, tokenBase int) {
	t.Helper()
	for i := 0; i < count; i++ {
		sampleID := project + "-sample-" + anyToString(i)
		s.contextPackQuality.recordQuality(map[string]any{
			"sample_id": sampleID, "project": project, "quality_score": 90,
			"task_class": "general", "retrieval_intent": "balanced",
			"model_call_token_basis": tokenBase + i, "returned_source_count": 3,
			"graph_context_used": i%2 == 0, "tokenizer_exact": true,
		})
		s.contextPackQuality.recordOutcome(map[string]any{
			"outcome_id": project + "-outcome-" + anyToString(i), "sample_id": sampleID,
			"project": project, "first_pass_success": true, "repair_required": false,
			"retry_count": 0, "observed_followup_tokens": 100, "provider_total_tokens": 900,
			"calibration_eligible": true, "task_class": "general", "retrieval_intent": "balanced",
		})
	}
}

func upsertFrontierT5Claim(t *testing.T, s *server, payload map[string]any) temporalClaim {
	t.Helper()
	claim, err := s.temporalClaims.upsert(payload)
	if err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	return claim
}

func assertFrontierT5Error(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error=%v, want substring %q", err, want)
	}
}

func TestFrontierT5PolicySimulationDeterministicSameSnapshotNoPersist(t *testing.T) {
	payload := map[string]any{
		"project": "simulation-project", "suite_id": "same-snapshot-suite", "limit": 2,
		"cases": []any{
			map[string]any{"case_id": "a", "snapshot_id": "snap-1", "snapshot_digest": "digest-1", "baseline": map[string]any{"tokens": 100, "utility": 0.5}, "candidate": map[string]any{"tokens": 90, "utility": 0.6}},
			map[string]any{"case_id": "b", "snapshot_id": "snap-1", "snapshot_digest": "digest-1", "baseline": map[string]any{"tokens": 200, "utility": 0.4}, "candidate": map[string]any{"tokens": 180, "utility": 0.5}},
			map[string]any{"case_id": "c", "snapshot_id": "snap-1", "snapshot_digest": "digest-1", "baseline": map[string]any{"tokens": 300, "utility": 0.3}, "candidate": map[string]any{"tokens": 270, "utility": 0.4}},
		},
	}
	first, err := buildPolicySimulation(payload)
	if err != nil {
		t.Fatalf("first simulation: %v", err)
	}
	second, err := buildPolicySimulation(payload)
	if err != nil {
		t.Fatalf("second simulation: %v", err)
	}
	for _, key := range []string{"simulation_id", "rows", "summary", "computation"} {
		if !reflect.DeepEqual(first[key], second[key]) {
			t.Fatalf("non-deterministic %s: first=%#v second=%#v", key, first[key], second[key])
		}
	}
	if anyToInt(first["input_case_count"], 0) != 3 || anyToInt(first["case_count"], 0) != 2 || anyToInt(first["same_snapshot_count"], 0) != 2 || !anyToBool(first["truncated"]) {
		t.Fatalf("unexpected same-snapshot counts: %#v", first)
	}
	computation := anyMap(first["computation"])
	if !anyToBool(computation["deterministic"]) || anyToInt(computation["network_calls"], -1) != 0 || anyToBool(computation["persisted"]) || anyToBool(computation["runtime_policy_mutated"]) {
		t.Fatalf("simulation must be deterministic and non-persistent: %#v", computation)
	}
}

func TestFrontierT5ScopedPolicyCardColdStartShrinkageAndProjectIsolation(t *testing.T) {
	s := newFrontierT5PolicyLabTestServer(t)
	seedFrontierT5PolicyOutcomes(t, s, "sparse-project", 4, 8000)
	seedFrontierT5PolicyOutcomes(t, s, "rich-project", 20, 5000)

	sparse, err := s.buildScopedPolicyCard(map[string]any{"project": "sparse-project", "minimum_outcomes": 10, "global_prior_weight": 20})
	if err != nil {
		t.Fatalf("sparse card: %v", err)
	}
	rich, err := s.buildScopedPolicyCard(map[string]any{"project": "rich-project", "minimum_outcomes": 10, "global_prior_weight": 20})
	if err != nil {
		t.Fatalf("rich card: %v", err)
	}
	if !anyToBool(sparse["cold_start"]) || anyToString(sparse["status"]) != "cold_start" || anyToInt(sparse["eligible_outcomes"], 0) != 4 {
		t.Fatalf("sparse project did not expose cold-start shrinkage: %#v", sparse)
	}
	if anyToFloat(sparse["project_evidence_weight"]) >= 0.6 || anyToInt(anyMap(sparse["evidence"])["cross_project_rows_used"], -1) != 0 {
		t.Fatalf("sparse project evidence was not shrunk and isolated: %#v", sparse)
	}
	if anyToBool(rich["cold_start"]) || anyToString(rich["status"]) != "evidence_bound" || anyToInt(rich["eligible_outcomes"], 0) != 20 {
		t.Fatalf("rich project card did not use only its own evidence: %#v", rich)
	}
	if anyToString(sparse["project"]) == anyToString(rich["project"]) || anyToString(sparse["card_id"]) == anyToString(rich["card_id"]) {
		t.Fatalf("cards are not project-isolated: sparse=%#v rich=%#v", sparse, rich)
	}
	wrongScope, err := s.buildScopedPolicyCard(map[string]any{"project": "rich-project", "task_class": "review", "retrieval_intent": "deep", "minimum_outcomes": 10})
	if err != nil {
		t.Fatalf("wrong-scope card: %v", err)
	}
	if !anyToBool(wrongScope["cold_start"]) || anyToInt(wrongScope["eligible_outcomes"], -1) != 0 {
		t.Fatalf("project-wide evidence leaked into a different task/intent scope: %#v", wrongScope)
	}
}

func TestFrontierT5PromotionRecommendationReceiptDriftAndSurvivorBiasGuard(t *testing.T) {
	newCandidate := func(t *testing.T) (*server, string) {
		t.Helper()
		s := newFrontierT5PolicyLabTestServer(t)
		candidateID := "candidate-t5-promotion"
		s.contextPolicy.candidates[candidateID] = map[string]any{
			"candidate_id": candidateID, "project": "promotion-project", "status": "shadow",
			"policy": map[string]any{"target_context_tokens": 4000},
		}
		return s, candidateID
	}
	assignments := []any{
		map[string]any{"assignment_id": "a1", "arm": "control", "completed": true},
		map[string]any{"assignment_id": "a2", "arm": "shadow", "completed": true},
		map[string]any{"assignment_id": "a3", "arm": "control", "completed": true},
		map[string]any{"assignment_id": "a4", "arm": "shadow", "completed": true},
		map[string]any{"assignment_id": "a4", "arm": "shadow", "completed": true},
	}
	base := map[string]any{
		"candidate_id": "candidate-t5-promotion", "project": "promotion-project",
		"control":       map[string]any{"sample_count": 1000, "first_pass_success_rate": 0.80, "repair_rate": 0.15, "average_followup_tokens": 200, "average_provider_total_tokens": 1000},
		"canary":        map[string]any{"sample_count": 1000, "first_pass_success_rate": 0.86, "repair_rate": 0.10, "average_followup_tokens": 170, "average_provider_total_tokens": 920},
		"assignments":   assignments,
		"drift_cohorts": []any{map[string]any{"cohort": "holdout", "sample_count": 100, "utility_delta": 0.01}},
	}
	s, _ := newCandidate(t)
	good, err := s.buildPolicyPromotionRecommendation(base)
	if err != nil {
		t.Fatalf("good promotion recommendation: %v", err)
	}
	receipt := anyMap(good["assignment_exposure_receipt"])
	if anyToInt(receipt["assignment_count"], 0) != 4 || anyToInt(receipt["duplicate_count"], 0) != 1 || !anyToBool(receipt["complete"]) || !anyToBool(receipt["survivor_bias_guard_passed"]) || !anyToBool(good["eligible"]) {
		t.Fatalf("assignment receipt did not pass: %#v", good)
	}

	s, _ = newCandidate(t)
	drift := cloneMap(base)
	drift["drift_cohorts"] = []any{map[string]any{"cohort": "holdout", "sample_count": 100, "utility_delta": -0.03}}
	driftResult, err := s.buildPolicyPromotionRecommendation(drift)
	if err != nil {
		t.Fatalf("drift recommendation: %v", err)
	}
	if anyToBool(driftResult["eligible"]) || anyToString(driftResult["recommendation"]) != "hold" || anyToBool(anyMap(driftResult["drift"])["passed"]) {
		t.Fatalf("drift guard did not hold promotion: %#v", driftResult)
	}

	s, _ = newCandidate(t)
	bias := cloneMap(base)
	bias["assignments"] = []any{
		map[string]any{"assignment_id": "b1", "arm": "control", "completed": true},
		map[string]any{"assignment_id": "b2", "arm": "shadow", "completed": false},
		map[string]any{"assignment_id": "b3", "arm": "control", "completed": false},
		map[string]any{"assignment_id": "b4", "arm": "shadow", "completed": false},
	}
	biasResult, err := s.buildPolicyPromotionRecommendation(bias)
	if err != nil {
		t.Fatalf("survivor-bias recommendation: %v", err)
	}
	biasReceipt := anyMap(biasResult["assignment_exposure_receipt"])
	if anyToBool(biasReceipt["survivor_bias_guard_passed"]) || anyToBool(biasResult["eligible"]) || anyToString(biasResult["recommendation"]) != "hold" {
		t.Fatalf("survivor-bias guard did not hold promotion: %#v", biasResult)
	}

	s, _ = newCandidate(t)
	missingDrift := cloneMap(base)
	delete(missingDrift, "drift_cohorts")
	missingDriftResult, err := s.buildPolicyPromotionRecommendation(missingDrift)
	if err != nil || anyToBool(missingDriftResult["eligible"]) || anyToBool(anyMap(missingDriftResult["drift"])["passed"]) {
		t.Fatalf("missing drift evidence did not fail closed: err=%v response=%#v", err, missingDriftResult)
	}

	s, _ = newCandidate(t)
	overlap := cloneMap(base)
	overlap["control"] = map[string]any{"sample_count": 20, "first_pass_success_rate": 0.80, "repair_rate": 0.15, "average_followup_tokens": 200, "average_provider_total_tokens": 1000}
	overlap["canary"] = map[string]any{"sample_count": 20, "first_pass_success_rate": 0.86, "repair_rate": 0.10, "average_followup_tokens": 170, "average_provider_total_tokens": 920}
	overlapResult, err := s.buildPolicyPromotionRecommendation(overlap)
	if err != nil || anyToBool(overlapResult["eligible"]) || anyToBool(anyMap(overlapResult["uncertainty"])["passed"]) {
		t.Fatalf("overlapping uncertainty did not fail closed: err=%v response=%#v", err, overlapResult)
	}
}

func TestFrontierT5RetirementApplyRetryRestoreAndGuards(t *testing.T) {
	s := newFrontierT5PolicyLabTestServer(t)
	entry := putFrontierT5TestMemory(t, s, "retirement-project", "notes/item.md", "retirement content", "durable", "hot", nil)
	state, err := frontierT5MemoryState(s.memoryStore, "retirement-project", "notes/item.md")
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"operation": "apply", "project": "retirement-project", "file": "notes/item.md", "approved": true, "expected_content_hash": state["content_hash"], "reason": "bounded retirement test"}
	first, err := s.buildMemoryRetirement(payload)
	if err != nil {
		t.Fatalf("retirement apply: %v", err)
	}
	receipt := anyMap(first["receipt"])
	if !anyToBool(first["ordinary_memory_rewritten"]) || anyToString(receipt["current_lifecycle"]) != "retired" || anyToString(receipt["content_hash"]) != entry.ContentHash {
		t.Fatalf("retirement receipt=%#v response=%#v", receipt, first)
	}
	retry, err := s.buildMemoryRetirement(payload)
	if err != nil || !anyToBool(retry["idempotent"]) {
		t.Fatalf("retry-safe retirement: err=%v response=%#v", err, retry)
	}
	restored, err := s.buildMemoryRetirement(map[string]any{"operation": "restore", "project": "retirement-project", "receipt_id": receipt["receipt_id"], "approved": true, "reason": "restore after review"})
	if err != nil {
		t.Fatalf("retirement restore: %v", err)
	}
	if anyToString(anyMap(restored["receipt"])["current_lifecycle"]) != "durable" {
		t.Fatalf("restore did not recover prior lifecycle: %#v", restored)
	}
	restoreRetry, err := s.buildMemoryRetirement(map[string]any{"operation": "restore", "project": "retirement-project", "receipt_id": receipt["receipt_id"], "approved": true, "reason": "restore after review"})
	if err != nil || !anyToBool(restoreRetry["idempotent"]) {
		t.Fatalf("retry-safe restore: err=%v response=%#v", err, restoreRetry)
	}

	hold := newFrontierT5PolicyLabTestServer(t)
	holdEntry := putFrontierT5TestMemory(t, hold, "retirement-project", "notes/hold.md", "held", "durable", "hot", []string{"legal_hold"})
	_, err = hold.buildMemoryRetirement(map[string]any{"operation": "apply", "project": "retirement-project", "file": holdEntry.FileName, "approved": true, "expected_content_hash": holdEntry.ContentHash, "reason": "must fail"})
	assertFrontierT5Error(t, err, "legal hold blocks")
	protected := newFrontierT5PolicyLabTestServer(t)
	protectedEntry := putFrontierT5TestMemory(t, protected, "retirement-project", "notes/protected.md", "protected", "durable", "hot", nil)
	protected.memoryStore.mu.Lock()
	protected.memoryStore.exactStatePaths[memoryStoreKey("retirement-project", protectedEntry.FileName)] = struct{}{}
	protected.memoryStore.mu.Unlock()
	_, err = protected.buildMemoryRetirement(map[string]any{"operation": "apply", "project": "retirement-project", "file": protectedEntry.FileName, "approved": true, "expected_content_hash": protectedEntry.ContentHash, "reason": "must fail"})
	assertFrontierT5Error(t, err, "cannot be retired")

	_, err = s.buildMemoryRetirement(map[string]any{"operation": "restore", "project": "other-project", "receipt_id": receipt["receipt_id"], "approved": true, "reason": "wrong project"})
	assertFrontierT5Error(t, err, "different project")

	disabled := newFrontierT5PolicyLabTestServer(t)
	disabledEntry := putFrontierT5TestMemory(t, disabled, "retirement-project", "notes/disabled.md", "disabled ledger", "durable", "hot", nil)
	disabledState, err := frontierT5MemoryState(disabled.memoryStore, "retirement-project", disabledEntry.FileName)
	if err != nil {
		t.Fatal(err)
	}
	beforeEntries := len(disabled.memoryStore.recent)
	disabled.frontierT5.enabled = false
	_, err = disabled.buildMemoryRetirement(map[string]any{"operation": "apply", "project": "retirement-project", "file": disabledEntry.FileName, "approved": true, "expected_content_hash": disabledState["content_hash"], "reason": "ledger refusal"})
	assertFrontierT5Error(t, err, "ledger is unavailable")
	if len(disabled.memoryStore.recent) != beforeEntries {
		t.Fatalf("disabled ledger mutated memory before refusing: before=%d after=%d", beforeEntries, len(disabled.memoryStore.recent))
	}
}

func TestFrontierT5ContradictionAbstentionWinnerDecisionReopenAppeal(t *testing.T) {
	s := newFrontierT5PolicyLabTestServer(t)
	upsertFrontierT5Claim(t, s, map[string]any{"project": "claims-project", "claim_id": "claim-a", "subject": "service", "predicate": "status", "object": "ready", "contradicts": []any{"claim-b"}, "confidence": 0.7, "verification": map[string]any{"status": "unverified"}})
	upsertFrontierT5Claim(t, s, map[string]any{"project": "claims-project", "claim_id": "claim-b", "subject": "service", "predicate": "status", "object": "blocked", "contradicts": []any{"claim-a"}, "confidence": 0.7, "verification": map[string]any{"status": "unverified"}})
	abstained, err := s.buildContradictionResolution(map[string]any{"operation": "recommend", "project": "claims-project", "claim_ids": []any{"claim-a", "claim-b"}})
	if err != nil {
		t.Fatalf("abstention recommendation: %v", err)
	}
	abstainRecommendation := anyMap(abstained["recommendation"])
	if anyToString(abstainRecommendation["status"]) != "abstained" || anyToString(abstainRecommendation["winning_claim_id"]) != "" {
		t.Fatalf("unverified contradiction did not abstain: %#v", abstainRecommendation)
	}

	s = newFrontierT5PolicyLabTestServer(t)
	upsertFrontierT5Claim(t, s, map[string]any{"project": "claims-project", "claim_id": "claim-a", "subject": "service", "predicate": "status", "object": "ready", "contradicts": []any{"claim-b"}, "confidence": 0.95, "support": []any{map[string]any{"ref_id": "ref-a", "kind": "test"}}, "verification": map[string]any{"status": "verified"}, "provenance": map[string]any{"source": "fixture"}})
	upsertFrontierT5Claim(t, s, map[string]any{"project": "claims-project", "claim_id": "claim-b", "subject": "service", "predicate": "status", "object": "blocked", "contradicts": []any{"claim-a"}, "confidence": 0.5, "verification": map[string]any{"status": "unverified"}})
	decided, err := s.buildContradictionResolution(map[string]any{"operation": "decide", "project": "claims-project", "claim_ids": []any{"claim-a", "claim-b"}, "winning_claim_id": "claim-a", "approved": true, "operator": "operator-1", "reason": "verified evidence wins"})
	if err != nil {
		t.Fatalf("contradiction decision: %v", err)
	}
	resolution := anyMap(decided["resolution"])
	if anyToString(anyMap(decided["recommendation"])["status"]) != "recommended" || anyToString(resolution["status"]) != "resolved" || !anyToBool(resolution["appealable"]) {
		t.Fatalf("winner decision was not recorded as appealable: %#v", decided)
	}
	statuses, err := frontierT5CurrentClaimStatuses(s.temporalClaims, "claims-project", "claim-a", []string{"claim-b"})
	if err != nil || statuses["claim-a"] != "active" || statuses["claim-b"] != "retracted" {
		t.Fatalf("claim statuses after decision: err=%v statuses=%#v", err, statuses)
	}

	wrongProject, err := s.buildContradictionResolution(map[string]any{"operation": "reopen", "project": "other-project", "resolution_id": resolution["resolution_id"], "approved": true, "operator": "operator-1", "reason": "wrong project"})
	if wrongProject != nil {
		t.Fatalf("wrong-project reopen returned response: %#v", wrongProject)
	}
	assertFrontierT5Error(t, err, "different project")
	reopened, err := s.buildContradictionResolution(map[string]any{"operation": "reopen", "project": "claims-project", "resolution_id": resolution["resolution_id"], "approved": true, "operator": "operator-1", "reason": "reopen for review"})
	if err != nil {
		t.Fatalf("reopen contradiction: %v", err)
	}
	if anyToString(anyMap(reopened["resolution"])["status"]) != "reopened" {
		t.Fatalf("reopen response=%#v", reopened)
	}
	statuses, err = frontierT5CurrentClaimStatuses(s.temporalClaims, "claims-project", "claim-a", []string{"claim-b"})
	if err != nil || statuses["claim-a"] != "active" || statuses["claim-b"] != "active" {
		t.Fatalf("claim statuses were not restored: err=%v statuses=%#v", err, statuses)
	}
	appealed, err := s.buildContradictionResolution(map[string]any{"operation": "appeal", "project": "claims-project", "resolution_id": resolution["resolution_id"], "approved": true, "operator": "operator-1", "reason": "operator appeal"})
	if err != nil || anyToString(anyMap(appealed["resolution"])["status"]) != "appealed" {
		t.Fatalf("appeal response: err=%v response=%#v", err, appealed)
	}
}

func TestFrontierT5ContradictionDecisionRejectsForeignAndUnrelatedClaims(t *testing.T) {
	s := newFrontierT5PolicyLabTestServer(t)
	upsertFrontierT5Claim(t, s, map[string]any{"project": "claims-project", "claim_id": "claim-a", "subject": "service", "predicate": "status", "object": "ready", "contradicts": []any{"claim-b"}, "confidence": 0.95, "support": []any{map[string]any{"ref_id": "ref-a", "kind": "test"}}, "verification": map[string]any{"status": "verified"}})
	upsertFrontierT5Claim(t, s, map[string]any{"project": "claims-project", "claim_id": "claim-b", "subject": "service", "predicate": "status", "object": "blocked", "contradicts": []any{"claim-a"}, "confidence": 0.4})
	upsertFrontierT5Claim(t, s, map[string]any{"project": "claims-project", "claim_id": "claim-unrelated", "subject": "other", "predicate": "state", "object": "idle", "confidence": 0.2})
	upsertFrontierT5Claim(t, s, map[string]any{"project": "foreign-project", "claim_id": "claim-foreign", "subject": "foreign", "predicate": "state", "object": "idle", "confidence": 0.2})

	result, err := s.buildContradictionResolution(map[string]any{"operation": "decide", "project": "claims-project", "claim_ids": []any{"claim-a", "claim-b", "claim-unrelated"}, "winning_claim_id": "claim-a", "approved": true, "operator": "operator-1", "reason": "direct conflict only"})
	if err != nil {
		t.Fatalf("direct contradiction decision: %v", err)
	}
	losers := anyToStringList(anyMap(result["resolution"])["losing_claim_ids"], 64)
	if len(losers) != 1 || losers[0] != "claim-b" {
		t.Fatalf("unrelated claim entered loser set: %#v", result)
	}
	statuses, err := frontierT5CurrentClaimStatuses(s.temporalClaims, "claims-project", "claim-unrelated", nil)
	if err != nil || statuses["claim-unrelated"] != "active" {
		t.Fatalf("unrelated claim was mutated: err=%v statuses=%#v", err, statuses)
	}
	_, err = s.buildContradictionResolution(map[string]any{"operation": "decide", "project": "claims-project", "claim_ids": []any{"claim-a", "claim-foreign"}, "winning_claim_id": "claim-a", "approved": true, "operator": "operator-1", "reason": "foreign rejection"})
	assertFrontierT5Error(t, err, "different project")
}

func TestFrontierT5StorageTemperatureRecommendApplyNoOpRestoreAndProjectMismatch(t *testing.T) {
	s := newFrontierT5PolicyLabTestServer(t)
	entry := putFrontierT5TestMemory(t, s, "temperature-project", "notes/temp.md", "temperature content", "durable", "hot", nil)
	recommend, err := s.buildStorageTemperatureDecision(map[string]any{"operation": "recommend", "project": "temperature-project", "file": entry.FileName})
	if err != nil {
		t.Fatalf("temperature recommendation: %v", err)
	}
	decisions := recommend["decisions"].([]any)
	if len(decisions) != 1 || anyToString(anyMap(decisions[0])["recommended_tier"]) != "hot" {
		t.Fatalf("unexpected temperature recommendation: %#v", recommend)
	}
	state, err := frontierT5MemoryState(s.memoryStore, "temperature-project", entry.FileName)
	if err != nil {
		t.Fatal(err)
	}
	applyPayload := map[string]any{"operation": "apply", "project": "temperature-project", "file": entry.FileName, "tier": "warm", "approved": true, "expected_content_hash": state["content_hash"], "reason": "bounded warm move"}
	applied, err := s.buildStorageTemperatureDecision(applyPayload)
	if err != nil {
		t.Fatalf("temperature apply: %v", err)
	}
	applyReceipt := anyMap(applied["receipt"])
	if !anyToBool(applied["retrieval_temperature_changed"]) || anyToString(applyReceipt["current_tier"]) != "warm" {
		t.Fatalf("temperature apply receipt=%#v response=%#v", applyReceipt, applied)
	}
	noOp := cloneMap(applyPayload)
	noOp["reason"] = "same tier no-op"
	noOpResult, err := s.buildStorageTemperatureDecision(noOp)
	if err != nil || anyToBool(noOpResult["retrieval_temperature_changed"]) {
		t.Fatalf("same-tier no-op: err=%v response=%#v", err, noOpResult)
	}
	restored, err := s.buildStorageTemperatureDecision(map[string]any{"operation": "restore", "project": "temperature-project", "receipt_id": applyReceipt["receipt_id"], "approved": true, "reason": "restore hot"})
	if err != nil {
		t.Fatalf("temperature restore: %v", err)
	}
	if anyToString(anyMap(restored["receipt"])["current_tier"]) != "hot" {
		t.Fatalf("temperature restore response=%#v", restored)
	}
	wrongProject, err := s.buildStorageTemperatureDecision(map[string]any{"operation": "restore", "project": "other-project", "receipt_id": applyReceipt["receipt_id"], "approved": true, "reason": "wrong project"})
	if wrongProject != nil {
		t.Fatalf("wrong-project restore returned response: %#v", wrongProject)
	}
	assertFrontierT5Error(t, err, "different project")
}

func TestFrontierT5StorageTemperatureRestoreRejectsSupersededReceipt(t *testing.T) {
	s := newFrontierT5PolicyLabTestServer(t)
	entry := putFrontierT5TestMemory(t, s, "temperature-project", "notes/lineage.md", "lineage", "durable", "hot", nil)
	state, err := frontierT5MemoryState(s.memoryStore, "temperature-project", entry.FileName)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.buildStorageTemperatureDecision(map[string]any{"operation": "apply", "project": "temperature-project", "file": entry.FileName, "tier": "warm", "approved": true, "expected_content_hash": state["content_hash"], "reason": "first transition"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.buildStorageTemperatureDecision(map[string]any{"operation": "apply", "project": "temperature-project", "file": entry.FileName, "tier": "deep", "approved": true, "expected_content_hash": state["content_hash"], "reason": "second transition"})
	if err != nil || anyToString(anyMap(second["receipt"])["current_tier"]) != "deep" {
		t.Fatalf("second transition: err=%v response=%#v", err, second)
	}
	_, err = s.buildStorageTemperatureDecision(map[string]any{"operation": "restore", "project": "temperature-project", "receipt_id": anyMap(first["receipt"])["receipt_id"], "approved": true, "reason": "stale restore"})
	assertFrontierT5Error(t, err, "superseded this receipt")
}

func TestFrontierT5StorageTemperaturePressurePreservesHighConfidenceAndCoolsSafeEvidence(t *testing.T) {
	highConfidence := map[string]any{"lifecycle": "active", "confidence": 0.95, "updated_at": time.Now().UTC().Add(-120 * 24 * time.Hour).Format(time.RFC3339Nano)}
	if tier, _ := frontierT5RecommendedStorageTier(highConfidence, map[string]any{"disk_pressure": "critical"}); tier != "hot" {
		t.Fatalf("critical pressure displaced rare high-confidence evidence: %s", tier)
	}
	stale := map[string]any{"lifecycle": "active", "confidence": 0.4, "updated_at": time.Now().UTC().Add(-45 * 24 * time.Hour).Format(time.RFC3339Nano)}
	if tier, reasons := frontierT5RecommendedStorageTier(stale, map[string]any{"disk_pressure": "high"}); tier != "deep" || len(reasons) == 0 {
		t.Fatalf("high pressure did not produce a bounded reversible deep recommendation: tier=%s reasons=%#v", tier, reasons)
	}
	held := map[string]any{"lifecycle": "active", "confidence": 0.1, "tags": []string{"legal_hold"}, "updated_at": time.Now().UTC().Add(-365 * 24 * time.Hour).Format(time.RFC3339Nano)}
	if tier, _ := frontierT5RecommendedStorageTier(held, map[string]any{"disk_pressure": "critical"}); tier != "hot" {
		t.Fatalf("pressure bypassed legal hold: %s", tier)
	}
}

func TestFrontierT5ColdDocsAndUntrackedInspection(t *testing.T) {
	s := newFrontierT5PolicyLabTestServer(t)
	putFrontierT5TestMemory(t, s, "docs-project", "notes/hot.md", "hot", "durable", "hot", nil)
	putFrontierT5TestMemory(t, s, "docs-project", "notes/deep.md", "deep", "durable", "deep", nil)
	putFrontierT5TestMemory(t, s, "docs-project", "notes/retired.md", "retired", "durable", "retired", nil)
	putFrontierT5TestMemory(t, s, "other-project", "notes/other.md", "other", "durable", "hot", nil)
	defaultDocs, err := s.memoryStore.collectDocs(context.Background(), "docs-project", false, false)
	if err != nil {
		t.Fatalf("default docs: %v", err)
	}
	includeColdDocs, err := s.memoryStore.collectDocs(context.Background(), "docs-project", true, false)
	if err != nil {
		t.Fatalf("include-cold docs: %v", err)
	}
	contains := func(docs []memoryStoreDoc, file string) bool {
		for _, doc := range docs {
			if doc.FileName == file {
				return true
			}
		}
		return false
	}
	if contains(defaultDocs, "notes/deep.md") || contains(defaultDocs, "notes/retired.md") || !contains(defaultDocs, "notes/hot.md") {
		t.Fatalf("default collectDocs did not suppress cold entries: %#v", defaultDocs)
	}
	if !contains(includeColdDocs, "notes/deep.md") || !contains(includeColdDocs, "notes/retired.md") || contains(includeColdDocs, "notes/other.md") {
		t.Fatalf("includeCold/project filter mismatch: %#v", includeColdDocs)
	}

	key := memoryStoreKey("docs-project", "notes/hot.md")
	sentinel := time.Date(2024, time.January, 2, 3, 4, 5, 0, time.UTC)
	s.memoryStore.mu.Lock()
	s.memoryStore.lastAccess[key] = sentinel
	s.memoryStore.mu.Unlock()
	if _, _, _, _, err := s.memoryStore.readFileUntracked("docs-project", "notes/hot.md"); err != nil {
		t.Fatalf("untracked inspection: %v", err)
	}
	s.memoryStore.mu.RLock()
	got := s.memoryStore.lastAccess[key]
	s.memoryStore.mu.RUnlock()
	if !got.Equal(sentinel) {
		t.Fatalf("untracked inspection changed last access: got=%s want=%s", got, sentinel)
	}
}

func TestFrontierT5LedgerRestartBoundedCompactionAndPreparationIdempotency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.ndjson")
	ledger, err := newFrontierT5LedgerForTest(path)
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}
	ledger.maxEntries = 3
	ledger.maxBytes = 1
	for i := 0; i < 5; i++ {
		if recorded, err := ledger.record(map[string]any{"schema_id": memoryRetirementContractID, "receipt_id": "receipt-" + anyToString(i), "project": "ledger-project", "file": "notes/item.md", "recorded_at": "2026-07-18T00:00:0" + anyToString(i) + "Z"}); err != nil || !recorded {
			t.Fatalf("record ledger row %d: recorded=%v err=%v", i, recorded, err)
		}
	}
	if anyToInt(ledger.snapshot()["entry_count"], 0) != 3 || anyToInt(ledger.snapshot()["compaction_count"], 0) == 0 {
		t.Fatalf("ledger was not bounded/compacted: %#v", ledger.snapshot())
	}
	loaded, err := newFrontierT5LedgerForTest(path)
	if err != nil {
		t.Fatalf("restart ledger: %v", err)
	}
	if len(loaded.receipt("receipt-4")) == 0 || len(loaded.receipt("receipt-0")) != 0 {
		t.Fatalf("restart did not preserve bounded latest rows: %#v", loaded.snapshot())
	}
	prepared, recorded, err := loaded.prepareMutation("transaction-1", map[string]any{"mutation": "test", "project": "ledger-project"})
	if err != nil || !recorded || anyToString(prepared["transaction_receipt_id"]) != "transaction-1" {
		t.Fatalf("first preparation: recorded=%v err=%v prepared=%#v", recorded, err, prepared)
	}
	replayed, recorded, err := loaded.prepareMutation("transaction-1", map[string]any{"mutation": "different"})
	if err != nil || recorded || !reflect.DeepEqual(prepared, replayed) {
		t.Fatalf("preparation was not idempotent: recorded=%v err=%v first=%#v replay=%#v", recorded, err, prepared, replayed)
	}
}

func TestFrontierT5HTTPRejectsNonStatusGETAndAttachesFormatContract(t *testing.T) {
	s := newFrontierT5PolicyLabTestServer(t)
	getRecorder := httptest.NewRecorder()
	s.memoryPolicySimulation(getRecorder, httptest.NewRequest(http.MethodGet, policySimulationPath, nil))
	if getRecorder.Code != http.StatusMethodNotAllowed || !strings.Contains(getRecorder.Body.String(), "GET supports operation=status only") {
		t.Fatalf("non-status GET was not rejected: status=%d body=%s", getRecorder.Code, getRecorder.Body.String())
	}
	body := bytes.NewBufferString(`{"project":"http-project","agent_id":"t5-test","cases":[{"case_id":"http-case","snapshot_id":"snap-http","snapshot_digest":"digest-http","baseline":{"tokens":100},"candidate":{"tokens":90}}]}`)
	postRecorder := httptest.NewRecorder()
	s.memoryPolicySimulation(postRecorder, httptest.NewRequest(http.MethodPost, policySimulationPath, body))
	if postRecorder.Code != http.StatusOK {
		t.Fatalf("valid HTTP simulation status=%d body=%s", postRecorder.Code, postRecorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(postRecorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode HTTP simulation: %v", err)
	}
	format := anyMap(payload["format_contract"])
	if anyToString(format["schema_id"]) != policySimulationContractID || anyToString(anyMap(format["validation"])["status"]) != "passed" {
		t.Fatalf("format contract was not attached and validated: %#v", format)
	}
	assertBoundaryContractPassed(t, policySimulationContractID, payload)
}

type frontierT5HoldoutCase struct {
	ID       string         `json:"id"`
	Item     int            `json:"item"`
	Surface  string         `json:"surface"`
	Input    map[string]any `json:"input"`
	Expected map[string]any `json:"expected"`
}

func TestFrontierT5PolicyLaboratoryHoldout(t *testing.T) {
	fixturePath := filepath.Join("..", "..", "docs", "evals", "fixtures", "frontier-t5-policy-laboratory-holdout.v1.json")
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	fixture := struct {
		SchemaID string                  `json:"schema_id"`
		Cases    []frontierT5HoldoutCase `json:"cases"`
	}{}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.SchemaID != "frontier_t5_policy_laboratory_holdout.v1" || len(fixture.Cases) != 18 {
		t.Fatalf("unexpected frozen holdout: schema=%s cases=%d", fixture.SchemaID, len(fixture.Cases))
	}
	started := time.Now()
	seenIDs := map[string]struct{}{}
	coveredItems := map[int]int{}
	passed := 0
	for _, testCase := range fixture.Cases {
		if testCase.ID == "" {
			t.Fatal("holdout case id is empty")
		}
		if _, exists := seenIDs[testCase.ID]; exists {
			t.Fatalf("duplicate holdout case id: %s", testCase.ID)
		}
		seenIDs[testCase.ID] = struct{}{}
		coveredItems[testCase.Item]++
		switch testCase.Surface {
		case "policy_simulation":
			result, simulationErr := buildPolicySimulation(cloneMap(testCase.Input))
			expectError := anyToBool(testCase.Expected["error"])
			if (simulationErr != nil) != expectError {
				t.Fatalf("%s: error=%v expected_error=%v", testCase.ID, simulationErr, expectError)
			}
			if simulationErr == nil {
				computation := anyMap(result["computation"])
				if anyToBool(computation["persisted"]) || anyToBool(computation["runtime_policy_mutated"]) || anyToInt(computation["network_calls"], -1) != 0 {
					t.Fatalf("%s: simulation crossed its no-persist boundary: %#v", testCase.ID, result)
				}
				if expected := testCase.Expected["tokens_delta"]; expected != nil {
					average := anyMap(anyMap(result["summary"])["average_deltas"])
					if math.Abs(anyToFloat(average["tokens"])-anyToFloat(expected)) > 0.001 {
						t.Fatalf("%s: token delta=%v expected=%v", testCase.ID, average["tokens"], expected)
					}
				}
			}
		case "policy_dimension":
			value, dimensionErr := frontierT5PolicyDimension(anyToString(testCase.Input["raw"]), anyToString(testCase.Input["fallback"]))
			expectError := anyToBool(testCase.Expected["error"])
			if (dimensionErr != nil) != expectError || (!expectError && value != anyToString(testCase.Expected["value"])) {
				t.Fatalf("%s: value=%q error=%v expected=%#v", testCase.ID, value, dimensionErr, testCase.Expected)
			}
		case "assignment_receipt":
			receipt := frontierT5AssignmentReceipt(testCase.Input)
			for _, key := range []string{"complete", "survivor_bias_guard_passed"} {
				if anyToBool(receipt[key]) != anyToBool(testCase.Expected[key]) {
					t.Fatalf("%s: %s=%v expected=%v", testCase.ID, key, receipt[key], testCase.Expected[key])
				}
			}
			if anyToInt(receipt["leakage_count"], -1) != anyToInt(testCase.Expected["leakage_count"], -2) {
				t.Fatalf("%s: leakage_count=%v expected=%v", testCase.ID, receipt["leakage_count"], testCase.Expected["leakage_count"])
			}
		case "legal_hold":
			held := frontierT5HasLegalHold(anyToStringList(testCase.Input["tags"], 64), anyMap(testCase.Input["payload"]))
			if held != anyToBool(testCase.Expected["held"]) {
				t.Fatalf("%s: held=%v expected=%v", testCase.ID, held, testCase.Expected["held"])
			}
		case "contradiction_recommendation":
			claimsRaw, marshalErr := json.Marshal(testCase.Input["claims"])
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			claims := []temporalClaim{}
			if err := json.Unmarshal(claimsRaw, &claims); err != nil {
				t.Fatal(err)
			}
			recommendation := frontierT5ContradictionRecommendation("holdout", claims, anyToFloat(testCase.Input["threshold"]))
			if anyToString(recommendation["status"]) != anyToString(testCase.Expected["status"]) || anyToString(recommendation["winning_claim_id"]) != anyToString(testCase.Expected["winner"]) {
				t.Fatalf("%s: recommendation=%#v expected=%#v", testCase.ID, recommendation, testCase.Expected)
			}
		case "storage_temperature":
			tier, reasons := frontierT5RecommendedStorageTier(anyMap(testCase.Input["state"]), anyMap(testCase.Input["policy"]))
			if tier != anyToString(testCase.Expected["tier"]) || len(reasons) == 0 {
				t.Fatalf("%s: tier=%s reasons=%#v expected=%#v", testCase.ID, tier, reasons, testCase.Expected)
			}
		default:
			t.Fatalf("%s: unsupported holdout surface %s", testCase.ID, testCase.Surface)
		}
		passed++
	}
	for _, item := range []int{19, 10, 3, 8, 9, 27} {
		if coveredItems[item] == 0 {
			t.Fatalf("holdout omitted Frontier item %d", item)
		}
	}
	if output := strings.TrimSpace(os.Getenv("FRONTIER_T5_POLICY_LAB_EVIDENCE_PATH")); output != "" {
		evidence := map[string]any{
			"schema_id": "frontier_t5_policy_laboratory_eval.v1", "tested_commit": os.Getenv("FRONTIER_T5_TESTED_COMMIT"),
			"holdout_schema_id": fixture.SchemaID, "case_count": len(fixture.Cases), "passed_count": passed,
			"failed_count": len(fixture.Cases) - passed, "item_case_counts": coveredItems,
			"duration_ms":    roundFloat(float64(time.Since(started).Microseconds())/1000, 6),
			"provider_calls": 0, "local_inference_calls": 0, "external_network_calls": 0,
			"ordinary_memory_deleted": false, "automatic_runtime_activation": false,
		}
		encoded, err := json.MarshalIndent(evidence, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(output, append(encoded, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
