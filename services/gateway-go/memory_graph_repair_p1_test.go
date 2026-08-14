package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMemoryGraphRepairProjectPriorRowsBindPredecessorBeforeLatestOverwrite(t *testing.T) {
	prior := memoryEdgeEntry{
		SourceID: "alpha::notes/source.md", TargetID: "alpha::notes/target.md",
		Relation: "supports", Project: "alpha", Confidence: 0.4, CreatedAt: "2026-01-01T00:00:00Z", Source: "test",
		Metadata: map[string]any{"state": "prior"},
	}
	prior.EdgeID = deterministicMemoryEdgeID(prior.SourceID, prior.Relation, prior.TargetID)
	repair := prior
	repair.Confidence = 0.9
	repair.Metadata = map[string]any{
		"repair_run_id":    "run-prior",
		"repair_action_id": "action-prior",
	}
	repair.Metadata["repair_server_append_marker"] = memoryGraphRepairServerAppendMarker("repair", repair)
	if !memoryGraphRepairServerAppendMarkerValid("repair", repair) {
		t.Fatalf("repair fixture marker is invalid: %#v", repair.Metadata)
	}
	evidence := &memoryGraphRepairEdgeEvidence{Project: "alpha", Latest: map[string]memoryEdgeEntry{}}
	evidence.record(memoryGraphRepairEdgeRow{Edge: prior})
	evidence.record(memoryGraphRepairEdgeRow{Edge: repair})
	got, ok := evidence.ProjectPriorRows["run-prior"]["action-prior"]
	if !ok || got.Confidence != prior.Confidence || got.Metadata["state"] != "prior" {
		t.Fatalf("predecessor index captured overwritten latest row: ok=%v got=%#v want=%#v", ok, got, prior)
	}
	if latest := evidence.Latest[repair.EdgeID]; latest.Confidence != repair.Confidence {
		t.Fatalf("latest row was not advanced after predecessor capture: %#v", latest)
	}

	noPredecessor := &memoryGraphRepairEdgeEvidence{Project: "alpha", Latest: map[string]memoryEdgeEntry{}}
	noPredecessor.record(memoryGraphRepairEdgeRow{Edge: repair})
	zero, ok := noPredecessor.ProjectPriorRows["run-prior"]["action-prior"]
	if !ok || zero.EdgeID != "" {
		t.Fatalf("missing predecessor was not recorded as an authenticated zero marker: ok=%v zero=%#v", ok, zero)
	}
}

func TestMemoryEdgeContextFenceAndPathLocksFailClosed(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()

	fence, err := store.memoryStore.acquireMemoryEdgeLogFenceContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = store.memoryStore.appendEdgeContext(ctx, memoryEdgeEntry{
		SourceID: "alpha::notes/a.md", TargetID: "alpha::notes/b.md", Relation: "references", Project: "alpha",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("append after canceled fence wait = %v, want context.Canceled", err)
	}
	fence.release()

	firstUnlock := store.memoryStore.lockMemoryPath("alpha::a")
	defer firstUnlock()
	acquireCtx, acquireCancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, lockErr := store.memoryStore.lockMemoryPathsContext(acquireCtx, "alpha::a", "alpha::b")
		result <- lockErr
	}()
	acquireCancel()
	if lockErr := <-result; !errors.Is(lockErr, context.Canceled) {
		t.Fatalf("partial path acquisition = %v, want context.Canceled", lockErr)
	}
	// The failed acquisition must not retain the second lock or a reference
	// count. It is immediately acquirable after the canceled waiter exits.
	unlock, err := store.memoryStore.lockMemoryPathContext(context.Background(), "alpha::b")
	if err != nil {
		t.Fatalf("path lock leaked after cancellation: %v", err)
	}
	unlock()
}

func TestExactStateValidationFailureFailsClosedAtCompatibilityBoundaries(t *testing.T) {
	store, _ := newExactStateBoundaryTestStore(t)
	if err := store.registerExactStatePath("alpha", "runtime/state.json"); err != nil {
		t.Fatal(err)
	}
	if err := writeOwnerOnlyDurableAtomicFile(store.policy.exactStateIndexPath, []byte(`{"schema_id":"wrong","paths":[]}`), true); err != nil {
		t.Fatal(err)
	}
	if err := store.loadExactStateIndex(); err == nil {
		t.Fatal("invalid exact-state index was accepted")
	}
	server := &server{memoryStore: store, writePolicy: loadWriteIngressPolicy()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := server.filterExactStateRowsChecked(ctx, []map[string]any{{
		"project": "alpha", "file": "notes/ordinary.md", "summary": "must not leak",
	}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("exact-state checked filter lost cancellation error: %v", err)
	}
	filtered, suppressed := server.filterExactStateRows([]map[string]any{{
		"project": "alpha", "file": "runtime/state.json", "summary": "must not leak",
	}})
	if len(filtered) != 0 || suppressed != 1 {
		t.Fatalf("invalid exact-state authority leaked rows: filtered=%#v suppressed=%d", filtered, suppressed)
	}
}

func TestMemoryGraphRepairEvidenceArtifactSurvivesCacheLoss(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/a.md", "runbooks/graph", "a", "", "")
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/b.md", "runbooks/graph", "b", "", "")
	req := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": false})
	snapshot, err := store.memoryStore.captureMemoryGraphRepairSnapshot(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.memoryStore.captureMemoryGraphRepairEdgesContext(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	rawArtifact, err := readMemoryGraphRepairArtifact(memoryGraphRepairEvidenceArtifactPath(store.memoryStore, snapshot.Project, snapshot.SnapshotDigest))
	if err != nil {
		t.Fatalf("read compact evidence artifact: %v", err)
	}
	var compactArtifact memoryGraphRepairEvidenceArtifact
	if err := json.Unmarshal(rawArtifact, &compactArtifact); err != nil {
		t.Fatalf("decode compact evidence artifact: %v", err)
	}
	if compactArtifact.Version != 2 || len(compactArtifact.Rows) != 0 || compactArtifact.LogContentHashState == "" || compactArtifact.DigestChain == "" {
		t.Fatalf("evidence artifact did not persist compact authenticated midstate: %#v", compactArtifact)
	}
	store.memoryStore.memoryGraphRepairEvidenceCacheMu.Lock()
	store.memoryStore.memoryGraphRepairEvidenceCache = nil
	store.memoryStore.memoryGraphRepairEvidenceCacheProject = ""
	store.memoryStore.memoryGraphRepairEvidenceCacheSnapshot = ""
	store.memoryStore.memoryGraphRepairEvidenceCacheMu.Unlock()
	var fullScans, legacyReplays int
	store.memoryStore.memoryEdgeLogObserveIO = func(kind string, _ int64) {
		if kind == "repair_evidence_full_scan" || kind == "full_scan_hash" {
			fullScans++
		}
		if kind == "repair_evidence_legacy_replay" {
			legacyReplays++
		}
	}
	if _, err := store.memoryStore.captureMemoryGraphRepairEdgesContext(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if fullScans != 0 || legacyReplays != 0 {
		t.Fatalf("cache-loss continuation replayed historical evidence: full_scans=%d legacy_replays=%d", fullScans, legacyReplays)
	}
	if _, _, err := store.memoryStore.loadMemoryGraphRepairEvidenceArtifact(context.Background(), snapshot); err != nil {
		t.Fatalf("durable evidence artifact could not be read: %v", err)
	}
}

func TestMemoryGraphRepairRestartContinuationScalesWithSuffixAcrossUnrelatedAppends(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	for index, summary := range []string{"restart anchor", "restart neighbor", "restart third"} {
		seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", fmt.Sprintf("notes/restart-%d.md", index), "runbooks/restart", summary, "", "")
	}
	dryReq := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": false, "topic_peer_limit": 1, "chunk_size": 1})
	dry, err := store.memoryStore.memoryGraphRepair(context.Background(), dryReq)
	if err != nil {
		t.Fatalf("restart scaling dry-run: %v", err)
	}
	apply := dryReq
	apply.DryRun, apply.Apply = false, true
	apply.PlanReceiptRef, apply.PlanReceiptDigest = anyToString(dry["plan_receipt_ref"]), anyToString(dry["plan_receipt_digest"])
	first, err := store.memoryStore.memoryGraphRepair(context.Background(), apply)
	if err != nil {
		t.Fatalf("restart scaling first chunk: %v", err)
	}
	if anyToInt(first["remaining"], 0) == 0 {
		t.Fatalf("restart scaling fixture produced no continuation actions: %#v", first)
	}
	checkpointID := anyToString(first["checkpoint_id"])
	if checkpointID == "" {
		t.Fatalf("restart scaling chunk omitted checkpoint: %#v", first)
	}
	store.memoryStore.memoryGraphRepairEvidenceCacheMu.Lock()
	store.memoryStore.memoryGraphRepairEvidenceCache = nil
	store.memoryStore.memoryGraphRepairEvidenceCacheProject = ""
	store.memoryStore.memoryGraphRepairEvidenceCacheSnapshot = ""
	store.memoryStore.memoryGraphRepairEvidenceCacheMu.Unlock()
	var fullReads, fullHashes, fullEvidence, incrementalBytes int64
	var snapshotCopies atomic.Int64
	store.memoryStore.memoryGraphRepairSnapshotCopied = func() { snapshotCopies.Add(1) }
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
	var suffixBytes int64
	for index := 0; index < 3; index++ {
		writer := testMemoryEdgeLogEntry("beta::notes/writer-a.md", "beta::notes/writer-b.md")
		writer.Project = "beta"
		writer.Relation = fmt.Sprintf("unrelated_%d", index)
		stored, err := store.memoryStore.upsertMemoryEdge(context.Background(), writer)
		if err != nil {
			t.Fatalf("append unrelated suffix %d: %v", index, err)
		}
		encoded, _ := json.Marshal(stored)
		suffixBytes += int64(len(encoded) + 1)
	}
	continuation := apply
	continuation.CheckpointID = checkpointID
	response, err := store.memoryStore.memoryGraphRepair(context.Background(), continuation)
	store.memoryStore.memoryGraphRepairSnapshotCopied = nil
	store.memoryStore.memoryEdgeLogObserveIO = nil
	if err != nil {
		t.Fatalf("restart scaling continuation: %v", err)
	}
	if fullReads != 0 || fullHashes != 0 || fullEvidence != 0 || snapshotCopies.Load() != 0 {
		t.Fatalf("restart continuation rebuilt historical state: reads=%d hashes=%d evidence=%d snapshot_copies=%d response=%#v", fullReads, fullHashes, fullEvidence, snapshotCopies.Load(), response)
	}
	if incrementalBytes <= 0 || incrementalBytes > suffixBytes {
		t.Fatalf("restart continuation read outside bounded suffix: incremental=%d suffix=%d", incrementalBytes, suffixBytes)
	}
	if anyToInt(response["cursor"], 0) <= anyToInt(first["cursor"], 0) {
		t.Fatalf("restart continuation did not advance action cursor: first=%#v response=%#v", first, response)
	}
}

func appendUnrelatedGraphRepairRows(t *testing.T, store *memoryStore, count int) int64 {
	t.Helper()
	var bytesWritten int64
	for index := 0; index < count; index++ {
		edge := testMemoryEdgeLogEntry(fmt.Sprintf("beta::notes/unrelated-source-%d.md", index), fmt.Sprintf("beta::notes/unrelated-target-%d.md", index))
		edge.Project = "beta"
		edge.Relation = fmt.Sprintf("unrelated_%d", index)
		stored, err := store.upsertMemoryEdge(context.Background(), edge)
		if err != nil {
			t.Fatalf("append unrelated row %d: %v", index, err)
		}
		encoded, err := json.Marshal(stored)
		if err != nil {
			t.Fatalf("marshal unrelated row %d: %v", index, err)
		}
		bytesWritten += int64(len(encoded) + 1)
	}
	return bytesWritten
}

func TestMemoryGraphRepairFirstApplyUsesPlanBoundary(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	for index, summary := range []string{"first apply anchor", "first apply neighbor", "first apply third"} {
		seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", fmt.Sprintf("notes/first-apply-%d.md", index), "runbooks/first-apply", summary, "", "")
	}
	dryReq := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": false, "topic_peer_limit": 1, "chunk_size": 1})
	dry, err := store.memoryStore.memoryGraphRepair(context.Background(), dryReq)
	if err != nil {
		t.Fatalf("first-apply dry-run: %v", err)
	}
	apply := dryReq
	apply.DryRun, apply.Apply = false, true
	apply.PlanReceiptRef, apply.PlanReceiptDigest = anyToString(dry["plan_receipt_ref"]), anyToString(dry["plan_receipt_digest"])
	suffixBytes := appendUnrelatedGraphRepairRows(t, store.memoryStore, 256)
	var fullReads, fullHashes, fullEvidence, incrementalBytes int64
	var snapshotCopies atomic.Int64
	store.memoryStore.memoryGraphRepairSnapshotCopied = func() { snapshotCopies.Add(1) }
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
	first, err := store.memoryStore.memoryGraphRepair(context.Background(), apply)
	store.memoryStore.memoryGraphRepairSnapshotCopied = nil
	store.memoryStore.memoryEdgeLogObserveIO = nil
	if err != nil {
		t.Fatalf("first apply from compact plan boundary: %v", err)
	}
	if anyToInt(first["remaining"], 0) == 0 {
		t.Fatalf("first-apply fixture produced no continuation actions: %#v", first)
	}
	if fullReads != 0 || fullHashes != 0 || fullEvidence != 0 || snapshotCopies.Load() != 0 {
		t.Fatalf("first apply rebuilt historical state: reads=%d hashes=%d evidence=%d snapshot_copies=%d response=%#v", fullReads, fullHashes, fullEvidence, snapshotCopies.Load(), first)
	}
	if incrementalBytes <= 0 || incrementalBytes > suffixBytes {
		t.Fatalf("first apply read outside bounded suffix: incremental=%d suffix=%d", incrementalBytes, suffixBytes)
	}
}

func TestMemoryGraphRepairFirstRollbackUsesCheckpointBoundary(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	for index, summary := range []string{"first rollback anchor", "first rollback neighbor", "first rollback third"} {
		seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", fmt.Sprintf("notes/first-rollback-%d.md", index), "runbooks/first-rollback", summary, "", "")
	}
	dryReq := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": false, "topic_peer_limit": 1, "chunk_size": 64})
	dry, err := store.memoryStore.memoryGraphRepair(context.Background(), dryReq)
	if err != nil {
		t.Fatalf("first-rollback dry-run: %v", err)
	}
	apply := dryReq
	apply.DryRun, apply.Apply = false, true
	apply.PlanReceiptRef, apply.PlanReceiptDigest = anyToString(dry["plan_receipt_ref"]), anyToString(dry["plan_receipt_digest"])
	applied, err := store.memoryStore.memoryGraphRepair(context.Background(), apply)
	if err != nil {
		t.Fatalf("first-rollback apply: %v", err)
	}
	checkpointID := anyToString(applied["checkpoint_id"])
	if checkpointID == "" || anyToInt(applied["total_actions"], 0) == 0 {
		t.Fatalf("first-rollback fixture produced no applied checkpoint: %#v", applied)
	}
	suffixBytes := appendUnrelatedGraphRepairRows(t, store.memoryStore, 256)
	rollback := dryReq
	rollback.DryRun, rollback.Apply, rollback.Rollback = false, false, true
	rollback.PlanReceiptRef, rollback.PlanReceiptDigest, rollback.CheckpointID = apply.PlanReceiptRef, apply.PlanReceiptDigest, checkpointID
	rollback.ChunkSize = 1
	var fullReads, fullHashes, fullEvidence, incrementalBytes int64
	var snapshotCopies atomic.Int64
	store.memoryStore.memoryGraphRepairSnapshotCopied = func() { snapshotCopies.Add(1) }
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
	firstRollback, err := store.memoryStore.memoryGraphRepair(context.Background(), rollback)
	store.memoryStore.memoryGraphRepairSnapshotCopied = nil
	store.memoryStore.memoryEdgeLogObserveIO = nil
	if err != nil {
		t.Fatalf("first rollback from compact checkpoint boundary: %v", err)
	}
	if anyToInt(firstRollback["cursor"], 0) != 1 {
		t.Fatalf("first rollback did not apply exactly one bounded action: %#v", firstRollback)
	}
	if fullReads != 0 || fullHashes != 0 || fullEvidence != 0 || snapshotCopies.Load() != 0 {
		t.Fatalf("first rollback rebuilt historical state: reads=%d hashes=%d evidence=%d snapshot_copies=%d response=%#v", fullReads, fullHashes, fullEvidence, snapshotCopies.Load(), firstRollback)
	}
	if incrementalBytes <= 0 || incrementalBytes > suffixBytes {
		t.Fatalf("first rollback read outside bounded suffix: incremental=%d suffix=%d", incrementalBytes, suffixBytes)
	}
}

func TestMemoryCollectorsReleaseWriterFenceBeforeSlowDiskRead(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()

	diskPath := filepath.Join(store.memoryStore.policy.rootPath, "alpha", "notes", "disk.md")
	if err := os.MkdirAll(filepath.Dir(diskPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(diskPath, []byte("disk collector writer progress"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshotReady := make(chan struct{})
	continueRead := make(chan struct{})
	store.memoryStore.memoryCollectorAfterSnapshot = func() {
		close(snapshotReady)
		<-continueRead
	}
	collectResult := make(chan error, 1)
	go func() {
		_, err := store.memoryStore.collectDocsFromDisk(context.Background(), "alpha", true, true)
		collectResult <- err
	}()
	<-snapshotReady
	if _, err := store.memoryStore.upsertMemoryEdge(context.Background(), memoryEdgeEntry{
		SourceID: "alpha::notes/a.md", TargetID: "alpha::notes/b.md", Relation: "supports", Project: "alpha", Confidence: 1, CreatedAt: nowUTCISO(),
	}); err != nil {
		t.Fatalf("writer was starved by slow disk read: %v", err)
	}
	close(continueRead)
	if err := <-collectResult; err != nil {
		t.Fatalf("disk collection after writer progress: %v", err)
	}
	store.memoryStore.memoryCollectorAfterSnapshot = nil
}

func TestMemoryCollectorsFailClosedOnIndexDriftAfterDiskSnapshot(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()

	diskPath := filepath.Join(store.memoryStore.policy.rootPath, "alpha", "notes", "disk.md")
	if err := os.MkdirAll(filepath.Dir(diskPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(diskPath, []byte("disk collector drift fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshotReady := make(chan struct{})
	continueRead := make(chan struct{})
	store.memoryStore.memoryCollectorAfterSnapshot = func() {
		close(snapshotReady)
		<-continueRead
	}
	collectResult := make(chan error, 1)
	go func() {
		_, err := store.memoryStore.collectDocsFromDisk(context.Background(), "alpha", true, true)
		collectResult <- err
	}()
	<-snapshotReady
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/mutated.md", "runbooks/drift", "mutation", "", "")
	close(continueRead)
	if err := <-collectResult; !errors.Is(err, errMemoryEdgeLogChangedDuringRead) {
		t.Fatalf("disk collector did not fail closed on index drift: %v", err)
	}
	store.memoryStore.memoryCollectorAfterSnapshot = nil
}

func clearMemoryGraphRepairEvidenceCache(store *memoryStore) {
	store.memoryGraphRepairEvidenceCacheMu.Lock()
	store.memoryGraphRepairEvidenceCache = nil
	store.memoryGraphRepairEvidenceCacheProject = ""
	store.memoryGraphRepairEvidenceCacheSnapshot = ""
	store.memoryGraphRepairEvidenceCacheMu.Unlock()
}

func TestMemoryGraphRepairLegacyEvidenceMigratesOnce(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	for index, summary := range []string{"legacy anchor", "legacy neighbor", "legacy third"} {
		seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", fmt.Sprintf("notes/legacy-%d.md", index), "runbooks/legacy", summary, "", "")
	}
	dryReq := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": false, "topic_peer_limit": 1, "chunk_size": 1})
	dry, err := store.memoryStore.memoryGraphRepair(context.Background(), dryReq)
	if err != nil {
		t.Fatalf("legacy migration dry-run: %v", err)
	}
	snapshot, err := store.memoryStore.captureMemoryGraphRepairSnapshot(context.Background(), dryReq)
	if err != nil {
		t.Fatal(err)
	}
	logSnapshot, err := store.memoryStore.snapshotMemoryEdgeLog(0)
	if err != nil {
		t.Fatalf("legacy migration edge snapshot: %v", err)
	}
	legacyEvidence, err := store.memoryStore.memoryGraphRepairEvidenceFromLogSnapshot(snapshot, logSnapshot)
	if err != nil {
		t.Fatalf("legacy migration evidence fixture: %v", err)
	}
	legacyPayload, err := json.Marshal(memoryGraphRepairEvidenceArtifact{
		SchemaID: memoryGraphRepairEvidenceSchemaID, Version: 1, Project: snapshot.Project, SnapshotDigest: snapshot.SnapshotDigest,
		Rows: legacyEvidence.Rows, Digest: legacyEvidence.Digest, Complete: legacyEvidence.Complete,
		ScannedLines: legacyEvidence.ScannedLines, DuplicateCount: legacyEvidence.DuplicateCount, InvalidCount: legacyEvidence.InvalidCount,
		UnboundCount: legacyEvidence.UnboundCount, BoundCount: legacyEvidence.BoundCount, UnboundExplicit: legacyEvidence.UnboundExplicit,
		UnboundInferred: legacyEvidence.UnboundInferred, LogGeneration: legacyEvidence.LogGeneration, LogDigest: legacyEvidence.LogDigest,
		LogContentDigest: legacyEvidence.LogContentDigest, LogContentHashState: legacyEvidence.LogContentHashState,
		LogContentHashedBytes: legacyEvidence.LogContentHashedBytes, LogFileSize: legacyEvidence.LogFileSize,
		LogFileIdentity: legacyEvidence.LogFileIdentity, ScannedBytes: legacyEvidence.ScannedBytes,
	})
	if err != nil {
		t.Fatalf("encode legacy evidence fixture: %v", err)
	}
	if err := writeOwnerOnlyDurableAtomicFile(memoryGraphRepairEvidenceArtifactPath(store.memoryStore, snapshot.Project, snapshot.SnapshotDigest), append(legacyPayload, '\n'), true); err != nil {
		t.Fatalf("write legacy evidence fixture: %v", err)
	}
	planReq := dryReq
	planReq.PlanReceiptRef, planReq.PlanReceiptDigest = anyToString(dry["plan_receipt_ref"]), anyToString(dry["plan_receipt_digest"])
	plan, _, err := store.memoryStore.loadMemoryGraphRepairPlanReceipt(planReq, true)
	if err != nil {
		t.Fatalf("load legacy plan: %v", err)
	}
	// Remove the compact plan boundary to force the bounded legacy apply path.
	plan.EdgeLogContentHashState = ""
	plan.EdgeLogContentHashedBytes = 0
	plan.EdgeLogFileSize = 0
	plan.EdgeLogFileIdentity = ""
	planRaw, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeOwnerOnlyDurableAtomicFile(memoryGraphRepairPlanReceiptPath(store.memoryStore, plan.ReceiptRef), append(planRaw, '\n'), true); err != nil {
		t.Fatalf("write legacy plan fixture: %v", err)
	}
	clearMemoryGraphRepairEvidenceCache(store.memoryStore)
	var legacyReplays, fullReads int64
	store.memoryStore.memoryEdgeLogObserveIO = func(operation string, bytes int64) {
		switch operation {
		case "repair_evidence_legacy_replay":
			legacyReplays++
		case "full_scan_read":
			fullReads += bytes
		}
	}
	apply := dryReq
	apply.DryRun, apply.Apply = false, true
	apply.PlanReceiptRef, apply.PlanReceiptDigest = plan.ReceiptRef, memoryGraphRepairPlanReceiptDigest(plan)
	if _, err := store.memoryStore.memoryGraphRepair(context.Background(), apply); err != nil {
		store.memoryStore.memoryEdgeLogObserveIO = nil
		t.Fatalf("legacy first apply: %v", err)
	}
	store.memoryStore.memoryEdgeLogObserveIO = nil
	if legacyReplays != 1 {
		t.Fatalf("legacy artifact replay count=%d, want exactly one bounded migration replay", legacyReplays)
	}
	if fullReads != 0 {
		t.Fatalf("legacy artifact migration reread the edge log (%d bytes)", fullReads)
	}
	upgradedRaw, err := readMemoryGraphRepairArtifact(memoryGraphRepairEvidenceArtifactPath(store.memoryStore, snapshot.Project, snapshot.SnapshotDigest))
	if err != nil {
		t.Fatal(err)
	}
	var upgraded memoryGraphRepairEvidenceArtifact
	if err := json.Unmarshal(upgradedRaw, &upgraded); err != nil {
		t.Fatal(err)
	}
	if upgraded.Version != 2 || len(upgraded.Rows) != 0 || upgraded.DigestChain == "" || upgraded.LogContentHashState == "" {
		t.Fatalf("legacy artifact was not upgraded to compact authenticated form: %#v", upgraded)
	}
}

func forgeWrongGraphRepairRow(t *testing.T, store *memoryStore, action memoryGraphRepairAction, runID, planRef, planDigest, actionDigest, custodyDigest string) memoryEdgeEntry {
	t.Helper()
	wrong := action.Edge
	wrong.SourceID = "alpha::notes/forged-source.md"
	wrong.TargetID = "alpha::notes/forged-target.md"
	wrong.EdgeID = ""
	wrong.Metadata = cloneJSONMap(wrong.Metadata)
	if wrong.Metadata == nil {
		wrong.Metadata = map[string]any{}
	}
	wrong.Metadata["repair_run_id"] = runID
	wrong.Metadata["repair_action_id"] = action.ActionID
	wrong.Metadata["repair_plan_ref"] = planRef
	wrong.Metadata["repair_plan_digest"] = planDigest
	wrong.Metadata["repair_action_digest"] = actionDigest
	wrong.Metadata["repair_custody_digest"] = custodyDigest
	wrong.Metadata["repair_cursor"] = 0
	wrong.Metadata["repair_server_append_marker"] = memoryGraphRepairServerAppendMarker("repair", wrong)
	appended, err := wrong.normalized()
	if err != nil {
		t.Fatalf("normalize forged graph-repair row: %v", err)
	}
	// The marker is over the normalized row, whose deterministic EdgeID is
	// intentionally different from the immutable action target.
	appended.Metadata = cloneJSONMap(appended.Metadata)
	appended.Metadata["repair_server_append_marker"] = memoryGraphRepairServerAppendMarker("repair", appended)
	// Install the forged row through the canonical raw custody appender. The
	// product repair writer must reject its stale binding; this fixture models a
	// pre-existing/adversarial durable row that recovery still has to detect.
	appended, _, err = store.appendMemoryEdgeLog(appended, true)
	if err != nil {
		t.Fatalf("append forged graph-repair row: %v", err)
	}
	return appended
}

func TestMemoryGraphRepairRejectsMarkedWrongEdgeWithoutAdvancement(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	for index, summary := range []string{"wrong edge anchor", "wrong edge neighbor", "wrong edge third"} {
		seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", fmt.Sprintf("notes/wrong-%d.md", index), "runbooks/wrong-edge", summary, "", "")
	}
	dryReq := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": false, "topic_peer_limit": 1, "chunk_size": 1})
	dry, err := store.memoryStore.memoryGraphRepair(context.Background(), dryReq)
	if err != nil {
		t.Fatalf("wrong-edge dry-run: %v", err)
	}
	planReq := dryReq
	planReq.PlanReceiptRef, planReq.PlanReceiptDigest = anyToString(dry["plan_receipt_ref"]), anyToString(dry["plan_receipt_digest"])
	plan, planDigest, err := store.memoryStore.loadMemoryGraphRepairPlanReceipt(planReq, true)
	if err != nil || len(plan.Actions) == 0 {
		t.Fatalf("wrong-edge plan: %v actions=%d", err, len(plan.Actions))
	}
	boundary := memoryGraphRepairPlanBoundaryCheckpoint(plan, planDigest)
	boundary.Status = "running"
	boundary.TotalActions = len(plan.Actions)
	boundary.Counts = map[string]int{"plan_actions": len(plan.Actions)}
	boundary.ActorPrincipalDigest = dryReq.ActorPrincipalDigest
	boundary.ActorScopeDigest = dryReq.ActorScopeDigest
	boundary.ActorCustodyDigest = dryReq.ActorCustodyDigest
	if err := store.memoryStore.writeMemoryGraphRepairCheckpoint(boundary); err != nil {
		t.Fatalf("write wrong-edge checkpoint: %v", err)
	}
	wrong := forgeWrongGraphRepairRow(t, store.memoryStore, plan.Actions[0], boundary.RunID, plan.ReceiptRef, planDigest, plan.ActionDigest, dryReq.ActorCustodyDigest)
	evidence := memoryGraphRepairEdgeEvidence{
		Latest:           map[string]memoryEdgeEntry{wrong.EdgeID: wrong},
		RepairActionRows: map[string]map[string]memoryEdgeEntry{boundary.RunID: {plan.Actions[0].ActionID: wrong}},
	}
	if _, recovered := memoryGraphRepairActionRecovered(plan.Actions[0], evidence, boundary.RunID, plan.ReceiptRef, planDigest, plan.ActionDigest, dryReq.ActorCustodyDigest); recovered {
		t.Fatal("wrong-edge crash row was accepted as immutable action recovery")
	}
	continuation := dryReq
	continuation.DryRun, continuation.Apply = false, true
	continuation.CheckpointID = boundary.CheckpointID
	continuation.PlanReceiptRef, continuation.PlanReceiptDigest = plan.ReceiptRef, planDigest
	if _, err := store.memoryStore.memoryGraphRepair(context.Background(), continuation); err == nil || !strings.Contains(err.Error(), "edge") {
		t.Fatalf("marked wrong-edge suffix was accepted: %v", err)
	}
	after, exists, err := store.memoryStore.loadMemoryGraphRepairCheckpoint("alpha", plan.ReceiptRef)
	if err != nil || !exists {
		t.Fatalf("load unchanged wrong-edge checkpoint: exists=%v err=%v", exists, err)
	}
	if after.Cursor != 0 || after.Status != "running" {
		t.Fatalf("wrong-edge rejection advanced checkpoint coverage: %#v", after)
	}
	if _, rollbackExists, rollbackErr := store.memoryStore.loadMemoryGraphRepairRollbackCheckpoint(plan.ReceiptRef); rollbackErr != nil || rollbackExists {
		t.Fatalf("wrong-edge rejection created rollback coverage: exists=%v err=%v", rollbackExists, rollbackErr)
	}
}

func TestMemoryGraphRepairSameKeyRewriteAdvancesGenerationAndFailsClosed(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/rewrite.md", "runbooks/rewrite", "before", "", "")
	dryReq := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": false})
	dry, err := store.memoryStore.memoryGraphRepair(context.Background(), dryReq)
	if err != nil {
		t.Fatalf("rewrite generation dry-run: %v", err)
	}
	beforeKey, beforeTopic, err := store.memoryStore.currentMemoryGraphRepairGeneration("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.memoryStore.put(normalizedWrite{project: "alpha", fileName: "notes/rewrite.md", topicPath: "runbooks/rewrite", content: "after same key", lifecycle: "durable"}); err != nil {
		t.Fatalf("same-key rewrite: %v", err)
	}
	afterKey, afterTopic, err := store.memoryStore.currentMemoryGraphRepairGeneration("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if afterKey <= beforeKey || afterTopic <= beforeTopic {
		t.Fatalf("same-key rewrite did not advance exact generations: before=%d/%d after=%d/%d", beforeKey, beforeTopic, afterKey, afterTopic)
	}
	apply := dryReq
	apply.DryRun, apply.Apply = false, true
	apply.PlanReceiptRef, apply.PlanReceiptDigest = anyToString(dry["plan_receipt_ref"]), anyToString(dry["plan_receipt_digest"])
	if _, err := store.memoryStore.memoryGraphRepair(context.Background(), apply); err == nil || !strings.Contains(err.Error(), "generation") {
		t.Fatalf("same-key rewrite allowed stale graph plan: %v", err)
	}

	snapshotReady := make(chan struct{})
	continueRead := make(chan struct{})
	store.memoryStore.memoryCollectorAfterSnapshot = func() {
		close(snapshotReady)
		<-continueRead
	}
	collectResult := make(chan error, 1)
	go func() {
		_, collectErr := store.memoryStore.collectDocsFromDisk(context.Background(), "alpha", true, true)
		collectResult <- collectErr
	}()
	<-snapshotReady
	if _, _, err := store.memoryStore.put(normalizedWrite{project: "alpha", fileName: "notes/rewrite.md", topicPath: "runbooks/rewrite", content: "after collector snapshot", lifecycle: "durable"}); err != nil {
		t.Fatalf("same-key collector rewrite: %v", err)
	}
	close(continueRead)
	store.memoryStore.memoryCollectorAfterSnapshot = nil
	if err := <-collectResult; !errors.Is(err, errMemoryEdgeLogChangedDuringRead) {
		t.Fatalf("collector did not fail closed after same-key rewrite: %v", err)
	}
}

func TestMemoryGraphRepairCopiedRootArtifactsFailCustody(t *testing.T) {
	source, sourceGateway := newMemoryGraphTestServer(t, true)
	defer sourceGateway.Close()
	target, targetGateway := newMemoryGraphTestServer(t, true)
	defer targetGateway.Close()
	seedMemoryGraphRepairDoc(t, source.memoryStore, "alpha", "notes/custody.md", "runbooks/custody", "custody", "", "")
	dryReq := repairRequest(t, source.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": false})
	dry, err := source.memoryStore.memoryGraphRepair(context.Background(), dryReq)
	if err != nil {
		t.Fatalf("custody source dry-run: %v", err)
	}
	planRef, planDigest := anyToString(dry["plan_receipt_ref"]), anyToString(dry["plan_receipt_digest"])
	planRaw, err := readMemoryGraphRepairArtifact(memoryGraphRepairPlanReceiptPath(source.memoryStore, planRef))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeOwnerOnlyDurableAtomicFile(memoryGraphRepairPlanReceiptPath(target.memoryStore, planRef), planRaw, true); err != nil {
		t.Fatalf("copy plan artifact: %v", err)
	}
	if _, _, err := target.memoryStore.loadMemoryGraphRepairPlanReceipt(memoryGraphRepairRequest{Project: "alpha", PlanReceiptRef: planRef, PlanReceiptDigest: planDigest}, true); err == nil || !strings.Contains(err.Error(), "custody") {
		t.Fatalf("copied plan artifact was accepted across roots: %v", err)
	}
	plan, _, err := source.memoryStore.loadMemoryGraphRepairPlanReceipt(memoryGraphRepairRequest{Project: "alpha", PlanReceiptRef: planRef, PlanReceiptDigest: planDigest}, true)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := memoryGraphRepairCheckpoint{SchemaID: memoryGraphRepairCheckpointID, Version: 1, CheckpointID: "repair_copied", RunID: "run_copied", Project: "alpha", PlanReceiptRef: planRef, PlanReceiptDigest: planDigest, SnapshotDigest: plan.SnapshotDigest, PolicyDigest: plan.PolicyDigest, ActionDigest: plan.ActionDigest, EdgeDigestBefore: plan.EdgeDigest, EdgeDigestAfter: plan.EdgeDigest, EdgeDigestAlgorithm: memoryGraphRepairDigestAlgorithm, TotalActions: len(plan.Actions), Status: "running", Counts: map[string]int{"plan_actions": len(plan.Actions)}, Custody: memoryGraphRepairCustody(source.memoryStore)}
	checkpoint.CheckpointDigest = memoryGraphRepairCheckpointDigest(checkpoint)
	checkpointRaw, _ := json.MarshalIndent(checkpoint, "", "  ")
	if err := writeOwnerOnlyDurableAtomicFile(memoryGraphRepairPlanCheckpointPath(target.memoryStore, planRef), append(checkpointRaw, '\n'), true); err != nil {
		t.Fatalf("copy apply checkpoint: %v", err)
	}
	if _, _, err := target.memoryStore.loadMemoryGraphRepairCheckpoint("alpha", planRef); err == nil || !strings.Contains(err.Error(), "custody") {
		t.Fatalf("copied apply checkpoint was accepted across roots: %v", err)
	}
	rollback := memoryGraphRepairRollbackCheckpoint{SchemaID: memoryGraphRepairRollbackCheckpointID, Version: 1, CheckpointID: "rollback_copied", RunID: "rollback_run_copied", Project: "alpha", PlanReceiptRef: planRef, PlanReceiptDigest: planDigest, SnapshotDigest: plan.SnapshotDigest, PolicyDigest: plan.PolicyDigest, ActionDigest: plan.ActionDigest, ApplyCheckpointID: checkpoint.CheckpointID, ApplyRunID: checkpoint.RunID, AppliedActionCount: 0, Status: "running", Counts: map[string]int{"restored_total": 0}, EdgeDigestAfter: plan.EdgeDigest, EdgeDigestAlgorithm: memoryGraphRepairDigestAlgorithm, Custody: memoryGraphRepairCustody(source.memoryStore)}
	rollback.CheckpointDigest = memoryGraphRepairRollbackCheckpointDigest(rollback)
	rollbackRaw, _ := json.MarshalIndent(rollback, "", "  ")
	if err := writeOwnerOnlyDurableAtomicFile(memoryGraphRepairRollbackCheckpointPath(target.memoryStore, planRef), append(rollbackRaw, '\n'), true); err != nil {
		t.Fatalf("copy rollback checkpoint: %v", err)
	}
	if _, _, err := target.memoryStore.loadMemoryGraphRepairRollbackCheckpoint(planRef); err == nil || !strings.Contains(err.Error(), "custody") {
		t.Fatalf("copied rollback checkpoint was accepted across roots: %v", err)
	}
}

func TestMemoryGraphRepairTamperedCheckpointCannotAdvance(t *testing.T) {
	for _, test := range []struct {
		name      string
		mutate    func(*memoryGraphRepairCheckpoint)
		recompute bool
	}{
		{name: "edge digest", mutate: func(checkpoint *memoryGraphRepairCheckpoint) {
			checkpoint.EdgeDigestAfter = "sha256:" + strings.Repeat("0", 64)
		}, recompute: true},
		{name: "digest algorithm", mutate: func(checkpoint *memoryGraphRepairCheckpoint) { checkpoint.EdgeDigestAlgorithm = "forged-algorithm" }, recompute: true},
		{name: "checkpoint digest", mutate: func(checkpoint *memoryGraphRepairCheckpoint) {
			checkpoint.CheckpointDigest = "sha256:" + strings.Repeat("f", 64)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, gateway := newMemoryGraphTestServer(t, true)
			defer gateway.Close()
			for index, summary := range []string{"tamper anchor", "tamper neighbor", "tamper third"} {
				seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", fmt.Sprintf("notes/tamper-%d.md", index), "runbooks/tamper", summary, "", "")
			}
			dryReq := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": false, "topic_peer_limit": 1, "chunk_size": 1})
			dry, err := store.memoryStore.memoryGraphRepair(context.Background(), dryReq)
			if err != nil {
				t.Fatal(err)
			}
			apply := dryReq
			apply.DryRun, apply.Apply = false, true
			apply.PlanReceiptRef, apply.PlanReceiptDigest = anyToString(dry["plan_receipt_ref"]), anyToString(dry["plan_receipt_digest"])
			first, err := store.memoryStore.memoryGraphRepair(context.Background(), apply)
			if err != nil {
				t.Fatalf("seed checkpoint: %v", err)
			}
			checkpointID := anyToString(first["checkpoint_id"])
			checkpointPath := memoryGraphRepairPlanCheckpointPath(store.memoryStore, apply.PlanReceiptRef)
			raw, err := readMemoryGraphRepairArtifact(checkpointPath)
			if err != nil {
				t.Fatal(err)
			}
			var checkpoint memoryGraphRepairCheckpoint
			if err := json.Unmarshal(raw, &checkpoint); err != nil {
				t.Fatal(err)
			}
			beforeCursor := checkpoint.Cursor
			test.mutate(&checkpoint)
			if test.recompute {
				checkpoint.CheckpointDigest = memoryGraphRepairCheckpointDigest(checkpoint)
			}
			mutated, _ := json.MarshalIndent(checkpoint, "", "  ")
			if err := writeOwnerOnlyDurableAtomicFile(checkpointPath, append(mutated, '\n'), true); err != nil {
				t.Fatal(err)
			}
			continuation := apply
			continuation.CheckpointID = checkpointID
			if _, err := store.memoryStore.memoryGraphRepair(context.Background(), continuation); err == nil {
				t.Fatal("tampered checkpoint was accepted")
			}
			afterRaw, err := readMemoryGraphRepairArtifact(checkpointPath)
			if err != nil {
				t.Fatal(err)
			}
			var after memoryGraphRepairCheckpoint
			if err := json.Unmarshal(afterRaw, &after); err != nil {
				t.Fatal(err)
			}
			if after.Cursor != beforeCursor {
				t.Fatalf("tampered checkpoint advanced cursor from %d to %d", beforeCursor, after.Cursor)
			}
		})
	}
}

func TestMemoryGraphRepairCompactProjectionTamperFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*memoryGraphRepairEvidenceArtifact)
	}{
		{name: "counter", mutate: func(artifact *memoryGraphRepairEvidenceArtifact) { artifact.ScannedLines++ }},
		{name: "latest", mutate: func(artifact *memoryGraphRepairEvidenceArtifact) {
			for edgeID, edge := range artifact.Latest {
				edge.Relation = "forged_relation"
				artifact.Latest[edgeID] = edge
				return
			}
			artifact.Latest["forged"] = memoryEdgeEntry{EdgeID: "forged", SourceID: "alpha::notes/a.md", TargetID: "alpha::notes/b.md", Relation: "forged_relation", Project: "alpha", CreatedAt: "2020-01-01T00:00:00Z"}
		}},
		{name: "repair rows", mutate: func(artifact *memoryGraphRepairEvidenceArtifact) {
			if artifact.RepairActionRows == nil {
				artifact.RepairActionRows = map[string]map[string]memoryEdgeEntry{}
			}
			artifact.RepairActionRows["forged_run"] = map[string]memoryEdgeEntry{"forged_action": {EdgeID: "forged", SourceID: "alpha::notes/a.md", TargetID: "alpha::notes/b.md", Relation: "forged_relation", Project: "alpha", CreatedAt: "2020-01-01T00:00:00Z"}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, gateway := newMemoryGraphTestServer(t, true)
			defer gateway.Close()
			seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/a.md", "runbooks/projection", "projection a", "", "")
			seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/b.md", "runbooks/projection", "projection b", "", "")
			if _, err := store.memoryStore.upsertMemoryEdge(context.Background(), memoryEdgeEntry{SourceID: "alpha::notes/a.md", TargetID: "alpha::notes/b.md", Relation: "supports", Project: "alpha", Confidence: 1, CreatedAt: "2020-01-01T00:00:00Z", Lifecycle: "durable"}); err != nil {
				t.Fatal(err)
			}
			req := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": false})
			snapshot, err := store.memoryStore.captureMemoryGraphRepairSnapshot(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.memoryStore.captureMemoryGraphRepairEdgesContext(context.Background(), snapshot); err != nil {
				t.Fatal(err)
			}
			artifactPath := memoryGraphRepairEvidenceArtifactPath(store.memoryStore, snapshot.Project, snapshot.SnapshotDigest)
			raw, err := readMemoryGraphRepairArtifact(artifactPath)
			if err != nil {
				t.Fatal(err)
			}
			var artifact memoryGraphRepairEvidenceArtifact
			if err := json.Unmarshal(raw, &artifact); err != nil {
				t.Fatal(err)
			}
			if artifact.Version != 2 || artifact.ProjectionDigest == "" {
				t.Fatalf("expected authenticated compact projection: %#v", artifact)
			}
			test.mutate(&artifact)
			mutated, err := json.Marshal(artifact)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeOwnerOnlyDurableAtomicFile(artifactPath, append(mutated, '\n'), true); err != nil {
				t.Fatal(err)
			}
			clearMemoryGraphRepairEvidenceCache(store.memoryStore)
			if _, ok, err := store.memoryStore.loadMemoryGraphRepairEvidenceArtifact(context.Background(), snapshot); err == nil || ok {
				t.Fatalf("tampered compact projection was accepted: ok=%v err=%v", ok, err)
			}
		})
	}
}

func TestMemoryGraphRepairRecoveryRejectsSameIDTupleForgery(t *testing.T) {
	actionEdge, err := (memoryEdgeEntry{
		SourceID: "alpha::notes/a.md", TargetID: "alpha::notes/b.md", Relation: "references", Project: "alpha",
		Confidence: 1, CreatedAt: "2020-01-01T00:00:00Z", Lifecycle: "durable",
	}).normalized()
	if err != nil {
		t.Fatal(err)
	}
	action := memoryGraphRepairAction{ActionID: "1:" + actionEdge.EdgeID, Kind: "write", Edge: actionEdge}
	for _, test := range []struct {
		name   string
		mutate func(*memoryEdgeEntry)
	}{
		{name: "wrong endpoint", mutate: func(edge *memoryEdgeEntry) { edge.TargetID = "alpha::notes/c.md" }},
		{name: "wrong relation", mutate: func(edge *memoryEdgeEntry) { edge.Relation = "depends_on" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			forged := action.Edge
			test.mutate(&forged)
			// Preserve the original EdgeID: the regression is specifically a
			// copied identity paired with a different normalized tuple.
			forged.EdgeID = action.Edge.EdgeID
			forged.Metadata = map[string]any{
				"repair_run_id": "run_tuple", "repair_action_id": action.ActionID,
				"repair_plan_ref": "plan_tuple", "repair_plan_digest": "sha256:" + strings.Repeat("1", 64),
				"repair_action_digest": "sha256:" + strings.Repeat("2", 64), "repair_custody_digest": "sha256:" + strings.Repeat("3", 64),
				"repair_server_append_marker": "",
			}
			forged.Metadata["repair_server_append_marker"] = memoryGraphRepairServerAppendMarker("repair", forged)
			evidence := memoryGraphRepairEdgeEvidence{
				Latest:           map[string]memoryEdgeEntry{forged.EdgeID: forged},
				RepairActionRows: map[string]map[string]memoryEdgeEntry{"run_tuple": {action.ActionID: forged}},
			}
			if _, recovered := memoryGraphRepairActionRecovered(action, evidence, "run_tuple", "plan_tuple", "sha256:"+strings.Repeat("1", 64), "sha256:"+strings.Repeat("2", 64), "sha256:"+strings.Repeat("3", 64)); recovered {
				t.Fatalf("same-ID forged %s tuple recovered as immutable action", test.name)
			}
		})
	}
}

func TestMemoryGraphRepairCurrentStateGenerationDurableAcrossRestartAndDuplicate(t *testing.T) {
	t.Setenv("GO_MEMORY_STORE_BACKGROUND_HYDRATION_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_BACKGROUND_ENABLED", "false")
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/restart.md", "runbooks/restart", "restart before", "", "")
	beforeKey, beforeTopic, beforeDigest, err := store.memoryStore.durableCurrentStateGeneration("alpha")
	if err != nil {
		t.Fatal(err)
	}
	state, ok := store.memoryStore.currentStateFor("alpha", "notes/restart.md")
	if !ok {
		t.Fatal("current-state fixture missing")
	}
	store.memoryStore.mu.Lock()
	store.memoryStore.recordEntry(state.Entry)
	store.memoryStore.mu.Unlock()
	duplicateKey, duplicateTopic, duplicateDigest, err := store.memoryStore.durableCurrentStateGeneration("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if duplicateKey != beforeKey || duplicateTopic != beforeTopic || duplicateDigest != beforeDigest {
		t.Fatalf("duplicate replay spuriously advanced durable generation: before=%d/%d/%s after=%d/%d/%s", beforeKey, beforeTopic, beforeDigest, duplicateKey, duplicateTopic, duplicateDigest)
	}
	req := repairRequest(t, store.memoryStore, map[string]any{"project": "alpha", "dry_run": true, "include_inferred": false})
	plan, err := store.memoryStore.memoryGraphRepair(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("restart current-state store: %v", err)
	}
	reloaded.ready.Store(true)
	reloadedKey, reloadedTopic, reloadedDigest, err := reloaded.durableCurrentStateGeneration("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if reloadedKey != beforeKey || reloadedTopic != beforeTopic || reloadedDigest != beforeDigest {
		t.Fatalf("restart changed durable current-state generation: before=%d/%d/%s after=%d/%d/%s", beforeKey, beforeTopic, beforeDigest, reloadedKey, reloadedTopic, reloadedDigest)
	}
	if _, _, err := reloaded.put(normalizedWrite{project: "alpha", fileName: "notes/restart.md", topicPath: "runbooks/restart", content: "restart after same-key rewrite", lifecycle: "durable"}); err != nil {
		t.Fatal(err)
	}
	apply := req
	apply.DryRun, apply.Apply = false, true
	apply.PlanReceiptRef, apply.PlanReceiptDigest = anyToString(plan["plan_receipt_ref"]), anyToString(plan["plan_receipt_digest"])
	if _, err := reloaded.memoryGraphRepair(context.Background(), apply); err == nil || !strings.Contains(err.Error(), "plan") {
		t.Fatalf("same-key rewrite outside edge-log tail restored stale plan: %v", err)
	}
}

func TestMemoryGraphRepairCurrentStateGenerationRejectsBoundedTailLoss(t *testing.T) {
	t.Setenv("GO_MEMORY_STORE_BACKGROUND_HYDRATION_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_BACKGROUND_ENABLED", "false")
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/lost-tail.md", "runbooks/loss", "bounded tail", "", "")
	shard := memoryCurrentStateShardForKey("alpha::notes/lost-tail.md")
	if err := os.Remove(store.memoryStore.currentStateShardPath(shard)); err != nil {
		t.Fatalf("remove bounded-tail fixture shard: %v", err)
	}
	if _, err := newMemoryStoreFromEnv(); err == nil || (!strings.Contains(err.Error(), "current-state generation manifest") && !strings.Contains(err.Error(), "current-state generation card")) {
		t.Fatalf("bounded current-state tail loss was not rejected: %v", err)
	}
}
