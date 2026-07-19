package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func frontierT7TestDigest(seed string) string {
	return "sha256:" + sha256Hex(seed)
}

func frontierT7TestGrant(t testing.TB, keys *contextIdentityKeys, now time.Time) frontierT7CollaborativeGrant {
	t.Helper()
	grant, err := frontierT7CreateCollaborativeGrant(keys, frontierT7GrantCreateRequest{
		Subject: frontierT7GrantSubject{
			SubjectID:      "agent-reviewer",
			Roles:          []string{"reviewer", "scout"},
			WorkspaceID:    "workspace-team",
			SnapshotDigest: frontierT7TestDigest("subject-snapshot"),
			ObservedAt:     now.Format(time.RFC3339Nano),
		},
		Project:        "contextlattice",
		Topics:         []string{"frontier-30", "release"},
		DataClasses:    []string{"context-pack", "checkpoint"},
		Actions:        []string{"continue", "delegate", "read"},
		Purpose:        "continue-reviewed-work",
		UsageLimit:     3,
		Approvers:      []string{"owner"},
		KeyEpoch:       4,
		RecipientKeyID: "age-x25519:recipient",
		NotBefore:      now.Add(-time.Minute),
		ExpiresAt:      now.Add(time.Hour),
	}, now)
	if err != nil {
		t.Fatalf("create grant: %v", err)
	}
	return grant
}

func TestFrontierT7PassportDiffIsHumanReadableBoundedAndRedacted(t *testing.T) {
	store := newTestPassportStore(t, t.TempDir())
	base := signedTestPassport(t, store, "contextlattice", "lineage-t7", 1, nil, "base")
	target := signedTestPassport(t, store, "contextlattice", "lineage-t7", 2, &base, "target")
	target.Objective = map[string]any{
		"objective": "continue safely",
		"api_key":   "must-not-serialize",
		"notes":     "Bearer abcdefghijklmnopqrstuvwxyz123456 at /home/example/private",
	}
	target.Claims = append(target.Claims, map[string]any{
		"portable_id": "claim-secret",
		"statement":   "token sk-abcdefghijklmnopqrstuvwxyz123456",
		"password":    "must-not-serialize",
	})
	if err := signContextPassport(&target, store.identity); err != nil {
		t.Fatalf("re-sign target: %v", err)
	}

	view, err := buildFrontierT7PassportDiffView(base, target)
	if err != nil {
		t.Fatalf("build diff view: %v", err)
	}
	if view["schema_id"] != frontierT7PassportDiffViewSchemaID || !anyToBool(view["lineage_valid"]) || !anyToBool(view["bounded"]) {
		t.Fatalf("unexpected diff envelope: %#v", view)
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	serialized := string(encoded)
	for _, forbidden := range []string{"must-not-serialize", "/home/example", "sk-abcdefghijklmnopqrstuvwxyz123456"} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("diff leaked %q: %s", forbidden, serialized)
		}
	}
	if !strings.Contains(serialized, "[local-root]") || !strings.Contains(serialized, "[token-redacted]") {
		t.Fatalf("expected explicit redaction markers: %s", serialized)
	}
	claimRows, _ := view["claims"].([]map[string]any)
	if len(claimRows) > frontierT7MaxDiffRows {
		t.Fatalf("diff exceeded row bound: %d", len(claimRows))
	}
}

func TestFrontierT7CollaborativeGrantEnforcesLeastPrivilegeAndRevocation(t *testing.T) {
	keys, err := loadOrCreateContextIdentity(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	grant := frontierT7TestGrant(t, keys, now)
	base := frontierT7GrantUseRequest{
		Project: "contextlattice", Topic: "frontier-30", DataClass: "context-pack", Action: "continue",
		Purpose: "continue-reviewed-work", RecipientKeyID: grant.RecipientKeyID,
		SubjectSnapshotDigest: grant.Subject.SnapshotDigest, KeyEpoch: grant.KeyEpoch, Now: now,
	}
	if decision := frontierT7AuthorizeGrant(grant, base); !decision.Allowed || decision.ExecutionPerformed || decision.NetworkCalls != 0 {
		t.Fatalf("valid grant denied or executed work: %#v", decision)
	}

	tests := []struct {
		name   string
		mutate func(*frontierT7GrantUseRequest)
		reason string
	}{
		{"project", func(r *frontierT7GrantUseRequest) { r.Project = "foreign" }, "project_scope_mismatch"},
		{"action", func(r *frontierT7GrantUseRequest) { r.Action = "write" }, "action_escalation"},
		{"subject", func(r *frontierT7GrantUseRequest) { r.SubjectSnapshotDigest = frontierT7TestDigest("downgraded") }, "subject_snapshot_changed"},
		{"epoch", func(r *frontierT7GrantUseRequest) { r.KeyEpoch++ }, "key_epoch_mismatch"},
		{"usage", func(r *frontierT7GrantUseRequest) { r.UsageCount = grant.UsageLimit }, "usage_limit_exhausted"},
		{"revoked", func(r *frontierT7GrantUseRequest) { r.RevokedGrantIDs = map[string]struct{}{grant.GrantID: {}} }, "grant_revoked"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := base
			tc.mutate(&request)
			decision := frontierT7AuthorizeGrant(grant, request)
			if decision.Allowed || !containsString(decision.Reasons, tc.reason) {
				t.Fatalf("decision = %#v, want denial %q", decision, tc.reason)
			}
		})
	}

	child, err := frontierT7CreateCollaborativeGrant(keys, frontierT7GrantCreateRequest{
		Subject: grant.Subject, Project: grant.Project, Topics: []string{"frontier-30"},
		DataClasses: []string{"context-pack"}, Actions: []string{"read"}, Purpose: grant.Purpose,
		UsageLimit: 2, Parent: &grant, DelegationDepth: 1, Approvers: []string{"owner"},
		KeyEpoch: grant.KeyEpoch, RecipientKeyID: grant.RecipientKeyID,
		NotBefore: now, ExpiresAt: now.Add(30 * time.Minute),
	}, now)
	if err != nil || child.ParentGrantID != grant.GrantID {
		t.Fatalf("bounded delegation failed: child=%#v err=%v", child, err)
	}
	if _, err := frontierT7CreateCollaborativeGrant(keys, frontierT7GrantCreateRequest{
		Subject: grant.Subject, Project: grant.Project, Topics: []string{"unauthorized"},
		DataClasses: []string{"context-pack"}, Actions: []string{"read"}, Purpose: grant.Purpose,
		UsageLimit: 2, Parent: &grant, DelegationDepth: 1, Approvers: []string{"owner"},
		KeyEpoch: grant.KeyEpoch, RecipientKeyID: grant.RecipientKeyID,
		NotBefore: now, ExpiresAt: now.Add(30 * time.Minute),
	}, now); err == nil {
		t.Fatal("delegation scope escalation was accepted")
	}
}

func frontierT7TestImportRecord(locator, content string) frontierT7ImportRecord {
	return frontierT7ImportRecord{
		TopicPath: "imports/research",
		Provenance: frontierT7Provenance{
			SchemaID: frontierT7ProvenanceSchemaID, SourceAlias: "obsidian", RelativeLocator: locator,
			SourceDigest: frontierT7TestDigest("source:" + locator), ContentDigest: frontierT7TestDigest(content),
			ImporterDigest: frontierT7TestDigest("importer"), ConfigDigest: frontierT7TestDigest("config"),
			TransformationManifest: map[string]any{"format": "markdown", "version": 1},
			RedactionManifest:      map[string]any{"scanner": "portable", "secret_count": 0},
			Classification:         "internal", License: "user-owned", Consent: true,
			DeletionLocator: locator, InstructionShaped: true,
		},
	}
}

func TestFrontierT7ImportPlanPreservesProvenanceAndResumesAtomically(t *testing.T) {
	records := []frontierT7ImportRecord{
		frontierT7TestImportRecord("notes/one.md", "one"),
		frontierT7TestImportRecord("notes/two.md", "two"),
		frontierT7TestImportRecord("notes/three.md", "three"),
	}
	plan, err := frontierT7BuildImportPlan("contextlattice", records, nil, 2)
	if err != nil {
		t.Fatalf("build import plan: %v", err)
	}
	if plan.BatchCount != 2 || !plan.AtomicBatches || !plan.Resumable || plan.ExecutionPerformed || plan.NetworkCalls != 0 {
		t.Fatalf("unexpected plan envelope: %#v", plan)
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0] != "instruction_shaped_content_is_untrusted_data" {
		t.Fatalf("warnings = %v", plan.Warnings)
	}
	for _, mapping := range plan.Mappings {
		if mapping.Provenance.SchemaID != frontierT7ProvenanceSchemaID || mapping.TopicPath != "imports/research" || mapping.Provenance.DeletionLocator == "" {
			t.Fatalf("mapping lost provenance: %#v", mapping)
		}
	}
	if _, err := frontierT7CommitImportBatch(plan, 1, nil, time.Now().UTC()); err == nil {
		t.Fatal("later batch committed without prior receipt")
	}
	first, err := frontierT7CommitImportBatch(plan, 0, nil, time.Now().UTC())
	if err != nil {
		t.Fatalf("commit first batch: %v", err)
	}
	prior := map[int]frontierT7ImportReceipt{0: first}
	second, err := frontierT7CommitImportBatch(plan, 1, prior, time.Now().UTC())
	if err != nil || second.ResumeBatchIndex != 2 || len(second.Mappings) != 1 {
		t.Fatalf("resume second batch: receipt=%#v err=%v", second, err)
	}
	replayed, err := frontierT7CommitImportBatch(plan, 0, prior, time.Now().UTC().Add(time.Hour))
	if err != nil || replayed.ReceiptID != first.ReceiptID || replayed.RecordedAt != first.RecordedAt {
		t.Fatalf("idempotent replay changed receipt: %#v err=%v", replayed, err)
	}
	bad := frontierT7TestImportRecord("../private.txt", "bad")
	if _, err := frontierT7BuildImportPlan("contextlattice", []frontierT7ImportRecord{bad}, nil, 1); err == nil {
		t.Fatal("path traversal import accepted")
	}
	bad = frontierT7TestImportRecord("notes/link.md", "bad")
	bad.Symlink = true
	if _, err := frontierT7BuildImportPlan("contextlattice", []frontierT7ImportRecord{bad}, nil, 1); err == nil {
		t.Fatal("symlink import accepted")
	}
}

func TestFrontierT7ContinuationManifestFailsClosedOnReplayAndConflict(t *testing.T) {
	keys, err := loadOrCreateContextIdentity(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	grant := frontierT7TestGrant(t, keys, now)
	request := frontierT7ContinuationRequest{
		Project: "contextlattice", PassportID: "passport-t7", PassportDigest: frontierT7TestDigest("passport"),
		LineageDigest: frontierT7TestDigest("lineage"), CheckpointDigest: frontierT7TestDigest("checkpoint"),
		LifecycleReceiptDigest:      frontierT7TestDigest("lifecycle"),
		UnresolvedObligationDigests: []string{frontierT7TestDigest("obligation")},
		RepositoryConstraintDigest:  frontierT7TestDigest("repo-constraints"),
		DestinationSessionDigest:    frontierT7TestDigest("destination-session"),
		RecipientKeyID:              grant.RecipientKeyID, Grant: grant, Transport: "operator-chosen", ExpiresAt: now.Add(time.Hour),
	}
	manifest, err := frontierT7CreateContinuationManifest(keys, request, now)
	if err != nil {
		t.Fatalf("create continuation: %v", err)
	}
	decision := frontierT7AuthorizeGrant(grant, frontierT7GrantUseRequest{
		Project: grant.Project, Topic: "frontier-30", DataClass: "context-pack", Action: "continue",
		Purpose: grant.Purpose, RecipientKeyID: grant.RecipientKeyID,
		SubjectSnapshotDigest: grant.Subject.SnapshotDigest, KeyEpoch: grant.KeyEpoch, Now: now,
	})
	reconciled := frontierT7ReconcileContinuation(manifest, now, grant.RecipientKeyID, request.LineageDigest, decision, nil)
	if !reconciled.Accepted || !reconciled.DryRun || reconciled.TransportExecuted || reconciled.PrivateKeyExported || reconciled.NetworkCalls != 0 {
		t.Fatalf("valid continuation did not remain dry-run: %#v", reconciled)
	}

	tests := []struct {
		name string
		run  func() frontierT7ContinuationReconciliation
		want string
	}{
		{"replay", func() frontierT7ContinuationReconciliation {
			return frontierT7ReconcileContinuation(manifest, now, grant.RecipientKeyID, request.LineageDigest, decision, map[string]string{manifest.ManifestID: manifest.ManifestDigest})
		}, "manifest_replay"},
		{"wrong-recipient", func() frontierT7ContinuationReconciliation {
			return frontierT7ReconcileContinuation(manifest, now, "other-recipient", request.LineageDigest, decision, nil)
		}, "wrong_recipient"},
		{"divergent-lineage", func() frontierT7ContinuationReconciliation {
			return frontierT7ReconcileContinuation(manifest, now, grant.RecipientKeyID, frontierT7TestDigest("other-lineage"), decision, nil)
		}, "divergent_lineage"},
		{"expired", func() frontierT7ContinuationReconciliation {
			return frontierT7ReconcileContinuation(manifest, now.Add(2*time.Hour), grant.RecipientKeyID, request.LineageDigest, decision, nil)
		}, "manifest_expired_or_invalid"},
		{"revoked", func() frontierT7ContinuationReconciliation {
			denied := decision
			denied.Allowed = false
			denied.Reasons = []string{"grant_revoked"}
			return frontierT7ReconcileContinuation(manifest, now, grant.RecipientKeyID, request.LineageDigest, denied, nil)
		}, "grant_denied_or_mismatched"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := tc.run()
			if result.Accepted || !containsString(result.Findings, tc.want) {
				t.Fatalf("reconciliation = %#v, want denial %q", result, tc.want)
			}
		})
	}

	tampered := manifest
	tampered.CheckpointDigest = frontierT7TestDigest("tampered")
	result := frontierT7ReconcileContinuation(tampered, now, grant.RecipientKeyID, request.LineageDigest, decision, nil)
	if result.Accepted || !containsString(result.Findings, "manifest_digest_mismatch") || !containsString(result.Findings, "manifest_signature_invalid") {
		t.Fatalf("tamper was not rejected: %#v", result)
	}
}
