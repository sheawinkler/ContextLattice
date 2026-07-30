package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func newMemoryReviewerStore(t *testing.T, root string, historyTail int) *memoryStore {
	t.Helper()
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", filepath.Join(root, "_contextlattice", "memory_write_history.ndjson"))
	t.Setenv("GO_MEMORY_STORE_HISTORY_STARTUP_MAX_LINES", fmt.Sprintf("%d", historyTail))
	t.Setenv("GO_MEMORY_STORE_HISTORY_STARTUP_TAIL_MAX_BYTES", "1048576")
	t.Setenv("GO_MEMORY_STORE_CONTENT_ADDRESSING_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_STARTUP_BUDGET_MILLIS", "5000")
	store, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("create memory store: %v", err)
	}
	if !store.isEnabled() {
		t.Fatal("memory store did not become ready")
	}
	return store
}

func TestMemoryCurrentStateSurvivesRestartBeyondHistoryTail(t *testing.T) {
	root := t.TempDir()
	store := newMemoryReviewerStore(t, root, 3)
	held, _, err := store.put(normalizedWrite{
		project:     "reviewer-restart",
		fileName:    "notes/held-retired.md",
		content:     "held retirement evidence",
		topicPath:   "reviewer/memory",
		tags:        []string{"legal_hold", "evidence"},
		lifecycle:   "retired",
		storageTier: "deep",
	})
	if err != nil {
		t.Fatalf("write held state: %v", err)
	}
	for index := 0; index < 8; index++ {
		if _, _, err := store.put(normalizedWrite{
			project:   "reviewer-restart",
			fileName:  fmt.Sprintf("notes/filler-%02d.md", index),
			content:   fmt.Sprintf("filler %d", index),
			topicPath: "reviewer/filler",
		}); err != nil {
			t.Fatalf("write filler %d: %v", index, err)
		}
	}

	restarted := newMemoryReviewerStore(t, root, 3)
	for _, entry := range restarted.recent {
		if entry.EventID == held.EventID {
			t.Fatalf("held state unexpectedly survived through the bounded history tail: %#v", entry)
		}
	}
	state, ok := restarted.currentStateFor("reviewer-restart", "notes/held-retired.md")
	if !ok {
		t.Fatal("canonical held state was missing after restart")
	}
	if !state.LegalHold || state.Entry.Lifecycle != "retired" || state.Entry.StorageTier != "deep" {
		t.Fatalf("canonical state changed after restart: %#v", state)
	}
	resolved, err := frontierT5MemoryState(restarted, "reviewer-restart", "notes/held-retired.md")
	if err != nil {
		t.Fatalf("resolve restarted T5 state: %v", err)
	}
	if !anyToBool(resolved["legal_hold"]) || anyToString(resolved["lifecycle"]) != "retired" || anyToString(resolved["storage_tier"]) != "deep" {
		t.Fatalf("T5 state resurrected held/retired metadata: %#v", resolved)
	}
	docs, err := restarted.collectDocs(context.Background(), "reviewer-restart", false, false)
	if err != nil {
		t.Fatalf("collect ordinary docs: %v", err)
	}
	for _, doc := range docs {
		if doc.FileName == "notes/held-retired.md" {
			t.Fatalf("ordinary retrieval resurrected retired state: %#v", doc)
		}
	}
}

func TestMemoryStorePutAppendFailureDoesNotPublishState(t *testing.T) {
	root := t.TempDir()
	store := newMemoryReviewerStore(t, root, 8)
	baseline, _, err := store.put(normalizedWrite{
		project:     "reviewer-append",
		fileName:    "notes/atomic.md",
		content:     "durable baseline",
		topicPath:   "reviewer/append",
		lifecycle:   "durable",
		storageTier: "hot",
	})
	if err != nil {
		t.Fatalf("write baseline: %v", err)
	}
	baselineRecent := len(store.recent)
	originalHistoryPath := store.policy.historyPath
	blockedHistoryPath := filepath.Join(root, "blocked-history")
	if err := os.Mkdir(blockedHistoryPath, 0o700); err != nil {
		t.Fatalf("create blocked history path: %v", err)
	}
	store.policy.historyPath = blockedHistoryPath

	if _, _, err := store.put(normalizedWrite{
		project:     "reviewer-append",
		fileName:    "notes/atomic.md",
		content:     "must not publish",
		topicPath:   "reviewer/append",
		tags:        []string{"legal_hold"},
		lifecycle:   "retired",
		storageTier: "retired",
	}); err == nil {
		t.Fatal("expected history append failure")
	}
	current, ok := store.currentStateFor("reviewer-append", "notes/atomic.md")
	if !ok {
		t.Fatal("baseline current state disappeared after append failure")
	}
	if current.Entry.EventID != baseline.EventID || current.Entry.ContentHash != baseline.ContentHash ||
		current.Entry.Lifecycle != "durable" || current.Entry.StorageTier != "hot" || current.LegalHold {
		t.Fatalf("append failure published non-durable state: %#v", current)
	}
	if len(store.recent) != baselineRecent {
		t.Fatalf("append failure changed recent history: before=%d after=%d", baselineRecent, len(store.recent))
	}

	store.policy.historyPath = originalHistoryPath
	if _, _, err := store.put(normalizedWrite{
		project:     "reviewer-append",
		fileName:    "notes/atomic.md",
		content:     "must not publish",
		topicPath:   "reviewer/append",
		tags:        []string{"legal_hold"},
		lifecycle:   "retired",
		storageTier: "retired",
	}); err != nil {
		t.Fatalf("retry after append repair: %v", err)
	}
}

func TestVectorRowsSuppressStaleLifecycleProjection(t *testing.T) {
	root := t.TempDir()
	store := newMemoryReviewerStore(t, root, 16)
	put := func(fileName, content, lifecycle, tier string, tags []string) memoryStoreEntry {
		t.Helper()
		entry, _, err := store.put(normalizedWrite{
			project:     "reviewer-vector",
			fileName:    fileName,
			content:     content,
			topicPath:   "reviewer/vector",
			tags:        tags,
			lifecycle:   lifecycle,
			storageTier: tier,
		})
		if err != nil {
			t.Fatalf("write %s: %v", fileName, err)
		}
		return entry
	}
	active := put("notes/active.md", "active", "durable", "hot", nil)
	held := put("notes/held.md", "held", "durable", "hot", []string{"legal_hold"})
	retired := put("notes/retired.md", "retired", "retired", "retired", nil)
	deep := put("notes/deep.md", "deep", "durable", "deep", nil)
	s := &server{memoryStore: store}
	rows := []map[string]any{
		{"project": "reviewer-vector", "file": active.FileName, "source": sourceQdrant, "lifecycle": "retired", "content_hash": active.ContentHash},
		{"project": "reviewer-vector", "file": active.FileName, "source": sourceQdrant, "lifecycle": "durable", "content_hash": sha256Hex("stale")},
		{"project": "reviewer-vector", "file": held.FileName, "source": sourcePgvector, "lifecycle": "durable"},
		{"project": "reviewer-vector", "file": retired.FileName, "source": sourceQdrant, "lifecycle": "durable", "content_hash": retired.ContentHash},
		{"project": "reviewer-vector", "file": deep.FileName, "source": sourcePgvector, "lifecycle": "durable"},
		{"project": "reviewer-vector", "file": "notes/missing-authority.md", "source": sourceQdrant, "lifecycle": "durable"},
	}

	filtered, suppressed := s.reconcileVectorRows(map[string]any{}, rows)
	if suppressed != 4 || len(filtered) != 2 {
		t.Fatalf("ordinary vector reconciliation mismatch: suppressed=%d rows=%#v", suppressed, filtered)
	}
	byFile := map[string]map[string]any{}
	for _, row := range filtered {
		byFile[anyToString(row["file"])] = row
	}
	if anyToString(byFile[active.FileName]["lifecycle"]) != "durable" {
		t.Fatalf("vector projection overrode active canonical lifecycle: %#v", byFile[active.FileName])
	}
	if !anyToBool(byFile[held.FileName]["legal_hold"]) {
		t.Fatalf("legal hold was not reconciled from authority: %#v", byFile[held.FileName])
	}
	withCold, suppressed := s.reconcileVectorRows(map[string]any{"include_cold": true}, rows)
	if suppressed != 3 || len(withCold) != 3 {
		t.Fatalf("include_cold reconciliation mismatch: suppressed=%d rows=%#v", suppressed, withCold)
	}
}

func TestVectorRowsRespectExplicitVectorOnlyMode(t *testing.T) {
	rows := []map[string]any{{
		"project": "vector-only",
		"file":    "notes/result.md",
		"source":  sourceQdrant,
	}}
	disabledStore := &memoryStore{policy: memoryStorePolicy{enabled: false}}
	filtered, suppressed := (&server{memoryStore: disabledStore}).reconcileVectorRows(map[string]any{}, rows)
	if suppressed != 0 || len(filtered) != 1 || anyToString(filtered[0]["file"]) != "notes/result.md" {
		t.Fatalf("explicit vector-only mode must preserve rows: suppressed=%d rows=%#v", suppressed, filtered)
	}

	filtered, suppressed = (&server{}).reconcileVectorRows(map[string]any{}, rows)
	if suppressed != 1 || len(filtered) != 0 {
		t.Fatalf("unexpectedly missing lifecycle authority must fail closed: suppressed=%d rows=%#v", suppressed, filtered)
	}
}

func TestVectorRowsPreferCurrentEventAndDeduplicateLegacyPath(t *testing.T) {
	root := t.TempDir()
	store := newMemoryReviewerStore(t, root, 16)
	entry, _, err := store.put(normalizedWrite{
		project:   "projection-identity",
		fileName:  "notes/current.md",
		content:   "authoritative current content",
		topicPath: "projection/current",
		lifecycle: "durable",
	})
	if err != nil {
		t.Fatalf("write current entry: %v", err)
	}
	s := &server{memoryStore: store}
	rows := []map[string]any{
		{
			"project": "projection-identity", "file": entry.FileName,
			"event_id": entry.EventID, "content_hash": sha256Hex("legacy projection hash"),
			"summary": "stale projected summary", "score": 0.91,
		},
		{
			"project": "projection-identity", "file": entry.FileName,
			"summary": "unidentified legacy duplicate", "score": 0.88,
		},
		{
			"project": "projection-identity", "file": entry.FileName,
			"event_id": "stale-event", "content_hash": entry.ContentHash,
			"summary": "stale event with matching content", "score": 0.87,
		},
	}

	filtered, stats := s.reconcileVectorRowsDetailed(map[string]any{}, rows)
	if len(filtered) != 1 || stats.Suppressed != 2 || stats.CurrentEvent != 1 ||
		stats.StaleEvent != 1 || stats.DuplicatePath != 1 {
		t.Fatalf("unexpected identity reconciliation rows=%#v stats=%#v", filtered, stats)
	}
	current := filtered[0]
	if anyToString(current["projection_authority"]) != "current_event" ||
		anyToString(current["event_id"]) != entry.EventID ||
		anyToString(current["content_hash"]) != entry.ContentHash ||
		anyToString(current["summary"]) != entry.Summary {
		t.Fatalf("projection did not resolve to owner authority: %#v", current)
	}
}

func TestCanonicalMemoryContentHashMatchesOwnerWrite(t *testing.T) {
	root := t.TempDir()
	store := newMemoryReviewerStore(t, root, 16)
	entry, _, err := store.put(normalizedWrite{
		project:  "projection-hash",
		fileName: "notes/hash.md",
		content:  "same logical content",
	})
	if err != nil {
		t.Fatalf("write owner entry: %v", err)
	}
	if entry.ContentHash != canonicalMemoryContentHash("same logical content") {
		t.Fatalf("projection hash %q does not match owner hash %q", canonicalMemoryContentHash("same logical content"), entry.ContentHash)
	}
	if canonicalMemoryContentHash("same logical content\n") != entry.ContentHash {
		t.Fatalf("canonical hash changed when content already ended in newline")
	}
}
