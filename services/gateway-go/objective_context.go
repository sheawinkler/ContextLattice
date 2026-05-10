package main

import (
	"fmt"
	"os"
	"strings"
	"unicode"
)

type objectiveContext struct {
	Mission   string
	Objective string
	Goal      string
}

func defaultContextLatticeMission() string {
	value := strings.TrimSpace(os.Getenv("CONTEXTLATTICE_MISSION"))
	if value == "" {
		value = "Compound knowledge across projects into better agent outcomes with less repeated inference."
	}
	return value
}

func defaultContextLatticeObjective() string {
	value := strings.TrimSpace(os.Getenv("CONTEXTLATTICE_OBJECTIVE"))
	if value == "" {
		value = "Improve longitudinal recall, retrieval quality, and orchestration decisions over time."
	}
	return value
}

func defaultContextLatticeGoal() string {
	value := strings.TrimSpace(os.Getenv("CONTEXTLATTICE_GOAL"))
	if value == "" {
		value = "Maximize useful context per token while preserving correctness, provenance, and latency discipline."
	}
	return value
}

func (c objectiveContext) empty() bool {
	return strings.TrimSpace(c.Mission) == "" &&
		strings.TrimSpace(c.Objective) == "" &&
		strings.TrimSpace(c.Goal) == ""
}

func (c objectiveContext) toMap() map[string]any {
	payload := map[string]any{}
	if value := strings.TrimSpace(c.Mission); value != "" {
		payload["mission"] = value
	}
	if value := strings.TrimSpace(c.Objective); value != "" {
		payload["objective"] = value
	}
	if value := strings.TrimSpace(c.Goal); value != "" {
		payload["goal"] = value
	}
	return payload
}

func extractObjectiveContext(payload map[string]any) objectiveContext {
	out := objectiveContext{}
	merge := func(source map[string]any) {
		if source == nil {
			return
		}
		if strings.TrimSpace(out.Mission) == "" {
			out.Mission = strings.TrimSpace(anyToString(source["mission"]))
		}
		if strings.TrimSpace(out.Objective) == "" {
			out.Objective = strings.TrimSpace(anyToString(source["objective"]))
		}
		if strings.TrimSpace(out.Goal) == "" {
			out.Goal = strings.TrimSpace(anyToString(source["goal"]))
		}
	}
	merge(payload)
	if nested, ok := payload["policy_context_package"].(map[string]any); ok {
		merge(nested)
	}
	if nested, ok := payload["objective_context"].(map[string]any); ok {
		merge(nested)
	}
	if nested, ok := payload["context"].(map[string]any); ok {
		merge(nested)
	}
	return out
}

func objectiveContextCaptureEnabled() bool {
	return envBool("GO_OBJECTIVE_CONTEXT_CAPTURE_ENABLED", true)
}

func objectiveTopicPrefix() string {
	prefix := strings.TrimSpace(os.Getenv("GO_OBJECTIVE_CONTEXT_TOPIC_PREFIX"))
	if prefix == "" {
		prefix = "runbooks/objectives"
	}
	return sanitizeTopicPath(prefix, "runbooks/objectives")
}

func sanitizeObjectiveAgentKey(agentID string) string {
	normalized := strings.TrimSpace(strings.ToLower(agentID))
	if normalized == "" {
		return "unknown_agent"
	}
	var b strings.Builder
	b.Grow(len(normalized))
	lastUnderscore := false
	for _, r := range normalized {
		valid := unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == '_'
		if valid {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteRune('_')
			lastUnderscore = true
		}
	}
	safe := strings.Trim(b.String(), "._-")
	if safe == "" {
		safe = "unknown_agent"
	}
	return safe
}

func renderObjectiveContextContent(project string, agentID string, context objectiveContext) string {
	lines := []string{
		"# Agent Objective Context",
		"project: " + strings.TrimSpace(project),
		"agent_id: " + strings.TrimSpace(agentID),
		"",
		"## Objective",
	}
	if value := strings.TrimSpace(context.Objective); value != "" {
		lines = append(lines, value)
	} else {
		lines = append(lines, "(unset)")
	}
	lines = append(lines, "", "## Goal")
	if value := strings.TrimSpace(context.Goal); value != "" {
		lines = append(lines, value)
	} else {
		lines = append(lines, "(unset)")
	}
	lines = append(lines, "", "## Mission")
	if value := strings.TrimSpace(context.Mission); value != "" {
		lines = append(lines, value)
	} else {
		lines = append(lines, "(unset)")
	}
	lines = append(lines, "")
	return strings.Join(lines, "\n")
}

func (s *server) captureObjectiveContextDatapoint(
	requestPayload map[string]any,
	context objectiveContext,
) (map[string]any, error) {
	result := map[string]any{
		"enabled": objectiveContextCaptureEnabled(),
		"status":  "skipped",
	}
	if !objectiveContextCaptureEnabled() {
		result["reason"] = "capture_disabled"
		return result, nil
	}
	if context.empty() {
		result["reason"] = "missing_objective_context"
		return result, nil
	}
	if s.memoryStore == nil || !s.memoryStore.policy.enabled {
		result["reason"] = "memory_store_unavailable"
		return result, nil
	}
	project := strings.TrimSpace(anyToString(requestPayload["project"]))
	if project == "" {
		result["reason"] = "missing_project"
		return result, nil
	}
	agentID := strings.TrimSpace(anyToString(requestPayload["agent_id"]))
	if agentID == "" {
		agentID = strings.TrimSpace(os.Getenv("CONTEXTLATTICE_AGENT_ID"))
	}
	if agentID == "" {
		agentID = "unknown_agent"
	}
	agentKey := sanitizeObjectiveAgentKey(agentID)
	topicPath := objectiveTopicPrefix() + "/" + agentKey
	fileName := "objectives/agents/" + agentKey + ".md"
	content := renderObjectiveContextContent(project, agentID, context)
	item := normalizedWrite{
		project:   project,
		fileName:  fileName,
		content:   content,
		topicPath: topicPath,
		agentID:   agentID,
		tags: []string{
			"type:objective_context",
			"source:retrieval",
			"agent:" + agentKey,
		},
		createdAt: nowUTCISO(),
	}
	entry, deduped, err := s.memoryStore.put(item)
	if err != nil {
		result["status"] = "error"
		result["reason"] = "write_failed"
		return result, err
	}
	result["status"] = "recorded"
	result["project"] = project
	result["file"] = fileName
	result["topic_path"] = topicPath
	result["deduped"] = deduped
	result["event_id"] = entry.EventID
	result["content_ref"] = entry.ContentRef
	if deduped {
		result["status"] = "already_present"
	}
	return result, nil
}

func objectiveContextWarning(capture map[string]any, err error) string {
	if err != nil {
		return "Objective context capture failed: " + err.Error()
	}
	status := strings.TrimSpace(anyToString(capture["status"]))
	if status == "" || status == "skipped" || status == "already_present" || status == "recorded" {
		return ""
	}
	reason := strings.TrimSpace(anyToString(capture["reason"]))
	if reason == "" {
		return "Objective context capture status=" + status
	}
	return fmt.Sprintf("Objective context capture status=%s reason=%s", status, reason)
}

func contextPackEvidence(pack map[string]any, maxItems int) []map[string]any {
	rows := []map[string]any{}
	if maxItems < 1 {
		maxItems = 1
	}
	if maxItems > 32 {
		maxItems = 32
	}
	if pack == nil {
		return rows
	}
	nested, _ := pack["context_pack"].(map[string]any)
	appendRows := func(items []any, textField string) {
		for _, raw := range items {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			text := strings.TrimSpace(anyToString(item[textField]))
			if text == "" && textField != "summary" {
				text = strings.TrimSpace(anyToString(item["summary"]))
			}
			if text == "" {
				continue
			}
			rows = append(rows, map[string]any{
				"text":       text,
				"source":     nilIfEmpty(strings.TrimSpace(anyToString(item["source"]))),
				"topic_path": nilIfEmpty(strings.TrimSpace(anyToString(item["topic_path"]))),
				"score":      item["score"],
			})
			if len(rows) >= maxItems {
				return
			}
		}
	}
	if facts, ok := nested["facts"].([]any); ok {
		appendRows(facts, "text")
	}
	if len(rows) < maxItems {
		if results, ok := nested["results"].([]any); ok {
			appendRows(results, "summary")
		}
	}
	return rows
}

func buildPolicyContextPackage(
	agent string,
	agentID string,
	project string,
	topicPath string,
	query string,
	retrievalMode string,
	primaryPack map[string]any,
	missionPack map[string]any,
	missionPackError error,
) map[string]any {
	mission := defaultContextLatticeMission()
	objective := defaultContextLatticeObjective()
	goal := defaultContextLatticeGoal()
	policy := map[string]any{
		"version":        "2026-05-10",
		"agent":          agent,
		"agent_id":       agentID,
		"project":        project,
		"topic_path":     topicPath,
		"query":          query,
		"retrieval_mode": retrievalMode,
		"mission":        mission,
		"objective":      objective,
		"goal":           goal,
		"skills": map[string]any{
			"required": []string{"objective", "goal"},
			"optional": []string{"mission"},
			"availability": map[string]any{
				"objective": true,
				"goal":      true,
				"mission":   false,
			},
		},
		"policy_contract": map[string]any{
			"retrieve_before_inference":         true,
			"checkpoint_during_execution":       true,
			"final_recency_pass_required":       true,
			"include_grounding":                 true,
			"include_retrieval_debug":           true,
			"broaden_scope_on_zero_or_degraded": true,
		},
		"handoff": map[string]any{
			"disperse_to_agents": true,
			"handoff_prompt": strings.TrimSpace(
				"Mission: " + mission + "\n" +
					"Objective: " + objective + "\n" +
					"Goal: " + goal + "\n" +
					"Policy: retrieve before inference, checkpoint key decisions, and run final recency retrieval.",
			),
		},
		"evidence": map[string]any{
			"primary_facts": contextPackEvidence(primaryPack, 8),
			"mission_facts": contextPackEvidence(missionPack, 8),
		},
	}
	if missionPackError != nil {
		policyEvidence, _ := policy["evidence"].(map[string]any)
		policyEvidence["mission_pack_error"] = missionPackError.Error()
	}
	return policy
}
