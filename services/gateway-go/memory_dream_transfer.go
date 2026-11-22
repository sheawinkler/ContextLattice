package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

const (
	dreamExperienceTransferSchemaID = "dream_experience_transfer.v1"
	dreamTransferReceiptSchemaID    = "dream_experience_transfer_receipt.v1"
	dreamTransferProofHeader        = "X-ContextLattice-Dream-Proof"
	dreamExperienceTransferMaxTTL   = 30 * 24 * time.Hour
	dreamExperienceTransferMaxBytes = 64 * 1024
	dreamTransferRecoveryRecordMax  = 32 * 1024
)

type dreamTransferIdentityContextKey struct{}

type dreamTransferIdentity struct {
	principal string
	role      string
}

func (s *server) dreamTransferContext(ctx context.Context, r *http.Request) context.Context {
	if s == nil || r == nil {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	apiKey, proof, allowed := dreamTransferWorkerCredentials(s, r)
	if !allowed {
		return ctx
	}
	identity := dreamTransferIdentity{
		role: "worker", principal: "api-key-sha256:" + sha256Hex(apiKey) + "/cap-sha256:" + sha256Hex(proof),
	}
	return context.WithValue(ctx, dreamTransferIdentityContextKey{}, identity)
}

func dreamTransferIdentityFromContext(ctx context.Context) (dreamTransferIdentity, bool) {
	if ctx == nil {
		return dreamTransferIdentity{}, false
	}
	identity, ok := ctx.Value(dreamTransferIdentityContextKey{}).(dreamTransferIdentity)
	return identity, ok && identity.role == "worker" && identity.principal != ""
}

type dreamTransferReceipt struct {
	SchemaID           string                   `json:"schema_id"`
	Version            int                      `json:"version"`
	ReceiptID          string                   `json:"receipt_id"`
	Kind               string                   `json:"kind"`
	GrantID            string                   `json:"grant_id"`
	TransferID         string                   `json:"transfer_id,omitempty"`
	SourceProject      string                   `json:"source_project"`
	SourcePassportID   string                   `json:"source_passport_id"`
	ExperienceRef      string                   `json:"experience_ref"`
	SourceRecordHash   string                   `json:"source_record_hash"`
	DestinationProject string                   `json:"destination_project"`
	CallerPrincipal    string                   `json:"caller_principal"`
	RecordDigest       string                   `json:"record_digest,omitempty"`
	ParentRecordDigest string                   `json:"parent_record_digest,omitempty"`
	ResultStatus       string                   `json:"result_status,omitempty"`
	RequestDigest      string                   `json:"request_digest,omitempty"`
	RecoveryRecord     string                   `json:"recovery_record,omitempty"`
	ExpiresAt          string                   `json:"expires_at"`
	RecordedAt         string                   `json:"recorded_at"`
	Issuer             contextPassportIssuer    `json:"issuer"`
	Signature          contextPassportSignature `json:"signature"`
}

type dreamTransferReceiptUnsigned struct {
	SchemaID           string                `json:"schema_id"`
	Version            int                   `json:"version"`
	ReceiptID          string                `json:"receipt_id"`
	Kind               string                `json:"kind"`
	GrantID            string                `json:"grant_id"`
	TransferID         string                `json:"transfer_id,omitempty"`
	SourceProject      string                `json:"source_project"`
	SourcePassportID   string                `json:"source_passport_id"`
	ExperienceRef      string                `json:"experience_ref"`
	SourceRecordHash   string                `json:"source_record_hash"`
	DestinationProject string                `json:"destination_project"`
	CallerPrincipal    string                `json:"caller_principal"`
	RecordDigest       string                `json:"record_digest,omitempty"`
	ParentRecordDigest string                `json:"parent_record_digest,omitempty"`
	ResultStatus       string                `json:"result_status,omitempty"`
	RequestDigest      string                `json:"request_digest,omitempty"`
	RecoveryRecord     string                `json:"recovery_record,omitempty"`
	ExpiresAt          string                `json:"expires_at"`
	RecordedAt         string                `json:"recorded_at"`
	Issuer             contextPassportIssuer `json:"issuer"`
}

func dreamTransferReceiptPayload(receipt dreamTransferReceipt) dreamTransferReceiptUnsigned {
	return dreamTransferReceiptUnsigned{
		SchemaID: receipt.SchemaID, Version: receipt.Version, ReceiptID: receipt.ReceiptID,
		Kind: receipt.Kind, GrantID: receipt.GrantID, TransferID: receipt.TransferID,
		SourceProject: receipt.SourceProject, SourcePassportID: receipt.SourcePassportID,
		ExperienceRef: receipt.ExperienceRef, SourceRecordHash: receipt.SourceRecordHash,
		DestinationProject: receipt.DestinationProject, CallerPrincipal: receipt.CallerPrincipal,
		RecordDigest: receipt.RecordDigest, ParentRecordDigest: receipt.ParentRecordDigest, ResultStatus: receipt.ResultStatus,
		RequestDigest: receipt.RequestDigest, RecoveryRecord: receipt.RecoveryRecord,
		ExpiresAt: receipt.ExpiresAt, RecordedAt: receipt.RecordedAt, Issuer: receipt.Issuer,
	}
}

func dreamTransferRequestDigest(destination string, input map[string]any) string {
	encoded, err := json.Marshal(map[string]any{"project": destination, "experience_transfer": input})
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func verifyDreamTransferReceipt(receipt dreamTransferReceipt) bool {
	if receipt.SchemaID != dreamTransferReceiptSchemaID || receipt.Version != 1 || receipt.ReceiptID == "" || receipt.GrantID == "" ||
		receipt.SourceProject == "" || receipt.SourcePassportID == "" || receipt.ExperienceRef == "" ||
		receipt.DestinationProject == "" || receipt.CallerPrincipal == "" ||
		!verifySignedBytes(dreamTransferReceiptPayload(receipt), receipt.Signature, receipt.Issuer) {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, receipt.RecordedAt); err != nil {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, receipt.ExpiresAt); err != nil {
		return false
	}
	switch receipt.Kind {
	case "grant":
		return receipt.TransferID == "" && receipt.RecordDigest == "" && receipt.ResultStatus == "" && receipt.SourceRecordHash != ""
	case "revocation":
		return receipt.TransferID == "" && receipt.RecordDigest == "" && receipt.ResultStatus == ""
	case "freeze":
		if receipt.TransferID == "" || receipt.RecordDigest == "" || receipt.ResultStatus != "" {
			return false
		}
		return dreamTransferReceiptHasValidRecovery(receipt, "freeze")
	case "result":
		switch receipt.ResultStatus {
		case "confirmed", "narrowed", "rejected", "unproven":
			return receipt.TransferID != "" && receipt.RecordDigest != "" && receipt.ParentRecordDigest != "" &&
				dreamTransferReceiptHasValidRecovery(receipt, "result")
		}
	}
	return false
}

func (s *contextMeshStore) appendDreamTransferReceipt(ctx context.Context, receipt dreamTransferReceipt, onlyOneResult bool) error {
	if s == nil || !s.enabled || s.identity == nil {
		return errors.New("transfer authority unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	unlockFile, err := lockOwnerOnlyFileContext(ctx, s.path+".dream-transfer.lock")
	if err != nil {
		return err
	}
	defer unlockFile()
	if err := s.loadDreamTransferReceiptsLocked(); err != nil {
		return err
	}
	if len(s.dreamTransferReceipts) >= s.maxReceipts {
		return errors.New("transfer receipt capacity reached")
	}
	now := time.Now().UTC()
	if receipt.Kind == "revocation" || receipt.Kind == "freeze" || receipt.Kind == "result" {
		grant, ok, revoked := s.dreamTransferGrantLocked(receipt.GrantID, now)
		if !ok || revoked || !dreamTransferGrantMatches(grant, receipt) {
			return errors.New("transfer grant unavailable")
		}
	}
	for _, existing := range s.dreamTransferReceipts {
		if existing.ReceiptID == receipt.ReceiptID || (onlyOneResult && existing.Kind == "result" && existing.TransferID == receipt.TransferID) {
			return errors.New("transfer receipt already recorded")
		}
		if receipt.Kind == "grant" && existing.Kind == "grant" && existing.GrantID == receipt.GrantID {
			return errors.New("transfer grant already recorded")
		}
		if receipt.Kind == "revocation" && existing.Kind == "revocation" && existing.GrantID == receipt.GrantID {
			return errors.New("transfer grant already revoked")
		}
		if receipt.Kind == "freeze" && existing.Kind == "freeze" && existing.TransferID == receipt.TransferID {
			return errors.New("transfer already frozen")
		}
		if receipt.Kind == "freeze" && existing.Kind == "freeze" && existing.GrantID == receipt.GrantID {
			return errors.New("transfer grant already used to freeze a prediction")
		}
		if receipt.Kind == "result" && existing.Kind == "freeze" && existing.TransferID == receipt.TransferID && existing.RecordDigest != dreamTransferRecordDigestForFreeze(receipt) {
			return errors.New("frozen transfer identity mismatch")
		}
	}
	if receipt.Kind == "result" {
		foundFreeze := false
		for _, existing := range s.dreamTransferReceipts {
			if existing.Kind == "freeze" && existing.TransferID == receipt.TransferID &&
				existing.GrantID == receipt.GrantID && existing.RecordDigest == dreamTransferRecordDigestForFreeze(receipt) &&
				verifyDreamTransferReceipt(existing) {
				foundFreeze = true
				break
			}
		}
		if !foundFreeze {
			return errors.New("frozen transfer receipt unavailable")
		}
	}
	receipt.SchemaID, receipt.Version = dreamTransferReceiptSchemaID, 1
	receipt.Issuer = contextPassportIssuer{
		InstanceID: s.identity.InstanceID, AgentID: "contextlattice-dream-transfer",
		SigningKeyID: s.identity.SigningKeyID, SigningPublicKey: s.identity.SigningPublicKey,
	}
	if receipt.RecordedAt == "" {
		receipt.RecordedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	signature, err := signBytesWithIdentity(dreamTransferReceiptPayload(receipt), s.identity)
	if err != nil {
		return err
	}
	receipt.Signature = signature
	if !verifyDreamTransferReceipt(receipt) {
		return errors.New("transfer receipt invalid")
	}
	previous := append([]dreamTransferReceipt(nil), s.dreamTransferReceipts...)
	s.dreamTransferReceipts = append(s.dreamTransferReceipts, receipt)
	if err := s.saveDreamTransferReceiptsLocked(); err != nil {
		s.dreamTransferReceipts = previous
		return err
	}
	return nil
}

func (s *contextMeshStore) dreamTransferGrant(grantID string) (dreamTransferReceipt, bool, bool) {
	if s == nil {
		return dreamTransferReceipt{}, false, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadDreamTransferReceiptsLocked() != nil {
		return dreamTransferReceipt{}, false, false
	}
	return s.dreamTransferGrantLocked(grantID, time.Now().UTC())
}

func (s *contextMeshStore) dreamTransferGrantLocked(grantID string, now time.Time) (dreamTransferReceipt, bool, bool) {
	var grant dreamTransferReceipt
	found, revoked := false, false
	for _, receipt := range s.dreamTransferReceipts {
		if receipt.GrantID != grantID || !verifyDreamTransferReceipt(receipt) || s.identity == nil || receipt.Issuer.SigningKeyID != s.identity.SigningKeyID {
			continue
		}
		switch receipt.Kind {
		case "grant":
			grant, found = receipt, true
		case "revocation":
			if found && dreamTransferGrantMatches(grant, receipt) {
				revoked = true
			}
		}
	}
	if !found || revoked {
		return grant, found, revoked
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, grant.ExpiresAt)
	if err != nil || !now.Before(expiresAt) {
		return grant, found, true
	}
	return grant, found, false
}

func dreamTransferGrantMatches(grant, candidate dreamTransferReceipt) bool {
	return grant.GrantID == candidate.GrantID && grant.SourceProject == candidate.SourceProject &&
		grant.SourcePassportID == candidate.SourcePassportID && grant.ExperienceRef == candidate.ExperienceRef &&
		grant.SourceRecordHash == candidate.SourceRecordHash && grant.DestinationProject == candidate.DestinationProject &&
		grant.CallerPrincipal == candidate.CallerPrincipal && grant.ExpiresAt == candidate.ExpiresAt
}

func dreamTransferRecordDigestForFreeze(receipt dreamTransferReceipt) string {
	if receipt.Kind == "freeze" {
		return receipt.RecordDigest
	}
	return receipt.ParentRecordDigest
}

func (s *contextMeshStore) dreamTransferReceipt(kind, transferID string) (dreamTransferReceipt, bool) {
	if s == nil {
		return dreamTransferReceipt{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadDreamTransferReceiptsLocked() != nil {
		return dreamTransferReceipt{}, false
	}
	for index := len(s.dreamTransferReceipts) - 1; index >= 0; index-- {
		receipt := s.dreamTransferReceipts[index]
		if receipt.Kind == kind && receipt.TransferID == transferID && verifyDreamTransferReceipt(receipt) &&
			s.identity != nil && receipt.Issuer.SigningKeyID == s.identity.SigningKeyID {
			return receipt, true
		}
	}
	return dreamTransferReceipt{}, false
}

type dreamExperienceTransferRecord struct {
	SchemaID            string              `json:"schema_id"`
	Version             int                 `json:"version"`
	TransferID          string              `json:"transfer_id"`
	GrantID             string              `json:"grant_id"`
	CallerPrincipal     string              `json:"caller_principal"`
	SourceProject       string              `json:"source_project"`
	SourcePassportID    string              `json:"source_passport_id"`
	SourceExperienceRef string              `json:"source_experience_ref"`
	SourceRecordDigest  string              `json:"source_record_digest"`
	DestinationProject  string              `json:"destination_project"`
	Mechanism           string              `json:"mechanism"`
	Mismatch            string              `json:"mismatch"`
	Prediction          string              `json:"prediction"`
	Falsifier           string              `json:"falsifier"`
	FrozenAt            string              `json:"frozen_at"`
	ExpiresAt           string              `json:"expires_at"`
	Status              string              `json:"status"`
	ResultStatus        string              `json:"result_status,omitempty"`
	ResultEvidence      []map[string]string `json:"result_evidence,omitempty"`
	ResultRecordedAt    string              `json:"result_recorded_at,omitempty"`
	HomeProjectEffect   string              `json:"home_project_effect"`
	RecordDigest        string              `json:"record_digest"`
}

func dreamTransferReceiptHasValidRecovery(receipt dreamTransferReceipt, kind string) bool {
	if receipt.RequestDigest == "" && receipt.RecoveryRecord == "" {
		return true // Legacy signed receipts remain verifiable but are not recoverable.
	}
	if !validDreamTransferRequestDigest(receipt.RequestDigest) || receipt.RecoveryRecord == "" ||
		len(receipt.RecoveryRecord) > dreamTransferRecoveryRecordMax {
		return false
	}
	var record dreamExperienceTransferRecord
	if strictJSONDecode([]byte(receipt.RecoveryRecord), &record) != nil || record.SchemaID != dreamExperienceTransferSchemaID ||
		record.Version != 1 || record.TransferID != receipt.TransferID || record.GrantID != receipt.GrantID ||
		record.CallerPrincipal != receipt.CallerPrincipal || record.SourceProject != receipt.SourceProject ||
		record.SourcePassportID != receipt.SourcePassportID || record.SourceExperienceRef != receipt.ExperienceRef ||
		record.SourceRecordDigest != receipt.SourceRecordHash || record.DestinationProject != receipt.DestinationProject ||
		record.RecordDigest != receipt.RecordDigest || record.RecordDigest != dreamTransferRecordDigest(record) {
		return false
	}
	grantExpiry, grantErr := time.Parse(time.RFC3339Nano, receipt.ExpiresAt)
	recordExpiry, recordErr := time.Parse(time.RFC3339Nano, record.ExpiresAt)
	if grantErr != nil || recordErr != nil || recordExpiry.After(grantExpiry) {
		return false
	}
	switch kind {
	case "freeze":
		return record.Status == "awaiting_result" && record.ResultStatus == "" && len(record.ResultEvidence) == 0 &&
			record.ResultRecordedAt == "" && receipt.ParentRecordDigest == "" && receipt.ResultStatus == ""
	case "result":
		return (record.Status == "confirmed" || record.Status == "narrowed" || record.Status == "rejected" || record.Status == "unproven") &&
			record.ResultStatus == record.Status && record.ResultRecordedAt != "" && receipt.ResultStatus == record.Status &&
			receipt.ParentRecordDigest == dreamTransferRecordDigestForFreezeRecord(record)
	default:
		return false
	}
}

func marshalDreamTransferRecoveryRecord(record dreamExperienceTransferRecord) (string, bool) {
	raw, err := json.Marshal(record)
	if err != nil || len(raw) > dreamTransferRecoveryRecordMax {
		return "", false
	}
	return string(raw), true
}

func (s *server) dreamTransferRecordSecretSafe(record dreamExperienceTransferRecord) bool {
	raw, err := json.Marshal(record)
	if err != nil || len(raw)+1 > dreamExperienceTransferMaxBytes {
		return false
	}
	payload := canonicalDreamWritePayload(map[string]any{
		"projectName": record.DestinationProject,
		"topicPath":   "contextlattice/dream-mode/transfers",
		"fileName":    "dream-transfers/" + record.TransferID + ".json",
		"content":     string(raw),
		"lifecycle":   "durable",
		"tags":        []any{"kind:dream_experience_transfer", "mode:manual_transfer"},
	}, "contextlattice/dream-mode/transfers", "dream-transfers/transfer")
	item, err := normalizeWritePayload("/memory/write", payload)
	if err != nil {
		return false
	}
	secured, filter, err := secureNormalizedWrite(item)
	s.recordWriteSecretFilter(filter, err != nil || filter.Findings > 0)
	return err == nil && filter.Findings == 0 && secured.content == item.content && item.content == string(raw)
}

func validDreamTransferRequestDigest(value string) bool {
	const prefix = "sha256:"
	digest, ok := strings.CutPrefix(value, prefix)
	return ok && len(digest) == 64 && digest == strings.ToLower(digest) && isHexDigest(digest)
}

func dreamTransferRecoveryRecord(receipt dreamTransferReceipt) (dreamExperienceTransferRecord, bool) {
	if !dreamTransferReceiptHasValidRecovery(receipt, receipt.Kind) || receipt.RecoveryRecord == "" {
		return dreamExperienceTransferRecord{}, false
	}
	var record dreamExperienceTransferRecord
	if strictJSONDecode([]byte(receipt.RecoveryRecord), &record) != nil {
		return dreamExperienceTransferRecord{}, false
	}
	return record, true
}

func (s *contextMeshStore) dreamTransferFreezeForRequest(grantID, caller, destination, requestDigest string) (dreamTransferReceipt, bool) {
	if s == nil {
		return dreamTransferReceipt{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadDreamTransferReceiptsLocked() != nil {
		return dreamTransferReceipt{}, false
	}
	for index := len(s.dreamTransferReceipts) - 1; index >= 0; index-- {
		receipt := s.dreamTransferReceipts[index]
		if receipt.Kind == "freeze" && receipt.GrantID == grantID && receipt.CallerPrincipal == caller &&
			receipt.DestinationProject == destination && receipt.RequestDigest == requestDigest &&
			verifyDreamTransferReceipt(receipt) && s.identity != nil && receipt.Issuer.SigningKeyID == s.identity.SigningKeyID {
			return receipt, true
		}
	}
	return dreamTransferReceipt{}, false
}

func dreamTransferProjectionHash(record dreamExperienceTransferRecord) string {
	content, err := json.Marshal(record)
	if err != nil || len(content)+1 > dreamExperienceTransferMaxBytes {
		return ""
	}
	return sha256Hex(canonicalMemoryContent(string(content)))
}

func dreamTransferFrozenProjection(record dreamExperienceTransferRecord) dreamExperienceTransferRecord {
	record.Status = "awaiting_result"
	record.ResultStatus = ""
	record.ResultEvidence = nil
	record.ResultRecordedAt = ""
	record.RecordDigest = ""
	record.RecordDigest = dreamTransferRecordDigest(record)
	return record
}

func (s *server) dreamTransferOwnerReceiptCurrent(ctx context.Context, receipt dreamTransferReceipt) bool {
	if s == nil || s.contextMesh == nil || !s.loadDreamTransferOwnerGrant(ctx, receipt.GrantID) {
		return false
	}
	grant, found, revoked := s.contextMesh.dreamTransferGrant(receipt.GrantID)
	now := time.Now().UTC()
	return found && !revoked && s.dreamTransferOwnerPermits(grant, now) && dreamTransferGrantMatches(grant, receipt)
}

func (s *server) buildDreamExperienceTransferResponse(
	ctx context.Context,
	headers http.Header,
	opts dreamModeOptions,
	raw any,
	endpoint string,
) (map[string]any, int) {
	result, status := s.applyDreamExperienceTransfer(ctx, headers, opts.Project, anyMap(raw))
	if status < 400 || anyToBool(result["ledger_recorded"]) {
		result["action"] = anyToString(anyMap(raw)["action"])
		result["request_digest"] = dreamTransferRequestDigest(opts.Project, anyMap(raw))
	}
	response := map[string]any{
		"ok":                        status < 400 && anyToString(result["status"]) != "abstained",
		"mode":                      "dream_transfer",
		"dream_available":           false,
		"intelligence_source":       "manual_transfer_record",
		"project":                   opts.Project,
		"goal":                      "",
		"query":                     "",
		"topic_path":                opts.TopicPath,
		"retrieval_mode":            opts.RetrievalMode,
		"agent_id":                  opts.AgentID,
		"hypotheses":                []any{},
		"experiments":               []any{},
		"evidence":                  map[string]any{"facts": []any{}, "results": []any{}, "citations": []any{}, "counts": map[string]any{}},
		"dream_experience_transfer": result,
		"writeback_required":        false,
		"persisted":                 status < 400 && anyToString(result["status"]) != "abstained",
		"warnings":                  []any{},
	}
	if status < 400 && anyToString(result["status"]) != "abstained" {
		transferInput := anyMap(raw)
		response["action"] = anyToString(transferInput["action"])
		response["request_digest"] = dreamTransferRequestDigest(opts.Project, transferInput)
	}
	if status >= 400 {
		response["ok"] = false
	}
	return attachDreamModeFormatContract(response, endpoint), status
}

func (s *server) applyDreamExperienceTransfer(ctx context.Context, headers http.Header, destination string, input map[string]any) (map[string]any, int) {
	if ctx == nil {
		ctx = context.Background()
	}
	if s == nil {
		return dreamTransferUnavailable()
	}
	if _, trusted := dreamTransferIdentityFromContext(ctx); !trusted {
		return dreamTransferUnavailable()
	}
	if s.contextMesh == nil || s.memoryStore == nil || !s.memoryStore.isEnabled() {
		return dreamTransferUnavailable()
	}
	destination = strings.TrimSpace(destination)
	cleanDestination, err := sanitizeMemoryProject(destination)
	if err != nil || destination == "" || cleanDestination != destination {
		return dreamTransferUnavailable()
	}
	action := anyToString(input["action"])
	switch action {
	case "grant", "revoke":
		// Grant administration requires a separate owner-only provisioning path.
		// The HTTP caller identity (including the shared orchestrator key) is not
		// evidence of source or destination project ownership.
		return dreamTransferUnavailable()
	case "freeze":
		if !dreamTransferOnlyKeys(input, "action", "grant_id", "source_passport_id", "experience_ref", "destination_project", "mechanism", "mismatch", "prediction", "falsifier", "expires_at") {
			return dreamTransferUnavailable()
		}
		unlockProvision := s.lockOptionalFrontierT1ProjectBoundary()
		defer unlockProvision()
		return s.freezeDreamExperienceTransfer(ctx, destination, input)
	case "record_result":
		if !dreamTransferOnlyKeys(input, "action", "transfer_id", "result", "evidence_refs") {
			return dreamTransferUnavailable()
		}
		unlockProvision := s.lockOptionalFrontierT1ProjectBoundary()
		defer unlockProvision()
		return s.recordDreamExperienceTransferResult(ctx, destination, input)
	default:
		return dreamTransferUnavailable()
	}
}

func (s *server) freezeDreamExperienceTransfer(ctx context.Context, destination string, input map[string]any) (map[string]any, int) {
	identity, trusted := dreamTransferIdentityFromContext(ctx)
	if !trusted {
		return dreamTransferUnavailable()
	}
	caller := identity.principal
	grantID := strings.TrimSpace(anyToString(input["grant_id"]))
	now := time.Now().UTC()
	if !s.loadDreamTransferOwnerGrant(ctx, grantID) {
		return dreamTransferUnavailable()
	}
	grant, found, revoked := s.contextMesh.dreamTransferGrant(grantID)
	passportID := strings.TrimSpace(anyToString(input["source_passport_id"]))
	experienceRef := strings.TrimSpace(anyToString(input["experience_ref"]))
	mechanism := strings.TrimSpace(anyToString(input["mechanism"]))
	mismatch := strings.TrimSpace(anyToString(input["mismatch"]))
	prediction := strings.TrimSpace(anyToString(input["prediction"]))
	falsifier := strings.TrimSpace(anyToString(input["falsifier"]))
	if !found || revoked || !s.dreamTransferOwnerPermits(grant, now) ||
		grant.DestinationProject != destination || grant.CallerPrincipal != caller ||
		grant.SourcePassportID != passportID || grant.ExperienceRef != experienceRef ||
		strings.TrimSpace(anyToString(input["destination_project"])) != destination ||
		passportID == "" || experienceRef == "" || len(experienceRef) > 160 ||
		mechanism == "" || len(mechanism) > 1000 || mismatch == "" || len(mismatch) > 1000 ||
		prediction == "" || len(prediction) > 1000 || falsifier == "" || len(falsifier) > 1000 {
		return dreamTransferUnavailable()
	}
	if requestedDestination := strings.TrimSpace(anyToString(input["destination_project"])); requestedDestination != destination {
		return dreamTransferUnavailable()
	}
	if s.contextPassports == nil || s.contextMesh == nil {
		return dreamTransferUnavailable()
	}
	passport, ok := s.contextPassports.get(passportID)
	if !ok || passport.Project != grant.SourceProject || len(verifyContextPassport(passport, time.Now().UTC(), true)) != 0 ||
		!dreamTransferHasSuccessfulImportReceipt(s.contextMesh, passport, s.contextPassports.identity) {
		return dreamTransferUnavailable()
	}
	claim, claimDigest, ok := dreamTransferConfirmedSourceClaim(passport, experienceRef)
	if !ok || claimDigest != grant.SourceRecordHash || strings.TrimSpace(anyToString(claim["mechanism"])) != mechanism {
		return dreamTransferUnavailable()
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(anyToString(input["expires_at"])))
	grantExpiresAt, grantExpiryErr := time.Parse(time.RFC3339Nano, grant.ExpiresAt)
	passportExpiresAt, passportExpiryErr := time.Parse(time.RFC3339Nano, passport.ExpiresAt)
	if err != nil || grantExpiryErr != nil || passportExpiryErr != nil || !expiresAt.After(now) ||
		expiresAt.After(now.Add(dreamExperienceTransferMaxTTL)) || expiresAt.After(grantExpiresAt) || expiresAt.After(passportExpiresAt) {
		return dreamTransferUnavailable()
	}
	createdAt := now.Format(time.RFC3339Nano)
	transferID := "dtx_" + randomHex(16)
	record := dreamExperienceTransferRecord{
		SchemaID: dreamExperienceTransferSchemaID, Version: 1, TransferID: transferID,
		GrantID: grantID, CallerPrincipal: caller,
		SourceProject: passport.Project, SourcePassportID: passport.PassportID,
		SourceExperienceRef: experienceRef, SourceRecordDigest: claimDigest,
		DestinationProject: destination, Mechanism: mechanism, Mismatch: mismatch,
		Prediction: prediction, Falsifier: falsifier, FrozenAt: createdAt,
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339Nano), Status: "awaiting_result",
		HomeProjectEffect: "unchanged",
	}
	record.RecordDigest = dreamTransferRecordDigest(record)
	requestDigest := dreamTransferRequestDigest(destination, input)
	if !validDreamTransferRequestDigest(requestDigest) || !s.dreamTransferRecordSecretSafe(record) {
		return dreamTransferUnavailable()
	}
	if existing, exists := s.contextMesh.dreamTransferFreezeForRequest(grantID, caller, destination, requestDigest); exists {
		return s.recoverDreamTransferFreeze(ctx, existing)
	}
	recoveryRecord, ok := marshalDreamTransferRecoveryRecord(record)
	if !ok {
		return dreamTransferUnavailable()
	}
	freezeReceipt := dreamTransferReceipt{
		ReceiptID: "dfr_" + randomHex(16), Kind: "freeze", GrantID: grantID, TransferID: transferID,
		SourceProject: record.SourceProject, SourcePassportID: passport.PassportID, ExperienceRef: experienceRef,
		SourceRecordHash: claimDigest, DestinationProject: destination, CallerPrincipal: caller,
		RecordDigest: record.RecordDigest, RequestDigest: requestDigest, RecoveryRecord: recoveryRecord, ExpiresAt: grant.ExpiresAt,
	}
	if err := s.contextMesh.appendDreamTransferReceipt(ctx, freezeReceipt, false); err != nil {
		if existing, exists := s.contextMesh.dreamTransferFreezeForRequest(grantID, caller, destination, requestDigest); exists {
			return s.recoverDreamTransferFreeze(ctx, existing)
		}
		return dreamTransferUnavailable()
	}
	if !s.dreamTransferOwnerReceiptCurrent(ctx, freezeReceipt) {
		return dreamTransferUnavailable()
	}
	writeResult, status := s.persistDreamExperienceTransferCAS(ctx, record, []string{dreamTransferProjectionHash(record)}, true)
	projectionPersisted := status < 400 && anyToBool(writeResult["ok"])
	if status == http.StatusConflict {
		return dreamTransferUnavailable()
	}
	return map[string]any{
		"schema_id": dreamExperienceTransferSchemaID, "status": "frozen",
		"transfer_id": transferID, "grant_id": grantID, "destination_project": destination,
		"mechanism": record.Mechanism, "mismatch": record.Mismatch,
		"prediction": record.Prediction, "falsifier": record.Falsifier,
		"record_content_hash": anyToString(writeResult["content_hash"]),
		"prediction_frozen":   true, "result_received": false,
		"ledger_recorded": true, "projection_persisted": projectionPersisted,
		"expires_at": record.ExpiresAt, "home_project_effect": "unchanged",
	}, dreamTransferProjectionStatus(projectionPersisted)
}

func (s *server) recoverDreamTransferFreeze(ctx context.Context, receipt dreamTransferReceipt) (map[string]any, int) {
	if receipt.RequestDigest == "" || !validDreamTransferRequestDigest(receipt.RequestDigest) ||
		s.contextMesh == nil {
		return dreamTransferUnavailable()
	}
	if _, terminal := s.contextMesh.dreamTransferReceipt("result", receipt.TransferID); terminal {
		return dreamTransferUnavailable()
	}
	record, ok := dreamTransferRecoveryRecord(receipt)
	if !ok || record.Status != "awaiting_result" || dreamTransferProjectionHash(record) == "" ||
		!s.dreamTransferRecordSecretSafe(record) || !s.dreamTransferOwnerReceiptCurrent(ctx, receipt) {
		return dreamTransferUnavailable()
	}
	current, contentHash, materialized := s.readDreamExperienceTransfer(ctx, record.DestinationProject, record.TransferID)
	if materialized {
		if current.RecordDigest != record.RecordDigest || contentHash != dreamTransferProjectionHash(record) {
			return dreamTransferUnavailable()
		}
		return dreamTransferFreezeResponse(record, receipt.GrantID, contentHash, true, true), http.StatusOK
	}
	writeResult, status := s.persistDreamExperienceTransferCAS(ctx, record, []string{dreamTransferProjectionHash(record)}, true)
	projectionPersisted := status < 400 && anyToBool(writeResult["ok"])
	if status == http.StatusConflict {
		return dreamTransferUnavailable()
	}
	return dreamTransferFreezeResponse(record, receipt.GrantID, anyToString(writeResult["content_hash"]), true, projectionPersisted),
		dreamTransferProjectionStatus(projectionPersisted)
}

func dreamTransferFreezeResponse(record dreamExperienceTransferRecord, grantID, contentHash string, ledgerRecorded, projectionPersisted bool) map[string]any {
	return map[string]any{
		"schema_id": dreamExperienceTransferSchemaID, "status": "frozen",
		"transfer_id": record.TransferID, "grant_id": grantID, "destination_project": record.DestinationProject,
		"mechanism": record.Mechanism, "mismatch": record.Mismatch,
		"prediction": record.Prediction, "falsifier": record.Falsifier,
		"record_content_hash": contentHash, "prediction_frozen": true, "result_received": false,
		"ledger_recorded": ledgerRecorded, "projection_persisted": projectionPersisted,
		"expires_at": record.ExpiresAt, "home_project_effect": "unchanged",
	}
}

func (s *server) recordDreamExperienceTransferResult(ctx context.Context, destination string, input map[string]any) (map[string]any, int) {
	transferID := strings.TrimSpace(anyToString(input["transfer_id"]))
	identity, trusted := dreamTransferIdentityFromContext(ctx)
	if !trusted {
		return dreamTransferUnavailable()
	}
	caller := identity.principal
	if !strings.HasPrefix(transferID, "dtx_") || len(transferID) > 64 {
		return dreamTransferUnavailable()
	}
	frozen, found := s.contextMesh.dreamTransferReceipt("freeze", transferID)
	if !found || frozen.CallerPrincipal != caller || frozen.DestinationProject != destination ||
		!s.dreamTransferOwnerReceiptCurrent(ctx, frozen) {
		return dreamTransferUnavailable()
	}
	requestDigest := dreamTransferRequestDigest(destination, input)
	if !validDreamTransferRequestDigest(requestDigest) {
		return dreamTransferUnavailable()
	}
	grant, found, revoked := s.contextMesh.dreamTransferGrant(frozen.GrantID)
	if !found || revoked || grant.CallerPrincipal != caller || grant.DestinationProject != destination ||
		!s.dreamTransferOwnerPermits(grant, time.Now().UTC()) || !dreamTransferGrantMatches(grant, frozen) {
		return dreamTransferUnavailable()
	}
	if resultReceipt, terminal := s.contextMesh.dreamTransferReceipt("result", transferID); terminal {
		return s.recoverDreamTransferResult(ctx, destination, input, frozen, grant, resultReceipt, requestDigest)
	}
	record, contentHash, ok := s.readDreamExperienceTransfer(ctx, destination, transferID)
	if !ok || record.Status != "awaiting_result" || record.CallerPrincipal != caller || record.DestinationProject != destination {
		return dreamTransferUnavailable()
	}
	now := time.Now().UTC()
	if record.GrantID != frozen.GrantID {
		return dreamTransferUnavailable()
	}
	expiresAt, expiryErr := time.Parse(time.RFC3339Nano, record.ExpiresAt)
	grant, found, revoked = s.contextMesh.dreamTransferGrant(record.GrantID)
	grantExpiresAt, grantExpiryErr := time.Parse(time.RFC3339Nano, grant.ExpiresAt)
	if expiryErr != nil || grantExpiryErr != nil || !now.Before(expiresAt) || !now.Before(grantExpiresAt) || revoked || !found ||
		!s.dreamTransferOwnerPermits(grant, now) ||
		!dreamTransferGrantMatchesRecord(grant, record) {
		return dreamTransferUnavailable()
	}
	passport, exists := s.contextPassports.get(record.SourcePassportID)
	if !exists || len(verifyContextPassport(passport, now, true)) != 0 || passport.Project != record.SourceProject ||
		!dreamTransferHasSuccessfulImportReceipt(s.contextMesh, passport, s.contextPassports.identity) {
		return dreamTransferUnavailable()
	}
	_, digest, sourceOK := dreamTransferConfirmedSourceClaim(passport, record.SourceExperienceRef)
	if !sourceOK || digest != record.SourceRecordDigest || digest != grant.SourceRecordHash {
		return dreamTransferUnavailable()
	}
	result := strings.TrimSpace(anyToString(input["result"]))
	switch result {
	case "confirmed", "narrowed", "rejected", "unproven":
	default:
		return dreamTransferUnavailable()
	}
	evidence, err := s.dreamTransferDestinationEvidence(ctx, destination, record.FrozenAt, anyToStringSlice(input["evidence_refs"]))
	if err != nil || (result != "unproven" && len(evidence) == 0) {
		return dreamTransferUnavailable()
	}
	frozenDigest := record.RecordDigest
	record.Status = result
	record.ResultStatus = result
	record.ResultEvidence = evidence
	record.ResultRecordedAt = now.Format(time.RFC3339Nano)
	record.HomeProjectEffect = "unchanged"
	record.RecordDigest = dreamTransferRecordDigest(record)
	if !s.dreamTransferRecordSecretSafe(record) {
		return dreamTransferUnavailable()
	}
	recoveryRecord, ok := marshalDreamTransferRecoveryRecord(record)
	if !ok {
		return dreamTransferUnavailable()
	}
	resultReceipt := dreamTransferReceipt{
		ReceiptID: "drr_" + randomHex(16), Kind: "result", GrantID: record.GrantID, TransferID: transferID,
		SourceProject: record.SourceProject, SourcePassportID: record.SourcePassportID,
		ExperienceRef: record.SourceExperienceRef, SourceRecordHash: record.SourceRecordDigest,
		DestinationProject: destination, CallerPrincipal: caller, RecordDigest: record.RecordDigest,
		ParentRecordDigest: frozenDigest, ResultStatus: result, RequestDigest: requestDigest,
		RecoveryRecord: recoveryRecord, ExpiresAt: grant.ExpiresAt,
	}
	if err := s.contextMesh.appendDreamTransferReceipt(ctx, resultReceipt, true); err != nil {
		if existing, exists := s.contextMesh.dreamTransferReceipt("result", transferID); exists {
			return s.recoverDreamTransferResult(ctx, destination, input, frozen, grant, existing, requestDigest)
		}
		return dreamTransferUnavailable()
	}
	if !s.dreamTransferOwnerReceiptCurrent(ctx, resultReceipt) {
		return dreamTransferUnavailable()
	}
	writeResult, status := s.persistDreamExperienceTransferCAS(ctx, record, []string{contentHash, dreamTransferProjectionHash(record)}, true)
	projectionPersisted := status < 400 && anyToBool(writeResult["ok"])
	if status == http.StatusConflict {
		return dreamTransferUnavailable()
	}
	return dreamTransferResultResponse(record, anyToString(writeResult["content_hash"]), contentHash, true, projectionPersisted),
		dreamTransferProjectionStatus(projectionPersisted)
}

func (s *server) recoverDreamTransferResult(
	ctx context.Context,
	destination string,
	input map[string]any,
	frozen, grant, receipt dreamTransferReceipt,
	requestDigest string,
) (map[string]any, int) {
	if receipt.RequestDigest == "" || receipt.RequestDigest != requestDigest ||
		!validDreamTransferRequestDigest(requestDigest) || receipt.DestinationProject != destination ||
		receipt.CallerPrincipal != frozen.CallerPrincipal || receipt.ParentRecordDigest != frozen.RecordDigest ||
		!s.dreamTransferOwnerReceiptCurrent(ctx, receipt) {
		return dreamTransferUnavailable()
	}
	record, ok := dreamTransferRecoveryRecord(receipt)
	if !ok || record.Status != receipt.ResultStatus || record.TransferID != frozen.TransferID ||
		record.GrantID != frozen.GrantID || record.DestinationProject != destination ||
		!dreamTransferGrantMatchesRecord(grant, record) || dreamTransferRecordDigestForFreezeRecord(record) != frozen.RecordDigest ||
		!s.dreamTransferRecordSecretSafe(record) {
		return dreamTransferUnavailable()
	}
	now := time.Now().UTC()
	expiresAt, expiryErr := time.Parse(time.RFC3339Nano, record.ExpiresAt)
	if expiryErr != nil || !now.Before(expiresAt) || !s.dreamTransferOwnerPermits(grant, now) {
		return dreamTransferUnavailable()
	}
	passport, exists := s.contextPassports.get(record.SourcePassportID)
	if !exists || len(verifyContextPassport(passport, now, true)) != 0 || passport.Project != record.SourceProject ||
		!dreamTransferHasSuccessfulImportReceipt(s.contextMesh, passport, s.contextPassports.identity) {
		return dreamTransferUnavailable()
	}
	_, sourceDigest, sourceOK := dreamTransferConfirmedSourceClaim(passport, record.SourceExperienceRef)
	if !sourceOK || sourceDigest != record.SourceRecordDigest || sourceDigest != grant.SourceRecordHash {
		return dreamTransferUnavailable()
	}
	result := strings.TrimSpace(anyToString(input["result"]))
	if result != receipt.ResultStatus {
		return dreamTransferUnavailable()
	}
	evidence, err := s.dreamTransferDestinationEvidence(ctx, destination, record.FrozenAt, anyToStringSlice(input["evidence_refs"]))
	if err != nil || !reflect.DeepEqual(evidence, record.ResultEvidence) || (result != "unproven" && len(evidence) == 0) {
		return dreamTransferUnavailable()
	}
	current, currentHash, currentOK := s.readDreamExperienceTransfer(ctx, destination, record.TransferID)
	if currentOK && current.RecordDigest == record.RecordDigest && currentHash == dreamTransferProjectionHash(record) {
		return dreamTransferUnavailable() // Terminal result is already materialized.
	}
	frozenProjection := dreamTransferFrozenProjection(record)
	if frozenProjection.RecordDigest != frozen.RecordDigest {
		return dreamTransferUnavailable()
	}
	if !s.dreamTransferOwnerReceiptCurrent(ctx, receipt) {
		return dreamTransferUnavailable()
	}
	writeResult, status := s.persistDreamExperienceTransferCAS(ctx, record,
		[]string{dreamTransferProjectionHash(frozenProjection), dreamTransferProjectionHash(record)}, true)
	projectionPersisted := status < 400 && anyToBool(writeResult["ok"])
	if status == http.StatusConflict {
		return dreamTransferUnavailable()
	}
	return dreamTransferResultResponse(record, anyToString(writeResult["content_hash"]), dreamTransferProjectionHash(frozenProjection), true, projectionPersisted),
		dreamTransferProjectionStatus(projectionPersisted)
}

func dreamTransferResultResponse(
	record dreamExperienceTransferRecord,
	contentHash, previousContentHash string,
	ledgerRecorded, projectionPersisted bool,
) map[string]any {
	return map[string]any{
		"schema_id": dreamExperienceTransferSchemaID, "status": record.Status,
		"transfer_id": record.TransferID, "destination_project": record.DestinationProject,
		"mechanism": record.Mechanism, "mismatch": record.Mismatch,
		"prediction": record.Prediction, "falsifier": record.Falsifier,
		"prediction_frozen": true, "result_received": true,
		"result_evidence": record.ResultEvidence, "record_content_hash": contentHash,
		"ledger_recorded": ledgerRecorded, "projection_persisted": projectionPersisted,
		"home_project_effect": "unchanged", "previous_record_content_hash": previousContentHash,
	}
}

func dreamTransferProjectionStatus(persisted bool) int {
	if persisted {
		return http.StatusOK
	}
	return http.StatusServiceUnavailable
}

func (s *server) persistDreamExperienceTransferCAS(
	ctx context.Context,
	record dreamExperienceTransferRecord,
	expectedHashes []string,
	expectAbsent bool,
) (map[string]any, int) {
	if ctx != nil && ctx.Err() != nil {
		return map[string]any{"ok": false}, http.StatusServiceUnavailable
	}
	content, err := json.Marshal(record)
	if err != nil || len(content)+1 > dreamExperienceTransferMaxBytes || s == nil || s.memoryStore == nil {
		return map[string]any{"ok": false}, http.StatusServiceUnavailable
	}
	fileName := "dream-transfers/" + record.TransferID + ".json"
	payload := canonicalDreamWritePayload(map[string]any{
		"projectName": record.DestinationProject,
		"topicPath":   "contextlattice/dream-mode/transfers",
		"fileName":    fileName,
		"content":     string(content),
		"lifecycle":   "durable",
		"tags":        []any{"kind:dream_experience_transfer", "mode:manual_transfer"},
	}, "contextlattice/dream-mode/transfers", "dream-transfers/transfer")
	item, err := normalizeWritePayload("/memory/write", payload)
	if err != nil {
		return map[string]any{"ok": false}, http.StatusServiceUnavailable
	}
	secretFilter := writeSecretFilterResult{Mode: writeSecretsStorageMode()}
	item, secretFilter, err = secureNormalizedWrite(item)
	s.recordWriteSecretFilter(secretFilter, err != nil)
	item.lifecycle = "durable"
	if err != nil || s.writePolicy.validateWrite(item) != nil {
		return map[string]any{"ok": false}, http.StatusServiceUnavailable
	}
	var entry memoryStoreEntry
	var deduped bool
	write := func(expected string, absent bool) error {
		entry, deduped, err = s.memoryStore.putDreamTransferProjectionCAS(item, expected, absent)
		return err
	}
	err = errMemoryStoreWritePrecondition
	for _, expected := range expectedHashes {
		if expected == "" || !isHexDigest(expected) {
			continue
		}
		err = write(expected, false)
		if !errors.Is(err, errMemoryStoreWritePrecondition) {
			break
		}
	}
	if errors.Is(err, errMemoryStoreWritePrecondition) && expectAbsent {
		err = write("", true)
	}
	if err != nil {
		if errors.Is(err, errMemoryStoreWritePrecondition) {
			return map[string]any{"ok": false, "precondition_failed": true}, http.StatusConflict
		}
		return map[string]any{"ok": false}, http.StatusServiceUnavailable
	}
	vectorFanout, warnings := s.handleNativeVectorWriteFanout(item, entry.EventID)
	fanout := map[string]any{"go_memory_store": "succeeded", "python_backend": "disabled"}
	for source, status := range vectorFanout {
		fanout[source] = status
	}
	return attachWriteSecretFilter(map[string]any{
		"ok":               true,
		"tool":             normalizeToolPath("/memory/dream"),
		"event_id":         entry.EventID,
		"source":           "go_memory_store",
		"lifecycle":        item.lifecycle,
		"content_hash":     entry.ContentHash,
		"content_ref":      entry.ContentRef,
		"warnings":         warnings,
		"rollup_buffered":  true,
		"deduped":          deduped,
		"fanout":           fanout,
		"writeback_source": "dream_mode",
	}, secretFilter), http.StatusOK
}

func (s *server) readDreamExperienceTransfer(ctx context.Context, destination, transferID string) (dreamExperienceTransferRecord, string, bool) {
	fileName := "dream-transfers/" + transferID + ".json"
	result, err := s.memoryStore.fetchByMemoryID(ctx, destination+"::"+fileName)
	if err != nil {
		return dreamExperienceTransferRecord{}, "", false
	}
	memory := anyMap(result["memory"])
	content := anyToString(memory["content"])
	observedHash := sha256Hex(content)
	s.memoryStore.mu.RLock()
	ledgerHash := s.memoryStore.latestHash[memoryStoreKey(destination, fileName)]
	s.memoryStore.mu.RUnlock()
	if observedHash == "" || ledgerHash == "" || observedHash != ledgerHash || len(content) > dreamExperienceTransferMaxBytes {
		return dreamExperienceTransferRecord{}, "", false
	}
	var record dreamExperienceTransferRecord
	if strictJSONDecode([]byte(content), &record) != nil || record.SchemaID != dreamExperienceTransferSchemaID ||
		record.Version != 1 || record.TransferID != transferID || record.DestinationProject != destination ||
		record.RecordDigest != dreamTransferRecordDigest(record) {
		return dreamExperienceTransferRecord{}, "", false
	}
	freeze, freezeOK := s.contextMesh.dreamTransferReceipt("freeze", transferID)
	if !freezeOK || freeze.RecordDigest == "" || freeze.RecordDigest != dreamTransferRecordDigestForStatus(record) ||
		freeze.GrantID != record.GrantID || freeze.SourceProject != record.SourceProject ||
		freeze.SourcePassportID != record.SourcePassportID || freeze.ExperienceRef != record.SourceExperienceRef ||
		freeze.SourceRecordHash != record.SourceRecordDigest || freeze.DestinationProject != record.DestinationProject ||
		freeze.CallerPrincipal != record.CallerPrincipal {
		return dreamExperienceTransferRecord{}, "", false
	}
	if record.Status != "awaiting_result" {
		resultReceipt, resultOK := s.contextMesh.dreamTransferReceipt("result", transferID)
		if !resultOK || resultReceipt.RecordDigest != record.RecordDigest || resultReceipt.ParentRecordDigest != freeze.RecordDigest ||
			resultReceipt.ResultStatus != record.Status || resultReceipt.GrantID != record.GrantID ||
			resultReceipt.DestinationProject != destination || resultReceipt.CallerPrincipal != record.CallerPrincipal {
			return dreamExperienceTransferRecord{}, "", false
		}
	}
	return record, observedHash, true
}

func dreamTransferRecordDigestForStatus(record dreamExperienceTransferRecord) string {
	if record.Status == "awaiting_result" {
		return record.RecordDigest
	}
	return dreamTransferRecordDigestForFreezeRecord(record)
}

func dreamTransferRecordDigestForFreezeRecord(record dreamExperienceTransferRecord) string {
	record.Status = "awaiting_result"
	record.ResultStatus = ""
	record.ResultEvidence = nil
	record.ResultRecordedAt = ""
	record.RecordDigest = ""
	return dreamTransferRecordDigest(record)
}

func dreamTransferConfirmedSourceClaim(passport contextPassport, experienceRef string) (map[string]any, string, bool) {
	var selected map[string]any
	for _, claim := range passport.Claims {
		if firstNonEmptyStrings(anyToString(claim["experience_id"]), anyToString(claim["portable_id"])) != experienceRef {
			continue
		}
		if anyToString(claim["kind"]) != "conditional_experience" || anyToString(claim["status"]) != "confirmed" ||
			strings.TrimSpace(anyToString(claim["mechanism"])) == "" || strings.TrimSpace(anyToString(claim["observed_result"])) == "" {
			return nil, "", false
		}
		referenced := anyToStringSlice(claim["evidence_refs"])
		if len(referenced) == 0 || !dreamTransferEvidenceRefsPresent(passport.Evidence, referenced) {
			return nil, "", false
		}
		if selected != nil {
			return nil, "", false
		}
		selected = claim
	}
	if selected == nil {
		return nil, "", false
	}
	encoded, err := json.Marshal(selected)
	if err != nil {
		return nil, "", false
	}
	hash := sha256.Sum256(encoded)
	return selected, "sha256:" + hex.EncodeToString(hash[:]), true
}

func dreamTransferEvidenceRefsPresent(evidence []map[string]any, refs []string) bool {
	available := map[string]struct{}{}
	for _, item := range evidence {
		id := firstNonEmptyStrings(anyToString(item["evidence_id"]), anyToString(item["portable_id"]))
		if id != "" {
			available[id] = struct{}{}
		}
	}
	for _, ref := range refs {
		if _, ok := available[strings.TrimSpace(ref)]; !ok {
			return false
		}
	}
	return true
}

func dreamTransferHasSuccessfulImportReceipt(mesh *contextMeshStore, passport contextPassport, identity *contextIdentityKeys) bool {
	if mesh == nil || identity == nil || !mesh.enabled {
		return false
	}
	mesh.mu.RLock()
	defer mesh.mu.RUnlock()
	for _, receipt := range mesh.receipts {
		if receipt.SchemaID == contextMeshReceiptSchemaID && receipt.Version == 1 &&
			receipt.PassportID == passport.PassportID && receipt.Project == passport.Project &&
			receipt.RecipientKeyID == identity.MeshKeyID && !receipt.Conflict && receipt.Action != "" &&
			(receipt.Action == "record_root" || receipt.Action == "advance" || receipt.Action == "idempotent") {
			return true
		}
	}
	return false
}

func (s *server) dreamTransferDestinationEvidence(ctx context.Context, destination, frozenAt string, refs []string) ([]map[string]string, error) {
	cutoff, err := time.Parse(time.RFC3339Nano, frozenAt)
	if err != nil {
		return nil, errors.New("frozen observation boundary unavailable")
	}
	if len(refs) > 8 {
		return nil, errors.New("destination evidence limit exceeded")
	}
	seen := map[string]struct{}{}
	out := make([]map[string]string, 0, len(refs))
	for _, raw := range refs {
		ref := strings.TrimSpace(raw)
		if ref == "" || !strings.HasPrefix(ref, destination+"::") || strings.HasPrefix(ref, destination+"::dream-transfers/") {
			return nil, errors.New("destination evidence must stay within the destination project")
		}
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		item, err := s.memoryStore.fetchByMemoryID(ctx, ref)
		if err != nil {
			return nil, errors.New("destination evidence is unavailable")
		}
		memory := anyMap(item["memory"])
		event, digest := anyToString(memory["event_id"]), anyToString(memory["content_hash"])
		recorded := anyToString(memory["event_recorded_at"])
		at, err := time.Parse(time.RFC3339Nano, recorded)
		fileName := anyToString(memory["file"])
		if anyToString(memory["project"]) != destination || ref != destination+"::"+fileName ||
			strings.HasPrefix(strings.ToLower(fileName), "dream-transfers/") || event == "" || !isHexDigest(digest) ||
			err != nil || !at.After(cutoff) || at.After(time.Now().UTC()) {
			return nil, errors.New("destination evidence must have a proven post-freeze write")
		}
		out = append(out, map[string]string{"memory_id": ref, "event_id": event, "content_hash": digest, "recorded_at": recorded})
	}
	return out, nil
}

func dreamTransferOnlyKeys(input map[string]any, allowed ...string) bool {
	set := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		set[key] = struct{}{}
	}
	for key := range input {
		if _, ok := set[key]; !ok {
			return false
		}
	}
	return true
}

func dreamTransferRecordDigest(record dreamExperienceTransferRecord) string {
	record.RecordDigest = ""
	encoded, err := json.Marshal(record)
	if err != nil {
		return ""
	}
	return "sha256:" + sha256Hex(string(encoded))
}

func dreamExperienceTransferAbstention(reason string) map[string]any {
	return map[string]any{
		"schema_id": dreamExperienceTransferSchemaID, "status": "abstained",
		"reason": reason, "prediction_frozen": false, "result_received": false,
		"home_project_effect": "unchanged",
	}
}

func dreamTransferUnavailable() (map[string]any, int) {
	return dreamExperienceTransferAbstention("transfer_unavailable"), http.StatusNotFound
}

func dreamTransferGrantMatchesRecord(grant dreamTransferReceipt, record dreamExperienceTransferRecord) bool {
	return grant.GrantID == record.GrantID && grant.SourceProject == record.SourceProject &&
		grant.SourcePassportID == record.SourcePassportID && grant.ExperienceRef == record.SourceExperienceRef &&
		grant.SourceRecordHash == record.SourceRecordDigest && grant.DestinationProject == record.DestinationProject &&
		grant.CallerPrincipal == record.CallerPrincipal
}
