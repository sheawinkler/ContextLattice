package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func methodAllowed(method string, allowed ...string) bool {
	normalized := strings.TrimSpace(strings.ToUpper(method))
	if normalized == "" {
		return false
	}
	for _, candidate := range allowed {
		if normalized == strings.TrimSpace(strings.ToUpper(candidate)) {
			return true
		}
	}
	return false
}

func backendPathWithQuery(path string, rawQuery string) string {
	trimmed := strings.TrimSpace(rawQuery)
	if trimmed == "" {
		return path
	}
	return path + "?" + trimmed
}

func (s *server) forwardJSONGET(w http.ResponseWriter, r *http.Request, backendPath string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if s.strictNoPythonRuntime {
		if !s.allowPythonHotPathFallback(w, backendPath, "strict_runtime_backend_forward_disabled") {
			return
		}
	}
	incomingHeaders, ok := s.prepareAuthorizedHeaders(w, r)
	if !ok {
		return
	}
	response, status, err := s.backendJSONRequest(
		r.Context(),
		http.MethodGet,
		backendPathWithQuery(backendPath, r.URL.RawQuery),
		incomingHeaders,
		nil,
	)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":      "backend unavailable",
			"detail":     err.Error(),
			"backendUrl": s.backendURL,
		})
		return
	}
	writeJSON(w, status, response)
}

func (s *server) forwardJSONPOST(w http.ResponseWriter, r *http.Request, backendPath string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if s.strictNoPythonRuntime {
		if !s.allowPythonHotPathFallback(w, backendPath, "strict_runtime_backend_forward_disabled") {
			return
		}
	}
	incomingHeaders, ok := s.prepareAuthorizedHeaders(w, r)
	if !ok {
		return
	}
	rawBody, err := readRequestBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
		return
	}
	payload, err := parseJSONMap(rawBody)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
		return
	}
	response, status, backendErr := s.backendJSONRequest(
		r.Context(),
		http.MethodPost,
		backendPathWithQuery(backendPath, r.URL.RawQuery),
		incomingHeaders,
		payload,
	)
	if backendErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":      "backend unavailable",
			"detail":     backendErr.Error(),
			"backendUrl": s.backendURL,
		})
		return
	}
	writeJSON(w, status, response)
}

func (s *server) forwardJSONAny(w http.ResponseWriter, r *http.Request, backendPath string) {
	method := strings.TrimSpace(strings.ToUpper(r.Method))
	if method == "" {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if s.strictNoPythonRuntime {
		if !s.allowPythonHotPathFallback(w, backendPath, "strict_runtime_backend_forward_disabled") {
			return
		}
	}
	incomingHeaders, ok := s.prepareAuthorizedHeaders(w, r)
	if !ok {
		return
	}
	var payload map[string]any
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodDelete {
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
			return
		}
		trimmed := strings.TrimSpace(string(rawBody))
		if trimmed != "" {
			parsed, err := parseJSONMap(rawBody)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
				return
			}
			payload = parsed
		}
	}
	response, status, backendErr := s.backendJSONRequest(
		r.Context(),
		method,
		backendPathWithQuery(backendPath, r.URL.RawQuery),
		incomingHeaders,
		payload,
	)
	if backendErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":      "backend unavailable",
			"detail":     backendErr.Error(),
			"backendUrl": s.backendURL,
		})
		return
	}
	writeJSON(w, status, response)
}

func (s *server) memoryRecent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	if s.memoryStore == nil || !s.memoryStore.isEnabled() {
		s.forwardJSONGET(w, r, "/memory/recent")
		return
	}
	limit, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	offset, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("offset")))
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	topicPath := strings.TrimSpace(r.URL.Query().Get("topic_path"))
	items := s.memoryStore.recentItems(project, topicPath, limit, offset)
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *server) memoryTopicTree(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	if s.memoryStore == nil || !s.memoryStore.isEnabled() {
		s.forwardJSONGET(w, r, "/memory/topics")
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	writeJSON(w, http.StatusOK, s.memoryStore.topicTree(project))
}

func (s *server) memoryTopicList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	if s.memoryStore == nil || !s.memoryStore.isEnabled() {
		s.forwardJSONGET(w, r, "/memory/topics/list")
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	minCount, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("min_count")))
	limit, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	depth, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("depth")))
	writeJSON(w, http.StatusOK, s.memoryStore.topicList(project, minCount, limit, depth))
}

func (s *server) memoryTopicRollups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	if s.memoryStore == nil || !s.memoryStore.isEnabled() {
		s.forwardJSONGET(w, r, "/memory/topic-rollups")
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	minCount, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("min_count")))
	limit, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	offset, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("offset")))
	writeJSON(w, http.StatusOK, s.memoryStore.topicRollups(project, minCount, limit, offset))
}

func (s *server) memoryContinuitySnapshot(w http.ResponseWriter, r *http.Request) {
	s.forwardJSONPOST(w, r, "/memory/continuity/snapshot")
}

func (s *server) memoryContinuitySnapshots(w http.ResponseWriter, r *http.Request) {
	s.forwardJSONGET(w, r, "/memory/continuity/snapshots")
}

func (s *server) memoryContinuitySnapshotByID(w http.ResponseWriter, r *http.Request) {
	s.forwardJSONGET(w, r, r.URL.Path)
}

func (s *server) memoryRecallEvalCases(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	cfg, err := loadSavedRecallEvalConfig()
	if err != nil {
		log.Printf("saved recall eval case load failed: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "failed to load saved recall eval cases", "code": "storage_access_error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"case_set_id":        ownerOnlyStoreRef("recall_eval_cases"),
		"schema_id":          cfg.SchemaID,
		"version":            cfg.Version,
		"updatedAt":          cfg.UpdatedAt,
		"case_set_digest":    cfg.CaseSetDigest,
		"snapshot":           cloneAnyMap(cfg.Snapshot),
		"custody":            cloneAnyMap(cfg.Custody),
		"benchmark_eligible": anyToBool(validateSavedRecallEvalCaseSet(cfg)["benchmark_eligible"]),
		"k":                  cfg.K,
		"case_set_health":    validateSavedRecallEvalCaseSet(cfg),
		"gate": map[string]any{
			"minRecallAtK":        cfg.Gate.MinRecallAtK,
			"minMrr":              cfg.Gate.MinMRR,
			"minNumericExactness": cfg.Gate.MinNumericExactly,
		},
		"count": len(cfg.Cases),
		"cases": cfg.Cases,
	})
}

func (s *server) memoryRecallEvalCasesRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	bodyBytes, err := readRequestBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
		return
	}
	payload := map[string]any{}
	if strings.TrimSpace(string(bodyBytes)) != "" {
		payload, err = parseJSONMap(bodyBytes)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
			return
		}
	}
	if anyToBool(payload["graph_corpus"]) || strings.EqualFold(strings.TrimSpace(anyToString(payload["corpus"])), "graph") {
		s.memoryRecallGraphCorpusRefresh(w, r, payload)
		return
	}
	maxCases := clampInt(anyToInt(payload["max_cases"], savedRecallEvalV3MaxCases), 1, savedRecallEvalV3MaxCases)
	minHits := clampInt(anyToInt(payload["min_hits"], 1), 1, 1000)
	project := strings.TrimSpace(anyToString(payload["project"]))
	topicPrefix := strings.TrimSpace(anyToString(payload["topic_prefix"]))
	includeGraphCases := anyToBool(payload["include_graph_cases"])
	graphMaxCases := clampInt(anyToInt(payload["graph_max_cases"], 100), 0, savedRecallEvalV3MaxGraphCases)
	refreshed := s.buildRefreshedRecallEvalCaseSetWithGraphContext(r.Context(), maxCases, minHits, project, topicPrefix, includeGraphCases, graphMaxCases)
	if includeGraphCases && graphMaxCases >= savedRecallEvalV3MinGraphReferences {
		population := anyMap(refreshed["graph_reference_population"])
		if !anyToBool(population["ready"]) {
			previousPath := resolveRecallEvalCasesPath()
			previous := defaultSavedRecallEvalConfig(previousPath)
			previousErr := error(nil)
			if _, statErr := os.Stat(previousPath); statErr == nil {
				previous, previousErr = loadSavedRecallEvalConfig()
			}
			preserved := previousErr == nil && len(previous.Cases) > 0
			writeJSON(w, http.StatusConflict, map[string]any{
				"ok":                          false,
				"code":                        "insufficient_current_state_bound_reference_population",
				"error":                       "refresh did not produce the required current-state-bound graph reference holdout; the existing case set was preserved",
				"graph_reference_population":  population,
				"preserved_existing_case_set": preserved,
				"existing_case_set": map[string]any{
					"case_set_digest": previous.CaseSetDigest,
					"updatedAt":       previous.UpdatedAt,
					"count":           len(previous.Cases),
				},
			})
			return
		}
	}
	path := resolveRecallEvalCasesPath()
	raw, err := json.MarshalIndent(refreshed, "", "  ")
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "failed to encode refreshed cases", "detail": err.Error()})
		return
	}
	if err := prepareOwnerOnlyFile(path, strings.TrimSpace(os.Getenv("ORCH_RECALL_EVAL_CASES_PATH")) == ""); err != nil {
		log.Printf("recall eval store preparation failed: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "failed to create recall eval directory", "code": "storage_access_error"})
		return
	}
	if err := writeOwnerOnlyAtomicFile(path, raw, false); err != nil {
		log.Printf("recall eval store write failed: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "failed to persist refreshed recall eval cases", "code": "storage_io_error"})
		return
	}
	casesAny, _ := refreshed["cases"].([]map[string]any)
	refreshedHealth := validateSavedRecallEvalCaseSet(recallEvalSavedConfig{
		Path:          path,
		SchemaID:      anyToString(refreshed["schema_id"]),
		Version:       refreshed["version"],
		UpdatedAt:     refreshed["updatedAt"],
		CaseSetDigest: anyToString(refreshed["case_set_digest"]),
		Source:        anyToString(refreshed["source"]),
		Synthetic:     anyToBool(refreshed["synthetic"]),
		Snapshot:      cloneAnyMap(anyMap(refreshed["snapshot"])),
		Custody:       cloneAnyMap(anyMap(refreshed["custody"])),
		SplitCounts:   cloneAnyMap(anyMap(refreshed["split_counts"])),
		K:             anyToInt(refreshed["k"], defaultRecallEvalK),
		Gate: recallEvalGate{
			MinRecallAtK:      defaultRecallEvalGateMinRecallAtK,
			MinMRR:            defaultRecallEvalGateMinMRR,
			MinNumericExactly: defaultRecallEvalGateMinNumeric,
		},
		Cases: casesAny,
	})
	response := map[string]any{
		"ok":              anyToBool(refreshedHealth["valid"]),
		"case_set_health": refreshedHealth,
		"savedCaseSet": map[string]any{
			"case_set_id":       ownerOnlyStoreRef("recall_eval_cases"),
			"version":           refreshed["version"],
			"updatedAt":         refreshed["updatedAt"],
			"count":             len(casesAny),
			"maxCases":          maxCases,
			"minHits":           minHits,
			"graphCaseCount":    anyToInt(refreshed["graphCaseCount"], 0),
			"graphCasesEnabled": includeGraphCases,
			"caseSetHealthy":    anyToBool(refreshedHealth["valid"]),
			"benchmarkEligible": anyToBool(refreshedHealth["benchmark_eligible"]),
			"caseSetDigest":     refreshed["case_set_digest"],
			"snapshot":          refreshed["snapshot"],
			"custody":           refreshed["custody"],
		},
	}
	if anyToBool(payload["run_evaluation"]) {
		response["evaluation"] = map[string]any{
			"ok":      false,
			"warning": "native refresh completed; run /memory/recall/evaluate/saved for full evaluation payload",
		}
	}
	writeJSON(w, http.StatusOK, response)
}

// memoryRecallGraphCorpusRefresh persists a separate closed graph benchmark.
// It intentionally does not replace the direct saved-recall v3 artifact.
func (s *server) memoryRecallGraphCorpusRefresh(w http.ResponseWriter, r *http.Request, payload map[string]any) {
	s.graphRecallCorpusRefreshMu.Lock()
	defer s.graphRecallCorpusRefreshMu.Unlock()
	project := strings.TrimSpace(anyToString(payload["project"]))
	topicPrefix := strings.TrimSpace(anyToString(payload["topic_prefix"]))
	seed := firstNonEmptyStrings(strings.TrimSpace(anyToString(payload["seed"])), "graph-v1")
	artifact := s.buildSavedRecallGraphCorpus(r.Context(), project, topicPrefix, seed)
	path := resolveSavedRecallGraphCorpusPath()
	health, persisted, validationErr := saveSavedRecallGraphCorpusArtifactIfHealthy(path, artifact)
	if validationErr != nil {
		log.Printf("graph recall corpus validation failed: %v", validationErr)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "failed to validate graph recall corpus", "code": "validation_error"})
		return
	}
	if !persisted {
		if err := saveSavedRecallGraphCorpusAttemptReceipt(path, artifact, health); err != nil {
			log.Printf("graph recall corpus attempt receipt persist failed: %v", err)
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "failed to persist graph recall corpus attempt receipt", "code": "storage_io_error", "case_set_health": health})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":                    false,
			"schema_id":             savedRecallGraphCorpusSchemaID,
			"graph_corpus":          true,
			"case_set_health":       health,
			"insufficiency_receipt": cloneJSONMap(anyMap(artifact["insufficiency_receipt"])),
			"canonical_replaced":    false,
			"attempt_receipt_saved": true,
		})
		return
	}
	validationReceipt := graphRecallCorpusValidationReceipt(savedRecallGraphCorpusConfigFromArtifact(path, artifact), health)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                    anyToBool(health["valid"]),
		"schema_id":             savedRecallGraphCorpusSchemaID,
		"graph_corpus":          true,
		"case_set_health":       health,
		"validation_receipt":    validationReceipt,
		"insufficiency_receipt": cloneJSONMap(anyMap(artifact["insufficiency_receipt"])),
		"savedCaseSet": map[string]any{
			"case_set_id":        ownerOnlyStoreRef("recall_graph_corpus"),
			"schema_id":          artifact["schema_id"],
			"version":            artifact["version"],
			"count":              len(anyToMapSlice(artifact["cases"])),
			"development_count":  len(anyToMapSlice(artifact["development_cases"])),
			"holdout_count":      len(anyToMapSlice(artifact["holdout_cases"])),
			"case_set_digest":    artifact["case_set_digest"],
			"manifest_digest":    anyToString(anyMap(artifact["manifest"])["digest"]),
			"topology_counts":    artifact["topology_counts"],
			"incremental_needed": len(anyToMapSlice(artifact["incremental_needed_cases"])),
			"custody":            artifact["custody"],
			"cost":               artifact["cost"],
		},
	})
}

func (s *server) memoryRecallEvaluateSaved(w http.ResponseWriter, r *http.Request) {
	s.memoryRecallEvaluateSavedNative(w, r)
}

func (s *server) buildRefreshedRecallEvalCaseSet(maxCases int, minHits int, project string, topicPrefix string) map[string]any {
	return s.buildRefreshedRecallEvalCaseSetWithGraphContext(context.Background(), maxCases, minHits, project, topicPrefix, false, 0)
}

func (s *server) buildRefreshedRecallEvalCaseSetWithGraph(maxCases int, minHits int, project string, topicPrefix string, includeGraph bool, graphMaxCases int) map[string]any {
	return s.buildRefreshedRecallEvalCaseSetWithGraphContext(context.Background(), maxCases, minHits, project, topicPrefix, includeGraph, graphMaxCases)
}

func (s *server) buildRefreshedRecallEvalCaseSetWithGraphContext(ctx context.Context, maxCases int, minHits int, project string, topicPrefix string, includeGraph bool, graphMaxCases int) map[string]any {
	maxCases = clampInt(maxCases, 1, savedRecallEvalV3MaxCases)
	graphMaxCases = clampInt(graphMaxCases, 0, minInt(savedRecallEvalV3MaxGraphCases, maxInt(0, maxCases-1)))
	if minHits < 1 {
		minHits = 1
	}
	project = strings.TrimSpace(project)
	topicPrefix = normalizeTopicPathLoose(topicPrefix)
	if ctx == nil {
		ctx = context.Background()
	}
	allCandidates, sourceKind, sourceStats := s.recallEvalIndexedCandidates(ctx, project, topicPrefix, savedRecallEvalV3MaxSourceDocs)
	eligible := recallEvalEligibleCandidates(allCandidates, minHits, project, topicPrefix)
	// Select the complete direct pool first. Graph rows replace direct rows only
	// when the graph store can actually produce valid cases; reserving the
	// configured maximum up front silently reduced real no-graph case sets.
	directCandidates, temporal := recallEvalSelectCandidates(eligible, maxCases)
	cases := recallEvalCasesFromCandidates(directCandidates, project)
	graphCases := make([]map[string]any, 0, graphMaxCases)
	if includeGraph && graphMaxCases > 0 && len(eligible) > 0 && len(cases) > 0 {
		candidateDocs := make([]memoryStoreDoc, 0, len(eligible))
		for _, candidate := range eligible {
			candidateDocs = append(candidateDocs, candidate.doc)
		}
		// Graph seeds are drawn from the complete bounded eligible population.
		// Restricting them to the direct sample can report zero references even
		// when the current-state graph has enough valid positives outside that
		// sample. The source population is already capped by
		// savedRecallEvalV3MaxSourceDocs, and graph generation stops at its
		// requested bound.
		graphSeeds := recallEvalCasesFromCandidates(eligible, project)
		graphCases = s.recallEvalGraphCasesFromDocs(ctx, candidateDocs, graphSeeds, graphMaxCases)
	}
	if len(graphCases) > 0 {
		directCount := maxInt(0, maxCases-len(graphCases))
		directCandidates, temporal = recallEvalSelectCandidates(eligible, directCount)
		cases = recallEvalCasesFromCandidates(directCandidates, project)
		cases = append(cases, graphCases...)
	}
	for _, graphCase := range graphCases {
		graphCase["case_kind"] = "graph_neighbor"
	}
	snapshotDigest := recallEvalSnapshotDigest(eligible)
	caseSetDigest := recallEvalCaseSetDigest(cases)
	updatedAt := nowUTCISO()
	splitCounts := map[string]any{"train": 0, "holdout": 0}
	for _, rawCase := range cases {
		split := strings.ToLower(strings.TrimSpace(anyToString(rawCase["split"])))
		if split == "" {
			split = "train"
		}
		splitCounts[split] = anyToInt(splitCounts[split], 0) + 1
	}
	snapshot := map[string]any{
		"schema_id":           savedRecallEvalV3SnapshotSchemaID,
		"captured_at":         updatedAt,
		"source":              sourceKind,
		"project_scope":       project,
		"topic_prefix":        topicPrefix,
		"candidate_count":     len(eligible),
		"selected_case_count": len(cases),
		"source_cap":          savedRecallEvalV3MaxSourceDocs,
		"digest":              "sha256:" + snapshotDigest,
		"temporal_holdout":    temporal,
	}
	populationMetadata := recallEvalPopulationMetadata(eligible, cases)
	graphReferenceCount := 0
	for _, graphCase := range graphCases {
		if anyToString(graphCase["graph_label_kind"]) == "current_state_bound_reference" {
			graphReferenceCount++
		}
	}
	graphReferenceReady := graphReferenceCount >= savedRecallEvalV3MinGraphReferences
	snapshot["population"] = populationMetadata["population"]
	snapshot["sample"] = populationMetadata["sample"]
	snapshot["diversity"] = populationMetadata["diversity"]
	snapshot["graph_reference_population"] = map[string]any{
		"available":    graphReferenceCount,
		"requested":    graphMaxCases,
		"minimum":      savedRecallEvalV3MinGraphReferences,
		"ready":        graphReferenceReady,
		"truth_source": "current_state_bound_reference_edges",
	}
	snapshot["source_stats"] = cloneAnyMap(sourceStats)
	custody := map[string]any{
		"schema_id":              savedRecallEvalV3CustodySchemaID,
		"owner":                  "gateway-go",
		"mode":                   "frozen_live_index",
		"synthetic":              false,
		"source_snapshot_digest": "sha256:" + snapshotDigest,
		"case_set_digest":        "sha256:" + caseSetDigest,
		"derivation":             "file_backed_memory_summary_with_filename_redaction",
		"oracle_leakage":         "filename_removed_from_query; summary-derived labels retained",
		"population_count":       len(eligible),
		"sample_count":           len(cases),
		"source_stats":           cloneAnyMap(sourceStats),
		"diversity_valid":        anyToBool(anyMap(populationMetadata["diversity"])["valid"]),
	}
	return map[string]any{
		"schema_id":        savedRecallEvalV3SchemaID,
		"version":          savedRecallEvalV3Version,
		"updatedAt":        updatedAt,
		"case_set_digest":  "sha256:" + caseSetDigest,
		"snapshot":         snapshot,
		"custody":          custody,
		"source":           sourceKind,
		"synthetic":        false,
		"population_count": len(eligible),
		"sample_count":     len(cases),
		"source_stats":     cloneAnyMap(sourceStats),
		"diversity_valid":  anyToBool(anyMap(populationMetadata["diversity"])["valid"]),
		"split_counts":     splitCounts,
		"k":                defaultRecallEvalK,
		"gate": map[string]any{
			"minRecallAtK":        defaultRecallEvalGateMinRecallAtK,
			"minMrr":              defaultRecallEvalGateMinMRR,
			"minNumericExactness": defaultRecallEvalGateMinNumeric,
		},
		"cases":             cases,
		"graphCaseCount":    len(graphCases),
		"graphCasesEnabled": includeGraph,
		"graph_reference_population": map[string]any{
			"available":    graphReferenceCount,
			"requested":    graphMaxCases,
			"minimum":      savedRecallEvalV3MinGraphReferences,
			"ready":        graphReferenceReady,
			"truth_source": "current_state_bound_reference_edges",
		},
	}
}

func recallEvalCasesFromDocs(docs []memoryStoreDoc, maxCases int, minHits int, project string, topicPrefix string) []map[string]any {
	topicCounts := map[string]int{}
	for _, doc := range docs {
		topic := recallEvalNormalizeTopicPath(doc.TopicPath)
		if topic == "" {
			continue
		}
		topicCounts[topic] += 1
	}
	filtered := make([]memoryStoreDoc, 0, len(docs))
	for _, doc := range docs {
		if strings.TrimSpace(doc.FileName) == "" {
			continue
		}
		topic := recallEvalNormalizeTopicPath(doc.TopicPath)
		if topicPrefix != "" && !strings.HasPrefix(topic, topicPrefix) {
			continue
		}
		if minHits > 1 && topicCounts[topic] < minHits {
			continue
		}
		if recallEvalQueryFromDoc(doc) == "" {
			continue
		}
		filtered = append(filtered, doc)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		leftTopic := recallEvalNormalizeTopicPath(filtered[i].TopicPath)
		rightTopic := recallEvalNormalizeTopicPath(filtered[j].TopicPath)
		leftDepth := topicDepth(leftTopic)
		rightDepth := topicDepth(rightTopic)
		if leftDepth != rightDepth {
			return leftDepth > rightDepth
		}
		if !filtered[i].UpdatedAt.Equal(filtered[j].UpdatedAt) {
			return filtered[i].UpdatedAt.After(filtered[j].UpdatedAt)
		}
		return strings.TrimSpace(filtered[i].FileName) < strings.TrimSpace(filtered[j].FileName)
	})
	seen := map[string]struct{}{}
	cases := make([]map[string]any, 0, maxCases)
	for _, doc := range filtered {
		fileName := strings.Trim(strings.TrimSpace(doc.FileName), "/")
		if fileName == "" {
			continue
		}
		dedupeKey := strings.ToLower(strings.TrimSpace(doc.Project + "::" + fileName))
		if _, ok := seen[dedupeKey]; ok {
			continue
		}
		seen[dedupeKey] = struct{}{}
		topic := recallEvalNormalizeTopicPath(doc.TopicPath)
		expectedTerms := []string{}
		if summary := strings.TrimSpace(doc.Summary); summary != "" {
			expectedTerms = append(expectedTerms, clipText(strings.ToLower(summary), 96))
		}
		caseProject := strings.TrimSpace(doc.Project)
		if caseProject == "" {
			caseProject = project
		}
		cases = append(cases, map[string]any{
			"id":                  recallEvalCaseID(caseProject+"::"+fileName, len(cases)),
			"query":               recallEvalQueryFromDoc(doc),
			"project":             caseProject,
			"topic_path":          topic,
			"limit":               10,
			"expected_files":      []string{fileName},
			"expected_substrings": expectedTerms,
		})
		if len(cases) >= maxCases {
			break
		}
	}
	return cases
}

func (s *server) recallEvalGraphCasesFromDocs(ctx context.Context, docs []memoryStoreDoc, directCases []map[string]any, maxCases int) []map[string]any {
	if s == nil || s.memoryStore == nil || maxCases < 1 {
		return []map[string]any{}
	}
	docsByID := make(map[string]memoryStoreDoc, len(docs))
	for _, doc := range docs {
		_, _, memoryID, _, err := canonicalMemoryID(doc.Project + "::" + doc.FileName)
		if err == nil {
			docsByID[strings.ToLower(memoryID)] = doc
		}
	}
	// New reference labels are limited to explicit, current-state-bound
	// references. The legacy association branch is retained only so an
	// already-frozen graph case can be read without being silently rewritten;
	// it is marked separately and is not part of the reference population.
	allowedRelations := map[string]struct{}{"references": {}, "same_session": {}, "same_topic": {}}
	seenEdges := map[string]struct{}{}
	graphCases := make([]map[string]any, 0, maxCases)
	for _, direct := range directCases {
		project := strings.TrimSpace(anyToString(direct["project"]))
		expectedFiles := sortedKeys(normalizeExpectedFileTokens(direct["expected_files"]))
		if project == "" || len(expectedFiles) != 1 {
			continue
		}
		_, _, seedID, _, err := canonicalMemoryID(project + "::" + expectedFiles[0])
		if err != nil {
			continue
		}
		edges, err := s.memoryStore.listMemoryEdges(ctx, memoryEdgeQuery{MemoryID: seedID, Project: project, Limit: 100})
		if err != nil {
			continue
		}
		for _, edge := range edges {
			if edge.Relation == "references" && !memoryReferenceBindingValid(edge.Binding) {
				continue
			}
			if edge.Confidence < 0.95 {
				continue
			}
			if _, allowed := allowedRelations[edge.Relation]; !allowed {
				continue
			}
			candidateID := edge.TargetID
			if strings.EqualFold(candidateID, seedID) {
				candidateID = edge.SourceID
			}
			_, _, canonicalCandidate, _, err := canonicalMemoryID(candidateID)
			if err != nil || strings.EqualFold(canonicalCandidate, seedID) {
				continue
			}
			target, exists := docsByID[strings.ToLower(canonicalCandidate)]
			if !exists || strings.TrimSpace(target.FileName) == "" {
				continue
			}
			if _, duplicate := seenEdges[edge.EdgeID]; duplicate {
				continue
			}
			seenEdges[edge.EdgeID] = struct{}{}
			graphCase := cloneMap(direct)
			graphCase["id"] = "graph-" + sha256Hex(seedID + "\x00" + edge.EdgeID)[:20]
			graphCase["case_kind"] = "graph_neighbor"
			graphCase["graph_seed_memory_id"] = seedID
			graphCase["graph_target_memory_id"] = canonicalCandidate
			graphCase["graph_expected_files"] = []string{target.FileName}
			graphCase["graph_expected_relations"] = []string{edge.Relation}
			graphCase["graph_min_confidence"] = 0.95
			if edge.Relation == "references" {
				graphCase["graph_label_kind"] = "current_state_bound_reference"
			} else {
				graphCase["graph_label_kind"] = "legacy_association"
			}
			graphCases = append(graphCases, graphCase)
			break
		}
		if len(graphCases) >= maxCases {
			break
		}
	}
	return graphCases
}

func recallEvalSummarySnippets(row map[string]any) []string {
	snippets := make([]string, 0, 3)
	for _, item := range anyToStringSlice(row["summarySnippets"]) {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			snippets = append(snippets, trimmed)
		}
	}
	if summary := strings.TrimSpace(anyToString(row["summary"])); summary != "" {
		snippets = append(snippets, summary)
	}
	return snippets
}

func recallEvalExpectedFilesFromTopic(row map[string]any) []string {
	seen := map[string]struct{}{}
	files := make([]string, 0, 5)
	add := func(fileName string) {
		trimmed := strings.Trim(strings.TrimSpace(fileName), "/")
		if trimmed == "" {
			return
		}
		normalized := strings.ToLower(trimmed)
		if _, exists := seen[normalized]; exists {
			return
		}
		seen[normalized] = struct{}{}
		files = append(files, trimmed)
	}
	for _, fileName := range anyToStringSlice(row["uniqueFiles"]) {
		add(fileName)
	}
	if partitions, ok := row["filePartitions"].([]any); ok {
		for _, raw := range partitions {
			partition := anyMap(raw)
			add(anyToString(partition["file"]))
		}
	}
	if partitions, ok := row["filePartitions"].([]map[string]any); ok {
		for _, partition := range partitions {
			add(anyToString(partition["file"]))
		}
	}
	if len(files) > 5 {
		files = files[:5]
	}
	return files
}

func recallEvalQueryFromDoc(doc memoryStoreDoc) string {
	topic := strings.ReplaceAll(normalizeTopicPathLoose(doc.TopicPath), "/", " ")
	// The expected file is intentionally absent from the query. Keep the
	// summary because it is the only deterministic content available from the
	// indexed write, but redact path/base-name tokens from that summary and
	// record the derivation caveat in each generated case.
	summary := recallEvalRedactFileTokens(clipText(doc.Summary, 160), doc.FileName)
	query := strings.TrimSpace(strings.Join([]string{topic, summary}, " "))
	if query != "" {
		return query
	}
	return strings.TrimSpace(recallEvalRedactFileTokens(clipText(doc.Summary, 160), doc.FileName))
}

func recallEvalRedactFileTokens(value string, fileName string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	cleanName := strings.ToLower(strings.Trim(strings.TrimSpace(strings.ReplaceAll(fileName, "\\", "/")), "/"))
	baseName := filepath.Base(cleanName)
	stem := strings.TrimSuffix(baseName, filepath.Ext(baseName))
	fileTokens := graphCorpusMeaningfulOverlapTokens(stem, nil)
	if len(fileTokens) == 0 {
		return strings.Join(graphCorpusLexicalTokens(value), " ")
	}
	kept := make([]string, 0)
	for _, token := range graphCorpusLexicalTokens(value) {
		if _, leaked := fileTokens[token]; leaked {
			continue
		}
		kept = append(kept, token)
	}
	return strings.Join(kept, " ")
}

func recallEvalCaseID(seed string, idx int) string {
	token := strings.TrimSpace(seed)
	if token == "" {
		token = strconv.Itoa(idx + 1)
	}
	return "refresh-" + sha256Hex(token)[:16]
}

func recallEvalNormalizeTopicPath(value string) string {
	return strings.Trim(strings.ToLower(strings.TrimSpace(value)), "/")
}

func (s *server) migrationRuntime(w http.ResponseWriter, r *http.Request) {
	if !methodAllowed(r.Method, http.MethodGet, http.MethodPost) {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	flags := map[string]any{
		"use_rust_codec":     envBool("USE_RUST_CODEC", true),
		"use_rust_memory":    envBool("USE_RUST_MEMORY", true),
		"use_rust_retrieval": envBool("USE_RUST_RETRIEVAL", true),
		"use_go_orchestrator": envBool(
			"USE_GO_ORCHESTRATOR",
			true,
		),
		"engine_mode":     strings.TrimSpace(os.Getenv("CONTEXTLATTICE_ENGINE_MODE")),
		"shadow_dual_run": envBool("CONTEXTLATTICE_SHADOW_DUAL_RUN", false),
		"canary_enabled":  envBool("CONTEXTLATTICE_CANARY_ENABLED", false),
	}
	if strings.TrimSpace(anyToString(flags["engine_mode"])) == "" {
		flags["engine_mode"] = "service"
	}
	services := s.strictRuntimeServices()
	healthyServices := 0
	for _, svc := range services {
		if anyToBool(svc["healthy"]) {
			healthyServices++
		}
	}
	pythonFallbackMode := "available"
	if s.strictNoPythonRuntime {
		pythonFallbackMode = "disabled"
	}
	payload := map[string]any{
		"enabled": true,
		"flags":   flags,
		"implementations": map[string]any{
			"gateway":         sourceOwnerGoNative,
			"retrieval":       sourceOwnerGoNative,
			"topic_rollups":   sourceOwnerGoNative,
			"memory_bank":     sourceOwnerRustNative,
			"python_fallback": pythonFallbackMode,
		},
		"snapshot": map[string]any{
			"strictNoPythonRuntime": s.strictNoPythonRuntime,
			"routeOwnerClass":       sourceOwnerGoNative,
			"pythonHotPathOwnership": map[string]any{
				"mode":       anyToString(s.pythonHotPathOwnershipSnapshot()["mode"]),
				"fallbacks":  anyToInt(s.pythonHotPathOwnershipSnapshot()["fallbacks"], 0),
				"status":     anyToString(s.pythonHotPathOwnershipSnapshot()["status"]),
				"lastAt":     anyToString(s.pythonHotPathOwnershipSnapshot()["lastFallbackAt"]),
				"lastReason": anyToString(s.pythonHotPathOwnershipSnapshot()["lastReason"]),
			},
			"runtimeBackendPolicy":    defaultRustBackendPolicy(),
			"retrievalFastSources":    append([]string{}, s.retrieval.fastSources...),
			"retrievalSlowSources":    append([]string{}, s.retrieval.slowSources...),
			"retrievalDefaultSources": append([]string{}, s.retrieval.defaultSources...),
			"serviceHealth": map[string]any{
				"healthy": healthyServices,
				"total":   len(services),
			},
		},
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *server) memoryFilesByProject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	if s.memoryStore == nil || !s.memoryStore.isEnabled() {
		s.forwardJSONGET(w, r, r.URL.Path)
		return
	}
	project, fileName, err := s.memoryStore.parseProjectFileFromPath(r.URL.Path)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	content, info, readErr := s.memoryStore.readFile(project, fileName)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			writeJSON(w, http.StatusNotFound, map[string]any{"detail": "memory file not found"})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "memory file read failed", "detail": readErr.Error()})
		return
	}
	w.Header().Set("content-type", "text/plain; charset=utf-8")
	w.Header().Set("x-contextlattice-source", "go_memory_store")
	if info != nil {
		w.Header().Set("x-contextlattice-last-modified", info.ModTime().UTC().Format(time.RFC3339Nano))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(content))
}

func (s *server) agentsTasksRoute(w http.ResponseWriter, r *http.Request) {
	if s == nil || s.taskLedger == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "authoritative Gateway task ledger unavailable"})
		return
	}
	// Every live task read and mutation uses the SQLite-WAL delivery ledger.
	// The legacy backend is never a second task writer.
	s.agentTaskDeliveryRoute(w, r)
}

func (s *server) telemetryRoute(w http.ResponseWriter, r *http.Request) {
	if !methodAllowed(r.Method, http.MethodGet) {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	s.forwardJSONGET(w, r, r.URL.Path)
}

func (s *server) maintenanceRoute(w http.ResponseWriter, r *http.Request) {
	if !methodAllowed(r.Method, http.MethodGet, http.MethodPost) {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	s.forwardJSONAny(w, r, r.URL.Path)
}
