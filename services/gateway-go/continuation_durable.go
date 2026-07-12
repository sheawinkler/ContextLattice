package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const continuationDurableSchemaVersion = 1

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
	Enabled            bool
	Dir                string
	Pending            int
	BySource           map[string]int
	OldestAgeSecs      float64
	MaxPending         int
	MaxPendingBySource int
	MaxAttempts        int
	LastEnqueueAt      string
	LastDrainAt        string
	LastError          string
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

	mu               sync.Mutex
	jobs             map[string]*continuationDurableJob
	fingerprintIndex map[string]string
	lastEnqueueAt    string
	lastDrainAt      string
	lastError        string
}

func newContinuationDurableQueue(policy retrievalPolicy) *continuationDurableQueue {
	queue := &continuationDurableQueue{
		enabled:            policy.continuationDurableEnabled,
		dir:                strings.TrimSpace(policy.continuationDurableDir),
		maxPending:         policy.continuationDurableMaxPending,
		maxPendingBySource: policy.continuationDurableMaxPendingBySrc,
		maxAttempts:        policy.continuationDurableMaxAttempts,
		drainBatch:         policy.continuationDurableDrainBatch,
		pollInterval:       policy.continuationDurablePollInterval,
		retryBase:          policy.continuationDurableRetryBase,
		retryMax:           policy.continuationDurableRetryMax,
		jobs:               map[string]*continuationDurableJob{},
		fingerprintIndex:   map[string]string{},
	}
	if !queue.enabled {
		return queue
	}
	if queue.dir == "" {
		queue.enabled = false
		queue.lastError = "durable continuation dir is empty"
		return queue
	}
	if err := os.MkdirAll(queue.dir, 0o755); err != nil {
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

func (q *continuationDurableQueue) loadFromDisk() error {
	if q == nil || !q.enabled {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	var firstErr error
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := strings.TrimSpace(entry.Name())
		if name == "" || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		path := filepath.Join(q.dir, name)
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			if firstErr == nil {
				firstErr = readErr
			}
			continue
		}
		var job continuationDurableJob
		if unmarshalErr := json.Unmarshal(raw, &job); unmarshalErr != nil {
			_ = os.Remove(path)
			if firstErr == nil {
				firstErr = unmarshalErr
			}
			continue
		}
		q.normalizeJobLocked(&job, strings.TrimSuffix(name, ".json"), now)
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
	q.trimExcessLocked()
	return firstErr
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
			if job == nil {
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
		if job == nil {
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
		"enabled":                snapshot.Enabled,
		"dir":                    snapshot.Dir,
		"pending":                snapshot.Pending,
		"by_source":              snapshot.BySource,
		"oldest_age_secs":        snapshot.OldestAgeSecs,
		"max_pending":            snapshot.MaxPending,
		"max_pending_per_source": snapshot.MaxPendingBySource,
		"max_attempts":           snapshot.MaxAttempts,
		"last_enqueue_at":        snapshot.LastEnqueueAt,
		"last_drain_at":          snapshot.LastDrainAt,
		"last_error":             snapshot.LastError,
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
	jobs := s.continuationDurable.dueJobs(time.Now().UTC(), s.continuationDurable.drainBatch)
	if len(jobs) == 0 {
		return
	}
	for _, job := range jobs {
		if job == nil {
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
