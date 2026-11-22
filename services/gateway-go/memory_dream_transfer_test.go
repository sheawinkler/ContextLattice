package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func newDreamTransferTestServer(t *testing.T) (*server, contextPassport) {
	t.Helper()
	root := t.TempDir()
	passportStore := newTestPassportStore(t, filepath.Join(root, "passport"))
	meshStore := newTestMeshStore(t, filepath.Join(root, "mesh"), passportStore)
	passport := signedTestPassport(t, passportStore, "home-project", "home-lineage", 1, nil, "home-experience")
	passport.Claims = []map[string]any{{
		"portable_id": "exp:cache-key",
		"kind":        "conditional_experience", "status": "confirmed",
		"mechanism": "cache by stable key", "observed_result": "Repeated lookup avoided a duplicate fetch.",
		"evidence_refs": []any{"evidence:home-result"},
	}}
	passport.Evidence = []map[string]any{{"portable_id": "evidence:home-result", "content_ref": "sha256:" + strings.Repeat("a", 64)}}
	if err := signContextPassport(&passport, passportStore.identity); err != nil {
		t.Fatalf("sign source experience passport: %v", err)
	}
	if _, err := passportStore.record(passport); err != nil {
		t.Fatalf("record source experience passport: %v", err)
	}
	envelope := contextMeshEnvelope{
		EnvelopeID: "env_imported_home_experience",
		Sender:     contextMeshSender{SigningKeyID: passport.Issuer.SigningKeyID},
	}
	if err := meshStore.recordReceipt(envelope, passport, passportStore.identity.MeshKeyID,
		contextPassportReconciliation{Action: "record_root", PassportID: passport.PassportID, Recorded: true}); err != nil {
		t.Fatalf("record successful import receipt: %v", err)
	}

	memoryRoot := filepath.Join(root, "memory")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ROOT", memoryRoot)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", filepath.Join(memoryRoot, "_contextlattice", "memory_write_history.ndjson"))
	t.Setenv("GO_MEMORY_STORE_ACCESS_LOG_PATH", filepath.Join(memoryRoot, "_contextlattice", "memory_access_log.ndjson"))
	t.Setenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH", filepath.Join(memoryRoot, "_contextlattice", "objects"))
	t.Setenv("GO_WRITE_QDRANT_FANOUT_MODE", "disabled")
	t.Setenv("GO_WRITE_PGVECTOR_FANOUT_MODE", "disabled")
	memoryStore, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("create destination memory ledger: %v", err)
	}
	s := &server{
		memoryStore: memoryStore, writePolicy: loadWriteIngressPolicy(), strictNoPythonRuntime: true,
		contextPassports: passportStore, contextMesh: meshStore,
		orchestratorAPIKey: "dream-shared-service-key",
		toolCalls: toolCallPolicy{
			allowAll: true, roleSplitEnabled: true, workerKey: "dream-scoped-worker-key",
			orchestratorRole: toolRolePolicy{allowAll: true}, workerRole: toolRolePolicy{allowAll: true},
		},
	}
	return s, passport
}

func dreamTransferAuthorizedContext(s *server) context.Context {
	request := httptest.NewRequest(http.MethodPost, "/memory/dream", nil)
	request.Header.Set("X-Api-Key", s.toolCalls.workerKey)
	request.Header.Set(dreamTransferProofHeader, strings.Repeat("a", 64))
	return s.dreamTransferContext(context.Background(), request)
}

func dreamTransferProvisionTestGrant(t *testing.T, s *server, passport contextPassport, destination string) string {
	return dreamTransferProvisionTestGrantUntil(t, s, passport, destination, time.Now().UTC().Add(10*time.Minute))
}

func dreamTransferProvisionTestGrantUntil(t *testing.T, s *server, passport contextPassport, destination string, expires time.Time) string {
	t.Helper()
	_, sourceHash, ok := dreamTransferConfirmedSourceClaim(passport, "exp:cache-key")
	if !ok {
		t.Fatal("test source experience is not authoritative")
	}
	identity, ok := dreamTransferIdentityFromContext(dreamTransferAuthorizedContext(s))
	if !ok {
		t.Fatal("test worker identity was not recognized")
	}
	grantID := "dgrant_" + randomHex(16)
	receipt := dreamTransferReceipt{
		ReceiptID: "", Kind: "grant", GrantID: grantID,
		SourceProject: passport.Project, SourcePassportID: passport.PassportID,
		ExperienceRef: "exp:cache-key", SourceRecordHash: sourceHash,
		DestinationProject: destination, CallerPrincipal: identity.principal,
		ExpiresAt: expires.UTC().Format(time.RFC3339Nano),
	}
	issued := time.Now().UTC()
	dreamTransferWriteOwnerTestGrant(t, s, receipt, issued)
	if expires.After(issued) && !s.loadDreamTransferOwnerGrant(dreamTransferAuthorizedContext(s), grantID) {
		t.Fatal("load owner-file grant into the signed mesh ledger")
	}
	return grantID
}

func dreamTransferPostMemory(t *testing.T, s *server, payload map[string]any) (map[string]any, int) {
	return dreamTransferPostMemoryWithProof(t, s, payload, []string{strings.Repeat("a", 64)})
}

func dreamTransferPostMemoryWithProof(t *testing.T, s *server, payload map[string]any, proofs []string) (map[string]any, int) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/memory/dream", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", s.toolCalls.workerKey)
	for _, proof := range proofs {
		request.Header.Add(dreamTransferProofHeader, proof)
	}
	recorder := httptest.NewRecorder()
	s.memoryDream(recorder, request)
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode /memory/dream response: %v body=%s", err, recorder.Body.String())
	}
	return response, recorder.Code
}

func TestDreamTransferMemoryRouteRequiresExactWorkerProofAndTransferPayload(t *testing.T) {
	s, passport := newDreamTransferTestServer(t)
	grantID := dreamTransferProvisionTestGrant(t, s, passport, "destination-project")
	freeze := dreamTransferFreezeInput(passport, grantID, time.Now().UTC().Add(5*time.Minute))
	freeze["use_llm"], freeze["persist"] = false, false

	t.Run("missing proof", func(t *testing.T) {
		response, status := dreamTransferPostMemoryWithProof(t, s, freeze, nil)
		if status != http.StatusForbidden || anyToString(response["error"]) != "forbidden" || len(response) != 2 {
			t.Fatalf("missing proof did not receive opaque denial: status=%d response=%#v", status, response)
		}
		baseline, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(freeze)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/memory/dream", bytes.NewReader(body))
		request.Header.Set("X-Api-Key", s.toolCalls.workerKey)
		request.Header.Set("X-ContextLattice-Trusted-Caller-Role", "owner")
		request.Header.Set("X-ContextLattice-Trusted-Caller-Principal", "source-owner")
		request.Header.Set("X-ContextLattice-Caller-Role", "orchestrator")
		recorder := httptest.NewRecorder()
		s.memoryDream(recorder, request)
		if recorder.Code != status || strings.TrimSpace(recorder.Body.String()) != string(baseline) {
			t.Fatalf("spurious authority headers changed opaque denial: status=%d body=%s baseline=%s", recorder.Code, recorder.Body.String(), baseline)
		}
	})
	t.Run("duplicate proof", func(t *testing.T) {
		response, status := dreamTransferPostMemoryWithProof(t, s, freeze, []string{strings.Repeat("a", 64), strings.Repeat("b", 64)})
		if status != http.StatusForbidden || anyToString(response["error"]) != "forbidden" || len(response) != 2 {
			t.Fatalf("duplicate proof did not receive opaque denial: status=%d response=%#v", status, response)
		}
	})
	t.Run("ordinary Dream remains denied", func(t *testing.T) {
		ordinary := map[string]any{"project": "destination-project", "goal": "ordinary worker request"}
		response, status := dreamTransferPostMemory(t, s, ordinary)
		if status != http.StatusForbidden || anyToString(response["error"]) != "forbidden" || len(response) != 2 {
			t.Fatalf("worker proof expanded ordinary Dream access: status=%d response=%#v", status, response)
		}
	})
	t.Run("optional flags are false only", func(t *testing.T) {
		for _, bad := range []any{true, "false", 0} {
			payload := map[string]any{"project": "destination-project", "experience_transfer": anyMap(freeze["experience_transfer"]), "persist": bad}
			response, status := dreamTransferPostMemory(t, s, payload)
			if status != http.StatusForbidden || anyToString(response["error"]) != "forbidden" || len(response) != 2 {
				t.Fatalf("non-false persist flag was accepted: value=%#v status=%d response=%#v", bad, status, response)
			}
		}
	})
	t.Run("extra envelope field denied", func(t *testing.T) {
		payload := map[string]any{"project": "destination-project", "experience_transfer": anyMap(freeze["experience_transfer"]), "caller": "owner"}
		response, status := dreamTransferPostMemory(t, s, payload)
		if status != http.StatusForbidden || anyToString(response["error"]) != "forbidden" || len(response) != 2 {
			t.Fatalf("extra envelope field was accepted: status=%d response=%#v", status, response)
		}
	})
}

func TestDreamTransferToolsRouteStillUsesWorkerAllowlist(t *testing.T) {
	s, _ := newDreamTransferTestServer(t)
	s.toolCalls.workerRole.allowAll = false
	s.toolCalls.workerRole.allowlist = nil
	request := httptest.NewRequest(http.MethodPost, "/tools/dream", strings.NewReader(`{"project":"destination-project","experience_transfer":{"action":"grant"}}`))
	request.Header.Set("X-Api-Key", s.toolCalls.workerKey)
	request.Header.Set(dreamTransferProofHeader, strings.Repeat("a", 64))
	recorder := httptest.NewRecorder()
	s.toolsDream(recorder, request)
	if recorder.Code != http.StatusForbidden || strings.Contains(recorder.Body.String(), "transfer_unavailable") {
		t.Fatalf("worker bypassed /tools/dream allowlist: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestDreamTransferFreezeReportsDurableLedgerWhenProjectionFails(t *testing.T) {
	s, passport := newDreamTransferTestServer(t)
	grantID := dreamTransferProvisionTestGrant(t, s, passport, "destination-project")
	blockedBlobRoot := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedBlobRoot, []byte("block"), 0600); err != nil {
		t.Fatal(err)
	}
	s.memoryStore.policy.contentBlobsPath = blockedBlobRoot
	response, status := dreamTransferPostMemory(t, s, dreamTransferFreezeInput(passport, grantID, time.Now().UTC().Add(5*time.Minute)))
	transfer := dreamTransferFromResponse(t, response)
	if status != http.StatusServiceUnavailable || anyToBool(response["ok"]) || anyToBool(response["persisted"]) ||
		!anyToBool(transfer["ledger_recorded"]) || anyToBool(transfer["projection_persisted"]) ||
		anyToString(transfer["status"]) != "frozen" {
		t.Fatalf("projection failure was reported as full success: status=%d response=%#v", status, response)
	}
}

func dreamTransferFreezeInput(passport contextPassport, grantID string, expires time.Time) map[string]any {
	return map[string]any{
		"project": "destination-project",
		"experience_transfer": map[string]any{
			"action": "freeze", "grant_id": grantID, "source_passport_id": passport.PassportID,
			"experience_ref": "exp:cache-key", "destination_project": "destination-project",
			"mechanism": "cache by stable key", "mismatch": "destination key includes tenant scope",
			"prediction": "Tenant-scoped keys will avoid duplicate fetches without cross-tenant cache hits.",
			"falsifier":  "Any observed cross-tenant cache hit or no reduction in duplicate fetches.",
			"expires_at": expires.Format(time.RFC3339Nano),
		},
	}
}

func dreamTransferFromResponse(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	transfer, ok := response["dream_experience_transfer"].(map[string]any)
	if !ok {
		t.Fatalf("dream experience transfer missing: %#v", response)
	}
	return transfer
}

func TestDreamTransferFreezesPredictionThenRecordsDestinationResult(t *testing.T) {
	s, passport := newDreamTransferTestServer(t)
	grantID := dreamTransferProvisionTestGrant(t, s, passport, "destination-project")
	payload := dreamTransferFreezeInput(passport, grantID, time.Now().UTC().Add(5*time.Minute))
	payload["use_llm"], payload["persist"] = false, false
	response, status := dreamTransferPostMemory(t, s, payload)
	experiments, _ := response["experiments"].([]any)
	if status != http.StatusOK || anyToString(response["mode"]) != "dream_transfer" || len(experiments) != 0 {
		t.Fatalf("freeze did not use the manual Dream transfer path: status=%d response=%#v", status, response)
	}
	frozen := dreamTransferFromResponse(t, response)
	freezeDigest := dreamTransferRequestDigest("destination-project", anyMap(payload["experience_transfer"]))
	if anyToString(response["action"]) != "freeze" || anyToString(response["request_digest"]) != freezeDigest ||
		anyToString(frozen["action"]) != "freeze" || anyToString(frozen["request_digest"]) != freezeDigest {
		t.Fatalf("freeze response did not bind the exact request: response=%#v", response)
	}
	if anyToString(frozen["status"]) != "frozen" || !anyToBool(frozen["prediction_frozen"]) || anyToBool(frozen["result_received"]) {
		t.Fatalf("prediction was not frozen before result: %#v", frozen)
	}
	if anyToString(frozen["prediction"]) != anyToString(anyMap(payload["experience_transfer"])["prediction"]) ||
		anyToString(frozen["falsifier"]) != anyToString(anyMap(payload["experience_transfer"])["falsifier"]) {
		t.Fatalf("destination did not receive its frozen prediction and falsifier: %#v", frozen)
	}
	transferID := anyToString(frozen["transfer_id"])
	if transferID == "" {
		t.Fatalf("frozen transfer lacks id: %#v", frozen)
	}
	if rows := s.memoryStore.recentItems("home-project", "", 10, 0); len(rows) != 0 {
		t.Fatalf("transfer wrote into the home project: %#v", rows)
	}

	if _, _, err := s.memoryStore.put(normalizedWrite{
		project: "destination-project", fileName: "evidence/result.json", topicPath: "evidence",
		content: `{"tenant_scoped":true,"duplicate_fetches":0}`,
	}); err != nil {
		t.Fatalf("write destination test evidence: %v", err)
	}
	resultPayload := map[string]any{
		"project": "destination-project",
		"use_llm": false, "persist": false,
		"experience_transfer": map[string]any{
			"action": "record_result", "transfer_id": transferID,
			"result": "confirmed", "evidence_refs": []any{"destination-project::evidence/result.json"},
		},
	}
	resultResponse, resultStatus := dreamTransferPostMemory(t, s, resultPayload)
	result := dreamTransferFromResponse(t, resultResponse)
	resultDigest := dreamTransferRequestDigest("destination-project", anyMap(resultPayload["experience_transfer"]))
	if anyToString(resultResponse["action"]) != "record_result" || anyToString(resultResponse["request_digest"]) != resultDigest ||
		anyToString(result["action"]) != "record_result" || anyToString(result["request_digest"]) != resultDigest {
		t.Fatalf("result response did not bind the exact request: response=%#v", resultResponse)
	}
	if resultStatus != http.StatusOK || anyToString(result["status"]) != "confirmed" || anyToString(result["home_project_effect"]) != "unchanged" {
		t.Fatalf("destination result was not recorded safely: status=%d transfer=%#v", resultStatus, result)
	}
	record, _, ok := s.readDreamExperienceTransfer(context.Background(), "destination-project", transferID)
	if !ok || record.Status != "confirmed" || record.Prediction == "" || record.Falsifier == "" || record.HomeProjectEffect != "unchanged" {
		t.Fatalf("final destination record lost frozen fields or home boundary: %+v", record)
	}
}

func TestDreamTransferAbstainsWithoutImportedAuthoritativeSource(t *testing.T) {
	s, passport := newDreamTransferTestServer(t)
	ctx := dreamTransferAuthorizedContext(s)
	grantID := dreamTransferProvisionTestGrant(t, s, passport, "destination-project")
	s.contextMesh.mu.Lock()
	s.contextMesh.receipts = nil
	s.contextMesh.mu.Unlock()
	payload := dreamTransferFreezeInput(passport, grantID, time.Now().UTC().Add(5*time.Minute))
	response, status := s.buildDreamModeResponse(ctx, make(http.Header), payload, "/memory/dream")
	transfer := dreamTransferFromResponse(t, response)
	if status != http.StatusNotFound || anyToString(transfer["status"]) != "abstained" || anyToString(transfer["reason"]) != "transfer_unavailable" {
		t.Fatalf("missing import proof did not abstain: status=%d transfer=%#v", status, transfer)
	}
	if rows := s.memoryStore.recentItems("destination-project", "", 10, 0); len(rows) != 0 {
		t.Fatalf("abstaining transfer wrote a recommendation: %#v", rows)
	}
}

func TestDreamTransferAbstainsOnConflictReceipt(t *testing.T) {
	s, passport := newDreamTransferTestServer(t)
	ctx := dreamTransferAuthorizedContext(s)
	grantID := dreamTransferProvisionTestGrant(t, s, passport, "destination-project")
	s.contextMesh.mu.Lock()
	s.contextMesh.receipts[0].Action = "conflict_branch"
	s.contextMesh.receipts[0].Conflict = true
	s.contextMesh.mu.Unlock()
	response, status := s.buildDreamModeResponse(ctx, make(http.Header),
		dreamTransferFreezeInput(passport, grantID, time.Now().UTC().Add(5*time.Minute)), "/memory/dream")
	transfer := dreamTransferFromResponse(t, response)
	if status != http.StatusNotFound || anyToString(transfer["status"]) != "abstained" || anyToString(transfer["reason"]) != "transfer_unavailable" {
		t.Fatalf("conflicting import receipt authorized transfer: status=%d transfer=%#v", status, transfer)
	}
}

func TestDreamTransferRejectsSameRequestResultAndPredictionMutation(t *testing.T) {
	s, passport := newDreamTransferTestServer(t)
	ctx := dreamTransferAuthorizedContext(s)
	grantID := dreamTransferProvisionTestGrant(t, s, passport, "destination-project")
	payload := dreamTransferFreezeInput(passport, grantID, time.Now().UTC().Add(5*time.Minute))
	anyMap(payload["experience_transfer"])["result"] = "confirmed"
	response, status := s.buildDreamModeResponse(ctx, make(http.Header), payload, "/memory/dream")
	if status != http.StatusNotFound || anyToString(dreamTransferFromResponse(t, response)["status"]) != "abstained" {
		t.Fatalf("result was accepted before freezing: status=%d response=%#v", status, response)
	}

	payload = dreamTransferFreezeInput(passport, grantID, time.Now().UTC().Add(5*time.Minute))
	response, status = s.buildDreamModeResponse(ctx, make(http.Header), payload, "/memory/dream")
	transferID := anyToString(dreamTransferFromResponse(t, response)["transfer_id"])
	changed := map[string]any{
		"project": "destination-project",
		"experience_transfer": map[string]any{
			"action": "record_result", "transfer_id": transferID, "result": "confirmed",
			"prediction": "Changed after seeing the result.",
		},
	}
	response, status = s.buildDreamModeResponse(ctx, make(http.Header), changed, "/memory/dream")
	if status != http.StatusNotFound || anyToString(dreamTransferFromResponse(t, response)["status"]) != "abstained" {
		t.Fatalf("prediction mutation was accepted: status=%d response=%#v", status, response)
	}
	record, _, ok := s.readDreamExperienceTransfer(context.Background(), "destination-project", transferID)
	if !ok || record.Status != "awaiting_result" || !strings.Contains(record.Prediction, "Tenant-scoped") {
		t.Fatalf("invalid result request altered frozen record: %+v", record)
	}
}

func TestDreamTransferRejectsCrossProjectDestinationEvidence(t *testing.T) {
	s, passport := newDreamTransferTestServer(t)
	ctx := dreamTransferAuthorizedContext(s)
	grantID := dreamTransferProvisionTestGrant(t, s, passport, "destination-project")
	payload := dreamTransferFreezeInput(passport, grantID, time.Now().UTC().Add(5*time.Minute))
	response, _ := s.buildDreamModeResponse(ctx, make(http.Header), payload, "/memory/dream")
	transferID := anyToString(dreamTransferFromResponse(t, response)["transfer_id"])
	result := map[string]any{
		"project": "destination-project",
		"experience_transfer": map[string]any{
			"action": "record_result", "transfer_id": transferID, "result": "rejected",
			"evidence_refs": []any{"home-project::private/result.md"},
		},
	}
	response, status := s.buildDreamModeResponse(ctx, make(http.Header), result, "/memory/dream")
	if status != http.StatusNotFound || anyToString(dreamTransferFromResponse(t, response)["status"]) != "abstained" {
		t.Fatalf("cross-project evidence was accepted: status=%d response=%#v", status, response)
	}
	record, _, ok := s.readDreamExperienceTransfer(context.Background(), "destination-project", transferID)
	if !ok || record.Status != "awaiting_result" {
		t.Fatalf("cross-project evidence changed destination record: %+v", record)
	}
}

func TestDreamTransferRecordDigestBindsPredictionAndExpiry(t *testing.T) {
	s, passport := newDreamTransferTestServer(t)
	ctx := dreamTransferAuthorizedContext(s)
	grantID := dreamTransferProvisionTestGrant(t, s, passport, "destination-project")
	payload := dreamTransferFreezeInput(passport, grantID, time.Now().UTC().Add(5*time.Minute))
	response, _ := s.buildDreamModeResponse(ctx, make(http.Header), payload, "/memory/dream")
	transferID := anyToString(dreamTransferFromResponse(t, response)["transfer_id"])
	record, _, ok := s.readDreamExperienceTransfer(context.Background(), "destination-project", transferID)
	if !ok {
		t.Fatal("frozen record did not read back from the destination ledger")
	}
	originalDigest := record.RecordDigest
	record.Prediction = "mutated"
	if dreamTransferRecordDigest(record) == originalDigest {
		t.Fatal("record digest did not bind the frozen prediction")
	}
	if _, err := json.Marshal(record); err != nil {
		t.Fatalf("transfer record is not serializable: %v", err)
	}
}

func TestDreamTransferSharedServiceKeyCannotGrantOrRevokeAcrossProjects(t *testing.T) {
	s, passport := newDreamTransferTestServer(t)
	mux := buildNativeMux(s)
	var reference any
	for _, path := range []string{"/memory/dream", "/tools/dream"} {
		for _, destination := range []string{"destination-alpha", "destination-beta"} {
			for _, action := range []string{"grant", "revoke"} {
				input := map[string]any{
					"action": action, "source_passport_id": passport.PassportID,
					"experience_ref": "exp:cache-key", "destination_project": destination,
					"caller_principal": "owner", "grant_id": "guessable-grant-id",
				}
				body := map[string]any{"project": destination, "experience_transfer": input}
				encoded, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(encoded)))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("X-Api-Key", s.orchestratorAPIKey)
				request.Header.Set("X-ContextLattice-Trusted-Caller-Role", "worker")
				request.Header.Set("X-ContextLattice-Trusted-Caller-Principal", "api-key-sha256:"+strings.Repeat("a", 64))
				request.Header.Set("X-ContextLattice-Caller-Role", "owner")
				recorder := httptest.NewRecorder()
				mux.ServeHTTP(recorder, request)
				if recorder.Code != http.StatusNotFound {
					t.Fatalf("shared service credential %s %s got status=%d body=%s", action, destination, recorder.Code, recorder.Body.String())
				}
				var response map[string]any
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					t.Fatalf("decode denial response: %v", err)
				}
				denial := anyMap(response["dream_experience_transfer"])
				if anyToString(denial["reason"]) != "transfer_unavailable" ||
					strings.Contains(recorder.Body.String(), passport.PassportID) ||
					strings.Contains(recorder.Body.String(), "guessable-grant-id") ||
					strings.Contains(recorder.Body.String(), "owner") {
					t.Fatalf("denial disclosed authority/source details: status=%d body=%s", recorder.Code, recorder.Body.String())
				}
				if reference == nil {
					reference = denial
				} else if !reflect.DeepEqual(reference, denial) {
					t.Fatalf("denial varied by destination, action, or endpoint: got=%#v want=%#v", denial, reference)
				}
			}
		}
	}
	if len(s.contextMesh.dreamTransferReceipts) != 0 {
		t.Fatalf("shared service key created grant receipts: %d", len(s.contextMesh.dreamTransferReceipts))
	}
}

func TestDreamTransferInternalCallerCannotUseSpoofedHeaders(t *testing.T) {
	s, passport := newDreamTransferTestServer(t)
	input := map[string]any{
		"project": "destination-project",
		"experience_transfer": map[string]any{
			"action": "grant", "source_passport_id": passport.PassportID,
			"experience_ref": "exp:cache-key", "destination_project": "destination-project",
		},
	}
	headers := http.Header{
		"X-Contextlattice-Trusted-Caller-Role":      []string{"orchestrator"},
		"X-Contextlattice-Trusted-Caller-Principal": []string{"api-key-sha256:" + strings.Repeat("a", 64)},
	}
	response, status := s.buildDreamModeResponse(context.Background(), headers, input, "/memory/dream")
	denial := dreamTransferFromResponse(t, response)
	if status != http.StatusNotFound || anyToString(denial["reason"]) != "transfer_unavailable" ||
		strings.Contains(anyToString(denial["source_passport_id"]), passport.PassportID) {
		t.Fatalf("untrusted internal caller headers authorized or distinguished transfer: status=%d response=%#v", status, response)
	}
}

func TestDreamTransferScopedGrantCannotCrossDestinationAndWorkerCannotSelfGrant(t *testing.T) {
	s, passport := newDreamTransferTestServer(t)
	ctx := dreamTransferAuthorizedContext(s)
	grantID := dreamTransferProvisionTestGrant(t, s, passport, "destination-alpha")
	freeze := dreamTransferFreezeInput(passport, grantID, time.Now().UTC().Add(5*time.Minute))
	freeze["project"] = "destination-beta"
	anyMap(freeze["experience_transfer"])["destination_project"] = "destination-beta"
	response, status := s.buildDreamModeResponse(ctx, make(http.Header), freeze, "/tools/dream")
	denial := dreamTransferFromResponse(t, response)
	if status != http.StatusNotFound || anyToString(denial["reason"]) != "transfer_unavailable" {
		t.Fatalf("grant for destination-alpha crossed to destination-beta: status=%d transfer=%#v", status, denial)
	}
	if rows := s.memoryStore.recentItems("destination-beta", "", 10, 0); len(rows) != 0 {
		t.Fatalf("cross-destination denial wrote a destination record: %#v", rows)
	}
	selfGrant := map[string]any{
		"project": "destination-beta",
		"experience_transfer": map[string]any{
			"action": "grant", "source_passport_id": passport.PassportID,
			"experience_ref": "exp:cache-key", "destination_project": "destination-beta",
		},
	}
	response, status = s.buildDreamModeResponse(ctx, make(http.Header), selfGrant, "/tools/dream")
	if status != http.StatusNotFound || anyToString(dreamTransferFromResponse(t, response)["reason"]) != "transfer_unavailable" {
		t.Fatalf("scoped worker self-authorized a grant: status=%d response=%#v", status, response)
	}
}

func TestDreamTransferExpiredOrRevokedGrantCannotFreeze(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		s, passport := newDreamTransferTestServer(t)
		ctx := dreamTransferAuthorizedContext(s)
		grantID := dreamTransferProvisionTestGrantUntil(t, s, passport, "destination-project", time.Now().UTC().Add(-time.Minute))
		response, status := s.buildDreamModeResponse(ctx, make(http.Header),
			dreamTransferFreezeInput(passport, grantID, time.Now().UTC().Add(5*time.Minute)), "/memory/dream")
		if status != http.StatusNotFound || anyToString(dreamTransferFromResponse(t, response)["reason"]) != "transfer_unavailable" {
			t.Fatalf("expired grant authorized transfer: status=%d response=%#v", status, response)
		}
	})
	t.Run("revoked", func(t *testing.T) {
		s, passport := newDreamTransferTestServer(t)
		ctx := dreamTransferAuthorizedContext(s)
		grantID := dreamTransferProvisionTestGrant(t, s, passport, "destination-project")
		grant, found, revoked := s.contextMesh.dreamTransferGrant(grantID)
		if !found || revoked {
			t.Fatal("test grant was not active before revocation")
		}
		revocation := grant
		revocation.ReceiptID = "dgr_revoke_test_" + randomHex(8)
		revocation.Kind = "revocation"
		revocation.Signature = contextPassportSignature{}
		if err := s.contextMesh.appendDreamTransferReceipt(context.Background(), revocation, false); err != nil {
			t.Fatalf("append test revocation receipt: %v", err)
		}
		response, status := s.buildDreamModeResponse(ctx, make(http.Header),
			dreamTransferFreezeInput(passport, grantID, time.Now().UTC().Add(5*time.Minute)), "/memory/dream")
		if status != http.StatusNotFound || anyToString(dreamTransferFromResponse(t, response)["reason"]) != "transfer_unavailable" {
			t.Fatalf("revoked grant authorized transfer: status=%d response=%#v", status, response)
		}
	})
}

func TestOrdinaryContextMeshWritePreservesDreamTransferReceipts(t *testing.T) {
	s, passport := newDreamTransferTestServer(t)
	staleWriter, err := newContextMeshStore(contextMeshStoreConfig{
		Enabled: s.contextMesh.enabled, Path: s.contextMesh.path, MaxBytes: s.contextMesh.maxBytes,
		MaxGrants: s.contextMesh.maxGrants, MaxRevocations: s.contextMesh.maxRevocations,
		MaxReceipts: s.contextMesh.maxReceipts, MaxEnvelopeBytes: s.contextMesh.maxEnvelopeBytes,
		MaxPlaintextBytes: s.contextMesh.maxPlaintextBytes, Fsync: s.contextMesh.fsync,
	}, s.contextPassports)
	if err != nil {
		t.Fatalf("open stale context mesh writer: %v", err)
	}
	grantID := dreamTransferProvisionTestGrant(t, s, passport, "destination-project")
	envelope := contextMeshEnvelope{
		EnvelopeID: "env_after_dream_grant",
		Sender:     contextMeshSender{SigningKeyID: passport.Issuer.SigningKeyID},
	}
	if err := staleWriter.recordReceipt(envelope, passport, "recipient-key",
		contextPassportReconciliation{Action: "record_root", PassportID: passport.PassportID, Recorded: true}); err != nil {
		t.Fatalf("ordinary context mesh write failed: %v", err)
	}
	s.contextMesh.mu.Lock()
	loadErr := s.contextMesh.load()
	s.contextMesh.mu.Unlock()
	if loadErr != nil {
		t.Fatalf("reload after ordinary context mesh write: %v", loadErr)
	}
	if _, found, revoked := s.contextMesh.dreamTransferGrant(grantID); !found || revoked {
		t.Fatal("ordinary context mesh write dropped the signed Dream grant")
	}
}

func TestDreamTransferRejectsEditedProjectionEvenWithRecomputedMemoryHash(t *testing.T) {
	s, passport := newDreamTransferTestServer(t)
	ctx := dreamTransferAuthorizedContext(s)
	grantID := dreamTransferProvisionTestGrant(t, s, passport, "destination-project")
	payload := dreamTransferFreezeInput(passport, grantID, time.Now().UTC().Add(5*time.Minute))
	response, status := s.buildDreamModeResponse(ctx, make(http.Header), payload, "/memory/dream")
	if status != http.StatusOK {
		t.Fatalf("freeze status=%d response=%#v", status, response)
	}
	transferID := anyToString(dreamTransferFromResponse(t, response)["transfer_id"])
	filename := "dream-transfers/" + transferID + ".json"
	stored, err := s.memoryStore.fetchByMemoryID(context.Background(), "destination-project::"+filename)
	if err != nil {
		t.Fatalf("read transfer projection: %v", err)
	}
	memory := anyMap(stored["memory"])
	var edited dreamExperienceTransferRecord
	if err := json.Unmarshal([]byte(anyToString(memory["content"])), &edited); err != nil {
		t.Fatalf("decode transfer projection: %v", err)
	}
	edited.Prediction = "Edited after observing the destination outcome."
	edited.RecordDigest = dreamTransferRecordDigest(edited)
	encoded, err := json.Marshal(edited)
	if err != nil {
		t.Fatalf("encode edited projection: %v", err)
	}
	if _, _, err := s.memoryStore.put(normalizedWrite{
		project: "destination-project", fileName: filename,
		topicPath: "contextlattice/dream-mode/transfers", content: string(encoded),
	}); err != nil {
		t.Fatalf("rewrite projection and recompute ordinary memory hash: %v", err)
	}
	if _, _, ok := s.readDreamExperienceTransfer(context.Background(), "destination-project", transferID); ok {
		t.Fatal("edited projection passed signed frozen-identity verification")
	}
	result := map[string]any{
		"project":             "destination-project",
		"experience_transfer": map[string]any{"action": "record_result", "transfer_id": transferID, "result": "unproven"},
	}
	response, status = s.buildDreamModeResponse(ctx, make(http.Header), result, "/memory/dream")
	if status != http.StatusNotFound || anyToString(dreamTransferFromResponse(t, response)["reason"]) != "transfer_unavailable" {
		t.Fatalf("edited projection accepted a result: status=%d response=%#v", status, response)
	}
}

func TestDreamTransferConcurrentResultsHaveOneSignedWinner(t *testing.T) {
	s, passport := newDreamTransferTestServer(t)
	ctx := dreamTransferAuthorizedContext(s)
	grantID := dreamTransferProvisionTestGrant(t, s, passport, "destination-project")
	payload := dreamTransferFreezeInput(passport, grantID, time.Now().UTC().Add(5*time.Minute))
	response, status := s.buildDreamModeResponse(ctx, make(http.Header), payload, "/memory/dream")
	if status != http.StatusOK {
		t.Fatalf("freeze status=%d response=%#v", status, response)
	}
	transferID := anyToString(dreamTransferFromResponse(t, response)["transfer_id"])
	config := contextMeshStoreConfig{
		Enabled: s.contextMesh.enabled, Path: s.contextMesh.path, MaxBytes: s.contextMesh.maxBytes,
		MaxGrants: s.contextMesh.maxGrants, MaxRevocations: s.contextMesh.maxRevocations,
		MaxReceipts: s.contextMesh.maxReceipts, MaxEnvelopeBytes: s.contextMesh.maxEnvelopeBytes,
		MaxPlaintextBytes: s.contextMesh.maxPlaintextBytes, Fsync: s.contextMesh.fsync,
	}
	secondMesh, err := newContextMeshStore(config, s.contextPassports)
	if err != nil {
		t.Fatalf("open second mesh writer: %v", err)
	}
	secondServer := &server{
		memoryStore: s.memoryStore, writePolicy: s.writePolicy, strictNoPythonRuntime: s.strictNoPythonRuntime,
		contextPassports: s.contextPassports, contextMesh: secondMesh,
		orchestratorAPIKey: s.orchestratorAPIKey, toolCalls: s.toolCalls,
	}
	servers := []*server{s, secondServer}
	contexts := []context.Context{ctx, dreamTransferAuthorizedContext(secondServer)}
	for i, writer := range servers {
		if !writer.loadDreamTransferOwnerGrant(contexts[i], grantID) {
			t.Fatalf("writer %d could not load its live owner grant before the race", i)
		}
	}
	if _, _, err := s.memoryStore.put(normalizedWrite{
		project: "destination-project", fileName: "evidence/result.json", topicPath: "evidence",
		content: `{"destination_check":"observed"}`,
	}); err != nil {
		t.Fatalf("write destination evidence: %v", err)
	}
	results := []map[string]any{
		{"project": "destination-project", "experience_transfer": map[string]any{
			"action": "record_result", "transfer_id": transferID, "result": "confirmed",
			"evidence_refs": []any{"destination-project::evidence/result.json"},
		}},
		{"project": "destination-project", "experience_transfer": map[string]any{
			"action": "record_result", "transfer_id": transferID, "result": "narrowed",
			"evidence_refs": []any{"destination-project::evidence/result.json"},
		}},
	}
	statuses := make([]int, len(results))
	var wait sync.WaitGroup
	for index := range results {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, statuses[index] = servers[index].buildDreamModeResponse(contexts[index], make(http.Header), results[index], "/memory/dream")
		}(index)
	}
	wait.Wait()
	successes, denials := 0, 0
	for _, code := range statuses {
		switch code {
		case http.StatusOK:
			successes++
		case http.StatusNotFound:
			denials++
		}
	}
	if successes != 1 || denials != 1 {
		t.Fatalf("concurrent result statuses=%v; require one accepted and one opaque denial", statuses)
	}
	reopened, err := newContextMeshStore(config, s.contextPassports)
	if err != nil {
		t.Fatalf("open fresh mesh reader: %v", err)
	}
	reopened.mu.Lock()
	defer reopened.mu.Unlock()
	if err := reopened.loadDreamTransferReceiptsLocked(); err != nil {
		t.Fatalf("reload authoritative transfer receipts: %v", err)
	}
	resultReceipts := 0
	for _, receipt := range reopened.dreamTransferReceipts {
		if receipt.Kind == "result" && receipt.TransferID == transferID && verifyDreamTransferReceipt(receipt) {
			resultReceipts++
		}
	}
	if resultReceipts != 1 {
		t.Fatalf("concurrent writers produced %d signed result receipts, want exactly one", resultReceipts)
	}
}
