package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

const (
	correctionTestProject = "alpha"
	correctionTestAgent   = "agent-a"
	correctionTestSession = "sess-correction"
	correctionTestTarget  = "claim-target"
)

func correctionTestServer(t *testing.T) *server {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CONTEXTLATTICE_TEMPORAL_CLAIMS_ENABLED", "true")
	t.Setenv("CONTEXTLATTICE_TEMPORAL_CLAIMS_PATH", filepath.Join(root, "claims.ndjson"))
	t.Setenv("GO_AGENT_SESSIONS_PATH", filepath.Join(root, "sessions.json"))
	t.Setenv("FEEDBACK_HISTORY_PATH", filepath.Join(root, "feedback.ndjson"))
	s := newTestServer(t, "http://127.0.0.1:1")
	mux := buildMux(s)
	correctionTestMustPost(t, mux, "/v1/agents/sessions/start", map[string]any{
		"session_id": correctionTestSession, "project": correctionTestProject, "agent": "codex", "agent_id": correctionTestAgent,
	})
	return s
}

func correctionTestPost(mux http.Handler, path string, body map[string]any) (int, map[string]any) {
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	mux.ServeHTTP(out, req)
	var result map[string]any
	_ = json.Unmarshal(out.Body.Bytes(), &result)
	return out.Code, result
}

func correctionTestMustPost(t *testing.T, mux http.Handler, path string, body map[string]any) map[string]any {
	t.Helper()
	code, result := correctionTestPost(mux, path, body)
	if code != http.StatusOK || result["ok"] != true {
		t.Fatalf("%s rejected: status=%d result=%v", path, code, result)
	}
	return result
}

func correctionTestLegacyClaim(project string) map[string]any {
	return map[string]any{
		"claim_id": correctionTestTarget, "project": project, "subject": "release", "predicate": "version", "object": "old",
		"verification": map[string]any{"status": "verified", "method": "independent test"},
	}
}

func correctionTestFeedback(t *testing.T, mux http.Handler, category, note, target string, factual bool) map[string]any {
	t.Helper()
	id := correctionOperationID(correctionTestProject, correctionTestSession, correctionTestAgent, category, factual, target, note)
	out := correctionTestMustPost(t, mux, "/tools/feedback_submit", map[string]any{
		"project": correctionTestProject, "source": "agent", "content": note, "idempotencyKey": id,
		"metadata": map[string]any{
			"schema_id": "contextlattice_correction.v1", "category": category, "factual": factual,
			"session_id": correctionTestSession, "agent_id": correctionTestAgent, "target_claim_id": target, "operation_id": id,
		},
	})
	return map[string]any{"operation_id": id, "category": category, "factual": factual, "target_claim_id": target, "feedback_id": anyMap(out["feedback"])["id"]}
}

func correctionTestClaim(req map[string]any) map[string]any {
	return map[string]any{
		"claim_id": "", "project": correctionTestProject, "session_id": correctionTestSession, "agent_id": correctionTestAgent,
		"subject": "release", "predicate": "version", "object": "new", "topic_path": "claims/corrections", "correction": req,
	}
}

func correctionTestEvent(req map[string]any) map[string]any {
	return map[string]any{
		"session_id": correctionTestSession, "project": correctionTestProject, "agent_id": correctionTestAgent,
		"type": "correction.recorded", "correction": req,
	}
}

func TestCorrectionRecordingHTTPReplay(t *testing.T) {
	s := correctionTestServer(t)
	mux := buildMux(s)
	legacy := correctionTestLegacyClaim(correctionTestProject)
	correctionTestMustPost(t, mux, "/memory/claims", legacy)
	again := correctionTestMustPost(t, mux, "/memory/claims", legacy)
	if anyToInt(anyMap(again["claim"])["revision"], 0) != 2 {
		t.Fatal("legacy writes must still revise the ordinary claim")
	}
	req := correctionTestFeedback(t, mux, "superseded", "The release record says new.", correctionTestTarget, true)
	claim, event := correctionTestClaim(req), correctionTestEvent(req)

	if code, _ := correctionTestPost(mux, "/v1/agents/sessions/event", event); code != http.StatusConflict {
		t.Fatalf("unpersisted claim proof was accepted: %d", code)
	}
	foreign := cloneJSONMap(claim)
	foreign["agent_id"] = "other-agent"
	if code, _ := correctionTestPost(mux, "/memory/claims", foreign); code != http.StatusNotFound {
		t.Fatalf("claim accepted a different session actor: %d", code)
	}
	badFeedback := cloneJSONMap(claim)
	anyMap(badFeedback["correction"])["feedback_id"] = "missing-feedback"
	if code, _ := correctionTestPost(mux, "/memory/claims", badFeedback); code != http.StatusConflict {
		t.Fatalf("claim accepted missing feedback proof: %d", code)
	}
	forged := cloneJSONMap(event)
	delete(forged, "correction")
	forged["metadata"] = map[string]any{"claim_persisted": true}
	if code, _ := correctionTestPost(mux, "/v1/agents/sessions/event", forged); code != http.StatusUnprocessableEntity {
		t.Fatalf("legacy event accepted reserved correction proof: %d", code)
	}
	_, before, _ := s.agentSessions.get(correctionTestSession)
	parallel := func(path string, body map[string]any, inspect func(map[string]any) bool) {
		t.Helper()
		const calls = 8
		var wg sync.WaitGroup
		notReplayed := make(chan bool, calls)
		for range calls {
			wg.Go(func() {
				code, out := correctionTestPost(mux, path, body)
				if code != http.StatusOK || out["ok"] != true {
					t.Errorf("%s failed: status=%d", path, code)
					return
				}
				notReplayed <- inspect(out)
			})
		}
		wg.Wait()
		close(notReplayed)
		newWrites := 0
		for fresh := range notReplayed {
			if fresh {
				newWrites++
			}
		}
		if newWrites != 1 {
			t.Fatalf("%s created %d writes, want 1", path, newWrites)
		}
	}
	parallel("/memory/claims", claim, func(out map[string]any) bool {
		row := anyMap(out["claim"])
		if row["schema_id"] != temporalClaimContractID || anyToInt(row["revision"], 0) != 1 ||
			anyMap(row["verification"])["status"] != "unverified" ||
			anyToBool(anyMap(out["format_contract"])["contract_valid"]) != true {
			t.Error("invalid claim receipt")
		}
		return out["idempotent_replay"] == false
	})
	parallel("/v1/agents/sessions/event", event, func(out map[string]any) bool {
		row := anyMap(out["event"])
		metadata := anyMap(row["metadata"])
		if row["id"] != req["operation_id"] || metadata["claim_persisted"] != true ||
			metadata["correction_status"] != "factual_persisted" || anyToInt(metadata["claim_revision"], 0) != 1 {
			t.Error("invalid factual event receipt")
		}
		return row["idempotent_replay"] != true
	})
	_, after, _ := s.agentSessions.get(correctionTestSession)
	if len(after) != len(before)+1 {
		t.Fatalf("replays changed visible event count: before=%d after=%d", len(before), len(after))
	}

	changed := cloneJSONMap(claim)
	changed["object"] = "changed payload"
	if code, _ := correctionTestPost(mux, "/memory/claims", changed); code != http.StatusConflict {
		t.Fatalf("changed payload reused correction key: %d", code)
	}
	collision := correctionTestClaim(correctionTestFeedback(t, mux, "superseded", "A different note.", correctionTestTarget, true))
	if code, _ := correctionTestPost(mux, "/memory/claims", collision); code != http.StatusConflict {
		t.Fatalf("new operation revised a prior claim ID: %d", code)
	}
	promotion := cloneJSONMap(claim)
	promotion["verification"] = map[string]any{"status": "verified"}
	if code, _ := correctionTestPost(mux, "/memory/claims", promotion); code != http.StatusUnprocessableEntity {
		t.Fatalf("caller-controlled verification was admitted: %d", code)
	}
	target := s.temporalClaims.claims[correctionTestTarget]
	if target.Revision != 2 || target.Status != "active" || target.Verification["status"] != "verified" {
		t.Fatal("unverified correction changed the target")
	}
	stored := s.temporalClaims.correctionClaims[anyToString(req["operation_id"])]
	if len(s.temporalClaims.correctionClaims) != 1 || s.temporalClaims.claims[stored.Claim.ClaimID].Revision != 1 || s.temporalClaims.logEntries != 3 {
		t.Fatal("correction conflicts or retries appended a claim revision")
	}
	row, _, _ := s.agentSessions.get(correctionTestSession)
	eventCount := anyToInt(row["event_count"], 0)

	s.temporalClaims.mu.Lock()
	err := s.temporalClaims.compactLocked()
	s.temporalClaims.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	s.agentSessions.maxEvents = 1
	if _, _, err := s.agentSessions.appendEvent(correctionTestSession, map[string]any{"type": "audit.control"}); err != nil {
		t.Fatal(err)
	}
	claims, err := newTemporalClaimStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := newAgentSessionStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	feedback, err := newFeedbackStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	s.temporalClaims, s.agentSessions, s.feedbackStore = claims, sessions, feedback
	replayedClaim := correctionTestMustPost(t, mux, "/memory/claims", claim)
	replayedEvent := correctionTestMustPost(t, mux, "/v1/agents/sessions/event", event)
	if replayedClaim["idempotent_replay"] != true || anyToInt(anyMap(replayedClaim["claim"])["revision"], 0) != 1 ||
		anyMap(replayedEvent["event"])["idempotent_replay"] != true {
		t.Fatal("reopened server lost the original receipt")
	}
	_, visible, _ := s.agentSessions.get(correctionTestSession)
	if len(visible) != 1 || visible[0]["type"] != "audit.control" {
		t.Fatal("replay reinserted an evicted event")
	}
	row, _, _ = s.agentSessions.get(correctionTestSession)
	if anyToInt(row["event_count"], 0) != eventCount+1 {
		t.Fatal("retained receipt replay changed event accounting")
	}

	feedbackOnly := correctionTestFeedback(t, mux, "useful", "Helpful source.", "", false)
	out := correctionTestMustPost(t, mux, "/v1/agents/sessions/event", correctionTestEvent(feedbackOnly))
	meta := anyMap(anyMap(out["event"])["metadata"])
	if meta["correction_status"] != "feedback_only" || meta["claim_persisted"] != false || meta["claim_id"] != nil {
		t.Fatal("feedback-only event claimed persistence")
	}
}

func TestCorrectionRecordingNativeCLIWithOwnerFailures(t *testing.T) {
	binary := os.Getenv("CONTEXTLATTICE_TEST_AGENT_TOOLS_BINARY")
	if !filepath.IsAbs(binary) {
		t.Skip("explicit source-bound native CLI binary required")
	}
	s := correctionTestServer(t)
	mux := buildMux(s)
	correctionTestMustPost(t, mux, "/memory/claims", correctionTestLegacyClaim(correctionTestProject))
	gateway := httptest.NewServer(mux)
	defer func() { gateway.Close() }()
	reopenOwners := func() {
		t.Helper()
		gateway.Close()
		var err error
		if s.feedbackStore, err = newFeedbackStoreFromEnv(); err != nil {
			t.Fatal(err)
		}
		if s.temporalClaims, err = newTemporalClaimStoreFromEnv(); err != nil {
			t.Fatal(err)
		}
		if s.agentSessions, err = newAgentSessionStoreFromEnv(); err != nil {
			t.Fatal(err)
		}
		gateway = httptest.NewServer(buildMux(s))
	}
	home := t.TempDir()
	run := func(wantOK bool, args ...string) map[string]any {
		t.Helper()
		cmd := exec.Command(binary, append([]string{"correct", "--raw", "--project", correctionTestProject,
			"--agent-id", correctionTestAgent, "--session-id", correctionTestSession}, args...)...)
		cmd.Dir = home
		cmd.Env = []string{"HOME=" + home, "CONTEXTLATTICE_ORCHESTRATOR_URL=" + gateway.URL,
			"CONTEXTLATTICE_ASYNC_INBOX_AUTO_DRAIN=0"}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		raw, err := cmd.Output()
		var out map[string]any
		if json.Unmarshal(raw, &out) != nil || (err == nil) != wantOK || out["ok"] != wantOK {
			t.Fatalf("CLI status=%v, error=%v, stderr=%s", out["status"], err, &stderr)
		}
		return out
	}
	factual := []string{"New release observed.", "--category", "wrong", "--factual", "--subject", "release",
		"--predicate", "version", "--object", "new", "--target-claim-id", correctionTestTarget}
	blocked := filepath.Join(home, "not-a-directory")
	if err := os.WriteFile(blocked, nil, 0600); err != nil {
		t.Fatal(err)
	}
	_, originalEvents, _ := s.agentSessions.get(correctionTestSession)
	claimPath := s.temporalClaims.path
	setClaimPath := func(path string) {
		s.temporalClaims.mu.Lock()
		s.temporalClaims.path = path
		s.temporalClaims.mu.Unlock()
	}
	setClaimPath(filepath.Join(blocked, "claims.ndjson"))
	out := run(false, factual...)
	setClaimPath(claimPath)
	_, events, _ := s.agentSessions.get(correctionTestSession)
	if out["status"] != "partial_failure" || out["feedback_saved"] != true || out["claim_persisted"] != false ||
		out["event_recorded"] != false || out["claim"] != nil || len(events) != len(originalEvents) {
		t.Fatalf("failed owner write produced misleading correction: %v", out["status"])
	}
	originalFeedbackID := anyMap(out["feedback"])["id"]
	feedbackPath := s.feedbackStore.path
	feedbackBytes, err := os.ReadFile(feedbackPath)
	if err != nil {
		t.Fatal(err)
	}
	reopenOwners()

	sessionPath := s.agentSessions.path
	setSessionPath := func(path string) {
		s.agentSessions.mu.Lock()
		s.agentSessions.path = path
		s.agentSessions.mu.Unlock()
	}
	setSessionPath(filepath.Join(blocked, "sessions.json"))
	out = run(false, factual...)
	setSessionPath(sessionPath)
	_, events, _ = s.agentSessions.get(correctionTestSession)
	if out["status"] != "partial_failure" || out["feedback_saved"] != true || out["claim_persisted"] != true ||
		out["event_recorded"] != false || len(events) != len(originalEvents) {
		t.Fatalf("failed session persist did not report the correct partial stages: %v", out["status"])
	}

	out = run(true, factual...)
	claim := anyMap(out["claim"])
	if out["status"] != "factual_persisted" || out["event_recorded"] != true ||
		anyToInt(claim["revision"], 0) != 1 || !containsString(anyToStringSlice(claim["contradicts"]), correctionTestTarget) ||
		anyMap(out["feedback"])["id"] != originalFeedbackID {
		t.Fatal("retry failed to complete the original operation")
	}
	verification := anyMap(claim["verification"])
	if verification["status"] != "unverified" || verification["method"] != "reporter_assertion" ||
		len(contextPackAnyList(claim["support"])) != 0 || len(contextPackAnyList(claim["opposition"])) != 0 {
		t.Fatal("persisted feedback was promoted into B2 attestation")
	}
	replayed := run(true, factual...)
	replayedEvent := anyMap(anyMap(replayed["event"])["event"])
	if replayed["operation_id"] != out["operation_id"] || replayedEvent["idempotent_replay"] != true {
		t.Fatal("CLI replay was not recognized by the server")
	}
	_, events, _ = s.agentSessions.get(correctionTestSession)
	if len(events) != len(originalEvents)+1 || len(s.feedbackStore.list(correctionTestProject, "", "agent", 10)) != 1 {
		t.Fatal("retry duplicated the correction event or feedback")
	}
	if after, err := os.ReadFile(feedbackPath); err != nil || !bytes.Equal(after, feedbackBytes) {
		t.Fatal("retry after partial failure and reopen appended a feedback row")
	}

	out = run(true, "Helpful note.", "--category", "useful")
	if out["status"] != "feedback_only" || out["feedback_saved"] != true || out["claim_persisted"] != false || out["event_recorded"] != true {
		t.Fatal("feedback-only correction was not recorded distinctly")
	}
	feedbackBytes, err = os.ReadFile(feedbackPath)
	if err != nil {
		t.Fatal(err)
	}
	reopenOwners()
	replayed = run(true, "Helpful note.", "--category", "useful")
	if replayed["status"] != "feedback_only" || anyMap(replayed["feedback"])["id"] != anyMap(out["feedback"])["id"] ||
		anyMap(anyMap(replayed["event"])["event"])["idempotent_replay"] != true {
		t.Fatal("feedback-only retry after reopen did not return the original receipts")
	}
	if after, err := os.ReadFile(feedbackPath); err != nil || !bytes.Equal(after, feedbackBytes) {
		t.Fatal("feedback-only retry after reopen appended a row")
	}

	gateway.Close()
	s.feedbackStore = nil
	gateway = httptest.NewServer(buildMux(s))
	out = run(false, factual...)
	if out["status"] != "failed" || out["feedback_saved"] != false || out["claim_persisted"] != false || out["event_recorded"] != false {
		t.Fatal("unavailable feedback owner produced a success receipt")
	}
}

func TestCorrectionRecordingOwnerReopenAfterUnconfirmedReplace(t *testing.T) {
	s := correctionTestServer(t)
	mux := buildMux(s)
	feedbackOnly := correctionTestFeedback(t, mux, "useful", "Preserve this next step.", "", false)
	event := correctionTestEvent(feedbackOnly)
	_, originalEvents, _ := s.agentSessions.get(correctionTestSession)
	s.agentSessions.decisionWriteHook = func(phase string) error {
		if phase == "after_replace" {
			return errors.New("injected confirmation failure after snapshot replacement")
		}
		return nil
	}
	if code, out := correctionTestPost(mux, "/v1/agents/sessions/event", event); code != http.StatusServiceUnavailable || out["event"] != nil {
		t.Fatalf("unconfirmed replacement unexpectedly acknowledged: status=%d", code)
	}
	if code, _ := correctionTestPost(mux, "/v1/agents/sessions/event", event); code != http.StatusServiceUnavailable {
		t.Fatalf("uncertain owner allowed another write: %d", code)
	}
	reopened, err := newAgentSessionStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	s.agentSessions = reopened
	row, events, _ := reopened.get(correctionTestSession)
	before := anyToInt(row["event_count"], 0)
	if len(events) != len(originalEvents)+1 || events[len(events)-1]["id"] != feedbackOnly["operation_id"] {
		t.Fatalf("snapshot replacement did not contain the correction event: %d events", len(events))
	}
	out := correctionTestMustPost(t, mux, "/v1/agents/sessions/event", event)
	if anyMap(out["event"])["idempotent_replay"] != true || anyToInt(anyMap(out["session"])["event_count"], 0) != before {
		t.Fatal("reopening and replaying the unconfirmed event changed event accounting")
	}
}

func TestCorrectionRecordingUnterminatedClaimRecordFailsClosed(t *testing.T) {
	s := correctionTestServer(t)
	mux := buildMux(s)
	req := correctionTestFeedback(t, mux, "useful", "Record this assertion.", "", true)
	payload := correctionTestClaim(req)
	result := correctionTestMustPost(t, mux, "/memory/claims", payload)
	claimID := anyToString(anyMap(result["claim"])["claim_id"])
	path := s.temporalClaims.path
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 || raw[len(raw)-1] != '\n' {
		t.Fatalf("expected an owner-produced, newline-terminated claim record: %v", err)
	}
	torn := raw[:len(raw)-1]
	if err := os.WriteFile(path, torn, 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := newTemporalClaimStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	s.temporalClaims = reopened
	if _, present := reopened.claims[claimID]; present {
		t.Fatal("unterminated write became visible as a claim")
	}
	if code, _ := correctionTestPost(mux, "/memory/claims", payload); code != http.StatusServiceUnavailable {
		t.Fatalf("uncertain claim ledger allowed a blind retry: %d", code)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, torn) {
		t.Fatal("failed-closed retry changed the ledger")
	}
}

func TestCorrectionRecordingPublicScope(t *testing.T) {
	s := correctionTestServer(t)
	mux := buildMux(s)
	legacy := correctionTestLegacyClaim("ALPHA")
	correctionTestMustPost(t, mux, "/memory/claims", legacy)
	req := correctionTestFeedback(t, mux, "wrong", "The target is not in this project.", correctionTestTarget, true)
	if code, _ := correctionTestPost(mux, "/memory/claims", correctionTestClaim(req)); code != http.StatusConflict {
		t.Fatalf("accepted a target in a different project: %d", code)
	}
	foreignEvent := correctionTestEvent(req)
	foreignEvent["agent_id"] = "other-actor"
	if code, _ := correctionTestPost(mux, "/v1/agents/sessions/event", foreignEvent); code != http.StatusNotFound {
		t.Fatalf("looked up custody for a different session actor: %d", code)
	}

	original := correctionFeedbackTestRequest("An ordinary note.")
	for _, field := range []string{"agent_id", "project"} {
		body := cloneJSONMap(original)
		meta := anyMap(body["metadata"])
		if field == "project" {
			body["project"] = "ALPHA"
		} else {
			meta["agent_id"] = "other-actor"
		}
		id := correctionOperationID(anyToString(body["project"]), correctionTestSession, anyToString(meta["agent_id"]),
			"useful", false, "", anyToString(body["content"]))
		body["idempotencyKey"], meta["operation_id"] = id, id
		if code, out := correctionTestPost(mux, "/tools/feedback_submit", body); code != http.StatusNotFound || out["feedback"] != nil {
			t.Fatalf("stored or disclosed feedback for a different session %s: %d", field, code)
		}
	}
	if len(s.feedbackStore.records) != 1 || len(s.temporalClaims.correctionClaims) != 0 {
		t.Fatal("project or actor failures left a durable write")
	}

	// A sessionless correction is still a public observation, not an event.
	body := cloneJSONMap(original)
	meta := anyMap(body["metadata"])
	id := correctionOperationID(correctionTestProject, "", correctionTestAgent, "useful", false, "", anyToString(body["content"]))
	body["idempotencyKey"], meta["operation_id"], meta["session_id"] = id, id, ""
	saved := correctionTestMustPost(t, mux, "/tools/feedback_submit", body)
	unbound := map[string]any{"operation_id": id, "category": "useful", "factual": false,
		"target_claim_id": "", "feedback_id": anyMap(saved["feedback"])["id"]}
	if code, _ := correctionTestPost(mux, "/v1/agents/sessions/event", correctionTestEvent(unbound)); code != http.StatusConflict {
		t.Fatalf("used a sessionless feedback receipt as a session receipt: %d", code)
	}
}
