package main

import (
	"context"
	"errors"
	"testing"
)

func TestDreamTransferCASPreservesNewerOrdinaryWriteAcrossOwners(t *testing.T) {
	t.Setenv("GO_MEMORY_STORE_BACKGROUND_HYDRATION_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_BACKGROUND_ENABLED", "false")
	stale := newDerivedMemoryTestStore(t)
	const project, file = "destination", "dream-transfers/test.json"
	item := normalizedWrite{project: project, topicPath: "review", fileName: file, content: "frozen projection"}
	first, _, err := stale.put(item)
	if err != nil {
		t.Fatal(err)
	}
	open := func() *memoryStore {
		t.Helper()
		s, err := newMemoryStoreFromEnv()
		if err != nil || !s.isEnabled() {
			t.Fatalf("open independent memory writer: %v", err)
		}
		return s
	}
	ordinary := open()
	item.content = "newer observation from ordinary writer"
	newer, _, err := ordinary.put(item)
	if err != nil || newer.ContentHash == first.ContentHash {
		t.Fatalf("independent write did not advance the disk contents: %v", err)
	}
	stale.mu.RLock()
	cached := stale.latestHash[memoryStoreKey(project, file)]
	stale.mu.RUnlock()
	if cached != first.ContentHash {
		t.Fatal("fixture did not retain the stale writer view")
	}
	item.content = "attempted rematerialization"
	if _, _, err := stale.putDreamTransferProjectionCAS(item, first.ContentHash, false); !errors.Is(err, errMemoryStoreWritePrecondition) {
		t.Fatalf("stale transfer precondition was not rejected: %v", err)
	}
	current, err := open().fetchByMemoryID(context.Background(), project+"::"+file)
	row := anyMap(current["memory"])
	if err != nil || row["event_id"] != newer.EventID || row["content_hash"] != newer.ContentHash || row["content"] != "newer observation from ordinary writer\n" {
		t.Fatalf("ordinary observation was not preserved: %v", err)
	}
}
