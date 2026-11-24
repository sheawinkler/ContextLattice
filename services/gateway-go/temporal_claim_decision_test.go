package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func manualDecisionBody(selector string, op map[string]any) map[string]any {
	return map[string]any{"project": correctionTestProject, "topic_path": "release/review", "agent_id": correctionTestAgent,
		"session_id": correctionTestSession, selector: op}
}

func manualDecisionPost(t *testing.T, h http.Handler, path, key string, body map[string]any, want int) map[string]any {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.RemoteAddr = "127.0.0.1:4567"
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("X-Api-Key", key)
	}
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	var result map[string]any
	if out.Code != want || json.Unmarshal(out.Body.Bytes(), &result) != nil {
		t.Fatalf("%s: got HTTP %d, want %d", path, out.Code, want)
	}
	if want != 200 && (result["decision"] != nil || result["claims"] != nil || result["recorded"] != nil) {
		t.Fatal("denial returned decision data")
	}
	return result
}

func TestManualDecisionNativeReceiptLineage(t *testing.T) {
	bin := os.Getenv("CONTEXTLATTICE_TEST_AGENT_TOOLS_BINARY")
	if !filepath.IsAbs(bin) {
		t.Skip("explicit source-built native CLI required")
	}
	s := correctionTestServer(t)
	s.orchestratorAPIKey = "manual-fixture-key"
	api := httptest.NewServer(buildMux(s))
	defer api.Close()
	session := correctionTestSession
	run := func(cmd string, args ...string) map[string]any {
		t.Helper()
		flags := []string{cmd, "--project", correctionTestProject, "--topic-path", "release/review", "--agent-id", correctionTestAgent, "--session-id", session, "--decision-id", "review", "--raw"}
		child := exec.Command(bin, append(flags, args...)...)
		child.Dir = t.TempDir()
		child.Env = []string{"HOME=" + child.Dir, "CONTEXTLATTICE_ORCHESTRATOR_URL=" + api.URL, "CONTEXTLATTICE_ORCHESTRATOR_API_KEY=" + s.orchestratorAPIKey}
		raw, err := child.CombinedOutput()
		var out map[string]any
		if err != nil || json.Unmarshal(raw, &out) != nil {
			t.Fatalf("native %s: %v: %s", cmd, err, raw)
		}
		return out
	}
	write := func(args ...string) map[string]any { return run("correct", args...) }
	query := func(args ...string) map[string]any { return anyMap(run("claim-query", args...)["decision"]) }
	value := func(row map[string]any) any { return anyMap(row["current"])["value"] }
	write("--effect", "set", "--expect-version", "0", "--object", "ask")
	write("--effect", "replace", "--expect-version", "1", "--target-version", "1", "--object", "review")
	write("--effect", "replace", "--expect-version", "2", "--target-version", "2", "--object", "two reviewers")
	exception := []string{"--effect", "exception", "--expect-version", "3", "--target-version", "3", "--scope", "preview", "--object", "one reviewer"}
	write(exception...)
	write("--effect", "detail", "--expect-version", "4", "--target-version", "4", "--scope", "preview", "--object", "keep the log")
	v := query("--scope", "preview")
	if value(v) != "one reviewer" || len(contextPackAnyList(v["details"])) != 1 || value(query("--scope", "preview/mobile")) != "two reviewers" {
		t.Fatal("scope/detail changed default")
	}
	write("--effect", "contradict", "--expect-version", "5", "--target-version", "4", "--scope", "preview", "--object", "approval pending")
	if v := query("--scope", "preview"); v["state"] != "unresolved_contradiction" || v["current"] != nil || v["details"] != nil {
		t.Fatal("disputed instruction was actionable")
	}
	write("--effect", "retract", "--expect-version", "6", "--target-version", "6", "--scope", "preview")
	write("--effect", "retract", "--expect-version", "7", "--target-version", "4", "--scope", "preview")
	// First replay original envelopes; then read only the compact owner/index pair.
	for _, compact := range []bool{false, true} {
		if compact {
			s.temporalClaims.mu.Lock()
			err := s.temporalClaims.compactLocked()
			s.temporalClaims.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
		}
		store, err := newTemporalClaimStoreFromEnv()
		if err != nil || store.correctionUncertain {
			t.Fatalf("owner reopen: %v", err)
		}
		s.temporalClaims = store
	}
	session = "fresh-manual-session"
	manualDecisionPost(t, buildMux(s), "/v1/agents/sessions/start", s.orchestratorAPIKey, map[string]any{"project": correctionTestProject, "session_id": session, "agent": "codex", "agent_id": correctionTestAgent}, 200)
	if v := query("--scope", "preview"); value(v) != "two reviewers" || v["details"] != nil {
		t.Fatal("fresh session did not restore the default")
	}
	if write(exception...)["idempotent_replay"] != true || s.temporalClaims.correctionStamp.Seq != 8 {
		t.Fatal("fresh session duplicated the receipt")
	}
	legacy := manualDecisionPost(t, buildMux(s), "/memory/claims/query", s.orchestratorAPIKey, map[string]any{"project": correctionTestProject}, 200)
	if anyToInt(legacy["claim_count"], -1) != 0 || len(s.feedbackStore.list(correctionTestProject, "", "agent", 10)) != 0 {
		t.Fatal("decision entered ordinary discovery or feedback")
	}
}
