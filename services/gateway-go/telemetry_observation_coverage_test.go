package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTokenImpactCompiledCoverageDoesNotBorrowTransportDenominator(t *testing.T) {
	telemetry := &tokenImpactTelemetry{limit: 10}
	for _, compiled := range []any{nil, 0, 19, "0", -1} {
		sample := map[string]any{"baseline_tokens_estimate": 100, "packed_tokens_estimate": 60, "transport_tokens_exact": 60, "transport_inclusive": true}
		if compiled != nil {
			sample["compiled_prompt_tokens_estimate"] = compiled
		}
		telemetry.record(sample)
	}
	snapshot := telemetry.snapshot()
	if anyToInt(snapshot["transport_inclusive_sample_count"], -1) != 5 || anyToInt(snapshot["compiled_prompt_sample_count"], -1) != 2 {
		t.Fatalf("compiled estimate coverage borrowed transport coverage: %#v", snapshot)
	}
	if anyToInt(snapshot["compiled_prompt_tokens_estimate"], -1) != 19 {
		t.Fatalf("compiled estimate sum changed: %#v", snapshot)
	}
	legacy := &tokenImpactTelemetry{limit: 10}
	legacy.applyEntryLocked(map[string]any{"baseline_tokens_estimate": 100, "packed_tokens_estimate": 60, "transport_tokens_exact": 60, "transport_inclusive": true, "compiled_prompt_tokens_estimate": 0})
	if anyToInt(legacy.snapshot()["compiled_prompt_sample_count"], -1) != 0 {
		t.Fatal("historical synthesized zero was promoted into an observed compiled estimate")
	}
}

func coverageTestOutcome(t *testing.T, id string, usage map[string]any) map[string]any {
	t.Helper()
	entry, err := contextPackQualityOutcomeFromSampleChecked(map[string]any{
		"outcome_id": id, "sample_id": "sample-" + id, "project": "coverage",
		"first_pass_success": true, "provider_usage": usage,
	})
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestProviderCoverageInvalidSidecarIsVisibleAndDoesNotCorruptCanonicalOutcome(t *testing.T) {
	for _, fixture := range []string{"truncated", "duplicate-key", "unknown-field", "oversized", "symlink"} {
		t.Run(fixture, func(t *testing.T) {
			root := t.TempDir()
			ledger := &contextPackQualityLedger{enabled: true, path: filepath.Join(root, "quality.ndjson"), maxSamples: 20}
			entry := coverageTestOutcome(t, "invalid-sidecar", map[string]any{"total_tokens": 0})
			if err := ledger.saveProviderCoverageLocked(entry); err != nil {
				t.Fatal(err)
			}
			name := ledger.path + providerCoverageSuffix
			raw, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			switch fixture {
			case "truncated":
				raw = raw[:len(raw)-1]
			case "duplicate-key":
				raw = bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":9,"version":1`), 1)
			case "unknown-field":
				raw = bytes.Replace(raw, []byte(`"version":1`), []byte(`"unexpected":true,"version":1`), 1)
			case "oversized":
				raw = bytes.Repeat([]byte(" "), providerCoverageMaxBytes+1)
			case "symlink":
				target := filepath.Join(root, "target.json")
				if err := os.Rename(name, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, name); err != nil {
					t.Fatal(err)
				}
			}
			if fixture != "symlink" {
				if err := os.WriteFile(name, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			reloaded := &contextPackQualityLedger{enabled: true, path: ledger.path, maxSamples: 20}
			legacy := providerCoverageLegacyOutcome(entry)
			rows := reloaded.withProviderCoverage([]map[string]any{legacy})
			if len(rows) != 1 || contextPackOutcomeLogicalClaimDigest(rows[0]) != contextPackOutcomeLogicalClaimDigest(legacy) {
				t.Fatal("corrupt optional metadata changed the canonical outcome")
			}
			status := providerCoverageOperationalStatus(reloaded)
			if anyToBool(status["healthy"]) || anyToString(status["error_code"]) == "" || !anyToBool(status["loaded"]) {
				t.Fatalf("coverage failure was hidden: %#v", status)
			}
			encoded, _ := json.Marshal(status)
			if strings.Contains(string(encoded), root) {
				t.Fatal("operational coverage status exposed a private path")
			}
		})
	}
}

func TestProviderCoverageBoundedRetentionAndUnjoinedMetadata(t *testing.T) {
	ledger := &contextPackQualityLedger{enabled: true, path: filepath.Join(t.TempDir(), "quality.ndjson"), maxSamples: 20}
	entries := make(map[string]map[string]any)
	for i := 0; i < 25; i++ {
		id := fmt.Sprintf("bounded-%d", i)
		entries[id] = coverageTestOutcome(t, id, map[string]any{"total_tokens": 0})
		if err := ledger.saveProviderCoverageLocked(entries[id]); err != nil {
			t.Fatal(err)
		}
	}
	if len(ledger.providerCoverageRows) != 20 {
		t.Fatalf("coverage metadata is unbounded: %d", len(ledger.providerCoverageRows))
	}
	if rows := ledger.withProviderCoverage(nil); len(rows) != 0 {
		t.Fatal("unjoined coverage metadata invented canonical outcomes")
	}
	entries["unrelated"] = coverageTestOutcome(t, "unrelated", map[string]any{"total_tokens": 0})
	for _, id := range []string{"bounded-0", "bounded-24", "unrelated"} {
		entry := providerCoverageLegacyOutcome(entries[id])
		rows := ledger.withProviderCoverage([]map[string]any{entry})
		_, covered := rows[0]["provider_usage_field_mask"]
		if covered != (id == "bounded-24") {
			t.Fatalf("wrong exact-claim coverage for %s: %#v", id, rows)
		}
	}
	later := cloneJSONMap(entries["bounded-24"])
	later["gateway_received_at"] = "2026-01-01T00:00:00Z"
	rows := ledger.withProviderCoverage([]map[string]any{providerCoverageLegacyOutcome(later)})
	if _, present := rows[0]["provider_usage_field_mask"]; present {
		t.Fatal("metadata for an earlier receipt decorated different canonical bytes")
	}
}

func TestProviderCoverageDurabilityFailureDoesNotAcknowledgeCoverage(t *testing.T) {
	ledger := &contextPackQualityLedger{enabled: true, path: filepath.Join(t.TempDir(), "quality.ndjson"), maxSamples: 20}
	ledger.writeFile = func(_ string, _ []byte, _ bool) error { return errors.New("injected unavailable storage") }
	entry := coverageTestOutcome(t, "write-failure", map[string]any{"total_tokens": 0})
	if err := ledger.saveProviderCoverageLocked(entry); err == nil {
		t.Fatal("coverage write failure was acknowledged")
	}
	if len(ledger.providerCoverageRows) != 0 || anyToBool(providerCoverageOperationalStatus(ledger)["healthy"]) {
		t.Fatal("failed coverage write changed acknowledged state")
	}
	rows := ledger.withProviderCoverage([]map[string]any{providerCoverageLegacyOutcome(entry)})
	if _, present := rows[0]["provider_usage_field_mask"]; present {
		t.Fatal("failed coverage write manufactured coverage")
	}
}

func TestProviderCoverageCanonicalFailureCannotDonatePresenceToLaterClaim(t *testing.T) {
	for _, laterUsage := range []map[string]any{{}, {"prompt_tokens": 0}} {
		t.Run(fmt.Sprint(laterUsage), func(t *testing.T) {
			name := filepath.Join(t.TempDir(), "quality.ndjson")
			if err := os.Mkdir(name, 0700); err != nil {
				t.Fatal(err)
			}
			ledger := &contextPackQualityLedger{enabled: true, path: name, maxSamples: 20, maxBytes: 1024 * 1024}
			attempt := coverageTestOutcome(t, "orphan-collision", map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0})
			attempt["gateway_received_at"] = "2026-01-01T00:00:00Z"
			if err := ledger.append(attempt); err == nil {
				t.Fatal("canonical directory fixture unexpectedly writable as a ledger")
			}
			if err := os.Remove(name); err != nil {
				t.Fatal(err)
			}
			later := coverageTestOutcome(t, "orphan-collision", laterUsage)
			later["gateway_received_at"] = "2026-01-01T00:01:00Z"
			reloaded := &contextPackQualityLedger{enabled: true, path: name, maxSamples: 20, maxBytes: 1024 * 1024}
			if err := reloaded.append(later); err != nil {
				t.Fatalf("uncommitted attempt vetoed a later canonical claim: %v", err)
			}
			rows, _, err := reloaded.readRows()
			if err != nil || len(rows) != 1 {
				t.Fatalf("canonical rows: %d %v", len(rows), err)
			}
			rows = reloaded.withProviderCoverage(rows)
			if anyToInt(rows[0]["provider_usage_field_mask"], 0) != anyToInt(later["provider_usage_field_mask"], 0) {
				t.Fatal("uncommitted attempt donated coverage to a later canonical claim")
			}
		})
	}
}

func TestProviderCoverageTransientWriteRecovery(t *testing.T) {
	ledger := &contextPackQualityLedger{enabled: true, path: filepath.Join(t.TempDir(), "quality.ndjson"), maxSamples: 20}
	ledger.writeFile = func(_ string, _ []byte, _ bool) error { return errors.New("injected transient failure") }
	entry := coverageTestOutcome(t, "transient", map[string]any{"total_tokens": 0})
	if err := ledger.saveProviderCoverageLocked(entry); err == nil {
		t.Fatal("fixture did not fail")
	}
	ledger.writeFile = writeOwnerOnlyDurableAtomicFile
	if err := ledger.saveProviderCoverageLocked(entry); err != nil {
		t.Fatalf("recovered storage remained permanently unavailable: %v", err)
	}
	if !anyToBool(providerCoverageOperationalStatus(ledger)["healthy"]) {
		t.Fatal("recovered and acknowledged coverage remains unhealthy")
	}
}

func TestProviderCoverageClientMaskDoesNotConferPresence(t *testing.T) {
	entry, err := contextPackQualityOutcomeFromSampleChecked(map[string]any{
		"outcome_id": "forged-mask", "sample_id": "forged-mask-sample", "project": "coverage",
		"first_pass_success": true, "provider_usage_field_mask": 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if anyToInt(entry["provider_usage_field_mask"], 0) != 0 {
		t.Fatal("caller-supplied coverage mask was trusted")
	}
}

func TestProviderCoverageUtilityRetrySurvivesRestart(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GO_UTILITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_UTILITY_LEDGER_PATH", filepath.Join(root, "utility.ndjson"))
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", filepath.Join(root, "quality.ndjson"))
	t.Setenv("GO_TOKEN_IMPACT_LEDGER_ENABLED", "false")
	srv := &server{contextPackQuality: newContextPackQualityTelemetry(20), utility: newUtilityTestTelemetry(t, 20)}
	srv.contextPackQuality.recordQuality(map[string]any{"sample_id": "utility-coverage-sample", "project": "coverage"})
	input := map[string]any{
		"outcome_id": "utility-coverage", "sample_id": "utility-coverage-sample", "project": "coverage", "first_pass_success": true,
		"provider_usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	}
	post := func() *httptest.ResponseRecorder {
		body, _ := json.Marshal(input)
		response := httptest.NewRecorder()
		srv.telemetryContextPackQualityOutcomeRoute(response, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(body)))
		return response
	}
	if response := post(); response.Code != http.StatusOK {
		t.Fatalf("first outcome: %d %s", response.Code, response.Body.String())
	}
	srv.utility.store.close()
	srv = &server{contextPackQuality: newContextPackQualityTelemetry(20), utility: newUtilityTestTelemetry(t, 20)}
	if response := post(); response.Code != http.StatusOK {
		t.Fatalf("quality/Utility retry after restart: %d %s", response.Code, response.Body.String())
	}
	input["first_pass_success"] = false
	if response := post(); response.Code != http.StatusConflict {
		t.Fatalf("changed claim accepted: %d %s", response.Code, response.Body.String())
	}
	if anyToInt(srv.contextPackQuality.snapshot()["observed_provider_complete_total_count"], -1) != 1 {
		t.Fatal("idempotent retry changed coverage denominator")
	}
}

func TestProviderCoverageLegacyRetryAndRollbackShape(t *testing.T) {
	for _, usage := range []map[string]any{{"prompt_tokens": 33}, {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}} {
		label, _ := json.Marshal(usage)
		t.Run(string(label), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "quality.ndjson")
			t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
			t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", path)
			telemetry := newContextPackQualityTelemetry(10)
			telemetry.recordQuality(map[string]any{"sample_id": "sample-upgrade", "project": "upgrade"})
			input := map[string]any{"outcome_id": "outcome-upgrade", "sample_id": "sample-upgrade", "project": "upgrade", "first_pass_success": true, "provider_usage": usage}
			post := func(current *contextPackQualityTelemetry) *httptest.ResponseRecorder {
				body, _ := json.Marshal(input)
				response := httptest.NewRecorder()
				(&server{contextPackQuality: current}).telemetryContextPackQualityOutcomeRoute(response, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(body)))
				return response
			}
			if response := post(telemetry); response.Code != http.StatusOK {
				t.Fatal(response.Body.String())
			}
			rows, _, err := telemetry.ledger.readRows()
			if err != nil {
				t.Fatal(err)
			}
			var legacy bytes.Buffer
			for _, row := range rows {
				if anyToString(row["schema_id"]) == contextPackQualityOutcomeSchemaID {
					if _, present := row["provider_usage_field_mask"]; present {
						t.Error("rollback reader cannot accept coverage field in canonical outcome")
					}
					delete(row, "provider_usage_field_mask")
					for _, field := range []string{"provider_prompt_tokens", "provider_completion_tokens", "provider_total_tokens"} {
						if count, present := row[field]; present && anyToInt(count, -1) == 0 {
							t.Error("rollback reader cannot accept persisted zero provider field")
							delete(row, field)
						}
					}
				}
				encoded, _ := json.Marshal(row)
				legacy.Write(append(encoded, '\n'))
			}
			if err := os.WriteFile(path, legacy.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			// Simulate a pre-upgrade writer, which had no optional coverage sidecar.
			if err := os.Remove(path + ".provider-coverage.v1.json"); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			reloaded := newContextPackQualityTelemetry(10)
			if response := post(reloaded); response.Code != http.StatusOK {
				t.Fatalf("identical legacy retry failed: %d %s", response.Code, response.Body.String())
			}
			if anyToInt(reloaded.snapshot()["observed_provider_complete_total_count"], -1) != 0 {
				t.Fatal("retry retroactively promoted legacy coverage")
			}
			input["first_pass_success"] = false
			if response := post(reloaded); response.Code != http.StatusConflict {
				t.Fatalf("changed legacy claim accepted: %d", response.Code)
			}
		})
	}
}

func TestProviderCoverageSurvivesAdmissionAndReplay(t *testing.T) {
	cases := []struct {
		name       string
		usage      map[string]any
		mask       int
		complete   int
		prompt     int
		completion int
		total      int
	}{
		{"absent", map[string]any{}, 0, 0, 0, 0, 0},
		{"prompt-only", map[string]any{"prompt_tokens": 33}, 1, 0, 1, 0, 0},
		{"completion-only", map[string]any{"completion_tokens": 7}, 2, 0, 0, 1, 0},
		{"total-only", map[string]any{"total_tokens": 47}, 4, 1, 0, 0, 47},
		{"complete-pair", map[string]any{"prompt_tokens": 33, "completion_tokens": 7}, 3, 1, 1, 1, 40},
		{"explicit-zero", map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}, 7, 1, 1, 1, 0},
		{"partial-zero", map[string]any{"prompt_tokens": 0}, 1, 0, 1, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
			t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", filepath.Join(t.TempDir(), "quality.ndjson"))
			telemetry := newContextPackQualityTelemetry(10)
			telemetry.recordQuality(map[string]any{"sample_id": "sample-coverage", "project": "coverage", "task_id": "task-coverage"})
			srv := &server{contextPackQuality: telemetry}
			input := map[string]any{
				"outcome_id": "outcome-coverage", "sample_id": "sample-coverage", "project": "coverage", "task_id": "task-coverage",
				"first_pass_success": true, "provider_usage": tc.usage,
			}
			body, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			srv.telemetryContextPackQualityOutcomeRoute(response, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(body)))
			if response.Code != http.StatusOK {
				t.Fatalf("outcome admission failed: %d %s", response.Code, response.Body.String())
			}
			for _, current := range []*contextPackQualityTelemetry{telemetry, newContextPackQualityTelemetry(10)} {
				snapshot := current.snapshot()
				checks := map[string]int{
					"provider_usage_coverage_version":         1,
					"observed_provider_complete_total_count":  tc.complete,
					"observed_provider_prompt_count":          tc.prompt,
					"observed_provider_completion_count":      tc.completion,
					"observed_provider_complete_total_tokens": tc.total,
				}
				for field, want := range checks {
					if got := anyToInt(snapshot[field], -1); got != want {
						t.Fatalf("%s=%d want %d: %#v", field, got, want, snapshot)
					}
				}
				outcomes := parseRows(snapshot["outcomes"])
				if len(outcomes) != 1 || anyToInt(outcomes[0]["provider_usage_field_mask"], 0) != tc.mask {
					t.Fatalf("provider field presence lost during admission/replay: %#v", outcomes)
				}
				wantUsage := 0
				if tc.mask != 0 {
					wantUsage = 1
				}
				if anyToInt(snapshot["observed_provider_usage_count"], -1) != wantUsage {
					t.Fatalf("explicit zero and absent observation conflated: %#v", snapshot)
				}
			}
		})
	}
}
