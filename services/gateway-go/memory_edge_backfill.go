package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const memoryEdgeBackfillVersion = "typed_edge_backfill.v1"

type memoryEdgeBackfillRequest struct {
	DryRun             bool
	Project            string
	IncludeCold        bool
	IncludeEphemeral   bool
	MinConfidence      float64
	MaxCandidates      int
	MaxHistoryLines    int
	TopicPeerLimit     int
	SampleLimit        int
	IncludeLowAudit    bool
	AllowedRelation    map[string]struct{}
	RequestedRelations []string
}

type memoryEdgeBackfillCandidate struct {
	Edge     memoryEdgeEntry
	Strategy string
	Reason   string
}

type memoryEdgeBackfillRelationStats struct {
	Generated              int `json:"generated"`
	Eligible               int `json:"eligible"`
	Written                int `json:"written"`
	Existing               int `json:"existing"`
	SkippedBelowConfidence int `json:"skipped_below_confidence"`
}

type memoryEdgeBackfillDoc struct {
	Project   string
	FileName  string
	MemoryID  string
	TopicPath string
	Summary   string
	UpdatedAt time.Time
	LastTouch time.Time
	Lifecycle string
}

func normalizeMemoryEdgeBackfillRequest(payload map[string]any, policy memoryStorePolicy) (memoryEdgeBackfillRequest, error) {
	req := memoryEdgeBackfillRequest{
		DryRun:             true,
		IncludeCold:        true,
		IncludeLowAudit:    true,
		MinConfidence:      0.95,
		MaxCandidates:      50000,
		MaxHistoryLines:    policy.historyStartupMaxLines,
		TopicPeerLimit:     2,
		SampleLimit:        20,
		AllowedRelation:    map[string]struct{}{},
		RequestedRelations: []string{},
	}
	if req.MaxHistoryLines < 1 {
		req.MaxHistoryLines = 20000
	}
	if _, ok := payload["dry_run"]; ok {
		req.DryRun = anyToBool(payload["dry_run"])
	}
	if project := strings.TrimSpace(anyToString(payload["project"])); project != "" {
		clean, err := sanitizeMemoryProject(project)
		if err != nil {
			return req, err
		}
		req.Project = clean
	}
	if _, ok := payload["include_cold"]; ok {
		req.IncludeCold = anyToBool(payload["include_cold"])
	}
	req.IncludeEphemeral = requestIncludesEphemeralMemory(payload)
	if _, ok := payload["min_confidence"]; ok {
		req.MinConfidence = anyToFloat64(payload["min_confidence"], 0.95)
	}
	if req.MinConfidence < 0 || req.MinConfidence > 1 {
		return req, errors.New("min_confidence must be between 0 and 1")
	}
	if _, ok := payload["max_candidates"]; ok {
		req.MaxCandidates = anyToInt(payload["max_candidates"], req.MaxCandidates)
	}
	req.MaxCandidates = clampInt(req.MaxCandidates, 1, 200000)
	if _, ok := payload["max_history_lines"]; ok {
		req.MaxHistoryLines = anyToInt(payload["max_history_lines"], req.MaxHistoryLines)
	}
	req.MaxHistoryLines = clampInt(req.MaxHistoryLines, 1, 200000)
	if _, ok := payload["topic_peer_limit"]; ok {
		req.TopicPeerLimit = anyToInt(payload["topic_peer_limit"], req.TopicPeerLimit)
	}
	req.TopicPeerLimit = clampInt(req.TopicPeerLimit, 1, 8)
	if _, ok := payload["sample_limit"]; ok {
		req.SampleLimit = anyToInt(payload["sample_limit"], req.SampleLimit)
	}
	req.SampleLimit = clampInt(req.SampleLimit, 0, 100)
	if _, ok := payload["include_low_confidence_audit"]; ok {
		req.IncludeLowAudit = anyToBool(payload["include_low_confidence_audit"])
	}
	if rawRelations, ok := payload["relations"].([]any); ok {
		for _, raw := range rawRelations {
			relation, err := normalizeMemoryEdgeRelation(anyToString(raw))
			if err != nil {
				return req, err
			}
			req.AllowedRelation[relation] = struct{}{}
			req.RequestedRelations = append(req.RequestedRelations, relation)
		}
	}
	return req, nil
}

func (req memoryEdgeBackfillRequest) relationAllowed(relation string) bool {
	if len(req.AllowedRelation) == 0 {
		return true
	}
	_, ok := req.AllowedRelation[relation]
	return ok
}

func (m *memoryStore) memoryEdgeExists(edgeID string) bool {
	if m == nil || strings.TrimSpace(edgeID) == "" {
		return false
	}
	m.mu.RLock()
	_, exists := m.edges[edgeID]
	m.mu.RUnlock()
	return exists
}

func (m *memoryStore) memoryBackfillHistoryEntries(maxLines int) []memoryStoreEntry {
	if m == nil || !m.policy.enabled {
		return []memoryStoreEntry{}
	}
	if maxLines < 1 {
		maxLines = 1
	}
	if file, err := os.Open(m.policy.historyPath); err == nil {
		defer file.Close()
		lines, readErr := readHistoryTailLines(file, maxLines, m.policy.historyStartupTailMaxBytes)
		if readErr == nil && len(lines) > 0 {
			entries := make([]memoryStoreEntry, 0, len(lines))
			for _, line := range lines {
				var entry memoryStoreEntry
				if err := json.Unmarshal([]byte(line), &entry); err != nil {
					continue
				}
				entries = append(entries, entry)
			}
			return entries
		}
	}
	m.mu.RLock()
	entries := append([]memoryStoreEntry(nil), m.recent...)
	m.mu.RUnlock()
	if len(entries) > maxLines {
		entries = entries[len(entries)-maxLines:]
	}
	return entries
}

func memoryBackfillDocsFromStoreDocs(docs []memoryStoreDoc) []memoryEdgeBackfillDoc {
	out := make([]memoryEdgeBackfillDoc, 0, len(docs))
	for _, doc := range docs {
		project, fileName, memoryID, _, err := canonicalMemoryID(doc.Project + "::" + doc.FileName)
		if err != nil {
			continue
		}
		out = append(out, memoryEdgeBackfillDoc{
			Project:   project,
			FileName:  fileName,
			MemoryID:  memoryID,
			TopicPath: sanitizeTopicPath(doc.TopicPath, fileName),
			Summary:   strings.TrimSpace(doc.Summary),
			UpdatedAt: doc.UpdatedAt,
			LastTouch: doc.UpdatedAt,
			Lifecycle: "durable",
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return memoryBackfillDocLess(out[i], out[j])
	})
	return out
}

func memoryBackfillDocLess(left memoryEdgeBackfillDoc, right memoryEdgeBackfillDoc) bool {
	if !strings.EqualFold(left.Project, right.Project) {
		return strings.ToLower(left.Project) < strings.ToLower(right.Project)
	}
	leftTopic := strings.ToLower(strings.Trim(left.TopicPath, "/"))
	rightTopic := strings.ToLower(strings.Trim(right.TopicPath, "/"))
	if leftTopic != rightTopic {
		return leftTopic < rightTopic
	}
	leftTime := left.LastTouch
	if leftTime.IsZero() {
		leftTime = left.UpdatedAt
	}
	rightTime := right.LastTouch
	if rightTime.IsZero() {
		rightTime = right.UpdatedAt
	}
	if !leftTime.Equal(rightTime) {
		return leftTime.Before(rightTime)
	}
	return strings.ToLower(left.FileName) < strings.ToLower(right.FileName)
}

func memoryEdgeBackfillCandidateEdge(
	sourceID string,
	targetID string,
	relation string,
	confidence float64,
	topicPath string,
	strategy string,
	reason string,
) (memoryEdgeBackfillCandidate, error) {
	project, sourceFile, canonicalSource, _, err := canonicalMemoryID(sourceID)
	if err != nil {
		return memoryEdgeBackfillCandidate{}, err
	}
	_, _, canonicalTarget, _, err := canonicalMemoryID(targetID)
	if err != nil {
		return memoryEdgeBackfillCandidate{}, err
	}
	relation, err = normalizeMemoryEdgeRelation(relation)
	if err != nil {
		return memoryEdgeBackfillCandidate{}, err
	}
	if canonicalSource == canonicalTarget {
		return memoryEdgeBackfillCandidate{}, errors.New("source_id and target_id must differ")
	}
	edge := memoryEdgeEntry{
		EdgeID:     deterministicMemoryEdgeID(canonicalSource, relation, canonicalTarget),
		SourceID:   canonicalSource,
		TargetID:   canonicalTarget,
		Relation:   relation,
		Project:    project,
		TopicPath:  sanitizeTopicPath(topicPath, sourceFile),
		Confidence: confidence,
		Provenance: map[string]any{
			"kind":     "retroactive_memory_edge_backfill",
			"strategy": strategy,
			"version":  memoryEdgeBackfillVersion,
			"reason":   reason,
		},
		Metadata: map[string]any{
			"backfill": true,
		},
		Lifecycle: "durable",
		CreatedAt: nowUTCISO(),
		Source:    memoryEdgeSource,
	}
	normalized, err := edge.normalized()
	if err != nil {
		return memoryEdgeBackfillCandidate{}, err
	}
	return memoryEdgeBackfillCandidate{Edge: normalized, Strategy: strategy, Reason: reason}, nil
}

func (m *memoryStore) deterministicMemoryEdgeBackfill(ctx context.Context, req memoryEdgeBackfillRequest) (map[string]any, error) {
	if m == nil || !m.policy.enabled {
		return nil, errors.New("go memory store is disabled")
	}
	storeDocs, err := m.collectDocs(ctx, req.Project)
	if err != nil {
		return nil, err
	}
	docs := memoryBackfillDocsFromStoreDocs(storeDocs)
	historyEntries := m.memoryBackfillHistoryEntries(req.MaxHistoryLines)
	generator := &memoryEdgeBackfillGenerator{
		store:      m,
		request:    req,
		docs:       docs,
		knownIDs:   map[string]memoryEdgeBackfillDoc{},
		stats:      map[string]*memoryEdgeBackfillRelationStats{},
		sampleRows: []map[string]any{},
	}
	for _, doc := range docs {
		generator.knownIDs[strings.ToLower(doc.MemoryID)] = doc
	}
	generator.generateTopicEdges(ctx)
	generator.generateReferenceEdges(ctx)
	generator.generateHistorySequenceEdges(ctx, historyEntries, "same_session", 0.98)
	if req.IncludeLowAudit {
		generator.generateHistorySequenceEdges(ctx, historyEntries, "same_agent", 0.82)
	}
	if generator.ctxErr != nil {
		return nil, generator.ctxErr
	}
	totalGenerated := 0
	totalEligible := 0
	totalWritten := 0
	totalExisting := 0
	totalSkipped := 0
	for _, stat := range generator.stats {
		totalGenerated += stat.Generated
		totalEligible += stat.Eligible
		totalWritten += stat.Written
		totalExisting += stat.Existing
		totalSkipped += stat.SkippedBelowConfidence
	}
	return map[string]any{
		"ok":                           len(generator.errorsList) == 0,
		"dry_run":                      req.DryRun,
		"source":                       "go_memory_store",
		"backfill_version":             memoryEdgeBackfillVersion,
		"project":                      req.Project,
		"scanned_docs":                 len(docs),
		"scanned_history_entries":      len(historyEntries),
		"generated":                    totalGenerated,
		"eligible":                     totalEligible,
		"would_write":                  totalEligible - totalExisting,
		"written":                      totalWritten,
		"existing":                     totalExisting,
		"skipped_below_confidence":     totalSkipped,
		"truncated":                    generator.truncated,
		"max_candidates":               req.MaxCandidates,
		"min_confidence":               req.MinConfidence,
		"topic_peer_limit":             req.TopicPeerLimit,
		"include_cold":                 req.IncludeCold,
		"include_ephemeral":            req.IncludeEphemeral,
		"include_low_confidence_audit": req.IncludeLowAudit,
		"requested_relations":          req.RequestedRelations,
		"relations":                    generator.stats,
		"samples":                      generator.sampleRows,
		"errors":                       generator.errorsList,
	}, nil
}

type memoryEdgeBackfillGenerator struct {
	store      *memoryStore
	request    memoryEdgeBackfillRequest
	docs       []memoryEdgeBackfillDoc
	knownIDs   map[string]memoryEdgeBackfillDoc
	stats      map[string]*memoryEdgeBackfillRelationStats
	sampleRows []map[string]any
	seen       map[string]struct{}
	generated  int
	truncated  bool
	errorsList []string
	ctxErr     error
}

func (g *memoryEdgeBackfillGenerator) stat(relation string) *memoryEdgeBackfillRelationStats {
	if g.stats == nil {
		g.stats = map[string]*memoryEdgeBackfillRelationStats{}
	}
	stat := g.stats[relation]
	if stat == nil {
		stat = &memoryEdgeBackfillRelationStats{}
		g.stats[relation] = stat
	}
	return stat
}

func (g *memoryEdgeBackfillGenerator) add(ctx context.Context, candidate memoryEdgeBackfillCandidate) {
	if g.ctxErr != nil || g.truncated {
		return
	}
	select {
	case <-ctx.Done():
		g.ctxErr = ctx.Err()
		return
	default:
	}
	if !g.request.relationAllowed(candidate.Edge.Relation) {
		return
	}
	if g.seen == nil {
		g.seen = map[string]struct{}{}
	}
	if _, exists := g.seen[candidate.Edge.EdgeID]; exists {
		return
	}
	g.seen[candidate.Edge.EdgeID] = struct{}{}
	stat := g.stat(candidate.Edge.Relation)
	stat.Generated += 1
	g.generated += 1
	if g.generated > g.request.MaxCandidates {
		g.truncated = true
		return
	}
	if len(g.sampleRows) < g.request.SampleLimit {
		g.sampleRows = append(g.sampleRows, map[string]any{
			"edge_id":    candidate.Edge.EdgeID,
			"source_id":  candidate.Edge.SourceID,
			"target_id":  candidate.Edge.TargetID,
			"relation":   candidate.Edge.Relation,
			"confidence": candidate.Edge.Confidence,
			"strategy":   candidate.Strategy,
			"reason":     candidate.Reason,
			"would_write": candidate.Edge.Confidence >= g.request.MinConfidence &&
				!g.store.memoryEdgeExists(candidate.Edge.EdgeID),
		})
	}
	if candidate.Edge.Confidence < g.request.MinConfidence {
		stat.SkippedBelowConfidence += 1
		return
	}
	stat.Eligible += 1
	if g.store.memoryEdgeExists(candidate.Edge.EdgeID) {
		stat.Existing += 1
		return
	}
	if g.request.DryRun {
		return
	}
	if _, err := g.store.upsertMemoryEdge(ctx, candidate.Edge); err != nil {
		g.errorsList = append(g.errorsList, candidate.Edge.EdgeID+": "+err.Error())
		return
	}
	stat.Written += 1
}

func (g *memoryEdgeBackfillGenerator) generateTopicEdges(ctx context.Context) {
	groups := map[string][]memoryEdgeBackfillDoc{}
	for _, doc := range g.docs {
		topic := strings.Trim(strings.ToLower(doc.TopicPath), "/")
		if topic == "" {
			topic = "root"
		}
		groups[strings.ToLower(doc.Project)+"\x00"+topic] = append(groups[strings.ToLower(doc.Project)+"\x00"+topic], doc)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		docs := groups[key]
		sort.Slice(docs, func(i, j int) bool {
			return memoryBackfillDocLess(docs[i], docs[j])
		})
		for i, source := range docs {
			for offset := 1; offset <= g.request.TopicPeerLimit && i+offset < len(docs); offset++ {
				target := docs[i+offset]
				topic := strings.Trim(strings.ToLower(source.TopicPath), "/")
				confidence := 0.95
				reason := "exact_topic_path"
				if topic == "" || topic == "root" {
					confidence = 0.86
					reason = "root_topic_path"
				}
				candidate, err := memoryEdgeBackfillCandidateEdge(
					source.MemoryID,
					target.MemoryID,
					"same_topic",
					confidence,
					source.TopicPath,
					"topic_peer",
					reason,
				)
				if err == nil {
					g.add(ctx, candidate)
				}
				if g.ctxErr != nil || g.truncated {
					return
				}
			}
		}
	}
}

func (g *memoryEdgeBackfillGenerator) generateReferenceEdges(ctx context.Context) {
	if len(g.knownIDs) == 0 {
		return
	}
	for _, source := range g.docs {
		targetIDs := referencedMemoryIDs(source.Project, source.Summary, g.knownIDs)
		sort.Strings(targetIDs)
		for _, targetID := range targetIDs {
			if strings.EqualFold(source.MemoryID, targetID) {
				continue
			}
			candidate, err := memoryEdgeBackfillCandidateEdge(
				source.MemoryID,
				targetID,
				"references",
				0.99,
				source.TopicPath,
				"explicit_reference",
				"summary_memory_id_match",
			)
			if err == nil {
				g.add(ctx, candidate)
			}
			if g.ctxErr != nil || g.truncated {
				return
			}
		}
	}
}

func referencedMemoryIDs(sourceProject string, text string, knownIDs map[string]memoryEdgeBackfillDoc) []string {
	if strings.TrimSpace(text) == "" || len(knownIDs) == 0 {
		return []string{}
	}
	seen := map[string]struct{}{}
	fields := strings.FieldsFunc(text, func(ch rune) bool {
		switch ch {
		case ' ', '\n', '\r', '\t', '"', '\'', '`', '(', ')', '[', ']', '{', '}', '<', '>', ',', ';':
			return true
		default:
			return false
		}
	})
	for _, raw := range fields {
		token := strings.Trim(raw, ".,:!?")
		if token == "" {
			continue
		}
		candidates := []string{}
		if strings.Contains(token, "::") {
			candidates = append(candidates, token)
		}
		if strings.Contains(token, "/") {
			candidates = append(candidates, sourceProject+"::"+token)
			parts := strings.SplitN(token, "/", 2)
			if len(parts) == 2 {
				candidates = append(candidates, parts[0]+"::"+parts[1])
			}
		}
		for _, candidate := range candidates {
			_, _, canonical, _, err := canonicalMemoryID(candidate)
			if err != nil {
				continue
			}
			if _, ok := knownIDs[strings.ToLower(canonical)]; ok {
				seen[canonical] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	return out
}

func (g *memoryEdgeBackfillGenerator) generateHistorySequenceEdges(
	ctx context.Context,
	entries []memoryStoreEntry,
	relation string,
	confidence float64,
) {
	type sequenceEntry struct {
		memoryID  string
		project   string
		topicPath string
		fileName  string
		eventID   string
		created   time.Time
		groupKey  string
	}
	groups := map[string][]sequenceEntry{}
	for _, entry := range entries {
		if isMemoryTombstone(entry) {
			continue
		}
		if g.request.Project != "" && !strings.EqualFold(entry.Project, g.request.Project) {
			continue
		}
		project, fileName, memoryID, _, err := canonicalMemoryID(entry.Project + "::" + entry.FileName)
		if err != nil {
			continue
		}
		topicPath := sanitizeTopicPath(entry.TopicPath, fileName)
		groupToken := ""
		switch relation {
		case "same_session":
			groupToken = strings.TrimSpace(entry.SessionID)
			if groupToken == "" {
				continue
			}
			groupToken = "session:" + strings.ToLower(project) + ":" + groupToken
		case "same_agent":
			groupToken = strings.TrimSpace(entry.AgentID)
			if groupToken == "" {
				continue
			}
			groupToken = "agent:" + strings.ToLower(project) + ":" + strings.ToLower(groupToken) + ":" + strings.Trim(strings.ToLower(topicPath), "/")
		default:
			continue
		}
		created := time.Time{}
		if parsed, ok := parseTimeBestEffort(entry.CreatedAt); ok {
			created = parsed
		}
		groups[groupToken] = append(groups[groupToken], sequenceEntry{
			memoryID:  memoryID,
			project:   project,
			topicPath: topicPath,
			fileName:  fileName,
			eventID:   strings.TrimSpace(entry.EventID),
			created:   created,
			groupKey:  groupToken,
		})
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		items := groups[key]
		sort.Slice(items, func(i, j int) bool {
			if !items[i].created.Equal(items[j].created) {
				return items[i].created.Before(items[j].created)
			}
			if items[i].eventID != items[j].eventID {
				return items[i].eventID < items[j].eventID
			}
			return items[i].fileName < items[j].fileName
		})
		previous := sequenceEntry{}
		for _, current := range items {
			if previous.memoryID != "" && previous.memoryID != current.memoryID {
				candidate, err := memoryEdgeBackfillCandidateEdge(
					previous.memoryID,
					current.memoryID,
					relation,
					confidence,
					current.topicPath,
					"history_sequence",
					key,
				)
				if err == nil {
					g.add(ctx, candidate)
				}
				if g.ctxErr != nil || g.truncated {
					return
				}
			}
			previous = current
		}
	}
}

func (s *server) memoryV1EdgesBackfill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	if s.memoryStore == nil || !s.memoryStore.policy.enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "go memory store is disabled"})
		return
	}
	bodyBytes, err := readRequestBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
		return
	}
	payload := map[string]any{}
	if len(strings.TrimSpace(string(bodyBytes))) > 0 {
		payload, err = parseJSONMap(bodyBytes)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
			return
		}
	}
	req, err := normalizeMemoryEdgeBackfillRequest(payload, s.memoryStore.policy)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	report, err := s.memoryStore.deterministicMemoryEdgeBackfill(r.Context(), req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "memory edge backfill failed", "detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, report)
}
