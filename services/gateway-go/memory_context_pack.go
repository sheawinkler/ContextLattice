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
	response := map[string]any{
		"query":            query,
		"context_pack":     contextPack,
		"warnings":         parseWarnings(searchResponse["warnings"]),
		"retrieval_mode":   searchResponse["retrieval_mode"],
		"retrieval_intent": searchResponse["retrieval_intent"],
		"traffic_class":    searchResponse["traffic_class"],
		"agent_id":         searchResponse["agent_id"],
	}
	if includeRetrievalDebug {
		if retrievalDebug, ok := searchResponse["retrieval_debug"]; ok {
			response["retrieval"] = retrievalDebug
		}
	}
	return response, status, nil
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

	return map[string]any{
		"query":             query,
		"generatedAt":       nowUTCISO(),
		"factualOnly":       true,
		"strictNumericCopy": anyToBoolOrDefault(grounding["strict_numeric_copy"], true),
		"facts":             factsAny,
		"numericFacts":      numericFactsAny,
		"citations":         citations,
		"results":           resultRows,
		"warnings":          parseWarnings(searchResponse["warnings"]),
		"retrievalMode":     searchResponse["retrieval_mode"],
		"retrievalIntent":   searchResponse["retrieval_intent"],
		"agentId":           searchResponse["agent_id"],
	}
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
