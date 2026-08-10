package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func currentStateSearchTestStore(entries ...memoryStoreEntry) *memoryStore {
	store := &memoryStore{
		policy:                         memoryStorePolicy{enabled: true, maxSummaryChars: 400, maxRecent: 1024, rollupUseHistoryIndex: true},
		currentState:                   map[string]memoryCurrentState{},
		currentKeysByProject:           map[string]map[string]struct{}{},
		currentKeyCountsByProject:      map[string]int{},
		currentKeysByProjectTopic:      map[string]map[string]map[string]struct{}{},
		currentTopicKeyCountsByProject: map[string]int{},
		currentKeyIndexGeneration:      map[string]uint64{},
		currentTopicIndexGeneration:    map[string]uint64{},
		latestTopic:                    map[string]string{},
		latestHash:                     map[string]string{},
		latestHorizon:                  map[string]int{},
		latestLifecycle:                map[string]string{},
		latestStorageTier:              map[string]string{},
		lastAccess:                     map[string]time.Time{},
		confidence:                     map[string]confidenceState{},
		rollupCache:                    map[string]topicRollupCacheEntry{},
		exactStatePaths:                map[string]struct{}{},
	}
	for _, entry := range entries {
		key := memoryStoreKey(entry.Project, entry.FileName)
		store.currentState[key] = memoryCurrentStateFromEntry(entry)
		store.addCurrentKeyLocked(entry.Project, key, entry.TopicPath)
		store.latestTopic[key] = entry.TopicPath
	}
	store.ready.Store(true)
	return store
}

func TestCurrentStateSearchRanksExactSummaryWithoutFilenameOracle(t *testing.T) {
	created := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	store := currentStateSearchTestStore(
		memoryStoreEntry{
			EventID: "event-target", Project: "alpha", FileName: "notes/target.md", TopicPath: "retrieval/quality",
			Summary: "bounded semantic retrieval preserves calibrated evidence and exact citations", Lifecycle: "durable", CreatedAt: created,
		},
		memoryStoreEntry{
			EventID: "event-distractor", Project: "alpha", FileName: "notes/distractor.md", TopicPath: "retrieval/quality",
			Summary: "generic retrieval notes without the calibrated evidence contract", Lifecycle: "durable", CreatedAt: created,
		},
		memoryStoreEntry{
			EventID: "event-other", Project: "other", FileName: "notes/other.md", TopicPath: "retrieval/quality",
			Summary: "bounded semantic retrieval preserves calibrated evidence and exact citations", Lifecycle: "durable", CreatedAt: created,
		},
	)
	rows, stats, err := store.searchCurrentStateRows(
		context.Background(),
		"retrieval quality bounded semantic retrieval preserves calibrated evidence and exact citations",
		"alpha",
		"retrieval/quality",
		10,
		false,
		false,
	)
	if err != nil {
		t.Fatalf("current-state search failed: %v", err)
	}
	if stats.ProjectDocuments != 2 || stats.Scanned != 2 || len(rows) != 2 {
		t.Fatalf("search was not project-proportional: stats=%#v rows=%#v", stats, rows)
	}
	if anyToString(rows[0]["file"]) != "notes/target.md" || anyToString(rows[0]["retrieval_lane"]) != "current_state_index" {
		t.Fatalf("expected the exact current-state citation first, got %#v", rows)
	}
	for _, row := range rows {
		if anyToString(row["project"]) != "alpha" {
			t.Fatalf("cross-project row leaked into search: %#v", row)
		}
	}

	filenameOnly, _, err := store.searchCurrentStateRows(
		context.Background(), "target.md", "alpha", "retrieval/quality", 10, false, false,
	)
	if err != nil {
		t.Fatalf("filename-only search failed: %v", err)
	}
	if len(filenameOnly) != 0 {
		t.Fatalf("file identity became a ranking oracle: %#v", filenameOnly)
	}
}

func TestCurrentStateSearchFailsClosedOnCorruptIndexAndFiltersLifecycle(t *testing.T) {
	created := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	store := currentStateSearchTestStore(
		memoryStoreEntry{EventID: "active", Project: "alpha", FileName: "notes/active.md", TopicPath: "runbooks/active", Summary: "active bounded procedure", Lifecycle: "durable", CreatedAt: created},
		memoryStoreEntry{EventID: "retired", Project: "alpha", FileName: "notes/retired.md", TopicPath: "runbooks/active", Summary: "retired bounded procedure", Lifecycle: "retired", CreatedAt: created},
	)
	rows, _, err := store.searchCurrentStateRows(context.Background(), "bounded procedure", "alpha", "runbooks/active", 10, false, false)
	if err != nil {
		t.Fatalf("current-state search failed: %v", err)
	}
	if len(rows) != 1 || anyToString(rows[0]["file"]) != "notes/active.md" {
		t.Fatalf("hidden lifecycle row surfaced: %#v", rows)
	}

	store.currentKeyCountsByProject["alpha"]++
	_, _, err = store.searchCurrentStateRows(context.Background(), "bounded procedure", "alpha", "runbooks/active", 10, false, false)
	if !errors.Is(err, errCurrentStateSearchIndexUnavailable) {
		t.Fatalf("corrupt project index did not fail closed: %v", err)
	}
}

func TestCurrentStateSearchFailsClosedOnTopicProjectionCorruption(t *testing.T) {
	created := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	store := currentStateSearchTestStore(memoryStoreEntry{
		EventID: "active", Project: "alpha", FileName: "notes/active.md", TopicPath: "runbooks/active",
		Summary: "active bounded procedure", Lifecycle: "durable", CreatedAt: created,
	})

	store.currentTopicKeyCountsByProject["alpha"]++
	_, _, err := store.searchCurrentStateRows(context.Background(), "bounded procedure", "alpha", "runbooks/active", 10, false, false)
	if !errors.Is(err, errCurrentStateSearchIndexUnavailable) {
		t.Fatalf("topic count corruption did not fail closed: %v", err)
	}
	store.currentTopicKeyCountsByProject["alpha"]--
	store.currentTopicIndexGeneration["alpha"]++
	_, _, err = store.searchCurrentStateRows(context.Background(), "bounded procedure", "alpha", "runbooks/active", 10, false, false)
	if !errors.Is(err, errCurrentStateSearchIndexUnavailable) {
		t.Fatalf("topic generation corruption did not fail closed: %v", err)
	}
}

func TestCurrentStateSearchScansOnlySelectedTopicRows(t *testing.T) {
	created := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	entries := make([]memoryStoreEntry, 0, 20003)
	for index := 0; index < 20000; index++ {
		entries = append(entries, memoryStoreEntry{
			EventID: fmt.Sprintf("noise-%05d", index), Project: "large", FileName: fmt.Sprintf("noise/%05d.md", index),
			TopicPath: "telemetry/bulk", Summary: "unrelated telemetry sample", Lifecycle: "durable", CreatedAt: created,
		})
	}
	for index := 0; index < 3; index++ {
		entries = append(entries, memoryStoreEntry{
			EventID: fmt.Sprintf("target-%d", index), Project: "large", FileName: fmt.Sprintf("target/%d.md", index),
			TopicPath: "runbooks/active", Summary: "bounded active recovery procedure", Lifecycle: "durable", CreatedAt: created,
		})
	}
	store := currentStateSearchTestStore(entries...)
	rows, stats, err := store.searchCurrentStateRows(
		context.Background(), "bounded active recovery procedure", "large", "runbooks/active", 10, false, false,
	)
	if err != nil || len(rows) != 3 {
		t.Fatalf("topic-projected search failed: rows=%d stats=%#v err=%v", len(rows), stats, err)
	}
	if stats.ProjectDocuments != 20003 || stats.ProjectTopics != 2 || stats.TopicsScanned != 2 || stats.Scanned != 3 {
		t.Fatalf("search did not stay selected-topic proportional: %#v", stats)
	}
}

func TestCurrentStateTopicProjectionTracksTopicMoveAndTombstone(t *testing.T) {
	base := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	store := currentStateSearchTestStore(memoryStoreEntry{
		EventID: "initial", Project: "alpha", FileName: "notes/active.md", TopicPath: "runbooks/old",
		Summary: "bounded recovery procedure", Lifecycle: "durable", CreatedAt: base.Format(time.RFC3339Nano),
	})
	store.recordEntry(memoryStoreEntry{
		EventID: "moved", Project: "alpha", FileName: "notes/active.md", TopicPath: "runbooks/new",
		Summary: "bounded recovery procedure", Lifecycle: "durable", CreatedAt: base.Add(time.Minute).Format(time.RFC3339Nano),
	})

	oldRows, _, err := store.searchCurrentStateRows(context.Background(), "bounded recovery procedure", "alpha", "runbooks/old", 10, false, false)
	if err != nil || len(oldRows) != 0 {
		t.Fatalf("moved row remained in the old topic projection: rows=%#v err=%v", oldRows, err)
	}
	newRows, stats, err := store.searchCurrentStateRows(context.Background(), "bounded recovery procedure", "alpha", "runbooks/new", 10, false, false)
	if err != nil || len(newRows) != 1 || stats.ProjectDocuments != 1 || stats.ProjectTopics != 1 {
		t.Fatalf("moved row was not atomically projected: rows=%#v stats=%#v err=%v", newRows, stats, err)
	}

	store.recordEntry(memoryStoreEntry{
		EventID: "deleted", Project: "alpha", FileName: "notes/active.md", TopicPath: "runbooks/new",
		DataClass: "memory_tombstone", CreatedAt: base.Add(2 * time.Minute).Format(time.RFC3339Nano),
	})
	rows, stats, err := store.searchCurrentStateRows(context.Background(), "bounded recovery procedure", "alpha", "runbooks/new", 10, false, false)
	if err != nil || len(rows) != 0 || stats.ProjectDocuments != 0 || stats.ProjectTopics != 0 {
		t.Fatalf("tombstone did not remove the topic projection: rows=%#v stats=%#v err=%v", rows, stats, err)
	}
	if store.currentKeyIndexGeneration["alpha"] != store.currentTopicIndexGeneration["alpha"] {
		t.Fatalf("topic projection generation diverged after mutation")
	}
}

func TestCurrentStateSearchUsesConfiguredProjectPopulationBound(t *testing.T) {
	created := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	entries := make([]memoryStoreEntry, 0, 1001)
	for index := 0; index < 1001; index++ {
		entries = append(entries, memoryStoreEntry{
			EventID:   fmt.Sprintf("event-%04d", index),
			Project:   "large",
			FileName:  fmt.Sprintf("notes/%04d.md", index),
			TopicPath: "retrieval/large",
			Summary:   "bounded large project retrieval evidence",
			Lifecycle: "durable",
			CreatedAt: created,
		})
	}
	store := currentStateSearchTestStore(entries...)

	t.Setenv("GO_MEMORY_CURRENT_STATE_SEARCH_MAX_PROJECT_DOCS", "250000")
	rows, stats, err := store.searchCurrentStateRows(
		context.Background(), "large project retrieval evidence", "large", "retrieval/large", 10, false, false,
	)
	if err != nil || stats.ProjectDocuments != 1001 || stats.Scanned != 1001 || len(rows) != 10 {
		t.Fatalf("configured production bound rejected a valid indexed project: stats=%#v rows=%d err=%v", stats, len(rows), err)
	}

	t.Setenv("GO_MEMORY_CURRENT_STATE_SEARCH_MAX_PROJECT_DOCS", "1000")
	_, stats, err = store.searchCurrentStateRows(
		context.Background(), "large project retrieval evidence", "large", "retrieval/large", 10, false, false,
	)
	if !errors.Is(err, errCurrentStateSearchIndexUnavailable) || stats.ProjectDocuments != 1001 {
		t.Fatalf("configured project bound did not fail closed with its denominator: stats=%#v err=%v", stats, err)
	}
	if !strings.Contains(err.Error(), "project_documents=1001 configured_limit=1000") {
		t.Fatalf("project-bound failure omitted bounded diagnostics: %v", err)
	}
}

func TestCurrentStateSearchNearestAncestorFillsSparseExactScope(t *testing.T) {
	created := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	store := currentStateSearchTestStore(
		memoryStoreEntry{
			EventID: "exact", ContentHash: "sha256:exact", Project: "alpha", FileName: "notes/exact.md",
			TopicPath: "runbooks/cache/worker", Summary: "cache worker rollback procedure with verified recovery",
			Lifecycle: "durable", CreatedAt: created,
		},
		memoryStoreEntry{
			EventID: "sibling-one", ContentHash: "sha256:sibling-one", Project: "alpha", FileName: "notes/rebuild.md",
			TopicPath: "runbooks/cache/rebuild", Summary: "cache rebuild procedure and verified rollback evidence",
			Lifecycle: "durable", CreatedAt: created,
		},
		memoryStoreEntry{
			EventID: "sibling-two", ContentHash: "sha256:sibling-two", Project: "alpha", FileName: "notes/recovery.md",
			TopicPath: "runbooks/cache/recovery", Summary: "cache recovery procedure with rollback verification",
			Lifecycle: "durable", CreatedAt: created,
		},
		memoryStoreEntry{
			EventID: "other-parent", ContentHash: "sha256:other-parent", Project: "alpha", FileName: "notes/network.md",
			TopicPath: "runbooks/network/recovery", Summary: "cache recovery procedure with rollback verification",
			Lifecycle: "durable", CreatedAt: created,
		},
		memoryStoreEntry{
			EventID: "other-project", ContentHash: "sha256:other-project", Project: "other", FileName: "notes/cache.md",
			TopicPath: "runbooks/cache/recovery", Summary: "cache recovery procedure with rollback verification",
			Lifecycle: "durable", CreatedAt: created,
		},
	)

	rows, stats, err := store.searchCurrentStateRowsWithAncestorFallback(
		context.Background(),
		"cache worker rollback procedure verified recovery",
		"alpha",
		"runbooks/cache/worker",
		5,
		false,
		false,
		3,
		0.55,
		0.25,
	)
	if err != nil {
		t.Fatalf("ancestor fallback search failed: %v", err)
	}
	if len(rows) != 3 || anyToString(rows[0]["file"]) != "notes/exact.md" {
		t.Fatalf("exact row was not retained first with bounded ancestor fill: %#v", rows)
	}
	if anyToString(rows[0]["retrieval_scope"]) != currentStateRetrievalScopeExact {
		t.Fatalf("exact row scope was not explicit: %#v", rows[0])
	}
	for _, row := range rows[1:] {
		if anyToString(row["retrieval_scope"]) != currentStateRetrievalScopeAncestor ||
			!strings.HasPrefix(anyToString(row["topic_path"]), "runbooks/cache/") {
			t.Fatalf("fallback escaped the nearest topic ancestor: %#v", row)
		}
	}
	if stats.ProjectDocuments != 4 || stats.Scanned != 4 || stats.ExactMatched != 1 ||
		stats.AncestorMatched != 3 || !stats.AncestorUsed || stats.AncestorPrefix != "runbooks/cache" {
		t.Fatalf("ancestor fallback stats are not truthful: %#v", stats)
	}
}

func TestCurrentStateSearchDoesNotUseAncestorWhenExactScopeIsSufficient(t *testing.T) {
	created := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	entries := []memoryStoreEntry{
		{EventID: "exact-one", ContentHash: "sha256:one", Project: "alpha", FileName: "notes/one.md", TopicPath: "runbooks/cache/worker", Summary: "cache worker verified rollback procedure one", Lifecycle: "durable", CreatedAt: created},
		{EventID: "exact-two", ContentHash: "sha256:two", Project: "alpha", FileName: "notes/two.md", TopicPath: "runbooks/cache/worker/detail", Summary: "cache worker verified rollback procedure two", Lifecycle: "durable", CreatedAt: created},
		{EventID: "exact-three", ContentHash: "sha256:three", Project: "alpha", FileName: "notes/three.md", TopicPath: "runbooks/cache/worker", Summary: "cache worker verified rollback procedure three", Lifecycle: "durable", CreatedAt: created},
		{EventID: "sibling", ContentHash: "sha256:sibling", Project: "alpha", FileName: "notes/sibling.md", TopicPath: "runbooks/cache/recovery", Summary: "cache worker verified rollback procedure sibling", Lifecycle: "durable", CreatedAt: created},
	}
	store := currentStateSearchTestStore(entries...)
	rows, stats, err := store.searchCurrentStateRowsWithAncestorFallback(
		context.Background(), "cache worker verified rollback procedure", "alpha", "runbooks/cache/worker", 5, false, false, 3, 0.55, 0.25,
	)
	if err != nil || len(rows) != 3 {
		t.Fatalf("sufficient exact scope search failed: rows=%#v stats=%#v err=%v", rows, stats, err)
	}
	for _, row := range rows {
		if anyToString(row["retrieval_scope"]) != currentStateRetrievalScopeExact {
			t.Fatalf("ancestor row was used despite sufficient exact coverage: %#v", rows)
		}
	}
	if stats.AncestorUsed || stats.AncestorMatched != 1 {
		t.Fatalf("ancestor candidate accounting was not separated from use: %#v", stats)
	}
}

func TestCurrentStateSearchWalksBoundedAncestorChainNearestFirst(t *testing.T) {
	created := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	store := currentStateSearchTestStore(
		memoryStoreEntry{EventID: "exact", ContentHash: "sha256:exact", Project: "alpha", FileName: "notes/exact.md", TopicPath: "orchestration/autonomous/recovery", Summary: "autonomous recovery verified procedure", Lifecycle: "durable", CreatedAt: created},
		memoryStoreEntry{EventID: "sibling", ContentHash: "sha256:sibling", Project: "alpha", FileName: "notes/repository.md", TopicPath: "orchestration/repository", Summary: "orchestration repository recovery procedure", Lifecycle: "durable", CreatedAt: created},
		memoryStoreEntry{EventID: "outside", ContentHash: "sha256:outside", Project: "alpha", FileName: "notes/policy.md", TopicPath: "policy/repository", Summary: "orchestration repository recovery procedure", Lifecycle: "durable", CreatedAt: created},
	)
	rows, stats, err := store.searchCurrentStateRowsWithAncestorFallback(
		context.Background(), "orchestration autonomous recovery verified procedure", "alpha", "orchestration/autonomous/recovery", 5, false, false, 2, 0.55, 0.15,
	)
	if err != nil || len(rows) != 2 {
		t.Fatalf("bounded ancestor chain did not fill sparse exact scope: rows=%#v stats=%#v err=%v", rows, stats, err)
	}
	ancestor := rows[1]
	if anyToString(ancestor["file"]) != "notes/repository.md" ||
		anyToInt(ancestor["retrieval_ancestor_distance"], 0) != 2 ||
		anyToString(ancestor["retrieval_ancestor_prefix"]) != "orchestration" {
		t.Fatalf("ancestor chain did not disclose its bounded widening: %#v", ancestor)
	}
	if stats.AncestorDepth != 2 || stats.AncestorPrefix != "orchestration" || !stats.ScopeExhaustive {
		t.Fatalf("ancestor chain stats were not truthful: %#v", stats)
	}
}

func TestCurrentStateSearchAncestorFallbackHonorsCancellation(t *testing.T) {
	store := currentStateSearchTestStore(memoryStoreEntry{
		EventID: "event", ContentHash: "sha256:event", Project: "alpha", FileName: "notes/event.md",
		TopicPath: "runbooks/cache/worker", Summary: "cache worker procedure", Lifecycle: "durable",
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := store.searchCurrentStateRowsWithAncestorFallback(
		ctx, "cache worker procedure", "alpha", "runbooks/cache/worker", 5, false, false, 3, 0.55, 0.25,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ancestor search did not stop: %v", err)
	}
}

func TestCurrentStateLexicalScoreDropsHighEntropyMetadataTokens(t *testing.T) {
	query := "orchestration review adjudication id=deleg_4b324ada ts=2026-08-09T12:34:56Z sha256:0123456789abcdef0123456789abcdef"
	score := currentStateLexicalScore(query, "orchestration/review", "review adjudication completed with verified evidence")
	if score < 0.7 {
		t.Fatalf("high-entropy metadata diluted semantic current-state match: score=%f", score)
	}
}

func TestTopicRollupSourceUsesCurrentStateCitationAndSkipsHistoricalClaim(t *testing.T) {
	created := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	store := currentStateSearchTestStore(memoryStoreEntry{
		EventID: "event-current", Project: "alpha", FileName: "notes/current.md", TopicPath: "runbooks/current",
		Summary: "current indexed procedure with verified rollback", Lifecycle: "durable", CreatedAt: created,
	})
	s := &server{memoryStore: store}
	rows, warnings, err := s.queryTopicRollupsSource(context.Background(), nil, map[string]any{
		"query": "runbooks current indexed procedure verified rollback", "project": "alpha", "topic_path": "runbooks/current", "limit": 5,
	})
	if err != nil || len(warnings) != 0 || len(rows) != 1 {
		t.Fatalf("current-state topic lane failed: rows=%#v warnings=%#v err=%v", rows, warnings, err)
	}
	if anyToString(rows[0]["file"]) != "notes/current.md" || anyToString(rows[0]["retrieval_lane"]) != "current_state_index" {
		t.Fatalf("topic lane returned a synthetic rollup instead of a file citation: %#v", rows[0])
	}
	if currentStateSearchAsOfSupported("2025-01-01T00:00:00Z") {
		t.Fatal("historical as_of was incorrectly treated as current-state replay")
	}
}

func TestAuthoritativeCurrentStateFastPathSkipsDefaultedRedundantSources(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "topic_rollups,mindsdb")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "topic_rollups")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "mindsdb")
	t.Setenv("GO_RETRIEVAL_AUTHORITATIVE_CURRENT_STATE_FAST_PATH_ENABLED", "true")

	var backendCalls atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[],"warnings":[]}`))
	}))
	defer backend.Close()

	created := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	store := currentStateSearchTestStore(
		memoryStoreEntry{
			EventID: "event-current", ContentHash: "sha256:current", Project: "alpha", FileName: "notes/current.md",
			TopicPath: "runbooks/current", Summary: "current indexed procedure with verified rollback",
			Lifecycle: "durable", CreatedAt: created,
		},
		memoryStoreEntry{
			EventID: "event-proof", ContentHash: "sha256:proof", Project: "alpha", FileName: "notes/proof.md",
			TopicPath: "runbooks/current", Summary: "current indexed procedure rollback proof and verification",
			Lifecycle: "durable", CreatedAt: created,
		},
	)
	s := newTestServer(t, backend.URL)
	s.memoryStore = store
	ctx := withContextPackDefaultBlockingSources(context.Background())
	preflight, selected := s.authoritativeCurrentStateFastPath(ctx, map[string]any{
		"query": "current indexed procedure with verified rollback", "project": "alpha", "topic_path": "runbooks/current", "limit": 5,
	}, "fast", 1)
	if !selected {
		t.Fatalf("valid authoritative current-state row was not selected: %#v", preflight)
	}
	response, status, err := s.executeRetrieval(ctx, nil, map[string]any{
		"query": "current indexed procedure with verified rollback", "project": "alpha", "topic_path": "runbooks/current",
		"limit": 5, "sync_slow_sources": true, "wait_for_slow_sources": true,
	}, true)
	if err != nil || status != http.StatusOK {
		t.Fatalf("authoritative fast path failed: status=%d err=%v response=%#v", status, err, response)
	}
	if backendCalls.Load() != 0 {
		t.Fatalf("defaulted redundant source was queried %d time(s)", backendCalls.Load())
	}
	debug := anyMap(response["retrieval_debug"])
	policy := anyMap(debug["source_policy"])
	if !anyToBool(policy["authoritative_current_state_fast_path"]) || anyToBool(policy["blocking_slow_sources"]) {
		t.Fatalf("fast-path policy was not projected truthfully: %#v", policy)
	}
	counts, _ := debug["source_counts"].(map[string]int)
	if counts[sourceTopicRollup] != 2 || len(anyMap(debug["source_errors"])) != 0 {
		t.Fatalf("unexpected fast-path source result: %#v", debug)
	}
	warnings := parseWarnings(response["warnings"])
	if len(warnings) != 1 || warnings[0] != "Authoritative current-state fast path satisfied scoped retrieval; redundant sources were not queried." {
		t.Fatalf("authoritative fast-path notice mismatch: %#v", warnings)
	}
}

func TestAuthoritativeCurrentStateFastPathUsesBoundedAncestorFill(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_AUTHORITATIVE_CURRENT_STATE_FAST_PATH_ENABLED", "true")
	t.Setenv("GO_RETRIEVAL_AUTHORITATIVE_CURRENT_STATE_MIN_SCORE", "0.55")
	t.Setenv("GO_RETRIEVAL_AUTHORITATIVE_CURRENT_STATE_ANCESTOR_MIN_SCORE", "0.25")
	created := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	store := currentStateSearchTestStore(
		memoryStoreEntry{EventID: "exact", ContentHash: "sha256:exact", Project: "alpha", FileName: "notes/exact.md", TopicPath: "runbooks/cache/worker", Summary: "cache worker rollback procedure verified recovery", Lifecycle: "durable", CreatedAt: created},
		memoryStoreEntry{EventID: "sibling-one", ContentHash: "sha256:sibling-one", Project: "alpha", FileName: "notes/rebuild.md", TopicPath: "runbooks/cache/rebuild", Summary: "cache rebuild procedure verified rollback", Lifecycle: "durable", CreatedAt: created},
		memoryStoreEntry{EventID: "sibling-two", ContentHash: "sha256:sibling-two", Project: "alpha", FileName: "notes/recovery.md", TopicPath: "runbooks/cache/recovery", Summary: "cache recovery procedure verified rollback", Lifecycle: "durable", CreatedAt: created},
	)
	s := &server{memoryStore: store}
	output, selected := s.authoritativeCurrentStateFastPath(context.Background(), map[string]any{
		"query": "cache worker rollback procedure verified recovery", "project": "alpha", "topic_path": "runbooks/cache/worker", "limit": 5,
	}, "fast", 3)
	if !selected || len(output.rows[sourceTopicRollup]) != 3 {
		t.Fatalf("bounded ancestor fill did not satisfy authoritative fast path: selected=%v output=%#v", selected, output)
	}
	debugRows := output.sourceChainDebug[sourceTopicRollup]
	if len(debugRows) != 1 || !anyToBool(debugRows[0]["ancestor_used"]) ||
		anyToInt(debugRows[0]["qualified_exact_rows"], 0) != 1 ||
		anyToInt(debugRows[0]["qualified_ancestor_rows"], 0) != 2 {
		t.Fatalf("ancestor qualification proof is incomplete: %#v", debugRows)
	}
}

func TestAuthoritativeCurrentStateFastPathRelaxesCardinalityOnlyForExhaustiveExactScope(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_AUTHORITATIVE_CURRENT_STATE_FAST_PATH_ENABLED", "true")
	created := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	store := currentStateSearchTestStore(memoryStoreEntry{
		EventID: "exact", ContentHash: "sha256:exact", Project: "alpha", FileName: "notes/exact.md",
		TopicPath: "orchestration/autonomous/recovery", Summary: "autonomous recovery verified procedure",
		Lifecycle: "durable", CreatedAt: created,
	})
	s := &server{memoryStore: store}
	output, selected := s.authoritativeCurrentStateFastPath(context.Background(), map[string]any{
		"query": "orchestration autonomous recovery verified procedure", "project": "alpha", "topic_path": "orchestration/autonomous/recovery", "limit": 5,
	}, "fast", 3)
	if !selected || len(output.rows[sourceTopicRollup]) != 1 {
		t.Fatalf("exhaustive single-row scope did not avoid redundant sources: selected=%v output=%#v", selected, output)
	}
	debug := output.sourceChainDebug[sourceTopicRollup]
	if len(debug) != 1 || !anyToBool(debug[0]["minimum_rows_relaxed"]) ||
		anyToString(debug[0]["minimum_rows_relaxed_reason"]) != "authoritative_scoped_universe_exhausted" {
		t.Fatalf("cardinality relaxation was not explicitly proven: %#v", debug)
	}
}

func TestAuthoritativeCurrentStateFastPathCapsPreCompilerRows(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_AUTHORITATIVE_CURRENT_STATE_FAST_PATH_ENABLED", "true")
	t.Setenv("GO_RETRIEVAL_AUTHORITATIVE_CURRENT_STATE_MAX_ROWS", "4")
	created := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	entries := make([]memoryStoreEntry, 0, 10)
	for index := 0; index < 10; index++ {
		entries = append(entries, memoryStoreEntry{
			EventID: fmt.Sprintf("event-%d", index), ContentHash: fmt.Sprintf("sha256:%d", index),
			Project: "alpha", FileName: fmt.Sprintf("notes/%d.md", index), TopicPath: "runbooks/cache/worker",
			Summary: "cache worker verified rollback procedure", Lifecycle: "durable", CreatedAt: created,
		})
	}
	s := &server{memoryStore: currentStateSearchTestStore(entries...)}
	output, selected := s.authoritativeCurrentStateFastPath(context.Background(), map[string]any{
		"query": "cache worker verified rollback procedure", "project": "alpha", "topic_path": "runbooks/cache/worker", "limit": 10,
	}, "fast", 2)
	if !selected || len(output.rows[sourceTopicRollup]) != 4 {
		t.Fatalf("pre-compiler authoritative rows were not bounded: selected=%v rows=%d", selected, len(output.rows[sourceTopicRollup]))
	}
	debug := output.sourceChainDebug[sourceTopicRollup]
	if len(debug) != 1 || anyToInt(debug[0]["requested_row_limit"], 0) != 10 || anyToInt(debug[0]["applied_row_limit"], 0) != 4 {
		t.Fatalf("row-bound proof is incomplete: %#v", debug)
	}
}

func TestContextPackPreservesNearestAncestorEvidenceScope(t *testing.T) {
	pack := buildContextPackPayload("cache recovery rollback", map[string]any{
		"results": []map[string]any{{
			"project": "alpha", "file": "notes/recovery.md", "source": sourceTopicRollup,
			"topic_path": "runbooks/cache/recovery", "summary": "cache recovery rollback procedure",
			"score": 0.7, "retrieval_scope": currentStateRetrievalScopeAncestor,
			"retrieval_ancestor_prefix": "runbooks/cache", "retrieval_ancestor_distance": 1,
		}},
		"grounding": map[string]any{"facts": []any{}, "numeric_facts": []any{}},
	}, 10, 10)
	compiled := compileContextPackForAgent(
		"cache recovery rollback",
		pack,
		map[string]any{"configured": []any{sourceTopicRollup}, "returned": []any{sourceTopicRollup}, "complete": true},
		objectiveContext{},
		contextPackTokenBudget{TargetContextPackTokens: 4096, RankedEvidenceTokens: 2048, Active: true},
	)
	evidence := contextPackAnyList(compiled["ranked_evidence"])
	if len(evidence) == 0 {
		t.Fatalf("ancestor-scoped row was lost during context compilation: %#v", compiled)
	}
	first := anyMap(evidence[0])
	if anyToString(first["retrieval_scope"]) != currentStateRetrievalScopeAncestor {
		t.Fatalf("ancestor scope was not preserved in ranked evidence: %#v", first)
	}
	if anyToString(first["retrieval_ancestor_prefix"]) != "runbooks/cache" ||
		anyToInt(first["retrieval_ancestor_distance"], 0) != 1 {
		t.Fatalf("ancestor distance proof was not preserved in ranked evidence: %#v", first)
	}
	foundSignal := false
	for _, signal := range contextPackAnyList(first["why_selected"]) {
		if anyToString(signal) == "nearest_topic_ancestor_context" {
			foundSignal = true
			break
		}
	}
	if !foundSignal {
		t.Fatalf("ancestor evidence did not disclose why it was selected: %#v", first)
	}
}

func BenchmarkCurrentStateSearchTopicProjection(b *testing.B) {
	created := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	entries := make([]memoryStoreEntry, 0, 50004)
	for index := 0; index < 50000; index++ {
		entries = append(entries, memoryStoreEntry{
			EventID: fmt.Sprintf("noise-%05d", index), Project: "large", FileName: fmt.Sprintf("noise/%05d.md", index),
			TopicPath: "telemetry/bulk", Summary: "unrelated telemetry sample", Lifecycle: "durable", CreatedAt: created,
		})
	}
	for index := 0; index < 4; index++ {
		entries = append(entries, memoryStoreEntry{
			EventID: fmt.Sprintf("target-%d", index), Project: "large", FileName: fmt.Sprintf("target/%d.md", index),
			TopicPath: "runbooks/active", Summary: "bounded active recovery procedure", Lifecycle: "durable", CreatedAt: created,
		})
	}
	store := currentStateSearchTestStore(entries...)
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		rows, stats, err := store.searchCurrentStateRows(
			context.Background(), "bounded active recovery procedure", "large", "runbooks/active", 10, false, false,
		)
		if err != nil || len(rows) != 4 || stats.Scanned != 4 {
			b.Fatalf("topic-projected benchmark failed: rows=%d stats=%#v err=%v", len(rows), stats, err)
		}
	}
}
