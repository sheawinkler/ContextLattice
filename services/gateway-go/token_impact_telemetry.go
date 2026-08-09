package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type tokenImpactTelemetry struct {
	mu                         sync.Mutex
	limit                      int
	ledger                     *tokenImpactLedger
	samples                    []map[string]any
	proofSamples               proofTimelineMapRing
	proofRevision              uint64
	exactArtifactKeys          map[string]string
	exactArtifactOrder         []string
	exactArtifactLimit         int
	sampleCount                int64
	legacySampleCount          int64
	exactArtifactReplayCount   int64
	exactArtifactConflictCount int64
	exactSamples               int64
	modelVisibleExactSamples   int64
	totalBaseline              int64
	totalPacked                int64
	totalCompiled              int64
	totalTransport             int64
	totalModelVisible          int64
	totalNetDelta              int64
	transportInclusiveSamples  int64
	totalSaved                 int64
	totalPenalty               int64
	bestRatio                  float64
	lastSampleAt               string
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
	ledger := newTokenImpactLedgerFromEnv()
	artifactLimit := limit
	if ledger != nil {
		artifactLimit = maxInt(artifactLimit, ledger.maxSamples)
	}
	t := &tokenImpactTelemetry{
		limit:              limit,
		ledger:             ledger,
		samples:            make([]map[string]any, 0, limit),
		exactArtifactKeys:  make(map[string]string, artifactLimit),
		exactArtifactOrder: make([]string, 0, artifactLimit),
		exactArtifactLimit: artifactLimit,
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
	return resolveStoragePath("GO_TOKEN_IMPACT_LEDGER_PATH", filepath.Join(".data", "orchestrator", "token_impact_ledger.ndjson"))
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
		"schema_id":                          "contextlattice_token_impact_telemetry.v1",
		"version":                            3,
		"updatedAt":                          nowUTCISO(),
		"sample_count":                       0,
		"legacy_sample_count":                0,
		"exact_artifact_replay_count":        0,
		"exact_artifact_conflict_count":      0,
		"exact_artifact_identity_limit":      tokenImpactArtifactIdentityDefaultLimit(ledger),
		"exact_sample_count":                 0,
		"calibration_grade":                  "heuristic",
		"confidence":                         "low",
		"estimate_method":                    "none",
		"tokenizer_exact":                    false,
		"baseline_tokens_estimate":           0,
		"packed_tokens_estimate":             0,
		"compiled_prompt_tokens_estimate":    0,
		"transport_tokens_exact":             0,
		"wire_tokens_exact":                  0,
		"model_visible_context_tokens_exact": 0,
		"model_visible_exact_sample_count":   0,
		"net_token_delta":                    0,
		"transport_inclusive":                false,
		"transport_inclusive_sample_count":   0,
		"saved_tokens_estimate":              0,
		"risk_penalty_tokens":                0,
		"compression_ratio":                  0,
		"average_saved_tokens":               0,
		"last_sample_at":                     nil,
		"source":                             "/telemetry/token-impact",
		"measurement_limit":                  "No context-pack token_impact samples have been recorded since gateway start.",
		"storage":                            tokenImpactLedgerPublicStatus(ledger),
		"cohort_window_sample_count":         0,
		"cohort_total_count":                 0,
		"cohort_returned_count":              0,
		"cohort_omitted_count":               0,
		"cohort_limit":                       tokenImpactCohortLimit,
		"cohorts":                            []any{},
		"samples":                            []any{},
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
	accepted := t.applyEntryLocked(entry)
	t.mu.Unlock()
	if !accepted {
		return
	}

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
	if scope := normalizeTokenImpactDimension(anyToString(sample["scope"])); scope != "" {
		entry["scope"] = scope
	}
	if packedKind := normalizeTokenImpactDimension(anyToString(sample["packed_kind"])); packedKind != "" {
		entry["packed_kind"] = packedKind
	}
	copyProofTimelineIdentity(entry, sample)
	if anyToBool(sample["tokenizer_exact"]) {
		if modelVisible := anyToInt(sample["model_visible_context_tokens_exact"], 0); modelVisible > 0 {
			entry["model_visible_context_tokens_exact"] = modelVisible
		}
	}
	if anyToBool(sample["transport_inclusive"]) {
		transport := anyToInt(sample["transport_tokens_exact"], 0)
		if transport > 0 {
			wire := anyToInt(sample["wire_tokens_exact"], transport)
			if wire <= 0 {
				wire = transport
			}
			entry["transport_inclusive"] = true
			entry["transport_tokens_exact"] = transport
			entry["wire_tokens_exact"] = wire
			entry["compiled_prompt_tokens_estimate"] = maxInt(0, anyToInt(sample["compiled_prompt_tokens_estimate"], 0))
			entry["net_token_delta"] = anyToInt(sample["net_token_delta"], baseline-transport)
		}
	}
	if encoding := anyToString(sample["tokenizer_encoding"]); encoding != "" {
		entry["tokenizer_encoding"] = encoding
	}
	return entry
}

func (t *tokenImpactTelemetry) applyEntryLocked(entry map[string]any) bool {
	baseline := anyToInt(entry["baseline_tokens_estimate"], 0)
	packed := anyToInt(entry["packed_tokens_estimate"], 0)
	if baseline <= 0 || packed <= 0 {
		return false
	}
	if artifactKey := tokenImpactExactArtifactKey(entry); artifactKey != "" {
		fingerprint := tokenImpactArtifactFingerprint(entry)
		if previous, found := t.exactArtifactKeys[artifactKey]; found {
			if previous == fingerprint {
				t.exactArtifactReplayCount++
			} else {
				t.exactArtifactConflictCount++
			}
			return false
		}
		t.rememberExactArtifactLocked(artifactKey, fingerprint)
	} else {
		t.legacySampleCount++
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
		if modelVisible := anyToInt(entry["model_visible_context_tokens_exact"], 0); modelVisible > 0 {
			t.modelVisibleExactSamples++
			t.totalModelVisible += int64(modelVisible)
		}
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
	stored := cloneMap(entry)
	t.samples = append(t.samples, stored)
	t.proofSamples.add(stored)
	t.proofRevision = nextProofTimelineRevision(t.proofRevision)
	if len(t.samples) > t.limit {
		t.samples = append([]map[string]any{}, t.samples[len(t.samples)-t.limit:]...)
	}
	return true
}

func tokenImpactArtifactIdentityDefaultLimit(ledger *tokenImpactLedger) int {
	if ledger != nil && ledger.maxSamples > 0 {
		return ledger.maxSamples
	}
	return 100
}

func (t *tokenImpactTelemetry) rememberExactArtifactLocked(key, fingerprint string) {
	if t.exactArtifactKeys == nil {
		t.exactArtifactKeys = make(map[string]string)
	}
	if t.exactArtifactLimit <= 0 {
		t.exactArtifactLimit = maxInt(t.limit, 1)
	}
	t.exactArtifactKeys[key] = fingerprint
	t.exactArtifactOrder = append(t.exactArtifactOrder, key)
	for len(t.exactArtifactOrder) > t.exactArtifactLimit {
		oldest := t.exactArtifactOrder[0]
		t.exactArtifactOrder = t.exactArtifactOrder[1:]
		delete(t.exactArtifactKeys, oldest)
	}
}

const tokenImpactCohortLimit = 24

func normalizeTokenImpactDimension(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" || len(value) > 96 || strings.ContainsAny(value, "/\\") {
		return ""
	}
	var builder strings.Builder
	previousSeparator := false
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') {
			builder.WriteRune(character)
			previousSeparator = false
			continue
		}
		if character == '_' || character == '-' || character == '.' {
			if !previousSeparator && builder.Len() > 0 {
				builder.WriteByte('_')
				previousSeparator = true
			}
			continue
		}
		return ""
	}
	return strings.Trim(builder.String(), "_")
}

func tokenImpactExactArtifactKey(entry map[string]any) string {
	sampleID := strings.TrimSpace(anyToString(entry["sample_id"]))
	scope := normalizeTokenImpactDimension(anyToString(entry["scope"]))
	packedKind := normalizeTokenImpactDimension(anyToString(entry["packed_kind"]))
	if sampleID == "" || scope == "" || packedKind == "" {
		return ""
	}
	return sampleID + "\x00" + scope + "\x00" + packedKind
}

func tokenImpactArtifactFingerprint(entry map[string]any) string {
	boolFlag := func(value bool) string {
		if value {
			return "1"
		}
		return "0"
	}
	parts := []string{
		anyToString(entry["baseline_tokens_estimate"]),
		anyToString(entry["packed_tokens_estimate"]),
		anyToString(entry["saved_tokens_estimate"]),
		anyToString(entry["risk_penalty_tokens"]),
		boolFlag(anyToBool(entry["tokenizer_exact"])),
		anyToString(entry["model_visible_context_tokens_exact"]),
		boolFlag(anyToBool(entry["transport_inclusive"])),
		anyToString(entry["transport_tokens_exact"]),
		anyToString(entry["wire_tokens_exact"]),
		anyToString(entry["compiled_prompt_tokens_estimate"]),
		anyToString(entry["net_token_delta"]),
	}
	return sha256Hex(strings.Join(parts, "\x00"))
}

type tokenImpactCohort struct {
	scope                     string
	packedKind                string
	sampleCount               int64
	wireExactSampleCount      int64
	wireTokensExact           int64
	modelVisibleExactCount    int64
	modelVisibleContextTokens int64
	signedNetDeltaSampleCount int64
	signedNetTokenDelta       int64
}

func tokenImpactCohortRows(samples []map[string]any) ([]map[string]any, int) {
	cohorts := make(map[string]*tokenImpactCohort)
	for _, sample := range samples {
		scope := normalizeTokenImpactDimension(anyToString(sample["scope"]))
		packedKind := normalizeTokenImpactDimension(anyToString(sample["packed_kind"]))
		if scope == "" || packedKind == "" {
			continue
		}
		key := scope + "\x00" + packedKind
		cohort := cohorts[key]
		if cohort == nil {
			cohort = &tokenImpactCohort{scope: scope, packedKind: packedKind}
			cohorts[key] = cohort
		}
		cohort.sampleCount++
		if anyToBool(sample["transport_inclusive"]) {
			if wireTokens, present := sample["wire_tokens_exact"]; present {
				cohort.wireExactSampleCount++
				cohort.wireTokensExact += int64(anyToInt(wireTokens, 0))
			}
			if signedDelta, present := sample["net_token_delta"]; present {
				cohort.signedNetDeltaSampleCount++
				cohort.signedNetTokenDelta += int64(anyToInt(signedDelta, 0))
			}
		}
		if anyToBool(sample["tokenizer_exact"]) {
			if modelVisible, present := sample["model_visible_context_tokens_exact"]; present {
				cohort.modelVisibleExactCount++
				cohort.modelVisibleContextTokens += int64(anyToInt(modelVisible, 0))
			}
		}
	}
	keys := make([]string, 0, len(cohorts))
	for key := range cohorts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	rows := make([]map[string]any, 0, minInt(len(keys), tokenImpactCohortLimit))
	for _, key := range keys {
		if len(rows) >= tokenImpactCohortLimit {
			break
		}
		cohort := cohorts[key]
		rows = append(rows, map[string]any{
			"scope":                              cohort.scope,
			"packed_kind":                        cohort.packedKind,
			"sample_count":                       cohort.sampleCount,
			"wire_exact_sample_count":            cohort.wireExactSampleCount,
			"wire_tokens_exact":                  cohort.wireTokensExact,
			"model_visible_exact_sample_count":   cohort.modelVisibleExactCount,
			"model_visible_context_tokens_exact": cohort.modelVisibleContextTokens,
			"signed_net_delta_sample_count":      cohort.signedNetDeltaSampleCount,
			"signed_net_token_delta":             cohort.signedNetTokenDelta,
		})
	}
	return rows, len(keys)
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
	cohorts, cohortTotal := tokenImpactCohortRows(t.samples)
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
		"schema_id":                          "contextlattice_token_impact_telemetry.v1",
		"version":                            3,
		"updatedAt":                          nowUTCISO(),
		"sample_count":                       t.sampleCount,
		"legacy_sample_count":                t.legacySampleCount,
		"exact_artifact_replay_count":        t.exactArtifactReplayCount,
		"exact_artifact_conflict_count":      t.exactArtifactConflictCount,
		"exact_artifact_identity_limit":      t.exactArtifactLimit,
		"exact_sample_count":                 t.exactSamples,
		"calibration_grade":                  calibrationGrade,
		"confidence":                         confidence,
		"estimate_method":                    estimateMethod,
		"tokenizer_exact":                    tokenizerExact,
		"baseline_tokens_estimate":           t.totalBaseline,
		"packed_tokens_estimate":             t.totalPacked,
		"compiled_prompt_tokens_estimate":    t.totalCompiled,
		"transport_tokens_exact":             t.totalTransport,
		"wire_tokens_exact":                  t.totalTransport,
		"model_visible_context_tokens_exact": t.totalModelVisible,
		"model_visible_exact_sample_count":   t.modelVisibleExactSamples,
		"net_token_delta":                    t.totalNetDelta,
		"transport_inclusive":                transportComplete,
		"transport_inclusive_sample_count":   t.transportInclusiveSamples,
		"saved_tokens_estimate":              t.totalSaved,
		"risk_penalty_tokens":                t.totalPenalty,
		"compression_ratio":                  ratio,
		"average_saved_tokens":               averageSaved,
		"best_compression_ratio":             roundFloat(t.bestRatio, 2),
		"last_sample_at":                     t.lastSampleAt,
		"source":                             "/telemetry/token-impact",
		"measurement_limit":                  measurementLimit,
		"storage":                            tokenImpactLedgerPublicStatus(t.ledger),
		"cohort_window_sample_count":         len(t.samples),
		"cohort_total_count":                 cohortTotal,
		"cohort_returned_count":              len(cohorts),
		"cohort_omitted_count":               maxInt(0, cohortTotal-len(cohorts)),
		"cohort_limit":                       tokenImpactCohortLimit,
		"cohorts":                            cohorts,
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

func (t *tokenImpactTelemetry) sampleForUtility(sampleID string) (map[string]any, bool) {
	if t == nil || strings.TrimSpace(sampleID) == "" {
		return nil, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	rows := t.proofSamples.ordered()
	if len(rows) == 0 {
		rows = t.samples
	}
	for index := len(rows) - 1; index >= 0; index-- {
		if anyToString(rows[index]["sample_id"]) == sampleID {
			return cloneAnyMap(rows[index]), true
		}
	}
	return nil, false
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
