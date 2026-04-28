package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	defaultRecallEvalK                  = 5
	defaultRecallEvalGateMinRecallAtK   = 0.75
	defaultRecallEvalGateMinMRR         = 0.55
	defaultRecallEvalGateMinNumeric     = 0.90
	defaultRecallEvalCasesRelativePath  = "services/orchestrator/data/recall_eval_cases.json"
	fallbackRecallEvalCasesRelativePath = "../orchestrator/data/recall_eval_cases.json"
)

type recallEvalGate struct {
	MinRecallAtK      float64
	MinMRR            float64
	MinNumericExactly float64
}

type recallEvalSavedConfig struct {
	Path      string
	Version   any
	UpdatedAt any
	K         int
	Gate      recallEvalGate
	Cases     []map[string]any
}

func (s *server) memoryRecallEvaluateSavedNative(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	incomingHeaders, ok := s.prepareAuthorizedHeaders(w, r)
	if !ok {
		return
	}
	bodyBytes, err := readRequestBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
		return
	}
	payload := map[string]any{}
	if strings.TrimSpace(string(bodyBytes)) != "" {
		parsed, parseErr := parseJSONMap(bodyBytes)
		if parseErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json payload"})
			return
		}
		payload = parsed
	}

	cfg, cfgErr := loadSavedRecallEvalConfig()
	if cfgErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "failed to load saved recall eval cases", "detail": cfgErr.Error()})
		return
	}
	if len(cfg.Cases) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "saved recall eval case set is empty"})
		return
	}

	k := clampInt(cfg.K, 1, 20)
	if raw, exists := payload["k"]; exists {
		k = clampInt(anyToInt(raw, cfg.K), 1, 20)
	}
	gate := cfg.Gate
	if raw, exists := payload["gate_min_recall_at_k"]; exists {
		gate.MinRecallAtK = clampFloat(parseAnyFloat(raw, gate.MinRecallAtK), 0.0, 1.0)
	}
	if raw, exists := payload["gate_min_mrr"]; exists {
		gate.MinMRR = clampFloat(parseAnyFloat(raw, gate.MinMRR), 0.0, 1.0)
	}
	if raw, exists := payload["gate_min_numeric_exactness"]; exists {
		gate.MinNumericExactly = clampFloat(parseAnyFloat(raw, gate.MinNumericExactly), 0.0, 1.0)
	}

	includeRetrievalDebug := anyToBool(payload["include_retrieval_debug"])
	includePreferences := anyToBool(payload["include_preferences"])
	userID := strings.TrimSpace(anyToString(payload["user_id"]))

	recallHits := 0
	reciprocalRankSum := 0.0
	evaluatedCases := 0
	numericExpectedTotal := 0
	numericMatchedTotal := 0
	caseReports := make([]map[string]any, 0, len(cfg.Cases))

	for idx, rawCase := range cfg.Cases {
		caseID := strings.TrimSpace(anyToString(rawCase["id"]))
		if caseID == "" {
			caseID = fmt.Sprintf("case-%d", idx+1)
		}
		query := strings.TrimSpace(anyToString(rawCase["query"]))
		if query == "" {
			caseReports = append(caseReports, map[string]any{
				"id":                  caseID,
				"query":               "",
				"k":                   k,
				"hit":                 false,
				"matched_rank":        nil,
				"reciprocal_rank":     0.0,
				"has_expectations":    false,
				"result_count":        0,
				"top_score":           0.0,
				"expected_files":      []string{},
				"expected_substrings": []string{},
				"expected_numeric":    []string{},
				"matched_numeric":     []string{},
				"warnings":            []string{"case query missing"},
				"retrieval_mode":      normalizeRetrievalMode(anyToString(rawCase["retrieval_mode"])),
				"agent_id":            strings.TrimSpace(anyToString(rawCase["agent_id"])),
			})
			continue
		}

		reqPayload := map[string]any{
			"query":                   query,
			"limit":                   clampInt(anyToInt(rawCase["limit"], 10), 1, 100),
			"project":                 strings.TrimSpace(anyToString(rawCase["project"])),
			"topic_path":              strings.TrimSpace(anyToString(rawCase["topic_path"])),
			"retrieval_mode":          normalizeRetrievalMode(anyToString(rawCase["retrieval_mode"])),
			"retrieval_intent":        strings.TrimSpace(strings.ToLower(anyToString(rawCase["retrieval_intent"]))),
			"sources":                 anyToStringSlice(rawCase["sources"]),
			"source_weights":          cloneAnyMap(anyMap(rawCase["source_weights"])),
			"rerank_with_learning":    true,
			"include_retrieval_debug": includeRetrievalDebug,
			"include_grounding":       true,
			"include_preferences":     includePreferences,
			"user_id":                 userID,
			"agent_id":                strings.TrimSpace(anyToString(rawCase["agent_id"])),
			"auto_escalate":           anyToBool(rawCase["auto_escalate"]),
			"query_expansion":         anyToBool(rawCase["query_expansion"]),
			"deep_async":              false,
			"callback_url":            "",
			"traffic_class":           "synthetic",
		}
		if strings.TrimSpace(anyToString(reqPayload["retrieval_intent"])) == "" {
			reqPayload["retrieval_intent"] = "decision"
		}

		searchResp, status, execErr := s.executeRetrieval(
			context.Background(),
			incomingHeaders,
			reqPayload,
			true,
		)
		if execErr != nil {
			caseReports = append(caseReports, map[string]any{
				"id":                  caseID,
				"query":               query,
				"k":                   k,
				"hit":                 false,
				"matched_rank":        nil,
				"reciprocal_rank":     0.0,
				"has_expectations":    false,
				"result_count":        0,
				"top_score":           0.0,
				"expected_files":      []string{},
				"expected_substrings": []string{},
				"expected_numeric":    []string{},
				"matched_numeric":     []string{},
				"warnings":            []string{"retrieval failed: " + execErr.Error()},
				"retrieval_mode":      reqPayload["retrieval_mode"],
				"agent_id":            reqPayload["agent_id"],
				"status_code":         status,
			})
			continue
		}

		results := parseRows(searchResp["results"])
		grounding := anyMap(searchResp["grounding"])
		expectedFiles := normalizeExpectedFileTokens(rawCase["expected_files"])
		expectedTerms := normalizeExpectedTerms(rawCase["expected_substrings"])
		expectedNumeric := normalizeExpectedNumeric(rawCase["expected_numeric"])

		matchedRank := matchRankWithinK(results, expectedFiles, expectedTerms, k)
		hit := matchedRank != nil
		reciprocalRank := 0.0
		if matchedRank != nil && *matchedRank > 0 {
			reciprocalRank = 1.0 / float64(*matchedRank)
		}
		numericMatches := matchedNumericFacts(grounding, expectedNumeric)
		hasExpectations := len(expectedFiles) > 0 || len(expectedTerms) > 0
		if hasExpectations {
			evaluatedCases += 1
			if hit {
				recallHits += 1
			}
			reciprocalRankSum += reciprocalRank
		}
		numericExpectedTotal += len(expectedNumeric)
		numericMatchedTotal += len(numericMatches)

		report := map[string]any{
			"id":                        caseID,
			"query":                     query,
			"k":                         k,
			"hit":                       hit,
			"matched_rank":              matchedRank,
			"reciprocal_rank":           roundFloat(reciprocalRank, 6),
			"has_expectations":          hasExpectations,
			"result_count":              len(results),
			"top_score":                 roundFloat(topResultScore(results), 6),
			"expected_files":            sortedKeys(expectedFiles),
			"expected_substrings":       expectedTerms,
			"expected_numeric":          expectedNumeric,
			"matched_numeric":           numericMatches,
			"warnings":                  parseWarnings(searchResp["warnings"]),
			"retrieval_mode":            searchResp["retrieval_mode"],
			"agent_id":                  searchResp["agent_id"],
			"retry_attempts":            0,
			"transient_retry_triggered": false,
			"transient_retry_recovered": false,
			"attempt_modes":             []string{normalizeRetrievalMode(anyToString(searchResp["retrieval_mode"]))},
		}
		if includeRetrievalDebug {
			report["retrieval"] = searchResp["retrieval_debug"]
		}
		caseReports = append(caseReports, report)
	}

	recallAtK := 0.0
	mrr := 0.0
	if evaluatedCases > 0 {
		recallAtK = float64(recallHits) / float64(evaluatedCases)
		mrr = reciprocalRankSum / float64(evaluatedCases)
	}
	numericExactness := 1.0
	if numericExpectedTotal > 0 {
		numericExactness = float64(numericMatchedTotal) / float64(numericExpectedTotal)
	}
	passed := evaluatedCases > 0 && recallAtK >= gate.MinRecallAtK && mrr >= gate.MinMRR && numericExactness >= gate.MinNumericExactly

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"passed": passed,
		"metrics": map[string]any{
			"k":                k,
			"casesTotal":       len(cfg.Cases),
			"casesEvaluated":   evaluatedCases,
			"recallAtK":        roundFloat(recallAtK, 6),
			"mrr":              roundFloat(mrr, 6),
			"numericExactness": roundFloat(numericExactness, 6),
			"numericExpected":  numericExpectedTotal,
			"numericMatched":   numericMatchedTotal,
		},
		"gate": map[string]any{
			"minRecallAtK":        gate.MinRecallAtK,
			"minMrr":              gate.MinMRR,
			"minNumericExactness": gate.MinNumericExactly,
		},
		"cases": caseReports,
		"savedCaseSet": map[string]any{
			"path":      cfg.Path,
			"version":   cfg.Version,
			"updatedAt": cfg.UpdatedAt,
			"count":     len(cfg.Cases),
		},
	})
}

func loadSavedRecallEvalConfig() (recallEvalSavedConfig, error) {
	path := resolveRecallEvalCasesPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return defaultSavedRecallEvalConfig(path), nil
	}
	payload := map[string]any{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return defaultSavedRecallEvalConfig(path), nil
	}
	casesRaw, _ := payload["cases"].([]any)
	cases := make([]map[string]any, 0, len(casesRaw))
	for _, row := range casesRaw {
		item := anyMap(row)
		if len(item) == 0 {
			continue
		}
		cases = append(cases, item)
	}
	gateRaw := anyMap(payload["gate"])
	cfg := recallEvalSavedConfig{
		Path:      path,
		Version:   payload["version"],
		UpdatedAt: payload["updatedAt"],
		K:         clampInt(anyToInt(payload["k"], defaultRecallEvalK), 1, 20),
		Gate: recallEvalGate{
			MinRecallAtK:      clampFloat(parseAnyFloat(gateRaw["minRecallAtK"], defaultRecallEvalGateMinRecallAtK), 0.0, 1.0),
			MinMRR:            clampFloat(parseAnyFloat(gateRaw["minMrr"], defaultRecallEvalGateMinMRR), 0.0, 1.0),
			MinNumericExactly: clampFloat(parseAnyFloat(gateRaw["minNumericExactness"], defaultRecallEvalGateMinNumeric), 0.0, 1.0),
		},
		Cases: cases,
	}
	if len(cfg.Cases) == 0 {
		cfg.Cases = defaultSavedRecallEvalConfig(path).Cases
	}
	return cfg, nil
}

func defaultSavedRecallEvalConfig(path string) recallEvalSavedConfig {
	return recallEvalSavedConfig{
		Path:      path,
		Version:   1,
		UpdatedAt: nowUTCISO(),
		K:         defaultRecallEvalK,
		Gate: recallEvalGate{
			MinRecallAtK:      defaultRecallEvalGateMinRecallAtK,
			MinMRR:            defaultRecallEvalGateMinMRR,
			MinNumericExactly: defaultRecallEvalGateMinNumeric,
		},
		Cases: []map[string]any{
			{
				"id":                  "health-surface",
				"query":               "health status",
				"limit":               8,
				"expected_substrings": []string{"health"},
			},
			{
				"id":                  "trading-telemetry-surface",
				"query":               "trading telemetry process",
				"limit":               8,
				"expected_substrings": []string{"trading"},
			},
			{
				"id":                  "retrieval-sources-surface",
				"query":               "letta memory_bank retrieval",
				"limit":               10,
				"expected_substrings": []string{"letta", "memory_bank"},
			},
		},
	}
}

func resolveRecallEvalCasesPath() string {
	envPath := strings.TrimSpace(os.Getenv("ORCH_RECALL_EVAL_CASES_PATH"))
	if envPath != "" {
		return envPath
	}
	candidates := []string{
		filepath.Clean(defaultRecallEvalCasesRelativePath),
		filepath.Clean(fallbackRecallEvalCasesRelativePath),
		filepath.Clean(filepath.Join("data", "recall_eval_cases.json")),
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return candidates[0]
}

func normalizeExpectedFileTokens(raw any) map[string]struct{} {
	out := map[string]struct{}{}
	for _, item := range anyToStringSlice(raw) {
		normalized := strings.Trim(strings.TrimSpace(strings.ToLower(item)), "/")
		if normalized == "" {
			continue
		}
		out[normalized] = struct{}{}
	}
	return out
}

func sortedKeys(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sortStrings(out)
	return out
}

func normalizeExpectedTerms(raw any) []string {
	out := []string{}
	seen := map[string]struct{}{}
	for _, item := range anyToStringSlice(raw) {
		normalized := strings.TrimSpace(strings.ToLower(item))
		if normalized == "" {
			continue
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	return out
}

func normalizeExpectedNumeric(raw any) []string {
	out := []string{}
	seen := map[string]struct{}{}
	for _, item := range anyToStringSlice(raw) {
		normalized := strings.TrimSpace(item)
		if normalized == "" {
			continue
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	return out
}

func matchRankWithinK(results []map[string]any, expectedFiles map[string]struct{}, expectedTerms []string, k int) *int {
	if len(expectedFiles) == 0 && len(expectedTerms) == 0 {
		return nil
	}
	maxRank := clampInt(k, 1, 100)
	for idx, row := range results {
		if idx >= maxRank {
			break
		}
		if resultHitsExpectations(row, expectedFiles, expectedTerms) {
			rank := idx + 1
			return &rank
		}
	}
	return nil
}

func resultHitsExpectations(row map[string]any, expectedFiles map[string]struct{}, expectedTerms []string) bool {
	rowFile := strings.Trim(strings.TrimSpace(strings.ToLower(anyToString(row["file"]))), "/")
	if rowFile != "" && len(expectedFiles) > 0 {
		for candidate := range expectedFiles {
			if rowFile == candidate || strings.HasSuffix(rowFile, candidate) {
				return true
			}
		}
	}
	if len(expectedTerms) > 0 {
		summary := strings.TrimSpace(strings.ToLower(anyToString(row["summary"])))
		project := strings.TrimSpace(strings.ToLower(anyToString(row["project"])))
		source := strings.TrimSpace(strings.ToLower(anyToString(row["source"])))
		haystack := rowFile + "\n" + project + "\n" + source + "\n" + summary
		for _, token := range expectedTerms {
			if token != "" && strings.Contains(haystack, token) {
				return true
			}
		}
	}
	return false
}

func matchedNumericFacts(grounding map[string]any, expected []string) []string {
	if len(expected) == 0 {
		return []string{}
	}
	numericSet := map[string]struct{}{}
	facts, _ := grounding["numeric_facts"].([]any)
	for _, row := range facts {
		item := anyMap(row)
		value := strings.TrimSpace(anyToString(item["value"]))
		if value != "" {
			numericSet[value] = struct{}{}
		}
	}
	matches := []string{}
	for _, value := range expected {
		if _, ok := numericSet[value]; ok {
			matches = append(matches, value)
		}
	}
	return matches
}

func topResultScore(results []map[string]any) float64 {
	best := 0.0
	for _, row := range results {
		score := parseAnyFloat(row["score"], 0.0)
		if score > best {
			best = score
		}
	}
	return best
}

func parseAnyFloat(raw any, fallback float64) float64 {
	switch value := raw.(type) {
	case float64:
		return value
	case float32:
		return float64(value)
	case int:
		return float64(value)
	case int64:
		return float64(value)
	case int32:
		return float64(value)
	case int16:
		return float64(value)
	case int8:
		return float64(value)
	case uint:
		return float64(value)
	case uint64:
		return float64(value)
	case uint32:
		return float64(value)
	case uint16:
		return float64(value)
	case uint8:
		return float64(value)
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err == nil {
			return parsed
		}
	}
	return fallback
}

func clampFloat(value float64, minValue float64, maxValue float64) float64 {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func sortStrings(values []string) {
	if len(values) < 2 {
		return
	}
	for idx := 1; idx < len(values); idx++ {
		pos := idx
		for pos > 0 && values[pos] < values[pos-1] {
			values[pos], values[pos-1] = values[pos-1], values[pos]
			pos--
		}
	}
}

func anyMap(raw any) map[string]any {
	value, _ := raw.(map[string]any)
	if value == nil {
		return map[string]any{}
	}
	return value
}
