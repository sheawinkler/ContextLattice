package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func utilityTestDigest(seed string) string {
	return "sha256:" + sha256Hex(seed)
}

func newUtilityTestTelemetry(t *testing.T, limit int) *utilityTelemetry {
	t.Helper()
	telemetry := newUtilityTelemetry(limit)
	t.Cleanup(func() {
		if telemetry != nil && telemetry.store != nil {
			telemetry.store.close()
		}
	})
	return telemetry
}

func utilityTestFixture(outcomeID, sampleID, sessionID, taskClass, project string, value float64, modelVisible int, pairing map[string]any) (map[string]any, map[string]any, map[string]any, []map[string]any) {
	if pairing != nil {
		pairing = cloneAnyMap(pairing)
		pairID := anyToString(pairing["pair_id"])
		defaults := map[string]any{
			"experiment_id": "experiment_" + pairID, "assignment_digest": utilityTestDigest("assignment:" + pairID),
			"model": "gpt-test", "runner": "test-runner", "harness": "go-test",
			"context_reconstruction_contract": "agent_packet_reconstruction.v1",
		}
		for key, value := range defaults {
			if anyToString(pairing[key]) == "" {
				pairing[key] = value
			}
		}
	}
	eventID := "evt_verify_" + outcomeID
	digest := utilityTestDigest("evidence:" + outcomeID)
	quality := map[string]any{
		"sample_id": sampleID, "session_id": sessionID, "project": project,
		"agent_id": "codex_test", "task_class": taskClass, "quality_score": 92, "confidence": "high",
	}
	impact := map[string]any{
		"sample_id": sampleID, "session_id": sessionID, "project": project,
		"agent_id": "codex_test", "tokenizer_exact": true,
		"wire_tokens_exact": modelVisible + 200, "model_visible_context_tokens_exact": modelVisible,
		"tokenizer_encoding": "cl100k_base",
	}
	outcome := map[string]any{
		"outcome_id": outcomeID, "sample_id": sampleID, "session_id": sessionID,
		"project": project, "agent_id": "codex_test", "task_class": taskClass,
		"utility": map[string]any{
			"value": value, "unit": "acceptance_points", "verification_event_id": eventID,
			"evidence_digest": digest, "verification_passed": true,
			"verifier_kind": "deterministic_test", "verifier_id": "go_holdout",
		},
		"economics": map[string]any{"latency_ms": 40, "cost_microusd": 0, "tool_calls": 2, "failures": 0},
		"pairing":   pairing,
	}
	event := map[string]any{
		"id": eventID, "session_id": sessionID, "type": "verification.completed", "agent_id": "go_holdout", "created_at": nowUTCISO(),
		"metadata": map[string]any{"utility_verification": map[string]any{
			"outcome_id": outcomeID, "sample_id": sampleID, "utility_value": value,
			"utility_unit": "acceptance_points", "evidence_digest": digest,
			"verification_passed": true, "verifier_kind": "deterministic_test", "verifier_id": "go_holdout",
		}},
	}
	return outcome, quality, impact, []map[string]any{event}
}

func TestUtilityObservationSeparatesExactDenominatorsAndVerifiedYield(t *testing.T) {
	outcome, quality, impact, events := utilityTestFixture("outcome_exact", "sample_exact", "session_exact", "coding", "contextlattice", 8, 400, nil)
	row := buildUtilityObservation(outcome, quality, impact, events)
	if anyToString(row["status"]) != "verified_exact" || !anyToBool(anyMap(row["eligibility"])["observed_yield_eligible"]) {
		t.Fatalf("expected verified exact observation, got %#v", row)
	}
	if anyToString(anyMap(row["utility"])["verification_actor_id"]) != "go_holdout" {
		t.Fatalf("verified utility must retain the bound event actor: %#v", row["utility"])
	}
	denominator := anyMap(row["denominator"])
	if anyToInt(denominator["wire_tokens"], 0) != 600 || anyToInt(denominator["model_visible_context_tokens"], 0) != 400 {
		t.Fatalf("wire and model-visible denominators were not kept separate: %#v", denominator)
	}
	if anyToFloat(row["observed_utility_per_1k_model_visible_tokens"]) != 20 {
		t.Fatalf("unexpected observed utility yield: %#v", row)
	}
	if anyToBool(denominator["provider_usage_observed"]) {
		t.Fatal("provider usage must not be inferred")
	}
}

func TestUtilityObservationExcludesFailedVerificationAndInexactTokens(t *testing.T) {
	outcome, quality, impact, events := utilityTestFixture("outcome_excluded", "sample_excluded", "session_excluded", "review", "contextlattice", 3, 300, nil)
	proof := anyMap(anyMap(events[0]["metadata"])["utility_verification"])
	proof["verification_passed"] = false
	anyMap(outcome["utility"])["verification_passed"] = false
	impact["tokenizer_exact"] = false
	row := buildUtilityObservation(outcome, quality, impact, events)
	if anyToBool(anyMap(row["eligibility"])["observed_yield_eligible"]) || anyToString(row["status"]) != "excluded" {
		t.Fatalf("failed verification and inexact tokens must be excluded: %#v", row)
	}
	reasons := anyToStringList(anyMap(row["eligibility"])["exclusion_reasons"], 32)
	if !containsString(reasons, "verification_failed") || !containsString(reasons, "model_visible_context_tokens_not_exact") {
		t.Fatalf("expected explicit exclusions, got %#v", reasons)
	}
}

func TestUtilityObservationRequiresCompleteExactSourceJoin(t *testing.T) {
	outcome, quality, impact, events := utilityTestFixture("outcome_join", "sample_join", "session_join", "review", "contextlattice", 3, 300, nil)
	delete(impact, "agent_id")
	row := buildUtilityObservation(outcome, quality, impact, events)
	reasons := anyToStringList(anyMap(row["eligibility"])["exclusion_reasons"], 32)
	if anyToBool(anyMap(row["eligibility"])["observed_yield_eligible"]) || !containsString(reasons, "source_join_key_missing_agent_id") {
		t.Fatalf("incomplete exact source join must be excluded: %#v", row)
	}
}

func TestUtilityRowsForOutcomeIDsBoundsCloningAndRetainsMatchedControls(t *testing.T) {
	rows := make([]map[string]any, 0, 11)
	wanted := make(map[string]struct{}, 5)
	opaqueControlRef := ""
	for index := 0; index < 5; index++ {
		controlID := "control-" + strconv.Itoa(index)
		treatmentID := "treatment-" + strconv.Itoa(index)
		captured := "2026-08-04T12:0" + strconv.Itoa(index) + ":00Z"
		control := map[string]any{"outcome_id": controlID, "project": "contextlattice", "task_class": "coding", "captured_at": captured, "pairing": map[string]any{"arm": "control"}}
		controlRef := controlID
		if index == 4 {
			controlRef = utilityOpaqueControlRef(control)
			opaqueControlRef = controlRef
		}
		rows = append(rows,
			control,
			map[string]any{"outcome_id": treatmentID, "project": "contextlattice", "task_class": "coding", "captured_at": captured, "pairing": map[string]any{"arm": "treatment", "matched_control_outcome_id": controlRef}},
		)
		wanted[treatmentID] = struct{}{}
	}
	rows = append(rows, map[string]any{"outcome_id": "unrelated", "project": "contextlattice", "task_class": "coding", "captured_at": "2026-08-04T13:00:00Z"})
	telemetry := &utilityTelemetry{limit: len(rows), observations: rows}
	telemetry.reindexLocked()
	if _, indexed := telemetry.byOpaqueControlRef[opaqueControlRef]; !indexed {
		t.Fatalf("opaque matched-control index was not built: %#v", telemetry.byOpaqueControlRef)
	}
	selected := telemetry.rowsForOutcomeIDs(utilityQuery{Project: "contextlattice", TaskClass: "coding", Limit: 2}, wanted)
	if len(selected) != 4 {
		t.Fatalf("bounded exact-outcome projection returned %d rows, want two newest treatments plus two controls: %#v", len(selected), selected)
	}
	seen := map[string]struct{}{}
	for _, row := range selected {
		seen[anyToString(row["outcome_id"])] = struct{}{}
	}
	for _, expected := range []string{"treatment-3", "treatment-4", "control-3", "control-4"} {
		if _, present := seen[expected]; !present {
			t.Fatalf("bounded exact-outcome projection omitted %s: %#v", expected, selected)
		}
	}
	if _, leaked := seen["unrelated"]; leaked {
		t.Fatalf("unrelated utility row crossed the activation projection: %#v", selected)
	}
	selectedID := anyToString(selected[0]["outcome_id"])
	selected[0]["project"] = "mutated"
	for _, row := range rows {
		if anyToString(row["outcome_id"]) == selectedID && anyToString(row["project"]) != "contextlattice" {
			t.Fatalf("activation projection returned caller-mutable ledger state: %#v", row)
		}
	}
}

func TestUtilityRowsHonorLimitAndRetrievalIntent(t *testing.T) {
	rows := make([]map[string]any, 0, 6)
	for index := 0; index < 6; index++ {
		intent := "decision"
		if index%2 == 0 {
			intent = "exploration"
		}
		rows = append(rows, map[string]any{
			"outcome_id": "limited-" + strconv.Itoa(index), "project": "contextlattice",
			"task_class": "agent_workflow", "retrieval_intent": intent,
			"captured_at": "2026-08-04T12:0" + strconv.Itoa(index) + ":00Z",
		})
	}
	telemetry := &utilityTelemetry{limit: len(rows), observations: rows}
	selected := telemetry.rows(utilityQuery{Project: "contextlattice", RetrievalIntent: "decision", Limit: 2})
	if len(selected) != 2 || anyToString(selected[0]["outcome_id"]) != "limited-3" || anyToString(selected[1]["outcome_id"]) != "limited-5" {
		t.Fatalf("bounded retrieval-intent Utility query returned the wrong rows: %#v", selected)
	}
	selected[0]["project"] = "caller-mutation"
	if anyToString(rows[3]["project"]) != "contextlattice" {
		t.Fatal("bounded Utility rows exposed mutable ledger state")
	}
}

func TestUtilityHistoricalQueryRejectsFutureMutableRevision(t *testing.T) {
	asOf := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	row := map[string]any{
		"outcome_id": "historical-utility", "project": "contextlattice", "task_class": "agent_workflow",
		"retrieval_intent": "decision", "captured_at": "2026-08-08T10:00:00Z", "updated_at": "2026-08-08T13:00:00Z",
		"utility": map[string]any{"independently_verified": true, "verification_status": "verified", "verified_at": "2026-08-08T13:00:00Z"},
	}
	telemetry := &utilityTelemetry{limit: 4, observations: []map[string]any{row}}
	telemetry.reindexLocked()
	query := utilityQuery{Project: "contextlattice", To: asOf, Limit: 4}
	wanted := map[string]struct{}{"historical-utility": {}}
	if rows := telemetry.rowsForOutcomeIDs(query, wanted); len(rows) != 0 {
		t.Fatalf("future Utility revision changed a historical query: %#v", rows)
	}
	row["updated_at"] = "2026-08-08T11:00:00Z"
	if rows := telemetry.rowsForOutcomeIDs(query, wanted); len(rows) != 0 {
		t.Fatalf("future Utility verification changed a historical query: %#v", rows)
	}
	anyMap(row["utility"])["verified_at"] = "2026-08-08T11:30:00Z"
	if rows := telemetry.rowsForOutcomeIDs(query, wanted); len(rows) != 1 {
		t.Fatalf("boundary-visible Utility revision was not returned: %#v", rows)
	}
}

func utilityRowsForOutcomeIDsTestRow(outcomeID, capturedAt string, revision int, pairing map[string]any) map[string]any {
	return map[string]any{
		"schema_id": utilityObservationContractID, "outcome_id": outcomeID,
		"project": "contextlattice", "task_class": "coding", "captured_at": capturedAt,
		"revision": revision, "pairing": pairing,
	}
}

func TestUtilityRowsForOutcomeIDsReindexesAfterRetention(t *testing.T) {
	telemetry := &utilityTelemetry{limit: 3, observations: []map[string]any{}, byOutcome: map[string]int{}, byOpaqueControlRef: map[string]int{}}
	oldControl := utilityRowsForOutcomeIDsTestRow("control-old", "2026-08-04T12:00:00Z", 1, map[string]any{"arm": "control"})
	oldControlRef := utilityOpaqueControlRef(oldControl)
	rows := []map[string]any{
		oldControl,
		utilityRowsForOutcomeIDsTestRow("treatment-old", "2026-08-04T12:01:00Z", 1, map[string]any{"arm": "treatment", "matched_control_outcome_id": oldControlRef}),
		utilityRowsForOutcomeIDsTestRow("control-new", "2026-08-04T12:02:00Z", 1, map[string]any{"arm": "control"}),
		utilityRowsForOutcomeIDsTestRow("treatment-new", "2026-08-04T12:03:00Z", 1, map[string]any{"arm": "treatment", "matched_control_outcome_id": "control-new"}),
	}
	telemetry.mu.Lock()
	for _, row := range rows {
		telemetry.applyLocked(row)
	}
	_, oldOutcomeIndexed := telemetry.byOutcome["control-old"]
	_, oldOpaqueIndexed := telemetry.byOpaqueControlRef[oldControlRef]
	telemetry.mu.Unlock()
	if oldOutcomeIndexed || oldOpaqueIndexed {
		t.Fatalf("retention left an evicted control indexed: outcome=%t opaque=%t", oldOutcomeIndexed, oldOpaqueIndexed)
	}

	wanted := map[string]struct{}{"treatment-old": {}, "treatment-new": {}}
	selected := telemetry.rowsForOutcomeIDs(utilityQuery{Project: "contextlattice", TaskClass: "coding", Limit: 2}, wanted)
	seen := map[string]struct{}{}
	for _, row := range selected {
		seen[anyToString(row["outcome_id"])] = struct{}{}
	}
	for _, expected := range []string{"treatment-old", "treatment-new", "control-new"} {
		if _, present := seen[expected]; !present {
			t.Fatalf("retained indexed projection omitted %s: %#v", expected, selected)
		}
	}
	if _, present := seen["control-old"]; present {
		t.Fatalf("evicted opaque control leaked through a stale index: %#v", selected)
	}
}

func TestUtilityRowsForOutcomeIDsConcurrentReindexAndLookup(t *testing.T) {
	telemetry := &utilityTelemetry{limit: 32, observations: []map[string]any{}, byOutcome: map[string]int{}, byOpaqueControlRef: map[string]int{}}
	wanted := map[string]struct{}{}
	for index := 0; index < 8; index++ {
		controlID := "race-control-" + strconv.Itoa(index)
		treatmentID := "race-treatment-" + strconv.Itoa(index)
		control := utilityRowsForOutcomeIDsTestRow(controlID, "2026-08-04T12:00:00Z", 1, map[string]any{"arm": "control"})
		controlRef := controlID
		if index%2 == 0 {
			controlRef = utilityOpaqueControlRef(control)
		}
		telemetry.applyLocked(control)
		telemetry.applyLocked(utilityRowsForOutcomeIDsTestRow(treatmentID, "2026-08-04T12:00:00Z", 1, map[string]any{
			"arm": "treatment", "matched_control_outcome_id": controlRef,
		}))
		wanted[treatmentID] = struct{}{}
	}

	errCh := make(chan string, 1)
	report := func(message string) {
		select {
		case errCh <- message:
		default:
		}
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for iteration := 0; iteration < 200; iteration++ {
			index := iteration % 8
			controlID := "race-control-" + strconv.Itoa(index)
			treatmentID := "race-treatment-" + strconv.Itoa(index)
			control := utilityRowsForOutcomeIDsTestRow(controlID, "2026-08-04T12:00:00Z", iteration+2, map[string]any{"arm": "control"})
			controlRef := controlID
			if index%2 == 0 {
				controlRef = utilityOpaqueControlRef(control)
			}
			telemetry.mu.Lock()
			telemetry.applyLocked(control)
			telemetry.applyLocked(utilityRowsForOutcomeIDsTestRow(treatmentID, "2026-08-04T12:00:00Z", iteration+2, map[string]any{
				"arm": "treatment", "matched_control_outcome_id": controlRef,
			}))
			telemetry.applyLocked(utilityRowsForOutcomeIDsTestRow("race-unrelated-"+strconv.Itoa(iteration), "2026-08-04T13:00:00Z", 1, nil))
			telemetry.mu.Unlock()
		}
	}()
	for reader := 0; reader < 4; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for iteration := 0; iteration < 250; iteration++ {
				rows := telemetry.rowsForOutcomeIDs(utilityQuery{Project: "contextlattice", TaskClass: "coding", Limit: 8}, wanted)
				for _, row := range rows {
					if anyToString(row["outcome_id"]) == "" || anyToString(row["project"]) != "contextlattice" {
						report("concurrent indexed projection returned an invalid row")
						return
					}
					row["project"] = "caller-mutation"
				}
			}
		}()
	}
	wg.Wait()
	select {
	case message := <-errCh:
		t.Fatal(message)
	default:
	}
	telemetry.mu.Lock()
	defer telemetry.mu.Unlock()
	for _, row := range telemetry.observations {
		if anyToString(row["project"]) != "contextlattice" {
			t.Fatalf("caller mutation crossed the clone boundary: %#v", row)
		}
	}
}

func TestUtilityOpaquePairingPreservesExactMatchedControlJoin(t *testing.T) {
	digest := utilityTestDigest("opaque-pair-task")
	controlPairing := map[string]any{
		"pair_id": "pair_private_customer_omega", "arm": "control", "matching_method": "exact_holdout",
		"task_match_digest": digest, "leakage_free": true,
	}
	treatmentPairing := cloneAnyMap(controlPairing)
	treatmentPairing["arm"] = "treatment"
	treatmentPairing["matched_control_outcome_id"] = "outcome_opaque_pair_control"
	control, controlQuality, controlImpact, controlEvents := utilityTestFixture("outcome_opaque_pair_control", "sample_opaque_pair_control", "session_opaque_pair_control", "coding", "contextlattice", 2, 300, controlPairing)
	treatment, treatmentQuality, treatmentImpact, treatmentEvents := utilityTestFixture("outcome_opaque_pair_treatment", "sample_opaque_pair_treatment", "session_opaque_pair_treatment", "coding", "contextlattice", 4, 300, treatmentPairing)
	controlEntry := contextPackQualityOutcomeFromSample(control)
	treatmentEntry := contextPackQualityOutcomeFromSample(treatment)
	controlObservation := buildUtilityObservation(controlEntry, controlQuality, controlImpact, controlEvents)
	treatmentObservation := buildUtilityObservation(treatmentEntry, treatmentQuality, treatmentImpact, treatmentEvents)
	if anyToString(controlObservation["status"]) != "verified_exact" || anyToString(treatmentObservation["status"]) != "verified_exact" {
		t.Fatalf("opaque utility claims did not join raw verification events: control=%#v treatment=%#v", controlObservation, treatmentObservation)
	}
	_, pairs, exclusions := utilityPairProjection([]map[string]any{controlObservation, treatmentObservation})
	if len(pairs) != 1 || len(exclusions) != 0 || !utilitySHA256DigestValid(pairs[0].PairID) || strings.Contains(pairs[0].PairID, "private_customer") {
		t.Fatalf("opaque pairing did not retain an exact matched-control join: pairs=%#v exclusions=%#v", pairs, exclusions)
	}
}

func TestUtilityObservationRejectsSelfVerification(t *testing.T) {
	outcome, quality, impact, events := utilityTestFixture("outcome_self", "sample_self", "session_self", "review", "contextlattice", 3, 300, nil)
	anyMap(outcome["utility"])["verifier_id"] = "codex_test"
	anyMap(anyMap(events[0]["metadata"])["utility_verification"])["verifier_id"] = "codex_test"
	events[0]["agent_id"] = "codex_test"
	row := buildUtilityObservation(outcome, quality, impact, events)
	reasons := anyToStringList(anyMap(row["eligibility"])["exclusion_reasons"], 32)
	if anyToBool(anyMap(row["eligibility"])["observed_yield_eligible"]) || !containsString(reasons, "verification_not_independent") {
		t.Fatalf("self-verification must be excluded explicitly: %#v", row)
	}
}

func TestUtilityObservationRejectsUnboundVerifierIdentity(t *testing.T) {
	outcome, quality, impact, events := utilityTestFixture("outcome_actor", "sample_actor", "session_actor", "review", "contextlattice", 3, 300, nil)
	events[0]["agent_id"] = "different_verifier"
	row := buildUtilityObservation(outcome, quality, impact, events)
	reasons := anyToStringList(anyMap(row["eligibility"])["exclusion_reasons"], 32)
	if anyToBool(anyMap(row["eligibility"])["observed_yield_eligible"]) || !containsString(reasons, "verification_verifier_identity_mismatch") {
		t.Fatalf("verification metadata without matching event identity must be excluded: %#v", row)
	}
}

func TestUtilityMatchedControlRetainsNegativeCausalGain(t *testing.T) {
	digest := utilityTestDigest("matched-task")
	controlPairing := map[string]any{"pair_id": "pair_negative", "arm": "control", "task_match_digest": digest, "matching_method": "exact_holdout", "leakage_free": true}
	treatmentPairing := map[string]any{"pair_id": "pair_negative", "arm": "treatment", "matched_control_outcome_id": "control_negative", "task_match_digest": digest, "matching_method": "exact_holdout", "leakage_free": true}
	controlOutcome, controlQuality, controlImpact, controlEvents := utilityTestFixture("control_negative", "sample_control", "session_control", "coding", "contextlattice", 10, 500, controlPairing)
	treatmentOutcome, treatmentQuality, treatmentImpact, treatmentEvents := utilityTestFixture("treatment_negative", "sample_treatment", "session_treatment", "coding", "contextlattice", 8, 500, treatmentPairing)
	rows := []map[string]any{
		buildUtilityObservation(controlOutcome, controlQuality, controlImpact, controlEvents),
		buildUtilityObservation(treatmentOutcome, treatmentQuality, treatmentImpact, treatmentEvents),
	}
	projected, pairs, exclusions := utilityPairProjection(rows)
	if len(pairs) != 1 || pairs[0].UtilityGain != -2 || pairs[0].GainPer1K != -4 {
		t.Fatalf("negative matched-control gain must remain visible: pairs=%#v exclusions=%#v", pairs, exclusions)
	}
	var treatment map[string]any
	for _, row := range projected {
		if anyToString(row["outcome_id"]) == "treatment_negative" {
			treatment = row
			break
		}
	}
	if !anyToBool(anyMap(treatment["eligibility"])["causal_gain_eligible"]) {
		t.Fatalf("valid negative pair was censored: %#v", treatment)
	}
}

func TestUtilityMatchedControlRequiresSymmetricExactDenominator(t *testing.T) {
	digest := utilityTestDigest("matched-denominator")
	controlPairing := map[string]any{"pair_id": "pair_denominator", "arm": "control", "task_match_digest": digest, "matching_method": "exact_holdout", "leakage_free": true}
	treatmentPairing := map[string]any{"pair_id": "pair_denominator", "arm": "treatment", "matched_control_outcome_id": "control_denominator", "task_match_digest": digest, "matching_method": "exact_holdout", "leakage_free": true}
	controlOutcome, controlQuality, controlImpact, controlEvents := utilityTestFixture("control_denominator", "sample_control_denominator", "session_control_denominator", "coding", "contextlattice", 4, 400, controlPairing)
	treatmentOutcome, treatmentQuality, treatmentImpact, treatmentEvents := utilityTestFixture("treatment_denominator", "sample_treatment_denominator", "session_treatment_denominator", "coding", "contextlattice", 8, 500, treatmentPairing)
	rows := []map[string]any{
		buildUtilityObservation(controlOutcome, controlQuality, controlImpact, controlEvents),
		buildUtilityObservation(treatmentOutcome, treatmentQuality, treatmentImpact, treatmentEvents),
	}
	projected, pairs, exclusions := utilityPairProjection(rows)
	if len(pairs) != 0 || exclusions["model_visible_context_tokens_mismatch"] != 1 {
		t.Fatalf("asymmetric exact denominators must abstain: pairs=%#v exclusions=%#v", pairs, exclusions)
	}
	if anyToBool(anyMap(projected[1]["eligibility"])["causal_gain_eligible"]) {
		t.Fatalf("asymmetric denominator was promoted to causal evidence: %#v", projected[1])
	}
}

func TestUtilityMatchedControlAbstainsOnLeakageAndMissingControl(t *testing.T) {
	digest := utilityTestDigest("leaky-task")
	controlPairing := map[string]any{"pair_id": "pair_leaky", "arm": "control", "task_match_digest": digest, "matching_method": "exact_holdout", "leakage_free": true}
	treatmentPairing := map[string]any{"pair_id": "pair_leaky", "arm": "treatment", "matched_control_outcome_id": "control_leaky", "task_match_digest": digest, "matching_method": "exact_holdout", "leakage_free": false}
	controlOutcome, controlQuality, controlImpact, controlEvents := utilityTestFixture("control_leaky", "sample_control_leaky", "session_control_leaky", "coding", "contextlattice", 5, 500, controlPairing)
	treatmentOutcome, treatmentQuality, treatmentImpact, treatmentEvents := utilityTestFixture("treatment_leaky", "sample_treatment_leaky", "session_treatment_leaky", "coding", "contextlattice", 9, 500, treatmentPairing)
	rows := []map[string]any{
		buildUtilityObservation(controlOutcome, controlQuality, controlImpact, controlEvents),
		buildUtilityObservation(treatmentOutcome, treatmentQuality, treatmentImpact, treatmentEvents),
	}
	_, pairs, exclusions := utilityPairProjection(rows)
	if len(pairs) != 0 || exclusions["treatment_leakage_unproven"] != 1 {
		t.Fatalf("leaky pair must abstain: pairs=%#v exclusions=%#v", pairs, exclusions)
	}
	missing := cloneAnyMap(rows[1])
	anyMap(missing["pairing"])["matched_control_outcome_id"] = "absent_control"
	_, pairs, exclusions = utilityPairProjection([]map[string]any{missing})
	if len(pairs) != 0 || exclusions["matched_control_not_found"] != 1 {
		t.Fatalf("missing control must remain explicit: pairs=%#v exclusions=%#v", pairs, exclusions)
	}
}

func TestUtilityAggregateRefusesMixedUtilityUnits(t *testing.T) {
	firstOutcome, firstQuality, firstImpact, firstEvents := utilityTestFixture("outcome_points", "sample_points", "session_points", "coding", "contextlattice", 8, 500, nil)
	secondOutcome, secondQuality, secondImpact, secondEvents := utilityTestFixture("outcome_seconds", "sample_seconds", "session_seconds", "coding", "contextlattice", 4, 500, nil)
	anyMap(secondOutcome["utility"])["unit"] = "seconds_saved"
	anyMap(anyMap(secondEvents[0]["metadata"])["utility_verification"])["utility_unit"] = "seconds_saved"
	rows := []map[string]any{
		buildUtilityObservation(firstOutcome, firstQuality, firstImpact, firstEvents),
		buildUtilityObservation(secondOutcome, secondQuality, secondImpact, secondEvents),
	}
	summary := utilityAggregate(rows, nil, nil)
	if anyToString(summary["claim_status"]) != "mixed_utility_units" || summary["verified_utility_sum"] != nil || summary["observed_utility_per_1k_model_visible_tokens"] != nil {
		t.Fatalf("incommensurate utility units were aggregated: %#v", summary)
	}
}

func TestUtilityLedgerPersistsLatestRevisionWithinBounds(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "utility.ndjson")
	t.Setenv("GO_UTILITY_LEDGER_PATH", ledgerPath)
	t.Setenv("GO_UTILITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_UTILITY_LEDGER_MAX_SAMPLES", "20")
	telemetry := newUtilityTestTelemetry(t, 20)
	outcome, quality, impact, events := utilityTestFixture("outcome_persisted", "sample_persisted", "session_persisted", "coding", "contextlattice", 4, 250, nil)
	row, recorded, err := telemetry.record(buildUtilityObservation(outcome, quality, impact, events))
	if err != nil || !recorded {
		t.Fatal("expected utility observation to be recorded")
	}
	anyMap(row["economics"])["failures"] = 1
	updated, err := telemetry.update(row)
	if err != nil {
		t.Fatalf("persist second revision: %v", err)
	}
	if anyToInt(updated["revision"], 0) != 2 {
		t.Fatalf("expected second durable revision, got %#v", updated)
	}
	telemetry.store.close()
	reloaded := newUtilityTestTelemetry(t, 20)
	snapshot := reloaded.snapshot(utilityQuery{Limit: 20})
	if anyToString(anyMap(snapshot["storage"])["durability"]) != "bounded_fsync_ndjson" {
		t.Fatalf("utility ledger must report its durable write contract: %#v", snapshot["storage"])
	}
	if anyToInt(anyMap(snapshot["summary"])["observation_count"], 0) != 1 {
		t.Fatalf("revisions must reload as one logical outcome: %#v", snapshot)
	}
	observations := contextPackAnyList(snapshot["observations"])
	if len(observations) != 1 || anyToInt(anyMap(observations[0])["revision"], 0) != 2 {
		t.Fatalf("latest revision was not retained: %#v", observations)
	}
}

func TestUtilityLedgerRevisionWritesStayPhysicallyBounded(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "utility.ndjson")
	t.Setenv("GO_UTILITY_LEDGER_PATH", ledgerPath)
	t.Setenv("GO_UTILITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_UTILITY_LEDGER_MAX_SAMPLES", "20")
	t.Setenv("GO_UTILITY_LEDGER_MAX_BYTES", "65536")
	telemetry := newUtilityTestTelemetry(t, 20)
	outcome, quality, impact, events := utilityTestFixture("outcome_revisions", "sample_revisions", "session_revisions", "coding", "contextlattice", 4, 250, nil)
	row, recorded, err := telemetry.record(buildUtilityObservation(outcome, quality, impact, events))
	if err != nil || !recorded {
		t.Fatalf("record revision seed: recorded=%t err=%v", recorded, err)
	}
	for index := 0; index < 40; index++ {
		anyMap(row["economics"])["latency_ms"] = index + 1
		row, err = telemetry.update(row)
		if err != nil {
			t.Fatalf("persist revision %d: %v", index+2, err)
		}
	}
	raw, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read revision-safe utility ledger: %v", err)
	}
	if lines := strings.Count(strings.TrimSpace(string(raw)), "\n") + 1; lines != 1 {
		t.Fatalf("revision writes accumulated physical rows: lines=%d", lines)
	}
	stat, err := os.Stat(ledgerPath)
	if err != nil || stat.Size() > 65536 {
		t.Fatalf("revision-safe utility ledger exceeded bytes: stat=%#v err=%v", stat, err)
	}
}

func TestUtilityLedgerOutOfOrderRevisionWriteCannotReplaceLatest(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "utility.ndjson")
	t.Setenv("GO_UTILITY_LEDGER_PATH", ledgerPath)
	t.Setenv("GO_UTILITY_LEDGER_ENABLED", "true")
	telemetry := newUtilityTestTelemetry(t, 20)
	claimDigest := utilityTestDigest("outcome_ordered")
	latest := map[string]any{"schema_id": utilityObservationContractID, "source_claim_digest": claimDigest, "outcome_id": "outcome_ordered", "revision": 3, "status": "latest"}
	stale := map[string]any{"schema_id": utilityObservationContractID, "source_claim_digest": claimDigest, "outcome_id": "outcome_ordered", "revision": 2, "status": "stale"}
	if _, wrote, err := telemetry.store.append(latest); err != nil || !wrote {
		t.Fatalf("append latest revision: %v", err)
	}
	if persisted, wrote, err := telemetry.store.append(stale); err != nil || wrote || anyToInt(persisted["revision"], 0) != 3 {
		t.Fatalf("append stale revision: %v", err)
	}
	rows, _, err := telemetry.store.readRows()
	if err != nil || len(rows) != 1 || anyToInt(rows[0]["revision"], 0) != 3 || anyToString(rows[0]["status"]) != "latest" {
		t.Fatalf("stale out-of-order revision replaced latest: rows=%#v err=%v", rows, err)
	}
}

func TestUtilityOutcomeDuplicateIsIdempotentButConflictingClaimReturns409(t *testing.T) {
	t.Setenv("GO_UTILITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_UTILITY_LEDGER_PATH", filepath.Join(t.TempDir(), "utility.ndjson"))
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", filepath.Join(t.TempDir(), "context-pack-quality.ndjson"))
	qualityTelemetry := newContextPackQualityTelemetry(20)
	qualityTelemetry.recordQuality(map[string]any{
		"sample_id": "sample_duplicate", "session_id": "session_duplicate", "project": "contextlattice", "agent_id": "codex_test", "task_class": "coding",
	})
	impactTelemetry := newTokenImpactTelemetry(20)
	impactTelemetry.record(map[string]any{
		"sample_id": "sample_duplicate", "session_id": "session_duplicate", "project": "contextlattice", "agent_id": "codex_test",
	})
	s := &server{contextPackQuality: qualityTelemetry, tokenImpact: impactTelemetry, utility: newUtilityTestTelemetry(t, 20)}
	payload := map[string]any{
		"outcome_id": "outcome_duplicate", "sample_id": "sample_duplicate", "session_id": "session_duplicate",
		"project": "contextlattice", "agent_id": "codex_test", "task_class": "coding",
		"utility": map[string]any{
			"value": 4, "unit": "acceptance_points", "verification_event_id": "event_duplicate",
			"evidence_digest": utilityTestDigest("duplicate"), "verification_passed": true,
			"verifier_kind": "deterministic_test", "verifier_id": "go_holdout",
		},
	}
	post := func(body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal duplicate payload: %v", err)
		}
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(raw))
		s.telemetryContextPackQualityOutcomeRoute(recorder, request)
		return recorder
	}
	if first := post(payload); first.Code != http.StatusOK {
		t.Fatalf("first utility claim status=%d body=%s", first.Code, first.Body.String())
	}
	if replay := post(cloneAnyMap(payload)); replay.Code != http.StatusOK {
		t.Fatalf("idempotent utility replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	conflict := cloneAnyMap(payload)
	anyMap(conflict["utility"])["value"] = 9
	response := post(conflict)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "utility_outcome_conflict") {
		t.Fatalf("conflicting utility claim status=%d body=%s", response.Code, response.Body.String())
	}
	stored, ok := s.utility.observation("outcome_duplicate")
	if !ok || anyToFloat(anyMap(stored["utility"])["value"]) != 4 {
		t.Fatalf("conflicting utility claim mutated the ledger: %#v", stored)
	}
}

func TestUtilityOutcomeDuplicateWithContextQualityUsesLogicalClaimIdentity(t *testing.T) {
	t.Setenv("GO_UTILITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_UTILITY_LEDGER_PATH", filepath.Join(t.TempDir(), "utility.ndjson"))
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", filepath.Join(t.TempDir(), "context-pack-quality.ndjson"))
	s := &server{
		contextPackQuality: newContextPackQualityTelemetry(20),
		utility:            newUtilityTestTelemetry(t, 20),
		tokenImpact:        newTokenImpactTelemetry(20),
	}
	payload := map[string]any{
		"outcome_id": "outcome_duplicate_context_quality", "sample_id": "sample_duplicate_context_quality", "session_id": "session_duplicate_context_quality",
		"project": "contextlattice", "agent_id": "codex_test", "task_class": "coding",
		"utility": map[string]any{
			"value": 4, "unit": "acceptance_points", "verification_event_id": "event_duplicate_context_quality",
			"evidence_digest": utilityTestDigest("duplicate-context-quality"), "verification_passed": true,
			"verifier_kind": "deterministic_test", "verifier_id": "go_holdout",
		},
	}
	s.contextPackQuality.recordQuality(map[string]any{
		"sample_id": "sample_duplicate_context_quality", "session_id": "session_duplicate_context_quality", "project": "contextlattice", "agent_id": "codex_test", "task_class": "coding",
	})
	s.tokenImpact.record(map[string]any{
		"sample_id": "sample_duplicate_context_quality", "session_id": "session_duplicate_context_quality", "project": "contextlattice", "agent_id": "codex_test",
	})
	post := func(body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal context-quality duplicate payload: %v", err)
		}
		recorder := httptest.NewRecorder()
		s.telemetryContextPackQualityOutcomeRoute(recorder, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(raw)))
		return recorder
	}
	if first := post(payload); first.Code != http.StatusOK {
		t.Fatalf("first context-quality utility claim status=%d body=%s", first.Code, first.Body.String())
	}
	if replay := post(cloneAnyMap(payload)); replay.Code != http.StatusOK {
		t.Fatalf("idempotent context-quality utility replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	conflict := cloneAnyMap(payload)
	anyMap(conflict["utility"])["value"] = 9
	if response := post(conflict); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "utility_outcome_conflict") {
		t.Fatalf("context-quality canonicalization accepted changed utility claim: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestUtilityDurableClaimBindingSurvivesMemoryEviction(t *testing.T) {
	t.Setenv("GO_UTILITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_UTILITY_LEDGER_PATH", filepath.Join(t.TempDir(), "utility.ndjson"))
	t.Setenv("GO_UTILITY_LEDGER_MAX_SAMPLES", "20")
	telemetry := newUtilityTestTelemetry(t, 1)
	firstOutcome, firstQuality, firstImpact, firstEvents := utilityTestFixture("outcome_evicted", "sample_evicted", "session_evicted", "coding", "contextlattice", 4, 250, nil)
	first := buildUtilityObservation(firstOutcome, firstQuality, firstImpact, firstEvents)
	if _, recorded, err := telemetry.record(first); err != nil || !recorded {
		t.Fatalf("record first durable claim: recorded=%t err=%v", recorded, err)
	}
	secondOutcome, secondQuality, secondImpact, secondEvents := utilityTestFixture("outcome_resident", "sample_resident", "session_resident", "coding", "contextlattice", 2, 250, nil)
	if _, recorded, err := telemetry.record(buildUtilityObservation(secondOutcome, secondQuality, secondImpact, secondEvents)); err != nil || !recorded {
		t.Fatalf("record eviction row: recorded=%t err=%v", recorded, err)
	}
	telemetry.mu.Lock()
	_, resident := telemetry.byOutcome["outcome_evicted"]
	telemetry.mu.Unlock()
	if resident {
		t.Fatal("test did not evict the first claim from the memory window")
	}
	if existing, recorded, err := telemetry.record(cloneAnyMap(first)); err != nil || recorded || anyToFloat(anyMap(existing["utility"])["value"]) != 4 {
		t.Fatalf("durable idempotent replay was not recognized: existing=%#v recorded=%t err=%v", existing, recorded, err)
	}
	conflict := cloneAnyMap(firstOutcome)
	anyMap(conflict["utility"])["value"] = 9
	conflictingRow := buildUtilityObservation(conflict, firstQuality, firstImpact, firstEvents)
	if _, recorded, err := telemetry.record(conflictingRow); !errors.Is(err, errUtilityOutcomeConflict) || recorded {
		t.Fatalf("evicted durable claim accepted a conflict: recorded=%t err=%v", recorded, err)
	}
	durable, ok := telemetry.store.observation("outcome_evicted")
	if !ok || anyToFloat(anyMap(durable["utility"])["value"]) != 4 {
		t.Fatalf("durable conflict mutated prior evidence: %#v", durable)
	}
}

func TestUtilitySecondProcessSharingLedgerFailsClosed(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "utility.ndjson")
	t.Setenv("GO_UTILITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_UTILITY_LEDGER_PATH", ledgerPath)
	firstStore := newUtilityTestTelemetry(t, 20)
	secondStore := newUtilityTestTelemetry(t, 20)
	firstStatus := utilityStorageStatus(firstStore.store)
	secondStatus := utilityStorageStatus(secondStore.store)
	if !anyToBool(firstStatus["enabled"]) || !anyToBool(secondStatus["configured"]) || anyToBool(secondStatus["enabled"]) ||
		anyToString(secondStatus["durability"]) != "unavailable" || anyToString(secondStatus["last_error"]) != "single_writer_lock_unavailable" {
		t.Fatalf("shared-path single-writer guard is ambiguous: first=%#v second=%#v", firstStatus, secondStatus)
	}
	outcome, quality, impact, _ := utilityTestFixture("outcome_single_writer", "sample_single_writer", "session_single_writer", "coding", "contextlattice", 4, 250, nil)
	row := buildUtilityObservation(outcome, quality, impact, nil)
	if _, recorded, err := secondStore.record(row); !errors.Is(err, errUtilityPersistenceUnavailable) || recorded {
		t.Fatalf("second process wrote through the single-writer guard: recorded=%t err=%v", recorded, err)
	}
	if _, recorded, err := firstStore.record(row); err != nil || !recorded {
		t.Fatalf("lock owner could not persist utility evidence: recorded=%t err=%v", recorded, err)
	}
	firstStore.store.close()
	if _, enabled := firstStore.store.availability(); enabled {
		t.Fatal("closed utility ledger store remained enabled without its lifetime lock")
	}
	if _, recorded, err := firstStore.record(row); !errors.Is(err, errUtilityPersistenceUnavailable) || recorded {
		t.Fatalf("closed utility ledger store accepted a write: recorded=%t err=%v", recorded, err)
	}
	reopened := newUtilityTestTelemetry(t, 20)
	if status := utilityStorageStatus(reopened.store); !anyToBool(status["enabled"]) {
		t.Fatalf("ledger lock was not released for restart: %#v", status)
	}
	if persisted, ok := reopened.observation("outcome_single_writer"); !ok || anyToFloat(anyMap(persisted["utility"])["value"]) != 4 {
		t.Fatalf("restart did not recover the lock owner's evidence: %#v", persisted)
	}
}

func TestUtilityVerificationReconcilesEvictedObservation(t *testing.T) {
	t.Setenv("GO_UTILITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_UTILITY_LEDGER_PATH", filepath.Join(t.TempDir(), "utility.ndjson"))
	t.Setenv("GO_UTILITY_LEDGER_MAX_SAMPLES", "20")
	telemetry := newUtilityTestTelemetry(t, 1)
	outcome, quality, impact, events := utilityTestFixture("outcome_evicted_verify", "sample_evicted_verify", "session_evicted_verify", "coding", "contextlattice", 4, 250, nil)
	if _, recorded, err := telemetry.record(buildUtilityObservation(outcome, quality, impact, nil)); err != nil || !recorded {
		t.Fatalf("record claim awaiting verification: recorded=%t err=%v", recorded, err)
	}
	otherOutcome, otherQuality, otherImpact, _ := utilityTestFixture("outcome_eviction_trigger", "sample_eviction_trigger", "session_eviction_trigger", "coding", "contextlattice", 2, 250, nil)
	if _, recorded, err := telemetry.record(buildUtilityObservation(otherOutcome, otherQuality, otherImpact, nil)); err != nil || !recorded {
		t.Fatalf("record eviction trigger: recorded=%t err=%v", recorded, err)
	}
	telemetry.mu.Lock()
	_, resident := telemetry.byOutcome["outcome_evicted_verify"]
	telemetry.mu.Unlock()
	if resident {
		t.Fatal("test did not evict the observation awaiting verification")
	}
	s := &server{utility: telemetry}
	reconciliation := s.recordUtilitySessionEvent(map[string]any{"id": "session_evicted_verify", "agent_id": "codex_test"}, events[0])
	if !anyToBool(reconciliation["ok"]) || anyToString(reconciliation["status"]) != "reconciled" || anyToInt(reconciliation["revision"], 0) != 2 {
		t.Fatalf("evicted observation reconciliation was falsely reported: %#v", reconciliation)
	}
	durable, ok := telemetry.store.observation("outcome_evicted_verify")
	if !ok || anyToInt(durable["revision"], 0) != 2 || !anyToBool(anyMap(durable["utility"])["independently_verified"]) {
		t.Fatalf("evicted observation was not durably reconciled: %#v", durable)
	}
}

func TestUtilityRestartRejectsConflictingEqualRevision(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "utility.ndjson")
	t.Setenv("GO_UTILITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_UTILITY_LEDGER_PATH", ledgerPath)
	outcome, quality, impact, events := utilityTestFixture("outcome_restart_conflict", "sample_restart_conflict", "session_restart_conflict", "coding", "contextlattice", 4, 250, nil)
	first := buildUtilityObservation(outcome, quality, impact, events)
	changedOutcome := cloneAnyMap(outcome)
	anyMap(changedOutcome["utility"])["value"] = 9
	second := buildUtilityObservation(changedOutcome, quality, impact, events)
	firstRaw, _ := json.Marshal(first)
	secondRaw, _ := json.Marshal(second)
	if err := os.WriteFile(ledgerPath, append(append(firstRaw, '\n'), append(secondRaw, '\n')...), 0o600); err != nil {
		t.Fatalf("seed conflicting durable revisions: %v", err)
	}
	reloaded := newUtilityTestTelemetry(t, 20)
	row, ok := reloaded.observation("outcome_restart_conflict")
	if !ok || anyToFloat(anyMap(row["utility"])["value"]) != 4 {
		t.Fatalf("restart last-wins behavior replaced the original claim: %#v", row)
	}
	storage := utilityStorageStatus(reloaded.store)
	if anyToInt(storage["parse_errors"], 0) != 1 || anyToInt(storage["physical_rows"], 0) != 1 {
		t.Fatalf("restart did not surface and compact conflicting evidence: %#v", storage)
	}
}

func TestUtilityPersistenceFailureDoesNotAcknowledgeOrMutateClaim(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "utility.ndjson")
	t.Setenv("GO_UTILITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_UTILITY_LEDGER_PATH", ledgerPath)
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", filepath.Join(t.TempDir(), "context-pack-quality.ndjson"))
	telemetry := newUtilityTestTelemetry(t, 20)
	if err := os.Remove(ledgerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove utility file before failure injection: %v", err)
	}
	if err := os.Mkdir(ledgerPath, 0o700); err != nil {
		t.Fatalf("replace utility file with directory: %v", err)
	}
	outcome, quality, impact, events := utilityTestFixture("outcome_persistence_failure", "sample_persistence_failure", "session_persistence_failure", "coding", "contextlattice", 4, 250, nil)
	row := buildUtilityObservation(outcome, quality, impact, events)
	if _, recorded, err := telemetry.record(row); !errors.Is(err, errUtilityPersistenceUnavailable) || recorded {
		t.Fatalf("persistence failure was acknowledged: recorded=%t err=%v", recorded, err)
	}
	if _, exists := telemetry.observation("outcome_persistence_failure"); exists {
		t.Fatal("persistence failure mutated the in-memory Utility Ledger")
	}
	qualityTelemetry := newContextPackQualityTelemetry(20)
	qualityTelemetry.recordQuality(quality)
	impactTelemetry := newTokenImpactTelemetry(20)
	impactTelemetry.record(impact)
	s := &server{contextPackQuality: qualityTelemetry, tokenImpact: impactTelemetry, utility: telemetry}
	raw, _ := json.Marshal(outcome)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(raw))
	s.telemetryContextPackQualityOutcomeRoute(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "utility_persistence_unavailable") {
		t.Fatalf("HTTP claim falsely acknowledged persistence: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestUtilityCommittedAtomicFailureLatchesFailClosedAndBindsUncertainBytes(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "utility.ndjson")
	t.Setenv("GO_UTILITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_UTILITY_LEDGER_PATH", ledgerPath)
	telemetry := newUtilityTestTelemetry(t, 20)
	telemetry.store.writeFile = func(path string, content []byte, dedicatedParent bool) error {
		if err := writeOwnerOnlyDurableAtomicFile(path, content, dedicatedParent); err != nil {
			return err
		}
		return errors.New("injected utility post-commit failure")
	}
	outcome, quality, impact, events := utilityTestFixture("outcome_sync_failure", "sample_sync_failure", "session_sync_failure", "coding", "contextlattice", 4, 250, nil)
	row := buildUtilityObservation(outcome, quality, impact, events)
	if _, recorded, err := telemetry.record(row); !errors.Is(err, errUtilityPersistenceUnavailable) || recorded {
		t.Fatalf("committed atomic failure was acknowledged: recorded=%t err=%v", recorded, err)
	}
	if _, exists := telemetry.observation("outcome_sync_failure"); exists {
		t.Fatal("committed atomic failure mutated acknowledged Utility Ledger state")
	}
	storage := utilityStorageStatus(telemetry.store)
	if !anyToBool(storage["configured"]) || anyToBool(storage["enabled"]) || anyToString(storage["durability"]) != "unavailable" || anyToString(storage["last_error"]) == "" {
		t.Fatalf("committed uncertainty did not latch the ledger closed: %#v", storage)
	}
	stat, err := os.Stat(ledgerPath)
	if err != nil || stat.Size() == 0 {
		t.Fatalf("failure injection did not create the uncertain-byte condition: stat=%#v err=%v", stat, err)
	}
	conflictingOutcome := cloneAnyMap(outcome)
	anyMap(conflictingOutcome["utility"])["value"] = 9
	conflicting := buildUtilityObservation(conflictingOutcome, quality, impact, events)
	if _, recorded, err := telemetry.record(conflicting); !errors.Is(err, errUtilityPersistenceUnavailable) || recorded {
		t.Fatalf("latched store accepted a conflicting retry: recorded=%t err=%v", recorded, err)
	}

	telemetry.store.close()
	reloaded := newUtilityTestTelemetry(t, 20)
	durable, ok := reloaded.observation("outcome_sync_failure")
	if !ok || anyToFloat(anyMap(durable["utility"])["value"]) != 4 {
		t.Fatalf("restart did not bind uncertain bytes to the first source claim: %#v", durable)
	}
	if _, recorded, err := reloaded.record(conflicting); !errors.Is(err, errUtilityOutcomeConflict) || recorded {
		t.Fatalf("restart accepted a conflicting claim over uncertain bytes: recorded=%t err=%v", recorded, err)
	}
}

func TestUtilityVerificationEventReportsDurableReconciliationFailureAsPartial(t *testing.T) {
	t.Setenv("GO_UTILITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_UTILITY_LEDGER_PATH", filepath.Join(t.TempDir(), "utility.ndjson"))
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))
	defer backend.Close()
	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	status, started := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/start", `{"session_id":"session_reconciliation_failure","agent":"codex","agent_id":"codex_test","project":"contextlattice","objective":"test utility reconciliation failure"}`)
	if status != http.StatusOK || !anyToBool(started["ok"]) {
		t.Fatalf("start verification session: status=%d payload=%#v", status, started)
	}
	outcome, quality, impact, events := utilityTestFixture("outcome_reconciliation_failure", "sample_reconciliation_failure", "session_reconciliation_failure", "coding", "contextlattice", 4, 250, nil)
	if _, recorded, err := s.utility.record(buildUtilityObservation(outcome, quality, impact, nil)); err != nil || !recorded {
		t.Fatalf("record claim awaiting verification: recorded=%t err=%v", recorded, err)
	}
	s.utility.store.mu.Lock()
	s.utility.store.enabled = false
	s.utility.store.lastError = "io_error"
	s.utility.store.mu.Unlock()
	eventRaw, _ := json.Marshal(events[0])
	status, response := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/event", string(eventRaw))
	reconciliation := anyMap(response["utility_reconciliation"])
	if status != http.StatusOK || anyToBool(response["ok"]) || !anyToBool(response["partial"]) || !anyToBool(response["event_recorded"]) {
		t.Fatalf("verification route hid partial persistence failure: status=%d payload=%#v", status, response)
	}
	if anyToBool(reconciliation["ok"]) || anyToString(reconciliation["status"]) != "persistence_unavailable" {
		t.Fatalf("verification reconciliation failure was ambiguous: %#v", reconciliation)
	}
}

func TestUtilityInitializationFailureRemainsFailClosed(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("blocked"), 0o600); err != nil {
		t.Fatalf("create path blocker: %v", err)
	}
	t.Setenv("GO_UTILITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_UTILITY_LEDGER_PATH", filepath.Join(blocker, "utility.ndjson"))
	telemetry := newUtilityTestTelemetry(t, 20)
	outcome, quality, impact, events := utilityTestFixture("outcome_init_failure", "sample_init_failure", "session_init_failure", "coding", "contextlattice", 4, 250, nil)
	if _, recorded, err := telemetry.record(buildUtilityObservation(outcome, quality, impact, events)); !errors.Is(err, errUtilityPersistenceUnavailable) || recorded {
		t.Fatalf("initialization failure was acknowledged: recorded=%t err=%v", recorded, err)
	}
	storage := utilityStorageStatus(telemetry.store)
	if !anyToBool(storage["configured"]) || anyToBool(storage["enabled"]) || anyToString(storage["durability"]) != "unavailable" || anyToString(storage["last_error"]) == "" {
		t.Fatalf("initialization failure state is ambiguous: %#v", storage)
	}
}

func TestUtilityExplicitDisableStopsDerivedRecording(t *testing.T) {
	t.Setenv("GO_UTILITY_LEDGER_ENABLED", "false")
	telemetry := newUtilityTestTelemetry(t, 20)
	outcome, quality, impact, events := utilityTestFixture("outcome_disabled", "sample_disabled", "session_disabled", "coding", "contextlattice", 4, 250, nil)
	if row, recorded, err := telemetry.record(buildUtilityObservation(outcome, quality, impact, events)); err != nil || recorded || row != nil {
		t.Fatalf("disabled Utility Ledger recorded a derived claim: row=%#v recorded=%t err=%v", row, recorded, err)
	}
	if rows := telemetry.rows(utilityQuery{}); len(rows) != 0 {
		t.Fatalf("disabled Utility Ledger retained observations: %#v", rows)
	}
	storage := utilityStorageStatus(telemetry.store)
	if anyToBool(storage["configured"]) || anyToBool(storage["enabled"]) || anyToString(storage["durability"]) != "disabled" {
		t.Fatalf("disabled Utility Ledger reported active storage: %#v", storage)
	}
}

func TestUtilityLedgerEnforcesPhysicalSampleAndByteBounds(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "utility.ndjson")
	t.Setenv("GO_UTILITY_LEDGER_PATH", ledgerPath)
	t.Setenv("GO_UTILITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_UTILITY_LEDGER_MAX_SAMPLES", "20")
	t.Setenv("GO_UTILITY_LEDGER_MAX_BYTES", "65536")
	telemetry := newUtilityTestTelemetry(t, 100)
	for index := 0; index < 25; index++ {
		outcomeID := "bounded_" + strconv.Itoa(index)
		outcome, quality, impact, events := utilityTestFixture(outcomeID, "sample_"+outcomeID, "session_"+outcomeID, "coding", "contextlattice", 1, 200, nil)
		if _, recorded, err := telemetry.record(buildUtilityObservation(outcome, quality, impact, events)); err != nil || !recorded {
			t.Fatalf("record bounded row %d", index)
		}
	}
	raw, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read bounded utility ledger: %v", err)
	}
	if lines := strings.Count(strings.TrimSpace(string(raw)), "\n") + 1; lines > 20 {
		t.Fatalf("physical utility ledger exceeded max samples: lines=%d", lines)
	}
	oversized := map[string]any{
		"schema_id": utilityObservationContractID, "revision": 1, "outcome_id": "oversized",
		"measurement_limit": strings.Repeat("x", 70_000),
	}
	if _, _, err := telemetry.store.append(oversized); err == nil {
		t.Fatal("oversized utility observation must fail before append")
	}
	stat, err := os.Stat(ledgerPath)
	if err != nil || stat.Size() > 65536 {
		t.Fatalf("utility ledger exceeded configured byte bound: stat=%#v err=%v", stat, err)
	}
}

func TestVerificationEventsRemainAllowedAfterTerminalSession(t *testing.T) {
	if !agentSessionAllowsPostTerminalEvent("verification.completed") {
		t.Fatal("verification receipts must remain appendable after terminal work")
	}
}

func TestUtilityOutcomeBeforeVerificationEventReconcilesInPlace(t *testing.T) {
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "false")
	t.Setenv("GO_TOKEN_IMPACT_LEDGER_ENABLED", "false")
	t.Setenv("GO_UTILITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_UTILITY_LEDGER_PATH", filepath.Join(t.TempDir(), "utility.ndjson"))
	outcome, quality, impact, events := utilityTestFixture("outcome_late_proof", "sample_late_proof", "session_late_proof", "coding", "contextlattice", 6, 300, nil)
	qualityTelemetry := newContextPackQualityTelemetry(20)
	qualityTelemetry.recordQuality(quality)
	impact["baseline_tokens_estimate"] = 900
	impact["packed_tokens_estimate"] = 300
	impact["saved_tokens_estimate"] = 600
	impact["transport_inclusive"] = true
	impact["transport_tokens_exact"] = 500
	impactTelemetry := newTokenImpactTelemetry(20)
	impactTelemetry.record(impact)
	s := &server{
		contextPackQuality: qualityTelemetry,
		tokenImpact:        impactTelemetry,
		utility:            newUtilityTestTelemetry(t, 20),
	}
	row, recorded, err := s.recordUtilityOutcome(outcome)
	if err != nil || !recorded || anyToString(row["status"]) != "excluded" {
		t.Fatalf("outcome must be retained while late verification is pending: recorded=%t row=%#v", recorded, row)
	}
	s.recordUtilitySessionEvent(map[string]any{"id": "session_late_proof", "agent_id": "codex_test"}, events[0])
	reconciled, ok := s.utility.observation("outcome_late_proof")
	if !ok || anyToString(reconciled["status"]) != "verified_exact" || !anyToBool(anyMap(reconciled["utility"])["independently_verified"]) || anyToInt(reconciled["revision"], 0) != 2 {
		t.Fatalf("late verification did not reconcile the durable observation: %#v", reconciled)
	}
}

func TestUtilityLedgerCoreRouteCarriesPassingContract(t *testing.T) {
	t.Setenv("GO_UTILITY_LEDGER_ENABLED", "false")
	s := &server{utility: newUtilityTestTelemetry(t, 100)}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, utilityTelemetryPath, nil)
	s.telemetryUtilityRoute(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("utility route status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	payload, err := parseJSONMap(recorder.Body.Bytes())
	if err != nil {
		t.Fatalf("decode utility response: %v", err)
	}
	validation := anyMap(anyMap(payload["format_contract"])["validation"])
	if anyToString(validation["status"]) != "passed" {
		t.Fatalf("utility ledger contract failed: %#v", payload["format_contract"])
	}
}

func TestUtilityExactTokenProducersDoNotSubstituteEstimates(t *testing.T) {
	referencePrompt := "Use this exact bounded evidence packet."
	impact := buildContextPackTokenImpact(
		"test utility accounting",
		map[string]any{"facts": []any{"one verified fact"}, "source_coverage": map[string]any{"returned": []any{"ledger"}}},
		map[string]any{
			"context_compiler": map[string]any{"ranked_evidence_count": 1},
			"token_budget":     map[string]any{"used_tokens_estimate": 10000, "active": true},
			"ranked_evidence":  []any{map[string]any{"kind": "check"}},
		},
		referencePrompt,
	)
	wantModelVisible := contextPackCountTokens(referencePrompt).Tokens
	if got := anyToInt(impact["model_visible_context_tokens_exact"], 0); got != wantModelVisible {
		t.Fatalf("model-visible exact count was replaced by a budget estimate: got=%d want=%d impact=%#v", got, wantModelVisible, impact)
	}
	if anyToInt(impact["packed_tokens_estimate"], 0) <= wantModelVisible {
		t.Fatalf("fixture must prove estimated and exact denominators remain distinct: %#v", impact)
	}

	payload := map[string]any{"token_impact": impact}
	applyTransportTokenImpact(payload, tokenCountResult{Tokens: 90, Method: "tiktoken", CalibrationGrade: "tokenizer_exact", Encoding: "o200k_base", TokenizerExact: true}, "test", "test_json")
	transport := anyMap(payload["token_impact"])
	if anyToInt(transport["wire_tokens_exact"], 0) != 90 || anyToInt(transport["model_visible_context_tokens_exact"], 0) != wantModelVisible {
		t.Fatalf("transport accounting collapsed exact denominators: %#v", transport)
	}

	delta := map[string]any{}
	exact := func(tokens int) tokenCountResult {
		return tokenCountResult{Tokens: tokens, Method: "tiktoken", CalibrationGrade: "tokenizer_exact", Encoding: "o200k_base", TokenizerExact: true}
	}
	applyAgentPacketDeltaTokenMetadata(delta, exact(1000), exact(700), exact(120), exact(240), map[string]any{"sample_id": "sample_delta"})
	deltaImpact := anyMap(delta["token_impact"])
	if anyToInt(deltaImpact["wire_tokens_exact"], 0) != 240 || anyToInt(deltaImpact["model_visible_context_tokens_exact"], 0) != 120 || anyToInt(deltaImpact["reconstructed_model_visible_tokens_exact"], 0) != 700 {
		t.Fatalf("delta exact accounting is incomplete: %#v", deltaImpact)
	}
}
