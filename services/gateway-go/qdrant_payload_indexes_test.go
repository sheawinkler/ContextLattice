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
	"sync/atomic"
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

func TestEnsureQdrantPayloadIndexesBootstrapsMissingCollectionFromObservedEmbeddingDimension(t *testing.T) {
	t.Setenv("ORCH_QDRANT_AUTO_CREATE_ON_STARTUP", "true")
	t.Setenv("ORCH_EMBED_PROVIDER", "fastembed-rs")
	t.Setenv("ORCH_QDRANT_EMBED_DIM", "384")
	t.Setenv("ORCH_FASTEMBED_RS_ROUTE", "/embed")
	t.Setenv("ORCH_FASTEMBED_RS_MODEL", "BAAI/bge-small-en-v1.5")

	fastembed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/embed" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"model":   "BAAI/bge-small-en-v1.5",
			"vectors": [][]float64{make([]float64, 384)},
		})
	}))
	defer fastembed.Close()
	t.Setenv("ORCH_FASTEMBED_RS_BASE_URL", fastembed.URL)

	collectionExists := false
	payloadSchema := map[string]any{}
	createdDimension := 0
	qdrant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/collections/contextlattice_notes":
			if !collectionExists {
				writeJSON(w, http.StatusNotFound, map[string]any{"status": "not found"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"result": map[string]any{
					"points_count":   0,
					"payload_schema": payloadSchema,
					"config": map[string]any{
						"params": map[string]any{"vectors": map[string]any{"size": createdDimension, "distance": "Cosine"}},
					},
				},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/collections/contextlattice_notes":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode collection create payload: %v", err)
			}
			createdDimension = anyToInt(anyMap(payload["vectors"])["size"], 0)
			collectionExists = true
			writeJSON(w, http.StatusOK, map[string]any{"result": true})
		case r.Method == http.MethodPut && r.URL.Path == "/collections/contextlattice_notes/index":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode index create payload: %v", err)
			}
			payloadSchema[anyToString(payload["field_name"])] = map[string]any{"data_type": anyToString(payload["field_schema"])}
			writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"status": "completed"}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer qdrant.Close()

	pointsCount, missing, _, err := ensureQdrantPayloadIndexesWithStartupDimension(
		context.Background(),
		qdrant.Client(),
		qdrant.URL,
		"contextlattice_notes",
		requiredQdrantPayloadIndexes,
		true,
		0,
	)
	if err != nil {
		t.Fatalf("bootstrap missing collection: %v", err)
	}
	if !collectionExists || createdDimension != 384 {
		t.Fatalf("collection bootstrap created=%v dimension=%d, want created=true dimension=384", collectionExists, createdDimension)
	}
	if pointsCount != 0 || len(missing) != 0 {
		t.Fatalf("bootstrap result points=%d missing=%v, want points=0 missing=[]", pointsCount, missing)
	}
}

func TestEnsureQdrantPayloadIndexesRejectsConfiguredEmbeddingDimensionMismatch(t *testing.T) {
	t.Setenv("ORCH_QDRANT_AUTO_CREATE_ON_STARTUP", "true")
	t.Setenv("ORCH_EMBED_PROVIDER", "fastembed-rs")
	t.Setenv("ORCH_QDRANT_EMBED_DIM", "768")
	t.Setenv("ORCH_FASTEMBED_RS_ROUTE", "/embed")

	fastembed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"model":   "BAAI/bge-small-en-v1.5",
			"vectors": [][]float64{make([]float64, 384)},
		})
	}))
	defer fastembed.Close()
	t.Setenv("ORCH_FASTEMBED_RS_BASE_URL", fastembed.URL)

	createCalls := 0
	qdrant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			createCalls++
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"status": "not found"})
	}))
	defer qdrant.Close()

	_, _, _, err := ensureQdrantPayloadIndexesWithStartupDimension(
		context.Background(),
		qdrant.Client(),
		qdrant.URL,
		"contextlattice_notes",
		requiredQdrantPayloadIndexes,
		true,
		0,
	)
	if err == nil || !strings.Contains(err.Error(), "configured=768 observed=384") {
		t.Fatalf("expected configured/observed dimension mismatch, got %v", err)
	}
	if !errors.Is(err, errQdrantCollectionDimensionMismatch) {
		t.Fatalf("dimension mismatch must remain classifiable, got %v", err)
	}
	if createCalls != 0 {
		t.Fatalf("dimension mismatch performed %d collection mutation(s), want 0", createCalls)
	}
}

func TestEnsureQdrantPayloadIndexesRejectsConcurrentCreationDimensionMismatch(t *testing.T) {
	t.Setenv("ORCH_QDRANT_AUTO_CREATE_ON_STARTUP", "true")
	t.Setenv("ORCH_EMBED_PROVIDER", "fastembed-rs")
	t.Setenv("ORCH_QDRANT_EMBED_DIM", "384")
	t.Setenv("ORCH_FASTEMBED_RS_ROUTE", "/embed")

	fastembed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"model":   "BAAI/bge-small-en-v1.5",
			"vectors": [][]float64{make([]float64, 384)},
		})
	}))
	defer fastembed.Close()
	t.Setenv("ORCH_FASTEMBED_RS_BASE_URL", fastembed.URL)

	concurrentCollectionCreated := false
	qdrant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/collections/contextlattice_notes":
			if !concurrentCollectionCreated {
				writeJSON(w, http.StatusNotFound, map[string]any{"status": "not found"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"result": map[string]any{
					"points_count": 0,
					"payload_schema": map[string]any{
						"project":    map[string]any{"data_type": "keyword"},
						"topic_tags": map[string]any{"data_type": "keyword"},
					},
					"config": map[string]any{
						"params": map[string]any{"vectors": map[string]any{"size": 768, "distance": "Cosine"}},
					},
				},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/collections/contextlattice_notes":
			concurrentCollectionCreated = true
			writeJSON(w, http.StatusConflict, map[string]any{"status": "already exists"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer qdrant.Close()

	_, _, _, err := ensureQdrantPayloadIndexesWithStartupDimension(
		context.Background(),
		qdrant.Client(),
		qdrant.URL,
		"contextlattice_notes",
		requiredQdrantPayloadIndexes,
		true,
		0,
	)
	if err == nil || !strings.Contains(err.Error(), "existing=768 required=384") {
		t.Fatalf("expected concurrent create dimension mismatch, got %v", err)
	}
	if !errors.Is(err, errQdrantCollectionDimensionMismatch) {
		t.Fatalf("concurrent dimension mismatch must remain classifiable, got %v", err)
	}
}

func TestNativeQdrantCreateCollectionConflictUsesFreshDimension(t *testing.T) {
	t.Setenv("ORCH_QDRANT_AUTO_CREATE_ON_STARTUP", "true")
	for _, tc := range []struct {
		name        string
		cachedDim   int
		remoteDim   int
		wantErr     bool
		wantMessage string
	}{
		{name: "matching winner", cachedDim: 768, remoteDim: 384},
		{name: "mismatched winner", cachedDim: 384, remoteDim: 768, wantErr: true, wantMessage: "existing=768 required=384"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getCalls := 0
			putCalls := 0
			qdrant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodPut:
					putCalls++
					writeJSON(w, http.StatusConflict, map[string]any{"status": "already exists"})
				case http.MethodGet:
					getCalls++
					writeJSON(w, http.StatusOK, map[string]any{
						"result": map[string]any{
							"config": map[string]any{
								"params": map[string]any{"vectors": map[string]any{"size": tc.remoteDim, "distance": "Cosine"}},
							},
						},
					})
				default:
					w.WriteHeader(http.StatusMethodNotAllowed)
				}
			}))
			defer qdrant.Close()
			key := nativeQdrantCollectionDimCacheKey(qdrant.URL, "contextlattice_notes")
			nativeQdrantSetCachedDim(qdrant.URL, "contextlattice_notes", tc.cachedDim)
			t.Cleanup(func() {
				nativeQdrantDimCacheMu.Lock()
				delete(nativeQdrantDimCache, key)
				nativeQdrantDimCacheMu.Unlock()
				nativeQdrantCreateFlight.Forget(key)
			})

			err := nativeQdrantCreateCollection(context.Background(), qdrant.Client(), qdrant.URL, "contextlattice_notes", 384)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), tc.wantMessage) || !errors.Is(err, errQdrantCollectionDimensionMismatch) {
					t.Fatalf("conflict error=%v, want classified %q", err, tc.wantMessage)
				}
			} else if err != nil {
				t.Fatalf("matching concurrent creator rejected: %v", err)
			}
			if putCalls != 1 || getCalls != 1 {
				t.Fatalf("conflict calls put=%d get=%d, want one mutation attempt and one fresh probe", putCalls, getCalls)
			}
			if got := nativeQdrantCachedDim(qdrant.URL, "contextlattice_notes"); got != tc.remoteDim {
				t.Fatalf("cached dimension=%d, want fresh remote dimension %d", got, tc.remoteDim)
			}
		})
	}
}

func TestEnsureQdrantPayloadIndexesAutoCreateDisabledPerformsNoMutation(t *testing.T) {
	t.Setenv("ORCH_QDRANT_AUTO_CREATE_ON_STARTUP", "false")
	var putCalls atomic.Int32
	qdrant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			putCalls.Add(1)
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"status": "not found"})
	}))
	defer qdrant.Close()

	_, _, _, err := ensureQdrantPayloadIndexesWithStartupDimension(
		context.Background(),
		qdrant.Client(),
		qdrant.URL,
		"contextlattice_notes",
		requiredQdrantPayloadIndexes,
		true,
		384,
	)
	if !errors.Is(err, errQdrantCollectionMissing) {
		t.Fatalf("disabled auto-create error=%v, want typed missing-collection error", err)
	}
	if got := putCalls.Load(); got != 0 {
		t.Fatalf("disabled auto-create performed %d mutation(s), want 0", got)
	}
}

func TestEnsureQdrantPayloadIndexesPreservesExistingCollection(t *testing.T) {
	t.Setenv("ORCH_QDRANT_AUTO_CREATE_ON_STARTUP", "true")
	var putCalls atomic.Int32
	qdrant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			putCalls.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"result": map[string]any{
				"points_count": 834784,
				"payload_schema": map[string]any{
					"project":    map[string]any{"data_type": "keyword"},
					"topic_tags": map[string]any{"data_type": "keyword"},
				},
				"config": map[string]any{
					"params": map[string]any{"vectors": map[string]any{"size": 768, "distance": "Cosine"}},
				},
			},
		})
	}))
	defer qdrant.Close()

	pointsCount, missing, _, err := ensureQdrantPayloadIndexesWithStartupDimension(
		context.Background(),
		qdrant.Client(),
		qdrant.URL,
		"contextlattice_notes",
		requiredQdrantPayloadIndexes,
		true,
		384,
	)
	if err != nil || pointsCount != 834784 || len(missing) != 0 {
		t.Fatalf("existing collection result points=%d missing=%v err=%v", pointsCount, missing, err)
	}
	if got := putCalls.Load(); got != 0 {
		t.Fatalf("existing collection performed %d mutation(s), want 0", got)
	}
}

func TestQdrantWriteDimensionHonorsAutoCreatePolicyAndObservedDimension(t *testing.T) {
	for _, tc := range []struct {
		name       string
		autoCreate string
		wantDim    int
		wantErr    error
		wantPuts   int32
		wantEmbeds int32
	}{
		{name: "disabled", autoCreate: "false", wantErr: errQdrantCollectionMissing},
		{name: "enabled", autoCreate: "true", wantDim: 384, wantPuts: 1, wantEmbeds: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ORCH_QDRANT_AUTO_CREATE_ON_STARTUP", tc.autoCreate)
			t.Setenv("ORCH_EMBED_PROVIDER", "fastembed-rs")
			t.Setenv("ORCH_QDRANT_EMBED_DIM", "384")
			t.Setenv("ORCH_FASTEMBED_RS_ROUTE", "/embed")
			var embedCalls atomic.Int32
			fastembed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				embedCalls.Add(1)
				writeJSON(w, http.StatusOK, map[string]any{"vectors": [][]float64{make([]float64, 384)}})
			}))
			defer fastembed.Close()
			t.Setenv("ORCH_FASTEMBED_RS_BASE_URL", fastembed.URL)

			var exists atomic.Bool
			var putCalls atomic.Int32
			qdrant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					if !exists.Load() {
						writeJSON(w, http.StatusNotFound, map[string]any{"status": "not found"})
						return
					}
					writeJSON(w, http.StatusOK, map[string]any{
						"result": map[string]any{"config": map[string]any{"params": map[string]any{"vectors": map[string]any{"size": 384}}}},
					})
				case http.MethodPut:
					putCalls.Add(1)
					var payload map[string]any
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Fatalf("decode collection create: %v", err)
					}
					if got := anyToInt(anyMap(payload["vectors"])["size"], 0); got != 384 {
						t.Fatalf("write path created dimension=%d, want observed 384", got)
					}
					exists.Store(true)
					writeJSON(w, http.StatusOK, map[string]any{"result": true})
				default:
					w.WriteHeader(http.StatusMethodNotAllowed)
				}
			}))
			defer qdrant.Close()

			dim, err := nativeQdrantWriteEmbeddingDimension(context.Background(), qdrant.Client(), qdrant.URL, "contextlattice_notes")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("write dimension error=%v, want %v", err, tc.wantErr)
				}
			} else if err != nil || dim != tc.wantDim {
				t.Fatalf("write dimension=%d err=%v, want dimension=%d", dim, err, tc.wantDim)
			}
			if got := putCalls.Load(); got != tc.wantPuts {
				t.Fatalf("write collection mutations=%d, want %d", got, tc.wantPuts)
			}
			if got := embedCalls.Load(); got != tc.wantEmbeds {
				t.Fatalf("write dimension probes=%d, want %d", got, tc.wantEmbeds)
			}
		})
	}
}

func TestQdrantMissingCollectionCreationIsSingleOwner(t *testing.T) {
	t.Setenv("ORCH_QDRANT_AUTO_CREATE_ON_STARTUP", "true")
	var putCalls atomic.Int32
	createEntered := make(chan struct{})
	releaseCreate := make(chan struct{})
	qdrant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if putCalls.Add(1) == 1 {
			close(createEntered)
		}
		<-releaseCreate
		writeJSON(w, http.StatusOK, map[string]any{"result": true})
	}))
	defer qdrant.Close()
	key := nativeQdrantCollectionDimCacheKey(qdrant.URL, "contextlattice_notes")
	t.Cleanup(func() { nativeQdrantCreateFlight.Forget(key) })

	results := make(chan error, 2)
	go func() {
		results <- nativeQdrantCreateMissingCollection(context.Background(), qdrant.Client(), qdrant.URL, "contextlattice_notes", 384)
	}()
	select {
	case <-createEntered:
	case <-time.After(time.Second):
		t.Fatal("first collection creator did not start")
	}
	secondCalling := make(chan struct{})
	go func() {
		close(secondCalling)
		results <- nativeQdrantCreateMissingCollection(context.Background(), qdrant.Client(), qdrant.URL, "contextlattice_notes", 384)
	}()
	<-secondCalling
	time.Sleep(50 * time.Millisecond)
	mutationsBeforeRelease := putCalls.Load()
	close(releaseCreate)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("serialized collection create failed: %v", err)
		}
	}
	if mutationsBeforeRelease != 1 || putCalls.Load() != 1 {
		t.Fatalf("collection create mutations before=%d after=%d, want one shared owner", mutationsBeforeRelease, putCalls.Load())
	}
}

func TestQdrantPayloadIndexHardenerReusesStartupEmbeddingDimensionAcrossCreateRetry(t *testing.T) {
	t.Setenv("QDRANT_LOCAL_URL", "")
	t.Setenv("ORCH_QDRANT_COLLECTION", "contextlattice_notes")
	t.Setenv("ORCH_QDRANT_AUTO_CREATE_ON_STARTUP", "true")
	t.Setenv("ORCH_EMBED_PROVIDER", "fastembed-rs")
	t.Setenv("ORCH_QDRANT_EMBED_DIM", "384")
	t.Setenv("ORCH_FASTEMBED_RS_ROUTE", "/embed")
	t.Setenv("ORCH_QDRANT_PAYLOAD_INDEX_HARDEN_RETRY_SECS", "0.001")
	t.Setenv("ORCH_QDRANT_PAYLOAD_INDEX_HARDEN_REQUEST_TIMEOUT_SECS", "1")

	embeddingCalls := 0
	fastembed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		embeddingCalls++
		writeJSON(w, http.StatusOK, map[string]any{
			"model":   "BAAI/bge-small-en-v1.5",
			"vectors": [][]float64{make([]float64, 384)},
		})
	}))
	defer fastembed.Close()
	t.Setenv("ORCH_FASTEMBED_RS_BASE_URL", fastembed.URL)

	collectionExists := false
	createCalls := 0
	qdrant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/collections/contextlattice_notes":
			if !collectionExists {
				writeJSON(w, http.StatusNotFound, map[string]any{"status": "not found"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"result": map[string]any{
					"points_count": 0,
					"payload_schema": map[string]any{
						"project":    map[string]any{"data_type": "keyword"},
						"topic_tags": map[string]any{"data_type": "keyword"},
					},
				},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/collections/contextlattice_notes":
			createCalls++
			if createCalls == 1 {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"status": "retry"})
				return
			}
			collectionExists = true
			writeJSON(w, http.StatusOK, map[string]any{"result": true})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer qdrant.Close()
	t.Setenv("QDRANT_URL", qdrant.URL)

	s := &server{
		client:               qdrant.Client(),
		qdrantPayloadIndexes: newQdrantPayloadIndexHardener(),
	}
	s.qdrantPayloadIndexes.begin(true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.runQdrantPayloadIndexHardening(ctx); err != nil {
		t.Fatalf("hardening retry failed: %v", err)
	}
	if createCalls != 2 {
		t.Fatalf("collection create calls=%d, want 2", createCalls)
	}
	if embeddingCalls != 1 {
		t.Fatalf("embedding dimension probes=%d, want 1 across create retry", embeddingCalls)
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
	rows, warnings, _, owner, _, err := s.callBackendSourceQuery(
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

func TestReadyzTracksQdrantPayloadIndexHardening(t *testing.T) {
	hardener := newQdrantPayloadIndexHardener()
	hardener.begin(true)
	s := &server{
		strictNoPythonRuntime: true,
		qdrantPayloadIndexes:  hardener,
	}

	assertStatus := func(path string, wantStatus int, wantReady bool, wantReason string) {
		t.Helper()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		if path == "/healthz" {
			s.healthz(recorder, request)
		} else {
			s.readyz(recorder, request)
		}
		if recorder.Code != wantStatus {
			t.Fatalf("%s status=%d, want %d: %s", path, recorder.Code, wantStatus, recorder.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode %s payload: %v", path, err)
		}
		if got := anyToBool(payload["ready"]); got != wantReady {
			t.Fatalf("%s ready=%v, want %v: %#v", path, got, wantReady, payload)
		}
		if wantReason != "" {
			readiness := anyMap(payload["readiness"])
			reasons := anyToStringSlice(readiness["reasons"])
			if !stringSliceContains(reasons, wantReason) {
				t.Fatalf("%s readiness reasons=%v, want %q", path, reasons, wantReason)
			}
		}
	}

	assertStatus("/healthz", http.StatusOK, false, "qdrant_payload_indexes_not_ready")
	assertStatus("/readyz", http.StatusServiceUnavailable, false, "qdrant_payload_indexes_not_ready")
	hardener.observe(-1, []string{"project", "topic_tags"}, errors.New("qdrant unavailable"))
	assertStatus("/readyz", http.StatusServiceUnavailable, false, "qdrant_payload_indexes_not_ready")
	hardener.observe(0, nil, nil)
	assertStatus("/readyz", http.StatusOK, true, "")
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
