package main

import (
	"path/filepath"
	"testing"

	"github.com/contextlattice/gateway-go/internal/gatewaystate"
)

func TestPublicGatewayStateInventoryExcludesCommercialOnlyOwners(t *testing.T) {
	entries := gatewayStateInventoryEntries()
	if len(entries) != 33 {
		t.Fatalf("public gateway state inventory count=%d, want 33", len(entries))
	}
	commercialOnly := map[string]bool{
		"agent_tasks": true, "cognition_activation": true, "continuity_snapshots": true,
		"frontier_t1_governance": true, "frontier_t2_packet_retention": true,
		"frontier_t2_shared_proof": true, "frontier_t4_retrieval_governance": true,
		"frontier_t5_policy_governance": true, "frontier_t6_agent_fit_governance": true,
		"frontier_t7_portable_governance": true, "frontier_t8_skill_evolution_governance": true,
		"frontier_t9_continuity_zero_governance": true, "frontier_t10_aggregate_governance": true,
		"machine_binding": true,
	}
	for _, entry := range entries {
		if commercialOnly[entry.ID] {
			t.Fatalf("public inventory leaked commercial-only state owner %q", entry.ID)
		}
	}
}

func TestCanonicalGatewayStateSurvivesStoreRestart(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "gateway-state")
	t.Setenv(gatewaystate.RootEnv, stateRoot)
	for _, name := range []string{
		"GO_MEMORY_STORE_ROOT",
		"MEMORY_BANK_ROOT",
		"FEEDBACK_HISTORY_PATH",
		"TASK_DB_PATH",
		"GO_AGENT_SESSIONS_PATH",
		"GO_AGENT_SESSION_LEDGER_PATH",
		"CONTEXTLATTICE_CONTINUITY_LEDGER_PATH",
		"GO_RETRIEVAL_CONTINUATION_DURABLE_DIR",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("CONTEXTLATTICE_CONTINUITY_ENABLED", "true")
	t.Setenv("CONTEXTLATTICE_CONTINUITY_LEDGER_FSYNC", "false")
	t.Setenv("GO_RETRIEVAL_CONTINUATION_DURABLE_ENABLED", "true")
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_BACKGROUND_ENABLED", "false")
	if _, err := gatewaystate.EnsureRoot(); err != nil {
		t.Fatalf("ensure canonical state root: %v", err)
	}

	feedback, err := newFeedbackStoreFromEnv()
	if err != nil {
		t.Fatalf("open feedback store: %v", err)
	}
	if feedback.path != filepath.Join(stateRoot, "feedback_records.ndjson") {
		t.Fatalf("feedback escaped canonical root: %s", feedback.path)
	}
	if err := feedback.append(map[string]any{"id": "feedback-restart", "project": "contextlattice", "rating": "useful"}); err != nil {
		t.Fatalf("append feedback: %v", err)
	}

	sessions, err := newAgentSessionStoreFromEnv()
	if err != nil {
		t.Fatalf("open session store: %v", err)
	}
	if sessions.path != filepath.Join(stateRoot, "agent_sessions.json") {
		t.Fatalf("sessions escaped canonical root: %s", sessions.path)
	}
	if _, err := sessions.start(map[string]any{
		"session_id": "sess_gateway_state_restart",
		"agent":      "codex",
		"project":    "contextlattice",
		"objective":  "prove canonical state restart",
	}); err != nil {
		t.Fatalf("start session: %v", err)
	}

	continuity, err := newContinuityStoreFromEnv()
	if err != nil {
		t.Fatalf("open continuity store: %v", err)
	}
	if continuity.path != filepath.Join(stateRoot, "continuity_ledger.ndjson") {
		continuity.close()
		t.Fatalf("continuity escaped canonical root: %s", continuity.path)
	}
	identity, err := continuity.reconcile(map[string]any{
		"project": "contextlattice", "repo": "contextlattice", "task_id": "gateway-state-restart",
		"objective": "prove canonical state restart", "agent_id": "codex",
	}, true)
	if err != nil {
		continuity.close()
		t.Fatalf("persist continuity identity: %v", err)
	}
	identityID := anyToString(identity["task_identity_id"])
	continuity.close()

	policy := loadRetrievalPolicy()
	if policy.continuationDurableDir != filepath.Join(stateRoot, "continuation_outbox") {
		t.Fatalf("continuation queue escaped canonical root: %s", policy.continuationDurableDir)
	}
	queue := newContinuationDurableQueue(policy)
	jobID, _, err := queue.enqueue(
		"qdrant",
		"restart persistence test",
		"stream-gateway-state-restart",
		map[string]any{"query": "canonical restart marker", "project": "contextlattice"},
		nil,
		"queued",
	)
	if err != nil {
		t.Fatalf("enqueue continuation job: %v", err)
	}

	restartedFeedback, err := newFeedbackStoreFromEnv()
	if err != nil || len(restartedFeedback.list("contextlattice", "", "", 10)) != 1 {
		t.Fatalf("feedback did not survive restart: records=%v err=%v", restartedFeedback.records, err)
	}
	restartedSessions, err := newAgentSessionStoreFromEnv()
	if err != nil {
		t.Fatalf("restart session store: %v", err)
	}
	if _, _, exists := restartedSessions.get("sess_gateway_state_restart"); !exists {
		t.Fatal("agent session did not survive restart")
	}
	restartedContinuity, err := newContinuityStoreFromEnv()
	if err != nil {
		t.Fatalf("restart continuity store: %v", err)
	}
	defer restartedContinuity.close()
	resolvedIdentity, err := restartedContinuity.reconcile(map[string]any{
		"project": "contextlattice", "repo": "contextlattice", "task_identity_id": identityID,
	}, false)
	if err != nil || anyToString(resolvedIdentity["task_identity_id"]) != identityID {
		t.Fatalf("continuity identity did not survive restart: result=%#v err=%v", resolvedIdentity, err)
	}
	restartedQueue := newContinuationDurableQueue(policy)
	restartedSnapshot := restartedQueue.snapshot()
	if restartedSnapshot.Pending != 1 {
		t.Fatalf("continuation job did not survive restart: job=%s snapshot=%#v", jobID, restartedSnapshot)
	}
}
