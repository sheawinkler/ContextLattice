package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

var testAgentTaskPublicationReconciliationFields = []string{
	"schema_id", "publication_id", "result_id", "idempotency_key", "task_id", "attempt_id", "lease_id",
	"generation", "assignment_generation", "lease_generation", "worker_id", "worker_instance_id", "status",
	"writeback_status", "publication_receipt", "cleanup_authorization",
}

func assertAgentTaskPublicationReconciliationFields(t *testing.T, payload map[string]any) {
	t.Helper()
	want := make(map[string]bool, len(testAgentTaskPublicationReconciliationFields))
	for _, field := range testAgentTaskPublicationReconciliationFields {
		want[field] = true
		if _, exists := payload[field]; !exists {
			t.Fatalf("publication reconciliation missing exact field %q: %#v", field, payload)
		}
	}
	unexpected := make([]string, 0)
	for field := range payload {
		if !want[field] {
			unexpected = append(unexpected, field)
		}
	}
	sort.Strings(unexpected)
	if len(payload) != len(want) || len(unexpected) != 0 {
		t.Fatalf("publication reconciliation is not the closed 16-field response: fields=%d unexpected=%v payload=%#v", len(payload), unexpected, payload)
	}
	if anyToString(payload["schema_id"]) != agentTaskPublicationReconciliationID {
		t.Fatalf("publication reconciliation schema=%q want %q", anyToString(payload["schema_id"]), agentTaskPublicationReconciliationID)
	}
	if err := agentTaskRequireContract(agentTaskPublicationReconciliationID, payload); err != nil {
		t.Fatalf("publication reconciliation violates canonical registry: %v", err)
	}
}

func assertAgentTaskPublicationReconciliationPythonConsumer(t *testing.T, payload map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root for Python reconciliation consumer: %v", err)
	}
	script := `import json, sys
from scripts.agent_contracts import validate_agent_task_publication_reconciliation
findings = validate_agent_task_publication_reconciliation(json.load(sys.stdin))
if findings:
    raise SystemExit(json.dumps(findings, sort_keys=True))
`
	command := exec.Command("python3", "-c", script)
	command.Dir = repoRoot
	command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	command.Stdin = bytes.NewReader(encoded)
	if output, runErr := command.CombinedOutput(); runErr != nil {
		t.Fatalf("production Python reconciliation consumer rejected route payload: %v (%s)", runErr, strings.TrimSpace(string(output)))
	}
}

func testAgentTaskCleanupReceipt(publication map[string]any, fence agentTaskFence) map[string]any {
	authorization := anyMap(publication["cleanup_authorization"])
	receipt := map[string]any{
		"schema_id": agentTaskCleanupReceiptID, "receipt_id": "cleanup-receipt-" + fence.AttemptID,
		"authority": "task-execution-worker", "state": "cleaned",
		"cleanup_id": authorization["cleanup_id"], "workspace_ref": authorization["workspace_ref"],
		"publication_id": publication["publication_id"], "result_id": publication["result_id"],
		"task_id": fence.TaskID, "attempt_id": fence.AttemptID, "lease_id": fence.LeaseID,
		"generation": fence.Generation, "worker_id": fence.WorkerID, "worker_instance_id": fence.WorkerInstanceID,
	}
	receipt["receipt_digest"] = agentTaskDigest(receipt)
	return receipt
}

func TestAgentTaskU3OmittedPublicationIdempotencyDefaultsThroughRouteRestartAndPythonConsumer(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	taskID, project, owner, sessionID := "u3-default-idempotency", "u3-default-project", "u3-default-owner", "sess_u3_default"
	manifest := testAgentTaskManifest(taskID, project, owner, sessionID)
	if _, err := ledger.submit(context.Background(), manifest); err != nil {
		t.Fatalf("submit omitted-idempotency task: %v", err)
	}
	claim, err := ledger.claimNext(context.Background(), "worker-"+taskID, "instance-"+taskID, "")
	if err != nil || claim == nil {
		t.Fatalf("claim omitted-idempotency task: row=%#v err=%v", claim, err)
	}
	fence := testAgentTaskFenceFromClaim(t, claim)
	if _, err := ledger.heartbeat(context.Background(), fence); err != nil {
		t.Fatalf("start omitted-idempotency attempt: %v", err)
	}
	exitCode := 0
	if _, err := ledger.observe(context.Background(), fence, "succeeded", &exitCode, map[string]any{"source": "u3-default-route-test"}); err != nil {
		t.Fatalf("observe omitted-idempotency attempt: %v", err)
	}

	server, _, _ := testAgentTaskServerWithMemory(t, ledger, project, owner, sessionID)
	server.orchestratorAPIKey = "u3-default-key"
	resultID := "result-" + taskID
	workspaceRef := "workspace-ref-" + taskID
	payload := fencePayload(fence)
	payload["publication_id"] = "publication-" + taskID
	payload["runner_exit_required"] = true
	payload["result"] = map[string]any{
		"result_id": resultID, "summary": "default publication identity", "output": "verified output",
		"context_pack_hash": anyToString(anyMap(claim["attempt"])["context_pack_hash"]),
		"workspace":         map[string]any{"workspace_ref": workspaceRef},
		"cleanup":           map[string]any{"cleanup_id": agentTaskCleanupID(fence.TaskID, fence.AttemptID, workspaceRef)},
	}
	if _, exists := payload["idempotency_key"]; exists {
		t.Fatal("omitted-idempotency route fixture unexpectedly supplied a top-level key")
	}
	if _, exists := anyMap(payload["result"])["idempotency_key"]; exists {
		t.Fatal("omitted-idempotency route fixture unexpectedly supplied a result key")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	publishRequest := httptest.NewRequest(http.MethodPost, "/agents/tasks/"+taskID+"/publish", bytes.NewReader(body))
	publishRequest.Header.Set("Content-Type", "application/json")
	publishRequest.Header.Set("X-Api-Key", "u3-default-key")
	publishResponse := httptest.NewRecorder()
	server.agentsTasksRoute(publishResponse, publishRequest)
	published := map[string]any{}
	if err := json.Unmarshal(publishResponse.Body.Bytes(), &published); err != nil {
		t.Fatalf("decode omitted-idempotency publish response: %v (%s)", err, publishResponse.Body.String())
	}
	if publishResponse.Code != http.StatusOK {
		t.Fatalf("omitted-idempotency publish status=%d response=%#v", publishResponse.Code, published)
	}
	expectedKey := "task-result:" + resultID
	if anyToString(published["idempotency_key"]) != expectedKey {
		t.Fatalf("omitted publication idempotency default=%q want %q", anyToString(published["idempotency_key"]), expectedKey)
	}
	if err := verifyAgentTaskPublicationReceipt(anyMap(published["publication_receipt"])); err != nil {
		t.Fatalf("defaulted publication returned an invalid receipt: %v", err)
	}
	if err := verifyAgentTaskCleanupAuthorization(anyMap(published["cleanup_authorization"])); err != nil {
		t.Fatalf("defaulted publication returned an invalid cleanup authorization: %v", err)
	}

	if err := ledger.close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatalf("restart omitted-idempotency ledger: %v", err)
	}
	t.Cleanup(func() { _ = restarted.close() })
	restartedServer, _, _ := testAgentTaskServerWithMemory(t, restarted, project, owner, sessionID)
	restartedServer.orchestratorAPIKey = "u3-default-restart-key"
	query := url.Values{
		"lease_id": []string{fence.LeaseID}, "generation": []string{fmt.Sprintf("%d", fence.Generation)},
		"assignment_generation": []string{fmt.Sprintf("%d", fence.Generation)}, "lease_generation": []string{fmt.Sprintf("%d", fence.Generation)},
		"worker_id": []string{fence.WorkerID}, "worker_instance_id": []string{fence.WorkerInstanceID},
		"idempotency_key": []string{expectedKey},
	}
	reconcileRequest := httptest.NewRequest(http.MethodGet, "/agents/tasks/"+taskID+"/attempts/"+fence.AttemptID+"/publication?"+query.Encode(), nil)
	reconcileRequest.Header.Set("X-Api-Key", "u3-default-restart-key")
	reconcileResponse := httptest.NewRecorder()
	restartedServer.agentsTasksRoute(reconcileResponse, reconcileRequest)
	reconciled := map[string]any{}
	if err := json.Unmarshal(reconcileResponse.Body.Bytes(), &reconciled); err != nil {
		t.Fatalf("decode omitted-idempotency reconciliation response: %v (%s)", err, reconcileResponse.Body.String())
	}
	if reconcileResponse.Code != http.StatusOK {
		t.Fatalf("omitted-idempotency reconciliation status=%d response=%#v", reconcileResponse.Code, reconciled)
	}
	assertAgentTaskPublicationReconciliationFields(t, reconciled)
	if anyToString(reconciled["idempotency_key"]) != expectedKey {
		t.Fatalf("restart reconciliation idempotency=%q want %q", anyToString(reconciled["idempotency_key"]), expectedKey)
	}
	if !reflect.DeepEqual(anyMap(reconciled["publication_receipt"]), anyMap(published["publication_receipt"])) || !reflect.DeepEqual(anyMap(reconciled["cleanup_authorization"]), anyMap(published["cleanup_authorization"])) {
		t.Fatalf("restart reconciliation changed defaulted publication receipts: published=%#v reconciled=%#v", published, reconciled)
	}
	assertAgentTaskPublicationReconciliationPythonConsumer(t, reconciled)
}

func TestAgentTaskU3RestartReconciliationReadsExactAttemptPublication(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	taskID, project, owner, sessionID := "u3-restart-contract", "u3-restart-project", "u3-restart-owner", "sess_u3_restart"
	fence, published := testAgentTaskStagePublication(t, ledger, taskID, project, owner, sessionID, nil)
	idempotencyKey := anyToString(published["idempotency_key"])
	if err := ledger.close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatalf("restart task ledger: %v", err)
	}
	t.Cleanup(func() { _ = restarted.close() })
	server, _, _ := testAgentTaskServerWithMemory(t, restarted, project, owner, sessionID)
	server.orchestratorAPIKey = "u3-restart-key"
	query := url.Values{
		"lease_id":              []string{fence.LeaseID},
		"generation":            []string{fmt.Sprintf("%d", fence.Generation)},
		"assignment_generation": []string{fmt.Sprintf("%d", fence.Generation)},
		"lease_generation":      []string{fmt.Sprintf("%d", fence.Generation)},
		"worker_id":             []string{fence.WorkerID},
		"worker_instance_id":    []string{fence.WorkerInstanceID},
		"idempotency_key":       []string{idempotencyKey},
	}
	direct, directErr := restarted.publicationForExactFence(context.Background(), fence, idempotencyKey)
	if directErr != nil {
		t.Fatalf("direct exact-fence reconciliation: %v", directErr)
	}
	assertAgentTaskPublicationReconciliationFields(t, direct)
	get := func(path string, values url.Values) (int, map[string]any) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, path+"?"+values.Encode(), nil)
		request.Header.Set("X-Api-Key", "u3-restart-key")
		response := httptest.NewRecorder()
		server.agentsTasksRoute(response, request)
		decoded := map[string]any{}
		if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("decode reconciliation response: %v (%s)", err, response.Body.String())
		}
		return response.Code, decoded
	}
	path := "/agents/tasks/" + taskID + "/attempts/" + fence.AttemptID + "/publication"
	status, reconciled := get(path, query)
	if status != http.StatusOK {
		t.Fatalf("restart reconciliation status=%d response=%#v", status, reconciled)
	}
	assertAgentTaskPublicationReconciliationFields(t, reconciled)
	expectedJSON, err := json.Marshal(published)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]any{}
	if err := json.Unmarshal(expectedJSON, &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(anyMap(reconciled["publication_receipt"]), anyMap(expected["publication_receipt"])) || !reflect.DeepEqual(anyMap(reconciled["cleanup_authorization"]), anyMap(expected["cleanup_authorization"])) {
		t.Fatalf("restart surface changed publish receipts: published=%#v reconciled=%#v", expected, reconciled)
	}
	for field, expectedValue := range map[string]string{
		"task_id": fence.TaskID, "attempt_id": fence.AttemptID, "lease_id": fence.LeaseID,
		"worker_id": fence.WorkerID, "worker_instance_id": fence.WorkerInstanceID, "idempotency_key": idempotencyKey,
	} {
		if anyToString(reconciled[field]) != expectedValue {
			t.Fatalf("restart reconciliation %s=%#v want %q", field, reconciled[field], expectedValue)
		}
	}
	foreignFence := cloneURLValues(query)
	foreignFence.Set("lease_id", "foreign-lease")
	if status, _ := get(path, foreignFence); status != http.StatusConflict {
		t.Fatalf("foreign restart fence status=%d, want %d", status, http.StatusConflict)
	}
	foreignKey := cloneURLValues(query)
	foreignKey.Set("idempotency_key", "foreign-idempotency")
	if status, _ := get(path, foreignKey); status != http.StatusConflict {
		t.Fatalf("foreign restart idempotency status=%d, want %d", status, http.StatusConflict)
	}
	if status, _ := get("/agents/tasks/"+taskID+"/attempts/foreign-attempt/publication", query); status != http.StatusNotFound {
		t.Fatalf("foreign restart attempt status=%d, want %d", status, http.StatusNotFound)
	}
	if status, _ := get("/agents/tasks/foreign-task/attempts/"+fence.AttemptID+"/publication", query); status != http.StatusNotFound {
		t.Fatalf("foreign restart task status=%d, want %d", status, http.StatusNotFound)
	}
	foreignPrincipal := agentTaskRouteAuth{Principal: "foreign-principal", Role: "worker", Workspace: "workspace-" + project, Signed: true}
	if err := server.authorizeTaskResource(context.Background(), taskID, foreignPrincipal); err == nil {
		t.Fatal("foreign signed principal was authorized for restart publication")
	}
}

func TestAgentTaskU3PublicationReconciliationCrossLanguageFixture(t *testing.T) {
	normalize := func(value map[string]any) map[string]any {
		encoded, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		decoded := map[string]any{}
		if unmarshalErr := json.Unmarshal(encoded, &decoded); unmarshalErr != nil {
			t.Fatal(unmarshalErr)
		}
		return decoded
	}
	fixturePattern := filepath.Join("..", "..", "config", "agent_contracts", "fixtures", "agent_task_publication_reconciliation.v1*.json")
	fixturePaths, err := filepath.Glob(fixturePattern)
	if err != nil || len(fixturePaths) != 4 {
		t.Fatalf("resolve shared publication reconciliation fixtures: paths=%v err=%v", fixturePaths, err)
	}
	wantMatrix := map[string]string{"writeback_pending": "pending", "writeback_failed": "failed", "committed": "committed", "dead_letter": "dead_letter"}
	seen := map[string]bool{}
	for _, fixturePath := range fixturePaths {
		raw, readErr := os.ReadFile(fixturePath)
		if readErr != nil {
			t.Fatalf("read shared publication reconciliation fixture %s: %v", fixturePath, readErr)
		}
		fixture := map[string]any{}
		if decodeErr := json.Unmarshal(raw, &fixture); decodeErr != nil {
			t.Fatalf("decode shared publication reconciliation fixture %s: %v", fixturePath, decodeErr)
		}
		assertAgentTaskPublicationReconciliationFields(t, fixture)
		status := anyToString(fixture["status"])
		if wantMatrix[status] == "" || anyToString(fixture["writeback_status"]) != wantMatrix[status] || seen[status] {
			t.Fatalf("shared publication reconciliation state matrix is incomplete or duplicated: fixture=%s payload=%#v", fixturePath, fixture)
		}
		seen[status] = true
		if anyToString(fixture["idempotency_key"]) != "task-result:"+anyToString(fixture["result_id"]) {
			t.Fatalf("shared fixture does not use the U3 task-result idempotency convention: %s", fixturePath)
		}
		if err := verifyAgentTaskPublicationReceipt(anyMap(fixture["publication_receipt"])); err != nil {
			t.Fatalf("shared publication receipt fixture %s is invalid: %v", fixturePath, err)
		}
		if anyToString(anyMap(fixture["publication_receipt"])["state"]) != "staged" {
			t.Fatalf("shared publication receipt fixture %s mutated its immutable staged state", fixturePath)
		}
		if err := verifyAgentTaskCleanupAuthorization(anyMap(fixture["cleanup_authorization"])); err != nil {
			t.Fatalf("shared cleanup authorization fixture %s is invalid: %v", fixturePath, err)
		}
		authorization := anyMap(fixture["cleanup_authorization"])
		receipt, expectedAuthorization, boundaryErr := agentTaskPublicationBoundary(agentTaskPublicationBoundaryIdentity{
			PublicationID: anyToString(fixture["publication_id"]), ResultID: anyToString(fixture["result_id"]),
			ResultDigest: "sha256:" + fmt.Sprintf("%064d", 0), PublicationStatus: status,
			TaskID: anyToString(fixture["task_id"]), AttemptID: anyToString(fixture["attempt_id"]), LeaseID: anyToString(fixture["lease_id"]),
			WorkerID: anyToString(fixture["worker_id"]), WorkerInstanceID: anyToString(fixture["worker_instance_id"]),
			IdempotencyKey: anyToString(fixture["idempotency_key"]), WorkspaceRef: anyToString(authorization["workspace_ref"]),
			CleanupID: anyToString(authorization["cleanup_id"]), AssignmentGeneration: anyToInt(fixture["assignment_generation"], 0),
			LeaseGeneration: anyToInt(fixture["lease_generation"], 0),
		})
		if boundaryErr != nil {
			t.Fatalf("rebuild shared publication reconciliation boundary %s: %v", fixturePath, boundaryErr)
		}
		if !reflect.DeepEqual(normalize(receipt), anyMap(fixture["publication_receipt"])) || !reflect.DeepEqual(normalize(expectedAuthorization), authorization) {
			t.Fatalf("shared fixture %s drifted from the Gateway boundary constructor: fixture=%#v receipt=%#v authorization=%#v", fixturePath, fixture, receipt, expectedAuthorization)
		}
	}
	if len(seen) != len(wantMatrix) {
		t.Fatalf("shared publication reconciliation fixtures do not cover the full state matrix: seen=%v", seen)
	}
}

func TestAgentTaskU3PublicationReconciliationRouteStateMatrix(t *testing.T) {
	wantMatrix := map[string]string{"writeback_pending": "pending", "writeback_failed": "failed", "committed": "committed", "dead_letter": "dead_letter"}
	for targetStatus, targetWritebackStatus := range wantMatrix {
		t.Run(targetStatus, func(t *testing.T) {
			ledger := testAgentTaskLedger(t)
			taskID := "u3-state-" + strings.ReplaceAll(targetStatus, "_", "-")
			project, owner, sessionID := taskID+"-project", taskID+"-owner", "sess_"+strings.ReplaceAll(targetStatus, "_", "")
			fence, staged := testAgentTaskStagePublication(t, ledger, taskID, project, owner, sessionID, nil)
			publicationID := anyToString(staged["publication_id"])
			switch targetStatus {
			case "writeback_failed":
				if _, err := ledger.finalizePublication(context.Background(), publicationID, "failed", "", "fixture writeback failure"); err != nil {
					t.Fatalf("move fixture publication to writeback_failed: %v", err)
				}
			case "committed":
				if _, err := ledger.finalizePublication(context.Background(), publicationID, "committed", "writeback-fixture", ""); err != nil {
					t.Fatalf("move fixture publication to committed: %v", err)
				}
			case "dead_letter":
				for attempt := 0; attempt < agentTaskDeliveryMaxAttempts; attempt++ {
					claimedPublication, claimed, err := ledger.claimPublication(context.Background(), publicationID, "u3-state-worker")
					if err != nil || !claimed {
						t.Fatalf("claim fixture publication retry %d: claimed=%v row=%#v err=%v", attempt, claimed, claimedPublication, err)
					}
					if _, err := ledger.finalizePublicationClaim(context.Background(), publicationID, anyToString(claimedPublication["worker_claim_id"]), "failed", "", "fixture writeback failure"); err != nil {
						t.Fatalf("fail fixture publication retry %d: %v", attempt, err)
					}
				}
				deadLetter, claimed, err := ledger.claimPublication(context.Background(), publicationID, "u3-state-worker")
				if err != nil || claimed || anyToString(deadLetter["status"]) != "dead_letter" {
					t.Fatalf("exhaust fixture publication retries: claimed=%v row=%#v err=%v", claimed, deadLetter, err)
				}
			}

			server, _, _ := testAgentTaskServerWithMemory(t, ledger, project, owner, sessionID)
			server.orchestratorAPIKey = "u3-state-key"
			query := url.Values{
				"lease_id": []string{fence.LeaseID}, "generation": []string{fmt.Sprintf("%d", fence.Generation)},
				"assignment_generation": []string{fmt.Sprintf("%d", fence.Generation)}, "lease_generation": []string{fmt.Sprintf("%d", fence.Generation)},
				"worker_id": []string{fence.WorkerID}, "worker_instance_id": []string{fence.WorkerInstanceID},
				"idempotency_key": []string{anyToString(staged["idempotency_key"])},
			}
			path := "/agents/tasks/" + taskID + "/attempts/" + fence.AttemptID + "/publication?" + query.Encode()
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.Header.Set("X-Api-Key", "u3-state-key")
			response := httptest.NewRecorder()
			server.agentsTasksRoute(response, request)
			payload := map[string]any{}
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode route-generated %s fixture: %v (%s)", targetStatus, err, response.Body.String())
			}
			if response.Code != http.StatusOK {
				t.Fatalf("route-generated %s fixture status=%d payload=%#v", targetStatus, response.Code, payload)
			}
			assertAgentTaskPublicationReconciliationFields(t, payload)
			if anyToString(payload["status"]) != targetStatus || anyToString(payload["writeback_status"]) != targetWritebackStatus {
				t.Fatalf("route-generated fixture left canonical state matrix: %#v", payload)
			}
			if anyToString(anyMap(payload["publication_receipt"])["state"]) != "staged" {
				t.Fatalf("route-generated %s fixture mutated its immutable staged receipt: %#v", targetStatus, payload)
			}
			expectedReceiptJSON, err := json.Marshal(staged["publication_receipt"])
			if err != nil {
				t.Fatal(err)
			}
			expectedReceipt := map[string]any{}
			if err := json.Unmarshal(expectedReceiptJSON, &expectedReceipt); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(anyMap(payload["publication_receipt"]), expectedReceipt) {
				t.Fatalf("route-generated %s fixture changed the publish receipt: staged=%#v reconciled=%#v", targetStatus, staged, payload)
			}
			t.Logf("route_fixture schema=%s fields=%d status=%s writeback_status=%s publication_receipt.state=%s", anyToString(payload["schema_id"]), len(payload), targetStatus, targetWritebackStatus, anyToString(anyMap(payload["publication_receipt"])["state"]))
		})
	}
}

func cloneURLValues(input url.Values) url.Values {
	output := make(url.Values, len(input))
	for key, values := range input {
		output[key] = append([]string{}, values...)
	}
	return output
}

func testAgentTaskCleanupRequest(fence agentTaskFence, receipt map[string]any) map[string]any {
	payload := fencePayload(fence)
	payload["fence"] = fencePayload(fence)
	payload["cleanup_receipt"] = receipt
	return payload
}

func TestAgentTaskU3PublicationAndCleanupCompatibilityContract(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	taskID, project, owner, sessionID := "u3-cleanup-contract", "u3-project", "u3-reviewer", "sess_u3_cleanup"
	fence, publication := testAgentTaskStagePublication(t, ledger, taskID, project, owner, sessionID, nil)

	for field, expected := range map[string]string{
		"task_id": fence.TaskID, "attempt_id": fence.AttemptID, "lease_id": fence.LeaseID,
		"worker_id": fence.WorkerID, "worker_instance_id": fence.WorkerInstanceID,
		"idempotency_key": "task-result:result-" + taskID,
	} {
		if anyToString(publication[field]) != expected {
			t.Fatalf("publication %s=%#v, want %#v", field, publication[field], expected)
		}
	}
	for _, field := range []string{"generation", "assignment_generation", "lease_generation"} {
		if anyToInt(publication[field], 0) != fence.Generation {
			t.Fatalf("publication %s=%#v, want %d", field, publication[field], fence.Generation)
		}
	}
	publicationReceipt := anyMap(publication["publication_receipt"])
	if err := verifyAgentTaskPublicationReceipt(publicationReceipt); err != nil {
		t.Fatalf("publication receipt is not the closed U3 contract: %v (%#v)", err, publicationReceipt)
	}
	authorization := anyMap(publication["cleanup_authorization"])
	if err := verifyAgentTaskCleanupAuthorization(authorization); err != nil {
		t.Fatalf("cleanup authorization is not the closed U3 contract: %v (%#v)", err, authorization)
	}
	if !anyToBool(authorization["attempt_terminal"]) || anyToInt(authorization["generation"], 0) != fence.Generation {
		t.Fatalf("cleanup authorization lost terminal fence: %#v", authorization)
	}

	server, _, _ := testAgentTaskServerWithMemory(t, ledger, project, owner, sessionID)
	server.orchestratorAPIKey = "u3-cleanup-key"
	post := func(path string, payload map[string]any) (int, map[string]any) {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Api-Key", "u3-cleanup-key")
		response := httptest.NewRecorder()
		server.agentsTasksRoute(response, request)
		decoded := map[string]any{}
		if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("decode cleanup response %d: %v (%s)", response.Code, err, response.Body.String())
		}
		return response.Code, decoded
	}

	cleanupReceipt := testAgentTaskCleanupReceipt(publication, fence)
	tampered := cloneAnyMap(cleanupReceipt)
	tampered["workspace_ref"] = "workspace-foreign"
	tampered["receipt_digest"] = agentTaskDigestExcluding(tampered, "receipt_digest")
	if status, _ := post("/agents/tasks/"+taskID+"/cleanup", testAgentTaskCleanupRequest(fence, tampered)); status != http.StatusUnprocessableEntity {
		t.Fatalf("foreign digest-valid cleanup receipt status=%d, want %d", status, http.StatusUnprocessableEntity)
	}
	var rows int
	if err := ledger.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM task_ledger_cleanup_receipts`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rejected cleanup receipt left durable residue: rows=%d err=%v", rows, err)
	}

	status, recorded := post("/agents/tasks/"+taskID+"/cleanup", testAgentTaskCleanupRequest(fence, cleanupReceipt))
	if status != http.StatusOK {
		t.Fatalf("cleanup POST status=%d body=%#v", status, recorded)
	}
	for field, expected := range cleanupReceipt {
		if field == "generation" {
			if anyToInt(recorded[field], 0) != anyToInt(expected, 0) {
				t.Fatalf("cleanup acknowledgement changed exact receipt field %s: got=%#v want=%#v", field, recorded[field], expected)
			}
			continue
		}
		if recorded[field] != expected {
			t.Fatalf("cleanup acknowledgement changed exact receipt field %s: got=%#v want=%#v", field, recorded[field], expected)
		}
	}
	if !anyToBool(recorded["recorded"]) || !anyToBool(recorded["durable"]) || !anyToBool(recorded["acknowledged"]) {
		t.Fatalf("cleanup acknowledgement lacks durable flags: %#v", recorded)
	}
	if err := verifyAgentTaskCleanupReceipt(recorded); err != nil {
		t.Fatalf("recorded cleanup receipt is invalid: %v", err)
	}
	if err := ledger.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM task_ledger_cleanup_receipts`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("cleanup receipt persistence rows=%d err=%v", rows, err)
	}

	status, replay := post("/agents/tasks/"+taskID+"/attempts/"+fence.AttemptID+"/cleanup", testAgentTaskCleanupRequest(fence, cleanupReceipt))
	if status != http.StatusOK || !reflect.DeepEqual(recorded, replay) {
		t.Fatalf("cleanup replay changed durable acknowledgement: status=%d first=%#v replay=%#v", status, recorded, replay)
	}
	if err := ledger.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM task_ledger_cleanup_receipts`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("cleanup replay duplicated persistence: rows=%d err=%v", rows, err)
	}
}
