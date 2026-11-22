package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newExactReadTestStore(t *testing.T) *memoryStore {
	t.Helper()
	root := t.TempDir()
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_BACKGROUND_HYDRATION_ENABLED", "false")
	t.Setenv("GO_MEMORY_STORE_CONTENT_ADDRESSING_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_CONTENT_LINK_MODE", "hardlink")
	for _, key := range []string{
		"GO_MEMORY_STORE_HISTORY_PATH", "GO_MEMORY_STORE_ACCESS_LOG_PATH", "GO_MEMORY_GRAPH_EDGE_PATH",
		"GO_MEMORY_STORE_CONTENT_BLOBS_PATH", "GO_MEMORY_STORE_EXACT_STATE_INDEX_PATH",
	} {
		t.Setenv(key, "")
	}
	store, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func assertExactReadBytes(t *testing.T, row map[string]any, content string) {
	t.Helper()
	digest := sha256Hex(content)
	if row["content"] != content || row["content_hash"] != digest || row["content_ref"] != "sha256:"+digest {
		t.Fatalf("GET changed the bytes or digest: %#v", row)
	}
}

func assertExactReadUnknown(t *testing.T, row map[string]any) {
	t.Helper()
	if row["topic_path"] != nil || row["object_id"] != nil || row["event_id"] != nil || row["event_recorded_at"] != nil {
		t.Fatalf("GET returned unproven identity: %#v", row)
	}
}

func TestMemoryStoreExactReadUnprovenRevisions(t *testing.T) {
	const project = "alpha"
	put := func(t *testing.T, store *memoryStore, file, content, topic, class string) {
		t.Helper()
		_, _, err := store.put(normalizedWrite{project: project, fileName: file, content: content, topicPath: topic, dataClass: class})
		if err != nil {
			t.Fatal(err)
		}
	}
	writeFile := func(t *testing.T, store *memoryStore, file, content string) {
		t.Helper()
		if err := writeOwnerOnlyAtomicFile(filepath.Join(store.policy.rootPath, project, file), []byte(content), true); err != nil {
			t.Fatal(err)
		}
	}
	checkUnknown := func(t *testing.T, reader *memoryStore, file, content string) {
		t.Helper()
		row := requestMemoryV1Get(t, &server{memoryStore: reader}, project+"::"+file, "", http.StatusOK)["memory"].(map[string]any)
		assertExactReadBytes(t, row, content)
		assertExactReadUnknown(t, row)
	}

	t.Run("file without metadata", func(t *testing.T) {
		store := newExactReadTestStore(t)
		writeFile(t, store, "unindexed.md", "legacy without newline")
		checkUnknown(t, store, "unindexed.md", "legacy without newline")
	})
	t.Run("old bytes do not recover old identity from history", func(t *testing.T) {
		store := newExactReadTestStore(t)
		put(t, store, "notes/replaced.md", "same body", "research/old", "")
		put(t, store, "notes/replaced.md", "new body", "research/new", "")
		writeFile(t, store, "notes/replaced.md", "same body\n")
		checkUnknown(t, store, "notes/replaced.md", "same body\n")
	})
	t.Run("exact-state mirror has no ordinary revision metadata", func(t *testing.T) {
		store := newExactReadTestStore(t)
		put(t, store, "notes/mirror.md", "same body", "research/old", "")
		put(t, store, "notes/mirror.md", "same body", "state/current", dataClassRuntimeStateMirror)
		checkUnknown(t, store, "notes/mirror.md", "same body\n")
	})
	t.Run("replay does not promote a derived topic to stored identity", func(t *testing.T) {
		store := newExactReadTestStore(t)
		const file = "notes/partial.md"
		const content = "legacy revision\n"
		digest := sha256Hex(content)
		writeFile(t, store, file, content)
		if err := store.appendHistory(memoryStoreEntry{
			EventID: "legacy-event", Project: project, FileName: file,
			ObjectID: store.objectIDFor(project, file, "notes", digest), ContentHash: digest,
			ContentRef: "sha256:" + digest, CreatedAt: nowUTCISO(), Source: "go_memory_store",
			DataClass: dataClassLearningMemory,
		}); err != nil {
			t.Fatal(err)
		}
		for attempt := 0; attempt < 2; attempt++ {
			reopened, err := newMemoryStoreFromEnv()
			if err != nil {
				t.Fatal(err)
			}
			checkUnknown(t, reopened, file, content)
		}
	})
	t.Run("unsequenced legacy replay does not expose a prior same-byte revision", func(t *testing.T) {
		store := newExactReadTestStore(t)
		const file = "notes/unsequenced.md"
		const content = "same body"
		const topic = "research/later"
		put(t, store, file, content, "research/earlier", "")
		digest := canonicalMemoryContentHash(content)
		if err := store.appendHistory(memoryStoreEntry{
			EventID: "legacy-event", Project: project, FileName: file, TopicPath: topic,
			ObjectID: store.objectIDFor(project, file, topic, digest), ContentHash: digest,
			ContentRef: "sha256:" + digest, Source: "go_memory_store", DataClass: dataClassLearningMemory,
		}); err != nil {
			t.Fatal(err)
		}
		reopened, err := newMemoryStoreFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		checkUnknown(t, reopened, file, canonicalMemoryContent(content))
	})
}

func TestMemoryStoreExactReadRejectsStaleGeneration(t *testing.T) {
	writer := newExactReadTestStore(t)
	item := normalizedWrite{project: "alpha", fileName: "notes/shared.md", topicPath: "research/before", content: "unchanged"}
	if _, _, err := writer.put(item); err != nil {
		t.Fatal(err)
	}
	reader, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	item.topicPath = "research/after"
	current, _, err := writer.put(item)
	if err != nil {
		t.Fatal(err)
	}
	id := item.project + "::" + item.fileName
	row := requestMemoryV1Get(t, &server{memoryStore: reader}, id, "", http.StatusOK)["memory"].(map[string]any)
	assertExactReadBytes(t, row, "unchanged\n")
	assertExactReadUnknown(t, row)

	row = requestMemoryV1Get(t, &server{memoryStore: writer}, id, "", http.StatusOK)["memory"].(map[string]any)
	assertExactReadBytes(t, row, "unchanged\n")
	if row["topic_path"] != current.TopicPath || row["object_id"] != current.ObjectID || row["event_id"] != current.EventID {
		t.Fatalf("current writer did not return the current revision: %#v", row)
	}
}

func TestMemoryStoreLegacyProjectionSurvivesExactReadUpgrade(t *testing.T) {
	store := newExactReadTestStore(t)
	const project, file, topic = "alpha", "notes/legacy/fixture.md", "notes/legacy"
	const content = "bounded cache rollback evidence\n"
	digest := sha256Hex(content)
	legacy := memoryStoreEntry{
		Project: project, FileName: file, CreatedAt: nowUTCISO(), Summary: content,
		ContentHash: digest, ContentRef: "sha256:" + digest,
		ObjectID: store.objectIDFor(project, file, topic, digest), DataClass: dataClassLearningMemory,
	}
	if err := writeOwnerOnlyAtomicFile(filepath.Join(store.policy.rootPath, project, file), []byte(content), true); err != nil {
		t.Fatal(err)
	}
	if err := store.appendHistory(legacy); err != nil {
		t.Fatal(err)
	}
	if err := store.persistAndRecordEntryLocked(legacy); err != nil {
		t.Fatal(err)
	}
	key := memoryStoreKey(project, file)
	shardPath := store.currentStateShardPath(memoryCurrentStateShardForKey(key))
	rawShard, err := os.ReadFile(shardPath)
	if err != nil {
		t.Fatal(err)
	}
	var shard memoryCurrentStateShard
	if err := json.Unmarshal(rawShard, &shard); err != nil || len(shard.Entries) != 1 ||
		shard.Entries[0].Entry.TopicPath != "" || shard.Entries[0].Entry.EventID != "" {
		t.Fatalf("fixture must persist the original missing topic/event: %s (%v)", rawShard, err)
	}

	reopen := func() *memoryStore {
		t.Helper()
		reader, err := newMemoryStoreFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		return reader
	}
	assertProjection := func(reader *memoryStore) {
		t.Helper()
		current, ok := reader.currentEntry(project, file)
		if !ok || current.TopicPath != topic || current.EventID == "" || reader.latestTopic[key] != topic {
			t.Fatalf("startup lost the legacy directory topic/event projection: %#v", current)
		}
		rows, _, err := reader.searchCurrentStateRows(context.Background(), content, project, topic, 5, false, false)
		if err != nil || len(rows) != 1 || rows[0]["file"] != file {
			t.Fatalf("directory-scoped legacy row unavailable: rows=%#v err=%v", rows, err)
		}
		rows, stats, err := reader.searchCurrentStateRowsWithAncestorFallback(
			context.Background(), content, project, topic+"/sibling", 5, false, false, 1, 0.55, 0.25,
		)
		if err != nil || len(rows) != 1 || rows[0]["file"] != file || !stats.AncestorUsed || stats.AncestorPrefix != topic {
			t.Fatalf("nearest-ancestor legacy row unavailable: rows=%#v stats=%#v err=%v", rows, stats, err)
		}
	}

	replayed := reopen()
	assertProjection(replayed)
	generationPath := replayed.currentStateGenerationCardPath(project)
	readCard := func() []byte {
		t.Helper()
		card, err := os.ReadFile(generationPath)
		if err != nil {
			t.Fatal(err)
		}
		return card
	}
	projectedCard := readCard()
	// Simulate the raw row aging out while the pre-upgrade projection remains.
	if err := os.Remove(store.policy.historyPath); err != nil {
		t.Fatal(err)
	}
	restored := reopen()
	assertProjection(restored)
	if !bytes.Equal(projectedCard, readCard()) {
		t.Fatalf("opening the durable legacy projection changed its generation card")
	}

	t.Run("synthetic projection is not exact provenance", func(t *testing.T) {
		for _, reader := range []*memoryStore{replayed, restored} {
			row := requestMemoryV1Get(t, &server{memoryStore: reader}, project+"::"+file, "", http.StatusOK)["memory"].(map[string]any)
			assertExactReadBytes(t, row, content)
			assertExactReadUnknown(t, row)
		}
	})
}

func TestMemoryStoreExactReadLargeProjectChecksOneBoundedCard(t *testing.T) {
	store := newExactReadTestStore(t)
	const project, topic, documents = "alpha", "research/large", 1024
	const content = "large scoped project evidence\n"
	digest := sha256Hex(content)
	var history bytes.Buffer
	encoder := json.NewEncoder(&history)
	for i := range documents {
		file := fmt.Sprintf("notes/%04d.md", i)
		entry := memoryStoreEntry{
			EventID: fmt.Sprintf("event-%04d", i), Project: project, FileName: file, TopicPath: topic,
			CreatedAt: nowUTCISO(), Summary: content, Source: "go_memory_store", DataClass: dataClassLearningMemory,
			ContentHash: digest, ContentRef: "sha256:" + digest, ObjectID: store.objectIDFor(project, file, topic, digest),
		}
		if err := encoder.Encode(entry); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeOwnerOnlyAtomicFile(store.policy.historyPath, history.Bytes(), true); err != nil {
		t.Fatal(err)
	}
	const file = "notes/1023.md"
	if err := writeOwnerOnlyAtomicFile(filepath.Join(store.policy.rootPath, project, file), []byte(content), true); err != nil {
		t.Fatal(err)
	}
	reader, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	cardPath := reader.currentStateGenerationCardPath(project)
	card, err := os.ReadFile(cardPath)
	if err != nil || len(card) == 0 || len(card) > 512 || reader.currentKeyCountsByProject[project] != documents {
		t.Fatalf("unexpected large-project fixture: card bytes=%d documents=%d err=%v", len(card), reader.currentKeyCountsByProject[project], err)
	}
	t.Logf("project documents=%d card bytes=%d", documents, len(card))
	reader.memoryCurrentStateDigestObserve = func(int, int) { t.Fatal("GET recomputed a shard digest") }
	reader.memoryCurrentStateGenerationCardActualObserve = func(int) { t.Fatal("GET walked the generation-card tree") }
	if err := os.Remove(reader.policy.historyPath); err != nil {
		t.Fatal(err)
	}
	s := &server{memoryStore: reader}
	row := requestMemoryV1Get(t, s, project+"::"+file, "", http.StatusOK)["memory"].(map[string]any)
	assertExactReadBytes(t, row, content)
	if row["event_id"] != "event-1023" || row["topic_path"] != topic || row["object_id"] != store.objectIDFor(project, file, topic, digest) {
		t.Fatalf("GET fell back from its replayed identity: %#v", row)
	}
	if err := os.Truncate(cardPath, int64(len(card))+1); err != nil {
		t.Fatal(err)
	}
	row = requestMemoryV1Get(t, s, project+"::"+file, "", http.StatusOK)["memory"].(map[string]any)
	assertExactReadBytes(t, row, content)
	assertExactReadUnknown(t, row)

	if err := writeOwnerOnlyAtomicFile(cardPath, card, true); err != nil {
		t.Fatal(err)
	}
	reopened, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	row = requestMemoryV1Get(t, &server{memoryStore: reopened}, project+"::"+file, "", http.StatusOK)["memory"].(map[string]any)
	assertExactReadBytes(t, row, content)
	assertExactReadUnknown(t, row)
}

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

func TestMemoryStoreRecoveryReadersHonorFiniteCaps(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "history.ndjson")
	rows := make([]string, 0, 20)
	for index := 0; index < 20; index++ {
		rows = append(rows, `{"row":`+strconv.Itoa(index)+`}`)
	}
	if err := os.WriteFile(path, []byte(strings.Join(rows, "\n")+"\n"), ownerOnlyFileMode); err != nil {
		t.Fatalf("write bounded history fixture: %v", err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open bounded history fixture: %v", err)
	}
	lines, err := readHistoryTailLines(file, 3, 40)
	_ = file.Close()
	if err != nil {
		t.Fatalf("read bounded history tail: %v", err)
	}
	if len(lines) > 3 || len(strings.Join(lines, "\n")) > 40 {
		t.Fatalf("history tail exceeded explicit caps: lines=%d bytes=%d", len(lines), len(strings.Join(lines, "\n")))
	}
	oversizedPath := filepath.Join(root, "oversized.bin")
	if err := os.WriteFile(oversizedPath, []byte(strings.Repeat("x", 17)), ownerOnlyFileMode); err != nil {
		t.Fatalf("write oversized bounded fixture: %v", err)
	}
	if _, err := readOwnerOnlyBoundedFile(oversizedPath, 16); !errors.Is(err, errMemoryEdgeLogOversized) {
		t.Fatalf("bounded owner-only read accepted oversized fixture: %v", err)
	}
	shardPath := filepath.Join(root, "current-state-00.json")
	if err := os.WriteFile(shardPath, []byte(strings.Repeat("y", 17)), ownerOnlyFileMode); err != nil {
		t.Fatalf("write oversized current-state fixture: %v", err)
	}
	if _, err := readMemoryCurrentStateShard(shardPath, 16); !errors.Is(err, errMemoryEdgeLogOversized) {
		t.Fatalf("bounded current-state read accepted oversized fixture: %v", err)
	}
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
