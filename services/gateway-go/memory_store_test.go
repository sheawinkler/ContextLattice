package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
