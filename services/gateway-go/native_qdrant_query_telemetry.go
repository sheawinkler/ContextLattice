package main

import (
	"sync"
	"sync/atomic"
	"time"
)

const nativeQdrantQueryStagesSchemaID = "contextlattice_native_qdrant_query_stages.v1"

type nativeQdrantQueryObservation struct {
	startedAt               time.Time
	collectionProbe         time.Duration
	embedding               time.Duration
	search                  time.Duration
	reconciliation          time.Duration
	totalForSnapshot        time.Duration
	terminalStage           string
	collectionProbeFallback bool
	queryNormalized         bool
	succeeded               bool
}

type nativeQdrantQueryTelemetryState struct {
	mu                       sync.Mutex
	attempts                 uint64
	successes                uint64
	failures                 uint64
	collectionProbeFallbacks uint64
	normalizedQueries        uint64
	total                    time.Duration
	collectionProbe          time.Duration
	embedding                time.Duration
	search                   time.Duration
	reconciliation           time.Duration
	last                     nativeQdrantQueryObservation
}

var (
	nativeQdrantQueryStats                 nativeQdrantQueryTelemetryState
	nativeQdrantDimensionProbeCacheHits    atomic.Uint64
	nativeQdrantDimensionProbeCoalescedHit atomic.Uint64
)

func newNativeQdrantQueryObservation() *nativeQdrantQueryObservation {
	return &nativeQdrantQueryObservation{startedAt: time.Now(), terminalStage: "initializing"}
}

func (observation *nativeQdrantQueryObservation) finish() {
	if observation == nil {
		return
	}
	total := time.Since(observation.startedAt)
	if total < 0 {
		total = 0
	}
	nativeQdrantQueryStats.mu.Lock()
	defer nativeQdrantQueryStats.mu.Unlock()
	nativeQdrantQueryStats.attempts++
	if observation.succeeded {
		nativeQdrantQueryStats.successes++
	} else {
		nativeQdrantQueryStats.failures++
	}
	if observation.collectionProbeFallback {
		nativeQdrantQueryStats.collectionProbeFallbacks++
	}
	if observation.queryNormalized {
		nativeQdrantQueryStats.normalizedQueries++
	}
	nativeQdrantQueryStats.total += total
	nativeQdrantQueryStats.collectionProbe += observation.collectionProbe
	nativeQdrantQueryStats.embedding += observation.embedding
	nativeQdrantQueryStats.search += observation.search
	nativeQdrantQueryStats.reconciliation += observation.reconciliation
	observationCopy := *observation
	observationCopy.startedAt = time.Time{}
	observationCopy.totalForSnapshot = total
	nativeQdrantQueryStats.last = observationCopy
}

func nativeQdrantDurationMillis(duration time.Duration) float64 {
	return roundFloat(float64(duration.Microseconds())/1000.0, 3)
}

func nativeQdrantQueryTelemetrySnapshot() map[string]any {
	nativeQdrantQueryStats.mu.Lock()
	defer nativeQdrantQueryStats.mu.Unlock()
	attempts := nativeQdrantQueryStats.attempts
	means := map[string]any{
		"total_ms":                    0.0,
		"collection_probe_ms":         0.0,
		"embedding_ms":                0.0,
		"qdrant_search_ms":            0.0,
		"authority_reconciliation_ms": 0.0,
	}
	if attempts > 0 {
		means["total_ms"] = nativeQdrantDurationMillis(nativeQdrantQueryStats.total / time.Duration(attempts))
		means["collection_probe_ms"] = nativeQdrantDurationMillis(nativeQdrantQueryStats.collectionProbe / time.Duration(attempts))
		means["embedding_ms"] = nativeQdrantDurationMillis(nativeQdrantQueryStats.embedding / time.Duration(attempts))
		means["qdrant_search_ms"] = nativeQdrantDurationMillis(nativeQdrantQueryStats.search / time.Duration(attempts))
		means["authority_reconciliation_ms"] = nativeQdrantDurationMillis(nativeQdrantQueryStats.reconciliation / time.Duration(attempts))
	}
	last := nativeQdrantQueryStats.last
	lastSnapshot := map[string]any{
		"terminal_stage":              last.terminalStage,
		"succeeded":                   last.succeeded,
		"query_normalized":            last.queryNormalized,
		"collection_probe_fallback":   last.collectionProbeFallback,
		"total_ms":                    nativeQdrantDurationMillis(last.totalForSnapshot),
		"collection_probe_ms":         nativeQdrantDurationMillis(last.collectionProbe),
		"embedding_ms":                nativeQdrantDurationMillis(last.embedding),
		"qdrant_search_ms":            nativeQdrantDurationMillis(last.search),
		"authority_reconciliation_ms": nativeQdrantDurationMillis(last.reconciliation),
	}
	return map[string]any{
		"schema_id":                  nativeQdrantQueryStagesSchemaID,
		"attempts":                   attempts,
		"successes":                  nativeQdrantQueryStats.successes,
		"failures":                   nativeQdrantQueryStats.failures,
		"collection_probe_fallbacks": nativeQdrantQueryStats.collectionProbeFallbacks,
		"normalized_queries":         nativeQdrantQueryStats.normalizedQueries,
		"dimension_probe_cache_hits": nativeQdrantDimensionProbeCacheHits.Load(),
		"dimension_probe_coalesced":  nativeQdrantDimensionProbeCoalescedHit.Load(),
		"mean":                       means,
		"last":                       lastSnapshot,
	}
}

func nativeQdrantDimensionProbeCacheHit() {
	nativeQdrantDimensionProbeCacheHits.Add(1)
}

func nativeQdrantDimensionProbeCoalesced() {
	nativeQdrantDimensionProbeCoalescedHit.Add(1)
}

func nativeQdrantQueryTelemetryResetForTest() {
	nativeQdrantQueryStats.mu.Lock()
	nativeQdrantQueryStats.attempts = 0
	nativeQdrantQueryStats.successes = 0
	nativeQdrantQueryStats.failures = 0
	nativeQdrantQueryStats.collectionProbeFallbacks = 0
	nativeQdrantQueryStats.normalizedQueries = 0
	nativeQdrantQueryStats.total = 0
	nativeQdrantQueryStats.collectionProbe = 0
	nativeQdrantQueryStats.embedding = 0
	nativeQdrantQueryStats.search = 0
	nativeQdrantQueryStats.reconciliation = 0
	nativeQdrantQueryStats.last = nativeQdrantQueryObservation{}
	nativeQdrantQueryStats.mu.Unlock()
	nativeQdrantDimensionProbeCacheHits.Store(0)
	nativeQdrantDimensionProbeCoalescedHit.Store(0)
}
