package main

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func correctionFeedbackTestRequest(note string) map[string]any {
	id := correctionOperationID(correctionTestProject, correctionTestSession, correctionTestAgent, "useful", false, "", note)
	return map[string]any{
		"project": correctionTestProject, "source": "agent", "content": note, "idempotencyKey": id, "task_id": "original-task",
		"metadata": map[string]any{
			"schema_id": correctionFeedbackSchema, "category": "useful", "factual": false,
			"session_id": correctionTestSession, "agent_id": correctionTestAgent, "target_claim_id": "", "operation_id": id,
		},
	}
}

func TestCorrectionFeedbackOwnerReplaysAfterEvictionAndReopen(t *testing.T) {
	t.Setenv("GO_FEEDBACK_MAX_IN_MEMORY", "500")
	s := correctionTestServer(t)
	mux := buildMux(s)
	body := correctionFeedbackTestRequest("A useful assertion.")
	const calls = 8
	var wg sync.WaitGroup
	responses := make(chan map[string]any, calls)
	for range calls {
		wg.Go(func() {
			code, out := correctionTestPost(mux, "/tools/feedback_submit", body)
			if code != http.StatusOK || out["ok"] != true {
				t.Errorf("feedback submit failed: status=%d", code)
				return
			}
			responses <- out
		})
	}
	wg.Wait()
	close(responses)
	originalID := ""
	created := 0
	for out := range responses {
		id := anyToString(anyMap(out["feedback"])["id"])
		if out["idempotentReplay"] == false {
			created++
		}
		if out["recorded"] != true || out["idempotencyScope"] != "correction_ledger" ||
			out["correction_operation_id"] != body["idempotencyKey"] || id == "" || (originalID != "" && originalID != id) {
			t.Fatal("concurrent retry lost original feedback identity or scope")
		}
		originalID = id
	}
	if created != 1 || len(s.feedbackStore.records) != 1 || len(s.feedbackStore.idempotency) != 0 {
		t.Fatal("concurrent correction feedback used the request cache or duplicated feedback")
	}

	// Use the ordinary owner append path to evict the typed row from the
	// retained preference window without deleting its authoritative bytes.
	for range s.feedbackStore.maxKeep {
		correctionTestMustPost(t, mux, "/tools/feedback_submit", map[string]any{
			"project": correctionTestProject, "source": "user", "content": "ordinary feedback", "include_preferences": false,
		})
	}
	if len(s.feedbackStore.list(correctionTestProject, "", "agent", 1)) != 0 {
		t.Fatal("fixture did not evict the typed row from memory")
	}
	path := s.feedbackStore.path
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := newFeedbackStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	s.feedbackStore = reopened
	if len(reopened.list(correctionTestProject, "", "agent", 1)) != 0 {
		t.Fatal("fixture did not evict the typed row from the reopened window")
	}
	out := correctionTestMustPost(t, mux, "/tools/feedback_submit", body)
	if out["idempotentReplay"] != true || anyMap(out["feedback"])["id"] != originalID {
		t.Fatal("disk replay returned a new feedback identity")
	}
	event := correctionTestEvent(map[string]any{
		"operation_id": body["idempotencyKey"], "category": "useful", "factual": false, "target_claim_id": "", "feedback_id": originalID,
	})
	receipt := correctionTestMustPost(t, mux, "/v1/agents/sessions/event", event)
	if anyMap(anyMap(receipt["event"])["metadata"])["feedback_id"] != originalID {
		t.Fatal("event owner did not bind the aged-out feedback receipt")
	}

	for key, value := range map[string]string{"task_id": "changed-task", "content": "changed content"} {
		changed := cloneJSONMap(body)
		changed[key] = value
		if code, rejected := correctionTestPost(mux, "/tools/feedback_submit", changed); code != http.StatusConflict || rejected["feedback"] != nil {
			t.Fatalf("changed %s did not conflict without disclosure: status=%d", key, code)
		}
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
		t.Fatal("exact or conflicting retries changed the authoritative feedback file")
	}

	legacy := map[string]any{
		"project": correctionTestProject, "source": "agent", "content": "legacy correction",
		"idempotencyKey": "legacy-correction-key", "metadata": map[string]any{"schema_id": correctionFeedbackSchema},
	}
	first := correctionTestMustPost(t, mux, "/tools/feedback_submit", legacy)
	replay := correctionTestMustPost(t, mux, "/tools/feedback_submit", legacy)
	if replay["idempotencyScope"] != "request" || replay["idempotentReplay"] != true ||
		anyMap(replay["feedback"])["id"] != anyMap(first["feedback"])["id"] {
		t.Fatal("untyped legacy feedback no longer uses the existing request-cache behavior")
	}
	if code, _ := correctionTestPost(mux, "/feedback", body); code != http.StatusUnprocessableEntity {
		t.Fatalf("untyped endpoint accepted reserved correction receipt metadata: status=%d", code)
	}
}

func TestCorrectionFeedbackOwnerFailsClosedOnUnavailableOrAmbiguousLedger(t *testing.T) {
	for _, mode := range []string{"missing", "replaced", "truncated", "duplicate_after_reopen", "malformed_after_reopen", "torn_after_reopen", "lost_index_after_reopen", "corrupt_index_after_reopen"} {
		t.Run(mode, func(t *testing.T) {
			s := correctionTestServer(t)
			mux := buildMux(s)
			body := correctionFeedbackTestRequest("Confirm the owner receipt.")
			correctionTestMustPost(t, mux, "/tools/feedback_submit", body)
			ownerPath := s.feedbackStore.path
			raw, err := os.ReadFile(ownerPath)
			if err != nil {
				t.Fatal(err)
			}
			var expected []byte
			switch mode {
			case "missing", "replaced":
				if err := os.Rename(ownerPath, filepath.Join(filepath.Dir(ownerPath), "preserved.ndjson")); err != nil {
					t.Fatal(err)
				}
				if mode == "replaced" {
					expected = raw
					err = os.WriteFile(ownerPath, expected, 0600)
				}
			case "truncated":
				expected = []byte{}
				err = os.Truncate(ownerPath, 0)
			case "duplicate_after_reopen", "malformed_after_reopen", "torn_after_reopen":
				extra := raw
				if mode == "malformed_after_reopen" {
					extra = []byte("unproved\n")
				} else if mode == "torn_after_reopen" {
					extra = raw[:len(raw)-1]
				}
				expected = append(append([]byte{}, raw...), extra...)
				err = os.WriteFile(ownerPath, expected, 0600)
				if err == nil {
					s.feedbackStore, err = newFeedbackStoreFromEnv()
				}
			case "lost_index_after_reopen":
				expected = raw
				err = os.Remove(correctionFeedbackIndexPath(ownerPath))
				if err == nil {
					s.feedbackStore, err = newFeedbackStoreFromEnv()
				}
			case "corrupt_index_after_reopen":
				expected = raw
				err = os.WriteFile(correctionFeedbackIndexPath(ownerPath), []byte(`{"checkpoint":{"records":{}}}`), 0600)
				if err == nil {
					s.feedbackStore, err = newFeedbackStoreFromEnv()
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			code, out := correctionTestPost(mux, "/tools/feedback_submit", body)
			if code != http.StatusServiceUnavailable || out["ok"] != false || out["feedback"] != nil {
				t.Fatalf("uncertain ledger returned an acknowledgment: status=%d", code)
			}
			if mode == "torn_after_reopen" {
				if code, _ := correctionTestPost(mux, "/tools/feedback_submit", map[string]any{"content": "ordinary feedback"}); code != http.StatusBadGateway {
					t.Fatalf("ordinary append was allowed to obscure a torn row: %d", code)
				}
			}
			after, err := os.ReadFile(ownerPath)
			if mode == "missing" {
				if !os.IsNotExist(err) {
					t.Fatal("retry recreated the missing ledger")
				}
			} else if err != nil || !bytes.Equal(after, expected) {
				t.Fatal("failed-closed retry changed the ledger")
			}
		})
	}
}

func TestCorrectionFeedbackOwnerDoesNotActivateOverUnindexedTypedHistory(t *testing.T) {
	s := correctionTestServer(t)
	mux := buildMux(s)
	body := correctionFeedbackTestRequest("Already persisted by the previous feedback writer.")
	normalized, status, message := normalizeFeedbackPayload(body, "agent")
	if status != 0 {
		t.Fatalf("fixture normalization failed: %d %s", status, message)
	}
	prior := s.buildFeedbackRecord(normalized)
	if err := s.feedbackStore.append(prior); err != nil {
		t.Fatal(err)
	}
	path := s.feedbackStore.path
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s.feedbackStore, err = newFeedbackStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if status, out := correctionTestPost(mux, "/tools/feedback_submit", body); status != http.StatusServiceUnavailable || out["feedback"] != nil {
		t.Fatal("unindexed typed row was treated as part of the untyped baseline")
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
		t.Fatal("pre-release epoch refusal rewrote the owner ledger")
	}
}

func appendCorrectionFeedbackLegacyFixture(t *testing.T, path string, minBytes int) int64 {
	t.Helper()
	line := []byte(`{"id":"fixture","project":"alpha","source":"user","content":"` + strings.Repeat("x", 1800) + "\"}\n")
	block := bytes.Repeat(line, 32)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	var written int
	for written < minBytes {
		n, err := file.Write(block)
		written += n
		if err != nil || n != len(block) {
			_ = file.Close()
			t.Fatalf("write synthetic history: %v", err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return int64(written)
}

func TestCorrectionFeedbackOwnerWorkIndependentOfUntypedHistory(t *testing.T) {
	t.Setenv("GO_FEEDBACK_MAX_IN_MEMORY", "500")
	for _, test := range []struct {
		name string
		size int
	}{{"small", 64 << 10}, {"above_old_limit", 65 << 20}} {
		t.Run(test.name, func(t *testing.T) {
			s := correctionTestServer(t)
			mux := buildMux(s)
			history := s.feedbackStore.path
			epoch := appendCorrectionFeedbackLegacyFixture(t, history, test.size)
			var err error
			s.feedbackStore, err = newFeedbackStoreFromEnv()
			if err != nil {
				t.Fatal(err)
			}
			reads := map[string]int{}
			observe := func(phase string, n int) { reads[phase] += n }
			s.feedbackStore.correctionIndex.observeRead = observe
			body := correctionFeedbackTestRequest("Proof-carrying feedback.")
			created := correctionTestMustPost(t, mux, "/tools/feedback_submit", body)
			if created["idempotentReplay"] != false || s.feedbackStore.correctionIndex.checkpoint.Epoch != epoch {
				t.Fatal("fresh write did not establish its epoch at verified baseline EOF")
			}
			if reads["catchup"] != 0 || reads["indexed"] != 0 || reads["witness"] > correctionFeedbackWitnessBytes {
				t.Fatalf("fresh submission read unrelated history: %v", reads)
			}

			s.feedbackStore, err = newFeedbackStoreFromEnv()
			if err != nil {
				t.Fatal(err)
			}
			clear(reads)
			s.feedbackStore.correctionIndex.observeRead = observe
			replay := correctionTestMustPost(t, mux, "/tools/feedback_submit", body)
			id := anyMap(created["feedback"])["id"]
			if replay["idempotentReplay"] != true || anyMap(replay["feedback"])["id"] != id {
				t.Fatal("reopen did not recover the indexed record")
			}
			event := correctionTestEvent(map[string]any{
				"operation_id": body["idempotencyKey"], "category": "useful", "factual": false, "target_claim_id": "", "feedback_id": id,
			})
			correctionTestMustPost(t, mux, "/v1/agents/sessions/event", event)
			entry, found, err := s.feedbackStore.lookupCorrectionFeedbackIndexLocked(anyToString(body["idempotencyKey"]))
			if err != nil || !found {
				t.Fatalf("indexed position unavailable: %v", err)
			}
			if reads["indexed"] != 2*entry.Length || reads["catchup"] != 0 || reads["witness"] > 2*correctionFeedbackWitnessBytes {
				t.Fatalf("replay or downstream proof read unrelated history: %v", reads)
			}
			t.Logf("untouched legacy prefix=%d bytes; indexed replay plus event proof read=%d bytes; witness reads=%d bytes",
				epoch, reads["indexed"], reads["witness"])
		})
	}
}

func TestCorrectionFeedbackOwnerRecoversLedgerAheadOfIndex(t *testing.T) {
	s := correctionTestServer(t)
	mux := buildMux(s)
	body := correctionFeedbackTestRequest("Crash before publishing the offset.")
	s.feedbackStore.correctionIndex.afterLedgerSync = func() error { return errors.New("lost owner before index commit") }
	if code, out := correctionTestPost(mux, "/tools/feedback_submit", body); code != http.StatusServiceUnavailable || out["feedback"] != nil {
		t.Fatal("unpublished offset received an acknowledgment")
	}
	historyPath := s.feedbackStore.path
	persisted, err := os.ReadFile(historyPath)
	if err != nil {
		t.Fatal(err)
	}
	original, err := parseJSONMap(persisted)
	if err != nil || original["id"] == nil || s.feedbackStore.correctionIndex.durableEnd != 0 {
		t.Fatal("failure did not leave one committed feedback row ahead of the index")
	}
	if code, _ := correctionTestPost(mux, "/tools/feedback_submit", body); code != http.StatusServiceUnavailable {
		t.Fatal("unopened uncertain owner accepted a retry")
	}

	// Simulate ordinary history written while the typed owner was offline.
	// The missing typed offset is in the first page, but no miss or replay may
	// be acknowledged until the entire suffix is classified.
	size := appendCorrectionFeedbackLegacyFixture(t, historyPath, 2*maxCorrectionFeedbackCatchupBytes) + int64(len(persisted))
	reopen := func() {
		t.Helper()
		var err error
		s.feedbackStore, err = newFeedbackStoreFromEnv()
		if err != nil {
			t.Fatal(err)
		}
	}
	reopen()
	unknown := correctionFeedbackTestRequest("Do not treat an unverified suffix as absence.")
	var catchupBytes int
	observe := func(phase string, n int) {
		if phase == "catchup" {
			catchupBytes += n
		}
	}
	s.feedbackStore.correctionIndex.observeRead = observe
	if code, _ := correctionTestPost(mux, "/tools/feedback_submit", unknown); code != http.StatusServiceUnavailable {
		t.Fatal("incomplete catchup authorized a new operation")
	}
	first := s.feedbackStore.correctionIndex.durableEnd
	if first <= 0 || first >= size || catchupBytes > maxCorrectionFeedbackCatchupBytes {
		t.Fatalf("first recovery page was not bounded and checkpointed: bytes=%d frontier=%d", catchupBytes, first)
	}
	reopen()
	if s.feedbackStore.correctionIndex.durableEnd != first {
		t.Fatal("restart discarded verified recovery progress")
	}
	s.feedbackStore.correctionIndex.observeRead = observe
	var replay map[string]any
	for range 3 {
		catchupBytes = 0
		code, out := correctionTestPost(mux, "/tools/feedback_submit", body)
		if catchupBytes > maxCorrectionFeedbackCatchupBytes {
			t.Fatal("one request exceeded its catchup budget")
		}
		if code == http.StatusOK {
			replay = out
			break
		}
		if code != http.StatusServiceUnavailable {
			t.Fatalf("unexpected catchup status: %d", code)
		}
	}
	if replay["idempotentReplay"] != true || anyMap(replay["feedback"])["id"] != original["id"] ||
		s.feedbackStore.correctionIndex.durableEnd != size {
		t.Fatal("bounded recovery did not replay the original record at verified EOF")
	}
	if info, err := os.Stat(historyPath); err != nil || info.Size() != size {
		t.Fatal("recovery appended to or rewrote the owner ledger")
	}
}

func TestCorrectionFeedbackOwnerCheckpointsOrdinaryGrowth(t *testing.T) {
	s := correctionTestServer(t)
	mux := buildMux(s)
	body := correctionFeedbackTestRequest("Keep the verified tail bounded.")
	created := correctionTestMustPost(t, mux, "/tools/feedback_submit", body)
	first := s.feedbackStore.correctionIndex.durableEnd
	legacy := map[string]any{"id": "older", "source": "user", "content": strings.Repeat("a", 768<<10)}
	for range 9 {
		if err := s.feedbackStore.append(legacy); err != nil {
			t.Fatal(err)
		}
		if lag := s.feedbackStore.correctionSize - s.feedbackStore.correctionIndex.durableEnd; lag > maxCorrectionFeedbackCatchupBytes {
			t.Fatalf("ordinary owner write left an oversized crash suffix: %d", lag)
		}
	}
	if s.feedbackStore.correctionIndex.durableEnd <= first {
		t.Fatal("ordinary growth never advanced the durable index")
	}
	var err error
	s.feedbackStore, err = newFeedbackStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	out := correctionTestMustPost(t, mux, "/tools/feedback_submit", body)
	if out["idempotentReplay"] != true || anyMap(out["feedback"])["id"] != anyMap(created["feedback"])["id"] ||
		s.feedbackStore.correctionIndex.durableEnd != s.feedbackStore.correctionSize {
		t.Fatal("one bounded recovery page did not reconcile ordinary growth")
	}
}
