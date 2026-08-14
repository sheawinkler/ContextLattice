package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	continuationDurableSchemaVersion                         = 1
	continuationEvaluationCleanupSchemaID                    = "contextlattice_evaluation_continuation_cleanup.v1"
	continuationEvaluationCleanupVersion                     = 1
	continuationEvaluationCleanupReceiptFile                 = ".evaluation-cleanup-receipt.json"
	continuationEvaluationCleanupMaxJobRefs                  = 16
	continuationEvaluationCleanupMaxPending                  = 256
	continuationEvaluationCleanupStatePending                = "pending"
	continuationEvaluationCleanupStateCompleted              = "completed"
	continuationEvaluationCleanupIndexDirectory              = ".evaluation-cleanup-index"
	continuationEvaluationCleanupMarkerMaxBytes              = int64(16 * 1024)
	continuationEvaluationCleanupMarkerSchemaID              = "contextlattice_evaluation_cleanup_marker.v1"
	continuationEvaluationCleanupMarkerVersion               = 1
	continuationEvaluationCleanupMarkerAction                = "evaluation_cleanup_completed"
	continuationEvaluationCleanupMarkerIndexFile             = "manifest.json"
	continuationEvaluationCleanupMarkerIndexSchemaID         = "contextlattice_evaluation_cleanup_marker_index.v1"
	continuationEvaluationCleanupMarkerIndexVersion          = 1
	continuationEvaluationCleanupMarkerIndexAction           = "evaluation_cleanup_marker_index"
	continuationEvaluationCleanupMarkerIndexStateReady       = "ready"
	continuationEvaluationCleanupMarkerIndexStatePending     = "pending"
	continuationEvaluationCleanupMarkerIndexStateLimit       = "limit"
	continuationEvaluationCleanupMarkerIndexStateUnavailable = "unavailable"
	// Content-addressed cleanup markers are exact-once custody. They are never
	// compacted or deleted automatically. These hard limits provide an explicit
	// operator-visible fail-closed boundary before storage exhaustion.
	continuationEvaluationCleanupMarkerIndexMaxCount = 100000
	continuationEvaluationCleanupMarkerIndexMaxBytes = int64(64 * 1024 * 1024)
	// Startup reads are deliberately bounded independently of the runtime queue
	// caps. A corrupted or abandoned durable directory must not turn process
	// startup into an unbounded directory walk or file allocation.
	continuationDurableLoadMaxFiles        = 2048
	continuationDurableLoadMaxBytes        = int64(32 * 1024 * 1024)
	continuationDurableJobMaxBytes         = int64(256 * 1024)
	continuationDurableReceiptMaxBytes     = int64(128 * 1024)
	continuationDurableQuarantineDirectory = ".quarantine"
)

var continuationDurableCounter atomic.Uint64

type continuationDurableJob struct {
	SchemaVersion int               `json:"schema_version"`
	ID            string            `json:"id"`
	Source        string            `json:"source"`
	Reason        string            `json:"reason"`
	StreamToken   string            `json:"stream_token"`
	Fingerprint   string            `json:"fingerprint"`
	BaseRequest   map[string]any    `json:"base_request"`
	Headers       map[string]string `json:"headers,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
	DueAt         time.Time         `json:"due_at"`
	Attempts      int               `json:"attempts"`
	LastStatus    string            `json:"last_status,omitempty"`
	LastError     string            `json:"last_error,omitempty"`
}

type continuationDurableSnapshot struct {
	Enabled                      bool
	Dir                          string
	Pending                      int
	BySource                     map[string]int
	OldestAgeSecs                float64
	MaxPending                   int
	MaxPendingBySource           int
	MaxAttempts                  int
	LastEnqueueAt                string
	LastDrainAt                  string
	LastError                    string
	EvaluationCleanup            map[string]any
	EvaluationCleanupTotal       int
	EvaluationCleanupMarkerIndex map[string]any
}

type continuationDurableQueue struct {
	enabled            bool
	dir                string
	maxPending         int
	maxPendingBySource int
	maxAttempts        int
	drainBatch         int
	pollInterval       time.Duration
	retryBase          time.Duration
	retryMax           time.Duration

	mu                     sync.Mutex
	jobs                   map[string]*continuationDurableJob
	fingerprintIndex       map[string]string
	lastEnqueueAt          string
	lastDrainAt            string
	lastError              string
	evaluationCleanup      map[string]any
	evaluationCleanupTotal int
	// evaluationCleanupWriter is injectable for fault-injection tests. The
	// production default is the owner-only durable atomic writer.
	evaluationCleanupWriter func(string, []byte, bool) error
	// Marker writes and ancestor directory syncs are injectable for
	// crash/power-loss tests. A marker is committed only after every newly
	// created ancestor and the marker itself have been durably synced.
	evaluationCleanupMarkerWriter func(string, string, string, string, []byte) error
	// Test-only fault hook. Production marker directory durability is provided
	// by descriptor-relative platform helpers; this hook is nil by default and
	// never performs pathname mutation in the marker subsystem.
	evaluationCleanupDirectorySync func(string) error
	// evaluationCleanupMarkerRebuildHook is test-only fault injection used to
	// replace an ancestor between descriptor enumeration and marker reads. The
	// production path leaves it nil and fails closed on identity drift.
	evaluationCleanupMarkerRebuildHook func(string) error
	// evaluationCleanupDeleter is injectable for delete-failure and crash-window
	// tests. Production deletion is the queue job-path unlink.
	evaluationCleanupDeleter                      func(string) error
	loadMaxFiles                                  int
	loadMaxBytes                                  int64
	jobMaxBytes                                   int64
	receiptMaxBytes                               int64
	evaluationCleanupMarkerCount                  int
	evaluationCleanupMarkerBytes                  int64
	evaluationCleanupMarkerState                  string
	evaluationCleanupMarkerLimitReason            string
	evaluationCleanupMarkerPendingRef             string
	evaluationCleanupMarkerPendingBytes           int64
	evaluationCleanupMarkerMaxCount               int
	evaluationCleanupMarkerMaxBytes               int64
	evaluationCleanupMarkerMigrationState         string
	evaluationCleanupMarkerMigrationPlanDigest    string
	evaluationCleanupMarkerMigrationReceiptDigest string
	// MigrationGeneration is the monotonic compare-and-swap epoch. The target
	// generation identifies the cap/plan currently selected by that epoch; a
	// rollback advances the epoch while selecting an older target.
	evaluationCleanupMarkerMigrationGeneration       int64
	evaluationCleanupMarkerMigrationTargetGeneration int64
}

func newContinuationDurableQueue(policy retrievalPolicy) *continuationDurableQueue {
	queue := &continuationDurableQueue{
		enabled:                         policy.continuationDurableEnabled,
		dir:                             strings.TrimSpace(policy.continuationDurableDir),
		maxPending:                      policy.continuationDurableMaxPending,
		maxPendingBySource:              policy.continuationDurableMaxPendingBySrc,
		maxAttempts:                     policy.continuationDurableMaxAttempts,
		drainBatch:                      policy.continuationDurableDrainBatch,
		pollInterval:                    policy.continuationDurablePollInterval,
		retryBase:                       policy.continuationDurableRetryBase,
		retryMax:                        policy.continuationDurableRetryMax,
		jobs:                            map[string]*continuationDurableJob{},
		fingerprintIndex:                map[string]string{},
		evaluationCleanup:               map[string]any{},
		evaluationCleanupWriter:         writeOwnerOnlyDurableAtomicFile,
		evaluationCleanupMarkerWriter:   writeEvaluationCleanupMarkerDurable,
		evaluationCleanupDirectorySync:  nil,
		evaluationCleanupDeleter:        os.Remove,
		loadMaxFiles:                    continuationDurableLoadMaxFiles,
		loadMaxBytes:                    continuationDurableLoadMaxBytes,
		jobMaxBytes:                     continuationDurableJobMaxBytes,
		receiptMaxBytes:                 continuationDurableReceiptMaxBytes,
		evaluationCleanupMarkerState:    continuationEvaluationCleanupMarkerIndexStateReady,
		evaluationCleanupMarkerMaxCount: continuationEvaluationCleanupMarkerIndexMaxCount,
		evaluationCleanupMarkerMaxBytes: continuationEvaluationCleanupMarkerIndexMaxBytes,
	}
	if !queue.enabled {
		return queue
	}
	if queue.dir == "" {
		queue.enabled = false
		queue.lastError = "durable continuation dir is empty"
		return queue
	}
	if err := ensureOwnerOnlyDirectoryDurable(queue.dir, true); err != nil {
		queue.enabled = false
		queue.lastError = err.Error()
		log.Printf("gateway-go continuation durable queue disabled: %v", err)
		return queue
	}
	if err := queue.loadFromDisk(); err != nil {
		queue.lastError = err.Error()
		log.Printf("gateway-go continuation durable queue load warning: %v", err)
	}
	return queue
}

// ensureOwnerOnlyDirectoryDurable creates a directory tree one component at a
// time and syncs each newly-created parent. MkdirAll alone does not provide a
// power-loss proof for the directory entries that lead to a marker shard.
func ensureOwnerOnlyDirectoryDurable(path string, tightenExisting bool) error {
	clean := filepath.Clean(strings.TrimSpace(path))
	if clean == "" || clean == "." {
		return errors.New("owner-only durable directory path is empty")
	}
	missing := []string{}
	cursor := clean
	for {
		info, err := os.Lstat(cursor)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("owner-only durable directory is not a real directory")
			}
			if len(missing) == 0 && tightenExisting {
				return enforceOwnerOnlyPermissions(cursor, ownerOnlyDirectoryMode)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = append(missing, cursor)
		parent := filepath.Dir(cursor)
		if parent == cursor {
			return errors.New("owner-only durable directory has no existing ancestor")
		}
		cursor = parent
	}
	for index := len(missing) - 1; index >= 0; index-- {
		created := missing[index]
		if err := os.Mkdir(created, ownerOnlyDirectoryMode); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		if err := ensureOwnerOnlyDirectory(created, true); err != nil {
			return err
		}
		if err := syncOwnerOnlyDirectory(filepath.Dir(created)); err != nil {
			return err
		}
	}
	return nil
}

// ensureEvaluationCleanupMarkerDirectoriesLocked performs marker directory
// creation below an already-established queue root through descriptor-relative
// platform helpers. It deliberately does not Lstat/Mkdir/path-sync: a path
// replacement between those operations and the later marker write could move
// custody outside the queue root. The optional hook is fault injection only.
func (q *continuationDurableQueue) ensureEvaluationCleanupMarkerDirectoriesLocked(path string) error {
	if q == nil || strings.TrimSpace(q.dir) == "" {
		return errors.New("evaluation cleanup marker root is missing")
	}
	index, shard, err := evaluationCleanupMarkerDirectoryComponents(q.dir, path)
	if err != nil {
		return err
	}
	return ensureEvaluationCleanupMarkerDirectoriesDurable(q.dir, index, shard, q.evaluationCleanupDirectorySync)
}

func (q *continuationDurableQueue) loadFromDisk() error {
	if q == nil || !q.enabled {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	markerIndexErr := q.loadEvaluationCleanupMarkerIndexLocked()
	entries, overflow, err := q.readDirectoryEntriesLocked()
	if err != nil {
		return err
	}
	var firstErr error
	if markerIndexErr != nil {
		firstErr = markerIndexErr
	}
	if overflow != nil {
		if quarantineErr := q.quarantineEntryLocked(overflow, "file_count"); quarantineErr != nil {
			firstErr = quarantineErr
		} else {
			firstErr = fmt.Errorf("continuation durable directory exceeds %d files", q.loadFileLimit())
		}
	}
	now := time.Now().UTC()
	loadedBytes := int64(0)
	cleanupAttempted := map[string]bool{}
	for _, entry := range entries {
		if entry == nil || entry.IsDir() {
			continue
		}
		name := strings.TrimSpace(entry.Name())
		if name == "" || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		path := filepath.Join(q.dir, name)
		maxBytes := q.jobByteLimit()
		if name == continuationEvaluationCleanupReceiptFile {
			maxBytes = q.receiptByteLimit()
		}
		info, statErr := os.Lstat(path)
		if statErr != nil {
			if firstErr == nil {
				firstErr = statErr
			}
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			if q.preservePendingEvaluationCleanupEntryLocked(name, cleanupAttempted) {
				if firstErr == nil {
					firstErr = errors.New("pending evaluation cleanup job is not a regular file")
				}
				continue
			}
			if firstErr == nil {
				firstErr = errors.New("continuation durable entry is not a regular file")
			}
			continue
		}
		if info.Size() > maxBytes || loadedBytes > q.loadByteLimit()-minInt64(info.Size(), q.loadByteLimit()) {
			if q.preservePendingEvaluationCleanupEntryLocked(name, cleanupAttempted) {
				if firstErr == nil {
					firstErr = fmt.Errorf("pending evaluation cleanup file %s exceeds bounded startup bytes", name)
				}
				continue
			}
			if quarantineErr := q.quarantineEntryLocked(entry, "byte_limit"); quarantineErr != nil && firstErr == nil {
				firstErr = quarantineErr
			} else if firstErr == nil {
				firstErr = fmt.Errorf("continuation durable file %s exceeds bounded startup bytes", name)
			}
			continue
		}
		if permissionErr := ensureOwnerOnlyFile(path); permissionErr != nil {
			if firstErr == nil {
				firstErr = permissionErr
			}
			continue
		}
		raw, readErr := readContinuationDurableFileBounded(path, maxBytes)
		if readErr != nil {
			if errors.Is(readErr, errContinuationDurableFileOversized) {
				if q.preservePendingEvaluationCleanupEntryLocked(name, cleanupAttempted) {
					if firstErr == nil {
						firstErr = readErr
					}
					continue
				}
				if quarantineErr := q.quarantineEntryLocked(entry, "byte_limit"); quarantineErr != nil && firstErr == nil {
					firstErr = quarantineErr
				} else if firstErr == nil {
					firstErr = readErr
				}
			} else if firstErr == nil {
				firstErr = readErr
			}
			continue
		}
		if loadedBytes+int64(len(raw)) > q.loadByteLimit() {
			if q.preservePendingEvaluationCleanupEntryLocked(name, cleanupAttempted) {
				if firstErr == nil {
					firstErr = fmt.Errorf("pending evaluation cleanup startup bytes exceed %d", q.loadByteLimit())
				}
				continue
			}
			if quarantineErr := q.quarantineEntryLocked(entry, "byte_limit"); quarantineErr != nil && firstErr == nil {
				firstErr = quarantineErr
			} else if firstErr == nil {
				firstErr = fmt.Errorf("continuation durable startup bytes exceed %d", q.loadByteLimit())
			}
			continue
		}
		loadedBytes += int64(len(raw))
		if name == continuationEvaluationCleanupReceiptFile {
			if receiptErr := q.loadEvaluationCleanupReceiptBytesLocked(raw); receiptErr != nil {
				// Keep malformed receipts recoverable but out of the hot path. A
				// subsequent cleanup can replace the quarantined receipt.
				if quarantineErr := q.quarantineEntryLocked(entry, "invalid_receipt"); quarantineErr != nil && firstErr == nil {
					firstErr = quarantineErr
				} else if firstErr == nil {
					firstErr = receiptErr
				}
			}
			continue
		}
		var job continuationDurableJob
		if unmarshalErr := json.Unmarshal(raw, &job); unmarshalErr != nil {
			if q.preservePendingEvaluationCleanupEntryLocked(name, cleanupAttempted) {
				if firstErr == nil {
					firstErr = unmarshalErr
				}
				continue
			}
			if quarantineErr := q.quarantineEntryLocked(entry, "invalid_job"); quarantineErr != nil && firstErr == nil {
				firstErr = quarantineErr
			} else if firstErr == nil {
				firstErr = unmarshalErr
			}
			continue
		}
		q.normalizeJobLocked(&job, strings.TrimSuffix(name, ".json"), now)
		if continuationDurableJobEvaluationOwned(&job) {
			// Persist the pending intent before unlinking. If that write fails,
			// the job remains on disk for a future restart.
			q.jobs[job.ID] = &job
			if job.Fingerprint != "" {
				q.fingerprintIndex[job.Fingerprint] = job.ID
			}
			cleanupAttempted[continuationEvaluationCleanupJobRef(&job)] = true
			if cleanupErr := q.cleanupEvaluationJobLocked(&job, "restart_load"); cleanupErr != nil && firstErr == nil {
				firstErr = cleanupErr
			}
			continue
		}
		if job.Source == "" || len(job.BaseRequest) == 0 {
			_ = os.Remove(path)
			continue
		}
		if job.Attempts >= q.maxAttempts {
			_ = os.Remove(path)
			continue
		}
		q.jobs[job.ID] = &job
		if job.Fingerprint != "" {
			q.fingerprintIndex[job.Fingerprint] = job.ID
		}
	}
	if reconcileErr := q.reconcilePendingEvaluationCleanupLocked(cleanupAttempted); reconcileErr != nil && firstErr == nil {
		firstErr = reconcileErr
	}
	q.trimExcessLocked()
	return firstErr
}

var errContinuationDurableFileOversized = errors.New("continuation durable file exceeds bounded startup size")

func (q *continuationDurableQueue) loadFileLimit() int {
	if q != nil && q.loadMaxFiles > 0 {
		return q.loadMaxFiles
	}
	return continuationDurableLoadMaxFiles
}

func (q *continuationDurableQueue) loadByteLimit() int64 {
	if q != nil && q.loadMaxBytes > 0 {
		return q.loadMaxBytes
	}
	return continuationDurableLoadMaxBytes
}

func (q *continuationDurableQueue) jobByteLimit() int64 {
	if q != nil && q.jobMaxBytes > 0 {
		return q.jobMaxBytes
	}
	return continuationDurableJobMaxBytes
}

func (q *continuationDurableQueue) receiptByteLimit() int64 {
	if q != nil && q.receiptMaxBytes > 0 {
		return q.receiptMaxBytes
	}
	return continuationDurableReceiptMaxBytes
}

func (q *continuationDurableQueue) readDirectoryEntriesLocked() ([]os.DirEntry, os.DirEntry, error) {
	handle, err := os.Open(q.dir)
	if err != nil {
		return nil, nil, err
	}
	defer handle.Close()
	// The process-shared owner lock is durable queue metadata, not a job. Read
	// one extra entry so it cannot consume a bounded startup slot even when the
	// directory implementation returns it among the first entries.
	entries, err := handle.ReadDir(q.loadFileLimit() + 2)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, err
	}
	filteredEntries := entries[:0]
	for _, entry := range entries {
		if entry == nil || entry.Name() == continuationEvaluationCleanupMarkerMigrationOwnerLockFile {
			continue
		}
		filteredEntries = append(filteredEntries, entry)
	}
	entries = filteredEntries
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	if len(entries) <= q.loadFileLimit() {
		return entries, nil, nil
	}
	overflow := entries[q.loadFileLimit()]
	return entries[:q.loadFileLimit()], overflow, nil
}

func readContinuationDurableFileBounded(path string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, errContinuationDurableFileOversized
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxBytes {
		return nil, errContinuationDurableFileOversized
	}
	return raw, nil
}

func (q *continuationDurableQueue) quarantineEntryLocked(entry os.DirEntry, reason string) error {
	if q == nil || entry == nil || entry.IsDir() {
		return nil
	}
	name := strings.TrimSpace(entry.Name())
	if name == "" {
		return errors.New("continuation durable entry has empty name")
	}
	path := filepath.Join(q.dir, name)
	if err := ensureOwnerOnlyFile(path); err != nil {
		return err
	}
	quarantineDir := filepath.Join(q.dir, continuationDurableQuarantineDirectory)
	if err := ensureOwnerOnlyDirectory(quarantineDir, true); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(name + "|" + strings.TrimSpace(reason)))
	destination := filepath.Join(quarantineDir, name+"."+hex.EncodeToString(sum[:6])+".quarantine")
	if _, err := os.Lstat(destination); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(path, destination); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func continuationDurableJobEvaluationOwned(job *continuationDurableJob) bool {
	return job != nil && retrievalEvaluationSideEffectsSuppressed(nil, job.BaseRequest)
}

func continuationEvaluationCleanupDigest(receipt map[string]any) string {
	copyReceipt := cloneAnyMap(receipt)
	delete(copyReceipt, "digest")
	raw, _ := json.Marshal(copyReceipt)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type continuationEvaluationCleanupPendingJob struct {
	JobID          string
	Source         string
	Ref            string
	Phase          string
	AlreadyCounted bool
}

func continuationEvaluationCleanupPendingJobs(value any) ([]continuationEvaluationCleanupPendingJob, bool) {
	if value == nil {
		return nil, true
	}
	var rawItems []any
	switch typed := value.(type) {
	case []any:
		rawItems = typed
	case []map[string]any:
		rawItems = make([]any, len(typed))
		for index := range typed {
			rawItems[index] = typed[index]
		}
	default:
		return nil, false
	}
	if len(rawItems) > continuationEvaluationCleanupMaxPending {
		return nil, false
	}
	pending := make([]continuationEvaluationCleanupPendingJob, 0, len(rawItems))
	for _, raw := range rawItems {
		row := anyMap(raw)
		item := continuationEvaluationCleanupPendingJob{
			JobID:          strings.TrimSpace(anyToString(row["job_id"])),
			Source:         strings.TrimSpace(strings.ToLower(anyToString(row["source"]))),
			Ref:            strings.TrimSpace(anyToString(row["job_ref"])),
			Phase:          strings.TrimSpace(anyToString(row["phase"])),
			AlreadyCounted: anyToBool(row["already_counted"]),
		}
		if item.JobID == "" || item.Source == "" || item.Ref == "" {
			return nil, false
		}
		pending = append(pending, item)
	}
	return pending, true
}

func continuationEvaluationCleanupPendingValue(pending []continuationEvaluationCleanupPendingJob) []any {
	value := make([]any, 0, len(pending))
	for _, item := range pending {
		value = append(value, map[string]any{
			"job_id": item.JobID, "source": item.Source, "job_ref": item.Ref, "phase": item.Phase,
			"already_counted": item.AlreadyCounted,
		})
	}
	return value
}

func (q *continuationDurableQueue) evaluationCleanupPendingJobsLocked() []continuationEvaluationCleanupPendingJob {
	if q == nil {
		return nil
	}
	pending, valid := continuationEvaluationCleanupPendingJobs(q.evaluationCleanup["pending_jobs"])
	if !valid {
		return nil
	}
	return pending
}

func (q *continuationDurableQueue) pendingEvaluationCleanupForJobLocked(job *continuationDurableJob) (continuationEvaluationCleanupPendingJob, bool, error) {
	if q == nil || job == nil {
		return continuationEvaluationCleanupPendingJob{}, false, nil
	}
	wantID := strings.TrimSpace(job.ID)
	wantRef := continuationEvaluationCleanupJobRef(job)
	for _, pending := range q.evaluationCleanupPendingJobsLocked() {
		if pending.JobID != wantID && !strings.EqualFold(pending.Ref, wantRef) {
			continue
		}
		if pending.JobID != wantID || !strings.EqualFold(pending.Ref, wantRef) {
			return continuationEvaluationCleanupPendingJob{}, true, errors.New("evaluation cleanup pending custody identity conflict")
		}
		return pending, true, nil
	}
	return continuationEvaluationCleanupPendingJob{}, false, nil
}

func (q *continuationDurableQueue) pendingEvaluationCleanupByIDLocked(jobID string) (continuationEvaluationCleanupPendingJob, bool) {
	jobID = strings.TrimSpace(jobID)
	for _, pending := range q.evaluationCleanupPendingJobsLocked() {
		if pending.JobID == jobID {
			return pending, true
		}
	}
	return continuationEvaluationCleanupPendingJob{}, false
}

func (q *continuationDurableQueue) preservePendingEvaluationCleanupEntryLocked(name string, attempted map[string]bool) bool {
	pending, exists := q.pendingEvaluationCleanupByIDLocked(strings.TrimSuffix(strings.TrimSpace(name), ".json"))
	if exists && attempted != nil {
		attempted[pending.Ref] = true
	}
	return exists
}

func continuationEvaluationCleanupRefs(existing []string, additions []string) []string {
	seen := map[string]struct{}{}
	refs := make([]string, 0, len(existing)+len(additions))
	for _, values := range [][]string{existing, additions} {
		for _, ref := range values {
			ref = strings.TrimSpace(ref)
			if ref == "" {
				continue
			}
			if _, exists := seen[ref]; exists {
				continue
			}
			seen[ref] = struct{}{}
			refs = append(refs, ref)
		}
	}
	if len(refs) > continuationEvaluationCleanupMaxPending {
		// The durable content-addressed index retains every identity. This
		// receipt field is only a bounded hot replay window, so retain the most
		// recently added identities instead of silently pinning the first page.
		refs = refs[len(refs)-continuationEvaluationCleanupMaxPending:]
	}
	sort.Strings(refs)
	return refs
}

func continuationEvaluationCleanupReceiptCurrentFormat(receipt map[string]any) bool {
	if len(receipt) == 0 {
		return false
	}
	return strings.TrimSpace(anyToString(receipt["cleanup_state"])) != "" || len(anyToStringSlice(receipt["completed_job_refs"])) > 0
}

func continuationEvaluationCleanupStrictInt(value any) (int, bool) {
	var parsed int64
	switch typed := value.(type) {
	case int:
		return typed, true
	case int8:
		return int(typed), true
	case int16:
		return int(typed), true
	case int32:
		return int(typed), true
	case int64:
		parsed = typed
	case uint:
		if uint64(typed) > uint64(^uint(0)>>1) {
			return 0, false
		}
		return int(typed), true
	case uint8:
		return int(typed), true
	case uint16:
		return int(typed), true
	case uint32:
		if uint64(typed) > uint64(^uint(0)>>1) {
			return 0, false
		}
		return int(typed), true
	case uint64:
		if typed > uint64(^uint(0)>>1) {
			return 0, false
		}
		return int(typed), true
	case json.Number:
		value, err := strconv.ParseInt(string(typed), 10, 64)
		if err != nil {
			return 0, false
		}
		parsed = value
	default:
		return 0, false
	}
	maxInt := int64(^uint(0) >> 1)
	minInt := -maxInt - 1
	if parsed > maxInt || parsed < minInt {
		return 0, false
	}
	return int(parsed), true
}

func continuationEvaluationCleanupStrictInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int8:
		return int64(typed), true
	case int16:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case uint:
		if uint64(typed) > uint64(^uint64(0)>>1) {
			return 0, false
		}
		return int64(typed), true
	case uint8:
		return int64(typed), true
	case uint16:
		return int64(typed), true
	case uint32:
		return int64(typed), true
	case uint64:
		if typed > uint64(^uint64(0)>>1) {
			return 0, false
		}
		return int64(typed), true
	case json.Number:
		parsed, err := strconv.ParseInt(string(typed), 10, 64)
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func continuationEvaluationCleanupStrictString(value any) (string, bool) {
	typed, ok := value.(string)
	if !ok || typed == "" || strings.TrimSpace(typed) != typed {
		return "", false
	}
	return typed, true
}

func continuationEvaluationCleanupStrictBool(value any) (bool, bool) {
	typed, ok := value.(bool)
	return typed, ok
}

func continuationEvaluationCleanupStrictRefs(value any) ([]string, bool) {
	var raw []any
	switch typed := value.(type) {
	case []any:
		raw = typed
	case []string:
		raw = make([]any, len(typed))
		for index := range typed {
			raw[index] = typed[index]
		}
	default:
		return nil, false
	}
	refs := make([]string, 0, len(raw))
	seen := map[string]struct{}{}
	for _, item := range raw {
		ref, ok := continuationEvaluationCleanupStrictString(item)
		if !ok || len(ref) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(ref, "sha256:") {
			return nil, false
		}
		decoded, err := hex.DecodeString(ref[len("sha256:"):])
		if err != nil || len(decoded) != sha256.Size || strings.ToLower(ref[len("sha256:"):]) != ref[len("sha256:"):] {
			return nil, false
		}
		if _, exists := seen[ref]; exists {
			return nil, false
		}
		seen[ref] = struct{}{}
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs, true
}

func continuationEvaluationCleanupLegacyAggregateRefs(receipt map[string]any) ([]string, bool) {
	if len(receipt) == 0 {
		return nil, false
	}
	// Any hybrid/current custody fields make the old aggregate ambiguous. The
	// caller must migrate every referenced row as uncounted rather than infer
	// completion from a partial aggregate.
	for _, field := range []string{"cleanup_state", "completed_job_refs", "pending_jobs"} {
		if _, present := receipt[field]; present {
			return nil, false
		}
	}
	if schema, ok := continuationEvaluationCleanupStrictString(receipt["schema_id"]); !ok || schema != continuationEvaluationCleanupSchemaID {
		return nil, false
	}
	if version, ok := continuationEvaluationCleanupStrictInt(receipt["version"]); !ok || version != continuationEvaluationCleanupVersion {
		return nil, false
	}
	for field, expected := range map[string]string{
		"authority": "gateway-go", "action": "remove_stale_evaluation_continuation", "traffic_class": "evaluation_holdout",
	} {
		value, ok := continuationEvaluationCleanupStrictString(receipt[field])
		if !ok || value != expected {
			return nil, false
		}
	}
	if phase, ok := continuationEvaluationCleanupStrictString(receipt["phase"]); !ok || phase == "" {
		return nil, false
	}
	if captured, ok := continuationEvaluationCleanupStrictString(receipt["captured_at"]); !ok || captured == "" {
		return nil, false
	}
	if suppressed, ok := continuationEvaluationCleanupStrictBool(receipt["side_effects_suppressed"]); !ok || !suppressed {
		return nil, false
	}
	truncated, ok := continuationEvaluationCleanupStrictBool(receipt["job_refs_truncated"])
	if !ok || truncated {
		return nil, false
	}
	refs, ok := continuationEvaluationCleanupStrictRefs(receipt["job_refs"])
	if !ok {
		return nil, false
	}
	detected, detectedOK := continuationEvaluationCleanupStrictInt(receipt["detected_jobs"])
	removed, removedOK := continuationEvaluationCleanupStrictInt(receipt["removed_jobs"])
	failures, failuresOK := continuationEvaluationCleanupStrictInt(receipt["removal_failures"])
	cumulative, cumulativeOK := continuationEvaluationCleanupStrictInt(receipt["cumulative_removed_jobs"])
	if !detectedOK || !removedOK || !failuresOK || !cumulativeOK || detected < 0 || removed < 0 || failures != 0 || cumulative < 0 || detected != removed || detected != len(refs) {
		return nil, false
	}
	if digest, ok := continuationEvaluationCleanupStrictString(receipt["digest"]); !ok || digest != continuationEvaluationCleanupDigest(receipt) {
		return nil, false
	}
	return refs, true
}

// continuationEvaluationCleanupRecordedRefs returns identities that the
// receipt itself can prove completed. Legacy receipts have no per-job state;
// their job_refs are trusted only when a complete, strictly typed aggregate
// proves every listed removal. A current receipt's bounded history and current
// job_refs are already the result of the two-phase cleanup protocol.
func continuationEvaluationCleanupRecordedRefs(receipt map[string]any) []string {
	if len(receipt) == 0 {
		return nil
	}
	refs := anyToStringSlice(receipt["completed_job_refs"])
	if continuationEvaluationCleanupReceiptCurrentFormat(receipt) {
		refs = append(refs, anyToStringSlice(receipt["job_refs"])...)
	} else if legacyRefs, valid := continuationEvaluationCleanupLegacyAggregateRefs(receipt); valid {
		refs = append(refs, legacyRefs...)
	}
	return continuationEvaluationCleanupRefs(refs, nil)
}

func (q *continuationDurableQueue) evaluationCleanupReceiptBaseLocked(phase string) map[string]any {
	receipt := cloneAnyMap(q.evaluationCleanup)
	delete(receipt, "digest")
	receipt["schema_id"] = continuationEvaluationCleanupSchemaID
	receipt["version"] = continuationEvaluationCleanupVersion
	receipt["authority"] = "gateway-go"
	receipt["action"] = "remove_stale_evaluation_continuation"
	if strings.TrimSpace(phase) != "" {
		receipt["phase"] = strings.TrimSpace(phase)
	}
	receipt["traffic_class"] = "evaluation_holdout"
	receipt["side_effects_suppressed"] = true
	receipt["captured_at"] = nowUTCISO()
	return receipt
}

func (q *continuationDurableQueue) writeEvaluationCleanupReceiptLocked(receipt map[string]any) error {
	if q == nil {
		return errors.New("nil continuation durable queue")
	}
	receipt = cloneAnyMap(receipt)
	delete(receipt, "digest")
	receipt["digest"] = continuationEvaluationCleanupDigest(receipt)
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if int64(len(raw)) > q.receiptByteLimit() {
		err := fmt.Errorf("evaluation cleanup receipt exceeds bounded durable size %d", q.receiptByteLimit())
		q.lastError = err.Error()
		return err
	}
	writer := q.evaluationCleanupWriter
	if writer == nil {
		writer = writeOwnerOnlyDurableAtomicFile
	}
	if err := writer(filepath.Join(q.dir, continuationEvaluationCleanupReceiptFile), append(raw, '\n'), true); err != nil {
		q.lastError = err.Error()
		return err
	}
	q.evaluationCleanup = receipt
	q.evaluationCleanupTotal = anyToInt(receipt["cumulative_removed_jobs"], q.evaluationCleanupTotal)
	q.lastDrainAt = nowUTCISO()
	return nil
}

func (q *continuationDurableQueue) loadEvaluationCleanupReceiptLocked(path string) error {
	if err := ensureOwnerOnlyFile(path); err != nil {
		return err
	}
	raw, err := readContinuationDurableFileBounded(path, q.receiptByteLimit())
	if err != nil {
		return err
	}
	return q.loadEvaluationCleanupReceiptBytesLocked(raw)
}

func (q *continuationDurableQueue) loadEvaluationCleanupReceiptBytesLocked(raw []byte) error {
	receipt := map[string]any{}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&receipt); err != nil {
		return err
	}
	if anyToString(receipt["schema_id"]) != continuationEvaluationCleanupSchemaID || anyToInt(receipt["version"], 0) != continuationEvaluationCleanupVersion || anyToString(receipt["digest"]) != continuationEvaluationCleanupDigest(receipt) || len(anyToStringSlice(receipt["job_refs"])) > continuationEvaluationCleanupMaxJobRefs || len(anyToStringSlice(receipt["completed_job_refs"])) > continuationEvaluationCleanupMaxPending {
		return errors.New("evaluation continuation cleanup receipt is invalid")
	}
	if pending, valid := continuationEvaluationCleanupPendingJobs(receipt["pending_jobs"]); !valid || len(pending) > continuationEvaluationCleanupMaxPending {
		return errors.New("evaluation continuation cleanup receipt pending custody is invalid")
	}
	q.evaluationCleanup = cloneAnyMap(receipt)
	q.evaluationCleanupTotal = anyToInt(receipt["cumulative_removed_jobs"], 0)
	return nil
}

func continuationEvaluationCleanupJobRef(job *continuationDurableJob) string {
	if job == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(job.ID) + "|" + strings.TrimSpace(job.Source)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func continuationEvaluationCleanupMarkerKey(ref string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(strings.ToLower(ref))))
	return hex.EncodeToString(sum[:])
}

func continuationEvaluationCleanupMarkerComponents(ref string) (string, string, string, bool) {
	ref = strings.TrimSpace(strings.ToLower(ref))
	if ref == "" {
		return "", "", "", false
	}
	key := continuationEvaluationCleanupMarkerKey(ref)
	if len(key) < 2 {
		return "", "", "", false
	}
	return continuationEvaluationCleanupIndexDirectory, key[:2], key + ".json", true
}

func (q *continuationDurableQueue) evaluationCleanupMarkerPathForRefLocked(ref string) string {
	if q == nil || strings.TrimSpace(q.dir) == "" || strings.TrimSpace(ref) == "" {
		return ""
	}
	index, shard, marker, ok := continuationEvaluationCleanupMarkerComponents(ref)
	if !ok {
		return ""
	}
	return filepath.Join(q.dir, index, shard, marker)
}

func (q *continuationDurableQueue) evaluationCleanupMarkerPathLocked(job *continuationDurableJob) string {
	if job == nil {
		return ""
	}
	return q.evaluationCleanupMarkerPathForRefLocked(continuationEvaluationCleanupJobRef(job))
}

func continuationEvaluationCleanupDecodeMarker(raw []byte) (map[string]any, error) {
	marker := map[string]any{}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&marker); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("evaluation cleanup marker has trailing values")
		}
		return nil, err
	}
	return marker, nil
}

func continuationEvaluationCleanupValidateMarker(marker map[string]any, ref, jobID, source string) (int, error) {
	ref = strings.TrimSpace(strings.ToLower(ref))
	if len(marker) == 0 || ref == "" {
		return 0, errors.New("evaluation cleanup marker identity is missing")
	}
	schema, schemaOK := continuationEvaluationCleanupStrictString(marker["schema_id"])
	version, versionOK := continuationEvaluationCleanupStrictInt(marker["version"])
	authority, authorityOK := continuationEvaluationCleanupStrictString(marker["authority"])
	action, actionOK := continuationEvaluationCleanupStrictString(marker["action"])
	markerRef, refOK := continuationEvaluationCleanupStrictString(marker["job_ref"])
	digest, digestOK := continuationEvaluationCleanupStrictString(marker["digest"])
	if !schemaOK || schema != continuationEvaluationCleanupMarkerSchemaID || !versionOK || version != continuationEvaluationCleanupMarkerVersion || !authorityOK || authority != "gateway-go" || !actionOK || action != continuationEvaluationCleanupMarkerAction || !refOK || !strings.EqualFold(markerRef, ref) || !digestOK || digest != continuationEvaluationCleanupDigest(marker) {
		return 0, errors.New("evaluation cleanup marker is invalid")
	}
	if captured, ok := continuationEvaluationCleanupStrictString(marker["captured_at"]); !ok || captured == "" {
		return 0, errors.New("evaluation cleanup marker timestamp is invalid")
	}
	if markerID, present := marker["job_id"]; present {
		id, ok := continuationEvaluationCleanupStrictString(markerID)
		if !ok || (strings.TrimSpace(jobID) != "" && id != strings.TrimSpace(jobID)) {
			return 0, errors.New("evaluation cleanup marker job identity conflicts")
		}
	}
	if markerSource, present := marker["source"]; present {
		value, ok := continuationEvaluationCleanupStrictString(markerSource)
		if !ok || (strings.TrimSpace(source) != "" && !strings.EqualFold(value, strings.TrimSpace(source))) {
			return 0, errors.New("evaluation cleanup marker source identity conflicts")
		}
	}
	removed, ok := continuationEvaluationCleanupStrictInt(marker["removed_jobs"])
	if !ok || removed < 0 || removed > 1 {
		return 0, errors.New("evaluation cleanup marker removal count is invalid")
	}
	return removed, nil
}

func (q *continuationDurableQueue) loadEvaluationCleanupMarkerLocked(job *continuationDurableJob) (bool, int, error) {
	if q == nil || job == nil {
		return false, 0, nil
	}
	ref := strings.TrimSpace(strings.ToLower(continuationEvaluationCleanupJobRef(job)))
	index, shard, marker, ok := continuationEvaluationCleanupMarkerComponents(ref)
	if !ok || strings.TrimSpace(q.dir) == "" {
		return false, 0, errors.New("evaluation cleanup marker identity is missing")
	}
	raw, err := readEvaluationCleanupMarkerFileBounded(q.dir, index, shard, marker, continuationEvaluationCleanupMarkerMaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	markerValue, err := continuationEvaluationCleanupDecodeMarker(raw)
	if err != nil {
		return false, 0, err
	}
	removed, err := continuationEvaluationCleanupValidateMarker(markerValue, ref, job.ID, job.Source)
	if err != nil {
		return false, 0, err
	}
	return true, removed, nil
}

func (q *continuationDurableQueue) evaluationCleanupMarkerCountLimitLocked() int {
	if q != nil && q.evaluationCleanupMarkerMaxCount > 0 {
		return q.evaluationCleanupMarkerMaxCount
	}
	return continuationEvaluationCleanupMarkerIndexMaxCount
}

func (q *continuationDurableQueue) evaluationCleanupMarkerByteLimitLocked() int64 {
	if q != nil && q.evaluationCleanupMarkerMaxBytes > 0 {
		return q.evaluationCleanupMarkerMaxBytes
	}
	return continuationEvaluationCleanupMarkerIndexMaxBytes
}

func continuationEvaluationCleanupMarkerIndexDecode(raw []byte) (map[string]any, error) {
	manifest := map[string]any{}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("evaluation cleanup marker index has trailing values")
		}
		return nil, err
	}
	return manifest, nil
}

func (q *continuationDurableQueue) evaluationCleanupMarkerIndexManifestLocked(state, pendingRef string, pendingBytes int64) map[string]any {
	if q == nil {
		return nil
	}
	if strings.TrimSpace(state) == "" {
		state = continuationEvaluationCleanupMarkerIndexStateReady
	}
	manifest := map[string]any{
		"schema_id":        continuationEvaluationCleanupMarkerIndexSchemaID,
		"version":          continuationEvaluationCleanupMarkerIndexVersion,
		"authority":        "gateway-go",
		"action":           continuationEvaluationCleanupMarkerIndexAction,
		"state":            state,
		"marker_count":     q.evaluationCleanupMarkerCount,
		"marker_bytes":     q.evaluationCleanupMarkerBytes,
		"max_marker_count": q.evaluationCleanupMarkerCountLimitLocked(),
		"max_marker_bytes": q.evaluationCleanupMarkerByteLimitLocked(),
		"retention":        "never_delete_automatically",
		"updated_at":       nowUTCISO(),
	}
	if strings.TrimSpace(pendingRef) != "" {
		manifest["pending_ref"] = strings.TrimSpace(strings.ToLower(pendingRef))
		manifest["pending_bytes"] = pendingBytes
	}
	if strings.TrimSpace(q.evaluationCleanupMarkerLimitReason) != "" {
		manifest["limit_reason"] = q.evaluationCleanupMarkerLimitReason
	}
	if strings.TrimSpace(q.evaluationCleanupMarkerMigrationState) != "" {
		manifest["cap_migration_state"] = q.evaluationCleanupMarkerMigrationState
		manifest["cap_migration_plan_digest"] = q.evaluationCleanupMarkerMigrationPlanDigest
		manifest["cap_migration_receipt_digest"] = q.evaluationCleanupMarkerMigrationReceiptDigest
		manifest["cap_migration_generation"] = q.evaluationCleanupMarkerMigrationGeneration
		manifest["cap_migration_target_generation"] = q.evaluationCleanupMarkerMigrationTargetGeneration
	}
	manifest["digest"] = continuationEvaluationCleanupDigest(manifest)
	return manifest
}

func (q *continuationDurableQueue) writeEvaluationCleanupMarkerIndexLocked(state, pendingRef string, pendingBytes int64) error {
	if q == nil || strings.TrimSpace(q.dir) == "" {
		return errors.New("evaluation cleanup marker index root is missing")
	}
	manifest := q.evaluationCleanupMarkerIndexManifestLocked(state, pendingRef, pendingBytes)
	raw, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if int64(len(raw)) > continuationEvaluationCleanupMarkerMaxBytes {
		return errors.New("evaluation cleanup marker index exceeds bounded durable size")
	}
	if err := q.ensureEvaluationCleanupMarkerDirectoriesLocked(filepath.Join(q.dir, continuationEvaluationCleanupIndexDirectory)); err != nil {
		return err
	}
	writer := q.evaluationCleanupMarkerWriter
	if writer == nil {
		writer = writeEvaluationCleanupMarkerDurable
	}
	if err := writer(q.dir, continuationEvaluationCleanupIndexDirectory, "", continuationEvaluationCleanupMarkerIndexFile, append(raw, '\n')); err != nil {
		return err
	}
	return nil
}

func (q *continuationDurableQueue) setEvaluationCleanupMarkerIndexUnavailableLocked(reason string) error {
	q.evaluationCleanupMarkerState = continuationEvaluationCleanupMarkerIndexStateUnavailable
	q.evaluationCleanupMarkerLimitReason = strings.TrimSpace(reason)
	if q.evaluationCleanupMarkerLimitReason == "" {
		q.evaluationCleanupMarkerLimitReason = "marker index reconciliation failed"
	}
	return q.writeEvaluationCleanupMarkerIndexLocked(q.evaluationCleanupMarkerState, "", 0)
}

func (q *continuationDurableQueue) setEvaluationCleanupMarkerIndexLimitLocked(reason string) error {
	q.evaluationCleanupMarkerState = continuationEvaluationCleanupMarkerIndexStateLimit
	q.evaluationCleanupMarkerLimitReason = strings.TrimSpace(reason)
	if q.evaluationCleanupMarkerLimitReason == "" {
		q.evaluationCleanupMarkerLimitReason = "marker index hard limit reached"
	}
	return q.writeEvaluationCleanupMarkerIndexLocked(q.evaluationCleanupMarkerState, "", 0)
}

func (q *continuationDurableQueue) rebuildEvaluationCleanupMarkerIndexLocked(indexPath string, expectedIndex os.FileInfo) error {
	_ = indexPath
	countLimit := q.evaluationCleanupMarkerCountLimitLocked()
	byteLimit := q.evaluationCleanupMarkerByteLimitLocked()
	entries, overflow, err := readEvaluationCleanupMarkerTreeBounded(q.dir, continuationEvaluationCleanupIndexDirectory, 256+1, countLimit+1, continuationEvaluationCleanupMarkerMaxBytes, expectedIndex, q.evaluationCleanupMarkerRebuildHook)
	if err != nil {
		return err
	}
	if overflow {
		return errors.New("evaluation cleanup marker index has too many shard entries")
	}
	count := 0
	var bytesRead int64
	seen := map[string]struct{}{}
	for _, shardEntry := range entries {
		name := strings.TrimSpace(shardEntry.name)
		if evaluationCleanupMarkerIndexControlEntry(name) || evaluationCleanupMarkerTemporaryEntry(name) {
			continue
		}
		if len(name) != 2 || strings.Trim(name, "0123456789abcdefABCDEF") != "" || !shardEntry.isDir {
			return fmt.Errorf("evaluation cleanup marker index entry %q is invalid", name)
		}
		if shardEntry.overflow {
			return errors.New("evaluation cleanup marker shard exceeds hard marker count")
		}
		for _, markerEntry := range shardEntry.entries {
			markerName := strings.TrimSpace(markerEntry.name)
			if evaluationCleanupMarkerTemporaryEntry(markerName) {
				continue
			}
			if markerEntry.isDir || markerName == "" {
				return fmt.Errorf("evaluation cleanup marker entry %q is invalid", markerName)
			}
			raw := markerEntry.raw
			marker, decodeErr := continuationEvaluationCleanupDecodeMarker(raw)
			if decodeErr != nil {
				return decodeErr
			}
			ref, refOK := continuationEvaluationCleanupStrictString(marker["job_ref"])
			if !refOK {
				return errors.New("evaluation cleanup marker index contains an unbound marker")
			}
			ref = strings.ToLower(strings.TrimSpace(ref))
			if _, exists := seen[ref]; exists {
				return errors.New("evaluation cleanup marker index contains duplicate identity")
			}
			expectedIndex, expectedShard, expectedName, componentsOK := continuationEvaluationCleanupMarkerComponents(ref)
			if !componentsOK || expectedIndex != continuationEvaluationCleanupIndexDirectory || expectedShard != name || expectedName != markerName {
				return errors.New("evaluation cleanup marker path is not bound to its identity")
			}
			if _, validateErr := continuationEvaluationCleanupValidateMarker(marker, ref, "", ""); validateErr != nil {
				return validateErr
			}
			seen[ref] = struct{}{}
			count++
			bytesRead += int64(len(raw))
			if count > countLimit || bytesRead > byteLimit {
				q.evaluationCleanupMarkerCount = count
				q.evaluationCleanupMarkerBytes = bytesRead
				q.evaluationCleanupMarkerState = continuationEvaluationCleanupMarkerIndexStateLimit
				q.evaluationCleanupMarkerLimitReason = "marker index hard limit reached during reconciliation"
				_ = q.writeEvaluationCleanupMarkerIndexLocked(q.evaluationCleanupMarkerState, "", 0)
				return errors.New(q.evaluationCleanupMarkerLimitReason)
			}
		}
	}
	q.evaluationCleanupMarkerCount = count
	q.evaluationCleanupMarkerBytes = bytesRead
	q.evaluationCleanupMarkerState = continuationEvaluationCleanupMarkerIndexStateReady
	q.evaluationCleanupMarkerLimitReason = ""
	q.evaluationCleanupMarkerPendingRef = ""
	q.evaluationCleanupMarkerPendingBytes = 0
	return q.writeEvaluationCleanupMarkerIndexLocked(q.evaluationCleanupMarkerState, "", 0)
}

func (q *continuationDurableQueue) reconcileEvaluationCleanupMarkerIndexPendingLocked() error {
	ref := strings.TrimSpace(strings.ToLower(q.evaluationCleanupMarkerPendingRef))
	if ref == "" {
		q.evaluationCleanupMarkerState = continuationEvaluationCleanupMarkerIndexStateReady
		q.evaluationCleanupMarkerPendingBytes = 0
		return q.writeEvaluationCleanupMarkerIndexLocked(q.evaluationCleanupMarkerState, "", 0)
	}
	index, shard, marker, ok := continuationEvaluationCleanupMarkerComponents(ref)
	if !ok {
		return errors.New("evaluation cleanup marker index pending identity is invalid")
	}
	raw, err := readEvaluationCleanupMarkerFileBounded(q.dir, index, shard, marker, continuationEvaluationCleanupMarkerMaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		q.evaluationCleanupMarkerState = continuationEvaluationCleanupMarkerIndexStateReady
		q.evaluationCleanupMarkerPendingRef = ""
		q.evaluationCleanupMarkerPendingBytes = 0
		return q.writeEvaluationCleanupMarkerIndexLocked(q.evaluationCleanupMarkerState, "", 0)
	}
	if err != nil {
		q.evaluationCleanupMarkerState = continuationEvaluationCleanupMarkerIndexStateUnavailable
		q.evaluationCleanupMarkerLimitReason = err.Error()
		return err
	}
	markerValue, decodeErr := continuationEvaluationCleanupDecodeMarker(raw)
	if decodeErr != nil {
		return decodeErr
	}
	if _, validateErr := continuationEvaluationCleanupValidateMarker(markerValue, ref, "", ""); validateErr != nil {
		return validateErr
	}
	if q.evaluationCleanupMarkerPendingBytes > 0 && q.evaluationCleanupMarkerPendingBytes != int64(len(raw)) {
		return errors.New("evaluation cleanup marker pending byte accounting conflicts")
	}
	if q.evaluationCleanupMarkerCount >= q.evaluationCleanupMarkerCountLimitLocked() || q.evaluationCleanupMarkerBytes+int64(len(raw)) > q.evaluationCleanupMarkerByteLimitLocked() {
		q.evaluationCleanupMarkerState = continuationEvaluationCleanupMarkerIndexStateLimit
		q.evaluationCleanupMarkerLimitReason = "marker index hard limit reached while reconciling pending marker"
		return errors.New(q.evaluationCleanupMarkerLimitReason)
	}
	q.evaluationCleanupMarkerCount++
	q.evaluationCleanupMarkerBytes += int64(len(raw))
	q.evaluationCleanupMarkerState = continuationEvaluationCleanupMarkerIndexStateReady
	q.evaluationCleanupMarkerPendingRef = ""
	q.evaluationCleanupMarkerPendingBytes = 0
	q.evaluationCleanupMarkerLimitReason = ""
	return q.writeEvaluationCleanupMarkerIndexLocked(q.evaluationCleanupMarkerState, "", 0)
}

func (q *continuationDurableQueue) loadEvaluationCleanupMarkerIndexLocked() (resultErr error) {
	if q == nil || strings.TrimSpace(q.dir) == "" {
		return errors.New("evaluation cleanup marker index root is missing")
	}
	// q.mu is always acquired before this process-shared owner lock. Extend,
	// rollback, and restart reconciliation therefore use one lock order and
	// cannot deadlock a second queue handle while the durable pointer is being
	// re-read or published.
	unlockOwner, lockErr := acquireEvaluationCleanupMarkerMigrationOwnerLock(q.dir)
	if lockErr != nil {
		return lockErr
	}
	defer unlockOwner()
	defer func() {
		if resultErr == nil || q.evaluationCleanupMarkerState == continuationEvaluationCleanupMarkerIndexStateLimit || q.evaluationCleanupMarkerState == continuationEvaluationCleanupMarkerIndexStateUnavailable {
			return
		}
		q.evaluationCleanupMarkerState = continuationEvaluationCleanupMarkerIndexStateUnavailable
		q.evaluationCleanupMarkerLimitReason = resultErr.Error()
		_ = q.writeEvaluationCleanupMarkerIndexLocked(q.evaluationCleanupMarkerState, "", 0)
	}()
	if q.evaluationCleanupMarkerState == "" {
		q.evaluationCleanupMarkerState = continuationEvaluationCleanupMarkerIndexStateReady
	}
	indexPath := filepath.Join(q.dir, continuationEvaluationCleanupIndexDirectory)
	indexInfo, err := os.Lstat(indexPath)
	if errors.Is(err, os.ErrNotExist) {
		q.evaluationCleanupMarkerCount = 0
		q.evaluationCleanupMarkerBytes = 0
		q.evaluationCleanupMarkerState = continuationEvaluationCleanupMarkerIndexStateReady
		q.evaluationCleanupMarkerLimitReason = ""
		q.evaluationCleanupMarkerPendingRef = ""
		q.evaluationCleanupMarkerPendingBytes = 0
		return nil
	}
	if err != nil {
		return err
	}
	if indexInfo.Mode()&os.ModeSymlink != 0 || !indexInfo.IsDir() {
		q.evaluationCleanupMarkerState = continuationEvaluationCleanupMarkerIndexStateUnavailable
		q.evaluationCleanupMarkerLimitReason = "marker index root is not a real directory"
		return errors.New(q.evaluationCleanupMarkerLimitReason)
	}
	if err := q.reconcileEvaluationCleanupMarkerMigrationLocked(); err != nil {
		q.evaluationCleanupMarkerState = continuationEvaluationCleanupMarkerIndexStateUnavailable
		q.evaluationCleanupMarkerLimitReason = err.Error()
		return err
	}
	raw, readErr := readEvaluationCleanupMarkerFileBoundedWithExpectedIndex(q.dir, continuationEvaluationCleanupIndexDirectory, "", continuationEvaluationCleanupMarkerIndexFile, continuationEvaluationCleanupMarkerMaxBytes, indexInfo)
	if errors.Is(readErr, os.ErrNotExist) {
		return q.rebuildEvaluationCleanupMarkerIndexLocked(indexPath, indexInfo)
	}
	if readErr != nil {
		q.evaluationCleanupMarkerState = continuationEvaluationCleanupMarkerIndexStateUnavailable
		q.evaluationCleanupMarkerLimitReason = readErr.Error()
		return readErr
	}
	manifest, decodeErr := continuationEvaluationCleanupMarkerIndexDecode(raw)
	if decodeErr != nil {
		q.evaluationCleanupMarkerState = continuationEvaluationCleanupMarkerIndexStateUnavailable
		q.evaluationCleanupMarkerLimitReason = decodeErr.Error()
		return decodeErr
	}
	if schema, ok := continuationEvaluationCleanupStrictString(manifest["schema_id"]); !ok || schema != continuationEvaluationCleanupMarkerIndexSchemaID {
		return errors.New("evaluation cleanup marker index schema is invalid")
	}
	if version, ok := continuationEvaluationCleanupStrictInt(manifest["version"]); !ok || version != continuationEvaluationCleanupMarkerIndexVersion {
		return errors.New("evaluation cleanup marker index version is invalid")
	}
	if authority, ok := continuationEvaluationCleanupStrictString(manifest["authority"]); !ok || authority != "gateway-go" {
		return errors.New("evaluation cleanup marker index authority is invalid")
	}
	if action, ok := continuationEvaluationCleanupStrictString(manifest["action"]); !ok || action != continuationEvaluationCleanupMarkerIndexAction {
		return errors.New("evaluation cleanup marker index action is invalid")
	}
	if digest, ok := continuationEvaluationCleanupStrictString(manifest["digest"]); !ok || digest != continuationEvaluationCleanupDigest(manifest) {
		return errors.New("evaluation cleanup marker index digest is invalid")
	}
	state, stateOK := continuationEvaluationCleanupStrictString(manifest["state"])
	count, countOK := continuationEvaluationCleanupStrictInt(manifest["marker_count"])
	bytesCount, bytesOK := continuationEvaluationCleanupStrictInt(manifest["marker_bytes"])
	maxCount, maxCountOK := continuationEvaluationCleanupStrictInt(manifest["max_marker_count"])
	maxBytes, maxBytesOK := continuationEvaluationCleanupStrictInt(manifest["max_marker_bytes"])
	manifestMaxCount := maxCount
	manifestMaxBytes := maxBytes
	if maxCountOK && maxBytesOK && (manifestMaxCount <= 0 || manifestMaxCount > continuationEvaluationCleanupMarkerIndexAbsoluteMaxCount || manifestMaxBytes <= 0 || int64(manifestMaxBytes) > continuationEvaluationCleanupMarkerIndexAbsoluteMaxBytes) {
		return errors.New("evaluation cleanup marker index cap is outside the absolute bound")
	}
	// A durable commit/rollback receipt is authoritative if process death
	// happened before its manifest publication. Reconcile the effective caps
	// from that receipt, then rewrite the manifest after accounting validates.
	if q.evaluationCleanupMarkerMigrationState == continuationEvaluationCleanupMarkerMigrationStateCommitted || q.evaluationCleanupMarkerMigrationState == continuationEvaluationCleanupMarkerMigrationStateRolledBack {
		maxCount = q.evaluationCleanupMarkerMaxCount
		maxBytes = int(q.evaluationCleanupMarkerMaxBytes)
	}
	if !stateOK || !countOK || !bytesOK || !maxCountOK || !maxBytesOK || maxCount <= 0 || maxCount > continuationEvaluationCleanupMarkerIndexAbsoluteMaxCount || maxBytes <= 0 || int64(maxBytes) > continuationEvaluationCleanupMarkerIndexAbsoluteMaxBytes || count < 0 || bytesCount < 0 || count > maxCount || int64(bytesCount) > int64(maxBytes) {
		return errors.New("evaluation cleanup marker index accounting is invalid")
	}
	q.evaluationCleanupMarkerMaxCount = maxCount
	q.evaluationCleanupMarkerMaxBytes = int64(maxBytes)
	if state != continuationEvaluationCleanupMarkerIndexStateReady && state != continuationEvaluationCleanupMarkerIndexStatePending && state != continuationEvaluationCleanupMarkerIndexStateLimit && state != continuationEvaluationCleanupMarkerIndexStateUnavailable {
		return errors.New("evaluation cleanup marker index state is invalid")
	}
	if retention, ok := continuationEvaluationCleanupStrictString(manifest["retention"]); !ok || retention != "never_delete_automatically" {
		return errors.New("evaluation cleanup marker index retention policy is invalid")
	}
	if updatedAt, ok := continuationEvaluationCleanupStrictString(manifest["updated_at"]); !ok || updatedAt == "" {
		return errors.New("evaluation cleanup marker index timestamp is invalid")
	}
	limitReason := ""
	if rawReason, present := manifest["limit_reason"]; present {
		var reasonOK bool
		limitReason, reasonOK = continuationEvaluationCleanupStrictString(rawReason)
		if !reasonOK {
			return errors.New("evaluation cleanup marker index limit reason is invalid")
		}
	}
	if (state == continuationEvaluationCleanupMarkerIndexStateLimit || state == continuationEvaluationCleanupMarkerIndexStateUnavailable) && limitReason == "" {
		return errors.New("evaluation cleanup marker index terminal state lacks a reason")
	}
	pendingRef := ""
	pendingBytes := 0
	if state == continuationEvaluationCleanupMarkerIndexStatePending {
		var pendingRefOK, pendingBytesOK bool
		pendingRef, pendingRefOK = continuationEvaluationCleanupStrictString(manifest["pending_ref"])
		pendingBytes, pendingBytesOK = continuationEvaluationCleanupStrictInt(manifest["pending_bytes"])
		if !pendingRefOK || !pendingBytesOK || pendingBytes <= 0 || int64(pendingBytes) > q.evaluationCleanupMarkerByteLimitLocked() {
			return errors.New("evaluation cleanup marker index pending transaction is invalid")
		}
		pendingRef = strings.TrimSpace(strings.ToLower(pendingRef))
		if _, _, _, ok := continuationEvaluationCleanupMarkerComponents(pendingRef); !ok {
			return errors.New("evaluation cleanup marker index pending identity is invalid")
		}
	} else if _, present := manifest["pending_ref"]; present {
		return errors.New("evaluation cleanup marker index has stale pending identity")
	}
	q.evaluationCleanupMarkerCount = count
	q.evaluationCleanupMarkerBytes = int64(bytesCount)
	q.evaluationCleanupMarkerState = state
	q.evaluationCleanupMarkerLimitReason = strings.TrimSpace(limitReason)
	q.evaluationCleanupMarkerPendingRef = pendingRef
	q.evaluationCleanupMarkerPendingBytes = int64(pendingBytes)
	if state == continuationEvaluationCleanupMarkerIndexStatePending {
		if err := q.reconcileEvaluationCleanupMarkerIndexPendingLocked(); err != nil {
			return err
		}
		return nil
	}
	if q.evaluationCleanupMarkerMigrationState != "" {
		return q.writeEvaluationCleanupMarkerIndexLocked(q.evaluationCleanupMarkerState, q.evaluationCleanupMarkerPendingRef, q.evaluationCleanupMarkerPendingBytes)
	}
	return nil
}

func (q *continuationDurableQueue) evaluationCleanupMarkerPayloadLocked(ref, jobID, source string, removed int) ([]byte, error) {
	ref = strings.TrimSpace(strings.ToLower(ref))
	if ref == "" || removed < 0 || removed > 1 {
		return nil, errors.New("evaluation cleanup marker identity or count is invalid")
	}
	marker := map[string]any{
		"schema_id":    continuationEvaluationCleanupMarkerSchemaID,
		"version":      continuationEvaluationCleanupMarkerVersion,
		"authority":    "gateway-go",
		"action":       continuationEvaluationCleanupMarkerAction,
		"job_ref":      ref,
		"removed_jobs": removed,
		"captured_at":  nowUTCISO(),
	}
	if strings.TrimSpace(jobID) != "" {
		marker["job_id"] = strings.TrimSpace(jobID)
	}
	if strings.TrimSpace(source) != "" {
		marker["source"] = strings.TrimSpace(strings.ToLower(source))
	}
	marker["digest"] = continuationEvaluationCleanupDigest(marker)
	raw, err := json.Marshal(marker)
	if err != nil {
		return nil, err
	}
	raw = append(raw, '\n')
	if int64(len(raw)) > continuationEvaluationCleanupMarkerMaxBytes {
		return nil, errors.New("evaluation cleanup marker exceeds bounded durable size")
	}
	return raw, nil
}

func (q *continuationDurableQueue) evaluationCleanupMarkerExistingLocked(ref string) (bool, int, []byte, error) {
	ref = strings.TrimSpace(strings.ToLower(ref))
	index, shard, marker, ok := continuationEvaluationCleanupMarkerComponents(ref)
	if !ok || q == nil || strings.TrimSpace(q.dir) == "" {
		return false, 0, nil, errors.New("evaluation cleanup marker identity is missing")
	}
	raw, err := readEvaluationCleanupMarkerFileBounded(q.dir, index, shard, marker, continuationEvaluationCleanupMarkerMaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		return false, 0, nil, nil
	}
	if err != nil {
		return false, 0, nil, err
	}
	markerValue, decodeErr := continuationEvaluationCleanupDecodeMarker(raw)
	if decodeErr != nil {
		return false, 0, nil, decodeErr
	}
	removed, validateErr := continuationEvaluationCleanupValidateMarker(markerValue, ref, "", "")
	if validateErr != nil {
		return false, 0, nil, validateErr
	}
	return true, removed, raw, nil
}

func (q *continuationDurableQueue) ensureEvaluationCleanupMarkerCapacityLocked(ref, jobID, source string, removed int) ([]byte, error) {
	if q == nil {
		return nil, errors.New("nil continuation durable queue")
	}
	exists, _, _, err := q.evaluationCleanupMarkerExistingLocked(ref)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, nil
	}
	if q.evaluationCleanupMarkerState == continuationEvaluationCleanupMarkerIndexStatePending {
		if !strings.EqualFold(strings.TrimSpace(q.evaluationCleanupMarkerPendingRef), strings.TrimSpace(strings.ToLower(ref))) {
			return nil, errors.New("evaluation cleanup marker index has unresolved custody for another identity")
		}
		if err := q.reconcileEvaluationCleanupMarkerIndexPendingLocked(); err != nil {
			return nil, err
		}
	}
	if q.evaluationCleanupMarkerState != continuationEvaluationCleanupMarkerIndexStateReady {
		return nil, fmt.Errorf("evaluation cleanup marker index is %s: %s", q.evaluationCleanupMarkerState, q.evaluationCleanupMarkerLimitReason)
	}
	raw, err := q.evaluationCleanupMarkerPayloadLocked(ref, jobID, source, removed)
	if err != nil {
		return nil, err
	}
	if q.evaluationCleanupMarkerCount >= q.evaluationCleanupMarkerCountLimitLocked() || q.evaluationCleanupMarkerBytes+int64(len(raw)) > q.evaluationCleanupMarkerByteLimitLocked() {
		limitErr := errors.New("evaluation cleanup marker index hard limit reached; cleanup is fail-closed")
		q.evaluationCleanupMarkerState = continuationEvaluationCleanupMarkerIndexStateLimit
		q.evaluationCleanupMarkerLimitReason = limitErr.Error()
		if manifestErr := q.writeEvaluationCleanupMarkerIndexLocked(q.evaluationCleanupMarkerState, "", 0); manifestErr != nil {
			q.lastError = manifestErr.Error()
		}
		return nil, limitErr
	}
	return raw, nil
}

func (q *continuationDurableQueue) ensureEvaluationCleanupMarkerRefLocked(ref, jobID, source string, removed int) error {
	ref = strings.TrimSpace(strings.ToLower(ref))
	if q == nil || ref == "" || removed < 0 || removed > 1 {
		return errors.New("evaluation cleanup marker identity or count is invalid")
	}
	index, shard, markerName, ok := continuationEvaluationCleanupMarkerComponents(ref)
	if !ok || strings.TrimSpace(q.dir) == "" {
		return errors.New("evaluation cleanup marker path is missing")
	}
	if exists, existingRemoved, _, err := q.evaluationCleanupMarkerExistingLocked(ref); err != nil {
		return err
	} else if exists {
		if existingRemoved == removed || (existingRemoved == 1 && removed == 0) {
			if q.evaluationCleanupMarkerState == continuationEvaluationCleanupMarkerIndexStatePending {
				if !strings.EqualFold(strings.TrimSpace(q.evaluationCleanupMarkerPendingRef), ref) {
					return errors.New("evaluation cleanup marker index has unresolved custody for another identity")
				}
				if err := q.reconcileEvaluationCleanupMarkerIndexPendingLocked(); err != nil {
					return err
				}
			}
			return nil
		}
		return fmt.Errorf("evaluation cleanup marker removal count conflicts for %s: existing=%d requested=%d", ref, existingRemoved, removed)
	}
	raw, err := q.ensureEvaluationCleanupMarkerCapacityLocked(ref, jobID, source, removed)
	if err != nil {
		return err
	}
	if raw == nil {
		return nil
	}
	path := filepath.Join(q.dir, index, shard, markerName)
	q.evaluationCleanupMarkerPendingRef = ref
	q.evaluationCleanupMarkerPendingBytes = int64(len(raw))
	q.evaluationCleanupMarkerState = continuationEvaluationCleanupMarkerIndexStatePending
	if err := q.writeEvaluationCleanupMarkerIndexLocked(q.evaluationCleanupMarkerState, ref, int64(len(raw))); err != nil {
		q.lastError = err.Error()
		return err
	}
	if err := q.ensureEvaluationCleanupMarkerDirectoriesLocked(filepath.Dir(path)); err != nil {
		return err
	}
	writer := q.evaluationCleanupMarkerWriter
	if writer == nil {
		writer = writeEvaluationCleanupMarkerDurable
	}
	if err := writer(q.dir, index, shard, markerName, raw); err != nil {
		q.lastError = err.Error()
		return err
	}
	q.evaluationCleanupMarkerCount++
	q.evaluationCleanupMarkerBytes += int64(len(raw))
	q.evaluationCleanupMarkerState = continuationEvaluationCleanupMarkerIndexStateReady
	q.evaluationCleanupMarkerLimitReason = ""
	q.evaluationCleanupMarkerPendingRef = ""
	q.evaluationCleanupMarkerPendingBytes = 0
	if err := q.writeEvaluationCleanupMarkerIndexLocked(q.evaluationCleanupMarkerState, "", 0); err != nil {
		// The marker is already durable. Keep pending custody in memory and let
		// startup reconcile the marker against the pending manifest transaction.
		q.evaluationCleanupMarkerCount--
		q.evaluationCleanupMarkerBytes -= int64(len(raw))
		q.evaluationCleanupMarkerState = continuationEvaluationCleanupMarkerIndexStatePending
		q.evaluationCleanupMarkerPendingRef = ref
		q.evaluationCleanupMarkerPendingBytes = int64(len(raw))
		q.lastError = err.Error()
		return err
	}
	return nil
}

func (q *continuationDurableQueue) ensureEvaluationCleanupMarkerLocked(job *continuationDurableJob, removed int) error {
	if q == nil || job == nil {
		return errors.New("evaluation cleanup marker job is missing")
	}
	return q.ensureEvaluationCleanupMarkerRefLocked(
		continuationEvaluationCleanupJobRef(job), job.ID, job.Source, removed,
	)
}

func (q *continuationDurableQueue) evaluationCleanupMarkerRecordedRefLocked(job *continuationDurableJob) bool {
	if q == nil || job == nil {
		return false
	}
	if _, exists, err := q.pendingEvaluationCleanupForJobLocked(job); err != nil || exists {
		return false
	}
	exists, _, err := q.loadEvaluationCleanupMarkerLocked(job)
	return err == nil && exists
}

func (q *continuationDurableQueue) evaluationCleanupRecordedLocked(job *continuationDurableJob) bool {
	return q.evaluationCleanupCompletedRefLocked(job) && q.evaluationCleanupJobAuthoritativelyAbsentLocked(job)
}

func (q *continuationDurableQueue) evaluationCleanupCompletedRefLocked(job *continuationDurableJob) bool {
	if q == nil || job == nil {
		return false
	}
	want := continuationEvaluationCleanupJobRef(job)
	for _, ref := range continuationEvaluationCleanupRecordedRefs(q.evaluationCleanup) {
		if strings.EqualFold(strings.TrimSpace(ref), want) {
			return true
		}
	}
	if q.evaluationCleanupMarkerRecordedRefLocked(job) {
		return true
	}
	return false
}

func (q *continuationDurableQueue) evaluationCleanupJobAuthoritativelyAbsentLocked(job *continuationDurableJob) bool {
	if q == nil || job == nil || strings.TrimSpace(job.ID) == "" {
		return false
	}
	if _, exists := q.jobs[strings.TrimSpace(job.ID)]; exists {
		return false
	}
	_, err := os.Lstat(q.jobPath(job.ID))
	return os.IsNotExist(err)
}

func (q *continuationDurableQueue) recordEvaluationCleanupPendingLocked(pending continuationEvaluationCleanupPendingJob) error {
	if q == nil || pending.JobID == "" || pending.Source == "" || pending.Ref == "" {
		return errors.New("evaluation cleanup pending custody is incomplete")
	}
	// A reappeared row that was already counted still needs a durable marker
	// carrying the original removal proof. The later receipt path records the
	// current attempt as removed=0 without weakening that marker to zero.
	markerRemoved := 1
	// Reserve capacity before the durable job is unlinked. A full or
	// unavailable exact-once marker index must fail closed while the job and
	// pending receipt still retain custody.
	if _, err := q.ensureEvaluationCleanupMarkerCapacityLocked(pending.Ref, pending.JobID, pending.Source, markerRemoved); err != nil {
		q.lastError = err.Error()
		return err
	}
	existing := q.evaluationCleanupPendingJobsLocked()
	for _, item := range existing {
		if item.JobID != pending.JobID && !strings.EqualFold(item.Ref, pending.Ref) {
			continue
		}
		if item.JobID != pending.JobID || !strings.EqualFold(item.Ref, pending.Ref) {
			return errors.New("evaluation cleanup pending custody identity conflict")
		}
		return nil
	}
	if len(existing) >= continuationEvaluationCleanupMaxPending {
		return errors.New("evaluation cleanup pending custody journal is full")
	}
	existing = append(existing, pending)
	previousCompletedRefs := continuationEvaluationCleanupRecordedRefs(q.evaluationCleanup)
	receipt := q.evaluationCleanupReceiptBaseLocked(pending.Phase)
	receipt["cleanup_state"] = continuationEvaluationCleanupStatePending
	receipt["pending_jobs"] = continuationEvaluationCleanupPendingValue(existing)
	receipt["detected_jobs"] = len(existing)
	receipt["removed_jobs"] = 0
	receipt["removal_failures"] = 0
	receipt["job_refs"] = []string{}
	receipt["job_refs_truncated"] = false
	for _, ref := range previousCompletedRefs {
		if err := q.ensureEvaluationCleanupMarkerRefLocked(ref, "", "", 1); err != nil {
			q.lastError = err.Error()
			return err
		}
	}
	receipt["completed_job_refs"] = continuationEvaluationCleanupRefs(previousCompletedRefs, nil)
	receipt["cumulative_removed_jobs"] = q.evaluationCleanupTotal
	return q.writeEvaluationCleanupReceiptLocked(receipt)
}

func (q *continuationDurableQueue) recordEvaluationCleanupLocked(phase string, jobs []*continuationDurableJob, removed, failures int) error {
	if q == nil || len(jobs) == 0 {
		return nil
	}
	refs := make([]string, 0, minInt(len(jobs), continuationEvaluationCleanupMaxJobRefs))
	remaining := make([]continuationEvaluationCleanupPendingJob, 0, len(q.evaluationCleanupPendingJobsLocked()))
	for _, pending := range q.evaluationCleanupPendingJobsLocked() {
		matched := false
		for _, job := range jobs {
			if job == nil {
				continue
			}
			ref := continuationEvaluationCleanupJobRef(job)
			if pending.JobID == strings.TrimSpace(job.ID) && strings.EqualFold(pending.Ref, ref) {
				matched = true
				break
			}
		}
		if !matched {
			remaining = append(remaining, pending)
		}
	}
	for _, job := range jobs {
		if job == nil || len(refs) >= continuationEvaluationCleanupMaxJobRefs {
			continue
		}
		refs = append(refs, continuationEvaluationCleanupJobRef(job))
	}
	sort.Strings(refs)
	removed = maxInt(removed, 0)
	failures = maxInt(failures, 0)
	previousCompletedRefs := continuationEvaluationCleanupRecordedRefs(q.evaluationCleanup)
	receipt := q.evaluationCleanupReceiptBaseLocked(phase)
	receipt["cleanup_state"] = continuationEvaluationCleanupStateCompleted
	if len(remaining) > 0 {
		receipt["cleanup_state"] = continuationEvaluationCleanupStatePending
	}
	receipt["pending_jobs"] = continuationEvaluationCleanupPendingValue(remaining)
	receipt["detected_jobs"] = len(jobs)
	receipt["removed_jobs"] = removed
	receipt["removal_failures"] = failures
	receipt["job_refs"] = refs
	receipt["job_refs_truncated"] = len(jobs) > len(refs)
	completedRefs := previousCompletedRefs
	for _, ref := range completedRefs {
		if err := q.ensureEvaluationCleanupMarkerRefLocked(ref, "", "", 1); err != nil {
			q.lastError = err.Error()
			return err
		}
	}
	if failures == 0 && removed > 0 {
		markerRemoved := 1
		for _, job := range jobs {
			if err := q.ensureEvaluationCleanupMarkerLocked(job, markerRemoved); err != nil {
				q.lastError = err.Error()
				return err
			}
		}
		completedRefs = continuationEvaluationCleanupRefs(completedRefs, refs)
	} else if failures == 0 {
		for _, job := range jobs {
			if err := q.ensureEvaluationCleanupMarkerLocked(job, 0); err != nil {
				q.lastError = err.Error()
				return err
			}
		}
	}
	receipt["completed_job_refs"] = completedRefs
	receipt["cumulative_removed_jobs"] = q.evaluationCleanupTotal + removed
	return q.writeEvaluationCleanupReceiptLocked(receipt)
}

func (q *continuationDurableQueue) unlinkEvaluationJobLocked(job *continuationDurableJob) error {
	if q == nil || job == nil || strings.TrimSpace(job.ID) == "" {
		return errors.New("evaluation cleanup job identity is missing")
	}
	id := strings.TrimSpace(job.ID)
	path := q.jobPath(id)
	deleter := q.evaluationCleanupDeleter
	if deleter == nil {
		deleter = os.Remove
	}
	deleteErr := deleter(path)
	pathErr := error(nil)
	if _, statErr := os.Lstat(path); statErr != nil {
		pathErr = statErr
	}
	if pathErr == nil {
		if deleteErr != nil {
			return deleteErr
		}
		return errors.New("evaluation cleanup unlink did not remove durable job")
	}
	if !os.IsNotExist(pathErr) {
		return pathErr
	}
	// The durable path is absent first. Only now remove the authoritative map
	// entry, so a delete failure retains both custody records for retry.
	if current, exists := q.jobs[id]; exists {
		delete(q.jobs, id)
		if strings.TrimSpace(current.Fingerprint) != "" && q.fingerprintIndex[current.Fingerprint] == id {
			delete(q.fingerprintIndex, current.Fingerprint)
		}
	}
	if _, exists := q.jobs[id]; exists {
		return errors.New("evaluation cleanup queue map still contains durable job")
	}
	return nil
}

func (q *continuationDurableQueue) reconcilePendingEvaluationCleanupEntryLocked(pending continuationEvaluationCleanupPendingJob) error {
	if q == nil || pending.JobID == "" || pending.Source == "" || pending.Ref == "" {
		return errors.New("evaluation cleanup pending custody is incomplete")
	}
	if job, exists := q.jobs[pending.JobID]; exists {
		if !continuationDurableJobEvaluationOwned(job) || !strings.EqualFold(continuationEvaluationCleanupJobRef(job), pending.Ref) || strings.TrimSpace(strings.ToLower(job.Source)) != pending.Source {
			return errors.New("evaluation cleanup pending custody identity conflict")
		}
		return q.cleanupEvaluationJobLocked(job, pending.Phase)
	}
	path := q.jobPath(pending.JobID)
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			synthetic := &continuationDurableJob{ID: pending.JobID, Source: pending.Source}
			removed := 1
			if pending.AlreadyCounted {
				removed = 0
			}
			return q.recordEvaluationCleanupLocked(pending.Phase, []*continuationDurableJob{synthetic}, removed, 0)
		}
		return err
	}
	raw, err := readContinuationDurableFileBounded(path, q.jobByteLimit())
	if err != nil {
		return err
	}
	var job continuationDurableJob
	if err := json.Unmarshal(raw, &job); err != nil {
		return err
	}
	q.normalizeJobLocked(&job, pending.JobID, time.Now().UTC())
	if !continuationDurableJobEvaluationOwned(&job) || strings.TrimSpace(job.ID) != pending.JobID || !strings.EqualFold(continuationEvaluationCleanupJobRef(&job), pending.Ref) || strings.TrimSpace(strings.ToLower(job.Source)) != pending.Source {
		return errors.New("evaluation cleanup pending custody identity conflict")
	}
	return q.cleanupEvaluationJobLocked(&job, pending.Phase)
}

func (q *continuationDurableQueue) reconcilePendingEvaluationCleanupLocked(skip map[string]bool) error {
	if q == nil {
		return nil
	}
	var firstErr error
	for _, pending := range q.evaluationCleanupPendingJobsLocked() {
		if skip != nil && skip[pending.Ref] {
			continue
		}
		if err := q.reconcilePendingEvaluationCleanupEntryLocked(pending); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (q *continuationDurableQueue) reconcilePendingEvaluationCleanup() error {
	if q == nil || !q.enabled {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.reconcilePendingEvaluationCleanupLocked(nil)
}

func (q *continuationDurableQueue) cleanupEvaluationJob(jobID, phase string) (bool, error) {
	if q == nil || !q.enabled {
		return false, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	jobID = strings.TrimSpace(jobID)
	job := q.jobs[jobID]
	if continuationDurableJobEvaluationOwned(job) {
		return true, q.cleanupEvaluationJobLocked(job, phase)
	}
	if pending, exists := q.pendingEvaluationCleanupByIDLocked(jobID); exists {
		return true, q.reconcilePendingEvaluationCleanupEntryLocked(pending)
	}
	return false, nil
}

func (q *continuationDurableQueue) cleanupEvaluationJobLocked(job *continuationDurableJob, phase string) error {
	if q == nil || !continuationDurableJobEvaluationOwned(job) {
		return nil
	}
	if _, exists := q.jobs[strings.TrimSpace(job.ID)]; !exists {
		if _, statErr := os.Lstat(q.jobPath(job.ID)); statErr == nil {
			q.jobs[strings.TrimSpace(job.ID)] = job
			if strings.TrimSpace(job.Fingerprint) != "" {
				q.fingerprintIndex[job.Fingerprint] = strings.TrimSpace(job.ID)
			}
		}
	}
	pending, exists, err := q.pendingEvaluationCleanupForJobLocked(job)
	if err != nil {
		q.lastError = err.Error()
		return err
	}
	if !exists {
		if q.evaluationCleanupRecordedLocked(job) {
			// A previously committed receipt is authoritative only when the
			// durable path and queue map are already absent.
			return nil
		}
		pending = continuationEvaluationCleanupPendingJob{
			JobID: strings.TrimSpace(job.ID), Source: strings.TrimSpace(strings.ToLower(job.Source)),
			Ref: continuationEvaluationCleanupJobRef(job), Phase: strings.TrimSpace(phase),
			AlreadyCounted: q.evaluationCleanupCompletedRefLocked(job),
		}
		if err := q.recordEvaluationCleanupPendingLocked(pending); err != nil {
			return err
		}
	}
	if q.evaluationCleanupCompletedRefLocked(job) {
		pending.AlreadyCounted = true
	}
	if strings.TrimSpace(pending.Phase) == "" {
		pending.Phase = strings.TrimSpace(phase)
	}
	if err := q.unlinkEvaluationJobLocked(job); err != nil {
		q.lastError = err.Error()
		return err
	}
	removed := 1
	if pending.AlreadyCounted {
		removed = 0
	}
	if err := q.recordEvaluationCleanupLocked(pending.Phase, []*continuationDurableJob{job}, removed, 0); err != nil {
		// The unlink and authoritative map removal have completed. The pending
		// journal remains durable, so restart/drain reconciliation can finish the
		// receipt without scheduling or counting this job twice.
		return err
	}
	q.lastDrainAt = nowUTCISO()
	return nil
}

func (q *continuationDurableQueue) normalizeJobLocked(job *continuationDurableJob, fallbackID string, now time.Time) {
	if job == nil {
		return
	}
	job.SchemaVersion = continuationDurableSchemaVersion
	job.ID = strings.TrimSpace(job.ID)
	if job.ID == "" {
		job.ID = strings.TrimSpace(fallbackID)
	}
	if job.ID == "" {
		job.ID = newContinuationDurableJobID("unknown", "")
	}
	job.Source = strings.TrimSpace(strings.ToLower(job.Source))
	job.Reason = strings.TrimSpace(job.Reason)
	job.StreamToken = strings.TrimSpace(job.StreamToken)
	if job.BaseRequest == nil {
		job.BaseRequest = map[string]any{}
	}
	if job.Headers == nil {
		job.Headers = map[string]string{}
	}
	if job.Fingerprint == "" {
		job.Fingerprint = continuationDurableFingerprint(job.Source, job.BaseRequest)
	}
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	if job.UpdatedAt.IsZero() {
		job.UpdatedAt = job.CreatedAt
	}
	if job.DueAt.IsZero() {
		job.DueAt = now
	}
	if job.Attempts < 0 {
		job.Attempts = 0
	}
}

func (q *continuationDurableQueue) trimExcessLocked() {
	if q.maxPendingBySource > 0 {
		bySource := map[string][]*continuationDurableJob{}
		for _, job := range q.jobs {
			if job == nil || continuationDurableJobEvaluationOwned(job) {
				continue
			}
			source := strings.TrimSpace(strings.ToLower(job.Source))
			if source == "" {
				source = "unknown"
			}
			bySource[source] = append(bySource[source], job)
		}
		for _, items := range bySource {
			if len(items) <= q.maxPendingBySource {
				continue
			}
			sort.Slice(items, func(i, j int) bool {
				if items[i].UpdatedAt.Equal(items[j].UpdatedAt) {
					return items[i].CreatedAt.After(items[j].CreatedAt)
				}
				return items[i].UpdatedAt.After(items[j].UpdatedAt)
			})
			for _, job := range items[q.maxPendingBySource:] {
				_ = q.deleteJobLocked(job.ID)
			}
		}
	}
	if q.maxPending < 1 || len(q.jobs) <= q.maxPending {
		return
	}
	items := make([]*continuationDurableJob, 0, len(q.jobs))
	for _, job := range q.jobs {
		if job == nil || continuationDurableJobEvaluationOwned(job) {
			continue
		}
		items = append(items, job)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].CreatedAt.Before(items[j].CreatedAt)
	})
	for len(q.jobs) > q.maxPending && len(items) > 0 {
		job := items[0]
		items = items[1:]
		_ = q.deleteJobLocked(job.ID)
	}
}

func (q *continuationDurableQueue) jobPath(id string) string {
	return filepath.Join(q.dir, strings.TrimSpace(id)+".json")
}

func (q *continuationDurableQueue) writeJobLocked(job *continuationDurableJob) error {
	if job == nil {
		return errors.New("nil continuation durable job")
	}
	raw, err := json.Marshal(job)
	if err != nil {
		return err
	}
	tmpPath := filepath.Join(q.dir, "."+job.ID+".tmp")
	if err := os.WriteFile(tmpPath, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, q.jobPath(job.ID)); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func (q *continuationDurableQueue) deleteJobLocked(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	if job, ok := q.jobs[id]; ok {
		delete(q.jobs, id)
		if strings.TrimSpace(job.Fingerprint) != "" {
			delete(q.fingerprintIndex, job.Fingerprint)
		}
	}
	if err := os.Remove(q.jobPath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func continuationDurableFingerprint(source string, baseRequest map[string]any) string {
	payload := map[string]any{
		"source":         strings.TrimSpace(strings.ToLower(source)),
		"project":        strings.TrimSpace(anyToString(baseRequest["project"])),
		"topic_path":     strings.TrimSpace(anyToString(baseRequest["topic_path"])),
		"query":          strings.TrimSpace(strings.ToLower(anyToString(baseRequest["query"]))),
		"retrieval_mode": normalizeRetrievalMode(anyToString(baseRequest["retrieval_mode"])),
	}
	encoded, _ := json.Marshal(payload)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:12])
}

func newContinuationDurableJobID(source string, fingerprint string) string {
	seed := fmt.Sprintf(
		"%d|%d|%s|%s",
		time.Now().UTC().UnixNano(),
		continuationDurableCounter.Add(1),
		strings.TrimSpace(strings.ToLower(source)),
		strings.TrimSpace(fingerprint),
	)
	sum := sha256.Sum256([]byte(seed))
	return "cont_" + hex.EncodeToString(sum[:8])
}

func cloneStringMap(input map[string]string) map[string]string {
	out := make(map[string]string, len(input))
	for key, value := range input {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			continue
		}
		out[key] = value
	}
	return out
}

func (q *continuationDurableQueue) enqueue(
	source string,
	reason string,
	streamToken string,
	baseRequest map[string]any,
	headers map[string]string,
	scheduleStatus string,
) (string, bool, error) {
	if q == nil || !q.enabled {
		return "", false, errors.New("continuation durable queue disabled")
	}
	if retrievalEvaluationSideEffectsSuppressed(nil, baseRequest) {
		return "", false, errors.New("evaluation continuation side effects suppressed")
	}
	normalizedSource := strings.TrimSpace(strings.ToLower(source))
	if normalizedSource == "" {
		return "", false, errors.New("continuation durable queue missing source")
	}
	baseCopy := cloneAnyMap(baseRequest)
	if len(baseCopy) == 0 {
		return "", false, errors.New("continuation durable queue missing base request")
	}
	headersCopy := cloneStringMap(headers)
	fingerprint := continuationDurableFingerprint(normalizedSource, baseCopy)
	now := time.Now().UTC()
	q.mu.Lock()
	defer q.mu.Unlock()
	if existingID, ok := q.fingerprintIndex[fingerprint]; ok {
		if existing, exists := q.jobs[existingID]; exists && existing != nil {
			if existing.DueAt.After(now) {
				existing.DueAt = now
			}
			existing.UpdatedAt = now
			existing.LastStatus = "dedup_" + strings.TrimSpace(strings.ToLower(scheduleStatus))
			if err := q.writeJobLocked(existing); err != nil {
				q.lastError = err.Error()
				return "", false, err
			}
			q.lastEnqueueAt = nowUTCISO()
			return existing.ID, true, nil
		}
		delete(q.fingerprintIndex, fingerprint)
	}
	if q.maxPendingBySource > 0 {
		sourcePending := 0
		for _, job := range q.jobs {
			if job == nil {
				continue
			}
			if strings.TrimSpace(strings.ToLower(job.Source)) == normalizedSource {
				sourcePending++
			}
		}
		if sourcePending >= q.maxPendingBySource {
			err := fmt.Errorf("continuation durable queue source %s is full", normalizedSource)
			q.lastError = err.Error()
			return "", false, err
		}
	}
	if q.maxPending > 0 && len(q.jobs) >= q.maxPending {
		err := errors.New("continuation durable queue is full")
		q.lastError = err.Error()
		return "", false, err
	}
	jobID := newContinuationDurableJobID(normalizedSource, fingerprint)
	job := &continuationDurableJob{
		SchemaVersion: continuationDurableSchemaVersion,
		ID:            jobID,
		Source:        normalizedSource,
		Reason:        strings.TrimSpace(reason),
		StreamToken:   strings.TrimSpace(streamToken),
		Fingerprint:   fingerprint,
		BaseRequest:   baseCopy,
		Headers:       headersCopy,
		CreatedAt:     now,
		UpdatedAt:     now,
		DueAt:         now,
		Attempts:      0,
		LastStatus:    strings.TrimSpace(strings.ToLower(scheduleStatus)),
	}
	if err := q.writeJobLocked(job); err != nil {
		q.lastError = err.Error()
		return "", false, err
	}
	q.jobs[job.ID] = job
	if fingerprint != "" {
		q.fingerprintIndex[fingerprint] = job.ID
	}
	q.lastEnqueueAt = nowUTCISO()
	return job.ID, false, nil
}

func (q *continuationDurableQueue) dueJobs(now time.Time, limit int) []*continuationDurableJob {
	if q == nil || !q.enabled || limit == 0 {
		return nil
	}
	if limit < 0 {
		limit = 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	items := make([]*continuationDurableJob, 0, len(q.jobs))
	for _, job := range q.jobs {
		if job == nil || job.DueAt.After(now) {
			continue
		}
		copyJob := *job
		copyJob.BaseRequest = cloneAnyMap(job.BaseRequest)
		copyJob.Headers = cloneStringMap(job.Headers)
		items = append(items, &copyJob)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].DueAt.Equal(items[j].DueAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].DueAt.Before(items[j].DueAt)
	})
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items
}

func (q *continuationDurableQueue) complete(jobID string) error {
	if q == nil || !q.enabled {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if job := q.jobs[strings.TrimSpace(jobID)]; continuationDurableJobEvaluationOwned(job) {
		return q.cleanupEvaluationJobLocked(job, "complete_guard")
	}
	if err := q.deleteJobLocked(jobID); err != nil {
		q.lastError = err.Error()
		return err
	}
	q.lastDrainAt = nowUTCISO()
	return nil
}

func (q *continuationDurableQueue) retryDelay(nextAttempt int, status string, detail map[string]any) time.Duration {
	if q == nil {
		return 2 * time.Second
	}
	if nextAttempt < 1 {
		nextAttempt = 1
	}
	if secs := anyToFloat64(detail["cooldown_remaining_secs"], 0); secs > 0 {
		cooldownDelay := time.Duration(secs * float64(time.Second))
		if cooldownDelay < q.pollInterval {
			cooldownDelay = q.pollInterval
		}
		if cooldownDelay > q.retryMax {
			cooldownDelay = q.retryMax
		}
		return cooldownDelay
	}
	delay := q.retryBase
	if delay < time.Millisecond {
		delay = time.Second
	}
	steps := nextAttempt - 1
	if steps > 8 {
		steps = 8
	}
	for i := 0; i < steps; i++ {
		delay *= 2
		if delay >= q.retryMax {
			delay = q.retryMax
			break
		}
	}
	switch strings.TrimSpace(strings.ToLower(status)) {
	case "pressure_shed", "max_inflight", "max_inflight_per_source":
		softCap := 5 * time.Second
		if softCap < q.pollInterval {
			softCap = q.pollInterval
		}
		if delay > softCap {
			delay = softCap
		}
	}
	if delay < q.pollInterval {
		delay = q.pollInterval
	}
	if delay > q.retryMax {
		delay = q.retryMax
	}
	return delay
}

func (q *continuationDurableQueue) reschedule(
	jobID string,
	status string,
	detail map[string]any,
	lastError string,
) (bool, time.Duration, error) {
	if q == nil || !q.enabled {
		return false, 0, errors.New("continuation durable queue disabled")
	}
	now := time.Now().UTC()
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.jobs[strings.TrimSpace(jobID)]
	if !ok || job == nil {
		return false, 0, nil
	}
	if continuationDurableJobEvaluationOwned(job) {
		return true, 0, q.cleanupEvaluationJobLocked(job, "reschedule_guard")
	}
	nextAttempt := job.Attempts + 1
	delay := q.retryDelay(nextAttempt, status, detail)
	if nextAttempt >= q.maxAttempts {
		if err := q.deleteJobLocked(job.ID); err != nil {
			q.lastError = err.Error()
			return true, 0, err
		}
		q.lastDrainAt = nowUTCISO()
		return true, 0, nil
	}
	job.Attempts = nextAttempt
	job.UpdatedAt = now
	job.DueAt = now.Add(delay)
	job.LastStatus = strings.TrimSpace(strings.ToLower(status))
	job.LastError = strings.TrimSpace(lastError)
	if err := q.writeJobLocked(job); err != nil {
		q.lastError = err.Error()
		return false, 0, err
	}
	q.lastDrainAt = nowUTCISO()
	return false, delay, nil
}

func (q *continuationDurableQueue) snapshot() continuationDurableSnapshot {
	snapshot := continuationDurableSnapshot{
		Enabled:    false,
		BySource:   map[string]int{},
		MaxPending: 0,
	}
	if q == nil {
		return snapshot
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	snapshot.Enabled = q.enabled
	snapshot.Dir = q.dir
	snapshot.MaxPending = q.maxPending
	snapshot.MaxPendingBySource = q.maxPendingBySource
	snapshot.MaxAttempts = q.maxAttempts
	snapshot.LastEnqueueAt = q.lastEnqueueAt
	snapshot.LastDrainAt = q.lastDrainAt
	snapshot.LastError = q.lastError
	snapshot.EvaluationCleanup = cloneAnyMap(q.evaluationCleanup)
	snapshot.EvaluationCleanupTotal = q.evaluationCleanupTotal
	snapshot.EvaluationCleanupMarkerIndex = map[string]any{
		"state":                           q.evaluationCleanupMarkerState,
		"marker_count":                    q.evaluationCleanupMarkerCount,
		"marker_bytes":                    q.evaluationCleanupMarkerBytes,
		"max_marker_count":                q.evaluationCleanupMarkerCountLimitLocked(),
		"max_marker_bytes":                q.evaluationCleanupMarkerByteLimitLocked(),
		"limit_reason":                    q.evaluationCleanupMarkerLimitReason,
		"retention":                       "never_delete_automatically",
		"cap_migration_state":             q.evaluationCleanupMarkerMigrationState,
		"cap_migration_plan_digest":       q.evaluationCleanupMarkerMigrationPlanDigest,
		"cap_migration_receipt_digest":    q.evaluationCleanupMarkerMigrationReceiptDigest,
		"cap_migration_generation":        q.evaluationCleanupMarkerMigrationGeneration,
		"cap_migration_target_generation": q.evaluationCleanupMarkerMigrationTargetGeneration,
	}
	if !q.enabled {
		return snapshot
	}
	now := time.Now().UTC()
	oldest := 0.0
	for _, job := range q.jobs {
		if job == nil {
			continue
		}
		snapshot.Pending++
		snapshot.BySource[job.Source] = snapshot.BySource[job.Source] + 1
		age := now.Sub(job.CreatedAt).Seconds()
		if age > oldest {
			oldest = age
		}
	}
	snapshot.OldestAgeSecs = roundFloat(oldest, 3)
	return snapshot
}

func continuationDurableHeaderSubset(incomingHeaders http.Header) map[string]string {
	if incomingHeaders == nil {
		return map[string]string{}
	}
	keys := []string{
		"X-Api-Key",
		"Authorization",
		"X-Agent-Id",
		"X-Session-Id",
		"X-ContextLattice-Agent",
		"X-ContextLattice-Session",
	}
	out := map[string]string{}
	for _, key := range keys {
		value := strings.TrimSpace(incomingHeaders.Get(key))
		if value == "" {
			continue
		}
		out[key] = value
	}
	return out
}

func continuationHeadersFromSubset(subset map[string]string) http.Header {
	headers := make(http.Header)
	for key, value := range subset {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			continue
		}
		headers.Set(key, value)
	}
	return headers
}

func (s *server) continuationDurableSnapshot() map[string]any {
	snapshot := continuationDurableSnapshot{Enabled: false, BySource: map[string]int{}}
	if s != nil && s.continuationDurable != nil {
		snapshot = s.continuationDurable.snapshot()
	}
	return map[string]any{
		"enabled":                         snapshot.Enabled,
		"dir":                             snapshot.Dir,
		"pending":                         snapshot.Pending,
		"by_source":                       snapshot.BySource,
		"oldest_age_secs":                 snapshot.OldestAgeSecs,
		"max_pending":                     snapshot.MaxPending,
		"max_pending_per_source":          snapshot.MaxPendingBySource,
		"max_attempts":                    snapshot.MaxAttempts,
		"last_enqueue_at":                 snapshot.LastEnqueueAt,
		"last_drain_at":                   snapshot.LastDrainAt,
		"last_error":                      snapshot.LastError,
		"evaluation_cleanup":              snapshot.EvaluationCleanup,
		"evaluation_cleanup_total":        snapshot.EvaluationCleanupTotal,
		"evaluation_cleanup_marker_index": snapshot.EvaluationCleanupMarkerIndex,
	}
}

func (s *server) enqueueContinuationDurable(
	incomingHeaders http.Header,
	baseRequest map[string]any,
	source string,
	reason string,
	streamToken string,
	scheduleStatus string,
	scheduleDetail map[string]any,
) (bool, error) {
	if retrievalEvaluationSideEffectsSuppressed(nil, baseRequest) {
		return false, errors.New("evaluation continuation side effects suppressed")
	}
	if s == nil || s.continuationDurable == nil {
		return false, errors.New("continuation durable queue is not configured")
	}
	jobID, deduped, err := s.continuationDurable.enqueue(
		source,
		reason,
		streamToken,
		baseRequest,
		continuationDurableHeaderSubset(incomingHeaders),
		scheduleStatus,
	)
	if err != nil {
		return false, err
	}
	payload := map[string]any{
		"event":           "deferred",
		"status":          "durable_queued",
		"source":          strings.TrimSpace(strings.ToLower(source)),
		"reason":          strings.TrimSpace(reason),
		"schedule_status": strings.TrimSpace(strings.ToLower(scheduleStatus)),
		"job_id":          jobID,
		"deduped":         deduped,
	}
	if len(scheduleDetail) > 0 {
		payload["queue"] = cloneAnyMap(scheduleDetail)
	}
	payload = continuationEventWithRequest(baseRequest, payload)
	s.publishContinuationEvent(streamToken, payload)
	return true, nil
}

func (s *server) scheduleOrDeferContinuation(
	incomingHeaders http.Header,
	baseRequest map[string]any,
	source string,
	reason string,
	streamToken string,
) (string, string, map[string]any) {
	if retrievalEvaluationSideEffectsSuppressed(nil, baseRequest) {
		return "suppressed", "evaluation_side_effects_suppressed", map[string]any{
			"side_effects_suppressed": true,
			"traffic_class":           "evaluation_holdout",
		}
	}
	scheduled, status, detail := s.scheduleContinuationWarmWithStatus(
		incomingHeaders,
		baseRequest,
		source,
		reason,
		streamToken,
	)
	if scheduled {
		return "scheduled", status, detail
	}
	deferred, durableErr := s.enqueueContinuationDurable(
		incomingHeaders,
		baseRequest,
		source,
		reason,
		streamToken,
		status,
		detail,
	)
	if deferred {
		trigger := continuationEventWithRequest(baseRequest, map[string]any{
			"event": "deferred", "status": "durable_queued", "source": source, "reason": reason,
		})
		s.emitContinuationSteering(baseRequest, streamToken, source, trigger)
		return "deferred", "durable_queued", detail
	}
	resultDetail := cloneAnyMap(detail)
	resultDetail["schedule_status"] = status
	if durableErr != nil {
		resultDetail["durable_error"] = durableErr.Error()
	}
	terminal := continuationEventWithRequest(baseRequest, map[string]any{
		"event": "dropped", "status": "unavailable", "source": source, "reason": reason,
	})
	s.publishContinuationEvent(streamToken, terminal)
	s.emitContinuationSteering(baseRequest, streamToken, source, terminal)
	return "unavailable", status, resultDetail
}

func (s *server) startContinuationDurableWorker() {
	if s == nil || s.continuationDurable == nil {
		return
	}
	snapshot := s.continuationDurable.snapshot()
	if !snapshot.Enabled {
		return
	}
	log.Printf(
		"gateway-go continuation durable worker enabled: dir=%s poll=%s batch=%d pending=%d",
		snapshot.Dir,
		s.continuationDurable.pollInterval.String(),
		s.continuationDurable.drainBatch,
		snapshot.Pending,
	)
	go func() {
		ticker := time.NewTicker(s.continuationDurable.pollInterval)
		defer ticker.Stop()
		for {
			s.drainContinuationDurableQueue()
			<-ticker.C
		}
	}()
}

func (s *server) drainContinuationDurableQueue() {
	if s == nil || s.continuationDurable == nil {
		return
	}
	if reconcileErr := s.continuationDurable.reconcilePendingEvaluationCleanup(); reconcileErr != nil {
		log.Printf("evaluation continuation durable pending cleanup retry failed: %v", reconcileErr)
	}
	jobs := s.continuationDurable.dueJobs(time.Now().UTC(), s.continuationDurable.drainBatch)
	if len(jobs) == 0 {
		return
	}
	for _, job := range jobs {
		if job == nil {
			continue
		}
		if continuationDurableJobEvaluationOwned(job) {
			cleaned, cleanupErr := s.continuationDurable.cleanupEvaluationJob(job.ID, "drain")
			if cleanupErr != nil {
				log.Printf("evaluation continuation durable cleanup failed id=%s source=%s err=%v", job.ID, job.Source, cleanupErr)
			}
			if cleaned {
				continue
			}
			// The copied due job was evaluation-owned but the authoritative queue
			// row no longer matches it. Never schedule from the stale copy.
			continue
		}
		incomingHeaders := continuationHeadersFromSubset(job.Headers)
		scheduled, status, detail := s.scheduleContinuationWarmWithStatus(
			incomingHeaders,
			job.BaseRequest,
			job.Source,
			job.Reason+"-durable-retry",
			job.StreamToken,
		)
		if scheduled {
			if err := s.continuationDurable.complete(job.ID); err != nil {
				log.Printf("continuation durable complete failed id=%s source=%s err=%v", job.ID, job.Source, err)
			}
			continue
		}
		dropped, delay, err := s.continuationDurable.reschedule(
			job.ID,
			status,
			detail,
			"schedule failed status="+status,
		)
		if err != nil {
			log.Printf("continuation durable reschedule failed id=%s source=%s err=%v", job.ID, job.Source, err)
			continue
		}
		if dropped {
			terminal := continuationEventWithRequest(job.BaseRequest, map[string]any{
				"event":   "dropped",
				"status":  "durable_max_attempts",
				"source":  job.Source,
				"reason":  job.Reason,
				"job_id":  job.ID,
				"attempt": job.Attempts + 1,
			})
			s.publishContinuationEvent(job.StreamToken, terminal)
			s.emitContinuationSteering(job.BaseRequest, job.StreamToken, job.Source, terminal)
			continue
		}
		retryEvent := continuationEventWithRequest(job.BaseRequest, map[string]any{
			"event":         "deferred_retry",
			"status":        status,
			"source":        job.Source,
			"reason":        job.Reason,
			"job_id":        job.ID,
			"retry_in_secs": roundFloat(delay.Seconds(), 3),
			"attempt":       job.Attempts + 1,
		})
		s.publishContinuationEvent(job.StreamToken, retryEvent)
	}
}
