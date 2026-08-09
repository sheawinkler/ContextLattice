package main

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func continuousCognitionLifecycleSelectionReceipt(workspaceRef string) map[string]any {
	decision := contextPackLearnedActivationDecision{
		Armed: true, Eligible: true, Arm: "control", Reason: "deterministic_control",
		CanaryPercent: 5, ExposureBucket: 500,
		RequestRef:              contextPackLearnedScopeRef("request", "cc-lifecycle"),
		ProjectScopeRef:         contextPackLearnedScopeRef("project", "contextlattice"),
		TaskClassScopeRef:       contextPackLearnedScopeRef("task_class", "agent_workflow"),
		RetrievalIntentScopeRef: contextPackLearnedScopeRef("retrieval_intent", "decision"),
		WorkspaceRef:            workspaceRef,
		PolicyRef:               contextPackLearnedScopeRef("policy", "cc-lifecycle"),
		ImpactProofRef:          contextPackLearnedScopeRef("impact", "cc-lifecycle"),
		ActuatorComparatorRef:   contextPackLearnedScopeRef("comparator", "cc-lifecycle"),
		ReputationSnapshotRef:   contextPackLearnedScopeRef("reputation", "cc-lifecycle"),
	}
	decision.ActivationReceiptID = contextPackLearnedActivationReceiptID(decision)
	return contextPackSelectionReceiptFromCandidatesWithActivation(nil, contextPackLearnedActivationReceipt(decision))
}

type continuousCognitionLifecycleFixture struct {
	server       *server
	request      continuousCognitionRequest
	snapshot     agentProofTimelineSnapshot
	workspaceRef string
	qualityPath  string
	outcomeID    string
}

func newContinuousCognitionLifecycleFixture(t *testing.T) continuousCognitionLifecycleFixture {
	t.Helper()
	request := continuousCognitionTestRequest(t)
	request.Operation = continuousCognitionOperationEvaluate
	request.AgentID = "codex_test"
	request.SessionID = "cc-lifecycle-session"
	request.TaskID = "cc-lifecycle-task"
	request.TaskIdentityID = "cc-lifecycle-identity"
	request.ExecutionLaneID = "cc-lifecycle-lane"
	request.AsOf = time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	workspaceRef := contextPackLearnedScopeRef("workspace", "cc-lifecycle-workspace")
	request.WorkspaceRef = workspaceRef

	response := composeRecallResponse(recallResponseTestInput(true))
	binding, ok := recallResponseBindingFromResponse(response)
	if !ok {
		t.Fatalf("recall response fixture did not produce a canonical binding: %#v", response)
	}
	qualityInput := contextPackPersistenceTestQualitySample()
	for key, value := range map[string]any{
		"sample_id": "cc_lifecycle_sample", "capturedAt": "2026-08-08T10:00:00Z",
		"project": request.Project, "task_class": "agent_workflow", "retrieval_intent": "decision",
		"session_id": request.SessionID, "task_id": request.TaskID, "agent_id": request.AgentID,
		"task_identity_id": request.TaskIdentityID, "execution_lane_id": request.ExecutionLaneID,
		"selection_receipt": continuousCognitionLifecycleSelectionReceipt(workspaceRef),
	} {
		qualityInput[key] = value
	}
	if !recallResponseCopyBinding(qualityInput, binding) {
		t.Fatal("failed to bind quality fixture to the recall response")
	}
	qualityTelemetry, qualityPath := contextPackOutcomeResponseBindingTelemetry(t, qualityInput)
	quality, found, err := qualityTelemetry.durableQualitySampleForOutcome("cc_lifecycle_sample")
	if err != nil || !found {
		t.Fatalf("read durable quality sample: found=%t err=%v", found, err)
	}
	if got := anyToString(quality["workspace_ref"]); got != workspaceRef {
		t.Fatalf("quality fixture lost canonical workspace: got=%q want=%q", got, workspaceRef)
	}

	const outcomeID = "cc_lifecycle_outcome"
	const verificationEventID = "cc_lifecycle_verification"
	const verifierID = "cc_lifecycle_holdout"
	evidenceDigest := "sha256:" + sha256Hex("cc-lifecycle-evidence")
	outcomeInput := map[string]any{
		"outcome_id": outcomeID, "sample_id": anyToString(quality["sample_id"]),
		"project": request.Project, "capturedAt": "2026-08-08T10:05:00Z", "first_pass_success": true,
		"utility": map[string]any{
			"value": 8.0, "unit": "acceptance_points", "verification_event_id": verificationEventID,
			"evidence_digest": evidenceDigest, "verification_passed": true,
			"verifier_kind": "deterministic_test", "verifier_id": verifierID,
		},
		"economics": map[string]any{"latency_ms": 40, "cost_microusd": 0, "tool_calls": 2, "failures": 0},
	}
	entry, err := contextPackQualityOutcomeFromSampleChecked(outcomeInput)
	if err != nil {
		t.Fatalf("normalize lifecycle outcome: %v", err)
	}
	entry, err = bindContextPackQualityOutcomeSample(entry, quality)
	if err != nil {
		t.Fatalf("bind lifecycle outcome to quality admission: %v", err)
	}
	entry["gateway_received_at"] = "2026-08-08T10:06:00Z"
	if recorded, err := qualityTelemetry.recordOutcomeEntryDurably(entry); err != nil || !recorded {
		t.Fatalf("persist lifecycle outcome: recorded=%t err=%v", recorded, err)
	}
	outcome, found, err := qualityTelemetry.authoritativeOutcomeForSample(anyToString(quality["sample_id"]))
	if err != nil || !found {
		t.Fatalf("read authoritative lifecycle outcome: found=%t err=%v", found, err)
	}

	impact := map[string]any{
		"sample_id": anyToString(quality["sample_id"]), "session_id": request.SessionID,
		"task_id": request.TaskID, "task_identity_id": request.TaskIdentityID,
		"execution_lane_id": request.ExecutionLaneID, "project": request.Project, "agent_id": request.AgentID,
		"tokenizer_exact": true, "wire_tokens_exact": 600, "model_visible_context_tokens_exact": 400,
		"tokenizer_encoding": "cl100k_base", "capturedAt": "2026-08-08T10:04:00Z",
	}
	events := []map[string]any{{
		"id": verificationEventID, "session_id": request.SessionID, "type": "verification.completed",
		"agent_id": verifierID, "created_at": "2026-08-08T10:07:00Z",
		"metadata": map[string]any{"utility_verification": map[string]any{
			"outcome_id": outcomeID, "sample_id": anyToString(quality["sample_id"]),
			"utility_value": 8.0, "utility_unit": "acceptance_points", "evidence_digest": evidenceDigest,
			"verification_passed": true, "verifier_kind": "deterministic_test", "verifier_id": verifierID,
		}},
	}}
	utility := &utilityTelemetry{limit: 20, observations: []map[string]any{}, byOutcome: map[string]int{}, byOpaqueControlRef: map[string]int{}}
	row := buildUtilityObservation(outcome, quality, impact, events)
	row["updated_at"] = "2026-08-08T10:07:00Z"
	row["observation_digest"] = utilityObservationDigest(row)
	if stored, recorded, err := utility.record(row); err != nil || !recorded || anyToString(stored["status"]) != "verified_exact" {
		t.Fatalf("record canonical Utility observation: stored=%#v recorded=%t err=%v", stored, recorded, err)
	}

	return continuousCognitionLifecycleFixture{
		server: &server{contextPackQuality: qualityTelemetry, utility: utility}, request: request,
		snapshot:     agentProofTimelineSnapshot{QualitySamples: []map[string]any{quality}, QualityOutcomes: []map[string]any{outcome}},
		workspaceRef: workspaceRef, qualityPath: qualityPath, outcomeID: outcomeID,
	}
}

func TestContinuousCognitionLifecycleProjectsCanonicalOutcomeAndUtilityReadOnly(t *testing.T) {
	fixture := newContinuousCognitionLifecycleFixture(t)
	qualityBefore, err := os.ReadFile(fixture.qualityPath)
	if err != nil {
		t.Fatalf("read quality ledger before projection: %v", err)
	}
	fixture.server.utility.mu.Lock()
	utilityBefore := frontierT6Digest(fixture.server.utility.observations)
	fixture.server.utility.mu.Unlock()

	outcome, evaluation, gaps := continuousCognitionProjectOutcomeEvaluation(
		fixture.server, fixture.request, fixture.snapshot, fixture.workspaceRef,
	)
	if outcome.State != "recorded" || !outcome.IndependentlyVerified || outcome.CausalEligible {
		t.Fatalf("canonical outcome projection is not exact: %#v", outcome)
	}
	if evaluation.State != "evaluated" || evaluation.UtilityStatus != "verified_exact" || !evaluation.Verified || evaluation.CausalEligible || evaluation.Reason != "exact_canonical_utility_observation" {
		t.Fatalf("canonical Utility evaluation is not exact: %#v", evaluation)
	}
	if len(gaps) != 0 || !strings.HasPrefix(outcome.OutcomeRef, "ref_outcome_") ||
		!strings.HasPrefix(outcome.ProofRef, "ref_outcome_proof_") ||
		!strings.HasPrefix(outcome.UtilityObservationRef, "ref_utility_observation_") {
		t.Fatalf("canonical projection omitted opaque proof refs: outcome=%#v gaps=%#v", outcome, gaps)
	}
	for _, value := range []string{outcome.OutcomeRef, outcome.ProofRef, outcome.UtilityObservationRef} {
		if strings.Contains(value, fixture.outcomeID) {
			t.Fatalf("raw outcome identity leaked through lifecycle proof ref: %q", value)
		}
	}

	qualityAfter, err := os.ReadFile(fixture.qualityPath)
	if err != nil {
		t.Fatalf("read quality ledger after projection: %v", err)
	}
	fixture.server.utility.mu.Lock()
	utilityAfter := frontierT6Digest(fixture.server.utility.observations)
	fixture.server.utility.mu.Unlock()
	if !reflect.DeepEqual(qualityBefore, qualityAfter) || utilityBefore != utilityAfter {
		t.Fatal("lifecycle projection mutated canonical quality or Utility evidence")
	}
}

func TestContinuousCognitionLifecycleFailsClosedBeforeOutcomeAndOnReceiptConflict(t *testing.T) {
	fixture := newContinuousCognitionLifecycleFixture(t)
	beforeOutcome := fixture.request
	beforeOutcome.AsOf = time.Date(2026, time.August, 8, 10, 5, 30, 0, time.UTC)
	outcome, evaluation, gaps := continuousCognitionProjectOutcomeEvaluation(
		fixture.server, beforeOutcome, fixture.snapshot, fixture.workspaceRef,
	)
	if outcome.State != "absent" || evaluation.State != "unavailable" || len(gaps) != 1 || gaps[0].Code != "response_bound_outcome_not_found" {
		t.Fatalf("pre-receipt as_of boundary did not fail closed: outcome=%#v evaluation=%#v gaps=%#v", outcome, evaluation, gaps)
	}

	conflictingSample := cloneAnyMap(fixture.snapshot.QualitySamples[0])
	conflictingSample["quality_score"] = anyToInt(conflictingSample["quality_score"], 0) - 1
	if contextPackQualitySampleAdmissionRef(conflictingSample) == contextPackQualitySampleAdmissionRef(fixture.snapshot.QualitySamples[0]) {
		t.Fatal("conflict fixture did not change the quality admission ref")
	}
	conflictingSnapshot := fixture.snapshot
	conflictingSnapshot.QualitySamples = append([]map[string]any{fixture.snapshot.QualitySamples[0]}, conflictingSample)
	outcome, evaluation, gaps = continuousCognitionProjectOutcomeEvaluation(
		fixture.server, fixture.request, conflictingSnapshot, fixture.workspaceRef,
	)
	if outcome.State != "source_conflict" || evaluation.Reason != "conflicting_quality_receipts" || len(gaps) != 1 || gaps[0].Code != "source_conflict" {
		t.Fatalf("conflicting quality admissions were not explicit: outcome=%#v evaluation=%#v gaps=%#v", outcome, evaluation, gaps)
	}
}

func TestContinuousCognitionLifecycleRequiresExactLaneAndIntentIdentity(t *testing.T) {
	fixture := newContinuousCognitionLifecycleFixture(t)
	cases := map[string]func(*continuousCognitionRequest){
		"missing_task_identity": func(request *continuousCognitionRequest) { request.TaskIdentityID = "" },
		"wrong_task_identity":   func(request *continuousCognitionRequest) { request.TaskIdentityID = "other-identity" },
		"missing_lane":          func(request *continuousCognitionRequest) { request.ExecutionLaneID = "" },
		"wrong_lane":            func(request *continuousCognitionRequest) { request.ExecutionLaneID = "other-lane" },
		"wrong_intent":          func(request *continuousCognitionRequest) { request.RetrievalIntent = "exploration" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			request := fixture.request
			mutate(&request)
			outcome, evaluation, gaps := continuousCognitionProjectOutcomeEvaluation(fixture.server, request, fixture.snapshot, fixture.workspaceRef)
			if outcome.State != "absent" || evaluation.State != "unavailable" || len(gaps) != 1 || gaps[0].Code != "response_bound_outcome_not_found" {
				t.Fatalf("non-exact lifecycle identity selected durable evidence: outcome=%#v evaluation=%#v gaps=%#v", outcome, evaluation, gaps)
			}
		})
	}
}

func TestContinuousCognitionProofSnapshotAtExcludesFutureAndAmbiguousEvidence(t *testing.T) {
	asOf := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	build := func(futureMarker string) agentProofTimelineSnapshot {
		return agentProofTimelineSnapshot{
			Session: map[string]any{"id": "cc-temporal-session"},
			Events: []map[string]any{
				{"id": "event-prior", "created_at": "2026-08-08T11:00:00Z"},
				{"id": futureMarker, "created_at": "2026-08-08T13:00:00Z"},
			},
			ContinuityEntries: []continuityLedgerEntry{
				{EntryID: "continuity-prior", RecordedAt: "2026-08-08T11:00:00Z"},
				{EntryID: futureMarker, RecordedAt: "2026-08-08T13:00:00Z"},
			},
			Claims: []temporalClaim{
				{ClaimID: "claim-prior", CreatedAt: "2026-08-08T10:00:00Z", UpdatedAt: "2026-08-08T11:00:00Z"},
				{ClaimID: futureMarker, CreatedAt: "2026-08-08T10:00:00Z", UpdatedAt: "2026-08-08T13:00:00Z"},
			},
			QualitySamples: []map[string]any{
				{"sample_id": "sample-prior", "capturedAt": "2026-08-08T11:00:00Z"},
				{"sample_id": futureMarker, "capturedAt": "2026-08-08T13:00:00Z"},
				{"sample_id": "sample-ambiguous"},
			},
			QualityOutcomes: []map[string]any{
				{"outcome_id": "outcome-prior", "gateway_received_at": "2026-08-08T11:00:00Z"},
				{"outcome_id": futureMarker, "gateway_received_at": "2026-08-08T13:00:00Z"},
			},
			TokenImpacts: []map[string]any{
				{"sample_id": "impact-prior", "capturedAt": "2026-08-08T11:00:00Z"},
				{"sample_id": futureMarker, "capturedAt": "2026-08-08T13:00:00Z"},
			},
			Availability: map[string]bool{
				"agent_session": true, "continuity": true, "temporal_claim": true,
				"context_pack_quality": true, "token_impact": true,
			},
			SourceOmitted:       map[string]int{},
			SourceAnchorsBefore: map[string]any{"snapshot": "same"},
			SourceAnchorsAfter:  map[string]any{"snapshot": "same"},
		}
	}
	first, firstAmbiguous := continuousCognitionProofSnapshotAt(build("future-a"), asOf)
	second, secondAmbiguous := continuousCognitionProofSnapshotAt(build("future-b"), asOf)
	if firstAmbiguous != 2 || secondAmbiguous != 2 || first.SourceOmitted["temporal_projection"] != 2 {
		t.Fatalf("temporal ambiguity accounting is not exact: first=%d second=%d omitted=%#v", firstAmbiguous, secondAmbiguous, first.SourceOmitted)
	}
	if len(first.Events) != 1 || len(first.ContinuityEntries) != 1 || len(first.Claims) != 1 ||
		len(first.QualitySamples) != 1 || len(first.QualityOutcomes) != 1 || len(first.TokenImpacts) != 1 {
		t.Fatalf("historical snapshot retained future or ambiguous rows: %#v", first)
	}
	firstRef, _, _, firstAnchor := continuousCognitionProofProjectionFromSnapshot(first)
	secondRef, _, _, secondAnchor := continuousCognitionProofProjectionFromSnapshot(second)
	if firstRef != secondRef || firstAnchor != secondAnchor {
		t.Fatalf("future-only evidence changed historical proof: first=%s/%s second=%s/%s", firstRef, firstAnchor, secondRef, secondAnchor)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "future-a") || strings.Contains(string(encoded), "sample-ambiguous") {
		t.Fatalf("future or temporally ambiguous material survived projection: %s", encoded)
	}
}

func TestContinuousCognitionSessionAtReplaysOnlyBoundaryVisibleEvents(t *testing.T) {
	asOf := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	sessionID := "cc-historical-session"
	priorEvent := map[string]any{
		"id": "event-prior", "session_id": sessionID, "type": "verification.started",
		"agent_id": "agent-prior", "project": "contextlattice", "status": "active",
		"created_at": "2026-08-08T11:00:00Z",
		"metadata": map[string]any{
			"ownership":   map[string]any{"task_id": "task-prior", "task_identity_id": "identity-prior", "execution_lane_id": "lane-prior"},
			"next_action": "historical-action",
		},
	}
	store := &agentSessionStore{
		idleTTL: 24 * time.Hour,
		sessions: map[string]map[string]any{sessionID: {
			"id": sessionID, "project": "contextlattice", "agent_id": "agent-prior",
			"status": "completed", "next_action": "future-action-a", "event_count": 2,
			"started_at": "2026-08-08T10:00:00Z", "updated_at": "2026-08-08T13:00:00Z",
			"last_event_at": "2026-08-08T13:00:00Z", "last_event_type": "session.completed",
		}},
		events: map[string][]map[string]any{sessionID: {
			priorEvent,
			{"id": "future-a", "type": "session.completed", "created_at": "2026-08-08T13:00:00Z"},
		}},
	}

	firstSession, firstEvents, found, firstComplete := continuousCognitionSessionAt(store, sessionID, asOf)
	if !found || firstComplete || len(firstEvents) != 1 || anyToString(firstSession["status"]) != "active" {
		t.Fatalf("historical session did not fail closed around future state: session=%#v events=%#v found=%t complete=%t", firstSession, firstEvents, found, firstComplete)
	}
	firstRef := continuousCognitionSessionProjection(firstSession, firstEvents, asOf)

	store.sessions[sessionID]["status"] = "failed"
	store.sessions[sessionID]["next_action"] = "future-action-b"
	store.sessions[sessionID]["objective_state"] = "future-objective-b"
	store.sessions[sessionID]["event_count"] = 99
	store.sessions[sessionID]["updated_at"] = "2026-08-08T14:00:00Z"
	store.sessions[sessionID]["last_event_at"] = "2026-08-08T14:00:00Z"
	store.events[sessionID][1] = map[string]any{"id": "future-b", "type": "session.failed", "created_at": "2026-08-08T14:00:00Z"}

	secondSession, secondEvents, found, secondComplete := continuousCognitionSessionAt(store, sessionID, asOf)
	secondRef := continuousCognitionSessionProjection(secondSession, secondEvents, asOf)
	if !found || secondComplete || firstRef != secondRef || continuousCognitionStableDigest(firstSession) != continuousCognitionStableDigest(secondSession) {
		t.Fatalf("future-only session mutation changed the historical projection: first=%#v second=%#v refs=%s/%s", firstSession, secondSession, firstRef, secondRef)
	}
	encoded, err := json.Marshal(secondSession)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "future-") || anyToInt(secondSession["event_count"], -1) != 1 {
		t.Fatalf("future-only mutable session state survived replay: %s", encoded)
	}
}

func TestUtilityRowsForOutcomeIDsHonorsHistoricalBoundsForTreatmentsAndControls(t *testing.T) {
	rows := []map[string]any{
		utilityRowsForOutcomeIDsTestRow("prior-treatment", "2026-08-08T11:00:00Z", 1, map[string]any{"arm": "treatment", "matched_control_outcome_id": "future-control"}),
		utilityRowsForOutcomeIDsTestRow("future-treatment", "2026-08-08T13:00:00Z", 1, map[string]any{"arm": "treatment"}),
		utilityRowsForOutcomeIDsTestRow("invalid-treatment", "not-a-time", 1, map[string]any{"arm": "treatment"}),
		utilityRowsForOutcomeIDsTestRow("future-control", "2026-08-08T13:00:00Z", 1, map[string]any{"arm": "control"}),
	}
	telemetry := &utilityTelemetry{limit: len(rows), observations: rows}
	telemetry.reindexLocked()
	selected := telemetry.rowsForOutcomeIDs(utilityQuery{
		Project: "contextlattice", TaskClass: "coding", To: time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC), Limit: 8,
	}, map[string]struct{}{"prior-treatment": {}, "future-treatment": {}, "invalid-treatment": {}})
	if len(selected) != 1 || anyToString(selected[0]["outcome_id"]) != "prior-treatment" {
		t.Fatalf("historical Utility projection retained future, invalid, or future control rows: %#v", selected)
	}
}

func TestContinuousCognitionLifecycleAdviceIsReadOnlyAndBounded(t *testing.T) {
	request := continuousCognitionTestRequest(t)
	request.Operation = continuousCognitionOperationRollback
	activation := continuousCognitionActivation{State: "ready", PrepID: "prep_opaque", Persisted: true}
	governance, _ := projectContinuousCognitionGovernance(nil, request, agentProofTimelineSnapshot{}, continuousCognitionTestObservation(), activation, request.WorkspaceRef)
	if governance.Rollback.State != "recommended" || governance.Rollback.TargetRef != activation.PrepID {
		t.Fatalf("nonterminal one-shot preparation did not receive bounded rollback advice: %#v", governance.Rollback)
	}

	request.Operation = continuousCognitionOperationRetire
	observation := continuousCognitionTestObservation()
	observation.ObjectiveTerminal = true
	governance, _ = projectContinuousCognitionGovernance(nil, request, agentProofTimelineSnapshot{}, observation, continuousCognitionActivation{}, request.WorkspaceRef)
	if governance.Retirement.State != "recommended" || governance.Retirement.TargetRef != observation.ObjectiveGraphRef {
		t.Fatalf("terminal objective did not receive bounded retirement advice: %#v", governance.Retirement)
	}
}
