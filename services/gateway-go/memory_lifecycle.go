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

func lifecycleSurfacesByDefault(lifecycle string) bool {
	switch normalizeMemoryLifecycle(lifecycle) {
	case "ephemeral", "test":
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
	return false
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
