package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"filippo.io/age"
)

const (
	frontierT7ContinuationPayloadSchemaID    = "context_continuation_payload.v1"
	frontierT7ContinuationEnvelopeSchemaID   = "context_continuation_envelope.v1"
	frontierT7ContinuationTransportOwner     = "external_adapter"
	frontierT7ContinuationMaxPlaintextBytes  = 256 * 1024
	frontierT7ContinuationMaxCiphertextBytes = 512 * 1024
)

type frontierT7ContinuationPayload struct {
	SchemaID                       string                         `json:"schema_id"`
	Version                        int                            `json:"version"`
	EnvelopeID                     string                         `json:"envelope_id"`
	Project                        string                         `json:"project"`
	CreatedAt                      string                         `json:"created_at"`
	ExpiresAt                      string                         `json:"expires_at"`
	Sender                         contextMeshSender              `json:"sender"`
	MeshGrantID                    string                         `json:"mesh_grant_id"`
	RecipientKeyID                 string                         `json:"recipient_key_id"`
	Manifest                       frontierT7ContinuationManifest `json:"manifest"`
	Grant                          frontierT7CollaborativeGrant   `json:"grant"`
	ManifestDigest                 string                         `json:"manifest_digest"`
	GrantDigest                    string                         `json:"grant_digest"`
	PayloadDigest                  string                         `json:"payload_digest"`
	Signature                      contextPassportSignature       `json:"signature"`
	Bounded                        bool                           `json:"bounded"`
	Redacted                       bool                           `json:"redacted"`
	MeshEncryptionRequired         bool                           `json:"mesh_encryption_required"`
	PlaintextExported              bool                           `json:"plaintext_exported"`
	DeliveryPerformed              bool                           `json:"delivery_performed"`
	TransportOwnedByContextLattice bool                           `json:"transport_owned_by_contextlattice"`
	PrivateKeyExported             bool                           `json:"private_key_exported"`
	NetworkCalls                   int                            `json:"network_calls"`
}

type frontierT7ContinuationPayloadUnsigned struct {
	SchemaID                       string                         `json:"schema_id"`
	Version                        int                            `json:"version"`
	EnvelopeID                     string                         `json:"envelope_id"`
	Project                        string                         `json:"project"`
	CreatedAt                      string                         `json:"created_at"`
	ExpiresAt                      string                         `json:"expires_at"`
	Sender                         contextMeshSender              `json:"sender"`
	MeshGrantID                    string                         `json:"mesh_grant_id"`
	RecipientKeyID                 string                         `json:"recipient_key_id"`
	Manifest                       frontierT7ContinuationManifest `json:"manifest"`
	Grant                          frontierT7CollaborativeGrant   `json:"grant"`
	ManifestDigest                 string                         `json:"manifest_digest"`
	GrantDigest                    string                         `json:"grant_digest"`
	Bounded                        bool                           `json:"bounded"`
	Redacted                       bool                           `json:"redacted"`
	MeshEncryptionRequired         bool                           `json:"mesh_encryption_required"`
	PlaintextExported              bool                           `json:"plaintext_exported"`
	DeliveryPerformed              bool                           `json:"delivery_performed"`
	TransportOwnedByContextLattice bool                           `json:"transport_owned_by_contextlattice"`
	PrivateKeyExported             bool                           `json:"private_key_exported"`
	NetworkCalls                   int                            `json:"network_calls"`
}

type frontierT7ContinuationEnvelope struct {
	SchemaID              string                   `json:"schema_id"`
	Version               int                      `json:"version"`
	EnvelopeID            string                   `json:"envelope_id"`
	Project               string                   `json:"project"`
	ManifestID            string                   `json:"manifest_id"`
	ManifestDigest        string                   `json:"manifest_digest"`
	GrantID               string                   `json:"grant_id"`
	GrantDigest           string                   `json:"grant_digest"`
	MeshGrantID           string                   `json:"mesh_grant_id"`
	RecipientKeyID        string                   `json:"recipient_key_id"`
	CreatedAt             string                   `json:"created_at"`
	ExpiresAt             string                   `json:"expires_at"`
	Sender                contextMeshSender        `json:"sender"`
	Encryption            map[string]any           `json:"encryption"`
	Ciphertext            string                   `json:"ciphertext"`
	CiphertextBytes       int                      `json:"ciphertext_bytes"`
	CiphertextDigest      string                   `json:"ciphertext_digest"`
	EnvelopeDigest        string                   `json:"envelope_digest"`
	Signature             contextPassportSignature `json:"signature"`
	NetworkCalls          int                      `json:"network_calls"`
	DeliveryPerformed     bool                     `json:"delivery_performed"`
	TransportOwner        string                   `json:"transport_owner"`
	PrivateKeyExported    bool                     `json:"private_key_exported"`
	OrdinaryMemoryMutated bool                     `json:"ordinary_memory_mutated"`
}

type frontierT7ContinuationEnvelopeUnsigned struct {
	SchemaID              string            `json:"schema_id"`
	Version               int               `json:"version"`
	EnvelopeID            string            `json:"envelope_id"`
	Project               string            `json:"project"`
	ManifestID            string            `json:"manifest_id"`
	ManifestDigest        string            `json:"manifest_digest"`
	GrantID               string            `json:"grant_id"`
	GrantDigest           string            `json:"grant_digest"`
	MeshGrantID           string            `json:"mesh_grant_id"`
	RecipientKeyID        string            `json:"recipient_key_id"`
	CreatedAt             string            `json:"created_at"`
	ExpiresAt             string            `json:"expires_at"`
	Sender                contextMeshSender `json:"sender"`
	Encryption            map[string]any    `json:"encryption"`
	Ciphertext            string            `json:"ciphertext"`
	CiphertextBytes       int               `json:"ciphertext_bytes"`
	CiphertextDigest      string            `json:"ciphertext_digest"`
	NetworkCalls          int               `json:"network_calls"`
	DeliveryPerformed     bool              `json:"delivery_performed"`
	TransportOwner        string            `json:"transport_owner"`
	PrivateKeyExported    bool              `json:"private_key_exported"`
	OrdinaryMemoryMutated bool              `json:"ordinary_memory_mutated"`
}

func frontierT7ContinuationPayloadUnsignedValue(payload frontierT7ContinuationPayload) frontierT7ContinuationPayloadUnsigned {
	return frontierT7ContinuationPayloadUnsigned{
		SchemaID: payload.SchemaID, Version: payload.Version, EnvelopeID: payload.EnvelopeID,
		Project: payload.Project, CreatedAt: payload.CreatedAt, ExpiresAt: payload.ExpiresAt,
		Sender: payload.Sender, MeshGrantID: payload.MeshGrantID, RecipientKeyID: payload.RecipientKeyID,
		Manifest: payload.Manifest, Grant: payload.Grant,
		ManifestDigest: payload.ManifestDigest, GrantDigest: payload.GrantDigest,
		Bounded: payload.Bounded, Redacted: payload.Redacted, MeshEncryptionRequired: payload.MeshEncryptionRequired,
		PlaintextExported: payload.PlaintextExported, DeliveryPerformed: payload.DeliveryPerformed,
		TransportOwnedByContextLattice: payload.TransportOwnedByContextLattice,
		PrivateKeyExported:             payload.PrivateKeyExported, NetworkCalls: payload.NetworkCalls,
	}
}

func frontierT7ContinuationEnvelopeUnsignedValue(envelope frontierT7ContinuationEnvelope) frontierT7ContinuationEnvelopeUnsigned {
	return frontierT7ContinuationEnvelopeUnsigned{
		SchemaID: envelope.SchemaID, Version: envelope.Version, EnvelopeID: envelope.EnvelopeID,
		Project: envelope.Project, ManifestID: envelope.ManifestID, ManifestDigest: envelope.ManifestDigest,
		GrantID: envelope.GrantID, GrantDigest: envelope.GrantDigest, MeshGrantID: envelope.MeshGrantID,
		RecipientKeyID: envelope.RecipientKeyID, CreatedAt: envelope.CreatedAt, ExpiresAt: envelope.ExpiresAt,
		Sender: envelope.Sender, Encryption: envelope.Encryption, Ciphertext: envelope.Ciphertext,
		CiphertextBytes: envelope.CiphertextBytes, CiphertextDigest: envelope.CiphertextDigest,
		NetworkCalls: envelope.NetworkCalls, DeliveryPerformed: envelope.DeliveryPerformed,
		TransportOwner: envelope.TransportOwner, PrivateKeyExported: envelope.PrivateKeyExported,
		OrdinaryMemoryMutated: envelope.OrdinaryMemoryMutated,
	}
}

func frontierT7SignContinuationPayload(payload *frontierT7ContinuationPayload, keys *contextIdentityKeys) error {
	if payload == nil || keys == nil {
		return errors.New("continuation payload and signing identity are required")
	}
	unsigned := frontierT7ContinuationPayloadUnsignedValue(*payload)
	payload.PayloadDigest = frontierT7Digest(unsigned)
	signature, err := signBytesWithIdentity(struct {
		PayloadDigest string                                `json:"payload_digest"`
		Payload       frontierT7ContinuationPayloadUnsigned `json:"payload"`
	}{PayloadDigest: payload.PayloadDigest, Payload: unsigned}, keys)
	if err != nil {
		return err
	}
	payload.Signature = signature
	return nil
}

func frontierT7SignContinuationEnvelope(envelope *frontierT7ContinuationEnvelope, keys *contextIdentityKeys) error {
	if envelope == nil || keys == nil {
		return errors.New("continuation envelope and signing identity are required")
	}
	unsigned := frontierT7ContinuationEnvelopeUnsignedValue(*envelope)
	envelope.EnvelopeDigest = frontierT7Digest(unsigned)
	signature, err := signBytesWithIdentity(struct {
		EnvelopeDigest string                                 `json:"envelope_digest"`
		Envelope       frontierT7ContinuationEnvelopeUnsigned `json:"envelope"`
	}{EnvelopeDigest: envelope.EnvelopeDigest, Envelope: unsigned}, keys)
	if err != nil {
		return err
	}
	envelope.Signature = signature
	return nil
}

func frontierT7ValidateContinuationMesh(mesh *contextMeshStore) error {
	if mesh == nil || !mesh.enabled || mesh.identity == nil {
		return errors.New("context mesh disabled")
	}
	if mesh.maxPlaintextBytes <= 0 || mesh.maxEnvelopeBytes <= 0 {
		return errors.New("context mesh envelope limits are invalid")
	}
	if err := validateContextIdentity(mesh.identity); err != nil {
		return fmt.Errorf("context mesh identity invalid: %w", err)
	}
	return nil
}

func frontierT7ContinuationPlaintextLimit(mesh *contextMeshStore) int {
	return minInt(mesh.maxPlaintextBytes, frontierT7ContinuationMaxPlaintextBytes)
}

func frontierT7ContinuationCiphertextLimit(mesh *contextMeshStore) int {
	return minInt(mesh.maxEnvelopeBytes, frontierT7ContinuationMaxCiphertextBytes)
}

func frontierT7ValidateContinuationManifest(manifest frontierT7ContinuationManifest, now time.Time) error {
	if manifest.SchemaID != frontierT7ContinuationSchemaID || manifest.Version != 1 {
		return errors.New("unsupported continuation manifest")
	}
	if _, err := frontierT7SafeID(manifest.ManifestID, "manifest_id", 200); err != nil {
		return err
	}
	project, err := sanitizeMemoryProject(manifest.Project)
	if err != nil || project != manifest.Project {
		return errors.New("continuation manifest project is not canonical")
	}
	for _, field := range []struct {
		value string
		name  string
		limit int
	}{
		{manifest.PassportID, "passport_id", 200},
		{manifest.RecipientKeyID, "recipient_key_id", 200},
		{manifest.GrantID, "grant_id", 200},
		{manifest.Transport, "transport", 80},
	} {
		if _, err := frontierT7SafeID(field.value, field.name, field.limit); err != nil {
			return err
		}
	}
	digests := []string{
		manifest.PassportDigest, manifest.LineageDigest, manifest.CheckpointDigest,
		manifest.LifecycleReceiptDigest, manifest.RepositoryConstraintDigest,
		manifest.DestinationSessionDigest, manifest.GrantDigest,
	}
	if strings.TrimSpace(manifest.EvidenceIdentityDigest) != "" && !frontierT7ValidDigest(manifest.EvidenceIdentityDigest) {
		return errors.New("continuation manifest evidence identity digest is invalid")
	}
	if len(manifest.UnresolvedObligationDigests) > frontierT7MaxObligations {
		return errors.New("continuation manifest has too many unresolved obligations")
	}
	digests = append(digests, manifest.UnresolvedObligationDigests...)
	for _, digest := range digests {
		if !frontierT7ValidDigest(digest) {
			return errors.New("continuation manifest contains an invalid digest")
		}
	}
	unsigned := frontierT7ContinuationUnsignedValue(manifest)
	if manifest.ManifestDigest != frontierT7Digest(unsigned) {
		return errors.New("continuation manifest digest mismatch")
	}
	if !verifySignedBytes(struct {
		ManifestDigest string                         `json:"manifest_digest"`
		Manifest       frontierT7ContinuationUnsigned `json:"manifest"`
	}{ManifestDigest: manifest.ManifestDigest, Manifest: unsigned}, manifest.Signature, manifest.Issuer) {
		return errors.New("continuation manifest signature invalid")
	}
	created, createdErr := time.Parse(time.RFC3339Nano, manifest.CreatedAt)
	expires, expiresErr := time.Parse(time.RFC3339Nano, manifest.ExpiresAt)
	if createdErr != nil || expiresErr != nil || !expires.After(created) || expires.Sub(created) > 24*time.Hour {
		return errors.New("continuation manifest time bounds invalid")
	}
	if created.After(now.Add(2 * time.Minute)) {
		return errors.New("continuation manifest creation time is in the future")
	}
	if !now.Before(expires) {
		return errors.New("continuation manifest expired")
	}
	return nil
}

func frontierT7ValidateContinuationGrant(grant frontierT7CollaborativeGrant, now time.Time) error {
	if findings := frontierT7VerifyGrant(grant); len(findings) > 0 {
		return fmt.Errorf("collaborative grant invalid: %s", strings.Join(findings, ","))
	}
	created, createdErr := time.Parse(time.RFC3339Nano, grant.CreatedAt)
	notBefore, beforeErr := time.Parse(time.RFC3339Nano, grant.NotBefore)
	expires, expiresErr := time.Parse(time.RFC3339Nano, grant.ExpiresAt)
	if createdErr != nil || beforeErr != nil || expiresErr != nil {
		return errors.New("collaborative grant time bounds invalid")
	}
	if created.After(now.Add(2 * time.Minute)) {
		return errors.New("collaborative grant creation time is in the future")
	}
	if now.Before(notBefore) {
		return errors.New("collaborative grant is not active yet")
	}
	if !now.Before(expires) {
		return errors.New("collaborative grant expired")
	}
	if !containsString(grant.Actions, "continue") {
		return errors.New("collaborative grant does not authorize continuation")
	}
	return nil
}

func frontierT7ValidateContinuationArtifacts(manifest frontierT7ContinuationManifest, grant frontierT7CollaborativeGrant, now time.Time) error {
	if err := frontierT7ValidateContinuationManifest(manifest, now); err != nil {
		return err
	}
	if err := frontierT7ValidateContinuationGrant(grant, now); err != nil {
		return err
	}
	if manifest.Project != grant.Project {
		return errors.New("continuation project binding mismatch")
	}
	if manifest.RecipientKeyID != grant.RecipientKeyID {
		return errors.New("continuation recipient binding mismatch")
	}
	if manifest.GrantID != grant.GrantID || manifest.GrantDigest != grant.GrantDigest {
		return errors.New("continuation collaborative grant binding mismatch")
	}
	if manifest.Issuer != grant.Issuer {
		return errors.New("continuation issuer binding mismatch")
	}
	return nil
}

func frontierT7SenderMatchesIssuer(sender contextMeshSender, issuer contextPassportIssuer) bool {
	return sender.InstanceID == issuer.InstanceID && sender.SigningKeyID == issuer.SigningKeyID && sender.SigningPublicKey == issuer.SigningPublicKey
}

func frontierT7ValidateContinuationSender(sender contextMeshSender) error {
	for _, field := range []struct {
		value string
		name  string
		limit int
	}{
		{sender.InstanceID, "sender_instance_id", 160},
		{sender.SigningKeyID, "sender_signing_key_id", 200},
		{sender.SigningPublicKey, "sender_signing_public_key", 160},
		{sender.MeshKeyID, "sender_mesh_key_id", 200},
		{sender.MeshRecipient, "sender_mesh_recipient", 200},
	} {
		if _, err := frontierT7SafeID(field.value, field.name, field.limit); err != nil {
			return err
		}
	}
	recipient, err := age.ParseX25519Recipient(sender.MeshRecipient)
	if err != nil || recipient.String() != sender.MeshRecipient {
		return errors.New("sender mesh recipient is not canonical X25519")
	}
	if sender.MeshKeyID != "age-x25519:"+digestPrefix(sender.MeshRecipient, 24) {
		return errors.New("sender mesh key id mismatch")
	}
	if sender.SigningKeyID != "ed25519:"+digestPrefix(sender.SigningPublicKey, 24) {
		return errors.New("sender signing key id mismatch")
	}
	return nil
}

func frontierT7ContinuationEncryptionProfile() map[string]any {
	return map[string]any{
		"format":          "age-encryption.org/v1",
		"recipient_type":  "X25519",
		"key_agreement":   "X25519",
		"payload_cipher":  "ChaCha20-Poly1305",
		"multi_recipient": false,
	}
}

func frontierT7ContinuationCiphertextDigest(ciphertext []byte) string {
	digest := sha256.Sum256(ciphertext)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func frontierT7ValidContinuationEncryptionProfile(profile map[string]any) bool {
	if len(profile) != 5 {
		return false
	}
	format, formatOK := profile["format"].(string)
	recipientType, recipientTypeOK := profile["recipient_type"].(string)
	keyAgreement, keyAgreementOK := profile["key_agreement"].(string)
	payloadCipher, payloadCipherOK := profile["payload_cipher"].(string)
	multiRecipient, multiRecipientOK := profile["multi_recipient"].(bool)
	return formatOK && recipientTypeOK && keyAgreementOK && payloadCipherOK && multiRecipientOK &&
		format == "age-encryption.org/v1" && recipientType == "X25519" && keyAgreement == "X25519" &&
		payloadCipher == "ChaCha20-Poly1305" && !multiRecipient
}

func frontierT7ContinuationExpiry(manifest frontierT7ContinuationManifest, grant frontierT7CollaborativeGrant, meshGrant contextMeshGrant) (time.Time, error) {
	manifestExpiry, manifestErr := time.Parse(time.RFC3339Nano, manifest.ExpiresAt)
	grantExpiry, grantErr := time.Parse(time.RFC3339Nano, grant.ExpiresAt)
	meshExpiry, meshErr := time.Parse(time.RFC3339Nano, meshGrant.ExpiresAt)
	if manifestErr != nil || grantErr != nil || meshErr != nil {
		return time.Time{}, errors.New("continuation expiry binding is invalid")
	}
	expires := manifestExpiry
	if grantExpiry.Before(expires) {
		expires = grantExpiry
	}
	if meshExpiry.Before(expires) {
		expires = meshExpiry
	}
	return expires.UTC(), nil
}

func frontierT7CreateContinuationEnvelope(mesh *contextMeshStore, manifest frontierT7ContinuationManifest, grant frontierT7CollaborativeGrant, meshGrantID string, now time.Time) (frontierT7ContinuationEnvelope, error) {
	if now.IsZero() {
		return frontierT7ContinuationEnvelope{}, errors.New("current time is required")
	}
	now = now.UTC()
	if err := frontierT7ValidateContinuationMesh(mesh); err != nil {
		return frontierT7ContinuationEnvelope{}, err
	}
	if err := frontierT7ValidateContinuationArtifacts(manifest, grant, now); err != nil {
		return frontierT7ContinuationEnvelope{}, err
	}
	if !frontierT7SenderMatchesIssuer(meshSender(mesh.identity), manifest.Issuer) {
		return frontierT7ContinuationEnvelope{}, errors.New("continuation sender and issuer binding mismatch")
	}
	meshGrantID, err := frontierT7SafeID(meshGrantID, "mesh_grant_id", 200)
	if err != nil {
		return frontierT7ContinuationEnvelope{}, err
	}
	meshGrant, err := mesh.activeGrant(meshGrantID, manifest.Project, now)
	if err != nil {
		return frontierT7ContinuationEnvelope{}, fmt.Errorf("context mesh grant invalid: %w", err)
	}
	if meshGrant.Issuer.InstanceID != mesh.identity.InstanceID ||
		meshGrant.Issuer.SigningKeyID != mesh.identity.SigningKeyID ||
		meshGrant.Issuer.SigningPublicKey != mesh.identity.SigningPublicKey {
		return frontierT7ContinuationEnvelope{}, errors.New("context mesh grant issuer does not match envelope sender")
	}
	if meshGrant.RecipientKeyID != manifest.RecipientKeyID {
		return frontierT7ContinuationEnvelope{}, errors.New("context mesh recipient binding mismatch")
	}
	recipient, err := age.ParseX25519Recipient(meshGrant.Recipient)
	if err != nil || recipient.String() != meshGrant.Recipient {
		return frontierT7ContinuationEnvelope{}, errors.New("context mesh recipient is not canonical X25519")
	}
	expires, err := frontierT7ContinuationExpiry(manifest, grant, meshGrant)
	if err != nil {
		return frontierT7ContinuationEnvelope{}, err
	}
	if !now.Before(expires) {
		return frontierT7ContinuationEnvelope{}, errors.New("continuation envelope expiry is not in the future")
	}

	createdAt := now.Format(time.RFC3339Nano)
	expiresAt := expires.Format(time.RFC3339Nano)
	envelopeID := "contenvelope_" + randomHex(16)
	payload := frontierT7ContinuationPayload{
		SchemaID: frontierT7ContinuationPayloadSchemaID, Version: 1, EnvelopeID: envelopeID,
		Project: manifest.Project, CreatedAt: createdAt, ExpiresAt: expiresAt,
		Sender: meshSender(mesh.identity), MeshGrantID: meshGrant.GrantID, RecipientKeyID: meshGrant.RecipientKeyID,
		Manifest: manifest, Grant: grant, ManifestDigest: manifest.ManifestDigest, GrantDigest: grant.GrantDigest,
		Bounded: true, Redacted: true, MeshEncryptionRequired: true, PlaintextExported: false,
		DeliveryPerformed: false, TransportOwnedByContextLattice: false, PrivateKeyExported: false, NetworkCalls: 0,
	}
	if err := frontierT7SignContinuationPayload(&payload, mesh.identity); err != nil {
		return frontierT7ContinuationEnvelope{}, err
	}
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return frontierT7ContinuationEnvelope{}, err
	}
	plaintextLimit := frontierT7ContinuationPlaintextLimit(mesh)
	if len(plaintext) > plaintextLimit {
		return frontierT7ContinuationEnvelope{}, fmt.Errorf("continuation payload exceeds %d byte limit", plaintextLimit)
	}

	var encrypted bytes.Buffer
	writer, err := age.Encrypt(&encrypted, recipient)
	if err != nil {
		return frontierT7ContinuationEnvelope{}, err
	}
	written, writeErr := writer.Write(plaintext)
	if writeErr != nil || written != len(plaintext) {
		_ = writer.Close()
		if writeErr != nil {
			return frontierT7ContinuationEnvelope{}, writeErr
		}
		return frontierT7ContinuationEnvelope{}, io.ErrShortWrite
	}
	if err := writer.Close(); err != nil {
		return frontierT7ContinuationEnvelope{}, err
	}
	ciphertextLimit := frontierT7ContinuationCiphertextLimit(mesh)
	if encrypted.Len() > ciphertextLimit {
		return frontierT7ContinuationEnvelope{}, fmt.Errorf("continuation ciphertext exceeds %d byte limit", ciphertextLimit)
	}
	ciphertext := encrypted.Bytes()
	envelope := frontierT7ContinuationEnvelope{
		SchemaID: frontierT7ContinuationEnvelopeSchemaID, Version: 1, EnvelopeID: envelopeID,
		Project: manifest.Project, ManifestID: manifest.ManifestID, ManifestDigest: manifest.ManifestDigest,
		GrantID: grant.GrantID, GrantDigest: grant.GrantDigest, MeshGrantID: meshGrant.GrantID,
		RecipientKeyID: meshGrant.RecipientKeyID, CreatedAt: createdAt, ExpiresAt: expiresAt,
		Sender: meshSender(mesh.identity), Encryption: frontierT7ContinuationEncryptionProfile(),
		Ciphertext: base64.RawStdEncoding.EncodeToString(ciphertext), CiphertextBytes: len(ciphertext),
		CiphertextDigest: frontierT7ContinuationCiphertextDigest(ciphertext), NetworkCalls: 0, DeliveryPerformed: false,
		TransportOwner: frontierT7ContinuationTransportOwner, PrivateKeyExported: false,
		OrdinaryMemoryMutated: false,
	}
	if err := frontierT7SignContinuationEnvelope(&envelope, mesh.identity); err != nil {
		return frontierT7ContinuationEnvelope{}, err
	}
	return envelope, nil
}

func frontierT7VerifyContinuationEnvelope(envelope frontierT7ContinuationEnvelope, now time.Time, maxEnvelopeBytes int) ([]byte, error) {
	if envelope.SchemaID != frontierT7ContinuationEnvelopeSchemaID || envelope.Version != 1 {
		return nil, errors.New("unsupported continuation envelope")
	}
	if maxEnvelopeBytes <= 0 || envelope.CiphertextBytes <= 0 || envelope.CiphertextBytes > maxEnvelopeBytes {
		return nil, errors.New("continuation ciphertext size invalid")
	}
	if len(envelope.Ciphertext) > base64.RawStdEncoding.EncodedLen(maxEnvelopeBytes) {
		return nil, errors.New("continuation ciphertext encoding exceeds limit")
	}
	if _, err := frontierT7SafeID(envelope.EnvelopeID, "envelope_id", 200); err != nil {
		return nil, err
	}
	if _, err := frontierT7SafeID(envelope.ManifestID, "manifest_id", 200); err != nil {
		return nil, err
	}
	if _, err := frontierT7SafeID(envelope.GrantID, "grant_id", 200); err != nil {
		return nil, err
	}
	if _, err := frontierT7SafeID(envelope.MeshGrantID, "mesh_grant_id", 200); err != nil {
		return nil, err
	}
	if _, err := frontierT7SafeID(envelope.RecipientKeyID, "recipient_key_id", 200); err != nil {
		return nil, err
	}
	project, projectErr := sanitizeMemoryProject(envelope.Project)
	if projectErr != nil || project != envelope.Project {
		return nil, errors.New("continuation envelope project is not canonical")
	}
	if !frontierT7ValidDigest(envelope.ManifestDigest) || !frontierT7ValidDigest(envelope.GrantDigest) ||
		!frontierT7ValidDigest(envelope.CiphertextDigest) || !frontierT7ValidDigest(envelope.EnvelopeDigest) {
		return nil, errors.New("continuation envelope digest metadata invalid")
	}
	if err := frontierT7ValidateContinuationSender(envelope.Sender); err != nil {
		return nil, err
	}
	if !frontierT7ValidContinuationEncryptionProfile(envelope.Encryption) {
		return nil, errors.New("unsupported continuation encryption profile")
	}
	if envelope.NetworkCalls != 0 || envelope.DeliveryPerformed ||
		envelope.TransportOwner != frontierT7ContinuationTransportOwner ||
		envelope.PrivateKeyExported || envelope.OrdinaryMemoryMutated {
		return nil, errors.New("continuation transport controls invalid")
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(envelope.Ciphertext)
	if err != nil || len(ciphertext) != envelope.CiphertextBytes {
		return nil, errors.New("continuation ciphertext encoding invalid")
	}
	if frontierT7ContinuationCiphertextDigest(ciphertext) != envelope.CiphertextDigest {
		return nil, errors.New("continuation ciphertext digest mismatch")
	}
	unsigned := frontierT7ContinuationEnvelopeUnsignedValue(envelope)
	if envelope.EnvelopeDigest != frontierT7Digest(unsigned) {
		return nil, errors.New("continuation envelope digest mismatch")
	}
	issuer := contextPassportIssuer{
		InstanceID: envelope.Sender.InstanceID, SigningKeyID: envelope.Sender.SigningKeyID,
		SigningPublicKey: envelope.Sender.SigningPublicKey,
	}
	if !verifySignedBytes(struct {
		EnvelopeDigest string                                 `json:"envelope_digest"`
		Envelope       frontierT7ContinuationEnvelopeUnsigned `json:"envelope"`
	}{EnvelopeDigest: envelope.EnvelopeDigest, Envelope: unsigned}, envelope.Signature, issuer) {
		return nil, errors.New("continuation envelope signature invalid")
	}
	created, createdErr := time.Parse(time.RFC3339Nano, envelope.CreatedAt)
	expires, expiresErr := time.Parse(time.RFC3339Nano, envelope.ExpiresAt)
	if createdErr != nil || expiresErr != nil || !expires.After(created) {
		return nil, errors.New("continuation envelope time bounds invalid")
	}
	if created.After(now.Add(2 * time.Minute)) {
		return nil, errors.New("continuation envelope creation time is in the future")
	}
	if !now.Before(expires) {
		return nil, errors.New("continuation envelope expired")
	}
	return ciphertext, nil
}

func frontierT7VerifyContinuationPayload(payload frontierT7ContinuationPayload, now time.Time) error {
	if payload.SchemaID != frontierT7ContinuationPayloadSchemaID || payload.Version != 1 {
		return errors.New("unsupported continuation payload")
	}
	if _, err := frontierT7SafeID(payload.EnvelopeID, "envelope_id", 200); err != nil {
		return err
	}
	if _, err := frontierT7SafeID(payload.MeshGrantID, "mesh_grant_id", 200); err != nil {
		return err
	}
	if _, err := frontierT7SafeID(payload.RecipientKeyID, "recipient_key_id", 200); err != nil {
		return err
	}
	project, projectErr := sanitizeMemoryProject(payload.Project)
	if projectErr != nil || project != payload.Project {
		return errors.New("continuation payload project is not canonical")
	}
	if err := frontierT7ValidateContinuationSender(payload.Sender); err != nil {
		return err
	}
	if !payload.Bounded || !payload.Redacted || !payload.MeshEncryptionRequired || payload.PlaintextExported || payload.DeliveryPerformed || payload.TransportOwnedByContextLattice || payload.PrivateKeyExported || payload.NetworkCalls != 0 {
		return errors.New("continuation payload safety controls invalid")
	}
	unsigned := frontierT7ContinuationPayloadUnsignedValue(payload)
	if payload.PayloadDigest != frontierT7Digest(unsigned) {
		return errors.New("continuation payload digest mismatch")
	}
	issuer := contextPassportIssuer{
		InstanceID: payload.Sender.InstanceID, SigningKeyID: payload.Sender.SigningKeyID,
		SigningPublicKey: payload.Sender.SigningPublicKey,
	}
	if !verifySignedBytes(struct {
		PayloadDigest string                                `json:"payload_digest"`
		Payload       frontierT7ContinuationPayloadUnsigned `json:"payload"`
	}{PayloadDigest: payload.PayloadDigest, Payload: unsigned}, payload.Signature, issuer) {
		return errors.New("continuation payload signature invalid")
	}
	created, createdErr := time.Parse(time.RFC3339Nano, payload.CreatedAt)
	expires, expiresErr := time.Parse(time.RFC3339Nano, payload.ExpiresAt)
	if createdErr != nil || expiresErr != nil || !expires.After(created) {
		return errors.New("continuation payload time bounds invalid")
	}
	if created.After(now.Add(2 * time.Minute)) {
		return errors.New("continuation payload creation time is in the future")
	}
	if !now.Before(expires) {
		return errors.New("continuation payload expired")
	}
	if err := frontierT7ValidateContinuationArtifacts(payload.Manifest, payload.Grant, now); err != nil {
		return err
	}
	if !frontierT7SenderMatchesIssuer(payload.Sender, payload.Manifest.Issuer) {
		return errors.New("continuation sender and issuer binding mismatch")
	}
	if payload.Project != payload.Manifest.Project || payload.RecipientKeyID != payload.Manifest.RecipientKeyID {
		return errors.New("continuation payload scope binding mismatch")
	}
	if payload.ManifestDigest != payload.Manifest.ManifestDigest || payload.GrantDigest != payload.Grant.GrantDigest {
		return errors.New("continuation payload artifact digest binding mismatch")
	}
	manifestCreated, _ := time.Parse(time.RFC3339Nano, payload.Manifest.CreatedAt)
	manifestExpiry, _ := time.Parse(time.RFC3339Nano, payload.Manifest.ExpiresAt)
	grantExpiry, _ := time.Parse(time.RFC3339Nano, payload.Grant.ExpiresAt)
	if manifestCreated.After(created.Add(2*time.Minute)) || expires.After(manifestExpiry) || expires.After(grantExpiry) {
		return errors.New("continuation payload time binding mismatch")
	}
	return nil
}

func frontierT7ContinuationMeshGrantLocallyRevoked(mesh *contextMeshStore, meshGrantID string) bool {
	mesh.mu.RLock()
	defer mesh.mu.RUnlock()
	if _, revoked := mesh.revocations[meshGrantID]; revoked {
		return true
	}
	grant, exists := mesh.grants[meshGrantID]
	return exists && grant.Status == "revoked"
}

func frontierT7DecryptContinuationEnvelope(mesh *contextMeshStore, envelope frontierT7ContinuationEnvelope, now time.Time) (frontierT7ContinuationManifest, frontierT7CollaborativeGrant, error) {
	if now.IsZero() {
		return frontierT7ContinuationManifest{}, frontierT7CollaborativeGrant{}, errors.New("current time is required")
	}
	now = now.UTC()
	if err := frontierT7ValidateContinuationMesh(mesh); err != nil {
		return frontierT7ContinuationManifest{}, frontierT7CollaborativeGrant{}, err
	}
	ciphertext, err := frontierT7VerifyContinuationEnvelope(envelope, now, frontierT7ContinuationCiphertextLimit(mesh))
	if err != nil {
		return frontierT7ContinuationManifest{}, frontierT7CollaborativeGrant{}, fmt.Errorf("continuation envelope invalid: %w", err)
	}
	if envelope.RecipientKeyID != mesh.identity.MeshKeyID {
		return frontierT7ContinuationManifest{}, frontierT7CollaborativeGrant{}, errors.New("wrong continuation recipient")
	}
	identity, err := age.ParseX25519Identity(mesh.identity.MeshIdentity)
	if err != nil || identity.Recipient().String() != mesh.identity.MeshRecipient {
		return frontierT7ContinuationManifest{}, frontierT7CollaborativeGrant{}, errors.New("local mesh identity invalid")
	}
	reader, err := age.Decrypt(bytes.NewReader(ciphertext), identity)
	if err != nil {
		return frontierT7ContinuationManifest{}, frontierT7CollaborativeGrant{}, errors.New("continuation decryption failed")
	}
	plaintextLimit := frontierT7ContinuationPlaintextLimit(mesh)
	plaintext, err := io.ReadAll(io.LimitReader(reader, int64(plaintextLimit)+1))
	if err != nil || len(plaintext) > plaintextLimit {
		return frontierT7ContinuationManifest{}, frontierT7CollaborativeGrant{}, errors.New("continuation plaintext exceeds limit")
	}
	var payload frontierT7ContinuationPayload
	if err := strictJSONDecode(plaintext, &payload); err != nil {
		return frontierT7ContinuationManifest{}, frontierT7CollaborativeGrant{}, errors.New("continuation payload strict decoding failed")
	}
	if err := frontierT7VerifyContinuationPayload(payload, now); err != nil {
		return frontierT7ContinuationManifest{}, frontierT7CollaborativeGrant{}, fmt.Errorf("continuation payload invalid: %w", err)
	}
	if payload.EnvelopeID != envelope.EnvelopeID || payload.Project != envelope.Project ||
		payload.CreatedAt != envelope.CreatedAt || payload.ExpiresAt != envelope.ExpiresAt ||
		payload.MeshGrantID != envelope.MeshGrantID || payload.RecipientKeyID != envelope.RecipientKeyID ||
		payload.Manifest.ManifestID != envelope.ManifestID || payload.ManifestDigest != envelope.ManifestDigest ||
		payload.Grant.GrantID != envelope.GrantID || payload.GrantDigest != envelope.GrantDigest ||
		!jsonValuesEqual(payload.Sender, envelope.Sender) {
		return frontierT7ContinuationManifest{}, frontierT7CollaborativeGrant{}, errors.New("continuation inner and outer metadata mismatch")
	}
	if frontierT7ContinuationMeshGrantLocallyRevoked(mesh, envelope.MeshGrantID) {
		return frontierT7ContinuationManifest{}, frontierT7CollaborativeGrant{}, errors.New("context mesh grant locally revoked")
	}
	return payload.Manifest, payload.Grant, nil
}

func decodeFrontierT7ContinuationEnvelope(value any) (frontierT7ContinuationEnvelope, error) {
	if object, ok := value.(map[string]any); ok {
		clean := make(map[string]any, len(object))
		for key, nested := range object {
			if key != "format_contract" {
				clean[key] = nested
			}
		}
		value = clean
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return frontierT7ContinuationEnvelope{}, err
	}
	var envelope frontierT7ContinuationEnvelope
	if err := strictJSONDecode(raw, &envelope); err != nil {
		return frontierT7ContinuationEnvelope{}, err
	}
	if !frontierT7ValidContinuationEncryptionProfile(envelope.Encryption) {
		return frontierT7ContinuationEnvelope{}, errors.New("unsupported continuation encryption profile")
	}
	return envelope, nil
}
