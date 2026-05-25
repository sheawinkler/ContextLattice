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
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "context_pack_unavailable", "detail": execErr.Error()})
		return
	}
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
		"traffic_class":           strings.TrimSpace(anyToString(requestPayload["traffic_class"])),
		"include_grounding":       true,
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
	combinedSources := anyToBoolOrDefault(requestPayload["combined_sources"], true)
	if _, present := requestPayload["wait_for_slow_sources"]; !present && combinedSources {
		searchRequest["wait_for_slow_sources"] = true
	}
	if _, present := requestPayload["sync_slow_sources"]; !present && combinedSources {
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

	contextPack := buildContextPackPayload(query, searchResponse, maxFacts, limit)
	sourceCoverage := contextPackSourceCoverage(searchResponse)
	contextPack["sourceCoverage"] = sourceCoverage
	contextPack["combinedSources"] = combinedSources
	response := map[string]any{
		"ok":                 true,
		"query":              query,
		"context_pack":       contextPack,
		"warnings":           parseWarnings(searchResponse["warnings"]),
		"retrieval_mode":     searchResponse["retrieval_mode"],
		"retrieval_intent":   searchResponse["retrieval_intent"],
		"traffic_class":      searchResponse["traffic_class"],
		"agent_id":           searchResponse["agent_id"],
		"source_coverage":    sourceCoverage,
		"writeback_required": true,
	}
	if includeRetrievalDebug {
		if retrievalDebug, ok := searchResponse["retrieval_debug"]; ok {
			response["retrieval"] = retrievalDebug
		}
	}
	return attachContextPackFormatContract(response), status, nil
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
