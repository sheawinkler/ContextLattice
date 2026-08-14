package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func credentialRouteRequest(t *testing.T, method, path, principal, workspace, instance, credential string, payload map[string]any) (*http.Request, agentTaskRouteAuth) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Worker-Instance-ID", instance)
	if credential != "" {
		request.Header.Set(workerInstanceCredentialHeader, credential)
	}
	request = withAgentTaskRouteAuthorization(request, agentTaskRouteAuth{Principal: principal, Workspace: workspace, Role: "worker", Signed: true})
	authServer := &server{}
	auth, err := authServer.authenticateAgentTaskRoute(request, "worker")
	if err != nil {
		t.Fatalf("authenticate signed worker request: %v", err)
	}
	return request, auth
}

func callCredentialHTTPIdentityRoute(t *testing.T, server *server, request *http.Request) (int, map[string]any, http.Header) {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.agentTaskDeliveryRoute(recorder, request)
	var response map[string]any
	if recorder.Body.Len() != 0 {
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode HTTP worker identity response: status=%d body=%s err=%v", recorder.Code, recorder.Body.String(), err)
		}
	}
	return recorder.Code, response, recorder.Header()
}

func registerCredentialIdentity(t *testing.T, server *server, principal, workspace, instance, requested, credential string) (agentTaskRouteAuth, map[string]any) {
	t.Helper()
	payload := map[string]any{
		"requested_worker_id": requested,
		"worker_instance_id":  instance,
	}
	request, auth := credentialRouteRequest(t, http.MethodPost, "/agents/workers/register", principal, workspace, instance, credential, payload)
	status, response, headers := callCredentialHTTPIdentityRoute(t, server, request)
	if status != http.StatusOK {
		t.Fatalf("register credential-bound worker: status=%d response=%#v", status, response)
	}
	if headers.Get(workerInstanceCredentialHeader) != "" {
		t.Fatalf("registration disclosed a credential response header")
	}
	if strings.Contains(string(credentialTestJSON(response)), credential) {
		t.Fatal("worker credential appeared in the public registration response")
	}
	return auth, response
}

func credentialTestJSON(value map[string]any) []byte {
	raw, _ := json.Marshal(value)
	return raw
}

func insertLegacyCredentialIdentity(t *testing.T, ledger *agentTaskDeliveryLedger, identity agentWorkerIdentityRecord) {
	t.Helper()
	if identity.WorkerInstanceCredentialGeneration == 0 {
		identity.WorkerInstanceCredentialGeneration = workerInstanceCredentialGenerationInitial
	}
	if identity.Status == "" {
		identity.Status = "active"
	}
	if identity.CreatedAt == "" {
		identity.CreatedAt = agentTaskNow()
	}
	if identity.UpdatedAt == "" {
		identity.UpdatedAt = identity.CreatedAt
	}
	identity.RequestedIDDigest = workerIdentityRequestedDigest(identity.RequestedWorkerID)
	identity.IdentityDigest = workerIdentityRecordDigest(identity)
	if _, err := ledger.db.Exec(`INSERT INTO task_ledger_worker_identities(identity_id,principal_id,workspace_id,requested_worker_id,canonical_worker_id,worker_instance_id,worker_instance_credential_verifier,worker_instance_credential_generation,worker_identity_update_generation,acknowledged_generation,requested_id_digest,identity_digest,status,created_at,updated_at,closed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, identity.IdentityID, identity.PrincipalID, identity.WorkspaceID, identity.RequestedWorkerID, identity.CanonicalWorkerID, identity.WorkerInstanceID, "", identity.WorkerInstanceCredentialGeneration, identity.IdentityUpdateGeneration, identity.AcknowledgedGeneration, identity.RequestedIDDigest, identity.IdentityDigest, identity.Status, identity.CreatedAt, identity.UpdatedAt, identity.ClosedAt); err != nil {
		t.Fatalf("insert legacy verifier-less identity: %v", err)
	}
}

func TestWorkerInstanceCredentialHeaderIsNarrowToProofBoundRoutes(t *testing.T) {
	for _, path := range []string{
		"/agents/workers/register", "/agents/workers/identity", "/agents/workers/identity/ack",
		"/agents/tasks/next", "/agents/tasks/task-1/heartbeat", "/agents/tasks/task-1/observe",
		"/agents/tasks/task-1/publish", "/agents/tasks/task-1/cleanup", "/agents/tasks/task-1/attempts/attempt-1/cleanup",
	} {
		if !agentWorkerInstanceCredentialRoutePath(path) {
			t.Fatalf("proof-bound route did not select the credential header: %s", path)
		}
	}
	for _, path := range []string{
		"/agents/tasks", "/agents/tasks/task-1", "/agents/tasks/task-1/deliveries",
		"/agents/tasks/task-1/finalize", "/agents/tasks/artifacts/artifact-1/content", "/agents/health",
	} {
		if agentWorkerInstanceCredentialRoutePath(path) {
			t.Fatalf("non-proof route selected the credential header: %s", path)
		}
	}
}

func TestWorkerInstanceCredentialBindsSamePrincipalRoutesAndRestart(t *testing.T) {
	ledger, second := identityTestLedgers(t)
	gateway := &server{taskLedger: ledger}
	const principal = "credential-principal"
	const workspace = "credential-workspace"
	credentialA := strings.Repeat("a", workerInstanceCredentialBytes*2)
	credentialB := strings.Repeat("b", workerInstanceCredentialBytes*2)
	authA, _ := registerCredentialIdentity(t, gateway, principal, workspace, "credential-instance-a", "credential-worker", credentialA)
	authB, _ := registerCredentialIdentity(t, gateway, principal, workspace, "credential-instance-b", "credential-worker", credentialB)
	if credentialA == credentialB {
		t.Fatal("same-principal instances received the same credential")
	}
	identityB, err := ledger.workerIdentityByAuthority(context.Background(), identityTestAuthority(principal, workspace, "credential-instance-b"))
	if err != nil {
		t.Fatal(err)
	}
	var verifier string
	if err := ledger.db.QueryRow(`SELECT worker_instance_credential_verifier FROM task_ledger_worker_identities WHERE identity_id=?`, identityB.IdentityID).Scan(&verifier); err != nil {
		t.Fatal(err)
	}
	if verifier == "" || strings.Contains(verifier, credentialA) || strings.Contains(verifier, credentialB) {
		t.Fatalf("credential was stored in plaintext or not stored as a verifier: %q", verifier)
	}

	readPayload := map[string]any{"worker_instance_id": "credential-instance-b"}
	wrongRequest, _ := credentialRouteRequest(t, http.MethodGet, "/agents/workers/identity", principal, workspace, "credential-instance-b", credentialA, readPayload)
	if status, response, _ := callCredentialHTTPIdentityRoute(t, gateway, wrongRequest); status == http.StatusOK || anyToString(response["code"]) == "worker_identity_credential_migration_required" {
		t.Fatal("instance A credential selected same-principal instance B")
	}
	foreignRequest, _ := credentialRouteRequest(t, http.MethodGet, "/agents/workers/identity", "credential-foreign-principal", workspace, "credential-instance-b", credentialB, readPayload)
	if status, _, _ := callCredentialHTTPIdentityRoute(t, gateway, foreignRequest); status == http.StatusOK {
		t.Fatal("credential crossed a principal boundary")
	}
	missingRequest, _ := credentialRouteRequest(t, http.MethodGet, "/agents/workers/identity", principal, workspace, "credential-instance-b", "", readPayload)
	if status, _, _ := callCredentialHTTPIdentityRoute(t, gateway, missingRequest); status == http.StatusOK {
		t.Fatal("missing worker instance credential was accepted")
	}
	for _, malformed := range []string{"wrong", strings.Repeat("a", workerInstanceCredentialBytes*2+1)} {
		request, _ := credentialRouteRequest(t, http.MethodGet, "/agents/workers/identity", principal, workspace, "credential-instance-b", malformed, readPayload)
		if status, _, _ := callCredentialHTTPIdentityRoute(t, gateway, request); status == http.StatusOK {
			t.Fatalf("malformed worker credential was accepted: %q", malformed)
		}
	}
	exactRequest, _ := credentialRouteRequest(t, http.MethodGet, "/agents/workers/identity", principal, workspace, "credential-instance-b", credentialB, readPayload)
	if status, response, _ := callCredentialHTTPIdentityRoute(t, gateway, exactRequest); status != http.StatusOK || response["identity_update_required"] == nil {
		t.Fatalf("exact worker credential was rejected: status=%d response=%#v", status, response)
	}

	fence := agentTaskFence{WorkerID: identityB.CanonicalWorkerID, WorkerInstanceID: identityB.WorkerInstanceID, WorkerIdentityUpdateGeneration: identityB.IdentityUpdateGeneration}
	wrongFenceAuth := authA
	wrongFenceAuth.WorkerInstanceID = identityB.WorkerInstanceID
	wrongFenceAuth.WorkerInstanceCredential = credentialA
	if err := gateway.authorizeAgentTaskFence(context.Background(), &wrongFenceAuth, fence); err == nil {
		t.Fatal("wrong same-principal credential crossed the fence authority")
	}
	exactFenceAuth := authB
	exactFenceAuth.WorkerInstanceCredential = credentialB
	if err := gateway.authorizeAgentTaskFence(context.Background(), &exactFenceAuth, fence); err != nil {
		t.Fatalf("exact worker credential did not authorize the fence: %v", err)
	}

	if err := ledger.close(); err != nil {
		t.Fatal(err)
	}
	if err := second.close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.close()
	restartedServer := &server{taskLedger: restarted}
	restartedIdentity, err := restarted.workerIdentityByAuthority(context.Background(), identityTestAuthority(principal, workspace, "credential-instance-b"))
	if err != nil {
		t.Fatal(err)
	}
	restartedAuth := authB
	restartedAuth.WorkerInstanceCredential = credentialB
	if err := restartedServer.authorizeAgentTaskFence(context.Background(), &restartedAuth, agentTaskFence{WorkerID: restartedIdentity.CanonicalWorkerID, WorkerInstanceID: restartedIdentity.WorkerInstanceID, WorkerIdentityUpdateGeneration: restartedIdentity.IdentityUpdateGeneration}); err != nil {
		t.Fatalf("restart did not preserve credential verification: %v", err)
	}
}

func TestWorkerInstanceCredentialUpgradesLegacyVerifierAndRetriesAfterLostResponse(t *testing.T) {
	ledger, _ := identityTestLedgers(t)
	defer ledger.close()
	gateway := &server{taskLedger: ledger}
	const principal = "legacy-verifier-principal"
	const workspace = "legacy-verifier-workspace"
	const instance = "legacy-verifier-instance"
	const requested = "legacy-verifier-worker"
	credential := strings.Repeat("e", workerInstanceCredentialBytes*2)
	registerCredentialIdentity(t, gateway, principal, workspace, instance, requested, credential)
	authority := identityTestAuthority(principal, workspace, instance)
	identity, err := ledger.workerIdentityByAuthority(context.Background(), authority)
	if err != nil {
		t.Fatal(err)
	}
	legacyVerifier := legacyWorkerInstanceCredentialVerifier(credential, authority, identity.WorkerInstanceCredentialGeneration)
	if _, err := ledger.db.Exec(`UPDATE task_ledger_worker_identities SET worker_instance_credential_verifier=? WHERE identity_id=?`, legacyVerifier, identity.IdentityID); err != nil {
		t.Fatalf("seed pre-repair verifier: %v", err)
	}
	// The first response is deliberately discarded, modeling a committed
	// registration whose HTTP response was lost. The same client credential
	// must prove the legacy row and atomically upgrade its verifier.
	firstRequest, _ := credentialRouteRequest(t, http.MethodPost, "/agents/workers/register", principal, workspace, instance, credential, map[string]any{
		"requested_worker_id": requested, "worker_instance_id": instance,
	})
	if status, _, _ := callCredentialHTTPIdentityRoute(t, gateway, firstRequest); status != http.StatusOK {
		t.Fatalf("legacy verifier proof was not accepted for upgrade: status=%d", status)
	}
	var upgraded string
	if err := ledger.db.QueryRow(`SELECT worker_instance_credential_verifier FROM task_ledger_worker_identities WHERE identity_id=?`, identity.IdentityID).Scan(&upgraded); err != nil {
		t.Fatal(err)
	}
	if upgraded != workerInstanceCredentialVerifier(credential, authority, identity.WorkerInstanceCredentialGeneration, identity.IdentityUpdateGeneration) || upgraded == legacyVerifier {
		t.Fatalf("legacy verifier was not atomically upgraded to current version: upgraded=%q legacy=%q", upgraded, legacyVerifier)
	}
	retryRequest, _ := credentialRouteRequest(t, http.MethodPost, "/agents/workers/register", principal, workspace, instance, credential, map[string]any{
		"requested_worker_id": requested, "worker_instance_id": instance,
	})
	retryStatus, retryResponse, _ := callCredentialHTTPIdentityRoute(t, gateway, retryRequest)
	if retryStatus != http.StatusOK || !anyToBool(retryResponse["idempotent_replay"]) {
		t.Fatalf("same credential did not replay after lost response/upgrade: status=%d response=%#v", retryStatus, retryResponse)
	}
	wrongRequest, _ := credentialRouteRequest(t, http.MethodPost, "/agents/workers/register", principal, workspace, instance, strings.Repeat("f", workerInstanceCredentialBytes*2), map[string]any{
		"requested_worker_id": requested, "worker_instance_id": instance,
	})
	if status, _, _ := callCredentialHTTPIdentityRoute(t, gateway, wrongRequest); status == http.StatusOK {
		t.Fatal("wrong credential replayed an upgraded legacy verifier")
	}
}

func TestWorkerInstanceCredentialBindsRealClaimHeartbeatPublishCleanupAndIdentityRoutes(t *testing.T) {
	ledger, _ := identityTestLedgers(t)
	const principal = "credential-route-principal"
	const workspace = "workspace-credential-route-project"
	const project = "credential-route-project"
	const sessionID = "credential-route-session"
	const requested = "credential-route-worker"
	const instanceA = "credential-route-instance-a"
	const instanceB = "credential-route-instance-b"
	credentialA := strings.Repeat("a", workerInstanceCredentialBytes*2)
	credentialB := strings.Repeat("b", workerInstanceCredentialBytes*2)
	server, _, _ := testAgentTaskServerWithMemory(t, ledger, project, principal, sessionID)
	call := func(request *http.Request) (int, map[string]any) {
		status, response, _ := callCredentialHTTPIdentityRoute(t, server, request)
		return status, response
	}
	registerCredentialIdentity(t, server, principal, workspace, instanceA, requested, credentialA)
	_, responseB := registerCredentialIdentity(t, server, principal, workspace, instanceB, requested, credentialB)
	updateID := anyToString(anyMap(responseB["identity_update"])["update_id"])
	if updateID == "" {
		t.Fatal("same-principal collision did not issue B's canonical identity update")
	}
	update, err := ledger.workerIdentityUpdateByID(context.Background(), updateID)
	if err != nil {
		t.Fatal(err)
	}
	ackPayload := identityTestAckPayload(update, identityTestAuthority(principal, workspace, instanceB))
	wrongAck, _ := credentialRouteRequest(t, http.MethodPost, "/agents/workers/identity/ack", principal, workspace, instanceB, credentialA, ackPayload)
	if status, _ := call(wrongAck); status == http.StatusOK {
		t.Fatal("instance A credential acknowledged instance B identity update")
	}
	exactAck, _ := credentialRouteRequest(t, http.MethodPost, "/agents/workers/identity/ack", principal, workspace, instanceB, credentialB, ackPayload)
	if status, response := call(exactAck); status != http.StatusOK || !anyToBool(response["acknowledged"]) {
		t.Fatalf("instance B credential could not acknowledge its identity update: status=%d response=%#v", status, response)
	}
	identityB, err := ledger.workerIdentityByAuthority(context.Background(), identityTestAuthority(principal, workspace, instanceB))
	if err != nil {
		t.Fatal(err)
	}
	manifest := testAgentTaskManifest("credential-route-task", project, principal, sessionID)
	manifest["workspace_id"] = workspace
	if _, err := ledger.submit(context.Background(), manifest); err != nil {
		t.Fatalf("submit credential route task: %v", err)
	}
	claimPayload := map[string]any{
		"requested_worker_id": requested, "worker": identityB.CanonicalWorkerID,
		"worker_instance_id": instanceB, "principal_id": principal, "workspace_id": workspace,
	}
	wrongClaim, _ := credentialRouteRequest(t, http.MethodPost, "/agents/tasks/next?worker="+identityB.CanonicalWorkerID, principal, workspace, instanceB, credentialA, claimPayload)
	if status, _ := call(wrongClaim); status == http.StatusOK {
		t.Fatal("instance A credential claimed instance B task")
	}
	exactClaim, _ := credentialRouteRequest(t, http.MethodPost, "/agents/tasks/next?worker="+identityB.CanonicalWorkerID, principal, workspace, instanceB, credentialB, claimPayload)
	status, claimResponse := call(exactClaim)
	if status != http.StatusOK || claimResponse["task"] == nil {
		t.Fatalf("instance B credential could not claim its task: status=%d response=%#v", status, claimResponse)
	}
	fence := testAgentTaskFenceFromClaim(t, claimResponse)
	fence.WorkerIdentityUpdateGeneration = anyToInt(anyMap(claimResponse["lease"])["worker_identity_update_generation"], -1)
	heartbeatPayload := fencePayload(fence)
	wrongHeartbeat, _ := credentialRouteRequest(t, http.MethodPost, "/agents/tasks/"+fence.TaskID+"/heartbeat", principal, workspace, instanceB, credentialA, heartbeatPayload)
	if status, _ := call(wrongHeartbeat); status == http.StatusOK {
		t.Fatal("instance A credential heartbeated instance B lease")
	}
	exactHeartbeat, _ := credentialRouteRequest(t, http.MethodPost, "/agents/tasks/"+fence.TaskID+"/heartbeat", principal, workspace, instanceB, credentialB, heartbeatPayload)
	if status, response := call(exactHeartbeat); status != http.StatusOK {
		t.Fatalf("instance B credential could not heartbeat its lease: status=%d response=%#v", status, response)
	}
	observePayload := fencePayload(fence)
	observePayload["runner_status"] = "succeeded"
	observePayload["exit_code"] = 0
	observePayload["metadata"] = map[string]any{"source": "credential-route-test"}
	exactObserve, _ := credentialRouteRequest(t, http.MethodPost, "/agents/tasks/"+fence.TaskID+"/observe", principal, workspace, instanceB, credentialB, observePayload)
	if status, _ := call(exactObserve); status != http.StatusOK {
		t.Fatalf("instance B credential could not observe its lease: status=%d", status)
	}
	resultID := "result-credential-route-task"
	workspaceRef := "workspace-ref-credential-route-task"
	publicationPayload := fencePayload(fence)
	publicationPayload["publication_id"] = "publication-credential-route-task"
	publicationPayload["idempotency_key"] = "task-result:" + resultID
	publicationPayload["runner_exit_required"] = true
	publicationPayload["result"] = map[string]any{
		"result_id": resultID, "summary": "credential route result", "output": "verified output",
		"context_pack_hash": anyToString(anyMap(claimResponse["attempt"])["context_pack_hash"]),
		"workspace":         map[string]any{"workspace_ref": workspaceRef},
		"cleanup":           map[string]any{"cleanup_id": agentTaskCleanupID(fence.TaskID, fence.AttemptID, workspaceRef)},
	}
	publicationPayload["artifacts"] = []any{}
	wrongPublish, _ := credentialRouteRequest(t, http.MethodPost, "/agents/tasks/"+fence.TaskID+"/publish", principal, workspace, instanceB, credentialA, publicationPayload)
	if status, _ := call(wrongPublish); status == http.StatusOK {
		t.Fatal("instance A credential published for instance B lease")
	}
	exactPublish, _ := credentialRouteRequest(t, http.MethodPost, "/agents/tasks/"+fence.TaskID+"/publish", principal, workspace, instanceB, credentialB, publicationPayload)
	status, publication := call(exactPublish)
	if status != http.StatusOK {
		t.Fatalf("instance B credential could not publish its result: status=%d response=%#v", status, publication)
	}
	cleanupPayload := fencePayload(fence)
	cleanupPayload["cleanup_receipt"] = testAgentTaskCleanupReceipt(publication, fence)
	wrongCleanup, _ := credentialRouteRequest(t, http.MethodPost, "/agents/tasks/"+fence.TaskID+"/cleanup", principal, workspace, instanceB, credentialA, cleanupPayload)
	if status, _ := call(wrongCleanup); status == http.StatusOK {
		t.Fatal("instance A credential acknowledged cleanup for instance B lease")
	}
	exactCleanup, _ := credentialRouteRequest(t, http.MethodPost, "/agents/tasks/"+fence.TaskID+"/cleanup", principal, workspace, instanceB, credentialB, cleanupPayload)
	if status, _ := call(exactCleanup); status != http.StatusOK {
		t.Fatalf("instance B credential could not acknowledge cleanup: status=%d", status)
	}
	const retireInstance = "credential-route-retire-instance"
	const retireRequested = "credential-route-retire-worker"
	retireCredential := strings.Repeat("c", workerInstanceCredentialBytes*2)
	registerCredentialIdentity(t, server, principal, workspace, retireInstance, retireRequested, retireCredential)
	retireIdentity, err := ledger.workerIdentityByAuthority(context.Background(), identityTestAuthority(principal, workspace, retireInstance))
	if err != nil {
		t.Fatal(err)
	}
	retirePayload := workerIdentityRetirementPayloadForTest(retireIdentity)
	wrongRetire, _ := credentialRouteRequest(t, http.MethodPost, "/agents/workers/identity/retire", principal, workspace, retireInstance, credentialA, retirePayload)
	if status, _ := call(wrongRetire); status == http.StatusOK {
		t.Fatal("instance A credential retired instance B identity")
	}
	exactRetire, _ := credentialRouteRequest(t, http.MethodPost, "/agents/workers/identity/retire", principal, workspace, retireInstance, retireCredential, retirePayload)
	if status, response := call(exactRetire); status != http.StatusOK || !anyToBool(response["retired"]) {
		t.Fatalf("exact retire credential did not close its identity: status=%d response=%#v", status, response)
	}
}

func TestWorkerInstanceCredentialRegistrationIssuesAtMostOneSecret(t *testing.T) {
	ledger, _ := identityTestLedgers(t)
	server := &server{taskLedger: ledger}
	const principal = "credential-race-principal"
	const workspace = "credential-race-workspace"
	const instance = "credential-race-instance"
	const requested = "credential-race-worker"
	type result struct {
		status     int
		credential string
	}
	results := make(chan result, 2)
	var group sync.WaitGroup
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			payload := map[string]any{"requested_worker_id": requested, "worker_instance_id": instance}
			request, auth := credentialRouteRequest(t, http.MethodPost, "/agents/workers/register", principal, workspace, instance, strings.Repeat("c", workerInstanceCredentialBytes*2), payload)
			recorder := httptest.NewRecorder()
			server.handleAgentWorkerIdentityRoute(recorder, request, auth, payload)
			results <- result{status: recorder.Code, credential: recorder.Header().Get(workerInstanceCredentialHeader)}
		}()
	}
	group.Wait()
	close(results)
	issued := 0
	ok := 0
	for item := range results {
		if item.status == http.StatusOK {
			ok++
		}
		if item.credential != "" {
			issued++
		}
	}
	if issued != 0 || ok != 2 {
		t.Fatalf("concurrent same-instance registration did not replay one client credential: issued=%d ok=%d", issued, ok)
	}
	var count int
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_worker_identities WHERE principal_id=? AND workspace_id=? AND worker_instance_id=?`, principal, workspace, instance).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("concurrent registration merged or duplicated identity rows: %d", count)
	}
}

func TestHTTPServiceRegistrationRequiresClientCredential(t *testing.T) {
	ledger, _ := identityTestLedgers(t)
	const apiKey = "service-registration-key"
	server := &server{taskLedger: ledger, orchestratorAPIKey: apiKey}
	payload := map[string]any{
		"principal_id":        "service-registration-principal",
		"workspace_id":        "service-registration-workspace",
		"requested_worker_id": "service-registration-worker",
		"worker_instance_id":  "service-registration-instance",
	}
	call := func(credential string) (int, map[string]any) {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/agents/workers/register", bytes.NewReader(raw))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-API-Key", apiKey)
		request.Header.Set("X-Worker-Instance-ID", payload["worker_instance_id"].(string))
		if credential != "" {
			request.Header.Set(workerInstanceCredentialHeader, credential)
		}
		recorder := httptest.NewRecorder()
		server.agentTaskDeliveryRoute(recorder, request)
		var response map[string]any
		if recorder.Body.Len() != 0 {
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode service registration response: status=%d body=%s err=%v", recorder.Code, recorder.Body.String(), err)
			}
		}
		return recorder.Code, response
	}
	if status, _ := call(""); status == http.StatusOK {
		t.Fatal("HTTP service registration without a client credential was accepted")
	}
	credential := strings.Repeat("d", workerInstanceCredentialBytes*2)
	if status, response := call(credential); status != http.StatusOK || anyMap(response["identity"])["worker_instance_id"] != payload["worker_instance_id"] {
		t.Fatalf("HTTP service registration with exact client credential failed: status=%d response=%#v", status, response)
	}
}

func TestLegacyWorkerIdentityCredentialMigrationRequeuesLeaseAndRotates(t *testing.T) {
	ledger, _ := identityTestLedgers(t)
	defer ledger.close()
	ctx := context.Background()
	const principal = "legacy-credential-principal"
	const workspace = "legacy-credential-workspace"
	const instance = "legacy-credential-instance"
	const requested = "legacy-credential-worker"
	manifest := testAgentTaskManifest("legacy-credential-task", "legacy-credential-project", principal, "legacy-credential-session")
	manifest["workspace_id"] = workspace
	if _, err := ledger.submit(ctx, manifest); err != nil {
		t.Fatalf("submit legacy task: %v", err)
	}
	claim, err := ledger.claimTask(ctx, requested, instance, workspace, "legacy-credential-task")
	if err != nil || claim == nil {
		t.Fatalf("claim legacy active lease: claim=%#v err=%v", claim, err)
	}
	legacy := agentWorkerIdentityRecord{
		IdentityID: "legacy-credential-identity", PrincipalID: principal, WorkspaceID: workspace,
		RequestedWorkerID: requested, CanonicalWorkerID: requested, WorkerInstanceID: instance,
		WorkerInstanceCredentialGeneration: workerInstanceCredentialGenerationInitial,
		Status:                             "active", CreatedAt: agentTaskNow(), UpdatedAt: agentTaskNow(),
	}
	legacy.RequestedIDDigest = workerIdentityRequestedDigest(legacy.RequestedWorkerID)
	legacy.IdentityDigest = workerIdentityRecordDigest(legacy)
	if _, err := ledger.db.Exec(`INSERT INTO task_ledger_worker_identities(identity_id,principal_id,workspace_id,requested_worker_id,canonical_worker_id,worker_instance_id,worker_instance_credential_verifier,worker_instance_credential_generation,worker_identity_update_generation,acknowledged_generation,requested_id_digest,identity_digest,status,created_at,updated_at,closed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, legacy.IdentityID, legacy.PrincipalID, legacy.WorkspaceID, legacy.RequestedWorkerID, legacy.CanonicalWorkerID, legacy.WorkerInstanceID, "", legacy.WorkerInstanceCredentialGeneration, legacy.IdentityUpdateGeneration, legacy.AcknowledgedGeneration, legacy.RequestedIDDigest, legacy.IdentityDigest, legacy.Status, legacy.CreatedAt, legacy.UpdatedAt, ""); err != nil {
		t.Fatalf("insert legacy verifier-less identity: %v", err)
	}
	oldCredential := strings.Repeat("a", workerInstanceCredentialBytes*2)
	request, _ := credentialRouteRequest(t, http.MethodPost, "/agents/workers/register", principal, workspace, instance, oldCredential, map[string]any{
		"requested_worker_id": requested, "worker_instance_id": instance,
	})
	status, response, _ := callCredentialHTTPIdentityRoute(t, &server{taskLedger: ledger}, request)
	if status != http.StatusConflict || anyToString(response["code"]) != "worker_identity_credential_migration_required" {
		t.Fatalf("legacy migration did not return the closed machine-readable boundary: status=%d response=%#v", status, response)
	}
	var identityStatus, attemptStatus, taskStatus, activeAttempt string
	if err := ledger.db.QueryRow(`SELECT status FROM task_ledger_worker_identities WHERE identity_id=?`, legacy.IdentityID).Scan(&identityStatus); err != nil {
		t.Fatal(err)
	}
	if err := ledger.db.QueryRow(`SELECT status FROM task_ledger_attempts WHERE attempt_id=?`, anyToString(anyMap(claim["attempt"])["attempt_id"])).Scan(&attemptStatus); err != nil {
		t.Fatal(err)
	}
	if err := ledger.db.QueryRow(`SELECT status,active_attempt_id FROM task_ledger_tasks WHERE id=?`, "legacy-credential-task").Scan(&taskStatus, &activeAttempt); err != nil {
		t.Fatal(err)
	}
	if identityStatus != "closed" || attemptStatus != "execution_failed" || taskStatus != "queued" || activeAttempt != "" {
		t.Fatalf("legacy migration did not close/requeue atomically: identity=%q attempt=%q task=%q active_attempt=%q", identityStatus, attemptStatus, taskStatus, activeAttempt)
	}
	var receiptCount int
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_migration_receipts WHERE phase=? AND details_json NOT LIKE ?`, workerIdentityCredentialMigrationPhase, "%"+oldCredential+"%").Scan(&receiptCount); err != nil {
		t.Fatal(err)
	}
	if receiptCount != 1 {
		t.Fatalf("legacy migration receipt was missing or contained credential material: %d", receiptCount)
	}
	retryRequest, _ := credentialRouteRequest(t, http.MethodPost, "/agents/workers/register", principal, workspace, instance, oldCredential, map[string]any{
		"requested_worker_id": requested, "worker_instance_id": instance,
	})
	if retryStatus, retryResponse, _ := callCredentialHTTPIdentityRoute(t, &server{taskLedger: ledger}, retryRequest); retryStatus != http.StatusConflict || anyToString(retryResponse["code"]) != "worker_identity_credential_migration_required" {
		t.Fatalf("legacy migration retry did not remain idempotently closed: status=%d response=%#v", retryStatus, retryResponse)
	}

	newInstance := "legacy-credential-fresh-instance"
	newCredential := strings.Repeat("b", workerInstanceCredentialBytes*2)
	newRequest, _ := credentialRouteRequest(t, http.MethodPost, "/agents/workers/register", principal, workspace, newInstance, newCredential, map[string]any{
		"requested_worker_id": requested, "worker_instance_id": newInstance,
	})
	newStatus, newResponse, _ := callCredentialHTTPIdentityRoute(t, &server{taskLedger: ledger}, newRequest)
	if newStatus != http.StatusOK {
		t.Fatalf("fresh credential-bound instance did not register after migration: status=%d response=%#v", newStatus, newResponse)
	}
	fresh, err := ledger.workerIdentityByAuthority(ctx, identityTestAuthority(principal, workspace, newInstance))
	if err != nil {
		t.Fatal(err)
	}
	if fresh.WorkerInstanceCredentialVerifier == "" || strings.Contains(fresh.WorkerInstanceCredentialVerifier, newCredential) || fresh.WorkerInstanceID == instance {
		t.Fatalf("fresh identity did not receive an independent verifier-bound instance: %#v", fresh)
	}
	if fresh.IdentityUpdateGeneration > fresh.AcknowledgedGeneration {
		update, updateErr := ledger.readWorkerIdentityUpdate(ctx, identityTestAuthority(principal, workspace, newInstance), "")
		if updateErr != nil {
			t.Fatal(updateErr)
		}
		acknowledgeIdentityForTest(t, ledger, identityTestAuthority(principal, workspace, newInstance), update.UpdateID)
		fresh, err = ledger.workerIdentityByAuthority(ctx, identityTestAuthority(principal, workspace, newInstance))
		if err != nil {
			t.Fatal(err)
		}
	}
	claimedAgain, err := ledger.claimTaskWithIdentity(ctx, fresh.CanonicalWorkerID, newInstance, workspace, "legacy-credential-task", fresh.IdentityUpdateGeneration)
	if err != nil || claimedAgain == nil {
		t.Fatalf("fresh credential-bound identity could not claim the exactly requeued task: claim=%#v err=%v", claimedAgain, err)
	}
}

func TestLegacyWorkerIdentityCredentialMigrationBindsGenerationZeroRequestedLease(t *testing.T) {
	ledger, _ := identityTestLedgers(t)
	defer ledger.close()
	ctx := context.Background()
	const principal = "legacy-collision-principal"
	const workspace = "legacy-collision-workspace"
	const instance = "legacy-collision-instance"
	const requested = "legacy-collision-worker"
	const canonical = "legacy-collision-worker-instance"
	manifest := testAgentTaskManifest("legacy-collision-task", "legacy-collision-project", principal, "legacy-collision-session")
	manifest["workspace_id"] = workspace
	if _, err := ledger.submit(ctx, manifest); err != nil {
		t.Fatal(err)
	}
	claim, err := ledger.claimTask(ctx, requested, instance, workspace, "legacy-collision-task")
	if err != nil || claim == nil {
		t.Fatalf("claim generation-zero requested lease: claim=%#v err=%v", claim, err)
	}
	insertLegacyCredentialIdentity(t, ledger, agentWorkerIdentityRecord{
		IdentityID: "legacy-collision-identity", PrincipalID: principal, WorkspaceID: workspace,
		RequestedWorkerID: requested, CanonicalWorkerID: canonical, WorkerInstanceID: instance,
		IdentityUpdateGeneration: 1, AcknowledgedGeneration: 1,
	})
	if err := ledger.migrateLegacyWorkerIdentitiesAtStartup(ctx); err != nil {
		t.Fatalf("collision legacy migration failed: %v", err)
	}
	var identityStatus, attemptStatus, taskStatus string
	if err := ledger.db.QueryRow(`SELECT status FROM task_ledger_worker_identities WHERE identity_id=?`, "legacy-collision-identity").Scan(&identityStatus); err != nil {
		t.Fatal(err)
	}
	if err := ledger.db.QueryRow(`SELECT status FROM task_ledger_attempts WHERE attempt_id=?`, anyToString(anyMap(claim["attempt"])["attempt_id"])).Scan(&attemptStatus); err != nil {
		t.Fatal(err)
	}
	if err := ledger.db.QueryRow(`SELECT status FROM task_ledger_tasks WHERE id=?`, "legacy-collision-task").Scan(&taskStatus); err != nil {
		t.Fatal(err)
	}
	if identityStatus != "closed" || attemptStatus != "execution_failed" || taskStatus != "queued" {
		t.Fatalf("generation-zero requested lease was not migrated: identity=%q attempt=%q task=%q", identityStatus, attemptStatus, taskStatus)
	}
	var claimEligible int
	if err := ledger.db.QueryRow(`SELECT claim_eligible FROM task_ledger_tasks WHERE id=?`, "legacy-collision-task").Scan(&claimEligible); err != nil {
		t.Fatal(err)
	}
	if claimEligible != 0 {
		t.Fatal("renamed legacy identity silently enabled an ambiguous requested-worker queue claim")
	}
}

func TestLegacyWorkerIdentityMigrationDoesNotTouchSameRequestedInstance(t *testing.T) {
	ledger, _ := identityTestLedgers(t)
	defer ledger.close()
	ctx := context.Background()
	const principal = "legacy-same-requested-principal"
	const workspace = "legacy-same-requested-workspace"
	const requested = "legacy-same-requested-worker"
	authorityA := identityTestAuthority(principal, workspace, "legacy-same-requested-instance-a")
	authorityB := identityTestAuthority(principal, workspace, "legacy-same-requested-instance-b")
	_, identityA := registerIdentityForTest(t, ledger, authorityA, requested)
	responseB, identityB := registerIdentityForTest(t, ledger, authorityB, requested)
	if anyToBool(responseB["identity_update_required"]) {
		acknowledgeIdentityForTest(t, ledger, authorityB, anyToString(anyMap(responseB["identity_update"])["update_id"]))
		identityB, _ = ledger.workerIdentityByAuthority(ctx, authorityB)
	}
	manifest := testAgentTaskManifest("legacy-same-requested-task", "legacy-same-requested-project", principal, "legacy-same-requested-session")
	manifest["workspace_id"] = workspace
	manifest["metadata"] = map[string]any{"worker": identityA.CanonicalWorkerID, "worker_instance_id": authorityA.WorkerInstanceID}
	if _, err := ledger.submit(ctx, manifest); err != nil {
		t.Fatalf("submit A-bound queued task: %v", err)
	}
	var boundIdentity, boundInstance string
	if err := ledger.db.QueryRow(`SELECT identity_id,worker_instance_id FROM task_ledger_worker_task_bindings WHERE task_id=?`, "legacy-same-requested-task").Scan(&boundIdentity, &boundInstance); err != nil {
		t.Fatalf("read A queued binding: %v", err)
	}
	if boundIdentity != identityA.IdentityID || boundInstance != authorityA.WorkerInstanceID {
		t.Fatalf("queued task was not bound to A: identity=%q instance=%q", boundIdentity, boundInstance)
	}
	if _, err := ledger.db.Exec(`UPDATE task_ledger_worker_identities SET worker_instance_credential_verifier='' WHERE identity_id=?`, identityB.IdentityID); err != nil {
		t.Fatalf("make B legacy: %v", err)
	}
	if err := ledger.migrateLegacyWorkerIdentity(ctx, identityB); err != nil {
		t.Fatalf("migrate B legacy identity: %v", err)
	}
	var status string
	if err := ledger.db.QueryRow(`SELECT status FROM task_ledger_worker_identities WHERE identity_id=?`, identityB.IdentityID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "closed" {
		t.Fatalf("B legacy identity did not close: %q", status)
	}
	var claimEligible int
	if err := ledger.db.QueryRow(`SELECT claim_eligible FROM task_ledger_tasks WHERE id=?`, "legacy-same-requested-task").Scan(&claimEligible); err != nil {
		t.Fatal(err)
	}
	if claimEligible != 1 {
		t.Fatalf("B migration disabled A's exact queued claim: eligible=%d", claimEligible)
	}
	if err := ledger.db.QueryRow(`SELECT identity_id,worker_instance_id FROM task_ledger_worker_task_bindings WHERE task_id=?`, "legacy-same-requested-task").Scan(&boundIdentity, &boundInstance); err != nil {
		t.Fatal(err)
	}
	if boundIdentity != identityA.IdentityID || boundInstance != authorityA.WorkerInstanceID {
		t.Fatalf("B migration changed A's exact queued binding: identity=%q instance=%q", boundIdentity, boundInstance)
	}
}

func TestLegacyWorkerIdentityQueuedClaimRecoveryIsExplicitAndIdempotent(t *testing.T) {
	ledger, _ := identityTestLedgers(t)
	defer ledger.close()
	ctx := context.Background()
	const principal = "legacy-recovery-principal"
	const workspace = "legacy-recovery-workspace"
	const requested = "legacy-recovery-worker"
	const oldInstance = "legacy-recovery-old-instance"
	manifest := testAgentTaskManifest("legacy-recovery-task", "legacy-recovery-project", principal, "legacy-recovery-session")
	manifest["workspace_id"] = workspace
	if _, err := ledger.submit(ctx, manifest); err != nil {
		t.Fatal(err)
	}
	claim, err := ledger.claimTask(ctx, requested, oldInstance, workspace, "legacy-recovery-task")
	if err != nil || claim == nil {
		t.Fatalf("claim legacy recovery lease: claim=%#v err=%v", claim, err)
	}
	oldIdentity := agentWorkerIdentityRecord{
		IdentityID: "legacy-recovery-old-identity", PrincipalID: principal, WorkspaceID: workspace,
		RequestedWorkerID: requested, CanonicalWorkerID: requested + "-old", WorkerInstanceID: oldInstance,
		IdentityUpdateGeneration: 1, AcknowledgedGeneration: 1,
	}
	insertLegacyCredentialIdentity(t, ledger, oldIdentity)
	if err := ledger.migrateLegacyWorkerIdentitiesAtStartup(ctx); err != nil {
		t.Fatalf("migrate legacy recovery identity: %v", err)
	}
	var migrationReceiptID string
	if err := ledger.db.QueryRow(`SELECT receipt_id FROM task_ledger_migration_receipts WHERE phase=?`, workerIdentityCredentialMigrationPhase).Scan(&migrationReceiptID); err != nil {
		t.Fatal(err)
	}
	newAuthority := identityTestAuthority(principal, workspace, "legacy-recovery-new-instance")
	_, newIdentity := registerIdentityForTest(t, ledger, newAuthority, requested)
	if newIdentity.IdentityUpdateGeneration != newIdentity.AcknowledgedGeneration || newIdentity.WorkerInstanceID == oldInstance {
		t.Fatalf("replacement identity is not immediately credential-bound: %#v", newIdentity)
	}
	wrongAuthority := identityTestAuthority("legacy-recovery-wrong-principal", workspace, "legacy-recovery-wrong-instance")
	_, wrongIdentity := registerIdentityForTest(t, ledger, wrongAuthority, requested)
	recoveryRequest := map[string]any{
		"phase": "worker_identity_rebind", "identity_id": oldIdentity.IdentityID,
		"new_identity_id": newIdentity.IdentityID, "migration_receipt_id": migrationReceiptID,
	}
	wrongRequest := map[string]any{
		"phase": "worker_identity_rebind", "identity_id": oldIdentity.IdentityID,
		"new_identity_id": wrongIdentity.IdentityID, "migration_receipt_id": migrationReceiptID,
	}
	if _, err := ledger.rebindLegacyWorkerIdentityQueuedClaims(ctx, wrongRequest); err == nil {
		t.Fatal("wrong-principal recovery rebound a legacy claim")
	}
	staleRequest := map[string]any{
		"phase": "worker_identity_rebind", "identity_id": oldIdentity.IdentityID,
		"new_identity_id": newIdentity.IdentityID, "migration_receipt_id": "stale-receipt",
	}
	if _, err := ledger.rebindLegacyWorkerIdentityQueuedClaims(ctx, staleRequest); err == nil {
		t.Fatal("stale migration receipt rebound a legacy claim")
	}
	recoveryServer := &server{taskLedger: ledger, orchestratorAPIKey: "legacy-recovery-operator-key"}
	recoveryBody, err := json.Marshal(recoveryRequest)
	if err != nil {
		t.Fatal(err)
	}
	recoveryHTTP := httptest.NewRequest(http.MethodPost, "/agents/tasks/migrate", bytes.NewReader(recoveryBody))
	recoveryHTTP.Header.Set("Content-Type", "application/json")
	recoveryHTTP.Header.Set("X-API-Key", "legacy-recovery-operator-key")
	recoveryRecorder := httptest.NewRecorder()
	recoveryServer.agentTaskDeliveryRoute(recoveryRecorder, recoveryHTTP)
	if recoveryRecorder.Code != http.StatusOK {
		t.Fatalf("operator recovery route failed: status=%d body=%s", recoveryRecorder.Code, recoveryRecorder.Body.String())
	}
	var recoveryEnvelope map[string]any
	if err := json.Unmarshal(recoveryRecorder.Body.Bytes(), &recoveryEnvelope); err != nil {
		t.Fatal(err)
	}
	receipt := anyMap(recoveryEnvelope["migration"])
	if anyToInt(receipt["rebound_claims"], -1) != 1 || anyToBool(receipt["idempotent_replay"]) {
		t.Fatalf("explicit legacy recovery failed: receipt=%#v", receipt)
	}
	var workerID, bindingIdentity, bindingInstance, bindingState string
	var eligible, generation int
	if err := ledger.db.QueryRow(`SELECT claim_worker_id,claim_eligible FROM task_ledger_tasks WHERE id=?`, "legacy-recovery-task").Scan(&workerID, &eligible); err != nil {
		t.Fatal(err)
	}
	if err := ledger.db.QueryRow(`SELECT identity_id,worker_instance_id,state,worker_identity_update_generation FROM task_ledger_worker_task_bindings WHERE task_id=?`, "legacy-recovery-task").Scan(&bindingIdentity, &bindingInstance, &bindingState, &generation); err != nil {
		t.Fatal(err)
	}
	if workerID != newIdentity.CanonicalWorkerID || eligible != 1 || bindingIdentity != newIdentity.IdentityID || bindingInstance != newIdentity.WorkerInstanceID || bindingState != "bound" || generation != newIdentity.IdentityUpdateGeneration {
		t.Fatalf("explicit recovery did not rebind exact queue: worker=%q eligible=%d identity=%q instance=%q state=%q generation=%d", workerID, eligible, bindingIdentity, bindingInstance, bindingState, generation)
	}
	replay, err := ledger.rebindLegacyWorkerIdentityQueuedClaims(ctx, recoveryRequest)
	if err != nil || anyToInt(replay["rebound_claims"], -1) != 0 || !anyToBool(replay["idempotent_replay"]) {
		t.Fatalf("legacy recovery replay was not idempotent: receipt=%#v err=%v", replay, err)
	}
	if recoveredClaim, err := ledger.claimTaskWithIdentity(ctx, newIdentity.CanonicalWorkerID, newAuthority.WorkerInstanceID, workspace, "legacy-recovery-task", newIdentity.IdentityUpdateGeneration); err != nil || recoveredClaim == nil {
		t.Fatalf("replacement identity could not claim recovered task: claim=%#v err=%v", recoveredClaim, err)
	}
}

func TestLegacyWorkerIdentityCredentialMigrationLeavesAmbiguousQueuedHintRecoverable(t *testing.T) {
	ledger, _ := identityTestLedgers(t)
	defer ledger.close()
	ctx := context.Background()
	const principal = "legacy-queued-claim-principal"
	const workspace = "legacy-queued-claim-workspace"
	const instance = "legacy-queued-claim-instance"
	const requested = "legacy-queued-claim-worker"
	manifest := testAgentTaskManifest("legacy-queued-claim-task", "legacy-queued-claim-project", principal, "legacy-queued-claim-session")
	manifest["workspace_id"] = workspace
	manifest["metadata"] = map[string]any{"worker": requested}
	if _, err := ledger.submit(ctx, manifest); err != nil {
		t.Fatal(err)
	}
	insertLegacyCredentialIdentity(t, ledger, agentWorkerIdentityRecord{
		IdentityID: "legacy-queued-claim-identity", PrincipalID: principal, WorkspaceID: workspace,
		RequestedWorkerID: requested, CanonicalWorkerID: requested, WorkerInstanceID: instance,
	})
	if err := ledger.migrateLegacyWorkerIdentitiesAtStartup(ctx); err != nil {
		t.Fatalf("queued claim migration failed: %v", err)
	}
	var identityStatus string
	if err := ledger.db.QueryRow(`SELECT status FROM task_ledger_worker_identities WHERE identity_id=?`, "legacy-queued-claim-identity").Scan(&identityStatus); err != nil {
		t.Fatal(err)
	}
	var taskStatus, claimWorkerID string
	var claimEligible int
	if err := ledger.db.QueryRow(`SELECT status,claim_eligible,claim_worker_id FROM task_ledger_tasks WHERE id=?`, "legacy-queued-claim-task").Scan(&taskStatus, &claimEligible, &claimWorkerID); err != nil {
		t.Fatal(err)
	}
	if identityStatus != "closed" || taskStatus != "queued" || claimEligible != 1 || claimWorkerID != requested {
		t.Fatalf("ambiguous queued hint was not preserved as recoverable: identity=%q task=%q eligible=%d worker=%q", identityStatus, taskStatus, claimEligible, claimWorkerID)
	}
	if bindingCount := func() int {
		var count int
		_ = ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_worker_task_bindings WHERE task_id=?`, "legacy-queued-claim-task").Scan(&count)
		return count
	}(); bindingCount != 0 {
		t.Fatalf("ambiguous requested hint acquired an invented identity binding: %d", bindingCount)
	}
	var receiptDetails string
	if err := ledger.db.QueryRow(`SELECT details_json FROM task_ledger_migration_receipts WHERE phase=?`, workerIdentityCredentialMigrationPhase).Scan(&receiptDetails); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(receiptDetails, "legacy-queued-claim-task") || strings.Contains(receiptDetails, "claim_rebind_required") {
		t.Fatalf("migration receipt claimed an exact handoff for an ambiguous queue hint: %s", receiptDetails)
	}
}

func TestWorkerIdentityMigrationExactBindingDoesNotTouchSameRequestedInstance(t *testing.T) {
	ledger, _ := identityTestLedgers(t)
	defer ledger.close()
	ctx := context.Background()
	const principal = "exact-binding-principal"
	const workspace = "exact-binding-workspace"
	requested := "exact-binding-worker"
	authorityA := identityTestAuthority(principal, workspace, "exact-binding-instance-a")
	authorityB := identityTestAuthority(principal, workspace, "exact-binding-instance-b")
	_, identityA := registerIdentityForTest(t, ledger, authorityA, requested)
	responseB, identityB := registerIdentityForTest(t, ledger, authorityB, requested)
	if !anyToBool(responseB["identity_update_required"]) {
		t.Fatal("same-requested second identity did not receive a canonical update")
	}
	acknowledgeIdentityForTest(t, ledger, authorityB, anyToString(anyMap(responseB["identity_update"])["update_id"]))
	identityB, _ = ledger.workerIdentityByAuthority(ctx, authorityB)
	manifest := testAgentTaskManifest("exact-binding-task", "exact-binding-project", principal, "exact-binding-session")
	manifest["workspace_id"] = workspace
	manifest["metadata"] = map[string]any{"worker": identityA.CanonicalWorkerID, "worker_instance_id": authorityA.WorkerInstanceID}
	if _, err := ledger.submit(ctx, manifest); err != nil {
		t.Fatalf("submit exact bound task: %v", err)
	}
	var boundIdentity, boundInstance string
	if err := ledger.db.QueryRow(`SELECT identity_id,worker_instance_id FROM task_ledger_worker_task_bindings WHERE task_id=?`, "exact-binding-task").Scan(&boundIdentity, &boundInstance); err != nil {
		t.Fatalf("read exact queued binding: %v", err)
	}
	if boundIdentity != identityA.IdentityID || boundInstance != authorityA.WorkerInstanceID {
		t.Fatalf("queued task was not bound to A: identity=%q instance=%q A=%#v", boundIdentity, boundInstance, identityA)
	}
	// B has the same principal/workspace/requested spelling but must not be
	// able to claim A's exact queue binding or block A's retirement predicate.
	if claim, err := ledger.claimTask(ctx, identityB.RequestedWorkerID, authorityB.WorkerInstanceID, workspace, "exact-binding-task"); err != nil {
		t.Fatalf("same-requested B claim returned an unexpected error: %v", err)
	} else if claim != nil {
		t.Fatalf("same-requested B inherited A's exact queue binding: %#v", claim)
	}
	if _, err := ledger.retireWorkerIdentity(ctx, workerIdentityRetirementPayloadForTest(identityB), authorityB); err != nil {
		t.Fatalf("B retirement was blocked by A's exact queued binding: %v", err)
	}
	identityA, _ = ledger.workerIdentityByAuthority(ctx, authorityA)
	if _, err := ledger.retireWorkerIdentity(ctx, workerIdentityRetirementPayloadForTest(identityA), authorityA); err == nil || !strings.Contains(strings.ToLower(err.Error()), "task") {
		t.Fatalf("A retirement did not remain blocked by its own exact queued binding: %v", err)
	}
	var eligible int
	if err := ledger.db.QueryRow(`SELECT claim_eligible FROM task_ledger_tasks WHERE id=?`, "exact-binding-task").Scan(&eligible); err != nil {
		t.Fatal(err)
	}
	if eligible != 1 {
		t.Fatalf("B changed A's queued eligibility: %d", eligible)
	}
}

func TestWorkerIdentityUpdateAckRebindsExactQueuedClaimWithDurableReceipt(t *testing.T) {
	ledger, second := identityTestLedgers(t)
	ctx := context.Background()
	const principal = "queued-rebind-principal"
	const workspace = "queued-rebind-workspace"
	const requested = "queued-rebind-worker"
	authority := identityTestAuthority(principal, workspace, "queued-rebind-instance")
	occupier := identityTestAuthority("queued-rebind-occupier", workspace, "queued-rebind-occupier-instance")
	registerIdentityForTest(t, ledger, occupier, requested)
	response, identity := registerIdentityForTest(t, ledger, authority, requested)
	updateID := anyToString(anyMap(response["identity_update"])["update_id"])
	if updateID == "" || identity.IdentityUpdateGeneration != 1 {
		t.Fatalf("expected pending canonical update: response=%#v identity=%#v", response, identity)
	}
	manifest := testAgentTaskManifest("queued-rebind-task", "queued-rebind-project", principal, "queued-rebind-session")
	manifest["workspace_id"] = workspace
	manifest["metadata"] = map[string]any{"worker": requested, "worker_instance_id": authority.WorkerInstanceID}
	if _, err := ledger.submit(ctx, manifest); err != nil {
		t.Fatalf("submit queued rebind task: %v", err)
	}
	tx, err := ledger.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindWorkerIdentityTaskTx(ctx, tx, "queued-rebind-task", identity, requested, 0); err != nil {
		_ = tx.Rollback()
		t.Fatalf("seed exact pre-update queue binding: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	update := acknowledgeIdentityForTest(t, ledger, authority, updateID)
	var workerID, bindingWorkerID, bindingState, receiptDigest string
	var claimEligible, bindingGeneration int
	if err := ledger.db.QueryRow(`SELECT claim_worker_id,claim_eligible FROM task_ledger_tasks WHERE id=?`, "queued-rebind-task").Scan(&workerID, &claimEligible); err != nil {
		t.Fatal(err)
	}
	if err := ledger.db.QueryRow(`SELECT worker_id,worker_identity_update_generation,state,rebind_receipt_digest FROM task_ledger_worker_task_bindings WHERE task_id=?`, "queued-rebind-task").Scan(&bindingWorkerID, &bindingGeneration, &bindingState, &receiptDigest); err != nil {
		t.Fatal(err)
	}
	if workerID != identity.CanonicalWorkerID || bindingWorkerID != identity.CanonicalWorkerID || bindingGeneration != update.IdentityUpdateGeneration || bindingState != "bound" || receiptDigest == "" || claimEligible != 1 {
		t.Fatalf("queued claim was not durably rebound: task_worker=%q binding_worker=%q generation=%d state=%q receipt=%q eligible=%d", workerID, bindingWorkerID, bindingGeneration, bindingState, receiptDigest, claimEligible)
	}
	var rebindEvents int
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_events WHERE task_id=? AND message=?`, "queued-rebind-task", "worker identity update rebound an exact queued claim").Scan(&rebindEvents); err != nil {
		t.Fatal(err)
	}
	if rebindEvents != 1 {
		t.Fatalf("expected one durable queued rebind event, got %d", rebindEvents)
	}
	// Wrong-principal and stale-generation acknowledgements fail before any
	// task mutation; the exact ACK is idempotent after a restart.
	wrong := identityTestAckPayload(update, identityTestAuthority("wrong-principal", workspace, authority.WorkerInstanceID))
	if _, err := ledger.acknowledgeWorkerIdentityUpdate(ctx, wrong, identityTestAuthority("wrong-principal", workspace, authority.WorkerInstanceID)); err == nil {
		t.Fatal("wrong-principal queued rebind acknowledgement was accepted")
	}
	stale := identityTestAckPayload(update, authority)
	stale["worker_identity_update_generation"] = update.IdentityUpdateGeneration + 1
	if _, err := ledger.acknowledgeWorkerIdentityUpdate(ctx, stale, authority); err == nil {
		t.Fatal("stale queued rebind acknowledgement was accepted")
	}
	if err := ledger.close(); err != nil {
		t.Fatal(err)
	}
	if err := second.close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatalf("restart queued rebind ledger: %v", err)
	}
	defer restarted.close()
	replay := identityTestAckPayload(update, authority)
	ack, err := restarted.acknowledgeWorkerIdentityUpdate(ctx, replay, authority)
	if err != nil || !anyToBool(ack["idempotent_replay"]) {
		t.Fatalf("queued rebind ACK replay was not idempotent: ack=%#v err=%v", ack, err)
	}
	if claim, err := restarted.claimTaskWithIdentity(ctx, identity.CanonicalWorkerID, authority.WorkerInstanceID, workspace, "queued-rebind-task", update.IdentityUpdateGeneration); err != nil || claim == nil {
		t.Fatalf("exact rebound identity could not claim after restart: claim=%#v err=%v", claim, err)
	}
}

func TestWorkerIdentityUpdateAckRebindsBlankAndNonQueuedBindings(t *testing.T) {
	ledger, second := identityTestLedgers(t)
	ctx := context.Background()
	const principal = "ack-binding-principal"
	const workspace = "ack-binding-workspace"
	const requested = "ack-binding-worker"
	authority := identityTestAuthority(principal, workspace, "ack-binding-instance")
	occupier := identityTestAuthority("ack-binding-occupier", workspace, "ack-binding-occupier-instance")
	registerIdentityForTest(t, ledger, occupier, requested)
	response, identity := registerIdentityForTest(t, ledger, authority, requested)
	updateID := anyToString(anyMap(response["identity_update"])["update_id"])
	if updateID == "" || identity.IdentityUpdateGeneration != 1 {
		t.Fatalf("expected pending canonical update: response=%#v identity=%#v", response, identity)
	}

	// This task is generic at the task projection layer (blank claim_worker_id),
	// but its exact generation-zero binding proves that the pending identity
	// owns it. ACK must be able to move that projection to the new canonical ID.
	queuedManifest := testAgentTaskManifest("ack-binding-queued", "ack-binding-project", principal, "ack-binding-queued-session")
	queuedManifest["workspace_id"] = workspace
	if _, err := ledger.submit(ctx, queuedManifest); err != nil {
		t.Fatalf("submit blank queued task: %v", err)
	}
	tx, err := ledger.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindWorkerIdentityTaskTx(ctx, tx, "ack-binding-queued", identity, requested, 0); err != nil {
		_ = tx.Rollback()
		t.Fatalf("bind blank queued task: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET claim_worker_id='' WHERE id=?`, "ack-binding-queued"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("clear queued generic claim projection: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// This task is claimed through the trusted compatibility surface before
	// ACK, leaving a running generation-zero binding and a blank projection.
	runningManifest := testAgentTaskManifest("ack-binding-running", "ack-binding-project", principal, "ack-binding-running-session")
	runningManifest["workspace_id"] = workspace
	if _, err := ledger.submit(ctx, runningManifest); err != nil {
		t.Fatalf("submit blank running task: %v", err)
	}
	claim, err := ledger.claimTask(ctx, requested, authority.WorkerInstanceID, workspace, "ack-binding-running")
	if err != nil || claim == nil {
		t.Fatalf("claim blank running task before ACK: claim=%#v err=%v", claim, err)
	}
	runningFence := testAgentTaskFenceFromClaim(t, claim)
	if _, err := ledger.heartbeat(ctx, runningFence); err != nil {
		t.Fatalf("heartbeat running generation-zero binding: %v", err)
	}

	update := acknowledgeIdentityForTest(t, ledger, authority, updateID)
	for _, taskID := range []string{"ack-binding-queued", "ack-binding-running"} {
		var claimWorkerID, bindingWorkerID, bindingState string
		var bindingGeneration int
		if err := ledger.db.QueryRow(`SELECT t.claim_worker_id,b.worker_id,b.worker_identity_update_generation,b.state FROM task_ledger_tasks t JOIN task_ledger_worker_task_bindings b ON b.task_id=t.id WHERE t.id=?`, taskID).Scan(&claimWorkerID, &bindingWorkerID, &bindingGeneration, &bindingState); err != nil {
			t.Fatalf("read rebound task %s: %v", taskID, err)
		}
		if claimWorkerID != identity.CanonicalWorkerID || bindingWorkerID != identity.CanonicalWorkerID || bindingGeneration != update.IdentityUpdateGeneration || bindingState != "bound" {
			t.Fatalf("exact %s binding was not rebound across task status: claim_worker=%q binding_worker=%q generation=%d state=%q", taskID, claimWorkerID, bindingWorkerID, bindingGeneration, bindingState)
		}
	}
	var queuedEligible, runningEligible int
	if err := ledger.db.QueryRow(`SELECT claim_eligible FROM task_ledger_tasks WHERE id=?`, "ack-binding-queued").Scan(&queuedEligible); err != nil {
		t.Fatal(err)
	}
	if err := ledger.db.QueryRow(`SELECT claim_eligible FROM task_ledger_tasks WHERE id=?`, "ack-binding-running").Scan(&runningEligible); err != nil {
		t.Fatal(err)
	}
	if queuedEligible != 1 || runningEligible != 0 {
		t.Fatalf("ACK changed eligibility outside the queued projection: queued=%d running=%d", queuedEligible, runningEligible)
	}

	// The old lease remains valid for its exact old attempt fence. Its retry
	// must retain the new binding and canonical projection rather than reviving
	// the requested spelling.
	exitCode := 125
	if _, err := ledger.observe(ctx, runningFence, "failed", &exitCode, map[string]any{"source": "identity-update-ack"}); err != nil {
		t.Fatalf("requeue running task after ACK: %v", err)
	}
	var status, claimWorkerID string
	if err := ledger.db.QueryRow(`SELECT status,claim_worker_id FROM task_ledger_tasks WHERE id=?`, "ack-binding-running").Scan(&status, &claimWorkerID); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || claimWorkerID != identity.CanonicalWorkerID {
		t.Fatalf("requeued task revived stale generic binding: status=%q claim_worker=%q", status, claimWorkerID)
	}

	// Replays from a wrong principal, another instance, or a stale generation
	// fail before touching either exact binding.
	wrongPrincipal := identityTestAuthority("ack-binding-wrong-principal", workspace, authority.WorkerInstanceID)
	if _, err := ledger.acknowledgeWorkerIdentityUpdate(ctx, identityTestAckPayload(update, wrongPrincipal), wrongPrincipal); err == nil {
		t.Fatal("wrong-principal ACK replay was accepted")
	}
	otherInstance := identityTestAuthority(principal, workspace, "ack-binding-other-instance")
	if _, err := ledger.acknowledgeWorkerIdentityUpdate(ctx, identityTestAckPayload(update, otherInstance), otherInstance); err == nil {
		t.Fatal("other-instance ACK replay was accepted")
	}
	stale := identityTestAckPayload(update, authority)
	stale["worker_identity_update_generation"] = update.IdentityUpdateGeneration + 1
	if _, err := ledger.acknowledgeWorkerIdentityUpdate(ctx, stale, authority); err == nil {
		t.Fatal("stale-generation ACK replay was accepted")
	}

	if err := ledger.close(); err != nil {
		t.Fatal(err)
	}
	if err := second.close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatalf("restart after non-queued ACK rebind: %v", err)
	}
	defer restarted.close()
	replay, err := restarted.acknowledgeWorkerIdentityUpdate(ctx, identityTestAckPayload(update, authority), authority)
	if err != nil || !anyToBool(replay["idempotent_replay"]) {
		t.Fatalf("ACK replay after restart was not idempotent: replay=%#v err=%v", replay, err)
	}
	if claim, err := restarted.claimTaskWithIdentity(ctx, identity.CanonicalWorkerID, authority.WorkerInstanceID, workspace, "ack-binding-running", update.IdentityUpdateGeneration); err != nil || claim == nil {
		t.Fatalf("requeued exact binding was not claimable after restart: claim=%#v err=%v", claim, err)
	}
}

func TestWorkerIdentityAckAdoptsPreRegistrationLeaseBeforeRequeue(t *testing.T) {
	ledger, second := identityTestLedgers(t)
	ctx := context.Background()
	const principal = "pre-registration-owner"
	const workspace = "pre-registration-workspace"
	const requested = "pre-registration-worker"
	authority := identityTestAuthority(principal, workspace, "pre-registration-instance")
	occupier := identityTestAuthority("pre-registration-occupier", workspace, "pre-registration-occupier-instance")
	registerIdentityForTest(t, ledger, occupier, requested)

	manifest := testAgentTaskManifest("pre-registration-lease", "pre-registration-project", principal, "pre-registration-session")
	manifest["workspace_id"] = workspace
	if _, err := ledger.submit(ctx, manifest); err != nil {
		t.Fatalf("submit pre-registration task: %v", err)
	}
	// The worker claims before registration. There is no identity row to bind
	// yet, so this intentionally exercises the generation-zero compatibility
	// surface that the ACK path must adopt later.
	legacyClaim, err := ledger.claimTask(ctx, requested, authority.WorkerInstanceID, workspace, "pre-registration-lease")
	if err != nil || legacyClaim == nil {
		t.Fatalf("pre-registration generation-zero claim failed: claim=%#v err=%v", legacyClaim, err)
	}
	var bindingsBefore int
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_worker_task_bindings WHERE task_id=?`, "pre-registration-lease").Scan(&bindingsBefore); err != nil {
		t.Fatal(err)
	}
	if bindingsBefore != 0 {
		t.Fatalf("pre-registration claim unexpectedly acquired an identity binding: %d", bindingsBefore)
	}

	response, identity := registerIdentityForTest(t, ledger, authority, requested)
	updateID := anyToString(anyMap(response["identity_update"])["update_id"])
	if updateID == "" || identity.IdentityUpdateGeneration != 1 || identity.CanonicalWorkerID == identity.RequestedWorkerID {
		t.Fatalf("collision registration did not issue the expected canonical update: response=%#v identity=%#v", response, identity)
	}
	update := acknowledgeIdentityForTest(t, ledger, authority, updateID)
	var bindingIdentity, bindingWorker, bindingInstance, bindingState string
	var bindingGeneration int
	if err := ledger.db.QueryRow(`SELECT identity_id,worker_id,worker_instance_id,worker_identity_update_generation,state FROM task_ledger_worker_task_bindings WHERE task_id=?`, "pre-registration-lease").Scan(&bindingIdentity, &bindingWorker, &bindingInstance, &bindingGeneration, &bindingState); err != nil {
		t.Fatalf("ACK did not adopt the pre-registration attempt binding: %v", err)
	}
	if bindingIdentity != identity.IdentityID || bindingWorker != identity.CanonicalWorkerID || bindingInstance != authority.WorkerInstanceID || bindingGeneration != update.IdentityUpdateGeneration || bindingState != "bound" {
		t.Fatalf("pre-registration attempt binding is not exact/current: identity=%q worker=%q instance=%q generation=%d state=%q", bindingIdentity, bindingWorker, bindingInstance, bindingGeneration, bindingState)
	}
	var projectedWorker string
	if err := ledger.db.QueryRow(`SELECT claim_worker_id FROM task_ledger_tasks WHERE id=?`, "pre-registration-lease").Scan(&projectedWorker); err != nil {
		t.Fatal(err)
	}
	if projectedWorker != identity.CanonicalWorkerID {
		t.Fatalf("ACK did not normalize the pre-registration task projection: %q", projectedWorker)
	}

	legacyFence := testAgentTaskFenceFromClaim(t, legacyClaim)
	server := &server{taskLedger: ledger}
	currentAuth := agentTaskRouteAuth{Principal: principal, Workspace: workspace, CanonicalWorkerID: identity.CanonicalWorkerID, WorkerInstanceID: authority.WorkerInstanceID, WorkerIdentityUpdateGeneration: identity.IdentityUpdateGeneration, Signed: true}
	if err := server.authorizeAgentTaskFence(ctx, &currentAuth, legacyFence); err != nil {
		t.Fatalf("exact pre-registration fence was not retained after ACK: %v", err)
	}
	wrongPrincipal := currentAuth
	wrongPrincipal.Principal = "pre-registration-wrong-principal"
	if err := server.authorizeAgentTaskFence(ctx, &wrongPrincipal, legacyFence); err == nil {
		t.Fatal("wrong-principal pre-registration fence was accepted")
	}
	otherInstance := currentAuth
	otherInstance.WorkerInstanceID = "pre-registration-other-instance"
	if err := server.authorizeAgentTaskFence(ctx, &otherInstance, legacyFence); err == nil {
		t.Fatal("other-instance pre-registration fence was accepted")
	}
	staleFence := legacyFence
	staleFence.WorkerIdentityUpdateGeneration = identity.IdentityUpdateGeneration + 1
	if err := server.authorizeAgentTaskFence(ctx, &currentAuth, staleFence); err == nil {
		t.Fatal("stale pre-registration fence generation was accepted")
	}

	exitCode := 125
	if _, err := ledger.observe(ctx, legacyFence, "failed", &exitCode, map[string]any{"source": "pre-registration-ack-adoption"}); err != nil {
		t.Fatalf("failed pre-registration attempt did not requeue: %v", err)
	}
	var status string
	if err := ledger.db.QueryRow(`SELECT status,claim_worker_id FROM task_ledger_tasks WHERE id=?`, "pre-registration-lease").Scan(&status, &projectedWorker); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || projectedWorker != identity.CanonicalWorkerID {
		t.Fatalf("failed pre-registration attempt requeued without the canonical projection: status=%q worker=%q", status, projectedWorker)
	}
	// The old requested spelling is no longer a claimant: the current exact
	// binding blocks the compatibility path even though the task is generic.
	oldClaim, err := ledger.claimTask(ctx, requested, authority.WorkerInstanceID, workspace, "pre-registration-lease")
	if err != nil {
		t.Fatalf("old compatibility claimant returned an unexpected error: %v", err)
	}
	if oldClaim != nil {
		t.Fatalf("old requested worker reclaimed the adopted task: %#v", oldClaim)
	}

	if err := ledger.close(); err != nil {
		t.Fatal(err)
	}
	if err := second.close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatalf("restart after pre-registration requeue: %v", err)
	}
	defer restarted.close()
	restartedIdentity, err := restarted.workerIdentityByAuthority(ctx, authority)
	if err != nil {
		t.Fatal(err)
	}
	if oldClaim, err := restarted.claimTask(ctx, requested, authority.WorkerInstanceID, workspace, "pre-registration-lease"); err != nil || oldClaim != nil {
		t.Fatalf("old requested worker reclaimed after restart: claim=%#v err=%v", oldClaim, err)
	}
	canonicalClaim, err := restarted.claimTaskWithIdentity(ctx, restartedIdentity.CanonicalWorkerID, authority.WorkerInstanceID, workspace, "pre-registration-lease", restartedIdentity.IdentityUpdateGeneration)
	if err != nil || canonicalClaim == nil {
		t.Fatalf("canonical owner could not reclaim adopted task after restart: claim=%#v err=%v", canonicalClaim, err)
	}
}

func TestWorkerIdentityAckAdoptsPreRegistrationRetryQueuedAttempt(t *testing.T) {
	ledger, second := identityTestLedgers(t)
	ctx := context.Background()
	const principal = "retry-before-registration-owner"
	const workspace = "retry-before-registration-workspace"
	const requested = "retry-before-registration-worker"
	authority := identityTestAuthority(principal, workspace, "retry-before-registration-instance")
	occupier := identityTestAuthority("retry-before-registration-occupier", workspace, "retry-before-registration-occupier-instance")
	registerIdentityForTest(t, ledger, occupier, requested)

	manifest := testAgentTaskManifest("pre-registration-retry", "pre-registration-retry-project", principal, "pre-registration-retry-session")
	manifest["workspace_id"] = workspace
	if _, err := ledger.submit(ctx, manifest); err != nil {
		t.Fatalf("submit pre-registration retry task: %v", err)
	}
	legacyClaim, err := ledger.claimTask(ctx, requested, authority.WorkerInstanceID, workspace, "pre-registration-retry")
	if err != nil || legacyClaim == nil {
		t.Fatalf("pre-registration retry claim failed: claim=%#v err=%v", legacyClaim, err)
	}
	legacyFence := testAgentTaskFenceFromClaim(t, legacyClaim)
	exitCode := 125
	if _, err := ledger.observe(ctx, legacyFence, "failed", &exitCode, map[string]any{"source": "retry-before-registration"}); err != nil {
		t.Fatalf("pre-registration attempt did not fail/requeue before registration: %v", err)
	}
	var taskStatus, activeAttemptID, claimWorkerID, failureDisposition string
	var taskAttemptNumber, taskGeneration, attemptNumber, attemptGeneration, bindingsBefore int
	if err := ledger.db.QueryRow(`SELECT status,active_attempt_id,claim_worker_id,attempt_number,generation FROM task_ledger_tasks WHERE id=?`, "pre-registration-retry").Scan(&taskStatus, &activeAttemptID, &claimWorkerID, &taskAttemptNumber, &taskGeneration); err != nil {
		t.Fatal(err)
	}
	if err := ledger.db.QueryRow(`SELECT failure_disposition,attempt_number,generation FROM task_ledger_attempts WHERE attempt_id=?`, legacyFence.AttemptID).Scan(&failureDisposition, &attemptNumber, &attemptGeneration); err != nil {
		t.Fatal(err)
	}
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_worker_task_bindings WHERE task_id=?`, "pre-registration-retry").Scan(&bindingsBefore); err != nil {
		t.Fatal(err)
	}
	if taskStatus != "queued" || activeAttemptID != "" || claimWorkerID != "" || taskAttemptNumber != attemptNumber || taskGeneration != attemptGeneration || failureDisposition != "retry_queued" || bindingsBefore != 0 {
		t.Fatalf("pre-registration retry was not the exact unbound latest attempt: task_status=%q active=%q claim_worker=%q task_attempt=%d/%d attempt=%d/%d disposition=%q bindings=%d", taskStatus, activeAttemptID, claimWorkerID, taskAttemptNumber, taskGeneration, attemptNumber, attemptGeneration, failureDisposition, bindingsBefore)
	}

	response, identity := registerIdentityForTest(t, ledger, authority, requested)
	updateID := anyToString(anyMap(response["identity_update"])["update_id"])
	if updateID == "" || identity.IdentityUpdateGeneration != 1 || identity.CanonicalWorkerID == identity.RequestedWorkerID {
		t.Fatalf("collision registration did not issue the expected retry update: response=%#v identity=%#v", response, identity)
	}
	update := acknowledgeIdentityForTest(t, ledger, authority, updateID)
	var bindingIdentity, bindingWorker, bindingInstance, bindingState string
	var bindingGeneration int
	if err := ledger.db.QueryRow(`SELECT identity_id,worker_id,worker_instance_id,worker_identity_update_generation,state FROM task_ledger_worker_task_bindings WHERE task_id=?`, "pre-registration-retry").Scan(&bindingIdentity, &bindingWorker, &bindingInstance, &bindingGeneration, &bindingState); err != nil {
		t.Fatalf("ACK missed the latest queued retry attempt: %v", err)
	}
	if bindingIdentity != identity.IdentityID || bindingWorker != identity.CanonicalWorkerID || bindingInstance != authority.WorkerInstanceID || bindingGeneration != update.IdentityUpdateGeneration || bindingState != "bound" {
		t.Fatalf("queued retry binding is not exact/current: identity=%q worker=%q instance=%q generation=%d state=%q", bindingIdentity, bindingWorker, bindingInstance, bindingGeneration, bindingState)
	}
	if err := ledger.db.QueryRow(`SELECT claim_worker_id FROM task_ledger_tasks WHERE id=?`, "pre-registration-retry").Scan(&claimWorkerID); err != nil {
		t.Fatal(err)
	}
	if claimWorkerID != identity.CanonicalWorkerID {
		t.Fatalf("ACK did not normalize the queued retry projection: %q", claimWorkerID)
	}

	// An exact ACK replay is idempotent, while copied authority cannot replay
	// the update or claim the task through the old requested spelling.
	replay, err := ledger.acknowledgeWorkerIdentityUpdate(ctx, identityTestAckPayload(update, authority), authority)
	if err != nil || !anyToBool(replay["idempotent_replay"]) {
		t.Fatalf("queued retry ACK replay was not idempotent: replay=%#v err=%v", replay, err)
	}
	wrongPrincipal := identityTestAuthority("retry-before-registration-wrong-principal", workspace, authority.WorkerInstanceID)
	if _, err := ledger.acknowledgeWorkerIdentityUpdate(ctx, identityTestAckPayload(update, wrongPrincipal), wrongPrincipal); err == nil {
		t.Fatal("wrong-principal queued retry ACK replay was accepted")
	}
	otherInstance := identityTestAuthority(principal, workspace, "retry-before-registration-other-instance")
	if _, err := ledger.acknowledgeWorkerIdentityUpdate(ctx, identityTestAckPayload(update, otherInstance), otherInstance); err == nil {
		t.Fatal("other-instance queued retry ACK replay was accepted")
	}
	if oldClaim, err := ledger.claimTask(ctx, requested, authority.WorkerInstanceID, workspace, "pre-registration-retry"); err != nil || oldClaim != nil {
		t.Fatalf("old requested owner reclaimed queued retry before restart: claim=%#v err=%v", oldClaim, err)
	}
	if occupierClaim, err := ledger.claimTask(ctx, requested, occupier.WorkerInstanceID, workspace, "pre-registration-retry"); err != nil || occupierClaim != nil {
		t.Fatalf("same-requested occupier reclaimed queued retry: claim=%#v err=%v", occupierClaim, err)
	}

	if err := ledger.close(); err != nil {
		t.Fatal(err)
	}
	if err := second.close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatalf("restart after queued retry adoption: %v", err)
	}
	defer restarted.close()
	restartedIdentity, err := restarted.workerIdentityByAuthority(ctx, authority)
	if err != nil {
		t.Fatal(err)
	}
	if oldClaim, err := restarted.claimTask(ctx, requested, authority.WorkerInstanceID, workspace, "pre-registration-retry"); err != nil || oldClaim != nil {
		t.Fatalf("old requested owner reclaimed queued retry after restart: claim=%#v err=%v", oldClaim, err)
	}
	if occupierClaim, err := restarted.claimTask(ctx, requested, occupier.WorkerInstanceID, workspace, "pre-registration-retry"); err != nil || occupierClaim != nil {
		t.Fatalf("same-requested occupier reclaimed queued retry after restart: claim=%#v err=%v", occupierClaim, err)
	}
	canonicalClaim, err := restarted.claimTaskWithIdentity(ctx, restartedIdentity.CanonicalWorkerID, authority.WorkerInstanceID, workspace, "pre-registration-retry", restartedIdentity.IdentityUpdateGeneration)
	if err != nil || canonicalClaim == nil {
		t.Fatalf("canonical owner could not reclaim queued retry after restart: claim=%#v err=%v", canonicalClaim, err)
	}
}

func TestWorkerIdentityLegacyVerifierReturnsFencedMigrationChallenge(t *testing.T) {
	ledger, _ := identityTestLedgers(t)
	gateway := &server{taskLedger: ledger}
	const principal = "legacy-challenge-principal"
	const workspace = "legacy-challenge-workspace"
	const instance = "legacy-challenge-instance"
	const requested = "legacy-challenge-worker"
	oldCredential := strings.Repeat("a", workerInstanceCredentialBytes*2)
	newCredential := strings.Repeat("b", workerInstanceCredentialBytes*2)
	authority := identityTestAuthority(principal, workspace, instance)
	response, identity := registerIdentityForTest(t, ledger, authority, requested)
	if anyToBool(response["identity_update_required"]) {
		acknowledgeIdentityForTest(t, ledger, authority, anyToString(anyMap(response["identity_update"])["update_id"]))
		identity, _ = ledger.workerIdentityByAuthority(context.Background(), authority)
	}
	legacyVerifier := legacyWorkerInstanceCredentialVerifier(oldCredential, authority, identity.WorkerInstanceCredentialGeneration)
	if _, err := ledger.db.Exec(`UPDATE task_ledger_worker_identities SET worker_instance_credential_verifier=? WHERE identity_id=?`, legacyVerifier, identity.IdentityID); err != nil {
		t.Fatal(err)
	}
	request, _ := credentialRouteRequest(t, http.MethodPost, "/agents/workers/register", principal, workspace, instance, newCredential, map[string]any{
		"requested_worker_id": requested, "worker_instance_id": instance,
	})
	status, challenge, _ := callCredentialHTTPIdentityRoute(t, gateway, request)
	if status != http.StatusConflict || anyToString(challenge["code"]) != "worker_identity_credential_migration_required" {
		t.Fatalf("legacy verifier mismatch did not return fenced migration challenge: status=%d response=%#v", status, challenge)
	}
	fence := anyMap(challenge["migration_challenge"])
	if anyToString(fence["identity_id"]) != identity.IdentityID || anyToString(fence["worker_instance_id"]) != instance || anyToString(fence["challenge_digest"]) == "" || strings.Contains(string(credentialTestJSON(challenge)), oldCredential) || strings.Contains(string(credentialTestJSON(challenge)), newCredential) {
		t.Fatalf("migration challenge was not exact and redacted: %#v", challenge)
	}
}

func TestLegacyWorkerIdentityCredentialMigrationQuarantinesPostExecutionWorkAcrossRestart(t *testing.T) {
	ledger, second := identityTestLedgers(t)
	ctx := context.Background()
	const principal = "legacy-postexecution-principal"
	const workspace = "legacy-postexecution-workspace"
	const instance = "legacy-postexecution-instance"
	const requested = "legacy-postexecution-worker"
	manifest := testAgentTaskManifest("legacy-postexecution-task", "legacy-postexecution-project", principal, "legacy-postexecution-session")
	manifest["workspace_id"] = workspace
	if _, err := ledger.submit(ctx, manifest); err != nil {
		t.Fatal(err)
	}
	claim, err := ledger.claimTask(ctx, requested, instance, workspace, "legacy-postexecution-task")
	if err != nil || claim == nil {
		t.Fatalf("claim post-execution lease: claim=%#v err=%v", claim, err)
	}
	attemptID := anyToString(anyMap(claim["attempt"])["attempt_id"])
	if _, err := ledger.db.Exec(`UPDATE task_ledger_attempts SET status='execution_observed',runner_status='observed',runner_exit_observed=1 WHERE attempt_id=?`, attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.db.Exec(`UPDATE task_ledger_tasks SET status='execution_observed' WHERE id=?`, "legacy-postexecution-task"); err != nil {
		t.Fatal(err)
	}
	insertLegacyCredentialIdentity(t, ledger, agentWorkerIdentityRecord{
		IdentityID: "legacy-postexecution-identity", PrincipalID: principal, WorkspaceID: workspace,
		RequestedWorkerID: requested, CanonicalWorkerID: requested, WorkerInstanceID: instance,
	})
	if err := ledger.migrateLegacyWorkerIdentitiesAtStartup(ctx); err != nil {
		t.Fatalf("post-execution legacy migration failed: %v", err)
	}
	var status string
	if err := ledger.db.QueryRow(`SELECT status FROM task_ledger_worker_identities WHERE identity_id=?`, "legacy-postexecution-identity").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "quarantined" {
		t.Fatalf("post-execution legacy identity was closed instead of quarantined: %q", status)
	}
	var closedStatus, blockedKind string
	if err := ledger.db.QueryRow(`SELECT json_extract(details_json,'$.closed_status'),json_extract(details_json,'$.blocked_work.kind') FROM task_ledger_migration_receipts WHERE phase=?`, workerIdentityCredentialMigrationPhase).Scan(&closedStatus, &blockedKind); err != nil {
		t.Fatal(err)
	}
	if closedStatus != "quarantined" || blockedKind != "attempt" {
		t.Fatalf("quarantine receipt did not bind the post-execution blocker: status=%q kind=%q", closedStatus, blockedKind)
	}
	if err := ledger.close(); err != nil {
		t.Fatal(err)
	}
	if err := second.close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatalf("restart after post-execution quarantine failed: %v", err)
	}
	defer restarted.close()
	if err := restarted.db.QueryRow(`SELECT status FROM task_ledger_worker_identities WHERE identity_id=?`, "legacy-postexecution-identity").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "quarantined" {
		t.Fatalf("post-execution quarantine was not durable across restart: %q", status)
	}
}

func TestLegacyWorkerIdentityCredentialStartupMigrationRequeuesLease(t *testing.T) {
	ledger, second := identityTestLedgers(t)
	ctx := context.Background()
	const principal = "legacy-startup-principal"
	const workspace = "legacy-startup-workspace"
	const instance = "legacy-startup-instance"
	const requested = "legacy-startup-worker"
	manifest := testAgentTaskManifest("legacy-startup-task", "legacy-startup-project", principal, "legacy-startup-session")
	manifest["workspace_id"] = workspace
	if _, err := ledger.submit(ctx, manifest); err != nil {
		t.Fatalf("submit startup migration task: %v", err)
	}
	claim, err := ledger.claimTask(ctx, requested, instance, workspace, "legacy-startup-task")
	if err != nil || claim == nil {
		t.Fatalf("claim startup migration lease: claim=%#v err=%v", claim, err)
	}
	legacy := agentWorkerIdentityRecord{
		IdentityID: "legacy-startup-identity", PrincipalID: principal, WorkspaceID: workspace,
		RequestedWorkerID: requested, CanonicalWorkerID: requested, WorkerInstanceID: instance,
		WorkerInstanceCredentialGeneration: workerInstanceCredentialGenerationInitial,
		Status:                             "active", CreatedAt: agentTaskNow(), UpdatedAt: agentTaskNow(),
	}
	legacy.RequestedIDDigest = workerIdentityRequestedDigest(legacy.RequestedWorkerID)
	legacy.IdentityDigest = workerIdentityRecordDigest(legacy)
	if _, err := ledger.db.Exec(`INSERT INTO task_ledger_worker_identities(identity_id,principal_id,workspace_id,requested_worker_id,canonical_worker_id,worker_instance_id,worker_instance_credential_verifier,worker_instance_credential_generation,worker_identity_update_generation,acknowledged_generation,requested_id_digest,identity_digest,status,created_at,updated_at,closed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, legacy.IdentityID, legacy.PrincipalID, legacy.WorkspaceID, legacy.RequestedWorkerID, legacy.CanonicalWorkerID, legacy.WorkerInstanceID, "", legacy.WorkerInstanceCredentialGeneration, legacy.IdentityUpdateGeneration, legacy.AcknowledgedGeneration, legacy.RequestedIDDigest, legacy.IdentityDigest, legacy.Status, legacy.CreatedAt, legacy.UpdatedAt, ""); err != nil {
		t.Fatalf("insert startup legacy identity: %v", err)
	}
	if err := ledger.close(); err != nil {
		t.Fatal(err)
	}
	if err := second.close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatalf("startup legacy migration failed: %v", err)
	}
	defer restarted.close()
	var identityStatus, attemptStatus, taskStatus, activeAttempt string
	if err := restarted.db.QueryRow(`SELECT status FROM task_ledger_worker_identities WHERE identity_id=?`, legacy.IdentityID).Scan(&identityStatus); err != nil {
		t.Fatal(err)
	}
	if err := restarted.db.QueryRow(`SELECT status FROM task_ledger_attempts WHERE attempt_id=?`, anyToString(anyMap(claim["attempt"])["attempt_id"])).Scan(&attemptStatus); err != nil {
		t.Fatal(err)
	}
	if err := restarted.db.QueryRow(`SELECT status,active_attempt_id FROM task_ledger_tasks WHERE id=?`, "legacy-startup-task").Scan(&taskStatus, &activeAttempt); err != nil {
		t.Fatal(err)
	}
	if identityStatus != "closed" || attemptStatus != "execution_failed" || taskStatus != "queued" || activeAttempt != "" {
		t.Fatalf("startup legacy migration did not close/requeue atomically: identity=%q attempt=%q task=%q active_attempt=%q", identityStatus, attemptStatus, taskStatus, activeAttempt)
	}
	var receiptCount int
	if err := restarted.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_migration_receipts WHERE phase=?`, workerIdentityCredentialMigrationPhase).Scan(&receiptCount); err != nil {
		t.Fatal(err)
	}
	if receiptCount != 1 {
		t.Fatalf("startup legacy migration did not write exactly one durable receipt: %d", receiptCount)
	}
}

func TestLegacyWorkerIdentityCredentialMigrationDrainsLargeBatchesAndRecomputesEligibility(t *testing.T) {
	ledger, _ := identityTestLedgers(t)
	defer ledger.close()
	ctx := context.Background()
	const principal = "legacy-large-principal"
	const workspace = "legacy-large-workspace"
	const instance = "legacy-large-instance"
	const requested = "legacy-large-worker"
	const attemptCount = workerIdentityCredentialMigrationBatchSize + 1
	for index := 0; index < attemptCount; index++ {
		taskID := fmt.Sprintf("legacy-large-task-%03d", index)
		manifest := testAgentTaskManifest(taskID, "legacy-large-project", principal, fmt.Sprintf("legacy-large-session-%03d", index))
		manifest["workspace_id"] = workspace
		if _, err := ledger.submit(ctx, manifest); err != nil {
			t.Fatalf("submit large legacy task %d: %v", index, err)
		}
		if claim, err := ledger.claimTask(ctx, requested, instance, workspace, taskID); err != nil || claim == nil {
			t.Fatalf("claim large legacy task %d: claim=%#v err=%v", index, claim, err)
		}
	}
	// A lease makes claim_eligible=0. Change one current task gate while its
	// lease is active; migration must derive the queued value from the current
	// approval/context policy, not blindly restore 1.
	if _, err := ledger.db.Exec(`UPDATE task_ledger_tasks SET approval_policy_json=?,approved=0 WHERE id=?`, `{"required":true}`, "legacy-large-task-000"); err != nil {
		t.Fatalf("make one large legacy task ineligible: %v", err)
	}
	insertLegacyCredentialIdentity(t, ledger, agentWorkerIdentityRecord{
		IdentityID: "legacy-large-identity", PrincipalID: principal, WorkspaceID: workspace,
		RequestedWorkerID: requested, CanonicalWorkerID: requested, WorkerInstanceID: instance,
	})
	if err := ledger.migrateLegacyWorkerIdentitiesAtStartup(ctx); err != nil {
		t.Fatalf("large legacy startup migration failed: %v", err)
	}
	var identityStatus string
	if err := ledger.db.QueryRow(`SELECT status FROM task_ledger_worker_identities WHERE identity_id=?`, "legacy-large-identity").Scan(&identityStatus); err != nil {
		t.Fatal(err)
	}
	if identityStatus != "closed" {
		t.Fatalf("large legacy identity was not closed after bounded progress: %q", identityStatus)
	}
	var failedAttempts, queuedTasks, eligibleTasks, ineligibleTasks int
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_attempts WHERE worker_instance_id=? AND failure_disposition=?`, instance, "credential_migration_requeued").Scan(&failedAttempts); err != nil {
		t.Fatal(err)
	}
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_tasks WHERE workspace_id=? AND status='queued' AND active_attempt_id=''`, workspace).Scan(&queuedTasks); err != nil {
		t.Fatal(err)
	}
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_tasks WHERE workspace_id=? AND status='queued' AND claim_eligible=1`, workspace).Scan(&eligibleTasks); err != nil {
		t.Fatal(err)
	}
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_tasks WHERE workspace_id=? AND status='queued' AND claim_eligible=0`, workspace).Scan(&ineligibleTasks); err != nil {
		t.Fatal(err)
	}
	if failedAttempts != attemptCount || queuedTasks != attemptCount || eligibleTasks != attemptCount-1 || ineligibleTasks != 1 {
		t.Fatalf("large legacy migration lost or over-eligible tasks: attempts=%d queued=%d eligible=%d ineligible=%d", failedAttempts, queuedTasks, eligibleTasks, ineligibleTasks)
	}
	var receiptCount int
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_migration_receipts WHERE source_digest LIKE 'sha256:%' AND phase=?`, workerIdentityCredentialMigrationPhase).Scan(&receiptCount); err != nil {
		t.Fatal(err)
	}
	if receiptCount != 1 {
		t.Fatalf("large legacy migration wrote %d receipts, want one", receiptCount)
	}
}

func TestLegacyWorkerIdentityCredentialMigrationDrainsLargeQueuedBindingSet(t *testing.T) {
	ledger, _ := identityTestLedgers(t)
	defer ledger.close()
	ctx := context.Background()
	const principal = "legacy-large-queued-principal"
	const workspace = "legacy-large-queued-workspace"
	const instance = "legacy-large-queued-instance"
	const requested = "legacy-large-queued-worker"
	const taskCount = workerIdentityCredentialMigrationBatchSize + 1
	insertLegacyCredentialIdentity(t, ledger, agentWorkerIdentityRecord{
		IdentityID: "legacy-large-queued-identity", PrincipalID: principal, WorkspaceID: workspace,
		RequestedWorkerID: requested, CanonicalWorkerID: requested, WorkerInstanceID: instance,
	})
	for index := 0; index < taskCount; index++ {
		taskID := fmt.Sprintf("legacy-large-queued-task-%03d", index)
		manifest := testAgentTaskManifest(taskID, "legacy-large-queued-project", principal, fmt.Sprintf("legacy-large-queued-session-%03d", index))
		manifest["workspace_id"] = workspace
		manifest["metadata"] = map[string]any{"worker": requested, "worker_instance_id": instance}
		if _, err := ledger.submit(ctx, manifest); err != nil {
			t.Fatalf("submit large queued legacy task %d: %v", index, err)
		}
	}
	if err := ledger.migrateLegacyWorkerIdentitiesAtStartup(ctx); err != nil {
		t.Fatalf("large queued legacy startup migration failed: %v", err)
	}
	var closed, ineligible, pending int
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_worker_identities WHERE identity_id=? AND status='closed'`, "legacy-large-queued-identity").Scan(&closed); err != nil {
		t.Fatal(err)
	}
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_tasks WHERE workspace_id=? AND status='queued' AND claim_eligible=0`, workspace).Scan(&ineligible); err != nil {
		t.Fatal(err)
	}
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_worker_task_bindings WHERE identity_id=? AND state='rebind_pending'`, "legacy-large-queued-identity").Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if closed != 1 || ineligible != taskCount || pending != taskCount {
		t.Fatalf("large queued legacy migration did not make durable bounded progress: closed=%d ineligible=%d pending=%d want=%d", closed, ineligible, pending, taskCount)
	}
}

func TestLegacyWorkerIdentityCredentialStartupMigrationDrainsLargeIdentitySet(t *testing.T) {
	ledger, _ := identityTestLedgers(t)
	defer ledger.close()
	const identityCount = workerIdentityCredentialMigrationBatchSize + 1
	for index := 0; index < identityCount; index++ {
		instance := fmt.Sprintf("legacy-identity-instance-%03d", index)
		insertLegacyCredentialIdentity(t, ledger, agentWorkerIdentityRecord{
			IdentityID: fmt.Sprintf("legacy-identity-%03d", index), PrincipalID: "legacy-identity-principal", WorkspaceID: "legacy-identity-workspace",
			RequestedWorkerID: fmt.Sprintf("legacy-identity-worker-%03d", index), CanonicalWorkerID: fmt.Sprintf("legacy-identity-worker-%03d", index), WorkerInstanceID: instance,
		})
	}
	if err := ledger.migrateLegacyWorkerIdentitiesAtStartup(context.Background()); err != nil {
		t.Fatalf("large legacy identity startup migration failed: %v", err)
	}
	var activeLegacy, closedIdentities, receipts int
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_worker_identities WHERE worker_instance_credential_verifier='' AND status='active'`).Scan(&activeLegacy); err != nil {
		t.Fatal(err)
	}
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_worker_identities WHERE workspace_id=? AND status='closed'`, "legacy-identity-workspace").Scan(&closedIdentities); err != nil {
		t.Fatal(err)
	}
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_migration_receipts WHERE phase=?`, workerIdentityCredentialMigrationPhase).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if activeLegacy != 0 || closedIdentities != identityCount || receipts != identityCount {
		t.Fatalf("large legacy identity set was not fully drained: active=%d closed=%d receipts=%d want=%d", activeLegacy, closedIdentities, receipts, identityCount)
	}
}
