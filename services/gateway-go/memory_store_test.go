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

	first := store.topicRollupsWithContext(context.Background(), "contextlattice", 1, 5000, 0, false)
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

	cached := store.topicRollupsWithContext(context.Background(), "contextlattice", 1, 5000, 0, false)
	totalCached := anyToInt(cached["total"], 0)
	if totalCached != totalBefore {
		t.Fatalf("expected cached rollups unchanged before ttl expiry, before=%d cached=%d", totalBefore, totalCached)
	}
	if strings.TrimSpace(anyToString(cached["cache"])) != "hit" {
		t.Fatalf("expected cache hit before ttl expiry, got %#v", cached["cache"])
	}

	time.Sleep(1200 * time.Millisecond)
	after := store.topicRollupsWithContext(context.Background(), "contextlattice", 1, 5000, 0, false)
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

	rows, err := store.collectDocs(context.Background(), "contextlattice", false, false)
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

func TestMemoryStoreHotHorizonFiltersOldDocsButIncludeColdRestores(t *testing.T) {
	root := t.TempDir()
	historyPath := filepath.Join(root, "_contextlattice", "memory_write_history.ndjson")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", historyPath)
	t.Setenv("GO_MEMORY_STORE_HOT_INDEX_MAX_AGE_DAYS", "1")

	store, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("newMemoryStoreFromEnv failed: %v", err)
	}
	if _, _, err := store.put(normalizedWrite{
		project:   "contextlattice",
		fileName:  "notes/horizon.md",
		content:   "horizon test",
		topicPath: "runbooks/hot-cold",
	}); err != nil {
		t.Fatalf("put failed: %v", err)
	}

	key := memoryStoreKey("contextlattice", "notes/horizon.md")
	oldTs := time.Now().UTC().Add(-72 * time.Hour)
	oldISO := oldTs.Format(time.RFC3339Nano)
	store.mu.Lock()
	store.lastAccess[key] = oldTs
	current := store.currentState[key]
	current.Entry.CreatedAt = oldISO
	current.Entry.LastAccess = oldISO
	store.currentState[key] = current
	for idx := range store.recent {
		if store.recent[idx].Project == "contextlattice" && store.recent[idx].FileName == "notes/horizon.md" {
			store.recent[idx].LastAccess = oldISO
			store.recent[idx].CreatedAt = oldISO
		}
	}
	store.mu.Unlock()
	filePath := filepath.Join(root, "contextlattice", "notes", "horizon.md")
	if err := os.Chtimes(filePath, oldTs, oldTs); err != nil {
		t.Fatalf("chtimes failed: %v", err)
	}

	hotOnly := store.topicRollupsWithContext(context.Background(), "contextlattice", 1, 100, 0, false)
	if anyToInt(hotOnly["total"], 0) != 0 {
		t.Fatalf("expected hot-only rollups to filter old docs, payload=%v", hotOnly)
	}

	withCold := store.topicRollupsWithContext(context.Background(), "contextlattice", 1, 100, 0, true)
	if anyToInt(withCold["total"], 0) < 1 {
		t.Fatalf("expected include_cold to restore rows, payload=%v", withCold)
	}
}

func TestMemoryStorePerWriteHorizonOverridesGlobal(t *testing.T) {
	root := t.TempDir()
	historyPath := filepath.Join(root, "_contextlattice", "memory_write_history.ndjson")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", historyPath)
	t.Setenv("GO_MEMORY_STORE_HOT_INDEX_MAX_AGE_DAYS", "1")
	t.Setenv("GO_MEMORY_STORE_HORIZON_TAG_PREFIX", "horizon_days:")

	store, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("newMemoryStoreFromEnv failed: %v", err)
	}
	if _, _, err := store.put(normalizedWrite{
		project:   "contextlattice",
		fileName:  "notes/overridden.md",
		content:   "override",
		topicPath: "runbooks/hot-cold",
		tags:      []string{"horizon_days:30"},
	}); err != nil {
		t.Fatalf("put failed: %v", err)
	}
	key := memoryStoreKey("contextlattice", "notes/overridden.md")
	oldTs := time.Now().UTC().Add(-7 * 24 * time.Hour)
	oldISO := oldTs.Format(time.RFC3339Nano)
	store.mu.Lock()
	store.lastAccess[key] = oldTs
	for idx := range store.recent {
		if store.recent[idx].Project == "contextlattice" && store.recent[idx].FileName == "notes/overridden.md" {
			store.recent[idx].LastAccess = oldISO
			store.recent[idx].CreatedAt = oldISO
		}
	}
	store.mu.Unlock()
	filePath := filepath.Join(root, "contextlattice", "notes", "overridden.md")
	if err := os.Chtimes(filePath, oldTs, oldTs); err != nil {
		t.Fatalf("chtimes failed: %v", err)
	}

	hotOnly := store.topicRollupsWithContext(context.Background(), "contextlattice", 1, 100, 0, false)
	if anyToInt(hotOnly["total"], 0) < 1 {
		t.Fatalf("expected per-write horizon override to keep row hot, payload=%v", hotOnly)
	}
}

func TestMemoryStoreEphemeralLifecycleExcludedFromRollupsByDefault(t *testing.T) {
	root := t.TempDir()
	historyPath := filepath.Join(root, "_contextlattice", "memory_write_history.ndjson")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", historyPath)

	store, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("newMemoryStoreFromEnv failed: %v", err)
	}
	if _, _, err := store.put(normalizedWrite{
		project:   "contextlattice",
		fileName:  "notes/durable.md",
		content:   "durable marker",
		topicPath: "runbooks/lifecycle",
		lifecycle: "durable",
	}); err != nil {
		t.Fatalf("put durable failed: %v", err)
	}
	if _, _, err := store.put(normalizedWrite{
		project:   "contextlattice",
		fileName:  "notes/ephemeral.md",
		content:   "ephemeral marker",
		topicPath: "runbooks/lifecycle",
		lifecycle: "ephemeral",
	}); err != nil {
		t.Fatalf("put ephemeral failed: %v", err)
	}

	defaultRollups := store.topicRollupsWithContext(context.Background(), "contextlattice", 1, 100, 0, false)
	defaultTopic := findRollupTopicForTest(defaultRollups, "runbooks/lifecycle")
	if anyToInt(defaultTopic["eventCount"], 0) != 1 {
		t.Fatalf("expected default rollup to exclude ephemeral row, topic=%#v payload=%#v", defaultTopic, defaultRollups)
	}

	withEphemeral := store.topicRollupsWithOptions(context.Background(), "contextlattice", 1, 100, 0, false, true)
	includedTopic := findRollupTopicForTest(withEphemeral, "runbooks/lifecycle")
	if anyToInt(includedTopic["eventCount"], 0) != 2 {
		t.Fatalf("expected includeEphemeral rollup to include both rows, topic=%#v payload=%#v", includedTopic, withEphemeral)
	}
}

func TestMemoryStoreTopicRollupsExposeAgentIntensitySignals(t *testing.T) {
	root := t.TempDir()
	historyPath := filepath.Join(root, "_contextlattice", "memory_write_history.ndjson")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", historyPath)

	store, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("newMemoryStoreFromEnv failed: %v", err)
	}
	writes := []normalizedWrite{
		{project: "contextlattice", fileName: "notes/review-a.md", content: "first review signal", topicPath: "runbooks/review", agentID: "agent-a", sessionID: "session-1"},
		{project: "contextlattice", fileName: "notes/review-b.md", content: "second review signal", topicPath: "runbooks/review", agentID: "agent-b", sessionID: "session-2"},
		{project: "contextlattice", fileName: "notes/review-a.md", content: "rewritten review signal with mitigation", topicPath: "runbooks/review", agentID: "agent-a", sessionID: "session-1"},
	}
	for _, item := range writes {
		if _, _, err := store.put(item); err != nil {
			t.Fatalf("put failed: %v", err)
		}
	}

	rollups := store.topicRollupsWithContext(context.Background(), "contextlattice", 1, 100, 0, false)
	topic := findRollupTopicForTest(rollups, "runbooks/review")
	if anyToInt(topic["writeCount"], 0) < 3 {
		t.Fatalf("expected writeCount from history, topic=%#v payload=%#v", topic, rollups)
	}
	if anyToInt(topic["recentEventCount"], 0) < 3 {
		t.Fatalf("expected recent event count from write history, topic=%#v", topic)
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
	diffCounts := anyMap(topic["diffStateCounts"])
	if anyToInt(diffCounts["rewrite"], 0) < 1 {
		t.Fatalf("expected rewrite diff signal, topic=%#v", topic)
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

func TestMemoryStorePutIntegrityFieldsPresent(t *testing.T) {
	root := t.TempDir()
	historyPath := filepath.Join(root, "_contextlattice", "memory_write_history.ndjson")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", historyPath)

	store, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("newMemoryStoreFromEnv failed: %v", err)
	}
	entry, _, err := store.put(normalizedWrite{
		project:   "contextlattice",
		fileName:  "notes/integrity.md",
		content:   "integrity payload",
		topicPath: "runbooks/integrity",
	})
	if err != nil {
		t.Fatalf("put failed: %v", err)
	}
	if strings.TrimSpace(entry.ObjectID) == "" {
		t.Fatalf("expected object_id to be populated")
	}
	if entry.Confidence <= 0 {
		t.Fatalf("expected confidence > 0, got %f", entry.Confidence)
	}
	if strings.TrimSpace(entry.DiffState) == "" {
		t.Fatalf("expected diff_state to be populated")
	}
	if strings.TrimSpace(entry.LastAccess) == "" {
		t.Fatalf("expected last_accessed_at to be populated")
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

func asAnySliceForTest(value any) []any {
	rows, _ := value.([]any)
	return rows
}
