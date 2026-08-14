package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testMemoryEdgeLogEntry(source, target string) memoryEdgeEntry {
	return memoryEdgeEntry{
		SourceID:   source,
		TargetID:   target,
		Relation:   "supports",
		Project:    "alpha",
		Confidence: 1,
		CreatedAt:  "2026-08-10T00:00:00Z",
		Lifecycle:  "durable",
	}
}

func readMemoryEdgeLogStateForTest(t *testing.T, store *memoryStore) memoryEdgeLogState {
	t.Helper()
	raw, err := os.ReadFile(memoryEdgeLogStatePath(store))
	if err != nil {
		t.Fatalf("read memory edge log state: %v", err)
	}
	var state memoryEdgeLogState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("decode memory edge log state: %v", err)
	}
	return state
}

func TestMemoryEdgeLogFenceAcquisitionHonorsCancellation(t *testing.T) {
	server, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	store := server.memoryStore
	fence, err := store.acquireMemoryEdgeLogFence()
	if err != nil {
		t.Fatalf("acquire primary edge fence: %v", err)
	}
	defer fence.release()
	ctx, cancel := context.WithCancel(context.Background())
	acquired := make(chan error, 1)
	go func() {
		_, lockErr := store.acquireMemoryEdgeLogFenceContext(ctx)
		acquired <- lockErr
	}()
	cancel()
	select {
	case lockErr := <-acquired:
		if !errors.Is(lockErr, context.Canceled) {
			t.Fatalf("canceled edge fence returned wrong error: %v", lockErr)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled edge fence acquisition remained blocked")
	}
}

func TestMemoryGraphRepairAndTelemetryScansHonorCancellation(t *testing.T) {
	server, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	store := server.memoryStore
	if _, _, err := store.appendMemoryEdgeLog(testMemoryEdgeLogEntry("alpha::notes/cancel-source.md", "alpha::notes/cancel-target.md"), true); err != nil {
		t.Fatalf("seed cancellation edge: %v", err)
	}
	snapshot, err := store.captureMemoryGraphRepairSnapshot(context.Background(), memoryGraphRepairRequest{Project: "alpha"})
	if err != nil {
		t.Fatalf("capture cancellation repair snapshot: %v", err)
	}
	repairCtx, repairCancel := context.WithCancel(context.Background())
	store.memoryEdgeLogObserveIO = func(operation string, _ int64) {
		if operation == "full_scan_read" {
			repairCancel()
		}
	}
	_, err = store.captureMemoryGraphRepairEdgesContext(repairCtx, snapshot)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("repair evidence scan ignored cancellation: %v", err)
	}
	store.memoryEdgeLogObserveIO = nil

	telemetryCtx, telemetryCancel := context.WithCancel(context.Background())
	store.memoryEdgeLogObserveIO = func(operation string, _ int64) {
		if operation == "full_scan_read" {
			telemetryCancel()
		}
	}
	_, _, err = store.memoryGraphTelemetryDurableEdgesContext(telemetryCtx)
	store.memoryEdgeLogObserveIO = nil
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("durable telemetry scan ignored cancellation: %v", err)
	}
}

func TestMemoryEdgeLogFenceRejectsPathReplacementWhileLeaseIsHeld(t *testing.T) {
	server, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	store := server.memoryStore
	if _, _, err := store.appendMemoryEdgeLog(testMemoryEdgeLogEntry("alpha::notes/a.md", "alpha::notes/b.md"), true); err != nil {
		t.Fatalf("seed path replacement edge: %v", err)
	}
	fence, err := store.acquireMemoryEdgeLogFence()
	if err != nil {
		t.Fatalf("acquire path replacement fence: %v", err)
	}
	defer fence.release()
	lockPath := memoryEdgeLogFencePath(store)
	replacedPath := lockPath + ".replaced"
	if err := os.Rename(lockPath, replacedPath); err != nil {
		t.Fatalf("replace writer lock pathname: %v", err)
	}
	defer func() {
		_ = os.Remove(lockPath)
		_ = os.Rename(replacedPath, lockPath)
	}()
	if _, err := store.newMemoryEdgeLogAppenderFastWithFenceLocked(true, fence); !errors.Is(err, errOwnerOnlyLockPathChanged) {
		t.Fatalf("stale writer lease was not rejected after lock pathname replacement: %v", err)
	}
}

func TestMemoryEdgeLogDetectsSameSizeReplacementAndRepairsExactState(t *testing.T) {
	server, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	store := server.memoryStore
	if _, state, err := store.appendMemoryEdgeLog(testMemoryEdgeLogEntry("alpha::notes/a.md", "alpha::notes/b.md"), true); err != nil {
		t.Fatalf("seed edge log: %v", err)
	} else if state.ContentDigest == "" {
		t.Fatal("successful append persisted an empty content digest")
	}
	before, err := store.snapshotMemoryEdgeLog(0)
	if err != nil {
		t.Fatalf("snapshot before same-size replacement: %v", err)
	}
	replacement := bytes.Replace(before.Bytes, []byte("notes/a.md"), []byte("notes/z.md"), 1)
	if len(replacement) != len(before.Bytes) || bytes.Equal(replacement, before.Bytes) {
		t.Fatal("same-size replacement fixture is invalid")
	}
	if err := os.WriteFile(store.policy.edgePath, replacement, ownerOnlyFileMode); err != nil {
		t.Fatalf("replace edge log out of band: %v", err)
	}
	preservedModTime := time.Unix(0, before.FileStamp.ModTimeNanos)
	if err := os.Chtimes(store.policy.edgePath, preservedModTime, preservedModTime); err != nil {
		t.Fatalf("restore replacement modtime: %v", err)
	}
	replacedStamp, err := memoryEdgeLogPlatformFileStamp(store.policy.edgePath)
	if err != nil {
		t.Fatalf("stat in-place replacement: %v", err)
	}
	if replacedStamp.Identity != before.FileStamp.Identity || replacedStamp.Size != before.FileStamp.Size || replacedStamp.ModTimeNanos != before.FileStamp.ModTimeNanos || replacedStamp.ChangeToken == before.FileStamp.ChangeToken {
		t.Fatalf("same-size in-place fixture did not isolate the change token: before=%#v after=%#v", before.FileStamp, replacedStamp)
	}
	var fullScanReadCalls, fullScanHashCalls int
	store.memoryEdgeLogObserveIO = func(operation string, _ int64) {
		switch operation {
		case "full_scan_read":
			fullScanReadCalls++
		case "full_scan_hash":
			fullScanHashCalls++
		}
	}
	appended := testMemoryEdgeLogEntry("alpha::notes/b.md", "alpha::notes/c.md")
	if _, state, err := store.appendMemoryEdgeLog(appended, true); err != nil {
		t.Fatalf("append after same-size in-place replacement: %v", err)
	} else if state.Generation != before.Generation+2 {
		t.Fatalf("same-size in-place replacement was not reconciled before append: before=%d after=%d", before.Generation, state.Generation)
	}
	store.memoryEdgeLogObserveIO = nil
	if fullScanReadCalls != 1 || fullScanHashCalls != 1 {
		t.Fatalf("same-size in-place replacement did not force one exact scan: reads=%d hashes=%d", fullScanReadCalls, fullScanHashCalls)
	}
	after, err := store.snapshotMemoryEdgeLog(0)
	if err != nil {
		t.Fatalf("reconcile same-size replacement: %v", err)
	}
	if after.Generation != before.Generation+2 || after.ContentDigest != memoryEdgeLogContentDigest(after.Bytes) || after.Digest == before.Digest || !bytes.HasPrefix(after.Bytes, replacement) || !bytes.Contains(after.Bytes, []byte(appended.TargetID)) {
		t.Fatalf("same-size replacement was not exactly reconciled: before=%#v after=%#v", before, after)
	}
	state := readMemoryEdgeLogStateForTest(t, store)
	if state.ContentDigest != after.ContentDigest || state.FileSize != int64(len(after.Bytes)) || state.Generation != after.Generation {
		t.Fatalf("reconciled sidecar is not exact: %#v", state)
	}
}

func TestMemoryEdgeLogAppendStateWriteFailureRecoversDurableRow(t *testing.T) {
	server, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	store := server.memoryStore
	if _, _, err := store.appendMemoryEdgeLog(testMemoryEdgeLogEntry("alpha::notes/a.md", "alpha::notes/b.md"), true); err != nil {
		t.Fatalf("seed edge log: %v", err)
	}
	before, err := store.snapshotMemoryEdgeLog(0)
	if err != nil {
		t.Fatalf("snapshot before injected append failure: %v", err)
	}
	injected := errors.New("injected edge-log state write failure")
	store.memoryEdgeLogBeforeStateWrite = func(memoryEdgeLogState) error { return injected }
	second := testMemoryEdgeLogEntry("alpha::notes/b.md", "alpha::notes/c.md")
	if _, _, err := store.appendMemoryEdgeLog(second, true); !errors.Is(err, injected) {
		t.Fatalf("append did not expose injected state failure: %v", err)
	}
	store.memoryEdgeLogBeforeStateWrite = nil
	recoveredStore := &memoryStore{policy: store.policy}
	recovered, err := recoveredStore.snapshotMemoryEdgeLog(0)
	if err != nil {
		t.Fatalf("recover append after state-write crash: %v", err)
	}
	if !bytes.Contains(recovered.Bytes, []byte(second.TargetID)) || recovered.Generation != before.Generation+1 || recovered.ContentDigest != memoryEdgeLogContentDigest(recovered.Bytes) {
		t.Fatalf("durable append was not exactly recovered: before=%#v recovered=%#v", before, recovered)
	}
	state := readMemoryEdgeLogStateForTest(t, recoveredStore)
	if state.ContentDigest != recovered.ContentDigest || state.FileSize != recovered.FileSize {
		t.Fatalf("append recovery left an inexact sidecar: %#v", state)
	}
}

func TestMemoryEdgeLogCompactionRenameStateWriteFailureRecoversExactFile(t *testing.T) {
	server, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	store := server.memoryStore
	if _, _, err := store.appendMemoryEdgeLog(testMemoryEdgeLogEntry("alpha::notes/a.md", "alpha::notes/b.md"), true); err != nil {
		t.Fatalf("seed edge log: %v", err)
	}
	fence, err := store.acquireMemoryEdgeLogFence()
	if err != nil {
		t.Fatalf("lock edge log: %v", err)
	}
	before, err := store.snapshotMemoryEdgeLogLocked(0)
	if err != nil {
		fence.release()
		t.Fatalf("snapshot before compaction fault: %v", err)
	}
	replacement := bytes.Replace(before.Bytes, []byte("notes/a.md"), []byte("notes/z.md"), 1)
	if len(replacement) != len(before.Bytes) || bytes.Equal(replacement, before.Bytes) {
		fence.release()
		t.Fatal("same-size compaction fixture is invalid")
	}
	injected := errors.New("injected compaction state write failure")
	store.memoryEdgeLogBeforeStateWrite = func(memoryEdgeLogState) error { return injected }
	_, replaceErr := store.replaceMemoryEdgeLogWithFenceLocked(replacement, "test_compaction", fence)
	store.memoryEdgeLogBeforeStateWrite = nil
	fence.release()
	if !errors.Is(replaceErr, injected) {
		t.Fatalf("compaction did not expose injected state failure: %v", replaceErr)
	}
	recoveredStore := &memoryStore{policy: store.policy}
	recovered, err := recoveredStore.snapshotMemoryEdgeLog(0)
	if err != nil {
		t.Fatalf("recover renamed compaction after state-write crash: %v", err)
	}
	if !bytes.Equal(recovered.Bytes, replacement) || recovered.Generation != before.Generation+1 || recovered.ContentDigest != memoryEdgeLogContentDigest(replacement) || recovered.Digest == before.Digest {
		t.Fatalf("renamed compaction was not exactly recovered: before=%#v recovered=%#v", before, recovered)
	}
}

func TestMemoryEdgeLogSameContentCompactionStateFailureAdvancesCAS(t *testing.T) {
	server, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	store := server.memoryStore
	if _, _, err := store.appendMemoryEdgeLog(testMemoryEdgeLogEntry("alpha::notes/a.md", "alpha::notes/b.md"), true); err != nil {
		t.Fatalf("seed edge log: %v", err)
	}
	fence, err := store.acquireMemoryEdgeLogFence()
	if err != nil {
		t.Fatalf("lock edge log: %v", err)
	}
	before, err := store.snapshotMemoryEdgeLogLocked(0)
	if err != nil {
		fence.release()
		t.Fatalf("snapshot before same-content compaction fault: %v", err)
	}
	injected := errors.New("injected same-content compaction state failure")
	store.memoryEdgeLogBeforeStateWrite = func(memoryEdgeLogState) error { return injected }
	_, replaceErr := store.replaceMemoryEdgeLogWithFenceLocked(append([]byte(nil), before.Bytes...), "same_content_compaction", fence)
	store.memoryEdgeLogBeforeStateWrite = nil
	fence.release()
	if !errors.Is(replaceErr, injected) {
		t.Fatalf("same-content compaction did not expose injected state failure: %v", replaceErr)
	}
	recoveredStore := &memoryStore{policy: store.policy}
	recovered, err := recoveredStore.snapshotMemoryEdgeLog(0)
	if err != nil {
		t.Fatalf("recover same-content compaction: %v", err)
	}
	if !bytes.Equal(recovered.Bytes, before.Bytes) || recovered.ContentDigest != before.ContentDigest || recovered.Generation != before.Generation+1 || recovered.Digest == before.Digest || recovered.FileStamp.Identity == before.FileStamp.Identity {
		t.Fatalf("same-content rename did not advance exact CAS state: before=%#v recovered=%#v", before, recovered)
	}
}

func TestMemoryEdgeLogOrdinaryAppendHashesOnlyNewBytes(t *testing.T) {
	server, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	store := server.memoryStore
	if _, err := store.snapshotMemoryEdgeLog(0); err != nil {
		t.Fatalf("initialize exact edge-log state: %v", err)
	}
	before := readMemoryEdgeLogStateForTest(t, store)
	var fullScanReadBytes, fullScanHashBytes, appendHashBytes int64
	store.memoryEdgeLogObserveIO = func(operation string, bytes int64) {
		switch operation {
		case "full_scan_read":
			fullScanReadBytes += bytes
		case "full_scan_hash":
			fullScanHashBytes += bytes
		case "append_content_hash", "append_row_hash":
			appendHashBytes += bytes
		}
	}
	for idx := 0; idx < 256; idx++ {
		edge := testMemoryEdgeLogEntry("alpha::notes/source.md", fmt.Sprintf("alpha::notes/target-%03d.md", idx))
		if _, _, err := store.appendMemoryEdgeLog(edge, true); err != nil {
			t.Fatalf("append edge %d: %v", idx, err)
		}
	}
	after := readMemoryEdgeLogStateForTest(t, store)
	store.memoryEdgeLogObserveIO = nil
	if fullScanReadBytes != 0 || fullScanHashBytes != 0 {
		t.Fatalf("ordinary append regressed to accumulated full scans: read=%d hashed=%d", fullScanReadBytes, fullScanHashBytes)
	}
	if appendHashBytes != 2*(after.FileSize-before.FileSize) {
		t.Fatalf("ordinary append hashing was not bounded to content plus row digests: hashed=%d growth=%d", appendHashBytes, after.FileSize-before.FileSize)
	}
	if _, err := memoryEdgeLogHashFromState(after); err != nil {
		t.Fatalf("final serialized hash state is not restorable: %v", err)
	}
	raw, err := os.ReadFile(store.policy.edgePath)
	if err != nil {
		t.Fatalf("read final edge log: %v", err)
	}
	if after.ContentDigest != memoryEdgeLogContentDigest(raw) {
		t.Fatal("incremental append sidecar does not bind the exact final bytes")
	}
}

func TestMemoryEdgeLogOrdinaryAppendRehashesSameSizeRename(t *testing.T) {
	server, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	store := server.memoryStore
	if _, _, err := store.appendMemoryEdgeLog(testMemoryEdgeLogEntry("alpha::notes/a.md", "alpha::notes/b.md"), true); err != nil {
		t.Fatalf("seed edge log: %v", err)
	}
	before, err := store.snapshotMemoryEdgeLog(0)
	if err != nil {
		t.Fatalf("snapshot before same-size rename: %v", err)
	}
	replacement := bytes.Replace(before.Bytes, []byte("notes/a.md"), []byte("notes/z.md"), 1)
	if len(replacement) != len(before.Bytes) || bytes.Equal(replacement, before.Bytes) {
		t.Fatal("same-size rename fixture is invalid")
	}
	tmpPath := store.policy.edgePath + ".same-size-replacement"
	if err := os.WriteFile(tmpPath, replacement, ownerOnlyFileMode); err != nil {
		t.Fatalf("write same-size replacement: %v", err)
	}
	preservedModTime := time.Unix(0, before.FileStamp.ModTimeNanos)
	if err := os.Chtimes(tmpPath, preservedModTime, preservedModTime); err != nil {
		t.Fatalf("preserve replacement modtime: %v", err)
	}
	if err := os.Rename(tmpPath, store.policy.edgePath); err != nil {
		t.Fatalf("rename same-size replacement: %v", err)
	}
	replacedStamp, err := memoryEdgeLogPlatformFileStamp(store.policy.edgePath)
	if err != nil {
		t.Fatalf("stat renamed replacement: %v", err)
	}
	if replacedStamp.Identity == before.FileStamp.Identity || replacedStamp.Size != before.FileStamp.Size || replacedStamp.ModTimeNanos != before.FileStamp.ModTimeNanos {
		t.Fatalf("same-size rename fixture did not isolate file identity: before=%#v after=%#v", before.FileStamp, replacedStamp)
	}
	var fullScanReadCalls, fullScanHashCalls, fullScanReadBytes, fullScanHashBytes int64
	store.memoryEdgeLogObserveIO = func(operation string, bytes int64) {
		switch operation {
		case "full_scan_read":
			fullScanReadCalls++
			fullScanReadBytes += bytes
		case "full_scan_hash":
			fullScanHashCalls++
			fullScanHashBytes += bytes
		}
	}
	newEdge := testMemoryEdgeLogEntry("alpha::notes/b.md", "alpha::notes/c.md")
	if _, state, err := store.appendMemoryEdgeLog(newEdge, true); err != nil {
		t.Fatalf("append after same-size rename: %v", err)
	} else if state.Generation != before.Generation+2 {
		t.Fatalf("same-size rename was not reconciled before append: before=%d after=%d", before.Generation, state.Generation)
	}
	store.memoryEdgeLogObserveIO = nil
	if fullScanReadCalls != 1 || fullScanHashCalls != 1 || fullScanReadBytes != int64(len(replacement)) || fullScanHashBytes != int64(len(replacement)) {
		t.Fatalf("same-size rename did not trigger one exact scan: read_calls=%d hash_calls=%d read_bytes=%d hash_bytes=%d want=%d", fullScanReadCalls, fullScanHashCalls, fullScanReadBytes, fullScanHashBytes, len(replacement))
	}
	final, err := store.snapshotMemoryEdgeLog(0)
	if err != nil {
		t.Fatalf("snapshot after same-size rename reconciliation: %v", err)
	}
	if !bytes.HasPrefix(final.Bytes, replacement) || !bytes.Contains(final.Bytes, []byte(newEdge.TargetID)) || final.ContentDigest != memoryEdgeLogContentDigest(final.Bytes) {
		t.Fatalf("same-size rename reconciliation lost exact content: %#v", final)
	}
}

func TestMemoryEdgeBackfillMaximumRequestUsesBoundedLinearBatches(t *testing.T) {
	server, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	store := server.memoryStore
	req, err := normalizeMemoryEdgeBackfillRequest(map[string]any{"dry_run": false, "max_writes": 200000}, store.policy)
	if err != nil {
		t.Fatalf("normalize maximum backfill request: %v", err)
	}
	if req.MaxWrites != 200000 {
		t.Fatalf("maximum accepted max_writes changed: %d", req.MaxWrites)
	}
	if _, err := store.snapshotMemoryEdgeLog(0); err != nil {
		t.Fatalf("initialize edge-log state: %v", err)
	}
	before := readMemoryEdgeLogStateForTest(t, store)
	var fullScanReadBytes, fullScanHashBytes, appendHashBytes int64
	batchSizes := []int{}
	store.memoryEdgeLogObserveIO = func(operation string, bytes int64) {
		switch operation {
		case "full_scan_read":
			fullScanReadBytes += bytes
		case "full_scan_hash":
			fullScanHashBytes += bytes
		case "append_content_hash", "append_row_hash":
			appendHashBytes += bytes
		}
	}
	store.memoryEdgeLogObserveBatch = func(size int) { batchSizes = append(batchSizes, size) }
	generator := &memoryEdgeBackfillGenerator{
		store:    store,
		request:  req,
		stats:    map[string]*memoryEdgeBackfillRelationStats{},
		knownIDs: map[string]memoryEdgeBackfillDoc{},
	}
	addCandidate := func(idx int) {
		edge := testMemoryEdgeLogEntry("alpha::notes/batch-source.md", fmt.Sprintf("alpha::notes/batch-target-%03d.md", idx))
		edge.Relation = "supports"
		normalized, normalizeErr := edge.normalized()
		if normalizeErr != nil {
			t.Fatalf("normalize candidate %d: %v", idx, normalizeErr)
		}
		generator.add(context.Background(), memoryEdgeBackfillCandidate{Edge: normalized, Strategy: "test", Reason: "bounded batch proof"})
	}
	for idx := 0; idx < memoryEdgeBackfillWriteBatchLimit; idx++ {
		addCandidate(idx)
	}
	ordinary := testMemoryEdgeLogEntry("alpha::notes/ordinary-source.md", "alpha::notes/ordinary-target.md")
	ordinary.Relation = "supports"
	if _, err := store.upsertMemoryEdge(context.Background(), ordinary); err != nil {
		t.Fatalf("ordinary writer did not progress between bounded backfill batches: %v", err)
	}
	for idx := memoryEdgeBackfillWriteBatchLimit; idx < memoryEdgeBackfillWriteBatchLimit*2+1; idx++ {
		addCandidate(idx)
	}
	generator.flushWrites(context.Background())
	store.memoryEdgeLogObserveIO = nil
	store.memoryEdgeLogObserveBatch = nil
	if generator.ctxErr != nil || len(generator.errorsList) != 0 {
		t.Fatalf("bounded backfill failed: ctx=%v errors=%v", generator.ctxErr, generator.errorsList)
	}
	wantWritten := memoryEdgeBackfillWriteBatchLimit*2 + 1
	if generator.written != wantWritten {
		t.Fatalf("bounded backfill made incomplete progress: written=%d want=%d", generator.written, wantWritten)
	}
	if len(batchSizes) != 3 {
		t.Fatalf("unexpected batch count: %v", batchSizes)
	}
	for _, size := range batchSizes {
		if size < 1 || size > memoryEdgeBackfillWriteBatchLimit {
			t.Fatalf("backfill held the writer fence for an unbounded batch: %v", batchSizes)
		}
	}
	if fullScanReadBytes != 0 || fullScanHashBytes != 0 {
		t.Fatalf("batched backfill regressed to accumulated scans: read=%d hashed=%d", fullScanReadBytes, fullScanHashBytes)
	}
	after := readMemoryEdgeLogStateForTest(t, store)
	if appendHashBytes != 2*(after.FileSize-before.FileSize) {
		t.Fatalf("batched backfill hashing was not bounded to content plus row digests: hashed=%d growth=%d", appendHashBytes, after.FileSize-before.FileSize)
	}
	raw, err := os.ReadFile(store.policy.edgePath)
	if err != nil {
		t.Fatalf("read bounded batch log: %v", err)
	}
	firstEnd := strings.Index(string(raw), "batch-target-063.md")
	ordinaryAt := strings.Index(string(raw), "ordinary-target.md")
	secondStart := strings.Index(string(raw), "batch-target-064.md")
	if firstEnd < 0 || ordinaryAt <= firstEnd || secondStart <= ordinaryAt {
		t.Fatalf("ordinary write did not land between released backfill fences: first=%d ordinary=%d second=%d", firstEnd, ordinaryAt, secondStart)
	}
}

func TestMemoryEdgeBatchStateFailureRecoversBeforeNextBoundedChunk(t *testing.T) {
	server, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	store := server.memoryStore
	if _, err := store.snapshotMemoryEdgeLog(0); err != nil {
		t.Fatalf("initialize edge-log state: %v", err)
	}
	edges := make([]memoryEdgeEntry, 4)
	for idx := range edges {
		edges[idx] = testMemoryEdgeLogEntry("alpha::notes/fault-source.md", fmt.Sprintf("alpha::notes/fault-target-%d.md", idx))
		edges[idx].Relation = "supports"
	}
	injected := errors.New("injected batched edge-log state failure")
	stateWrites := 0
	store.memoryEdgeLogBeforeStateWrite = func(memoryEdgeLogState) error {
		stateWrites++
		if stateWrites == 3 {
			return injected
		}
		return nil
	}
	results, appendErr := store.upsertMemoryEdgesBatch(context.Background(), edges[:3])
	store.memoryEdgeLogBeforeStateWrite = nil
	if !errors.Is(appendErr, injected) || len(results) != 2 {
		t.Fatalf("batched state fault was not surfaced at exact durable prefix: results=%d err=%v", len(results), appendErr)
	}
	if results, err := store.upsertMemoryEdgesBatch(context.Background(), edges[3:]); err != nil || len(results) != 1 {
		t.Fatalf("next bounded chunk did not reconcile and progress: results=%d err=%v", len(results), err)
	}
	final, err := store.snapshotMemoryEdgeLog(0)
	if err != nil {
		t.Fatalf("snapshot recovered batch log: %v", err)
	}
	for _, edge := range edges {
		if !bytes.Contains(final.Bytes, []byte(edge.TargetID)) {
			t.Fatalf("durable batch row was lost across state failure: %s", edge.TargetID)
		}
	}
	if final.ContentDigest != memoryEdgeLogContentDigest(final.Bytes) {
		t.Fatal("batch recovery sidecar does not bind exact durable bytes")
	}
}

func TestMemoryEdgeBatchStateFailureHydratesLiveProjectionAndRetryIsIdempotent(t *testing.T) {
	server, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	store := server.memoryStore
	if _, err := store.snapshotMemoryEdgeLog(0); err != nil {
		t.Fatalf("initialize edge-log state: %v", err)
	}
	edge := testMemoryEdgeLogEntry("alpha::notes/retry-source.md", "alpha::notes/retry-target.md")
	injected := errors.New("injected retry state failure")
	store.memoryEdgeLogBeforeStateWrite = func(memoryEdgeLogState) error { return injected }
	results, appendErr := store.upsertMemoryEdgesBatch(context.Background(), []memoryEdgeEntry{edge})
	if !errors.Is(appendErr, injected) || len(results) != 0 {
		t.Fatalf("expected one durable-but-unacknowledged batch row: results=%d err=%v", len(results), appendErr)
	}
	store.memoryEdgeLogBeforeStateWrite = nil
	normalized, err := edge.normalized()
	if err != nil {
		t.Fatalf("normalize retry edge: %v", err)
	}
	if !store.memoryEdgeExists(normalized.EdgeID) {
		t.Fatal("durable batch row was not hydrated into the live existence projection")
	}
	listed, err := store.listMemoryEdges(context.Background(), memoryEdgeQuery{MemoryID: normalized.SourceID, Limit: 10})
	if err != nil {
		t.Fatalf("list hydrated retry edge: %v", err)
	}
	if len(listed) != 1 || listed[0].EdgeID != normalized.EdgeID {
		t.Fatalf("live list did not expose exactly the hydrated row: %#v", listed)
	}
	results, err = store.upsertMemoryEdgesBatch(context.Background(), []memoryEdgeEntry{edge})
	if err != nil || len(results) != 1 || !results[0].Existing {
		t.Fatalf("retry did not recognize the durable row: results=%#v err=%v", results, err)
	}
	raw, err := os.ReadFile(store.policy.edgePath)
	if err != nil {
		t.Fatalf("read retry edge log: %v", err)
	}
	physicalRows := 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.Contains(line, normalized.EdgeID) {
			physicalRows++
		}
	}
	if physicalRows != 1 {
		t.Fatalf("retry created duplicate physical rows: count=%d raw=%s", physicalRows, raw)
	}
}

func TestMemoryEdgeCompactionStateFailureReloadsLiveProjectionBeforeRetry(t *testing.T) {
	server, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	store := server.memoryStore
	kept := testMemoryEdgeLogEntry("alpha::notes/keep-source.md", "alpha::notes/keep-target.md")
	volatile := testMemoryEdgeLogEntry("alpha::notes/volatile-source.md", "alpha::notes/volatile-target.md")
	store.policy.graphExcludeLowValue = false
	if _, err := store.upsertMemoryEdge(context.Background(), kept); err != nil {
		t.Fatalf("seed kept edge: %v", err)
	}
	if _, err := store.upsertMemoryEdge(context.Background(), volatile); err != nil {
		t.Fatalf("seed volatile edge: %v", err)
	}
	store.policy.graphExcludeLowValue = true
	store.policy.graphExcludeFilePatterns = []string{"*volatile*"}
	injected := errors.New("injected compaction projection failure")
	store.memoryEdgeLogBeforeStateWrite = func(memoryEdgeLogState) error { return injected }
	_, compactErr := store.pruneVolatileMemoryGraphEdges(context.Background(), false)
	if !errors.Is(compactErr, injected) {
		t.Fatalf("expected compaction sidecar failure: %v", compactErr)
	}
	store.memoryEdgeLogBeforeStateWrite = nil
	keptNormalized, err := kept.normalized()
	if err != nil {
		t.Fatalf("normalize kept edge: %v", err)
	}
	volatileNormalized, err := volatile.normalized()
	if err != nil {
		t.Fatalf("normalize volatile edge: %v", err)
	}
	if !store.memoryEdgeExists(keptNormalized.EdgeID) || store.memoryEdgeExists(volatileNormalized.EdgeID) {
		t.Fatalf("compaction failure left the pre-compaction live existence projection in place: kept=%t volatile=%t edges=%#v", store.memoryEdgeExists(keptNormalized.EdgeID), store.memoryEdgeExists(volatileNormalized.EdgeID), store.edges)
	}
	listed, err := store.listMemoryEdges(context.Background(), memoryEdgeQuery{Limit: 10})
	if err != nil {
		t.Fatalf("list after failed compaction: %v", err)
	}
	if len(listed) != 1 || listed[0].EdgeID != keptNormalized.EdgeID {
		t.Fatalf("list after failed compaction is stale: %#v", listed)
	}
	subsequent := testMemoryEdgeLogEntry("alpha::notes/subsequent-source.md", "alpha::notes/subsequent-target.md")
	if _, err := store.upsertMemoryEdge(context.Background(), subsequent); err != nil {
		t.Fatalf("subsequent write after compaction recovery: %v", err)
	}
	final, err := store.snapshotMemoryEdgeLog(0)
	if err != nil {
		t.Fatalf("snapshot after compaction recovery: %v", err)
	}
	if bytes.Contains(final.Bytes, []byte(volatileNormalized.EdgeID)) || !bytes.Contains(final.Bytes, []byte(subsequent.TargetID)) {
		t.Fatalf("compaction recovery did not preserve the exact durable set: %s", final.Bytes)
	}
}

func TestMemoryEdgeAppendHashStampRaceFallsBackToExactScan(t *testing.T) {
	server, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	store := server.memoryStore
	seed := testMemoryEdgeLogEntry("alpha::notes/race-source.md", "alpha::notes/race-target.md")
	if _, _, err := store.appendMemoryEdgeLog(seed, true); err != nil {
		t.Fatalf("seed race edge: %v", err)
	}
	second := testMemoryEdgeLogEntry("alpha::notes/race-target.md", "alpha::notes/race-next.md")
	mutated := false
	store.memoryEdgeLogObserveIO = func(operation string, _ int64) {
		if operation != "append_content_hash" || mutated {
			return
		}
		mutated = true
		stamp, stampErr := memoryEdgeLogPlatformFileStamp(store.policy.edgePath)
		if stampErr != nil {
			t.Fatalf("stamp race fixture: %v", stampErr)
		}
		raw, readErr := os.ReadFile(store.policy.edgePath)
		if readErr != nil {
			t.Fatalf("read stamp race fixture: %v", readErr)
		}
		replacement := bytes.Replace(raw, []byte("notes/race-source.md"), []byte("notes/race-rename.md"), 1)
		if len(replacement) != len(raw) || bytes.Equal(replacement, raw) {
			t.Fatal("stamp race fixture must be a same-size replacement")
		}
		if err := os.WriteFile(store.policy.edgePath, replacement, ownerOnlyFileMode); err != nil {
			t.Fatalf("mutate stamp race fixture: %v", err)
		}
		if err := os.Chtimes(store.policy.edgePath, time.Unix(0, stamp.ModTimeNanos), time.Unix(0, stamp.ModTimeNanos)); err != nil {
			t.Fatalf("preserve stamp race modtime: %v", err)
		}
	}
	_, _, appendErr := store.appendMemoryEdgeLog(second, true)
	store.memoryEdgeLogObserveIO = nil
	if !mutated || appendErr == nil || !strings.Contains(appendErr.Error(), "changed during append hash") {
		t.Fatalf("same-size hash/stamp race was not rejected: mutated=%t err=%v", mutated, appendErr)
	}
	recovered, err := store.snapshotMemoryEdgeLog(0)
	if err != nil {
		t.Fatalf("recover exact bytes after hash/stamp race: %v", err)
	}
	if recovered.ContentDigest != memoryEdgeLogContentDigest(recovered.Bytes) || !bytes.Contains(recovered.Bytes, []byte("notes/race-rename.md")) || !bytes.Contains(recovered.Bytes, []byte(second.TargetID)) {
		t.Fatalf("hash/stamp race persisted an inexact sidecar: %#v", recovered)
	}
	normalized, err := second.normalized()
	if err != nil {
		t.Fatalf("normalize retried race edge: %v", err)
	}
	if !store.memoryEdgeExists(normalized.EdgeID) {
		t.Fatal("exact race recovery did not hydrate the appended row")
	}
	results, err := store.upsertMemoryEdgesBatch(context.Background(), []memoryEdgeEntry{second})
	if err != nil || len(results) != 1 || !results[0].Existing {
		t.Fatalf("retry after exact race recovery: results=%#v err=%v", results, err)
	}
	raw, err := os.ReadFile(store.policy.edgePath)
	if err != nil {
		t.Fatalf("read race edge log: %v", err)
	}
	physicalRows := 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.Contains(line, normalized.EdgeID) {
			physicalRows++
		}
	}
	if physicalRows != 1 {
		t.Fatalf("race retry duplicated the durable row: count=%d raw=%s", physicalRows, raw)
	}
}

func TestMemoryEdgeLogAppendRejectsMutationAfterDescriptorValidation(t *testing.T) {
	server, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	store := server.memoryStore
	seed := testMemoryEdgeLogEntry("alpha::notes/toctou-source.md", "alpha::notes/toctou-target.md")
	if _, _, err := store.appendMemoryEdgeLog(seed, true); err != nil {
		t.Fatalf("seed descriptor TOCTOU edge: %v", err)
	}
	before, err := store.snapshotMemoryEdgeLog(0)
	if err != nil {
		t.Fatalf("snapshot descriptor TOCTOU edge: %v", err)
	}
	second := testMemoryEdgeLogEntry("alpha::notes/toctou-target.md", "alpha::notes/toctou-next.md")
	mutated := false
	store.memoryEdgeLogBeforeAppendWrite = func() {
		mutated = true
		raw, err := os.ReadFile(store.policy.edgePath)
		if err != nil {
			t.Fatalf("read descriptor TOCTOU fixture: %v", err)
		}
		replacement := bytes.Replace(raw, []byte("notes/toctou-source.md"), []byte("notes/toctou-mutant.md"), 1)
		if len(replacement) != len(raw) || bytes.Equal(replacement, raw) {
			t.Fatal("descriptor TOCTOU fixture must be a same-size replacement")
		}
		if err := os.WriteFile(store.policy.edgePath, replacement, ownerOnlyFileMode); err != nil {
			t.Fatalf("mutate descriptor TOCTOU fixture: %v", err)
		}
		if err := os.Chtimes(store.policy.edgePath, time.Unix(0, before.FileStamp.ModTimeNanos), time.Unix(0, before.FileStamp.ModTimeNanos)); err != nil {
			t.Fatalf("preserve descriptor TOCTOU modtime: %v", err)
		}
		afterStamp, err := memoryEdgeLogPlatformFileStamp(store.policy.edgePath)
		if err != nil {
			t.Fatalf("stamp descriptor TOCTOU fixture: %v", err)
		}
		if afterStamp.Identity != before.FileStamp.Identity || afterStamp.Size != before.FileStamp.Size || afterStamp.ModTimeNanos != before.FileStamp.ModTimeNanos || afterStamp.ChangeToken == before.FileStamp.ChangeToken {
			t.Fatalf("descriptor TOCTOU fixture did not preserve same-inode identity while changing content: before=%#v after=%#v", before.FileStamp, afterStamp)
		}
	}
	_, _, appendErr := store.appendMemoryEdgeLog(second, true)
	store.memoryEdgeLogBeforeAppendWrite = nil
	if !mutated || appendErr == nil || !strings.Contains(appendErr.Error(), "changed after descriptor validation") {
		t.Fatalf("mutation after cached descriptor validation was not rejected: mutated=%t err=%v", mutated, appendErr)
	}
	failed, err := store.snapshotMemoryEdgeLog(0)
	if err != nil {
		t.Fatalf("snapshot exact bytes after descriptor TOCTOU rejection: %v", err)
	}
	if !bytes.Contains(failed.Bytes, []byte("notes/toctou-mutant.md")) || bytes.Contains(failed.Bytes, []byte(second.TargetID)) || failed.ContentDigest != memoryEdgeLogContentDigest(failed.Bytes) {
		t.Fatalf("descriptor TOCTOU rejection did not fail closed with exact sidecar: %#v", failed)
	}
	if _, err := store.upsertMemoryEdge(context.Background(), second); err != nil {
		t.Fatalf("retry after descriptor TOCTOU reconciliation: %v", err)
	}
	normalized, err := second.normalized()
	if err != nil {
		t.Fatalf("normalize descriptor TOCTOU retry: %v", err)
	}
	if !store.memoryEdgeExists(normalized.EdgeID) {
		t.Fatal("successful retry did not update same-process edge existence")
	}
	raw, err := os.ReadFile(store.policy.edgePath)
	if err != nil {
		t.Fatalf("read descriptor TOCTOU retry log: %v", err)
	}
	rows := 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.Contains(line, normalized.EdgeID) {
			rows++
		}
	}
	if rows != 1 {
		t.Fatalf("descriptor TOCTOU retry duplicated the row: count=%d raw=%s", rows, raw)
	}
}

func TestMemoryEdgeLogAppendRejectsPreOpenRenameRace(t *testing.T) {
	server, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	store := server.memoryStore
	seed := testMemoryEdgeLogEntry("alpha::notes/preopen-source.md", "alpha::notes/preopen-target.md")
	if _, _, err := store.appendMemoryEdgeLog(seed, true); err != nil {
		t.Fatalf("seed pre-open rename edge: %v", err)
	}
	before, err := store.snapshotMemoryEdgeLog(0)
	if err != nil {
		t.Fatalf("snapshot pre-open rename edge: %v", err)
	}
	oldPath := store.policy.edgePath + ".preopen-old"
	second := testMemoryEdgeLogEntry("alpha::notes/preopen-target.md", "alpha::notes/preopen-next.md")
	mutated := false
	store.memoryEdgeLogBeforeAppendWrite = func() {
		mutated = true
		if err := os.Rename(store.policy.edgePath, oldPath); err != nil {
			t.Fatalf("rename pre-open fixture: %v", err)
		}
		if err := os.WriteFile(store.policy.edgePath, before.Bytes, ownerOnlyFileMode); err != nil {
			t.Fatalf("recreate pre-open fixture: %v", err)
		}
	}
	_, _, appendErr := store.appendMemoryEdgeLog(second, true)
	store.memoryEdgeLogBeforeAppendWrite = nil
	_ = os.Remove(oldPath)
	if !mutated || appendErr == nil || !strings.Contains(appendErr.Error(), "changed after descriptor validation") {
		t.Fatalf("pre-open rename race was not rejected: mutated=%t err=%v", mutated, appendErr)
	}
	after, err := store.snapshotMemoryEdgeLog(0)
	if err != nil {
		t.Fatalf("snapshot after pre-open rename race: %v", err)
	}
	if !bytes.Equal(after.Bytes, before.Bytes) || bytes.Contains(after.Bytes, []byte(second.TargetID)) || after.ContentDigest != memoryEdgeLogContentDigest(after.Bytes) {
		t.Fatalf("pre-open rename race changed the durable row set: before=%#v after=%#v", before, after)
	}
}

func TestMemoryEdgeLogAppendRejectsPreOpenSymlink(t *testing.T) {
	server, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	store := server.memoryStore
	outsidePath := filepath.Join(t.TempDir(), "outside.ndjson")
	outside := []byte("outside-owner-only\n")
	if err := os.WriteFile(outsidePath, outside, ownerOnlyFileMode); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	if err := os.Remove(store.policy.edgePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove edge log for symlink fixture: %v", err)
	}
	if err := os.Symlink(outsidePath, store.policy.edgePath); err != nil {
		t.Skipf("symlink creation unavailable on this platform: %v", err)
	}
	defer os.Remove(store.policy.edgePath)
	_, _, appendErr := store.appendMemoryEdgeLog(testMemoryEdgeLogEntry("alpha::notes/symlink-source.md", "alpha::notes/symlink-target.md"), true)
	if appendErr == nil {
		t.Fatal("append followed a pre-open edge-log symlink")
	}
	actual, err := os.ReadFile(outsidePath)
	if err != nil {
		t.Fatalf("read symlink target after rejected append: %v", err)
	}
	if !bytes.Equal(actual, outside) {
		t.Fatalf("rejected symlink append mutated target: %q", actual)
	}
}

func TestMemoryEdgeLogLegacyMutationEntryPointsRequireFence(t *testing.T) {
	server, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	store := server.memoryStore
	if _, _, err := store.appendMemoryEdgeLogLocked(testMemoryEdgeLogEntry("alpha::notes/unfenced-source.md", "alpha::notes/unfenced-target.md"), true); !errors.Is(err, errMemoryEdgeLogWriterFenceRequired) {
		t.Fatalf("unfenced append entry point was not rejected: %v", err)
	}
	if _, err := store.replaceMemoryEdgeLogLocked(nil, "unfenced_replace"); !errors.Is(err, errMemoryEdgeLogWriterFenceRequired) {
		t.Fatalf("unfenced replacement entry point was not rejected: %v", err)
	}
	if _, err := store.newMemoryEdgeLogAppenderFastLocked(true); !errors.Is(err, errMemoryEdgeLogWriterFenceRequired) {
		t.Fatalf("unfenced appender entry point was not rejected: %v", err)
	}
	if _, err := store.appendMemoryGraphRepairEdgeLocked(testMemoryEdgeLogEntry("alpha::notes/unfenced-repair-source.md", "alpha::notes/unfenced-repair-target.md")); !errors.Is(err, errMemoryEdgeLogWriterFenceRequired) {
		t.Fatalf("unfenced repair writer was not rejected: %v", err)
	}
	if _, err := store.appendMemoryGraphRepairEdgeWithAppenderLocked(testMemoryEdgeLogEntry("alpha::notes/unfenced-appender-source.md", "alpha::notes/unfenced-appender-target.md"), nil); !errors.Is(err, errMemoryEdgeLogWriterFenceRequired) {
		t.Fatalf("unfenced repair appender writer was not rejected: %v", err)
	}
}

func TestMemoryEdgeProjectionFinalInstallRechecksExactStateRegistration(t *testing.T) {
	server, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	store := server.memoryStore
	edge, err := testMemoryEdgeLogEntry("alpha::notes/exact-race-source.md", "alpha::notes/exact-race-target.md").normalized()
	if err != nil {
		t.Fatalf("normalize exact-state race edge: %v", err)
	}
	line, err := json.Marshal(edge)
	if err != nil {
		t.Fatalf("marshal exact-state race edge: %v", err)
	}
	registered := false
	store.memoryEdgeProjectionBeforeInstall = func() {
		if registered {
			return
		}
		registered = true
		if err := store.registerExactStatePath("alpha", "notes/exact-race-source.md"); err != nil {
			t.Fatalf("register exact-state race path: %v", err)
		}
	}
	if err := store.reloadMemoryEdgesFromRawLocked(append(line, '\n')); err != nil {
		t.Fatalf("reload projection under exact-state barrier: %v", err)
	}
	store.memoryEdgeProjectionBeforeInstall = nil
	if !registered {
		t.Fatal("exact-state registration barrier did not run")
	}
	if store.memoryEdgeExists(edge.EdgeID) {
		t.Fatal("final projection install reinserted an edge forbidden by concurrent exact-state registration")
	}
}

func TestMemoryEdgeLogShortWriteTruncatesAndRetryIsExact(t *testing.T) {
	server, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	store := server.memoryStore
	seed := testMemoryEdgeLogEntry("alpha::notes/short-source.md", "alpha::notes/short-target.md")
	if _, _, err := store.appendMemoryEdgeLog(seed, true); err != nil {
		t.Fatalf("seed short-write edge: %v", err)
	}
	before, err := store.snapshotMemoryEdgeLog(0)
	if err != nil {
		t.Fatalf("snapshot before short write: %v", err)
	}
	injected := errors.New("injected short write")
	store.memoryEdgeLogWrite = func(file *os.File, payload []byte) (int, error) {
		n := len(payload) / 2
		if n == 0 {
			n = 1
		}
		if _, err := file.Write(payload[:n]); err != nil {
			return n, err
		}
		return n, injected
	}
	second := testMemoryEdgeLogEntry("alpha::notes/short-target.md", "alpha::notes/short-next.md")
	if _, _, err := store.appendMemoryEdgeLog(second, true); !errors.Is(err, injected) || !errors.Is(err, errMemoryEdgeLogPartialWrite) {
		t.Fatalf("short write was not surfaced as a bounded partial failure: %v", err)
	}
	store.memoryEdgeLogWrite = nil
	afterFailure, err := store.snapshotMemoryEdgeLog(0)
	if err != nil {
		t.Fatalf("snapshot after short-write recovery: %v", err)
	}
	if !bytes.Equal(afterFailure.Bytes, before.Bytes) || afterFailure.Generation < before.Generation {
		t.Fatalf("partial row was not truncated to the exact prior log: before=%#v after=%#v", before, afterFailure)
	}
	if _, _, err := store.appendMemoryEdgeLog(second, true); err != nil {
		t.Fatalf("retry after short-write recovery: %v", err)
	}
	raw, err := os.ReadFile(store.policy.edgePath)
	if err != nil {
		t.Fatalf("read short-write retry log: %v", err)
	}
	if count := strings.Count(string(raw), second.TargetID); count != 1 {
		t.Fatalf("short-write retry duplicated or lost the durable row: count=%d raw=%s", count, raw)
	}
}

func TestMemoryEdgeLogSyncAmbiguityReconcilesCompleteRowWithoutDuplicate(t *testing.T) {
	server, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	store := server.memoryStore
	seed := testMemoryEdgeLogEntry("alpha::notes/sync-source.md", "alpha::notes/sync-target.md")
	if _, _, err := store.appendMemoryEdgeLog(seed, true); err != nil {
		t.Fatalf("seed sync edge: %v", err)
	}
	injected := errors.New("injected sync ambiguity")
	store.memoryEdgeLogSync = func(*os.File) error { return injected }
	second := testMemoryEdgeLogEntry("alpha::notes/sync-target.md", "alpha::notes/sync-next.md")
	if _, _, err := store.appendMemoryEdgeLog(second, true); !errors.Is(err, injected) || !errors.Is(err, errMemoryEdgeLogDurabilityAmbiguous) {
		t.Fatalf("sync ambiguity was not surfaced with exact recovery: %v", err)
	}
	store.memoryEdgeLogSync = nil
	normalized, err := second.normalized()
	if err != nil {
		t.Fatalf("normalize sync retry edge: %v", err)
	}
	if !store.memoryEdgeExists(normalized.EdgeID) {
		t.Fatal("complete row was not hydrated after ambiguous sync")
	}
	results, err := store.upsertMemoryEdgesBatch(context.Background(), []memoryEdgeEntry{second})
	if err != nil || len(results) != 1 || !results[0].Existing {
		t.Fatalf("retry after sync ambiguity did not deduplicate the durable row: results=%#v err=%v", results, err)
	}
	raw, err := os.ReadFile(store.policy.edgePath)
	if err != nil {
		t.Fatalf("read sync retry log: %v", err)
	}
	if count := strings.Count(string(raw), normalized.EdgeID); count != 1 {
		t.Fatalf("sync ambiguity retry duplicated the durable row: count=%d raw=%s", count, raw)
	}
}

func TestMemoryEdgeLogReplacementReadbackRejectsSameInodeHookMutation(t *testing.T) {
	server, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	store := server.memoryStore
	seed := testMemoryEdgeLogEntry("alpha::notes/replace-source.md", "alpha::notes/replace-target.md")
	if _, _, err := store.appendMemoryEdgeLog(seed, true); err != nil {
		t.Fatalf("seed replacement edge: %v", err)
	}
	before, err := store.snapshotMemoryEdgeLog(0)
	if err != nil {
		t.Fatalf("snapshot before replacement: %v", err)
	}
	replacement := bytes.Replace(before.Bytes, []byte("replace-source.md"), []byte("replace-mutant.md"), 1)
	if len(replacement) != len(before.Bytes) || bytes.Equal(replacement, before.Bytes) {
		t.Fatal("same-size replacement fixture is invalid")
	}
	store.memoryEdgeLogAfterReplacementRename = func() {
		mutated := bytes.Replace(replacement, []byte("replace-target.md"), []byte("replace-poison.md"), 1)
		file, openErr := os.OpenFile(store.policy.edgePath, os.O_WRONLY|os.O_TRUNC, ownerOnlyFileMode)
		if openErr != nil {
			t.Fatalf("open same-inode replacement hook: %v", openErr)
		}
		if _, writeErr := file.Write(mutated); writeErr != nil {
			_ = file.Close()
			t.Fatalf("write same-inode replacement hook: %v", writeErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			t.Fatalf("close same-inode replacement hook: %v", closeErr)
		}
	}
	fence, err := store.acquireMemoryEdgeLogFence()
	if err != nil {
		t.Fatalf("acquire replacement fence: %v", err)
	}
	_, replaceErr := store.replaceMemoryEdgeLogWithFenceLocked(replacement, "same_inode_hook", fence)
	fence.release()
	store.memoryEdgeLogAfterReplacementRename = nil
	if replaceErr == nil || !strings.Contains(replaceErr.Error(), "replacement") {
		t.Fatalf("same-inode mutation was not rejected after rename: %v", replaceErr)
	}
	actual, err := store.snapshotMemoryEdgeLog(0)
	if err != nil {
		t.Fatalf("snapshot after poisoned replacement rejection: %v", err)
	}
	if !bytes.Contains(actual.Bytes, []byte("replace-poison.md")) || actual.ContentDigest != memoryEdgeLogContentDigest(actual.Bytes) {
		t.Fatalf("poisoned replacement was not exactly reconciled: %#v", actual)
	}
	state := readMemoryEdgeLogStateForTest(t, store)
	if state.ContentDigest != actual.ContentDigest || state.FileSize != actual.FileSize {
		t.Fatalf("replacement recovery sidecar does not bind actual bytes: %#v actual=%#v", state, actual)
	}
}
