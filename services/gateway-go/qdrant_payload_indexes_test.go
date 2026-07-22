package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestQdrantPayloadIndexHardenerCreatesRequiredIndexes(t *testing.T) {
	t.Setenv("QDRANT_LOCAL_URL", "")
	t.Setenv("ORCH_QDRANT_COLLECTION", "contextlattice_notes")
	t.Setenv("ORCH_QDRANT_PAYLOAD_INDEX_HARDEN_WAIT", "true")
	t.Setenv("ORCH_QDRANT_PAYLOAD_INDEX_HARDEN_RETRY_SECS", "0.01")
	t.Setenv("ORCH_QDRANT_PAYLOAD_INDEX_HARDEN_REQUEST_TIMEOUT_SECS", "1")

	var mu sync.Mutex
	payloadSchema := map[string]any{}
	created := []string{}
	qdrant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/collections/contextlattice_notes":
			mu.Lock()
			schemaCopy := map[string]any{}
			for key, value := range payloadSchema {
				schemaCopy[key] = value
			}
			mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{
				"result": map[string]any{
					"points_count":   700000,
					"payload_schema": schemaCopy,
				},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/collections/contextlattice_notes/index":
			if r.URL.Query().Get("wait") != "true" {
				t.Fatalf("expected wait=true, got %q", r.URL.RawQuery)
			}
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode index payload: %v", err)
			}
			field := anyToString(payload["field_name"])
			if anyToString(payload["field_schema"]) != "keyword" {
				t.Fatalf("expected keyword schema, got %#v", payload)
			}
			mu.Lock()
			created = append(created, field)
			payloadSchema[field] = map[string]any{"data_type": "keyword"}
			mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"status": "completed"}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer qdrant.Close()
	t.Setenv("QDRANT_URL", qdrant.URL)

	s := &server{
		client:               &http.Client{Timeout: time.Second},
		qdrantPayloadIndexes: newQdrantPayloadIndexHardener(),
	}
	s.qdrantPayloadIndexes.begin(true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.runQdrantPayloadIndexHardening(ctx); err != nil {
		t.Fatalf("hardening failed: %v", err)
	}

	mu.Lock()
	sort.Strings(created)
	gotCreated := append([]string(nil), created...)
	mu.Unlock()
	if len(gotCreated) != 2 || gotCreated[0] != "project" || gotCreated[1] != "topic_tags" {
		t.Fatalf("created indexes=%v, want [project topic_tags]", gotCreated)
	}
	snapshot := s.qdrantPayloadIndexes.snapshot()
	if snapshot["ready"] != true || snapshot["status"] != "ready" {
		t.Fatalf("unexpected hardener snapshot: %#v", snapshot)
	}
	if anyToInt(snapshot["points_count"], 0) != 700000 {
		t.Fatalf("unexpected points count: %#v", snapshot)
	}
}

func TestQdrantQueryFailsFastWhilePayloadIndexesWarm(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_NATIVE_QDRANT_ENABLED", "true")
	t.Setenv("QDRANT_LOCAL_URL", "")
	calls := 0
	qdrant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer qdrant.Close()
	t.Setenv("QDRANT_URL", qdrant.URL)

	s := &server{
		client:               &http.Client{Timeout: time.Second},
		qdrantPayloadIndexes: newQdrantPayloadIndexHardener(),
	}
	s.qdrantPayloadIndexes.begin(true)
	started := time.Now()
	_, warnings, err := s.queryQdrantSource(context.Background(), map[string]any{
		"query":   "large store availability",
		"project": "contextlattice",
		"limit":   2,
	})
	if !errors.Is(err, errQdrantPayloadIndexesWarming) {
		t.Fatalf("expected warming error, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("warming gate was not fail-fast: %s", elapsed)
	}
	if calls != 0 {
		t.Fatalf("warming query touched qdrant %d time(s)", calls)
	}
	if len(warnings) != 1 {
		t.Fatalf("expected one availability warning, got %v", warnings)
	}
}

func TestQdrantWarmingNeverFallsBackToPythonBackend(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_NATIVE_QDRANT_ENABLED", "true")
	t.Setenv("QDRANT_LOCAL_URL", "")

	backendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
		writeJSON(w, http.StatusOK, map[string]any{"results": []any{}})
	}))
	defer backend.Close()
	t.Setenv("QDRANT_URL", backend.URL)

	s := &server{
		backendURL:            backend.URL,
		client:                &http.Client{Timeout: time.Second},
		strictNoPythonRuntime: false,
		qdrantPayloadIndexes:  newQdrantPayloadIndexHardener(),
	}
	s.qdrantPayloadIndexes.begin(true)
	rows, warnings, _, owner, err := s.callBackendSourceQuery(
		context.Background(),
		http.Header{},
		map[string]any{"query": "large store availability", "limit": 2},
		sourceQdrant,
		false,
	)
	if !errors.Is(err, errQdrantPayloadIndexesWarming) {
		t.Fatalf("expected warming error, got %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected no qdrant rows while warming, got %#v", rows)
	}
	if owner != sourceOwnerGoNative {
		t.Fatalf("expected go-native ownership, got %q", owner)
	}
	if backendCalls != 0 {
		t.Fatalf("warming qdrant source fell through to backend %d time(s)", backendCalls)
	}
	if len(warnings) != 1 {
		t.Fatalf("expected one availability warning, got %v", warnings)
	}
}

func TestQdrantPayloadIndexHardenerDisabledDoesNotGateQueries(t *testing.T) {
	hardener := newQdrantPayloadIndexHardener()
	hardener.begin(false)
	if err := hardener.queryGate(); err != nil {
		t.Fatalf("disabled hardener gated query: %v", err)
	}
	snapshot := hardener.snapshot()
	if snapshot["ready"] != true || snapshot["status"] != "disabled" {
		t.Fatalf("unexpected disabled snapshot: %#v", snapshot)
	}
}

func TestQdrantPayloadIndexHardeningWaitsForMemoryStoreMigration(t *testing.T) {
	t.Setenv("ORCH_QDRANT_PAYLOAD_INDEX_HARDEN_WAIT_FOR_MEMORY_STORE", "true")
	t.Setenv("ORCH_QDRANT_PAYLOAD_INDEX_HARDEN_PREREQUISITE_POLL_SECS", "0.01")
	store := &memoryStore{
		policy:    memoryStorePolicy{enabled: true},
		migration: newOwnerOnlyMigrationRuntime("test", true),
	}
	hardener := newQdrantPayloadIndexHardener()
	hardener.begin(true)
	s := &server{memoryStore: store, qdrantPayloadIndexes: hardener}

	blockedCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := s.waitForQdrantPayloadIndexPrerequisites(blockedCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected prerequisite wait deadline, got %v", err)
	}
	if status := anyToString(hardener.snapshot()["status"]); status != "waiting_for_memory_store" {
		t.Fatalf("hardener status=%q, want waiting_for_memory_store", status)
	}

	store.ready.Store(true)
	store.migration.markReady(ownerOnlyMigrationReport{})
	readyCtx, readyCancel := context.WithTimeout(context.Background(), time.Second)
	defer readyCancel()
	if err := s.waitForQdrantPayloadIndexPrerequisites(readyCtx); err != nil {
		t.Fatalf("ready prerequisite failed: %v", err)
	}
	if status := anyToString(hardener.snapshot()["status"]); status != "warming" {
		t.Fatalf("hardener status=%q, want warming", status)
	}
}

func TestQdrantPayloadIndexHardenerRejectsWrongSchema(t *testing.T) {
	qdrant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"result": map[string]any{
				"points_count": 700000,
				"payload_schema": map[string]any{
					"project":    map[string]any{"data_type": "text"},
					"topic_tags": map[string]any{"data_type": "keyword"},
				},
			},
		})
	}))
	defer qdrant.Close()

	pointsCount, missing, err := inspectQdrantPayloadIndexes(
		context.Background(),
		qdrant.Client(),
		qdrant.URL,
		"contextlattice_notes",
		requiredQdrantPayloadIndexes,
	)
	if err == nil || !strings.Contains(err.Error(), "field=project got=text want=keyword") {
		t.Fatalf("expected project schema mismatch, got %v", err)
	}
	if pointsCount != 700000 {
		t.Fatalf("points count=%d, want 700000", pointsCount)
	}
	if len(missing) != 1 || missing[0] != "project" {
		t.Fatalf("missing fields=%v, want [project]", missing)
	}
}
