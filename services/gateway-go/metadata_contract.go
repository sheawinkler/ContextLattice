package main

import (
	"os"
	"sort"
	"strings"
	"sync/atomic"
)

type canonicalMetadata struct {
	agentID   string
	sessionID string
	tags      []string
	createdAt string
	lifecycle string
}

type metadataContractTracker struct {
	totalWrites              atomic.Uint64
	missingAgentID           atomic.Uint64
	missingSessionID         atomic.Uint64
	missingTags              atomic.Uint64
	missingCreatedAt         atomic.Uint64
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

func normalizeMemoryLifecycle(raw string) string {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "", "durable", "permanent", "persist", "persistent", "learning":
		return "durable"
	case "working", "work", "scratch", "draft":
		return "working"
	case "ephemeral", "temporary", "temp", "tmp", "transient":
		return "ephemeral"
	case "test", "smoke", "synthetic", "fixture":
		return "test"
	default:
		return "durable"
	}
}

func lifecycleFromTags(tags []string) string {
	for _, raw := range tags {
		tag := strings.TrimSpace(strings.ToLower(raw))
		if strings.HasPrefix(tag, "lifecycle:") {
			if lifecycle := normalizeMemoryLifecycle(strings.TrimPrefix(tag, "lifecycle:")); lifecycle != "durable" || strings.Contains(tag, "durable") {
				return lifecycle
			}
		}
		if tag == "kind:test" || tag == "type:test" || tag == "purpose:test" {
			return "test"
		}
		if tag == "kind:smoke" || tag == "type:smoke" || tag == "retrieval-seed" {
			return "test"
		}
	}
	return ""
}

func defaultMetadataAgentID() string {
	return firstNonEmptyStrings(
		os.Getenv("GO_WRITE_DEFAULT_AGENT_ID"),
		os.Getenv("CONTEXTLATTICE_AGENT_ID"),
		os.Getenv("MEMMCP_AGENT_ID"),
		"gateway-go",
	)
}

func defaultMetadataSessionID() string {
	return firstNonEmptyStrings(
		os.Getenv("GO_WRITE_DEFAULT_SESSION_ID"),
		os.Getenv("CONTEXTLATTICE_SESSION_ID"),
		os.Getenv("MEMMCP_SESSION_ID"),
		os.Getenv("LETTA_AUTO_SESSION_ID"),
		"gateway-go",
	)
}

func defaultMetadataTags() []string {
	tags := normalizeTagList(os.Getenv("GO_WRITE_DEFAULT_TAGS"))
	if len(tags) == 0 {
		return []string{"source:gateway-go"}
	}
	return tags
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
	if agentID == "" {
		agentID = defaultMetadataAgentID()
	}
	sessionID := firstNonEmptyStrings(
		anyToString(raw["session_id"]),
		anyToString(raw["sessionId"]),
		anyToString(raw["session"]),
		anyToString(metaMap["session_id"]),
		anyToString(metaMap["sessionId"]),
		anyToString(metaMap["session"]),
	)
	if sessionID == "" {
		sessionID = defaultMetadataSessionID()
	}
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
	if len(tags) == 0 {
		tags = defaultMetadataTags()
	}
	lifecycle := firstNonEmptyStrings(
		anyToString(raw["lifecycle"]),
		anyToString(raw["memory_lifecycle"]),
		anyToString(raw["data_lifecycle"]),
		anyToString(metaMap["lifecycle"]),
		anyToString(metaMap["memory_lifecycle"]),
		anyToString(metaMap["data_lifecycle"]),
		lifecycleFromTags(tags),
	)
	lifecycle = normalizeMemoryLifecycle(lifecycle)
	return canonicalMetadata{
		agentID:   agentID,
		sessionID: sessionID,
		tags:      tags,
		createdAt: createdAt,
		lifecycle: lifecycle,
	}
}

func hasWriteMetadataValue(raw map[string]any, keys ...string) bool {
	metaMap, _ := raw["metadata"].(map[string]any)
	for _, key := range keys {
		if strings.TrimSpace(anyToString(raw[key])) != "" {
			return true
		}
		if strings.TrimSpace(anyToString(metaMap[key])) != "" {
			return true
		}
	}
	return false
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
	if strings.TrimSpace(item.createdAt) == "" {
		contractTracker.missingCreatedAt.Add(1)
	}
	if !hasWriteMetadataValue(item.raw, "created_at", "createdAt", "timestamp") {
		contractTracker.missingCreatedAtProvided.Add(1)
	}
}

func metadataContractSnapshot() map[string]any {
	total := contractTracker.totalWrites.Load()
	agentMissing := contractTracker.missingAgentID.Load()
	sessionMissing := contractTracker.missingSessionID.Load()
	tagMissing := contractTracker.missingTags.Load()
	createdAtMissing := contractTracker.missingCreatedAt.Load()
	createdAtProvidedMissing := contractTracker.missingCreatedAtProvided.Load()
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
			"created_at":          createdAtMissing,
			"created_at_provided": createdAtProvidedMissing,
		},
		"coverage": map[string]any{
			"agent_id":            coverage(agentMissing),
			"session_id":          coverage(sessionMissing),
			"tags":                coverage(tagMissing),
			"created_at":          coverage(createdAtMissing),
			"created_at_provided": coverage(createdAtProvidedMissing),
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
