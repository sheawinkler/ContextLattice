package main

import (
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
	success := map[string]any{
		"outcome_id":           "task_task-1_cpq-1",
		"sample_id":            "cpq-1",
		"task_id":              "task-1",
		"first_pass_success":   true,
		"repair_required":      false,
		"calibration_eligible": true,
		"outcome_source":       "task_agent_worker.gateway_inference",
		"outcome_class":        "success",
	}
	if !telemetry.recordOutcome(success) {
		t.Fatal("expected first outcome to be recorded")
	}
	if telemetry.recordOutcome(success) {
		t.Fatal("expected duplicate outcome id to be ignored")
	}
	infrastructureFailure := map[string]any{
		"outcome_id":           "task_task-2_cpq-2",
		"sample_id":            "cpq-2",
		"task_id":              "task-2",
		"first_pass_success":   false,
		"repair_required":      false,
		"calibration_eligible": false,
		"outcome_source":       "task_agent_worker.runner_adapter",
		"outcome_class":        "infrastructure_failure",
	}
	if !telemetry.recordOutcome(infrastructureFailure) {
		t.Fatal("expected non-calibration outcome to remain observable")
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
	if reloaded.recordOutcome(success) {
		t.Fatal("expected replay after restart to remain idempotent")
	}
	raw, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read outcome ledger: %v", err)
	}
	if got := len(strings.Split(strings.TrimSpace(string(raw)), "\n")); got != 2 {
		t.Fatalf("expected exactly two durable rows, got %d", got)
	}
}
