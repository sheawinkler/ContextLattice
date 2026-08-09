package main

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
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
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"filippo.io/age"
)

const (
	contextPassportContractID           = "context_passport.v1"
	contextPassportVerifyContractID     = "context_passport_verify.v1"
	contextPassportDiffContractID       = "context_passport_diff.v1"
	contextPassportReplayContractID     = "context_passport_replay.v1"
	contextPassportStatusSchemaID       = "context_passport_status.v1"
	contextIdentityKeysSchemaID         = "context_identity_keys.v1"
	contextPassportLedgerSchemaID       = "context_passport_record.v1"
	contextPassportLedgerAnchorSchemaID = "context_passport_ledger_anchor.v1"
)

var (
	portableBearerPattern = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{16,}`)
	portableTokenPattern  = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{12,}|ghp_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{20,}|AKIA[0-9A-Z]{16})\b`)
	portableUnixHome      = regexp.MustCompile(`/(?:Users|home)/[^/\s]+`)
	portableWindowsHome   = regexp.MustCompile(`(?i)[A-Z]:\\Users\\[^\\\s]+`)
)

type contextPassportIssuer struct {
	InstanceID       string `json:"instance_id"`
	AgentID          string `json:"agent_id,omitempty"`
	SigningKeyID     string `json:"signing_key_id"`
	SigningPublicKey string `json:"signing_public_key"`
}

type contextPassportSignature struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"key_id"`
	Value     string `json:"value"`
}

type contextPassport struct {
	SchemaID         string                    `json:"schema_id"`
	Version          int                       `json:"version"`
	PassportID       string                    `json:"passport_id"`
	LineageID        string                    `json:"lineage_id"`
	Project          string                    `json:"project"`
	Revision         int                       `json:"revision"`
	ParentPassportID string                    `json:"parent_passport_id,omitempty"`
	ParentDigest     string                    `json:"parent_digest,omitempty"`
	CreatedAt        string                    `json:"created_at"`
	ExpiresAt        string                    `json:"expires_at"`
	Issuer           contextPassportIssuer     `json:"issuer"`
	Scope            map[string]any            `json:"scope"`
	Objective        map[string]any            `json:"objective"`
	Claims           []map[string]any          `json:"claims"`
	Evidence         []map[string]any          `json:"evidence"`
	Lineage          map[string]any            `json:"lineage"`
	Capabilities     []string                  `json:"capabilities"`
	Redactions       map[string]any            `json:"redactions"`
	Replay           map[string]any            `json:"replay"`
	EvidenceIdentity *portableEvidenceIdentity `json:"portable_evidence_identity,omitempty"`
	ContentDigest    string                    `json:"content_digest"`
	Signature        contextPassportSignature  `json:"signature"`
}

type contextPassportUnsigned struct {
	SchemaID         string                    `json:"schema_id"`
	Version          int                       `json:"version"`
	LineageID        string                    `json:"lineage_id"`
	Project          string                    `json:"project"`
	Revision         int                       `json:"revision"`
	ParentPassportID string                    `json:"parent_passport_id,omitempty"`
	ParentDigest     string                    `json:"parent_digest,omitempty"`
	CreatedAt        string                    `json:"created_at"`
	ExpiresAt        string                    `json:"expires_at"`
	Issuer           contextPassportIssuer     `json:"issuer"`
	Scope            map[string]any            `json:"scope"`
	Objective        map[string]any            `json:"objective"`
	Claims           []map[string]any          `json:"claims"`
	Evidence         []map[string]any          `json:"evidence"`
	Lineage          map[string]any            `json:"lineage"`
	Capabilities     []string                  `json:"capabilities"`
	Redactions       map[string]any            `json:"redactions"`
	Replay           map[string]any            `json:"replay"`
	EvidenceIdentity *portableEvidenceIdentity `json:"portable_evidence_identity,omitempty"`
}

type contextPassportSigned struct {
	PassportID    string                  `json:"passport_id"`
	ContentDigest string                  `json:"content_digest"`
	Payload       contextPassportUnsigned `json:"payload"`
}

type contextIdentityKeys struct {
	SchemaID                string `json:"schema_id"`
	Version                 int    `json:"version"`
	InstanceID              string `json:"instance_id"`
	CreatedAt               string `json:"created_at"`
	SigningKeyID            string `json:"signing_key_id"`
	SigningPublicKey        string `json:"signing_public_key"`
	SigningPrivateKey       string `json:"signing_private_key"`
	MeshKeyID               string `json:"mesh_key_id"`
	MeshRecipient           string `json:"mesh_recipient"`
	MeshIdentity            string `json:"mesh_identity"`
	EncryptionFormat        string `json:"encryption_format"`
	EncryptionKeyAgreement  string `json:"encryption_key_agreement"`
	EncryptionPayloadCipher string `json:"encryption_payload_cipher"`
}

type contextPassportReconciliation struct {
	Action            string `json:"action"`
	Conflict          bool   `json:"conflict"`
	Reason            string `json:"reason"`
	PassportID        string `json:"passport_id"`
	LineageID         string `json:"lineage_id"`
	Revision          int    `json:"revision"`
	ExistingPassport  string `json:"existing_passport_id,omitempty"`
	ParentPassportID  string `json:"parent_passport_id,omitempty"`
	Recorded          bool   `json:"recorded"`
	Idempotent        bool   `json:"idempotent"`
	PreservedAsBranch bool   `json:"preserved_as_branch"`
}

type contextPassportLedgerRow struct {
	SchemaID       string                        `json:"schema_id"`
	RecordedAt     string                        `json:"recorded_at"`
	BatchID        string                        `json:"batch_id,omitempty"`
	BatchIndex     int                           `json:"batch_index,omitempty"`
	BatchSize      int                           `json:"batch_size,omitempty"`
	PrevEntryHash  string                        `json:"prev_entry_hash,omitempty"`
	EntryHash      string                        `json:"entry_hash,omitempty"`
	Passport       contextPassport               `json:"passport"`
	Reconciliation contextPassportReconciliation `json:"reconciliation"`
}

// contextPassportLedgerAnchor is an owner-only, bounded durable checkpoint for
// the append-only ledger. The ledger rows carry the chain links so an anchor
// cannot be silently discarded and treated as a legacy file on restart.
type contextPassportLedgerAnchor struct {
	SchemaID         string `json:"schema_id"`
	Version          int    `json:"version"`
	LedgerPathDigest string `json:"ledger_path_digest"`
	Generation       uint64 `json:"generation"`
	EntryCount       int    `json:"entry_count"`
	ChainDigest      string `json:"chain_digest"`
}

type contextPassportStoreConfig struct {
	Enabled      bool
	Path         string
	KeyPath      string
	MaxBytes     int64
	MaxEntries   int
	MaxItemBytes int
	Fsync        bool
}

type contextPassportStore struct {
	mu              sync.RWMutex
	ioMu            sync.Mutex
	enabled         bool
	path            string
	anchorPath      string
	maxBytes        int64
	maxEntries      int
	maxItemBytes    int
	fsync           bool
	identity        *contextIdentityKeys
	passports       map[string]contextPassport
	order           []string
	reconciliations map[string]contextPassportReconciliation
	logEntries      int
	parseErrors     int
	compactions     int
	lastPersistedAt string
	lastError       string
	anchor          contextPassportLedgerAnchor
}

type portableRedactionStats struct {
	SecretKeys int
	Tokens     int
	Paths      int
	Clipped    int
	Lists      int
}

func defaultContextPassportStoreConfig() contextPassportStoreConfig {
	return contextPassportStoreConfig{
		Enabled:      envBool("CONTEXTLATTICE_CONTEXT_PASSPORT_ENABLED", true),
		Path:         resolveStoragePath("CONTEXTLATTICE_CONTEXT_PASSPORT_PATH", filepath.Join(".data", "orchestrator", "context_passports.ndjson")),
		KeyPath:      resolveStoragePath("CONTEXTLATTICE_CONTEXT_IDENTITY_KEY_PATH", filepath.Join(".data", "orchestrator", "context_identity_keys.json")),
		MaxBytes:     int64(clampInt(envInt("CONTEXTLATTICE_CONTEXT_PASSPORT_MAX_BYTES", 16*1024*1024), 256*1024, 128*1024*1024)),
		MaxEntries:   clampInt(envInt("CONTEXTLATTICE_CONTEXT_PASSPORT_MAX_ENTRIES", 512), 16, 10000),
		MaxItemBytes: clampInt(envInt("CONTEXTLATTICE_CONTEXT_PASSPORT_MAX_ITEM_BYTES", 180*1024), 16*1024, 1024*1024),
		Fsync:        envBool("CONTEXTLATTICE_CONTEXT_PASSPORT_FSYNC", true),
	}
}

func newContextPassportStoreFromEnv() (*contextPassportStore, error) {
	return newContextPassportStore(defaultContextPassportStoreConfig())
}

func newContextPassportStore(config contextPassportStoreConfig) (*contextPassportStore, error) {
	store := &contextPassportStore{
		enabled:         config.Enabled,
		path:            strings.TrimSpace(config.Path),
		anchorPath:      strings.TrimSpace(config.Path) + ".anchor",
		maxBytes:        config.MaxBytes,
		maxEntries:      config.MaxEntries,
		maxItemBytes:    config.MaxItemBytes,
		fsync:           config.Fsync,
		passports:       map[string]contextPassport{},
		order:           []string{},
		reconciliations: map[string]contextPassportReconciliation{},
	}
	if !store.enabled || store.path == "" || strings.TrimSpace(config.KeyPath) == "" {
		store.enabled = false
		return store, nil
	}
	identity, err := loadOrCreateContextIdentity(config.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("context identity: %w", err)
	}
	store.identity = identity
	if err := createOwnerOnlyDurableEmptyFileIfMissing(store.anchorPath, false); err != nil {
		return nil, fmt.Errorf("context passport ledger anchor: %w", err)
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

func loadOrCreateContextIdentity(path string) (*contextIdentityKeys, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return nil, errors.New("identity key path is required")
	}
	raw, err := os.ReadFile(path)
	if err == nil {
		var keys contextIdentityKeys
		if err := strictJSONDecode(raw, &keys); err != nil {
			return nil, fmt.Errorf("decode identity key file: %w", err)
		}
		if err := validateContextIdentity(&keys); err != nil {
			return nil, err
		}
		_ = os.Chmod(path, 0o600)
		return &keys, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate signing key: %w", err)
	}
	meshIdentity, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, fmt.Errorf("generate mesh identity: %w", err)
	}
	publicEncoded := base64.RawStdEncoding.EncodeToString(publicKey)
	meshRecipient := meshIdentity.Recipient().String()
	keys := &contextIdentityKeys{
		SchemaID:                contextIdentityKeysSchemaID,
		Version:                 1,
		InstanceID:              "cli_" + randomHex(12),
		CreatedAt:               nowUTCISO(),
		SigningKeyID:            "ed25519:" + digestPrefix(publicEncoded, 24),
		SigningPublicKey:        publicEncoded,
		SigningPrivateKey:       base64.RawStdEncoding.EncodeToString(privateKey),
		MeshKeyID:               "age-x25519:" + digestPrefix(meshRecipient, 24),
		MeshRecipient:           meshRecipient,
		MeshIdentity:            meshIdentity.String(),
		EncryptionFormat:        "age-encryption.org/v1",
		EncryptionKeyAgreement:  "X25519",
		EncryptionPayloadCipher: "ChaCha20-Poly1305",
	}
	if err := validateContextIdentity(keys); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	encoded, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return nil, err
	}
	encoded = append(encoded, '\n')
	if err := writeAtomicFile(path, encoded, 0o600); err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	return keys, nil
}

func validateContextIdentity(keys *contextIdentityKeys) error {
	if keys == nil || keys.SchemaID != contextIdentityKeysSchemaID || keys.Version != 1 {
		return errors.New("unsupported identity key schema")
	}
	publicKey, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(keys.SigningPublicKey))
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return errors.New("invalid identity signing public key")
	}
	privateKey, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(keys.SigningPrivateKey))
	if err != nil || len(privateKey) != ed25519.PrivateKeySize {
		return errors.New("invalid identity signing private key")
	}
	derived := ed25519.PrivateKey(privateKey).Public().(ed25519.PublicKey)
	if !bytes.Equal(derived, publicKey) {
		return errors.New("identity signing key pair mismatch")
	}
	meshIdentity, err := age.ParseX25519Identity(strings.TrimSpace(keys.MeshIdentity))
	if err != nil {
		return errors.New("invalid identity mesh private key")
	}
	if meshIdentity.Recipient().String() != strings.TrimSpace(keys.MeshRecipient) {
		return errors.New("identity mesh key pair mismatch")
	}
	if keys.SigningKeyID != "ed25519:"+digestPrefix(keys.SigningPublicKey, 24) {
		return errors.New("identity signing key id mismatch")
	}
	if keys.MeshKeyID != "age-x25519:"+digestPrefix(keys.MeshRecipient, 24) {
		return errors.New("identity mesh key id mismatch")
	}
	if strings.TrimSpace(keys.InstanceID) == "" {
		return errors.New("identity instance id is required")
	}
	return nil
}

func (k *contextIdentityKeys) publicMetadata() map[string]any {
	if k == nil {
		return map[string]any{"available": false}
	}
	return map[string]any{
		"available": true, "schema_id": contextIdentityKeysSchemaID, "version": 1,
		"instance_id": k.InstanceID, "created_at": k.CreatedAt,
		"signing_key_id": k.SigningKeyID, "signing_public_key": k.SigningPublicKey,
		"mesh_key_id": k.MeshKeyID, "mesh_recipient": k.MeshRecipient,
		"encryption_format":         k.EncryptionFormat,
		"encryption_key_agreement":  k.EncryptionKeyAgreement,
		"encryption_payload_cipher": k.EncryptionPayloadCipher,
		"private_key_exported":      false,
	}
}

func strictJSONDecode(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values are not allowed")
	}
	return nil
}

func randomHex(bytesCount int) string {
	if bytesCount < 1 {
		bytesCount = 1
	}
	raw := make([]byte, bytesCount)
	if _, err := rand.Read(raw); err != nil {
		sum := sha256.Sum256([]byte(nowUTCISO()))
		return hex.EncodeToString(sum[:bytesCount])
	}
	return hex.EncodeToString(raw)
}

func digestPrefix(value string, length int) string {
	sum := sha256.Sum256([]byte(value))
	encoded := hex.EncodeToString(sum[:])
	if length > len(encoded) {
		length = len(encoded)
	}
	return encoded[:length]
}

func passportUnsigned(passport contextPassport) contextPassportUnsigned {
	return contextPassportUnsigned{
		SchemaID: passport.SchemaID, Version: passport.Version,
		LineageID: passport.LineageID, Project: passport.Project, Revision: passport.Revision,
		ParentPassportID: passport.ParentPassportID, ParentDigest: passport.ParentDigest,
		CreatedAt: passport.CreatedAt, ExpiresAt: passport.ExpiresAt, Issuer: passport.Issuer,
		Scope: passport.Scope, Objective: passport.Objective, Claims: passport.Claims,
		Evidence: passport.Evidence, Lineage: passport.Lineage, Capabilities: passport.Capabilities,
		Redactions: passport.Redactions, Replay: passport.Replay, EvidenceIdentity: passport.EvidenceIdentity,
	}
}

func signContextPassport(passport *contextPassport, keys *contextIdentityKeys) error {
	if passport == nil || keys == nil {
		return errors.New("passport and signing identity are required")
	}
	unsigned := passportUnsigned(*passport)
	payload, err := json.Marshal(unsigned)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(payload)
	passport.ContentDigest = "sha256:" + hex.EncodeToString(digest[:])
	passport.PassportID = "passport_" + hex.EncodeToString(digest[:12])
	signedBytes, err := json.Marshal(contextPassportSigned{PassportID: passport.PassportID, ContentDigest: passport.ContentDigest, Payload: unsigned})
	if err != nil {
		return err
	}
	privateKey, err := base64.RawStdEncoding.DecodeString(keys.SigningPrivateKey)
	if err != nil || len(privateKey) != ed25519.PrivateKeySize {
		return errors.New("invalid local signing key")
	}
	passport.Signature = contextPassportSignature{
		Algorithm: "Ed25519", KeyID: keys.SigningKeyID,
		Value: base64.RawStdEncoding.EncodeToString(ed25519.Sign(ed25519.PrivateKey(privateKey), signedBytes)),
	}
	return nil
}

func verifyContextPassport(passport contextPassport, now time.Time, checkExpiry bool) []string {
	errorsOut := []string{}
	if passport.SchemaID != contextPassportContractID || passport.Version != 1 {
		errorsOut = append(errorsOut, "unsupported_schema")
	}
	if strings.TrimSpace(passport.Project) == "" || strings.TrimSpace(passport.LineageID) == "" || passport.Revision < 1 {
		errorsOut = append(errorsOut, "invalid_identity_fields")
	}
	unsigned := passportUnsigned(passport)
	payload, err := json.Marshal(unsigned)
	if err != nil {
		return append(errorsOut, "canonicalization_failed")
	}
	digest := sha256.Sum256(payload)
	expectedDigest := "sha256:" + hex.EncodeToString(digest[:])
	expectedID := "passport_" + hex.EncodeToString(digest[:12])
	if passport.ContentDigest != expectedDigest {
		errorsOut = append(errorsOut, "content_digest_mismatch")
	}
	if passport.PassportID != expectedID {
		errorsOut = append(errorsOut, "passport_id_mismatch")
	}
	if err := validatePortableEvidenceIdentity(passport.EvidenceIdentity); err != nil {
		errorsOut = append(errorsOut, "portable_evidence_identity_invalid")
	}
	if passport.Signature.Algorithm != "Ed25519" || passport.Signature.KeyID != passport.Issuer.SigningKeyID {
		errorsOut = append(errorsOut, "signature_metadata_mismatch")
	} else {
		if passport.Issuer.SigningKeyID != "ed25519:"+digestPrefix(passport.Issuer.SigningPublicKey, 24) {
			errorsOut = append(errorsOut, "issuer_key_id_mismatch")
		}
		publicKey, publicErr := base64.RawStdEncoding.DecodeString(passport.Issuer.SigningPublicKey)
		signature, signatureErr := base64.RawStdEncoding.DecodeString(passport.Signature.Value)
		signedBytes, marshalErr := json.Marshal(contextPassportSigned{PassportID: passport.PassportID, ContentDigest: passport.ContentDigest, Payload: unsigned})
		if publicErr != nil || len(publicKey) != ed25519.PublicKeySize || signatureErr != nil || len(signature) != ed25519.SignatureSize || marshalErr != nil || !ed25519.Verify(ed25519.PublicKey(publicKey), signedBytes, signature) {
			errorsOut = append(errorsOut, "signature_invalid")
		}
	}
	createdAt, createdErr := time.Parse(time.RFC3339Nano, passport.CreatedAt)
	expiresAt, expiresErr := time.Parse(time.RFC3339Nano, passport.ExpiresAt)
	if createdErr != nil || expiresErr != nil || !expiresAt.After(createdAt) {
		errorsOut = append(errorsOut, "invalid_validity_window")
	} else if checkExpiry && !now.Before(expiresAt) {
		errorsOut = append(errorsOut, "passport_expired")
	}
	if passport.Revision == 1 && (passport.ParentPassportID != "" || passport.ParentDigest != "") {
		errorsOut = append(errorsOut, "root_has_parent")
	}
	if passport.Revision > 1 && (passport.ParentPassportID == "" || passport.ParentDigest == "") {
		errorsOut = append(errorsOut, "revision_missing_parent")
	}
	return uniqueSortedStrings(errorsOut)
}

func decodeContextPassport(value any) (contextPassport, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return contextPassport{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return contextPassport{}, err
	}
	if identity, present := fields["portable_evidence_identity"]; present {
		if _, err := decodePortableEvidenceIdentity(identity); err != nil {
			return contextPassport{}, fmt.Errorf("portable evidence identity: %w", err)
		}
	}
	var passport contextPassport
	if err := strictJSONDecode(raw, &passport); err != nil {
		return contextPassport{}, err
	}
	return passport, nil
}

func contextPassportLedgerPathDigest(path string) string {
	return "sha256:" + digestPrefix("context-passport-ledger-path:"+filepath.Clean(path), 64)
}

func contextPassportLedgerGenesisDigest(path string) string {
	return "sha256:" + digestPrefix("context-passport-ledger-genesis:"+contextPassportLedgerPathDigest(path), 64)
}

func emptyContextPassportLedgerAnchor(path string) contextPassportLedgerAnchor {
	return contextPassportLedgerAnchor{
		SchemaID: contextPassportLedgerAnchorSchemaID, Version: 1,
		LedgerPathDigest: contextPassportLedgerPathDigest(path),
		ChainDigest:      contextPassportLedgerGenesisDigest(path),
	}
}

func contextPassportLedgerUnsignedRow(row contextPassportLedgerRow) contextPassportLedgerRow {
	row.PrevEntryHash = ""
	row.EntryHash = ""
	return row
}

func contextPassportLedgerRowDigest(previous string, row contextPassportLedgerRow) (string, error) {
	encoded, err := json.Marshal(contextPassportLedgerUnsignedRow(row))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append(append([]byte{}, []byte(previous)...), append([]byte{'\n'}, encoded...)...))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func contextPassportLedgerChainRows(path string, rows []contextPassportLedgerRow, generation uint64) ([]contextPassportLedgerRow, contextPassportLedgerAnchor, error) {
	anchor := emptyContextPassportLedgerAnchor(path)
	anchor.Generation = generation
	anchored := make([]contextPassportLedgerRow, len(rows))
	previous := anchor.ChainDigest
	for index, row := range rows {
		row.PrevEntryHash = previous
		entryHash, err := contextPassportLedgerRowDigest(previous, row)
		if err != nil {
			return nil, contextPassportLedgerAnchor{}, err
		}
		row.EntryHash = entryHash
		anchored[index] = row
		previous = entryHash
	}
	anchor.EntryCount = len(anchored)
	anchor.ChainDigest = previous
	return anchored, anchor, nil
}

func validateContextPassportLedgerAnchor(anchor contextPassportLedgerAnchor, path string) error {
	if anchor.SchemaID != contextPassportLedgerAnchorSchemaID || anchor.Version != 1 ||
		anchor.LedgerPathDigest != contextPassportLedgerPathDigest(path) || anchor.Generation == 0 || anchor.EntryCount < 0 ||
		!strings.HasPrefix(anchor.ChainDigest, "sha256:") || len(strings.TrimPrefix(anchor.ChainDigest, "sha256:")) != sha256.Size*2 {
		return errors.New("context passport ledger anchor is invalid")
	}
	return nil
}

func (s *contextPassportStore) readLedgerAnchor() (contextPassportLedgerAnchor, bool, error) {
	raw, err := os.ReadFile(s.anchorPath)
	if errors.Is(err, os.ErrNotExist) {
		return emptyContextPassportLedgerAnchor(s.path), false, nil
	}
	if err != nil {
		return contextPassportLedgerAnchor{}, true, fmt.Errorf("read context passport ledger anchor: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return emptyContextPassportLedgerAnchor(s.path), false, nil
	}
	var anchor contextPassportLedgerAnchor
	if err := strictJSONDecode(bytes.TrimSpace(raw), &anchor); err != nil {
		return contextPassportLedgerAnchor{}, true, fmt.Errorf("decode context passport ledger anchor: %w", err)
	}
	if err := validateContextPassportLedgerAnchor(anchor, s.path); err != nil {
		return contextPassportLedgerAnchor{}, true, fmt.Errorf("validate context passport ledger anchor: %w", err)
	}
	return anchor, true, nil
}

func (s *contextPassportStore) persistLedgerAnchor(anchor contextPassportLedgerAnchor) error {
	if err := validateContextPassportLedgerAnchor(anchor, s.path); err != nil {
		return err
	}
	s.mu.RLock()
	currentGeneration := s.anchor.Generation
	s.mu.RUnlock()
	if currentGeneration > 0 && anchor.Generation <= currentGeneration {
		return errors.New("context passport ledger anchor generation is not monotonic")
	}
	encoded, err := json.Marshal(anchor)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if err := writeOwnerOnlyDurableAtomicFile(s.anchorPath, encoded, false); err != nil {
		return err
	}
	s.mu.Lock()
	s.anchor = anchor
	s.mu.Unlock()
	return nil
}

func (s *contextPassportStore) recordWriteError(err error) error {
	if err == nil || s == nil {
		return err
	}
	s.mu.Lock()
	s.lastError = err.Error()
	if ownerOnlyAtomicWriteCommitted(err) {
		s.enabled = false
		s.lastError = "commit_unknown: " + err.Error()
	}
	s.mu.Unlock()
	return err
}

func validateContextPassportLedgerRow(row contextPassportLedgerRow) error {
	if row.SchemaID != contextPassportLedgerSchemaID {
		return errors.New("context passport ledger row schema is invalid")
	}
	if findings := verifyContextPassport(row.Passport, time.Now().UTC(), false); len(findings) > 0 {
		return fmt.Errorf("context passport ledger row passport is invalid: %s", strings.Join(findings, ","))
	}
	if row.BatchID == "" {
		if row.BatchIndex != 0 || row.BatchSize != 0 {
			return errors.New("context passport ledger row batch metadata is invalid")
		}
	} else if row.BatchSize < 1 || row.BatchSize > 32 || row.BatchIndex < 0 || row.BatchIndex >= row.BatchSize {
		return errors.New("context passport ledger row batch metadata is invalid")
	}
	return nil
}

func validateCompleteContextPassportLedgerBatches(rows []contextPassportLedgerRow) error {
	type batchState struct {
		size    int
		indices map[int]struct{}
	}
	states := map[string]*batchState{}
	for _, row := range rows {
		if row.BatchID == "" {
			continue
		}
		state := states[row.BatchID]
		if state == nil {
			state = &batchState{size: row.BatchSize, indices: map[int]struct{}{}}
			states[row.BatchID] = state
		}
		if state.size != row.BatchSize {
			return fmt.Errorf("context passport ledger batch %q has inconsistent size", row.BatchID)
		}
		if _, exists := state.indices[row.BatchIndex]; exists {
			return fmt.Errorf("context passport ledger batch %q has duplicate index", row.BatchID)
		}
		state.indices[row.BatchIndex] = struct{}{}
	}
	for batchID, state := range states {
		if len(state.indices) != state.size {
			return fmt.Errorf("context passport ledger batch %q is incomplete", batchID)
		}
	}
	return nil
}

func (s *contextPassportStore) verifyAnchoredLedger(rows []contextPassportLedgerRow, anchor contextPassportLedgerAnchor) error {
	if anchor.EntryCount != len(rows) {
		return fmt.Errorf("context passport ledger entry count rolled back or truncated: anchor=%d rows=%d", anchor.EntryCount, len(rows))
	}
	previous := contextPassportLedgerGenesisDigest(s.path)
	for index, row := range rows {
		if row.PrevEntryHash == "" || row.EntryHash == "" || row.PrevEntryHash != previous {
			return fmt.Errorf("context passport ledger hash chain is invalid at row %d", index)
		}
		expected, err := contextPassportLedgerRowDigest(previous, row)
		if err != nil {
			return fmt.Errorf("hash context passport ledger row %d: %w", index, err)
		}
		if row.EntryHash != expected {
			return fmt.Errorf("context passport ledger entry hash is invalid at row %d", index)
		}
		previous = row.EntryHash
	}
	if anchor.ChainDigest != previous {
		return errors.New("context passport ledger anchor digest mismatch")
	}
	return nil
}

func (s *contextPassportStore) migrateLegacyLedger(rows []contextPassportLedgerRow) error {
	anchoredRows, anchor, err := contextPassportLedgerChainRows(s.path, rows, 1)
	if err != nil {
		return fmt.Errorf("chain legacy context passport ledger: %w", err)
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	for _, row := range anchoredRows {
		if err := encoder.Encode(row); err != nil {
			return err
		}
	}
	if err := writeOwnerOnlyDurableAtomicFile(s.path, buffer.Bytes(), false); err != nil {
		return fmt.Errorf("migrate legacy context passport ledger: %w", err)
	}
	if err := s.persistLedgerAnchor(anchor); err != nil {
		return fmt.Errorf("anchor migrated context passport ledger: %w", err)
	}
	return nil
}

func (s *contextPassportStore) load() error {
	anchor, anchorPresent, err := s.readLedgerAnchor()
	if err != nil {
		return err
	}
	file, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		if anchorPresent && anchor.EntryCount != 0 {
			return errors.New("context passport ledger is missing behind its durable anchor")
		}
		s.anchor = anchor
		return nil
	}
	if err != nil {
		return fmt.Errorf("open context passport ledger: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), maxInt(s.maxItemBytes*2, 2*1024*1024))
	rows := []contextPassportLedgerRow{}
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var row contextPassportLedgerRow
		if err := strictJSONDecode(line, &row); err != nil {
			s.parseErrors++
			return fmt.Errorf("context passport ledger row %d is malformed: %w", lineNumber, err)
		}
		if err := validateContextPassportLedgerRow(row); err != nil {
			s.parseErrors++
			return fmt.Errorf("context passport ledger row %d is invalid: %w", lineNumber, err)
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan context passport ledger: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close context passport ledger: %w", err)
	}
	if err := validateCompleteContextPassportLedgerBatches(rows); err != nil {
		s.parseErrors++
		return err
	}
	if anchorPresent {
		if err := s.verifyAnchoredLedger(rows, anchor); err != nil {
			s.parseErrors++
			return err
		}
		s.anchor = anchor
	} else if len(rows) > 0 {
		for _, row := range rows {
			if row.PrevEntryHash != "" || row.EntryHash != "" {
				return errors.New("context passport ledger chain exists without its durable anchor")
			}
		}
		if err := s.migrateLegacyLedger(rows); err != nil {
			return err
		}
	} else {
		s.anchor = emptyContextPassportLedgerAnchor(s.path)
	}
	for _, row := range rows {
		s.applyLedgerRowLocked(row)
		s.logEntries++
	}
	s.trimLocked()
	return nil
}

func (s *contextPassportStore) applyLedgerRowLocked(row contextPassportLedgerRow) {
	id := row.Passport.PassportID
	if _, exists := s.passports[id]; !exists {
		s.order = append(s.order, id)
	}
	s.passports[id] = row.Passport
	s.reconciliations[id] = row.Reconciliation
}

func (s *contextPassportStore) trimLocked() {
	for len(s.order) > s.maxEntries {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.passports, oldest)
		delete(s.reconciliations, oldest)
	}
}

func (s *contextPassportStore) get(id string) (contextPassport, bool) {
	if s == nil {
		return contextPassport{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.enabled {
		return contextPassport{}, false
	}
	passport, ok := s.passports[strings.TrimSpace(id)]
	return passport, ok
}

func (s *contextPassportStore) planLocked(passport contextPassport) contextPassportReconciliation {
	return planContextPassport(passport, s.passports)
}

func planContextPassport(passport contextPassport, passports map[string]contextPassport) contextPassportReconciliation {
	result := contextPassportReconciliation{
		Action: "record", PassportID: passport.PassportID, LineageID: passport.LineageID,
		Revision: passport.Revision, ParentPassportID: passport.ParentPassportID,
	}
	if existing, ok := passports[passport.PassportID]; ok {
		if existing.ContentDigest == passport.ContentDigest {
			result.Action, result.Reason, result.Idempotent = "idempotent", "identical_digest_already_present", true
			return result
		}
		result.Action, result.Reason, result.Conflict, result.ExistingPassport = "conflict", "passport_id_digest_collision", true, existing.PassportID
		return result
	}
	for _, existing := range passports {
		if existing.LineageID != passport.LineageID {
			continue
		}
		if existing.Revision == passport.Revision {
			result.Action, result.Reason, result.Conflict = "conflict_branch", "same_lineage_revision_has_different_digest", true
			result.ExistingPassport, result.PreservedAsBranch = existing.PassportID, true
			return result
		}
	}
	if passport.Revision == 1 {
		result.Action, result.Reason = "record_root", "new_lineage_root"
		return result
	}
	parent, ok := passports[passport.ParentPassportID]
	if !ok {
		result.Action, result.Reason, result.Conflict, result.PreservedAsBranch = "conflict_branch", "parent_not_present", true, true
		return result
	}
	if parent.ContentDigest != passport.ParentDigest || parent.LineageID != passport.LineageID || parent.Revision+1 != passport.Revision {
		result.Action, result.Reason, result.Conflict, result.ExistingPassport, result.PreservedAsBranch = "conflict_branch", "parent_lineage_or_revision_mismatch", true, parent.PassportID, true
		return result
	}
	result.Action, result.Reason = "advance", "parent_digest_and_revision_match"
	return result
}

func clonePassports(source map[string]contextPassport) map[string]contextPassport {
	cloned := make(map[string]contextPassport, len(source))
	for id, passport := range source {
		cloned[id] = passport
	}
	return cloned
}

func planContextPassportBatch(passports []contextPassport, existing map[string]contextPassport) []contextPassportReconciliation {
	shadow := clonePassports(existing)
	results := make([]contextPassportReconciliation, 0, len(passports))
	for _, passport := range passports {
		reconciliation := planContextPassport(passport, shadow)
		results = append(results, reconciliation)
		if !reconciliation.Idempotent && (!reconciliation.Conflict || reconciliation.PreservedAsBranch) {
			shadow[passport.PassportID] = passport
		}
	}
	return results
}

func (s *contextPassportStore) planBatch(passports []contextPassport) []contextPassportReconciliation {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.enabled {
		return nil
	}
	return planContextPassportBatch(passports, s.passports)
}

func (s *contextPassportStore) plan(passport contextPassport) contextPassportReconciliation {
	if s == nil {
		return contextPassportReconciliation{Action: "rejected", Reason: "passport_store_disabled", PassportID: passport.PassportID, Conflict: true}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.enabled {
		return contextPassportReconciliation{Action: "rejected", Reason: "passport_store_disabled", PassportID: passport.PassportID, Conflict: true}
	}
	return s.planLocked(passport)
}

func (s *contextPassportStore) record(passport contextPassport) (contextPassportReconciliation, error) {
	if s == nil {
		return contextPassportReconciliation{}, errors.New("context passport store disabled")
	}
	if validation := verifyContextPassport(passport, time.Now().UTC(), false); len(validation) > 0 {
		return contextPassportReconciliation{}, fmt.Errorf("invalid context passport: %s", strings.Join(validation, ","))
	}
	encodedPassport, err := json.Marshal(passport)
	if err != nil {
		return contextPassportReconciliation{}, err
	}
	if len(encodedPassport) > s.maxItemBytes {
		return contextPassportReconciliation{}, fmt.Errorf("passport exceeds %d byte limit", s.maxItemBytes)
	}

	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	s.mu.RLock()
	enabled := s.enabled
	s.mu.RUnlock()
	if !enabled {
		return contextPassportReconciliation{}, errors.New("context passport store disabled")
	}
	s.mu.Lock()
	reconciliation := s.planLocked(passport)
	if reconciliation.Idempotent {
		s.mu.Unlock()
		return reconciliation, nil
	}
	if reconciliation.Conflict && !reconciliation.PreservedAsBranch {
		s.mu.Unlock()
		return reconciliation, errors.New(reconciliation.Reason)
	}
	reconciliation.Recorded = true
	row := contextPassportLedgerRow{SchemaID: contextPassportLedgerSchemaID, RecordedAt: nowUTCISO(), Passport: passport, Reconciliation: reconciliation}
	s.mu.Unlock()
	if err := s.appendRows([]contextPassportLedgerRow{row}); err != nil {
		return reconciliation, err
	}
	s.mu.Lock()
	if _, exists := s.passports[passport.PassportID]; !exists {
		s.order = append(s.order, passport.PassportID)
	}
	s.passports[passport.PassportID] = passport
	s.reconciliations[passport.PassportID] = reconciliation
	s.trimLocked()
	s.mu.Unlock()
	if err := s.compactIfNeededLocked(); err != nil {
		return reconciliation, err
	}
	return reconciliation, nil
}

func (s *contextPassportStore) recordBatch(passports []contextPassport, requireConflictFree bool) ([]contextPassportReconciliation, error) {
	if s == nil {
		return nil, errors.New("context passport store disabled")
	}
	if len(passports) == 0 || len(passports) > 32 {
		return nil, errors.New("one to 32 context passports are required")
	}
	for _, passport := range passports {
		if validation := verifyContextPassport(passport, time.Now().UTC(), false); len(validation) > 0 {
			return nil, fmt.Errorf("invalid context passport %s: %s", passport.PassportID, strings.Join(validation, ","))
		}
		encodedPassport, err := json.Marshal(passport)
		if err != nil {
			return nil, err
		}
		if len(encodedPassport) > s.maxItemBytes {
			return nil, fmt.Errorf("passport %s exceeds %d byte limit", passport.PassportID, s.maxItemBytes)
		}
	}

	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	s.mu.RLock()
	enabled := s.enabled
	s.mu.RUnlock()
	if !enabled {
		return nil, errors.New("context passport store disabled")
	}
	s.mu.Lock()
	reconciliations := planContextPassportBatch(passports, s.passports)
	rows := make([]contextPassportLedgerRow, 0, len(passports))
	var rejectionReason string
	for index, passport := range passports {
		reconciliation := reconciliations[index]
		if reconciliation.Idempotent {
			continue
		}
		if reconciliation.Conflict && (requireConflictFree || !reconciliation.PreservedAsBranch) {
			if rejectionReason == "" {
				rejectionReason = reconciliation.Reason
			}
			continue
		}
		reconciliation.Recorded = true
		reconciliations[index] = reconciliation
		rows = append(rows, contextPassportLedgerRow{
			SchemaID: contextPassportLedgerSchemaID, RecordedAt: nowUTCISO(),
			Passport: passport, Reconciliation: reconciliation,
		})
	}
	s.mu.Unlock()
	if rejectionReason != "" {
		for index := range reconciliations {
			reconciliations[index].Recorded = false
		}
		return reconciliations, fmt.Errorf("batch reconciliation rejected: %s", rejectionReason)
	}
	if len(rows) == 0 {
		return reconciliations, nil
	}
	batchID := "passport_batch_" + randomHex(12)
	for index := range rows {
		rows[index].BatchID = batchID
		rows[index].BatchIndex = index
		rows[index].BatchSize = len(rows)
	}
	if err := s.appendRows(rows); err != nil {
		return reconciliations, err
	}
	s.mu.Lock()
	for _, row := range rows {
		s.applyLedgerRowLocked(row)
	}
	s.trimLocked()
	s.mu.Unlock()
	if err := s.compactIfNeededLocked(); err != nil {
		return reconciliations, err
	}
	return reconciliations, nil
}

func (s *contextPassportStore) appendRows(rows []contextPassportLedgerRow) error {
	if len(rows) == 0 {
		return nil
	}
	s.mu.RLock()
	previous := s.anchor.ChainDigest
	generation := s.anchor.Generation + 1
	if previous == "" {
		previous = contextPassportLedgerGenesisDigest(s.path)
	}
	s.mu.RUnlock()
	anchoredRows, nextAnchor, err := contextPassportLedgerChainRows(s.path, rows, generation)
	if err != nil {
		return err
	}
	// The chain builder starts from the path-specific genesis. Continue from
	// the existing durable anchor for normal appends.
	if previous != contextPassportLedgerGenesisDigest(s.path) {
		for index, row := range anchoredRows {
			row.PrevEntryHash = previous
			entryHash, digestErr := contextPassportLedgerRowDigest(previous, row)
			if digestErr != nil {
				return digestErr
			}
			row.EntryHash = entryHash
			anchoredRows[index] = row
			previous = entryHash
		}
		nextAnchor.ChainDigest = previous
	}
	s.mu.RLock()
	nextAnchor.EntryCount = s.anchor.EntryCount + len(anchoredRows)
	nextAnchor.Generation = s.anchor.Generation + 1
	s.mu.RUnlock()
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	for _, row := range anchoredRows {
		if err := encoder.Encode(row); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	file, err := openOwnerOnlyAppend(s.path, false)
	if err != nil {
		return s.recordWriteError(err)
	}
	committed := false
	for buffer.Len() > 0 {
		written, writeErr := file.Write(buffer.Bytes())
		if written > 0 {
			buffer.Next(written)
			committed = true
		}
		if writeErr != nil {
			_ = file.Close()
			return s.recordWriteError(&ownerOnlyAtomicWriteError{Operation: "append context passport ledger", Committed: committed, Err: writeErr})
		}
		if written == 0 {
			_ = file.Close()
			return s.recordWriteError(&ownerOnlyAtomicWriteError{Operation: "append context passport ledger", Committed: committed, Err: io.ErrShortWrite})
		}
	}
	if s.fsync {
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return s.recordWriteError(&ownerOnlyAtomicWriteError{Operation: "sync context passport ledger", Committed: committed, Err: err})
		}
	}
	if err := file.Close(); err != nil {
		return s.recordWriteError(&ownerOnlyAtomicWriteError{Operation: "close context passport ledger", Committed: committed, Err: err})
	}
	if err := s.persistLedgerAnchor(nextAnchor); err != nil {
		return s.recordWriteError(&ownerOnlyAtomicWriteError{Operation: "persist context passport ledger anchor", Committed: true, Err: err})
	}
	s.mu.Lock()
	s.logEntries += len(anchoredRows)
	s.lastPersistedAt = nowUTCISO()
	s.lastError = ""
	s.mu.Unlock()
	return nil
}

func (s *contextPassportStore) compactIfNeeded() error {
	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	return s.compactIfNeededLocked()
}

func (s *contextPassportStore) compactIfNeededLocked() error {
	info, err := os.Stat(s.path)
	if err != nil || info.Size() <= s.maxBytes {
		return nil
	}
	s.mu.Lock()
	retainedIDs := make([]string, 0, len(s.order))
	retainedRowsReversed := make([]contextPassportLedgerRow, 0, len(s.order))
	var retainedBytes int64
	for index := len(s.order) - 1; index >= 0; index-- {
		id := s.order[index]
		passport, ok := s.passports[id]
		if !ok {
			continue
		}
		row := contextPassportLedgerRow{SchemaID: contextPassportLedgerSchemaID, RecordedAt: nowUTCISO(), Passport: passport, Reconciliation: s.reconciliations[id]}
		encoded, _ := json.Marshal(row)
		rowBytes := int64(len(encoded) + 1)
		if len(retainedRowsReversed) > 0 && retainedBytes+rowBytes > s.maxBytes {
			continue
		}
		retainedBytes += rowBytes
		retainedIDs = append(retainedIDs, id)
		retainedRowsReversed = append(retainedRowsReversed, row)
	}
	for left, right := 0, len(retainedIDs)-1; left < right; left, right = left+1, right-1 {
		retainedIDs[left], retainedIDs[right] = retainedIDs[right], retainedIDs[left]
		retainedRowsReversed[left], retainedRowsReversed[right] = retainedRowsReversed[right], retainedRowsReversed[left]
	}
	rows, nextAnchor, err := contextPassportLedgerChainRows(s.path, retainedRowsReversed, s.anchor.Generation+1)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	for _, row := range rows {
		if err := encoder.Encode(row); err != nil {
			return err
		}
	}
	if err := writeOwnerOnlyDurableAtomicFile(s.path, buffer.Bytes(), false); err != nil {
		return s.recordWriteError(err)
	}
	if err := s.persistLedgerAnchor(nextAnchor); err != nil {
		return s.recordWriteError(&ownerOnlyAtomicWriteError{Operation: "persist compacted context passport ledger anchor", Committed: true, Err: err})
	}
	s.mu.Lock()
	retainedSet := map[string]struct{}{}
	for _, id := range retainedIDs {
		retainedSet[id] = struct{}{}
	}
	for id := range s.passports {
		if _, keep := retainedSet[id]; !keep {
			delete(s.passports, id)
			delete(s.reconciliations, id)
		}
	}
	s.order = retainedIDs
	s.compactions++
	s.logEntries = len(rows)
	s.lastPersistedAt = nowUTCISO()
	s.mu.Unlock()
	return nil
}

func (s *contextPassportStore) snapshot() map[string]any {
	if s == nil {
		return map[string]any{"schema_id": contextPassportStatusSchemaID, "enabled": false, "passport_count": 0}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	lineages := map[string]int{}
	conflicts := 0
	latest := make([]map[string]any, 0, minInt(len(s.order), 32))
	for index := len(s.order) - 1; index >= 0; index-- {
		passport := s.passports[s.order[index]]
		lineages[passport.LineageID]++
		if s.reconciliations[passport.PassportID].Conflict {
			conflicts++
		}
		if len(latest) < 32 {
			latest = append(latest, map[string]any{
				"passport_id": passport.PassportID, "lineage_id": passport.LineageID,
				"project": passport.Project, "revision": passport.Revision,
				"created_at": passport.CreatedAt, "expires_at": passport.ExpiresAt,
				"content_digest": passport.ContentDigest,
				"reconciliation": s.reconciliations[passport.PassportID],
			})
		}
	}
	return map[string]any{
		"schema_id": contextPassportStatusSchemaID, "version": 1, "enabled": s.enabled,
		"passport_count": len(s.passports), "lineage_count": len(lineages), "conflict_count": conflicts,
		"latest": latest, "identity": s.identity.publicMetadata(),
		"storage": map[string]any{
			"max_bytes": s.maxBytes, "max_entries": s.maxEntries, "max_item_bytes": s.maxItemBytes,
			"log_entries": s.logEntries, "parse_errors": s.parseErrors, "compaction_count": s.compactions,
			"last_persisted_at": s.lastPersistedAt, "last_error": s.lastError,
		},
		"private_key_exported": false, "transport": "external_to_contextlattice",
	}
}

func portableCanonicalKey(key string) string {
	var normalized strings.Builder
	var lastWritten rune
	runes := []rune(strings.TrimSpace(key))
	for index, current := range runes {
		if unicode.IsUpper(current) {
			if normalized.Len() > 0 {
				previous := runes[index-1]
				var next rune
				if index+1 < len(runes) {
					next = runes[index+1]
				}
				if previous != '_' && (unicode.IsLower(previous) || unicode.IsDigit(previous) || (unicode.IsUpper(previous) && unicode.IsLower(next))) {
					normalized.WriteByte('_')
					lastWritten = '_'
				}
			}
			current = unicode.ToLower(current)
		}
		if unicode.IsLetter(current) || unicode.IsDigit(current) {
			normalized.WriteRune(current)
			lastWritten = current
			continue
		}
		if normalized.Len() > 0 && lastWritten != '_' {
			normalized.WriteByte('_')
			lastWritten = '_'
		}
	}
	return strings.Trim(normalized.String(), "_")
}

func portableSecretKey(key string) bool {
	normalized := portableCanonicalKey(key)
	if normalized == "token_budget" || normalized == "max_prompt_tokens" || normalized == "prompt_tokens" || normalized == "provider_tokens" || normalized == "estimated_tokens" {
		return false
	}
	for _, fragment := range []string{"secret", "password", "api_key", "apikey", "authorization", "private_key", "credential", "access_token", "refresh_token", "bearer"} {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return normalized == "token"
}

func portableString(value string, stats *portableRedactionStats) string {
	cleaned := portableBearerPattern.ReplaceAllString(value, "[bearer-redacted]")
	if cleaned != value {
		stats.Tokens++
	}
	next := portableTokenPattern.ReplaceAllString(cleaned, "[token-redacted]")
	if next != cleaned {
		stats.Tokens++
	}
	cleaned = next
	next = portableUnixHome.ReplaceAllString(cleaned, "[local-root]")
	next = portableWindowsHome.ReplaceAllString(next, "[local-root]")
	if next != cleaned {
		stats.Paths++
	}
	cleaned = next
	if len(cleaned) > 4000 {
		cleaned = clipText(cleaned, 4000)
		stats.Clipped++
	}
	return cleaned
}

func portableValue(value any, depth int, stats *portableRedactionStats) any {
	if depth > 8 {
		stats.Clipped++
		return "[depth-clipped]"
	}
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := map[string]any{}
		for _, key := range keys {
			if portableSecretKey(key) {
				stats.SecretKeys++
				continue
			}
			out[key] = portableValue(typed[key], depth+1, stats)
		}
		return out
	case []any:
		limit := minInt(len(typed), 64)
		out := make([]any, 0, limit)
		for _, item := range typed[:limit] {
			out = append(out, portableValue(item, depth+1, stats))
		}
		if len(typed) > limit {
			stats.Lists += len(typed) - limit
		}
		return out
	case []map[string]any:
		items := make([]any, 0, len(typed))
		for _, item := range typed {
			items = append(items, item)
		}
		return portableValue(items, depth, stats)
	case string:
		return portableString(typed, stats)
	case json.Number, float64, float32, int, int64, int32, uint, uint64, uint32, bool, nil:
		return typed
	default:
		return portableString(anyToString(typed), stats)
	}
}

func portableMap(value map[string]any, stats *portableRedactionStats) map[string]any {
	result, _ := portableValue(value, 0, stats).(map[string]any)
	if result == nil {
		return map[string]any{}
	}
	return result
}

func portableRows(values []any, limit int, stats *portableRedactionStats, idKeys ...string) []map[string]any {
	if limit < 1 {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, minInt(len(values), limit))
	for _, raw := range values {
		if len(out) >= limit {
			break
		}
		row := portableMap(anyMap(raw), stats)
		if len(row) == 0 {
			continue
		}
		identity := ""
		for _, key := range idKeys {
			identity = firstNonEmptyStrings(identity, anyToString(row[key]))
		}
		if identity == "" {
			encoded, _ := json.Marshal(row)
			identity = "ref_" + digestPrefix(string(encoded), 24)
		}
		row["portable_id"] = portableString(identity, stats)
		out = append(out, row)
	}
	return out
}

func uniqueSortedStrings(values []string) []string {
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func defaultPassportCapabilities() []string {
	return []string{
		"adaptive_retrieval_planner.v1", "context_pack.v1", "context_passport.v1",
		"proof_carrying_synthesis.v2", "temporal_claim_graph.v1",
	}
}

func contextPassportAvailableCapabilities() map[string]struct{} {
	values := []string{
		"adaptive_retrieval_planner.v1", "agent_adoption.v1", "agent_lifecycle.v1",
		"async_retrieval_warming.v1", "cli.primary.v1", "context_mesh.v1", "context_pack.v1",
		"context_passport.v1", "memory.write.v1", "memory_graph_recall.v1", "omp_mercury_integration.v1",
		"outcome_context_policy.v1", "pi_droid_runners.v1", "proof_carrying_synthesis.v2",
		"runner_quality.v1", "runtime_policy.v1", "skill_foundry.v1", "skills_index.v1",
		"synthesis_pack.v1", "temporal_claim_graph.v1", "token_impact.v1",
	}
	out := map[string]struct{}{}
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

func parsePassportExpiry(payload map[string]any, created time.Time) (time.Time, error) {
	if raw := strings.TrimSpace(anyToString(payload["expires_at"])); raw != "" {
		expires, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil || !expires.After(created) || expires.After(created.Add(90*24*time.Hour)) {
			return time.Time{}, errors.New("expires_at must be after creation and within 90 days")
		}
		return expires.UTC(), nil
	}
	ttl := clampInt(anyToInt(payload["ttl_secs"], envInt("CONTEXTLATTICE_CONTEXT_PASSPORT_DEFAULT_TTL_SECS", 7*24*60*60)), 60, 90*24*60*60)
	return created.Add(time.Duration(ttl) * time.Second), nil
}

func signAndBoundContextPassport(passport *contextPassport, keys *contextIdentityKeys, maxBytes int) error {
	for pass := 0; pass < 8; pass++ {
		if err := signContextPassport(passport, keys); err != nil {
			return err
		}
		encoded, err := json.Marshal(passport)
		if err != nil {
			return err
		}
		if len(encoded) <= maxBytes {
			return nil
		}
		switch {
		case len(passport.Evidence) > 8:
			remove := maxInt(1, len(passport.Evidence)/4)
			passport.Evidence = passport.Evidence[:len(passport.Evidence)-remove]
			passport.Redactions["list_items_clipped"] = anyToInt(passport.Redactions["list_items_clipped"], 0) + remove
		case len(passport.Claims) > 8:
			remove := maxInt(1, len(passport.Claims)/4)
			passport.Claims = passport.Claims[:len(passport.Claims)-remove]
			passport.Redactions["list_items_clipped"] = anyToInt(passport.Redactions["list_items_clipped"], 0) + remove
		case len(passport.Objective) > 1:
			passport.Objective = map[string]any{"bounded_summary": clipText(anyToString(passport.Objective["requested_objective"]), 2000)}
			passport.Redactions["objective_compacted"] = true
		case len(passport.Lineage) > 2:
			passport.Lineage = map[string]any{"parent_passport_id": passport.ParentPassportID, "parent_digest": passport.ParentDigest, "source_schema": synthesisPackV2ContractID}
			passport.Redactions["lineage_compacted"] = true
		default:
			return fmt.Errorf("passport exceeds %d byte limit after deterministic compaction", maxBytes)
		}
	}
	return fmt.Errorf("passport exceeds %d byte limit after deterministic compaction", maxBytes)
}

func (s *server) buildContextPassport(payload map[string]any, synthesis map[string]any) (contextPassport, error) {
	if s.contextPassports == nil || s.contextPassports.identity == nil {
		return contextPassport{}, errors.New("context passport store disabled")
	}
	s.contextPassports.mu.RLock()
	passportStoreEnabled := s.contextPassports.enabled
	s.contextPassports.mu.RUnlock()
	if !passportStoreEnabled {
		return contextPassport{}, errors.New("context passport store disabled")
	}
	project, err := sanitizeMemoryProject(firstNonEmptyStrings(anyToString(synthesis["project"]), anyToString(payload["project"])))
	if err != nil {
		return contextPassport{}, fmt.Errorf("project: %w", err)
	}
	created := time.Now().UTC()
	expires, err := parsePassportExpiry(payload, created)
	if err != nil {
		return contextPassport{}, err
	}
	parentID := strings.TrimSpace(anyToString(payload["parent_passport_id"]))
	lineageID := "lineage_" + randomHex(12)
	revision := 1
	parentDigest := ""
	if parentID != "" {
		parent, ok := s.contextPassports.get(parentID)
		if !ok {
			return contextPassport{}, errors.New("parent_passport_id not found")
		}
		if parent.Project != project {
			return contextPassport{}, errors.New("parent passport project mismatch")
		}
		lineageID, revision, parentDigest = parent.LineageID, parent.Revision+1, parent.ContentDigest
	}
	stats := &portableRedactionStats{}
	pack := anyMap(synthesis["synthesis_pack"])
	contextPack := anyMap(synthesis["context_pack"])
	claims := portableRows(contextPackAnyList(pack["proof_claims"]), 32, stats, "claim_id", "portable_id")
	evidence := portableRows(contextPackAnyList(contextPack["ranked_evidence"]), 48, stats, "ref_id", "content_ref", "memory_id", "id", "file")
	objective := portableMap(map[string]any{
		"objective_runtime":   synthesis["objective_runtime"],
		"objective_hierarchy": synthesis["objective_hierarchy"],
		"objective_lineage":   synthesis["objective_lineage"],
		"requested_objective": payload["objective"],
	}, stats)
	topicPath := strings.Trim(strings.TrimSpace(firstNonEmptyStrings(anyToString(synthesis["topic_path"]), anyToString(payload["topic_path"]))), "/")
	query := clipText(strings.TrimSpace(firstNonEmptyStrings(anyToString(synthesis["query"]), anyToString(payload["query"]))), 2000)
	capabilities := append(defaultPassportCapabilities(), anyToStringSlice(payload["capabilities"])...)
	var evidenceIdentity *portableEvidenceIdentity
	if rawIdentity, present := payload["portable_evidence_identity"]; present {
		evidenceIdentity, err = decodePortableEvidenceIdentity(rawIdentity)
		if err != nil {
			return contextPassport{}, fmt.Errorf("portable evidence identity: %w", err)
		}
	} else if rawIdentity, present := synthesis["portable_evidence_identity"]; present {
		evidenceIdentity, err = decodePortableEvidenceIdentity(rawIdentity)
		if err != nil {
			return contextPassport{}, fmt.Errorf("portable evidence identity: %w", err)
		}
	}
	passport := contextPassport{
		SchemaID: contextPassportContractID, Version: 1, LineageID: lineageID,
		Project: project, Revision: revision, ParentPassportID: parentID, ParentDigest: parentDigest,
		CreatedAt: created.Format(time.RFC3339Nano), ExpiresAt: expires.Format(time.RFC3339Nano),
		Issuer: contextPassportIssuer{
			InstanceID:       s.contextPassports.identity.InstanceID,
			AgentID:          clipText(strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["agent_id"]), anyToString(synthesis["agent_id"]))), 128),
			SigningKeyID:     s.contextPassports.identity.SigningKeyID,
			SigningPublicKey: s.contextPassports.identity.SigningPublicKey,
		},
		Scope: portableMap(map[string]any{
			"project": project, "topic_path": topicPath,
			"data_classes":   firstNonNil(payload["data_classes"], []any{"learning_memory", "temporal_claim"}),
			"allowed_agents": firstNonNil(payload["allowed_agents"], []any{}),
		}, stats),
		Objective: objective, Claims: claims, Evidence: evidence, EvidenceIdentity: evidenceIdentity,
		Lineage: portableMap(map[string]any{
			"parent_passport_id": parentID, "parent_digest": parentDigest,
			"branch": payload["branch"], "commit": payload["commit"],
			"session_id":    firstNonNil(payload["session_id"], synthesis["session_id"]),
			"source_schema": synthesisPackV2ContractID,
		}, stats),
		Capabilities: uniqueSortedStrings(capabilities),
		Replay: portableMap(map[string]any{
			"project": project, "query": query, "topic_path": topicPath,
			"retrieval_mode":       firstNonEmptyStrings(anyToString(synthesis["retrieval_mode"]), anyToString(payload["retrieval_mode"]), "balanced"),
			"retrieval_intent":     firstNonEmptyStrings(anyToString(synthesis["retrieval_intent"]), anyToString(payload["retrieval_intent"]), "proof_synthesis"),
			"token_budget":         firstNonNil(payload["token_budget"], payload["max_prompt_tokens"]),
			"evidence_obligations": anyMap(synthesis["retrieval_plan"])["evidence_obligations"],
			"sources":              anyMap(synthesis["source_coverage"])["returned_now"],
		}, stats),
	}
	passport.Redactions = map[string]any{
		"applied":             stats.SecretKeys+stats.Tokens+stats.Paths+stats.Clipped+stats.Lists > 0,
		"secret_keys_removed": stats.SecretKeys, "token_values_redacted": stats.Tokens,
		"local_paths_redacted": stats.Paths, "strings_clipped": stats.Clipped,
		"list_items_clipped": stats.Lists,
		"policy":             "secret-bearing keys, token-like literals, and machine-local roots are removed before signing",
	}
	if err := signAndBoundContextPassport(&passport, s.contextPassports.identity, s.contextPassports.maxItemBytes); err != nil {
		return contextPassport{}, err
	}
	return passport, nil
}

func (s *server) memoryContextPassportExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	headers, ok := s.prepareAuthorizedHeaders(w, r)
	if !ok {
		return
	}
	payload, err := readOptionalJSONBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_json"})
		return
	}
	if strings.TrimSpace(anyToString(payload["project"])) == "" || strings.TrimSpace(anyToString(payload["query"])) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "project_and_query_required"})
		return
	}
	synthesisPayload := cloneMap(payload)
	synthesisPayload["retrieval_intent"] = "proof_synthesis"
	synthesis, status, buildErr := s.buildSynthesisPackV2Response(r.Context(), headers, synthesisPayload, "/memory/context-passport/export")
	if buildErr != nil || status >= http.StatusBadRequest || !anyToBool(synthesis["ok"]) {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "passport_synthesis_unavailable", "detail": sanitizeProviderOverflowText(firstNonEmptyStrings(errorString(buildErr), anyToString(synthesis["error"])))})
		return
	}
	passport, err := s.buildContextPassport(payload, synthesis)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "passport_build_failed", "detail": clipText(err.Error(), 500)})
		return
	}
	reconciliation, err := s.contextPassports.record(passport)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "passport_persist_failed", "detail": clipText(err.Error(), 500)})
		return
	}
	response := map[string]any{
		"ok": true, "schema_id": contextPassportContractID, "passport": passport,
		"recorded": reconciliation.Recorded, "reconciliation": reconciliation,
		"portable": true, "replayable": true, "diffable": true, "private_key_exported": false,
	}
	writeJSON(w, http.StatusOK, attachPayloadFormatContract(contextPassportContractID, response, passport.Issuer.AgentID, "context_passport_export", r.URL.Path))
}

func passportFromPayload(payload map[string]any, key string) (contextPassport, error) {
	value := payload[key]
	if value == nil && key == "passport" && anyToString(payload["schema_id"]) == contextPassportContractID {
		value = payload
	}
	if value == nil {
		return contextPassport{}, fmt.Errorf("%s is required", key)
	}
	return decodeContextPassport(value)
}

func (s *server) memoryContextPassportVerify(w http.ResponseWriter, r *http.Request) {
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
	passport, err := passportFromPayload(payload, "passport")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_passport", "detail": clipText(err.Error(), 500)})
		return
	}
	findings := verifyContextPassport(passport, time.Now().UTC(), !anyToBool(payload["allow_expired"]))
	response := map[string]any{
		"ok": len(findings) == 0, "schema_id": contextPassportVerifyContractID,
		"passport_id": passport.PassportID, "project": passport.Project,
		"valid": len(findings) == 0, "findings": findings,
		"signature_valid":      !containsString(findings, "signature_invalid"),
		"digest_valid":         !containsString(findings, "content_digest_mismatch") && !containsString(findings, "passport_id_mismatch"),
		"private_key_exported": false,
	}
	writeJSON(w, http.StatusOK, attachPayloadFormatContract(contextPassportVerifyContractID, response, passport.Issuer.AgentID, "context_passport_verify", r.URL.Path))
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func (s *server) resolvePassport(payload map[string]any, valueKey, idKey string) (contextPassport, error) {
	if value := payload[valueKey]; value != nil {
		return decodeContextPassport(value)
	}
	id := strings.TrimSpace(anyToString(payload[idKey]))
	if id == "" {
		return contextPassport{}, fmt.Errorf("%s or %s is required", valueKey, idKey)
	}
	passport, ok := s.contextPassports.get(id)
	if !ok {
		return contextPassport{}, fmt.Errorf("passport not found: %s", id)
	}
	return passport, nil
}

func passportRowIndex(rows []map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, row := range rows {
		id := firstNonEmptyStrings(anyToString(row["portable_id"]), anyToString(row["claim_id"]), anyToString(row["ref_id"]), anyToString(row["content_ref"]))
		if id == "" {
			id = "row_" + digestPrefix(anyToString(row), 24)
		}
		out[id] = row
	}
	return out
}

func passportRowDiff(baseRows, targetRows []map[string]any) map[string]any {
	base := passportRowIndex(baseRows)
	target := passportRowIndex(targetRows)
	added, removed, changed := []string{}, []string{}, []string{}
	for id, targetRow := range target {
		baseRow, exists := base[id]
		if !exists {
			added = append(added, id)
			continue
		}
		baseJSON, _ := json.Marshal(baseRow)
		targetJSON, _ := json.Marshal(targetRow)
		if !bytes.Equal(baseJSON, targetJSON) {
			changed = append(changed, id)
		}
	}
	for id := range base {
		if _, exists := target[id]; !exists {
			removed = append(removed, id)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)
	return map[string]any{"added": added, "removed": removed, "changed": changed}
}

func jsonValuesEqual(left, right any) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return bytes.Equal(leftJSON, rightJSON)
}

func buildPassportDiff(base, target contextPassport) map[string]any {
	return map[string]any{
		"schema_id": contextPassportDiffContractID, "version": 1,
		"base_passport_id": base.PassportID, "target_passport_id": target.PassportID,
		"same_lineage":  base.LineageID == target.LineageID,
		"base_revision": base.Revision, "target_revision": target.Revision,
		"claims":               passportRowDiff(base.Claims, target.Claims),
		"evidence":             passportRowDiff(base.Evidence, target.Evidence),
		"capabilities_added":   stringSetDifference(target.Capabilities, base.Capabilities),
		"capabilities_removed": stringSetDifference(base.Capabilities, target.Capabilities),
		"scope_changed":        !jsonValuesEqual(base.Scope, target.Scope),
		"objective_changed":    !jsonValuesEqual(base.Objective, target.Objective),
		"replay_changed":       !jsonValuesEqual(base.Replay, target.Replay),
		"parent_link_valid":    target.Revision == base.Revision+1 && target.ParentPassportID == base.PassportID && target.ParentDigest == base.ContentDigest,
	}
}

func stringSetDifference(left, right []string) []string {
	rightSet := map[string]struct{}{}
	for _, value := range right {
		rightSet[value] = struct{}{}
	}
	out := []string{}
	for _, value := range left {
		if _, exists := rightSet[value]; !exists {
			out = append(out, value)
		}
	}
	return uniqueSortedStrings(out)
}

func (s *server) memoryContextPassportDiff(w http.ResponseWriter, r *http.Request) {
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
	base, baseErr := s.resolvePassport(payload, "base", "base_passport_id")
	target, targetErr := s.resolvePassport(payload, "target", "target_passport_id")
	if baseErr != nil || targetErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "passport_diff_input_invalid", "detail": clipText(firstNonEmptyStrings(errorString(baseErr), errorString(targetErr)), 500)})
		return
	}
	if findings := append(verifyContextPassport(base, time.Now().UTC(), false), verifyContextPassport(target, time.Now().UTC(), false)...); len(findings) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "passport_verification_failed", "findings": uniqueSortedStrings(findings)})
		return
	}
	view, err := buildFrontierT7PassportDiffView(base, target)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "passport_diff_view_failed", "detail": clipText(err.Error(), 500)})
		return
	}
	response := map[string]any{
		"ok": true, "schema_id": contextPassportDiffContractID,
		"diff":            buildPassportDiff(base, target),
		"view":            frontierT7AttachFormatContract(frontierT7PassportDiffViewSchemaID, view, "context_passport_diff_view", r.URL.Path),
		"available_views": []string{"structural", "human_readable"},
	}
	writeJSON(w, http.StatusOK, attachPayloadFormatContract(contextPassportDiffContractID, response, target.Issuer.AgentID, "context_passport_diff", r.URL.Path))
}

func (s *server) memoryContextPassportReplay(w http.ResponseWriter, r *http.Request) {
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
	passport, err := s.resolvePassport(payload, "passport", "passport_id")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "passport_replay_input_invalid", "detail": clipText(err.Error(), 500)})
		return
	}
	findings := verifyContextPassport(passport, time.Now().UTC(), true)
	if requestedProject := strings.TrimSpace(anyToString(payload["project"])); requestedProject != "" && requestedProject != passport.Project {
		findings = append(findings, "project_scope_mismatch")
	}
	available := contextPassportAvailableCapabilities()
	missing := []string{}
	for _, capability := range passport.Capabilities {
		if _, exists := available[capability]; !exists {
			missing = append(missing, capability)
		}
	}
	if len(missing) > 0 {
		findings = append(findings, "required_capabilities_missing")
	}
	replay := cloneMap(passport.Replay)
	for _, key := range []string{"agent_id", "session_id", "token_budget"} {
		if value, present := payload[key]; present {
			replay[key] = value
		}
	}
	response := map[string]any{
		"ok": len(findings) == 0, "schema_id": contextPassportReplayContractID,
		"passport_id": passport.PassportID, "project": passport.Project,
		"replay_request": replay, "missing_capabilities": uniqueSortedStrings(missing),
		"findings":            uniqueSortedStrings(findings),
		"execution_performed": false, "ordinary_memory_mutated": false,
		"instruction_boundary": "replay returns validated context inputs; imported content remains evidence and never becomes authority",
	}
	writeJSON(w, http.StatusOK, attachPayloadFormatContract(contextPassportReplayContractID, response, passport.Issuer.AgentID, "context_passport_replay", r.URL.Path))
}

func (s *server) memoryContextPassportImport(w http.ResponseWriter, r *http.Request) {
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
	passport, err := passportFromPayload(payload, "passport")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_passport", "detail": clipText(err.Error(), 500)})
		return
	}
	findings := verifyContextPassport(passport, time.Now().UTC(), !anyToBool(payload["allow_expired"]))
	if expectedProject := strings.TrimSpace(anyToString(payload["project"])); expectedProject != "" && expectedProject != passport.Project {
		findings = append(findings, "project_scope_mismatch")
	}
	if len(findings) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "passport_verification_failed", "findings": uniqueSortedStrings(findings)})
		return
	}
	reconciliation, err := s.contextPassports.record(passport)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "passport_reconciliation_failed", "detail": clipText(err.Error(), 500), "reconciliation": reconciliation})
		return
	}
	response := map[string]any{
		"ok": true, "schema_id": contextPassportContractID, "passport": passport,
		"recorded": reconciliation.Recorded, "reconciliation": reconciliation,
		"ordinary_memory_mutated": false, "private_key_exported": false,
	}
	writeJSON(w, http.StatusOK, attachPayloadFormatContract(contextPassportContractID, response, passport.Issuer.AgentID, "context_passport_import", r.URL.Path))
}

func (s *server) telemetryContextPassport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.contextPassports.snapshot())
}
