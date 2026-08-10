package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func contextPackOutcomeResponseBindingFixture(t *testing.T, sampleID string) (map[string]any, map[string]any) {
	t.Helper()
	response := composeRecallResponse(recallResponseTestInput(true))
	binding, ok := recallResponseBindingFromResponse(response)
	if !ok {
		t.Fatalf("response fixture did not produce a canonical binding: %#v", response)
	}
	sample := contextPackPersistenceTestQualitySample()
	sample["sample_id"] = sampleID
	if !recallResponseCopyBinding(sample, binding) {
		t.Fatalf("failed to copy response binding into quality fixture")
	}
	return sample, binding
}

func contextPackOutcomeResponseBindingTelemetry(t *testing.T, sample map[string]any) (*contextPackQualityTelemetry, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "quality.ndjson")
	ledger := &contextPackQualityLedger{
		enabled: true, path: path, maxBytes: 2 * 1024 * 1024, maxSamples: 20,
		writeFile: writeOwnerOnlyDurableAtomicFile,
	}
	if err := prepareOwnerOnlyFile(path, true); err != nil {
		t.Fatalf("prepare quality ledger: %v", err)
	}
	telemetry := newContextPackQualityTelemetryWithLedger(20, ledger)
	if err := telemetry.recordQualityDurably(sample); err != nil {
		t.Fatalf("persist canonical quality sample: %v", err)
	}
	return telemetry, path
}

func contextPackOutcomeResponseBindingPayload(sampleID, outcomeID string) map[string]any {
	return map[string]any{
		"outcome_id": outcomeID, "sample_id": sampleID, "project": "contextlattice",
		"first_pass_success": true, "query": "raw query must not cross outcome ingress",
		"prompt": "raw prompt must not cross outcome ingress", "content": "raw content must not cross outcome ingress",
		"workspace_ref": "caller-workspace-must-not-win",
	}
}

func TestContextPackOutcomeResponseBindingPropagatesAndRejectsInvalidClaims(t *testing.T) {
	sample, binding := contextPackOutcomeResponseBindingFixture(t, "cpq_response_binding_unit")
	canonical := contextPackQualityEntryFromSample(sample)
	if len(canonical) == 0 {
		t.Fatal("canonical quality sample was not normalized")
	}

	legacyEntry, err := contextPackQualityOutcomeFromSampleChecked(map[string]any{
		"outcome_id": "cpo_response_binding_legacy", "sample_id": sample["sample_id"], "first_pass_success": true,
	})
	if err != nil {
		t.Fatalf("legacy reporter omission was rejected: %v", err)
	}
	if recallResponseBindingHasAnyFields(legacyEntry) {
		t.Fatalf("legacy reporter omission unexpectedly gained a binding before canonical admission: %#v", legacyEntry)
	}
	bound, err := bindContextPackQualityOutcomeSample(legacyEntry, canonical)
	if err != nil || !recallResponseBindingsEqual(bound, binding) {
		t.Fatalf("canonical binding was not copied to an omitted reporter binding: bound=%#v err=%v", bound, err)
	}
	if contextPackOutcomeLogicalClaimDigest(legacyEntry) == contextPackOutcomeLogicalClaimDigest(bound) || utilitySourceClaimDigest(legacyEntry) == utilitySourceClaimDigest(bound) {
		t.Fatalf("response binding did not participate in logical/source claim identity: legacy=%q bound=%q", contextPackOutcomeLogicalClaimDigest(legacyEntry), contextPackOutcomeLogicalClaimDigest(bound))
	}

	exactReporter := map[string]any{
		"outcome_id": "cpo_response_binding_exact", "sample_id": sample["sample_id"], "first_pass_success": true,
	}
	if !recallResponseCopyBinding(exactReporter, binding) {
		t.Fatal("failed to build exact reporter binding")
	}
	exactEntry, err := contextPackQualityOutcomeFromSampleChecked(exactReporter)
	if err != nil {
		t.Fatalf("exact reporter binding was rejected: %v", err)
	}
	if bound, err := bindContextPackQualityOutcomeSample(exactEntry, canonical); err != nil || !recallResponseBindingsEqual(bound, binding) {
		t.Fatalf("exact reporter binding was not accepted: bound=%#v err=%v", bound, err)
	}

	partial := map[string]any{
		"outcome_id": "cpo_response_binding_partial", "sample_id": sample["sample_id"], "first_pass_success": true,
		"recall_response_id": binding["recall_response_id"],
	}
	if entry, err := contextPackQualityOutcomeFromSampleChecked(partial); !errors.Is(err, errContextPackOutcomeInvalidResponseBinding) || len(entry) != 0 {
		t.Fatalf("partial reporter binding was not rejected by the dedicated validation error: entry=%#v err=%v", entry, err)
	}

	malformed := map[string]any{
		"outcome_id": "cpo_response_binding_malformed", "sample_id": sample["sample_id"], "first_pass_success": true,
	}
	if !recallResponseCopyBinding(malformed, binding) {
		t.Fatal("failed to build malformed reporter binding")
	}
	malformed["recall_response_digest"] = "not-a-digest"
	if entry, err := contextPackQualityOutcomeFromSampleChecked(malformed); !errors.Is(err, errContextPackOutcomeInvalidResponseBinding) || len(entry) != 0 {
		t.Fatalf("malformed reporter binding was not rejected: entry=%#v err=%v", entry, err)
	}

	otherResponse := composeRecallResponse(recallResponseTestInput(false))
	otherBinding, ok := recallResponseBindingFromResponse(otherResponse)
	if !ok {
		t.Fatal("mismatch response fixture did not produce a binding")
	}
	mismatch := map[string]any{
		"outcome_id": "cpo_response_binding_mismatch", "sample_id": sample["sample_id"], "first_pass_success": true,
	}
	if !recallResponseCopyBinding(mismatch, otherBinding) {
		t.Fatal("failed to build mismatched reporter binding")
	}
	mismatchEntry, err := contextPackQualityOutcomeFromSampleChecked(mismatch)
	if err != nil {
		t.Fatalf("mismatch fixture was rejected before binding comparison: %v", err)
	}
	if _, err := bindContextPackQualityOutcomeSample(mismatchEntry, canonical); !errors.Is(err, errContextPackOutcomeInvalidResponseBinding) {
		t.Fatalf("mismatched reporter binding was not rejected: %v", err)
	}

	legacyCanonical := contextPackQualityEntryFromSample(func() map[string]any {
		unbound := cloneJSONMap(sample)
		for _, key := range []string{"recall_response_id", "recall_response_digest", "response_component_refs"} {
			delete(unbound, key)
		}
		return unbound
	}())
	if _, err := bindContextPackQualityOutcomeSample(legacyEntry, legacyCanonical); err != nil {
		t.Fatalf("both unbound legacy rows changed behavior: %v", err)
	}
	if _, err := bindContextPackQualityOutcomeSample(exactEntry, legacyCanonical); !errors.Is(err, errContextPackOutcomeInvalidResponseBinding) {
		t.Fatalf("response binding attached to an unbound legacy sample: %v", err)
	}

	malformedCanonical := cloneJSONMap(canonical)
	malformedCanonical["recall_response_id"] = "rr_malformed"
	if _, err := bindContextPackQualityOutcomeSample(legacyEntry, malformedCanonical); !errors.Is(err, errContextPackOutcomeInvalidResponseBinding) {
		t.Fatalf("malformed canonical binding was not rejected: %v", err)
	}
}

func TestContextPackOutcomeResponseBindingRouteStatusAndReplay(t *testing.T) {
	sample, binding := contextPackOutcomeResponseBindingFixture(t, "cpq_response_binding_route")
	telemetry, path := contextPackOutcomeResponseBindingTelemetry(t, sample)
	s := &server{contextPackQuality: telemetry}
	post := func(payload map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal outcome payload: %v", err)
		}
		recorder := httptest.NewRecorder()
		s.telemetryContextPackQualityOutcomeRoute(recorder, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(raw)))
		return recorder
	}

	omitted := contextPackOutcomeResponseBindingPayload("cpq_response_binding_route", "cpo_response_binding_route")
	first := post(omitted)
	if first.Code != http.StatusOK {
		t.Fatalf("canonical-bound reporter omission was rejected: status=%d body=%s", first.Code, first.Body.String())
	}
	result, err := parseJSONMap(first.Body.Bytes())
	if err != nil {
		t.Fatalf("parse accepted outcome response: %v", err)
	}
	stored := anyMap(result["outcome"])
	if !recallResponseBindingsEqual(stored, binding) {
		t.Fatalf("accepted outcome did not retain canonical binding: %#v", stored)
	}
	if strings.Contains(first.Body.String(), "raw query must not cross") || strings.Contains(first.Body.String(), "raw prompt must not cross") || strings.Contains(first.Body.String(), "raw content must not cross") || strings.Contains(first.Body.String(), "caller-workspace-must-not-win") {
		t.Fatalf("raw/internal reporter fields leaked into outcome response: %s", first.Body.String())
	}

	replay := post(omitted)
	if replay.Code != http.StatusOK {
		t.Fatalf("same exact outcome replay was not accepted: status=%d body=%s", replay.Code, replay.Body.String())
	}
	replayResult, err := parseJSONMap(replay.Body.Bytes())
	if err != nil || !anyToBool(replayResult["duplicate"]) {
		t.Fatalf("same exact replay was not marked duplicate: result=%#v err=%v", replayResult, err)
	}

	partial := contextPackOutcomeResponseBindingPayload("cpq_response_binding_route", "cpo_response_binding_partial_route")
	partial["recall_response_id"] = binding["recall_response_id"]
	response := post(partial)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "invalid_outcome_response_binding") {
		t.Fatalf("partial reporter binding did not produce bounded 422: status=%d body=%s", response.Code, response.Body.String())
	}

	otherResponse := composeRecallResponse(recallResponseTestInput(false))
	otherBinding, ok := recallResponseBindingFromResponse(otherResponse)
	if !ok {
		t.Fatal("mismatch response fixture did not produce a binding")
	}
	mismatch := contextPackOutcomeResponseBindingPayload("cpq_response_binding_route", "cpo_response_binding_mismatch_route")
	if !recallResponseCopyBinding(mismatch, otherBinding) {
		t.Fatal("failed to attach mismatch binding")
	}
	response = post(mismatch)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "invalid_outcome_response_binding") {
		t.Fatalf("mismatched binding did not produce bounded 422: status=%d body=%s", response.Code, response.Body.String())
	}

	legacySample := cloneJSONMap(sample)
	for _, key := range []string{"recall_response_id", "recall_response_digest", "response_component_refs"} {
		delete(legacySample, key)
	}
	legacyTelemetry, _ := contextPackOutcomeResponseBindingTelemetry(t, legacySample)
	legacyServer := &server{contextPackQuality: legacyTelemetry}
	legacyPayload := contextPackOutcomeResponseBindingPayload("cpq_response_binding_route", "cpo_response_binding_legacy_attachment")
	if !recallResponseCopyBinding(legacyPayload, binding) {
		t.Fatal("failed to attach binding to legacy payload")
	}
	raw, err := json.Marshal(legacyPayload)
	if err != nil {
		t.Fatal(err)
	}
	legacyResponse := httptest.NewRecorder()
	legacyServer.telemetryContextPackQualityOutcomeRoute(legacyResponse, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(raw)))
	if legacyResponse.Code != http.StatusUnprocessableEntity || !strings.Contains(legacyResponse.Body.String(), "invalid_outcome_response_binding") {
		t.Fatalf("binding attached to legacy sample did not produce bounded 422: status=%d body=%s", legacyResponse.Code, legacyResponse.Body.String())
	}

	rows, _, err := telemetry.ledger.readRows()
	if err != nil || len(rows) != 2 {
		t.Fatalf("durable quality/outcome rows missing after accepted route: rows=%#v err=%v", rows, err)
	}
	var outcomeRow map[string]any
	for _, row := range rows {
		if anyToString(row["schema_id"]) == contextPackQualityOutcomeSchemaID {
			outcomeRow = row
		}
	}
	if outcomeRow == nil || !recallResponseBindingsEqual(outcomeRow, binding) {
		t.Fatalf("durable outcome row lost response binding: %#v", outcomeRow)
	}

	stat, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat quality ledger: %v", err)
	}
	outcomeRaw, err := json.Marshal(outcomeRow)
	if err != nil {
		t.Fatal(err)
	}
	// Leave enough room for the canonical quality row and both identical outcome
	// rows while still forcing the append-triggered rewrite boundary.
	telemetry.ledger.maxBytes = stat.Size() + int64(len(outcomeRaw)) + 2
	if err := telemetry.ledger.append(outcomeRow); err != nil {
		t.Fatalf("force durable compaction with bound outcome: %v", err)
	}
	restarted := newContextPackQualityTelemetryWithLedger(20, &contextPackQualityLedger{
		enabled: true, path: path, maxBytes: 2 * 1024 * 1024, maxSamples: 20,
		writeFile: writeOwnerOnlyDurableAtomicFile,
	})
	loadedRows := restarted.outcomeSourceRows(10)
	if len(loadedRows) != 1 || !recallResponseBindingsEqual(loadedRows[0], binding) {
		t.Fatalf("restart/compaction did not preserve bound outcome: rows=%#v", loadedRows)
	}
}

func TestContextPackOutcomeResponseBindingMalformedDurableRowsFailClosed(t *testing.T) {
	_, binding := contextPackOutcomeResponseBindingFixture(t, "cpq_response_binding_malformed_durable")
	row := map[string]any{
		"schema_id": contextPackQualityOutcomeSchemaID, "version": 1,
		"capturedAt": nowUTCISO(), "outcome_id": "cpo_malformed_durable", "sample_id": "cpq_response_binding_malformed_durable",
		"project": "contextlattice", "task_id": "", "task_class": "", "retrieval_intent": "",
		"first_pass_success": true, "repair_required": false, "retry_count": 0, "observed_followup_tokens": 0,
		"outcome_source": "agent_report", "outcome_class": "success", "context_attribution": "unknown", "calibration_eligible": true,
		"recall_response_id": binding["recall_response_id"],
	}
	path := filepath.Join(t.TempDir(), "quality.ndjson")
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger := &contextPackQualityLedger{enabled: true, path: path, maxBytes: 2 * 1024 * 1024, maxSamples: 20, writeFile: writeOwnerOnlyDurableAtomicFile}
	rows, parseErrors, err := ledger.readRows()
	if err != nil || len(rows) != 0 || parseErrors != 1 {
		t.Fatalf("malformed response-bound outcome was not rejected by durable reader: rows=%#v parse_errors=%d err=%v", rows, parseErrors, err)
	}
	restarted := newContextPackQualityTelemetryWithLedger(20, ledger)
	if len(restarted.outcomes) != 0 || restarted.calibrationOutcomeCount != 0 {
		t.Fatalf("malformed response-bound outcome became resident/calibration eligible: outcomes=%#v calibration=%d", restarted.outcomes, restarted.calibrationOutcomeCount)
	}
}

func contextPackLegacyResponseBindingRow(t *testing.T, sampleID string) map[string]any {
	t.Helper()
	sample, _ := contextPackOutcomeResponseBindingFixture(t, sampleID)
	row := contextPackQualityEntryFromSample(sample)
	if len(row) == 0 {
		t.Fatal("canonical quality fixture was not normalized")
	}
	legacyRefs := make([]any, 0)
	for _, raw := range contextPackAnyList(row["response_component_refs"]) {
		ref := anyMap(raw)
		legacyRefs = append(legacyRefs, map[string]any{
			"component_ref":    ref["component_ref"],
			"component_digest": ref["component_digest"],
			"ordinal":          ref["ordinal"],
			"kind":             ref["kind"],
		})
	}
	row["response_component_refs"] = legacyRefs
	if contextPackQualityRowBindingValid(row) || !contextPackQualityLegacyResponseBindingRetirable(row) {
		t.Fatalf("fixture is not the exact retirable legacy binding shape: %#v", row["response_component_refs"])
	}
	return row
}

func TestContextPackQualityLegacyResponseBindingMigrationRetainsQualityAndReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quality.ndjson")
	legacy := contextPackLegacyResponseBindingRow(t, "cpq_legacy_response_binding")
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	ledger := &contextPackQualityLedger{
		enabled: true, path: path, maxBytes: 2 * 1024 * 1024, maxSamples: 20,
		writeFile: writeOwnerOnlyDurableAtomicFile,
	}
	restarted := newContextPackQualityTelemetryWithLedger(20, ledger)
	if !contextPackQualityLedgerAvailable(ledger) {
		t.Fatalf("legacy migration disabled the quality ledger: %#v", contextPackQualityLedgerPublicStatus(ledger))
	}
	rows, parseErrors, err := ledger.readRows()
	if err != nil || parseErrors != 0 || len(rows) != 1 {
		t.Fatalf("migrated quality row was not retained: rows=%#v parse_errors=%d err=%v", rows, parseErrors, err)
	}
	migrated := rows[0]
	if recallResponseBindingHasAnyFields(migrated) {
		t.Fatalf("incomplete legacy response binding survived migration: %#v", migrated["response_component_refs"])
	}
	if len(contextPackSelectionReceiptFromSample(migrated["selection_receipt"])) == 0 {
		t.Fatal("legacy migration removed the durable selection receipt")
	}
	if anyToString(migrated["sample_id"]) != anyToString(legacy["sample_id"]) ||
		anyToFloat(migrated["quality_score"]) != anyToFloat(legacy["quality_score"]) {
		t.Fatalf("legacy migration changed quality identity or measurement: before=%#v after=%#v", legacy, migrated)
	}
	loaded, found := restarted.sampleForUtility(anyToString(legacy["sample_id"]))
	if !found || recallResponseBindingHasAnyFields(loaded) || len(contextPackSelectionReceiptFromSample(loaded["selection_receipt"])) == 0 {
		t.Fatalf("migrated row was not usable as unbound quality evidence: found=%v row=%#v", found, loaded)
	}
}

func TestContextPackQualityLegacyResponseBindingMigrationRejectsDependentOutcome(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quality.ndjson")
	legacy := contextPackLegacyResponseBindingRow(t, "cpq_legacy_response_binding_linked")
	outcome, err := contextPackQualityOutcomeFromSampleChecked(map[string]any{
		"outcome_id": "cpo_legacy_response_binding_linked", "sample_id": legacy["sample_id"],
		"project": "contextlattice", "first_pass_success": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	qualityRaw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	outcomeRaw, err := json.Marshal(outcome)
	if err != nil {
		t.Fatal(err)
	}
	before := append(append(append([]byte(nil), qualityRaw...), '\n'), outcomeRaw...)
	before = append(before, '\n')
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}

	ledger := &contextPackQualityLedger{
		enabled: true, path: path, maxBytes: 2 * 1024 * 1024, maxSamples: 20,
		writeFile: writeOwnerOnlyDurableAtomicFile,
	}
	restarted := newContextPackQualityTelemetryWithLedger(20, ledger)
	if ledger.enabled || anyToString(contextPackQualityLedgerPublicStatus(ledger)["last_error"]) != "privacy_migration_failed" {
		t.Fatalf("dependent legacy outcome did not fail closed: %#v", contextPackQualityLedgerPublicStatus(ledger))
	}
	if len(restarted.samples) != 0 || len(restarted.outcomes) != 0 {
		t.Fatalf("rejected legacy binding became resident: samples=%#v outcomes=%#v", restarted.samples, restarted.outcomes)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed-closed legacy migration changed durable bytes")
	}
}
