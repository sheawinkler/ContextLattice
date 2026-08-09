package main

import (
	"sort"
	"strings"
	"time"
)

func continuousCognitionStableValue(value any, depth int) any {
	if depth > 8 {
		return "[depth-clipped]"
	}
	switch typed := value.(type) {
	case map[string]any:
		result := map[string]any{}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			switch strings.ToLower(key) {
			case "timestamp", "exact_timestamp", "generated_at", "created_at", "updated_at", "projection_ms", "as_of":
				continue
			}
			result[key] = continuousCognitionStableValue(typed[key], depth+1)
		}
		return result
	case []any:
		result := make([]any, 0, len(typed))
		for _, item := range typed {
			result = append(result, continuousCognitionStableValue(item, depth+1))
		}
		return result
	case []map[string]any:
		result := make([]any, 0, len(typed))
		for _, item := range typed {
			result = append(result, continuousCognitionStableValue(item, depth+1))
		}
		return result
	default:
		return value
	}
}

func continuousCognitionStableDigest(value any) string {
	return frontierT6Digest(continuousCognitionStableValue(value, 0))
}

// continuousCognitionHistoricalSession rebuilds only the session state that is
// evidenced by events at or before the requested boundary. The session store
// keeps a mutable latest row, so using that row after a later event would make
// an old cognition proof change when the future-only state changes.
func continuousCognitionHistoricalSession(row map[string]any, events []map[string]any, asOf time.Time) map[string]any {
	projected := map[string]any{
		"id":                  strings.TrimSpace(anyToString(row["id"])),
		"started_at":          anyToString(row["started_at"]),
		"status":              "active",
		"completed_at":        "",
		"last_event_type":     "session.started",
		"last_event_at":       anyToString(row["started_at"]),
		"updated_at":          anyToString(row["started_at"]),
		"event_count":         0,
		"memory_contribution": map[string]any{},
	}
	for _, event := range events {
		metadata := anyMap(event["metadata"])
		identity := proofTimelineIdentityFromMaps(
			event,
			metadata,
			anyMap(metadata["agent_state"]),
			anyMap(metadata["ownership"]),
		)
		for _, key := range []string{"agent_id", "project", "task_id", "task_identity_id", "execution_lane_id"} {
			if strings.TrimSpace(anyToString(projected[key])) == "" && strings.TrimSpace(anyToString(identity[key])) != "" {
				projected[key] = identity[key]
			}
		}
		if strings.TrimSpace(anyToString(projected["agent"])) == "" && strings.TrimSpace(anyToString(event["agent"])) != "" {
			projected["agent"] = event["agent"]
		}

		eventType := strings.TrimSpace(anyToString(event["type"]))
		createdAt := anyToString(event["created_at"])
		projected["last_event_type"] = eventType
		projected["last_event_at"] = createdAt
		projected["updated_at"] = createdAt
		projected["event_count"] = anyToInt(projected["event_count"], 0) + 1
		projected["memory_contribution"] = bumpContribution(
			anyMap(projected["memory_contribution"]),
			eventType,
			metadata,
			createdAt,
		)
		if objectiveState := strings.TrimSpace(anyToString(metadata["objective_state"])); objectiveState != "" {
			projected["objective_state"] = clipText(objectiveState, 80)
		}
		if nextAction := strings.TrimSpace(anyToString(metadata["next_action"])); nextAction != "" {
			projected["next_action"] = clipText(nextAction, 720)
		}
		if statePayload := anyMap(metadata["agent_state"]); len(statePayload) > 0 {
			projected["agent_state"] = normalizeAgentLifecyclePayload(statePayload, anyToString(event["status"]))
		}
		switch strings.ToLower(eventType) {
		case "session.completed", "agent.session.completed":
			projected["status"] = "completed"
			projected["completed_at"] = createdAt
		case "session.failed", "agent.session.failed":
			projected["status"] = "failed"
			projected["completed_at"] = createdAt
		case "session.blocked", "agent.session.blocked":
			projected["status"] = "blocked"
			projected["completed_at"] = ""
		case "session.canceled", "agent.session.canceled":
			projected["status"] = "canceled"
			projected["completed_at"] = createdAt
		default:
			if !agentSessionTerminal(anyToString(projected["status"])) {
				if state := anyToString(anyMap(projected["agent_state"])["state"]); state != "" {
					projected["status"] = normalizeAgentSessionStatus(state)
				} else {
					projected["status"] = "active"
				}
			}
		}
	}
	projected["as_of"] = asOf.UTC().Format(time.RFC3339Nano)
	return projected
}

func continuousCognitionSessionAt(store *agentSessionStore, sessionID string, asOf time.Time) (map[string]any, []map[string]any, bool, bool) {
	return continuousCognitionSessionAtVisible(store, sessionID, asOf, nil)
}

func continuousCognitionSessionAtVisible(
	store *agentSessionStore,
	sessionID string,
	asOf time.Time,
	visible func(map[string]any) bool,
) (map[string]any, []map[string]any, bool, bool) {
	if store == nil || strings.TrimSpace(sessionID) == "" || asOf.IsZero() {
		return nil, nil, false, false
	}
	sessionID = strings.TrimSpace(sessionID)
	store.mu.RLock()
	row, ok := store.sessions[sessionID]
	if !ok {
		store.mu.RUnlock()
		return nil, nil, false, false
	}
	row = cloneAnyMap(row)
	retainedEvents := make([]map[string]any, 0, len(store.events[sessionID]))
	for _, event := range store.events[sessionID] {
		retainedEvents = append(retainedEvents, cloneAnyMap(event))
	}
	idleTTL := store.idleTTL
	store.mu.RUnlock()
	// Evaluate the visibility predicate on the immutable clone. A route
	// predicate is allowed to consult other state without acquiring this
	// session-store lock, so it cannot create a lock-order inversion.
	if visible != nil && !visible(row) {
		return nil, nil, false, false
	}
	startedAt, startedOK := parseTimeBestEffort(anyToString(row["started_at"]))
	if startedOK && startedAt.After(asOf.UTC()) {
		return nil, nil, false, true
	}
	temporalComplete := startedOK
	if updatedAt, parsed := parseTimeBestEffort(firstNonEmptyStrings(anyToString(row["updated_at"]), anyToString(row["last_event_at"]))); !parsed || updatedAt.After(asOf.UTC()) {
		temporalComplete = false
	}
	events := make([]map[string]any, 0, len(retainedEvents))
	for _, event := range retainedEvents {
		createdAt, parsed := parseTimeBestEffort(anyToString(event["created_at"]))
		if !parsed {
			temporalComplete = false
			continue
		}
		if createdAt.After(asOf.UTC()) {
			continue
		}
		events = append(events, event)
	}
	if temporalComplete {
		return agentSessionEffectiveSnapshot(row, idleTTL, asOf.UTC()), events, true, true
	}
	projected := continuousCognitionHistoricalSession(row, events, asOf)
	return agentSessionEffectiveSnapshot(projected, idleTTL, asOf.UTC()), events, true, false
}

func continuousCognitionObjectiveProjection(graph map[string]any, objectiveID string) (string, string, bool, bool) {
	if len(graph) == 0 || !anyToBool(graph["ok"]) {
		return "", continuousCognitionUnavailableRef("objective_graph"), false, false
	}
	nodes, ok := graph["nodes"].([]objectiveGraphNode)
	if !ok {
		return "", continuousCognitionUnavailableRef("objective_graph"), false, false
	}
	orderedNodes := append([]objectiveGraphNode(nil), nodes...)
	sort.SliceStable(orderedNodes, func(i, j int) bool {
		return orderedNodes[i].ObjectiveID < orderedNodes[j].ObjectiveID
	})
	stableNodes := make([]any, 0, len(orderedNodes))
	state := "unknown"
	available := false
	for _, node := range orderedNodes {
		if node.ObjectiveID == objectiveID {
			state = strings.TrimSpace(node.Status)
			available = true
		}
		stableNodes = append(stableNodes, map[string]any{
			"objective_id":        node.ObjectiveID,
			"status":              node.Status,
			"parent_objective_id": node.ParentObjectiveID,
			"task_identity_ids":   continuousCognitionSortedStrings(node.TaskIdentityIDs),
			"execution_lane_ids":  continuousCognitionSortedStrings(node.ExecutionLaneIDs),
			"session_ids":         continuousCognitionSortedStrings(node.SessionIDs),
			"decision_change_ids": continuousCognitionSortedStrings(node.DecisionChangeIDs),
			"outcome_ids":         continuousCognitionSortedStrings(node.OutcomeIDs),
			"checkpoint_ids":      continuousCognitionSortedStrings(node.CheckpointIDs),
		})
	}
	stableEdges := make([]any, 0)
	if edges, ok := graph["edges"].([]objectiveGraphEdge); ok {
		orderedEdges := append([]objectiveGraphEdge(nil), edges...)
		sort.SliceStable(orderedEdges, func(i, j int) bool {
			return orderedEdges[i].EdgeID < orderedEdges[j].EdgeID
		})
		for _, edge := range orderedEdges {
			stableEdges = append(stableEdges, map[string]any{
				"edge_id": edge.EdgeID, "from_id": edge.FromID, "to_id": edge.ToID, "type": edge.Type,
			})
		}
	}
	material := map[string]any{
		"complete": graph["complete"], "graph_truncated": graph["graph_truncated"],
		"node_count": graph["node_count"], "edge_count": graph["edge_count"],
		"transition_count": graph["transition_count"], "nodes": stableNodes, "edges": stableEdges,
	}
	return state, continuousCognitionDigestPrefix("ref_objective_graph_", material), available, anyToBool(graph["complete"])
}

func continuousCognitionSessionProjection(session map[string]any, events []map[string]any, asOf time.Time) string {
	rollup := buildAgentSessionRollup(session, events, asOf.UTC())
	stable := map[string]any{}
	for _, key := range []string{"session_id", "agent_id", "status", "objective_state", "next_action", "confidence", "source_coverage", "risk_summary", "artifact_summary", "event_count", "retained_event_count"} {
		if value, exists := rollup[key]; exists {
			stable[key] = continuousCognitionStableValue(value, 0)
		}
	}
	return continuousCognitionDigestPrefix("ref_session_rollup_", stable)
}
