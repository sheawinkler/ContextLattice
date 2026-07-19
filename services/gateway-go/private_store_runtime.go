package main

import (
	"errors"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const ownerOnlyMigrationStatusSchemaID = "contextlattice_owner_only_migration_status.v1"

type ownerOnlyMigrationRuntime struct {
	mu sync.RWMutex

	configured       bool
	background       bool
	phase            string
	storeRef         string
	attempts         int64
	processedEntries int64
	enforcedEntries  int64
	batchCount       int64
	maxBatchEntries  int
	startedAt        string
	updatedAt        string
	completedAt      string
	durationMillis   int64
	lastErrorCode    string
}

func newOwnerOnlyMigrationRuntime(root string, configured bool) *ownerOnlyMigrationRuntime {
	phase := "disabled"
	if configured {
		phase = "pending"
	}
	now := nowUTCISO()
	return &ownerOnlyMigrationRuntime{
		configured: configured,
		phase:      phase,
		storeRef:   ownerOnlyStoreRef("owner_only_migration:" + root),
		updatedAt:  now,
	}
}

func ownerOnlyMigrationErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errOwnerOnlyMigrationYield):
		return "startup_budget_exhausted"
	case errors.Is(err, errOwnerOnlyMigrationLocked):
		return "migration_worker_busy"
	default:
		return "migration_failed"
	}
}

func (runtime *ownerOnlyMigrationRuntime) markStarted(background bool) {
	if runtime == nil {
		return
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.attempts++
	runtime.background = background
	runtime.phase = "migrating"
	if runtime.startedAt == "" {
		runtime.startedAt = nowUTCISO()
	}
	runtime.updatedAt = nowUTCISO()
	runtime.lastErrorCode = ""
}

func (runtime *ownerOnlyMigrationRuntime) markWaiting(report ownerOnlyMigrationReport, err error) {
	if runtime == nil {
		return
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.phase = "migrating"
	runtime.background = true
	runtime.applyReportLocked(report)
	runtime.updatedAt = nowUTCISO()
	runtime.lastErrorCode = ownerOnlyMigrationErrorCode(err)
}

func (runtime *ownerOnlyMigrationRuntime) markProgress(report ownerOnlyMigrationReport) {
	if runtime == nil {
		return
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.phase = "migrating"
	runtime.background = true
	runtime.applyReportLocked(report)
	runtime.updatedAt = nowUTCISO()
	runtime.lastErrorCode = ""
}

func (runtime *ownerOnlyMigrationRuntime) markBlocked(report ownerOnlyMigrationReport, err error) {
	if runtime == nil {
		return
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.phase = "blocked"
	runtime.applyReportLocked(report)
	runtime.updatedAt = nowUTCISO()
	runtime.lastErrorCode = ownerOnlyMigrationErrorCode(err)
}

func (runtime *ownerOnlyMigrationRuntime) markReady(report ownerOnlyMigrationReport) {
	if runtime == nil {
		return
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.phase = "ready"
	runtime.applyReportLocked(report)
	runtime.updatedAt = nowUTCISO()
	runtime.completedAt = runtime.updatedAt
	runtime.lastErrorCode = ""
}

func (runtime *ownerOnlyMigrationRuntime) applyReportLocked(report ownerOnlyMigrationReport) {
	runtime.processedEntries = report.ProcessedEntries
	runtime.enforcedEntries = report.EnforcedEntries
	runtime.batchCount = report.BatchCount
	runtime.maxBatchEntries = report.MaxBatchEntries
	runtime.durationMillis = report.DurationMillis
}

func (runtime *ownerOnlyMigrationRuntime) snapshot(ready bool) map[string]any {
	if runtime == nil {
		return map[string]any{
			"schema_id":  ownerOnlyMigrationStatusSchemaID,
			"configured": false,
			"ready":      false,
			"phase":      "disabled",
		}
	}
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	payload := map[string]any{
		"schema_id":         ownerOnlyMigrationStatusSchemaID,
		"configured":        runtime.configured,
		"ready":             ready,
		"phase":             runtime.phase,
		"background":        runtime.background,
		"store_ref":         runtime.storeRef,
		"writer_policy":     ownerOnlyWriterPolicyVersion,
		"attempts":          runtime.attempts,
		"processed_entries": runtime.processedEntries,
		"enforced_entries":  runtime.enforcedEntries,
		"batch_count":       runtime.batchCount,
		"max_batch_entries": runtime.maxBatchEntries,
		"updated_at":        runtime.updatedAt,
		"duration_ms":       runtime.durationMillis,
	}
	if runtime.startedAt != "" {
		payload["started_at"] = runtime.startedAt
	}
	if runtime.completedAt != "" {
		payload["completed_at"] = runtime.completedAt
	}
	if runtime.lastErrorCode != "" {
		payload["last_error_code"] = runtime.lastErrorCode
	}
	return payload
}

func ownerOnlyMigrationStartupBudget() time.Duration {
	millis := clampInt(
		envInt("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_STARTUP_BUDGET_MILLIS", 50),
		1,
		5000,
	)
	return time.Duration(millis) * time.Millisecond
}

func ownerOnlyMigrationRetryDelay() time.Duration {
	millis := clampInt(
		envInt("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_RETRY_MILLIS", 250),
		10,
		10000,
	)
	return time.Duration(millis) * time.Millisecond
}

func (m *memoryStore) migrationSnapshot() map[string]any {
	if m == nil {
		return newOwnerOnlyMigrationRuntime("", false).snapshot(false)
	}
	return m.migration.snapshot(m.isEnabled())
}

func memoryStoreReadyRequiredPath(path string) bool {
	path = strings.TrimSpace(path)
	if strings.HasPrefix(path, "/v1/memory/") || strings.HasPrefix(path, "/memory/files/") {
		return true
	}
	switch path {
	case "/memory/write",
		"/memory/write/batch",
		"/memory/browser-context",
		"/memory/recent",
		"/memory/topics",
		"/memory/topics/list",
		"/memory/topic-rollups",
		"/tools/checkpoint_write",
		"/tools/ephemeral_memory_write",
		"/tools/ephemeral_memory_purge",
		"/tools/memory_file_get",
		"/tools/memory_write_batch":
		return true
	default:
		return false
	}
}

func (s *server) enforceMemoryStoreReadiness(w http.ResponseWriter, r *http.Request) bool {
	if s == nil || s.memoryStore == nil || !s.memoryStore.isConfigured() || s.memoryStore.isEnabled() {
		return true
	}
	if !memoryStoreReadyRequiredPath(r.URL.Path) {
		return true
	}
	snapshot := s.memoryStore.migrationSnapshot()
	phase := anyToString(snapshot["phase"])
	code := "owner_only_migration_in_progress"
	if phase == "blocked" {
		code = "owner_only_migration_blocked"
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"ok":          false,
		"error":       "memory_store_not_ready",
		"code":        code,
		"memoryStore": snapshot,
	})
	return false
}

func (m *memoryStore) initializeAfterOwnerOnlyMigration() error {
	if m == nil || !m.isConfigured() {
		return nil
	}
	m.initializeOnce.Do(func() {
		for _, path := range []string{
			filepath.Dir(m.policy.historyPath),
			m.currentStateRootPath(),
			filepath.Dir(m.policy.accessLogPath),
			filepath.Dir(m.policy.edgePath),
			filepath.Dir(m.policy.agentEdgePath),
		} {
			if err := ensureOwnerOnlyDirectory(path, true); err != nil {
				m.initializeErr = err
				return
			}
		}
		if m.policy.contentAddressed {
			if err := ensureOwnerOnlyDirectory(m.policy.contentBlobsPath, true); err != nil {
				m.initializeErr = err
				return
			}
		}
		if err := m.loadExactStateIndex(); err != nil {
			m.initializeErr = err
			return
		}
		if err := m.loadCurrentState(); err != nil {
			m.initializeErr = err
			return
		}
		if err := m.loadHistory(); err != nil {
			m.initializeErr = err
			return
		}
		if err := m.loadAccessLog(); err != nil {
			m.initializeErr = err
			return
		}
		if err := m.loadEdges(); err != nil {
			m.initializeErr = err
			return
		}
		m.initializeErr = m.loadAgentEventEdges()
	})
	return m.initializeErr
}

func (m *memoryStore) finishOwnerOnlyMigration(report ownerOnlyMigrationReport) error {
	if err := m.initializeAfterOwnerOnlyMigration(); err != nil {
		m.migration.markBlocked(report, err)
		return err
	}
	m.ready.Store(true)
	m.migration.markReady(report)
	return nil
}

func (m *memoryStore) startOwnerOnlyMigrationBackground() {
	if m == nil {
		return
	}
	m.migrationOnce.Do(func() {
		go func() {
			for {
				m.migration.markStarted(true)
				report, err := migrateOwnerOnlyStoreWithOptions(m.policy.rootPath, ownerOnlyMigrationOptions{
					onProgress: m.migration.markProgress,
				})
				if errors.Is(err, errOwnerOnlyMigrationLocked) {
					m.migration.markWaiting(report, err)
					time.Sleep(ownerOnlyMigrationRetryDelay())
					continue
				}
				if err != nil {
					m.migration.markBlocked(report, err)
					log.Printf("gateway-go owner-only migration blocked: code=%s", ownerOnlyMigrationErrorCode(err))
					return
				}
				if err := m.finishOwnerOnlyMigration(report); err != nil {
					log.Printf("gateway-go memory store initialization blocked after migration")
				}
				return
			}
		}()
	})
}
