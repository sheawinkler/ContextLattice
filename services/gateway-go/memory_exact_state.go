package main

import (
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
	raw, err := os.ReadFile(m.policy.exactStateIndexPath)
	if errors.Is(err, os.ErrNotExist) {
		if err := ensureOwnerOnlyDirectory(filepath.Dir(m.policy.exactStateIndexPath), true); err != nil {
			return fmt.Errorf("create exact state index directory: %w", err)
		}
		if err := m.persistExactStateIndexLocked(map[string]struct{}{}); err != nil {
			return fmt.Errorf("initialize exact state index: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read exact state index: %w", err)
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
	if m.exactStatePaths == nil {
		m.exactStatePaths = map[string]struct{}{}
	}
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
		m.exactStatePaths[memoryStoreKey(cleanProject, cleanFile)] = struct{}{}
	}
	return ensureOwnerOnlyFile(m.policy.exactStateIndexPath)
}

func (m *memoryStore) persistExactStateIndexLocked(paths map[string]struct{}) error {
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
		return err
	}
	return writeOwnerOnlyDurableAtomicFile(m.policy.exactStateIndexPath, payload, true)
}

func (m *memoryStore) registerExactStatePath(project string, fileName string) error {
	if m == nil || !m.isConfigured() {
		return errors.New("go memory store is disabled")
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
	if err := m.persistExactStateIndexLocked(next); err != nil {
		return fmt.Errorf("persist exact state path index: %w", err)
	}
	m.exactStatePaths = next
	delete(m.latestTopic, key)
	delete(m.latestHash, key)
	filtered := m.recent[:0]
	for _, entry := range m.recent {
		if memoryStoreKey(entry.Project, entry.FileName) != key {
			filtered = append(filtered, entry)
		}
	}
	m.recent = filtered
	m.rollupCache = map[string]topicRollupCacheEntry{}
	return nil
}

func (m *memoryStore) isExactStatePath(project string, fileName string) bool {
	if m == nil {
		return false
	}
	key := memoryStoreKey(project, fileName)
	m.mu.RLock()
	_, ok := m.exactStatePaths[key]
	m.mu.RUnlock()
	return ok
}

func (m *memoryStore) exactStatePathsSnapshot() map[string]struct{} {
	out := map[string]struct{}{}
	if m == nil {
		return out
	}
	m.mu.RLock()
	for key := range m.exactStatePaths {
		out[key] = struct{}{}
	}
	m.mu.RUnlock()
	return out
}

func exactStatePathSetContains(paths map[string]struct{}, project string, fileName string) bool {
	_, ok := paths[memoryStoreKey(strings.TrimSpace(project), strings.TrimSpace(fileName))]
	return ok
}

func (s *server) filterExactStateRows(rows []map[string]any) ([]map[string]any, int) {
	if len(rows) == 0 {
		return rows, 0
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
		isExactState := strings.EqualFold(anyToString(row["data_class"]), dataClassRuntimeStateMirror) ||
			policy.isDurableMemoryFile(normalizedWrite{project: project, fileName: fileName}) ||
			(s != nil && s.memoryStore != nil && s.memoryStore.isExactStatePath(project, fileName))
		if isExactState {
			suppressed++
			continue
		}
		filtered = append(filtered, row)
	}
	return filtered, suppressed
}
