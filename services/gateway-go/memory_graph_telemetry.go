package main

import (
	"context"
	"errors"
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
	Project            string                `json:"project"`
	Docs               int                   `json:"docs"`
	ConnectedDocs      int                   `json:"connected_docs"`
	IsolatedDocs       int                   `json:"isolated_docs"`
	Edges              int                   `json:"edges"`
	InferredEdges      int                   `json:"inferred_edges"`
	ExplicitEdges      int                   `json:"explicit_edges"`
	InboundEdges       int                   `json:"inbound_edges"`
	OutboundEdges      int                   `json:"outbound_edges"`
	DensityEdgesPerDoc float64               `json:"density_edges_per_doc"`
	TopRelations       []graphTelemetryCount `json:"top_relations"`
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
	OK                  bool                    `json:"ok"`
	Source              string                  `json:"source"`
	GeneratedAt         string                  `json:"generated_at"`
	ProjectFilter       string                  `json:"project_filter,omitempty"`
	Corpus              string                  `json:"corpus"`
	Status              string                  `json:"status"`
	DocCount            int                     `json:"doc_count"`
	EdgeCount           int                     `json:"edge_count"`
	ConnectedDocCount   int                     `json:"connected_doc_count"`
	IsolatedDocCount    int                     `json:"isolated_doc_count"`
	InferredEdgeCount   int                     `json:"inferred_edge_count"`
	ExplicitEdgeCount   int                     `json:"explicit_edge_count"`
	DensityEdgesPerDoc  float64                 `json:"density_edges_per_doc"`
	Projects            []graphTelemetryProject `json:"projects"`
	Relations           []graphTelemetryCount   `json:"relations"`
	Lifecycles          []graphTelemetryCount   `json:"lifecycles"`
	TopNodes            []graphTelemetryNode    `json:"top_nodes"`
	Recommendations     []string                `json:"recommendations"`
	EdgeStore           map[string]any          `json:"edge_store"`
	DocCollectionStatus string                  `json:"doc_collection_status"`
}

type graphTelemetryProjectStats struct {
	docs          int
	docIDs        map[string]struct{}
	connected     map[string]struct{}
	isolated      int
	edges         int
	inferred      int
	explicit      int
	inbound       int
	outbound      int
	relationCount map[string]int
}

type graphTelemetryNodeStats struct {
	memoryID string
	project  string
	file     string
	inbound  int
	outbound int
}

func (m *memoryStore) memoryGraphTelemetrySnapshot(ctx context.Context, projectFilter string, includeEphemeral bool, topLimit int) (graphTelemetrySnapshot, error) {
	if m == nil || !m.policy.enabled {
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

	docStatus := "history_index"
	docs, ok := m.collectDocsFromHistoryIndex(ctx, projectFilter)
	if !ok {
		var err error
		docs, err = m.collectDocsFromDisk(ctx, projectFilter, true, includeEphemeral)
		if err != nil {
			return graphTelemetrySnapshot{}, err
		}
		docStatus = "disk_fallback"
	}

	docIDs := map[string]struct{}{}
	projectStats := map[string]*graphTelemetryProjectStats{}
	for _, doc := range docs {
		memoryID := doc.Project + "::" + doc.FileName
		docIDs[strings.ToLower(memoryID)] = struct{}{}
		stats := projectStats[strings.ToLower(doc.Project)]
		if stats == nil {
			stats = &graphTelemetryProjectStats{docIDs: map[string]struct{}{}, connected: map[string]struct{}{}, relationCount: map[string]int{}}
			projectStats[strings.ToLower(doc.Project)] = stats
		}
		stats.docs += 1
		stats.docIDs[strings.ToLower(memoryID)] = struct{}{}
	}

	m.mu.RLock()
	edges := make([]memoryEdgeEntry, 0, len(m.edgeOrder))
	for _, edgeID := range m.edgeOrder {
		edge, exists := m.edges[edgeID]
		if !exists {
			continue
		}
		if projectFilter != "" && !strings.EqualFold(edge.Project, projectFilter) {
			continue
		}
		if !includeEphemeral && !shouldSurfaceMemoryLifecycle(edge.Lifecycle, false) {
			continue
		}
		edges = append(edges, edge)
	}
	m.mu.RUnlock()

	relationCounts := map[string]int{}
	lifecycleCounts := map[string]int{}
	nodeStats := map[string]*graphTelemetryNodeStats{}
	inferredCount := 0
	explicitCount := 0
	for _, edge := range edges {
		projectKey := strings.ToLower(edge.Project)
		stats := projectStats[projectKey]
		if stats == nil {
			stats = &graphTelemetryProjectStats{docIDs: map[string]struct{}{}, connected: map[string]struct{}{}, relationCount: map[string]int{}}
			projectStats[projectKey] = stats
		}
		stats.edges += 1
		stats.relationCount[edge.Relation] += 1
		relationCounts[edge.Relation] += 1
		lifecycle := normalizeMemoryLifecycle(edge.Lifecycle)
		lifecycleCounts[lifecycle] += 1
		inferred := memoryEdgeTelemetryInferred(edge)
		if inferred {
			inferredCount += 1
			stats.inferred += 1
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
	if len(docs) == 0 && len(edges) == 0 {
		status = "empty"
	} else if len(docs) > 0 && len(edges) == 0 {
		status = "no_edges"
	} else if len(docs) > 0 && float64(len(edges))/float64(len(docs)) < 0.15 {
		status = "sparse"
	}

	return graphTelemetrySnapshot{
		OK:                  true,
		Source:              "go_memory_store",
		GeneratedAt:         time.Now().UTC().Format(time.RFC3339Nano),
		ProjectFilter:       projectFilter,
		Corpus:              "memory_store",
		Status:              status,
		DocCount:            len(docs),
		EdgeCount:           len(edges),
		ConnectedDocCount:   connectedDocCount,
		IsolatedDocCount:    isolatedDocCount,
		InferredEdgeCount:   inferredCount,
		ExplicitEdgeCount:   explicitCount,
		DensityEdgesPerDoc:  ratioFloat(len(edges), len(docs)),
		Projects:            graphTelemetryProjects(projectStats, topLimit),
		Relations:           topGraphTelemetryCounts(relationCounts, topLimit),
		Lifecycles:          topGraphTelemetryCounts(lifecycleCounts, topLimit),
		TopNodes:            graphTelemetryTopNodes(nodeStats, topLimit),
		Recommendations:     graphTelemetryRecommendations(status, len(docs), len(edges), inferredCount, isolatedDocCount),
		EdgeStore:           m.memoryGraphEdgeStoreInfo(),
		DocCollectionStatus: docStatus,
	}, nil
}

func (s *graphTelemetryProjectStats) connectedDocCount() int {
	if s == nil {
		return 0
	}
	if len(s.docIDs) == 0 {
		return len(s.connected)
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

func graphTelemetryProjects(stats map[string]*graphTelemetryProjectStats, limit int) []graphTelemetryProject {
	rows := make([]graphTelemetryProject, 0, len(stats))
	for key, item := range stats {
		if item == nil {
			continue
		}
		rows = append(rows, graphTelemetryProject{
			Project:            key,
			Docs:               item.docs,
			ConnectedDocs:      item.connectedDocCount(),
			IsolatedDocs:       item.isolated,
			Edges:              item.edges,
			InferredEdges:      item.inferred,
			ExplicitEdges:      item.explicit,
			InboundEdges:       item.inbound,
			OutboundEdges:      item.outbound,
			DensityEdgesPerDoc: ratioFloat(item.edges, item.docs),
			TopRelations:       topGraphTelemetryCounts(item.relationCount, 5),
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
	if len(rows) > limit {
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
		recommendations = append(recommendations, "Run scripts/agent/memory-edge-backfill --include-inferred --inferred-min-score 0.80 --inferred-peer-limit 5 before graph-neighbor debugging.")
	}
	if isolated > 0 && docs > 0 && float64(isolated)/float64(docs) > 0.35 {
		recommendations = append(recommendations, "High isolated-doc ratio: run disk corpus edge backfill for older projects or bootstrap missing project summaries.")
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
		"path":                   m.policy.edgePath,
		"max_edges":              m.policy.maxEdges,
		"startup_max_lines":      m.policy.edgeStartupMaxLines,
		"max_edge_neighbors":     m.policy.maxEdgeNeighbors,
		"history_startup_lines":  m.policy.historyStartupMaxLines,
		"history_tail_max_bytes": m.policy.historyStartupTailMaxBytes,
	}
	if stat, err := os.Stat(m.policy.edgePath); err == nil {
		info["bytes"] = stat.Size()
		info["updated_at"] = stat.ModTime().UTC().Format(time.RFC3339Nano)
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
	if s.memoryStore == nil || !s.memoryStore.policy.enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "go memory store is disabled"})
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	includeEphemeral := anyToBool(r.URL.Query().Get("include_ephemeral"))
	limit := clampInt(anyToInt(r.URL.Query().Get("limit"), 10), 1, 50)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	snapshot, err := s.memoryStore.memoryGraphTelemetrySnapshot(ctx, project, includeEphemeral, limit)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}
