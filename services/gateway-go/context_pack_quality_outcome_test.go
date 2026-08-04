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
)

func TestContextPackQualityOutcomeIsIdempotentAndCalibrationAware(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "context-pack-quality.ndjson")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", ledgerPath)
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")

	telemetry := newContextPackQualityTelemetry(20)
	telemetry.recordQuality(map[string]any{"sample_id": "cpq-1", "project": "contextlattice", "task_id": "task-1"})
	telemetry.recordQuality(map[string]any{"sample_id": "cpq-2", "project": "contextlattice", "task_id": "task-2"})
	srv := &server{contextPackQuality: telemetry}
	success := map[string]any{
		"outcome_id":           "task_task-1_cpq-1",
		"sample_id":            "cpq-1",
		"task_id":              "task-1",
		"project":              "contextlattice",
		"first_pass_success":   true,
		"repair_required":      false,
		"calibration_eligible": true,
		"outcome_source":       "task_agent_worker.gateway_inference",
		"outcome_class":        "success",
	}
	post := func(payload map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		srv.telemetryContextPackQualityOutcomeRoute(response, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(raw)))
		return response
	}
	if response := post(success); response.Code != http.StatusOK {
		t.Fatalf("expected first authoritative outcome, got %d: %s", response.Code, response.Body.String())
	}
	if response := post(success); response.Code != http.StatusOK {
		t.Fatalf("expected duplicate authoritative outcome, got %d: %s", response.Code, response.Body.String())
	}
	infrastructureFailure := map[string]any{
		"outcome_id":           "task_task-2_cpq-2",
		"sample_id":            "cpq-2",
		"task_id":              "task-2",
		"project":              "contextlattice",
		"first_pass_success":   false,
		"repair_required":      false,
		"calibration_eligible": false,
		"outcome_source":       "task_agent_worker.runner_adapter",
		"outcome_class":        "infrastructure_failure",
	}
	if response := post(infrastructureFailure); response.Code != http.StatusOK {
		t.Fatalf("expected non-calibration authoritative outcome, got %d: %s", response.Code, response.Body.String())
	}

	snapshot := telemetry.snapshot()
	if got := anyToInt(snapshot["outcome_sample_count"], 0); got != 2 {
		t.Fatalf("expected two observed outcomes, got %d", got)
	}
	if got := anyToInt(snapshot["calibration_outcome_sample_count"], 0); got != 1 {
		t.Fatalf("expected one calibration-eligible outcome, got %d", got)
	}
	if got := anyToFloat(snapshot["observed_first_pass_success_rate"]); got != 1 {
		t.Fatalf("infrastructure failure must not lower context calibration rate, got %v", got)
	}

	reloaded := newContextPackQualityTelemetry(20)
	reloadedSnapshot := reloaded.snapshot()
	if got := anyToInt(reloadedSnapshot["outcome_sample_count"], 0); got != 2 {
		t.Fatalf("expected persisted outcomes to reload once each, got %d", got)
	}
	reloadedServer := &server{contextPackQuality: reloaded}
	if raw, err := json.Marshal(success); err != nil {
		t.Fatal(err)
	} else {
		response := httptest.NewRecorder()
		reloadedServer.telemetryContextPackQualityOutcomeRoute(response, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(raw)))
		if response.Code != http.StatusOK {
			t.Fatalf("expected replay after restart to remain idempotent, got %d: %s", response.Code, response.Body.String())
		}
	}
	raw, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read outcome ledger: %v", err)
	}
	if got := len(strings.Split(strings.TrimSpace(string(raw)), "\n")); got != 4 {
		t.Fatalf("expected two quality samples plus two durable outcomes, got %d rows", got)
	}
}
