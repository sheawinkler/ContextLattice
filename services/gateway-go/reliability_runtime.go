package main

import (
	"context"
	"log"
	"sort"
	"strings"
	"time"
)

const sourceRowSchemaVersion = 1

type continuationQueueSnapshot struct {
	Pending        int
	PendingTotal   int
	BySource       map[string]int
	CooldownActive int
	OldestAgeSecs  float64
	RetryingCount  int
	RetryingBySrc  map[string]int
	DurablePending int
	DurableBySrc   map[string]int
	DurableOldest  float64
}

func buildSyncSourceSem(policy retrievalPolicy) map[string]chan struct{} {
	defaultCap := policy.syncSourceConcurrencyDefault
	if defaultCap < 1 {
		defaultCap = 1
	}
	sources := orderedSourceUnion(
		defaultAllSources,
		policy.defaultSources,
		policy.fastSources,
		policy.slowSources,
		policy.syncFallbackSources,
	)
	semBySource := make(map[string]chan struct{}, len(sources))
	for _, source := range sources {
		limit := defaultCap
		if override, ok := policy.syncSourceConcurrencyOverrides[source]; ok && override > 0 {
			limit = override
		}
		if limit < 1 {
			limit = 1
		}
		semBySource[source] = make(chan struct{}, limit)
	}
	return semBySource
}

func (s *server) syncSourceSemaphoreFor(source string) chan struct{} {
	normalized := strings.TrimSpace(strings.ToLower(source))
	if normalized == "" {
		return nil
	}
	if s.syncSourceSem == nil {
		return nil
	}
	sem, ok := s.syncSourceSem[normalized]
	if !ok {
		return nil
	}
	return sem
}

func (s *server) acquireSyncSourceSlot(ctx context.Context, source string) (time.Duration, bool) {
	sem := s.syncSourceSemaphoreFor(source)
	if sem == nil {
		return 0, true
	}
	normalized := strings.TrimSpace(strings.ToLower(source))
	if normalized == "" {
		return 0, false
	}
	enqueuedAt := time.Now().UTC()
	s.syncQueueMu.Lock()
	s.syncSourcePending[normalized] = append(s.syncSourcePending[normalized], enqueuedAt)
	s.syncQueueMu.Unlock()
	select {
	case sem <- struct{}{}:
		waited := time.Since(enqueuedAt)
		s.syncQueueMu.Lock()
		queue := s.syncSourcePending[normalized]
		if len(queue) <= 1 {
			delete(s.syncSourcePending, normalized)
		} else {
			s.syncSourcePending[normalized] = queue[1:]
		}
		s.syncSourceInFlight[normalized] = s.syncSourceInFlight[normalized] + 1
		if retrying := s.syncSourceRetrying[normalized]; retrying > 0 {
			if retrying == 1 {
				delete(s.syncSourceRetrying, normalized)
			} else {
				s.syncSourceRetrying[normalized] = retrying - 1
			}
		}
		s.syncQueueMu.Unlock()
		return waited, true
	case <-ctx.Done():
		waited := time.Since(enqueuedAt)
		s.syncQueueMu.Lock()
		queue := s.syncSourcePending[normalized]
		if len(queue) <= 1 {
			delete(s.syncSourcePending, normalized)
		} else {
			s.syncSourcePending[normalized] = queue[1:]
		}
		s.syncSourceRetrying[normalized] = s.syncSourceRetrying[normalized] + 1
		s.syncQueueMu.Unlock()
		return waited, false
	}
}

func (s *server) releaseSyncSourceSlot(source string) {
	sem := s.syncSourceSemaphoreFor(source)
	normalized := strings.TrimSpace(strings.ToLower(source))
	if sem != nil {
		select {
		case <-sem:
		default:
		}
	}
	if normalized == "" {
		return
	}
	s.syncQueueMu.Lock()
	current := s.syncSourceInFlight[normalized]
	if current <= 1 {
		delete(s.syncSourceInFlight, normalized)
	} else {
		s.syncSourceInFlight[normalized] = current - 1
	}
	s.syncQueueMu.Unlock()
}

func (s *server) syncQueueSnapshot() map[string]any {
	now := time.Now().UTC()
	bySource := map[string]map[string]any{}
	totalPending := 0
	totalRetrying := 0
	totalInflight := 0
	oldestAge := 0.0
	s.syncQueueMu.Lock()
	for source, queue := range s.syncSourcePending {
		pending := len(queue)
		if pending > 0 {
			totalPending += pending
			age := now.Sub(queue[0]).Seconds()
			if age > oldestAge {
				oldestAge = age
			}
			row := bySource[source]
			if row == nil {
				row = map[string]any{}
			}
			row["pending_count"] = pending
			row["oldest_age_secs"] = roundFloat(age, 3)
			bySource[source] = row
		}
	}
	for source, retrying := range s.syncSourceRetrying {
		if retrying <= 0 {
			continue
		}
		totalRetrying += retrying
		row := bySource[source]
		if row == nil {
			row = map[string]any{}
		}
		row["retrying_count"] = retrying
		bySource[source] = row
	}
	for source, inflight := range s.syncSourceInFlight {
		if inflight <= 0 {
			continue
		}
		totalInflight += inflight
		row := bySource[source]
		if row == nil {
			row = map[string]any{}
		}
		row["inflight_count"] = inflight
		bySource[source] = row
	}
	s.syncQueueMu.Unlock()

	sources := make([]string, 0, len(bySource))
	for source := range bySource {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	ordered := make([]map[string]any, 0, len(sources))
	for _, source := range sources {
		row := bySource[source]
		if _, ok := row["pending_count"]; !ok {
			row["pending_count"] = 0
		}
		if _, ok := row["retrying_count"]; !ok {
			row["retrying_count"] = 0
		}
		if _, ok := row["inflight_count"]; !ok {
			row["inflight_count"] = 0
		}
		if _, ok := row["oldest_age_secs"]; !ok {
			row["oldest_age_secs"] = 0.0
		}
		ordered = append(ordered, map[string]any{
			"source":          source,
			"pending_count":   row["pending_count"],
			"retrying_count":  row["retrying_count"],
			"inflight_count":  row["inflight_count"],
			"oldest_age_secs": row["oldest_age_secs"],
		})
	}
	return map[string]any{
		"pending_count":   totalPending,
		"retrying_count":  totalRetrying,
		"inflight_count":  totalInflight,
		"oldest_age_secs": roundFloat(oldestAge, 3),
		"by_source":       ordered,
	}
}

func (s *server) continuationQueueSnapshot() continuationQueueSnapshot {
	now := time.Now().UTC()
	snapshot := continuationQueueSnapshot{
		BySource:      map[string]int{},
		RetryingBySrc: map[string]int{},
		DurableBySrc:  map[string]int{},
	}
	s.continuationMu.Lock()
	for source, count := range s.continuationInFlight {
		if count <= 0 {
			continue
		}
		snapshot.BySource[source] = count
		snapshot.Pending += count
	}
	for source, queue := range s.continuationInFlightStarted {
		if len(queue) == 0 {
			continue
		}
		age := now.Sub(queue[0]).Seconds()
		if age > snapshot.OldestAgeSecs {
			snapshot.OldestAgeSecs = age
		}
		if _, ok := snapshot.BySource[source]; !ok {
			snapshot.BySource[source] = len(queue)
		}
	}
	for source, retrying := range s.continuationRetrying {
		if retrying <= 0 {
			continue
		}
		snapshot.RetryingBySrc[source] = retrying
		snapshot.RetryingCount += retrying
	}
	for source, until := range s.continuationSourceCooldownUntil {
		if now.Before(until) {
			if _, ok := snapshot.BySource[source]; !ok {
				snapshot.BySource[source] = 0
			}
			snapshot.CooldownActive++
		} else {
			delete(s.continuationSourceCooldownUntil, source)
		}
	}
	s.continuationMu.Unlock()
	if s.continuationDurable != nil {
		durable := s.continuationDurable.snapshot()
		snapshot.DurablePending = durable.Pending
		snapshot.DurableOldest = durable.OldestAgeSecs
		for source, count := range durable.BySource {
			if count <= 0 {
				continue
			}
			snapshot.DurableBySrc[source] = count
		}
	}
	snapshot.PendingTotal = snapshot.Pending + snapshot.DurablePending
	snapshot.OldestAgeSecs = roundFloat(snapshot.OldestAgeSecs, 3)
	snapshot.DurableOldest = roundFloat(snapshot.DurableOldest, 3)
	return snapshot
}

func (s *server) incrementContinuationRetrying(source string) {
	normalized := strings.TrimSpace(strings.ToLower(source))
	if normalized == "" {
		return
	}
	s.continuationMu.Lock()
	s.continuationRetrying[normalized] = s.continuationRetrying[normalized] + 1
	s.continuationMu.Unlock()
}

func (s *server) decrementContinuationRetrying(source string) {
	normalized := strings.TrimSpace(strings.ToLower(source))
	if normalized == "" {
		return
	}
	s.continuationMu.Lock()
	current := s.continuationRetrying[normalized]
	if current <= 1 {
		delete(s.continuationRetrying, normalized)
	} else {
		s.continuationRetrying[normalized] = current - 1
	}
	s.continuationMu.Unlock()
}

func (s *server) shouldShedContinuation(source string) (bool, string, map[string]any) {
	if !s.retrieval.continuationSheddingEnabled {
		return false, "", nil
	}
	normalized := strings.TrimSpace(strings.ToLower(source))
	if normalized == "" {
		return false, "", nil
	}
	if s.isNonDegradableSource(normalized) {
		return false, "", nil
	}
	if _, ok := s.retrieval.continuationSheddingSources[normalized]; !ok {
		return false, "", nil
	}
	queueCap := cap(s.continuationSem)
	if queueCap < 1 {
		queueCap = 1
	}
	snapshot := s.continuationQueueSnapshot()
	ratio := float64(snapshot.Pending) / float64(queueCap)
	if snapshot.Pending >= s.retrieval.continuationSheddingPendingHigh || ratio >= s.retrieval.continuationSheddingQueueRatio {
		return true, "pressure_shed", map[string]any{
			"pending_count":      snapshot.Pending,
			"pending_ratio":      roundFloat(ratio, 3),
			"cooldown_active":    snapshot.CooldownActive,
			"oldest_age_secs":    snapshot.OldestAgeSecs,
			"retrying_count":     snapshot.RetryingCount,
			"queue_ratio_target": s.retrieval.continuationSheddingQueueRatio,
			"pending_high":       s.retrieval.continuationSheddingPendingHigh,
		}
	}
	return false, "", nil
}

func (s *server) recordTimeoutContractViolation(
	source string,
	phase string,
	timeout time.Duration,
	elapsed time.Duration,
	reason string,
) {
	normalizedSource := strings.TrimSpace(strings.ToLower(source))
	if normalizedSource == "" {
		normalizedSource = "unknown"
	}
	normalizedPhase := strings.TrimSpace(strings.ToLower(phase))
	if normalizedPhase == "" {
		normalizedPhase = "unknown"
	}
	normalizedReason := strings.TrimSpace(strings.ToLower(reason))
	if normalizedReason == "" {
		normalizedReason = "unknown"
	}
	total := s.timeoutContractViolations.Add(1)
	s.timeoutContractMu.Lock()
	s.timeoutContractBySource[normalizedSource] = s.timeoutContractBySource[normalizedSource] + 1
	s.timeoutContractLast = map[string]any{
		"at":             nowUTCISO(),
		"source":         normalizedSource,
		"phase":          normalizedPhase,
		"reason":         normalizedReason,
		"timeout_secs":   roundFloat(timeout.Seconds(), 3),
		"elapsed_secs":   roundFloat(elapsed.Seconds(), 3),
		"violations":     total,
		"contract_grace": roundFloat(s.retrieval.timeoutContractGrace.Seconds(), 3),
	}
	s.timeoutContractMu.Unlock()
	log.Printf(
		"retrieval timeout contract violation source=%s phase=%s reason=%s timeout=%s elapsed=%s total=%d",
		normalizedSource,
		normalizedPhase,
		normalizedReason,
		timeout.String(),
		elapsed.String(),
		total,
	)
}

func (s *server) timeoutContractSnapshot() map[string]any {
	total := s.timeoutContractViolations.Load()
	bySource := map[string]uint64{}
	last := map[string]any{}
	s.timeoutContractMu.Lock()
	for source, count := range s.timeoutContractBySource {
		bySource[source] = count
	}
	if len(s.timeoutContractLast) > 0 {
		last = cloneAnyMap(s.timeoutContractLast)
	}
	s.timeoutContractMu.Unlock()
	return map[string]any{
		"violations": total,
		"by_source":  bySource,
		"last":       last,
	}
}

func (s *server) watchTimeoutContract(
	source string,
	phase string,
	timeout time.Duration,
	startedAt time.Time,
	callDone <-chan sourceCallPayload,
) {
	if timeout <= 0 || callDone == nil {
		return
	}
	grace := s.retrieval.timeoutContractGrace
	if grace <= 0 {
		grace = 75 * time.Millisecond
	}
	contractWindow := timeout + grace
	go func() {
		select {
		case <-callDone:
			elapsed := time.Since(startedAt)
			if elapsed > contractWindow {
				s.recordTimeoutContractViolation(source, phase, timeout, elapsed, "late_completion")
			}
		case <-time.After(contractWindow):
			elapsed := time.Since(startedAt)
			s.recordTimeoutContractViolation(source, phase, timeout, elapsed, "no_completion_within_contract")
		}
	}()
}

func looksLikeParseError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(err.Error()))
	if text == "" {
		return false
	}
	markers := []string{
		"cannot unmarshal",
		"invalid character",
		"unexpected end of json input",
		"failed to parse",
		"malformed",
		"decode",
		"parse",
	}
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func (s *server) recordDrift(class string, source string, detail string) {
	driftClass := strings.TrimSpace(strings.ToLower(class))
	if driftClass == "" {
		driftClass = "unknown"
	}
	driftSource := strings.TrimSpace(strings.ToLower(source))
	if driftSource == "" {
		driftSource = "unknown"
	}
	s.driftMu.Lock()
	s.driftByClass[driftClass] = s.driftByClass[driftClass] + 1
	s.driftBySource[driftSource] = s.driftBySource[driftSource] + 1
	s.driftLast = map[string]any{
		"at":     nowUTCISO(),
		"class":  driftClass,
		"source": driftSource,
		"detail": strings.TrimSpace(detail),
	}
	s.driftMu.Unlock()
}

func (s *server) driftSnapshot() map[string]any {
	s.driftMu.Lock()
	defer s.driftMu.Unlock()
	byClass := map[string]uint64{}
	bySource := map[string]uint64{}
	total := uint64(0)
	for class, count := range s.driftByClass {
		byClass[class] = count
		total += count
	}
	for source, count := range s.driftBySource {
		bySource[source] = count
	}
	last := map[string]any{}
	if len(s.driftLast) > 0 {
		last = cloneAnyMap(s.driftLast)
	}
	return map[string]any{
		"total":      total,
		"by_class":   byClass,
		"by_source":  bySource,
		"last_event": last,
	}
}

func (s *server) normalizeSourceRows(source string, rows []map[string]any) []map[string]any {
	normalizedSource := strings.TrimSpace(strings.ToLower(source))
	if normalizedSource == "" {
		normalizedSource = "unknown"
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			s.recordDrift("schema_nil_row", normalizedSource, "source returned nil row")
			continue
		}
		cloned := cloneAnyMap(row)
		project := strings.TrimSpace(anyToString(cloned["project"]))
		fileName := strings.TrimSpace(anyToString(cloned["file"]))
		summary := strings.TrimSpace(anyToString(cloned["summary"]))
		if summary == "" {
			summary = clipText(strings.TrimSpace(anyToString(cloned["content"])), 400)
		}
		if summary == "" && project != "" && fileName != "" {
			summary = "context row for " + project + "/" + fileName
		}
		if project == "" || fileName == "" || summary == "" {
			s.recordDrift("schema_malformed_row", normalizedSource, "missing project/file/summary fields")
			continue
		}
		topicPath := strings.TrimSpace(anyToString(cloned["topic_path"]))
		if topicPath == "" {
			topicPath = deriveTopicFromFile(fileName)
		}
		score := parseScore(cloned)
		if score <= 0 {
			score = 0.001
			s.recordDrift("score_missing_or_invalid", normalizedSource, "score missing or invalid; clamped")
		}
		cloned["project"] = project
		cloned["file"] = fileName
		cloned["summary"] = summary
		cloned["topic_path"] = topicPath
		cloned["score"] = score
		cloned["source"] = normalizedSource
		if strings.TrimSpace(anyToString(cloned["source_owner"])) == "" {
			cloned["source_owner"] = sourceOwnerForSource(normalizedSource)
		}
		cloned["schema_version"] = sourceRowSchemaVersion
		dataClass := strings.TrimSpace(strings.ToLower(anyToString(cloned["data_class"])))
		if dataClass != dataClassRuntimeStateMirror {
			dataClass = dataClassLearningMemory
		}
		cloned["data_class"] = dataClass
		out = append(out, cloned)
	}
	return out
}
