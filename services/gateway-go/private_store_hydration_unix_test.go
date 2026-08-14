//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestCompletedOwnerOnlyReceiptDefersHydrationBeforeListen(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	configureOwnerOnlyMemoryStoreTest(t, root)
	t.Setenv("GO_MEMORY_STORE_BACKGROUND_HYDRATION_ENABLED", "true")
	if err := migrateOwnerOnlyStore(root); err != nil {
		t.Fatalf("prepare completed owner-only receipt: %v", err)
	}

	policy := loadMemoryStorePolicy()
	fixture := &memoryStore{policy: policy}
	shardPath := fixture.currentStateShardPath(0)
	if err := os.MkdirAll(filepath.Dir(shardPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(shardPath, 0o600); err != nil {
		t.Fatalf("create hydration holdout: %v", err)
	}

	type result struct {
		store *memoryStore
		err   error
	}
	resultCh := make(chan result, 1)
	startedAt := time.Now()
	go func() {
		store, err := newMemoryStoreFromEnv()
		resultCh <- result{store: store, err: err}
	}()

	var initialized result
	select {
	case initialized = <-resultCh:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("completed-store hydration blocked on a non-regular shard")
	}
	if initialized.err != nil {
		t.Fatalf("construct memory store: %v", initialized.err)
	}
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("constructor exceeded startup bound: %s", elapsed)
	}
	initial := initialized.store.migrationSnapshot()
	if anyToString(initial["phase"]) != "hydrating" ||
		!anyToBool(initial["background"]) ||
		anyToBool(initial["ready"]) {
		t.Fatalf("expected fail-closed background hydration, got %#v", initial)
	}

	deadline := time.Now().Add(5 * time.Second)
	for anyToString(initialized.store.migrationSnapshot()["phase"]) != "blocked" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	final := initialized.store.migrationSnapshot()
	if anyToString(final["phase"]) != "blocked" || initialized.store.isEnabled() {
		t.Fatalf("non-regular shard was not rejected fail-closed: %#v", final)
	}
}
