package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	testRoot, err := os.MkdirTemp("", "contextlattice-gateway-tests-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create gateway test root: %v\n", err)
		os.Exit(1)
	}
	runtimePaths := map[string]string{
		"GO_MEMORY_STORE_ROOT":                           filepath.Join(testRoot, "memory_store"),
		"GO_AGENT_SESSIONS_PATH":                         filepath.Join(testRoot, "agent_sessions.json"),
		"GO_AGENT_TASKS_PATH":                            filepath.Join(testRoot, "agent_tasks.json"),
		"FEEDBACK_HISTORY_PATH":                          filepath.Join(testRoot, "feedback.ndjson"),
		"GO_RETRIEVAL_CONTINUATION_DURABLE_DIR":          filepath.Join(testRoot, "continuation_outbox"),
		"CONTEXTLATTICE_TEMPORAL_CLAIMS_PATH":            filepath.Join(testRoot, "temporal_claims.ndjson"),
		"CONTEXTLATTICE_CONTEXT_POLICY_PATH":             filepath.Join(testRoot, "context_policy.ndjson"),
		"CONTEXTLATTICE_SKILL_FOUNDRY_PATH":              filepath.Join(testRoot, "skill_foundry.ndjson"),
		"CONTEXTLATTICE_CONTEXT_PASSPORT_PATH":           filepath.Join(testRoot, "context_passports.ndjson"),
		"CONTEXTLATTICE_CONTEXT_IDENTITY_KEY_PATH":       filepath.Join(testRoot, "context_identity_keys.json"),
		"CONTEXTLATTICE_CONTEXT_MESH_STATE_PATH":         filepath.Join(testRoot, "context_mesh_state.json"),
		"CONTEXTLATTICE_COGNITION_ACTIVATION_STATE_PATH": filepath.Join(testRoot, "cognition_activation.json"),
		"CONTEXTLATTICE_SKILL_ACTIVATION_ROOT":           filepath.Join(testRoot, "skills"),
		"GO_TELEMETRY_SPOOL_PATH":                        filepath.Join(testRoot, "telemetry_spool.ndjson"),
		"AGENT_MEMORY_PROFILE_PATH":                      filepath.Join(testRoot, "agent_memory_profiles.json"),
		"ORCH_RECALL_EVAL_CASES_PATH":                    filepath.Join(testRoot, "recall_eval_cases.json"),
		"RECALL_MONITOR_PATH":                            filepath.Join(testRoot, "recall_monitor.ndjson"),
		"TRADING_HISTORY_PATH":                           filepath.Join(testRoot, "trading_metrics.ndjson"),
		"ORCH_CONTINUITY_SNAPSHOT_DIR":                   filepath.Join(testRoot, "continuity_snapshots"),
		"ORCH_STORAGE_LEDGER_PATH":                       filepath.Join(testRoot, "storage_ledger.ndjson"),
	}
	for name, path := range runtimePaths {
		if err := os.Setenv(name, path); err != nil {
			fmt.Fprintf(os.Stderr, "set gateway test path %s: %v\n", name, err)
			os.Exit(1)
		}
	}
	// Most unit tests require immediately writable fixture stores. Production
	// keeps background hydration enabled by default; focused startup tests opt
	// back into that contract explicitly.
	if err := os.Setenv("GO_MEMORY_STORE_BACKGROUND_HYDRATION_ENABLED", "false"); err != nil {
		fmt.Fprintf(os.Stderr, "set gateway test hydration mode: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()
	if err := os.RemoveAll(testRoot); err != nil && code == 0 {
		fmt.Fprintf(os.Stderr, "remove gateway test root: %v\n", err)
		code = 1
	}
	os.Exit(code)
}
