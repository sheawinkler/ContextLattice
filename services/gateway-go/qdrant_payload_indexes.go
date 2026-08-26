package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	errQdrantPayloadIndexesWarming       = errors.New("qdrant payload indexes are not ready")
	errQdrantCollectionMissing           = errors.New("qdrant collection missing")
	errQdrantCollectionDimensionMismatch = errors.New("qdrant collection dimension mismatch")
)

type qdrantPayloadIndexSpec struct {
	field  string
	schema string
}

var requiredQdrantPayloadIndexes = []qdrantPayloadIndexSpec{
	{field: "project", schema: "keyword"},
	{field: "topic_tags", schema: "keyword"},
}

type qdrantPayloadIndexHardener struct {
	mu            sync.RWMutex
	started       bool
	enabled       bool
	ready         bool
	status        string
	attempts      int
	pointsCount   int
	required      []string
	missing       []string
	lastError     string
	lastCheckedAt string
}

func newQdrantPayloadIndexHardener() *qdrantPayloadIndexHardener {
	required := make([]string, 0, len(requiredQdrantPayloadIndexes))
	for _, spec := range requiredQdrantPayloadIndexes {
		required = append(required, spec.field)
	}
	return &qdrantPayloadIndexHardener{
		status:      "not_started",
		pointsCount: -1,
		required:    required,
	}
}

func (h *qdrantPayloadIndexHardener) begin(enabled bool) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.started = true
	h.enabled = enabled
	h.ready = !enabled
	if enabled {
		h.status = "warming"
		h.missing = append([]string(nil), h.required...)
	} else {
		h.status = "disabled"
		h.missing = nil
	}
	h.lastError = ""
	h.mu.Unlock()
}

func (h *qdrantPayloadIndexHardener) observe(pointsCount int, missing []string, err error) int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.attempts++
	h.pointsCount = pointsCount
	h.lastCheckedAt = nowUTCISO()
	h.missing = append([]string(nil), missing...)
	sort.Strings(h.missing)
	if err != nil {
		h.ready = false
		h.status = "degraded"
		h.lastError = err.Error()
	} else if len(h.missing) > 0 {
		h.ready = false
		h.status = "warming"
		h.lastError = ""
	} else {
		h.ready = true
		h.status = "ready"
		h.lastError = ""
	}
	return h.attempts
}

func (h *qdrantPayloadIndexHardener) setStatus(status string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.started && h.enabled && !h.ready {
		h.status = status
	}
}

func (h *qdrantPayloadIndexHardener) queryGate() error {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if !h.started || !h.enabled || h.ready {
		return nil
	}
	return fmt.Errorf("%w: status=%s missing=%s", errQdrantPayloadIndexesWarming, h.status, strings.Join(h.missing, ","))
}

func (h *qdrantPayloadIndexHardener) snapshot() map[string]any {
	if h == nil {
		return map[string]any{"started": false, "enabled": false, "ready": true, "status": "unavailable"}
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return map[string]any{
		"started":         h.started,
		"enabled":         h.enabled,
		"ready":           h.ready,
		"status":          h.status,
		"attempts":        h.attempts,
		"points_count":    h.pointsCount,
		"required_fields": append([]string(nil), h.required...),
		"missing_fields":  append([]string(nil), h.missing...),
		"last_error":      h.lastError,
		"last_checked_at": h.lastCheckedAt,
	}
}

func qdrantPayloadIndexHardeningEnabled() bool {
	return envBool("ORCH_QDRANT_PAYLOAD_INDEX_HARDEN_ENABLED", true) &&
		envBool("ORCH_QDRANT_PAYLOAD_INDEX_HARDEN_ON_STARTUP", true)
}

func (s *server) startQdrantPayloadIndexHardening() {
	if s == nil || s.qdrantPayloadIndexes == nil {
		return
	}
	enabled := qdrantPayloadIndexHardeningEnabled() && strings.TrimSpace(nativeQdrantURL()) != ""
	s.qdrantPayloadIndexes.begin(enabled)
	if !enabled {
		return
	}
	go func() {
		if err := s.waitForQdrantPayloadIndexPrerequisites(context.Background()); err != nil {
			log.Printf("gateway-go qdrant payload index prerequisites stopped: %v", err)
			return
		}
		if err := s.runQdrantPayloadIndexHardening(context.Background()); err != nil {
			log.Printf("gateway-go qdrant payload index hardening stopped: %v", err)
		}
	}()
}

func qdrantPayloadIndexMemoryStoreReady(snapshot map[string]any) bool {
	if !anyToBool(snapshot["configured"]) || anyToBool(snapshot["ready"]) {
		return true
	}
	switch anyToString(snapshot["phase"]) {
	case "blocked", "disabled", "ready":
		return true
	default:
		return false
	}
}

func (s *server) waitForQdrantPayloadIndexPrerequisites(ctx context.Context) error {
	if s == nil || s.qdrantPayloadIndexes == nil ||
		!envBool("ORCH_QDRANT_PAYLOAD_INDEX_HARDEN_WAIT_FOR_MEMORY_STORE", true) {
		return nil
	}
	pollInterval := envDurationSeconds("ORCH_QDRANT_PAYLOAD_INDEX_HARDEN_PREREQUISITE_POLL_SECS", 2)
	for {
		if s.memoryStore == nil || qdrantPayloadIndexMemoryStoreReady(s.memoryStore.migrationSnapshot()) {
			s.qdrantPayloadIndexes.setStatus("warming")
			return nil
		}
		s.qdrantPayloadIndexes.setStatus("waiting_for_memory_store")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func (s *server) runQdrantPayloadIndexHardening(ctx context.Context) error {
	if s == nil || s.client == nil || s.qdrantPayloadIndexes == nil {
		return errors.New("qdrant payload index hardener unavailable")
	}
	baseURL := nativeQdrantURL()
	if baseURL == "" {
		return errors.New("qdrant URL not configured")
	}
	collection := nativeQdrantCollection()
	wait := envBool("ORCH_QDRANT_PAYLOAD_INDEX_HARDEN_WAIT", false)
	retryInterval := envDurationSeconds("ORCH_QDRANT_PAYLOAD_INDEX_HARDEN_RETRY_SECS", 5)
	requestTimeout := envDurationSeconds("ORCH_QDRANT_PAYLOAD_INDEX_HARDEN_REQUEST_TIMEOUT_SECS", 15)
	startupEmbeddingDimension := 0
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		pointsCount, missing, observedDimension, err := ensureQdrantPayloadIndexesWithStartupDimension(
			attemptCtx,
			s.client,
			baseURL,
			collection,
			requiredQdrantPayloadIndexes,
			wait,
			startupEmbeddingDimension,
		)
		cancel()
		if observedDimension > 0 {
			startupEmbeddingDimension = observedDimension
		}
		attempt := s.qdrantPayloadIndexes.observe(pointsCount, missing, err)
		if err == nil && len(missing) == 0 {
			log.Printf("gateway-go qdrant payload indexes ready collection=%s points=%d attempts=%d", collection, pointsCount, attempt)
			return nil
		}
		if errors.Is(err, errQdrantCollectionDimensionMismatch) {
			return err
		}
		if err != nil && (attempt == 1 || attempt%12 == 0) {
			log.Printf("gateway-go qdrant payload index hardening retry collection=%s attempt=%d err=%v", collection, attempt, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryInterval):
		}
	}
}

func ensureQdrantPayloadIndexesWithStartupDimension(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	collection string,
	specs []qdrantPayloadIndexSpec,
	wait bool,
	startupEmbeddingDimension int,
) (int, []string, int, error) {
	pointsCount, missing, err := inspectQdrantPayloadIndexes(ctx, client, baseURL, collection, specs)
	if err != nil && nativeQdrantCollectionMissing(err) {
		vectorDim := startupEmbeddingDimension
		if !qdrantAutoCreateEnabled() {
			return pointsCount, missing, vectorDim, err
		}
		if vectorDim <= 0 {
			var dimensionErr error
			vectorDim, dimensionErr = qdrantStartupEmbeddingDimension(ctx, client)
			if dimensionErr != nil {
				return pointsCount, missing, 0, fmt.Errorf("qdrant collection bootstrap dimension: %w", dimensionErr)
			}
		}
		if createErr := nativeQdrantCreateMissingCollection(ctx, client, baseURL, collection, vectorDim); createErr != nil {
			return pointsCount, missing, vectorDim, fmt.Errorf("qdrant collection bootstrap create: %w", createErr)
		}
		log.Printf(
			"gateway-go qdrant collection auto-created collection=%s vector_dim=%d reason=missing_collection",
			collection,
			vectorDim,
		)
		pointsCount, missing, err = inspectQdrantPayloadIndexes(ctx, client, baseURL, collection, specs)
		startupEmbeddingDimension = vectorDim
	}
	if err != nil || len(missing) == 0 {
		return pointsCount, missing, startupEmbeddingDimension, err
	}
	for _, field := range missing {
		var spec qdrantPayloadIndexSpec
		for _, candidate := range specs {
			if candidate.field == field {
				spec = candidate
				break
			}
		}
		if spec.field == "" {
			continue
		}
		if err := createQdrantPayloadIndex(ctx, client, baseURL, collection, spec, wait); err != nil {
			return pointsCount, missing, startupEmbeddingDimension, err
		}
	}
	pointsCount, missing, err = inspectQdrantPayloadIndexes(ctx, client, baseURL, collection, specs)
	return pointsCount, missing, startupEmbeddingDimension, err
}

func qdrantStartupEmbeddingDimension(ctx context.Context, client *http.Client) (int, error) {
	configured := envInt("ORCH_QDRANT_EMBED_DIM", 0)
	provider := nativeEmbeddingProvider()
	if nativeEmbeddingProviderUsesFastembed(provider) {
		if !nativeSourceAdapterEnabled("fastembed", true) {
			return 0, errors.New("fastembed adapter disabled")
		}
		baseURL := nativeFastembedBaseURL()
		if baseURL == "" {
			return 0, errors.New("fastembed URL not configured")
		}
		vector, err := nativeFastembedRequestVector(
			ctx,
			client,
			"contextlattice qdrant startup dimension probe",
			0,
			baseURL,
			nativeFastembedRoute(),
		)
		if err != nil {
			return 0, fmt.Errorf("fastembed dimension probe failed: %w", err)
		}
		observed := len(vector)
		if observed <= 0 {
			return 0, errors.New("fastembed dimension probe returned an empty vector")
		}
		if configured > 0 && configured != observed {
			return 0, fmt.Errorf("%w: configured=%d observed=%d", errQdrantCollectionDimensionMismatch, configured, observed)
		}
		return observed, nil
	}
	if configured > 0 {
		return configured, nil
	}
	return 0, fmt.Errorf("ORCH_QDRANT_EMBED_DIM is required for embedding provider %q", provider)
}

func inspectQdrantPayloadIndexes(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	collection string,
	specs []qdrantPayloadIndexSpec,
) (int, []string, error) {
	requestURL := strings.TrimRight(baseURL, "/") + "/collections/" + url.PathEscape(collection)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return -1, requiredQdrantPayloadIndexFields(specs), err
	}
	nativeApplyQdrantHeaders(req, false)
	resp, err := client.Do(req)
	if err != nil {
		return -1, requiredQdrantPayloadIndexFields(specs), err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return -1, requiredQdrantPayloadIndexFields(specs), readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusNotFound {
			return -1, requiredQdrantPayloadIndexFields(specs), fmt.Errorf("%w: status=%d", errQdrantCollectionMissing, resp.StatusCode)
		}
		return -1, requiredQdrantPayloadIndexFields(specs), fmt.Errorf("qdrant collection status=%d", resp.StatusCode)
	}
	payload, err := parseJSONMap(body)
	if err != nil {
		return -1, requiredQdrantPayloadIndexFields(specs), err
	}
	result, _ := payload["result"].(map[string]any)
	pointsCount := anyToInt(result["points_count"], 0)
	payloadSchema, _ := result["payload_schema"].(map[string]any)
	missing := make([]string, 0, len(specs))
	mismatched := make([]string, 0)
	for _, spec := range specs {
		rawSchema, ok := payloadSchema[spec.field]
		if !ok {
			missing = append(missing, spec.field)
			continue
		}
		actualSchema := qdrantPayloadIndexSchema(rawSchema)
		if actualSchema != strings.ToLower(strings.TrimSpace(spec.schema)) {
			missing = append(missing, spec.field)
			if actualSchema == "" {
				actualSchema = "unknown"
			}
			mismatched = append(
				mismatched,
				fmt.Sprintf("field=%s got=%s want=%s", spec.field, actualSchema, spec.schema),
			)
		}
	}
	sort.Strings(missing)
	if len(mismatched) > 0 {
		sort.Strings(mismatched)
		return pointsCount, missing, errors.New("qdrant payload index schema mismatch: " + strings.Join(mismatched, "; "))
	}
	return pointsCount, missing, nil
}

func qdrantPayloadIndexSchema(raw any) string {
	switch typed := raw.(type) {
	case string:
		return strings.ToLower(strings.TrimSpace(typed))
	case map[string]any:
		return strings.ToLower(strings.TrimSpace(anyToString(typed["data_type"])))
	default:
		return ""
	}
}

func requiredQdrantPayloadIndexFields(specs []qdrantPayloadIndexSpec) []string {
	fields := make([]string, 0, len(specs))
	for _, spec := range specs {
		fields = append(fields, spec.field)
	}
	sort.Strings(fields)
	return fields
}

func createQdrantPayloadIndex(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	collection string,
	spec qdrantPayloadIndexSpec,
	wait bool,
) error {
	body, err := json.Marshal(map[string]any{
		"field_name":   spec.field,
		"field_schema": spec.schema,
	})
	if err != nil {
		return err
	}
	requestURL := strings.TrimRight(baseURL, "/") + "/collections/" + url.PathEscape(collection) + "/index?wait=" + strconv.FormatBool(wait)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, requestURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	nativeApplyQdrantHeaders(req, true)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(resp.Body)
	if (resp.StatusCode >= 200 && resp.StatusCode < 300) || resp.StatusCode == http.StatusConflict {
		return nil
	}
	message := "qdrant payload index create status=" + strconv.Itoa(resp.StatusCode) + " field=" + spec.field
	if trimmed := strings.TrimSpace(string(responseBody)); trimmed != "" {
		message += " body=" + clipRunes(trimmed, 500)
	}
	return errors.New(message)
}
