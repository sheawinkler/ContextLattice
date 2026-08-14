package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func seedMemoryGraphRepairDoc(t *testing.T, store *memoryStore, project, fileName, topic, summary, agent, session string) memoryStoreEntry {
	t.Helper()
	entry, _, err := store.put(normalizedWrite{
		project: project, fileName: fileName, topicPath: topic, content: summary,
		agentID: agent, sessionID: session, createdAt: nowUTCISO(), lifecycle: "durable",
	})
	if err != nil {
		t.Fatalf("seed repair doc %s/%s: %v", project, fileName, err)
	}
	return entry
}

func repairRequest(t *testing.T, store *memoryStore, payload map[string]any) memoryGraphRepairRequest {
	t.Helper()
	req, err := normalizeMemoryGraphRepairRequest(payload, store.policy)
	if err != nil {
		t.Fatalf("normalize repair request: %v", err)
	}
	req.ActorAuthority = "signed_test_authority"
	req.ActorPrincipalDigest = "sha256:" + strings.Repeat("1", 64)
	req.ActorScopeDigest = "sha256:" + strings.Repeat("2", 64)
	req.ActorWorkspaceDigest = "sha256:" + strings.Repeat("3", 64)
	req.ActorInstallDigest = "sha256:" + strings.Repeat("4", 64)
	req.ActorAuthorityDigest = "sha256:" + strings.Repeat("5", 64)
	req.ActorCustodyDigest = "sha256:" + strings.Repeat("6", 64)
	req.PlanApplicable = true
	return req
}

func TestMemoryGraphRepairEvidenceCacheBoundsRepeatedWholeLogScans(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/cache-a.md", "runbooks/cache", "cache anchor", "", "")
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/cache-b.md", "runbooks/cache", "cache neighbor", "", "")
	if _, err := store.memoryStore.upsertMemoryEdge(context.Background(), testMemoryEdgeLogEntry("alpha::notes/cache-a.md", "alpha::notes/cache-b.md")); err != nil {
		t.Fatalf("seed cached repair edge: %v", err)
	}
	req := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": false})
	snapshot, err := store.memoryStore.captureMemoryGraphRepairSnapshot(context.Background(), req)
	if err != nil {
		t.Fatalf("capture cached repair snapshot: %v", err)
	}
	var fullReads, fullHashes int64
	store.memoryStore.memoryEdgeLogObserveIO = func(operation string, bytes int64) {
		switch operation {
		case "full_scan_read":
			fullReads++
		case "full_scan_hash":
			fullHashes++
		}
	}
	if _, err := store.memoryStore.captureMemoryGraphRepairEdges(snapshot); err != nil {
		t.Fatalf("first cached repair evidence capture: %v", err)
	}
	if _, err := store.memoryStore.captureMemoryGraphRepairEdges(snapshot); err != nil {
		t.Fatalf("second cached repair evidence capture: %v", err)
	}
	store.memoryStore.memoryEdgeLogObserveIO = nil
	if fullReads > 1 || fullHashes > 1 {
		t.Fatalf("repair evidence recaptured the whole log instead of reusing validated state: reads=%d hashes=%d", fullReads, fullHashes)
	}
}

func TestMemoryGraphRepairEvidenceCacheUsesIncrementalSuffixAfterAppend(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/cache-a.md", "runbooks/cache", "cache anchor", "", "")
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/cache-b.md", "runbooks/cache", "cache neighbor", "", "")
	req := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": false})
	snapshot, err := store.memoryStore.captureMemoryGraphRepairSnapshot(context.Background(), req)
	if err != nil {
		t.Fatalf("capture incremental repair snapshot: %v", err)
	}
	if _, err := store.memoryStore.captureMemoryGraphRepairEdges(snapshot); err != nil {
		t.Fatalf("seed incremental evidence cache: %v", err)
	}
	var fullReads, fullHashes, fullEvidence, incrementalBytes int64
	store.memoryStore.memoryEdgeLogObserveIO = func(operation string, bytes int64) {
		switch operation {
		case "full_scan_read":
			fullReads++
		case "full_scan_hash":
			fullHashes++
		case "repair_evidence_full_scan":
			fullEvidence++
		case "incremental_evidence_read":
			incrementalBytes += bytes
		}
	}
	var appendedBytes int64
	for index := 0; index < 32; index++ {
		edge := testMemoryEdgeLogEntry("alpha::notes/cache-a.md", "alpha::notes/cache-b.md")
		edge.Relation = fmt.Sprintf("references_%d", index)
		stored, _, appendErr := store.memoryStore.appendMemoryEdgeLog(edge, true)
		if appendErr != nil {
			t.Fatalf("append incremental evidence row %d: %v", index, appendErr)
		}
		encoded, _ := json.Marshal(stored)
		appendedBytes += int64(len(encoded) + 1)
		evidence, captureErr := store.memoryStore.captureMemoryGraphRepairEdges(snapshot)
		if captureErr != nil {
			t.Fatalf("capture incremental evidence row %d: %v", index, captureErr)
		}
		if evidence.LogFileSize < appendedBytes {
			t.Fatalf("incremental evidence lost durable size: evidence=%d appended=%d", evidence.LogFileSize, appendedBytes)
		}
	}
	store.memoryStore.memoryEdgeLogObserveIO = nil
	if fullReads != 0 || fullHashes != 0 || fullEvidence != 0 {
		t.Fatalf("incremental repair continuation performed a whole-log scan: reads=%d hashes=%d evidence=%d", fullReads, fullHashes, fullEvidence)
	}
	if incrementalBytes <= 0 || incrementalBytes > appendedBytes {
		t.Fatalf("incremental repair evidence bytes were not bounded to appended rows: incremental=%d appended=%d", incrementalBytes, appendedBytes)
	}
}

func TestMemoryGraphRepairEvidenceRejectsSameSizeReplacementBeforeAppend(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/cache-a.md", "runbooks/cache", "cache anchor", "", "")
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/cache-b.md", "runbooks/cache", "cache neighbor", "", "")
	if _, err := store.memoryStore.upsertMemoryEdge(context.Background(), testMemoryEdgeLogEntry("alpha::notes/cache-a.md", "alpha::notes/cache-b.md")); err != nil {
		t.Fatalf("seed replacement cache edge: %v", err)
	}
	req := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": false})
	snapshot, err := store.memoryStore.captureMemoryGraphRepairSnapshot(context.Background(), req)
	if err != nil {
		t.Fatalf("capture replacement cache snapshot: %v", err)
	}
	if _, err := store.memoryStore.captureMemoryGraphRepairEdges(snapshot); err != nil {
		t.Fatalf("seed replacement cache evidence: %v", err)
	}
	before, err := store.memoryStore.snapshotMemoryEdgeLog(memoryEdgeLogMaxRecoveryBytes)
	if err != nil {
		t.Fatalf("snapshot before same-size repair replacement: %v", err)
	}
	replacement := bytes.Replace(before.Bytes, []byte("notes/cache-a.md"), []byte("notes/cache-z.md"), 1)
	if len(replacement) != len(before.Bytes) || bytes.Equal(replacement, before.Bytes) {
		t.Fatal("same-size repair replacement fixture is invalid")
	}
	if err := os.WriteFile(store.memoryStore.policy.edgePath, replacement, ownerOnlyFileMode); err != nil {
		t.Fatalf("write same-size repair replacement: %v", err)
	}
	appended := testMemoryEdgeLogEntry("alpha::notes/cache-z.md", "alpha::notes/cache-b.md")
	appended.Relation = "after_same_size_replacement"
	if _, _, err := store.memoryStore.appendMemoryEdgeLog(appended, true); err != nil {
		t.Fatalf("append after same-size repair replacement: %v", err)
	}
	var fullEvidence int64
	store.memoryStore.memoryEdgeLogObserveIO = func(operation string, _ int64) {
		if operation == "repair_evidence_full_scan" {
			fullEvidence++
		}
	}
	evidence, err := store.memoryStore.captureMemoryGraphRepairEdges(snapshot)
	store.memoryStore.memoryEdgeLogObserveIO = nil
	if err != nil {
		t.Fatalf("capture after same-size replacement and append: %v", err)
	}
	if fullEvidence != 1 {
		t.Fatalf("stale prefix evidence was reused after replacement and append: full_evidence_scans=%d", fullEvidence)
	}
	var sawReplacement, sawSuffix, sawOldPrefix bool
	for _, row := range evidence.Rows {
		if row.Invalid {
			continue
		}
		source := strings.ToLower(row.Edge.SourceID)
		if strings.Contains(source, "cache-z.md") {
			sawReplacement = true
		}
		if row.Edge.Relation == appended.Relation {
			sawSuffix = true
		}
		if strings.Contains(source, "cache-a.md") {
			sawOldPrefix = true
		}
	}
	if !sawReplacement || !sawSuffix || sawOldPrefix {
		t.Fatalf("replacement evidence mixed stale prefix and current suffix: replacement=%t suffix=%t old_prefix=%t rows=%#v", sawReplacement, sawSuffix, sawOldPrefix, evidence.Rows)
	}
}

func TestMemoryGraphRepairContinuationIndexStaysBoundedFor200KRows(t *testing.T) {
	evidence := memoryGraphRepairEdgeEvidence{
		Latest: map[string]memoryEdgeEntry{}, RetirementSeen: map[string]bool{},
		RepairRuns: map[string]map[string]struct{}{}, RepairActionRows: map[string]map[string]memoryEdgeEntry{},
		Complete: true, Project: "alpha", DigestIndex: &memoryGraphRepairDigestIndex{},
	}
	for index := 0; index < 200000; index++ {
		evidence.record(memoryGraphRepairEdgeRow{Edge: memoryEdgeEntry{
			EdgeID: fmt.Sprintf("edge-%06d", index), Project: "alpha",
			SourceID: fmt.Sprintf("alpha::source-%06d.md", index), TargetID: fmt.Sprintf("alpha::target-%06d.md", index),
		}, RawDigest: fmt.Sprintf("sha256:%064d", index)})
	}
	if evidence.DigestIndex == nil || evidence.DigestIndex.root == nil || evidence.DigestIndex.root.height > 64 {
		t.Fatalf("continuation digest index lost its hard height bound: height=%d", evidence.DigestIndex.root.height)
	}
	digest := evidence.projectDigest()
	evidence.Digest = digest
	for chunk := 0; chunk < 3125; chunk++ {
		resumed := memoryGraphRepairEvidenceWithoutPendingRun(evidence, "alpha", "run-200k", map[string]struct{}{fmt.Sprintf("future-%d", chunk): {}})
		if resumed.DigestIndex != evidence.DigestIndex || resumed.Digest != digest {
			t.Fatalf("continuation rebuilt the validated prefix at chunk=%d", chunk)
		}
	}
}

func TestMemoryGraphRepairRequestLockHonorsCancellation(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	release := store.memoryStore.lockMemoryPath("memory-graph-repair:cancel")
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := store.memoryStore.lockMemoryPathContext(ctx, "memory-graph-repair:cancel")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("request-owned repair lock ignored cancellation: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancellable repair lock wait exceeded bound: %s", elapsed)
	}
}

func TestExactStateSnapshotContextFailsClosedOnCanceledFence(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	paths, err := store.memoryStore.exactStatePathsSnapshotContext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("exact-state snapshot did not surface fence cancellation: paths=%#v err=%v", paths, err)
	}
	if paths != nil {
		t.Fatalf("exact-state snapshot exposed a mirror after fence cancellation: %#v", paths)
	}
}

func postRepair(t *testing.T, gatewayURL string, payload string, role string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, gatewayURL+"/v1/memory/edges/repair", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("build repair request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if role != "" {
		req.Header.Set("X-ContextLattice-Workspace-Role", role)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("repair request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	decoded := map[string]any{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("decode repair response: %v body=%s", err, raw)
		}
	}
	return resp.StatusCode, decoded
}

func TestMemoryGraphRepairCanonicalAliasesAndLegacyIDs(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	left := seedMemoryGraphRepairDoc(t, store.memoryStore, "Alpha", "Notes\\a.md", "runbooks/graph", "shared graph identity", "", "")
	right := seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/b.md", "runbooks/graph", "shared graph identity", "", "")
	req := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": false})
	snapshot, err := store.memoryStore.captureMemoryGraphRepairSnapshot(context.Background(), req)
	if err != nil {
		t.Fatalf("capture repair snapshot: %v", err)
	}
	aliases := &memoryGraphRepairAliasIndex{Aliases: snapshot.Aliases, AmbiguousAliases: snapshot.AmbiguousAliases}
	for _, raw := range []string{"alpha::notes/a.md", "alpha/notes/a.md", "memory://alpha/notes/a.md", "ALPHA\\NOTES\\A.MD", left.EventID, left.ObjectID} {
		got, status := aliases.resolve(raw)
		if status != "bound" || !strings.EqualFold(got, "Alpha::Notes/a.md") {
			t.Fatalf("alias %q resolved to %q/%s; snapshot=%#v", raw, got, status, snapshot)
		}
	}
	got, status := aliases.resolve("alpha::notes/missing.md")
	if status != "unbound" || got != "alpha::notes/missing.md" {
		t.Fatalf("expected valid but unbound legacy path, got %q/%s", got, status)
	}
	// A legacy event-id edge is accepted only after alias binding. It must be
	// canonicalized to current-state identities without changing the source log.
	legacy := memoryEdgeEntry{SourceID: "alpha/notes/a.md", TargetID: right.EventID, Relation: "references", Project: "alpha", Confidence: 1, CreatedAt: nowUTCISO()}
	file, err := openOwnerOnlyAppend(store.memoryStore.policy.edgePath, true)
	if err != nil {
		t.Fatalf("open legacy edge fixture: %v", err)
	}
	encoded, _ := json.Marshal(legacy)
	for i := 0; i < 2; i++ {
		if _, err := file.Write(append(encoded, '\n')); err != nil {
			t.Fatalf("write legacy edge fixture: %v", err)
		}
	}
	if err := file.Sync(); err != nil {
		t.Fatalf("sync legacy edge fixture: %v", err)
	}
	_ = file.Close()
	evidence, err := store.memoryStore.captureMemoryGraphRepairEdges(snapshot)
	if err != nil {
		t.Fatalf("capture legacy edge evidence: %v", err)
	}
	legacyID := deterministicMemoryEdgeID("alpha::notes/a.md", "references", "alpha::notes/b.md")
	if got := evidence.Latest[legacyID]; got.SourceID != "alpha::notes/a.md" || got.TargetID != "alpha::notes/b.md" {
		t.Fatalf("legacy edge was not rebound to current identities: %#v", got)
	}
	if evidence.DuplicateCount < 1 {
		t.Fatalf("duplicate edge evidence was not reported: %#v", evidence)
	}
}

func TestMemoryGraphRepairDryRunIsBoundedAndHonest(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/a.md", "runbooks/graph", "graph anchor", "", "")
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/b.md", "runbooks/graph", "graph neighbor", "", "")
	if _, _, err := store.memoryStore.appendMemoryEdgeLog(memoryEdgeEntry{
		SourceID: "alpha::notes/legacy.md", TargetID: "alpha::notes/retired.md", Relation: "references", Project: "alpha", Confidence: 1,
		CreatedAt: nowUTCISO(), Lifecycle: "durable", Provenance: map[string]any{"kind": "legacy_explicit"},
	}, true); err != nil {
		t.Fatalf("seed unbound legacy edge: %v", err)
	}
	status, response := postRepair(t, gateway.URL, `{"project":"alpha","mode":"dry_run","include_inferred":false,"topic_peer_limit":1}`, "")
	if status != http.StatusOK {
		t.Fatalf("dry run status=%d response=%#v", status, response)
	}
	if !anyToBool(response["snapshot_complete"]) || !anyToBool(response["edge_evidence_complete"]) || anyToBool(response["raw_store_scanned"]) {
		t.Fatalf("dry run violated bounded snapshot contract: %#v", response)
	}
	if anyToInt(response["indexed_doc_count"], 0) != 2 || anyToInt(response["isolated_doc_count"], -1) != 2 {
		t.Fatalf("dry-run telemetry claimed unbound connectedness: %#v", response)
	}
	if anyToString(response["connectedness_claim"]) != "bound_current_state_intersection_only" || anyToBool(response["qdrant_authoritative"]) {
		t.Fatalf("dry-run honesty fields missing: %#v", response)
	}
	if anyToInt(response["bound_edge_count"], -1) != 0 || anyToInt(response["unbound_edge_count"], 0) != 1 {
		t.Fatalf("unbound legacy edge was presented as bound: %#v", response)
	}
	bindingBefore, ok := response["binding_before"].(map[string]any)
	if !ok || anyToInt(bindingBefore["bound_edges"], -1) != 0 || anyToInt(bindingBefore["unbound_edges"], -1) != 1 {
		t.Fatalf("dry-run binding breakdown was not honest: %#v", response["binding_before"])
	}
	if _, ok := bindingBefore["by_relation"].(map[string]any); !ok {
		t.Fatalf("dry-run binding breakdown omitted relation counts: %#v", bindingBefore)
	}
	resolutionProof, ok := response["repair_resolution_proof"].(map[string]any)
	if !ok || !anyToBool(resolutionProof["all_repaired_writes_bound"]) {
		t.Fatalf("dry-run repaired-edge resolution proof was not current-state bound: %#v", response["repair_resolution_proof"])
	}
	telemetry, err := store.memoryStore.memoryGraphTelemetrySnapshot(context.Background(), "alpha", false, 10, time.Time{})
	if err != nil {
		t.Fatalf("graph telemetry: %v", err)
	}
	if telemetry.BoundEdgeCount != 0 || telemetry.UnboundEdgeCount != 1 || telemetry.ConnectednessClaim != "bound_current_state_intersection_only" {
		t.Fatalf("graph telemetry did not disclose endpoint binding: %#v", telemetry)
	}
}

func TestMemoryGraphRepairApplyOperatorChunkRestartAndIdempotency(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/a.md", "runbooks/graph", "graph anchor", "", "")
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/b.md", "runbooks/graph", "graph neighbor", "", "")
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/c.md", "runbooks/graph", "graph third", "", "")
	applyBody := `{"project":"alpha","mode":"apply","confirm_project":"alpha","operator_confirmed":true,"include_inferred":false,"topic_peer_limit":1,"chunk_size":1}`
	status, denied := postRepair(t, gateway.URL, applyBody, "")
	if status == http.StatusOK || anyToBool(denied["ok"]) {
		t.Fatalf("spoofable caller role reached apply: status=%d response=%#v", status, denied)
	}
	dryReq := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": false, "topic_peer_limit": 1})
	dryReport, err := store.memoryStore.memoryGraphRepair(context.Background(), dryReq)
	if err != nil {
		t.Fatalf("create dry-run plan: %v", err)
	}
	applyReq := dryReq
	applyReq.DryRun, applyReq.Apply = false, true
	applyReq.PlanReceiptRef = anyToString(dryReport["plan_receipt_ref"])
	applyReq.PlanReceiptDigest = anyToString(dryReport["plan_receipt_digest"])
	applyReq.ChunkSize = 1
	var captureCalls atomic.Int64
	store.memoryStore.memoryGraphRepairCaptureHook = func() { captureCalls.Add(1) }
	response, err := store.memoryStore.memoryGraphRepair(context.Background(), applyReq)
	if err != nil || !anyToBool(response["apply"]) {
		t.Fatalf("first operator apply failed: err=%v response=%#v", err, response)
	}
	if _, ok := response["binding_before"].(map[string]any); !ok {
		t.Fatalf("apply omitted pre-chunk binding evidence: %#v", response)
	}
	if _, ok := response["binding_after"].(map[string]any); !ok {
		t.Fatalf("apply omitted post-chunk binding evidence: %#v", response)
	}
	if proof, ok := response["resolution_proof"].(map[string]any); !ok || !anyToBool(proof["all_repaired_writes_bound"]) {
		t.Fatalf("apply did not prove repaired writes resolve to current-state docs: %#v", response["resolution_proof"])
	}
	if got := captureCalls.Load(); got > 3 {
		t.Fatalf("edge evidence was recaptured per action: capture_calls=%d", got)
	}
	checkpointID := anyToString(response["checkpoint_id"])
	if checkpointID == "" || anyToInt(response["remaining"], 0) == 0 {
		t.Fatalf("chunked apply did not return a continuation checkpoint: %#v", response)
	}
	// Continuation remains bound to the byte-identical immutable plan receipt.
	// Receipt mutation after the first chunk is not an expiry simulation; it is
	// custody tampering and is covered by the receipt guard tests below.
	store.memoryStore.memoryGraphRepairCaptureHook = nil
	for i := 0; i < 8 && anyToInt(response["remaining"], 0) > 0; i++ {
		applyReq.CheckpointID = checkpointID
		response, err = store.memoryStore.memoryGraphRepair(context.Background(), applyReq)
		if err != nil {
			checkSnap, snapErr := store.memoryStore.captureMemoryGraphRepairSnapshot(context.Background(), applyReq)
			checkEv, evErr := store.memoryStore.captureMemoryGraphRepairEdges(checkSnap)
			checkActs, _, actErr := store.memoryStore.buildMemoryGraphRepairPlan(context.Background(), checkSnap, checkEv, applyReq)
			t.Fatalf("continuation %d failed: %v plan=%s current=%s snap=%v ev=%v acts=%v", i, err, anyToString(dryReport["plan_action_digest"]), memoryGraphRepairActionDigest(checkActs), snapErr, evErr, actErr)
		}
	}
	if anyToInt(response["remaining"], -1) != 0 || anyToString(response["status"]) != "complete" {
		t.Fatalf("bounded continuation did not complete: %#v", response)
	}
	statBefore, err := os.Stat(store.memoryStore.policy.edgePath)
	if err != nil {
		t.Fatalf("stat edge evidence: %v", err)
	}
	applyReq.CheckpointID = checkpointID
	repeated, err := store.memoryStore.memoryGraphRepair(context.Background(), applyReq)
	if err != nil || !anyToBool(repeated["idempotent"]) {
		t.Fatalf("completed apply was not idempotent: err=%v response=%#v", err, repeated)
	}
	statAfter, err := os.Stat(store.memoryStore.policy.edgePath)
	if err != nil {
		t.Fatalf("stat repeated edge evidence: %v", err)
	}
	if statAfter.Size() != statBefore.Size() {
		t.Fatalf("idempotent replay appended evidence: before=%d after=%d", statBefore.Size(), statAfter.Size())
	}
}

func TestMemoryGraphRepairRetiresStaleInferredWithoutDeletingEvidence(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	left := seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/a.md", "runbooks/graph", "shared graph repair token", "", "")
	right := seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/b.md", "runbooks/graph", "shared graph repair token", "", "")
	old := memoryEdgeEntry{SourceID: "alpha::notes/a.md", TargetID: "alpha::notes/b.md", Relation: "inferred_related", Project: "alpha", Confidence: 0.95, CreatedAt: "2020-01-01T00:00:00Z", Lifecycle: "durable", Metadata: map[string]any{"inferred": true}, Provenance: map[string]any{"kind": "inferred_memory_edge_scoring", "version": "old"}}
	oldStored, err := store.memoryStore.upsertMemoryEdge(context.Background(), old)
	if err != nil {
		t.Fatalf("seed stale inferred edge: %v", err)
	}
	before, err := os.ReadFile(store.memoryStore.policy.edgePath)
	if err != nil {
		t.Fatalf("read original edge evidence: %v", err)
	}
	req := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": true, "inferred_min_shared_terms": 1, "inferred_min_score": 0.6, "stale_after": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)})
	dryReport, err := store.memoryStore.memoryGraphRepair(context.Background(), req)
	if err != nil {
		t.Fatalf("create stale dry-run plan: %v", err)
	}
	snapshot, err := store.memoryStore.captureMemoryGraphRepairSnapshot(context.Background(), req)
	if err != nil {
		t.Fatalf("capture stale snapshot: %v", err)
	}
	evidence, err := store.memoryStore.captureMemoryGraphRepairEdges(snapshot)
	if err != nil {
		t.Fatalf("capture stale evidence: %v", err)
	}
	actions, counts, err := store.memoryStore.buildMemoryGraphRepairPlan(context.Background(), snapshot, evidence, req)
	if err != nil {
		t.Fatalf("build stale repair plan: %v", err)
	}
	if counts["retire"] < 1 || len(actions) < 1 {
		t.Fatalf("stale inferred edge was not scheduled for retirement: counts=%#v actions=%#v", counts, actions)
	}
	if !strings.Contains(string(before), "inferred_memory_edge_scoring") {
		t.Fatalf("stale evidence fixture missing")
	}
	applyReq := req
	applyReq.DryRun, applyReq.Apply = false, true
	applyReq.ChunkSize = 1
	applyReq.OperatorConfirmed, applyReq.ConfirmProject = true, "alpha"
	applyReq.PlanReceiptRef = anyToString(dryReport["plan_receipt_ref"])
	applyReq.PlanReceiptDigest = anyToString(dryReport["plan_receipt_digest"])
	result, err := store.memoryStore.memoryGraphRepair(context.Background(), applyReq)
	if err != nil {
		t.Fatalf("apply stale repair: %v", err)
	}
	countsResult, _ := result["counts"].(map[string]int)
	if countsResult["retired_total"] < 1 {
		t.Fatalf("stale inferred retirement not recorded: %#v", result)
	}
	betweenSnapshot, betweenErr := store.memoryStore.captureMemoryGraphRepairSnapshot(context.Background(), req)
	betweenEvidence, evidenceErr := store.memoryStore.captureMemoryGraphRepairEdges(betweenSnapshot)
	if betweenErr != nil || evidenceErr != nil || !memoryGraphRepairEdgeRetired(betweenEvidence.Latest[oldStored.EdgeID]) || anyToString(result["status"]) != "running" {
		t.Fatalf("retirement was not durably visible at the chunk boundary: snapshot_err=%v evidence_err=%v result=%#v latest=%#v", betweenErr, evidenceErr, result, betweenEvidence.Latest[oldStored.EdgeID])
	}
	applyReq.CheckpointID = anyToString(result["checkpoint_id"])
	for index := 0; anyToInt(result["remaining"], 0) > 0 && index <= len(actions); index++ {
		result, err = store.memoryStore.memoryGraphRepair(context.Background(), applyReq)
		if err != nil {
			t.Fatalf("continue stale repair across retirement boundary: %v", err)
		}
	}
	if anyToInt(result["remaining"], -1) != 0 || anyToString(result["status"]) != "complete" {
		t.Fatalf("stale repair did not complete its immutable cross-chunk plan: %#v", result)
	}
	after, err := os.ReadFile(store.memoryStore.policy.edgePath)
	if err != nil {
		t.Fatalf("read repaired edge evidence: %v", err)
	}
	if !strings.HasPrefix(string(after), string(before)) || len(after) <= len(before) {
		t.Fatalf("repair deleted or rewrote valid evidence")
	}
	postSnapshot, err := store.memoryStore.captureMemoryGraphRepairSnapshot(context.Background(), req)
	if err != nil {
		t.Fatalf("capture repaired snapshot: %v", err)
	}
	postEvidence, err := store.memoryStore.captureMemoryGraphRepairEdges(postSnapshot)
	if err != nil {
		t.Fatalf("capture repaired evidence: %v", err)
	}
	latest, ok := postEvidence.Latest[oldStored.EdgeID]
	if !ok || memoryGraphRepairEdgeRetired(latest) || !memoryGraphRepairEdgeIsInferred(latest) || anyToString(latest.Provenance["kind"]) != "inferred_memory_edge_scoring" {
		t.Fatalf("stale inferred edge was not replaced with preserved inferred provenance: %#v", latest)
	}
	for _, row := range postEvidence.Rows {
		if row.Invalid || anyToString(row.Edge.Metadata["repair_run_id"]) == "" {
			continue
		}
		kind := "write"
		if anyToString(row.Edge.Metadata["repair_action"]) == "retire_stale_inferred" {
			kind = "retire"
		}
		if !memoryGraphRepairProvenanceClosed(row.Edge, snapshot, kind) {
			t.Fatalf("repair append lacked closed snapshot provenance: kind=%s edge=%#v", kind, row.Edge)
		}
		if anyToString(row.Edge.Provenance["source_corpus"]) != memoryGraphRepairSourceCorpus || anyToString(row.Edge.Provenance["source_snapshot_digest"]) != snapshot.SnapshotDigest || anyToString(row.Edge.Provenance["document_set_digest"]) != snapshot.SnapshotDigest {
			t.Fatalf("repair append provenance was not bound to the exact source snapshot: %#v", row.Edge.Provenance)
		}
	}
	_ = left
	_ = right
}

func TestMemoryGraphRepairRetirementUsesLatestDurableStateAfterRollback(t *testing.T) {
	snapshot := memoryGraphRepairSnapshot{SnapshotDigest: "sha256:snapshot", KeyGeneration: 7, TopicGeneration: 11}
	action := memoryGraphRepairAction{ActionID: "0:edge_stale", Kind: "retire", Edge: memoryEdgeEntry{EdgeID: "edge_stale"}}
	activeRestoration := memoryEdgeEntry{EdgeID: "edge_stale", SourceID: "alpha::notes/a.md", TargetID: "alpha::notes/b.md", Relation: "inferred_related", Lifecycle: "durable", Metadata: map[string]any{"inferred": true}}
	retiredRecovery := activeRestoration
	retiredRecovery.Lifecycle = "retired"
	retiredRecovery.Metadata = map[string]any{"repair_run_id": "run-repair", "repair_action_id": action.ActionID, "repair_snapshot_digest": snapshot.SnapshotDigest, "repair_plan_ref": "plan-ref", "repair_plan_digest": "plan-digest", "repair_action_digest": "action-digest", "repair_custody_digest": "custody-digest"}
	evidence := memoryGraphRepairEdgeEvidence{Latest: map[string]memoryEdgeEntry{"edge_stale": activeRestoration}, RetirementSeen: map[string]bool{"edge_stale": true}, RepairActionRows: map[string]map[string]memoryEdgeEntry{"run-repair": {action.ActionID: retiredRecovery}}}
	if memoryGraphRepairActionApplied(action, evidence, snapshot.SnapshotDigest, time.Time{}) {
		t.Fatalf("historical retirement was allowed to hide an append-only active restoration")
	}
	if _, recovered := memoryGraphRepairActionRecovered(action, evidence, "run-repair", "plan-ref", "plan-digest", "action-digest", "custody-digest"); recovered {
		t.Fatalf("crash recovery resurrected a superseded retirement append")
	}
	retired := activeRestoration
	retired.Lifecycle = "retired"
	retired.Metadata = map[string]any{"repair_snapshot_digest": snapshot.SnapshotDigest, "repair_action_id": action.ActionID}
	evidence.Latest[action.Edge.EdgeID] = retired
	if !memoryGraphRepairActionApplied(action, evidence, snapshot.SnapshotDigest, time.Time{}) {
		t.Fatalf("exact latest repair retirement was not recognized as idempotent")
	}
}

func TestMemoryGraphRepairStatusesAndOpaqueCustody(t *testing.T) {
	for code, want := range map[string]int{
		"plan_receipt_required": http.StatusPreconditionRequired,
		"plan_receipt_invalid":  http.StatusUnprocessableEntity,
		"plan_receipt_mismatch": http.StatusUnprocessableEntity,
		"plan_receipt_expired":  http.StatusUnprocessableEntity,
		"plan_receipt_conflict": http.StatusConflict,
		"repair_busy":           http.StatusConflict,
		"bounded_limit":         http.StatusUnprocessableEntity,
		"checkpoint_invalid":    http.StatusUnprocessableEntity,
	} {
		if got := memoryGraphRepairHTTPStatus(code); got != want || got < 400 || got >= 500 {
			t.Fatalf("repair code %q mapped to %d, want deterministic 4xx %d", code, got, want)
		}
	}
	principal, scope := memoryGraphRepairActorDigests("operator-secret", "alpha", "workspace-1\x00install-1")
	if principal == "" || scope == "" || strings.Contains(principal, "operator-secret") || strings.Contains(scope, "operator-secret") {
		t.Fatalf("actor custody digests leaked principal material: principal=%q scope=%q", principal, scope)
	}
	if principal == scope {
		t.Fatalf("principal and scope custody digests were not distinct")
	}
}

func TestMemoryGraphRepairAnonymousDryRunIsExplicitlyNonApplicable(t *testing.T) {
	t.Setenv("GO_GATEWAY_TEST_KEEP_ORCH_KEY", "true")
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "operator-test-key")
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/a.md", "runbooks/graph", "a", "", "")
	status, response := postRepair(t, gateway.URL, `{"project":"alpha","mode":"dry_run","include_inferred":false}`, "")
	if status != http.StatusOK {
		t.Fatalf("authenticated dry-run failed: status=%d response=%#v", status, response)
	}
	custody, ok := response["custody"].(map[string]any)
	if !ok || anyToString(custody["actor_principal_digest"]) != "" || anyToBool(response["plan_applicable"]) || anyToString(response["actor_authority"]) != "anonymous_preview_non_applicable" {
		t.Fatalf("anonymous preview was misattributed or made applicable: %#v", response)
	}
	if strings.Contains(anyToString(response["custody"]), "operator-test-key") || strings.Contains(anyToString(response["actor_authority"]), "operator-test-key") {
		t.Fatalf("dry-run custody leaked the authenticated key: %#v", response)
	}

	explicit, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/memory/edges/repair", strings.NewReader(`{"project":"alpha","mode":"dry_run","include_inferred":false}`))
	if err != nil {
		t.Fatalf("build explicit preview request: %v", err)
	}
	explicit.Header.Set("Content-Type", "application/json")
	explicit.Header.Set("X-Api-Key", "operator-test-key")
	explicitResponse, err := http.DefaultClient.Do(explicit)
	if err != nil {
		t.Fatalf("explicit preview request: %v", err)
	}
	defer explicitResponse.Body.Close()
	var attributed map[string]any
	if err := json.NewDecoder(explicitResponse.Body).Decode(&attributed); err != nil {
		t.Fatalf("decode explicit preview response: %v", err)
	}
	expectedPrincipal, _ := memoryGraphRepairActorDigests("operator-test-key", "alpha", "preview")
	attributedCustody := anyMap(attributed["custody"])
	if explicitResponse.StatusCode != http.StatusOK || anyToString(attributedCustody["actor_principal_digest"]) != expectedPrincipal || anyToBool(attributed["plan_applicable"]) {
		t.Fatalf("explicit preview was not attributed to the actual authenticated principal: status=%d response=%#v", explicitResponse.StatusCode, attributed)
	}
}

func TestMemoryGraphRepairRejectsIncompleteIndexAndKeepsProjectsScoped(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/a.md", "runbooks/alpha", "alpha", "", "")
	seedMemoryGraphRepairDoc(t, store.memoryStore, "beta", "notes/a.md", "runbooks/beta", "beta", "", "")
	seedMemoryGraphRepairDoc(t, store.memoryStore, "beta", "notes/b.md", "runbooks/beta", "beta", "", "")
	if _, err := store.memoryStore.upsertMemoryEdge(context.Background(), memoryEdgeEntry{
		SourceID: "beta::notes/a.md", TargetID: "beta::notes/b.md", Relation: "supports", Project: "beta", Confidence: 1,
		CreatedAt: nowUTCISO(), Lifecycle: "durable", Provenance: map[string]any{"kind": "beta_explicit"},
	}); err != nil {
		t.Fatalf("seed beta edge: %v", err)
	}
	store.memoryStore.mu.Lock()
	store.memoryStore.currentKeyCountsByProject["alpha"]++
	store.memoryStore.mu.Unlock()
	req := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true})
	if _, err := store.memoryStore.memoryGraphRepair(context.Background(), req); err == nil || !strings.Contains(err.Error(), "current-state/index count") {
		t.Fatalf("incomplete index snapshot was accepted: %v", err)
	}
	store.memoryStore.mu.Lock()
	store.memoryStore.currentKeyCountsByProject["alpha"]--
	store.memoryStore.mu.Unlock()
	status, response := postRepair(t, gateway.URL, `{"project":"alpha","mode":"dry_run","include_inferred":false}`, "")
	if status != http.StatusOK || anyToInt(response["indexed_doc_count"], 0) != 1 {
		t.Fatalf("project-scoped repair saw another project: status=%d response=%#v", status, response)
	}
	if anyToInt(response["bound_edge_count"], -1) != 0 || anyToInt(response["actions"], -1) != 0 {
		t.Fatalf("project-scoped repair imported beta edge evidence: %#v", response)
	}
	if anyToInt(response["isolated_doc_count"], -1) != 1 {
		t.Fatalf("project-scoped telemetry was not honest: %#v", response)
	}
	if _, exists := response["checkpoint_path"]; exists {
		t.Fatalf("dry-run exposed a private checkpoint path: %#v", response["checkpoint_path"])
	}
}

func TestMemoryGraphRepairPlanReceiptGuardsAndAmbiguousAliases(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/a.md", "runbooks/graph", "a", "", "")
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/b.md", "runbooks/graph", "b", "", "")
	req := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": false})
	plan, err := store.memoryStore.memoryGraphRepair(context.Background(), req)
	if err != nil {
		t.Fatalf("dry-run plan: %v", err)
	}
	apply := req
	apply.DryRun, apply.Apply = false, true
	if _, err := store.memoryStore.memoryGraphRepair(context.Background(), apply); err == nil || !strings.Contains(err.Error(), "plan receipt") {
		t.Fatalf("apply without plan receipt was accepted: %v", err)
	}
	apply.PlanReceiptRef = anyToString(plan["plan_receipt_ref"])
	apply.PlanReceiptDigest = anyToString(plan["plan_receipt_digest"])
	apply.CheckpointID = "repair_forged_checkpoint"
	if _, err := store.memoryStore.memoryGraphRepair(context.Background(), apply); err == nil || !strings.Contains(err.Error(), "checkpoint") {
		t.Fatalf("forged checkpoint started a new apply: %v", err)
	}
	apply.CheckpointID = ""
	apply.PlanReceiptDigest = "sha256:" + strings.Repeat("0", 64)
	if _, err := store.memoryStore.memoryGraphRepair(context.Background(), apply); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("forged plan receipt digest was accepted: %v", err)
	}
	receipt, _, err := store.memoryStore.loadMemoryGraphRepairPlanReceipt(memoryGraphRepairRequest{Project: "alpha", PlanReceiptRef: anyToString(plan["plan_receipt_ref"]), PlanReceiptDigest: anyToString(plan["plan_receipt_digest"])}, false)
	if err != nil {
		t.Fatalf("load plan receipt: %v", err)
	}
	receipt.ExpiresAt = "2000-01-01T00:00:00Z"
	if err := writeOwnerOnlyDurableAtomicFile(memoryGraphRepairPlanReceiptPath(store.memoryStore, receipt.ReceiptRef), append(mustJSON(receipt), '\n'), true); err != nil {
		t.Fatalf("forge expired fixture: %v", err)
	}
	apply.PlanReceiptDigest = memoryGraphRepairPlanReceiptDigest(receipt)
	if _, err := store.memoryStore.memoryGraphRepair(context.Background(), apply); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired plan receipt was accepted: %v", err)
	}
	aliases := memoryGraphRepairAliasIndex{Aliases: map[string]string{}, AmbiguousAliases: map[string]struct{}{}}
	aliases.add("legacy-id", "alpha::notes/a.md")
	aliases.add("legacy-id", "alpha::notes/b.md")
	if got, status := aliases.resolve("legacy-id"); got != "" || status != "ambiguous" {
		t.Fatalf("ambiguous alias was treated as bound: %q/%s", got, status)
	}
}

func TestMemoryGraphRepairRejectsSnapshotDriftAfterDryRun(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/a.md", "runbooks/graph", "a", "", "")
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/b.md", "runbooks/graph", "b", "", "")
	req := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": false})
	plan, err := store.memoryStore.memoryGraphRepair(context.Background(), req)
	if err != nil {
		t.Fatalf("dry-run plan: %v", err)
	}
	store.memoryStore.mu.Lock()
	store.memoryStore.currentKeyIndexGeneration["alpha"]++
	store.memoryStore.currentTopicIndexGeneration["alpha"]++
	store.memoryStore.mu.Unlock()
	apply := req
	apply.DryRun, apply.Apply = false, true
	apply.PlanReceiptRef, apply.PlanReceiptDigest = anyToString(plan["plan_receipt_ref"]), anyToString(plan["plan_receipt_digest"])
	if _, err := store.memoryStore.memoryGraphRepair(context.Background(), apply); err == nil || !strings.Contains(err.Error(), "plan") {
		t.Fatalf("apply crossed current-state generation drift: %v", err)
	}
}

func TestMemoryGraphRepairRejectsIncompleteEdgeTailAndPreservesExplicitEvidence(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/a.md", "runbooks/graph", "a", "", "")
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/b.md", "runbooks/graph", "b", "", "")
	explicit := memoryEdgeEntry{SourceID: "alpha::notes/a.md", TargetID: "alpha::notes/b.md", Relation: "supports", Project: "alpha", Confidence: 1, CreatedAt: "2020-01-01T00:00:00Z", Lifecycle: "durable", Provenance: map[string]any{"kind": "operator_explicit"}}
	explicitStored, err := store.memoryStore.upsertMemoryEdge(context.Background(), explicit)
	if err != nil {
		t.Fatalf("seed explicit edge: %v", err)
	}
	req := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": false})
	snapshot, err := store.memoryStore.captureMemoryGraphRepairSnapshot(context.Background(), req)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	store.memoryStore.policy.edgeStartupMaxLines = 1
	if _, err := store.memoryStore.upsertMemoryEdge(context.Background(), memoryEdgeEntry{SourceID: "alpha::notes/b.md", TargetID: "alpha::notes/a.md", Relation: "supports", Project: "alpha", Confidence: 0.95, CreatedAt: nowUTCISO(), Lifecycle: "durable"}); err != nil {
		t.Fatalf("seed second edge: %v", err)
	}
	if _, err := store.memoryStore.captureMemoryGraphRepairEdges(snapshot); err == nil || !strings.Contains(err.Error(), "bounded cap") {
		t.Fatalf("incomplete edge tail was accepted: %v", err)
	}
	store.memoryStore.policy.edgeStartupMaxLines = 100
	evidence, err := store.memoryStore.captureMemoryGraphRepairEdges(snapshot)
	if err != nil {
		t.Fatalf("complete edge evidence: %v", err)
	}
	if got := evidence.Latest[explicitStored.EdgeID]; memoryGraphRepairEdgeIsInferred(got) || memoryGraphRepairEdgeRetired(got) {
		t.Fatalf("explicit evidence was changed by repair planning: %#v", got)
	}
	if status, response := postRepair(t, gateway.URL, `{"project":"alpha","mode":"dry_run","include_inferred":false}`, ""); status != http.StatusOK || anyToInt(response["bound_edge_count"], 0) != 2 {
		t.Fatalf("honest bound telemetry missing: status=%d response=%#v", status, response)
	}
}

func TestMemoryGraphRepairRejectsMalformedEdgeEvidence(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/a.md", "runbooks/graph", "a", "", "")
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/b.md", "runbooks/graph", "b", "", "")
	file, err := openOwnerOnlyAppend(store.memoryStore.policy.edgePath, true)
	if err != nil {
		t.Fatalf("open malformed edge fixture: %v", err)
	}
	if _, err := file.WriteString("{not-json}\n"); err != nil {
		_ = file.Close()
		t.Fatalf("write malformed edge fixture: %v", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		t.Fatalf("sync malformed edge fixture: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close malformed edge fixture: %v", err)
	}
	req := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true})
	if _, err := store.memoryStore.memoryGraphRepair(context.Background(), req); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("malformed edge evidence was accepted: %v", err)
	}
}

func TestMemoryGraphRepairConcurrentApplyAndCrashRecovery(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/a.md", "runbooks/graph", "a", "", "")
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/b.md", "runbooks/graph", "b", "", "")
	req := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": false, "topic_peer_limit": 1, "chunk_size": 1})
	plan, err := store.memoryStore.memoryGraphRepair(context.Background(), req)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	apply := req
	apply.DryRun, apply.Apply = false, true
	apply.PlanReceiptRef, apply.PlanReceiptDigest = anyToString(plan["plan_receipt_ref"]), anyToString(plan["plan_receipt_digest"])
	var wg sync.WaitGroup
	results := make([]error, 2)
	responses := make([]map[string]any, 2)
	for i := range results {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			responses[index], results[index] = store.memoryStore.memoryGraphRepair(context.Background(), apply)
		}(i)
	}
	wg.Wait()
	successes := 0
	nonIdempotent := 0
	for index, resultErr := range results {
		if resultErr == nil {
			successes++
			if !anyToBool(responses[index]["idempotent"]) {
				nonIdempotent++
			}
		}
	}
	if successes != 1 || nonIdempotent != 1 {
		t.Fatalf("concurrent apply did not fence to one writer: errors=%#v responses=%#v", results, responses)
	}

	// Recreate a running checkpoint with cursor zero, then append the first
	// action as if the process crashed after the durable edge fsync. Resume must
	// recognize the run/action receipt embedded in that row and avoid a duplicate.
	store2, gateway2 := newMemoryGraphTestServer(t, true)
	defer gateway2.Close()
	seedMemoryGraphRepairDoc(t, store2.memoryStore, "alpha", "notes/a.md", "runbooks/graph", "a", "", "")
	seedMemoryGraphRepairDoc(t, store2.memoryStore, "alpha", "notes/b.md", "runbooks/graph", "b", "", "")
	dryReq := repairRequest(t, store2.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": false, "topic_peer_limit": 1, "chunk_size": 1})
	dry, err := store2.memoryStore.memoryGraphRepair(context.Background(), dryReq)
	if err != nil {
		t.Fatalf("crash dry-run: %v", err)
	}
	snap, err := store2.memoryStore.captureMemoryGraphRepairSnapshot(context.Background(), dryReq)
	if err != nil {
		t.Fatalf("crash snapshot: %v", err)
	}
	ev, err := store2.memoryStore.captureMemoryGraphRepairEdges(snap)
	if err != nil {
		t.Fatalf("crash evidence: %v", err)
	}
	planReq := dryReq
	planReq.PlanReceiptRef, planReq.PlanReceiptDigest = anyToString(dry["plan_receipt_ref"]), anyToString(dry["plan_receipt_digest"])
	planReceipt, planDigest, err := store2.memoryStore.loadMemoryGraphRepairPlanReceipt(planReq, true)
	if err != nil || len(planReceipt.Actions) == 0 {
		t.Fatalf("crash plan: %v actions=%d", err, len(planReceipt.Actions))
	}
	acts := planReceipt.Actions
	keyGen, topicGen, _ := store2.memoryStore.currentMemoryGraphRepairGeneration("alpha")
	checkpoint := memoryGraphRepairCheckpoint{SchemaID: memoryGraphRepairCheckpointID, Version: 1, CheckpointID: "repair_crash_fixture", RunID: "run_crash_fixture", Project: "alpha", PlanReceiptRef: planReceipt.ReceiptRef, PlanReceiptDigest: planDigest, SnapshotDigest: snap.SnapshotDigest, PolicyDigest: planReceipt.PolicyDigest, ActionDigest: planReceipt.ActionDigest, StaleAfter: planReceipt.StaleAfter, ObservedAt: planReceipt.ObservedAt, EdgeDigestBefore: ev.Digest, EdgeDigestAfter: ev.Digest, KeyGeneration: keyGen, TopicGeneration: topicGen, EdgeLogGeneration: ev.LogGeneration, EdgeLogDigest: ev.LogDigest, EdgeLogContentDigest: ev.LogContentDigest, TotalActions: len(acts), Counts: map[string]int{"plan_actions": len(acts)}, Status: "running", ActorPrincipalDigest: dryReq.ActorPrincipalDigest, ActorScopeDigest: dryReq.ActorScopeDigest, ActorCustodyDigest: dryReq.ActorCustodyDigest}
	if err := store2.memoryStore.writeMemoryGraphRepairCheckpoint(checkpoint); err != nil {
		t.Fatalf("write crash checkpoint: %v", err)
	}
	crashed := acts[0].Edge
	crashed.Metadata = cloneJSONMap(crashed.Metadata)
	crashed.Metadata["repair_run_id"] = checkpoint.RunID
	crashed.Metadata["repair_action_id"] = acts[0].ActionID
	crashed.Metadata["repair_snapshot_digest"] = snap.SnapshotDigest
	crashed.Metadata["repair_plan_ref"] = planReceipt.ReceiptRef
	crashed.Metadata["repair_plan_digest"] = planDigest
	crashed.Metadata["repair_action_digest"] = planReceipt.ActionDigest
	crashed.Metadata["repair_custody_digest"] = dryReq.ActorCustodyDigest
	crashed.Metadata["repair_prior_row_digest"] = memoryGraphRepairOptionalEdgeDigest(acts[0].Previous)
	crashed.Metadata["repair_cursor"] = 0
	crashed.Metadata["repair_edge_log_generation_before"] = ev.LogGeneration
	crashed.Metadata["repair_edge_log_digest_before"] = ev.LogDigest
	crashed.Metadata["repair_edge_log_content_digest_before"] = ev.LogContentDigest
	crashed.Metadata["repair_server_append_marker"] = memoryGraphRepairServerAppendMarker("repair", crashed)
	beforeForgery, err := store2.memoryStore.snapshotMemoryEdgeLog(0)
	if err != nil {
		t.Fatalf("snapshot before ordinary crash-row forgery: %v", err)
	}
	if _, err := store2.memoryStore.upsertMemoryEdge(context.Background(), crashed); err == nil || !strings.Contains(err.Error(), "reserved server repair namespace") {
		t.Fatalf("ordinary edge write accepted a forged repair recovery row: %v", err)
	}
	afterForgery, err := store2.memoryStore.snapshotMemoryEdgeLog(0)
	if err != nil {
		t.Fatalf("snapshot after ordinary crash-row forgery: %v", err)
	}
	if afterForgery.Generation != beforeForgery.Generation || afterForgery.Digest != beforeForgery.Digest || !bytes.Equal(afterForgery.Bytes, beforeForgery.Bytes) {
		t.Fatal("rejected ordinary repair-row forgery mutated durable edge state")
	}
	if err := store2.memoryStore.appendMemoryGraphRepairEdge(crashed); err != nil {
		t.Fatalf("append crash edge: %v", err)
	}
	resume := dryReq
	resume.DryRun, resume.Apply = false, true
	resume.CheckpointID = checkpoint.CheckpointID
	resume.PlanReceiptRef, resume.PlanReceiptDigest = anyToString(dry["plan_receipt_ref"]), anyToString(dry["plan_receipt_digest"])
	recovered, err := store2.memoryStore.memoryGraphRepair(context.Background(), resume)
	if err != nil {
		t.Fatalf("crash recovery rejected idempotent append: %v", err)
	}
	recoveredCounts, _ := recovered["counts"].(map[string]int)
	if recoveredCounts["applied_total"] != 1 || recoveredCounts["skipped_total"] != 0 {
		t.Fatalf("append-before-receipt recovery misclassified counts: %#v", recovered)
	}
	receiptPath := memoryGraphRepairReceiptPath(store2.memoryStore, anyToString(recovered["run_id"]), anyToInt(recovered["cursor"], 0), anyToString(recovered["edge_digest"]))
	receiptRaw, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatalf("read recovered receipt: %v", err)
	}
	var recoveredReceipt memoryGraphRepairReceipt
	if err := json.Unmarshal(receiptRaw, &recoveredReceipt); err != nil {
		t.Fatalf("decode recovered receipt: %v", err)
	}
	if recoveredReceipt.Applied != 1 || len(recoveredReceipt.RollbackReceipts) != recoveredReceipt.Applied {
		t.Fatalf("recovered receipt lost rollback coverage: %#v", recoveredReceipt)
	}

	// A second fixture injects a crash after the chunk receipt is durable but
	// before the checkpoint cursor advances. Replay must dedupe the append while
	// reconstructing the same applied/rollback evidence.
	store3, gateway3 := newMemoryGraphTestServer(t, true)
	defer gateway3.Close()
	seedMemoryGraphRepairDoc(t, store3.memoryStore, "alpha", "notes/a.md", "runbooks/graph", "a", "", "")
	seedMemoryGraphRepairDoc(t, store3.memoryStore, "alpha", "notes/b.md", "runbooks/graph", "b", "", "")
	dryReq3 := repairRequest(t, store3.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": false, "topic_peer_limit": 1, "chunk_size": 1})
	dry3, err := store3.memoryStore.memoryGraphRepair(context.Background(), dryReq3)
	if err != nil {
		t.Fatalf("receipt crash dry-run: %v", err)
	}
	apply3 := dryReq3
	apply3.DryRun, apply3.Apply = false, true
	apply3.PlanReceiptRef, apply3.PlanReceiptDigest = anyToString(dry3["plan_receipt_ref"]), anyToString(dry3["plan_receipt_digest"])
	store3.memoryStore.memoryGraphRepairBeforeCheckpoint = func() error { return errors.New("simulated crash after receipt") }
	if _, err := store3.memoryStore.memoryGraphRepair(context.Background(), apply3); err == nil || !strings.Contains(err.Error(), "simulated crash") {
		t.Fatalf("receipt-before-checkpoint crash was not injected: %v", err)
	}
	crashCheckpoint, exists, err := store3.memoryStore.loadMemoryGraphRepairCheckpoint("alpha")
	if err != nil || !exists || crashCheckpoint.Status != "running" {
		t.Fatalf("receipt crash did not leave resumable checkpoint: exists=%v status=%q err=%v", exists, crashCheckpoint.Status, err)
	}
	store3.memoryStore.memoryGraphRepairBeforeCheckpoint = nil
	apply3.CheckpointID = crashCheckpoint.CheckpointID
	replayed, err := store3.memoryStore.memoryGraphRepair(context.Background(), apply3)
	if err != nil {
		t.Fatalf("receipt-before-checkpoint replay failed: %v", err)
	}
	replayedCounts, _ := replayed["counts"].(map[string]int)
	if replayedCounts["applied_total"] != 1 || replayedCounts["skipped_total"] != 0 {
		t.Fatalf("receipt-before-checkpoint replay misclassified counts: %#v", replayed)
	}
	replayedReceiptPath := memoryGraphRepairReceiptPath(store3.memoryStore, anyToString(replayed["run_id"]), anyToInt(replayed["cursor"], 0), anyToString(replayed["edge_digest"]))
	replayedRaw, err := os.ReadFile(replayedReceiptPath)
	if err != nil {
		t.Fatalf("read replayed receipt: %v", err)
	}
	var replayedReceipt memoryGraphRepairReceipt
	if err := json.Unmarshal(replayedRaw, &replayedReceipt); err != nil {
		t.Fatalf("decode replayed receipt: %v", err)
	}
	if replayedReceipt.Applied != 1 || len(replayedReceipt.RollbackReceipts) != replayedReceipt.Applied {
		t.Fatalf("replayed receipt lost rollback coverage: %#v", replayedReceipt)
	}
	if len(replayedReceipt.BindingBefore) == 0 || len(replayedReceipt.BindingAfter) == 0 || !anyToBool(replayedReceipt.ResolutionProof["all_repaired_writes_bound"]) {
		t.Fatalf("replayed receipt lost endpoint-resolution custody: %#v", replayedReceipt)
	}
	if replayedReceipt.EdgeDigestBefore != anyToString(dry3["edge_digest"]) {
		t.Fatalf("replayed receipt changed the authoritative before-digest: got=%q want=%q", replayedReceipt.EdgeDigestBefore, anyToString(dry3["edge_digest"]))
	}
}

func TestMemoryEdgeLogFencePreservesConcurrentAppendAcrossCompactionReplace(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	first := memoryEdgeEntry{SourceID: "alpha::notes/a.md", TargetID: "alpha::notes/b.md", Relation: "references", Project: "alpha", Confidence: 1, CreatedAt: nowUTCISO(), Lifecycle: "durable"}
	if _, _, err := store.memoryStore.appendMemoryEdgeLog(first, true); err != nil {
		t.Fatalf("seed fenced edge log: %v", err)
	}
	secondHandle := &memoryStore{policy: store.memoryStore.policy}
	fence, err := store.memoryStore.acquireMemoryEdgeLogFence()
	if err != nil {
		t.Fatalf("lock edge log for compaction fixture: %v", err)
	}
	before, err := store.memoryStore.snapshotMemoryEdgeLogLocked(0)
	if err != nil {
		fence.release()
		t.Fatalf("snapshot edge log under fence: %v", err)
	}
	appendDone := make(chan error, 1)
	second := memoryEdgeEntry{SourceID: "alpha::notes/b.md", TargetID: "alpha::notes/c.md", Relation: "references", Project: "alpha", Confidence: 1, CreatedAt: nowUTCISO(), Lifecycle: "durable"}
	go func() {
		_, _, appendErr := secondHandle.appendMemoryEdgeLog(second, true)
		appendDone <- appendErr
	}()
	if _, err := store.memoryStore.replaceMemoryEdgeLogWithFenceLocked(before.Bytes, "fault_injected_compaction", fence); err != nil {
		fence.release()
		t.Fatalf("replace edge log under compaction fence: %v", err)
	}
	fence.release()
	if err := <-appendDone; err != nil {
		t.Fatalf("concurrent append after compaction fence: %v", err)
	}
	final, err := store.memoryStore.snapshotMemoryEdgeLog(0)
	if err != nil {
		t.Fatalf("capture final fenced log: %v", err)
	}
	if !bytes.Contains(final.Bytes, []byte(first.SourceID)) || !bytes.Contains(final.Bytes, []byte(second.TargetID)) || final.Generation < before.Generation+2 {
		t.Fatalf("compaction/append fence lost a row or generation: generation=%d log=%s", final.Generation, final.Bytes)
	}
}

func TestMemoryGraphRepairMaximumRequestUsesBoundedLockChunkAndResumesAfterWriterProgress(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	for index := 0; index < 140; index++ {
		seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", fmt.Sprintf("notes/chunk-%03d.md", index), "runbooks/bounded-lock", fmt.Sprintf("bounded lock document %03d", index), "", "")
	}
	dry := repairRequest(t, store.memoryStore, map[string]any{
		"project": "alpha", "dry_run": true, "include_inferred": false,
		"topic_peer_limit": 1, "chunk_size": 10000,
	})
	if dry.ChunkSize != memoryGraphRepairMaxLockActions {
		t.Fatalf("maximum caller chunk was not hard-capped: got=%d want=%d", dry.ChunkSize, memoryGraphRepairMaxLockActions)
	}
	preview, err := store.memoryStore.memoryGraphRepair(context.Background(), dry)
	if err != nil {
		t.Fatalf("bounded-lock preview: %v", err)
	}
	planReq := dry
	planReq.PlanReceiptRef, planReq.PlanReceiptDigest = anyToString(preview["plan_receipt_ref"]), anyToString(preview["plan_receipt_digest"])
	plan, _, err := store.memoryStore.loadMemoryGraphRepairPlanReceipt(planReq, true)
	if err != nil {
		t.Fatalf("load bounded-lock plan: %v", err)
	}
	if len(plan.Actions) <= memoryGraphRepairMaxLockActions {
		t.Fatalf("bounded-lock fixture did not exceed one lock chunk: actions=%d", len(plan.Actions))
	}
	apply := planReq
	apply.DryRun, apply.Apply = false, true
	first, err := store.memoryStore.memoryGraphRepair(context.Background(), apply)
	if err != nil {
		t.Fatalf("first bounded repair chunk: %v", err)
	}
	if cursor := anyToInt(first["cursor"], 0); cursor != memoryGraphRepairMaxLockActions || anyToString(first["status"]) != "running" {
		t.Fatalf("maximum request held more than the hard lock chunk: %#v", first)
	}
	writer := testMemoryEdgeLogEntry("beta::notes/writer-a.md", "beta::notes/writer-b.md")
	writer.Project = "beta"
	if _, err := store.memoryStore.upsertMemoryEdge(context.Background(), writer); err != nil {
		t.Fatalf("ordinary writer did not progress after bounded repair chunk: %v", err)
	}
	afterWriter, err := store.memoryStore.snapshotMemoryEdgeLog(0)
	if err != nil {
		t.Fatalf("snapshot after ordinary writer progress: %v", err)
	}
	continuation := apply
	continuation.CheckpointID = anyToString(first["checkpoint_id"])
	checkpointBefore, exists, err := store.memoryStore.loadMemoryGraphRepairCheckpoint("alpha", plan.ReceiptRef)
	if err != nil || !exists {
		t.Fatalf("load checkpoint before cancellation: exists=%v err=%v", exists, err)
	}
	canceledContext, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.memoryStore.memoryGraphRepair(canceledContext, continuation); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled continuation did not stop between durable chunks: %v", err)
	}
	checkpointAfter, exists, err := store.memoryStore.loadMemoryGraphRepairCheckpoint("alpha", plan.ReceiptRef)
	if err != nil || !exists || checkpointAfter.Cursor != checkpointBefore.Cursor || checkpointAfter.LastReceiptDigest != checkpointBefore.LastReceiptDigest {
		t.Fatalf("canceled continuation advanced durable checkpoint: before=%#v after=%#v exists=%v err=%v", checkpointBefore, checkpointAfter, exists, err)
	}
	second, err := store.memoryStore.memoryGraphRepair(context.Background(), continuation)
	if err != nil {
		t.Fatalf("continuation after ordinary writer progress: %v", err)
	}
	if advanced := anyToInt(second["cursor"], 0) - anyToInt(first["cursor"], 0); advanced < 1 || advanced > memoryGraphRepairMaxLockActions {
		t.Fatalf("continuation did not obey bounded progress: first=%#v second=%#v", first, second)
	}
	receiptPath := memoryGraphRepairReceiptPath(store.memoryStore, anyToString(second["run_id"]), anyToInt(second["cursor"], 0), anyToString(second["edge_digest"]))
	receiptRaw, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatalf("read continuation receipt: %v", err)
	}
	var receipt memoryGraphRepairReceipt
	if err := json.Unmarshal(receiptRaw, &receipt); err != nil {
		t.Fatalf("decode continuation receipt: %v", err)
	}
	if receipt.EdgeLogGenerationBefore != afterWriter.Generation || receipt.EdgeLogDigestBefore != afterWriter.Digest {
		t.Fatalf("continuation receipt did not bind fresh per-lock CAS state: receipt=%#v writer=%#v", receipt, afterWriter)
	}
}

func TestMemoryGraphRepairCanonicalizesFreshInferredBeforeRetirement(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/a.md", "runbooks/graph", "shared bounded inference", "", "")
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/b.md", "runbooks/graph", "shared bounded inference", "", "")
	fresh := nowUTCISO()
	bound, err := store.memoryStore.upsertMemoryEdge(context.Background(), memoryEdgeEntry{SourceID: "alpha::notes/a.md", TargetID: "alpha::notes/b.md", Relation: "inferred_related", Project: "alpha", Confidence: 0.95, CreatedAt: fresh, Lifecycle: "durable", Metadata: map[string]any{"inferred": true}})
	if err != nil {
		t.Fatalf("seed fresh bound inferred edge: %v", err)
	}
	unbound, err := store.memoryStore.upsertMemoryEdge(context.Background(), memoryEdgeEntry{SourceID: "alpha::missing/a.md", TargetID: "alpha::missing/b.md", Relation: "inferred_related", Project: "alpha", Confidence: 0.95, CreatedAt: fresh, Lifecycle: "durable", Metadata: map[string]any{"inferred": true}})
	if err != nil {
		t.Fatalf("seed fresh unbound inferred edge: %v", err)
	}
	req := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": true, "inferred_min_shared_terms": 1, "stale_after": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)})
	snapshot, _ := store.memoryStore.captureMemoryGraphRepairSnapshot(context.Background(), req)
	evidence, _ := store.memoryStore.captureMemoryGraphRepairEdges(snapshot)
	actions, _, err := store.memoryStore.buildMemoryGraphRepairPlan(context.Background(), snapshot, evidence, req)
	if err != nil {
		t.Fatalf("build canonical inferred plan: %v", err)
	}
	retired := map[string]bool{}
	for _, action := range actions {
		if action.Kind == "retire" {
			retired[action.Edge.EdgeID] = true
		}
	}
	if retired[bound.EdgeID] || !retired[unbound.EdgeID] {
		t.Fatalf("fresh inferred canonicalization retired wrong identities: bound=%v unbound=%v actions=%#v", retired[bound.EdgeID], retired[unbound.EdgeID], actions)
	}
}

func TestMemoryGraphRepairObservationTimeIsIndependentOfSourceDocumentAge(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	old := "2018-01-02T03:04:05Z"
	for _, file := range []string{"notes/a.md", "notes/b.md"} {
		entry, _, err := store.memoryStore.put(normalizedWrite{project: "alpha", fileName: file, topicPath: "runbooks/graph", content: "old source with current repair relation", lifecycle: "durable"})
		if err != nil {
			t.Fatalf("seed old source doc: %v", err)
		}
		key := memoryStoreKey(entry.Project, entry.FileName)
		store.memoryStore.mu.Lock()
		state := store.memoryStore.currentState[key]
		state.Entry.CreatedAt = old
		store.memoryStore.currentState[key] = state
		store.memoryStore.mu.Unlock()
	}
	observed := time.Now().UTC().Truncate(time.Microsecond)
	req := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": false, "topic_peer_limit": 1})
	req.ObservationAt = observed
	snapshot, _ := store.memoryStore.captureMemoryGraphRepairSnapshot(context.Background(), req)
	evidence, _ := store.memoryStore.captureMemoryGraphRepairEdges(snapshot)
	actions, _, err := store.memoryStore.buildMemoryGraphRepairPlan(context.Background(), snapshot, evidence, req)
	if err != nil || len(actions) == 0 {
		t.Fatalf("build observation-time plan: err=%v actions=%d", err, len(actions))
	}
	created, ok := parseTimeBestEffort(actions[0].Edge.CreatedAt)
	if !ok || !created.Equal(observed) || anyToString(actions[0].Edge.Provenance["source_document_latest_at"]) != old {
		t.Fatalf("repair evidence time reused source age: edge=%#v", actions[0].Edge)
	}
}

func TestMemoryGraphRepairPlanIdentityReplayReissueAndBoundedPendingPage(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	for _, file := range []string{"notes/a.md", "notes/b.md", "notes/c.md"} {
		seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", file, "runbooks/graph", "plan identity fixture", "agent", "session")
	}
	req := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": false, "topic_peer_limit": 2})
	first, err := store.memoryStore.memoryGraphRepair(context.Background(), req)
	if err != nil {
		t.Fatalf("first immutable plan: %v", err)
	}
	replay := req
	replay.PlanReceiptRef, replay.PlanReceiptDigest = anyToString(first["plan_receipt_ref"]), anyToString(first["plan_receipt_digest"])
	replayed, err := store.memoryStore.memoryGraphRepair(context.Background(), replay)
	if err != nil || !anyToBool(replayed["plan_replayed"]) || anyToString(replayed["plan_receipt_ref"]) != anyToString(first["plan_receipt_ref"]) {
		t.Fatalf("exact plan retry was not idempotent: err=%v response=%#v", err, replayed)
	}
	reissued, err := store.memoryStore.memoryGraphRepair(context.Background(), req)
	if err != nil || anyToString(reissued["plan_receipt_ref"]) == anyToString(first["plan_receipt_ref"]) {
		t.Fatalf("new observation did not receive a new complete plan identity: err=%v first=%#v second=%#v", err, first, reissued)
	}
	identity := memoryGraphRepairPlanReceiptRef("alpha", "snapshot", "edge", "action", "policy", "custody", "observed")
	for name, changed := range map[string]string{
		"project":     memoryGraphRepairPlanReceiptRef("beta", "snapshot", "edge", "action", "policy", "custody", "observed"),
		"snapshot":    memoryGraphRepairPlanReceiptRef("alpha", "snapshot-2", "edge", "action", "policy", "custody", "observed"),
		"edge":        memoryGraphRepairPlanReceiptRef("alpha", "snapshot", "edge-2", "action", "policy", "custody", "observed"),
		"action":      memoryGraphRepairPlanReceiptRef("alpha", "snapshot", "edge", "action-2", "policy", "custody", "observed"),
		"policy":      memoryGraphRepairPlanReceiptRef("alpha", "snapshot", "edge", "action", "policy-2", "custody", "observed"),
		"custody":     memoryGraphRepairPlanReceiptRef("alpha", "snapshot", "edge", "action", "policy", "custody-2", "observed"),
		"observation": memoryGraphRepairPlanReceiptRef("alpha", "snapshot", "edge", "action", "policy", "custody", "observed-2"),
	} {
		if changed == identity {
			t.Fatalf("complete plan identity ignored changed %s", name)
		}
	}
	actions := make([]memoryGraphRepairAction, 200)
	for index := range actions {
		actions[index] = memoryGraphRepairAction{ActionID: fmt.Sprintf("action-%03d", index)}
	}
	plan := memoryGraphRepairPlanReceipt{ReceiptRef: "plan_bounded", ActionDigest: memoryGraphRepairActionDigest(actions), Actions: actions}
	pending := memoryGraphRepairPendingActions(plan, 0)
	_, leaked := pending["pending_action_ids"]
	if anyToInt(pending["page_size"], 0) != 64 || len(anyToStringSlice(pending["action_refs"])) != 64 || anyToString(pending["next_page_ref"]) == "" || leaked {
		t.Fatalf("pending action response was not bounded by digest/ref/page: %#v", pending)
	}
}

func TestMemoryGraphRepairAuthenticatedRollbackAndSupersedingWriteRefusal(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/a.md", "runbooks/graph", "rollback a", "", "")
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/b.md", "runbooks/graph", "rollback b", "", "")
	dryReq := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": false, "topic_peer_limit": 1, "chunk_size": 100})
	plan, err := store.memoryStore.memoryGraphRepair(context.Background(), dryReq)
	if err != nil {
		t.Fatalf("rollback dry plan: %v", err)
	}
	apply := dryReq
	apply.DryRun, apply.Apply = false, true
	apply.PlanReceiptRef, apply.PlanReceiptDigest = anyToString(plan["plan_receipt_ref"]), anyToString(plan["plan_receipt_digest"])
	applied, err := store.memoryStore.memoryGraphRepair(context.Background(), apply)
	if err != nil || anyToString(applied["status"]) != "complete" {
		t.Fatalf("rollback fixture apply: err=%v result=%#v", err, applied)
	}
	rollback := apply
	rollback.Apply, rollback.Rollback = false, true
	rollback.CheckpointID = anyToString(applied["checkpoint_id"])
	rolled, err := store.memoryStore.memoryGraphRepair(context.Background(), rollback)
	if err != nil || anyToString(rolled["status"]) != "complete" || !anyToBool(rolled["rollback"]) {
		t.Fatalf("authenticated rollback failed: err=%v result=%#v", err, rolled)
	}
	replayed, err := store.memoryStore.memoryGraphRepair(context.Background(), rollback)
	if err != nil || !anyToBool(replayed["idempotent"]) {
		t.Fatalf("completed rollback did not replay idempotently: err=%v result=%#v", err, replayed)
	}

	store2, gateway2 := newMemoryGraphTestServer(t, true)
	defer gateway2.Close()
	seedMemoryGraphRepairDoc(t, store2.memoryStore, "alpha", "notes/a.md", "runbooks/graph", "supersede a", "", "")
	seedMemoryGraphRepairDoc(t, store2.memoryStore, "alpha", "notes/b.md", "runbooks/graph", "supersede b", "", "")
	dry2 := repairRequest(t, store2.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": false, "topic_peer_limit": 1, "chunk_size": 100})
	plan2, _ := store2.memoryStore.memoryGraphRepair(context.Background(), dry2)
	apply2 := dry2
	apply2.DryRun, apply2.Apply = false, true
	apply2.PlanReceiptRef, apply2.PlanReceiptDigest = anyToString(plan2["plan_receipt_ref"]), anyToString(plan2["plan_receipt_digest"])
	applied2, err := store2.memoryStore.memoryGraphRepair(context.Background(), apply2)
	if err != nil {
		t.Fatalf("superseding rollback apply: %v", err)
	}
	receipt2, _, _ := store2.memoryStore.loadMemoryGraphRepairPlanReceipt(apply2, true)
	changed := receipt2.Actions[0].Edge
	changed.Metadata = map[string]any{"operator_superseding_write": true}
	changed.Provenance = map[string]any{"kind": "operator_superseding_write"}
	changed.CreatedAt = nowUTCISO()
	if _, err := store2.memoryStore.upsertMemoryEdge(context.Background(), changed); err != nil {
		t.Fatalf("append superseding edge: %v", err)
	}
	before, _ := os.Stat(store2.memoryStore.policy.edgePath)
	rollback2 := apply2
	rollback2.Apply, rollback2.Rollback, rollback2.CheckpointID = false, true, anyToString(applied2["checkpoint_id"])
	if _, err := store2.memoryStore.memoryGraphRepair(context.Background(), rollback2); err == nil || !strings.Contains(err.Error(), "superseding") {
		t.Fatalf("rollback accepted a superseding durable write: %v", err)
	}
	after, _ := os.Stat(store2.memoryStore.policy.edgePath)
	if after.Size() != before.Size() {
		t.Fatalf("superseding-write refusal appended partial rollback evidence: before=%d after=%d", before.Size(), after.Size())
	}
}

func TestMemoryGraphRepairRollbackCrashReplayAndCustodyMismatch(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/a.md", "runbooks/graph", "crash rollback a", "", "")
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/b.md", "runbooks/graph", "crash rollback b", "", "")
	dry := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": false, "topic_peer_limit": 1, "chunk_size": 100})
	plan, _ := store.memoryStore.memoryGraphRepair(context.Background(), dry)
	apply := dry
	apply.DryRun, apply.Apply = false, true
	apply.PlanReceiptRef, apply.PlanReceiptDigest = anyToString(plan["plan_receipt_ref"]), anyToString(plan["plan_receipt_digest"])
	applied, err := store.memoryStore.memoryGraphRepair(context.Background(), apply)
	if err != nil {
		t.Fatalf("crash rollback apply: %v", err)
	}
	planReq := apply
	planReceipt, planDigest, err := store.memoryStore.loadMemoryGraphRepairPlanReceipt(planReq, true)
	if err != nil || len(planReceipt.Actions) == 0 {
		t.Fatalf("load rollback-forgery plan: err=%v actions=%d", err, len(planReceipt.Actions))
	}
	applyCheckpoint, exists, err := store.memoryStore.loadMemoryGraphRepairCheckpoint("alpha", planReceipt.ReceiptRef)
	if err != nil || !exists {
		t.Fatalf("load rollback-forgery apply checkpoint: exists=%v err=%v", exists, err)
	}
	rollbackSeed := planReceipt.ReceiptRef + "\x00" + applyCheckpoint.RunID + "\x00" + apply.ActorCustodyDigest
	forgedRollback := planReceipt.Actions[0].Edge
	forgedRollback.Metadata = cloneJSONMap(forgedRollback.Metadata)
	if forgedRollback.Metadata == nil {
		forgedRollback.Metadata = map[string]any{}
	}
	forgedRollback.Metadata["rollback_run_id"] = "rollback_run_" + sha256Hex(rollbackSeed)[:32]
	forgedRollback.Metadata["rollback_action_id"] = planReceipt.Actions[0].ActionID
	forgedRollback.Metadata["rollback_cursor"] = 0
	forgedRollback.Metadata["rollback_plan_ref"] = planReceipt.ReceiptRef
	forgedRollback.Metadata["rollback_plan_digest"] = planDigest
	forgedRollback.Metadata["rollback_apply_run_id"] = applyCheckpoint.RunID
	forgedRollback.Metadata["rollback_custody_digest"] = apply.ActorCustodyDigest
	forgedRollback.Metadata["rollback_server_append_marker"] = memoryGraphRepairServerAppendMarker("rollback", forgedRollback)
	if !memoryGraphRepairServerAppendMarkerValid("rollback", forgedRollback) {
		t.Fatal("rollback forgery fixture does not carry an exact marker")
	}
	beforeForgery, err := store.memoryStore.snapshotMemoryEdgeLog(0)
	if err != nil {
		t.Fatalf("snapshot before ordinary rollback forgery: %v", err)
	}
	if _, err := store.memoryStore.upsertMemoryEdge(context.Background(), forgedRollback); err == nil || !strings.Contains(err.Error(), "reserved server repair namespace") {
		t.Fatalf("ordinary edge write accepted a forged rollback recovery row: %v", err)
	}
	afterForgery, err := store.memoryStore.snapshotMemoryEdgeLog(0)
	if err != nil {
		t.Fatalf("snapshot after ordinary rollback forgery: %v", err)
	}
	if afterForgery.Generation != beforeForgery.Generation || afterForgery.Digest != beforeForgery.Digest || !bytes.Equal(afterForgery.Bytes, beforeForgery.Bytes) {
		t.Fatal("rejected ordinary rollback-row forgery mutated durable edge state")
	}
	wrong := apply
	wrong.ActorCustodyDigest = "sha256:" + strings.Repeat("9", 64)
	wrong.CheckpointID = anyToString(applied["checkpoint_id"])
	if _, err := store.memoryStore.memoryGraphRepair(context.Background(), wrong); err == nil || !strings.Contains(err.Error(), "custody") {
		t.Fatalf("apply continuation accepted mismatched custody: %v", err)
	}
	rollback := apply
	rollback.Apply, rollback.Rollback, rollback.CheckpointID = false, true, anyToString(applied["checkpoint_id"])
	store.memoryStore.memoryGraphRepairBeforeRollbackCheckpoint = func() error { return errors.New("simulated rollback crash after receipt") }
	if _, err := store.memoryStore.memoryGraphRepair(context.Background(), rollback); err == nil || !strings.Contains(err.Error(), "simulated rollback crash") {
		t.Fatalf("rollback crash seam did not fire: %v", err)
	}
	store.memoryStore.memoryGraphRepairBeforeRollbackCheckpoint = nil
	replayed, err := store.memoryStore.memoryGraphRepair(context.Background(), rollback)
	if err != nil || anyToString(replayed["status"]) != "complete" {
		t.Fatalf("rollback crash did not replay immutable receipt/checkpoint: err=%v result=%#v", err, replayed)
	}
}

func TestMemoryGraphRepairRollbackCrashReplaysPairedRetireWriteActions(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/a.md", "runbooks/graph", "paired rollback shared graph token", "", "")
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/b.md", "runbooks/graph", "paired rollback shared graph token", "", "")
	stale, err := store.memoryStore.upsertMemoryEdge(context.Background(), memoryEdgeEntry{
		SourceID: "alpha::notes/a.md", TargetID: "alpha::notes/b.md", Relation: "inferred_related", Project: "alpha",
		Confidence: 0.95, CreatedAt: "2020-01-01T00:00:00Z", Lifecycle: "durable", Metadata: map[string]any{"inferred": true},
		Provenance: map[string]any{"kind": "inferred_memory_edge_scoring", "version": "pre_repair"},
	})
	if err != nil {
		t.Fatalf("seed paired stale edge: %v", err)
	}
	dry := repairRequest(t, store.memoryStore, map[string]any{
		"project": "alpha", "dry_run": true, "include_inferred": true, "inferred_min_shared_terms": 1,
		"inferred_min_score": 0.6, "stale_after": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano), "chunk_size": 100,
	})
	preview, err := store.memoryStore.memoryGraphRepair(context.Background(), dry)
	if err != nil {
		t.Fatalf("paired rollback plan: %v", err)
	}
	planReq := dry
	planReq.PlanReceiptRef, planReq.PlanReceiptDigest = anyToString(preview["plan_receipt_ref"]), anyToString(preview["plan_receipt_digest"])
	plan, _, err := store.memoryStore.loadMemoryGraphRepairPlanReceipt(planReq, true)
	if err != nil {
		t.Fatalf("load paired rollback plan: %v", err)
	}
	paired := 0
	for _, action := range plan.Actions {
		if action.Edge.EdgeID == stale.EdgeID {
			paired++
		}
	}
	if paired != 2 {
		t.Fatalf("fixture did not produce paired retire/write actions: %#v", plan.Actions)
	}
	apply := dry
	apply.DryRun, apply.Apply = false, true
	apply.PlanReceiptRef, apply.PlanReceiptDigest = planReq.PlanReceiptRef, planReq.PlanReceiptDigest
	applied, err := store.memoryStore.memoryGraphRepair(context.Background(), apply)
	if err != nil {
		t.Fatalf("apply paired repair: %v", err)
	}
	rollback := apply
	rollback.Apply, rollback.Rollback, rollback.CheckpointID = false, true, anyToString(applied["checkpoint_id"])
	store.memoryStore.memoryGraphRepairBeforeRollbackCheckpoint = func() error { return errors.New("simulated paired rollback checkpoint crash") }
	if _, err := store.memoryStore.memoryGraphRepair(context.Background(), rollback); err == nil || !strings.Contains(err.Error(), "paired rollback checkpoint crash") {
		t.Fatalf("paired rollback crash seam did not fire: %v", err)
	}
	store.memoryStore.memoryGraphRepairBeforeRollbackCheckpoint = nil
	replayed, err := store.memoryStore.memoryGraphRepair(context.Background(), rollback)
	if err != nil || anyToString(replayed["status"]) != "complete" {
		t.Fatalf("paired rollback did not replay exact durable rows: err=%v result=%#v", err, replayed)
	}
	snapshot, _ := store.memoryStore.captureMemoryGraphRepairSnapshot(context.Background(), dry)
	evidence, _ := store.memoryStore.captureMemoryGraphRepairEdges(snapshot)
	latest := evidence.Latest[stale.EdgeID]
	if memoryGraphRepairEdgeRetired(latest) || anyToString(latest.Provenance["version"]) != "pre_repair" || anyToString(latest.Metadata["rollback_run_id"]) == "" {
		t.Fatalf("paired rollback did not restore the valid pre-repair row: %#v", latest)
	}
}

func TestMemoryGraphRepairSnapshotGenerationRevalidationAndErrorClasses(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/a.md", "runbooks/graph", "snapshot", "", "")
	req := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true})
	store.memoryStore.memoryGraphRepairSnapshotCopied = func() {
		store.memoryStore.mu.Lock()
		store.memoryStore.currentKeyIndexGeneration["alpha"]++
		store.memoryStore.currentTopicIndexGeneration["alpha"]++
		store.memoryStore.mu.Unlock()
	}
	if _, err := store.memoryStore.captureMemoryGraphRepairSnapshot(context.Background(), req); err == nil || !strings.Contains(err.Error(), "changed during snapshot traversal") {
		t.Fatalf("snapshot generation revalidation did not fail closed: %v", err)
	}
	store.memoryStore.memoryGraphRepairSnapshotCopied = nil
	if got := memoryGraphRepairHTTPStatus("repair_busy"); got != http.StatusConflict {
		t.Fatalf("owner lock contention status=%d", got)
	}
	for _, code := range []string{"repair_lock_io", "edge_log_io", "checkpoint_io", "receipt_io", "plan_receipt_io", "rollback_io"} {
		if got := memoryGraphRepairHTTPStatus(code); got < 500 {
			t.Fatalf("non-contention storage error %s collapsed to a 4xx: %d", code, got)
		}
	}
}

func TestMemoryGraphRepairRefreshesStaleStructuredExplicitBinding(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	targetV1, _, err := store.memoryStore.put(normalizedWrite{
		project: "alpha", fileName: "notes/repair-reference-target.md", content: "target v1", topicPath: "runbooks/repair-reference",
	})
	if err != nil {
		t.Fatalf("seed structured repair target v1: %v", err)
	}
	source, _, err := store.memoryStore.put(normalizedWrite{
		project: "alpha", fileName: "notes/repair-reference-source.md", content: "source", topicPath: "runbooks/repair-reference",
		references: []memoryStructuredReference{{TargetID: "alpha::notes/repair-reference-target.md", Relation: "references", Confidence: 1}},
	})
	if err != nil {
		t.Fatalf("seed structured repair source: %v", err)
	}
	edges, err := store.memoryStore.listMemoryEdges(context.Background(), memoryEdgeQuery{MemoryID: source.Project + "::" + source.FileName, Relation: "references", Limit: 10})
	if err != nil || len(edges) != 1 || edges[0].Binding == nil || edges[0].Binding.TargetEventID != targetV1.EventID {
		t.Fatalf("load structured repair v1 edge: edges=%#v err=%v", edges, err)
	}
	stale := edges[0]
	targetV2, _, err := store.memoryStore.put(normalizedWrite{
		project: "alpha", fileName: "notes/repair-reference-target.md", content: "target v2", topicPath: "runbooks/repair-reference",
	})
	if err != nil {
		t.Fatalf("advance structured repair target: %v", err)
	}
	if visible, listErr := store.memoryStore.listMemoryEdges(context.Background(), memoryEdgeQuery{MemoryID: source.Project + "::" + source.FileName, Relation: "references", Limit: 10}); listErr != nil || len(visible) != 0 {
		t.Fatalf("stale structured binding remained ordinarily visible: edges=%#v err=%v", visible, listErr)
	}
	dry := repairRequest(t, store.memoryStore, map[string]any{
		"project": "alpha", "dry_run": true, "include_inferred": false, "topic_peer_limit": 1,
	})
	preview, err := store.memoryStore.memoryGraphRepair(context.Background(), dry)
	if err != nil {
		t.Fatalf("preview stale structured binding repair: %v", err)
	}
	planReq := dry
	planReq.PlanReceiptRef = anyToString(preview["plan_receipt_ref"])
	planReq.PlanReceiptDigest = anyToString(preview["plan_receipt_digest"])
	plan, _, err := store.memoryStore.loadMemoryGraphRepairPlanReceipt(planReq, true)
	if err != nil {
		t.Fatalf("load stale structured binding plan: %v", err)
	}
	var refreshed *memoryGraphRepairAction
	for index := range plan.Actions {
		action := &plan.Actions[index]
		if action.Kind == "write" && action.Edge.EdgeID == stale.EdgeID {
			refreshed = action
			break
		}
	}
	if refreshed == nil || refreshed.Edge.Binding == nil || refreshed.Edge.Binding.TargetEventID != targetV2.EventID || refreshed.PreviousBindingReason != "stale_current_state_binding" || refreshed.BindingReason != "bound_current_state" {
		t.Fatalf("repair plan did not distinguish and refresh stale structured binding: action=%#v target_v2=%s", refreshed, targetV2.EventID)
	}
	apply := dry
	apply.DryRun, apply.Apply = false, true
	apply.PlanReceiptRef, apply.PlanReceiptDigest = planReq.PlanReceiptRef, planReq.PlanReceiptDigest
	result, err := store.memoryStore.memoryGraphRepair(context.Background(), apply)
	if err != nil || anyToString(result["status"]) != "complete" {
		t.Fatalf("apply stale structured binding repair: result=%#v err=%v", result, err)
	}
	edges, err = store.memoryStore.listMemoryEdges(context.Background(), memoryEdgeQuery{MemoryID: source.Project + "::" + source.FileName, Relation: "references", Limit: 10})
	if err != nil || len(edges) != 1 || edges[0].Binding == nil || edges[0].Binding.TargetEventID != targetV2.EventID {
		t.Fatalf("applied structured binding is not current: edges=%#v err=%v", edges, err)
	}
	restarted, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("restart refreshed structured binding: %v", err)
	}
	edges, err = restarted.listMemoryEdges(context.Background(), memoryEdgeQuery{MemoryID: source.Project + "::" + source.FileName, Relation: "references", Limit: 10})
	if err != nil || len(edges) != 1 || edges[0].Binding == nil || edges[0].Binding.TargetEventID != targetV2.EventID {
		t.Fatalf("restart lost refreshed structured binding: edges=%#v err=%v", edges, err)
	}
}
