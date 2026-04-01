package main

import (
	"sort"
	"strings"
	"sync/atomic"
)

type canonicalMetadata struct {
	agentID   string
	sessionID string
	tags      []string
	createdAt string
}

type metadataContractTracker struct {
	totalWrites              atomic.Uint64
	missingAgentID           atomic.Uint64
	missingSessionID         atomic.Uint64
	missingTags              atomic.Uint64
	missingCreatedAtProvided atomic.Uint64
}

var contractTracker metadataContractTracker

func firstNonEmptyStrings(values ...string) string {
	for _, value := range values {
		token := strings.TrimSpace(value)
		if token != "" {
			return token
		}
	}
	return ""
}

func normalizeTagList(values ...any) []string {
	out := make([]string, 0, 4)
	seen := map[string]struct{}{}
	push := func(value string) {
		token := strings.TrimSpace(value)
		if token == "" {
			return
		}
		lower := strings.ToLower(token)
		if _, exists := seen[lower]; exists {
			return
		}
		seen[lower] = struct{}{}
		out = append(out, token)
	}
	for _, value := range values {
		switch typed := value.(type) {
		case string:
			for _, token := range strings.Split(typed, ",") {
				push(token)
			}
		case []string:
			for _, token := range typed {
				push(token)
			}
		case []any:
			for _, token := range typed {
				push(anyToString(token))
			}
		case map[string]any:
			for key, enabled := range typed {
				if anyToBool(enabled) {
					push(key)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func normalizeWriteMetadata(raw map[string]any) canonicalMetadata {
	metaMap, _ := raw["metadata"].(map[string]any)
	agentID := firstNonEmptyStrings(
		anyToString(raw["agent_id"]),
		anyToString(raw["agentId"]),
		anyToString(raw["agent"]),
		anyToString(raw["source_agent_id"]),
		anyToString(metaMap["agent_id"]),
		anyToString(metaMap["agentId"]),
		anyToString(metaMap["agent"]),
	)
	sessionID := firstNonEmptyStrings(
		anyToString(raw["session_id"]),
		anyToString(raw["sessionId"]),
		anyToString(raw["session"]),
		anyToString(metaMap["session_id"]),
		anyToString(metaMap["sessionId"]),
		anyToString(metaMap["session"]),
	)
	createdAt := firstNonEmptyStrings(
		anyToString(raw["created_at"]),
		anyToString(raw["createdAt"]),
		anyToString(raw["timestamp"]),
		anyToString(metaMap["created_at"]),
		anyToString(metaMap["createdAt"]),
		anyToString(metaMap["timestamp"]),
	)
	if createdAt == "" {
		createdAt = nowUTCISO()
	}
	tags := normalizeTagList(
		raw["tags"],
		raw["labels"],
		metaMap["tags"],
		metaMap["labels"],
	)
	return canonicalMetadata{
		agentID:   agentID,
		sessionID: sessionID,
		tags:      tags,
		createdAt: createdAt,
	}
}

func recordMetadataContractObservation(item normalizedWrite) {
	contractTracker.totalWrites.Add(1)
	if strings.TrimSpace(item.agentID) == "" {
		contractTracker.missingAgentID.Add(1)
	}
	if strings.TrimSpace(item.sessionID) == "" {
		contractTracker.missingSessionID.Add(1)
	}
	if len(item.tags) == 0 {
		contractTracker.missingTags.Add(1)
	}
	if strings.TrimSpace(anyToString(item.raw["created_at"])) == "" &&
		strings.TrimSpace(anyToString(item.raw["createdAt"])) == "" &&
		strings.TrimSpace(anyToString(item.raw["timestamp"])) == "" {
		contractTracker.missingCreatedAtProvided.Add(1)
	}
}

func metadataContractSnapshot() map[string]any {
	total := contractTracker.totalWrites.Load()
	agentMissing := contractTracker.missingAgentID.Load()
	sessionMissing := contractTracker.missingSessionID.Load()
	tagMissing := contractTracker.missingTags.Load()
	createdAtMissing := contractTracker.missingCreatedAtProvided.Load()
	coverage := func(missing uint64) float64 {
		if total == 0 {
			return 1
		}
		present := float64(total-missing) / float64(total)
		if present < 0 {
			return 0
		}
		if present > 1 {
			return 1
		}
		return present
	}
	return map[string]any{
		"totalWrites": total,
		"missing": map[string]any{
			"agent_id":            agentMissing,
			"session_id":          sessionMissing,
			"tags":                tagMissing,
			"created_at_provided": createdAtMissing,
		},
		"coverage": map[string]any{
			"agent_id":            coverage(agentMissing),
			"session_id":          coverage(sessionMissing),
			"tags":                coverage(tagMissing),
			"created_at_provided": coverage(createdAtMissing),
		},
		"contract": []string{
			"project",
			"agent_id",
			"session_id",
			"topic_path",
			"tags",
			"created_at",
			"content_ref",
		},
	}
}
