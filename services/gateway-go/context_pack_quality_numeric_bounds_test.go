package main

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContextPackOutcomeNumericClaimsAreStrictAndBounded(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value any
	}{
		{name: "string", field: "retry_count", value: "1"},
		{name: "boolean", field: "retry_count", value: true},
		{name: "fraction", field: "observed_followup_tokens", value: 1.25},
		{name: "nan", field: "observed_followup_tokens", value: math.NaN()},
		{name: "positive infinity", field: "observed_followup_tokens", value: math.Inf(1)},
		{name: "negative infinity", field: "observed_followup_tokens", value: math.Inf(-1)},
		{name: "json nan", field: "observed_followup_tokens", value: json.Number("NaN")},
		{name: "retry over per outcome maximum", field: "retry_count", value: contextPackOutcomeMaxRetryCount + 1},
		{name: "followup over per outcome maximum", field: "observed_followup_tokens", value: contextPackOutcomeMaxFollowupTokens + 1},
		{name: "provider uint overflow", field: "provider_total_tokens", value: uint64(math.MaxInt64) + 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sample := map[string]any{
				"outcome_id":         "outcome_numeric_" + tt.name,
				"sample_id":          "sample_numeric_" + tt.name,
				"first_pass_success": true,
				tt.field:             tt.value,
			}
			if entry, err := contextPackQualityOutcomeFromSampleChecked(sample); !errorsIsContextPackOutcomeInvalidNumeric(err) || len(entry) != 0 {
				t.Fatalf("invalid numeric claim was normalized: entry=%#v err=%v", entry, err)
			}
			if entry := contextPackQualityOutcomeFromSample(sample); len(entry) != 0 {
				t.Fatalf("compatibility wrapper admitted invalid numeric claim: %#v", entry)
			}
		})
	}
}

func errorsIsContextPackOutcomeInvalidNumeric(err error) bool {
	return err == errContextPackOutcomeInvalidNumeric
}

func TestContextPackOutcomeProviderCountsAreConsistentAndDeriveOnlyWhenAbsent(t *testing.T) {
	derived, err := contextPackQualityOutcomeFromSampleChecked(map[string]any{
		"outcome_id": "outcome_provider_derived", "sample_id": "sample_provider_derived", "first_pass_success": true,
		"provider_usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 7},
	})
	if err != nil {
		t.Fatalf("derive provider total when absent: %v", err)
	}
	if got := anyToInt(derived["provider_total_tokens"], 0); got != 12 {
		t.Fatalf("derived provider total = %d, want 12: %#v", got, derived)
	}

	for _, providerUsage := range []map[string]any{
		{"prompt_tokens": 5, "completion_tokens": 7, "total_tokens": 11},
		{"prompt_tokens": 5, "completion_tokens": 7, "total_tokens": 13},
		{"prompt_tokens": 5, "total_tokens": 4},
		{"completion_tokens": 7, "total_tokens": 6},
	} {
		entry, err := contextPackQualityOutcomeFromSampleChecked(map[string]any{
			"outcome_id": "outcome_provider_inconsistent", "sample_id": "sample_provider_inconsistent", "first_pass_success": true,
			"provider_usage": providerUsage,
		})
		if !errorsIsContextPackOutcomeInvalidNumeric(err) || len(entry) != 0 {
			t.Fatalf("inconsistent provider counts were admitted: usage=%#v entry=%#v err=%v", providerUsage, entry, err)
		}
	}
}

func TestContextPackOutcomeNumericIngressFailsBeforeTelemetryOrUtility(t *testing.T) {
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", filepath.Join(t.TempDir(), "quality.ndjson"))
	t.Setenv("GO_UTILITY_LEDGER_ENABLED", "false")
	telemetry := newContextPackQualityTelemetry(20)
	utility := newUtilityTelemetry(20)
	s := &server{contextPackQuality: telemetry, utility: utility}

	invalidBodies := []string{
		`{"outcome_id":"route_string","sample_id":"route_string","first_pass_success":true,"retry_count":"1"}`,
		`{"outcome_id":"route_boolean","sample_id":"route_boolean","first_pass_success":true,"retry_count":true}`,
		`{"outcome_id":"route_fraction","sample_id":"route_fraction","first_pass_success":true,"observed_followup_tokens":1.25}`,
		`{"outcome_id":"route_oversized","sample_id":"route_oversized","first_pass_success":true,"provider_total_tokens":20000001}`,
		`{"outcome_id":"route_inconsistent","sample_id":"route_inconsistent","first_pass_success":true,"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":11}}`,
	}
	for _, body := range invalidBodies {
		recorder := httptest.NewRecorder()
		s.telemetryContextPackQualityOutcomeRoute(recorder, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewBufferString(body)))
		if got, want := recorder.Code, http.StatusUnprocessableEntity; got != want {
			t.Fatalf("invalid numeric ingress status = %d, want %d: %s", got, want, recorder.Body.String())
		}
	}
	if got := anyToInt(telemetry.snapshot()["outcome_sample_count"], -1); got != 0 {
		t.Fatalf("invalid numeric ingress inflated quality telemetry: %#v", telemetry.snapshot())
	}
	if len(utility.observations) != 0 {
		t.Fatalf("invalid numeric ingress reached Utility Ledger: %#v", utility.observations)
	}
	// The remaining assertions isolate quality replay/conflict behavior; the
	// invalid requests above already proved that Utility was never invoked.
	s.utility = nil

	qualitySample := buildContextPackQualitySample(contextPackQualitySampleInput{
		Project: "contextlattice", TaskID: "task_route_replay", SessionID: "session_route_replay",
	})
	telemetry.recordQuality(qualitySample)
	validBody, err := json.Marshal(map[string]any{
		"outcome_id": "route_replay", "sample_id": qualitySample["sample_id"], "first_pass_success": true,
		"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 7},
	})
	if err != nil {
		t.Fatal(err)
	}
	valid := string(validBody)
	for attempt := 0; attempt < 2; attempt++ {
		recorder := httptest.NewRecorder()
		s.telemetryContextPackQualityOutcomeRoute(recorder, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewBufferString(valid)))
		if got, want := recorder.Code, http.StatusOK; got != want {
			t.Fatalf("valid numeric replay %d status = %d, want %d: %s", attempt, got, want, recorder.Body.String())
		}
	}
	conflictBody, err := json.Marshal(map[string]any{
		"outcome_id": "route_replay", "sample_id": qualitySample["sample_id"], "first_pass_success": true,
		"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	conflict := string(conflictBody)
	recorder := httptest.NewRecorder()
	s.telemetryContextPackQualityOutcomeRoute(recorder, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewBufferString(conflict)))
	if got, want := recorder.Code, http.StatusConflict; got != want {
		t.Fatalf("changed numeric source claim status = %d, want %d: %s", got, want, recorder.Body.String())
	}
}

func TestContextPackOutcomeAggregatesSaturateWithoutWrapping(t *testing.T) {
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", filepath.Join(t.TempDir(), "quality.ndjson"))
	telemetry := newContextPackQualityTelemetry(20)
	telemetry.outcomeCount = math.MaxInt64 - 1
	telemetry.calibrationOutcomeCount = math.MaxInt64 - 1
	telemetry.firstPassSuccessCount = math.MaxInt64 - 1
	telemetry.totalRetryCount = math.MaxInt64 - 1
	telemetry.totalObservedFollowupTokens = math.MaxInt64 - 1
	telemetry.providerUsageCount = math.MaxInt64 - 1
	telemetry.totalProviderPromptTokens = math.MaxInt64 - 1
	telemetry.totalProviderCompletionTokens = math.MaxInt64 - 1
	telemetry.totalProviderTotalTokens = math.MaxInt64 - 1

	qualitySample := buildContextPackQualitySample(contextPackQualitySampleInput{
		Project: "contextlattice", TaskID: "task_saturating", SessionID: "session_saturating",
	})
	telemetry.recordQuality(qualitySample)
	entry, err := contextPackQualityOutcomeFromSampleChecked(map[string]any{
		"outcome_id": "outcome_saturating", "sample_id": qualitySample["sample_id"], "first_pass_success": true,
		"retry_count": 1, "observed_followup_tokens": 1,
		"provider_usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	entry, err = bindContextPackQualityOutcomeSample(entry, contextPackQualityEntryFromSample(qualitySample))
	if err != nil {
		t.Fatal(err)
	}
	if !telemetry.recordOutcomeEntry(entry) {
		t.Fatal("record bounded outcome at aggregate limit")
	}
	for name, got := range map[string]int64{
		"outcomes":             telemetry.outcomeCount,
		"calibration outcomes": telemetry.calibrationOutcomeCount,
		"first-pass outcomes":  telemetry.firstPassSuccessCount,
		"retry total":          telemetry.totalRetryCount,
		"followup total":       telemetry.totalObservedFollowupTokens,
		"provider uses":        telemetry.providerUsageCount,
		"provider prompt":      telemetry.totalProviderPromptTokens,
		"provider completion":  telemetry.totalProviderCompletionTokens,
		"provider total":       telemetry.totalProviderTotalTokens,
	} {
		if got != math.MaxInt64 {
			t.Fatalf("%s wrapped or failed to saturate: got %d want %d", name, got, math.MaxInt64)
		}
	}
	legacyNegative := int64(-4)
	contextPackOutcomeSaturatingAdd(&legacyNegative, 3)
	if legacyNegative != 3 {
		t.Fatalf("legacy negative aggregate did not recover safely: %d", legacyNegative)
	}
}

func TestContextPackOutcomeAdmissionRejectsUnknownAndReusedSamples(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "quality.ndjson")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", ledgerPath)
	telemetry := newContextPackQualityTelemetry(50)
	s := &server{contextPackQuality: telemetry}
	qualitySample := buildContextPackQualitySample(contextPackQualitySampleInput{
		Project: "contextlattice", TaskID: "task_admission", SessionID: "session_admission",
		TaskClass: "coding", RetrievalIntent: "balanced", AgentID: "codex_test",
	})
	telemetry.recordQuality(qualitySample)

	post := func(payload map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		s.telemetryContextPackQualityOutcomeRoute(recorder, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(body)))
		return recorder
	}
	for index := 0; index < 12; index++ {
		response := post(map[string]any{
			"outcome_id": "unknown_outcome_" + anyToString(index), "sample_id": "cpq_unknown_sample",
			"first_pass_success": true, "provider_total_tokens": 100,
		})
		if got, want := response.Code, http.StatusUnprocessableEntity; got != want {
			t.Fatalf("unknown sample attempt %d status=%d want=%d body=%s", index, got, want, response.Body.String())
		}
	}

	firstPayload := map[string]any{
		"outcome_id": "authoritative_outcome", "sample_id": qualitySample["sample_id"], "first_pass_success": true,
		"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 7},
	}
	if response := post(firstPayload); response.Code != http.StatusOK {
		t.Fatalf("authoritative outcome status=%d body=%s", response.Code, response.Body.String())
	}
	for index := 0; index < 12; index++ {
		response := post(map[string]any{
			"outcome_id": "reused_outcome_" + anyToString(index), "sample_id": qualitySample["sample_id"],
			"first_pass_success": true, "provider_total_tokens": 100,
		})
		if got, want := response.Code, http.StatusConflict; got != want {
			t.Fatalf("reused sample attempt %d status=%d want=%d body=%s", index, got, want, response.Body.String())
		}
	}
	if response := post(map[string]any{
		"outcome_id": "mismatched_scope", "sample_id": qualitySample["sample_id"], "project": "other_project", "first_pass_success": true,
	}); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("mismatched sample identity status=%d body=%s", response.Code, response.Body.String())
	}

	snapshot := telemetry.snapshot()
	if got := anyToInt(snapshot["outcome_sample_count"], 0); got != 1 {
		t.Fatalf("unknown/reused samples inflated outcome count: %#v", snapshot)
	}
	if got := anyToInt(snapshot["calibration_outcome_sample_count"], 0); got != 1 {
		t.Fatalf("unknown/reused samples inflated calibration count: %#v", snapshot)
	}
	if got := anyToInt(snapshot["observed_provider_total_tokens"], 0); got != 12 {
		t.Fatalf("unknown/reused samples inflated provider totals: %#v", snapshot)
	}
	raw, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "unknown_outcome_") || strings.Contains(string(raw), "reused_outcome_") || strings.Contains(string(raw), "mismatched_scope") {
		t.Fatalf("unadmitted outcome was persisted: %s", raw)
	}
	if got := len(strings.Split(strings.TrimSpace(string(raw)), "\n")); got != 2 {
		t.Fatalf("ledger rows=%d want quality plus one authoritative outcome: %s", got, raw)
	}
}

func TestContextPackOutcomeAdmissionRejectsEmptyProjectQualitySample(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "quality.ndjson")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", ledgerPath)
	telemetry := newContextPackQualityTelemetry(20)
	telemetry.recordQuality(map[string]any{"sample_id": "cpq_empty_project", "project": "", "quality_score": 1})
	s := &server{contextPackQuality: telemetry}
	body, err := json.Marshal(map[string]any{
		"outcome_id": "outcome_empty_project", "sample_id": "cpq_empty_project", "first_pass_success": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	s.telemetryContextPackQualityOutcomeRoute(response, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(body)))
	if got, want := response.Code, http.StatusUnprocessableEntity; got != want {
		t.Fatalf("empty-project quality sample outcome status=%d want=%d body=%s", got, want, response.Body.String())
	}
	snapshot := telemetry.snapshot()
	if got := anyToInt(snapshot["outcome_sample_count"], 0); got != 0 {
		t.Fatalf("empty-project sample admitted an outcome: %#v", snapshot)
	}
	if got := anyToInt(snapshot["calibration_outcome_sample_count"], 0); got != 0 {
		t.Fatalf("empty-project sample admitted calibration: %#v", snapshot)
	}
	if rows := parseRows(snapshot["outcomes"]); len(rows) != 0 {
		t.Fatalf("empty-project sample retained an admitted outcome: %#v", rows)
	}
	raw, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "outcome_empty_project") || strings.Contains(string(raw), contextPackOutcomeAdmissionSchemaID) {
		t.Fatalf("empty-project quality sample persisted an authoritative outcome: %s", raw)
	}
}

func TestContextPackLegacyOutcomeIsObservableButCalibrationIneligible(t *testing.T) {
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", filepath.Join(t.TempDir(), "quality.ndjson"))
	telemetry := newContextPackQualityTelemetry(20)
	if !telemetry.recordOutcome(map[string]any{
		"outcome_id": "legacy_outcome", "sample_id": "legacy_sample", "first_pass_success": true,
		"provider_total_tokens": 100,
	}) {
		t.Fatal("record legacy-shaped outcome")
	}
	reloaded := newContextPackQualityTelemetry(20)
	if !contextPackQualityLedgerAvailable(reloaded.ledger) {
		t.Fatalf("legacy observable-only outcome caused the quality ledger to fail closed: %#v", reloaded.ledger)
	}
	snapshot := reloaded.snapshot()
	if got := anyToInt(snapshot["outcome_sample_count"], 0); got != 1 {
		t.Fatalf("legacy outcome was not observable: %#v", snapshot)
	}
	if got := anyToInt(snapshot["calibration_outcome_sample_count"], 0); got != 0 {
		t.Fatalf("legacy outcome calibrated without server-bound sample: %#v", snapshot)
	}
	if got := anyToInt(snapshot["observed_provider_total_tokens"], 0); got != 0 {
		t.Fatalf("legacy outcome contributed provider telemetry: %#v", snapshot)
	}
	rows := parseRows(reloaded.snapshot()["outcomes"])
	if len(rows) != 1 || anyToString(rows[0]["quality_sample_admission"]) != "legacy_ineligible" || anyToBool(rows[0]["calibration_eligible"]) {
		t.Fatalf("legacy row was not explicitly marked ineligible: %#v", rows)
	}
	if anyToString(rows[0]["project"]) != "" {
		t.Fatalf("legacy row did not retain its empty authoritative scope: %#v", rows)
	}
}
