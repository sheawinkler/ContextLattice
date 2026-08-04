package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func recordAuthoritativeContextPackOutcomeForTest(t testing.TB, s *server, qualitySample, outcome map[string]any) {
	t.Helper()
	if s == nil || s.contextPackQuality == nil {
		t.Fatal("context-pack quality telemetry is unavailable")
	}
	s.contextPackQuality.recordQuality(qualitySample)
	if !contextPackQualityLedgerAvailable(s.contextPackQuality.ledger) {
		t.Fatal("test fixture requires an acknowledged context-pack quality ledger")
	}
	body, err := json.Marshal(outcome)
	if err != nil {
		t.Fatalf("marshal authoritative context-pack outcome: %v", err)
	}
	response := httptest.NewRecorder()
	s.telemetryContextPackQualityOutcomeRoute(response, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("record authoritative context-pack outcome: status=%d body=%s", response.Code, response.Body.String())
	}
}

func seedContextPolicyOutcomesForProject(t testing.TB, s *server, project string, count int) {
	t.Helper()
	for index := 0; index < count; index++ {
		sampleID := "cpq_policy_" + project + "_" + anyToString(index)
		recordAuthoritativeContextPackOutcomeForTest(t, s, map[string]any{
			"sample_id": sampleID, "project": project, "quality_score": 88,
			"model_call_token_basis": 3600 + index, "returned_source_count": 3,
			"graph_context_used": index%2 == 0, "tokenizer_exact": true,
		}, map[string]any{
			"outcome_id": "outcome_policy_" + project + "_" + anyToString(index), "sample_id": sampleID,
			"project":            project,
			"first_pass_success": true, "repair_required": false, "retry_count": 0,
			"followup_tokens": 80, "provider_total_tokens": 900,
			"outcome_source": "contract_test",
		})
	}
}

func seedContextPolicyOutcomes(t testing.TB, s *server, count int) {
	seedContextPolicyOutcomesForProject(t, s, "contextlattice", count)
}

func BenchmarkContextPolicyCandidate100Outcomes(b *testing.B) {
	b.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	b.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", filepath.Join(b.TempDir(), "context-pack-quality.ndjson"))
	s := &server{contextPackQuality: newContextPackQualityTelemetry(200)}
	seedContextPolicyOutcomes(b, s, 100)
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		_, _, _ = s.buildContextPolicyCandidate(map[string]any{"project": "contextlattice", "minimum_outcomes": 20})
	}
}

func BenchmarkContextPolicyCanaryGate(b *testing.B) {
	control := map[string]any{"sample_count": 50, "first_pass_success_rate": 0.82, "repair_rate": 0.14, "average_followup_tokens": 220, "average_provider_total_tokens": 1100}
	canary := map[string]any{"sample_count": 50, "first_pass_success_rate": 0.88, "repair_rate": 0.09, "average_followup_tokens": 180, "average_provider_total_tokens": 980}
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		_, _, _ = evaluateContextPolicyArms(control, canary, 20)
	}
}

func BenchmarkSkillFoundryDraft20Runs(b *testing.B) {
	b.Setenv("ORCH_SKILLS_INDEX_ROOTS", b.TempDir())
	s := &server{}
	payload := map[string]any{"project": "contextlattice", "name": "bounded-release-proof", "description": "Use for repeatable bounded release proof.", "workflow_runs": foundryRuns("bench-", 20)}
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		_, _ = s.buildSkillDraft(payload)
	}
}

func TestContextPolicyLifecycleIsOneStepAndAdvisory(t *testing.T) {
	t.Setenv("CONTEXTLATTICE_CONTEXT_POLICY_ENABLED", "true")
	t.Setenv("CONTEXTLATTICE_CONTEXT_POLICY_PATH", filepath.Join(t.TempDir(), "policy.ndjson"))
	t.Setenv("CONTEXTLATTICE_CONTEXT_POLICY_FSYNC", "false")
	t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_ENABLED", "false")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", filepath.Join(t.TempDir(), "context-pack-quality.ndjson"))
	s := newTestServer(t, "http://127.0.0.1:1")
	seedContextPolicyOutcomes(t, s, 24)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	candidatePayload := postJSONForTest(t, gateway.URL+"/memory/context-policy/candidate", `{"project":"contextlattice"}`)
	assertBoundaryContractPassed(t, contextPolicyCandidateContractID, candidatePayload)
	candidate := anyMap(candidatePayload["candidate"])
	if !anyToBool(candidatePayload["recorded"]) || anyToString(candidate["status"]) != "candidate" {
		t.Fatalf("expected recorded candidate, got %#v", candidatePayload)
	}
	if anyToBool(anyMap(candidate["activation"])["allowed"]) {
		t.Fatalf("public candidate must not permit runtime activation: %#v", candidate)
	}
	id := anyToString(candidate["candidate_id"])

	shadow := postJSONForTest(t, gateway.URL+"/memory/context-policy/evaluate", `{"candidate_id":"`+id+`","apply_transition":true}`)
	assertBoundaryContractPassed(t, contextPolicyEvaluationContractID, shadow)
	if anyToString(anyMap(shadow["candidate"])["status"]) != "shadow" || anyToString(anyMap(shadow["evaluation"])["previous_phase"]) != "candidate" {
		t.Fatalf("expected candidate to advance exactly one step to shadow: %#v", shadow)
	}

	armPayload := `{"candidate_id":"` + id + `","apply_transition":true,"minimum_arm_samples":10,"control":{"sample_count":20,"first_pass_success_rate":0.80,"repair_rate":0.15,"average_followup_tokens":200,"average_provider_total_tokens":1000},"canary":{"sample_count":20,"first_pass_success_rate":0.86,"repair_rate":0.10,"average_followup_tokens":170,"average_provider_total_tokens":920}}`
	canary := postJSONForTest(t, gateway.URL+"/tools/context_policy_evaluate", armPayload)
	assertBoundaryContractPassed(t, contextPolicyEvaluationContractID, canary)
	if anyToString(anyMap(canary["candidate"])["status"]) != "canary" {
		t.Fatalf("expected shadow to advance to canary: %#v", canary)
	}
	promoted := postJSONForTest(t, gateway.URL+"/memory/context-policy/evaluate", strings.Replace(armPayload, `"minimum_arm_samples":10`, `"minimum_arm_samples":20`, 1))
	assertBoundaryContractPassed(t, contextPolicyEvaluationContractID, promoted)
	evaluation := anyMap(promoted["evaluation"])
	if anyToString(anyMap(promoted["candidate"])["status"]) != "promoted" || anyToBool(evaluation["runtime_activation"]) {
		t.Fatalf("expected advisory promotion without runtime activation: %#v", promoted)
	}
	if !anyToBool(evaluation["one_step_only"]) || !contextPolicyGuardrailsPass(contextPackAnyList(evaluation["guardrails"])) {
		t.Fatalf("expected passing one-step canary gate: %#v", evaluation)
	}
}

func TestContextPolicyHardRegressionRollsBack(t *testing.T) {
	guards, beneficial, regression := evaluateContextPolicyArms(
		map[string]any{"sample_count": 20, "first_pass_success_rate": 0.9, "repair_rate": 0.05, "average_followup_tokens": 100, "average_provider_total_tokens": 800},
		map[string]any{"sample_count": 20, "first_pass_success_rate": 0.7, "repair_rate": 0.3, "average_followup_tokens": 300, "average_provider_total_tokens": 1200},
		20,
	)
	if beneficial || !regression || contextPolicyGuardrailsPass(guards) {
		t.Fatalf("expected hard regression with failed guardrails, guards=%#v", guards)
	}
}

func TestContextPolicyTerminalPhasesRejectFurtherEvaluation(t *testing.T) {
	for _, phase := range []string{"promoted", "rolled_back"} {
		t.Run(phase, func(t *testing.T) {
			id := "ctxpol_terminal_" + phase
			s := &server{contextPolicy: &contextPolicyStore{candidates: map[string]map[string]any{
				id: {"candidate_id": id, "project": "contextlattice", "status": phase},
			}}}
			_, _, err := s.contextPolicyEvaluation(map[string]any{"candidate_id": id})
			if err == nil || !strings.Contains(err.Error(), "terminal") {
				t.Fatalf("expected terminal lifecycle rejection, got %v", err)
			}
		})
	}
}

func TestContextPolicyTrainingIsProjectScoped(t *testing.T) {
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", filepath.Join(t.TempDir(), "context-pack-quality.ndjson"))
	s := &server{contextPackQuality: newContextPackQualityTelemetry(100), contextPolicy: &contextPolicyStore{candidates: map[string]map[string]any{}}}
	for _, project := range []string{"alpha", "beta"} {
		seedContextPolicyOutcomesForProject(t, s, project, 12)
	}
	s.contextPackQuality.recordOutcome(map[string]any{"outcome_id": "legacy-unlinked", "sample_id": "missing", "first_pass_success": true, "calibration_eligible": true})
	candidate, recorded, err := s.buildContextPolicyCandidate(map[string]any{"project": "alpha", "minimum_outcomes": 10})
	if err != nil || !recorded {
		t.Fatalf("build alpha candidate: recorded=%v err=%v", recorded, err)
	}
	if anyToInt(candidate["eligible_outcomes"], 0) != 12 {
		t.Fatalf("expected only alpha outcomes, got %#v", candidate["eligible_outcomes"])
	}
}

func TestContextPolicyPersistedEvidenceIsPhaseScopedAndCannotBeMixed(t *testing.T) {
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", filepath.Join(t.TempDir(), "context-pack-quality.ndjson"))
	id := "ctxpol_phase_scope"
	s := &server{
		contextPackQuality: newContextPackQualityTelemetry(200),
		contextPolicy: &contextPolicyStore{
			enabled: true, maxEntries: 100, candidates: map[string]map[string]any{
				id: {"candidate_id": id, "project": "contextlattice", "status": "canary"},
			},
		},
	}
	for index := 0; index < 24; index++ {
		arm := "control"
		if index%2 == 1 {
			arm = "shadow"
		}
		sampleID := "phase-sample-" + anyToString(index)
		recordAuthoritativeContextPackOutcomeForTest(t, s, map[string]any{
			"sample_id": sampleID, "project": "contextlattice", "quality_score": 90,
		}, map[string]any{
			"outcome_id": "phase-" + anyToString(index), "sample_id": "phase-sample-" + anyToString(index),
			"project": "contextlattice", "policy_id": id, "policy_arm": arm, "policy_phase": "shadow",
			"first_pass_success": true, "repair_required": false,
		})
	}
	evaluation, _, err := s.contextPolicyEvaluation(map[string]any{"candidate_id": id, "minimum_arm_samples": 10})
	if err != nil {
		t.Fatalf("evaluate phase-scoped evidence: %v", err)
	}
	if anyToString(evaluation["evidence_source"]) != "persisted_outcomes" || anyToInt(anyMap(evaluation["control"])["sample_count"], -1) != 0 || anyToInt(anyMap(evaluation["canary"])["sample_count"], -1) != 0 {
		t.Fatalf("shadow evidence must not satisfy a canary gate: %#v", evaluation)
	}
	_, _, err = s.contextPolicyEvaluation(map[string]any{
		"candidate_id": id,
		"control":      map[string]any{"sample_count": 20},
	})
	if err == nil || !strings.Contains(err.Error(), "both be supplied") {
		t.Fatalf("expected mixed evidence rejection, got %v", err)
	}
}

func TestContextPolicyRejectsInvalidOperatorMetrics(t *testing.T) {
	id := "ctxpol_invalid_metrics"
	s := &server{contextPolicy: &contextPolicyStore{candidates: map[string]map[string]any{
		id: {"candidate_id": id, "project": "contextlattice", "status": "shadow"},
	}}}
	valid := map[string]any{"sample_count": 20, "first_pass_success_rate": 0.8, "repair_rate": 0.1, "average_followup_tokens": 100}
	invalid := cloneMap(valid)
	invalid["first_pass_success_rate"] = 1.5
	_, _, err := s.contextPolicyEvaluation(map[string]any{"candidate_id": id, "control": valid, "canary": invalid})
	if err == nil || !strings.Contains(err.Error(), "between 0 and 1") {
		t.Fatalf("expected invalid metric rejection, got %v", err)
	}
}

func TestContextPolicyLifecycleCannotRegressOrApplyAStaleTransition(t *testing.T) {
	t.Setenv("CONTEXTLATTICE_CONTEXT_POLICY_ENABLED", "true")
	t.Setenv("CONTEXTLATTICE_CONTEXT_POLICY_PATH", filepath.Join(t.TempDir(), "policy.ndjson"))
	t.Setenv("CONTEXTLATTICE_CONTEXT_POLICY_FSYNC", "false")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", filepath.Join(t.TempDir(), "context-pack-quality.ndjson"))
	s := &server{contextPackQuality: newContextPackQualityTelemetry(100)}
	store, err := newContextPolicyStoreFromEnv()
	if err != nil {
		t.Fatalf("new policy store: %v", err)
	}
	s.contextPolicy = store
	seedContextPolicyOutcomes(t, s, 24)
	candidate, recorded, err := s.buildContextPolicyCandidate(map[string]any{"project": "contextlattice"})
	if err != nil || !recorded {
		t.Fatalf("build candidate: recorded=%v err=%v", recorded, err)
	}
	if err := s.contextPolicy.recordCandidate(candidate); err != nil {
		t.Fatalf("record candidate: %v", err)
	}
	id := anyToString(candidate["candidate_id"])
	evaluation, advanced, err := s.contextPolicyEvaluation(map[string]any{"candidate_id": id, "apply_transition": true})
	if err != nil {
		t.Fatalf("evaluate candidate: %v", err)
	}
	if err := s.contextPolicy.recordEvaluation(evaluation, advanced); err != nil {
		t.Fatalf("record shadow transition: %v", err)
	}
	rebuilt, _, err := s.buildContextPolicyCandidate(map[string]any{"project": "contextlattice"})
	if err != nil {
		t.Fatalf("rebuild candidate: %v", err)
	}
	if anyToString(rebuilt["status"]) != "shadow" {
		t.Fatalf("candidate regeneration regressed lifecycle: %#v", rebuilt)
	}
	if err := s.contextPolicy.recordCandidate(rebuilt); err != nil {
		t.Fatalf("record rebuilt candidate: %v", err)
	}
	stale := cloneMap(rebuilt)
	stale["status"] = "canary"
	staleEvaluation := map[string]any{
		"schema_id": contextPolicyEvaluationContractID, "candidate_id": id, "previous_phase": "candidate",
		"transition_applied": true,
	}
	if err := s.contextPolicy.recordEvaluation(staleEvaluation, stale); !errors.Is(err, errContextPolicyTransitionConflict) {
		t.Fatalf("expected stale transition conflict, got %v", err)
	}
	if anyToString(s.contextPolicy.candidate(id)["status"]) != "shadow" {
		t.Fatalf("stale transition changed candidate state: %#v", s.contextPolicy.candidate(id))
	}
}

func foundryRuns(prefix string, count int) []map[string]any {
	runs := make([]map[string]any, 0, count)
	for index := 0; index < count; index++ {
		runs = append(runs, map[string]any{
			"run_id": prefix + anyToString(index), "verified": true, "success": true, "checks_passed": true,
			"steps":         []any{"Inspect scoped state", "Apply one bounded change", "Run deterministic verification"},
			"checks":        []any{"Tests pass", "Diff is bounded"},
			"evidence_refs": []any{"check:" + prefix + anyToString(index)},
		})
	}
	return runs
}

func TestSkillFoundryRequiresIndependentHoldoutsAndHumanApproval(t *testing.T) {
	root := t.TempDir()
	ledger := filepath.Join(t.TempDir(), "foundry.ndjson")
	t.Setenv("CONTEXTLATTICE_CONTEXT_POLICY_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_ENABLED", "true")
	t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_PATH", ledger)
	t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_FSYNC", "false")
	t.Setenv("ORCH_SKILLS_INDEX_ROOTS", root)
	s := newTestServer(t, "http://127.0.0.1:1")
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	draftRequest := map[string]any{
		"project": "contextlattice", "name": "bounded-release-proof", "description": "Use for repeatable bounded release proof.",
		"workflow_runs": foundryRuns("train-", 3),
	}
	raw, _ := json.Marshal(draftRequest)
	draftPayload := postJSONForTest(t, gateway.URL+"/tools/skill_foundry_draft", string(raw))
	assertBoundaryContractPassed(t, skillDraftContractID, draftPayload)
	draft := anyMap(draftPayload["draft"])
	if anyToString(draft["status"]) != "draft" || anyToInt(draft["verified_run_count"], 0) != 3 {
		t.Fatalf("unexpected foundry draft: %#v", draft)
	}
	if anyToBool(anyMap(draft["activation"])["automatic"]) {
		t.Fatalf("draft must remain inactive: %#v", draft)
	}
	draftID := anyToString(draft["draft_id"])
	holdouts := foundryRuns("holdout-", 3)
	evalRaw, _ := json.Marshal(map[string]any{"draft_id": draftID, "holdouts": holdouts})
	evalPayload := postJSONForTest(t, gateway.URL+"/memory/skills/foundry/evaluate", string(evalRaw))
	assertBoundaryContractPassed(t, skillEvaluationContractID, evalPayload)
	if !anyToBool(anyMap(evalPayload["evaluation"])["passed"]) {
		t.Fatalf("expected independent holdouts to pass: %#v", evalPayload)
	}

	deniedBody, _ := json.Marshal(map[string]any{"draft_id": draftID, "human_approved": false, "approver": "release-owner"})
	deniedResp, err := http.Post(gateway.URL+"/memory/skills/foundry/export", "application/json", strings.NewReader(string(deniedBody)))
	if err != nil {
		t.Fatalf("export denial request: %v", err)
	}
	defer deniedResp.Body.Close()
	if deniedResp.StatusCode != http.StatusUnprocessableEntity {
		body, _ := io.ReadAll(deniedResp.Body)
		t.Fatalf("expected approval gate 422, got %d body=%s", deniedResp.StatusCode, string(body))
	}

	exportRaw, _ := json.Marshal(map[string]any{"draft_id": draftID, "human_approved": true, "approver": "release-owner"})
	exportPayload := postJSONForTest(t, gateway.URL+"/tools/skill_foundry_export", string(exportRaw))
	assertBoundaryContractPassed(t, skillExportContractID, exportPayload)
	exported := anyMap(exportPayload["export"])
	if anyToString(exported["activation_state"]) != "inactive" || anyToBool(exported["automatic_activation"]) {
		t.Fatalf("export must not activate itself: %#v", exported)
	}
	if !strings.Contains(anyToString(exported["skill_markdown"]), "## Verification") {
		t.Fatalf("expected bounded skill artifact: %#v", exported)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("public foundry must not write into active skill roots: %#v", entries)
	}
	reloaded, err := newSkillFoundryStoreFromEnv()
	if err != nil {
		t.Fatalf("reload foundry: %v", err)
	}
	if anyToString(reloaded.draft(draftID)["status"]) != "exported" {
		t.Fatalf("expected exported draft to survive reload: %#v", reloaded.draft(draftID))
	}
}

func TestSkillFoundryRetirementIsTerminalIdempotentAndDurable(t *testing.T) {
	ledger := filepath.Join(t.TempDir(), "foundry.ndjson")
	t.Setenv("CONTEXTLATTICE_CONTEXT_POLICY_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_ENABLED", "true")
	t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_PATH", ledger)
	t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_FSYNC", "false")
	t.Setenv("ORCH_SKILLS_INDEX_ROOTS", t.TempDir())
	s := newTestServer(t, "http://127.0.0.1:1")
	draftInput := map[string]any{
		"project": "contextlattice", "name": "retire-smoke-proof", "description": "Temporary Foundry smoke proof.",
		"workflow_runs": foundryRuns("retire-train-", 3),
	}
	draft, err := s.buildSkillDraft(draftInput)
	if err != nil {
		t.Fatalf("build draft: %v", err)
	}
	if err := s.skillFoundry.record(draft); err != nil {
		t.Fatalf("record draft: %v", err)
	}
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()
	body := `{"draft_id":"` + anyToString(draft["draft_id"]) + `","operator":"release-owner","reason":"smoke artifact completed its purpose"}`
	first := postJSONForTest(t, gateway.URL+"/memory/skills/foundry/retire", body)
	assertBoundaryContractPassed(t, skillRetirementContractID, first)
	retirement := anyMap(first["retirement"])
	if anyToString(retirement["status"]) != "retired" || anyToBool(retirement["deletion_performed"]) || anyToBool(retirement["runtime_mutation"]) {
		t.Fatalf("retirement must be terminal and non-destructive: %#v", first)
	}
	if anyToString(anyMap(first["draft"])["status"]) != "retired" || !anyToBool(first["recorded"]) {
		t.Fatalf("expected first retirement to persist: %#v", first)
	}
	second := postJSONForTest(t, gateway.URL+"/memory/skills/foundry/retire", body)
	if !anyToBool(second["idempotent"]) || anyToBool(second["recorded"]) || anyToString(anyMap(second["retirement"])["retirement_id"]) != anyToString(retirement["retirement_id"]) {
		t.Fatalf("repeat retirement must be idempotent: %#v", second)
	}
	if _, _, err := s.evaluateSkillDraft(map[string]any{"draft_id": draft["draft_id"], "holdouts": foundryRuns("retire-holdout-", 3)}); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("retired draft must reject evaluation, got %v", err)
	}
	replayed, err := s.buildSkillDraft(draftInput)
	if err != nil {
		t.Fatalf("rebuild retired draft: %v", err)
	}
	if anyToString(replayed["status"]) != "retired" || anyToString(anyMap(replayed["retirement"])["retirement_id"]) != anyToString(retirement["retirement_id"]) {
		t.Fatalf("identical draft replay must preserve terminal retirement evidence: %#v", replayed)
	}
	reloaded, err := newSkillFoundryStoreFromEnv()
	if err != nil {
		t.Fatalf("reload foundry: %v", err)
	}
	if anyToString(reloaded.draft(anyToString(draft["draft_id"]))["status"]) != "retired" || len(reloaded.latestRetirement(anyToString(draft["draft_id"]))) == 0 {
		t.Fatalf("retirement must survive reload: %#v", reloaded.snapshot())
	}
}

func TestSkillFoundryRetirementRecoversPartialLedgerAppend(t *testing.T) {
	ledger := filepath.Join(t.TempDir(), "foundry.ndjson")
	t.Setenv("CONTEXTLATTICE_CONTEXT_POLICY_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_ENABLED", "true")
	t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_PATH", ledger)
	t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_FSYNC", "false")
	t.Setenv("ORCH_SKILLS_INDEX_ROOTS", t.TempDir())
	s := newTestServer(t, "http://127.0.0.1:1")
	draft, err := s.buildSkillDraft(map[string]any{
		"project": "contextlattice", "name": "partial-retirement-proof", "description": "Retirement recovery proof.",
		"workflow_runs": foundryRuns("partial-retire-", 3),
	})
	if err != nil {
		t.Fatalf("build draft: %v", err)
	}
	if err := s.skillFoundry.record(draft); err != nil {
		t.Fatalf("record draft: %v", err)
	}
	retirement, _, created, err := s.retireSkillDraft(map[string]any{
		"draft_id": draft["draft_id"], "operator": "release-owner", "reason": "simulate interrupted append",
	})
	if err != nil || !created {
		t.Fatalf("build retirement: created=%v err=%v", created, err)
	}
	draftLine, _ := json.Marshal(draft)
	retirementLine, _ := json.Marshal(retirement)
	partial := append(append(append([]byte{}, draftLine...), '\n'), retirementLine...)
	partial = append(partial, '\n')
	if err := os.WriteFile(ledger, partial, 0o600); err != nil {
		t.Fatalf("write partial ledger: %v", err)
	}
	reloaded, err := newSkillFoundryStoreFromEnv()
	if err != nil {
		t.Fatalf("reload partial ledger: %v", err)
	}
	recovered := reloaded.draft(anyToString(draft["draft_id"]))
	if anyToString(recovered["status"]) != "retired" || anyToString(anyMap(recovered["retirement"])["retirement_id"]) != anyToString(retirement["retirement_id"]) {
		t.Fatalf("retirement tombstone must dominate a stale draft snapshot: %#v", recovered)
	}
}

func TestSkillFoundryRetirementRejectsActiveSkill(t *testing.T) {
	t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_ENABLED", "true")
	t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_PATH", filepath.Join(t.TempDir(), "foundry.ndjson"))
	t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_FSYNC", "false")
	t.Setenv("ORCH_SKILLS_INDEX_ROOTS", t.TempDir())
	s := newTestServer(t, "http://127.0.0.1:1")
	draft, err := s.buildSkillDraft(map[string]any{
		"project": "contextlattice", "name": "active-retirement-guard", "description": "Active retirement guard.",
		"workflow_runs": foundryRuns("active-retire-", 3),
	})
	if err != nil {
		t.Fatalf("build draft: %v", err)
	}
	if err := s.skillFoundry.record(draft); err != nil {
		t.Fatalf("record draft: %v", err)
	}
	s.skillFoundry.mu.Lock()
	s.skillFoundry.drafts[anyToString(draft["draft_id"])]["activation"] = map[string]any{"state": "active", "automatic": false}
	s.skillFoundry.mu.Unlock()
	_, _, _, err = s.retireSkillDraft(map[string]any{"draft_id": draft["draft_id"], "operator": "owner", "reason": "should block"})
	if err == nil || !strings.Contains(err.Error(), "deactivated") {
		t.Fatalf("expected active-skill retirement rejection, got %v", err)
	}
}

func TestSkillFoundryRejectsTrainingHoldoutLeakage(t *testing.T) {
	t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_ENABLED", "true")
	t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_PATH", filepath.Join(t.TempDir(), "foundry.ndjson"))
	t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_FSYNC", "false")
	t.Setenv("ORCH_SKILLS_INDEX_ROOTS", t.TempDir())
	s := newTestServer(t, "http://127.0.0.1:1")
	draft, err := s.buildSkillDraft(map[string]any{"project": "contextlattice", "name": "leakage-guard", "description": "Use to prove holdout separation.", "workflow_runs": foundryRuns("same-", 3)})
	if err != nil {
		t.Fatalf("build draft: %v", err)
	}
	if err := s.skillFoundry.record(draft); err != nil {
		t.Fatalf("record draft: %v", err)
	}
	_, _, err = s.evaluateSkillDraft(map[string]any{"draft_id": draft["draft_id"], "holdouts": foundryRuns("same-", 3)})
	if err == nil || !strings.Contains(err.Error(), "overlaps training") {
		t.Fatalf("expected training-holdout leakage rejection, got %v", err)
	}
}

func TestSkillFoundryRejectsUnprovenRuns(t *testing.T) {
	t.Setenv("ORCH_SKILLS_INDEX_ROOTS", t.TempDir())
	s := &server{}
	runs := foundryRuns("unproven-", 3)
	for _, run := range runs {
		delete(run, "evidence_refs")
	}
	_, err := s.buildSkillDraft(map[string]any{
		"project": "contextlattice", "name": "unproven-workflow", "description": "Use for a workflow that lacks proof.", "workflow_runs": runs,
	})
	if err == nil || !strings.Contains(err.Error(), "verified successful runs") {
		t.Fatalf("expected proofless runs to be rejected, got %v", err)
	}
}

func TestSkillFoundryDraftIdentityBindsMaterialContent(t *testing.T) {
	t.Setenv("ORCH_SKILLS_INDEX_ROOTS", t.TempDir())
	s := &server{}
	base := map[string]any{
		"project": "contextlattice", "name": "material-identity", "description": "Use for material identity proof.",
		"skill_version": 1, "workflow_runs": foundryRuns("identity-", 3),
	}
	first, err := s.buildSkillDraft(base)
	if err != nil {
		t.Fatalf("build first draft: %v", err)
	}
	changed := cloneMap(base)
	changed["skill_version"] = 2
	second, err := s.buildSkillDraft(changed)
	if err != nil {
		t.Fatalf("build changed draft: %v", err)
	}
	if anyToString(first["draft_id"]) == anyToString(second["draft_id"]) || anyToString(first["draft_fingerprint"]) == anyToString(second["draft_fingerprint"]) {
		t.Fatalf("materially different drafts must not share evaluation identity: first=%#v second=%#v", first, second)
	}
}

func TestSkillFoundryRejectsProoflessHoldouts(t *testing.T) {
	t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_ENABLED", "true")
	t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_PATH", filepath.Join(t.TempDir(), "foundry.ndjson"))
	t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_FSYNC", "false")
	t.Setenv("ORCH_SKILLS_INDEX_ROOTS", t.TempDir())
	s := newTestServer(t, "http://127.0.0.1:1")
	draft, err := s.buildSkillDraft(map[string]any{
		"project": "contextlattice", "name": "holdout-proof", "description": "Use for holdout proof.", "workflow_runs": foundryRuns("proof-train-", 3),
	})
	if err != nil {
		t.Fatalf("build draft: %v", err)
	}
	if err := s.skillFoundry.record(draft); err != nil {
		t.Fatalf("record draft: %v", err)
	}
	holdouts := foundryRuns("proof-holdout-", 3)
	delete(holdouts[1], "evidence_refs")
	_, _, err = s.evaluateSkillDraft(map[string]any{"draft_id": draft["draft_id"], "holdouts": holdouts})
	if err == nil || !strings.Contains(err.Error(), "evidence_refs") {
		t.Fatalf("expected proofless holdout rejection, got %v", err)
	}
}

func TestSkillFoundryReplayCannotRegressExportedState(t *testing.T) {
	t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_ENABLED", "true")
	t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_PATH", filepath.Join(t.TempDir(), "foundry.ndjson"))
	t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_FSYNC", "false")
	t.Setenv("ORCH_SKILLS_INDEX_ROOTS", t.TempDir())
	s := newTestServer(t, "http://127.0.0.1:1")
	payload := map[string]any{
		"project": "contextlattice", "name": "monotonic-skill", "description": "Use for monotonic state proof.", "workflow_runs": foundryRuns("monotonic-train-", 3),
	}
	draft, err := s.buildSkillDraft(payload)
	if err != nil {
		t.Fatalf("build draft: %v", err)
	}
	if err := s.skillFoundry.record(draft); err != nil {
		t.Fatalf("record draft: %v", err)
	}
	evaluation, evaluated, err := s.evaluateSkillDraft(map[string]any{"draft_id": draft["draft_id"], "holdouts": foundryRuns("monotonic-holdout-", 3)})
	if err != nil {
		t.Fatalf("evaluate draft: %v", err)
	}
	if err := s.skillFoundry.record(evaluation, evaluated); err != nil {
		t.Fatalf("record evaluation: %v", err)
	}
	exported, exportedDraft, err := s.exportSkillDraft(map[string]any{"draft_id": draft["draft_id"], "human_approved": true, "approver": "owner"})
	if err != nil {
		t.Fatalf("export draft: %v", err)
	}
	if err := s.skillFoundry.record(exported, exportedDraft); err != nil {
		t.Fatalf("record export: %v", err)
	}
	replayed, err := s.buildSkillDraft(payload)
	if err != nil {
		t.Fatalf("rebuild identical draft: %v", err)
	}
	if anyToString(replayed["status"]) != "exported" {
		t.Fatalf("identical draft replay regressed lifecycle: %#v", replayed)
	}
	if err := s.skillFoundry.record(replayed); err != nil {
		t.Fatalf("record replayed draft: %v", err)
	}
	if anyToString(s.skillFoundry.draft(anyToString(draft["draft_id"]))["status"]) != "exported" {
		t.Fatalf("stored draft regressed after replay: %#v", s.skillFoundry.draft(anyToString(draft["draft_id"])))
	}
}

func TestSkillFoundryCollisionRequiresSupersession(t *testing.T) {
	root := t.TempDir()
	writeSkillIndexFixture(t, root, "bounded-release-proof", "bounded-release-proof", "Existing skill.")
	t.Setenv("ORCH_SKILLS_INDEX_ROOTS", root)
	t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_ENABLED", "true")
	t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_PATH", filepath.Join(t.TempDir(), "foundry.ndjson"))
	t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_FSYNC", "false")
	s := newTestServer(t, "http://127.0.0.1:1")
	draft, err := s.buildSkillDraft(map[string]any{
		"project": "contextlattice", "name": "bounded-release-proof", "description": "Use for repeatable bounded release proof.", "workflow_runs": foundryRuns("collision-train-", 3),
	})
	if err != nil {
		t.Fatalf("build colliding draft: %v", err)
	}
	if !anyToBool(anyMap(draft["existing_skill_collision"])["detected"]) {
		t.Fatalf("expected existing skill collision: %#v", draft)
	}
	if err := s.skillFoundry.record(draft); err != nil {
		t.Fatalf("record draft: %v", err)
	}
	evaluation, updated, err := s.evaluateSkillDraft(map[string]any{"draft_id": draft["draft_id"], "holdouts": foundryRuns("collision-holdout-", 3)})
	if err != nil {
		t.Fatalf("evaluate draft: %v", err)
	}
	if err := s.skillFoundry.record(evaluation, updated); err != nil {
		t.Fatalf("record evaluation: %v", err)
	}
	_, _, err = s.exportSkillDraft(map[string]any{"draft_id": draft["draft_id"], "human_approved": true, "approver": "owner"})
	if err == nil || !strings.Contains(err.Error(), "supersedes must name") {
		t.Fatalf("expected collision export gate, got %v", err)
	}
}
