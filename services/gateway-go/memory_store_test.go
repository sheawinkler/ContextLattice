package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMemoryStorePutSkipsUnchangedDuplicateHistory(t *testing.T) {
	root := t.TempDir()
	historyPath := filepath.Join(root, "_contextlattice", "memory_write_history.ndjson")

	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", historyPath)

	store, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("newMemoryStoreFromEnv failed: %v", err)
	}

	item := normalizedWrite{
		project:   "contextlattice",
		fileName:  "notes/dedupe.md",
		content:   "same payload",
		topicPath: "runbooks/testing",
	}

	if _, deduped, err := store.put(item); err != nil {
		t.Fatalf("first put failed: %v", err)
	} else if deduped {
		t.Fatalf("expected first put deduped=false")
	}

	if _, deduped, err := store.put(item); err != nil {
		t.Fatalf("second put failed: %v", err)
	} else if !deduped {
		t.Fatalf("expected second put deduped=true")
	}

	raw, err := os.ReadFile(historyPath)
	if err != nil {
		t.Fatalf("read history failed: %v", err)
	}
	if got := countNonEmptyLines(string(raw)); got != 1 {
		t.Fatalf("expected 1 history line after duplicate write, got %d", got)
	}
}

func TestMemoryStorePutSameContentDifferentTopicPersistsHistory(t *testing.T) {
	root := t.TempDir()
	historyPath := filepath.Join(root, "_contextlattice", "memory_write_history.ndjson")

	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", historyPath)

	store, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("newMemoryStoreFromEnv failed: %v", err)
	}

	item := normalizedWrite{
		project:   "contextlattice",
		fileName:  "notes/topic-shift.md",
		content:   "same payload",
		topicPath: "runbooks/one",
	}
	if _, _, err := store.put(item); err != nil {
		t.Fatalf("first put failed: %v", err)
	}

	item.topicPath = "runbooks/two"
	if _, deduped, err := store.put(item); err != nil {
		t.Fatalf("second put failed: %v", err)
	} else if !deduped {
		t.Fatalf("expected second put deduped=true for unchanged content hash")
	}

	raw, err := os.ReadFile(historyPath)
	if err != nil {
		t.Fatalf("read history failed: %v", err)
	}
	if got := countNonEmptyLines(string(raw)); got != 2 {
		t.Fatalf("expected 2 history lines when topic changes, got %d", got)
	}

	key := memoryStoreKey("contextlattice", "notes/topic-shift.md")
	store.mu.RLock()
	latestTopic := store.latestTopic[key]
	store.mu.RUnlock()
	if latestTopic != "runbooks/two" {
		t.Fatalf("expected latest topic runbooks/two, got %q", latestTopic)
	}
}

func countNonEmptyLines(raw string) int {
	count := 0
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) != "" {
			count += 1
		}
	}
	return count
}

func TestMemoryStoreTopicRollupCacheTTL(t *testing.T) {
	root := t.TempDir()
	historyPath := filepath.Join(root, "_contextlattice", "memory_write_history.ndjson")
	projectRoot := filepath.Join(root, "contextlattice")
	manualFile := filepath.Join(projectRoot, "notes", "manual-outside-put.md")

	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", historyPath)
	t.Setenv("GO_MEMORY_STORE_ROLLUP_CACHE_TTL_SECS", "1")

	store, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("newMemoryStoreFromEnv failed: %v", err)
	}
	if _, _, err := store.put(normalizedWrite{
		project:   "contextlattice",
		fileName:  "notes/seed.md",
		content:   "seed",
		topicPath: "runbooks/testing",
	}); err != nil {
		t.Fatalf("seed put failed: %v", err)
	}

	first := store.topicRollupsWithContext(context.Background(), "contextlattice", 1, 5000, 0)
	totalBefore := anyToInt(first["total"], 0)
	if totalBefore < 1 {
		t.Fatalf("expected non-empty topic rollups, got %#v", first)
	}

	if err := os.MkdirAll(filepath.Dir(manualFile), 0o755); err != nil {
		t.Fatalf("mkdir manual file path failed: %v", err)
	}
	if err := os.WriteFile(manualFile, []byte("manual update\n"), 0o644); err != nil {
		t.Fatalf("write manual file failed: %v", err)
	}

	cached := store.topicRollupsWithContext(context.Background(), "contextlattice", 1, 5000, 0)
	totalCached := anyToInt(cached["total"], 0)
	if totalCached != totalBefore {
		t.Fatalf("expected cached rollups unchanged before ttl expiry, before=%d cached=%d", totalBefore, totalCached)
	}
	if strings.TrimSpace(anyToString(cached["cache"])) != "hit" {
		t.Fatalf("expected cache hit before ttl expiry, got %#v", cached["cache"])
	}

	time.Sleep(1200 * time.Millisecond)
	after := store.topicRollupsWithContext(context.Background(), "contextlattice", 1, 5000, 0)
	totalAfter := anyToInt(after["total"], 0)
	if totalAfter <= totalBefore {
		t.Fatalf("expected cache expiry to pick up manual file, before=%d after=%d", totalBefore, totalAfter)
	}
	if strings.TrimSpace(anyToString(after["cache"])) != "miss" {
		t.Fatalf("expected cache miss after ttl expiry, got %#v", after["cache"])
	}
}
