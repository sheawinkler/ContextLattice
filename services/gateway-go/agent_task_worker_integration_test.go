package main

import (
	"context"
	"database/sql"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestTaskWorkerSeamSchemaV9UpgradePreservesCanonicalBinding(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "agent_tasks.sqlite3")
	t.Setenv("GO_AGENT_TASK_LEDGER_PATH", dbPath)
	t.Setenv("GO_AGENT_TASK_ARTIFACT_DIR", filepath.Join(root, "artifacts"))
	t.Setenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH", "")

	ledger, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const (
		workspace = "seam-schema-workspace"
		requested = "seam-schema-worker"
		taskID    = "seam-schema-task"
	)
	registerIdentityForTest(t, ledger, identityTestAuthority("seam-schema-occupier", workspace, "seam-schema-occupier-instance"), requested)
	authority := identityTestAuthority("seam-schema-owner", workspace, "seam-schema-instance")
	response, identity := registerIdentityForTest(t, ledger, authority, requested)
	updateID := anyToString(anyMap(response["identity_update"])["update_id"])
	if updateID == "" || identity.CanonicalWorkerID == requested {
		t.Fatalf("collision did not allocate a canonical identity: response=%#v identity=%#v", response, identity)
	}
	manifest := testAgentTaskManifest(taskID, "seam-schema-project", authority.PrincipalID, "seam-schema-session")
	manifest["workspace_id"] = workspace
	manifest["metadata"] = map[string]any{"worker": requested, "worker_instance_id": authority.WorkerInstanceID}
	if _, err := ledger.submit(ctx, manifest); err != nil {
		t.Fatal(err)
	}
	tx, err := ledger.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindWorkerIdentityTaskTx(ctx, tx, taskID, identity, requested, 0); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	update := acknowledgeIdentityForTest(t, ledger, authority, updateID)
	if err := ledger.close(); err != nil {
		t.Fatal(err)
	}

	legacyDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`PRAGMA foreign_keys = OFF`,
		`ALTER TABLE task_ledger_tasks DROP COLUMN revision_envelope_json`,
		`ALTER TABLE task_ledger_attempts DROP COLUMN revision_envelope_json`,
		`PRAGMA user_version = 9`,
		`UPDATE task_ledger_meta SET value='9',updated_at='2026-08-12T00:00:00Z' WHERE key='schema_version'`,
	} {
		if _, err := legacyDB.ExecContext(ctx, statement); err != nil {
			_ = legacyDB.Close()
			t.Fatalf("prepare schema-v9 fixture with %q: %v", statement, err)
		}
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatalf("upgrade schema-v9 fixture: %v", err)
	}
	t.Cleanup(func() { _ = restarted.close() })
	var schemaVersion, taskRevisionColumn, attemptRevisionColumn int
	if err := restarted.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&schemaVersion); err != nil {
		t.Fatal(err)
	}
	if err := restarted.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('task_ledger_tasks') WHERE name='revision_envelope_json'`).Scan(&taskRevisionColumn); err != nil {
		t.Fatal(err)
	}
	if err := restarted.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('task_ledger_attempts') WHERE name='revision_envelope_json'`).Scan(&attemptRevisionColumn); err != nil {
		t.Fatal(err)
	}
	if schemaVersion != agentTaskLedgerSchemaVersion || taskRevisionColumn != 1 || attemptRevisionColumn != 1 {
		t.Fatalf("schema upgrade incomplete: version=%d task_revision=%d attempt_revision=%d", schemaVersion, taskRevisionColumn, attemptRevisionColumn)
	}
	var claimWorkerID, bindingWorkerID, bindingState, rebindReceipt string
	var bindingGeneration int
	if err := restarted.db.QueryRowContext(ctx, `SELECT t.claim_worker_id,b.worker_id,b.worker_identity_update_generation,b.state,b.rebind_receipt_digest FROM task_ledger_tasks t JOIN task_ledger_worker_task_bindings b ON b.task_id=t.id WHERE t.id=?`, taskID).Scan(&claimWorkerID, &bindingWorkerID, &bindingGeneration, &bindingState, &rebindReceipt); err != nil {
		t.Fatal(err)
	}
	if claimWorkerID != identity.CanonicalWorkerID || bindingWorkerID != identity.CanonicalWorkerID || bindingGeneration != update.IdentityUpdateGeneration || bindingState != "bound" || rebindReceipt == "" {
		t.Fatalf("schema upgrade overwrote canonical binding: claim=%q binding=%q generation=%d state=%q receipt=%q", claimWorkerID, bindingWorkerID, bindingGeneration, bindingState, rebindReceipt)
	}
}

func TestTaskWorkerSeamCollisionRevisionClaimBindsCanonicalIdentityAcrossRestart(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GO_AGENT_TASK_LEDGER_PATH", filepath.Join(root, "agent_tasks.sqlite3"))
	t.Setenv("GO_AGENT_TASK_ARTIFACT_DIR", filepath.Join(root, "artifacts"))
	t.Setenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH", "")
	ledger, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const (
		workspace = "seam-revision-workspace"
		requested = "seam-revision-worker"
		taskID    = "seam-revision-task"
		project   = "seam-revision-project"
	)
	occupierAuthority := identityTestAuthority("seam-revision-occupier", workspace, "seam-revision-occupier-instance")
	_, occupier := registerIdentityForTest(t, ledger, occupierAuthority, requested)
	authority := identityTestAuthority("seam-revision-owner", workspace, "seam-revision-instance")
	response, identity := registerIdentityForTest(t, ledger, authority, requested)
	updateID := anyToString(anyMap(response["identity_update"])["update_id"])
	update := acknowledgeIdentityForTest(t, ledger, authority, updateID)
	identity, err = ledger.workerIdentityByAuthority(ctx, authority)
	if err != nil || identity.CanonicalWorkerID == requested || identity.AcknowledgedGeneration != update.IdentityUpdateGeneration {
		t.Fatalf("collision acknowledgement did not converge: identity=%#v update=%#v err=%v", identity, update, err)
	}
	manifest := testAgentTaskManifest(taskID, project, authority.PrincipalID, "seam-revision-session")
	manifest["workspace_id"] = workspace
	if _, err := ledger.submit(ctx, manifest); err != nil {
		t.Fatal(err)
	}
	claim, err := ledger.claimTaskWithIdentity(ctx, identity.CanonicalWorkerID, identity.WorkerInstanceID, workspace, taskID, identity.IdentityUpdateGeneration)
	if err != nil || claim == nil {
		t.Fatalf("canonical worker could not claim initial task: claim=%#v err=%v", claim, err)
	}
	fence := testAgentTaskFenceFromClaim(t, claim)
	fence.WorkerIdentityUpdateGeneration = anyToInt(anyMap(claim["lease"])["worker_identity_update_generation"], -1)
	if _, err := ledger.heartbeat(ctx, fence); err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	if _, err := ledger.observe(ctx, fence, "succeeded", &exitCode, map[string]any{"source": "task-worker-seam"}); err != nil {
		t.Fatal(err)
	}
	resultID := "result-" + taskID
	publicationID := "publication-" + taskID
	workspaceRef := "workspace-ref-" + taskID
	publication, err := ledger.stagePublication(ctx, fence, map[string]any{
		"publication_id": publicationID, "idempotency_key": "task-result:" + resultID, "runner_exit_required": true,
		"result": map[string]any{
			"result_id": resultID, "summary": "revision seam result", "output": "request a canonical revision",
			"context_pack_hash": anyToString(anyMap(claim["attempt"])["context_pack_hash"]),
			"workspace":         map[string]any{"workspace_ref": workspaceRef},
			"cleanup":           map[string]any{"cleanup_id": agentTaskCleanupID(taskID, fence.AttemptID, workspaceRef)},
		},
		"artifacts": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	committed, err := ledger.finalizePublication(ctx, anyToString(publication["publication_id"]), "committed", "seam-revision-writeback", "")
	if err != nil {
		t.Fatal(err)
	}
	deliveries := anySlice(committed["deliveries"])
	if len(deliveries) != 1 {
		t.Fatalf("revision fixture delivery count=%d", len(deliveries))
	}
	if _, err := ledger.claimReview(ctx, taskID, resultID, anyToString(anyMap(deliveries[0])["delivery_id"]), authority.PrincipalID); err != nil {
		t.Fatal(err)
	}
	review, err := ledger.reviewWithFence(ctx, taskID, resultID, authority.PrincipalID, "request_changes", "bind the revision to the acknowledged canonical worker", "", fence.AttemptID, fence.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.close() })
	if wrongClaim, err := restarted.claimTaskWithIdentity(ctx, occupier.CanonicalWorkerID, occupier.WorkerInstanceID, workspace, taskID, occupier.IdentityUpdateGeneration); err != nil || wrongClaim != nil {
		t.Fatalf("requested-ID occupier inherited canonical revision: claim=%#v err=%v", wrongClaim, err)
	}
	revisionClaim, err := restarted.claimTaskWithIdentity(ctx, identity.CanonicalWorkerID, identity.WorkerInstanceID, workspace, taskID, identity.IdentityUpdateGeneration)
	if err != nil || revisionClaim == nil {
		t.Fatalf("canonical identity could not reclaim revision after restart: claim=%#v err=%v", revisionClaim, err)
	}
	revisionFence := testAgentTaskFenceFromClaim(t, revisionClaim)
	revisionFence.WorkerIdentityUpdateGeneration = anyToInt(anyMap(revisionClaim["lease"])["worker_identity_update_generation"], -1)
	envelope := anyMap(revisionClaim["revision_envelope"])
	if revisionFence.WorkerID != identity.CanonicalWorkerID || revisionFence.WorkerID == requested || revisionFence.WorkerInstanceID != identity.WorkerInstanceID || revisionFence.WorkerIdentityUpdateGeneration != identity.IdentityUpdateGeneration {
		t.Fatalf("revision claim lost canonical identity fence: %#v", revisionFence)
	}
	if anyToString(envelope["review_id"]) != anyToString(review["review_id"]) || anyToInt(envelope["worker_identity_update_generation"], -1) != identity.IdentityUpdateGeneration || verifyAgentTaskRevisionEnvelope(envelope, revisionFence) != nil {
		t.Fatalf("revision envelope did not bind the exact canonical identity generation: envelope=%#v fence=%#v", envelope, revisionFence)
	}
}

func seamMutationPayload(suffix string, fence agentTaskFence) map[string]any {
	payload := fencePayload(fence)
	switch suffix {
	case "observe":
		payload["runner_status"] = "succeeded"
		payload["exit_code"] = 0
		payload["metadata"] = map[string]any{"source": "task-worker-seam-rejection"}
	case "cleanup":
		payload["cleanup_receipt"] = map[string]any{
			"schema_id": agentTaskCleanupReceiptID, "task_id": fence.TaskID, "attempt_id": fence.AttemptID,
			"lease_id": fence.LeaseID, "generation": fence.Generation, "worker_id": fence.WorkerID,
			"worker_instance_id": fence.WorkerInstanceID,
		}
	}
	return payload
}

func TestTaskWorkerSeamCredentialAndActiveAttemptAuthorizeEveryMutation(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	const (
		principal = "seam-route-principal"
		project   = "seam-route-project"
		workspace = "workspace-seam-route-project"
		instanceA = "seam-route-instance-a"
		instanceB = "seam-route-instance-b"
		taskID    = "seam-route-task"
	)
	server, _, _ := testAgentTaskServerWithMemory(t, ledger, project, principal, "seam-route-session")
	credentialA := strings.Repeat("a", workerInstanceCredentialBytes*2)
	credentialB := strings.Repeat("b", workerInstanceCredentialBytes*2)
	_, responseA := registerCredentialIdentity(t, server, principal, workspace, instanceA, "seam-route-worker-a", credentialA)
	registerCredentialIdentity(t, server, principal, workspace, instanceB, "seam-route-worker-b", credentialB)
	identityA, err := ledger.workerIdentityByAuthority(context.Background(), identityTestAuthority(principal, workspace, instanceA))
	if err != nil {
		t.Fatal(err)
	}
	if anyToString(anyMap(responseA["identity"])["canonical_worker_id"]) != identityA.CanonicalWorkerID {
		t.Fatalf("registration/readback canonical mismatch: response=%#v identity=%#v", responseA, identityA)
	}
	manifest := testAgentTaskManifest(taskID, project, principal, "seam-route-session")
	manifest["workspace_id"] = workspace
	if _, err := ledger.submit(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	call := func(request *http.Request) (int, map[string]any) {
		status, response, _ := callCredentialHTTPIdentityRoute(t, server, request)
		return status, response
	}
	claimPayload := map[string]any{"requested_worker_id": identityA.RequestedWorkerID, "worker": identityA.CanonicalWorkerID, "worker_instance_id": instanceA}
	claimRequest, _ := credentialRouteRequest(t, http.MethodPost, "/agents/tasks/next?worker="+identityA.CanonicalWorkerID, principal, workspace, instanceA, credentialA, claimPayload)
	status, claim := call(claimRequest)
	if status != http.StatusOK || claim["task"] == nil {
		t.Fatalf("credential-bound initial claim failed: status=%d response=%#v", status, claim)
	}
	firstFence := testAgentTaskFenceFromClaim(t, claim)
	firstFence.WorkerIdentityUpdateGeneration = anyToInt(anyMap(claim["lease"])["worker_identity_update_generation"], -1)
	heartbeat, _ := credentialRouteRequest(t, http.MethodPost, "/agents/tasks/"+taskID+"/heartbeat", principal, workspace, instanceA, credentialA, fencePayload(firstFence))
	if status, response := call(heartbeat); status != http.StatusOK {
		t.Fatalf("exact credential heartbeat failed: status=%d response=%#v", status, response)
	}
	failure := fencePayload(firstFence)
	failure["runner_status"] = "failed"
	failure["exit_code"] = 17
	failure["metadata"] = map[string]any{"source": "task-worker-seam-retry"}
	failRequest, _ := credentialRouteRequest(t, http.MethodPost, "/agents/tasks/"+taskID+"/observe", principal, workspace, instanceA, credentialA, failure)
	if status, response := call(failRequest); status != http.StatusOK || anyToString(response["failure_disposition"]) != "retry_queued" {
		t.Fatalf("credential-bound retry observation failed: status=%d response=%#v", status, response)
	}
	retryRequest, _ := credentialRouteRequest(t, http.MethodPost, "/agents/tasks/next?worker="+identityA.CanonicalWorkerID, principal, workspace, instanceA, credentialA, claimPayload)
	status, retryClaim := call(retryRequest)
	if status != http.StatusOK || retryClaim["task"] == nil {
		t.Fatalf("credential-bound retry claim failed: status=%d response=%#v", status, retryClaim)
	}
	retryFence := testAgentTaskFenceFromClaim(t, retryClaim)
	retryFence.WorkerIdentityUpdateGeneration = anyToInt(anyMap(retryClaim["lease"])["worker_identity_update_generation"], -1)
	if retryFence.AttemptID == firstFence.AttemptID || retryFence.Generation != firstFence.Generation+1 {
		t.Fatalf("retry did not advance the exact active attempt: first=%#v retry=%#v", firstFence, retryFence)
	}

	for _, suffix := range []string{"heartbeat", "observe", "publish", "cleanup"} {
		path := "/agents/tasks/" + taskID + "/" + suffix
		staleRequest, _ := credentialRouteRequest(t, http.MethodPost, path, principal, workspace, instanceA, credentialA, seamMutationPayload(suffix, firstFence))
		if status, _ := call(staleRequest); status == http.StatusOK {
			t.Fatalf("%s accepted a credential-valid stale attempt", suffix)
		}
		foreignRequest, _ := credentialRouteRequest(t, http.MethodPost, path, principal, workspace, instanceB, credentialB, seamMutationPayload(suffix, retryFence))
		if status, _ := call(foreignRequest); status == http.StatusOK {
			t.Fatalf("%s accepted a foreign-instance credential", suffix)
		}
	}

	retryHeartbeat, _ := credentialRouteRequest(t, http.MethodPost, "/agents/tasks/"+taskID+"/heartbeat", principal, workspace, instanceA, credentialA, fencePayload(retryFence))
	if status, response := call(retryHeartbeat); status != http.StatusOK {
		t.Fatalf("exact retry heartbeat failed: status=%d response=%#v", status, response)
	}
	success := fencePayload(retryFence)
	success["runner_status"] = "succeeded"
	success["exit_code"] = 0
	success["metadata"] = map[string]any{"source": "task-worker-seam-success"}
	successRequest, _ := credentialRouteRequest(t, http.MethodPost, "/agents/tasks/"+taskID+"/observe", principal, workspace, instanceA, credentialA, success)
	if status, response := call(successRequest); status != http.StatusOK {
		t.Fatalf("exact retry observation failed: status=%d response=%#v", status, response)
	}
	resultID := "result-" + taskID
	workspaceRef := "workspace-ref-" + taskID
	publicationPayload := fencePayload(retryFence)
	publicationPayload["publication_id"] = "publication-" + taskID
	publicationPayload["idempotency_key"] = "task-result:" + resultID
	publicationPayload["runner_exit_required"] = true
	publicationPayload["result"] = map[string]any{
		"result_id": resultID, "summary": "credential and active-attempt seam", "output": "verified",
		"context_pack_hash": anyToString(anyMap(retryClaim["attempt"])["context_pack_hash"]),
		"workspace":         map[string]any{"workspace_ref": workspaceRef},
		"cleanup":           map[string]any{"cleanup_id": agentTaskCleanupID(taskID, retryFence.AttemptID, workspaceRef)},
	}
	publicationPayload["artifacts"] = []any{}
	publishRequest, _ := credentialRouteRequest(t, http.MethodPost, "/agents/tasks/"+taskID+"/publish", principal, workspace, instanceA, credentialA, publicationPayload)
	status, publication := call(publishRequest)
	if status != http.StatusOK {
		t.Fatalf("exact retry publication failed: status=%d response=%#v", status, publication)
	}
	cleanupPayload := fencePayload(retryFence)
	cleanupPayload["cleanup_receipt"] = testAgentTaskCleanupReceipt(publication, retryFence)
	cleanupRequest, _ := credentialRouteRequest(t, http.MethodPost, "/agents/tasks/"+taskID+"/cleanup", principal, workspace, instanceA, credentialA, cleanupPayload)
	if status, response := call(cleanupRequest); status != http.StatusOK {
		t.Fatalf("exact retry cleanup failed: status=%d response=%#v", status, response)
	}
}

type taskWorkerPreRegistrationReviewFixture struct {
	TaskID, ResultID, DeliveryID, ContextPackHash string
	Fence                                         agentTaskFence
}

func prepareTaskWorkerPreRegistrationLease(t *testing.T, ledger *agentTaskDeliveryLedger, taskID, project, principal, workspace, requested, instance string) taskWorkerPreRegistrationReviewFixture {
	t.Helper()
	ctx := context.Background()
	manifest := testAgentTaskManifest(taskID, project, principal, taskID+"-session")
	manifest["workspace_id"] = workspace
	if _, err := ledger.submit(ctx, manifest); err != nil {
		t.Fatalf("submit pre-registration review task: %v", err)
	}
	claim, err := ledger.claimTask(ctx, requested, instance, workspace, taskID)
	if err != nil || claim == nil {
		t.Fatalf("claim pre-registration review task: claim=%#v err=%v", claim, err)
	}
	fence := testAgentTaskFenceFromClaim(t, claim)
	if fence.WorkerIdentityUpdateGeneration != 0 || fence.WorkerID != requested || fence.WorkerInstanceID != instance {
		t.Fatalf("pre-registration review claim was not generation zero: %#v", fence)
	}
	return taskWorkerPreRegistrationReviewFixture{
		TaskID:          taskID,
		ContextPackHash: anyToString(anyMap(claim["attempt"])["context_pack_hash"]),
		Fence:           fence,
	}
}

func observeTaskWorkerPreRegistrationResult(t *testing.T, ledger *agentTaskDeliveryLedger, fixture taskWorkerPreRegistrationReviewFixture) {
	t.Helper()
	ctx := context.Background()
	exitCode := 0
	if _, err := ledger.observe(ctx, fixture.Fence, "succeeded", &exitCode, map[string]any{"source": "task-worker-pre-registration-review"}); err != nil {
		t.Fatalf("observe pre-registration review task: %v", err)
	}
}

func stageTaskWorkerPreRegistrationResult(t *testing.T, ledger *agentTaskDeliveryLedger, fixture taskWorkerPreRegistrationReviewFixture) taskWorkerPreRegistrationReviewFixture {
	t.Helper()
	ctx := context.Background()
	resultID := "result-" + fixture.TaskID
	publicationID := "publication-" + fixture.TaskID
	workspaceRef := "workspace-ref-" + fixture.TaskID
	publication, err := ledger.stagePublication(ctx, fixture.Fence, map[string]any{
		"publication_id": publicationID, "idempotency_key": "task-result:" + resultID, "runner_exit_required": true,
		"result": map[string]any{
			"result_id": resultID, "summary": "pre-registration review result", "output": "immutable review evidence",
			"context_pack_hash": fixture.ContextPackHash,
			"workspace":         map[string]any{"workspace_ref": workspaceRef},
			"cleanup":           map[string]any{"cleanup_id": agentTaskCleanupID(fixture.TaskID, fixture.Fence.AttemptID, workspaceRef)},
		},
		"artifacts": []any{},
	})
	if err != nil {
		t.Fatalf("stage pre-registration review result: %v", err)
	}
	deliveries := anySlice(publication["deliveries"])
	if len(deliveries) != 1 {
		t.Fatalf("pre-registration staged delivery count=%d", len(deliveries))
	}
	fixture.ResultID = resultID
	fixture.DeliveryID = anyToString(anyMap(deliveries[0])["delivery_id"])
	return fixture
}

func finalizeTaskWorkerPreRegistrationResult(t *testing.T, ledger *agentTaskDeliveryLedger, fixture taskWorkerPreRegistrationReviewFixture) taskWorkerPreRegistrationReviewFixture {
	t.Helper()
	committed, err := ledger.finalizePublication(context.Background(), "publication-"+fixture.TaskID, "committed", "writeback-"+fixture.TaskID, "")
	if err != nil {
		t.Fatalf("finalize pre-registration review result: %v", err)
	}
	deliveries := anySlice(committed["deliveries"])
	if len(deliveries) != 1 {
		t.Fatalf("pre-registration review delivery count=%d", len(deliveries))
	}
	fixture.DeliveryID = anyToString(anyMap(deliveries[0])["delivery_id"])
	return fixture
}

func publishTaskWorkerPreRegistrationResult(t *testing.T, ledger *agentTaskDeliveryLedger, fixture taskWorkerPreRegistrationReviewFixture) taskWorkerPreRegistrationReviewFixture {
	t.Helper()
	observeTaskWorkerPreRegistrationResult(t, ledger, fixture)
	fixture = stageTaskWorkerPreRegistrationResult(t, ledger, fixture)
	return finalizeTaskWorkerPreRegistrationResult(t, ledger, fixture)
}

func prepareTaskWorkerPreRegistrationPublication(t *testing.T, ledger *agentTaskDeliveryLedger, taskID, project, principal, workspace, requested, instance string) taskWorkerPreRegistrationReviewFixture {
	t.Helper()
	fixture := prepareTaskWorkerPreRegistrationLease(t, ledger, taskID, project, principal, workspace, requested, instance)
	return publishTaskWorkerPreRegistrationResult(t, ledger, fixture)
}

func claimTaskWorkerPreRegistrationReview(t *testing.T, ledger *agentTaskDeliveryLedger, fixture taskWorkerPreRegistrationReviewFixture, principal string) {
	t.Helper()
	if _, err := ledger.claimReview(context.Background(), fixture.TaskID, fixture.ResultID, fixture.DeliveryID, principal); err != nil {
		t.Fatalf("claim pre-registration review: %v", err)
	}
}

func prepareTaskWorkerPreRegistrationReview(t *testing.T, ledger *agentTaskDeliveryLedger, taskID, project, principal, workspace, requested, instance string) taskWorkerPreRegistrationReviewFixture {
	t.Helper()
	fixture := prepareTaskWorkerPreRegistrationPublication(t, ledger, taskID, project, principal, workspace, requested, instance)
	claimTaskWorkerPreRegistrationReview(t, ledger, fixture, principal)
	return fixture
}

func acknowledgeTaskWorkerPreRegistrationReview(t *testing.T, ledger *agentTaskDeliveryLedger, fixture taskWorkerPreRegistrationReviewFixture, principal string) map[string]any {
	t.Helper()
	review, err := ledger.reviewWithFence(context.Background(), fixture.TaskID, fixture.ResultID, principal, "acknowledge", "acknowledge immutable result before reviewer claim", "", fixture.Fence.AttemptID, fixture.Fence.Generation)
	if err != nil {
		t.Fatalf("acknowledge pre-registration review: %v", err)
	}
	return review
}

func requestTaskWorkerPreRegistrationChanges(t *testing.T, ledger *agentTaskDeliveryLedger, fixture taskWorkerPreRegistrationReviewFixture, principal string) map[string]any {
	t.Helper()
	review, err := ledger.reviewWithFence(context.Background(), fixture.TaskID, fixture.ResultID, principal, "request_changes", "bind the revision to the exact pre-registration worker", "", fixture.Fence.AttemptID, fixture.Fence.Generation)
	if err != nil {
		t.Fatalf("request pre-registration changes: %v", err)
	}
	return review
}

func assertTaskWorkerCanonicalClaimAfterRestart(t *testing.T, ledger *agentTaskDeliveryLedger, fixture taskWorkerPreRegistrationReviewFixture, identity, occupier agentWorkerIdentityRecord, requested, workspace, expectedReviewID string) {
	t.Helper()
	ctx := context.Background()
	var bindingIdentity, bindingWorker, bindingInstance, bindingState, bindingReceipt string
	var bindingGeneration int
	if err := ledger.db.QueryRowContext(ctx, `SELECT identity_id,worker_id,worker_instance_id,worker_identity_update_generation,state,rebind_receipt_digest FROM task_ledger_worker_task_bindings WHERE task_id=?`, fixture.TaskID).Scan(&bindingIdentity, &bindingWorker, &bindingInstance, &bindingGeneration, &bindingState, &bindingReceipt); err != nil {
		t.Fatalf("read adopted task binding: %v", err)
	}
	if bindingIdentity != identity.IdentityID || bindingWorker != identity.CanonicalWorkerID || bindingInstance != identity.WorkerInstanceID || bindingGeneration != identity.IdentityUpdateGeneration || bindingState != "bound" || bindingReceipt == "" {
		t.Fatalf("task binding is not exact/current: identity=%q worker=%q instance=%q generation=%d state=%q receipt=%q", bindingIdentity, bindingWorker, bindingInstance, bindingGeneration, bindingState, bindingReceipt)
	}
	if claim, err := ledger.claimTask(ctx, requested, identity.WorkerInstanceID, workspace, fixture.TaskID); err != nil || claim != nil {
		t.Fatalf("stale requested owner reclaimed canonical revision: claim=%#v err=%v", claim, err)
	}
	if claim, err := ledger.claimTask(ctx, requested, occupier.WorkerInstanceID, workspace, fixture.TaskID); err != nil || claim != nil {
		t.Fatalf("same-requested occupier reclaimed canonical revision: claim=%#v err=%v", claim, err)
	}
	if claim, err := ledger.claimTask(ctx, identity.CanonicalWorkerID, occupier.WorkerInstanceID, workspace, fixture.TaskID); err != nil || claim != nil {
		t.Fatalf("foreign instance reclaimed canonical revision by copying its ID: claim=%#v err=%v", claim, err)
	}
	if claim, err := ledger.claimTaskWithIdentity(ctx, identity.CanonicalWorkerID, identity.WorkerInstanceID, workspace, fixture.TaskID, identity.IdentityUpdateGeneration-1); err == nil || claim != nil {
		t.Fatalf("stale identity generation reclaimed canonical revision: claim=%#v err=%v", claim, err)
	}
	claim, err := ledger.claimTaskWithIdentity(ctx, identity.CanonicalWorkerID, identity.WorkerInstanceID, workspace, fixture.TaskID, identity.IdentityUpdateGeneration)
	if err != nil || claim == nil {
		t.Fatalf("canonical identity could not reclaim adopted task: claim=%#v err=%v", claim, err)
	}
	fence := testAgentTaskFenceFromClaim(t, claim)
	fence.WorkerIdentityUpdateGeneration = anyToInt(anyMap(claim["lease"])["worker_identity_update_generation"], -1)
	envelope := anyMap(claim["revision_envelope"])
	if fence.WorkerID != identity.CanonicalWorkerID || fence.WorkerInstanceID != identity.WorkerInstanceID || fence.WorkerIdentityUpdateGeneration != identity.IdentityUpdateGeneration {
		t.Fatalf("canonical claim lost the exact worker authority: %#v", fence)
	}
	if expectedReviewID != "" && (anyToString(envelope["review_id"]) != expectedReviewID || anyToInt(envelope["worker_identity_update_generation"], -1) != identity.IdentityUpdateGeneration || verifyAgentTaskRevisionEnvelope(envelope, fence) != nil) {
		t.Fatalf("canonical revision envelope lost the exact worker authority: fence=%#v envelope=%#v", fence, envelope)
	}
}

func assertTaskWorkerCanonicalRevisionAfterRestart(t *testing.T, ledger *agentTaskDeliveryLedger, fixture taskWorkerPreRegistrationReviewFixture, identity, occupier agentWorkerIdentityRecord, requested, workspace string, review map[string]any) {
	t.Helper()
	assertTaskWorkerCanonicalClaimAfterRestart(t, ledger, fixture, identity, occupier, requested, workspace, anyToString(review["review_id"]))
}

func TestTaskWorkerSeamAckAdoptsCompletedReviewBeforeRequestChanges(t *testing.T) {
	ledger, second := identityTestLedgers(t)
	const (
		principal = "seam-ack-first-principal"
		workspace = "seam-ack-first-workspace"
		requested = "seam-ack-first-worker"
		instance  = "seam-ack-first-instance"
		taskID    = "seam-ack-first-task"
	)
	occupierAuthority := identityTestAuthority("seam-ack-first-occupier", workspace, "seam-ack-first-occupier-instance")
	_, occupier := registerIdentityForTest(t, ledger, occupierAuthority, requested)
	fixture := prepareTaskWorkerPreRegistrationReview(t, ledger, taskID, "seam-ack-first-project", principal, workspace, requested, instance)
	authority := identityTestAuthority(principal, workspace, instance)
	response, identity := registerIdentityForTest(t, ledger, authority, requested)
	updateID := anyToString(anyMap(response["identity_update"])["update_id"])
	if updateID == "" || identity.IdentityUpdateGeneration != 1 {
		t.Fatalf("completed review collision did not issue an update: response=%#v identity=%#v", response, identity)
	}
	acknowledgeIdentityForTest(t, ledger, authority, updateID)
	identity, _ = ledger.workerIdentityByAuthority(context.Background(), authority)
	var bindingCount int
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_worker_task_bindings WHERE task_id=? AND identity_id=?`, taskID, identity.IdentityID).Scan(&bindingCount); err != nil || bindingCount != 1 {
		t.Fatalf("ACK did not adopt completed review attempt: count=%d err=%v", bindingCount, err)
	}
	review := requestTaskWorkerPreRegistrationChanges(t, ledger, fixture, principal)
	if err := ledger.close(); err != nil {
		t.Fatal(err)
	}
	if err := second.close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatalf("restart ACK-first review seam: %v", err)
	}
	defer restarted.close()
	restartedIdentity, err := restarted.workerIdentityByAuthority(context.Background(), authority)
	if err != nil {
		t.Fatal(err)
	}
	assertTaskWorkerCanonicalRevisionAfterRestart(t, restarted, fixture, restartedIdentity, occupier, requested, workspace, review)
}

func TestTaskWorkerSeamAckAdoptsCompletedPublicationBeforeReviewerClaim(t *testing.T) {
	ledger, second := identityTestLedgers(t)
	const (
		principal = "seam-published-first-principal"
		workspace = "seam-published-first-workspace"
		requested = "seam-published-first-worker"
		instance  = "seam-published-first-instance"
		taskID    = "seam-published-first-task"
	)
	occupierAuthority := identityTestAuthority("seam-published-first-occupier", workspace, "seam-published-first-occupier-instance")
	_, occupier := registerIdentityForTest(t, ledger, occupierAuthority, requested)
	fixture := prepareTaskWorkerPreRegistrationPublication(t, ledger, taskID, "seam-published-first-project", principal, workspace, requested, instance)
	var taskStatus string
	var reviewerClaims, bindingCount int
	if err := ledger.db.QueryRow(`SELECT status FROM task_ledger_tasks WHERE id=?`, taskID).Scan(&taskStatus); err != nil {
		t.Fatal(err)
	}
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_reviewer_claims WHERE task_id=?`, taskID).Scan(&reviewerClaims); err != nil {
		t.Fatal(err)
	}
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_worker_task_bindings WHERE task_id=?`, taskID).Scan(&bindingCount); err != nil {
		t.Fatal(err)
	}
	if taskStatus != "execution_succeeded" || reviewerClaims != 0 || bindingCount != 0 {
		t.Fatalf("publication fixture did not stop before reviewer custody: status=%q claims=%d bindings=%d", taskStatus, reviewerClaims, bindingCount)
	}
	authority := identityTestAuthority(principal, workspace, instance)
	response, identity := registerIdentityForTest(t, ledger, authority, requested)
	updateID := anyToString(anyMap(response["identity_update"])["update_id"])
	if updateID == "" || identity.IdentityUpdateGeneration != 1 {
		t.Fatalf("published result collision did not issue an update: response=%#v identity=%#v", response, identity)
	}
	acknowledgeIdentityForTest(t, ledger, authority, updateID)
	identity, lookupErr := ledger.workerIdentityByAuthority(context.Background(), authority)
	if lookupErr != nil {
		t.Fatal(lookupErr)
	}
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_worker_task_bindings WHERE task_id=? AND identity_id=?`, taskID, identity.IdentityID).Scan(&bindingCount); err != nil || bindingCount != 1 {
		t.Fatalf("ACK did not adopt completed publication before reviewer claim: count=%d err=%v", bindingCount, err)
	}
	claimTaskWorkerPreRegistrationReview(t, ledger, fixture, principal)
	review := requestTaskWorkerPreRegistrationChanges(t, ledger, fixture, principal)
	if err := ledger.close(); err != nil {
		t.Fatal(err)
	}
	if err := second.close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatalf("restart published-first review seam: %v", err)
	}
	defer restarted.close()
	restartedIdentity, err := restarted.workerIdentityByAuthority(context.Background(), authority)
	if err != nil {
		t.Fatal(err)
	}
	assertTaskWorkerCanonicalRevisionAfterRestart(t, restarted, fixture, restartedIdentity, occupier, requested, workspace, review)
}

func TestTaskWorkerSeamRequestChangesHoldsUntilCollisionAckAdopts(t *testing.T) {
	ledger, second := identityTestLedgers(t)
	const (
		principal = "seam-review-first-principal"
		workspace = "seam-review-first-workspace"
		requested = "seam-review-first-worker"
		instance  = "seam-review-first-instance"
		taskID    = "seam-review-first-task"
	)
	occupierAuthority := identityTestAuthority("seam-review-first-occupier", workspace, "seam-review-first-occupier-instance")
	_, occupier := registerIdentityForTest(t, ledger, occupierAuthority, requested)
	fixture := prepareTaskWorkerPreRegistrationReview(t, ledger, taskID, "seam-review-first-project", principal, workspace, requested, instance)
	review := requestTaskWorkerPreRegistrationChanges(t, ledger, fixture, principal)
	var taskStatus, claimWorkerID string
	var claimEligible, bindingCount int
	if err := ledger.db.QueryRow(`SELECT status,claim_worker_id,claim_eligible FROM task_ledger_tasks WHERE id=?`, taskID).Scan(&taskStatus, &claimWorkerID, &claimEligible); err != nil {
		t.Fatal(err)
	}
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_worker_task_bindings WHERE task_id=?`, taskID).Scan(&bindingCount); err != nil {
		t.Fatal(err)
	}
	if taskStatus != "queued" || claimWorkerID != "" || claimEligible != 0 || bindingCount != 0 {
		t.Fatalf("unregistered revision was not held closed: status=%q worker=%q eligible=%d bindings=%d", taskStatus, claimWorkerID, claimEligible, bindingCount)
	}
	if claim, err := ledger.claimTask(context.Background(), requested, instance, workspace, taskID); err != nil || claim != nil {
		t.Fatalf("pre-registration owner claimed revision before identity ACK: claim=%#v err=%v", claim, err)
	}
	if claim, err := ledger.claimTask(context.Background(), requested, occupier.WorkerInstanceID, workspace, taskID); err != nil || claim != nil {
		t.Fatalf("occupier claimed held revision before identity ACK: claim=%#v err=%v", claim, err)
	}
	authority := identityTestAuthority(principal, workspace, instance)
	response, identity := registerIdentityForTest(t, ledger, authority, requested)
	updateID := anyToString(anyMap(response["identity_update"])["update_id"])
	if updateID == "" || identity.IdentityUpdateGeneration != 1 {
		t.Fatalf("review-first collision did not issue an update: response=%#v identity=%#v", response, identity)
	}
	acknowledgeIdentityForTest(t, ledger, authority, updateID)
	if err := ledger.close(); err != nil {
		t.Fatal(err)
	}
	if err := second.close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatalf("restart review-first seam: %v", err)
	}
	defer restarted.close()
	restartedIdentity, err := restarted.workerIdentityByAuthority(context.Background(), authority)
	if err != nil {
		t.Fatal(err)
	}
	assertTaskWorkerCanonicalRevisionAfterRestart(t, restarted, fixture, restartedIdentity, occupier, requested, workspace, review)
}

type taskWorkerAdoptionStateCase struct {
	name, label, taskStatus, attemptStatus string
	steps, finishSteps                     []string
	queuedBeforeACK                        bool
}

func taskWorkerAdoptionStateCases() []taskWorkerAdoptionStateCase {
	return []taskWorkerAdoptionStateCase{
		{name: "leased", label: "claimed", taskStatus: "leased", attemptStatus: "leased", finishSteps: []string{"heartbeat", "observe", "stage", "finalize", "claim-review", "request-changes"}},
		{name: "running", label: "running", taskStatus: "running", attemptStatus: "running", steps: []string{"heartbeat"}, finishSteps: []string{"observe", "stage", "finalize", "claim-review", "request-changes"}},
		{name: "waiting_for_input", label: "waiting", taskStatus: "waiting_for_input", attemptStatus: "waiting_for_input", steps: []string{"heartbeat", "wait"}, finishSteps: []string{"observe", "stage", "finalize", "claim-review", "request-changes"}},
		{name: "execution_observed", label: "observed", taskStatus: "execution_observed", attemptStatus: "execution_observed", steps: []string{"heartbeat", "observe"}, finishSteps: []string{"stage", "finalize", "claim-review", "request-changes"}},
		{name: "writeback_pending", label: "writeback-pending", taskStatus: "writeback_pending", attemptStatus: "execution_observed", steps: []string{"heartbeat", "observe", "stage"}, finishSteps: []string{"finalize", "claim-review", "request-changes"}},
		{name: "writeback_failed", label: "writeback-failed", taskStatus: "writeback_failed", attemptStatus: "execution_observed", steps: []string{"heartbeat", "observe", "stage", "fail-writeback"}, finishSteps: []string{"finalize", "claim-review", "request-changes"}},
		{name: "execution_succeeded", label: "published", taskStatus: "execution_succeeded", attemptStatus: "completed", steps: []string{"heartbeat", "observe", "stage", "finalize"}, finishSteps: []string{"claim-review", "request-changes"}},
		{name: "review_acknowledged", label: "ack-review", taskStatus: "review_pending", attemptStatus: "completed", steps: []string{"heartbeat", "observe", "stage", "finalize", "ack-review"}, finishSteps: []string{"claim-review", "request-changes"}},
		{name: "review_claimed", label: "claimed-review", taskStatus: "review_pending", attemptStatus: "completed", steps: []string{"heartbeat", "observe", "stage", "finalize", "claim-review"}, finishSteps: []string{"request-changes"}},
		{name: "review_blocked", label: "blocked-review", taskStatus: "review_blocked", attemptStatus: "completed", steps: []string{"heartbeat", "observe", "stage", "finalize", "claim-review", "block-review"}, finishSteps: []string{"answer-review"}},
		{name: "answered_queued", label: "answered-review", taskStatus: "queued", attemptStatus: "completed", steps: []string{"heartbeat", "observe", "stage", "finalize", "claim-review", "block-review", "answer-review"}, queuedBeforeACK: true},
		{name: "revision_queued", label: "revision", taskStatus: "queued", attemptStatus: "completed", steps: []string{"heartbeat", "observe", "stage", "finalize", "claim-review", "request-changes"}, queuedBeforeACK: true},
		{name: "retry_queued", label: "retry", taskStatus: "queued", attemptStatus: "execution_failed", steps: []string{"heartbeat", "fail-execution"}, queuedBeforeACK: true},
		{name: "quarantined", label: "quarantine", taskStatus: "quarantined", attemptStatus: "quarantined", steps: []string{"heartbeat", "quarantine"}, finishSteps: []string{"resolve-quarantine"}},
		{name: "quarantine_resolved_queued", label: "resolved-quarantine", taskStatus: "queued", attemptStatus: "quarantined", steps: []string{"heartbeat", "quarantine", "resolve-quarantine"}, queuedBeforeACK: true},
	}
}

func advanceTaskWorkerPreRegistrationTransition(t *testing.T, ledger *agentTaskDeliveryLedger, fixture taskWorkerPreRegistrationReviewFixture, principal, transition string) (taskWorkerPreRegistrationReviewFixture, map[string]any) {
	t.Helper()
	ctx := context.Background()
	switch transition {
	case "heartbeat":
		if _, err := ledger.heartbeat(ctx, fixture.Fence); err != nil {
			t.Fatalf("advance pre-registration attempt to running: %v", err)
		}
	case "wait":
		tx, err := ledger.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_attempts SET status='waiting_for_input' WHERE attempt_id=? AND status='running'`, fixture.Fence.AttemptID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET status='waiting_for_input' WHERE id=? AND status='running' AND active_attempt_id=?`, fixture.TaskID, fixture.Fence.AttemptID); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	case "observe":
		observeTaskWorkerPreRegistrationResult(t, ledger, fixture)
	case "stage":
		fixture = stageTaskWorkerPreRegistrationResult(t, ledger, fixture)
	case "fail-writeback":
		if _, err := ledger.finalizePublication(ctx, "publication-"+fixture.TaskID, "failed", "", "matrix writeback failed"); err != nil {
			t.Fatalf("advance pre-registration publication to writeback failure: %v", err)
		}
	case "finalize":
		fixture = finalizeTaskWorkerPreRegistrationResult(t, ledger, fixture)
	case "ack-review":
		return fixture, acknowledgeTaskWorkerPreRegistrationReview(t, ledger, fixture, principal)
	case "claim-review":
		claimTaskWorkerPreRegistrationReview(t, ledger, fixture, principal)
	case "block-review":
		review, err := ledger.reviewWithFence(ctx, fixture.TaskID, fixture.ResultID, principal, "block", "matrix review requires an answer", "", fixture.Fence.AttemptID, fixture.Fence.Generation)
		if err != nil {
			t.Fatalf("block pre-registration review: %v", err)
		}
		return fixture, review
	case "answer-review":
		if _, err := ledger.answerBlockingQuestion(ctx, fixture.TaskID, fixture.ResultID, fixture.DeliveryID, principal, "matrix blocking answer", fixture.Fence.AttemptID); err != nil {
			t.Fatalf("answer pre-registration blocked review: %v", err)
		}
	case "request-changes":
		return fixture, requestTaskWorkerPreRegistrationChanges(t, ledger, fixture, principal)
	case "fail-execution":
		exitCode := 17
		if _, err := ledger.observe(ctx, fixture.Fence, "failed", &exitCode, map[string]any{"source": "task-worker-adoption-state-matrix"}); err != nil {
			t.Fatalf("advance pre-registration attempt to retry queue: %v", err)
		}
	case "quarantine":
		if _, err := ledger.cancelAttempt(ctx, fixture.Fence, false, "matrix runner termination is unverified"); err != nil {
			t.Fatalf("advance pre-registration attempt to quarantine: %v", err)
		}
	case "resolve-quarantine":
		if _, err := ledger.resolveQuarantinedAttempt(ctx, fixture.Fence, true, "matrix runner process group terminated"); err != nil {
			t.Fatalf("resolve pre-registration quarantine: %v", err)
		}
	default:
		t.Fatalf("unsupported pre-registration transition %q", transition)
	}
	return fixture, nil
}

func advanceTaskWorkerPreRegistrationTransitions(t *testing.T, ledger *agentTaskDeliveryLedger, fixture taskWorkerPreRegistrationReviewFixture, principal string, transitions []string, review map[string]any) (taskWorkerPreRegistrationReviewFixture, map[string]any) {
	t.Helper()
	for _, transition := range transitions {
		var nextReview map[string]any
		fixture, nextReview = advanceTaskWorkerPreRegistrationTransition(t, ledger, fixture, principal, transition)
		if anyToString(nextReview["review_id"]) != "" {
			review = nextReview
		}
	}
	return fixture, review
}

func assertTaskWorkerPreRegistrationMatrixState(t *testing.T, ledger *agentTaskDeliveryLedger, fixture taskWorkerPreRegistrationReviewFixture, state taskWorkerAdoptionStateCase, bindingExpected bool) {
	t.Helper()
	var taskStatus, attemptStatus string
	var bindingCount int
	if err := ledger.db.QueryRow(`SELECT status FROM task_ledger_tasks WHERE id=?`, fixture.TaskID).Scan(&taskStatus); err != nil {
		t.Fatal(err)
	}
	if err := ledger.db.QueryRow(`SELECT status FROM task_ledger_attempts WHERE attempt_id=?`, fixture.Fence.AttemptID).Scan(&attemptStatus); err != nil {
		t.Fatal(err)
	}
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_worker_task_bindings WHERE task_id=?`, fixture.TaskID).Scan(&bindingCount); err != nil {
		t.Fatal(err)
	}
	expectedBindings := 0
	if bindingExpected {
		expectedBindings = 1
	}
	if taskStatus != state.taskStatus || attemptStatus != state.attemptStatus || bindingCount != expectedBindings {
		t.Fatalf("pre-registration matrix state mismatch: state=%q task=%q want=%q attempt=%q want=%q bindings=%d want=%d", state.name, taskStatus, state.taskStatus, attemptStatus, state.attemptStatus, bindingCount, expectedBindings)
	}
	if state.name == "review_acknowledged" {
		var reviewStatus, decision string
		var reviewerClaims int
		if err := ledger.db.QueryRow(`SELECT status,decision FROM task_ledger_reviews WHERE task_id=? AND result_id=?`, fixture.TaskID, fixture.ResultID).Scan(&reviewStatus, &decision); err != nil {
			t.Fatal(err)
		}
		if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_reviewer_claims WHERE task_id=? AND result_id=?`, fixture.TaskID, fixture.ResultID).Scan(&reviewerClaims); err != nil {
			t.Fatal(err)
		}
		if reviewStatus != "acknowledged" || decision != "acknowledge" || reviewerClaims != 0 {
			t.Fatalf("acknowledged pre-claim review evidence is not exact: status=%q decision=%q claims=%d", reviewStatus, decision, reviewerClaims)
		}
	}
	if state.name == "review_claimed" || state.name == "review_blocked" || state.name == "answered_queued" {
		var activeClaims int
		if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_reviewer_claims WHERE task_id=? AND result_id=? AND status='active'`, fixture.TaskID, fixture.ResultID).Scan(&activeClaims); err != nil {
			t.Fatal(err)
		}
		if activeClaims != 1 {
			t.Fatalf("review custody evidence is ambiguous: state=%q active_claims=%d", state.name, activeClaims)
		}
	}
	if state.name == "answered_queued" {
		var answers int
		if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_blocking_answers WHERE task_id=? AND result_id=? AND source_attempt_id=?`, fixture.TaskID, fixture.ResultID, fixture.Fence.AttemptID).Scan(&answers); err != nil {
			t.Fatal(err)
		}
		if answers != 1 {
			t.Fatalf("answered review evidence is ambiguous: answers=%d", answers)
		}
	}
}

func TestTaskWorkerSeamAdoptionMatrix(t *testing.T) {
	timings := []struct{ name, label string }{
		{name: "ack_before_boundary", label: "ack-before"},
		{name: "register_before_boundary_ack_after", label: "register-before"},
		{name: "register_and_ack_after_boundary", label: "ack-after"},
	}
	for _, stateCase := range taskWorkerAdoptionStateCases() {
		stateCase := stateCase
		for _, timingCase := range timings {
			timingCase := timingCase
			t.Run(stateCase.label+"/"+timingCase.label, func(t *testing.T) {
				state, timing := stateCase.name, timingCase.name
				ledger, second := identityTestLedgers(t)
				principal := "matrix-owner-" + state + "-" + timing
				workspace := "matrix-workspace-" + state + "-" + timing
				requested := "matrix-worker-" + state + "-" + timing
				instance := "matrix-instance-" + state + "-" + timing
				taskID := "matrix-task-" + state + "-" + timing
				authority := identityTestAuthority(principal, workspace, instance)
				_, occupier := registerIdentityForTest(t, ledger, identityTestAuthority("matrix-occupier-"+state+"-"+timing, workspace, "matrix-occupier-instance-"+state+"-"+timing), requested)
				fixture := prepareTaskWorkerPreRegistrationLease(t, ledger, taskID, "matrix-project-"+state+"-"+timing, principal, workspace, requested, instance)

				var identity agentWorkerIdentityRecord
				var updateID string
				register := func() {
					response, registered := registerIdentityForTest(t, ledger, authority, requested)
					identity = registered
					updateID = anyToString(anyMap(response["identity_update"])["update_id"])
					if updateID == "" || identity.IdentityUpdateGeneration != 1 || identity.CanonicalWorkerID == requested {
						t.Fatalf("matrix collision registration did not issue canonical update: response=%#v identity=%#v", response, identity)
					}
				}
				acknowledge := func() {
					acknowledgeIdentityForTest(t, ledger, authority, updateID)
					var err error
					identity, err = ledger.workerIdentityByAuthority(context.Background(), authority)
					if err != nil || identity.AcknowledgedGeneration != identity.IdentityUpdateGeneration {
						t.Fatalf("matrix collision ACK did not converge: identity=%#v err=%v", identity, err)
					}
				}

				predecessorSteps := stateCase.steps
				var boundaryStep []string
				if len(stateCase.steps) > 0 {
					predecessorSteps = stateCase.steps[:len(stateCase.steps)-1]
					boundaryStep = stateCase.steps[len(stateCase.steps)-1:]
				}
				var review map[string]any
				fixture, review = advanceTaskWorkerPreRegistrationTransitions(t, ledger, fixture, principal, predecessorSteps, review)
				if timing == "ack_before_boundary" {
					register()
					acknowledge()
				} else if timing == "register_before_boundary_ack_after" {
					register()
				}
				fixture, review = advanceTaskWorkerPreRegistrationTransitions(t, ledger, fixture, principal, boundaryStep, review)
				assertTaskWorkerPreRegistrationMatrixState(t, ledger, fixture, stateCase, timing == "ack_before_boundary")

				if timing == "register_and_ack_after_boundary" {
					register()
				}
				if timing != "ack_before_boundary" && stateCase.queuedBeforeACK {
					var claimEligible int
					if err := ledger.db.QueryRow(`SELECT claim_eligible FROM task_ledger_tasks WHERE id=?`, taskID).Scan(&claimEligible); err != nil {
						t.Fatal(err)
					}
					if claimEligible != 0 {
						t.Fatalf("unacknowledged collision successor was claimable: state=%q eligible=%d", state, claimEligible)
					}
					if claim, err := ledger.claimTask(context.Background(), requested, occupier.WorkerInstanceID, workspace, taskID); err != nil || claim != nil {
						t.Fatalf("requested-ID occupier claimed before collision ACK: state=%q claim=%#v err=%v", state, claim, err)
					}
				}
				if timing != "ack_before_boundary" {
					acknowledge()
				}

				var bindingIdentity, bindingWorker, bindingInstance, bindingState, bindingReceipt string
				var bindingGeneration int
				if err := ledger.db.QueryRow(`SELECT identity_id,worker_id,worker_instance_id,worker_identity_update_generation,state,rebind_receipt_digest FROM task_ledger_worker_task_bindings WHERE task_id=?`, taskID).Scan(&bindingIdentity, &bindingWorker, &bindingInstance, &bindingGeneration, &bindingState, &bindingReceipt); err != nil {
					t.Fatalf("matrix ACK did not adopt exact state %q: %v", state, err)
				}
				if bindingIdentity != identity.IdentityID || bindingWorker != identity.CanonicalWorkerID || bindingInstance != instance || bindingGeneration != identity.IdentityUpdateGeneration || bindingState != "bound" || bindingReceipt == "" {
					t.Fatalf("matrix adoption binding is not exact: identity=%q worker=%q instance=%q generation=%d state=%q receipt=%q", bindingIdentity, bindingWorker, bindingInstance, bindingGeneration, bindingState, bindingReceipt)
				}

				fixture, review = advanceTaskWorkerPreRegistrationTransitions(t, ledger, fixture, principal, stateCase.finishSteps, review)
				var queuedStatus, queuedWorker string
				var claimEligible int
				if err := ledger.db.QueryRow(`SELECT status,claim_worker_id,claim_eligible FROM task_ledger_tasks WHERE id=?`, taskID).Scan(&queuedStatus, &queuedWorker, &claimEligible); err != nil {
					t.Fatal(err)
				}
				if queuedStatus != "queued" || queuedWorker != identity.CanonicalWorkerID || claimEligible != 1 {
					t.Fatalf("matrix state did not converge to canonical queue: status=%q worker=%q eligible=%d", queuedStatus, queuedWorker, claimEligible)
				}

				if err := ledger.close(); err != nil {
					t.Fatal(err)
				}
				if err := second.close(); err != nil {
					t.Fatal(err)
				}
				restarted, err := newAgentTaskDeliveryLedgerFromEnv()
				if err != nil {
					t.Fatalf("restart matrix state %q timing %q: %v", state, timing, err)
				}
				defer restarted.close()
				restartedIdentity, err := restarted.workerIdentityByAuthority(context.Background(), authority)
				if err != nil {
					t.Fatal(err)
				}
				expectedReviewID := ""
				if len(stateCase.finishSteps) > 0 && stateCase.finishSteps[len(stateCase.finishSteps)-1] == "request-changes" || state == "revision_queued" {
					expectedReviewID = anyToString(review["review_id"])
				}
				assertTaskWorkerCanonicalClaimAfterRestart(t, restarted, fixture, restartedIdentity, occupier, requested, workspace, expectedReviewID)
			})
		}
	}
}

func TestTaskWorkerSeamAdoptionStateMachineAudit(t *testing.T) {
	// Every accepted task status is classified. Only stable states that retain
	// this attempt's execution custody or can create a later worker attempt are
	// adoption boundaries. Transaction-internal projections cannot interleave
	// with collision ACK under SQLite's sole-writer transaction. Downstream and
	// terminal states never return custody to an execution worker.
	statusDisposition := map[string]string{
		"queued": "adoption_boundary", "leased": "adoption_boundary", "running": "adoption_boundary", "waiting_for_input": "adoption_boundary",
		"execution_observed": "adoption_boundary", "writeback_pending": "adoption_boundary", "writeback_failed": "adoption_boundary",
		"execution_succeeded": "adoption_boundary", "review_pending": "adoption_boundary", "review_blocked": "adoption_boundary", "quarantined": "adoption_boundary",
		"execution_failed": "transaction_internal", "publication_pending": "transaction_internal", "result_published": "transaction_internal", "changes_requested": "transaction_internal",
		"accepted_for_integration": "downstream", "knowledge_accepted": "downstream", "integration_pending": "downstream", "approval_pending": "downstream",
		"canceled": "terminal", "rejected": "terminal", "superseded": "terminal", "unintegrated": "terminal", "integrated": "terminal", "integration_failed": "terminal", "dead_letter": "terminal",
	}
	knownStatuses := []string{
		"queued", "leased", "running", "waiting_for_input", "execution_observed", "execution_failed", "canceled", "quarantined",
		"publication_pending", "writeback_pending", "writeback_failed", "result_published", "execution_succeeded", "review_pending", "review_blocked",
		"accepted_for_integration", "changes_requested", "rejected", "superseded", "knowledge_accepted", "unintegrated", "integration_pending",
		"integrated", "integration_failed", "approval_pending", "dead_letter",
	}
	const expectedAcceptedTaskStatusCount = 26
	if got := len(knownStatuses); got != expectedAcceptedTaskStatusCount {
		t.Fatalf("state-machine audit accepted-status list drifted: got=%d want=%d", got, expectedAcceptedTaskStatusCount)
	}
	coveredStableStatuses := map[string]bool{}
	for _, state := range taskWorkerAdoptionStateCases() {
		coveredStableStatuses[state.taskStatus] = true
	}
	for _, status := range knownStatuses {
		if agentTaskStatus(status) != status {
			t.Fatalf("state-machine audit status is not accepted: %q", status)
		}
		disposition := statusDisposition[status]
		if disposition == "" {
			t.Fatalf("state-machine audit omitted status %q", status)
		}
		if disposition == "adoption_boundary" && !coveredStableStatuses[status] {
			t.Fatalf("stable custody status lacks an adoption case: %q", status)
		}
	}
	if len(statusDisposition) != len(knownStatuses) {
		t.Fatalf("state-machine audit has unrecognized status dispositions: dispositions=%d statuses=%d", len(statusDisposition), len(knownStatuses))
	}
	if got := len(statusDisposition); got != expectedAcceptedTaskStatusCount {
		t.Fatalf("state-machine audit disposition count drifted: got=%d want=%d", got, expectedAcceptedTaskStatusCount)
	}
	for from, targets := range agentTaskTransitionMatrix() {
		if statusDisposition[from] == "" {
			t.Fatalf("transition source lacks a status disposition: %q", from)
		}
		for to, allowed := range targets {
			if !allowed || statusDisposition[to] == "" || !agentTaskAllowedTransition(from, to) {
				t.Fatalf("transition audit mismatch: %s -> %s allowed=%t", from, to, allowed)
			}
		}
	}

	// These are the externally observable committed boundaries exercised by the
	// matrix. Several collapse two declared transitions into one SQLite commit;
	// listing them here prevents a declared-only status from being mistaken for
	// an ACK-interleavable state.
	committedBoundaries := []struct{ operation, from, to string }{
		{"claim", "queued", "leased"}, {"heartbeat", "leased", "running"}, {"wait", "running", "waiting_for_input"},
		{"observe", "running", "execution_observed"}, {"stage", "execution_observed", "writeback_pending"},
		{"writeback-failure", "writeback_pending", "writeback_failed"}, {"finalize", "writeback_pending", "execution_succeeded"},
		{"finalize-after-failure", "writeback_failed", "execution_succeeded"}, {"acknowledge-review", "execution_succeeded", "review_pending"},
		{"claim-review", "execution_succeeded", "review_pending"}, {"block-review", "review_pending", "review_blocked"},
		{"answer-review", "review_blocked", "queued"}, {"request-changes", "review_pending", "queued"},
		{"retry", "running", "queued"}, {"quarantine", "running", "quarantined"}, {"resolve-quarantine", "quarantined", "queued"},
	}
	for _, boundary := range committedBoundaries {
		if statusDisposition[boundary.from] == "" || statusDisposition[boundary.to] != "adoption_boundary" || !coveredStableStatuses[boundary.to] {
			t.Fatalf("committed boundary lacks adoption coverage: %s %s -> %s", boundary.operation, boundary.from, boundary.to)
		}
	}
}

func TestTaskWorkerSeamAdoptionRejectsAmbiguousEvidence(t *testing.T) {
	type rejectionCase struct {
		name        string
		ackFails    bool
		bindingRows int
		mutate      func(*testing.T, *agentTaskDeliveryLedger, taskWorkerPreRegistrationReviewFixture, agentWorkerIdentityRecord, agentWorkerIdentityRecord)
	}
	cases := []rejectionCase{
		{
			name: "later-attempt",
			mutate: func(t *testing.T, ledger *agentTaskDeliveryLedger, fixture taskWorkerPreRegistrationReviewFixture, _, _ agentWorkerIdentityRecord) {
				t.Helper()
				now := agentTaskNow()
				_, err := ledger.db.Exec(`INSERT INTO task_ledger_attempts(attempt_id,task_id,attempt_number,lease_id,generation,worker_id,worker_instance_id,worker_identity_update_generation,status,context_pack_hash,session_id,lease_expires_at,claimed_at,heartbeat_at,completed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
					"later-"+fixture.TaskID, fixture.TaskID, fixture.Fence.Generation+1, "later-lease-"+fixture.TaskID, fixture.Fence.Generation+1,
					"foreign-later-worker", "foreign-later-instance", 0, "canceled", fixture.ContextPackHash, fixture.TaskID+"-session", now, now, now, now)
				if err != nil {
					t.Fatalf("insert later-attempt ambiguity: %v", err)
				}
			},
		},
		{
			name:     "ambiguous-result",
			ackFails: true,
			mutate: func(t *testing.T, ledger *agentTaskDeliveryLedger, fixture taskWorkerPreRegistrationReviewFixture, _, _ agentWorkerIdentityRecord) {
				t.Helper()
				payload := map[string]any{"result_id": "ambiguous-" + fixture.ResultID, "task_id": fixture.TaskID, "attempt_id": fixture.Fence.AttemptID}
				if _, err := ledger.db.Exec(`INSERT INTO task_ledger_results(result_id,task_id,attempt_id,schema_id,status,execution_observed,payload_json,digest,created_at,immutable) VALUES(?,?,?,?,?,?,?,?,?,1)`,
					payload["result_id"], fixture.TaskID, fixture.Fence.AttemptID, agentTaskResultManifestContractID, "result_published", 1, encodeAgentTaskJSON(payload), agentTaskDigest(payload), agentTaskNow()); err != nil {
					t.Fatalf("insert ambiguous result evidence: %v", err)
				}
			},
		},
		{
			name:     "mutated-result-payload",
			ackFails: true,
			mutate: func(t *testing.T, ledger *agentTaskDeliveryLedger, fixture taskWorkerPreRegistrationReviewFixture, _, _ agentWorkerIdentityRecord) {
				t.Helper()
				if _, err := ledger.db.Exec(`UPDATE task_ledger_results SET payload_json=json_set(payload_json,'$.summary','tampered after immutable commit') WHERE result_id=?`, fixture.ResultID); err != nil {
					t.Fatalf("mutate immutable result payload: %v", err)
				}
			},
		},
		{
			name:     "foreign-publication-link",
			ackFails: true,
			mutate: func(t *testing.T, ledger *agentTaskDeliveryLedger, fixture taskWorkerPreRegistrationReviewFixture, _, _ agentWorkerIdentityRecord) {
				t.Helper()
				foreignAttemptID := "foreign-attempt-" + fixture.TaskID
				now := agentTaskNow()
				if _, err := ledger.db.Exec(`INSERT INTO task_ledger_attempts(attempt_id,task_id,attempt_number,lease_id,generation,worker_id,worker_instance_id,worker_identity_update_generation,status,context_pack_hash,session_id,lease_expires_at,claimed_at,heartbeat_at,completed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
					foreignAttemptID, fixture.TaskID, 0, "foreign-lease-"+fixture.TaskID, 0, "foreign-worker", "foreign-instance", 0, "canceled", fixture.ContextPackHash, fixture.TaskID+"-foreign-session", now, now, now, now); err != nil {
					t.Fatalf("insert foreign publication attempt: %v", err)
				}
				if _, err := ledger.db.Exec(`UPDATE task_ledger_publications SET attempt_id=? WHERE publication_id=?`, foreignAttemptID, "publication-"+fixture.TaskID); err != nil {
					t.Fatalf("insert foreign publication evidence: %v", err)
				}
			},
		},
		{
			name:        "foreign-binding",
			ackFails:    true,
			bindingRows: 1,
			mutate: func(t *testing.T, ledger *agentTaskDeliveryLedger, fixture taskWorkerPreRegistrationReviewFixture, _, occupier agentWorkerIdentityRecord) {
				t.Helper()
				tx, err := ledger.db.BeginTx(context.Background(), nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := bindWorkerIdentityTaskTx(context.Background(), tx, fixture.TaskID, occupier, occupier.RequestedWorkerID, 0); err != nil {
					_ = tx.Rollback()
					t.Fatalf("bind foreign identity ambiguity: %v", err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:        "stale-binding",
			ackFails:    true,
			bindingRows: 1,
			mutate: func(t *testing.T, ledger *agentTaskDeliveryLedger, fixture taskWorkerPreRegistrationReviewFixture, identity, _ agentWorkerIdentityRecord) {
				t.Helper()
				now := agentTaskNow()
				if _, err := ledger.db.Exec(`INSERT INTO task_ledger_worker_task_bindings(task_id,identity_id,principal_id,workspace_id,requested_worker_id,canonical_worker_id,worker_id,worker_instance_id,worker_identity_update_generation,state,rebind_update_id,rebind_receipt_digest,rebind_acknowledged_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,'bound','','','',?,?)`, fixture.TaskID, identity.IdentityID, identity.PrincipalID, identity.WorkspaceID, identity.RequestedWorkerID, identity.CanonicalWorkerID, identity.CanonicalWorkerID, identity.WorkerInstanceID, identity.IdentityUpdateGeneration+1, now, now); err != nil {
					t.Fatalf("insert stale identity ambiguity: %v", err)
				}
			},
		},
		{
			name:     "foreign-projection",
			ackFails: true,
			mutate: func(t *testing.T, ledger *agentTaskDeliveryLedger, fixture taskWorkerPreRegistrationReviewFixture, _, _ agentWorkerIdentityRecord) {
				t.Helper()
				if _, err := ledger.db.Exec(`UPDATE task_ledger_tasks SET claim_worker_id='foreign-worker-projection' WHERE id=?`, fixture.TaskID); err != nil {
					t.Fatalf("insert foreign task projection: %v", err)
				}
			},
		},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			ledger, _ := identityTestLedgers(t)
			principal := "ambiguity-owner-" + testCase.name
			workspace := "ambiguity-workspace-" + testCase.name
			requested := "ambiguity-worker-" + testCase.name
			instance := "ambiguity-instance-" + testCase.name
			authority := identityTestAuthority(principal, workspace, instance)
			_, occupier := registerIdentityForTest(t, ledger, identityTestAuthority("ambiguity-occupier-"+testCase.name, workspace, "ambiguity-occupier-instance-"+testCase.name), requested)
			fixture := prepareTaskWorkerPreRegistrationPublication(t, ledger, "ambiguity-task-"+testCase.name, "ambiguity-project-"+testCase.name, principal, workspace, requested, instance)
			acknowledgeTaskWorkerPreRegistrationReview(t, ledger, fixture, principal)
			response, identity := registerIdentityForTest(t, ledger, authority, requested)
			updateID := anyToString(anyMap(response["identity_update"])["update_id"])
			update, err := ledger.readWorkerIdentityUpdate(context.Background(), authority, updateID)
			if err != nil {
				t.Fatal(err)
			}
			testCase.mutate(t, ledger, fixture, identity, occupier)

			_, ackErr := ledger.acknowledgeWorkerIdentityUpdate(context.Background(), identityTestAckPayload(update, authority), authority)
			if testCase.ackFails && ackErr == nil {
				t.Fatal("ambiguous adoption ACK unexpectedly succeeded")
			}
			if !testCase.ackFails && ackErr != nil {
				t.Fatalf("non-adopting ACK did not fail closed cleanly: %v", ackErr)
			}
			stored, err := ledger.readWorkerIdentityUpdate(context.Background(), authority, updateID)
			if err != nil {
				t.Fatal(err)
			}
			if testCase.ackFails && stored.State == agentWorkerIdentityStateAcknowledged {
				t.Fatal("failed ambiguous ACK was committed")
			}
			if !testCase.ackFails && stored.State != agentWorkerIdentityStateAcknowledged {
				t.Fatalf("later-attempt no-adoption ACK did not commit exact identity receipt: state=%q", stored.State)
			}
			var bindingRows int
			if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_worker_task_bindings WHERE task_id=?`, fixture.TaskID).Scan(&bindingRows); err != nil {
				t.Fatal(err)
			}
			if bindingRows != testCase.bindingRows {
				t.Fatalf("ambiguous adoption changed binding rows: got=%d want=%d", bindingRows, testCase.bindingRows)
			}
		})
	}
}

func TestTaskWorkerSeamAdoptionRejectsIncompleteAnsweredReviewEvidence(t *testing.T) {
	ledger, _ := identityTestLedgers(t)
	const (
		principal = "answered-ambiguity-owner"
		workspace = "answered-ambiguity-workspace"
		requested = "answered-ambiguity-worker"
		instance  = "answered-ambiguity-instance"
		taskID    = "answered-ambiguity-task"
	)
	registerIdentityForTest(t, ledger, identityTestAuthority("answered-ambiguity-occupier", workspace, "answered-ambiguity-occupier-instance"), requested)
	fixture := prepareTaskWorkerPreRegistrationLease(t, ledger, taskID, "answered-ambiguity-project", principal, workspace, requested, instance)
	fixture, _ = advanceTaskWorkerPreRegistrationTransitions(t, ledger, fixture, principal, []string{"heartbeat", "observe", "stage", "finalize", "claim-review", "block-review", "answer-review"}, nil)
	var claimEligible int
	if err := ledger.db.QueryRow(`SELECT claim_eligible FROM task_ledger_tasks WHERE id=?`, taskID).Scan(&claimEligible); err != nil || claimEligible != 0 {
		t.Fatalf("unbound answered review did not fail closed: eligible=%d err=%v", claimEligible, err)
	}
	authority := identityTestAuthority(principal, workspace, instance)
	response, identity := registerIdentityForTest(t, ledger, authority, requested)
	updateID := anyToString(anyMap(response["identity_update"])["update_id"])
	update, err := ledger.readWorkerIdentityUpdate(context.Background(), authority, updateID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.db.Exec(`DELETE FROM task_ledger_blocking_answers WHERE task_id=? AND result_id=? AND source_attempt_id=?`, taskID, fixture.ResultID, fixture.Fence.AttemptID); err != nil {
		t.Fatalf("remove exact blocking answer evidence: %v", err)
	}
	if _, err := ledger.acknowledgeWorkerIdentityUpdate(context.Background(), identityTestAckPayload(update, authority), authority); err == nil || !strings.Contains(err.Error(), "ambiguous lifecycle evidence") {
		t.Fatalf("incomplete answered review ACK did not fail closed: %v", err)
	}
	stored, err := ledger.readWorkerIdentityUpdate(context.Background(), authority, updateID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State == agentWorkerIdentityStateAcknowledged {
		t.Fatal("incomplete answered review ACK was committed")
	}
	var bindings int
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_worker_task_bindings WHERE task_id=? AND identity_id=?`, taskID, identity.IdentityID).Scan(&bindings); err != nil || bindings != 0 {
		t.Fatalf("incomplete answered review was adopted: bindings=%d err=%v", bindings, err)
	}
}
