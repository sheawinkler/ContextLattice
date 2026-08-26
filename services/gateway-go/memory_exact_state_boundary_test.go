package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newExactStateBoundaryTestStore(t *testing.T) (*memoryStore, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "memory-bank")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", filepath.Join(root, "_contextlattice", "memory_write_history.ndjson"))
	t.Setenv("GO_MEMORY_STORE_ACCESS_LOG_PATH", filepath.Join(root, "_contextlattice", "memory_access.ndjson"))
	t.Setenv("GO_MEMORY_GRAPH_EDGE_PATH", filepath.Join(root, "_contextlattice", "memory_edges.ndjson"))
	t.Setenv("GO_MEMORY_STORE_CONTENT_ADDRESSING_ENABLED", "false")
	store, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("create exact-state boundary store: %v", err)
	}
	return store, root
}

func TestExactStateRowsFailClosedAcrossSemanticSources(t *testing.T) {
	store, _ := newExactStateBoundaryTestStore(t)
	if err := store.registerExactStatePath("contextlattice", "runtime/registered.json"); err != nil {
		t.Fatal(err)
	}
	s := &server{memoryStore: store, writePolicy: loadWriteIngressPolicy()}
	normalizedRuntime := s.normalizeSourceRows(sourceQdrant, []map[string]any{
		{
			"project":    "other",
			"file":       "runtime/state.json",
			"summary":    "source classified runtime state",
			"score":      1.0,
			"data_class": dataClassRuntimeStateMirror,
		},
	})
	if len(normalizedRuntime) != 1 || anyToString(normalizedRuntime[0]["data_class"]) != dataClassRuntimeStateMirror {
		t.Fatalf("source normalization lost exact-state classification: %#v", normalizedRuntime)
	}

	rows := []map[string]any{
		{"project": "contextlattice", "file": "notes/learning.md", "summary": "ordinary", "source": sourceQdrant},
		{"project": "contextlattice", "file": "runtime/registered.json", "summary": "exact", "source": sourceQdrant},
		{"project": "contextlattice", "file": "runtime//./registered.json", "summary": "exact alias", "source": sourceQdrant},
		{"project": "contextlattice", "file": `runtime\registered.json`, "summary": "exact alias", "source": sourceQdrant},
		{"project": "contextlattice", "file": "/runtime/registered.json", "summary": "exact alias", "source": sourceQdrant},
		{"project": "contextlattice", "file": "../runtime/registered.json", "summary": "traversal", "source": sourceQdrant},
		{"project": "_contextlattice", "file": "memory_write_history.ndjson", "summary": "internal", "source": sourceMemoryBank},
		normalizedRuntime[0],
	}

	filtered, suppressed := s.filterExactStateRows(rows)
	if suppressed != 7 {
		t.Fatalf("suppressed = %d, want 7", suppressed)
	}
	if len(filtered) != 1 || anyToString(filtered[0]["file"]) != "notes/learning.md" {
		t.Fatalf("filtered rows = %#v", filtered)
	}
}

func TestExactStateSourceRequestOverfetchesBeforeFiltering(t *testing.T) {
	store, _ := newExactStateBoundaryTestStore(t)
	if err := store.registerExactStatePath("project", "runtime/state.json"); err != nil {
		t.Fatal(err)
	}
	s := &server{memoryStore: store}
	base := map[string]any{"query": "state", "limit": 1}
	request := s.exactStateSourceRequest(base, sourceQdrant)
	if got := anyToInt(request["limit"], 0); got <= 1 || got > 100 {
		t.Fatalf("source request limit = %d, want bounded overfetch", got)
	}
	if got := anyToInt(base["limit"], 0); got != 1 {
		t.Fatalf("source overfetch mutated final request limit: %d", got)
	}
	letRequest := s.exactStateSourceRequest(base, sourceLetta)
	if got := anyToInt(letRequest["limit"], 0); got != 1 {
		t.Fatalf("letta mode contract limit changed by exact-state overfetch: %d", got)
	}
}

func TestExactStateSourceRequestDoesNotWaitForRegistryPersistenceLock(t *testing.T) {
	store, _ := newExactStateBoundaryTestStore(t)
	if err := store.registerExactStatePath("project", "runtime/state.json"); err != nil {
		t.Fatal(err)
	}
	s := &server{memoryStore: store}
	store.mu.Lock()
	defer store.mu.Unlock()

	done := make(chan map[string]any, 1)
	go func() {
		done <- s.exactStateSourceRequest(map[string]any{"limit": 1}, sourceQdrant)
	}()
	select {
	case request := <-done:
		if got := anyToInt(request["limit"], 0); got <= 1 {
			t.Fatalf("source request did not use atomic exact-state count: %d", got)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("source overfetch waited on exact-state registry persistence lock")
	}
}

func TestExactStateRegistryRejectsAmbiguousMemoryKeys(t *testing.T) {
	if _, err := sanitizeMemoryProject("project::alias"); err == nil {
		t.Fatal("project delimiter must be rejected")
	}
	if _, err := sanitizeMemoryFile("runtime::state.json"); err == nil {
		t.Fatal("file delimiter must be rejected")
	}
	if _, _, ok := parseMemoryStoreKeyToken("project::runtime::state.json"); ok {
		t.Fatal("ambiguous registry key must be rejected")
	}

	root := filepath.Join(t.TempDir(), "memory-bank")
	indexPath := filepath.Join(root, "_contextlattice", "exact_state_paths.json")
	if err := os.MkdirAll(filepath.Dir(indexPath), 0o700); err != nil {
		t.Fatal(err)
	}
	raw := "{\"schema_id\":\"contextlattice_exact_state_index.v1\",\"paths\":[\"project::runtime::state.json\"]}"
	if err := os.WriteFile(indexPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &memoryStore{
		policy: memoryStorePolicy{
			enabled:             true,
			rootPath:            root,
			exactStateIndexPath: indexPath,
			exactStateMaxPaths:  100,
		},
		exactStatePaths: map[string]struct{}{},
	}
	if err := store.loadExactStateIndex(); err == nil {
		t.Fatal("ambiguous persisted registry key must fail closed")
	}
}

func TestMemoryStoreRejectsSymlinkRootBeforeRegistryInitialization(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "memory-bank")
	if err := os.Symlink(target, root); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)

	if _, err := newMemoryStoreFromEnv(); err == nil || !strings.Contains(err.Error(), "validate memory store root") {
		t.Fatalf("expected root symlink rejection, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "_contextlattice")); !os.IsNotExist(err) {
		t.Fatalf("registry initialization mutated symlink target: %v", err)
	}
}

func TestRegisterExactStateRemovesAndRejectsGraphEdges(t *testing.T) {
	store, _ := newExactStateBoundaryTestStore(t)
	edge := memoryEdgeEntry{
		SourceID:   "project::notes/source.md",
		TargetID:   "project::notes/target.md",
		Relation:   "supports",
		Confidence: 0.9,
		CreatedAt:  nowUTCISO(),
	}
	if _, err := store.upsertMemoryEdge(context.Background(), edge); err != nil {
		t.Fatalf("seed graph edge: %v", err)
	}
	if err := store.registerExactStatePath("project", "/notes//./source.md"); err != nil {
		t.Fatalf("register exact state: %v", err)
	}
	edges, err := store.listMemoryEdges(context.Background(), memoryEdgeQuery{Limit: 10})
	if err != nil || len(edges) != 0 {
		t.Fatalf("registered exact state retained graph edges: edges=%#v err=%v", edges, err)
	}
	if _, err := store.upsertMemoryEdge(context.Background(), edge); err == nil ||
		!strings.Contains(err.Error(), "exact state") {
		t.Fatalf("edge touching exact state must be rejected, got %v", err)
	}

	// Reload before pruning to prove the startup filter independently rejects
	// the stale append-only edge log.
	reloaded, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("reload store before graph prune: %v", err)
	}
	edges, err = reloaded.listMemoryEdges(context.Background(), memoryEdgeQuery{Limit: 10})
	if err != nil || len(edges) != 0 {
		t.Fatalf("startup load resurrected exact-state semantics: edges=%#v err=%v", edges, err)
	}

	pruneResult, err := store.pruneVolatileMemoryGraphEdges(context.Background(), false)
	if err != nil {
		t.Fatalf("prune exact-state graph edge: %v", err)
	}
	if pruneResult["kept"] != 0 || pruneResult["skipped_exact_state"] != 1 {
		t.Fatalf("graph prune retained exact-state edge: %#v", pruneResult)
	}
	edgeLog, err := os.ReadFile(store.policy.edgePath)
	if err != nil || strings.TrimSpace(string(edgeLog)) != "" {
		t.Fatalf("graph prune did not remove persisted exact-state edge: log=%q err=%v", edgeLog, err)
	}
}

func TestConcurrentGraphUpsertAndExactRegistrationSerializeByCanonicalPath(t *testing.T) {
	store, _ := newExactStateBoundaryTestStore(t)
	edge := memoryEdgeEntry{
		SourceID:   "project::notes/source.md",
		TargetID:   "project::notes/target.md",
		Relation:   "supports",
		Confidence: 0.9,
		CreatedAt:  nowUTCISO(),
	}
	enteredEdgeCommit := make(chan struct{})
	releaseEdgeCommit := make(chan struct{})
	var hookOnce sync.Once
	store.beforeEdgeCommit = func() {
		hookOnce.Do(func() {
			close(enteredEdgeCommit)
			<-releaseEdgeCommit
		})
	}

	edgeDone := make(chan error, 1)
	go func() {
		_, err := store.upsertMemoryEdge(context.Background(), edge)
		edgeDone <- err
	}()
	<-enteredEdgeCommit

	registerStarted := make(chan struct{})
	registerDone := make(chan error, 1)
	go func() {
		close(registerStarted)
		registerDone <- store.registerExactStatePath("project", "/notes//./source.md")
	}()
	<-registerStarted
	select {
	case err := <-registerDone:
		t.Fatalf("exact registration crossed active graph append: %v", err)
	case <-time.After(250 * time.Millisecond):
	}
	close(releaseEdgeCommit)
	if err := <-edgeDone; err != nil {
		t.Fatalf("graph upsert failed: %v", err)
	}
	if err := <-registerDone; err != nil {
		t.Fatalf("exact registration failed after graph append: %v", err)
	}

	edges, err := store.listMemoryEdges(context.Background(), memoryEdgeQuery{Limit: 10})
	if err != nil || len(edges) != 0 {
		t.Fatalf("registered exact path retained a graph edge: edges=%#v err=%v", edges, err)
	}
	if _, err := store.upsertMemoryEdge(context.Background(), edge); err == nil ||
		!strings.Contains(err.Error(), "exact state") {
		t.Fatalf("post-registration graph upsert must fail closed, got %v", err)
	}
	store.pathLocksMu.Lock()
	activeLocks := len(store.pathLocks)
	store.pathLocksMu.Unlock()
	if activeLocks != 0 {
		t.Fatalf("graph path locks leaked: %d", activeLocks)
	}
}

func TestConcurrentOrdinaryAndExactWritesSerializeByCanonicalPath(t *testing.T) {
	store, _ := newExactStateBoundaryTestStore(t)
	enteredOrdinaryCommit := make(chan struct{})
	releaseOrdinaryCommit := make(chan struct{})
	var hookOnce sync.Once
	store.beforeOrdinaryCommit = func() {
		hookOnce.Do(func() {
			close(enteredOrdinaryCommit)
			<-releaseOrdinaryCommit
		})
	}

	type putResult struct {
		entry memoryStoreEntry
		err   error
	}
	ordinaryDone := make(chan putResult, 1)
	go func() {
		entry, _, err := store.put(normalizedWrite{
			project:   "project",
			fileName:  "runtime//./contended.json",
			content:   "{\"writer\":\"ordinary\"}",
			dataClass: dataClassLearningMemory,
		})
		ordinaryDone <- putResult{entry: entry, err: err}
	}()
	<-enteredOrdinaryCommit

	exactDone := make(chan putResult, 1)
	go func() {
		entry, _, err := store.put(normalizedWrite{
			project:   "project",
			fileName:  "runtime\\contended.json",
			content:   "{\"writer\":\"exact\"}",
			dataClass: dataClassRuntimeStateMirror,
		})
		exactDone <- putResult{entry: entry, err: err}
	}()

	select {
	case result := <-exactDone:
		t.Fatalf("exact write bypassed in-flight canonical path lock: entry=%#v err=%v", result.entry, result.err)
	case <-time.After(250 * time.Millisecond):
	}
	close(releaseOrdinaryCommit)

	ordinaryResult := <-ordinaryDone
	if ordinaryResult.err != nil {
		t.Fatalf("ordinary write failed: %v", ordinaryResult.err)
	}
	exactResult := <-exactDone
	if exactResult.err != nil || exactResult.entry.DataClass != dataClassRuntimeStateMirror {
		t.Fatalf("exact write failed: entry=%#v err=%v", exactResult.entry, exactResult.err)
	}
	content, _, err := store.readFile("project", "runtime/contended.json")
	if err != nil || !strings.Contains(content, "\"writer\":\"exact\"") {
		t.Fatalf("exact state did not win serialized transition: content=%q err=%v", content, err)
	}

	historyBefore, err := os.ReadFile(store.policy.historyPath)
	if err != nil {
		t.Fatalf("read pre-registration history: %v", err)
	}
	postEntry, _, err := store.put(normalizedWrite{
		project:   "project",
		fileName:  "/runtime/contended.json",
		content:   "{\"writer\":\"post-registration\"}",
		dataClass: dataClassLearningMemory,
	})
	if err != nil || postEntry.DataClass != dataClassRuntimeStateMirror {
		t.Fatalf("registered path escaped exact-state routing: entry=%#v err=%v", postEntry, err)
	}
	historyAfter, err := os.ReadFile(store.policy.historyPath)
	if err != nil {
		t.Fatalf("read post-registration history: %v", err)
	}
	if string(historyAfter) != string(historyBefore) {
		t.Fatal("post-registration write repopulated semantic history")
	}

	store.pathLocksMu.Lock()
	activeLocks := len(store.pathLocks)
	store.pathLocksMu.Unlock()
	if activeLocks != 0 {
		t.Fatalf("canonical path locks leaked: %d", activeLocks)
	}
}

func TestQdrantFanoutSerializesWithExactStateRegistration(t *testing.T) {
	store, _ := newExactStateBoundaryTestStore(t)
	t.Setenv("GO_RETRIEVAL_NATIVE_QDRANT_ENABLED", "true")
	t.Setenv("QDRANT_LOCAL_URL", "")
	t.Setenv("QDRANT_API_KEY", "")
	t.Setenv("ORCH_FASTEMBED_RS_BASE_URL", "")
	t.Setenv("ORCH_QDRANT_AUTO_CREATE_ON_STARTUP", "true")
	t.Setenv("ORCH_EMBED_PROVIDER", "cheap")
	t.Setenv("ORCH_QDRANT_EMBED_DIM", "768")

	enteredUpsert := make(chan struct{})
	releaseUpsert := make(chan struct{})
	var enteredOnce sync.Once
	var upsertCalls atomic.Int32
	qdrant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/collections/contextlattice_notes":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"status":{"error":"not found"}}`))
		case r.Method == http.MethodPut && r.URL.Path == "/collections/contextlattice_notes":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":true}`))
		case r.Method == http.MethodPut && r.URL.Path == "/collections/contextlattice_notes/points":
			upsertCalls.Add(1)
			enteredOnce.Do(func() { close(enteredUpsert) })
			<-releaseUpsert
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":{"status":"completed"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer qdrant.Close()
	t.Setenv("QDRANT_URL", qdrant.URL)

	s := newServer()
	s.memoryStore = store
	type fanoutResult struct {
		status string
		err    error
	}
	fanoutDone := make(chan fanoutResult, 1)
	item := normalizedWrite{
		project:   "project",
		fileName:  "runtime/state.json",
		content:   "ordinary semantic write",
		topicPath: "runtime",
		dataClass: dataClassLearningMemory,
	}
	go func() {
		status, err := s.upsertQdrantFromWrite(context.Background(), item, "event-ordinary")
		fanoutDone <- fanoutResult{status: status, err: err}
	}()
	select {
	case <-enteredUpsert:
	case <-time.After(5 * time.Second):
		t.Fatal("qdrant upsert did not start")
	}

	exactDone := make(chan error, 1)
	go func() {
		_, _, err := store.put(normalizedWrite{
			project:   "project",
			fileName:  "runtime\\state.json",
			content:   "{\"state\":\"exact\"}",
			dataClass: dataClassRuntimeStateMirror,
		})
		exactDone <- err
	}()
	select {
	case err := <-exactDone:
		t.Fatalf("exact registration crossed active qdrant upsert: %v", err)
	case <-time.After(250 * time.Millisecond):
	}
	close(releaseUpsert)
	result := <-fanoutDone
	if result.err != nil || result.status != "succeeded" {
		t.Fatalf("qdrant fanout failed: status=%s err=%v", result.status, result.err)
	}
	if err := <-exactDone; err != nil {
		t.Fatalf("exact state write failed after fanout: %v", err)
	}

	status, err := s.upsertQdrantFromWrite(context.Background(), item, "event-after-registration")
	if err != nil || status != "skipped_exact_state_mirror" {
		t.Fatalf("post-registration qdrant fanout was not skipped: status=%s err=%v", status, err)
	}
	if got := upsertCalls.Load(); got != 1 {
		t.Fatalf("qdrant received %d upserts, want 1", got)
	}
}

func TestQdrantFanoutRechecksExactStateAfterEmbeddingBeforeMutation(t *testing.T) {
	store, _ := newExactStateBoundaryTestStore(t)
	t.Setenv("GO_RETRIEVAL_NATIVE_QDRANT_ENABLED", "true")
	t.Setenv("GO_RETRIEVAL_NATIVE_FASTEMBED_ENABLED", "true")
	t.Setenv("ORCH_EMBED_PROVIDER", "fastembed-rs")
	t.Setenv("QDRANT_LOCAL_URL", "")
	t.Setenv("QDRANT_API_KEY", "")

	enteredEmbed := make(chan struct{})
	releaseEmbed := make(chan struct{})
	var enteredOnce sync.Once
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseEmbed) }) }
	defer release()
	embed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		enteredOnce.Do(func() { close(enteredEmbed) })
		<-releaseEmbed
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"vectors":[[1,0]]}`))
	}))
	defer embed.Close()
	t.Setenv("ORCH_FASTEMBED_RS_BASE_URL", embed.URL)
	t.Setenv("ORCH_FASTEMBED_RS_ROUTE", "/embed")

	var upsertCalls atomic.Int32
	qdrant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/collections/contextlattice_notes":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":{"config":{"params":{"vectors":{"size":2,"distance":"Cosine"}}}}}`))
		case r.Method == http.MethodPut && r.URL.Path == "/collections/contextlattice_notes/points":
			upsertCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":{"status":"completed"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer qdrant.Close()
	t.Setenv("QDRANT_URL", qdrant.URL)

	s := newServer()
	s.memoryStore = store
	type fanoutResult struct {
		status string
		err    error
	}
	item := normalizedWrite{
		project:   "project",
		fileName:  "runtime/state.json",
		content:   "ordinary semantic write",
		topicPath: "runtime",
		dataClass: dataClassLearningMemory,
	}
	fanoutDone := make(chan fanoutResult, 1)
	go func() {
		status, err := s.upsertQdrantFromWrite(context.Background(), item, "event-before-registration")
		fanoutDone <- fanoutResult{status: status, err: err}
	}()
	select {
	case <-enteredEmbed:
	case <-time.After(5 * time.Second):
		t.Fatal("qdrant fanout did not enter embedding")
	}

	exactDone := make(chan error, 1)
	go func() {
		_, _, err := store.put(normalizedWrite{
			project:   "project",
			fileName:  "runtime\\state.json",
			content:   `{"state":"exact"}`,
			dataClass: dataClassRuntimeStateMirror,
		})
		exactDone <- err
	}()
	select {
	case err := <-exactDone:
		if err != nil {
			t.Fatalf("exact state registration during embedding failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("embedding held the canonical path lock and blocked exact registration")
	}
	release()
	result := <-fanoutDone
	if result.err != nil || result.status != "skipped_exact_state_mirror" {
		t.Fatalf("fanout did not recheck exact state after embedding: status=%s err=%v", result.status, result.err)
	}
	if got := upsertCalls.Load(); got != 0 {
		t.Fatalf("qdrant received %d post-registration upserts, want 0", got)
	}
}
