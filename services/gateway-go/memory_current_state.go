package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	memoryCurrentStateSchemaID   = "contextlattice_memory_current_state.v1"
	memoryCurrentStateShardCount = 64
)

type memoryCurrentState struct {
	Entry     memoryStoreEntry `json:"entry"`
	LegalHold bool             `json:"legal_hold,omitempty"`
	Tombstone bool             `json:"tombstone,omitempty"`
}

type memoryCurrentStateShard struct {
	SchemaID string               `json:"schema_id"`
	Version  int                  `json:"version"`
	Shard    int                  `json:"shard"`
	Entries  []memoryCurrentState `json:"entries"`
}

func memoryTagsHaveLegalHold(tags []string) bool {
	for _, raw := range tags {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "legal_hold", "legal-hold", "hold:legal", "retention:legal-hold":
			return true
		}
	}
	return false
}

func memoryTagsEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (m *memoryStore) currentStateRootPath() string {
	if m == nil {
		return ""
	}
	if path := strings.TrimSpace(m.policy.currentStatePath); path != "" {
		return filepath.Clean(path)
	}
	return filepath.Join(m.policy.rootPath, "_contextlattice", "memory_current_state")
}

func memoryCurrentStateShardForKey(key string) int {
	sum := sha256.Sum256([]byte(strings.TrimSpace(strings.ToLower(key))))
	return int(sum[0]) % memoryCurrentStateShardCount
}

func (m *memoryStore) currentStateShardPath(shard int) string {
	return filepath.Join(m.currentStateRootPath(), fmt.Sprintf("%02x.json", shard))
}

func memoryCurrentStateFromEntry(entry memoryStoreEntry) memoryCurrentState {
	copyEntry := entry
	copyEntry.Tags = append([]string(nil), entry.Tags...)
	copyEntry.Lifecycle = normalizeMemoryLifecycle(entry.Lifecycle)
	copyEntry.StorageTier = normalizeMemoryStorageTier(entry.StorageTier)
	return memoryCurrentState{
		Entry:     copyEntry,
		LegalHold: memoryTagsHaveLegalHold(copyEntry.Tags),
		Tombstone: isMemoryTombstone(copyEntry),
	}
}

func memoryCurrentStateSupersedes(candidate memoryCurrentState, current memoryCurrentState) bool {
	candidateAt, candidateOK := parseTimeBestEffort(candidate.Entry.CreatedAt)
	currentAt, currentOK := parseTimeBestEffort(current.Entry.CreatedAt)
	if candidateOK && currentOK {
		if candidateAt.After(currentAt) {
			return true
		}
		if candidateAt.Before(currentAt) {
			return false
		}
	}
	if candidateOK != currentOK {
		return candidateOK
	}
	if candidate.Entry.EventID == current.Entry.EventID {
		return true
	}
	return strings.TrimSpace(candidate.Entry.EventID) > strings.TrimSpace(current.Entry.EventID)
}

func (m *memoryStore) ensureCurrentStateMapLocked() {
	if m.currentState == nil {
		m.currentState = map[string]memoryCurrentState{}
	}
}

func (m *memoryStore) ensureCurrentKeyIndexLocked() {
	if m == nil {
		return
	}
	if m.currentKeysByProject == nil {
		m.currentKeysByProject = map[string]map[string]struct{}{}
	}
	if m.currentKeyCountsByProject == nil {
		m.currentKeyCountsByProject = map[string]int{}
	}
	if m.currentKeysByProjectTopic == nil {
		m.currentKeysByProjectTopic = map[string]map[string]map[string]struct{}{}
	}
	if m.currentTopicKeyCountsByProject == nil {
		m.currentTopicKeyCountsByProject = map[string]int{}
	}
	if m.currentKeyIndexGeneration == nil {
		m.currentKeyIndexGeneration = map[string]uint64{}
	}
	if m.currentTopicIndexGeneration == nil {
		m.currentTopicIndexGeneration = map[string]uint64{}
	}
}

func normalizeCurrentKeyIndexProject(project string) string {
	return strings.ToLower(strings.TrimSpace(project))
}

const currentStateUnscopedTopicBucket = "\x00"

func currentStateTopicBucket(topicPath string) string {
	normalized := normalizeTopicPathLoose(topicPath)
	if normalized == "" {
		return currentStateUnscopedTopicBucket
	}
	return normalized
}

func (m *memoryStore) ensureCurrentProjectIndexLocked(projectKey string) {
	if m == nil || projectKey == "" {
		return
	}
	m.ensureCurrentKeyIndexLocked()
	if m.currentKeysByProject[projectKey] == nil {
		m.currentKeysByProject[projectKey] = map[string]struct{}{}
	}
	if m.currentKeysByProjectTopic[projectKey] == nil {
		m.currentKeysByProjectTopic[projectKey] = map[string]map[string]struct{}{}
	}
	if _, exists := m.currentKeyCountsByProject[projectKey]; !exists {
		m.currentKeyCountsByProject[projectKey] = 0
	}
	if _, exists := m.currentTopicKeyCountsByProject[projectKey]; !exists {
		m.currentTopicKeyCountsByProject[projectKey] = 0
	}
	if _, exists := m.currentKeyIndexGeneration[projectKey]; !exists {
		m.currentKeyIndexGeneration[projectKey] = 0
	}
	if _, exists := m.currentTopicIndexGeneration[projectKey]; !exists {
		m.currentTopicIndexGeneration[projectKey] = 0
	}
}

func (m *memoryStore) advanceCurrentIndexGenerationLocked(projectKey string) {
	if m == nil || projectKey == "" {
		return
	}
	keyGeneration := m.currentKeyIndexGeneration[projectKey]
	topicGeneration := m.currentTopicIndexGeneration[projectKey]
	if keyGeneration != topicGeneration {
		return
	}
	if keyGeneration == ^uint64(0) {
		// Saturation is an explicit fail-closed state; never wrap and make a
		// stale topic projection look coherent with the primary index.
		m.currentTopicIndexGeneration[projectKey] = 0
		return
	}
	keyGeneration++
	m.currentKeyIndexGeneration[projectKey] = keyGeneration
	m.currentTopicIndexGeneration[projectKey] = keyGeneration
}

func (m *memoryStore) addCurrentKeyLocked(project string, key string, topicPath string) {
	if m == nil || key == "::" {
		return
	}
	projectKey := normalizeCurrentKeyIndexProject(project)
	if projectKey == "" {
		return
	}
	m.ensureCurrentProjectIndexLocked(projectKey)
	keys := m.currentKeysByProject[projectKey]
	changed := false
	if _, exists := keys[key]; !exists {
		keys[key] = struct{}{}
		m.currentKeyCountsByProject[projectKey]++
		changed = true
	}

	newTopic := currentStateTopicBucket(topicPath)
	oldTopic := currentStateTopicBucket(m.latestTopic[key])
	topics := m.currentKeysByProjectTopic[projectKey]
	if oldTopic != newTopic {
		if oldKeys := topics[oldTopic]; oldKeys != nil {
			if _, exists := oldKeys[key]; exists {
				delete(oldKeys, key)
				m.currentTopicKeyCountsByProject[projectKey]--
				changed = true
				if len(oldKeys) == 0 {
					delete(topics, oldTopic)
				}
			}
		}
	}
	newKeys := topics[newTopic]
	if newKeys == nil {
		newKeys = map[string]struct{}{}
		topics[newTopic] = newKeys
	}
	if _, exists := newKeys[key]; !exists {
		newKeys[key] = struct{}{}
		m.currentTopicKeyCountsByProject[projectKey]++
		changed = true
	}
	if changed {
		m.advanceCurrentIndexGenerationLocked(projectKey)
	}
}

func (m *memoryStore) removeCurrentKeyLocked(project string, key string) {
	if m == nil || key == "::" || m.currentKeysByProject == nil {
		return
	}
	projectKey := normalizeCurrentKeyIndexProject(project)
	keys := m.currentKeysByProject[projectKey]
	if keys == nil {
		return
	}
	if _, exists := keys[key]; !exists {
		return
	}
	delete(keys, key)
	oldTopic := currentStateTopicBucket(m.latestTopic[key])
	if topics := m.currentKeysByProjectTopic[projectKey]; topics != nil {
		if topicKeys := topics[oldTopic]; topicKeys != nil {
			if _, exists := topicKeys[key]; exists {
				delete(topicKeys, key)
				m.currentTopicKeyCountsByProject[projectKey]--
				if len(topicKeys) == 0 {
					delete(topics, oldTopic)
				}
			}
		}
	}
	if count := m.currentKeyCountsByProject[projectKey] - 1; count > 0 {
		m.currentKeyCountsByProject[projectKey] = count
	} else {
		m.currentKeyCountsByProject[projectKey] = 0
	}
	m.advanceCurrentIndexGenerationLocked(projectKey)
}

func (m *memoryStore) applyCurrentStateEntryLocked(entry memoryStoreEntry) bool {
	if m == nil {
		return false
	}
	m.ensureCurrentStateMapLocked()
	key := memoryStoreKey(entry.Project, entry.FileName)
	if key == "::" {
		return false
	}
	candidate := memoryCurrentStateFromEntry(entry)
	if current, exists := m.currentState[key]; exists && !memoryCurrentStateSupersedes(candidate, current) {
		return false
	}
	m.currentState[key] = candidate
	return true
}

func (m *memoryStore) restoreLatestIndexesFromCurrentStateLocked() {
	if m == nil {
		return
	}
	m.ensureCurrentStateMapLocked()
	m.ensureCurrentKeyIndexLocked()
	m.currentKeysByProject = map[string]map[string]struct{}{}
	m.currentKeyCountsByProject = map[string]int{}
	m.currentKeysByProjectTopic = map[string]map[string]map[string]struct{}{}
	m.currentTopicKeyCountsByProject = map[string]int{}
	m.currentKeyIndexGeneration = map[string]uint64{}
	m.currentTopicIndexGeneration = map[string]uint64{}
	m.latestTopic = map[string]string{}
	for key, state := range m.currentState {
		entry := state.Entry
		if exactStatePathSetContains(m.exactStatePaths, entry.Project, entry.FileName) {
			delete(m.latestTopic, key)
			delete(m.latestHash, key)
			delete(m.latestHorizon, key)
			delete(m.latestLifecycle, key)
			delete(m.latestStorageTier, key)
			delete(m.lastAccess, key)
			delete(m.confidence, key)
			continue
		}
		if state.Tombstone {
			projectKey := normalizeCurrentKeyIndexProject(entry.Project)
			if projectKey != "" {
				m.ensureCurrentProjectIndexLocked(projectKey)
			}
			delete(m.latestTopic, key)
			delete(m.latestHash, key)
			delete(m.latestHorizon, key)
			delete(m.latestLifecycle, key)
			delete(m.latestStorageTier, key)
			delete(m.lastAccess, key)
			delete(m.confidence, key)
			continue
		}
		m.addCurrentKeyLocked(entry.Project, key, entry.TopicPath)
		m.latestTopic[key] = entry.TopicPath
		m.latestLifecycle[key] = normalizeMemoryLifecycle(entry.Lifecycle)
		m.latestStorageTier[key] = normalizeMemoryStorageTier(entry.StorageTier)
		if strings.TrimSpace(entry.ContentHash) != "" {
			m.latestHash[key] = entry.ContentHash
		}
		if entry.HorizonDays != 0 {
			m.latestHorizon[key] = entry.HorizonDays
		}
		if accessedAt, ok := parseTimeBestEffort(entry.LastAccess); ok {
			m.lastAccess[key] = accessedAt
		}
		if entry.Confidence > 0 {
			weight := m.policy.confidenceReadWeight + m.policy.confidenceWriteWeight
			m.confidence[key] = confidenceState{
				alpha: m.policy.confidencePriorAlpha + (entry.Confidence * weight),
				beta:  m.policy.confidencePriorBeta + ((1.0 - entry.Confidence) * weight),
			}
		}
	}
}

func (m *memoryStore) loadCurrentState() error {
	if m == nil || !m.isConfigured() {
		return nil
	}
	m.ensureCurrentStateMapLocked()
	for shard := 0; shard < memoryCurrentStateShardCount; shard++ {
		path := m.currentStateShardPath(shard)
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read memory current-state shard %d: %w", shard, err)
		}
		payload := memoryCurrentStateShard{}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("decode memory current-state shard %d: %w", shard, err)
		}
		if payload.SchemaID != memoryCurrentStateSchemaID || payload.Version != 1 || payload.Shard != shard {
			return fmt.Errorf("memory current-state shard %d has an invalid contract", shard)
		}
		for _, state := range payload.Entries {
			project, err := sanitizeMemoryProject(state.Entry.Project)
			if err != nil {
				return fmt.Errorf("memory current-state shard %d has invalid project: %w", shard, err)
			}
			fileName, err := sanitizeMemoryFile(state.Entry.FileName)
			if err != nil {
				return fmt.Errorf("memory current-state shard %d has invalid file: %w", shard, err)
			}
			state.Entry.Project = project
			state.Entry.FileName = fileName
			state.Entry.Tags = append([]string(nil), state.Entry.Tags...)
			state.Entry.Lifecycle = normalizeMemoryLifecycle(state.Entry.Lifecycle)
			state.Entry.StorageTier = normalizeMemoryStorageTier(state.Entry.StorageTier)
			state.LegalHold = state.LegalHold || memoryTagsHaveLegalHold(state.Entry.Tags)
			state.Tombstone = state.Tombstone || isMemoryTombstone(state.Entry)
			key := memoryStoreKey(project, fileName)
			if memoryCurrentStateShardForKey(key) != shard {
				return fmt.Errorf("memory current-state shard %d contains a misplaced entry", shard)
			}
			if current, exists := m.currentState[key]; !exists || memoryCurrentStateSupersedes(state, current) {
				m.currentState[key] = state
			}
		}
	}
	m.restoreLatestIndexesFromCurrentStateLocked()
	return nil
}

func (m *memoryStore) persistCurrentStateShardLocked(shard int) error {
	if m == nil || shard < 0 || shard >= memoryCurrentStateShardCount {
		return errors.New("invalid memory current-state shard")
	}
	entries := make([]memoryCurrentState, 0)
	for key, state := range m.currentState {
		if memoryCurrentStateShardForKey(key) != shard {
			continue
		}
		state.Entry.Tags = append([]string(nil), state.Entry.Tags...)
		entries = append(entries, state)
	}
	sort.Slice(entries, func(i, j int) bool {
		left := memoryStoreKey(entries[i].Entry.Project, entries[i].Entry.FileName)
		right := memoryStoreKey(entries[j].Entry.Project, entries[j].Entry.FileName)
		return left < right
	})
	payload, err := json.Marshal(memoryCurrentStateShard{
		SchemaID: memoryCurrentStateSchemaID,
		Version:  1,
		Shard:    shard,
		Entries:  entries,
	})
	if err != nil {
		return fmt.Errorf("encode memory current-state shard %d: %w", shard, err)
	}
	if err := writeOwnerOnlyDurableAtomicFile(m.currentStateShardPath(shard), append(payload, '\n'), true); err != nil {
		return fmt.Errorf("persist memory current-state shard %d: %w", shard, err)
	}
	return nil
}

func (m *memoryStore) persistCurrentStateShardsLocked(shards map[int]struct{}) error {
	ordered := make([]int, 0, len(shards))
	for shard := range shards {
		ordered = append(ordered, shard)
	}
	sort.Ints(ordered)
	for _, shard := range ordered {
		if err := m.persistCurrentStateShardLocked(shard); err != nil {
			return err
		}
	}
	return nil
}

func (m *memoryStore) persistAndRecordEntry(entry memoryStoreEntry) error {
	if m == nil {
		return errors.New("memory store unavailable")
	}
	key := memoryStoreKey(entry.Project, entry.FileName)
	shard := memoryCurrentStateShardForKey(key)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureCurrentStateMapLocked()
	previous, existed := m.currentState[key]
	changed := m.applyCurrentStateEntryLocked(entry)
	if changed {
		if err := m.persistCurrentStateShardLocked(shard); err != nil {
			if existed {
				m.currentState[key] = previous
			} else {
				delete(m.currentState, key)
			}
			return err
		}
	}
	m.recordEntry(entry)
	return nil
}

func (m *memoryStore) currentStateFor(project, fileName string) (memoryCurrentState, bool) {
	if m == nil {
		return memoryCurrentState{}, false
	}
	key := memoryStoreKey(project, fileName)
	m.mu.RLock()
	state, ok := m.currentState[key]
	m.mu.RUnlock()
	if !ok {
		return memoryCurrentState{}, false
	}
	state.Entry.Tags = append([]string(nil), state.Entry.Tags...)
	return state, true
}

func (m *memoryStore) currentEntry(project, fileName string) (memoryStoreEntry, bool) {
	state, ok := m.currentStateFor(project, fileName)
	if !ok || state.Tombstone {
		return memoryStoreEntry{}, false
	}
	return state.Entry, true
}

func requestIncludesColdMemory(request map[string]any) bool {
	return anyToBool(request["include_cold"]) ||
		strings.EqualFold(strings.TrimSpace(anyToString(request["retrieval_mode"])), "deep")
}

func normalizeProjectedContentHash(raw string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(raw)), "sha256:")
}

type vectorReconcileStats struct {
	Suppressed       int
	CurrentEvent     int
	CurrentHash      int
	LegacyPathOnly   int
	StaleEvent       int
	HashMismatch     int
	MissingAuthority int
	LifecycleHidden  int
	DuplicatePath    int
}

func (stats vectorReconcileStats) warning(source string) string {
	return fmt.Sprintf(
		"%s authoritative memory state suppressed %d fallback result(s) (stale_event=%d hash_mismatch=%d missing_authority=%d lifecycle_hidden=%d duplicate_path=%d); accepted current_event=%d current_hash=%d legacy_path_only=%d",
		source,
		stats.Suppressed,
		stats.StaleEvent,
		stats.HashMismatch,
		stats.MissingAuthority,
		stats.LifecycleHidden,
		stats.DuplicatePath,
		stats.CurrentEvent,
		stats.CurrentHash,
		stats.LegacyPathOnly,
	)
}

type reconciledVectorCandidate struct {
	row      map[string]any
	priority int
	class    string
	order    int
}

// reconcileVectorRows performs one bounded in-memory authority pass over an
// already-bounded vector result set. Vector projections never become authority.
func (s *server) reconcileVectorRows(request map[string]any, rows []map[string]any) ([]map[string]any, int) {
	filtered, stats := s.reconcileVectorRowsDetailed(request, rows)
	return filtered, stats.Suppressed
}

func (s *server) reconcileVectorRowsDetailed(request map[string]any, rows []map[string]any) ([]map[string]any, vectorReconcileStats) {
	stats := vectorReconcileStats{}
	if len(rows) == 0 {
		return rows, stats
	}
	if s == nil || s.memoryStore == nil {
		stats.Suppressed = len(rows)
		stats.MissingAuthority = len(rows)
		return []map[string]any{}, stats
	}
	// Explicit vector-only deployments have no local lifecycle authority to
	// reconcile. Missing authority in an enabled deployment still fails closed.
	if !s.memoryStore.isEnabled() {
		return rows, stats
	}
	includeCold := requestIncludesColdMemory(request)
	includeEphemeral := requestIncludesEphemeralMemory(request)
	chosen := map[string]reconciledVectorCandidate{}
	s.memoryStore.mu.RLock()
	defer s.memoryStore.mu.RUnlock()
	for order, row := range rows {
		if row == nil {
			continue
		}
		key := memoryStoreKey(anyToString(row["project"]), anyToString(row["file"]))
		state, ok := s.memoryStore.currentState[key]
		if !ok || state.Tombstone {
			stats.Suppressed++
			stats.MissingAuthority++
			continue
		}
		lifecycle := normalizeMemoryLifecycle(state.Entry.Lifecycle)
		tier := normalizeMemoryStorageTier(state.Entry.StorageTier)
		if !shouldSurfaceMemoryLifecycle(lifecycle, includeEphemeral) ||
			(!includeCold && (tier == "deep" || tier == "retired")) {
			stats.Suppressed++
			stats.LifecycleHidden++
			continue
		}
		projectedEventID := strings.TrimSpace(anyToString(row["event_id"]))
		currentEventID := strings.TrimSpace(state.Entry.EventID)
		projectedHash := normalizeProjectedContentHash(anyToString(row["content_hash"]))
		currentHash := normalizeProjectedContentHash(state.Entry.ContentHash)
		authorityClass := "legacy_path_only"
		priority := 1
		switch {
		case projectedEventID != "":
			if currentEventID == "" || projectedEventID != currentEventID {
				stats.Suppressed++
				stats.StaleEvent++
				continue
			}
			authorityClass = "current_event"
			priority = 3
		case projectedHash != "":
			if currentHash == "" || projectedHash != currentHash {
				stats.Suppressed++
				stats.HashMismatch++
				continue
			}
			authorityClass = "current_hash"
			priority = 2
		}
		resolved := cloneAnyMap(row)
		resolved["project"] = state.Entry.Project
		resolved["file"] = state.Entry.FileName
		resolved["summary"] = state.Entry.Summary
		resolved["topic_path"] = state.Entry.TopicPath
		resolved["event_id"] = state.Entry.EventID
		resolved["content_hash"] = state.Entry.ContentHash
		resolved["lifecycle"] = lifecycle
		resolved["storage_tier"] = tier
		resolved["legal_hold"] = state.LegalHold
		resolved["projection_authority"] = authorityClass
		candidate := reconciledVectorCandidate{
			row:      resolved,
			priority: priority,
			class:    authorityClass,
			order:    order,
		}
		if existing, exists := chosen[key]; exists {
			stats.Suppressed++
			stats.DuplicatePath++
			if candidate.priority > existing.priority {
				candidate.order = existing.order
				chosen[key] = candidate
			}
			continue
		}
		chosen[key] = candidate
	}
	candidates := make([]reconciledVectorCandidate, 0, len(chosen))
	for _, candidate := range chosen {
		candidates = append(candidates, candidate)
		switch candidate.class {
		case "current_event":
			stats.CurrentEvent++
		case "current_hash":
			stats.CurrentHash++
		default:
			stats.LegacyPathOnly++
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].order < candidates[j].order
	})
	filtered := make([]map[string]any, 0, len(candidates))
	for _, candidate := range candidates {
		filtered = append(filtered, candidate.row)
	}
	return filtered, stats
}
