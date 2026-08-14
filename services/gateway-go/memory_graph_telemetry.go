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
	"time"
)

type graphTelemetryCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type graphTelemetryProject struct {
	Project                   string                `json:"project"`
	Docs                      int                   `json:"docs"`
	ConnectedDocs             int                   `json:"connected_docs"`
	IsolatedDocs              int                   `json:"isolated_docs"`
	ConnectednessClaim        string                `json:"connectedness_claim"`
	Edges                     int                   `json:"edges"`
	BoundEdges                int                   `json:"bound_edges"`
	UnboundEdges              int                   `json:"unbound_edges"`
	HistoricalEdges           int                   `json:"historical_edges"`
	RetiredEdges              int                   `json:"retired_edges"`
	InferredEdges             int                   `json:"inferred_edges"`
	StaleInferredEdges        int                   `json:"stale_inferred_edges"`
	ExplicitEdges             int                   `json:"explicit_edges"`
	TotalInferredEdges        int                   `json:"total_inferred_edges"`
	TotalStaleInferredEdges   int                   `json:"total_stale_inferred_edges"`
	TotalExplicitEdges        int                   `json:"total_explicit_edges"`
	InboundEdges              int                   `json:"inbound_edges"`
	OutboundEdges             int                   `json:"outbound_edges"`
	DensityEdgesPerDoc        float64               `json:"density_edges_per_doc"`
	IsolationRatio            float64               `json:"isolation_ratio"`
	MaxNodeDegree             int                   `json:"max_node_degree"`
	OverconnectedAnchorCount  int                   `json:"overconnected_anchor_count"`
	QualityScore              int                   `json:"quality_score"`
	QualityStatus             string                `json:"quality_status"`
	QualityReasons            []string              `json:"quality_reasons"`
	NeedsBackfill             bool                  `json:"needs_backfill"`
	RecommendedBackfillCorpus string                `json:"recommended_backfill_corpus"`
	TopRelations              []graphTelemetryCount `json:"top_relations"`
	TotalRelations            []graphTelemetryCount `json:"total_relations"`
}

type graphTelemetryNode struct {
	MemoryID string `json:"memory_id"`
	Project  string `json:"project"`
	File     string `json:"file"`
	Degree   int    `json:"degree"`
	Inbound  int    `json:"inbound"`
	Outbound int    `json:"outbound"`
}

type graphTelemetrySnapshot struct {
	OK                          bool                    `json:"ok"`
	Source                      string                  `json:"source"`
	GeneratedAt                 string                  `json:"generated_at"`
	ProjectFilter               string                  `json:"project_filter,omitempty"`
	Corpus                      string                  `json:"corpus"`
	Status                      string                  `json:"status"`
	DocCount                    int                     `json:"doc_count"`
	ExcludedDocCount            int                     `json:"excluded_doc_count"`
	EdgeCount                   int                     `json:"edge_count"`
	BoundEdgeCount              int                     `json:"bound_edge_count"`
	UnboundEdgeCount            int                     `json:"unbound_edge_count"`
	HistoricalEdgeCount         int                     `json:"historical_edge_count"`
	RetiredEdgeCount            int                     `json:"retired_edge_count"`
	ConnectednessClaim          string                  `json:"connectedness_claim"`
	ConnectedDocCount           int                     `json:"connected_doc_count"`
	IsolatedDocCount            int                     `json:"isolated_doc_count"`
	InferredEdgeCount           int                     `json:"inferred_edge_count"`
	StaleInferredEdgeCount      int                     `json:"stale_inferred_edge_count"`
	ExplicitEdgeCount           int                     `json:"explicit_edge_count"`
	TotalInferredEdgeCount      int                     `json:"total_inferred_edge_count"`
	TotalStaleInferredEdgeCount int                     `json:"total_stale_inferred_edge_count"`
	TotalExplicitEdgeCount      int                     `json:"total_explicit_edge_count"`
	DensityEdgesPerDoc          float64                 `json:"density_edges_per_doc"`
	QualityStatus               string                  `json:"quality_status"`
	QualityScore                int                     `json:"quality_score"`
	RepairProjectCount          int                     `json:"repair_project_count"`
	Projects                    []graphTelemetryProject `json:"projects"`
	Relations                   []graphTelemetryCount   `json:"relations"`
	TotalRelations              []graphTelemetryCount   `json:"total_relations"`
	Lifecycles                  []graphTelemetryCount   `json:"lifecycles"`
	TotalLifecycles             []graphTelemetryCount   `json:"total_lifecycles"`
	TopNodes                    []graphTelemetryNode    `json:"top_nodes"`
	Recommendations             []string                `json:"recommendations"`
	EdgeStore                   map[string]any          `json:"edge_store"`
	DocCollectionStatus         string                  `json:"doc_collection_status"`
}

type graphTelemetryProjectStats struct {
	docs                     int
	docIDs                   map[string]struct{}
	connected                map[string]struct{}
	isolated                 int
	edges                    int
	boundEdges               int
	unboundEdges             int
	inferred                 int
	staleInferred            int
	explicit                 int
	totalInferred            int
	totalStaleInferred       int
	totalExplicit            int
	historicalRows           int
	retired                  int
	inbound                  int
	outbound                 int
	maxNodeDegree            int
	overconnectedAnchorCount int
	relationCount            map[string]int
	totalRelationCount       map[string]int
}

type graphTelemetryNodeStats struct {
	memoryID string
	project  string
	file     string
	inbound  int
	outbound int
}

func (m *memoryStore) memoryGraphTelemetryCurrentDocs(ctx context.Context, projectFilter string, includeEphemeral bool) ([]memoryStoreDoc, int, error) {
	projects := []string{}
	if projectFilter != "" {
		projects = append(projects, projectFilter)
	} else {
		indexed := map[string]struct{}{}
		current := map[string]struct{}{}
		m.mu.RLock()
		if m.currentKeysByProject == nil || m.currentState == nil || m.currentKeyIndexGeneration == nil || m.currentTopicIndexGeneration == nil {
			m.mu.RUnlock()
			return nil, 0, errors.New("current-state index is unavailable")
		}
		if len(m.currentState) > memoryGraphRepairMaxDocs || len(m.currentKeysByProject) > memoryGraphRepairMaxDocs {
			m.mu.RUnlock()
			return nil, 0, errors.New("current-state index exceeds bounded telemetry snapshot cap")
		}
		for project := range m.currentKeysByProject {
			indexed[project] = struct{}{}
		}
		for key, state := range m.currentState {
			if state.Tombstone {
				continue
			}
			project, _, ok := parseMemoryStoreKeyToken(key)
			if ok {
				current[normalizeCurrentKeyIndexProject(project)] = struct{}{}
			}
		}
		m.mu.RUnlock()
		for project := range current {
			if _, ok := indexed[project]; !ok {
				return nil, 0, errors.New("current-state rows are missing an exact index projection")
			}
		}
		for project := range indexed {
			projects = append(projects, project)
		}
		sort.Strings(projects)
	}
	docs := []memoryStoreDoc{}
	excluded := 0
	for _, project := range projects {
		snapshot, err := m.captureMemoryGraphRepairSnapshot(ctx, memoryGraphRepairRequest{Project: project, IncludeCold: true, IncludeEphemeral: includeEphemeral})
		if err != nil {
			return nil, excluded, fmt.Errorf("current-state index snapshot for %s: %w", project, err)
		}
		excluded += snapshot.ExcludedCount
		for _, doc := range snapshot.Docs {
			docs = append(docs, memoryStoreDoc{Project: doc.Project, FileName: doc.FileName, TopicPath: doc.TopicPath, Summary: doc.Summary, ObjectID: doc.ObjectID, UpdatedAt: doc.UpdatedAt, LastTouch: doc.LastTouch, Lifecycle: doc.Lifecycle, StorageTier: doc.StorageTier})
		}
	}
	return docs, excluded, nil
}

func (m *memoryStore) memoryGraphTelemetryDurableEdges() ([]memoryEdgeEntry, map[string]memoryEdgeEntry, error) {
	return m.memoryGraphTelemetryDurableEdgesContext(context.Background())
}

func (m *memoryStore) memoryGraphTelemetryDurableEdgesContext(ctx context.Context) ([]memoryEdgeEntry, map[string]memoryEdgeEntry, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	maxBytes := m.policy.historyStartupTailMaxBytes
	if maxBytes < 1 || maxBytes > memoryGraphRepairMaxEdgeBytes {
		maxBytes = memoryGraphRepairMaxEdgeBytes
	}
	snapshot, err := m.snapshotMemoryEdgeLogContext(ctx, maxBytes)
	if err != nil {
		return nil, nil, err
	}
	maxLines := m.policy.edgeStartupMaxLines
	if maxLines < 1 || maxLines > memoryGraphRepairMaxEdgeLines {
		maxLines = memoryGraphRepairMaxEdgeLines
	}
	rows := []memoryEdgeEntry{}
	latest := map[string]memoryEdgeEntry{}
	scanner := bufio.NewScanner(bytes.NewReader(snapshot.Bytes))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		default:
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if len(rows) >= maxLines {
			return nil, nil, errors.New("durable edge evidence exceeds telemetry snapshot cap")
		}
		var edge memoryEdgeEntry
		if json.Unmarshal(line, &edge) != nil {
			return nil, nil, errors.New("durable edge evidence contains an invalid row")
		}
		normalized, normalizeErr := edge.normalized()
		if normalizeErr != nil {
			return nil, nil, errors.New("durable edge evidence contains a noncanonical row")
		}
		rows = append(rows, normalized)
		latest[normalized.EdgeID] = normalized
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, err
	}
	return rows, latest, nil
}

func (m *memoryStore) memoryGraphTelemetrySnapshot(ctx context.Context, projectFilter string, includeEphemeral bool, topLimit int, staleInferredAfter time.Time) (graphTelemetrySnapshot, error) {
	if m == nil || !m.isEnabled() {
		return graphTelemetrySnapshot{}, errors.New("go memory store is disabled")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	topLimit = clampInt(topLimit, 1, 50)
	projectFilter = strings.TrimSpace(projectFilter)
	if projectFilter != "" {
		clean, err := sanitizeMemoryProject(projectFilter)
		if err != nil {
			return graphTelemetrySnapshot{}, err
		}
		projectFilter = clean
	}

	docStatus := "current_state_index"
	docs, excludedDocs, err := m.memoryGraphTelemetryCurrentDocs(ctx, projectFilter, includeEphemeral)
	if err != nil {
		return graphTelemetrySnapshot{}, err
	}

	docIDs := map[string]struct{}{}
	projectStats := map[string]*graphTelemetryProjectStats{}
	for _, doc := range docs {
		memoryID := doc.Project + "::" + doc.FileName
		docIDs[strings.ToLower(memoryID)] = struct{}{}
		stats := projectStats[strings.ToLower(doc.Project)]
		if stats == nil {
			stats = &graphTelemetryProjectStats{docIDs: map[string]struct{}{}, connected: map[string]struct{}{}, relationCount: map[string]int{}, totalRelationCount: map[string]int{}}
			projectStats[strings.ToLower(doc.Project)] = stats
		}
		stats.docs += 1
		stats.docIDs[strings.ToLower(memoryID)] = struct{}{}
	}

	historicalRows, durableLatest, err := m.memoryGraphTelemetryDurableEdgesContext(ctx)
	if err != nil {
		return graphTelemetrySnapshot{}, err
	}
	historicalByProject := map[string]int{}
	for _, edge := range historicalRows {
		if projectFilter == "" || strings.EqualFold(edge.Project, projectFilter) {
			historicalByProject[strings.ToLower(edge.Project)]++
		}
	}
	edges := make([]memoryEdgeEntry, 0, len(durableLatest))
	retiredEdgeCount := 0
	for _, edge := range durableLatest {
		if projectFilter != "" && !strings.EqualFold(edge.Project, projectFilter) {
			continue
		}
		projectKey := strings.ToLower(edge.Project)
		stats := projectStats[projectKey]
		if stats == nil {
			stats = &graphTelemetryProjectStats{docIDs: map[string]struct{}{}, connected: map[string]struct{}{}, relationCount: map[string]int{}, totalRelationCount: map[string]int{}}
			projectStats[projectKey] = stats
		}
		stats.historicalRows = historicalByProject[projectKey]
		if memoryGraphRepairEdgeRetired(edge) {
			retiredEdgeCount++
			stats.retired++
			continue
		}
		if !includeEphemeral && !shouldSurfaceMemoryLifecycle(edge.Lifecycle, false) {
			continue
		}
		if excluded, _ := m.memoryGraphEdgeExcluded(edge); excluded {
			continue
		}
		edges = append(edges, edge)
	}
	relationCounts := map[string]int{}
	totalRelationCounts := map[string]int{}
	lifecycleCounts := map[string]int{}
	totalLifecycleCounts := map[string]int{}
	nodeStats := map[string]*graphTelemetryNodeStats{}
	inferredCount := 0
	staleInferredCount := 0
	explicitCount := 0
	totalInferredCount := 0
	totalStaleInferredCount := 0
	totalExplicitCount := 0
	boundEdgeCount := 0
	unboundEdgeCount := 0
	for _, edge := range edges {
		projectKey := strings.ToLower(edge.Project)
		stats := projectStats[projectKey]
		if stats == nil {
			stats = &graphTelemetryProjectStats{docIDs: map[string]struct{}{}, connected: map[string]struct{}{}, relationCount: map[string]int{}, totalRelationCount: map[string]int{}}
			projectStats[projectKey] = stats
		}
		stats.edges++
		stats.totalRelationCount[edge.Relation]++
		totalRelationCounts[edge.Relation]++
		lifecycle := normalizeMemoryLifecycle(edge.Lifecycle)
		totalLifecycleCounts[lifecycle]++
		inferred := memoryEdgeTelemetryInferred(edge)
		if inferred {
			totalInferredCount++
			stats.totalInferred++
			if !staleInferredAfter.IsZero() {
				if createdAt, ok := parseTimeBestEffort(edge.CreatedAt); ok && createdAt.Before(staleInferredAfter) {
					totalStaleInferredCount++
					stats.totalStaleInferred++
				}
			}
		} else {
			totalExplicitCount++
			stats.totalExplicit++
		}
		sourceBound, targetBound := false, false
		if _, _, sourceCanonical, _, sourceErr := canonicalMemoryID(edge.SourceID); sourceErr == nil {
			_, sourceBound = stats.docIDs[strings.ToLower(sourceCanonical)]
		}
		if _, _, targetCanonical, _, targetErr := canonicalMemoryID(edge.TargetID); targetErr == nil {
			_, targetBound = stats.docIDs[strings.ToLower(targetCanonical)]
		}
		if memoryGraphEdgeRequiresBinding(edge) && !m.referenceEdgeCurrent(edge) {
			// Preserve historical evidence in total/unbound telemetry while
			// preventing stale or unbound promoted relations from creating
			// current topology, relation, or connectedness claims.
			sourceBound, targetBound = false, false
		}
		if sourceBound && targetBound {
			stats.boundEdges++
			boundEdgeCount++
		} else {
			stats.unboundEdges++
			unboundEdgeCount++
		}
		if !(sourceBound && targetBound) {
			// Historical/unbound evidence remains visible in total counters, but
			// cannot create current-state topology or connectedness claims.
			continue
		}
		stats.relationCount[edge.Relation] += 1
		relationCounts[edge.Relation] += 1
		lifecycleCounts[lifecycle] += 1
		if inferred {
			inferredCount += 1
			stats.inferred += 1
			if !staleInferredAfter.IsZero() {
				if createdAt, ok := parseTimeBestEffort(edge.CreatedAt); ok && createdAt.Before(staleInferredAfter) {
					staleInferredCount += 1
					stats.staleInferred += 1
				}
			}
		} else {
			explicitCount += 1
			stats.explicit += 1
		}
		recordNode := func(memoryID string, outbound bool) {
			project, fileName, canonical, _, err := canonicalMemoryID(memoryID)
			if err != nil {
				return
			}
			key := strings.ToLower(canonical)
			node := nodeStats[key]
			if node == nil {
				node = &graphTelemetryNodeStats{memoryID: canonical, project: project, file: fileName}
				nodeStats[key] = node
			}
			if outbound {
				node.outbound += 1
				stats.outbound += 1
			} else {
				node.inbound += 1
				stats.inbound += 1
			}
			stats.connected[key] = struct{}{}
		}
		recordNode(edge.SourceID, true)
		recordNode(edge.TargetID, false)
	}
	graphTelemetryApplyNodeQuality(projectStats, nodeStats)

	connectedDocCount := 0
	for id := range docIDs {
		if _, exists := nodeStats[id]; exists {
			connectedDocCount += 1
		}
	}
	isolatedDocCount := len(docIDs) - connectedDocCount
	if isolatedDocCount < 0 {
		isolatedDocCount = 0
	}
	for key, stats := range projectStats {
		connectedDocs := stats.connectedDocCount()
		stats.isolated = stats.docs - connectedDocs
		if stats.isolated < 0 {
			stats.isolated = 0
		}
		if stats.docs == 0 && stats.edges == 0 {
			delete(projectStats, key)
		}
	}

	status := "healthy"
	if len(docs) == 0 && boundEdgeCount == 0 {
		status = "empty"
	} else if len(docs) > 0 && boundEdgeCount == 0 {
		status = "no_edges"
	} else if len(docs) > 0 && float64(boundEdgeCount)/float64(len(docs)) < 0.15 {
		status = "sparse"
	}

	allProjects := graphTelemetryProjects(projectStats, 0)
	projects := graphTelemetryProjects(projectStats, topLimit)
	qualityStatus, qualityScore, repairProjectCount := graphTelemetryGlobalQuality(allProjects, status)
	historicalEdgeCount := 0
	for _, count := range historicalByProject {
		historicalEdgeCount += count
	}
	return graphTelemetrySnapshot{
		OK:                          true,
		Source:                      "go_memory_store",
		GeneratedAt:                 time.Now().UTC().Format(time.RFC3339Nano),
		ProjectFilter:               projectFilter,
		Corpus:                      "gateway_current_state_index",
		Status:                      status,
		DocCount:                    len(docs),
		ExcludedDocCount:            excludedDocs,
		EdgeCount:                   len(edges),
		BoundEdgeCount:              boundEdgeCount,
		UnboundEdgeCount:            unboundEdgeCount,
		HistoricalEdgeCount:         historicalEdgeCount,
		RetiredEdgeCount:            retiredEdgeCount,
		ConnectednessClaim:          "bound_current_state_intersection_only",
		ConnectedDocCount:           connectedDocCount,
		IsolatedDocCount:            isolatedDocCount,
		InferredEdgeCount:           inferredCount,
		StaleInferredEdgeCount:      staleInferredCount,
		ExplicitEdgeCount:           explicitCount,
		TotalInferredEdgeCount:      totalInferredCount,
		TotalStaleInferredEdgeCount: totalStaleInferredCount,
		TotalExplicitEdgeCount:      totalExplicitCount,
		DensityEdgesPerDoc:          ratioFloat(boundEdgeCount, len(docs)),
		QualityStatus:               qualityStatus,
		QualityScore:                qualityScore,
		RepairProjectCount:          repairProjectCount,
		Projects:                    projects,
		Relations:                   topGraphTelemetryCounts(relationCounts, topLimit),
		TotalRelations:              topGraphTelemetryCounts(totalRelationCounts, topLimit),
		Lifecycles:                  topGraphTelemetryCounts(lifecycleCounts, topLimit),
		TotalLifecycles:             topGraphTelemetryCounts(totalLifecycleCounts, topLimit),
		TopNodes:                    graphTelemetryTopNodes(nodeStats, topLimit),
		Recommendations:             graphTelemetryRecommendations(status, len(docs), boundEdgeCount, inferredCount, isolatedDocCount),
		EdgeStore:                   m.memoryGraphEdgeStoreInfo(),
		DocCollectionStatus:         docStatus,
	}, nil
}

func (s *graphTelemetryProjectStats) connectedDocCount() int {
	if s == nil {
		return 0
	}
	if len(s.docIDs) == 0 {
		return 0
	}
	count := 0
	for id := range s.docIDs {
		if _, exists := s.connected[id]; exists {
			count += 1
		}
	}
	return count
}

func memoryEdgeTelemetryInferred(edge memoryEdgeEntry) bool {
	if anyToBool(edge.Metadata["inferred"]) {
		return true
	}
	kind := strings.ToLower(strings.TrimSpace(anyToString(edge.Provenance["kind"])))
	return strings.Contains(kind, "inferred")
}

func ratioFloat(numerator int, denominator int) float64 {
	if denominator <= 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}

func graphTelemetryAnchorThreshold(docs int) int {
	if docs <= 0 {
		return 0
	}
	threshold := (docs + 3) / 4
	if docs < 20 {
		threshold = docs * 4
	}
	if threshold < 12 {
		threshold = 12
	}
	return threshold
}

func graphTelemetryApplyNodeQuality(projectStats map[string]*graphTelemetryProjectStats, nodeStats map[string]*graphTelemetryNodeStats) {
	for _, node := range nodeStats {
		if node == nil {
			continue
		}
		stats := projectStats[strings.ToLower(node.project)]
		if stats == nil {
			continue
		}
		degree := node.inbound + node.outbound
		if degree > stats.maxNodeDegree {
			stats.maxNodeDegree = degree
		}
		threshold := graphTelemetryAnchorThreshold(stats.docs)
		if threshold > 0 && degree >= threshold {
			stats.overconnectedAnchorCount += 1
		}
	}
}

func graphTelemetryProjectQuality(stats *graphTelemetryProjectStats) (int, string, bool, string, []string) {
	if stats == nil {
		return 100, "healthy", false, "current_state_index", nil
	}
	score := 100
	needsBackfill := false
	reasons := []string{}
	recommendedCorpus := "current_state_index"
	density := ratioFloat(stats.boundEdges, stats.docs)
	isolationRatio := ratioFloat(stats.isolated, stats.docs)

	if stats.docs == 0 {
		// An edge-only project is historical/unbound evidence. It is retained
		// for custody, but cannot lower current-state topology quality.
		return clampInt(score, 0, 100), graphTelemetryQualityStatus(score, false, reasons), false, recommendedCorpus, reasons
	}
	if stats.isolated > 0 {
		penalty := int(isolationRatio * 80)
		if stats.isolated >= 100 && penalty < 15 {
			penalty = 15
		}
		score -= clampInt(penalty, 5, 35)
		if isolationRatio >= 0.10 || stats.isolated >= 100 {
			needsBackfill = true
			recommendedCorpus = "current_state_index"
			reasons = append(reasons, "high_isolated_doc_coverage")
		} else {
			reasons = append(reasons, "some_isolated_docs")
		}
	}
	if density < 0.50 {
		score -= 25
		needsBackfill = true
		reasons = append(reasons, "sparse_edge_density")
	} else if density < 1.00 {
		score -= 10
		reasons = append(reasons, "low_edge_density")
	}
	if stats.inferred == 0 && stats.docs >= 2 {
		score -= 20
		needsBackfill = true
		reasons = append(reasons, "missing_inferred_edges")
	}
	if stats.staleInferred > 0 {
		score -= minInt(15, maxInt(5, stats.staleInferred/100))
		reasons = append(reasons, "stale_inferred_edges")
	}
	if stats.overconnectedAnchorCount > 0 {
		score -= minInt(20, stats.overconnectedAnchorCount*5)
		reasons = append(reasons, "overconnected_anchor_nodes")
	}
	score = clampInt(score, 0, 100)
	return score, graphTelemetryQualityStatus(score, needsBackfill, reasons), needsBackfill, recommendedCorpus, reasons
}

func graphTelemetryQualityStatus(score int, needsBackfill bool, reasons []string) string {
	if needsBackfill {
		return "repair_recommended"
	}
	if score < 85 || len(reasons) > 0 {
		return "watch"
	}
	return "healthy"
}

func graphTelemetryGlobalQuality(projects []graphTelemetryProject, fallbackStatus string) (string, int, int) {
	if len(projects) == 0 {
		if fallbackStatus == "empty" {
			return "empty", 100, 0
		}
		return "healthy", 100, 0
	}
	score := 100
	repairProjects := 0
	watchProjects := 0
	for _, project := range projects {
		if project.QualityScore < score {
			score = project.QualityScore
		}
		if project.NeedsBackfill {
			repairProjects += 1
		}
		if project.QualityStatus == "watch" {
			watchProjects += 1
		}
	}
	if repairProjects > 0 {
		return "repair_recommended", score, repairProjects
	}
	if watchProjects > 0 || score < 85 {
		return "watch", score, repairProjects
	}
	return "healthy", score, repairProjects
}

func graphTelemetryProjects(stats map[string]*graphTelemetryProjectStats, limit int) []graphTelemetryProject {
	rows := make([]graphTelemetryProject, 0, len(stats))
	for key, item := range stats {
		if item == nil {
			continue
		}
		qualityScore, qualityStatus, needsBackfill, corpus, reasons := graphTelemetryProjectQuality(item)
		rows = append(rows, graphTelemetryProject{
			Project:                   key,
			Docs:                      item.docs,
			ConnectedDocs:             item.connectedDocCount(),
			IsolatedDocs:              item.isolated,
			ConnectednessClaim:        "bound_current_state_intersection_only",
			Edges:                     item.edges,
			BoundEdges:                item.boundEdges,
			UnboundEdges:              item.unboundEdges,
			HistoricalEdges:           item.historicalRows,
			RetiredEdges:              item.retired,
			InferredEdges:             item.inferred,
			StaleInferredEdges:        item.staleInferred,
			ExplicitEdges:             item.explicit,
			TotalInferredEdges:        item.totalInferred,
			TotalStaleInferredEdges:   item.totalStaleInferred,
			TotalExplicitEdges:        item.totalExplicit,
			InboundEdges:              item.inbound,
			OutboundEdges:             item.outbound,
			DensityEdgesPerDoc:        ratioFloat(item.boundEdges, item.docs),
			IsolationRatio:            ratioFloat(item.isolated, item.docs),
			MaxNodeDegree:             item.maxNodeDegree,
			OverconnectedAnchorCount:  item.overconnectedAnchorCount,
			QualityScore:              qualityScore,
			QualityStatus:             qualityStatus,
			QualityReasons:            reasons,
			NeedsBackfill:             needsBackfill,
			RecommendedBackfillCorpus: corpus,
			TopRelations:              topGraphTelemetryCounts(item.relationCount, 5),
			TotalRelations:            topGraphTelemetryCounts(item.totalRelationCount, 5),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Edges != rows[j].Edges {
			return rows[i].Edges > rows[j].Edges
		}
		if rows[i].Docs != rows[j].Docs {
			return rows[i].Docs > rows[j].Docs
		}
		return rows[i].Project < rows[j].Project
	})
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}

func topGraphTelemetryCounts(counts map[string]int, limit int) []graphTelemetryCount {
	rows := make([]graphTelemetryCount, 0, len(counts))
	for name, count := range counts {
		rows = append(rows, graphTelemetryCount{Name: name, Count: count})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		return rows[i].Name < rows[j].Name
	})
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}

func graphTelemetryTopNodes(stats map[string]*graphTelemetryNodeStats, limit int) []graphTelemetryNode {
	rows := make([]graphTelemetryNode, 0, len(stats))
	for _, item := range stats {
		if item == nil {
			continue
		}
		rows = append(rows, graphTelemetryNode{
			MemoryID: item.memoryID,
			Project:  item.project,
			File:     item.file,
			Degree:   item.inbound + item.outbound,
			Inbound:  item.inbound,
			Outbound: item.outbound,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Degree != rows[j].Degree {
			return rows[i].Degree > rows[j].Degree
		}
		return strings.ToLower(rows[i].MemoryID) < strings.ToLower(rows[j].MemoryID)
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}

func graphTelemetryRecommendations(status string, docs int, edges int, inferred int, isolated int) []string {
	recommendations := []string{}
	if docs > 0 && edges == 0 {
		recommendations = append(recommendations, "Run bounded current-state-index graph repair with an authenticated operator plan before graph-neighbor debugging.")
	}
	if isolated > 0 && docs > 0 && float64(isolated)/float64(docs) > 0.35 {
		recommendations = append(recommendations, "High isolated-doc ratio: run bounded current-state-index graph repair; historical or unbound edge evidence cannot establish connectedness.")
	}
	if edges > 0 && inferred == 0 {
		recommendations = append(recommendations, "No inferred edges visible: enable inferred_related backfill when semantic relationship recall is expected.")
	}
	if status == "healthy" && len(recommendations) == 0 {
		recommendations = append(recommendations, "Graph density is sufficient for relationship recall; inspect top nodes for over-connected memory anchors.")
	}
	return recommendations
}

func (m *memoryStore) memoryGraphEdgeStoreInfo() map[string]any {
	info := map[string]any{
		"edge_store_ref":         ownerOnlyStoreRef("memory_edges"),
		"max_edges":              m.policy.maxEdges,
		"startup_max_lines":      m.policy.edgeStartupMaxLines,
		"max_edge_neighbors":     m.policy.maxEdgeNeighbors,
		"history_startup_lines":  m.policy.historyStartupMaxLines,
		"history_tail_max_bytes": m.policy.historyStartupTailMaxBytes,
	}
	if stamp, err := memoryEdgeLogPlatformFileStamp(m.policy.edgePath); err == nil {
		info["bytes"] = stamp.Size
		info["updated_at"] = time.Unix(0, stamp.ModTimeNanos).UTC().Format(time.RFC3339Nano)
	} else if errors.Is(err, os.ErrNotExist) {
		info["bytes"] = 0
	}
	return info
}

func (s *server) telemetryMemoryGraphRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	if s.memoryStore == nil || !s.memoryStore.isEnabled() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "go memory store is disabled"})
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	includeEphemeral := anyToBool(r.URL.Query().Get("include_ephemeral"))
	limit := clampInt(anyToInt(r.URL.Query().Get("limit"), 10), 1, 50)
	staleDays := clampInt(anyToInt(r.URL.Query().Get("stale_inferred_days"), 30), 1, 3650)
	staleAfter := time.Now().UTC().Add(-time.Duration(staleDays) * 24 * time.Hour)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	snapshot, err := s.memoryStore.memoryGraphTelemetrySnapshot(ctx, project, includeEphemeral, limit, staleAfter)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}
