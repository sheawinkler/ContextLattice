package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	agentWorkerIdentityStatePending      = "pending"
	agentWorkerIdentityStateDelivering   = "delivering"
	agentWorkerIdentityStateDelivered    = "delivered"
	agentWorkerIdentityStateAcknowledged = "acknowledged"
	// Pull readback is retry-safe until the exact acknowledgement arrives.
	// Delivery telemetry is bounded and saturated; a repeated read never
	// consumes the update or turns it into a terminal failure state.
	agentWorkerIdentityMaxAttempts                         = 255
	agentWorkerIdentityMaxIDBytes                          = 256
	agentWorkerIdentitySuffixBytes                         = 12
	agentWorkerIdentityCanonicalMaxBytes                   = 256
	workerIdentityAckReceiptPayloadVersionExact            = 1
	workerIdentityAckReceiptPayloadVersionLegacyReconciled = 2
	workerInstanceCredentialBytes                          = 32
	workerInstanceCredentialMaxBytes                       = 256
	workerInstanceCredentialGenerationInitial              = 1
	workerInstanceCredentialVerifierDomain                 = "contextlattice/agent-worker-instance-credential/v1"
	workerInstanceCredentialVerifierLegacyPrefix           = "sha256:"
	workerInstanceCredentialVerifierCurrentPrefix          = "sha256:v2:"
	workerInstanceCredentialHeader                         = "X-Worker-Instance-Credential"
	workerIdentityCredentialMigrationPhase                 = "worker_identity_credential_migration_v1"
	workerIdentityCredentialMigrationBatchSize             = 256
)

var (
	errWorkerIdentityNotRegistered             = errors.New("worker identity is not registered")
	errWorkerIdentityUpdatePending             = errors.New("worker identity update acknowledgement is required before task claim")
	errWorkerIdentityLegacyCredentialMigration = errors.New("legacy worker identity credential migration required; register a new worker instance")
)

type workerIdentityCredentialMigrationChallengeError struct {
	Challenge map[string]any
}

func (e *workerIdentityCredentialMigrationChallengeError) Error() string {
	return "legacy worker identity credential migration challenge required; rotate worker instance"
}

func workerIdentityCredentialMigrationChallenge(identity agentWorkerIdentityRecord) *workerIdentityCredentialMigrationChallengeError {
	digest := agentTaskDigest(map[string]any{
		"kind":        "worker_identity_credential_migration_challenge",
		"identity_id": identity.IdentityID, "principal_id": identity.PrincipalID,
		"workspace_id": identity.WorkspaceID, "requested_worker_id": identity.RequestedWorkerID,
		"worker_instance_id":                    identity.WorkerInstanceID,
		"worker_identity_update_generation":     identity.IdentityUpdateGeneration,
		"worker_instance_credential_generation": identity.WorkerInstanceCredentialGeneration,
		"identity_digest":                       identity.IdentityDigest,
	})
	return &workerIdentityCredentialMigrationChallengeError{Challenge: map[string]any{
		"schema_id": "worker_identity_credential_migration_challenge.v1", "contract_version": 1,
		"identity_id": identity.IdentityID, "principal_id": identity.PrincipalID, "workspace_id": identity.WorkspaceID,
		"requested_worker_id": identity.RequestedWorkerID, "worker_instance_id": identity.WorkerInstanceID,
		"worker_identity_update_generation":     identity.IdentityUpdateGeneration,
		"worker_instance_credential_generation": identity.WorkerInstanceCredentialGeneration,
		"identity_digest":                       identity.IdentityDigest, "challenge_digest": digest,
		"action": "rotate_worker_instance_and_register",
	}}
}

type agentWorkerIdentityAuthority struct {
	PrincipalID      string
	WorkspaceID      string
	WorkerInstanceID string
}

type agentWorkerIdentityRecord struct {
	IdentityID                         string
	PrincipalID                        string
	WorkspaceID                        string
	RequestedWorkerID                  string
	CanonicalWorkerID                  string
	WorkerInstanceID                   string
	WorkerInstanceCredentialVerifier   string
	WorkerInstanceCredentialGeneration int
	IdentityUpdateGeneration           int
	AcknowledgedGeneration             int
	RequestedIDDigest                  string
	IdentityDigest                     string
	Status                             string
	CreatedAt                          string
	UpdatedAt                          string
	ClosedAt                           string
}

func newWorkerInstanceCredential() (string, error) {
	// Only the trusted in-process compatibility path may generate this value;
	// external registration supplies its client-persisted credential instead.
	raw := make([]byte, workerInstanceCredentialBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate worker instance credential: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

func workerInstanceCredentialVerifierDigest(credential string, authority agentWorkerIdentityAuthority, credentialGeneration, identityGeneration int) string {
	material := strings.Join([]string{
		workerInstanceCredentialVerifierDomain,
		authority.PrincipalID,
		authority.WorkspaceID,
		authority.WorkerInstanceID,
		strconv.Itoa(credentialGeneration),
		strconv.Itoa(identityGeneration),
		credential,
	}, "\x00")
	digest := sha256.Sum256([]byte(material))
	return hex.EncodeToString(digest[:])
}

func workerInstanceCredentialVerifier(credential string, authority agentWorkerIdentityAuthority, credentialGeneration, identityGeneration int) string {
	return workerInstanceCredentialVerifierCurrentPrefix + workerInstanceCredentialVerifierDigest(credential, authority, credentialGeneration, identityGeneration)
}

// legacyWorkerInstanceCredentialVerifier is the pre-proof-boundary format.
// Its material included the exact principal/workspace/instance and credential
// generation, but not the identity-update generation. Existing rows may still
// contain this verifier and are upgraded only after the same credential proves
// possession in an atomic transaction.
func legacyWorkerInstanceCredentialVerifier(credential string, authority agentWorkerIdentityAuthority, credentialGeneration int) string {
	material := strings.Join([]string{
		workerInstanceCredentialVerifierDomain,
		authority.PrincipalID,
		authority.WorkspaceID,
		authority.WorkerInstanceID,
		strconv.Itoa(credentialGeneration),
		credential,
	}, "\x00")
	digest := sha256.Sum256([]byte(material))
	return workerInstanceCredentialVerifierLegacyPrefix + hex.EncodeToString(digest[:])
}

func workerInstanceCredentialVerifierUnversionedCurrent(credential string, authority agentWorkerIdentityAuthority, credentialGeneration, identityGeneration int) string {
	return workerInstanceCredentialVerifierLegacyPrefix + workerInstanceCredentialVerifierDigest(credential, authority, credentialGeneration, identityGeneration)
}

func validateWorkerInstanceCredential(credential string) error {
	if len([]byte(credential)) != workerInstanceCredentialBytes*2 {
		return errors.New("worker instance credential is malformed")
	}
	for _, ch := range credential {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			return errors.New("worker instance credential is malformed")
		}
	}
	return nil
}

type workerInstanceCredentialVerifierVersion uint8

const (
	workerInstanceCredentialVerifierInvalid workerInstanceCredentialVerifierVersion = iota
	workerInstanceCredentialVerifierCurrent
	workerInstanceCredentialVerifierLegacy
)

func constantTimeVerifierEqual(stored, expected string) bool {
	return len(stored) == len(expected) && subtle.ConstantTimeCompare([]byte(stored), []byte(expected)) == 1
}

func workerInstanceCredentialVerifierVersionFor(identity agentWorkerIdentityRecord, credential string) (workerInstanceCredentialVerifierVersion, error) {
	// Always derive a verifier from the supplied value before comparing it. The
	// stored verifier is fixed-width and every accepted comparison is constant-
	// time; no caller-controlled credential is ever persisted or included in a
	// receipt.
	if err := validateWorkerInstanceCredential(credential); err != nil {
		return workerInstanceCredentialVerifierInvalid, err
	}
	authority := agentWorkerIdentityAuthority{
		PrincipalID: identity.PrincipalID, WorkspaceID: identity.WorkspaceID, WorkerInstanceID: identity.WorkerInstanceID,
	}
	expectedCurrent := workerInstanceCredentialVerifier(credential, authority, identity.WorkerInstanceCredentialGeneration, identity.IdentityUpdateGeneration)
	expectedUnversionedCurrent := workerInstanceCredentialVerifierUnversionedCurrent(credential, authority, identity.WorkerInstanceCredentialGeneration, identity.IdentityUpdateGeneration)
	expectedLegacy := legacyWorkerInstanceCredentialVerifier(credential, authority, identity.WorkerInstanceCredentialGeneration)
	stored := identity.WorkerInstanceCredentialVerifier
	current := constantTimeVerifierEqual(stored, expectedCurrent)
	// Rows created by the first repair candidate used the current material with
	// the old unversioned prefix. Treat them as an explicit upgrade alias, not
	// as a new accepted long-term format.
	unversionedCurrent := constantTimeVerifierEqual(stored, expectedUnversionedCurrent)
	legacy := constantTimeVerifierEqual(stored, expectedLegacy)
	if current {
		return workerInstanceCredentialVerifierCurrent, nil
	}
	if unversionedCurrent || legacy {
		return workerInstanceCredentialVerifierLegacy, nil
	}
	if stored == "" {
		return workerInstanceCredentialVerifierInvalid, errors.New("worker instance credential is required; re-register a new instance")
	}
	return workerInstanceCredentialVerifierInvalid, errors.New("worker instance credential rejected; rotate worker instance")
}

func verifyWorkerInstanceCredential(identity agentWorkerIdentityRecord, credential string) error {
	if _, err := workerInstanceCredentialVerifierVersionFor(identity, credential); err != nil {
		return err
	}
	return nil
}

func (l *agentTaskDeliveryLedger) verifyAndUpgradeWorkerInstanceCredentialTx(ctx context.Context, tx *sql.Tx, identity *agentWorkerIdentityRecord, credential string) error {
	if identity == nil {
		return errors.New("worker identity credential authority is unavailable")
	}
	version, err := workerInstanceCredentialVerifierVersionFor(*identity, credential)
	if err != nil {
		// A pre-repair nonempty verifier may belong to a server-issued secret
		// whose first response was lost. Do not trust a new bearer or disclose
		// verifier material; return a bounded, exact identity challenge so the
		// client can rotate safely. Malformed input remains a plain rejection.
		storedVerifier := strings.TrimSpace(identity.WorkerInstanceCredentialVerifier)
		if validateWorkerInstanceCredential(credential) == nil && storedVerifier != "" && strings.HasPrefix(storedVerifier, workerInstanceCredentialVerifierLegacyPrefix) && !strings.HasPrefix(storedVerifier, workerInstanceCredentialVerifierCurrentPrefix) {
			return workerIdentityCredentialMigrationChallenge(*identity)
		}
		return err
	}
	if identity.Status != "active" {
		return errors.New("worker identity is closed")
	}
	if version == workerInstanceCredentialVerifierCurrent {
		return nil
	}
	current := workerInstanceCredentialVerifier(credential, agentWorkerIdentityAuthority{
		PrincipalID: identity.PrincipalID, WorkspaceID: identity.WorkspaceID, WorkerInstanceID: identity.WorkerInstanceID,
	}, identity.WorkerInstanceCredentialGeneration, identity.IdentityUpdateGeneration)
	updated, err := tx.ExecContext(ctx, `UPDATE task_ledger_worker_identities SET worker_instance_credential_verifier=?,updated_at=? WHERE identity_id=? AND status='active' AND worker_instance_credential_verifier=?`, current, agentTaskNow(), identity.IdentityID, identity.WorkerInstanceCredentialVerifier)
	if err != nil {
		return err
	}
	affected, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return errors.New("worker instance credential upgrade lost its identity CAS")
	}
	identity.WorkerInstanceCredentialVerifier = current
	return nil
}

func (l *agentTaskDeliveryLedger) verifyAndUpgradeWorkerInstanceCredential(ctx context.Context, identity agentWorkerIdentityRecord, credential string) (agentWorkerIdentityRecord, error) {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return agentWorkerIdentityRecord{}, err
	}
	defer tx.Rollback()
	current, err := scanWorkerIdentity(tx.QueryRowContext(ctx, `SELECT `+workerIdentitySelectColumns+` FROM task_ledger_worker_identities WHERE identity_id=?`, identity.IdentityID))
	if err != nil {
		return agentWorkerIdentityRecord{}, err
	}
	if err := l.verifyAndUpgradeWorkerInstanceCredentialTx(ctx, tx, &current, credential); err != nil {
		return agentWorkerIdentityRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return agentWorkerIdentityRecord{}, err
	}
	return current, nil
}

type agentWorkerIdentityUpdateRecord struct {
	UpdateID                 string
	IdentityID               string
	PrincipalID              string
	WorkspaceID              string
	WorkerInstanceID         string
	OldWorkerID              string
	RequestedWorkerID        string
	NewWorkerID              string
	CanonicalWorkerID        string
	IdentityUpdateGeneration int
	UpdateDigest             string
	ReceiptDigest            string
	State                    string
	DeliveryAttempts         int
	LastError                string
	CreatedAt                string
	UpdatedAt                string
	DeliveredAt              string
	AcknowledgedAt           string
	AckReceiptDigest         string
	AckReceiptPayloadJSON    string
	AckReceiptPayloadVersion int
	ExpiresAt                string
}

func normalizeWorkerIdentityText(value, field string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	if err := agentTaskValidateText(value, field, agentWorkerIdentityMaxIDBytes); err != nil {
		return "", err
	}
	return value, nil
}

func normalizeWorkerIdentityID(value, field string) (string, error) {
	value, err := normalizeWorkerIdentityText(value, field)
	if err != nil {
		return "", err
	}
	value = strings.ToLower(value)
	if !workerIdentityPublicLeaseID(value) {
		return "", fmt.Errorf("%s is not a valid public lease identifier", field)
	}
	return value, nil
}

// workerIdentityPublicLeaseID is the shared Go/Python public lease-ID
// grammar. IDs cross the Gateway/worker boundary and are also used in the
// execution workspace and lease fence, so accepting arbitrary UTF-8 here
// would create a durable identity that the Python fence parser cannot carry.
// Keep this byte-for-byte aligned with scripts/task_agent_execution.py.
func workerIdentityPublicLeaseID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len([]byte(value)) > agentWorkerIdentityCanonicalMaxBytes {
		return false
	}
	for index := 0; index < len(value); index++ {
		ch := value[index]
		if index == 0 {
			if !((ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')) {
				return false
			}
			continue
		}
		if !((ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '.' || ch == '_' || ch == ':' || ch == '@' || ch == '-') {
			return false
		}
	}
	return true
}

func normalizeWorkerIdentityAuthority(principal, workspace, instance string) (agentWorkerIdentityAuthority, error) {
	principal, err := normalizeWorkerIdentityText(principal, "authenticated principal")
	if err != nil {
		return agentWorkerIdentityAuthority{}, err
	}
	workspace, err = normalizeWorkerIdentityText(workspace, "authenticated workspace")
	if err != nil {
		return agentWorkerIdentityAuthority{}, err
	}
	// Workspace governance is case-insensitive. Store and compare its canonical
	// form before any lookup, digest, or uniqueness decision so case variants
	// cannot create a second logical workspace namespace.
	workspace = strings.ToLower(workspace)
	instance, err = normalizeWorkerIdentityText(instance, "worker_instance_id")
	if err != nil {
		return agentWorkerIdentityAuthority{}, err
	}
	if !workerIdentityPublicLeaseID(instance) {
		return agentWorkerIdentityAuthority{}, errors.New("worker_instance_id is not a valid public lease identifier")
	}
	return agentWorkerIdentityAuthority{PrincipalID: principal, WorkspaceID: workspace, WorkerInstanceID: instance}, nil
}

func workerIdentityRequestedDigest(requested string) string {
	return agentTaskDigest(map[string]any{"requested_worker_id": strings.TrimSpace(requested)})
}

func workerIdentityRecordDigest(record agentWorkerIdentityRecord) string {
	return agentTaskDigest(map[string]any{
		"identity_id": record.IdentityID, "principal_id": record.PrincipalID, "workspace_id": record.WorkspaceID,
		"requested_worker_id": record.RequestedWorkerID, "canonical_worker_id": record.CanonicalWorkerID,
		"worker_instance_id": record.WorkerInstanceID, "worker_identity_update_generation": record.IdentityUpdateGeneration,
	})
}

func workerIdentityUpdateDigest(record agentWorkerIdentityUpdateRecord) string {
	return agentTaskDigest(map[string]any{
		"update_id": record.UpdateID, "identity_id": record.IdentityID, "principal_id": record.PrincipalID,
		"workspace_id": record.WorkspaceID, "worker_instance_id": record.WorkerInstanceID,
		"old_worker_id": record.OldWorkerID, "requested_worker_id": record.RequestedWorkerID,
		"new_worker_id": record.NewWorkerID, "canonical_worker_id": record.CanonicalWorkerID,
		"worker_identity_update_generation": record.IdentityUpdateGeneration,
	})
}

func workerIdentityReceiptDigest(record agentWorkerIdentityUpdateRecord) string {
	return agentTaskDigest(map[string]any{
		"update_id": record.UpdateID, "identity_id": record.IdentityID, "principal_id": record.PrincipalID,
		"workspace_id": record.WorkspaceID, "worker_instance_id": record.WorkerInstanceID,
		"old_worker_id": record.OldWorkerID, "requested_worker_id": record.RequestedWorkerID,
		"new_worker_id": record.NewWorkerID, "canonical_worker_id": record.CanonicalWorkerID,
		"worker_identity_update_generation": record.IdentityUpdateGeneration,
		"update_digest":                     record.UpdateDigest,
	})
}

func workerIdentityAckReceiptDigest(record agentWorkerIdentityUpdateRecord, authority agentWorkerIdentityAuthority) string {
	return agentTaskDigest(map[string]any{
		"identity_update_receipt": record.ReceiptDigest, "update_digest": record.UpdateDigest,
		"update_id": record.UpdateID, "identity_id": record.IdentityID,
		"principal_id": authority.PrincipalID, "workspace_id": authority.WorkspaceID,
		"worker_instance_id": authority.WorkerInstanceID,
		"old_worker_id":      record.OldWorkerID, "requested_worker_id": record.RequestedWorkerID,
		"canonical_worker_id": record.CanonicalWorkerID, "new_worker_id": record.NewWorkerID,
		"worker_identity_update_generation": record.IdentityUpdateGeneration, "acknowledged": true,
	})
}

func boundedCanonicalWorkerID(requested, instance string, retry int) string {
	hash := sha256.Sum256([]byte(strings.TrimSpace(instance)))
	// The separator is intentionally in the shared public lease-ID grammar;
	// Python's execution fence must be able to carry this server-issued ID.
	suffix := "-" + hex.EncodeToString(hash[:agentWorkerIdentitySuffixBytes/2])
	if retry > 0 {
		suffix += fmt.Sprintf("-%x", retry)
	}
	requested = strings.TrimSpace(strings.ToLower(requested))
	maxPrefix := agentWorkerIdentityCanonicalMaxBytes - len([]byte(suffix))
	if maxPrefix < 1 {
		maxPrefix = 1
	}
	if len([]byte(requested)) > maxPrefix {
		prefix := []rune(requested)
		for len([]byte(string(prefix))) > maxPrefix {
			prefix = prefix[:len(prefix)-1]
		}
		requested = string(prefix)
	}
	return requested + suffix
}

func (r agentWorkerIdentityRecord) payload() map[string]any {
	payload := map[string]any{
		"schema_id": agentWorkerIdentityReadbackContractID, "contract_version": 1,
		"identity_id": r.IdentityID, "principal_id": r.PrincipalID, "workspace_id": r.WorkspaceID,
		"requested_worker_id": r.RequestedWorkerID, "canonical_worker_id": r.CanonicalWorkerID,
		"worker_instance_id":                r.WorkerInstanceID,
		"worker_identity_update_generation": r.IdentityUpdateGeneration,
		"acknowledged_generation":           r.AcknowledgedGeneration,
		"requested_id_digest":               r.RequestedIDDigest, "identity_digest": r.IdentityDigest,
		"status": r.Status, "created_at": r.CreatedAt, "updated_at": r.UpdatedAt, "closed_at": r.ClosedAt,
	}
	return agentTaskContractPayload(agentWorkerIdentityReadbackContractID, payload)
}

func (r agentWorkerIdentityUpdateRecord) payload() map[string]any {
	ackReceiptDigest := r.AckReceiptDigest
	if ackReceiptDigest == "" {
		ackReceiptDigest = workerIdentityAckReceiptDigest(r, agentWorkerIdentityAuthority{PrincipalID: r.PrincipalID, WorkspaceID: r.WorkspaceID, WorkerInstanceID: r.WorkerInstanceID})
	}
	payload := map[string]any{
		"schema_id": agentWorkerIdentityUpdateContractID, "contract_version": 1,
		"update_id": r.UpdateID, "identity_id": r.IdentityID, "principal_id": r.PrincipalID,
		"workspace_id": r.WorkspaceID, "worker_instance_id": r.WorkerInstanceID,
		"old_worker_id": r.OldWorkerID, "requested_worker_id": r.RequestedWorkerID,
		"new_worker_id": r.NewWorkerID, "canonical_worker_id": r.CanonicalWorkerID,
		"worker_identity_update_generation": r.IdentityUpdateGeneration,
		"update_digest":                     r.UpdateDigest, "receipt_digest": r.ReceiptDigest, "state": r.State,
		"delivery_attempts": r.DeliveryAttempts, "last_error": r.LastError,
		"created_at": r.CreatedAt, "updated_at": r.UpdatedAt, "delivered_at": r.DeliveredAt,
		"acknowledged_at": r.AcknowledgedAt, "ack_receipt_digest": ackReceiptDigest, "expires_at": r.ExpiresAt,
		"ack_required": r.State != agentWorkerIdentityStateAcknowledged,
	}
	return agentTaskContractPayload(agentWorkerIdentityUpdateContractID, payload)
}

func scanWorkerIdentity(scanner agentTaskSQLScanner) (agentWorkerIdentityRecord, error) {
	var r agentWorkerIdentityRecord
	err := scanner.Scan(&r.IdentityID, &r.PrincipalID, &r.WorkspaceID, &r.RequestedWorkerID, &r.CanonicalWorkerID, &r.WorkerInstanceID, &r.WorkerInstanceCredentialVerifier, &r.WorkerInstanceCredentialGeneration, &r.IdentityUpdateGeneration, &r.AcknowledgedGeneration, &r.RequestedIDDigest, &r.IdentityDigest, &r.Status, &r.CreatedAt, &r.UpdatedAt, &r.ClosedAt)
	return r, err
}

func scanWorkerIdentityUpdate(scanner agentTaskSQLScanner) (agentWorkerIdentityUpdateRecord, error) {
	var r agentWorkerIdentityUpdateRecord
	err := scanner.Scan(&r.UpdateID, &r.IdentityID, &r.PrincipalID, &r.WorkspaceID, &r.WorkerInstanceID, &r.OldWorkerID, &r.RequestedWorkerID, &r.NewWorkerID, &r.CanonicalWorkerID, &r.IdentityUpdateGeneration, &r.UpdateDigest, &r.ReceiptDigest, &r.State, &r.DeliveryAttempts, &r.LastError, &r.CreatedAt, &r.UpdatedAt, &r.DeliveredAt, &r.AcknowledgedAt, &r.AckReceiptDigest, &r.AckReceiptPayloadJSON, &r.AckReceiptPayloadVersion, &r.ExpiresAt)
	return r, err
}

const workerIdentitySelectColumns = `identity_id,principal_id,workspace_id,requested_worker_id,canonical_worker_id,worker_instance_id,worker_instance_credential_verifier,worker_instance_credential_generation,worker_identity_update_generation,acknowledged_generation,requested_id_digest,identity_digest,status,created_at,updated_at,closed_at`
const workerIdentityUpdateSelectColumns = `update_id,identity_id,principal_id,workspace_id,worker_instance_id,old_worker_id,requested_worker_id,new_worker_id,canonical_worker_id,worker_identity_update_generation,update_digest,receipt_digest,state,delivery_attempts,last_error,created_at,updated_at,delivered_at,acknowledged_at,ack_receipt_digest,ack_receipt_payload_json,ack_receipt_payload_version,expires_at`

// migrateLegacyWorkerIdentityAckReceiptsTx closes the pre-v6 replay gap. Old
// acknowledged rows had no durable receipt snapshot. They are backfilled only
// when every immutable digest, authority binding, generation, and terminal
// state proves the row is internally consistent. Any ambiguity aborts schema
// initialization and leaves the row untouched.
func (l *agentTaskDeliveryLedger) migrateLegacyWorkerIdentityAckReceiptsTx(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT `+workerIdentityUpdateSelectColumns+` FROM task_ledger_worker_identity_updates WHERE state=? AND (ack_receipt_payload_json='' OR ack_receipt_payload_version=0)`, agentWorkerIdentityStateAcknowledged)
	if err != nil {
		return err
	}
	updates := make([]agentWorkerIdentityUpdateRecord, 0)
	for rows.Next() {
		update, scanErr := scanWorkerIdentityUpdate(rows)
		if scanErr != nil {
			_ = rows.Close()
			return scanErr
		}
		updates = append(updates, update)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, update := range updates {
		if update.State != agentWorkerIdentityStateAcknowledged || update.AckReceiptPayloadVersion != 0 || update.AcknowledgedAt == "" || update.DeliveredAt == "" || update.IdentityUpdateGeneration <= 0 {
			return fmt.Errorf("legacy worker identity acknowledgement %s is ambiguous", update.UpdateID)
		}
		identity, identityErr := scanWorkerIdentity(tx.QueryRowContext(ctx, `SELECT `+workerIdentitySelectColumns+` FROM task_ledger_worker_identities WHERE identity_id=?`, update.IdentityID))
		if identityErr != nil {
			return fmt.Errorf("legacy worker identity acknowledgement %s has no identity: %w", update.UpdateID, identityErr)
		}
		if identity.IdentityUpdateGeneration != update.IdentityUpdateGeneration || identity.AcknowledgedGeneration != update.IdentityUpdateGeneration || identity.Status != "active" || identity.PrincipalID != update.PrincipalID || identity.WorkspaceID != update.WorkspaceID || identity.WorkerInstanceID != update.WorkerInstanceID {
			return fmt.Errorf("legacy worker identity acknowledgement %s has an inconsistent authority or generation", update.UpdateID)
		}
		if update.UpdateDigest != workerIdentityUpdateDigest(update) || update.ReceiptDigest != workerIdentityReceiptDigest(update) {
			return fmt.Errorf("legacy worker identity acknowledgement %s has invalid update evidence", update.UpdateID)
		}
		authority, authorityErr := normalizeWorkerIdentityAuthority(update.PrincipalID, update.WorkspaceID, update.WorkerInstanceID)
		if authorityErr != nil || update.AckReceiptDigest == "" || update.AckReceiptDigest != workerIdentityAckReceiptDigest(update, authority) {
			return fmt.Errorf("legacy worker identity acknowledgement %s has invalid acknowledgement evidence", update.UpdateID)
		}
		// Before v6 the receipt version was not persisted. Reconstruct the
		// immutable delivered receipt from the durable pre-ack fields. If a
		// snapshot already exists, it must match that exact reconstruction; an
		// acknowledged/rewritten or otherwise ambiguous snapshot is rejected.
		legacyReceipt := update
		legacyReceipt.State = agentWorkerIdentityStateDelivered
		legacyReceipt.AcknowledgedAt = ""
		legacyReceipt.UpdatedAt = legacyReceipt.DeliveredAt
		legacyReceipt.AckReceiptPayloadJSON = ""
		legacyReceipt.AckReceiptPayloadVersion = 0
		snapshot := strings.TrimSpace(update.AckReceiptPayloadJSON)
		version := workerIdentityAckReceiptPayloadVersionExact
		if snapshot != "" {
			if !workerIdentityReceiptSnapshotMatches(legacyReceipt.payload(), snapshot) {
				return fmt.Errorf("legacy worker identity acknowledgement %s has an ambiguous receipt snapshot", update.UpdateID)
			}
		} else {
			snapshot = encodeAgentTaskJSON(legacyReceipt.payload())
			version = workerIdentityAckReceiptPayloadVersionLegacyReconciled
		}
		if snapshot == "{}" {
			return fmt.Errorf("legacy worker identity acknowledgement %s cannot be serialized", update.UpdateID)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_worker_identity_updates SET ack_receipt_payload_json=?,ack_receipt_payload_version=? WHERE update_id=? AND state=? AND ack_receipt_payload_version=0`, snapshot, version, update.UpdateID, agentWorkerIdentityStateAcknowledged); err != nil {
			return err
		}
	}
	return nil
}

func (l *agentTaskDeliveryLedger) workerIdentityByID(ctx context.Context, identityID string) (agentWorkerIdentityRecord, error) {
	return scanWorkerIdentity(l.db.QueryRowContext(ctx, `SELECT `+workerIdentitySelectColumns+` FROM task_ledger_worker_identities WHERE identity_id=?`, strings.TrimSpace(identityID)))
}

func (l *agentTaskDeliveryLedger) workerIdentityByAuthority(ctx context.Context, authority agentWorkerIdentityAuthority) (agentWorkerIdentityRecord, error) {
	normalized, err := normalizeWorkerIdentityAuthority(authority.PrincipalID, authority.WorkspaceID, authority.WorkerInstanceID)
	if err != nil {
		return agentWorkerIdentityRecord{}, err
	}
	return scanWorkerIdentity(l.db.QueryRowContext(ctx, `SELECT `+workerIdentitySelectColumns+` FROM task_ledger_worker_identities WHERE workspace_id=? AND worker_instance_id=? AND principal_id=?`, normalized.WorkspaceID, normalized.WorkerInstanceID, normalized.PrincipalID))
}

func (l *agentTaskDeliveryLedger) workerIdentityUpdateByID(ctx context.Context, updateID string) (agentWorkerIdentityUpdateRecord, error) {
	return scanWorkerIdentityUpdate(l.db.QueryRowContext(ctx, `SELECT `+workerIdentityUpdateSelectColumns+` FROM task_ledger_worker_identity_updates WHERE update_id=?`, strings.TrimSpace(updateID)))
}

func (l *agentTaskDeliveryLedger) newWorkerIdentityID(ctx context.Context, tx *sql.Tx, kind, prefix string) (string, error) {
	return l.newUniqueID(ctx, tx, kind, prefix)
}

func validateWorkerIdentityInstanceAuthority(ctx context.Context, tx *sql.Tx, authority agentWorkerIdentityAuthority, requested string) error {
	rows, err := tx.QueryContext(ctx, `SELECT principal_id,workspace_id,requested_worker_id FROM task_ledger_worker_identities WHERE worker_instance_id=?`, authority.WorkerInstanceID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var principalID, workspaceID, requestedWorkerID string
		if err := rows.Scan(&principalID, &workspaceID, &requestedWorkerID); err != nil {
			return err
		}
		if principalID != authority.PrincipalID || workspaceID != authority.WorkspaceID || requestedWorkerID != requested {
			return errors.New("worker instance is bound to a different principal, workspace, or requested worker ID")
		}
	}
	return rows.Err()
}

func workerIdentityConstraintCollision(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint") || strings.Contains(message, "constraint failed") || strings.Contains(message, "constraint violation")
}

func workerIdentityCredentialMigrationSourceDigest(identity agentWorkerIdentityRecord) string {
	return agentTaskDigest(map[string]any{
		"phase": workerIdentityCredentialMigrationPhase, "identity_id": identity.IdentityID,
		"principal_id": identity.PrincipalID, "workspace_id": identity.WorkspaceID,
		"requested_worker_id": identity.RequestedWorkerID, "canonical_worker_id": identity.CanonicalWorkerID,
		"worker_instance_id": identity.WorkerInstanceID, "identity_digest": identity.IdentityDigest,
		"identity_generation":   identity.IdentityUpdateGeneration,
		"credential_generation": identity.WorkerInstanceCredentialGeneration,
	})
}

func workerIdentityCredentialMigrationReceiptID(sourceDigest string) string {
	trimmed := strings.TrimPrefix(strings.TrimSpace(sourceDigest), "sha256:")
	if len(trimmed) > 32 {
		trimmed = trimmed[:32]
	}
	return "worker-identity-credential-migration-" + trimmed
}

func workerIdentityMigrationClaimEligibility(approvalPolicyJSON, contextRequestJSON string, approved int) int {
	policy := decodeAgentTaskMap(approvalPolicyJSON)
	contextRequest := decodeAgentTaskMap(contextRequestJSON)
	eligible := agentTaskCanonicalSHA256(anyToString(contextRequest["content_hash"])) && strings.TrimSpace(anyToString(contextRequest["session_id"])) != ""
	if anyToBool(policy["required"]) && approved == 0 {
		eligible = false
	}
	return boolToSQLiteInt(eligible)
}

func workerIdentityAttemptBindingPredicate(alias string) string {
	// The worker spelling is only meaningful together with the exact instance
	// and identity-update generation that issued it.  A requested/canonical
	// worker ID is a routing alias, not an authority on its own.
	return `EXISTS (SELECT 1 FROM task_ledger_worker_identities wi JOIN task_ledger_tasks binding_task ON binding_task.id=` + alias + `.task_id WHERE wi.identity_id=? AND wi.principal_id=? AND wi.worker_instance_id=` + alias + `.worker_instance_id AND lower(trim(wi.workspace_id))=lower(trim(binding_task.workspace_id)) AND ((` + alias + `.worker_identity_update_generation=0 AND lower(trim(` + alias + `.worker_id))=lower(?)) OR (` + alias + `.worker_identity_update_generation>0 AND ` + alias + `.worker_identity_update_generation=wi.worker_identity_update_generation AND lower(trim(` + alias + `.worker_id))=lower(?))))`
}

func workerIdentityTaskBindingPredicate(alias string) string {
	// Queued claims are bound by this durable row, never by a workspace-wide
	// requested/canonical spelling.  The generation predicate intentionally
	// accepts only the pre-update generation or the exact current generation;
	// no other instance can satisfy the identity/instance/generation tuple.
	return `EXISTS (SELECT 1 FROM task_ledger_worker_task_bindings b WHERE b.task_id=` + alias + `.id AND b.identity_id=? AND b.principal_id=? AND lower(trim(b.workspace_id))=lower(?) AND b.worker_instance_id=? AND ((b.worker_identity_update_generation=0 AND lower(trim(b.worker_id))=lower(?)) OR (b.worker_identity_update_generation=? AND ? > 0 AND lower(trim(b.worker_id))=lower(?))) AND b.state IN ('bound','rebind_pending'))`
}

func workerIdentityAttemptBindingArgs(identity agentWorkerIdentityRecord) []any {
	return []any{identity.IdentityID, identity.PrincipalID, identity.RequestedWorkerID, identity.CanonicalWorkerID}
}

func workerIdentityTaskBindingArgs(identity agentWorkerIdentityRecord) []any {
	return []any{identity.IdentityID, identity.PrincipalID, identity.WorkspaceID, identity.WorkerInstanceID, identity.RequestedWorkerID, identity.IdentityUpdateGeneration, identity.IdentityUpdateGeneration, identity.CanonicalWorkerID}
}

// workerIdentityGenerationZeroSourceBoundTx classifies the durable task binding
// before any generation-zero attempt creates a queued successor. An unregistered
// attempt stays compatible with legacy delivery only while no other active
// instance occupies its worker alias. Once a collision exists, retry, review,
// answer, and quarantine successors remain ineligible until the exact owner ACK
// adopts their immutable source evidence. Any existing binding must prove the
// requested worker/instance authority at generation zero or at its fully
// acknowledged canonical generation; foreign, pending, and stale rows fail
// closed.
func workerIdentityGenerationZeroSourceBoundTx(ctx context.Context, tx *sql.Tx, taskID, attemptWorkerID, attemptWorkerInstanceID string) (bool, error) {
	var (
		bindingIdentity, bindingPrincipal, bindingWorkspace, bindingRequested, bindingCanonical, bindingWorker, bindingInstance, bindingState string
		identityID, identityPrincipal, identityWorkspace, identityRequested, identityCanonical, identityInstance, identityStatus              string
		taskWorkspace                                                                                                                         string
		bindingGeneration, identityGeneration, acknowledgedGeneration                                                                         int
	)
	err := tx.QueryRowContext(ctx, `SELECT
		b.identity_id,b.principal_id,b.workspace_id,b.requested_worker_id,b.canonical_worker_id,b.worker_id,b.worker_instance_id,b.worker_identity_update_generation,b.state,
		wi.identity_id,wi.principal_id,wi.workspace_id,wi.requested_worker_id,wi.canonical_worker_id,wi.worker_instance_id,wi.worker_identity_update_generation,wi.acknowledged_generation,wi.status,
		t.workspace_id
		FROM task_ledger_worker_task_bindings b
		JOIN task_ledger_worker_identities wi ON wi.identity_id=b.identity_id
		JOIN task_ledger_tasks t ON t.id=b.task_id
		WHERE b.task_id=?`, strings.TrimSpace(taskID)).Scan(
		&bindingIdentity, &bindingPrincipal, &bindingWorkspace, &bindingRequested, &bindingCanonical, &bindingWorker, &bindingInstance, &bindingGeneration, &bindingState,
		&identityID, &identityPrincipal, &identityWorkspace, &identityRequested, &identityCanonical, &identityInstance, &identityGeneration, &acknowledgedGeneration, &identityStatus,
		&taskWorkspace,
	)
	if errors.Is(err, sql.ErrNoRows) {
		var foreignOccupiers int
		if collisionErr := tx.QueryRowContext(ctx, `SELECT COUNT(*)
			FROM task_ledger_worker_identities wi
			JOIN task_ledger_tasks t ON t.id=?
			WHERE wi.status='active'
			 AND lower(trim(wi.workspace_id))=lower(trim(t.workspace_id))
			 AND (lower(trim(wi.requested_worker_id))=lower(trim(?)) OR lower(trim(wi.canonical_worker_id))=lower(trim(?)))
			 AND wi.worker_instance_id<>?`, strings.TrimSpace(taskID), strings.TrimSpace(attemptWorkerID), strings.TrimSpace(attemptWorkerID), strings.TrimSpace(attemptWorkerInstanceID)).Scan(&foreignOccupiers); collisionErr != nil {
			return false, collisionErr
		}
		return foreignOccupiers == 0, nil
	}
	if err != nil {
		return false, err
	}
	commonExact := bindingState == "bound" && identityStatus == "active" &&
		bindingIdentity == identityID && bindingPrincipal == identityPrincipal && strings.EqualFold(bindingWorkspace, identityWorkspace) && strings.EqualFold(bindingWorkspace, taskWorkspace) &&
		bindingRequested == identityRequested && bindingCanonical == identityCanonical && bindingInstance == identityInstance && bindingInstance == strings.TrimSpace(attemptWorkerInstanceID) &&
		strings.EqualFold(identityRequested, strings.TrimSpace(attemptWorkerID))
	generationZeroExact := bindingGeneration == 0 && identityGeneration == 0 && acknowledgedGeneration == 0 && strings.EqualFold(bindingWorker, identityRequested)
	canonicalExact := bindingGeneration > 0 && bindingGeneration == identityGeneration && bindingGeneration == acknowledgedGeneration && strings.EqualFold(bindingWorker, identityCanonical)
	if !commonExact || (!generationZeroExact && !canonicalExact) {
		return false, errors.New("generation-zero source attempt has a foreign, pending, or stale worker task binding")
	}
	return true, nil
}

type workerIdentityMigrationBlockedWork struct {
	Kind   string
	ID     string
	Status string
}

type workerIdentityMigrationDisabledTask struct {
	TaskID         string
	ClaimWorkerID  string
	PreviousStatus string
	ClaimEligible  int
}

// bindWorkerIdentityTaskTx records the exact worker authority that issued a
// claim.  The task's worker spelling remains a compatibility projection, but
// this row is the durable authorization fence used for queued migration and
// retirement.  A task can never be silently rebound to another identity.
func bindWorkerIdentityTaskTx(ctx context.Context, tx *sql.Tx, taskID string, identity agentWorkerIdentityRecord, workerID string, generation int) error {
	taskID = strings.TrimSpace(taskID)
	workerID = strings.TrimSpace(workerID)
	if taskID == "" || identity.IdentityID == "" || identity.WorkerInstanceID == "" {
		return errors.New("worker task binding authority is incomplete")
	}
	expectedWorker := identity.CanonicalWorkerID
	if generation == 0 {
		expectedWorker = identity.RequestedWorkerID
	}
	if generation < 0 || generation != 0 && generation != identity.IdentityUpdateGeneration || !strings.EqualFold(workerID, expectedWorker) {
		return errors.New("worker task binding generation or worker ID is not authoritative")
	}
	now := agentTaskNow()
	var existingIdentity, existingPrincipal, existingWorkspace, existingInstance, existingWorker string
	var existingGeneration int
	lookupErr := tx.QueryRowContext(ctx, `SELECT identity_id,principal_id,workspace_id,worker_instance_id,worker_id,worker_identity_update_generation FROM task_ledger_worker_task_bindings WHERE task_id=?`, taskID).Scan(&existingIdentity, &existingPrincipal, &existingWorkspace, &existingInstance, &existingWorker, &existingGeneration)
	if lookupErr == nil {
		if existingIdentity != identity.IdentityID || existingPrincipal != identity.PrincipalID || !strings.EqualFold(existingWorkspace, identity.WorkspaceID) || existingInstance != identity.WorkerInstanceID || existingGeneration != generation || !strings.EqualFold(existingWorker, workerID) {
			return errors.New("worker task binding is owned by a different identity")
		}
		_, err := tx.ExecContext(ctx, `UPDATE task_ledger_worker_task_bindings SET requested_worker_id=?,canonical_worker_id=?,worker_id=?,state='bound',updated_at=? WHERE task_id=? AND identity_id=? AND worker_instance_id=? AND worker_identity_update_generation=? AND state IN ('bound','rebind_pending')`, identity.RequestedWorkerID, identity.CanonicalWorkerID, workerID, now, taskID, identity.IdentityID, identity.WorkerInstanceID, generation)
		return err
	}
	if !errors.Is(lookupErr, sql.ErrNoRows) {
		return lookupErr
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO task_ledger_worker_task_bindings(task_id,identity_id,principal_id,workspace_id,requested_worker_id,canonical_worker_id,worker_id,worker_instance_id,worker_identity_update_generation,state,rebind_update_id,rebind_receipt_digest,rebind_acknowledged_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,'bound','','','',?,?)`, taskID, identity.IdentityID, identity.PrincipalID, identity.WorkspaceID, identity.RequestedWorkerID, identity.CanonicalWorkerID, workerID, identity.WorkerInstanceID, generation, now, now)
	return err
}

// rebindWorkerIdentityTaskBindingsTx applies a collision rename only after
// the owning worker has acknowledged the exact server-issued update. The
// transaction is the fence: every exact task binding and its compatibility
// projection, regardless of task status, are normalized together with the
// durable receipt/event. This is necessary because a running task can later
// requeue; leaving its binding at the old generation would strand that retry
// behind a stale worker spelling. A blank projection is legitimate when the
// exact binding is authoritative, but a nonblank foreign projection fails
// closed rather than being broadened or overwritten.
func (l *agentTaskDeliveryLedger) rebindWorkerIdentityTaskBindingsTx(ctx context.Context, tx *sql.Tx, identity agentWorkerIdentityRecord, update agentWorkerIdentityUpdateRecord, ackReceiptDigest string) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT b.task_id,b.worker_id,t.status,t.claim_worker_id,t.claim_eligible,t.approval_policy_json,t.context_request_json,t.approved FROM task_ledger_worker_task_bindings b JOIN task_ledger_tasks t ON t.id=b.task_id WHERE b.identity_id=? AND b.principal_id=? AND lower(trim(b.workspace_id))=lower(?) AND b.worker_instance_id=? AND b.worker_identity_update_generation=? AND lower(trim(b.worker_id))=lower(?) AND b.state IN ('bound','rebind_pending') ORDER BY b.task_id`, identity.IdentityID, identity.PrincipalID, identity.WorkspaceID, identity.WorkerInstanceID, update.IdentityUpdateGeneration-1, update.OldWorkerID)
	if err != nil {
		return 0, err
	}
	type queuedBinding struct {
		TaskID, WorkerID, Status, ClaimWorkerID, ApprovalPolicyJSON, ContextRequestJSON string
		ClaimEligible, Approved                                                         int
	}
	items := make([]queuedBinding, 0)
	for rows.Next() {
		var item queuedBinding
		if err := rows.Scan(&item.TaskID, &item.WorkerID, &item.Status, &item.ClaimWorkerID, &item.ClaimEligible, &item.ApprovalPolicyJSON, &item.ContextRequestJSON, &item.Approved); err != nil {
			_ = rows.Close()
			return 0, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	count := 0
	for _, item := range items {
		claimWorkerID := strings.TrimSpace(item.ClaimWorkerID)
		if claimWorkerID != "" && !strings.EqualFold(claimWorkerID, update.OldWorkerID) && !strings.EqualFold(claimWorkerID, update.NewWorkerID) {
			return 0, fmt.Errorf("worker identity task binding %s has a foreign claim worker projection", item.TaskID)
		}
		eligible := item.ClaimEligible
		if item.Status == "queued" {
			eligible = workerIdentityMigrationClaimEligibility(item.ApprovalPolicyJSON, item.ContextRequestJSON, item.Approved)
		}
		updated, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET claim_worker_id=?,claim_eligible=CASE WHEN status='queued' THEN ? ELSE claim_eligible END,updated_at=? WHERE id=? AND (trim(claim_worker_id)='' OR lower(trim(claim_worker_id))=lower(?) OR lower(trim(claim_worker_id))=lower(?))`, update.NewWorkerID, eligible, agentTaskNow(), item.TaskID, update.OldWorkerID, update.NewWorkerID)
		if err != nil {
			return 0, err
		}
		if affected, err := updated.RowsAffected(); err != nil || affected != 1 {
			if err != nil {
				return 0, err
			}
			return 0, fmt.Errorf("worker identity queued claim rebind lost task %s", item.TaskID)
		}
		receipt := agentTaskDigest(map[string]any{
			"task_id": item.TaskID, "identity_id": identity.IdentityID, "principal_id": identity.PrincipalID,
			"workspace_id": identity.WorkspaceID, "worker_instance_id": identity.WorkerInstanceID,
			"old_worker_id": update.OldWorkerID, "new_worker_id": update.NewWorkerID,
			"worker_identity_update_generation": update.IdentityUpdateGeneration,
			"previous_task_status":              item.Status, "previous_claim_worker_id": claimWorkerID,
			"update_id": update.UpdateID, "ack_receipt_digest": ackReceiptDigest,
		})
		bindingUpdate, err := tx.ExecContext(ctx, `UPDATE task_ledger_worker_task_bindings SET requested_worker_id=?,canonical_worker_id=?,worker_id=?,worker_identity_update_generation=?,state='bound',rebind_update_id=?,rebind_receipt_digest=?,rebind_acknowledged_at=?,updated_at=? WHERE task_id=? AND identity_id=? AND principal_id=? AND lower(trim(workspace_id))=lower(?) AND worker_instance_id=? AND worker_identity_update_generation=? AND lower(trim(worker_id))=lower(?) AND state IN ('bound','rebind_pending')`, update.RequestedWorkerID, update.CanonicalWorkerID, update.NewWorkerID, update.IdentityUpdateGeneration, update.UpdateID, receipt, agentTaskNow(), agentTaskNow(), item.TaskID, identity.IdentityID, identity.PrincipalID, identity.WorkspaceID, identity.WorkerInstanceID, update.IdentityUpdateGeneration-1, update.OldWorkerID)
		if err != nil {
			return 0, err
		}
		if affected, err := bindingUpdate.RowsAffected(); err != nil || affected != 1 {
			if err != nil {
				return 0, err
			}
			return 0, fmt.Errorf("worker identity queued claim rebind lost exact binding for task %s", item.TaskID)
		}
		eventStatus := item.Status
		eventMessage := "worker identity update rebound an exact task binding"
		if item.Status == "queued" {
			eventStatus = "queued"
			eventMessage = "worker identity update rebound an exact queued claim"
		}
		if err := l.appendEventTx(ctx, tx, item.TaskID, "", eventStatus, eventMessage, map[string]any{
			"identity_id": identity.IdentityID, "principal_id": identity.PrincipalID, "workspace_id": identity.WorkspaceID,
			"worker_instance_id": identity.WorkerInstanceID, "old_worker_id": update.OldWorkerID, "new_worker_id": update.NewWorkerID,
			"worker_identity_update_generation": update.IdentityUpdateGeneration, "identity_update_id": update.UpdateID,
			"claim_rebind_receipt_digest": receipt, "claim_rebind_acknowledged": true,
			"previous_task_status": item.Status, "previous_claim_worker_id": claimWorkerID,
			"claim_eligible": eligible != 0,
		}); err != nil {
			return 0, err
		}
		count++
	}
	return count, nil
}

// workerIdentityPreRegistrationAdoptionCTE is the single closed classifier for
// every supported generation-zero state that can still require worker custody.
// The same evidence predicate selects candidates and fences the projection CAS,
// so adding a lifecycle state cannot silently widen only one half of adoption.
const workerIdentityPreRegistrationAdoptionCTE = `
	WITH pre_registration_attempts AS (
		SELECT
			a.attempt_id,a.task_id,a.attempt_number,a.generation,a.worker_id,a.worker_instance_id,a.status AS attempt_status,a.observation_digest,a.failure_disposition,
			t.status AS task_status,COALESCE(t.claim_worker_id,'') AS claim_worker_id,t.claim_eligible,t.approval_policy_json,t.context_request_json,t.approved,
			t.revision_envelope_json,COALESCE(t.active_attempt_id,'') AS active_attempt_id,
			t.attempt_number AS task_attempt_number,t.generation AS task_generation,
			COALESCE(t.result_id,'') AS task_result_id,COALESCE(t.publication_id,'') AS task_publication_id,t.review_owner
		FROM task_ledger_attempts a
		JOIN task_ledger_tasks t ON t.id=a.task_id
		WHERE a.worker_instance_id=?
		  AND lower(trim(t.workspace_id))=lower(?)
		  AND a.worker_identity_update_generation=0
		  AND lower(trim(a.worker_id))=lower(?)
	),
	pre_registration_publications AS (
		SELECT
			source.attempt_id,r.result_id AS evidence_result_id,p.publication_id AS evidence_publication_id,
			r.status AS result_status,r.payload_json AS result_payload_json,r.digest AS result_digest,
			p.status AS publication_status,p.writeback_status,
			CASE WHEN
				r.schema_id='agent_task_result_manifest.v1'
				AND r.execution_observed=1 AND r.immutable=1
				AND length(r.digest)=71 AND substr(r.digest,1,7)='sha256:' AND substr(r.digest,8) NOT GLOB '*[^0-9a-f]*'
				AND json_valid(r.payload_json)=1
				AND json_extract(r.payload_json,'$.result_id')=r.result_id
				AND json_extract(r.payload_json,'$.task_id')=source.task_id
				AND json_extract(r.payload_json,'$.attempt_id')=source.attempt_id
				AND length(p.intent_digest)=71 AND substr(p.intent_digest,1,7)='sha256:' AND substr(p.intent_digest,8) NOT GLOB '*[^0-9a-f]*'
				AND p.delivery_row_count>0
				AND (SELECT COUNT(*) FROM task_ledger_deliveries d WHERE d.publication_id=p.publication_id)=p.delivery_row_count
				AND NOT EXISTS (
					SELECT 1 FROM task_ledger_deliveries d
					WHERE d.publication_id=p.publication_id
					  AND (d.result_id<>r.result_id OR d.task_id<>source.task_id)
				)
			THEN 1 ELSE 0 END AS exact_immutable_publication
		FROM pre_registration_attempts source
		JOIN task_ledger_results r ON r.task_id=source.task_id AND r.attempt_id=source.attempt_id
		JOIN task_ledger_publications p ON p.result_id=r.result_id AND p.task_id=r.task_id AND p.attempt_id=r.attempt_id
		WHERE (SELECT COUNT(*) FROM task_ledger_results other_result WHERE other_result.task_id=source.task_id AND other_result.attempt_id=source.attempt_id)=1
		  AND (SELECT COUNT(*) FROM task_ledger_publications other_publication WHERE other_publication.task_id=source.task_id AND other_publication.attempt_id=source.attempt_id)=1
	),
	pre_registration_candidates AS (
		SELECT source.*,publication.evidence_result_id,publication.evidence_publication_id,publication.result_status,
			COALESCE(publication.result_payload_json,'') AS result_payload_json,COALESCE(publication.result_digest,'') AS result_digest,
			publication.publication_status,publication.writeback_status,
			COALESCE(publication.exact_immutable_publication,0) AS exact_immutable_publication,
			CASE WHEN source.active_attempt_id=source.attempt_id AND source.task_attempt_number=source.attempt_number AND source.task_generation=source.generation THEN 1 ELSE 0 END AS exact_active_projection,
			CASE WHEN trim(source.active_attempt_id)='' AND source.task_attempt_number=source.attempt_number AND source.task_generation=source.generation THEN 1 ELSE 0 END AS exact_queued_projection,
			CASE WHEN NOT EXISTS (
				SELECT 1 FROM task_ledger_attempts later
				WHERE later.task_id=source.task_id AND later.attempt_id<>source.attempt_id
				  AND (later.attempt_number>source.attempt_number OR (later.attempt_number=source.attempt_number AND later.generation>source.generation))
			) THEN 1 ELSE 0 END AS latest_attempt,
			CASE WHEN NOT EXISTS (SELECT 1 FROM task_ledger_results r WHERE r.task_id=source.task_id AND r.attempt_id=source.attempt_id)
				 AND NOT EXISTS (SELECT 1 FROM task_ledger_publications p WHERE p.task_id=source.task_id AND p.attempt_id=source.attempt_id)
			THEN 1 ELSE 0 END AS no_result_or_publication,
			CASE WHEN length(source.observation_digest)=71 AND substr(source.observation_digest,1,7)='sha256:' AND substr(source.observation_digest,8) NOT GLOB '*[^0-9a-f]*'
			THEN 1 ELSE 0 END AS exact_observation,
			CASE WHEN NOT EXISTS (
				SELECT 1 FROM task_ledger_reviews v WHERE v.task_id=source.task_id AND v.result_id=publication.evidence_result_id
			) THEN 1 ELSE 0 END AS no_review_record,
			CASE WHEN EXISTS (
				SELECT 1
				FROM task_ledger_reviews v
				WHERE v.task_id=source.task_id AND v.result_id=publication.evidence_result_id
				  AND v.status='acknowledged' AND v.decision='acknowledge'
				  AND lower(trim(v.reviewer_owner))=lower(trim(source.review_owner))
				  AND lower(trim(v.actor))=lower(trim(source.review_owner))
			) THEN 1 ELSE 0 END AS exact_acknowledged_review,
			CASE WHEN NOT EXISTS (
				SELECT 1 FROM task_ledger_reviewer_claims c WHERE c.task_id=source.task_id AND c.result_id=publication.evidence_result_id
			) THEN 1 ELSE 0 END AS no_review_claim,
			CASE WHEN EXISTS (
				SELECT 1
				FROM task_ledger_reviewer_claims c
				JOIN task_ledger_deliveries d ON d.delivery_id=c.delivery_id AND d.result_id=c.result_id AND d.task_id=c.task_id
				WHERE c.task_id=source.task_id AND c.result_id=publication.evidence_result_id AND c.generation=source.generation AND c.status='active'
				  AND d.publication_id=publication.evidence_publication_id
				  AND lower(trim(c.reviewer_owner))=lower(trim(source.review_owner))
				  AND lower(trim(c.actor))=lower(trim(source.review_owner))
			) THEN 1 ELSE 0 END AS exact_active_review_claim,
			CASE WHEN EXISTS (
				SELECT 1
				FROM task_ledger_reviews v
				JOIN task_ledger_reviewer_claims c ON c.result_id=v.result_id AND c.task_id=v.task_id AND c.generation=source.generation
				JOIN task_ledger_deliveries d ON d.delivery_id=c.delivery_id AND d.result_id=c.result_id AND d.task_id=c.task_id
				WHERE v.task_id=source.task_id AND v.result_id=publication.evidence_result_id
				  AND v.status='review_blocked' AND v.decision='block'
				  AND lower(trim(v.reviewer_owner))=lower(trim(source.review_owner))
				  AND lower(trim(v.actor))=lower(trim(source.review_owner))
				  AND c.status='active'
				  AND lower(trim(c.reviewer_owner))=lower(trim(source.review_owner))
				  AND lower(trim(c.actor))=lower(trim(source.review_owner))
				  AND d.publication_id=publication.evidence_publication_id
			) THEN 1 ELSE 0 END AS exact_blocked_review,
			CASE WHEN (
				SELECT COUNT(*)
				FROM task_ledger_blocking_answers b
				JOIN task_ledger_reviews v ON v.task_id=b.task_id AND v.result_id=b.result_id
				JOIN task_ledger_reviewer_claims c ON c.task_id=v.task_id AND c.result_id=v.result_id AND c.generation=source.generation
				JOIN task_ledger_deliveries d ON d.delivery_id=b.delivery_id AND d.delivery_id=c.delivery_id AND d.task_id=b.task_id AND d.result_id=b.result_id
				WHERE b.task_id=source.task_id AND b.result_id=publication.evidence_result_id
				  AND b.source_attempt_id=source.attempt_id
				  AND trim(b.answer)<>''
				  AND (lower(trim(b.actor))=lower(trim(source.review_owner)) OR lower(trim(b.actor))=lower(trim(d.recipient_id)))
				  AND v.status='review_blocked' AND v.decision='block'
				  AND lower(trim(v.reviewer_owner))=lower(trim(source.review_owner))
				  AND lower(trim(v.actor))=lower(trim(source.review_owner))
				  AND c.status='active'
				  AND lower(trim(c.reviewer_owner))=lower(trim(source.review_owner))
				  AND lower(trim(c.actor))=lower(trim(source.review_owner))
				  AND d.publication_id=publication.evidence_publication_id
			)=1 THEN 1 ELSE 0 END AS exact_blocking_answer,
			CASE WHEN EXISTS (
				SELECT 1
				FROM task_ledger_reviews v
				JOIN task_ledger_results r ON r.result_id=v.result_id AND r.task_id=v.task_id AND r.attempt_id=source.attempt_id
				JOIN task_ledger_publications p ON p.result_id=r.result_id AND p.task_id=r.task_id AND p.attempt_id=r.attempt_id
				JOIN task_ledger_reviewer_claims c ON c.result_id=v.result_id AND c.task_id=v.task_id AND c.generation=source.generation
				JOIN task_ledger_deliveries d ON d.delivery_id=c.delivery_id AND d.result_id=c.result_id AND d.task_id=c.task_id AND d.publication_id=p.publication_id
				WHERE v.review_id=json_extract(source.revision_envelope_json,'$.review_id')
				  AND v.result_id=json_extract(source.revision_envelope_json,'$.source_result_id')
				  AND v.task_id=source.task_id
				  AND v.decision='request_changes' AND v.status='changes_requested'
				  AND v.reason=json_extract(source.revision_envelope_json,'$.reason')
				  AND lower(trim(v.reviewer_owner))=lower(trim(source.review_owner))
				  AND lower(trim(v.actor))=lower(trim(source.review_owner))
				  AND r.status='result_published' AND r.execution_observed=1 AND r.immutable=1
				  AND p.status='committed' AND p.writeback_status='committed'
				  AND c.status='changes_requested' AND lower(trim(c.reviewer_owner))=lower(trim(v.reviewer_owner)) AND lower(trim(c.actor))=lower(trim(v.actor))
			) THEN 1 ELSE 0 END AS exact_revision_review
		FROM pre_registration_attempts source
		LEFT JOIN pre_registration_publications publication ON publication.attempt_id=source.attempt_id
	),
	pre_registration_classified AS (
		SELECT *,CASE
			WHEN attempt_status IN ('leased','running','waiting_for_input')
			 AND task_status=attempt_status AND exact_active_projection=1 AND latest_attempt=1
			 AND COALESCE(failure_disposition,'')='' AND COALESCE(observation_digest,'')='' AND no_result_or_publication=1
			 AND trim(task_result_id)='' AND trim(task_publication_id)=''
			THEN 'execution_active'
			WHEN attempt_status='execution_observed' AND COALESCE(failure_disposition,'')=''
			 AND task_status='execution_observed' AND exact_active_projection=1 AND latest_attempt=1
			 AND exact_observation=1 AND no_result_or_publication=1
			 AND trim(task_result_id)='' AND trim(task_publication_id)=''
			THEN 'execution_observed'
			WHEN attempt_status='execution_observed' AND COALESCE(failure_disposition,'')=''
			 AND task_status IN ('writeback_pending','writeback_failed') AND exact_active_projection=1 AND latest_attempt=1
			 AND exact_observation=1 AND exact_immutable_publication=1
			 AND task_result_id=evidence_result_id AND task_publication_id=evidence_publication_id
			 AND result_status='publication_pending' AND publication_status=task_status
			 AND no_review_record=1 AND no_review_claim=1
			 AND ((task_status='writeback_pending' AND writeback_status='pending')
			   OR (task_status='writeback_failed' AND writeback_status NOT IN ('','pending','committed','succeeded','ok')))
			THEN 'publication_staged'
			WHEN attempt_status='quarantined' AND COALESCE(failure_disposition,'')=''
			 AND task_status='quarantined' AND exact_active_projection=1 AND latest_attempt=1 AND no_result_or_publication=1
			 AND trim(task_result_id)='' AND trim(task_publication_id)=''
			THEN 'quarantined'
			WHEN attempt_status='quarantined'
			 AND length(failure_disposition)=length('termination_verified_requeued:')+64
			 AND substr(failure_disposition,length('termination_verified_requeued:')+1) NOT GLOB '*[^0-9a-f]*'
			 AND task_status='queued' AND exact_queued_projection=1 AND latest_attempt=1
			 AND no_result_or_publication=1 AND trim(task_result_id)='' AND trim(task_publication_id)=''
			 AND json_valid(revision_envelope_json)=1 AND json_type(revision_envelope_json,'$')='object'
			THEN 'quarantine_resolved_queued'
			WHEN attempt_status='execution_failed' AND failure_disposition='retry_queued'
			 AND task_status='queued' AND exact_queued_projection=1 AND latest_attempt=1
			 AND exact_observation=1 AND no_result_or_publication=1 AND trim(task_result_id)='' AND trim(task_publication_id)=''
			 AND json_valid(revision_envelope_json)=1 AND json_type(revision_envelope_json,'$')='object'
			THEN 'retry_queued'
			WHEN attempt_status='completed' AND COALESCE(failure_disposition,'')=''
			 AND task_status='execution_succeeded' AND exact_active_projection=1 AND latest_attempt=1
			 AND exact_observation=1 AND exact_immutable_publication=1
			 AND result_status='result_published' AND publication_status='committed' AND writeback_status='committed'
			 AND task_result_id=evidence_result_id AND task_publication_id=evidence_publication_id
			 AND no_review_record=1 AND no_review_claim=1
			THEN 'execution_succeeded'
			WHEN attempt_status='completed' AND COALESCE(failure_disposition,'')=''
			 AND task_status='review_pending' AND exact_active_projection=1 AND latest_attempt=1
			 AND exact_observation=1 AND exact_immutable_publication=1
			 AND result_status='result_published' AND publication_status='committed' AND writeback_status='committed'
			 AND task_result_id=evidence_result_id AND task_publication_id=evidence_publication_id AND exact_active_review_claim=1
			 AND (no_review_record=1 OR exact_acknowledged_review=1)
			THEN 'review_claimed'
			WHEN attempt_status='completed' AND COALESCE(failure_disposition,'')=''
			 AND task_status='review_pending' AND exact_active_projection=1 AND latest_attempt=1
			 AND exact_observation=1 AND exact_immutable_publication=1
			 AND result_status='result_published' AND publication_status='committed' AND writeback_status='committed'
			 AND task_result_id=evidence_result_id AND task_publication_id=evidence_publication_id
			 AND exact_acknowledged_review=1 AND no_review_claim=1
			THEN 'review_acknowledged'
			WHEN attempt_status='completed' AND COALESCE(failure_disposition,'')=''
			 AND task_status='review_blocked' AND exact_active_projection=1 AND latest_attempt=1
			 AND exact_observation=1 AND exact_immutable_publication=1
			 AND result_status='result_published' AND publication_status='committed' AND writeback_status='committed'
			 AND task_result_id=evidence_result_id AND task_publication_id=evidence_publication_id
			 AND exact_active_review_claim=1 AND exact_blocked_review=1
			THEN 'review_blocked'
			WHEN attempt_status='completed' AND COALESCE(failure_disposition,'')=''
			 AND task_status='queued' AND exact_queued_projection=1 AND latest_attempt=1
			 AND trim(task_result_id)='' AND trim(task_publication_id)=''
			 AND exact_observation=1 AND exact_immutable_publication=1
			 AND result_status='result_published' AND publication_status='committed' AND writeback_status='committed'
			 AND exact_active_review_claim=1 AND exact_blocked_review=1 AND exact_blocking_answer=1
			 AND json_valid(revision_envelope_json)=1 AND json_type(revision_envelope_json,'$')='object'
			THEN 'review_answered_queued'
			WHEN attempt_status='completed' AND COALESCE(failure_disposition,'')=''
			 AND task_status='queued' AND exact_queued_projection=1 AND latest_attempt=1
			 AND trim(task_result_id)='' AND trim(task_publication_id)=''
			 AND exact_observation=1 AND exact_immutable_publication=1
			 AND result_status='result_published' AND publication_status='committed' AND writeback_status='committed'
			 AND json_valid(revision_envelope_json)=1 AND json_type(revision_envelope_json,'$')='object'
			 AND json_extract(revision_envelope_json,'$.schema_id')='agent_task_revision_source.v1'
			 AND json_extract(revision_envelope_json,'$.task_id')=task_id
			 AND json_extract(revision_envelope_json,'$.source_attempt_id')=attempt_id
			 AND json_extract(revision_envelope_json,'$.source_generation')=generation
			 AND exact_revision_review=1
			THEN 'revision_queued'
			WHEN latest_attempt=1 AND (
				task_status IN ('leased','running','waiting_for_input','execution_observed','writeback_pending','writeback_failed','execution_succeeded','review_pending','review_blocked','quarantined')
				OR (task_status='queued' AND attempt_status IN ('execution_failed','quarantined','completed'))
			) THEN 'invalid'
			ELSE ''
		END AS adoption_kind
		FROM pre_registration_candidates
	),
	worker_identity_pre_registration_adoptions AS (
		SELECT * FROM pre_registration_classified WHERE adoption_kind<>''
	)
`

// adoptWorkerIdentityPreRegistrationAttemptsTx closes the compatibility gap
// that cannot be handled by the durable task-binding table alone: a generation-
// zero claim made before registration had no identity row available to bind.
// Once the exact owner acknowledges its collision update, the closed classifier
// above admits only one fully evidenced lifecycle state. A foreign task
// projection or binding is a hard failure; adoption never broadens an unowned
// task or overwrites another identity.
func (l *agentTaskDeliveryLedger) adoptWorkerIdentityPreRegistrationAttemptsTx(ctx context.Context, tx *sql.Tx, identity agentWorkerIdentityRecord, update agentWorkerIdentityUpdateRecord, ackReceiptDigest string) (int, error) {
	rows, err := tx.QueryContext(ctx, workerIdentityPreRegistrationAdoptionCTE+`SELECT
		attempt_id,task_id,attempt_number,generation,worker_id,worker_instance_id,attempt_status,failure_disposition,
		task_status,claim_worker_id,claim_eligible,approval_policy_json,context_request_json,approved,result_payload_json,result_digest,adoption_kind
		FROM worker_identity_pre_registration_adoptions
		ORDER BY attempt_id`, identity.WorkerInstanceID, identity.WorkspaceID, update.OldWorkerID)
	if err != nil {
		return 0, err
	}
	type preRegistrationAttempt struct {
		AttemptID, TaskID, WorkerID, WorkerInstanceID, AttemptStatus, FailureDisposition, TaskStatus, ClaimWorkerID, ApprovalPolicyJSON, ContextRequestJSON string
		ResultPayloadJSON, ResultDigest, AdoptionKind                                                                                                       string
		AttemptNumber, Generation, ClaimEligible, Approved                                                                                                  int
	}
	items := make([]preRegistrationAttempt, 0)
	for rows.Next() {
		var item preRegistrationAttempt
		if err := rows.Scan(&item.AttemptID, &item.TaskID, &item.AttemptNumber, &item.Generation, &item.WorkerID, &item.WorkerInstanceID, &item.AttemptStatus, &item.FailureDisposition, &item.TaskStatus, &item.ClaimWorkerID, &item.ClaimEligible, &item.ApprovalPolicyJSON, &item.ContextRequestJSON, &item.Approved, &item.ResultPayloadJSON, &item.ResultDigest, &item.AdoptionKind); err != nil {
			_ = rows.Close()
			return 0, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	count := 0
	for _, item := range items {
		if item.AdoptionKind == "invalid" {
			return 0, fmt.Errorf("worker identity pre-registration attempt %s has ambiguous lifecycle evidence", item.AttemptID)
		}
		if item.ResultPayloadJSON != "" && agentTaskDigest(decodeAgentTaskMap(item.ResultPayloadJSON)) != item.ResultDigest {
			return 0, fmt.Errorf("worker identity pre-registration attempt %s has invalid immutable result evidence", item.AttemptID)
		}
		claimWorkerID := strings.TrimSpace(item.ClaimWorkerID)
		if claimWorkerID != "" && !strings.EqualFold(claimWorkerID, update.OldWorkerID) && !strings.EqualFold(claimWorkerID, update.NewWorkerID) {
			return 0, fmt.Errorf("worker identity pre-registration attempt %s has a foreign claim worker projection", item.AttemptID)
		}

		var existingIdentity, existingPrincipal, existingWorkspace, existingWorker, existingInstance, existingState string
		var existingGeneration int
		bindingErr := tx.QueryRowContext(ctx, `SELECT identity_id,principal_id,workspace_id,worker_id,worker_instance_id,worker_identity_update_generation,state FROM task_ledger_worker_task_bindings WHERE task_id=?`, item.TaskID).Scan(&existingIdentity, &existingPrincipal, &existingWorkspace, &existingWorker, &existingInstance, &existingGeneration, &existingState)
		if bindingErr == nil {
			if existingIdentity != identity.IdentityID || existingPrincipal != identity.PrincipalID || !strings.EqualFold(existingWorkspace, identity.WorkspaceID) || existingInstance != identity.WorkerInstanceID || existingGeneration != update.IdentityUpdateGeneration || !strings.EqualFold(existingWorker, update.NewWorkerID) || existingState != "bound" {
				return 0, fmt.Errorf("worker identity pre-registration attempt %s has a foreign or stale task binding", item.AttemptID)
			}
			continue
		}
		if !errors.Is(bindingErr, sql.ErrNoRows) {
			return 0, bindingErr
		}

		eligible := item.ClaimEligible
		if item.TaskStatus == "queued" {
			eligible = workerIdentityMigrationClaimEligibility(item.ApprovalPolicyJSON, item.ContextRequestJSON, item.Approved)
		}
		now := agentTaskNow()
		updated, err := tx.ExecContext(ctx, workerIdentityPreRegistrationAdoptionCTE+`
			UPDATE task_ledger_tasks
			SET claim_worker_id=?,claim_eligible=CASE WHEN status='queued' THEN ? ELSE claim_eligible END,updated_at=?
			WHERE id=?
			  AND (trim(claim_worker_id)='' OR lower(trim(claim_worker_id))=lower(?) OR lower(trim(claim_worker_id))=lower(?))
			  AND EXISTS (
				SELECT 1 FROM worker_identity_pre_registration_adoptions exact
				WHERE exact.task_id=task_ledger_tasks.id AND exact.attempt_id=? AND exact.adoption_kind=?
			  )`, identity.WorkerInstanceID, identity.WorkspaceID, update.OldWorkerID, update.NewWorkerID, eligible, now, item.TaskID, update.OldWorkerID, update.NewWorkerID, item.AttemptID, item.AdoptionKind)
		if err != nil {
			return 0, err
		}
		if affected, err := updated.RowsAffected(); err != nil || affected != 1 {
			if err != nil {
				return 0, err
			}
			return 0, fmt.Errorf("worker identity pre-registration attempt %s lost its exact task projection", item.AttemptID)
		}
		receipt := agentTaskDigest(map[string]any{
			"kind": item.AdoptionKind, "attempt_id": item.AttemptID, "task_id": item.TaskID,
			"identity_id": identity.IdentityID, "principal_id": identity.PrincipalID, "workspace_id": identity.WorkspaceID,
			"worker_instance_id": identity.WorkerInstanceID, "old_worker_id": update.OldWorkerID, "new_worker_id": update.NewWorkerID,
			"worker_identity_update_generation": update.IdentityUpdateGeneration, "previous_attempt_status": item.AttemptStatus,
			"previous_task_status": item.TaskStatus, "previous_claim_worker_id": claimWorkerID, "attempt_number": item.AttemptNumber,
			"generation": item.Generation, "failure_disposition": item.FailureDisposition,
			"update_id": update.UpdateID, "ack_receipt_digest": ackReceiptDigest,
		})
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_ledger_worker_task_bindings(task_id,identity_id,principal_id,workspace_id,requested_worker_id,canonical_worker_id,worker_id,worker_instance_id,worker_identity_update_generation,state,rebind_update_id,rebind_receipt_digest,rebind_acknowledged_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,'bound',?,?,?,?,?)`, item.TaskID, identity.IdentityID, identity.PrincipalID, identity.WorkspaceID, update.RequestedWorkerID, update.CanonicalWorkerID, update.NewWorkerID, identity.WorkerInstanceID, update.IdentityUpdateGeneration, update.UpdateID, receipt, now, now, now); err != nil {
			return 0, err
		}
		eventMessage := "worker identity update adopted an exact pre-registration attempt binding"
		switch item.AdoptionKind {
		case "execution_observed":
			eventMessage = "worker identity update adopted an exact pre-registration execution observation binding"
		case "publication_staged":
			eventMessage = "worker identity update adopted an exact pre-registration staged publication binding"
		case "retry_queued":
			eventMessage = "worker identity update adopted an exact pre-registration retry binding"
		case "quarantined":
			eventMessage = "worker identity update adopted an exact pre-registration quarantined attempt binding"
		case "quarantine_resolved_queued":
			eventMessage = "worker identity update adopted an exact pre-registration resolved quarantine binding"
		case "review_acknowledged":
			eventMessage = "worker identity update adopted an exact pre-registration acknowledged review binding"
		case "review_claimed":
			eventMessage = "worker identity update adopted an exact pre-registration claimed review binding"
		case "review_blocked":
			eventMessage = "worker identity update adopted an exact pre-registration blocked review binding"
		case "review_answered_queued":
			eventMessage = "worker identity update adopted an exact pre-registration answered review binding"
		case "execution_succeeded":
			eventMessage = "worker identity update adopted an exact pre-registration completed publication binding"
		case "revision_queued":
			eventMessage = "worker identity update adopted an exact pre-registration revision binding"
		}
		if err := l.appendEventTx(ctx, tx, item.TaskID, item.AttemptID, item.TaskStatus, eventMessage, map[string]any{
			"identity_id": identity.IdentityID, "principal_id": identity.PrincipalID, "workspace_id": identity.WorkspaceID,
			"worker_instance_id": identity.WorkerInstanceID, "old_worker_id": update.OldWorkerID, "new_worker_id": update.NewWorkerID,
			"worker_identity_update_generation": update.IdentityUpdateGeneration, "identity_update_id": update.UpdateID,
			"pre_registration_attempt_rebind_receipt_digest": receipt, "pre_registration_attempt_rebind_acknowledged": true,
			"previous_attempt_status": item.AttemptStatus, "previous_task_status": item.TaskStatus, "attempt_number": item.AttemptNumber,
			"generation": item.Generation, "failure_disposition": item.FailureDisposition, "adoption_kind": item.AdoptionKind,
			"previous_claim_worker_id": claimWorkerID, "claim_eligible": eligible != 0,
		}); err != nil {
			return 0, err
		}
		count++
	}
	return count, nil
}

// disableLegacyWorkerIdentityQueuedClaims materializes a bounded batch before
// mutating it. Only a task carrying the exact identity/instance/generation
// binding may be disabled. A workspace-wide requested/canonical spelling is
// deliberately not sufficient: another live instance may use the same
// requested ID. Each disabled row retains a durable rebind receipt so it is
// visible to the owner/recovery authority rather than becoming an invisible
// permanently ineligible queue entry.
func (l *agentTaskDeliveryLedger) disableLegacyWorkerIdentityQueuedClaims(ctx context.Context, tx *sql.Tx, identity agentWorkerIdentityRecord, receiptID string) ([]workerIdentityMigrationDisabledTask, bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT t.id,t.claim_worker_id,t.claim_eligible FROM task_ledger_tasks t JOIN task_ledger_worker_task_bindings b ON b.task_id=t.id WHERE b.identity_id=? AND b.principal_id=? AND lower(trim(b.workspace_id))=lower(?) AND b.worker_instance_id=? AND ((b.worker_identity_update_generation=0 AND lower(trim(b.worker_id))=lower(?)) OR (b.worker_identity_update_generation=? AND ? > 0 AND lower(trim(b.worker_id))=lower(?))) AND b.state='bound' AND t.status='queued' ORDER BY t.id LIMIT ?`, identity.IdentityID, identity.PrincipalID, identity.WorkspaceID, identity.WorkerInstanceID, identity.RequestedWorkerID, identity.IdentityUpdateGeneration, identity.IdentityUpdateGeneration, identity.CanonicalWorkerID, workerIdentityCredentialMigrationBatchSize+1)
	if err != nil {
		return nil, false, err
	}
	items := make([]workerIdentityMigrationDisabledTask, 0)
	for rows.Next() {
		var item workerIdentityMigrationDisabledTask
		if err := rows.Scan(&item.TaskID, &item.ClaimWorkerID, &item.ClaimEligible); err != nil {
			rows.Close()
			return nil, false, err
		}
		item.PreviousStatus = "queued"
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, false, err
	}
	if err := rows.Close(); err != nil {
		return nil, false, err
	}
	more := len(items) > workerIdentityCredentialMigrationBatchSize
	if more {
		items = items[:workerIdentityCredentialMigrationBatchSize]
	}
	now := agentTaskNow()
	for _, item := range items {
		updated, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET claim_eligible=0,updated_at=? WHERE id=? AND status='queued' AND claim_eligible=1 AND EXISTS (SELECT 1 FROM task_ledger_worker_task_bindings b WHERE b.task_id=task_ledger_tasks.id AND b.identity_id=? AND b.principal_id=? AND lower(trim(b.workspace_id))=lower(?) AND b.worker_instance_id=? AND ((b.worker_identity_update_generation=0 AND lower(trim(b.worker_id))=lower(?)) OR (b.worker_identity_update_generation=? AND ? > 0 AND lower(trim(b.worker_id))=lower(?))) AND b.state IN ('bound','rebind_pending'))`, now, item.TaskID, identity.IdentityID, identity.PrincipalID, identity.WorkspaceID, identity.WorkerInstanceID, identity.RequestedWorkerID, identity.IdentityUpdateGeneration, identity.IdentityUpdateGeneration, identity.CanonicalWorkerID)
		if err != nil {
			return nil, false, err
		}
		affected, err := updated.RowsAffected()
		if err != nil {
			return nil, false, err
		}
		if item.ClaimEligible != 0 && affected != 1 {
			return nil, false, fmt.Errorf("legacy worker identity migration lost queued claim handoff for task %s", item.TaskID)
		}
		bindingUpdate, err := tx.ExecContext(ctx, `UPDATE task_ledger_worker_task_bindings SET state='rebind_pending',rebind_update_id=?,rebind_receipt_digest=?,rebind_acknowledged_at='',updated_at=? WHERE task_id=? AND identity_id=? AND principal_id=? AND lower(trim(workspace_id))=lower(?) AND worker_instance_id=?`, receiptID, agentTaskDigest(map[string]any{"migration_receipt_id": receiptID, "task_id": item.TaskID, "identity_id": identity.IdentityID, "principal_id": identity.PrincipalID, "workspace_id": identity.WorkspaceID, "worker_instance_id": identity.WorkerInstanceID}), now, item.TaskID, identity.IdentityID, identity.PrincipalID, identity.WorkspaceID, identity.WorkerInstanceID)
		if err != nil {
			return nil, false, err
		}
		if affected, err := bindingUpdate.RowsAffected(); err != nil || affected != 1 {
			if err != nil {
				return nil, false, err
			}
			return nil, false, fmt.Errorf("legacy worker identity migration lost exact queued binding for task %s", item.TaskID)
		}
		if err := l.appendEventTx(ctx, tx, item.TaskID, "", "queued", "legacy worker identity credential migration disabled an exact bound claim pending recovery rebind", map[string]any{
			"migration_receipt_id": receiptID, "identity_id": identity.IdentityID, "principal_id": identity.PrincipalID,
			"workspace_id": identity.WorkspaceID, "worker_instance_id": identity.WorkerInstanceID,
			"previous_status": item.PreviousStatus, "previous_claim_worker_id": item.ClaimWorkerID,
			"claim_eligible": false, "claim_rebind_required": true,
		}); err != nil {
			return nil, false, err
		}
	}
	return items, more, nil
}

// rebindLegacyWorkerIdentityQueuedClaims is the explicit operator recovery
// authority for a legacy identity whose queued canonical claims were fenced
// during credential migration.  The old worker cannot authenticate a row with
// an empty verifier, so no worker bearer is accepted here: the Gateway
// operator must name the exact immutable migration receipt and an already
// credential-bound replacement identity.  Every batch changes the binding,
// task projection, receipt digest, and event in one WAL transaction.  A
// repeated request finds no pending old binding and is therefore idempotent.
func (l *agentTaskDeliveryLedger) rebindLegacyWorkerIdentityQueuedClaims(ctx context.Context, request map[string]any) (map[string]any, error) {
	if request == nil {
		return nil, errors.New("worker identity queued-claim recovery request is required")
	}
	if err := agentTaskValidateStructured(request, "worker identity queued-claim recovery request", agentTaskEventMaxBytes); err != nil {
		return nil, err
	}
	if !strings.EqualFold(strings.TrimSpace(anyToString(request["phase"])), "worker_identity_rebind") {
		return nil, errors.New("worker identity queued-claim recovery phase is invalid")
	}
	oldIdentityID := strings.TrimSpace(anyToString(request["identity_id"]))
	newIdentityID := strings.TrimSpace(anyToString(request["new_identity_id"]))
	receiptID := strings.TrimSpace(anyToString(request["migration_receipt_id"]))
	if oldIdentityID == "" || newIdentityID == "" || receiptID == "" || oldIdentityID == newIdentityID {
		return nil, errors.New("worker identity queued-claim recovery requires distinct identity and migration receipt IDs")
	}
	if len([]byte(oldIdentityID)) > agentWorkerIdentityMaxIDBytes || len([]byte(newIdentityID)) > agentWorkerIdentityMaxIDBytes || len([]byte(receiptID)) > agentWorkerIdentityMaxIDBytes {
		return nil, errors.New("worker identity queued-claim recovery identifier is oversized")
	}
	total := 0
	recoveryDigest := ""
	recoveryUpdateID := ""
	for {
		tx, err := l.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		oldIdentity, err := scanWorkerIdentity(tx.QueryRowContext(ctx, `SELECT `+workerIdentitySelectColumns+` FROM task_ledger_worker_identities WHERE identity_id=?`, oldIdentityID))
		if errors.Is(err, sql.ErrNoRows) {
			_ = tx.Rollback()
			return nil, errors.New("worker identity queued-claim recovery old identity is unknown")
		}
		if err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		newIdentity, err := scanWorkerIdentity(tx.QueryRowContext(ctx, `SELECT `+workerIdentitySelectColumns+` FROM task_ledger_worker_identities WHERE identity_id=?`, newIdentityID))
		if errors.Is(err, sql.ErrNoRows) {
			_ = tx.Rollback()
			return nil, errors.New("worker identity queued-claim recovery replacement identity is unknown")
		}
		if err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		if (oldIdentity.Status != "closed" && oldIdentity.Status != "quarantined") || oldIdentity.WorkerInstanceCredentialVerifier != "" {
			_ = tx.Rollback()
			return nil, errors.New("worker identity queued-claim recovery requires a closed verifier-less legacy identity")
		}
		if newIdentity.Status != "active" || newIdentity.WorkerInstanceCredentialVerifier == "" || newIdentity.IdentityUpdateGeneration != newIdentity.AcknowledgedGeneration {
			_ = tx.Rollback()
			return nil, errors.New("worker identity queued-claim recovery requires an active acknowledged credential-bound replacement")
		}
		if oldIdentity.PrincipalID != newIdentity.PrincipalID || oldIdentity.WorkspaceID != newIdentity.WorkspaceID || !strings.EqualFold(oldIdentity.RequestedWorkerID, newIdentity.RequestedWorkerID) || oldIdentity.WorkerInstanceID == newIdentity.WorkerInstanceID {
			_ = tx.Rollback()
			return nil, errors.New("worker identity queued-claim recovery replacement does not bind the exact principal, workspace, requested worker, and new instance")
		}
		var sourceDigest, phase string
		var imported, validated, frozen, rolledBack int
		if err := tx.QueryRowContext(ctx, `SELECT source_digest,phase,imported,validated,frozen,rolled_back FROM task_ledger_migration_receipts WHERE receipt_id=?`, receiptID).Scan(&sourceDigest, &phase, &imported, &validated, &frozen, &rolledBack); err != nil {
			_ = tx.Rollback()
			return nil, errors.New("worker identity queued-claim recovery migration receipt is unavailable")
		}
		if phase != workerIdentityCredentialMigrationPhase || imported != 1 || validated != 1 || frozen != 1 || rolledBack != 0 || sourceDigest != workerIdentityCredentialMigrationSourceDigest(oldIdentity) {
			_ = tx.Rollback()
			return nil, errors.New("worker identity queued-claim recovery migration receipt does not bind the exact old identity")
		}
		if recoveryDigest == "" {
			recoveryDigest = agentTaskDigest(map[string]any{
				"kind": "worker_identity_queued_claim_recovery", "migration_receipt_id": receiptID,
				"old_identity_id": oldIdentity.IdentityID, "old_principal_id": oldIdentity.PrincipalID,
				"workspace_id": oldIdentity.WorkspaceID, "old_worker_instance_id": oldIdentity.WorkerInstanceID,
				"new_identity_id": newIdentity.IdentityID, "new_principal_id": newIdentity.PrincipalID,
				"new_worker_instance_id": newIdentity.WorkerInstanceID, "new_identity_generation": newIdentity.IdentityUpdateGeneration,
			})
			recoveryUpdateID = workerIdentityCredentialMigrationReceiptID(recoveryDigest)
		}
		rows, err := tx.QueryContext(ctx, `SELECT b.task_id,b.worker_id,t.claim_eligible,t.approval_policy_json,t.context_request_json,t.approved FROM task_ledger_worker_task_bindings b JOIN task_ledger_tasks t ON t.id=b.task_id WHERE b.identity_id=? AND b.principal_id=? AND lower(trim(b.workspace_id))=lower(?) AND b.worker_instance_id=? AND ((b.worker_identity_update_generation=0 AND lower(trim(b.worker_id))=lower(?)) OR (b.worker_identity_update_generation=? AND ? > 0 AND lower(trim(b.worker_id))=lower(?))) AND b.state='rebind_pending' AND t.status='queued' ORDER BY b.task_id LIMIT ?`, oldIdentity.IdentityID, oldIdentity.PrincipalID, oldIdentity.WorkspaceID, oldIdentity.WorkerInstanceID, oldIdentity.RequestedWorkerID, oldIdentity.IdentityUpdateGeneration, oldIdentity.IdentityUpdateGeneration, oldIdentity.CanonicalWorkerID, workerIdentityCredentialMigrationBatchSize+1)
		if err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		type pendingClaim struct {
			TaskID, WorkerID, ApprovalPolicyJSON, ContextRequestJSON string
			ClaimEligible, Approved                                  int
		}
		claims := make([]pendingClaim, 0, workerIdentityCredentialMigrationBatchSize+1)
		for rows.Next() {
			var claim pendingClaim
			if err := rows.Scan(&claim.TaskID, &claim.WorkerID, &claim.ClaimEligible, &claim.ApprovalPolicyJSON, &claim.ContextRequestJSON, &claim.Approved); err != nil {
				_ = rows.Close()
				_ = tx.Rollback()
				return nil, err
			}
			claims = append(claims, claim)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			_ = tx.Rollback()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		more := len(claims) > workerIdentityCredentialMigrationBatchSize
		if more {
			claims = claims[:workerIdentityCredentialMigrationBatchSize]
		}
		for _, claim := range claims {
			eligible := workerIdentityMigrationClaimEligibility(claim.ApprovalPolicyJSON, claim.ContextRequestJSON, claim.Approved)
			updated, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET claim_worker_id=?,claim_eligible=?,updated_at=? WHERE id=? AND status='queued' AND (trim(claim_worker_id)='' OR lower(trim(claim_worker_id))=lower(?)) AND EXISTS (SELECT 1 FROM task_ledger_worker_task_bindings b WHERE b.task_id=task_ledger_tasks.id AND b.identity_id=? AND b.principal_id=? AND lower(trim(b.workspace_id))=lower(?) AND b.worker_instance_id=? AND ((b.worker_identity_update_generation=0 AND lower(trim(b.worker_id))=lower(?)) OR (b.worker_identity_update_generation=? AND ? > 0 AND lower(trim(b.worker_id))=lower(?))) AND b.state='rebind_pending')`, newIdentity.CanonicalWorkerID, eligible, agentTaskNow(), claim.TaskID, claim.WorkerID, oldIdentity.IdentityID, oldIdentity.PrincipalID, oldIdentity.WorkspaceID, oldIdentity.WorkerInstanceID, oldIdentity.RequestedWorkerID, oldIdentity.IdentityUpdateGeneration, oldIdentity.IdentityUpdateGeneration, oldIdentity.CanonicalWorkerID)
			if err != nil {
				_ = tx.Rollback()
				return nil, err
			}
			if affected, err := updated.RowsAffected(); err != nil || affected != 1 {
				_ = tx.Rollback()
				if err != nil {
					return nil, err
				}
				return nil, fmt.Errorf("worker identity queued-claim recovery lost task %s", claim.TaskID)
			}
			claimReceipt := agentTaskDigest(map[string]any{
				"recovery_update_id": recoveryUpdateID, "recovery_digest": recoveryDigest, "task_id": claim.TaskID,
				"old_identity_id": oldIdentity.IdentityID, "old_worker_instance_id": oldIdentity.WorkerInstanceID,
				"new_identity_id": newIdentity.IdentityID, "new_worker_instance_id": newIdentity.WorkerInstanceID,
				"old_worker_id": claim.WorkerID, "new_worker_id": newIdentity.CanonicalWorkerID,
				"new_worker_identity_update_generation": newIdentity.IdentityUpdateGeneration,
			})
			updated, err = tx.ExecContext(ctx, `UPDATE task_ledger_worker_task_bindings SET identity_id=?,principal_id=?,workspace_id=?,requested_worker_id=?,canonical_worker_id=?,worker_id=?,worker_instance_id=?,worker_identity_update_generation=?,state='bound',rebind_update_id=?,rebind_receipt_digest=?,rebind_acknowledged_at=?,updated_at=? WHERE task_id=? AND identity_id=? AND principal_id=? AND lower(trim(workspace_id))=lower(?) AND worker_instance_id=? AND ((worker_identity_update_generation=0 AND lower(trim(worker_id))=lower(?)) OR (worker_identity_update_generation=? AND ? > 0 AND lower(trim(worker_id))=lower(?))) AND state='rebind_pending'`, newIdentity.IdentityID, newIdentity.PrincipalID, newIdentity.WorkspaceID, newIdentity.RequestedWorkerID, newIdentity.CanonicalWorkerID, newIdentity.CanonicalWorkerID, newIdentity.WorkerInstanceID, newIdentity.IdentityUpdateGeneration, recoveryUpdateID, claimReceipt, agentTaskNow(), agentTaskNow(), claim.TaskID, oldIdentity.IdentityID, oldIdentity.PrincipalID, oldIdentity.WorkspaceID, oldIdentity.WorkerInstanceID, oldIdentity.RequestedWorkerID, oldIdentity.IdentityUpdateGeneration, oldIdentity.IdentityUpdateGeneration, oldIdentity.CanonicalWorkerID)
			if err != nil {
				_ = tx.Rollback()
				return nil, err
			}
			if affected, err := updated.RowsAffected(); err != nil || affected != 1 {
				_ = tx.Rollback()
				if err != nil {
					return nil, err
				}
				return nil, fmt.Errorf("worker identity queued-claim recovery lost exact binding for task %s", claim.TaskID)
			}
			if err := l.appendEventTx(ctx, tx, claim.TaskID, "", "queued", "operator rebound an exact legacy worker identity claim", map[string]any{
				"migration_receipt_id": receiptID, "recovery_update_id": recoveryUpdateID, "recovery_receipt_digest": claimReceipt,
				"old_identity_id": oldIdentity.IdentityID, "old_principal_id": oldIdentity.PrincipalID, "old_workspace_id": oldIdentity.WorkspaceID,
				"old_worker_instance_id": oldIdentity.WorkerInstanceID, "new_identity_id": newIdentity.IdentityID, "new_principal_id": newIdentity.PrincipalID,
				"new_workspace_id": newIdentity.WorkspaceID, "new_worker_instance_id": newIdentity.WorkerInstanceID,
				"new_worker_identity_update_generation": newIdentity.IdentityUpdateGeneration, "claim_eligible": eligible != 0,
			}); err != nil {
				_ = tx.Rollback()
				return nil, err
			}
			total++
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		if !more {
			return map[string]any{
				"schema_id": "worker_identity_queued_claim_rebind_receipt.v1", "contract_version": 1,
				"migration_receipt_id": receiptID, "recovery_update_id": recoveryUpdateID, "recovery_digest": recoveryDigest,
				"old_identity_id": oldIdentityID, "new_identity_id": newIdentityID, "principal_id": oldIdentity.PrincipalID,
				"workspace_id": oldIdentity.WorkspaceID, "old_worker_instance_id": oldIdentity.WorkerInstanceID, "new_worker_instance_id": newIdentity.WorkerInstanceID,
				"rebound_claims": total, "idempotent_replay": total == 0, "authoritative_backend": "gateway-go-sqlite-wal",
			}, nil
		}
	}
}

// workerIdentityMigrationBoundWork is intentionally conservative. It runs
// after the lease-like attempts in the current migration batch have been
// durably requeued, and excludes only attempts carrying the migration failure
// disposition. Every other observed/result/publication/writeback/cleanup or
// downstream row remains proof-bound and quarantines the legacy identity.
func workerIdentityMigrationBoundWork(ctx context.Context, tx *sql.Tx, identity agentWorkerIdentityRecord) (workerIdentityMigrationBlockedWork, bool, error) {
	const terminalTaskStatuses = "('canceled','quarantined','execution_failed','rejected','superseded','unintegrated','integrated','integration_failed','dead_letter')"
	const terminalAttemptStatuses = "('execution_failed','canceled','quarantined','completed')"
	const terminalResultStatuses = "('result_published')"
	const terminalPublicationStatuses = "('committed','dead_letter')"
	const terminalDeliveryStatuses = "('delivered','acknowledged','dead_letter')"
	const terminalReviewStatuses = "('accepted_for_integration','changes_requested','rejected','superseded','knowledge_accepted','unintegrated')"
	const terminalIntegrationStatuses = "('integrated','rejected','unintegrated','follow_up_queued')"
	taskBinding := workerIdentityTaskBindingPredicate("t")
	attemptBinding := workerIdentityAttemptBindingPredicate("a")
	find := func(query string, args ...any) (workerIdentityMigrationBlockedWork, bool, error) {
		var work workerIdentityMigrationBlockedWork
		if err := tx.QueryRowContext(ctx, query, args...).Scan(&work.ID, &work.Status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return workerIdentityMigrationBlockedWork{}, false, nil
			}
			return workerIdentityMigrationBlockedWork{}, false, err
		}
		return work, true, nil
	}
	args := append([]any{identity.WorkspaceID}, workerIdentityTaskBindingArgs(identity)...)
	work, blocked, err := find(`SELECT t.id,t.status FROM task_ledger_tasks t WHERE lower(trim(t.workspace_id))=lower(?) AND `+taskBinding+` AND t.claim_eligible=1 AND t.status NOT IN `+terminalTaskStatuses+` AND NOT EXISTS (SELECT 1 FROM task_ledger_attempts migrated WHERE migrated.task_id=t.id AND migrated.failure_disposition='credential_migration_requeued') LIMIT 1`, args...)
	if err != nil || blocked {
		work.Kind = "task_claim"
		return work, blocked, err
	}
	work, blocked, err = find(`SELECT a.attempt_id,a.status FROM task_ledger_attempts a JOIN task_ledger_tasks t ON t.id=a.task_id WHERE lower(trim(t.workspace_id))=lower(?) AND a.worker_instance_id=? AND `+attemptBinding+` AND a.status NOT IN `+terminalAttemptStatuses+` LIMIT 1`, append([]any{identity.WorkspaceID, identity.WorkerInstanceID}, workerIdentityAttemptBindingArgs(identity)...)...)
	if err != nil || blocked {
		work.Kind = "attempt"
		return work, blocked, err
	}
	work, blocked, err = find(`SELECT r.result_id,r.status FROM task_ledger_results r JOIN task_ledger_attempts a ON a.attempt_id=r.attempt_id AND a.task_id=r.task_id JOIN task_ledger_tasks t ON t.id=r.task_id WHERE lower(trim(t.workspace_id))=lower(?) AND a.worker_instance_id=? AND `+attemptBinding+` AND r.status NOT IN `+terminalResultStatuses+` LIMIT 1`, append([]any{identity.WorkspaceID, identity.WorkerInstanceID}, workerIdentityAttemptBindingArgs(identity)...)...)
	if err != nil || blocked {
		work.Kind = "result"
		return work, blocked, err
	}
	work, blocked, err = find(`SELECT p.publication_id,p.status FROM task_ledger_publications p JOIN task_ledger_attempts a ON a.attempt_id=p.attempt_id AND a.task_id=p.task_id JOIN task_ledger_tasks t ON t.id=p.task_id WHERE lower(trim(t.workspace_id))=lower(?) AND a.worker_instance_id=? AND `+attemptBinding+` AND (p.status NOT IN `+terminalPublicationStatuses+` OR (p.status='committed' AND p.writeback_status<>'committed')) LIMIT 1`, append([]any{identity.WorkspaceID, identity.WorkerInstanceID}, workerIdentityAttemptBindingArgs(identity)...)...)
	if err != nil || blocked {
		work.Kind = "publication"
		return work, blocked, err
	}
	work, blocked, err = find(`SELECT d.delivery_id,d.status FROM task_ledger_deliveries d JOIN task_ledger_publications p ON p.publication_id=d.publication_id AND p.result_id=d.result_id AND p.task_id=d.task_id JOIN task_ledger_attempts a ON a.attempt_id=p.attempt_id AND a.task_id=p.task_id JOIN task_ledger_tasks t ON t.id=d.task_id WHERE lower(trim(t.workspace_id))=lower(?) AND a.worker_instance_id=? AND `+attemptBinding+` AND d.status NOT IN `+terminalDeliveryStatuses+` LIMIT 1`, append([]any{identity.WorkspaceID, identity.WorkerInstanceID}, workerIdentityAttemptBindingArgs(identity)...)...)
	if err != nil || blocked {
		work.Kind = "delivery"
		return work, blocked, err
	}
	work, blocked, err = find(`SELECT ar.artifact_id,CAST(ar.finalized AS TEXT) FROM task_ledger_artifacts ar JOIN task_ledger_attempts a ON a.attempt_id=ar.attempt_id AND a.task_id=ar.task_id JOIN task_ledger_tasks t ON t.id=ar.task_id WHERE lower(trim(t.workspace_id))=lower(?) AND a.worker_instance_id=? AND `+attemptBinding+` AND ar.finalized<>1 LIMIT 1`, append([]any{identity.WorkspaceID, identity.WorkerInstanceID}, workerIdentityAttemptBindingArgs(identity)...)...)
	if err != nil || blocked {
		work.Kind = "artifact"
		return work, blocked, err
	}
	work, blocked, err = find(`SELECT i.integration_id,i.status FROM task_ledger_integrations i JOIN task_ledger_results r ON r.result_id=i.result_id AND r.task_id=i.task_id JOIN task_ledger_attempts a ON a.attempt_id=r.attempt_id AND a.task_id=r.task_id JOIN task_ledger_tasks t ON t.id=i.task_id WHERE lower(trim(t.workspace_id))=lower(?) AND a.worker_instance_id=? AND `+attemptBinding+` AND i.status NOT IN `+terminalIntegrationStatuses+` LIMIT 1`, append([]any{identity.WorkspaceID, identity.WorkerInstanceID}, workerIdentityAttemptBindingArgs(identity)...)...)
	if err != nil || blocked {
		work.Kind = "integration"
		return work, blocked, err
	}
	work, blocked, err = find(`SELECT r.review_id,r.status FROM task_ledger_reviews r JOIN task_ledger_results result ON result.result_id=r.result_id AND result.task_id=r.task_id JOIN task_ledger_attempts a ON a.attempt_id=result.attempt_id AND a.task_id=result.task_id JOIN task_ledger_tasks t ON t.id=r.task_id WHERE lower(trim(t.workspace_id))=lower(?) AND a.worker_instance_id=? AND `+attemptBinding+` AND r.status NOT IN `+terminalReviewStatuses+` LIMIT 1`, append([]any{identity.WorkspaceID, identity.WorkerInstanceID}, workerIdentityAttemptBindingArgs(identity)...)...)
	if err != nil || blocked {
		work.Kind = "review"
		return work, blocked, err
	}
	// Reviewer claims remain proof-bound even when the result row itself is
	// terminal. Require the same exact terminal review closure as retirement;
	// an active, unknown, missing, or owner-mismatched closure quarantines the
	// legacy identity instead of silently tombstoning reviewer authority.
	reviewerRows, err := tx.QueryContext(ctx, `SELECT c.claim_id,c.status,c.reviewer_owner,c.actor,t.review_owner,COALESCE(r.review_id,''),COALESCE(r.status,''),COALESCE(r.reviewer_owner,''),COALESCE(r.actor,'') FROM task_ledger_reviewer_claims c JOIN task_ledger_results result ON result.result_id=c.result_id AND result.task_id=c.task_id JOIN task_ledger_attempts a ON a.attempt_id=result.attempt_id AND a.task_id=result.task_id JOIN task_ledger_tasks t ON t.id=c.task_id LEFT JOIN task_ledger_reviews r ON r.result_id=c.result_id AND r.task_id=c.task_id WHERE lower(trim(t.workspace_id))=lower(?) AND a.worker_instance_id=? AND `+attemptBinding, append([]any{identity.WorkspaceID, identity.WorkerInstanceID}, workerIdentityAttemptBindingArgs(identity)...)...)
	if err != nil {
		return workerIdentityMigrationBlockedWork{}, false, err
	}
	for reviewerRows.Next() {
		var claimID, claimStatus, claimOwner, claimActor, taskOwner, reviewID, reviewStatus, reviewOwner, reviewActor string
		if err := reviewerRows.Scan(&claimID, &claimStatus, &claimOwner, &claimActor, &taskOwner, &reviewID, &reviewStatus, &reviewOwner, &reviewActor); err != nil {
			reviewerRows.Close()
			return workerIdentityMigrationBlockedWork{}, false, err
		}
		if claimStatus == "active" || !workerIdentityTerminalReviewStatus(claimStatus) || reviewID == "" || reviewStatus != claimStatus || !workerIdentityTerminalReviewStatus(reviewStatus) || claimOwner == "" || !strings.EqualFold(claimOwner, taskOwner) || !strings.EqualFold(claimActor, taskOwner) || !strings.EqualFold(reviewOwner, taskOwner) || !strings.EqualFold(reviewActor, taskOwner) {
			reviewerRows.Close()
			return workerIdentityMigrationBlockedWork{Kind: "reviewer_claim", ID: claimID, Status: claimStatus}, true, nil
		}
	}
	if err := reviewerRows.Err(); err != nil {
		reviewerRows.Close()
		return workerIdentityMigrationBlockedWork{}, false, err
	}
	if err := reviewerRows.Close(); err != nil {
		return workerIdentityMigrationBlockedWork{}, false, err
	}
	// Approval authority is a set, not a single row. Scan every row in a
	// deterministic order and require the exact current task reviewer for each
	// nonterminal approval. LIMIT 1 would let an expired/ambiguous row hide a
	// second live approval or make the result depend on SQLite's row order.
	approvalRows, err := tx.QueryContext(ctx, `SELECT p.approval_id,p.status,p.expires_at,p.approver,t.review_owner,p.task_id,p.attempt_id FROM task_ledger_approvals p JOIN task_ledger_attempts a ON a.attempt_id=p.attempt_id AND a.task_id=p.task_id JOIN task_ledger_tasks t ON t.id=p.task_id WHERE lower(trim(t.workspace_id))=lower(?) AND a.worker_instance_id=? AND `+attemptBinding+` ORDER BY p.approval_id ASC`, append([]any{identity.WorkspaceID, identity.WorkerInstanceID}, workerIdentityAttemptBindingArgs(identity)...)...)
	if err != nil {
		return workerIdentityMigrationBlockedWork{}, false, err
	}
	type migrationApproval struct {
		ID, Status, ExpiresAt, Approver, ReviewOwner, TaskID, AttemptID string
	}
	approvals := make([]migrationApproval, 0)
	for approvalRows.Next() {
		var approval migrationApproval
		if err := approvalRows.Scan(&approval.ID, &approval.Status, &approval.ExpiresAt, &approval.Approver, &approval.ReviewOwner, &approval.TaskID, &approval.AttemptID); err != nil {
			_ = approvalRows.Close()
			return workerIdentityMigrationBlockedWork{}, false, err
		}
		approvals = append(approvals, approval)
	}
	if err := approvalRows.Err(); err != nil {
		_ = approvalRows.Close()
		return workerIdentityMigrationBlockedWork{}, false, err
	}
	if err := approvalRows.Close(); err != nil {
		return workerIdentityMigrationBlockedWork{}, false, err
	}
	now := time.Now().UTC()
	for _, approval := range approvals {
		if approval.TaskID == "" || approval.AttemptID == "" || approval.Approver == "" || !strings.EqualFold(approval.Approver, approval.ReviewOwner) {
			return workerIdentityMigrationBlockedWork{Kind: "approval", ID: approval.ID, Status: "invalid_authority"}, true, nil
		}
		switch approval.Status {
		case "used":
			continue
		case "valid":
			expires, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(approval.ExpiresAt))
			if parseErr != nil || now.Before(expires) {
				return workerIdentityMigrationBlockedWork{Kind: "approval", ID: approval.ID, Status: approval.Status}, true, nil
			}
		default:
			return workerIdentityMigrationBlockedWork{Kind: "approval", ID: approval.ID, Status: approval.Status}, true, nil
		}
	}
	// A committed/dead-letter publication is terminal only after exactly one
	// durable cleanup receipt. Materialize every terminal publication before
	// checking receipts so a second publication cannot hide behind LIMIT 1.
	publicationRows, err := tx.QueryContext(ctx, `SELECT p.publication_id,p.status FROM task_ledger_publications p JOIN task_ledger_attempts a ON a.attempt_id=p.attempt_id AND a.task_id=p.task_id JOIN task_ledger_tasks t ON t.id=p.task_id WHERE lower(trim(t.workspace_id))=lower(?) AND a.worker_instance_id=? AND `+attemptBinding+` AND p.status IN ('committed','dead_letter') ORDER BY p.publication_id`, append([]any{identity.WorkspaceID, identity.WorkerInstanceID}, workerIdentityAttemptBindingArgs(identity)...)...)
	if err != nil {
		return workerIdentityMigrationBlockedWork{}, false, err
	}
	type terminalPublication struct {
		ID     string
		Status string
	}
	terminalPublications := make([]terminalPublication, 0)
	for publicationRows.Next() {
		var publication terminalPublication
		if err := publicationRows.Scan(&publication.ID, &publication.Status); err != nil {
			publicationRows.Close()
			return workerIdentityMigrationBlockedWork{}, false, err
		}
		terminalPublications = append(terminalPublications, publication)
	}
	if err := publicationRows.Err(); err != nil {
		publicationRows.Close()
		return workerIdentityMigrationBlockedWork{}, false, err
	}
	if err := publicationRows.Close(); err != nil {
		return workerIdentityMigrationBlockedWork{}, false, err
	}
	for _, publication := range terminalPublications {
		var cleanupCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_ledger_cleanup_receipts WHERE publication_id=?`, publication.ID).Scan(&cleanupCount); err != nil {
			return workerIdentityMigrationBlockedWork{}, false, err
		}
		if cleanupCount != 1 {
			return workerIdentityMigrationBlockedWork{Kind: "cleanup", ID: publication.ID, Status: publication.Status}, true, nil
		}
	}
	if len(terminalPublications) > 0 {
		if err := workerIdentityRetirementCleanupReceipts(ctx, tx, identity); err != nil {
			// The row is terminal but its cleanup evidence is malformed or
			// mismatched. Keep the identity quarantined for explicit recovery;
			// migration must not turn an invalid receipt into a tombstone.
			return workerIdentityMigrationBlockedWork{Kind: "cleanup", ID: terminalPublications[0].ID, Status: "invalid"}, true, nil
		}
	}
	return workerIdentityMigrationBlockedWork{}, false, nil
}

// migrateLegacyWorkerIdentityBatch closes a verifier-less legacy identity when
// its active attempts have been exhausted, and otherwise durably requeues one
// bounded batch. Each invocation is one WAL transaction; callers repeat it
// until complete so large installations do not turn a safety bound into a
// permanent startup availability limit. The receipt contains no credential.
func (l *agentTaskDeliveryLedger) migrateLegacyWorkerIdentityBatch(ctx context.Context, tx *sql.Tx, identity agentWorkerIdentityRecord) (bool, error) {
	sourceDigest := workerIdentityCredentialMigrationSourceDigest(identity)
	var existingReceipt string
	lookupErr := tx.QueryRowContext(ctx, `SELECT receipt_id FROM task_ledger_migration_receipts WHERE source_digest=? AND phase=?`, sourceDigest, workerIdentityCredentialMigrationPhase).Scan(&existingReceipt)
	if lookupErr == nil {
		return true, nil
	}
	if !errors.Is(lookupErr, sql.ErrNoRows) {
		return false, lookupErr
	}
	receiptID := workerIdentityCredentialMigrationReceiptID(sourceDigest)
	now := agentTaskNow()
	type migratedAttempt struct {
		AttemptID, TaskID, LeaseID, WorkerID, WorkerInstanceID, AttemptStatus, TaskStatus, ObservationDigest string
		ApprovalPolicyJSON, ContextRequestJSON                                                               string
		AttemptNumber, Generation, IdentityGeneration, ClaimEligible, Approved                               int
	}
	attemptBinding := workerIdentityAttemptBindingPredicate("a")
	attemptArgs := append([]any{identity.WorkerInstanceID, identity.WorkspaceID}, workerIdentityAttemptBindingArgs(identity)...)
	attemptArgs = append(attemptArgs, workerIdentityCredentialMigrationBatchSize+1)
	rows, err := tx.QueryContext(ctx, `SELECT a.attempt_id,a.task_id,a.attempt_number,a.lease_id,a.generation,a.worker_id,a.worker_instance_id,a.worker_identity_update_generation,a.status,t.status,t.claim_eligible,t.approval_policy_json,t.context_request_json,t.approved FROM task_ledger_attempts a JOIN task_ledger_tasks t ON t.id=a.task_id WHERE a.worker_instance_id=? AND lower(trim(t.workspace_id))=lower(?) AND `+attemptBinding+` AND a.status IN ('leased','running','waiting_for_input') ORDER BY a.task_id ASC,a.attempt_id ASC LIMIT ?`, attemptArgs...)
	if err != nil {
		return false, err
	}
	migrated := make([]migratedAttempt, 0)
	for rows.Next() {
		var item migratedAttempt
		if err := rows.Scan(&item.AttemptID, &item.TaskID, &item.AttemptNumber, &item.LeaseID, &item.Generation, &item.WorkerID, &item.WorkerInstanceID, &item.IdentityGeneration, &item.AttemptStatus, &item.TaskStatus, &item.ClaimEligible, &item.ApprovalPolicyJSON, &item.ContextRequestJSON, &item.Approved); err != nil {
			rows.Close()
			return false, err
		}
		migrated = append(migrated, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	more := len(migrated) > workerIdentityCredentialMigrationBatchSize
	if more {
		migrated = migrated[:workerIdentityCredentialMigrationBatchSize]
	}
	for index := range migrated {
		item := &migrated[index]
		if !agentTaskAllowedTransition(item.TaskStatus, "execution_failed") || !agentTaskAllowedTransition("execution_failed", "queued") {
			return false, fmt.Errorf("legacy worker identity migration cannot requeue task %s from status %s", item.TaskID, item.TaskStatus)
		}
		observation := map[string]any{
			"task_id": item.TaskID, "attempt_id": item.AttemptID, "lease_id": item.LeaseID,
			"worker_id": item.WorkerID, "worker_instance_id": item.WorkerInstanceID,
			"generation": item.Generation, "worker_identity_update_generation": item.IdentityGeneration,
			"reason": "legacy_worker_identity_credential_migration",
		}
		item.ObservationDigest = agentTaskDigest(observation)
		item.ClaimEligible = workerIdentityMigrationClaimEligibility(item.ApprovalPolicyJSON, item.ContextRequestJSON, item.Approved)
		// A renamed identity cannot safely inherit a queued hint that still
		// names the requested worker: another active identity may own that
		// requested canonical. Disable the hint durably until an explicit
		// canonical handoff/update receipt rebinds it.
		if !strings.EqualFold(identity.CanonicalWorkerID, identity.RequestedWorkerID) {
			item.ClaimEligible = 0
		}
	}
	for _, item := range migrated {
		attemptUpdated, err := tx.ExecContext(ctx, `UPDATE task_ledger_attempts SET status='execution_failed',runner_status='worker_identity_credential_migration',runner_exit_code=125,runner_exit_observed=1,observation_digest=?,failure_disposition='credential_migration_requeued',completed_at=? WHERE attempt_id=? AND status IN ('leased','running','waiting_for_input')`, item.ObservationDigest, now, item.AttemptID)
		if err != nil {
			return false, err
		}
		if affected, err := attemptUpdated.RowsAffected(); err != nil || affected != 1 {
			if err != nil {
				return false, err
			}
			return false, fmt.Errorf("legacy worker identity migration lost active attempt %s", item.AttemptID)
		}
		updated, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET status='execution_failed',claim_eligible=0,updated_at=? WHERE id=? AND active_attempt_id=? AND status=?`, now, item.TaskID, item.AttemptID, item.TaskStatus)
		if err != nil {
			return false, err
		}
		if affected, err := updated.RowsAffected(); err != nil || affected != 1 {
			return false, fmt.Errorf("legacy worker identity migration lost active task %s", item.TaskID)
		}
		if err := l.appendEventTx(ctx, tx, item.TaskID, item.AttemptID, "execution_failed", "legacy worker identity credential migration expired the active attempt", map[string]any{
			"migration_receipt_id": receiptID, "previous_status": item.TaskStatus, "attempt_status": item.AttemptStatus,
			"failure_disposition": "credential_migration_requeued", "observation_digest": item.ObservationDigest,
		}); err != nil {
			return false, err
		}
		requeued, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET status='queued',claim_eligible=?,active_attempt_id='',updated_at=? WHERE id=? AND active_attempt_id=? AND status='execution_failed'`, item.ClaimEligible, now, item.TaskID, item.AttemptID)
		if err != nil {
			return false, err
		}
		if affected, err := requeued.RowsAffected(); err != nil || affected != 1 {
			if err != nil {
				return false, err
			}
			return false, fmt.Errorf("legacy worker identity migration lost queued task %s", item.TaskID)
		}
		if !strings.EqualFold(identity.CanonicalWorkerID, identity.RequestedWorkerID) {
			if err := bindWorkerIdentityTaskTx(ctx, tx, item.TaskID, identity, item.WorkerID, item.IdentityGeneration); err != nil {
				return false, fmt.Errorf("legacy worker identity migration could not preserve exact queued binding for task %s: %w", item.TaskID, err)
			}
		} else {
			// A same-ID legacy handoff has no collision alias to preserve. Release
			// any exact old binding with durable migration evidence so a fresh
			// credential-bound registration can claim the requeued task without
			// broadening a renamed/colliding identity's authority.
			handoffDigest := agentTaskDigest(map[string]any{
				"migration_receipt_id": receiptID, "task_id": item.TaskID,
				"identity_id": identity.IdentityID, "principal_id": identity.PrincipalID,
				"workspace_id": identity.WorkspaceID, "worker_instance_id": identity.WorkerInstanceID,
				"worker_identity_update_generation": item.IdentityGeneration,
			})
			if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_worker_task_bindings SET state='released',rebind_update_id=?,rebind_receipt_digest=?,rebind_acknowledged_at=?,updated_at=? WHERE task_id=? AND identity_id=? AND principal_id=? AND lower(trim(workspace_id))=lower(?) AND worker_instance_id=? AND worker_identity_update_generation=? AND lower(trim(worker_id))=lower(?) AND state IN ('bound','rebind_pending')`, receiptID, handoffDigest, now, now, item.TaskID, identity.IdentityID, identity.PrincipalID, identity.WorkspaceID, identity.WorkerInstanceID, item.IdentityGeneration, item.WorkerID); err != nil {
				return false, err
			}
		}
		if err := l.appendEventTx(ctx, tx, item.TaskID, item.AttemptID, "queued", "legacy worker identity credential migration requeued the task", map[string]any{
			"migration_receipt_id": receiptID, "attempt_number": item.AttemptNumber,
			"next_generation": item.Generation + 1, "observation_digest": item.ObservationDigest,
			"claim_eligible":        item.ClaimEligible != 0,
			"claim_rebind_required": !strings.EqualFold(identity.CanonicalWorkerID, identity.RequestedWorkerID),
		}); err != nil {
			return false, err
		}
	}
	if more {
		return false, nil
	}
	disabledClaims, moreDisabledClaims, err := l.disableLegacyWorkerIdentityQueuedClaims(ctx, tx, identity, receiptID)
	if err != nil {
		return false, err
	}
	if moreDisabledClaims {
		return false, nil
	}
	blockedWork, blocked, blockedErr := workerIdentityMigrationBoundWork(ctx, tx, identity)
	if blockedErr != nil {
		return false, blockedErr
	}
	closedStatus := "closed"
	if blocked {
		closedStatus = "quarantined"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_worker_identities SET status=?,updated_at=?,closed_at=? WHERE identity_id=? AND status='active' AND worker_instance_credential_verifier=''`, closedStatus, now, now, identity.IdentityID); err != nil {
		return false, err
	}
	attemptEvidence := make([]map[string]any, 0, len(migrated))
	for _, item := range migrated {
		attemptEvidence = append(attemptEvidence, map[string]any{
			"attempt_id": item.AttemptID, "task_id": item.TaskID, "attempt_number": item.AttemptNumber,
			"generation": item.Generation, "worker_id": item.WorkerID, "worker_instance_id": item.WorkerInstanceID,
			"previous_attempt_status": item.AttemptStatus, "previous_task_status": item.TaskStatus,
			"observation_digest": item.ObservationDigest, "requeued": true,
		})
	}
	details := map[string]any{
		"schema_id": "worker_identity_credential_migration.v1", "phase": workerIdentityCredentialMigrationPhase,
		"identity_id": identity.IdentityID, "principal_id": identity.PrincipalID, "workspace_id": identity.WorkspaceID,
		"requested_worker_id": identity.RequestedWorkerID, "canonical_worker_id": identity.CanonicalWorkerID,
		"worker_instance_id": identity.WorkerInstanceID, "identity_digest": identity.IdentityDigest,
		"closed_status": closedStatus, "quarantined": blocked, "requeued_attempts": attemptEvidence,
	}
	disabledClaimEvidence := make([]map[string]any, 0, len(disabledClaims))
	for _, item := range disabledClaims {
		disabledClaimEvidence = append(disabledClaimEvidence, map[string]any{
			"task_id": item.TaskID, "previous_claim_worker_id": item.ClaimWorkerID,
			"previous_status": item.PreviousStatus, "previous_claim_eligible": item.ClaimEligible != 0,
			"claim_eligible": false, "claim_rebind_required": true,
			"identity_id": identity.IdentityID, "principal_id": identity.PrincipalID,
			"workspace_id": identity.WorkspaceID, "worker_instance_id": identity.WorkerInstanceID,
		})
	}
	details["disabled_claims"] = disabledClaimEvidence
	if blocked {
		details["blocked_work"] = map[string]any{"kind": blockedWork.Kind, "id": blockedWork.ID, "status": blockedWork.Status}
		details["recovery_required"] = "reconcile proof-bound lifecycle work before credential-bound registration"
	}
	details["details_digest"] = agentTaskDigest(details)
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_ledger_migration_receipts(receipt_id,source_path,source_digest,phase,imported,validated,frozen,rolled_back,details_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, receiptID, "worker-identity:"+identity.IdentityID, sourceDigest, workerIdentityCredentialMigrationPhase, 1, 1, 1, 0, encodeAgentTaskJSON(details), now); err != nil {
		if workerIdentityConstraintCollision(err) {
			return true, nil
		}
		return false, err
	}
	return true, nil
}

// migrateLegacyWorkerIdentity processes every bounded batch with a separate
// commit. This is used by the lazy registration path so a large legacy row is
// fully retired before the caller is told to rotate, while preserving durable
// progress if the process stops between batches.
func (l *agentTaskDeliveryLedger) migrateLegacyWorkerIdentity(ctx context.Context, identity agentWorkerIdentityRecord) error {
	for {
		tx, err := l.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		complete, batchErr := l.migrateLegacyWorkerIdentityBatch(ctx, tx, identity)
		if batchErr != nil {
			_ = tx.Rollback()
			return batchErr
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if complete {
			return nil
		}
	}
}

// migrateLegacyWorkerIdentitiesAtStartup is the broad safety net for rows
// written before credential binding existed. It runs after schema migration,
// before the Gateway serves routes, so a legacy worker that never returns
// cannot keep an active lease indefinitely. Registration keeps the same
// idempotent lazy path for rows created by an older handle after startup.
func (l *agentTaskDeliveryLedger) migrateLegacyWorkerIdentitiesAtStartup(ctx context.Context) error {
	for {
		tx, err := l.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+workerIdentitySelectColumns+` FROM task_ledger_worker_identities WHERE status='active' AND worker_instance_credential_verifier='' ORDER BY identity_id LIMIT 1`)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		if !rows.Next() {
			rowsErr := rows.Err()
			closeErr := rows.Close()
			if rowsErr != nil {
				_ = tx.Rollback()
				return rowsErr
			}
			if closeErr != nil {
				_ = tx.Rollback()
				return closeErr
			}
			return tx.Commit()
		}
		identity, scanErr := scanWorkerIdentity(rows)
		closeErr := rows.Close()
		if scanErr != nil {
			_ = tx.Rollback()
			return scanErr
		}
		if closeErr != nil {
			_ = tx.Rollback()
			return closeErr
		}
		if _, batchErr := l.migrateLegacyWorkerIdentityBatch(ctx, tx, identity); batchErr != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migrate legacy worker identity %s: %w", identity.IdentityID, batchErr)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
}

// registerWorkerIdentity registers a new worker instance or performs an
// authenticated idempotent replay. The variadic credential preserves the
// trusted in-process ledger API used by migration tests; every HTTP route
// passes one explicit value (including an empty value) and therefore receives
// the proof-of-possession boundary below.
func (l *agentTaskDeliveryLedger) registerWorkerIdentity(ctx context.Context, principal, workspace, requested, instance string, credentials ...string) (map[string]any, error) {
	authority, err := normalizeWorkerIdentityAuthority(principal, workspace, instance)
	if err != nil {
		return nil, err
	}
	requested, err = normalizeWorkerIdentityID(requested, "requested_worker_id")
	if err != nil {
		return nil, err
	}
	routeCredentialBoundary := len(credentials) > 0
	providedCredential := ""
	if routeCredentialBoundary {
		providedCredential = credentials[0]
		if len([]byte(providedCredential)) > workerInstanceCredentialMaxBytes {
			return nil, errors.New("worker instance credential exceeds the bounded ingress size")
		}
		if err := validateWorkerInstanceCredential(providedCredential); err != nil {
			return nil, err
		}
	}
	for retry := 0; retry < agentTaskIDCollisionRetries; retry++ {
		tx, txErr := l.db.BeginTx(ctx, nil)
		if txErr != nil {
			return nil, txErr
		}
		if authorityErr := validateWorkerIdentityInstanceAuthority(ctx, tx, authority, requested); authorityErr != nil {
			_ = tx.Rollback()
			return nil, authorityErr
		}
		identity, lookupErr := scanWorkerIdentity(tx.QueryRowContext(ctx, `SELECT `+workerIdentitySelectColumns+` FROM task_ledger_worker_identities WHERE workspace_id=? AND worker_instance_id=?`, authority.WorkspaceID, authority.WorkerInstanceID))
		if lookupErr == nil {
			if identity.WorkerInstanceCredentialVerifier == "" {
				if identity.Status == "closed" {
					var migratedReceipt string
					migrationErr := tx.QueryRowContext(ctx, `SELECT receipt_id FROM task_ledger_migration_receipts WHERE source_digest=? AND phase=?`, workerIdentityCredentialMigrationSourceDigest(identity), workerIdentityCredentialMigrationPhase).Scan(&migratedReceipt)
					_ = tx.Rollback()
					if migrationErr == nil {
						return nil, errWorkerIdentityLegacyCredentialMigration
					}
					if !errors.Is(migrationErr, sql.ErrNoRows) {
						return nil, migrationErr
					}
					return nil, errors.New("worker identity is closed and cannot be rebound")
				}
				_ = tx.Rollback()
				if migrationErr := l.migrateLegacyWorkerIdentity(ctx, identity); migrationErr != nil {
					return nil, migrationErr
				}
				return nil, errWorkerIdentityLegacyCredentialMigration
			}
			upgradedInTx := false
			if routeCredentialBoundary && identity.WorkerInstanceCredentialVerifier != "" {
				if credentialErr := l.verifyAndUpgradeWorkerInstanceCredentialTx(ctx, tx, &identity, providedCredential); credentialErr != nil {
					_ = tx.Rollback()
					return nil, credentialErr
				}
				upgradedInTx = true
			} else if identity.WorkerInstanceCredentialVerifier != "" {
				_ = tx.Rollback()
			}
			if identity.Status != "active" {
				_ = tx.Rollback()
				return nil, errors.New("worker identity is closed and cannot be rebound")
			}
			if identity.PrincipalID != authority.PrincipalID || identity.WorkspaceID != authority.WorkspaceID || identity.RequestedWorkerID != requested {
				_ = tx.Rollback()
				return nil, errors.New("worker instance is bound to a different principal, workspace, or requested worker ID")
			}
			if upgradedInTx {
				if commitErr := tx.Commit(); commitErr != nil {
					return nil, commitErr
				}
			}
			response, responseErr := l.workerIdentityResponse(ctx, identity)
			if responseErr == nil {
				response["idempotent_replay"] = true
			}
			return response, responseErr
		}
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			_ = tx.Rollback()
			return nil, lookupErr
		}
		var caseVariantWorkspace string
		caseVariantErr := tx.QueryRowContext(ctx, `SELECT workspace_id FROM task_ledger_worker_identities WHERE lower(workspace_id)=lower(?) AND workspace_id<>? ORDER BY identity_id LIMIT 1`, authority.WorkspaceID, authority.WorkspaceID).Scan(&caseVariantWorkspace)
		if caseVariantErr == nil {
			_ = tx.Rollback()
			return nil, errors.New("worker identity workspace has an unresolved case-variant migration conflict")
		}
		if !errors.Is(caseVariantErr, sql.ErrNoRows) {
			_ = tx.Rollback()
			return nil, caseVariantErr
		}

		canonical := requested
		for canonicalRetry := 0; canonicalRetry < agentTaskIDCollisionRetries; canonicalRetry++ {
			var occupied string
			canonicalErr := tx.QueryRowContext(ctx, `SELECT identity_id FROM task_ledger_worker_identities WHERE workspace_id=? AND canonical_worker_id=?`, authority.WorkspaceID, canonical).Scan(&occupied)
			if errors.Is(canonicalErr, sql.ErrNoRows) {
				break
			}
			if canonicalErr != nil {
				_ = tx.Rollback()
				return nil, canonicalErr
			}
			canonical = boundedCanonicalWorkerID(requested, authority.WorkerInstanceID, canonicalRetry)
		}
		var occupied string
		if canonicalErr := tx.QueryRowContext(ctx, `SELECT identity_id FROM task_ledger_worker_identities WHERE workspace_id=? AND canonical_worker_id=?`, authority.WorkspaceID, canonical).Scan(&occupied); canonicalErr == nil {
			_ = tx.Rollback()
			continue
		} else if !errors.Is(canonicalErr, sql.ErrNoRows) {
			_ = tx.Rollback()
			return nil, canonicalErr
		}
		identityID, idErr := l.newWorkerIdentityID(ctx, tx, "worker-identity", "worker-identity")
		if idErr != nil {
			_ = tx.Rollback()
			return nil, idErr
		}
		generation := 0
		if canonical != requested {
			generation = 1
		}
		credentialGeneration := workerInstanceCredentialGenerationInitial
		credential := providedCredential
		if !routeCredentialBoundary {
			var credentialErr error
			credential, credentialErr = newWorkerInstanceCredential()
			if credentialErr != nil {
				_ = tx.Rollback()
				return nil, credentialErr
			}
		}
		now := agentTaskNow()
		identity = agentWorkerIdentityRecord{
			IdentityID: identityID, PrincipalID: authority.PrincipalID, WorkspaceID: authority.WorkspaceID,
			RequestedWorkerID: requested, CanonicalWorkerID: canonical, WorkerInstanceID: authority.WorkerInstanceID,
			WorkerInstanceCredentialVerifier:   workerInstanceCredentialVerifier(credential, authority, credentialGeneration, generation),
			WorkerInstanceCredentialGeneration: credentialGeneration,
			IdentityUpdateGeneration:           generation, AcknowledgedGeneration: 0,
			RequestedIDDigest: workerIdentityRequestedDigest(requested), Status: "active", CreatedAt: now, UpdatedAt: now,
		}
		identity.IdentityDigest = workerIdentityRecordDigest(identity)
		_, insertErr := tx.ExecContext(ctx, `INSERT INTO task_ledger_worker_identities(identity_id,principal_id,workspace_id,requested_worker_id,canonical_worker_id,worker_instance_id,worker_instance_credential_verifier,worker_instance_credential_generation,worker_identity_update_generation,acknowledged_generation,requested_id_digest,identity_digest,status,created_at,updated_at,closed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, identity.IdentityID, identity.PrincipalID, identity.WorkspaceID, identity.RequestedWorkerID, identity.CanonicalWorkerID, identity.WorkerInstanceID, identity.WorkerInstanceCredentialVerifier, identity.WorkerInstanceCredentialGeneration, identity.IdentityUpdateGeneration, identity.AcknowledgedGeneration, identity.RequestedIDDigest, identity.IdentityDigest, identity.Status, identity.CreatedAt, identity.UpdatedAt, identity.ClosedAt)
		if insertErr != nil {
			_ = tx.Rollback()
			if workerIdentityConstraintCollision(insertErr) {
				continue
			}
			return nil, insertErr
		}
		if canonical != requested {
			update := agentWorkerIdentityUpdateRecord{
				IdentityID: identity.IdentityID, PrincipalID: identity.PrincipalID, WorkspaceID: identity.WorkspaceID,
				WorkerInstanceID: identity.WorkerInstanceID, OldWorkerID: requested, RequestedWorkerID: requested,
				NewWorkerID: canonical, CanonicalWorkerID: canonical, IdentityUpdateGeneration: generation,
				State: agentWorkerIdentityStatePending, CreatedAt: now, UpdatedAt: now,
			}
			update.UpdateID, idErr = l.newWorkerIdentityID(ctx, tx, "worker-identity-update", "worker-identity-update")
			if idErr != nil {
				_ = tx.Rollback()
				return nil, idErr
			}
			update.UpdateDigest = workerIdentityUpdateDigest(update)
			update.ReceiptDigest = workerIdentityReceiptDigest(update)
			if _, insertErr = tx.ExecContext(ctx, `INSERT INTO task_ledger_worker_identity_updates(update_id,identity_id,principal_id,workspace_id,worker_instance_id,old_worker_id,requested_worker_id,new_worker_id,canonical_worker_id,worker_identity_update_generation,update_digest,receipt_digest,state,delivery_attempts,last_error,created_at,updated_at,delivered_at,acknowledged_at,ack_receipt_digest,ack_receipt_payload_json,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, update.UpdateID, update.IdentityID, update.PrincipalID, update.WorkspaceID, update.WorkerInstanceID, update.OldWorkerID, update.RequestedWorkerID, update.NewWorkerID, update.CanonicalWorkerID, update.IdentityUpdateGeneration, update.UpdateDigest, update.ReceiptDigest, update.State, 0, "", update.CreatedAt, update.UpdatedAt, "", "", "", "", ""); insertErr != nil {
				_ = tx.Rollback()
				if workerIdentityConstraintCollision(insertErr) {
					continue
				}
				return nil, insertErr
			}
		}
		if commitErr := tx.Commit(); commitErr != nil {
			if workerIdentityConstraintCollision(commitErr) {
				continue
			}
			return nil, commitErr
		}
		response, responseErr := l.workerIdentityResponse(ctx, identity)
		if responseErr == nil {
			response["idempotent_replay"] = false
		}
		return response, responseErr
	}
	return nil, fmt.Errorf("worker identity registration collision retry budget exhausted")
}

func workerIdentityRetirementDigest(identity agentWorkerIdentityRecord) string {
	return agentTaskDigest(map[string]any{
		"identity_id": identity.IdentityID, "principal_id": identity.PrincipalID, "workspace_id": identity.WorkspaceID,
		"requested_worker_id": identity.RequestedWorkerID, "canonical_worker_id": identity.CanonicalWorkerID,
		"worker_instance_id":                identity.WorkerInstanceID,
		"worker_identity_update_generation": identity.IdentityUpdateGeneration,
		"acknowledged_generation":           identity.AcknowledgedGeneration,
		"identity_digest":                   identity.IdentityDigest,
		"retired":                           true,
	})
}

func workerIdentityRetirementReceiptDigest(receipt map[string]any) string {
	return agentTaskDigest(map[string]any{
		"retirement_id": receipt["retirement_id"], "identity_id": receipt["identity_id"],
		"principal_id": receipt["principal_id"], "workspace_id": receipt["workspace_id"],
		"requested_worker_id": receipt["requested_worker_id"], "canonical_worker_id": receipt["canonical_worker_id"],
		"tombstone_canonical_worker_id":     receipt["tombstone_canonical_worker_id"],
		"worker_instance_id":                receipt["worker_instance_id"],
		"worker_identity_update_generation": receipt["worker_identity_update_generation"],
		"acknowledged_generation":           receipt["acknowledged_generation"],
		"identity_digest":                   receipt["identity_digest"],
		"closed_identity_digest":            receipt["closed_identity_digest"],
		"closed_status":                     receipt["closed_status"],
		"retirement_digest":                 receipt["retirement_digest"],
		"closed_at":                         receipt["closed_at"],
		"retired":                           true,
		"canonical_reclaimed":               true,
	})
}

func workerIdentityRetirementRequestMatches(receipt, request map[string]any) bool {
	for _, field := range []string{
		"identity_id", "principal_id", "workspace_id", "requested_worker_id", "canonical_worker_id", "worker_instance_id",
		"worker_identity_update_generation", "acknowledged_generation", "identity_digest", "retirement_digest",
	} {
		if !agentTaskCanonicalMapEqual(map[string]any{"value": receipt[field]}, map[string]any{"value": request[field]}) {
			return false
		}
	}
	retired, ok := request["retired"].(bool)
	return ok && retired && receipt["retired"] == true
}

func workerIdentityTombstoneCanonical(identityID string, retry int) string {
	hash := sha256.Sum256([]byte(strings.TrimSpace(identityID)))
	tombstone := "closed-" + hex.EncodeToString(hash[:12])
	if retry > 0 {
		tombstone += fmt.Sprintf("-%x", retry)
	}
	return tombstone
}

func (l *agentTaskDeliveryLedger) retireWorkerIdentity(ctx context.Context, payload map[string]any, authority agentWorkerIdentityAuthority) (map[string]any, error) {
	if payload == nil {
		return nil, errors.New("worker identity retirement is required")
	}
	if findings := validateAgentContractPayload(agentWorkerIdentityRetireContractID, payload); len(findings) != 0 {
		return nil, errors.New("worker identity retirement contract is invalid")
	}
	authority, err := normalizeWorkerIdentityAuthority(authority.PrincipalID, authority.WorkspaceID, authority.WorkerInstanceID)
	if err != nil {
		return nil, err
	}
	identityID, ok := payload["identity_id"].(string)
	if !ok || strings.TrimSpace(identityID) == "" {
		return nil, errors.New("worker identity retirement identity_id is required")
	}
	requested, err := normalizeWorkerIdentityID(anyToString(payload["requested_worker_id"]), "requested_worker_id")
	if err != nil {
		return nil, err
	}
	canonical, err := normalizeWorkerIdentityID(anyToString(payload["canonical_worker_id"]), "canonical_worker_id")
	if err != nil {
		return nil, err
	}
	generation, generationValid := strictWorkerIdentityInteger(payload["worker_identity_update_generation"])
	acknowledgedGeneration, acknowledgedValid := strictWorkerIdentityInteger(payload["acknowledged_generation"])
	if !generationValid || !acknowledgedValid || generation < 0 || acknowledgedGeneration < 0 || acknowledgedGeneration != generation {
		return nil, errors.New("worker identity retirement generation is invalid")
	}
	identityDigest, ok := payload["identity_digest"].(string)
	if !ok || !agentTaskCanonicalSHA256(identityDigest) {
		return nil, errors.New("worker identity retirement identity digest is invalid")
	}
	retirementDigest, ok := payload["retirement_digest"].(string)
	if !ok || !agentTaskCanonicalSHA256(retirementDigest) {
		return nil, errors.New("worker identity retirement digest is invalid")
	}
	if payload["retired"] != true {
		return nil, errors.New("worker identity retirement must assert retired=true")
	}
	request := map[string]any{
		"identity_id": identityID, "principal_id": authority.PrincipalID, "workspace_id": authority.WorkspaceID,
		"requested_worker_id": requested, "canonical_worker_id": canonical, "worker_instance_id": authority.WorkerInstanceID,
		"worker_identity_update_generation": generation, "acknowledged_generation": acknowledgedGeneration,
		"identity_digest": identityDigest, "retirement_digest": retirementDigest, "retired": true,
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	identity, err := scanWorkerIdentity(tx.QueryRowContext(ctx, `SELECT `+workerIdentitySelectColumns+` FROM task_ledger_worker_identities WHERE identity_id=?`, identityID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("worker identity retirement identity is unknown")
	}
	if err != nil {
		return nil, err
	}
	if identity.Status == "closed" {
		var storedJSON string
		if err := tx.QueryRowContext(ctx, `SELECT payload_json FROM task_ledger_worker_identity_retirements WHERE identity_id=?`, identityID).Scan(&storedJSON); err != nil {
			return nil, errors.New("worker identity retirement proof is unavailable")
		}
		stored := map[string]any{}
		if err := json.Unmarshal([]byte(storedJSON), &stored); err != nil || !workerIdentityRetirementRequestMatches(stored, request) {
			return nil, errors.New("worker identity retirement replay does not match the exact closed receipt")
		}
		if findings := validateAgentContractPayload(agentWorkerIdentityRetirementReceiptID, stored); len(findings) != 0 ||
			anyToString(stored["closed_status"]) != "closed" ||
			anyToString(stored["tombstone_canonical_worker_id"]) != identity.CanonicalWorkerID ||
			anyToString(stored["closed_identity_digest"]) == "" ||
			anyToString(stored["closed_identity_digest"]) != identity.IdentityDigest ||
			identity.Status != anyToString(stored["closed_status"]) ||
			anyToString(stored["retirement_receipt_digest"]) != workerIdentityRetirementReceiptDigest(stored) {
			return nil, errors.New("worker identity retirement replay is not bound to the durable closed tombstone")
		}
		stored["idempotent_replay"] = true
		return agentTaskContractPayload(agentWorkerIdentityRetirementReceiptID, stored), nil
	}
	if identity.Status != "active" {
		return nil, errors.New("worker identity is not active")
	}
	if identity.PrincipalID != authority.PrincipalID || identity.WorkspaceID != authority.WorkspaceID || identity.WorkerInstanceID != authority.WorkerInstanceID || identity.RequestedWorkerID != requested || identity.CanonicalWorkerID != canonical || identity.IdentityUpdateGeneration != generation || identity.AcknowledgedGeneration != acknowledgedGeneration || identity.IdentityDigest != identityDigest {
		return nil, errors.New("worker identity retirement does not bind the exact authority")
	}
	if identity.IdentityDigest != workerIdentityRecordDigest(identity) || retirementDigest != workerIdentityRetirementDigest(identity) {
		return nil, errors.New("worker identity retirement digest does not match the durable identity")
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_ledger_worker_identity_updates WHERE identity_id=? AND state<>?`, identity.IdentityID, agentWorkerIdentityStateAcknowledged).Scan(&pending); err != nil {
		return nil, err
	}
	if pending != 0 {
		return nil, errors.New("worker identity retirement requires an acknowledged update")
	}
	if err := workerIdentityRetirementBoundWork(ctx, tx, identity); err != nil {
		return nil, err
	}
	retirementID, err := l.newUniqueID(ctx, tx, "worker-identity-retirement", "worker-identity-retirement")
	if err != nil {
		return nil, err
	}
	closedAt := agentTaskNow()
	tombstone := ""
	for retry := 0; retry < agentTaskIDCollisionRetries; retry++ {
		candidate := workerIdentityTombstoneCanonical(identity.IdentityID, retry)
		var occupied string
		occupiedErr := tx.QueryRowContext(ctx, `SELECT identity_id FROM task_ledger_worker_identities WHERE workspace_id=? AND canonical_worker_id=?`, identity.WorkspaceID, candidate).Scan(&occupied)
		if errors.Is(occupiedErr, sql.ErrNoRows) {
			tombstone = candidate
			break
		}
		if occupiedErr != nil {
			return nil, occupiedErr
		}
	}
	if tombstone == "" {
		return nil, errors.New("worker identity retirement tombstone collision retry budget exhausted")
	}
	closedIdentity := identity
	closedIdentity.Status = "closed"
	closedIdentity.CanonicalWorkerID = tombstone
	closedIdentity.UpdatedAt = closedAt
	closedIdentity.ClosedAt = closedAt
	closedIdentity.IdentityDigest = workerIdentityRecordDigest(closedIdentity)
	updateResult, err := tx.ExecContext(ctx, `UPDATE task_ledger_worker_identities SET canonical_worker_id=?,identity_digest=?,status='closed',updated_at=?,closed_at=? WHERE identity_id=? AND status='active' AND principal_id=? AND workspace_id=? AND worker_instance_id=? AND canonical_worker_id=? AND identity_digest=? AND worker_identity_update_generation=? AND acknowledged_generation=?`, tombstone, closedIdentity.IdentityDigest, closedAt, closedAt, identity.IdentityID, identity.PrincipalID, identity.WorkspaceID, identity.WorkerInstanceID, identity.CanonicalWorkerID, identity.IdentityDigest, identity.IdentityUpdateGeneration, identity.AcknowledgedGeneration)
	if err != nil {
		return nil, err
	}
	if affected, err := updateResult.RowsAffected(); err != nil || affected != 1 {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("worker identity retirement active-to-closed CAS lost")
	}
	finalIdentity, err := scanWorkerIdentity(tx.QueryRowContext(ctx, `SELECT `+workerIdentitySelectColumns+` FROM task_ledger_worker_identities WHERE identity_id=?`, identity.IdentityID))
	if err != nil {
		return nil, err
	}
	if finalIdentity.Status != "closed" || finalIdentity.CanonicalWorkerID != tombstone || finalIdentity.ClosedAt != closedAt || finalIdentity.IdentityDigest != closedIdentity.IdentityDigest || finalIdentity.IdentityDigest != workerIdentityRecordDigest(finalIdentity) {
		return nil, errors.New("worker identity retirement closed tombstone readback is not exact")
	}
	receipt := map[string]any{
		"schema_id": agentWorkerIdentityRetirementReceiptID, "contract_version": 1,
		"retirement_id": retirementID, "identity_id": identity.IdentityID,
		"principal_id": identity.PrincipalID, "workspace_id": identity.WorkspaceID,
		"requested_worker_id": identity.RequestedWorkerID, "canonical_worker_id": identity.CanonicalWorkerID,
		"tombstone_canonical_worker_id": tombstone, "worker_instance_id": identity.WorkerInstanceID,
		"worker_identity_update_generation": identity.IdentityUpdateGeneration, "acknowledged_generation": identity.AcknowledgedGeneration,
		"identity_digest": identity.IdentityDigest, "closed_identity_digest": finalIdentity.IdentityDigest, "closed_status": finalIdentity.Status,
		"retirement_digest": retirementDigest,
		"retired":           true, "canonical_reclaimed": true, "closed_at": closedAt, "idempotent_replay": false,
	}
	receipt["retirement_receipt_digest"] = workerIdentityRetirementReceiptDigest(receipt)
	receipt = agentTaskContractPayload(agentWorkerIdentityRetirementReceiptID, receipt)
	receiptJSON := encodeAgentTaskJSON(receipt)
	if receiptJSON == "{}" {
		return nil, errors.New("worker identity retirement receipt is not serializable")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_ledger_worker_identity_retirements(retirement_id,identity_id,principal_id,workspace_id,requested_worker_id,canonical_worker_id,tombstone_canonical_worker_id,worker_instance_id,worker_identity_update_generation,acknowledged_generation,identity_digest,closed_identity_digest,closed_status,retirement_digest,retirement_receipt_digest,closed_at,payload_json) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, retirementID, identity.IdentityID, identity.PrincipalID, identity.WorkspaceID, identity.RequestedWorkerID, identity.CanonicalWorkerID, tombstone, identity.WorkerInstanceID, identity.IdentityUpdateGeneration, identity.AcknowledgedGeneration, identity.IdentityDigest, finalIdentity.IdentityDigest, finalIdentity.Status, retirementDigest, anyToString(receipt["retirement_receipt_digest"]), closedAt, receiptJSON); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return receipt, nil
}

// readWorkerIdentityRetirement is the server-authoritative recovery surface
// for a client that lost its local save after the retirement transaction
// committed. It accepts only the original identity and retirement digests,
// binds the lookup to the exact principal/workspace/instance authority, and
// returns the immutable closed receipt without reopening or rebinding it.
func (l *agentTaskDeliveryLedger) readWorkerIdentityRetirement(ctx context.Context, payload map[string]any, authority agentWorkerIdentityAuthority) (map[string]any, error) {
	if payload == nil {
		return nil, errors.New("worker identity retirement readback is required")
	}
	authority, err := normalizeWorkerIdentityAuthority(authority.PrincipalID, authority.WorkspaceID, authority.WorkerInstanceID)
	if err != nil {
		return nil, err
	}
	identityID := strings.TrimSpace(anyToString(payload["identity_id"]))
	identityDigest := strings.TrimSpace(anyToString(payload["identity_digest"]))
	retirementDigest := strings.TrimSpace(anyToString(payload["retirement_digest"]))
	if identityID == "" || !agentTaskCanonicalSHA256(identityDigest) || !agentTaskCanonicalSHA256(retirementDigest) {
		return nil, errors.New("worker identity retirement readback requires exact identity and retirement digests")
	}
	optionalIDFields := []string{"requested_worker_id", "canonical_worker_id", "worker_instance_id"}
	for _, field := range optionalIDFields {
		if raw, present := payload[field]; present && strings.TrimSpace(anyToString(raw)) == "" {
			return nil, fmt.Errorf("worker identity retirement readback %s is invalid", field)
		}
	}
	for _, field := range []string{"requested_worker_id", "canonical_worker_id"} {
		if raw, present := payload[field]; present {
			if _, err := normalizeWorkerIdentityID(anyToString(raw), field); err != nil {
				return nil, err
			}
		}
	}
	if raw, present := payload["worker_identity_update_generation"]; present {
		generation, valid := strictWorkerIdentityInteger(raw)
		if !valid || generation < 0 {
			return nil, errors.New("worker identity retirement readback generation is invalid")
		}
	}
	if raw, present := payload["acknowledged_generation"]; present {
		generation, valid := strictWorkerIdentityInteger(raw)
		if !valid || generation < 0 {
			return nil, errors.New("worker identity retirement readback acknowledged generation is invalid")
		}
	}
	if raw, present := payload["retirement_receipt_digest"]; present && strings.TrimSpace(anyToString(raw)) != "" && !agentTaskCanonicalSHA256(anyToString(raw)) {
		return nil, errors.New("worker identity retirement readback receipt digest is invalid")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	identity, err := scanWorkerIdentity(tx.QueryRowContext(ctx, `SELECT `+workerIdentitySelectColumns+` FROM task_ledger_worker_identities WHERE identity_id=?`, identityID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("worker identity retirement identity is unknown")
	}
	if err != nil {
		return nil, err
	}
	if identity.PrincipalID != authority.PrincipalID || identity.WorkspaceID != authority.WorkspaceID || identity.WorkerInstanceID != authority.WorkerInstanceID {
		return nil, errors.New("worker identity retirement readback authority does not match")
	}
	if identity.Status != "closed" {
		return nil, errors.New("worker identity retirement is not durably closed")
	}
	var storedJSON, storedPrincipal, storedWorkspace, storedRequested, storedCanonical, storedInstance, storedIdentityDigest, storedClosedDigest, storedStatus, storedRetirementDigest, storedReceiptDigest string
	if err := tx.QueryRowContext(ctx, `SELECT principal_id,workspace_id,requested_worker_id,canonical_worker_id,worker_instance_id,identity_digest,closed_identity_digest,closed_status,retirement_digest,retirement_receipt_digest,payload_json FROM task_ledger_worker_identity_retirements WHERE identity_id=?`, identityID).Scan(&storedPrincipal, &storedWorkspace, &storedRequested, &storedCanonical, &storedInstance, &storedIdentityDigest, &storedClosedDigest, &storedStatus, &storedRetirementDigest, &storedReceiptDigest, &storedJSON); err != nil {
		return nil, errors.New("worker identity retirement proof is unavailable")
	}
	if storedPrincipal != authority.PrincipalID || storedWorkspace != authority.WorkspaceID || storedRequested != identity.RequestedWorkerID || storedCanonical == "" || storedInstance != authority.WorkerInstanceID || storedPrincipal != identity.PrincipalID || storedWorkspace != identity.WorkspaceID || storedInstance != identity.WorkerInstanceID || storedClosedDigest != identity.IdentityDigest || storedIdentityDigest != identityDigest || storedRetirementDigest != retirementDigest || storedStatus != "closed" || identity.IdentityDigest != workerIdentityRecordDigest(identity) {
		return nil, errors.New("worker identity retirement readback is not bound to the exact closed identity")
	}
	stored := map[string]any{}
	if err := json.Unmarshal([]byte(storedJSON), &stored); err != nil {
		return nil, errors.New("worker identity retirement proof is invalid")
	}
	if findings := validateAgentContractPayload(agentWorkerIdentityRetirementReceiptID, stored); len(findings) != 0 || anyToString(stored["retirement_receipt_digest"]) != storedReceiptDigest || anyToString(stored["retirement_receipt_digest"]) != workerIdentityRetirementReceiptDigest(stored) || anyToString(stored["closed_identity_digest"]) != identity.IdentityDigest || anyToString(stored["closed_status"]) != "closed" || !anyToBool(stored["retired"]) || !anyToBool(stored["canonical_reclaimed"]) {
		return nil, errors.New("worker identity retirement readback receipt is invalid")
	}
	for field, expected := range map[string]string{
		"identity_id": identity.IdentityID, "principal_id": authority.PrincipalID, "workspace_id": authority.WorkspaceID,
		"requested_worker_id": storedRequested, "canonical_worker_id": storedCanonical,
		"worker_instance_id": authority.WorkerInstanceID, "identity_digest": identityDigest, "retirement_digest": retirementDigest,
	} {
		if anyToString(stored[field]) != expected {
			return nil, errors.New("worker identity retirement readback receipt does not match the exact authority")
		}
	}
	if anyToString(stored["canonical_worker_id"]) != anyToString(payload["canonical_worker_id"]) && strings.TrimSpace(anyToString(payload["canonical_worker_id"])) != "" {
		return nil, errors.New("worker identity retirement readback canonical ID does not match")
	}
	if anyToString(stored["requested_worker_id"]) != anyToString(payload["requested_worker_id"]) && strings.TrimSpace(anyToString(payload["requested_worker_id"])) != "" {
		return nil, errors.New("worker identity retirement readback requested ID does not match")
	}
	if anyToString(stored["worker_instance_id"]) != anyToString(payload["worker_instance_id"]) && strings.TrimSpace(anyToString(payload["worker_instance_id"])) != "" {
		return nil, errors.New("worker identity retirement readback instance does not match")
	}
	if generation, present := payload["worker_identity_update_generation"]; present && anyToInt(generation, -1) != anyToInt(stored["worker_identity_update_generation"], -2) {
		return nil, errors.New("worker identity retirement readback generation does not match")
	}
	if generation, present := payload["acknowledged_generation"]; present && anyToInt(generation, -1) != anyToInt(stored["acknowledged_generation"], -2) {
		return nil, errors.New("worker identity retirement readback acknowledged generation does not match")
	}
	if suppliedReceiptDigest := strings.TrimSpace(anyToString(payload["retirement_receipt_digest"])); suppliedReceiptDigest != "" && suppliedReceiptDigest != storedReceiptDigest {
		return nil, errors.New("worker identity retirement readback receipt digest does not match")
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	stored["idempotent_replay"] = true
	return agentTaskContractPayload(agentWorkerIdentityRetirementReceiptID, stored), nil
}

func (l *agentTaskDeliveryLedger) workerIdentityResponse(ctx context.Context, identity agentWorkerIdentityRecord) (map[string]any, error) {
	if identity.IdentityDigest == "" {
		identity.IdentityDigest = workerIdentityRecordDigest(identity)
	}
	response := map[string]any{"identity": identity.payload(), "authoritative_backend": "gateway-go-sqlite-wal"}
	if identity.IdentityUpdateGeneration > identity.AcknowledgedGeneration {
		update, err := l.readWorkerIdentityUpdate(ctx, agentWorkerIdentityAuthority{PrincipalID: identity.PrincipalID, WorkspaceID: identity.WorkspaceID, WorkerInstanceID: identity.WorkerInstanceID}, "")
		if err != nil {
			return nil, err
		}
		if update.UpdateID != "" {
			response["identity_update"] = update.payload()
			response["identity_update_required"] = true
		}
	}
	if _, ok := response["identity_update"]; !ok {
		response["identity_update"] = nil
		response["identity_update_required"] = false
	}
	return response, nil
}

func (l *agentTaskDeliveryLedger) readWorkerIdentityUpdate(ctx context.Context, authority agentWorkerIdentityAuthority, updateID string) (agentWorkerIdentityUpdateRecord, error) {
	authority, err := normalizeWorkerIdentityAuthority(authority.PrincipalID, authority.WorkspaceID, authority.WorkerInstanceID)
	if err != nil {
		return agentWorkerIdentityUpdateRecord{}, err
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return agentWorkerIdentityUpdateRecord{}, err
	}
	defer tx.Rollback()
	var update agentWorkerIdentityUpdateRecord
	if strings.TrimSpace(updateID) == "" {
		var identityID string
		if identityErr := tx.QueryRowContext(ctx, `SELECT identity_id FROM task_ledger_worker_identities WHERE principal_id=? AND workspace_id=? AND worker_instance_id=?`, authority.PrincipalID, authority.WorkspaceID, authority.WorkerInstanceID).Scan(&identityID); errors.Is(identityErr, sql.ErrNoRows) {
			return agentWorkerIdentityUpdateRecord{}, errWorkerIdentityNotRegistered
		} else if identityErr != nil {
			return agentWorkerIdentityUpdateRecord{}, identityErr
		}
	}
	if strings.TrimSpace(updateID) != "" {
		update, err = scanWorkerIdentityUpdate(tx.QueryRowContext(ctx, `SELECT `+workerIdentityUpdateSelectColumns+` FROM task_ledger_worker_identity_updates WHERE update_id=? AND principal_id=? AND workspace_id=? AND worker_instance_id=?`, strings.TrimSpace(updateID), authority.PrincipalID, authority.WorkspaceID, authority.WorkerInstanceID))
	} else {
		update, err = scanWorkerIdentityUpdate(tx.QueryRowContext(ctx, `SELECT `+workerIdentityUpdateSelectColumns+` FROM task_ledger_worker_identity_updates WHERE principal_id=? AND workspace_id=? AND worker_instance_id=? AND state<>? ORDER BY worker_identity_update_generation ASC LIMIT 1`, authority.PrincipalID, authority.WorkspaceID, authority.WorkerInstanceID, agentWorkerIdentityStateAcknowledged))
	}
	if errors.Is(err, sql.ErrNoRows) {
		if strings.TrimSpace(updateID) != "" {
			return agentWorkerIdentityUpdateRecord{}, errWorkerIdentityNotRegistered
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return agentWorkerIdentityUpdateRecord{}, commitErr
		}
		return agentWorkerIdentityUpdateRecord{}, nil
	}
	if err != nil {
		return agentWorkerIdentityUpdateRecord{}, err
	}
	if update.State == agentWorkerIdentityStateAcknowledged {
		if commitErr := tx.Commit(); commitErr != nil {
			return agentWorkerIdentityUpdateRecord{}, commitErr
		}
		return update, nil
	}
	if update.State == agentWorkerIdentityStateDelivered {
		if commitErr := tx.Commit(); commitErr != nil {
			return agentWorkerIdentityUpdateRecord{}, commitErr
		}
		return update, nil
	}
	if update.State != agentWorkerIdentityStatePending && update.State != agentWorkerIdentityStateDelivering {
		return agentWorkerIdentityUpdateRecord{}, fmt.Errorf("worker identity update cannot be read from %s", update.State)
	}
	now := agentTaskNow()
	attempts := update.DeliveryAttempts
	if attempts < agentWorkerIdentityMaxAttempts {
		attempts++
	}
	result, err := tx.ExecContext(ctx, `UPDATE task_ledger_worker_identity_updates SET state=?,delivery_attempts=?,updated_at=? WHERE update_id=? AND state IN (?,?)`, agentWorkerIdentityStateDelivering, attempts, now, update.UpdateID, agentWorkerIdentityStatePending, agentWorkerIdentityStateDelivering)
	if err != nil {
		return agentWorkerIdentityUpdateRecord{}, err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		current, currentErr := scanWorkerIdentityUpdate(tx.QueryRowContext(ctx, `SELECT `+workerIdentityUpdateSelectColumns+` FROM task_ledger_worker_identity_updates WHERE update_id=?`, update.UpdateID))
		if currentErr == nil && (current.State == agentWorkerIdentityStateDelivered || current.State == agentWorkerIdentityStateAcknowledged) {
			if commitErr := tx.Commit(); commitErr != nil {
				return agentWorkerIdentityUpdateRecord{}, commitErr
			}
			return current, nil
		}
		if err != nil {
			return agentWorkerIdentityUpdateRecord{}, err
		}
		return agentWorkerIdentityUpdateRecord{}, errors.New("worker identity delivery CAS lost")
	}
	result, err = tx.ExecContext(ctx, `UPDATE task_ledger_worker_identity_updates SET state=?,delivered_at=?,updated_at=? WHERE update_id=? AND state=?`, agentWorkerIdentityStateDelivered, now, now, update.UpdateID, agentWorkerIdentityStateDelivering)
	if err != nil {
		return agentWorkerIdentityUpdateRecord{}, err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		current, currentErr := scanWorkerIdentityUpdate(tx.QueryRowContext(ctx, `SELECT `+workerIdentityUpdateSelectColumns+` FROM task_ledger_worker_identity_updates WHERE update_id=?`, update.UpdateID))
		if currentErr == nil && (current.State == agentWorkerIdentityStateDelivered || current.State == agentWorkerIdentityStateAcknowledged) {
			if commitErr := tx.Commit(); commitErr != nil {
				return agentWorkerIdentityUpdateRecord{}, commitErr
			}
			return current, nil
		}
		if err != nil {
			return agentWorkerIdentityUpdateRecord{}, err
		}
		return agentWorkerIdentityUpdateRecord{}, errors.New("worker identity delivery completion CAS lost")
	}
	if err := tx.Commit(); err != nil {
		return agentWorkerIdentityUpdateRecord{}, err
	}
	return l.workerIdentityUpdateByID(ctx, update.UpdateID)
}

func validateWorkerIdentityAckPayload(payload map[string]any) error {
	allowed := map[string]struct{}{
		"schema_id": {}, "contract_version": {}, "update_id": {}, "identity_id": {},
		"principal_id": {}, "workspace_id": {}, "worker_instance_id": {}, "old_worker_id": {},
		"requested_worker_id": {}, "new_worker_id": {}, "canonical_worker_id": {},
		"worker_identity_update_generation": {}, "update_digest": {},
		"receipt_digest": {}, "ack_receipt_digest": {}, "acknowledged": {}, "idempotent_replay": {}, "identity_update": {},
		"format_contract": {},
	}
	for key := range payload {
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("worker identity acknowledgement contains an unknown field")
		}
	}
	schemaID, ok := payload["schema_id"].(string)
	if !ok || strings.TrimSpace(schemaID) != agentWorkerIdentityAckContractID {
		return errors.New("worker identity acknowledgement contract is invalid")
	}
	if version, ok := payload["contract_version"]; !ok {
		return errors.New("worker identity acknowledgement contract version is invalid")
	} else if parsed, valid := strictWorkerIdentityInteger(version); !valid || parsed != 1 {
		return errors.New("worker identity acknowledgement contract version is invalid")
	}
	acknowledged, ok := payload["acknowledged"].(bool)
	if !ok || !acknowledged {
		return errors.New("worker identity acknowledgement must assert acknowledged=true")
	}
	if replay, ok := payload["idempotent_replay"].(bool); !ok || replay {
		return errors.New("worker identity acknowledgement replay marker is invalid")
	}
	for _, field := range []string{"update_id", "identity_id", "principal_id", "workspace_id", "worker_instance_id", "old_worker_id", "requested_worker_id", "new_worker_id", "canonical_worker_id", "update_digest", "receipt_digest", "ack_receipt_digest"} {
		if _, err := strictWorkerIdentityAckString(payload, field, true); err != nil {
			return err
		}
	}
	if generation, valid := strictWorkerIdentityInteger(payload["worker_identity_update_generation"]); !valid || generation < 0 {
		return errors.New("worker identity acknowledgement generation is invalid")
	}
	if _, ok := payload["format_contract"].(map[string]any); !ok {
		return errors.New("worker identity acknowledgement format contract is invalid")
	}
	nested, exists := payload["identity_update"]
	if !exists {
		return errors.New("worker identity acknowledgement identity_update is required")
	}
	nestedMap, ok := nested.(map[string]any)
	if !ok {
		return errors.New("worker identity acknowledgement identity_update is invalid")
	}
	nestedAllowed := map[string]struct{}{
		"schema_id": {}, "contract_version": {}, "update_id": {}, "identity_id": {}, "principal_id": {}, "workspace_id": {}, "worker_instance_id": {},
		"old_worker_id": {}, "requested_worker_id": {}, "new_worker_id": {}, "canonical_worker_id": {}, "worker_identity_update_generation": {},
		"update_digest": {}, "receipt_digest": {}, "state": {}, "delivery_attempts": {}, "last_error": {}, "created_at": {}, "updated_at": {},
		"delivered_at": {}, "acknowledged_at": {}, "ack_receipt_digest": {}, "expires_at": {}, "ack_required": {}, "format_contract": {},
	}
	for key := range nestedMap {
		if _, ok := nestedAllowed[key]; !ok {
			return errors.New("worker identity acknowledgement identity_update contains an unknown field")
		}
	}
	for _, field := range []string{"schema_id", "update_id", "identity_id", "principal_id", "workspace_id", "worker_instance_id", "old_worker_id", "requested_worker_id", "new_worker_id", "canonical_worker_id", "update_digest", "receipt_digest", "state", "last_error", "created_at", "updated_at", "delivered_at", "acknowledged_at", "ack_receipt_digest", "expires_at"} {
		if _, err := strictWorkerIdentityAckString(nestedMap, field, false); err != nil {
			return fmt.Errorf("worker identity acknowledgement identity_update: %w", err)
		}
	}
	nestedVersion, valid := strictWorkerIdentityInteger(nestedMap["contract_version"])
	if !valid || nestedVersion != 1 {
		return errors.New("worker identity acknowledgement nested contract version is invalid")
	}
	nestedGeneration, valid := strictWorkerIdentityInteger(nestedMap["worker_identity_update_generation"])
	if !valid || nestedGeneration <= 0 {
		return errors.New("worker identity acknowledgement nested generation is invalid")
	}
	if attempts, valid := strictWorkerIdentityInteger(nestedMap["delivery_attempts"]); !valid || attempts < 0 {
		return errors.New("worker identity acknowledgement nested delivery attempts are invalid")
	}
	state, ok := nestedMap["state"].(string)
	if !ok || !workerIdentityUpdateStateValid(state) {
		return errors.New("worker identity acknowledgement nested state is invalid")
	}
	ackRequired, ok := nestedMap["ack_required"].(bool)
	if !ok || ackRequired != (state != agentWorkerIdentityStateAcknowledged) {
		return errors.New("worker identity acknowledgement nested ack_required is invalid")
	}
	if _, ok := nestedMap["format_contract"].(map[string]any); !ok {
		return errors.New("worker identity acknowledgement nested format contract is invalid")
	}
	if findings := validateAgentContractPayload(agentWorkerIdentityAckContractID, payload); len(findings) != 0 {
		return fmt.Errorf("worker identity acknowledgement contract validation failed")
	}
	return nil
}

func strictWorkerIdentityAckString(payload map[string]any, field string, nonEmpty bool) (string, error) {
	value, exists := payload[field]
	if !exists {
		return "", fmt.Errorf("worker identity acknowledgement field %s is required", field)
	}
	text, valid := value.(string)
	if !valid || (nonEmpty && strings.TrimSpace(text) == "") {
		return "", fmt.Errorf("worker identity acknowledgement field %s has an invalid type", field)
	}
	return text, nil
}

func workerIdentityUpdateStateValid(state string) bool {
	switch strings.TrimSpace(state) {
	case agentWorkerIdentityStatePending, agentWorkerIdentityStateDelivering, agentWorkerIdentityStateDelivered, agentWorkerIdentityStateAcknowledged:
		return true
	default:
		return false
	}
}

func workerIdentityTerminalReviewStatus(status string) bool {
	switch strings.TrimSpace(strings.ToLower(status)) {
	case "accepted_for_integration", "changes_requested", "rejected", "superseded", "knowledge_accepted", "unintegrated":
		return true
	default:
		return false
	}
}

// workerIdentityRetirementBoundWork is evaluated inside the retirement
// transaction. The canonical worker ID is also the durable task assignment
// key, so changing the identity's canonical ID before proving that no work is
// still bound to it would let a fresh identity inherit an old queue entry.
//
// A task's claim_worker_id is only a routing hint. Unkeyed tasks deliberately
// store an empty claim_worker_id, while their authoritative ownership is on
// the attempt row. Every downstream check therefore joins through the exact
// attempt and workspace instead of trusting the task routing hint. Keep the
// status sets explicit and conservative: they are the states written by this
// schema, and an unknown value is nonterminal until a migration defines it.
func workerIdentityRetirementBoundWork(ctx context.Context, tx *sql.Tx, identity agentWorkerIdentityRecord) error {
	const terminalTaskStatuses = "('canceled','quarantined','execution_failed','rejected','superseded','unintegrated','integrated','integration_failed','dead_letter')"
	const terminalAttemptStatuses = "('execution_failed','canceled','quarantined','completed')"
	const terminalResultStatuses = "('result_published')"
	const terminalPublicationStatuses = "('committed','dead_letter')"
	const terminalDeliveryStatuses = "('delivered','acknowledged','dead_letter')"
	const terminalReviewStatuses = "('accepted_for_integration','changes_requested','rejected','superseded','knowledge_accepted','unintegrated')"
	const terminalApprovalStatus = "used"
	const terminalIntegrationStatuses = "('integrated','rejected','unintegrated','follow_up_queued')"
	attemptArgs := append([]any{identity.WorkerInstanceID, identity.WorkspaceID}, workerIdentityAttemptBindingArgs(identity)...)

	var taskID, taskStatus string
	err := tx.QueryRowContext(ctx, `SELECT task.id,task.status FROM task_ledger_tasks task JOIN task_ledger_worker_task_bindings b ON b.task_id=task.id WHERE b.identity_id=? AND b.principal_id=? AND lower(trim(b.workspace_id))=lower(?) AND b.worker_instance_id=? AND ((b.worker_identity_update_generation=0 AND lower(trim(b.worker_id))=lower(?)) OR (b.worker_identity_update_generation=? AND ? > 0 AND lower(trim(b.worker_id))=lower(?))) AND b.state IN ('bound','rebind_pending') AND lower(trim(task.workspace_id))=lower(?) AND task.status='queued' LIMIT 1`, identity.IdentityID, identity.PrincipalID, identity.WorkspaceID, identity.WorkerInstanceID, identity.RequestedWorkerID, identity.IdentityUpdateGeneration, identity.IdentityUpdateGeneration, identity.CanonicalWorkerID, identity.WorkspaceID).Scan(&taskID, &taskStatus)
	if err == nil {
		return fmt.Errorf("worker identity retirement requires task %s with canonical assignment in nonterminal state %s to be reconciled", taskID, taskStatus)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// Only the active attempt carries a task's current nonterminal authority;
	// historical attempts may be terminal after a bounded retry.
	err = tx.QueryRowContext(ctx, `SELECT t.id,t.status FROM task_ledger_tasks t JOIN task_ledger_attempts a ON a.attempt_id=t.active_attempt_id AND a.task_id=t.id WHERE a.worker_instance_id=? AND lower(trim(t.workspace_id))=lower(?) AND `+workerIdentityAttemptBindingPredicate("a")+` AND t.status NOT IN `+terminalTaskStatuses+` LIMIT 1`, attemptArgs...).Scan(&taskID, &taskStatus)
	if err == nil {
		return fmt.Errorf("worker identity retirement requires task %s with exact active attempt in nonterminal state %s to be reconciled", taskID, taskStatus)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var attemptID, attemptStatus string
	err = tx.QueryRowContext(ctx, `SELECT a.attempt_id,a.status FROM task_ledger_attempts a JOIN task_ledger_tasks t ON t.id=a.task_id WHERE a.worker_instance_id=? AND lower(trim(t.workspace_id))=lower(?) AND `+workerIdentityAttemptBindingPredicate("a")+` AND a.status NOT IN `+terminalAttemptStatuses+` LIMIT 1`, attemptArgs...).Scan(&attemptID, &attemptStatus)
	if err == nil {
		return fmt.Errorf("worker identity retirement requires attempt %s in nonterminal state %s to be reconciled", attemptID, attemptStatus)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var resultID, resultStatus string
	err = tx.QueryRowContext(ctx, `SELECT r.result_id,r.status FROM task_ledger_results r JOIN task_ledger_attempts a ON a.attempt_id=r.attempt_id AND a.task_id=r.task_id JOIN task_ledger_tasks t ON t.id=r.task_id WHERE a.worker_instance_id=? AND lower(trim(t.workspace_id))=lower(?) AND `+workerIdentityAttemptBindingPredicate("a")+` AND r.status NOT IN `+terminalResultStatuses+` LIMIT 1`, attemptArgs...).Scan(&resultID, &resultStatus)
	if err == nil {
		return fmt.Errorf("worker identity retirement requires result %s in nonterminal state %s to be reconciled", resultID, resultStatus)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var publicationID, publicationStatus string
	err = tx.QueryRowContext(ctx, `SELECT p.publication_id,p.status FROM task_ledger_publications p JOIN task_ledger_attempts a ON a.attempt_id=p.attempt_id AND a.task_id=p.task_id JOIN task_ledger_tasks t ON t.id=p.task_id WHERE a.worker_instance_id=? AND lower(trim(t.workspace_id))=lower(?) AND `+workerIdentityAttemptBindingPredicate("a")+` AND (p.status NOT IN `+terminalPublicationStatuses+` OR (p.status='committed' AND p.writeback_status<>'committed')) LIMIT 1`, attemptArgs...).Scan(&publicationID, &publicationStatus)
	if err == nil {
		return fmt.Errorf("worker identity retirement requires publication %s in nonterminal state %s to be reconciled", publicationID, publicationStatus)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var deliveryID, deliveryStatus string
	err = tx.QueryRowContext(ctx, `SELECT d.delivery_id,d.status FROM task_ledger_deliveries d JOIN task_ledger_publications p ON p.publication_id=d.publication_id AND p.result_id=d.result_id AND p.task_id=d.task_id JOIN task_ledger_attempts a ON a.attempt_id=p.attempt_id AND a.task_id=p.task_id JOIN task_ledger_tasks t ON t.id=d.task_id WHERE a.worker_instance_id=? AND lower(trim(t.workspace_id))=lower(?) AND `+workerIdentityAttemptBindingPredicate("a")+` AND d.status NOT IN `+terminalDeliveryStatuses+` LIMIT 1`, attemptArgs...).Scan(&deliveryID, &deliveryStatus)
	if err == nil {
		return fmt.Errorf("worker identity retirement requires delivery %s in nonterminal state %s to be reconciled", deliveryID, deliveryStatus)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// Reviewer custody remains an immutable row after completion. Retirement
	// may proceed only when every identity-bound claim has an exact terminal
	// review closure owned by the same canonical reviewer; an active, unknown,
	// missing, or owner-mismatched closure remains blocking authority.
	reviewerRows, err := tx.QueryContext(ctx, `SELECT c.claim_id,c.status,c.reviewer_owner,c.actor,c.task_id,c.result_id,t.review_owner,COALESCE(r.review_id,''),COALESCE(r.status,''),COALESCE(r.reviewer_owner,''),COALESCE(r.actor,'') FROM task_ledger_reviewer_claims c JOIN task_ledger_results result ON result.result_id=c.result_id AND result.task_id=c.task_id JOIN task_ledger_attempts a ON a.attempt_id=result.attempt_id AND a.task_id=result.task_id JOIN task_ledger_tasks t ON t.id=c.task_id LEFT JOIN task_ledger_reviews r ON r.result_id=c.result_id AND r.task_id=c.task_id WHERE a.worker_instance_id=? AND lower(trim(t.workspace_id))=lower(?) AND `+workerIdentityAttemptBindingPredicate("a"), attemptArgs...)
	if err != nil {
		return err
	}
	for reviewerRows.Next() {
		var claimID, claimStatus, claimOwner, claimActor, claimTaskID, claimResultID, taskReviewerOwner, reviewID, reviewStatus, reviewOwner, reviewActor string
		if err := reviewerRows.Scan(&claimID, &claimStatus, &claimOwner, &claimActor, &claimTaskID, &claimResultID, &taskReviewerOwner, &reviewID, &reviewStatus, &reviewOwner, &reviewActor); err != nil {
			reviewerRows.Close()
			return err
		}
		if claimStatus == "active" || !workerIdentityTerminalReviewStatus(claimStatus) {
			reviewerRows.Close()
			return fmt.Errorf("worker identity retirement requires reviewer claim %s in nonterminal state %s to be reconciled", claimID, claimStatus)
		}
		if reviewID == "" || reviewStatus != claimStatus || !workerIdentityTerminalReviewStatus(reviewStatus) {
			reviewerRows.Close()
			return fmt.Errorf("worker identity retirement requires reviewer claim %s with an exact terminal review closure", claimID)
		}
		if claimTaskID == "" || claimResultID == "" || !strings.EqualFold(claimOwner, taskReviewerOwner) || !strings.EqualFold(claimActor, taskReviewerOwner) || !strings.EqualFold(reviewOwner, taskReviewerOwner) || !strings.EqualFold(reviewActor, taskReviewerOwner) {
			reviewerRows.Close()
			return fmt.Errorf("worker identity retirement requires reviewer claim %s with an exact terminal review closure", claimID)
		}
	}
	if err := reviewerRows.Err(); err != nil {
		reviewerRows.Close()
		return err
	}
	if err := reviewerRows.Close(); err != nil {
		return err
	}
	var reviewID, reviewStatus string
	err = tx.QueryRowContext(ctx, `SELECT r.review_id,r.status FROM task_ledger_reviews r JOIN task_ledger_results result ON result.result_id=r.result_id AND result.task_id=r.task_id JOIN task_ledger_attempts a ON a.attempt_id=result.attempt_id AND a.task_id=result.task_id JOIN task_ledger_tasks t ON t.id=r.task_id WHERE a.worker_instance_id=? AND lower(trim(t.workspace_id))=lower(?) AND `+workerIdentityAttemptBindingPredicate("a")+` AND r.status NOT IN `+terminalReviewStatuses+` LIMIT 1`, attemptArgs...).Scan(&reviewID, &reviewStatus)
	if err == nil {
		return fmt.Errorf("worker identity retirement requires review %s in nonterminal state %s to be reconciled", reviewID, reviewStatus)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// Approvals have only two writer-defined states: valid and used. A valid
	// row is mutable authority until its bounded expiry; an expired valid row
	// is terminal by time. Parse the timestamp in Go so malformed legacy rows
	// cannot be accidentally treated as expired by SQLite text comparison.
	approvalRows, err := tx.QueryContext(ctx, `SELECT p.approval_id,p.status,p.expires_at,p.approver,t.review_owner,p.task_id,p.attempt_id FROM task_ledger_approvals p JOIN task_ledger_attempts a ON a.attempt_id=p.attempt_id AND a.task_id=p.task_id JOIN task_ledger_tasks t ON t.id=p.task_id WHERE a.worker_instance_id=? AND lower(trim(t.workspace_id))=lower(?) AND `+workerIdentityAttemptBindingPredicate("a")+` ORDER BY p.approval_id ASC`, attemptArgs...)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for approvalRows.Next() {
		var approvalID, approvalStatus, expiresAt, approver, reviewOwner, approvalTaskID, approvalAttemptID string
		if err := approvalRows.Scan(&approvalID, &approvalStatus, &expiresAt, &approver, &reviewOwner, &approvalTaskID, &approvalAttemptID); err != nil {
			approvalRows.Close()
			return err
		}
		if approvalTaskID == "" || approvalAttemptID == "" || approver == "" || !strings.EqualFold(approver, reviewOwner) {
			approvalRows.Close()
			return fmt.Errorf("worker identity retirement requires approval %s with an exact current authority", approvalID)
		}
		if approvalStatus == terminalApprovalStatus {
			continue
		}
		if approvalStatus != "valid" {
			approvalRows.Close()
			return fmt.Errorf("worker identity retirement requires approval %s in unknown state %s to be reconciled", approvalID, approvalStatus)
		}
		expires, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(expiresAt))
		if parseErr != nil {
			approvalRows.Close()
			return fmt.Errorf("worker identity retirement requires approval %s with invalid expiry to be reconciled", approvalID)
		}
		if now.Before(expires) {
			approvalRows.Close()
			return fmt.Errorf("worker identity retirement requires approval %s in nonterminal state valid to be reconciled", approvalID)
		}
	}
	if err := approvalRows.Err(); err != nil {
		approvalRows.Close()
		return err
	}
	if err := approvalRows.Close(); err != nil {
		return err
	}
	var artifactID string
	var artifactFinalized int
	err = tx.QueryRowContext(ctx, `SELECT ar.artifact_id,ar.finalized FROM task_ledger_artifacts ar JOIN task_ledger_attempts a ON a.attempt_id=ar.attempt_id AND a.task_id=ar.task_id JOIN task_ledger_tasks t ON t.id=ar.task_id WHERE a.worker_instance_id=? AND lower(trim(t.workspace_id))=lower(?) AND `+workerIdentityAttemptBindingPredicate("a")+` AND ar.finalized<>1 LIMIT 1`, attemptArgs...).Scan(&artifactID, &artifactFinalized)
	if err == nil {
		return fmt.Errorf("worker identity retirement requires artifact %s with finalized=%d to be reconciled", artifactID, artifactFinalized)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var integrationID, integrationStatus string
	err = tx.QueryRowContext(ctx, `SELECT i.integration_id,i.status FROM task_ledger_integrations i JOIN task_ledger_results result ON result.result_id=i.result_id AND result.task_id=i.task_id JOIN task_ledger_attempts a ON a.attempt_id=result.attempt_id AND a.task_id=result.task_id JOIN task_ledger_tasks t ON t.id=i.task_id WHERE a.worker_instance_id=? AND lower(trim(t.workspace_id))=lower(?) AND `+workerIdentityAttemptBindingPredicate("a")+` AND i.status NOT IN `+terminalIntegrationStatuses+` LIMIT 1`, attemptArgs...).Scan(&integrationID, &integrationStatus)
	if err == nil {
		return fmt.Errorf("worker identity retirement requires integration %s in nonterminal state %s to be reconciled", integrationID, integrationStatus)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := workerIdentityRetirementCleanupReceipts(ctx, tx, identity); err != nil {
		return err
	}
	return nil
}

// workerIdentityRetirementCleanupReceipts proves that every durable terminal
// publication owned by the identity has the exact cleanup acknowledgement that
// the publication boundary authorized. A publication row alone is not proof of
// cleanup: the immutable receipt and both boundary digests must bind the same
// task/result/attempt/lease/worker/instance/generation tuple.
func workerIdentityRetirementCleanupReceipts(ctx context.Context, tx *sql.Tx, identity agentWorkerIdentityRecord) error {
	attemptArgs := append([]any{identity.WorkerInstanceID, identity.WorkspaceID}, workerIdentityAttemptBindingArgs(identity)...)
	rows, err := tx.QueryContext(ctx, `SELECT p.publication_id,p.result_id,p.task_id,p.attempt_id,p.idempotency_key,p.status,p.writeback_status,r.digest,r.payload_json,r.execution_observed,r.immutable,a.lease_id,a.generation,a.generation,a.worker_identity_update_generation,a.worker_id,a.worker_instance_id FROM task_ledger_publications p JOIN task_ledger_results r ON r.result_id=p.result_id AND r.task_id=p.task_id AND r.attempt_id=p.attempt_id JOIN task_ledger_attempts a ON a.attempt_id=p.attempt_id AND a.task_id=p.task_id JOIN task_ledger_tasks t ON t.id=p.task_id WHERE a.worker_instance_id=? AND lower(trim(t.workspace_id))=lower(?) AND `+workerIdentityAttemptBindingPredicate("a")+` AND p.status IN ('committed','dead_letter')`, attemptArgs...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var boundary agentTaskPublicationBoundaryIdentity
		var writebackStatus, resultJSON string
		var resultObserved, resultImmutable int
		var identityUpdateGeneration int
		if err := rows.Scan(&boundary.PublicationID, &boundary.ResultID, &boundary.TaskID, &boundary.AttemptID, &boundary.IdempotencyKey, &boundary.PublicationStatus, &writebackStatus, &boundary.ResultDigest, &resultJSON, &resultObserved, &resultImmutable, &boundary.LeaseID, &boundary.AssignmentGeneration, &boundary.LeaseGeneration, &identityUpdateGeneration, &boundary.WorkerID, &boundary.WorkerInstanceID); err != nil {
			return err
		}
		expectedWorkerID := identity.CanonicalWorkerID
		if identityUpdateGeneration == 0 {
			expectedWorkerID = identity.RequestedWorkerID
		}
		if !((identityUpdateGeneration == 0 && strings.EqualFold(boundary.WorkerID, identity.RequestedWorkerID)) || (identityUpdateGeneration > 0 && strings.EqualFold(boundary.WorkerID, identity.CanonicalWorkerID))) || boundary.WorkerID != expectedWorkerID || boundary.WorkerInstanceID != identity.WorkerInstanceID || resultObserved != 1 || resultImmutable != 1 {
			return fmt.Errorf("worker identity retirement publication %s is not backed by exact immutable worker evidence", boundary.PublicationID)
		}
		expectedWritebackStatus := map[string]string{"committed": "committed", "dead_letter": "dead_letter"}[boundary.PublicationStatus]
		if writebackStatus != expectedWritebackStatus {
			return fmt.Errorf("worker identity retirement publication %s has nonterminal writeback status %s", boundary.PublicationID, writebackStatus)
		}
		result := decodeAgentTaskMap(resultJSON)
		if anyToString(result["task_id"]) != boundary.TaskID || anyToString(result["attempt_id"]) != boundary.AttemptID || agentTaskDigest(result) != boundary.ResultDigest {
			return fmt.Errorf("worker identity retirement publication %s result digest linkage is invalid", boundary.PublicationID)
		}
		var bindingErr error
		boundary.WorkspaceRef, boundary.CleanupID, bindingErr = agentTaskResultCleanupBinding(result, boundary.TaskID, boundary.AttemptID)
		if bindingErr != nil {
			return fmt.Errorf("worker identity retirement publication %s cleanup binding is invalid: %w", boundary.PublicationID, bindingErr)
		}
		publicationReceipt, cleanupAuthorization, boundaryErr := agentTaskPublicationBoundary(boundary)
		if boundaryErr != nil {
			return fmt.Errorf("worker identity retirement publication %s boundary is invalid: %w", boundary.PublicationID, boundaryErr)
		}
		var cleanupCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_ledger_cleanup_receipts WHERE publication_id=?`, boundary.PublicationID).Scan(&cleanupCount); err != nil {
			return err
		}
		if cleanupCount != 1 {
			return fmt.Errorf("worker identity retirement requires exactly one cleanup receipt for publication %s, found %d", boundary.PublicationID, cleanupCount)
		}
		var receiptID, authorizationDigest, publicationDigest, publicationID, resultID, taskID, attemptID, leaseID, idempotencyKey, workerID, workerInstanceID, payloadJSON string
		var assignmentGeneration, leaseGeneration int
		if err := tx.QueryRowContext(ctx, `SELECT cleanup_receipt_id,cleanup_authorization_digest,publication_receipt_digest,publication_id,result_id,task_id,attempt_id,lease_id,assignment_generation,lease_generation,worker_id,worker_instance_id,idempotency_key,payload_json FROM task_ledger_cleanup_receipts WHERE publication_id=?`, boundary.PublicationID).Scan(&receiptID, &authorizationDigest, &publicationDigest, &publicationID, &resultID, &taskID, &attemptID, &leaseID, &assignmentGeneration, &leaseGeneration, &workerID, &workerInstanceID, &idempotencyKey, &payloadJSON); err != nil {
			return err
		}
		receipt := decodeAgentTaskMap(payloadJSON)
		if err := verifyAgentTaskCleanupReceipt(receipt); err != nil || !anyToBool(receipt["recorded"]) || !anyToBool(receipt["durable"]) || !anyToBool(receipt["acknowledged"]) {
			if err == nil {
				err = errors.New("cleanup receipt acknowledgement flags are incomplete")
			}
			return fmt.Errorf("worker identity retirement cleanup receipt %s is invalid: %w", receiptID, err)
		}
		if receiptID != anyToString(receipt["receipt_id"]) || authorizationDigest != anyToString(cleanupAuthorization["authorization_digest"]) || publicationDigest != anyToString(publicationReceipt["receipt_digest"]) || publicationID != boundary.PublicationID || resultID != boundary.ResultID || taskID != boundary.TaskID || attemptID != boundary.AttemptID || leaseID != boundary.LeaseID || assignmentGeneration != boundary.AssignmentGeneration || leaseGeneration != boundary.LeaseGeneration || workerID != boundary.WorkerID || workerInstanceID != boundary.WorkerInstanceID || idempotencyKey != boundary.IdempotencyKey {
			return fmt.Errorf("worker identity retirement cleanup receipt %s is not linked to the exact publication boundary", receiptID)
		}
		for field, expected := range map[string]string{
			"cleanup_id": boundary.CleanupID, "workspace_ref": boundary.WorkspaceRef,
			"publication_id": boundary.PublicationID, "result_id": boundary.ResultID,
			"task_id": boundary.TaskID, "attempt_id": boundary.AttemptID, "lease_id": boundary.LeaseID,
			"worker_id": boundary.WorkerID, "worker_instance_id": boundary.WorkerInstanceID,
		} {
			if anyToString(receipt[field]) != expected {
				return fmt.Errorf("worker identity retirement cleanup receipt %s does not bind %s", receiptID, field)
			}
		}
		if anyToInt(receipt["generation"], 0) != boundary.LeaseGeneration {
			return fmt.Errorf("worker identity retirement cleanup receipt %s generation linkage is invalid", receiptID)
		}
	}
	return rows.Err()
}

func strictWorkerIdentityInteger(value any) (int, bool) {
	switch typed := value.(type) {
	case json.Number:
		raw := strings.TrimSpace(typed.String())
		if raw == "" || strings.ContainsAny(raw, ".eE") {
			return 0, false
		}
		parsed, err := strconv.ParseInt(raw, 10, 0)
		if err != nil {
			return 0, false
		}
		return int(parsed), true
	case int:
		return typed, true
	case int8:
		return int(typed), true
	case int16:
		return int(typed), true
	case int32:
		return int(typed), true
	case int64:
		return int(typed), int64(int(typed)) == typed
	case uint:
		return int(typed), uint(int(typed)) == typed
	case uint8:
		return int(typed), true
	case uint16:
		return int(typed), true
	case uint32:
		return int(typed), uint32(int(typed)) == typed
	case uint64:
		return int(typed), uint64(int(typed)) == typed
	default:
		return 0, false
	}
}

var workerIdentityReceiptFields = []string{
	"schema_id", "contract_version", "update_id", "identity_id", "principal_id", "workspace_id", "worker_instance_id",
	"old_worker_id", "requested_worker_id", "new_worker_id", "canonical_worker_id", "worker_identity_update_generation",
	"update_digest", "receipt_digest", "state", "delivery_attempts", "last_error", "created_at", "updated_at",
	"delivered_at", "acknowledged_at", "ack_receipt_digest", "expires_at", "ack_required", "format_contract",
}

func workerIdentityReceiptMatchesUpdate(receipt map[string]any, update agentWorkerIdentityUpdateRecord) bool {
	if receipt == nil {
		return false
	}
	expected := update.payload()
	for _, field := range workerIdentityReceiptFields {
		if _, present := receipt[field]; !present {
			return false
		}
		if !agentTaskCanonicalMapEqual(map[string]any{"value": receipt[field]}, map[string]any{"value": expected[field]}) {
			return false
		}
	}
	return true
}

func workerIdentityReceiptSnapshotMatches(receipt map[string]any, raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return false
	}
	var stored map[string]any
	if err := json.Unmarshal([]byte(raw), &stored); err != nil || stored == nil {
		return false
	}
	return agentTaskCanonicalMapEqual(receipt, stored)
}

func (l *agentTaskDeliveryLedger) acknowledgeWorkerIdentityUpdate(ctx context.Context, payload map[string]any, authority agentWorkerIdentityAuthority) (map[string]any, error) {
	if payload == nil {
		return nil, errors.New("worker identity acknowledgement is required")
	}
	if err := validateWorkerIdentityAckPayload(payload); err != nil {
		return nil, err
	}
	authority, err := normalizeWorkerIdentityAuthority(authority.PrincipalID, authority.WorkspaceID, authority.WorkerInstanceID)
	if err != nil {
		return nil, err
	}
	updateID := strings.TrimSpace(payload["update_id"].(string))
	if updateID == "" {
		return nil, errors.New("worker identity update_id is required")
	}
	requested := strings.TrimSpace(payload["requested_worker_id"].(string))
	canonical := strings.TrimSpace(payload["canonical_worker_id"].(string))
	requestedAlias := strings.TrimSpace(payload["old_worker_id"].(string))
	canonicalAlias := strings.TrimSpace(payload["new_worker_id"].(string))
	if requested != requestedAlias || canonical != canonicalAlias {
		return nil, errors.New("worker identity acknowledgement worker ID aliases do not match")
	}
	generation, generationValid := strictWorkerIdentityInteger(payload["worker_identity_update_generation"])
	if !generationValid {
		return nil, errors.New("worker identity acknowledgement generation is invalid")
	}
	receipt := strings.TrimSpace(payload["ack_receipt_digest"].(string))
	if requested == "" || canonical == "" || generation <= 0 || receipt == "" {
		return nil, errors.New("complete worker identity acknowledgement binding is required")
	}
	if !agentTaskCanonicalSHA256(receipt) {
		return nil, errors.New("worker identity acknowledgement receipt digest is invalid")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	update, err := scanWorkerIdentityUpdate(tx.QueryRowContext(ctx, `SELECT `+workerIdentityUpdateSelectColumns+` FROM task_ledger_worker_identity_updates WHERE update_id=?`, updateID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("unknown worker identity update")
	}
	if err != nil {
		return nil, err
	}
	if update.PrincipalID != authority.PrincipalID || update.WorkspaceID != authority.WorkspaceID || update.WorkerInstanceID != authority.WorkerInstanceID {
		return nil, errors.New("worker identity update authority does not match")
	}
	if update.IdentityUpdateGeneration != generation || update.RequestedWorkerID != requested || update.OldWorkerID != requested || update.CanonicalWorkerID != canonical || update.NewWorkerID != canonical {
		return nil, errors.New("worker identity update acknowledgement is stale or tampered")
	}
	if strings.TrimSpace(payload["identity_id"].(string)) != update.IdentityID ||
		strings.TrimSpace(payload["principal_id"].(string)) != update.PrincipalID ||
		strings.TrimSpace(payload["workspace_id"].(string)) != update.WorkspaceID ||
		strings.TrimSpace(payload["worker_instance_id"].(string)) != update.WorkerInstanceID ||
		strings.TrimSpace(payload["update_digest"].(string)) != update.UpdateDigest ||
		strings.TrimSpace(payload["receipt_digest"].(string)) != update.ReceiptDigest {
		return nil, errors.New("worker identity acknowledgement does not bind the exact update authority")
	}
	if oldWorkerID := strings.TrimSpace(payload["old_worker_id"].(string)); oldWorkerID != update.OldWorkerID || strings.TrimSpace(payload["requested_worker_id"].(string)) != update.RequestedWorkerID || strings.TrimSpace(payload["new_worker_id"].(string)) != update.NewWorkerID || strings.TrimSpace(payload["canonical_worker_id"].(string)) != update.CanonicalWorkerID {
		return nil, errors.New("worker identity acknowledgement does not bind the exact worker IDs")
	}
	nestedMap := payload["identity_update"].(map[string]any)
	for _, field := range []string{"update_id", "identity_id", "principal_id", "workspace_id", "worker_instance_id", "old_worker_id", "requested_worker_id", "new_worker_id", "canonical_worker_id", "update_digest", "receipt_digest", "ack_receipt_digest"} {
		if strings.TrimSpace(nestedMap[field].(string)) != strings.TrimSpace(payload[field].(string)) {
			return nil, errors.New("worker identity acknowledgement nested update does not match the exact receipt")
		}
	}
	if update.UpdateDigest != workerIdentityUpdateDigest(update) || update.ReceiptDigest != workerIdentityReceiptDigest(update) || workerIdentityAckReceiptDigest(update, authority) != receipt {
		return nil, errors.New("worker identity update acknowledgement digest does not match")
	}
	if update.State == agentWorkerIdentityStateAcknowledged {
		if (update.AckReceiptPayloadVersion != workerIdentityAckReceiptPayloadVersionExact && update.AckReceiptPayloadVersion != workerIdentityAckReceiptPayloadVersionLegacyReconciled) || update.AckReceiptDigest != receipt || !workerIdentityReceiptSnapshotMatches(nestedMap, update.AckReceiptPayloadJSON) {
			return nil, errors.New("worker identity acknowledgement replay does not match")
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return map[string]any{"identity_update": update.payload(), "idempotent_replay": true, "authoritative_backend": "gateway-go-sqlite-wal"}, nil
	}
	if update.State != agentWorkerIdentityStatePending && update.State != agentWorkerIdentityStateDelivering && update.State != agentWorkerIdentityStateDelivered {
		return nil, fmt.Errorf("worker identity update cannot be acknowledged from %s", update.State)
	}
	if !workerIdentityReceiptMatchesUpdate(nestedMap, update) {
		return nil, errors.New("worker identity acknowledgement nested update does not match the durable receipt")
	}
	now := agentTaskNow()
	receiptSnapshot := encodeAgentTaskJSON(nestedMap)
	if receiptSnapshot == "{}" {
		return nil, errors.New("worker identity acknowledgement receipt is not serializable")
	}
	identity, identityErr := scanWorkerIdentity(tx.QueryRowContext(ctx, `SELECT `+workerIdentitySelectColumns+` FROM task_ledger_worker_identities WHERE identity_id=?`, update.IdentityID))
	if identityErr != nil {
		return nil, errors.New("worker identity acknowledgement authority is unavailable")
	}
	if identity.PrincipalID != update.PrincipalID || identity.WorkspaceID != update.WorkspaceID || identity.WorkerInstanceID != update.WorkerInstanceID || identity.IdentityUpdateGeneration != update.IdentityUpdateGeneration || identity.Status != "active" {
		return nil, errors.New("worker identity acknowledgement authority is stale")
	}
	if _, err := l.rebindWorkerIdentityTaskBindingsTx(ctx, tx, identity, update, receipt); err != nil {
		return nil, err
	}
	if _, err := l.adoptWorkerIdentityPreRegistrationAttemptsTx(ctx, tx, identity, update, receipt); err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE task_ledger_worker_identity_updates SET state=?,acknowledged_at=?,ack_receipt_digest=?,ack_receipt_payload_json=?,ack_receipt_payload_version=1,updated_at=? WHERE update_id=? AND state IN (?,?,?)`, agentWorkerIdentityStateAcknowledged, now, receipt, receiptSnapshot, now, update.UpdateID, agentWorkerIdentityStatePending, agentWorkerIdentityStateDelivering, agentWorkerIdentityStateDelivered)
	if err != nil {
		return nil, err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		current, currentErr := scanWorkerIdentityUpdate(tx.QueryRowContext(ctx, `SELECT `+workerIdentityUpdateSelectColumns+` FROM task_ledger_worker_identity_updates WHERE update_id=?`, update.UpdateID))
		if currentErr == nil && current.State == agentWorkerIdentityStateAcknowledged && (current.AckReceiptPayloadVersion == workerIdentityAckReceiptPayloadVersionExact || current.AckReceiptPayloadVersion == workerIdentityAckReceiptPayloadVersionLegacyReconciled) && current.AckReceiptDigest == receipt && workerIdentityReceiptSnapshotMatches(nestedMap, current.AckReceiptPayloadJSON) {
			if commitErr := tx.Commit(); commitErr != nil {
				return nil, commitErr
			}
			return map[string]any{"identity_update": current.payload(), "idempotent_replay": true, "authoritative_backend": "gateway-go-sqlite-wal"}, nil
		}
		if err != nil {
			return nil, err
		}
		return nil, errors.New("worker identity acknowledgement CAS lost")
	}
	result, err = tx.ExecContext(ctx, `UPDATE task_ledger_worker_identities SET acknowledged_generation=?,updated_at=? WHERE identity_id=? AND worker_identity_update_generation=? AND acknowledged_generation<?`, generation, now, update.IdentityID, generation, generation)
	if err != nil {
		return nil, err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		current, currentErr := scanWorkerIdentity(tx.QueryRowContext(ctx, `SELECT `+workerIdentitySelectColumns+` FROM task_ledger_worker_identities WHERE identity_id=?`, update.IdentityID))
		if currentErr == nil && current.AcknowledgedGeneration == generation {
			if commitErr := tx.Commit(); commitErr != nil {
				return nil, commitErr
			}
			update.State = agentWorkerIdentityStateAcknowledged
			update.AcknowledgedAt = current.UpdatedAt
			update.AckReceiptDigest = receipt
			return map[string]any{"identity_update": update.payload(), "idempotent_replay": true, "authoritative_backend": "gateway-go-sqlite-wal"}, nil
		}
		if err != nil {
			return nil, err
		}
		return nil, errors.New("worker identity generation CAS lost")
	}
	update.State = agentWorkerIdentityStateAcknowledged
	update.AcknowledgedAt = now
	update.AckReceiptDigest = receipt
	update.AckReceiptPayloadVersion = workerIdentityAckReceiptPayloadVersionExact
	update.UpdatedAt = now
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]any{"identity_update": update.payload(), "idempotent_replay": false, "authoritative_backend": "gateway-go-sqlite-wal"}, nil
}

func (l *agentTaskDeliveryLedger) resolveWorkerIdentityForClaim(ctx context.Context, principal, workspace, requested, instance string) (agentWorkerIdentityRecord, error) {
	authority, err := normalizeWorkerIdentityAuthority(principal, workspace, instance)
	if err != nil {
		return agentWorkerIdentityRecord{}, err
	}
	requested, err = normalizeWorkerIdentityID(requested, "requested_worker_id")
	if err != nil {
		return agentWorkerIdentityRecord{}, err
	}
	identity, err := l.workerIdentityByAuthority(ctx, authority)
	if errors.Is(err, sql.ErrNoRows) {
		return agentWorkerIdentityRecord{}, errWorkerIdentityNotRegistered
	}
	if err != nil {
		return agentWorkerIdentityRecord{}, err
	}
	if identity.Status != "active" {
		return agentWorkerIdentityRecord{}, errors.New("worker identity is closed")
	}
	if identity.RequestedWorkerID != requested {
		return agentWorkerIdentityRecord{}, errors.New("requested worker ID does not match the registered instance")
	}
	if identity.IdentityUpdateGeneration > identity.AcknowledgedGeneration {
		return agentWorkerIdentityRecord{}, errWorkerIdentityUpdatePending
	}
	return identity, nil
}

func (l *agentTaskDeliveryLedger) workerIdentityUpdatePending(ctx context.Context, identityID string) (bool, error) {
	var count int
	err := l.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_ledger_worker_identity_updates WHERE identity_id=? AND state<>?`, strings.TrimSpace(identityID), agentWorkerIdentityStateAcknowledged).Scan(&count)
	return count > 0, err
}

func workerIdentityGenerationFromClaim(claim map[string]any) int {
	return anyToInt(anyMap(claim["lease"])["worker_identity_update_generation"], anyToInt(anyMap(claim["attempt"])["worker_identity_update_generation"], 0))
}

func (l *agentTaskDeliveryLedger) workerIdentityUpdateState(ctx context.Context, updateID string) (string, error) {
	var state string
	err := l.db.QueryRowContext(ctx, `SELECT state FROM task_ledger_worker_identity_updates WHERE update_id=?`, strings.TrimSpace(updateID)).Scan(&state)
	return state, err
}

func workerIdentityNow() string { return time.Now().UTC().Format(time.RFC3339Nano) }
