package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const repairTaskAuthorizationHeader = "X-Test-Task-Authorization"

func repairTaskRouteJSON(t *testing.T, server *server, method, path string, payload map[string]any, runtimeToken, apiKey string, workerCredential ...string) (int, map[string]any) {
	t.Helper()
	if payload == nil {
		payload = map[string]any{}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	if runtimeToken != "" {
		request.Header.Set(repairTaskAuthorizationHeader, runtimeToken)
	}
	if apiKey != "" {
		request.Header.Set("X-Api-Key", apiKey)
	}
	if len(workerCredential) > 0 && strings.TrimSpace(workerCredential[0]) != "" {
		request.Header.Set(workerInstanceCredentialHeader, workerCredential[0])
		request.Header.Set("X-Worker-Instance-ID", anyToString(payload["worker_instance_id"]))
	}
	response := httptest.NewRecorder()
	server.agentsTasksRoute(response, request)
	decoded := map[string]any{}
	if response.Body.Len() > 0 {
		if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("decode %s %s response: %v (%s)", method, path, err, response.Body.String())
		}
	}
	return response.Code, decoded
}

func repairTaskConfigureRuntimeSigner(t *testing.T, server *server, keyID string) func(subject, workspace, role, jti string) string {
	t.Helper()
	authorizations := map[string]agentTaskRouteAuth{}
	server.taskSignedRouteAuth = func(request *http.Request) (agentTaskRouteAuth, bool, error) {
		token := strings.TrimSpace(request.Header.Get(repairTaskAuthorizationHeader))
		if token == "" {
			return agentTaskRouteAuth{}, false, nil
		}
		auth, ok := authorizations[token]
		if !ok {
			return agentTaskRouteAuth{}, false, errors.New("authenticated task test authority is invalid")
		}
		return auth, true, nil
	}
	return func(subject, workspace, role, jti string) string {
		token := "task-test-authority:" + keyID + ":" + jti
		authorizations[token] = agentTaskRouteAuth{Principal: subject, Workspace: workspace, Role: role, Signed: true, RequestBound: true}
		return token
	}
}

type repairTaskReviewFixture struct {
	taskID     string
	resultID   string
	deliveryID string
	digest     string
	fence      agentTaskFence
}

func repairTaskReviewReady(t *testing.T, ledger *agentTaskDeliveryLedger, suffix, project, owner string) repairTaskReviewFixture {
	t.Helper()
	fence, publication := testAgentTaskStagePublication(t, ledger, suffix, project, owner, "sess_"+suffix, nil)
	committed, err := ledger.finalizePublication(context.Background(), anyToString(publication["publication_id"]), "committed", "receipt-"+suffix, "")
	if err != nil {
		t.Fatalf("finalize review fixture: %v", err)
	}
	resultID := anyToString(committed["result_id"])
	deliveries := anySlice(committed["deliveries"])
	if len(deliveries) != 1 {
		t.Fatalf("review fixture delivery count=%d", len(deliveries))
	}
	deliveryID := anyToString(anyMap(deliveries[0])["delivery_id"])
	if _, err := ledger.claimReview(context.Background(), fence.TaskID, resultID, deliveryID, owner); err != nil {
		t.Fatalf("claim review fixture: %v", err)
	}
	return repairTaskReviewFixture{
		taskID: fence.TaskID, resultID: resultID, deliveryID: deliveryID,
		digest: anyToString(anyMap(committed["result"])["result_digest"]), fence: fence,
	}
}

func TestAgentTaskP1ExplicitPublicationIdempotencySurvivesGoPythonRestart(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	fence, claim := hardeningObservedAgentTask(t, ledger, "p1-explicit-publication", "p1-publication-project", "p1-publication-owner", "sess_p1_publication")
	request := hardeningPublicationRequest(fence, claim, "p1-explicit-publication", []any{map[string]any{"name": "proof.txt", "media_type": "text/plain", "content": "canonical artifact proof"}})
	explicitKey := anyToString(request["idempotency_key"])
	anyMap(request["result"])["idempotency_key"] = explicitKey
	mismatched := cloneAnyMap(request)
	mismatchedResult := cloneAnyMap(anyMap(request["result"]))
	mismatchedResult["idempotency_key"] = explicitKey + "-mismatched-copy"
	mismatched["result"] = mismatchedResult
	if _, err := ledger.stagePublication(context.Background(), fence, mismatched); err == nil || !strings.Contains(err.Error(), "copies do not match") {
		t.Fatalf("mismatched publication idempotency copies accepted: %v", err)
	}
	staged, err := ledger.stagePublication(context.Background(), fence, request)
	if err != nil {
		t.Fatalf("stage explicit publication: %v", err)
	}
	if anyToString(staged["idempotency_key"]) != explicitKey {
		t.Fatalf("explicit publication key=%q want %q", anyToString(staged["idempotency_key"]), explicitKey)
	}
	var intentDigest string
	if err := ledger.db.QueryRowContext(context.Background(), `SELECT intent_digest FROM task_ledger_publications WHERE publication_id=?`, anyToString(staged["publication_id"])).Scan(&intentDigest); err != nil || !agentTaskCanonicalSHA256(intentDigest) {
		t.Fatalf("publication intent digest was not persisted canonically: %q %v", intentDigest, err)
	}
	if anyToString(staged["intent_digest"]) != intentDigest {
		t.Fatalf("publication response did not expose persisted intent digest: %#v", staged)
	}
	if err := ledger.close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatalf("restart explicit publication ledger: %v", err)
	}
	t.Cleanup(func() { _ = restarted.close() })
	replayed, err := restarted.stagePublication(context.Background(), fence, request)
	if err != nil {
		t.Fatalf("replay explicit publication after restart: %v", err)
	}
	if anyToString(replayed["publication_id"]) != anyToString(staged["publication_id"]) || anyToString(replayed["idempotency_key"]) != explicitKey {
		t.Fatalf("explicit publication replay changed binding: staged=%#v replayed=%#v", staged, replayed)
	}
	attemptContextReplay := cloneAnyMap(request)
	attemptContextResult := cloneAnyMap(anyMap(request["result"]))
	contextPackHash := anyToString(attemptContextResult["context_pack_hash"])
	delete(attemptContextResult, "context_pack_hash")
	attemptContextReplay["result"] = attemptContextResult
	attemptContextReplay["attempt"] = map[string]any{"context_pack_hash": contextPackHash}
	if replayed, err := restarted.stagePublication(context.Background(), fence, attemptContextReplay); err != nil || anyToString(replayed["publication_id"]) != anyToString(staged["publication_id"]) {
		t.Fatalf("canonical attempt-context replay changed binding: %#v %v", replayed, err)
	}
	changedAttemptContext := cloneAnyMap(attemptContextReplay)
	changedAttemptContext["attempt"] = map[string]any{"context_pack_hash": "sha256:" + strings.Repeat("f", 64)}
	if _, err := restarted.stagePublication(context.Background(), fence, changedAttemptContext); err == nil || !strings.Contains(err.Error(), "publication intent") {
		t.Fatalf("changed attempt-context publication replay accepted: %v", err)
	}
	conflict := cloneAnyMap(request)
	conflict["idempotency_key"] = explicitKey + "-foreign"
	if _, err := restarted.stagePublication(context.Background(), fence, conflict); err == nil || !strings.Contains(err.Error(), "idempotency") {
		t.Fatalf("conflicting explicit publication replay accepted: %v", err)
	}
	changedBody := cloneAnyMap(request)
	changedBodyResult := cloneAnyMap(anyMap(request["result"]))
	changedBodyResult["output"] = "different immutable output"
	changedBody["result"] = changedBodyResult
	if _, err := restarted.stagePublication(context.Background(), fence, changedBody); err == nil || !strings.Contains(err.Error(), "publication intent") {
		t.Fatalf("changed publication body replay accepted: %v", err)
	}
	changedArtifact := cloneAnyMap(request)
	changedArtifact["artifacts"] = []any{map[string]any{"name": "proof.txt", "media_type": "text/plain", "content": "different artifact proof"}}
	if _, err := restarted.stagePublication(context.Background(), fence, changedArtifact); err == nil || !strings.Contains(err.Error(), "publication intent") {
		t.Fatalf("changed publication artifact replay accepted: %v", err)
	}
	if _, err := restarted.finalizePublication(context.Background(), anyToString(staged["publication_id"]), "committed", "p1-explicit-publication-receipt", ""); err != nil {
		t.Fatalf("finalize explicit publication: %v", err)
	}
	postTerminalReplay, err := restarted.stagePublication(context.Background(), fence, request)
	if err != nil || anyToString(postTerminalReplay["publication_id"]) != anyToString(staged["publication_id"]) {
		t.Fatalf("terminal publication replay lost canonical intent: %#v %v", postTerminalReplay, err)
	}
	reconciled, err := restarted.publicationForExactFence(context.Background(), fence, explicitKey)
	if err != nil {
		t.Fatalf("reconcile explicit publication after restart: %v", err)
	}
	assertAgentTaskPublicationReconciliationFields(t, reconciled)
	assertAgentTaskPublicationReconciliationPythonConsumer(t, reconciled)
}

func TestAgentTaskP1PublicationRunnerExitPolicyIsBoundAcrossRestart(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	taskID, project, owner := "p1-runner-exit-intent", "p1-runner-exit-project", "p1-runner-exit-owner"
	if _, err := ledger.submit(context.Background(), testAgentTaskManifest(taskID, project, owner, "sess_p1_runner_exit")); err != nil {
		t.Fatal(err)
	}
	claim, err := ledger.claimNext(context.Background(), "p1-runner-exit-worker", "p1-runner-exit-instance", "")
	if err != nil || claim == nil {
		t.Fatalf("claim runner-exit fixture: %#v %v", claim, err)
	}
	fence := testAgentTaskFenceFromClaim(t, claim)
	if _, err := ledger.heartbeat(context.Background(), fence); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.observe(context.Background(), fence, "succeeded", nil, map[string]any{"source": "runner-exit-intent"}); err != nil {
		t.Fatal(err)
	}
	request := hardeningPublicationRequest(fence, claim, taskID, nil)
	request["runner_exit_required"] = false
	staged, err := ledger.stagePublication(context.Background(), fence, request)
	if err != nil {
		t.Fatalf("stage publication with explicitly optional runner exit: %v", err)
	}
	if err := ledger.close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.close() })
	if replayed, err := restarted.stagePublication(context.Background(), fence, request); err != nil || anyToString(replayed["publication_id"]) != anyToString(staged["publication_id"]) {
		t.Fatalf("exact runner-exit policy replay failed after restart: %#v %v", replayed, err)
	}
	stricter := cloneAnyMap(request)
	stricter["runner_exit_required"] = true
	if _, err := restarted.stagePublication(context.Background(), fence, stricter); err == nil || !strings.Contains(err.Error(), "publication intent") {
		t.Fatalf("false-to-true runner-exit policy replay bypassed immutable intent: %v", err)
	}
	defaulted := cloneAnyMap(request)
	delete(defaulted, "runner_exit_required")
	if _, err := restarted.stagePublication(context.Background(), fence, defaulted); err == nil || !strings.Contains(err.Error(), "publication intent") {
		t.Fatalf("local-default runner-exit policy replay bypassed immutable intent: %v", err)
	}
}

func TestAgentTaskP1ConcurrentPreparedPublicationWinnerRejectsDifferentIntentAndKey(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	fence, claim := hardeningObservedAgentTask(t, ledger, "p1-publication-winner", "p1-publication-winner-project", "p1-publication-winner-owner", "sess_p1_publication_winner")
	winnerRequest := hardeningPublicationRequest(fence, claim, "p1-publication-winner", nil)
	winnerRequest["runner_exit_required"] = false
	differentBodyRequest := cloneAnyMap(winnerRequest)
	differentBodyResult := cloneAnyMap(anyMap(winnerRequest["result"]))
	differentBodyResult["output"] = "adversarial concurrently prepared output"
	differentBodyRequest["result"] = differentBodyResult
	differentKeyRequest := cloneAnyMap(winnerRequest)
	differentKeyRequest["idempotency_key"] = anyToString(winnerRequest["idempotency_key"]) + "-different"
	differentRunnerExitRequest := cloneAnyMap(winnerRequest)
	differentRunnerExitRequest["runner_exit_required"] = true

	winner, err := ledger.preparePublication(context.Background(), fence, winnerRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer winner.close()
	contender, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = contender.close() })
	differentBody, err := contender.preparePublication(context.Background(), fence, differentBodyRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer differentBody.close()
	differentKey, err := contender.preparePublication(context.Background(), fence, differentKeyRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer differentKey.close()
	differentRunnerExit, err := contender.preparePublication(context.Background(), fence, differentRunnerExitRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer differentRunnerExit.close()

	publicationID, err := ledger.commitPreparedPublication(context.Background(), fence, winner)
	if err != nil || publicationID == "" {
		t.Fatalf("commit concurrent winner: %q %v", publicationID, err)
	}
	if _, err := contender.commitPreparedPublication(context.Background(), fence, differentBody); err == nil || !strings.Contains(err.Error(), "publication intent") {
		t.Fatalf("concurrently prepared different body was accepted: %v", err)
	}
	if _, err := contender.commitPreparedPublication(context.Background(), fence, differentKey); err == nil || !strings.Contains(err.Error(), "different immutable task evidence") {
		t.Fatalf("concurrently prepared different key was accepted: %v", err)
	}
	if _, err := contender.commitPreparedPublication(context.Background(), fence, differentRunnerExit); err == nil || !strings.Contains(err.Error(), "publication intent") {
		t.Fatalf("concurrently prepared different runner-exit policy was accepted: %v", err)
	}
	if testAgentTaskCount(t, ledger, "task_ledger_publications") != 1 || testAgentTaskCount(t, ledger, "task_ledger_results") != 1 {
		t.Fatal("losing prepared publication mutated immutable winner rows")
	}
}

func TestAgentTaskP1SignedWorkerOwnsExactActiveFenceEndToEnd(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	project, owner, worker := "p1-signed-project", "p1-signed-owner", "p1-signed-worker"
	if _, err := ledger.submit(context.Background(), testAgentTaskManifest("p1-signed-publish", project, owner, "sess_p1_signed")); err != nil {
		t.Fatal(err)
	}
	server, _, _ := testAgentTaskServerWithMemory(t, ledger, project, owner, "sess_p1_signed")
	sign := repairTaskConfigureRuntimeSigner(t, server, "p1-signed-worker-key")
	workerToken := sign(worker, "workspace-"+project, "member", "p1-signed-worker-token")
	foreignToken := sign("p1-foreign-worker", "workspace-"+project, "member", "p1-foreign-worker-token")
	workerCredential := strings.Repeat("c", workerInstanceCredentialBytes*2)
	registration := map[string]any{"requested_worker_id": worker, "worker_instance_id": "p1-signed-instance"}
	if status, response := repairTaskRouteJSON(t, server, http.MethodPost, "/agents/workers/register", registration, workerToken, "", workerCredential); status != http.StatusOK {
		t.Fatalf("register signed worker identity status=%d response=%#v", status, response)
	}

	status, claim := repairTaskRouteJSON(t, server, http.MethodPost, "/agents/tasks/next", map[string]any{"requested_worker_id": worker, "worker_instance_id": "p1-signed-instance"}, workerToken, "", workerCredential)
	if status != http.StatusOK || anyMap(claim["attempt"]) == nil {
		t.Fatalf("signed worker claim status=%d response=%#v", status, claim)
	}
	fence := testAgentTaskFenceFromClaim(t, claim)
	if fence.WorkerID != worker {
		t.Fatalf("signed claim worker=%q want %q", fence.WorkerID, worker)
	}
	if status, _ := repairTaskRouteJSON(t, server, http.MethodPost, "/agents/tasks/"+fence.TaskID+"/heartbeat", fencePayload(fence), foreignToken, ""); status == http.StatusOK {
		t.Fatal("foreign signed worker mutated the active heartbeat fence")
	}
	attempt, err := ledger.attempt(context.Background(), fence.AttemptID)
	if err != nil || anyToString(attempt["status"]) != "leased" {
		t.Fatalf("foreign heartbeat changed attempt: %#v %v", attempt, err)
	}
	if status, response := repairTaskRouteJSON(t, server, http.MethodPost, "/agents/tasks/"+fence.TaskID+"/heartbeat", fencePayload(fence), workerToken, "", workerCredential); status != http.StatusOK {
		t.Fatalf("signed heartbeat status=%d response=%#v", status, response)
	}
	observation := fencePayload(fence)
	observation["runner_status"] = "succeeded"
	observation["exit_code"] = 0
	observation["metadata"] = map[string]any{"source": "signed-worker-e2e"}
	if status, response := repairTaskRouteJSON(t, server, http.MethodPost, "/agents/tasks/"+fence.TaskID+"/observe", observation, workerToken, "", workerCredential); status != http.StatusOK {
		t.Fatalf("signed observation status=%d response=%#v", status, response)
	}
	publicationRequest := hardeningPublicationRequest(fence, claim, "p1-signed-publish", nil)
	for field, value := range fencePayload(fence) {
		publicationRequest[field] = value
	}
	status, publication := repairTaskRouteJSON(t, server, http.MethodPost, "/agents/tasks/"+fence.TaskID+"/publish", publicationRequest, workerToken, "", workerCredential)
	if status != http.StatusOK || anyToString(publication["publication_id"]) == "" {
		t.Fatalf("signed publication status=%d response=%#v", status, publication)
	}
	if anyToString(anyMap(publication["publication_receipt"])["worker_id"]) != worker {
		t.Fatalf("signed publication lost worker identity: %#v", publication)
	}

	if _, err := ledger.submit(context.Background(), testAgentTaskManifest("p1-signed-cancel", project, owner, "sess_p1_signed_cancel")); err != nil {
		t.Fatal(err)
	}
	status, cancelClaim := repairTaskRouteJSON(t, server, http.MethodPost, "/agents/tasks/next", map[string]any{"requested_worker_id": worker, "worker_instance_id": "p1-signed-instance"}, workerToken, "", workerCredential)
	if status != http.StatusOK || anyMap(cancelClaim["attempt"]) == nil {
		t.Fatalf("signed cancellation claim status=%d response=%#v", status, cancelClaim)
	}
	cancelFence := testAgentTaskFenceFromClaim(t, cancelClaim)
	cancelRequest := fencePayload(cancelFence)
	cancelRequest["termination_verified"] = true
	cancelRequest["reason"] = "worker requested cancellation"
	status, canceled := repairTaskRouteJSON(t, server, http.MethodPost, "/agents/tasks/"+cancelFence.TaskID+"/cancel", cancelRequest, workerToken, "", workerCredential)
	if status != http.StatusOK || anyToString(anyMap(canceled["task"])["status"]) != "quarantined" || anyToBool(canceled["termination_verified"]) {
		t.Fatalf("signed worker cancellation did not fail closed: status=%d response=%#v", status, canceled)
	}
}

func TestAgentTaskP1OperatorResolvesExactQuarantineOnceAcrossRestart(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	taskID, project, owner := "p1-quarantine-resolution", "p1-quarantine-project", "p1-quarantine-owner"
	if _, err := ledger.submit(context.Background(), testAgentTaskManifest(taskID, project, owner, "sess_p1_quarantine")); err != nil {
		t.Fatal(err)
	}
	claim, err := ledger.claimNext(context.Background(), "p1-quarantine-worker", "p1-quarantine-instance", "")
	if err != nil || claim == nil {
		t.Fatalf("claim quarantine fixture: %#v %v", claim, err)
	}
	fence := testAgentTaskFenceFromClaim(t, claim)
	if _, err := ledger.db.ExecContext(context.Background(), `UPDATE task_ledger_attempts SET lease_expires_at=? WHERE attempt_id=?`, time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), fence.AttemptID); err != nil {
		t.Fatal(err)
	}
	if rows, err := ledger.recoverExpired(context.Background(), 10); err != nil || len(rows) != 1 {
		t.Fatalf("recover expired quarantine fixture: rows=%#v err=%v", rows, err)
	}
	if err := ledger.close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	server, _, _ := testAgentTaskServerWithMemory(t, restarted, project, owner, "sess_p1_quarantine")
	server.orchestratorAPIKey = "p1-quarantine-service-key"
	sign := repairTaskConfigureRuntimeSigner(t, server, "p1-quarantine-signer")
	ownerToken := sign(owner, "workspace-"+project, "owner", "p1-quarantine-owner-token")
	resolution := fencePayload(fence)
	resolution["termination_verified"] = true
	resolution["reason"] = "operator observed process group exit"
	path := "/agents/tasks/" + taskID + "/attempts/" + fence.AttemptID + "/termination"
	if status, _ := repairTaskRouteJSON(t, server, http.MethodPost, path, resolution, ownerToken, ""); status == http.StatusOK {
		t.Fatal("signed owner bypassed operator-only quarantine resolution")
	}
	stale := cloneAnyMap(resolution)
	stale["generation"] = fence.Generation + 1
	if status, _ := repairTaskRouteJSON(t, server, http.MethodPost, path, stale, "", server.orchestratorAPIKey); status == http.StatusOK {
		t.Fatal("stale quarantine generation was resolved")
	}
	status, resolved := repairTaskRouteJSON(t, server, http.MethodPost, path, resolution, "", server.orchestratorAPIKey)
	if status != http.StatusOK || !anyToBool(resolved["requeued"]) || anyToBool(resolved["idempotent_replay"]) {
		t.Fatalf("operator quarantine resolution status=%d response=%#v", status, resolved)
	}
	if anyToString(anyMap(resolved["task"])["status"]) != "queued" {
		t.Fatalf("resolved quarantine not queued: %#v", resolved)
	}
	eventsBeforeReplay := testAgentTaskCount(t, restarted, "task_ledger_events")
	if err := restarted.close(); err != nil {
		t.Fatal(err)
	}

	afterRestart, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = afterRestart.close() })
	restartServer, _, _ := testAgentTaskServerWithMemory(t, afterRestart, project, owner, "sess_p1_quarantine")
	restartServer.orchestratorAPIKey = "p1-quarantine-restart-key"
	status, replayed := repairTaskRouteJSON(t, restartServer, http.MethodPost, path, resolution, "", restartServer.orchestratorAPIKey)
	if status != http.StatusOK || !anyToBool(replayed["idempotent_replay"]) {
		t.Fatalf("restart quarantine replay status=%d response=%#v", status, replayed)
	}
	if events := testAgentTaskCount(t, afterRestart, "task_ledger_events"); events != eventsBeforeReplay {
		t.Fatalf("quarantine replay duplicated events: before=%d after=%d", eventsBeforeReplay, events)
	}
	changedProof := cloneAnyMap(resolution)
	changedProof["reason"] = "different termination proof"
	if status, _ := repairTaskRouteJSON(t, restartServer, http.MethodPost, path, changedProof, "", restartServer.orchestratorAPIKey); status == http.StatusOK {
		t.Fatal("changed quarantine proof replay was accepted")
	}
	newClaim, err := afterRestart.claimNext(context.Background(), "p1-replacement-worker", "p1-replacement-instance", "")
	if err != nil || newClaim == nil {
		t.Fatalf("claim requeued quarantine revision: %#v %v", newClaim, err)
	}
	newFence := testAgentTaskFenceFromClaim(t, newClaim)
	if newFence.Generation != fence.Generation+1 || newFence.AttemptID == fence.AttemptID {
		t.Fatalf("quarantine revision fence=%#v source=%#v", newFence, fence)
	}
}

func TestAgentTaskP1RequestChangesRequeuesOneNewFencedRevision(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	project, owner := "p1-request-changes-project", "p1-request-changes-owner"
	fixture := repairTaskReviewReady(t, ledger, "p1-request-changes", project, owner)
	server, _, _ := testAgentTaskServerWithMemory(t, ledger, project, owner, "sess_p1-request-changes")
	sign := repairTaskConfigureRuntimeSigner(t, server, "p1-request-changes-signer")
	ownerToken := sign(owner, "workspace-"+project, "owner", "p1-request-changes-token")
	path := "/agents/tasks/" + fixture.taskID + "/review"
	request := map[string]any{"result_id": fixture.resultID, "decision": "request_changes", "reason": "revise the bounded proof"}
	if status, _ := repairTaskRouteJSON(t, server, http.MethodPost, path, request, ownerToken, ""); status == http.StatusOK {
		t.Fatal("request_changes without exact source fence was accepted")
	}
	stale := cloneAnyMap(request)
	stale["source_attempt_id"] = fixture.fence.AttemptID
	stale["source_generation"] = fixture.fence.Generation + 1
	if status, _ := repairTaskRouteJSON(t, server, http.MethodPost, path, stale, ownerToken, ""); status == http.StatusOK {
		t.Fatal("request_changes with stale generation was accepted")
	}
	request["source_attempt_id"] = fixture.fence.AttemptID
	request["source_generation"] = fixture.fence.Generation
	status, response := repairTaskRouteJSON(t, server, http.MethodPost, path, request, ownerToken, "")
	review := anyMap(response["review"])
	if status != http.StatusOK || anyToString(review["status"]) != "changes_requested" || anyToString(review["source_attempt_id"]) != fixture.fence.AttemptID || anyToInt(review["source_generation"], 0) != fixture.fence.Generation {
		t.Fatalf("fenced request_changes status=%d response=%#v", status, response)
	}
	task, err := ledger.queryTask(context.Background(), fixture.taskID)
	if err != nil || anyToString(task["status"]) != "queued" || anyToString(task["active_attempt_id"]) != "" || anyToString(task["result_id"]) != "" {
		t.Fatalf("request_changes did not clear current binding and requeue: %#v %v", task, err)
	}
	revisionSource := anyMap(task["revision_envelope"])
	if anyToString(revisionSource["review_id"]) != anyToString(review["review_id"]) || anyToString(revisionSource["source_result_id"]) != fixture.resultID || anyToString(revisionSource["source_attempt_id"]) != fixture.fence.AttemptID || anyToInt(revisionSource["source_generation"], 0) != fixture.fence.Generation || anyToString(revisionSource["reason"]) != "revise the bounded proof" {
		t.Fatalf("request_changes did not persist the bounded revision source: %#v", revisionSource)
	}
	eventsBeforeReplay := testAgentTaskCount(t, ledger, "task_ledger_events")
	if err := ledger.close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.close() })
	replayed, err := restarted.reviewWithFence(context.Background(), fixture.taskID, fixture.resultID, owner, "request_changes", "revise the bounded proof", "", fixture.fence.AttemptID, fixture.fence.Generation)
	if err != nil || anyToString(replayed["review_id"]) != anyToString(review["review_id"]) {
		t.Fatalf("restart request_changes replay: %#v %v", replayed, err)
	}
	if testAgentTaskCount(t, restarted, "task_ledger_reviews") != 1 || testAgentTaskCount(t, restarted, "task_ledger_events") != eventsBeforeReplay {
		t.Fatal("request_changes replay duplicated durable evidence")
	}
	if _, err := restarted.reviewWithFence(context.Background(), fixture.taskID, fixture.resultID, owner, "request_changes", "different revision", "", fixture.fence.AttemptID, fixture.fence.Generation); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("changed request_changes replay accepted: %v", err)
	}
	newClaim, err := restarted.claimNext(context.Background(), "p1-revision-worker", "p1-revision-instance", "")
	if err != nil || newClaim == nil {
		t.Fatalf("claim request-changes revision: %#v %v", newClaim, err)
	}
	newFence := testAgentTaskFenceFromClaim(t, newClaim)
	if newFence.Generation != fixture.fence.Generation+1 || newFence.AttemptID == fixture.fence.AttemptID {
		t.Fatalf("request-changes revision did not receive a new fence: source=%#v new=%#v", fixture.fence, newFence)
	}
	revisionEnvelope := anyMap(newClaim["revision_envelope"])
	if err := verifyAgentTaskRevisionEnvelope(revisionEnvelope, newFence); err != nil {
		t.Fatalf("next claim revision envelope is not authorized for its exact fence: %#v %v", revisionEnvelope, err)
	}
	if anyToString(revisionEnvelope["review_id"]) != anyToString(review["review_id"]) || anyToString(revisionEnvelope["source_result_id"]) != fixture.resultID || anyToString(revisionEnvelope["source_attempt_id"]) != fixture.fence.AttemptID || anyToInt(revisionEnvelope["source_generation"], 0) != fixture.fence.Generation || anyToString(revisionEnvelope["reason"]) != "revise the bounded proof" {
		t.Fatalf("next claim lost immutable revision evidence: %#v", revisionEnvelope)
	}
	contextEnvelope := anyMap(anyMap(newClaim["context"])["revision_envelope"])
	if anyToString(contextEnvelope["authorization_digest"]) != anyToString(revisionEnvelope["authorization_digest"]) {
		t.Fatalf("worker context did not receive the exact authorized revision envelope: %#v", newClaim["context"])
	}
	staleEnvelope := cloneAnyMap(revisionEnvelope)
	staleEnvelope["worker_instance_id"] = "stale-revision-instance"
	if err := verifyAgentTaskRevisionEnvelope(staleEnvelope, newFence); err == nil || !strings.Contains(err.Error(), "stale_revision_fence") {
		t.Fatalf("stale worker revision envelope was accepted: %v", err)
	}
	storedAttempt, err := restarted.attempt(context.Background(), newFence.AttemptID)
	if err != nil || anyToString(anyMap(storedAttempt["revision_envelope"])["authorization_digest"]) != anyToString(revisionEnvelope["authorization_digest"]) {
		t.Fatalf("claimed attempt did not persist the exact revision envelope: %#v %v", storedAttempt, err)
	}
	claimedTask, err := restarted.queryTask(context.Background(), fixture.taskID)
	if err != nil || len(anyMap(claimedTask["revision_envelope"])) != 0 {
		t.Fatalf("claimed task retained a replayable pending revision source: %#v %v", claimedTask, err)
	}
	if _, err := restarted.reviewWithFence(context.Background(), fixture.taskID, fixture.resultID, owner, "request_changes", "revise the bounded proof", "", newFence.AttemptID, newFence.Generation); err == nil || !strings.Contains(err.Error(), "stale_review_fence") {
		t.Fatalf("new attempt was accepted as the old result review fence: %v", err)
	}
	if err := restarted.close(); err != nil {
		t.Fatal(err)
	}
	afterClaimRestart, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = afterClaimRestart.close() })
	restartedAttempt, err := afterClaimRestart.attempt(context.Background(), newFence.AttemptID)
	if err != nil || anyToString(anyMap(restartedAttempt["revision_envelope"])["authorization_digest"]) != anyToString(revisionEnvelope["authorization_digest"]) {
		t.Fatalf("revision envelope did not survive claimed-attempt restart: %#v %v", restartedAttempt, err)
	}
	failureExit := 1
	failed, err := afterClaimRestart.observe(context.Background(), newFence, "failed", &failureExit, map[string]any{"source": "revision-retry-proof"})
	if err != nil || anyToString(failed["failure_disposition"]) != "retry_queued" {
		t.Fatalf("failed revision attempt was not retry-queued: %#v %v", failed, err)
	}
	failedTask, err := afterClaimRestart.queryTask(context.Background(), fixture.taskID)
	failedRevisionSource := anyMap(failedTask["revision_envelope"])
	if err != nil || anyToString(failedTask["status"]) != "queued" || anyToString(failedRevisionSource["review_id"]) != anyToString(review["review_id"]) || anyToString(failedRevisionSource["source_result_id"]) != fixture.resultID || anyToString(failedRevisionSource["reason"]) != "revise the bounded proof" {
		t.Fatalf("failed revision attempt lost its retry instructions: %#v %v", failedTask, err)
	}
	if err := afterClaimRestart.close(); err != nil {
		t.Fatal(err)
	}

	afterFailureRestart, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = afterFailureRestart.close() })
	retryClaim, err := afterFailureRestart.claimNext(context.Background(), "p1-revision-retry-worker", "p1-revision-retry-instance", "")
	if err != nil || retryClaim == nil {
		t.Fatalf("claim failed revision retry after restart: %#v %v", retryClaim, err)
	}
	retryFence := testAgentTaskFenceFromClaim(t, retryClaim)
	retryEnvelope := anyMap(retryClaim["revision_envelope"])
	if retryFence.Generation != newFence.Generation+1 || verifyAgentTaskRevisionEnvelope(retryEnvelope, retryFence) != nil || anyToString(retryEnvelope["review_id"]) != anyToString(review["review_id"]) || anyToString(retryEnvelope["source_result_id"]) != fixture.resultID || anyToString(retryEnvelope["reason"]) != "revise the bounded proof" || anyToString(retryEnvelope["authorization_digest"]) == anyToString(revisionEnvelope["authorization_digest"]) {
		t.Fatalf("failed revision retry did not receive fresh exact-fence instructions: fence=%#v envelope=%#v", retryFence, retryEnvelope)
	}
	if _, err := afterFailureRestart.db.ExecContext(context.Background(), `UPDATE task_ledger_attempts SET lease_expires_at=? WHERE attempt_id=?`, time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), retryFence.AttemptID); err != nil {
		t.Fatal(err)
	}
	if recovered, err := afterFailureRestart.recoverExpired(context.Background(), 10); err != nil || len(recovered) != 1 {
		t.Fatalf("quarantine revision retry: %#v %v", recovered, err)
	}
	if resolved, err := afterFailureRestart.resolveQuarantinedAttempt(context.Background(), retryFence, true, "revision worker process group terminated"); err != nil || !anyToBool(resolved["requeued"]) {
		t.Fatalf("resolve quarantined revision retry: %#v %v", resolved, err)
	}
	quarantineTask, err := afterFailureRestart.queryTask(context.Background(), fixture.taskID)
	quarantineRevisionSource := anyMap(quarantineTask["revision_envelope"])
	if err != nil || anyToString(quarantineTask["status"]) != "queued" || anyToString(quarantineRevisionSource["review_id"]) != anyToString(review["review_id"]) || anyToString(quarantineRevisionSource["source_attempt_id"]) != fixture.fence.AttemptID || anyToString(quarantineRevisionSource["reason"]) != "revise the bounded proof" {
		t.Fatalf("quarantine requeue lost revision instructions: %#v %v", quarantineTask, err)
	}
	if err := afterFailureRestart.close(); err != nil {
		t.Fatal(err)
	}

	afterQuarantineRestart, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = afterQuarantineRestart.close() })
	postQuarantineClaim, err := afterQuarantineRestart.claimNext(context.Background(), "p1-revision-post-quarantine-worker", "p1-revision-post-quarantine-instance", "")
	if err != nil || postQuarantineClaim == nil {
		t.Fatalf("claim quarantined revision after restart: %#v %v", postQuarantineClaim, err)
	}
	postQuarantineFence := testAgentTaskFenceFromClaim(t, postQuarantineClaim)
	postQuarantineEnvelope := anyMap(postQuarantineClaim["revision_envelope"])
	if postQuarantineFence.Generation != retryFence.Generation+1 || verifyAgentTaskRevisionEnvelope(postQuarantineEnvelope, postQuarantineFence) != nil || anyToString(postQuarantineEnvelope["review_id"]) != anyToString(review["review_id"]) || anyToString(postQuarantineEnvelope["source_result_id"]) != fixture.resultID || anyToString(postQuarantineEnvelope["reason"]) != "revise the bounded proof" || anyToString(postQuarantineEnvelope["authorization_digest"]) == anyToString(retryEnvelope["authorization_digest"]) {
		t.Fatalf("post-quarantine revision did not receive fresh exact-fence instructions: fence=%#v envelope=%#v", postQuarantineFence, postQuarantineEnvelope)
	}
	if testAgentTaskCount(t, afterQuarantineRestart, "task_ledger_reviews") != 1 {
		t.Fatal("revision retries duplicated the immutable request-changes review")
	}
}

func TestAgentTaskP1TerminalReviewIsImmutableAndAcknowledgeIsPreDecisionOnly(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	fixture := repairTaskReviewReady(t, ledger, "p1-terminal-review", "p1-terminal-project", "p1-terminal-owner")
	owner := "p1-terminal-owner"
	acknowledged, err := ledger.review(context.Background(), fixture.taskID, fixture.resultID, owner, "acknowledge", "received", "")
	if err != nil || anyToString(acknowledged["status"]) != "acknowledged" {
		t.Fatalf("pre-decision acknowledgement: %#v %v", acknowledged, err)
	}
	accepted, err := ledger.review(context.Background(), fixture.taskID, fixture.resultID, owner, "accept", "verified", "")
	if err != nil || anyToString(accepted["status"]) != "accepted_for_integration" {
		t.Fatalf("terminal accept decision: %#v %v", accepted, err)
	}
	eventsBeforeReplay := testAgentTaskCount(t, ledger, "task_ledger_events")
	replayed, err := ledger.review(context.Background(), fixture.taskID, fixture.resultID, owner, "accept", "verified", "")
	if err != nil || anyToString(replayed["review_id"]) != anyToString(accepted["review_id"]) {
		t.Fatalf("exact terminal review replay: %#v %v", replayed, err)
	}
	if _, err := ledger.review(context.Background(), fixture.taskID, fixture.resultID, owner, "acknowledge", "received", ""); err == nil || !strings.Contains(err.Error(), "terminal review decision is immutable") {
		t.Fatalf("post-decision acknowledgement accepted: %v", err)
	}
	if _, err := ledger.review(context.Background(), fixture.taskID, fixture.resultID, owner, "reject", "changed", ""); err == nil || !strings.Contains(err.Error(), "terminal review decision is immutable") {
		t.Fatalf("terminal review mutation accepted: %v", err)
	}
	if events := testAgentTaskCount(t, ledger, "task_ledger_events"); events != eventsBeforeReplay {
		t.Fatalf("terminal review replays changed event count: before=%d after=%d", eventsBeforeReplay, events)
	}
	stored, err := ledger.reviewPayload(context.Background(), anyToString(accepted["review_id"]))
	if err != nil || anyToString(stored["decision"]) != "accept" || anyToString(stored["reason"]) != "verified" || anyToString(stored["status"]) != "accepted_for_integration" {
		t.Fatalf("terminal review row mutated: %#v %v", stored, err)
	}
}

func TestAgentTaskP1IntegrationReplayBindsExactIdentityAndRejectsCrossTaskCollision(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	project, owner := "p1-integration-project", "p1-integration-owner"
	first := repairTaskReviewReady(t, ledger, "p1-integration-first", project, owner)
	second := repairTaskReviewReady(t, ledger, "p1-integration-second", project, owner)
	if _, err := ledger.review(context.Background(), first.taskID, first.resultID, owner, "accept", "first verified", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.review(context.Background(), second.taskID, second.resultID, owner, "accept", "second verified", ""); err != nil {
		t.Fatal(err)
	}
	integrationID := "p1-shared-integration-id"
	firstRequest := map[string]any{"integration_id": integrationID, "task_id": first.taskID, "result_id": first.resultID, "actor": owner, "action": "merge", "digest": first.digest, "target": "main"}
	firstIntegration, err := ledger.integrate(context.Background(), firstRequest)
	if err != nil || anyToString(firstIntegration["integration_id"]) != integrationID {
		t.Fatalf("record first integration: %#v %v", firstIntegration, err)
	}
	if err := ledger.close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.close() })
	replayed, err := restarted.integrate(context.Background(), firstRequest)
	if err != nil || anyToString(replayed["task_id"]) != first.taskID || anyToString(replayed["result_id"]) != first.resultID || anyToString(replayed["actor"]) != owner {
		t.Fatalf("restart integration replay changed identity: %#v %v", replayed, err)
	}
	for label, mutate := range map[string]func(map[string]any){
		"action": func(request map[string]any) { request["action"] = "leave_unintegrated" },
		"actor":  func(request map[string]any) { request["actor"] = strings.ToUpper(owner) },
		"digest": func(request map[string]any) { request["digest"] = "sha256:" + strings.Repeat("0", 64) },
		"target": func(request map[string]any) { request["target"] = "different-target" },
	} {
		mutation := cloneAnyMap(firstRequest)
		mutate(mutation)
		if _, err := restarted.integrate(context.Background(), mutation); err == nil {
			t.Fatalf("integration replay with changed %s was accepted", label)
		}
	}
	differentID := cloneAnyMap(firstRequest)
	differentID["integration_id"] = integrationID + "-different"
	if _, err := restarted.integrate(context.Background(), differentID); err == nil || !strings.Contains(err.Error(), "different integration_id") {
		t.Fatalf("existing integration tuple accepted a different integration_id: %v", err)
	}
	var secondEventsBefore int
	if err := restarted.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM task_ledger_events WHERE task_id=?`, second.taskID).Scan(&secondEventsBefore); err != nil {
		t.Fatal(err)
	}
	collision := map[string]any{"integration_id": integrationID, "task_id": second.taskID, "result_id": second.resultID, "actor": owner, "action": "merge", "digest": second.digest, "target": "main"}
	if _, err := restarted.integrate(context.Background(), collision); err == nil || !strings.Contains(err.Error(), "integration replay identity") {
		t.Fatalf("cross-task integration ID collision accepted: %v", err)
	}
	if count := testAgentTaskCount(t, restarted, "task_ledger_integrations"); count != 1 {
		t.Fatalf("cross-task collision changed integration rows: %d", count)
	}
	secondTask, err := restarted.queryTask(context.Background(), second.taskID)
	if err != nil || anyToString(secondTask["status"]) != "accepted_for_integration" {
		t.Fatalf("cross-task collision mutated second task: %#v %v", secondTask, err)
	}
	var secondEventsAfter int
	if err := restarted.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM task_ledger_events WHERE task_id=?`, second.taskID).Scan(&secondEventsAfter); err != nil {
		t.Fatal(err)
	}
	if secondEventsAfter != secondEventsBefore {
		t.Fatalf("cross-task collision changed second task events: before=%d after=%d", secondEventsBefore, secondEventsAfter)
	}
}

func TestAgentTaskP1IntegrationReplayPersistsExactPolicyAndApprovalExpiryEvidence(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	project, owner, taskID := "p1-policy-project", "p1-policy-owner", "p1-policy-integration"
	manifest := testAgentTaskManifest(taskID, project, owner, "sess_p1_policy_integration")
	manifest["approval_policy"] = map[string]any{"required": true, "policy_version": "p1-policy.v1", "scope": "leave_unintegrated"}
	manifest["approved"] = true
	if _, err := ledger.submit(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	claim, err := ledger.claimNext(context.Background(), "p1-policy-worker", "p1-policy-instance", "")
	if err != nil || claim == nil {
		t.Fatalf("claim policy task: %#v %v", claim, err)
	}
	fence := testAgentTaskFenceFromClaim(t, claim)
	if _, err := ledger.heartbeat(context.Background(), fence); err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	if _, err := ledger.observe(context.Background(), fence, "succeeded", &exitCode, map[string]any{"source": "policy-evidence-test"}); err != nil {
		t.Fatal(err)
	}
	publication, err := ledger.stagePublication(context.Background(), fence, hardeningPublicationRequest(fence, claim, taskID, nil))
	if err != nil {
		t.Fatal(err)
	}
	committed, err := ledger.finalizePublication(context.Background(), anyToString(publication["publication_id"]), "committed", "p1-policy-receipt", "")
	if err != nil {
		t.Fatal(err)
	}
	resultID := anyToString(committed["result_id"])
	digest := anyToString(anyMap(committed["result"])["result_digest"])
	deliveryID := anyToString(anyMap(anySlice(committed["deliveries"])[0])["delivery_id"])
	if _, err := ledger.claimReview(context.Background(), taskID, resultID, deliveryID, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.review(context.Background(), taskID, resultID, owner, "accept", "policy evidence verified", ""); err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339Nano)
	approval, err := ledger.createApproval(context.Background(), map[string]any{
		"approval_id": "p1-policy-approval", "task_id": taskID, "attempt_id": fence.AttemptID,
		"result_or_commit_digest": digest, "target": "archive", "policy_version": "p1-policy.v1",
		"approver": owner, "expires_at": expiresAt, "nonce": "p1-policy-approval-nonce",
	})
	if err != nil {
		t.Fatal(err)
	}
	integrationRequest := map[string]any{"integration_id": "p1-policy-integration-id", "task_id": taskID, "result_id": resultID, "actor": owner, "action": "leave_unintegrated", "digest": digest, "target": "archive"}
	integration, err := ledger.integrate(context.Background(), integrationRequest)
	if err != nil {
		t.Fatal(err)
	}
	if anyToString(integration["target"]) != "archive" || anyToString(integration["approval_id"]) != anyToString(approval["approval_id"]) || anyToString(integration["approval_expires_at"]) != expiresAt || anyToString(integration["approval_policy_version"]) != "p1-policy.v1" || !agentTaskCanonicalSHA256(anyToString(integration["policy_digest"])) || !agentTaskCanonicalSHA256(anyToString(integration["policy_evidence_digest"])) {
		t.Fatalf("integration omitted exact policy/expiry evidence: %#v", integration)
	}
	eventsBefore := testAgentTaskCount(t, ledger, "task_ledger_events")
	if err := ledger.close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.close() })
	replayed, err := restarted.integrate(context.Background(), integrationRequest)
	if err != nil || anyToString(replayed["policy_evidence_digest"]) != anyToString(integration["policy_evidence_digest"]) {
		t.Fatalf("restart policy-evidence replay: %#v %v", replayed, err)
	}
	changedTarget := cloneAnyMap(integrationRequest)
	changedTarget["target"] = "different-archive"
	if _, err := restarted.integrate(context.Background(), changedTarget); err == nil || !strings.Contains(err.Error(), "target") {
		t.Fatalf("same integration id accepted a different target: %v", err)
	}
	if testAgentTaskCount(t, restarted, "task_ledger_integrations") != 1 || testAgentTaskCount(t, restarted, "task_ledger_events") != eventsBefore {
		t.Fatal("different-target integration replay mutated authoritative state")
	}
	if _, err := restarted.db.ExecContext(context.Background(), `UPDATE task_ledger_approvals SET expires_at=? WHERE approval_id=?`, time.Now().UTC().Add(11*time.Minute).Format(time.RFC3339Nano), anyToString(approval["approval_id"])); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.integrate(context.Background(), integrationRequest); err == nil || !strings.Contains(err.Error(), "expiry evidence") {
		t.Fatalf("tampered approval expiry evidence replay was accepted: %v", err)
	}
}

func TestAgentTaskP1ProviderIntegrationRequiresExactNonemptyReceiptTarget(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	owner := "p1-provider-target-owner"
	fixture := repairTaskReviewReady(t, ledger, "p1-provider-target", "p1-provider-target-project", owner)
	if _, err := ledger.review(context.Background(), fixture.taskID, fixture.resultID, owner, "accept", "provider target verified", ""); err != nil {
		t.Fatal(err)
	}
	receipt := map[string]any{
		"receipt_id": "p1-provider-target-receipt", "authority": "provider-neutral", "status": "succeeded",
		"result_digest": fixture.digest, "target": "refs/heads/main", "provider_ref": "provider-pr-42",
	}
	baseRequest := map[string]any{
		"integration_id": "p1-provider-target-integration", "task_id": fixture.taskID, "result_id": fixture.resultID,
		"actor": owner, "action": "open_pr", "digest": fixture.digest, "execution_receipt": receipt,
	}
	if _, err := ledger.integrate(context.Background(), baseRequest); err == nil || !strings.Contains(err.Error(), "nonempty exact target") {
		t.Fatalf("provider integration accepted an empty request target: %v", err)
	}
	mismatched := cloneAnyMap(baseRequest)
	mismatched["target"] = "refs/heads/main"
	mismatchedReceipt := cloneAnyMap(receipt)
	mismatchedReceipt["target"] = "refs/heads/release"
	mismatched["execution_receipt"] = mismatchedReceipt
	if _, err := ledger.integrate(context.Background(), mismatched); err == nil || !strings.Contains(err.Error(), "exact nonempty target") {
		t.Fatalf("provider integration accepted a mismatched receipt target: %v", err)
	}
	missingReceiptTarget := cloneAnyMap(baseRequest)
	missingReceiptTarget["target"] = "refs/heads/main"
	missingTargetReceipt := cloneAnyMap(receipt)
	delete(missingTargetReceipt, "target")
	missingReceiptTarget["execution_receipt"] = missingTargetReceipt
	if _, err := ledger.integrate(context.Background(), missingReceiptTarget); err == nil || !strings.Contains(err.Error(), "exact nonempty target") {
		t.Fatalf("provider integration accepted a receipt without its target: %v", err)
	}
	if testAgentTaskCount(t, ledger, "task_ledger_integrations") != 0 {
		t.Fatal("invalid provider target requests mutated integration rows")
	}

	validRequest := cloneAnyMap(baseRequest)
	validRequest["target"] = "refs/heads/main"
	validRequest["execution_receipt"] = cloneAnyMap(receipt)
	integration, err := ledger.integrate(context.Background(), validRequest)
	if err != nil || anyToString(integration["target"]) != "refs/heads/main" || !agentTaskCanonicalSHA256(anyToString(integration["execution_receipt_digest"])) {
		t.Fatalf("provider integration did not persist the exact receipt target: %#v %v", integration, err)
	}
	eventsBefore := testAgentTaskCount(t, ledger, "task_ledger_events")
	if err := ledger.close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.close() })
	replayed, err := restarted.integrate(context.Background(), validRequest)
	if err != nil || anyToString(replayed["target"]) != "refs/heads/main" || anyToString(replayed["execution_receipt_digest"]) != anyToString(integration["execution_receipt_digest"]) {
		t.Fatalf("provider target replay changed persisted evidence: %#v %v", replayed, err)
	}
	changedTarget := cloneAnyMap(validRequest)
	changedTarget["target"] = "refs/heads/release"
	changedReceipt := cloneAnyMap(receipt)
	changedReceipt["target"] = "refs/heads/release"
	changedTarget["execution_receipt"] = changedReceipt
	if _, err := restarted.integrate(context.Background(), changedTarget); err == nil || !strings.Contains(err.Error(), "target") {
		t.Fatalf("provider integration replay accepted a different exact target: %v", err)
	}
	if testAgentTaskCount(t, restarted, "task_ledger_integrations") != 1 || testAgentTaskCount(t, restarted, "task_ledger_events") != eventsBefore {
		t.Fatal("different provider target replay mutated authoritative state")
	}
}

func TestPublicProductionTaskAuthorityCompletesOwnerLocalLifecycle(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	const apiKey = "public-owner-local-task-key"
	const project = "public-owner-local-project"
	const sessionID = "sess_public_owner_local"
	const sessionAgentID = "codex_gpt5"
	server, _, sessions := testAgentTaskServerWithMemory(
		t,
		ledger,
		project,
		sessionAgentID,
		sessionID,
	)
	server.orchestratorAPIKey = apiKey
	server.taskProjectWorkspace = publicLocalAgentTaskProjectWorkspace
	server.taskServiceWorkerAuthority = publicLocalAgentTaskServiceWorkerAuthority
	server.taskServiceOwnerLocalLifecycle = true
	sessions.idleTTL = time.Hour

	foreignManifest := testAgentTaskManifest(
		"public-owner-local-foreign-task",
		project,
		"foreign-task-owner",
		sessionID,
	)
	delete(foreignManifest, "workspace_id")
	if status, _ := repairTaskRouteJSON(t, server, http.MethodPost, "/agents/tasks", foreignManifest, "", apiKey); status != http.StatusForbidden {
		t.Fatalf("public service accepted caller-owned reviewer/recipient authority: status=%d", status)
	}
	for index, mutate := range []func(map[string]any){
		func(recipient map[string]any) { recipient["sessionId"] = "sess_foreign_alias" },
		func(recipient map[string]any) { recipient["principalId"] = "foreign-principal-alias" },
		func(recipient map[string]any) { recipient["project_name"] = "foreign-project-alias" },
		func(recipient map[string]any) { recipient["workspaceId"] = "foreign-workspace-alias" },
	} {
		aliasManifest := testAgentTaskManifest(
			fmt.Sprintf("public-owner-local-recipient-alias-%d", index),
			project,
			sessionAgentID,
			sessionID,
		)
		delete(aliasManifest, "workspace_id")
		mutate(anyMap(anySlice(aliasManifest["recipients"])[0]))
		if status, _ := repairTaskRouteJSON(t, server, http.MethodPost, "/agents/tasks", aliasManifest, "", apiKey); status == http.StatusOK {
			t.Fatalf("public service accepted conflicting recipient identity aliases: case=%d", index)
		}
	}
	for index, mutate := range []func(map[string]any){
		func(candidate map[string]any) { candidate["project_name"] = "foreign-project-alias" },
		func(candidate map[string]any) { candidate["workspaceId"] = "foreign-workspace-alias" },
		func(candidate map[string]any) { candidate["reviewOwner"] = "foreign-reviewer-alias" },
		func(candidate map[string]any) { candidate["requestingAgentId"] = "foreign-requester-alias" },
		func(candidate map[string]any) {
			anyMap(candidate["context_request"])["sessionId"] = "sess_foreign_alias"
		},
	} {
		conflict := testAgentTaskManifest(
			fmt.Sprintf("public-owner-local-root-alias-%d", index),
			project,
			sessionAgentID,
			sessionID,
		)
		delete(conflict, "workspace_id")
		mutate(conflict)
		if status, _ := repairTaskRouteJSON(t, server, http.MethodPost, "/agents/tasks", conflict, "", apiKey); status == http.StatusOK {
			t.Fatalf("public service accepted conflicting root identity aliases: case=%d", index)
		}
	}
	if _, _, err := sessions.startOrReuse(map[string]any{
		"session_id": "sess_public_owner_local_alt",
		"agent":      "test",
		"agent_id":   sessionAgentID,
		"project":    project,
		"objective":  "receive the same durable task result",
	}); err != nil {
		t.Fatalf("start alternate owner-local recipient session: %v", err)
	}
	duplicateManifest := testAgentTaskManifest(
		"public-owner-local-duplicate-recipient",
		project,
		sessionAgentID,
		sessionID,
	)
	delete(duplicateManifest, "workspace_id")
	duplicateManifest["recipients"] = append(anySlice(duplicateManifest["recipients"]), map[string]any{
		"principal_id": sessionAgentID,
		"role":         "reviewer",
		"project":      project,
		"observer":     false,
		"session_id":   "sess_public_owner_local_alt",
	})
	if status, _ := repairTaskRouteJSON(t, server, http.MethodPost, "/agents/tasks", duplicateManifest, "", apiKey); status == http.StatusOK {
		t.Fatal("public service accepted duplicate canonical recipient principals")
	}
	alternateOnlyManifest := testAgentTaskManifest(
		"public-owner-local-alternate-only-recipient",
		project,
		sessionAgentID,
		sessionID,
	)
	delete(alternateOnlyManifest, "workspace_id")
	alternateOnlyManifest["recipients"] = []any{map[string]any{
		"principal_id": sessionAgentID,
		"role":         "reviewer",
		"project":      project,
		"observer":     false,
		"session_id":   "sess_public_owner_local_alt",
	}}
	if status, _ := repairTaskRouteJSON(t, server, http.MethodPost, "/agents/tasks", alternateOnlyManifest, "", apiKey); status == http.StatusOK {
		t.Fatal("public service accepted reviewer principal under an alternate recipient session")
	}

	manifest := testAgentTaskManifest(
		"public-owner-local-task",
		project,
		sessionAgentID,
		sessionID,
	)
	manifest["approval_policy"] = map[string]any{"required": true, "policy_version": "owner-local.v1"}
	manifest["approved"] = false
	delete(manifest, "workspace_id")
	status, submitted := repairTaskRouteJSON(t, server, http.MethodPost, "/agents/tasks", manifest, "", apiKey)
	if status != http.StatusOK {
		t.Fatalf("public production task submission failed: status=%d response=%#v", status, submitted)
	}
	task := anyMap(submitted["task"])
	if anyToString(task["task_id"]) != "public-owner-local-task" ||
		anyToString(task["workspace_id"]) != publicLocalAgentTaskWorkspaceID ||
		anyToString(task["review_owner"]) != sessionAgentID ||
		anyToString(task["requesting_agent_id"]) != sessionAgentID {
		t.Fatalf("public task did not receive the closed owner-local authority: %#v", task)
	}
	for _, recipient := range agentTaskRecipientRows(task) {
		if anyToString(recipient["principal_id"]) != sessionAgentID {
			t.Fatalf("public task retained caller-owned recipient authority: %#v", recipient)
		}
	}

	if status, response := repairTaskRouteJSON(t, server, http.MethodPost, "/agents/tasks/public-owner-local-task/approve", map[string]any{"note": "owner-local approval"}, "", apiKey); status != http.StatusOK || !anyToBool(anyMap(response["task"])["approved"]) {
		t.Fatalf("public owner-local approval failed: status=%d response=%#v", status, response)
	}

	credential := strings.Repeat("a", workerInstanceCredentialBytes*2)
	registration := map[string]any{
		"requested_worker_id": "public-owner-local-worker",
		"worker_instance_id":  "public-owner-local-instance",
	}
	foreignRegistration := cloneAnyMap(registration)
	foreignRegistration["principal_id"] = "foreign-service-worker"
	foreignRegistration["workspace_id"] = "foreign-service-workspace"
	if foreignStatus, _ := repairTaskRouteJSON(t, server, http.MethodPost, "/agents/workers/register", foreignRegistration, "", apiKey, credential); foreignStatus != http.StatusForbidden {
		t.Fatalf("public service worker rebound owner-local authority: status=%d", foreignStatus)
	}
	status, registered := repairTaskRouteJSON(t, server, http.MethodPost, "/agents/workers/register", registration, "", apiKey, credential)
	if status != http.StatusOK {
		t.Fatalf("public production worker registration failed: status=%d response=%#v", status, registered)
	}
	identity := anyMap(registered["identity"])
	claimRequest := map[string]any{
		"requested_worker_id":               identity["requested_worker_id"],
		"canonical_worker_id":               identity["canonical_worker_id"],
		"worker_instance_id":                identity["worker_instance_id"],
		"worker_identity_update_generation": identity["worker_identity_update_generation"],
	}
	status, claimed := repairTaskRouteJSON(t, server, http.MethodPost, "/agents/tasks/next", claimRequest, "", apiKey, credential)
	if status != http.StatusOK {
		t.Fatalf("public production task claim failed: status=%d response=%#v", status, claimed)
	}
	claimedTask := anyMap(claimed["task"])
	if anyToString(claimedTask["task_id"]) != anyToString(task["task_id"]) || anyToString(claimedTask["workspace_id"]) != publicLocalAgentTaskWorkspaceID {
		t.Fatalf("public production task lifecycle did not return the submitted task: %#v", claimed)
	}
	fence := testAgentTaskFenceFromClaim(t, claimed)
	if status, response := repairTaskRouteJSON(t, server, http.MethodPost, "/agents/tasks/"+fence.TaskID+"/heartbeat", fencePayload(fence), "", apiKey); status != http.StatusOK {
		t.Fatalf("public owner-local heartbeat failed: status=%d response=%#v", status, response)
	}
	observation := fencePayload(fence)
	observation["runner_status"] = "succeeded"
	observation["exit_code"] = 0
	observation["metadata"] = map[string]any{"source": "public-owner-local-lifecycle"}
	if status, response := repairTaskRouteJSON(t, server, http.MethodPost, "/agents/tasks/"+fence.TaskID+"/observe", observation, "", apiKey); status != http.StatusOK {
		t.Fatalf("public owner-local observation failed: status=%d response=%#v", status, response)
	}
	publicationRequest := hardeningPublicationRequest(fence, claimed, fence.TaskID, []any{
		map[string]any{"name": "owner-local-proof.txt", "media_type": "text/plain", "content": "bounded owner-local proof"},
	})
	for field, value := range fencePayload(fence) {
		publicationRequest[field] = value
	}
	status, staged := repairTaskRouteJSON(t, server, http.MethodPost, "/agents/tasks/"+fence.TaskID+"/publish", publicationRequest, "", apiKey)
	if status != http.StatusOK || anyToString(staged["publication_id"]) == "" {
		t.Fatalf("public owner-local publication staging failed: status=%d response=%#v", status, staged)
	}
	status, committed := repairTaskRouteJSON(t, server, http.MethodPost, "/agents/tasks/"+fence.TaskID+"/finalize", map[string]any{"publication_id": staged["publication_id"]}, "", apiKey)
	if status != http.StatusOK || anyToString(committed["status"]) != "committed" {
		t.Fatalf("public owner-local publication finalization failed: status=%d response=%#v", status, committed)
	}
	deliveries := anySlice(committed["deliveries"])
	artifacts := anySlice(anyMap(committed["result"])["artifacts"])
	if len(deliveries) != 1 || len(artifacts) != 1 {
		t.Fatalf("public publication lost durable artifact/delivery rows: %#v", committed)
	}
	if !anyToBool(anyMap(anyMap(committed["result"])["format_contract"])["contract_valid"]) {
		t.Fatalf("public publication retained an invalid result contract: %#v", committed["result"])
	}
	deliveryID := anyToString(anyMap(deliveries[0])["delivery_id"])
	resultID := anyToString(committed["result_id"])
	artifactID := anyToString(anyMap(artifacts[0])["artifact_id"])
	if status, response := repairTaskRouteJSON(t, server, http.MethodPost, "/agents/tasks/"+fence.TaskID+"/deliveries/"+deliveryID+"/deliver", map[string]any{}, "", apiKey); status != http.StatusOK || anyToString(anyMap(response["delivery"])["status"]) != "delivered" {
		t.Fatalf("public owner-local delivery projection failed: status=%d response=%#v", status, response)
	}
	// The immutable task/delivery authority remains usable after the live
	// session becomes terminal and is eventually evicted. Submission and
	// projection already proved the exact session/project/agent binding.
	sessions.mu.Lock()
	sessions.sessions[sessionID]["status"] = "completed"
	sessions.mu.Unlock()
	if status, response := repairTaskRouteJSON(t, server, http.MethodPost, "/agents/tasks/"+fence.TaskID+"/deliveries/"+deliveryID+"/ack", map[string]any{}, "", apiKey); status != http.StatusOK || anyToString(anyMap(response["delivery"])["status"]) != "acknowledged" {
		t.Fatalf("public owner-local delivery acknowledgement failed: status=%d response=%#v", status, response)
	}
	sessions.mu.Lock()
	delete(sessions.sessions, sessionID)
	delete(sessions.events, sessionID)
	sessions.mu.Unlock()
	if status, response := repairTaskRouteJSON(t, server, http.MethodGet, "/agents/tasks/artifacts/"+artifactID, map[string]any{}, "", apiKey); status != http.StatusOK || anyToString(anyMap(response["artifact"])["artifact_id"]) != artifactID {
		t.Fatalf("public owner-local artifact read failed: status=%d response=%#v", status, response)
	}
	if status, response := repairTaskRouteJSON(t, server, http.MethodPost, "/agents/tasks/"+fence.TaskID+"/review-claim", map[string]any{"result_id": resultID, "delivery_id": deliveryID}, "", apiKey); status != http.StatusOK || anyToString(anyMap(response["reviewer_claim"])["actor"]) != sessionAgentID {
		t.Fatalf("public owner-local review claim failed: status=%d response=%#v", status, response)
	}
	if status, response := repairTaskRouteJSON(t, server, http.MethodPost, "/agents/tasks/"+fence.TaskID+"/review", map[string]any{"result_id": resultID, "decision": "accept", "reason": "owner-local evidence verified"}, "", apiKey); status != http.StatusOK || anyToString(anyMap(response["review"])["decision"]) != "accept" {
		t.Fatalf("public owner-local review failed: status=%d response=%#v", status, response)
	}
	cleanupReceipt := testAgentTaskCleanupReceipt(committed, fence)
	if status, response := repairTaskRouteJSON(t, server, http.MethodPost, "/agents/tasks/"+fence.TaskID+"/cleanup", testAgentTaskCleanupRequest(fence, cleanupReceipt), "", apiKey); status != http.StatusOK || !anyToBool(response["acknowledged"]) {
		t.Fatalf("public owner-local cleanup acknowledgement failed: status=%d response=%#v", status, response)
	}
}

func TestPublicProductionTaskAuthorityIsWiredIntoGatewayStartup(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"taskServiceWorkerAuthority:      publicLocalAgentTaskServiceWorkerAuthority",
		"taskServiceOwnerLocalLifecycle:  true",
	} {
		if !bytes.Contains(source, []byte(required)) {
			t.Fatalf("production Gateway is missing task authority wiring %q", required)
		}
	}
	workspace, err := (&server{}).resolveAgentTaskProjectWorkspace("public-project")
	if err != nil {
		t.Fatalf("public task project resolver is unavailable: %v", err)
	}
	if workspace != publicLocalAgentTaskWorkspaceID {
		t.Fatalf("public task project resolver returned %q, want %q", workspace, publicLocalAgentTaskWorkspaceID)
	}
	if bytes.Contains(source, []byte("taskProjectWorkspace:            publicLocalAgentTaskProjectWorkspace")) {
		t.Fatal("production Gateway must retain the optional paid project-workspace resolver seam")
	}
}
