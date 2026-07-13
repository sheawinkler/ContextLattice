package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func assertNoCanaryPath(t *testing.T, payload any, canary string) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), canary) {
		t.Fatalf("response leaked local storage path %q: %s", canary, string(raw))
	}
}

func TestLegacyRuntimeStoresMigrateFilesToOwnerOnly(t *testing.T) {
	root := t.TempDir()
	sessionPath := filepath.Join(root, "sessions.json")
	feedbackPath := filepath.Join(root, "feedback.ndjson")
	fixtures := map[string]string{
		sessionPath:  `{"sessions":[],"events":{}}`,
		feedbackPath: "{}\n",
	}
	for path, content := range fixtures {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GO_AGENT_SESSIONS_PATH", sessionPath)
	t.Setenv("FEEDBACK_HISTORY_PATH", feedbackPath)

	sessions, err := newAgentSessionStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	feedback, err := newFeedbackStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	for path := range fixtures {
		assertMode(t, path, 0o600)
	}
	if err := feedback.append(map[string]any{"id": "feedback_test"}); err != nil {
		t.Fatal(err)
	}
	assertMode(t, feedbackPath, 0o600)

	snapshot := sessions.runtimeSnapshot(10)
	if anyToString(snapshot["store_ref"]) == "" {
		t.Fatalf("session runtime must expose an opaque store reference: %#v", snapshot)
	}
	assertNoCanaryPath(t, snapshot, sessionPath)
}

func TestStorageResponseProjectionsDoNotExposeHostPaths(t *testing.T) {
	root := t.TempDir()
	artifactPath := filepath.Join(root, "secret-ledger.ndjson")
	if err := os.WriteFile(artifactPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tracked := collectTrackedStorage(map[string]string{"secret": artifactPath}, 10)
	assertNoCanaryPath(t, tracked, artifactPath)
	if anyToString(anyMap(tracked["secret"])["artifact_ref"]) == "" {
		t.Fatalf("tracked storage must expose an opaque artifact reference: %#v", tracked)
	}
	disk, err := diskUsageSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	assertNoCanaryPath(t, disk, root)

	memory := &memoryStore{policy: memoryStorePolicy{edgePath: artifactPath, maxEdges: 10}}
	assertNoCanaryPath(t, memory.memoryGraphEdgeStoreInfo(), artifactPath)
}

func TestRecallCaseRouteReturnsOpaqueCaseSetID(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "recall-cases.json")
	if err := os.WriteFile(path, []byte(`{"version":"test","updatedAt":"2026-07-13T00:00:00Z","k":5,"cases":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCH_RECALL_EVAL_CASES_PATH", path)
	s := newTestServer(t, "http://127.0.0.1:1")
	recorder := httptest.NewRecorder()
	s.memoryRecallEvalCases(recorder, httptest.NewRequest(http.MethodGet, "/memory/recall/eval-cases", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("recall case route returned %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), path) || strings.Contains(recorder.Body.String(), `"path"`) {
		t.Fatalf("recall case route leaked storage topology: %s", recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if anyToString(payload["case_set_id"]) == "" {
		t.Fatalf("recall case route missing opaque case_set_id: %#v", payload)
	}
	assertMode(t, path, 0o600)
}

func TestRecallCaseRouteRedactsStorePreparationErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional Windows privileges")
	}
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.json")
	path := filepath.Join(root, "recall-cases.json")
	if err := os.WriteFile(outside, []byte(`{"cases":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCH_RECALL_EVAL_CASES_PATH", path)
	s := newTestServer(t, "http://127.0.0.1:1")
	recorder := httptest.NewRecorder()
	s.memoryRecallEvalCases(recorder, httptest.NewRequest(http.MethodGet, "/memory/recall/eval-cases", nil))
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("expected redacted storage failure, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), path) || strings.Contains(recorder.Body.String(), outside) {
		t.Fatalf("recall failure leaked storage topology: %s", recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"code":"storage_access_error"`) {
		t.Fatalf("recall failure missing stable error code: %s", recorder.Body.String())
	}
}
