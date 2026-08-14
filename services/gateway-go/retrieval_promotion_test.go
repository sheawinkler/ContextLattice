package main

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRetrievalPromotionStrictIntRejectsUnsignedAndFloatOverflow(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	cases := []struct {
		name  string
		value any
	}{
		{name: "uint64 above max int64", value: uint64(math.MaxInt64) + 1},
		{name: "max uint64", value: uint64(math.MaxUint64)},
		{name: "uint above platform max int", value: uint(maxInt) + 1},
		{name: "float64 two to the sixty-third", value: math.Ldexp(1, 63)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if parsed, ok := retrievalPromotionStrictInt(testCase.value); ok {
				t.Fatalf("overflow value was accepted as %d", parsed)
			}
		})
	}
	if parsed, ok := retrievalPromotionStrictInt(uint(maxInt)); !ok || parsed != maxInt {
		t.Fatalf("platform max int boundary was rejected: parsed=%d ok=%v", parsed, ok)
	}
	if parsed, ok := retrievalPromotionStrictInt(float64(42)); !ok || parsed != 42 {
		t.Fatalf("exact float integer was rejected: parsed=%d ok=%v", parsed, ok)
	}
}

func mustJSONForRetrievalPromotion(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal test JSON: %v", err)
	}
	return raw
}

func retrievalPromotionTestRef(seed string) string {
	return "sha256:" + sha256Hex("retrieval-promotion-test\x00"+seed)
}

func retrievalPromotionTestCandidate(seed string, occurrence int, protected bool) retrievalPromotionCandidateInput {
	return retrievalPromotionCandidateInput{
		CandidateRef: "rtc_" + strings.Repeat(seed, 24)[:24],
		Occurrence:   occurrence, Protected: protected, Relevant: true,
	}
}

func retrievalPromotionTestCanaryReceipt(now time.Time, operation string, generation uint64, previous, assignment string) retrievalPromotionCanaryReceipt {
	return retrievalPromotionCanaryReceipt{
		SchemaID: retrievalPromotionCanaryReceiptSchemaID, Version: 1, Operation: operation, Generation: generation,
		WorkspaceRef: contextPackLearnedScopeRef("workspace", "workspace"), ProjectRef: contextPackLearnedScopeRef("project", "project"),
		TaskClassRef: contextPackLearnedScopeRef("task_class", "task"), RetrievalIntentRef: contextPackLearnedScopeRef("retrieval_intent", "intent"),
		PolicyRef: retrievalPromotionTestRef("policy"), SnapshotRef: retrievalPromotionTestRef("snapshot"), CaseSetRef: retrievalPromotionTestRef("case-set"),
		AssignmentSubjectRef: contextPackLearnedScopeRef("assignment_subject", assignment),
		ShadowBasisPoints:    0, ControlBasisPoints: 9500, CanaryBasisPoints: 500,
		MinimumCanarySamples: retrievalPromotionCanaryMinimumSamples, MinimumDwellSeconds: int(retrievalPromotionCanaryMinimumDwell / time.Second),
		RecordedAt: now.Format(time.RFC3339Nano), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339Nano), PreviousHash: previous,
		IdempotencyKey: "test-" + operation + "-" + strconv.FormatUint(generation, 10),
	}
}

func TestRetrievalPromotionStableCohortUsesOpaqueInputsAndExplicitWeights(t *testing.T) {
	weights := retrievalPromotionCohortWeights{ControlBasisPoints: 9500, CanaryBasisPoints: 500}
	first := retrievalPromotionStableCohort("opaque-subject", retrievalPromotionTestRef("snapshot"), retrievalPromotionTestRef("policy"), weights)
	second := retrievalPromotionStableCohort("opaque-subject", retrievalPromotionTestRef("snapshot"), retrievalPromotionTestRef("policy"), weights)
	if anyToString(first["arm"]) != anyToString(second["arm"]) || anyToInt(first["bucket_basis_points"], -1) != anyToInt(second["bucket_basis_points"], -1) {
		t.Fatalf("stable cohort assignment drifted: first=%#v second=%#v", first, second)
	}
	if anyToInt(anyMap(first["weights"])["control_basis_points"], 0)+anyToInt(anyMap(first["weights"])["canary_basis_points"], 0) != retrievalPromotionTotalBasisPoints {
		t.Fatalf("cohort did not retain explicit 100%% weights: %#v", first)
	}
	changed := retrievalPromotionStableCohort("opaque-subject", retrievalPromotionTestRef("snapshot"), retrievalPromotionTestRef("different-policy"), weights)
	if anyToInt(changed["bucket_basis_points"], -1) == anyToInt(first["bucket_basis_points"], -1) && anyToString(changed["arm"]) == anyToString(first["arm"]) {
		// A collision is mathematically possible but should be vanishingly rare;
		// use a second policy seed to keep the test deterministic and meaningful.
		changed = retrievalPromotionStableCohort("opaque-subject", retrievalPromotionTestRef("snapshot"), retrievalPromotionTestRef("different-policy-2"), weights)
	}
	if anyToString(changed["snapshot_ref"]) != retrievalPromotionTestRef("snapshot") {
		t.Fatalf("cohort receipt did not bind the exact snapshot: %#v", changed)
	}
}

func TestRetrievalPromotionCanonicalAssignmentSubjectRefIsSharedByCohortAndReceipt(t *testing.T) {
	weights := retrievalPromotionCohortWeights{ControlBasisPoints: 9500, CanaryBasisPoints: 500}
	raw := "opaque-subject-canonical"
	canonical := retrievalPromotionAssignmentSubjectRef(raw)
	fromRaw := retrievalPromotionStableCohort(raw, retrievalPromotionTestRef("snapshot"), retrievalPromotionTestRef("policy"), weights)
	fromCanonical := retrievalPromotionStableCohort(canonical, retrievalPromotionTestRef("snapshot"), retrievalPromotionTestRef("policy"), weights)
	if canonical == "" || anyToString(fromRaw["subject_ref"]) != canonical || anyToString(fromCanonical["subject_ref"]) != canonical ||
		anyToInt(fromRaw["bucket_basis_points"], -1) != anyToInt(fromCanonical["bucket_basis_points"], -2) || anyToString(fromRaw["arm"]) != anyToString(fromCanonical["arm"]) {
		t.Fatalf("cohort and authorization did not share canonical assignment subject: raw=%#v canonical=%#v", fromRaw, fromCanonical)
	}
}

func TestRetrievalPromotionEnvelopeUsesSignedCohortSnapshotAsFinalAuthority(t *testing.T) {
	now := time.Date(2026, time.August, 10, 20, 0, 0, 0, time.UTC)
	assignment := "signed-envelope-subject"
	receipt := retrievalPromotionTestCanaryReceipt(now, "configure", 1, "", assignment)
	weights := retrievalPromotionCohortWeights{ControlBasisPoints: 9500, CanaryBasisPoints: 500}
	for index := 0; index < 10000; index++ {
		candidate := assignment + "-" + strconv.Itoa(index)
		cohort := retrievalPromotionStableCohort(candidate, receipt.SnapshotRef, receipt.PolicyRef, weights)
		if anyToString(cohort["arm"]) == "canary" {
			assignment = candidate
			receipt.AssignmentSubjectRef = retrievalPromotionAssignmentSubjectRef(assignment)
			break
		}
	}
	decision := contextPackLearnedActivationDecision{
		Armed: true, Eligible: true, AssignedTreatment: true, Performed: true, Arm: "canary", Reason: "signed_canary_verified",
		CanaryPercent: 5, RequestRef: receipt.AssignmentSubjectRef, AssignmentSubjectRef: receipt.AssignmentSubjectRef,
		ProjectScopeRef: receipt.ProjectRef, TaskClassScopeRef: receipt.TaskClassRef, RetrievalIntentScopeRef: receipt.RetrievalIntentRef,
		WorkspaceRef: receipt.WorkspaceRef, PolicyRef: receipt.PolicyRef, ImpactProofRef: retrievalPromotionTestRef("impact"),
		SnapshotRef: receipt.SnapshotRef, ActuatorComparatorRef: retrievalPromotionTestRef("actuator"), ReputationSnapshotRef: retrievalPromotionTestRef("reputation"),
		CandidateMultipliers: map[string]float64{}, ExposureBucket: 123,
	}
	cohort := retrievalPromotionStableCohort(receipt.AssignmentSubjectRef, receipt.SnapshotRef, receipt.PolicyRef, weights)
	receipt.ReceiptDigest = retrievalPromotionTestRef("receipt")
	cohort["receipt_digest"] = receipt.ReceiptDigest
	cohort["case_set_ref"] = receipt.CaseSetRef
	decision.Promotion = map[string]any{
		"promotion_eligible": true, "cohort": cohort, "metric_guard": map[string]any{"pass": true},
		"canary_receipt_digest": cohort["receipt_digest"], "canary_receipt": retrievalPromotionCanaryReceiptSnapshot(receipt),
	}
	decision.PromotionLease = &retrievalPromotionExposureLease{receipt: receipt}
	items := []contextPackEvidenceItem{{CandidateID: "rtc_" + strings.Repeat("a", 24), Occurrence: 1, Kind: "fact"}}
	// Rebuild the envelope with the exact signed decision while keeping the
	// candidate union bounded and presentation-only.
	envelope := retrievalPromotionBuildContextEnvelope(retrievalTrustResult{Eligible: items}, items, items, nil, items, items, nil, contextPackTokenBudget{}, nil, decision)
	if got := retrievalPromotionNormalizeReceipt(envelope); got == nil {
		t.Fatalf("signed cohort snapshot did not survive promotion normalization: %#v", envelope)
	}
}

func TestRetrievalPromotionLossGuardRetainsProtectedAndRelevantUnion(t *testing.T) {
	protected := retrievalPromotionTestCandidate("a", 1, true)
	relevant := retrievalPromotionTestCandidate("b", 1, false)
	control := []retrievalPromotionCandidateInput{protected, relevant}
	pass := retrievalPromotionLossGuard(control, []retrievalPromotionCandidateInput{relevant, protected})
	if !anyToBool(pass["pass"]) || len(contextPackAnyList(pass["missing_identities"])) != 0 {
		t.Fatalf("equivalent reordered union failed loss guard: %#v", pass)
	}
	fail := retrievalPromotionLossGuard(control, []retrievalPromotionCandidateInput{protected})
	if anyToBool(fail["pass"]) || len(fail["missing_identities"].([]string)) != 1 || len(fail["relevant_missing_identities"].([]string)) != 1 {
		t.Fatalf("relevant union loss was not fail-closed: %#v", fail)
	}
}

func TestRetrievalPromotionContextEnvelopeSeparatesPolicyExclusionFromPresentationOmission(t *testing.T) {
	items := []contextPackEvidenceItem{
		{CandidateID: "rtc_" + strings.Repeat("a", 24), Occurrence: 1, Kind: "decision", Score: 90, EstimatedTokens: 12},
		{CandidateID: "rtc_" + strings.Repeat("b", 24), Occurrence: 2, Kind: "fact", Score: 70, EstimatedTokens: 12},
		{CandidateID: "rtc_" + strings.Repeat("c", 24), Occurrence: 3, Kind: "memory", Score: 60, EstimatedTokens: 12},
	}
	trust := retrievalTrustResult{
		Eligible: items, CandidateCount: 4, ProcessedCandidateCount: 3,
		PreDecisions:  []retrievalDecisionRecord{{CandidateID: "rtc_" + strings.Repeat("d", 24), Occurrence: 4, Decision: "quarantined", Reasons: []string{"high_impact_instruction"}}},
		TrustEnvelope: map[string]any{"input_boundary": map[string]any{"continuation_durable": true, "continuation_available": true, "continuation_token": "opaque-continuation", "omitted_count": 1}},
	}
	nativeSelected := []contextPackEvidenceItem{items[0], items[1]}
	nativeOmitted := []contextPackEvidenceItem{items[2]}
	treatmentEligible := []contextPackEvidenceItem{items[1], items[0], items[2]}
	treatmentSelected := []contextPackEvidenceItem{items[1], items[0]}
	treatmentOmitted := []contextPackEvidenceItem{items[2]}
	envelope := retrievalPromotionBuildContextEnvelope(trust, items, nativeSelected, nativeOmitted, treatmentEligible, treatmentSelected, treatmentOmitted, contextPackTokenBudget{Active: true}, nil, contextPackLearnedActivationDecision{})
	union := anyMap(envelope["candidate_union"])
	if anyToInt(union["safe_count"], 0) != 3 || anyToInt(union["hard_excluded_count"], 0) != 1 {
		t.Fatalf("envelope lost safe or hard-excluded identity: %#v", envelope)
	}
	if !anyToBool(anyMap(envelope["loss_guard"])["pass"]) || anyToInt(anyMap(envelope["omission_receipt"])["presentation_omitted_count"], 0) != 1 {
		t.Fatalf("envelope did not distinguish union preservation from presentation omission: %#v", envelope)
	}
	hard := contextPackAnyList(union["hard_exclusions"])
	if anyToString(anyMap(hard[0])["exclusion_class"]) != "quarantine" {
		t.Fatalf("hard quarantine exclusion was not classified authoritatively: %#v", hard)
	}
	serialized := string(mustJSONForRetrievalPromotion(t, envelope))
	for _, forbidden := range []string{"opaque-continuation"} {
		// Continuation tokens are opaque receipt state and are allowed; raw
		// candidate text/path would not be. Keep this assertion as a shape check.
		if !strings.Contains(serialized, forbidden) {
			t.Fatalf("opaque continuation binding was lost: %s", serialized)
		}
	}
	if normalized := retrievalPromotionNormalizeReceipt(envelope); len(normalized) == 0 {
		t.Fatal("valid context promotion envelope did not survive receipt normalization")
	}
}

func TestRetrievalPromotionSelectionReceiptCarriesSanitizedUnion(t *testing.T) {
	items := []contextPackEvidenceItem{{CandidateID: "rtc_" + strings.Repeat("e", 24), Occurrence: 1, Kind: "fact", Score: 50}}
	trust := retrievalTrustResult{Eligible: items, TrustEnvelope: map[string]any{"input_boundary": map[string]any{}}}
	envelope := retrievalPromotionBuildContextEnvelope(trust, items, items, nil, items, items, nil, contextPackTokenBudget{}, nil, contextPackLearnedActivationDecision{})
	receipt := contextPackSelectionReceiptWithActivation(renderSelectionReceiptRankedRefs(items), nil, nil, envelope)
	if len(receipt) == 0 {
		t.Fatal("selection receipt rejected valid promotion envelope")
	}
	parsed := contextPackSelectionReceiptFromSample(receipt)
	if len(parsed) == 0 || len(anyMap(parsed["retrieval_promotion"])) == 0 {
		t.Fatalf("promotion envelope did not remain receipt-bound: receipt=%#v parsed=%#v", receipt, parsed)
	}
	if strings.Contains(string(mustJSONForRetrievalPromotion(t, parsed)), "raw candidate text") {
		t.Fatal("selection receipt retained raw candidate content")
	}
}

func TestRetrievalPromotionSearchEnvelopeExcludesQuarantineFromSafeOrders(t *testing.T) {
	trusted := map[string]any{
		"content": "trusted candidate", "project": "project", "file": "trusted.md",
		"gateway_trust_assessment": searchIntelligenceGatewayTrustEnvelope{trustLabel: "bounded"},
	}
	quarantined := map[string]any{
		"content": "quarantined candidate", "project": "project", "file": "quarantine.md",
		"gateway_trust_assessment": searchIntelligenceGatewayTrustEnvelope{trustLabel: "quarantined", quarantined: true},
	}
	unverified := map[string]any{"content": "unverified native candidate", "project": "project", "file": "unverified.md"}
	envelope := retrievalPromotionSearchEnvelope(searchIntelligenceInput{
		AllMerged: []map[string]any{trusted, quarantined, unverified},
		Literal:   []map[string]any{trusted, quarantined, unverified},
	})
	union := anyMap(envelope["candidate_union"])
	if anyToInt(union["safe_count"], 0) != 2 || anyToInt(union["hard_excluded_count"], 0) != 1 {
		t.Fatalf("search promotion envelope did not preserve the bounded union split: %#v", envelope)
	}
	if anyToBool(envelope["safe_union_verified"]) || anyToString(envelope["safe_union_status"]) != "policy_safety_unverified" {
		t.Fatalf("unverified AllMerged rows were labeled post-policy-safe: %#v", envelope)
	}
	if normalized := retrievalPromotionNormalizeReceipt(envelope); len(normalized) == 0 {
		t.Fatalf("search promotion envelope did not satisfy the durable receipt shape: %#v", envelope)
	}
	quarantineRef := searchIntelligenceCandidateIdentity(quarantined).CandidateRef
	for _, orderName := range []string{"native_order", "control_order", "selected_order", "omitted_order"} {
		for _, raw := range contextPackAnyList(anyMap(envelope["ordering"])[orderName]) {
			if anyToString(anyMap(raw)["candidate_ref"]) == quarantineRef {
				t.Fatalf("quarantined candidate entered %s: %#v", orderName, envelope)
			}
		}
	}
	for _, raw := range contextPackAnyList(union["hard_exclusions"]) {
		if anyToString(anyMap(raw)["candidate_ref"]) != quarantineRef || anyToString(anyMap(raw)["exclusion_class"]) != "quarantine" {
			t.Fatalf("quarantine was not retained as an authoritative hard exclusion: %#v", raw)
		}
	}
}

func TestRetrievalPromotionGovernanceRouteConfiguresReadbackRollsBackAndRejectsStaleGeneration(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "retrieval-promotion-canary.ndjson")
	t.Setenv(retrievalPromotionCanaryLedgerPathEnv, ledgerPath)
	identity, err := loadOrCreateContextIdentity(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatalf("create route signer identity: %v", err)
	}
	s := &server{contextMesh: &contextMeshStore{identity: identity}}
	previousAuthorization := optionalRetrievalPromotionGovernanceAuthorization
	optionalRetrievalPromotionGovernanceAuthorization = func(_ *server, _ http.ResponseWriter, _ *http.Request, _ map[string]any, _, _ string) (string, map[string]any, bool) {
		return "workspace_t4_public_test", map[string]any{"runtime_license_subject": "route-test-operator"}, true
	}
	t.Cleanup(func() { optionalRetrievalPromotionGovernanceAuthorization = previousAuthorization })
	path := retrievalPromotionCanaryGovernancePath
	actor := "route-test-operator"
	expiresAt := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339Nano)
	payload := map[string]any{
		"operation": "configure", "actor": actor, "operator_approved": true, "expected_generation": 0,
		"idempotency_key": "route-configure-1", "reason": "route lifecycle test", "project": "project",
		"task_class_ref":       contextPackLearnedScopeRef("task_class", "task"),
		"retrieval_intent_ref": contextPackLearnedScopeRef("retrieval_intent", "intent"),
		"policy_ref":           retrievalPromotionTestRef("policy"), "snapshot_ref": retrievalPromotionTestRef("snapshot"),
		"case_set_ref": retrievalPromotionTestRef("case-set"), "assignment_subject_ref": contextPackLearnedScopeRef("assignment_subject", "opaque-agent-subject"),
		"shadow_basis_points": 0, "control_basis_points": 9500, "canary_basis_points": 500,
		"minimum_canary_samples": retrievalPromotionCanaryMinimumSamples, "minimum_dwell_seconds": int(retrievalPromotionCanaryMinimumDwell / time.Second),
		"expires_at": expiresAt,
	}
	call := func(method string, requestPayload map[string]any) (int, map[string]any) {
		t.Helper()
		body := bytes.NewReader(nil)
		if requestPayload != nil {
			raw, err := json.Marshal(requestPayload)
			if err != nil {
				t.Fatalf("marshal governance request: %v", err)
			}
			body = bytes.NewReader(raw)
		}
		request := httptest.NewRequest(method, path, body)
		response := httptest.NewRecorder()
		s.retrievalPromotionCanaryGovernance(response, request)
		decoded := map[string]any{}
		if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("decode governance response: %v body=%s", err, response.Body.String())
		}
		return response.Code, decoded
	}
	status, configured := call(http.MethodPost, payload)
	if status != http.StatusOK || !anyToBool(configured["active"]) || anyToString(configured["schema_id"]) != retrievalPromotionCanaryGovernanceContractID {
		t.Fatalf("server-owned configure route did not activate bounded canary: status=%d response=%#v", status, configured)
	}
	if _, ok := configured["format_contract"].(map[string]any); !ok {
		t.Fatalf("configure response omitted format contract: %#v", configured)
	}
	if anyToString(anyMap(configured["receipt"])["operation"]) != "configure" {
		t.Fatalf("configure response omitted signed receipt: %#v", configured)
	}
	status, replayed := call(http.MethodPost, payload)
	if status != http.StatusOK || anyToString(anyMap(replayed["receipt"])["receipt_digest"]) != anyToString(anyMap(configured["receipt"])["receipt_digest"]) {
		t.Fatalf("exact stale-generation retry did not replay the original signed result: status=%d response=%#v", status, replayed)
	}
	conflictingRetry := cloneJSONMap(payload)
	conflictingRetry["canary_basis_points"] = 400
	status, conflict := call(http.MethodPost, conflictingRetry)
	if status != http.StatusConflict || anyToString(conflict["error"]) != "canary_idempotency_conflict" {
		t.Fatalf("conflicting idempotency reuse did not fail before stale generation: status=%d response=%#v", status, conflict)
	}
	status, readback := call(http.MethodGet, nil)
	if status != http.StatusOK || !anyToBool(readback["active"]) || anyToString(anyMap(readback["readback_receipt"])["operation"]) != "readback" {
		t.Fatalf("readback route did not return exact signed active state: status=%d response=%#v", status, readback)
	}
	stale := cloneJSONMap(payload)
	stale["idempotency_key"] = "route-stale-2"
	status, staleResponse := call(http.MethodPost, stale)
	if status != http.StatusConflict || anyToString(staleResponse["error"]) != "stale_canary_generation" {
		t.Fatalf("stale expected generation was not rejected: status=%d response=%#v", status, staleResponse)
	}
	rollback := cloneJSONMap(payload)
	rollback["operation"] = "rollback"
	rollback["expected_generation"] = 1
	rollback["idempotency_key"] = "route-rollback-2"
	delete(rollback, "shadow_basis_points")
	delete(rollback, "control_basis_points")
	delete(rollback, "canary_basis_points")
	delete(rollback, "minimum_canary_samples")
	delete(rollback, "minimum_dwell_seconds")
	delete(rollback, "expires_at")
	status, rolledBack := call(http.MethodPost, rollback)
	if status != http.StatusOK || anyToBool(rolledBack["active"]) || anyToString(anyMap(rolledBack["receipt"])["operation"]) != "rollback" {
		t.Fatalf("rollback route did not durably dominate active state: status=%d response=%#v", status, rolledBack)
	}
	status, afterRestart := call(http.MethodGet, nil)
	if status != http.StatusOK || anyToBool(afterRestart["active"]) || anyToString(afterRestart["reason"]) != "canary_rolled_back" || !anyToBool(afterRestart["head_verified"]) {
		t.Fatalf("rollback dominance did not survive route readback: status=%d response=%#v", status, afterRestart)
	}
	status, replayAfterRollback := call(http.MethodPost, payload)
	if status != http.StatusOK || !anyToBool(replayAfterRollback["replayed"]) || anyToBool(replayAfterRollback["active"]) ||
		anyToBool(replayAfterRollback["current_active"]) || anyToInt(replayAfterRollback["current_generation"], 0) != 2 ||
		anyToString(replayAfterRollback["current_head_digest"]) != anyToString(anyMap(rolledBack["receipt"])["receipt_digest"]) ||
		!anyToBool(replayAfterRollback["current_head_verified"]) || !anyToBool(replayAfterRollback["as_of_active"]) ||
		anyToInt(replayAfterRollback["as_of_generation"], 0) != 1 ||
		anyToString(replayAfterRollback["as_of_head_digest"]) != anyToString(anyMap(configured["receipt"])["receipt_digest"]) {
		t.Fatalf("historical configure replay claimed stale active state or lost as-of/current semantics: status=%d response=%#v", status, replayAfterRollback)
	}
}

func TestRetrievalPromotionCanaryReceiptSignsAndVerifiesConfigureReadbackRollback(t *testing.T) {
	identity, err := loadOrCreateContextIdentity(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatalf("create test identity: %v", err)
	}
	now := time.Date(2026, time.August, 10, 20, 0, 0, 0, time.UTC)
	receipt := retrievalPromotionCanaryReceipt{
		SchemaID: retrievalPromotionCanaryReceiptSchemaID, Version: 1, Operation: "configure", Generation: 1,
		WorkspaceRef: retrievalPromotionTestRef("workspace"), ProjectRef: retrievalPromotionTestRef("project"),
		TaskClassRef: retrievalPromotionTestRef("task"), RetrievalIntentRef: retrievalPromotionTestRef("intent"),
		PolicyRef: retrievalPromotionTestRef("policy"), SnapshotRef: retrievalPromotionTestRef("snapshot"), CaseSetRef: retrievalPromotionTestRef("case-set"),
		AssignmentSubjectRef: contextPackLearnedScopeRef("assignment_subject", "opaque-subject"),
		ControlBasisPoints:   9500, CanaryBasisPoints: 500, MinimumCanarySamples: retrievalPromotionCanaryMinimumSamples,
		MinimumDwellSeconds: int(retrievalPromotionCanaryMinimumDwell / time.Second), RecordedAt: now.Format(time.RFC3339Nano),
		ExpiresAt: now.Add(time.Hour).Format(time.RFC3339Nano),
	}
	if err := retrievalPromotionSignCanaryReceipt(&receipt, identity); err != nil || !retrievalPromotionVerifyCanaryReceipt(receipt, now, identity) {
		t.Fatalf("signed configure receipt did not verify: receipt=%#v err=%v", receipt, err)
	}
	receipt.Operation = "readback"
	receipt.ReceiptDigest = ""
	if err := retrievalPromotionSignCanaryReceipt(&receipt, identity); err != nil || !retrievalPromotionVerifyCanaryReceipt(receipt, now, identity) {
		t.Fatalf("signed readback receipt did not verify: receipt=%#v err=%v", receipt, err)
	}
	receipt.Operation = "rollback"
	receipt.ControlBasisPoints = retrievalPromotionTotalBasisPoints
	receipt.CanaryBasisPoints = 0
	receipt.ReceiptDigest = ""
	if err := retrievalPromotionSignCanaryReceipt(&receipt, identity); err != nil || !retrievalPromotionVerifyCanaryReceipt(receipt, now, identity) {
		t.Fatalf("signed rollback receipt did not verify: receipt=%#v err=%v", receipt, err)
	}
	serialized := mustJSONForRetrievalPromotion(t, receipt)
	if strings.Contains(string(serialized), identity.SigningPrivateKey) {
		t.Fatal("canary receipt leaked signing private key")
	}
}

func TestRetrievalPromotionMetricGuardRollsBackRepairAndLatencyRegression(t *testing.T) {
	metrics := func(repair, latency float64) map[string]any {
		return map[string]any{
			"decision_impact_recall_at_5": 0.9, "mrr": 0.9, "numeric_exactness": 1.0,
			"citation_coverage": 1.0, "citation_exactness": 1.0, "quality_score": 90.0,
			"p95_latency_ms": latency, "repair_required_rate": repair,
		}
	}
	guard := retrievalPromotionMetricGuard(map[string]any{"baseline": metrics(0.20, 10), "shadow": metrics(0.25, 12)}, map[string]any{})
	if anyToBool(guard["pass"]) || !anyToBool(guard["rollback"]) || !strings.Contains(anyToString(guard["reason"]), "latency_regression") {
		t.Fatalf("metric guard did not trigger automatic rollback: %#v", guard)
	}
}

func TestRetrievalPromotionCanaryLedgerDurableHeadAndRollbackDominance(t *testing.T) {
	identity, err := loadOrCreateContextIdentity(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatalf("create test identity: %v", err)
	}
	path := filepath.Join(t.TempDir(), "promotion.ndjson")
	t.Setenv(retrievalPromotionCanaryLedgerPathEnv, path)
	now := time.Date(2026, time.August, 10, 20, 0, 0, 0, time.UTC)
	store, err := newRetrievalPromotionCanaryLedger(path, identity)
	if err != nil {
		t.Fatalf("create canary ledger: %v", err)
	}
	configure := retrievalPromotionTestCanaryReceipt(now, "configure", 1, "", "opaque-agent-subject")
	if err := retrievalPromotionSignCanaryReceipt(&configure, identity); err != nil {
		t.Fatalf("sign configure: %v", err)
	}
	if err := store.configure(configure, now); err != nil {
		t.Fatalf("append configure: %v", err)
	}
	store.close()
	store, err = newRetrievalPromotionCanaryLedger(path, identity)
	if err != nil {
		t.Fatalf("reload configure ledger: %v", err)
	}
	active, activeOK, reason := store.activeReceipt(now)
	if !activeOK || active.ReceiptDigest != configure.ReceiptDigest {
		t.Fatalf("durable active head missing: active=%#v ok=%v reason=%s", active, activeOK, reason)
	}
	readback, err := store.signedReadback(now)
	if err != nil || readback.Operation != "readback" || !retrievalPromotionVerifyCanaryReceipt(readback, now, identity) {
		t.Fatalf("signed readback failed: receipt=%#v err=%v", readback, err)
	}
	rollback := configure
	rollback.Operation = "rollback"
	rollback.Generation = 2
	rollback.PreviousHash = configure.ReceiptDigest
	rollback.ControlBasisPoints = retrievalPromotionTotalBasisPoints
	rollback.CanaryBasisPoints = 0
	rollback.RecordedAt = now.Add(time.Minute).Format(time.RFC3339Nano)
	rollback.ExpiresAt = now.Add(time.Hour).Format(time.RFC3339Nano)
	rollback.IdempotencyKey = "test-rollback"
	rollback.ReasonDigest = "sha256:" + sha256Hex("regression")
	rollback.ReceiptDigest = ""
	rollback.Issuer = contextPassportIssuer{}
	rollback.Signature = contextPassportSignature{}
	if err := retrievalPromotionSignCanaryReceipt(&rollback, identity); err != nil {
		t.Fatalf("sign rollback: %v", err)
	}
	if err := store.rollback(rollback, now.Add(time.Minute)); err != nil {
		t.Fatalf("append rollback: %v", err)
	}
	store.close()
	store, err = newRetrievalPromotionCanaryLedger(path, identity)
	if err != nil {
		t.Fatalf("reload rollback ledger: %v", err)
	}
	defer store.close()
	if _, activeOK, reason = store.activeReceipt(now.Add(2 * time.Minute)); activeOK || reason != "canary_rolled_back" {
		t.Fatalf("rollback did not dominate active state: ok=%v reason=%s", activeOK, reason)
	}
	store.close()
	if _, configured, configuredReason := retrievalPromotionConfiguredCanaryReceiptFromLedger(identity, now.Add(2*time.Minute)); configured || configuredReason != "canary_rolled_back" {
		t.Fatalf("rollback did not survive configured readback: configured=%v reason=%s", configured, configuredReason)
	}
}

func TestRetrievalPromotionRejectsSelfSignedAndReplayOrStaleReceipts(t *testing.T) {
	trusted, err := loadOrCreateContextIdentity(filepath.Join(t.TempDir(), "trusted.json"))
	if err != nil {
		t.Fatalf("create trusted identity: %v", err)
	}
	attacker, err := loadOrCreateContextIdentity(filepath.Join(t.TempDir(), "attacker.json"))
	if err != nil {
		t.Fatalf("create attacker identity: %v", err)
	}
	now := time.Date(2026, time.August, 10, 20, 0, 0, 0, time.UTC)
	forged := retrievalPromotionTestCanaryReceipt(now, "configure", 1, "", "opaque-agent-subject")
	if err := retrievalPromotionSignCanaryReceipt(&forged, attacker); err != nil {
		t.Fatalf("sign forged receipt: %v", err)
	}
	if retrievalPromotionVerifyCanaryReceipt(forged, now, trusted) || retrievalPromotionVerifyCanaryReceipt(forged, now) {
		t.Fatal("self-signed receipt was accepted without the server-owned signer")
	}
	path := filepath.Join(t.TempDir(), "replay.ndjson")
	store, err := newRetrievalPromotionCanaryLedger(path, trusted)
	if err != nil {
		t.Fatalf("create replay ledger: %v", err)
	}
	first := retrievalPromotionTestCanaryReceipt(now, "configure", 1, "", "opaque-agent-subject")
	if err := retrievalPromotionSignCanaryReceipt(&first, trusted); err != nil {
		t.Fatalf("sign first receipt: %v", err)
	}
	if err := store.configure(first, now); err != nil {
		t.Fatalf("append first receipt: %v", err)
	}
	replayed := first
	replayed.IdempotencyKey = "different-replay"
	if err := store.configure(replayed, now); err == nil {
		t.Fatal("replayed generation was accepted")
	}
	stale := retrievalPromotionTestCanaryReceipt(now, "configure", 3, "sha256:"+strings.Repeat("a", 64), "opaque-agent-subject")
	if err := retrievalPromotionSignCanaryReceipt(&stale, trusted); err != nil {
		t.Fatalf("sign stale receipt: %v", err)
	}
	if err := store.configure(stale, now); err == nil {
		t.Fatal("stale generation was accepted")
	}
	store.close()
}

func retrievalPromotionCompleteImpact(now time.Time, receipt retrievalPromotionCanaryReceipt) map[string]any {
	proofGates := map[string]any{}
	for _, name := range []string{"comparative_shadow", "receipt_ledger_durability", "train_holdout_minimums", "negative_retention", "independent_verifiers", "exact_denominators", "causal_interval", "outcome_regressions_absent", "outcome_identity_consistency"} {
		proofGates[name] = map[string]any{"pass": true}
	}
	return map[string]any{
		"schema_id": searchImpactIntelligenceContractID, "canary_eligible": true, "proof_gates": proofGates,
		"activation_evidence": map[string]any{
			"same_snapshot": true, "time_split": "chronological_80_20", "snapshot_ref": receipt.SnapshotRef, "case_set_ref": receipt.CaseSetRef,
			"holdout_count": 60, "train_count": 300, "canary_sample_count": receipt.MinimumCanarySamples,
			"canary_started_at": now.Add(-time.Duration(receipt.MinimumDwellSeconds) * time.Second).Format(time.RFC3339Nano),
			"execution_receipt": map[string]any{"complete": true, "exact_zero": true, "provider_calls": 0, "provider_tokens": 0, "provider_cost": 0, "external_network_calls": 0},
			"sample_sufficiency": map[string]any{
				"schema_id": "contextlattice_search_impact_sample_sufficiency.v1", "version": 1,
				"source": "server_reconciled_canary_outcomes", "method": "exact_count_against_signed_authority_minimum_v1",
				"pass": true, "required_sample_count": receipt.MinimumCanarySamples, "observed_sample_count": receipt.MinimumCanarySamples,
				"promotion_authority": "trusted_signed_canary_ledger", "canary_receipt_digest": receipt.ReceiptDigest,
				"canary_generation": receipt.Generation, "statistical_power_available": false,
				"limits": "count_threshold_only_no_power_or_effect_size_estimate",
			},
		},
		"impact_intelligence": map[string]any{"utility_reconciliation": map[string]any{"causal_interval": map[string]any{"low": 0.1, "point": 0.2, "high": 0.3}}},
	}
}

func retrievalPromotionCompleteMetrics(latency float64) map[string]any {
	return map[string]any{"decision_impact_recall_at_5": 0.90, "mrr": 0.90, "numeric_exactness": 1.0, "citation_coverage": 0.90, "citation_exactness": 0.90, "quality_score": 90.0, "p95_latency_ms": latency, "repair_required_rate": 0.1}
}

func retrievalPromotionCompleteShadow(latency float64) map[string]any {
	shadow := searchImpactValidComparativeShadow()
	shadow["baseline"] = retrievalPromotionCompleteMetrics(10)
	shadow["shadow"] = retrievalPromotionCompleteMetrics(latency)
	for _, metrics := range []map[string]any{anyMap(shadow["baseline"]), anyMap(shadow["shadow"])} {
		metrics["safety_case_count"] = 2
		metrics["safety_failure_count"] = 0
		metrics["safety_failure_rate"] = 0.0
		metrics["effective_k_min"] = 5
		metrics["effective_k_max"] = 5
		metrics["sparse_candidate_case_count"] = 0
	}
	anyMap(shadow["baseline"])["decision_impact_ndcg_at_5"] = 0.80
	anyMap(shadow["shadow"])["decision_impact_ndcg_at_5"] = 0.90
	anyMap(shadow["baseline"])["decision_impact_recall_at_5"] = 0.80
	anyMap(shadow["shadow"])["decision_impact_recall_at_5"] = 0.90
	return shadow
}

func TestRetrievalPromotionMetricFloorsAndResourceReceiptFailClosed(t *testing.T) {
	receipt := retrievalPromotionTestCanaryReceipt(time.Now().UTC(), "configure", 1, "", "opaque-agent-subject")
	impact := retrievalPromotionCompleteImpact(time.Now().UTC(), receipt)
	shadow := map[string]any{"baseline": retrievalPromotionCompleteMetrics(10), "shadow": retrievalPromotionCompleteMetrics(10)}
	shadowMetrics := anyMap(shadow["shadow"])
	shadowMetrics["decision_impact_recall_at_5"] = 0.899
	guard := retrievalPromotionMetricGuard(shadow, impact)
	if anyToBool(guard["pass"]) || !strings.Contains(anyToString(guard["reason"]), "recall_floor") {
		t.Fatalf("89.9 recall did not fail absolute floor: %#v", guard)
	}
	shadowMetrics["decision_impact_recall_at_5"] = 0.90
	delete(anyMap(anyMap(impact["activation_evidence"])["execution_receipt"]), "provider_cost")
	guard = retrievalPromotionMetricGuard(shadow, impact)
	if anyToBool(guard["pass"]) || !strings.Contains(anyToString(guard["reason"]), "exact_zero_provider_network_receipt_missing") {
		t.Fatalf("missing exact cost receipt did not fail closed: %#v", guard)
	}
}

func TestRetrievalPromotionSampleSufficiencyUsesExactSignedMinimumWithoutPowerClaims(t *testing.T) {
	identity, err := loadOrCreateContextIdentity(filepath.Join(t.TempDir(), "sample-sufficiency-identity.json"))
	if err != nil {
		t.Fatalf("create sample-sufficiency identity: %v", err)
	}
	now := time.Now().UTC()
	receipt := retrievalPromotionTestCanaryReceipt(now, "configure", 1, "", "opaque-agent-subject")
	receipt.MinimumCanarySamples = 40
	if err := retrievalPromotionSignCanaryReceipt(&receipt, identity); err != nil {
		t.Fatalf("sign sample-sufficiency receipt: %v", err)
	}
	impact := retrievalPromotionCompleteImpact(now, receipt)
	pass, reason, sufficiency := retrievalPromotionSampleSufficiencyGate(impact, &receipt)
	if !pass || reason != "signed_authority_sample_sufficiency_verified" ||
		anyToInt(sufficiency["required_sample_count"], 0) != receipt.MinimumCanarySamples {
		t.Fatalf("exact signed sample minimum was not accepted: pass=%v reason=%s receipt=%#v", pass, reason, sufficiency)
	}
	for _, forbidden := range []string{"power", "statistical_power", "minimum_effect_size", "effect_size"} {
		if _, present := sufficiency[forbidden]; present {
			t.Fatalf("count-only sufficiency receipt contained %q: %#v", forbidden, sufficiency)
		}
	}

	wrongMinimum := cloneJSONMap(impact)
	anyMap(anyMap(wrongMinimum["activation_evidence"])["sample_sufficiency"])["required_sample_count"] = retrievalPromotionCanaryMinimumSamples
	if pass, reason, _ := retrievalPromotionSampleSufficiencyGate(wrongMinimum, &receipt); pass || reason != "sample_sufficiency_authority_mismatch" {
		t.Fatalf("different valid minimum escaped signed authority: pass=%v reason=%s", pass, reason)
	}

	insufficient := cloneJSONMap(impact)
	insufficientEvidence := anyMap(insufficient["activation_evidence"])
	gate := anyMap(insufficientEvidence["sample_sufficiency"])
	gate["observed_sample_count"] = receipt.MinimumCanarySamples - 1
	gate["pass"] = false
	insufficientEvidence["canary_sample_count"] = receipt.MinimumCanarySamples - 1
	if pass, reason, _ := retrievalPromotionSampleSufficiencyGate(insufficient, &receipt); pass || reason != "signed_authority_sample_minimum_not_met" {
		t.Fatalf("insufficient exact count passed: pass=%v reason=%s", pass, reason)
	}

	fabricatedPower := cloneJSONMap(impact)
	anyMap(anyMap(fabricatedPower["activation_evidence"])["sample_sufficiency"])["power"] = 0.99
	if pass, reason, _ := retrievalPromotionSampleSufficiencyGate(fabricatedPower, &receipt); pass || reason != "sample_sufficiency_contains_statistical_claim" {
		t.Fatalf("fabricated power claim survived count-only gate: pass=%v reason=%s", pass, reason)
	}

	unknownClaim := cloneJSONMap(impact)
	anyMap(anyMap(unknownClaim["activation_evidence"])["sample_sufficiency"])["estimated_power"] = 0.99
	if pass, reason, _ := retrievalPromotionSampleSufficiencyGate(unknownClaim, &receipt); pass || reason != "sample_sufficiency_contains_unknown_claim" {
		t.Fatalf("unknown statistical claim survived strict count-only schema: pass=%v reason=%s", pass, reason)
	}

	legacyPowerOnly := cloneJSONMap(impact)
	legacyEvidence := anyMap(legacyPowerOnly["activation_evidence"])
	delete(legacyEvidence, "sample_sufficiency")
	legacyEvidence["sample_power"] = map[string]any{"pass": true, "sample_count": 1, "power": 1.0}
	if pass, reason, _ := retrievalPromotionSampleSufficiencyGate(legacyPowerOnly, &receipt); pass || reason != "sample_sufficiency_missing_or_invalid" {
		t.Fatalf("legacy fabricated power authorized activation: pass=%v reason=%s", pass, reason)
	}
}

func TestRetrievalPromotionRejectsCallerSuppliedMetricEvidence(t *testing.T) {
	trusted, err := loadOrCreateContextIdentity(filepath.Join(t.TempDir(), "trusted.json"))
	if err != nil {
		t.Fatalf("create trusted identity: %v", err)
	}
	now := time.Now().UTC()
	assignment := "opaque-agent-subject"
	receipt := retrievalPromotionTestCanaryReceipt(now, "configure", 1, "", assignment)
	if err := retrievalPromotionSignCanaryReceipt(&receipt, trusted); err != nil {
		t.Fatalf("sign configure receipt: %v", err)
	}
	impact := retrievalPromotionCompleteImpact(now, receipt)
	callerMetrics := map[string]any{
		"baseline": map[string]any{"decision_impact_recall_at_5": 0.01},
		"shadow":   map[string]any{"decision_impact_recall_at_5": 1.0},
	}
	eligible, reason, evidence := retrievalPromotionAuthorizeActivation(retrievalPromotionActivationInput{
		Project: "project", TaskClass: "task", RetrievalIntent: "intent", WorkspaceRef: "workspace", PolicyRef: receipt.PolicyRef,
		Assignment: assignment, Impact: impact, Shadow: callerMetrics, Receipt: receipt, TrustedSigner: trusted,
		CanaryHeadDigest: receipt.ReceiptDigest, Now: now,
	})
	if eligible || reason != "comparative_shadow_receipt_invalid" || anyToBool(evidence["promotion_eligible"]) {
		t.Fatalf("caller-supplied metrics became activation authority: eligible=%v reason=%s evidence=%#v", eligible, reason, evidence)
	}
}

func TestRetrievalPromotionScopeMatchAcceptsServerCanonicalRefs(t *testing.T) {
	now := time.Now().UTC()
	receipt := retrievalPromotionTestCanaryReceipt(now, "configure", 1, "", "opaque-agent-subject")
	if !retrievalPromotionReceiptMatchesScope(receipt, "project", "task", "intent", "workspace", receipt.PolicyRef) {
		t.Fatalf("plain governance scope did not match canonical receipt refs: %#v", receipt)
	}
	if !retrievalPromotionReceiptMatchesScope(receipt, receipt.ProjectRef, receipt.TaskClassRef, receipt.RetrievalIntentRef, receipt.WorkspaceRef, receipt.PolicyRef) {
		t.Fatalf("server canonical scope refs did not match canonical receipt refs: %#v", receipt)
	}
}

func TestRetrievalPromotionNormalizeRejectsIdentitySwapRanksAndPartitions(t *testing.T) {
	items := []contextPackEvidenceItem{
		{CandidateID: "rtc_" + strings.Repeat("a", 24), Occurrence: 1, Kind: "fact"},
		{CandidateID: "rtc_" + strings.Repeat("b", 24), Occurrence: 1, Kind: "fact"},
	}
	envelope := retrievalPromotionBuildContextEnvelope(retrievalTrustResult{Eligible: items}, items, items, nil, []contextPackEvidenceItem{items[1], items[0]}, []contextPackEvidenceItem{items[1], items[0]}, nil, contextPackTokenBudget{}, nil, contextPackLearnedActivationDecision{})
	if retrievalPromotionNormalizeReceipt(envelope) == nil {
		t.Fatal("valid reordered union did not normalize")
	}
	swapped := cloneJSONMap(envelope)
	ordering := anyMap(swapped["ordering"])
	selected := contextPackAnyList(ordering["selected_order"])
	selected[0], selected[1] = selected[1], selected[0]
	ordering["selected_order"] = selected
	if retrievalPromotionNormalizeReceipt(swapped) != nil {
		t.Fatal("equal-length identity swap bypassed rank recomputation")
	}
	malformedRank := cloneJSONMap(envelope)
	native := contextPackAnyList(anyMap(malformedRank["ordering"])["native_order"])
	anyMap(native[0])["order"] = 2
	if retrievalPromotionNormalizeReceipt(malformedRank) != nil {
		t.Fatal("malformed sequential rank was accepted")
	}
	malformedCount := cloneJSONMap(envelope)
	anyMap(malformedCount["candidate_union"])["safe_count"] = 99
	if retrievalPromotionNormalizeReceipt(malformedCount) != nil {
		t.Fatal("malformed union count was accepted")
	}
}

func TestRetrievalPromotionMetricRegressionDurablyRollsBackActiveCanary(t *testing.T) {
	identity, err := loadOrCreateContextIdentity(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatalf("create test identity: %v", err)
	}
	path := filepath.Join(t.TempDir(), "automatic-rollback.ndjson")
	t.Setenv(retrievalPromotionCanaryLedgerPathEnv, path)
	now := time.Date(2026, time.August, 10, 20, 0, 0, 0, time.UTC)
	assignment := "opaque-agent-subject"
	receipt := retrievalPromotionTestCanaryReceipt(now.Add(-20*time.Minute), "configure", 1, "", assignment)
	if err := retrievalPromotionSignCanaryReceipt(&receipt, identity); err != nil {
		t.Fatalf("sign active receipt: %v", err)
	}
	store, err := newRetrievalPromotionCanaryLedger(path, identity)
	if err != nil {
		t.Fatalf("create rollback ledger: %v", err)
	}
	if err := store.configure(receipt, now.Add(-20*time.Minute)); err != nil {
		t.Fatalf("append active receipt: %v", err)
	}
	store.close()
	for suffix := 0; ; suffix++ {
		candidate := assignment + strconv.Itoa(suffix)
		cohort := retrievalPromotionStableCohort(candidate, receipt.SnapshotRef, receipt.PolicyRef, retrievalPromotionCohortWeights{ControlBasisPoints: 9500, CanaryBasisPoints: 500})
		if anyToString(cohort["arm"]) == "canary" {
			assignment = candidate
			receipt.AssignmentSubjectRef = contextPackLearnedScopeRef("assignment_subject", assignment)
			// The signed scope includes the assignment ref; rebuild the receipt and
			// ledger head so the activation path remains exact.
			receipt.ReceiptDigest = ""
			receipt.Issuer = contextPassportIssuer{}
			receipt.Signature = contextPassportSignature{}
			if err := retrievalPromotionSignCanaryReceipt(&receipt, identity); err != nil {
				t.Fatalf("resign canary assignment: %v", err)
			}
			store, err = newRetrievalPromotionCanaryLedger(path, identity)
			if err != nil {
				t.Fatalf("reload assignment ledger: %v", err)
			}
			// The old receipt is already durable; this branch only uses the first
			// assignment if it happened to map to canary. Rebuild path below when
			// the initial subject is control.
			store.close()
			break
		}
	}
	// Ensure the durable head's assignment and signature match the chosen
	// canary subject by using a fresh ledger for the actual assertion.
	path = filepath.Join(t.TempDir(), "automatic-rollback-final.ndjson")
	t.Setenv(retrievalPromotionCanaryLedgerPathEnv, path)
	receipt.Generation, receipt.PreviousHash, receipt.IdempotencyKey = 1, "", "auto-test-configure"
	receipt.RecordedAt = now.Add(-20 * time.Minute).Format(time.RFC3339Nano)
	receipt.ExpiresAt = now.Add(time.Hour).Format(time.RFC3339Nano)
	receipt.ReceiptDigest = ""
	receipt.Issuer = contextPassportIssuer{}
	receipt.Signature = contextPassportSignature{}
	if err := retrievalPromotionSignCanaryReceipt(&receipt, identity); err != nil {
		t.Fatalf("sign final active receipt: %v", err)
	}
	store, err = newRetrievalPromotionCanaryLedger(path, identity)
	if err != nil {
		t.Fatalf("create final rollback ledger: %v", err)
	}
	if err := store.configure(receipt, now.Add(-20*time.Minute)); err != nil {
		t.Fatalf("append final active receipt: %v", err)
	}
	store.close()
	impact := retrievalPromotionCompleteImpact(now, receipt)
	shadow := retrievalPromotionCompleteShadow(10)
	anyMap(shadow["shadow"])["repair_required_rate"] = 0.2
	eligible, reason, evidence := retrievalPromotionConfiguredActivationGate("project", "task", "intent", "workspace", receipt.PolicyRef, assignment, impact, shadow, now, identity)
	if eligible || reason != "automatic_rollback_committed" || !anyToBool(anyMap(evidence["automatic_rollback"])["committed"]) {
		t.Fatalf("metric regression did not commit durable rollback: eligible=%v reason=%s evidence=%#v", eligible, reason, evidence)
	}
	if _, configured, configuredReason := retrievalPromotionConfiguredCanaryReceiptFromLedger(identity, now.Add(time.Minute)); configured || configuredReason != "canary_rolled_back" {
		t.Fatalf("automatic rollback did not dominate after activation rejection: configured=%v reason=%s", configured, configuredReason)
	}
}

func TestRetrievalPromotionOwnerSerializesConcurrentAutomaticRollbackWithoutReplay(t *testing.T) {
	identity, err := loadOrCreateContextIdentity(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatalf("create test identity: %v", err)
	}
	path := filepath.Join(t.TempDir(), "concurrent-rollback.ndjson")
	now := time.Date(2026, time.August, 10, 20, 0, 0, 0, time.UTC)
	store, err := newRetrievalPromotionCanaryLedger(path, identity)
	if err != nil {
		t.Fatalf("create owner ledger: %v", err)
	}
	active := retrievalPromotionTestCanaryReceipt(now.Add(-20*time.Minute), "configure", 1, "", "opaque-agent-subject")
	if err := retrievalPromotionSignCanaryReceipt(&active, identity); err != nil {
		t.Fatalf("sign active receipt: %v", err)
	}
	if err := store.configure(active, now.Add(-20*time.Minute)); err != nil {
		t.Fatalf("append active receipt: %v", err)
	}
	if got := store.loadCount.Load(); got != 1 {
		t.Fatalf("owner replayed its append log before requests: loads=%d", got)
	}
	const callers = 8
	results := make(chan struct {
		receipt retrievalPromotionCanaryReceipt
		ok      bool
		reason  string
	}, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			receipt, ok, reason := retrievalPromotionAutomaticRollbackOnLedger(store, identity, active, now, "repair_regression")
			results <- struct {
				receipt retrievalPromotionCanaryReceipt
				ok      bool
				reason  string
			}{receipt, ok, reason}
		}()
	}
	wg.Wait()
	close(results)
	committed := 0
	for result := range results {
		if !result.ok {
			t.Fatalf("concurrent automatic rollback failed instead of idempotently observing the head: reason=%s", result.reason)
		}
		if result.reason == "automatic_rollback_committed" {
			committed++
		}
		if result.receipt.Operation != "rollback" {
			t.Fatalf("automatic rollback returned a non-rollback receipt: %#v", result.receipt)
		}
	}
	if committed != 1 {
		t.Fatalf("expected exactly one durable rollback commit, got %d", committed)
	}
	if got := store.loadCount.Load(); got != 1 {
		t.Fatalf("concurrent requests replayed the append log: loads=%d", got)
	}
	store.close()
	restarted, err := newRetrievalPromotionCanaryLedger(path, identity)
	if err != nil {
		t.Fatalf("reload owner ledger after concurrent rollback: %v", err)
	}
	defer restarted.close()
	if got := restarted.loadCount.Load(); got != 1 {
		t.Fatalf("restart did not perform exactly one bounded ledger replay: loads=%d", got)
	}
	if _, activeOK, reason := restarted.activeReceipt(now.Add(time.Minute)); activeOK || reason != "canary_rolled_back" {
		t.Fatalf("durable rollback did not dominate after restart: active=%v reason=%s", activeOK, reason)
	}
}

func TestRetrievalPromotionExposureLeaseRejectsHeadChangeBeforeRanking(t *testing.T) {
	identity, err := loadOrCreateContextIdentity(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatalf("create test identity: %v", err)
	}
	path := filepath.Join(t.TempDir(), "exposure-lease.ndjson")
	now := time.Date(2026, time.August, 10, 20, 0, 0, 0, time.UTC)
	store, err := newRetrievalPromotionCanaryLedger(path, identity)
	if err != nil {
		t.Fatalf("create exposure lease ledger: %v", err)
	}
	defer store.close()
	assignment := "lease-subject"
	active := retrievalPromotionTestCanaryReceipt(now.Add(-20*time.Minute), "configure", 1, "", assignment)
	for index := 0; index < 10000; index++ {
		candidate := assignment + "-" + strconv.Itoa(index)
		cohort := retrievalPromotionStableCohort(candidate, active.SnapshotRef, active.PolicyRef, retrievalPromotionCohortWeights{ControlBasisPoints: 9500, CanaryBasisPoints: 500})
		if anyToString(cohort["arm"]) == "canary" {
			assignment = candidate
			active.AssignmentSubjectRef = retrievalPromotionAssignmentSubjectRef(assignment)
			break
		}
	}
	active.ReceiptDigest = ""
	active.Issuer = contextPassportIssuer{}
	active.Signature = contextPassportSignature{}
	if err := retrievalPromotionSignCanaryReceipt(&active, identity); err != nil {
		t.Fatalf("sign active receipt: %v", err)
	}
	if err := store.configure(active, now.Add(-20*time.Minute)); err != nil {
		t.Fatalf("append active receipt: %v", err)
	}
	lease, reason := store.acquireExposureLease(active, now)
	if lease == nil || reason != "" {
		t.Fatalf("acquire exposure lease: lease=%#v reason=%s", lease, reason)
	}
	rollback := active
	rollback.Operation = "rollback"
	rollback.Generation = 2
	rollback.PreviousHash = active.ReceiptDigest
	rollback.ControlBasisPoints = retrievalPromotionTotalBasisPoints
	rollback.CanaryBasisPoints = 0
	rollback.RecordedAt = now.Format(time.RFC3339Nano)
	rollback.ExpiresAt = now.Add(time.Hour).Format(time.RFC3339Nano)
	rollback.IdempotencyKey = "lease-rollback"
	rollback.ReasonDigest = "sha256:" + sha256Hex("lease test")
	rollback.ReceiptDigest = ""
	rollback.Issuer = contextPassportIssuer{}
	rollback.Signature = contextPassportSignature{}
	if err := retrievalPromotionSignCanaryReceipt(&rollback, identity); err != nil {
		t.Fatalf("sign rollback receipt: %v", err)
	}
	if err := store.rollback(rollback, now); err != nil {
		t.Fatalf("append rollback receipt: %v", err)
	}
	if lease.withVerifiedHead(func() { t.Fatal("stale exposure lease applied ranking") }) {
		t.Fatal("head change was not observed by the exposure lease")
	}
}

func TestRetrievalPromotionExposureLeaseSerializesConcurrentRollback(t *testing.T) {
	identity, err := loadOrCreateContextIdentity(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatalf("create test identity: %v", err)
	}
	store, err := newRetrievalPromotionCanaryLedger(filepath.Join(t.TempDir(), "concurrent-exposure.ndjson"), identity)
	if err != nil {
		t.Fatalf("create concurrent exposure ledger: %v", err)
	}
	defer store.close()
	now := time.Date(2026, time.August, 10, 20, 0, 0, 0, time.UTC)
	active := retrievalPromotionTestCanaryReceipt(now.Add(-20*time.Minute), "configure", 1, "", "concurrent-lease-subject")
	if err := retrievalPromotionSignCanaryReceipt(&active, identity); err != nil {
		t.Fatalf("sign active receipt: %v", err)
	}
	if err := store.configure(active, now.Add(-20*time.Minute)); err != nil {
		t.Fatalf("append active receipt: %v", err)
	}
	lease, reason := store.acquireExposureLease(active, now)
	if lease == nil || reason != "" {
		t.Fatalf("acquire exposure lease: lease=%#v reason=%s", lease, reason)
	}
	rollback := active
	rollback.Operation = "rollback"
	rollback.Generation = 2
	rollback.PreviousHash = active.ReceiptDigest
	rollback.ControlBasisPoints = retrievalPromotionTotalBasisPoints
	rollback.ShadowBasisPoints = 0
	rollback.CanaryBasisPoints = 0
	rollback.RecordedAt = now.Format(time.RFC3339Nano)
	rollback.ExpiresAt = now.Add(time.Hour).Format(time.RFC3339Nano)
	rollback.IdempotencyKey = "concurrent-rollback"
	rollback.ReasonDigest = "sha256:" + sha256Hex("concurrent rollback")
	rollback.ReceiptDigest = ""
	rollback.Issuer = contextPassportIssuer{}
	rollback.Signature = contextPassportSignature{}
	if err := retrievalPromotionSignCanaryReceipt(&rollback, identity); err != nil {
		t.Fatalf("sign rollback receipt: %v", err)
	}

	rankingStarted := make(chan struct{})
	releaseRanking := make(chan struct{})
	rankingResult := make(chan bool, 1)
	go func() {
		rankingResult <- lease.withVerifiedHead(func() {
			close(rankingStarted)
			<-releaseRanking
		})
	}()
	<-rankingStarted
	rollbackStarted := make(chan struct{})
	rollbackResult := make(chan error, 1)
	go func() {
		close(rollbackStarted)
		rollbackResult <- store.rollback(rollback, now)
	}()
	<-rollbackStarted
	select {
	case err := <-rollbackResult:
		close(releaseRanking)
		<-rankingResult
		t.Fatalf("rollback interleaved with ranking lock: %v", err)
	default:
	}
	close(releaseRanking)
	if !<-rankingResult {
		t.Fatal("verified ranking did not complete before the concurrent rollback")
	}
	if err := <-rollbackResult; err != nil {
		t.Fatalf("durable concurrent rollback failed after ranking released: %v", err)
	}
	if _, activeOK, reason := store.activeReceipt(now); activeOK || reason != "canary_rolled_back" {
		t.Fatalf("rollback did not dominate after serialized ranking: active=%v reason=%s", activeOK, reason)
	}
	if lease.withVerifiedHead(func() {}) {
		t.Fatal("old exposure lease remained valid after rollback head advanced")
	}
}
