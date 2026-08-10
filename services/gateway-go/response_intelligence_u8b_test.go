package main

import (
	"fmt"
	"testing"
	"time"
)

func u8bObjectiveTransition(objectiveID, transitionID, status string, occurredAt time.Time, sequence uint64) objectiveTransition {
	return objectiveTransition{
		SchemaID:       objectiveTransitionContractID,
		TransitionID:   transitionID,
		ObjectiveID:    objectiveID,
		Project:        "contextlattice",
		Objective:      "bounded objective replay",
		TransitionType: "progressed",
		ToStatus:       status,
		Actor:          "test",
		Summary:        transitionID,
		IdempotencyKey: "idem_" + transitionID,
		OccurredAt:     occurredAt.UTC().Format(time.RFC3339Nano),
		RecordedAt:     occurredAt.UTC().Format(time.RFC3339Nano),
		ledgerSequence: sequence,
	}
}

func newU8bObjectiveStore() *continuityStore {
	store := &continuityStore{enabled: true}
	store.ensureIndexesLocked()
	return store
}

func TestU8bObjectiveReplayUsesOnlySelectedIndexesAndFailsClosed(t *testing.T) {
	store := newU8bObjectiveStore()
	base := time.Date(2026, time.August, 9, 8, 0, 0, 0, time.UTC)
	const unrelated = 20000
	for index := 0; index < unrelated; index++ {
		store.applyObjectiveTransitionValidatedLocked(u8bObjectiveTransition(
			fmt.Sprintf("unrelated_%05d", index), fmt.Sprintf("ot_unrelated_%05d", index), "active", base, uint64(index+1),
		))
	}
	selected := []objectiveTransition{
		u8bObjectiveTransition("selected", "ot_selected_late", "completed", base.Add(2*time.Hour), unrelated+1),
		u8bObjectiveTransition("selected", "ot_selected_early", "active", base, unrelated+2),
		u8bObjectiveTransition("selected", "ot_selected_mid", "blocked", base.Add(time.Hour), unrelated+3),
		u8bObjectiveTransition("selected", "ot_selected_future", "completed", base.Add(4*time.Hour), unrelated+4),
	}
	for _, transition := range selected {
		store.applyObjectiveTransitionValidatedLocked(transition)
	}

	graph := store.objectiveGraph("contextlattice", "selected", base.Add(3*time.Hour), true, 100)
	if !anyToBool(graph["ok"]) || !anyToBool(graph["index_integrity_valid"]) {
		t.Fatalf("selected index replay was not valid: %#v", graph)
	}
	if got := anyToInt(graph["replay_inspection_count"], -1); got != len(selected) {
		t.Fatalf("replay inspected unrelated project history: got=%d selected=%d project=%d", got, len(selected), len(store.objectiveProjectIndex["contextlattice"]))
	}
	rows, ok := graph["transitions"].([]objectiveTransition)
	if !ok || len(rows) != 3 || rows[0].TransitionID != "ot_selected_early" || rows[1].TransitionID != "ot_selected_mid" || rows[2].TransitionID != "ot_selected_late" {
		t.Fatalf("selected replay lost chronology or as_of filtering: %#v", graph)
	}
	nodes, ok := graph["nodes"].([]objectiveGraphNode)
	if !ok || len(nodes) != 1 || nodes[0].Status != "completed" {
		t.Fatalf("selected replay produced the wrong objective state: %#v", graph)
	}

	key := continuityScopedIndexKey("contextlattice", "selected")
	store.objectiveTransitionIndex[key] = append(store.objectiveTransitionIndex[key], len(store.objectiveTransitions)+7)
	invalid := store.objectiveGraph("contextlattice", "selected", base.Add(3*time.Hour), true, 100)
	if anyToBool(invalid["ok"]) || anyToBool(invalid["complete"]) || anyToBool(invalid["index_integrity_valid"]) ||
		anyToString(invalid["error"]) != "objective_transition_index_invalid" || anyToInt(invalid["invalid_index_count"], 0) != 1 {
		t.Fatalf("invalid selected index did not fail closed with evidence: %#v", invalid)
	}
}

func TestU8bSelectedHistoryTruncationIsExplicit(t *testing.T) {
	const rows = objectiveGraphMaxReplayInspections + 1
	store := &continuityStore{
		enabled:                  true,
		objectiveTransitions:     make([]objectiveTransition, 0, rows),
		objectiveTransitionIndex: map[string][]int{},
		objectiveRelationIndex:   map[string][]objectiveGraphRelationRef{},
	}
	key := continuityScopedIndexKey("contextlattice", "selected_dense")
	store.objectiveTransitionIndex[key] = make([]int, 0, rows)
	when := time.Date(2026, time.August, 9, 8, 0, 0, 0, time.UTC)
	for index := 0; index < rows; index++ {
		store.objectiveTransitions = append(store.objectiveTransitions, u8bObjectiveTransition(
			"selected_dense", fmt.Sprintf("ot_dense_%06d", index), "active", when, uint64(index+1),
		))
		store.objectiveTransitionIndex[key] = append(store.objectiveTransitionIndex[key], index)
	}
	graph := store.objectiveGraph("contextlattice", "selected_dense", when.Add(time.Hour), false, 1)
	if !anyToBool(graph["replay_truncated"]) || anyToBool(graph["transition_count_exact"]) ||
		anyToBool(graph["complete"]) || !anyToBool(graph["graph_truncated"]) ||
		anyToInt(graph["replay_inspection_count"], 0) != objectiveGraphMaxReplayInspections {
		t.Fatalf("selected history truncation was not explicit: %#v", graph)
	}
}

func TestU8bObjectiveSelectionIndexesFailClosedBeforeSelection(t *testing.T) {
	when := time.Date(2026, time.August, 9, 8, 0, 0, 0, time.UTC)

	t.Run("explicit objective with only invalid transition indexes", func(t *testing.T) {
		store := newU8bObjectiveStore()
		key := continuityScopedIndexKey("contextlattice", "missing_selected")
		store.objectiveTransitionIndex[key] = []int{7}
		graph := store.objectiveGraph("contextlattice", "missing_selected", when, true, 10)
		if anyToBool(graph["ok"]) || anyToString(graph["error"]) != "objective_transition_index_invalid" ||
			anyToInt(graph["invalid_index_count"], 0) != 1 || anyToBool(graph["index_integrity_valid"]) {
			t.Fatalf("invalid-only objective index did not fail closed: %#v", graph)
		}
	})

	t.Run("project selection with only invalid indexes", func(t *testing.T) {
		store := newU8bObjectiveStore()
		store.objectiveProjectIndex["contextlattice"] = []int{-1}
		graph := store.objectiveGraph("contextlattice", "", when, true, 10)
		if anyToBool(graph["ok"]) || anyToString(graph["error"]) != "objective_transition_index_invalid" ||
			anyToInt(graph["invalid_index_count"], 0) != 1 || anyToBool(graph["index_integrity_valid"]) {
			t.Fatalf("invalid-only project index did not fail closed: %#v", graph)
		}
	})

	t.Run("relation index cannot invent objective membership", func(t *testing.T) {
		store := newU8bObjectiveStore()
		transition := u8bObjectiveTransition("other", "ot_other", "active", when, 1)
		store.applyObjectiveTransitionValidatedLocked(transition)
		key := continuityScopedIndexKey("contextlattice", "missing_relation")
		store.objectiveRelationIndex[key] = []objectiveGraphRelationRef{{RelatedObjectiveID: "other", TransitionIndex: 0}}
		graph := store.objectiveGraph("contextlattice", "missing_relation", when, true, 10)
		if anyToBool(graph["ok"]) || anyToString(graph["error"]) != "objective_transition_index_invalid" ||
			anyToInt(graph["invalid_index_count"], 0) != 1 || anyToBool(graph["index_integrity_valid"]) {
			t.Fatalf("forged relation membership did not fail closed: %#v", graph)
		}
	})
}

func TestU8bProofSourceRevisionsAreAtomicAndMonotonic(t *testing.T) {
	continuity := newTestContinuityStore(t)
	continuityBefore := continuity.proofTimelineCurrentRevision()
	if _, err := continuity.recordObjectiveTransition(map[string]any{
		"project": "contextlattice", "objective_id": "u8b_revision", "objective": "revision proof",
		"transition_type": "created", "actor": "test",
	}); err != nil {
		t.Fatalf("record continuity revision: %v", err)
	}
	if got := continuity.proofTimelineCurrentRevision(); got <= continuityBefore {
		t.Fatalf("continuity revision was not monotonic: before=%d after=%d", continuityBefore, got)
	}

	claims := &temporalClaimStore{
		enabled: true, claims: map[string]temporalClaim{}, proofSessionIndex: map[string][]string{},
	}
	claim := temporalClaim{ClaimID: "claim_u8b", Project: "contextlattice", SessionID: "session_u8b", Revision: 1, UpdatedAt: nowUTCISO()}
	claims.mu.Lock()
	claims.setClaimLocked(claim)
	claim.Revision++
	claims.setClaimLocked(claim)
	claims.mu.Unlock()
	if got := claims.proofTimelineCurrentRevision(); got != 2 {
		t.Fatalf("temporal claim revision did not track both row mutations: %d", got)
	}

	quality := &contextPackQualityTelemetry{limit: 10, samples: []map[string]any{}, outcomes: []map[string]any{}, outcomeKeys: map[string]struct{}{}}
	quality.mu.Lock()
	quality.applyQualityEntryLocked(map[string]any{
		"sample_id": "sample_u8b", "session_id": "session_u8b", "project": "contextlattice", "quality_score": 90, "capturedAt": nowUTCISO(),
	})
	quality.applyOutcomeEntryLocked(map[string]any{
		"outcome_id": "outcome_u8b", "sample_id": "sample_u8b", "session_id": "session_u8b", "project": "contextlattice", "capturedAt": nowUTCISO(),
	})
	quality.mu.Unlock()
	if got := quality.proofTimelineCurrentRevision(); got != 2 {
		t.Fatalf("quality revision did not track sample and outcome mutations: %d", got)
	}

	impact := &tokenImpactTelemetry{limit: 10, samples: []map[string]any{}, exactArtifactKeys: map[string]string{}}
	impact.mu.Lock()
	accepted := impact.applyEntryLocked(map[string]any{
		"sample_id": "impact_u8b", "session_id": "session_u8b", "project": "contextlattice",
		"baseline_tokens_estimate": 100, "packed_tokens_estimate": 50, "capturedAt": nowUTCISO(),
	})
	impact.mu.Unlock()
	if !accepted || impact.proofTimelineCurrentRevision() != 1 {
		t.Fatalf("token impact revision did not track accepted mutation: accepted=%v revision=%d", accepted, impact.proofTimelineCurrentRevision())
	}

	scope := proofTimelineScope{SessionID: "session_u8b", Project: "contextlattice"}
	_, anchor, available, _ := claims.proofTimelineRowsWithRevision(scope)
	anchorRevision, ok := anchor["revision"].(uint64)
	if !available || !ok || anchorRevision != claims.proofTimelineCurrentRevision() {
		t.Fatalf("rows and revision were not captured atomically: anchor=%#v current=%d", anchor, claims.proofTimelineCurrentRevision())
	}
}

func TestU8bConcurrentProofMutationCannotReportStable(t *testing.T) {
	claims := &temporalClaimStore{
		enabled: true, claims: map[string]temporalClaim{}, proofSessionIndex: map[string][]string{},
	}
	claim := temporalClaim{
		ClaimID: "claim_concurrent", Project: "contextlattice", SessionID: "session_concurrent", Revision: 1,
		CreatedAt: nowUTCISO(), UpdatedAt: nowUTCISO(), ObservedAt: nowUTCISO(),
	}
	claims.mu.Lock()
	claims.setClaimLocked(claim)
	claims.mu.Unlock()
	scope := proofTimelineScope{SessionID: claim.SessionID, Project: claim.Project}
	rows, before, available, omitted := claims.proofTimelineRowsWithRevision(scope)
	if !available || omitted != 0 || len(rows) != 1 {
		t.Fatalf("initial proof snapshot unavailable: rows=%d available=%v omitted=%d", len(rows), available, omitted)
	}

	mutate := make(chan struct{})
	mutated := make(chan struct{})
	go func() {
		<-mutate
		claims.mu.Lock()
		claim.Revision++
		claim.UpdatedAt = time.Now().UTC().Add(time.Nanosecond).Format(time.RFC3339Nano)
		claims.setClaimLocked(claim)
		claims.mu.Unlock()
		close(mutated)
	}()
	close(mutate)
	<-mutated

	after := proofTimelineAnchorAtRevision(before, claims.proofTimelineCurrentRevision())
	snapshot := agentProofTimelineSnapshot{
		Session: map[string]any{"id": claim.SessionID, "project": claim.Project}, Claims: rows,
		Availability: map[string]bool{"temporal_claim": true}, SourceOmitted: map[string]int{"temporal_claim": 0},
		SourceAnchorsBefore: map[string]any{"temporal_claim": before},
		SourceAnchorsAfter:  map[string]any{"temporal_claim": after},
	}
	_, status, complete, _ := continuousCognitionProofProjectionFromSnapshot(snapshot)
	if status != "degraded" || complete || proofTimelineDigest(snapshot.SourceAnchorsBefore) == proofTimelineDigest(snapshot.SourceAnchorsAfter) {
		t.Fatalf("concurrent source mutation was falsely reported stable: status=%s complete=%v before=%#v after=%#v", status, complete, before, after)
	}
}

func BenchmarkU8bObjectiveReplayIgnoresUnrelatedProjectHistory(b *testing.B) {
	store := newU8bObjectiveStore()
	when := time.Date(2026, time.August, 9, 8, 0, 0, 0, time.UTC)
	for index := 0; index < 50000; index++ {
		store.applyObjectiveTransitionValidatedLocked(u8bObjectiveTransition(
			fmt.Sprintf("unrelated_bench_%05d", index), fmt.Sprintf("ot_unrelated_bench_%05d", index), "active", when, uint64(index+1),
		))
	}
	for index := 0; index < 4; index++ {
		store.applyObjectiveTransitionValidatedLocked(u8bObjectiveTransition(
			"selected_bench", fmt.Sprintf("ot_selected_bench_%02d", index), "active", when.Add(time.Duration(index)*time.Second), uint64(50001+index),
		))
	}
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		graph := store.objectiveGraph("contextlattice", "selected_bench", when.Add(time.Hour), false, 100)
		if anyToInt(graph["replay_inspection_count"], -1) != 4 || anyToInt(graph["transition_count"], -1) != 4 {
			b.Fatalf("selected replay scaled with unrelated history: %#v", graph)
		}
	}
}
