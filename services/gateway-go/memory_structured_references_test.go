package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func assertStructuredReferenceTransactionRecovered(t *testing.T, store *memoryStore, sourceID string, expectedEdges int) {
	t.Helper()
	edges, err := store.listMemoryEdges(context.Background(), memoryEdgeQuery{MemoryID: sourceID, Relation: "references", Direction: "out", Limit: expectedEdges + 1})
	if err != nil || len(edges) != expectedEdges {
		t.Fatalf("recovered structured edge set: edges=%#v err=%v", edges, err)
	}
	seenTargets := map[string]struct{}{}
	for _, edge := range edges {
		if edge.Binding == nil || anyToString(edge.Metadata["reference_transaction_id"]) == "" {
			t.Fatalf("recovered edge lacks closed transaction custody: %#v", edge)
		}
		seenTargets[edge.TargetID] = struct{}{}
	}
	if len(seenTargets) != expectedEdges {
		t.Fatalf("recovered structured edge set contains duplicates: %#v", edges)
	}
}

func countStructuredReferenceDurableRows(t *testing.T, store *memoryStore, sourceID string) (int, int) {
	t.Helper()
	historyRaw, err := os.ReadFile(store.policy.historyPath)
	if err != nil {
		t.Fatalf("read recovered history: %v", err)
	}
	historyRows := 0
	for _, line := range strings.Split(strings.TrimSpace(string(historyRaw)), "\n") {
		var entry memoryStoreEntry
		if json.Unmarshal([]byte(line), &entry) == nil && strings.EqualFold(entry.Project+"::"+entry.FileName, sourceID) {
			historyRows++
		}
	}
	edgeRaw, err := os.ReadFile(store.policy.edgePath)
	if err != nil {
		t.Fatalf("read recovered edge log: %v", err)
	}
	edgeRows := 0
	for _, line := range strings.Split(strings.TrimSpace(string(edgeRaw)), "\n") {
		var edge memoryEdgeEntry
		if json.Unmarshal([]byte(line), &edge) == nil && strings.EqualFold(edge.SourceID, sourceID) && memoryReferenceTransactionIDFromEdge(edge) != "" {
			edgeRows++
		}
	}
	return historyRows, edgeRows
}

func postStructuredReferenceBackfillStatus(t *testing.T, baseURL string, payload map[string]any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode reference backfill request: %v", err)
	}
	resp, err := http.Post(baseURL+"/v1/memory/edges/backfill", "application/json", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("reference backfill request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read reference backfill response: %v", err)
	}
	decoded := map[string]any{}
	if json.Unmarshal(body, &decoded) != nil {
		t.Fatalf("decode reference backfill response: status=%d body=%s", resp.StatusCode, string(body))
	}
	return resp.StatusCode, decoded
}

func structuredReferenceSamples(value any) []any {
	rows, _ := value.([]any)
	return rows
}

func TestStructuredReferenceTransactionRecoversEveryDurabilityFenceWithoutPartialVisibility(t *testing.T) {
	tests := []struct {
		name string
		arm  func(*memoryStore)
	}{
		{name: "history_append", arm: func(store *memoryStore) {
			store.beforeReferenceHistoryAppend = func() error { return errors.New("injected history append failure") }
		}},
		{name: "history_sync", arm: func(store *memoryStore) {
			store.beforeReferenceHistorySync = func() error { return errors.New("injected history sync failure") }
		}},
		{name: "edge_sync", arm: func(store *memoryStore) {
			store.beforeReferenceEdgeSync = func() error { return errors.New("injected edge sync failure") }
		}},
		{name: "edge_state", arm: func(store *memoryStore) {
			store.memoryEdgeLogBeforeStateWrite = func(memoryEdgeLogState) error { return errors.New("injected edge state failure") }
		}},
		{name: "receipt_close", arm: func(store *memoryStore) {
			store.beforeReferenceReceiptClose = func() error { return errors.New("injected receipt close failure") }
		}},
		{name: "current_state", arm: func(store *memoryStore) {
			store.beforeReferenceCurrentCommit = func() error { return errors.New("injected current-state failure") }
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, gateway := newMemoryGraphTestServer(t, true)
			defer gateway.Close()
			for index := 0; index < 2; index++ {
				if _, _, err := store.memoryStore.put(normalizedWrite{project: "alpha", fileName: fmt.Sprintf("notes/target-%d.md", index), content: "target", topicPath: "runbooks/transactions"}); err != nil {
					t.Fatalf("seed target %d: %v", index, err)
				}
			}
			if _, err := store.memoryStore.snapshotMemoryEdgeLog(0); err != nil {
				t.Fatalf("initialize exact edge-log state: %v", err)
			}
			write := normalizedWrite{
				project: "alpha", fileName: "notes/source.md", content: "source", topicPath: "runbooks/transactions",
				references: []memoryStructuredReference{
					{TargetID: "alpha::notes/target-0.md", Relation: "references", Confidence: 1},
					{TargetID: "alpha::notes/target-1.md", Relation: "references", Confidence: 1},
				},
			}
			test.arm(store.memoryStore)
			if _, _, err := store.memoryStore.put(write); err == nil {
				t.Fatal("injected durability fence must fail the initiating write")
			}
			if _, ok := store.memoryStore.currentEntry("alpha", "notes/source.md"); ok {
				t.Fatal("source must not become current before the transaction receipt and current-state commit")
			}
			if edges, err := store.memoryStore.listMemoryEdges(context.Background(), memoryEdgeQuery{MemoryID: "alpha::notes/source.md", Limit: 10}); err != nil || len(edges) != 0 {
				t.Fatalf("partial edge set became visible before restart reconciliation: edges=%#v err=%v", edges, err)
			}

			recovered, err := newMemoryStoreFromEnv()
			if err != nil {
				t.Fatalf("restart reconciliation: %v", err)
			}
			assertStructuredReferenceTransactionRecovered(t, recovered, "alpha::notes/source.md", 2)
			if _, _, err := recovered.put(write); err != nil {
				t.Fatalf("idempotent transaction retry: %v", err)
			}
			assertStructuredReferenceTransactionRecovered(t, recovered, "alpha::notes/source.md", 2)
			historyRows, edgeRows := countStructuredReferenceDurableRows(t, recovered, "alpha::notes/source.md")
			if historyRows != 1 || edgeRows != 2 {
				t.Fatalf("durable retry duplicated or lost rows: history=%d edges=%d", historyRows, edgeRows)
			}
		})
	}
}

func TestStructuredReferenceWriteRejectsPromotedRelationSemanticMismatch(t *testing.T) {
	tests := []struct {
		name          string
		relation      string
		sourceTopic   string
		targetTopic   string
		sourceSession string
		targetSession string
	}{
		{name: "same_session", relation: "same_session", sourceTopic: "runbooks/a", targetTopic: "runbooks/a", sourceSession: "session-a", targetSession: "session-b"},
		{name: "same_topic", relation: "same_topic", sourceTopic: "runbooks/a", targetTopic: "runbooks/b", sourceSession: "session-a", targetSession: "session-a"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, gateway := newMemoryGraphTestServer(t, true)
			defer gateway.Close()
			if _, _, err := s.memoryStore.put(normalizedWrite{
				project: "alpha", fileName: "notes/semantic-target.md", content: "target", topicPath: test.targetTopic, sessionID: test.targetSession,
			}); err != nil {
				t.Fatalf("seed target: %v", err)
			}
			_, _, err := s.memoryStore.put(normalizedWrite{
				project: "alpha", fileName: "notes/semantic-source.md", content: "source", topicPath: test.sourceTopic, sessionID: test.sourceSession,
				references: []memoryStructuredReference{{TargetID: "alpha::notes/semantic-target.md", Relation: test.relation, Confidence: 1}},
			})
			if err == nil {
				t.Fatalf("%s mismatch was accepted", test.relation)
			}
			if _, ok := s.memoryStore.currentEntry("alpha", "notes/semantic-source.md"); ok {
				t.Fatalf("%s mismatch promoted its source", test.relation)
			}
			if edges, listErr := s.memoryStore.listMemoryEdges(context.Background(), memoryEdgeQuery{MemoryID: "alpha::notes/semantic-source.md", Limit: 10}); listErr != nil || len(edges) != 0 {
				t.Fatalf("%s mismatch promoted graph edges: edges=%#v err=%v", test.relation, edges, listErr)
			}
		})
	}
}

func TestStructuredReferencePendingReconciliationUsesOneExactEdgeIndexGeneration(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	for index := 0; index < 2; index++ {
		if _, _, err := store.memoryStore.put(normalizedWrite{
			project: "alpha", fileName: fmt.Sprintf("notes/index-target-%d.md", index), content: "target", topicPath: "runbooks/index",
		}); err != nil {
			t.Fatalf("seed target %d: %v", index, err)
		}
	}
	store.memoryStore.beforeReferenceHistoryAppend = func() error { return errors.New("injected pre-history interruption") }
	for index := 0; index < 2; index++ {
		_, _, err := store.memoryStore.put(normalizedWrite{
			project: "alpha", fileName: fmt.Sprintf("notes/index-source-%d.md", index), content: "source", topicPath: "runbooks/index",
			references: []memoryStructuredReference{
				{TargetID: "alpha::notes/index-target-0.md", Relation: "references", Confidence: 1},
				{TargetID: "alpha::notes/index-target-1.md", Relation: "references", Confidence: 1},
			},
		})
		if err == nil {
			t.Fatalf("pending transaction %d did not stop at the injected boundary", index)
		}
	}
	store.memoryStore.beforeReferenceHistoryAppend = nil
	fullScans := 0
	store.memoryStore.memoryEdgeLogObserveIO = func(operation string, _ int64) {
		if operation == "full_scan_read" {
			fullScans++
		}
	}
	if err := store.memoryStore.reconcileMemoryReferenceTransactions(context.Background()); err != nil {
		t.Fatalf("reconcile pending transaction set: %v", err)
	}
	store.memoryStore.memoryEdgeLogObserveIO = nil
	if fullScans > 1 {
		t.Fatalf("pending set rescanned the edge log per transaction: full_scans=%d", fullScans)
	}
	for index := 0; index < 2; index++ {
		assertStructuredReferenceTransactionRecovered(t, store.memoryStore, fmt.Sprintf("alpha::notes/index-source-%d.md", index), 2)
	}
}

func TestStructuredReferenceClosedSetSurvivesBoundedStartupTailAsACompleteSet(t *testing.T) {
	s, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	for index := 0; index < 2; index++ {
		if _, _, err := s.memoryStore.put(normalizedWrite{
			project: "alpha", fileName: fmt.Sprintf("notes/tail-target-%d.md", index), content: "target", topicPath: "runbooks/tail",
		}); err != nil {
			t.Fatalf("seed target %d: %v", index, err)
		}
	}
	if _, _, err := s.memoryStore.put(normalizedWrite{
		project: "alpha", fileName: "notes/tail-source.md", content: "source", topicPath: "runbooks/tail",
		references: []memoryStructuredReference{
			{TargetID: "alpha::notes/tail-target-0.md", Relation: "references", Confidence: 1},
			{TargetID: "alpha::notes/tail-target-1.md", Relation: "references", Confidence: 1},
		},
	}); err != nil {
		t.Fatalf("write closed two-edge transaction: %v", err)
	}
	t.Setenv("GO_MEMORY_GRAPH_EDGE_STARTUP_MAX_LINES", "1")
	restarted, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("restart with one-row edge tail: %v", err)
	}
	assertStructuredReferenceTransactionRecovered(t, restarted, "alpha::notes/tail-source.md", 2)
}

func TestStructuredReferenceClosedCurrentSetRejectsMissingEdgeLog(t *testing.T) {
	s, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	if _, _, err := s.memoryStore.put(normalizedWrite{
		project: "alpha", fileName: "notes/missing-log-target.md", content: "target", topicPath: "runbooks/missing-log",
	}); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	if _, _, err := s.memoryStore.put(normalizedWrite{
		project: "alpha", fileName: "notes/missing-log-source.md", content: "source", topicPath: "runbooks/missing-log",
		references: []memoryStructuredReference{{TargetID: "alpha::notes/missing-log-target.md", Relation: "references", Confidence: 1}},
	}); err != nil {
		t.Fatalf("write closed reference transaction: %v", err)
	}
	if err := os.Remove(s.memoryStore.policy.edgePath); err != nil {
		t.Fatalf("remove edge log fault fixture: %v", err)
	}
	if err := s.memoryStore.loadEdges(); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("closed current reference set accepted a missing durable edge log: %v", err)
	}
	restarted, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("restart did not reconcile the closed set into the missing edge log: %v", err)
	}
	assertStructuredReferenceTransactionRecovered(t, restarted, "alpha::notes/missing-log-source.md", 1)
	if raw, err := os.ReadFile(restarted.policy.edgePath); err != nil || !strings.Contains(string(raw), "missing-log-target.md") {
		t.Fatalf("restart did not durably restore the missing edge row: content=%q err=%v", string(raw), err)
	}
}

func TestStructuredReferenceStartupReconciliationReusesCanonicalFence(t *testing.T) {
	s, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	if _, _, err := s.memoryStore.put(normalizedWrite{
		project: "alpha", fileName: "notes/fenced-target.md", content: "target", topicPath: "runbooks/fenced-startup",
	}); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	s.memoryStore.beforeReferenceHistoryAppend = func() error { return errors.New("injected pre-history interruption") }
	if _, _, err := s.memoryStore.put(normalizedWrite{
		project: "alpha", fileName: "notes/fenced-source.md", content: "source", topicPath: "runbooks/fenced-startup",
		references: []memoryStructuredReference{{TargetID: "alpha::notes/fenced-target.md", Relation: "references", Confidence: 1}},
	}); err == nil {
		t.Fatal("pending transaction did not stop at the injected boundary")
	}
	s.memoryStore.beforeReferenceHistoryAppend = nil
	fence, err := s.memoryStore.acquireMemoryEdgeLogFence()
	if err != nil {
		t.Fatalf("acquire canonical edge-log fence: %v", err)
	}
	defer fence.release()
	done := make(chan error, 1)
	go func() {
		if reconcileErr := s.memoryStore.reconcileMemoryReferenceTransactionsWithFence(context.Background(), fence); reconcileErr != nil {
			done <- reconcileErr
			return
		}
		done <- s.memoryStore.loadEdgesWithFenceLocked(fence)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("same-fence startup reconciliation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("same-fence startup reconciliation attempted to reacquire its canonical writer fence")
	}
	fence.release()
	assertStructuredReferenceTransactionRecovered(t, s.memoryStore, "alpha::notes/fenced-source.md", 1)
}

func TestStructuredReferenceProjectionReloadAndHydrationSuppressUnclosedAndStaleRows(t *testing.T) {
	t.Run("unclosed", func(t *testing.T) {
		s, gateway := newMemoryGraphTestServer(t, true)
		defer gateway.Close()
		if _, _, err := s.memoryStore.put(normalizedWrite{project: "alpha", fileName: "notes/unclosed-target.md", content: "target", topicPath: "runbooks/projection"}); err != nil {
			t.Fatalf("seed target: %v", err)
		}
		s.memoryStore.beforeReferenceReceiptClose = func() error { return errors.New("injected receipt interruption") }
		if _, _, err := s.memoryStore.put(normalizedWrite{
			project: "alpha", fileName: "notes/unclosed-source.md", content: "source", topicPath: "runbooks/projection",
			references: []memoryStructuredReference{{TargetID: "alpha::notes/unclosed-target.md", Relation: "references", Confidence: 1}},
		}); err == nil {
			t.Fatal("unclosed projection fixture did not stop before receipt close")
		}
		raw, err := os.ReadFile(s.memoryStore.policy.edgePath)
		if err != nil {
			t.Fatalf("read unclosed edge row: %v", err)
		}
		var edge memoryEdgeEntry
		if err := json.Unmarshal(bytes.TrimSpace(raw), &edge); err != nil || memoryReferenceTransactionIDFromEdge(edge) == "" {
			t.Fatalf("decode unclosed edge row: edge=%#v err=%v", edge, err)
		}
		s.memoryStore.invalidateMemoryEdges()
		if err := s.memoryStore.reloadMemoryEdgesFromRawLocked(raw); err != nil {
			t.Fatalf("reload unclosed edge log: %v", err)
		}
		if s.memoryStore.memoryEdgeExists(edge.EdgeID) {
			t.Fatal("projection reload surfaced an unclosed structured edge")
		}
		if err := s.memoryStore.hydrateMemoryEdgeProjection(edge); err != nil {
			t.Fatalf("hydrate unclosed edge: %v", err)
		}
		if s.memoryStore.memoryEdgeExists(edge.EdgeID) {
			t.Fatal("append recovery hydration surfaced an unclosed structured edge")
		}
	})

	t.Run("stale", func(t *testing.T) {
		s, gateway := newMemoryGraphTestServer(t, true)
		defer gateway.Close()
		if _, _, err := s.memoryStore.put(normalizedWrite{project: "alpha", fileName: "notes/stale-target.md", content: "target v1", topicPath: "runbooks/projection"}); err != nil {
			t.Fatalf("seed target: %v", err)
		}
		if _, _, err := s.memoryStore.put(normalizedWrite{
			project: "alpha", fileName: "notes/stale-source.md", content: "source", topicPath: "runbooks/projection",
			references: []memoryStructuredReference{{TargetID: "alpha::notes/stale-target.md", Relation: "references", Confidence: 1}},
		}); err != nil {
			t.Fatalf("write closed reference: %v", err)
		}
		edges, err := s.memoryStore.listMemoryEdges(context.Background(), memoryEdgeQuery{MemoryID: "alpha::notes/stale-source.md", Relation: "references", Limit: 10})
		if err != nil || len(edges) != 1 {
			t.Fatalf("load current reference fixture: edges=%#v err=%v", edges, err)
		}
		stale := edges[0]
		if _, _, err := s.memoryStore.put(normalizedWrite{project: "alpha", fileName: "notes/stale-target.md", content: "target v2", topicPath: "runbooks/projection"}); err != nil {
			t.Fatalf("advance target: %v", err)
		}
		raw, err := os.ReadFile(s.memoryStore.policy.edgePath)
		if err != nil {
			t.Fatalf("read stale edge log: %v", err)
		}
		s.memoryStore.invalidateMemoryEdges()
		if err := s.memoryStore.reloadMemoryEdgesFromRawLocked(raw); err != nil {
			t.Fatalf("reload stale edge log: %v", err)
		}
		if s.memoryStore.memoryEdgeExists(stale.EdgeID) {
			t.Fatal("projection reload surfaced a stale structured binding")
		}
		if err := s.memoryStore.hydrateMemoryEdgeProjection(stale); err != nil {
			t.Fatalf("hydrate stale edge: %v", err)
		}
		if s.memoryStore.memoryEdgeExists(stale.EdgeID) {
			t.Fatal("append recovery hydration surfaced a stale structured binding")
		}
	})
}

func TestStructuredReferenceWritePreservesTaskAttributionAndCanonicalGeneration(t *testing.T) {
	s, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	if _, _, err := s.memoryStore.put(normalizedWrite{project: "alpha", fileName: "notes/context-target.md", content: "target", topicPath: "runbooks/context"}); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	s.memoryStore.mu.RLock()
	beforeGeneration := s.memoryStore.currentKeyIndexGeneration["alpha"]
	s.memoryStore.mu.RUnlock()
	write := normalizedWrite{
		project: "alpha", fileName: "notes/context-source.md", content: "source", topicPath: "runbooks/context",
		taskAttribution: map[string]any{"task_id": "task-one", "worker": "runner-a"},
		references:      []memoryStructuredReference{{TargetID: "alpha::notes/context-target.md", Relation: "references", Confidence: 1}},
	}
	first, _, err := s.memoryStore.put(write)
	if err != nil {
		t.Fatalf("write attributed structured reference: %v", err)
	}
	if anyToString(first.TaskAttribution["task_id"]) != "task-one" || len(first.References) != 1 {
		t.Fatalf("structured write dropped task attribution or references: %#v", first)
	}
	s.memoryStore.mu.RLock()
	afterGeneration := s.memoryStore.currentKeyIndexGeneration["alpha"]
	generationRecord := s.memoryStore.currentStateGenerationRecords["alpha"]
	s.memoryStore.mu.RUnlock()
	if afterGeneration <= beforeGeneration || generationRecord.KeyGeneration != afterGeneration || generationRecord.TopicGeneration != afterGeneration || !memoryCurrentStateGenerationDigestValid(generationRecord.StateDigest) {
		t.Fatalf("structured write bypassed canonical generation custody: before=%d after=%d record=%#v", beforeGeneration, afterGeneration, generationRecord)
	}
	edges, err := s.memoryStore.listMemoryEdges(context.Background(), memoryEdgeQuery{MemoryID: "alpha::notes/context-source.md", Relation: "references", Limit: 10})
	if err != nil || len(edges) != 1 || edges[0].Binding == nil || edges[0].Binding.SourceIndexGeneration != afterGeneration {
		t.Fatalf("structured binding did not match canonical generation: edges=%#v err=%v", edges, err)
	}
	retry, deduped, err := s.memoryStore.put(write)
	if err != nil || !deduped || retry.EventID != first.EventID {
		t.Fatalf("exact attributed retry was not idempotent: first=%s retry=%s deduped=%v err=%v", first.EventID, retry.EventID, deduped, err)
	}
	write.taskAttribution = map[string]any{"task_id": "task-two", "worker": "runner-b"}
	updated, _, err := s.memoryStore.put(write)
	if err != nil {
		t.Fatalf("update structured task attribution: %v", err)
	}
	if updated.EventID == first.EventID || anyToString(updated.TaskAttribution["task_id"]) != "task-two" {
		t.Fatalf("task attribution did not participate in structured transaction identity: first=%#v updated=%#v", first.TaskAttribution, updated.TaskAttribution)
	}
	restarted, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("restart canonical structured current state: %v", err)
	}
	current, ok := restarted.currentEntry("alpha", "notes/context-source.md")
	if !ok || anyToString(current.TaskAttribution["task_id"]) != "task-two" || len(current.References) != 1 {
		t.Fatalf("restart lost structured task/reference state: current=%#v ok=%v", current, ok)
	}
	assertStructuredReferenceTransactionRecovered(t, restarted, "alpha::notes/context-source.md", 1)
}

func TestStructuredReferenceUnclosedHistoryNeverPromotesAfterTargetSnapshotTurnsStale(t *testing.T) {
	s, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	if _, _, err := s.memoryStore.put(normalizedWrite{project: "alpha", fileName: "notes/target.md", content: "target v1", topicPath: "runbooks/transactions"}); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	write := normalizedWrite{
		project: "alpha", fileName: "notes/source.md", content: "source", topicPath: "runbooks/transactions",
		references: []memoryStructuredReference{{TargetID: "alpha::notes/target.md", Relation: "references", Confidence: 1}},
	}
	s.memoryStore.beforeReferenceReceiptClose = func() error { return errors.New("injected receipt failure") }
	if _, _, err := s.memoryStore.put(write); err == nil {
		t.Fatal("receipt failure must interrupt the source transaction")
	}
	s.memoryStore.beforeReferenceReceiptClose = nil
	if _, _, err := s.memoryStore.put(normalizedWrite{project: "alpha", fileName: "notes/target.md", content: "target v2", topicPath: "runbooks/transactions"}); err != nil {
		t.Fatalf("advance target snapshot: %v", err)
	}
	restarted, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("restart with stale pending transaction: %v", err)
	}
	if _, ok := restarted.currentEntry("alpha", "notes/source.md"); ok {
		t.Fatal("unclosed transaction history became current after its target snapshot turned stale")
	}
	if edges, err := restarted.listMemoryEdges(context.Background(), memoryEdgeQuery{MemoryID: "alpha::notes/source.md", Limit: 10}); err != nil || len(edges) != 0 {
		t.Fatalf("unclosed stale transaction edges became visible: edges=%#v err=%v", edges, err)
	}
	if _, _, err := restarted.put(write); err != nil {
		t.Fatalf("fresh retry against the new target snapshot: %v", err)
	}
	assertStructuredReferenceTransactionRecovered(t, restarted, "alpha::notes/source.md", 1)
}

func TestStructuredReferenceContinuationIsOpaqueBoundOneUseAndGapFree(t *testing.T) {
	s, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	if _, _, err := s.memoryStore.put(normalizedWrite{project: "alpha", fileName: "notes/target.md", content: "target", topicPath: "runbooks/cursor"}); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	if _, _, err := s.memoryStore.put(normalizedWrite{project: "alpha", fileName: "notes/target-extra.md", content: "target extra", topicPath: "runbooks/cursor"}); err != nil {
		t.Fatalf("seed extra target: %v", err)
	}
	for index := 0; index < 3; index++ {
		references := []memoryStructuredReference{{TargetID: "alpha::notes/target.md", Relation: "references", Confidence: 1}}
		if index == 0 {
			references = append(references, memoryStructuredReference{TargetID: "alpha::notes/target-extra.md", Relation: "references", Confidence: 1})
		}
		if _, _, err := s.memoryStore.put(normalizedWrite{
			project: "alpha", fileName: fmt.Sprintf("notes/source-%d.md", index), content: fmt.Sprintf("source %d", index), topicPath: "runbooks/cursor",
			references: references,
		}); err != nil {
			t.Fatalf("seed source %d: %v", index, err)
		}
	}
	request := map[string]any{
		"project": "alpha", "relations": []any{"references"}, "max_candidates": 1, "max_writes": 1,
		"sample_limit": 10, "include_low_confidence_audit": false,
	}
	status, first := postStructuredReferenceBackfillStatus(t, gateway.URL, request)
	if status != http.StatusOK {
		t.Fatalf("first cursor page: status=%d payload=%#v", status, first)
	}
	firstPopulation := anyMap(first["reference_population"])
	token := anyToString(firstPopulation["continuation_next"])
	if len(token) < 32 || strings.Count(token, ".") != 1 || anyToBool(firstPopulation["continuation_complete"]) {
		t.Fatalf("expected opaque continuation token for incomplete page: %#v", firstPopulation)
	}
	tokenParts := strings.Split(token, ".")
	payloadRaw, err := base64.RawURLEncoding.DecodeString(tokenParts[0])
	if err != nil {
		t.Fatalf("decode signed cursor payload for reservation proof: %v", err)
	}
	var expectedCursor memoryReferenceCursorPayload
	if err := json.Unmarshal(payloadRaw, &expectedCursor); err != nil {
		t.Fatalf("decode cursor payload for reservation proof: %v", err)
	}
	reserved, reservation, err := s.memoryStore.decodeAndReserveMemoryReferenceCursor(token, expectedCursor)
	if err != nil {
		t.Fatalf("reserve cursor for interrupted-attempt proof: %v", err)
	}
	if _, _, err := s.memoryStore.decodeAndReserveMemoryReferenceCursor(token, expectedCursor); err == nil {
		t.Fatal("concurrent cursor reservation was not fenced")
	}
	if err := s.memoryStore.finishMemoryReferenceCursor(token, reserved.CursorID, reservation, false); err != nil {
		t.Fatalf("release cursor after interrupted-attempt proof: %v", err)
	}

	tampered := token[:len(token)-1] + map[bool]string{true: "A", false: "B"}[token[len(token)-1] != 'A']
	tamperedRequest := cloneJSONMap(request)
	tamperedRequest["reference_continuation"] = tampered
	if status, payload := postStructuredReferenceBackfillStatus(t, gateway.URL, tamperedRequest); status != http.StatusConflict {
		t.Fatalf("tampered cursor must fail closed: status=%d payload=%#v", status, payload)
	}

	mismatchRequest := cloneJSONMap(request)
	mismatchRequest["max_candidates"] = 2
	mismatchRequest["reference_continuation"] = token
	if status, payload := postStructuredReferenceBackfillStatus(t, gateway.URL, mismatchRequest); status != http.StatusConflict {
		t.Fatalf("request-mismatched cursor must fail closed: status=%d payload=%#v", status, payload)
	}

	pageRequest := cloneJSONMap(request)
	pageRequest["reference_continuation"] = token
	status, second := postStructuredReferenceBackfillStatus(t, gateway.URL, pageRequest)
	if status != http.StatusOK {
		t.Fatalf("second cursor page: status=%d payload=%#v", status, second)
	}
	if status, payload := postStructuredReferenceBackfillStatus(t, gateway.URL, pageRequest); status != http.StatusConflict {
		t.Fatalf("cursor replay must fail closed: status=%d payload=%#v", status, payload)
	}

	seen := map[string]struct{}{}
	for _, page := range []map[string]any{first, second} {
		for _, rawSample := range structuredReferenceSamples(page["samples"]) {
			sample := anyMap(rawSample)
			if anyToString(sample["relation"]) == "references" {
				seen[anyToString(sample["edge_id"])] = struct{}{}
			}
		}
	}
	continuation := anyToString(anyMap(second["reference_population"])["continuation_next"])
	for pageIndex := 0; continuation != "" && pageIndex < 10; pageIndex++ {
		nextRequest := cloneJSONMap(request)
		nextRequest["reference_continuation"] = continuation
		status, page := postStructuredReferenceBackfillStatus(t, gateway.URL, nextRequest)
		if status != http.StatusOK {
			t.Fatalf("continued cursor page: status=%d payload=%#v", status, page)
		}
		for _, rawSample := range structuredReferenceSamples(page["samples"]) {
			sample := anyMap(rawSample)
			if anyToString(sample["relation"]) == "references" {
				seen[anyToString(sample["edge_id"])] = struct{}{}
			}
		}
		continuation = anyToString(anyMap(page["reference_population"])["continuation_next"])
	}
	if continuation != "" {
		t.Fatalf("reference continuation did not converge within the bounded page count")
	}
	if len(seen) != 4 {
		t.Fatalf("continuation skipped or duplicated reference documents: unique_edges=%d edges=%#v", len(seen), seen)
	}

	status, staleStart := postStructuredReferenceBackfillStatus(t, gateway.URL, request)
	if status != http.StatusOK {
		t.Fatalf("stale cursor setup: status=%d payload=%#v", status, staleStart)
	}
	staleToken := anyToString(anyMap(staleStart["reference_population"])["continuation_next"])
	if _, _, err := s.memoryStore.put(normalizedWrite{project: "alpha", fileName: "notes/snapshot-change.md", content: "changed", topicPath: "runbooks/cursor"}); err != nil {
		t.Fatalf("mutate current snapshot: %v", err)
	}
	staleRequest := cloneJSONMap(request)
	staleRequest["reference_continuation"] = staleToken
	if status, payload := postStructuredReferenceBackfillStatus(t, gateway.URL, staleRequest); status != http.StatusConflict {
		t.Fatalf("stale snapshot cursor must fail closed: status=%d payload=%#v", status, payload)
	}
}

func TestStructuredReferenceCursorStateHasHardCapacity(t *testing.T) {
	s, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	now := time.Now().UTC()
	entries := make([]memoryReferenceCursorState, memoryReferenceCursorMaxState)
	for index := range entries {
		entries[index] = memoryReferenceCursorState{
			CursorID: fmt.Sprintf("cursor-capacity-%04d", index), TokenDigest: "sha256:" + strings.Repeat("a", 64),
			IssuedAt: now.Add(time.Duration(index) * time.Nanosecond).Format(time.RFC3339Nano), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339Nano),
		}
	}
	raw, err := json.Marshal(memoryReferenceCursorStateFile{
		SchemaID: "contextlattice_memory_reference_cursor_state.v1", Version: 1, Entries: entries,
	})
	if err != nil {
		t.Fatalf("encode full cursor state: %v", err)
	}
	if err := writeOwnerOnlyDurableAtomicFile(s.memoryStore.memoryReferenceCursorStatePath(), append(raw, '\n'), true); err != nil {
		t.Fatalf("persist full cursor state: %v", err)
	}
	payload := memoryReferenceCursorPayload{
		Version: 1, CursorID: "cursor-over-capacity", RequestDigest: "sha256:" + strings.Repeat("b", 64),
		Project: "alpha", Corpus: "history_index", RelationDigest: "sha256:" + strings.Repeat("c", 64),
		SnapshotDigest: "sha256:" + strings.Repeat("d", 64), GenerationDigest: "sha256:" + strings.Repeat("e", 64), DocSetDigest: "sha256:" + strings.Repeat("f", 64),
		LastDocKey: "alpha::notes/a.md", IssuedAt: now.Format(time.RFC3339Nano), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339Nano),
	}
	if _, err := s.memoryStore.encodeMemoryReferenceCursor(payload); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("cursor state exceeded its hard capacity: %v", err)
	}
}

func TestStructuredReferenceCursorStateRejectsOversizedSparseFile(t *testing.T) {
	s, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	path := s.memoryStore.memoryReferenceCursorStatePath()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("create oversized cursor state: %v", err)
	}
	if err := file.Truncate(memoryReferenceCursorStateMaxBytes + 1); err != nil {
		_ = file.Close()
		t.Fatalf("size oversized cursor state: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close oversized cursor state: %v", err)
	}
	if err := s.memoryStore.withMemoryReferenceCursorState(func(map[string]memoryReferenceCursorState) error { return nil }); err == nil {
		t.Fatal("oversized cursor state was read without its hard byte bound")
	}
}

func TestStructuredReferenceWriteAndBackfillCaptureOneBoundedCancellableSnapshot(t *testing.T) {
	s, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	claims := make([]memoryStructuredReference, 0, memoryStructuredReferenceMaxClaims)
	for index := 0; index < memoryStructuredReferenceMaxClaims; index++ {
		fileName := fmt.Sprintf("notes/target-%02d.md", index)
		if _, _, err := s.memoryStore.put(normalizedWrite{project: "alpha", fileName: fileName, content: "target", topicPath: "runbooks/snapshot"}); err != nil {
			t.Fatalf("seed target %d: %v", index, err)
		}
		claims = append(claims, memoryStructuredReference{TargetID: "alpha::" + fileName, Relation: "references", Confidence: 1})
	}
	var countMu sync.Mutex
	captures := 0
	s.memoryStore.referenceSnapshotCaptured = func(_ int) {
		countMu.Lock()
		captures++
		countMu.Unlock()
	}
	if _, _, err := s.memoryStore.put(normalizedWrite{
		project: "alpha", fileName: "notes/source.md", content: "source", topicPath: "runbooks/snapshot", references: claims,
	}); err != nil {
		t.Fatalf("structured write: %v", err)
	}
	countMu.Lock()
	writeCaptures := captures
	captures = 0
	countMu.Unlock()
	if writeCaptures != 1 {
		t.Fatalf("structured write captured %d snapshots for %d claims; want exactly one", writeCaptures, len(claims))
	}
	status, report := postStructuredReferenceBackfillStatus(t, gateway.URL, map[string]any{
		"project": "alpha", "relations": []any{"references"}, "max_candidates": 1000,
	})
	if status != http.StatusOK {
		t.Fatalf("reference backfill: status=%d report=%#v", status, report)
	}
	countMu.Lock()
	backfillCaptures := captures
	countMu.Unlock()
	if backfillCaptures != 1 {
		t.Fatalf("reference backfill captured %d snapshots; want exactly one", backfillCaptures)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.memoryStore.captureMemoryReferenceSnapshot(canceled, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled snapshot must stop with context cancellation, got %v", err)
	}
	previousLimit := s.memoryStore.policy.scanLimit
	s.memoryStore.policy.scanLimit = 1
	if _, err := s.memoryStore.captureMemoryReferenceSnapshot(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "bounded document cap") {
		t.Fatalf("oversized snapshot must fail its document cap, got %v", err)
	}
	s.memoryStore.policy.scanLimit = previousLimit
}

func TestStructuredReferenceBlobReadRejectsSymlinkOversizeAndDigestMismatch(t *testing.T) {
	s, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	entry, _, err := s.memoryStore.put(normalizedWrite{project: "alpha", fileName: "notes/blob.md", content: "descriptor-bound", topicPath: "runbooks/blob"})
	if err != nil {
		t.Fatalf("seed blob: %v", err)
	}
	blobPath, err := s.memoryStore.blobPathForHash(entry.ContentHash)
	if err != nil {
		t.Fatalf("resolve blob path: %v", err)
	}
	if content, _, err := s.memoryStore.readReferenceContentBlob(context.Background(), entry.ContentRef, memoryReferenceBackfillMaxBlobBytes); err != nil || strings.TrimSpace(content) != "descriptor-bound" {
		t.Fatalf("read descriptor-bound blob: content=%q err=%v", content, err)
	}
	externalPath := filepath.Join(filepath.Dir(blobPath), "symlink-target.txt")
	if err := os.WriteFile(externalPath, []byte("descriptor-bound\n"), 0o600); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	if err := os.Remove(blobPath); err != nil {
		t.Fatalf("remove seeded blob: %v", err)
	}
	if err := os.Symlink(externalPath, blobPath); err != nil {
		t.Skipf("platform cannot create a test symlink: %v", err)
	}
	if _, _, err := s.memoryStore.readReferenceContentBlob(context.Background(), entry.ContentRef, memoryReferenceBackfillMaxBlobBytes); err == nil {
		t.Fatal("descriptor-bound blob reader followed a symlink")
	}
	if err := os.Remove(blobPath); err != nil {
		t.Fatalf("remove test symlink: %v", err)
	}
	if err := os.WriteFile(blobPath, []byte(strings.Repeat("x", 128)), 0o600); err != nil {
		t.Fatalf("write oversized blob: %v", err)
	}
	if _, _, err := s.memoryStore.readReferenceContentBlob(context.Background(), entry.ContentRef, 16); err == nil {
		t.Fatal("descriptor-bound blob reader accepted an oversized file")
	}
	wrongDigest := strings.Repeat("z", len("descriptor-bound\n"))
	if err := os.WriteFile(blobPath, []byte(wrongDigest), 0o600); err != nil {
		t.Fatalf("write digest-mismatched blob: %v", err)
	}
	if _, _, err := s.memoryStore.readReferenceContentBlob(context.Background(), entry.ContentRef, memoryReferenceBackfillMaxBlobBytes); err == nil {
		t.Fatal("descriptor-bound blob reader accepted a digest mismatch")
	}
}

func TestStructuredReferenceBlobDescriptorDoesNotFollowPathReplacement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "blob")
	preserved := filepath.Join(root, "opened-blob")
	external := filepath.Join(root, "replacement")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatalf("write original blob: %v", err)
	}
	if err := os.WriteFile(external, []byte("replacement"), 0o600); err != nil {
		t.Fatalf("write replacement blob: %v", err)
	}
	file, size, err := openBoundedRegularFileNoFollow(path, 64)
	if err != nil {
		t.Fatalf("open descriptor-bound blob: %v", err)
	}
	defer file.Close()
	if err := os.Rename(path, preserved); err != nil {
		t.Fatalf("move opened blob: %v", err)
	}
	if err := os.Symlink(external, path); err != nil {
		t.Skipf("platform cannot create a test symlink: %v", err)
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("read opened descriptor after path replacement: %v", err)
	}
	if size != int64(len("original")) || string(raw) != "original" {
		t.Fatalf("path replacement redirected the opened descriptor: size=%d content=%q", size, string(raw))
	}
}

func TestPromotedGraphRelationsRevalidateExactCurrentEndpointsForEveryConsumer(t *testing.T) {
	s, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	seedPair := func(project string) {
		t.Helper()
		for _, fileName := range []string{"notes/source.md", "notes/target.md"} {
			if _, _, err := s.memoryStore.put(normalizedWrite{
				project: project, fileName: fileName, content: project + " " + fileName + " shared semantic tokens", topicPath: "runbooks/promoted",
				agentID: "agent-a", sessionID: "session-a",
			}); err != nil {
				t.Fatalf("seed %s::%s: %v", project, fileName, err)
			}
		}
		status, report := postStructuredReferenceBackfillStatus(t, gateway.URL, map[string]any{
			"project": project, "dry_run": false, "relations": []any{"same_session", "same_topic"}, "max_candidates": 100, "max_writes": 100,
		})
		if status != http.StatusOK || anyToInt(report["written"], 0) != 2 {
			t.Fatalf("promote exact relations for %s: status=%d report=%#v", project, status, report)
		}
	}
	seedPair("alpha")
	alphaEdges, err := s.memoryStore.listMemoryEdges(context.Background(), memoryEdgeQuery{MemoryID: "alpha::notes/source.md", Limit: 10})
	if err != nil || len(alphaEdges) != 2 {
		t.Fatalf("list promoted alpha edges: edges=%#v err=%v", alphaEdges, err)
	}
	for _, edge := range alphaEdges {
		if edge.Binding == nil || edge.Binding.RelationSemantic != edge.Relation || edge.Binding.SourceEventID == "" || edge.Binding.TargetEventID == "" || edge.Binding.SemanticDigest == "" {
			t.Fatalf("promoted relation lacks exact endpoint binding: %#v", edge)
		}
	}
	if _, _, err := s.memoryStore.put(normalizedWrite{
		project: "alpha", fileName: "notes/source.md", content: "updated endpoint", topicPath: "runbooks/promoted", agentID: "agent-a", sessionID: "session-b",
	}); err != nil {
		t.Fatalf("update promoted endpoint: %v", err)
	}
	if edges, err := s.memoryStore.listMemoryEdges(context.Background(), memoryEdgeQuery{MemoryID: "alpha::notes/source.md", Limit: 10}); err != nil || len(edges) != 0 {
		t.Fatalf("endpoint update did not suppress stale promoted edges: edges=%#v err=%v", edges, err)
	}
	telemetry, err := s.memoryStore.memoryGraphTelemetrySnapshot(context.Background(), "alpha", false, 10, time.Time{})
	if err != nil || telemetry.BoundEdgeCount != 0 || telemetry.ConnectedDocCount != 0 || len(telemetry.Relations) != 0 {
		t.Fatalf("graph telemetry promoted stale edges into current topology: telemetry=%#v err=%v", telemetry, err)
	}

	seedPair("beta")
	target, ok := s.memoryStore.currentEntry("beta", "notes/target.md")
	if !ok {
		t.Fatal("beta target missing before tombstone")
	}
	tombstone := memoryStoreEntry{
		EventID: bson.NewObjectID().Hex(), Project: target.Project, FileName: target.FileName, TopicPath: target.TopicPath,
		DataClass: "memory_tombstone", Lifecycle: target.Lifecycle, CreatedAt: time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano), Source: "test",
	}
	if err := s.memoryStore.appendHistory(tombstone); err != nil {
		t.Fatalf("append target tombstone: %v", err)
	}
	if err := s.memoryStore.persistAndRecordEntry(tombstone); err != nil {
		t.Fatalf("persist target tombstone: %v", err)
	}
	if edges, err := s.memoryStore.listMemoryEdges(context.Background(), memoryEdgeQuery{MemoryID: "beta::notes/source.md", Limit: 10}); err != nil || len(edges) != 0 {
		t.Fatalf("endpoint tombstone did not suppress stale promoted edges: edges=%#v err=%v", edges, err)
	}
	telemetry, err = s.memoryStore.memoryGraphTelemetrySnapshot(context.Background(), "beta", false, 10, time.Time{})
	if err != nil || telemetry.BoundEdgeCount != 0 || telemetry.ConnectedDocCount != 0 || len(telemetry.Relations) != 0 {
		t.Fatalf("graph telemetry promoted tombstoned edges into current topology: telemetry=%#v err=%v", telemetry, err)
	}
}

func TestGraphRepairPlanBindsPromotedRelationsToItsExactCurrentSnapshot(t *testing.T) {
	server, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	seedMemoryGraphRepairDoc(t, server.memoryStore, "alpha", "notes/a.md", "runbooks/repair-binding", "shared repair binding", "agent-a", "session-a")
	seedMemoryGraphRepairDoc(t, server.memoryStore, "alpha", "notes/b.md", "runbooks/repair-binding", "shared repair binding", "agent-a", "session-a")
	req := repairRequest(t, server.memoryStore, map[string]any{
		"project": "alpha", "dry_run": true, "include_inferred": false, "topic_peer_limit": 1,
	})
	snapshot, err := server.memoryStore.captureMemoryGraphRepairSnapshot(context.Background(), req)
	if err != nil {
		t.Fatalf("capture repair snapshot: %v", err)
	}
	evidence, err := server.memoryStore.captureMemoryGraphRepairEdges(snapshot)
	if err != nil {
		t.Fatalf("capture repair evidence: %v", err)
	}
	actions, _, err := server.memoryStore.buildMemoryGraphRepairPlan(context.Background(), snapshot, evidence, req)
	if err != nil {
		t.Fatalf("build repair plan: %v", err)
	}
	bound := []memoryEdgeEntry{}
	for _, action := range actions {
		if action.Kind == "write" && memoryGraphRelationRequiresBinding(action.Edge.Relation) {
			if !memoryReferenceBindingValid(action.Edge.Binding) || !server.memoryStore.referenceEdgeCurrent(action.Edge) {
				t.Fatalf("repair promoted an edge without exact current semantics: %#v", action.Edge)
			}
			bound = append(bound, action.Edge)
		}
	}
	if len(bound) == 0 {
		t.Fatal("repair fixture produced no promoted relation to validate")
	}
	if _, _, err := server.memoryStore.put(normalizedWrite{
		project: "alpha", fileName: "notes/b.md", content: "updated repair endpoint", topicPath: "runbooks/repair-binding", agentID: "agent-a", sessionID: "session-a",
	}); err != nil {
		t.Fatalf("update repair endpoint: %v", err)
	}
	for _, edge := range bound {
		if server.memoryStore.referenceEdgeCurrent(edge) {
			t.Fatalf("repair binding survived an endpoint event/content update: %#v", edge)
		}
	}
}

func TestNormalizeMemoryStructuredReferencesRejectsAmbiguousClaims(t *testing.T) {
	tests := []struct {
		name    string
		payload map[string]any
	}{
		{name: "relative target", payload: map[string]any{"references": []any{"notes/target.md"}}},
		{name: "alias conflict", payload: map[string]any{"references": []any{map[string]any{"target_id": "alpha::notes/a.md", "target": "alpha::notes/b.md"}}}},
		{name: "relation missing", payload: map[string]any{"relations": []any{map[string]any{"target": "alpha::notes/a.md"}}}},
		{name: "too many", payload: map[string]any{"references": make([]any, memoryStructuredReferenceMaxClaims+1)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := normalizeMemoryStructuredReferences(test.payload); err == nil {
				t.Fatalf("expected malformed claim to be rejected")
			}
		})
	}
	if claims, err := normalizeMemoryStructuredReferences(map[string]any{"references": []any{}}); err != nil || len(claims) != 0 {
		t.Fatalf("an explicit empty reference list must clear prior claims, claims=%#v err=%v", claims, err)
	}
}

func TestStructuredReferenceWriteBindsAndRevalidatesCurrentState(t *testing.T) {
	s, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	target, _, err := s.memoryStore.put(normalizedWrite{
		project: "alpha", fileName: "notes/target.md", content: "target v1", topicPath: "runbooks/refs",
	})
	if err != nil {
		t.Fatalf("target write: %v", err)
	}
	source, _, err := s.memoryStore.put(normalizedWrite{
		project: "alpha", fileName: "notes/source.md", content: "source", topicPath: "runbooks/refs",
		agentID: "agent-a", sessionID: "session-a",
		references: []memoryStructuredReference{{TargetID: "alpha::notes/target.md", Relation: "references", Confidence: 1}},
	})
	if err != nil {
		t.Fatalf("source write: %v", err)
	}
	edges, err := s.memoryStore.listMemoryEdges(context.Background(), memoryEdgeQuery{MemoryID: "alpha::notes/source.md", Relation: "references", Limit: 10})
	if err != nil || len(edges) != 1 {
		t.Fatalf("expected one bound reference edge, edges=%#v err=%v", edges, err)
	}
	edge := edges[0]
	if edge.Binding == nil || edge.Binding.SourceEventID != source.EventID || edge.Binding.TargetEventID != target.EventID {
		t.Fatalf("reference binding missing event custody: %#v", edge.Binding)
	}
	if edge.Binding.SourceContentHash != source.ContentHash || edge.Binding.TargetContentHash != target.ContentHash || edge.Binding.DocSetDigest == "" || edge.Binding.ExclusionPolicyDigest == "" {
		t.Fatalf("reference binding missing content/index policy custody: %#v", edge.Binding)
	}
	if anyToString(edge.Metadata["claim_kind"]) != "structured_write" {
		t.Fatalf("expected structured claim provenance, got %#v", edge.Metadata)
	}
	if _, _, err := s.memoryStore.put(normalizedWrite{
		project: "alpha", fileName: "notes/unrelated.md", content: "unrelated", topicPath: "runbooks/refs",
	}); err != nil {
		t.Fatalf("unrelated write: %v", err)
	}
	if edges, err := s.memoryStore.listMemoryEdges(context.Background(), memoryEdgeQuery{MemoryID: "alpha::notes/source.md", Relation: "references", Limit: 10}); err != nil || len(edges) != 1 {
		t.Fatalf("unrelated current-state additions must not invalidate the reference, edges=%#v err=%v", edges, err)
	}

	updatedTarget, _, err := s.memoryStore.put(normalizedWrite{
		project: "alpha", fileName: "notes/target.md", content: "target v2", topicPath: "runbooks/refs",
	})
	if err != nil {
		t.Fatalf("target update: %v", err)
	}
	if edges, err := s.memoryStore.listMemoryEdges(context.Background(), memoryEdgeQuery{MemoryID: "alpha::notes/source.md", Relation: "references", Limit: 10}); err != nil || len(edges) != 0 {
		t.Fatalf("target content change must invalidate old binding, edges=%#v err=%v", edges, err)
	}

	_, _, err = s.memoryStore.put(normalizedWrite{
		project: "alpha", fileName: "notes/source.md", content: "source v2", topicPath: "runbooks/refs",
		agentID: "agent-a", sessionID: "session-a",
		references: []memoryStructuredReference{{TargetID: "alpha::notes/target.md", Relation: "references", Confidence: 1}},
	})
	if err != nil {
		t.Fatalf("source rebind: %v", err)
	}
	edges, err = s.memoryStore.listMemoryEdges(context.Background(), memoryEdgeQuery{MemoryID: "alpha::notes/source.md", Relation: "references", Limit: 10})
	if err != nil || len(edges) != 1 || edges[0].Binding.TargetContentHash != updatedTarget.ContentHash {
		t.Fatalf("expected updated target binding after source rewrite, edges=%#v err=%v", edges, err)
	}

	if _, err := newMemoryStoreFromEnv(); err != nil {
		t.Fatalf("restart reload: %v", err)
	}
}

func TestStructuredReferenceWriteRejectsUnresolvedAndSelfClaims(t *testing.T) {
	s, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	for _, claim := range []memoryStructuredReference{
		{TargetID: "alpha::notes/missing.md", Relation: "references", Confidence: 1},
		{TargetID: "alpha::notes/source.md", Relation: "references", Confidence: 1},
	} {
		_, _, err := s.memoryStore.put(normalizedWrite{
			project: "alpha", fileName: "notes/source.md", content: "source", topicPath: "runbooks/refs", references: []memoryStructuredReference{claim},
		})
		if err == nil {
			t.Fatalf("expected claim %q to fail", claim.TargetID)
		}
		if _, ok := s.memoryStore.currentEntry("alpha", "notes/source.md"); ok {
			t.Fatalf("rejected claim must not persist source entry")
		}
	}
	_ = gateway
}

func TestStructuredReferenceBackfillUsesOnlyExplicitClaimsAndWritesAudit(t *testing.T) {
	s, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	if _, _, err := s.memoryStore.put(normalizedWrite{project: "alpha", fileName: "notes/target.md", content: "target", topicPath: "runbooks/refs"}); err != nil {
		t.Fatalf("target write: %v", err)
	}
	if _, _, err := s.memoryStore.put(normalizedWrite{project: "alpha", fileName: "notes/source.md", content: "explicit alpha::notes/target.md", topicPath: "runbooks/refs"}); err != nil {
		t.Fatalf("source write: %v", err)
	}
	dryRun := postEdgeBackfillForTest(t, gateway.URL, `{"dry_run":true,"project":"alpha","relations":["references"],"sample_limit":20}`)
	if anyToInt(dryRun["written"], -1) != 0 || anyToInt(dryRun["eligible"], 0) == 0 {
		t.Fatalf("expected bounded textual reference dry run, got %#v", dryRun)
	}
	if backfillRelationStatIntMap(dryRun, "same_topic", "generated") != 0 {
		t.Fatalf("topic adjacency must not be classified as references: %#v", dryRun)
	}
	ledger := anyMap(dryRun["audit_ledger"])
	if !anyToBool(ledger["closed"]) || anyToString(ledger["run_id"]) == "" {
		t.Fatalf("expected closed audit ledger, got %#v", ledger)
	}
	if _, err := os.Stat(filepath.Join(s.memoryStore.policy.rootPath, "_contextlattice", "memory_reference_backfill_ledger.ndjson")); err != nil {
		t.Fatalf("expected durable audit ledger: %v", err)
	}
	writeRun := postEdgeBackfillForTest(t, gateway.URL, `{"dry_run":false,"project":"alpha","relations":["references"],"sample_limit":20}`)
	if anyToInt(writeRun["written"], 0) != 1 {
		t.Fatalf("expected one explicit textual reference write, got %#v", writeRun)
	}
	edges, err := s.memoryStore.listMemoryEdges(context.Background(), memoryEdgeQuery{Relation: "references", Limit: 10})
	if err != nil || len(edges) != 1 || !memoryReferenceBindingValid(edges[0].Binding) {
		t.Fatalf("expected current-state-bound textual reference, edges=%#v err=%v", edges, err)
	}
}

func TestStructuredReferenceContentBackfillRevalidatesBoundedBlob(t *testing.T) {
	s, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	if _, _, err := s.memoryStore.put(normalizedWrite{project: "alpha", fileName: "notes/blob-target.md", content: "target", topicPath: "runbooks/refs"}); err != nil {
		t.Fatalf("target write: %v", err)
	}
	content := strings.Repeat("summary prefix ", 400) + " alpha::notes/blob-target.md"
	if _, _, err := s.memoryStore.put(normalizedWrite{project: "alpha", fileName: "notes/blob-source.md", content: content, topicPath: "runbooks/refs"}); err != nil {
		t.Fatalf("source write: %v", err)
	}
	run := postEdgeBackfillForTest(t, gateway.URL, `{"dry_run":false,"project":"alpha","relations":["references"],"include_reference_content":true,"sample_limit":20}`)
	population := anyMap(run["reference_population"])
	if anyToInt(population["content_blob_claims"], 0) == 0 || anyToInt(run["written"], 0) != 1 {
		t.Fatalf("expected one bounded content-blob reference, report=%#v", run)
	}
	edges, err := s.memoryStore.listMemoryEdges(context.Background(), memoryEdgeQuery{MemoryID: "alpha::notes/blob-source.md", Relation: "references", Limit: 10})
	if err != nil || len(edges) != 1 || anyToString(edges[0].Metadata["claim_kind"]) != "textual_content_blob" {
		t.Fatalf("expected textual content-blob edge, edges=%#v err=%v", edges, err)
	}
}

func TestStructuredReferenceQuarantinesLegacyUnboundEdgesOnRestart(t *testing.T) {
	s, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	legacy := memoryEdgeEntry{
		EdgeID:   deterministicMemoryEdgeID("alpha::notes/a.md", "references", "alpha::notes/b.md"),
		SourceID: "alpha::notes/a.md", TargetID: "alpha::notes/b.md", Relation: "references", Project: "alpha", Confidence: 1, CreatedAt: nowUTCISO(), Source: memoryEdgeSource,
	}
	if err := s.memoryStore.appendEdge(legacy); err != nil {
		t.Fatalf("append legacy edge: %v", err)
	}
	reloaded, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("reload legacy edge: %v", err)
	}
	edges, err := reloaded.listMemoryEdges(context.Background(), memoryEdgeQuery{Relation: "references", Limit: 10})
	if err != nil || len(edges) != 0 {
		t.Fatalf("legacy unbound reference must not become active, edges=%#v err=%v", edges, err)
	}
	quarantinePath := filepath.Join(s.memoryStore.policy.edgePath + ".quarantine.ndjson")
	raw, err := os.ReadFile(quarantinePath)
	if err != nil || !strings.Contains(string(raw), "legacy_or_unbound_reference") {
		t.Fatalf("expected legacy edge quarantine, err=%v raw=%s", err, string(raw))
	}
	_ = gateway
}

func TestStructuredReferenceConcurrentWritesRemainDeterministic(t *testing.T) {
	s, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	if _, _, err := s.memoryStore.put(normalizedWrite{project: "alpha", fileName: "notes/target.md", content: "target", topicPath: "runbooks/refs"}); err != nil {
		t.Fatalf("target write: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, _, _ = s.memoryStore.put(normalizedWrite{
				project: "alpha", fileName: "notes/source.md", content: "source", topicPath: "runbooks/refs", agentID: "agent", sessionID: "session",
				references: []memoryStructuredReference{{TargetID: "alpha::notes/target.md", Relation: "references", Confidence: 1}},
			})
		}(i)
	}
	wg.Wait()
	edges, err := s.memoryStore.listMemoryEdges(context.Background(), memoryEdgeQuery{MemoryID: "alpha::notes/source.md", Relation: "references", Limit: 10})
	if err != nil || len(edges) != 1 {
		t.Fatalf("concurrent claims must converge to one deterministic edge, edges=%#v err=%v", edges, err)
	}
	encoded, err := json.Marshal(edges[0])
	if err != nil || !strings.Contains(string(encoded), "memory_reference_claim.v1") {
		t.Fatalf("expected stable binding schema after concurrent writes, err=%v edge=%s", err, string(encoded))
	}
}

func TestStructuredReferenceTransactionLocksAllEndpointsThroughReceipt(t *testing.T) {
	s, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	if _, _, err := s.memoryStore.put(normalizedWrite{project: "alpha", fileName: "notes/target.md", content: "target v1", topicPath: "runbooks/refs"}); err != nil {
		t.Fatalf("target write: %v", err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s.memoryStore.beforeReferenceHistoryAppend = func() error {
		once.Do(func() { close(entered) })
		<-release
		return nil
	}
	sourceDone := make(chan error, 1)
	go func() {
		_, _, err := s.memoryStore.put(normalizedWrite{
			project: "alpha", fileName: "notes/source.md", content: "source", topicPath: "runbooks/refs",
			references: []memoryStructuredReference{{TargetID: "alpha::notes/target.md", Relation: "references", Confidence: 1}},
		})
		sourceDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("structured transaction did not reach the history fence")
	}
	targetDone := make(chan error, 1)
	go func() {
		_, _, err := s.memoryStore.put(normalizedWrite{project: "alpha", fileName: "notes/target.md", content: "target v2", topicPath: "runbooks/refs"})
		targetDone <- err
	}()
	select {
	case err := <-targetDone:
		t.Fatalf("target endpoint changed before the structured receipt closed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-sourceDone; err != nil {
		t.Fatalf("structured transaction: %v", err)
	}
	if err := <-targetDone; err != nil {
		t.Fatalf("target update after receipt: %v", err)
	}
	if edges, err := s.memoryStore.listMemoryEdges(context.Background(), memoryEdgeQuery{MemoryID: "alpha::notes/source.md", Relation: "references", Limit: 10}); err != nil || len(edges) != 0 {
		t.Fatalf("post-receipt target update must suppress the old exact binding: edges=%#v err=%v", edges, err)
	}
}

func TestStructuredReferenceFixtureOnlyGraphPopulationReachesNinetyFiveBoundedCases(t *testing.T) {
	s, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	docs := make([]memoryStoreDoc, 0, 190)
	directCases := make([]map[string]any, 0, 95)
	for index := 0; index < 95; index++ {
		targetFile := fmt.Sprintf("notes/target-%03d.md", index)
		sourceFile := fmt.Sprintf("notes/source-%03d.md", index)
		if _, _, err := s.memoryStore.put(normalizedWrite{project: "alpha", fileName: targetFile, content: "target", topicPath: "runbooks/refs"}); err != nil {
			t.Fatalf("target %d write: %v", index, err)
		}
		if _, _, err := s.memoryStore.put(normalizedWrite{
			project: "alpha", fileName: sourceFile, content: fmt.Sprintf("source %03d", index), topicPath: "runbooks/refs",
			references: []memoryStructuredReference{{TargetID: "alpha::" + targetFile, Relation: "references", Confidence: 1}},
		}); err != nil {
			t.Fatalf("source %d write: %v", index, err)
		}
		docs = append(docs,
			memoryStoreDoc{Project: "alpha", FileName: targetFile, TopicPath: "runbooks/refs", Summary: "target"},
			memoryStoreDoc{Project: "alpha", FileName: sourceFile, TopicPath: "runbooks/refs", Summary: fmt.Sprintf("source %03d", index)},
		)
		directCases = append(directCases, map[string]any{
			"id": "direct-" + fmt.Sprintf("%03d", index), "project": "alpha", "query": "source", "expected_files": []string{sourceFile},
		})
	}
	graphCases := s.recallEvalGraphCasesFromDocs(context.Background(), docs, directCases, savedRecallEvalV3MaxGraphCases)
	// These synthetic fixtures exercise bounded graph-case construction. They
	// do not measure the live corpus or claim live reference coverage.
	t.Logf("fixture-only current-state-bound reference positives=%d capacity=%d minimum=%d", len(graphCases), savedRecallEvalV3MaxGraphCases, savedRecallEvalV3MinGraphReferences)
	if len(graphCases) != 95 {
		t.Fatalf("expected all 95 fixture-only current-state-bound references, got %d", len(graphCases))
	}
	for index, graphCase := range graphCases {
		if anyToString(graphCase["graph_label_kind"]) != "current_state_bound_reference" {
			t.Fatalf("graph case %d was not a current-state-bound reference: %#v", index, graphCase)
		}
	}
}

func TestStructuredReferenceGraphRefreshPreservesExistingHoldoutWhenInsufficient(t *testing.T) {
	evalPath := filepath.Join(t.TempDir(), "saved-recall-eval.json")
	t.Setenv("ORCH_RECALL_EVAL_CASES_PATH", evalPath)
	s, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	previous := []byte(`{"schema_id":"saved_recall_eval_case_set.v3","version":3,"case_set_digest":"sha256:previous","cases":[{"id":"previous-case","project":"alpha","expected_files":["notes/previous.md"],"query":"previous"}]}`)
	if err := os.WriteFile(evalPath, previous, 0o600); err != nil {
		t.Fatalf("write previous holdout: %v", err)
	}
	if _, _, err := s.memoryStore.put(normalizedWrite{project: "alpha", fileName: "notes/one.md", content: "one", topicPath: "runbooks/refs"}); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	resp, err := http.Post(gateway.URL+"/memory/recall/eval-cases/refresh", "application/json", strings.NewReader(`{"project":"alpha","include_graph_cases":true,"graph_max_cases":100}`))
	if err != nil {
		t.Fatalf("refresh request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected truthful insufficient-population conflict, status=%d body=%s", resp.StatusCode, raw)
	}
	raw, _ := os.ReadFile(evalPath)
	if string(raw) != string(previous) {
		t.Fatalf("insufficient refresh changed the existing holdout: got=%s", raw)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode conflict response: %v", err)
	}
	if !anyToBool(payload["preserved_existing_case_set"]) || anyToString(payload["code"]) != "insufficient_current_state_bound_reference_population" {
		t.Fatalf("expected preserved truthful conflict, got %#v", payload)
	}
}

func TestStructuredReferenceHistoryRepairRejectsSymlinkAndPathReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not consistently available to an unprivileged Windows test process")
	}
	write := normalizedWrite{
		project: "alpha", fileName: "notes/history-source.md", content: "source", topicPath: "runbooks/history-safety",
		references: []memoryStructuredReference{{TargetID: "alpha::notes/history-target.md", Relation: "references", Confidence: 1}},
	}
	t.Run("symlink", func(t *testing.T) {
		s, gateway := newMemoryGraphTestServer(t, true)
		defer gateway.Close()
		if _, _, err := s.memoryStore.put(normalizedWrite{project: "alpha", fileName: "notes/history-target.md", content: "target", topicPath: "runbooks/history-safety"}); err != nil {
			t.Fatalf("seed history target: %v", err)
		}
		victim := filepath.Join(t.TempDir(), "victim.ndjson")
		victimBytes := []byte("victim-must-not-change\npartial")
		if err := os.WriteFile(victim, victimBytes, 0o600); err != nil {
			t.Fatalf("write history symlink victim: %v", err)
		}
		if err := os.Remove(s.memoryStore.policy.historyPath); err != nil {
			t.Fatalf("remove history for symlink fixture: %v", err)
		}
		if err := os.Symlink(victim, s.memoryStore.policy.historyPath); err != nil {
			t.Fatalf("install history symlink fixture: %v", err)
		}
		if _, _, err := s.memoryStore.put(write); err == nil {
			t.Fatal("structured history append followed an adversarial symlink")
		}
		if got, err := os.ReadFile(victim); err != nil || !bytes.Equal(got, victimBytes) {
			t.Fatalf("history symlink victim changed: got=%q err=%v", got, err)
		}
	})

	t.Run("replacement_after_open", func(t *testing.T) {
		s, gateway := newMemoryGraphTestServer(t, true)
		defer gateway.Close()
		if _, _, err := s.memoryStore.put(normalizedWrite{project: "alpha", fileName: "notes/history-target.md", content: "target", topicPath: "runbooks/history-safety"}); err != nil {
			t.Fatalf("seed history target: %v", err)
		}
		original, err := os.ReadFile(s.memoryStore.policy.historyPath)
		if err != nil {
			t.Fatalf("read original history: %v", err)
		}
		detached := s.memoryStore.policy.historyPath + ".detached"
		replacement := []byte("replacement-must-not-change\n")
		s.memoryStore.memoryReferenceHistoryDescriptorOpened = func() {
			if err := os.Rename(s.memoryStore.policy.historyPath, detached); err != nil {
				t.Fatalf("detach opened history: %v", err)
			}
			if err := os.WriteFile(s.memoryStore.policy.historyPath, replacement, 0o600); err != nil {
				t.Fatalf("replace opened history path: %v", err)
			}
		}
		if _, _, err := s.memoryStore.put(write); err == nil || !strings.Contains(err.Error(), "path changed") {
			t.Fatalf("structured history append accepted path replacement: %v", err)
		}
		if got, err := os.ReadFile(detached); err != nil || !bytes.Equal(got, original) {
			t.Fatalf("detached history descriptor was modified: got=%q err=%v", got, err)
		}
		if got, err := os.ReadFile(s.memoryStore.policy.historyPath); err != nil || !bytes.Equal(got, replacement) {
			t.Fatalf("replacement history path was modified: got=%q err=%v", got, err)
		}
	})
}

func TestStructuredReferenceHistoryRetryUsesBoundedDurableIndex(t *testing.T) {
	s, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	if _, _, err := s.memoryStore.put(normalizedWrite{project: "alpha", fileName: "notes/indexed-target.md", content: "target", topicPath: "runbooks/history-index"}); err != nil {
		t.Fatalf("seed indexed history target: %v", err)
	}
	fillerContent := canonicalMemoryContent("large history filler")
	filler := memoryStoreEntry{
		EventID: bson.NewObjectID().Hex(), Project: "fixture", FileName: "notes/large-history-filler.md", TopicPath: "runbooks/history-index",
		ContentHash: canonicalMemoryContentHash(fillerContent), ContentRef: "sha256:" + canonicalMemoryContentHash(fillerContent),
		DataClass: dataClassLearningMemory, Lifecycle: "durable", StorageTier: "hot", CreatedAt: nowUTCISO(), LastAccess: nowUTCISO(), RawBytes: len(fillerContent),
	}
	fillerRaw, err := json.Marshal(filler)
	if err != nil {
		t.Fatalf("encode large history filler: %v", err)
	}
	largeHistory := bytes.Repeat(append(fillerRaw, '\n'), 5000)
	if len(largeHistory) < 2*1024*1024 {
		t.Fatalf("large history fixture is unexpectedly small: %d", len(largeHistory))
	}
	if err := writeOwnerOnlyDurableAtomicFile(s.memoryStore.policy.historyPath, largeHistory, true); err != nil {
		t.Fatalf("install large history fixture: %v", err)
	}
	write := normalizedWrite{
		project: "alpha", fileName: "notes/indexed-source.md", content: "source", topicPath: "runbooks/history-index",
		references: []memoryStructuredReference{{TargetID: "alpha::notes/indexed-target.md", Relation: "references", Confidence: 1}},
	}
	s.memoryStore.beforeReferenceEdgeSync = func() error { return errors.New("injected post-history interruption") }
	if _, _, err := s.memoryStore.put(write); err == nil {
		t.Fatal("indexed history fixture did not stop after its durable history append")
	}
	s.memoryStore.beforeReferenceEdgeSync = nil
	var observedBytes int64
	var exactReads int
	s.memoryStore.memoryReferenceHistoryObserveIO = func(operation string, count int64) {
		observedBytes += count
		if operation == "exact_index_read" {
			exactReads++
		}
	}
	if err := s.memoryStore.reconcileMemoryReferenceTransactions(context.Background()); err != nil {
		t.Fatalf("reconcile indexed history transaction: %v", err)
	}
	s.memoryStore.memoryReferenceHistoryObserveIO = nil
	if exactReads != 1 || observedBytes > memoryReferenceTransactionMaxBytes+16 {
		t.Fatalf("history retry exceeded bounded exact lookup: exact_reads=%d observed_bytes=%d history_bytes=%d", exactReads, observedBytes, len(largeHistory))
	}
	restarted, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("restart after bounded history acknowledgement: %v", err)
	}
	assertStructuredReferenceTransactionRecovered(t, restarted, "alpha::notes/indexed-source.md", 1)
}

func TestStructuredReferenceABACreatesFreshSourceVersionAndRetiresStaleTransaction(t *testing.T) {
	s, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	if _, _, err := s.memoryStore.put(normalizedWrite{project: "alpha", fileName: "notes/aba-target.md", content: "target", topicPath: "runbooks/aba"}); err != nil {
		t.Fatalf("seed A-B-A target: %v", err)
	}
	writeA := normalizedWrite{
		project: "alpha", fileName: "notes/aba-source.md", content: "A", topicPath: "runbooks/aba",
		references: []memoryStructuredReference{{TargetID: "alpha::notes/aba-target.md", Relation: "references", Confidence: 1}},
	}
	firstA, _, err := s.memoryStore.put(writeA)
	if err != nil {
		t.Fatalf("write first A: %v", err)
	}
	firstTransactions, err := s.memoryStore.loadMemoryReferenceTransactions()
	if err != nil || len(firstTransactions) != 1 {
		t.Fatalf("load first A transaction: count=%d err=%v", len(firstTransactions), err)
	}
	firstTransactionID := firstTransactions[0].TransactionID
	writeB := normalizedWrite{project: "alpha", fileName: "notes/aba-source.md", content: "B", topicPath: "runbooks/aba"}
	if _, _, err := s.memoryStore.put(writeB); err != nil {
		t.Fatalf("write intervening B: %v", err)
	}
	secondA, deduped, err := s.memoryStore.put(writeA)
	if err != nil || deduped {
		t.Fatalf("write refreshed A: deduped=%v err=%v", deduped, err)
	}
	if secondA.EventID == firstA.EventID {
		t.Fatalf("A-B-A reused stale source event: %s", secondA.EventID)
	}
	retry, deduped, err := s.memoryStore.put(writeA)
	if err != nil || !deduped || retry.EventID != secondA.EventID {
		t.Fatalf("exact refreshed A retry was not idempotent: retry=%#v deduped=%v err=%v", retry, deduped, err)
	}
	transactions, err := s.memoryStore.loadMemoryReferenceTransactions()
	if err != nil || len(transactions) != 1 || transactions[0].TransactionID == firstTransactionID || transactions[0].Entry.EventID != secondA.EventID {
		t.Fatalf("stale A transaction was not retired/reissued: transactions=%#v err=%v", transactions, err)
	}
	currentTransactionID := transactions[0].TransactionID
	s.memoryStore.referenceEdgeIndexMu.Lock()
	_, staleIndexed := s.memoryStore.referenceEdgeIndex[firstTransactionID]
	_, currentIndexed := s.memoryStore.referenceEdgeIndex[currentTransactionID]
	indexedTransactions := len(s.memoryStore.referenceEdgeIndex)
	s.memoryStore.referenceEdgeIndexMu.Unlock()
	if staleIndexed || !currentIndexed || indexedTransactions != 1 {
		t.Fatalf("in-process edge index retained retired transaction: stale=%v current=%v count=%d", staleIndexed, currentIndexed, indexedTransactions)
	}
	edges, err := s.memoryStore.listMemoryEdges(context.Background(), memoryEdgeQuery{MemoryID: "alpha::notes/aba-source.md", Relation: "references", Limit: 10})
	if err != nil || len(edges) != 1 || edges[0].Binding == nil || edges[0].Binding.SourceEventID != secondA.EventID {
		t.Fatalf("refreshed A binding is not current: edges=%#v err=%v", edges, err)
	}
	restarted, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("restart A-B-A transaction: %v", err)
	}
	current, ok := restarted.currentEntry("alpha", "notes/aba-source.md")
	if !ok || current.EventID != secondA.EventID {
		t.Fatalf("restart restored stale A source: current=%#v ok=%v", current, ok)
	}
	assertStructuredReferenceTransactionRecovered(t, restarted, "alpha::notes/aba-source.md", 1)
	fence, err := restarted.acquireMemoryEdgeLogFence()
	if err != nil {
		t.Fatalf("acquire edge-log fence for restart index proof: %v", err)
	}
	snapshot, snapshotErr := restarted.snapshotMemoryEdgeLogContextLocked(context.Background(), 0)
	if snapshotErr == nil {
		restarted.referenceEdgeIndexMu.Lock()
		snapshotErr = restarted.refreshMemoryReferenceEdgeIndexLocked(snapshot)
		_, staleIndexed = restarted.referenceEdgeIndex[firstTransactionID]
		_, currentIndexed = restarted.referenceEdgeIndex[currentTransactionID]
		indexedTransactions = len(restarted.referenceEdgeIndex)
		restarted.referenceEdgeIndexMu.Unlock()
	}
	fence.release()
	if snapshotErr != nil {
		t.Fatalf("refresh restart transaction edge index: %v", snapshotErr)
	}
	if staleIndexed || !currentIndexed || indexedTransactions != 1 {
		t.Fatalf("restart edge index included retired historical rows: stale=%v current=%v count=%d", staleIndexed, currentIndexed, indexedTransactions)
	}
}

func TestStructuredReferenceMaintenanceReloadPreservesCompleteClosedSet(t *testing.T) {
	s, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	for index := 0; index < 2; index++ {
		if _, _, err := s.memoryStore.put(normalizedWrite{project: "alpha", fileName: fmt.Sprintf("notes/maintenance-target-%d.md", index), content: "target", topicPath: "runbooks/maintenance"}); err != nil {
			t.Fatalf("seed maintenance target %d: %v", index, err)
		}
	}
	if _, _, err := s.memoryStore.put(normalizedWrite{
		project: "alpha", fileName: "notes/maintenance-source.md", content: "source", topicPath: "runbooks/maintenance",
		references: []memoryStructuredReference{
			{TargetID: "alpha::notes/maintenance-target-0.md", Relation: "references", Confidence: 1},
			{TargetID: "alpha::notes/maintenance-target-1.md", Relation: "references", Confidence: 1},
		},
	}); err != nil {
		t.Fatalf("write maintenance closed set: %v", err)
	}
	s.memoryStore.policy.edgeStartupMaxLines = 1
	if _, err := s.memoryStore.pruneVolatileMemoryGraphEdges(context.Background(), false); err != nil {
		t.Fatalf("compact and reload structured edge set: %v", err)
	}
	assertStructuredReferenceTransactionRecovered(t, s.memoryStore, "alpha::notes/maintenance-source.md", 2)
	t.Setenv("GO_MEMORY_GRAPH_EDGE_STARTUP_MAX_LINES", "1")
	restarted, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("restart maintenance closed set: %v", err)
	}
	assertStructuredReferenceTransactionRecovered(t, restarted, "alpha::notes/maintenance-source.md", 2)
}

func TestReferenceTransactionBlobValidationRejectsConcurrentGrowthAndReplacement(t *testing.T) {
	t.Run("growth", func(t *testing.T) {
		s, gateway := newMemoryGraphTestServer(t, true)
		defer gateway.Close()
		content := "bounded transaction blob"
		hash := canonicalMemoryContentHash(content)
		path, err := s.memoryStore.ensureReferenceTransactionBlob(hash, content)
		if err != nil {
			t.Fatalf("seed transaction blob: %v", err)
		}
		s.memoryStore.memoryReferenceBlobDescriptorOpened = func() {
			file, openErr := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			if openErr != nil {
				t.Fatalf("open transaction blob growth fixture: %v", openErr)
			}
			if _, writeErr := file.WriteString("x"); writeErr != nil {
				_ = file.Close()
				t.Fatalf("grow transaction blob fixture: %v", writeErr)
			}
			if closeErr := file.Close(); closeErr != nil {
				t.Fatalf("close transaction blob growth fixture: %v", closeErr)
			}
		}
		if _, err := s.memoryStore.ensureReferenceTransactionBlob(hash, content); err == nil {
			t.Fatal("concurrently grown transaction blob passed bounded validation")
		}
	})

	t.Run("replacement", func(t *testing.T) {
		s, gateway := newMemoryGraphTestServer(t, true)
		defer gateway.Close()
		content := "descriptor identity blob"
		hash := canonicalMemoryContentHash(content)
		path, err := s.memoryStore.ensureReferenceTransactionBlob(hash, content)
		if err != nil {
			t.Fatalf("seed transaction blob: %v", err)
		}
		detached := path + ".detached"
		s.memoryStore.memoryReferenceBlobDescriptorOpened = func() {
			if err := os.Rename(path, detached); err != nil {
				t.Fatalf("detach transaction blob: %v", err)
			}
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatalf("replace transaction blob path: %v", err)
			}
		}
		if _, err := s.memoryStore.ensureReferenceTransactionBlob(hash, content); err == nil {
			t.Fatal("replaced transaction blob path passed descriptor identity validation")
		}
	})
}

func writeReferenceTransactionCapacityFixture(t *testing.T, store *memoryStore, target memoryStoreEntry, index int) memoryReferenceTransaction {
	t.Helper()
	content := fmt.Sprintf("capacity source %d", index)
	claim := memoryStructuredReference{TargetID: target.Project + "::" + target.FileName, Relation: "references", Confidence: 1}
	source := memoryStoreEntry{
		EventID: bson.NewObjectID().Hex(), Project: "alpha", FileName: fmt.Sprintf("notes/capacity-%04d.md", index), TopicPath: "runbooks/capacity",
		ContentHash: canonicalMemoryContentHash(content), ContentRef: "sha256:" + canonicalMemoryContentHash(content), Lifecycle: "durable", StorageTier: "hot",
		CreatedAt: nowUTCISO(), RawBytes: len(content), References: []memoryStructuredReference{claim},
	}
	snapshotDigest := "sha256:" + sha256Hex("capacity-snapshot")
	edge, err := store.buildMemoryReferenceEdge(source, target, 1, 1, snapshotDigest, store.referenceExclusionPolicyDigest(), claim, "structured_write")
	if err != nil {
		t.Fatalf("build capacity transaction edge %d: %v", index, err)
	}
	requestDigest := "sha256:" + sha256Hex(fmt.Sprintf("capacity-request-%d", index))
	transactionID := "ref_tx_" + sha256Hex(requestDigest + "\x00" + source.Project + "::" + source.FileName)[:32]
	source.ReferenceTransactionID = transactionID
	edge.Metadata["reference_transaction_id"] = transactionID
	entryJSON, err := json.Marshal(source)
	if err != nil {
		t.Fatalf("encode capacity transaction entry %d: %v", index, err)
	}
	transaction := memoryReferenceTransaction{
		SchemaID: memoryReferenceTransactionSchemaID, Version: 1, TransactionID: transactionID, RequestDigest: requestDigest,
		PreparedAt: nowUTCISO(), ContentRef: source.ContentRef, HistoryDigest: "sha256:" + sha256Hex(string(entryJSON)),
		EdgeSetDigest: memoryReferenceEdgeSetDigest([]memoryEdgeEntry{edge}), SnapshotDigest: snapshotDigest, Entry: source, Edges: []memoryEdgeEntry{edge},
	}
	if !memoryReferenceTransactionValid(transaction) {
		t.Fatalf("capacity transaction fixture %d is invalid", index)
	}
	raw, err := json.Marshal(transaction)
	if err != nil {
		t.Fatalf("encode capacity transaction %d: %v", index, err)
	}
	if err := os.WriteFile(store.memoryReferenceTransactionPath(transactionID), append(raw, '\n'), 0o600); err != nil {
		t.Fatalf("write capacity transaction %d: %v", index, err)
	}
	receipt := memoryReferenceTransactionReceipt{
		SchemaID: memoryReferenceReceiptSchemaID, Version: 1, TransactionID: transactionID, RequestDigest: requestDigest,
		HistoryDigest: transaction.HistoryDigest, EdgeSetDigest: transaction.EdgeSetDigest, ClosedAt: nowUTCISO(),
	}
	receipt.ReceiptDigest = memoryReferenceReceiptDigest(receipt)
	receiptRaw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("encode capacity receipt %d: %v", index, err)
	}
	if err := os.WriteFile(store.memoryReferenceReceiptPath(transactionID), append(receiptRaw, '\n'), 0o600); err != nil {
		t.Fatalf("write capacity receipt %d: %v", index, err)
	}
	return transaction
}

func TestReferenceTransactionCapacityAdmissionAndAtomicTempRestartInventory(t *testing.T) {
	s, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	target, _, err := s.memoryStore.put(normalizedWrite{project: "alpha", fileName: "notes/capacity-target.md", content: "target", topicPath: "runbooks/capacity"})
	if err != nil {
		t.Fatalf("seed capacity target: %v", err)
	}
	currentWrite := normalizedWrite{
		project: "alpha", fileName: "notes/capacity-current.md", content: "current-v1", topicPath: "runbooks/capacity",
		references: []memoryStructuredReference{{TargetID: "alpha::notes/capacity-target.md", Relation: "references", Confidence: 1}},
	}
	if _, _, err := s.memoryStore.put(currentWrite); err != nil {
		t.Fatalf("seed replaceable capacity transaction: %v", err)
	}
	if err := ensureOwnerOnlyDirectory(s.memoryStore.memoryReferenceTransactionRoot(), true); err != nil {
		t.Fatalf("prepare capacity transaction root: %v", err)
	}
	for index := 1; index < memoryReferenceTransactionMaxStartup; index++ {
		writeReferenceTransactionCapacityFixture(t, s.memoryStore, target, index)
	}
	atomicTemp := filepath.Join(s.memoryStore.memoryReferenceTransactionRoot(), ".ref_tx_atomic.pending.json.tmp-interrupted")
	if err := os.WriteFile(atomicTemp, []byte("interrupted atomic temporary"), 0o600); err != nil {
		t.Fatalf("write atomic temporary fixture: %v", err)
	}
	transactions, err := s.memoryStore.loadMemoryReferenceTransactions()
	if err != nil || len(transactions) != memoryReferenceTransactionMaxStartup {
		t.Fatalf("fresh startup inventory rejected 4096 closed transactions plus atomic temp: count=%d err=%v", len(transactions), err)
	}
	freshLoader := &memoryStore{policy: s.memoryStore.policy}
	transactions, err = freshLoader.loadMemoryReferenceTransactions()
	if err != nil || len(transactions) != memoryReferenceTransactionMaxStartup {
		t.Fatalf("restart loader rejected bounded transaction inventory: count=%d err=%v", len(transactions), err)
	}
	closed, err := freshLoader.closedMemoryReferenceTransactions()
	if err != nil || len(closed) != memoryReferenceTransactionMaxStartup {
		t.Fatalf("restart loader lost closed transaction receipts: count=%d err=%v", len(closed), err)
	}
	slack := writeReferenceTransactionCapacityFixture(t, s.memoryStore, target, memoryReferenceTransactionMaxStartup)
	transactions, err = freshLoader.loadMemoryReferenceTransactions()
	if err != nil || len(transactions) != memoryReferenceTransactionMaxStartup+1 {
		t.Fatalf("restart loader rejected one crash-recovery slack transaction: count=%d err=%v", len(transactions), err)
	}
	if err := os.Remove(s.memoryStore.memoryReferenceTransactionPath(slack.TransactionID)); err != nil {
		t.Fatalf("deactivate crash-recovery slack fixture: %v", err)
	}
	if err := syncOwnerOnlyDirectory(s.memoryStore.memoryReferenceTransactionRoot()); err != nil {
		t.Fatalf("sync crash-recovery slack deactivation: %v", err)
	}
	if err := os.Remove(s.memoryStore.memoryReferenceReceiptPath(slack.TransactionID)); err != nil {
		t.Fatalf("remove crash-recovery slack receipt fixture: %v", err)
	}
	if err := syncOwnerOnlyDirectory(s.memoryStore.memoryReferenceTransactionRoot()); err != nil {
		t.Fatalf("sync crash-recovery slack cleanup: %v", err)
	}
	if _, _, err := s.memoryStore.put(normalizedWrite{
		project: "alpha", fileName: "notes/capacity-4097.md", content: "must-reject", topicPath: "runbooks/capacity",
		references: []memoryStructuredReference{{TargetID: "alpha::notes/capacity-target.md", Relation: "references", Confidence: 1}},
	}); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("4097th unrelated transaction bypassed bounded admission: %v", err)
	}
	if _, err := os.Lstat(atomicTemp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fenced capacity compaction retained interrupted atomic temp: %v", err)
	}
	currentWrite.content = "current-v2"
	updated, _, err := s.memoryStore.put(currentWrite)
	if err != nil {
		t.Fatalf("bounded replacement did not use restart-safe admission slack: %v", err)
	}
	transactions, err = s.memoryStore.loadMemoryReferenceTransactions()
	if err != nil || len(transactions) != memoryReferenceTransactionMaxStartup {
		t.Fatalf("replacement did not retire its superseded closed artifacts: count=%d event=%s err=%v", len(transactions), updated.EventID, err)
	}
}

func backfillRelationStatIntMap(payload map[string]any, relation string, field string) int {
	relations := anyMap(payload["relations"])
	return anyToInt(anyMap(relations[relation])[field], 0)
}
