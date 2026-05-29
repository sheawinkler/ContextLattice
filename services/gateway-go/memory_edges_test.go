package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func newMemoryGraphTestServer(t *testing.T, strictNoPython bool) (*server, *httptest.Server) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("BACKEND_URL", "http://127.0.0.1:1")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", filepath.Join(root, "_contextlattice", "memory_write_history.ndjson"))
	t.Setenv("GO_MEMORY_STORE_ACCESS_LOG_PATH", filepath.Join(root, "_contextlattice", "memory_access_log.ndjson"))
	t.Setenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH", filepath.Join(root, "_contextlattice", "objects"))
	t.Setenv("GO_MEMORY_GRAPH_EDGE_PATH", filepath.Join(root, "_contextlattice", "memory_edges.ndjson"))
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_RETRIEVAL_CONTINUATION_DURABLE_ENABLED", "false")
	t.Setenv("GO_RUNTIME_STRICT_NO_PYTHON", map[bool]string{true: "true", false: "false"}[strictNoPython])
	if !envBool("GO_GATEWAY_TEST_KEEP_ORCH_KEY", false) {
		t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "")
	}
	s := newServer()
	gateway := httptest.NewServer(buildMux(s))
	return s, gateway
}

func TestMemoryEdgesWriteListAndReload(t *testing.T) {
	_, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()

	body := `{"source_id":"alpha::notes/a.md","target_id":"alpha::notes/b.md","relation":"depends-on","confidence":0.72,"topic_path":"runbooks/testing","provenance":{"kind":"unit"},"agent_id":"agent-a"}`
	resp, err := http.Post(gateway.URL+"/v1/memory/edges", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("edge write failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(raw))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode edge response: %v", err)
	}
	edge, _ := payload["edge"].(map[string]any)
	edgeID := strings.TrimSpace(anyToString(payload["edge_id"]))
	if edgeID == "" || !strings.HasPrefix(edgeID, "edge_") {
		t.Fatalf("expected deterministic edge_id, got %#v", payload["edge_id"])
	}
	if got := anyToString(edge["relation"]); got != "depends_on" {
		t.Fatalf("expected normalized relation depends_on, got %q", got)
	}
	if got := anyToFloat64(edge["confidence"], 0); got != 0.72 {
		t.Fatalf("expected confidence 0.72, got %#v", edge["confidence"])
	}

	listResp, err := http.Get(gateway.URL + "/v1/memory/edges?memory_id=alpha%3A%3Anotes%2Fa.md&direction=out&relation=depends_on")
	if err != nil {
		t.Fatalf("edge list failed: %v", err)
	}
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(listResp.Body)
		t.Fatalf("expected 200 list, got %d body=%s", listResp.StatusCode, string(raw))
	}
	var listPayload map[string]any
	if err := json.NewDecoder(listResp.Body).Decode(&listPayload); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if anyToInt(listPayload["count"], 0) != 1 {
		t.Fatalf("expected count=1, got %#v", listPayload)
	}

	reloaded, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("reload memory store: %v", err)
	}
	edges, err := reloaded.listMemoryEdges(context.Background(), memoryEdgeQuery{
		MemoryID: "alpha::notes/a.md",
		Relation: "depends_on",
		Limit:    10,
	})
	if err != nil {
		t.Fatalf("list reloaded edges: %v", err)
	}
	if len(edges) != 1 || edges[0].EdgeID != edgeID {
		t.Fatalf("expected reloaded edge %s, got %#v", edgeID, edges)
	}
}

func TestMemoryV1NeighborsReturnsExplicitEdgesWhenRetrievalDisabled(t *testing.T) {
	_, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()

	resp, err := http.Post(
		gateway.URL+"/v1/memory/edges",
		"application/json",
		strings.NewReader(`{"source_id":"alpha::notes/a.md","target_id":"alpha::notes/b.md","relation":"blocks","confidence":0.9,"topic_path":"runbooks/testing"}`),
	)
	if err != nil {
		t.Fatalf("edge write failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected edge write 200, got %d", resp.StatusCode)
	}

	neighbors, err := http.Post(
		gateway.URL+"/v1/memory/neighbors",
		"application/json",
		strings.NewReader(`{"memory_id":"alpha::notes/a.md","limit":5}`),
	)
	if err != nil {
		t.Fatalf("neighbors request failed: %v", err)
	}
	defer neighbors.Body.Close()
	if neighbors.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(neighbors.Body)
		t.Fatalf("expected 200 neighbors, got %d body=%s", neighbors.StatusCode, string(raw))
	}
	var payload map[string]any
	if err := json.NewDecoder(neighbors.Body).Decode(&payload); err != nil {
		t.Fatalf("decode neighbors: %v", err)
	}
	results, _ := payload["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("expected one edge neighbor, got %#v", payload)
	}
	row, _ := results[0].(map[string]any)
	if anyToString(row["memory_id"]) != "alpha::notes/b.md" {
		t.Fatalf("expected target neighbor, got %#v", row)
	}
	if anyToString(row["source"]) != memoryEdgeSource || anyToString(row["relation"]) != "blocks" {
		t.Fatalf("expected memory edge relation row, got %#v", row)
	}
	if anyToString(row["edge_direction"]) != "out" {
		t.Fatalf("expected outbound edge direction, got %#v", row["edge_direction"])
	}
}

func TestMergeNeighborRowsPrefersExplicitEdgesAndDedupesRetrieval(t *testing.T) {
	edgeRows := []map[string]any{
		{"memory_id": "alpha::notes/b.md", "project": "alpha", "file": "notes/b.md", "source": memoryEdgeSource, "score": 0.9},
	}
	retrievalRows := []any{
		map[string]any{"project": "alpha", "file": "notes/b.md", "source": "qdrant", "score": 0.8},
		map[string]any{"project": "alpha", "file": "notes/c.md", "source": "topic_rollups", "score": 0.7},
	}
	merged := mergeNeighborRows("alpha::notes/a.md", edgeRows, retrievalRows, 10)
	if len(merged) != 2 {
		t.Fatalf("expected two merged rows after dedupe, got %#v", merged)
	}
	first, _ := merged[0].(map[string]any)
	second, _ := merged[1].(map[string]any)
	if anyToString(first["source"]) != memoryEdgeSource {
		t.Fatalf("expected explicit edge first, got %#v", first)
	}
	if anyToString(second["file"]) != "notes/c.md" {
		t.Fatalf("expected non-duplicate retrieval row second, got %#v", second)
	}
}

func TestMemoryEdgesBackfillDryRunWriteAndIdempotency(t *testing.T) {
	s, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()

	writes := []normalizedWrite{
		{
			project:   "alpha",
			fileName:  "notes/a.md",
			content:   "A references alpha::notes/b.md",
			topicPath: "runbooks/testing",
			agentID:   "agent-a",
			sessionID: "session-1",
		},
		{
			project:   "alpha",
			fileName:  "notes/b.md",
			content:   "B implementation notes",
			topicPath: "runbooks/testing",
			agentID:   "agent-a",
			sessionID: "session-1",
		},
		{
			project:   "alpha",
			fileName:  "notes/c.md",
			content:   "C related runbook",
			topicPath: "runbooks/testing",
			agentID:   "agent-a",
			sessionID: "session-1",
		},
	}
	for _, write := range writes {
		if _, _, err := s.memoryStore.put(write); err != nil {
			t.Fatalf("seed memory write %s: %v", write.fileName, err)
		}
	}

	dryRun := postEdgeBackfillForTest(t, gateway.URL, `{"project":"alpha","sample_limit":20}`)
	if !anyToBool(dryRun["dry_run"]) {
		t.Fatalf("expected dry_run default true, got %#v", dryRun)
	}
	if anyToInt(dryRun["written"], -1) != 0 {
		t.Fatalf("dry run must not write edges, got %#v", dryRun)
	}
	if anyToInt(dryRun["eligible"], 0) == 0 || anyToInt(dryRun["would_write"], 0) == 0 {
		t.Fatalf("expected eligible dry-run candidates, got %#v", dryRun)
	}
	if got := backfillRelationStatInt(dryRun, "same_agent", "skipped_below_confidence"); got == 0 {
		t.Fatalf("expected low-confidence same_agent audit rows to be skipped, got %#v", dryRun["relations"])
	}
	if edges, err := s.memoryStore.listMemoryEdges(context.Background(), memoryEdgeQuery{MemoryID: "alpha::notes/a.md", Limit: 100}); err != nil || len(edges) != 0 {
		t.Fatalf("dry-run should leave edge store empty, edges=%#v err=%v", edges, err)
	}

	writeRun := postEdgeBackfillForTest(t, gateway.URL, `{"dry_run":false,"project":"alpha","sample_limit":20}`)
	written := anyToInt(writeRun["written"], 0)
	if written == 0 {
		t.Fatalf("expected write mode to persist high-confidence candidates, got %#v", writeRun)
	}
	if got := backfillRelationStatInt(writeRun, "same_agent", "written"); got != 0 {
		t.Fatalf("same_agent is audit-only at default threshold, wrote=%d report=%#v", got, writeRun["relations"])
	}
	edges, err := s.memoryStore.listMemoryEdges(context.Background(), memoryEdgeQuery{MemoryID: "alpha::notes/a.md", Limit: 100})
	if err != nil {
		t.Fatalf("list written edges: %v", err)
	}
	if len(edges) == 0 {
		t.Fatalf("expected written backfill edges")
	}

	repeatRun := postEdgeBackfillForTest(t, gateway.URL, `{"dry_run":false,"project":"alpha","sample_limit":20}`)
	if anyToInt(repeatRun["written"], -1) != 0 {
		t.Fatalf("repeat backfill should be idempotent and skip existing edges, got %#v", repeatRun)
	}
	if anyToInt(repeatRun["existing"], 0) == 0 {
		t.Fatalf("repeat backfill should report existing edges, got %#v", repeatRun)
	}
}

func TestMemoryEdgesBackfillInferredScoringBounded(t *testing.T) {
	s, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()

	writes := []normalizedWrite{
		{
			project:   "alpha",
			fileName:  "notes/a.md",
			content:   "qdrant vector recall latency memory graph shared context scoring qdrant",
			topicPath: "runbooks/graph",
			agentID:   "agent-a",
			sessionID: "session-1",
		},
		{
			project:   "alpha",
			fileName:  "notes/b.md",
			content:   "qdrant vector recall latency memory graph shared context scoring",
			topicPath: "runbooks/graph",
			agentID:   "agent-b",
			sessionID: "session-2",
		},
		{
			project:   "alpha",
			fileName:  "notes/d.md",
			content:   "qdrant vector recall latency memory graph shared context scoring rerank",
			topicPath: "runbooks/graph",
			agentID:   "agent-c",
			sessionID: "session-3",
		},
		{
			project:   "alpha",
			fileName:  "billing/invoice.md",
			content:   "billing invoice entitlement provider route pricing plan",
			topicPath: "billing",
			agentID:   "agent-b",
			sessionID: "session-4",
		},
	}
	for _, write := range writes {
		if _, _, err := s.memoryStore.put(write); err != nil {
			t.Fatalf("seed memory write %s: %v", write.fileName, err)
		}
	}

	capped := postEdgeBackfillForTest(t, gateway.URL, `{"project":"alpha","relations":["inferred_related"],"include_inferred":true,"max_candidates":1,"inferred_peer_limit":2,"inferred_min_score":0.9,"inferred_min_shared_terms":3,"sample_limit":20}`)
	if !anyToBool(capped["truncated"]) {
		t.Fatalf("expected max_candidates cap to truncate inferred scoring, got %#v", capped)
	}
	if got := backfillRelationStatInt(capped, "inferred_related", "generated"); got < 2 {
		t.Fatalf("expected multiple inferred candidates before truncation, got %#v", capped["relations"])
	}

	dryRun := postEdgeBackfillForTest(t, gateway.URL, `{"project":"alpha","relations":["inferred_related"],"include_inferred":true,"min_confidence":0.9,"inferred_peer_limit":1,"inferred_min_score":0.9,"inferred_min_shared_terms":3,"sample_limit":20}`)
	if !anyToBool(dryRun["dry_run"]) || !anyToBool(dryRun["include_inferred"]) {
		t.Fatalf("expected inferred dry-run response, got %#v", dryRun)
	}
	if got := backfillRelationStatInt(dryRun, "inferred_related", "eligible"); got == 0 {
		t.Fatalf("expected eligible inferred candidates, got %#v", dryRun["relations"])
	}
	if anyToInt(dryRun["written"], -1) != 0 {
		t.Fatalf("dry run must not write inferred edges, got %#v", dryRun)
	}
	if edges, err := s.memoryStore.listMemoryEdges(context.Background(), memoryEdgeQuery{Relation: "inferred_related", Limit: 100}); err != nil || len(edges) != 0 {
		t.Fatalf("dry-run should leave inferred edge store empty, edges=%#v err=%v", edges, err)
	}

	writeRun := postEdgeBackfillForTest(t, gateway.URL, `{"dry_run":false,"project":"alpha","relations":["inferred_related"],"include_inferred":true,"min_confidence":0.9,"inferred_peer_limit":1,"inferred_min_score":0.9,"inferred_min_shared_terms":3,"sample_limit":20}`)
	written := anyToInt(writeRun["written"], 0)
	if written == 0 || written > 2 {
		t.Fatalf("expected bounded inferred writes, got %#v", writeRun)
	}
	edges, err := s.memoryStore.listMemoryEdges(context.Background(), memoryEdgeQuery{Relation: "inferred_related", Limit: 100})
	if err != nil {
		t.Fatalf("list inferred edges: %v", err)
	}
	if len(edges) != written {
		t.Fatalf("expected %d inferred edges, got %#v", written, edges)
	}
	for _, edge := range edges {
		if anyToString(edge.Provenance["kind"]) != "inferred_memory_edge_scoring" {
			t.Fatalf("expected inferred provenance, got %#v", edge.Provenance)
		}
		if !anyToBool(edge.Metadata["inferred"]) {
			t.Fatalf("expected inferred metadata, got %#v", edge.Metadata)
		}
		if edge.Confidence < 0.9 {
			t.Fatalf("expected persisted inferred confidence >= 0.9, got %#v", edge)
		}
	}

	repeatRun := postEdgeBackfillForTest(t, gateway.URL, `{"dry_run":false,"project":"alpha","relations":["inferred_related"],"include_inferred":true,"min_confidence":0.9,"inferred_peer_limit":1,"inferred_min_score":0.9,"inferred_min_shared_terms":3,"sample_limit":20}`)
	if anyToInt(repeatRun["written"], -1) != 0 {
		t.Fatalf("repeat inferred backfill should be idempotent, got %#v", repeatRun)
	}
	if anyToInt(repeatRun["existing"], 0) == 0 {
		t.Fatalf("repeat inferred backfill should report existing edges, got %#v", repeatRun)
	}
}

func postEdgeBackfillForTest(t *testing.T, baseURL string, body string) map[string]any {
	t.Helper()
	resp, err := http.Post(baseURL+"/v1/memory/edges/backfill", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("edge backfill request failed: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 backfill response, got %d body=%s", resp.StatusCode, string(raw))
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode backfill response: %v body=%s", err, string(raw))
	}
	return payload
}

func backfillRelationStatInt(payload map[string]any, relation string, field string) int {
	relations, _ := payload["relations"].(map[string]any)
	rawStat, _ := relations[relation].(map[string]any)
	return anyToInt(rawStat[field], 0)
}
