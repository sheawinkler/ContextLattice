package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type AgentEventMeta struct {
	AgentID   string   `json:"agent_id,omitempty"`
	SessionID string   `json:"session_id,omitempty"`
	Project   string   `json:"project,omitempty"`
	TopicPath string   `json:"topic_path,omitempty"`
	FileName  string   `json:"file,omitempty"`
	EventID   string   `json:"event_id,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	Summary   string   `json:"summary,omitempty"`
	CreatedAt string   `json:"created_at,omitempty"`
}

type AgentEventEdge struct {
	EdgeID    string         `json:"edge_id"`
	SourceID  string         `json:"source_id"`
	TargetID  string         `json:"target_id"`
	Relation  string         `json:"relation"`
	Project   string         `json:"project,omitempty"`
	TopicPath string         `json:"topic_path,omitempty"`
	EventID   string         `json:"event_id,omitempty"`
	AgentID   string         `json:"agent_id,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
	Weight    int            `json:"weight,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	CreatedAt string         `json:"created_at,omitempty"`
}

func deriveAgentEventMeta(entry memoryStoreEntry) AgentEventMeta {
	return AgentEventMeta{
		AgentID:   strings.TrimSpace(entry.AgentID),
		SessionID: strings.TrimSpace(entry.SessionID),
		Project:   strings.TrimSpace(entry.Project),
		TopicPath: strings.Trim(strings.TrimSpace(entry.TopicPath), "/"),
		FileName:  strings.TrimSpace(entry.FileName),
		EventID:   strings.TrimSpace(entry.EventID),
		Tags:      append([]string{}, entry.Tags...),
		Summary:   strings.TrimSpace(entry.Summary),
		CreatedAt: strings.TrimSpace(entry.CreatedAt),
	}
}

func deriveAgentEventEdges(entry memoryStoreEntry) []AgentEventEdge {
	meta := deriveAgentEventMeta(entry)
	if meta.Project == "" {
		return []AgentEventEdge{}
	}
	createdAt := meta.CreatedAt
	if createdAt == "" {
		createdAt = nowUTCISO()
	}
	edges := []AgentEventEdge{}
	add := func(sourceID string, relation string, targetID string, weight int, metadata map[string]any) {
		sourceID = strings.TrimSpace(sourceID)
		targetID = strings.TrimSpace(targetID)
		relation = strings.TrimSpace(relation)
		if sourceID == "" || targetID == "" || relation == "" {
			return
		}
		edges = append(edges, AgentEventEdge{
			EdgeID:    stableAgentEventEdgeID(sourceID, relation, targetID, meta.EventID),
			SourceID:  sourceID,
			TargetID:  targetID,
			Relation:  relation,
			Project:   meta.Project,
			TopicPath: meta.TopicPath,
			EventID:   meta.EventID,
			AgentID:   meta.AgentID,
			SessionID: meta.SessionID,
			Weight:    maxInt(weight, 1),
			Metadata:  metadata,
			CreatedAt: createdAt,
		})
	}

	agentNode := ""
	if meta.AgentID != "" {
		agentNode = "agent:" + meta.AgentID
		add(agentNode, "agent_touched_project", "project:"+meta.Project, 4, nil)
		if meta.TopicPath != "" {
			add(agentNode, "agent_touched_topic", "topic:"+meta.Project+"/"+meta.TopicPath, 6, nil)
		}
		if meta.FileName != "" {
			add(agentNode, "agent_touched_file", "file:"+meta.Project+"/"+meta.FileName, 5, nil)
		}
		if meta.SessionID != "" {
			add(agentNode, "agent_participated_session", "session:"+meta.SessionID, 3, nil)
		}
	}
	if meta.TopicPath != "" && meta.FileName != "" {
		add("topic:"+meta.Project+"/"+meta.TopicPath, "topic_mentions_file", "file:"+meta.Project+"/"+meta.FileName, 2, nil)
	}
	for _, tool := range extractAgentToolTags(meta.Tags) {
		if agentNode != "" {
			add(agentNode, "agent_used_tool", "tool:"+tool, 2, map[string]any{"tag": tool})
		}
	}
	lowerSummary := strings.ToLower(meta.Summary)
	if agentNode != "" && (strings.Contains(lowerSummary, "finding") || strings.Contains(lowerSummary, "blocker") || strings.Contains(lowerSummary, "regression")) {
		target := "project:" + meta.Project
		if meta.TopicPath != "" {
			target = "topic:" + meta.Project + "/" + meta.TopicPath
		}
		add(agentNode, "agent_contributed_finding", target, 4, map[string]any{"summary": clipUTF8Bytes(meta.Summary, 240)})
	}
	if agentNode != "" && meta.FileName != "" {
		add(agentNode, "agent_emitted_artifact", "file:"+meta.Project+"/"+meta.FileName, 3, nil)
	}
	return edges
}

func extractAgentToolTags(tags []string) []string {
	out := []string{}
	seen := map[string]struct{}{}
	for _, raw := range tags {
		tag := strings.TrimSpace(strings.ToLower(raw))
		var tool string
		switch {
		case strings.HasPrefix(tag, "tool:"):
			tool = strings.TrimSpace(strings.TrimPrefix(tag, "tool:"))
		case strings.HasPrefix(tag, "tool="):
			tool = strings.TrimSpace(strings.TrimPrefix(tag, "tool="))
		case strings.HasPrefix(tag, "used_tool:"):
			tool = strings.TrimSpace(strings.TrimPrefix(tag, "used_tool:"))
		}
		tool = strings.Trim(tool, " /")
		if tool == "" {
			continue
		}
		if _, exists := seen[tool]; exists {
			continue
		}
		seen[tool] = struct{}{}
		out = append(out, tool)
		if len(out) >= 12 {
			break
		}
	}
	return out
}

func stableAgentEventEdgeID(sourceID string, relation string, targetID string, eventID string) string {
	key := strings.TrimSpace(sourceID) + "\x00" + strings.TrimSpace(relation) + "\x00" + strings.TrimSpace(targetID) + "\x00" + strings.TrimSpace(eventID)
	return "agent_edge_" + sha256Hex(key)[:32]
}

func (m *memoryStore) loadAgentEventEdges() error {
	if m == nil || !m.policy.enabled {
		return nil
	}
	file, err := os.Open(m.policy.agentEdgePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open memory agent edge log: %w", err)
	}
	defer file.Close()
	lines, err := readHistoryTailLines(file, m.policy.agentEdgeStartupMaxLines, m.policy.historyStartupTailMaxBytes)
	if err != nil {
		return fmt.Errorf("scan memory agent edge log: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, line := range lines {
		var edge AgentEventEdge
		if err := json.Unmarshal([]byte(line), &edge); err != nil {
			continue
		}
		m.recordAgentEventEdgeLocked(edge)
	}
	return nil
}

func (m *memoryStore) storeAgentEdges(entry memoryStoreEntry) error {
	if m == nil || !m.policy.enabled {
		return nil
	}
	edges := deriveAgentEventEdges(entry)
	if len(edges) == 0 {
		return nil
	}
	file, err := os.OpenFile(m.policy.agentEdgePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open memory agent edge append: %w", err)
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, edge := range edges {
		if edge.EdgeID == "" {
			edge.EdgeID = stableAgentEventEdgeID(edge.SourceID, edge.Relation, edge.TargetID, edge.EventID)
		}
		if _, exists := m.agentEdges[edge.EdgeID]; exists {
			continue
		}
		if err := encoder.Encode(edge); err != nil {
			return fmt.Errorf("append memory agent edge: %w", err)
		}
		m.recordAgentEventEdgeLocked(edge)
	}
	return nil
}

func (m *memoryStore) recordAgentEventEdgeLocked(edge AgentEventEdge) {
	if m == nil || strings.TrimSpace(edge.EdgeID) == "" {
		return
	}
	if _, exists := m.agentEdges[edge.EdgeID]; !exists {
		m.agentEdgeOrder = append(m.agentEdgeOrder, edge.EdgeID)
	}
	m.agentEdges[edge.EdgeID] = edge
	for len(m.agentEdgeOrder) > m.policy.maxAgentEdges {
		oldest := m.agentEdgeOrder[0]
		m.agentEdgeOrder = m.agentEdgeOrder[1:]
		delete(m.agentEdges, oldest)
	}
}
