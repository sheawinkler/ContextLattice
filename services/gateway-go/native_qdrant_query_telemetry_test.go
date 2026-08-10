package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeQdrantCollectionDimensionProbeCoalescesConcurrentMisses(t *testing.T) {
	var requests atomic.Int64
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{"config": map[string]any{"params": map[string]any{"vectors": map[string]any{"size": 384}}}},
		})
	}))
	defer server.Close()
	key := nativeQdrantCollectionDimCacheKey(server.URL, "coalesced")
	t.Cleanup(func() {
		nativeQdrantDimCacheMu.Lock()
		delete(nativeQdrantDimCache, key)
		nativeQdrantDimCacheMu.Unlock()
	})

	const callers = 12
	start := make(chan struct{})
	ready := sync.WaitGroup{}
	ready.Add(callers)
	workers := sync.WaitGroup{}
	workers.Add(callers)
	errorsOut := make(chan error, callers)
	for index := 0; index < callers; index++ {
		go func() {
			defer workers.Done()
			ready.Done()
			<-start
			dim, err := nativeQdrantCollectionDim(context.Background(), server.Client(), server.URL, "coalesced")
			if err == nil && dim != 384 {
				err = &nativeQdrantDimensionTestError{dim: dim}
			}
			errorsOut <- err
		}()
	}
	ready.Wait()
	close(start)
	time.Sleep(20 * time.Millisecond)
	close(release)
	workers.Wait()
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatalf("coalesced dimension probe failed: %v", err)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("concurrent dimension cache misses issued %d probes, want 1", got)
	}
}

type nativeQdrantDimensionTestError struct{ dim int }

func (err *nativeQdrantDimensionTestError) Error() string {
	return "unexpected qdrant dimension " + strconv.Itoa(err.dim)
}

func TestNativeQdrantPrimaryEmbeddingQueryStripsOnlyMetadataNoise(t *testing.T) {
	query := "decision profile=holdout nonce=90210 retain signed rollback evidence"
	normalized, changed := nativeQdrantPrimaryEmbeddingQuery(query, 2)
	if !changed || normalized != "decision retain signed rollback evidence" {
		t.Fatalf("primary embedding query was not deterministically normalized: changed=%v query=%q", changed, normalized)
	}
	semantic := "release v4.0.11 rollback evidence"
	if normalized, changed := nativeQdrantPrimaryEmbeddingQuery(semantic, 2); changed || normalized != semantic {
		t.Fatalf("semantic version query was altered: changed=%v query=%q", changed, normalized)
	}
}

func TestNativeQdrantQueryStageTelemetryIsClosedAndTruthful(t *testing.T) {
	nativeQdrantQueryTelemetryResetForTest()
	t.Cleanup(nativeQdrantQueryTelemetryResetForTest)
	observation := newNativeQdrantQueryObservation()
	observation.collectionProbe = 2 * time.Millisecond
	observation.embedding = 3 * time.Millisecond
	observation.search = 4 * time.Millisecond
	observation.reconciliation = time.Millisecond
	observation.queryNormalized = true
	observation.succeeded = true
	observation.terminalStage = "complete"
	observation.finish()

	failure := newNativeQdrantQueryObservation()
	failure.terminalStage = "embedding"
	failure.collectionProbeFallback = true
	failure.finish()

	snapshot := nativeQdrantQueryTelemetrySnapshot()
	if anyToString(snapshot["schema_id"]) != nativeQdrantQueryStagesSchemaID ||
		anyToInt(snapshot["attempts"], 0) != 2 || anyToInt(snapshot["successes"], 0) != 1 ||
		anyToInt(snapshot["failures"], 0) != 1 || anyToInt(snapshot["normalized_queries"], 0) != 1 ||
		anyToInt(snapshot["collection_probe_fallbacks"], 0) != 1 {
		t.Fatalf("qdrant stage telemetry totals are not truthful: %#v", snapshot)
	}
	last := anyMap(snapshot["last"])
	if anyToString(last["terminal_stage"]) != "embedding" || anyToBool(last["succeeded"]) {
		t.Fatalf("qdrant stage telemetry lost the terminal failure stage: %#v", last)
	}
}
