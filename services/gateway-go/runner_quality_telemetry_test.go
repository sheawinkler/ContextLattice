package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunnerQualityTelemetrySummarizesLedgerAdvisorOnly(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	ledgerPath := filepath.Join(root, "_contextlattice", "runner_quality_ledger.ndjson")
	if err := os.MkdirAll(filepath.Dir(ledgerPath), 0755); err != nil {
		t.Fatalf("mkdir ledger dir: %v", err)
	}
	rows := []map[string]any{
		{
			"schema_id":     runnerQualitySampleSchemaID,
			"runner":        "pi",
			"task_class":    "scout",
			"status":        "succeeded",
			"ok":            true,
			"duration_secs": 1.2,
			"context_pack_quality": map[string]any{
				"quality_score":                    88,
				"exact_prompt_tokens_saved":        1200,
				"modeled_inference_tokens_avoided": 2400,
			},
		},
		{
			"schema_id":     runnerQualitySampleSchemaID,
			"runner":        "droid",
			"task_class":    "scout",
			"status":        "blocked",
			"ok":            false,
			"duration_secs": 0.8,
			"context_pack_quality": map[string]any{
				"quality_score":                    80,
				"exact_prompt_tokens_saved":        900,
				"modeled_inference_tokens_avoided": 1500,
			},
		},
		{
			"schema_id":     runnerQualitySampleSchemaID,
			"runner":        "droid",
			"task_class":    "implementer",
			"status":        "succeeded",
			"ok":            true,
			"duration_secs": 2.1,
			"context_pack_quality": map[string]any{
				"quality_score":                    91,
				"exact_prompt_tokens_saved":        1400,
				"modeled_inference_tokens_avoided": 3000,
			},
		},
	}
	var raw strings.Builder
	for _, row := range rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatalf("marshal row: %v", err)
		}
		raw.Write(encoded)
		raw.WriteByte('\n')
	}
	raw.WriteString("{not-json\n")
	if err := os.WriteFile(ledgerPath, []byte(raw.String()), 0644); err != nil {
		t.Fatalf("write ledger: %v", err)
	}

	gateway := httptest.NewServer(http.HandlerFunc((&server{}).telemetryRunnerQualityRoute))
	defer gateway.Close()
	resp, err := http.Get(gateway.URL + "/telemetry/runner-quality?task_class=scout&limit=20")
	if err != nil {
		t.Fatalf("runner quality telemetry request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if anyToString(payload["schema_id"]) != runnerQualityTelemetrySchemaID || anyToInt(payload["sample_count"], 0) != 2 || anyToInt(payload["total_sample_count"], 0) != 3 {
		t.Fatalf("unexpected runner quality summary: %#v", payload)
	}
	if _, leaked := payload["routing_hint"]; leaked {
		t.Fatalf("runner quality telemetry must not expose routing_hint: %#v", payload)
	}
	recommendations := anyMap(payload["recommendations"])
	if anyToString(recommendations["mode"]) != "advisor_only" || anyToString(recommendations["top_runner"]) != "pi" {
		t.Fatalf("expected advisor-only pi recommendation, got %#v", recommendations)
	}
	storage := anyMap(payload["storage"])
	if !anyToBool(storage["exists"]) || anyToInt(storage["parse_errors"], 0) != 1 {
		t.Fatalf("expected bounded ledger storage status, got %#v", storage)
	}
	encodedPayload, _ := json.Marshal(payload)
	if strings.Contains(string(encodedPayload), ledgerPath) || strings.Contains(string(encodedPayload), "runner_quality_ledger.ndjson") {
		t.Fatalf("telemetry response leaked local ledger path: %s", string(encodedPayload))
	}
}
