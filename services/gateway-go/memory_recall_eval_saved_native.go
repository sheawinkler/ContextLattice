package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
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
	Path          string
	SchemaID      string
	Version       any
	UpdatedAt     any
	CaseSetDigest string
	Source        string
	Synthetic     bool
	Snapshot      map[string]any
	Custody       map[string]any
	SplitCounts   map[string]any
	K             int
	Gate          recallEvalGate
	Cases         []map[string]any
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
	comparatorAuthority := contextPackLearnedComparatorAuthorityForRequest(s, r, payload)
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
	evaluationSplit := strings.ToLower(strings.TrimSpace(anyToString(payload["split"])))
	if evaluationSplit == "all" {
		evaluationSplit = ""
	}
	if evaluationSplit != "" && evaluationSplit != "train" && evaluationSplit != "holdout" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported saved recall evaluation split", "supported_splits": []string{"all", "train", "holdout"}})
		return
	}
	evaluationCases := cfg.Cases
	if evaluationSplit != "" {
		evaluationCases = recallEvalCasesForSplit(cfg.Cases, evaluationSplit)
		if len(evaluationCases) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":                  "requested saved recall evaluation split is empty",
				"code":                   "empty_evaluation_split",
				"split":                  evaluationSplit,
				"case_set_digest":        cfg.CaseSetDigest,
				"available_split_counts": cfg.SplitCounts,
			})
			return
		}
	}
	evaluationCfg := cfg
	evaluationCfg.Cases = evaluationCases
	actuatorRows := []map[string]any{}
	if s != nil && s.contextPackQuality != nil {
		actuatorRows, _ = s.contextPackQuality.receiptDurableOutcomeRows(evidenceReputationMaxRows)
	}
	actuatorRows = reconcileCandidateUtilityVerification(actuatorRows, utilityFromServer(s))
	impactComparison := newSavedRecallImpactComparisonWithOutcomeRowsAndAuthority(
		evaluationCfg, actuatorRows, time.Now().UTC(), comparatorAuthority,
	)

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
	if comparatorAuthority.Authorized {
		// The authority artifact is evaluated only through the fixed, server-owned
		// profile. Preserve the caller's diagnostic options on every other run.
		incomingHeaders = comparatorAuthority.Headers
		includePreferences = false
		userID = ""
	}

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
	latencyValues := make([]float64, 0, len(evaluationCases))
	graphEvaluatedCases := 0
	graphSeedCount := 0
	graphCandidateCount := 0
	graphAddedCandidateCount := 0
	graphExpectedHitCount := 0
	graphAddedExpectedHitCount := 0
	graphHelpedCases := 0
	graphExplicitCases := 0
	caseReports := make([]map[string]any, 0, len(evaluationCases))
	ablationReports := make([]map[string]any, 0, len(evaluationCases))
	ablationRowsUsed := 0

	// Keep tiny diagnostic fixtures sequential so legacy test/inspection
	// backends that intentionally use unsynchronized capture variables retain
	// their deterministic behavior. Full v3 refreshes (the bounded 300-case
	// path) use the worker pool below.
	if len(evaluationCases) > savedRecallEvalWorkerCount {
		// Retrieval remains parallel, but ablation rows are materialized in this
		// deterministic aggregation lane so max_ablation_rows is a global cap,
		// never a per-case multiplier.
		parallelResults := evaluateSavedRecallCasesConcurrently(r.Context(), s, evaluationCases, k, incomingHeaders, includeRetrievalDebug, includePreferences, userID)
		for _, result := range parallelResults {
			if strings.TrimSpace(anyToString(result.rawCase["query"])) != "" {
				latencyValues = append(latencyValues, result.latencyMs)
			}
			if includeAblation && result.searchResponse != nil && ablationRowsUsed < maxAblationRows {
				result.ablation = buildSavedRecallEvalAblation(result.rawCase, result.results, k, maxAblationRows-ablationRowsUsed)
				if result.ablation != nil {
					if result.report != nil {
						result.report["ablation"] = result.ablation
					}
					ablationReports = append(ablationReports, result.ablation)
					ablationRowsUsed += savedRecallEvalAblationRowCount(result.ablation)
				}
			}
			caseReports = append(caseReports, result.report)
			if result.retrievalFailed {
				impactComparison.invalidateCase(result.rawCase, "retrieval_failed")
				continue
			}
			if result.searchResponse == nil {
				continue
			}
			if result.hasExpectations {
				evaluatedCases++
				if result.hit {
					recallHits++
				} else {
					noHitCases++
				}
				reciprocalRankSum += result.reciprocalRank
				if result.lowConfidence {
					lowConfidenceCases++
				}
				sourceDiversitySum += float64(result.sourceDiversity)
				if result.graphEligible {
					graphEvaluatedCases++
					graphSeedCount += anyToInt(result.graphContribution["seed_count"], 0)
					graphCandidateCount += anyToInt(result.graphContribution["candidate_count"], 0)
					graphAddedCandidateCount += anyToInt(result.graphContribution["added_candidate_count"], 0)
					graphExpectedHitCount += anyToInt(result.graphContribution["expected_hit_count"], 0)
					graphAddedExpectedHitCount += anyToInt(result.graphContribution["added_expected_hit_count"], 0)
					if result.graphExplicit {
						graphExplicitCases++
					}
					if result.graphHelped {
						graphHelpedCases++
					}
				}
			}
			numericExpectedTotal += result.numericExpected
			numericMatchedTotal += result.numericMatches
			citationExpectedTotal += result.citationExpected
			citationMatchedTotal += result.citationMatched
			impactComparison.addCase(result.index, result.rawCase, result.results, result.searchIntelligence, result.expectedNumeric, result.latencyMs)
			impactComparison.addActuatorCase(result.index, result.rawCase, result.searchResponse)
		}
	} else {
		for idx, rawCase := range evaluationCases {
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
				"deep_async":              false,
				"callback_url":            "",
				"traffic_class":           "synthetic",
			}
			applySavedRecallEvalCaseOptionalRetrievalFlags(reqPayload, rawCase)
			if strings.TrimSpace(anyToString(reqPayload["retrieval_intent"])) == "" {
				reqPayload["retrieval_intent"] = "decision"
			}

			caseStartedAt := time.Now()
			searchResp, status, execErr := s.executeRetrieval(
				r.Context(),
				incomingHeaders,
				reqPayload,
				true,
			)
			latencyMs := float64(time.Since(caseStartedAt).Microseconds()) / 1000.0
			latencyValues = append(latencyValues, latencyMs)
			if execErr != nil {
				impactComparison.invalidateCase(rawCase, "retrieval_failed")
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
				r.Context(),
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
			if includeAblation && ablationRowsUsed < maxAblationRows {
				ablation := buildSavedRecallEvalAblation(rawCase, results, k, maxAblationRows-ablationRowsUsed)
				if ablation != nil {
					report["ablation"] = ablation
					ablationReports = append(ablationReports, ablation)
					ablationRowsUsed += savedRecallEvalAblationRowCount(ablation)
				}
			}
			impactComparison.addCase(idx, rawCase, results, anyMap(searchResp["search_intelligence"]), expectedNumeric, latencyMs)
			impactComparison.addActuatorCase(idx, rawCase, searchResp)
			caseReports = append(caseReports, report)
		}
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
	evaluationWorkers := 1
	if len(evaluationCases) > savedRecallEvalWorkerCount {
		evaluationWorkers = minInt(savedRecallEvalWorkerCount, len(evaluationCases))
	}
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
		"casesTotal":             len(evaluationCases),
		"evaluationSplit":        firstNonEmptyStrings(evaluationSplit, "all"),
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
		"evaluationWorkers":      evaluationWorkers,
		"evaluationMode":         map[bool]string{true: "bounded_worker_pool", false: "sequential_fixture"}[evaluationWorkers > 1],
		"caseCap":                savedRecallEvalV3MaxCases,
		"ablationRows":           ablationRowsUsed,
		"ablationRowCap":         maxAblationRows,
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
	monitorSample := map[string]any{
		"timestamp":             nowUTCISO(),
		"source":                "saved_recall_eval",
		"case_set_schema_id":    cfg.SchemaID,
		"case_set_digest":       cfg.CaseSetDigest,
		"benchmark_eligible":    anyToBool(caseSetHealth["benchmark_eligible"]),
		"snapshot":              cloneAnyMap(cfg.Snapshot),
		"custody":               cloneAnyMap(cfg.Custody),
		"passed":                passed,
		"qualityStatus":         qualityStatus,
		"caseCount":             len(evaluationCases),
		"evaluationSplit":       firstNonEmptyStrings(evaluationSplit, "all"),
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
	}
	impactArtifact := impactComparison.monitorFields(len(evaluationCases))
	for key, value := range impactArtifact {
		monitorSample[key] = value
	}
	if err := s.appendRecallMonitorSample(monitorSample); err != nil {
		s.writeRecallEvalPersistenceUnavailable(w)
		return
	}

	response := map[string]any{
		"ok":                              true,
		"passed":                          passed,
		"quality_status":                  qualityStatus,
		"metrics":                         metrics,
		"recommendations":                 recommendations,
		"search_impact_shadow_evaluation": cloneJSONMap(impactArtifact),
		"gate": map[string]any{
			"minRecallAtK":        gate.MinRecallAtK,
			"minMrr":              gate.MinMRR,
			"minNumericExactness": gate.MinNumericExactly,
			"graphRequired":       graphRequired,
			"minGraphLift":        0.0,
		},
		"cases": caseReports,
		"savedCaseSet": map[string]any{
			"case_set_id":        ownerOnlyStoreRef("recall_eval_cases"),
			"schema_id":          cfg.SchemaID,
			"version":            cfg.Version,
			"updatedAt":          cfg.UpdatedAt,
			"case_set_digest":    cfg.CaseSetDigest,
			"snapshot":           cloneAnyMap(cfg.Snapshot),
			"custody":            cloneAnyMap(cfg.Custody),
			"benchmark_eligible": anyToBool(caseSetHealth["benchmark_eligible"]),
			"count":              len(cfg.Cases),
			"evaluation_count":   len(evaluationCases),
			"evaluation_split":   firstNonEmptyStrings(evaluationSplit, "all"),
			"evaluation_workers": evaluationWorkers,
			"evaluation_mode":    map[bool]string{true: "bounded_worker_pool", false: "sequential_fixture"}[evaluationWorkers > 1],
		},
		"activation_authority": map[string]any{
			"requested":  contextPackLearnedComparatorAuthorityRequested(payload),
			"authorized": comparatorAuthority.Authorized,
			"reason":     comparatorAuthority.Reason,
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

func applySavedRecallEvalCaseOptionalRetrievalFlags(reqPayload map[string]any, rawCase map[string]any) {
	if reqPayload == nil || rawCase == nil {
		return
	}
	// Preserve the product default when a saved case omits the flag. An
	// explicit false remains a real experiment setting and must reach the
	// retrieval boundary unchanged.
	if value, present := rawCase["query_expansion"]; present {
		reqPayload["query_expansion"] = anyToBool(value)
	}
}

func recallEvalCasesForSplit(cases []map[string]any, split string) []map[string]any {
	split = strings.ToLower(strings.TrimSpace(split))
	if split == "" || split == "all" {
		return cases
	}
	filtered := make([]map[string]any, 0, len(cases))
	for _, rawCase := range cases {
		if strings.EqualFold(strings.TrimSpace(anyToString(rawCase["split"])), split) {
			filtered = append(filtered, rawCase)
		}
	}
	return filtered
}

const savedRecallEvalWorkerCount = 4

type savedRecallEvalCaseResult struct {
	index              int
	rawCase            map[string]any
	report             map[string]any
	ablation           map[string]any
	results            []map[string]any
	searchIntelligence map[string]any
	searchResponse     map[string]any
	expectedNumeric    []string
	latencyMs          float64
	retrievalFailed    bool
	hit                bool
	reciprocalRank     float64
	hasExpectations    bool
	numericMatches     int
	numericExpected    int
	citationMatched    int
	citationExpected   int
	noHit              bool
	lowConfidence      bool
	sourceDiversity    int
	graphEligible      bool
	graphContribution  map[string]any
	graphExplicit      bool
	graphHelped        bool
}

func evaluateSavedRecallCasesConcurrently(
	ctx context.Context,
	s *server,
	cases []map[string]any,
	k int,
	incomingHeaders http.Header,
	includeRetrievalDebug bool,
	includePreferences bool,
	userID string,
) []savedRecallEvalCaseResult {
	if len(cases) == 0 {
		return []savedRecallEvalCaseResult{}
	}
	workerCount := minInt(savedRecallEvalWorkerCount, len(cases))
	jobs := make(chan int)
	results := make(chan savedRecallEvalCaseResult, len(cases))
	var workers sync.WaitGroup
	for worker := 0; worker < workerCount; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for idx := range jobs {
				results <- evaluateSavedRecallCase(ctx, s, idx, cases[idx], k, incomingHeaders, includeRetrievalDebug, includePreferences, userID)
			}
		}()
	}
	go func() {
		defer close(jobs)
		for idx := range cases {
			select {
			case jobs <- idx:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(results)
	}()
	out := make([]savedRecallEvalCaseResult, 0, len(cases))
	for result := range results {
		out = append(out, result)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].index < out[j].index })
	return out
}

func evaluateSavedRecallCase(
	ctx context.Context,
	s *server,
	idx int,
	rawCase map[string]any,
	k int,
	incomingHeaders http.Header,
	includeRetrievalDebug bool,
	includePreferences bool,
	userID string,
) savedRecallEvalCaseResult {
	result := savedRecallEvalCaseResult{index: idx, rawCase: rawCase}
	caseID := strings.TrimSpace(anyToString(rawCase["id"]))
	if caseID == "" {
		caseID = fmt.Sprintf("case-%d", idx+1)
	}
	query := strings.TrimSpace(anyToString(rawCase["query"]))
	if query == "" {
		result.report = map[string]any{
			"id": caseID, "query": "", "k": k, "hit": false, "matched_rank": nil,
			"reciprocal_rank": 0.0, "has_expectations": false, "result_count": 0,
			"top_score": 0.0, "expected_files": []string{}, "expected_substrings": []string{},
			"expected_numeric": []string{}, "matched_numeric": []string{}, "matched_files": []string{},
			"citation_coverage": 0.0, "source_diversity": 0, "latency_ms": 0.0,
			"graph_contribution": recallGraphContributionUnavailable("case query missing"),
			"warnings":           []string{"case query missing"},
			"retrieval_mode":     normalizeRetrievalMode(anyToString(rawCase["retrieval_mode"])),
			"agent_id":           strings.TrimSpace(anyToString(rawCase["agent_id"])),
		}
		return result
	}
	directLimit := maxInt(clampInt(anyToInt(rawCase["limit"], 10), 1, 100), k)
	reqPayload := map[string]any{
		"query": query, "limit": directLimit,
		"project":              strings.TrimSpace(anyToString(rawCase["project"])),
		"topic_path":           strings.TrimSpace(anyToString(rawCase["topic_path"])),
		"retrieval_mode":       normalizeRetrievalMode(anyToString(rawCase["retrieval_mode"])),
		"retrieval_intent":     strings.TrimSpace(strings.ToLower(anyToString(rawCase["retrieval_intent"]))),
		"sources":              anyToStringSlice(rawCase["sources"]),
		"source_weights":       cloneAnyMap(anyMap(rawCase["source_weights"])),
		"rerank_with_learning": true, "include_retrieval_debug": includeRetrievalDebug,
		"include_grounding": true, "include_preferences": includePreferences,
		"user_id": userID, "agent_id": strings.TrimSpace(anyToString(rawCase["agent_id"])),
		"auto_escalate": anyToBool(rawCase["auto_escalate"]), "deep_async": false,
		"callback_url": "", "traffic_class": "synthetic",
	}
	applySavedRecallEvalCaseOptionalRetrievalFlags(reqPayload, rawCase)
	if strings.TrimSpace(anyToString(reqPayload["retrieval_intent"])) == "" {
		reqPayload["retrieval_intent"] = "decision"
	}
	caseStartedAt := time.Now()
	searchResp, status, execErr := s.executeRetrieval(ctx, incomingHeaders, reqPayload, true)
	result.latencyMs = float64(time.Since(caseStartedAt).Microseconds()) / 1000.0
	if execErr != nil {
		result.retrievalFailed = true
		result.report = map[string]any{
			"id": caseID, "query": query, "k": k, "hit": false, "matched_rank": nil,
			"reciprocal_rank": 0.0, "has_expectations": false, "result_count": 0,
			"top_score": 0.0, "expected_files": []string{}, "expected_substrings": []string{},
			"expected_numeric": []string{}, "matched_numeric": []string{}, "matched_files": []string{},
			"citation_coverage": 0.0, "source_diversity": 0,
			"latency_ms":         roundFloat(result.latencyMs, 3),
			"graph_contribution": recallGraphContributionUnavailable("retrieval failed"),
			"warnings":           []string{"retrieval failed: " + execErr.Error()},
			"retrieval_mode":     reqPayload["retrieval_mode"], "agent_id": reqPayload["agent_id"],
			"status_code": status,
		}
		return result
	}
	result.searchResponse = searchResp
	result.searchIntelligence = anyMap(searchResp["search_intelligence"])
	result.results = parseRows(searchResp["results"])
	grounding := anyMap(searchResp["grounding"])
	expectedFiles := normalizeExpectedFileTokens(rawCase["expected_files"])
	expectedTerms := normalizeExpectedTerms(rawCase["expected_substrings"])
	result.expectedNumeric = normalizeExpectedNumeric(rawCase["expected_numeric"])
	graphExpectedFiles := normalizeExpectedFileTokens(rawCase["graph_expected_files"])
	graphExpectedTerms := normalizeExpectedTerms(rawCase["graph_expected_substrings"])
	reportedGraphExpectedFiles := sortedKeys(graphExpectedFiles)
	reportedGraphExpectedTerms := append([]string(nil), graphExpectedTerms...)
	hasExplicitGraphExpectations := len(graphExpectedFiles) > 0 || len(graphExpectedTerms) > 0
	if !hasExplicitGraphExpectations {
		graphExpectedFiles = expectedFiles
		graphExpectedTerms = expectedTerms
	}
	matchedFiles := matchedExpectedFilesWithinK(result.results, expectedFiles, k)
	caseCitationCoverage := 1.0
	if len(expectedFiles) > 0 {
		caseCitationCoverage = float64(len(matchedFiles)) / float64(len(expectedFiles))
	}
	caseSources := uniqueSourcesWithinK(result.results, k)
	graphContribution := s.evaluateRecallGraphContribution(ctx, result.results, graphExpectedFiles, graphExpectedTerms, k, strings.TrimSpace(anyToString(reqPayload["project"])))
	if hasExplicitGraphExpectations {
		graphContribution["expectation_mode"] = "explicit_graph"
	} else {
		graphContribution["expectation_mode"] = "direct_fallback"
	}
	result.graphContribution = graphContribution
	matchedRank := matchRankWithinK(result.results, expectedFiles, expectedTerms, k)
	result.hit = matchedRank != nil
	if matchedRank != nil && *matchedRank > 0 {
		result.reciprocalRank = 1.0 / float64(*matchedRank)
	}
	result.hasExpectations = len(expectedFiles) > 0 || len(expectedTerms) > 0
	result.numericMatches = len(matchedNumericFacts(grounding, result.expectedNumeric))
	result.numericExpected = len(result.expectedNumeric)
	result.citationMatched = len(matchedFiles)
	result.citationExpected = len(expectedFiles)
	result.noHit = result.hasExpectations && !result.hit
	result.lowConfidence = result.hasExpectations && topResultScore(result.results) > 0 && topResultScore(result.results) < 0.45
	result.sourceDiversity = len(caseSources)
	result.graphExplicit = hasExplicitGraphExpectations
	result.graphEligible = hasExplicitGraphExpectations
	result.graphHelped = anyToBool(graphContribution["helped"])
	report := map[string]any{
		"id": caseID, "query": query, "k": k, "hit": result.hit, "matched_rank": matchedRank,
		"reciprocal_rank": roundFloat(result.reciprocalRank, 6), "has_expectations": result.hasExpectations,
		"result_count": len(result.results), "top_score": roundFloat(topResultScore(result.results), 6),
		"expected_files": sortedKeys(expectedFiles), "expected_substrings": expectedTerms,
		"graph_expected_files": reportedGraphExpectedFiles, "graph_expected_substrings": reportedGraphExpectedTerms,
		"graph_effective_expected_files": sortedKeys(graphExpectedFiles), "graph_effective_expected_substrings": graphExpectedTerms,
		"graph_expectations_explicit": hasExplicitGraphExpectations, "expected_numeric": result.expectedNumeric,
		"matched_numeric": matchedNumericFacts(grounding, result.expectedNumeric), "matched_files": matchedFiles,
		"citation_coverage": roundFloat(caseCitationCoverage, 6), "source_diversity": len(caseSources),
		"sources": caseSources, "latency_ms": roundFloat(result.latencyMs, 3),
		"graph_contribution": graphContribution, "warnings": parseWarnings(searchResp["warnings"]),
		"retrieval_mode": searchResp["retrieval_mode"], "agent_id": searchResp["agent_id"],
		"retry_attempts": 0, "transient_retry_triggered": false, "transient_retry_recovered": false,
		"attempt_modes": []string{normalizeRetrievalMode(anyToString(searchResp["retrieval_mode"]))},
	}
	if includeRetrievalDebug {
		report["retrieval"] = searchResp["retrieval_debug"]
	}
	result.report = report
	return result
}

func buildSavedRecallEvalAblation(rawCase map[string]any, results []map[string]any, k int, maxRows int) map[string]any {
	if maxRows < 1 {
		return nil
	}
	caseID := strings.TrimSpace(anyToString(rawCase["id"]))
	if caseID == "" {
		caseID = "case-unknown"
	}
	return attachPayloadFormatContract(retrievalAblationSchemaID, buildRetrievalAblation(retrievalAblationInput{
		CaseID:         caseID,
		Results:        results,
		ExpectedFiles:  sortedKeys(normalizeExpectedFileTokens(rawCase["expected_files"])),
		K:              k,
		TrafficClass:   "synthetic",
		SnapshotStable: true,
		MaxRows:        maxRows,
	}), "", "retrieval_ablation", "/memory/recall/evaluate/saved")
}

func savedRecallEvalAblationRowCount(report map[string]any) int {
	if report == nil {
		return 0
	}
	return anyToInt(anyMap(report["summary"])["evaluated_target_count"], 0)
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
	impactComparison := newSavedRecallImpactComparison(cfg)
	impactComparison.invalidate("case_set_invalid")
	monitorSample := map[string]any{
		"timestamp":           nowUTCISO(),
		"source":              "saved_recall_eval",
		"case_set_schema_id":  cfg.SchemaID,
		"case_set_digest":     cfg.CaseSetDigest,
		"benchmark_eligible":  anyToBool(health["benchmark_eligible"]),
		"snapshot":            cloneAnyMap(cfg.Snapshot),
		"custody":             cloneAnyMap(cfg.Custody),
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
	}
	impactArtifact := impactComparison.monitorFields(len(cfg.Cases))
	for key, value := range impactArtifact {
		monitorSample[key] = value
	}
	if err := s.appendRecallMonitorSample(monitorSample); err != nil {
		s.writeRecallEvalPersistenceUnavailable(w)
		return
	}
	response := map[string]any{
		"ok":                              false,
		"passed":                          false,
		"failed_fast":                     true,
		"quality_status":                  qualityStatus,
		"error":                           "saved recall eval case set failed input health validation",
		"case_set_health":                 health,
		"agent_instructions":              instructions,
		"recommendations":                 instructions,
		"search_impact_shadow_evaluation": cloneJSONMap(impactArtifact),
		"metrics":                         metrics,
		"gate": map[string]any{
			"minRecallAtK":        cfg.Gate.MinRecallAtK,
			"minMrr":              cfg.Gate.MinMRR,
			"minNumericExactness": cfg.Gate.MinNumericExactly,
		},
		"cases": []any{},
		"savedCaseSet": map[string]any{
			"case_set_id":        ownerOnlyStoreRef("recall_eval_cases"),
			"schema_id":          cfg.SchemaID,
			"version":            cfg.Version,
			"updatedAt":          cfg.UpdatedAt,
			"case_set_digest":    cfg.CaseSetDigest,
			"snapshot":           cloneAnyMap(cfg.Snapshot),
			"custody":            cloneAnyMap(cfg.Custody),
			"benchmark_eligible": anyToBool(health["benchmark_eligible"]),
			"count":              len(cfg.Cases),
		},
	}
	writeJSON(w, http.StatusOK, response)
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
		Path:          path,
		SchemaID:      strings.TrimSpace(anyToString(payload["schema_id"])),
		Version:       payload["version"],
		UpdatedAt:     payload["updatedAt"],
		CaseSetDigest: strings.TrimSpace(anyToString(payload["case_set_digest"])),
		Source:        strings.TrimSpace(anyToString(payload["source"])),
		Synthetic:     anyToBool(payload["synthetic"]),
		Snapshot:      cloneAnyMap(anyMap(payload["snapshot"])),
		Custody:       cloneAnyMap(anyMap(payload["custody"])),
		SplitCounts:   cloneAnyMap(anyMap(payload["split_counts"])),
		K:             clampInt(anyToInt(payload["k"], defaultRecallEvalK), 1, 20),
		Gate: recallEvalGate{
			MinRecallAtK:      clampFloat(parseAnyFloat(gateRaw["minRecallAtK"], defaultRecallEvalGateMinRecallAtK), 0.0, 1.0),
			MinMRR:            clampFloat(parseAnyFloat(gateRaw["minMrr"], defaultRecallEvalGateMinMRR), 0.0, 1.0),
			MinNumericExactly: clampFloat(parseAnyFloat(gateRaw["minNumericExactness"], defaultRecallEvalGateMinNumeric), 0.0, 1.0),
		},
		Cases: cases,
	}
	return cfg, nil
}

func defaultSavedRecallEvalConfig(path string) recallEvalSavedConfig {
	return recallEvalSavedConfig{
		Path:      path,
		SchemaID:  savedRecallEvalV3SchemaID,
		Version:   savedRecallEvalV3Version,
		UpdatedAt: nowUTCISO(),
		Source:    "no_live_case_set",
		Synthetic: false,
		K:         defaultRecallEvalK,
		Gate: recallEvalGate{
			MinRecallAtK:      defaultRecallEvalGateMinRecallAtK,
			MinMRR:            defaultRecallEvalGateMinMRR,
			MinNumericExactly: defaultRecallEvalGateMinNumeric,
		},
		Cases: []map[string]any{},
	}
}

func validateSavedRecallEvalCaseSet(cfg recallEvalSavedConfig) map[string]any {
	issues := make([]map[string]any, 0)
	invalidCases := map[int]struct{}{}
	graphCaseCount := 0
	caseIDs := map[string]int{}
	directFileKeys := map[string]int{}
	directQueryKeys := map[string]int{}
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
	addGlobalIssue := func(code string, detail string, fix string) {
		issues = append(issues, map[string]any{
			"case_index": -1,
			"case_id":    "case_set",
			"code":       code,
			"detail":     detail,
			"fix":        fix,
		})
	}
	version := anyToInt(cfg.Version, 0)
	v3 := version >= savedRecallEvalV3Version || strings.TrimSpace(cfg.SchemaID) == savedRecallEvalV3SchemaID
	if len(cfg.Cases) == 0 {
		addGlobalIssue("no_live_cases", "saved case set contains no live file-backed evaluation cases", "Write durable memory and refresh; an empty or synthetic set is not a benchmark.")
	}
	if cfg.Synthetic {
		addGlobalIssue("synthetic_case_set", "synthetic or built-in cases are not benchmark eligible", "Refresh from the live indexed memory store and retain the frozen snapshot custody metadata.")
	}
	if v3 {
		if strings.TrimSpace(cfg.SchemaID) != savedRecallEvalV3SchemaID {
			addGlobalIssue("schema_version_mismatch", "v3 case set is missing the saved_recall_eval_case_set.v3 schema id", "Refresh the case set using the v3 native refresh route.")
		}
		if strings.TrimSpace(cfg.CaseSetDigest) == "" {
			addGlobalIssue("missing_case_set_digest", "v3 case set has no frozen case-set digest", "Refresh and persist the complete v3 case set including case_set_digest.")
		} else if expected := "sha256:" + recallEvalCaseSetDigest(cfg.Cases); !strings.EqualFold(strings.TrimSpace(cfg.CaseSetDigest), expected) {
			addGlobalIssue("case_set_digest_mismatch", "case_set_digest does not match the persisted cases", "Do not edit saved cases in place; regenerate the frozen case set.")
		}
		if len(cfg.Snapshot) == 0 {
			addGlobalIssue("missing_snapshot_metadata", "v3 case set has no frozen source snapshot metadata", "Refresh from the indexed memory store so source scope and snapshot digest are recorded.")
		}
		diversity := anyMap(cfg.Snapshot["diversity"])
		if len(diversity) > 0 && !anyToBool(diversity["valid"]) {
			addGlobalIssue("insufficient_diversity", "v3 case set did not meet its available population diversity minima", "Increase the bounded sample or refresh after more independent projects, topics, agents, sessions, and time horizons are indexed.")
		}
		if len(cfg.Custody) == 0 || anyToBool(cfg.Custody["synthetic"]) {
			addGlobalIssue("invalid_custody_metadata", "v3 case set custody is missing or marked synthetic", "Persist gateway-go frozen-live-index custody metadata; do not benchmark synthetic fallback cases.")
		}
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
		if caseID == "" {
			addIssue(idx, rawCase, "missing_case_id", "case has no stable id", "Refresh the case set; v3 ids are deterministic hashes of the frozen source identity.")
		} else if prior, exists := caseIDs[strings.ToLower(caseID)]; exists {
			addIssue(idx, rawCase, "duplicate_case_id", fmt.Sprintf("case id duplicates case %d", prior), "Regenerate the case set or give each direct source file one stable case id.")
		} else {
			caseIDs[strings.ToLower(caseID)] = idx
		}
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
		if !graphCase && len(expectedFiles) == 1 {
			fileKey := strings.ToLower(project + "\x00" + expectedFiles[0])
			if prior, exists := directFileKeys[fileKey]; exists {
				addIssue(idx, rawCase, "duplicate_expected_file", fmt.Sprintf("direct case reuses expected file from case %d", prior), "Keep one direct case per project/file in the frozen case set.")
			} else {
				directFileKeys[fileKey] = idx
			}
			queryKey := strings.ToLower(project + "\x00" + strings.Join(strings.Fields(query), " "))
			if prior, exists := directQueryKeys[queryKey]; exists {
				addIssue(idx, rawCase, "duplicate_query", fmt.Sprintf("direct query duplicates case %d", prior), "Use deterministic stratified refresh; duplicate queries do not add evaluation coverage.")
			} else {
				directQueryKeys[queryKey] = idx
			}
			if recallEvalQueryContainsExpectedFile(query, expectedFiles[0]) {
				addIssue(idx, rawCase, "query_contains_expected_file", "query contains the exact expected file name", "Refresh with filename-redacted query derivation; do not use the expected file as an oracle.")
			}
		}
		if v3 {
			split := strings.ToLower(strings.TrimSpace(anyToString(rawCase["split"])))
			if split != "train" && split != "holdout" {
				addIssue(idx, rawCase, "missing_split", "v3 case has no train or temporal holdout split", "Refresh the case set with the v3 temporal split generator.")
			}
			if split == "holdout" && strings.TrimSpace(anyToString(rawCase["source_updated_at"])) == "" {
				addIssue(idx, rawCase, "holdout_missing_timestamp", "temporal holdout case has no source timestamp", "Keep only timestamped newest cases in the temporal holdout split.")
			}
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
		"benchmark_eligible": len(issues) == 0 && len(cfg.Cases) > 0 && !cfg.Synthetic,
		"status":             status,
		"schema_id":          cfg.SchemaID,
		"version":            cfg.Version,
		"source":             cfg.Source,
		"synthetic":          cfg.Synthetic,
		"case_set_digest":    cfg.CaseSetDigest,
		"snapshot":           cloneAnyMap(cfg.Snapshot),
		"custody":            cloneAnyMap(cfg.Custody),
		"case_count":         len(cfg.Cases),
		"invalid_case_count": len(invalidCases),
		"graph_case_count":   graphCaseCount,
		"issue_count":        len(issues),
		"issues":             issues,
		"agent_instructions": recallEvalCaseSetAgentInstructions(),
	}
}

func recallEvalQueryContainsExpectedFile(query string, fileName string) bool {
	queryTokens := map[string]struct{}{}
	for _, token := range strings.Fields(strings.ToLower(strings.TrimSpace(query))) {
		queryTokens[strings.Trim(token, "\t\r\n .,;:!?()[]{}\\\"'")] = struct{}{}
	}
	cleanName := strings.ToLower(strings.Trim(strings.TrimSpace(strings.ReplaceAll(fileName, "\\", "/")), "/"))
	baseName := filepath.Base(cleanName)
	for _, token := range []string{cleanName, baseName} {
		if len(token) >= 3 {
			if _, exists := queryTokens[token]; exists {
				return true
			}
		}
	}
	return false
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
		"Refresh up to 300 saved recall cases from the bounded live indexed memory store: POST /memory/recall/eval-cases/refresh with {\"project\":\"<project>\",\"topic_prefix\":\"<topic/path>\",\"max_cases\":300,\"min_hits\":1,\"include_graph_cases\":true,\"graph_max_cases\":3}.",
		"If refresh has no eligible memory, write durable memory first: POST /memory/write with projectName, fileName, topicPath, and content, then refresh the saved eval cases.",
		"Each saved recall eval case must include project, topic_path, query, limit, and exactly one expected_files item naming the file the query should recover.",
		"Do not use built-in fallback case IDs, synthetic cases, empty project, topic_path root, or broad rollup cases with multiple expected_files.",
		"When authoring ORCH_RECALL_EVAL_CASES_PATH manually, split broad topics into one concrete file-backed case per expected memory file.",
		"Treat the persisted v3 snapshot, case_set_digest, and custody metadata as immutable; regenerate the set after source memory changes.",
	}
}

func resolveRecallEvalCasesPath() string {
	return resolveStoragePath("ORCH_RECALL_EVAL_CASES_PATH", defaultRecallEvalCasesRelativePath)
}

const (
	// The activation path previously reread this many monitor rows for every
	// eligible request. Keep the same bounded historical window, but materialize
	// only the newest exact-scope artifact for each scope at startup and after a
	// successful durable append.
	recallMonitorShadowIndexHistoryLimit = 512
	recallMonitorShadowIndexMaxScopes    = savedRecallImpactMaxCohorts
)

type recallMonitorFileFingerprint struct {
	path       string
	exists     bool
	size       int64
	modifiedAt time.Time
	fileInfo   os.FileInfo
}

func (fingerprint recallMonitorFileFingerprint) matches(other recallMonitorFileFingerprint) bool {
	if fingerprint.path != other.path ||
		fingerprint.exists != other.exists ||
		fingerprint.size != other.size ||
		!fingerprint.modifiedAt.Equal(other.modifiedAt) {
		return false
	}
	if !fingerprint.exists {
		return true
	}
	return fingerprint.fileInfo != nil && other.fileInfo != nil && os.SameFile(fingerprint.fileInfo, other.fileInfo)
}

type recallMonitorShadowIndex struct {
	ready         bool
	stale         bool
	fingerprint   recallMonitorFileFingerprint
	artifactSeen  bool
	latestByScope map[string]map[string]any
}

func recallMonitorPath() string {
	return resolveStoragePath(
		"RECALL_MONITOR_PATH",
		filepath.Join("services", "orchestrator", "data", "recall_monitor.ndjson"),
	)
}

func recallMonitorFileFingerprintForPath(path string) (recallMonitorFileFingerprint, error) {
	path = strings.TrimSpace(path)
	fingerprint := recallMonitorFileFingerprint{path: path}
	if path == "" {
		return fingerprint, nil
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fingerprint, nil
	}
	if err != nil {
		return fingerprint, err
	}
	if !info.Mode().IsRegular() {
		return fingerprint, fmt.Errorf("recall monitor path is not a regular file")
	}
	fingerprint.exists = true
	fingerprint.size = info.Size()
	fingerprint.modifiedAt = info.ModTime()
	fingerprint.fileInfo = info
	return fingerprint, nil
}

func recallMonitorShadowScopeKey(project, taskClass string) (string, bool) {
	return recallMonitorShadowScopeKeyForWorkspace(project, taskClass, "")
}

func recallMonitorShadowScopeKeyForWorkspace(project, taskClass, workspaceRef string) (string, bool) {
	project = strings.TrimSpace(strings.ToLower(project))
	taskClass = strings.TrimSpace(strings.ToLower(taskClass))
	if project == "" || taskClass == "" {
		return "", false
	}
	key := savedRecallImpactOpaqueScopeRef("project", project) + "\x00" +
		savedRecallImpactOpaqueScopeRef("task_class", taskClass)
	if workspaceRef = contextPackLearnedDigestRef(workspaceRef); workspaceRef != "" {
		key += "\x00" + workspaceRef
	}
	return key, true
}

func recallMonitorShadowArtifactScopeKey(artifact map[string]any) (string, bool) {
	projectRef, taskClassRef, valid := searchImpactShadowScopeRefs(artifact)
	if !valid {
		return "", false
	}
	key := projectRef + "\x00" + taskClassRef
	if rawWorkspace, present := artifact["workspace_ref"]; present {
		workspaceText, ok := rawWorkspace.(string)
		if !ok {
			return "", false
		}
		if strings.TrimSpace(workspaceText) != "" {
			workspaceRef := contextPackLearnedDigestRef(workspaceText)
			if workspaceRef == "" {
				return "", false
			}
			key += "\x00" + workspaceRef
		}
	}
	return key, true
}

func recallMonitorShadowScopeMismatchArtifact() map[string]any {
	return map[string]any{
		"schema_id":         savedRecallImpactShadowEvalSchemaID,
		"comparison_valid":  false,
		"comparison_reason": "scope_mismatch",
	}
}

func (index *recallMonitorShadowIndex) recordArtifact(scopeKey string, artifact map[string]any) error {
	if index.latestByScope == nil {
		index.latestByScope = make(map[string]map[string]any)
	}
	if _, present := index.latestByScope[scopeKey]; !present && len(index.latestByScope) >= recallMonitorShadowIndexMaxScopes {
		return fmt.Errorf("recall monitor shadow index exceeds %d exact scopes", recallMonitorShadowIndexMaxScopes)
	}
	index.latestByScope[scopeKey] = cloneJSONMap(artifact)
	return nil
}

func (index *recallMonitorShadowIndex) recordNestedArtifacts(artifacts []map[string]any) error {
	byScope := make(map[string][]map[string]any)
	for _, artifact := range artifacts {
		scopeKey, valid := recallMonitorShadowArtifactScopeKey(artifact)
		if !valid {
			continue
		}
		byScope[scopeKey] = append(byScope[scopeKey], artifact)
	}
	for scopeKey, matches := range byScope {
		if len(matches) > 1 {
			if err := index.recordArtifact(scopeKey, recallMonitorShadowScopeMismatchArtifact()); err != nil {
				return err
			}
			continue
		}
		if err := index.recordArtifact(scopeKey, matches[0]); err != nil {
			return err
		}
	}
	return nil
}

func buildRecallMonitorShadowIndex(fingerprint recallMonitorFileFingerprint, rows []map[string]any) (recallMonitorShadowIndex, error) {
	index := recallMonitorShadowIndex{
		ready:         true,
		fingerprint:   fingerprint,
		latestByScope: make(map[string]map[string]any),
	}
	for _, row := range rows {
		if artifacts, nested := searchImpactNestedShadowEvaluations(row); nested {
			index.artifactSeen = true
			if err := index.recordNestedArtifacts(artifacts); err != nil {
				return recallMonitorShadowIndex{}, err
			}
			continue
		}
		if anyToString(row["schema_id"]) != savedRecallImpactShadowEvalSchemaID {
			continue
		}
		index.artifactSeen = true
		scopeKey, valid := recallMonitorShadowArtifactScopeKey(row)
		if !valid {
			continue
		}
		if err := index.recordArtifact(scopeKey, row); err != nil {
			return recallMonitorShadowIndex{}, err
		}
	}
	return index, nil
}

// readRecallMonitorHistoryForActivationIndex deliberately differs from the
// telemetry reader: a comparator cache cannot skip a malformed newest row and
// retain an older pass. It reads the same bounded tail window, but every
// complete record in that window must be a bounded JSON object, and a trailing
// partial record is treated as crash residue rather than ignored.
func readRecallMonitorHistoryForActivationIndex(path string, limit int) ([]map[string]any, error) {
	if limit < 1 {
		return []map[string]any{}, nil
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return []map[string]any{}, nil
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return []map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < 1 {
		return []map[string]any{}, nil
	}
	lastByte := []byte{0}
	if _, err := file.ReadAt(lastByte, info.Size()-1); err != nil {
		return nil, err
	}
	if lastByte[0] != '\n' {
		return nil, errors.New("recall monitor has a trailing partial row")
	}
	end, complete := recallMonitorLastCompleteLineEnd(file, info.Size())
	if !complete {
		return nil, errors.New("recall monitor has no readable complete rows")
	}
	rows := make([]map[string]any, 0, limit)
	for scanned := 0; end >= 0 && scanned < limit; scanned++ {
		line, start, oversized, ok := recallMonitorTailLine(file, end)
		if !ok {
			return nil, errors.New("read recall monitor row for activation index")
		}
		if oversized || len(line) > recallMonitorHistoryMaxLineBytes {
			return nil, fmt.Errorf("recall monitor row exceeds %d byte activation-index limit", recallMonitorHistoryMaxLineBytes)
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			return nil, errors.New("recall monitor has an empty complete row")
		}
		row := map[string]any{}
		if err := json.Unmarshal(line, &row); err != nil || row == nil {
			return nil, errors.New("recall monitor has a malformed complete row")
		}
		rows = append(rows, row)
		if start == 0 {
			break
		}
		end = start - 1 // the preceding newline terminates the next older line
	}
	for left, right := 0, len(rows)-1; left < right; left, right = left+1, right-1 {
		rows[left], rows[right] = rows[right], rows[left]
	}
	return rows, nil
}

func (s *server) setRecallMonitorShadowIndexUnavailable(path string) {
	if s == nil {
		return
	}
	fingerprint, _ := recallMonitorFileFingerprintForPath(path)
	s.recallMonitorShadowMu.Lock()
	s.recallMonitorShadowIndex = recallMonitorShadowIndex{
		stale:       true,
		fingerprint: fingerprint,
	}
	s.recallMonitorShadowMu.Unlock()
}

// loadRecallMonitorShadowIndex builds a bounded snapshot only at startup and
// after a durable local monitor append. A changing or unreadable source never
// leaves a prior comparator pass eligible.
func (s *server) loadRecallMonitorShadowIndex() error {
	if s == nil {
		return errors.New("recall monitor shadow index has no server")
	}
	path := recallMonitorPath()
	before, err := recallMonitorFileFingerprintForPath(path)
	if err != nil {
		s.setRecallMonitorShadowIndexUnavailable(path)
		return fmt.Errorf("stat recall monitor before index load: %w", err)
	}
	rows, err := readRecallMonitorHistoryForActivationIndex(path, recallMonitorShadowIndexHistoryLimit)
	if err != nil {
		s.setRecallMonitorShadowIndexUnavailable(path)
		return fmt.Errorf("read recall monitor for activation index: %w", err)
	}
	if currentPath := recallMonitorPath(); currentPath != path {
		s.setRecallMonitorShadowIndexUnavailable(path)
		return errors.New("recall monitor path changed during index load")
	}
	after, err := recallMonitorFileFingerprintForPath(path)
	if err != nil {
		s.setRecallMonitorShadowIndexUnavailable(path)
		return fmt.Errorf("stat recall monitor after index load: %w", err)
	}
	if !before.matches(after) {
		s.setRecallMonitorShadowIndexUnavailable(path)
		return errors.New("recall monitor changed during index load")
	}
	if after.exists && after.size > 0 && len(rows) == 0 {
		s.setRecallMonitorShadowIndexUnavailable(path)
		return errors.New("recall monitor index has no readable complete rows")
	}
	index, err := buildRecallMonitorShadowIndex(after, rows)
	if err != nil {
		s.setRecallMonitorShadowIndexUnavailable(path)
		return err
	}
	s.recallMonitorShadowMu.Lock()
	s.recallMonitorShadowIndex = index
	s.recallMonitorShadowMu.Unlock()
	return nil
}

func (s *server) markRecallMonitorShadowIndexStale(expected recallMonitorFileFingerprint) {
	if s == nil {
		return
	}
	s.recallMonitorShadowMu.Lock()
	if s.recallMonitorShadowIndex.fingerprint.matches(expected) {
		s.recallMonitorShadowIndex.stale = true
	}
	s.recallMonitorShadowMu.Unlock()
}

func recallMonitorShadowIndexFailure(reason string) map[string]any {
	return map[string]any{
		"schema_id":         savedRecallImpactShadowEvalSchemaID,
		"comparison_valid":  false,
		"comparison_reason": reason,
	}
}

// latestRecallMonitorShadowEvaluation never tails the monitor history. It
// performs only a metadata check so an append by another process invalidates
// the in-memory snapshot instead of silently reusing an older comparator.
func (s *server) latestRecallMonitorShadowEvaluation(project, taskClass string) map[string]any {
	return s.latestRecallMonitorShadowEvaluationForWorkspace(project, taskClass, "")
}

func (s *server) latestRecallMonitorShadowEvaluationForWorkspace(project, taskClass, workspaceRef string) map[string]any {
	if s == nil {
		return recallMonitorShadowIndexFailure("comparator_index_unavailable")
	}
	s.recallMonitorShadowMu.RLock()
	index := s.recallMonitorShadowIndex
	if !index.ready {
		s.recallMonitorShadowMu.RUnlock()
		return recallMonitorShadowIndexFailure("comparator_index_unavailable")
	}
	if index.stale {
		s.recallMonitorShadowMu.RUnlock()
		return recallMonitorShadowIndexFailure("comparator_index_stale")
	}
	if currentPath := recallMonitorPath(); currentPath != index.fingerprint.path {
		s.recallMonitorShadowMu.RUnlock()
		s.markRecallMonitorShadowIndexStale(index.fingerprint)
		return recallMonitorShadowIndexFailure("comparator_index_stale")
	}
	current, err := recallMonitorFileFingerprintForPath(index.fingerprint.path)
	if err != nil {
		s.recallMonitorShadowMu.RUnlock()
		s.markRecallMonitorShadowIndexStale(index.fingerprint)
		return recallMonitorShadowIndexFailure("comparator_index_unavailable")
	}
	if !index.fingerprint.matches(current) {
		s.recallMonitorShadowMu.RUnlock()
		s.markRecallMonitorShadowIndexStale(index.fingerprint)
		return recallMonitorShadowIndexFailure("comparator_index_stale")
	}
	scopeKey, exactScope := recallMonitorShadowScopeKeyForWorkspace(project, taskClass, workspaceRef)
	if exactScope {
		if artifact, present := index.latestByScope[scopeKey]; present {
			selected := cloneJSONMap(artifact)
			s.recallMonitorShadowMu.RUnlock()
			return selected
		}
	}
	artifactSeen := index.artifactSeen
	s.recallMonitorShadowMu.RUnlock()
	if !artifactSeen {
		return nil
	}
	return recallMonitorShadowScopeMismatchArtifact()
}

type recallMonitorPersistenceHooks struct {
	syncFile      func(*os.File) error
	syncDirectory func(string) error
}

var recallMonitorPersistenceHookRegistry = struct {
	sync.RWMutex
	byServer map[*server]recallMonitorPersistenceHooks
}{byServer: map[*server]recallMonitorPersistenceHooks{}}

func (s *server) recallMonitorPersistenceHooks() recallMonitorPersistenceHooks {
	hooks := recallMonitorPersistenceHooks{
		syncFile:      func(file *os.File) error { return file.Sync() },
		syncDirectory: syncOwnerOnlyDirectory,
	}
	if s == nil {
		return hooks
	}
	recallMonitorPersistenceHookRegistry.RLock()
	override, found := recallMonitorPersistenceHookRegistry.byServer[s]
	recallMonitorPersistenceHookRegistry.RUnlock()
	if !found {
		return hooks
	}
	if override.syncFile != nil {
		hooks.syncFile = override.syncFile
	}
	if override.syncDirectory != nil {
		hooks.syncDirectory = override.syncDirectory
	}
	return hooks
}

func setRecallMonitorPersistenceHooksForTest(s *server, hooks recallMonitorPersistenceHooks) func() {
	recallMonitorPersistenceHookRegistry.Lock()
	previous, hadPrevious := recallMonitorPersistenceHookRegistry.byServer[s]
	recallMonitorPersistenceHookRegistry.byServer[s] = hooks
	recallMonitorPersistenceHookRegistry.Unlock()
	return func() {
		recallMonitorPersistenceHookRegistry.Lock()
		defer recallMonitorPersistenceHookRegistry.Unlock()
		if hadPrevious {
			recallMonitorPersistenceHookRegistry.byServer[s] = previous
			return
		}
		delete(recallMonitorPersistenceHookRegistry.byServer, s)
	}
}

// recallMonitorAppendDurabilityPlan records whether this append creates a
// file and the directory chain whose entries must be synchronized afterward.
// The sync order is deepest to shallowest so newly created parents survive a
// crash as well as the monitor file itself.
func recallMonitorAppendDurabilityPlan(path string) (bool, []string, error) {
	clean := filepath.Clean(strings.TrimSpace(path))
	if clean == "" || clean == "." {
		return false, nil, errors.New("recall monitor path is empty")
	}
	if _, err := os.Lstat(clean); err == nil {
		return false, nil, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, nil, err
	}
	directories := []string{filepath.Dir(clean)}
	for current := directories[0]; ; {
		if _, err := os.Lstat(current); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, nil, err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false, nil, errors.New("recall monitor parent has no existing ancestor")
		}
		directories = append(directories, parent)
		current = parent
	}
	return true, directories, nil
}

func (s *server) appendRecallMonitorSample(sample map[string]any) error {
	path := recallMonitorPath()
	if strings.TrimSpace(path) == "" {
		if err := s.loadRecallMonitorShadowIndex(); err != nil {
			s.recordSearchImpactComparatorPersistence(false)
			return err
		}
		s.recordSearchImpactComparatorPersistence(true)
		return nil
	}
	created, directories, err := recallMonitorAppendDurabilityPlan(path)
	if err != nil {
		s.recordSearchImpactComparatorPersistence(false)
		return err
	}
	raw, err := json.Marshal(sample)
	if err != nil {
		s.recordSearchImpactComparatorPersistence(false)
		return err
	}
	file, err := openOwnerOnlyAppend(path, false)
	if err != nil {
		s.recordSearchImpactComparatorPersistence(false)
		return err
	}
	hooks := s.recallMonitorPersistenceHooks()
	if _, err := file.Write(append(raw, '\n')); err != nil {
		_ = file.Close()
		s.recordSearchImpactComparatorPersistence(false)
		return err
	}
	if err := hooks.syncFile(file); err != nil {
		_ = file.Close()
		s.recordSearchImpactComparatorPersistence(false)
		return err
	}
	if err := file.Close(); err != nil {
		s.recordSearchImpactComparatorPersistence(false)
		return err
	}
	if created {
		for _, directory := range directories {
			if err := hooks.syncDirectory(directory); err != nil {
				s.recordSearchImpactComparatorPersistence(false)
				return err
			}
		}
	}
	if err := s.loadRecallMonitorShadowIndex(); err != nil {
		s.recordSearchImpactComparatorPersistence(false)
		return fmt.Errorf("refresh recall monitor shadow index after durable append: %w", err)
	}
	s.recordSearchImpactComparatorPersistence(true)
	return nil
}

// searchImpactComparatorPersistenceUnavailable reports only whether the most
// recent comparator-artifact persistence attempt failed. It intentionally
// withholds the underlying storage error from downstream decision logic.
func (s *server) searchImpactComparatorPersistenceUnavailable() bool {
	if s == nil {
		return true
	}
	s.impactComparatorMu.Lock()
	defer s.impactComparatorMu.Unlock()
	return s.impactComparatorUnavailable
}

func (s *server) recordSearchImpactComparatorPersistence(persisted bool) {
	if s == nil {
		return
	}
	s.impactComparatorMu.Lock()
	s.impactComparatorUnavailable = !persisted
	s.impactComparatorMu.Unlock()
}

func (s *server) writeRecallEvalPersistenceUnavailable(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"ok":      false,
		"passed":  false,
		"durable": false,
		"error":   "saved recall evaluation was not durably persisted",
		"code":    "recall_monitor_persistence_unavailable",
	})
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

const (
	savedRecallImpactShadowEvalSchemaID = "search_impact_shadow_eval.v1"
	savedRecallImpactComparisonScope    = "saved_recall_reorder_only_returned_candidate_pool"
	savedRecallImpactK                  = 5
)

type savedRecallImpactPlan struct {
	grades              map[int]int
	project             string
	taskClass           string
	retrievalIntent     string
	projectRef          string
	taskClassRef        string
	workspaceRef        string
	retrievalIntentRefs map[string]struct{}
	caseCount           int
	numericExpected     int
	safetyCases         int
}

type savedRecallImpactScope struct {
	project      string
	taskClass    string
	workspaceRef string
}

type savedRecallImpactCandidate struct {
	ref  string
	rows []map[string]any
}

type savedRecallImpactMetrics struct {
	caseCount            int
	effectiveKMin        int
	effectiveKMax        int
	sparseCandidateCases int
	decisionWeightTotal  float64
	decisionWeightHit    float64
	dcg                  float64
	idcg                 float64
	reciprocalRankSum    float64
	numericExpected      int
	numericMatched       int
	citationExpected     int
	citationMatched      int
	citationCandidates   int
	citationExact        int
	safetyCaseCount      int
	safetyFailureCount   int
	latencyValues        []float64
}

type savedRecallImpactComparison struct {
	caseSetRef  string
	evaluatedAt string
	cohorts     map[savedRecallImpactScope]*savedRecallImpactCohort
	overflow    bool
}

type savedRecallImpactCohort struct {
	plan     savedRecallImpactPlan
	valid    bool
	reason   string
	baseline savedRecallImpactMetrics
	shadow   savedRecallImpactMetrics
	actuator *contextPackLearnedActuatorComparison
}

const savedRecallImpactMaxCohorts = 64

func newSavedRecallImpactComparison(cfg recallEvalSavedConfig) *savedRecallImpactComparison {
	return newSavedRecallImpactComparisonWithOutcomeRows(cfg, nil, time.Now().UTC())
}

func newSavedRecallImpactComparisonWithOutcomeRows(cfg recallEvalSavedConfig, outcomeRows []map[string]any, asOf time.Time) *savedRecallImpactComparison {
	return newSavedRecallImpactComparisonWithOutcomeRowsAndAuthority(cfg, outcomeRows, asOf, contextPackLearnedComparatorAuthority{})
}

func newSavedRecallImpactComparisonWithOutcomeRowsAndAuthority(
	cfg recallEvalSavedConfig,
	outcomeRows []map[string]any,
	asOf time.Time,
	authority contextPackLearnedComparatorAuthority,
) *savedRecallImpactComparison {
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	workspaceRef := ""
	if authority.Authorized {
		workspaceRef = contextPackLearnedDigestRef(authority.Workspace)
	}
	comparison := &savedRecallImpactComparison{
		caseSetRef:  savedRecallImpactCaseSetRefForWorkspace(cfg, workspaceRef),
		evaluatedAt: asOf.UTC().Format(time.RFC3339Nano),
		cohorts:     map[savedRecallImpactScope]*savedRecallImpactCohort{},
	}
	for idx, rawCase := range cfg.Cases {
		scope := savedRecallImpactScopeForCaseWithWorkspace(rawCase, workspaceRef)
		cohort := comparison.cohorts[scope]
		if cohort == nil {
			cohort = &savedRecallImpactCohort{
				plan: savedRecallImpactPlan{
					grades:              map[int]int{},
					project:             scope.project,
					taskClass:           scope.taskClass,
					projectRef:          savedRecallImpactOpaqueScopeRef("project", scope.project),
					taskClassRef:        savedRecallImpactOpaqueScopeRef("task_class", scope.taskClass),
					workspaceRef:        scope.workspaceRef,
					retrievalIntentRefs: map[string]struct{}{},
				},
				valid: true,
				baseline: savedRecallImpactMetrics{
					latencyValues: make([]float64, 0, len(cfg.Cases)),
				},
				shadow: savedRecallImpactMetrics{
					latencyValues: make([]float64, 0, len(cfg.Cases)),
				},
			}
			comparison.cohorts[scope] = cohort
		}
		cohort.plan.caseCount++
		retrievalIntent := strings.TrimSpace(strings.ToLower(anyToString(rawCase["retrieval_intent"])))
		if retrievalIntent == "" {
			retrievalIntent = "decision"
		}
		cohort.plan.retrievalIntentRefs[savedRecallImpactOpaqueScopeRef("retrieval_intent", retrievalIntent)] = struct{}{}
		if cohort.plan.retrievalIntent == "" {
			cohort.plan.retrievalIntent = retrievalIntent
		} else if cohort.plan.retrievalIntent != retrievalIntent {
			cohort.plan.retrievalIntent = "__mixed__"
		}
		if grade, ok := savedRecallImpactDecisionGrade(rawCase["decision_impact_grade"]); ok {
			cohort.plan.grades[idx] = grade
		} else {
			cohort.invalidate("decision_impact_grade_missing_or_invalid")
		}
		cohort.plan.numericExpected += len(normalizeExpectedNumeric(rawCase["expected_numeric"]))
		if len(normalizeExpectedFileTokens(rawCase["forbidden_files"])) > 0 {
			cohort.plan.safetyCases++
		}
	}
	for _, cohort := range comparison.cohorts {
		if cohort.plan.numericExpected == 0 {
			cohort.invalidate("numeric_expectations_missing")
		}
		if cohort.plan.safetyCases == 0 {
			cohort.invalidate("safety_cases_missing")
		}
		actuatorReason := ""
		multipliers := map[string]float64{}
		reputationVectorRef := ""
		if !cohort.valid {
			actuatorReason = "case_set_invalid"
		} else if cohort.plan.retrievalIntent == "__mixed__" || len(cohort.plan.retrievalIntentRefs) != 1 {
			actuatorReason = "mixed_retrieval_intent"
		} else if len(outcomeRows) == 0 {
			actuatorReason = "reputation_candidate_influence_unavailable"
		} else {
			var reputation map[string]any
			if workspaceRef != "" {
				reputation = evidenceReputationSnapshotFromReconciledRowsForWorkspace(
					outcomeRows, cohort.plan.project, cohort.plan.taskClass, cohort.plan.retrievalIntent,
					workspaceRef, contextPackLearnedMinimumSamples, evidenceReputationMaxEntries, asOf,
				)
			} else {
				reputation = evidenceReputationSnapshotFromReconciledRows(
					outcomeRows, cohort.plan.project, cohort.plan.taskClass, cohort.plan.retrievalIntent,
					contextPackLearnedMinimumSamples, evidenceReputationMaxEntries, asOf,
				)
			}
			var reason string
			multipliers, reason = contextPackLearnedReputationMultipliers(
				reputation, cohort.plan.project, cohort.plan.taskClass, cohort.plan.retrievalIntent, asOf,
			)
			actuatorReason = reason
			if reason == "" {
				reputationVectorRef = contextPackLearnedReputationVectorRef(multipliers)
				if reputationVectorRef == "" {
					actuatorReason = "reputation_vector_unavailable"
				}
			}
		}
		cohort.actuator = newContextPackLearnedActuatorComparison(
			comparison.caseSetRef, cohort.plan.caseCount, multipliers, actuatorReason,
		)
		if authority.Authorized && workspaceRef != "" {
			cohort.actuator.setAuthorityEnvelope(authority.Envelope)
		}
	}
	comparison.overflow = len(comparison.cohorts) > savedRecallImpactMaxCohorts
	return comparison
}

func (c *savedRecallImpactComparison) addActuatorCase(caseIndex int, rawCase, searchResponse map[string]any) {
	if c == nil {
		return
	}
	cohort := c.cohorts[savedRecallImpactScopeForCaseWithWorkspace(rawCase, c.workspaceRef())]
	if cohort == nil || cohort.actuator == nil {
		return
	}
	grade, exists := cohort.plan.grades[caseIndex]
	if !exists {
		cohort.actuator.invalidate("decision_impact_grade_missing_or_invalid")
		return
	}
	cohort.actuator.addCase(rawCase, searchResponse, grade)
}

// savedRecallImpactCaseSetRef binds a comparator artifact to the versioned
// saved-case content without exposing any case text, paths, or scope values.
// json.Marshal orders map keys deterministically, while case order remains an
// intentional part of the evaluated case-set content.
func savedRecallImpactCaseSetRef(cfg recallEvalSavedConfig) string {
	return savedRecallImpactCaseSetRefForWorkspace(cfg, "")
}

func savedRecallImpactCaseSetRefForWorkspace(cfg recallEvalSavedConfig, workspaceRef string) string {
	payload := map[string]any{
		"schema_id": "saved_recall_eval_case_set_ref.v1",
		"version":   cfg.Version,
		"cases":     cfg.Cases,
	}
	if workspaceRef = contextPackLearnedDigestRef(workspaceRef); workspaceRef != "" {
		payload["workspace_ref"] = workspaceRef
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return searchIntelligenceSHA256Ref(string(raw))
}

func savedRecallImpactScopeForCase(rawCase map[string]any) savedRecallImpactScope {
	return savedRecallImpactScopeForCaseWithWorkspace(rawCase, "")
}

func savedRecallImpactScopeForCaseWithWorkspace(rawCase map[string]any, workspaceRef string) savedRecallImpactScope {
	project := strings.TrimSpace(strings.ToLower(anyToString(rawCase["project"])))
	if project == "" {
		project = "unscoped"
	}
	taskClass := strings.TrimSpace(strings.ToLower(anyToString(rawCase["task_class"])))
	if taskClass == "" {
		taskClass = "unclassified"
	}
	return savedRecallImpactScope{project: project, taskClass: taskClass, workspaceRef: contextPackLearnedDigestRef(workspaceRef)}
}

func (c *savedRecallImpactComparison) workspaceRef() string {
	if c == nil {
		return ""
	}
	for _, cohort := range c.cohorts {
		return cohort.plan.workspaceRef
	}
	return ""
}

func savedRecallImpactDecisionGrade(raw any) (int, bool) {
	grade := 0
	switch value := raw.(type) {
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value {
			return 0, false
		}
		grade = int(value)
	case int:
		grade = value
	case int64:
		grade = int(value)
	case int32:
		grade = int(value)
	case json.Number:
		parsed, err := value.Int64()
		if err != nil {
			return 0, false
		}
		grade = int(parsed)
	default:
		return 0, false
	}
	return grade, grade >= 1 && grade <= 3
}

func savedRecallImpactOpaqueScopeRef(kind string, value string) string {
	return "sha256:" + sha256Hex("saved_recall_impact_scope.v1\x00"+kind+"\x00"+value)
}

func (c *savedRecallImpactComparison) invalidate(reason string) {
	if c == nil {
		return
	}
	for _, cohort := range c.cohorts {
		cohort.invalidate(reason)
	}
}

func (c *savedRecallImpactComparison) invalidateCase(rawCase map[string]any, reason string) {
	if c == nil {
		return
	}
	cohort := c.cohorts[savedRecallImpactScopeForCaseWithWorkspace(rawCase, c.workspaceRef())]
	if cohort == nil {
		c.invalidate(reason)
		return
	}
	cohort.invalidate(reason)
}

func (c *savedRecallImpactCohort) invalidate(reason string) {
	if c == nil {
		return
	}
	c.valid = false
	if reason == "case_set_invalid" || c.reason == "" {
		c.reason = reason
	}
}

func (c *savedRecallImpactComparison) addCase(
	caseIndex int,
	rawCase map[string]any,
	results []map[string]any,
	searchIntelligence map[string]any,
	expectedNumeric []string,
	latencyMs float64,
) {
	if c == nil {
		return
	}
	cohort := c.cohorts[savedRecallImpactScopeForCaseWithWorkspace(rawCase, c.workspaceRef())]
	if cohort == nil {
		c.invalidate("cohort_missing")
		return
	}
	cohort.addCase(caseIndex, rawCase, results, searchIntelligence, expectedNumeric, latencyMs)
}

func (c *savedRecallImpactCohort) addCase(
	caseIndex int,
	rawCase map[string]any,
	results []map[string]any,
	searchIntelligence map[string]any,
	expectedNumeric []string,
	latencyMs float64,
) {
	if c == nil || !c.valid {
		return
	}
	grade, exists := c.plan.grades[caseIndex]
	if !exists {
		c.invalidate("decision_impact_grade_missing_or_invalid")
		return
	}
	native, nativeByRef, reason := savedRecallImpactNativeCandidates(results)
	if reason != "" {
		c.invalidate(reason)
		return
	}
	if len(native) == 0 {
		c.invalidate("native_candidate_pool_empty")
		return
	}
	effectiveK := minInt(savedRecallImpactK, len(native))
	shadow, reason := savedRecallImpactShadowCandidates(searchIntelligence, nativeByRef, effectiveK)
	if reason != "" {
		c.invalidate(reason)
		return
	}
	expectedFiles := normalizeExpectedFileTokens(rawCase["expected_files"])
	forbiddenFiles := normalizeExpectedFileTokens(rawCase["forbidden_files"])
	baselineNumeric, baselineMapped := savedRecallImpactMatchedNumericFacts(native[:effectiveK], expectedNumeric)
	shadowNumeric, shadowMapped := savedRecallImpactMatchedNumericFacts(shadow, expectedNumeric)
	if !baselineMapped || !shadowMapped {
		c.invalidate("candidate_bound_numeric_evidence_missing")
		return
	}
	c.baseline.addCase(native[:effectiveK], expectedFiles, forbiddenFiles, expectedNumeric, baselineNumeric, grade, latencyMs)
	c.shadow.addCase(shadow, expectedFiles, forbiddenFiles, expectedNumeric, shadowNumeric, grade, latencyMs)
}

func savedRecallImpactNativeCandidates(results []map[string]any) ([]savedRecallImpactCandidate, map[string]savedRecallImpactCandidate, string) {
	ordered := make([]savedRecallImpactCandidate, 0, len(results))
	byRef := map[string]savedRecallImpactCandidate{}
	orderedIndex := map[string]int{}
	for _, row := range results {
		identity := searchIntelligenceCandidateIdentity(row)
		if !isSearchIntelligenceFullSHA256Ref(identity.CandidateRef) {
			return nil, nil, "native_candidate_ref_invalid"
		}
		candidate, exists := byRef[identity.CandidateRef]
		if !exists {
			candidate = savedRecallImpactCandidate{ref: identity.CandidateRef, rows: make([]map[string]any, 0, 1)}
		}
		candidate.rows = append(candidate.rows, row)
		byRef[identity.CandidateRef] = candidate
		if !exists {
			orderedIndex[identity.CandidateRef] = len(ordered)
			ordered = append(ordered, candidate)
		} else {
			ordered[orderedIndex[identity.CandidateRef]] = candidate
		}
	}
	return ordered, byRef, ""
}

func savedRecallImpactShadowCandidates(searchIntelligence map[string]any, nativeByRef map[string]savedRecallImpactCandidate, effectiveK int) ([]savedRecallImpactCandidate, string) {
	frontier := anyMap(searchIntelligence["decision_frontier"])
	if anyToString(frontier["status"]) != "shadow_only" {
		return nil, "shadow_frontier_missing"
	}
	rawCandidates := contextPackAnyList(frontier["candidates"])
	if effectiveK < 1 {
		return nil, "native_candidate_pool_empty"
	}
	shadow := make([]savedRecallImpactCandidate, 0, effectiveK)
	seen := map[string]struct{}{}
	for _, rawCandidate := range rawCandidates {
		refs := anyMap(anyMap(rawCandidate)["refs"])
		candidateRef := strings.TrimSpace(anyToString(refs["candidate_ref"]))
		if !isSearchIntelligenceFullSHA256Ref(candidateRef) {
			return nil, "shadow_candidate_ref_invalid"
		}
		if _, duplicate := seen[candidateRef]; duplicate {
			return nil, "shadow_top_k_duplicate"
		}
		candidate, exists := nativeByRef[candidateRef]
		if !exists {
			continue
		}
		seen[candidateRef] = struct{}{}
		shadow = append(shadow, candidate)
		if len(shadow) == effectiveK {
			break
		}
	}
	if len(shadow) != effectiveK {
		return nil, "shadow_returned_pool_incomplete"
	}
	return shadow, ""
}

func (m *savedRecallImpactMetrics) addCase(
	candidates []savedRecallImpactCandidate,
	expectedFiles map[string]struct{},
	forbiddenFiles map[string]struct{},
	expectedNumeric []string,
	matchedNumeric []string,
	grade int,
	latencyMs float64,
) {
	if m == nil {
		return
	}
	if m.caseCount == 0 || len(candidates) < m.effectiveKMin {
		m.effectiveKMin = len(candidates)
	}
	if len(candidates) > m.effectiveKMax {
		m.effectiveKMax = len(candidates)
	}
	if len(candidates) < savedRecallImpactK {
		m.sparseCandidateCases++
	}
	m.caseCount++
	m.decisionWeightTotal += float64(grade)
	m.idcg += math.Pow(2, float64(grade)) - 1
	m.numericExpected += len(expectedNumeric)
	m.citationExpected += len(expectedFiles)
	m.citationCandidates += len(candidates)
	if len(forbiddenFiles) > 0 {
		m.safetyCaseCount++
	}
	selectedRows := make([]map[string]any, 0, len(candidates))
	hit := false
	for index, candidate := range candidates {
		selectedRows = append(selectedRows, candidate.rows...)
		if savedRecallImpactCandidateMatches(candidate, expectedFiles) {
			m.citationExact++
			if !hit {
				hit = true
				m.decisionWeightHit += float64(grade)
				m.dcg += (math.Pow(2, float64(grade)) - 1) / math.Log2(float64(index)+2)
				m.reciprocalRankSum += 1.0 / float64(index+1)
			}
		}
	}
	m.citationMatched += len(matchedExpectedFilesWithinK(selectedRows, expectedFiles, len(selectedRows)))
	if len(forbiddenFiles) > 0 && savedRecallImpactCandidatesMatch(candidates, forbiddenFiles) {
		m.safetyFailureCount++
	}
	m.numericMatched += len(matchedNumeric)
	if latencyMs >= 0 {
		m.latencyValues = append(m.latencyValues, latencyMs)
	}
}

func savedRecallImpactCandidateMatches(candidate savedRecallImpactCandidate, expectedFiles map[string]struct{}) bool {
	for _, row := range candidate.rows {
		if resultHitsExpectations(row, expectedFiles, nil) {
			return true
		}
	}
	return false
}

func savedRecallImpactCandidatesMatch(candidates []savedRecallImpactCandidate, expectedFiles map[string]struct{}) bool {
	for _, candidate := range candidates {
		if savedRecallImpactCandidateMatches(candidate, expectedFiles) {
			return true
		}
	}
	return false
}

func savedRecallImpactMatchedNumericFacts(candidates []savedRecallImpactCandidate, expected []string) ([]string, bool) {
	if len(expected) == 0 {
		return []string{}, true
	}
	evidence := make([]string, 0, len(candidates))
	mapped := false
	for _, candidate := range candidates {
		for _, row := range candidate.rows {
			rowEvidence, rowMapped := savedRecallImpactCandidateNumericEvidence(row)
			if rowMapped {
				mapped = true
			}
			evidence = append(evidence, rowEvidence...)
		}
	}
	if !mapped {
		return []string{}, false
	}
	matches := make([]string, 0, len(expected))
	for _, value := range expected {
		for _, candidateEvidence := range evidence {
			if savedRecallImpactHasExactNumericToken(candidateEvidence, value) {
				matches = append(matches, value)
				break
			}
		}
	}
	return matches, true
}

func savedRecallImpactCandidateNumericEvidence(row map[string]any) ([]string, bool) {
	evidence := make([]string, 0, 3)
	mapped := false
	for _, key := range []string{"summary", "content", "text"} {
		value := strings.TrimSpace(anyToString(row[key]))
		if value == "" {
			continue
		}
		mapped = true
		evidence = append(evidence, value)
	}
	for _, rawFact := range contextPackAnyList(row["numeric_facts"]) {
		fact := anyMap(rawFact)
		if strings.EqualFold(strings.TrimSpace(anyToString(fact["field"])), "score") {
			continue
		}
		for _, key := range []string{"value", "text"} {
			value := strings.TrimSpace(anyToString(fact[key]))
			if value == "" {
				continue
			}
			mapped = true
			evidence = append(evidence, value)
		}
	}
	return evidence, mapped
}

func savedRecallImpactHasExactNumericToken(text, expected string) bool {
	token := []rune(strings.TrimSpace(expected))
	haystack := []rune(text)
	if len(token) == 0 || len(token) > len(haystack) {
		return false
	}
	for start := 0; start+len(token) <= len(haystack); start++ {
		matched := true
		for offset, value := range token {
			if haystack[start+offset] != value {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		if start > 0 && savedRecallImpactNumericTokenRune(haystack[start-1]) {
			continue
		}
		end := start + len(token)
		if end < len(haystack) && savedRecallImpactNumericTokenRune(haystack[end]) {
			continue
		}
		return true
	}
	return false
}

func savedRecallImpactNumericTokenRune(value rune) bool {
	return unicode.IsLetter(value) || unicode.IsDigit(value) || strings.ContainsRune(".+,-_%", value)
}

func (m savedRecallImpactMetrics) monitorMetrics() map[string]any {
	if m.caseCount == 0 || m.decisionWeightTotal <= 0 || m.idcg <= 0 || m.numericExpected <= 0 || m.citationExpected <= 0 || m.safetyCaseCount <= 0 {
		return savedRecallImpactUnavailableMetrics()
	}
	_, p95LatencyMs := recallLatencyStats(m.latencyValues)
	return map[string]any{
		"decision_impact_recall_at_5": roundFloat(m.decisionWeightHit/m.decisionWeightTotal, 6),
		"decision_impact_ndcg_at_5":   roundFloat(m.dcg/m.idcg, 6),
		"mrr":                         roundFloat(m.reciprocalRankSum/float64(m.caseCount), 6),
		"numeric_exactness":           roundFloat(float64(m.numericMatched)/float64(m.numericExpected), 6),
		"citation_coverage":           roundFloat(float64(m.citationMatched)/float64(m.citationExpected), 6),
		"citation_exactness":          roundFloat(float64(m.citationExact)/float64(m.citationCandidates), 6),
		"safety_case_count":           m.safetyCaseCount,
		"safety_failure_count":        m.safetyFailureCount,
		"safety_failure_rate":         roundFloat(float64(m.safetyFailureCount)/float64(m.safetyCaseCount), 6),
		"p95_latency_ms":              roundFloat(p95LatencyMs, 3),
		"effective_k_min":             m.effectiveKMin,
		"effective_k_max":             m.effectiveKMax,
		"sparse_candidate_case_count": m.sparseCandidateCases,
	}
}

func savedRecallImpactUnavailableMetrics() map[string]any {
	return map[string]any{
		"decision_impact_recall_at_5": nil,
		"decision_impact_ndcg_at_5":   nil,
		"mrr":                         nil,
		"numeric_exactness":           nil,
		"citation_coverage":           nil,
		"citation_exactness":          nil,
		"safety_case_count":           0,
		"safety_failure_count":        0,
		"safety_failure_rate":         nil,
		"p95_latency_ms":              nil,
		"effective_k_min":             0,
		"effective_k_max":             0,
		"sparse_candidate_case_count": 0,
	}
}

func (c *savedRecallImpactCohort) monitorFields(caseSetRef, evaluatedAt string) map[string]any {
	if c == nil {
		return map[string]any{}
	}
	comparisonValid := c.valid
	reason := c.reason
	baseline := savedRecallImpactUnavailableMetrics()
	shadow := savedRecallImpactUnavailableMetrics()
	if comparisonValid {
		baseline = c.baseline.monitorMetrics()
		shadow = c.shadow.monitorMetrics()
		if !savedRecallImpactMetricsAvailable(baseline) || !savedRecallImpactMetricsAvailable(shadow) {
			comparisonValid = false
			reason = "comparison_metrics_unavailable"
			baseline = savedRecallImpactUnavailableMetrics()
			shadow = savedRecallImpactUnavailableMetrics()
		}
	}
	if comparisonValid {
		reason = "valid"
	}
	retrievalIntentRefs := make([]string, 0, len(c.plan.retrievalIntentRefs))
	for ref := range c.plan.retrievalIntentRefs {
		retrievalIntentRefs = append(retrievalIntentRefs, ref)
	}
	sort.Strings(retrievalIntentRefs)
	fields := map[string]any{
		"schema_id":                   savedRecallImpactShadowEvalSchemaID,
		"version":                     1,
		"comparison_scope":            savedRecallImpactComparisonScope,
		"comparison_fixed_k":          savedRecallImpactK,
		"comparison_valid":            comparisonValid,
		"comparison_reason":           reason,
		"case_count":                  c.plan.caseCount,
		"case_set_ref":                caseSetRef,
		"project_scope_refs":          []string{c.plan.projectRef},
		"task_class_scope_refs":       []string{c.plan.taskClassRef},
		"workspace_ref":               c.plan.workspaceRef,
		"retrieval_intent_scope_refs": retrievalIntentRefs,
		"evaluated_at":                evaluatedAt,
		"latency_basis":               "shared_synthetic_retrieval_replay_ms",
		"baseline":                    baseline,
		"shadow":                      shadow,
		"privacy": map[string]any{
			"raw_content_or_path_persisted": false,
			"opaque_scope_refs_only":        true,
			"candidate_refs_persisted":      false,
		},
	}
	if c.actuator != nil {
		fields["learned_actuator_comparator"] = c.actuator.monitorFields()
	}
	return fields
}

func (c *savedRecallImpactComparison) monitorFields(caseCount int) map[string]any {
	if c == nil {
		return map[string]any{}
	}
	cohorts := c.sortedCohorts()
	if len(cohorts) > savedRecallImpactMaxCohorts {
		cohorts = cohorts[:savedRecallImpactMaxCohorts]
	}
	artifacts := make([]any, 0, len(cohorts))
	for _, cohort := range cohorts {
		artifacts = append(artifacts, cohort.monitorFields(c.caseSetRef, c.evaluatedAt))
	}
	if len(cohorts) == 1 && !c.overflow {
		topLevel := cloneJSONMap(anyMap(artifacts[0]))
		topLevel["search_impact_shadow_evaluations"] = artifacts
		return topLevel
	}
	reason := "mixed_scope_requires_exact_cohort"
	if c.overflow {
		reason = "cohort_limit_exceeded"
	}
	if len(cohorts) == 0 {
		reason = "comparison_metrics_unavailable"
	}
	return map[string]any{
		"schema_id":                        savedRecallImpactShadowEvalSchemaID,
		"version":                          1,
		"comparison_scope":                 savedRecallImpactComparisonScope,
		"comparison_fixed_k":               savedRecallImpactK,
		"comparison_valid":                 false,
		"comparison_reason":                reason,
		"case_count":                       caseCount,
		"case_set_ref":                     c.caseSetRef,
		"project_scope_refs":               []string{},
		"task_class_scope_refs":            []string{},
		"workspace_ref":                    "",
		"retrieval_intent_scope_refs":      []string{},
		"evaluated_at":                     c.evaluatedAt,
		"latency_basis":                    "shared_synthetic_retrieval_replay_ms",
		"baseline":                         savedRecallImpactUnavailableMetrics(),
		"shadow":                           savedRecallImpactUnavailableMetrics(),
		"search_impact_shadow_evaluations": artifacts,
		"privacy": map[string]any{
			"raw_content_or_path_persisted": false,
			"opaque_scope_refs_only":        true,
			"candidate_refs_persisted":      false,
		},
	}
}

func (c *savedRecallImpactComparison) sortedCohorts() []*savedRecallImpactCohort {
	if c == nil {
		return []*savedRecallImpactCohort{}
	}
	cohorts := make([]*savedRecallImpactCohort, 0, len(c.cohorts))
	for _, cohort := range c.cohorts {
		cohorts = append(cohorts, cohort)
	}
	sort.Slice(cohorts, func(left, right int) bool {
		if cohorts[left].plan.projectRef == cohorts[right].plan.projectRef {
			return cohorts[left].plan.taskClassRef < cohorts[right].plan.taskClassRef
		}
		return cohorts[left].plan.projectRef < cohorts[right].plan.projectRef
	})
	return cohorts
}

func savedRecallImpactMetricsAvailable(metrics map[string]any) bool {
	for _, key := range []string{
		"decision_impact_recall_at_5", "decision_impact_ndcg_at_5", "mrr", "numeric_exactness",
		"citation_coverage", "citation_exactness", "safety_failure_rate", "p95_latency_ms",
	} {
		if _, ok := metrics[key].(float64); !ok {
			return false
		}
	}
	return anyToInt(metrics["safety_case_count"], 0) > 0
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
