package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func continuousCognitionU6HighValueFixture(t *testing.T) (continuousCognitionRequest, continuousCognitionObservation, continuousCognitionFrontier) {
	t.Helper()
	request := continuousCognitionTestRequest(t)
	observation := continuousCognitionTestObservation()
	observation.UtilityVerified = true
	observation.UtilityStatus = "verified"
	observation.ObjectiveState = "blocked"
	frontier := computeContinuousCognitionFrontier(observation, continuousCognitionFrontierPolicy{
		MaxRounds: 3, InvestigateThreshold: 0.55, ContinueThreshold: 0.70, ConsequenceHighThreshold: 0.70,
	})
	return request, observation, frontier
}

func TestU6ContinuousCognitionSilenceReasonsAndContract(t *testing.T) {
	baseRequest := continuousCognitionTestRequest(t)
	baseObservation := continuousCognitionTestObservation()
	baseFrontier := computeContinuousCognitionFrontier(baseObservation, continuousCognitionFrontierPolicy{})
	cases := []struct {
		name       string
		mutate     func(*continuousCognitionRequest, *continuousCognitionObservation)
		suppressed bool
		want       string
	}{
		{name: "terminal", mutate: func(_ *continuousCognitionRequest, observation *continuousCognitionObservation) {
			observation.ObjectiveState = "completed"
		}, want: "terminal"},
		{name: "duplicate", mutate: func(_ *continuousCognitionRequest, observation *continuousCognitionObservation) {
			observation.Gaps = []continuousCognitionGap{{Code: "duplicate_cycle", Source: "cycle", DetailRef: "ref_cycle_duplicate"}}
		}, want: "duplicate"},
		{name: "low utility", mutate: func(_ *continuousCognitionRequest, _ *continuousCognitionObservation) {}, want: "low_utility"},
		{name: "missing identity", mutate: func(request *continuousCognitionRequest, _ *continuousCognitionObservation) {
			request.TaskIdentityID = ""
		}, want: "missing_identity"},
		{name: "policy suppressed", mutate: func(_ *continuousCognitionRequest, _ *continuousCognitionObservation) {}, suppressed: true, want: "policy_suppressed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request, observation := baseRequest, baseObservation
			tc.mutate(&request, &observation)
			frontier := baseFrontier
			if observation.ObjectiveState == "completed" {
				frontier = computeContinuousCognitionFrontier(observation, continuousCognitionFrontierPolicy{})
			}
			decision := decideContinuousCognitionSilence(request, observation, frontier, tc.suppressed)
			if decision.Reason != tc.want || decision.Threshold != continuousCognitionSilenceThreshold || decision.PolicyVersion != continuousCognitionSilencePolicyVersion || decision.LoopGuardDigest == "" {
				t.Fatalf("decision=%#v want reason=%q", decision, tc.want)
			}
		})
	}

	request, observation := baseRequest, baseObservation
	frontier := baseFrontier
	silence := decideContinuousCognitionSilence(request, observation, frontier, false)
	payload := buildContinuousCognitionSemanticPayloadWithGovernanceAndSilence(request, observation, frontier, continuousCognitionInvestigation{}, continuousCognitionDefaultActivation(), continuousCognitionDefaultGovernance(), silence)
	if anyToString(payload["decision"]) != "silence" || anyToString(payload["next_action"]) != "none" || anyToBool(payload["writeback_required"]) {
		t.Fatalf("silence did not close the action/writeback boundary: %#v", payload)
	}
	if activation := anyMap(payload["activation"]); anyToString(activation["state"]) != "not_requested" {
		t.Fatalf("silence exposed activation advice: %#v", activation)
	}
	payload = attachPayloadFormatContract(continuousCognitionContractID, payload, "u6-test", "test", "/u6/continuous-cognition")
	if findings := validateAgentContractPayload(continuousCognitionContractID, payload); len(findings) != 0 {
		t.Fatalf("silence payload failed contract: %#v", findings)
	}

	highRequest, highObservation, highFrontier := continuousCognitionU6HighValueFixture(t)
	highSilence := decideContinuousCognitionSilence(highRequest, highObservation, highFrontier, false)
	if highSilence.Reason != "not_silenced" || highSilence.ValueScore < continuousCognitionSilenceThreshold {
		t.Fatalf("high-value observation was silenced: %#v", highSilence)
	}
	highPayload := buildContinuousCognitionSemanticPayloadWithGovernanceAndSilence(highRequest, highObservation, highFrontier, continuousCognitionInvestigation{}, continuousCognitionDefaultActivation(), continuousCognitionDefaultGovernance(), highSilence)
	if anyToString(highPayload["decision"]) == "silence" || !anyToBool(highPayload["writeback_required"]) || anyToString(highPayload["next_action"]) == "none" {
		t.Fatalf("non-silence did not preserve writeback/next action: %#v", highPayload)
	}
	highPayload = attachPayloadFormatContract(continuousCognitionContractID, highPayload, "u6-test", "test", "/u6/continuous-cognition")
	if findings := validateAgentContractPayload(continuousCognitionContractID, highPayload); len(findings) != 0 {
		t.Fatalf("non-silence payload failed contract: %#v", findings)
	}
}

func TestU6ContinuousCognitionSilenceThresholdBoundaries(t *testing.T) {
	request := continuousCognitionTestRequest(t)
	base := continuousCognitionTestObservation()
	base.SourceComplete = false
	base.ProofComplete = false
	base.SourceAnchorDigest = ""
	base.ObjectiveState = "active"
	base.Gaps = nil
	base.UtilityVerified = true
	base.UtilityStatus = "verified"
	base.UtilitySnapshotRef = "ref_utility_snapshot_eeeeeeeeeeeeeeeeeeeeeeee"
	frontier := continuousCognitionFrontier{}
	for _, tc := range []struct {
		name         string
		mutate       func(*continuousCognitionObservation)
		wantScore    int
		wantSilenced bool
	}{
		{name: "zero", mutate: func(*continuousCognitionObservation) {}, wantScore: 0, wantSilenced: true},
		{name: "one", mutate: func(observation *continuousCognitionObservation) {
			observation.SourceComplete = true
			observation.ProofComplete = true
			observation.SourceAnchorDigest = "sha256:" + strings.Repeat("a", 64)
		}, wantScore: 1, wantSilenced: true},
		{name: "two activates", mutate: func(observation *continuousCognitionObservation) {
			observation.SourceComplete = true
			observation.ProofComplete = true
			observation.SourceAnchorDigest = "sha256:" + strings.Repeat("a", 64)
			observation.ObjectiveState = "blocked"
		}, wantScore: 2, wantSilenced: false},
		{name: "unavailable value silences", mutate: func(observation *continuousCognitionObservation) {
			observation.SourceComplete = true
			observation.ProofComplete = true
			observation.SourceAnchorDigest = "sha256:" + strings.Repeat("a", 64)
			observation.ObjectiveState = "blocked"
			observation.UtilityVerified = false
			observation.UtilityStatus = "not_observed"
			observation.UtilitySnapshotRef = continuousCognitionUnavailableRef("utility_snapshot")
		}, wantScore: 2, wantSilenced: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observation := base
			tc.mutate(&observation)
			decision := decideContinuousCognitionSilence(request, observation, frontier, false)
			if decision.ValueScore != tc.wantScore || (decision.Reason != "not_silenced") != tc.wantSilenced {
				t.Fatalf("threshold decision drifted: %#v", decision)
			}
		})
	}
}

func TestU6PortableEvidenceIdentityIsClosedAndLegacyStable(t *testing.T) {
	identity := &portableEvidenceIdentity{
		SchemaID: portableEvidenceIdentitySchemaID, Version: 1,
		ResponseDigest: frontierT7TestDigest("response"), CognitionDigest: frontierT7TestDigest("cognition"),
		TemporalPremiseDigest: frontierT7TestDigest("temporal"), OrderedComponentSetDigest: frontierT7TestDigest("components"),
		ScopeDigest: frontierT7TestDigest("scope"), GapDigest: frontierT7TestDigest("gaps"),
		ActionBoundaryDigest: frontierT7TestDigest("action"), Basis: "evidence_only", CanAuthorize: false,
	}
	identity.IdentityDigest = portableEvidenceIdentityDigest(*identity)
	if err := validatePortableEvidenceIdentity(identity); err != nil {
		t.Fatalf("valid portable evidence identity rejected: %v", err)
	}
	if findings := validateAgentContractPayload(portableEvidenceIdentitySchemaID, identity); len(findings) != 0 {
		t.Fatalf("valid identity failed closed contract: %#v", findings)
	}
	legacy, err := json.Marshal(contextPassportUnsigned{SchemaID: contextPassportContractID, Version: 1})
	if err != nil || strings.Contains(string(legacy), "portable_evidence_identity") {
		t.Fatalf("legacy unsigned passport signature shape changed: %s err=%v", legacy, err)
	}

	withIdentity := *identity
	encoded, err := json.Marshal(withIdentity)
	if err != nil || !strings.Contains(string(encoded), portableEvidenceIdentitySchemaID) {
		t.Fatalf("identity did not serialize as a closed object: %s err=%v", encoded, err)
	}
	withIdentity.CanAuthorize = true
	if err := validatePortableEvidenceIdentity(&withIdentity); err == nil {
		t.Fatal("authority-bearing evidence identity was accepted")
	}
	withIdentity = *identity
	withIdentity.ResponseDigest = frontierT7TestDigest("tampered")
	if err := validatePortableEvidenceIdentity(&withIdentity); err == nil {
		t.Fatal("mismatched evidence identity digest was accepted")
	}
	for name, value := range map[string]any{
		"null":    nil,
		"partial": map[string]any{"schema_id": portableEvidenceIdentitySchemaID},
		"unknown": map[string]any{
			"schema_id": portableEvidenceIdentitySchemaID, "version": 1,
			"response_digest": identity.ResponseDigest, "cognition_digest": identity.CognitionDigest,
			"temporal_premise_digest":      identity.TemporalPremiseDigest,
			"ordered_component_set_digest": identity.OrderedComponentSetDigest,
			"scope_digest":                 identity.ScopeDigest, "gap_digest": identity.GapDigest,
			"action_boundary_digest": identity.ActionBoundaryDigest, "basis": identity.Basis,
			"can_authorize": false, "identity_digest": identity.IdentityDigest, "authority": "forged",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodePortableEvidenceIdentity(value); err == nil {
				t.Fatalf("%s portable evidence identity was accepted", name)
			}
		})
	}
}

func TestU6ContinuationEvidenceIdentityBindsManifestAndStatus(t *testing.T) {
	root := t.TempDir()
	identity, err := loadOrCreateContextIdentity(root + "/identity.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	store, err := newFrontierT7PortableStore(root+"/state.json", frontierT7StoreLimits{}, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	grant, err := store.createGrant(frontierT7GrantCreateRequest{
		Subject: frontierT7GrantSubject{SubjectID: "u6-agent", Roles: []string{"reviewer"}, WorkspaceID: "u6-workspace", SnapshotDigest: frontierT7TestDigest("subject"), ObservedAt: now.Format(time.RFC3339Nano)},
		Project: "contextlattice", Topics: []string{"response-intelligence"}, DataClasses: []string{"checkpoint"}, Actions: []string{"continue", "read"}, Purpose: "u6-evidence", UsageLimit: 2, Approvers: []string{"owner"}, KeyEpoch: 1, RecipientKeyID: identity.MeshKeyID, NotBefore: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	evidenceDigest := frontierT7TestDigest("portable-evidence-identity")
	manifest, err := frontierT7CreateContinuationManifest(identity, frontierT7ContinuationRequest{
		Project: "contextlattice", PassportID: "passport-u6", PassportDigest: frontierT7TestDigest("passport"), EvidenceIdentityDigest: evidenceDigest, LineageDigest: frontierT7TestDigest("lineage"), CheckpointDigest: frontierT7TestDigest("checkpoint"), LifecycleReceiptDigest: frontierT7TestDigest("lifecycle"), RepositoryConstraintDigest: frontierT7TestDigest("repo"), DestinationSessionDigest: frontierT7TestDigest("destination"), RecipientKeyID: identity.MeshKeyID, Grant: grant, Transport: "context-mesh", ExpiresAt: now.Add(30 * time.Minute),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.EvidenceIdentityDigest != evidenceDigest || frontierT7ValidateContinuationManifest(manifest, now) != nil {
		t.Fatalf("evidence identity was not bound into the signed manifest: %#v", manifest)
	}
	legacyManifest, err := frontierT7CreateContinuationManifest(identity, frontierT7ContinuationRequest{
		Project: "contextlattice", PassportID: "passport-u6-legacy", PassportDigest: frontierT7TestDigest("passport-legacy"), LineageDigest: frontierT7TestDigest("lineage-legacy"), CheckpointDigest: frontierT7TestDigest("checkpoint-legacy"), LifecycleReceiptDigest: frontierT7TestDigest("lifecycle-legacy"), RepositoryConstraintDigest: frontierT7TestDigest("repo-legacy"), DestinationSessionDigest: frontierT7TestDigest("destination-legacy"), RecipientKeyID: identity.MeshKeyID, Grant: grant, Transport: "context-mesh", ExpiresAt: now.Add(30 * time.Minute),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.recordCreatedManifest(manifest, now); err != nil {
		t.Fatal(err)
	}
	if err := store.recordCreatedManifest(legacyManifest, now); err != nil {
		t.Fatal(err)
	}
	status := store.snapshot(now)
	if anyToInt(status["evidence_identity_bound_manifests"], 0) != 1 || anyToInt(status["evidence_identity_legacy_manifests"], 0) != 1 {
		t.Fatalf("status did not retain bound/legacy evidence identity counts: %#v", status)
	}
	tampered := manifest
	tampered.EvidenceIdentityDigest = frontierT7TestDigest("other-scope")
	if err := frontierT7ValidateContinuationManifest(tampered, now); err == nil {
		t.Fatal("cross-scope evidence identity mutation passed signed manifest validation")
	}
	if _, err := frontierT7CreateContinuationManifest(identity, frontierT7ContinuationRequest{Project: "contextlattice", PassportID: "passport-u6-invalid", PassportDigest: frontierT7TestDigest("passport-invalid"), EvidenceIdentityDigest: "not-a-digest", LineageDigest: frontierT7TestDigest("lineage-invalid"), CheckpointDigest: frontierT7TestDigest("checkpoint-invalid"), LifecycleReceiptDigest: frontierT7TestDigest("lifecycle-invalid"), RepositoryConstraintDigest: frontierT7TestDigest("repo-invalid"), DestinationSessionDigest: frontierT7TestDigest("destination-invalid"), RecipientKeyID: identity.MeshKeyID, Grant: grant, Transport: "context-mesh", ExpiresAt: now.Add(30 * time.Minute)}, now); err == nil {
		t.Fatal("malformed evidence identity digest was accepted")
	}
}
