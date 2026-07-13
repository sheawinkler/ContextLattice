package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type tokenImpactTelemetry struct {
	mu                        sync.Mutex
	limit                     int
	ledger                    *tokenImpactLedger
	samples                   []map[string]any
	sampleCount               int64
	exactSamples              int64
	totalBaseline             int64
	totalPacked               int64
	totalCompiled             int64
	totalTransport            int64
	totalNetDelta             int64
	transportInclusiveSamples int64
	totalSaved                int64
	totalPenalty              int64
	bestRatio                 float64
	lastSampleAt              string
}

type tokenImpactLedger struct {
	mu          sync.Mutex
	enabled     bool
	path        string
	maxBytes    int64
	maxSamples  int
	parseErrors int
	writeErrors int
	loadedRows  int
	lastWriteAt string
	lastError   string
}

func newTokenImpactTelemetry(limit int) *tokenImpactTelemetry {
	if limit <= 0 {
		limit = 100
	}
	t := &tokenImpactTelemetry{
		limit:   limit,
		ledger:  newTokenImpactLedgerFromEnv(),
		samples: make([]map[string]any, 0, limit),
	}
	t.loadPersistedSamples()
	return t
}

func newTokenImpactLedgerFromEnv() *tokenImpactLedger {
	enabled := envBool("GO_TOKEN_IMPACT_LEDGER_ENABLED", true)
	path := tokenImpactLedgerPath()
	if strings.TrimSpace(path) == "" {
		enabled = false
	}
	maxBytes := int64(clampInt(envInt("GO_TOKEN_IMPACT_LEDGER_MAX_BYTES", 2*1024*1024), 64*1024, 64*1024*1024))
	maxSamples := clampInt(envInt("GO_TOKEN_IMPACT_LEDGER_MAX_SAMPLES", 1000), 20, 20000)
	ledger := &tokenImpactLedger{enabled: enabled, path: path, maxBytes: maxBytes, maxSamples: maxSamples}
	if enabled {
		dedicatedParent := strings.TrimSpace(os.Getenv("GO_TOKEN_IMPACT_LEDGER_PATH")) == ""
		if err := prepareOwnerOnlyFile(path, dedicatedParent); err != nil {
			ledger.enabled = false
			ledger.lastError = err.Error()
		}
	}
	return ledger
}

func tokenImpactLedgerPath() string {
	if explicit := strings.TrimSpace(os.Getenv("GO_TOKEN_IMPACT_LEDGER_PATH")); explicit != "" {
		return filepath.Clean(explicit)
	}
	root := strings.TrimSpace(os.Getenv("GO_MEMORY_STORE_ROOT"))
	if root == "" {
		root = strings.TrimSpace(os.Getenv("MEMORY_BANK_ROOT"))
	}
	if root != "" {
		return filepath.Clean(filepath.Join(root, "_contextlattice", "token_impact_ledger.ndjson"))
	}
	return filepath.Clean(filepath.Join(".data", "orchestrator", "token_impact_ledger.ndjson"))
}

func (s *server) recordTokenImpact(sample map[string]any) {
	if s == nil || s.tokenImpact == nil || len(sample) == 0 {
		return
	}
	s.tokenImpact.record(sample)
}

func (s *server) tokenImpactTelemetrySnapshot() map[string]any {
	if s == nil || s.tokenImpact == nil {
		return defaultTokenImpactTelemetrySnapshot(nil)
	}
	return s.tokenImpact.snapshot()
}

func defaultTokenImpactTelemetrySnapshot(ledger *tokenImpactLedger) map[string]any {
	return map[string]any{
		"schema_id":                        "contextlattice_token_impact_telemetry.v1",
		"version":                          3,
		"updatedAt":                        nowUTCISO(),
		"sample_count":                     0,
		"exact_sample_count":               0,
		"calibration_grade":                "heuristic",
		"confidence":                       "low",
		"estimate_method":                  "none",
		"tokenizer_exact":                  false,
		"baseline_tokens_estimate":         0,
		"packed_tokens_estimate":           0,
		"compiled_prompt_tokens_estimate":  0,
		"transport_tokens_exact":           0,
		"net_token_delta":                  0,
		"transport_inclusive":              false,
		"transport_inclusive_sample_count": 0,
		"saved_tokens_estimate":            0,
		"risk_penalty_tokens":              0,
		"compression_ratio":                0,
		"average_saved_tokens":             0,
		"last_sample_at":                   nil,
		"source":                           "/telemetry/token-impact",
		"measurement_limit":                "No context-pack token_impact samples have been recorded since gateway start.",
		"storage":                          tokenImpactLedgerPublicStatus(ledger),
		"samples":                          []any{},
	}
}

func (t *tokenImpactTelemetry) loadPersistedSamples() {
	if t == nil || t.ledger == nil || !t.ledger.enabled || t.ledger.path == "" {
		return
	}
	rows, parseErrors, err := t.ledger.readRows()
	if err != nil {
		t.ledger.setError(err)
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, row := range rows {
		t.applyEntryLocked(row)
	}
	t.ledger.loadedRows = len(rows)
	t.ledger.parseErrors = parseErrors
}

func (t *tokenImpactTelemetry) record(sample map[string]any) {
	if t == nil {
		return
	}
	entry := tokenImpactEntryFromSample(sample)
	if len(entry) == 0 {
		return
	}

	t.mu.Lock()
	t.applyEntryLocked(entry)
	t.mu.Unlock()

	if t.ledger != nil && t.ledger.enabled {
		if err := t.ledger.append(entry); err != nil {
			t.ledger.setError(err)
		}
	}
}

func tokenImpactEntryFromSample(sample map[string]any) map[string]any {
	baseline := anyToInt(sample["baseline_tokens_estimate"], 0)
	packed := anyToInt(sample["packed_tokens_estimate"], 0)
	saved := anyToInt(sample["saved_tokens_estimate"], 0)
	if baseline <= 0 || packed <= 0 {
		return nil
	}
	if saved < 0 {
		saved = 0
	}
	ratio := anyToFloat(sample["compression_ratio"])
	if ratio <= 0 && packed > 0 {
		ratio = roundFloat(float64(baseline)/float64(packed), 2)
	}
	entry := map[string]any{
		"schema_id":                "contextlattice_token_impact.v1",
		"version":                  3,
		"capturedAt":               nowUTCISO(),
		"calibration_grade":        firstNonEmptyStrings(anyToString(sample["calibration_grade"]), "sampled_pack_estimate"),
		"confidence":               firstNonEmptyStrings(anyToString(sample["confidence"]), "medium"),
		"estimate_method":          firstNonEmptyStrings(anyToString(sample["estimate_method"]), "chars_div_4"),
		"tokenizer_exact":          anyToBool(sample["tokenizer_exact"]),
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
	if anyToBool(sample["transport_inclusive"]) {
		transport := anyToInt(sample["transport_tokens_exact"], 0)
		if transport > 0 {
			entry["transport_inclusive"] = true
			entry["transport_tokens_exact"] = transport
			entry["compiled_prompt_tokens_estimate"] = anyToInt(sample["compiled_prompt_tokens_estimate"], 0)
			entry["net_token_delta"] = anyToInt(sample["net_token_delta"], baseline-transport)
		}
	}
	if encoding := anyToString(sample["tokenizer_encoding"]); encoding != "" {
		entry["tokenizer_encoding"] = encoding
	}
	return entry
}

func (t *tokenImpactTelemetry) applyEntryLocked(entry map[string]any) {
	baseline := anyToInt(entry["baseline_tokens_estimate"], 0)
	packed := anyToInt(entry["packed_tokens_estimate"], 0)
	if baseline <= 0 || packed <= 0 {
		return
	}
	saved := anyToInt(entry["saved_tokens_estimate"], 0)
	if saved < 0 {
		saved = 0
	}
	ratio := anyToFloat(entry["compression_ratio"])
	if ratio <= 0 {
		ratio = roundFloat(float64(baseline)/float64(packed), 2)
		entry["compression_ratio"] = ratio
	}
	t.sampleCount++
	if anyToBool(entry["tokenizer_exact"]) {
		t.exactSamples++
	}
	t.totalBaseline += int64(baseline)
	t.totalPacked += int64(packed)
	if anyToBool(entry["transport_inclusive"]) {
		t.transportInclusiveSamples++
		t.totalCompiled += int64(anyToInt(entry["compiled_prompt_tokens_estimate"], 0))
		t.totalTransport += int64(anyToInt(entry["transport_tokens_exact"], 0))
		t.totalNetDelta += int64(anyToInt(entry["net_token_delta"], 0))
	}
	t.totalSaved += int64(saved)
	t.totalPenalty += int64(anyToInt(entry["risk_penalty_tokens"], 0))
	if ratio > t.bestRatio {
		t.bestRatio = ratio
	}
	t.lastSampleAt = anyToString(entry["capturedAt"])
	if t.lastSampleAt == "" {
		t.lastSampleAt = nowUTCISO()
		entry["capturedAt"] = t.lastSampleAt
	}
	t.samples = append(t.samples, cloneMap(entry))
	if len(t.samples) > t.limit {
		t.samples = append([]map[string]any{}, t.samples[len(t.samples)-t.limit:]...)
	}
}

func (t *tokenImpactTelemetry) snapshot() map[string]any {
	if t == nil {
		return defaultTokenImpactTelemetrySnapshot(nil)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sampleCount == 0 {
		return defaultTokenImpactTelemetrySnapshot(t.ledger)
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
	calibrationGrade := "sampled_pack_estimate"
	estimateMethod := "mixed"
	tokenizerExact := false
	if t.exactSamples == t.sampleCount {
		calibrationGrade = "tokenizer_exact"
		estimateMethod = "tiktoken"
		tokenizerExact = true
	}
	measurementLimit := "Aggregates bounded context-pack token_impact rows. No raw prompt or source text is persisted."
	if !tokenizerExact {
		measurementLimit += " Some rows use fallback estimation because tokenizer accounting was unavailable."
	}
	transportComplete := t.transportInclusiveSamples == t.sampleCount
	payload := map[string]any{
		"schema_id":                        "contextlattice_token_impact_telemetry.v1",
		"version":                          3,
		"updatedAt":                        nowUTCISO(),
		"sample_count":                     t.sampleCount,
		"exact_sample_count":               t.exactSamples,
		"calibration_grade":                calibrationGrade,
		"confidence":                       confidence,
		"estimate_method":                  estimateMethod,
		"tokenizer_exact":                  tokenizerExact,
		"baseline_tokens_estimate":         t.totalBaseline,
		"packed_tokens_estimate":           t.totalPacked,
		"compiled_prompt_tokens_estimate":  t.totalCompiled,
		"transport_tokens_exact":           t.totalTransport,
		"net_token_delta":                  t.totalNetDelta,
		"transport_inclusive":              transportComplete,
		"transport_inclusive_sample_count": t.transportInclusiveSamples,
		"saved_tokens_estimate":            t.totalSaved,
		"risk_penalty_tokens":              t.totalPenalty,
		"compression_ratio":                ratio,
		"average_saved_tokens":             averageSaved,
		"best_compression_ratio":           roundFloat(t.bestRatio, 2),
		"last_sample_at":                   t.lastSampleAt,
		"source":                           "/telemetry/token-impact",
		"measurement_limit":                measurementLimit,
		"storage":                          tokenImpactLedgerPublicStatus(t.ledger),
		"basis": []any{
			"context_pack_response.token_impact",
			"raw candidate evidence JSON token count",
			"compiled reference_prompt token count",
			"ranked evidence token budget",
		},
		"factors": []any{
			map[string]any{"label": "sampled raw baselines", "role": "baseline", "tokens": t.totalBaseline, "value": anyToString(t.sampleCount) + " packs", "detail": "raw candidate evidence prompt-stuffing counterfactual"},
			map[string]any{"label": "compiled prompt packets", "role": "packed", "tokens": t.totalPacked, "value": anyToString(t.sampleCount) + " packs", "detail": "bounded ContextLattice reference prompts"},
			map[string]any{"label": "reliability penalty", "role": "penalty", "tokens": t.totalPenalty, "value": "aggregate", "detail": "reserved for sample-level risk drag"},
		},
		"samples": samples,
	}
	if !transportComplete {
		payload["transport_measurement_limit"] = "Transport-exact totals include only transport-inclusive samples; older ledger rows remain excluded rather than inferred."
	}
	if encoding := dominantTokenizerEncoding(t.samples); encoding != "" {
		payload["tokenizer_encoding"] = encoding
	}
	return payload
}

func dominantTokenizerEncoding(samples []map[string]any) string {
	counts := map[string]int{}
	best := ""
	bestCount := 0
	for _, sample := range samples {
		encoding := anyToString(sample["tokenizer_encoding"])
		if encoding == "" {
			continue
		}
		counts[encoding]++
		if counts[encoding] > bestCount {
			best = encoding
			bestCount = counts[encoding]
		}
	}
	return best
}

func tokenImpactLedgerPublicStatus(ledger *tokenImpactLedger) map[string]any {
	status := map[string]any{
		"enabled":    false,
		"durability": "memory_only",
	}
	if ledger == nil {
		return status
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	status["enabled"] = ledger.enabled
	if ledger.enabled {
		status["durability"] = "bounded_ndjson"
	}
	status["max_bytes"] = ledger.maxBytes
	status["max_samples"] = ledger.maxSamples
	status["loaded_rows"] = ledger.loadedRows
	status["parse_errors"] = ledger.parseErrors
	status["write_errors"] = ledger.writeErrors
	status["last_write_at"] = ledger.lastWriteAt
	status["last_error"] = ledger.lastError
	return status
}

func (l *tokenImpactLedger) readRows() ([]map[string]any, int, error) {
	if l == nil || !l.enabled || l.path == "" {
		return nil, 0, nil
	}
	file, err := os.Open(l.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer file.Close()

	rows := make([]map[string]any, 0, l.maxSamples)
	parseErrors := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			parseErrors++
			continue
		}
		if anyToString(row["schema_id"]) != "contextlattice_token_impact.v1" {
			continue
		}
		rows = append(rows, row)
		if len(rows) > l.maxSamples {
			rows = append([]map[string]any{}, rows[len(rows)-l.maxSamples:]...)
		}
	}
	if err := scanner.Err(); err != nil {
		return rows, parseErrors, err
	}
	return rows, parseErrors, nil
}

func (l *tokenImpactLedger) append(entry map[string]any) error {
	if l == nil || !l.enabled || l.path == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	file, err := openOwnerOnlyAppend(l.path, false)
	if err != nil {
		l.writeErrors++
		return err
	}
	encoded, err := json.Marshal(entry)
	if err == nil {
		_, err = file.Write(append(encoded, '\n'))
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		l.writeErrors++
		return err
	}
	l.lastWriteAt = nowUTCISO()
	l.lastError = ""
	if stat, statErr := os.Stat(l.path); statErr == nil && stat.Size() > l.maxBytes {
		if pruneErr := l.pruneLocked(); pruneErr != nil {
			l.writeErrors++
			return pruneErr
		}
	}
	return nil
}

func (l *tokenImpactLedger) pruneLocked() error {
	rows, _, err := l.readRowsUnlocked()
	if err != nil {
		return err
	}
	if len(rows) > l.maxSamples {
		rows = rows[len(rows)-l.maxSamples:]
	}
	encodedRows := make([][]byte, 0, len(rows))
	total := int64(0)
	for i := len(rows) - 1; i >= 0; i-- {
		encoded, err := json.Marshal(rows[i])
		if err != nil {
			continue
		}
		lineBytes := int64(len(encoded) + 1)
		if len(encodedRows) > 0 && total+lineBytes > l.maxBytes {
			break
		}
		encodedRows = append(encodedRows, encoded)
		total += lineBytes
	}
	for i, j := 0, len(encodedRows)-1; i < j; i, j = i+1, j-1 {
		encodedRows[i], encodedRows[j] = encodedRows[j], encodedRows[i]
	}
	tmp := l.path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, ownerOnlyFileMode)
	if err != nil {
		return err
	}
	for _, row := range encodedRows {
		if _, err := file.Write(append(row, '\n')); err != nil {
			_ = file.Close()
			return err
		}
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, l.path); err != nil {
		return err
	}
	return ensureOwnerOnlyFile(l.path)
}

func (l *tokenImpactLedger) readRowsUnlocked() ([]map[string]any, int, error) {
	file, err := os.Open(l.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer file.Close()
	rows := []map[string]any{}
	parseErrors := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)
	for scanner.Scan() {
		var row map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(scanner.Text())), &row); err != nil {
			parseErrors++
			continue
		}
		if anyToString(row["schema_id"]) == "contextlattice_token_impact.v1" {
			rows = append(rows, row)
		}
	}
	return rows, parseErrors, scanner.Err()
}

func (l *tokenImpactLedger) setError(err error) {
	if l == nil || err == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastError = tokenImpactLedgerErrorCode(err)
}

func tokenImpactLedgerErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, os.ErrPermission):
		return "permission_denied"
	case errors.Is(err, os.ErrNotExist):
		return "not_found"
	default:
		return "io_error"
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
