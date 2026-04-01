package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const telemetrySpoolSchemaVersion = 1

type telemetrySpool struct {
	enabled    bool
	path       string
	backupPath string
	maxBytes   int64
	mu         sync.Mutex
}

func newTelemetrySpoolFromEnv() *telemetrySpool {
	spoolPath := resolveStoragePath(
		"GO_TELEMETRY_SPOOL_PATH",
		filepath.Join("services", "orchestrator", "data", "telemetry_spool.ndjson"),
	)
	maxBytes := int64(envInt("GO_TELEMETRY_SPOOL_MAX_BYTES", 64*1024*1024))
	if maxBytes < 1024*1024 {
		maxBytes = 1024 * 1024
	}
	enabled := envBool("GO_TELEMETRY_SPOOL_ENABLED", true)
	if strings.TrimSpace(spoolPath) == "" {
		enabled = false
	}
	return &telemetrySpool{
		enabled:    enabled,
		path:       spoolPath,
		backupPath: spoolPath + ".1",
		maxBytes:   maxBytes,
	}
}

func (s *telemetrySpool) spoolWrite(item normalizedWrite, sourcePath string, ingestErr error) (map[string]any, error) {
	if s == nil || !s.enabled {
		return nil, fmt.Errorf("telemetry spool disabled")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return nil, fmt.Errorf("create telemetry spool dir: %w", err)
	}

	entry := map[string]any{
		"schema_version": telemetrySpoolSchemaVersion,
		"timestamp":      time.Now().UTC().Format(time.RFC3339Nano),
		"project":        item.project,
		"file_name":      item.fileName,
		"topic_path":     item.topicPath,
		"agent_id":       item.agentID,
		"session_id":     item.sessionID,
		"tags":           append([]string{}, item.tags...),
		"created_at":     item.createdAt,
		"source_path":    sourcePath,
		"content":        item.content,
		"item_id":        item.itemID,
		"idempotencyKey": item.idempotencyKey,
		"ingest_error":   ingestErr.Error(),
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return nil, fmt.Errorf("marshal telemetry spool entry: %w", err)
	}
	raw = append(raw, '\n')

	if err := s.rotateIfNeededLocked(int64(len(raw))); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open telemetry spool: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(raw); err != nil {
		return nil, fmt.Errorf("append telemetry spool entry: %w", err)
	}

	hashSeed := fmt.Sprintf("%s|%s|%s|%s", item.project, item.fileName, item.topicPath, item.content)
	sum := sha256.Sum256([]byte(hashSeed))
	eventID := "spool_" + hex.EncodeToString(sum[:8])
	return map[string]any{
		"event_id":    eventID,
		"spool_ref":   filepath.Base(s.path) + ":" + eventID,
		"spool_bytes": len(raw),
	}, nil
}

func (s *telemetrySpool) rotateIfNeededLocked(incomingBytes int64) error {
	if s == nil || !s.enabled || s.maxBytes <= 0 {
		return nil
	}
	info, err := os.Stat(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat telemetry spool: %w", err)
	}
	if info.Size()+incomingBytes <= s.maxBytes {
		return nil
	}
	if err := os.Remove(s.backupPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear telemetry spool backup: %w", err)
	}
	if err := os.Rename(s.path, s.backupPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("rotate telemetry spool: %w", err)
	}
	return nil
}

func (s *telemetrySpool) snapshot() map[string]any {
	if s == nil {
		return map[string]any{
			"enabled": false,
		}
	}
	sizeBytes := int64(0)
	exists := false
	if info, err := os.Stat(s.path); err == nil {
		sizeBytes = info.Size()
		exists = true
	}
	return map[string]any{
		"enabled":    s.enabled,
		"path":       s.path,
		"backupPath": s.backupPath,
		"maxBytes":   s.maxBytes,
		"exists":     exists,
		"sizeBytes":  sizeBytes,
	}
}
