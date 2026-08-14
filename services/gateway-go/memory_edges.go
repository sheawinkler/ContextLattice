package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
)

const memoryEdgeSource = "memory_edges"

type memoryReferenceBinding struct {
	SchemaVersion         string `json:"schema_version"`
	ParserVersion         string `json:"parser_version"`
	RelationSemantic      string `json:"relation_semantic"`
	SourceContentHash     string `json:"source_content_hash"`
	TargetContentHash     string `json:"target_content_hash"`
	SourceEventID         string `json:"source_event_id"`
	TargetEventID         string `json:"target_event_id"`
	SourceTopicPath       string `json:"source_topic_path"`
	TargetTopicPath       string `json:"target_topic_path"`
	SourceSessionID       string `json:"source_session_id,omitempty"`
	TargetSessionID       string `json:"target_session_id,omitempty"`
	SourceAgentID         string `json:"source_agent_id,omitempty"`
	TargetAgentID         string `json:"target_agent_id,omitempty"`
	SourceLifecycle       string `json:"source_lifecycle"`
	TargetLifecycle       string `json:"target_lifecycle"`
	SemanticDigest        string `json:"semantic_digest"`
	SourceIndexGeneration uint64 `json:"source_index_generation"`
	TargetIndexGeneration uint64 `json:"target_index_generation"`
	DocSetDigest          string `json:"doc_set_digest"`
	ExclusionPolicyDigest string `json:"exclusion_policy_digest"`
	BoundAt               string `json:"bound_at"`
}

// Index generations and digests are retained as binding custody. They are
// intentionally not used as a sole liveness gate: the current index
// generation is project-wide, so an unrelated write must not invalidate an
// otherwise unchanged source/target pair. Event and content-hash equality is
// the pair-local liveness proof; source/target updates and tombstones change
// those values and suppress the edge.

type memoryEdgeEntry struct {
	EdgeID     string                  `json:"edge_id"`
	SourceID   string                  `json:"source_id"`
	TargetID   string                  `json:"target_id"`
	Relation   string                  `json:"relation"`
	Project    string                  `json:"project,omitempty"`
	TopicPath  string                  `json:"topic_path,omitempty"`
	Confidence float64                 `json:"confidence"`
	Provenance map[string]any          `json:"provenance,omitempty"`
	Metadata   map[string]any          `json:"metadata,omitempty"`
	AgentID    string                  `json:"agent_id,omitempty"`
	SessionID  string                  `json:"session_id,omitempty"`
	Lifecycle  string                  `json:"lifecycle,omitempty"`
	CreatedAt  string                  `json:"created_at"`
	Source     string                  `json:"source,omitempty"`
	Binding    *memoryReferenceBinding `json:"binding,omitempty"`
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
	edge := memoryEdgeEntry{
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
	}
	if field, reserved := memoryGraphEdgeReservedServerNamespace(edge); reserved {
		return memoryEdgeEntry{}, fmt.Errorf("memory edge field %s uses a reserved server repair namespace", field)
	}
	return edge, nil
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
	if edge.Binding != nil {
		row["binding"] = edge.Binding
	}
	return row
}

func (m *memoryStore) memoryEdgeProjectionEligible(edge memoryEdgeEntry, closedReferenceTransactions map[string]map[string]string, fence *memoryEdgeLogFenceToken) (bool, error) {
	if transactionID := memoryReferenceTransactionIDFromEdge(edge); transactionID != "" {
		edges, closed := closedReferenceTransactions[transactionID]
		if !closed {
			return false, nil
		}
		if expected, ok := edges[edge.EdgeID]; !ok || expected != memoryReferenceEdgeDigest(edge) {
			return false, errors.New("closed reference transaction edge does not match its immutable receipt")
		}
	}
	if memoryGraphEdgeRequiresBinding(edge) {
		if !memoryReferenceBindingValid(edge.Binding) {
			reason := "unbound_promoted_relation"
			if edge.Relation == "references" {
				reason = "legacy_or_unbound_reference"
			}
			m.quarantineMemoryReferenceEdge(edge, reason)
			return false, nil
		}
		current := false
		if fence != nil || m == nil || strings.TrimSpace(m.policy.edgePath) == "" {
			current = m.referenceEdgeCurrentWithFence(edge, fence)
		} else {
			current = m.referenceEdgeCurrent(edge)
		}
		if !current {
			return false, nil
		}
	}
	if excluded, _ := m.memoryGraphEdgeExcluded(edge); excluded {
		return false, nil
	}
	return true, nil
}

func (m *memoryStore) loadEdges() error {
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
	return m.loadEdgesWithFenceLocked(fence)
}

func (m *memoryStore) loadEdgesWithFenceLocked(fence *memoryEdgeLogFenceToken) error {
	if m == nil || !m.isConfigured() {
		return nil
	}
	if err := requireMemoryEdgeLogFenceOptional(m, fence); err != nil {
		return err
	}
	closedReferenceTransactions, err := m.closedMemoryReferenceTransactions()
	if err != nil {
		return fmt.Errorf("load closed reference transactions: %w", err)
	}
	currentReferenceEdges, err := m.currentClosedMemoryReferenceEdges()
	if err != nil {
		return fmt.Errorf("load current closed reference edge sets: %w", err)
	}
	raw, stamp, err := m.readMemoryEdgeLogBytesLocked(memoryEdgeLogRecoveryCap(m, 0))
	if errors.Is(err, os.ErrNotExist) || stamp.Identity == "absent" {
		if len(currentReferenceEdges) > 0 {
			return errors.New("memory graph edge log is missing for a closed current reference transaction")
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read memory graph edge log: %w", err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
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
	exactStatePaths, err := m.exactStatePathsSnapshotWithFenceChecked(fence)
	if err != nil {
		return fmt.Errorf("snapshot exact state index for edge load: %w", err)
	}
	loadedEdges := make([]memoryEdgeEntry, 0, len(currentReferenceEdges)+len(lines))
	recordLoadedEdge := func(normalized memoryEdgeEntry) error {
		eligible, err := m.memoryEdgeProjectionEligible(normalized, closedReferenceTransactions, fence)
		if err != nil {
			return err
		}
		if !eligible || edgeReferencesExactStatePaths(exactStatePaths, normalized) {
			skippedPolicy++
			return nil
		}
		loadedEdges = append(loadedEdges, normalized)
		loaded++
		return nil
	}
	for _, edge := range currentReferenceEdges {
		normalized, err := edge.normalized()
		if err != nil || !memoryReferenceBindingValid(normalized.Binding) {
			return errors.New("closed current reference transaction contains an invalid edge")
		}
		if err := recordLoadedEdge(normalized); err != nil {
			return err
		}
	}
	for _, line := range lines {
		var edge memoryEdgeEntry
		if err := json.Unmarshal([]byte(line), &edge); err != nil {
			continue
		}
		if normalized, err := edge.normalized(); err == nil {
			if err := recordLoadedEdge(normalized); err != nil {
				return err
			}
		}
	}
	m.mu.Lock()
	for _, edge := range loadedEdges {
		m.recordEdgeLocked(edge)
	}
	m.mu.Unlock()
	if loaded > 0 {
		logMemoryEdgeLoad(loaded, len(lines), skippedPolicy, m.policy.edgeStartupMaxLines)
	}
	return nil
}

// reloadMemoryEdgesFromRawLocked rebuilds the process-local edge projection from
// a byte snapshot that was read while the edge-log writer fence was held. The
// durable log remains authoritative; this projection is only a bounded read
// cache used by same-process list/existence calls.
func (m *memoryStore) reloadMemoryEdgesFromRawLocked(raw []byte) error {
	return m.reloadMemoryEdgesFromRawInternalLocked(raw, true, nil)
}

func (m *memoryStore) reloadMemoryEdgesFromRawWithFenceLocked(raw []byte, fence *memoryEdgeLogFenceToken) error {
	if err := requireMemoryEdgeLogFence(m, fence); err != nil {
		return err
	}
	// A caller holding the common fence has already serialized exact-state
	// registration.  Do not invoke the public registration barrier while that
	// fence is held: it would attempt to acquire the same cross-process lock and
	// create a lock-order deadlock.  The final exact-state map check below is the
	// authoritative install guard.
	return m.reloadMemoryEdgesFromRawInternalLocked(raw, false, fence)
}

func (m *memoryStore) reloadMemoryEdgesFromRawInternalLocked(raw []byte, runBarrierHook bool, fence *memoryEdgeLogFenceToken) error {
	if m == nil {
		return errors.New("memory edge store is unavailable")
	}
	if int64(len(raw)) > memoryEdgeLogRecoveryCap(m, 0) {
		return fmt.Errorf("%w: projection reload bytes=%d cap=%d", errMemoryEdgeLogOversized, len(raw), memoryEdgeLogRecoveryCap(m, 0))
	}
	lines := make([]string, 0, minInt(m.policy.edgeStartupMaxLines, 4096))
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 1024*64), 1024*1024)
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
		return fmt.Errorf("scan memory graph edge log for projection reload: %w", err)
	}
	closedReferenceTransactions, err := m.closedMemoryReferenceTransactions()
	if err != nil {
		return fmt.Errorf("load closed reference transactions for projection reload: %w", err)
	}
	currentReferenceEdges, err := m.currentClosedMemoryReferenceEdges()
	if err != nil {
		return fmt.Errorf("load current closed reference edge sets for projection reload: %w", err)
	}
	loaded := make([]memoryEdgeEntry, 0, len(currentReferenceEdges)+len(lines))
	recordLoadedEdge := func(normalized memoryEdgeEntry) error {
		eligible, eligibleErr := m.memoryEdgeProjectionEligible(normalized, closedReferenceTransactions, fence)
		if eligibleErr != nil {
			return eligibleErr
		}
		if eligible {
			loaded = append(loaded, normalized)
		}
		return nil
	}
	for _, edge := range currentReferenceEdges {
		normalized, normalizeErr := edge.normalized()
		if normalizeErr != nil || !memoryReferenceBindingValid(normalized.Binding) {
			return errors.New("closed current reference transaction contains an invalid edge during projection reload")
		}
		if err := recordLoadedEdge(normalized); err != nil {
			return err
		}
	}
	for _, line := range lines {
		var edge memoryEdgeEntry
		if err := json.Unmarshal([]byte(line), &edge); err != nil {
			continue
		}
		normalized, err := edge.normalized()
		if err != nil {
			continue
		}
		if err := recordLoadedEdge(normalized); err != nil {
			return err
		}
	}

	// A registration can complete while the raw snapshot is being parsed. The
	// hook is a deterministic barrier for that race in tests; the final lock
	// below is the authority for the current exact-state policy in production.
	if runBarrierHook && m.memoryEdgeProjectionBeforeInstall != nil {
		m.memoryEdgeProjectionBeforeInstall()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.edges = map[string]memoryEdgeEntry{}
	m.edgeOrder = []string{}
	m.edgeOrdinal = map[string]int64{}
	m.nextEdgeOrdinal = 0
	m.edgeAdjacency = map[string]map[string]struct{}{}
	for _, edge := range loaded {
		if edgeReferencesExactStatePaths(m.exactStatePaths, edge) {
			continue
		}
		m.recordEdgeLocked(edge)
	}
	return nil
}

// hydrateMemoryEdgeProjection installs the exact normalized row that was just
// fsynced by the owner writer. For a closed structured transaction it restores
// that transaction's complete immutable edge set from bounded pending records.
// It intentionally does not rescan the durable log.
func (m *memoryStore) hydrateMemoryEdgeProjection(edge memoryEdgeEntry) error {
	if m == nil {
		return errors.New("memory edge store is unavailable")
	}
	fence, err := m.acquireMemoryEdgeLogFenceOptional()
	if err != nil {
		return err
	}
	if fence != nil {
		defer fence.release()
	}
	return m.hydrateMemoryEdgeProjectionWithFenceLocked(edge, fence)
}

func (m *memoryStore) hydrateMemoryEdgeProjectionWithFenceLocked(edge memoryEdgeEntry, fence *memoryEdgeLogFenceToken) error {
	if m == nil {
		return errors.New("memory edge store is unavailable")
	}
	if err := requireMemoryEdgeLogFenceOptional(m, fence); err != nil {
		return err
	}
	normalized, err := edge.normalized()
	if err != nil {
		return err
	}
	closedReferenceTransactions := map[string]map[string]string{}
	candidates := []memoryEdgeEntry{normalized}
	if transactionID := memoryReferenceTransactionIDFromEdge(normalized); transactionID != "" {
		closedReferenceTransactions, err = m.closedMemoryReferenceTransactions()
		if err != nil {
			return fmt.Errorf("load closed reference transactions for projection hydration: %w", err)
		}
		currentReferenceEdges, currentErr := m.currentClosedMemoryReferenceEdges()
		if currentErr != nil {
			return fmt.Errorf("load current closed reference edge sets for projection hydration: %w", currentErr)
		}
		candidates = candidates[:0]
		for _, current := range currentReferenceEdges {
			if memoryReferenceTransactionIDFromEdge(current) == transactionID {
				candidates = append(candidates, current)
			}
		}
		if len(candidates) == 0 {
			return nil
		}
	}
	loaded := make([]memoryEdgeEntry, 0, len(candidates))
	for _, candidate := range candidates {
		candidate, err = candidate.normalized()
		if err != nil {
			return err
		}
		eligible, eligibleErr := m.memoryEdgeProjectionEligible(candidate, closedReferenceTransactions, fence)
		if eligibleErr != nil {
			return eligibleErr
		}
		if eligible {
			loaded = append(loaded, candidate)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, candidate := range loaded {
		if edgeReferencesExactStatePaths(m.exactStatePaths, candidate) {
			continue
		}
		m.recordEdgeLocked(candidate)
	}
	return nil
}

func (m *memoryStore) invalidateMemoryEdges() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.edges = map[string]memoryEdgeEntry{}
	m.edgeOrder = []string{}
	m.edgeOrdinal = map[string]int64{}
	m.nextEdgeOrdinal = 0
	m.edgeAdjacency = map[string]map[string]struct{}{}
	m.mu.Unlock()
}

func logMemoryEdgeLoad(loaded int, scanned int, skippedPolicy int, cap int) {
	if loaded <= 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "gateway-go memory graph edge startup load: scanned=%d loaded=%d skipped_policy=%d cap=%d\n", scanned, loaded, skippedPolicy, cap)
}

func edgeReferencesExactStatePaths(paths map[string]struct{}, edge memoryEdgeEntry) bool {
	if paths == nil {
		// A nil snapshot is the compatibility wrapper's fail-closed signal for
		// an exact-state authority/fence validation failure. A valid empty
		// snapshot is always a non-nil map.
		return true
	}
	for _, memoryID := range []string{edge.SourceID, edge.TargetID} {
		project, fileName, _, _, err := canonicalMemoryID(memoryID)
		if err != nil {
			continue
		}
		if exactStatePathSetContains(paths, project, fileName) {
			return true
		}
	}
	return false
}

func (m *memoryStore) memoryEdgeExistsExact(edge memoryEdgeEntry) bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	existing, ok := m.edges[edge.EdgeID]
	m.mu.RUnlock()
	return ok && bytes.Equal(mustJSON(existing), mustJSON(edge))
}

func (m *memoryStore) removeMemoryEdgeLocked(edgeID string) {
	edge, exists := m.edges[edgeID]
	if !exists {
		return
	}
	delete(m.edges, edgeID)
	delete(m.edgeOrdinal, edgeID)
	for _, memoryID := range []string{edge.SourceID, edge.TargetID} {
		_, _, _, key, err := canonicalMemoryID(memoryID)
		if err != nil {
			continue
		}
		delete(m.edgeAdjacency[key], edgeID)
		if len(m.edgeAdjacency[key]) == 0 {
			delete(m.edgeAdjacency, key)
		}
	}
}

func (m *memoryStore) removeMemoryEdgesForKeyLocked(key string) {
	if m == nil {
		return
	}
	adjacent := m.edgeAdjacency[key]
	if len(adjacent) == 0 {
		return
	}
	edgeIDs := make([]string, 0, len(adjacent))
	for edgeID := range adjacent {
		edgeIDs = append(edgeIDs, edgeID)
	}
	for _, edgeID := range edgeIDs {
		m.removeMemoryEdgeLocked(edgeID)
	}
	filtered := m.edgeOrder[:0]
	for _, edgeID := range m.edgeOrder {
		if _, exists := m.edges[edgeID]; exists {
			filtered = append(filtered, edgeID)
		}
	}
	m.edgeOrder = filtered
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
	return m.appendEdgeContext(context.Background(), edge)
}

func (m *memoryStore) appendEdgeContext(ctx context.Context, edge memoryEdgeEntry) error {
	if m == nil || !m.isEnabled() {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	fence, err := m.acquireMemoryEdgeLogFenceOptionalContext(ctx)
	if err != nil {
		return err
	}
	if fence != nil {
		defer fence.release()
	}
	if _, _, err := m.appendMemoryEdgeLogWithFenceContextLocked(ctx, edge, true, fence); err != nil {
		return fmt.Errorf("append memory graph edge: %w", err)
	}
	return nil
}

func (m *memoryStore) appendEdgeWithFence(edge memoryEdgeEntry, syncFile bool, fence *memoryEdgeLogFenceToken) (memoryEdgeEntry, memoryEdgeLogState, error) {
	if m == nil || !m.isEnabled() {
		return memoryEdgeEntry{}, memoryEdgeLogState{}, errors.New("go memory store is disabled")
	}
	if err := requireMemoryEdgeLogFenceOptional(m, fence); err != nil {
		return memoryEdgeEntry{}, memoryEdgeLogState{}, err
	}
	return m.appendMemoryEdgeLogWithFenceLocked(edge, syncFile, fence)
}

func memoryGraphEdgeReservedServerNamespace(edge memoryEdgeEntry) (string, bool) {
	for _, fields := range []struct {
		name   string
		values map[string]any
	}{
		{name: "metadata", values: edge.Metadata},
		{name: "provenance", values: edge.Provenance},
	} {
		for key := range fields.values {
			normalized := strings.ToLower(strings.TrimSpace(key))
			if normalized == "repair" || normalized == "rollback" || strings.HasPrefix(normalized, "repair_") || strings.HasPrefix(normalized, "rollback_") {
				return fields.name + "." + key, true
			}
		}
	}
	return "", false
}

func (m *memoryStore) pruneVolatileMemoryGraphEdges(ctx context.Context, dryRun bool) (map[string]any, error) {
	if m == nil || !m.isEnabled() {
		return nil, errors.New("go memory store is disabled")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	fence, err := m.acquireMemoryEdgeLogFenceContext(ctx)
	if err != nil {
		return nil, err
	}
	defer fence.release()
	logSnapshot, err := m.snapshotMemoryEdgeLogContextLocked(ctx, 0)
	if err != nil {
		return nil, err
	}
	beforeBytes := logSnapshot.FileSize

	scanner := bufio.NewScanner(bytes.NewReader(logSnapshot.Bytes))
	scanner.Buffer(make([]byte, 0, 1024*64), 1024*1024)
	kept := []memoryEdgeEntry{}
	seen := map[string]int{}
	scanned := 0
	skippedVolatile := 0
	skippedExactState := 0
	skippedInvalid := 0
	skippedDuplicate := 0
	exactStatePaths, err := m.exactStatePathsSnapshotWithFenceChecked(fence)
	if err != nil {
		return nil, err
	}
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
		if memoryGraphEdgeRequiresBinding(normalized) && !memoryReferenceBindingValid(normalized.Binding) {
			reason := "unbound_promoted_relation"
			if normalized.Relation == "references" {
				reason = "legacy_or_unbound_reference"
			}
			m.quarantineMemoryReferenceEdge(normalized, reason)
			skippedInvalid += 1
			continue
		}
		if edgeReferencesExactStatePaths(exactStatePaths, normalized) {
			skippedExactState += 1
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
	// Recheck after the scan so registrations completed during maintenance are
	// also removed from the rewritten edge log.
	latestExactStatePaths, err := m.exactStatePathsSnapshotWithFenceChecked(fence)
	if err != nil {
		return nil, err
	}
	filtered := kept[:0]
	for _, edge := range kept {
		if edgeReferencesExactStatePaths(latestExactStatePaths, edge) {
			skippedExactState += 1
			continue
		}
		filtered = append(filtered, edge)
	}
	kept = filtered

	afterBytes := beforeBytes
	if !dryRun {
		var output bytes.Buffer
		enc := json.NewEncoder(&output)
		for _, edge := range kept {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
			if err := enc.Encode(edge); err != nil {
				return nil, err
			}
		}
		if _, err := m.replaceMemoryEdgeLogWithFenceLocked(output.Bytes(), "compact", fence); err != nil {
			// The log replacement may already be durable when the sidecar
			// acknowledgement fails. Reconcile the live projection from the
			// current bytes before returning the original error so same-process
			// reads and retries cannot observe the pre-compaction graph.
			var replacementErr *memoryEdgeLogReplacementError
			if errors.As(err, &replacementErr) && replacementErr.Committed {
				actual, _, readErr := m.readMemoryEdgeLogBytesLocked(memoryEdgeLogMaxReplacementBytes)
				if readErr != nil {
					m.invalidateMemoryEdges()
					return nil, errors.Join(err, fmt.Errorf("read memory graph after compaction failure: %w", readErr))
				}
				if reloadErr := m.reloadMemoryEdgesFromRawWithFenceLocked(actual, fence); reloadErr != nil {
					m.invalidateMemoryEdges()
					return nil, errors.Join(err, fmt.Errorf("reload memory graph after compaction failure: %w", reloadErr))
				}
			}
			return nil, err
		}
		afterBytes = int64(output.Len())
		if err := m.reloadMemoryEdgesFromRawWithFenceLocked(output.Bytes(), fence); err != nil {
			m.invalidateMemoryEdges()
			return nil, fmt.Errorf("reload memory graph after compaction: %w", err)
		}
	}

	return map[string]any{
		"ok":                  true,
		"dry_run":             dryRun,
		"edge_store_ref":      ownerOnlyStoreRef("memory_edges"),
		"scanned":             scanned,
		"kept":                len(kept),
		"skipped_exact_state": skippedExactState,
		"skipped_volatile":    skippedVolatile,
		"skipped_invalid":     skippedInvalid,
		"skipped_duplicate":   skippedDuplicate,
		"bytes_before":        beforeBytes,
		"bytes_after":         afterBytes,
	}, nil
}

func (m *memoryStore) upsertMemoryEdge(ctx context.Context, edge memoryEdgeEntry) (memoryEdgeEntry, error) {
	if m == nil || !m.isEnabled() {
		return memoryEdgeEntry{}, errors.New("go memory store is disabled")
	}
	if ctx == nil {
		ctx = context.Background()
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
	if field, reserved := memoryGraphEdgeReservedServerNamespace(normalized); reserved {
		return memoryEdgeEntry{}, fmt.Errorf("memory edge field %s uses a reserved server repair namespace", field)
	}
	_, _, _, sourceKey, err := canonicalMemoryID(normalized.SourceID)
	if err != nil {
		return memoryEdgeEntry{}, err
	}
	_, _, _, targetKey, err := canonicalMemoryID(normalized.TargetID)
	if err != nil {
		return memoryEdgeEntry{}, err
	}
	fence, err := m.acquireMemoryEdgeLogFenceOptionalContext(ctx)
	if err != nil {
		return memoryEdgeEntry{}, err
	}
	if fence != nil {
		defer fence.release()
	}
	unlockPaths, err := m.lockMemoryPathsContext(ctx, sourceKey, targetKey)
	if err != nil {
		return memoryEdgeEntry{}, err
	}
	defer unlockPaths()
	if excluded, reason := m.memoryGraphEdgeExcluded(normalized); excluded {
		return memoryEdgeEntry{}, fmt.Errorf("memory edge rejected by graph artifact policy: %s", reason)
	}
	exactStatePaths, err := m.exactStatePathsSnapshotWithFenceChecked(fence)
	if err != nil {
		return memoryEdgeEntry{}, err
	}
	if edgeReferencesExactStatePaths(exactStatePaths, normalized) {
		return memoryEdgeEntry{}, errors.New("memory edge rejected because exact state is not graph-addressable")
	}
	if normalized.Relation == "references" && !memoryReferenceBindingValid(normalized.Binding) {
		return memoryEdgeEntry{}, errors.New("reference edges require a current-state binding")
	}
	if memoryGraphEdgeRequiresBinding(normalized) {
		if normalized.Binding == nil {
			snapshot, snapshotErr := m.captureMemoryReferenceSnapshot(ctx, nil)
			if snapshotErr == nil {
				if bound, bindErr := m.bindPromotedMemoryEdge(snapshot, normalized); bindErr == nil {
					normalized = bound
				}
			}
		}
		if normalized.Binding == nil && !memoryGraphEdgeAllowsUnboundEvidence(normalized) {
			return memoryEdgeEntry{}, errors.New("memory graph edge endpoints do not have an exact current binding")
		}
		// A supplied binding is authoritative input and must be current before
		// append. Explicit legacy evidence remains durable only for repair custody.
		if normalized.Binding != nil && (!memoryReferenceBindingValid(normalized.Binding) || !m.referenceEdgeCurrentWithFence(normalized, fence)) {
			return memoryEdgeEntry{}, errors.New("memory graph edge binding is not current")
		}
	}
	// A durable append can complete while its Sync/state acknowledgement is
	// ambiguous. Recovery hydrates the exact row into this projection before
	// returning the error; acknowledge an exact retry without another row.
	if m.memoryEdgeExistsExact(normalized) {
		return normalized, nil
	}
	if m.beforeEdgeCommit != nil {
		m.beforeEdgeCommit()
	}
	if _, _, err := m.appendMemoryEdgeLogWithFenceContextLocked(ctx, normalized, true, fence); err != nil {
		return memoryEdgeEntry{}, err
	}
	m.mu.Lock()
	m.recordEdgeLocked(normalized)
	m.mu.Unlock()
	return normalized, nil
}

type memoryEdgeBatchUpsertResult struct {
	Edge     memoryEdgeEntry
	Existing bool
}

// upsertMemoryEdgesBatch amortizes edge-log validation across a hard-bounded
// backfill chunk. The path locks and cross-process writer fence are never held
// for more than memoryEdgeBackfillWriteBatchLimit candidate actions.
func (m *memoryStore) upsertMemoryEdgesBatch(ctx context.Context, edges []memoryEdgeEntry) ([]memoryEdgeBatchUpsertResult, error) {
	if m == nil || !m.isEnabled() {
		return nil, errors.New("go memory store is disabled")
	}
	if len(edges) == 0 {
		return []memoryEdgeBatchUpsertResult{}, nil
	}
	if len(edges) > memoryEdgeBackfillWriteBatchLimit {
		return nil, fmt.Errorf("memory edge batch exceeds hard limit: %d > %d", len(edges), memoryEdgeBackfillWriteBatchLimit)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	normalized := make([]memoryEdgeEntry, len(edges))
	pathKeys := make([]string, 0, len(edges)*2)
	for idx, edge := range edges {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		item, err := edge.normalized()
		if err != nil {
			return nil, err
		}
		if field, reserved := memoryGraphEdgeReservedServerNamespace(item); reserved {
			return nil, fmt.Errorf("memory edge field %s uses a reserved server repair namespace", field)
		}
		_, _, _, sourceKey, err := canonicalMemoryID(item.SourceID)
		if err != nil {
			return nil, err
		}
		_, _, _, targetKey, err := canonicalMemoryID(item.TargetID)
		if err != nil {
			return nil, err
		}
		normalized[idx] = item
		pathKeys = append(pathKeys, sourceKey, targetKey)
	}

	fence, err := m.acquireMemoryEdgeLogFenceOptionalContext(ctx)
	if err != nil {
		return nil, err
	}
	if fence != nil {
		defer fence.release()
	}
	unlockPaths, err := m.lockMemoryPathsContext(ctx, pathKeys...)
	if err != nil {
		return nil, err
	}
	defer unlockPaths()
	exactStatePaths, err := m.exactStatePathsSnapshotWithFenceChecked(fence)
	if err != nil {
		return nil, err
	}
	var bindingSnapshot *memoryReferenceSnapshot
	for idx, edge := range normalized {
		if excluded, reason := m.memoryGraphEdgeExcluded(edge); excluded {
			return nil, fmt.Errorf("memory edge rejected by graph artifact policy: %s", reason)
		}
		if edgeReferencesExactStatePaths(exactStatePaths, edge) {
			return nil, errors.New("memory edge rejected because exact state is not graph-addressable")
		}
		if memoryGraphEdgeRequiresBinding(edge) {
			if edge.Binding == nil {
				if bindingSnapshot == nil {
					bindingSnapshot, _ = m.captureMemoryReferenceSnapshot(ctx, nil)
				}
				if bindingSnapshot != nil {
					if bound, bindErr := m.bindPromotedMemoryEdge(bindingSnapshot, edge); bindErr == nil {
						edge = bound
						normalized[idx] = bound
					}
				}
			}
			if edge.Binding == nil && !memoryGraphEdgeAllowsUnboundEvidence(edge) {
				return nil, errors.New("memory graph edge endpoints do not have an exact current binding")
			}
			if edge.Binding != nil && (!memoryReferenceBindingValid(edge.Binding) || !m.referenceEdgeCurrentWithFence(edge, fence)) {
				return nil, errors.New("memory graph edge binding is not current")
			}
		}
	}

	appender, err := m.newMemoryEdgeLogAppenderFastWithFenceContextLocked(ctx, true, fence)
	if err != nil {
		return nil, err
	}
	if m.memoryEdgeLogObserveBatch != nil {
		m.memoryEdgeLogObserveBatch(len(normalized))
	}
	results := make([]memoryEdgeBatchUpsertResult, 0, len(normalized))
	for _, edge := range normalized {
		select {
		case <-ctx.Done():
			return results, ctx.Err()
		default:
		}
		if m.memoryEdgeVersionExistsWithFence(edge, fence) {
			results = append(results, memoryEdgeBatchUpsertResult{Edge: edge, Existing: true})
			continue
		}
		if m.beforeEdgeCommit != nil {
			m.beforeEdgeCommit()
		}
		stored, _, err := appender.append(edge)
		if err != nil {
			return results, err
		}
		m.mu.Lock()
		m.recordEdgeLocked(stored)
		m.mu.Unlock()
		results = append(results, memoryEdgeBatchUpsertResult{Edge: stored})
	}
	return results, nil
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
	candidates := make([]memoryEdgeEntry, 0, len(ids))
	for i := len(ids) - 1; i >= 0; i-- {
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
		candidates = append(candidates, edge)
	}
	m.mu.RUnlock()
	edges := make([]memoryEdgeEntry, 0, minInt(len(candidates), query.Limit))
	for _, edge := range candidates {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		if memoryGraphEdgeRequiresBinding(edge) {
			if !m.referenceEdgeCurrent(edge) {
				continue
			}
		}
		edges = append(edges, edge)
		if len(edges) >= query.Limit {
			break
		}
	}
	return edges, nil
}

// listMemoryEdgesComplete reads the bounded adjacency index without applying a
// neighbor limit. It is used only while freezing the closed graph corpus; a
// caller must still enforce the corpus-wide edge cap. The boolean is false
// when the index cannot prove a complete adjacency snapshot.
func (m *memoryStore) listMemoryEdgesComplete(ctx context.Context, query memoryEdgeQuery, maxEdges int) ([]memoryEdgeEntry, bool, error) {
	if m == nil || !m.isEnabled() || maxEdges < 1 {
		return []memoryEdgeEntry{}, false, nil
	}
	query.Direction = normalizeMemoryEdgeDirection(query.Direction)
	project := strings.TrimSpace(query.Project)
	if project != "" {
		var err error
		project, err = sanitizeMemoryProject(project)
		if err != nil {
			return nil, false, err
		}
	}
	memoryID := ""
	memoryKey := ""
	if strings.TrimSpace(query.MemoryID) != "" {
		_, _, canonical, key, err := canonicalMemoryID(query.MemoryID)
		if err != nil {
			return nil, false, err
		}
		memoryID, memoryKey = canonical, key
	}
	if ctx == nil {
		ctx = context.Background()
	}
	fence, err := m.acquireMemoryEdgeLogFenceOptionalContext(ctx)
	if err != nil {
		return nil, false, err
	}
	if fence != nil {
		defer fence.release()
	}
	m.mu.RLock()
	var ids []string
	if memoryKey != "" {
		for edgeID := range m.edgeAdjacency[memoryKey] {
			ids = append(ids, edgeID)
		}
	} else {
		ids = append(ids, m.edgeOrder...)
	}
	seenIDs := make(map[string]struct{}, len(ids))
	for _, edgeID := range ids {
		if _, duplicate := seenIDs[edgeID]; duplicate {
			m.mu.RUnlock()
			return []memoryEdgeEntry{}, false, nil
		}
		if _, exists := m.edges[edgeID]; !exists {
			m.mu.RUnlock()
			return []memoryEdgeEntry{}, false, nil
		}
		if _, exists := m.edgeOrdinal[edgeID]; !exists {
			m.mu.RUnlock()
			return []memoryEdgeEntry{}, false, nil
		}
		seenIDs[edgeID] = struct{}{}
	}
	if memoryKey == "" && (len(seenIDs) != len(m.edges) || len(m.edgeOrdinal) != len(m.edges)) {
		m.mu.RUnlock()
		return []memoryEdgeEntry{}, false, nil
	}
	sort.Slice(ids, func(i, j int) bool { return m.edgeOrdinal[ids[i]] < m.edgeOrdinal[ids[j]] })
	if len(ids) > maxEdges {
		m.mu.RUnlock()
		return []memoryEdgeEntry{}, false, nil
	}
	edges := make([]memoryEdgeEntry, 0, len(ids))
	for index := len(ids) - 1; index >= 0; index-- {
		select {
		case <-ctx.Done():
			m.mu.RUnlock()
			return nil, false, ctx.Err()
		default:
		}
		edge, exists := m.edges[ids[index]]
		if !exists {
			continue
		}
		if project != "" && !strings.EqualFold(edge.Project, project) {
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
		if !query.IncludeEphemeral && !shouldSurfaceMemoryLifecycle(edge.Lifecycle, false) {
			continue
		}
		excluded, _ := m.memoryGraphEdgeExcluded(edge)
		if excluded {
			continue
		}
		edges = append(edges, edge)
	}
	m.mu.RUnlock()
	if m.memoryEdgesCompleteAfterSnapshot != nil {
		m.memoryEdgesCompleteAfterSnapshot()
	}
	current := make([]memoryEdgeEntry, 0, len(edges))
	for _, edge := range edges {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if memoryGraphEdgeRequiresBinding(edge) && !m.referenceEdgeCurrentWithFence(edge, fence) {
			continue
		}
		current = append(current, edge)
	}
	return current, true, nil
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
				if field, reserved := memoryGraphEdgeReservedServerNamespace(memoryEdgeEntry{Metadata: mapFromAny(parsed["metadata"]), Provenance: mapFromAny(parsed["provenance"])}); reserved {
					writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": fmt.Sprintf("memory edge field %s uses a reserved server repair namespace", field)})
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
	add := func(row map[string]any) {
		if len(out) >= limit || row == nil {
			return
		}
		if rowMemoryID := contextPackGraphMemoryID(row); rowMemoryID != "" {
			if _, _, canonicalRow, _, err := canonicalMemoryID(rowMemoryID); err == nil && strings.EqualFold(canonicalRow, canonicalSelf) {
				return
			}
		}
		identity := rowIdentity(row)
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
		add(row)
	}
	for _, raw := range retrievalRows {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		add(row)
	}
	return out
}
