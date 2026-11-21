package main

import (
	"context"
	"debug/buildinfo"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPublicNativeCallerIdentityAcrossActorsAndRestart(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "contextlattice")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/contextlattice-agent-tools")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build native CLI: %v: %s", err, output)
	}
	info, err := buildinfo.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" || setting.Key == "vcs.modified" {
			t.Logf("native binary %s=%s", setting.Key, setting.Value)
		}
	}
	for _, name := range []string{"home", "state", "cwd"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GO_RETRIEVAL_NATIVE_QDRANT_ENABLED", "false")
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", sourceQdrant)
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", sourceQdrant)
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("GO_WRITE_DEFAULT_AGENT_ID", "wrong-gateway-default")
	t.Setenv("GO_AGENT_SESSIONS_PATH", filepath.Join(dir, "sessions.json"))
	t.Setenv("GO_AGENT_SESSION_LEDGER_PATH", filepath.Join(dir, "session-events.ndjson"))
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", filepath.Join(dir, "quality.ndjson"))
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_RETRIEVAL_CONTINUATION_DURABLE_ENABLED", "false")
	var transportCalls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/retrieval/query" {
			t.Errorf("unexpected backend call: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": []any{map[string]any{
			"project": "alpha", "source": sourceQdrant, "score": 0.96, "summary": "Fixture attribution confirms a deterministic next action.",
		}}, "warnings": []any{}})
	}))
	defer backend.Close()
	s := newTestServer(t, backend.URL)
	s.memoryStore = newExactReadTestStore(t)
	s.orchestratorAPIKey = ""
	// Expose only the real handlers needed by the fixture. No reviewer route.
	router := http.NewServeMux()
	router.HandleFunc("/memory/synthesis-pack/v2", s.memorySynthesisPackV2)
	router.HandleFunc("/memory/recall/response", s.memoryRecallResponse)
	router.HandleFunc("/v1/agents/sessions/", s.agentsSessionsRoute)
	router.HandleFunc("/memory/write", s.memoryWrite)
	router.HandleFunc("/telemetry/context-pack-quality/outcome", s.telemetryContextPackQualityOutcomeRoute)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		transportCalls.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Error("keyless public caller sent credentials")
		}
		if r.URL.Path == "/telemetry/context-pack-quality/outcome" {
			response := httptest.NewRecorder()
			router.ServeHTTP(response, r)
			if response.Code != http.StatusOK {
				t.Logf("outcome handler status=%d response=%s", response.Code, response.Body.String())
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(response.Code)
			_, _ = w.Write(response.Body.Bytes())
			return
		}
		router.ServeHTTP(w, r)
	}))
	defer gateway.Close()

	const project, firstID, secondID = "alpha", "mercury_agent", "omp_agent"
	run := func(actor, model, selected string, fail bool, args ...string) map[string]any {
		t.Helper()
		command := exec.CommandContext(ctx, binary, append(args, "--project", project, "--raw")...)
		command.Dir = filepath.Join(dir, "cwd")
		command.Env = []string{
			"PATH=" + os.Getenv("PATH"), "TMPDIR=" + os.TempDir(),
			"HOME=" + filepath.Join(dir, "home"), "CONTEXTLATTICE_GLOBAL_HOME=" + filepath.Join(dir, "state"),
			"CONTEXTLATTICE_ORCHESTRATOR_URL=" + gateway.URL, "CONTEXTLATTICE_ASYNC_INBOX_AUTO_DRAIN=0",
			"CONTEXTLATTICE_NATIVE_SESSION_ID=native-" + actor,
			"CONTEXTLATTICE_AGENT_ID=" + actor, "MEMMCP_AGENT_ID=wrong-legacy-default",
			"CONTEXTLATTICE_MODEL=" + model, "CONTEXTLATTICE_SESSION_ID=" + selected,
		}
		output, err := command.Output()
		if fail {
			if err == nil {
				t.Fatalf("cross-actor %s unexpectedly succeeded: %s", args[0], output)
			}
			return nil
		}
		if err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				t.Fatalf("%s %s: %v: %s %s", actor, args[0], err, exit.Stderr, output)
			}
			t.Fatal(err)
		}
		var receipt map[string]any
		if err := json.Unmarshal(output, &receipt); err != nil || receipt["ok"] != true {
			t.Fatalf("%s %s did not emit a successful receipt: %s (%v)", actor, args[0], output, err)
		}
		return receipt
	}
	sessionID := func(actor string) string {
		t.Helper()
		s.agentSessions.mu.RLock()
		defer s.agentSessions.mu.RUnlock()
		var id string
		for key, session := range s.agentSessions.sessions {
			if session["agent_id"] == actor {
				if id != "" {
					t.Fatalf("more than one session for %s", actor)
				}
				id = key
			}
		}
		if id == "" {
			t.Fatalf("no durable session for %s", actor)
		}
		return id
	}
	run(firstID, "opus55", "", false, "context", "fixture attribution")
	firstSession := sessionID(firstID)
	response := run(secondID, "", "", false, "context", "fixture attribution", "--response")
	if response["schema_id"] != recallResponseContractID || response["caller_attribution"] != nil {
		t.Fatalf("context --response did not preserve the public recall contract: %#v", response)
	}
	secondSession := sessionID(secondID)
	if firstSession == secondSession {
		t.Fatal("two actors selected one durable session")
	}
	// Each command is a new native process, sharing only the disposable
	// caller cache and the durable gateway state.
	run(secondID, "", firstSession, true, "context", "must not cross actors", "--response")
	run(secondID, "", firstSession, true, "write", "--file", "notes/cross.md", "--content", "must not cross actors")
	run(secondID, "", firstSession, true, "remember", "must not cross actors", "--file", "notes/cross.md")
	run(secondID, "", firstSession, true, "finish", "must not cross actors", "--no-outcome")
	if _, ok := s.memoryStore.currentEntry(project, "notes/cross.md"); ok {
		t.Fatal("a rejected caller wrote a checkpoint")
	}

	run(firstID, "opus55", "", false, "write", "--file", "notes/same.md", "--content", "same fixture bytes")
	entry, ok := s.memoryStore.currentEntry(project, "notes/same.md")
	if !ok || entry.AgentID != firstID || entry.SessionID != firstSession || !slices.Contains(entry.Tags, firstID+"-opus55") {
		t.Fatalf("known write attribution was not durable: %#v", entry)
	}
	run(secondID, "", "", false, "write", "--file", "notes/same.md", "--content", "same fixture bytes")
	entry, ok = s.memoryStore.currentEntry(project, "notes/same.md")
	if !ok || entry.AgentID != secondID || entry.SessionID != secondSession || slices.Contains(entry.Tags, firstID+"-opus55") {
		t.Fatalf("same-byte write inherited the wrong caller or model: %#v", entry)
	}
	run(firstID, "", "", false, "remember", "model unknown at checkpoint", "--file", "notes/first.md")
	run(secondID, "fixture-model", "", false, "adapter", "checkpoint", "--content", "known at checkpoint", "--file", "notes/second.md")
	for _, want := range []struct {
		file, actor, id, model string
	}{
		{"notes/first.md", firstID, firstSession, ""},
		{"notes/second.md", secondID, secondSession, "fixture-model"},
	} {
		entry, ok := s.memoryStore.currentEntry(project, want.file)
		if !ok || entry.AgentID != want.actor || entry.SessionID != want.id {
			t.Fatalf("wrong stored checkpoint caller: %#v", entry)
		}
		gotModelTag := ""
		for _, tag := range entry.Tags {
			if strings.HasPrefix(tag, want.actor+"-") {
				gotModelTag = tag
			}
		}
		if want.model == "" && gotModelTag != "" || want.model != "" && gotModelTag != want.actor+"-"+want.model {
			t.Fatalf("wrong stored checkpoint model: %q", gotModelTag)
		}
	}
	run(firstID, "opus55", "", false, "finish", "first actor done", "--success")
	run(secondID, "", "", false, "finish", "second actor done", "--success")

	reloaded, err := newAgentSessionStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		actor, id, contextModel, checkpointModel, finishModel string
	}{
		{firstID, firstSession, "opus55", "", "opus55"},
		{secondID, secondSession, "", "fixture-model", ""},
	} {
		session, events, found := reloaded.get(want.id)
		if !found || session["agent_id"] != want.actor || session["status"] != "completed" {
			t.Fatalf("wrong durable lifecycle: %#v", session)
		}
		expected := map[string]string{
			"context_pack.completed": want.contextModel, "writeback.completed": want.checkpointModel,
			"context_pack.outcome_reported": want.finishModel, "session.completed": want.finishModel,
		}
		if want.actor == secondID {
			delete(expected, "context_pack.outcome_reported")
		}
		for _, event := range events {
			kind := anyToString(event["type"])
			if strings.Contains(kind, "accept") || strings.Contains(kind, "verif") {
				t.Fatalf("caller telemetry manufactured review or verification: %s", kind)
			}
			if want.actor == secondID && kind == "context_pack.outcome_reported" {
				t.Fatal("bounded recall manufactured a finish sample")
			}
			model, check := expected[kind]
			if !check {
				continue
			}
			metadata := anyMap(anyMap(event["metadata"])["caller_attribution"])
			if event["agent_id"] != want.actor || metadata["source"] != "caller_reported" ||
				metadata["model_known"] != (model != "") || anyToString(metadata["model"]) != model {
				t.Fatalf("wrong %s attribution for %s: %#v", kind, want.actor, metadata)
			}
			delete(expected, kind)
		}
		if len(expected) > 0 {
			t.Fatalf("missing durable events for %s: %v", want.actor, expected)
		}
	}
	t.Logf("fixture actors=2 CLI invocations=12 loopback requests=%d inference_calls=0 independent_acceptances=0", transportCalls.Load())
}
