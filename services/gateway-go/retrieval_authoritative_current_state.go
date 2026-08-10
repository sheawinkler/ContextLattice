package main

import (
	"context"
	"strings"
	"time"
)

type contextPackDefaultBlockingSourcesKey struct{}

func withContextPackDefaultBlockingSources(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, contextPackDefaultBlockingSourcesKey{}, true)
}

func contextPackDefaultBlockingSources(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	value, _ := ctx.Value(contextPackDefaultBlockingSourcesKey{}).(bool)
	return value
}

func (s *server) authoritativeCurrentStateFastPath(
	ctx context.Context,
	request map[string]any,
	retrievalMode string,
	minResults int,
) (sourceBatchOutput, bool) {
	output := sourceBatchOutput{
		rows:                        map[string][]map[string]any{},
		sourceOwners:                map[string]string{},
		sourceErrors:                map[string]map[string]any{},
		sourceChainDebug:            map[string][]map[string]any{},
		warnings:                    []string{},
		effectiveTimeoutsSecs:       map[string]float64{},
		adaptiveBudgets:             map[string]map[string]any{},
		serverProactiveObservations: map[string]map[string]any{},
	}
	if s == nil || s.memoryStore == nil || !s.memoryStore.isEnabled() ||
		!envBool("GO_RETRIEVAL_AUTHORITATIVE_CURRENT_STATE_FAST_PATH_ENABLED", true) ||
		normalizeRetrievalMode(retrievalMode) == "deep" ||
		!currentStateSearchAsOfSupported(request["as_of"]) {
		return output, false
	}
	project := strings.TrimSpace(anyToString(request["project"]))
	topicPath := normalizeTopicPathLoose(anyToString(request["topic_path"]))
	query := strings.TrimSpace(anyToString(request["query"]))
	if project == "" || topicPath == "" || query == "" {
		return output, false
	}
	limit := clampInt(anyToInt(request["limit"], 10), 1, 100)
	rowLimit := minInt(limit, clampInt(envInt("GO_RETRIEVAL_AUTHORITATIVE_CURRENT_STATE_MAX_ROWS", 4), 2, 16))
	includeCold := anyToBool(request["include_cold"])
	includeEphemeral := requestIncludesEphemeralMemory(request)
	minResults = maxInt(1, minResults)
	minScore := clampFloat(envFloat("GO_RETRIEVAL_AUTHORITATIVE_CURRENT_STATE_MIN_SCORE", 0.55), 0, 1)
	ancestorMinScore := clampFloat(envFloat("GO_RETRIEVAL_AUTHORITATIVE_CURRENT_STATE_ANCESTOR_MIN_SCORE", 0.15), 0, 1)
	started := time.Now()
	rows, stats, err := s.memoryStore.searchCurrentStateRowsWithAncestorFallback(
		ctx,
		query,
		project,
		topicPath,
		rowLimit,
		includeCold,
		includeEphemeral,
		minResults,
		minScore,
		ancestorMinScore,
	)
	latency := time.Since(started)
	if err != nil || len(rows) == 0 {
		return output, false
	}
	qualified := 0
	qualifiedExact := 0
	qualifiedAncestor := 0
	for _, row := range rows {
		retrievalScope := strings.ToLower(strings.TrimSpace(anyToString(row["retrieval_scope"])))
		threshold := minScore
		if retrievalScope == currentStateRetrievalScopeAncestor {
			threshold = ancestorMinScore
		}
		if !strings.EqualFold(anyToString(row["retrieval_lane"]), "current_state_index") ||
			!strings.EqualFold(anyToString(row["projection_authority"]), "current_event") ||
			strings.TrimSpace(anyToString(row["event_id"])) == "" ||
			strings.TrimSpace(anyToString(row["content_hash"])) == "" ||
			parseScore(row) < threshold {
			continue
		}
		qualified++
		if retrievalScope == currentStateRetrievalScopeAncestor {
			qualifiedAncestor++
		} else {
			qualifiedExact++
		}
	}
	minimumRelaxed := false
	if qualified < minResults {
		// The project/topic index was validated and scanned to completion. One
		// strong exact row is therefore a truthful complete scoped result when
		// the bounded ancestor chain contains no additional qualifying rows.
		// Do not force redundant slow sources merely to satisfy a cardinality
		// target that the authoritative scoped universe cannot supply.
		if qualifiedExact < 1 || !stats.ScopeExhaustive {
			return output, false
		}
		minimumRelaxed = true
	}
	if qualified == 0 {
		return output, false
	}
	owner := sourceOwnerGoNative
	rows = s.normalizeSourceRowsWithOwner(sourceTopicRollup, owner, rows)
	rows = searchIntelligenceNormalizeGatewayTrustRows(rows)
	output.rows[sourceTopicRollup] = rows
	output.sourceOwners[sourceTopicRollup] = owner
	output.sourceChainDebug[sourceTopicRollup] = []map[string]any{{
		"authoritative_current_state_fast_path": true,
		"project_documents":                     stats.ProjectDocuments,
		"project_topics":                        stats.ProjectTopics,
		"topics_scanned":                        stats.TopicsScanned,
		"scanned":                               stats.Scanned,
		"index_generation":                      stats.IndexGeneration,
		"matched":                               stats.Matched,
		"exact_matched":                         stats.ExactMatched,
		"ancestor_matched":                      stats.AncestorMatched,
		"ancestor_used":                         stats.AncestorUsed,
		"ancestor_prefix":                       stats.AncestorPrefix,
		"ancestor_depth":                        stats.AncestorDepth,
		"ancestor_prefixes":                     append([]string(nil), stats.AncestorPrefixes...),
		"scoped_universe_exhaustive":            stats.ScopeExhaustive,
		"qualified_rows":                        qualified,
		"qualified_exact_rows":                  qualifiedExact,
		"qualified_ancestor_rows":               qualifiedAncestor,
		"minimum_rows":                          minResults,
		"requested_row_limit":                   limit,
		"applied_row_limit":                     rowLimit,
		"minimum_score":                         minScore,
		"ancestor_minimum_score":                ancestorMinScore,
		"minimum_rows_relaxed":                  minimumRelaxed,
		"minimum_rows_relaxed_reason": func() string {
			if minimumRelaxed {
				return "authoritative_scoped_universe_exhausted"
			}
			return ""
		}(),
	}}
	output.warnings = append(
		output.warnings,
		"Authoritative current-state fast path satisfied scoped retrieval; redundant sources were not queried.",
	)
	s.telemetry.record(retrievalEvent{
		Source: sourceTopicRollup, Phase: "fast-authoritative", Status: "ok", LatencyMs: latency.Milliseconds(),
	})
	return output, true
}
