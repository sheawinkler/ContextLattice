package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

const (
	frontierT7GrantsPath    = "/memory/portable-continuation/grants"
	frontierT7ImportsPath   = "/memory/portable-continuation/imports"
	frontierT7ManifestsPath = "/memory/portable-continuation/manifests"
	frontierT7TelemetryPath = "/telemetry/portable-continuation"
)

type frontierT7GrantRouteRequest struct {
	Operation             string                 `json:"operation"`
	Subject               frontierT7GrantSubject `json:"subject"`
	Project               string                 `json:"project"`
	Topics                []string               `json:"topics"`
	DataClasses           []string               `json:"data_classes"`
	Actions               []string               `json:"actions"`
	Purpose               string                 `json:"purpose"`
	UsageLimit            int                    `json:"usage_limit"`
	ParentGrantID         string                 `json:"parent_grant_id"`
	DelegationDepth       int                    `json:"delegation_depth"`
	Approvers             []string               `json:"approvers"`
	KeyEpoch              int                    `json:"key_epoch"`
	RecipientKeyID        string                 `json:"recipient_key_id"`
	NotBefore             string                 `json:"not_before"`
	ExpiresAt             string                 `json:"expires_at"`
	GrantID               string                 `json:"grant_id"`
	Topic                 string                 `json:"topic"`
	DataClass             string                 `json:"data_class"`
	Action                string                 `json:"action"`
	SubjectSnapshotDigest string                 `json:"subject_snapshot_digest"`
	Consume               bool                   `json:"consume"`
	Reason                string                 `json:"reason"`
}

type frontierT7ImportRouteRequest struct {
	Operation               string                   `json:"operation"`
	Project                 string                   `json:"project"`
	Records                 []frontierT7ImportRecord `json:"records"`
	BatchSize               int                      `json:"batch_size"`
	PlanID                  string                   `json:"plan_id"`
	BatchIndex              int                      `json:"batch_index"`
	ExternalExecutionDigest string                   `json:"external_execution_digest"`
}

type frontierT7ManifestRouteRequest struct {
	Operation                   string   `json:"operation"`
	Project                     string   `json:"project"`
	PassportID                  string   `json:"passport_id"`
	PassportDigest              string   `json:"passport_digest"`
	EvidenceIdentityDigest      string   `json:"evidence_identity_digest,omitempty"`
	LineageDigest               string   `json:"lineage_digest"`
	CheckpointDigest            string   `json:"checkpoint_digest"`
	LifecycleReceiptDigest      string   `json:"lifecycle_receipt_digest"`
	UnresolvedObligationDigests []string `json:"unresolved_obligation_digests"`
	RepositoryConstraintDigest  string   `json:"repository_constraint_digest"`
	DestinationSessionDigest    string   `json:"destination_session_digest"`
	GrantID                     string   `json:"grant_id"`
	MeshGrantID                 string   `json:"mesh_grant_id"`
	Transport                   string   `json:"transport"`
	ExpiresAt                   string   `json:"expires_at"`
	Envelope                    any      `json:"envelope"`
	ExpectedLineageDigest       string   `json:"expected_lineage_digest"`
	Topic                       string   `json:"topic"`
	DataClass                   string   `json:"data_class"`
	Purpose                     string   `json:"purpose"`
	SubjectSnapshotDigest       string   `json:"subject_snapshot_digest"`
	KeyEpoch                    int      `json:"key_epoch"`
}

func frontierT7DecodeRoutePayload(payload map[string]any, destination any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return strictJSONDecode(raw, destination)
}

func frontierT7ParseRouteTime(value, field string, fallback time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" && !fallback.IsZero() {
		return fallback.UTC(), nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, errors.New(field + " must be RFC3339Nano")
	}
	return parsed.UTC(), nil
}

func frontierT7AttachFormatContract(schemaID string, payload any, surface, route string) map[string]any {
	raw, err := json.Marshal(payload)
	if err != nil {
		return attachPayloadFormatContract(schemaID, map[string]any{"schema_id": schemaID}, "", surface, route)
	}
	result := map[string]any{}
	if err := json.Unmarshal(raw, &result); err != nil {
		result = map[string]any{"schema_id": schemaID}
	}
	return attachPayloadFormatContract(schemaID, result, "", surface, route)
}

func (s *server) frontierT7Authorized(w http.ResponseWriter, r *http.Request) bool {
	if s == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "runtime_unavailable"})
		return false
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return false
	}
	if s.frontierT7 == nil || !s.frontierT7.enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "portable_continuation_unavailable"})
		return false
	}
	return true
}

func (s *server) frontierT7WriteError(w http.ResponseWriter, status int, code string, err error) {
	payload := map[string]any{"ok": false, "error": code}
	if err != nil {
		payload["detail"] = clipText(err.Error(), 500)
	}
	writeJSON(w, status, payload)
}

func (s *server) frontierT7GrantsRoute(w http.ResponseWriter, r *http.Request) {
	if !s.frontierT7Authorized(w, r) {
		return
	}
	if r.Method == http.MethodGet {
		payload := frontierT7AttachFormatContract(frontierT7StatusSchemaID, s.frontierT7.snapshot(time.Now().UTC()), "portable_continuation_grants_status", r.URL.Path)
		writeJSON(w, http.StatusOK, payload)
		return
	}
	if r.Method != http.MethodPost {
		s.frontierT7WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", nil)
		return
	}
	payload, err := readOptionalJSONBody(r)
	if err != nil {
		s.frontierT7WriteError(w, http.StatusBadRequest, "invalid_json", err)
		return
	}
	var request frontierT7GrantRouteRequest
	if err := frontierT7DecodeRoutePayload(payload, &request); err != nil {
		s.frontierT7WriteError(w, http.StatusBadRequest, "invalid_grant_request", err)
		return
	}
	now := time.Now().UTC()
	switch strings.ToLower(strings.TrimSpace(request.Operation)) {
	case "create":
		notBefore, err := frontierT7ParseRouteTime(request.NotBefore, "not_before", now)
		if err != nil {
			s.frontierT7WriteError(w, http.StatusBadRequest, "invalid_grant_request", err)
			return
		}
		expiresAt, err := frontierT7ParseRouteTime(request.ExpiresAt, "expires_at", time.Time{})
		if err != nil {
			s.frontierT7WriteError(w, http.StatusBadRequest, "invalid_grant_request", err)
			return
		}
		var parent *frontierT7CollaborativeGrant
		if request.ParentGrantID != "" {
			value, exists := s.frontierT7.getGrant(request.ParentGrantID)
			if !exists {
				s.frontierT7WriteError(w, http.StatusNotFound, "parent_grant_not_found", nil)
				return
			}
			parent = &value
		}
		grant, err := s.frontierT7.createGrant(frontierT7GrantCreateRequest{
			Subject: request.Subject, Project: request.Project, Topics: request.Topics, DataClasses: request.DataClasses,
			Actions: request.Actions, Purpose: request.Purpose, UsageLimit: request.UsageLimit, Parent: parent,
			DelegationDepth: request.DelegationDepth, Approvers: request.Approvers, KeyEpoch: request.KeyEpoch,
			RecipientKeyID: request.RecipientKeyID, NotBefore: notBefore, ExpiresAt: expiresAt,
		}, now)
		if err != nil {
			s.frontierT7WriteError(w, http.StatusUnprocessableEntity, "grant_create_failed", err)
			return
		}
		response := frontierT7AttachFormatContract(frontierT7CollaborativeGrantSchemaID, grant, "portable_continuation_grant_create", r.URL.Path)
		writeJSON(w, http.StatusOK, response)
	case "authorize":
		decision, err := s.frontierT7.authorize(request.GrantID, frontierT7GrantUseRequest{Project: request.Project, Topic: request.Topic, DataClass: request.DataClass, Action: request.Action, Purpose: request.Purpose, RecipientKeyID: request.RecipientKeyID, SubjectSnapshotDigest: request.SubjectSnapshotDigest, KeyEpoch: request.KeyEpoch}, request.Consume, now)
		if err != nil {
			s.frontierT7WriteError(w, http.StatusUnprocessableEntity, "grant_authorization_failed", err)
			return
		}
		response := frontierT7AttachFormatContract(frontierT7GrantDecisionSchemaID, decision, "portable_continuation_grant_authorize", r.URL.Path)
		writeJSON(w, http.StatusOK, response)
	case "revoke":
		revocation, err := s.frontierT7.revokeGrant(request.GrantID, request.Reason, now)
		if err != nil {
			s.frontierT7WriteError(w, http.StatusUnprocessableEntity, "grant_revoke_failed", err)
			return
		}
		response := frontierT7AttachFormatContract(frontierT7GrantRevocationSchemaID, revocation, "portable_continuation_grant_revoke", r.URL.Path)
		writeJSON(w, http.StatusOK, response)
	default:
		s.frontierT7WriteError(w, http.StatusBadRequest, "unsupported_grant_operation", nil)
	}
}

func (s *server) frontierT7ImportsRoute(w http.ResponseWriter, r *http.Request) {
	if !s.frontierT7Authorized(w, r) {
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, frontierT7AttachFormatContract(frontierT7StatusSchemaID, s.frontierT7.snapshot(time.Now().UTC()), "portable_continuation_imports_status", r.URL.Path))
		return
	}
	if r.Method != http.MethodPost {
		s.frontierT7WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", nil)
		return
	}
	payload, err := readOptionalJSONBody(r)
	if err != nil {
		s.frontierT7WriteError(w, http.StatusBadRequest, "invalid_json", err)
		return
	}
	var request frontierT7ImportRouteRequest
	if err := frontierT7DecodeRoutePayload(payload, &request); err != nil {
		s.frontierT7WriteError(w, http.StatusBadRequest, "invalid_import_request", err)
		return
	}
	now := time.Now().UTC()
	switch strings.ToLower(strings.TrimSpace(request.Operation)) {
	case "plan":
		plan, err := s.frontierT7.buildImportPlan(request.Project, request.Records, request.BatchSize, now)
		if err != nil {
			s.frontierT7WriteError(w, http.StatusUnprocessableEntity, "import_plan_failed", err)
			return
		}
		response := frontierT7AttachFormatContract(frontierT7ImportPlanSchemaID, plan, "portable_continuation_import_plan", r.URL.Path)
		writeJSON(w, http.StatusOK, response)
	case "commit":
		receipt, err := s.frontierT7.commitImport(request.PlanID, request.BatchIndex, request.ExternalExecutionDigest, now)
		if err != nil {
			s.frontierT7WriteError(w, http.StatusUnprocessableEntity, "import_commit_failed", err)
			return
		}
		response := frontierT7AttachFormatContract(frontierT7ImportReceiptSchemaID, receipt, "portable_continuation_import_commit", r.URL.Path)
		writeJSON(w, http.StatusOK, response)
	default:
		s.frontierT7WriteError(w, http.StatusBadRequest, "unsupported_import_operation", nil)
	}
}

func (s *server) frontierT7ManifestsRoute(w http.ResponseWriter, r *http.Request) {
	if !s.frontierT7Authorized(w, r) {
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, frontierT7AttachFormatContract(frontierT7StatusSchemaID, s.frontierT7.snapshot(time.Now().UTC()), "portable_continuation_manifests_status", r.URL.Path))
		return
	}
	if r.Method != http.MethodPost {
		s.frontierT7WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", nil)
		return
	}
	payload, err := readOptionalJSONBody(r)
	if err != nil {
		s.frontierT7WriteError(w, http.StatusBadRequest, "invalid_json", err)
		return
	}
	var request frontierT7ManifestRouteRequest
	if err := frontierT7DecodeRoutePayload(payload, &request); err != nil {
		s.frontierT7WriteError(w, http.StatusBadRequest, "invalid_manifest_request", err)
		return
	}
	if rawIdentity, present := payload["evidence_identity_digest"]; present {
		if rawIdentity == nil {
			s.frontierT7WriteError(w, http.StatusBadRequest, "invalid_manifest_request", errors.New("evidence_identity_digest cannot be null"))
			return
		}
		if value, ok := rawIdentity.(string); ok && strings.TrimSpace(value) == "" {
			s.frontierT7WriteError(w, http.StatusBadRequest, "invalid_manifest_request", errors.New("evidence_identity_digest cannot be empty"))
			return
		}
	}
	now := time.Now().UTC()
	switch strings.ToLower(strings.TrimSpace(request.Operation)) {
	case "create":
		grant, exists := s.frontierT7.getGrant(request.GrantID)
		if !exists {
			s.frontierT7WriteError(w, http.StatusNotFound, "grant_not_found", nil)
			return
		}
		expiresAt, err := frontierT7ParseRouteTime(request.ExpiresAt, "expires_at", time.Time{})
		if err != nil {
			s.frontierT7WriteError(w, http.StatusBadRequest, "invalid_manifest_request", err)
			return
		}
		manifest, err := s.frontierT7.prepareManifest(frontierT7ContinuationRequest{Project: request.Project, PassportID: request.PassportID, PassportDigest: request.PassportDigest, EvidenceIdentityDigest: request.EvidenceIdentityDigest, LineageDigest: request.LineageDigest, CheckpointDigest: request.CheckpointDigest, LifecycleReceiptDigest: request.LifecycleReceiptDigest, UnresolvedObligationDigests: request.UnresolvedObligationDigests, RepositoryConstraintDigest: request.RepositoryConstraintDigest, DestinationSessionDigest: request.DestinationSessionDigest, RecipientKeyID: grant.RecipientKeyID, Grant: grant, Transport: request.Transport, ExpiresAt: expiresAt}, now)
		if err != nil {
			s.frontierT7WriteError(w, http.StatusUnprocessableEntity, "manifest_create_failed", err)
			return
		}
		envelope, err := frontierT7CreateContinuationEnvelope(s.contextMesh, manifest, grant, request.MeshGrantID, now)
		if err != nil {
			s.frontierT7WriteError(w, http.StatusUnprocessableEntity, "manifest_seal_failed", err)
			return
		}
		if err := s.frontierT7.recordCreatedManifest(manifest, now); err != nil {
			s.frontierT7WriteError(w, http.StatusServiceUnavailable, "manifest_record_failed", err)
			return
		}
		response := frontierT7AttachFormatContract(frontierT7ContinuationEnvelopeSchemaID, envelope, "portable_continuation_manifest_create", r.URL.Path)
		writeJSON(w, http.StatusOK, response)
	case "reconcile":
		envelope, err := decodeFrontierT7ContinuationEnvelope(request.Envelope)
		if err != nil {
			s.frontierT7WriteError(w, http.StatusBadRequest, "invalid_continuation_envelope", err)
			return
		}
		manifest, grant, err := frontierT7DecryptContinuationEnvelope(s.contextMesh, envelope, now)
		if err != nil {
			s.frontierT7WriteError(w, http.StatusUnprocessableEntity, "continuation_decrypt_failed", err)
			return
		}
		result, err := s.frontierT7.reconcileManifest(manifest, grant, request.ExpectedLineageDigest, frontierT7ContinuationAuthorization{Topic: request.Topic, DataClass: request.DataClass, Purpose: request.Purpose, SubjectSnapshotDigest: request.SubjectSnapshotDigest, KeyEpoch: request.KeyEpoch}, now)
		if err != nil {
			s.frontierT7WriteError(w, http.StatusServiceUnavailable, "continuation_reconcile_failed", err)
			return
		}
		status := http.StatusOK
		if !result.Accepted {
			status = http.StatusConflict
		}
		response := frontierT7AttachFormatContract(frontierT7ReconciliationSchemaID, result, "portable_continuation_manifest_reconcile", r.URL.Path)
		writeJSON(w, status, response)
	default:
		s.frontierT7WriteError(w, http.StatusBadRequest, "unsupported_manifest_operation", nil)
	}
}

func (s *server) frontierT7TelemetryRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.frontierT7WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", nil)
		return
	}
	if !s.frontierT7Authorized(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, frontierT7AttachFormatContract(frontierT7StatusSchemaID, s.frontierT7.snapshot(time.Now().UTC()), "portable_continuation_status", r.URL.Path))
}
