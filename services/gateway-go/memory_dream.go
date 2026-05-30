package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type dreamModeOptions struct {
	Goal                  string
	Query                 string
	Project               string
	TopicPath             string
	RetrievalMode         string
	AgentID               string
	RiskTolerance         string
	NoveltyLevel          int
	MaxHypotheses         int
	Limit                 int
	MaxFacts              int
	UseLLM                bool
	Persist               bool
	Model                 string
	Provider              string
	BaseURL               string
	APIKey                string
	IncludeRetrievalDebug bool
}

func (s *server) memoryDream(w http.ResponseWriter, r *http.Request) {
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
	response, status := s.buildDreamModeResponse(r.Context(), incomingHeaders, payload, "/memory/dream")
	writeJSON(w, status, response)
}

func (s *server) toolsDream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	incomingHeaders, ok := s.prepareToolHeaders(w, r, "/tools/dream")
	if !ok {
		return
	}
	payload, err := readDreamOptionalJSONBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
		return
	}
	response, status := s.buildDreamModeResponse(r.Context(), incomingHeaders, payload, "/tools/dream")
	response["tool"] = "dream"
	response = attachPayloadFormatContract(dreamModeResponseContractID, response, anyToString(response["agent_id"]), "dream", "/tools/dream")
	writeJSON(w, status, response)
}

func (s *server) buildDreamModeResponse(
	ctx context.Context,
	incomingHeaders http.Header,
	payload map[string]any,
	endpoint string,
) (map[string]any, int) {
	opts := normalizeDreamModeOptions(payload)
	if strings.TrimSpace(opts.Goal) == "" && strings.TrimSpace(opts.Query) == "" {
		return attachDreamModeFormatContract(dreamModeMissingGoalResponse(opts), endpoint), http.StatusUnprocessableEntity
	}

	contextPayload := dreamContextPackRequest(opts, payload)
	contextResponse, contextStatus, contextErr := s.buildContextPackResponse(ctx, incomingHeaders, contextPayload)
	warnings := parseWarnings(contextResponse["warnings"])
	if contextErr != nil {
		warnings = append(warnings, "Dream Mode retrieval failed: "+sanitizeProviderOverflowText(contextErr.Error()))
		contextResponse = emptyDreamContextPackResponse(opts, warnings)
		contextStatus = http.StatusBadGateway
	}
	if contextStatus >= 400 {
		warnings = append(warnings, fmt.Sprintf("Dream Mode retrieval returned status %d.", contextStatus))
	}

	evidence := dreamEvidenceFromContextPack(contextResponse, opts.MaxFacts, opts.Limit)
	hypotheses := dreamDeterministicHypotheses(opts, evidence)
	experiments := dreamExperimentsFromHypotheses(hypotheses)
	llm := s.dreamLLMSynthesis(opts, evidence, hypotheses)
	if suggestions := dreamHypothesesFromLLM(llm, opts); len(suggestions) > 0 {
		for _, suggestion := range suggestions {
			if len(hypotheses) >= opts.MaxHypotheses {
				break
			}
			hypotheses = append(hypotheses, suggestion)
		}
		experiments = dreamExperimentsFromHypotheses(hypotheses)
	}

	sourceCoverage := anyMap(contextResponse["source_coverage"])
	if len(sourceCoverage) == 0 {
		sourceCoverage = map[string]any{
			"configured": []any{},
			"returned":   []any{},
			"complete":   false,
		}
	}
	response := map[string]any{
		"ok":                 contextErr == nil,
		"mode":               "dream",
		"project":            opts.Project,
		"goal":               opts.Goal,
		"query":              opts.Query,
		"topic_path":         opts.TopicPath,
		"retrieval_mode":     opts.RetrievalMode,
		"novelty_level":      opts.NoveltyLevel,
		"risk_tolerance":     opts.RiskTolerance,
		"agent_id":           opts.AgentID,
		"hypotheses":         hypotheses,
		"experiments":        experiments,
		"evidence":           evidence,
		"source_coverage":    sourceCoverage,
		"llm":                llm,
		"warnings":           warnings,
		"writeback_required": true,
		"persisted":          false,
		"tool_use": map[string]any{
			"memory_context_pack": true,
			"backend_llm":         anyToBool(llm["enabled"]),
			"llm_used":            anyToBool(llm["used"]),
			"retrieval_sources":   sourceCoverage["configured"],
		},
	}
	if opts.IncludeRetrievalDebug {
		if retrievalDebug, ok := contextResponse["retrieval"]; ok {
			response["retrieval"] = retrievalDebug
		}
	}
	if opts.Persist {
		persistPayload, persistStatus := s.persistDreamReport(ctx, incomingHeaders, opts, response, endpoint)
		response["persisted"] = anyToBool(persistPayload["ok"])
		response["writeback"] = persistPayload
		if persistStatus >= 400 {
			response["warnings"] = append(parseWarnings(response["warnings"]), fmt.Sprintf("Dream Mode writeback returned status %d.", persistStatus))
		}
	}
	status := http.StatusOK
	if contextErr != nil {
		status = http.StatusBadGateway
	}
	return attachDreamModeFormatContract(response, endpoint), status
}

func normalizeDreamModeOptions(payload map[string]any) dreamModeOptions {
	goal := firstNonEmptyStrings(
		anyToString(payload["goal"]),
		anyToString(payload["objective"]),
		anyToString(payload["mission"]),
		anyToString(payload["query"]),
	)
	query := firstNonEmptyStrings(anyToString(payload["query"]), goal)
	topicPath := firstNonEmptyStrings(
		anyToString(payload["topic_path"]),
		anyToString(payload["topic"]),
		envStringAny("contextlattice/dream-mode", "GO_DREAM_TOPIC_PATH", "CONTEXTLATTICE_DREAM_TOPIC_PATH"),
	)
	retrievalMode := normalizeRetrievalMode(firstNonEmptyStrings(
		anyToString(payload["retrieval_mode"]),
		envStringAny("balanced", "GO_DREAM_RETRIEVAL_MODE", "ORCH_RETRIEVAL_MODE_DEFAULT"),
	))
	maxHypotheses := clampInt(anyToInt(payload["max_hypotheses"], envInt("GO_DREAM_MAX_HYPOTHESES", 4)), 1, 8)
	return dreamModeOptions{
		Goal:                  clipText(goal, 2000),
		Query:                 clipText(query, 2000),
		Project:               strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["project"]), anyToString(payload["project_name"]))),
		TopicPath:             topicPath,
		RetrievalMode:         retrievalMode,
		AgentID:               strings.TrimSpace(anyToString(payload["agent_id"])),
		RiskTolerance:         normalizeDreamRisk(anyToString(payload["risk_tolerance"])),
		NoveltyLevel:          clampInt(anyToInt(payload["novelty_level"], envInt("GO_DREAM_NOVELTY_LEVEL", 3)), 1, 5),
		MaxHypotheses:         maxHypotheses,
		Limit:                 clampInt(anyToInt(payload["limit"], envInt("GO_DREAM_CONTEXT_LIMIT", 12)), 1, 24),
		MaxFacts:              clampInt(anyToInt(payload["max_facts"], envInt("GO_DREAM_MAX_FACTS", 16)), 1, 32),
		UseLLM:                anyToBoolOrDefault(payload["use_llm"], envBool("GO_DREAM_LLM_ENABLED", true)),
		Persist:               anyToBoolOrDefault(firstPresent(payload, "persist", "writeback"), envBool("GO_DREAM_PERSIST_DEFAULT", false)),
		Model:                 firstNonEmptyStrings(anyToString(payload["model"]), envStringAny("qwen3.5:9b", "GO_DREAM_MODEL", "TASK_MODEL")),
		Provider:              firstNonEmptyStrings(anyToString(payload["provider"]), anyToString(payload["llm_provider"]), envStringAny("auto", "GO_DREAM_PROVIDER", "ORCH_INFER_PROVIDER", "TASK_MODEL_PROVIDER")),
		BaseURL:               strings.TrimSpace(anyToString(payload["base_url"])),
		APIKey:                strings.TrimSpace(anyToString(payload["api_key"])),
		IncludeRetrievalDebug: anyToBool(payload["include_retrieval_debug"]),
	}
}

func firstPresent(payload map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := payload[key]; ok {
			return value
		}
	}
	return nil
}

func normalizeDreamRisk(value string) string {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "safe", "low", "conservative":
		return "conservative"
	case "high", "relaxed", "bold":
		return "relaxed"
	case "experimental", "wild", "max":
		return "experimental"
	default:
		return "balanced"
	}
}

func dreamContextPackRequest(opts dreamModeOptions, payload map[string]any) map[string]any {
	searchQuery := strings.TrimSpace(opts.Query)
	if searchQuery == "" {
		searchQuery = opts.Goal
	}
	searchQuery = strings.TrimSpace(searchQuery + " nonlinear synthesis hypotheses experiments contradictions adjacent possibilities")
	request := map[string]any{
		"project":                 opts.Project,
		"query":                   searchQuery,
		"topic_path":              opts.TopicPath,
		"retrieval_mode":          opts.RetrievalMode,
		"retrieval_intent":        "decision",
		"limit":                   opts.Limit,
		"max_facts":               opts.MaxFacts,
		"agent_id":                opts.AgentID,
		"include_retrieval_debug": opts.IncludeRetrievalDebug,
		"combined_sources":        anyToBoolOrDefault(payload["combined_sources"], true),
	}
	for _, key := range []string{
		"sources",
		"source_weights",
		"auto_escalate",
		"query_expansion",
		"include_ephemeral",
		"include_ephemeral_memory",
		"include_test_memory",
		"blocking",
		"wait_for_slow_sources",
		"sync_slow_sources",
		"user_id",
		"traffic_class",
	} {
		if value, present := payload[key]; present {
			request[key] = value
		}
	}
	return request
}

func emptyDreamContextPackResponse(opts dreamModeOptions, warnings []string) map[string]any {
	return map[string]any{
		"ok":       false,
		"query":    opts.Query,
		"warnings": warnings,
		"context_pack": map[string]any{
			"facts":               []any{},
			"numeric_facts":       []any{},
			"citations":           []any{},
			"results":             []any{},
			"relevant_decisions":  []any{},
			"files_to_read":       []any{},
			"files_to_avoid":      []any{},
			"capabilities_to_use": []any{},
			"runbooks":            []any{},
			"known_failure_modes": []any{},
			"commands":            []any{},
			"acceptance_criteria": []any{},
		},
		"source_coverage": map[string]any{
			"configured": []any{},
			"returned":   []any{},
			"failed":     []any{"context_pack"},
			"complete":   false,
		},
	}
}

func dreamEvidenceFromContextPack(contextResponse map[string]any, maxFacts int, maxResults int) map[string]any {
	pack := anyMap(contextResponse["context_pack"])
	facts := dreamEvidenceItems(pack["facts"], "fact", maxFacts)
	results := dreamEvidenceItems(pack["results"], "result", maxResults)
	citations := dreamEvidenceItems(pack["citations"], "citation", maxResults)
	combined := make([]any, 0, len(facts)+len(results))
	combined = append(combined, facts...)
	combined = append(combined, results...)
	return map[string]any{
		"facts":     facts,
		"results":   results,
		"citations": citations,
		"combined":  combined,
		"counts": map[string]any{
			"facts":              dreamListLen(pack["facts"]),
			"results":            dreamListLen(pack["results"]),
			"citations":          dreamListLen(pack["citations"]),
			"returned_evidence":  len(combined),
			"rendered_facts":     len(facts),
			"rendered_results":   len(results),
			"rendered_citations": len(citations),
		},
	}
}

func dreamEvidenceItems(raw any, kind string, maxItems int) []any {
	items, ok := asAnySlice(raw)
	if !ok {
		return []any{}
	}
	maxItems = clampInt(maxItems, 1, 64)
	out := make([]any, 0, minInt(len(items), maxItems))
	for _, rawItem := range items {
		if len(out) >= maxItems {
			break
		}
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		text := firstNonEmptyStrings(
			anyToString(item["text"]),
			anyToString(item["summary"]),
			anyToString(item["file"]),
			anyToString(item["source"]),
		)
		if strings.TrimSpace(text) == "" {
			continue
		}
		id := fmt.Sprintf("e%d", len(out)+1)
		rendered := map[string]any{
			"id":         id,
			"kind":       kind,
			"text":       clipText(text, 700),
			"project":    nilIfEmpty(strings.TrimSpace(anyToString(item["project"]))),
			"file":       nilIfEmpty(strings.TrimSpace(anyToString(item["file"]))),
			"source":     nilIfEmpty(strings.TrimSpace(anyToString(item["source"]))),
			"topic_path": nilIfEmpty(strings.TrimSpace(anyToString(item["topic_path"]))),
			"score":      anyToFloat(item["score"]),
			"timestamp":  item["timestamp"],
		}
		out = append(out, rendered)
	}
	return out
}

func dreamListLen(raw any) int {
	items, ok := asAnySlice(raw)
	if !ok {
		return 0
	}
	return len(items)
}

func dreamDeterministicHypotheses(opts dreamModeOptions, evidence map[string]any) []any {
	combined, _ := evidence["combined"].([]any)
	out := make([]any, 0, opts.MaxHypotheses)
	if len(combined) == 0 {
		return []any{map[string]any{
			"id":                  "h1",
			"type":                "evidence_gap",
			"title":               "Map the missing evidence before betting on a novel direction",
			"claim":               "The current memory surface did not return enough evidence for a strong nonlinear synthesis, so the next useful move is a scout query/writeback pass.",
			"why_novel":           "This treats the absence of evidence as signal instead of forcing a fabricated idea.",
			"supporting_evidence": []any{},
			"contradictions":      []any{"No retrieved facts or results were available for this Dream Mode request."},
			"experiment":          "Run a broader context pack with slow sources enabled, then write back the strongest missing artifacts before rerunning Dream Mode.",
			"expected_signal":     "At least three evidence rows from two or more memory sources become available.",
			"novelty_score":       dreamScore(0.25 + float64(opts.NoveltyLevel)*0.06),
			"feasibility_score":   0.8,
			"risk_score":          dreamRiskScore(opts.RiskTolerance),
			"confidence":          0.2,
			"speculative":         true,
		}}
	}
	for idx := 0; idx < opts.MaxHypotheses && idx < len(combined); idx++ {
		left, _ := combined[idx].(map[string]any)
		right, _ := combined[(idx+1)%len(combined)].(map[string]any)
		if len(combined) == 1 {
			right = left
		}
		leftText := anyToString(left["text"])
		rightText := anyToString(right["text"])
		leftTheme := dreamTheme(leftText, anyToString(left["file"]), anyToString(left["source"]))
		rightTheme := dreamTheme(rightText, anyToString(right["file"]), anyToString(right["source"]))
		title := clipText(fmt.Sprintf("Combine %s with %s", leftTheme, rightTheme), 120)
		support := []any{left["id"]}
		if anyToString(right["id"]) != "" && anyToString(right["id"]) != anyToString(left["id"]) {
			support = append(support, right["id"])
		}
		confidence := 0.38 + (float64(len(support)) * 0.12)
		if anyToString(left["source"]) != "" && anyToString(left["source"]) != anyToString(right["source"]) {
			confidence += 0.08
		}
		out = append(out, map[string]any{
			"id":                  fmt.Sprintf("h%d", idx+1),
			"type":                "cross_evidence_synthesis",
			"title":               title,
			"claim":               clipText(fmt.Sprintf("For %s, treat '%s' as the constraint and '%s' as the lever. The combined move may expose an approach that a linear recall pass would miss.", opts.Goal, leftTheme, rightTheme), 700),
			"why_novel":           dreamWhyNovel(opts.NoveltyLevel, leftTheme, rightTheme),
			"supporting_evidence": support,
			"contradictions":      dreamContradictions(leftText, rightText),
			"experiment":          dreamExperiment(opts, leftTheme, rightTheme),
			"expected_signal":     dreamExpectedSignal(leftTheme, rightTheme),
			"novelty_score":       dreamScore(0.32 + float64(opts.NoveltyLevel)*0.12 + float64(idx)*0.03),
			"feasibility_score":   dreamScore(0.82 - float64(opts.NoveltyLevel)*0.06 + dreamFeasibilityOffset(opts.RiskTolerance)),
			"risk_score":          dreamRiskScore(opts.RiskTolerance),
			"confidence":          dreamScore(confidence),
			"speculative":         true,
		})
	}
	return out
}

func dreamTheme(text string, fileName string, source string) string {
	terms := queryTerms(text, 4)
	filtered := make([]string, 0, len(terms))
	stop := map[string]bool{
		"the": true, "and": true, "for": true, "with": true, "that": true, "this": true, "from": true, "into": true,
		"contextlattice": true, "memory": true,
	}
	for _, term := range terms {
		if len(term) < 4 || stop[term] {
			continue
		}
		filtered = append(filtered, term)
		if len(filtered) >= 3 {
			break
		}
	}
	if len(filtered) > 0 {
		return strings.Join(filtered, " ")
	}
	if strings.TrimSpace(fileName) != "" {
		return strings.TrimSpace(fileName)
	}
	if strings.TrimSpace(source) != "" {
		return strings.TrimSpace(source)
	}
	return "retrieved signal"
}

func dreamWhyNovel(noveltyLevel int, leftTheme string, rightTheme string) string {
	if noveltyLevel >= 4 {
		return clipText(fmt.Sprintf("It deliberately crosses two memory signals that are usually handled separately: %s and %s.", leftTheme, rightTheme), 500)
	}
	return clipText(fmt.Sprintf("It reuses existing evidence but changes the ordering: prove %s through the lens of %s.", leftTheme, rightTheme), 500)
}

func dreamContradictions(leftText string, rightText string) []any {
	joined := strings.ToLower(leftText + " " + rightText)
	out := []any{}
	for _, marker := range []string{"risk", "fail", "timeout", "blocked", "degraded", "rollback", "regression"} {
		if strings.Contains(joined, marker) {
			out = append(out, "Evidence mentions "+marker+"; validate before scaling the idea.")
		}
	}
	if len(out) == 0 {
		out = append(out, "No direct contradiction was retrieved; treat that as unverified, not proven safe.")
	}
	return out
}

func dreamExperiment(opts dreamModeOptions, leftTheme string, rightTheme string) string {
	switch opts.RiskTolerance {
	case "conservative":
		return clipText(fmt.Sprintf("Prototype a read-only audit that measures whether %s predicts useful choices when paired with %s.", leftTheme, rightTheme), 600)
	case "relaxed", "experimental":
		return clipText(fmt.Sprintf("Run a bounded spike that lets an agent use %s as a planning rule and %s as the validation target, then compare against the normal workflow.", leftTheme, rightTheme), 600)
	default:
		return clipText(fmt.Sprintf("Add a small harness around %s and %s, then keep the change only if the signal improves without new failure modes.", leftTheme, rightTheme), 600)
	}
}

func dreamExpectedSignal(leftTheme string, rightTheme string) string {
	return clipText(fmt.Sprintf("The run produces a reusable decision, measurable recall improvement, or a falsifiable reason to avoid combining %s with %s.", leftTheme, rightTheme), 500)
}

func dreamRiskScore(riskTolerance string) float64 {
	switch riskTolerance {
	case "conservative":
		return 0.25
	case "relaxed":
		return 0.55
	case "experimental":
		return 0.72
	default:
		return 0.4
	}
}

func dreamFeasibilityOffset(riskTolerance string) float64 {
	switch riskTolerance {
	case "conservative":
		return 0.08
	case "experimental":
		return -0.1
	default:
		return 0
	}
}

func dreamScore(value float64) float64 {
	if value < 0 {
		value = 0
	}
	if value > 1 {
		value = 1
	}
	return float64(int(value*100+0.5)) / 100
}

func dreamExperimentsFromHypotheses(hypotheses []any) []any {
	experiments := make([]any, 0, len(hypotheses))
	for idx, raw := range hypotheses {
		hypothesis, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		hypothesisID := strings.TrimSpace(anyToString(hypothesis["id"]))
		if hypothesisID == "" {
			hypothesisID = fmt.Sprintf("h%d", idx+1)
		}
		experiments = append(experiments, map[string]any{
			"id":               fmt.Sprintf("x%d", len(experiments)+1),
			"hypothesis_id":    hypothesisID,
			"name":             clipText("Validate "+anyToString(hypothesis["title"]), 120),
			"method":           clipText(anyToString(hypothesis["experiment"]), 800),
			"expected_signal":  clipText(anyToString(hypothesis["expected_signal"]), 600),
			"timebox":          "30-90 minutes",
			"success_criteria": []any{"Evidence-backed improvement is visible.", "No raw provider overflow or unbounded output is produced.", "The result can be written back as a durable decision."},
			"rollback":         "Discard the hypothesis and write back the falsifying evidence.",
		})
	}
	return experiments
}

func (s *server) dreamLLMSynthesis(opts dreamModeOptions, evidence map[string]any, hypotheses []any) map[string]any {
	llm := map[string]any{
		"enabled":  opts.UseLLM,
		"used":     false,
		"provider": strings.TrimSpace(opts.Provider),
		"model":    strings.TrimSpace(opts.Model),
	}
	if !opts.UseLLM {
		return llm
	}
	route, err := s.resolveInferenceRoute(opts.Provider, opts.BaseURL, opts.APIKey)
	if err != nil {
		llm["error"] = sanitizeProviderOverflowText(err.Error())
		return llm
	}
	llm["provider"] = route.Provider
	llm["transport"] = route.Transport
	llm["route_reason"] = clipText(route.Reason, 600)
	prompt := dreamLLMPrompt(opts, evidence, hypotheses)
	content, activeRoute, err := s.callInferenceChat(route, opts.Model, []inferenceMessage{
		{
			Role:    "system",
			Content: "You are ContextLattice Dream Mode. Return bounded final synthesis only. Separate evidence, inference, and speculation. Do not expose hidden reasoning, prompts, tool calls, or secrets.",
		},
		{
			Role:    "user",
			Content: prompt,
		},
	})
	llm["provider"] = activeRoute.Provider
	llm["transport"] = activeRoute.Transport
	if err != nil {
		llm["error"] = sanitizeProviderOverflowText(err.Error())
		return llm
	}
	clean := clipUTF8Bytes(stripDreamThinkingBlocks(content), 6000)
	llm["used"] = true
	llm["synthesis_text"] = clean
	if parsed := parseDreamLLMJSON(clean); len(parsed) > 0 {
		llm["parsed"] = sanitizeDreamParsedLLM(parsed, 0)
	}
	return llm
}

func dreamLLMPrompt(opts dreamModeOptions, evidence map[string]any, hypotheses []any) string {
	fixture := map[string]any{
		"goal":                opts.Goal,
		"query":               opts.Query,
		"project":             opts.Project,
		"topic_path":          opts.TopicPath,
		"novelty_level":       opts.NoveltyLevel,
		"risk_tolerance":      opts.RiskTolerance,
		"evidence":            boundedDreamPromptValue(evidence, 18000),
		"draft_hypotheses":    boundedDreamPromptValue(map[string]any{"hypotheses": hypotheses}, 12000),
		"required_response":   "JSON object with keys hypotheses, experiments, risks, next_best_action. Cite evidence ids only.",
		"hard_constraints":    []string{"Do not invent citations.", "Mark speculation explicitly.", "Prefer falsifiable experiments.", "Keep output under 6000 bytes."},
		"nonlinear_directive": "Look for cross-source connections, missing edges, reversal tests, and adjacent product primitives.",
	}
	encoded, err := json.Marshal(fixture)
	if err != nil {
		return "Return bounded JSON hypotheses for: " + opts.Goal
	}
	return string(encoded)
}

func boundedDreamPromptValue(value any, maxBytes int) any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return map[string]any{}
	}
	if len(encoded) <= maxBytes {
		return value
	}
	var clipped any
	if err := json.Unmarshal([]byte(clipUTF8Bytes(string(encoded), maxBytes)), &clipped); err == nil {
		return clipped
	}
	return map[string]any{"clipped_json": clipUTF8Bytes(string(encoded), maxBytes)}
}

func stripDreamThinkingBlocks(text string) string {
	out := strings.TrimSpace(text)
	for {
		lower := strings.ToLower(out)
		start := strings.Index(lower, "<think>")
		if start < 0 {
			break
		}
		endRelative := strings.Index(lower[start:], "</think>")
		if endRelative < 0 {
			out = strings.TrimSpace(out[:start])
			break
		}
		end := start + endRelative + len("</think>")
		out = strings.TrimSpace(out[:start] + out[end:])
	}
	return strings.TrimSpace(out)
}

func parseDreamLLMJSON(text string) map[string]any {
	candidate := strings.TrimSpace(text)
	if strings.HasPrefix(candidate, "```") {
		lines := strings.Split(candidate, "\n")
		if len(lines) >= 3 {
			candidate = strings.Join(lines[1:len(lines)-1], "\n")
		}
	}
	if !strings.HasPrefix(strings.TrimSpace(candidate), "{") {
		start := strings.Index(candidate, "{")
		end := strings.LastIndex(candidate, "}")
		if start >= 0 && end > start {
			candidate = candidate[start : end+1]
		}
	}
	decoder := json.NewDecoder(strings.NewReader(candidate))
	decoder.UseNumber()
	parsed := map[string]any{}
	if err := decoder.Decode(&parsed); err != nil {
		return map[string]any{}
	}
	return parsed
}

func sanitizeDreamParsedLLM(value any, depth int) any {
	if depth > 8 {
		return nil
	}
	forbidden := map[string]bool{
		"hookSpecificOutput":      true,
		"messages":                true,
		"tool_calls":              true,
		"function_call":           true,
		"raw_contextlattice_json": true,
		"raw_prompt":              true,
		"secret":                  true,
		"secrets":                 true,
	}
	switch typed := value.(type) {
	case map[string]any:
		out := map[string]any{}
		for key, item := range typed {
			if forbidden[key] {
				continue
			}
			cleaned := sanitizeDreamParsedLLM(item, depth+1)
			if cleaned != nil {
				out[key] = cleaned
			}
		}
		return out
	case []any:
		limit := minInt(len(typed), 16)
		out := make([]any, 0, limit)
		for idx := 0; idx < limit; idx++ {
			cleaned := sanitizeDreamParsedLLM(typed[idx], depth+1)
			if cleaned != nil {
				out = append(out, cleaned)
			}
		}
		return out
	case string:
		return clipUTF8Bytes(sanitizeProviderOverflowText(typed), 2000)
	default:
		return value
	}
}

func dreamHypothesesFromLLM(llm map[string]any, opts dreamModeOptions) []any {
	parsed := anyMap(llm["parsed"])
	rawHypotheses, ok := asAnySlice(parsed["hypotheses"])
	if !ok {
		return []any{}
	}
	out := []any{}
	for _, raw := range rawHypotheses {
		if len(out) >= 2 {
			break
		}
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		title := clipText(firstNonEmptyStrings(anyToString(item["title"]), anyToString(item["name"])), 120)
		claim := clipText(firstNonEmptyStrings(anyToString(item["claim"]), anyToString(item["summary"]), anyToString(item["idea"])), 700)
		if title == "" || claim == "" {
			continue
		}
		support := dreamSupportEvidenceList(firstPresent(item, "supporting_evidence", "evidence", "evidence_ids"))
		out = append(out, map[string]any{
			"id":                  fmt.Sprintf("h-llm-%d", len(out)+1),
			"type":                "llm_synthesis",
			"title":               title,
			"claim":               claim,
			"why_novel":           clipText(firstNonEmptyStrings(anyToString(item["why_novel"]), "The backend LLM identified this as a cross-memory synthesis candidate."), 500),
			"supporting_evidence": support,
			"contradictions":      dreamSupportEvidenceList(firstPresent(item, "contradictions", "risks")),
			"experiment":          clipText(firstNonEmptyStrings(anyToString(item["experiment"]), anyToString(item["test"]), anyToString(item["validation"])), 700),
			"expected_signal":     clipText(firstNonEmptyStrings(anyToString(item["expected_signal"]), anyToString(item["success_signal"]), "A measurable change appears in the target workflow."), 500),
			"novelty_score":       dreamScore(0.45 + float64(opts.NoveltyLevel)*0.1),
			"feasibility_score":   dreamScore(0.62 + dreamFeasibilityOffset(opts.RiskTolerance)),
			"risk_score":          dreamRiskScore(opts.RiskTolerance),
			"confidence":          0.45,
			"speculative":         true,
		})
	}
	return out
}

func dreamSupportEvidenceList(value any) []any {
	rows := anyToStringList(value, 8)
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, clipText(row, 160))
	}
	return out
}

func (s *server) persistDreamReport(
	ctx context.Context,
	incomingHeaders http.Header,
	opts dreamModeOptions,
	response map[string]any,
	endpoint string,
) (map[string]any, int) {
	topicPath := firstNonEmptyStrings(opts.TopicPath, "contextlattice/dream-mode")
	project := opts.Project
	if strings.TrimSpace(project) == "" {
		project = "contextlattice"
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	payload := canonicalDreamWritePayload(map[string]any{
		"projectName": project,
		"topicPath":   topicPath,
		"fileName":    "dreams/" + stamp + ".md",
		"content":     renderDreamReportMarkdown(response),
		"agent_id":    opts.AgentID,
		"lifecycle":   "durable",
		"tags":        []any{"kind:dream_mode", "mode:dream"},
	}, topicPath, "dreams/dream")
	return s.commitDreamMemoryWrite(ctx, incomingHeaders, payload, endpoint)
}

func renderDreamReportMarkdown(response map[string]any) string {
	var buf bytes.Buffer
	buf.WriteString("# ContextLattice Dream Mode\n\n")
	buf.WriteString("Goal: " + clipText(anyToString(response["goal"]), 1000) + "\n\n")
	buf.WriteString("Query: " + clipText(anyToString(response["query"]), 1000) + "\n\n")
	if hypotheses, ok := response["hypotheses"].([]any); ok {
		buf.WriteString("## Hypotheses\n")
		for _, raw := range hypotheses {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			buf.WriteString("- " + clipText(anyToString(item["title"]), 160) + ": " + clipText(anyToString(item["claim"]), 500) + "\n")
		}
		buf.WriteString("\n")
	}
	if experiments, ok := response["experiments"].([]any); ok {
		buf.WriteString("## Experiments\n")
		for _, raw := range experiments {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			buf.WriteString("- " + clipText(anyToString(item["name"]), 160) + ": " + clipText(anyToString(item["method"]), 500) + "\n")
		}
	}
	return clipUTF8Bytes(buf.String(), 20000)
}

func dreamModeMissingGoalResponse(opts dreamModeOptions) map[string]any {
	return map[string]any{
		"ok":                 false,
		"mode":               "dream",
		"project":            opts.Project,
		"goal":               "",
		"query":              "",
		"topic_path":         opts.TopicPath,
		"retrieval_mode":     opts.RetrievalMode,
		"novelty_level":      opts.NoveltyLevel,
		"risk_tolerance":     opts.RiskTolerance,
		"agent_id":           opts.AgentID,
		"error":              "goal_or_query_required",
		"instructions":       "Send JSON with a non-empty goal or query. Optional fields: project, topic_path, novelty_level, risk_tolerance, use_llm, max_hypotheses.",
		"hypotheses":         []any{},
		"experiments":        []any{},
		"evidence":           map[string]any{"facts": []any{}, "results": []any{}, "citations": []any{}, "counts": map[string]any{}},
		"source_coverage":    map[string]any{"configured": []any{}, "returned": []any{}, "complete": false},
		"llm":                map[string]any{"enabled": opts.UseLLM, "used": false, "provider": opts.Provider, "model": opts.Model},
		"writeback_required": true,
	}
}

func attachDreamModeFormatContract(payload map[string]any, endpoint string) map[string]any {
	return attachPayloadFormatContract(
		dreamModeResponseContractID,
		payload,
		anyToString(payload["agent_id"]),
		"dream",
		endpoint,
	)
}

func readDreamOptionalJSONBody(r *http.Request) (map[string]any, error) {
	bodyBytes, err := readRequestBody(r)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(bodyBytes)) == "" {
		return map[string]any{}, nil
	}
	return parseJSONMap(bodyBytes)
}

func canonicalDreamWritePayload(payload map[string]any, defaultTopic string, defaultFilePrefix string) map[string]any {
	out := cloneMap(payload)
	if strings.TrimSpace(anyToString(out["projectName"])) == "" {
		out["projectName"] = firstNonEmptyStrings(anyToString(out["project"]), anyToString(out["project_name"]))
	}
	if strings.TrimSpace(anyToString(out["fileName"])) == "" {
		out["fileName"] = firstNonEmptyStrings(anyToString(out["file"]), anyToString(out["file_name"]))
	}
	if strings.TrimSpace(anyToString(out["topicPath"])) == "" {
		out["topicPath"] = firstNonEmptyStrings(anyToString(out["topic_path"]), anyToString(out["topic"]))
	}
	if strings.TrimSpace(anyToString(out["topicPath"])) == "" {
		out["topicPath"] = defaultTopic
	}
	if strings.TrimSpace(anyToString(out["fileName"])) == "" && defaultFilePrefix != "" {
		stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
		out["fileName"] = strings.TrimRight(defaultFilePrefix, "-") + "-" + stamp + ".md"
	}
	return out
}

func (s *server) commitDreamMemoryWrite(
	ctx context.Context,
	incomingHeaders http.Header,
	payload map[string]any,
	endpoint string,
) (map[string]any, int) {
	item, err := normalizeWritePayload("/memory/write", payload)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}, http.StatusBadRequest
	}
	item.lifecycle = "durable"
	if err := s.writePolicy.validateWrite(item); err != nil {
		return map[string]any{"ok": false, "error": err.Error()}, http.StatusUnprocessableEntity
	}
	if s.memoryStore != nil && s.memoryStore.policy.enabled {
		entry, deduped, storeErr := s.memoryStore.put(item)
		if storeErr != nil {
			return map[string]any{"ok": false, "error": "memory store write failed", "detail": storeErr.Error()}, http.StatusBadGateway
		}
		fanout := map[string]any{
			"go_memory_store": "succeeded",
			"python_backend":  "disabled",
		}
		fanoutStatus, warnings := s.handlePgvectorWriteFanout(item, entry.EventID)
		if strings.TrimSpace(fanoutStatus) != "" {
			fanout["postgres_pgvector"] = fanoutStatus
		}
		return map[string]any{
			"ok":               true,
			"tool":             normalizeToolPath(endpoint),
			"event_id":         entry.EventID,
			"source":           "go_memory_store",
			"lifecycle":        item.lifecycle,
			"content_hash":     entry.ContentHash,
			"content_ref":      entry.ContentRef,
			"warnings":         warnings,
			"rollup_buffered":  true,
			"deduped":          deduped,
			"fanout":           fanout,
			"writeback_source": "dream_mode",
		}, http.StatusOK
	}
	response, status, backendErr := s.callBackendJSON(ctx, incomingHeaders, http.MethodPost, "/memory/write", payload)
	if backendErr != nil {
		return map[string]any{
			"ok":         false,
			"error":      "backend unavailable",
			"detail":     sanitizeProviderOverflowText(backendErr.Error()),
			"backendUrl": s.backendURL,
		}, http.StatusBadGateway
	}
	response["tool"] = normalizeToolPath(endpoint)
	response["writeback_source"] = "dream_mode"
	return response, status
}
