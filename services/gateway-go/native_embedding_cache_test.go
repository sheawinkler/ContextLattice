package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

func TestNativeEmbeddingCacheCoalescesVectorSources(t *testing.T) {
	nativeEmbeddingCacheResetForTest()
	t.Cleanup(nativeEmbeddingCacheResetForTest)
	t.Setenv("ORCH_FASTEMBED_RS_MODEL", "BAAI/bge-small-en-v1.5")
	t.Setenv("ORCH_FASTEMBED_RS_TIMEOUT_SECS", "5")
	t.Setenv("GO_RETRIEVAL_EMBED_CACHE_TTL_SECS", "300")
	var requests atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			close(entered)
		}
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"vectors": [][]float64{{1, 2, 3}}})
	}))
	defer server.Close()

	const callers = 8
	start := make(chan struct{})
	results := make(chan []float64, callers)
	errorsOut := make(chan error, callers)
	var workers sync.WaitGroup
	for idx := 0; idx < callers; idx++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			vector, err := nativeFastembedCachedVector(context.Background(), server.Client(), "shared exact query", 3, server.URL, "/embed")
			results <- vector
			errorsOut <- err
		}()
	}
	close(start)
	<-entered
	close(release)
	workers.Wait()
	close(results)
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatalf("coalesced embedding failed: %v", err)
		}
	}
	for vector := range results {
		if len(vector) != 3 || vector[0] != 1 || vector[2] != 3 {
			t.Fatalf("unexpected coalesced vector: %#v", vector)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("identical qdrant/pgvector work was not coalesced: requests=%d", got)
	}
	telemetry := nativeEmbeddingCacheSnapshot()
	if anyToInt(telemetry["attempts"], 0) != 1 || anyToInt(telemetry["entries"], 0) != 1 {
		t.Fatalf("embedding cache telemetry is not truthful: %#v", telemetry)
	}
}

func TestNativeEmbeddingCacheDoesNotCacheTransientFailure(t *testing.T) {
	nativeEmbeddingCacheResetForTest()
	t.Cleanup(nativeEmbeddingCacheResetForTest)
	t.Setenv("ORCH_FASTEMBED_RS_TIMEOUT_SECS", "5")
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	for attempt := 0; attempt < 2; attempt++ {
		if _, err := nativeFastembedCachedVector(context.Background(), server.Client(), "retryable query", 3, server.URL, "/embed"); err == nil {
			t.Fatal("transient sidecar failure was accepted")
		}
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("transient failure was cached: requests=%d", got)
	}
	telemetry := nativeEmbeddingCacheSnapshot()
	if anyToInt(telemetry["failures"], 0) != 2 || anyToInt(telemetry["entries"], 0) != 0 {
		t.Fatalf("failure telemetry/cache state is wrong: %#v", telemetry)
	}
}
