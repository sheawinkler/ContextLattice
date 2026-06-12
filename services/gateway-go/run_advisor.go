package main

import (
	"sort"
	"strings"
	"unicode"
)

type runAdvisorInput struct {
	Query           string
	Project         string
	TopicPath       string
	RetrievalMode   string
	SessionID       string
	AgentID         string
	SourceCoverage  map[string]any
	Retrieval       map[string]any
	Objective       objectiveContext
	RankedEvidence  []any
	ReferencePrompt string
	GraphQuality    map[string]any
	Surface         string
}

func buildRunAdvisor(input runAdvisorInput) map[string]any {
	coverage := anyMap(input.SourceCoverage)
	returned := anyToStringList(coverage["returned"], 16)
	pending := anyToStringList(coverage["pending"], 16)
	warming := anyToStringList(coverage["warming"], 16)
	failed := anyToStringList(coverage["failed"], 8)
	timedOut := anyToStringList(coverage["timed_out"], 8)
	budgetExceeded := anyToStringList(coverage["budget_exceeded"], 8)
	complete := anyToBool(coverage["complete"])
	rankedCount := len(input.RankedEvidence)
	referenceChars := len(input.ReferencePrompt)

	promptScore := 30
	if rankedCount > 0 {
		promptScore += minInt(30, rankedCount*5)
	}
	if len(returned) > 0 {
		promptScore += minInt(20, len(returned)*5)
	}
	if referenceChars >= 300 {
		promptScore += 10
	}
	if !input.Objective.empty() {
		promptScore += 8
	}
	if complete {
		promptScore += 12
	} else {
		promptScore -= minInt(20, (len(pending)+len(warming)+len(failed)+len(timedOut)+len(budgetExceeded))*4)
	}
	promptScore = clampInt(promptScore, 0, 100)

	missing := []any{}
	if rankedCount == 0 {
		missing = append(missing, "ranked_evidence")
	}
	if len(returned) == 0 {
		missing = append(missing, "returned_sources")
	}
	if input.Objective.empty() {
		missing = append(missing, "objective_context")
	}
	if !complete {
		missing = append(missing, "complete_source_coverage")
	}

	promptState := "ready"
	posture := "ready"
	switch {
	case len(returned) == 0 && rankedCount == 0 && len(failed)+len(timedOut) > 0 && len(pending)+len(warming) == 0:
		promptState = "blocked"
		posture = "blocked"
	case len(returned) == 0 && rankedCount == 0:
		promptState = "needs_context"
		posture = "needs_retrieval"
	case !complete:
		promptState = "usable_partial"
		posture = "partial_context"
	case promptScore < 70:
		promptState = "usable_partial"
		posture = "partial_context"
	}

	continuation := runAdvisorContinuation(input.Retrieval, coverage, pending, warming, failed, timedOut, budgetExceeded)
	objectiveCoherence := runAdvisorObjectiveCoherence(input.Query, input.Objective)
	retrievalAdvice := runAdvisorRetrievalAdvice(posture, promptState, complete, input.RetrievalMode, pending, warming, failed, timedOut, budgetExceeded)
	graphQuality := runAdvisorGraphQuality(input.GraphQuality)
	nextActions := runAdvisorNextActions(input, promptState, continuation, objectiveCoherence, graphQuality)

	payload := map[string]any{
		"ok":        true,
		"schema_id": runAdvisorContractID,
		"posture":   posture,
		"prompt_quality": map[string]any{
			"score":                  promptScore,
			"state":                  promptState,
			"ranked_evidence_count":  rankedCount,
			"reference_prompt_chars": referenceChars,
			"returned_source_count":  len(returned),
			"complete":               complete,
			"missing":                missing,
		},
		"retrieval_advice":    retrievalAdvice,
		"continuation":        continuation,
		"objective_coherence": objectiveCoherence,
		"graph_quality":       graphQuality,
		"next_actions":        nextActions,
	}
	return attachPayloadFormatContract(runAdvisorContractID, payload, input.AgentID, "run_advisor", firstNonEmptyStrings(input.Surface, "run_advisor"))
}

func runAdvisorContinuation(
	retrieval map[string]any,
	coverage map[string]any,
	pending []string,
	warming []string,
	failed []string,
	timedOut []string,
	budgetExceeded []string,
) map[string]any {
	lifecycle := anyMap(coverage["retrieval_lifecycle"])
	if len(lifecycle) == 0 {
		lifecycle = anyMap(retrieval["retrieval_lifecycle"])
	}
	continuation := anyMap(retrieval["continuation_async"])
	agentVisibility := anyMap(continuation["agent_visibility"])
	token := strings.TrimSpace(anyToString(continuation["token"]))
	status := strings.TrimSpace(anyToString(lifecycle["status"]))
	if status == "" {
		switch {
		case len(pending)+len(warming) > 0:
			status = "partial"
		case len(failed)+len(timedOut)+len(budgetExceeded) > 0:
			status = "failed"
		default:
			status = "succeeded"
		}
	}
	pollURL := strings.TrimSpace(firstNonEmptyStrings(anyToString(continuation["poll_url"]), anyToString(continuation["continuation_poll_url"])))
	eventsURL := strings.TrimSpace(firstNonEmptyStrings(anyToString(continuation["events_url"]), anyToString(continuation["continuation_events_url"])))
	if token != "" {
		if pollURL == "" {
			pollURL = "/memory/search/continuations/" + token
		}
		if eventsURL == "" {
			eventsURL = "/memory/search/continuations/" + token + "/events"
		}
	}
	repair := "Continue with the compiled prompt packet; rerun context retrieval only if the task needs fresher evidence."
	if len(pending)+len(warming) > 0 {
		repair = "Watch continuation events or rerun with --blocking when the next model call requires complete slow-source evidence."
	} else if len(failed)+len(timedOut)+len(budgetExceeded) > 0 {
		repair = "Retry with a narrower query, a longer source timeout, or a smaller source set before making evidence-backed claims."
	}
	out := map[string]any{
		"status":                   status,
		"token":                    token,
		"poll_url":                 pollURL,
		"events_url":               eventsURL,
		"pending_sources":          toAnyStringList(pending, 16),
		"warming_sources":          toAnyStringList(warming, 16),
		"failed_sources":           toAnyStringList(failed, 8),
		"timed_out_sources":        toAnyStringList(timedOut, 8),
		"budget_exceeded_sources":  toAnyStringList(budgetExceeded, 8),
		"continuation_available":   token != "",
		"modeled_progress":         anyMap(firstPresentAny(continuation["modeled_progress"], lifecycle["modeled_progress"])),
		"retrieval_progress":       anyMap(continuation["retrieval_progress"]),
		"agent_visibility":         agentVisibility,
		"repair_instruction":       repair,
		"agent_followup_command":   "",
		"agent_followup_endpoint":  "",
		"agent_followup_transport": "none",
	}
	if token != "" {
		out["agent_followup_command"] = firstNonEmptyStrings(
			anyToString(agentVisibility["watch_command"]),
			"curl -fsS http://127.0.0.1:8075"+pollURL,
		)
		out["agent_followup_endpoint"] = pollURL
		out["agent_followup_transport"] = "http_or_cli_watch"
	}
	return out
}

func runAdvisorRetrievalAdvice(
	posture string,
	promptState string,
	complete bool,
	currentMode string,
	pending []string,
	warming []string,
	failed []string,
	timedOut []string,
	budgetExceeded []string,
) map[string]any {
	mode := normalizeRetrievalMode(currentMode)
	if mode == "" {
		mode = "balanced"
	}
	rationale := []any{}
	switch posture {
	case "ready":
		rationale = append(rationale, "compiled_context_ready_for_next_agent_step")
	case "partial_context":
		if len(pending)+len(warming) > 0 {
			rationale = append(rationale, "slow_sources_still_warming")
			mode = "balanced"
		}
		if !complete {
			rationale = append(rationale, "source_coverage_incomplete")
		}
	case "needs_retrieval":
		mode = "deep"
		rationale = append(rationale, "no_ranked_evidence_or_returned_sources")
	case "blocked":
		mode = "balanced"
		rationale = append(rationale, "retrieval_failed_or_timed_out")
	}
	if len(failed)+len(timedOut)+len(budgetExceeded) > 0 {
		rationale = append(rationale, "some_sources_failed_or_exceeded_budget")
	}
	if promptState == "needs_context" && len(rationale) == 0 {
		rationale = append(rationale, "context_packet_needs_more_evidence")
	}
	return map[string]any{
		"recommended_mode":     mode,
		"recommended_surface":  "cli_for_local_agents",
		"alternate_surfaces":   []any{"http_for_app_integrations", "mcp_for_tool_calling_hosts"},
		"rationale":            rationale,
		"blocking_recommended": len(pending)+len(warming) > 0,
	}
}

func runAdvisorObjectiveCoherence(query string, objective objectiveContext) map[string]any {
	objectiveText := strings.TrimSpace(strings.Join(append([]string{
		objective.Mission,
		objective.Objective,
		objective.Goal,
		objective.ProjectPrimaryObjective,
		objective.TopicObjective,
		objective.SessionObjective,
	}, objective.Subobjectives...), " "))
	queryTokens := runAdvisorTokenSet(query)
	objectiveTokens := runAdvisorTokenSet(objectiveText)
	shared := []string{}
	for token := range queryTokens {
		if _, ok := objectiveTokens[token]; ok {
			shared = append(shared, token)
		}
	}
	sort.Strings(shared)
	score := 0
	status := "missing"
	if strings.TrimSpace(query) != "" && strings.TrimSpace(objectiveText) != "" {
		denominator := maxInt(1, minInt(len(queryTokens), len(objectiveTokens)))
		score = clampInt(40+int(float64(len(shared))/float64(denominator)*60.0), 0, 100)
		status = "partial"
		if score >= 82 {
			status = "aligned"
		} else if score < 50 {
			status = "mismatch"
		}
	}
	repair := "Carry the user objective, goal, and mission into the next prompt packet."
	if status == "aligned" {
		repair = "Objective context is aligned; preserve it while executing the smallest verifiable next action."
	} else if status == "mismatch" {
		repair = "Restate the current user objective explicitly and retrieve again if the packet is from a stale or different task."
	}
	return map[string]any{
		"score":  score,
		"status": status,
		"signals": map[string]any{
			"mission_present":                   strings.TrimSpace(objective.Mission) != "",
			"objective_present":                 strings.TrimSpace(objective.Objective) != "",
			"goal_present":                      strings.TrimSpace(objective.Goal) != "",
			"project_primary_objective_present": strings.TrimSpace(objective.ProjectPrimaryObjective) != "",
			"topic_objective_present":           strings.TrimSpace(objective.TopicObjective) != "",
			"session_objective_present":         strings.TrimSpace(objective.SessionObjective) != "",
			"subobjective_count":                len(objectiveCleanStringList(objective.Subobjectives, 12)),
			"shared_terms":                      toAnyStringList(shared, 12),
			"query_token_count":                 len(queryTokens),
			"context_token_count":               len(objectiveTokens),
		},
		"repair_instruction": repair,
	}
}

func runAdvisorGraphQuality(input map[string]any) map[string]any {
	if len(input) == 0 {
		return map[string]any{
			"status":         "not_sampled",
			"score":          0,
			"signals":        map[string]any{"edge_samples": 0},
			"recommendation": "Run contextlattice_memory_topology or memory-edge-backfill with inferred audit when graph evidence matters.",
		}
	}
	status := strings.TrimSpace(anyToString(input["status"]))
	if status == "" {
		status = "sampled"
	}
	score := anyToInt(input["score"], 0)
	if score == 0 {
		score = anyToInt(input["quality_score"], 0)
	}
	recommendation := strings.TrimSpace(anyToString(input["recommendation"]))
	if recommendation == "" {
		recommendation = "Use high-confidence edges as supporting context, not sole authority."
	}
	return map[string]any{
		"status":         status,
		"score":          score,
		"signals":        anyMap(input["signals"]),
		"recommendation": recommendation,
	}
}

func runAdvisorNextActions(
	input runAdvisorInput,
	promptState string,
	continuation map[string]any,
	objectiveCoherence map[string]any,
	graphQuality map[string]any,
) []any {
	actions := []any{}
	query := clipText(strings.TrimSpace(input.Query), 220)
	mode := normalizeRetrievalMode(input.RetrievalMode)
	if mode == "" {
		mode = "balanced"
	}
	if promptState == "needs_context" || promptState == "blocked" {
		actions = append(actions, map[string]any{
			"label":   "rebuild_context_pack",
			"command": "contextlattice_pack " + shellQuoteForAdvisor(query) + " --project " + shellQuoteForAdvisor(firstNonEmptyStrings(input.Project, "contextlattice")) + " --mode deep --pretty",
			"reason":  "No usable ranked evidence is ready for the next model call.",
		})
	} else {
		actions = append(actions, map[string]any{
			"label":   "send_reference_prompt",
			"command": "use response.reference_prompt for the next model call",
			"reason":  "The packet is bounded and shaped for agent prompt repackaging.",
		})
	}
	if anyToBool(continuation["continuation_available"]) {
		actions = append(actions, map[string]any{
			"label":   "watch_continuation",
			"command": anyToString(continuation["agent_followup_command"]),
			"reason":  "Slow-source evidence is still available through the continuation lifecycle.",
		})
	}
	if anyToString(objectiveCoherence["status"]) == "mismatch" || anyToString(objectiveCoherence["status"]) == "missing" {
		actions = append(actions, map[string]any{
			"label":   "repair_objective_context",
			"command": "include mission/objective/goal fields on the next context-pack or preflight request",
			"reason":  anyToString(objectiveCoherence["repair_instruction"]),
		})
	}
	if anyToString(graphQuality["status"]) == "not_sampled" {
		actions = append(actions, map[string]any{
			"label":   "sample_graph_edges",
			"command": "contextlattice_memory_topology --pretty",
			"reason":  "Graph contribution was not sampled for this run trace.",
		})
	}
	if len(actions) > 5 {
		actions = actions[:5]
	}
	return actions
}

func latestRunAdvisorFromEvents(events []map[string]any) map[string]any {
	for idx := len(events) - 1; idx >= 0; idx-- {
		metadata := anyMap(events[idx]["metadata"])
		advisor := anyMap(metadata["run_advisor"])
		if len(advisor) > 0 {
			return cloneAnyMap(advisor)
		}
	}
	return map[string]any{}
}

func buildRunAdvisorFromTraceRollup(session map[string]any, rollup map[string]any, events []map[string]any) map[string]any {
	if advisor := latestRunAdvisorFromEvents(events); len(advisor) > 0 {
		return advisor
	}
	retrievalSummary := anyMap(rollup["retrieval_summary"])
	sourceCoverage := map[string]any{
		"returned": anyToStringList(retrievalSummary["returned_sources"], 16),
		"pending":  anyToStringList(retrievalSummary["pending_sources"], 16),
		"failed":   anyToStringList(retrievalSummary["failed_sources"], 8),
		"complete": len(anyToStringList(retrievalSummary["pending_sources"], 16)) == 0 && len(anyToStringList(retrievalSummary["failed_sources"], 8)) == 0,
	}
	promptPackage := anyMap(rollup["prompt_package"])
	referencePrompt := ""
	if anyToBool(promptPackage["ready"]) {
		referencePrompt = "agent prompt package ready"
	}
	graphQuality := map[string]any{
		"status": "trace_sampled",
		"score":  anyToInt(rollup["confidence"], 0),
		"signals": map[string]any{
			"graph_touches": anyToInt(anyMap(rollup["artifact_summary"])["graph_touches"], 0),
			"handoffs":      anyToInt(anyMap(rollup["artifact_summary"])["handoffs"], 0),
			"checkpoints":   anyToInt(anyMap(rollup["artifact_summary"])["checkpoints"], 0),
		},
		"recommendation": "Use trace graph touches as run-shaping evidence, then inspect cited memory before relying on edge semantics.",
	}
	return buildRunAdvisor(runAdvisorInput{
		Query:          firstNonEmptyStrings(anyToString(rollup["objective"]), anyToString(session["objective"])),
		Project:        firstNonEmptyStrings(anyToString(rollup["project"]), anyToString(session["project"])),
		RetrievalMode:  "balanced",
		SessionID:      firstNonEmptyStrings(anyToString(rollup["session_id"]), anyToString(session["id"])),
		AgentID:        firstNonEmptyStrings(anyToString(rollup["agent_id"]), anyToString(session["agent_id"])),
		SourceCoverage: sourceCoverage,
		Objective: objectiveContext{
			Mission:                 anyToString(rollup["mission"]),
			Objective:               anyToString(rollup["objective"]),
			Goal:                    anyToString(rollup["goal"]),
			ProjectPrimaryObjective: anyToString(anyMap(anyMap(rollup["objective_hierarchy"])["project"])["primary_objective"]),
			TopicObjective:          anyToString(anyMap(anyMap(rollup["objective_hierarchy"])["topic"])["objective"]),
			SessionObjective:        anyToString(anyMap(anyMap(rollup["objective_hierarchy"])["session"])["objective"]),
			Subobjectives:           anyToStringList(anyMap(rollup["objective_hierarchy"])["subobjectives"], 12),
		},
		RankedEvidence:  []any{},
		ReferencePrompt: referencePrompt,
		GraphQuality:    graphQuality,
		Surface:         "/v1/agents/sessions/{session_id}/trace",
	})
}

func runAdvisorTokenSet(text string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, token := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		token = strings.TrimSpace(token)
		if len(token) < 3 {
			continue
		}
		if _, skip := runAdvisorStopWords[token]; skip {
			continue
		}
		out[token] = struct{}{}
	}
	return out
}

var runAdvisorStopWords = map[string]struct{}{
	"and": {}, "are": {}, "but": {}, "for": {}, "from": {}, "have": {}, "into": {}, "less": {}, "more": {},
	"not": {}, "over": {}, "the": {}, "then": {}, "this": {}, "that": {}, "with": {}, "while": {}, "your": {},
}

func toAnyStringList(values []string, limit int) []any {
	if limit < 1 {
		limit = len(values)
	}
	if len(values) > limit {
		values = values[:limit]
	}
	out := make([]any, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func firstPresentAny(values ...any) any {
	for _, value := range values {
		if value == nil {
			continue
		}
		switch typed := value.(type) {
		case string:
			if strings.TrimSpace(typed) != "" {
				return value
			}
		case map[string]any:
			if len(typed) > 0 {
				return value
			}
		case []any:
			if len(typed) > 0 {
				return value
			}
		default:
			return value
		}
	}
	return nil
}

func shellQuoteForAdvisor(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
