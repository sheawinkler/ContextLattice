package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	frontierT5LedgerEnabledEnv    = "CONTEXTLATTICE_FRONTIER_T5_LEDGER_ENABLED"
	frontierT5LedgerPathEnv       = "CONTEXTLATTICE_FRONTIER_T5_LEDGER_PATH"
	frontierT5LedgerMaxBytesEnv   = "CONTEXTLATTICE_FRONTIER_T5_LEDGER_MAX_BYTES"
	frontierT5LedgerMaxEntriesEnv = "CONTEXTLATTICE_FRONTIER_T5_LEDGER_MAX_ENTRIES"
	frontierT5LedgerFsyncEnv      = "CONTEXTLATTICE_FRONTIER_T5_LEDGER_FSYNC"
)

type frontierT5Ledger struct {
	mu                  sync.RWMutex
	enabled             bool
	path                string
	maxBytes            int64
	maxEntries          int
	fsync               bool
	rows                []map[string]any
	byID                map[string]map[string]any
	lifecycleLatest     map[string]map[string]any
	temperatureLatest   map[string]map[string]any
	contradictionLatest map[string]map[string]any
	parseErrors         int
	compactionCount     int
	lastPersistedAt     string
	lastError           string
}

func frontierT5LedgerPath() string {
	return resolveStoragePath(frontierT5LedgerPathEnv, filepath.Join(".data", "orchestrator", "frontier_t5_policy_lab.ndjson"))
}

func newFrontierT5LedgerFromEnv() (*frontierT5Ledger, error) {
	ledger := &frontierT5Ledger{
		enabled:             envBool(frontierT5LedgerEnabledEnv, true),
		path:                frontierT5LedgerPath(),
		maxBytes:            int64(clampInt(envInt(frontierT5LedgerMaxBytesEnv, 4*1024*1024), 64*1024, 64*1024*1024)),
		maxEntries:          clampInt(envInt(frontierT5LedgerMaxEntriesEnv, 2048), 32, 20000),
		fsync:               envBool(frontierT5LedgerFsyncEnv, true),
		rows:                []map[string]any{},
		byID:                map[string]map[string]any{},
		lifecycleLatest:     map[string]map[string]any{},
		temperatureLatest:   map[string]map[string]any{},
		contradictionLatest: map[string]map[string]any{},
	}
	if !ledger.enabled {
		return ledger, nil
	}
	if err := prepareOwnerOnlyFile(ledger.path, strings.TrimSpace(os.Getenv(frontierT5LedgerPathEnv)) == ""); err != nil {
		return ledger, err
	}
	if err := ledger.load(); err != nil {
		return ledger, err
	}
	return ledger, nil
}

func newFrontierT5LedgerForTest(path string) (*frontierT5Ledger, error) {
	ledger := &frontierT5Ledger{
		enabled: true, path: path, maxBytes: 2 * 1024 * 1024, maxEntries: 512,
		rows: []map[string]any{}, byID: map[string]map[string]any{},
		lifecycleLatest: map[string]map[string]any{}, temperatureLatest: map[string]map[string]any{},
		contradictionLatest: map[string]map[string]any{},
	}
	if err := prepareOwnerOnlyFile(path, false); err != nil {
		return nil, err
	}
	return ledger, ledger.load()
}

func (l *frontierT5Ledger) load() error {
	file, err := os.Open(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open Frontier T5 ledger: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			l.parseErrors++
			continue
		}
		l.indexLocked(row)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan Frontier T5 ledger: %w", err)
	}
	l.trimLocked()
	return nil
}

func frontierT5EntityKey(project, file string) string {
	return strings.ToLower(strings.TrimSpace(project)) + "\x00" + strings.TrimSpace(file)
}

func (l *frontierT5Ledger) indexLocked(row map[string]any) {
	row = cloneMap(row)
	l.rows = append(l.rows, row)
	if id := strings.TrimSpace(firstNonEmptyStrings(anyToString(row["receipt_id"]), anyToString(row["resolution_id"]), anyToString(row["decision_id"]))); id != "" {
		l.byID[id] = row
	}
	key := frontierT5EntityKey(anyToString(row["project"]), anyToString(row["file"]))
	switch anyToString(row["schema_id"]) {
	case memoryRetirementContractID:
		if key != "\x00" {
			l.lifecycleLatest[key] = row
		}
	case storageTemperatureDecisionContractID:
		if key != "\x00" {
			l.temperatureLatest[key] = row
		}
	case contradictionResolutionContractID:
		if project := strings.ToLower(strings.TrimSpace(anyToString(row["project"]))); project != "" {
			l.contradictionLatest[project] = row
		}
	}
}

func (l *frontierT5Ledger) trimLocked() {
	if l.maxEntries < 1 || len(l.rows) <= l.maxEntries {
		return
	}
	l.rows = append([]map[string]any(nil), l.rows[len(l.rows)-l.maxEntries:]...)
	l.reindexLocked()
}

func (l *frontierT5Ledger) reindexLocked() {
	rows := append([]map[string]any(nil), l.rows...)
	l.rows = []map[string]any{}
	l.byID = map[string]map[string]any{}
	l.lifecycleLatest = map[string]map[string]any{}
	l.temperatureLatest = map[string]map[string]any{}
	l.contradictionLatest = map[string]map[string]any{}
	for _, row := range rows {
		l.indexLocked(row)
	}
}

func (l *frontierT5Ledger) record(row map[string]any) (bool, error) {
	if l == nil || !l.enabled {
		return false, errors.New("Frontier T5 ledger is disabled")
	}
	id := strings.TrimSpace(firstNonEmptyStrings(anyToString(row["receipt_id"]), anyToString(row["resolution_id"]), anyToString(row["decision_id"])))
	if id == "" {
		return false, errors.New("Frontier T5 receipt identity is required")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.byID[id]; exists {
		return false, nil
	}
	file, err := openOwnerOnlyAppend(l.path, false)
	if err != nil {
		l.lastError = clipText(err.Error(), 500)
		return false, err
	}
	if err := json.NewEncoder(file).Encode(row); err != nil {
		_ = file.Close()
		l.lastError = clipText(err.Error(), 500)
		return false, err
	}
	if l.fsync {
		if err := file.Sync(); err != nil {
			_ = file.Close()
			l.lastError = clipText(err.Error(), 500)
			return false, err
		}
	}
	if err := file.Close(); err != nil {
		l.lastError = clipText(err.Error(), 500)
		return false, err
	}
	l.indexLocked(row)
	l.trimLocked()
	l.lastPersistedAt = nowUTCISO()
	l.lastError = ""
	if info, err := os.Stat(l.path); err == nil && info.Size() > l.maxBytes {
		if err := l.compactLocked(); err != nil {
			l.lastError = clipText(err.Error(), 500)
			return true, err
		}
	}
	return true, nil
}

func frontierT5MutationPreparationID(receiptID string) string {
	return "t5prep_" + sha256Hex(strings.TrimSpace(receiptID))[:32]
}

func (l *frontierT5Ledger) prepareMutation(receiptID string, details map[string]any) (map[string]any, bool, error) {
	if l == nil || !l.enabled || strings.TrimSpace(l.path) == "" {
		return nil, false, errors.New("Frontier T5 ledger is unavailable; mutation refused")
	}
	receiptID = strings.TrimSpace(receiptID)
	if receiptID == "" {
		return nil, false, errors.New("Frontier T5 transaction receipt identity is required")
	}
	preparationID := frontierT5MutationPreparationID(receiptID)
	if existing := l.receipt(preparationID); len(existing) > 0 {
		return existing, false, nil
	}
	prepared := cloneMap(details)
	prepared["schema_id"] = "frontier_t5_mutation_preparation.internal.v1"
	prepared["receipt_id"] = preparationID
	prepared["transaction_receipt_id"] = receiptID
	prepared["operation"] = "prepare"
	prepared["phase"] = "prepared"
	prepared["recorded_at"] = nowUTCISO()
	recorded, err := l.record(prepared)
	if err != nil {
		return nil, false, err
	}
	if !recorded {
		if existing := l.receipt(preparationID); len(existing) > 0 {
			return existing, false, nil
		}
		return nil, false, errors.New("Frontier T5 mutation preparation was not persisted")
	}
	return prepared, true, nil
}

func (l *frontierT5Ledger) compactLocked() error {
	rows := append([]map[string]any(nil), l.rows...)
	sort.SliceStable(rows, func(i, j int) bool {
		return anyToString(rows[i]["recorded_at"]) < anyToString(rows[j]["recorded_at"])
	})
	var out strings.Builder
	encoder := json.NewEncoder(&out)
	for _, row := range rows {
		if err := encoder.Encode(row); err != nil {
			return err
		}
	}
	if err := writeOwnerOnlyDurableAtomicFile(l.path, []byte(out.String()), true); err != nil {
		return err
	}
	l.compactionCount++
	return nil
}

func (l *frontierT5Ledger) receipt(id string) map[string]any {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return cloneMap(l.byID[strings.TrimSpace(id)])
}

func (l *frontierT5Ledger) latest(kind, project, file string) map[string]any {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	key := frontierT5EntityKey(project, file)
	switch kind {
	case "retirement":
		return cloneMap(l.lifecycleLatest[key])
	case "temperature":
		return cloneMap(l.temperatureLatest[key])
	case "contradiction":
		return cloneMap(l.contradictionLatest[strings.ToLower(strings.TrimSpace(project))])
	default:
		return nil
	}
}

func (l *frontierT5Ledger) snapshot() map[string]any {
	if l == nil {
		return map[string]any{"enabled": false, "entry_count": 0}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return map[string]any{
		"enabled": l.enabled, "entry_count": len(l.rows), "retirement_count": len(l.lifecycleLatest),
		"temperature_count": len(l.temperatureLatest), "contradiction_project_count": len(l.contradictionLatest),
		"max_bytes": l.maxBytes, "max_entries": l.maxEntries, "parse_errors": l.parseErrors,
		"compaction_count": l.compactionCount, "last_persisted_at": l.lastPersistedAt, "last_error": l.lastError,
	}
}
