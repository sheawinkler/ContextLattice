package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestCorrectionFeedbackSQLiteIndexScalesPastLegacyCeiling(t *testing.T) {
	s := correctionTestServer(t)
	mux := buildMux(s)
	oldest := correctionFeedbackTestRequest("First indexed feedback.")
	first := correctionTestMustPost(t, mux, "/tools/feedback_submit", oldest)
	history := s.feedbackStore.path
	indexPath := correctionFeedbackIndexPath(history)

	// Assemble a historical owner suffix, equivalent to durable typed writes
	// past a restored index checkpoint. Do not seed SQLite offset rows in the
	// test: recovery must derive them from the actual owner record format.
	file, err := os.OpenFile(history, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	buffer := bufio.NewWriter(file)
	encoder := json.NewEncoder(buffer)
	for i := 1; i < 8192; i++ {
		req := correctionFeedbackTestRequest(fmt.Sprintf("Historical correction %05d.", i))
		normalized, status, _ := normalizeFeedbackPayload(req, "agent")
		if status != 0 {
			t.Fatalf("invalid historical fixture at %d", i)
		}
		if err := encoder.Encode(s.buildFeedbackRecord(normalized)); err != nil {
			t.Fatal(err)
		}
	}
	if err := buffer.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	s.feedbackStore, err = newFeedbackStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	for {
		before := s.feedbackStore.correctionIndex.durableEnd
		status, replay := correctionTestPost(mux, "/tools/feedback_submit", oldest)
		if status == http.StatusOK && replay["idempotentReplay"] == true {
			break
		}
		if status != http.StatusServiceUnavailable || s.feedbackStore.correctionIndex.durableEnd <= before {
			t.Fatalf("bounded suffix recovery stopped progressing (status=%d)", status)
		}
	}

	db, identity, err := openCorrectionFeedbackDB(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count, pageSize int
	if err := db.QueryRow(`SELECT count(*) FROM correction_offsets`).Scan(&count); err != nil || count != 8192 {
		t.Fatalf("expected 8192 actual recovered offsets: count=%d err=%v", count, err)
	}
	if err := db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	var synchronous int
	if err := db.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil || synchronous != 2 {
		t.Fatalf("expected SQLite FULL synchronous mode: %d (%v)", synchronous, err)
	}
	var planID, parent, unused int
	var plan string
	if err := db.QueryRow(`EXPLAIN QUERY PLAN `+correctionFeedbackOffsetQuery, oldest["idempotencyKey"]).
		Scan(&planID, &parent, &unused, &plan); err != nil || !strings.Contains(plan, "USING PRIMARY KEY") {
		t.Fatalf("offset reads do not use the primary key: %q (%v)", plan, err)
	}
	// Keep this connection open so commits leave observable WAL frames instead
	// of SQLite checkpointing automatically on the last connection close.
	frames := func() int64 {
		t.Helper()
		info, err := os.Stat(indexPath + "-wal")
		if os.IsNotExist(err) || err == nil && info.Size() == 0 {
			return 0
		}
		if err != nil || info.Size() < 32 || (info.Size()-32)%int64(pageSize+24) != 0 {
			t.Fatalf("invalid WAL accounting: %v", err)
		}
		return (info.Size() - 32) / int64(pageSize+24)
	}
	touched := make(map[string]int64)
	ledgerReads := make(map[string]int)
	s.feedbackStore.correctionIndex.observeIndex = func(phase string, n int64) { touched[phase] += n }
	s.feedbackStore.correctionIndex.observeRead = func(phase string, n int) { ledgerReads[phase] += n }
	startFrames := frames()
	replayed := correctionTestMustPost(t, mux, "/tools/feedback_submit", oldest)
	if anyMap(replayed["feedback"])["id"] != anyMap(first["feedback"])["id"] ||
		touched["frontier_read"] != 1 || touched["offset_read"] != 1 ||
		touched["offset_write"] != 0 || frames() != startFrames ||
		ledgerReads["catchup"] != 0 || ledgerReads["witness"] > correctionFeedbackWitnessBytes ||
		ledgerReads["indexed"] <= 0 || ledgerReads["indexed"] > 1024 {
		t.Fatalf("old receipt lookup was not bounded and read-only: rows=%v ledger bytes=%v", touched, ledgerReads)
	}
	newest := correctionFeedbackTestRequest("Correction past the former global ceiling.")
	for _, request := range []map[string]any{newest, correctionFeedbackTestRequest("Another correction past the former ceiling.")} {
		clear(touched)
		clear(ledgerReads)
		before := frames()
		out := correctionTestMustPost(t, mux, "/tools/feedback_submit", request)
		written := frames() - before
		if out["idempotentReplay"] != false || touched["frontier_read"] != 1 ||
			touched["frontier_write"] != 1 || touched["offset_write"] != 1 ||
			touched["offset_read"] != 0 || ledgerReads["catchup"] != 0 ||
			ledgerReads["indexed"] != 0 || ledgerReads["witness"] > correctionFeedbackWitnessBytes ||
			written <= 0 || written > 16 {
			t.Fatalf("append was not bounded: rows=%v ledger bytes=%v WAL frames=%d", touched, ledgerReads, written)
		}
		t.Logf("new operation: one offset row plus one frontier row, %d WAL pages; oldest lookup: %s", written, plan)
	}
	if err := db.QueryRow(`SELECT count(*) FROM correction_offsets`).Scan(&count); err != nil || count != 8194 {
		t.Fatalf("typed operations beyond the retired ceiling were not saved: count=%d err=%v", count, err)
	}
	if now, err := os.Stat(indexPath); err != nil || !os.SameFile(identity, now) {
		t.Fatal("append replaced the SQLite index file")
	}
	s.feedbackStore, err = newFeedbackStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []map[string]any{oldest, newest} {
		out := correctionTestMustPost(t, mux, "/tools/feedback_submit", request)
		if out["idempotentReplay"] != true {
			t.Fatal("old or post-ceiling receipt was lost after reopen")
		}
	}
}

func TestCorrectionFeedbackSQLiteDoesNotPublishPartialCatchup(t *testing.T) {
	s := correctionTestServer(t)
	mux := buildMux(s)
	oldest := correctionFeedbackTestRequest("Original feedback in the checkpoint.")
	correctionTestMustPost(t, mux, "/tools/feedback_submit", oldest)
	history := s.feedbackStore.path
	original, err := os.ReadFile(history)
	if err != nil {
		t.Fatal(err)
	}
	initial := s.feedbackStore.correctionIndex.durableEnd
	pending := correctionFeedbackTestRequest("Valid new offset before a duplicate.")
	normalized, status, _ := normalizeFeedbackPayload(pending, "agent")
	if status != 0 {
		t.Fatal("invalid fixture")
	}
	line, err := json.Marshal(s.buildFeedbackRecord(normalized))
	if err != nil {
		t.Fatal(err)
	}
	suffix := append(append(line, '\n'), original...)
	expected := append(append([]byte{}, original...), suffix...)
	if err := os.WriteFile(history, expected, 0600); err != nil {
		t.Fatal(err)
	}
	s.feedbackStore, err = newFeedbackStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if status, out := correctionTestPost(mux, "/tools/feedback_submit", pending); status != http.StatusServiceUnavailable || out["feedback"] != nil {
		t.Fatalf("duplicate suffix unexpectedly received an acknowledgment: %d", status)
	}
	db, _, err := openCorrectionFeedbackDB(correctionFeedbackIndexPath(history))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	durable, err := readCorrectionFeedbackFrontier(db)
	if err != nil || durable.Frontier != initial {
		t.Fatalf("rejected page advanced the durable frontier: %+v (%v)", durable, err)
	}
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM correction_offsets`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rejected page published a partial offset batch: %d (%v)", rows, err)
	}
	if after, err := os.ReadFile(history); err != nil || !bytes.Equal(after, expected) {
		t.Fatal("failed recovery modified authoritative rows")
	}
}
