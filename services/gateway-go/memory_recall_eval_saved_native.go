package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultRecallEvalK                  = 5
	defaultRecallEvalGateMinRecallAtK   = 0.75
	defaultRecallEvalGateMinMRR         = 0.55
	defaultRecallEvalGateMinNumeric     = 0.90
	defaultRecallEvalCasesRelativePath  = ".data/orchestrator/recall_eval_cases.json"
	fallbackRecallEvalCasesRelativePath = "services/orchestrator/data/recall_eval_cases.json"
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
	mode := strings.ToLower(strings.TrimSpace(anyToString(payload["mode"])))
	if mode == "derive" {
		maxRows := clampInt(anyToInt(payload["max_rows"], derivedRegressionDefaultMaxRows), 1, derivedRegressionMaxRows)
		maxProposals := clampInt(anyToInt(payload["max_proposals"], derivedRegressionMaxProposals), 1, derivedRegressionMaxProposals)
		rows := []map[string]any{}
		if s != nil && s.contextPackQuality != nil {
			rows = s.contextPackQuality.derivedRegressionSourceRows(maxRows)
		}
		response := buildDerivedRegressionSuite(rows, derivedRegressionSuiteOptions{MaxRows: maxRows, MaxProposals: maxProposals})
		response["ok"] = true
		response["mode"] = "derive"
		writeJSON(w, http.StatusOK, attachPayloadFormatContract(
			derivedRegressionSuiteSchemaID,
			response,
			"",
			"derived_regression_suite",
			"/memory/recall/evaluate/saved",
		))
		return
	}
	if mode != "" && mode != "evaluate" && mode != "ablation" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported saved recall evaluation mode", "supported_modes": []string{"evaluate", "derive", "ablation"}})
		return
	}
	includeAblation := mode == "ablation"
	maxAblationRows := clampInt(anyToInt(payload["max_ablation_rows"], retrievalAblationDefaultMaxRows), 1, retrievalAblationMaxRows)

	cfg, cfgErr := loadSavedRecallEvalConfig()
	if cfgErr != nil {
		log.Printf("saved recall eval case load failed: %v", cfgErr)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "failed to load saved recall eval cases", "code": "storage_access_error"})
		return
	}
	if len(cfg.Cases) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "saved recall eval case set is empty"})
		return
	}
	caseSetHealth := validateSavedRecallEvalCaseSet(cfg)
	if !anyToBool(caseSetHealth["valid"]) {
		s.writeRecallEvalCaseSetInvalid(w, cfg, caseSetHealth)
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

	evaluationStartedAt := time.Now()
	recallHits := 0
	reciprocalRankSum := 0.0
	evaluatedCases := 0
	numericExpectedTotal := 0
	numericMatchedTotal := 0
	citationExpectedTotal := 0
	citationMatchedTotal := 0
	noHitCases := 0
	lowConfidenceCases := 0
	sourceDiversitySum := 0.0
	latencyValues := make([]float64, 0, len(cfg.Cases))
	graphEvaluatedCases := 0
	graphSeedCount := 0
	graphCandidateCount := 0
	graphAddedCandidateCount := 0
	graphExpectedHitCount := 0
	graphAddedExpectedHitCount := 0
	graphHelpedCases := 0
	graphExplicitCases := 0
	caseReports := make([]map[string]any, 0, len(cfg.Cases))
	ablationReports := make([]map[string]any, 0, len(cfg.Cases))

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
				"matched_files":       []string{},
				"citation_coverage":   0.0,
				"source_diversity":    0,
				"latency_ms":          0.0,
				"graph_contribution":  recallGraphContributionUnavailable("case query missing"),
				"warnings":            []string{"case query missing"},
				"retrieval_mode":      normalizeRetrievalMode(anyToString(rawCase["retrieval_mode"])),
				"agent_id":            strings.TrimSpace(anyToString(rawCase["agent_id"])),
			})
			continue
		}

		// Fetch at least K direct results so graph lift cannot be manufactured by
		// truncating the direct baseline below the evaluation window.
		directLimit := maxInt(clampInt(anyToInt(rawCase["limit"], 10), 1, 100), k)
		reqPayload := map[string]any{
			"query":                   query,
			"limit":                   directLimit,
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

		caseStartedAt := time.Now()
		searchResp, status, execErr := s.executeRetrieval(
			context.Background(),
			incomingHeaders,
			reqPayload,
			true,
		)
		latencyMs := float64(time.Since(caseStartedAt).Microseconds()) / 1000.0
		latencyValues = append(latencyValues, latencyMs)
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
				"matched_files":       []string{},
				"citation_coverage":   0.0,
				"source_diversity":    0,
				"latency_ms":          roundFloat(latencyMs, 3),
				"graph_contribution":  recallGraphContributionUnavailable("retrieval failed"),
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
		graphExpectedFiles := normalizeExpectedFileTokens(rawCase["graph_expected_files"])
		graphExpectedTerms := normalizeExpectedTerms(rawCase["graph_expected_substrings"])
		reportedGraphExpectedFiles := sortedKeys(graphExpectedFiles)
		reportedGraphExpectedTerms := append([]string(nil), graphExpectedTerms...)
		hasExplicitGraphExpectations := len(graphExpectedFiles) > 0 || len(graphExpectedTerms) > 0
		if !hasExplicitGraphExpectations {
			graphExpectedFiles = expectedFiles
			graphExpectedTerms = expectedTerms
		}
		matchedFiles := matchedExpectedFilesWithinK(results, expectedFiles, k)
		caseCitationCoverage := 1.0
		if len(expectedFiles) > 0 {
			caseCitationCoverage = float64(len(matchedFiles)) / float64(len(expectedFiles))
		}
		caseSources := uniqueSourcesWithinK(results, k)
		graphContribution := s.evaluateRecallGraphContribution(
			context.Background(),
			results,
			graphExpectedFiles,
			graphExpectedTerms,
			k,
			strings.TrimSpace(anyToString(reqPayload["project"])),
		)
		if hasExplicitGraphExpectations {
			graphContribution["expectation_mode"] = "explicit_graph"
		} else {
			graphContribution["expectation_mode"] = "direct_fallback"
		}

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
			} else {
				noHitCases += 1
			}
			reciprocalRankSum += reciprocalRank
			if topResultScore(results) > 0 && topResultScore(results) < 0.45 {
				lowConfidenceCases += 1
			}
			sourceDiversitySum += float64(len(caseSources))
			graphEligible := hasExplicitGraphExpectations
			if graphEligible {
				graphEvaluatedCases += 1
				graphSeedCount += anyToInt(graphContribution["seed_count"], 0)
				graphCandidateCount += anyToInt(graphContribution["candidate_count"], 0)
				graphAddedCandidateCount += anyToInt(graphContribution["added_candidate_count"], 0)
				graphExpectedHitCount += anyToInt(graphContribution["expected_hit_count"], 0)
				graphAddedExpectedHitCount += anyToInt(graphContribution["added_expected_hit_count"], 0)
				if hasExplicitGraphExpectations {
					graphExplicitCases += 1
				}
				if anyToBool(graphContribution["helped"]) {
					graphHelpedCases += 1
				}
			}
		}
		numericExpectedTotal += len(expectedNumeric)
		numericMatchedTotal += len(numericMatches)
		citationExpectedTotal += len(expectedFiles)
		citationMatchedTotal += len(matchedFiles)

		report := map[string]any{
			"id":                                  caseID,
			"query":                               query,
			"k":                                   k,
			"hit":                                 hit,
			"matched_rank":                        matchedRank,
			"reciprocal_rank":                     roundFloat(reciprocalRank, 6),
			"has_expectations":                    hasExpectations,
			"result_count":                        len(results),
			"top_score":                           roundFloat(topResultScore(results), 6),
			"expected_files":                      sortedKeys(expectedFiles),
			"expected_substrings":                 expectedTerms,
			"graph_expected_files":                reportedGraphExpectedFiles,
			"graph_expected_substrings":           reportedGraphExpectedTerms,
			"graph_effective_expected_files":      sortedKeys(graphExpectedFiles),
			"graph_effective_expected_substrings": graphExpectedTerms,
			"graph_expectations_explicit":         hasExplicitGraphExpectations,
			"expected_numeric":                    expectedNumeric,
			"matched_numeric":                     numericMatches,
			"matched_files":                       matchedFiles,
			"citation_coverage":                   roundFloat(caseCitationCoverage, 6),
			"source_diversity":                    len(caseSources),
			"sources":                             caseSources,
			"latency_ms":                          roundFloat(latencyMs, 3),
			"graph_contribution":                  graphContribution,
			"warnings":                            parseWarnings(searchResp["warnings"]),
			"retrieval_mode":                      searchResp["retrieval_mode"],
			"agent_id":                            searchResp["agent_id"],
			"retry_attempts":                      0,
			"transient_retry_triggered":           false,
			"transient_retry_recovered":           false,
			"attempt_modes":                       []string{normalizeRetrievalMode(anyToString(searchResp["retrieval_mode"]))},
		}
		if includeRetrievalDebug {
			report["retrieval"] = searchResp["retrieval_debug"]
		}
		if includeAblation {
			ablation := attachPayloadFormatContract(retrievalAblationSchemaID, buildRetrievalAblation(retrievalAblationInput{
				CaseID:         caseID,
				Results:        results,
				ExpectedFiles:  sortedKeys(expectedFiles),
				K:              k,
				TrafficClass:   "synthetic",
				SnapshotStable: true,
				MaxRows:        maxAblationRows,
			}), "", "retrieval_ablation", "/memory/recall/evaluate/saved")
			report["ablation"] = ablation
			ablationReports = append(ablationReports, ablation)
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
	citationCoverage := 1.0
	if citationExpectedTotal > 0 {
		citationCoverage = float64(citationMatchedTotal) / float64(citationExpectedTotal)
	}
	avgSourceDiversity := 0.0
	noHitRate := 0.0
	lowConfidenceRate := 0.0
	graphLift := 0.0
	if evaluatedCases > 0 {
		avgSourceDiversity = sourceDiversitySum / float64(evaluatedCases)
		noHitRate = float64(noHitCases) / float64(evaluatedCases)
		lowConfidenceRate = float64(lowConfidenceCases) / float64(evaluatedCases)
	}
	if graphEvaluatedCases > 0 {
		graphLift = float64(graphHelpedCases) / float64(graphEvaluatedCases)
	}
	avgLatencyMs, p95LatencyMs := recallLatencyStats(latencyValues)
	directPassed := evaluatedCases > 0 && recallAtK >= gate.MinRecallAtK && mrr >= gate.MinMRR && numericExactness >= gate.MinNumericExactly
	graphEfficacyStatus := "unmeasured"
	graphPassed := false
	if graphEvaluatedCases > 0 {
		graphEfficacyStatus = "failed"
		graphPassed = graphHelpedCases > 0 && graphAddedExpectedHitCount > 0 && graphLift > 0
		if graphPassed {
			graphEfficacyStatus = "passed"
		}
	}
	graphRequired := graphExplicitCases > 0
	passed := directPassed && (!graphRequired || graphPassed)
	qualityStatus := recallEvalQualityStatus(passed, evaluatedCases, recallAtK, mrr, numericExactness)
	metrics := map[string]any{
		"k":                      k,
		"casesTotal":             len(cfg.Cases),
		"casesEvaluated":         evaluatedCases,
		"recallAtK":              roundFloat(recallAtK, 6),
		"mrr":                    roundFloat(mrr, 6),
		"numericExactness":       roundFloat(numericExactness, 6),
		"numericExpected":        numericExpectedTotal,
		"numericMatched":         numericMatchedTotal,
		"citationCoverage":       roundFloat(citationCoverage, 6),
		"citationExpected":       citationExpectedTotal,
		"citationMatched":        citationMatchedTotal,
		"noHitRate":              roundFloat(noHitRate, 6),
		"lowConfidenceRate":      roundFloat(lowConfidenceRate, 6),
		"sourceDiversity":        roundFloat(avgSourceDiversity, 3),
		"avgLatencyMs":           roundFloat(avgLatencyMs, 3),
		"p95LatencyMs":           roundFloat(p95LatencyMs, 3),
		"durationMs":             roundFloat(float64(time.Since(evaluationStartedAt).Microseconds())/1000.0, 3),
		"qualityStatus":          qualityStatus,
		"directPassed":           directPassed,
		"graphEvaluatedCases":    graphEvaluatedCases,
		"graphExplicitCases":     graphExplicitCases,
		"graphSeedCount":         graphSeedCount,
		"graphCandidateCount":    graphCandidateCount,
		"graphAddedCandidates":   graphAddedCandidateCount,
		"graphExpectedHits":      graphExpectedHitCount,
		"graphAddedExpectedHits": graphAddedExpectedHitCount,
		"graphHelpedCases":       graphHelpedCases,
		"graphLift":              roundFloat(graphLift, 6),
		"graphPassed":            graphPassed,
		"graphRequired":          graphRequired,
		"graphEfficacyStatus":    graphEfficacyStatus,
		"graphContribution": map[string]any{
			"evaluatedCases":         graphEvaluatedCases,
			"explicitCases":          graphExplicitCases,
			"seedCount":              graphSeedCount,
			"candidateCount":         graphCandidateCount,
			"addedCandidateCount":    graphAddedCandidateCount,
			"expectedHitCount":       graphExpectedHitCount,
			"addedExpectedHitCount":  graphAddedExpectedHitCount,
			"helpedCases":            graphHelpedCases,
			"lift":                   roundFloat(graphLift, 6),
			"passed":                 graphPassed,
			"required":               graphRequired,
			"status":                 graphEfficacyStatus,
			"neighborLimitPerSeed":   recallEvalGraphNeighborLimit(),
			"memoryGraphStoreActive": s.memoryGraphBackend() != nil,
		},
	}
	recommendations := recallEvalRecommendations(metrics, gate, passed)
	_ = s.appendRecallMonitorSample(map[string]any{
		"timestamp":             nowUTCISO(),
		"source":                "saved_recall_eval",
		"passed":                passed,
		"qualityStatus":         qualityStatus,
		"caseCount":             len(cfg.Cases),
		"evaluatedCases":        evaluatedCases,
		"k":                     k,
		"recallAtK":             roundFloat(recallAtK, 6),
		"mrr":                   roundFloat(mrr, 6),
		"numericExactness":      roundFloat(numericExactness, 6),
		"citationCoverage":      roundFloat(citationCoverage, 6),
		"noHitRate":             roundFloat(noHitRate, 6),
		"lowConfidenceRate":     roundFloat(lowConfidenceRate, 6),
		"staleHitRate":          0.0,
		"maxSourceErrorRate":    0.0,
		"sourceDiversity":       roundFloat(avgSourceDiversity, 3),
		"graphLift":             roundFloat(graphLift, 6),
		"graphEfficacyStatus":   graphEfficacyStatus,
		"graphExpectedHitCount": graphExpectedHitCount,
		"graphHelpedCases":      graphHelpedCases,
		"avgLatencyMs":          roundFloat(avgLatencyMs, 3),
		"evalP95Ms":             roundFloat(p95LatencyMs, 3),
		"retrievalAlertCount":   0,
	})

	response := map[string]any{
		"ok":              true,
		"passed":          passed,
		"quality_status":  qualityStatus,
		"metrics":         metrics,
		"recommendations": recommendations,
		"gate": map[string]any{
			"minRecallAtK":        gate.MinRecallAtK,
			"minMrr":              gate.MinMRR,
			"minNumericExactness": gate.MinNumericExactly,
			"graphRequired":       graphRequired,
			"minGraphLift":        0.0,
		},
		"cases": caseReports,
		"savedCaseSet": map[string]any{
			"case_set_id": ownerOnlyStoreRef("recall_eval_cases"),
			"version":     cfg.Version,
			"updatedAt":   cfg.UpdatedAt,
			"count":       len(cfg.Cases),
		},
	}
	if includeAblation {
		response["mode"] = "ablation"
		response["ablation"] = attachPayloadFormatContract(
			retrievalAblationReportSchemaID,
			summarizeRetrievalAblations(ablationReports),
			"",
			"retrieval_ablation_report",
			"/memory/recall/evaluate/saved",
		)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *server) writeRecallEvalCaseSetInvalid(w http.ResponseWriter, cfg recallEvalSavedConfig, health map[string]any) {
	k := clampInt(cfg.K, 1, 20)
	qualityStatus := "case_set_invalid"
	instructions := recallEvalCaseSetAgentInstructions()
	metrics := map[string]any{
		"k":                 k,
		"casesTotal":        len(cfg.Cases),
		"casesEvaluated":    0,
		"recallAtK":         0.0,
		"mrr":               0.0,
		"numericExactness":  0.0,
		"numericExpected":   0,
		"numericMatched":    0,
		"citationCoverage":  0.0,
		"citationExpected":  0,
		"citationMatched":   0,
		"noHitRate":         0.0,
		"lowConfidenceRate": 0.0,
		"sourceDiversity":   0.0,
		"avgLatencyMs":      0.0,
		"p95LatencyMs":      0.0,
		"durationMs":        0.0,
		"qualityStatus":     qualityStatus,
		"failedFast":        true,
		"inputHealthStatus": anyToString(health["status"]),
		"graphContribution": map[string]any{
			"evaluatedCases":         0,
			"seedCount":              0,
			"candidateCount":         0,
			"addedCandidateCount":    0,
			"expectedHitCount":       0,
			"addedExpectedHitCount":  0,
			"helpedCases":            0,
			"lift":                   0.0,
			"neighborLimitPerSeed":   recallEvalGraphNeighborLimit(),
			"memoryGraphStoreActive": s.memoryGraphBackend() != nil,
		},
	}
	_ = s.appendRecallMonitorSample(map[string]any{
		"timestamp":           nowUTCISO(),
		"source":              "saved_recall_eval",
		"passed":              false,
		"failedFast":          true,
		"qualityStatus":       qualityStatus,
		"inputHealthStatus":   anyToString(health["status"]),
		"inputHealthIssues":   anyToInt(health["issue_count"], 0),
		"caseCount":           len(cfg.Cases),
		"evaluatedCases":      0,
		"k":                   k,
		"recallAtK":           0.0,
		"mrr":                 0.0,
		"numericExactness":    0.0,
		"citationCoverage":    0.0,
		"noHitRate":           0.0,
		"lowConfidenceRate":   0.0,
		"sourceDiversity":     0.0,
		"graphLift":           0.0,
		"avgLatencyMs":        0.0,
		"evalP95Ms":           0.0,
		"retrievalAlertCount": anyToInt(health["issue_count"], 0),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                 false,
		"passed":             false,
		"failed_fast":        true,
		"quality_status":     qualityStatus,
		"error":              "saved recall eval case set failed input health validation",
		"case_set_health":    health,
		"agent_instructions": instructions,
		"recommendations":    instructions,
		"metrics":            metrics,
		"gate": map[string]any{
			"minRecallAtK":        cfg.Gate.MinRecallAtK,
			"minMrr":              cfg.Gate.MinMRR,
			"minNumericExactness": cfg.Gate.MinNumericExactly,
		},
		"cases": []any{},
		"savedCaseSet": map[string]any{
			"case_set_id": ownerOnlyStoreRef("recall_eval_cases"),
			"version":     cfg.Version,
			"updatedAt":   cfg.UpdatedAt,
			"count":       len(cfg.Cases),
		},
	})
}

func loadSavedRecallEvalConfig() (recallEvalSavedConfig, error) {
	path := resolveRecallEvalCasesPath()
	envPath := strings.TrimSpace(os.Getenv("ORCH_RECALL_EVAL_CASES_PATH"))
	if err := prepareOwnerOnlyFile(path, envPath == ""); err != nil {
		return defaultSavedRecallEvalConfig(path), err
	}
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

func validateSavedRecallEvalCaseSet(cfg recallEvalSavedConfig) map[string]any {
	issues := make([]map[string]any, 0)
	invalidCases := map[int]struct{}{}
	graphCaseCount := 0
	addIssue := func(idx int, rawCase map[string]any, code string, detail string, fix string) {
		caseID := strings.TrimSpace(anyToString(rawCase["id"]))
		if caseID == "" {
			caseID = fmt.Sprintf("case-%d", idx+1)
		}
		invalidCases[idx] = struct{}{}
		issues = append(issues, map[string]any{
			"case_index": idx,
			"case_id":    caseID,
			"code":       code,
			"detail":     detail,
			"fix":        fix,
		})
	}
	defaultIDs := map[string]struct{}{
		"health-surface":            {},
		"trading-telemetry-surface": {},
		"retrieval-sources-surface": {},
	}
	for idx, rawCase := range cfg.Cases {
		caseID := strings.TrimSpace(anyToString(rawCase["id"]))
		query := strings.TrimSpace(anyToString(rawCase["query"]))
		project := strings.TrimSpace(anyToString(rawCase["project"]))
		topicPath := recallEvalNormalizeCaseTopic(anyToString(rawCase["topic_path"]))
		expectedFiles := sortedKeys(normalizeExpectedFileTokens(rawCase["expected_files"]))
		graphExpectedFiles := sortedKeys(normalizeExpectedFileTokens(rawCase["graph_expected_files"]))
		graphExpectedTerms := normalizeExpectedTerms(rawCase["graph_expected_substrings"])
		graphCase := strings.EqualFold(strings.TrimSpace(anyToString(rawCase["case_kind"])), "graph_neighbor") || len(graphExpectedFiles) > 0 || len(graphExpectedTerms) > 0
		if _, ok := defaultIDs[caseID]; ok {
			addIssue(idx, rawCase, "default_fallback_case", "case matches the built-in fallback recall surface", "Refresh saved cases from live memory with /memory/recall/eval-cases/refresh after writing file-backed memory.")
		}
		if query == "" {
			addIssue(idx, rawCase, "missing_query", "case has no query", "Add a concrete query that can recover the expected memory file.")
		}
		if project == "" {
			addIssue(idx, rawCase, "missing_project", "case is not scoped to a project", "Set project to the memory project that owns the expected file, or refresh with {\"project\":\"<project>\"}.")
		}
		if topicPath == "" {
			addIssue(idx, rawCase, "missing_topic_path", "case has no topic_path", "Set topic_path to the durable memory topic for the expected file.")
		} else if topicPath == "root" || topicPath == "." {
			addIssue(idx, rawCase, "broad_root_topic", "case uses a broad root topic", "Use a concrete topic path such as runbooks/contextlattice/recall-quality-loop instead of root.")
		}
		if len(expectedFiles) == 0 {
			addIssue(idx, rawCase, "missing_expected_file", "case has no expected_files entry", "Set expected_files to exactly one durable memory file that the query should recover.")
		}
		if len(expectedFiles) > 1 {
			addIssue(idx, rawCase, "multi_file_rollup_case", "case expects multiple files from a broad rollup", "Split this into one case per expected file, or refresh from file-backed memory docs.")
		}
		if graphCase {
			graphCaseCount++
			if len(graphExpectedFiles) != 1 {
				addIssue(idx, rawCase, "invalid_graph_expected_file", "graph case must name exactly one graph_expected_files target", "Refresh with include_graph_cases=true or set one high-confidence neighboring memory file.")
			} else if len(expectedFiles) == 1 && strings.EqualFold(expectedFiles[0], graphExpectedFiles[0]) {
				addIssue(idx, rawCase, "graph_target_matches_seed", "graph case target matches its direct-recall seed", "Choose a distinct neighboring memory as graph_expected_files.")
			}
		}
	}
	status := "healthy"
	if len(issues) > 0 {
		status = "invalid"
	}
	return map[string]any{
		"valid":              len(issues) == 0,
		"status":             status,
		"case_count":         len(cfg.Cases),
		"invalid_case_count": len(invalidCases),
		"graph_case_count":   graphCaseCount,
		"issue_count":        len(issues),
		"issues":             issues,
		"agent_instructions": recallEvalCaseSetAgentInstructions(),
	}
}

func recallEvalNormalizeCaseTopic(value string) string {
	normalized := strings.Trim(strings.TrimSpace(strings.ReplaceAll(value, "\\", "/")), "/")
	for strings.Contains(normalized, "//") {
		normalized = strings.ReplaceAll(normalized, "//", "/")
	}
	return strings.ToLower(normalized)
}

func recallEvalCaseSetAgentInstructions() []string {
	return []string{
		"Refresh saved recall cases from live file-backed memory: POST /memory/recall/eval-cases/refresh with {\"project\":\"<project>\",\"topic_prefix\":\"<topic/path>\",\"max_cases\":12,\"min_hits\":1,\"include_graph_cases\":true,\"graph_max_cases\":3}.",
		"If refresh has no eligible memory, write durable memory first: POST /memory/write with projectName, fileName, topicPath, and content, then refresh the saved eval cases.",
		"Each saved recall eval case must include project, topic_path, query, limit, and exactly one expected_files item naming the file the query should recover.",
		"Do not use built-in fallback case IDs, empty project, topic_path root, or broad rollup cases with multiple expected_files.",
		"When authoring ORCH_RECALL_EVAL_CASES_PATH manually, split broad topics into one concrete file-backed case per expected memory file.",
	}
}

func resolveRecallEvalCasesPath() string {
	return resolveStoragePath("ORCH_RECALL_EVAL_CASES_PATH", defaultRecallEvalCasesRelativePath)
}

func (s *server) appendRecallMonitorSample(sample map[string]any) error {
	path := resolveStoragePath(
		"RECALL_MONITOR_PATH",
		filepath.Join("services", "orchestrator", "data", "recall_monitor.ndjson"),
	)
	if strings.TrimSpace(path) == "" {
		return nil
	}
	raw, err := json.Marshal(sample)
	if err != nil {
		return err
	}
	file, err := openOwnerOnlyAppend(path, false)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(append(raw, '\n')); err != nil {
		return err
	}
	return nil
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

func matchedExpectedFilesWithinK(results []map[string]any, expectedFiles map[string]struct{}, k int) []string {
	if len(expectedFiles) == 0 {
		return []string{}
	}
	matched := map[string]struct{}{}
	maxRank := clampInt(k, 1, 100)
	for idx, row := range results {
		if idx >= maxRank {
			break
		}
		rowFile := strings.Trim(strings.TrimSpace(strings.ToLower(anyToString(row["file"]))), "/")
		if rowFile == "" {
			continue
		}
		for candidate := range expectedFiles {
			if rowFile == candidate || strings.HasSuffix(rowFile, candidate) {
				matched[candidate] = struct{}{}
			}
		}
	}
	return sortedKeys(matched)
}

func uniqueSourcesWithinK(results []map[string]any, k int) []string {
	seen := map[string]struct{}{}
	maxRank := clampInt(k, 1, 100)
	for idx, row := range results {
		if idx >= maxRank {
			break
		}
		for _, source := range sourcesForRecallResult(row) {
			seen[source] = struct{}{}
		}
	}
	return sortedKeys(seen)
}

func sourcesForRecallResult(row map[string]any) []string {
	seen := map[string]struct{}{}
	for _, item := range anyToStringSlice(row["sources"]) {
		source := strings.TrimSpace(strings.ToLower(item))
		if source != "" {
			seen[source] = struct{}{}
		}
	}
	for _, key := range []string{"source", "retrieval_source"} {
		source := strings.TrimSpace(strings.ToLower(anyToString(row[key])))
		if source != "" {
			seen[source] = struct{}{}
		}
	}
	return sortedKeys(seen)
}

func recallResultMemoryID(row map[string]any) string {
	if memoryID := strings.TrimSpace(anyToString(row["memory_id"])); memoryID != "" {
		if _, _, canonical, _, err := canonicalMemoryID(memoryID); err == nil {
			return canonical
		}
		return memoryID
	}
	project := strings.TrimSpace(anyToString(row["project"]))
	fileName := strings.TrimSpace(anyToString(row["file"]))
	if project != "" && fileName != "" {
		if _, _, canonical, _, err := canonicalMemoryID(project + "::" + fileName); err == nil {
			return canonical
		}
		return project + "::" + fileName
	}
	return strings.TrimSpace(anyToString(row["id"]))
}

func recallEvalGraphNeighborLimit() int {
	return clampInt(envInt("ORCH_RECALL_EVAL_GRAPH_NEIGHBOR_LIMIT", 40), 1, 200)
}

func recallGraphContributionUnavailable(reason string) map[string]any {
	return map[string]any{
		"enabled":                           false,
		"reason":                            reason,
		"seed_count":                        0,
		"candidate_count":                   0,
		"added_candidate_count":             0,
		"expected_hit_count":                0,
		"added_expected_hit_count":          0,
		"edge_expected_match_count":         0,
		"hydrated_expected_hit_count":       0,
		"added_hydrated_expected_hit_count": 0,
		"helped":                            false,
		"relations":                         []string{},
	}
}

func (s *server) evaluateRecallGraphContribution(
	ctx context.Context,
	results []map[string]any,
	expectedFiles map[string]struct{},
	expectedTerms []string,
	k int,
	project string,
) map[string]any {
	if len(expectedFiles) == 0 && len(expectedTerms) == 0 {
		return recallGraphContributionUnavailable("no expectations")
	}
	backend := s.memoryGraphBackend()
	if backend == nil {
		return recallGraphContributionUnavailable("memory graph store unavailable")
	}
	maxRank := clampInt(k, 1, 100)
	seedIDs := make([]string, 0, maxRank)
	topIDs := map[string]struct{}{}
	for idx, row := range results {
		if idx >= maxRank {
			break
		}
		memoryID := recallResultMemoryID(row)
		if memoryID == "" {
			continue
		}
		if _, _, canonical, _, err := canonicalMemoryID(memoryID); err == nil {
			memoryID = canonical
		}
		if _, exists := topIDs[memoryID]; exists {
			continue
		}
		topIDs[memoryID] = struct{}{}
		seedIDs = append(seedIDs, memoryID)
	}
	if len(seedIDs) == 0 {
		return recallGraphContributionUnavailable("top results have no memory ids")
	}

	limit := recallEvalGraphNeighborLimit()
	relationCounts := map[string]int{}
	candidateRows := map[string]map[string]any{}
	candidateSeen := map[string]struct{}{}
	addedCandidateCount := 0
	expectedHitCount := 0
	addedExpectedHitCount := 0

	for _, seedID := range seedIDs {
		_, _, canonicalSeed, _, err := canonicalMemoryID(seedID)
		if err != nil {
			continue
		}
		edges, err := backend.listMemoryEdges(ctx, memoryEdgeQuery{
			MemoryID: canonicalSeed,
			Project:  project,
			Limit:    limit,
		})
		if err != nil {
			continue
		}
		for _, edge := range edges {
			relation := strings.TrimSpace(edge.Relation)
			if relation == "" {
				relation = "related"
			}
			relationCounts[relation] += 1
			candidateID := edge.TargetID
			if edge.TargetID == canonicalSeed {
				candidateID = edge.SourceID
			}
			projectName, fileName, canonicalCandidate, _, err := canonicalMemoryID(candidateID)
			if err != nil {
				continue
			}
			if _, exists := candidateSeen[canonicalCandidate]; exists {
				continue
			}
			candidateSeen[canonicalCandidate] = struct{}{}
			row := map[string]any{
				"memory_id":  canonicalCandidate,
				"project":    projectName,
				"file":       fileName,
				"source":     memoryEdgeSource,
				"summary":    "memory edge " + edge.SourceID + " -[" + edge.Relation + "]-> " + edge.TargetID,
				"score":      edge.Confidence,
				"relation":   relation,
				"edge_id":    edge.EdgeID,
				"created_at": edge.CreatedAt,
			}
			candidateRows[canonicalCandidate] = row
			if _, exists := topIDs[canonicalCandidate]; !exists {
				addedCandidateCount += 1
			}
		}
	}

	matchedCandidateIDs := make([]string, 0)
	addedMatchedCandidateIDs := make([]string, 0)
	edgeExpectedMatchCount := 0
	hydratedExpectedHitCount := 0
	addedHydratedExpectedHitCount := 0
	for candidateID, row := range candidateRows {
		if !resultHitsExpectations(row, expectedFiles, expectedTerms) {
			continue
		}
		edgeExpectedMatchCount += 1
		row = s.hydrateContextPackGraphNeighbor(row)
		candidateRows[candidateID] = row
		if !anyToBool(row["hydrated"]) {
			continue
		}
		expectedHitCount += 1
		hydratedExpectedHitCount += 1
		matchedCandidateIDs = append(matchedCandidateIDs, candidateID)
		if _, exists := topIDs[candidateID]; !exists {
			addedExpectedHitCount += 1
			addedHydratedExpectedHitCount += 1
			addedMatchedCandidateIDs = append(addedMatchedCandidateIDs, candidateID)
		}
	}
	sortStrings(matchedCandidateIDs)
	sortStrings(addedMatchedCandidateIDs)
	relations := make([]string, 0, len(relationCounts))
	for relation := range relationCounts {
		relations = append(relations, relation)
	}
	sortStrings(relations)
	return map[string]any{
		"enabled":                           true,
		"seed_count":                        len(seedIDs),
		"candidate_count":                   len(candidateRows),
		"added_candidate_count":             addedCandidateCount,
		"expected_hit_count":                expectedHitCount,
		"added_expected_hit_count":          addedExpectedHitCount,
		"edge_expected_match_count":         edgeExpectedMatchCount,
		"hydrated_expected_hit_count":       hydratedExpectedHitCount,
		"added_hydrated_expected_hit_count": addedHydratedExpectedHitCount,
		"helped":                            addedHydratedExpectedHitCount > 0,
		"relations":                         relations,
		"relation_counts":                   relationCounts,
		"matched_memory_ids":                matchedCandidateIDs,
		"added_matched_memory_ids":          addedMatchedCandidateIDs,
	}
}

func recallLatencyStats(values []float64) (float64, float64) {
	clean := make([]float64, 0, len(values))
	sum := 0.0
	for _, value := range values {
		if value < 0 {
			continue
		}
		clean = append(clean, value)
		sum += value
	}
	if len(clean) == 0 {
		return 0, 0
	}
	sort.Float64s(clean)
	return sum / float64(len(clean)), percentileFloat(clean, 0.95)
}

func recallEvalQualityStatus(passed bool, evaluatedCases int, recallAtK float64, mrr float64, numericExactness float64) string {
	if evaluatedCases == 0 {
		return "insufficient_cases"
	}
	if passed &&
		recallAtK >= defaultRecallEvalGateMinRecallAtK &&
		mrr >= defaultRecallEvalGateMinMRR &&
		numericExactness >= defaultRecallEvalGateMinNumeric {
		return "healthy"
	}
	if recallAtK < 0.5 || mrr < 0.35 || numericExactness < 0.8 {
		return "repair_recommended"
	}
	return "watch"
}

func recallEvalRecommendations(metrics map[string]any, gate recallEvalGate, passed bool) []string {
	recommendations := make([]string, 0, 5)
	recallAtK := anyToFloat64(metrics["recallAtK"], 0)
	mrr := anyToFloat64(metrics["mrr"], 0)
	citationCoverage := anyToFloat64(metrics["citationCoverage"], 1)
	sourceDiversity := anyToFloat64(metrics["sourceDiversity"], 0)
	graphLift := anyToFloat64(metrics["graphLift"], 0)
	p95LatencyMs := anyToFloat64(metrics["p95LatencyMs"], 0)
	if recallAtK < gate.MinRecallAtK {
		recommendations = append(recommendations, "Refresh saved eval cases, then inspect source coverage for queries below recall gate.")
	}
	if mrr < gate.MinMRR {
		recommendations = append(recommendations, "Tune ranking weights or source ordering; hits are present but not surfacing early enough.")
	}
	if citationCoverage < 0.9 {
		recommendations = append(recommendations, "Increase citation-backed retrieval coverage for cases with expected files.")
	}
	if graphLift > 0 {
		recommendations = append(recommendations, "Graph neighbors add recall coverage; keep first-hop memory-edge expansion available for agent context packs.")
	}
	if sourceDiversity < 1.5 && anyToInt(metrics["casesEvaluated"], 0) > 0 {
		recommendations = append(recommendations, "Source diversity is low; verify qdrant, pgvector, and topic rollup lanes are available for this profile.")
	}
	if p95LatencyMs > 5000 {
		recommendations = append(recommendations, "Recall eval p95 latency is elevated; inspect /telemetry/retrieval/source-quality before widening fanout.")
	}
	if len(recommendations) == 0 {
		if passed {
			recommendations = append(recommendations, "Recall quality is inside the saved gate; keep scheduled evaluation and graph quality repair enabled.")
		} else {
			recommendations = append(recommendations, "Recall gate failed without a dominant signal; inspect per-case failures and retrieval debug.")
		}
	}
	return recommendations
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
