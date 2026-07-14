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
	"sort"
	"strings"
)

const memoryEdgeSource = "memory_edges"

type memoryEdgeEntry struct {
	EdgeID     string         `json:"edge_id"`
	SourceID   string         `json:"source_id"`
	TargetID   string         `json:"target_id"`
	Relation   string         `json:"relation"`
	Project    string         `json:"project,omitempty"`
	TopicPath  string         `json:"topic_path,omitempty"`
	Confidence float64        `json:"confidence"`
	Provenance map[string]any `json:"provenance,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	AgentID    string         `json:"agent_id,omitempty"`
	SessionID  string         `json:"session_id,omitempty"`
	Lifecycle  string         `json:"lifecycle,omitempty"`
	CreatedAt  string         `json:"created_at"`
	Source     string         `json:"source,omitempty"`
}

type memoryEdgeQuery struct {
	MemoryID         string
	Project          string
	Relation         string
	Direction        string
	Limit            int
	IncludeEphemeral bool
}

type memoryGraphNeighborQuery struct {
	MemoryID         string
	Limit            int
	Relation         string
	Direction        string
	IncludeEphemeral bool
	TopicPath        string
}

type memoryGraphEdgeBackend interface {
	upsertMemoryEdge(ctx context.Context, edge memoryEdgeEntry) (memoryEdgeEntry, error)
	listMemoryEdges(ctx context.Context, query memoryEdgeQuery) ([]memoryEdgeEntry, error)
	memoryGraphNeighbors(ctx context.Context, query memoryGraphNeighborQuery) ([]map[string]any, error)
}

func canonicalMemoryID(raw string) (string, string, string, string, error) {
	project, fileName, err := splitEngineMemoryID(raw)
	if err != nil {
		return "", "", "", "", err
	}
	project, err = sanitizeMemoryProject(project)
	if err != nil {
		return "", "", "", "", err
	}
	fileName, err = sanitizeMemoryFile(fileName)
	if err != nil {
		return "", "", "", "", err
	}
	memoryID := project + "::" + fileName
	return project, fileName, memoryID, memoryStoreKey(project, fileName), nil
}

func normalizeMemoryEdgeRelation(raw string) (string, error) {
	relation := strings.TrimSpace(strings.ToLower(raw))
	relation = strings.ReplaceAll(relation, "-", "_")
	relation = strings.ReplaceAll(relation, " ", "_")
	relation = strings.Trim(relation, "_")
	if relation == "" {
		return "", errors.New("relation is required")
	}
	if len(relation) > 64 {
		return "", errors.New("relation must be 64 characters or fewer")
	}
	for _, ch := range relation {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == ':' || ch == '.' {
			continue
		}
		return "", errors.New("relation may contain only letters, digits, underscore, colon, or dot")
	}
	return relation, nil
}

func normalizeMemoryEdgeDirection(raw string) string {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "out", "outbound", "source", "from":
		return "out"
	case "in", "inbound", "target", "to":
		return "in"
	default:
		return "both"
	}
}

func deterministicMemoryEdgeID(sourceID string, relation string, targetID string) string {
	return "edge_" + sha256Hex(sourceID + "\x00" + relation + "\x00" + targetID)[:32]
}

func normalizeMemoryEdgePayload(payload map[string]any) (memoryEdgeEntry, error) {
	sourceProject, sourceFile, sourceID, _, err := canonicalMemoryID(firstNonEmptyStrings(
		anyToString(payload["source_id"]),
		anyToString(payload["sourceId"]),
	))
	if err != nil {
		return memoryEdgeEntry{}, fmt.Errorf("source_id: %w", err)
	}
	_, _, targetID, _, err := canonicalMemoryID(firstNonEmptyStrings(
		anyToString(payload["target_id"]),
		anyToString(payload["targetId"]),
	))
	if err != nil {
		return memoryEdgeEntry{}, fmt.Errorf("target_id: %w", err)
	}
	if sourceID == targetID {
		return memoryEdgeEntry{}, errors.New("source_id and target_id must differ")
	}
	relation, err := normalizeMemoryEdgeRelation(anyToString(payload["relation"]))
	if err != nil {
		return memoryEdgeEntry{}, err
	}
	confidence := 1.0
	if _, exists := payload["confidence"]; exists {
		confidence = anyToFloat64(payload["confidence"], 1.0)
		if confidence < 0 || confidence > 1 {
			return memoryEdgeEntry{}, errors.New("confidence must be between 0 and 1")
		}
	}
	project := strings.TrimSpace(anyToString(payload["project"]))
	if project == "" {
		project = sourceProject
	}
	project, err = sanitizeMemoryProject(project)
	if err != nil {
		return memoryEdgeEntry{}, fmt.Errorf("project: %w", err)
	}
	topicPath := sanitizeTopicPath(anyToString(payload["topic_path"]), sourceFile)
	createdAt := strings.TrimSpace(anyToString(payload["created_at"]))
	if createdAt == "" {
		createdAt = nowUTCISO()
	}
	lifecycle := normalizeMemoryLifecycle(anyToString(payload["lifecycle"]))
	return memoryEdgeEntry{
		EdgeID:     deterministicMemoryEdgeID(sourceID, relation, targetID),
		SourceID:   sourceID,
		TargetID:   targetID,
		Relation:   relation,
		Project:    project,
		TopicPath:  topicPath,
		Confidence: confidence,
		Provenance: mapFromAny(payload["provenance"]),
		Metadata:   mapFromAny(payload["metadata"]),
		AgentID:    strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["agent_id"]), anyToString(payload["agentId"]))),
		SessionID:  strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["session_id"]), anyToString(payload["sessionId"]))),
		Lifecycle:  lifecycle,
		CreatedAt:  createdAt,
		Source:     memoryEdgeSource,
	}, nil
}

func (edge memoryEdgeEntry) normalized() (memoryEdgeEntry, error) {
	sourceProject, sourceFile, sourceID, _, err := canonicalMemoryID(edge.SourceID)
	if err != nil {
		return memoryEdgeEntry{}, fmt.Errorf("source_id: %w", err)
	}
	_, _, targetID, _, err := canonicalMemoryID(edge.TargetID)
	if err != nil {
		return memoryEdgeEntry{}, fmt.Errorf("target_id: %w", err)
	}
	if sourceID == targetID {
		return memoryEdgeEntry{}, errors.New("source_id and target_id must differ")
	}
	relation, err := normalizeMemoryEdgeRelation(edge.Relation)
	if err != nil {
		return memoryEdgeEntry{}, err
	}
	project := strings.TrimSpace(edge.Project)
	if project == "" {
		project = sourceProject
	}
	project, err = sanitizeMemoryProject(project)
	if err != nil {
		return memoryEdgeEntry{}, fmt.Errorf("project: %w", err)
	}
	if edge.Confidence < 0 || edge.Confidence > 1 {
		return memoryEdgeEntry{}, errors.New("confidence must be between 0 and 1")
	}
	edge.SourceID = sourceID
	edge.TargetID = targetID
	edge.Relation = relation
	edge.Project = project
	edge.TopicPath = sanitizeTopicPath(edge.TopicPath, sourceFile)
	edge.Lifecycle = normalizeMemoryLifecycle(edge.Lifecycle)
	if strings.TrimSpace(edge.CreatedAt) == "" {
		edge.CreatedAt = nowUTCISO()
	}
	edge.EdgeID = deterministicMemoryEdgeID(edge.SourceID, edge.Relation, edge.TargetID)
	if strings.TrimSpace(edge.Source) == "" {
		edge.Source = memoryEdgeSource
	}
	return edge, nil
}

func (edge memoryEdgeEntry) toMap() map[string]any {
	row := map[string]any{
		"edge_id":    edge.EdgeID,
		"source_id":  edge.SourceID,
		"target_id":  edge.TargetID,
		"relation":   edge.Relation,
		"project":    edge.Project,
		"topic_path": edge.TopicPath,
		"confidence": edge.Confidence,
		"lifecycle":  normalizeMemoryLifecycle(edge.Lifecycle),
		"created_at": edge.CreatedAt,
		"source":     edge.Source,
	}
	if len(edge.Provenance) > 0 {
		row["provenance"] = edge.Provenance
	}
	if len(edge.Metadata) > 0 {
		row["metadata"] = edge.Metadata
	}
	if strings.TrimSpace(edge.AgentID) != "" {
		row["agent_id"] = edge.AgentID
	}
	if strings.TrimSpace(edge.SessionID) != "" {
		row["session_id"] = edge.SessionID
	}
	return row
}

func (m *memoryStore) loadEdges() error {
	if m == nil || !m.isConfigured() {
		return nil
	}
	file, err := os.Open(m.policy.edgePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open memory graph edge log: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	buffer := make([]byte, 0, 1024*64)
	scanner.Buffer(buffer, 1024*1024)
	lines := make([]string, 0, minInt(m.policy.edgeStartupMaxLines, 4096))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lines = append(lines, line)
		if m.policy.edgeStartupMaxLines > 0 && len(lines) > m.policy.edgeStartupMaxLines {
			over := len(lines) - m.policy.edgeStartupMaxLines
			lines = lines[over:]
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan memory graph edge log: %w", err)
	}

	loaded := 0
	skippedPolicy := 0
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, line := range lines {
		var edge memoryEdgeEntry
		if err := json.Unmarshal([]byte(line), &edge); err != nil {
			continue
		}
		if normalized, err := edge.normalized(); err == nil {
			if excluded, _ := m.memoryGraphEdgeExcluded(normalized); excluded {
				skippedPolicy += 1
				continue
			}
			m.recordEdgeLocked(normalized)
			loaded += 1
		}
	}
	if loaded > 0 {
		logMemoryEdgeLoad(loaded, len(lines), skippedPolicy, m.policy.edgeStartupMaxLines)
	}
	return nil
}

func logMemoryEdgeLoad(loaded int, scanned int, skippedPolicy int, cap int) {
	if loaded <= 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "gateway-go memory graph edge startup load: scanned=%d loaded=%d skipped_policy=%d cap=%d\n", scanned, loaded, skippedPolicy, cap)
}

func (m *memoryStore) recordEdgeLocked(edge memoryEdgeEntry) {
	if m == nil {
		return
	}
	if _, exists := m.edges[edge.EdgeID]; !exists {
		m.edgeOrder = append(m.edgeOrder, edge.EdgeID)
		m.edgeOrdinal[edge.EdgeID] = m.nextEdgeOrdinal
		m.nextEdgeOrdinal += 1
	}
	m.edges[edge.EdgeID] = edge
	for _, memoryID := range []string{edge.SourceID, edge.TargetID} {
		_, _, _, key, err := canonicalMemoryID(memoryID)
		if err != nil {
			continue
		}
		if m.edgeAdjacency[key] == nil {
			m.edgeAdjacency[key] = map[string]struct{}{}
		}
		m.edgeAdjacency[key][edge.EdgeID] = struct{}{}
	}
	for len(m.edgeOrder) > m.policy.maxEdges {
		oldest := m.edgeOrder[0]
		m.edgeOrder = m.edgeOrder[1:]
		oldEdge, exists := m.edges[oldest]
		if !exists {
			continue
		}
		delete(m.edges, oldest)
		delete(m.edgeOrdinal, oldest)
		for _, memoryID := range []string{oldEdge.SourceID, oldEdge.TargetID} {
			_, _, _, key, err := canonicalMemoryID(memoryID)
			if err != nil {
				continue
			}
			delete(m.edgeAdjacency[key], oldest)
			if len(m.edgeAdjacency[key]) == 0 {
				delete(m.edgeAdjacency, key)
			}
		}
	}
}

func (m *memoryStore) appendEdge(edge memoryEdgeEntry) error {
	if m == nil || !m.isEnabled() {
		return nil
	}
	payload, err := json.Marshal(edge)
	if err != nil {
		return fmt.Errorf("encode memory graph edge: %w", err)
	}
	line := append(payload, '\n')
	file, err := openOwnerOnlyAppend(m.policy.edgePath, true)
	if err != nil {
		return fmt.Errorf("open memory graph edge append: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(line); err != nil {
		return fmt.Errorf("append memory graph edge: %w", err)
	}
	return nil
}

func (m *memoryStore) pruneVolatileMemoryGraphEdges(ctx context.Context, dryRun bool) (map[string]any, error) {
	if m == nil || !m.isEnabled() {
		return nil, errors.New("go memory store is disabled")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	path := m.policy.edgePath
	beforeBytes := int64(0)
	if stat, err := os.Stat(path); err == nil {
		beforeBytes = stat.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{"ok": true, "dry_run": dryRun, "edge_store_ref": ownerOnlyStoreRef("memory_edges"), "scanned": 0, "kept": 0, "skipped_volatile": 0, "skipped_invalid": 0, "skipped_duplicate": 0, "bytes_before": 0, "bytes_after": 0}, nil
		}
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1024*64), 1024*1024)
	kept := []memoryEdgeEntry{}
	seen := map[string]int{}
	scanned := 0
	skippedVolatile := 0
	skippedInvalid := 0
	skippedDuplicate := 0
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		scanned += 1
		var edge memoryEdgeEntry
		if err := json.Unmarshal([]byte(line), &edge); err != nil {
			skippedInvalid += 1
			continue
		}
		normalized, err := edge.normalized()
		if err != nil {
			skippedInvalid += 1
			continue
		}
		if excluded, _ := m.memoryGraphEdgeExcluded(normalized); excluded {
			skippedVolatile += 1
			continue
		}
		if idx, exists := seen[normalized.EdgeID]; exists {
			kept[idx] = normalized
			skippedDuplicate += 1
			continue
		}
		seen[normalized.EdgeID] = len(kept)
		kept = append(kept, normalized)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	afterBytes := beforeBytes
	if !dryRun {
		if err := ensureOwnerOnlyDirectory(filepath.Dir(path), true); err != nil {
			return nil, err
		}
		tmpPath := path + ".tmp-" + strings.ReplaceAll(nowUTCISO(), ":", "")
		out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, ownerOnlyFileMode)
		if err != nil {
			return nil, err
		}
		enc := json.NewEncoder(out)
		for _, edge := range kept {
			select {
			case <-ctx.Done():
				_ = out.Close()
				_ = os.Remove(tmpPath)
				return nil, ctx.Err()
			default:
			}
			if err := enc.Encode(edge); err != nil {
				_ = out.Close()
				_ = os.Remove(tmpPath)
				return nil, err
			}
		}
		if err := out.Close(); err != nil {
			_ = os.Remove(tmpPath)
			return nil, err
		}
		if err := os.Rename(tmpPath, path); err != nil {
			_ = os.Remove(tmpPath)
			return nil, err
		}
		if err := ensureOwnerOnlyFile(path); err != nil {
			return nil, err
		}
		if stat, err := os.Stat(path); err == nil {
			afterBytes = stat.Size()
		}
		m.mu.Lock()
		m.edges = map[string]memoryEdgeEntry{}
		m.edgeOrder = []string{}
		m.edgeOrdinal = map[string]int64{}
		m.nextEdgeOrdinal = 0
		m.edgeAdjacency = map[string]map[string]struct{}{}
		for _, edge := range kept {
			m.recordEdgeLocked(edge)
		}
		m.mu.Unlock()
	}

	return map[string]any{
		"ok":                true,
		"dry_run":           dryRun,
		"edge_store_ref":    ownerOnlyStoreRef("memory_edges"),
		"scanned":           scanned,
		"kept":              len(kept),
		"skipped_volatile":  skippedVolatile,
		"skipped_invalid":   skippedInvalid,
		"skipped_duplicate": skippedDuplicate,
		"bytes_before":      beforeBytes,
		"bytes_after":       afterBytes,
	}, nil
}

func (m *memoryStore) upsertMemoryEdge(ctx context.Context, edge memoryEdgeEntry) (memoryEdgeEntry, error) {
	if m == nil || !m.isEnabled() {
		return memoryEdgeEntry{}, errors.New("go memory store is disabled")
	}
	select {
	case <-ctx.Done():
		return memoryEdgeEntry{}, ctx.Err()
	default:
	}
	normalized, err := edge.normalized()
	if err != nil {
		return memoryEdgeEntry{}, err
	}
	if excluded, reason := m.memoryGraphEdgeExcluded(normalized); excluded {
		return memoryEdgeEntry{}, fmt.Errorf("memory edge rejected by graph artifact policy: %s", reason)
	}
	m.mu.Lock()
	m.recordEdgeLocked(normalized)
	m.mu.Unlock()
	if err := m.appendEdge(normalized); err != nil {
		return memoryEdgeEntry{}, err
	}
	return normalized, nil
}

func (m *memoryStore) listMemoryEdges(ctx context.Context, query memoryEdgeQuery) ([]memoryEdgeEntry, error) {
	if m == nil || !m.isEnabled() {
		return []memoryEdgeEntry{}, nil
	}
	if query.Limit < 1 {
		query.Limit = 50
	}
	query.Limit = clampInt(query.Limit, 1, m.policy.maxEdgeNeighbors)
	query.Direction = normalizeMemoryEdgeDirection(query.Direction)
	relation := strings.TrimSpace(query.Relation)
	if relation != "" {
		var err error
		relation, err = normalizeMemoryEdgeRelation(relation)
		if err != nil {
			return nil, err
		}
	}
	project := strings.TrimSpace(query.Project)
	if project != "" {
		var err error
		project, err = sanitizeMemoryProject(project)
		if err != nil {
			return nil, err
		}
	}
	memoryID := ""
	memoryKey := ""
	if strings.TrimSpace(query.MemoryID) != "" {
		_, _, canonical, key, err := canonicalMemoryID(query.MemoryID)
		if err != nil {
			return nil, err
		}
		memoryID = canonical
		memoryKey = key
	}

	m.mu.RLock()
	var ids []string
	if memoryKey != "" {
		adjacent := m.edgeAdjacency[memoryKey]
		for edgeID := range adjacent {
			ids = append(ids, edgeID)
		}
		sort.Slice(ids, func(i, j int) bool {
			return m.edgeOrdinal[ids[i]] < m.edgeOrdinal[ids[j]]
		})
	} else {
		ids = append(ids, m.edgeOrder...)
	}
	edges := make([]memoryEdgeEntry, 0, minInt(len(ids), query.Limit))
	for i := len(ids) - 1; i >= 0 && len(edges) < query.Limit; i-- {
		select {
		case <-ctx.Done():
			m.mu.RUnlock()
			return nil, ctx.Err()
		default:
		}
		edge, exists := m.edges[ids[i]]
		if !exists {
			continue
		}
		if project != "" && !strings.EqualFold(edge.Project, project) {
			continue
		}
		if relation != "" && edge.Relation != relation {
			continue
		}
		if !query.IncludeEphemeral && !shouldSurfaceMemoryLifecycle(edge.Lifecycle, false) {
			continue
		}
		if excluded, _ := m.memoryGraphEdgeExcluded(edge); excluded {
			continue
		}
		if memoryID != "" {
			switch query.Direction {
			case "out":
				if edge.SourceID != memoryID {
					continue
				}
			case "in":
				if edge.TargetID != memoryID {
					continue
				}
			default:
				if edge.SourceID != memoryID && edge.TargetID != memoryID {
					continue
				}
			}
		}
		edges = append(edges, edge)
	}
	m.mu.RUnlock()
	return edges, nil
}

func (m *memoryStore) memoryGraphNeighbors(ctx context.Context, query memoryGraphNeighborQuery) ([]map[string]any, error) {
	edges, err := m.listMemoryEdges(ctx, memoryEdgeQuery{
		MemoryID:         query.MemoryID,
		Relation:         query.Relation,
		Direction:        query.Direction,
		Limit:            query.Limit,
		IncludeEphemeral: query.IncludeEphemeral,
	})
	if err != nil {
		return nil, err
	}
	_, _, memoryID, _, err := canonicalMemoryID(query.MemoryID)
	if err != nil {
		return nil, err
	}
	rows := make([]map[string]any, 0, len(edges))
	for _, edge := range edges {
		neighborID := edge.TargetID
		direction := "out"
		if edge.TargetID == memoryID {
			neighborID = edge.SourceID
			direction = "in"
		}
		project, fileName, canonicalNeighbor, _, err := canonicalMemoryID(neighborID)
		if err != nil {
			continue
		}
		if strings.TrimSpace(query.TopicPath) != "" {
			normalizedTopic := strings.Trim(strings.ToLower(edge.TopicPath), "/")
			filterTopic := strings.Trim(strings.ToLower(query.TopicPath), "/")
			if normalizedTopic != filterTopic && !strings.HasPrefix(normalizedTopic, filterTopic+"/") {
				continue
			}
		}
		rows = append(rows, map[string]any{
			"memory_id":      canonicalNeighbor,
			"project":        project,
			"file":           fileName,
			"topic_path":     edge.TopicPath,
			"summary":        "explicit memory edge: " + edge.SourceID + " -[" + edge.Relation + "]-> " + edge.TargetID,
			"score":          edge.Confidence,
			"source":         memoryEdgeSource,
			"source_owner":   sourceOwnerGoNative,
			"relation":       edge.Relation,
			"edge_id":        edge.EdgeID,
			"edge_direction": direction,
			"edge":           edge.toMap(),
		})
	}
	return rows, nil
}

func (s *server) memoryGraphBackend() memoryGraphEdgeBackend {
	if s == nil || s.memoryStore == nil || !s.memoryStore.isEnabled() {
		return nil
	}
	return s.memoryStore
}

func (s *server) memoryV1Edges(w http.ResponseWriter, r *http.Request) {
	incomingHeaders, ok := s.prepareAuthorizedHeaders(w, r)
	if !ok {
		return
	}
	backend := s.memoryGraphBackend()
	if backend == nil {
		if s.strictNoPythonRuntime {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "memory graph edge store unavailable"})
			return
		}
		targetPath := "/v1/memory/edges"
		if r.URL.RawQuery != "" {
			targetPath += "?" + r.URL.RawQuery
		}
		var forwardPayload map[string]any
		if r.Method == http.MethodPost {
			bodyBytes, readErr := readRequestBody(r)
			if readErr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
				return
			}
			if len(strings.TrimSpace(string(bodyBytes))) > 0 {
				parsed, parseErr := parseJSONMap(bodyBytes)
				if parseErr != nil {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": parseErr.Error()})
					return
				}
				forwardPayload = parsed
			}
		}
		response, status, err := s.backendJSONRequest(r.Context(), r.Method, targetPath, incomingHeaders, forwardPayload)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "backend unavailable", "detail": err.Error(), "backendUrl": s.backendURL})
			return
		}
		writeJSON(w, status, response)
		return
	}
	switch r.Method {
	case http.MethodPost:
		bodyBytes, err := readRequestBody(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
			return
		}
		payload, err := parseJSONMap(bodyBytes)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
			return
		}
		edge, err := normalizeMemoryEdgePayload(payload)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		edge, err = backend.upsertMemoryEdge(r.Context(), edge)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "memory edge write failed", "detail": err.Error()})
			return
		}
		response := map[string]any{"ok": true, "edge_id": edge.EdgeID, "edge": edge.toMap()}
		if strings.TrimSpace(edge.SessionID) != "" {
			session := s.recordAgentSessionEvent(edge.SessionID, "graph.edge_touched", map[string]any{
				"agent_id": edge.AgentID,
				"project":  edge.Project,
				"summary":  edge.SourceID + " " + edge.Relation + " " + edge.TargetID,
				"metadata": map[string]any{
					"endpoint":   "/v1/memory/edges",
					"edge_id":    edge.EdgeID,
					"source_id":  edge.SourceID,
					"target_id":  edge.TargetID,
					"relation":   edge.Relation,
					"confidence": edge.Confidence,
				},
			})
			if session != nil {
				response["agent_runtime"] = map[string]any{
					"session_id":          edge.SessionID,
					"memory_contribution": session["memory_contribution"],
				}
			}
		}
		writeJSON(w, http.StatusOK, response)
	case http.MethodGet:
		limit := clampInt(anyToInt(r.URL.Query().Get("limit"), 50), 1, 1000)
		query := memoryEdgeQuery{
			MemoryID:         strings.TrimSpace(r.URL.Query().Get("memory_id")),
			Project:          strings.TrimSpace(r.URL.Query().Get("project")),
			Relation:         strings.TrimSpace(r.URL.Query().Get("relation")),
			Direction:        strings.TrimSpace(r.URL.Query().Get("direction")),
			Limit:            limit,
			IncludeEphemeral: anyToBool(r.URL.Query().Get("include_ephemeral")),
		}
		edges, err := backend.listMemoryEdges(r.Context(), query)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		rows := make([]map[string]any, 0, len(edges))
		for _, edge := range edges {
			rows = append(rows, edge.toMap())
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "edges": rows, "count": len(rows), "backend": "go_memory_store"})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

func mergeNeighborRows(memoryID string, edgeRows []map[string]any, retrievalRows []any, limit int) []any {
	if limit < 1 {
		limit = 10
	}
	limit = clampInt(limit, 1, 100)
	_, _, canonicalSelf, _, _ := canonicalMemoryID(memoryID)
	out := make([]any, 0, limit)
	seen := map[string]struct{}{}
	add := func(row map[string]any, fallbackSource string) {
		if len(out) >= limit || row == nil {
			return
		}
		identity := rowIdentity(row, fallbackSource)
		if strings.EqualFold(identity, canonicalSelf) {
			return
		}
		lower := strings.ToLower(strings.TrimSpace(identity))
		if lower == "" {
			return
		}
		if _, exists := seen[lower]; exists {
			return
		}
		seen[lower] = struct{}{}
		out = append(out, row)
	}
	for _, row := range edgeRows {
		add(row, memoryEdgeSource)
	}
	for _, raw := range retrievalRows {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		add(row, strings.TrimSpace(anyToString(row["source"])))
	}
	return out
}
