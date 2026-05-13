package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type storageGovernancePolicy struct {
	enabled              bool
	diskRoot             string
	warnUsedRatio        float64
	highUsedRatio        float64
	minFreeBytes         uint64
	retentionWarnFactor  float64
	retentionHighFactor  float64
	taskDBCompactEnabled bool
}

func loadStorageGovernancePolicy() storageGovernancePolicy {
	root := strings.TrimSpace(os.Getenv("ORCH_STORAGE_GOVERNANCE_DISK_ROOT"))
	if root == "" {
		root = "."
	}
	minFreeGB := envFloat("ORCH_STORAGE_GOVERNANCE_MIN_FREE_GB", 40)
	if minFreeGB < 0 {
		minFreeGB = 0
	}
	policy := storageGovernancePolicy{
		enabled:              envBool("ORCH_STORAGE_GOVERNANCE_ENABLED", true),
		diskRoot:             root,
		warnUsedRatio:        envFloat("ORCH_STORAGE_GOVERNANCE_WARN_USED_RATIO", 0.85),
		highUsedRatio:        envFloat("ORCH_STORAGE_GOVERNANCE_HIGH_USED_RATIO", 0.92),
		minFreeBytes:         uint64(minFreeGB * 1024 * 1024 * 1024),
		retentionWarnFactor:  envFloat("ORCH_STORAGE_GOVERNANCE_RETENTION_MULTIPLIER_WARN", 1.5),
		retentionHighFactor:  envFloat("ORCH_STORAGE_GOVERNANCE_RETENTION_MULTIPLIER_HIGH", 2.5),
		taskDBCompactEnabled: envBool("ORCH_STORAGE_GOVERNANCE_TASK_DB_COMPACT_ENABLED", true),
	}
	if policy.warnUsedRatio <= 0 || policy.warnUsedRatio >= 1 {
		policy.warnUsedRatio = 0.85
	}
	if policy.highUsedRatio <= 0 || policy.highUsedRatio >= 1 {
		policy.highUsedRatio = 0.92
	}
	if policy.highUsedRatio < policy.warnUsedRatio {
		policy.highUsedRatio = policy.warnUsedRatio
	}
	if policy.retentionWarnFactor < 1.0 {
		policy.retentionWarnFactor = 1.0
	}
	if policy.retentionHighFactor < policy.retentionWarnFactor {
		policy.retentionHighFactor = policy.retentionWarnFactor
	}
	return policy
}

func resolveStoragePath(envName string, fallback string) string {
	raw := strings.TrimSpace(os.Getenv(envName))
	if raw == "" {
		raw = fallback
	}
	if raw == "" {
		return ""
	}
	return filepath.Clean(raw)
}

func defaultTrackedPaths() map[string]string {
	dataDir := filepath.Join(".data", "orchestrator")
	return map[string]string{
		"task_db":                   resolveStoragePath("TASK_DB_PATH", filepath.Join(dataDir, "agent_tasks.db")),
		"topic_index":               resolveStoragePath("TOPIC_INDEX_PATH", filepath.Join(dataDir, "topic_index.json")),
		"memory_write_history":      resolveStoragePath("MEMORY_WRITE_HISTORY_PATH", filepath.Join(dataDir, "memory_write_history.ndjson")),
		"trading_history":           resolveStoragePath("TRADING_HISTORY_PATH", filepath.Join(dataDir, "trading_metrics.ndjson")),
		"strategy_history":          resolveStoragePath("STRATEGY_HISTORY_PATH", filepath.Join(dataDir, "strategy_metrics.ndjson")),
		"signal_history":            resolveStoragePath("SIGNAL_HISTORY_PATH", filepath.Join(dataDir, "solana_signals.ndjson")),
		"override_history":          resolveStoragePath("OVERRIDE_HISTORY_PATH", filepath.Join(dataDir, "solana_overrides.ndjson")),
		"recall_monitor":            resolveStoragePath("RECALL_MONITOR_PATH", filepath.Join(dataDir, "recall_monitor.ndjson")),
		"memory_bank_cleanup_state": resolveStoragePath("ORCH_MEMORY_BANK_TELEMETRY_CLEANUP_STATE_PATH", filepath.Join(dataDir, "memory_bank_telemetry_cleanup_state.json")),
		"fanout_payload_blobs":      resolveStoragePath("FANOUT_OUTBOX_PAYLOAD_BLOB_DIR", filepath.Join(dataDir, "fanout_payload_blobs")),
		"mongo_raw_content_blobs":   resolveStoragePath("ORCH_MONGO_RAW_CONTENT_BLOB_DIR", filepath.Join(dataDir, "mongo_raw_content_blobs")),
		"continuation_outbox":       resolveStoragePath("GO_RETRIEVAL_CONTINUATION_DURABLE_DIR", filepath.Join(dataDir, "continuation_outbox")),
	}
}

func fileOrDirSize(path string, maxFiles int) (int64, bool, error) {
	if strings.TrimSpace(path) == "" {
		return 0, false, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if !info.IsDir() {
		return info.Size(), false, nil
	}
	total := int64(0)
	scanned := 0
	truncated := false
	err = filepath.WalkDir(path, func(_ string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		scanned += 1
		if maxFiles > 0 && scanned > maxFiles {
			truncated = true
			return filepath.SkipDir
		}
		item, statErr := entry.Info()
		if statErr != nil {
			return nil
		}
		total += item.Size()
		return nil
	})
	if err != nil && !errors.Is(err, filepath.SkipDir) {
		return total, truncated, err
	}
	return total, truncated, nil
}

func collectTrackedStorage(paths map[string]string, maxFiles int) map[string]any {
	rows := map[string]any{}
	total := int64(0)
	for key, path := range paths {
		sizeBytes, truncated, err := fileOrDirSize(path, maxFiles)
		exists := false
		if strings.TrimSpace(path) != "" {
			if _, statErr := os.Stat(path); statErr == nil {
				exists = true
			}
		}
		row := map[string]any{
			"path":       path,
			"bytes":      sizeBytes,
			"bytesHuman": humanizeBytes(sizeBytes),
			"exists":     exists,
		}
		if truncated {
			row["truncated"] = true
		}
		if err != nil {
			row["error"] = err.Error()
		}
		rows[key] = row
		if sizeBytes > 0 {
			total += sizeBytes
		}
	}
	rows["_total"] = map[string]any{
		"bytes":      total,
		"bytesHuman": humanizeBytes(total),
	}
	return rows
}

func diskUsageSnapshot(root string) (map[string]any, error) {
	cleanRoot := strings.TrimSpace(root)
	if cleanRoot == "" {
		cleanRoot = "."
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(cleanRoot, &fs); err != nil {
		return nil, err
	}
	total := fs.Blocks * uint64(fs.Bsize)
	free := fs.Bavail * uint64(fs.Bsize)
	used := total - free
	usedRatio := 0.0
	if total > 0 {
		usedRatio = float64(used) / float64(total)
	}
	return map[string]any{
		"root":          cleanRoot,
		"totalBytes":    total,
		"freeBytes":     free,
		"usedBytes":     used,
		"usedRatio":     usedRatio,
		"totalHuman":    humanizeBytes(int64(total)),
		"freeHuman":     humanizeBytes(int64(free)),
		"usedHuman":     humanizeBytes(int64(used)),
		"capturedAt":    time.Now().UTC().Format(time.RFC3339),
		"platform":      runtimePlatform(),
		"storageDriver": "statfs",
	}, nil
}

func pressureBand(usedRatio float64, freeBytes uint64, policy storageGovernancePolicy) string {
	if usedRatio >= policy.highUsedRatio {
		return "high"
	}
	if policy.minFreeBytes > 0 && freeBytes <= policy.minFreeBytes {
		return "high"
	}
	if usedRatio >= policy.warnUsedRatio {
		return "warn"
	}
	if policy.minFreeBytes > 0 && freeBytes <= uint64(float64(policy.minFreeBytes)*1.5) {
		return "warn"
	}
	return "healthy"
}

func runtimePlatform() string {
	return runtime.GOOS
}

func humanizeBytes(value int64) string {
	if value < 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	size := float64(value)
	unit := 0
	for size >= 1024.0 && unit < len(units)-1 {
		size /= 1024.0
		unit += 1
	}
	if unit == 0 {
		return fmt.Sprintf("%d %s", int64(size), units[unit])
	}
	return fmt.Sprintf("%.2f %s", size, units[unit])
}

func parseStorageLedgerTime(raw string) (time.Time, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Time{}, false
	}
	if ts, err := time.Parse(time.RFC3339Nano, trimmed); err == nil {
		return ts.UTC(), true
	}
	if ts, err := time.Parse(time.RFC3339, trimmed); err == nil {
		return ts.UTC(), true
	}
	return time.Time{}, false
}

func readStorageLedgerEntries(path string, limit int, since *time.Time, maxLineBytes int) ([]map[string]any, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()

	rows := make([]map[string]any, 0, limit)
	parseErrors := 0
	scanner := bufio.NewScanner(file)
	if maxLineBytes < 64*1024 {
		maxLineBytes = 64 * 1024
	}
	scanner.Buffer(make([]byte, 0, 128*1024), maxLineBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		row := map[string]any{}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			parseErrors += 1
			continue
		}
		if since != nil {
			capturedRaw := anyToString(row["captured_at"])
			if capturedRaw == "" {
				capturedRaw = anyToString(row["timestamp"])
			}
			capturedAt, ok := parseStorageLedgerTime(capturedRaw)
			if !ok || capturedAt.Before(*since) {
				continue
			}
		}
		if len(rows) < limit {
			rows = append(rows, row)
			continue
		}
		copy(rows, rows[1:])
		rows[len(rows)-1] = row
	}
	if err := scanner.Err(); err != nil {
		return rows, parseErrors, err
	}
	return rows, parseErrors, nil
}

func (s *server) storageTelemetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	policy := loadStorageGovernancePolicy()
	disk, diskErr := diskUsageSnapshot(policy.diskRoot)
	diskStatus := "ok"
	if diskErr != nil {
		disk = map[string]any{"root": policy.diskRoot, "error": diskErr.Error()}
		diskStatus = "error"
	}
	pressure := "unknown"
	if diskErr == nil {
		pressure = pressureBand(
			anyToFloat64(disk["usedRatio"], 0.0),
			uint64(anyToInt64(disk["freeBytes"], 0)),
			policy,
		)
	}
	telemetrySummary := map[string]any{"enabled": false}
	if s.telemetrySink != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		summary, err := s.telemetrySink.summary(ctx)
		if err != nil {
			telemetrySummary = map[string]any{"enabled": s.telemetrySink.enabled, "error": err.Error()}
		} else {
			telemetrySummary = summary
		}
	}
	tracked := collectTrackedStorage(defaultTrackedPaths(), 200000)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"capturedAt": time.Now().UTC().Format(time.RFC3339),
		"storageGovernance": map[string]any{
			"enabled":                 policy.enabled,
			"warnUsedRatio":           policy.warnUsedRatio,
			"highUsedRatio":           policy.highUsedRatio,
			"minFreeBytes":            policy.minFreeBytes,
			"retentionWarnMultiplier": policy.retentionWarnFactor,
			"retentionHighMultiplier": policy.retentionHighFactor,
			"taskDbCompactEnabled":    policy.taskDBCompactEnabled,
			"pressureBand":            pressure,
		},
		"dataClasses": map[string]any{
			"learning_memory": map[string]any{
				"retention":      "indefinite",
				"storage":        "go_memory_store + content_blobs",
				"deletionPolicy": "never_auto_delete",
			},
			"rollups": map[string]any{
				"retention":      "indefinite",
				"storage":        "topic rollup graph + history index",
				"deletionPolicy": "never_auto_delete",
			},
			"telemetry": map[string]any{
				"retention_days_hot": envInt("GO_TELEMETRY_RETENTION_DAYS", 75),
				"storage":            "mongo telemetry + compressed blobs",
				"cold_policy":        "content-addressed compressed blobs + pointer refs",
			},
			"ephemeral_state": map[string]any{
				"retention_days_hot": envInt("GO_TELEMETRY_RETENTION_DAYS", 75),
				"storage":            "telemetry lane (isolated)",
				"cold_policy":        "compressed blob refs",
			},
		},
		"disk":             disk,
		"diskStatus":       diskStatus,
		"trackedArtifacts": tracked,
		"telemetrySink":    telemetrySummary,
	})
}

func (s *server) storageTelemetryLedger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}

	ledgerPath := resolveStoragePath("ORCH_STORAGE_LEDGER_PATH", filepath.Join(".data", "orchestrator", "storage_ledger.ndjson"))
	defaultLimit := clampInt(envInt("ORCH_STORAGE_LEDGER_READ_LIMIT_DEFAULT", 168), 1, 5000)
	maxLimit := envInt("ORCH_STORAGE_LEDGER_READ_LIMIT_MAX", 5000)
	if maxLimit < 1 {
		maxLimit = 5000
	}
	maxLimit = clampInt(maxLimit, 1, 20000)
	maxLineBytes := envInt("ORCH_STORAGE_LEDGER_LINE_MAX_BYTES", 2*1024*1024)

	limit := defaultLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid limit query param"})
			return
		}
		limit = parsed
	}
	limit = clampInt(limit, 1, maxLimit)

	sinceRaw := strings.TrimSpace(r.URL.Query().Get("since"))
	var since *time.Time
	if sinceRaw != "" {
		parsed, ok := parseStorageLedgerTime(sinceRaw)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid since query param; expected RFC3339 timestamp"})
			return
		}
		since = &parsed
	}

	rows, parseErrors, err := readStorageLedgerEntries(ledgerPath, limit, since, maxLineBytes)
	exists := true
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			exists = false
			rows = []map[string]any{}
		} else {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":         false,
				"capturedAt": time.Now().UTC().Format(time.RFC3339),
				"path":       ledgerPath,
				"exists":     true,
				"error":      err.Error(),
				"count":      0,
				"rows":       []map[string]any{},
			})
			return
		}
	}

	payload := map[string]any{
		"ok":         true,
		"capturedAt": time.Now().UTC().Format(time.RFC3339),
		"path":       ledgerPath,
		"exists":     exists,
		"count":      len(rows),
		"limit":      limit,
		"since":      sinceRaw,
		"rows":       rows,
	}
	if parseErrors > 0 {
		payload["parseErrors"] = parseErrors
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *server) storageMaintenanceRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if !s.writeAuthorizedRequest(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "Invalid API key"})
		return
	}

	policy := loadStorageGovernancePolicy()
	beforeDisk, beforeErr := diskUsageSnapshot(policy.diskRoot)
	taskResults := map[string]any{}
	errorsList := []string{}

	if s.telemetrySink == nil || !s.telemetrySink.enabled {
		taskResults["telemetry_blob_gc"] = map[string]any{
			"enabled": false,
			"skipped": true,
			"reason":  "telemetry sink disabled",
		}
	} else {
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		result, err := s.telemetrySink.runBlobGCOnce(ctx)
		if err != nil {
			taskResults["telemetry_blob_gc"] = map[string]any{"enabled": true, "ok": false, "error": err.Error()}
			errorsList = append(errorsList, "telemetry_blob_gc: "+err.Error())
		} else {
			taskResults["telemetry_blob_gc"] = map[string]any{"enabled": true, "ok": true, "result": result}
		}
	}
	afterDisk, afterErr := diskUsageSnapshot(policy.diskRoot)
	if beforeErr != nil {
		errorsList = append(errorsList, "disk_before: "+beforeErr.Error())
	}
	if afterErr != nil {
		errorsList = append(errorsList, "disk_after: "+afterErr.Error())
	}

	ok := len(errorsList) == 0
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          ok,
		"performedAt": time.Now().UTC().Format(time.RFC3339),
		"pressureBand": func() string {
			if afterErr == nil {
				return pressureBand(anyToFloat64(afterDisk["usedRatio"], 0.0), uint64(anyToInt64(afterDisk["freeBytes"], 0)), policy)
			}
			if beforeErr == nil {
				return pressureBand(anyToFloat64(beforeDisk["usedRatio"], 0.0), uint64(anyToInt64(beforeDisk["freeBytes"], 0)), policy)
			}
			return "unknown"
		}(),
		"disk": map[string]any{
			"before": beforeDisk,
			"after":  afterDisk,
		},
		"tasks":            taskResults,
		"trackedArtifacts": collectTrackedStorage(defaultTrackedPaths(), 200000),
		"errors":           errorsList,
	})
}

func anyToFloat64(value any, fallback float64) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case uint64:
		return float64(typed)
	default:
		return fallback
	}
}

func anyToInt64(value any, fallback int64) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int32:
		return int64(typed)
	case int64:
		return typed
	case uint:
		return int64(typed)
	case uint32:
		return int64(typed)
	case uint64:
		if typed > uint64(^uint64(0)>>1) {
			return fallback
		}
		return int64(typed)
	case float64:
		return int64(typed)
	case float32:
		return int64(typed)
	default:
		return fallback
	}
}
