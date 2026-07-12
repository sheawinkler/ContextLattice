package main

import "testing"

func TestTokenImpactTelemetryPreservesTransportTruthWithoutBackfillingOldRows(t *testing.T) {
	telemetry := &tokenImpactTelemetry{limit: 10, samples: make([]map[string]any, 0, 10)}
	telemetry.record(map[string]any{
		"baseline_tokens_estimate":        100,
		"packed_tokens_estimate":          60,
		"compiled_prompt_tokens_estimate": 20,
		"transport_tokens_exact":          60,
		"net_token_delta":                 40,
		"saved_tokens_estimate":           40,
		"transport_inclusive":             true,
		"tokenizer_exact":                 true,
	})
	telemetry.record(map[string]any{
		"baseline_tokens_estimate": 80,
		"packed_tokens_estimate":   24,
		"saved_tokens_estimate":    56,
	})

	snapshot := telemetry.snapshot()
	if got := anyToInt(snapshot["compiled_prompt_tokens_estimate"], -1); got != 20 {
		t.Fatalf("compiled prompt total = %d, want 20", got)
	}
	if got := anyToInt(snapshot["transport_tokens_exact"], -1); got != 60 {
		t.Fatalf("transport exact total = %d, want 60", got)
	}
	if got := anyToInt(snapshot["net_token_delta"], -1); got != 40 {
		t.Fatalf("net token delta = %d, want 40", got)
	}
	if got := anyToInt(snapshot["transport_inclusive_sample_count"], -1); got != 1 {
		t.Fatalf("transport sample count = %d, want 1", got)
	}
	if anyToBool(snapshot["transport_inclusive"]) {
		t.Fatal("mixed historical rows must not be presented as fully transport-inclusive")
	}
	if anyToString(snapshot["transport_measurement_limit"]) == "" {
		t.Fatal("expected explicit partial-coverage warning")
	}
}
