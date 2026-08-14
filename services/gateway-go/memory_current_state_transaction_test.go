package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestMemoryCurrentStateTransactionRollsForwardEveryFaultBoundary(t *testing.T) {
	t.Setenv("GO_MEMORY_STORE_BACKGROUND_HYDRATION_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_BACKGROUND_ENABLED", "false")
	for _, event := range []string{"before_marker", "after_marker", "after_shard_rename", "after_card_rename", "after_manifest_rename", "before_marker_removal"} {
		t.Run(event, func(t *testing.T) {
			store, gateway := newMemoryGraphTestServer(t, true)
			seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/txn.md", "runbooks/txn", "before transaction", "", "")
			fault := errors.New("injected " + event)
			store.memoryStore.memoryCurrentStateTransactionFault = func(candidate string) error {
				if candidate == event {
					return fault
				}
				return nil
			}
			_, _, err := store.memoryStore.put(normalizedWrite{project: "alpha", fileName: "notes/txn.md", topicPath: "runbooks/txn", content: "after transaction", lifecycle: "durable"})
			if err == nil || !strings.Contains(err.Error(), event) {
				t.Fatalf("fault boundary %s was not surfaced: %v", event, err)
			}
			markerPath := store.memoryStore.currentStateTransactionPath()
			if event != "before_marker" {
				if _, statErr := os.Stat(markerPath); statErr != nil {
					t.Fatalf("fault boundary %s did not leave a recovery marker: %v", event, statErr)
				}
			} else if _, statErr := os.Stat(markerPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("fault boundary %s left a marker after pre-marker failure: %v", event, statErr)
			}
			gateway.Close()
			reloaded, reloadErr := newMemoryStoreFromEnv()
			if reloadErr != nil {
				t.Fatalf("restart recovery after %s: %v", event, reloadErr)
			}
			state, ok := reloaded.currentStateFor("alpha", "notes/txn.md")
			if !ok || state.Entry.Summary != "after transaction" {
				t.Fatalf("restart did not recover exact new state after %s: ok=%v state=%#v", event, ok, state)
			}
			if _, statErr := os.Stat(reloaded.currentStateTransactionPath()); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("restart did not clear recovered marker after %s: %v", event, statErr)
			}
		})
	}
}

func TestMemoryCurrentStateTransactionRejectsForeignMarkerPaths(t *testing.T) {
	store, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	foreign := filepath.Join(t.TempDir(), "foreign-stage.json")
	if err := os.WriteFile(foreign, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := memoryCurrentStateTransaction{
		SchemaID: memoryCurrentStateTransactionSchemaID, Version: memoryCurrentStateTransactionVersion,
		TransactionID: "foreign", State: "prepared", StageManifestPath: "../foreign-stage.json", FinalManifestPath: "generations.json",
		NewManifestDigest: "sha256:" + strings.Repeat("1", 64),
	}
	raw, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeOwnerOnlyDurableAtomicFile(store.memoryStore.currentStateTransactionPath(), append(raw, '\n'), true); err != nil {
		t.Fatal(err)
	}
	if err := store.memoryStore.recoverCurrentStateTransactionLocked(); err == nil {
		t.Fatal("foreign transaction marker was accepted")
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("foreign staged file was removed: %v", err)
	}
}

func TestMemoryCurrentStateTransactionRejectsForeignSiblingPaths(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*memoryCurrentStateTransaction)
	}{
		{name: "manifest", mutate: func(marker *memoryCurrentStateTransaction) {
			marker.StageManifestPath = ".txn-0123456789abcdef01234567-foreign.json"
		}},
		{name: "shard", mutate: func(marker *memoryCurrentStateTransaction) {
			marker.Shards = []memoryCurrentStateTransactionShard{{Shard: 0, StagePath: ".txn-0123456789abcdef01234567-foreign.json", FinalPath: "00.json", NewDigest: "sha256:" + strings.Repeat("2", 64)}}
		}},
		{name: "exact", mutate: func(marker *memoryCurrentStateTransaction) {
			marker.ExactStagePath = ".txn-0123456789abcdef01234567-exact-state-index-foreign.json"
			marker.ExactFinalPath = "exact_state_paths.json"
			marker.NewExactDigest = "sha256:" + strings.Repeat("3", 64)
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store, _ := newExactStateBoundaryTestStore(t)
			marker := memoryCurrentStateTransaction{
				SchemaID: memoryCurrentStateTransactionSchemaID, Version: memoryCurrentStateTransactionVersion,
				TransactionID: "0123456789abcdef01234567", State: "prepared",
				StageManifestPath: ".txn-0123456789abcdef01234567-generations.json", FinalManifestPath: "generations.json",
				NewManifestDigest: "sha256:" + strings.Repeat("1", 64),
			}
			testCase.mutate(&marker)
			if err := store.currentStateTransactionMarker(marker); err == nil || !strings.Contains(err.Error(), "foreign") {
				t.Fatalf("foreign sibling path was accepted: %v", err)
			}
		})
	}
}

func TestMemoryCurrentStateTransactionRejectsForeignAtomicTempName(t *testing.T) {
	store, _ := newExactStateBoundaryTestStore(t)
	foreign := filepath.Join(store.currentStateRootPath(), "..txn-0123456789abcdef01234567-generations.json.tmp-foreign")
	if err := os.WriteFile(foreign, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := store.recoverCurrentStateTransactionLocked()
	if err == nil || !strings.Contains(err.Error(), "foreign current-state transaction atomic temporary name") {
		t.Fatalf("foreign atomic temporary name was accepted: %v", err)
	}
	if _, err := os.Lstat(foreign); err != nil {
		t.Fatalf("foreign atomic temporary name was removed: %v", err)
	}
}

func seedMemoryCurrentStateBatchProjectSet(t *testing.T, store *memoryStore, count int, persistFinalShards bool) map[int]struct{} {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	store.currentState = make(map[string]memoryCurrentState, count)
	store.currentStateByShard = make(map[int]map[string]memoryCurrentState, memoryCurrentStateShardCount)
	store.currentStateShardPayloads = map[int][]byte{}
	store.currentStateShardDigests = map[int]string{}
	store.currentStateProjectShardDigests = map[string]map[int]string{}
	store.currentKeysByProject = make(map[string]map[string]struct{}, count)
	store.currentKeyCountsByProject = make(map[string]int, count)
	store.currentKeysByProjectTopic = make(map[string]map[string]map[string]struct{}, count)
	store.currentTopicKeyCountsByProject = make(map[string]int, count)
	store.currentKeyIndexGeneration = make(map[string]uint64, count)
	store.currentTopicIndexGeneration = make(map[string]uint64, count)
	store.currentStateGenerationRecords = make(map[string]memoryCurrentStateGenerationRecord, count)
	store.currentStateDigestIndexesInitialized = false
	for shard := 0; shard < memoryCurrentStateShardCount; shard++ {
		store.currentStateByShard[shard] = map[string]memoryCurrentState{}
	}
	for index := 0; index < count; index++ {
		project := fmt.Sprintf("max-batch-project-%06d", index)
		entry := memoryStoreEntry{
			Project: project, FileName: "notes/state.md", TopicPath: "runbooks/max-batch",
			Summary: "state", EventID: fmt.Sprintf("max-batch-%06d", index), CreatedAt: "2026-08-12T00:00:00Z", Lifecycle: "durable",
		}
		state := memoryCurrentStateFromEntry(entry)
		key := memoryStoreKey(project, entry.FileName)
		shard := memoryCurrentStateShardForKey(key)
		store.currentState[key] = state
		store.currentStateByShard[shard][key] = state
		store.currentKeyIndexGeneration[project] = 1
		store.currentTopicIndexGeneration[project] = 1
	}
	store.ensureCurrentStateDigestIndexesLocked()
	for project := range store.currentKeyIndexGeneration {
		store.currentStateGenerationRecords[project] = memoryCurrentStateGenerationRecord{
			KeyGeneration: 1, TopicGeneration: 1,
			StateDigest: memoryCurrentStateRootDigest(project, 1, store.currentStateProjectShardDigests[project]),
		}
	}
	if err := store.setCurrentStateGenerationCardsAccumulatorLocked(store.currentStateGenerationRecords); err != nil {
		t.Fatal(err)
	}
	store.currentStateGenerationManifestLoaded = true
	store.currentStateGenerationManifestVersion = memoryCurrentStateGenerationVersion
	dirty := make(map[int]struct{}, memoryCurrentStateShardCount)
	for shard, states := range store.currentStateByShard {
		if len(states) == 0 {
			continue
		}
		dirty[shard] = struct{}{}
		if !persistFinalShards {
			continue
		}
		payload := append([]byte(nil), store.currentStateShardPayloads[shard]...)
		payload = append(payload, '\n')
		if err := writeOwnerOnlyDurableAtomicFile(store.currentStateShardPath(shard), payload, true); err != nil {
			t.Fatalf("persist final shard %d fixture: %v", shard, err)
		}
	}
	return dirty
}

func TestMemoryCurrentStateTransactionMaxCardsFullShardsRestartsFromDurableSession(t *testing.T) {
	t.Setenv("GO_MEMORY_STORE_BACKGROUND_HYDRATION_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_BACKGROUND_ENABLED", "false")
	store, _ := newExactStateBoundaryTestStore(t)
	dirty := seedMemoryCurrentStateBatchProjectSet(t, store, memoryCurrentStateGenerationMaxCards, true)
	if len(dirty) != memoryCurrentStateShardCount {
		t.Fatalf("max-card fixture covered %d shards, want %d", len(dirty), memoryCurrentStateShardCount)
	}
	for _, dir := range []string{
		store.currentStateGenerationCardsPath(),
		filepath.Join(store.currentStateRootPath(), memoryCurrentStateTransactionQuarantineDir),
	} {
		if err := ensureOwnerOnlyDirectory(dir, true); err != nil {
			t.Fatal(err)
		}
	}
	store.mu.Lock()
	store.memoryCurrentStateTransactionFault = func(event string) error {
		if event == "batch_session_durable" {
			return errors.New("injected max-card durable session restart")
		}
		return nil
	}
	err := store.persistCurrentStateShardsLocked(dirty)
	store.mu.Unlock()
	if err == nil || !errors.Is(err, errMemoryCurrentStateTransactionCommitted) || !strings.Contains(err.Error(), "max-card durable session restart") {
		t.Fatalf("max-card batch-session fault was not surfaced as committed: %v", err)
	}
	rootEntries, err := os.ReadDir(store.currentStateRootPath())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(rootEntries), memoryCurrentStateTransactionMaxFixedRootEntries-2; got != want {
		t.Fatalf("durable max-card/full-shard root entries=%d want=%d (peak minus marker/temp)", got, want)
	}
	restarted, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("restart max-card/full-shard durable session: %v", err)
	}
	restarted.mu.RLock()
	stateCount := len(restarted.currentState)
	cardCount := restarted.currentStateGenerationCardCount
	restarted.mu.RUnlock()
	if stateCount != memoryCurrentStateGenerationMaxCards || cardCount != memoryCurrentStateGenerationMaxCards {
		t.Fatalf("max-card restart recovered state/cards=%d/%d want=%d/%d", stateCount, cardCount, memoryCurrentStateGenerationMaxCards, memoryCurrentStateGenerationMaxCards)
	}
	for shard := 0; shard < memoryCurrentStateShardCount; shard++ {
		if _, err := os.Stat(restarted.currentStateShardPath(shard)); err != nil {
			t.Fatalf("max-card restart missing shard %d: %v", shard, err)
		}
	}
	remaining, err := os.ReadDir(restarted.currentStateRootPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range remaining {
		if strings.HasPrefix(entry.Name(), ".txn-") || strings.HasPrefix(entry.Name(), "..txn-") {
			t.Fatalf("max-card restart left transaction artifact %q", entry.Name())
		}
	}
}

func TestMemoryCurrentStateTransactionCompletedSessionCleansPayloadsAfterMarkerRemovalFault(t *testing.T) {
	t.Setenv("GO_MEMORY_STORE_BACKGROUND_HYDRATION_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_BACKGROUND_ENABLED", "false")
	store, _ := newExactStateBoundaryTestStore(t)
	dirty := seedMemoryCurrentStateBatchProjectSet(t, store, memoryCurrentStateTransactionMaxCards+1, false)
	markerRemovals := 0
	store.mu.Lock()
	store.memoryCurrentStateTransactionFault = func(event string) error {
		if event == "after_marker_removal" {
			markerRemovals++
			if markerRemovals == 2 {
				return errors.New("injected final batch after marker removal")
			}
		}
		return nil
	}
	err := store.persistCurrentStateShardsLocked(dirty)
	store.mu.Unlock()
	if err == nil || !errors.Is(err, errMemoryCurrentStateTransactionCommitted) || !strings.Contains(err.Error(), "final batch after marker removal") {
		t.Fatalf("final-batch marker-removal fault was not surfaced as committed: %v", err)
	}
	if markerRemovals != 2 {
		t.Fatalf("marker removals=%d want=2", markerRemovals)
	}
	sessionName, err := store.currentStateTransactionBatchSessionNameLocked()
	if err != nil {
		t.Fatal(err)
	}
	if sessionName == "" {
		t.Fatal("completed batch session was not preserved after marker-removal fault")
	}
	session, _, err := store.currentStateTransactionReadBatchSessionLocked(sessionName)
	if err != nil {
		t.Fatal(err)
	}
	if session.NextBatch != session.TotalBatches || len(session.BatchPayloadPaths) != 2 {
		t.Fatalf("fault session progress=%d/%d payloads=%d want completed with 2 payloads", session.NextBatch, session.TotalBatches, len(session.BatchPayloadPaths))
	}
	// Emulate a second crash after cleanup removed one payload but before the
	// session commit record. Recovery must not require that payload to reappear.
	removedPayload, err := store.currentStateTransactionAbsolutePath(session.BatchPayloadPaths[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(removedPayload); err != nil {
		t.Fatal(err)
	}
	if err := syncOwnerOnlyDirectory(store.currentStateRootPath()); err != nil {
		t.Fatal(err)
	}
	restarted, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("restart completed batch session after partial cleanup: %v", err)
	}
	if !restarted.currentStateGenerationManifestLoaded || restarted.currentStateGenerationManifestVersion != memoryCurrentStateGenerationVersion {
		t.Fatalf("completed session did not install manifest bookkeeping: loaded=%v version=%d", restarted.currentStateGenerationManifestLoaded, restarted.currentStateGenerationManifestVersion)
	}
	for _, relative := range append([]string{sessionName}, session.BatchPayloadPaths...) {
		path, pathErr := restarted.currentStateTransactionAbsolutePath(relative)
		if pathErr != nil {
			t.Fatal(pathErr)
		}
		if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("completed session cleanup left %q: %v", relative, statErr)
		}
	}
	if _, err := os.Lstat(restarted.currentStateTransactionPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed session restart left marker: %v", err)
	}
}

func TestMemoryCurrentStateTransactionCompletedSessionPreflightsSameProcessBatch(t *testing.T) {
	t.Setenv("GO_MEMORY_STORE_BACKGROUND_HYDRATION_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_BACKGROUND_ENABLED", "false")
	store, _ := newExactStateBoundaryTestStore(t)
	dirty := seedMemoryCurrentStateBatchProjectSet(t, store, memoryCurrentStateTransactionMaxCards+1, false)
	markerRemovals := 0
	store.mu.Lock()
	store.memoryCurrentStateTransactionFault = func(event string) error {
		if event == "after_marker_removal" {
			markerRemovals++
			if markerRemovals == 2 {
				return errors.New("injected final batch before same-process retry")
			}
		}
		return nil
	}
	err := store.persistCurrentStateShardsLocked(dirty)
	store.mu.Unlock()
	if err == nil || !errors.Is(err, errMemoryCurrentStateTransactionCommitted) || !strings.Contains(err.Error(), "final batch before same-process retry") {
		t.Fatalf("final-batch fault was not surfaced as committed: %v", err)
	}
	sessionName, err := store.currentStateTransactionBatchSessionNameLocked()
	if err != nil {
		t.Fatal(err)
	}
	if sessionName == "" {
		t.Fatal("completed batch session was not preserved for same-process retry")
	}
	store.mu.Lock()
	store.memoryCurrentStateTransactionFault = nil
	err = store.persistCurrentStateShardsLocked(dirty)
	store.mu.Unlock()
	if err != nil {
		t.Fatalf("same-process batch did not recover completed predecessor: %v", err)
	}
	for _, root := range []string{store.currentStateRootPath(), store.currentStateGenerationCardsPath()} {
		entries, readErr := os.ReadDir(root)
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".txn-") || strings.HasPrefix(entry.Name(), "..txn-") {
				t.Fatalf("same-process successor left transaction artifact in %q: %q", root, entry.Name())
			}
		}
	}
	restarted, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("restart after same-process successor batch: %v", err)
	}
	restarted.mu.RLock()
	stateCount := len(restarted.currentState)
	cardCount := restarted.currentStateGenerationCardCount
	restarted.mu.RUnlock()
	if stateCount != memoryCurrentStateTransactionMaxCards+1 || cardCount != memoryCurrentStateTransactionMaxCards+1 {
		t.Fatalf("same-process successor restart recovered state/cards=%d/%d want=%d/%d", stateCount, cardCount, memoryCurrentStateTransactionMaxCards+1, memoryCurrentStateTransactionMaxCards+1)
	}
	for _, root := range []string{restarted.currentStateRootPath(), restarted.currentStateGenerationCardsPath()} {
		entries, readErr := os.ReadDir(root)
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".txn-") || strings.HasPrefix(entry.Name(), "..txn-") {
				t.Fatalf("restart after same-process successor left transaction artifact in %q: %q", root, entry.Name())
			}
		}
	}
}

func TestMemoryCurrentStateTransactionBatches257ProjectsAcrossRestart(t *testing.T) {
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_BACKGROUND_HYDRATION_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_BACKGROUND_ENABLED", "false")
	root := filepath.Join(t.TempDir(), "memory-bank")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", filepath.Join(root, "_contextlattice", "memory_write_history.ndjson"))
	t.Setenv("GO_MEMORY_STORE_ACCESS_LOG_PATH", filepath.Join(root, "_contextlattice", "memory_access.ndjson"))
	t.Setenv("GO_MEMORY_GRAPH_EDGE_PATH", filepath.Join(root, "_contextlattice", "memory_edges.ndjson"))
	policy := memoryStorePolicy{enabled: true, rootPath: root, currentStatePath: filepath.Join(root, "_contextlattice", "memory_current_state"), exactStateIndexPath: filepath.Join(root, "_contextlattice", "exact_state_paths.json"), historyPath: filepath.Join(root, "_contextlattice", "memory_write_history.ndjson"), accessLogPath: filepath.Join(root, "_contextlattice", "memory_access.ndjson"), edgePath: filepath.Join(root, "_contextlattice", "memory_edges.ndjson"), exactStateMaxPaths: 100000}
	store := &memoryStore{
		policy: policy, currentState: map[string]memoryCurrentState{}, currentStateByShard: map[int]map[string]memoryCurrentState{}, currentStateShardPayloads: map[int][]byte{}, currentStateShardDigests: map[int]string{}, currentStateProjectShardDigests: map[string]map[int]string{}, currentKeysByProject: map[string]map[string]struct{}{}, currentKeyCountsByProject: map[string]int{}, currentKeysByProjectTopic: map[string]map[string]map[string]struct{}{}, currentTopicKeyCountsByProject: map[string]int{}, currentKeyIndexGeneration: map[string]uint64{}, currentTopicIndexGeneration: map[string]uint64{}, currentStateGenerationRecords: map[string]memoryCurrentStateGenerationRecord{}, latestTopic: map[string]string{}, latestHash: map[string]string{}, latestHorizon: map[string]int{}, latestLifecycle: map[string]string{}, latestStorageTier: map[string]string{}, lastAccess: map[string]time.Time{}, confidence: map[string]confidenceState{}, rollupCache: map[string]topicRollupCacheEntry{}, edges: map[string]memoryEdgeEntry{}, edgeOrdinal: map[string]int64{}, edgeAdjacency: map[string]map[string]struct{}{}, exactStatePaths: map[string]struct{}{}, pathLocks: map[string]*memoryPathLock{},
		currentStateGenerationManifestLoaded: true, currentStateGenerationManifestVersion: memoryCurrentStateGenerationVersion,
	}
	if err := ensureOwnerOnlyDirectory(store.currentStateRootPath(), true); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 257; index++ {
		project := fmt.Sprintf("batch-project-%03d", index)
		fileName := "notes/state.md"
		entry := memoryStoreEntry{Project: project, FileName: fileName, TopicPath: "runbooks/batch", Summary: "batch", EventID: fmt.Sprintf("batch-%03d", index), CreatedAt: nowUTCISO(), Lifecycle: "durable"}
		state := memoryCurrentStateFromEntry(entry)
		key := memoryStoreKey(project, fileName)
		shard := memoryCurrentStateShardForKey(key)
		if store.currentStateByShard[shard] == nil {
			store.currentStateByShard[shard] = map[string]memoryCurrentState{}
		}
		store.currentState[key] = state
		store.currentStateByShard[shard][key] = state
		store.currentKeyIndexGeneration[project] = 1
		store.currentTopicIndexGeneration[project] = 1
	}
	store.mu.Lock()
	store.ensureCurrentStateDigestIndexesLocked()
	for project := range store.currentKeyIndexGeneration {
		store.currentStateGenerationRecords[project] = memoryCurrentStateGenerationRecord{KeyGeneration: 1, TopicGeneration: 1, StateDigest: memoryCurrentStateRootDigest(project, 1, store.currentStateProjectShardDigests[project])}
	}
	store.setCurrentStateGenerationCardsAccumulatorLocked(store.currentStateGenerationRecords)
	dirty := map[int]struct{}{}
	for shard := range store.currentStateByShard {
		if len(store.currentStateByShard[shard]) > 0 {
			dirty[shard] = struct{}{}
		}
	}
	store.memoryCurrentStateTransactionFault = func(event string) error {
		if event == "after_batch_progress" {
			return errors.New("injected after_batch_progress")
		}
		return nil
	}
	err := store.persistCurrentStateShardsLocked(dirty)
	store.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "after_batch_progress") {
		t.Fatalf("batch progress fault was not surfaced as committed: %v", err)
	}
	restarted, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("restart after 257-project transaction batches: %v", err)
	}
	restarted.mu.RLock()
	cardCount := restarted.currentStateGenerationCardCount
	restarted.mu.RUnlock()
	if cardCount != 257 {
		t.Fatalf("restart recovered card count=%d want=257", cardCount)
	}
	if _, err := os.Stat(restarted.currentStateTransactionPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("batch marker remained after commit: %v", err)
	}
	entries, err := os.ReadDir(restarted.currentStateRootPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if memoryCurrentStateTransactionOrphanKind(entry.Name()) == "batch" {
			t.Fatalf("batch session unexpectedly remained: %s", entry.Name())
		}
	}
}

func TestMemoryCurrentStateGenerationNoManifestMigrationBatchesCardsAcrossRestart(t *testing.T) {
	t.Setenv("GO_MEMORY_STORE_BACKGROUND_HYDRATION_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_BACKGROUND_ENABLED", "false")
	store, _ := newExactStateBoundaryTestStore(t)
	store.currentStateGenerationManifestLoaded = true
	store.currentStateGenerationManifestVersion = memoryCurrentStateGenerationVersion
	store.mu.Lock()
	store.ensureCurrentStateMapLocked()
	store.ensureCurrentKeyIndexLocked()
	store.currentStateDigestIndexesInitialized = false
	for index := 0; index < 257; index++ {
		project := fmt.Sprintf("no-manifest-project-%03d", index)
		entry := memoryStoreEntry{Project: project, FileName: "notes/state.md", TopicPath: "runbooks/no-manifest", Summary: "state", EventID: fmt.Sprintf("no-manifest-%03d", index), CreatedAt: nowUTCISO(), Lifecycle: "durable"}
		state := memoryCurrentStateFromEntry(entry)
		key := memoryStoreKey(project, entry.FileName)
		shard := memoryCurrentStateShardForKey(key)
		if store.currentStateByShard[shard] == nil {
			store.currentStateByShard[shard] = map[string]memoryCurrentState{}
		}
		store.currentState[key] = state
		store.currentStateByShard[shard][key] = state
		store.currentKeyIndexGeneration[project] = 1
		store.currentTopicIndexGeneration[project] = 1
	}
	store.ensureCurrentStateDigestIndexesLocked()
	for project := range store.currentKeyIndexGeneration {
		store.currentStateGenerationRecords[project] = memoryCurrentStateGenerationRecord{KeyGeneration: 1, TopicGeneration: 1, StateDigest: memoryCurrentStateRootDigest(project, 1, store.currentStateProjectShardDigests[project])}
	}
	if err := store.setCurrentStateGenerationCardsAccumulatorLocked(store.currentStateGenerationRecords); err != nil {
		store.mu.Unlock()
		t.Fatal(err)
	}
	dirty := map[int]struct{}{}
	for shard, states := range store.currentStateByShard {
		if len(states) > 0 {
			dirty[shard] = struct{}{}
		}
	}
	if err := store.persistCurrentStateShardsLocked(dirty); err != nil {
		store.mu.Unlock()
		t.Fatalf("seed no-manifest migration fixture: %v", err)
	}
	store.mu.Unlock()

	if err := os.Remove(store.currentStateGenerationPath()); err != nil {
		t.Fatalf("remove generation manifest for migration fixture: %v", err)
	}
	cardEntries, err := os.ReadDir(store.currentStateGenerationCardsPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range cardEntries {
		if err := os.Remove(filepath.Join(store.currentStateGenerationCardsPath(), entry.Name())); err != nil {
			t.Fatalf("remove generation card %q for migration fixture: %v", entry.Name(), err)
		}
	}

	restarted, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("no-manifest migration did not recover 257 projects: %v", err)
	}
	restarted.mu.RLock()
	cardCount := restarted.currentStateGenerationCardCount
	restarted.mu.RUnlock()
	if cardCount != 257 {
		t.Fatalf("no-manifest migration recovered card count=%d want=257", cardCount)
	}
	if _, err := newMemoryStoreFromEnv(); err != nil {
		t.Fatalf("restart after no-manifest card migration failed: %v", err)
	}
}

func TestMemoryCurrentStateTransactionCommittedBatchSessionPreservesRecovery(t *testing.T) {
	t.Setenv("GO_MEMORY_STORE_BACKGROUND_HYDRATION_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_BACKGROUND_ENABLED", "false")
	store, _ := newExactStateBoundaryTestStore(t)
	store.currentStateGenerationManifestLoaded = true
	store.currentStateGenerationManifestVersion = memoryCurrentStateGenerationVersion
	store.mu.Lock()
	store.ensureCurrentStateMapLocked()
	store.ensureCurrentKeyIndexLocked()
	store.currentStateDigestIndexesInitialized = false
	for index := 0; index < 257; index++ {
		project := fmt.Sprintf("committed-batch-project-%03d", index)
		entry := memoryStoreEntry{Project: project, FileName: "notes/state.md", TopicPath: "runbooks/committed-batch", Summary: "batch", EventID: fmt.Sprintf("committed-batch-%03d", index), CreatedAt: nowUTCISO(), Lifecycle: "durable"}
		state := memoryCurrentStateFromEntry(entry)
		key := memoryStoreKey(project, entry.FileName)
		shard := memoryCurrentStateShardForKey(key)
		store.currentState[key] = state
		store.currentStateByShard[shard][key] = state
		store.currentKeyIndexGeneration[project] = 1
		store.currentTopicIndexGeneration[project] = 1
	}
	store.ensureCurrentStateDigestIndexesLocked()
	for project := range store.currentKeyIndexGeneration {
		store.currentStateGenerationRecords[project] = memoryCurrentStateGenerationRecord{KeyGeneration: 1, TopicGeneration: 1, StateDigest: memoryCurrentStateRootDigest(project, 1, store.currentStateProjectShardDigests[project])}
	}
	if err := store.setCurrentStateGenerationCardsAccumulatorLocked(store.currentStateGenerationRecords); err != nil {
		store.mu.Unlock()
		t.Fatal(err)
	}
	dirty := map[int]struct{}{}
	for shard, states := range store.currentStateByShard {
		if len(states) > 0 {
			dirty[shard] = struct{}{}
		}
	}
	store.memoryCurrentStateTransactionAtomicWriteFault = func(event string) error {
		if event == "batch_session_after_replace" {
			return &ownerOnlyAtomicWriteError{Operation: "injected committed batch session write", Committed: true, Err: errors.New("injected committed batch session write")}
		}
		return nil
	}
	err := store.persistCurrentStateShardsLocked(dirty)
	store.mu.Unlock()
	if err == nil || !errors.Is(err, errMemoryCurrentStateTransactionCommitted) {
		t.Fatalf("committed batch-session write was not surfaced as committed: %v", err)
	}
	entries, err := os.ReadDir(store.currentStateRootPath())
	if err != nil {
		t.Fatal(err)
	}
	var sessionName string
	for _, entry := range entries {
		if memoryCurrentStateTransactionOrphanKind(entry.Name()) == "batch" {
			sessionName = entry.Name()
		}
	}
	if sessionName == "" {
		t.Fatal("committed batch session was deleted instead of preserved")
	}
	if _, err := os.Stat(filepath.Join(store.currentStateRootPath(), ".txn-"+strings.TrimSuffix(strings.TrimPrefix(sessionName, ".txn-"), "-batch.json")+"-generations.json")); err != nil {
		t.Fatalf("staged manifest was not preserved with committed session: %v", err)
	}
	restarted, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("restart did not recover committed batch session: %v", err)
	}
	restarted.mu.RLock()
	cardCount := restarted.currentStateGenerationCardCount
	restarted.mu.RUnlock()
	if cardCount != 257 {
		t.Fatalf("committed session restart recovered card count=%d want=257", cardCount)
	}
	if _, err := os.Stat(restarted.currentStateTransactionPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committed session restart left transaction marker: %v", err)
	}
	remaining, err := os.ReadDir(restarted.currentStateRootPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range remaining {
		if memoryCurrentStateTransactionOrphanKind(entry.Name()) == "batch" || memoryCurrentStateTransactionOrphanKind(entry.Name()) == "batch-payload" {
			t.Fatalf("committed session recovery left batch artifact %q", entry.Name())
		}
	}
}

func TestMemoryCurrentStateHistoryReplay257ProjectsUsesBoundedBatches(t *testing.T) {
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_BACKGROUND_HYDRATION_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_BACKGROUND_ENABLED", "false")
	root := filepath.Join(t.TempDir(), "memory-bank")
	historyPath := filepath.Join(root, "_contextlattice", "memory_write_history.ndjson")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", historyPath)
	t.Setenv("GO_MEMORY_STORE_ACCESS_LOG_PATH", filepath.Join(root, "_contextlattice", "memory_access.ndjson"))
	t.Setenv("GO_MEMORY_GRAPH_EDGE_PATH", filepath.Join(root, "_contextlattice", "memory_edges.ndjson"))
	if err := ensureOwnerOnlyDirectory(filepath.Dir(historyPath), true); err != nil {
		t.Fatal(err)
	}
	history, err := os.OpenFile(historyPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, ownerOnlyFileMode)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 257; index++ {
		entry := memoryStoreEntry{EventID: fmt.Sprintf("history-batch-%03d", index), Project: fmt.Sprintf("history-project-%03d", index), FileName: "notes/replay.md", TopicPath: "runbooks/replay", Summary: "replayed", DataClass: dataClassLearningMemory, Lifecycle: "durable", CreatedAt: nowUTCISO()}
		raw, marshalErr := json.Marshal(entry)
		if marshalErr != nil {
			history.Close()
			t.Fatal(marshalErr)
		}
		if _, writeErr := history.Write(append(raw, '\n')); writeErr != nil {
			history.Close()
			t.Fatal(writeErr)
		}
	}
	if err := history.Sync(); err != nil {
		history.Close()
		t.Fatal(err)
	}
	if err := history.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("real constructor history replay failed: %v", err)
	}
	store.mu.RLock()
	stateCount := len(store.currentState)
	cardCount := store.currentStateGenerationCardCount
	store.mu.RUnlock()
	if stateCount != 257 || cardCount != 257 {
		t.Fatalf("history replay recovered state=%d cards=%d want 257/257", stateCount, cardCount)
	}
	restarted, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("restart after real constructor history replay failed: %v", err)
	}
	restarted.mu.RLock()
	stateCount = len(restarted.currentState)
	cardCount = restarted.currentStateGenerationCardCount
	restarted.mu.RUnlock()
	if stateCount != 257 || cardCount != 257 {
		t.Fatalf("restart recovered state=%d cards=%d want 257/257", stateCount, cardCount)
	}
}

func TestMemoryCurrentStateTransactionAbruptDeathDuringCardAtomicTempIsReconciled(t *testing.T) {
	events := []string{"card_after_temp_create", "card_after_temp_write", "card_after_temp_sync"}
	if selected := strings.TrimSpace(os.Getenv("CONTEXTLATTICE_CURRENT_STATE_CARD_TEMP_ABORT_EVENT")); selected != "" {
		events = []string{selected}
	}
	for _, event := range events {
		t.Run(event, func(t *testing.T) {
			runMemoryCurrentStateTransactionAbruptDeathDuringCardAtomicTempIsReconciled(t, event)
		})
	}
}

func runMemoryCurrentStateTransactionAbruptDeathDuringCardAtomicTempIsReconciled(t *testing.T, faultEvent string) {
	if os.Getenv("CONTEXTLATTICE_CURRENT_STATE_CARD_TEMP_ABORT_CHILD") == "1" {
		if childRoot := os.Getenv("CONTEXTLATTICE_CURRENT_STATE_CARD_TEMP_ABORT_ROOT"); childRoot != "" {
			_ = os.Setenv("GO_MEMORY_STORE_ROOT", childRoot)
		}
		store, err := newMemoryStoreFromEnv()
		if err != nil {
			os.Exit(120)
		}
		store.memoryCurrentStateTransactionAtomicWriteFault = func(event string) error {
			if event == faultEvent {
				// The process dies before the card temp is renamed. Recovery must
				// classify the exact server-owned name and retain a bounded receipt
				// rather than silently ignoring it, at create, write, and fsync.
				os.Exit(127)
			}
			return nil
		}
		if err := store.persistAndRecordEntry(memoryStoreEntry{Project: "alpha", FileName: "notes/card-temp.md", TopicPath: "runbooks/card-temp", Summary: "new", Lifecycle: "durable", DataClass: dataClassLearningMemory, CreatedAt: nowUTCISO(), EventID: "card-temp-child"}); err != nil {
			os.Exit(121)
		}
		os.Exit(122)
	}
	t.Setenv("GO_MEMORY_STORE_BACKGROUND_HYDRATION_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_BACKGROUND_ENABLED", "false")
	root := filepath.Join(t.TempDir(), "memory-bank")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", filepath.Join(root, "_contextlattice", "memory_write_history.ndjson"))
	t.Setenv("GO_MEMORY_STORE_ACCESS_LOG_PATH", filepath.Join(root, "_contextlattice", "memory_access.ndjson"))
	t.Setenv("GO_MEMORY_GRAPH_EDGE_PATH", filepath.Join(root, "_contextlattice", "memory_edges.ndjson"))
	store, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("create card-temp fixture: %v", err)
	}
	if err := store.persistAndRecordEntry(memoryStoreEntry{Project: "alpha", FileName: "notes/card-temp.md", TopicPath: "runbooks/card-temp", Summary: "old", Lifecycle: "durable", EventID: "card-temp-old"}); err != nil {
		t.Fatalf("seed card-temp fixture: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestMemoryCurrentStateTransactionAbruptDeathDuringCardAtomicTempIsReconciled", "-test.count=1")
	cmd.Env = append(os.Environ(), "CONTEXTLATTICE_CURRENT_STATE_CARD_TEMP_ABORT_CHILD=1", "CONTEXTLATTICE_CURRENT_STATE_CARD_TEMP_ABORT_ROOT="+root, "CONTEXTLATTICE_CURRENT_STATE_CARD_TEMP_ABORT_EVENT="+faultEvent)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("card-temp child unexpectedly succeeded: output=%s", output)
	}
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 127 {
		t.Fatalf("card-temp child exit=%v output=%s", err, output)
	}
	restarted, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("restart after card temp abrupt death: %v", err)
	}
	state, ok := restarted.currentStateFor("alpha", "notes/card-temp.md")
	if !ok || state.Entry.Summary != "old" {
		t.Fatalf("card temp altered durable state: ok=%v state=%#v", ok, state)
	}
	for _, rootPath := range []string{restarted.currentStateRootPath(), restarted.currentStateGenerationCardsPath()} {
		entries, readErr := os.ReadDir(rootPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".txn-") || strings.HasPrefix(entry.Name(), "..txn-") {
				t.Fatalf("transaction artifact remained live in %q: %s", rootPath, entry.Name())
			}
		}
	}
	quarantineEntries, err := os.ReadDir(filepath.Join(restarted.currentStateRootPath(), memoryCurrentStateTransactionQuarantineDir))
	if err != nil {
		t.Fatalf("read card-temp quarantine: %v", err)
	}
	if len(quarantineEntries) < 2 {
		t.Fatalf("card-temp death did not quarantine shard and temp artifacts: %d", len(quarantineEntries))
	}
}

func TestMemoryCurrentStateTransactionAbruptDeathDuringChunkWriteQuarantinesTruncatedTemp(t *testing.T) {
	if os.Getenv("CONTEXTLATTICE_CURRENT_STATE_CHUNK_ABORT_CHILD") == "1" {
		root := os.Getenv("CONTEXTLATTICE_CURRENT_STATE_CHUNK_ABORT_ROOT")
		if root == "" {
			os.Exit(130)
		}
		_ = os.Setenv("GO_MEMORY_STORE_ROOT", root)
		store, err := newMemoryStoreFromEnv()
		if err != nil {
			os.Exit(131)
		}
		content := []byte(strings.Repeat("x", 128*1024))
		path := filepath.Join(store.currentStateRootPath(), ".txn-0123456789abcdef01234567-generations.json")
		hook := func(event string) error {
			if event == "after_temp_write_chunk" {
				// The first 4 KiB chunk is durable; the process dies before the
				// remaining bytes and fsync. Recovery must quarantine the exact
				// server-owned nonzero truncated temp before any load/resume.
				os.Exit(132)
			}
			return nil
		}
		if err := writeOwnerOnlyDurableAtomicFileWithHook(path, content, true, hook); err != nil {
			os.Exit(133)
		}
		os.Exit(134)
	}
	t.Setenv("GO_MEMORY_STORE_BACKGROUND_HYDRATION_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_BACKGROUND_ENABLED", "false")
	root := filepath.Join(t.TempDir(), "memory-bank")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	store, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestMemoryCurrentStateTransactionAbruptDeathDuringChunkWriteQuarantinesTruncatedTemp", "-test.count=1")
	cmd.Env = append(os.Environ(), "CONTEXTLATTICE_CURRENT_STATE_CHUNK_ABORT_CHILD=1", "CONTEXTLATTICE_CURRENT_STATE_CHUNK_ABORT_ROOT="+root)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("chunk-abort child unexpectedly succeeded: output=%s", output)
	}
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 132 {
		t.Fatalf("chunk-abort child exit=%v output=%s", err, output)
	}
	restarted, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("restart after truncated atomic temp: %v", err)
	}
	for _, entry := range []string{"..txn-0123456789abcdef01234567-generations.json.tmp-"} {
		entries, readErr := os.ReadDir(restarted.currentStateRootPath())
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, candidate := range entries {
			if strings.HasPrefix(candidate.Name(), entry) {
				t.Fatalf("truncated atomic temp remained live: %q", candidate.Name())
			}
		}
	}
	quarantineEntries, err := os.ReadDir(filepath.Join(restarted.currentStateRootPath(), memoryCurrentStateTransactionQuarantineDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(quarantineEntries) == 0 {
		t.Fatal("truncated atomic temp was not quarantined")
	}
	_ = store
}

func TestMemoryCurrentStateGenerationCardCapsRejectMutationBeforeDurableWrite(t *testing.T) {
	store, _ := newExactStateBoundaryTestStore(t)
	store.mu.Lock()
	store.currentStateGenerationCardsDigestInitialized = true
	store.currentStateGenerationCardCount = memoryCurrentStateGenerationMaxCards
	store.currentStateGenerationCardBytes = memoryCurrentStateGenerationMaxCardBytes
	store.mu.Unlock()
	if _, _, err := store.put(normalizedWrite{project: "overflow-project", fileName: "notes/overflow.md", topicPath: "runbooks/overflow", content: "must reject", lifecycle: "durable"}); err == nil || !strings.Contains(err.Error(), "card count exceeds cap") {
		t.Fatalf("100001st project mutation was accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.policy.rootPath, "overflow-project", "notes", "overflow.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cap rejection committed the memory file: %v", err)
	}
}

func TestMemoryCurrentStateGenerationCardPreflightIncludesNineToTenGrowth(t *testing.T) {
	store, _ := newExactStateBoundaryTestStore(t)
	project := "alpha"
	oldRecord := memoryCurrentStateGenerationRecord{KeyGeneration: 9, TopicGeneration: 9, StateDigest: memoryCurrentStateRootDigest(project, 9, nil)}
	newRecord := memoryCurrentStateGenerationRecord{KeyGeneration: 10, TopicGeneration: 10, StateDigest: memoryCurrentStateRootDigest(project, 10, nil)}
	oldPayload, err := memoryCurrentStateGenerationCardPayload(project, oldRecord)
	if err != nil {
		t.Fatal(err)
	}
	newPayload, err := memoryCurrentStateGenerationCardPayload(project, newRecord)
	if err != nil {
		t.Fatal(err)
	}
	if len(newPayload) <= len(oldPayload) {
		t.Fatalf("generation 9 -> 10 card payload did not grow: old=%d new=%d", len(oldPayload), len(newPayload))
	}
	store.mu.Lock()
	store.ensureCurrentStateMapLocked()
	store.ensureCurrentKeyIndexLocked()
	store.currentStateGenerationRecords = map[string]memoryCurrentStateGenerationRecord{project: oldRecord}
	store.currentKeyIndexGeneration[project] = 9
	store.currentTopicIndexGeneration[project] = 9
	if err := store.setCurrentStateGenerationCardsAccumulatorLocked(store.currentStateGenerationRecords); err != nil {
		store.mu.Unlock()
		t.Fatal(err)
	}
	// Leave the projected state one byte over the cap after replacing the old
	// card. The exact
	// post-mutation generation-10 payload must be rejected before file/history
	// mutation, whereas a stale generation-9 preflight would pass.
	store.currentStateGenerationCardBytes = memoryCurrentStateGenerationMaxCardBytes - int64(len(newPayload)) + int64(len(oldPayload)) + 1
	store.mu.Unlock()

	historyBefore, err := os.ReadFile(store.policy.historyPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if _, _, err := store.put(normalizedWrite{project: project, fileName: "notes/generation-boundary.md", topicPath: "runbooks/generation-boundary", content: "must reject before history", lifecycle: "durable"}); err == nil || !strings.Contains(err.Error(), "cards exceed byte cap") {
		t.Fatalf("generation 9 -> 10 overbyte mutation was accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.policy.rootPath, project, "notes", "generation-boundary.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("generation boundary rejection committed the memory file: %v", err)
	}
	historyAfter, err := os.ReadFile(store.policy.historyPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if !bytes.Equal(historyBefore, historyAfter) {
		t.Fatal("generation boundary rejection appended history before capacity admission")
	}
	store.mu.RLock()
	gotGeneration := store.currentKeyIndexGeneration[project]
	store.mu.RUnlock()
	if gotGeneration != 9 {
		t.Fatalf("generation boundary rejection advanced generation: got=%d want=9", gotGeneration)
	}
}

func TestMemoryCurrentStateGenerationCardMigrationCapsBeforeCommit(t *testing.T) {
	store, _ := newExactStateBoundaryTestStore(t)
	store.mu.Lock()
	store.currentStateGenerationRecords = make(map[string]memoryCurrentStateGenerationRecord, memoryCurrentStateGenerationMaxCards+1)
	store.currentState = make(map[string]memoryCurrentState, memoryCurrentStateGenerationMaxCards+1)
	for index := 0; index <= memoryCurrentStateGenerationMaxCards; index++ {
		project := fmt.Sprintf("migration-overflow-%06d", index)
		store.currentStateGenerationRecords[project] = memoryCurrentStateGenerationRecord{KeyGeneration: 1, TopicGeneration: 1, StateDigest: memoryCurrentStateRootDigest(project, 1, nil)}
		store.currentState[memoryStoreKey(project, "notes/state.md")] = memoryCurrentState{}
	}
	store.mu.Unlock()
	if err := store.migrateCurrentStateGenerationCardsLocked(); err == nil || !strings.Contains(err.Error(), "card count exceeds cap") {
		t.Fatalf("oversized migration was accepted: %v", err)
	}
	entries, err := os.ReadDir(store.currentStateGenerationCardsPath())
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("oversized migration wrote card files before cap validation: %d", len(entries))
	}
}

func TestMemoryCurrentStateTransactionBatchSessionRefsBoundedFor100kAndEscapedBoundary(t *testing.T) {
	transactionID := "0123456789abcdef01234567"
	session := memoryCurrentStateTransactionBatchSession{
		SchemaID: memoryCurrentStateTransactionSchemaID, Version: memoryCurrentStateTransactionVersion,
		TransactionID: transactionID, State: "prepared", CardCount: memoryCurrentStateGenerationMaxCards,
		TotalBatches:      memoryCurrentStateTransactionBatchPayloadCount,
		BatchPayloadPaths: make([]string, memoryCurrentStateTransactionBatchPayloadCount),
		StageManifestPath: ".txn-" + transactionID + "-generations.json", FinalManifestPath: "generations.json",
		NewManifestDigest: "sha256:" + strings.Repeat("1", 64),
	}
	for index := range session.BatchPayloadPaths {
		session.BatchPayloadPaths[index] = memoryCurrentStateTransactionBatchPayloadPath(transactionID, index)
	}
	if err := validateMemoryCurrentStateTransactionBatchSession(session); err != nil {
		t.Fatal(err)
	}
	sessionRaw, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(sessionRaw)) >= memoryCurrentStateTransactionBatchMaxBytes {
		t.Fatalf("100k-card ref-only session grew beyond cap: bytes=%d cap=%d", len(sessionRaw), memoryCurrentStateTransactionBatchMaxBytes)
	}
	// Find an accepted project whose JSON escaping reaches the aggregate card
	// boundary, then prove its length-prefixed batch envelope still fits the
	// durable 128 MiB artifact cap. The next escaped pair must fail the 64 MiB
	// card cap.
	record := memoryCurrentStateGenerationRecord{KeyGeneration: 1, TopicGeneration: 1, StateDigest: memoryCurrentStateRootDigest("escaped", 1, nil)}
	base, err := memoryCurrentStateGenerationCardPayload("escaped", record)
	if err != nil {
		t.Fatal(err)
	}
	makeEscapedPayload := func(quoteCount int) ([]byte, error) {
		return memoryCurrentStateGenerationCardPayload(strings.Repeat("\"", quoteCount), record)
	}
	quotes := (int(memoryCurrentStateGenerationMaxCardBytes) - (len(base) - len("escaped"))) / 2
	if quotes < 1 {
		t.Fatal("card cap is too small for escaped boundary fixture")
	}
	escapedProject := strings.Repeat("\"", quotes)
	escapedPayload, err := makeEscapedPayload(quotes)
	if err != nil {
		t.Fatal(err)
	}
	for len(escapedPayload) > int(memoryCurrentStateGenerationMaxCardBytes) {
		quotes--
		escapedProject = strings.Repeat("\"", quotes)
		escapedPayload, err = makeEscapedPayload(quotes)
		if err != nil {
			t.Fatal(err)
		}
	}
	for {
		nextPayload, nextErr := makeEscapedPayload(quotes + 1)
		if nextErr != nil || len(nextPayload) > int(memoryCurrentStateGenerationMaxCardBytes) {
			break
		}
		quotes++
		escapedProject = strings.Repeat("\"", quotes)
		escapedPayload = nextPayload
	}
	if _, totalBytes, capacityErr := memoryCurrentStateGenerationCardSetCapacity(map[string]memoryCurrentStateGenerationRecord{escapedProject: record}); capacityErr != nil || totalBytes > memoryCurrentStateGenerationMaxCardBytes {
		t.Fatalf("escaped boundary card was rejected unexpectedly: bytes=%d err=%v", totalBytes, capacityErr)
	}
	tooLargeProject := escapedProject + "\""
	if _, _, oversizedErr := memoryCurrentStateGenerationCardSetCapacity(map[string]memoryCurrentStateGenerationRecord{tooLargeProject: record}); oversizedErr == nil {
		t.Fatal("escaped boundary card did not fail aggregate cap")
	}
	batchCard := memoryCurrentStateTransactionBatchCard{Project: escapedProject, Payload: escapedPayload, OldDigest: "", NewDigest: memoryCurrentStateTransactionDigest(escapedPayload)}
	batchRaw, err := encodeMemoryCurrentStateTransactionBatchPayload([]memoryCurrentStateTransactionBatchCard{batchCard})
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(batchRaw)) > memoryCurrentStateTransactionBatchPayloadMaxBytes {
		t.Fatalf("escaped boundary batch payload exceeded ref artifact cap: bytes=%d cap=%d", len(batchRaw), memoryCurrentStateTransactionBatchPayloadMaxBytes)
	}
}

func TestMemoryCurrentStateGenerationCardMigrationRejectsAggregateBytesBeforeCommit(t *testing.T) {
	store, _ := newExactStateBoundaryTestStore(t)
	store.mu.Lock()
	store.currentStateGenerationRecords = make(map[string]memoryCurrentStateGenerationRecord, 36)
	store.currentState = make(map[string]memoryCurrentState, 36)
	for index := 0; index < 36; index++ {
		project := fmt.Sprintf("long-project-%02d-%s", index, strings.Repeat("p", 1900000))
		store.currentStateGenerationRecords[project] = memoryCurrentStateGenerationRecord{KeyGeneration: 1, TopicGeneration: 1, StateDigest: memoryCurrentStateRootDigest(project, 1, nil)}
		store.currentState[memoryStoreKey(project, "notes/state.md")] = memoryCurrentState{}
	}
	store.mu.Unlock()
	if err := store.migrateCurrentStateGenerationCardsLocked(); err == nil || !strings.Contains(err.Error(), "byte cap") {
		t.Fatalf("overbyte migration was accepted: %v", err)
	}
	entries, err := os.ReadDir(store.currentStateGenerationCardsPath())
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("overbyte migration wrote card files before cap validation: %d", len(entries))
	}
}

func TestMemoryCurrentStateTransactionOrphanEnumerationCapsSkippedEntries(t *testing.T) {
	t.Setenv("GO_MEMORY_STORE_BACKGROUND_HYDRATION_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_BACKGROUND_ENABLED", "false")
	store, _ := newExactStateBoundaryTestStore(t)
	root := store.currentStateRootPath()
	if err := ensureOwnerOnlyDirectory(root, true); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if memoryCurrentStateTransactionMaxFixedRootEntries != 526 {
		t.Fatalf("fixed-root cap=%d want exact max-card/full-shard peak 526", memoryCurrentStateTransactionMaxFixedRootEntries)
	}
	for index := len(entries); index < memoryCurrentStateTransactionMaxFixedRootEntries; index++ {
		path := filepath.Join(root, fmt.Sprintf("ordinary-entry-%04d", index))
		if err := os.WriteFile(path, []byte("ordinary"), 0o600); err != nil {
			t.Fatalf("write skipped ordinary entry %d: %v", index, err)
		}
	}
	if err := store.recoverCurrentStateTransactionLocked(); err != nil {
		t.Fatalf("exact fixed-root entry cap was rejected: %v", err)
	}
	overflow := filepath.Join(root, "ordinary-entry-overflow")
	if err := os.WriteFile(overflow, []byte("ordinary"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = store.recoverCurrentStateTransactionLocked()
	if err == nil || !strings.Contains(err.Error(), "entry count") || !strings.Contains(err.Error(), "exceeds cap") {
		t.Fatalf("unbounded skipped-entry root was accepted: %v", err)
	}
	if _, err := os.Stat(overflow); err != nil {
		t.Fatalf("entry-cap rejection removed an ordinary entry: %v", err)
	}
}

func TestMemoryCurrentStateTransactionAbruptDeathQuarantinesMarkerlessStages(t *testing.T) {
	if os.Getenv("CONTEXTLATTICE_CURRENT_STATE_ABORT_CHILD") == "1" {
		if childRoot := os.Getenv("CONTEXTLATTICE_CURRENT_STATE_ABORT_ROOT"); childRoot != "" {
			_ = os.Setenv("GO_MEMORY_STORE_ROOT", childRoot)
		}
		store, err := newMemoryStoreFromEnv()
		if err != nil {
			os.Exit(90)
		}
		store.memoryCurrentStateTransactionFault = func(event string) error {
			if event == "before_marker" {
				// This is intentionally an abrupt process death: staged shard,
				// card, and manifest files are durable but no transaction marker
				// exists to authorize roll-forward.
				os.Exit(97)
			}
			return nil
		}
		if err := store.persistAndRecordEntry(memoryStoreEntry{
			Project: "alpha", FileName: "notes/abrupt.md", TopicPath: "runbooks/abrupt",
			Summary: "new-but-unmarked", Lifecycle: "durable", EventID: "z-abrupt-child",
		}); err != nil {
			os.Exit(91)
		}
		os.Exit(92)
	}
	t.Setenv("GO_MEMORY_STORE_BACKGROUND_HYDRATION_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_BACKGROUND_ENABLED", "false")
	root := filepath.Join(t.TempDir(), "memory-bank")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	store, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("create abrupt-death fixture: %v", err)
	}
	if err := store.persistAndRecordEntry(memoryStoreEntry{
		Project: "alpha", FileName: "notes/abrupt.md", TopicPath: "runbooks/abrupt",
		Summary: "old", Lifecycle: "durable", EventID: "abrupt-old",
	}); err != nil {
		t.Fatalf("seed abrupt-death fixture: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestMemoryCurrentStateTransactionAbruptDeathQuarantinesMarkerlessStages", "-test.count=1")
	cmd.Env = append(os.Environ(), "CONTEXTLATTICE_CURRENT_STATE_ABORT_CHILD=1", "CONTEXTLATTICE_CURRENT_STATE_ABORT_ROOT="+root)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("abrupt child unexpectedly succeeded: output=%s", output)
	}
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 97 {
		t.Fatalf("abrupt child exit=%v output=%s", err, output)
	}
	restarted, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("restart after markerless abrupt death: %v", err)
	}
	state, ok := restarted.currentStateFor("alpha", "notes/abrupt.md")
	if !ok || state.Entry.Summary != "old" {
		t.Fatalf("markerless stages changed durable state: ok=%v state=%#v", ok, state)
	}
	quarantine := filepath.Join(restarted.currentStateRootPath(), memoryCurrentStateTransactionQuarantineDir)
	entries, err := os.ReadDir(quarantine)
	if err != nil {
		t.Fatalf("read orphan quarantine: %v", err)
	}
	if len(entries) < 3 {
		t.Fatalf("abrupt death did not quarantine all durable stages: entries=%d", len(entries))
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".txn-") {
			t.Fatalf("quarantine entry retained executable transaction name: %q", entry.Name())
		}
	}
	for _, dir := range []string{restarted.currentStateRootPath(), restarted.currentStateGenerationCardsPath()} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read reconciled transaction directory %q: %v", dir, err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".txn-") {
				t.Fatalf("markerless transaction stage remained in %q: %s", dir, entry.Name())
			}
		}
	}
	shardRaw, err := json.Marshal(memoryCurrentStateShard{SchemaID: memoryCurrentStateSchemaID, Version: 1, Shard: 0, Entries: []memoryCurrentState{}})
	if err != nil {
		t.Fatal(err)
	}
	repeatedStage := filepath.Join(restarted.currentStateRootPath(), ".txn-aaaaaaaaaaaaaaaaaaaaaaaa-00.json")
	if err := os.WriteFile(repeatedStage, shardRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	beforeRepeat, err := os.ReadDir(quarantine)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newMemoryStoreFromEnv(); err != nil {
		t.Fatalf("first repeated orphan cycle failed: %v", err)
	}
	afterRepeat, err := os.ReadDir(quarantine)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterRepeat) != len(beforeRepeat)+1 {
		t.Fatalf("first repeated orphan cycle quarantine count=%d before=%d", len(afterRepeat), len(beforeRepeat))
	}
	if err := os.WriteFile(repeatedStage, shardRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newMemoryStoreFromEnv(); err != nil {
		t.Fatalf("duplicate repeated orphan cycle failed: %v", err)
	}
	finalRepeat, err := os.ReadDir(quarantine)
	if err != nil {
		t.Fatal(err)
	}
	if len(finalRepeat) != len(afterRepeat) {
		t.Fatalf("duplicate repeated orphan grew quarantine: after=%d final=%d", len(afterRepeat), len(finalRepeat))
	}
	if _, err := os.Lstat(repeatedStage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("duplicate repeated orphan stage remained live: %v", err)
	}
	oversized := filepath.Join(restarted.currentStateRootPath(), ".txn-0123456789abcdef01234567-generations.json")
	if err := os.WriteFile(oversized, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(oversized, memoryEdgeLogMaxRecoveryBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := newMemoryStoreFromEnv(); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized markerless stage was accepted: %v", err)
	}
	if _, err := os.Lstat(oversized); err != nil {
		t.Fatalf("oversized markerless stage was silently removed: %v", err)
	}
	if err := os.Remove(oversized); err != nil {
		t.Fatal(err)
	}
	for index := 0; index <= memoryCurrentStateTransactionMaxOrphans; index++ {
		transactionID := fmt.Sprintf("%024x", index+1)
		path := filepath.Join(restarted.currentStateRootPath(), ".txn-"+transactionID+"-00.json")
		if err := os.WriteFile(path, shardRaw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := newMemoryStoreFromEnv(); err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("orphan count cap was not enforced: %v", err)
	}
	quarantineEntries, err := os.ReadDir(quarantine)
	if err != nil {
		t.Fatal(err)
	}
	if len(quarantineEntries) > memoryCurrentStateTransactionMaxOrphans {
		t.Fatalf("orphan quarantine exceeded bounded retention: %d", len(quarantineEntries))
	}
}

func TestExactRegistrationCurrentRowUsesAtomicCurrentStateTransaction(t *testing.T) {
	t.Setenv("GO_MEMORY_STORE_BACKGROUND_HYDRATION_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_BACKGROUND_ENABLED", "false")
	for _, event := range []string{"before_marker", "after_marker", "after_card_rename", "after_exact_state_rename", "after_manifest_rename", "before_marker_removal"} {
		t.Run(event, func(t *testing.T) {
			store, _ := newExactStateBoundaryTestStore(t)
			if _, _, err := store.put(normalizedWrite{
				project: "alpha", fileName: "notes/exact-existing.md", topicPath: "runbooks/exact", content: "existing", lifecycle: "durable",
			}); err != nil {
				t.Fatalf("seed existing current row: %v", err)
			}
			edge := memoryEdgeEntry{
				EdgeID: "exact-registration-rollback-edge", SourceID: "alpha::notes/exact-existing.md", TargetID: "alpha::notes/other.md",
				Relation: "supports", Project: "alpha", Confidence: 0.9, CreatedAt: nowUTCISO(), Source: memoryEdgeSource,
			}
			storedEdge, err := store.upsertMemoryEdge(context.Background(), edge)
			if err != nil {
				t.Fatalf("seed exact-registration edge: %v", err)
			}
			beforeKey, beforeTopic, beforeDigest, err := store.durableCurrentStateGeneration("alpha")
			if err != nil {
				t.Fatalf("read pre-registration generation: %v", err)
			}
			store.memoryCurrentStateTransactionFault = func(candidate string) error {
				if candidate == event {
					return errors.New("injected " + event)
				}
				return nil
			}
			err = store.registerExactStatePath("alpha", "notes/exact-existing.md")
			if err == nil || !strings.Contains(err.Error(), event) {
				t.Fatalf("exact registration fault %s was not surfaced: %v", event, err)
			}
			if event == "before_marker" {
				if store.isExactStatePath("alpha", "notes/exact-existing.md") {
					t.Fatal("pre-marker exact registration leaked the in-memory registry")
				}
			} else if _, statErr := os.Stat(store.currentStateTransactionPath()); statErr != nil {
				t.Fatalf("post-marker exact registration lost recovery marker: %v", statErr)
			}
			reloaded, err := newMemoryStoreFromEnv()
			if event == "before_marker" {
				if err != nil {
					t.Fatalf("restart after pre-marker rollback: %v", err)
				}
				if err := reloaded.loadEdges(); err != nil {
					t.Fatalf("reload edge projection after pre-marker rollback: %v", err)
				}
				if reloaded.isExactStatePath("alpha", "notes/exact-existing.md") {
					t.Fatal("pre-marker registration became durable")
				}
				reloaded.mu.RLock()
				_, edgePresent := reloaded.edges[storedEdge.EdgeID]
				reloaded.mu.RUnlock()
				if !edgePresent {
					t.Fatal("pre-marker exact registration removed an edge from the recovered projection")
				}
				reloaded.mu.RLock()
				_, latest := reloaded.latestTopic[memoryStoreKey("alpha", "notes/exact-existing.md")]
				reloaded.mu.RUnlock()
				if !latest {
					t.Fatal("pre-marker rollback lost existing semantic current row")
				}
				afterKey, afterTopic, afterDigest, generationErr := reloaded.durableCurrentStateGeneration("alpha")
				if generationErr != nil || afterKey != beforeKey || afterTopic != beforeTopic || afterDigest != beforeDigest {
					t.Fatalf("pre-marker rollback changed generation: before=%d/%d/%s after=%d/%d/%s err=%v", beforeKey, beforeTopic, beforeDigest, afterKey, afterTopic, afterDigest, generationErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("restart after committed exact registration fault: %v", err)
			}
			if err := reloaded.loadEdges(); err != nil {
				t.Fatalf("reload edge projection after committed exact registration: %v", err)
			}
			if !reloaded.isExactStatePath("alpha", "notes/exact-existing.md") {
				t.Fatal("committed exact registration was not recovered")
			}
			reloaded.mu.RLock()
			_, latest := reloaded.latestTopic[memoryStoreKey("alpha", "notes/exact-existing.md")]
			reloaded.mu.RUnlock()
			if latest {
				t.Fatal("exact registration resurrected semantic latest state after restart")
			}
			reloaded.mu.RLock()
			_, edgePresent := reloaded.edges[storedEdge.EdgeID]
			reloaded.mu.RUnlock()
			if edgePresent {
				t.Fatal("committed exact registration left an exact-state edge in the graph projection")
			}
			afterKey, afterTopic, afterDigest, generationErr := reloaded.durableCurrentStateGeneration("alpha")
			if generationErr != nil || afterKey <= beforeKey || afterTopic != afterKey || afterDigest == beforeDigest {
				t.Fatalf("committed exact registration did not advance one coherent generation: before=%d/%d/%s after=%d/%d/%s err=%v", beforeKey, beforeTopic, beforeDigest, afterKey, afterTopic, afterDigest, generationErr)
			}
		})
	}
}

func TestMemoryCurrentStateGenerationManifestV1MigratesToIndexedRoots(t *testing.T) {
	t.Setenv("GO_MEMORY_STORE_BACKGROUND_HYDRATION_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_BACKGROUND_ENABLED", "false")
	store, gateway := newMemoryGraphTestServer(t, true)
	seedMemoryGraphRepairDoc(t, store.memoryStore, "alpha", "notes/migrate.md", "runbooks/migrate", "migration", "", "")
	store.memoryStore.mu.RLock()
	legacyProjects := map[string]memoryCurrentStateGenerationRecord{}
	for project, record := range store.memoryStore.currentStateGenerationRecords {
		legacyProjects[project] = memoryCurrentStateGenerationRecord{KeyGeneration: record.KeyGeneration, TopicGeneration: record.TopicGeneration, StateDigest: memoryCurrentStateDigest(store.memoryStore.currentState, project)}
	}
	legacy := memoryCurrentStateGenerationManifest{SchemaID: memoryCurrentStateGenerationSchemaID, Version: 1, StateDigest: memoryCurrentStateDigest(store.memoryStore.currentState, ""), Projects: legacyProjects}
	store.memoryStore.mu.RUnlock()
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeOwnerOnlyDurableAtomicFile(store.memoryStore.currentStateGenerationPath(), append(raw, '\n'), true); err != nil {
		t.Fatal(err)
	}
	gateway.Close()
	reloaded, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("v1 generation manifest migration: %v", err)
	}
	migrated, err := readOwnerOnlyBoundedFile(reloaded.currentStateGenerationPath(), memoryEdgeLogMaxRecoveryBytes)
	if err != nil {
		t.Fatal(err)
	}
	var manifest memoryCurrentStateGenerationManifest
	if err := json.Unmarshal(migrated, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version != memoryCurrentStateGenerationVersion || !strings.HasPrefix(manifest.StateDigest, "sha256:") {
		t.Fatalf("v1 manifest was not migrated to indexed roots: %#v", manifest)
	}
}

func TestMemoryCurrentStateDigestRootHasFixedLeafWork(t *testing.T) {
	store := &memoryStore{
		currentState:                    map[string]memoryCurrentState{},
		currentStateByShard:             map[int]map[string]memoryCurrentState{},
		currentStateShardPayloads:       map[int][]byte{},
		currentStateShardDigests:        map[int]string{},
		currentStateProjectShardDigests: map[string]map[int]string{},
		currentKeyIndexGeneration:       map[string]uint64{"alpha": 7},
		currentTopicIndexGeneration:     map[string]uint64{"alpha": 7},
	}
	for index := 0; index < 10000; index++ {
		key := fmt.Sprintf("alpha::notes/%05d.md", index)
		state := memoryCurrentStateFromEntry(memoryStoreEntry{Project: "alpha", FileName: fmt.Sprintf("notes/%05d.md", index), TopicPath: "runbooks/digest", Summary: "digest"})
		store.currentState[key] = state
	}
	store.mu.Lock()
	store.ensureCurrentStateDigestIndexesLocked()
	before := store.currentStateDigestLocked("alpha")
	store.mu.Unlock()
	if !memoryCurrentStateGenerationDigestValid(before) {
		t.Fatalf("indexed project root is not a digest: %q", before)
	}
	if got := len(store.currentStateProjectShardDigests["alpha"]); got > memoryCurrentStateShardCount {
		t.Fatalf("project digest leaves exceeded shard count: %d", got)
	}
	store.mu.Lock()
	after := store.currentStateDigestLocked("alpha")
	store.mu.Unlock()
	if before != after {
		t.Fatalf("indexed root was not restart-stable in memory: before=%s after=%s", before, after)
	}
}

func TestMemoryCurrentStateWriteRefreshesOnlyAffectedShard(t *testing.T) {
	store := &memoryStore{
		currentState:                    map[string]memoryCurrentState{},
		currentStateByShard:             map[int]map[string]memoryCurrentState{},
		currentStateShardPayloads:       map[int][]byte{},
		currentStateShardDigests:        map[int]string{},
		currentStateProjectShardDigests: map[string]map[int]string{},
		currentKeyIndexGeneration:       map[string]uint64{},
		currentTopicIndexGeneration:     map[string]uint64{},
	}
	for projectIndex := 0; projectIndex < 100; projectIndex++ {
		project := fmt.Sprintf("project-%03d", projectIndex)
		store.currentKeyIndexGeneration[project] = 1
		store.currentTopicIndexGeneration[project] = 1
		for row := 0; row < 50; row++ {
			fileName := fmt.Sprintf("notes/%03d.md", row)
			key := memoryStoreKey(project, fileName)
			store.currentState[key] = memoryCurrentStateFromEntry(memoryStoreEntry{Project: project, FileName: fileName, TopicPath: "runbooks/scaling", Summary: "scaling"})
		}
	}
	store.mu.Lock()
	store.ensureCurrentStateDigestIndexesLocked()
	store.mu.Unlock()
	totalRows := len(store.currentState)
	var refreshes, refreshedRows int
	store.memoryCurrentStateDigestObserve = func(_ int, rows int) {
		refreshes++
		refreshedRows += rows
	}
	store.mu.Lock()
	if !store.applyCurrentStateEntryLocked(memoryStoreEntry{Project: "project-050", FileName: "notes/new.md", TopicPath: "runbooks/scaling", Summary: "new"}) {
		store.mu.Unlock()
		t.Fatal("new current-state key was rejected")
	}
	store.mu.Unlock()
	if refreshes != 1 {
		t.Fatalf("current-state write refreshed %d shards, want 1", refreshes)
	}
	if refreshedRows <= 0 || refreshedRows >= totalRows {
		t.Fatalf("current-state write refreshed %d rows out of %d total; expected affected-shard scaling", refreshedRows, totalRows)
	}
}

func TestMemoryCurrentStateStartupRefreshesEachDirtyShardOnce(t *testing.T) {
	store := &memoryStore{
		currentState:                    map[string]memoryCurrentState{},
		currentStateByShard:             map[int]map[string]memoryCurrentState{},
		currentStateShardPayloads:       map[int][]byte{},
		currentStateShardDigests:        map[int]string{},
		currentStateProjectShardDigests: map[string]map[int]string{},
		currentKeyIndexGeneration:       map[string]uint64{},
		currentTopicIndexGeneration:     map[string]uint64{},
	}
	store.mu.Lock()
	store.ensureCurrentStateDigestIndexesLocked()
	store.currentStateDigestIndexDeferred = true
	store.mu.Unlock()
	dirty := map[int]struct{}{}
	for index := 0; len(dirty) < memoryCurrentStateShardCount; index++ {
		fileName := fmt.Sprintf("notes/startup-%06d.md", index)
		entry := memoryStoreEntry{Project: "startup", FileName: fileName, TopicPath: "runbooks/startup", Summary: "startup"}
		key := memoryStoreKey(entry.Project, entry.FileName)
		shard := memoryCurrentStateShardForKey(key)
		if _, exists := dirty[shard]; exists {
			continue
		}
		store.mu.Lock()
		if !store.applyCurrentStateEntryLocked(entry) {
			store.mu.Unlock()
			t.Fatalf("startup fixture rejected shard %d", shard)
		}
		store.mu.Unlock()
		dirty[shard] = struct{}{}
	}
	var refreshes, refreshedRows int
	store.memoryCurrentStateDigestObserve = func(_ int, rows int) {
		refreshes++
		refreshedRows += rows
	}
	store.mu.Lock()
	store.currentStateDigestIndexDeferred = false
	for shard := range dirty {
		if err := store.refreshCurrentStateShardDigestIndexesLocked(shard); err != nil {
			store.mu.Unlock()
			t.Fatal(err)
		}
	}
	store.mu.Unlock()
	if refreshes != memoryCurrentStateShardCount || refreshedRows != memoryCurrentStateShardCount {
		t.Fatalf("startup dirty refreshes=%d rows=%d want=%d each", refreshes, refreshedRows, memoryCurrentStateShardCount)
	}
}

func TestMemoryCurrentStateStartupDigestWorkScalesWithDirtyShardRows(t *testing.T) {
	store := &memoryStore{
		currentState:                    map[string]memoryCurrentState{},
		currentStateByShard:             map[int]map[string]memoryCurrentState{},
		currentStateShardPayloads:       map[int][]byte{},
		currentStateShardDigests:        map[int]string{},
		currentStateProjectShardDigests: map[string]map[int]string{},
		currentKeyIndexGeneration:       map[string]uint64{},
		currentTopicIndexGeneration:     map[string]uint64{},
	}
	store.mu.Lock()
	store.ensureCurrentStateDigestIndexesLocked()
	store.currentStateDigestIndexDeferred = true
	store.mu.Unlock()
	for projectIndex := 0; projectIndex < 100; projectIndex++ {
		project := fmt.Sprintf("startup-project-%03d", projectIndex)
		for rowIndex := 0; rowIndex < 128; rowIndex++ {
			entry := memoryStoreEntry{
				Project: project, FileName: fmt.Sprintf("notes/row-%04d.md", rowIndex),
				TopicPath: "runbooks/startup-scaling", Summary: "startup-scaling",
			}
			store.mu.Lock()
			if !store.applyCurrentStateEntryLocked(entry) {
				store.mu.Unlock()
				t.Fatalf("startup fixture rejected %s/%d", project, rowIndex)
			}
			store.mu.Unlock()
		}
	}
	totalRows := len(store.currentState)
	dirty := map[int]struct{}{}
	for key := range store.currentState {
		dirty[memoryCurrentStateShardForKey(key)] = struct{}{}
	}
	if len(dirty) != memoryCurrentStateShardCount {
		t.Fatalf("fixture did not cover all dirty shards: %d", len(dirty))
	}
	var refreshes, refreshedRows int
	store.memoryCurrentStateDigestObserve = func(_ int, rows int) {
		refreshes++
		refreshedRows += rows
	}
	store.mu.Lock()
	store.currentStateDigestIndexDeferred = false
	for shard := range dirty {
		if err := store.refreshCurrentStateShardDigestIndexesLocked(shard); err != nil {
			store.mu.Unlock()
			t.Fatal(err)
		}
	}
	store.mu.Unlock()
	if refreshes != memoryCurrentStateShardCount || refreshedRows != totalRows {
		t.Fatalf("startup digest work scanned outside dirty shard rows: refreshes=%d rows=%d total=%d", refreshes, refreshedRows, totalRows)
	}
}

func TestMemoryCurrentStateGenerationManifestPayloadIsFixedRoot(t *testing.T) {
	store := &memoryStore{
		currentState:                    map[string]memoryCurrentState{},
		currentStateByShard:             map[int]map[string]memoryCurrentState{},
		currentStateShardPayloads:       map[int][]byte{},
		currentStateShardDigests:        map[int]string{},
		currentStateProjectShardDigests: map[string]map[int]string{},
		currentKeyIndexGeneration:       map[string]uint64{},
		currentTopicIndexGeneration:     map[string]uint64{},
		currentStateGenerationRecords:   map[string]memoryCurrentStateGenerationRecord{},
	}
	for projectIndex := 0; projectIndex < 100000; projectIndex++ {
		project := fmt.Sprintf("high-cardinality-project-%06d", projectIndex)
		store.currentStateGenerationRecords[project] = memoryCurrentStateGenerationRecord{
			KeyGeneration: 1, TopicGeneration: 1, StateDigest: memoryCurrentStateRootDigest(project, 1, nil),
		}
	}
	store.mu.Lock()
	payload, err := store.currentStateGenerationManifestPayloadLocked()
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 1024 || strings.Contains(string(raw), `"projects"`) {
		t.Fatalf("v3 generation manifest grew with project cardinality: bytes=%d payload=%s", len(raw), raw)
	}
}

func TestMemoryCurrentStateGenerationCardsRequireExactDurableSet(t *testing.T) {
	store, _ := newExactStateBoundaryTestStore(t)
	if _, _, err := store.put(normalizedWrite{project: "alpha", fileName: "notes/card-set.md", topicPath: "runbooks/card-set", content: "card-set", lifecycle: "durable"}); err != nil {
		t.Fatalf("seed card-set fixture: %v", err)
	}
	alphaCard := store.currentStateGenerationCardPath("alpha")
	alphaRaw, err := os.ReadFile(alphaCard)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := store.currentStateGenerationPath()
	manifestRaw, err := readOwnerOnlyBoundedFile(manifestPath, memoryEdgeLogMaxRecoveryBytes)
	if err != nil {
		t.Fatal(err)
	}
	var manifest memoryCurrentStateGenerationManifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alphaCard); err != nil {
		t.Fatal(err)
	}
	// A tampered root count/digest must not turn a missing durable project
	// card into a valid subset. The loader checks exact correspondence before
	// accepting the root accumulator.
	emptyCount, emptyDigest := memoryCurrentStateGenerationCardsDigest(map[string]memoryCurrentStateGenerationRecord{})
	manifest.ProjectCardsCount = emptyCount
	manifest.ProjectCardsDigest = emptyDigest
	tamperedManifest, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeOwnerOnlyDurableAtomicFile(manifestPath, append(tamperedManifest, '\n'), true); err != nil {
		t.Fatal(err)
	}
	if _, err := newMemoryStoreFromEnv(); err == nil || !strings.Contains(err.Error(), "missing durable project") {
		t.Fatalf("missing project card was accepted: %v", err)
	}
	if err := writeOwnerOnlyDurableAtomicFile(alphaCard, alphaRaw, true); err != nil {
		t.Fatal(err)
	}
	if err := writeOwnerOnlyDurableAtomicFile(manifestPath, manifestRaw, true); err != nil {
		t.Fatal(err)
	}
	betaPayload, err := memoryCurrentStateGenerationCardPayload("beta", memoryCurrentStateGenerationRecord{
		KeyGeneration: 1, TopicGeneration: 1, StateDigest: memoryCurrentStateRootDigest("beta", 1, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeOwnerOnlyDurableAtomicFile(store.currentStateGenerationCardPath("beta"), betaPayload, true); err != nil {
		t.Fatal(err)
	}
	if _, err := newMemoryStoreFromEnv(); err == nil || !strings.Contains(err.Error(), "stale project") {
		t.Fatalf("foreign project card was accepted: %v", err)
	}
}

func TestMemoryCurrentStateGenerationCardCommitmentBindsRecordSet(t *testing.T) {
	oldRecord := memoryCurrentStateGenerationRecord{
		KeyGeneration: 1, TopicGeneration: 1,
		StateDigest: memoryCurrentStateRootDigest("alpha", 1, nil),
	}
	newRecord := oldRecord
	newRecord.KeyGeneration = 2
	newRecord.TopicGeneration = 2
	newRecord.StateDigest = memoryCurrentStateRootDigest("alpha", 2, nil)
	oldCount, oldDigest := memoryCurrentStateGenerationCardsDigest(map[string]memoryCurrentStateGenerationRecord{"alpha": oldRecord})
	newCount, newDigest := memoryCurrentStateGenerationCardsDigest(map[string]memoryCurrentStateGenerationRecord{"alpha": newRecord})
	if oldCount != 1 || newCount != 1 || oldDigest == newDigest || !memoryCurrentStateGenerationDigestValid(oldDigest) || !memoryCurrentStateGenerationDigestValid(newDigest) {
		t.Fatalf("card commitment did not bind a substituted record: old=(%d,%s) new=(%d,%s)", oldCount, oldDigest, newCount, newDigest)
	}
	store := &memoryStore{}
	store.setCurrentStateGenerationCardsAccumulatorLocked(map[string]memoryCurrentStateGenerationRecord{"alpha": oldRecord})
	store.updateCurrentStateGenerationCardAccumulatorLocked("alpha", oldRecord, true, newRecord, true)
	if store.currentStateGenerationCardCount != newCount || store.currentStateGenerationCardsDigest != newDigest {
		t.Fatalf("incremental card commitment diverged from canonical root: got=(%d,%s) want=(%d,%s)", store.currentStateGenerationCardCount, store.currentStateGenerationCardsDigest, newCount, newDigest)
	}
}

func TestMemoryCurrentStateGenerationCardPatriciaExistingUpdatesStayBounded(t *testing.T) {
	makeRecord := func(project string, generation uint64) memoryCurrentStateGenerationRecord {
		return memoryCurrentStateGenerationRecord{
			KeyGeneration: generation, TopicGeneration: generation,
			StateDigest: memoryCurrentStateRootDigest(project, generation, nil),
		}
	}

	twoLeafRecords := map[string]memoryCurrentStateGenerationRecord{
		"patricia-alpha": makeRecord("patricia-alpha", 1),
		"patricia-beta":  makeRecord("patricia-beta", 1),
	}
	twoLeafStore := &memoryStore{}
	if err := twoLeafStore.setCurrentStateGenerationCardsAccumulatorLocked(twoLeafRecords); err != nil {
		t.Fatal(err)
	}
	var twoLeafHashes int
	twoLeafStore.memoryCurrentStateGenerationCardActualObserve = func(actual int) { twoLeafHashes = actual }
	updated := makeRecord("patricia-alpha", 2)
	if err := twoLeafStore.updateCurrentStateGenerationCardAccumulatorLocked("patricia-alpha", twoLeafRecords["patricia-alpha"], true, updated, true); err != nil {
		t.Fatal(err)
	}
	if twoLeafHashes <= 0 || twoLeafHashes > memoryCurrentStateGenerationCardPatriciaMaxMutationHashes {
		t.Fatalf("two-leaf Patricia update hashed %d authenticated branch/skip nodes, want 1..%d", twoLeafHashes, memoryCurrentStateGenerationCardPatriciaMaxMutationHashes)
	}
	twoLeafRecords["patricia-alpha"] = updated
	wantCount, wantDigest := memoryCurrentStateGenerationCardsDigest(twoLeafRecords)
	if twoLeafStore.currentStateGenerationCardCount != wantCount || twoLeafStore.currentStateGenerationCardsDigest != wantDigest {
		t.Fatalf("two-leaf Patricia update diverged from canonical root: got=(%d,%s) want=(%d,%s)", twoLeafStore.currentStateGenerationCardCount, twoLeafStore.currentStateGenerationCardsDigest, wantCount, wantDigest)
	}

	// Exercise a tree with many branches at different compressed depths. Every
	// existing-record update remains within the declared fixed mutation bound,
	// regardless of where its siblings happen to diverge.
	const projectCount = 512
	manyRecords := make(map[string]memoryCurrentStateGenerationRecord, projectCount)
	for index := 0; index < projectCount; index++ {
		project := fmt.Sprintf("patricia-adversarial-%03d", index)
		manyRecords[project] = makeRecord(project, 1)
	}
	manyStore := &memoryStore{}
	if err := manyStore.setCurrentStateGenerationCardsAccumulatorLocked(manyRecords); err != nil {
		t.Fatal(err)
	}
	var updates int
	manyStore.memoryCurrentStateGenerationCardActualObserve = func(actual int) {
		updates++
		if actual <= 0 || actual > memoryCurrentStateGenerationCardPatriciaMaxMutationHashes {
			t.Errorf("adversarial Patricia update %d hashed %d authenticated branch/skip nodes, want 1..%d", updates, actual, memoryCurrentStateGenerationCardPatriciaMaxMutationHashes)
		}
	}
	for index := 0; index < projectCount; index += 37 {
		project := fmt.Sprintf("patricia-adversarial-%03d", index)
		oldRecord := manyRecords[project]
		newRecord := makeRecord(project, oldRecord.KeyGeneration+1)
		if err := manyStore.updateCurrentStateGenerationCardAccumulatorLocked(project, oldRecord, true, newRecord, true); err != nil {
			t.Fatal(err)
		}
		manyRecords[project] = newRecord
	}
	if updates != (projectCount+36)/37 {
		t.Fatalf("adversarial Patricia update observer calls=%d want=%d", updates, (projectCount+36)/37)
	}
	wantCount, wantDigest = memoryCurrentStateGenerationCardsDigest(manyRecords)
	if manyStore.currentStateGenerationCardCount != wantCount || manyStore.currentStateGenerationCardsDigest != wantDigest {
		t.Fatalf("adversarial Patricia updates diverged from canonical root: got=(%d,%s) want=(%d,%s)", manyStore.currentStateGenerationCardCount, manyStore.currentStateGenerationCardsDigest, wantCount, wantDigest)
	}
}

// The reference commitment below intentionally does not use the production
// tree builder. It constructs the canonical Patricia partition recursively and
// duplicates only the wire encodings, so incremental roots are checked against
// an independently ordered result rather than against another invocation of
// the same mutation code.
type memoryCurrentStateGenerationCardReferenceNode struct {
	keyHash [32]byte
	project string
	record  memoryCurrentStateGenerationRecord
	depth   int
	leaf    bool
	left    *memoryCurrentStateGenerationCardReferenceNode
	right   *memoryCurrentStateGenerationCardReferenceNode
	digest  [32]byte
}

func memoryCurrentStateGenerationCardReferenceKey(project string) [32]byte {
	return sha256.Sum256([]byte("contextlattice-generation-card-key:v5:" + normalizeCurrentKeyIndexProject(project)))
}

func memoryCurrentStateGenerationCardReferenceEmpty() [32]byte {
	return sha256.Sum256([]byte("contextlattice-generation-card-patricia-empty:v1"))
}

func memoryCurrentStateGenerationCardReferenceLeaf(project string, record memoryCurrentStateGenerationRecord) [32]byte {
	project = normalizeCurrentKeyIndexProject(project)
	card := make([]byte, 0, 96+len(project)+len(record.StateDigest))
	card = append(card, "contextlattice-generation-card-leaf:v2"...)
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(project)))
	card = append(card, length[:]...)
	card = append(card, project...)
	binary.BigEndian.PutUint64(length[:], record.KeyGeneration)
	card = append(card, length[:]...)
	binary.BigEndian.PutUint64(length[:], record.TopicGeneration)
	card = append(card, length[:]...)
	binary.BigEndian.PutUint64(length[:], uint64(len(record.StateDigest)))
	card = append(card, length[:]...)
	card = append(card, record.StateDigest...)
	cardLeaf := sha256.Sum256(card)
	payload := make([]byte, 0, 128)
	payload = append(payload, "contextlattice-generation-card-patricia-leaf:v1"...)
	keyHash := memoryCurrentStateGenerationCardReferenceKey(project)
	payload = append(payload, keyHash[:]...)
	payload = append(payload, cardLeaf[:]...)
	return sha256.Sum256(payload)
}

func memoryCurrentStateGenerationCardReferenceSkip(startDepth, endDepth int, keyHash, child [32]byte) [32]byte {
	payload := make([]byte, 0, 128)
	payload = append(payload, "contextlattice-generation-card-patricia-skip:v1"...)
	payload = append(payload, byte(startDepth>>8), byte(startDepth), byte(endDepth>>8), byte(endDepth))
	payload = append(payload, keyHash[:]...)
	payload = append(payload, child[:]...)
	return sha256.Sum256(payload)
}

func memoryCurrentStateGenerationCardReferenceBranch(depth int, left, right [32]byte) [32]byte {
	payload := make([]byte, 0, 128)
	payload = append(payload, "contextlattice-generation-card-patricia-branch:v1"...)
	payload = append(payload, byte(depth>>8), byte(depth))
	payload = append(payload, left[:]...)
	payload = append(payload, right[:]...)
	return sha256.Sum256(payload)
}

func memoryCurrentStateGenerationCardReferenceBit(hash [32]byte, depth int) byte {
	return (hash[depth/8] >> uint(7-(depth%8))) & 1
}

func memoryCurrentStateGenerationCardReferenceBuild(projects []string, records map[string]memoryCurrentStateGenerationRecord, start, end, minimumDepth int) *memoryCurrentStateGenerationCardReferenceNode {
	if end-start == 1 {
		project := projects[start]
		return &memoryCurrentStateGenerationCardReferenceNode{
			keyHash: memoryCurrentStateGenerationCardReferenceKey(project), project: project,
			record: records[project], depth: memoryCurrentStateGenerationCardPatriciaDepth,
			leaf: true, digest: memoryCurrentStateGenerationCardReferenceLeaf(project, records[project]),
		}
	}
	leftHash := memoryCurrentStateGenerationCardReferenceKey(projects[start])
	rightHash := memoryCurrentStateGenerationCardReferenceKey(projects[end-1])
	depth := minimumDepth
	for depth < memoryCurrentStateGenerationCardPatriciaDepth && memoryCurrentStateGenerationCardReferenceBit(leftHash, depth) == memoryCurrentStateGenerationCardReferenceBit(rightHash, depth) {
		depth++
	}
	split := start
	for split < end && memoryCurrentStateGenerationCardReferenceBit(memoryCurrentStateGenerationCardReferenceKey(projects[split]), depth) == 0 {
		split++
	}
	if split == start || split == end {
		panic(fmt.Sprintf("reference Patricia partition has no split at depth %d", depth))
	}
	node := &memoryCurrentStateGenerationCardReferenceNode{
		depth: depth,
		left:  memoryCurrentStateGenerationCardReferenceBuild(projects, records, start, split, depth+1),
		right: memoryCurrentStateGenerationCardReferenceBuild(projects, records, split, end, depth+1),
	}
	memoryCurrentStateGenerationCardReferenceRefresh(node)
	return node
}

func memoryCurrentStateGenerationCardReferenceRepresentative(node *memoryCurrentStateGenerationCardReferenceNode) [32]byte {
	for node != nil && !node.leaf {
		node = node.left
	}
	if node == nil {
		return [32]byte{}
	}
	return node.keyHash
}

func memoryCurrentStateGenerationCardReferenceDigestAt(node *memoryCurrentStateGenerationCardReferenceNode, startDepth int) [32]byte {
	if node == nil {
		return memoryCurrentStateGenerationCardReferenceEmpty()
	}
	if startDepth >= node.depth {
		return node.digest
	}
	keyHash := memoryCurrentStateGenerationCardReferenceRepresentative(node)
	return memoryCurrentStateGenerationCardReferenceSkip(startDepth, node.depth, keyHash, node.digest)
}

func memoryCurrentStateGenerationCardReferenceRefresh(node *memoryCurrentStateGenerationCardReferenceNode) {
	if node == nil || node.leaf {
		return
	}
	left := memoryCurrentStateGenerationCardReferenceDigestAt(node.left, node.depth+1)
	right := memoryCurrentStateGenerationCardReferenceDigestAt(node.right, node.depth+1)
	node.digest = memoryCurrentStateGenerationCardReferenceBranch(node.depth, left, right)
}

func memoryCurrentStateGenerationCardReferenceRoot(records map[string]memoryCurrentStateGenerationRecord) string {
	projects := make([]string, 0, len(records))
	canonical := make(map[string]memoryCurrentStateGenerationRecord, len(records))
	for project, record := range records {
		project = normalizeCurrentKeyIndexProject(project)
		if project == "" {
			continue
		}
		if _, exists := canonical[project]; !exists {
			projects = append(projects, project)
		}
		canonical[project] = record
	}
	sort.Slice(projects, func(left, right int) bool {
		leftHash := memoryCurrentStateGenerationCardReferenceKey(projects[left])
		rightHash := memoryCurrentStateGenerationCardReferenceKey(projects[right])
		if order := bytes.Compare(leftHash[:], rightHash[:]); order != 0 {
			return order < 0
		}
		return projects[left] < projects[right]
	})
	if len(projects) == 0 {
		empty := memoryCurrentStateGenerationCardReferenceEmpty()
		return "sha256:" + fmt.Sprintf("%x", empty[:])
	}
	root := memoryCurrentStateGenerationCardReferenceBuild(projects, canonical, 0, len(projects), 0)
	digest := memoryCurrentStateGenerationCardReferenceDigestAt(root, 0)
	return "sha256:" + fmt.Sprintf("%x", digest[:])
}

func TestMemoryCurrentStateGenerationCardPatriciaBoundedMutationsAndCanonicalRoot(t *testing.T) {
	makeRecord := func(project string, generation uint64) memoryCurrentStateGenerationRecord {
		return memoryCurrentStateGenerationRecord{
			KeyGeneration: generation, TopicGeneration: generation,
			StateDigest: memoryCurrentStateRootDigest(project, generation, nil),
		}
	}
	assertWork := func(operation string, work int) {
		if work <= 0 || work > memoryCurrentStateGenerationCardPatriciaMaxMutationHashes {
			t.Fatalf("%s hashed %d authenticated branch/skip nodes, want 1..%d", operation, work, memoryCurrentStateGenerationCardPatriciaMaxMutationHashes)
		}
	}
	assertRoot := func(store *memoryStore, records map[string]memoryCurrentStateGenerationRecord, operation string) {
		if want := memoryCurrentStateGenerationCardReferenceRoot(records); store.currentStateGenerationCardsDigest != want {
			t.Fatalf("%s root mismatch: got=%s want=%s", operation, store.currentStateGenerationCardsDigest, want)
		}
	}
	assertMutation := func(store *memoryStore, records map[string]memoryCurrentStateGenerationRecord, operation string, work int) {
		assertWork(operation, work)
		assertRoot(store, records, operation)
	}

	records := map[string]memoryCurrentStateGenerationRecord{}
	store := &memoryStore{}
	if err := store.setCurrentStateGenerationCardsAccumulatorLocked(records); err != nil {
		t.Fatal(err)
	}
	var work int
	store.memoryCurrentStateGenerationCardActualObserve = func(actual int) { work = actual }
	alpha := makeRecord("patricia-alpha", 1)
	if err := store.updateCurrentStateGenerationCardAccumulatorLocked("patricia-alpha", memoryCurrentStateGenerationRecord{}, false, alpha, true); err != nil {
		t.Fatal(err)
	}
	records["patricia-alpha"] = alpha
	assertMutation(store, records, "empty-to-one insertion", work)

	beta := makeRecord("patricia-beta", 1)
	if err := store.updateCurrentStateGenerationCardAccumulatorLocked("patricia-beta", memoryCurrentStateGenerationRecord{}, false, beta, true); err != nil {
		t.Fatal(err)
	}
	records["patricia-beta"] = beta
	assertMutation(store, records, "one-to-two insertion", work)

	for index := 0; index < 512; index++ {
		project := fmt.Sprintf("patricia-multi-%03d", index)
		record := makeRecord(project, 1)
		if err := store.updateCurrentStateGenerationCardAccumulatorLocked(project, memoryCurrentStateGenerationRecord{}, false, record, true); err != nil {
			t.Fatal(err)
		}
		records[project] = record
		assertWork(fmt.Sprintf("multi-branch insertion %d", index), work)
	}
	assertRoot(store, records, "multi-branch insertion final root")

	deleteRecords := map[string]memoryCurrentStateGenerationRecord{"patricia-alpha": records["patricia-alpha"], "patricia-beta": records["patricia-beta"]}
	deleteStore := &memoryStore{}
	if err := deleteStore.setCurrentStateGenerationCardsAccumulatorLocked(deleteRecords); err != nil {
		t.Fatal(err)
	}
	deleteStore.memoryCurrentStateGenerationCardActualObserve = func(actual int) { work = actual }
	if err := deleteStore.updateCurrentStateGenerationCardAccumulatorLocked("patricia-alpha", deleteRecords["patricia-alpha"], true, memoryCurrentStateGenerationRecord{}, false); err != nil {
		t.Fatal(err)
	}
	delete(deleteRecords, "patricia-alpha")
	assertMutation(deleteStore, deleteRecords, "branch-collapsing delete", work)

	persisted, _ := newExactStateBoundaryTestStore(t)
	write := normalizedWrite{
		project: "patricia-reload", fileName: "notes/patricia-reload.md",
		topicPath: "runbooks/patricia-reload", content: "before reload", lifecycle: "durable",
	}
	if _, _, err := persisted.put(write); err != nil {
		t.Fatalf("seed persisted Patricia reload fixture: %v", err)
	}
	reloaded, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("reload persisted Patricia fixture: %v", err)
	}
	work = 0
	reloaded.memoryCurrentStateGenerationCardActualObserve = func(actual int) { work = actual }
	write.content = "after reload"
	if _, _, err := reloaded.put(write); err != nil {
		t.Fatalf("update persisted Patricia fixture after reload: %v", err)
	}
	assertMutation(reloaded, reloaded.currentStateGenerationRecords, "update after persisted reload", work)

	verified, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("verify persisted Patricia update on second reload: %v", err)
	}
	assertRoot(verified, verified.currentStateGenerationRecords, "second persisted reload")
}

func TestMemoryCurrentStateGenerationCardPatriciaWorstCaseInsertionUsesDeclaredBound(t *testing.T) {
	singleBitHash := func(depth int) [32]byte {
		var hash [32]byte
		hash[depth/8] = 1 << uint(7-(depth%8))
		return hash
	}
	makeRecord := func(project string) memoryCurrentStateGenerationRecord {
		return memoryCurrentStateGenerationRecord{
			KeyGeneration: 1, TopicGeneration: 1,
			StateDigest: memoryCurrentStateRootDigest(project, 1, nil),
		}
	}

	targetProject := "patricia-bound-target"
	root, err := memoryCurrentStateGenerationCardPatriciaInsert(nil, [32]byte{}, targetProject, makeRecord(targetProject))
	if err != nil {
		t.Fatal(err)
	}
	for depth := 0; depth < memoryCurrentStateGenerationCardPatriciaDepth-2; depth++ {
		project := fmt.Sprintf("patricia-bound-sibling-%03d", depth)
		var work int
		root, err = memoryCurrentStateGenerationCardPatriciaInsert(root, singleBitHash(depth), project, makeRecord(project), &work)
		if err != nil {
			t.Fatal(err)
		}
		if work <= 0 || work > memoryCurrentStateGenerationCardPatriciaMaxMutationHashes {
			t.Fatalf("synthetic insertion depth %d hashed %d authenticated branch/skip nodes, want 1..%d", depth, work, memoryCurrentStateGenerationCardPatriciaMaxMutationHashes)
		}
	}

	project := "patricia-bound-maximum"
	var work int
	if _, err := memoryCurrentStateGenerationCardPatriciaInsert(root, singleBitHash(memoryCurrentStateGenerationCardPatriciaDepth-2), project, makeRecord(project), &work); err != nil {
		t.Fatal(err)
	}
	if work != memoryCurrentStateGenerationCardPatriciaMaxMutationHashes {
		t.Fatalf("worst-case insertion hashed %d authenticated branch/skip nodes, want exactly %d", work, memoryCurrentStateGenerationCardPatriciaMaxMutationHashes)
	}
}

func TestMemoryCurrentStateGenerationCardCommitmentSamePrefixHasBoundedPath(t *testing.T) {
	records := make(map[string]memoryCurrentStateGenerationRecord)
	firstProject := "same-prefix-project-000000"
	targetByte := memoryCurrentStateGenerationCardPatriciaKey(firstProject)[0]
	for index := 0; len(records) < 512 && index < 500000; index++ {
		project := fmt.Sprintf("same-prefix-project-%06d", index)
		if memoryCurrentStateGenerationCardPatriciaKey(project)[0] != targetByte {
			continue
		}
		records[project] = memoryCurrentStateGenerationRecord{KeyGeneration: 1, TopicGeneration: 1, StateDigest: memoryCurrentStateRootDigest(project, 1, nil)}
	}
	if len(records) < 512 {
		t.Fatalf("could not construct same-full-hash-prefix fixture: %d", len(records))
	}
	store := &memoryStore{}
	work := 0
	store.memoryCurrentStateGenerationCardActualObserve = func(actual int) { work = actual }
	if err := store.setCurrentStateGenerationCardsAccumulatorLocked(records); err != nil {
		t.Fatal(err)
	}
	target := firstProject
	if _, ok := records[target]; !ok {
		for project := range records {
			target = project
			break
		}
	}
	updated := records[target]
	updated.KeyGeneration++
	updated.TopicGeneration++
	updated.StateDigest = memoryCurrentStateRootDigest(target, updated.KeyGeneration, nil)
	if err := store.updateCurrentStateGenerationCardAccumulatorLocked(target, records[target], true, updated, true); err != nil {
		t.Fatal(err)
	}
	canonical := make(map[string]memoryCurrentStateGenerationRecord, len(records))
	for project, record := range records {
		canonical[project] = record
	}
	canonical[target] = updated
	count, digest := memoryCurrentStateGenerationCardsDigest(canonical)
	if store.currentStateGenerationCardCount != count || store.currentStateGenerationCardsDigest != digest {
		t.Fatalf("same-prefix incremental commitment diverged: got=(%d,%s) want=(%d,%s)", store.currentStateGenerationCardCount, store.currentStateGenerationCardsDigest, count, digest)
	}
	if work <= 0 || work > memoryCurrentStateGenerationCardPatriciaMaxMutationHashes {
		t.Fatalf("Patricia update exceeded its fixed mutation bound: work=%d cards=%d bound=%d", work, count, memoryCurrentStateGenerationCardPatriciaMaxMutationHashes)
	}
	var height func(*memoryCurrentStateGenerationCardPatriciaNode) int
	height = func(node *memoryCurrentStateGenerationCardPatriciaNode) int {
		if node == nil || node.leaf {
			return 0
		}
		left, right := height(node.left), height(node.right)
		if left > right {
			return left + 1
		}
		return right + 1
	}
	if depth := height(store.currentStateGenerationCardTree); depth > memoryCurrentStateGenerationCardPatriciaDepth {
		t.Fatalf("Patricia commitment height exceeded fixed key depth: depth=%d cards=%d", depth, count)
	}
	projects := make([]string, 0, len(records))
	for project := range records {
		projects = append(projects, project)
	}
	forward, reverse := (*memoryCurrentStateGenerationCardPatriciaNode)(nil), (*memoryCurrentStateGenerationCardPatriciaNode)(nil)
	for _, project := range projects {
		var err error
		forward, err = memoryCurrentStateGenerationCardPatriciaInsert(forward, memoryCurrentStateGenerationCardPatriciaKey(project), project, records[project])
		if err != nil {
			t.Fatal(err)
		}
	}
	for index := len(projects) - 1; index >= 0; index-- {
		project := projects[index]
		var err error
		reverse, err = memoryCurrentStateGenerationCardPatriciaInsert(reverse, memoryCurrentStateGenerationCardPatriciaKey(project), project, records[project])
		if err != nil {
			t.Fatal(err)
		}
	}
	if memoryCurrentStateGenerationCardPatriciaDigest(forward) != memoryCurrentStateGenerationCardPatriciaDigest(reverse) {
		t.Fatal("Patricia commitment changed with insertion order")
	}
	collisionHash := [32]byte{1, 2, 3}
	collisionRoot, err := memoryCurrentStateGenerationCardPatriciaInsert(nil, collisionHash, "collision-one", records[target])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memoryCurrentStateGenerationCardPatriciaInsert(collisionRoot, collisionHash, "collision-two", records[target]); err == nil {
		t.Fatal("full-hash project collision was accepted")
	}

	large := make(map[string]memoryCurrentStateGenerationRecord, 100000)
	for index := 0; index < 100000; index++ {
		project := fmt.Sprintf("adversarial-same-prefix-%06d", index)
		large[project] = memoryCurrentStateGenerationRecord{KeyGeneration: 1, TopicGeneration: 1, StateDigest: memoryCurrentStateRootDigest(project, 1, nil)}
	}
	largeStore := &memoryStore{}
	if err := largeStore.setCurrentStateGenerationCardsAccumulatorLocked(large); err != nil {
		t.Fatal(err)
	}
	if depth := height(largeStore.currentStateGenerationCardTree); depth > memoryCurrentStateGenerationCardPatriciaDepth {
		t.Fatalf("100k adversarial Patricia commitment height exceeded fixed key depth: depth=%d", depth)
	}
	largeWork := 0
	largeStore.memoryCurrentStateGenerationCardActualObserve = func(actual int) { largeWork = actual }
	largeTarget := "adversarial-same-prefix-000000"
	largeUpdated := large[largeTarget]
	largeUpdated.KeyGeneration++
	largeUpdated.TopicGeneration++
	largeUpdated.StateDigest = memoryCurrentStateRootDigest(largeTarget, largeUpdated.KeyGeneration, nil)
	if err := largeStore.updateCurrentStateGenerationCardAccumulatorLocked(largeTarget, large[largeTarget], true, largeUpdated, true); err != nil {
		t.Fatal(err)
	}
	if largeWork <= 0 || largeWork > memoryCurrentStateGenerationCardPatriciaMaxMutationHashes {
		t.Fatalf("100k adversarial update exceeded fixed mutation bound: work=%d bound=%d", largeWork, memoryCurrentStateGenerationCardPatriciaMaxMutationHashes)
	}
}

func TestMemoryCurrentStateGenerationLegacyCardDigestMigrates(t *testing.T) {
	store, _ := newExactStateBoundaryTestStore(t)
	if _, _, err := store.put(normalizedWrite{project: "alpha", fileName: "notes/legacy-card.md", topicPath: "runbooks/legacy-card", content: "legacy-card", lifecycle: "durable"}); err != nil {
		t.Fatalf("seed legacy card fixture: %v", err)
	}
	manifestPath := store.currentStateGenerationPath()
	manifestRaw, err := readOwnerOnlyBoundedFile(manifestPath, memoryEdgeLogMaxRecoveryBytes)
	if err != nil {
		t.Fatal(err)
	}
	var manifest memoryCurrentStateGenerationManifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatal(err)
	}
	cardRaw, err := readOwnerOnlyBoundedFile(store.currentStateGenerationCardPath("alpha"), memoryEdgeLogMaxRecoveryBytes)
	if err != nil {
		t.Fatal(err)
	}
	var card memoryCurrentStateGenerationCard
	if err := json.Unmarshal(cardRaw, &card); err != nil {
		t.Fatal(err)
	}
	legacyCount, legacyDigest := memoryCurrentStateGenerationCardsLegacyDigest(map[string]memoryCurrentStateGenerationRecord{"alpha": card.Record})
	manifest.ProjectCardsDigestVersion = 0
	manifest.ProjectCardsCount = legacyCount
	manifest.ProjectCardsDigest = legacyDigest
	legacyRaw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeOwnerOnlyDurableAtomicFile(manifestPath, append(legacyRaw, '\n'), true); err != nil {
		t.Fatal(err)
	}
	if _, err := newMemoryStoreFromEnv(); err != nil {
		t.Fatalf("legacy v3 card digest did not migrate: %v", err)
	}
	migratedRaw, err := readOwnerOnlyBoundedFile(manifestPath, memoryEdgeLogMaxRecoveryBytes)
	if err != nil {
		t.Fatal(err)
	}
	var migrated memoryCurrentStateGenerationManifest
	if err := json.Unmarshal(migratedRaw, &migrated); err != nil {
		t.Fatal(err)
	}
	if migrated.ProjectCardsDigestVersion != memoryCurrentStateGenerationCardsDigestVersion || migrated.ProjectCardsDigest == legacyDigest || migrated.ProjectCardsCount != 1 {
		t.Fatalf("legacy card digest was not replaced by the v%d Patricia root: %#v", memoryCurrentStateGenerationCardsDigestVersion, migrated)
	}
}

func TestMemoryCurrentStateGenerationLegacySparseCardDigestMigrates(t *testing.T) {
	store, _ := newExactStateBoundaryTestStore(t)
	if _, _, err := store.put(normalizedWrite{project: "alpha", fileName: "notes/legacy-sparse-card.md", topicPath: "runbooks/legacy-sparse-card", content: "legacy-sparse-card", lifecycle: "durable"}); err != nil {
		t.Fatalf("seed legacy sparse card fixture: %v", err)
	}
	manifestPath := store.currentStateGenerationPath()
	manifestRaw, err := readOwnerOnlyBoundedFile(manifestPath, memoryEdgeLogMaxRecoveryBytes)
	if err != nil {
		t.Fatal(err)
	}
	var manifest memoryCurrentStateGenerationManifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatal(err)
	}
	cardRaw, err := readOwnerOnlyBoundedFile(store.currentStateGenerationCardPath("alpha"), memoryEdgeLogMaxRecoveryBytes)
	if err != nil {
		t.Fatal(err)
	}
	var card memoryCurrentStateGenerationCard
	if err := json.Unmarshal(cardRaw, &card); err != nil {
		t.Fatal(err)
	}
	legacyCount, legacyDigest := memoryCurrentStateGenerationCardsLegacySparseDigest(map[string]memoryCurrentStateGenerationRecord{"alpha": card.Record})
	manifest.ProjectCardsDigestVersion = memoryCurrentStateGenerationCardsLegacySparseVersion
	manifest.ProjectCardsCount = legacyCount
	manifest.ProjectCardsDigest = legacyDigest
	legacyRaw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeOwnerOnlyDurableAtomicFile(manifestPath, append(legacyRaw, '\n'), true); err != nil {
		t.Fatal(err)
	}
	reloaded, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("legacy sparse card digest did not migrate: %v", err)
	}
	if reloaded.currentStateGenerationCardsDigest == legacyDigest || reloaded.currentStateGenerationManifestVersion != memoryCurrentStateGenerationVersion {
		t.Fatalf("legacy sparse digest was not converted to the v%d root: digest=%s version=%d", memoryCurrentStateGenerationCardsDigestVersion, reloaded.currentStateGenerationCardsDigest, reloaded.currentStateGenerationManifestVersion)
	}
}

func TestMemoryCurrentStateGenerationLegacyTreeCardDigestMigrates(t *testing.T) {
	store, _ := newExactStateBoundaryTestStore(t)
	if _, _, err := store.put(normalizedWrite{project: "alpha", fileName: "notes/legacy-tree.md", topicPath: "runbooks/legacy-tree", content: "legacy-tree", lifecycle: "durable"}); err != nil {
		t.Fatalf("seed legacy tree card fixture: %v", err)
	}
	manifestPath := store.currentStateGenerationPath()
	manifestRaw, err := readOwnerOnlyBoundedFile(manifestPath, memoryEdgeLogMaxRecoveryBytes)
	if err != nil {
		t.Fatal(err)
	}
	var manifest memoryCurrentStateGenerationManifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatal(err)
	}
	cardRaw, err := readOwnerOnlyBoundedFile(store.currentStateGenerationCardPath("alpha"), memoryEdgeLogMaxRecoveryBytes)
	if err != nil {
		t.Fatal(err)
	}
	var card memoryCurrentStateGenerationCard
	if err := json.Unmarshal(cardRaw, &card); err != nil {
		t.Fatal(err)
	}
	legacyCount, legacyDigest := memoryCurrentStateGenerationCardsLegacyTreeDigest(map[string]memoryCurrentStateGenerationRecord{"alpha": card.Record})
	manifest.ProjectCardsDigestVersion = memoryCurrentStateGenerationCardsLegacyTreeVersion
	manifest.ProjectCardsCount = legacyCount
	manifest.ProjectCardsDigest = legacyDigest
	legacyRaw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeOwnerOnlyDurableAtomicFile(manifestPath, append(legacyRaw, '\n'), true); err != nil {
		t.Fatal(err)
	}
	if _, err := newMemoryStoreFromEnv(); err != nil {
		t.Fatalf("legacy tree card digest did not migrate: %v", err)
	}
	migratedRaw, err := readOwnerOnlyBoundedFile(manifestPath, memoryEdgeLogMaxRecoveryBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(migratedRaw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.ProjectCardsDigestVersion != memoryCurrentStateGenerationCardsDigestVersion || manifest.ProjectCardsDigest == legacyDigest || manifest.ProjectCardsCount != 1 {
		t.Fatalf("legacy tree card digest was not replaced by the v%d Patricia root: %#v", memoryCurrentStateGenerationCardsDigestVersion, manifest)
	}
}

func TestMemoryCurrentStateGenerationBucketCardDigestMigrates(t *testing.T) {
	store, _ := newExactStateBoundaryTestStore(t)
	if _, _, err := store.put(normalizedWrite{project: "alpha", fileName: "notes/legacy-bucket.md", topicPath: "runbooks/legacy-bucket", content: "legacy-bucket", lifecycle: "durable"}); err != nil {
		t.Fatalf("seed legacy bucket card fixture: %v", err)
	}
	manifestPath := store.currentStateGenerationPath()
	manifestRaw, err := readOwnerOnlyBoundedFile(manifestPath, memoryEdgeLogMaxRecoveryBytes)
	if err != nil {
		t.Fatal(err)
	}
	var manifest memoryCurrentStateGenerationManifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatal(err)
	}
	cardRaw, err := readOwnerOnlyBoundedFile(store.currentStateGenerationCardPath("alpha"), memoryEdgeLogMaxRecoveryBytes)
	if err != nil {
		t.Fatal(err)
	}
	var card memoryCurrentStateGenerationCard
	if err := json.Unmarshal(cardRaw, &card); err != nil {
		t.Fatal(err)
	}
	legacyCount, legacyDigest := memoryCurrentStateGenerationCardsLegacyBucketDigest(map[string]memoryCurrentStateGenerationRecord{"alpha": card.Record})
	manifest.ProjectCardsDigestVersion = memoryCurrentStateGenerationCardsLegacyBucketVersion
	manifest.ProjectCardsCount = legacyCount
	manifest.ProjectCardsDigest = legacyDigest
	legacyRaw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeOwnerOnlyDurableAtomicFile(manifestPath, append(legacyRaw, '\n'), true); err != nil {
		t.Fatal(err)
	}
	if _, err := newMemoryStoreFromEnv(); err != nil {
		t.Fatalf("legacy bucket card digest did not migrate: %v", err)
	}
	migratedRaw, err := readOwnerOnlyBoundedFile(manifestPath, memoryEdgeLogMaxRecoveryBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(migratedRaw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.ProjectCardsDigestVersion != memoryCurrentStateGenerationCardsDigestVersion || manifest.ProjectCardsDigest == legacyDigest || manifest.ProjectCardsCount != 1 {
		t.Fatalf("legacy bucket card digest was not replaced by the v3 tree root: %#v", manifest)
	}
}

func BenchmarkMemoryCurrentStateIndexedProjectRoot(b *testing.B) {
	for _, rows := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("rows_%d", rows), func(b *testing.B) {
			store := &memoryStore{
				currentState:                    map[string]memoryCurrentState{},
				currentStateByShard:             map[int]map[string]memoryCurrentState{},
				currentStateShardPayloads:       map[int][]byte{},
				currentStateShardDigests:        map[int]string{},
				currentStateProjectShardDigests: map[string]map[int]string{},
				currentKeyIndexGeneration:       map[string]uint64{"alpha": 1},
				currentTopicIndexGeneration:     map[string]uint64{"alpha": 1},
			}
			for index := 0; index < rows; index++ {
				fileName := fmt.Sprintf("notes/%06d.md", index)
				store.currentState[memoryStoreKey("alpha", fileName)] = memoryCurrentStateFromEntry(memoryStoreEntry{Project: "alpha", FileName: fileName, TopicPath: "runbooks/benchmark", Summary: "benchmark"})
			}
			store.mu.Lock()
			store.ensureCurrentStateDigestIndexesLocked()
			store.mu.Unlock()
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				store.mu.RLock()
				_ = store.currentStateDigestReadLocked("alpha")
				store.mu.RUnlock()
			}
		})
	}
}

func BenchmarkMemoryCurrentStateTransactionWriteAtCorpusShape(b *testing.B) {
	const corpusRows = 2_990_000
	for _, projectCount := range []int{1000, 100000} {
		b.Run(fmt.Sprintf("rows_2990000_projects_%d_affected_write", projectCount), func(b *testing.B) {
			root := b.TempDir()
			store := &memoryStore{
				policy: memoryStorePolicy{
					enabled: true, rootPath: root,
					currentStatePath:    filepath.Join(root, "memory_current_state"),
					exactStateIndexPath: filepath.Join(root, "exact_state_paths.json"),
				},
				currentState:                          map[string]memoryCurrentState{},
				currentStateByShard:                   map[int]map[string]memoryCurrentState{},
				currentStateShardPayloads:             map[int][]byte{},
				currentStateShardDigests:              map[int]string{},
				currentStateProjectShardDigests:       map[string]map[int]string{},
				currentKeyIndexGeneration:             map[string]uint64{},
				currentTopicIndexGeneration:           map[string]uint64{},
				currentStateGenerationRecords:         map[string]memoryCurrentStateGenerationRecord{},
				currentStateGenerationManifestLoaded:  true,
				currentStateGenerationManifestVersion: memoryCurrentStateGenerationVersion,
			}
			for rowIndex := 0; rowIndex < corpusRows; rowIndex++ {
				projectIndex := rowIndex % projectCount
				project := fmt.Sprintf("high-cardinality-project-%06d", projectIndex)
				fileName := fmt.Sprintf("notes/corpus-%07d.md", rowIndex)
				entry := memoryStoreEntry{
					Project: project, FileName: fileName, TopicPath: "runbooks/benchmark",
					Summary: "corpus-shape", EventID: fmt.Sprintf("corpus-%07d", rowIndex), CreatedAt: "2026-01-01T00:00:00Z",
				}
				store.currentState[memoryStoreKey(project, fileName)] = memoryCurrentStateFromEntry(entry)
			}
			project := "high-cardinality-project-000042"
			fileName := "notes/corpus-0000042.md"
			key := memoryStoreKey(project, fileName)
			for projectIndex := 0; projectIndex < projectCount; projectIndex++ {
				cardProject := fmt.Sprintf("high-cardinality-project-%06d", projectIndex)
				store.currentKeyIndexGeneration[cardProject] = 1
				store.currentTopicIndexGeneration[cardProject] = 1
			}
			store.mu.Lock()
			store.ensureCurrentStateDigestIndexesLocked()
			for projectIndex := 0; projectIndex < projectCount; projectIndex++ {
				cardProject := fmt.Sprintf("high-cardinality-project-%06d", projectIndex)
				store.currentStateGenerationRecords[cardProject] = memoryCurrentStateGenerationRecord{
					KeyGeneration: 1, TopicGeneration: 1,
					StateDigest: memoryCurrentStateRootDigest(cardProject, 1, store.currentStateProjectShardDigests[cardProject]),
				}
			}
			store.setCurrentStateGenerationCardsAccumulatorLocked(store.currentStateGenerationRecords)
			store.mu.Unlock()
			if err := ensureOwnerOnlyDirectory(store.currentStateRootPath(), true); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ReportMetric(float64(corpusRows), "corpus_rows")
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				store.mu.Lock()
				updated := memoryStoreEntry{
					Project: project, FileName: fileName, TopicPath: "runbooks/benchmark",
					Summary: "affected-write", EventID: fmt.Sprintf("zz-affected-%08d", index+1), CreatedAt: "2026-01-01T00:00:00Z",
				}
				if !store.applyCurrentStateEntryLocked(updated) {
					store.mu.Unlock()
					b.Fatal("affected current-state update was rejected")
				}
				err := store.persistCurrentStateTransactionLocked(map[int]struct{}{memoryCurrentStateShardForKey(key): {}}, project, 1)
				store.mu.Unlock()
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
