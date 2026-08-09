package main

import (
	"fmt"
	"testing"
	"time"
)

func TestU8aUtilityRowsAreBoundedAndDeterministicallyTruncated(t *testing.T) {
	telemetry := &utilityTelemetry{
		limit:              128,
		observations:       make([]map[string]any, 0, 128),
		byOutcome:          map[string]int{},
		byOpaqueControlRef: map[string]int{},
	}
	for _, sample := range []struct {
		id string
		at string
	}{
		{id: "outcome-3", at: "2026-08-09T00:03:00Z"},
		{id: "outcome-1", at: "2026-08-09T00:01:00Z"},
		{id: "outcome-4", at: "2026-08-09T00:04:00Z"},
		{id: "outcome-2", at: "2026-08-09T00:02:00Z"},
	} {
		telemetry.observations = append(telemetry.observations, map[string]any{
			"outcome_id": sample.id, "project": "u8a", "captured_at": sample.at,
		})
	}

	rows := telemetry.rows(utilityQuery{Project: "u8a", Limit: 2})
	if len(rows) != 2 {
		t.Fatalf("bounded utility selection returned %d rows, want 2", len(rows))
	}
	if got := anyToString(rows[0]["outcome_id"]); got != "outcome-3" {
		t.Fatalf("oldest retained row = %q, want outcome-3", got)
	}
	if got := anyToString(rows[1]["outcome_id"]); got != "outcome-4" {
		t.Fatalf("newest retained row = %q, want outcome-4", got)
	}
	untruncated := telemetry.rows(utilityQuery{Project: "u8a", Limit: 10})
	if got := anyToString(untruncated[0]["outcome_id"]); got != "outcome-3" {
		t.Fatalf("untruncated row order changed at index 0: %q", got)
	}
	if got := anyToString(untruncated[1]["outcome_id"]); got != "outcome-1" {
		t.Fatalf("untruncated row order changed at index 1: %q", got)
	}
	rows[0]["project"] = "mutated-copy"
	if got := anyToString(telemetry.observations[0]["project"]); got != "u8a" {
		t.Fatalf("utility row clone was not isolated: %q", got)
	}
}

func TestU8aSessionVisibilityRunsAfterSnapshotLockRelease(t *testing.T) {
	const sessionID = "session-u8a-lock"
	asOf := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	store := &agentSessionStore{
		idleTTL:  time.Hour,
		sessions: map[string]map[string]any{},
		events:   map[string][]map[string]any{},
	}
	store.sessions[sessionID] = map[string]any{
		"id": sessionID, "status": "active",
		"started_at":    asOf.Add(-time.Minute).Format(time.RFC3339Nano),
		"updated_at":    asOf.Add(-time.Second).Format(time.RFC3339Nano),
		"last_event_at": asOf.Add(-time.Second).Format(time.RFC3339Nano),
	}

	var visibleCalls int
	session, _, found, temporalComplete := continuousCognitionSessionAtVisible(
		store,
		sessionID,
		asOf,
		func(clone map[string]any) bool {
			visibleCalls++
			// A visibility predicate may consult another store surface. This
			// must not run while the session RLock is held.
			store.mu.Lock()
			store.mu.Unlock()
			return anyToString(clone["id"]) == sessionID
		},
	)
	if !found || !temporalComplete || len(session) == 0 {
		t.Fatalf("session snapshot = %#v, found=%v, temporal_complete=%v", session, found, temporalComplete)
	}
	if visibleCalls != 1 {
		t.Fatalf("visibility predicate calls = %d, want 1", visibleCalls)
	}
	if got := anyToString(session["status"]); got != "active" {
		t.Fatalf("effective status = %q, want active", got)
	}
}

func BenchmarkU8aUtilityRowsBounded(b *testing.B) {
	const observationCount = 50000
	telemetry := &utilityTelemetry{
		limit:              observationCount,
		observations:       make([]map[string]any, 0, observationCount),
		byOutcome:          map[string]int{},
		byOpaqueControlRef: map[string]int{},
	}
	for index := 0; index < observationCount; index++ {
		telemetry.observations = append(telemetry.observations, map[string]any{
			"outcome_id": fmt.Sprintf("outcome-%04d", index),
			"project":    "u8a",
			"captured_at": time.Date(2026, 8, 9, 0, 0, index,
				0, time.UTC).Format(time.RFC3339Nano),
		})
	}
	query := utilityQuery{Project: "u8a", Limit: 32}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = telemetry.rows(query)
	}
}
