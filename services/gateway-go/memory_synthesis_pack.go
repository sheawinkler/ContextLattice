package main

import (
	"context"
	"net/http"
	"sort"
	"strings"
)

func (s *server) memorySynthesisPack(w http.ResponseWriter, r *http.Request) {
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
	response, status, execErr := s.buildSynthesisPackResponse(r.Context(), incomingHeaders, payload, "/memory/synthesis-pack")
	if execErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "synthesis_pack_unavailable", "detail": sanitizeProviderOverflowText(execErr.Error())})
		return
	}
	writeJSON(w, status, response)
}

func (s *server) toolsSynthesisPack(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	incomingHeaders, ok := s.prepareToolHeaders(w, r, "/tools/synthesis_pack")
	if !ok {
		return
	}
	payload, err := readOptionalJSONBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
		return
	}
	payload["_suppress_final_token_impact_recording"] = true
	response, status, execErr := s.buildSynthesisPackResponse(r.Context(), incomingHeaders, payload, "/tools/synthesis_pack")
	if execErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "synthesis_pack_unavailable", "detail": sanitizeProviderOverflowText(execErr.Error())})
		return
	}
	response["tool"] = "synthesis_pack"
	if anyToString(response["schema_id"]) == agentPacketContractID {
		response["surface"] = "tools_synthesis_pack"
		response = finalizeAgentPacket(response)
	} else {
		attach := func(value map[string]any) map[string]any {
			return attachPayloadFormatContract(synthesisPackContractID, value, anyToString(value["agent_id"]), "synthesis_pack", "/tools/synthesis_pack")
		}
		response = finalizeFullTransport(response, attach, "tools_synthesis_pack_transport", "serialized_tools_synthesis_pack_response_json")
	}
	s.recordTokenImpact(anyMap(response["token_impact"]))
	writeJSON(w, status, response)
}

func (s *server) buildSynthesisPackResponse(
	ctx context.Context,
	incomingHeaders http.Header,
	payload map[string]any,
	surface string,
) (map[string]any, int, error) {
	requestPayload := cloneMap(payload)
	packetRequested := agentPacketRequested(requestPayload)
	if strings.TrimSpace(anyToString(requestPayload["retrieval_intent"])) == "" {
		requestPayload["retrieval_intent"] = "synthesis"
	}
	if _, present := requestPayload["limit"]; !present {
		requestPayload["limit"] = 16
	}
	if _, present := requestPayload["max_facts"]; !present {
		requestPayload["max_facts"] = 32
	}

	contextRequest := cloneMap(requestPayload)
	delete(contextRequest, "output_mode")
	delete(contextRequest, "projection")
	delete(contextRequest, "response_mode")
	contextRequest["_suppress_token_impact_recording"] = true
	contextResponse, status, execErr := s.buildContextPackResponse(ctx, incomingHeaders, contextRequest)
	if execErr != nil {
		return nil, 0, execErr
	}
	if status >= http.StatusBadRequest || !anyToBool(contextResponse["ok"]) || len(anyMap(contextResponse["context_pack"])) == 0 {
		return contextResponse, status, nil
	}

	contextPack := anyMap(contextResponse["context_pack"])
	project := strings.TrimSpace(firstNonEmptyStrings(anyToString(contextResponse["project"]), anyToString(contextPack["project"]), anyToString(requestPayload["project"])))
	topicPath := strings.Trim(strings.TrimSpace(firstNonEmptyStrings(anyToString(contextResponse["topic_path"]), anyToString(contextPack["topic_path"]), anyToString(requestPayload["topic_path"]))), "/")
	query := strings.TrimSpace(firstNonEmptyStrings(anyToString(contextResponse["query"]), anyToString(contextPack["query"]), anyToString(requestPayload["query"])))
	retrievalMode := strings.TrimSpace(firstNonEmptyStrings(anyToString(contextResponse["retrieval_mode"]), anyToString(contextPack["retrieval_mode"])))
	retrievalIntent := strings.TrimSpace(firstNonEmptyStrings(anyToString(contextResponse["retrieval_intent"]), anyToString(contextPack["retrieval_intent"]), "synthesis"))
	agentID := strings.TrimSpace(anyToString(contextResponse["agent_id"]))
	sessionID := strings.TrimSpace(anyToString(contextResponse["session_id"]))

	synthesisPack := s.buildSynthesisPack(ctx, contextResponse, requestPayload, surface)
	referencePrompt := synthesisPackReferencePrompt(synthesisPack)
	response := map[string]any{
		"ok":                   true,
		"schema_id":            synthesisPackContractID,
		"version":              1,
		"project":              project,
		"query":                query,
		"topic_path":           topicPath,
		"retrieval_mode":       retrievalMode,
		"retrieval_intent":     retrievalIntent,
		"synthesis_pack":       synthesisPack,
		"context_pack":         contextPack,
		"context_compiler":     contextResponse["context_compiler"],
		"source_coverage":      contextResponse["source_coverage"],
		"reference_prompt":     referencePrompt,
		"token_impact":         contextResponse["token_impact"],
		"context_pack_quality": contextResponse["context_pack_quality"],
		"run_advisor":          contextResponse["run_advisor"],
		"warnings":             contextResponse["warnings"],
		"writeback_required":   true,
		"agent_id":             agentID,
		"session_id":           sessionID,
	}
	if runtime := anyMap(contextResponse["objective_runtime"]); len(runtime) > 0 {
		response["objective_runtime"] = runtime
		response["objective_hierarchy"] = contextResponse["objective_hierarchy"]
		response["objective_lineage"] = contextResponse["objective_lineage"]
	}
	if agentRuntime := anyMap(contextResponse["agent_runtime"]); len(agentRuntime) > 0 {
		response["agent_runtime"] = agentRuntime
	}
	if sessionID != "" {
		findings := contextPackAnyList(synthesisPack["high_signal_findings"])
		relatedTopics := contextPackAnyList(synthesisPack["topic_gravity"])
		session := s.recordAgentSessionEvent(sessionID, "synthesis_pack.completed", map[string]any{
			"agent_id": agentID,
			"project":  project,
			"summary":  query,
			"metadata": map[string]any{
				"endpoint":               surface,
				"retrieval_mode":         retrievalMode,
				"retrieval_intent":       retrievalIntent,
				"finding_count":          len(findings),
				"related_topic_count":    len(relatedTopics),
				"synthesis_quality":      synthesisPack["synthesis_quality"],
				"source_coverage":        contextResponse["source_coverage"],
				"context_pack_quality":   contextResponse["context_pack_quality"],
				"token_impact":           contextResponse["token_impact"],
				"context_pack_completed": true,
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
	if packetRequested {
		packet := finalizeAgentPacket(buildAgentPacket(response, requestPayload, "synthesis_pack"))
		if !anyToBool(requestPayload["_suppress_final_token_impact_recording"]) {
			s.recordTokenImpact(anyMap(packet["token_impact"]))
		}
		return packet, status, nil
	}
	attach := func(value map[string]any) map[string]any {
		return attachPayloadFormatContract(synthesisPackContractID, value, agentID, "synthesis_pack", surface)
	}
	response = finalizeFullTransport(response, attach, "synthesis_pack_transport", "serialized_synthesis_pack_response_json")
	if !anyToBool(requestPayload["_suppress_final_token_impact_recording"]) {
		s.recordTokenImpact(anyMap(response["token_impact"]))
	}
	return response, status, nil
}

func (s *server) buildSynthesisPack(ctx context.Context, contextResponse map[string]any, requestPayload map[string]any, surface string) map[string]any {
	contextPack := anyMap(contextResponse["context_pack"])
	project := strings.TrimSpace(firstNonEmptyStrings(anyToString(contextResponse["project"]), anyToString(contextPack["project"]), anyToString(requestPayload["project"])))
	topicPath := strings.Trim(strings.TrimSpace(firstNonEmptyStrings(anyToString(contextResponse["topic_path"]), anyToString(contextPack["topic_path"]), anyToString(requestPayload["topic_path"]))), "/")
	query := strings.TrimSpace(firstNonEmptyStrings(anyToString(contextResponse["query"]), anyToString(contextPack["query"]), anyToString(requestPayload["query"])))
	rankedEvidence := contextPackAnyList(contextPack["ranked_evidence"])
	highSignal := synthesisPackEvidenceCards(rankedEvidence, 12)
	topicGravity := s.synthesisPackTopicGravity(ctx, project, topicPath, query, rankedEvidence, requestPayload)
	crossProject := synthesisPackCrossProjectBridges(project, rankedEvidence, contextPackAnyList(contextPack["graph_neighbors"]))
	mustNotForget := synthesisPackMustNotForget(contextPack, rankedEvidence)
	nextActions := synthesisPackNextActions(contextPack, anyMap(contextResponse["run_advisor"]), highSignal)
	openQuestions := synthesisPackOpenQuestions(anyMap(contextResponse["source_coverage"]), contextPackAnyList(contextResponse["omitted_high_value_refs"]), rankedEvidence, topicGravity)
	semanticTags := synthesisPackSemanticTags(query, contextPack, highSignal, topicGravity, crossProject, anyMap(contextResponse["source_coverage"]))
	quality := synthesisPackQuality(highSignal, topicGravity, crossProject, anyMap(contextResponse["source_coverage"]), anyMap(contextResponse["context_pack_quality"]))
	decisionGate := synthesisDecisionGate(highSignal, anyMap(contextResponse["source_coverage"]), quality)
	nextActions = synthesisActionsForDecisionGate(decisionGate, nextActions)
	evidenceTrail := synthesisPackEvidenceTrail(contextPack, highSignal, 12)
	trace := map[string]any{
		"mode":                 "deterministic_v1",
		"llm_used":             false,
		"surface":              surface,
		"basis":                []any{"context_pack.ranked_evidence", "context_pack.graph_neighbors", "memory_store.topic_rollups", "source_coverage", "context_pack_quality", "token_impact"},
		"rust_semantic_tagger": "not_detected_in_repo_inspection",
		"inference_boundary":   "Synthesis Pack groups and scores already-retrieved evidence; it does not claim uncited facts.",
	}
	return map[string]any{
		"schema_id":                synthesisPackContractID,
		"version":                  1,
		"generated_at":             nowUTCISO(),
		"project":                  project,
		"query":                    query,
		"topic_path":               topicPath,
		"summary":                  synthesisPackSummary(highSignal, topicGravity, crossProject, anyMap(contextResponse["source_coverage"])),
		"decision_gate":            decisionGate,
		"high_signal_findings":     highSignal,
		"topic_gravity":            topicGravity,
		"cross_project_bridges":    crossProject,
		"must_not_forget":          mustNotForget,
		"recommended_next_actions": nextActions,
		"open_questions":           openQuestions,
		"semantic_tags":            semanticTags,
		"synthesis_quality":        quality,
		"evidence_trail":           evidenceTrail,
		"synthesis_trace":          trace,
	}
}

func synthesisPackEvidenceCards(items []any, limit int) []any {
	limit = clampInt(limit, 1, 32)
	out := []any{}
	for _, raw := range items {
		if len(out) >= limit {
			break
		}
		item := anyMap(raw)
		text := clipText(strings.TrimSpace(anyToString(item["text"])), 360)
		if text == "" {
			text = clipText(strings.TrimSpace(anyToString(item["summary"])), 360)
		}
		if text == "" {
			continue
		}
		card := map[string]any{
			"rank":            anyToInt(item["rank"], len(out)+1),
			"kind":            strings.TrimSpace(anyToString(item["kind"])),
			"text":            text,
			"score":           roundFloat(anyToFloat(item["score"]), 3),
			"confidence":      anyToFloat(item["confidence"]),
			"query_relevance": anyToFloat(item["query_relevance"]),
			"freshness":       anyToString(item["freshness"]),
			"why_it_matters":  synthesisPackWhyItMatters(item),
			"citation":        contextPackEvidenceCitation(item),
		}
		for _, key := range []string{"project", "file", "source", "topic_path", "timestamp", "relation", "edge_direction"} {
			if value := strings.TrimSpace(anyToString(item[key])); value != "" {
				card[key] = value
			}
		}
		if signals := contextPackAnyList(item["why_selected"]); len(signals) > 0 {
			card["signals"] = signals
		}
		out = append(out, card)
	}
	return out
}

func synthesisDecisionGate(highSignal []any, sourceCoverage map[string]any, quality map[string]any) map[string]any {
	maxAlignment := 0.0
	alignmentTotal := 0.0
	alignmentCount := 0
	for _, raw := range highSignal {
		relevance := anyToFloat(anyMap(raw)["query_relevance"])
		if relevance > maxAlignment {
			maxAlignment = relevance
		}
		alignmentTotal += relevance
		alignmentCount++
	}
	meanAlignment := 0.0
	if alignmentCount > 0 {
		meanAlignment = alignmentTotal / float64(alignmentCount)
	}
	decision := "act"
	reasons := []any{}
	if len(highSignal) == 0 || maxAlignment < 0.15 {
		decision = "abstain"
		reasons = append(reasons, "No selected evidence is sufficiently aligned with the request.")
	} else if !anyToBool(sourceCoverage["complete"]) || maxAlignment < 0.35 || anyToString(quality["status"]) == "sparse" {
		decision = "verify"
		reasons = append(reasons, "Evidence is relevant but incomplete, weakly aligned, or sparse; verify local truth before acting.")
	} else {
		reasons = append(reasons, "Selected evidence is request-aligned and effective source coverage is complete.")
	}
	return map[string]any{
		"decision":                decision,
		"evidence_alignment_max":  roundFloat(maxAlignment, 3),
		"evidence_alignment_mean": roundFloat(meanAlignment, 3),
		"source_complete":         anyToBool(sourceCoverage["complete"]),
		"refusal":                 decision == "abstain",
		"reasons":                 reasons,
		"policy":                  "act only on aligned, provenance-carrying evidence; verify partial truth; abstain from unsupported action",
	}
}

func synthesisActionsForDecisionGate(gate map[string]any, actions []any) []any {
	decision := strings.ToLower(strings.TrimSpace(anyToString(gate["decision"])))
	if decision == "act" {
		return actions
	}
	safe := []any{
		map[string]any{
			"label": "inspect_provenance", "command": "", "reason": "Inspect cited files or source records before changing external state.", "source": "decision_gate",
		},
	}
	if decision == "abstain" {
		safe = append(safe, map[string]any{
			"label": "retrieve_broader_context", "command": "", "reason": "Broaden or deepen retrieval; the current evidence does not support action.", "source": "decision_gate",
		})
		return safe
	}
	safe = append(safe, map[string]any{
		"label": "run_local_verification", "command": "", "reason": "Use deterministic checks to resolve the remaining evidence gap.", "source": "decision_gate",
	})
	return safe
}

func synthesisPackWhyItMatters(item map[string]any) string {
	switch strings.TrimSpace(anyToString(item["kind"])) {
	case "decision":
		return "Durable choice or policy; it should constrain the next move."
	case "risk":
		return "Known failure mode; handling it avoids repeated repair loops."
	case "check":
		return "Verification signal; it turns recall into an executable acceptance gate."
	case "runbook":
		return "Procedure memory; it can shorten the path from context to action."
	case "capability":
		return "Tooling or skill signal; it points at the right execution surface."
	case "graph_neighbor":
		return "Memory graph bridge; it expands the seed context through an explicit edge."
	case "fact":
		return "Grounded fact; cite it before making the claim."
	default:
		return "High-ranked memory result selected by impact-per-token evidence scoring."
	}
}

func (s *server) synthesisPackTopicGravity(ctx context.Context, project string, topicPath string, query string, rankedEvidence []any, requestPayload map[string]any) []any {
	evidenceTopics := synthesisPackEvidenceTopicSet(rankedEvidence)
	rows := []map[string]any{}
	if s.memoryStore != nil && s.memoryStore.policy.enabled {
		includeCold := anyToBool(requestPayload["include_cold"])
		includeEphemeral := requestIncludesEphemeralMemory(requestPayload)
		rollups := s.memoryStore.topicRollupsWithOptions(ctx, project, 1, 128, 0, includeCold, includeEphemeral)
		for _, raw := range contextPackAnyList(rollups["topics"]) {
			row := anyMap(raw)
			path := strings.Trim(strings.TrimSpace(anyToString(row["path"])), "/")
			if path == "" {
				continue
			}
			score, reason := synthesisPackTopicScore(path, query, topicPath, evidenceTopics, row)
			if score <= 0 {
				continue
			}
			rows = append(rows, map[string]any{
				"project":               firstNonEmptyStrings(anyToString(row["project"]), project),
				"topic_path":            path,
				"score":                 score,
				"reason":                reason,
				"event_count":           anyToInt(row["eventCount"], 0),
				"recent_event_count":    anyToInt(row["recentEventCount"], 0),
				"agent_intensity_score": anyToInt(row["agentIntensityScore"], 0),
				"unique_file_count":     anyToInt(row["uniqueFileCount"], 0),
				"latest_timestamp":      row["latestTimestamp"],
				"children":              anyToStringList(row["children"], 12),
			})
		}
	}
	for topic := range evidenceTopics {
		if topic == "" {
			continue
		}
		if synthesisPackTopicRowExists(rows, topic) {
			continue
		}
		rows = append(rows, map[string]any{
			"project":    project,
			"topic_path": topic,
			"score":      45,
			"reason":     "topic_path surfaced directly in ranked evidence",
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		leftScore := anyToInt(rows[i]["score"], 0)
		rightScore := anyToInt(rows[j]["score"], 0)
		if leftScore == rightScore {
			return anyToString(rows[i]["topic_path"]) < anyToString(rows[j]["topic_path"])
		}
		return leftScore > rightScore
	})
	limit := minInt(len(rows), 12)
	out := make([]any, 0, limit)
	for idx := 0; idx < limit; idx++ {
		out = append(out, rows[idx])
	}
	return out
}

func synthesisPackEvidenceTopicSet(items []any) map[string]struct{} {
	out := map[string]struct{}{}
	for _, raw := range items {
		topic := strings.Trim(strings.TrimSpace(anyToString(anyMap(raw)["topic_path"])), "/")
		if topic == "" {
			continue
		}
		for _, prefix := range topicPrefixes(topic) {
			out[prefix] = struct{}{}
		}
	}
	return out
}

func synthesisPackTopicScore(path string, query string, requestedTopic string, evidenceTopics map[string]struct{}, row map[string]any) (int, string) {
	score := minInt(anyToInt(row["agentIntensityScore"], 0), 60)
	if score == 0 {
		score = minInt(anyToInt(row["eventCount"], 0)*4, 40)
	}
	reasons := []string{}
	cleanRequested := strings.Trim(strings.TrimSpace(requestedTopic), "/")
	if cleanRequested != "" && (path == cleanRequested || strings.HasPrefix(path, cleanRequested+"/") || strings.HasPrefix(cleanRequested, path+"/")) {
		score += 35
		reasons = append(reasons, "near requested topic")
	}
	if _, ok := evidenceTopics[path]; ok {
		score += 25
		reasons = append(reasons, "present in ranked evidence")
	}
	queryTokens := synthesisPackQueryTokens(query)
	for _, token := range queryTokens {
		if strings.Contains(strings.ToLower(path), token) {
			score += 6
			reasons = append(reasons, "query token match")
			break
		}
	}
	if anyToInt(row["recentEventCount"], 0) > 0 {
		score += 8
		reasons = append(reasons, "recent writes")
	}
	if anyToInt(row["uniqueFileCount"], 0) > 1 {
		score += 5
		reasons = append(reasons, "multi-file topic")
	}
	if len(reasons) == 0 && score > 0 {
		reasons = append(reasons, "project topic gravity")
	}
	return clampInt(score, 0, 100), strings.Join(dedupeStrings(reasons, 4), "; ")
}

func synthesisPackQueryTokens(query string) []string {
	parts := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
	out := []string{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if len(part) < 4 {
			continue
		}
		out = append(out, part)
		if len(out) >= 8 {
			break
		}
	}
	return out
}

func synthesisPackTopicRowExists(rows []map[string]any, topic string) bool {
	for _, row := range rows {
		if strings.EqualFold(strings.Trim(anyToString(row["topic_path"]), "/"), strings.Trim(topic, "/")) {
			return true
		}
	}
	return false
}

func synthesisPackCrossProjectBridges(project string, rankedEvidence []any, graphNeighbors []any) []any {
	type bridge struct {
		Project string
		Topic   string
		Source  string
		File    string
		Reason  string
		Score   float64
	}
	bridges := []bridge{}
	add := func(raw map[string]any, reason string) {
		otherProject := strings.TrimSpace(anyToString(raw["project"]))
		if otherProject == "" || strings.EqualFold(otherProject, project) {
			return
		}
		bridges = append(bridges, bridge{
			Project: otherProject,
			Topic:   strings.Trim(strings.TrimSpace(anyToString(raw["topic_path"])), "/"),
			Source:  strings.TrimSpace(anyToString(raw["source"])),
			File:    strings.TrimSpace(anyToString(raw["file"])),
			Reason:  reason,
			Score:   anyToFloat(raw["score"]),
		})
	}
	for _, raw := range rankedEvidence {
		add(anyMap(raw), "ranked evidence references another project")
	}
	for _, raw := range graphNeighbors {
		add(anyMap(raw), "memory graph neighbor crosses project boundary")
	}
	sort.SliceStable(bridges, func(i, j int) bool {
		if bridges[i].Score == bridges[j].Score {
			return bridges[i].Project < bridges[j].Project
		}
		return bridges[i].Score > bridges[j].Score
	})
	seen := map[string]struct{}{}
	out := []any{}
	for _, item := range bridges {
		key := strings.ToLower(item.Project + "|" + item.Topic + "|" + item.File + "|" + item.Reason)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, map[string]any{
			"project":    item.Project,
			"topic_path": item.Topic,
			"file":       item.File,
			"source":     item.Source,
			"score":      roundFloat(item.Score, 3),
			"reason":     item.Reason,
		})
		if len(out) >= 8 {
			break
		}
	}
	return out
}

func synthesisPackMustNotForget(contextPack map[string]any, rankedEvidence []any) []any {
	out := []any{}
	for _, raw := range rankedEvidence {
		item := anyMap(raw)
		kind := strings.TrimSpace(anyToString(item["kind"]))
		if kind != "decision" && kind != "risk" && kind != "check" {
			continue
		}
		cards := synthesisPackEvidenceCards([]any{item}, 1)
		if len(cards) == 1 {
			out = append(out, cards[0])
		}
		if len(out) >= 8 {
			return out
		}
	}
	for _, field := range []string{"relevant_decisions", "known_failure_modes", "acceptance_criteria"} {
		for _, raw := range contextPackAnyList(contextPack[field]) {
			item := anyMap(raw)
			text := clipText(strings.TrimSpace(anyToString(item["text"])), 280)
			if text == "" {
				continue
			}
			out = append(out, map[string]any{
				"kind":             synthesisPackKindForSection(field),
				"text":             text,
				"why_it_matters":   synthesisPackSectionWhy(field),
				"citation":         contextPackEvidenceCitation(item),
				"project":          item["project"],
				"file":             item["file"],
				"source":           item["source"],
				"topic_path":       item["topic_path"],
				"dedupe_candidate": true,
			})
			if len(out) >= 8 {
				return out
			}
		}
	}
	return out
}

func synthesisPackKindForSection(field string) string {
	switch field {
	case "relevant_decisions":
		return "decision"
	case "known_failure_modes":
		return "risk"
	case "acceptance_criteria":
		return "check"
	default:
		return "memory"
	}
}

func synthesisPackSectionWhy(field string) string {
	switch field {
	case "relevant_decisions":
		return "Decision memory should shape the next action."
	case "known_failure_modes":
		return "Risk memory should prevent repeated failure."
	case "acceptance_criteria":
		return "Acceptance memory should become a deterministic check."
	default:
		return "Retrieved memory may be useful for the task."
	}
}

func synthesisPackNextActions(contextPack map[string]any, runAdvisor map[string]any, highSignal []any) []any {
	out := []any{}
	for _, raw := range contextPackAnyList(runAdvisor["next_actions"]) {
		item := anyMap(raw)
		label := strings.TrimSpace(anyToString(item["label"]))
		command := strings.TrimSpace(anyToString(item["command"]))
		reason := strings.TrimSpace(anyToString(item["reason"]))
		if label == "" && command == "" {
			continue
		}
		out = append(out, map[string]any{"label": label, "command": clipText(command, 400), "reason": clipText(reason, 360), "source": "run_advisor"})
		if len(out) >= 6 {
			return out
		}
	}
	for _, fileName := range anyToStringList(contextPack["files_to_read"], 6) {
		out = append(out, map[string]any{"label": "inspect_file", "command": "inspect " + fileName, "reason": "Cited file appears in the compact evidence trail.", "source": "context_pack.files_to_read"})
		if len(out) >= 6 {
			return out
		}
	}
	for _, raw := range contextPackAnyList(contextPack["commands"]) {
		item := anyMap(raw)
		command := strings.TrimSpace(firstNonEmptyStrings(anyToString(item["text"]), anyToString(raw)))
		if command == "" {
			continue
		}
		out = append(out, map[string]any{"label": "run_or_inspect_command", "command": clipText(command, 400), "reason": "Command surfaced from retrieved memory; verify before execution.", "source": "context_pack.commands"})
		if len(out) >= 6 {
			return out
		}
	}
	if len(highSignal) > 0 {
		out = append(out, map[string]any{"label": "use_synthesis_pack", "command": "use response.reference_prompt for the next model call", "reason": "High-signal evidence is available and bounded.", "source": "synthesis_pack"})
	}
	return out
}

func synthesisPackOpenQuestions(sourceCoverage map[string]any, omitted []any, rankedEvidence []any, topicGravity []any) []any {
	out := []any{}
	if !anyToBool(sourceCoverage["complete"]) {
		out = append(out, map[string]any{
			"question": "Are slow or pending sources hiding higher-value evidence?",
			"reason":   "source_coverage.complete=false",
			"sources":  append(anyToStringList(sourceCoverage["pending"], 8), anyToStringList(sourceCoverage["timed_out"], 8)...),
		})
	}
	if len(omitted) > 0 {
		out = append(out, map[string]any{
			"question": "Should omitted high-value refs be retrieved before acting?",
			"reason":   "context budget omitted frontier evidence",
			"count":    len(omitted),
		})
	}
	if len(rankedEvidence) == 0 {
		out = append(out, map[string]any{"question": "No ranked evidence was available; should retrieval broaden scope or switch mode?", "reason": "ranked_evidence_empty"})
	}
	if len(topicGravity) == 0 {
		out = append(out, map[string]any{"question": "No topic rollup gravity was found; should memory writes be tagged with clearer topic paths?", "reason": "topic_gravity_empty"})
	}
	if len(out) > 6 {
		out = out[:6]
	}
	return out
}

func synthesisPackSemanticTags(query string, contextPack map[string]any, highSignal []any, topicGravity []any, crossProject []any, sourceCoverage map[string]any) []any {
	set := map[string]struct{}{"synthesis_pack_v1": {}, "deterministic_synthesis": {}}
	for _, raw := range highSignal {
		kind := strings.TrimSpace(anyToString(anyMap(raw)["kind"]))
		switch kind {
		case "decision":
			set["decision_memory"] = struct{}{}
		case "risk":
			set["risk_memory"] = struct{}{}
		case "check":
			set["verification_memory"] = struct{}{}
		case "graph_neighbor":
			set["graph_linked"] = struct{}{}
		}
	}
	if len(topicGravity) > 0 {
		set["topic_gravity"] = struct{}{}
	}
	if len(crossProject) > 0 {
		set["cross_project_bridge"] = struct{}{}
	}
	if !anyToBool(sourceCoverage["complete"]) {
		set["partial_source_coverage"] = struct{}{}
	}
	if len(contextPackAnyList(contextPack["omitted_high_value_refs"])) > 0 {
		set["budget_omitted_frontier"] = struct{}{}
	}
	for _, token := range synthesisPackQueryTokens(query) {
		set["query:"+token] = struct{}{}
		if len(set) >= 18 {
			break
		}
	}
	return stringSetAnySorted(set, 24)
}

func synthesisPackQuality(highSignal []any, topicGravity []any, crossProject []any, sourceCoverage map[string]any, contextPackQuality map[string]any) map[string]any {
	score := 30
	basis := []any{}
	if len(highSignal) > 0 {
		score += minInt(len(highSignal)*5, 30)
		basis = append(basis, "ranked evidence present")
	}
	if len(topicGravity) > 0 {
		score += minInt(len(topicGravity)*3, 18)
		basis = append(basis, "topic rollup gravity present")
	}
	if len(crossProject) > 0 {
		score += minInt(len(crossProject)*4, 12)
		basis = append(basis, "cross-project bridge evidence present")
	}
	if anyToBool(sourceCoverage["complete"]) {
		score += 8
		basis = append(basis, "source coverage complete")
	}
	if anyToString(anyMap(contextPackQuality["quality"])["status"]) != "" {
		basis = append(basis, "context-pack quality telemetry attached")
	}
	score = clampInt(score, 0, 100)
	status := "sparse"
	if score >= 75 {
		status = "strong"
	} else if score >= 55 {
		status = "usable_partial"
	}
	return map[string]any{
		"status": status,
		"score":  score,
		"basis":  basis,
		"limits": []any{
			"No uncited LLM synthesis was used.",
			"Cross-project bridges require evidence from retrieved rows or memory graph edges.",
		},
	}
}

func synthesisPackEvidenceTrail(contextPack map[string]any, highSignal []any, limit int) []any {
	out := []any{}
	for _, raw := range highSignal {
		item := anyMap(raw)
		citation := strings.TrimSpace(anyToString(item["citation"]))
		if citation == "" {
			continue
		}
		out = append(out, map[string]any{
			"citation": citation,
			"kind":     item["kind"],
			"rank":     item["rank"],
		})
		if len(out) >= limit {
			return out
		}
	}
	for _, raw := range contextPackAnyList(contextPack["citations"]) {
		item := anyMap(raw)
		citation := contextPackEvidenceCitation(item)
		if citation == "" {
			continue
		}
		out = append(out, map[string]any{"citation": citation, "kind": "citation"})
		if len(out) >= limit {
			return out
		}
	}
	return out
}

func synthesisPackSummary(highSignal []any, topicGravity []any, crossProject []any, sourceCoverage map[string]any) string {
	coverage := "partial"
	if anyToBool(sourceCoverage["complete"]) {
		coverage = "complete"
	}
	return "Synthesis Pack v1 grouped " + anyToString(len(highSignal)) + " high-signal evidence cards, " + anyToString(len(topicGravity)) + " topic-gravity links, and " + anyToString(len(crossProject)) + " cross-project bridges with " + coverage + " source coverage."
}

func synthesisPackReferencePrompt(pack map[string]any) string {
	gate := anyMap(pack["decision_gate"])
	lines := []string{
		"Use this ContextLattice Synthesis Pack v1 as the decision spine for the next reasoning step.",
		"Summary: " + anyToString(pack["summary"]),
		"Decision gate: " + firstNonEmptyStrings(anyToString(gate["decision"]), "verify") + ".",
		"Rule: treat high_signal_findings and evidence_trail as observed evidence; treat synthesis_quality/open_questions as deterministic guidance, not facts.",
		"",
		"High-signal findings:",
	}
	for idx, raw := range contextPackAnyList(pack["high_signal_findings"]) {
		if idx >= 8 {
			break
		}
		item := anyMap(raw)
		line := "- [" + anyToString(item["kind"]) + "] " + anyToString(item["text"])
		if citation := strings.TrimSpace(anyToString(item["citation"])); citation != "" {
			line += " (" + citation + ")"
		}
		lines = append(lines, line)
	}
	if topics := contextPackAnyList(pack["topic_gravity"]); len(topics) > 0 {
		lines = append(lines, "", "Topic gravity:")
		for idx, raw := range topics {
			if idx >= 6 {
				break
			}
			item := anyMap(raw)
			lines = append(lines, "- "+anyToString(item["topic_path"])+" :: "+anyToString(item["reason"]))
		}
	}
	if actions := contextPackAnyList(pack["recommended_next_actions"]); len(actions) > 0 {
		lines = append(lines, "", "Recommended next actions:")
		for idx, raw := range actions {
			if idx >= 5 {
				break
			}
			item := anyMap(raw)
			lines = append(lines, "- "+firstNonEmptyStrings(anyToString(item["label"]), "act")+": "+anyToString(item["command"]))
		}
	}
	if questions := contextPackAnyList(pack["open_questions"]); len(questions) > 0 {
		lines = append(lines, "", "Open questions before relying on this pack:")
		for idx, raw := range questions {
			if idx >= 4 {
				break
			}
			lines = append(lines, "- "+anyToString(anyMap(raw)["question"]))
		}
	}
	return clipText(strings.Join(lines, "\n"), 5000)
}

func dedupeStrings(values []string, limit int) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func stringSetAnySorted(set map[string]struct{}, limit int) []any {
	values := make([]string, 0, len(set))
	for value := range set {
		if strings.TrimSpace(value) != "" {
			values = append(values, value)
		}
	}
	sort.Strings(values)
	if limit > 0 && len(values) > limit {
		values = values[:limit]
	}
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}
