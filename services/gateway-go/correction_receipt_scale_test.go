package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
)

func receiptTestRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func receiptTestStable(t *testing.T, path string, before []byte) {
	t.Helper()
	if !bytes.Equal(receiptTestRead(t, path), before) {
		t.Fatalf("replay changed %s", path)
	}
}

func receiptTestPointWrites(t *testing.T, path string, before []byte, kind, operation string, want int) {
	t.Helper()
	after := receiptTestRead(t, path)
	pageSize := int(binary.BigEndian.Uint16(after[16:18]))
	if pageSize == 1 {
		pageSize = 65536
	}
	changed := 0
	for i := 0; i < len(after); i += pageSize {
		if i+pageSize > len(before) || !bytes.Equal(before[i:i+pageSize], after[i:i+pageSize]) {
			changed++
		}
	}
	// Includes file header, pending page, receipt page(s) and unique indexes.
	// Existing rows are not repacked on insert, even at a B-tree split.
	if changed < 1 || changed > 24 || len(before) <= 24*pageSize {
		t.Fatalf("index touched %d pages out of %d existing", changed, len(before)/pageSize)
	}
	db, _, err := openCorrectionReceiptDB(strings.TrimSuffix(path, ".correction-receipts.v1.sqlite"), kind, "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM receipts").Scan(&count); err != nil || count != want {
		t.Fatalf("want %d stored receipts; got %d, %v", want, count, err)
	}
	for _, query := range []string{correctionReceiptLookup, "SELECT 1 FROM receipts WHERE alternate = ?"} {
		var id, parent, extra int
		var detail string
		if err := db.QueryRow("EXPLAIN QUERY PLAN "+query, operation).Scan(&id, &parent, &extra, &detail); err != nil || !strings.Contains(detail, "SEARCH receipts USING") {
			t.Fatalf("receipt point lookup is not indexed: %q, %v", detail, err)
		}
	}
	t.Logf("%s: %d existing SQLite pages; %d pages changed for one new receipt", kind, len(before)/pageSize, changed)
}

func receiptClaimPayload(req correctionRequest, note, claimID string) map[string]any {
	return map[string]any{
		"claim_id": claimID, "project": correctionTestProject, "subject": "release", "predicate": "version", "object": "new",
		"statement": note, "topic_path": "claims/corrections", "agent_id": correctionTestAgent, "session_id": correctionTestSession,
		"status": "active", "verification": map[string]any{"status": "unverified", "method": "reporter_assertion"},
		"provenance": map[string]any{"source": "correction_reporter_assertion", "source_id": req.target, "run_id": req.id},
	}
}

func receiptClaimFixture(t *testing.T, req correctionRequest, note, claimID string) correctionClaimRecord {
	t.Helper()
	payload := receiptClaimPayload(req, note, claimID)
	claim, err := normalizeTemporalClaim(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	claim.CreatedAt, claim.UpdatedAt = "2020-01-01T00:00:00Z", "2020-01-01T00:00:00Z"
	return correctionClaimRecord{ID: req.id, Fingerprint: sha256Hex(string(raw)), FeedbackID: req.feedbackID, Category: req.category, Claim: claim, Apply: true}
}

func TestCorrectionRecordingClaimReceiptsScalePast4096AndCompact(t *testing.T) {
	t.Setenv("CONTEXTLATTICE_TEMPORAL_CLAIMS_MAX", "128")
	s := correctionTestServer(t)
	mux := buildMux(s)
	old := correctionTestFeedback(t, mux, "useful", "Historical correction 0.", "", true)
	path := s.temporalClaims.path
	history, err := openOwnerOnlyAppend(path, false)
	if err != nil {
		t.Fatal(err)
	}
	legacyEncoder := json.NewEncoder(history)
	const historical = 4097
	for i := 0; i < historical; i++ {
		note := fmt.Sprintf("Historical correction %d.", i)
		req := correctionRequest{id: correctionOperationID(correctionTestProject, correctionTestSession, correctionTestAgent, "useful", true, "", note), category: "useful", feedbackID: fmt.Sprintf("feedback-historical-%d", i)}
		if i == 0 {
			req.feedbackID = anyToString(old["feedback_id"])
		}
		row := receiptClaimFixture(t, req, note, fmt.Sprintf("claim-historical-%d", i))
		if i == 0 {
			row.Apply = false // v1 compacted proof for an already trimmed claim.
		}
		if err := legacyEncoder.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(history.Sync(), history.Close()); err != nil {
		t.Fatal(err)
	}
	s.temporalClaims, err = newTemporalClaimStoreFromEnv()
	if err != nil || s.temporalClaims.correctionUncertain {
		t.Fatalf("legacy claim receipt migration: %v", err)
	}
	oldClaim := correctionTestClaim(old)
	oldClaim["claim_id"] = "claim-historical-0"
	oldResult := correctionTestMustPost(t, mux, "/memory/claims", oldClaim)
	if oldResult["idempotent_replay"] != true || anyMap(oldResult["claim"])["created_at"] != "2020-01-01T00:00:00Z" {
		t.Fatal("old compacted proof did not replay its exact original claim")
	}
	index := correctionReceiptDBPath(path)
	before := receiptTestRead(t, index)
	newReq := correctionTestFeedback(t, mux, "useful", "Post-ceiling factual claim.", "", true)
	newClaim := correctionTestClaim(newReq)
	newClaim["claim_id"] = "claim-post-ceiling"
	newResult := correctionTestMustPost(t, mux, "/memory/claims", newClaim)
	receiptTestPointWrites(t, index, before, correctionClaimReceiptKind, anyToString(newReq["operation_id"]), historical+1)
	if newResult["idempotent_replay"] != false {
		t.Fatal("new correction did not create its first factual receipt")
	}
	duplicateID := correctionTestFeedback(t, mux, "useful", "A distinct operation reusing a historical claim ID.", "", true)
	conflict := correctionTestClaim(duplicateID)
	conflict["claim_id"] = oldClaim["claim_id"]
	if code, _ := correctionTestPost(mux, "/memory/claims", conflict); code != http.StatusConflict {
		t.Fatalf("old, trimmed claim ID was not reserved: %d", code)
	}
	s.temporalClaims.mu.Lock()
	err = s.temporalClaims.compactLocked()
	s.temporalClaims.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	ledger, indexBytes := receiptTestRead(t, path), receiptTestRead(t, index)
	if bytes.Count(ledger, []byte{10}) > 129 || bytes.Contains(ledger, []byte(correctionClaimRecordPrefix)) {
		t.Fatal("claim compaction recopied historical proof rows")
	}
	if s.temporalClaims, err = newTemporalClaimStoreFromEnv(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ payload, original map[string]any }{{oldClaim, oldResult}, {newClaim, newResult}} {
		replayed := correctionTestMustPost(t, mux, "/memory/claims", tc.payload)
		if replayed["idempotent_replay"] != true || !reflect.DeepEqual(replayed["claim"], tc.original["claim"]) {
			t.Fatal("reopen/compaction changed a factual claim receipt")
		}
	}
	receiptTestStable(t, path, ledger)
	receiptTestStable(t, index, indexBytes)
	if err := os.Rename(index, index+".held"); err != nil {
		t.Fatal(err)
	}
	if s.temporalClaims, err = newTemporalClaimStoreFromEnv(); err != nil {
		t.Fatal(err)
	}
	if code, _ := correctionTestPost(mux, "/memory/claims", oldClaim); code != http.StatusServiceUnavailable {
		t.Fatalf("missing compacted receipt owner was treated as absence: %d", code)
	}
	receiptTestStable(t, path, ledger)
}

func TestCorrectionRecordingSessionReceiptsScalePast128(t *testing.T) {
	t.Setenv("GO_AGENT_SESSION_MAX_EVENTS_PER_SESSION", "16")
	s := correctionTestServer(t)
	mux := buildMux(s)
	path := s.agentSessions.path
	index := correctionReceiptDBPath(path)
	var first, last map[string]any
	var before []byte
	const events = 130
	for i := 0; i < events; i++ {
		factual := i == 0 || i == events-1
		req := correctionTestFeedback(t, mux, "useful", fmt.Sprintf("Same-session correction %d.", i), "", factual)
		if factual {
			payload := correctionTestClaim(req)
			payload["claim_id"] = fmt.Sprintf("claim-event-%d", i)
			correctionTestMustPost(t, mux, "/memory/claims", payload)
		}
		if i == events-1 {
			before = receiptTestRead(t, index)
		}
		result := correctionTestMustPost(t, mux, "/v1/agents/sessions/event", correctionTestEvent(req))
		if i == 0 {
			first = map[string]any{"request": correctionTestEvent(req), "event": result["event"]}
		}
		if i == events-1 {
			last = map[string]any{"request": correctionTestEvent(req), "event": result["event"]}
			receiptTestPointWrites(t, index, before, correctionSessionReceiptKind, anyToString(req["operation_id"]), events)
		}
	}
	row, ring, _ := s.agentSessions.get(correctionTestSession)
	count := anyToInt(row["event_count"], 0)
	firstID := anyMap(first["event"])["id"]
	for _, event := range ring {
		if event["id"] == firstID {
			t.Fatal("old receipt did not leave the bounded visible event window")
		}
	}
	snapshot, indexBytes := receiptTestRead(t, path), receiptTestRead(t, index)
	if len(ring) != 16 || bytes.Contains(snapshot, []byte(`"correction_receipts"`)) || bytes.Contains(snapshot, []byte(anyToString(firstID))) {
		t.Fatal("snapshot retained a growing receipt history")
	}
	var err error
	if s.agentSessions, err = newAgentSessionStoreFromEnv(); err != nil {
		t.Fatal(err)
	}
	for _, one := range []map[string]any{first, last} {
		result := correctionTestMustPost(t, mux, "/v1/agents/sessions/event", anyMap(one["request"]))
		got := anyMap(result["event"])
		if got["idempotent_replay"] != true || anyToInt(anyMap(result["session"])["event_count"], 0) != count {
			t.Fatal("event replay was not exact or changed session accounting")
		}
		delete(got, "idempotent_replay")
		if !reflect.DeepEqual(got, one["event"]) {
			t.Fatal("event replay changed the original event")
		}
	}
	receiptTestStable(t, path, snapshot)
	receiptTestStable(t, index, indexBytes)
	if err := os.Rename(index, index+".held"); err != nil {
		t.Fatal(err)
	}
	if s.agentSessions, err = newAgentSessionStoreFromEnv(); err != nil {
		t.Fatal(err)
	}
	if code, _ := correctionTestPost(mux, "/v1/agents/sessions/event", anyMap(first["request"])); code != http.StatusServiceUnavailable {
		t.Fatalf("missing event receipt owner was treated as an empty DB: %d", code)
	}
	receiptTestStable(t, path, snapshot)
}

func TestCorrectionRecordingClaimReservationReopen(t *testing.T) {
	s := correctionTestServer(t)
	mux := buildMux(s)
	for _, persisted := range []bool{false, true} {
		note := fmt.Sprintf("Crash control, ledger reached: %v.", persisted)
		reqMap := correctionTestFeedback(t, mux, "useful", note, "", true)
		req, err := parseCorrectionRequest(reqMap)
		if err != nil {
			t.Fatal(err)
		}
		claimID := fmt.Sprintf("claim-pending-%v", persisted)
		record := receiptClaimFixture(t, req, note, claimID)
		proof, err := record.receipt()
		if err != nil {
			t.Fatal(err)
		}
		owner := s.temporalClaims
		db, token, err := openCorrectionReceiptDB(owner.path, correctionClaimReceiptKind, owner.correctionStamp.Owner)
		if err != nil {
			t.Fatal(err)
		}
		if err := createOwnerOnlyDurableEmptyFileIfMissing(owner.path, false); err != nil {
			t.Fatal(err)
		}
		tip := owner.correctionStamp
		tip.Owner = token
		stamp, err := reserveCorrectionReceipt(db, tip, proof)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if persisted {
			record.Receipt = &stamp
			line, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			file, err := openOwnerOnlyAppend(owner.path, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := owner.appendCorrectionClaimLocked(file, append(line, 10)); err != nil {
				t.Fatal(err)
			}
		}
		original := receiptTestRead(t, owner.path)
		reopened, err := newTemporalClaimStoreFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		s.temporalClaims = reopened
		payload := correctionTestClaim(reqMap)
		payload["claim_id"] = claimID
		result := correctionTestMustPost(t, mux, "/memory/claims", payload)
		if result["idempotent_replay"] != persisted || anyToInt(anyMap(result["claim"])["revision"], 0) != 1 {
			t.Fatal("restart did not distinguish a durable append from a reservation")
		}
		if persisted {
			receiptTestStable(t, owner.path, original)
		} else if raw := receiptTestRead(t, owner.path); bytes.Count(raw[len(original):], []byte{10}) != 1 {
			t.Fatal("retry appended more than the single absent claim")
		}
	}
}

func TestCorrectionRecordingSessionMigratesInlineProofAndRejectsReusedSessionID(t *testing.T) {
	s := correctionTestServer(t)
	mux := buildMux(s)
	req := correctionTestFeedback(t, mux, "useful", "Historical inline event.", "", false)
	operation := anyToString(req["operation_id"])
	owner := s.agentSessions
	owner.mu.Lock()
	eventPayload := map[string]any{
		"id": operation, "session_id": correctionTestSession, "project": correctionTestProject,
		"agent_id": correctionTestAgent, "agent": "codex", "type": "correction.recorded",
		"summary": "Correction recorded: useful", "created_at": "2020-01-01T00:00:00Z",
		"metadata": map[string]any{
			"operation_id": operation, "category": "useful", "factual": false, "target_claim_id": "",
			"feedback_saved": true, "feedback_id": req["feedback_id"], "claim_persisted": false, "correction_status": "feedback_only",
		},
	}
	owner.sessions[correctionTestSession]["correction_receipts"] = map[string]any{operation: normalizeAgentSessionEvent(correctionTestSession, eventPayload)}
	_, historical, err := owner.appendEventScopedLocked(correctionTestSession, eventPayload, nil)
	owner.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := receiptTestRead(t, owner.path)
	for i := 0; i < 2; i++ { // Repeat migration before the snapshot carries its first stamp.
		s.agentSessions, err = newAgentSessionStoreFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		result := correctionTestMustPost(t, mux, "/v1/agents/sessions/event", correctionTestEvent(req))
		got := anyMap(result["event"])
		if got["idempotent_replay"] != true {
			t.Fatal("legacy inline event was not recognized as replay")
		}
		delete(got, "idempotent_replay")
		want, _ := json.Marshal(historical)
		if actual, _ := json.Marshal(got); !bytes.Equal(actual, want) {
			t.Fatal("migrated inline event was not exact")
		}
		receiptTestStable(t, owner.path, snapshot)
	}
	s.agentSessions.maxKeep = 1
	for _, id := range []string{"sess-new-generation", correctionTestSession} {
		correctionTestMustPost(t, mux, "/v1/agents/sessions/start", map[string]any{
			"session_id": id, "project": correctionTestProject, "agent": "codex", "agent_id": correctionTestAgent,
		})
	}
	if _, _, found := s.agentSessions.get(correctionTestSession); !found {
		t.Fatal("control did not recreate the old session ID")
	}
	updated, stored := receiptTestRead(t, owner.path), receiptTestRead(t, correctionReceiptDBPath(owner.path))
	if code, _ := correctionTestPost(mux, "/v1/agents/sessions/event", correctionTestEvent(req)); code != http.StatusConflict {
		t.Fatalf("old proof escaped original session generation: %d", code)
	}
	receiptTestStable(t, owner.path, updated)
	receiptTestStable(t, correctionReceiptDBPath(owner.path), stored)
}

func TestCorrectionRecordingSessionRetriesBeforeSnapshotReplacement(t *testing.T) {
	s := correctionTestServer(t)
	mux := buildMux(s)
	owner := s.agentSessions
	original := receiptTestRead(t, owner.path)
	req := correctionTestEvent(correctionTestFeedback(t, mux, "useful", "Retry before replacing snapshot.", "", false))
	row, _, _ := owner.get(correctionTestSession)
	count := anyToInt(row["event_count"], 0)
	hit := false
	owner.decisionWriteHook = func(phase string) error {
		if phase == "after_temp_close" {
			hit = true
			return errors.New("injected interruption before replacing owner snapshot")
		}
		return nil
	}
	if code, _ := correctionTestPost(mux, "/v1/agents/sessions/event", req); code != http.StatusServiceUnavailable || !hit {
		t.Fatalf("pre-replacement failure was not exercised: %d", code)
	}
	receiptTestStable(t, owner.path, original)
	var err error
	if s.agentSessions, err = newAgentSessionStoreFromEnv(); err != nil {
		t.Fatal(err)
	}
	result := correctionTestMustPost(t, mux, "/v1/agents/sessions/event", req)
	if anyMap(result["event"])["idempotent_replay"] == true || anyToInt(anyMap(result["session"])["event_count"], 0) != count+1 {
		t.Fatal("pre-replacement failure consumed or duplicated the event")
	}
}

func TestCorrectionRecordingClaimCleanRejectionDoesNotFenceReopen(t *testing.T) {
	s := correctionTestServer(t)
	mux := buildMux(s)
	bad := correctionTestClaim(correctionTestFeedback(t, mux, "wrong", "Target is absent.", "claim-missing", true))
	if code, _ := correctionTestPost(mux, "/memory/claims", bad); code != http.StatusConflict {
		t.Fatalf("expected clean rejection of absent target, got %d", code)
	}
	reopened, err := newTemporalClaimStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	s.temporalClaims = reopened
	good := correctionTestClaim(correctionTestFeedback(t, mux, "useful", "Valid after reopening an empty owner.", "", true))
	correctionTestMustPost(t, mux, "/memory/claims", good)
}

func TestCorrectionRecordingUncertainMigrationDoesNotCompactAwayProof(t *testing.T) {
	s := correctionTestServer(t)
	path := s.temporalClaims.path
	note := "Valid typed proof followed by damaged old ledger material."
	req := correctionRequest{id: correctionOperationID(correctionTestProject, correctionTestSession, correctionTestAgent, "useful", true, "", note), category: "useful", feedbackID: "feedback-legacy"}
	record := receiptClaimFixture(t, req, note, "claim-preserve-migration")
	row, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	original := append(append(row, 10), "unparseable legacy row\n"...)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := newTemporalClaimStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.compactLocked(); !errors.Is(err, errCorrectionUnavailable) {
		t.Fatalf("unverified migration permitted compaction: %v", err)
	}
	receiptTestStable(t, path, original)
}

func TestCorrectionRecordingReceiptInventoryFollowsOwners(t *testing.T) {
	s := correctionTestServer(t)
	for id, path := range map[string]string{"agent_sessions": s.agentSessions.path, "temporal_claims": s.temporalClaims.path} {
		owner := gatewayStateInventoryEntryByID(t, id)
		receipts := gatewayStateInventoryEntryByID(t, id+"_correction_receipts")
		if owner.Path != path || receipts.Path != correctionReceiptDBPath(path) ||
			receipts.SourceEnv != owner.SourceEnv || receipts.PersistenceClass != "owner_only_durable_sqlite" {
			t.Fatalf("correction receipt store left its owner's storage tier: %+v", receipts)
		}
	}
}
