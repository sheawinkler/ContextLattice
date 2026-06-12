package main

import (
	"fmt"
	"os"
	"strings"
	"unicode"
)

type objectiveContext struct {
	Mission                 string
	Objective               string
	Goal                    string
	ProjectPrimaryObjective string
	TopicObjective          string
	SessionObjective        string
	Subobjectives           []string
}

func defaultObjectiveContext() objectiveContext {
	objective := defaultContextLatticeObjective()
	projectPrimary := strings.TrimSpace(os.Getenv("CONTEXTLATTICE_PROJECT_PRIMARY_OBJECTIVE"))
	if projectPrimary == "" {
		projectPrimary = objective
	}
	return objectiveContext{
		Mission:                 defaultContextLatticeMission(),
		Objective:               objective,
		Goal:                    defaultContextLatticeGoal(),
		ProjectPrimaryObjective: projectPrimary,
		TopicObjective:          strings.TrimSpace(os.Getenv("CONTEXTLATTICE_TOPIC_OBJECTIVE")),
		SessionObjective:        strings.TrimSpace(os.Getenv("CONTEXTLATTICE_SESSION_OBJECTIVE")),
		Subobjectives:           objectiveStringList(os.Getenv("CONTEXTLATTICE_SUBOBJECTIVES"), 12),
	}
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
		strings.TrimSpace(c.Goal) == "" &&
		strings.TrimSpace(c.ProjectPrimaryObjective) == "" &&
		strings.TrimSpace(c.TopicObjective) == "" &&
		strings.TrimSpace(c.SessionObjective) == "" &&
		len(c.Subobjectives) == 0
}

func (c objectiveContext) withDefaults() objectiveContext {
	defaults := defaultObjectiveContext()
	out := objectiveContext{
		Mission:                 strings.TrimSpace(c.Mission),
		Objective:               strings.TrimSpace(c.Objective),
		Goal:                    strings.TrimSpace(c.Goal),
		ProjectPrimaryObjective: strings.TrimSpace(c.ProjectPrimaryObjective),
		TopicObjective:          strings.TrimSpace(c.TopicObjective),
		SessionObjective:        strings.TrimSpace(c.SessionObjective),
		Subobjectives:           objectiveCleanStringList(c.Subobjectives, 12),
	}
	if out.Mission == "" {
		out.Mission = defaults.Mission
	}
	if out.Objective == "" {
		out.Objective = defaults.Objective
	}
	if out.Goal == "" {
		out.Goal = defaults.Goal
	}
	if out.ProjectPrimaryObjective == "" {
		out.ProjectPrimaryObjective = firstNonEmptyStrings(defaults.ProjectPrimaryObjective, out.Objective)
	}
	if out.TopicObjective == "" {
		out.TopicObjective = defaults.TopicObjective
	}
	if out.SessionObjective == "" {
		out.SessionObjective = defaults.SessionObjective
	}
	if len(out.Subobjectives) == 0 {
		out.Subobjectives = objectiveCleanStringList(defaults.Subobjectives, 12)
	}
	return out
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
	if value := strings.TrimSpace(c.ProjectPrimaryObjective); value != "" {
		payload["project_primary_objective"] = value
	}
	if value := strings.TrimSpace(c.TopicObjective); value != "" {
		payload["topic_objective"] = value
	}
	if value := strings.TrimSpace(c.SessionObjective); value != "" {
		payload["session_objective"] = value
	}
	if values := objectiveCleanStringList(c.Subobjectives, 12); len(values) > 0 {
		payload["subobjectives"] = values
	}
	return payload
}

func objectiveCleanStringList(values []string, limit int) []string {
	if limit < 1 {
		limit = 1
	}
	out := []string{}
	seen := map[string]struct{}{}
	for _, value := range values {
		value = clipText(strings.TrimSpace(value), 720)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func objectiveStringList(value any, limit int) []string {
	switch typed := value.(type) {
	case nil:
		return nil
	case []string:
		return objectiveCleanStringList(typed, limit)
	case []any:
		return objectiveCleanStringList(anyToStringList(typed, limit), limit)
	case string:
		raw := strings.TrimSpace(typed)
		if raw == "" {
			return nil
		}
		raw = strings.ReplaceAll(raw, "\r\n", "\n")
		raw = strings.ReplaceAll(raw, ";", "\n")
		raw = strings.ReplaceAll(raw, "|", "\n")
		parts := strings.Split(raw, "\n")
		if len(parts) == 1 {
			parts = strings.Split(raw, ",")
		}
		return objectiveCleanStringList(parts, limit)
	default:
		if rendered := strings.TrimSpace(anyToString(value)); rendered != "" {
			return objectiveCleanStringList([]string{rendered}, limit)
		}
		return nil
	}
}

func mergeObjectiveString(current string, value any) string {
	if strings.TrimSpace(current) != "" {
		return strings.TrimSpace(current)
	}
	return strings.TrimSpace(anyToString(value))
}

func objectiveContextFromHierarchy(source map[string]any) objectiveContext {
	if source == nil {
		return objectiveContext{}
	}
	out := objectiveContext{}
	out.ProjectPrimaryObjective = mergeObjectiveString(out.ProjectPrimaryObjective, source["project_primary_objective"])
	out.ProjectPrimaryObjective = mergeObjectiveString(out.ProjectPrimaryObjective, source["primary_objective"])
	out.TopicObjective = mergeObjectiveString(out.TopicObjective, source["topic_objective"])
	out.SessionObjective = mergeObjectiveString(out.SessionObjective, source["session_objective"])
	out.Objective = mergeObjectiveString(out.Objective, source["objective"])
	out.Mission = mergeObjectiveString(out.Mission, source["mission"])
	out.Goal = mergeObjectiveString(out.Goal, source["goal"])
	out.Subobjectives = objectiveStringList(source["subobjectives"], 12)
	if project := anyMap(source["project"]); len(project) > 0 {
		out.ProjectPrimaryObjective = mergeObjectiveString(out.ProjectPrimaryObjective, project["primary_objective"])
		out.ProjectPrimaryObjective = mergeObjectiveString(out.ProjectPrimaryObjective, project["objective"])
	}
	if topic := anyMap(source["topic"]); len(topic) > 0 {
		out.TopicObjective = mergeObjectiveString(out.TopicObjective, topic["objective"])
	}
	if session := anyMap(source["session"]); len(session) > 0 {
		out.SessionObjective = mergeObjectiveString(out.SessionObjective, session["objective"])
	}
	if current := anyMap(source["current"]); len(current) > 0 {
		out.SessionObjective = mergeObjectiveString(out.SessionObjective, current["objective"])
	}
	return out
}

func extractObjectiveContext(payload map[string]any) objectiveContext {
	out := objectiveContext{}
	merge := func(source map[string]any) {
		if source == nil {
			return
		}
		out.Mission = mergeObjectiveString(out.Mission, source["mission"])
		out.Objective = mergeObjectiveString(out.Objective, source["objective"])
		out.Goal = mergeObjectiveString(out.Goal, source["goal"])
		out.ProjectPrimaryObjective = mergeObjectiveString(out.ProjectPrimaryObjective, source["project_primary_objective"])
		out.ProjectPrimaryObjective = mergeObjectiveString(out.ProjectPrimaryObjective, source["primary_objective"])
		out.TopicObjective = mergeObjectiveString(out.TopicObjective, source["topic_objective"])
		out.SessionObjective = mergeObjectiveString(out.SessionObjective, source["session_objective"])
		if len(out.Subobjectives) == 0 {
			out.Subobjectives = objectiveStringList(source["subobjectives"], 12)
		}
		if nested := anyMap(source["objective_hierarchy"]); len(nested) > 0 {
			hierarchyCtx := objectiveContextFromHierarchy(nested)
			out.Mission = mergeObjectiveString(out.Mission, hierarchyCtx.Mission)
			out.Objective = mergeObjectiveString(out.Objective, hierarchyCtx.Objective)
			out.Goal = mergeObjectiveString(out.Goal, hierarchyCtx.Goal)
			out.ProjectPrimaryObjective = mergeObjectiveString(out.ProjectPrimaryObjective, hierarchyCtx.ProjectPrimaryObjective)
			out.TopicObjective = mergeObjectiveString(out.TopicObjective, hierarchyCtx.TopicObjective)
			out.SessionObjective = mergeObjectiveString(out.SessionObjective, hierarchyCtx.SessionObjective)
			if len(out.Subobjectives) == 0 {
				out.Subobjectives = hierarchyCtx.Subobjectives
			}
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

func objectiveContextFromPreflightRequest(req agentPreflightRequest, payload map[string]any) objectiveContext {
	out := extractObjectiveContext(payload)
	if strings.TrimSpace(req.Mission) != "" {
		out.Mission = req.Mission
	}
	if strings.TrimSpace(req.Objective) != "" {
		out.Objective = req.Objective
	}
	if strings.TrimSpace(req.Goal) != "" {
		out.Goal = req.Goal
	}
	return out.withDefaults()
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

func sanitizeObjectivePathKey(value string, fallback string) string {
	key := sanitizeObjectiveAgentKey(value)
	if key == "unknown_agent" && strings.TrimSpace(fallback) != "" {
		return sanitizeObjectiveAgentKey(fallback)
	}
	return key
}

func objectiveTopicSegments(topicPath string) []any {
	parts := strings.Split(strings.Trim(strings.TrimSpace(topicPath), "/"), "/")
	out := []any{}
	for _, part := range parts {
		part = clipText(strings.TrimSpace(part), 120)
		if part == "" {
			continue
		}
		out = append(out, part)
		if len(out) >= 8 {
			break
		}
	}
	return out
}

func objectiveAnyStringList(values []string, limit int) []any {
	clean := objectiveCleanStringList(values, limit)
	out := make([]any, 0, len(clean))
	for _, value := range clean {
		out = append(out, value)
	}
	return out
}

func objectivePairAlignment(left string, right string) string {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" || right == "" {
		return "missing"
	}
	leftTokens := runAdvisorTokenSet(left)
	rightTokens := runAdvisorTokenSet(right)
	if len(leftTokens) == 0 || len(rightTokens) == 0 {
		return "missing"
	}
	shared := 0
	for token := range leftTokens {
		if _, ok := rightTokens[token]; ok {
			shared++
		}
	}
	denominator := maxInt(1, minInt(len(leftTokens), len(rightTokens)))
	ratio := float64(shared) / float64(denominator)
	switch {
	case ratio >= 0.55:
		return "aligned"
	case ratio >= 0.25:
		return "partial"
	default:
		return "mismatch"
	}
}

func (c objectiveContext) effectiveProjectPrimaryObjective() string {
	withDefaults := c.withDefaults()
	return firstNonEmptyStrings(withDefaults.ProjectPrimaryObjective, withDefaults.Objective, withDefaults.Goal)
}

func (c objectiveContext) effectiveTopicObjective(query string) string {
	withDefaults := c.withDefaults()
	return firstNonEmptyStrings(withDefaults.TopicObjective, withDefaults.Objective, query, withDefaults.Goal)
}

func (c objectiveContext) effectiveSessionObjective(query string) string {
	withDefaults := c.withDefaults()
	return firstNonEmptyStrings(withDefaults.SessionObjective, withDefaults.Objective, query, withDefaults.Goal)
}

func (c objectiveContext) hierarchy(project string, topicPath string, sessionID string, query string) map[string]any {
	withDefaults := c.withDefaults()
	projectPrimary := clipText(c.effectiveProjectPrimaryObjective(), 1200)
	topicObjective := clipText(c.effectiveTopicObjective(query), 1200)
	sessionObjective := clipText(c.effectiveSessionObjective(query), 1200)
	currentLevel := "session"
	currentObjective := sessionObjective
	if currentObjective == "" {
		currentLevel = "topic"
		currentObjective = topicObjective
	}
	if currentObjective == "" {
		currentLevel = "project"
		currentObjective = projectPrimary
	}
	segments := objectiveTopicSegments(topicPath)
	subtopic := ""
	if len(segments) > 0 {
		subtopic = anyToString(segments[len(segments)-1])
	}
	return map[string]any{
		"schema_id": "contextlattice_objective_hierarchy.v1",
		"project": map[string]any{
			"name":              clipText(strings.TrimSpace(project), 160),
			"primary_objective": projectPrimary,
		},
		"topic": map[string]any{
			"topic_path":     clipText(strings.TrimSpace(topicPath), 240),
			"subtopic":       clipText(subtopic, 120),
			"path_segments":  segments,
			"objective":      topicObjective,
			"source":         "topic_path_or_objective_context",
			"fallback_used":  strings.TrimSpace(withDefaults.TopicObjective) == "",
			"fallback_value": "current_objective",
		},
		"session": map[string]any{
			"session_id": clipText(strings.TrimSpace(sessionID), 128),
			"objective":  sessionObjective,
			"query":      clipText(strings.TrimSpace(query), 720),
		},
		"subobjectives": objectiveAnyStringList(withDefaults.Subobjectives, 12),
		"current": map[string]any{
			"level":     currentLevel,
			"objective": clipText(currentObjective, 1200),
		},
	}
}

func (c objectiveContext) lineage(project string, topicPath string, sessionID string, query string) map[string]any {
	hierarchy := c.hierarchy(project, topicPath, sessionID, query)
	projectNode := anyMap(hierarchy["project"])
	topicNode := anyMap(hierarchy["topic"])
	sessionNode := anyMap(hierarchy["session"])
	projectPrimary := anyToString(projectNode["primary_objective"])
	topicObjective := anyToString(topicNode["objective"])
	sessionObjective := anyToString(sessionNode["objective"])
	projectToTopic := objectivePairAlignment(projectPrimary, topicObjective)
	topicToSession := objectivePairAlignment(topicObjective, sessionObjective)
	projectToSession := objectivePairAlignment(projectPrimary, sessionObjective)
	overall := "aligned"
	for _, status := range []string{projectToTopic, topicToSession, projectToSession} {
		if status == "mismatch" {
			overall = "mismatch"
			break
		}
		if status == "partial" && overall != "mismatch" {
			overall = "partial"
		}
		if status == "missing" && overall == "aligned" {
			overall = "partial"
		}
	}
	return map[string]any{
		"schema_id": "contextlattice_objective_lineage.v1",
		"source":    "request_objective_context_with_contextlattice_defaults",
		"precedence": []any{
			"session.objective",
			"topic.objective",
			"project.primary_objective",
			"objective_context.objective",
			"contextlattice.defaults",
		},
		"drift": map[string]any{
			"status":             overall,
			"project_to_topic":   projectToTopic,
			"topic_to_session":   topicToSession,
			"project_to_session": projectToSession,
		},
		"handoff_rule": "carry project primary objective, topic objective, session objective, subobjectives, evidence, and next action into the next agent/model prompt",
	}
}

func renderObjectiveContextContent(project string, agentID string, topicPath string, sessionID string, scope string, context objectiveContext) string {
	hierarchy := context.hierarchy(project, topicPath, sessionID, "")
	lineage := context.lineage(project, topicPath, sessionID, "")
	projectNode := anyMap(hierarchy["project"])
	topicNode := anyMap(hierarchy["topic"])
	sessionNode := anyMap(hierarchy["session"])
	drift := anyMap(lineage["drift"])
	lines := []string{
		"# Objective Context",
		"project: " + strings.TrimSpace(project),
		"agent_id: " + strings.TrimSpace(agentID),
		"scope: " + strings.TrimSpace(scope),
		"topic_path: " + strings.TrimSpace(topicPath),
		"session_id: " + strings.TrimSpace(sessionID),
		"",
		"## Project Primary Objective",
	}
	if value := strings.TrimSpace(anyToString(projectNode["primary_objective"])); value != "" {
		lines = append(lines, value)
	} else {
		lines = append(lines, "(unset)")
	}
	lines = append(lines, "", "## Topic Objective")
	if value := strings.TrimSpace(anyToString(topicNode["objective"])); value != "" {
		lines = append(lines, value)
	} else {
		lines = append(lines, "(unset)")
	}
	lines = append(lines, "", "## Session Objective")
	if value := strings.TrimSpace(anyToString(sessionNode["objective"])); value != "" {
		lines = append(lines, value)
	} else {
		lines = append(lines, "(unset)")
	}
	if subobjectives := objectiveCleanStringList(context.withDefaults().Subobjectives, 12); len(subobjectives) > 0 {
		lines = append(lines, "", "## Subobjectives")
		for _, value := range subobjectives {
			lines = append(lines, "- "+value)
		}
	}
	lines = append(lines, "", "## Current Objective")
	current := anyMap(hierarchy["current"])
	if value := strings.TrimSpace(anyToString(current["objective"])); value != "" {
		lines = append(lines, value)
	} else {
		lines = append(lines, "(unset)")
	}
	lines = append(lines, "", "## Lineage")
	lines = append(lines, "status: "+firstNonEmptyStrings(anyToString(drift["status"]), "unknown"))
	lines = append(lines, "project_to_topic: "+firstNonEmptyStrings(anyToString(drift["project_to_topic"]), "unknown"))
	lines = append(lines, "topic_to_session: "+firstNonEmptyStrings(anyToString(drift["topic_to_session"]), "unknown"))
	lines = append(lines, "", "## Legacy Objective", "### Objective")
	if value := strings.TrimSpace(context.Objective); value != "" {
		lines = append(lines, value)
	} else {
		lines = append(lines, "(unset)")
	}
	lines = append(lines, "", "### Goal")
	if value := strings.TrimSpace(context.Goal); value != "" {
		lines = append(lines, value)
	} else {
		lines = append(lines, "(unset)")
	}
	lines = append(lines, "", "### Mission")
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
	requestTopicPath := strings.TrimSpace(firstNonEmptyStrings(anyToString(requestPayload["topic_path"]), anyToString(requestPayload["topicPath"])))
	sessionID := strings.TrimSpace(firstNonEmptyStrings(anyToString(requestPayload["session_id"]), anyToString(requestPayload["sessionId"])))
	projectKey := sanitizeObjectivePathKey(project, "project")
	topicKey := sanitizeObjectivePathKey(requestTopicPath, "root")
	sessionKey := sanitizeObjectivePathKey(sessionID, "session")
	prefix := objectiveTopicPrefix()
	type writeSpec struct {
		scope     string
		fileName  string
		topicPath string
		tags      []string
	}
	specs := []writeSpec{
		{
			scope:     "agent",
			fileName:  "objectives/agents/" + agentKey + ".md",
			topicPath: prefix + "/agents/" + agentKey,
			tags:      []string{"type:objective_context", "scope:agent", "source:retrieval", "agent:" + agentKey},
		},
		{
			scope:     "project",
			fileName:  "objectives/projects/" + projectKey + ".md",
			topicPath: prefix + "/projects/" + projectKey,
			tags:      []string{"type:objective_context", "scope:project", "source:retrieval", "project:" + projectKey},
		},
	}
	if requestTopicPath != "" {
		specs = append(specs, writeSpec{
			scope:     "topic",
			fileName:  "objectives/topics/" + projectKey + "/" + topicKey + ".md",
			topicPath: prefix + "/topics/" + projectKey + "/" + topicKey,
			tags:      []string{"type:objective_context", "scope:topic", "source:retrieval", "project:" + projectKey, "topic:" + topicKey},
		})
	}
	if sessionID != "" {
		specs = append(specs, writeSpec{
			scope:     "session",
			fileName:  "objectives/sessions/" + sessionKey + ".md",
			topicPath: prefix + "/sessions/" + sessionKey,
			tags:      []string{"type:objective_context", "scope:session", "source:retrieval", "agent:" + agentKey, "session:" + sessionKey},
		})
	}
	writes := []any{}
	allDeduped := true
	for _, spec := range specs {
		content := renderObjectiveContextContent(project, agentID, requestTopicPath, sessionID, spec.scope, context)
		item := normalizedWrite{
			project:   project,
			fileName:  spec.fileName,
			content:   content,
			topicPath: spec.topicPath,
			agentID:   agentID,
			tags:      spec.tags,
			createdAt: nowUTCISO(),
		}
		entry, deduped, err := s.memoryStore.put(item)
		if err != nil {
			result["status"] = "error"
			result["reason"] = "write_failed"
			result["scope"] = spec.scope
			return result, err
		}
		if !deduped {
			allDeduped = false
		}
		write := map[string]any{
			"scope":       spec.scope,
			"file":        spec.fileName,
			"topic_path":  spec.topicPath,
			"deduped":     deduped,
			"event_id":    entry.EventID,
			"content_ref": entry.ContentRef,
		}
		writes = append(writes, write)
		if _, present := result["file"]; !present {
			result["file"] = spec.fileName
			result["topic_path"] = spec.topicPath
			result["deduped"] = deduped
			result["event_id"] = entry.EventID
			result["content_ref"] = entry.ContentRef
		}
	}
	result["status"] = "recorded"
	result["project"] = project
	result["writes"] = writes
	result["write_count"] = len(writes)
	if allDeduped {
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

func buildObjectiveRuntimeState(
	agent string,
	agentID string,
	project string,
	topicPath string,
	query string,
	retrievalMode string,
	sessionID string,
	requestedContext objectiveContext,
	actionExecuted string,
) map[string]any {
	policyContext := requestedContext.withDefaults()
	if strings.TrimSpace(actionExecuted) == "" {
		actionExecuted = "objective_runtime_contract_built"
	}
	objectiveHierarchy := policyContext.hierarchy(project, topicPath, sessionID, query)
	objectiveLineage := policyContext.lineage(project, topicPath, sessionID, query)
	payload := map[string]any{
		"version":             "2026-06-12",
		"agent":               strings.TrimSpace(agent),
		"agent_id":            strings.TrimSpace(agentID),
		"project":             strings.TrimSpace(project),
		"session_id":          strings.TrimSpace(sessionID),
		"objective_state":     "active",
		"mission":             policyContext.Mission,
		"objective":           policyContext.Objective,
		"goal":                policyContext.Goal,
		"objective_hierarchy": objectiveHierarchy,
		"objective_lineage":   objectiveLineage,
		"scoreboard": map[string]any{
			"primary_kpi":   firstNonEmptyStrings(os.Getenv("CONTEXTLATTICE_PRIMARY_KPI"), "agent makes measurable progress toward the requested objective"),
			"guardrail_kpi": firstNonEmptyStrings(os.Getenv("CONTEXTLATTICE_GUARDRAIL_KPI"), "outputs stay contract-valid, bounded, evidence-grounded, and generic across agent runners"),
			"cadence_kpi":   firstNonEmptyStrings(os.Getenv("CONTEXTLATTICE_CADENCE_KPI"), "each preflight, context pack, checkpoint, handoff, and completion emits objective/session evidence"),
		},
		"action_executed": actionExecuted,
		"evidence": map[string]any{
			"required": []any{
				"retrieved_context_or_explicit_no_data",
				"deterministic_check_or_artifact_inspection",
				"checkpoint_or_session_event_for_handoff",
			},
			"current": []any{
				map[string]any{
					"kind":           "preflight_contract",
					"query":          clipText(strings.TrimSpace(query), 720),
					"topic_path":     strings.TrimSpace(topicPath),
					"retrieval_mode": strings.TrimSpace(retrievalMode),
					"session_id":     strings.TrimSpace(sessionID),
				},
			},
		},
		"objective_delta": map[string]any{
			"before": "objective state unproven until agent records an executed action with evidence",
			"after":  "agent has a bounded objective runtime contract and session path for subsequent events",
		},
		"risk_or_blocker": map[string]any{
			"status":                "none_reported",
			"fastest_recovery_path": "run preflight or contextlattice-session ensure, then attach the returned session_id to context, checkpoint, and handoff calls",
		},
		"next_action": "execute the smallest useful action, verify it with matching artifacts, and emit a session event or checkpoint before handoff",
	}
	metadata := contractMetadata(objectiveRuntimeStateContractID)
	payload["format_contract"] = metadata
	enforceAgentBoundaryContract(objectiveRuntimeStateContractID, payload)
	findings := validateAgentContractPayload(objectiveRuntimeStateContractID, payload)
	payload["format_contract"] = stampContractValidation(metadata, findings)
	enforceAgentBoundaryContract(objectiveRuntimeStateContractID, payload)
	findings = validateAgentContractPayload(objectiveRuntimeStateContractID, payload)
	payload["format_contract"] = stampContractValidation(metadata, findings)
	return payload
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
	objectiveRuntime map[string]any,
	requestedContext objectiveContext,
) map[string]any {
	policyContext := requestedContext.withDefaults()
	mission := policyContext.Mission
	objective := policyContext.Objective
	goal := policyContext.Goal
	objectiveHierarchy := policyContext.hierarchy(project, topicPath, anyToString(anyMap(objectiveRuntime)["session_id"]), query)
	objectiveLineage := policyContext.lineage(project, topicPath, anyToString(anyMap(objectiveRuntime)["session_id"]), query)
	formatContract := contractMetadata(policyContextPackageContractID)
	if objectiveRuntime == nil {
		objectiveRuntime = buildObjectiveRuntimeState(agent, agentID, project, topicPath, query, retrievalMode, "", requestedContext, "policy_context_package_built")
		objectiveHierarchy = anyMap(objectiveRuntime["objective_hierarchy"])
		objectiveLineage = anyMap(objectiveRuntime["objective_lineage"])
	} else {
		if runtimeHierarchy := anyMap(objectiveRuntime["objective_hierarchy"]); len(runtimeHierarchy) > 0 {
			objectiveHierarchy = runtimeHierarchy
		}
		if runtimeLineage := anyMap(objectiveRuntime["objective_lineage"]); len(runtimeLineage) > 0 {
			objectiveLineage = runtimeLineage
		}
	}
	policy := map[string]any{
		"version":             "2026-06-12",
		"agent":               agent,
		"agent_id":            agentID,
		"project":             project,
		"topic_path":          topicPath,
		"query":               query,
		"retrieval_mode":      retrievalMode,
		"mission":             mission,
		"objective":           objective,
		"goal":                goal,
		"objective_hierarchy": objectiveHierarchy,
		"objective_lineage":   objectiveLineage,
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
			"anti_scheming_required":            true,
			"objective_runtime_required":        true,
			"checkpoint_during_execution":       true,
			"final_recency_pass_required":       true,
			"include_grounding":                 true,
			"include_retrieval_debug":           true,
			"broaden_scope_on_zero_or_degraded": true,
			"format_validation_required":        true,
			"contract_boundary_validated":       true,
			"fail_closed_on_contract_violation": true,
		},
		"objective_runtime":      objectiveRuntime,
		"anti_scheming_protocol": antiSchemingProtocol(),
		"handoff": map[string]any{
			"disperse_to_agents": true,
			"handoff_prompt": strings.TrimSpace(
				"Project primary objective: " + anyToString(anyMap(objectiveHierarchy["project"])["primary_objective"]) + "\n" +
					"Topic objective: " + anyToString(anyMap(objectiveHierarchy["topic"])["objective"]) + "\n" +
					"Session objective: " + anyToString(anyMap(objectiveHierarchy["session"])["objective"]) + "\n" +
					"Mission: " + mission + "\n" +
					"Objective: " + objective + "\n" +
					"Goal: " + goal + "\n" +
					"Policy: retrieve before inference, checkpoint key decisions, run final recency retrieval, and change conclusions to match evidence.",
			),
		},
		"evidence": map[string]any{
			"primary_facts":      contextPackEvidence(primaryPack, 8),
			"mission_facts":      contextPackEvidence(missionPack, 8),
			"mission_pack_error": nil,
		},
		"format_contract": formatContract,
	}
	if missionPackError != nil {
		policyEvidence, _ := policy["evidence"].(map[string]any)
		policyEvidence["mission_pack_error"] = missionPackError.Error()
	}
	findings := validateAgentContractPayload(antiSchemingContractID, policy["anti_scheming_protocol"])
	findings = append(findings, validateAgentContractPayload(policyContextPackageContractID, policy)...)
	policy["format_contract"] = stampContractValidation(formatContract, findings)
	return policy
}
