package main

import (
	"container/heap"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

func normalizeTopicPathLoose(value string) string {
	trimmed := strings.Trim(strings.TrimSpace(strings.ReplaceAll(value, "\\", "/")), "/")
	if trimmed == "" {
		return ""
	}
	parts := strings.Split(trimmed, "/")
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		token := strings.TrimSpace(part)
		if token == "" || token == "." {
			continue
		}
		if token == ".." {
			continue
		}
		clean = append(clean, token)
	}
	return strings.Join(clean, "/")
}

const (
	savedRecallEvalV3SchemaID         = "saved_recall_eval_case_set.v3"
	savedRecallEvalV3Version          = 3
	savedRecallEvalV3SnapshotSchemaID = "saved_recall_eval_snapshot.v1"
	savedRecallEvalV3CustodySchemaID  = "saved_recall_eval_custody.v1"
	savedRecallEvalV3MaxCases         = 300
	savedRecallEvalV3MaxSourceDocs    = 20000
	savedRecallEvalV3MaxGraphCases    = 25
	savedRecallEvalV3HoldoutPercent   = 20
)

type recallEvalSourceCandidate struct {
	doc          memoryStoreDoc
	agentID      string
	sessionID    string
	sourceFamily string
	createdAt    time.Time
	stableKey    string
	ageBucket    string
	queryIntent  string
	difficulty   string
	split        string
}

type recallEvalRankedKey struct {
	key  string
	rank string
}

// recallEvalRankedKeyHeap keeps the worst (largest hash rank) item at the
// root. It is deliberately bounded by the requested sample size so refresh
// does not materialize or sort the full current-state corpus.
type recallEvalRankedKeyHeap []recallEvalRankedKey

func (h recallEvalRankedKeyHeap) Len() int { return len(h) }

func (h recallEvalRankedKeyHeap) Less(i int, j int) bool {
	if h[i].rank == h[j].rank {
		return h[i].key > h[j].key
	}
	return h[i].rank > h[j].rank
}

func (h recallEvalRankedKeyHeap) Swap(i int, j int) { h[i], h[j] = h[j], h[i] }

func (h *recallEvalRankedKeyHeap) Push(value any) {
	*h = append(*h, value.(recallEvalRankedKey))
}

func (h *recallEvalRankedKeyHeap) Pop() any {
	old := *h
	n := len(old)
	value := old[n-1]
	*h = old[:n-1]
	return value
}

func recallEvalRankedKeyIsBetter(candidate recallEvalRankedKey, current recallEvalRankedKey) bool {
	if candidate.rank == current.rank {
		return candidate.key < current.key
	}
	return candidate.rank < current.rank
}

func recallEvalRankedKeyRank(key string) string {
	return sha256Hex("saved_recall_eval.v3.source\x00" + key)
}

func recallEvalAddRankedKey(sample *recallEvalRankedKeyHeap, candidate recallEvalRankedKey, limit int) {
	if sample == nil || limit < 1 {
		return
	}
	if sample.Len() < limit {
		heap.Push(sample, candidate)
		return
	}
	if sample.Len() > 0 && recallEvalRankedKeyIsBetter(candidate, (*sample)[0]) {
		heap.Pop(sample)
		heap.Push(sample, candidate)
	}
}

func recallEvalSortedRankedKeys(sample recallEvalRankedKeyHeap) []recallEvalRankedKey {
	result := append([]recallEvalRankedKey(nil), sample...)
	sort.Slice(result, func(i int, j int) bool {
		if result[i].rank == result[j].rank {
			return result[i].key < result[j].key
		}
		return result[i].rank < result[j].rank
	})
	return result
}

type recallEvalSourceStats struct {
	Scanned         int
	Population      int
	Sample          int
	IndexMode       string
	IndexIntegrity  bool
	Bounded         bool
	ContextCanceled bool
}

func (stats recallEvalSourceStats) mapValue() map[string]any {
	return map[string]any{
		"scanned_count":     stats.Scanned,
		"population_count":  stats.Population,
		"sample_count":      stats.Sample,
		"index_mode":        stats.IndexMode,
		"index_integrity":   stats.IndexIntegrity,
		"bounded":           stats.Bounded,
		"context_cancelled": stats.ContextCanceled,
	}
}

// recallEvalSourceStateEligibleForSampling removes records that can never
// become a valid direct v3 case before bottom-K selection. This is essential
// on real stores where a high-volume root-topic lane would otherwise consume
// the bounded sample and crowd out concrete, benchmarkable topics.
func recallEvalSourceStateEligibleForSampling(state memoryCurrentState, fallbackTopic string, topicPrefix string) bool {
	if state.Tombstone {
		return false
	}
	entry := state.Entry
	if strings.TrimSpace(entry.Project) == "" || strings.Trim(strings.TrimSpace(entry.FileName), "/") == "" || strings.TrimSpace(entry.Summary) == "" {
		return false
	}
	topic := normalizeTopicPathLoose(entry.TopicPath)
	if topic == "" || topic == "root" || topic == "." {
		topic = normalizeTopicPathLoose(fallbackTopic)
	}
	if topic == "" || topic == "root" || topic == "." {
		topic = normalizeTopicPathLoose(deriveTopicFromFile(entry.FileName))
	}
	if topic == "" || topic == "root" || topic == "." {
		return false
	}
	return recallEvalTopicMatchesPrefix(topic, topicPrefix)
}

// recallEvalIndexedCandidates reads the bounded in-process current-state or
// recent-history index. It deliberately does not walk the external memory
// volume when the workspace scope is empty; refresh must remain proportional
// to the indexed metadata available to gateway-go.
func (s *server) recallEvalIndexedCandidates(ctx context.Context, project string, topicPrefix string, maxDocs int) ([]recallEvalSourceCandidate, string, map[string]any) {
	if s == nil || s.memoryStore == nil || !s.memoryStore.isEnabled() || maxDocs < 1 {
		return []recallEvalSourceCandidate{}, "memory_store_unavailable", recallEvalSourceStats{
			IndexMode: "unavailable", Bounded: true,
		}.mapValue()
	}
	m := s.memoryStore
	if ctx == nil {
		ctx = context.Background()
	}
	project = strings.TrimSpace(project)
	if project == "" {
		var sample recallEvalRankedKeyHeap
		heap.Init(&sample)
		stats := recallEvalSourceStats{IndexMode: "current_state_bottom_k", IndexIntegrity: true, Bounded: true}
		m.mu.RLock()
		stats.Population = len(m.currentState)
		for key, state := range m.currentState {
			select {
			case <-ctx.Done():
				stats.ContextCanceled = true
				break
			default:
			}
			if stats.ContextCanceled {
				break
			}
			stats.Scanned++
			if key == "::" {
				continue
			}
			if !recallEvalSourceStateEligibleForSampling(state, m.latestTopic[key], topicPrefix) {
				continue
			}
			recallEvalAddRankedKey(&sample, recallEvalRankedKey{key: key, rank: recallEvalRankedKeyRank(key)}, maxDocs)
		}
		m.mu.RUnlock()
		keys := recallEvalSortedRankedKeys(sample)
		entries := recallEvalCurrentStateEntriesForKeys(ctx, m, keys)
		identity, exactStatePaths := recallEvalMetadataForKeys(m, keys)
		stats.Sample = len(entries)
		if stats.ContextCanceled || ctx.Err() != nil {
			stats.ContextCanceled = true
		}
		if len(entries) > 0 {
			candidates, source := recallEvalCandidatesFromCurrentStates(ctx, m, entries, identity, exactStatePaths, project, topicPrefix, maxDocs, "current_state_bottom_k")
			stats.Sample = len(candidates)
			return candidates, source, stats.mapValue()
		}
		// A legacy process may have only its bounded recent ring in memory.
		return recallEvalCandidatesFromRecentHistory(ctx, m, project, topicPrefix, maxDocs, stats)
	}

	// The scoped lane uses the integrity-checked project index. It never scans
	// unrelated current-state keys, and it releases the store lock before
	// sorting, copying, or deriving candidates.
	var sample recallEvalRankedKeyHeap
	heap.Init(&sample)
	stats := recallEvalSourceStats{IndexMode: "project_current_state_bottom_k", Bounded: true}
	projectKey := normalizeCurrentKeyIndexProject(project)
	indexValid := false
	indexMaterialized := false
	m.mu.RLock()
	indexMaterialized = m.currentKeysByProject != nil || m.currentKeyCountsByProject != nil
	indexedKeys, indexPresent := m.currentKeysByProject[projectKey]
	expectedCount, countKnown := m.currentKeyCountsByProject[projectKey]
	if indexMaterialized {
		// Once the project index exists, a missing count/key set is an empty
		// project or corruption, never permission to rescan unrelated storage.
		if !indexPresent && !countKnown {
			indexValid = true
			stats.IndexMode = "project_current_state_empty"
		} else if !indexPresent || !countKnown || expectedCount < 0 || expectedCount != len(indexedKeys) {
			stats.IndexMode = "project_index_integrity_invalid"
		} else {
			indexValid = true
			stats.Population = expectedCount
			for key := range indexedKeys {
				select {
				case <-ctx.Done():
					stats.ContextCanceled = true
				default:
				}
				if stats.ContextCanceled {
					break
				}
				stats.Scanned++
				keyProject, fileName, parsed := parseMemoryStoreKeyToken(key)
				state, stateOK := m.currentState[key]
				if !parsed || fileName == "" || !strings.EqualFold(keyProject, project) || !stateOK || state.Tombstone ||
					!strings.EqualFold(strings.TrimSpace(state.Entry.Project), project) {
					indexValid = false
					stats.IndexMode = "project_index_integrity_invalid"
					break
				}
				if !recallEvalSourceStateEligibleForSampling(state, m.latestTopic[key], topicPrefix) {
					continue
				}
				recallEvalAddRankedKey(&sample, recallEvalRankedKey{key: key, rank: recallEvalRankedKeyRank(key)}, maxDocs)
			}
		}
	}
	m.mu.RUnlock()
	stats.IndexIntegrity = indexValid
	if indexMaterialized {
		if !indexValid {
			return []recallEvalSourceCandidate{}, "project_index_integrity_invalid", stats.mapValue()
		}
		if stats.ContextCanceled || ctx.Err() != nil {
			stats.ContextCanceled = true
			return []recallEvalSourceCandidate{}, "project_current_state_cancelled", stats.mapValue()
		}
		keys := recallEvalSortedRankedKeys(sample)
		entries := recallEvalCurrentStateEntriesForKeys(ctx, m, keys)
		identity, exactStatePaths := recallEvalMetadataForKeys(m, keys)
		stats.Sample = len(entries)
		if len(entries) > 0 {
			candidates, source := recallEvalCandidatesFromCurrentStates(ctx, m, entries, identity, exactStatePaths, project, topicPrefix, maxDocs, "project_current_state_bottom_k")
			stats.Sample = len(candidates)
			return candidates, source, stats.mapValue()
		}
		return []recallEvalSourceCandidate{}, "project_current_state_empty", stats.mapValue()
	}

	// A pre-indexed store from an older process may not have current-state
	// materialized yet. Keep this fallback project-scoped and bounded by the
	// memory-store scan policy; it is never used for an unscoped workspace.
	docs, err := m.collectDocs(ctx, project, true, false)
	if err != nil {
		stats.IndexMode = "project_scoped_store_unavailable"
		stats.ContextCanceled = stats.ContextCanceled || ctx.Err() != nil
		return []recallEvalSourceCandidate{}, "project_scoped_store_unavailable", stats.mapValue()
	}
	sourcePopulation := len(docs)
	if len(docs) > maxDocs {
		docs = docs[:maxDocs]
	}
	fallbackKeys := make([]recallEvalRankedKey, 0, len(docs))
	for _, doc := range docs {
		key := memoryStoreKey(doc.Project, doc.FileName)
		fallbackKeys = append(fallbackKeys, recallEvalRankedKey{key: key, rank: recallEvalRankedKeyRank(key)})
	}
	identity, _ := recallEvalMetadataForKeys(m, fallbackKeys)
	candidates := make([]recallEvalSourceCandidate, 0, len(docs))
	for _, doc := range docs {
		createdAt := doc.UpdatedAt
		key := memoryStoreKey(doc.Project, doc.FileName)
		meta := identity[key]
		candidates = append(candidates, recallEvalSourceCandidate{
			doc:          doc,
			agentID:      meta.agentID,
			sessionID:    meta.sessionID,
			sourceFamily: "unknown",
			createdAt:    createdAt,
			stableKey:    recallEvalCandidateStableKey(doc.Project, doc.FileName, doc.TopicPath),
		})
	}
	stats.IndexMode = "project_scoped_store_fallback"
	stats.Population = sourcePopulation
	stats.Scanned = sourcePopulation
	stats.Sample = len(candidates)
	stats.IndexIntegrity = false
	return candidates, "project_scoped_store_fallback", stats.mapValue()
}

func recallEvalCurrentStateEntriesForKeys(ctx context.Context, m *memoryStore, keys []recallEvalRankedKey) []memoryCurrentState {
	if m == nil || len(keys) == 0 {
		return []memoryCurrentState{}
	}
	entries := make([]memoryCurrentState, 0, len(keys))
	m.mu.RLock()
	for _, ranked := range keys {
		select {
		case <-ctx.Done():
			m.mu.RUnlock()
			return entries
		default:
		}
		if state, ok := m.currentState[ranked.key]; ok {
			entries = append(entries, state)
		}
	}
	m.mu.RUnlock()
	return entries
}

func recallEvalCandidatesFromRecentHistory(ctx context.Context, m *memoryStore, project string, topicPrefix string, maxDocs int, stats recallEvalSourceStats) ([]recallEvalSourceCandidate, string, map[string]any) {
	if m == nil {
		return []recallEvalSourceCandidate{}, "recent_history_unavailable", stats.mapValue()
	}
	var sample recallEvalRankedKeyHeap
	heap.Init(&sample)
	selectedEntries := make(map[string]memoryStoreEntry, maxDocs)
	m.mu.RLock()
	stats.Population = len(m.recent)
	for idx := len(m.recent) - 1; idx >= 0; idx-- {
		select {
		case <-ctx.Done():
			stats.ContextCanceled = true
		default:
		}
		if stats.ContextCanceled {
			break
		}
		stats.Scanned++
		entry := m.recent[idx]
		key := memoryStoreKey(entry.Project, entry.FileName)
		if key == "::" {
			continue
		}
		if _, alreadySelected := selectedEntries[key]; alreadySelected {
			continue
		}
		candidate := recallEvalRankedKey{key: key, rank: recallEvalRankedKeyRank(key)}
		if sample.Len() < maxDocs {
			heap.Push(&sample, candidate)
			selectedEntries[key] = entry
			continue
		}
		if sample.Len() > 0 && recallEvalRankedKeyIsBetter(candidate, sample[0]) {
			evicted := sample[0].key
			heap.Pop(&sample)
			heap.Push(&sample, candidate)
			delete(selectedEntries, evicted)
			selectedEntries[key] = entry
		}
	}
	m.mu.RUnlock()
	ranked := recallEvalSortedRankedKeys(sample)
	identity := map[string]recallEvalIdentity{}
	exactStatePaths := recallEvalExactStatePathsForKeys(m, ranked)
	entries := make([]memoryCurrentState, 0, len(ranked))
	for _, item := range ranked {
		if entry, ok := selectedEntries[item.key]; ok {
			entries = append(entries, memoryCurrentStateFromEntry(entry))
		}
	}
	stats.Sample = len(entries)
	candidates, source := recallEvalCandidatesFromCurrentStates(ctx, m, entries, identity, exactStatePaths, project, topicPrefix, maxDocs, "recent_history_bottom_k")
	stats.Sample = len(candidates)
	return candidates, source, stats.mapValue()
}

type recallEvalIdentity struct {
	agentID   string
	sessionID string
	createdAt time.Time
}

func recallEvalMetadataForKeys(m *memoryStore, keys []recallEvalRankedKey) (map[string]recallEvalIdentity, map[string]struct{}) {
	identity := make(map[string]recallEvalIdentity, len(keys))
	exactStatePaths := make(map[string]struct{}, len(keys))
	if m == nil || len(keys) == 0 {
		return identity, exactStatePaths
	}
	keySet := make(map[string]struct{}, len(keys))
	for _, ranked := range keys {
		keySet[ranked.key] = struct{}{}
	}
	// Scan the bounded recent ring while retaining metadata only for the
	// already-selected bottom-K keys. This keeps the refresh auxiliary memory
	// proportional to maxDocs rather than maxRecent or the exact-state corpus.
	m.mu.RLock()
	for _, entry := range m.recent {
		key := memoryStoreKey(entry.Project, entry.FileName)
		if _, selected := keySet[key]; !selected || key == "::" {
			continue
		}
		createdAt, _ := parseTimeBestEffort(entry.CreatedAt)
		current, exists := identity[key]
		if exists && !createdAt.After(current.createdAt) {
			continue
		}
		identity[key] = recallEvalIdentity{
			agentID:   strings.TrimSpace(entry.AgentID),
			sessionID: strings.TrimSpace(entry.SessionID),
			createdAt: createdAt,
		}
	}
	for _, ranked := range keys {
		if _, exact := m.exactStatePaths[ranked.key]; exact {
			exactStatePaths[ranked.key] = struct{}{}
		}
	}
	m.mu.RUnlock()
	return identity, exactStatePaths
}

func recallEvalExactStatePathsForKeys(m *memoryStore, keys []recallEvalRankedKey) map[string]struct{} {
	paths := make(map[string]struct{}, len(keys))
	if m == nil || len(keys) == 0 {
		return paths
	}
	m.mu.RLock()
	for _, ranked := range keys {
		if _, exact := m.exactStatePaths[ranked.key]; exact {
			paths[ranked.key] = struct{}{}
		}
	}
	m.mu.RUnlock()
	return paths
}

func recallEvalCandidatesFromCurrentStates(
	ctx context.Context,
	m *memoryStore,
	entries []memoryCurrentState,
	identity map[string]recallEvalIdentity,
	exactStatePaths map[string]struct{},
	project string,
	topicPrefix string,
	maxDocs int,
	sourceKind string,
) ([]recallEvalSourceCandidate, string) {
	candidates := make([]recallEvalSourceCandidate, 0, minInt(len(entries), maxDocs))
	writePolicy := loadWriteIngressPolicy()
	for _, state := range entries {
		select {
		case <-ctx.Done():
			return candidates, sourceKind
		default:
		}
		if state.Tombstone {
			continue
		}
		entry := state.Entry
		entryProject := strings.TrimSpace(entry.Project)
		fileName := strings.Trim(strings.TrimSpace(entry.FileName), "/")
		if entryProject == "" || fileName == "" || (project != "" && !strings.EqualFold(entryProject, project)) {
			continue
		}
		if exactStatePathSetContains(exactStatePaths, entryProject, fileName) || writePolicy.isDurableMemoryFile(normalizedWrite{project: entryProject, fileName: fileName}) {
			continue
		}
		topic := normalizeTopicPathLoose(entry.TopicPath)
		if topic == "" {
			m.mu.RLock()
			topic = normalizeTopicPathLoose(m.latestTopic[memoryStoreKey(entryProject, fileName)])
			m.mu.RUnlock()
		}
		if topic == "" {
			topic = normalizeTopicPathLoose(deriveTopicFromFile(fileName))
		}
		if !recallEvalTopicMatchesPrefix(topic, topicPrefix) {
			continue
		}
		lifecycle := normalizeMemoryLifecycle(entry.Lifecycle)
		if isEphemeralMemoryIdentity(fileName, topic, entry.Summary, lifecycle) || !shouldSurfaceMemoryLifecycle(lifecycle, false) {
			continue
		}
		storageTier := normalizeMemoryStorageTier(entry.StorageTier)
		createdAt, _ := parseTimeBestEffort(entry.CreatedAt)
		updatedAt := createdAt
		if updatedAt.IsZero() {
			updatedAt = time.Time{}
		}
		key := memoryStoreKey(entryProject, fileName)
		meta := identity[key]
		if strings.TrimSpace(entry.AgentID) == "" {
			entry.AgentID = meta.agentID
		}
		if strings.TrimSpace(entry.SessionID) == "" {
			entry.SessionID = meta.sessionID
		}
		if createdAt.IsZero() {
			createdAt = meta.createdAt
		}
		lastTouch := m.docLastTouch(key, updatedAt)
		horizon := m.effectiveHotHorizonDays(key)
		objectID := strings.TrimSpace(entry.ObjectID)
		if objectID == "" {
			objectID = m.objectIDFor(entryProject, fileName, topic, strings.TrimPrefix(strings.ToLower(entry.ContentHash), "sha256:"))
		}
		summary := strings.TrimSpace(entry.Summary)
		if summary == "" {
			continue
		}
		candidates = append(candidates, recallEvalSourceCandidate{
			doc: memoryStoreDoc{
				Project: entryProject, FileName: fileName, TopicPath: topic,
				Summary: clipSummary(summary, m.policy.maxSummaryChars), UpdatedAt: updatedAt,
				ObjectID: objectID, Horizon: horizon, Score: entry.Confidence,
				LastTouch: lastTouch, Lifecycle: lifecycle, StorageTier: storageTier,
			},
			agentID: strings.TrimSpace(entry.AgentID), sessionID: strings.TrimSpace(entry.SessionID),
			sourceFamily: firstNonEmptyStrings(strings.TrimSpace(entry.Source), "unknown"),
			createdAt:    createdAt,
			stableKey:    recallEvalCandidateStableKey(entryProject, fileName, topic),
		})
		if len(candidates) >= maxDocs {
			break
		}
	}
	return candidates, sourceKind
}

func recallEvalTopicMatchesPrefix(topic string, prefix string) bool {
	topic = normalizeTopicPathLoose(topic)
	prefix = normalizeTopicPathLoose(prefix)
	return prefix == "" || topic == prefix || strings.HasPrefix(topic, prefix+"/")
}

func recallEvalCandidateStableKey(project string, fileName string, topic string) string {
	return strings.ToLower(strings.TrimSpace(project)) + "\x00" +
		strings.ToLower(strings.Trim(strings.TrimSpace(fileName), "/")) + "\x00" +
		normalizeTopicPathLoose(topic)
}

func recallEvalEligibleCandidates(candidates []recallEvalSourceCandidate, minHits int, project string, topicPrefix string) []recallEvalSourceCandidate {
	topicCounts := map[string]int{}
	for _, candidate := range candidates {
		if !recallEvalTopicMatchesPrefix(candidate.doc.TopicPath, topicPrefix) {
			continue
		}
		topicCounts[normalizeTopicPathLoose(candidate.doc.TopicPath)]++
	}
	seen := map[string]struct{}{}
	eligible := make([]recallEvalSourceCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if project != "" && !strings.EqualFold(strings.TrimSpace(candidate.doc.Project), project) {
			continue
		}
		topic := normalizeTopicPathLoose(candidate.doc.TopicPath)
		if !recallEvalTopicMatchesPrefix(topic, topicPrefix) || topic == "" || topic == "root" || topic == "." || topicCounts[topic] < minHits {
			continue
		}
		fileName := strings.Trim(strings.TrimSpace(candidate.doc.FileName), "/")
		if fileName == "" || recallEvalQueryFromDoc(candidate.doc) == "" {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(candidate.doc.Project)) + "\x00" + strings.ToLower(fileName)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		candidate.stableKey = recallEvalCandidateStableKey(candidate.doc.Project, fileName, topic)
		candidate.queryIntent = recallEvalQueryIntent(candidate.doc.TopicPath, candidate.doc.Summary)
		candidate.difficulty = recallEvalDifficulty(candidate.doc, candidate.queryIntent)
		candidate.sourceFamily = firstNonEmptyStrings(candidate.sourceFamily, "unknown")
		eligible = append(eligible, candidate)
	}
	sort.SliceStable(eligible, func(i, j int) bool { return eligible[i].stableKey < eligible[j].stableKey })
	// The validator treats project+query as the direct-case identity. Deduplicate
	// after stable sorting so repeated summaries cannot create a knowingly
	// invalid frozen set and the retained representative is deterministic.
	unique := make([]recallEvalSourceCandidate, 0, len(eligible))
	seenQueries := map[string]struct{}{}
	for _, candidate := range eligible {
		query := strings.ToLower(strings.Join(strings.Fields(recallEvalQueryFromDoc(candidate.doc)), " "))
		queryKey := strings.ToLower(strings.TrimSpace(candidate.doc.Project)) + "\x00" + query
		if _, exists := seenQueries[queryKey]; exists {
			continue
		}
		seenQueries[queryKey] = struct{}{}
		unique = append(unique, candidate)
	}
	eligible = unique
	times := make([]time.Time, 0, len(eligible))
	for _, candidate := range eligible {
		if !candidate.createdAt.IsZero() {
			times = append(times, candidate.createdAt)
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	for idx := range eligible {
		eligible[idx].ageBucket = recallEvalAgeBucket(eligible[idx].createdAt, times)
	}
	return eligible
}

func recallEvalSelectCandidates(candidates []recallEvalSourceCandidate, limit int) ([]recallEvalSourceCandidate, map[string]any) {
	if limit <= 0 || len(candidates) == 0 {
		return []recallEvalSourceCandidate{}, map[string]any{"enabled": false, "reason": "no_candidates"}
	}
	withTimes := make([]recallEvalSourceCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		candidate.createdAt = candidate.createdAt.UTC()
		withTimes = append(withTimes, candidate)
	}
	times := make([]time.Time, 0, len(withTimes))
	for _, candidate := range withTimes {
		if !candidate.createdAt.IsZero() {
			times = append(times, candidate.createdAt)
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	holdoutCount := 0
	holdoutCutoff := time.Time{}
	if len(times) >= 2 && !times[0].Equal(times[len(times)-1]) {
		holdoutCount = maxInt(1, (len(times)*savedRecallEvalV3HoldoutPercent+99)/100)
		if holdoutCount >= len(times) {
			holdoutCount = len(times) - 1
		}
		holdoutCutoff = times[len(times)-holdoutCount]
	}
	for idx := range withTimes {
		withTimes[idx].ageBucket = recallEvalAgeBucket(withTimes[idx].createdAt, times)
		withTimes[idx].queryIntent = firstNonEmptyStrings(withTimes[idx].queryIntent, recallEvalQueryIntent(withTimes[idx].doc.TopicPath, withTimes[idx].doc.Summary))
		withTimes[idx].difficulty = firstNonEmptyStrings(withTimes[idx].difficulty, recallEvalDifficulty(withTimes[idx].doc, withTimes[idx].queryIntent))
		withTimes[idx].sourceFamily = firstNonEmptyStrings(withTimes[idx].sourceFamily, "unknown")
		if !holdoutCutoff.IsZero() && !withTimes[idx].createdAt.IsZero() && !withTimes[idx].createdAt.Before(holdoutCutoff) {
			withTimes[idx].split = "holdout"
		} else {
			withTimes[idx].split = "train"
		}
	}
	selected := make([]recallEvalSourceCandidate, 0, minInt(limit, len(withTimes)))
	// Reserve a temporal holdout slice when the source has enough timestamped
	// documents. This keeps the benchmark honest instead of letting diversity
	// selection accidentally consume only the training period.
	holdoutLimit := 0
	if holdoutCount > 0 {
		holdoutLimit = minInt(maxInt(1, (limit*savedRecallEvalV3HoldoutPercent+99)/100), holdoutCount)
	}
	trainLimit := maxInt(0, limit-holdoutLimit)
	holdouts := recallEvalDiversePick(withTimes, "holdout", holdoutLimit)
	trains := recallEvalDiversePick(withTimes, "train", trainLimit)
	selected = append(selected, trains...)
	selected = append(selected, holdouts...)
	if len(selected) < minInt(limit, len(withTimes)) {
		selectedKeys := map[string]struct{}{}
		for _, candidate := range selected {
			selectedKeys[candidate.stableKey] = struct{}{}
		}
		remaining := make([]recallEvalSourceCandidate, 0, len(withTimes))
		for _, candidate := range withTimes {
			if _, exists := selectedKeys[candidate.stableKey]; !exists {
				remaining = append(remaining, candidate)
			}
		}
		selected = append(selected, recallEvalDiversePick(remaining, "", limit-len(selected))...)
	}
	sort.SliceStable(selected, func(i, j int) bool {
		if selected[i].split != selected[j].split {
			return selected[i].split < selected[j].split
		}
		return selected[i].stableKey < selected[j].stableKey
	})
	return selected, map[string]any{
		"enabled":          !holdoutCutoff.IsZero(),
		"holdout_cutoff":   anyTimeString(holdoutCutoff),
		"holdout_count":    holdoutCount,
		"holdout_selected": len(holdouts),
		"train_selected":   len(trains),
		"policy":           "newest_timestamped_documents_reserved_as_temporal_holdout",
	}
}

func recallEvalDiversePick(candidates []recallEvalSourceCandidate, split string, limit int) []recallEvalSourceCandidate {
	if limit <= 0 {
		return []recallEvalSourceCandidate{}
	}
	available := make([]recallEvalSourceCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if split == "" || candidate.split == split {
			available = append(available, candidate)
		}
	}
	selected := make([]recallEvalSourceCandidate, 0, minInt(limit, len(available)))
	dimensions := []string{"project", "topic", "age", "agent", "session", "source_family", "lifecycle", "intent", "difficulty", "horizon"}
	seen := map[string]map[string]struct{}{}
	for _, dimension := range dimensions {
		seen[dimension] = map[string]struct{}{}
	}
	for len(selected) < limit && len(available) > 0 {
		best := 0
		bestScore := -1
		bestRank := ""
		for idx, candidate := range available {
			values := []string{
				strings.ToLower(strings.TrimSpace(candidate.doc.Project)),
				normalizeTopicPathLoose(candidate.doc.TopicPath),
				candidate.ageBucket,
				strings.ToLower(strings.TrimSpace(candidate.agentID)),
				strings.ToLower(strings.TrimSpace(candidate.sessionID)),
				strings.ToLower(strings.TrimSpace(candidate.sourceFamily)),
				normalizeMemoryLifecycle(candidate.doc.Lifecycle),
				strings.ToLower(strings.TrimSpace(candidate.queryIntent)),
				strings.ToLower(strings.TrimSpace(candidate.difficulty)),
				fmt.Sprintf("horizon_%d", maxInt(candidate.doc.Horizon, 0)),
			}
			score := 0
			for dimension, value := range values {
				if value == "" {
					value = "unknown"
				}
				if _, exists := seen[dimensions[dimension]][value]; !exists {
					score++
				}
			}
			rank := sha256Hex("saved_recall_eval.v3.pick\x00" + candidate.stableKey)
			if score > bestScore || (score == bestScore && rank < bestRank) {
				best, bestScore, bestRank = idx, score, rank
			}
		}
		candidate := available[best]
		selected = append(selected, candidate)
		values := []string{
			strings.ToLower(strings.TrimSpace(candidate.doc.Project)), normalizeTopicPathLoose(candidate.doc.TopicPath), candidate.ageBucket,
			strings.ToLower(strings.TrimSpace(candidate.agentID)), strings.ToLower(strings.TrimSpace(candidate.sessionID)),
			strings.ToLower(strings.TrimSpace(candidate.sourceFamily)), normalizeMemoryLifecycle(candidate.doc.Lifecycle),
			strings.ToLower(strings.TrimSpace(candidate.queryIntent)), strings.ToLower(strings.TrimSpace(candidate.difficulty)),
			fmt.Sprintf("horizon_%d", maxInt(candidate.doc.Horizon, 0)),
		}
		for dimension, value := range values {
			if value == "" {
				value = "unknown"
			}
			seen[dimensions[dimension]][value] = struct{}{}
		}
		available = append(available[:best], available[best+1:]...)
	}
	return selected
}

func recallEvalAgeBucket(value time.Time, times []time.Time) string {
	if value.IsZero() || len(times) == 0 {
		return "unknown"
	}
	rank := sort.Search(len(times), func(idx int) bool { return !times[idx].Before(value) })
	bucket := rank * 4 / maxInt(1, len(times))
	if bucket > 3 {
		bucket = 3
	}
	return fmt.Sprintf("age_q%d", bucket)
}

func recallEvalQueryIntent(topic string, summary string) string {
	text := strings.ToLower(strings.TrimSpace(topic + " " + summary))
	switch {
	case strings.Contains(text, "debug") || strings.Contains(text, "incident") || strings.Contains(text, "failure"):
		return "debug"
	case strings.Contains(text, "release") || strings.Contains(text, "deploy") || strings.Contains(text, "version"):
		return "release"
	case strings.Contains(text, "decision") || strings.Contains(text, "objective") || strings.Contains(text, "plan"):
		return "decision"
	case strings.Contains(text, "research") || strings.Contains(text, "experiment") || strings.Contains(text, "benchmark"):
		return "research"
	case strings.Contains(text, "runbook") || strings.Contains(text, "procedure") || strings.Contains(text, "how to"):
		return "procedural"
	default:
		return "general"
	}
}

func recallEvalDifficulty(doc memoryStoreDoc, intent string) string {
	length := len([]rune(strings.TrimSpace(doc.Summary)))
	depth := topicDepth(doc.TopicPath)
	switch {
	case length >= 900 || depth >= 5 || intent == "research":
		return "hard"
	case length >= 300 || depth >= 3 || intent == "debug" || intent == "decision":
		return "medium"
	default:
		return "easy"
	}
}

func recallEvalCasesFromCandidates(candidates []recallEvalSourceCandidate, project string) []map[string]any {
	cases := make([]map[string]any, 0, len(candidates))
	for _, candidate := range candidates {
		fileName := strings.Trim(strings.TrimSpace(candidate.doc.FileName), "/")
		caseProject := strings.TrimSpace(candidate.doc.Project)
		if caseProject == "" {
			caseProject = project
		}
		query := recallEvalQueryFromDoc(candidate.doc)
		if query == "" || caseProject == "" || fileName == "" {
			continue
		}
		caseID := "case-" + sha256Hex("saved_recall_eval.v3.case\x00" + caseProject + "\x00" + fileName + "\x00" + candidate.split + "\x00" + query)[:20]
		expectedTerms := []string{}
		if summary := recallEvalRedactFileTokens(strings.ToLower(strings.TrimSpace(candidate.doc.Summary)), fileName); summary != "" {
			expectedTerms = append(expectedTerms, clipText(summary, 96))
		}
		row := map[string]any{
			"id":                  caseID,
			"query":               query,
			"project":             caseProject,
			"topic_path":          normalizeTopicPathLoose(candidate.doc.TopicPath),
			"limit":               10,
			"expected_files":      []string{fileName},
			"expected_substrings": expectedTerms,
			"split":               candidate.split,
			"temporal_holdout":    candidate.split == "holdout",
			"age_bucket":          candidate.ageBucket,
			"source_updated_at":   anyTimeString(candidate.createdAt),
			"agent_id":            strings.TrimSpace(candidate.agentID),
			"session_id":          strings.TrimSpace(candidate.sessionID),
			"source_family":       firstNonEmptyStrings(candidate.sourceFamily, "unknown"),
			"lifecycle":           firstNonEmptyStrings(candidate.doc.Lifecycle, "unknown"),
			"storage_tier":        firstNonEmptyStrings(candidate.doc.StorageTier, "unknown"),
			"horizon_days":        maxInt(candidate.doc.Horizon, 0),
			"query_intent":        firstNonEmptyStrings(candidate.queryIntent, "general"),
			"difficulty":          firstNonEmptyStrings(candidate.difficulty, "unknown"),
			"label_derivation":    "one_file_from_current_state_index",
			"query_derivation":    "topic_plus_summary_filename_redacted",
			"oracle_leakage":      "filename_removed; summary-derived query retained",
		}
		cases = append(cases, row)
	}
	return cases
}

func recallEvalSnapshotDigest(candidates []recallEvalSourceCandidate) string {
	rows := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		rows = append(rows, strings.Join([]string{
			candidate.stableKey,
			candidate.doc.UpdatedAt.UTC().Format(time.RFC3339Nano),
			candidate.createdAt.UTC().Format(time.RFC3339Nano),
			strings.TrimSpace(candidate.agentID), strings.TrimSpace(candidate.sessionID),
			sha256Hex(strings.TrimSpace(candidate.doc.Summary)),
		}, "\x00"))
	}
	sort.Strings(rows)
	return sha256Hex(strings.Join(rows, "\n"))
}

func recallEvalCaseSetDigest(cases []map[string]any) string {
	canonical, err := json.Marshal(cases)
	if err != nil {
		return sha256Hex("saved_recall_eval.v3.empty")
	}
	return sha256Hex(string(canonical))
}

func recallEvalPopulationMetadata(population []recallEvalSourceCandidate, sample []map[string]any) map[string]any {
	countCandidates := func(dimension string, values []string) map[string]int {
		counts := map[string]int{}
		for _, value := range values {
			value = recallEvalOpaqueStratumValue(dimension, value)
			counts[value]++
		}
		return counts
	}
	populationDimensions := map[string][]string{
		"project": {}, "topic": {}, "age": {}, "agent": {}, "session": {},
		"source_family": {}, "lifecycle": {}, "intent": {}, "difficulty": {}, "horizon": {},
	}
	for _, candidate := range population {
		populationDimensions["project"] = append(populationDimensions["project"], candidate.doc.Project)
		populationDimensions["topic"] = append(populationDimensions["topic"], candidate.doc.TopicPath)
		populationDimensions["age"] = append(populationDimensions["age"], candidate.ageBucket)
		populationDimensions["agent"] = append(populationDimensions["agent"], candidate.agentID)
		populationDimensions["session"] = append(populationDimensions["session"], candidate.sessionID)
		populationDimensions["source_family"] = append(populationDimensions["source_family"], candidate.sourceFamily)
		populationDimensions["lifecycle"] = append(populationDimensions["lifecycle"], candidate.doc.Lifecycle)
		populationDimensions["intent"] = append(populationDimensions["intent"], candidate.queryIntent)
		populationDimensions["difficulty"] = append(populationDimensions["difficulty"], candidate.difficulty)
		populationDimensions["horizon"] = append(populationDimensions["horizon"], fmt.Sprintf("%d", maxInt(candidate.doc.Horizon, 0)))
	}
	populationCounts := map[string]any{}
	populationUnique := map[string]int{}
	for dimension, values := range populationDimensions {
		counts := countCandidates(dimension, values)
		populationCounts[dimension] = counts
		populationUnique[dimension] = len(counts)
	}
	sampleDimensions := map[string][]string{}
	for dimension := range populationDimensions {
		sampleDimensions[dimension] = []string{}
	}
	for _, rawCase := range sample {
		sampleDimensions["project"] = append(sampleDimensions["project"], anyToString(rawCase["project"]))
		sampleDimensions["topic"] = append(sampleDimensions["topic"], anyToString(rawCase["topic_path"]))
		sampleDimensions["age"] = append(sampleDimensions["age"], anyToString(rawCase["age_bucket"]))
		sampleDimensions["agent"] = append(sampleDimensions["agent"], anyToString(rawCase["agent_id"]))
		sampleDimensions["session"] = append(sampleDimensions["session"], anyToString(rawCase["session_id"]))
		sampleDimensions["source_family"] = append(sampleDimensions["source_family"], anyToString(rawCase["source_family"]))
		sampleDimensions["lifecycle"] = append(sampleDimensions["lifecycle"], anyToString(rawCase["lifecycle"]))
		sampleDimensions["intent"] = append(sampleDimensions["intent"], anyToString(rawCase["query_intent"]))
		sampleDimensions["difficulty"] = append(sampleDimensions["difficulty"], anyToString(rawCase["difficulty"]))
		sampleDimensions["horizon"] = append(sampleDimensions["horizon"], fmt.Sprintf("%d", maxInt(anyToInt(rawCase["horizon_days"], 0), 0)))
	}
	sampleCounts := map[string]any{}
	sampleUnique := map[string]int{}
	for dimension, values := range sampleDimensions {
		counts := countCandidates(dimension, values)
		sampleCounts[dimension] = counts
		sampleUnique[dimension] = len(counts)
	}
	// Small populations cannot satisfy a large-workspace diversity threshold;
	// minima scale with the observed population and never invent unavailable
	// strata. The resulting boolean is persisted and validated as custody data.
	minimums := map[string]int{}
	for _, dimension := range []string{"project", "topic", "age", "agent", "session", "source_family", "lifecycle", "intent", "difficulty", "horizon"} {
		populationMinimum := 1
		if populationUnique[dimension] >= 10 {
			populationMinimum = 2
		}
		if populationUnique[dimension] >= 20 {
			populationMinimum = 3
		}
		minimums[dimension] = minInt(populationUnique[dimension], populationMinimum)
	}
	valid := len(sample) > 0
	missing := []string{}
	for dimension, minimum := range minimums {
		if sampleUnique[dimension] < minimum {
			valid = false
			missing = append(missing, dimension)
		}
	}
	sort.Strings(missing)
	return map[string]any{
		"population": map[string]any{"count": len(population), "unique": populationUnique, "counts": populationCounts},
		"sample":     map[string]any{"count": len(sample), "unique": sampleUnique, "counts": sampleCounts},
		"diversity": map[string]any{
			"valid":    valid,
			"minimums": minimums,
			"missing":  missing,
			"policy":   "scaled_minimum_two_or_three_per_available_dimension",
		},
	}
}

func recallEvalOpaqueStratumValue(dimension string, value string) string {
	value = firstNonEmptyStrings(strings.ToLower(strings.TrimSpace(value)), "unknown")
	// Population/sample metadata is persisted with the frozen case set. Keep
	// project, topic, agent, session, and source-family strata useful for
	// cardinality proofs without writing private identity/content labels into
	// the benchmark artifact.
	switch dimension {
	case "project", "topic", "agent", "session", "source_family":
		return "sha256:" + sha256Hex("saved_recall_eval.v3.stratum\x00" + dimension + "\x00" + value)[:16]
	default:
		return value
	}
}

func anyTimeString(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}
