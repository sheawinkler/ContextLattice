package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func newAgentTaskRoutingTestStore(t *testing.T) *agentTaskStore {
	t.Helper()
	return &agentTaskStore{
		path: filepath.Join(t.TempDir(), "agent_tasks.json"), leaseTTL: time.Minute,
		tasks: map[string]map[string]any{}, order: []string{}, events: map[string][]map[string]any{},
	}
}

func TestAgentTaskClaimWorkerResolution(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		bodyWorker string
		want       string
		wantErr    bool
	}{
		{name: "query", path: "/agents/tasks/next?worker=query-worker", want: "query-worker"},
		{name: "body", path: "/agents/tasks/next", bodyWorker: "body-worker", want: "body-worker"},
		{name: "matching query and body", path: "/agents/tasks/next?worker=same-worker", bodyWorker: "same-worker", want: "same-worker"},
		{name: "conflicting query and body", path: "/agents/tasks/next?worker=query-worker", bodyWorker: "body-worker", wantErr: true},
		{name: "internal fallback", path: "/agents/tasks/next", want: defaultAgentTaskWorker},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, nil)
			payload := map[string]any{}
			if test.bodyWorker != "" {
				payload["worker"] = test.bodyWorker
			}
			worker, err := resolveAgentTaskClaimWorker(request, payload)
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected worker identity conflict, got worker %q", worker)
				}
				return
			}
			if err != nil || worker != test.want {
				t.Fatalf("resolved worker = %q, err=%v; want %q", worker, err, test.want)
			}
		})
	}
}

func TestAgentTaskSQLiteClaimFiltersByWorkerBeforePriorityAndPersistsExactIdentity(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	for _, item := range []struct {
		id       string
		worker   string
		priority int
	}{
		{id: "codex-high", worker: "codex", priority: 100},
		{id: "hermes-match", worker: "hermes-agent", priority: 10},
		{id: "generic", worker: "", priority: 5},
	} {
		manifest := testAgentTaskManifest(item.id, "routing-project", "routing-reviewer", "sess_"+item.id)
		manifest["priority"] = item.priority
		manifest["metadata"] = map[string]any{"worker": item.worker}
		if _, err := ledger.submit(context.Background(), manifest); err != nil {
			t.Fatalf("submit %s: %v", item.id, err)
		}
	}
	claim, err := ledger.claimNext(context.Background(), "hermes-agent", "hermes-instance-a", "")
	if err != nil || claim == nil {
		t.Fatalf("claim matching task: row=%#v err=%v", claim, err)
	}
	if got := anyToString(anyMap(claim["task"])["task_id"]); got != "hermes-match" {
		t.Fatalf("worker claimed %q; want hermes-match", got)
	}
	attempt := anyMap(claim["attempt"])
	if anyToString(attempt["worker_id"]) != "hermes-agent" || anyToString(attempt["worker_instance_id"]) != "hermes-instance-a" || anyToInt(attempt["generation"], 0) != 1 {
		t.Fatalf("claim lost exact worker identity or fence: %#v", attempt)
	}
	queued, err := ledger.queryTask(context.Background(), "codex-high")
	if err != nil || anyToString(queued["status"]) != "queued" {
		t.Fatalf("foreign higher-priority task changed: %#v err=%v", queued, err)
	}
}

func TestAgentTaskLegacyArchiveMutationsAndLiveFallbackFailClosed(t *testing.T) {
	archive := newAgentTaskRoutingTestStore(t)
	if _, err := archive.submit(map[string]any{"title": "must not persist"}); !errors.Is(err, errLegacyAgentTaskMutationDisabled) {
		t.Fatalf("legacy submit did not fail closed: %v", err)
	}
	if _, err := archive.claimNext("worker"); !errors.Is(err, errLegacyAgentTaskMutationDisabled) {
		t.Fatalf("legacy claim did not fail closed: %v", err)
	}
	if _, err := archive.updateStatus("task", "succeeded", "", nil); !errors.Is(err, errLegacyAgentTaskMutationDisabled) {
		t.Fatalf("legacy status mutation did not fail closed: %v", err)
	}
	if _, err := archive.approve("task", "reviewer", ""); !errors.Is(err, errLegacyAgentTaskMutationDisabled) {
		t.Fatalf("legacy approval did not fail closed: %v", err)
	}
	if _, err := archive.replay("task", "reviewer", "", false); !errors.Is(err, errLegacyAgentTaskMutationDisabled) {
		t.Fatalf("legacy replay did not fail closed: %v", err)
	}
	if _, err := archive.recoverExpired(10); !errors.Is(err, errLegacyAgentTaskMutationDisabled) {
		t.Fatalf("legacy recovery did not fail closed: %v", err)
	}
	if matches, err := filepath.Glob(archive.path); err != nil {
		t.Fatal(err)
	} else if len(matches) != 0 {
		t.Fatalf("disabled legacy authority wrote archive path %s", archive.path)
	}

	server := &server{taskStore: archive}
	response := httptest.NewRecorder()
	server.agentsTasksRoute(response, httptest.NewRequest(http.MethodGet, "/agents/tasks", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("live route fell back to archive: code=%d body=%s", response.Code, response.Body.String())
	}
}
