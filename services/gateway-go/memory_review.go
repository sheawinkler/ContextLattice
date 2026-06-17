package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

type reviewModeOptions struct {
	Project          string
	TopicPath        string
	Query            string
	AgentID          string
	SessionID        string
	WindowHours      int
	MaxPatterns      int
	Limit            int
	IncludeCold      bool
	IncludeEphemeral bool
}

func (s *server) memoryReview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	payload := map[string]any{}
	if r.Method == http.MethodPost {
		var err error
		payload, err = readOptionalJSONBody(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
			return
		}
	} else {
		payload = queryPayload(r)
	}
	response, status := s.buildReviewModeResponse(r.Context(), payload, "/memory/review")
	writeJSON(w, status, response)
}

func (s *server) toolsReview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareToolHeaders(w, r, "/tools/review"); !ok {
		return
	}
	payload := map[string]any{}
	if r.Method == http.MethodPost {
		var err error
		payload, err = readOptionalJSONBody(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
			return
		}
	} else {
		payload = queryPayload(r)
	}
	response, status := s.buildReviewModeResponse(r.Context(), payload, "/tools/review")
	response["tool"] = "review"
	response = attachPayloadFormatContract(reviewModeResponseContractID, response, anyToString(response["agent_id"]), "review", "/tools/review")
	writeJSON(w, status, response)
}

func queryPayload(r *http.Request) map[string]any {
	payload := map[string]any{}
	for key, values := range r.URL.Query() {
		if len(values) == 0 {
			continue
		}
		payload[key] = values[len(values)-1]
	}
	return payload
}

func (s *server) buildReviewModeResponse(ctx context.Context, payload map[string]any, endpoint string) (map[string]any, int) {
	opts := normalizeReviewModeOptions(payload)
	if s == nil || s.memoryStore == nil || !s.memoryStore.policy.enabled {
		response := reviewModeUnavailableResponse(opts, "Go memory store is disabled; Review Mode needs memory write history.")
		return attachReviewModeFormatContract(response, endpoint), http.StatusOK
	}
	if ctx == nil {
		ctx = context.Background()
	}

	entries := s.memoryStore.reviewEntries(opts)
	rollups := s.memoryStore.topicRollupsWithOptions(ctx, opts.Project, 1, opts.Limit, 0, opts.IncludeCold, opts.IncludeEphemeral)
	rawTopics, _ := rollups["topics"].([]any)
	topics := reviewTopicRows(rawTopics, opts.TopicPath)
	patterns := buildReviewPatterns(opts, entries, topics, rollups)
	if len(patterns) > opts.MaxPatterns {
		patterns = patterns[:opts.MaxPatterns]
	}
	guidance := reviewAgentGuidance(patterns, opts)
	summary := reviewSummary(opts, entries, topics, patterns, rollups)
	response := map[string]any{
		"ok":                 true,
		"schema_id":          reviewModeResponseContractID,
		"mode":               "review",
		"project":            opts.Project,
		"query":              opts.Query,
		"topic_path":         opts.TopicPath,
		"window_hours":       opts.WindowHours,
		"agent_id":           opts.AgentID,
		"session_id":         opts.SessionID,
		"summary":            summary,
		"patterns":           patterns,
		"agent_guidance":     guidance,
		"source_coverage":    reviewSourceCoverage(true, []any{"memory_write_history", "topic_rollups"}),
		"writeback_required": true,
		"review_context": map[string]any{
			"generated_at":       nowUTCISO(),
			"endpoint":           endpoint,
			"max_patterns":       opts.MaxPatterns,
			"limit":              opts.Limit,
			"include_cold":       opts.IncludeCold,
			"include_ephemeral":  opts.IncludeEphemeral,
			"rollup_total":       anyToInt(rollups["total"], 0),
			"history_scanned":    anyToInt(rollups["historyEntriesScanned"], 0),
			"history_deduped":    anyToInt(rollups["historyEntriesDeduped"], 0),
			"deterministic_only": true,
		},
		"warnings": []any{},
	}
	if opts.SessionID != "" {
		session := s.recordAgentSessionEvent(opts.SessionID, "review.completed", map[string]any{
			"agent_id": opts.AgentID,
			"project":  opts.Project,
			"summary":  firstNonEmptyStrings(opts.Query, opts.TopicPath, "ContextLattice Review Mode"),
			"metadata": map[string]any{
				"endpoint":      endpoint,
				"topic_path":    opts.TopicPath,
				"pattern_count": len(patterns),
				"window_hours":  opts.WindowHours,
				"summary":       summary,
			},
		})
		if session != nil {
			response["agent_runtime"] = map[string]any{
				"session_id":          opts.SessionID,
				"memory_contribution": session["memory_contribution"],
				"last_event_type":     session["last_event_type"],
			}
		}
	}
	return attachReviewModeFormatContract(response, endpoint), http.StatusOK
}

func normalizeReviewModeOptions(payload map[string]any) reviewModeOptions {
	project := strings.TrimSpace(firstNonEmptyStrings(
		anyToString(payload["project"]),
		anyToString(payload["workspace"]),
		osEnvString("CONTEXTLATTICE_PROJECT"),
		"contextlattice",
	))
	topicPath := strings.Trim(strings.TrimSpace(firstNonEmptyStrings(
		anyToString(payload["topic_path"]),
		anyToString(payload["topicPath"]),
		anyToString(payload["path"]),
	)), "/")
	query := strings.TrimSpace(firstNonEmptyStrings(
		anyToString(payload["query"]),
		anyToString(payload["goal"]),
		anyToString(payload["objective"]),
		"review repeated ContextLattice memory patterns and mitigation steps",
	))
	windowHours := clampInt(anyToInt(firstPresent(payload, "window_hours", "windowHours"), envInt("GO_REVIEW_WINDOW_HOURS", 72)), 1, 2160)
	maxPatterns := clampInt(anyToInt(firstPresent(payload, "max_patterns", "maxPatterns"), envInt("GO_REVIEW_MAX_PATTERNS", 6)), 1, 12)
	limit := clampInt(anyToInt(payload["limit"], envInt("GO_REVIEW_ROLLUP_LIMIT", 160)), 12, 500)
	includeCold := anyToBool(firstPresent(payload, "include_cold", "includeCold"))
	includeEphemeral := anyToBool(firstPresent(payload, "include_ephemeral", "includeEphemeral", "include_test_memory"))
	return reviewModeOptions{
		Project:          project,
		TopicPath:        topicPath,
		Query:            query,
		AgentID:          strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["agent_id"]), anyToString(payload["agentId"]), osEnvString("CONTEXTLATTICE_AGENT_ID"))),
		SessionID:        strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["session_id"]), anyToString(payload["sessionId"]))),
		WindowHours:      windowHours,
		MaxPatterns:      maxPatterns,
		Limit:            limit,
		IncludeCold:      includeCold,
		IncludeEphemeral: includeEphemeral,
	}
}

func osEnvString(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}

func (m *memoryStore) reviewEntries(opts reviewModeOptions) []memoryStoreEntry {
	if m == nil || !m.policy.enabled {
		return []memoryStoreEntry{}
	}
	cutoff := time.Now().UTC().Add(-time.Duration(opts.WindowHours) * time.Hour)
	project := strings.TrimSpace(opts.Project)
	topic := strings.Trim(strings.ToLower(strings.TrimSpace(opts.TopicPath)), "/")

	m.mu.RLock()
	entries := append([]memoryStoreEntry(nil), m.recent...)
	m.mu.RUnlock()

	out := make([]memoryStoreEntry, 0, minInt(len(entries), opts.Limit*4))
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if isMemoryTombstone(entry) {
			continue
		}
		if project != "" && !strings.EqualFold(strings.TrimSpace(entry.Project), project) {
			continue
		}
		lifecycle := normalizeMemoryLifecycle(entry.Lifecycle)
		if !shouldSurfaceMemoryLifecycle(lifecycle, opts.IncludeEphemeral) {
			continue
		}
		entryTopic := strings.Trim(strings.ToLower(strings.TrimSpace(entry.TopicPath)), "/")
		if topic != "" && entryTopic != topic && !strings.HasPrefix(entryTopic, topic+"/") {
			continue
		}
		if parsed, ok := parseTimeBestEffort(entry.CreatedAt); ok && parsed.UTC().Before(cutoff) {
			continue
		}
		out = append(out, entry)
		if len(out) >= opts.Limit*4 {
			break
		}
	}
	return out
}

func reviewTopicRows(rawTopics []any, topicPath string) []map[string]any {
	rows := make([]map[string]any, 0, len(rawTopics))
	topic := strings.Trim(strings.ToLower(strings.TrimSpace(topicPath)), "/")
	for _, raw := range rawTopics {
		row := anyMap(raw)
		path := strings.Trim(strings.ToLower(anyToString(row["path"])), "/")
		if topic != "" && path != topic && !strings.HasPrefix(path, topic+"/") {
			continue
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		left := anyToInt(rows[i]["agentIntensityScore"], 0)
		right := anyToInt(rows[j]["agentIntensityScore"], 0)
		if left == right {
			return anyToInt(rows[i]["writeCount"], 0) > anyToInt(rows[j]["writeCount"], 0)
		}
		return left > right
	})
	return rows
}

func buildReviewPatterns(opts reviewModeOptions, entries []memoryStoreEntry, topics []map[string]any, rollups map[string]any) []any {
	patterns := []map[string]any{}
	if len(entries) == 0 && len(topics) == 0 {
		patterns = append(patterns, reviewPattern(
			"thin_memory_gap",
			"low",
			0.72,
			opts.Project,
			opts.TopicPath,
			0,
			[]string{"No recent memory writes or topic rollups matched this review scope."},
			[]string{
				"Run a focused context pack before executing high-risk work.",
				"Write a durable checkpoint after the next verified decision.",
			},
			"Do not infer continuity from an empty scope; retrieve broader project context before acting.",
		))
		return reviewPatternList(patterns)
	}

	if len(topics) > 0 {
		top := topics[0]
		score := anyToInt(top["agentIntensityScore"], 0)
		writeCount := anyToInt(top["writeCount"], anyToInt(top["eventCount"], 0))
		if score >= 45 || writeCount >= 8 {
			path := anyToString(top["path"])
			patterns = append(patterns, reviewPattern(
				"agent_write_intensity",
				severityForScore(score, writeCount),
				roundFloat(0.62+float64(minInt(score, 100))/260, 3),
				anyToString(top["project"]),
				path,
				maxInt(score, writeCount),
				[]string{
					fmt.Sprintf("%s has intensity=%d, writes=%d, recent=%d.", pathOrRoot(path), score, writeCount, anyToInt(top["recentEventCount"], 0)),
					fmt.Sprintf("unique agents=%d, unique sessions=%d.", anyToInt(top["uniqueAgentCount"], 0), anyToInt(top["uniqueSessionCount"], 0)),
				},
				[]string{
					"Consolidate repeated branch learnings into one durable runbook/checkpoint before new execution.",
					"Have the next agent read the focused branch and explicitly separate current facts from stale attempts.",
				},
				"Treat this branch as high-pressure shared memory; stabilize it before adding more speculative writes.",
			))
		}
	}

	topicCounts := map[string]int{}
	compactionCount := 0
	checkpointCount := 0
	missingAttribution := 0
	rewriteCount := 0
	failureTerms := map[string]int{}
	for _, entry := range entries {
		topic := strings.Trim(strings.TrimSpace(entry.TopicPath), "/")
		if topic == "" {
			topic = deriveTopicFromFile(entry.FileName)
		}
		topicCounts[topic] += 1
		text := strings.ToLower(entry.FileName + " " + entry.TopicPath + " " + entry.Summary)
		if strings.Contains(text, "compact") || strings.Contains(text, "compaction") {
			compactionCount += 1
		}
		if strings.Contains(text, "checkpoint") || strings.Contains(text, "handoff") || strings.Contains(text, "preflight") {
			checkpointCount += 1
		}
		if strings.TrimSpace(entry.AgentID) == "" || strings.TrimSpace(entry.SessionID) == "" {
			missingAttribution += 1
		}
		if strings.EqualFold(strings.TrimSpace(entry.DiffState), "rewrite") {
			rewriteCount += 1
		}
		for _, term := range []string{"overflow", "timeout", "failed", "failure", "blocked", "stale", "retry", "regression"} {
			if strings.Contains(text, term) {
				failureTerms[term] += 1
			}
		}
	}

	topTopic, topTopicCount := topStringCount(topicCounts)
	if topTopicCount >= 4 {
		patterns = append(patterns, reviewPattern(
			"repeated_topic_churn",
			severityForCount(topTopicCount, 10, 6),
			roundFloat(0.58+float64(minInt(topTopicCount, 12))/36, 3),
			opts.Project,
			topTopic,
			topTopicCount,
			[]string{fmt.Sprintf("%d recent writes landed on %s.", topTopicCount, pathOrRoot(topTopic))},
			[]string{
				"Summarize the repeated attempts into a single mitigation note.",
				"Before continuing, identify which previous attempt changed the actual system state.",
			},
			"Stop adding parallel notes to the same branch until the repeated work is collapsed into one current decision.",
		))
	}

	if compactionCount >= 3 || checkpointCount >= 5 {
		patterns = append(patterns, reviewPattern(
			"handoff_compaction_pressure",
			severityForCount(compactionCount+checkpointCount, 14, 7),
			roundFloat(0.6+float64(minInt(compactionCount+checkpointCount, 14))/40, 3),
			opts.Project,
			opts.TopicPath,
			compactionCount+checkpointCount,
			[]string{
				fmt.Sprintf("compaction-like writes=%d, checkpoint/handoff writes=%d.", compactionCount, checkpointCount),
			},
			[]string{
				"Write one bounded handoff with objective, exact verification state, and remaining blocker.",
				"Use Review Mode guidance in the next preflight before broad retrieval.",
			},
			"Assume context churn is now a product signal; checkpoint the current truth before further execution.",
		))
	}

	if rewriteCount >= 2 {
		patterns = append(patterns, reviewPattern(
			"rewrite_churn",
			severityForCount(rewriteCount, 7, 3),
			roundFloat(0.57+float64(minInt(rewriteCount, 8))/32, 3),
			opts.Project,
			opts.TopicPath,
			rewriteCount,
			[]string{fmt.Sprintf("%d recent writes were classified as rewrites.", rewriteCount)},
			[]string{
				"Diff the latest artifact against the previous stable checkpoint before continuing.",
				"Require acceptance criteria in the next write so future agents can tell revision from regression.",
			},
			"Do not treat rewrite volume as progress; verify the current artifact against the intended behavior.",
		))
	}

	if missingAttribution >= 2 && missingAttribution*3 >= maxInt(len(entries), 1) {
		patterns = append(patterns, reviewPattern(
			"missing_agent_attribution",
			"medium",
			0.74,
			opts.Project,
			opts.TopicPath,
			missingAttribution,
			[]string{fmt.Sprintf("%d/%d recent writes lacked agent_id or session_id.", missingAttribution, len(entries))},
			[]string{
				"Set CONTEXTLATTICE_AGENT_ID and keep a session id for agent-runtime writes.",
				"Prefer contextlattice_agent_policy_pack or agent preflight so session metadata is attached automatically.",
			},
			"Fix attribution before relying on cross-agent intensity; unattributed writes hide ownership and repeat loops.",
		))
	}

	if term, count := topStringCount(failureTerms); count >= 3 {
		patterns = append(patterns, reviewPattern(
			"repeated_failure_language",
			severityForCount(count, 8, 4),
			roundFloat(0.56+float64(minInt(count, 8))/28, 3),
			opts.Project,
			opts.TopicPath,
			count,
			[]string{fmt.Sprintf("The term %q appeared in %d recent memory summaries/files.", term, count)},
			[]string{
				"Name the repeated failure mode and write the mitigation as an acceptance criterion.",
				"Route the next agent through focused review before implementation.",
			},
			"Treat repeated failure language as a blocker signal until a mitigation is written and verified.",
		))
	}

	if len(patterns) == 0 {
		patterns = append(patterns, reviewPattern(
			"no_repeat_pattern_threshold",
			"low",
			0.68,
			opts.Project,
			opts.TopicPath,
			len(entries),
			[]string{fmt.Sprintf("%d recent writes and %d rollup topics were reviewed.", len(entries), len(topics))},
			[]string{
				"Continue normal preflight and checkpoint cadence.",
				"Re-run Review Mode with a broader topic if the agent sees unexplained drift.",
			},
			"No strong repeat pattern crossed threshold in this scope; keep using evidence-first retrieval.",
		))
	}

	_ = rollups
	sort.SliceStable(patterns, func(i, j int) bool {
		leftSeverity := severityRank(anyToString(patterns[i]["severity"]))
		rightSeverity := severityRank(anyToString(patterns[j]["severity"]))
		if leftSeverity == rightSeverity {
			return anyToInt(patterns[i]["signal_count"], 0) > anyToInt(patterns[j]["signal_count"], 0)
		}
		return leftSeverity > rightSeverity
	})
	return reviewPatternList(patterns)
}

func reviewSummary(opts reviewModeOptions, entries []memoryStoreEntry, topics []map[string]any, patterns []any, rollups map[string]any) map[string]any {
	agents := map[string]struct{}{}
	sessions := map[string]struct{}{}
	recentBytes := 0
	latest := time.Time{}
	for _, entry := range entries {
		if agent := strings.TrimSpace(entry.AgentID); agent != "" {
			agents[agent] = struct{}{}
		}
		if session := strings.TrimSpace(entry.SessionID); session != "" {
			sessions[session] = struct{}{}
		}
		recentBytes += entry.RawBytes
		if parsed, ok := parseTimeBestEffort(entry.CreatedAt); ok && (latest.IsZero() || parsed.After(latest)) {
			latest = parsed.UTC()
		}
	}
	maxPressure := 0
	hotTopics := 0
	for _, topic := range topics {
		score := anyToInt(topic["agentIntensityScore"], 0)
		if score > maxPressure {
			maxPressure = score
		}
		if anyToInt(topic["recentEventCount"], 0) > 0 || score >= 45 {
			hotTopics += 1
		}
	}
	posture := "clear"
	for _, pattern := range patterns {
		row := anyMap(pattern)
		if severityRank(anyToString(row["severity"])) >= severityRank("high") {
			posture = "mitigate"
			break
		}
		if severityRank(anyToString(row["severity"])) >= severityRank("medium") {
			posture = "watch"
		}
	}
	latestText := ""
	if !latest.IsZero() {
		latestText = latest.UTC().Format(time.RFC3339Nano)
	}
	return map[string]any{
		"posture":              posture,
		"project":              opts.Project,
		"topic_path":           opts.TopicPath,
		"window_hours":         opts.WindowHours,
		"recent_writes":        len(entries),
		"recent_raw_bytes":     recentBytes,
		"unique_agents":        len(agents),
		"unique_sessions":      len(sessions),
		"hot_topics":           hotTopics,
		"pressure_score":       maxPressure,
		"pattern_count":        len(patterns),
		"rollup_total":         anyToInt(rollups["total"], 0),
		"latest_write_at":      latestText,
		"deterministic_review": true,
	}
}

func reviewAgentGuidance(patterns []any, opts reviewModeOptions) []any {
	if len(patterns) == 0 {
		return []any{"Run normal ContextLattice preflight and checkpoint after verified state changes."}
	}
	guidance := []any{}
	for _, pattern := range patterns {
		row := anyMap(pattern)
		text := strings.TrimSpace(anyToString(row["agent_guidance"]))
		if text == "" {
			continue
		}
		guidance = append(guidance, text)
		if len(guidance) >= 4 {
			break
		}
	}
	if len(guidance) == 0 {
		guidance = append(guidance, "Review the selected memory branch before continuing execution.")
	}
	if opts.TopicPath != "" {
		guidance = append(guidance, "Next preflight should include topic_path="+opts.TopicPath+" so mitigation stays scoped.")
	}
	return guidance
}

func reviewModeUnavailableResponse(opts reviewModeOptions, reason string) map[string]any {
	return map[string]any{
		"ok":                 false,
		"schema_id":          reviewModeResponseContractID,
		"mode":               "review",
		"project":            opts.Project,
		"query":              opts.Query,
		"topic_path":         opts.TopicPath,
		"window_hours":       opts.WindowHours,
		"agent_id":           opts.AgentID,
		"session_id":         opts.SessionID,
		"summary":            map[string]any{"posture": "unavailable", "recent_writes": 0, "pattern_count": 0, "pressure_score": 0},
		"patterns":           []any{},
		"agent_guidance":     []any{"Enable the Go memory store or rerun against a gateway with memory write history available."},
		"source_coverage":    reviewSourceCoverage(false, []any{}),
		"writeback_required": true,
		"review_context":     map[string]any{"generated_at": nowUTCISO(), "deterministic_only": true},
		"warnings":           []any{reason},
	}
}

func reviewSourceCoverage(complete bool, returned []any) map[string]any {
	return map[string]any{
		"configured": []any{"go_memory_store", "topic_rollups"},
		"returned":   returned,
		"complete":   complete,
	}
}

func reviewPattern(id string, severity string, confidence float64, project string, topicPath string, signalCount int, evidence []string, mitigation []string, agentGuidance string) map[string]any {
	evidenceAny := make([]any, 0, minInt(len(evidence), 6))
	for _, item := range evidence {
		text := strings.TrimSpace(item)
		if text != "" {
			evidenceAny = append(evidenceAny, clipUTF8Bytes(text, 500))
		}
		if len(evidenceAny) >= 6 {
			break
		}
	}
	mitigationAny := make([]any, 0, minInt(len(mitigation), 6))
	for _, item := range mitigation {
		text := strings.TrimSpace(item)
		if text != "" {
			mitigationAny = append(mitigationAny, clipUTF8Bytes(text, 520))
		}
		if len(mitigationAny) >= 6 {
			break
		}
	}
	return map[string]any{
		"id":             id,
		"category":       id,
		"severity":       severity,
		"confidence":     confidence,
		"project":        strings.TrimSpace(project),
		"topic_path":     strings.Trim(strings.TrimSpace(topicPath), "/"),
		"signal_count":   signalCount,
		"evidence":       evidenceAny,
		"mitigation":     mitigationAny,
		"agent_guidance": clipUTF8Bytes(strings.TrimSpace(agentGuidance), 700),
	}
}

func reviewPatternList(patterns []map[string]any) []any {
	out := make([]any, 0, len(patterns))
	for _, pattern := range patterns {
		out = append(out, pattern)
	}
	return out
}

func attachReviewModeFormatContract(payload map[string]any, endpoint string) map[string]any {
	return attachPayloadFormatContract(
		reviewModeResponseContractID,
		payload,
		anyToString(payload["agent_id"]),
		"review",
		endpoint,
	)
}

func topStringCount(counts map[string]int) (string, int) {
	bestKey := ""
	bestCount := 0
	for key, count := range counts {
		if count > bestCount || (count == bestCount && key < bestKey) {
			bestKey = key
			bestCount = count
		}
	}
	return bestKey, bestCount
}

func severityForScore(score int, count int) string {
	if score >= 82 || count >= 18 {
		return "high"
	}
	if score >= 55 || count >= 10 {
		return "medium"
	}
	return "low"
}

func severityForCount(count int, high int, medium int) string {
	if count >= high {
		return "high"
	}
	if count >= medium {
		return "medium"
	}
	return "low"
}

func severityRank(severity string) int {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "critical":
		return 4
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

func pathOrRoot(path string) string {
	path = strings.Trim(strings.TrimSpace(path), "/")
	if path == "" {
		return "root"
	}
	return path
}
