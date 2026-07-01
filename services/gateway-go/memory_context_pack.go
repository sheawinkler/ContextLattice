package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

func (s *server) memoryContextPack(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
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
	payload, err := parseJSONMap(bodyBytes)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
		return
	}
	response, status, execErr := s.buildContextPackResponse(r.Context(), incomingHeaders, payload)
	if execErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "context_pack_unavailable", "detail": sanitizeProviderOverflowText(execErr.Error())})
		return
	}
	writeJSON(w, status, response)
}

func (s *server) toolsContextPack(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	incomingHeaders, ok := s.prepareToolHeaders(w, r, "/tools/context_pack")
	if !ok {
		return
	}
	payload, err := readOptionalJSONBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
		return
	}
	response, status, execErr := s.buildContextPackResponse(r.Context(), incomingHeaders, payload)
	if execErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "context_pack_unavailable", "detail": sanitizeProviderOverflowText(execErr.Error())})
		return
	}
	response["tool"] = "context_pack"
	response = attachPayloadFormatContract(contextPackResponseContractID, response, anyToString(response["agent_id"]), "context_pack", "/tools/context_pack")
	writeJSON(w, status, response)
}

func (s *server) buildContextPackResponse(
	ctx context.Context,
	incomingHeaders http.Header,
	payload map[string]any,
) (map[string]any, int, error) {
	requestPayload := cloneMap(payload)
	query := strings.TrimSpace(anyToString(requestPayload["query"]))
	if query == "" {
		return map[string]any{"error": "query is required"}, http.StatusUnprocessableEntity, nil
	}
	limit := clampInt(anyToInt(requestPayload["limit"], 10), 1, 100)
	maxFacts := clampInt(anyToInt(requestPayload["max_facts"], 20), 1, 100)
	includeRetrievalDebug := anyToBool(requestPayload["include_retrieval_debug"])

	searchRequest := map[string]any{
		"query":                   query,
		"limit":                   limit,
		"project":                 strings.TrimSpace(anyToString(requestPayload["project"])),
		"topic_path":              strings.TrimSpace(anyToString(requestPayload["topic_path"])),
		"retrieval_mode":          strings.TrimSpace(anyToString(requestPayload["retrieval_mode"])),
		"retrieval_intent":        strings.TrimSpace(anyToString(requestPayload["retrieval_intent"])),
		"user_id":                 strings.TrimSpace(anyToString(requestPayload["user_id"])),
		"include_preferences":     anyToBool(requestPayload["include_preferences"]),
		"include_retrieval_debug": includeRetrievalDebug,
		"agent_id":                strings.TrimSpace(anyToString(requestPayload["agent_id"])),
		"session_id":              strings.TrimSpace(firstNonEmptyStrings(anyToString(requestPayload["session_id"]), anyToString(requestPayload["sessionId"]))),
		"traffic_class":           strings.TrimSpace(anyToString(requestPayload["traffic_class"])),
		"include_grounding":       true,
	}
	objectiveCtx := extractObjectiveContext(requestPayload)
	effectiveObjectiveCtx := objectiveCtx
	if effectiveObjectiveCtx.empty() {
		effectiveObjectiveCtx = objectiveCtx.withDefaults()
	}
	if !objectiveCtx.empty() {
		searchRequest["objective_context"] = objectiveCtx.toMap()
	}
	if value := requestPayload["sources"]; value != nil {
		searchRequest["sources"] = value
	}
	if value := requestPayload["source_weights"]; value != nil {
		searchRequest["source_weights"] = value
	}
	if value, present := requestPayload["auto_escalate"]; present {
		searchRequest["auto_escalate"] = value
	}
	if value, present := requestPayload["query_expansion"]; present {
		searchRequest["query_expansion"] = value
	}
	for _, flag := range []string{"include_ephemeral", "include_ephemeral_memory", "include_test_memory"} {
		if value, present := requestPayload[flag]; present {
			searchRequest[flag] = value
		}
	}
	for _, flag := range []string{"blocking", "wait_for_slow_sources", "sync_slow_sources"} {
		if value, present := requestPayload[flag]; present {
			searchRequest[flag] = value
		}
	}
	combinedSources := anyToBoolOrDefault(requestPayload["combined_sources"], true)
	blockSlowByDefault := combinedSources && contextPackBlocksSlowSourcesByDefault()
	if _, present := requestPayload["wait_for_slow_sources"]; !present && blockSlowByDefault {
		searchRequest["wait_for_slow_sources"] = true
	}
	if _, present := requestPayload["sync_slow_sources"]; !present && blockSlowByDefault {
		searchRequest["sync_slow_sources"] = true
	}

	searchResponse, status, execErr := s.executeRetrieval(ctx, incomingHeaders, searchRequest, true)
	if execErr != nil {
		return nil, 0, execErr
	}
	retrievalMode := normalizeRetrievalMode(anyToString(searchRequest["retrieval_mode"]))
	retrievalIntent := strings.TrimSpace(strings.ToLower(anyToString(searchRequest["retrieval_intent"])))
	if retrievalIntent == "" {
		retrievalIntent = "decision"
	}
	trafficClass := strings.TrimSpace(strings.ToLower(anyToString(searchRequest["traffic_class"])))
	if trafficClass == "" {
		trafficClass = "user"
	}
	searchResponse["learning_enabled"] = true
	searchResponse["retrieval_mode"] = retrievalMode
	searchResponse["retrieval_intent"] = retrievalIntent
	searchResponse["traffic_class"] = trafficClass
	if agentID := strings.TrimSpace(anyToString(searchRequest["agent_id"])); agentID != "" {
		searchResponse["agent_id"] = agentID
	}
	sessionID := strings.TrimSpace(anyToString(searchRequest["session_id"]))
	if sessionID != "" {
		searchResponse["session_id"] = sessionID
	}

	tokenBudget := contextPackTokenBudgetFromRequest(requestPayload)
	contextPack := buildContextPackPayload(query, searchResponse, maxFacts, limit)
	contextPack["project"] = strings.TrimSpace(anyToString(searchRequest["project"]))
	contextPack["topic_path"] = strings.TrimSpace(anyToString(searchRequest["topic_path"]))
	graphQuality := s.enrichContextPackWithGraph(ctx, contextPack, requestPayload)
	sourceCoverage := contextPackSourceCoverage(searchResponse)
	sourceCoverage = contextPackSourceCoverageWithGraph(sourceCoverage, graphQuality)
	contextPack["sourceCoverage"] = sourceCoverage
	contextPack["combinedSources"] = combinedSources
	compiled := compileContextPackForAgent(query, contextPack, sourceCoverage, effectiveObjectiveCtx, tokenBudget)
	agentGuidance := anyMap(compiled["agent_guidance"])
	contextPack["rankedEvidence"] = compiled["ranked_evidence"]
	contextPack["ranked_evidence"] = compiled["ranked_evidence"]
	contextPack["tokenBudget"] = compiled["token_budget"]
	contextPack["token_budget"] = compiled["token_budget"]
	contextPack["omittedHighValueRefs"] = compiled["omitted_high_value_refs"]
	contextPack["omitted_high_value_refs"] = compiled["omitted_high_value_refs"]
	contextPack["agentGuidance"] = agentGuidance
	contextPack["agent_guidance"] = agentGuidance
	contextPack["promptSections"] = compiled["prompt_sections"]
	contextPack["prompt_sections"] = compiled["prompt_sections"]
	contextPack["contextCompiler"] = compiled["context_compiler"]
	contextPack["context_compiler"] = compiled["context_compiler"]
	referencePrompt := anyToString(compiled["reference_prompt"])
	tokenImpact := buildContextPackTokenImpact(query, contextPack, compiled, referencePrompt)
	s.recordTokenImpact(tokenImpact)
	contextPack["tokenImpact"] = tokenImpact
	contextPack["token_impact"] = tokenImpact
	rankedEvidence := contextPackAnyList(compiled["ranked_evidence"])
	runAdvisor := buildRunAdvisor(runAdvisorInput{
		Query:           query,
		Project:         strings.TrimSpace(anyToString(requestPayload["project"])),
		TopicPath:       strings.TrimSpace(anyToString(requestPayload["topic_path"])),
		RetrievalMode:   retrievalMode,
		SessionID:       sessionID,
		AgentID:         strings.TrimSpace(anyToString(searchResponse["agent_id"])),
		SourceCoverage:  sourceCoverage,
		Retrieval:       searchResponse,
		Objective:       effectiveObjectiveCtx,
		RankedEvidence:  rankedEvidence,
		ReferencePrompt: referencePrompt,
		GraphQuality:    graphQuality,
		Surface:         "/memory/context-pack",
	})
	contextPack["runAdvisor"] = runAdvisor
	contextPack["run_advisor"] = runAdvisor
	response := map[string]any{
		"ok":                      true,
		"query":                   query,
		"context_pack":            contextPack,
		"context_compiler":        compiled["context_compiler"],
		"agent_guidance":          agentGuidance,
		"reference_prompt":        referencePrompt,
		"token_impact":            tokenImpact,
		"run_advisor":             runAdvisor,
		"token_budget":            compiled["token_budget"],
		"omitted_high_value_refs": compiled["omitted_high_value_refs"],
		"warnings":                parseWarnings(searchResponse["warnings"]),
		"retrieval_mode":          searchResponse["retrieval_mode"],
		"retrieval_intent":        searchResponse["retrieval_intent"],
		"traffic_class":           searchResponse["traffic_class"],
		"agent_id":                searchResponse["agent_id"],
		"session_id":              sessionID,
		"source_coverage":         sourceCoverage,
		"writeback_required":      true,
	}
	if includeRetrievalDebug {
		if retrievalDebug, ok := searchResponse["retrieval_debug"]; ok {
			response["retrieval"] = retrievalDebug
		}
	}
	if !effectiveObjectiveCtx.empty() {
		response["objective_context"] = effectiveObjectiveCtx.toMap()
	}
	objectiveRuntime := map[string]any{}
	if sessionID != "" || !effectiveObjectiveCtx.empty() {
		objectiveRuntime = buildObjectiveRuntimeState(
			"context-pack",
			strings.TrimSpace(anyToString(searchResponse["agent_id"])),
			strings.TrimSpace(anyToString(requestPayload["project"])),
			strings.TrimSpace(anyToString(requestPayload["topic_path"])),
			query,
			retrievalMode,
			sessionID,
			effectiveObjectiveCtx,
			"context_pack.completed",
		)
		response["objective_runtime"] = objectiveRuntime
		response["objective_hierarchy"] = objectiveRuntime["objective_hierarchy"]
		response["objective_lineage"] = objectiveRuntime["objective_lineage"]
	}
	if sessionID != "" {
		facts, _ := asAnySlice(contextPack["facts"])
		results, _ := asAnySlice(contextPack["results"])
		session := s.recordAgentSessionEvent(sessionID, "context_pack.completed", map[string]any{
			"agent_id": searchResponse["agent_id"],
			"project":  requestPayload["project"],
			"summary":  query,
			"metadata": map[string]any{
				"endpoint":            "/memory/context-pack",
				"retrieval_mode":      retrievalMode,
				"retrieval_intent":    retrievalIntent,
				"traffic_class":       trafficClass,
				"source_coverage":     sourceCoverage,
				"fact_count":          len(facts),
				"result_count":        len(results),
				"memory_hits":         len(results),
				"warnings_count":      len(parseWarnings(response["warnings"])),
				"objective_state":     anyToString(objectiveRuntime["objective_state"]),
				"next_action":         anyToString(objectiveRuntime["next_action"]),
				"objective_runtime":   objectiveRuntime,
				"objective_hierarchy": objectiveRuntime["objective_hierarchy"],
				"objective_lineage":   objectiveRuntime["objective_lineage"],
				"context_compiler":    compiled["context_compiler"],
				"run_advisor":         runAdvisor,
				"graph_quality":       graphQuality,
			},
		})
		if session != nil {
			response["agent_runtime"] = map[string]any{
				"session_id":          sessionID,
				"memory_contribution": session["memory_contribution"],
				"last_event_type":     session["last_event_type"],
			}
		}
	}
	return attachContextPackFormatContract(response), status, nil
}

func contextPackBlocksSlowSourcesByDefault() bool {
	return envBool("GO_CONTEXT_PACK_BLOCKING_SLOW_DEFAULT", true) ||
		envBool("GO_CONTEXT_PACK_SYNC_SLOW_SOURCES_DEFAULT", false)
}

func contextPackGraphSeedMax() int {
	return clampInt(envInt("GO_CONTEXT_PACK_GRAPH_SEED_MAX", 3), 0, 8)
}

func contextPackGraphNeighborMax() int {
	return clampInt(envInt("GO_CONTEXT_PACK_GRAPH_NEIGHBOR_MAX", 4), 0, 12)
}

func contextPackGraphNeighborPerSeed() int {
	return clampInt(envInt("GO_CONTEXT_PACK_GRAPH_NEIGHBOR_PER_SEED", 2), 1, 6)
}

func (s *server) enrichContextPackWithGraph(
	ctx context.Context,
	contextPack map[string]any,
	requestPayload map[string]any,
) map[string]any {
	signals := map[string]any{
		"seed_count":           0,
		"candidate_count":      0,
		"added_evidence_count": 0,
		"relations":            map[string]any{},
	}
	quality := map[string]any{
		"status":         "not_sampled",
		"score":          0,
		"used":           false,
		"signals":        signals,
		"recommendation": "Run contextlattice_memory_topology when graph evidence matters.",
	}
	if !envBool("GO_CONTEXT_PACK_GRAPH_NEIGHBORS_ENABLED", true) {
		quality["status"] = "disabled"
		quality["skipped_reason"] = "GO_CONTEXT_PACK_GRAPH_NEIGHBORS_ENABLED=false"
		quality["recommendation"] = "Graph-neighbor context enrichment is disabled by configuration."
		contextPack["graph_context"] = quality
		contextPack["graphContext"] = quality
		return quality
	}
	backend := s.memoryGraphBackend()
	if backend == nil {
		quality["status"] = "unavailable"
		quality["skipped_reason"] = "memory graph backend unavailable"
		quality["recommendation"] = "Enable the Go memory store to sample graph neighbors for context packs."
		contextPack["graph_context"] = quality
		contextPack["graphContext"] = quality
		return quality
	}
	seedLimit := contextPackGraphSeedMax()
	totalLimit := contextPackGraphNeighborMax()
	if seedLimit == 0 || totalLimit == 0 {
		quality["status"] = "disabled"
		quality["skipped_reason"] = "graph seed or neighbor cap is zero"
		quality["recommendation"] = "Raise graph context caps if first-hop memory-edge evidence should be sampled."
		contextPack["graph_context"] = quality
		contextPack["graphContext"] = quality
		return quality
	}

	results := contextPackAnyList(contextPack["results"])
	topicFilter := strings.Trim(strings.TrimSpace(anyToString(requestPayload["topic_path"])), "/")
	includeEphemeral := requestIncludesEphemeralMemory(requestPayload)
	perSeed := contextPackGraphNeighborPerSeed()
	seenNeighbors := map[string]struct{}{}
	relationCounts := map[string]int{}
	graphRows := []any{}
	seeds := 0
	candidates := 0
	for _, raw := range results {
		if seeds >= seedLimit || len(graphRows) >= totalLimit {
			break
		}
		seed := anyMap(raw)
		memoryID := contextPackGraphMemoryID(seed)
		if memoryID == "" {
			continue
		}
		if _, _, canonicalSeed, _, err := canonicalMemoryID(memoryID); err == nil {
			memoryID = canonicalSeed
		} else {
			continue
		}
		seeds += 1
		rows, err := backend.memoryGraphNeighbors(ctx, memoryGraphNeighborQuery{
			MemoryID:         memoryID,
			Limit:            perSeed,
			IncludeEphemeral: includeEphemeral,
			TopicPath:        topicFilter,
		})
		if err != nil {
			continue
		}
		candidates += len(rows)
		for _, row := range rows {
			if len(graphRows) >= totalLimit {
				break
			}
			rendered := renderContextPackGraphNeighbor(memoryID, row)
			if len(rendered) == 0 {
				continue
			}
			key := strings.ToLower(strings.Join([]string{
				anyToString(rendered["memory_id"]),
				anyToString(rendered["relation"]),
				anyToString(rendered["edge_direction"]),
			}, "\x1f"))
			if key == "" {
				continue
			}
			if _, exists := seenNeighbors[key]; exists {
				continue
			}
			seenNeighbors[key] = struct{}{}
			relation := anyToString(rendered["relation"])
			if relation != "" {
				relationCounts[relation] += 1
			}
			graphRows = append(graphRows, rendered)
		}
	}
	signals["seed_count"] = seeds
	signals["candidate_count"] = candidates
	signals["added_evidence_count"] = len(graphRows)
	relationSignals := map[string]any{}
	for relation, count := range relationCounts {
		relationSignals[relation] = count
	}
	signals["relations"] = relationSignals

	if len(graphRows) == 0 {
		quality["status"] = "empty"
		quality["score"] = 35
		quality["skipped_reason"] = "no first-hop graph neighbors for ranked seed memories"
		quality["recommendation"] = "Run memory-edge-backfill or disk-corpus inferred retrofill for older projects when graph recall should help."
		contextPack["graph_neighbors"] = []any{}
		contextPack["graphNeighbors"] = []any{}
		contextPack["graph_context"] = quality
		contextPack["graphContext"] = quality
		return quality
	}

	contextPack["graph_neighbors"] = graphRows
	contextPack["graphNeighbors"] = graphRows
	contextPack["results"] = append(contextPackAnyList(contextPack["results"]), graphRows...)
	contextPack["files_to_read"] = mergeContextPackFiles(contextPack["files_to_read"], graphRows, 12)
	contextPack["filesToRead"] = contextPack["files_to_read"]
	quality["status"] = "sampled"
	quality["score"] = clampInt(70+len(graphRows)*5, 70, 95)
	quality["used"] = true
	quality["recommendation"] = "Use high-confidence first-hop memory edges as supporting context, then inspect cited files before making code claims."
	contextPack["graph_context"] = quality
	contextPack["graphContext"] = quality
	return quality
}

func contextPackGraphMemoryID(row map[string]any) string {
	if row == nil {
		return ""
	}
	if memoryID := strings.TrimSpace(anyToString(row["memory_id"])); memoryID != "" {
		return memoryID
	}
	project := strings.TrimSpace(anyToString(row["project"]))
	fileName := strings.TrimSpace(firstNonEmptyStrings(anyToString(row["file"]), anyToString(row["file_name"])))
	if project == "" || fileName == "" {
		return ""
	}
	return project + "::" + fileName
}

func renderContextPackGraphNeighbor(seedMemoryID string, row map[string]any) map[string]any {
	if row == nil {
		return nil
	}
	memoryID := strings.TrimSpace(anyToString(row["memory_id"]))
	project := strings.TrimSpace(anyToString(row["project"]))
	fileName := strings.TrimSpace(anyToString(row["file"]))
	if memoryID == "" || project == "" || fileName == "" {
		return nil
	}
	relation := strings.TrimSpace(anyToString(row["relation"]))
	direction := strings.TrimSpace(anyToString(row["edge_direction"]))
	score := clampFloat(anyToFloat(row["score"]), 0.1, 0.99)
	summary := "Graph neighbor via " + relation + ": " + memoryID
	if direction != "" {
		summary += " (" + direction + " edge from " + seedMemoryID + ")"
	}
	return map[string]any{
		"memory_id":      memoryID,
		"project":        project,
		"file":           fileName,
		"topic_path":     strings.Trim(strings.TrimSpace(anyToString(row["topic_path"])), "/"),
		"summary":        clipText(summary, 360),
		"score":          roundFloat(score, 3),
		"source":         memoryEdgeSource,
		"source_owner":   sourceOwnerGoNative,
		"relation":       relation,
		"edge_id":        strings.TrimSpace(anyToString(row["edge_id"])),
		"edge_direction": direction,
		"seed_memory_id": seedMemoryID,
	}
}

func mergeContextPackFiles(existing any, graphRows []any, limit int) []any {
	if limit < 1 {
		limit = 1
	}
	limit = clampInt(limit, 1, 32)
	out := []any{}
	seen := map[string]struct{}{}
	add := func(fileName string) {
		fileName = strings.TrimSpace(fileName)
		if fileName == "" || len(out) >= limit {
			return
		}
		key := strings.ToLower(fileName)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		out = append(out, fileName)
	}
	for _, raw := range contextPackAnyList(existing) {
		add(anyToString(raw))
	}
	for _, raw := range graphRows {
		add(anyToString(anyMap(raw)["file"]))
	}
	return out
}

type contextPackTokenBudget struct {
	AgentContextBudgetTokens int
	ModelContextWindowTokens int
	ReservedResponseTokens   int
	AlreadyLoadedTokens      int
	TargetContextPackTokens  int
	RankedEvidenceTokens     int
	Active                   bool
	EstimateMethod           string
}

type contextPackEvidenceAllocation struct {
	RankedEvidence       []any
	OmittedHighValueRefs []any
	TokenBudget          map[string]any
	UsedTokensEstimate   int
	CompressionLevel     string
}

type contextPackEvidenceItem struct {
	Rank            int
	Kind            string
	Score           float64
	ImpactScore     float64
	ValueDensity    float64
	Reason          string
	Text            string
	Project         string
	File            string
	Source          string
	TopicPath       string
	Timestamp       string
	Confidence      float64
	EstimatedTokens int
	WhySelected     []any
	DiversityKey    string
}

func contextPackTokenBudgetFromRequest(payload map[string]any) contextPackTokenBudget {
	readInt := func(keys ...string) int {
		for _, key := range keys {
			if value, ok := payload[key]; ok {
				parsed := anyToInt(value, 0)
				if parsed > 0 {
					return parsed
				}
			}
		}
		return 0
	}
	budget := contextPackTokenBudget{
		AgentContextBudgetTokens: readInt("agent_context_budget_tokens", "agentContextBudgetTokens"),
		ModelContextWindowTokens: readInt("model_context_window_tokens", "modelContextWindowTokens"),
		ReservedResponseTokens:   readInt("reserved_response_tokens", "reservedResponseTokens"),
		AlreadyLoadedTokens:      readInt("already_loaded_tokens", "alreadyLoadedTokens"),
		TargetContextPackTokens:  readInt("target_context_pack_tokens", "targetContextPackTokens", "budget_tokens", "budgetTokens"),
		EstimateMethod:           "chars_div_4",
	}
	if budget.TargetContextPackTokens <= 0 {
		available := 0
		if budget.AgentContextBudgetTokens > 0 {
			available = budget.AgentContextBudgetTokens
		} else if budget.ModelContextWindowTokens > 0 {
			available = budget.ModelContextWindowTokens
		}
		available -= budget.ReservedResponseTokens
		available -= budget.AlreadyLoadedTokens
		if available > 0 {
			budget.TargetContextPackTokens = available
		}
	}
	if budget.TargetContextPackTokens > 0 {
		budget.TargetContextPackTokens = clampInt(budget.TargetContextPackTokens, 128, 32768)
		budget.RankedEvidenceTokens = clampInt((budget.TargetContextPackTokens*60)/100, 96, budget.TargetContextPackTokens)
		budget.Active = true
	}
	return budget
}

func contextPackEstimateTokens(text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 1
	}
	return maxInt(1, len(text)/4)
}

func compileContextPackForAgent(query string, contextPack map[string]any, sourceCoverage map[string]any, objectiveCtx objectiveContext, tokenBudget contextPackTokenBudget) map[string]any {
	allocation := contextPackRankedEvidence(contextPack, tokenBudget)
	rankedEvidence := allocation.RankedEvidence
	agentGuidance := buildAgentEvidenceGuidance(agentEvidenceGuidanceInput{
		Query:          query,
		Surface:        "context_pack",
		SourceCoverage: sourceCoverage,
		RankedEvidence: rankedEvidence,
		MaxThemes:      6,
		MaxRiskMarkers: 6,
		MaxLinks:       5,
		MaxHints:       8,
	})
	promptSections := contextPackPromptSections(query, contextPack, sourceCoverage, objectiveCtx, rankedEvidence, allocation.TokenBudget, allocation.OmittedHighValueRefs, agentGuidance)
	strategy := "ranked_evidence_prompt_packet"
	if tokenBudget.Active {
		strategy = "impact_per_token_prompt_packet"
	}
	compiler := map[string]any{
		"schema_id":           "contextlattice_context_compiler.v1",
		"version":             1,
		"strategy":            strategy,
		"intended_use":        "send the next model call with bounded task context, ranked evidence, constraints, and checks",
		"recommended_surface": "cli_for_local_agents",
		"alternate_surfaces": []any{
			"http_for_app_integrations",
			"mcp_for_tool_calling_hosts",
		},
		"ranked_evidence_count":           len(rankedEvidence),
		"ranked_evidence_tokens_estimate": allocation.UsedTokensEstimate,
		"omitted_high_value_ref_count":    len(allocation.OmittedHighValueRefs),
		"token_budget":                    allocation.TokenBudget,
		"agent_guidance":                  true,
		"source_count":                    len(anyToStringList(sourceCoverage["returned"], 64)),
		"complete":                        anyToBool(sourceCoverage["complete"]),
		"guardrails": []any{
			"bounded_contract_output",
			"impact_per_estimated_token_selection",
			"cite_or_inspect_before_claims",
			"no_raw_logs_or_volatile_artifacts",
			"strict_numeric_copy",
		},
	}
	return map[string]any{
		"context_compiler":        compiler,
		"ranked_evidence":         rankedEvidence,
		"token_budget":            allocation.TokenBudget,
		"omitted_high_value_refs": allocation.OmittedHighValueRefs,
		"agent_guidance":          agentGuidance,
		"prompt_sections":         promptSections,
		"reference_prompt":        contextPackReferencePrompt(promptSections),
	}
}

func contextPackRankedEvidence(contextPack map[string]any, tokenBudget contextPackTokenBudget) contextPackEvidenceAllocation {
	out := []contextPackEvidenceItem{}
	seen := map[string]struct{}{}
	impactSignals := func(kind string, text string, source map[string]any) []any {
		signals := []any{kind + "_priority"}
		lower := strings.ToLower(text)
		if strings.TrimSpace(anyToString(source["file"])) != "" {
			signals = append(signals, "file_provenance")
		}
		if strings.TrimSpace(anyToString(source["source"])) != "" {
			signals = append(signals, "source_provenance")
		}
		if strings.TrimSpace(anyToString(source["topic_path"])) != "" {
			signals = append(signals, "topic_scope")
		}
		if containsAny(lower, []string{"fail", "failure", "timeout", "blocked", "blocker", "regression", "risk", "contradict", "do not", "must not"}) {
			signals = append(signals, "risk_or_contradiction_signal")
		}
		if containsAny(lower, []string{"verify", "test", "check", "acceptance", "must pass", "script", "command", "gmake", "go test", "pytest", "curl"}) {
			signals = append(signals, "actionable_verification_signal")
		}
		if strings.TrimSpace(anyToString(source["timestamp"])) != "" {
			signals = append(signals, "timestamped")
		}
		return signals
	}
	add := func(kind string, baseScore float64, reason string, text string, source map[string]any) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		project := strings.TrimSpace(anyToString(source["project"]))
		fileName := strings.TrimSpace(anyToString(source["file"]))
		sourceName := strings.TrimSpace(anyToString(source["source"]))
		topicPath := strings.TrimSpace(anyToString(source["topic_path"]))
		key := strings.Join([]string{kind, project, fileName, sourceName, topicPath, text}, "\x1f")
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		sourceScore := anyToFloat(source["score"])
		score := baseScore
		if sourceScore > 0 {
			score += sourceScore * 10
		}
		signals := impactSignals(kind, text, source)
		for _, raw := range signals {
			switch anyToString(raw) {
			case "risk_or_contradiction_signal":
				score += 10
			case "actionable_verification_signal":
				score += 8
			case "file_provenance", "source_provenance", "topic_scope":
				score += 3
			case "timestamped":
				score += 2
			}
		}
		clippedText := clipText(text, 520)
		estimatedTokens := contextPackEstimateTokens(clippedText + " " + reason + " " + project + " " + fileName + " " + sourceName + " " + topicPath)
		diversityKey := firstNonEmptyStrings(fileName, topicPath, sourceName, kind)
		out = append(out, contextPackEvidenceItem{
			Kind:            kind,
			Score:           score,
			ImpactScore:     score,
			Reason:          reason,
			Text:            clippedText,
			Project:         project,
			File:            fileName,
			Source:          sourceName,
			TopicPath:       topicPath,
			Timestamp:       strings.TrimSpace(anyToString(source["timestamp"])),
			Confidence:      roundFloat(clampFloat(score/100, 0.1, 0.99), 3),
			EstimatedTokens: estimatedTokens,
			ValueDensity:    roundFloat(score/float64(maxInt(estimatedTokens, 1)), 4),
			WhySelected:     signals,
			DiversityKey:    diversityKey,
		})
	}
	for _, item := range contextPackAnyList(contextPack["relevant_decisions"]) {
		source := anyMap(item)
		add("decision", 92, "retrieved decision or policy matched the task", anyToString(source["text"]), source)
	}
	for _, item := range contextPackAnyList(contextPack["known_failure_modes"]) {
		source := anyMap(item)
		add("risk", 88, "known failure mode can prevent repeated mistakes", anyToString(source["text"]), source)
	}
	for _, item := range contextPackAnyList(contextPack["acceptance_criteria"]) {
		source := anyMap(item)
		add("check", 84, "acceptance or verification signal should shape the next action", anyToString(source["text"]), source)
	}
	for _, item := range contextPackAnyList(contextPack["runbooks"]) {
		source := anyMap(item)
		add("runbook", 80, "runbook or workflow can guide execution", anyToString(source["text"]), source)
	}
	for _, item := range contextPackAnyList(contextPack["capabilities_to_use"]) {
		source := anyMap(item)
		add("capability", 76, "capability or skill is relevant to the task", anyToString(source["text"]), source)
	}
	for _, item := range contextPackAnyList(contextPack["facts"]) {
		source := anyMap(item)
		text := firstNonEmptyStrings(anyToString(source["text"]), anyToString(source["summary"]), anyToString(source["claim"]))
		add("fact", 72, "grounded fact returned by retrieval", text, source)
	}
	for _, item := range contextPackAnyList(contextPack["graph_neighbors"]) {
		source := anyMap(item)
		add("graph_neighbor", 70, "first-hop memory edge expands the ranked seed context", anyToString(source["summary"]), source)
	}
	for _, item := range contextPackAnyList(contextPack["results"]) {
		source := anyMap(item)
		if strings.TrimSpace(anyToString(source["source"])) == memoryEdgeSource {
			continue
		}
		add("memory", 64, "retrieved memory result matched the task", anyToString(source["summary"]), source)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score == out[j].Score {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Score > out[j].Score
	})
	selected, omitted, usedTokens, compressionLevel := allocateContextPackEvidence(out, tokenBudget)
	limit := minInt(len(selected), 16)
	rendered := make([]any, 0, limit)
	for idx := 0; idx < limit; idx++ {
		item := selected[idx]
		item.Rank = idx + 1
		renderedItem := map[string]any{
			"rank":             item.Rank,
			"kind":             item.Kind,
			"score":            roundFloat(item.Score, 3),
			"impact_score":     roundFloat(item.ImpactScore, 3),
			"value_density":    item.ValueDensity,
			"estimated_tokens": item.EstimatedTokens,
			"confidence":       item.Confidence,
			"reason":           item.Reason,
			"why_selected":     item.WhySelected,
			"text":             item.Text,
		}
		if item.Project != "" {
			renderedItem["project"] = item.Project
		}
		if item.File != "" {
			renderedItem["file"] = item.File
		}
		if item.Source != "" {
			renderedItem["source"] = item.Source
		}
		if item.TopicPath != "" {
			renderedItem["topic_path"] = item.TopicPath
		}
		if item.Timestamp != "" {
			renderedItem["timestamp"] = item.Timestamp
		}
		rendered = append(rendered, renderedItem)
	}
	omittedRefs := renderOmittedHighValueRefs(omitted, 8)
	tokenReport := contextPackTokenBudgetReport(tokenBudget, usedTokens, len(rendered), len(omittedRefs), compressionLevel)
	return contextPackEvidenceAllocation{
		RankedEvidence:       rendered,
		OmittedHighValueRefs: omittedRefs,
		TokenBudget:          tokenReport,
		UsedTokensEstimate:   usedTokens,
		CompressionLevel:     compressionLevel,
	}
}

func allocateContextPackEvidence(items []contextPackEvidenceItem, tokenBudget contextPackTokenBudget) ([]contextPackEvidenceItem, []contextPackEvidenceItem, int, string) {
	if !tokenBudget.Active || tokenBudget.RankedEvidenceTokens <= 0 {
		limit := minInt(len(items), 16)
		selected := append([]contextPackEvidenceItem{}, items[:limit]...)
		used := 0
		for _, item := range selected {
			used += item.EstimatedTokens
		}
		return selected, []contextPackEvidenceItem{}, used, "none"
	}

	budget := maxInt(96, tokenBudget.RankedEvidenceTokens)
	selected := []contextPackEvidenceItem{}
	omitted := []contextPackEvidenceItem{}
	remaining := append([]contextPackEvidenceItem{}, items...)
	selectedDiversity := map[string]int{}
	used := 0
	compressionLevel := "none"

	isProtected := func(item contextPackEvidenceItem) bool {
		if (item.Kind == "risk" || item.Kind == "check" || item.Kind == "decision") && item.Score >= 80 {
			return true
		}
		for _, signal := range item.WhySelected {
			if anyToString(signal) == "risk_or_contradiction_signal" {
				return true
			}
		}
		return false
	}
	addSelected := func(item contextPackEvidenceItem) {
		selected = append(selected, item)
		used += item.EstimatedTokens
		if item.DiversityKey != "" {
			selectedDiversity[item.DiversityKey]++
		}
	}
	tryFit := func(item contextPackEvidenceItem) (contextPackEvidenceItem, bool) {
		if used+item.EstimatedTokens <= budget {
			return item, true
		}
		available := budget - used
		if available < 40 {
			return item, false
		}
		clipped := item
		clipped.Text = clipText(item.Text, maxInt(80, (available-16)*4))
		clipped.EstimatedTokens = contextPackEstimateTokens(clipped.Text + " " + clipped.Reason + " " + clipped.Project + " " + clipped.File + " " + clipped.Source + " " + clipped.TopicPath)
		clipped.ValueDensity = roundFloat(clipped.Score/float64(maxInt(clipped.EstimatedTokens, 1)), 4)
		clipped.WhySelected = append(append([]any{}, clipped.WhySelected...), "compressed_to_fit_budget")
		if used+clipped.EstimatedTokens <= budget {
			compressionLevel = "compact"
			return clipped, true
		}
		return item, false
	}

	filtered := remaining[:0]
	for _, item := range remaining {
		if !isProtected(item) {
			filtered = append(filtered, item)
			continue
		}
		if fitted, ok := tryFit(item); ok {
			addSelected(fitted)
		} else {
			omitted = append(omitted, item)
		}
	}
	remaining = filtered

	for len(remaining) > 0 && len(selected) < 16 {
		bestIdx := -1
		bestAdjusted := -1.0
		bestRawScore := -1.0
		for idx, item := range remaining {
			adjusted := item.ValueDensity
			if item.DiversityKey != "" && selectedDiversity[item.DiversityKey] > 0 {
				adjusted *= 0.55
			}
			if adjusted > bestAdjusted || (adjusted == bestAdjusted && item.Score > bestRawScore) {
				bestIdx = idx
				bestAdjusted = adjusted
				bestRawScore = item.Score
			}
		}
		if bestIdx < 0 {
			break
		}
		item := remaining[bestIdx]
		remaining = append(remaining[:bestIdx], remaining[bestIdx+1:]...)
		if fitted, ok := tryFit(item); ok {
			addSelected(fitted)
			continue
		}
		omitted = append(omitted, item)
		if budget-used < 40 {
			break
		}
	}
	omitted = append(omitted, remaining...)

	sort.SliceStable(selected, func(i, j int) bool {
		if selected[i].Score == selected[j].Score {
			return selected[i].Kind < selected[j].Kind
		}
		return selected[i].Score > selected[j].Score
	})
	sort.SliceStable(omitted, func(i, j int) bool {
		if omitted[i].Score == omitted[j].Score {
			return omitted[i].EstimatedTokens > omitted[j].EstimatedTokens
		}
		return omitted[i].Score > omitted[j].Score
	})
	if len(omitted) > 0 && compressionLevel == "none" {
		compressionLevel = "selective_omission"
	}
	return selected, omitted, used, compressionLevel
}

func renderOmittedHighValueRefs(items []contextPackEvidenceItem, limit int) []any {
	out := []any{}
	for _, item := range items {
		if len(out) >= limit {
			break
		}
		if item.Score < 70 && len(out) > 0 {
			continue
		}
		row := map[string]any{
			"kind":             item.Kind,
			"score":            roundFloat(item.Score, 3),
			"impact_score":     roundFloat(item.ImpactScore, 3),
			"estimated_tokens": item.EstimatedTokens,
			"reason":           item.Reason,
			"omitted_reason":   "budget_or_diversity_limit",
			"summary":          clipText(item.Text, 220),
		}
		if item.Project != "" {
			row["project"] = item.Project
		}
		if item.File != "" {
			row["file"] = item.File
		}
		if item.Source != "" {
			row["source"] = item.Source
		}
		if item.TopicPath != "" {
			row["topic_path"] = item.TopicPath
		}
		out = append(out, row)
	}
	return out
}

func contextPackTokenBudgetReport(tokenBudget contextPackTokenBudget, usedTokens int, selectedCount int, omittedCount int, compressionLevel string) map[string]any {
	return map[string]any{
		"schema_id":                     "contextlattice_context_token_budget.v1",
		"active":                        tokenBudget.Active,
		"estimate_method":               firstNonEmptyStrings(tokenBudget.EstimateMethod, "chars_div_4"),
		"selection_strategy":            "impact_per_estimated_token_with_provenance_diversity",
		"agent_context_budget_tokens":   tokenBudget.AgentContextBudgetTokens,
		"model_context_window_tokens":   tokenBudget.ModelContextWindowTokens,
		"reserved_response_tokens":      tokenBudget.ReservedResponseTokens,
		"already_loaded_tokens":         tokenBudget.AlreadyLoadedTokens,
		"target_context_pack_tokens":    tokenBudget.TargetContextPackTokens,
		"ranked_evidence_budget_tokens": tokenBudget.RankedEvidenceTokens,
		"used_tokens_estimate":          usedTokens,
		"selected_count":                selectedCount,
		"omitted_high_value_count":      omittedCount,
		"compression_level":             compressionLevel,
	}
}

func buildContextPackTokenImpact(query string, contextPack map[string]any, compiled map[string]any, referencePrompt string) map[string]any {
	compiler := anyMap(compiled["context_compiler"])
	tokenBudget := anyMap(compiled["token_budget"])
	sourceCoverage := anyMap(contextPack["sourceCoverage"])
	if len(sourceCoverage) == 0 {
		sourceCoverage = anyMap(contextPack["source_coverage"])
	}

	baselinePayload := map[string]any{
		"query":                   query,
		"facts":                   contextPackAnyList(contextPack["facts"]),
		"numeric_facts":           contextPackAnyList(contextPack["numeric_facts"]),
		"results":                 contextPackAnyList(contextPack["results"]),
		"relevant_decisions":      contextPackAnyList(contextPack["relevant_decisions"]),
		"known_failure_modes":     contextPackAnyList(contextPack["known_failure_modes"]),
		"acceptance_criteria":     contextPackAnyList(contextPack["acceptance_criteria"]),
		"runbooks":                contextPackAnyList(contextPack["runbooks"]),
		"capabilities_to_use":     contextPackAnyList(contextPack["capabilities_to_use"]),
		"commands":                contextPackAnyList(contextPack["commands"]),
		"citations":               contextPackAnyList(contextPack["citations"]),
		"graph_neighbors":         contextPackAnyList(contextPack["graph_neighbors"]),
		"topic_rollup":            contextPack["topic_rollup"],
		"source_coverage":         sourceCoverage,
		"agent_guidance":          compiled["agent_guidance"],
		"omitted_high_value_refs": contextPackAnyList(compiled["omitted_high_value_refs"]),
	}
	baselineTokens := contextPackEstimateAnyTokens(baselinePayload)
	packedTokens := contextPackEstimateTokens(referencePrompt)
	if used := anyToInt(tokenBudget["used_tokens_estimate"], 0); used > 0 {
		packedTokens = maxInt(packedTokens, used+contextPackEstimateTokens(query)+600)
	}
	if packedTokens <= 0 {
		packedTokens = 1
	}
	if baselineTokens < packedTokens {
		baselineTokens = packedTokens
	}
	savedTokens := maxInt(0, baselineTokens-packedTokens)
	ratio := 1.0
	if packedTokens > 0 {
		ratio = roundFloat(float64(baselineTokens)/float64(packedTokens), 2)
	}

	selectedCount := anyToInt(compiler["ranked_evidence_count"], len(contextPackAnyList(compiled["ranked_evidence"])))
	omittedCount := anyToInt(compiler["omitted_high_value_ref_count"], len(contextPackAnyList(compiled["omitted_high_value_refs"])))
	returnedSourceCount := len(anyToStringList(sourceCoverage["returned"], 64))
	confidence := "medium"
	if savedTokens > 0 && selectedCount > 0 && returnedSourceCount > 0 {
		confidence = "high"
	}
	if selectedCount == 0 || baselineTokens == packedTokens {
		confidence = "low"
	}

	return map[string]any{
		"schema_id":                "contextlattice_token_impact.v1",
		"version":                  1,
		"scope":                    "context_pack_response",
		"calibration_grade":        "sampled_pack_estimate",
		"confidence":               confidence,
		"estimate_method":          "chars_div_4",
		"baseline_kind":            "raw_candidate_evidence_prompt_stuffing",
		"packed_kind":              "compiled_ranked_evidence_reference_prompt",
		"baseline_tokens_estimate": baselineTokens,
		"packed_tokens_estimate":   packedTokens,
		"saved_tokens_estimate":    savedTokens,
		"compression_ratio":        ratio,
		"sample_count":             1,
		"selected_evidence_count":  selectedCount,
		"omitted_high_value_count": omittedCount,
		"returned_source_count":    returnedSourceCount,
		"token_budget_active":      anyToBool(tokenBudget["active"]),
		"token_budget_target":      anyToInt(tokenBudget["target_context_pack_tokens"], 0),
		"ranked_evidence_tokens":   anyToInt(tokenBudget["used_tokens_estimate"], anyToInt(compiler["ranked_evidence_tokens_estimate"], 0)),
		"selection_strategy":       firstNonEmptyStrings(anyToString(tokenBudget["selection_strategy"]), anyToString(compiler["strategy"])),
		"moat_claim":               "ContextLattice converts raw candidate memory into a bounded, provenance-carrying prompt packet and reports the estimated token delta per pack.",
		"measurement_limit":        "Estimate uses chars_div_4 until model-tokenizer accounting is wired.",
		"basis": []any{
			"raw candidate evidence JSON",
			"compiled reference_prompt",
			"ranked evidence token budget",
			"source coverage",
			"omitted high-value references",
		},
	}
}

func contextPackEstimateAnyTokens(value any) int {
	raw, err := json.Marshal(value)
	if err != nil {
		return 1
	}
	return contextPackEstimateTokens(string(raw))
}

func contextPackPromptSections(
	query string,
	contextPack map[string]any,
	sourceCoverage map[string]any,
	objectiveCtx objectiveContext,
	rankedEvidence []any,
	tokenBudget map[string]any,
	omittedHighValueRefs []any,
	agentGuidance map[string]any,
) map[string]any {
	files := anyToStringList(contextPack["files_to_read"], 12)
	commands := contextPackTextList(contextPack["commands"], 8)
	checks := contextPackTextList(contextPack["acceptance_criteria"], 8)
	risks := contextPackTextList(contextPack["known_failure_modes"], 8)
	capabilities := contextPackTextList(contextPack["capabilities_to_use"], 8)
	nextAction := "Use the ranked evidence, inspect cited files when necessary, then execute the smallest verifiable step."
	objective := strings.TrimSpace(query)
	if !objectiveCtx.empty() && strings.TrimSpace(objectiveCtx.Objective) != "" {
		objective = objectiveCtx.Objective
	}
	hierarchy := objectiveCtx.hierarchy(
		anyToString(contextPack["project"]),
		anyToString(contextPack["topic_path"]),
		"",
		query,
	)
	lineage := objectiveCtx.lineage(
		anyToString(contextPack["project"]),
		anyToString(contextPack["topic_path"]),
		"",
		query,
	)
	return map[string]any{
		"objective":               clipText(objective, 900),
		"task":                    clipText(query, 900),
		"mission":                 clipText(objectiveCtx.Mission, 900),
		"goal":                    clipText(objectiveCtx.Goal, 900),
		"objective_hierarchy":     hierarchy,
		"objective_lineage":       lineage,
		"next_action":             clipText(nextAction, 900),
		"evidence":                rankedEvidence,
		"token_budget":            tokenBudget,
		"omitted_high_value_refs": omittedHighValueRefs,
		"agent_guidance":          agentGuidance,
		"files_to_inspect":        files,
		"commands":                commands,
		"checks":                  checks,
		"risks":                   risks,
		"capabilities":            capabilities,
		"source_coverage": map[string]any{
			"returned": anyToStringList(sourceCoverage["returned"], 16),
			"pending":  anyToStringList(sourceCoverage["pending"], 16),
			"failed":   anyToStringList(sourceCoverage["failed"], 8),
			"complete": anyToBool(sourceCoverage["complete"]),
		},
		"constraints": []any{
			"Prefer cited evidence over inference.",
			"Inspect files before claiming current code behavior.",
			"Do not include raw logs, volatile telemetry, secrets, or oversized provider errors.",
			"If omitted_high_value_refs is non-empty, treat it as frontier context and retrieve before relying on it.",
			"Keep the next model call focused on the requested task and acceptance checks.",
		},
	}
}

func contextPackReferencePrompt(promptSections map[string]any) string {
	lines := []string{
		"Use this ContextLattice compiled context package as the factual packet for the next reasoning step.",
		"Objective: " + anyToString(promptSections["objective"]),
		"Task: " + anyToString(promptSections["task"]),
	}
	if mission := strings.TrimSpace(anyToString(promptSections["mission"])); mission != "" {
		lines = append(lines, "Mission: "+mission)
	}
	if goal := strings.TrimSpace(anyToString(promptSections["goal"])); goal != "" {
		lines = append(lines, "Goal: "+goal)
	}
	hierarchy := anyMap(promptSections["objective_hierarchy"])
	projectObjective := anyToString(anyMap(hierarchy["project"])["primary_objective"])
	topicObjective := anyToString(anyMap(hierarchy["topic"])["objective"])
	sessionObjective := anyToString(anyMap(hierarchy["session"])["objective"])
	if strings.TrimSpace(projectObjective+topicObjective+sessionObjective) != "" {
		lines = append(lines, "Project primary objective: "+projectObjective)
		lines = append(lines, "Topic objective: "+topicObjective)
		lines = append(lines, "Session objective: "+sessionObjective)
	}
	if subobjectives := anyToStringList(hierarchy["subobjectives"], 8); len(subobjectives) > 0 {
		lines = append(lines, "Subobjectives: "+strings.Join(subobjectives, "; "))
	}
	lines = append(lines, "Next action: "+anyToString(promptSections["next_action"]))
	if budget := anyMap(promptSections["token_budget"]); len(budget) > 0 && anyToBool(budget["active"]) {
		lines = append(lines, "Context budget: selected "+anyToString(budget["selected_count"])+" ranked evidence items using ~"+anyToString(budget["used_tokens_estimate"])+"/"+anyToString(budget["ranked_evidence_budget_tokens"])+" estimated ranked-evidence tokens; compression="+anyToString(budget["compression_level"])+".")
	}
	lines = append(lines, "", "Ranked evidence:")
	evidence := contextPackAnyList(promptSections["evidence"])
	if len(evidence) == 0 {
		lines = append(lines, "- No ranked evidence returned; retrieve or inspect before making claims.")
	}
	for idx, item := range evidence {
		if idx >= 10 {
			break
		}
		entry := anyMap(item)
		citation := contextPackEvidenceCitation(entry)
		line := "- [" + anyToString(entry["kind"]) + " #" + anyToString(entry["rank"]) + "] " + anyToString(entry["text"])
		if citation != "" {
			line += " (" + citation + ")"
		}
		lines = append(lines, line)
	}
	if omitted := contextPackAnyList(promptSections["omitted_high_value_refs"]); len(omitted) > 0 {
		lines = append(lines, "", "Omitted high-value refs:")
		for idx, item := range omitted {
			if idx >= 5 {
				break
			}
			entry := anyMap(item)
			citation := contextPackEvidenceCitation(entry)
			line := "- [" + anyToString(entry["kind"]) + "] " + firstNonEmptyStrings(anyToString(entry["summary"]), anyToString(entry["text"]))
			if citation != "" {
				line += " (" + citation + ")"
			}
			lines = append(lines, line)
		}
		lines = append(lines, "Retrieve omitted refs before making claims based on them.")
	}
	if files := anyToStringList(promptSections["files_to_inspect"], 8); len(files) > 0 {
		lines = append(lines, "", "Files to inspect:")
		for _, fileName := range files {
			lines = append(lines, "- "+fileName)
		}
	}
	if checks := anyToStringList(promptSections["checks"], 8); len(checks) > 0 {
		lines = append(lines, "", "Acceptance checks:")
		for _, check := range checks {
			lines = append(lines, "- "+check)
		}
	}
	if risks := anyToStringList(promptSections["risks"], 6); len(risks) > 0 {
		lines = append(lines, "", "Known risks:")
		for _, risk := range risks {
			lines = append(lines, "- "+risk)
		}
	}
	if guidance := anyMap(promptSections["agent_guidance"]); len(guidance) > 0 {
		if hints := anyToStringList(guidance["prompt_hints"], 6); len(hints) > 0 {
			lines = append(lines, "", "Agent guidance hints:")
			for _, hint := range hints {
				lines = append(lines, "- "+hint)
			}
		}
	}
	lines = append(lines, "", "Rules: cite evidence, inspect current files for code claims, avoid raw logs/volatile telemetry, and keep output bounded.")
	return clipText(strings.Join(lines, "\n"), 5000)
}

func contextPackEvidenceCitation(item map[string]any) string {
	parts := []string{}
	for _, key := range []string{"project", "file", "source", "topic_path"} {
		value := strings.TrimSpace(anyToString(item[key]))
		if value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, " / ")
}

func contextPackTextList(value any, maxItems int) []string {
	out := []string{}
	for _, item := range contextPackAnyList(value) {
		text := strings.TrimSpace(anyToString(item))
		if text == "" {
			text = strings.TrimSpace(anyToString(anyMap(item)["text"]))
		}
		if text == "" {
			continue
		}
		out = append(out, clipText(text, 360))
		if len(out) >= maxItems {
			break
		}
	}
	return out
}

func contextPackAnyList(value any) []any {
	items, ok := asAnySlice(value)
	if !ok {
		return []any{}
	}
	return items
}

func buildContextPackPayload(
	query string,
	searchResponse map[string]any,
	maxFacts int,
	maxResults int,
) map[string]any {
	results := parseRows(searchResponse["results"])
	grounding, _ := searchResponse["grounding"].(map[string]any)
	if grounding == nil {
		grounding = map[string]any{
			"facts":               []any{},
			"numeric_facts":       []any{},
			"strict_numeric_copy": true,
		}
	}
	factsAny, _ := grounding["facts"].([]any)
	numericFactsAny, _ := grounding["numeric_facts"].([]any)
	if factsAny == nil {
		factsAny = []any{}
	}
	if numericFactsAny == nil {
		numericFactsAny = []any{}
	}
	maxFacts = clampInt(maxFacts, 1, 100)
	maxResults = clampInt(maxResults, 1, 100)
	if len(factsAny) > maxFacts {
		factsAny = factsAny[:maxFacts]
	}
	if len(numericFactsAny) > maxFacts {
		numericFactsAny = numericFactsAny[:maxFacts]
	}

	citations := contextPackCitations(factsAny, results)
	resultRows := make([]map[string]any, 0, minInt(len(results), maxResults))
	for idx, row := range results {
		if idx >= maxResults {
			break
		}
		rendered := map[string]any{
			"project":    row["project"],
			"file":       row["file"],
			"source":     row["source"],
			"score":      anyToFloat(row["score"]),
			"topic_path": row["topic_path"],
			"timestamp":  contextPackTimestamp(row),
			"summary":    clipText(anyToString(row["summary"]), 480),
		}
		if lifecycle := strings.TrimSpace(anyToString(row["lifecycle"])); lifecycle != "" {
			rendered["lifecycle"] = normalizeMemoryLifecycle(lifecycle)
		}
		if topicRollup, ok := row["topic_rollup"].(map[string]any); ok {
			rendered["topic_rollup"] = contextPackTopicRollup(topicRollup)
		}
		resultRows = append(resultRows, rendered)
	}
	sections := contextPackAgentSections(factsAny, resultRows)
	generatedAt := nowUTCISO()

	return map[string]any{
		"query":               query,
		"generatedAt":         generatedAt,
		"generated_at":        generatedAt,
		"factualOnly":         true,
		"factual_only":        true,
		"strictNumericCopy":   anyToBoolOrDefault(grounding["strict_numeric_copy"], true),
		"strict_numeric_copy": anyToBoolOrDefault(grounding["strict_numeric_copy"], true),
		"facts":               factsAny,
		"numericFacts":        numericFactsAny,
		"numeric_facts":       numericFactsAny,
		"citations":           citations,
		"results":             resultRows,
		"relevantDecisions":   sections["relevantDecisions"],
		"relevant_decisions":  sections["relevantDecisions"],
		"filesToRead":         sections["filesToRead"],
		"files_to_read":       sections["filesToRead"],
		"filesToAvoid":        sections["filesToAvoid"],
		"files_to_avoid":      sections["filesToAvoid"],
		"capabilitiesToUse":   sections["capabilitiesToUse"],
		"capabilities_to_use": sections["capabilitiesToUse"],
		"runbooks":            sections["runbooks"],
		"knownFailureModes":   sections["knownFailureModes"],
		"known_failure_modes": sections["knownFailureModes"],
		"commands":            sections["commands"],
		"acceptanceCriteria":  sections["acceptanceCriteria"],
		"acceptance_criteria": sections["acceptanceCriteria"],
		"warnings":            parseWarnings(searchResponse["warnings"]),
		"retrievalMode":       searchResponse["retrieval_mode"],
		"retrieval_mode":      searchResponse["retrieval_mode"],
		"retrievalIntent":     searchResponse["retrieval_intent"],
		"retrieval_intent":    searchResponse["retrieval_intent"],
		"agentId":             searchResponse["agent_id"],
		"agent_id":            searchResponse["agent_id"],
	}
}

func contextPackSourceCoverage(searchResponse map[string]any) map[string]any {
	summary := anyMap(searchResponse["source_summary"])
	debug := anyMap(searchResponse["retrieval_debug"])
	staged := anyMap(debug["staged_fetch"])
	sourceCounts := anyMap(debug["source_counts"])
	sourceOwners := anyMap(summary["source_owners"])
	if len(sourceOwners) == 0 {
		sourceOwners = anyMap(debug["source_owners"])
	}
	configured := anyToStringList(summary["sources"], 100)
	if len(configured) == 0 {
		configured = anyToStringList(debug["sources"], 100)
	}
	returned := anyToStringList(summary["returned_now"], 100)
	pending := anyToStringList(summary["pending_sources"], 100)
	warming := anyToStringList(summary["warming_sources"], 100)
	timedOut := anyToStringList(summary["timed_out_sources"], 100)
	failed := anyToStringList(summary["failed_sources"], 100)
	budgetExceeded := anyToStringList(summary["budget_exceeded_sources"], 100)
	skipped := anyToStringList(summary["skipped_sources"], 100)
	unavailable := anyToStringList(summary["continuation_unavailable_sources"], 100)
	queriedSet := map[string]struct{}{}
	for _, list := range [][]string{configured, returned, pending, warming, timedOut, failed, budgetExceeded, skipped, unavailable} {
		for _, source := range list {
			queriedSet[source] = struct{}{}
		}
	}
	rowCounts := map[string]int{}
	for source, count := range sourceCounts {
		normalized := strings.TrimSpace(strings.ToLower(source))
		if normalized == "" {
			continue
		}
		rowCounts[normalized] = anyToInt(count, 0)
		queriedSet[normalized] = struct{}{}
	}
	complete := len(pending) == 0 && len(warming) == 0 && len(timedOut) == 0 && len(failed) == 0 && len(budgetExceeded) == 0 && len(skipped) == 0 && len(unavailable) == 0
	return map[string]any{
		"configured":                     configured,
		"queried":                        mapKeysSorted(queriedSet),
		"returned":                       returned,
		"pending":                        pending,
		"warming":                        warming,
		"timed_out":                      timedOut,
		"failed":                         failed,
		"budget_exceeded":                budgetExceeded,
		"skipped":                        skipped,
		"continuation_unavailable":       unavailable,
		"row_counts":                     rowCounts,
		"source_owners":                  sourceOwners,
		"complete":                       complete,
		"blocking_slow_sources":          anyToBool(anyMap(debug["source_policy"])["blocking_slow_sources"]),
		"sync_fallback_slow_sources":     anyToStringList(staged["sync_fallback_slow_sources"], 100),
		"async_warm_slow_sources":        anyToStringList(staged["async_warm_slow_sources"], 100),
		"fail_open_continuation_sources": anyToStringList(staged["fail_open_continuation_sources"], 100),
		"effective_timeout_secs":         anyMap(staged["effective_timeout_secs"]),
		"continuation_durable":           summary["continuation_durable"],
		"retrieval_lifecycle":            searchResponse["retrieval_lifecycle"],
	}
}

func contextPackSourceCoverageWithGraph(sourceCoverage map[string]any, graphQuality map[string]any) map[string]any {
	if !anyToBool(graphQuality["used"]) {
		return sourceCoverage
	}
	coverage := cloneAnyMap(sourceCoverage)
	addSource := func(key string) {
		values := anyToStringList(coverage[key], 100)
		for _, value := range values {
			if strings.EqualFold(value, memoryEdgeSource) {
				coverage[key] = values
				return
			}
		}
		values = append(values, memoryEdgeSource)
		sort.Strings(values)
		coverage[key] = values
	}
	addSource("returned")
	addSource("queried")
	rowCounts := cloneAnyMap(anyMap(coverage["row_counts"]))
	signals := anyMap(graphQuality["signals"])
	rowCounts[memoryEdgeSource] = anyToInt(signals["added_evidence_count"], 0)
	coverage["row_counts"] = rowCounts
	sourceOwners := cloneAnyMap(anyMap(coverage["source_owners"]))
	sourceOwners[memoryEdgeSource] = sourceOwnerGoNative
	coverage["source_owners"] = sourceOwners
	return coverage
}

func contextPackAgentSections(facts []any, results []map[string]any) map[string]any {
	sections := map[string][]any{
		"relevantDecisions":  {},
		"filesToRead":        {},
		"filesToAvoid":       {},
		"capabilitiesToUse":  {},
		"runbooks":           {},
		"knownFailureModes":  {},
		"commands":           {},
		"acceptanceCriteria": {},
	}
	seenFiles := map[string]struct{}{}
	addFile := func(fileName string) {
		fileName = strings.TrimSpace(fileName)
		if fileName == "" {
			return
		}
		if _, ok := seenFiles[fileName]; ok {
			return
		}
		seenFiles[fileName] = struct{}{}
		sections["filesToRead"] = append(sections["filesToRead"], fileName)
	}
	addText := func(section string, text string, source map[string]any) {
		text = clipText(strings.TrimSpace(text), 360)
		if text == "" {
			return
		}
		item := map[string]any{"text": text}
		for _, key := range []string{"project", "file", "source", "topic_path", "timestamp"} {
			if value, ok := source[key]; ok && strings.TrimSpace(anyToString(value)) != "" {
				item[key] = value
			}
		}
		sections[section] = append(sections[section], item)
	}
	classify := func(text string, source map[string]any) {
		lower := strings.ToLower(text)
		fileName := strings.TrimSpace(anyToString(source["file"]))
		topicPath := strings.TrimSpace(strings.ToLower(anyToString(source["topic_path"])))
		addFile(fileName)
		if strings.Contains(topicPath, "runbook") || strings.Contains(strings.ToLower(fileName), "runbook") {
			addText("runbooks", text, source)
		}
		if containsAny(lower, []string{"decision", "decided", "choose", "chosen", "policy", "contract"}) {
			addText("relevantDecisions", text, source)
		}
		if containsAny(lower, []string{"fail", "failure", "timeout", "blocked", "blocker", "regression", "risk", "vulnerab"}) {
			addText("knownFailureModes", text, source)
		}
		if containsAny(lower, []string{"verify", "test", "check", "acceptance", "must pass", "criteria"}) {
			addText("acceptanceCriteria", text, source)
		}
		if containsAny(lower, []string{"script", "hook", "contextlattice", "capability", "skill"}) {
			addText("capabilitiesToUse", text, source)
		}
		for _, command := range extractBacktickCommands(text, 6) {
			addText("commands", command, source)
		}
	}
	for _, factAny := range facts {
		fact, ok := factAny.(map[string]any)
		if !ok {
			continue
		}
		classify(anyToString(fact["text"]), fact)
	}
	for _, row := range results {
		classify(anyToString(row["summary"]), row)
	}
	for key, values := range sections {
		if len(values) > 20 {
			values = values[:20]
		}
		sections[key] = values
	}
	return map[string]any{
		"relevantDecisions":  sections["relevantDecisions"],
		"filesToRead":        sections["filesToRead"],
		"filesToAvoid":       sections["filesToAvoid"],
		"capabilitiesToUse":  sections["capabilitiesToUse"],
		"runbooks":           sections["runbooks"],
		"knownFailureModes":  sections["knownFailureModes"],
		"commands":           sections["commands"],
		"acceptanceCriteria": sections["acceptanceCriteria"],
	}
}

func containsAny(text string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func extractBacktickCommands(text string, limit int) []string {
	out := []string{}
	parts := strings.Split(text, "`")
	for idx := 1; idx < len(parts); idx += 2 {
		candidate := strings.TrimSpace(parts[idx])
		if candidate == "" {
			continue
		}
		if strings.Contains(candidate, " ") || strings.Contains(candidate, "/") || strings.Contains(candidate, "-") {
			out = append(out, candidate)
		}
		if len(out) >= limit {
			break
		}
	}
	return out
}

func contextPackTopicRollup(topicRollup map[string]any) map[string]any {
	rawRefs := anyToStringList(topicRollup["raw_refs"], 50)
	filePartitions := []map[string]any{}
	if rawPartitions, ok := topicRollup["file_partitions"].([]any); ok {
		for _, item := range rawPartitions {
			partition, ok := item.(map[string]any)
			if !ok {
				continue
			}
			filePartitions = append(filePartitions, map[string]any{
				"topic_path":   strings.TrimSpace(anyToString(partition["topic_path"])),
				"file_count":   anyToInt(partition["file_count"], 0),
				"sample_files": anyToStringList(partition["sample_files"], 50),
			})
		}
	}
	return map[string]any{
		"event_count":        anyToInt(topicRollup["event_count"], 0),
		"recent_event_count": anyToInt(topicRollup["recent_event_count"], 0),
		"unique_file_count":  anyToInt(topicRollup["unique_file_count"], 0),
		"latest_timestamp":   topicRollup["latest_timestamp"],
		"raw_refs":           rawRefs,
		"file_partitions":    filePartitions,
	}
}

func contextPackCitations(facts []any, results []map[string]any) []map[string]any {
	citations := []map[string]any{}
	seen := map[string]struct{}{}
	appendCitation := func(project string, fileName string, source string, topicPath any, timestamp any) {
		project = strings.TrimSpace(project)
		fileName = strings.TrimSpace(fileName)
		source = strings.TrimSpace(source)
		if project == "" && fileName == "" && source == "" {
			return
		}
		key := project + "|" + fileName + "|" + source + "|" + strings.TrimSpace(anyToString(timestamp))
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		citations = append(citations, map[string]any{
			"project":    nilIfEmpty(project),
			"file":       nilIfEmpty(fileName),
			"source":     nilIfEmpty(source),
			"topic_path": topicPath,
			"timestamp":  timestamp,
		})
	}

	for _, item := range facts {
		fact, ok := item.(map[string]any)
		if !ok {
			continue
		}
		project := strings.TrimSpace(anyToString(fact["project"]))
		fileName := strings.TrimSpace(anyToString(fact["file"]))
		source := strings.TrimSpace(anyToString(fact["source"]))
		topicPath := fact["topic_path"]
		timestamp := fact["timestamp"]
		if sourceMap, ok := fact["source"].(map[string]any); ok {
			if project == "" {
				project = strings.TrimSpace(anyToString(sourceMap["project"]))
			}
			if fileName == "" {
				fileName = strings.TrimSpace(anyToString(sourceMap["file"]))
			}
			if source == "" {
				source = strings.TrimSpace(anyToString(sourceMap["source"]))
			}
			if topicPath == nil {
				topicPath = sourceMap["topic_path"]
			}
			if timestamp == nil {
				timestamp = sourceMap["timestamp"]
			}
		}
		appendCitation(project, fileName, source, topicPath, timestamp)
	}

	for _, row := range results {
		project := strings.TrimSpace(anyToString(row["project"]))
		fileName := strings.TrimSpace(anyToString(row["file"]))
		source := strings.TrimSpace(anyToString(row["source"]))
		appendCitation(project, fileName, source, row["topic_path"], contextPackTimestamp(row))
		if topicRollup, ok := row["topic_rollup"].(map[string]any); ok {
			for _, rawFile := range anyToStringList(topicRollup["raw_refs"], 50) {
				appendCitation(project, rawFile, "topic_rollup_raw_ref", row["topic_path"], topicRollup["latest_timestamp"])
			}
			if partitions, ok := topicRollup["file_partitions"].([]any); ok {
				for _, item := range partitions {
					partition, ok := item.(map[string]any)
					if !ok {
						continue
					}
					partitionPath := strings.TrimSpace(anyToString(partition["topic_path"]))
					for _, sampleFile := range anyToStringList(partition["sample_files"], 50) {
						appendCitation(project, sampleFile, "topic_rollup_partition_ref", nilIfEmpty(partitionPath), topicRollup["latest_timestamp"])
					}
				}
			}
		}
	}

	sort.SliceStable(citations, func(i, j int) bool {
		left := strings.TrimSpace(anyToString(citations[i]["project"])) + "|" + strings.TrimSpace(anyToString(citations[i]["file"]))
		right := strings.TrimSpace(anyToString(citations[j]["project"])) + "|" + strings.TrimSpace(anyToString(citations[j]["file"]))
		return left < right
	})
	return citations
}

func contextPackTimestamp(row map[string]any) any {
	for _, key := range []string{"timestamp", "created_at", "createdAt", "updated_at", "updatedAt"} {
		value := strings.TrimSpace(anyToString(row[key]))
		if value != "" {
			return value
		}
	}
	return nil
}

func anyToFloat(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case json.Number:
		parsed, err := typed.Float64()
		if err == nil {
			return parsed
		}
	}
	return 0
}

func anyToStringList(value any, maxItems int) []string {
	maxItems = maxInt(1, maxItems)
	out := []string{}
	switch typed := value.(type) {
	case []string:
		for _, item := range typed {
			candidate := strings.TrimSpace(item)
			if candidate == "" {
				continue
			}
			out = append(out, candidate)
			if len(out) >= maxItems {
				return out
			}
		}
	case []any:
		for _, item := range typed {
			candidate := strings.TrimSpace(anyToString(item))
			if candidate == "" {
				continue
			}
			out = append(out, candidate)
			if len(out) >= maxItems {
				return out
			}
		}
	}
	return out
}

func anyToBoolOrDefault(value any, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return anyToBool(value)
}

func nilIfEmpty(value string) any {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	return trimmed
}

func minInt(left int, right int) int {
	if left < right {
		return left
	}
	return right
}

func maxInt(left int, right int) int {
	if left > right {
		return left
	}
	return right
}
