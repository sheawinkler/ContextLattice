package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

type nativeEmbeddingCacheEntry struct {
	vector    []float64
	expiresAt time.Time
	sequence  uint64
}

type nativeEmbeddingTelemetry struct {
	attempts  atomic.Uint64
	successes atomic.Uint64
	failures  atomic.Uint64
	fallbacks atomic.Uint64
	cacheHits atomic.Uint64
	coalesced atomic.Uint64
	mu        sync.Mutex
	lastError string
	lastMs    float64
}

var (
	nativeEmbeddingCacheMu       sync.Mutex
	nativeEmbeddingCache         = map[string]nativeEmbeddingCacheEntry{}
	nativeEmbeddingCacheSequence uint64
	nativeEmbeddingFlight        = &singleflight.Group{}
	nativeEmbeddingStats         nativeEmbeddingTelemetry
)

func nativeFastembedModel() string {
	return strings.TrimSpace(os.Getenv("ORCH_FASTEMBED_RS_MODEL"))
}

func nativeEmbeddingCacheKey(query string, targetDim int, baseURL string, route string) string {
	return strings.Join([]string{
		"fastembed-rs",
		strings.ToLower(strings.TrimSpace(baseURL)),
		strings.TrimSpace(route),
		strings.ToLower(nativeFastembedModel()),
		strconv.Itoa(targetDim),
		sha256Hex(strings.TrimSpace(query)),
	}, "\x00")
}

func nativeEmbeddingCacheTTL() time.Duration {
	return envDurationSeconds("GO_RETRIEVAL_EMBED_CACHE_TTL_SECS", 300)
}

func nativeEmbeddingCacheMaxEntries() int {
	return clampInt(envInt("GO_RETRIEVAL_EMBED_CACHE_MAX_ENTRIES", 2048), 32, 16384)
}

func nativeEmbeddingCacheGet(key string, now time.Time) ([]float64, bool) {
	nativeEmbeddingCacheMu.Lock()
	defer nativeEmbeddingCacheMu.Unlock()
	entry, ok := nativeEmbeddingCache[key]
	if !ok {
		return nil, false
	}
	if !entry.expiresAt.After(now) {
		delete(nativeEmbeddingCache, key)
		return nil, false
	}
	return append([]float64(nil), entry.vector...), true
}

func nativeEmbeddingCachePut(key string, vector []float64, now time.Time) {
	ttl := nativeEmbeddingCacheTTL()
	if ttl <= 0 || len(vector) == 0 {
		return
	}
	nativeEmbeddingCacheMu.Lock()
	defer nativeEmbeddingCacheMu.Unlock()
	nativeEmbeddingCacheSequence++
	nativeEmbeddingCache[key] = nativeEmbeddingCacheEntry{
		vector: append([]float64(nil), vector...), expiresAt: now.Add(ttl), sequence: nativeEmbeddingCacheSequence,
	}
	maxEntries := nativeEmbeddingCacheMaxEntries()
	for len(nativeEmbeddingCache) > maxEntries {
		oldestKey := ""
		oldestSequence := ^uint64(0)
		for candidateKey, entry := range nativeEmbeddingCache {
			if entry.sequence < oldestSequence {
				oldestKey = candidateKey
				oldestSequence = entry.sequence
			}
		}
		if oldestKey == "" {
			break
		}
		delete(nativeEmbeddingCache, oldestKey)
	}
}

func nativeFastembedCachedVector(
	ctx context.Context,
	client *http.Client,
	query string,
	targetDim int,
	baseURL string,
	route string,
) ([]float64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	key := nativeEmbeddingCacheKey(query, targetDim, baseURL, route)
	if vector, ok := nativeEmbeddingCacheGet(key, time.Now()); ok {
		nativeEmbeddingStats.cacheHits.Add(1)
		return vector, nil
	}
	resultCh := nativeEmbeddingFlight.DoChan(key, func() (any, error) {
		if vector, ok := nativeEmbeddingCacheGet(key, time.Now()); ok {
			nativeEmbeddingStats.cacheHits.Add(1)
			return vector, nil
		}
		started := time.Now()
		nativeEmbeddingStats.attempts.Add(1)
		requestCtx := ctx
		cancel := func() {}
		if timeout := envDurationSeconds("ORCH_FASTEMBED_RS_TIMEOUT_SECS", 2.5); timeout > 0 {
			requestCtx, cancel = context.WithTimeout(ctx, timeout)
		}
		defer cancel()
		vector, err := nativeFastembedRequestVector(requestCtx, client, query, targetDim, baseURL, route)
		latencyMs := float64(time.Since(started).Microseconds()) / 1000.0
		if err != nil {
			nativeEmbeddingStats.failures.Add(1)
			nativeEmbeddingRecordLast(latencyMs, err)
			return nil, err
		}
		nativeEmbeddingStats.successes.Add(1)
		nativeEmbeddingRecordLast(latencyMs, nil)
		nativeEmbeddingCachePut(key, vector, time.Now())
		return vector, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-resultCh:
		if result.Shared {
			nativeEmbeddingStats.coalesced.Add(1)
		}
		if result.Err != nil {
			return nil, result.Err
		}
		vector, ok := result.Val.([]float64)
		if !ok || len(vector) == 0 {
			return nil, errors.New("fastembed adapter returned an invalid shared vector")
		}
		return append([]float64(nil), vector...), nil
	}
}

func nativeFastembedRequestVector(
	ctx context.Context,
	client *http.Client,
	query string,
	targetDim int,
	baseURL string,
	route string,
) ([]float64, error) {
	payload := map[string]any{"input": []string{query}}
	if model := nativeFastembedModel(); model != "" {
		payload["model"] = model
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("request serialization failed: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+route, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("request build failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	bodyBytes, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, readErr
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("status=%d", resp.StatusCode)
	}
	responsePayload, err := parseJSONMap(bodyBytes)
	if err != nil {
		return nil, fmt.Errorf("response parse failed: %w", err)
	}
	rawVectors, _ := responsePayload["vectors"].([]any)
	if len(rawVectors) == 0 {
		return nil, errors.New("returned empty vectors")
	}
	firstVector, _ := rawVectors[0].([]any)
	if len(firstVector) == 0 {
		return nil, errors.New("returned empty vector row")
	}
	vector := make([]float64, 0, len(firstVector))
	for _, value := range firstVector {
		vector = append(vector, anyToFloat(value))
	}
	vector = nativeAdjustVectorDim(vector, targetDim)
	if len(vector) == 0 {
		return nil, errors.New("vector coercion failed")
	}
	return vector, nil
}

func nativeEmbeddingRecordFallback() {
	nativeEmbeddingStats.fallbacks.Add(1)
}

func nativeEmbeddingRecordLast(latencyMs float64, err error) {
	nativeEmbeddingStats.mu.Lock()
	defer nativeEmbeddingStats.mu.Unlock()
	nativeEmbeddingStats.lastMs = latencyMs
	if err == nil {
		nativeEmbeddingStats.lastError = ""
		return
	}
	nativeEmbeddingStats.lastError = err.Error()
}

func nativeEmbeddingCacheSnapshot() map[string]any {
	nativeEmbeddingCacheMu.Lock()
	entries := len(nativeEmbeddingCache)
	nativeEmbeddingCacheMu.Unlock()
	nativeEmbeddingStats.mu.Lock()
	lastError := nativeEmbeddingStats.lastError
	lastMs := nativeEmbeddingStats.lastMs
	nativeEmbeddingStats.mu.Unlock()
	var lastErrorValue any
	if lastError != "" {
		lastErrorValue = lastError
	}
	var lastLatencyValue any
	if lastMs > 0 {
		lastLatencyValue = roundFloat(lastMs, 3)
	}
	return map[string]any{
		"attempts": nativeEmbeddingStats.attempts.Load(), "successes": nativeEmbeddingStats.successes.Load(),
		"failures": nativeEmbeddingStats.failures.Load(), "fallbacks": nativeEmbeddingStats.fallbacks.Load(),
		"cacheHits": nativeEmbeddingStats.cacheHits.Load(), "coalesced": nativeEmbeddingStats.coalesced.Load(),
		"entries": entries, "maxEntries": nativeEmbeddingCacheMaxEntries(), "ttlSecs": nativeEmbeddingCacheTTL().Seconds(),
		"lastError": lastErrorValue, "lastLatencyMs": lastLatencyValue,
	}
}

func nativeEmbeddingCacheResetForTest() {
	nativeEmbeddingCacheMu.Lock()
	nativeEmbeddingCache = map[string]nativeEmbeddingCacheEntry{}
	nativeEmbeddingCacheSequence = 0
	nativeEmbeddingCacheMu.Unlock()
	nativeEmbeddingFlight = &singleflight.Group{}
	nativeEmbeddingStats.attempts.Store(0)
	nativeEmbeddingStats.successes.Store(0)
	nativeEmbeddingStats.failures.Store(0)
	nativeEmbeddingStats.fallbacks.Store(0)
	nativeEmbeddingStats.cacheHits.Store(0)
	nativeEmbeddingStats.coalesced.Store(0)
	nativeEmbeddingStats.mu.Lock()
	nativeEmbeddingStats.lastError = ""
	nativeEmbeddingStats.lastMs = 0
	nativeEmbeddingStats.mu.Unlock()
}
