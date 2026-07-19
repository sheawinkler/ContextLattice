package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func frontierT7RouteTestServer(t testing.TB, root string) (*server, *frontierT7PortableStore, *contextMeshStore) {
	t.Helper()
	passports := newTestPassportStore(t, filepath.Join(root, "passport"))
	mesh := newTestMeshStore(t, filepath.Join(root, "mesh"), passports)
	store, err := newFrontierT7PortableStore(filepath.Join(root, "portable.json"), frontierT7StoreLimits{}, mesh.identity)
	if err != nil {
		t.Fatalf("create portable store: %v", err)
	}
	t.Cleanup(store.close)
	return &server{orchestratorAPIKey: "test-owner-key", contextPassports: passports, contextMesh: mesh, frontierT7: store}, store, mesh
}

func frontierT7RouteRequest(t testing.TB, handler http.Handler, method, path string, payload any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var body bytes.Buffer
	if payload != nil {
		if err := json.NewEncoder(&body).Encode(payload); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, &body)
	request.Header.Set("X-Api-Key", "test-owner-key")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	decoded := map[string]any{}
	if recorder.Body.Len() > 0 {
		if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("decode response status=%d body=%s: %v", recorder.Code, recorder.Body.String(), err)
		}
	}
	return recorder, decoded
}

func frontierT7AssertContractPassed(t testing.TB, payload map[string]any) {
	t.Helper()
	validation := anyMap(anyMap(payload["format_contract"])["validation"])
	if anyToString(validation["status"]) != "passed" {
		t.Fatalf("contract validation did not pass: %#v", payload)
	}
}

func frontierT7RouteGrantCreatePayload(now time.Time, recipientKeyID string) map[string]any {
	return map[string]any{
		"operation": "create",
		"subject":   map[string]any{"subject_id": "reviewer", "roles": []string{"reviewer"}, "workspace_id": "workspace", "snapshot_digest": frontierT7TestDigest("subject"), "observed_at": now.Format(time.RFC3339Nano)},
		"project":   "contextlattice", "topics": []string{"frontier-30"}, "data_classes": []string{"context-pack"},
		"actions": []string{"continue", "read"}, "purpose": "continue-reviewed-work", "usage_limit": 2,
		"approvers": []string{"owner"}, "key_epoch": 1, "recipient_key_id": recipientKeyID,
		"not_before": now.Add(-time.Minute).Format(time.RFC3339Nano), "expires_at": now.Add(time.Hour).Format(time.RFC3339Nano),
	}
}

func TestFrontierT7RoutesSealCrossMachineContinuationAndRejectReplay(t *testing.T) {
	sender, _, senderMesh := frontierT7RouteTestServer(t, filepath.Join(t.TempDir(), "sender"))
	receiver, _, receiverMesh := frontierT7RouteTestServer(t, filepath.Join(t.TempDir(), "receiver"))
	senderMux, receiverMux := buildNativeMux(sender), buildNativeMux(receiver)
	now := time.Now().UTC().Truncate(time.Microsecond)
	meshGrant, err := senderMesh.createGrant(map[string]any{
		"recipient_id": "receiver", "recipient": receiverMesh.identity.MeshRecipient,
		"project": "contextlattice", "ttl_secs": 3600,
	})
	if err != nil {
		t.Fatalf("create Mesh grant: %v", err)
	}

	created, grantPayload := frontierT7RouteRequest(t, senderMux, http.MethodPost, frontierT7GrantsPath, frontierT7RouteGrantCreatePayload(now, receiverMesh.identity.MeshKeyID))
	if created.Code != http.StatusOK {
		t.Fatalf("grant create status=%d body=%s", created.Code, created.Body.String())
	}
	frontierT7AssertContractPassed(t, grantPayload)
	grant := grantPayload
	grantID := anyToString(grant["grant_id"])

	sealed, manifestPayload := frontierT7RouteRequest(t, senderMux, http.MethodPost, frontierT7ManifestsPath, map[string]any{
		"operation": "create", "project": "contextlattice", "passport_id": "passport-route",
		"passport_digest": frontierT7TestDigest("passport"), "lineage_digest": frontierT7TestDigest("lineage"),
		"checkpoint_digest": frontierT7TestDigest("checkpoint"), "lifecycle_receipt_digest": frontierT7TestDigest("lifecycle"),
		"unresolved_obligation_digests": []string{frontierT7TestDigest("obligation")},
		"repository_constraint_digest":  frontierT7TestDigest("repository"), "destination_session_digest": frontierT7TestDigest("destination"),
		"grant_id": grantID, "mesh_grant_id": meshGrant.GrantID, "transport": "context-mesh",
		"expires_at": now.Add(30 * time.Minute).Format(time.RFC3339Nano),
	})
	if sealed.Code != http.StatusOK {
		t.Fatalf("manifest create status=%d body=%s", sealed.Code, sealed.Body.String())
	}
	frontierT7AssertContractPassed(t, manifestPayload)
	if anyToBool(manifestPayload["delivery_performed"]) || anyToString(manifestPayload["transport_owner"]) != "external_adapter" || anyToBool(manifestPayload["private_key_exported"]) {
		t.Fatalf("manifest route crossed execution boundary: %#v", manifestPayload)
	}
	envelope := manifestPayload
	reconcileRequest := map[string]any{
		"operation": "reconcile", "envelope": envelope, "expected_lineage_digest": frontierT7TestDigest("lineage"),
		"topic": "frontier-30", "data_class": "context-pack", "purpose": "continue-reviewed-work",
		"subject_snapshot_digest": frontierT7TestDigest("subject"), "key_epoch": 1,
	}
	reconciled, resultPayload := frontierT7RouteRequest(t, receiverMux, http.MethodPost, frontierT7ManifestsPath, reconcileRequest)
	if reconciled.Code != http.StatusOK || !anyToBool(resultPayload["accepted"]) {
		t.Fatalf("reconcile status=%d body=%s", reconciled.Code, reconciled.Body.String())
	}
	frontierT7AssertContractPassed(t, resultPayload)
	result := resultPayload
	if !anyToBool(result["accepted"]) || !anyToBool(result["dry_run"]) || anyToBool(result["transport_executed"]) || anyToBool(result["private_key_exported"]) {
		t.Fatalf("reconciliation crossed dry-run boundary: %#v", resultPayload)
	}
	replayed, replayPayload := frontierT7RouteRequest(t, receiverMux, http.MethodPost, frontierT7ManifestsPath, reconcileRequest)
	if replayed.Code != http.StatusConflict || anyToBool(replayPayload["accepted"]) || !containsString(anyToStringSlice(replayPayload["findings"]), "manifest_replay") {
		t.Fatalf("replay status=%d body=%s", replayed.Code, replayed.Body.String())
	}
}

func TestFrontierT7RoutesRecordExternalImportProofAndKeepTelemetryPathFree(t *testing.T) {
	root := t.TempDir()
	s, _, _ := frontierT7RouteTestServer(t, root)
	mux := buildNativeMux(s)
	planResponse, planPayload := frontierT7RouteRequest(t, mux, http.MethodPost, frontierT7ImportsPath, map[string]any{
		"operation": "plan", "project": "contextlattice", "batch_size": 1,
		"records": []frontierT7ImportRecord{frontierT7TestImportRecord("notes/import.md", "import")},
	})
	if planResponse.Code != http.StatusOK {
		t.Fatalf("plan status=%d body=%s", planResponse.Code, planResponse.Body.String())
	}
	frontierT7AssertContractPassed(t, planPayload)
	planID := anyToString(planPayload["plan_id"])
	commitResponse, commitPayload := frontierT7RouteRequest(t, mux, http.MethodPost, frontierT7ImportsPath, map[string]any{
		"operation": "commit", "plan_id": planID, "batch_index": 0,
		"external_execution_digest": frontierT7TestDigest("external-worker-proof"),
	})
	if commitResponse.Code != http.StatusOK {
		t.Fatalf("commit status=%d body=%s", commitResponse.Code, commitResponse.Body.String())
	}
	frontierT7AssertContractPassed(t, commitPayload)
	receipt := commitPayload
	if anyToBool(receipt["gateway_mutated_memory"]) || anyToString(receipt["execution_owner"]) != "external_import_worker" || !frontierT7ValidDigest(anyToString(receipt["external_execution_digest"])) {
		t.Fatalf("invalid external import proof boundary: %#v", commitPayload)
	}
	telemetryResponse, telemetry := frontierT7RouteRequest(t, mux, http.MethodGet, frontierT7TelemetryPath, nil)
	if telemetryResponse.Code != http.StatusOK {
		t.Fatalf("telemetry status=%d body=%s", telemetryResponse.Code, telemetryResponse.Body.String())
	}
	frontierT7AssertContractPassed(t, telemetry)
	encoded := telemetryResponse.Body.String()
	if bytes.Contains([]byte(encoded), []byte(root)) || strings.Contains(encoded, "portable.json") || strings.Contains(encoded, "state_path") || anyToBool(anyMap(telemetry["ownership"])["transport_owned_by_contextlattice"]) {
		t.Fatalf("telemetry leaked a path or boundary: %s", encoded)
	}
}

func TestFrontierT7RoutesSealBeforePersistAndEmitSignedRevocation(t *testing.T) {
	root := t.TempDir()
	s, store, mesh := frontierT7RouteTestServer(t, root)
	mux := buildNativeMux(s)
	now := time.Now().UTC().Truncate(time.Microsecond)
	created, grantPayload := frontierT7RouteRequest(t, mux, http.MethodPost, frontierT7GrantsPath, frontierT7RouteGrantCreatePayload(now, mesh.identity.MeshKeyID))
	if created.Code != http.StatusOK {
		t.Fatalf("grant create status=%d body=%s", created.Code, created.Body.String())
	}
	frontierT7AssertContractPassed(t, grantPayload)
	grantID := anyToString(grantPayload["grant_id"])
	failedSeal, failedPayload := frontierT7RouteRequest(t, mux, http.MethodPost, frontierT7ManifestsPath, map[string]any{
		"operation": "create", "project": "contextlattice", "passport_id": "passport-no-partial",
		"passport_digest": frontierT7TestDigest("passport-no-partial"), "lineage_digest": frontierT7TestDigest("lineage-no-partial"),
		"checkpoint_digest": frontierT7TestDigest("checkpoint-no-partial"), "lifecycle_receipt_digest": frontierT7TestDigest("lifecycle-no-partial"),
		"repository_constraint_digest": frontierT7TestDigest("repository-no-partial"), "destination_session_digest": frontierT7TestDigest("destination-no-partial"),
		"grant_id": grantID, "mesh_grant_id": "missing-mesh-grant", "transport": "context-mesh",
		"expires_at": now.Add(30 * time.Minute).Format(time.RFC3339Nano),
	})
	if failedSeal.Code != http.StatusUnprocessableEntity || anyToString(failedPayload["error"]) != "manifest_seal_failed" {
		t.Fatalf("seal failure status=%d body=%s", failedSeal.Code, failedSeal.Body.String())
	}
	store.mu.RLock()
	manifestCount := len(store.state.Manifests)
	store.mu.RUnlock()
	if manifestCount != 0 {
		t.Fatalf("failed envelope seal persisted %d manifest records", manifestCount)
	}

	revoked, revocationPayload := frontierT7RouteRequest(t, mux, http.MethodPost, frontierT7GrantsPath, map[string]any{
		"operation": "revoke", "grant_id": grantID, "reason": "operator revoked continuation access",
	})
	if revoked.Code != http.StatusOK {
		t.Fatalf("grant revoke status=%d body=%s", revoked.Code, revoked.Body.String())
	}
	frontierT7AssertContractPassed(t, revocationPayload)
	if !anyToBool(revocationPayload["tombstone_only"]) || anyToBool(revocationPayload["transport_owned_by_contextlattice"]) || !frontierT7ValidDigest(anyToString(revocationPayload["revocation_digest"])) {
		t.Fatalf("invalid signed revocation response: %#v", revocationPayload)
	}
}
