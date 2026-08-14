package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const exactStateIndexSchemaID = "contextlattice_exact_state_index.v1"

type exactStateIndex struct {
	SchemaID  string   `json:"schema_id"`
	Paths     []string `json:"paths"`
	UpdatedAt string   `json:"updated_at"`
}

func (m *memoryStore) loadExactStateIndex() error {
	if m == nil || !m.isConfigured() {
		return nil
	}
	fence, err := m.acquireMemoryEdgeLogFenceOptional()
	if err != nil {
		return err
	}
	if fence != nil {
		defer fence.release()
	}
	return m.loadExactStateIndexWithFenceLocked(fence)
}

func (m *memoryStore) loadExactStateIndexWithFenceLocked(fence *memoryEdgeLogFenceToken) error {
	if m == nil || !m.isConfigured() {
		return nil
	}
	if err := requireMemoryEdgeLogFenceOptional(m, fence); err != nil {
		return err
	}
	raw, err := readOwnerOnlyBoundedFile(m.policy.exactStateIndexPath, memoryExactStateIndexMaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		// Recheck while the common fence is held immediately before creating an
		// empty index. A concurrent registration can never pass this fence, and a
		// path replacement is therefore either observed here or rejected by the
		// descriptor-bound bounded read.
		raw, err = readOwnerOnlyBoundedFile(m.policy.exactStateIndexPath, memoryExactStateIndexMaxBytes)
		if err == nil {
			return m.loadExactStateIndexBytesWithFenceLocked(raw, fence)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("recheck exact state index: %w", err)
		}
		if err := ensureOwnerOnlyDirectory(filepath.Dir(m.policy.exactStateIndexPath), true); err != nil {
			return fmt.Errorf("create exact state index directory: %w", err)
		}
		if err := m.persistExactStateIndexLocked(map[string]struct{}{}); err != nil {
			return fmt.Errorf("initialize exact state index: %w", err)
		}
		m.mu.Lock()
		m.exactStatePaths = map[string]struct{}{}
		m.mu.Unlock()
		m.exactStateCount.Store(0)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read exact state index: %w", err)
	}
	return m.loadExactStateIndexBytesWithFenceLocked(raw, fence)
}

func (m *memoryStore) loadExactStateIndexBytesWithFenceLocked(raw []byte, fence *memoryEdgeLogFenceToken) error {
	if err := requireMemoryEdgeLogFenceOptional(m, fence); err != nil {
		return err
	}
	var index exactStateIndex
	if err := json.Unmarshal(raw, &index); err != nil {
		return errors.New("exact state index is invalid")
	}
	if index.SchemaID != exactStateIndexSchemaID {
		return errors.New("exact state index schema mismatch")
	}
	if len(index.Paths) > m.policy.exactStateMaxPaths {
		return errors.New("exact state index exceeds configured path limit")
	}
	next := make(map[string]struct{}, len(index.Paths))
	for _, token := range index.Paths {
		project, fileName, ok := parseMemoryStoreKeyToken(token)
		if !ok {
			return errors.New("exact state index contains an invalid path key")
		}
		cleanProject, err := sanitizeMemoryProject(project)
		if err != nil {
			return errors.New("exact state index contains an invalid project")
		}
		cleanFile, err := sanitizeMemoryFile(fileName)
		if err != nil {
			return errors.New("exact state index contains an invalid file")
		}
		next[memoryStoreKey(cleanProject, cleanFile)] = struct{}{}
	}
	m.mu.Lock()
	m.exactStatePaths = next
	m.exactStateCount.Store(int64(len(next)))
	m.mu.Unlock()
	return ensureOwnerOnlyFile(m.policy.exactStateIndexPath)
}

func memoryExactStateIndexPayload(paths map[string]struct{}) ([]byte, error) {
	rows := make([]string, 0, len(paths))
	for key := range paths {
		rows = append(rows, key)
	}
	sort.Strings(rows)
	payload, err := json.MarshalIndent(exactStateIndex{
		SchemaID:  exactStateIndexSchemaID,
		Paths:     rows,
		UpdatedAt: nowUTCISO(),
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	if int64(len(payload))+1 > memoryExactStateIndexMaxBytes {
		return nil, fmt.Errorf("%w: exact state index bytes=%d cap=%d", errMemoryEdgeLogOversized, len(payload)+1, memoryExactStateIndexMaxBytes)
	}
	return payload, nil
}

func (m *memoryStore) persistExactStateIndexLocked(paths map[string]struct{}) error {
	payload, err := memoryExactStateIndexPayload(paths)
	if err != nil {
		return err
	}
	return writeOwnerOnlyDurableAtomicFile(m.policy.exactStateIndexPath, payload, true)
}

type memoryExactStateEdgeSnapshot struct {
	edges       map[string]memoryEdgeEntry
	ordinals    map[string]int64
	edgeOrder   []string
	adjacency   map[string]map[string]struct{}
	nextOrdinal int64
}

func (m *memoryStore) captureExactStateEdgeSnapshotLocked(key string) memoryExactStateEdgeSnapshot {
	snapshot := memoryExactStateEdgeSnapshot{
		edges:       map[string]memoryEdgeEntry{},
		ordinals:    map[string]int64{},
		edgeOrder:   append([]string(nil), m.edgeOrder...),
		adjacency:   map[string]map[string]struct{}{},
		nextOrdinal: m.nextEdgeOrdinal,
	}
	for edgeID := range m.edgeAdjacency[key] {
		if edge, exists := m.edges[edgeID]; exists {
			snapshot.edges[edgeID] = edge
			snapshot.ordinals[edgeID] = m.edgeOrdinal[edgeID]
			for _, memoryID := range []string{edge.SourceID, edge.TargetID} {
				_, _, _, endpointKey, err := canonicalMemoryID(memoryID)
				if err != nil {
					continue
				}
				if _, captured := snapshot.adjacency[endpointKey]; captured {
					continue
				}
				if adjacent := m.edgeAdjacency[endpointKey]; adjacent != nil {
					snapshot.adjacency[endpointKey] = make(map[string]struct{}, len(adjacent))
					for adjacentID := range adjacent {
						snapshot.adjacency[endpointKey][adjacentID] = struct{}{}
					}
				}
			}
		}
	}
	return snapshot
}

func (m *memoryStore) restoreExactStateEdgeSnapshotLocked(snapshot memoryExactStateEdgeSnapshot) {
	if m == nil {
		return
	}
	if len(snapshot.edges) > 0 {
		if m.edges == nil {
			m.edges = map[string]memoryEdgeEntry{}
		}
		if m.edgeOrdinal == nil {
			m.edgeOrdinal = map[string]int64{}
		}
	}
	if len(snapshot.adjacency) > 0 && m.edgeAdjacency == nil {
		m.edgeAdjacency = map[string]map[string]struct{}{}
	}
	for edgeID, edge := range snapshot.edges {
		m.edges[edgeID] = edge
		m.edgeOrdinal[edgeID] = snapshot.ordinals[edgeID]
	}
	m.edgeOrder = append([]string(nil), snapshot.edgeOrder...)
	m.nextEdgeOrdinal = snapshot.nextOrdinal
	for key, adjacent := range snapshot.adjacency {
		if adjacent == nil {
			delete(m.edgeAdjacency, key)
			continue
		}
		m.edgeAdjacency[key] = make(map[string]struct{}, len(adjacent))
		for edgeID := range adjacent {
			m.edgeAdjacency[key][edgeID] = struct{}{}
		}
	}
}

func (m *memoryStore) registerExactStatePath(project string, fileName string) error {
	if m == nil || !m.isConfigured() {
		return errors.New("go memory store is disabled")
	}
	cleanProject, err := sanitizeMemoryProject(project)
	if err != nil {
		return err
	}
	cleanFile, err := sanitizeMemoryFile(fileName)
	if err != nil {
		return err
	}
	key := memoryStoreKey(cleanProject, cleanFile)
	fence, err := m.acquireMemoryEdgeLogFenceOptional()
	if err != nil {
		return err
	}
	if fence != nil {
		defer fence.release()
	}
	unlockPath := m.lockMemoryPath(key)
	defer unlockPath()
	return m.registerExactStatePathWithFenceLocked(cleanProject, cleanFile, fence)
}

// registerExactStatePathLocked is intentionally unavailable to callers that
// do not hold the common edge-log writer fence.  Exact registration changes
// both the semantic projection and durable graph eligibility, so it must be
// ordered fence -> canonical path lock everywhere.
func (m *memoryStore) registerExactStatePathLocked(project string, fileName string) error {
	return errMemoryEdgeLogWriterFenceRequired
}

func (m *memoryStore) registerExactStatePathWithFenceLocked(project string, fileName string, fence *memoryEdgeLogFenceToken) error {
	if err := requireMemoryEdgeLogFenceOptional(m, fence); err != nil {
		return err
	}
	key := memoryStoreKey(project, fileName)
	if key == "::" {
		return errors.New("exact state path is invalid")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.exactStatePaths[key]; exists {
		return nil
	}
	if len(m.exactStatePaths) >= m.policy.exactStateMaxPaths {
		return errors.New("exact state path index is full")
	}
	next := make(map[string]struct{}, len(m.exactStatePaths)+1)
	for existing := range m.exactStatePaths {
		next[existing] = struct{}{}
	}
	next[key] = struct{}{}
	indexPayload, err := memoryExactStateIndexPayload(next)
	if err != nil {
		return fmt.Errorf("encode exact state path index: %w", err)
	}
	previousExactPaths := m.exactStatePaths
	previousExactCount := len(previousExactPaths)
	// Initialize the index maps without creating the candidate project before
	// the rollback snapshot. A pre-marker failure must restore an exact no-op,
	// including a project that had no prior semantic index entry.
	m.ensureCurrentKeyIndexLocked()
	snapshot := m.captureCurrentStateMutationSnapshotLocked(key, project, m.latestTopic[key])
	edgeSnapshot := m.captureExactStateEdgeSnapshotLocked(key)
	m.ensureCurrentProjectIndexLocked(project)
	m.exactStatePaths = next
	m.exactStateCount.Store(int64(len(next)))
	delete(m.latestTopic, key)
	m.removeCurrentKeyLocked(project, key)
	delete(m.latestHash, key)
	m.removeMemoryEdgesForKeyLocked(key)
	filtered := m.recent[:0]
	for _, entry := range m.recent {
		if memoryStoreKey(entry.Project, entry.FileName) != key {
			filtered = append(filtered, entry)
		}
	}
	m.recent = filtered
	m.rollupCache = map[string]topicRollupCacheEntry{}
	if err := m.persistCurrentStateTransactionWithExactStateLocked(nil, project, 0, indexPayload); err != nil {
		if !errors.Is(err, errMemoryCurrentStateTransactionCommitted) {
			m.restoreCurrentStateMutationSnapshotLocked(snapshot)
			m.restoreExactStateEdgeSnapshotLocked(edgeSnapshot)
			m.exactStatePaths = previousExactPaths
			m.exactStateCount.Store(int64(previousExactCount))
		}
		return fmt.Errorf("persist exact state path transaction: %w", err)
	}
	return nil
}

func (m *memoryStore) isExactStatePath(project string, fileName string) bool {
	ok, err := m.isExactStatePathContext(context.Background(), project, fileName)
	if err != nil {
		// This compatibility predicate is used by policy classifiers which
		// cannot return an error.  Treat an unreadable/invalid authority index
		// as exact-state so callers fail closed instead of admitting a write.
		return true
	}
	return ok
}

func (m *memoryStore) isExactStatePathWithFence(project string, fileName string, fence *memoryEdgeLogFenceToken) bool {
	ok, err := m.isExactStatePathWithFenceChecked(project, fileName, fence)
	if err != nil {
		return true
	}
	return ok
}

func (m *memoryStore) isExactStatePathContext(ctx context.Context, project string, fileName string) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if m == nil {
		return false, nil
	}
	fence, err := m.acquireMemoryEdgeLogFenceOptionalContext(ctx)
	if err != nil {
		return false, err
	}
	if fence != nil {
		defer fence.release()
	}
	return m.isExactStatePathWithFenceChecked(project, fileName, fence)
}

func (m *memoryStore) isExactStatePathWithFenceChecked(project string, fileName string, fence *memoryEdgeLogFenceToken) (bool, error) {
	if m == nil {
		return false, nil
	}
	if err := requireMemoryEdgeLogFenceOptional(m, fence); err != nil {
		return false, err
	}
	cleanProject, err := sanitizeMemoryProject(project)
	if err != nil {
		return false, err
	}
	cleanFile, err := sanitizeMemoryFile(fileName)
	if err != nil {
		return false, err
	}
	project, fileName = cleanProject, cleanFile
	key := memoryStoreKey(project, fileName)
	m.mu.RLock()
	_, ok := m.exactStatePaths[key]
	m.mu.RUnlock()
	return ok, nil
}

func (m *memoryStore) exactStatePathsSnapshot() (map[string]struct{}, error) {
	return m.exactStatePathsSnapshotContext(context.Background())
}

func (m *memoryStore) exactStatePathsSnapshotContext(ctx context.Context) (map[string]struct{}, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	fence, err := m.acquireMemoryEdgeLogFenceOptionalContext(ctx)
	if err != nil {
		return nil, err
	}
	if fence != nil {
		defer fence.release()
	}
	return m.exactStatePathsSnapshotWithFenceChecked(fence)
}

func (m *memoryStore) exactStatePathsSnapshotWithFence(fence *memoryEdgeLogFenceToken) map[string]struct{} {
	out, err := m.exactStatePathsSnapshotWithFenceChecked(fence)
	if err != nil {
		return nil
	}
	return out
}

func (m *memoryStore) exactStatePathsSnapshotWithFenceChecked(fence *memoryEdgeLogFenceToken) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	if m == nil {
		return out, nil
	}
	if err := requireMemoryEdgeLogFenceOptional(m, fence); err != nil {
		return nil, err
	}
	m.mu.RLock()
	for key := range m.exactStatePaths {
		out[key] = struct{}{}
	}
	m.mu.RUnlock()
	return out, nil
}

func (s *server) exactStateSourceRequest(baseRequest map[string]any, source string) map[string]any {
	request := cloneAnyMap(baseRequest)
	switch strings.TrimSpace(strings.ToLower(source)) {
	case sourceQdrant, sourceWeaviate, sourcePgvector, sourceMongoRaw, sourceTopicRollup, sourceMemoryBank:
	default:
		return request
	}
	requested := clampInt(anyToInt(baseRequest["limit"], 10), 1, 100)
	reserve := clampInt(envInt("GO_EXACT_STATE_SOURCE_OVERFETCH_RESERVE", 8), 0, 32)
	exactStateCount := 0
	if s != nil && s.memoryStore != nil {
		exactStateCount = int(s.memoryStore.exactStateCount.Load())
	}
	request["limit"] = clampInt(requested+reserve+exactStateCount, requested, 100)
	return request
}

func (s *server) exactStateFanoutSkipStatus(item normalizedWrite) string {
	status, err := s.exactStateFanoutSkipStatusChecked(context.Background(), item)
	if err != nil {
		return "skipped_exact_state_validation"
	}
	return status
}

func (s *server) exactStateFanoutSkipStatusChecked(ctx context.Context, item normalizedWrite) (string, error) {
	if item.dataClass == dataClassRuntimeStateMirror {
		return "skipped_exact_state_mirror", nil
	}
	project, projectErr := sanitizeMemoryProject(item.project)
	fileName, fileErr := sanitizeMemoryFile(item.fileName)
	if projectErr != nil || fileErr != nil {
		return "skipped_invalid_memory_path", nil
	}
	if s != nil && s.memoryStore != nil {
		exact, err := s.memoryStore.isExactStatePathContext(ctx, project, fileName)
		if err != nil {
			return "", err
		}
		if exact {
			return "skipped_exact_state_mirror", nil
		}
	}
	return "", nil
}

func (s *server) acquireExactStateFanoutPath(item normalizedWrite) (normalizedWrite, func(), string) {
	item, release, status, _ := s.acquireExactStateFanoutPathContext(context.Background(), item)
	return item, release, status
}

func (s *server) acquireExactStateFanoutPathContext(ctx context.Context, item normalizedWrite) (normalizedWrite, func(), string, error) {
	noOp := func() {}
	if ctx == nil {
		ctx = context.Background()
	}
	if status, err := s.exactStateFanoutSkipStatusChecked(ctx, item); err != nil {
		return item, noOp, "", err
	} else if status != "" {
		return item, noOp, status, nil
	}
	if s == nil || s.memoryStore == nil {
		return item, noOp, "", nil
	}
	project, err := sanitizeMemoryProject(item.project)
	if err != nil {
		return item, noOp, "skipped_invalid_memory_path", nil
	}
	fileName, err := sanitizeMemoryFile(item.fileName)
	if err != nil {
		return item, noOp, "skipped_invalid_memory_path", nil
	}
	item.project = project
	item.fileName = fileName
	fence, err := s.memoryStore.acquireMemoryEdgeLogFenceOptionalContext(ctx)
	if err != nil {
		return item, noOp, "", err
	}
	unlock, err := s.memoryStore.lockMemoryPathContext(ctx, memoryStoreKey(project, fileName))
	if err != nil {
		if fence != nil {
			fence.release()
		}
		return item, noOp, "", err
	}
	exact, err := s.memoryStore.isExactStatePathWithFenceChecked(project, fileName, fence)
	if err != nil {
		unlock()
		if fence != nil {
			fence.release()
		}
		return item, noOp, "", err
	}
	if exact {
		unlock()
		if fence != nil {
			fence.release()
		}
		return item, noOp, "skipped_exact_state_mirror", nil
	}
	return item, func() {
		unlock()
		if fence != nil {
			fence.release()
		}
	}, "", nil
}

func exactStatePathSetContains(paths map[string]struct{}, project string, fileName string) bool {
	ok, err := exactStatePathSetContainsChecked(paths, project, fileName)
	if err != nil {
		return true
	}
	return ok
}

func exactStatePathSetContainsChecked(paths map[string]struct{}, project string, fileName string) (bool, error) {
	if paths == nil {
		return false, errors.New("exact state path set is unavailable")
	}
	cleanProject, err := sanitizeMemoryProject(strings.TrimSpace(project))
	if err != nil {
		return false, err
	}
	cleanFile, err := sanitizeMemoryFile(strings.TrimSpace(fileName))
	if err != nil {
		return false, err
	}
	_, ok := paths[memoryStoreKey(cleanProject, cleanFile)]
	return ok, nil
}

func exactStatePathSetsEqual(left, right map[string]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for key := range left {
		if _, ok := right[key]; !ok {
			return false
		}
	}
	return true
}

func (s *server) filterExactStateRows(rows []map[string]any) ([]map[string]any, int) {
	filtered, suppressed, err := s.filterExactStateRowsChecked(context.Background(), rows)
	if err != nil {
		// A caller retaining the legacy two-value API cannot report validation
		// failure.  Suppress every row so corrupted exact-state authority can
		// never leak into retrieval results.
		return nil, len(rows)
	}
	return filtered, suppressed
}

func (s *server) filterExactStateRowsChecked(ctx context.Context, rows []map[string]any) ([]map[string]any, int, error) {
	if len(rows) == 0 {
		return rows, 0, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	policy := loadWriteIngressPolicy()
	if s != nil {
		policy = s.writePolicy
	}
	filtered := make([]map[string]any, 0, len(rows))
	suppressed := 0
	for _, row := range rows {
		project := strings.TrimSpace(anyToString(row["project"]))
		fileName := strings.TrimSpace(anyToString(row["file"]))
		if fileName == "" {
			fileName = strings.TrimSpace(anyToString(row["file_name"]))
		}
		if fileName == "" {
			fileName = strings.TrimSpace(anyToString(row["path"]))
		}
		_, projectErr := sanitizeMemoryProject(project)
		_, fileErr := sanitizeMemoryFile(fileName)
		isExactState := projectErr != nil || fileErr != nil ||
			strings.EqualFold(anyToString(row["data_class"]), dataClassRuntimeStateMirror) ||
			policy.isDurableMemoryFile(normalizedWrite{project: project, fileName: fileName}) ||
			false
		if s != nil && s.memoryStore != nil && projectErr == nil && fileErr == nil {
			exact, err := s.memoryStore.isExactStatePathContext(ctx, project, fileName)
			if err != nil {
				return nil, 0, err
			}
			isExactState = isExactState || exact
		}
		if isExactState {
			suppressed++
			continue
		}
		filtered = append(filtered, row)
	}
	return filtered, suppressed, nil
}
