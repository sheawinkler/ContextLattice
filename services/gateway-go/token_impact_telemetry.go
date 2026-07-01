package main

import (
	"net/http"
	"sync"
)

type tokenImpactTelemetry struct {
	mu            sync.Mutex
	limit         int
	samples       []map[string]any
	sampleCount   int64
	totalBaseline int64
	totalPacked   int64
	totalSaved    int64
	totalPenalty  int64
	bestRatio     float64
	lastSampleAt  string
}

func newTokenImpactTelemetry(limit int) *tokenImpactTelemetry {
	if limit <= 0 {
		limit = 100
	}
	return &tokenImpactTelemetry{limit: limit, samples: make([]map[string]any, 0, limit)}
}

func (s *server) recordTokenImpact(sample map[string]any) {
	if s == nil || s.tokenImpact == nil || len(sample) == 0 {
		return
	}
	s.tokenImpact.record(sample)
}

func (s *server) tokenImpactTelemetrySnapshot() map[string]any {
	if s == nil || s.tokenImpact == nil {
		return defaultTokenImpactTelemetrySnapshot()
	}
	return s.tokenImpact.snapshot()
}

func defaultTokenImpactTelemetrySnapshot() map[string]any {
	return map[string]any{
		"schema_id":                "contextlattice_token_impact_telemetry.v1",
		"version":                  1,
		"updatedAt":                nowUTCISO(),
		"sample_count":             0,
		"calibration_grade":        "heuristic",
		"confidence":               "low",
		"baseline_tokens_estimate": 0,
		"packed_tokens_estimate":   0,
		"saved_tokens_estimate":    0,
		"risk_penalty_tokens":      0,
		"compression_ratio":        0,
		"average_saved_tokens":     0,
		"last_sample_at":           nil,
		"source":                   "/telemetry/token-impact",
		"measurement_limit":        "No context-pack token_impact samples have been recorded since gateway start.",
		"samples":                  []any{},
	}
}

func (t *tokenImpactTelemetry) record(sample map[string]any) {
	if t == nil {
		return
	}
	baseline := anyToInt(sample["baseline_tokens_estimate"], 0)
	packed := anyToInt(sample["packed_tokens_estimate"], 0)
	saved := anyToInt(sample["saved_tokens_estimate"], 0)
	if baseline <= 0 || packed <= 0 {
		return
	}
	if saved < 0 {
		saved = 0
	}
	now := nowUTCISO()
	ratio := anyToFloat(sample["compression_ratio"])
	if ratio <= 0 && packed > 0 {
		ratio = roundFloat(float64(baseline)/float64(packed), 2)
	}
	entry := map[string]any{
		"schema_id":                "contextlattice_token_impact.v1",
		"capturedAt":               now,
		"calibration_grade":        firstNonEmptyStrings(anyToString(sample["calibration_grade"]), "sampled_pack_estimate"),
		"confidence":               firstNonEmptyStrings(anyToString(sample["confidence"]), "medium"),
		"estimate_method":          firstNonEmptyStrings(anyToString(sample["estimate_method"]), "chars_div_4"),
		"baseline_tokens_estimate": baseline,
		"packed_tokens_estimate":   packed,
		"saved_tokens_estimate":    saved,
		"risk_penalty_tokens":      anyToInt(sample["risk_penalty_tokens"], 0),
		"compression_ratio":        ratio,
		"selected_evidence_count":  anyToInt(sample["selected_evidence_count"], 0),
		"omitted_high_value_count": anyToInt(sample["omitted_high_value_count"], 0),
		"returned_source_count":    anyToInt(sample["returned_source_count"], 0),
		"token_budget_active":      anyToBool(sample["token_budget_active"]),
		"token_budget_target":      anyToInt(sample["token_budget_target"], 0),
		"selection_strategy":       anyToString(sample["selection_strategy"]),
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.sampleCount++
	t.totalBaseline += int64(baseline)
	t.totalPacked += int64(packed)
	t.totalSaved += int64(saved)
	t.totalPenalty += int64(anyToInt(sample["risk_penalty_tokens"], 0))
	if ratio > t.bestRatio {
		t.bestRatio = ratio
	}
	t.lastSampleAt = now
	t.samples = append(t.samples, entry)
	if len(t.samples) > t.limit {
		t.samples = append([]map[string]any{}, t.samples[len(t.samples)-t.limit:]...)
	}
}

func (t *tokenImpactTelemetry) snapshot() map[string]any {
	if t == nil {
		return defaultTokenImpactTelemetrySnapshot()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sampleCount == 0 {
		return defaultTokenImpactTelemetrySnapshot()
	}
	samples := make([]any, 0, minInt(len(t.samples), 20))
	start := maxInt(0, len(t.samples)-20)
	for _, sample := range t.samples[start:] {
		samples = append(samples, cloneMap(sample))
	}
	ratio := 0.0
	if t.totalPacked > 0 {
		ratio = roundFloat(float64(t.totalBaseline)/float64(t.totalPacked), 2)
	}
	averageSaved := int64(0)
	if t.sampleCount > 0 {
		averageSaved = t.totalSaved / t.sampleCount
	}
	confidence := "medium"
	if t.sampleCount >= 3 && t.totalSaved > 0 {
		confidence = "high"
	}
	return map[string]any{
		"schema_id":                "contextlattice_token_impact_telemetry.v1",
		"version":                  1,
		"updatedAt":                nowUTCISO(),
		"sample_count":             t.sampleCount,
		"calibration_grade":        "sampled_pack_estimate",
		"confidence":               confidence,
		"baseline_tokens_estimate": t.totalBaseline,
		"packed_tokens_estimate":   t.totalPacked,
		"saved_tokens_estimate":    t.totalSaved,
		"risk_penalty_tokens":      t.totalPenalty,
		"compression_ratio":        ratio,
		"average_saved_tokens":     averageSaved,
		"best_compression_ratio":   roundFloat(t.bestRatio, 2),
		"last_sample_at":           t.lastSampleAt,
		"source":                   "/telemetry/token-impact",
		"measurement_limit":        "Aggregates sampled context-pack token_impact estimates; tokenizer-exact accounting is not yet enabled.",
		"basis": []any{
			"context_pack_response.token_impact",
			"raw candidate evidence JSON",
			"compiled reference_prompt",
			"ranked evidence token budget",
		},
		"factors": []any{
			map[string]any{"label": "sampled raw baselines", "role": "baseline", "tokens": t.totalBaseline, "value": anyToString(t.sampleCount) + " packs", "detail": "raw candidate evidence prompt-stuffing counterfactual"},
			map[string]any{"label": "compiled prompt packets", "role": "packed", "tokens": t.totalPacked, "value": anyToString(t.sampleCount) + " packs", "detail": "bounded ContextLattice reference prompts"},
			map[string]any{"label": "reliability penalty", "role": "penalty", "tokens": t.totalPenalty, "value": "aggregate", "detail": "reserved for sample-level risk drag"},
		},
		"samples": samples,
	}
}

func (s *server) telemetryTokenImpactRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.tokenImpactTelemetrySnapshot())
}
