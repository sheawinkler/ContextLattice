package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
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
	t.Setenv("GO_MEMORY_STORE_ROLLUP_USE_HISTORY_INDEX", "false")

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

func TestMemoryStoreCollectDocsSkipsLargeFilesAndBoundsRead(t *testing.T) {
	root := t.TempDir()
	historyPath := filepath.Join(root, "_contextlattice", "memory_write_history.ndjson")

	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", historyPath)
	t.Setenv("GO_MEMORY_STORE_ROLLUP_MAX_FILE_BYTES", "2048")
	t.Setenv("GO_MEMORY_STORE_ROLLUP_MAX_READ_BYTES", "1024")

	store, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("newMemoryStoreFromEnv failed: %v", err)
	}

	projectRoot := filepath.Join(root, "contextlattice", "notes")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatalf("mkdir project root failed: %v", err)
	}

	smallPath := filepath.Join(projectRoot, "small.md")
	smallContent := "HEAD_MARKER " + strings.Repeat("x", 1300) + " TAIL_MARKER"
	if err := os.WriteFile(smallPath, []byte(smallContent), 0o644); err != nil {
		t.Fatalf("write small file failed: %v", err)
	}

	largePath := filepath.Join(projectRoot, "large.md")
	largeContent := "LARGE_MARKER " + strings.Repeat("z", 5000)
	if err := os.WriteFile(largePath, []byte(largeContent), 0o644); err != nil {
		t.Fatalf("write large file failed: %v", err)
	}

	rows, err := store.collectDocs(context.Background(), "contextlattice")
	if err != nil {
		t.Fatalf("collectDocs failed: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly one row after large-file skip, got %d", len(rows))
	}
	if rows[0].FileName != "notes/small.md" {
		t.Fatalf("expected only notes/small.md, got %q", rows[0].FileName)
	}
	if !strings.Contains(rows[0].Summary, "HEAD_MARKER") {
		t.Fatalf("expected summary to contain head marker, summary=%q", rows[0].Summary)
	}
	if strings.Contains(rows[0].Summary, "TAIL_MARKER") {
		t.Fatalf("expected bounded read to trim tail marker, summary=%q", rows[0].Summary)
	}
}

func TestMemoryStoreContentAddressedBlobHardlinkMode(t *testing.T) {
	root := t.TempDir()
	historyPath := filepath.Join(root, "_contextlattice", "memory_write_history.ndjson")

	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", historyPath)
	t.Setenv("GO_MEMORY_STORE_CONTENT_ADDRESSING_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_CONTENT_LINK_MODE", "hardlink")

	store, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("newMemoryStoreFromEnv failed: %v", err)
	}

	content := "shared payload for content-addressed storage"
	first := normalizedWrite{
		project:   "contextlattice",
		fileName:  "notes/a.md",
		content:   content,
		topicPath: "runbooks/testing",
	}
	second := normalizedWrite{
		project:   "contextlattice",
		fileName:  "notes/b.md",
		content:   content,
		topicPath: "runbooks/testing",
	}
	entryA, _, err := store.put(first)
	if err != nil {
		t.Fatalf("first put failed: %v", err)
	}
	entryB, _, err := store.put(second)
	if err != nil {
		t.Fatalf("second put failed: %v", err)
	}
	if strings.TrimSpace(entryA.ContentRef) == "" || strings.TrimSpace(entryB.ContentRef) == "" {
		t.Fatalf("expected content_ref values, got %#v %#v", entryA.ContentRef, entryB.ContentRef)
	}
	if entryA.ContentRef != entryB.ContentRef {
		t.Fatalf("expected shared content_ref for identical payloads, got %q vs %q", entryA.ContentRef, entryB.ContentRef)
	}

	hash := strings.TrimPrefix(entryA.ContentRef, "sha256:")
	blobPath := filepath.Join(root, "_contextlattice", "content_blobs", hash[:2], hash+".txt")
	blobBytes, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("expected blob file %s, err=%v", blobPath, err)
	}
	if strings.TrimSpace(string(blobBytes)) != content {
		t.Fatalf("unexpected blob content: %q", string(blobBytes))
	}

	fileA := filepath.Join(root, "contextlattice", "notes", "a.md")
	fileB := filepath.Join(root, "contextlattice", "notes", "b.md")
	rawA, err := os.ReadFile(fileA)
	if err != nil {
		t.Fatalf("read fileA failed: %v", err)
	}
	rawB, err := os.ReadFile(fileB)
	if err != nil {
		t.Fatalf("read fileB failed: %v", err)
	}
	if string(rawA) != string(rawB) {
		t.Fatalf("expected identical logical file content")
	}
	if runtime.GOOS != "windows" {
		infoA, err := os.Stat(fileA)
		if err != nil {
			t.Fatalf("stat fileA failed: %v", err)
		}
		infoB, err := os.Stat(fileB)
		if err != nil {
			t.Fatalf("stat fileB failed: %v", err)
		}
		if !os.SameFile(infoA, infoB) {
			t.Fatalf("expected hardlinked logical files to reference the same file")
		}
	}
}

func TestMemoryStoreAgentEdgesAndReviewSignals(t *testing.T) {
	root := t.TempDir()
	historyPath := filepath.Join(root, "_contextlattice", "memory_write_history.ndjson")
	agentEdgePath := filepath.Join(root, "_contextlattice", "memory_agent_event_edges.ndjson")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", historyPath)
	t.Setenv("GO_MEMORY_AGENT_EDGE_PATH", agentEdgePath)

	store, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("newMemoryStoreFromEnv failed: %v", err)
	}
	writes := []normalizedWrite{
		{project: "contextlattice", fileName: "notes/review-a.md", content: "first review signal", topicPath: "runbooks/review", agentID: "agent-a", sessionID: "session-1", tags: []string{"tool:edit"}},
		{project: "contextlattice", fileName: "notes/review-b.md", content: "second review signal", topicPath: "runbooks/review", agentID: "agent-b", sessionID: "session-2"},
		{project: "contextlattice", fileName: "notes/review-a.md", content: "finding: rewritten review signal with mitigation", topicPath: "runbooks/review", agentID: "agent-a", sessionID: "session-1"},
	}
	for _, item := range writes {
		if _, _, err := store.put(item); err != nil {
			t.Fatalf("put failed: %v", err)
		}
	}

	store.mu.RLock()
	edgeCount := len(store.agentEdges)
	store.mu.RUnlock()
	if edgeCount == 0 {
		t.Fatalf("expected agent event edges to be recorded")
	}
	if raw, err := os.ReadFile(agentEdgePath); err != nil {
		t.Fatalf("read agent edge log failed: %v", err)
	} else if countNonEmptyLines(string(raw)) == 0 {
		t.Fatalf("expected persisted agent edge log rows")
	}

	rollups := store.topicRollupsWithOptions(context.Background(), "contextlattice", 1, 100, 0, false, false)
	topic := findRollupTopicForTest(rollups, "runbooks/review")
	if anyToInt(topic["writeCount"], 0) < 3 {
		t.Fatalf("expected writeCount from history, topic=%#v payload=%#v", topic, rollups)
	}
	if anyToInt(topic["uniqueAgentCount"], 0) != 2 {
		t.Fatalf("expected uniqueAgentCount=2, topic=%#v", topic)
	}
	if anyToInt(topic["uniqueSessionCount"], 0) != 2 {
		t.Fatalf("expected uniqueSessionCount=2, topic=%#v", topic)
	}
	if anyToInt(topic["agentIntensityScore"], 0) <= 0 {
		t.Fatalf("expected positive intensity score, topic=%#v", topic)
	}

	opts := reviewModeOptions{Project: "contextlattice", TopicPath: "runbooks/review", WindowHours: 168, MaxPatterns: 5, Limit: 100}
	topics := reviewTopicRows(asAnySliceForTest(rollups["topics"]), opts.TopicPath)
	patterns := buildReviewPatterns(opts, store.reviewEntries(opts), topics, rollups)
	if len(patterns) == 0 {
		t.Fatalf("expected review patterns from agent intensity")
	}
}

func findRollupTopicForTest(payload map[string]any, path string) map[string]any {
	rawTopics, _ := payload["topics"].([]any)
	for _, raw := range rawTopics {
		row, _ := raw.(map[string]any)
		if anyToString(row["path"]) == path {
			return row
		}
	}
	return map[string]any{}
}

func asAnySliceForTest(value any) []any {
	rows, _ := value.([]any)
	return rows
}
