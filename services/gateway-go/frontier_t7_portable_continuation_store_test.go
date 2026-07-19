package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func frontierT7StoreTestGrantRequest(identity *contextIdentityKeys, now time.Time) frontierT7GrantCreateRequest {
	return frontierT7GrantCreateRequest{
		Subject: frontierT7GrantSubject{
			SubjectID: "portable-agent", Roles: []string{"reviewer"}, WorkspaceID: "portable-workspace",
			SnapshotDigest: frontierT7TestDigest("portable-subject"), ObservedAt: now.Format(time.RFC3339Nano),
		},
		Project: "contextlattice", Topics: []string{"frontier-30"}, DataClasses: []string{"context-pack"},
		Actions: []string{"continue", "read"}, Purpose: "continue-reviewed-work", UsageLimit: 4,
		Approvers: []string{"owner"}, KeyEpoch: 1, RecipientKeyID: identity.MeshKeyID,
		NotBefore: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
	}
}

func TestFrontierT7PortableStorePersistsUsageImportsReplayAndRevocation(t *testing.T) {
	root := t.TempDir()
	identity, err := loadOrCreateContextIdentity(filepath.Join(root, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "portable.json")
	store, err := newFrontierT7PortableStore(statePath, frontierT7StoreLimits{}, identity)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	grant, err := store.createGrant(frontierT7StoreTestGrantRequest(identity, now), now)
	if err != nil {
		store.close()
		t.Fatalf("create grant: %v", err)
	}
	decision, err := store.authorize(grant.GrantID, frontierT7GrantUseRequest{
		Project: grant.Project, Topic: "frontier-30", DataClass: "context-pack", Action: "read", Purpose: grant.Purpose,
		RecipientKeyID: grant.RecipientKeyID, SubjectSnapshotDigest: grant.Subject.SnapshotDigest, KeyEpoch: grant.KeyEpoch,
	}, true, now)
	if err != nil || !decision.Allowed || decision.RemainingUses != 3 {
		store.close()
		t.Fatalf("consume grant: decision=%#v err=%v", decision, err)
	}

	plan, err := store.buildImportPlan("contextlattice", []frontierT7ImportRecord{
		frontierT7TestImportRecord("notes/one.md", "one"), frontierT7TestImportRecord("notes/two.md", "two"),
	}, 1, now)
	if err != nil {
		store.close()
		t.Fatalf("build import plan: %v", err)
	}
	executionDigest := frontierT7TestDigest("external-import-execution")
	if _, err := store.commitImport(plan.PlanID, 0, executionDigest, now); err != nil {
		store.close()
		t.Fatalf("commit import: %v", err)
	}

	manifest, err := store.createManifest(frontierT7ContinuationRequest{
		Project: grant.Project, PassportID: "passport-portable", PassportDigest: frontierT7TestDigest("passport"),
		LineageDigest: frontierT7TestDigest("lineage"), CheckpointDigest: frontierT7TestDigest("checkpoint"),
		LifecycleReceiptDigest: frontierT7TestDigest("lifecycle"), RepositoryConstraintDigest: frontierT7TestDigest("repo"),
		DestinationSessionDigest: frontierT7TestDigest("destination"), RecipientKeyID: grant.RecipientKeyID,
		Grant: grant, Transport: "context-mesh", ExpiresAt: now.Add(30 * time.Minute),
	}, now)
	if err != nil {
		store.close()
		t.Fatalf("create manifest: %v", err)
	}
	authorization := frontierT7ContinuationAuthorization{Topic: "frontier-30", DataClass: "context-pack", Purpose: grant.Purpose, SubjectSnapshotDigest: grant.Subject.SnapshotDigest, KeyEpoch: grant.KeyEpoch}
	result, err := store.reconcileManifest(manifest, grant, manifest.LineageDigest, authorization, now)
	if err != nil || !result.Accepted {
		store.close()
		t.Fatalf("reconcile manifest: result=%#v err=%v", result, err)
	}
	store.close()

	reopened, err := newFrontierT7PortableStore(statePath, frontierT7StoreLimits{}, identity)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	result, err = reopened.reconcileManifest(manifest, grant, manifest.LineageDigest, authorization, now.Add(time.Second))
	if err != nil || result.Accepted || !containsString(result.Findings, "manifest_replay") {
		reopened.close()
		t.Fatalf("persistent replay was not rejected: result=%#v err=%v", result, err)
	}
	revocation, err := reopened.revokeGrant(grant.GrantID, "operator revoked continuation access", now.Add(2*time.Second))
	if err != nil || !frontierT7VerifyGrantRevocation(revocation) {
		reopened.close()
		t.Fatalf("revoke grant: revocation=%#v err=%v", revocation, err)
	}
	status := reopened.snapshot(now.Add(2 * time.Second))
	if anyToInt(status["grants"], 0) != 1 || anyToInt(status["import_receipts"], 0) != 1 || anyToInt(anyMap(status["storage"])["replay_records"], 0) != 1 || anyToBool(anyMap(status["ownership"])["transport_owned_by_contextlattice"]) {
		reopened.close()
		t.Fatalf("unexpected status: %#v", status)
	}
	reopened.close()

	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]any{}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	state["updated_at"] = "tampered"
	tampered, _ := json.Marshal(state)
	if err := os.WriteFile(statePath, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if invalid, err := newFrontierT7PortableStore(statePath, frontierT7StoreLimits{}, identity); err == nil {
		invalid.close()
		t.Fatal("tampered state reopened")
	}
}

func TestFrontierT7PortableStoreFailsClosedAtCapacity(t *testing.T) {
	root := t.TempDir()
	identity, err := loadOrCreateContextIdentity(filepath.Join(root, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := newFrontierT7PortableStore(filepath.Join(root, "portable.json"), frontierT7StoreLimits{MaxGrants: 1}, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	now := time.Now().UTC()
	if _, err := store.createGrant(frontierT7StoreTestGrantRequest(identity, now), now); err != nil {
		t.Fatal(err)
	}
	second := frontierT7StoreTestGrantRequest(identity, now.Add(time.Second))
	second.Subject.SnapshotDigest = frontierT7TestDigest("second-subject")
	if _, err := store.createGrant(second, now.Add(time.Second)); err == nil {
		t.Fatal("grant capacity was not enforced")
	}
}

func TestFrontierT7PortableStoreDeniedReconciliationDoesNotMutateState(t *testing.T) {
	root := t.TempDir()
	identity, err := loadOrCreateContextIdentity(filepath.Join(root, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := newFrontierT7PortableStore(filepath.Join(root, "portable.json"), frontierT7StoreLimits{}, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	now := time.Now().UTC().Truncate(time.Microsecond)
	grant, err := store.createGrant(frontierT7StoreTestGrantRequest(identity, now), now)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := frontierT7CreateContinuationManifest(identity, frontierT7ContinuationRequest{
		Project: grant.Project, PassportID: "passport-denied", PassportDigest: frontierT7TestDigest("passport-denied"),
		LineageDigest: frontierT7TestDigest("lineage-denied"), CheckpointDigest: frontierT7TestDigest("checkpoint-denied"),
		LifecycleReceiptDigest: frontierT7TestDigest("lifecycle-denied"), RepositoryConstraintDigest: frontierT7TestDigest("repo-denied"),
		DestinationSessionDigest: frontierT7TestDigest("destination-denied"), RecipientKeyID: grant.RecipientKeyID,
		Grant: grant, Transport: "context-mesh", ExpiresAt: now.Add(30 * time.Minute),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	store.mu.RLock()
	beforeHash, beforeUpdatedAt, beforeBytes := store.state.StateHash, store.state.UpdatedAt, store.fileBytes
	store.mu.RUnlock()
	result, err := store.reconcileManifest(manifest, grant, frontierT7TestDigest("different-lineage"), frontierT7ContinuationAuthorization{
		Topic: "frontier-30", DataClass: "context-pack", Purpose: grant.Purpose,
		SubjectSnapshotDigest: grant.Subject.SnapshotDigest, KeyEpoch: grant.KeyEpoch,
	}, now.Add(time.Second))
	if err != nil || result.Accepted || !containsString(result.Findings, "divergent_lineage") {
		t.Fatalf("denied reconciliation result=%#v err=%v", result, err)
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.state.StateHash != beforeHash || store.state.UpdatedAt != beforeUpdatedAt || store.fileBytes != beforeBytes || len(store.state.Replay) != 0 || len(store.state.Manifests) != 0 || store.state.GrantUsage[grant.GrantID] != 0 {
		t.Fatalf("denied reconciliation mutated durable state: %#v", store.state)
	}
}
