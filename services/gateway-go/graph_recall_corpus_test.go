package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type graphRecallRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn graphRecallRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func graphRecallTestPairedLatencyEvidence(count int, delta float64) map[string]any {
	totals := graphRecallEvaluationTotals{
		positiveExpected:       count,
		treatmentLatencyValues: make([]float64, 0, count),
		controlLatencyValues:   make([]float64, 0, count),
		latencyDeltaValues:     make([]float64, 0, count),
		pairedLatencySamples:   make([]graphRecallPairedLatencySample, 0, count),
		pairedLatencyCases:     count,
	}
	for index := 0; index < count; index++ {
		control := 100.0 + float64(index)
		treatment := control + delta
		totals.controlLatencyValues = append(totals.controlLatencyValues, control)
		totals.treatmentLatencyValues = append(totals.treatmentLatencyValues, treatment)
		totals.latencyDeltaValues = append(totals.latencyDeltaValues, delta)
		totals.pairedLatencySamples = append(totals.pairedLatencySamples, graphRecallPairedLatencySample{CaseID: fmt.Sprintf("latency-case-%03d", index), GraphTreatmentMS: treatment, DirectControlMS: control})
	}
	return graphRecallPairedLatencySummary(totals)
}

func graphRecallTestHardNegativeIdentityReceipt(count int, sourceEdgeDigest string) map[string]any {
	receipt := map[string]any{
		"schema_id": graphRecallHardNegativeCurrentIdentitySchemaID, "version": graphRecallHardNegativeCurrentIdentityVersion,
		"authority": "gateway_current_state_index", "server_owned": true, "all_current": true,
		"expected_cases": count, "observed_cases": count, "source_edge_snapshot_digest": sourceEdgeDigest,
		"identity_digest": "sha256:fixture", "captured_at": nowUTCISO(),
	}
	receipt["digest"] = "sha256:" + graphCorpusDigestMap(receipt, "digest")
	return receipt
}

func graphCorpusFixtureCandidates(count int) []recallEvalSourceCandidate {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	candidates := make([]recallEvalSourceCandidate, 0, count)
	for index := 0; index < count; index++ {
		project := fmt.Sprintf("project-%d", index%10)
		file := fmt.Sprintf("notes/memory-%03d.md", index)
		topic := fmt.Sprintf("graph/topic/%d", index%15)
		updated := base.Add(time.Duration(index) * 24 * time.Hour)
		candidates = append(candidates, recallEvalSourceCandidate{
			doc:     memoryStoreDoc{Project: project, FileName: file, TopicPath: topic, Summary: fmt.Sprintf("bounded graph fixture summary %d", index), UpdatedAt: updated},
			agentID: fmt.Sprintf("agent-family-%d", index%8), sessionID: fmt.Sprintf("session-%02d", index%40), sourceFamily: fmt.Sprintf("source-%d", index%4), createdAt: updated,
			stableKey: recallEvalCandidateStableKey(project, file, topic),
		})
	}
	return candidates
}

func graphCorpusFixtureEdges(candidates []recallEvalSourceCandidate) []memoryEdgeEntry {
	relations := []string{"references", "same_session", "same_topic"}
	edges := make([]memoryEdgeEntry, 0, len(candidates)*len(relations))
	for index, candidate := range candidates {
		_, _, sourceID, _, _ := canonicalMemoryID(candidate.doc.Project + "::" + candidate.doc.FileName)
		for relationIndex, relation := range relations {
			offset := []int{10, 40, 30}[relationIndex]
			target := candidates[(index+offset)%len(candidates)]
			_, _, targetID, _, _ := canonicalMemoryID(target.doc.Project + "::" + target.doc.FileName)
			if relation == "references" && !strings.Contains(candidates[index].doc.Summary, targetID) {
				candidates[index].doc.Summary += " references " + targetID
			}
			edges = append(edges, memoryEdgeEntry{
				EdgeID: deterministicMemoryEdgeID(sourceID, relation, targetID), SourceID: sourceID, TargetID: targetID,
				Relation: relation, Project: candidate.doc.Project, TopicPath: candidate.doc.TopicPath, Confidence: 0.99,
				CreatedAt: candidate.createdAt.Format(time.RFC3339Nano), Source: memoryEdgeSource,
			})
		}
	}
	return edges
}

func bindGraphCorpusFixtureRuntime(t *testing.T) {
	t.Helper()
	originalCommit, originalTree := contextLatticeSourceCommit, contextLatticeSourceTree
	contextLatticeSourceCommit, contextLatticeSourceTree = strings.Repeat("c", 40), strings.Repeat("d", 40)
	t.Cleanup(func() { contextLatticeSourceCommit, contextLatticeSourceTree = originalCommit, originalTree })
}

func newGraphRecallFixtureMemoryStore(t *testing.T) *memoryStore {
	t.Helper()
	root := t.TempDir()
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", filepath.Join(root, "_contextlattice", "memory_write_history.ndjson"))
	t.Setenv("GO_MEMORY_STORE_CURRENT_STATE_PATH", filepath.Join(root, "_contextlattice", "memory_current_state.ndjson"))
	t.Setenv("GO_MEMORY_STORE_ACCESS_LOG_PATH", filepath.Join(root, "_contextlattice", "memory_access_log.ndjson"))
	t.Setenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH", filepath.Join(root, "_contextlattice", "objects"))
	t.Setenv("GO_MEMORY_GRAPH_EDGE_PATH", filepath.Join(root, "_contextlattice", "memory_edges.ndjson"))
	t.Setenv("GO_MEMORY_STORE_BACKGROUND_HYDRATION_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_BACKGROUND_ENABLED", "false")
	store, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("create graph recall fixture store: %v", err)
	}
	if !store.isEnabled() {
		t.Fatal("graph recall fixture store did not become ready")
	}
	return store
}

func graphCorpusFixtureIncrementalControls(candidates []recallEvalSourceCandidate, edges []memoryEdgeEntry, seam map[string]any) map[string]any {
	records, _ := graphCorpusBuildRecords(candidates, edges)
	controls := map[string]any{}
	completeSeedCount := len(candidates)
	edgeSnapshotDigest := "sha256:" + graphCorpusDigestMap(map[string]any{"schema_id": "saved_recall_graph_edge_snapshot.v1", "complete": true, "truncated": false, "continuation_complete": true, "candidate_count": len(candidates), "complete_seed_count": completeSeedCount, "edge_digest": "sha256:" + graphCorpusEdgesDigest(edges)})
	sourceSnapshotDigest := "sha256:" + graphCorpusCandidatesDigest(candidates)
	sourceEdgeSnapshotDigest := graphCorpusSourceEdgeSnapshotDigest(candidates, edges)
	controlVisible := map[string]any{
		"production_response_seam": true, "production_final_response_seam": true,
		"production_response_schema_id": recallResponseContractID, "production_response_digest": "sha256:" + strings.Repeat("e", 64), "production_response_scope_digest": "sha256:" + strings.Repeat("f", 64), "production_response_contract_valid": true,
		"side_effects_suppressed": true, "competing_interventions_disabled": true,
		"final_model_visible_k": defaultRecallEvalK, "final_model_visible_ordered": true,
		"final_model_visible_evidence": []map[string]any{}, "final_model_visible_memory_refs": []string{}, "final_model_visible_file_refs": []string{},
	}
	controlVisible["final_model_visible_digest"] = graphRecallFinalModelVisibleDigest(controlVisible)
	for _, record := range records {
		request := map[string]any{
			"query": record.Query, "limit": defaultRecallEvalK, "project": record.Project, "topic_path": record.TopicPath,
			"retrieval_mode": "balanced", "retrieval_intent": "decision", "sources": []string{sourceTopicRollup},
			"include_grounding": true, "include_retrieval_debug": false, "include_preferences": false, "rerank_with_learning": false,
			"user_id": "", "agent_id": record.AgentID, "auto_escalate": false, "deep_async": false,
			"callback_url": "", "traffic_class": "evaluation_holdout",
		}
		response := map[string]any{"results": []map[string]any{}, "grounding": map[string]any{}, "retrieval_mode": "balanced", "retrieval_intent": "decision", "traffic_class": "evaluation_holdout"}
		controls[record.LineageKey] = map[string]any{
			"schema_id": savedRecallGraphIncrementalControlSchemaID, "version": savedRecallGraphIncrementalControlVersion, "authority": savedRecallGraphIncrementalControlAuthority,
			"graph_influence_disabled": true, "graph_backend_consulted": false, "graph_results_used": false,
			"candidate_allocation_active": false, "treatment_active": false, "traffic_class": "evaluation_holdout",
			"seed_target_lineage": record.LineageKey, "seed_memory_id": record.SeedID, "target_memory_id": record.TargetID,
			"target_direct_hit": false, "control_snapshot_digest": sourceEdgeSnapshotDigest, "source_snapshot_digest": sourceSnapshotDigest, "edge_snapshot_digest": edgeSnapshotDigest,
			"control_k": defaultRecallEvalK, "control_request_digest": graphRecallControlRequestDigest(request), "control_response_digest": graphRecallControlResponseDigest(response),
			"control_final_model_visible": cloneJSONMap(controlVisible), "control_composition_path": "production_final_response_seam_graph_disabled",
			"control_latency_ms": 5.0, "control_latency_scope": graphRecallPairedLatencyScope, "control_latency_comparable": true,
			"cost_observability": cloneJSONMap(seam), "source_runtime_identity": contextLatticeBuildIdentity(),
			"captured_at": "2026-01-01T00:00:00Z", "control_path": "fixture_server_graph_disabled",
		}
	}
	return controls
}

func graphCorpusFixtureSourceStats(t *testing.T, candidates []recallEvalSourceCandidate, edges []memoryEdgeEntry) map[string]any {
	t.Helper()
	bindGraphCorpusFixtureRuntime(t)
	completeSeedIDs := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		_, _, memoryID, _, _ := canonicalMemoryID(candidate.doc.Project + "::" + candidate.doc.FileName)
		completeSeedIDs = append(completeSeedIDs, memoryID)
	}
	edgeSnapshot := map[string]any{"schema_id": "saved_recall_graph_edge_snapshot.v1", "complete": true, "truncated": false, "continuation_complete": true, "candidate_count": len(candidates), "complete_seed_count": len(completeSeedIDs), "edge_digest": "sha256:" + graphCorpusEdgesDigest(edges)}
	edgeSnapshot["digest"] = "sha256:" + graphCorpusDigestMap(edgeSnapshot)
	store := &memoryStore{policy: memoryStorePolicy{enabled: true}}
	store.ready.Store(true)
	fixtureServer := &server{memoryStore: store}
	preflight := retrievalEvaluationSourcePreflight(fixtureServer, []string{sourceTopicRollup})
	seam := retrievalCostObservabilityEnvelope(
		fixtureServer,
		map[string]any{"traffic_class": "evaluation_holdout", "_evaluation_source_preflight": preflight},
		[]string{sourceTopicRollup}, map[string]string{sourceTopicRollup: sourceOwnerGoNative},
		map[string][]map[string]any{sourceTopicRollup: {}}, map[string]any{"staged_fetch": map[string]any{}},
	)
	totals := graphRecallEvaluationTotals{}
	for index := 0; index < savedRecallGraphCorpusTotalPositiveCases; index++ {
		graphRecallRecordEconomics(map[string]any{"cost_observability": seam}, &totals)
	}
	controlCost := graphCorpusControlCostReceipt(totals)
	return map[string]any{
		"bounded": true, "index_integrity": true, "index_mode": "current_state_bottom_k", "raw_store_scanned": false,
		"runtime_identity": contextLatticeBuildIdentity(), "captured_at": "2026-01-01T00:00:00Z",
		"edge_snapshot": edgeSnapshot, "complete_seed_ids": completeSeedIDs,
		"source_snapshot_digest": "sha256:" + graphCorpusCandidatesDigest(candidates), "source_edge_snapshot_digest": graphCorpusSourceEdgeSnapshotDigest(candidates, edges),
		"snapshot_stable_during_control_capture": true, "capture_project": "", "capture_topic_prefix": "",
		"incremental_controls": graphCorpusFixtureIncrementalControls(candidates, edges, seam), "incremental_control_cost": controlCost,
		"direct_baseline_binding": map[string]any{
			"schema_id": savedRecallEvalV3SchemaID, "version": savedRecallEvalV3Version, "k": defaultRecallEvalK, "case_set_digest": "sha256:fixture-direct",
			"snapshot_digest": "sha256:fixture-snapshot", "benchmark_eligible": true, "binding_kind": "frozen_direct_saved_recall_artifact", "evaluation_split": "all", "evaluation_case_set_digest": "sha256:fixture-eval", "evaluation_case_count": 1, "evaluation_traffic_class": "evaluation_holdout", "file_names_disclosed": false,
		},
	}
}

func TestGraphCorpusBuildRecordsRequiresCurrentRelationTruth(t *testing.T) {
	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	baseSeed := memoryStoreDoc{
		Project: "project-a", FileName: "notes/seed.md", TopicPath: "graph/topic",
		Summary: "current seed references project-a::notes/target.md", UpdatedAt: baseTime, Lifecycle: "durable",
	}
	baseTarget := memoryStoreDoc{
		Project: "project-a", FileName: "notes/target.md", TopicPath: "graph/topic",
		Summary: "current target evidence", UpdatedAt: baseTime.Add(time.Minute), Lifecycle: "durable",
	}
	type relationCase struct {
		name          string
		relation      string
		seed          memoryStoreDoc
		target        memoryStoreDoc
		seedSession   string
		targetSession string
		mutateEdge    func(*memoryEdgeEntry)
		want          bool
	}
	cases := []relationCase{
		{name: "current textual reference", relation: "references", seed: baseSeed, target: baseTarget, want: true},
		{name: "caller forged reference without current binding", relation: "references", seed: func() memoryStoreDoc {
			doc := baseSeed
			doc.Summary = "current seed without a target binding"
			return doc
		}(), target: baseTarget},
		{name: "stale reference points at former target", relation: "references", seed: func() memoryStoreDoc {
			doc := baseSeed
			doc.Summary = "current seed references project-a::notes/former.md"
			return doc
		}(), target: baseTarget},
		{name: "exact current session", relation: "same_session", seed: baseSeed, target: baseTarget, seedSession: "session-A", targetSession: "session-A", want: true},
		{name: "unbound current session", relation: "same_session", seed: baseSeed, target: baseTarget, seedSession: "", targetSession: "session-A"},
		{name: "session case mismatch", relation: "same_session", seed: baseSeed, target: baseTarget, seedSession: "session-A", targetSession: "session-a"},
		{name: "exact normalized current topic", relation: "same_topic", seed: baseSeed, target: baseTarget, want: true},
		{name: "stale topic relation", relation: "same_topic", seed: baseSeed, target: func() memoryStoreDoc { doc := baseTarget; doc.TopicPath = "graph/other"; return doc }()},
		{name: "topic case mismatch", relation: "same_topic", seed: baseSeed, target: func() memoryStoreDoc { doc := baseTarget; doc.TopicPath = "Graph/Topic"; return doc }()},
		{name: "retired edge", relation: "references", seed: baseSeed, target: baseTarget, mutateEdge: func(edge *memoryEdgeEntry) { edge.Lifecycle = "retired" }},
		{name: "superseded current endpoint", relation: "references", seed: baseSeed, target: func() memoryStoreDoc { doc := baseTarget; doc.Lifecycle = "superseded"; return doc }()},
		{name: "explicitly stale edge receipt", relation: "references", seed: baseSeed, target: baseTarget, mutateEdge: func(edge *memoryEdgeEntry) { edge.Metadata = map[string]any{"stale": true} }},
		{name: "unbound edge identity", relation: "references", seed: baseSeed, target: baseTarget, mutateEdge: func(edge *memoryEdgeEntry) { edge.EdgeID = "" }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, seedID, _, seedErr := canonicalMemoryID(testCase.seed.Project + "::" + testCase.seed.FileName)
			_, _, targetID, _, targetErr := canonicalMemoryID(testCase.target.Project + "::" + testCase.target.FileName)
			if seedErr != nil || targetErr != nil {
				t.Fatalf("canonical fixture identities: seed=%v target=%v", seedErr, targetErr)
			}
			edge := memoryEdgeEntry{
				EdgeID: deterministicMemoryEdgeID(seedID, testCase.relation, targetID), SourceID: seedID, TargetID: targetID,
				Relation: testCase.relation, Project: testCase.seed.Project, TopicPath: testCase.seed.TopicPath,
				Confidence: 0.99, Lifecycle: "durable", CreatedAt: baseTime.Add(2 * time.Minute).Format(time.RFC3339Nano), Source: memoryEdgeSource,
			}
			if testCase.mutateEdge != nil {
				testCase.mutateEdge(&edge)
			}
			candidates := []recallEvalSourceCandidate{
				{doc: testCase.seed, agentID: "agent-a", sessionID: testCase.seedSession, createdAt: baseTime, stableKey: recallEvalCandidateStableKey(testCase.seed.Project, testCase.seed.FileName, testCase.seed.TopicPath)},
				{doc: testCase.target, agentID: "agent-b", sessionID: testCase.targetSession, createdAt: baseTime.Add(time.Minute), stableKey: recallEvalCandidateStableKey(testCase.target.Project, testCase.target.FileName, testCase.target.TopicPath)},
			}
			records, counts := graphCorpusBuildRecords(candidates, []memoryEdgeEntry{edge})
			if got := len(records) == 1 && counts[testCase.relation] == 1; got != testCase.want {
				t.Fatalf("current relation truth result=%t want=%t records=%#v counts=%#v edge=%#v", got, testCase.want, records, counts, edge)
			}
		})
	}
}

func TestGraphCorpusHardNegativesRejectUnedgedCurrentSemanticRelations(t *testing.T) {
	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	buildCandidate := func(file, topic, summary, session string) recallEvalSourceCandidate {
		doc := memoryStoreDoc{
			Project: "project-a", FileName: file, TopicPath: topic, Summary: summary,
			UpdatedAt: baseTime, Lifecycle: "durable",
		}
		return recallEvalSourceCandidate{
			doc: doc, agentID: "agent-a", sessionID: session, createdAt: baseTime,
			stableKey: recallEvalCandidateStableKey(doc.Project, doc.FileName, doc.TopicPath),
		}
	}
	malformedUnrelatedEdge := []memoryEdgeEntry{{SourceID: "not-a-memory-id", TargetID: "also-not-a-memory-id", Relation: "references"}}
	for _, testCase := range []struct {
		name       string
		candidates []recallEvalSourceCandidate
	}{
		{
			name: "current reference without an edge row",
			candidates: []recallEvalSourceCandidate{
				buildCandidate("notes/a.md", "topic/a", "current a references project-a::notes/b.md", "session-a"),
				buildCandidate("notes/b.md", "topic/b", "current b references project-a::notes/a.md", "session-b"),
			},
		},
		{
			name: "current exact session without an edge row",
			candidates: []recallEvalSourceCandidate{
				buildCandidate("notes/a.md", "topic/a", "current a", "same-session"),
				buildCandidate("notes/b.md", "topic/b", "current b", "same-session"),
			},
		},
		{
			name: "current exact topic without an edge row",
			candidates: []recallEvalSourceCandidate{
				buildCandidate("notes/a.md", "topic/shared", "current a", "session-a"),
				buildCandidate("notes/b.md", "topic/shared", "current b", "session-b"),
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			complete := map[string]struct{}{}
			for _, candidate := range testCase.candidates {
				_, _, memoryID, _, err := canonicalMemoryID(candidate.doc.Project + "::" + candidate.doc.FileName)
				if err != nil {
					t.Fatalf("canonical candidate: %v", err)
				}
				complete[memoryID] = struct{}{}
			}
			if negatives := graphCorpusBuildNegativeRecordsWithSnapshot(testCase.candidates, nil, malformedUnrelatedEdge, complete, 1); len(negatives) != 0 {
				t.Fatalf("current semantic relation was mislabeled as a hard negative: %#v", negatives)
			}
		})
	}
}

func TestGraphCorpusHardNegativesRejectCurrentTargetContentAndSemanticOverlap(t *testing.T) {
	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	buildCandidate := func(file, topic, summary string) recallEvalSourceCandidate {
		doc := memoryStoreDoc{Project: "project-a", FileName: file, TopicPath: topic, Summary: summary, UpdatedAt: baseTime, Lifecycle: "durable"}
		return recallEvalSourceCandidate{doc: doc, agentID: "agent-" + file, sessionID: "session-" + file, createdAt: baseTime, stableKey: recallEvalCandidateStableKey(doc.Project, doc.FileName, doc.TopicPath)}
	}
	malformedUnrelatedEdge := []memoryEdgeEntry{{SourceID: "not-a-memory-id", TargetID: "also-not-a-memory-id", Relation: "references"}}
	for _, testCase := range []struct {
		name       string
		seed       recallEvalSourceCandidate
		target     recallEvalSourceCandidate
		wantRecord bool
	}{
		{
			name:       "target current content appears in seed query",
			seed:       buildCandidate("notes/seed-content.md", "topic/seed", "seed discusses private launch plan details"),
			target:     buildCandidate("notes/target-content.md", "topic/target", "private launch plan"),
			wantRecord: false,
		},
		{
			name:       "target current semantic topic anchors overlap",
			seed:       buildCandidate("notes/seed-semantic.md", "semantic/retrieval/storage", "seed storage note"),
			target:     buildCandidate("notes/target-semantic.md", "semantic/retrieval/graph", "target graph note"),
			wantRecord: false,
		},
		{
			name:       "unrelated current target remains eligible",
			seed:       buildCandidate("notes/seed-valid.md", "topic/seed", "seed bounded note"),
			target:     buildCandidate("notes/target-valid.md", "topic/other", "independent audit decision"),
			wantRecord: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			candidates := []recallEvalSourceCandidate{testCase.seed, testCase.target}
			complete := map[string]struct{}{}
			for _, candidate := range candidates {
				_, _, memoryID, _, err := canonicalMemoryID(candidate.doc.Project + "::" + candidate.doc.FileName)
				if err != nil {
					t.Fatalf("canonical candidate: %v", err)
				}
				complete[memoryID] = struct{}{}
			}
			negatives := graphCorpusBuildNegativeRecordsWithSnapshot(candidates, nil, malformedUnrelatedEdge, complete, 1)
			if got := len(negatives) == 1; got != testCase.wantRecord {
				t.Fatalf("content/semantic leakage result=%t want=%t negatives=%#v", got, testCase.wantRecord, negatives)
			}
			if testCase.wantRecord {
				record := negatives[0]
				if !record.NegativeContentChecked || !record.NegativeSemanticChecked || record.NegativeContentProof == "" || record.NegativeSemanticProof == "" {
					t.Fatalf("valid negative did not carry current-state overlap proofs: %#v", record)
				}
			}
		})
	}
}

func TestGraphRecallLeakageOverlapUsesTokenPhraseAndSemanticBoundaries(t *testing.T) {
	if got := recallEvalRedactFileTokens("metadata retention catalog", "data"); got != "metadata retention catalog" {
		t.Fatalf("substring-only redaction treated data as metadata: %q", got)
	}
	if got := recallEvalRedactFileTokens("data retention catalog", "data"); got != "retention catalog" {
		t.Fatalf("exact file token leakage was not redacted: %q", got)
	}
	if got := recallEvalRedactFileTokens("secret plan rollout", "secret-plan.md"); got != "rollout" {
		t.Fatalf("multi-token file phrase leakage was not redacted: %q", got)
	}
	if graphCorpusFileOracleOverlap("metadata retention catalog", "data") {
		t.Fatal("file oracle used raw substring overlap for data inside metadata")
	}
	if !graphCorpusFileOracleOverlap("data retention catalog", "data") || !graphCorpusFileOracleOverlap("secret plan rollout", "secret-plan.md") {
		t.Fatal("real token or phrase file leakage was not rejected")
	}
	base := memoryStoreDoc{Project: "project-a", Lifecycle: "durable", UpdatedAt: time.Now().UTC()}
	seed := base
	seed.Summary = "metadata catalog"
	seed.TopicPath = "catalog/storage"
	target := base
	target.Summary = "data"
	target.TopicPath = "privacy/classification"
	if content, semantic, reason := graphCorpusCurrentContentSemanticOverlap(seed, target); content || semantic || reason != "current_target_content_and_semantic_anchors_disjoint" {
		t.Fatalf("data was conflated with metadata: content=%t semantic=%t reason=%s", content, semantic, reason)
	}
	seed.Summary = "data retention rollout"
	target.Summary = "data retention"
	if content, _, _ := graphCorpusCurrentContentSemanticOverlap(seed, target); !content {
		t.Fatal("exact current target phrase leakage was not rejected")
	}
}

func TestGraphRecallHardNegativeForbiddenIdentityMustBeExactAndCurrent(t *testing.T) {
	record := graphRecallCorpusRecord{
		Project: "project-a", TopicPath: "graph/negative", AgentID: "agent-a", AgentFamily: "agent-a", SessionID: "session-a",
		SeedID: "project-a::notes/seed.md", TargetID: "project-a::notes/forbidden.md", SeedFile: "notes/seed.md", TargetFile: "notes/forbidden.md",
		TimeBucket: "2026-01", Query: "independent bounded audit", HardNegative: true,
	}
	record.LineageKey = graphCorpusLineageKey(record.Project, record.AgentFamily, record.SessionID, record.TimeBucket, record.SeedID, record.TargetID, "hard_negative")
	row := graphCorpusHardNegativeCase(record, "holdout", 0)
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	s := &server{memoryStore: currentStateSearchTestStore(memoryStoreEntry{
		EventID: "event-current", Project: "project-a", FileName: "notes/forbidden.md", TopicPath: "graph/negative", Summary: "independent target", Lifecycle: "durable", CreatedAt: created,
	})}
	receipt, err := s.graphRecallHardNegativeCurrentIdentityReceipt([]map[string]any{row}, "sha256:source-edge")
	if err != nil || !graphRecallHardNegativeCurrentIdentityReceiptValid(receipt, 1, "sha256:source-edge") {
		t.Fatalf("exact current forbidden identity did not validate: receipt=%#v err=%v", receipt, err)
	}
	unrelated := cloneJSONMap(row)
	unrelated["forbidden_memory_ids"] = []string{"project-a::notes/unrelated.md"}
	if _, _, _, err := graphRecallHardNegativeCanonicalIdentity(unrelated); err == nil {
		t.Fatal("unrelated forbidden memory id was accepted for the canonical forbidden file")
	}
	nonexistent := cloneJSONMap(row)
	nonexistentID := "project-a::notes/nonexistent.md"
	nonexistent["forbidden_graph_files"] = []string{"notes/nonexistent.md"}
	nonexistent["forbidden_memory_ids"] = []string{nonexistentID}
	nonexistent["negative_target_memory_id"] = nonexistentID
	nonexistent["target_memory_id"] = nonexistentID
	nonexistent["graph_target_memory_id"] = nonexistentID
	anyMap(nonexistent["negative_oracle"])["forbidden_target"] = nonexistentID
	anyMap(nonexistent["negative_oracle"])["forbidden_file"] = "notes/nonexistent.md"
	if _, err := s.graphRecallHardNegativeCurrentIdentityReceipt([]map[string]any{nonexistent}, "sha256:source-edge"); err == nil {
		t.Fatal("self-consistent but nonexistent forbidden target was accepted as current")
	}
	substituted := cloneJSONMap(row)
	substituted["project"] = "project-b"
	if _, err := s.graphRecallHardNegativeCurrentIdentityReceipt([]map[string]any{substituted}, "sha256:source-edge"); err == nil {
		t.Fatal("project-substituted forbidden identity was accepted")
	}
}

func TestGraphRecallHardNegativeIdentityFailureIsOpaqueAndCategorical(t *testing.T) {
	record := graphRecallCorpusRecord{
		Project: "project-a", TopicPath: "graph/negative", AgentID: "agent-a", AgentFamily: "agent-a", SessionID: "session-a",
		SeedID: "project-a::notes/seed.md", TargetID: "project-a::notes/forbidden.md", SeedFile: "notes/seed.md", TargetFile: "notes/forbidden.md",
		TimeBucket: "2026-01", Query: "independent bounded audit", HardNegative: true,
	}
	record.LineageKey = graphCorpusLineageKey(record.Project, record.AgentFamily, record.SessionID, record.TimeBucket, record.SeedID, record.TargetID, "hard_negative")
	row := graphCorpusHardNegativeCase(record, "holdout", 0)
	row["forbidden_memory_ids"] = nil
	_, _, _, err := graphRecallHardNegativeCanonicalIdentity(row)
	if err == nil {
		t.Fatal("missing forbidden identity was accepted")
	}
	detail := graphRecallPublicFailureDetails(err)
	encoded, marshalErr := json.Marshal(detail)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if strings.Contains(string(encoded), "project-a::notes/forbidden.md") || strings.Contains(string(encoded), "notes/forbidden.md") {
		t.Fatalf("hard-negative identity detail leaked raw project/file identity: %s", encoded)
	}
	if anyToString(detail["code"]) != "hard_negative_identity_missing" || !strings.HasPrefix(anyToString(detail["case_ref"]), "sha256:") {
		t.Fatalf("hard-negative identity detail was not stable and opaque: %#v", detail)
	}
	if got := err.Error(); got != "hard_negative_identity_missing" {
		t.Fatalf("identity error exposed unstable diagnostic text: %q", got)
	}
}

func TestGraphRecallExecutionFailureCodeIsStable(t *testing.T) {
	if got := graphRecallExecutionErrorCode(http.StatusGatewayTimeout, fmt.Errorf("private provider path")); got != "graph_retrieval_timeout" {
		t.Fatalf("timeout error was not categorized: %q", got)
	}
	if got := graphRecallExecutionErrorCode(http.StatusBadGateway, fmt.Errorf("private provider path")); got != "graph_retrieval_server_error" {
		t.Fatalf("server error was not categorized: %q", got)
	}
	if got := graphRecallExecutionErrorCode(http.StatusBadRequest, fmt.Errorf("private provider path")); got != "graph_retrieval_rejected" {
		t.Fatalf("request error was not categorized: %q", got)
	}
	if got := graphRecallExecutionErrorCode(http.StatusOK, fmt.Errorf("private provider path")); got != "graph_retrieval_failed" {
		t.Fatalf("generic error was not categorized: %q", got)
	}
}

func TestSavedRecallGraphCorpusGenerationIsClosedDeterministicAndSplit(t *testing.T) {
	candidates := graphCorpusFixtureCandidates(400)
	edges := graphCorpusFixtureEdges(candidates)
	sourceStats := graphCorpusFixtureSourceStats(t, candidates, edges)
	first := buildSavedRecallGraphCorpusFromCandidates(candidates, edges, "fixture-seed", "current_state_bottom_k", sourceStats)
	sourceStats["captured_at"] = "2026-01-02T00:00:00Z"
	second := buildSavedRecallGraphCorpusFromCandidates(candidates, edges, "fixture-seed", "current_state_bottom_k", sourceStats)
	if anyToString(first["case_set_digest"]) == "" || anyToString(first["case_set_digest"]) != anyToString(second["case_set_digest"]) || anyToString(anyMap(first["snapshot"])["digest"]) != anyToString(anyMap(second["snapshot"])["digest"]) {
		t.Fatalf("snapshot/case digest is not deterministic: first=%#v second=%#v", first["case_set_digest"], second["case_set_digest"])
	}
	if len(anyToMapSlice(first["development_cases"])) != savedRecallGraphCorpusDevelopmentCases || len(anyToMapSlice(first["holdout_cases"])) != savedRecallGraphCorpusHoldoutCases {
		t.Fatalf("unexpected split sizes: development=%d holdout=%d", len(anyToMapSlice(first["development_cases"])), len(anyToMapSlice(first["holdout_cases"])))
	}
	health := savedRecallGraphCorpusHealth("", first)
	if !anyToBool(health["valid"]) || !anyToBool(health["benchmark_eligible"]) {
		t.Fatalf("fixture corpus failed closed validation: %#v cost=%#v", health, first["cost"])
	}
	holdout := anyToMapSlice(first["holdout_cases"])
	counts := map[string]int{}
	negatives := 0
	for _, row := range holdout {
		relation := anyToString(row["relation"])
		if anyToBool(row["hard_negative"]) {
			negatives++
			relation = "hard_negative"
		}
		counts[relation]++
		if anyToBool(row["hard_negative"]) && len(normalizeExpectedFileTokens(row["graph_expected_files"])) != 0 {
			t.Fatalf("hard negative exposes a positive graph target: %#v", row)
		}
	}
	if negatives != 10 || counts["references"] != 30 || counts["same_session"] != 30 || counts["same_topic"] != 30 {
		t.Fatalf("holdout topology quotas are wrong: counts=%#v", counts)
	}
}

func TestSavedRecallGraphCorpusHealthRejectsClosedCustodyAndEdgeIdentityTampering(t *testing.T) {
	candidates := graphCorpusFixtureCandidates(400)
	edges := graphCorpusFixtureEdges(candidates)
	artifact := buildSavedRecallGraphCorpusFromCandidates(candidates, edges, "fixture-seed", "current_state_bottom_k", graphCorpusFixtureSourceStats(t, candidates, edges))

	custodyTampered := cloneJSONMap(artifact)
	anyMap(custodyTampered["custody"])["mode"] = "caller_resealed"
	custodyHealth := savedRecallGraphCorpusHealth("", custodyTampered)
	if anyToBool(custodyHealth["valid"]) || !graphCorpusContainsIssueCode(custodyHealth, "custody_schema_mismatch") {
		t.Fatalf("tampered custody mode escaped the closed schema: %#v", custodyHealth)
	}

	edgeTampered := cloneJSONMap(artifact)
	cases := anyToMapSlice(edgeTampered["cases"])
	changed := false
	for _, row := range cases {
		if anyToBool(row["hard_negative"]) {
			continue
		}
		row["edge_id"] = deterministicMemoryEdgeID(anyToString(row["target_memory_id"]), anyToString(row["relation"]), anyToString(row["seed_memory_id"]))
		changed = true
		break
	}
	if !changed {
		t.Fatal("fixture has no positive edge to tamper")
	}
	edgeTampered["case_set_digest"] = "sha256:" + graphCorpusCaseSetDigest(cases)
	custody := anyMap(edgeTampered["custody"])
	custody["case_set_digest"] = edgeTampered["case_set_digest"]
	custody["case_capture_digest"] = "sha256:" + graphCorpusCaseCustodyDigest(cases)
	edgeHealth := savedRecallGraphCorpusHealth("", edgeTampered)
	if anyToBool(edgeHealth["valid"]) || !graphCorpusContainsIssueCode(edgeHealth, "edge_identity_binding_mismatch") {
		t.Fatalf("self-consistent forged edge identity escaped validation: %#v", edgeHealth)
	}
}

func TestSavedRecallGraphCorpusValidatorRejectsLeakageAndNegativeOracleTampering(t *testing.T) {
	candidates := graphCorpusFixtureCandidates(400)
	artifact := buildSavedRecallGraphCorpusFromCandidates(candidates, graphCorpusFixtureEdges(candidates), "fixture-seed", "current_state_bottom_k", graphCorpusFixtureSourceStats(t, candidates, graphCorpusFixtureEdges(candidates)))
	cases := anyToMapSlice(artifact["cases"])
	cases[0]["seed_target_lineage"] = cases[1]["seed_target_lineage"]
	cases[0]["provenance"] = map[string]any{"owner": savedRecallGraphCorpusOwner, "source_index": "raw_store", "project_scope_verified": false, "raw_store_scanned": true}
	artifact["cases"] = cases
	cfg := savedRecallGraphCorpusConfigFromArtifact("", artifact)
	health := validateSavedRecallGraphCorpusConfig(cfg)
	issues := anyToMapSlice(health["issues"])
	found := map[string]bool{}
	for _, issue := range issues {
		found[anyToString(issue["code"])] = true
	}
	if !found["case_set_digest_mismatch"] || !found["authoritative_provenance_mismatch"] {
		t.Fatalf("validator did not reject tampered provenance/digest: %#v", health)
	}
	for _, row := range cases {
		if anyToBool(row["hard_negative"]) {
			row["forbidden_memory_ids"] = []string{"project-0::notes/not-the-forbidden-target.md"}
			break
		}
	}
	negativeHealth := validateSavedRecallGraphCorpusConfig(savedRecallGraphCorpusConfigFromArtifact("", artifact))
	negativeFound := false
	for _, issue := range anyToMapSlice(negativeHealth["issues"]) {
		if anyToString(issue["code"]) == "invalid_negative_oracle" {
			negativeFound = true
		}
	}
	if !negativeFound {
		t.Fatalf("validator accepted mismatched forbidden negative target: %#v", negativeHealth)
	}
}

func TestGraphRecallPromotionGateBlocksMissingEvidence(t *testing.T) {
	gate := graphRecallPromotionGate(true,
		map[string]any{"passed": true, "recallAtK": 1.0, "mrr": 1.0, "numericExactness": 1.0},
		map[string]any{"available": true, "mean": 95.0, "p10": 91.0},
		map[string]any{"positive_cases": 90, "graph_hits": 90, "graph_recall_at_5": 1.0, "incremental_denominator": 30, "incremental_help": 1.0, "hard_negative_cases": 10, "hard_negative_passed": 10, "explicit_cases": 90, "latency_comparable": true, "paired_latency_cases": 90, "paired_latency_expected": 90, "paired_latency_failures": 0, "paired_latency": map[string]any{"scope": graphRecallPairedLatencyScope, "paired_cases": 90, "expected_cases": 90, "failed_cases": 0}},
		map[string]any{"valid": false, "benchmark_eligible": false},
	)
	if anyToBool(gate["promotion_eligible"]) {
		t.Fatalf("invalid corpus unexpectedly passed graph promotion gate: %#v", gate)
	}
	blocked := graphCorpusSortedStrings(anyToStringSlice(gate["blocked_reasons"]))
	expected := map[string]bool{"context_quality_calibration_unavailable": true, "cost_observability_unknown": true, "cost_observability_incomplete": true, "cost_source_policy_unbound": true, "external_network_nonzero_or_unproven": true, "invalid_evaluation_traffic_class": true, "direct_baseline_unavailable": true, "direct_metrics_regressed": true, "graph_corpus_not_benchmark_eligible": true, "graph_corpus_validation_receipt_unbound": true, "graph_evaluation_denominator_unbound": true, "graph_latency_incomparable": true, "graph_attribution_binding_incomplete": true}
	for _, reason := range blocked {
		delete(expected, reason)
	}
	if len(expected) != 0 {
		t.Fatalf("unexpected graph promotion blockers: %#v", gate)
	}
}

func TestGraphRecallPairedLatencySummaryIsSeparateAndFailClosed(t *testing.T) {
	totals := graphRecallEvaluationTotals{
		positiveExpected:       2,
		treatmentLatencyValues: []float64{4, 6},
		controlLatencyValues:   []float64{5, 5},
		latencyDeltaValues:     []float64{-1, 1},
		pairedLatencySamples: []graphRecallPairedLatencySample{
			{CaseID: "improvement", GraphTreatmentMS: 4, DirectControlMS: 5},
			{CaseID: "regression", GraphTreatmentMS: 6, DirectControlMS: 5},
		},
		pairedLatencyCases: 2,
	}
	summary := graphRecallPairedLatencySummary(totals)
	if !anyToBool(summary["comparable"]) || !anyToBool(summary["claims_allowed"]) || anyToInt(summary["paired_cases"], 0) != 2 {
		t.Fatalf("complete paired latency evidence did not pass: %#v", summary)
	}
	if anyToFloat(summary["graph_treatment_avg_ms"]) != 5 || anyToFloat(summary["direct_control_avg_ms"]) != 5 || anyToFloat(summary["latency_delta_avg_ms"]) != 0 || anyToInt(summary["improvement_count"], 0) != 1 || anyToInt(summary["regression_count"], 0) != 1 {
		t.Fatalf("separate paired latency statistics are not deterministic: %#v", summary)
	}
	samples := anyToMapSlice(summary["samples"])
	if len(samples) != 2 || anyToFloat(samples[0]["delta_ms"]) != -1 || anyToString(samples[0]["classification"]) != "improvement" || anyToFloat(samples[1]["delta_ms"]) != 1 || anyToString(samples[1]["classification"]) != "regression" {
		t.Fatalf("signed exact latency pairs were not retained: %#v", samples)
	}
	if valid, withinBudget := graphRecallPairedLatencyEvidenceValid(summary, 2); !valid || !withinBudget {
		t.Fatalf("exact signed latency evidence did not validate: valid=%t within_budget=%t summary=%#v", valid, withinBudget, summary)
	}
	tampered := cloneJSONMap(summary)
	tampered["latency_delta_avg_ms"] = 99.0
	if valid, _ := graphRecallPairedLatencyEvidenceValid(tampered, 2); valid {
		t.Fatalf("aggregate latency evidence detached from exact samples was accepted: %#v", tampered)
	}
	overBudget := graphRecallTestPairedLatencyEvidence(2, graphRecallPairedLatencyRegressionBudgetMS+0.001)
	if valid, withinBudget := graphRecallPairedLatencyEvidenceValid(overBudget, 2); !valid || withinBudget || anyToBool(overBudget["claims_allowed"]) {
		t.Fatalf("arbitrary slowdown did not exceed the canonical budget: valid=%t within_budget=%t summary=%#v", valid, withinBudget, overBudget)
	}
	totals.pairedLatencyCases = 1
	if anyToBool(graphRecallPairedLatencySummary(totals)["comparable"]) {
		t.Fatal("incomplete paired latency evidence was allowed to claim comparability")
	}
	if _, ok := graphRecallControlLatencyValid(map[string]any{"control_latency_ms": -1.0, "control_latency_scope": graphRecallPairedLatencyScope, "control_latency_comparable": true}); ok {
		t.Fatal("negative control latency passed closed validation")
	}
}

func TestGraphRecallPromotionGateRequiresAllDirectMetricsAndPassesBoundEvidence(t *testing.T) {
	originalCommit, originalTree := contextLatticeSourceCommit, contextLatticeSourceTree
	contextLatticeSourceCommit, contextLatticeSourceTree = strings.Repeat("c", 40), strings.Repeat("d", 40)
	t.Cleanup(func() { contextLatticeSourceCommit, contextLatticeSourceTree = originalCommit, originalTree })
	binding := map[string]any{"schema_id": savedRecallEvalV3SchemaID, "version": savedRecallEvalV3Version, "k": defaultRecallEvalK, "case_set_digest": "sha256:direct", "snapshot_digest": "sha256:snapshot", "evaluation_split": "holdout", "evaluation_case_set_digest": "sha256:direct-holdout", "evaluation_case_count": 90, "evaluation_traffic_class": "evaluation_holdout"}
	baseline := map[string]any{"binding": binding, "recallAtK": 0.9, "mrr": 0.8, "numericExactness": 0.95, "citationCoverage": 0.9, "sourceDiversity": 1.0}
	baseDirect := map[string]any{"passed": true, "recallAtK": 0.91, "mrr": 0.81, "numericExactness": 0.96, "citationCoverage": 0.91, "sourceDiversity": 1.1, "baseline": baseline, "baseline_receipt": map[string]any{"available": true, "digest": "sha256:baseline"}}
	quality := map[string]any{"available": true, "same_snapshot": true, "cohort_complete": true, "formula": "unchanged_contextPackQualityScore_0_to_100", "mean": 95.0, "p10": 91.0, "expected_case_count": 90, "sample_count": 90}
	graph := map[string]any{"cases_expected": 100, "cases_attempted": 100, "cases_evaluated": 100, "cases_terminal": 100, "case_failures": 0, "positive_expected": 90, "positive_failures": 0, "positive_cases": 90, "graph_hits": 90, "graph_attribution_binding": "finalized_visible_graph_provenance.v1", "graph_attributed_hits": 90, "graph_attributed_denominator": 90, "graph_recall_at_5": 1.0, "incremental_denominator": 90, "incremental_help": 1.0, "hard_negative_expected": 10, "hard_negative_failures": 0, "hard_negative_oracle_available": 10, "hard_negative_cases": 10, "hard_negative_passed": 10, "explicit_cases": 90, "latency_comparable": true, "paired_latency_cases": 90, "paired_latency_expected": 90, "paired_latency_failures": 0}
	graph["paired_latency"] = graphRecallTestPairedLatencyEvidence(90, -1)
	graph["hard_negative_current_identity"] = graphRecallTestHardNegativeIdentityReceipt(10, "sha256:source-edge")
	corpus := map[string]any{"valid": true, "benchmark_eligible": true, "case_count": 300, "case_set_digest": "sha256:graph-cases", "manifest_digest": "sha256:manifest", "custody": map[string]any{"case_set_digest": "sha256:graph-cases"}, "direct_baseline_binding": binding, "cost": map[string]any{
		"schema_id": retrievalCostObservabilitySchemaID, "authority": retrievalCostObservabilityAuthority, "transport_observed": true, "proven_zero": true,
		"network_calls": 3, "network_calls_observed": true, "local_backend_calls": 3, "local_backend_calls_observed": true, "external_network_calls": 0, "external_network_calls_observed": true, "external_network_zero_proven": true,
		"provider_calls": 0, "provider_calls_observed": true, "provider_tokens": 0, "provider_tokens_observed": true, "provider_cost_microusd": 0, "provider_cost_observed": true,
		"observation_expected": 1, "observation_expected_required": 1, "observation_observed": 1, "observation_missing": 0, "traffic_class": "evaluation_holdout", "source_policy_observed": true, "source_policy_consistent": true,
	}, "evaluation_split": "holdout", "evaluation_case_count": 100, "evaluation_positive_cases": 90, "evaluation_hard_negative_cases": 10, "evaluation_incremental_cases": 90, "source_edge_snapshot_digest": "sha256:source-edge"}
	policyRun := map[string]any{"schema_id": retrievalEvaluationSourcePolicySchemaID, "version": retrievalEvaluationSourcePolicyVersion, "receipt_kind": "run", "authority": retrievalCostObservabilityAuthority, "server_owned": true, "evaluation_holdout": true, "allowed_transport": "in_process", "provider_policy": "provider_incapable_in_process_only", "redirect_escape_disabled": true, "eligible": true, "expected_case_count": 1, "observed_case_count": 1, "policy_digests": []string{"sha256:policy"}, "source_runtime_identity": contextLatticeBuildIdentity()}
	policyRun["digest"] = "sha256:" + graphCorpusDigestMap(policyRun)
	anyMap(corpus["cost"])["source_policy_run"] = policyRun
	validation := map[string]any{"schema_id": "saved_recall_graph_validation.v1", "version": 1, "authority": savedRecallGraphCorpusOwner, "server_owned": true, "valid": true, "benchmark_eligible": true, "case_count": 300, "case_set_digest": "sha256:graph-cases", "manifest_digest": "sha256:manifest", "custody_case_set_digest": "sha256:graph-cases", "captured_at": nowUTCISO()}
	validation["digest"] = "sha256:" + graphCorpusDigestMap(validation)
	corpus["validation_receipt"] = validation
	if gate := graphRecallPromotionGate(true, baseDirect, quality, graph, corpus); !anyToBool(gate["promotion_eligible"]) {
		t.Fatalf("bound graph evidence unexpectedly blocked: %#v", gate)
	}
	regressedGraph := cloneJSONMap(graph)
	regressedGraph["paired_latency"] = graphRecallTestPairedLatencyEvidence(90, graphRecallPairedLatencyRegressionBudgetMS+0.001)
	gate := graphRecallPromotionGate(true, baseDirect, quality, regressedGraph, corpus)
	if anyToBool(gate["promotion_eligible"]) || !graphCorpusContainsString(anyToStringSlice(gate["blocked_reasons"]), "graph_latency_regression_budget_exceeded") {
		t.Fatalf("arbitrary graph latency slowdown passed promotion: %#v", gate)
	}
	delete(baseDirect, "citationCoverage")
	gate = graphRecallPromotionGate(true, baseDirect, quality, graph, corpus)
	if anyToBool(gate["promotion_eligible"]) || !graphCorpusContainsString(anyToStringSlice(gate["blocked_reasons"]), "direct_baseline_metric_missing") {
		t.Fatalf("missing citation metric did not block promotion: %#v", gate)
	}
	baseDirect["citationCoverage"] = 0.91
	baseDirect["binding_valid"] = false
	gate = graphRecallPromotionGate(true, baseDirect, quality, graph, corpus)
	if anyToBool(gate["promotion_eligible"]) || !graphCorpusContainsString(anyToStringSlice(gate["blocked_reasons"]), "direct_cohort_binding_mismatch") {
		t.Fatalf("direct cohort binding mismatch did not block promotion: %#v", gate)
	}
	mismatchCorpus := cloneJSONMap(corpus)
	mismatchBinding := cloneJSONMap(binding)
	mismatchBinding["evaluation_case_set_digest"] = "sha256:other-cohort"
	mismatchCorpus["direct_baseline_binding"] = mismatchBinding
	gate = graphRecallPromotionGate(true, baseDirect, quality, graph, mismatchCorpus)
	if anyToBool(gate["promotion_eligible"]) || !graphCorpusContainsString(anyToStringSlice(gate["blocked_reasons"]), "direct_baseline_binding_mismatch") {
		t.Fatalf("same-population direct baseline mismatch did not block promotion: %#v", gate)
	}
	nonzeroCorpus := cloneJSONMap(corpus)
	anyMap(nonzeroCorpus["cost"])["provider_calls"] = 1
	gate = graphRecallPromotionGate(true, baseDirect, quality, graph, nonzeroCorpus)
	if anyToBool(gate["promotion_eligible"]) || !graphCorpusContainsString(anyToStringSlice(gate["blocked_reasons"]), "provider_calls_nonzero") {
		t.Fatalf("nonzero provider calls passed promotion: %#v", gate)
	}
	missingCost := cloneJSONMap(corpus)
	delete(anyMap(missingCost["cost"]), "provider_tokens_observed")
	gate = graphRecallPromotionGate(true, baseDirect, quality, graph, missingCost)
	if anyToBool(gate["promotion_eligible"]) || !graphCorpusContainsString(anyToStringSlice(gate["blocked_reasons"]), "cost_observability_unknown") {
		t.Fatalf("one missing per-case cost field passed promotion: %#v", gate)
	}
	externalCost := cloneJSONMap(corpus)
	anyMap(externalCost["cost"])["external_network_calls"] = 1
	gate = graphRecallPromotionGate(true, baseDirect, quality, graph, externalCost)
	if anyToBool(gate["promotion_eligible"]) || !graphCorpusContainsString(anyToStringSlice(gate["blocked_reasons"]), "external_network_nonzero_or_unproven") {
		t.Fatalf("nonzero external network call passed promotion: %#v", gate)
	}
	syntheticCost := cloneJSONMap(corpus)
	anyMap(syntheticCost["cost"])["traffic_class"] = "synthetic"
	gate = graphRecallPromotionGate(true, baseDirect, quality, graph, syntheticCost)
	if anyToBool(gate["promotion_eligible"]) || !graphCorpusContainsString(anyToStringSlice(gate["blocked_reasons"]), "invalid_evaluation_traffic_class") {
		t.Fatalf("synthetic traffic class passed promotion: %#v", gate)
	}
	runtimeCorpus := cloneJSONMap(corpus)
	runtimeCorpus["runtime_identity_required"] = true
	runtimeCorpus["runtime_identity"] = contextLatticeBuildIdentity()
	runtimeCorpus["case_set_digest"] = "sha256:graph"
	runtimeCorpus["graph_snapshot_digest"] = "sha256:snapshot"
	runtimeCorpus["edge_snapshot_digest"] = "sha256:edge"
	runtimeCorpus["source_edge_snapshot_digest"] = "sha256:source-edge"
	runtimeCorpus["evaluation_snapshot_start_digest"] = "sha256:source-edge"
	runtimeCorpus["evaluation_snapshot_end_digest"] = "sha256:source-edge"
	runtimeCorpus["evaluation_snapshot_stable"] = true
	runtimeCorpus["baseline_policy_digest"] = "sha256:policy"
	runtimeCorpus["evaluation_captured_at"] = nowUTCISO()
	runtimeCorpus["evaluation_traffic_class"] = "evaluation_holdout"
	runtimeValidation := cloneJSONMap(anyMap(runtimeCorpus["validation_receipt"]))
	runtimeValidation["case_set_digest"] = runtimeCorpus["case_set_digest"]
	runtimeValidation["manifest_digest"] = runtimeCorpus["manifest_digest"]
	runtimeValidation["custody_case_set_digest"] = anyMap(runtimeCorpus["custody"])["case_set_digest"]
	runtimeValidation["captured_at"] = nowUTCISO()
	runtimeValidation["digest"] = "sha256:" + graphCorpusDigestMap(runtimeValidation, "digest")
	runtimeCorpus["validation_receipt"] = runtimeValidation
	runtimeDirect := cloneJSONMap(baseDirect)
	delete(runtimeDirect, "binding_valid")
	if gate = graphRecallPromotionGate(true, runtimeDirect, quality, graph, runtimeCorpus); !anyToBool(gate["promotion_eligible"]) {
		t.Fatalf("complete runtime-bound evidence unexpectedly blocked: %#v", gate)
	}
	vanished := cloneJSONMap(graph)
	vanished["cases_attempted"] = 100
	vanished["cases_evaluated"] = 99
	vanished["cases_terminal"] = 100
	vanished["case_failures"] = 1
	vanished["positive_cases"] = 89
	vanished["positive_failures"] = 1
	vanished["graph_hits"] = 89
	vanished["graph_recall_at_5"] = 1.0
	vanished["explicit_cases"] = 89
	gate = graphRecallPromotionGate(true, runtimeDirect, quality, vanished, runtimeCorpus)
	if anyToBool(gate["promotion_eligible"]) || !graphCorpusContainsString(anyToStringSlice(gate["blocked_reasons"]), "graph_case_denominator_incomplete") {
		t.Fatalf("vanished positive case passed sealed denominator gate: %#v", gate)
	}
	incrementalFailure := cloneJSONMap(graph)
	incrementalFailure["incremental_denominator"] = 89
	incrementalFailure["incremental_help"] = 1.0
	gate = graphRecallPromotionGate(true, runtimeDirect, quality, incrementalFailure, runtimeCorpus)
	if anyToBool(gate["promotion_eligible"]) || !graphCorpusContainsString(anyToStringSlice(gate["blocked_reasons"]), "incremental_denominator_binding_mismatch") {
		t.Fatalf("failed incremental treatment was allowed to shrink the sealed denominator: %#v", gate)
	}
	driftCorpus := cloneJSONMap(runtimeCorpus)
	anyMap(driftCorpus["runtime_identity"])["source_commit"] = strings.Repeat("e", 40)
	gate = graphRecallPromotionGate(true, runtimeDirect, quality, graph, driftCorpus)
	if anyToBool(gate["promotion_eligible"]) || !graphCorpusContainsString(anyToStringSlice(gate["blocked_reasons"]), "graph_runtime_identity_mismatch") {
		t.Fatalf("source build drift passed graph promotion: %#v", gate)
	}
}

func TestSavedRecallGraphCorpusRejectsIncompleteAdjacencySnapshot(t *testing.T) {
	candidates := graphCorpusFixtureCandidates(400)
	edges := graphCorpusFixtureEdges(candidates)
	completeSeedIDs := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		_, _, memoryID, _, _ := canonicalMemoryID(candidate.doc.Project + "::" + candidate.doc.FileName)
		completeSeedIDs = append(completeSeedIDs, memoryID)
	}
	artifact := buildSavedRecallGraphCorpusFromCandidates(candidates, edges, "fixture-seed", "current_state_bottom_k", map[string]any{
		"bounded": true, "index_integrity": true, "index_mode": "current_state_bottom_k", "raw_store_scanned": false, "complete_seed_ids": completeSeedIDs,
		"edge_snapshot": map[string]any{
			"schema_id": "saved_recall_graph_edge_snapshot.v1", "complete": false, "truncated": true,
			"continuation_complete": false, "edge_digest": "sha256:" + graphCorpusEdgesDigest(edges),
		},
	})
	if len(anyToMapSlice(artifact["cases"])) != 0 || len(anyMap(artifact["insufficiency_receipt"])) == 0 || anyToBool(artifact["benchmark_eligible"]) {
		t.Fatalf("incomplete adjacency snapshot was not an explicit non-eligible receipt: %#v", artifact)
	}
	if !strings.Contains(anyToString(anyMap(artifact["insufficiency_receipt"])["detail"]), "incomplete") {
		t.Fatalf("insufficiency receipt did not identify incomplete adjacency: %#v", artifact["insufficiency_receipt"])
	}
}

func TestSavedRecallGraphCorpusPreservesValidCanonicalOnInsufficientRefresh(t *testing.T) {
	root, err := os.MkdirTemp(os.Getenv("TMPDIR"), "graph-corpus-preserve-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	canonical := filepath.Join(root, "graph-corpus.json")
	t.Setenv("ORCH_RECALL_GRAPH_CORPUS_PATH", canonical)
	candidates := graphCorpusFixtureCandidates(400)
	edges := graphCorpusFixtureEdges(candidates)
	valid := buildSavedRecallGraphCorpusFromCandidates(candidates, edges, "fixture-seed", "current_state_bottom_k", graphCorpusFixtureSourceStats(t, candidates, edges))
	health, persisted, err := saveSavedRecallGraphCorpusArtifactIfHealthy(canonical, valid)
	if err != nil || !persisted || !anyToBool(health["valid"]) {
		t.Fatalf("valid fixture was not persisted: health=%#v persisted=%t err=%v", health, persisted, err)
	}
	before, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatal(err)
	}
	insufficient := graphCorpusInsufficientArtifact("fixture", map[string]int{"projects": 1}, graphCorpusRequiredDimensions(), map[string]int{}, "source population cannot satisfy closed graph quotas")
	failedHealth, replaced, err := saveSavedRecallGraphCorpusArtifactIfHealthy(canonical, insufficient)
	if err != nil || replaced || anyToBool(failedHealth["valid"]) {
		t.Fatalf("insufficient refresh was not rejected before canonical replacement: health=%#v replaced=%t err=%v", failedHealth, replaced, err)
	}
	if err := saveSavedRecallGraphCorpusAttemptReceipt(canonical, insufficient, failedHealth); err != nil {
		t.Fatalf("write bounded insufficiency receipt: %v", err)
	}
	after, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("insufficient refresh changed the last valid canonical graph corpus")
	}
	if _, err := os.Stat(canonical + ".attempt.json"); err != nil {
		t.Fatalf("bounded insufficiency receipt was not written: %v", err)
	}
}

func TestSavedRecallGraphCorpusIncrementalControlLabelsAreSealedBeforeTreatment(t *testing.T) {
	candidates := graphCorpusFixtureCandidates(400)
	edges := graphCorpusFixtureEdges(candidates)
	stats := graphCorpusFixtureSourceStats(t, candidates, edges)
	allDirect := cloneJSONMap(stats)
	allDirect["incremental_controls"] = cloneJSONMap(anyMap(stats["incremental_controls"]))
	for _, raw := range anyMap(allDirect["incremental_controls"]) {
		anyMap(raw)["target_direct_hit"] = true
	}
	artifact := buildSavedRecallGraphCorpusFromCandidates(candidates, edges, "fixture-seed", "current_state_bottom_k", allDirect)
	health := validateSavedRecallGraphCorpusConfig(savedRecallGraphCorpusConfigFromArtifact("", artifact))
	if anyToBool(health["valid"]) || !graphCorpusContainsIssueCode(health, "holdout_incremental_denominator_below_30") {
		t.Fatalf("direct-hit control cohort was not excluded from the fixed incremental denominator: %#v", health)
	}
	completeArtifact := buildSavedRecallGraphCorpusFromCandidates(candidates, edges, "fixture-seed", "current_state_bottom_k", stats)
	selectedLineage := ""
	for _, row := range anyToMapSlice(completeArtifact["development_cases"]) {
		if !anyToBool(row["hard_negative"]) {
			selectedLineage = anyToString(row["seed_target_lineage"])
			break
		}
	}
	missing := cloneJSONMap(stats)
	controls := cloneJSONMap(anyMap(missing["incremental_controls"]))
	if selectedLineage == "" {
		t.Fatal("fixture did not produce a selected positive lineage")
	}
	delete(controls, selectedLineage)
	missing["incremental_controls"] = controls
	insufficient := buildSavedRecallGraphCorpusFromCandidates(candidates, edges, "fixture-seed", "current_state_bottom_k", missing)
	if anyToBool(insufficient["benchmark_eligible"]) || !strings.Contains(anyToString(anyMap(insufficient["insufficiency_receipt"])["detail"]), "control") {
		t.Fatalf("missing pre-treatment control was allowed to shrink or pass the cohort: %#v", insufficient)
	}
}

func graphCorpusContainsIssueCode(health map[string]any, code string) bool {
	for _, issue := range anyToMapSlice(health["issues"]) {
		if anyToString(issue["code"]) == code {
			return true
		}
	}
	return false
}

func TestListMemoryEdgesCompleteRejectsInconsistentAdjacencyIndex(t *testing.T) {
	edge := memoryEdgeEntry{
		EdgeID:     "edge-complete-index",
		SourceID:   "project-a::notes/source.md",
		TargetID:   "project-a::notes/target.md",
		Relation:   "references",
		Project:    "project-a",
		Confidence: 0.99,
		CreatedAt:  "2026-01-01T00:00:00Z",
		Source:     memoryEdgeSource,
	}
	store := &memoryStore{
		policy:      memoryStorePolicy{enabled: true},
		edges:       map[string]memoryEdgeEntry{edge.EdgeID: edge},
		edgeOrder:   []string{},
		edgeOrdinal: map[string]int64{edge.EdgeID: 1},
	}
	store.ready.Store(true)

	edges, complete, err := store.listMemoryEdgesComplete(context.Background(), memoryEdgeQuery{Project: "project-a"}, 10)
	if err != nil {
		t.Fatalf("list complete graph edges: %v", err)
	}
	if complete || len(edges) != 0 {
		t.Fatalf("inconsistent edge order was reported complete: complete=%v edges=%#v", complete, edges)
	}

	store.edgeOrder = []string{edge.EdgeID, edge.EdgeID}
	edges, complete, err = store.listMemoryEdgesComplete(context.Background(), memoryEdgeQuery{Project: "project-a"}, 10)
	if err != nil {
		t.Fatalf("list duplicate graph edges: %v", err)
	}
	if complete || len(edges) != 0 {
		t.Fatalf("duplicate edge order was reported complete: complete=%v edges=%#v", complete, edges)
	}
}

func TestListMemoryEdgesCompleteHoldsFenceThroughProjectionSnapshot(t *testing.T) {
	server, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()

	readerAfterSnapshot := make(chan struct{})
	resumeReader := make(chan struct{})
	server.memoryStore.memoryEdgesCompleteAfterSnapshot = func() {
		close(readerAfterSnapshot)
		<-resumeReader
	}
	type completeResult struct {
		edges    []memoryEdgeEntry
		complete bool
		err      error
	}
	result := make(chan completeResult, 1)
	go func() {
		edges, complete, err := server.memoryStore.listMemoryEdgesComplete(
			context.Background(),
			memoryEdgeQuery{Project: "alpha"},
			10,
		)
		result <- completeResult{edges: edges, complete: complete, err: err}
	}()
	<-readerAfterSnapshot

	writerCtx, cancelWriter := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancelWriter()
	_, err := server.memoryStore.upsertMemoryEdge(writerCtx, memoryEdgeEntry{
		SourceID:   "alpha::notes/writer-during-snapshot-source.md",
		TargetID:   "alpha::notes/writer-during-snapshot-target.md",
		Relation:   "depends_on",
		Project:    "alpha",
		Confidence: 1,
		CreatedAt:  nowUTCISO(),
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		close(resumeReader)
		t.Fatalf("writer crossed complete-reader fence after projection snapshot: %v", err)
	}
	close(resumeReader)
	snapshot := <-result
	server.memoryStore.memoryEdgesCompleteAfterSnapshot = nil
	if snapshot.err != nil {
		t.Fatalf("list complete graph edges while holding fence: %v", snapshot.err)
	}
	if !snapshot.complete {
		t.Fatalf("complete graph reader failed closed while writer was fenced: %#v", snapshot.edges)
	}
	if len(snapshot.edges) != 0 {
		t.Fatalf("complete graph reader observed a writer that never crossed its fence: %#v", snapshot.edges)
	}
}

func TestGraphCorpusCompleteSnapshotSuppressesStaleStructuredBinding(t *testing.T) {
	server, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	if _, _, err := server.memoryStore.put(normalizedWrite{
		project: "alpha", fileName: "notes/corpus-binding-target.md", content: "target v1", topicPath: "runbooks/corpus-binding",
	}); err != nil {
		t.Fatalf("seed graph corpus binding target: %v", err)
	}
	if _, _, err := server.memoryStore.put(normalizedWrite{
		project: "alpha", fileName: "notes/corpus-binding-source.md", content: "source", topicPath: "runbooks/corpus-binding",
		references: []memoryStructuredReference{{TargetID: "alpha::notes/corpus-binding-target.md", Relation: "references", Confidence: 1}},
	}); err != nil {
		t.Fatalf("seed graph corpus structured binding: %v", err)
	}
	if _, _, err := server.memoryStore.put(normalizedWrite{
		project: "alpha", fileName: "notes/corpus-binding-target.md", content: "target v2", topicPath: "runbooks/corpus-binding",
	}); err != nil {
		t.Fatalf("advance graph corpus binding target: %v", err)
	}
	ordinary, err := server.memoryStore.listMemoryEdges(context.Background(), memoryEdgeQuery{Project: "alpha", MemoryID: "alpha::notes/corpus-binding-source.md", Relation: "references", Limit: 10})
	if err != nil || len(ordinary) != 0 {
		t.Fatalf("ordinary graph list retained stale structured binding: edges=%#v err=%v", ordinary, err)
	}
	completeEdges, complete, err := server.memoryStore.listMemoryEdgesComplete(context.Background(), memoryEdgeQuery{Project: "alpha"}, 10)
	if err != nil || !complete || len(completeEdges) != 0 {
		t.Fatalf("complete graph snapshot retained stale structured binding: complete=%v edges=%#v err=%v", complete, completeEdges, err)
	}
	candidates, _, stats := server.recallEvalIndexedCandidates(context.Background(), "alpha", "runbooks/corpus-binding", savedRecallGraphCorpusMaxSourceDocs)
	if !anyToBool(stats["bounded"]) || !anyToBool(stats["index_integrity"]) {
		t.Fatalf("graph corpus candidate fixture is not authoritative: %#v", stats)
	}
	expected := graphCorpusSourceEdgeSnapshotDigest(candidates, nil)
	actual, err := server.currentSavedRecallGraphSourceEdgeDigest(context.Background(), map[string]any{
		"capture_project": "alpha", "capture_topic_prefix": "runbooks/corpus-binding",
	})
	if err != nil || actual != expected {
		t.Fatalf("saved graph corpus snapshot was contaminated by stale binding: got=%q want=%q err=%v", actual, expected, err)
	}
}

func TestGraphCorpusEdgeSnapshotDigestBindsEligibilityState(t *testing.T) {
	edge := memoryEdgeEntry{
		EdgeID:     "edge-custody",
		SourceID:   "project-a::notes/source.md",
		TargetID:   "project-a::notes/target.md",
		Relation:   "references",
		Project:    "project-a",
		TopicPath:  "graph/topic",
		Confidence: 0.9500004,
		Provenance: map[string]any{"active": true, "valid": true},
		Metadata:   map[string]any{"stale": false},
		AgentID:    "agent-a",
		SessionID:  "session-a",
		Lifecycle:  "durable",
		CreatedAt:  "2026-01-01T00:00:00Z",
		Source:     memoryEdgeSource,
	}
	baseline := graphCorpusEdgesDigest([]memoryEdgeEntry{edge})

	for name, mutate := range map[string]func(*memoryEdgeEntry){
		"confidence across eligibility boundary": func(candidate *memoryEdgeEntry) { candidate.Confidence = 0.9499996 },
		"retired lifecycle":                      func(candidate *memoryEdgeEntry) { candidate.Lifecycle = "retired" },
		"stale metadata":                         func(candidate *memoryEdgeEntry) { candidate.Metadata = map[string]any{"stale": true} },
		"inactive provenance": func(candidate *memoryEdgeEntry) {
			candidate.Provenance = map[string]any{"active": false, "valid": true}
		},
		"session binding": func(candidate *memoryEdgeEntry) { candidate.SessionID = "session-b" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := edge
			candidate.Provenance = cloneJSONMap(edge.Provenance)
			candidate.Metadata = cloneJSONMap(edge.Metadata)
			mutate(&candidate)
			if got := graphCorpusEdgesDigest([]memoryEdgeEntry{candidate}); got == baseline {
				t.Fatalf("eligibility-relevant edge mutation retained snapshot digest: %s", got)
			}
		})
	}
}

func TestGraphRecallHardNegativeRequiresSealedSeedInDirectCohort(t *testing.T) {
	seedID := "project-a::notes/seed.md"
	targetID := "project-a::notes/forbidden.md"
	otherSeedID := "project-a::notes/other.md"
	edge := memoryEdgeEntry{EdgeID: "edge-sealed", SourceID: seedID, TargetID: targetID, Relation: "references", Project: "project-a", Confidence: 0.99, CreatedAt: "2026-01-01T00:00:00Z", Source: memoryEdgeSource}
	store := &memoryStore{
		policy: memoryStorePolicy{enabled: true}, edges: map[string]memoryEdgeEntry{edge.EdgeID: edge}, edgeOrder: []string{edge.EdgeID}, edgeOrdinal: map[string]int64{edge.EdgeID: 1},
	}
	store.ready.Store(true)
	server := &server{memoryStore: store}
	contribution := server.evaluateRecallGraphContributionForSeed(context.Background(), []map[string]any{{"memory_id": otherSeedID}}, nil, nil, 5, "project-a", seedID, true)
	if anyToBool(contribution["enabled"]) || !strings.Contains(anyToString(contribution["reason"]), "sealed hard-negative seed") {
		t.Fatalf("unrelated clean seed incorrectly satisfied sealed negative oracle: %#v", contribution)
	}
}

func TestGraphRecallHardNegativeRejectsForbiddenFileWithoutMemoryID(t *testing.T) {
	forbiddenID := "project-a::notes/forbidden.md"
	forbiddenFile := "notes/forbidden.md"
	forbiddenFiles := map[string]struct{}{forbiddenFile: {}}
	results := []map[string]any{{"file": forbiddenFile}}
	if !graphRecallResultContainsTarget(results, forbiddenID, forbiddenFiles, defaultRecallEvalK) {
		t.Fatal("forbidden direct result without a memory_id escaped the hard-negative oracle")
	}

	finalVisible := map[string]any{
		"production_response_seam": true, "production_final_response_seam": true,
		"production_response_schema_id": recallResponseContractID, "production_response_digest": "sha256:" + strings.Repeat("d", 64), "production_response_scope_digest": "sha256:" + strings.Repeat("c", 64), "production_response_contract_valid": true,
		"side_effects_suppressed": true, "competing_interventions_disabled": true,
		"final_model_visible_k":       defaultRecallEvalK,
		"final_model_visible_ordered": true,
		"final_model_visible_evidence": []map[string]any{{
			"rank": 1, "response_rank": 1, "response_ref": graphRecallOpaqueMemoryRef("response-forbidden"), "file_ref": graphRecallOpaqueMemoryRef(forbiddenFile),
		}},
		"final_model_visible_memory_refs": []string{},
		"final_model_visible_file_refs":   []string{graphRecallOpaqueMemoryRef(forbiddenFile)},
	}
	finalVisible["final_model_visible_digest"] = graphRecallFinalModelVisibleDigest(finalVisible)
	if !graphRecallFinalModelVisibleContains(finalVisible, forbiddenID, forbiddenFiles) {
		t.Fatal("forbidden production-visible file without a memory_id escaped the hard-negative oracle")
	}
}

func TestGraphCorpusHydratesEdgeEndpointOutsideOrdinarySample(t *testing.T) {
	seed := memoryStoreEntry{EventID: "seed", Project: "project-a", FileName: "notes/seed.md", TopicPath: "graph/topic", Summary: "seed summary", AgentID: "agent-a", SessionID: "session-a", CreatedAt: "2026-01-01T00:00:00Z", Lifecycle: "active"}
	target := memoryStoreEntry{EventID: "target", Project: "project-a", FileName: "notes/target-outside-sample.md", TopicPath: "graph/topic", Summary: "target summary outside bottom k", AgentID: "agent-b", SessionID: "session-b", CreatedAt: "2026-01-02T00:00:00Z", Lifecycle: "active"}
	seedID := "project-a::notes/seed.md"
	targetID := "project-a::notes/target-outside-sample.md"
	store := &memoryStore{
		policy: memoryStorePolicy{enabled: true}, currentState: map[string]memoryCurrentState{
			memoryStoreKey(seed.Project, seed.FileName): memoryCurrentStateFromEntry(seed), memoryStoreKey(target.Project, target.FileName): memoryCurrentStateFromEntry(target),
		}, recent: []memoryStoreEntry{seed}, exactStatePaths: map[string]struct{}{}, latestTopic: map[string]string{},
	}
	edge := memoryEdgeEntry{EdgeID: "edge-endpoint", SourceID: seedID, TargetID: targetID, Relation: "references", Project: "project-a", TopicPath: "graph/topic", Confidence: 0.99, CreatedAt: "2026-01-02T00:00:00Z", Source: memoryEdgeSource}
	endpointCandidates := graphCorpusHydrateEdgeEndpoints(context.Background(), store, []memoryEdgeEntry{edge}, "project-a", "graph/topic")
	if len(endpointCandidates) != 2 {
		t.Fatalf("edge-first endpoint hydration did not include both exact endpoints: %#v", endpointCandidates)
	}
	foundTarget := false
	for _, candidate := range endpointCandidates {
		if strings.EqualFold(candidate.doc.Project+"::"+candidate.doc.FileName, targetID) {
			foundTarget = true
		}
	}
	if !foundTarget {
		t.Fatalf("target outside ordinary sample was not hydrated from bounded edge snapshot: %#v", endpointCandidates)
	}
}

func TestGraphRecallFinalModelVisibleTargetMustSurviveCompiledContextPack(t *testing.T) {
	targetID := "project-a::notes/graph-target.md"
	targetFile := "notes/graph-target.md"
	seedOnly := map[string]any{
		"final_model_visible_memory_refs": []string{graphRecallOpaqueMemoryRef("project-a::notes/seed.md")},
		"final_model_visible_file_refs":   []string{graphRecallOpaqueMemoryRef("notes/seed.md")},
	}
	if graphRecallFinalModelVisibleContains(seedOnly, targetID, map[string]struct{}{targetFile: {}}) {
		t.Fatal("off-path graph target was counted when the compiled context pack omitted it")
	}
	compiledTarget := map[string]any{
		"production_response_seam": true, "production_final_response_seam": true,
		"production_response_schema_id": recallResponseContractID, "production_response_digest": "sha256:" + strings.Repeat("d", 64), "production_response_scope_digest": "sha256:" + strings.Repeat("c", 64), "production_response_contract_valid": true,
		"side_effects_suppressed": true, "competing_interventions_disabled": true,
		"final_model_visible_k":       defaultRecallEvalK,
		"final_model_visible_ordered": true,
		"final_model_visible_evidence": []map[string]any{{
			"rank": 1, "response_rank": 1, "response_ref": graphRecallOpaqueMemoryRef("response-target"), "memory_ref": graphRecallOpaqueMemoryRef(targetID), "file_ref": graphRecallOpaqueMemoryRef(targetFile),
		}},
		"final_model_visible_memory_refs": []string{graphRecallOpaqueMemoryRef(targetID)},
		"final_model_visible_file_refs":   []string{graphRecallOpaqueMemoryRef(targetFile)},
	}
	compiledTarget["final_model_visible_digest"] = graphRecallFinalModelVisibleDigest(compiledTarget)
	if !graphRecallFinalModelVisibleContains(compiledTarget, targetID, map[string]struct{}{targetFile: {}}) {
		t.Fatal("target present in final compiled context pack was not recognized")
	}
	lateRanked := []any{}
	for rank := 1; rank <= 6; rank++ {
		row := map[string]any{
			"rank": rank, "project": "project-a", "file": fmt.Sprintf("notes/rank-%d.md", rank), "memory_id": fmt.Sprintf("project-a::notes/rank-%d.md", rank),
			"kind": "memory", "status": "selected", "confidence": 0.95, "source": sourceTopicRollup,
			"content_digest": "sha256:" + fmt.Sprintf("%064x", rank), "text": fmt.Sprintf("ranked evidence %d", rank),
		}
		if rank == 6 {
			row["file"] = targetFile
			row["memory_id"] = targetID
		}
		lateRanked = append(lateRanked, row)
	}
	responseInput := recallResponseTestInput(false)
	responseInput["context_pack"] = map[string]any{"ranked_evidence": lateRanked}
	finalResponse := finalizeRecallResponseTransport(composeRecallResponse(responseInput), "graph-eval-test", "recall_response", memoryRecallResponsePath)
	scopeDigest := strings.TrimSpace(anyToString(anyMap(finalResponse["request_scope"])["scope_digest"]))
	for index, raw := range lateRanked {
		row := anyMap(raw)
		if got, want := graphRecallFinalResponseEvidenceRef(scopeDigest, row, index), recallResponseCanonicalSourceRef(row, "evidence"); got == "" || got != want {
			t.Fatalf("graph scorer and response producer evidence refs differ at rank %d: got=%q want=%q", index+1, got, want)
		}
	}
	evidence, memories, files, ordered := graphRecallFinalModelVisibleRefs(finalResponse, anyMap(responseInput["context_pack"]))
	lateSample := map[string]any{
		"production_response_seam": true, "production_final_response_seam": true,
		"production_response_schema_id": recallResponseContractID, "production_response_digest": finalResponse["response_digest"], "production_response_scope_digest": anyMap(finalResponse["request_scope"])["scope_digest"], "production_response_contract_valid": true,
		"side_effects_suppressed": true, "competing_interventions_disabled": true,
		"final_model_visible_k": defaultRecallEvalK, "final_model_visible_ordered": ordered,
		"final_model_visible_evidence": evidence, "final_model_visible_memory_refs": memories, "final_model_visible_file_refs": files,
	}
	lateSample["final_model_visible_digest"] = graphRecallFinalModelVisibleDigest(lateSample)
	if !ordered || len(evidence) != defaultRecallEvalK || graphRecallFinalModelVisibleContains(lateSample, targetID, map[string]struct{}{targetFile: {}}) {
		t.Fatalf("target at rank six received Recall@5 credit: %#v", lateSample)
	}
	finalRows := contextPackAnyList(finalResponse["evidence"])
	if len(finalRows) < 6 {
		t.Fatalf("fixture did not produce six final-response evidence rows: %#v", finalResponse)
	}
	lateOnlyResponse := cloneJSONMap(finalResponse)
	lateOnlyResponse["evidence"] = []any{cloneJSONValue(finalRows[5])}
	lateOnlyAnswer := cloneJSONMap(anyMap(lateOnlyResponse["answer"]))
	lateOnlyAnswer["claim_refs"] = []any{anyToString(anyMap(finalRows[5])["ref_id"])}
	lateOnlyResponse["answer"] = lateOnlyAnswer
	lateOnlyState := cloneJSONMap(anyMap(lateOnlyResponse["state"]))
	lateOnlyState["evidence_count"] = 1
	lateOnlyResponse["state"] = lateOnlyState
	lateOnlyEvidence, _, _, lateOnlyOrdered := graphRecallFinalModelVisibleRefs(lateOnlyResponse, anyMap(responseInput["context_pack"]))
	if !lateOnlyOrdered || len(lateOnlyEvidence) != 0 {
		t.Fatalf("rank-six compiled target was promoted after earlier final-response evidence was pruned: ordered=%v evidence=%#v", lateOnlyOrdered, lateOnlyEvidence)
	}
}

func TestGraphRecallGraphHitRequiresFinalVisibleGraphProvenanceAndMaterialContribution(t *testing.T) {
	targetID := "project-a::notes/graph-target.md"
	targetRef := graphRecallOpaqueMemoryRef(targetID)
	sample := map[string]any{
		"production_response_seam": true, "production_final_response_seam": true,
		"production_response_schema_id":      recallResponseContractID,
		"production_response_digest":         "sha256:" + strings.Repeat("a", 64),
		"production_response_scope_digest":   "sha256:" + strings.Repeat("b", 64),
		"production_response_contract_valid": true, "side_effects_suppressed": true,
		"competing_interventions_disabled": true, "final_model_visible_k": defaultRecallEvalK,
		"final_model_visible_ordered": true,
		"final_model_visible_evidence": []map[string]any{{
			"rank": 1, "response_rank": 1, "response_ref": graphRecallOpaqueMemoryRef("response-target"),
			"memory_ref": targetRef, "file_ref": graphRecallOpaqueMemoryRef("notes/graph-target.md"),
			"project_ref": graphRecallOpaqueMemoryRef("project-a"),
			"graph_provenance": map[string]any{
				"schema_id": "graph_provenance.v1", "source": memoryEdgeSource, "source_owner": sourceOwnerGoNative,
				"seed_memory_ref":   graphRecallOpaqueMemoryRef("project-a::notes/seed.md"),
				"target_memory_ref": targetRef, "edge_ref": "sha256:" + strings.Repeat("e", 64), "relation": "references",
			},
		}},
		"final_model_visible_memory_refs": []string{targetRef},
		"final_model_visible_file_refs":   []string{graphRecallOpaqueMemoryRef("notes/graph-target.md")},
	}
	sample["final_model_visible_digest"] = graphRecallFinalModelVisibleDigest(sample)
	contribution := map[string]any{"enabled": true, "added_hydrated_expected_hit_count": 1, "added_matched_memory_ids": []string{targetID}}
	if !graphRecallFinalModelVisibleGraphAttribution(sample, targetID, map[string]struct{}{"notes/graph-target.md": {}}, "project-a", contribution) {
		t.Fatalf("valid final-visible graph provenance was not attributed: %#v", sample)
	}
	noContribution := cloneJSONMap(contribution)
	noContribution["added_hydrated_expected_hit_count"] = 0
	if graphRecallFinalModelVisibleGraphAttribution(sample, targetID, nil, "project-a", noContribution) {
		t.Fatal("direct-present/no-graph-contribution incorrectly counted as a graph hit")
	}
	disabled := cloneJSONMap(contribution)
	disabled["enabled"] = false
	if graphRecallFinalModelVisibleGraphAttribution(sample, targetID, nil, "project-a", disabled) {
		t.Fatal("disabled graph operation incorrectly counted as a graph hit")
	}
	withoutProvenance := cloneJSONMap(sample)
	withoutProvenance["final_model_visible_evidence"] = []map[string]any{{
		"rank": 1, "response_rank": 1, "response_ref": graphRecallOpaqueMemoryRef("response-target"),
		"memory_ref": targetRef, "file_ref": graphRecallOpaqueMemoryRef("notes/graph-target.md"),
		"project_ref": graphRecallOpaqueMemoryRef("project-a"),
	}}
	withoutProvenance["final_model_visible_digest"] = graphRecallFinalModelVisibleDigest(withoutProvenance)
	if graphRecallFinalModelVisibleGraphAttribution(withoutProvenance, targetID, nil, "project-a", contribution) {
		t.Fatal("final-visible target without graph provenance incorrectly counted as a graph hit")
	}
	if graphRecallFinalModelVisibleGraphAttribution(sample, targetID, nil, "project-b", contribution) {
		t.Fatal("cross-project graph provenance incorrectly counted as a graph hit")
	}
}

func TestGraphRecallQualityAndCostCallerReceiptsFailClosed(t *testing.T) {
	forgedSample := map[string]any{
		"signals":   map[string]any{"ranked_evidence_count": 10, "returned_source_count": 5, "source_coverage_complete": true, "tokenizer_exact": true},
		"authority": "caller", "case_set_digest": "sha256:case", "snapshot_digest": "sha256:snapshot",
	}
	quality := graphRecallQualityCalibrationSnapshotForCohort("sha256:case", "sha256:snapshot", nil, []map[string]any{{
		"id": "case-1", "status": "evaluated", "context_quality_sample": forgedSample,
	}}, map[string]any{
		"context_quality": map[string]any{
			"case_set_digest": "sha256:case", "snapshot_digest": "sha256:snapshot",
			"samples": []map[string]any{{"case_id": "case-1", "signals": forgedSample["signals"], "authority": "caller"}},
		},
	})
	if anyToBool(quality["available"]) || anyToString(quality["reason"]) != "same_snapshot_graph_quality_cohort_incomplete" {
		t.Fatalf("caller quality receipt passed same-snapshot gate: %#v", quality)
	}
	originalCommit, originalTree := contextLatticeSourceCommit, contextLatticeSourceTree
	contextLatticeSourceCommit, contextLatticeSourceTree = strings.Repeat("f", 40), strings.Repeat("g", 40)
	t.Cleanup(func() { contextLatticeSourceCommit, contextLatticeSourceTree = originalCommit, originalTree })
	qualitySample := map[string]any{
		"signals":   map[string]any{"ranked_evidence_count": 10, "high_impact_evidence_count": 2, "omitted_high_value_count": 0, "returned_source_count": 3, "warning_count": 0, "tokenizer_exact": true, "token_budget_active": true, "source_coverage_complete": true, "graph_context_used": true, "exact_prompt_tokens_saved": 5},
		"authority": "execute_retrieval_server", "scorer_schema_id": contextPackQualitySchemaID, "scorer_version": 1,
		"case_id": "positive-1", "case_set_digest": "sha256:case", "snapshot_digest": "sha256:snapshot", "source_runtime_identity": contextLatticeBuildIdentity(), "captured_at": nowUTCISO(),
	}
	missingPositiveQuality := graphRecallQualityCalibrationSnapshotForCohort("sha256:case", "sha256:snapshot", []map[string]any{{"id": "positive-1"}, {"id": "positive-2"}}, []map[string]any{{"id": "positive-1", "status": "evaluated", "context_quality_sample": qualitySample}}, nil)
	if anyToBool(missingPositiveQuality["available"]) || anyToString(missingPositiveQuality["reason"]) != "same_snapshot_graph_quality_cohort_incomplete" {
		t.Fatalf("missing positive quality row passed full-cohort gate: %#v", missingPositiveQuality)
	}
	binding := map[string]any{"schema_id": savedRecallEvalV3SchemaID, "version": savedRecallEvalV3Version, "k": defaultRecallEvalK, "case_set_digest": "sha256:direct", "snapshot_digest": "sha256:snapshot"}
	gate := graphRecallPromotionGate(true,
		map[string]any{"passed": true, "recallAtK": 1.0, "mrr": 1.0, "numericExactness": 1.0, "citationCoverage": 1.0, "sourceDiversity": 1.0, "baseline": map[string]any{"binding": binding, "recallAtK": 1.0, "mrr": 1.0, "numericExactness": 1.0, "citationCoverage": 1.0, "sourceDiversity": 1.0}},
		map[string]any{"available": true, "same_snapshot": true, "cohort_complete": true, "formula": "unchanged_contextPackQualityScore_0_to_100", "mean": 95.0, "p10": 91.0},
		map[string]any{"positive_cases": 90, "graph_hits": 90, "graph_recall_at_5": 1.0, "incremental_denominator": 90, "incremental_help": 1.0, "hard_negative_cases": 10, "hard_negative_passed": 10, "explicit_cases": 90},
		map[string]any{"valid": true, "benchmark_eligible": true, "direct_baseline_binding": binding, "cost": map[string]any{"cost_observability": map[string]any{"schema_id": "retrieval_cost_observability.v1", "proven_zero": true}}},
	)
	if anyToBool(gate["promotion_eligible"]) || !graphCorpusContainsString(anyToStringSlice(gate["blocked_reasons"]), "cost_observability_unknown") {
		t.Fatalf("forged caller zero-cost receipt passed promotion: %#v", gate)
	}
}

func TestRetrievalCostObservabilityClassifiesRemoteNativeAndUnknownTransports(t *testing.T) {
	bindGraphCorpusFixtureRuntime(t)
	t.Setenv("ORCH_FASTEMBED_RS_BASE_URL", "")
	t.Setenv("ORCH_EMBED_PROVIDER", "cheap")
	t.Setenv("QDRANT_LOCAL_URL", "")
	t.Setenv("QDRANT_URL", "")
	loopbackFallback := retrievalCostObservabilityEnvelope(
		&server{backendURL: "http://127.0.0.1:8075"},
		map[string]any{"traffic_class": "evaluation_holdout"},
		[]string{sourceQdrant}, map[string]string{sourceQdrant: sourceOwnerPythonBackendFallback},
		map[string][]map[string]any{}, map[string]any{"staged_fetch": map[string]any{}},
	)
	if anyToBool(loopbackFallback["proven_zero"]) || anyToBool(loopbackFallback["transport_observed"]) || len(anyToStringSlice(anyMap(loopbackFallback["source_policy"])["blocked_reasons"])) == 0 {
		t.Fatalf("loopback Python fallback was incorrectly eligible for sealed cost proof: %#v", loopbackFallback)
	}
	store := &memoryStore{policy: memoryStorePolicy{enabled: true}}
	store.ready.Store(true)
	localServer := &server{memoryStore: store}
	localPreflight := retrievalEvaluationSourcePreflight(localServer, []string{sourceTopicRollup})
	nativeInProcess := retrievalCostObservabilityEnvelope(
		localServer,
		map[string]any{"traffic_class": "evaluation_holdout", "_evaluation_source_preflight": localPreflight},
		[]string{sourceTopicRollup}, map[string]string{sourceTopicRollup: sourceOwnerGoNative},
		map[string][]map[string]any{}, map[string]any{"staged_fetch": map[string]any{}},
	)
	if !anyToBool(nativeInProcess["proven_zero"]) || anyToInt(nativeInProcess["external_network_calls"], 0) != 0 || anyToString(anyMap(anyMap(nativeInProcess["transport_classification"])[sourceTopicRollup])["class"]) != "in_process" {
		t.Fatalf("provider-incapable in-process lane was not eligible: %#v", nativeInProcess)
	}
	t.Setenv("QDRANT_LOCAL_URL", "http://127.0.0.1:6333")
	t.Setenv("QDRANT_URL", "")
	localNativeServer := &server{}
	localNativePreflight := retrievalEvaluationSourcePreflight(localNativeServer, []string{sourceQdrant})
	localNative := retrievalCostObservabilityEnvelope(
		localNativeServer,
		map[string]any{"traffic_class": "evaluation_holdout", "_evaluation_source_preflight": localNativePreflight},
		[]string{sourceQdrant}, map[string]string{sourceQdrant: sourceOwnerGoNative},
		map[string][]map[string]any{}, map[string]any{"staged_fetch": map[string]any{}},
	)
	if !anyToBool(localNative["proven_zero"]) || anyToInt(localNative["network_calls"], 0) != 1 || anyToInt(localNative["local_backend_calls"], 0) != 1 || anyToInt(localNative["external_network_calls"], 0) != 0 || anyToString(anyMap(anyMap(localNative["transport_classification"])[sourceQdrant])["class"]) != "approved_local_endpoint" {
		t.Fatalf("approved local native data-store call was not allowed with external/provider zero: %#v", localNative)
	}
	t.Setenv("ORCH_EMBED_PROVIDER", "fastembed-rs")
	t.Setenv("ORCH_FASTEMBED_RS_BASE_URL", "http://127.0.0.1:8090")
	localProvider := retrievalCostObservabilityEnvelope(
		&server{},
		map[string]any{"traffic_class": "evaluation_holdout"},
		[]string{sourceQdrant}, map[string]string{sourceQdrant: sourceOwnerGoNative},
		map[string][]map[string]any{}, map[string]any{"staged_fetch": map[string]any{}},
	)
	if anyToBool(localProvider["proven_zero"]) || anyToBool(localProvider["transport_observed"]) {
		t.Fatalf("provider-capable loopback embedding service was incorrectly eligible without downstream zero receipt: %#v", localProvider)
	}
	t.Setenv("ORCH_EMBED_PROVIDER", "cheap")
	t.Setenv("ORCH_FASTEMBED_RS_BASE_URL", "")
	t.Setenv("QDRANT_LOCAL_URL", "")
	rescue := retrievalCostObservabilityEnvelope(
		&server{backendURL: "http://127.0.0.1:8075"},
		map[string]any{"traffic_class": "evaluation_holdout"},
		[]string{sourceQdrant}, map[string]string{sourceQdrant: sourceOwnerPythonBackendFallback},
		map[string][]map[string]any{}, map[string]any{"staged_fetch": map[string]any{"coverage_rescue_applied": true}},
	)
	if anyToBool(rescue["proven_zero"]) || anyToBool(rescue["transport_observed"]) {
		t.Fatalf("rescue path was incorrectly eligible for sealed cost proof: %#v", rescue)
	}
	redirect := retrievalCostObservabilityEnvelope(
		&server{memoryStore: store},
		map[string]any{"traffic_class": "evaluation_holdout"},
		[]string{sourceTopicRollup}, map[string]string{sourceTopicRollup: sourceOwnerGoNative},
		map[string][]map[string]any{}, map[string]any{"staged_fetch": map[string]any{"redirected_sources": []string{sourceTopicRollup}}},
	)
	if anyToBool(redirect["proven_zero"]) || anyToBool(redirect["transport_observed"]) {
		t.Fatalf("local redirect/proxy path was incorrectly eligible for sealed cost proof: %#v", redirect)
	}
	remoteBackend := retrievalCostObservabilityEnvelope(
		&server{backendURL: "https://remote.example.invalid"},
		map[string]any{"traffic_class": "evaluation_holdout"},
		[]string{sourceQdrant}, map[string]string{sourceQdrant: sourceOwnerPythonBackendFallback},
		map[string][]map[string]any{}, map[string]any{"staged_fetch": map[string]any{}},
	)
	if anyToBool(remoteBackend["proven_zero"]) || anyToInt(remoteBackend["external_network_calls"], 0) == 0 {
		t.Fatalf("remote backend was incorrectly proven zero-cost: %#v", remoteBackend)
	}
	t.Setenv("QDRANT_URL", "https://qdrant.example.invalid")
	remoteNative := retrievalCostObservabilityEnvelope(
		&server{backendURL: "http://127.0.0.1:8075"},
		map[string]any{"traffic_class": "evaluation_holdout"},
		[]string{sourceQdrant}, map[string]string{sourceQdrant: sourceOwnerGoNative},
		map[string][]map[string]any{}, map[string]any{"staged_fetch": map[string]any{}},
	)
	if anyToBool(remoteNative["proven_zero"]) || anyToInt(remoteNative["external_network_calls"], 0) == 0 {
		t.Fatalf("remote native Qdrant endpoint was incorrectly proven local: %#v", remoteNative)
	}
	t.Setenv("QDRANT_URL", "http://contextlattice-fake:6333")
	arbitrarySingleLabel := retrievalCostObservabilityEnvelope(
		&server{backendURL: "http://127.0.0.1:8075"},
		map[string]any{"traffic_class": "evaluation_holdout"},
		[]string{sourceQdrant}, map[string]string{sourceQdrant: sourceOwnerGoNative},
		map[string][]map[string]any{}, map[string]any{"staged_fetch": map[string]any{}},
	)
	if anyToBool(arbitrarySingleLabel["proven_zero"]) || anyToBool(arbitrarySingleLabel["transport_observed"]) {
		t.Fatalf("arbitrary single-label hostname was incorrectly approved local: %#v", arbitrarySingleLabel)
	}
	unknown := retrievalCostObservabilityEnvelope(
		&server{backendURL: "http://127.0.0.1:8075"},
		map[string]any{"traffic_class": "evaluation_holdout"},
		[]string{sourceQdrant}, map[string]string{sourceQdrant: "mystery_owner"},
		map[string][]map[string]any{}, map[string]any{"staged_fetch": map[string]any{}},
	)
	if anyToBool(unknown["proven_zero"]) || anyToBool(unknown["transport_observed"]) {
		t.Fatalf("unknown transport was incorrectly proven: %#v", unknown)
	}
}

func TestEvaluationSourcePreflightRejectsExternalEndpointBeforeAdapterExecution(t *testing.T) {
	bindGraphCorpusFixtureRuntime(t)
	t.Setenv("GO_RETRIEVAL_NATIVE_QDRANT_ENABLED", "true")
	t.Setenv("QDRANT_LOCAL_URL", "")
	t.Setenv("QDRANT_URL", "https://qdrant.example.invalid")
	t.Setenv("ORCH_EMBED_PROVIDER", "cheap")
	t.Setenv("ORCH_FASTEMBED_RS_BASE_URL", "")
	var adapterCalls atomic.Int32
	s := &server{
		client: &http.Client{Transport: graphRecallRoundTripFunc(func(*http.Request) (*http.Response, error) {
			adapterCalls.Add(1)
			return nil, fmt.Errorf("adapter transport must not execute")
		})},
	}
	response, status, err := s.executeRetrieval(context.Background(), nil, map[string]any{
		"query": "sealed provider-free graph control", "limit": defaultRecallEvalK,
		"sources": []string{sourceQdrant}, "traffic_class": "evaluation_holdout",
	}, true)
	if status != http.StatusPreconditionFailed || err == nil || anyToString(response["code"]) != "evaluation_source_preflight_rejected" {
		t.Fatalf("external source was not rejected by pre-execution policy: status=%d response=%#v err=%v", status, response, err)
	}
	if calls := adapterCalls.Load(); calls != 0 {
		t.Fatalf("source adapter executed %d times before evaluation rejection", calls)
	}
	preflight := anyMap(response["source_policy_preflight"])
	if anyToBool(preflight["eligible"]) || !anyToBool(preflight["pre_execution_enforced"]) || len(anyToStringSlice(preflight["blocked_reasons"])) == 0 {
		t.Fatalf("preflight rejection receipt was incomplete: %#v", preflight)
	}
}

func TestEvaluationHoldoutLocalAdapterCannotRedirectExternally(t *testing.T) {
	bindGraphCorpusFixtureRuntime(t)
	t.Setenv("GO_RETRIEVAL_NATIVE_QDRANT_ENABLED", "true")
	t.Setenv("QDRANT_LOCAL_URL", "http://127.0.0.1:6333")
	t.Setenv("QDRANT_URL", "")
	t.Setenv("ORCH_EMBED_PROVIDER", "cheap")
	t.Setenv("ORCH_FASTEMBED_RS_BASE_URL", "")
	var localCalls atomic.Int32
	var escapedCalls atomic.Int32
	s := &server{
		driftByClass: map[string]uint64{}, driftBySource: map[string]uint64{},
		client: &http.Client{Transport: graphRecallRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Hostname() == "provider.example.invalid" {
				escapedCalls.Add(1)
				return nil, fmt.Errorf("external redirect escape executed")
			}
			localCalls.Add(1)
			return &http.Response{
				StatusCode: http.StatusTemporaryRedirect,
				Header:     http.Header{"Location": []string{"https://provider.example.invalid/escape"}},
				Body:       http.NoBody,
				Request:    request,
			}, nil
		})},
	}
	if preflight := retrievalEvaluationSourcePreflight(s, []string{sourceQdrant}); !retrievalEvaluationSourcePreflightValid(preflight) {
		t.Fatalf("approved local adapter did not pass provider-incapable preflight: %#v", preflight)
	}
	_, _, _, _, _, _ = s.callBackendSourceQuery(context.Background(), nil, map[string]any{
		"query": "sealed redirect boundary", "limit": defaultRecallEvalK,
		"traffic_class": "evaluation_holdout",
	}, sourceQdrant, true)
	if localCalls.Load() == 0 {
		t.Fatal("approved local adapter was not exercised")
	}
	if calls := escapedCalls.Load(); calls != 0 {
		t.Fatalf("evaluation adapter followed %d redirect(s) to an external endpoint", calls)
	}
}

func TestEvaluationHoldoutNativeFailureCannotEscapeToLoopbackBackend(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_NATIVE_QDRANT_ENABLED", "true")
	t.Setenv("QDRANT_LOCAL_URL", "http://127.0.0.1:6333")
	t.Setenv("QDRANT_URL", "")
	t.Setenv("ORCH_EMBED_PROVIDER", "cheap")
	var qdrantCalls atomic.Int32
	var backendCalls atomic.Int32
	s := &server{
		backendURL:    "http://127.0.0.1:8075",
		driftByClass:  map[string]uint64{},
		driftBySource: map[string]uint64{},
		client: &http.Client{Transport: graphRecallRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Port() == "8075" {
				backendCalls.Add(1)
			} else {
				qdrantCalls.Add(1)
			}
			return nil, fmt.Errorf("synthetic native qdrant failure")
		})},
	}
	_, _, _, owner, _, err := s.callBackendSourceQuery(context.Background(), nil, map[string]any{
		"query": "sealed fallback test", "limit": defaultRecallEvalK, "traffic_class": "evaluation_holdout",
	}, sourceQdrant, true)
	if err == nil || !strings.Contains(err.Error(), "evaluation holdout forbids backend fallback") || owner != sourceOwnerGoNative {
		t.Fatalf("native failure did not stop at the provider-incapable boundary: owner=%q err=%v", owner, err)
	}
	if qdrantCalls.Load() == 0 || backendCalls.Load() != 0 {
		t.Fatalf("native failure escaped to loopback backend: native_calls=%d backend_calls=%d", qdrantCalls.Load(), backendCalls.Load())
	}
}

func TestEvaluationHoldoutEmptyTopicRollupCannotFallbackToBackend(t *testing.T) {
	store := newGraphRecallFixtureMemoryStore(t)
	var backendCalls atomic.Int32
	s := &server{
		memoryStore: store, backendURL: "http://127.0.0.1:8075",
		client: &http.Client{Transport: graphRecallRoundTripFunc(func(*http.Request) (*http.Response, error) {
			backendCalls.Add(1)
			return nil, fmt.Errorf("evaluation topic rollup must not call backend")
		})},
	}
	rows, _, _, owner, _, err := s.callBackendSourceQuery(context.Background(), nil, map[string]any{
		"query": "no local match", "limit": defaultRecallEvalK, "project": "alpha", "topic_path": "graph/test",
		"traffic_class": "evaluation_holdout",
	}, sourceTopicRollup, true)
	if err != nil || len(rows) != 0 || owner != sourceOwnerGoNative {
		t.Fatalf("empty in-process topic rollup did not terminate locally: rows=%#v owner=%q err=%v", rows, owner, err)
	}
	if calls := backendCalls.Load(); calls != 0 {
		t.Fatalf("empty evaluation topic rollup escaped to backend %d times", calls)
	}
}

func TestEvaluationPreflightRejectsNativeMemoryBankBackend(t *testing.T) {
	bindGraphCorpusFixtureRuntime(t)
	t.Setenv("ORCH_MEMORY_BANK_SEARCH_BACKEND", "native")
	var backendCalls atomic.Int32
	s := &server{
		backendURL: "http://127.0.0.1:8075",
		client: &http.Client{Transport: graphRecallRoundTripFunc(func(*http.Request) (*http.Response, error) {
			backendCalls.Add(1)
			return nil, fmt.Errorf("memory bank backend must not execute")
		})},
	}
	response, status, err := s.executeRetrieval(context.Background(), nil, map[string]any{
		"query": "sealed memory bank test", "limit": defaultRecallEvalK,
		"sources": []string{sourceMemoryBank}, "traffic_class": "evaluation_holdout",
	}, true)
	if status != http.StatusPreconditionFailed || err == nil || anyToString(response["code"]) != "evaluation_source_preflight_rejected" {
		t.Fatalf("provider-capable native memory bank was not rejected pre-execution: status=%d response=%#v err=%v", status, response, err)
	}
	if calls := backendCalls.Load(); calls != 0 {
		t.Fatalf("memory bank backend executed %d times before rejection", calls)
	}
}

func TestEvaluationPreflightRejectsDisabledProviderCapableMemoryBankLane(t *testing.T) {
	bindGraphCorpusFixtureRuntime(t)
	t.Setenv("ORCH_MEMORY_BANK_SEARCH_BACKEND", "disabled")
	preflight := retrievalEvaluationSourcePreflight(&server{}, []string{sourceMemoryBank})
	if retrievalEvaluationSourcePreflightValid(preflight) || anyToBool(preflight["eligible"]) {
		t.Fatalf("disabled provider-capable lane was mislabeled provider-incapable: %#v", preflight)
	}
	transport := anyMap(anyMap(preflight["transport_classification"])[sourceMemoryBank])
	if anyToString(transport["class"]) != "provider_capable_source" {
		t.Fatalf("memory-bank preflight did not retain provider-capable source truth: %#v", transport)
	}
}

func TestSavedRecallGraphControlCostBindsAll270PositiveControls(t *testing.T) {
	candidates := graphCorpusFixtureCandidates(400)
	edges := graphCorpusFixtureEdges(candidates)
	artifact := buildSavedRecallGraphCorpusFromCandidates(candidates, edges, "fixture-seed", "current_state_bottom_k", graphCorpusFixtureSourceStats(t, candidates, edges))
	cost := anyMap(artifact["cost"])
	if anyToInt(cost["control_requests_expected"], 0) != savedRecallGraphCorpusTotalPositiveCases || anyToInt(cost["control_requests_observed"], 0) != savedRecallGraphCorpusTotalPositiveCases || anyToInt(cost["control_requests_missing"], -1) != 0 {
		t.Fatalf("complete control cohort is not accounted as 270 observations: %#v", cost)
	}

	tampered := cloneJSONMap(artifact)
	tamperedCost := anyMap(tampered["cost"])
	for _, field := range []string{"control_requests_expected", "control_requests_attempted", "control_requests_observed", "observation_expected", "observation_expected_required", "observation_attempted", "observation_observed"} {
		tamperedCost[field] = savedRecallGraphCorpusPositiveCases
	}
	policy := anyMap(tamperedCost["source_policy_run"])
	policy["expected_case_count"] = savedRecallGraphCorpusPositiveCases
	policy["observed_case_count"] = savedRecallGraphCorpusPositiveCases
	policy["digest"] = "sha256:" + graphCorpusDigestMap(policy, "digest")
	tamperedCost["digest"] = "sha256:" + graphCorpusDigestMap(tamperedCost, "digest")
	manifest := anyMap(tampered["manifest"])
	manifest["control_requests_expected"] = savedRecallGraphCorpusPositiveCases
	manifest["control_requests_observed"] = savedRecallGraphCorpusPositiveCases
	manifest["control_cost_digest"] = tamperedCost["digest"]
	manifest["digest"] = "sha256:" + graphCorpusDigestMap(manifest, "digest")
	custody := anyMap(tampered["custody"])
	custody["control_cost_digest"] = tamperedCost["digest"]
	custody["manifest_digest"] = manifest["digest"]
	health := savedRecallGraphCorpusHealth("", tampered)
	if anyToBool(health["valid"]) || !graphCorpusContainsIssueCode(health, "control_cost_invalid") {
		t.Fatalf("self-digested 90-of-270 economics receipt passed closed validation: %#v", health)
	}

	inconsistent := cloneJSONMap(artifact)
	inconsistentCost := anyMap(inconsistent["cost"])
	inconsistentCost["network_calls"] = anyToInt(inconsistentCost["network_calls"], 0) + 1
	inconsistentCost["digest"] = "sha256:" + graphCorpusDigestMap(inconsistentCost, "digest")
	inconsistentManifest := anyMap(inconsistent["manifest"])
	inconsistentManifest["network_calls"] = inconsistentCost["network_calls"]
	inconsistentManifest["control_cost_digest"] = inconsistentCost["digest"]
	inconsistentManifest["digest"] = "sha256:" + graphCorpusDigestMap(inconsistentManifest, "digest")
	inconsistentCustody := anyMap(inconsistent["custody"])
	inconsistentCustody["control_cost_digest"] = inconsistentCost["digest"]
	inconsistentCustody["manifest_digest"] = inconsistentManifest["digest"]
	inconsistentHealth := savedRecallGraphCorpusHealth("", inconsistent)
	if anyToBool(inconsistentHealth["valid"]) || !graphCorpusContainsIssueCode(inconsistentHealth, "control_cost_case_receipt_mismatch") || graphCorpusContainsIssueCode(inconsistentHealth, "control_cost_invalid") {
		t.Fatalf("self-digested aggregate cost inconsistent with 270 per-control receipts escaped custody validation: %#v", inconsistentHealth)
	}
}

func TestGraphCorpusSemanticDigestExcludesTemporalControlCustody(t *testing.T) {
	candidates := graphCorpusFixtureCandidates(400)
	edges := graphCorpusFixtureEdges(candidates)
	artifact := buildSavedRecallGraphCorpusFromCandidates(candidates, edges, "fixture-seed", "current_state_bottom_k", graphCorpusFixtureSourceStats(t, candidates, edges))
	cases := anyToMapSlice(artifact["cases"])
	semanticBefore := graphCorpusCaseSetDigest(cases)
	custodyBefore := graphCorpusCaseCustodyDigest(cases)
	changed := false
	for _, row := range cases {
		if anyToBool(row["hard_negative"]) {
			continue
		}
		control := anyMap(row["incremental_control"])
		cost := anyMap(control["cost_observability"])
		cost["captured_at"] = "2026-02-02T03:04:05Z"
		cost["digest"] = "sha256:" + graphCorpusDigestMap(cost, "digest")
		control["captured_at"] = "2026-02-02T03:04:05Z"
		anyMap(control["control_final_model_visible"])["production_response_digest"] = "sha256:" + strings.Repeat("9", 64)
		control["digest"] = "sha256:" + graphCorpusDigestMap(control, "digest")
		changed = true
		break
	}
	if !changed {
		t.Fatal("fixture did not contain a positive control receipt")
	}
	if semanticAfter := graphCorpusCaseSetDigest(cases); semanticAfter != semanticBefore {
		t.Fatalf("custody-only capture time changed semantic case digest: before=%s after=%s", semanticBefore, semanticAfter)
	}
	if custodyAfter := graphCorpusCaseCustodyDigest(cases); custodyAfter == custodyBefore {
		t.Fatal("custody digest did not bind the changed temporal receipt")
	}
	snapshot := anyMap(artifact["snapshot"])
	semanticSnapshotBefore := anyToString(snapshot["digest"])
	temporalSnapshotBefore := graphCorpusSnapshotCaptureDigest(snapshot)
	snapshot["captured_at"] = "2026-02-03T04:05:06Z"
	if semanticSnapshotAfter := "sha256:" + graphCorpusDigestMap(snapshot, "digest", "captured_at"); semanticSnapshotAfter != semanticSnapshotBefore {
		t.Fatalf("capture time changed semantic source-edge snapshot identity: before=%s after=%s", semanticSnapshotBefore, semanticSnapshotAfter)
	}
	if temporalSnapshotAfter := graphCorpusSnapshotCaptureDigest(snapshot); temporalSnapshotAfter == temporalSnapshotBefore {
		t.Fatal("snapshot custody digest did not bind the changed capture time")
	}
	health := savedRecallGraphCorpusHealth("", artifact)
	if anyToBool(health["valid"]) || !graphCorpusContainsIssueCode(health, "snapshot_capture_digest_mismatch") {
		t.Fatalf("temporal snapshot tampering escaped custody validation: %#v", health)
	}
}

func TestGraphRecallFinalVisibleDigestSeparatesTemporalResponseCustody(t *testing.T) {
	sample := map[string]any{
		"production_response_seam": true, "production_final_response_seam": true,
		"production_response_schema_id":      recallResponseContractID,
		"production_response_digest":         "sha256:" + strings.Repeat("a", 64),
		"production_response_scope_digest":   "sha256:" + strings.Repeat("b", 64),
		"production_response_contract_valid": true,
		"side_effects_suppressed":            true, "competing_interventions_disabled": true,
		"final_model_visible_k": defaultRecallEvalK, "final_model_visible_ordered": true,
		"final_model_visible_evidence":    []map[string]any{{"rank": 1, "response_rank": 1, "response_ref": "sha256:" + strings.Repeat("c", 64), "memory_ref": "sha256:" + strings.Repeat("d", 64)}},
		"final_model_visible_memory_refs": []string{"sha256:" + strings.Repeat("d", 64)},
		"final_model_visible_file_refs":   []string{},
	}
	semanticDigest := graphRecallFinalModelVisibleDigest(sample)
	sample["final_model_visible_digest"] = semanticDigest
	if !graphRecallFinalModelVisibleProjectionValid(sample) {
		t.Fatalf("baseline final-visible projection is invalid: %#v", sample)
	}

	laterCustody := cloneJSONMap(sample)
	laterCustody["production_response_digest"] = "sha256:" + strings.Repeat("e", 64)
	laterCustody["final_model_visible_digest"] = graphRecallFinalModelVisibleDigest(laterCustody)
	if anyToString(laterCustody["final_model_visible_digest"]) != semanticDigest || !graphRecallFinalModelVisibleProjectionValid(laterCustody) {
		t.Fatalf("temporal response custody changed the stable paired evidence identity: before=%s after=%#v", semanticDigest, laterCustody)
	}

	contaminatedScope := cloneJSONMap(sample)
	contaminatedScope["production_response_scope_digest"] = "sha256:" + strings.Repeat("f", 64)
	if graphRecallFinalModelVisibleDigest(contaminatedScope) == semanticDigest {
		t.Fatal("request-scope contamination escaped the stable paired evidence identity")
	}
}

func TestGraphRecallControlRequestDigestAllowsOnlyGraphTreatmentDelta(t *testing.T) {
	control := map[string]any{
		"query": "sealed paired request", "limit": defaultRecallEvalK, "project": "alpha", "topic_path": "graph/test",
		"retrieval_mode": "balanced", "retrieval_intent": "decision", "sources": []string{sourceTopicRollup},
		"include_grounding": true, "include_retrieval_debug": false, "include_preferences": false, "rerank_with_learning": false,
		"user_id": "", "agent_id": "agent-a", "auto_escalate": false, "deep_async": false,
		"callback_url": "", "traffic_class": "evaluation_holdout",
	}
	treatment := cloneJSONMap(control)
	treatment["graph_influence_disabled"] = false
	treatment["graph_backend_consulted"] = true
	if graphRecallControlRequestDigest(control) != graphRecallControlRequestDigest(treatment) {
		t.Fatal("graph-only treatment markers changed the paired semantic request digest")
	}
	contaminated := cloneJSONMap(treatment)
	contaminated["rerank_with_learning"] = true
	if graphRecallControlRequestDigest(control) == graphRecallControlRequestDigest(contaminated) {
		t.Fatal("non-graph ranking intervention was not detected in paired request digest")
	}
	contaminated = cloneJSONMap(treatment)
	contaminated["sources"] = []string{sourceQdrant}
	if graphRecallControlRequestDigest(control) == graphRecallControlRequestDigest(contaminated) {
		t.Fatal("source-cohort contamination was not detected in paired request digest")
	}
	contaminated = cloneJSONMap(treatment)
	contaminated["include_retrieval_debug"] = true
	if graphRecallControlRequestDigest(control) == graphRecallControlRequestDigest(contaminated) {
		t.Fatal("debug-response contamination was not detected in paired request digest")
	}
}

func TestSavedRecallGraphCorpusRequiresExactPerRelationSplitQuotas(t *testing.T) {
	candidates := graphCorpusFixtureCandidates(400)
	edges := graphCorpusFixtureEdges(candidates)
	artifact := cloneJSONMap(buildSavedRecallGraphCorpusFromCandidates(candidates, edges, "fixture-seed", "current_state_bottom_k", graphCorpusFixtureSourceStats(t, candidates, edges)))
	cases := anyToMapSlice(artifact["cases"])
	changed := false
	for _, row := range cases {
		if anyToString(row["split"]) == "development" && anyToString(row["relation"]) == "references" && !anyToBool(row["hard_negative"]) {
			row["relation"] = "same_topic"
			row["graph_expected_relations"] = []string{"same_topic"}
			changed = true
			break
		}
	}
	if !changed {
		t.Fatal("fixture did not contain a development references row")
	}
	artifact["case_set_digest"] = "sha256:" + graphCorpusCaseSetDigest(cases)
	custody := anyMap(artifact["custody"])
	custody["case_set_digest"] = artifact["case_set_digest"]
	custody["case_capture_digest"] = "sha256:" + graphCorpusCaseCustodyDigest(cases)
	topology := map[string]any{"references": 0, "same_session": 0, "same_topic": 0, "hard_negative": 0}
	for _, row := range cases {
		relation := anyToString(row["relation"])
		if anyToBool(row["hard_negative"]) {
			relation = "hard_negative"
		}
		topology[relation] = anyToInt(topology[relation], 0) + 1
	}
	manifest := anyMap(artifact["manifest"])
	manifest["topology_counts"] = topology
	manifest["digest"] = "sha256:" + graphCorpusDigestMap(manifest, "digest")
	custody["manifest_digest"] = manifest["digest"]
	health := savedRecallGraphCorpusHealth("", artifact)
	if anyToBool(health["valid"]) || !graphCorpusContainsIssueCode(health, "topology_quota_mismatch") || graphCorpusContainsIssueCode(health, "topology_manifest_mismatch") {
		t.Fatalf("self-consistent 59/61 development topology mutation escaped the exact quota gate: %#v", health)
	}
}

func TestGraphRecallQualityUsesCompleteProductionResponseSeamWithoutSideEffects(t *testing.T) {
	bindGraphCorpusFixtureRuntime(t)
	t.Setenv("GO_CONTEXT_PACK_GRAPH_NEIGHBORS_ENABLED", "true")
	store := newGraphRecallFixtureMemoryStore(t)
	longGraphTarget := strings.Repeat("long graph target evidence remains compiler-bound ", 24)
	if len(longGraphTarget) <= 520 {
		t.Fatalf("long graph fixture must exceed the compiler identity limit: %d", len(longGraphTarget))
	}
	for _, item := range []normalizedWrite{
		{project: "alpha", fileName: "notes/seed.md", content: "seed memory", topicPath: "graph/test"},
		{project: "alpha", fileName: "notes/target.md", content: longGraphTarget, topicPath: "graph/test"},
	} {
		if _, _, err := store.put(item); err != nil {
			t.Fatalf("seed long graph memory: %v", err)
		}
	}
	if _, err := store.upsertMemoryEdge(context.Background(), memoryEdgeEntry{
		SourceID: "alpha::notes/seed.md", TargetID: "alpha::notes/target.md", Relation: "supports",
		Project: "alpha", TopicPath: "graph/test", Confidence: 0.94, CreatedAt: nowUTCISO(), Source: memoryEdgeSource,
	}); err != nil {
		t.Fatalf("seed long graph edge: %v", err)
	}
	var retrievalCalls atomic.Int32
	s := &server{
		memoryStore: store,
		client: &http.Client{Transport: graphRecallRoundTripFunc(func(*http.Request) (*http.Response, error) {
			retrievalCalls.Add(1)
			return nil, fmt.Errorf("provided evaluation response must not perform retrieval")
		})},
	}
	request := map[string]any{
		"query": "target memory graph support", "limit": defaultRecallEvalK, "project": "alpha", "topic_path": "graph/test",
		"retrieval_mode": "balanced", "retrieval_intent": "decision", "sources": []string{sourceTopicRollup},
		"include_grounding": true, "include_preferences": false, "rerank_with_learning": false,
		"agent_id": "agent-a", "session_id": "evaluation-session", "traffic_class": "evaluation_holdout",
	}
	searchResponse := map[string]any{
		"results": []map[string]any{{
			"project": "alpha", "file": "notes/seed.md", "memory_id": "alpha::notes/seed.md",
			"summary": "seed memory", "topic_path": "graph/test", "source": sourceTopicRollup, "score": 0.99,
		}},
		"grounding":      map[string]any{"facts": []any{}, "numeric_facts": []any{}, "strict_numeric_copy": true},
		"retrieval_mode": "balanced", "retrieval_intent": "decision", "traffic_class": "evaluation_holdout",
		"source_summary": map[string]any{
			"configured_sources": []string{sourceTopicRollup}, "effective_sources": []string{sourceTopicRollup},
			"sources": []string{sourceTopicRollup}, "returned_now": []string{sourceTopicRollup},
			"source_owners": map[string]any{sourceTopicRollup: sourceOwnerGoNative},
		},
	}
	response, status, err := s.buildContextPackResponseForSurfaceWithOptions(context.Background(), nil, request, "graph_recall_evaluation", contextPackResponseBuildOptions{
		useProvidedSearchResponse: true, providedSearchResponse: searchResponse, providedSearchStatus: http.StatusOK, suppressSideEffects: true, disableActiveContextPolicy: true, disableLearnedActivation: true,
	})
	if err != nil || status != http.StatusOK || anyToBool(response["writeback_required"]) || len(anyMap(response["context_pack"])) == 0 || len(anyMap(response["context_pack_quality"])) == 0 || len(anyMap(response["run_advisor"])) == 0 || response["reference_prompt"] == nil {
		t.Fatalf("complete production composer did not return a side-effect-free evaluation response: status=%d response=%#v err=%v", status, response, err)
	}
	if response["active_context_policy"] != nil || anyMap(response["context_pack"])["active_context_policy"] != nil {
		t.Fatalf("active context policy contaminated the isolated graph evaluation response: %#v", response)
	}
	if anyToBool(response["learning_enabled"]) || anyToBool(response["learned_ranking_armed"]) || anyToBool(response["learned_ranking_applied"]) {
		t.Fatalf("learned ranking contaminated the isolated graph evaluation response: %#v", response["learned_activation"])
	}
	controlVisible, _, controlOK := s.graphRecallProductionResponseProjection(context.Background(), request, searchResponse, true)
	if !controlOK || graphRecallFinalModelVisibleContains(controlVisible, "alpha::notes/target.md", map[string]struct{}{"notes/target.md": {}}) {
		t.Fatalf("graph-disabled production control included the graph-only target: %#v", controlVisible)
	}
	sample := s.graphRecallQualitySampleFromServer(context.Background(), request, searchResponse, nil, "case-production-seam", "sha256:case", "sha256:snapshot")
	if !anyToBool(sample["production_response_seam"]) || !anyToBool(sample["production_final_response_seam"]) || !anyToBool(sample["production_response_contract_valid"]) || anyToString(sample["production_response_schema_id"]) != recallResponseContractID || !utilitySHA256DigestValid(anyToString(sample["production_response_digest"])) || !utilitySHA256DigestValid(anyToString(sample["production_response_scope_digest"])) || !anyToBool(sample["side_effects_suppressed"]) || !anyToBool(sample["competing_interventions_disabled"]) || anyToInt(sample["final_model_visible_k"], 0) != defaultRecallEvalK || !anyToBool(sample["final_model_visible_ordered"]) {
		t.Fatalf("graph quality sample did not bind the production response seam: %#v", sample)
	}
	if !graphRecallFinalModelVisibleContains(sample, "alpha::notes/target.md", map[string]struct{}{"notes/target.md": {}}) {
		t.Fatalf("hydrated graph target was absent from production-visible top five: %#v", sample)
	}
	if _, valid := graphRecallQualityScoreFromSample(sample); !valid {
		t.Fatalf("production-visible graph quality sample was rejected by the promotion scorer: %#v", sample)
	}
	if calls := retrievalCalls.Load(); calls != 0 {
		t.Fatalf("provided-response production composition performed %d retrieval calls", calls)
	}
}

func TestGraphRecallSnapshotFenceDetectsCurrentStateMutation(t *testing.T) {
	store := newGraphRecallFixtureMemoryStore(t)
	s := &server{memoryStore: store}
	seedGraphContextMemory(t, s)
	snapshot := map[string]any{"capture_project": "alpha", "capture_topic_prefix": "graph/test"}
	before, err := s.currentSavedRecallGraphSourceEdgeDigest(context.Background(), snapshot)
	if err != nil || before == "" {
		t.Fatalf("capture initial source-edge snapshot: digest=%q err=%v", before, err)
	}
	if _, _, err := store.put(normalizedWrite{project: "alpha", fileName: "notes/concurrent.md", content: "concurrent snapshot mutation", topicPath: "graph/test"}); err != nil {
		t.Fatalf("mutate current-state snapshot: %v", err)
	}
	after, err := s.currentSavedRecallGraphSourceEdgeDigest(context.Background(), snapshot)
	if err != nil || after == "" {
		t.Fatalf("capture mutated source-edge snapshot: digest=%q err=%v", after, err)
	}
	if before == after {
		t.Fatalf("current-state mutation escaped semantic snapshot fence: %s", before)
	}
}

func TestSavedRecallDirectBaselineCaptureIsBoundAndNotGraphPostTreatment(t *testing.T) {
	root, err := os.MkdirTemp(os.Getenv("TMPDIR"), "graph-direct-baseline-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("ORCH_RECALL_DIRECT_BASELINE_PATH", filepath.Join(root, "baseline.json"))
	originalCommit, originalTree := contextLatticeSourceCommit, contextLatticeSourceTree
	contextLatticeSourceCommit = strings.Repeat("a", 40)
	contextLatticeSourceTree = strings.Repeat("b", 40)
	t.Cleanup(func() { contextLatticeSourceCommit, contextLatticeSourceTree = originalCommit, originalTree })
	cases := []map[string]any{{
		"id": "direct-1", "query": "alpha runbook", "project": "alpha", "topic_path": "runbooks/testing",
		"expected_files": []string{"notes/expected.md"}, "split": "train",
	}}
	cfg := recallEvalSavedConfig{
		SchemaID: savedRecallEvalV3SchemaID, Version: savedRecallEvalV3Version,
		CaseSetDigest: "sha256:" + recallEvalCaseSetDigest(cases), Snapshot: map[string]any{"digest": "sha256:direct-snapshot"},
		Custody: map[string]any{"synthetic": false}, K: defaultRecallEvalK, Gate: recallEvalGate{MinRecallAtK: 0.75, MinMRR: 0.55, MinNumericExactly: 0.90}, Cases: cases,
	}
	metrics := map[string]any{"recallAtK": 0.91, "mrr": 0.81, "numericExactness": 0.96, "citationCoverage": 0.92, "sourceDiversity": 1.4}
	binding := graphCorpusDirectBaselineBinding(cfg)
	control := graphRecallDirectControlCohortReceipt([]map[string]any{savedRecallDirectControlReceipt(map[string]any{
		"direct_baseline_case_id": "direct-1", "direct_baseline_case_set_digest": binding["case_set_digest"], "direct_baseline_snapshot_digest": binding["snapshot_digest"], "direct_baseline_k": binding["k"],
		"traffic_class": "evaluation_holdout",
	})}, cases, binding)
	receipt, err := captureSavedRecallDirectBaseline(cfg, metrics, control)
	if err != nil || !anyToBool(receipt["available"]) {
		t.Fatalf("authoritative direct baseline capture failed: receipt=%#v err=%v", receipt, err)
	}
	loaded, loadedReceipt := loadSavedRecallDirectBaseline(binding)
	if len(loaded) == 0 || !anyToBool(loadedReceipt["available"]) || !graphRecallMetricMapsEqual(loaded, metrics) {
		t.Fatalf("captured direct baseline was not loadable/bound: baseline=%#v receipt=%#v", loaded, loadedReceipt)
	}
	if replacement, replacementErr := captureSavedRecallDirectBaseline(cfg, map[string]any{"recallAtK": 0.99, "mrr": 0.99, "numericExactness": 0.99, "citationCoverage": 0.99, "sourceDiversity": 9.0}, control); replacementErr == nil || anyToBool(replacement["available"]) || anyToString(replacement["reason"]) != "baseline_immutable_existing" {
		t.Fatalf("sealed direct baseline was replaceable without a new binding: receipt=%#v err=%v", replacement, replacementErr)
	}
	raw, err := os.ReadFile(filepath.Join(root, "baseline.json"))
	if err != nil {
		t.Fatal(err)
	}
	forged := map[string]any{}
	if err := json.Unmarshal(raw, &forged); err != nil {
		t.Fatal(err)
	}
	anyMap(forged["metrics"])["citationCoverage"] = 0.01
	forgedRaw, _ := json.Marshal(forged)
	if err := os.WriteFile(filepath.Join(root, "baseline.json"), forgedRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if baseline, forgedReceipt := loadSavedRecallDirectBaseline(binding); len(baseline) != 0 || anyToBool(forgedReceipt["available"]) {
		t.Fatalf("forged direct baseline receipt passed closed validation: %#v", forgedReceipt)
	}
	if anyToBool(anyMap(forged["evaluation"])["graph_results_used"]) {
		t.Fatalf("direct baseline capture was marked as graph post-treatment: %#v", forged["evaluation"])
	}
	forgedControl := cloneJSONMap(control)
	forgedControl["graph_results_used"] = true
	if forgedReceipt, forgedErr := captureSavedRecallDirectBaseline(cfg, metrics, forgedControl); forgedErr == nil || anyToBool(forgedReceipt["available"]) || anyToString(forgedReceipt["reason"]) != "direct_control_receipt_invalid" {
		t.Fatalf("forged graph-disabled control receipt passed capture: receipt=%#v err=%v", forgedReceipt, forgedErr)
	}
	postTreatmentMetrics := cloneJSONMap(metrics)
	postTreatmentMetrics["candidate_allocation_active"] = true
	if postReceipt, postErr := captureSavedRecallDirectBaseline(cfg, postTreatmentMetrics, control); postErr == nil || anyToBool(postReceipt["available"]) || anyToString(postReceipt["reason"]) != "direct_control_receipt_invalid" {
		t.Fatalf("post-treatment baseline capture was accepted: receipt=%#v err=%v", postReceipt, postErr)
	}
	missingRoot := filepath.Join(root, "missing")
	t.Setenv("ORCH_RECALL_DIRECT_BASELINE_PATH", filepath.Join(missingRoot, "baseline.json"))
	if missing, missingReceipt := loadSavedRecallDirectBaseline(binding); len(missing) != 0 || anyToBool(missingReceipt["available"]) {
		t.Fatalf("missing baseline unexpectedly loaded: %#v", missingReceipt)
	}
	if _, statErr := os.Lstat(filepath.Join(missingRoot, "baseline.json")); !os.IsNotExist(statErr) {
		t.Fatalf("read-only baseline load materialized missing artifact: err=%v", statErr)
	}
}

func graphCorpusContainsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
