package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const memoryEdgeBackfillVersion = "typed_edge_backfill.v1"
const memoryEdgeInferredScoringVersion = "memory_edge_inferred_scoring.v1"

type memoryEdgeBackfillRequest struct {
	DryRun              bool
	Project             string
	IncludeCold         bool
	IncludeEphemeral    bool
	MinConfidence       float64
	MaxCandidates       int
	MaxHistoryLines     int
	TopicPeerLimit      int
	SampleLimit         int
	IncludeLowAudit     bool
	IncludeInferred     bool
	InferredRelation    string
	InferredPeerLimit   int
	InferredScanLimit   int
	InferredMinScore    float64
	InferredMinShared   int
	InferredMaxPostings int
	Corpus              string
	AllowedRelation     map[string]struct{}
	RequestedRelations  []string
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
		DryRun:              true,
		IncludeCold:         true,
		IncludeLowAudit:     true,
		MinConfidence:       0.95,
		MaxCandidates:       50000,
		MaxHistoryLines:     policy.historyStartupMaxLines,
		TopicPeerLimit:      2,
		SampleLimit:         20,
		InferredRelation:    "inferred_related",
		InferredPeerLimit:   2,
		InferredScanLimit:   5000,
		InferredMinScore:    0.90,
		InferredMinShared:   3,
		InferredMaxPostings: 64,
		Corpus:              "history_index",
		AllowedRelation:     map[string]struct{}{},
		RequestedRelations:  []string{},
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
	if _, ok := payload["include_inferred"]; ok {
		req.IncludeInferred = anyToBool(payload["include_inferred"])
	}
	if rawRelation := strings.TrimSpace(anyToString(payload["inferred_relation"])); rawRelation != "" {
		relation, err := normalizeMemoryEdgeRelation(rawRelation)
		if err != nil {
			return req, err
		}
		req.InferredRelation = relation
	}
	if _, ok := payload["inferred_peer_limit"]; ok {
		req.InferredPeerLimit = anyToInt(payload["inferred_peer_limit"], req.InferredPeerLimit)
	}
	req.InferredPeerLimit = clampInt(req.InferredPeerLimit, 1, 10)
	if _, ok := payload["inferred_scan_limit"]; ok {
		req.InferredScanLimit = anyToInt(payload["inferred_scan_limit"], req.InferredScanLimit)
	}
	req.InferredScanLimit = clampInt(req.InferredScanLimit, 2, 50000)
	if _, ok := payload["inferred_min_score"]; ok {
		req.InferredMinScore = anyToFloat64(payload["inferred_min_score"], req.InferredMinScore)
	}
	if req.InferredMinScore < 0 || req.InferredMinScore > 1 {
		return req, errors.New("inferred_min_score must be between 0 and 1")
	}
	if _, ok := payload["inferred_min_shared_terms"]; ok {
		req.InferredMinShared = anyToInt(payload["inferred_min_shared_terms"], req.InferredMinShared)
	}
	req.InferredMinShared = clampInt(req.InferredMinShared, 1, 20)
	if _, ok := payload["inferred_max_token_postings"]; ok {
		req.InferredMaxPostings = anyToInt(payload["inferred_max_token_postings"], req.InferredMaxPostings)
	}
	req.InferredMaxPostings = clampInt(req.InferredMaxPostings, 4, 512)
	if rawCorpus := firstNonEmptyStrings(
		anyToString(payload["corpus"]),
		anyToString(payload["backfill_corpus"]),
		anyToString(payload["scan_source"]),
	); rawCorpus != "" {
		switch strings.TrimSpace(strings.ToLower(rawCorpus)) {
		case "history", "history_index", "history-index", "hot", "hot_index", "hot-index", "auto":
			req.Corpus = "history_index"
		case "disk", "filesystem", "file_system", "full_disk", "full-disk":
			req.Corpus = "disk"
		default:
			return req, errors.New("corpus must be history_index or disk")
		}
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
	if _, ok := req.AllowedRelation[req.InferredRelation]; ok {
		req.IncludeInferred = true
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

func (m *memoryStore) memoryBackfillDocsFromStoreDocs(docs []memoryStoreDoc) ([]memoryEdgeBackfillDoc, int) {
	out := make([]memoryEdgeBackfillDoc, 0, len(docs))
	skipped := 0
	for _, doc := range docs {
		project, fileName, memoryID, _, err := canonicalMemoryID(doc.Project + "::" + doc.FileName)
		if err != nil {
			continue
		}
		topicPath := sanitizeTopicPath(doc.TopicPath, fileName)
		if excluded, _ := m.memoryGraphArtifactExcluded(project, fileName, topicPath); excluded {
			skipped += 1
			continue
		}
		lastTouch := doc.UpdatedAt
		if lastTouch.IsZero() {
			lastTouch = doc.UpdatedAt
		}
		out = append(out, memoryEdgeBackfillDoc{
			Project:   project,
			FileName:  fileName,
			MemoryID:  memoryID,
			TopicPath: topicPath,
			Summary:   strings.TrimSpace(doc.Summary),
			UpdatedAt: doc.UpdatedAt,
			LastTouch: lastTouch,
			Lifecycle: "durable",
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return memoryBackfillDocLess(out[i], out[j])
	})
	return out, skipped
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
	var storeDocs []memoryStoreDoc
	var err error
	if req.Corpus == "disk" {
		storeDocs, err = m.collectDocsFromDisk(ctx, req.Project, req.IncludeCold, req.IncludeEphemeral)
	} else {
		storeDocs, err = m.collectDocs(ctx, req.Project)
	}
	if err != nil {
		return nil, err
	}
	docs, skippedLowValueDocs := m.memoryBackfillDocsFromStoreDocs(storeDocs)
	historyEntries := m.memoryBackfillHistoryEntries(req.MaxHistoryLines)
	generator := &memoryEdgeBackfillGenerator{
		store:               m,
		request:             req,
		docs:                docs,
		knownIDs:            map[string]memoryEdgeBackfillDoc{},
		stats:               map[string]*memoryEdgeBackfillRelationStats{},
		sampleRows:          []map[string]any{},
		skippedLowValueDocs: skippedLowValueDocs,
	}
	for _, doc := range docs {
		generator.knownIDs[strings.ToLower(doc.MemoryID)] = doc
	}
	generator.generateTopicEdges(ctx)
	generator.generateReferenceEdges(ctx)
	if req.IncludeInferred {
		generator.generateInferredRelatedEdges(ctx)
	}
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
		"skipped_low_value_docs":       generator.skippedLowValueDocs,
		"skipped_low_value_history":    generator.skippedLowValueHistory,
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
		"include_inferred":             req.IncludeInferred,
		"inferred_relation":            req.InferredRelation,
		"inferred_peer_limit":          req.InferredPeerLimit,
		"inferred_scan_limit":          req.InferredScanLimit,
		"inferred_min_score":           req.InferredMinScore,
		"inferred_min_shared_terms":    req.InferredMinShared,
		"inferred_max_token_postings":  req.InferredMaxPostings,
		"corpus":                       req.Corpus,
		"requested_relations":          req.RequestedRelations,
		"relations":                    generator.stats,
		"quality_audit":                generator.qualityAudit(),
		"samples":                      generator.sampleRows,
		"errors":                       generator.errorsList,
	}, nil
}

type memoryEdgeBackfillGenerator struct {
	store                  *memoryStore
	request                memoryEdgeBackfillRequest
	docs                   []memoryEdgeBackfillDoc
	knownIDs               map[string]memoryEdgeBackfillDoc
	stats                  map[string]*memoryEdgeBackfillRelationStats
	sampleRows             []map[string]any
	seen                   map[string]struct{}
	generated              int
	truncated              bool
	skippedLowValueDocs    int
	skippedLowValueHistory int
	qualityTotal           int
	qualityHigh            int
	qualityReview          int
	qualityLow             int
	qualityInferred        int
	qualityConfidenceSum   float64
	errorsList             []string
	ctxErr                 error
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
	quality := memoryEdgeCandidateQuality(candidate, g.request.MinConfidence)
	g.recordQualityAudit(candidate, quality)
	if g.generated > g.request.MaxCandidates {
		g.truncated = true
		return
	}
	wouldWrite := candidate.Edge.Confidence >= g.request.MinConfidence &&
		!g.store.memoryEdgeExists(candidate.Edge.EdgeID)
	if len(g.sampleRows) < g.request.SampleLimit {
		g.sampleRows = append(g.sampleRows, map[string]any{
			"edge_id":     candidate.Edge.EdgeID,
			"source_id":   candidate.Edge.SourceID,
			"target_id":   candidate.Edge.TargetID,
			"relation":    candidate.Edge.Relation,
			"confidence":  candidate.Edge.Confidence,
			"strategy":    candidate.Strategy,
			"reason":      candidate.Reason,
			"quality":     quality,
			"would_write": wouldWrite,
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

func (g *memoryEdgeBackfillGenerator) recordQualityAudit(candidate memoryEdgeBackfillCandidate, quality map[string]any) {
	g.qualityTotal += 1
	g.qualityConfidenceSum += candidate.Edge.Confidence
	if anyToBool(candidate.Edge.Metadata["inferred"]) || strings.Contains(strings.ToLower(candidate.Strategy), "inferred") {
		g.qualityInferred += 1
	}
	switch strings.TrimSpace(anyToString(quality["status"])) {
	case "high_confidence":
		g.qualityHigh += 1
	case "low_confidence":
		g.qualityLow += 1
	default:
		g.qualityReview += 1
	}
}

func (g *memoryEdgeBackfillGenerator) qualityAudit() map[string]any {
	avg := 0.0
	if g.qualityTotal > 0 {
		avg = g.qualityConfidenceSum / float64(g.qualityTotal)
	}
	return map[string]any{
		"schema_id":           "memory_edge_quality_audit.v1",
		"scoring_version":     memoryEdgeInferredScoringVersion,
		"generated":           g.qualityTotal,
		"high_confidence":     g.qualityHigh,
		"review_recommended":  g.qualityReview,
		"low_confidence":      g.qualityLow,
		"inferred_candidates": g.qualityInferred,
		"average_confidence":  roundFloat(avg, 4),
		"min_confidence":      g.request.MinConfidence,
		"audit_first":         g.request.DryRun,
		"write_gate":          "confidence_and_existing_edge",
		"recommendation":      "Use high-confidence same-topic/reference/session edges as retrieval context; treat inferred_related edges as supporting signals until sampled in recall evals.",
	}
}

func memoryEdgeCandidateQuality(candidate memoryEdgeBackfillCandidate, minConfidence float64) map[string]any {
	confidence := clampFloat(candidate.Edge.Confidence, 0, 1)
	inferred := anyToBool(candidate.Edge.Metadata["inferred"]) || strings.Contains(strings.ToLower(candidate.Strategy), "inferred")
	status := "review"
	if confidence < minConfidence {
		status = "low_confidence"
	} else if confidence >= 0.95 && !inferred {
		status = "high_confidence"
	}
	impact := "supporting"
	switch candidate.Edge.Relation {
	case "references", "same_session":
		if confidence >= minConfidence {
			impact = "strong"
		}
	case "same_topic", "same_agent":
		impact = "medium"
	case "inferred_related":
		impact = "exploratory"
	}
	warnings := []any{}
	if inferred {
		warnings = append(warnings, "inferred_edge_requires_sampling")
	}
	if confidence < minConfidence {
		warnings = append(warnings, "below_write_confidence_threshold")
	}
	if strings.TrimSpace(candidate.Reason) == "" {
		warnings = append(warnings, "missing_reason")
	}
	signals := map[string]any{
		"relation":    candidate.Edge.Relation,
		"strategy":    candidate.Strategy,
		"reason":      candidate.Reason,
		"inferred":    inferred,
		"topic_path":  candidate.Edge.TopicPath,
		"provenance":  anyMap(candidate.Edge.Provenance),
		"metadata":    anyMap(candidate.Edge.Metadata),
		"min_gate":    minConfidence,
		"write_ready": confidence >= minConfidence,
	}
	if shared := anyToInt(candidate.Edge.Metadata["shared_terms"], 0); shared > 0 {
		signals["shared_terms"] = shared
	}
	return map[string]any{
		"score":            int(roundFloat(confidence*100, 0)),
		"status":           status,
		"retrieval_impact": impact,
		"warnings":         warnings,
		"signals":          signals,
	}
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

type memoryEdgeInferredDoc struct {
	doc    memoryEdgeBackfillDoc
	tokens map[string]struct{}
}

type memoryEdgeInferredScore struct {
	targetIndex int
	shared      int
	score       float64
	reason      string
}

func (g *memoryEdgeBackfillGenerator) generateInferredRelatedEdges(ctx context.Context) {
	if g.request.InferredPeerLimit < 1 || len(g.docs) < 2 {
		return
	}
	docs := boundedInferredMemoryDocs(g.docs, g.request.InferredScanLimit)
	inferredDocs := make([]memoryEdgeInferredDoc, 0, len(docs))
	for _, doc := range docs {
		tokens := memoryEdgeInferenceTokens(doc)
		if len(tokens) < g.request.InferredMinShared {
			continue
		}
		inferredDocs = append(inferredDocs, memoryEdgeInferredDoc{doc: doc, tokens: tokens})
	}
	if len(inferredDocs) < 2 {
		return
	}

	postings := map[string][]int{}
	for idx, doc := range inferredDocs {
		for token := range doc.tokens {
			postings[token] = append(postings[token], idx)
		}
	}
	for token, ids := range postings {
		sort.Slice(ids, func(i, j int) bool {
			return strings.ToLower(inferredDocs[ids[i]].doc.MemoryID) < strings.ToLower(inferredDocs[ids[j]].doc.MemoryID)
		})
		postings[token] = ids
	}

	for sourceIndex, source := range inferredDocs {
		select {
		case <-ctx.Done():
			g.ctxErr = ctx.Err()
			return
		default:
		}
		sharedByTarget := map[int]int{}
		for token := range source.tokens {
			ids := postings[token]
			if len(ids) == 0 || len(ids) > g.request.InferredMaxPostings {
				continue
			}
			for _, targetIndex := range ids {
				if targetIndex == sourceIndex {
					continue
				}
				sharedByTarget[targetIndex] += 1
			}
		}

		scored := make([]memoryEdgeInferredScore, 0, len(sharedByTarget))
		for targetIndex, shared := range sharedByTarget {
			if shared < g.request.InferredMinShared {
				continue
			}
			target := inferredDocs[targetIndex]
			if !strings.EqualFold(source.doc.Project, target.doc.Project) {
				continue
			}
			score, reason := inferredMemoryEdgeScore(source, target, shared)
			if score < g.request.InferredMinScore {
				continue
			}
			scored = append(scored, memoryEdgeInferredScore{
				targetIndex: targetIndex,
				shared:      shared,
				score:       score,
				reason:      reason,
			})
		}
		sort.Slice(scored, func(i, j int) bool {
			if scored[i].score != scored[j].score {
				return scored[i].score > scored[j].score
			}
			if scored[i].shared != scored[j].shared {
				return scored[i].shared > scored[j].shared
			}
			leftID := strings.ToLower(inferredDocs[scored[i].targetIndex].doc.MemoryID)
			rightID := strings.ToLower(inferredDocs[scored[j].targetIndex].doc.MemoryID)
			return leftID < rightID
		})

		addedForSource := 0
		for _, item := range scored {
			if addedForSource >= g.request.InferredPeerLimit {
				break
			}
			target := inferredDocs[item.targetIndex]
			sourceID, targetID, topicPath := orderedInferredMemoryEdgePair(source.doc, target.doc)
			candidate, err := memoryEdgeBackfillCandidateEdge(
				sourceID,
				targetID,
				g.request.InferredRelation,
				item.score,
				topicPath,
				"bounded_inferred_similarity",
				item.reason,
			)
			if err != nil {
				continue
			}
			candidate.Edge.Provenance["kind"] = "inferred_memory_edge_scoring"
			candidate.Edge.Provenance["version"] = memoryEdgeInferredScoringVersion
			candidate.Edge.Provenance["shared_terms"] = item.shared
			candidate.Edge.Provenance["min_shared_terms"] = g.request.InferredMinShared
			candidate.Edge.Provenance["min_score"] = g.request.InferredMinScore
			candidate.Edge.Metadata["inferred"] = true
			candidate.Edge.Metadata["shared_terms"] = item.shared
			candidate.Edge.Metadata["scoring_version"] = memoryEdgeInferredScoringVersion
			g.add(ctx, candidate)
			addedForSource += 1
			if g.ctxErr != nil || g.truncated {
				return
			}
		}
	}
}

func boundedInferredMemoryDocs(docs []memoryEdgeBackfillDoc, limit int) []memoryEdgeBackfillDoc {
	if limit < 1 || len(docs) <= limit {
		return append([]memoryEdgeBackfillDoc(nil), docs...)
	}
	candidates := append([]memoryEdgeBackfillDoc(nil), docs...)
	sort.Slice(candidates, func(i, j int) bool {
		left := memoryBackfillDocTouch(candidates[i])
		right := memoryBackfillDocTouch(candidates[j])
		if !left.Equal(right) {
			return left.After(right)
		}
		return strings.ToLower(candidates[i].MemoryID) < strings.ToLower(candidates[j].MemoryID)
	})
	candidates = candidates[:limit]
	sort.Slice(candidates, func(i, j int) bool {
		return memoryBackfillDocLess(candidates[i], candidates[j])
	})
	return candidates
}

func memoryBackfillDocTouch(doc memoryEdgeBackfillDoc) time.Time {
	if !doc.LastTouch.IsZero() {
		return doc.LastTouch
	}
	return doc.UpdatedAt
}

func memoryEdgeInferenceTokens(doc memoryEdgeBackfillDoc) map[string]struct{} {
	rawTokens := lexicalTokenSet(strings.TrimSpace(doc.Summary + " " + doc.TopicPath + " " + doc.FileName))
	tokens := map[string]struct{}{}
	for token := range rawTokens {
		if _, skip := memoryEdgeInferenceStopWords[token]; skip {
			continue
		}
		tokens[token] = struct{}{}
	}
	return tokens
}

var memoryEdgeInferenceStopWords = map[string]struct{}{
	"about": {}, "after": {}, "again": {}, "against": {}, "all": {}, "also": {}, "and": {},
	"any": {}, "are": {}, "because": {}, "been": {}, "before": {}, "being": {}, "between": {},
	"but": {}, "can": {}, "could": {}, "did": {}, "does": {}, "done": {}, "each": {},
	"for": {}, "from": {}, "had": {}, "has": {}, "have": {}, "into": {}, "its": {},
	"more": {}, "not": {}, "now": {}, "only": {}, "our": {}, "out": {}, "over": {},
	"same": {}, "should": {}, "that": {}, "the": {}, "their": {}, "then": {}, "there": {},
	"these": {}, "this": {}, "through": {}, "under": {}, "using": {}, "was": {}, "were": {},
	"when": {}, "where": {}, "which": {}, "while": {}, "with": {}, "would": {},
}

func inferredMemoryEdgeScore(source memoryEdgeInferredDoc, target memoryEdgeInferredDoc, shared int) (float64, string) {
	union := len(source.tokens) + len(target.tokens) - shared
	if union < 1 {
		union = 1
	}
	jaccard := float64(shared) / float64(union)
	score := 0.62 + 0.28*jaccard
	reasons := []string{fmt.Sprintf("summary_token_overlap:%d", shared)}
	sourceTopic := strings.Trim(strings.ToLower(source.doc.TopicPath), "/")
	targetTopic := strings.Trim(strings.ToLower(target.doc.TopicPath), "/")
	switch {
	case sourceTopic != "" && sourceTopic == targetTopic && sourceTopic != "root":
		score += 0.08
		reasons = append(reasons, "exact_topic")
	case sourceTopic != "" && targetTopic != "" &&
		(strings.HasPrefix(sourceTopic, targetTopic+"/") || strings.HasPrefix(targetTopic, sourceTopic+"/")):
		score += 0.04
		reasons = append(reasons, "topic_prefix")
	}
	if memoryFileDirectory(source.doc.FileName) != "" && memoryFileDirectory(source.doc.FileName) == memoryFileDirectory(target.doc.FileName) {
		score += 0.03
		reasons = append(reasons, "same_directory")
	}
	if source.doc.Project != "" && strings.EqualFold(source.doc.Project, target.doc.Project) {
		score += 0.02
		reasons = append(reasons, "same_project")
	}
	if shared >= 6 {
		score += 0.02
		reasons = append(reasons, "strong_overlap")
	}
	if score > 0.99 {
		score = 0.99
	}
	return score, strings.Join(reasons, ",")
}

func memoryFileDirectory(fileName string) string {
	trimmed := strings.TrimSpace(fileName)
	idx := strings.LastIndex(trimmed, "/")
	if idx <= 0 {
		return ""
	}
	return strings.ToLower(trimmed[:idx])
}

func orderedInferredMemoryEdgePair(left memoryEdgeBackfillDoc, right memoryEdgeBackfillDoc) (string, string, string) {
	leftID := strings.ToLower(left.MemoryID)
	rightID := strings.ToLower(right.MemoryID)
	if leftID < rightID || (leftID == rightID && left.MemoryID <= right.MemoryID) {
		return left.MemoryID, right.MemoryID, left.TopicPath
	}
	return right.MemoryID, left.MemoryID, right.TopicPath
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
		if excluded, _ := g.store.memoryGraphArtifactExcluded(project, fileName, topicPath); excluded {
			g.skippedLowValueHistory += 1
			continue
		}
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

func (s *server) maintenanceMemoryGraphPruneVolatile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if !s.writeAuthorizedRequest(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "Invalid API key"})
		return
	}
	if s.memoryStore == nil || !s.memoryStore.policy.enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "go memory store is disabled"})
		return
	}
	dryRun := true
	bodyBytes, err := readRequestBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "failed to read request body"})
		return
	}
	if strings.TrimSpace(string(bodyBytes)) != "" {
		payload, err := parseJSONMap(bodyBytes)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid json", "detail": err.Error()})
			return
		}
		if _, exists := payload["dry_run"]; exists {
			dryRun = anyToBool(payload["dry_run"])
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	result, err := s.memoryStore.pruneVolatileMemoryGraphEdges(ctx, dryRun)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "memory graph prune failed", "detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}
