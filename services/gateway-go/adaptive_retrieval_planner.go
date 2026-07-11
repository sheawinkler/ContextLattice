package main

import (
	"net/http"
	"sort"
	"strings"
)

const retrievalPlanContractID = "retrieval_plan.v1"

type retrievalObligation struct {
	ID       string
	Label    string
	Required bool
	Sources  []string
}

func (s *server) memoryRetrievalPlan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	s.handleRetrievalPlan(w, r, false)
}

func (s *server) toolsRetrievalPlan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareToolHeaders(w, r, "/tools/retrieval_plan"); !ok {
		return
	}
	s.handleRetrievalPlan(w, r, true)
}

func (s *server) handleRetrievalPlan(w http.ResponseWriter, r *http.Request, tool bool) {
	payload, err := readOptionalJSONBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
		return
	}
	query := strings.TrimSpace(anyToString(payload["query"]))
	if query == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "query is required"})
		return
	}
	response := s.buildAdaptiveRetrievalPlan(payload)
	response["ok"] = true
	if tool {
		response["tool"] = "retrieval_plan"
	}
	writeJSON(w, http.StatusOK, attachPayloadFormatContract(retrievalPlanContractID, response, anyToString(payload["agent_id"]), "retrieval_plan", r.URL.Path))
}

func (s *server) buildAdaptiveRetrievalPlan(payload map[string]any) map[string]any {
	query := strings.TrimSpace(anyToString(payload["query"]))
	project := strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["project"]), anyToString(payload["projectName"])))
	taskPhase := normalizeRetrievalTaskPhase(firstNonEmptyStrings(anyToString(payload["task_phase"]), anyToString(payload["phase"])), query)
	retrievalIntent := strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["retrieval_intent"]), taskPhase))
	retrievalMode := strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["retrieval_mode"]), "balanced"))
	tokenBudget := clampInt(anyToInt(firstNonNil(payload["token_budget"], payload["target_context_pack_tokens"], payload["agent_context_budget_tokens"], payload["max_prompt_tokens"]), 4000), 512, 64000)
	obligations := retrievalObligationsForPhase(taskPhase)
	obligations = appendExplicitRetrievalObligations(obligations, payload["evidence_obligations"])

	order, statsBySource := s.retrievalSourceStatsSnapshot()
	selected := map[string]struct{}{}
	for _, source := range append(append([]string{}, s.retrieval.fastSources...), s.retrieval.defaultSources...) {
		selected[source] = struct{}{}
	}
	if retrievalMode == "deep" || taskPhase == "research" || taskPhase == "release" {
		for _, source := range s.retrieval.slowSources {
			selected[source] = struct{}{}
		}
	}
	for _, obligation := range obligations {
		if !obligation.Required {
			continue
		}
		for _, source := range obligation.Sources {
			if stringSliceContains(order, source) {
				selected[source] = struct{}{}
			}
		}
	}

	type scoredSource struct {
		name        string
		score       float64
		reliability float64
		latency     float64
		observed    bool
		selected    bool
		reason      []string
	}
	scored := make([]scoredSource, 0, len(order))
	for _, source := range order {
		stats := statsBySource[source]
		reliability := 0.72
		observed := stats.Requests > 0
		if observed {
			failures := maxInt(stats.Errors, stats.Timeouts)
			reliability = 1 - (float64(failures) / float64(stats.Requests))
			if reliability < 0 {
				reliability = 0
			}
		}
		base := retrievalSourcePrior(source)
		latencyFactor := 1.0
		if stats.P95Ms > 0 {
			latencyFactor = 1 / (1 + stats.P95Ms/2000)
		}
		score := base*0.5 + reliability*0.35 + latencyFactor*0.15
		_, isSelected := selected[source]
		reasons := []string{retrievalSourceRole(source)}
		if observed {
			reasons = append(reasons, "observed reliability and p95 latency included")
		} else {
			reasons = append(reasons, "no observed sample; conservative prior used")
		}
		if isSelected && observed && reliability < 0.5 && !isProtectedRetrievalSource(s.retrieval, source) {
			isSelected = false
			reasons = append(reasons, "excluded from blocking plan because observed reliability is below 0.50")
		}
		scored = append(scored, scoredSource{name: source, score: score, reliability: reliability, latency: stats.P95Ms, observed: observed, selected: isSelected, reason: reasons})
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].selected != scored[j].selected {
			return scored[i].selected
		}
		if scored[i].score == scored[j].score {
			return scored[i].name < scored[j].name
		}
		return scored[i].score > scored[j].score
	})
	selectedCount := 0
	for _, source := range scored {
		if source.selected {
			selectedCount++
		}
	}
	if selectedCount == 0 && len(scored) > 0 {
		scored[0].selected = true
		selectedCount = 1
	}
	perSourceBudget := maxInt(128, int(float64(tokenBudget)*0.62/float64(maxInt(1, selectedCount))))
	sourcePlan := make([]any, 0, len(scored))
	for _, source := range scored {
		lane := "optional"
		if stringSliceContains(s.retrieval.fastSources, source.name) {
			lane = "fast"
		} else if stringSliceContains(s.retrieval.slowSources, source.name) {
			lane = "slow_async"
		}
		budget := 0
		if source.selected {
			budget = perSourceBudget
		}
		sourcePlan = append(sourcePlan, map[string]any{
			"source": source.name, "lane": lane, "selected": source.selected,
			"score": roundFloat(source.score, 4), "reliability": roundFloat(source.reliability, 4),
			"p95_ms": source.latency, "observed": source.observed, "budget_tokens": budget,
			"reason": source.reason,
		})
	}

	quality := s.contextPackQualityTelemetrySnapshot()
	calibrationOutcomes := anyToInt(quality["calibration_outcome_sample_count"], 0)
	graphRequested := taskPhase == "research" || taskPhase == "debug" || strings.Contains(strings.ToLower(query), "cross-project") || strings.Contains(strings.ToLower(query), "linkage")
	claimRequested := taskPhase != "orient" || strings.Contains(strings.ToLower(query), "decision") || strings.Contains(strings.ToLower(query), "current")
	queryPlan := retrievalQueryPlan(query, taskPhase, obligations)
	return map[string]any{
		"schema_id":            retrievalPlanContractID,
		"version":              1,
		"generated_at":         nowUTCISO(),
		"mode":                 "advisor",
		"activation_state":     "shadow_only",
		"project":              project,
		"query":                query,
		"task_phase":           taskPhase,
		"retrieval_intent":     retrievalIntent,
		"retrieval_mode":       retrievalMode,
		"token_budget":         tokenBudget,
		"evidence_obligations": retrievalObligationMaps(obligations),
		"query_plan":           queryPlan,
		"source_plan":          sourcePlan,
		"expansion": map[string]any{
			"temporal_claims": claimRequested,
			"memory_graph":    graphRequested,
			"cross_project":   graphRequested && strings.TrimSpace(project) != "",
			"max_graph_hops":  1,
			"reason":          "expansion is proposed only; execution remains controlled by the caller",
		},
		"budget_allocation": map[string]any{
			"ranked_evidence_tokens": int(float64(tokenBudget) * 0.62),
			"temporal_claim_tokens":  int(float64(tokenBudget) * 0.14),
			"graph_bridge_tokens":    int(float64(tokenBudget) * 0.09),
			"synthesis_tokens":       tokenBudget - int(float64(tokenBudget)*0.85),
		},
		"stop_conditions": map[string]any{
			"max_rounds":                            3,
			"minimum_obligation_coverage":           0.9,
			"minimum_marginal_value_per_100_tokens": 0.012,
			"stop_on_no_new_supported_claims":       true,
			"stop_on_budget_exhaustion":             true,
		},
		"calibration": map[string]any{
			"outcome_samples":                calibrationOutcomes,
			"grade":                          quality["calibration_grade"],
			"activation_eligible":            false,
			"minimum_outcomes_for_candidate": 20,
			"reason":                         "v3.12 planner is advisor-only; outcome-trained activation requires a later canary contract",
		},
		"proof": map[string]any{
			"inputs":        []any{"configured source policy", "observed source reliability", "observed p95 latency", "context-pack quality calibration", "task-phase evidence obligations"},
			"llm_used":      false,
			"deterministic": true,
		},
	}
}

func normalizeRetrievalTaskPhase(raw string, query string) string {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "orient", "plan", "implement", "verify", "debug", "release", "research":
		return strings.TrimSpace(strings.ToLower(raw))
	}
	lower := strings.ToLower(query)
	for _, rule := range []struct {
		phase string
		terms []string
	}{
		{"release", []string{"release", "publish", "tag ", "version bump", "ship"}},
		{"debug", []string{"debug", "broken", "failure", "error", "fix ", "regression", "why is"}},
		{"verify", []string{"verify", "test ", "audit", "prove", "validation", "check ", "review"}},
		{"implement", []string{"implement", "build ", "edit ", "refactor", "migrate", "add feature"}},
		{"research", []string{"research", "compare", "frontier", "investigate", "explore", "synthesize"}},
		{"plan", []string{"plan", "design", "architect", "roadmap"}},
	} {
		for _, term := range rule.terms {
			if strings.Contains(lower, term) {
				return rule.phase
			}
		}
	}
	return "orient"
}

func retrievalObligationsForPhase(phase string) []retrievalObligation {
	base := []retrievalObligation{
		{ID: "current_state", Label: "current observed state", Required: true, Sources: []string{sourceTopicRollup, sourceQdrant}},
		{ID: "provenance", Label: "source and timestamp provenance", Required: true, Sources: []string{sourceMongoRaw, sourceTopicRollup}},
	}
	switch phase {
	case "plan":
		return append(base, retrievalObligation{ID: "constraints_decisions_risks", Label: "prior decisions, constraints, and known risks", Required: true, Sources: []string{sourceTopicRollup, sourceQdrant}})
	case "implement":
		return append(base, retrievalObligation{ID: "implementation_contract", Label: "files, contracts, runbooks, and acceptance checks", Required: true, Sources: []string{sourceQdrant, sourceTopicRollup}})
	case "verify":
		return append(base, retrievalObligation{ID: "verification_evidence", Label: "acceptance criteria, test output, and deployment identity", Required: true, Sources: []string{sourceMongoRaw, sourceQdrant}})
	case "debug":
		return append(base, retrievalObligation{ID: "failure_chain", Label: "symptom, reproduction, recent change, and known failure modes", Required: true, Sources: []string{sourceQdrant, sourceMongoRaw, sourceMemoryBank}})
	case "release":
		return append(base, retrievalObligation{ID: "release_proof", Label: "version, security, tests, boundary, artifacts, and deployment proof", Required: true, Sources: []string{sourceQdrant, sourceMongoRaw, sourceTopicRollup}})
	case "research":
		return append(base, retrievalObligation{ID: "opposition_and_links", Label: "supporting, opposing, temporal, and cross-project evidence", Required: true, Sources: []string{sourceQdrant, sourceMemoryBank, sourceLetta}})
	default:
		return base
	}
}

func appendExplicitRetrievalObligations(existing []retrievalObligation, raw any) []retrievalObligation {
	seen := map[string]struct{}{}
	for _, item := range existing {
		seen[item.ID] = struct{}{}
	}
	for _, value := range contextPackAnyList(raw) {
		label := clipText(strings.TrimSpace(firstNonEmptyStrings(anyToString(anyMap(value)["label"]), anyToString(value))), 300)
		if label == "" {
			continue
		}
		id := "explicit_" + sha256Hex(strings.ToLower(label))[:12]
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		existing = append(existing, retrievalObligation{ID: id, Label: label, Required: true})
		if len(existing) >= 12 {
			break
		}
	}
	return existing
}

func retrievalObligationMaps(obligations []retrievalObligation) []any {
	out := make([]any, 0, len(obligations))
	for _, item := range obligations {
		out = append(out, map[string]any{"id": item.ID, "label": item.Label, "required": item.Required, "preferred_sources": item.Sources})
	}
	return out
}

func retrievalQueryPlan(query string, phase string, obligations []retrievalObligation) []any {
	out := []any{map[string]any{"query": query, "purpose": "primary task wording", "round": 1}}
	for _, obligation := range obligations {
		if len(out) >= 4 {
			break
		}
		out = append(out, map[string]any{
			"query":   clipText(query+" | "+obligation.Label, 1200),
			"purpose": obligation.ID,
			"round":   2,
		})
	}
	return out
}

func retrievalSourcePrior(source string) float64 {
	switch source {
	case sourceTopicRollup:
		return 0.94
	case sourceQdrant:
		return 0.91
	case sourceMongoRaw:
		return 0.82
	case sourcePgvector, sourceWeaviate:
		return 0.78
	case sourceMemoryBank, sourceLetta:
		return 0.68
	default:
		return 0.6
	}
}

func retrievalSourceRole(source string) string {
	switch source {
	case sourceTopicRollup:
		return "topic gravity and compact project history"
	case sourceQdrant:
		return "primary semantic evidence"
	case sourceMongoRaw:
		return "raw durable provenance"
	case sourceMemoryBank, sourceLetta:
		return "slow deep-recall enrichment"
	case sourcePgvector, sourceWeaviate:
		return "optional parallel semantic lane"
	default:
		return "configured retrieval source"
	}
}

func stringSliceContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func isProtectedRetrievalSource(policy retrievalPolicy, source string) bool {
	_, ok := policy.protectedSources[source]
	return ok
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}
