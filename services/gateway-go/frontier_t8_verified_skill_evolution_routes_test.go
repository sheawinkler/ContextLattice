package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func frontierT8RouteTestServer(t *testing.T) (*server, http.Handler, string) {
	t.Helper()
	root := t.TempDir()
	skillsRoot := filepath.Join(root, "skills_active")
	for _, name := range []string{"verified-release-gate", "release-gate", "skill-release-gate-v4"} {
		dir := filepath.Join(skillsRoot, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		body := "---\nname: " + name + "\ndescription: Bounded test skill.\n---\n"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ORCH_SKILLS_INDEX_ROOTS", skillsRoot)
	t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_ENABLED", "true")
	t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_PATH", filepath.Join(root, "foundry.ndjson"))
	t.Setenv("GO_UTILITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_UTILITY_LEDGER_PATH", filepath.Join(root, "utility.ndjson"))
	s := newTestServer(t, "")
	return s, buildNativeMux(s), root
}

func frontierT8RouteCurrentPayload(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	result := frontierT8TestCloneMap(t, payload)
	now := time.Now().UTC().Truncate(time.Second)
	result["as_of"] = now.Format(time.RFC3339Nano)
	for _, key := range []string{"training_receipts", "holdout_receipts"} {
		rows := frontierT8TestReceiptRows(result, key)
		for _, row := range rows {
			row["verified_at"] = now.Add(-12 * time.Hour).Format(time.RFC3339Nano)
		}
		frontierT8TestRechain(rows)
	}
	if _, retirement := result["review_window"]; retirement {
		result["last_verified_at"] = now.Add(-120 * 24 * time.Hour).Format(time.RFC3339Nano)
		result["review_window"] = map[string]any{
			"start_at": now.Add(-30 * 24 * time.Hour).Format(time.RFC3339Nano),
			"end_at":   now.Add(24 * time.Hour).Format(time.RFC3339Nano),
		}
	}
	return result
}

func frontierT8SeedEvidenceAuthority(t *testing.T, s *server, input map[string]any) {
	t.Helper()
	claims := []frontierT8EvidenceClaim{}
	if err := frontierT8CollectEvidenceClaims(input, "input", time.Time{}, &claims); err != nil {
		t.Fatalf("collect T8 evidence claims: %v", err)
	}
	project := anyToString(input["project"])
	now := time.Now().UTC().Truncate(time.Second)
	for _, claim := range claims {
		ref := claim.Ref
		outcomeID := anyToString(ref["ref_id"])
		sampleID := "sample_" + sha256Hex(outcomeID)[:16]
		sessionID := "sess_t8_" + sha256Hex(outcomeID)[:16]
		producerID := anyToString(ref["producer_id"])
		verifierID := anyToString(ref["verifier_id"])
		verifierKind := anyToString(ref["kind"])
		digest := anyToString(ref["digest"])
		eventID := anyToString(ref["verification_id"])
		verifiedAt := claim.ExpectedVerifiedAt
		if verifiedAt.IsZero() {
			verifiedAt = now.Add(-12 * time.Hour)
		}
		event := map[string]any{
			"id": eventID, "session_id": sessionID, "type": "verification.completed",
			"agent_id": verifierID, "project": project, "created_at": verifiedAt.Format(time.RFC3339Nano),
			"metadata": map[string]any{"utility_verification": map[string]any{
				"outcome_id": outcomeID, "sample_id": sampleID, "utility_value": 1.0,
				"utility_unit": "verified_workflow", "evidence_digest": digest,
				"verification_passed": true, "verifier_kind": verifierKind, "verifier_id": verifierID,
			}},
		}
		quality := map[string]any{
			"sample_id": sampleID, "session_id": sessionID, "project": project,
			"agent_id": producerID, "quality_score": 100, "confidence": "high",
		}
		impact := map[string]any{
			"sample_id": sampleID, "session_id": sessionID, "project": project,
			"agent_id": producerID, "tokenizer_exact": true, "wire_tokens_exact": 128,
			"model_visible_context_tokens_exact": 64, "tokenizer_encoding": "test-exact",
		}
		outcome := map[string]any{
			"outcome_id": outcomeID, "sample_id": sampleID, "session_id": sessionID,
			"project": project, "agent_id": producerID, "task_class": "skill-evolution",
			"captured_at": verifiedAt.Format(time.RFC3339Nano),
			"utility": map[string]any{
				"value": 1.0, "unit": "verified_workflow", "verification_event_id": eventID,
				"evidence_digest": digest, "verification_passed": true,
				"verifier_kind": verifierKind, "verifier_id": verifierID,
			},
			"economics": map[string]any{"latency_ms": 1, "cost_microusd": 0, "tool_calls": 1, "failures": 0},
		}
		observation := buildUtilityObservation(outcome, quality, impact, []map[string]any{event})
		if _, _, err := s.utility.record(observation); err != nil {
			t.Fatalf("record T8 utility evidence %s: %v", outcomeID, err)
		}
		s.agentSessions.mu.Lock()
		if _, exists := s.agentSessions.sessions[sessionID]; !exists {
			s.agentSessions.sessions[sessionID] = map[string]any{
				"id": sessionID, "project": project, "agent_id": producerID,
				"status": "done", "created_at": verifiedAt.Format(time.RFC3339Nano),
				"updated_at": verifiedAt.Format(time.RFC3339Nano),
			}
			s.agentSessions.order = append(s.agentSessions.order, sessionID)
		}
		s.agentSessions.events[sessionID] = []map[string]any{event}
		if err := s.agentSessions.persistLocked(); err != nil {
			s.agentSessions.mu.Unlock()
			t.Fatalf("persist T8 verification session %s: %v", sessionID, err)
		}
		s.agentSessions.mu.Unlock()
	}
}

func frontierT8RouteRequest(t *testing.T, handler http.Handler, method string, payload any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var body bytes.Buffer
	if payload != nil {
		if err := json.NewEncoder(&body).Encode(payload); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, frontierT8SkillEvolutionPath, &body)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	decoded := map[string]any{}
	if recorder.Body.Len() > 0 {
		if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("decode response status=%d body=%s: %v", recorder.Code, recorder.Body.String(), err)
		}
	}
	return recorder, decoded
}

func frontierT8AssertRouteContract(t *testing.T, payload map[string]any, contractID string) {
	t.Helper()
	if anyToString(payload["schema_id"]) != contractID {
		t.Fatalf("schema_id=%q want %q", anyToString(payload["schema_id"]), contractID)
	}
	validation := anyMap(anyMap(payload["format_contract"])["validation"])
	if anyToString(validation["status"]) != "passed" {
		t.Fatalf("format contract failed: %#v", payload["format_contract"])
	}
}

func TestFrontierT8EvolutionRouteDerivesWithoutPersistenceAndValidatesTopLevelContract(t *testing.T) {
	s, handler, root := frontierT8RouteTestServer(t)
	input := frontierT8RouteCurrentPayload(t, frontierT8TestSkillCandidatePayload())
	frontierT8SeedEvidenceAuthority(t, s, input)
	recorder, payload := frontierT8RouteRequest(t, handler, http.MethodPost, map[string]any{
		"operation": frontierT8OperationDeriveReusable,
		"agent_id":  "codex_gpt5",
		"input":     input,
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("derive status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	frontierT8AssertRouteContract(t, payload, frontierT8ReusableSkillCandidateSchemaID)
	candidate := anyMap(payload["candidate"])
	if anyToString(candidate["status"]) != "inactive" || anyToBool(anyMap(candidate["persistence"])["performed"]) {
		t.Fatalf("candidate crossed inactive advisory boundary: %#v", candidate)
	}
	if !anyToBool(anyMap(anyMap(payload["skills_index"])["candidate_name"])["detected"]) || anyToBool(anyMap(payload["skills_index"])["mutation_performed"]) {
		t.Fatalf("native Skills Index lookup was not read-only and exact: %#v", payload["skills_index"])
	}
	s.skillFoundry.mu.RLock()
	drafts, evaluations, exports, retirements := len(s.skillFoundry.drafts), len(s.skillFoundry.evaluations), len(s.skillFoundry.exports), len(s.skillFoundry.retirements)
	s.skillFoundry.mu.RUnlock()
	if drafts != 0 || evaluations != 0 || exports != 0 || retirements != 0 {
		t.Fatalf("candidate-only route persisted Foundry state: drafts=%d evaluations=%d exports=%d retirements=%d", drafts, evaluations, exports, retirements)
	}
	encoded := recorder.Body.String()
	if strings.Contains(encoded, root) || strings.Contains(encoded, "skill_markdown") {
		t.Fatalf("route leaked a local path or raw generated content: %s", encoded)
	}
}

func TestFrontierT8EvolutionRouteExplicitHandoffReusesFoundryAndStopsBeforeExport(t *testing.T) {
	s, handler, root := frontierT8RouteTestServer(t)
	input := frontierT8RouteCurrentPayload(t, frontierT8TestSkillCandidatePayload())
	frontierT8SeedEvidenceAuthority(t, s, input)
	recorder, payload := frontierT8RouteRequest(t, handler, http.MethodPost, map[string]any{
		"operation":        frontierT8OperationHandoffReusable,
		"explicit_handoff": true,
		"input":            input,
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("handoff status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	frontierT8AssertRouteContract(t, payload, frontierT8ReusableSkillCandidateSchemaID)
	handoff := anyMap(payload["foundry_handoff"])
	if !anyToBool(handoff["performed"]) || !anyToBool(handoff["draft_recorded"]) || !anyToBool(handoff["evaluation_recorded"]) {
		t.Fatalf("Foundry lifecycle was not reused: %#v", handoff)
	}
	for _, field := range []string{"export_performed", "installation_performed", "activation_performed", "deactivation_performed", "terminal_retirement_performed"} {
		if anyToBool(handoff[field]) {
			t.Fatalf("handoff crossed forbidden lifecycle action %s: %#v", field, handoff)
		}
	}
	draftID := anyToString(anyMap(handoff["draft_ref"])["draft_id"])
	draft := s.skillFoundry.draft(draftID)
	evaluation := s.skillFoundry.latestEvaluation(draftID)
	if anyToString(draft["status"]) != "evaluated" || !anyToBool(evaluation["passed"]) || anyToString(evaluation["activation_state"]) != "inactive" {
		t.Fatalf("unexpected Foundry handoff state: draft=%#v evaluation=%#v", draft, evaluation)
	}
	material := anyMap(draft["workflow_material"])
	for _, key := range []string{"source_candidate_id", "source_workflow_signature", "prerequisites", "rollback", "side_effects", "platform_constraints", "verification_commands"} {
		if !frontierT8Meaningful(material[key]) {
			t.Fatalf("Foundry handoff lost workflow material %s: %#v", key, material)
		}
	}
	replayRecorder, replayPayload := frontierT8RouteRequest(t, handler, http.MethodPost, map[string]any{
		"operation": frontierT8OperationHandoffReusable, "explicit_handoff": true, "input": input,
	})
	if replayRecorder.Code != http.StatusOK || !anyToBool(anyMap(replayPayload["foundry_handoff"])["idempotent"]) {
		t.Fatalf("Foundry handoff replay was not idempotent status=%d body=%s", replayRecorder.Code, replayRecorder.Body.String())
	}
	s.skillFoundry.mu.RLock()
	evaluationCount, transactionCount := len(s.skillFoundry.evaluations), len(s.skillFoundry.transactions)
	s.skillFoundry.mu.RUnlock()
	if evaluationCount != 1 || transactionCount != 1 {
		t.Fatalf("Foundry replay duplicated state: evaluations=%d transactions=%d", evaluationCount, transactionCount)
	}
	s.skillFoundry.mu.RLock()
	exports, retirements := len(s.skillFoundry.exports), len(s.skillFoundry.retirements)
	s.skillFoundry.mu.RUnlock()
	if exports != 0 || retirements != 0 {
		t.Fatalf("handoff exported or terminally retired a draft: exports=%d retirements=%d", exports, retirements)
	}
	if strings.Contains(recorder.Body.String(), root) || strings.Contains(recorder.Body.String(), "skill_markdown") {
		t.Fatalf("handoff response leaked local path or raw generated content: %s", recorder.Body.String())
	}
	if err := s.skillFoundry.compact(); err != nil {
		t.Fatalf("compact Foundry transaction: %v", err)
	}
	file, err := os.OpenFile(s.skillFoundry.path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open Foundry ledger for torn-row fixture: %v", err)
	}
	if _, err := file.WriteString(`{"schema_id":"skill_foundry_transaction.v1","transaction_id":"torn"`); err != nil {
		_ = file.Close()
		t.Fatalf("write torn transaction fixture: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close torn transaction fixture: %v", err)
	}
	restarted, err := newSkillFoundryStoreFromEnv()
	if err != nil {
		t.Fatalf("restart Foundry store: %v", err)
	}
	if len(restarted.transaction(anyToString(handoff["transaction_id"]))) == 0 || len(restarted.transaction("torn")) != 0 {
		t.Fatalf("restart did not preserve the committed transaction and ignore the torn row: %#v", restarted.snapshot())
	}
	if anyToString(restarted.draft(draftID)["status"]) != "evaluated" || len(restarted.evaluations) != 1 || restarted.parseErrors != 1 {
		t.Fatalf("restart lifecycle mismatch: %#v", restarted.snapshot())
	}
}

func TestFrontierT8EvolutionRouteRetirementIsDistinctReadOnlyAndProtectedWhenSeasonal(t *testing.T) {
	s, handler, _ := frontierT8RouteTestServer(t)
	baseline := frontierT8RouteCurrentPayload(t, frontierT8TestRetirementPayload())
	frontierT8SeedEvidenceAuthority(t, s, baseline)
	recorder, payload := frontierT8RouteRequest(t, handler, http.MethodPost, map[string]any{
		"operation": frontierT8OperationDeriveRetirement,
		"input":     baseline,
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("retirement status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	frontierT8AssertRouteContract(t, payload, frontierT8SkillRetirementCandidateSchemaID)
	candidate := anyMap(payload["candidate"])
	if anyToString(candidate["status"]) != "candidate" || anyToString(candidate["schema_id"]) == skillRetirementContractID || anyToBool(candidate["terminal_retirement"]) {
		t.Fatalf("retirement advisory crossed terminal boundary: %#v", candidate)
	}
	index := anyMap(payload["skills_index"])
	if !anyToBool(anyMap(index["current_skill"])["detected"]) || !anyToBool(anyMap(index["replacement"])["detected"]) || anyToBool(index["mutation_performed"]) {
		t.Fatalf("retirement lookup did not use the read-only native index: %#v", index)
	}

	seasonal := frontierT8TestCloneMap(t, baseline)
	seasonal["seasonality"] = map[string]any{"seasonal": true, "full_observation_cycle": true, "season_id": "tax-season"}
	protectedRecorder, protectedPayload := frontierT8RouteRequest(t, handler, http.MethodPost, map[string]any{
		"operation": frontierT8OperationDeriveRetirement,
		"input":     seasonal,
	})
	if protectedRecorder.Code != http.StatusOK || anyToString(anyMap(protectedPayload["candidate"])["status"]) != "protected" {
		t.Fatalf("seasonal retirement did not abstain status=%d body=%s", protectedRecorder.Code, protectedRecorder.Body.String())
	}
	s.skillFoundry.mu.RLock()
	drafts, retirements := len(s.skillFoundry.drafts), len(s.skillFoundry.retirements)
	s.skillFoundry.mu.RUnlock()
	if drafts != 0 || retirements != 0 {
		t.Fatalf("retirement advisory mutated Foundry: drafts=%d retirements=%d", drafts, retirements)
	}
}

func TestFrontierT8EvolutionRouteRejectsSelfAssertedEvidenceWithoutAuthoritativeLedgers(t *testing.T) {
	_, handler, _ := frontierT8RouteTestServer(t)
	input := frontierT8RouteCurrentPayload(t, frontierT8TestSkillCandidatePayload())
	recorder, payload := frontierT8RouteRequest(t, handler, http.MethodPost, map[string]any{
		"operation": frontierT8OperationDeriveReusable, "input": input,
	})
	if recorder.Code != http.StatusUnprocessableEntity || anyToString(payload["error"]) != "reusable_candidate_rejected" || !strings.Contains(anyToString(payload["detail"]), "was not found") {
		t.Fatalf("self-asserted evidence did not fail closed status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestFrontierT8EvidenceClaimCollectionIsDeterministic(t *testing.T) {
	input := map[string]any{
		"zeta":  map[string]any{"evidence_refs": []any{map[string]any{"ref_id": "z"}}},
		"alpha": map[string]any{"evidence_refs": []any{map[string]any{"ref_id": "a"}}},
	}
	var first []frontierT8EvidenceClaim
	if err := frontierT8CollectEvidenceClaims(input, "input", time.Time{}, &first); err != nil {
		t.Fatalf("first collection failed: %v", err)
	}
	var second []frontierT8EvidenceClaim
	if err := frontierT8CollectEvidenceClaims(input, "input", time.Time{}, &second); err != nil {
		t.Fatalf("second collection failed: %v", err)
	}
	if len(first) != 2 || len(second) != 2 || first[0].Path != "input.alpha.evidence_refs[0]" || first[1].Path != "input.zeta.evidence_refs[0]" {
		t.Fatalf("claims were not collected in canonical order: first=%#v second=%#v", first, second)
	}
	if first[0].Path != second[0].Path || first[1].Path != second[1].Path {
		t.Fatalf("claim order drifted across identical input: first=%#v second=%#v", first, second)
	}
}

func TestFrontierT8EvolutionRouteIsStrictAndRequiresExplicitHandoff(t *testing.T) {
	_, handler, _ := frontierT8RouteTestServer(t)
	tests := []struct {
		name       string
		method     string
		payload    any
		wantStatus int
		wantError  string
	}{
		{name: "post_only", method: http.MethodGet, wantStatus: http.StatusMethodNotAllowed, wantError: "method_not_allowed"},
		{name: "unknown_top_level", method: http.MethodPost, payload: map[string]any{"operation": frontierT8OperationDeriveReusable, "input": frontierT8TestSkillCandidatePayload(), "unexpected": true}, wantStatus: http.StatusBadRequest, wantError: "invalid_evolution_request"},
		{name: "unsupported_operation", method: http.MethodPost, payload: map[string]any{"operation": "export", "input": frontierT8TestSkillCandidatePayload()}, wantStatus: http.StatusBadRequest, wantError: "unsupported_evolution_operation"},
		{name: "handoff_requires_explicit_true", method: http.MethodPost, payload: map[string]any{"operation": frontierT8OperationHandoffReusable, "input": frontierT8TestSkillCandidatePayload()}, wantStatus: http.StatusBadRequest, wantError: "explicit_handoff_required"},
		{name: "nested_raw_content_rejected", method: http.MethodPost, payload: map[string]any{"operation": frontierT8OperationDeriveReusable, "input": map[string]any{"raw_content": "hidden"}}, wantStatus: http.StatusUnprocessableEntity, wantError: "reusable_candidate_rejected"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder, payload := frontierT8RouteRequest(t, handler, test.method, test.payload)
			if recorder.Code != test.wantStatus || anyToString(payload["error"]) != test.wantError {
				t.Fatalf("status=%d error=%q body=%s", recorder.Code, anyToString(payload["error"]), recorder.Body.String())
			}
		})
	}
}
