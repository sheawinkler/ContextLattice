package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	defaultAgentTasksPathRel  = ".data/orchestrator/agent_tasks.json"
	defaultAgentTaskLeaseSecs = 180
	defaultAgentTaskWorker    = "gateway-worker"
)

// agentTaskStore is retained only as the shape of the former JSON migration
// archive. No live route selects it, and every mutation fails closed.
type agentTaskStore struct {
	mu       sync.Mutex
	path     string
	leaseTTL time.Duration
	tasks    map[string]map[string]any
	order    []string
	events   map[string][]map[string]any
}

var errLegacyAgentTaskMutationDisabled = errors.New("legacy task authority is disabled; use Gateway SQLite-WAL task delivery")

func agentTasksPath() string {
	if path := strings.TrimSpace(os.Getenv("GO_AGENT_TASKS_PATH")); path != "" {
		return filepath.Clean(path)
	}
	if trackedPath := strings.TrimSpace(os.Getenv("TASK_DB_PATH")); trackedPath != "" {
		base := strings.TrimSuffix(trackedPath, filepath.Ext(trackedPath))
		if base != "" {
			return filepath.Clean(base + ".json")
		}
	}
	return resolveStoragePath("", defaultAgentTasksPathRel)
}

func (s *agentTaskStore) submit(map[string]any) (map[string]any, error) {
	return nil, errLegacyAgentTaskMutationDisabled
}

func (s *agentTaskStore) claimNext(string) (map[string]any, error) {
	return nil, errLegacyAgentTaskMutationDisabled
}

func (s *agentTaskStore) updateStatus(string, string, string, map[string]any) (map[string]any, error) {
	return nil, errLegacyAgentTaskMutationDisabled
}

func (s *agentTaskStore) approve(string, string, string) (map[string]any, error) {
	return nil, errLegacyAgentTaskMutationDisabled
}

func (s *agentTaskStore) replay(string, string, string, bool) (map[string]any, error) {
	return nil, errLegacyAgentTaskMutationDisabled
}

func (s *agentTaskStore) recoverExpired(int) (int, error) {
	return 0, errLegacyAgentTaskMutationDisabled
}

func resolveAgentTaskClaimWorker(r *http.Request, payload map[string]any) (string, error) {
	queryWorker := strings.TrimSpace(r.URL.Query().Get("worker"))
	bodyWorker := strings.TrimSpace(anyToString(payload["worker"]))
	if queryWorker != "" && bodyWorker != "" && queryWorker != bodyWorker {
		return "", errors.New("worker identity conflicts between query parameter and json body")
	}
	if queryWorker != "" {
		return queryWorker, nil
	}
	if bodyWorker != "" {
		return bodyWorker, nil
	}
	return defaultAgentTaskWorker, nil
}
