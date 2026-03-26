package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

const telemetryRingSchemaVersion = 1

type telemetryRingEntry struct {
	insertedAt     time.Time
	project        string
	fileName       string
	topicPath      string
	sourcePath     string
	content        string
	itemID         string
	idempotencyKey string
	ingestError    string
	spoolError     string
	lowValue       bool
	eventID        string
}

type telemetryRing struct {
	enabled          bool
	capacity         int
	lowValueMarkers  []string
	highValueMarkers []string
	mu               sync.Mutex
	entries          []telemetryRingEntry
	accepted         uint64
	dropped          uint64
	droppedLowValue  uint64
}

func newTelemetryRingFromEnv() *telemetryRing {
	capacity := envInt("GO_TELEMETRY_RING_CAPACITY", 512)
	if capacity < 1 {
		capacity = 1
	}
	return &telemetryRing{
		enabled:          envBool("GO_TELEMETRY_RING_ENABLED", true),
		capacity:         capacity,
		lowValueMarkers:  csvLowerListEnv("GO_TELEMETRY_RING_LOW_VALUE_MARKERS", "heartbeat,health,status,cpu,memory,latency,queue,throughput,runtime,debug,trace"),
		highValueMarkers: csvLowerListEnv("GO_TELEMETRY_RING_HIGH_VALUE_MARKERS", "error,fail,failure,exception,critical,timeout,reject,alert,incident"),
		entries:          make([]telemetryRingEntry, 0, capacity),
	}
}

func (r *telemetryRing) enqueue(
	item normalizedWrite,
	sourcePath string,
	ingestErr error,
	spoolErr error,
) (map[string]any, error) {
	if r == nil || !r.enabled {
		return nil, fmt.Errorf("telemetry ring disabled")
	}
	if ingestErr == nil {
		ingestErr = fmt.Errorf("telemetry sink unavailable")
	}
	now := time.Now().UTC()
	hashSeed := fmt.Sprintf(
		"%s|%s|%s|%s|%d",
		item.project,
		item.fileName,
		item.topicPath,
		item.content,
		now.UnixNano(),
	)
	sum := sha256.Sum256([]byte(hashSeed))
	eventID := "ring_" + hex.EncodeToString(sum[:8])
	entry := telemetryRingEntry{
		insertedAt:     now,
		project:        item.project,
		fileName:       item.fileName,
		topicPath:      item.topicPath,
		sourcePath:     sourcePath,
		content:        item.content,
		itemID:         item.itemID,
		idempotencyKey: item.idempotencyKey,
		ingestError:    strings.TrimSpace(ingestErr.Error()),
		spoolError:     errorString(spoolErr),
		lowValue:       r.isLowValue(item),
		eventID:        eventID,
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	evicted := telemetryRingEntry{}
	evictedAny := false
	if len(r.entries) >= r.capacity {
		evictIdx := r.indexForEvictionLocked()
		if evictIdx < 0 || evictIdx >= len(r.entries) {
			evictIdx = 0
		}
		evicted = r.entries[evictIdx]
		evictedAny = true
		if evicted.lowValue {
			r.droppedLowValue += 1
		}
		r.dropped += 1
		r.entries = append(r.entries[:evictIdx], r.entries[evictIdx+1:]...)
		log.Printf(
			"telemetry ring eviction: dropped_event_id=%s low_value=%t file=%s topic=%s",
			evicted.eventID,
			evicted.lowValue,
			evicted.fileName,
			evicted.topicPath,
		)
	}

	r.entries = append(r.entries, entry)
	r.accepted += 1
	response := map[string]any{
		"event_id":         eventID,
		"ring_ref":         fmt.Sprintf("inmem_ring:%s", eventID),
		"ring_schema":      telemetryRingSchemaVersion,
		"ring_depth":       len(r.entries),
		"ring_capacity":    r.capacity,
		"ring_dropped":     int64(r.dropped),
		"ring_dropped_low": int64(r.droppedLowValue),
		"ring_low_value":   entry.lowValue,
	}
	if evictedAny {
		response["ring_evicted_event_id"] = evicted.eventID
		response["ring_evicted_low_value"] = evicted.lowValue
	}
	return response, nil
}

func (r *telemetryRing) indexForEvictionLocked() int {
	for idx := range r.entries {
		if r.entries[idx].lowValue {
			return idx
		}
	}
	if len(r.entries) == 0 {
		return -1
	}
	return 0
}

func (r *telemetryRing) isLowValue(item normalizedWrite) bool {
	corpus := strings.ToLower(
		strings.TrimSpace(item.fileName + " " + item.topicPath + " " + item.content),
	)
	for _, marker := range r.highValueMarkers {
		if marker != "" && strings.Contains(corpus, marker) {
			return false
		}
	}
	for _, marker := range r.lowValueMarkers {
		if marker != "" && strings.Contains(corpus, marker) {
			return true
		}
	}
	return false
}

func (r *telemetryRing) snapshot() map[string]any {
	if r == nil {
		return map[string]any{"enabled": false}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]any{
		"enabled":         r.enabled,
		"capacity":        r.capacity,
		"depth":           len(r.entries),
		"accepted":        int64(r.accepted),
		"dropped":         int64(r.dropped),
		"droppedLowValue": int64(r.droppedLowValue),
	}
	if len(r.entries) > 0 {
		out["oldestAt"] = r.entries[0].insertedAt.Format(time.RFC3339Nano)
		out["newestAt"] = r.entries[len(r.entries)-1].insertedAt.Format(time.RFC3339Nano)
	}
	return out
}

func (r *telemetryRing) debugEntries() []telemetryRingEntry {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]telemetryRingEntry, len(r.entries))
	copy(out, r.entries)
	return out
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return strings.TrimSpace(err.Error())
}
