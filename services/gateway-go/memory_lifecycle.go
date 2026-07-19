package main

import (
	"strings"
	"time"
)

func requestIncludesEphemeralMemory(request map[string]any) bool {
	return anyToBool(request["include_ephemeral"]) ||
		anyToBool(request["include_ephemeral_memory"]) ||
		anyToBool(request["include_test_memory"])
}

func isEphemeralMemoryIdentity(fileName string, topicPath string, summary string, lifecycle string) bool {
	normalizedLifecycle := normalizeMemoryLifecycle(lifecycle)
	if normalizedLifecycle == "ephemeral" || normalizedLifecycle == "test" {
		return true
	}
	file := strings.TrimSpace(strings.ToLower(fileName))
	topic := strings.Trim(strings.TrimSpace(strings.ToLower(topicPath)), "/")
	text := strings.TrimSpace(strings.ToLower(summary))
	if strings.HasPrefix(file, "notes/codex/external-provider-cli-smoke-seed-clx-smoke-") ||
		file == "notes/codex/external-provider-cli-smoke-seed.md" {
		return true
	}
	if strings.HasPrefix(topic, "ops/ephemeral/") ||
		strings.HasPrefix(topic, "tmp/") ||
		strings.HasPrefix(topic, "test/") {
		return true
	}
	return strings.Contains(text, "retrieval_nonce: clx-smoke-") &&
		strings.Contains(text, "purpose: prove provider retrieval smoke")
}

func safeEphemeralPurgeSelector(project string, topicPath string, filePrefix string) bool {
	if strings.TrimSpace(project) == "" {
		return false
	}
	prefix := strings.TrimSpace(strings.ToLower(filePrefix))
	topic := strings.Trim(strings.TrimSpace(strings.ToLower(topicPath)), "/")
	if prefix == "" || strings.Contains(prefix, "..") || strings.HasPrefix(prefix, "/") {
		return false
	}
	if strings.HasPrefix(prefix, "notes/codex/external-provider-cli-smoke-seed") {
		return true
	}
	if strings.Contains(prefix, "ephemeral") || strings.Contains(prefix, "smoke") || strings.Contains(prefix, "test") {
		return true
	}
	return strings.HasPrefix(topic, "ops/ephemeral/") ||
		strings.HasPrefix(topic, "tmp/") ||
		strings.HasPrefix(topic, "test/")
}

func lifecycleSurfacesByDefault(lifecycle string) bool {
	switch normalizeMemoryLifecycle(lifecycle) {
	case "ephemeral", "test", "retired", "superseded", "retracted":
		return false
	default:
		return true
	}
}

func shouldSurfaceMemoryLifecycle(lifecycle string, includeEphemeral bool) bool {
	if includeEphemeral {
		return true
	}
	return lifecycleSurfacesByDefault(lifecycle)
}

func isMemoryTombstone(entry memoryStoreEntry) bool {
	return strings.EqualFold(strings.TrimSpace(entry.DataClass), "memory_tombstone")
}

func parseTimeBestEffort(raw string) (time.Time, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Time{}, false
	}
	if ts, err := time.Parse(time.RFC3339Nano, trimmed); err == nil {
		return ts.UTC(), true
	}
	if ts, err := time.Parse(time.RFC3339, trimmed); err == nil {
		return ts.UTC(), true
	}
	return time.Time{}, false
}
