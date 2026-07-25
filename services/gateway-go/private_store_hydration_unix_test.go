//go:build !windows

package main

import (
	"encoding/json"
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
		releaseErr := make(chan error, 1)
		go func() { releaseErr <- writeHydrationFIFO(shardPath) }()
		select {
		case initialized = <-resultCh:
			<-releaseErr
			t.Fatalf("completed-store hydration blocked constructor before listen: %v", initialized.err)
		case <-time.After(5 * time.Second):
			t.Fatal("completed-store hydration remained blocked after holdout release")
		}
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

	releaseHydrationFIFO(t, shardPath)
	deadline := time.Now().Add(5 * time.Second)
	for !initialized.store.isEnabled() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !initialized.store.isEnabled() {
		t.Fatalf("hydration did not complete: %#v", initialized.store.migrationSnapshot())
	}
}

func releaseHydrationFIFO(t *testing.T, path string) {
	t.Helper()
	if err := writeHydrationFIFO(path); err != nil {
		t.Fatal(err)
	}
}

func writeHydrationFIFO(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	payload := memoryCurrentStateShard{
		SchemaID: memoryCurrentStateSchemaID,
		Version:  1,
		Shard:    0,
		Entries:  []memoryCurrentState{},
	}
	if err := json.NewEncoder(file).Encode(payload); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return nil
}
