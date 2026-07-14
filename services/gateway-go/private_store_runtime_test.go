package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func configureOwnerOnlyMemoryStoreTest(t *testing.T, root string) {
	t.Helper()
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", filepath.Join(root, "_contextlattice", "history.ndjson"))
	t.Setenv("GO_MEMORY_STORE_ACCESS_LOG_PATH", filepath.Join(root, "_contextlattice", "access.ndjson"))
	t.Setenv("GO_MEMORY_GRAPH_EDGE_PATH", filepath.Join(root, "_contextlattice", "edges.ndjson"))
}

func TestOwnerOnlyMemoryStoreStartupYieldsAndBecomesReadyInBackground(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2048; index++ {
		path := filepath.Join(root, fmt.Sprintf("entry-%05d.json", index))
		if err := os.WriteFile(path, []byte(`{"private":true}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	configureOwnerOnlyMemoryStoreTest(t, root)
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_STARTUP_BUDGET_MILLIS", "1")
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_MAX_ENTRIES", "256")
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_BACKGROUND_ENABLED", "true")

	startedAt := time.Now()
	store, err := newMemoryStoreFromEnv()
	startupDuration := time.Since(startedAt)
	if err != nil {
		t.Fatalf("bounded startup failed: %v", err)
	}
	if startupDuration > 500*time.Millisecond {
		t.Fatalf("owner-only migration blocked startup for %s", startupDuration)
	}
	initial := store.migrationSnapshot()
	if anyToString(initial["phase"]) != "migrating" || anyToBool(initial["ready"]) {
		t.Fatalf("expected fail-closed background migration, got %#v", initial)
	}
	deadline := time.Now().Add(15 * time.Second)
	for !store.isEnabled() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !store.isEnabled() {
		t.Fatalf("memory store never became ready: %#v", store.migrationSnapshot())
	}
	final := store.migrationSnapshot()
	if anyToString(final["phase"]) != "ready" || !anyToBool(final["ready"]) {
		t.Fatalf("unexpected completed migration status: %#v", final)
	}
	encoded, err := json.Marshal(final)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), root) {
		t.Fatalf("migration status leaked store path: %s", encoded)
	}
	assertMode(t, filepath.Join(root, "entry-02047.json"), 0o600)
}

func TestOwnerOnlyMigrationSerializesConcurrentWorkers(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "memory.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, err := migrateOwnerOnlyStoreWithOptions(root, ownerOnlyMigrationOptions{
			beforeEntry: func(string) error {
				select {
				case <-entered:
				default:
					close(entered)
				}
				<-release
				return nil
			},
		})
		firstDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first migration worker did not acquire the lock")
	}

	const contenders = 16
	var wg sync.WaitGroup
	errorsSeen := make(chan error, contenders)
	for index := 0; index < contenders; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := migrateOwnerOnlyStoreWithOptions(root, ownerOnlyMigrationOptions{})
			errorsSeen <- err
		}()
	}
	wg.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if !errors.Is(err, errOwnerOnlyMigrationLocked) {
			t.Fatalf("concurrent migration was not rejected as busy: %v", err)
		}
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("serialized migration worker failed: %v", err)
	}
}

func TestOwnerOnlyMigrationInvalidatesRootIdentityMismatch(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "memory.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := migrateOwnerOnlyStore(root); err != nil {
		t.Fatal(err)
	}
	state, err := loadOwnerOnlyMigrationState(ownerOnlyStatePath(root))
	if err != nil {
		t.Fatal(err)
	}
	state.RootIdentity = "different-root"
	if err := persistOwnerOnlyMigrationState(ownerOnlyStatePath(root), state); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := migrateOwnerOnlyStoreWithOptions(root, ownerOnlyMigrationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete || report.ProcessedEntries == 0 {
		t.Fatalf("root identity mismatch incorrectly trusted completed receipt: %+v", report)
	}
	assertMode(t, path, 0o600)
}

func TestOwnerOnlyMigrationFailsClosedBeyondDepthLimit(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	path := root
	for index := 0; index < 10; index++ {
		path = filepath.Join(path, fmt.Sprintf("level-%02d", index))
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_MAX_DEPTH", "8")
	if _, err := migrateOwnerOnlyStoreWithOptions(root, ownerOnlyMigrationOptions{}); err == nil || !strings.Contains(err.Error(), "maximum traversal depth") {
		t.Fatalf("expected bounded traversal depth failure, got %v", err)
	}
}

func TestOwnerOnlyBlockedStoreIsNotWriteReady(t *testing.T) {
	store := &memoryStore{
		policy:    memoryStorePolicy{enabled: true},
		migration: newOwnerOnlyMigrationRuntime("private-root", true),
	}
	store.migration.markBlocked(ownerOnlyMigrationReport{}, errors.New("synthetic"))
	if store.isEnabled() {
		t.Fatal("blocked memory store reported ready")
	}
	if _, _, err := store.put(normalizedWrite{project: "p", fileName: "note.md", content: "x"}); err == nil {
		t.Fatal("blocked memory store accepted a write")
	}

	recorder := httptest.NewRecorder()
	(&server{memoryStore: store}).info(recorder, httptest.NewRequest("GET", "/v1/info", nil))
	body := recorder.Body.String()
	if strings.Contains(body, "private-root") || strings.Contains(body, "rootPath") {
		t.Fatalf("info endpoint exposed a raw store path: %s", body)
	}
	if !strings.Contains(body, `"phase":"blocked"`) {
		t.Fatalf("info endpoint omitted blocked migration status: %s", body)
	}

	gateway := httptest.NewServer(buildMux(&server{memoryStore: store}))
	defer gateway.Close()
	response, err := http.Post(
		gateway.URL+"/memory/write",
		"application/json",
		strings.NewReader(`{"projectName":"p","fileName":"note.md","content":"x"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("blocked store write status=%d, want 503", response.StatusCode)
	}
}
