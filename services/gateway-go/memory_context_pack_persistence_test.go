package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func contextPackPersistenceTestQualitySample() map[string]any {
	return buildContextPackQualitySample(contextPackQualitySampleInput{
		Query: "persistence hook test", Project: "contextlattice", TaskClass: "agent_workflow", RetrievalIntent: "decision",
		TokenImpact: map[string]any{}, Compiled: map[string]any{}, SourceCoverage: map[string]any{}, GraphQuality: map[string]any{},
		RankedEvidence: []any{map[string]any{
			"candidate_id": "rtc_aaaaaaaaaaaaaaaaaaaaaaaa", "kind": "memory", "rank": 1,
		}},
	})
}

func contextPackPersistenceTestInput(learned contextPackLearnedActivationDecision) contextPackCompilationInput {
	return contextPackCompilationInput{
		Query: "persistence hook test", Project: "contextlattice", TaskClass: "agent_workflow",
		RetrievalMode: "balanced", RetrievalIntent: "decision", SessionID: "persistence-session", AgentID: "codex-test",
		ContextPack: map[string]any{
			"results": []any{}, "facts": []any{}, "relevant_decisions": []any{}, "known_failure_modes": []any{},
			"acceptance_criteria": []any{}, "runbooks": []any{}, "capabilities_to_use": []any{}, "graph_neighbors": []any{},
		},
		SearchResponse: map[string]any{}, RequestPayload: map[string]any{"session_id": "persistence-session"},
		SourceCoverage: map[string]any{}, GraphQuality: map[string]any{}, Learned: learned,
	}
}

func contextPackPersistenceTestServer(t *testing.T, enabled bool) *server {
	t.Helper()
	ledger := &contextPackQualityLedger{enabled: enabled}
	if enabled {
		ledger.path = filepath.Join(t.TempDir(), "quality.ndjson")
		ledger.maxBytes = 2 * 1024 * 1024
		ledger.maxSamples = 20
		ledger.writeFile = writeOwnerOnlyDurableAtomicFile
		if err := prepareOwnerOnlyFile(ledger.path, true); err != nil {
			t.Fatalf("prepare quality ledger: %v", err)
		}
	}
	return &server{contextPackQuality: newContextPackQualityTelemetryWithLedger(20, ledger)}
}

func TestContextPackCompilationHookRunsBeforeDurableQualityRecord(t *testing.T) {
	s := contextPackPersistenceTestServer(t, true)
	input := contextPackPersistenceTestInput(contextPackLearnedActivationDecision{})
	artifacts := contextPackCompilationArtifacts{Quality: contextPackPersistenceTestQualitySample()}
	calls := []bool{}
	if got := s.persistContextPackCompilationOrFallbackWithHook(input, artifacts, func(gotInput contextPackCompilationInput, gotArtifacts contextPackCompilationArtifacts, durable bool) contextPackCompilationArtifacts {
		if gotInput.Query != input.Query || anyToString(gotArtifacts.Quality["sample_id"]) == "" {
			t.Fatalf("hook did not receive the retrieved input and quality artifacts: input=%#v artifacts=%#v", gotInput, gotArtifacts)
		}
		if s.contextPackQuality.sampleCount != 0 {
			t.Fatalf("durable quality row was recorded before hook: count=%d", s.contextPackQuality.sampleCount)
		}
		calls = append(calls, durable)
		return gotArtifacts
	}); got.Quality["sample_id"] == nil {
		t.Fatalf("durable persistence returned incomplete artifacts: %#v", got)
	}
	if !reflect.DeepEqual(calls, []bool{true}) {
		t.Fatalf("unexpected successful hook sequence: %v", calls)
	}
	if s.contextPackQuality.sampleCount != 1 || len(s.contextPackQuality.samples) != 1 {
		t.Fatalf("durable success was not recorded exactly once: count=%d samples=%d", s.contextPackQuality.sampleCount, len(s.contextPackQuality.samples))
	}
	if len(s.contextPackQuality.durableReceiptSamples) != 1 {
		t.Fatalf("durable receipt index missing after success: %#v", s.contextPackQuality.durableReceiptSamples)
	}
}

func TestContextPackCompilationCarriesCanonicalWorkspaceThroughUtility(t *testing.T) {
	workspaceRef := contextPackLearnedScopeRef("workspace", "native-control-owner")
	input := contextPackPersistenceTestInput(contextPackLearnedActivationDecision{})
	input.WorkspaceRef = workspaceRef
	input.RequestPayload["task_id"] = "workspace-task"
	input.RequestPayload["task_identity_id"] = "workspace-identity"
	input.RequestPayload["execution_lane_id"] = "workspace-lane"
	artifacts := buildContextPackCompilationArtifacts(input)
	if got := anyToString(artifacts.Quality["workspace_ref"]); got != workspaceRef {
		t.Fatalf("compiled native-control quality sample lost workspace: got=%q want=%q", got, workspaceRef)
	}
	quality := contextPackQualityEntryFromSample(artifacts.Quality)
	if got := anyToString(quality["workspace_ref"]); got != workspaceRef {
		t.Fatalf("canonical quality row lost workspace: got=%q want=%q", got, workspaceRef)
	}
	outcome, err := contextPackQualityOutcomeFromSampleChecked(map[string]any{
		"sample_id": anyToString(quality["sample_id"]), "project": input.Project,
		"first_pass_success": true, "outcome_id": "workspace-outcome",
	})
	if err != nil {
		t.Fatalf("normalize workspace outcome: %v", err)
	}
	outcome, err = bindContextPackQualityOutcomeSample(outcome, quality)
	if err != nil || anyToString(outcome["workspace_ref"]) != workspaceRef {
		t.Fatalf("outcome did not retain canonical workspace: outcome=%#v err=%v", outcome, err)
	}
	utility := buildUtilityObservation(outcome, quality, map[string]any{}, nil)
	if got := anyToString(utility["workspace_ref"]); got != workspaceRef {
		t.Fatalf("Utility observation lost canonical workspace: got=%q want=%q row=%#v", got, workspaceRef, utility)
	}
}

func TestContextPackCompilationHookRetriesUnboundLocalSampleOnceAfterOrdinaryFailure(t *testing.T) {
	s := contextPackPersistenceTestServer(t, false)
	input := contextPackPersistenceTestInput(contextPackLearnedActivationDecision{})
	artifacts := contextPackCompilationArtifacts{Quality: contextPackPersistenceTestQualitySample()}
	calls := []bool{}
	got := s.persistContextPackCompilationOrFallbackWithHook(input, artifacts, func(gotInput contextPackCompilationInput, gotArtifacts contextPackCompilationArtifacts, durable bool) contextPackCompilationArtifacts {
		if s.contextPackQuality.sampleCount != 0 {
			t.Fatalf("quality row was recorded before hook phase %v: count=%d", durable, s.contextPackQuality.sampleCount)
		}
		calls = append(calls, durable)
		if !durable {
			delete(gotArtifacts.Quality, "selection_receipt")
		}
		return gotArtifacts
	})
	if !reflect.DeepEqual(calls, []bool{true, false}) {
		t.Fatalf("ordinary failure did not rerun hook exactly once unbound: %v", calls)
	}
	if s.contextPackQuality.sampleCount != 1 || len(s.contextPackQuality.samples) != 1 {
		t.Fatalf("ordinary failure double-counted or dropped local sample: count=%d samples=%d", s.contextPackQuality.sampleCount, len(s.contextPackQuality.samples))
	}
	if len(s.contextPackQuality.durableReceiptSamples) != 0 || len(anyMap(s.contextPackQuality.samples[0]["selection_receipt"])) != 0 {
		t.Fatalf("ordinary failure retained a durable receipt in local fallback: %#v", s.contextPackQuality.samples[0])
	}
	if anyToString(got.Quality["sample_id"]) == "" {
		t.Fatalf("ordinary fallback returned incomplete artifacts: %#v", got)
	}
}

func TestContextPackCompilationHookLearnedFailureRebuildsControlWithoutSecondRetrieval(t *testing.T) {
	learned := contextPackLearnedActivationDecision{
		Armed: true, Eligible: true, AssignedTreatment: true, Performed: true, Arm: "canary",
		Reason: "canary_assigned", CandidateMultipliers: map[string]float64{"rtc_aaaaaaaaaaaaaaaaaaaaaaaa": 1.15},
	}
	s := contextPackPersistenceTestServer(t, false)
	input := contextPackPersistenceTestInput(learned)
	artifacts := contextPackCompilationArtifacts{
		Quality: contextPackPersistenceTestQualitySample(), Learned: learned,
		Compiled: map[string]any{"retrieval_marker": input.SearchResponse},
	}
	calls := []bool{}
	hookInputs := []contextPackLearnedActivationDecision{}
	got := s.persistContextPackCompilationOrFallbackWithHook(input, artifacts, func(gotInput contextPackCompilationInput, gotArtifacts contextPackCompilationArtifacts, durable bool) contextPackCompilationArtifacts {
		if s.contextPackQuality.sampleCount != 0 {
			t.Fatalf("quality row was recorded before learned hook phase %v: count=%d", durable, s.contextPackQuality.sampleCount)
		}
		calls = append(calls, durable)
		hookInputs = append(hookInputs, gotInput.Learned)
		if !durable {
			delete(gotArtifacts.Quality, "selection_receipt")
		}
		return gotArtifacts
	})
	if !reflect.DeepEqual(calls, []bool{true, false}) {
		t.Fatalf("learned failure did not rerun hook exactly once: %v", calls)
	}
	if len(hookInputs) != 2 || hookInputs[0].Arm != "canary" || hookInputs[0].Eligible != true ||
		hookInputs[1].Arm != "shadow" || hookInputs[1].Eligible || hookInputs[1].Performed || hookInputs[1].Reason != "receipt_persistence_failed" {
		t.Fatalf("learned fallback did not expose exact control identity: %#v", hookInputs)
	}
	if got.Learned.Eligible || got.Learned.Performed || got.Learned.Arm != "shadow" || got.Learned.Reason != "receipt_persistence_failed" {
		t.Fatalf("learned fallback returned non-control artifacts: %#v", got.Learned)
	}
	if s.contextPackQuality.sampleCount != 1 || len(s.contextPackQuality.samples) != 1 || len(s.contextPackQuality.durableReceiptSamples) != 0 {
		t.Fatalf("learned fallback was not exactly one local unbound sample: count=%d samples=%d receipts=%d", s.contextPackQuality.sampleCount, len(s.contextPackQuality.samples), len(s.contextPackQuality.durableReceiptSamples))
	}
}

func TestContextPackCompilationHookLearnedFailureUsesNewUnboundFallbackResponseIdentity(t *testing.T) {
	learned := contextPackLearnedActivationDecision{
		Armed: true, Eligible: true, AssignedTreatment: true, Performed: true, Arm: "canary",
		Reason: "canary_assigned", CandidateMultipliers: map[string]float64{"rtc_aaaaaaaaaaaaaaaaaaaaaaaa": 1.15},
	}
	s := contextPackPersistenceTestServer(t, false)
	input := contextPackPersistenceTestInput(learned)
	input.ContextPack["ranked_evidence"] = []any{map[string]any{
		"candidate_id": "rtc_aaaaaaaaaaaaaaaaaaaaaaaa", "kind": "memory", "rank": 1, "score": 0.8,
	}}
	artifacts := buildContextPackCompilationArtifacts(input)
	// Keep the fixture's explicit learned decision eligible; the production
	// builder supplies this decision from the retrieval authority boundary.
	artifacts.Learned = learned
	if len(contextPackSelectionReceiptFromSample(artifacts.Quality["selection_receipt"])) == 0 {
		t.Fatal("learned response fixture did not produce a durable selection receipt")
	}
	request := map[string]any{
		"query": input.Query, "project": input.Project, "topic_path": input.TopicPath,
		"agent_id": input.AgentID, "retrieval_mode": input.RetrievalMode, "retrieval_intent": input.RetrievalIntent,
	}
	ids := []string{}
	got := s.persistContextPackCompilationOrFallbackWithHook(input, artifacts, func(gotInput contextPackCompilationInput, gotArtifacts contextPackCompilationArtifacts, durable bool) contextPackCompilationArtifacts {
		if !durable {
			for _, key := range []string{"recall_response_id", "recall_response_digest", "response_component_refs"} {
				delete(gotArtifacts.Quality, key)
			}
			delete(gotArtifacts.Quality, "selection_receipt")
		}
		response := composeRecallResponse(recallResponseCompositionInputFromCompilation(request, gotInput, gotArtifacts, durable))
		response = finalizeRecallResponseTransport(response, input.AgentID, "recall_response", memoryRecallResponsePath)
		if durable {
			binding, ok := recallResponseBindingFromResponse(response)
			if !ok || !recallResponseCopyBinding(gotArtifacts.Quality, binding) {
				t.Fatalf("learned provisional response did not produce complete binding: %#v", response)
			}
		}
		ids = append(ids, anyToString(response["response_id"]))
		return gotArtifacts
	})
	if len(ids) != 2 || ids[0] == "" || ids[1] == "" || ids[0] == ids[1] {
		t.Fatalf("learned persistence failure reused provisional response identity: ids=%v", ids)
	}
	if got.Learned.Eligible || got.Learned.Performed || got.Learned.Arm != "shadow" || got.Learned.Reason != "receipt_persistence_failed" {
		t.Fatalf("learned fallback did not return control artifacts: %#v", got.Learned)
	}
	if len(s.contextPackQuality.samples) != 1 || recallResponseBindingHasAnyFields(s.contextPackQuality.samples[0]) {
		t.Fatalf("fallback local sample was not exactly one unbound row: %#v", s.contextPackQuality.samples)
	}
	fallbackResponse := composeRecallResponse(recallResponseCompositionInputFromCompilation(request, input, got, false))
	fallbackResponse = finalizeRecallResponseTransport(fallbackResponse, input.AgentID, "recall_response", memoryRecallResponsePath)
	if anyToString(fallbackResponse["response_id"]) != ids[1] {
		t.Fatalf("returned fallback response did not match fallback artifacts: got=%q want=%q", anyToString(fallbackResponse["response_id"]), ids[1])
	}
}

func TestRecallResponseCompositionProjectionDoesNotMutateCompilationArtifacts(t *testing.T) {
	input := contextPackPersistenceTestInput(contextPackLearnedActivationDecision{})
	artifacts := buildContextPackCompilationArtifacts(input)
	before, err := json.Marshal(artifacts.ContextPack)
	if err != nil {
		t.Fatalf("marshal context pack before projection: %v", err)
	}
	request := map[string]any{
		"query": input.Query, "project": input.Project, "topic_path": input.TopicPath,
		"agent_id": input.AgentID, "retrieval_mode": input.RetrievalMode, "retrieval_intent": input.RetrievalIntent,
	}
	_ = recallResponseCompositionInputFromCompilation(request, input, artifacts, true)
	after, err := json.Marshal(artifacts.ContextPack)
	if err != nil {
		t.Fatalf("marshal context pack after projection: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("recall response projection mutated compilation artifacts: before=%s after=%s", before, after)
	}
}

func TestContextPackQualityRecallBindingSurvivesRestartAndCompaction(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "quality.ndjson")
	newLedger := func(maxBytes int64) *contextPackQualityLedger {
		return &contextPackQualityLedger{
			enabled: true, path: ledgerPath, maxBytes: maxBytes, maxSamples: 20,
			writeFile: writeOwnerOnlyDurableAtomicFile,
		}
	}
	initial := contextPackPersistenceTestQualitySample()
	response := composeRecallResponse(recallResponseTestInput(true))
	binding, ok := recallResponseBindingFromResponse(response)
	if !ok || !recallResponseCopyBinding(initial, binding) {
		t.Fatalf("failed to create complete quality binding fixture: response=%#v sample=%#v", response, initial)
	}
	ledger := newLedger(2 * 1024 * 1024)
	if err := prepareOwnerOnlyFile(ledger.path, true); err != nil {
		t.Fatalf("prepare quality ledger: %v", err)
	}
	telemetry := newContextPackQualityTelemetryWithLedger(20, ledger)
	if err := telemetry.recordQualityDurably(initial); err != nil {
		t.Fatalf("persist bound quality sample: %v", err)
	}
	rows, _, err := ledger.readRows()
	if err != nil || len(rows) != 1 {
		t.Fatalf("initial durable quality row missing: rows=%#v err=%v", rows, err)
	}
	if !recallResponseBindingsEqual(rows[0], binding) {
		t.Fatalf("initial durable binding changed: row=%#v binding=%#v", rows[0], binding)
	}

	restarted := newContextPackQualityTelemetryWithLedger(20, newLedger(2*1024*1024))
	loaded, found := restarted.sampleForUtility(anyToString(initial["sample_id"]))
	if !found || loaded == nil || !recallResponseBindingsEqual(loaded, binding) {
		t.Fatalf("restart did not load complete response binding into quality sample: loaded=%#v found=%v", loaded, found)
	}
	rows, _, err = restarted.ledger.readRows()
	if err != nil || len(rows) != 1 || !recallResponseBindingsEqual(rows[0], binding) {
		t.Fatalf("restart did not preserve complete response binding: rows=%#v err=%v", rows, err)
	}

	stat, err := os.Stat(ledgerPath)
	if err != nil {
		t.Fatalf("stat quality ledger: %v", err)
	}
	// A duplicate append forces bounded compaction while retaining the newest
	// complete row and its response binding.
	restarted.ledger.maxBytes = stat.Size() + 1
	if err := restarted.recordQualityDurably(initial); err != nil {
		t.Fatalf("append bound quality sample through compaction: %v", err)
	}
	compactedRows, _, err := restarted.ledger.readRows()
	if err != nil || len(compactedRows) == 0 {
		t.Fatalf("compaction removed all quality rows: rows=%#v err=%v", compactedRows, err)
	}
	for _, row := range compactedRows {
		if !recallResponseBindingsEqual(row, binding) {
			t.Fatalf("compaction changed complete response binding: row=%#v binding=%#v", row, binding)
		}
	}
}

func TestContextPackQualityRecallBindingRejectsPartialOrMalformedRows(t *testing.T) {
	response := composeRecallResponse(recallResponseTestInput(true))
	binding, ok := recallResponseBindingFromResponse(response)
	if !ok {
		t.Fatal("failed to create binding fixture")
	}
	base := contextPackPersistenceTestQualitySample()
	for name, mutate := range map[string]func(map[string]any){
		"partial": func(sample map[string]any) {
			sample["recall_response_id"] = binding["recall_response_id"]
		},
		"malformed_digest": func(sample map[string]any) {
			_ = recallResponseCopyBinding(sample, binding)
			sample["recall_response_digest"] = "not-a-digest"
		},
		"malformed_component": func(sample map[string]any) {
			_ = recallResponseCopyBinding(sample, binding)
			refs := contextPackAnyList(sample["response_component_refs"])
			refs[0].(map[string]any)["ordinal"] = 99
		},
	} {
		t.Run(name, func(t *testing.T) {
			sample := cloneJSONMap(base)
			mutate(sample)
			if got := contextPackQualityEntryFromSample(sample); got != nil {
				t.Fatalf("invalid binding became a usable quality row: %#v", got)
			}
			ledger := &contextPackQualityLedger{enabled: true, path: filepath.Join(t.TempDir(), "quality.ndjson"), maxBytes: 2 * 1024 * 1024, maxSamples: 20, writeFile: writeOwnerOnlyDurableAtomicFile}
			if err := prepareOwnerOnlyFile(ledger.path, true); err != nil {
				t.Fatalf("prepare quality ledger: %v", err)
			}
			telemetry := newContextPackQualityTelemetryWithLedger(20, ledger)
			if err := telemetry.recordQualityDurably(sample); err == nil {
				t.Fatal("durable append accepted malformed response binding")
			}
			if telemetry.sampleCount != 0 {
				t.Fatalf("malformed binding entered local quality samples: %#v", telemetry.samples)
			}
		})
	}

	legacy := cloneJSONMap(base)
	if entry := contextPackQualityEntryFromSample(legacy); entry == nil {
		t.Fatal("legacy unbound quality row no longer loads")
	}
	encoded, _ := json.Marshal(legacy)
	if strings.Contains(string(encoded), "recall_response_") {
		t.Fatalf("legacy fixture unexpectedly carried response binding: %s", encoded)
	}

	malformed := contextPackQualityEntryFromSample(base)
	malformed["recall_response_id"] = binding["recall_response_id"]
	malformedBytes, err := json.Marshal(malformed)
	if err != nil {
		t.Fatalf("marshal malformed durable row: %v", err)
	}
	ledgerPath := filepath.Join(t.TempDir(), "malformed-quality.ndjson")
	if err := os.WriteFile(ledgerPath, append(malformedBytes, '\n'), 0o600); err != nil {
		t.Fatalf("write malformed durable row: %v", err)
	}
	ledger := &contextPackQualityLedger{
		enabled: true, path: ledgerPath, maxBytes: 2 * 1024 * 1024, maxSamples: 20,
		writeFile: writeOwnerOnlyDurableAtomicFile,
	}
	rows, parseErrors, err := ledger.readRows()
	if err != nil || len(rows) != 0 || parseErrors != 1 {
		t.Fatalf("restart reader did not fail closed on malformed binding: rows=%#v parse_errors=%d err=%v", rows, parseErrors, err)
	}
	restarted := newContextPackQualityTelemetryWithLedger(20, ledger)
	if restarted.sampleCount != 0 || len(restarted.samples) != 0 {
		t.Fatalf("restart admitted malformed response-bound quality row: %#v", restarted.samples)
	}
}
