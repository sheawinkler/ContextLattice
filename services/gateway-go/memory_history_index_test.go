package main

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func newHistoryIndexTestStore() *memoryStore {
	return &memoryStore{
		policy: memoryStorePolicy{
			enabled:               true,
			rollupUseHistoryIndex: true,
			maxRecent:             100000,
			maxSummaryChars:       400,
			confidencePriorAlpha:  1,
			confidencePriorBeta:   1,
			confidenceWriteWeight: 0.5,
			confidenceReadWeight:  1,
		},
		recent:                    make([]memoryStoreEntry, 0, 1024),
		currentState:              map[string]memoryCurrentState{},
		currentKeysByProject:      map[string]map[string]struct{}{},
		currentKeyCountsByProject: map[string]int{},
		latestTopic:               map[string]string{},
		latestHash:                map[string]string{},
		latestHorizon:             map[string]int{},
		latestLifecycle:           map[string]string{},
		latestStorageTier:         map[string]string{},
		lastAccess:                map[string]time.Time{},
		confidence:                map[string]confidenceState{},
		rollupCache:               map[string]topicRollupCacheEntry{},
		exactStatePaths:           map[string]struct{}{},
	}
}

func historyIndexTestEntry(project, fileName string, at time.Time, tombstone bool) memoryStoreEntry {
	entry := memoryStoreEntry{
		EventID:     fmt.Sprintf("event-%s-%s-%d", project, fileName, at.UnixNano()),
		Project:     project,
		FileName:    fileName,
		TopicPath:   "notes",
		Summary:     "indexed memory",
		ContentHash: fmt.Sprintf("hash-%d", at.UnixNano()),
		ContentRef:  fmt.Sprintf("sha256:hash-%d", at.UnixNano()),
		ObjectID:    fmt.Sprintf("object-%d", at.UnixNano()),
		CreatedAt:   at.UTC().Format(time.RFC3339Nano),
		Lifecycle:   "durable",
		StorageTier: "hot",
		Confidence:  0.8,
		Source:      "test",
	}
	if tombstone {
		entry.DataClass = "memory_tombstone"
		entry.Summary = "deleted"
	}
	return entry
}

func recordHistoryIndexTestEntry(store *memoryStore, entry memoryStoreEntry) {
	store.mu.Lock()
	store.recordEntry(entry)
	store.mu.Unlock()
}

func TestMemoryStoreProjectHistoryIndexScopesSnapshot(t *testing.T) {
	store := newHistoryIndexTestStore()
	base := time.Unix(1700000000, 0).UTC()
	for i := 0; i < 10000; i++ {
		recordHistoryIndexTestEntry(store, historyIndexTestEntry(
			fmt.Sprintf("unrelated-%d", i), "notes/other.md", base.Add(time.Duration(i)*time.Nanosecond), false,
		))
	}
	recordHistoryIndexTestEntry(store, historyIndexTestEntry("Alpha", "notes/a.md", base.Add(time.Hour), false))
	recordHistoryIndexTestEntry(store, historyIndexTestEntry("alpha", "notes/b.md", base.Add(2*time.Hour), false))

	docs, ok := store.collectDocsFromHistoryIndex(context.Background(), " alpha ", true, true)
	if !ok {
		t.Fatal("expected project-scoped history index to be usable")
	}
	if len(docs) != 2 {
		t.Fatalf("expected two selected-project docs, got %d: %#v", len(docs), docs)
	}
	for _, doc := range docs {
		if doc.Project != "Alpha" && doc.Project != "alpha" {
			t.Fatalf("unrelated project leaked into scoped docs: %#v", doc)
		}
	}
	store.mu.RLock()
	indexed := len(store.currentKeysByProject["alpha"])
	store.mu.RUnlock()
	if indexed != 2 {
		t.Fatalf("expected two active keys in alpha index, got %d", indexed)
	}
}

func TestMemoryStoreProjectHistoryIndexRemovesTombstones(t *testing.T) {
	store := newHistoryIndexTestStore()
	base := time.Unix(1700000000, 0).UTC()
	recordHistoryIndexTestEntry(store, historyIndexTestEntry("alpha", "notes/deleted.md", base, false))
	recordHistoryIndexTestEntry(store, historyIndexTestEntry("alpha", "notes/deleted.md", base.Add(time.Hour), true))

	store.mu.RLock()
	indexedKeys, indexed := store.currentKeysByProject["alpha"]
	indexedCount, countKnown := store.currentKeyCountsByProject["alpha"]
	current, currentOK := store.currentState[memoryStoreKey("alpha", "notes/deleted.md")]
	store.mu.RUnlock()
	if !indexed || !countKnown || len(indexedKeys) != 0 || indexedCount != 0 {
		t.Fatalf("tombstoned project should retain an explicit zero marker, indexed=%v count_known=%v keys=%d count=%d", indexed, countKnown, len(indexedKeys), indexedCount)
	}
	if !currentOK || !current.Tombstone {
		t.Fatal("expected tombstone to remain authoritative in current state")
	}
	if docs, ok := store.collectDocsFromHistoryIndex(context.Background(), "alpha", true, true); !ok || len(docs) != 0 {
		t.Fatalf("tombstoned project should remain authoritatively empty, ok=%v docs=%#v", ok, docs)
	}
}

func TestMemoryStoreProjectHistoryIndexTreatsUnknownProjectAsEmpty(t *testing.T) {
	store := newHistoryIndexTestStore()
	recordHistoryIndexTestEntry(store, historyIndexTestEntry(
		"known", "notes/known.md", time.Unix(1700000000, 0).UTC(), false,
	))

	docs, ok := store.collectDocsFromHistoryIndex(context.Background(), "unknown", true, true)
	if !ok || len(docs) != 0 {
		t.Fatalf("materialized project index should make an unknown project authoritatively empty, ok=%v docs=%#v", ok, docs)
	}
}

func TestMemoryStoreProjectHistoryIndexRestoresOnStartup(t *testing.T) {
	store := newHistoryIndexTestStore()
	entry := historyIndexTestEntry("alpha", "notes/restarted.md", time.Unix(1700000000, 0).UTC(), false)
	key := memoryStoreKey(entry.Project, entry.FileName)
	store.currentState[key] = memoryCurrentStateFromEntry(entry)
	store.restoreLatestIndexesFromCurrentStateLocked()

	store.mu.RLock()
	_, indexed := store.currentKeysByProject["alpha"][key]
	store.mu.RUnlock()
	if !indexed {
		t.Fatal("startup restore did not rebuild project index")
	}
	docs, ok := store.collectDocsFromHistoryIndex(context.Background(), "alpha", true, true)
	if !ok || len(docs) != 1 || docs[0].FileName != entry.FileName {
		t.Fatalf("restored index returned wrong docs, ok=%v docs=%#v", ok, docs)
	}
}

func TestMemoryStoreProjectHistoryIndexFailsClosedOnInconsistency(t *testing.T) {
	store := newHistoryIndexTestStore()
	base := time.Unix(1700000000, 0).UTC()
	recordHistoryIndexTestEntry(store, historyIndexTestEntry("alpha", "notes/valid.md", base, false))
	store.mu.Lock()
	store.currentKeysByProject["alpha"][memoryStoreKey("alpha", "notes/missing.md")] = struct{}{}
	store.mu.Unlock()

	if docs, ok := store.collectDocsFromHistoryIndex(context.Background(), "alpha", true, true); ok || len(docs) != 0 {
		t.Fatalf("inconsistent project index must fail closed, ok=%v docs=%#v", ok, docs)
	}
}

func TestMemoryStoreProjectHistoryIndexHonorsCancellation(t *testing.T) {
	store := newHistoryIndexTestStore()
	base := time.Unix(1700000000, 0).UTC()
	for i := 0; i < 1000; i++ {
		recordHistoryIndexTestEntry(store, historyIndexTestEntry(
			"alpha", fmt.Sprintf("notes/%d.md", i), base.Add(time.Duration(i)*time.Nanosecond), false,
		))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	docs, ok := store.collectDocsFromHistoryIndex(ctx, "alpha", true, true)
	if !ok || len(docs) != 0 {
		t.Fatalf("canceled collection should stop before snapshot, ok=%v docs=%d", ok, len(docs))
	}
}

func TestMemoryStoreProjectHistoryIndexConcurrentMutation(t *testing.T) {
	store := newHistoryIndexTestStore()
	base := time.Unix(1700000000, 0).UTC()
	recordHistoryIndexTestEntry(store, historyIndexTestEntry("alpha", "notes/live.md", base, false))

	const iterations = 500
	done := make(chan struct{})
	go func() {
		for i := 1; i <= iterations; i++ {
			recordHistoryIndexTestEntry(store, historyIndexTestEntry("alpha", "notes/live.md", base.Add(time.Duration(i)*time.Nanosecond), false))
		}
		close(done)
	}()
	for i := 0; i < iterations; i++ {
		docs, ok := store.collectDocsFromHistoryIndex(context.Background(), "alpha", true, true)
		if !ok {
			// Same-key rewrites advance the exact collector generation. A
			// collection that overlaps one must fail closed rather than publish
			// a stale snapshot; verify below that a stable post-writer read still
			// returns the active key.
			continue
		}
		if len(docs) != 1 {
			t.Fatalf("concurrent scoped collection lost active key, ok=%v docs=%#v", ok, docs)
		}
	}
	<-done
	docs, ok := store.collectDocsFromHistoryIndex(context.Background(), "alpha", true, true)
	if !ok || len(docs) != 1 {
		t.Fatalf("stable scoped collection lost active key after concurrent rewrites, ok=%v docs=%#v", ok, docs)
	}
}

func BenchmarkMemoryStoreProjectHistoryIndexCollection(b *testing.B) {
	store := newHistoryIndexTestStore()
	base := time.Unix(1700000000, 0).UTC()
	for i := 0; i < 20000; i++ {
		recordHistoryIndexTestEntry(store, historyIndexTestEntry(
			fmt.Sprintf("unrelated-%d", i), "notes/other.md", base.Add(time.Duration(i)*time.Nanosecond), false,
		))
	}
	for i := 0; i < 100; i++ {
		recordHistoryIndexTestEntry(store, historyIndexTestEntry(
			"target", fmt.Sprintf("notes/%d.md", i), base.Add(time.Duration(i+30000)*time.Nanosecond), false,
		))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		docs, ok := store.collectDocsFromHistoryIndex(context.Background(), "target", true, true)
		if !ok || len(docs) != 100 {
			b.Fatalf("scoped collection changed during benchmark, ok=%v docs=%d", ok, len(docs))
		}
	}
}
