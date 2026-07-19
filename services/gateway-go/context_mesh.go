package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"filippo.io/age"
)

const (
	contextMeshGrantContractID      = "context_mesh_grant.v1"
	contextMeshRevocationContractID = "context_mesh_revocation.v1"
	contextMeshEnvelopeContractID   = "context_mesh_envelope.v1"
	contextMeshImportContractID     = "context_mesh_import.v1"
	contextMeshStatusSchemaID       = "context_mesh_status.v1"
	contextMeshStateSchemaID        = "context_mesh_state.v1"
	contextMeshPayloadSchemaID      = "context_mesh_payload.v1"
	contextMeshRevocationSchemaID   = contextMeshRevocationContractID
	contextMeshReceiptSchemaID      = "context_mesh_receipt.v1"
)

type contextMeshGrant struct {
	SchemaID         string                   `json:"schema_id"`
	Version          int                      `json:"version"`
	GrantID          string                   `json:"grant_id"`
	RecipientID      string                   `json:"recipient_id"`
	Recipient        string                   `json:"recipient"`
	RecipientKeyID   string                   `json:"recipient_key_id"`
	Projects         []string                 `json:"projects"`
	Capabilities     []string                 `json:"capabilities"`
	Status           string                   `json:"status"`
	CreatedAt        string                   `json:"created_at"`
	ExpiresAt        string                   `json:"expires_at"`
	RevokedAt        string                   `json:"revoked_at,omitempty"`
	RevocationReason string                   `json:"revocation_reason,omitempty"`
	Issuer           contextPassportIssuer    `json:"issuer"`
	Signature        contextPassportSignature `json:"signature"`
}

type contextMeshGrantUnsigned struct {
	SchemaID         string                `json:"schema_id"`
	Version          int                   `json:"version"`
	GrantID          string                `json:"grant_id"`
	RecipientID      string                `json:"recipient_id"`
	Recipient        string                `json:"recipient"`
	RecipientKeyID   string                `json:"recipient_key_id"`
	Projects         []string              `json:"projects"`
	Capabilities     []string              `json:"capabilities"`
	Status           string                `json:"status"`
	CreatedAt        string                `json:"created_at"`
	ExpiresAt        string                `json:"expires_at"`
	RevokedAt        string                `json:"revoked_at,omitempty"`
	RevocationReason string                `json:"revocation_reason,omitempty"`
	Issuer           contextPassportIssuer `json:"issuer"`
}

type contextMeshRevocation struct {
	SchemaID  string                   `json:"schema_id"`
	Version   int                      `json:"version"`
	GrantID   string                   `json:"grant_id"`
	RevokedAt string                   `json:"revoked_at"`
	Reason    string                   `json:"reason"`
	Issuer    contextPassportIssuer    `json:"issuer"`
	Signature contextPassportSignature `json:"signature"`
}

type contextMeshRevocationUnsigned struct {
	SchemaID  string                `json:"schema_id"`
	Version   int                   `json:"version"`
	GrantID   string                `json:"grant_id"`
	RevokedAt string                `json:"revoked_at"`
	Reason    string                `json:"reason"`
	Issuer    contextPassportIssuer `json:"issuer"`
}

type contextMeshReceipt struct {
	SchemaID       string `json:"schema_id"`
	Version        int    `json:"version"`
	ReceiptID      string `json:"receipt_id"`
	EnvelopeID     string `json:"envelope_id"`
	PassportID     string `json:"passport_id"`
	Project        string `json:"project"`
	Action         string `json:"action"`
	Conflict       bool   `json:"conflict"`
	RecordedAt     string `json:"recorded_at"`
	SenderKeyID    string `json:"sender_key_id"`
	RecipientKeyID string `json:"recipient_key_id"`
}

type contextMeshState struct {
	SchemaID    string                           `json:"schema_id"`
	Version     int                              `json:"version"`
	Grants      map[string]contextMeshGrant      `json:"grants"`
	Revocations map[string]contextMeshRevocation `json:"revocations"`
	Receipts    []contextMeshReceipt             `json:"receipts"`
	UpdatedAt   string                           `json:"updated_at"`
}

type contextMeshStoreConfig struct {
	Enabled           bool
	Path              string
	MaxBytes          int
	MaxGrants         int
	MaxRevocations    int
	MaxReceipts       int
	MaxEnvelopeBytes  int
	MaxPlaintextBytes int
	Fsync             bool
}

type contextMeshStore struct {
	mu                sync.RWMutex
	enabled           bool
	path              string
	maxBytes          int
	maxGrants         int
	maxRevocations    int
	maxReceipts       int
	maxEnvelopeBytes  int
	maxPlaintextBytes int
	fsync             bool
	identity          *contextIdentityKeys
	grants            map[string]contextMeshGrant
	revocations       map[string]contextMeshRevocation
	receipts          []contextMeshReceipt
	lastPersistedAt   string
	lastError         string
	parseErrors       int
}

type contextMeshSender struct {
	InstanceID       string `json:"instance_id"`
	SigningKeyID     string `json:"signing_key_id"`
	SigningPublicKey string `json:"signing_public_key"`
	MeshKeyID        string `json:"mesh_key_id"`
	MeshRecipient    string `json:"mesh_recipient"`
}

type contextMeshPayload struct {
	SchemaID       string                   `json:"schema_id"`
	Version        int                      `json:"version"`
	EnvelopeID     string                   `json:"envelope_id"`
	Project        string                   `json:"project"`
	CreatedAt      string                   `json:"created_at"`
	ExpiresAt      string                   `json:"expires_at"`
	Sender         contextMeshSender        `json:"sender"`
	Grants         []contextMeshGrant       `json:"grants"`
	Passport       contextPassport          `json:"passport"`
	PassportDigest string                   `json:"passport_digest"`
	PayloadDigest  string                   `json:"payload_digest"`
	Signature      contextPassportSignature `json:"signature"`
}

type contextMeshPayloadUnsigned struct {
	SchemaID       string             `json:"schema_id"`
	Version        int                `json:"version"`
	EnvelopeID     string             `json:"envelope_id"`
	Project        string             `json:"project"`
	CreatedAt      string             `json:"created_at"`
	ExpiresAt      string             `json:"expires_at"`
	Sender         contextMeshSender  `json:"sender"`
	Grants         []contextMeshGrant `json:"grants"`
	Passport       contextPassport    `json:"passport"`
	PassportDigest string             `json:"passport_digest"`
}

type contextMeshEnvelope struct {
	SchemaID         string                   `json:"schema_id"`
	Version          int                      `json:"version"`
	EnvelopeID       string                   `json:"envelope_id"`
	Project          string                   `json:"project"`
	CreatedAt        string                   `json:"created_at"`
	ExpiresAt        string                   `json:"expires_at"`
	Sender           contextMeshSender        `json:"sender"`
	GrantIDs         []string                 `json:"grant_ids"`
	RecipientKeyIDs  []string                 `json:"recipient_key_ids"`
	Encryption       map[string]any           `json:"encryption"`
	Ciphertext       string                   `json:"ciphertext"`
	CiphertextBytes  int                      `json:"ciphertext_bytes"`
	CiphertextDigest string                   `json:"ciphertext_digest"`
	EnvelopeDigest   string                   `json:"envelope_digest"`
	Signature        contextPassportSignature `json:"signature"`
	Transport        map[string]any           `json:"transport"`
}

type contextMeshEnvelopeUnsigned struct {
	SchemaID         string            `json:"schema_id"`
	Version          int               `json:"version"`
	EnvelopeID       string            `json:"envelope_id"`
	Project          string            `json:"project"`
	CreatedAt        string            `json:"created_at"`
	ExpiresAt        string            `json:"expires_at"`
	Sender           contextMeshSender `json:"sender"`
	GrantIDs         []string          `json:"grant_ids"`
	RecipientKeyIDs  []string          `json:"recipient_key_ids"`
	Encryption       map[string]any    `json:"encryption"`
	Ciphertext       string            `json:"ciphertext"`
	CiphertextBytes  int               `json:"ciphertext_bytes"`
	CiphertextDigest string            `json:"ciphertext_digest"`
	Transport        map[string]any    `json:"transport"`
}

func defaultContextMeshStoreConfig() contextMeshStoreConfig {
	return contextMeshStoreConfig{
		Enabled:           envBool("CONTEXTLATTICE_CONTEXT_MESH_ENABLED", true),
		Path:              resolveStoragePath("CONTEXTLATTICE_CONTEXT_MESH_STATE_PATH", filepath.Join(".data", "orchestrator", "context_mesh_state.json")),
		MaxBytes:          clampInt(envInt("CONTEXTLATTICE_CONTEXT_MESH_STATE_MAX_BYTES", 2*1024*1024), 64*1024, 32*1024*1024),
		MaxGrants:         clampInt(envInt("CONTEXTLATTICE_CONTEXT_MESH_MAX_GRANTS", 512), 8, 10000),
		MaxRevocations:    clampInt(envInt("CONTEXTLATTICE_CONTEXT_MESH_MAX_REVOCATIONS", 1024), 8, 10000),
		MaxReceipts:       clampInt(envInt("CONTEXTLATTICE_CONTEXT_MESH_MAX_RECEIPTS", 512), 8, 10000),
		MaxEnvelopeBytes:  clampInt(envInt("CONTEXTLATTICE_CONTEXT_MESH_MAX_ENVELOPE_BYTES", 384*1024), 64*1024, 16*1024*1024),
		MaxPlaintextBytes: clampInt(envInt("CONTEXTLATTICE_CONTEXT_MESH_MAX_PLAINTEXT_BYTES", 256*1024), 32*1024, 8*1024*1024),
		Fsync:             envBool("CONTEXTLATTICE_CONTEXT_MESH_FSYNC", true),
	}
}

func newContextMeshStoreFromEnv(passports *contextPassportStore) (*contextMeshStore, error) {
	return newContextMeshStore(defaultContextMeshStoreConfig(), passports)
}

func newContextMeshStore(config contextMeshStoreConfig, passports *contextPassportStore) (*contextMeshStore, error) {
	maxGrants := config.MaxGrants
	if maxGrants <= 0 {
		maxGrants = 32
	}
	maxRevocations := config.MaxRevocations
	if maxRevocations <= 0 {
		maxRevocations = maxInt(maxGrants, 32)
	}
	maxReceipts := config.MaxReceipts
	if maxReceipts <= 0 {
		maxReceipts = 32
	}
	store := &contextMeshStore{
		enabled: config.Enabled, path: strings.TrimSpace(config.Path), maxBytes: config.MaxBytes,
		maxGrants: maxGrants, maxRevocations: maxRevocations, maxReceipts: maxReceipts,
		maxEnvelopeBytes: config.MaxEnvelopeBytes, maxPlaintextBytes: config.MaxPlaintextBytes,
		fsync: config.Fsync, grants: map[string]contextMeshGrant{},
		revocations: map[string]contextMeshRevocation{}, receipts: []contextMeshReceipt{},
	}
	if !store.enabled || store.path == "" || passports == nil || passports.identity == nil {
		store.enabled = false
		return store, nil
	}
	store.identity = passports.identity
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

func grantUnsigned(grant contextMeshGrant) contextMeshGrantUnsigned {
	return contextMeshGrantUnsigned{
		SchemaID: grant.SchemaID, Version: grant.Version, GrantID: grant.GrantID,
		RecipientID: grant.RecipientID, Recipient: grant.Recipient, RecipientKeyID: grant.RecipientKeyID,
		Projects: grant.Projects, Capabilities: grant.Capabilities, Status: grant.Status,
		CreatedAt: grant.CreatedAt, ExpiresAt: grant.ExpiresAt,
		RevokedAt: grant.RevokedAt, RevocationReason: grant.RevocationReason, Issuer: grant.Issuer,
	}
}

func signBytesWithIdentity(value any, keys *contextIdentityKeys) (contextPassportSignature, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return contextPassportSignature{}, err
	}
	privateKey, err := base64.RawStdEncoding.DecodeString(keys.SigningPrivateKey)
	if err != nil || len(privateKey) != ed25519.PrivateKeySize {
		return contextPassportSignature{}, errors.New("invalid local signing key")
	}
	return contextPassportSignature{
		Algorithm: "Ed25519", KeyID: keys.SigningKeyID,
		Value: base64.RawStdEncoding.EncodeToString(ed25519.Sign(ed25519.PrivateKey(privateKey), encoded)),
	}, nil
}

func verifySignedBytes(value any, signature contextPassportSignature, issuer contextPassportIssuer) bool {
	if signature.Algorithm != "Ed25519" || signature.KeyID != issuer.SigningKeyID {
		return false
	}
	if issuer.SigningKeyID != "ed25519:"+digestPrefix(issuer.SigningPublicKey, 24) {
		return false
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return false
	}
	publicKey, publicErr := base64.RawStdEncoding.DecodeString(issuer.SigningPublicKey)
	signatureBytes, signatureErr := base64.RawStdEncoding.DecodeString(signature.Value)
	return publicErr == nil && signatureErr == nil && len(publicKey) == ed25519.PublicKeySize && len(signatureBytes) == ed25519.SignatureSize && ed25519.Verify(ed25519.PublicKey(publicKey), encoded, signatureBytes)
}

func signContextMeshGrant(grant *contextMeshGrant, keys *contextIdentityKeys) error {
	signature, err := signBytesWithIdentity(grantUnsigned(*grant), keys)
	if err != nil {
		return err
	}
	grant.Signature = signature
	return nil
}

func verifyContextMeshGrant(grant contextMeshGrant, now time.Time, checkExpiry bool) []string {
	findings := []string{}
	if grant.SchemaID != contextMeshGrantContractID || grant.Version != 1 || grant.GrantID == "" || grant.RecipientID == "" {
		findings = append(findings, "invalid_grant_identity")
	}
	recipient, err := age.ParseX25519Recipient(grant.Recipient)
	if err != nil || recipient.String() != grant.Recipient {
		findings = append(findings, "invalid_x25519_recipient")
	}
	if grant.RecipientKeyID != "age-x25519:"+digestPrefix(grant.Recipient, 24) {
		findings = append(findings, "recipient_key_id_mismatch")
	}
	if !verifySignedBytes(grantUnsigned(grant), grant.Signature, grant.Issuer) {
		findings = append(findings, "grant_signature_invalid")
	}
	created, createdErr := time.Parse(time.RFC3339Nano, grant.CreatedAt)
	expires, expiresErr := time.Parse(time.RFC3339Nano, grant.ExpiresAt)
	if createdErr != nil || expiresErr != nil || !expires.After(created) {
		findings = append(findings, "invalid_grant_validity")
	} else if checkExpiry && !now.Before(expires) {
		findings = append(findings, "grant_expired")
	}
	if grant.Status != "active" && grant.Status != "revoked" {
		findings = append(findings, "invalid_grant_status")
	}
	if grant.Status == "revoked" && strings.TrimSpace(grant.RevokedAt) == "" {
		findings = append(findings, "revocation_timestamp_missing")
	}
	if len(grant.Projects) == 0 {
		findings = append(findings, "grant_project_scope_missing")
	}
	if !containsString(grant.Capabilities, contextPassportContractID) || !containsString(grant.Capabilities, contextMeshImportContractID) {
		findings = append(findings, "grant_required_capabilities_missing")
	}
	return uniqueSortedStrings(findings)
}

func (s *contextMeshStore) load() error {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(raw) > s.maxBytes {
		return errors.New("context mesh state exceeds configured byte limit")
	}
	var state contextMeshState
	if err := strictJSONDecode(raw, &state); err != nil {
		return fmt.Errorf("decode context mesh state: %w", err)
	}
	if state.SchemaID != contextMeshStateSchemaID || state.Version != 1 {
		return errors.New("unsupported context mesh state schema")
	}
	for id, grant := range state.Grants {
		if id != grant.GrantID || len(verifyContextMeshGrant(grant, time.Now().UTC(), false)) > 0 {
			return fmt.Errorf("invalid persisted context mesh grant: %s", id)
		}
	}
	for id, revocation := range state.Revocations {
		unsigned := contextMeshRevocationUnsigned{SchemaID: revocation.SchemaID, Version: revocation.Version, GrantID: revocation.GrantID, RevokedAt: revocation.RevokedAt, Reason: revocation.Reason, Issuer: revocation.Issuer}
		if id != revocation.GrantID || revocation.SchemaID != contextMeshRevocationSchemaID || revocation.Version != 1 || !verifySignedBytes(unsigned, revocation.Signature, revocation.Issuer) {
			return fmt.Errorf("invalid persisted context mesh revocation: %s", id)
		}
	}
	if len(state.Grants) > s.maxGrants {
		return fmt.Errorf("persisted context mesh grants exceed configured entry limit: %d > %d", len(state.Grants), s.maxGrants)
	}
	if len(state.Revocations) > s.maxRevocations {
		return fmt.Errorf("persisted context mesh revocations exceed configured entry limit: %d > %d", len(state.Revocations), s.maxRevocations)
	}
	s.grants = state.Grants
	if s.grants == nil {
		s.grants = map[string]contextMeshGrant{}
	}
	s.revocations = state.Revocations
	if s.revocations == nil {
		s.revocations = map[string]contextMeshRevocation{}
	}
	s.receipts = state.Receipts
	s.trimLocked()
	return nil
}

func (s *contextMeshStore) trimLocked() {
	if len(s.receipts) > s.maxReceipts {
		s.receipts = append([]contextMeshReceipt(nil), s.receipts[len(s.receipts)-s.maxReceipts:]...)
	}
}

func (s *contextMeshStore) reclaimGrantCapacityLocked(now time.Time) {
	if len(s.grants) < s.maxGrants {
		return
	}
	type candidate struct{ id, timestamp string }
	candidates := []candidate{}
	for id, grant := range s.grants {
		expires, err := time.Parse(time.RFC3339Nano, grant.ExpiresAt)
		if grant.Status == "revoked" || (err == nil && !now.Before(expires)) {
			candidates = append(candidates, candidate{id: id, timestamp: firstNonEmptyStrings(grant.RevokedAt, grant.ExpiresAt, grant.CreatedAt)})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].timestamp != candidates[j].timestamp {
			return candidates[i].timestamp < candidates[j].timestamp
		}
		return candidates[i].id < candidates[j].id
	})
	for _, item := range candidates {
		if len(s.grants) < s.maxGrants {
			break
		}
		delete(s.grants, item.id)
	}
}

func cloneContextMeshState(grants map[string]contextMeshGrant, revocations map[string]contextMeshRevocation, receipts []contextMeshReceipt) contextMeshState {
	grantCopy := make(map[string]contextMeshGrant, len(grants))
	for id, grant := range grants {
		grantCopy[id] = grant
	}
	revocationCopy := make(map[string]contextMeshRevocation, len(revocations))
	for id, revocation := range revocations {
		revocationCopy[id] = revocation
	}
	return contextMeshState{SchemaID: contextMeshStateSchemaID, Version: 1, Grants: grantCopy, Revocations: revocationCopy, Receipts: append([]contextMeshReceipt(nil), receipts...), UpdatedAt: nowUTCISO()}
}

func (s *contextMeshStore) saveLocked() error {
	s.trimLocked()
	if len(s.grants) > s.maxGrants {
		return errors.New("context mesh grant capacity reached")
	}
	if len(s.revocations) > s.maxRevocations {
		return errors.New("context mesh revocation capacity reached")
	}
	state := cloneContextMeshState(s.grants, s.revocations, s.receipts)
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if len(encoded) > s.maxBytes {
		return fmt.Errorf("context mesh state exceeds %d byte limit", s.maxBytes)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	if err := writeAtomicFile(s.path, append(encoded, '\n'), 0o600); err != nil {
		return err
	}
	if s.fsync {
		file, err := os.OpenFile(s.path, os.O_RDONLY, 0)
		if err != nil {
			return err
		}
		err = file.Sync()
		_ = file.Close()
		if err != nil {
			return err
		}
	}
	s.lastPersistedAt = nowUTCISO()
	s.lastError = ""
	return nil
}

func (s *contextMeshStore) createGrant(payload map[string]any) (contextMeshGrant, error) {
	if s == nil || !s.enabled || s.identity == nil {
		return contextMeshGrant{}, errors.New("context mesh disabled")
	}
	recipientID := clipText(strings.TrimSpace(anyToString(payload["recipient_id"])), 128)
	recipientText := strings.TrimSpace(anyToString(payload["recipient"]))
	if recipientID == "" || recipientText == "" {
		return contextMeshGrant{}, errors.New("recipient_id and recipient are required")
	}
	recipient, err := age.ParseX25519Recipient(recipientText)
	if err != nil || recipient.String() != recipientText {
		return contextMeshGrant{}, errors.New("recipient must be a canonical native age X25519 recipient")
	}
	projects := uniqueSortedStrings(anyToStringSlice(payload["projects"]))
	if project := strings.TrimSpace(anyToString(payload["project"])); project != "" {
		projects = uniqueSortedStrings(append(projects, project))
	}
	if len(projects) == 0 || len(projects) > 32 {
		return contextMeshGrant{}, errors.New("one to 32 projects are required")
	}
	for index, project := range projects {
		normalized, err := sanitizeMemoryProject(project)
		if err != nil {
			return contextMeshGrant{}, fmt.Errorf("project scope: %w", err)
		}
		projects[index] = normalized
	}
	created := time.Now().UTC()
	expires, err := parsePassportExpiry(payload, created)
	if err != nil {
		return contextMeshGrant{}, err
	}
	keyID := "age-x25519:" + digestPrefix(recipientText, 24)
	grant := contextMeshGrant{
		SchemaID: contextMeshGrantContractID, Version: 1,
		GrantID:     "grant_" + digestPrefix(strings.Join([]string{recipientID, keyID, strings.Join(projects, ","), created.Format(time.RFC3339Nano), randomHex(8)}, "\x00"), 24),
		RecipientID: recipientID, Recipient: recipientText, RecipientKeyID: keyID,
		Projects: projects, Capabilities: uniqueSortedStrings(append(anyToStringSlice(payload["capabilities"]), contextPassportContractID, contextMeshImportContractID)),
		Status: "active", CreatedAt: created.Format(time.RFC3339Nano), ExpiresAt: expires.Format(time.RFC3339Nano),
		Issuer: contextPassportIssuer{InstanceID: s.identity.InstanceID, SigningKeyID: s.identity.SigningKeyID, SigningPublicKey: s.identity.SigningPublicKey},
	}
	if err := signContextMeshGrant(&grant, s.identity); err != nil {
		return contextMeshGrant{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previousState := cloneContextMeshState(s.grants, s.revocations, s.receipts)
	s.reclaimGrantCapacityLocked(created)
	if len(s.grants) >= s.maxGrants {
		return contextMeshGrant{}, errors.New("context mesh grant capacity reached")
	}
	s.grants[grant.GrantID] = grant
	if err := s.saveLocked(); err != nil {
		s.grants, s.revocations, s.receipts = previousState.Grants, previousState.Revocations, previousState.Receipts
		return contextMeshGrant{}, err
	}
	return grant, nil
}

func (s *contextMeshStore) revokeGrant(grantID, reason string) (contextMeshGrant, contextMeshRevocation, error) {
	if s == nil || !s.enabled || s.identity == nil {
		return contextMeshGrant{}, contextMeshRevocation{}, errors.New("context mesh disabled")
	}
	grantID = clipText(strings.TrimSpace(grantID), 128)
	if grantID == "" {
		return contextMeshGrant{}, contextMeshRevocation{}, errors.New("grant_id is required")
	}
	now := nowUTCISO()
	reason = clipText(strings.TrimSpace(firstNonEmptyStrings(reason, "operator_revoked")), 500)
	reason = portableString(reason, &portableRedactionStats{})
	s.mu.Lock()
	defer s.mu.Unlock()
	previousState := cloneContextMeshState(s.grants, s.revocations, s.receipts)
	if _, exists := s.revocations[grantID]; !exists && len(s.revocations) >= s.maxRevocations {
		return contextMeshGrant{}, contextMeshRevocation{}, errors.New("context mesh revocation capacity reached")
	}
	previousGrant, grantExists := s.grants[grantID]
	grant := previousGrant
	if grantExists {
		grant.Status, grant.RevokedAt, grant.RevocationReason = "revoked", now, reason
		if err := signContextMeshGrant(&grant, s.identity); err != nil {
			return contextMeshGrant{}, contextMeshRevocation{}, err
		}
		s.grants[grantID] = grant
	}
	revocation := contextMeshRevocation{
		SchemaID: contextMeshRevocationSchemaID, Version: 1, GrantID: grantID,
		RevokedAt: now, Reason: reason,
		Issuer: contextPassportIssuer{InstanceID: s.identity.InstanceID, SigningKeyID: s.identity.SigningKeyID, SigningPublicKey: s.identity.SigningPublicKey},
	}
	unsignedRevocation := contextMeshRevocationUnsigned{SchemaID: revocation.SchemaID, Version: revocation.Version, GrantID: revocation.GrantID, RevokedAt: revocation.RevokedAt, Reason: revocation.Reason, Issuer: revocation.Issuer}
	signature, err := signBytesWithIdentity(unsignedRevocation, s.identity)
	if err != nil {
		return contextMeshGrant{}, contextMeshRevocation{}, err
	}
	revocation.Signature = signature
	s.revocations[grantID] = revocation
	if err := s.saveLocked(); err != nil {
		s.grants, s.revocations, s.receipts = previousState.Grants, previousState.Revocations, previousState.Receipts
		return contextMeshGrant{}, contextMeshRevocation{}, err
	}
	return grant, revocation, nil
}

func grantAllowsProject(grant contextMeshGrant, project string) bool {
	for _, allowed := range grant.Projects {
		if allowed == project {
			return true
		}
	}
	return false
}

func (s *contextMeshStore) activeGrant(id, project string, now time.Time) (contextMeshGrant, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	grant, ok := s.grants[strings.TrimSpace(id)]
	if !ok {
		return contextMeshGrant{}, errors.New("grant not found")
	}
	if _, revoked := s.revocations[grant.GrantID]; revoked || grant.Status != "active" {
		return contextMeshGrant{}, errors.New("grant revoked")
	}
	if findings := verifyContextMeshGrant(grant, now, true); len(findings) > 0 {
		return contextMeshGrant{}, fmt.Errorf("grant invalid: %s", strings.Join(findings, ","))
	}
	if !grantAllowsProject(grant, project) {
		return contextMeshGrant{}, errors.New("grant project scope mismatch")
	}
	return grant, nil
}

func meshSender(keys *contextIdentityKeys) contextMeshSender {
	return contextMeshSender{
		InstanceID: keys.InstanceID, SigningKeyID: keys.SigningKeyID,
		SigningPublicKey: keys.SigningPublicKey, MeshKeyID: keys.MeshKeyID,
		MeshRecipient: keys.MeshRecipient,
	}
}

func meshPayloadUnsigned(payload contextMeshPayload) contextMeshPayloadUnsigned {
	return contextMeshPayloadUnsigned{
		SchemaID: payload.SchemaID, Version: payload.Version, EnvelopeID: payload.EnvelopeID,
		Project: payload.Project, CreatedAt: payload.CreatedAt, ExpiresAt: payload.ExpiresAt,
		Sender: payload.Sender, Grants: payload.Grants, Passport: payload.Passport,
		PassportDigest: payload.PassportDigest,
	}
}

func signMeshPayload(payload *contextMeshPayload, keys *contextIdentityKeys) error {
	unsigned := meshPayloadUnsigned(*payload)
	encoded, err := json.Marshal(unsigned)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(encoded)
	payload.PayloadDigest = "sha256:" + hex.EncodeToString(digest[:])
	payload.Signature, err = signBytesWithIdentity(struct {
		PayloadDigest string                     `json:"payload_digest"`
		Payload       contextMeshPayloadUnsigned `json:"payload"`
	}{payload.PayloadDigest, unsigned}, keys)
	return err
}

func verifyMeshPayload(payload contextMeshPayload, now time.Time) []string {
	findings := []string{}
	if payload.SchemaID != contextMeshPayloadSchemaID || payload.Version != 1 {
		findings = append(findings, "unsupported_mesh_payload")
	}
	unsigned := meshPayloadUnsigned(payload)
	encoded, err := json.Marshal(unsigned)
	if err != nil {
		return append(findings, "payload_canonicalization_failed")
	}
	digest := sha256.Sum256(encoded)
	expected := "sha256:" + hex.EncodeToString(digest[:])
	issuer := contextPassportIssuer{InstanceID: payload.Sender.InstanceID, SigningKeyID: payload.Sender.SigningKeyID, SigningPublicKey: payload.Sender.SigningPublicKey}
	if payload.Sender.MeshKeyID != "age-x25519:"+digestPrefix(payload.Sender.MeshRecipient, 24) {
		findings = append(findings, "sender_mesh_key_id_mismatch")
	}
	if payload.PayloadDigest != expected {
		findings = append(findings, "payload_digest_mismatch")
	}
	if !verifySignedBytes(struct {
		PayloadDigest string                     `json:"payload_digest"`
		Payload       contextMeshPayloadUnsigned `json:"payload"`
	}{payload.PayloadDigest, unsigned}, payload.Signature, issuer) {
		findings = append(findings, "payload_signature_invalid")
	}
	if payload.Passport.ContentDigest != payload.PassportDigest {
		findings = append(findings, "passport_digest_binding_mismatch")
	}
	if payload.Passport.Project != payload.Project {
		findings = append(findings, "passport_project_binding_mismatch")
	}
	findings = append(findings, verifyContextPassport(payload.Passport, now, true)...)
	created, createdErr := time.Parse(time.RFC3339Nano, payload.CreatedAt)
	expires, expiresErr := time.Parse(time.RFC3339Nano, payload.ExpiresAt)
	if createdErr != nil || expiresErr != nil || !expires.After(created) || !now.Before(expires) {
		findings = append(findings, "mesh_payload_expired_or_invalid")
	}
	return uniqueSortedStrings(findings)
}

func meshEnvelopeUnsigned(envelope contextMeshEnvelope) contextMeshEnvelopeUnsigned {
	return contextMeshEnvelopeUnsigned{
		SchemaID: envelope.SchemaID, Version: envelope.Version, EnvelopeID: envelope.EnvelopeID,
		Project: envelope.Project, CreatedAt: envelope.CreatedAt, ExpiresAt: envelope.ExpiresAt,
		Sender: envelope.Sender, GrantIDs: envelope.GrantIDs, RecipientKeyIDs: envelope.RecipientKeyIDs,
		Encryption: envelope.Encryption, Ciphertext: envelope.Ciphertext,
		CiphertextBytes: envelope.CiphertextBytes, CiphertextDigest: envelope.CiphertextDigest,
		Transport: envelope.Transport,
	}
}

func signMeshEnvelope(envelope *contextMeshEnvelope, keys *contextIdentityKeys) error {
	unsigned := meshEnvelopeUnsigned(*envelope)
	encoded, err := json.Marshal(unsigned)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(encoded)
	envelope.EnvelopeDigest = "sha256:" + hex.EncodeToString(digest[:])
	envelope.Signature, err = signBytesWithIdentity(struct {
		EnvelopeDigest string                      `json:"envelope_digest"`
		Envelope       contextMeshEnvelopeUnsigned `json:"envelope"`
	}{envelope.EnvelopeDigest, unsigned}, keys)
	return err
}

func verifyMeshEnvelope(envelope contextMeshEnvelope, now time.Time, maxBytes int) []string {
	findings := []string{}
	if envelope.SchemaID != contextMeshEnvelopeContractID || envelope.Version != 1 {
		findings = append(findings, "unsupported_mesh_envelope")
	}
	unsigned := meshEnvelopeUnsigned(envelope)
	encoded, err := json.Marshal(unsigned)
	if err != nil {
		return append(findings, "envelope_canonicalization_failed")
	}
	digest := sha256.Sum256(encoded)
	expected := "sha256:" + hex.EncodeToString(digest[:])
	issuer := contextPassportIssuer{InstanceID: envelope.Sender.InstanceID, SigningKeyID: envelope.Sender.SigningKeyID, SigningPublicKey: envelope.Sender.SigningPublicKey}
	if envelope.Sender.MeshKeyID != "age-x25519:"+digestPrefix(envelope.Sender.MeshRecipient, 24) {
		findings = append(findings, "sender_mesh_key_id_mismatch")
	}
	if recipient, err := age.ParseX25519Recipient(envelope.Sender.MeshRecipient); err != nil || recipient.String() != envelope.Sender.MeshRecipient {
		findings = append(findings, "sender_mesh_recipient_invalid")
	}
	if envelope.EnvelopeDigest != expected {
		findings = append(findings, "envelope_digest_mismatch")
	}
	if !verifySignedBytes(struct {
		EnvelopeDigest string                      `json:"envelope_digest"`
		Envelope       contextMeshEnvelopeUnsigned `json:"envelope"`
	}{envelope.EnvelopeDigest, unsigned}, envelope.Signature, issuer) {
		findings = append(findings, "envelope_signature_invalid")
	}
	ciphertext, decodeErr := base64.RawStdEncoding.DecodeString(envelope.Ciphertext)
	if decodeErr != nil || len(ciphertext) != envelope.CiphertextBytes || len(ciphertext) > maxBytes {
		findings = append(findings, "ciphertext_size_or_encoding_invalid")
	} else {
		digest := sha256.Sum256(ciphertext)
		if envelope.CiphertextDigest != "sha256:"+hex.EncodeToString(digest[:]) {
			findings = append(findings, "ciphertext_digest_mismatch")
		}
	}
	created, createdErr := time.Parse(time.RFC3339Nano, envelope.CreatedAt)
	expires, expiresErr := time.Parse(time.RFC3339Nano, envelope.ExpiresAt)
	if createdErr != nil || expiresErr != nil || !expires.After(created) || !now.Before(expires) {
		findings = append(findings, "mesh_envelope_expired_or_invalid")
	}
	if anyToString(envelope.Encryption["format"]) != "age-encryption.org/v1" ||
		anyToString(envelope.Encryption["recipient_type"]) != "X25519" ||
		anyToString(envelope.Encryption["key_agreement"]) != "X25519" ||
		anyToString(envelope.Encryption["payload_cipher"]) != "ChaCha20-Poly1305" ||
		anyToBool(envelope.Encryption["multi_recipient"]) != (len(uniqueSortedStrings(envelope.RecipientKeyIDs)) > 1) {
		findings = append(findings, "unsupported_encryption_profile")
	}
	return uniqueSortedStrings(findings)
}

func (s *server) createMeshEnvelope(passport contextPassport, grantIDs []string, expiresAt string) (contextMeshEnvelope, error) {
	if s.contextMesh == nil || !s.contextMesh.enabled || s.contextMesh.identity == nil {
		return contextMeshEnvelope{}, errors.New("context mesh disabled")
	}
	if findings := verifyContextPassport(passport, time.Now().UTC(), true); len(findings) > 0 {
		return contextMeshEnvelope{}, fmt.Errorf("passport invalid: %s", strings.Join(findings, ","))
	}
	grantIDs = uniqueSortedStrings(grantIDs)
	if len(grantIDs) == 0 || len(grantIDs) > 32 {
		return contextMeshEnvelope{}, errors.New("one to 32 grant_ids are required")
	}
	grants := make([]contextMeshGrant, 0, len(grantIDs))
	recipients := make([]age.Recipient, 0, len(grantIDs))
	recipientKeys := []string{}
	seenRecipients := map[string]struct{}{}
	for _, id := range grantIDs {
		grant, err := s.contextMesh.activeGrant(id, passport.Project, time.Now().UTC())
		if err != nil {
			return contextMeshEnvelope{}, fmt.Errorf("grant %s: %w", id, err)
		}
		grants = append(grants, grant)
		if _, exists := seenRecipients[grant.Recipient]; exists {
			continue
		}
		recipient, err := age.ParseX25519Recipient(grant.Recipient)
		if err != nil {
			return contextMeshEnvelope{}, err
		}
		seenRecipients[grant.Recipient] = struct{}{}
		recipients = append(recipients, recipient)
		recipientKeys = append(recipientKeys, grant.RecipientKeyID)
	}
	created := time.Now().UTC()
	expires := created.Add(24 * time.Hour)
	if strings.TrimSpace(expiresAt) != "" {
		parsed, err := time.Parse(time.RFC3339Nano, expiresAt)
		if err != nil || !parsed.After(created) || parsed.After(created.Add(30*24*time.Hour)) {
			return contextMeshEnvelope{}, errors.New("envelope expires_at must be after creation and within 30 days")
		}
		expires = parsed.UTC()
	}
	passportExpiry, _ := time.Parse(time.RFC3339Nano, passport.ExpiresAt)
	if !passportExpiry.IsZero() && passportExpiry.Before(expires) {
		expires = passportExpiry
	}
	payload := contextMeshPayload{
		SchemaID: contextMeshPayloadSchemaID, Version: 1, EnvelopeID: "mesh_" + randomHex(16),
		Project: passport.Project, CreatedAt: created.Format(time.RFC3339Nano), ExpiresAt: expires.Format(time.RFC3339Nano),
		Sender: meshSender(s.contextMesh.identity), Grants: grants, Passport: passport,
		PassportDigest: passport.ContentDigest,
	}
	if err := signMeshPayload(&payload, s.contextMesh.identity); err != nil {
		return contextMeshEnvelope{}, err
	}
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return contextMeshEnvelope{}, err
	}
	if len(plaintext) > s.contextMesh.maxPlaintextBytes {
		return contextMeshEnvelope{}, fmt.Errorf("mesh payload exceeds %d byte limit", s.contextMesh.maxPlaintextBytes)
	}
	var encrypted bytes.Buffer
	writer, err := age.Encrypt(&encrypted, recipients...)
	if err != nil {
		return contextMeshEnvelope{}, err
	}
	if _, err := writer.Write(plaintext); err != nil {
		_ = writer.Close()
		return contextMeshEnvelope{}, err
	}
	if err := writer.Close(); err != nil {
		return contextMeshEnvelope{}, err
	}
	if encrypted.Len() > s.contextMesh.maxEnvelopeBytes {
		return contextMeshEnvelope{}, fmt.Errorf("mesh ciphertext exceeds %d byte limit", s.contextMesh.maxEnvelopeBytes)
	}
	cipherDigest := sha256.Sum256(encrypted.Bytes())
	envelope := contextMeshEnvelope{
		SchemaID: contextMeshEnvelopeContractID, Version: 1, EnvelopeID: payload.EnvelopeID,
		Project: passport.Project, CreatedAt: payload.CreatedAt, ExpiresAt: payload.ExpiresAt,
		Sender: payload.Sender, GrantIDs: grantIDs, RecipientKeyIDs: uniqueSortedStrings(recipientKeys),
		Encryption: map[string]any{
			"format": "age-encryption.org/v1", "recipient_type": "X25519",
			"key_agreement": "X25519", "payload_cipher": "ChaCha20-Poly1305",
			"multi_recipient": len(recipients) > 1,
		},
		Ciphertext:      base64.RawStdEncoding.EncodeToString(encrypted.Bytes()),
		CiphertextBytes: encrypted.Len(), CiphertextDigest: "sha256:" + hex.EncodeToString(cipherDigest[:]),
		Transport: map[string]any{
			"delivery_performed": false, "transport_owned_by_contextlattice": false,
			"mode": "caller_supplied_file_or_json", "network_calls": 0,
		},
	}
	if err := signMeshEnvelope(&envelope, s.contextMesh.identity); err != nil {
		return contextMeshEnvelope{}, err
	}
	return envelope, nil
}

func decodeMeshEnvelope(value any) (contextMeshEnvelope, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return contextMeshEnvelope{}, err
	}
	var envelope contextMeshEnvelope
	if err := strictJSONDecode(raw, &envelope); err != nil {
		return contextMeshEnvelope{}, err
	}
	return envelope, nil
}

func (s *contextMeshStore) decryptEnvelope(envelope contextMeshEnvelope) (contextMeshPayload, string, error) {
	if findings := verifyMeshEnvelope(envelope, time.Now().UTC(), s.maxEnvelopeBytes); len(findings) > 0 {
		return contextMeshPayload{}, "", fmt.Errorf("mesh envelope invalid: %s", strings.Join(findings, ","))
	}
	if !containsString(envelope.RecipientKeyIDs, s.identity.MeshKeyID) {
		return contextMeshPayload{}, "", errors.New("wrong recipient")
	}
	ciphertext, _ := base64.RawStdEncoding.DecodeString(envelope.Ciphertext)
	identity, err := age.ParseX25519Identity(s.identity.MeshIdentity)
	if err != nil {
		return contextMeshPayload{}, "", errors.New("local mesh identity invalid")
	}
	reader, err := age.Decrypt(bytes.NewReader(ciphertext), identity)
	if err != nil {
		return contextMeshPayload{}, "", errors.New("mesh decryption failed")
	}
	plaintext, err := io.ReadAll(io.LimitReader(reader, int64(s.maxPlaintextBytes+1)))
	if err != nil || len(plaintext) > s.maxPlaintextBytes {
		return contextMeshPayload{}, "", errors.New("mesh plaintext exceeds limit")
	}
	var payload contextMeshPayload
	if err := strictJSONDecode(plaintext, &payload); err != nil {
		return contextMeshPayload{}, "", errors.New("mesh payload invalid")
	}
	if findings := verifyMeshPayload(payload, time.Now().UTC()); len(findings) > 0 {
		return contextMeshPayload{}, "", fmt.Errorf("mesh payload verification failed: %s", strings.Join(findings, ","))
	}
	if payload.EnvelopeID != envelope.EnvelopeID || payload.Project != envelope.Project || payload.CreatedAt != envelope.CreatedAt || payload.ExpiresAt != envelope.ExpiresAt || !jsonValuesEqual(payload.Sender, envelope.Sender) {
		return contextMeshPayload{}, "", errors.New("inner and outer envelope metadata mismatch")
	}
	grantIDs := []string{}
	recipientKeyIDs := []string{}
	matchedRecipient := ""
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, grant := range payload.Grants {
		grantIDs = append(grantIDs, grant.GrantID)
		recipientKeyIDs = append(recipientKeyIDs, grant.RecipientKeyID)
		if findings := verifyContextMeshGrant(grant, time.Now().UTC(), true); len(findings) > 0 {
			return contextMeshPayload{}, "", fmt.Errorf("embedded grant invalid: %s", strings.Join(findings, ","))
		}
		if grant.Status != "active" || !grantAllowsProject(grant, payload.Project) {
			return contextMeshPayload{}, "", errors.New("embedded grant inactive or out of scope")
		}
		if grant.Issuer.InstanceID != payload.Sender.InstanceID || grant.Issuer.SigningKeyID != payload.Sender.SigningKeyID || grant.Issuer.SigningPublicKey != payload.Sender.SigningPublicKey {
			return contextMeshPayload{}, "", errors.New("embedded grant issuer does not match envelope sender")
		}
		if _, revoked := s.revocations[grant.GrantID]; revoked {
			return contextMeshPayload{}, "", errors.New("grant locally revoked")
		}
		if grant.RecipientKeyID == s.identity.MeshKeyID {
			matchedRecipient = grant.RecipientKeyID
		}
	}
	if !jsonValuesEqual(uniqueSortedStrings(grantIDs), envelope.GrantIDs) {
		return contextMeshPayload{}, "", errors.New("grant binding mismatch")
	}
	if !jsonValuesEqual(uniqueSortedStrings(recipientKeyIDs), envelope.RecipientKeyIDs) {
		return contextMeshPayload{}, "", errors.New("recipient binding mismatch")
	}
	if matchedRecipient == "" {
		return contextMeshPayload{}, "", errors.New("local recipient grant missing")
	}
	return payload, matchedRecipient, nil
}

func (s *contextMeshStore) recordReceipt(envelope contextMeshEnvelope, passport contextPassport, recipientKeyID string, reconciliation contextPassportReconciliation) error {
	receipt := contextMeshReceipt{
		SchemaID: contextMeshReceiptSchemaID, Version: 1,
		ReceiptID:  "receipt_" + digestPrefix(envelope.EnvelopeID+"\x00"+passport.PassportID+"\x00"+reconciliation.Action, 24),
		EnvelopeID: envelope.EnvelopeID, PassportID: passport.PassportID, Project: passport.Project,
		Action: reconciliation.Action, Conflict: reconciliation.Conflict, RecordedAt: nowUTCISO(),
		SenderKeyID: envelope.Sender.SigningKeyID, RecipientKeyID: recipientKeyID,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.receipts {
		if existing.ReceiptID == receipt.ReceiptID {
			return nil
		}
	}
	previousState := cloneContextMeshState(s.grants, s.revocations, s.receipts)
	s.receipts = append(s.receipts, receipt)
	if err := s.saveLocked(); err != nil {
		s.grants, s.revocations, s.receipts = previousState.Grants, previousState.Revocations, previousState.Receipts
		return err
	}
	return nil
}

func (s *contextMeshStore) snapshot() map[string]any {
	if s == nil {
		return map[string]any{"schema_id": contextMeshStatusSchemaID, "enabled": false}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	grants := make([]contextMeshGrant, 0, len(s.grants))
	active, revoked, expired := 0, 0, 0
	now := time.Now().UTC()
	for _, grant := range s.grants {
		grants = append(grants, grant)
		if grant.Status == "revoked" {
			revoked++
		} else if expires, err := time.Parse(time.RFC3339Nano, grant.ExpiresAt); err == nil && !now.Before(expires) {
			expired++
		} else {
			active++
		}
	}
	sort.Slice(grants, func(i, j int) bool { return grants[i].CreatedAt > grants[j].CreatedAt })
	if len(grants) > 64 {
		grants = grants[:64]
	}
	receipts := append([]contextMeshReceipt(nil), s.receipts...)
	if len(receipts) > 64 {
		receipts = receipts[len(receipts)-64:]
	}
	return map[string]any{
		"schema_id": contextMeshStatusSchemaID, "version": 1, "enabled": s.enabled,
		"identity": s.identity.publicMetadata(), "grants": grants,
		"grant_count": len(s.grants), "active_grants": active, "revoked_grants": revoked,
		"expired_grants": expired, "local_revocation_count": len(s.revocations),
		"receipt_count": len(s.receipts), "recent_receipts": receipts,
		"limits": map[string]any{
			"max_state_bytes": s.maxBytes, "max_grants": s.maxGrants, "max_revocations": s.maxRevocations, "max_receipts": s.maxReceipts,
			"max_envelope_bytes": s.maxEnvelopeBytes, "max_plaintext_bytes": s.maxPlaintextBytes,
		},
		"transport": map[string]any{
			"owned_by_contextlattice": false, "network_calls": 0,
			"delivery": "caller_supplied_file_or_json",
		},
		"last_persisted_at": s.lastPersistedAt, "last_error": s.lastError,
		"private_key_exported": false,
	}
}

func (s *contextMeshStore) aggregateSignalSufficientStatistics(now time.Time) map[string]any {
	if s == nil {
		return map[string]any{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	active := 0
	for _, grant := range s.grants {
		if grant.Status == "revoked" {
			continue
		}
		if expires, err := time.Parse(time.RFC3339Nano, grant.ExpiresAt); err == nil && !now.UTC().Before(expires) {
			continue
		}
		active++
	}
	return map[string]any{
		"active_mesh_grant_count": active,
		"mesh_revocation_count":   len(s.revocations),
	}
}

func (s *server) memoryContextMeshIdentity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	if s.contextMesh == nil || s.contextMesh.identity == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "context_mesh_disabled"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "schema_id": contextIdentityKeysSchemaID, "identity": s.contextMesh.identity.publicMetadata()})
}

func (s *server) memoryContextMeshGrants(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
			return
		}
		writeJSON(w, http.StatusOK, s.contextMesh.snapshot())
	case http.MethodPost:
		if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
			return
		}
		payload, err := readOptionalJSONBody(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_json"})
			return
		}
		grant, err := s.contextMesh.createGrant(payload)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "grant_create_failed", "detail": clipText(err.Error(), 500)})
			return
		}
		response := map[string]any{"ok": true, "schema_id": contextMeshGrantContractID, "grant": grant, "private_key_exported": false}
		writeJSON(w, http.StatusOK, attachPayloadFormatContract(contextMeshGrantContractID, response, "", "context_mesh_grant_create", r.URL.Path))
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

func (s *server) memoryContextMeshGrantRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	payload, err := readOptionalJSONBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_json"})
		return
	}
	grant, revocation, err := s.contextMesh.revokeGrant(anyToString(payload["grant_id"]), anyToString(payload["reason"]))
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "grant_revoke_failed", "detail": clipText(err.Error(), 500)})
		return
	}
	response := map[string]any{"ok": true, "schema_id": contextMeshRevocationContractID, "grant": grant, "revocation": revocation, "tombstone_only": grant.GrantID == ""}
	writeJSON(w, http.StatusOK, attachPayloadFormatContract(contextMeshRevocationContractID, response, "", "context_mesh_grant_revoke", r.URL.Path))
}

func (s *server) memoryContextMeshExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	payload, err := readOptionalJSONBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_json"})
		return
	}
	passportID := strings.TrimSpace(anyToString(payload["passport_id"]))
	passport, ok := s.contextPassports.get(passportID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "passport_not_found"})
		return
	}
	envelope, err := s.createMeshEnvelope(passport, anyToStringSlice(payload["grant_ids"]), anyToString(payload["expires_at"]))
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "mesh_export_failed", "detail": clipText(err.Error(), 500)})
		return
	}
	response := map[string]any{
		"ok": true, "schema_id": contextMeshEnvelopeContractID, "envelope": envelope,
		"delivery_performed": false, "transport_owned_by_contextlattice": false,
		"private_key_exported": false,
	}
	writeJSON(w, http.StatusOK, attachPayloadFormatContract(contextMeshEnvelopeContractID, response, passport.Issuer.AgentID, "context_mesh_export", r.URL.Path))
}

func (s *server) reconcileMeshEnvelope(envelope contextMeshEnvelope, apply bool) (map[string]any, int) {
	payload, recipientKeyID, err := s.contextMesh.decryptEnvelope(envelope)
	if err != nil {
		return map[string]any{"ok": false, "schema_id": contextMeshImportContractID, "error": "mesh_import_rejected", "detail": clipText(err.Error(), 500), "applied": false}, http.StatusUnprocessableEntity
	}
	reconciliation := s.contextPassports.plan(payload.Passport)
	if apply {
		reconciliation, err = s.contextPassports.record(payload.Passport)
		if err != nil {
			return map[string]any{"ok": false, "schema_id": contextMeshImportContractID, "error": "mesh_reconciliation_failed", "detail": clipText(err.Error(), 500), "reconciliation": reconciliation, "applied": false}, http.StatusConflict
		}
		if err := s.contextMesh.recordReceipt(envelope, payload.Passport, recipientKeyID, reconciliation); err != nil {
			return map[string]any{"ok": false, "schema_id": contextMeshImportContractID, "error": "mesh_receipt_persist_failed", "detail": clipText(err.Error(), 500), "reconciliation": reconciliation, "applied": true}, http.StatusServiceUnavailable
		}
	}
	response := map[string]any{
		"ok": true, "schema_id": contextMeshImportContractID,
		"envelope_id": envelope.EnvelopeID, "passport_id": payload.Passport.PassportID,
		"project": payload.Project, "reconciliation": reconciliation,
		"applied": apply, "dry_run": !apply, "ordinary_memory_mutated": false,
		"transport_owned_by_contextlattice": false, "private_key_exported": false,
	}
	return response, http.StatusOK
}

func (s *server) memoryContextMeshImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	payload, err := readOptionalJSONBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_json"})
		return
	}
	envelope, err := decodeMeshEnvelope(payload["envelope"])
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_mesh_envelope", "detail": clipText(err.Error(), 500)})
		return
	}
	response, status := s.reconcileMeshEnvelope(envelope, anyToBool(payload["apply"]))
	response = attachPayloadFormatContract(contextMeshImportContractID, response, "", "context_mesh_import", r.URL.Path)
	writeJSON(w, status, response)
}

func (s *server) telemetryContextMesh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.contextMesh.snapshot())
}
