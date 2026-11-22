package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func dreamTransferColdReopen(t *testing.T, warm *server) *server {
	t.Helper()
	t.Setenv("GO_MEMORY_STORE_BACKGROUND_HYDRATION_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_BACKGROUND_ENABLED", "false")
	passports := newTestPassportStore(t, filepath.Dir(warm.contextPassports.path))
	mesh := newTestMeshStore(t, filepath.Dir(warm.contextMesh.path), passports)
	store, err := newMemoryStoreFromEnv()
	if err != nil || !store.isEnabled() {
		t.Fatalf("cold-open destination store: enabled=%v err=%v", store != nil && store.isEnabled(), err)
	}
	cold := &server{
		memoryStore: store, writePolicy: warm.writePolicy, strictNoPythonRuntime: warm.strictNoPythonRuntime,
		contextPassports: passports, contextMesh: mesh, orchestratorAPIKey: warm.orchestratorAPIKey,
		toolCalls: warm.toolCalls,
	}
	setupDreamOwnerTestWorkspace(t, cold)
	return cold
}

func dreamTransferBlockBlobWrites(t *testing.T, s *server) func() {
	t.Helper()
	previous := s.memoryStore.policy.contentBlobsPath
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("block"), 0600); err != nil {
		t.Fatal(err)
	}
	s.memoryStore.policy.contentBlobsPath = blocked
	return func() { s.memoryStore.policy.contentBlobsPath = previous }
}

func dreamTransferWriteDestinationEvidence(t *testing.T, s *server, content string) {
	t.Helper()
	if _, _, err := s.memoryStore.put(normalizedWrite{
		project: "destination-project", fileName: "evidence/result.json", topicPath: "evidence/results",
		content: content, lifecycle: "durable", tags: []string{"kind:evidence"},
	}); err != nil {
		t.Fatalf("write destination evidence: %v", err)
	}
}

func dreamTransferResultPayload(transferID string) map[string]any {
	return map[string]any{
		"project": "destination-project",
		"experience_transfer": map[string]any{
			"action": "record_result", "transfer_id": transferID,
			"result": "confirmed", "evidence_refs": []any{"destination-project::evidence/result.json"},
		},
	}
}

func dreamTransferStoredRecord(t *testing.T, s *server, transferID string) dreamExperienceTransferRecord {
	t.Helper()
	content := dreamTransferStoredContent(t, s, transferID)
	var record dreamExperienceTransferRecord
	if strictJSONDecode([]byte(content), &record) != nil || record.RecordDigest != dreamTransferRecordDigest(record) {
		t.Fatal("projected bytes do not match the signed record digest")
	}
	return record
}

func dreamTransferStoredContent(t *testing.T, s *server, transferID string) string {
	t.Helper()
	result, err := s.memoryStore.fetchByMemoryID(context.Background(), "destination-project::dream-transfers/"+transferID+".json")
	if err != nil {
		t.Fatalf("read projected transfer: %v", err)
	}
	return anyToString(anyMap(result["memory"])["content"])
}

func TestDreamTransferFreezeRecoveryAfterColdReopenIsExactAndIdempotent(t *testing.T) {
	s, passport := newDreamTransferTestServer(t)
	grantID := dreamTransferProvisionTestGrant(t, s, passport, "destination-project")
	payload := dreamTransferFreezeInput(passport, grantID, time.Now().UTC().Add(5*time.Minute))
	restore := dreamTransferBlockBlobWrites(t, s)
	failed, status := dreamTransferPostMemory(t, s, payload)
	if status != http.StatusServiceUnavailable || anyToBool(failed["ok"]) || anyToBool(failed["persisted"]) {
		restore()
		t.Fatalf("injected freeze projection failure was misreported: status=%d response=%#v", status, failed)
	}
	frozen := dreamTransferFromResponse(t, failed)
	transferID := anyToString(frozen["transfer_id"])
	if transferID == "" || !anyToBool(frozen["ledger_recorded"]) || anyToBool(frozen["projection_persisted"]) {
		restore()
		t.Fatalf("failed freeze did not retain a recoverable signed identity: %#v", frozen)
	}
	if _, err := s.memoryStore.fetchByMemoryID(context.Background(), "destination-project::dream-transfers/"+transferID+".json"); err == nil {
		restore()
		t.Fatal("injected failure unexpectedly materialized the freeze projection")
	}
	receipt, found := s.contextMesh.dreamTransferReceipt("freeze", transferID)
	recovery, recoveryOK := dreamTransferRecoveryRecord(receipt)
	if !found || !recoveryOK || recovery.RecordDigest != receipt.RecordDigest ||
		receipt.RequestDigest != dreamTransferRequestDigest("destination-project", anyMap(payload["experience_transfer"])) {
		restore()
		t.Fatal("signed freeze receipt did not bind exact request and bounded recovery record")
	}

	wrongWorker, wrongWorkerStatus := dreamTransferPostMemoryWithProof(t, s, payload, []string{strings.Repeat("b", 64)})
	if wrongWorkerStatus != http.StatusNotFound || !strings.Contains(anyToString(anyMap(wrongWorker["dream_experience_transfer"])["reason"]), "transfer_unavailable") {
		restore()
		t.Fatal("different worker proof recovered the projection")
	}
	changed := map[string]any{
		"project":             payload["project"],
		"experience_transfer": cloneMap(anyMap(payload["experience_transfer"])),
		"use_llm":             false, "persist": false,
	}
	anyMap(changed["experience_transfer"])["falsifier"] = "different falsifier"
	wrongRequest, wrongRequestStatus := dreamTransferPostMemory(t, s, changed)
	if wrongRequestStatus != http.StatusNotFound || anyToString(dreamTransferFromResponse(t, wrongRequest)["status"]) != "abstained" {
		restore()
		t.Fatal("changed freeze request was treated as the signed retry")
	}
	restore()

	cold := dreamTransferColdReopen(t, s)
	authCtx := dreamTransferAuthorizedContext(cold)
	identity, identityOK := dreamTransferIdentityFromContext(authCtx)
	ownerSnapshot, ownerSnapshotOK := cold.dreamOwnerSnapshot(grantID, time.Now().UTC())
	loaded := cold.loadDreamTransferOwnerGrant(authCtx, grantID)
	coldGrant, coldFound, coldRevoked := cold.contextMesh.dreamTransferGrant(grantID)
	if !loaded || !coldFound || coldRevoked || !cold.dreamTransferOwnerPermits(coldGrant, time.Now().UTC()) ||
		!dreamTransferGrantMatches(coldGrant, receipt) {
		t.Fatalf("cold owner grant checks failed: identity=%v principalMatch=%v snapshot=%v snapshotMatches=%v loaded=%v found=%v revoked=%v ownerPermits=%v grantMatches=%v",
			identityOK, identity.principal == receipt.CallerPrincipal, ownerSnapshotOK,
			ownerSnapshot.ReceiptID == coldGrant.ReceiptID, loaded, coldFound, coldRevoked,
			cold.dreamTransferOwnerPermits(coldGrant, time.Now().UTC()), dreamTransferGrantMatches(coldGrant, receipt))
	}
	if coldReceipt, ok := cold.contextMesh.dreamTransferFreezeForRequest(grantID,
		receipt.CallerPrincipal, "destination-project", receipt.RequestDigest); !ok || coldReceipt.ReceiptID != receipt.ReceiptID {
		t.Fatal("cold owner could not locate the exact signed freeze retry")
	} else {
		candidate, recoveryOK := dreamTransferRecoveryRecord(coldReceipt)
		if !recoveryOK || !cold.dreamTransferRecordSecretSafe(candidate) || dreamTransferProjectionHash(candidate) == "" {
			t.Fatalf("cold signed recovery payload failed validation: recoverable=%v secretSafe=%v hashPresent=%v",
				recoveryOK, cold.dreamTransferRecordSecretSafe(candidate), dreamTransferProjectionHash(candidate) != "")
		}
		if _, terminal := cold.contextMesh.dreamTransferReceipt("result", transferID); terminal {
			t.Fatal("unexpected terminal result receipt before freeze recovery")
		}
	}
	recovered, recoveredStatus := dreamTransferPostMemory(t, cold, payload)
	recoveredTransfer := dreamTransferFromResponse(t, recovered)
	if recoveredStatus != http.StatusOK || anyToString(recoveredTransfer["transfer_id"]) != transferID ||
		!anyToBool(recoveredTransfer["projection_persisted"]) {
		t.Fatalf("cold owner did not recover the same freeze: status=%d transfer=%#v", recoveredStatus, recoveredTransfer)
	}
	if len(cold.contextMesh.dreamTransferReceipts) != 2 {
		t.Fatalf("retry appended another freeze identity: receipts=%d", len(cold.contextMesh.dreamTransferReceipts))
	}
	before := cold.memoryStore.latestHash[memoryStoreKey("destination-project", "dream-transfers/"+transferID+".json")]
	replayed, replayStatus := dreamTransferPostMemory(t, cold, payload)
	if replayStatus != http.StatusOK || anyToString(dreamTransferFromResponse(t, replayed)["transfer_id"]) != transferID {
		t.Fatalf("materialized freeze retry did not return the existing identity: status=%d response=%#v", replayStatus, replayed)
	}
	after := cold.memoryStore.latestHash[memoryStoreKey("destination-project", "dream-transfers/"+transferID+".json")]
	if before == "" || after != before {
		t.Fatal("materialized freeze retry performed a new projection write")
	}
	stored := dreamTransferStoredRecord(t, cold, transferID)
	if stored.RecordDigest != receipt.RecordDigest || stored.Status != "awaiting_result" ||
		strings.TrimSuffix(dreamTransferStoredContent(t, cold, transferID), "\n") != receipt.RecoveryRecord {
		t.Fatal("recovered projection bytes did not match the signed freeze receipt")
	}
}

func TestDreamTransferResultRecoveryAfterColdReopenAndChangedEvidenceRefusal(t *testing.T) {
	t.Run("same evidence recovers after restart", func(t *testing.T) {
		s, passport := newDreamTransferTestServer(t)
		grantID := dreamTransferProvisionTestGrant(t, s, passport, "destination-project")
		freezePayload := dreamTransferFreezeInput(passport, grantID, time.Now().UTC().Add(5*time.Minute))
		freeze, freezeStatus := dreamTransferPostMemory(t, s, freezePayload)
		transferID := anyToString(dreamTransferFromResponse(t, freeze)["transfer_id"])
		if freezeStatus != http.StatusOK || transferID == "" {
			t.Fatalf("freeze status=%d response=%#v", freezeStatus, freeze)
		}
		dreamTransferWriteDestinationEvidence(t, s, "result observed after frozen prediction")
		resultPayload := dreamTransferResultPayload(transferID)
		restore := dreamTransferBlockBlobWrites(t, s)
		failed, status := dreamTransferPostMemory(t, s, resultPayload)
		if status != http.StatusServiceUnavailable || anyToBool(failed["ok"]) || anyToBool(failed["persisted"]) {
			restore()
			t.Fatalf("injected result projection failure was misreported: status=%d response=%#v", status, failed)
		}
		resultReceipt, found := s.contextMesh.dreamTransferReceipt("result", transferID)
		if !found || resultReceipt.RequestDigest != dreamTransferRequestDigest("destination-project", anyMap(resultPayload["experience_transfer"])) {
			restore()
			t.Fatal("failed result did not leave an exact signed retry identity")
		}
		wrongProof, wrongStatus := dreamTransferPostMemoryWithProof(t, s, resultPayload, []string{strings.Repeat("c", 64)})
		if wrongStatus != http.StatusNotFound || anyToString(dreamTransferFromResponse(t, wrongProof)["reason"]) != "transfer_unavailable" {
			restore()
			t.Fatal("wrong worker proof recovered terminal projection")
		}
		restore()

		cold := dreamTransferColdReopen(t, s)
		recovered, recoveredStatus := dreamTransferPostMemory(t, cold, resultPayload)
		transfer := dreamTransferFromResponse(t, recovered)
		if recoveredStatus != http.StatusOK || anyToString(transfer["status"]) != "confirmed" || !anyToBool(transfer["projection_persisted"]) {
			t.Fatalf("cold owner did not recover signed result: status=%d transfer=%#v", recoveredStatus, transfer)
		}
		stored := dreamTransferStoredRecord(t, cold, transferID)
		if stored.RecordDigest != resultReceipt.RecordDigest || stored.Status != "confirmed" ||
			strings.TrimSuffix(dreamTransferStoredContent(t, cold, transferID), "\n") != resultReceipt.RecoveryRecord {
			t.Fatal("recovered result bytes do not match signed result digest")
		}
	})

	t.Run("changed evidence refuses recovery", func(t *testing.T) {
		s, passport := newDreamTransferTestServer(t)
		grantID := dreamTransferProvisionTestGrant(t, s, passport, "destination-project")
		freezePayload := dreamTransferFreezeInput(passport, grantID, time.Now().UTC().Add(5*time.Minute))
		freeze, freezeStatus := dreamTransferPostMemory(t, s, freezePayload)
		transferID := anyToString(dreamTransferFromResponse(t, freeze)["transfer_id"])
		if freezeStatus != http.StatusOK || transferID == "" {
			t.Fatalf("freeze status=%d response=%#v", freezeStatus, freeze)
		}
		dreamTransferWriteDestinationEvidence(t, s, "original result evidence")
		resultPayload := dreamTransferResultPayload(transferID)
		restore := dreamTransferBlockBlobWrites(t, s)
		failed, status := dreamTransferPostMemory(t, s, resultPayload)
		if status != http.StatusServiceUnavailable || !anyToBool(dreamTransferFromResponse(t, failed)["ledger_recorded"]) {
			restore()
			t.Fatalf("result failure did not persist signed recovery receipt: status=%d response=%#v", status, failed)
		}
		restore()
		dreamTransferWriteDestinationEvidence(t, s, "replacement result evidence")
		cold := dreamTransferColdReopen(t, s)
		response, retryStatus := dreamTransferPostMemory(t, cold, resultPayload)
		if retryStatus != http.StatusNotFound || anyToString(dreamTransferFromResponse(t, response)["status"]) != "abstained" {
			t.Fatalf("changed evidence was accepted during recovery: status=%d response=%#v", retryStatus, response)
		}
		stored, _, ok := cold.readDreamExperienceTransfer(context.Background(), "destination-project", transferID)
		if !ok || stored.Status != "awaiting_result" {
			t.Fatal("changed-evidence refusal overwrote the frozen projection")
		}
	})
}

func TestDreamTransferRecoveryRefusesRevokedOwnerAndDifferentDiskBytes(t *testing.T) {
	t.Run("revoked owner", func(t *testing.T) {
		s, passport := newDreamTransferTestServer(t)
		grantID := dreamTransferProvisionTestGrant(t, s, passport, "destination-project")
		payload := dreamTransferFreezeInput(passport, grantID, time.Now().UTC().Add(5*time.Minute))
		restore := dreamTransferBlockBlobWrites(t, s)
		failed, status := dreamTransferPostMemory(t, s, payload)
		if status != http.StatusServiceUnavailable {
			restore()
			t.Fatalf("freeze status=%d response=%#v", status, failed)
		}
		restore()
		grant, found, revoked := s.contextMesh.dreamTransferGrant(grantID)
		if !found || revoked {
			t.Fatal("owner grant missing before revocation")
		}
		revocation := grant
		revocation.ReceiptID = "dgr_revoke_recovery_" + randomHex(8)
		revocation.Kind = "revocation"
		revocation.Signature = contextPassportSignature{}
		if err := s.contextMesh.appendDreamTransferReceipt(context.Background(), revocation, false); err != nil {
			t.Fatal(err)
		}
		cold := dreamTransferColdReopen(t, s)
		response, retryStatus := dreamTransferPostMemory(t, cold, payload)
		if retryStatus != http.StatusNotFound || anyToString(dreamTransferFromResponse(t, response)["status"]) != "abstained" {
			t.Fatalf("revoked owner recovered a projection: status=%d response=%#v", retryStatus, response)
		}
	})

	t.Run("unrelated projection bytes", func(t *testing.T) {
		s, _ := newDreamTransferTestServer(t)
		fileName := "dream-transfers/dtx_cascontrol.json"
		old := normalizedWrite{project: "destination-project", fileName: fileName, topicPath: "contextlattice/dream-mode/transfers", content: "unrelated user projection", lifecycle: "durable"}
		if _, _, err := s.memoryStore.put(old); err != nil {
			t.Fatal(err)
		}
		before, err := s.memoryStore.fetchByMemoryID(context.Background(), "destination-project::"+fileName)
		if err != nil {
			t.Fatal(err)
		}
		beforeContent := anyToString(anyMap(before["memory"])["content"])
		replacement := old
		replacement.content = "recovery content must not clobber this"
		if _, _, err := s.memoryStore.putDreamTransferProjectionCAS(replacement, "", true); !errors.Is(err, errMemoryStoreWritePrecondition) {
			t.Fatalf("create-if-absent CAS replaced an existing row: err=%v", err)
		}
		if _, _, err := s.memoryStore.putDreamTransferProjectionCAS(replacement, strings.Repeat("0", 64), false); !errors.Is(err, errMemoryStoreWritePrecondition) {
			t.Fatalf("mismatched expected-value CAS replaced an existing row: err=%v", err)
		}
		after, err := s.memoryStore.fetchByMemoryID(context.Background(), "destination-project::"+fileName)
		if err != nil || anyToString(anyMap(after["memory"])["content"]) != beforeContent {
			t.Fatalf("pre-existing projection bytes changed: err=%v after=%#v", err, after)
		}
	})
}

func TestDreamTransferConcurrentConflictingRecoveryVerdictsHaveOneWinner(t *testing.T) {
	s, passport := newDreamTransferTestServer(t)
	grantID := dreamTransferProvisionTestGrant(t, s, passport, "destination-project")
	freezePayload := dreamTransferFreezeInput(passport, grantID, time.Now().UTC().Add(5*time.Minute))
	freeze, freezeStatus := dreamTransferPostMemory(t, s, freezePayload)
	transferID := anyToString(dreamTransferFromResponse(t, freeze)["transfer_id"])
	if freezeStatus != http.StatusOK || transferID == "" {
		t.Fatalf("freeze status=%d response=%#v", freezeStatus, freeze)
	}
	dreamTransferWriteDestinationEvidence(t, s, "shared evidence for competing verdicts")
	requests := []map[string]any{dreamTransferResultPayload(transferID), dreamTransferResultPayload(transferID)}
	anyMap(requests[0]["experience_transfer"])["result"] = "confirmed"
	anyMap(requests[1]["experience_transfer"])["result"] = "rejected"
	statuses := make([]int, len(requests))
	responses := make([]map[string]any, len(requests))
	var wait sync.WaitGroup
	for index := range requests {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			responses[index], statuses[index] = dreamTransferPostMemory(t, s, requests[index])
		}(index)
	}
	wait.Wait()
	winners := 0
	for _, status := range statuses {
		if status == http.StatusOK {
			winners++
		} else if status != http.StatusNotFound {
			t.Fatalf("conflicting result got unexpected status=%d responses=%#v", status, responses)
		}
	}
	if winners != 1 {
		t.Fatalf("conflicting results had %d accepted winners: statuses=%v", winners, statuses)
	}
	result, found := s.contextMesh.dreamTransferReceipt("result", transferID)
	if !found || (result.ResultStatus != "confirmed" && result.ResultStatus != "rejected") {
		t.Fatal("signed ledger does not contain exactly one terminal verdict")
	}
	stored := dreamTransferStoredRecord(t, s, transferID)
	if stored.Status != result.ResultStatus || stored.RecordDigest != result.RecordDigest {
		t.Fatal("terminal projection disagrees with the signed winning verdict")
	}
}

func TestDreamTransferSecretInRecoveryRecordIsOpaqueAndNeverSigned(t *testing.T) {
	s, passport := newDreamTransferTestServer(t)
	grantID := dreamTransferProvisionTestGrant(t, s, passport, "destination-project")
	payload := dreamTransferFreezeInput(passport, grantID, time.Now().UTC().Add(5*time.Minute))
	secret := "api_key=syntheticSecretValue123456"
	anyMap(payload["experience_transfer"])["falsifier"] = "Fail if observed; " + secret
	response, status := dreamTransferPostMemory(t, s, payload)
	if status != http.StatusNotFound || anyToString(dreamTransferFromResponse(t, response)["status"]) != "abstained" {
		t.Fatalf("secret-bearing recovery record was not refused opaquely: status=%d response=%#v", status, response)
	}
	ledger, err := os.ReadFile(s.contextMesh.dreamLedgerPath())
	if err != nil || strings.Contains(string(ledger), secret) || strings.Contains(mustJSONForDreamRecoveryTest(response), secret) {
		t.Fatal("synthetic secret appeared in signed sidecar or HTTP response")
	}
	for _, receipt := range s.contextMesh.dreamTransferReceipts {
		if receipt.Kind == "freeze" {
			t.Fatal("secret-bearing freeze created a signed receipt")
		}
	}
}

func mustJSONForDreamRecoveryTest(value map[string]any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
