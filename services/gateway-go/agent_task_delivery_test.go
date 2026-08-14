package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testAgentTaskLedger(t *testing.T) *agentTaskDeliveryLedger {
	t.Helper()
	root := t.TempDir()
	t.Setenv("GO_AGENT_TASK_LEDGER_PATH", filepath.Join(root, "agent_tasks.sqlite3"))
	t.Setenv("GO_AGENT_TASK_ARTIFACT_DIR", filepath.Join(root, "artifacts"))
	t.Setenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH", "")
	ledger, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatalf("new task ledger: %v", err)
	}
	t.Cleanup(func() { _ = ledger.close() })
	return ledger
}

func testAgentTaskManifest(id, project, owner, sessionID string) map[string]any {
	return map[string]any{
		"schema_id": "agent_task_manifest.v1", "contract_version": 1,
		"task_id": id, "project": project, "objective": "exercise durable delivery",
		"acceptance_criteria": []any{"durable result is staged"}, "task_class": "non_coding",
		"execution_profile": "local-default", "risk_level": "low",
		"approval_policy": map[string]any{"required": false},
		"context_request": map[string]any{"content_hash": "sha256:" + strings.Repeat("a", 64), "session_id": sessionID, "topic_path": "tasks/durable"},
		"recipients":      []any{map[string]any{"principal_id": owner, "role": "reviewer", "project": project, "observer": false, "session_id": sessionID}},
		"review_owner":    owner, "idempotency_key": "task-idempotency:" + id, "status": "queued",
		"requesting_agent_id": owner, "workspace_id": "workspace-" + project,
	}
}

func testAgentTaskStagePublication(t *testing.T, ledger *agentTaskDeliveryLedger, id, project, owner, sessionID string, artifacts []any) (agentTaskFence, map[string]any) {
	t.Helper()
	manifest := testAgentTaskManifest(id, project, owner, sessionID)
	if _, err := ledger.submit(context.Background(), manifest); err != nil {
		t.Fatalf("submit task: %v", err)
	}
	claim, err := ledger.claimNext(context.Background(), "worker-"+id, "instance-"+id, "")
	if err != nil || claim == nil {
		t.Fatalf("claim task: row=%#v err=%v", claim, err)
	}
	fence := testAgentTaskFenceFromClaim(t, claim)
	if _, err := ledger.heartbeat(context.Background(), fence); err != nil {
		t.Fatalf("start running attempt: %v", err)
	}
	exitCode := 0
	if _, err := ledger.observe(context.Background(), fence, "succeeded", &exitCode, map[string]any{"source": "test"}); err != nil {
		t.Fatalf("observe execution: %v", err)
	}
	resultID := "result-" + id
	publicationID := "publication-" + id
	workspaceRef := "workspace-ref-" + id
	publication, err := ledger.stagePublication(context.Background(), fence, map[string]any{
		"publication_id": publicationID, "idempotency_key": "task-result:" + resultID, "runner_exit_required": true,
		"result": map[string]any{
			"result_id": resultID, "summary": "durable result " + id, "output": "verified output",
			"context_pack_hash": anyToString(anyMap(claim["attempt"])["context_pack_hash"]),
			"workspace":         map[string]any{"workspace_ref": workspaceRef},
			"cleanup":           map[string]any{"cleanup_id": agentTaskCleanupID(fence.TaskID, fence.AttemptID, workspaceRef)},
		},
		"artifacts": artifacts,
	})
	if err != nil {
		t.Fatalf("stage publication: %v", err)
	}
	return fence, publication
}

func testAgentTaskServerWithMemory(t *testing.T, ledger *agentTaskDeliveryLedger, project, owner, sessionID string) (*server, *memoryStore, *agentSessionStore) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "memory")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", filepath.Join(root, "_contextlattice", "history.ndjson"))
	t.Setenv("GO_MEMORY_STORE_ACCESS_LOG_PATH", filepath.Join(root, "_contextlattice", "access.ndjson"))
	t.Setenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH", filepath.Join(root, "_contextlattice", "objects"))
	t.Setenv("GO_MEMORY_STORE_BACKGROUND_HYDRATION_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_BACKGROUND_ENABLED", "false")
	memory, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("new memory store: %v", err)
	}
	sessions := &agentSessionStore{path: filepath.Join(t.TempDir(), "sessions.json"), maxKeep: 16, maxEvents: 64, idleTTL: 3600, sessions: map[string]map[string]any{}, events: map[string][]map[string]any{}}
	if _, _, err := sessions.startOrReuse(map[string]any{"session_id": sessionID, "agent": "test", "agent_id": owner, "project": project, "objective": "receive durable task result"}); err != nil {
		t.Fatalf("start recipient session: %v", err)
	}
	resolveWorkspace := func(candidate string) (string, error) {
		if !strings.EqualFold(strings.TrimSpace(candidate), project) {
			return "", errors.New("task project is not actively bound to a workspace")
		}
		return "workspace-" + project, nil
	}
	return &server{taskLedger: ledger, memoryStore: memory, writePolicy: loadWriteIngressPolicy(), agentSessions: sessions, taskProjectWorkspace: resolveWorkspace}, memory, sessions
}

func testAgentTaskFenceFromClaim(t *testing.T, claim map[string]any) agentTaskFence {
	t.Helper()
	attempt := anyMap(claim["attempt"])
	lease := anyMap(claim["lease"])
	return agentTaskFence{
		TaskID: anyToString(lease["task_id"]), AttemptID: anyToString(lease["attempt_id"]), LeaseID: anyToString(lease["lease_id"]),
		WorkerID: anyToString(lease["worker_id"]), WorkerInstanceID: anyToString(lease["worker_instance_id"]),
		Generation: anyToInt(firstNonEmptyStrings(anyToString(lease["generation"]), anyToString(attempt["generation"])), 0),
	}
}

func TestAgentTaskLifecycleKeepsExecutionSucceededReviewable(t *testing.T) {
	if agentTaskTerminal("execution_succeeded") {
		t.Fatal("execution_succeeded must remain nonterminal while review/integration follows")
	}
	if !agentTaskAllowedTransition("execution_succeeded", "review_pending") {
		t.Fatal("execution_succeeded must transition into explicit review")
	}
	if agentTaskAllowedTransition("review_pending", "execution_succeeded") {
		t.Fatal("review must not roll a task back into execution_succeeded")
	}
}

func TestAgentTaskTwoSQLiteHandlesFenceClaimAndIdempotency(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "agent_tasks.sqlite3")
	artifactPath := filepath.Join(root, "artifacts")
	t.Setenv("GO_AGENT_TASK_LEDGER_PATH", dbPath)
	t.Setenv("GO_AGENT_TASK_ARTIFACT_DIR", artifactPath)
	t.Setenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH", "")
	first, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer first.close()
	second, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer second.close()
	manifest := testAgentTaskManifest(t.Name(), "race-project", "reviewer", "sess_race")
	var wg sync.WaitGroup
	results := make(chan map[string]any, 2)
	errorsCh := make(chan error, 2)
	for _, ledger := range []*agentTaskDeliveryLedger{first, second} {
		wg.Add(1)
		go func(l *agentTaskDeliveryLedger) {
			defer wg.Done()
			row, submitErr := l.submit(context.Background(), manifest)
			if submitErr != nil {
				errorsCh <- submitErr
				return
			}
			results <- row
		}(ledger)
	}
	wg.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		t.Fatalf("same idempotency key did not converge: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected two idempotent responses, got %d", len(results))
	}
	if row, claimErr := first.claimNext(context.Background(), "shared-logical-worker", "", ""); claimErr == nil || row != nil || !strings.Contains(claimErr.Error(), "worker_instance_id") {
		t.Fatalf("claim without exact worker instance was not rejected: row=%#v err=%v", row, claimErr)
	}
	claimed := make(chan map[string]any, 2)
	for _, item := range []struct {
		ledger *agentTaskDeliveryLedger
		worker string
	}{
		{first, "worker-a"}, {second, "worker-b"},
	} {
		wg.Add(1)
		go func(item struct {
			ledger *agentTaskDeliveryLedger
			worker string
		}) {
			defer wg.Done()
			row, claimErr := item.ledger.claimNext(context.Background(), item.worker, item.worker+"-instance", "")
			if claimErr == nil && row != nil {
				claimed <- row
			}
		}(item)
	}
	wg.Wait()
	close(claimed)
	if len(claimed) != 1 {
		t.Fatalf("immediate transaction claim fence allowed %d claimants", len(claimed))
	}
	for row := range claimed {
		fence := testAgentTaskFenceFromClaim(t, row)
		attempt, lease := anyMap(row["attempt"]), anyMap(row["lease"])
		if fence.Generation != 1 || fence.WorkerID == "" || fence.WorkerInstanceID == "" {
			t.Fatalf("incomplete claim fence: %#v", fence)
		}
		if anyToInt(attempt["assignment_generation"], 0) != fence.Generation || anyToInt(attempt["lease_generation"], 0) != fence.Generation || anyToInt(attempt["worker_identity_update_generation"], -1) != 0 || anyToInt(lease["assignment_generation"], 0) != fence.Generation || anyToInt(lease["lease_generation"], 0) != fence.Generation {
			t.Fatalf("claim did not preserve distinct assignment/lease/identity-update generations: attempt=%#v lease=%#v", attempt, lease)
		}
		if _, heartbeatErr := first.heartbeat(context.Background(), fence); heartbeatErr != nil {
			t.Fatalf("claim heartbeat: %v", heartbeatErr)
		}
		stale := fence
		stale.WorkerInstanceID = "wrong-instance"
		if _, heartbeatErr := second.heartbeat(context.Background(), stale); heartbeatErr == nil || !strings.Contains(heartbeatErr.Error(), "stale_lease_fence") {
			t.Fatalf("stale instance fence was accepted: %v", heartbeatErr)
		}
	}
}

func TestAgentTaskMemoryBlobSiblingIsNeverReconciled(t *testing.T) {
	root := t.TempDir()
	memoryRoot := filepath.Join(root, "memory-content")
	if err := os.MkdirAll(memoryRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(memoryRoot, "ordinary.blob")
	original := []byte("ordinary memory content")
	if err := os.WriteFile(sibling, original, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH", memoryRoot)
	t.Setenv("GO_AGENT_TASK_LEDGER_PATH", filepath.Join(root, "agent_tasks.sqlite3"))
	t.Setenv("GO_AGENT_TASK_ARTIFACT_DIR", "")
	ledger, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.close()
	got, err := os.ReadFile(sibling)
	if err != nil || string(got) != string(original) {
		t.Fatalf("ordinary sibling content was touched during task namespace reconciliation: err=%v content=%q", err, got)
	}
	if filepath.Clean(ledger.artifactRoot) == filepath.Clean(memoryRoot) {
		t.Fatal("task artifact root shared the ordinary memory blob root")
	}
}

func TestAgentTaskProjectionReachesSteeringInboxAndDedupes(t *testing.T) {
	root := t.TempDir()
	store := &agentSessionStore{path: filepath.Join(root, "sessions.json"), maxKeep: 16, maxEvents: 32, idleTTL: 3600, sessions: map[string]map[string]any{}, events: map[string][]map[string]any{}}
	sessionID := "sess_delivery_inbox"
	if _, _, err := store.startOrReuse(map[string]any{"session_id": sessionID, "agent": "test", "agent_id": "reviewer", "project": "delivery-project", "objective": "receive delivery"}); err != nil {
		t.Fatal(err)
	}
	server := &server{agentSessions: store}
	delivery := map[string]any{
		"task_id": "task-inbox", "result_id": "result-inbox", "delivery_id": "delivery-inbox",
		"publication_id": "publication-inbox", "session_id": sessionID, "project": "delivery-project",
		"dedupe_key": "result-inbox:reviewer", "summary": "A durable result is ready.",
		"recipient": map[string]any{"principal_id": "reviewer", "role": "reviewer", "session_id": sessionID},
	}
	if !server.projectTaskDeliveryDurably(context.Background(), delivery) {
		t.Fatal("durable delivery projection failed")
	}
	if !server.projectTaskDeliveryDurably(context.Background(), delivery) {
		t.Fatal("idempotent delivery projection retry failed")
	}
	_, events, exists := store.get(sessionID)
	if !exists {
		t.Fatal("projected session does not exist")
	}
	inbox := agentSessionSteeringInbox(sessionID, events)
	if anyToInt(inbox["pending_count"], 0) != 1 || strings.TrimSpace(anyToString(anyMap(inbox["latest"])["message"])) == "" {
		t.Fatalf("projected delivery did not reach actual steering inbox: %#v", inbox)
	}
	misbound := cloneAnyMap(delivery)
	misbound["delivery_id"] = "delivery-misbound"
	misbound["dedupe_key"] = "result-inbox:different-principal"
	misbound["recipient"] = map[string]any{"principal_id": "different-principal", "role": "reviewer", "project": "delivery-project", "session_id": sessionID}
	if server.projectTaskDeliveryDurably(context.Background(), misbound) {
		t.Fatal("delivery projected into a session owned by a different principal")
	}
	crossProject := cloneAnyMap(delivery)
	crossProject["delivery_id"] = "delivery-cross-project"
	crossProject["dedupe_key"] = "result-inbox:cross-project"
	crossProject["recipient"] = map[string]any{"principal_id": "reviewer", "role": "reviewer", "project": "different-project", "session_id": sessionID}
	if server.projectTaskDeliveryDurably(context.Background(), crossProject) {
		t.Fatal("delivery projected into a session owned by a different project")
	}
}

func TestAgentTaskProjectionFaultIsRetryable(t *testing.T) {
	root := t.TempDir()
	store := &agentSessionStore{path: filepath.Join(root, "sessions.json"), maxKeep: 16, maxEvents: 32, idleTTL: 3600, sessions: map[string]map[string]any{}, events: map[string][]map[string]any{}}
	sessionID := "sess_delivery_fault"
	if _, _, err := store.startOrReuse(map[string]any{"session_id": sessionID, "agent": "test", "agent_id": "reviewer", "project": "delivery-project", "objective": "receive delivery"}); err != nil {
		t.Fatal(err)
	}
	server := &server{agentSessions: store, taskDeliveryProjectionFault: func() error { return os.ErrPermission }}
	delivery := map[string]any{
		"task_id": "task-fault", "result_id": "result-fault", "delivery_id": "delivery-fault",
		"publication_id": "publication-fault", "session_id": sessionID, "project": "delivery-project",
		"dedupe_key": "result-fault:reviewer", "summary": "Retryable result.",
		"recipient": map[string]any{"principal_id": "reviewer", "role": "reviewer", "session_id": sessionID},
	}
	if server.projectTaskDeliveryDurably(context.Background(), delivery) {
		t.Fatal("fault-injected session projection unexpectedly succeeded")
	}
	server.taskDeliveryProjectionFault = nil
	if !server.projectTaskDeliveryDurably(context.Background(), delivery) {
		t.Fatal("delivery projection did not recover after injected failure")
	}
}

func TestAgentTaskMemoryPutThenSessionFailureRetriesWithoutDuplicateAndDrainsSteeringInbox(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	project, owner, sessionID := "writeback-project", "reviewer-writeback", "sess_writeback_retry"
	fence, publication := testAgentTaskStagePublication(t, ledger, t.Name(), project, owner, sessionID, nil)
	server, memory, sessions := testAgentTaskServerWithMemory(t, ledger, project, owner, sessionID)
	failSessionOnce := true
	server.taskSessionEventFault = func(eventType string) error {
		if failSessionOnce && eventType == agentTaskWritebackEventType {
			failSessionOnce = false
			return errors.New("injected session event failure after memory put")
		}
		return nil
	}
	publicationID := anyToString(publication["publication_id"])
	first, err := server.runTaskPublicationWorker(context.Background(), publicationID)
	if err != nil {
		t.Fatalf("first publication attempt should durably record a retryable failure: %v", err)
	}
	if got := anyToString(first["status"]); got != "writeback_failed" {
		t.Fatalf("first publication status=%q, want writeback_failed", got)
	}
	key := memoryStoreKey(project, anyToString(anyMap(first["writeback_intent"])["file_name"]))
	memory.mu.RLock()
	stored, exists := memory.currentState[key]
	memory.mu.RUnlock()
	if !exists {
		t.Fatal("memoryStore.put did not create the required durable writeback record")
	}
	attribution := stored.Entry.TaskAttribution
	if anyToString(attribution["worker_id"]) != fence.WorkerID || anyToString(attribution["worker_instance_id"]) != fence.WorkerInstanceID || anyToInt(attribution["lease_generation"], 0) != fence.Generation || anyToString(attribution["requesting_agent_id"]) != owner || anyToString(attribution["review_agent_id"]) != owner {
		t.Fatalf("memory writeback erased fenced worker/request provenance: %#v", attribution)
	}
	server.taskSessionEventFault = nil
	committed, err := server.runTaskPublicationWorker(context.Background(), publicationID)
	if err != nil {
		t.Fatalf("publication retry: %v", err)
	}
	if got := anyToString(committed["status"]); got != "committed" {
		t.Fatalf("publication status=%q, want committed", got)
	}
	history, err := os.ReadFile(memory.policy.historyPath)
	if err != nil {
		t.Fatalf("read memory history: %v", err)
	}
	if got := countNonEmptyLines(string(history)); got != 1 {
		t.Fatalf("memory put retry created %d history records, want exactly 1", got)
	}
	_, events, exists := sessions.get(sessionID)
	if !exists {
		t.Fatal("recipient session disappeared")
	}
	writebacks, deliveries := 0, 0
	for _, event := range events {
		switch anyToString(event["type"]) {
		case agentTaskWritebackEventType:
			writebacks++
			writeback := anyMap(anyMap(event["metadata"])["writeback"])
			receipt := anyMap(writeback["memory_receipt"])
			if anyToString(receipt["worker_instance_id"]) != fence.WorkerInstanceID || anyToInt(receipt["assignment_generation"], 0) != fence.Generation {
				t.Fatalf("writeback receipt lost worker-instance attribution: %#v", receipt)
			}
		case agentTaskDeliveryEventType:
			deliveries++
		}
	}
	if writebacks != 1 || deliveries != 1 {
		t.Fatalf("session event dedupe failed: writebacks=%d deliveries=%d events=%#v", writebacks, deliveries, events)
	}
	inbox := agentSessionSteeringInbox(sessionID, events)
	if anyToInt(inbox["pending_count"], 0) != 1 || strings.TrimSpace(anyToString(anyMap(inbox["latest"])["message"])) == "" {
		t.Fatalf("delivery did not drain through metadata.steering_comment inbox: %#v", inbox)
	}
}

func TestAgentTaskCancellationRequiresExactFenceAndVerifiedTermination(t *testing.T) {
	t.Run("verified", func(t *testing.T) {
		ledger := testAgentTaskLedger(t)
		manifest := testAgentTaskManifest(t.Name(), "cancel-project", "reviewer", "sess_cancel")
		if _, err := ledger.submit(context.Background(), manifest); err != nil {
			t.Fatal(err)
		}
		claim, err := ledger.claimNext(context.Background(), "cancel-worker", "cancel-instance", "")
		if err != nil || claim == nil {
			t.Fatalf("claim: %#v %v", claim, err)
		}
		fence := testAgentTaskFenceFromClaim(t, claim)
		stale := fence
		stale.Generation++
		if _, err := ledger.cancelAttempt(context.Background(), stale, true, "cancel"); err == nil || !strings.Contains(err.Error(), "stale_lease_fence") {
			t.Fatalf("stale cancellation fence accepted: %v", err)
		}
		result, err := ledger.cancelAttempt(context.Background(), fence, true, "operator cancellation")
		if err != nil || anyToString(anyMap(result["task"])["status"]) != "canceled" {
			t.Fatalf("verified cancellation failed: %#v %v", result, err)
		}
	})
	t.Run("unverified-quarantines", func(t *testing.T) {
		ledger := testAgentTaskLedger(t)
		if _, err := ledger.submit(context.Background(), testAgentTaskManifest(t.Name(), "cancel-project", "reviewer", "sess_quarantine")); err != nil {
			t.Fatal(err)
		}
		claim, err := ledger.claimNext(context.Background(), "cancel-worker", "cancel-instance", "")
		if err != nil || claim == nil {
			t.Fatalf("claim: %#v %v", claim, err)
		}
		result, err := ledger.cancelAttempt(context.Background(), testAgentTaskFenceFromClaim(t, claim), false, "process group could not be verified")
		if err != nil || anyToString(anyMap(result["task"])["status"]) != "quarantined" {
			t.Fatalf("unverified cancellation did not quarantine: %#v %v", result, err)
		}
	})
}

func TestAgentTaskExpiredLeaseQuarantinesWithoutUnverifiedRequeue(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	manifest := testAgentTaskManifest(t.Name(), "expired-lease-project", "expired-lease-reviewer", "sess_expired_lease")
	if _, err := ledger.submit(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	claim, err := ledger.claimNext(context.Background(), "expired-worker", "expired-instance", "")
	if err != nil || claim == nil {
		t.Fatalf("claim: %#v %v", claim, err)
	}
	fence := testAgentTaskFenceFromClaim(t, claim)
	if _, err := ledger.db.ExecContext(context.Background(), `UPDATE task_ledger_attempts SET lease_expires_at=? WHERE attempt_id=?`, time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), fence.AttemptID); err != nil {
		t.Fatal(err)
	}
	recovered, err := ledger.recoverExpired(context.Background(), 10)
	if err != nil || len(recovered) != 1 || anyToString(recovered[0]["status"]) != "quarantined" || anyToBool(recovered[0]["termination_verified"]) {
		t.Fatalf("expired lease did not fail closed: recovered=%#v err=%v", recovered, err)
	}
	task, err := ledger.queryTask(context.Background(), t.Name())
	if err != nil || anyToString(task["status"]) != "quarantined" {
		t.Fatalf("expired task was requeued without termination proof: task=%#v err=%v", task, err)
	}
	if next, claimErr := ledger.claimNext(context.Background(), "replacement-worker", "replacement-instance", ""); claimErr != nil || next != nil {
		t.Fatalf("quarantined task was claimable without termination proof: next=%#v err=%v", next, claimErr)
	}
}

func TestAgentTaskArtifactIDCannotCrossTaskNamespaces(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	artifact := map[string]any{"artifact_id": "shared-worker-artifact-id", "name": "proof.txt", "content": "bounded proof", "media_type": "text/plain"}
	_, first := testAgentTaskStagePublication(t, ledger, "artifact-one", "artifact-project", "reviewer-one", "sess_artifact_one", []any{artifact})
	if _, err := ledger.finalizePublication(context.Background(), anyToString(first["publication_id"]), "committed", "receipt-one", ""); err != nil {
		t.Fatalf("finalize first artifact: %v", err)
	}
	if _, err := ledger.artifact(context.Background(), "shared-worker-artifact-id", "reviewer-one"); err != nil {
		t.Fatalf("authorized first-task artifact unavailable: %v", err)
	}
	manifest := testAgentTaskManifest("artifact-two", "artifact-project", "reviewer-two", "sess_artifact_two")
	if _, err := ledger.submit(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	claim, err := ledger.claimNext(context.Background(), "worker-two", "instance-two", "")
	if err != nil || claim == nil {
		t.Fatalf("claim second: %#v %v", claim, err)
	}
	fence := testAgentTaskFenceFromClaim(t, claim)
	if _, err := ledger.heartbeat(context.Background(), fence); err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	if _, err := ledger.observe(context.Background(), fence, "succeeded", &exitCode, nil); err != nil {
		t.Fatal(err)
	}
	_, err = ledger.stagePublication(context.Background(), fence, map[string]any{
		"publication_id": "publication-artifact-two", "runner_exit_required": true,
		"result": map[string]any{
			"result_id": "result-artifact-two", "summary": "second", "output": "second", "context_pack_hash": anyToString(anyMap(claim["attempt"])["context_pack_hash"]),
			"workspace": map[string]any{"workspace_ref": "workspace-ref-artifact-two"},
			"cleanup":   map[string]any{"cleanup_id": agentTaskCleanupID(fence.TaskID, fence.AttemptID, "workspace-ref-artifact-two")},
		},
		"artifacts": []any{artifact},
	})
	if err == nil || !strings.Contains(err.Error(), "different immutable artifact id") && !strings.Contains(err.Error(), "immutable artifact id already exists") && !strings.Contains(err.Error(), "no rows") {
		t.Fatalf("cross-task artifact id collision was accepted: %v", err)
	}
	if _, err := ledger.artifact(context.Background(), "shared-worker-artifact-id", "reviewer-two"); err == nil || !strings.Contains(err.Error(), "authorized") {
		t.Fatalf("foreign task recipient read artifact: %v", err)
	}
}

func TestAgentTaskRejectedSecretArtifactLeavesNoBlobOrLedgerResidue(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	manifest := testAgentTaskManifest(t.Name(), "artifact-secret-project", "artifact-secret-reviewer", "sess_artifact_secret")
	if _, err := ledger.submit(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	claim, err := ledger.claimNext(context.Background(), "artifact-worker", "artifact-instance", "")
	if err != nil || claim == nil {
		t.Fatalf("claim: %#v %v", claim, err)
	}
	fence := testAgentTaskFenceFromClaim(t, claim)
	if _, err := ledger.heartbeat(context.Background(), fence); err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	if _, err := ledger.observe(context.Background(), fence, "succeeded", &exitCode, nil); err != nil {
		t.Fatal(err)
	}
	cleanContent := "bounded clean evidence"
	cleanDigest := agentTaskBytesDigest([]byte(cleanContent))
	cleanPath, err := ledger.artifactPath(cleanDigest)
	if err != nil {
		t.Fatal(err)
	}
	secretContent := "-----BEGIN " + "PRIVATE KEY-----\nnot-a-real-key-but-must-never-persist\n-----END PRIVATE KEY-----"
	secretDigest := agentTaskBytesDigest([]byte(secretContent))
	secretPath, err := ledger.artifactPath(secretDigest)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ledger.stagePublication(context.Background(), fence, map[string]any{
		"publication_id": "publication-secret-rejected", "runner_exit_required": true,
		"result": map[string]any{
			"result_id": "result-secret-rejected", "summary": "must reject", "output": "must reject", "context_pack_hash": anyToString(anyMap(claim["attempt"])["context_pack_hash"]),
			"workspace": map[string]any{"workspace_ref": "workspace-ref-secret-rejected"},
			"cleanup":   map[string]any{"cleanup_id": agentTaskCleanupID(fence.TaskID, fence.AttemptID, "workspace-ref-secret-rejected")},
		},
		"artifacts": []any{
			map[string]any{"artifact_id": "artifact-clean-before-secret", "name": "clean.txt", "content": cleanContent, "digest": cleanDigest, "media_type": "text/plain"},
			map[string]any{"artifact_id": "artifact-secret-rejected", "name": "secret.pem", "content": secretContent, "digest": secretDigest, "media_type": "application/x-pem-file"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "canonical Gateway secret boundary") {
		t.Fatalf("secret artifact was not rejected by canonical scanner: %v", err)
	}
	if _, err := os.Lstat(secretPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected secret left a content-addressed blob: path=%s err=%v", secretPath, err)
	}
	if _, err := os.Lstat(cleanPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("later secret rejection left an earlier clean blob: path=%s err=%v", cleanPath, err)
	}
	entries, err := os.ReadDir(ledger.artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	entryNames := map[string]bool{}
	for _, entry := range entries {
		entryNames[entry.Name()] = true
	}
	if len(entries) != 2 || !entryNames[agentTaskArtifactNamespaceMarker] || !entryNames[agentTaskArtifactNamespaceLock] {
		t.Fatalf("rejected secret left artifact/orphan namespace residue: %#v", entries)
	}
	for _, query := range []string{
		`SELECT COUNT(*) FROM task_ledger_artifacts WHERE artifact_id='artifact-clean-before-secret'`,
		`SELECT COUNT(*) FROM task_ledger_artifacts WHERE artifact_id='artifact-secret-rejected'`,
		`SELECT COUNT(*) FROM task_ledger_results WHERE result_id='result-secret-rejected'`,
		`SELECT COUNT(*) FROM task_ledger_publications WHERE publication_id='publication-secret-rejected'`,
	} {
		var count int
		if err := ledger.db.QueryRowContext(context.Background(), query).Scan(&count); err != nil || count != 0 {
			t.Fatalf("rejected secret left ledger residue: query=%s count=%d err=%v", query, count, err)
		}
	}
}

func TestAgentTaskRecoveryWorkerStartStopOwnershipIsIdempotent(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	server := &server{taskLedger: ledger}
	server.startTaskDeliveryRecoveryWorker()
	server.taskRecoveryMu.Lock()
	firstDone := server.taskRecoveryDone
	server.taskRecoveryMu.Unlock()
	if firstDone == nil {
		t.Fatal("recovery worker did not establish owned lifecycle")
	}
	server.startTaskDeliveryRecoveryWorker()
	server.taskRecoveryMu.Lock()
	secondDone := server.taskRecoveryDone
	server.taskRecoveryMu.Unlock()
	if firstDone != secondDone {
		t.Fatal("duplicate recovery worker was started")
	}
	if err := server.closeTaskDeliveryRuntime(); err != nil {
		t.Fatalf("close recovery runtime: %v", err)
	}
	if err := server.closeTaskDeliveryRuntime(); err != nil {
		t.Fatalf("idempotent close recovery runtime: %v", err)
	}
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("owned recovery worker did not stop cleanly")
	}
	server.startTaskDeliveryRecoveryWorker()
	server.taskRecoveryMu.Lock()
	restarted := server.taskRecoveryDone
	server.taskRecoveryMu.Unlock()
	if restarted != nil {
		t.Fatal("closed recovery runtime restarted and accessed a closed ledger")
	}
}

func TestAgentTaskRestartRecoveryCommitsStagedPublication(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GO_AGENT_TASK_LEDGER_PATH", filepath.Join(root, "agent_tasks.sqlite3"))
	t.Setenv("GO_AGENT_TASK_ARTIFACT_DIR", filepath.Join(root, "artifacts"))
	t.Setenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH", "")
	ledger, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	project, owner, sessionID := "restart-project", "restart-reviewer", "sess_restart_recovery"
	_, publication := testAgentTaskStagePublication(t, ledger, t.Name(), project, owner, sessionID, nil)
	server, _, sessions := testAgentTaskServerWithMemory(t, ledger, project, owner, sessionID)
	if err := ledger.close(); err != nil {
		t.Fatalf("close pre-restart ledger: %v", err)
	}
	reopened, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatalf("reopen task ledger: %v", err)
	}
	t.Cleanup(func() { _ = reopened.close() })
	server.taskLedger = reopened
	server.reconcileTaskDeliveryOnce(context.Background())
	recovered, err := reopened.publication(context.Background(), anyToString(publication["publication_id"]))
	if err != nil || anyToString(recovered["status"]) != "committed" {
		t.Fatalf("restart reconciliation did not commit publication: %#v %v", recovered, err)
	}
	_, events, exists := sessions.get(sessionID)
	if !exists || anyToInt(agentSessionSteeringInbox(sessionID, events)["pending_count"], 0) != 1 {
		t.Fatalf("restart recovery did not project one steering delivery: exists=%v events=%#v", exists, events)
	}
}

func TestAgentTaskPublicationCrashBoundariesRecoverWithoutDuplicateWriteback(t *testing.T) {
	for _, stage := range []string{"after_claim", "before_memory_put", "after_memory_put", "after_session_event", "before_finalize", "after_finalize"} {
		t.Run(stage, func(t *testing.T) {
			ledger := testAgentTaskLedger(t)
			project, owner, sessionID := "publication-crash-project", "publication-crash-reviewer", "sess_pub_crash_"+stage
			_, publication := testAgentTaskStagePublication(t, ledger, t.Name(), project, owner, sessionID, nil)
			server, memory, sessions := testAgentTaskServerWithMemory(t, ledger, project, owner, sessionID)
			injected := true
			server.taskPublicationFault = func(at string) error {
				if injected && at == stage {
					injected = false
					return errors.New("injected publication crash at " + stage)
				}
				return nil
			}
			publicationID := anyToString(publication["publication_id"])
			if _, err := server.runTaskPublicationWorker(context.Background(), publicationID); err == nil {
				t.Fatalf("fault hook %s did not interrupt publication", stage)
			}
			server.taskPublicationFault = nil
			if _, err := ledger.db.ExecContext(context.Background(), `UPDATE task_ledger_publications SET worker_claim_expires_at=? WHERE publication_id=? AND status!='committed'`, time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), publicationID); err != nil {
				t.Fatal(err)
			}
			server.reconcileTaskDeliveryOnce(context.Background())
			recovered, err := ledger.publication(context.Background(), publicationID)
			if err != nil || anyToString(recovered["status"]) != "committed" {
				t.Fatalf("publication did not recover at %s: %#v %v", stage, recovered, err)
			}
			history, err := os.ReadFile(memory.policy.historyPath)
			if err != nil || countNonEmptyLines(string(history)) != 1 {
				t.Fatalf("publication recovery at %s duplicated or lost memory put: err=%v history=%q", stage, err, history)
			}
			_, events, exists := sessions.get(sessionID)
			if !exists || anyToInt(agentSessionSteeringInbox(sessionID, events)["pending_count"], 0) != 1 {
				t.Fatalf("publication recovery at %s did not dedupe delivery: exists=%v events=%#v", stage, exists, events)
			}
		})
	}
}

func TestAgentTaskDeliveryCrashBoundariesRecoverWithoutDuplicateProjection(t *testing.T) {
	for _, stage := range []string{"after_claim", "before_projection", "after_projection", "after_finish"} {
		t.Run(stage, func(t *testing.T) {
			ledger := testAgentTaskLedger(t)
			project, owner, sessionID := "delivery-crash-project", "delivery-crash-reviewer", "sess_delivery_crash_"+stage
			_, publication := testAgentTaskStagePublication(t, ledger, t.Name(), project, owner, sessionID, nil)
			server, _, sessions := testAgentTaskServerWithMemory(t, ledger, project, owner, sessionID)
			injected := true
			server.taskDeliveryFault = func(at string) error {
				if injected && at == stage {
					injected = false
					return errors.New("injected delivery crash at " + stage)
				}
				return nil
			}
			committed, err := server.runTaskPublicationWorker(context.Background(), anyToString(publication["publication_id"]))
			if err != nil || anyToString(committed["status"]) != "committed" {
				t.Fatalf("publication before delivery recovery: %#v %v", committed, err)
			}
			server.taskDeliveryFault = nil
			if _, err := ledger.db.ExecContext(context.Background(), `UPDATE task_ledger_deliveries SET worker_claim_expires_at=? WHERE publication_id=? AND status NOT IN ('delivered','acknowledged')`, time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), anyToString(publication["publication_id"])); err != nil {
				t.Fatal(err)
			}
			server.reconcileTaskDeliveryOnce(context.Background())
			rows, err := ledger.deliveries(context.Background(), anyToString(anyMap(publication["result"])["task_id"]), anyToString(publication["result_id"]))
			if err != nil || len(rows) != 1 || anyToString(rows[0]["status"]) != "delivered" {
				t.Fatalf("delivery did not recover at %s: %#v %v", stage, rows, err)
			}
			_, events, exists := sessions.get(sessionID)
			if !exists {
				t.Fatalf("recipient session missing at %s", stage)
			}
			deliveryEvents := 0
			for _, event := range events {
				if anyToString(event["type"]) == agentTaskDeliveryEventType {
					deliveryEvents++
				}
			}
			if deliveryEvents != 1 || anyToInt(agentSessionSteeringInbox(sessionID, events)["pending_count"], 0) != 1 {
				t.Fatalf("delivery projection duplicated at %s: delivery_events=%d events=%#v", stage, deliveryEvents, events)
			}
		})
	}
}

func TestAgentTaskResourceRevalidatesCurrentServerOwnedWorkspaceBinding(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	project, workspace, owner := "task-binding-project", "workspace-task-binding", "task-binding-reviewer"
	active := true
	server := &server{taskLedger: ledger}
	server.taskProjectWorkspace = func(candidate string) (string, error) {
		if !active || !strings.EqualFold(strings.TrimSpace(candidate), project) {
			return "", errors.New("task project is not actively bound to a workspace")
		}
		return workspace, nil
	}
	manifest := testAgentTaskManifest(t.Name(), project, owner, "sess_task_binding")
	manifest["workspace_id"] = workspace
	manifest["metadata"] = map[string]any{"authorized_workspace_id": "spoofed-workspace"}
	if _, err := ledger.submit(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	auth := agentTaskRouteAuth{Principal: owner, Workspace: workspace, Signed: true}
	if err := server.authorizeTaskResource(context.Background(), t.Name(), auth); err != nil {
		t.Fatalf("active server-owned binding rejected: %v", err)
	}
	serviceAuth := agentTaskRouteAuth{Principal: "gateway-service", Service: true}
	if err := server.authorizeTaskResource(context.Background(), t.Name(), serviceAuth); err != nil {
		t.Fatalf("service access rejected active server-owned binding: %v", err)
	}
	artifactResponse := map[string]any{"artifact": map[string]any{"task_id": t.Name()}}
	if !agentTaskArtifactAllowsAuth(context.Background(), server, artifactResponse, serviceAuth) {
		t.Fatal("service artifact access rejected active server-owned binding")
	}
	active = false
	if err := server.authorizeTaskResource(context.Background(), t.Name(), auth); err == nil || !strings.Contains(err.Error(), "actively bound") {
		t.Fatalf("resource access trusted stale/spoofable workspace metadata: %v", err)
	}
	if err := server.authorizeTaskResource(context.Background(), t.Name(), serviceAuth); err == nil || !strings.Contains(err.Error(), "actively bound") {
		t.Fatalf("service resource access bypassed current server-owned governance: %v", err)
	}
	if agentTaskArtifactAllowsAuth(context.Background(), server, artifactResponse, serviceAuth) {
		t.Fatal("service artifact access bypassed current server-owned governance")
	}
}

func TestAgentTaskTwoSQLiteHandlesFencePublicationAndDeliveryClaims(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GO_AGENT_TASK_LEDGER_PATH", filepath.Join(root, "agent_tasks.sqlite3"))
	t.Setenv("GO_AGENT_TASK_ARTIFACT_DIR", filepath.Join(root, "artifacts"))
	t.Setenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH", "")
	first, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer first.close()
	second, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer second.close()
	_, publication := testAgentTaskStagePublication(t, first, t.Name(), "outbox-race-project", "outbox-reviewer", "sess_outbox_race", nil)
	publicationID := anyToString(publication["publication_id"])
	type claimResult struct {
		ledger  *agentTaskDeliveryLedger
		row     map[string]any
		claimed bool
		err     error
	}
	publicationClaims := make(chan claimResult, 2)
	var wg sync.WaitGroup
	for index, ledger := range []*agentTaskDeliveryLedger{first, second} {
		wg.Add(1)
		go func(index int, ledger *agentTaskDeliveryLedger) {
			defer wg.Done()
			row, claimed, claimErr := ledger.claimPublication(context.Background(), publicationID, "publication-racer-"+string(rune('a'+index)))
			publicationClaims <- claimResult{ledger: ledger, row: row, claimed: claimed, err: claimErr}
		}(index, ledger)
	}
	wg.Wait()
	close(publicationClaims)
	publicationWinners := []claimResult{}
	for claim := range publicationClaims {
		if claim.err != nil {
			t.Fatalf("publication race: %v", claim.err)
		}
		if claim.claimed {
			publicationWinners = append(publicationWinners, claim)
		}
	}
	if len(publicationWinners) != 1 {
		t.Fatalf("publication outbox admitted %d concurrent owners", len(publicationWinners))
	}
	winner := publicationWinners[0]
	if _, err := winner.ledger.finalizePublicationClaim(context.Background(), publicationID, anyToString(winner.row["worker_claim_id"]), "committed", "race-writeback-receipt", ""); err != nil {
		t.Fatalf("finalize publication winner: %v", err)
	}
	final, err := first.publication(context.Background(), publicationID)
	if err != nil {
		t.Fatal(err)
	}
	deliveryID := anyToString(anyMap(anySlice(final["deliveries"])[0])["delivery_id"])
	deliveryClaims := make(chan claimResult, 2)
	for index, ledger := range []*agentTaskDeliveryLedger{first, second} {
		wg.Add(1)
		go func(index int, ledger *agentTaskDeliveryLedger) {
			defer wg.Done()
			row, claimed, claimErr := ledger.claimDelivery(context.Background(), deliveryID, "delivery-racer-"+string(rune('a'+index)))
			deliveryClaims <- claimResult{ledger: ledger, row: row, claimed: claimed, err: claimErr}
		}(index, ledger)
	}
	wg.Wait()
	close(deliveryClaims)
	deliveryWinners := 0
	for claim := range deliveryClaims {
		if claim.err != nil {
			t.Fatalf("delivery race: %v", claim.err)
		}
		if claim.claimed {
			deliveryWinners++
		}
	}
	if deliveryWinners != 1 {
		t.Fatalf("delivery outbox admitted %d concurrent owners", deliveryWinners)
	}
}

func TestAgentTaskOutboxRetryBudgetsDeadLetterDurably(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	_, publication := testAgentTaskStagePublication(t, ledger, t.Name(), "dead-letter-project", "dead-letter-reviewer", "sess_dead_letter", nil)
	publicationID := anyToString(publication["publication_id"])
	if _, err := ledger.db.ExecContext(context.Background(), `UPDATE task_ledger_publications SET worker_attempts=?,last_error='persistent writeback fault' WHERE publication_id=?`, agentTaskDeliveryMaxAttempts, publicationID); err != nil {
		t.Fatal(err)
	}
	deadPublication, claimed, err := ledger.claimPublication(context.Background(), publicationID, "publication-worker")
	if err != nil || claimed || anyToString(deadPublication["status"]) != "dead_letter" {
		t.Fatalf("publication retry budget did not dead-letter: claimed=%v row=%#v err=%v", claimed, deadPublication, err)
	}
	deliveryID := anyToString(anyMap(anySlice(deadPublication["deliveries"])[0])["delivery_id"])
	if _, err := ledger.db.ExecContext(context.Background(), `UPDATE task_ledger_deliveries SET attempts=?,status='failed',last_error='persistent projection fault' WHERE delivery_id=?`, agentTaskDeliveryMaxAttempts, deliveryID); err != nil {
		t.Fatal(err)
	}
	deadDelivery, claimed, err := ledger.claimDelivery(context.Background(), deliveryID, "delivery-worker")
	if err != nil || claimed || anyToString(deadDelivery["status"]) != "dead_letter" {
		t.Fatalf("delivery retry budget did not dead-letter: claimed=%v row=%#v err=%v", claimed, deadDelivery, err)
	}
}

func TestAgentTaskReviewAndIntegrationStagesAreExplicitAndMergeStaysClosed(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	_, publication := testAgentTaskStagePublication(t, ledger, t.Name(), "review-project", "canonical-reviewer", "sess_review", nil)
	publicationID := anyToString(publication["publication_id"])
	committed, err := ledger.finalizePublication(context.Background(), publicationID, "committed", "writeback-receipt", "")
	if err != nil {
		t.Fatal(err)
	}
	taskID, resultID := anyToString(committed["task_id"]), anyToString(committed["result_id"])
	if _, err := ledger.review(context.Background(), taskID, resultID, "not-the-reviewer", "acknowledge", "", ""); err == nil || !strings.Contains(err.Error(), "canonical reviewer") {
		t.Fatalf("non-owner review accepted: %v", err)
	}
	deliveryID := anyToString(anyMap(anySlice(committed["deliveries"])[0])["delivery_id"])
	claim, err := ledger.claimReview(context.Background(), taskID, resultID, deliveryID, "canonical-reviewer")
	if err != nil || anyToString(claim["status"]) != "active" {
		t.Fatalf("canonical reviewer claim failed: %#v %v", claim, err)
	}
	replayedClaim, err := ledger.claimReview(context.Background(), taskID, resultID, deliveryID, "canonical-reviewer")
	if err != nil || !anyToBool(replayedClaim["idempotent_replay"]) || anyToString(replayedClaim["claim_id"]) != anyToString(claim["claim_id"]) {
		t.Fatalf("reviewer claim did not replay immutably: %#v %v", replayedClaim, err)
	}
	if _, err := ledger.review(context.Background(), taskID, resultID, "canonical-reviewer", "acknowledge", "received", ""); err != nil {
		t.Fatalf("acknowledge review: %v", err)
	}
	if _, err := ledger.review(context.Background(), taskID, resultID, "canonical-reviewer", "accept", "verified", ""); err != nil {
		t.Fatalf("accept review: %v", err)
	}
	digest := anyToString(anyMap(committed["result"])["result_digest"])
	integration, err := ledger.integrate(context.Background(), map[string]any{"task_id": taskID, "result_id": resultID, "actor": "canonical-reviewer", "action": "merge", "digest": digest, "target": "main"})
	if err != nil {
		t.Fatalf("record closed merge decision: %v", err)
	}
	if anyToString(integration["status"]) != "rejected" || anyToBool(integration["merge_allowed"]) {
		t.Fatalf("Core unexpectedly authorized merge side effect: %#v", integration)
	}
	task, _, err := ledger.get(context.Background(), taskID)
	if err != nil || anyToString(task["status"]) != "integration_failed" {
		t.Fatalf("closed merge decision was not immutable lifecycle evidence: %#v %v", task, err)
	}
}

func TestAgentTaskManifestBoundsContextRequestBeforeStorage(t *testing.T) {
	manifest := testAgentTaskManifest(t.Name(), "bounded-project", "bounded-reviewer", "sess_bounded")
	manifest["context_request"] = map[string]any{"content_hash": "sha256:" + strings.Repeat("a", 64), "session_id": "sess_bounded", "payload": strings.Repeat("x", agentTaskContextPackMaxBytes)}
	if _, _, err := normalizeAgentTaskManifest(manifest); err == nil || !strings.Contains(err.Error(), "context_request exceeds") {
		t.Fatalf("oversized context request accepted: %v", err)
	}
	missing := testAgentTaskManifest(t.Name()+"-missing", "bounded-project", "bounded-reviewer", "sess_bounded")
	missing["context_request"] = map[string]any{}
	if _, _, err := normalizeAgentTaskManifest(missing); err == nil || !strings.Contains(err.Error(), "canonical content_hash") {
		t.Fatalf("manifest without pinned context/session linkage accepted: %v", err)
	}
}

func TestAgentTaskBlockingAnswerQueuesNewFencedAttempt(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	_, publication := testAgentTaskStagePublication(t, ledger, t.Name(), "blocking-project", "blocking-reviewer", "sess_blocking", nil)
	committed, err := ledger.finalizePublication(context.Background(), anyToString(publication["publication_id"]), "committed", "writeback-receipt", "")
	if err != nil {
		t.Fatal(err)
	}
	taskID, resultID, attemptID := anyToString(committed["task_id"]), anyToString(committed["result_id"]), anyToString(committed["attempt_id"])
	deliveryID := anyToString(anyMap(anySlice(committed["deliveries"])[0])["delivery_id"])
	if _, err := ledger.claimReview(context.Background(), taskID, resultID, deliveryID, "blocking-reviewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.review(context.Background(), taskID, resultID, "blocking-reviewer", "block", "need exact input", ""); err != nil {
		t.Fatal(err)
	}
	answer, err := ledger.answerBlockingQuestion(context.Background(), taskID, resultID, deliveryID, "blocking-reviewer", "use the verified bounded input", attemptID)
	if err != nil || !anyToBool(answer["queued"]) {
		t.Fatalf("blocking answer did not queue follow-up attempt: %#v %v", answer, err)
	}
	claim, err := ledger.claimNext(context.Background(), "blocking-worker", "blocking-worker-instance", "")
	if err != nil || claim == nil {
		t.Fatalf("answered task was not claimable: %#v %v", claim, err)
	}
	newFence := testAgentTaskFenceFromClaim(t, claim)
	if newFence.AttemptID == attemptID || newFence.Generation != 2 {
		t.Fatalf("blocking answer reused stale attempt fence: old=%s new=%#v", attemptID, newFence)
	}
}

func TestAgentTaskFollowUpIntegrationCreatesBoundedQueuedTask(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	_, publication := testAgentTaskStagePublication(t, ledger, t.Name(), "follow-up-project", "follow-up-reviewer", "sess_follow_up", nil)
	committed, err := ledger.finalizePublication(context.Background(), anyToString(publication["publication_id"]), "committed", "writeback-receipt", "")
	if err != nil {
		t.Fatal(err)
	}
	taskID, resultID := anyToString(committed["task_id"]), anyToString(committed["result_id"])
	deliveryID := anyToString(anyMap(anySlice(committed["deliveries"])[0])["delivery_id"])
	if _, err := ledger.claimReview(context.Background(), taskID, resultID, deliveryID, "follow-up-reviewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.review(context.Background(), taskID, resultID, "follow-up-reviewer", "accept", "follow-up needed", ""); err != nil {
		t.Fatal(err)
	}
	digest := anyToString(anyMap(committed["result"])["result_digest"])
	integration, err := ledger.integrate(context.Background(), map[string]any{"task_id": taskID, "result_id": resultID, "actor": "follow-up-reviewer", "action": "follow_up_task", "digest": digest})
	if err != nil {
		t.Fatal(err)
	}
	followUpID := anyToString(integration["follow_up_task_id"])
	if anyToString(integration["status"]) != "follow_up_queued" || followUpID == "" {
		t.Fatalf("follow-up integration did not create task evidence: %#v", integration)
	}
	followUp, _, err := ledger.get(context.Background(), followUpID)
	if err != nil || anyToString(followUp["status"]) != "queued" || anyToString(anyMap(followUp["metadata"])["parent_result_id"]) != resultID {
		t.Fatalf("follow-up task missing parent linkage: %#v %v", followUp, err)
	}
	claim, err := ledger.claimNext(context.Background(), "follow-up-worker", "follow-up-instance", "")
	if err != nil || claim == nil || anyToString(anyMap(claim["task"])["task_id"]) != followUpID {
		t.Fatalf("follow-up task was not independently claimable: %#v %v", claim, err)
	}
}
