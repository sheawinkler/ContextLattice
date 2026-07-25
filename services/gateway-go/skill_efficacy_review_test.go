package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func skillEfficacyTestSkill(t *testing.T, root, sourceKind string) (map[string]any, string) {
	t.Helper()
	path := filepath.Join(root, "skills_active", "verified-release-gate", "SKILL.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read skill fixture: %v", err)
	}
	return map[string]any{
		"id": "skill_verified_release_gate", "name": "verified-release-gate", "version": "1.0.0",
		"digest": "sha256:" + sha256Hex(string(raw)), "source_kind": sourceKind,
		"source_ref": "fixture/verified-release-gate",
	}, path
}

func skillEfficacyTestSearchInput(usageID, sessionID string, skill map[string]any) map[string]any {
	return map[string]any{
		"project": "contextlattice", "usage_id": usageID, "idempotency_key": usageID + "-searched",
		"stage": "searched", "session_id": sessionID, "agent_id": "codex_test", "skill": skill,
		"search": map[string]any{
			"query_digest": utilityTestDigest("query:" + usageID), "rank": 1,
			"matched_terms": []any{"verified", "release"},
		},
	}
}

func skillEfficacyTestRecord(t *testing.T, handler http.Handler, input map[string]any) map[string]any {
	t.Helper()
	recorder, payload := frontierT8RouteRequest(t, handler, http.MethodPost, map[string]any{
		"operation": frontierT8OperationRecordUsage, "agent_id": "codex_test", "input": input,
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("record usage status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	frontierT8AssertRouteContract(t, payload, skillUsageReceiptContractID)
	return payload
}

func skillEfficacyTestSeedOutcome(
	t *testing.T,
	s *server,
	outcomeID, sessionID string,
	value float64,
	pairing map[string]any,
) {
	t.Helper()
	outcome, quality, impact, verificationEvents := utilityTestFixture(
		outcomeID, "sample_"+outcomeID, sessionID, "skill-assisted-coding",
		"contextlattice", value, 400, pairing,
	)
	observation := buildUtilityObservation(outcome, quality, impact, verificationEvents)
	if anyToString(observation["status"]) != "verified_exact" {
		t.Fatalf("utility fixture is not independently verified: %#v", observation)
	}
	if _, _, err := s.utility.record(observation); err != nil {
		t.Fatalf("record utility fixture: %v", err)
	}
	outcomeEvent := map[string]any{
		"id": "evt_session_" + outcomeID, "session_id": sessionID,
		"type": "context_pack.outcome_reported", "agent_id": "codex_test",
		"project": "contextlattice", "created_at": nowUTCISO(),
		"metadata": map[string]any{"outcome": map[string]any{
			"outcome_id": outcomeID, "first_pass_success": true,
			"repair_required": false, "retry_count": 0,
		}},
	}
	s.agentSessions.mu.Lock()
	s.agentSessions.sessions[sessionID] = map[string]any{
		"id": sessionID, "project": "contextlattice", "agent_id": "codex_test",
		"status": "done", "created_at": nowUTCISO(), "updated_at": nowUTCISO(),
	}
	s.agentSessions.order = append(s.agentSessions.order, sessionID)
	s.agentSessions.events[sessionID] = []map[string]any{outcomeEvent, verificationEvents[0]}
	s.agentSessions.mu.Unlock()
}

func skillEfficacyTestFullUsage(
	t *testing.T,
	s *server,
	handler http.Handler,
	usageID, sessionID, outcomeID string,
	skill map[string]any,
	value float64,
	pairing map[string]any,
) map[string]any {
	t.Helper()
	search := skillEfficacyTestRecord(t, handler, skillEfficacyTestSearchInput(usageID, sessionID, skill))
	selected := skillEfficacyTestRecord(t, handler, map[string]any{
		"usage_id": usageID, "idempotency_key": usageID + "-selected", "stage": "selected",
		"expected_previous_receipt_digest": anyMap(search["receipt"])["receipt_digest"],
		"selection":                        map[string]any{"reason_code": "agent_judgment"},
	})
	invoked := skillEfficacyTestRecord(t, handler, map[string]any{
		"usage_id": usageID, "idempotency_key": usageID + "-invoked", "stage": "invoked",
		"expected_previous_receipt_digest": anyMap(selected["receipt"])["receipt_digest"],
		"invocation":                       map[string]any{"mode": "workflow"},
	})
	skillEfficacyTestSeedOutcome(t, s, outcomeID, sessionID, value, pairing)
	return skillEfficacyTestRecord(t, handler, map[string]any{
		"usage_id": usageID, "idempotency_key": usageID + "-outcome", "stage": "verified_outcome",
		"expected_previous_receipt_digest": anyMap(invoked["receipt"])["receipt_digest"],
		"outcome":                          map[string]any{"outcome_id": outcomeID},
	})
}

func TestSkillUsageReceiptChainFailsClosedAndReplaysAfterCompaction(t *testing.T) {
	s, handler, root := frontierT8RouteTestServer(t)
	skill, _ := skillEfficacyTestSkill(t, root, "local")
	usageID, sessionID := "usage_chain", "session_chain"
	searchInput := skillEfficacyTestSearchInput(usageID, sessionID, skill)
	search := skillEfficacyTestRecord(t, handler, searchInput)
	searchReceipt := anyMap(search["receipt"])
	if anyToString(searchReceipt["stage"]) != skillUsageStageSearched || anyToBool(searchReceipt["efficacy_eligible"]) {
		t.Fatalf("search received efficacy credit: %#v", searchReceipt)
	}
	if strings.Contains(strings.ToLower(fmt.Sprint(searchReceipt)), "raw query") {
		t.Fatalf("search receipt retained raw query material: %#v", searchReceipt)
	}

	replay := skillEfficacyTestRecord(t, handler, searchInput)
	if !anyToBool(replay["replayed"]) || anyToString(anyMap(replay["receipt"])["receipt_digest"]) != anyToString(searchReceipt["receipt_digest"]) {
		t.Fatalf("exact idempotent replay failed: %#v", replay)
	}
	conflictInput := cloneMap(searchInput)
	anyMap(conflictInput["search"])["rank"] = 2
	conflictRecorder, conflictPayload := frontierT8RouteRequest(t, handler, http.MethodPost, map[string]any{
		"operation": frontierT8OperationRecordUsage, "input": conflictInput,
	})
	if conflictRecorder.Code != http.StatusUnprocessableEntity || !strings.Contains(anyToString(conflictPayload["detail"]), "conflicts") {
		t.Fatalf("idempotency conflict was not rejected status=%d body=%s", conflictRecorder.Code, conflictRecorder.Body.String())
	}

	skipRecorder, _ := frontierT8RouteRequest(t, handler, http.MethodPost, map[string]any{
		"operation": frontierT8OperationRecordUsage,
		"input": map[string]any{
			"usage_id": usageID, "idempotency_key": usageID + "-skip", "stage": "invoked",
			"expected_previous_receipt_digest": searchReceipt["receipt_digest"],
			"invocation":                       map[string]any{"mode": "workflow"},
		},
	})
	if skipRecorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("skipped usage stage was accepted: %s", skipRecorder.Body.String())
	}

	selected := skillEfficacyTestRecord(t, handler, map[string]any{
		"usage_id": usageID, "idempotency_key": usageID + "-selected", "stage": "selected",
		"expected_previous_receipt_digest": searchReceipt["receipt_digest"],
		"selection":                        map[string]any{"reason_code": "top_match"},
	})
	if anyToBool(anyMap(selected["receipt"])["efficacy_eligible"]) {
		t.Fatalf("selected but non-invoked use received efficacy credit: %#v", selected["receipt"])
	}
	invoked := skillEfficacyTestRecord(t, handler, map[string]any{
		"usage_id": usageID, "idempotency_key": usageID + "-invoked", "stage": "invoked",
		"expected_previous_receipt_digest": anyMap(selected["receipt"])["receipt_digest"],
		"invocation":                       map[string]any{"mode": "workflow"},
	})
	finalInput := map[string]any{
		"usage_id": usageID, "idempotency_key": usageID + "-outcome", "stage": "verified_outcome",
		"expected_previous_receipt_digest": anyMap(invoked["receipt"])["receipt_digest"],
		"outcome":                          map[string]any{"outcome_id": "outcome_chain"},
	}
	unprovenRecorder, unprovenPayload := frontierT8RouteRequest(t, handler, http.MethodPost, map[string]any{
		"operation": frontierT8OperationRecordUsage, "input": finalInput,
	})
	if unprovenRecorder.Code != http.StatusUnprocessableEntity || !strings.Contains(anyToString(unprovenPayload["detail"]), "Utility Ledger") {
		t.Fatalf("unproven outcome was accepted status=%d body=%s", unprovenRecorder.Code, unprovenRecorder.Body.String())
	}
	skillEfficacyTestSeedOutcome(t, s, "outcome_chain", sessionID, 5, nil)
	s.agentSessions.mu.Lock()
	sessionFixture := s.agentSessions.sessions[sessionID]
	eventFixture := s.agentSessions.events[sessionID]
	delete(s.agentSessions.sessions, sessionID)
	delete(s.agentSessions.events, sessionID)
	s.agentSessions.mu.Unlock()
	unboundRecorder, unboundPayload := frontierT8RouteRequest(t, handler, http.MethodPost, map[string]any{
		"operation": frontierT8OperationRecordUsage, "input": finalInput,
	})
	if unboundRecorder.Code != http.StatusUnprocessableEntity || !strings.Contains(anyToString(unboundPayload["detail"]), "session") {
		t.Fatalf("Utility-only outcome was accepted status=%d body=%s", unboundRecorder.Code, unboundRecorder.Body.String())
	}
	s.agentSessions.mu.Lock()
	s.agentSessions.sessions[sessionID] = sessionFixture
	s.agentSessions.events[sessionID] = eventFixture
	s.agentSessions.mu.Unlock()
	final := skillEfficacyTestRecord(t, handler, finalInput)
	finalReceipt := anyMap(final["receipt"])
	if !anyToBool(finalReceipt["efficacy_eligible"]) || anyToString(finalReceipt["stage"]) != skillUsageStageVerifiedOutcome ||
		len(contextPackAnyList(finalReceipt["stage_events"])) != 4 {
		t.Fatalf("verified outcome did not complete the receipt chain: %#v", finalReceipt)
	}

	if err := s.skillFoundry.compact(); err != nil {
		t.Fatalf("compact skill efficacy ledger: %v", err)
	}
	restarted, err := newSkillFoundryStoreFromEnv()
	if err != nil {
		t.Fatalf("reload compacted skill efficacy ledger: %v", err)
	}
	s.skillFoundry = restarted
	compactedReplay := skillEfficacyTestRecord(t, handler, finalInput)
	if !anyToBool(compactedReplay["replayed"]) ||
		anyToString(anyMap(compactedReplay["receipt"])["receipt_digest"]) != anyToString(finalReceipt["receipt_digest"]) {
		t.Fatalf("compacted exact replay failed: %#v", compactedReplay)
	}
}

func TestSkillUsageReceiptConcurrentIdempotencyWritesOnce(t *testing.T) {
	s, _, root := frontierT8RouteTestServer(t)
	skill, _ := skillEfficacyTestSkill(t, root, "local")
	input := skillEfficacyTestSearchInput("usage_concurrent", "session_concurrent", skill)
	type result struct {
		payload map[string]any
		err     error
	}
	const callers = 12
	start := make(chan struct{})
	results := make(chan result, callers)
	var workers sync.WaitGroup
	for index := 0; index < callers; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			payload, err := s.frontierT8RecordSkillUsage(input, time.Now().UTC())
			results <- result{payload: payload, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	digest := ""
	replays := 0
	for item := range results {
		if item.err != nil {
			t.Fatalf("concurrent idempotency failed: %v", item.err)
		}
		current := anyToString(anyMap(item.payload["receipt"])["receipt_digest"])
		if digest == "" {
			digest = current
		} else if current != digest {
			t.Fatalf("concurrent replay returned divergent receipts: %q != %q", current, digest)
		}
		if anyToBool(item.payload["replayed"]) {
			replays++
		}
	}
	if replays != callers-1 {
		t.Fatalf("concurrent idempotency replay count=%d want=%d", replays, callers-1)
	}
	s.skillFoundry.mu.RLock()
	transactionCount := len(s.skillFoundry.transactions)
	s.skillFoundry.mu.RUnlock()
	if transactionCount != 1 {
		t.Fatalf("concurrent idempotency persisted %d transactions", transactionCount)
	}
}

func TestSkillEfficacyReviewSearchOnlyAbstainsWithoutEfficacyCredit(t *testing.T) {
	s, handler, root := frontierT8RouteTestServer(t)
	skill, _ := skillEfficacyTestSkill(t, root, "third_party")
	usageID := "usage_search_only"
	skillEfficacyTestRecord(t, handler, skillEfficacyTestSearchInput(usageID, "session_search_only", skill))
	recorder, payload := frontierT8RouteRequest(t, handler, http.MethodPost, map[string]any{
		"operation": frontierT8OperationReviewEfficacy,
		"input": map[string]any{
			"project": "contextlattice", "skill_id": skill["id"], "name": skill["name"],
			"idempotency_key": "review-search-only", "baseline_usage_ids": []any{usageID},
			"holdout_usage_ids": []any{}, "proposal": map[string]any{"kind": "none", "delivery": "none"},
		},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("search-only review status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	frontierT8AssertRouteContract(t, payload, skillEfficacyReviewContractID)
	review := anyMap(payload["review"])
	if anyToString(review["decision"]) != "abstain" || anyToBool(anyMap(review["discoverability"])["search_efficacy_credit"]) {
		t.Fatalf("search-only evidence affected efficacy: %#v", review)
	}
	if anyToInt(anyMap(anyMap(review["attribution"])["baseline"])["count"], -1) != 0 ||
		anyToBool(anyMap(anyMap(review["gates"])["repeated_verified_evidence"])["passed"]) {
		t.Fatalf("search-only evidence was counted as verified use: %#v", review)
	}
	if len(s.skillFoundry.efficacyReview(anyToString(review["review_id"]))) == 0 {
		t.Fatal("abstention review was not persisted")
	}
}

func TestSkillEfficacyProposalBoundsNoveltySourcePolicyAndRegression(t *testing.T) {
	_, _, root := frontierT8RouteTestServer(t)
	skill, _ := skillEfficacyTestSkill(t, root, "third_party")
	duplicate, duplicateGates, err := skillEfficacyProposal(map[string]any{
		"kind": "note", "summary": "Duplicate existing guidance.",
		"bounded_delta": "description: Bounded test skill.", "delivery": "local_overlay",
	}, skill)
	if err != nil || duplicate == nil || duplicateGates["novel"] {
		t.Fatalf("duplicate guidance passed novelty: proposal=%#v gates=%#v err=%v", duplicate, duplicateGates, err)
	}
	oversizedLines := make([]string, skillEfficacyNoteMaxLines+1)
	for index := range oversizedLines {
		oversizedLines[index] = fmt.Sprintf("Novel bounded instruction %d.", index)
	}
	_, oversizedGates, err := skillEfficacyProposal(map[string]any{
		"kind": "note", "summary": "Too many lines.",
		"bounded_delta": strings.Join(oversizedLines, "\n"), "delivery": "local_overlay",
	}, skill)
	if err != nil || oversizedGates["budget"] {
		t.Fatalf("oversized note passed budget: gates=%#v err=%v", oversizedGates, err)
	}
	_, sourceGates, err := skillEfficacyProposal(map[string]any{
		"kind": "revision", "summary": "Invalid third-party delivery.",
		"bounded_delta": "Add a genuinely novel, bounded verification instruction.",
		"delivery":      "foundry_revision",
	}, skill)
	if err != nil || sourceGates["source_policy"] {
		t.Fatalf("third-party Foundry mutation passed source policy: gates=%#v err=%v", sourceGates, err)
	}
	baseline := skillEfficacyGroupMetrics{Count: 3, FirstPass: 3, LatencyMS: 120, CostMicrousd: 300}
	regressed := skillEfficacyGroupMetrics{Count: 3, FirstPass: 3, LatencyMS: 150, CostMicrousd: 300}
	if skillEfficacyNoRegression(baseline, regressed) {
		t.Fatal("25 percent latency regression passed the 20 percent guardrail")
	}
}

func TestSkillEfficacyReviewRequiresMatchedLiftAndCurrentSourceThenStaysInactive(t *testing.T) {
	s, handler, root := frontierT8RouteTestServer(t)
	skill, skillPath := skillEfficacyTestSkill(t, root, "third_party")
	before, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatal(err)
	}
	baselineIDs := make([]any, 0, skillEfficacyMinBaselineUses)
	holdoutIDs := make([]any, 0, skillEfficacyMinHoldoutUses)
	for index := 0; index < skillEfficacyMinHoldoutUses; index++ {
		pairID := fmt.Sprintf("skill_pair_%d", index)
		taskDigest := utilityTestDigest("skill-task:" + pairID)
		controlOutcomeID := fmt.Sprintf("outcome_control_%d", index)
		controlPairing := map[string]any{
			"pair_id": pairID, "arm": "control", "task_match_digest": taskDigest,
			"matching_method": "exact_holdout", "leakage_free": true,
		}
		treatmentPairing := map[string]any{
			"pair_id": pairID, "arm": "treatment", "matched_control_outcome_id": controlOutcomeID,
			"task_match_digest": taskDigest, "matching_method": "exact_holdout", "leakage_free": true,
		}
		baselineUsageID := fmt.Sprintf("usage_control_%d", index)
		holdoutUsageID := fmt.Sprintf("usage_treatment_%d", index)
		skillEfficacyTestFullUsage(
			t, s, handler, baselineUsageID, fmt.Sprintf("session_control_%d", index),
			controlOutcomeID, skill, 4, controlPairing,
		)
		skillEfficacyTestFullUsage(
			t, s, handler, holdoutUsageID, fmt.Sprintf("session_treatment_%d", index),
			fmt.Sprintf("outcome_treatment_%d", index), skill, 6, treatmentPairing,
		)
		baselineIDs = append(baselineIDs, baselineUsageID)
		holdoutIDs = append(holdoutIDs, holdoutUsageID)
	}
	reviewInput := map[string]any{
		"project": "contextlattice", "skill_id": skill["id"], "name": skill["name"],
		"idempotency_key": "review-matched-note", "baseline_usage_ids": baselineIDs,
		"holdout_usage_ids": holdoutIDs,
		"proposal": map[string]any{
			"kind": "note", "summary": "Retain one concise verification cue.",
			"bounded_delta": "Before execution, verify one source-bound release fact.",
			"delivery":      "local_overlay",
		},
	}
	recorder, payload := frontierT8RouteRequest(t, handler, http.MethodPost, map[string]any{
		"operation": frontierT8OperationReviewEfficacy, "agent_id": "codex_test", "input": reviewInput,
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("matched efficacy review status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	frontierT8AssertRouteContract(t, payload, skillEfficacyReviewContractID)
	review := anyMap(payload["review"])
	if anyToString(review["decision"]) != "add_bounded_note" ||
		anyToInt(anyMap(review["attribution"])["exact_matched_pairs"], 0) != skillEfficacyMinHoldoutUses ||
		anyToFloat(anyMap(review["attribution"])["mean_utility_lift"]) != 2 {
		t.Fatalf("matched lift did not produce the bounded note decision: %#v", review)
	}
	if !anyToBool(anyMap(anyMap(review["gates"])["source_current"])["passed"]) ||
		!anyToBool(anyMap(anyMap(review["gates"])["no_material_regression"])["passed"]) {
		t.Fatalf("required source/regression gates did not pass: %#v", review["gates"])
	}
	artifact, governance, safety := anyMap(review["artifact"]), anyMap(review["governance"]), anyMap(review["safety"])
	operationSafety := anyMap(payload["safety"])
	if anyToString(review["status"]) != "inactive" || anyToString(artifact["state"]) != "inactive" ||
		anyToBool(artifact["filesystem_write_performed"]) || anyToBool(artifact["vendor_source_mutated"]) ||
		anyToBool(governance["promotion_allowed"]) || anyToBool(safety["activation_performed"]) {
		t.Fatalf("review crossed the inactive advisory boundary: %#v", review)
	}
	if anyToInt(operationSafety["filesystem_mutations"], 0) != 1 || anyToInt(operationSafety["ledger_writes"], 0) != 1 ||
		anyToInt(safety["filesystem_mutations"], 0) != 1 || anyToInt(safety["ledger_writes"], 0) != 1 {
		t.Fatalf("review did not report its exact persistence mutation: response=%#v review=%#v", operationSafety, safety)
	}
	after, err := os.ReadFile(skillPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("review mutated the active skill: err=%v", err)
	}
	status := s.skillFoundry.snapshot()
	recent := anyMap(contextPackAnyList(status["efficacy_reviews"])[0])
	if _, leaked := anyMap(recent["proposal"])["bounded_delta"]; leaked {
		t.Fatalf("Foundry status leaked review change material: %#v", recent)
	}

	staleBody := string(before) + "\nCurrent source changed after the recorded invocation.\n"
	if err := os.WriteFile(skillPath, []byte(staleBody), 0o600); err != nil {
		t.Fatalf("write stale-source fixture: %v", err)
	}
	staleInput := frontierT8TestCloneMap(t, reviewInput)
	staleInput["idempotency_key"] = "review-stale-source"
	staleRecorder, stalePayload := frontierT8RouteRequest(t, handler, http.MethodPost, map[string]any{
		"operation": frontierT8OperationReviewEfficacy, "input": staleInput,
	})
	if staleRecorder.Code != http.StatusOK {
		t.Fatalf("stale-source review status=%d body=%s", staleRecorder.Code, staleRecorder.Body.String())
	}
	staleReview := anyMap(stalePayload["review"])
	if anyToString(staleReview["decision"]) != "abstain" ||
		anyToBool(anyMap(anyMap(staleReview["gates"])["source_current"])["passed"]) {
		t.Fatalf("stale skill source did not force abstention: %#v", staleReview)
	}
}
