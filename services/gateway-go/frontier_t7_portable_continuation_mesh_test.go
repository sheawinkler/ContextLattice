package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type frontierT7ContinuationMeshFixture struct {
	now          time.Time
	senderMesh   *contextMeshStore
	receiverMesh *contextMeshStore
	wrongMesh    *contextMeshStore
	meshGrant    contextMeshGrant
	grant        frontierT7CollaborativeGrant
	manifest     frontierT7ContinuationManifest
}

func newFrontierT7ContinuationMeshFixture(t testing.TB) frontierT7ContinuationMeshFixture {
	t.Helper()
	senderRoot := t.TempDir()
	receiverRoot := t.TempDir()
	wrongRoot := t.TempDir()
	senderPassports := newTestPassportStore(t, senderRoot)
	receiverPassports := newTestPassportStore(t, receiverRoot)
	wrongPassports := newTestPassportStore(t, wrongRoot)
	senderMesh := newTestMeshStore(t, senderRoot, senderPassports)
	receiverMesh := newTestMeshStore(t, receiverRoot, receiverPassports)
	wrongMesh := newTestMeshStore(t, wrongRoot, wrongPassports)
	now := time.Now().UTC()
	meshGrant, err := senderMesh.createGrant(map[string]any{
		"recipient_id": "frontier-t7-receiver", "recipient": receiverMesh.identity.MeshRecipient,
		"project": "contextlattice", "ttl_secs": 3600,
	})
	if err != nil {
		t.Fatalf("create mesh grant: %v", err)
	}
	grant, manifest := frontierT7ContinuationMeshArtifacts(t, senderMesh.identity, "contextlattice", receiverMesh.identity.MeshKeyID, now)
	return frontierT7ContinuationMeshFixture{
		now: now, senderMesh: senderMesh, receiverMesh: receiverMesh, wrongMesh: wrongMesh,
		meshGrant: meshGrant, grant: grant, manifest: manifest,
	}
}

func frontierT7ContinuationMeshArtifacts(t testing.TB, keys *contextIdentityKeys, project, recipientKeyID string, now time.Time) (frontierT7CollaborativeGrant, frontierT7ContinuationManifest) {
	t.Helper()
	grant, err := frontierT7CreateCollaborativeGrant(keys, frontierT7GrantCreateRequest{
		Subject: frontierT7GrantSubject{
			SubjectID: "frontier-t7-agent", Roles: []string{"reviewer"}, WorkspaceID: "frontier-t7-workspace",
			SnapshotDigest: frontierT7TestDigest("frontier-t7-subject"), ObservedAt: now.Format(time.RFC3339Nano),
		},
		Project: project, Topics: []string{"frontier-30"}, DataClasses: []string{"checkpoint"},
		Actions: []string{"continue", "read"}, Purpose: "continue-reviewed-work", UsageLimit: 3,
		Approvers: []string{"owner"}, KeyEpoch: 1, RecipientKeyID: recipientKeyID,
		NotBefore: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
	}, now)
	if err != nil {
		t.Fatalf("create collaborative grant: %v", err)
	}
	manifest, err := frontierT7CreateContinuationManifest(keys, frontierT7ContinuationRequest{
		Project: project, PassportID: "passport-frontier-t7", PassportDigest: frontierT7TestDigest("frontier-t7-passport"),
		LineageDigest: frontierT7TestDigest("frontier-t7-lineage"), CheckpointDigest: frontierT7TestDigest("frontier-t7-checkpoint"),
		LifecycleReceiptDigest:      frontierT7TestDigest("frontier-t7-lifecycle"),
		UnresolvedObligationDigests: []string{frontierT7TestDigest("frontier-t7-obligation")},
		RepositoryConstraintDigest:  frontierT7TestDigest("frontier-t7-repository"),
		DestinationSessionDigest:    frontierT7TestDigest("frontier-t7-destination"),
		RecipientKeyID:              recipientKeyID, Grant: grant, Transport: "operator-chosen", ExpiresAt: now.Add(30 * time.Minute),
	}, now)
	if err != nil {
		t.Fatalf("create continuation manifest: %v", err)
	}
	return grant, manifest
}

func frontierT7ContinuationMeshEnvelope(t testing.TB, fixture frontierT7ContinuationMeshFixture) frontierT7ContinuationEnvelope {
	t.Helper()
	envelope, err := frontierT7CreateContinuationEnvelope(
		fixture.senderMesh, fixture.manifest, fixture.grant, fixture.meshGrant.GrantID, fixture.now,
	)
	if err != nil {
		t.Fatalf("create continuation envelope: %v", err)
	}
	return envelope
}

func frontierT7TamperEnvelopeText(value string) string {
	if value == "" {
		return "A"
	}
	index := len(value) / 2
	replacement := byte('A')
	if value[index] == replacement {
		replacement = 'B'
	}
	return value[:index] + string(replacement) + value[index+1:]
}

func TestFrontierT7ContinuationMeshRoundTrip(t *testing.T) {
	fixture := newFrontierT7ContinuationMeshFixture(t)
	envelope := frontierT7ContinuationMeshEnvelope(t, fixture)
	if envelope.SchemaID != frontierT7ContinuationEnvelopeSchemaID || envelope.Version != 1 ||
		envelope.NetworkCalls != 0 || envelope.DeliveryPerformed || envelope.PrivateKeyExported ||
		envelope.OrdinaryMemoryMutated || envelope.TransportOwner != frontierT7ContinuationTransportOwner ||
		!frontierT7ValidContinuationEncryptionProfile(envelope.Encryption) {
		t.Fatalf("unexpected continuation envelope controls: %#v", envelope)
	}

	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		fixture.senderMesh.identity.SigningPrivateKey,
		fixture.senderMesh.identity.MeshIdentity,
		fixture.receiverMesh.identity.MeshIdentity,
		"AGE-SECRET-KEY",
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("continuation envelope leaked private key material: %q", forbidden)
		}
	}
	var value any
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeFrontierT7ContinuationEnvelope(value)
	if err != nil {
		t.Fatalf("strict decode continuation envelope: %v", err)
	}
	manifest, grant, err := frontierT7DecryptContinuationEnvelope(fixture.receiverMesh, decoded, fixture.now)
	if err != nil {
		t.Fatalf("decrypt continuation envelope: %v", err)
	}
	if manifest.ManifestDigest != fixture.manifest.ManifestDigest || grant.GrantDigest != fixture.grant.GrantDigest {
		t.Fatalf("decrypted continuation mismatch: manifest=%#v grant=%#v", manifest, grant)
	}
}

func TestFrontierT7ContinuationMeshRejectsWrongRecipient(t *testing.T) {
	fixture := newFrontierT7ContinuationMeshFixture(t)
	envelope := frontierT7ContinuationMeshEnvelope(t, fixture)
	if _, _, err := frontierT7DecryptContinuationEnvelope(fixture.wrongMesh, envelope, fixture.now); err == nil || !strings.Contains(err.Error(), "wrong continuation recipient") {
		t.Fatalf("wrong recipient error = %v", err)
	}
}

func TestFrontierT7ContinuationMeshRejectsTamper(t *testing.T) {
	fixture := newFrontierT7ContinuationMeshFixture(t)
	envelope := frontierT7ContinuationMeshEnvelope(t, fixture)
	t.Run("ciphertext", func(t *testing.T) {
		tampered := envelope
		tampered.Ciphertext = frontierT7TamperEnvelopeText(tampered.Ciphertext)
		if _, _, err := frontierT7DecryptContinuationEnvelope(fixture.receiverMesh, tampered, fixture.now); err == nil {
			t.Fatal("tampered continuation ciphertext was accepted")
		}
	})
	t.Run("signature", func(t *testing.T) {
		tampered := envelope
		tampered.Signature.Value = frontierT7TamperEnvelopeText(tampered.Signature.Value)
		if _, _, err := frontierT7DecryptContinuationEnvelope(fixture.receiverMesh, tampered, fixture.now); err == nil {
			t.Fatal("tampered continuation signature was accepted")
		}
	})
}

func TestFrontierT7ContinuationMeshRejectsExpiredEnvelope(t *testing.T) {
	fixture := newFrontierT7ContinuationMeshFixture(t)
	envelope := frontierT7ContinuationMeshEnvelope(t, fixture)
	expires, err := time.Parse(time.RFC3339Nano, envelope.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := frontierT7DecryptContinuationEnvelope(fixture.receiverMesh, envelope, expires); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired envelope error = %v", err)
	}
}

func TestFrontierT7ContinuationMeshRejectsRevokedMeshGrant(t *testing.T) {
	fixture := newFrontierT7ContinuationMeshFixture(t)
	envelope := frontierT7ContinuationMeshEnvelope(t, fixture)
	if _, _, err := fixture.receiverMesh.revokeGrant(fixture.meshGrant.GrantID, "destination denylist"); err != nil {
		t.Fatalf("revoke mesh grant locally: %v", err)
	}
	if _, _, err := frontierT7DecryptContinuationEnvelope(fixture.receiverMesh, envelope, fixture.now); err == nil || !strings.Contains(err.Error(), "locally revoked") {
		t.Fatalf("revoked mesh grant error = %v", err)
	}
}

func TestFrontierT7ContinuationMeshRejectsMismatchedT7Scope(t *testing.T) {
	fixture := newFrontierT7ContinuationMeshFixture(t)
	t.Run("project", func(t *testing.T) {
		grant, manifest := frontierT7ContinuationMeshArtifacts(t, fixture.senderMesh.identity, "different-project", fixture.receiverMesh.identity.MeshKeyID, fixture.now)
		if _, err := frontierT7CreateContinuationEnvelope(fixture.senderMesh, manifest, grant, fixture.meshGrant.GrantID, fixture.now); err == nil || !strings.Contains(err.Error(), "project") {
			t.Fatalf("mismatched T7 project error = %v", err)
		}
	})
	t.Run("recipient", func(t *testing.T) {
		grant, manifest := frontierT7ContinuationMeshArtifacts(t, fixture.senderMesh.identity, "contextlattice", fixture.wrongMesh.identity.MeshKeyID, fixture.now)
		if _, err := frontierT7CreateContinuationEnvelope(fixture.senderMesh, manifest, grant, fixture.meshGrant.GrantID, fixture.now); err == nil || !strings.Contains(err.Error(), "recipient") {
			t.Fatalf("mismatched T7 recipient error = %v", err)
		}
	})
}

func TestFrontierT7ContinuationMeshRejectsThirdPartyRepackaging(t *testing.T) {
	fixture := newFrontierT7ContinuationMeshFixture(t)
	thirdPartyRoot := t.TempDir()
	thirdPartyPassports := newTestPassportStore(t, thirdPartyRoot)
	thirdPartyMesh := newTestMeshStore(t, thirdPartyRoot, thirdPartyPassports)
	meshGrant, err := thirdPartyMesh.createGrant(map[string]any{
		"recipient_id": "frontier-t7-receiver", "recipient": fixture.receiverMesh.identity.MeshRecipient,
		"project": "contextlattice", "ttl_secs": 3600,
	})
	if err != nil {
		t.Fatalf("create third-party Mesh grant: %v", err)
	}
	if _, err := frontierT7CreateContinuationEnvelope(thirdPartyMesh, fixture.manifest, fixture.grant, meshGrant.GrantID, fixture.now); err == nil || !strings.Contains(err.Error(), "sender and issuer") {
		t.Fatalf("third-party repackaging error = %v", err)
	}
}

func TestFrontierT7ContinuationMeshStrictDecoderRejectsUnknownFields(t *testing.T) {
	fixture := newFrontierT7ContinuationMeshFixture(t)
	envelope := frontierT7ContinuationMeshEnvelope(t, fixture)
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	value["unexpected_field"] = true
	if _, err := decodeFrontierT7ContinuationEnvelope(value); err == nil {
		t.Fatal("strict continuation envelope decoder accepted an unknown field")
	}
}
