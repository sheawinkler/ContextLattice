package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// The task-delivery contracts are deliberately separate from the legacy
// agent_task_result.v1 envelope.  Legacy readers can continue to consume the
// latter while every new mutation carries an explicit lifecycle contract.
const (
	agentTaskManifestContractID               = "agent_task_manifest.v1"
	agentTaskRecipientContractID              = "agent_task_recipient.v1"
	agentTaskLeaseContractID                  = "agent_task_lease.v1"
	agentTaskAttemptContractID                = "agent_task_attempt.v1"
	agentTaskResultManifestContractID         = "agent_task_result_manifest.v1"
	agentTaskWritebackIntentContractID        = "agent_task_writeback_intent.v1"
	agentTaskPublicationContractID            = "agent_task_publication.v1"
	agentTaskPublicationReconciliationID      = "agent_task_publication_reconciliation.v1"
	agentTaskPublicationReceiptID             = "agent_task_publication_receipt.v1"
	agentTaskCleanupAuthorizationID           = "agent_task_cleanup_authorization.v1"
	agentTaskCleanupReceiptID                 = "agent_task_cleanup_receipt.v1"
	agentTaskArtifactContractID               = "agent_task_artifact.v1"
	agentTaskDeliveryContractID               = "agent_task_delivery.v1"
	agentTaskReviewerClaimContractID          = "agent_task_reviewer_claim.v1"
	agentTaskReviewContractID                 = "agent_task_review.v1"
	agentTaskRevisionEnvelopeContractID       = "agent_task_revision_envelope.v1"
	agentTaskApprovalContractID               = "agent_task_approval.v1"
	agentTaskIntegrationContractID            = "agent_task_integration.v1"
	agentTaskBlockingAnswerContractID         = "agent_task_blocking_answer.v1"
	agentTaskScheduleContractID               = "agent_task_schedule.v1"
	agentExecutionSurfaceContractID           = "agent_execution_surface.v1"
	agentWorkerIdentityRegistrationContractID = "agent_worker_identity_registration.v1"
	agentWorkerIdentityReadbackContractID     = "agent_worker_identity_readback.v1"
	agentWorkerIdentityUpdateContractID       = "agent_worker_identity_update.v1"
	agentWorkerIdentityAckContractID          = "agent_worker_identity_ack.v1"
	agentWorkerIdentityRetireContractID       = "agent_worker_identity_retire.v1"
	agentWorkerIdentityRetirementReceiptID    = "agent_worker_identity_retirement_receipt.v1"
)

const (
	agentTaskContextPackMaxBytes   = 256 * 1024
	agentTaskEventMaxBytes         = 16 * 1024
	agentTaskSummaryMaxBytes       = 32 * 1024
	agentTaskNotificationMaxBytes  = 16 * 1024
	agentTaskArtifactMaxBytes      = 256 * 1024 * 1024
	agentTaskArtifactSetMaxBytes   = 1024 * 1024 * 1024
	agentTaskMaxBlockingQuestions  = 4
	agentTaskMaxArtifactReferences = 32
	agentTaskMaxEvents             = 1000
	agentTaskDefaultRetentionDays  = 7
	agentTaskStructuredMaxDepth    = 32
	agentTaskStructuredMaxNodes    = 20000
	agentTaskStructuredMaxKeyBytes = 256
	agentTaskIDCollisionRetries    = 16
)

var agentTaskIDCounter atomic.Uint64
var agentTaskProcessStarted = time.Now().UTC().UnixNano()

type agentTaskDeliveryLimits struct {
	ContextPackBytes      int   `json:"context_pack_bytes"`
	EventBytes            int   `json:"event_bytes"`
	SummaryBytes          int   `json:"summary_bytes"`
	NotificationBytes     int   `json:"notification_bytes"`
	MaxArtifactBytes      int64 `json:"max_artifact_bytes"`
	MaxArtifactSetBytes   int64 `json:"max_artifact_set_bytes"`
	MaxEvents             int   `json:"max_events"`
	MaxArtifactReferences int   `json:"max_artifact_references"`
	MaxBlockingQuestions  int   `json:"max_blocking_questions"`
	RetentionDays         int   `json:"retention_days"`
}

func defaultAgentTaskDeliveryLimits() agentTaskDeliveryLimits {
	return agentTaskDeliveryLimits{
		ContextPackBytes:      agentTaskContextPackMaxBytes,
		EventBytes:            agentTaskEventMaxBytes,
		SummaryBytes:          agentTaskSummaryMaxBytes,
		NotificationBytes:     agentTaskNotificationMaxBytes,
		MaxArtifactBytes:      agentTaskArtifactMaxBytes,
		MaxArtifactSetBytes:   agentTaskArtifactSetMaxBytes,
		MaxEvents:             agentTaskMaxEvents,
		MaxArtifactReferences: agentTaskMaxArtifactReferences,
		MaxBlockingQuestions:  agentTaskMaxBlockingQuestions,
		RetentionDays:         agentTaskDefaultRetentionDays,
	}
}

func newAgentTaskID(prefix string) (string, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" || len(prefix) > 64 {
		return "", errors.New("task id prefix is invalid")
	}
	var entropy [32]byte
	if _, err := io.ReadFull(rand.Reader, entropy[:]); err != nil {
		return "", fmt.Errorf("read cryptographic task id entropy: %w", err)
	}
	seed := fmt.Sprintf("%s|%d|%d|%d|%s", prefix, os.Getpid(), agentTaskProcessStarted, agentTaskIDCounter.Add(1), hex.EncodeToString(entropy[:]))
	sum := sha256.Sum256([]byte(seed))
	return prefix + "_" + hex.EncodeToString(sum[:16]), nil
}

// agentTaskValidateStructured is the single persistence boundary for
// caller-, import-, and runtime-controlled JSON values. It rejects secrets,
// unsupported values, oversized keys/strings/collections, excessive nesting,
// and non-finite numbers before a transaction or artifact write can begin.
func agentTaskValidateStructured(value any, subject string, maxBytes int) error {
	subject = firstNonEmptyStrings(strings.TrimSpace(subject), "task payload")
	if maxBytes <= 0 {
		return fmt.Errorf("%s has no configured storage bound", subject)
	}
	nodes := 0
	stringBytes := 0
	var validate func(any, int) error
	validate = func(current any, depth int) error {
		if depth > agentTaskStructuredMaxDepth {
			return fmt.Errorf("%s exceeds the %d level nesting limit", subject, agentTaskStructuredMaxDepth)
		}
		nodes++
		if nodes > agentTaskStructuredMaxNodes {
			return fmt.Errorf("%s exceeds the %d value limit", subject, agentTaskStructuredMaxNodes)
		}
		switch typed := current.(type) {
		case nil, bool:
			return nil
		case string:
			stringBytes += len([]byte(typed))
			if len([]byte(typed)) > maxBytes || stringBytes > maxBytes {
				return fmt.Errorf("%s contains a string value outside its %d byte bound", subject, maxBytes)
			}
			return nil
		case json.Number:
			if _, err := typed.Float64(); err != nil {
				return fmt.Errorf("%s contains an invalid number", subject)
			}
			return nil
		case float64:
			if math.IsNaN(typed) || math.IsInf(typed, 0) {
				return fmt.Errorf("%s contains a non-finite number", subject)
			}
			return nil
		case float32:
			if math.IsNaN(float64(typed)) || math.IsInf(float64(typed), 0) {
				return fmt.Errorf("%s contains a non-finite number", subject)
			}
			return nil
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return nil
		case map[string]any:
			if len(typed) > agentTaskStructuredMaxNodes {
				return fmt.Errorf("%s contains an oversized object", subject)
			}
			for key, nested := range typed {
				if strings.TrimSpace(key) == "" || len([]byte(key)) > agentTaskStructuredMaxKeyBytes {
					return fmt.Errorf("%s contains an empty or oversized object key", subject)
				}
				stringBytes += len([]byte(key))
				if stringBytes > maxBytes {
					return fmt.Errorf("%s exceeds its %d byte key/value bound", subject, maxBytes)
				}
				if err := validate(nested, depth+1); err != nil {
					return err
				}
			}
			return nil
		case map[string]string:
			converted := make(map[string]any, len(typed))
			for key, nested := range typed {
				converted[key] = nested
			}
			return validate(converted, depth)
		case []any:
			if len(typed) > agentTaskStructuredMaxNodes {
				return fmt.Errorf("%s contains an oversized list", subject)
			}
			for _, nested := range typed {
				if err := validate(nested, depth+1); err != nil {
					return err
				}
			}
			return nil
		case []string:
			converted := make([]any, 0, len(typed))
			for _, nested := range typed {
				converted = append(converted, nested)
			}
			return validate(converted, depth)
		case []map[string]any:
			converted := make([]any, 0, len(typed))
			for _, nested := range typed {
				converted = append(converted, nested)
			}
			return validate(converted, depth)
		default:
			return fmt.Errorf("%s contains unsupported value type %T", subject, current)
		}
	}
	if err := validate(value, 0); err != nil {
		return err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("%s is not canonically serializable: %w", subject, err)
	}
	if len(encoded) > maxBytes {
		return fmt.Errorf("%s exceeds the %d byte storage bound", subject, maxBytes)
	}
	filter := writeSecretFilterResult{Mode: "block"}
	_ = scrubWriteSecrets(value, &filter, 0)
	if filter.Findings > 0 {
		return fmt.Errorf("%s rejected by canonical Gateway secret boundary", subject)
	}
	return nil
}

func agentTaskValidateText(value, subject string, maxBytes int) error {
	if len([]byte(value)) > maxBytes {
		return fmt.Errorf("%s exceeds the %d byte limit", subject, maxBytes)
	}
	return agentTaskValidateStructured(map[string]any{"value": value}, subject, maxBytes+32)
}

func agentTaskDigest(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func agentTaskRevisionAuthorizationBasis(envelope map[string]any) map[string]any {
	return map[string]any{
		"schema_id":                         agentTaskRevisionEnvelopeContractID,
		"review_id":                         anyToString(envelope["review_id"]),
		"task_id":                           anyToString(envelope["task_id"]),
		"source_result_id":                  anyToString(envelope["source_result_id"]),
		"source_attempt_id":                 anyToString(envelope["source_attempt_id"]),
		"source_generation":                 anyToInt(envelope["source_generation"], 0),
		"reason":                            anyToString(envelope["reason"]),
		"attempt_id":                        anyToString(envelope["attempt_id"]),
		"lease_id":                          anyToString(envelope["lease_id"]),
		"generation":                        anyToInt(envelope["generation"], 0),
		"worker_id":                         anyToString(envelope["worker_id"]),
		"worker_instance_id":                anyToString(envelope["worker_instance_id"]),
		"worker_identity_update_generation": anyToInt(envelope["worker_identity_update_generation"], 0),
	}
}

func agentTaskMaterializeRevisionEnvelope(base map[string]any, fence agentTaskFence) (map[string]any, error) {
	if len(base) == 0 {
		return map[string]any{}, nil
	}
	reason, truncated := agentTaskBoundedString(anyToString(base["reason"]), 3000)
	if truncated {
		return nil, errors.New("revision envelope reason exceeds the 3000 byte limit")
	}
	if anyToString(base["review_id"]) == "" || anyToString(base["source_result_id"]) == "" || anyToString(base["source_attempt_id"]) == "" || anyToInt(base["source_generation"], 0) <= 0 {
		return nil, errors.New("revision envelope is missing immutable review source evidence")
	}
	if anyToString(base["task_id"]) != fence.TaskID || fence.Generation <= anyToInt(base["source_generation"], 0) {
		return nil, errors.New("stale_revision_fence: revision source does not precede the exact claimed generation")
	}
	envelope := map[string]any{
		"schema_id": agentTaskRevisionEnvelopeContractID, "contract_version": 1,
		"review_id": anyToString(base["review_id"]), "task_id": fence.TaskID,
		"source_result_id": anyToString(base["source_result_id"]), "source_attempt_id": anyToString(base["source_attempt_id"]),
		"source_generation": anyToInt(base["source_generation"], 0), "reason": reason,
		"attempt_id": fence.AttemptID, "lease_id": fence.LeaseID, "generation": fence.Generation,
		"assignment_generation": fence.Generation, "lease_generation": fence.Generation,
		"worker_id": fence.WorkerID, "worker_instance_id": fence.WorkerInstanceID,
		"worker_identity_update_generation": fence.WorkerIdentityUpdateGeneration, "authorized": true,
	}
	envelope["authorization_digest"] = agentTaskDigest(agentTaskRevisionAuthorizationBasis(envelope))
	envelope = agentTaskContractPayload(agentTaskRevisionEnvelopeContractID, envelope)
	if err := agentTaskRequireContract(agentTaskRevisionEnvelopeContractID, envelope); err != nil {
		return nil, err
	}
	if err := agentTaskValidateStructured(envelope, "worker-authorized revision envelope", agentTaskEventMaxBytes); err != nil {
		return nil, err
	}
	return envelope, nil
}

func verifyAgentTaskRevisionEnvelope(envelope map[string]any, fence agentTaskFence) error {
	if err := agentTaskRequireContract(agentTaskRevisionEnvelopeContractID, envelope); err != nil {
		return err
	}
	if anyToString(envelope["task_id"]) != fence.TaskID || anyToString(envelope["attempt_id"]) != fence.AttemptID || anyToString(envelope["lease_id"]) != fence.LeaseID || anyToInt(envelope["generation"], 0) != fence.Generation || anyToInt(envelope["assignment_generation"], 0) != fence.Generation || anyToInt(envelope["lease_generation"], 0) != fence.Generation || anyToString(envelope["worker_id"]) != fence.WorkerID || anyToString(envelope["worker_instance_id"]) != fence.WorkerInstanceID || anyToInt(envelope["worker_identity_update_generation"], -1) != fence.WorkerIdentityUpdateGeneration || !anyToBool(envelope["authorized"]) {
		return errors.New("stale_revision_fence: revision envelope is not authorized for the exact worker claim")
	}
	expected := agentTaskDigest(agentTaskRevisionAuthorizationBasis(envelope))
	if expected == "" || anyToString(envelope["authorization_digest"]) != expected {
		return errors.New("revision envelope authorization digest mismatch")
	}
	return nil
}

// agentTaskRevisionSourceForRequeue reduces the worker-specific envelope back
// to its immutable review instructions only after proving that the stored
// envelope belongs to the exact failed or quarantined attempt. The next claim
// can then materialize a fresh worker fence without losing or duplicating the
// original request-changes evidence.
func agentTaskRevisionSourceForRequeue(envelope map[string]any, fence agentTaskFence) (map[string]any, error) {
	if len(envelope) == 0 {
		return map[string]any{}, nil
	}
	if err := verifyAgentTaskRevisionEnvelope(envelope, fence); err != nil {
		return nil, err
	}
	source := map[string]any{
		"schema_id": "agent_task_revision_source.v1", "review_id": anyToString(envelope["review_id"]),
		"task_id": fence.TaskID, "source_result_id": anyToString(envelope["source_result_id"]),
		"source_attempt_id": anyToString(envelope["source_attempt_id"]),
		"source_generation": anyToInt(envelope["source_generation"], 0), "reason": anyToString(envelope["reason"]),
	}
	if err := agentTaskValidateStructured(source, "request-changes revision retry source", agentTaskEventMaxBytes); err != nil {
		return nil, err
	}
	return source, nil
}

func agentTaskBytesDigest(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func agentTaskCanonicalSHA256(value string) bool {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	for _, ch := range value[len("sha256:"):] {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			return false
		}
	}
	return true
}

func agentTaskBoundedString(value string, maxBytes int) (string, bool) {
	value = strings.TrimSpace(value)
	if maxBytes <= 0 || len([]byte(value)) <= maxBytes {
		return value, false
	}
	raw := []byte(value)
	if maxBytes <= len("... [truncated]") {
		bounded := strings.ToValidUTF8(string(raw[:maxBytes]), "")
		return bounded, true
	}
	prefix := strings.ToValidUTF8(string(raw[:maxBytes-len("... [truncated]")]), "")
	return prefix + "... [truncated]", true
}

func agentTaskStatus(raw string) string {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "queued", "leased", "running", "waiting_for_input", "execution_observed", "execution_failed", "canceled", "quarantined", "publication_pending", "writeback_pending", "writeback_failed", "result_published", "execution_succeeded", "review_pending", "review_blocked", "accepted_for_integration", "changes_requested", "rejected", "superseded", "knowledge_accepted", "unintegrated", "integration_pending", "integrated", "integration_failed", "approval_pending", "dead_letter":
		return strings.TrimSpace(strings.ToLower(raw))
	default:
		return ""
	}
}

func agentTaskTerminal(status string) bool {
	switch agentTaskStatus(status) {
	case "canceled", "quarantined", "execution_failed", "rejected", "superseded", "unintegrated", "integrated", "integration_failed", "dead_letter":
		return true
	default:
		return false
	}
}

func agentTaskTransitionMatrix() map[string]map[string]bool {
	return map[string]map[string]bool{
		"queued":                   {"leased": true, "canceled": true},
		"leased":                   {"running": true, "queued": true, "execution_observed": true, "execution_failed": true, "quarantined": true, "canceled": true},
		"running":                  {"waiting_for_input": true, "execution_observed": true, "execution_failed": true, "canceled": true, "quarantined": true},
		"waiting_for_input":        {"running": true, "execution_observed": true, "execution_failed": true, "canceled": true, "quarantined": true},
		"execution_failed":         {"queued": true, "dead_letter": true},
		"execution_observed":       {"publication_pending": true, "execution_failed": true, "quarantined": true},
		"publication_pending":      {"writeback_pending": true, "writeback_failed": true, "execution_failed": true},
		"writeback_pending":        {"writeback_failed": true, "result_published": true, "execution_failed": true},
		"writeback_failed":         {"writeback_pending": true, "execution_failed": true},
		"result_published":         {"execution_succeeded": true},
		"execution_succeeded":      {"review_pending": true},
		"review_pending":           {"review_blocked": true, "accepted_for_integration": true, "changes_requested": true, "rejected": true, "superseded": true, "knowledge_accepted": true, "unintegrated": true},
		"review_blocked":           {"waiting_for_input": true, "queued": true, "rejected": true},
		"changes_requested":        {"queued": true, "superseded": true},
		"quarantined":              {"queued": true},
		"accepted_for_integration": {"integration_pending": true, "approval_pending": true, "unintegrated": true, "integration_failed": true},
		"approval_pending":         {"integration_pending": true, "review_blocked": true},
		"integration_pending":      {"integrated": true, "integration_failed": true, "unintegrated": true},
		"knowledge_accepted":       {"integrated": true},
	}
}

func agentTaskAllowedTransition(from, to string) bool {
	from = agentTaskStatus(from)
	to = agentTaskStatus(to)
	if from == to {
		return true
	}
	allowed := agentTaskTransitionMatrix()
	return allowed[from][to]
}

func agentTaskContractPayload(contractID string, payload map[string]any) map[string]any {
	if payload == nil {
		payload = map[string]any{}
	}
	if strings.TrimSpace(anyToString(payload["schema_id"])) == "" {
		payload["schema_id"] = contractID
	}
	return attachPayloadFormatContract(contractID, payload, anyToString(payload["worker_id"]), "task_delivery", "/agents/tasks")
}

func agentTaskContractFindings(contractID string, payload map[string]any) []map[string]any {
	if payload == nil {
		return []map[string]any{{"reason": "payload_not_object", "contract_id": contractID}}
	}
	return validateAgentContractPayload(contractID, payload)
}

func agentTaskRequireContract(contractID string, payload map[string]any) error {
	findings := agentTaskContractFindings(contractID, payload)
	if len(findings) == 0 {
		return nil
	}
	return fmt.Errorf("%s validation failed: %s", contractID, agentTaskValidationSummary(findings))
}

func agentTaskValidationSummary(findings []map[string]any) string {
	if len(findings) == 0 {
		return ""
	}
	parts := make([]string, 0, minInt(len(findings), 4))
	for _, finding := range findings[:minInt(len(findings), 4)] {
		path := firstNonEmptyStrings(anyToString(finding["path"]), anyToString(finding["field"]), "payload")
		reason := firstNonEmptyStrings(anyToString(finding["reason"]), "invalid")
		parts = append(parts, path+":"+reason)
	}
	return strings.Join(parts, ",")
}

func normalizeAgentTaskRecipients(raw any, project, reviewOwner string) ([]map[string]any, string, error) {
	rows := make([]map[string]any, 0, 4)
	if list, ok := raw.([]any); ok {
		for _, item := range list {
			if text := strings.TrimSpace(anyToString(item)); text != "" {
				if _, truncated := agentTaskBoundedString(text, 2048); truncated {
					return nil, "", errors.New("recipient principal_id exceeds the 2048 byte limit")
				}
				rows = append(rows, map[string]any{"principal_id": text, "role": "observer", "project": project, "observer": true})
				continue
			}
			candidate := cloneAnyMap(anyMap(item))
			if len(candidate) == 0 {
				continue
			}
			principal := strings.TrimSpace(firstNonEmptyStrings(anyToString(candidate["principal_id"]), anyToString(candidate["principal"]), anyToString(candidate["id"])))
			if principal == "" {
				return nil, "", errors.New("recipient principal_id is required")
			}
			if _, truncated := agentTaskBoundedString(principal, 2048); truncated {
				return nil, "", errors.New("recipient principal_id exceeds the 2048 byte limit")
			}
			role := strings.TrimSpace(strings.ToLower(firstNonEmptyStrings(anyToString(candidate["role"]), "observer")))
			if role != "reviewer" && role != "pm" && role != "primary_agent" && role != "observer" {
				return nil, "", fmt.Errorf("recipient %s has unsupported role %q", principal, role)
			}
			candidate["principal_id"] = principal
			candidate["role"] = role
			candidate["project"] = project
			candidate["observer"] = role == "observer" || anyToBool(candidate["observer"])
			if sessionID := strings.TrimSpace(anyToString(candidate["session_id"])); sessionID != "" {
				if _, truncated := agentTaskBoundedString(sessionID, 2048); truncated {
					return nil, "", errors.New("recipient session_id exceeds the 2048 byte limit")
				}
				candidate["session_id"] = sessionID
			}
			rows = append(rows, candidate)
		}
	}
	reviewOwner = strings.TrimSpace(reviewOwner)
	if reviewOwner == "" && len(rows) > 0 {
		for _, row := range rows {
			if !anyToBool(row["observer"]) || anyToString(row["role"]) == "reviewer" {
				reviewOwner = anyToString(row["principal_id"])
				break
			}
		}
	}
	if reviewOwner == "" {
		return nil, "", errors.New("review_owner is required")
	}
	if _, truncated := agentTaskBoundedString(reviewOwner, 2048); truncated {
		return nil, "", errors.New("review_owner exceeds the 2048 byte limit")
	}
	hasOwner := false
	for _, row := range rows {
		if strings.EqualFold(anyToString(row["principal_id"]), reviewOwner) {
			hasOwner = true
			row["role"] = "reviewer"
			row["observer"] = false
		}
	}
	if !hasOwner {
		rows = append(rows, map[string]any{"principal_id": reviewOwner, "role": "reviewer", "project": project, "observer": false})
	}
	return rows, reviewOwner, nil
}

func normalizeAgentTaskManifest(input map[string]any) (map[string]any, bool, error) {
	if input == nil {
		return nil, false, errors.New("task manifest is required")
	}
	manifest := cloneAnyMap(input)
	legacy := strings.TrimSpace(anyToString(manifest["schema_id"])) == "" && strings.TrimSpace(anyToString(manifest["contract_version"])) == ""
	project := strings.TrimSpace(firstNonEmptyStrings(anyToString(manifest["project"]), anyToString(manifest["project_name"])))
	workspaceID := strings.TrimSpace(anyToString(manifest["workspace_id"]))
	objective := strings.TrimSpace(firstNonEmptyStrings(anyToString(manifest["objective"]), anyToString(manifest["title"]), anyToString(manifest["task"])))
	if project == "" {
		return nil, legacy, errors.New("project is required")
	}
	if workspaceID == "" {
		if legacy {
			workspaceID = "legacy-unbound"
		} else {
			return nil, legacy, errors.New("workspace_id is required")
		}
	}
	if objective == "" {
		return nil, legacy, errors.New("objective is required")
	}
	if err := agentTaskValidateText(objective, "task objective", agentTaskSummaryMaxBytes); err != nil {
		return nil, legacy, err
	}
	for field, value := range map[string]string{"project": project, "workspace_id": workspaceID} {
		if _, truncated := agentTaskBoundedString(value, 2048); truncated {
			return nil, legacy, fmt.Errorf("%s exceeds the 2048 byte limit", field)
		}
	}
	criteria := anyToStringSlice(manifest["acceptance_criteria"])
	if len(criteria) == 0 {
		if legacy {
			criteria = []string{"complete the stated objective"}
		} else {
			return nil, legacy, errors.New("acceptance_criteria must contain at least one item")
		}
	}
	if len(criteria) > 64 {
		return nil, legacy, errors.New("acceptance_criteria exceeds the 64 item limit")
	}
	for _, criterion := range criteria {
		if err := agentTaskValidateText(criterion, "acceptance criterion", 8192); err != nil {
			return nil, legacy, err
		}
	}
	reviewOwner := strings.TrimSpace(firstNonEmptyStrings(anyToString(manifest["review_owner"]), anyToString(manifest["canonical_reviewer"]), anyToString(manifest["reviewer"])))
	recipients, reviewOwner, err := normalizeAgentTaskRecipients(manifest["recipients"], project, reviewOwner)
	if err != nil {
		if !legacy {
			return nil, legacy, err
		}
		fallback := strings.TrimSpace(firstNonEmptyStrings(reviewOwner, anyToString(manifest["agent"]), "primary-agent"))
		recipients = []map[string]any{{"principal_id": fallback, "role": "reviewer", "project": project, "observer": false}}
		reviewOwner = fallback
	}
	if _, truncated := agentTaskBoundedString(reviewOwner, 2048); truncated {
		return nil, legacy, errors.New("review_owner exceeds the 2048 byte limit")
	}
	if len(recipients) == 0 {
		return nil, legacy, errors.New("at least one recipient is required")
	}
	if len(recipients) > 64 {
		return nil, legacy, errors.New("recipients exceeds the 64 principal limit")
	}
	taskID := strings.TrimSpace(anyToString(manifest["task_id"]))
	if taskID == "" {
		return nil, legacy, errors.New("task_id must be allocated by the authoritative ledger")
	}
	if _, truncated := agentTaskBoundedString(taskID, 2048); truncated {
		return nil, legacy, errors.New("task_id exceeds the 2048 byte limit")
	}
	idempotencyKey := strings.TrimSpace(firstNonEmptyStrings(anyToString(manifest["idempotency_key"]), anyToString(manifest["idempotencyKey"])))
	if idempotencyKey == "" {
		if legacy {
			idempotencyKey = "legacy:" + taskID
		} else {
			return nil, legacy, errors.New("idempotency_key is required")
		}
	}
	if _, truncated := agentTaskBoundedString(idempotencyKey, 2048); truncated {
		return nil, legacy, errors.New("idempotency_key exceeds the 2048 byte limit")
	}
	taskClass := strings.TrimSpace(strings.ToLower(firstNonEmptyStrings(anyToString(manifest["task_class"]), "non_coding")))
	if taskClass != "coding" && taskClass != "non_coding" {
		return nil, legacy, errors.New("task_class must be coding or non_coding")
	}
	status := strings.TrimSpace(strings.ToLower(firstNonEmptyStrings(anyToString(manifest["status"]), "queued")))
	if status == "" {
		status = "queued"
	}
	if agentTaskStatus(status) == "" || status != "queued" {
		return nil, legacy, errors.New("new task manifests must start queued")
	}
	manifest["schema_id"] = agentTaskManifestContractID
	manifest["contract_version"] = 1
	manifest["task_id"] = taskID
	manifest["project"] = project
	manifest["workspace_id"] = workspaceID
	manifest["objective"] = objective
	manifest["title"] = objective
	manifest["acceptance_criteria"] = criteria
	manifest["task_class"] = taskClass
	manifest["execution_profile"] = firstNonEmptyStrings(anyToString(manifest["execution_profile"]), "local-default")
	manifest["risk_level"] = firstNonEmptyStrings(anyToString(manifest["risk_level"]), "normal")
	if err := agentTaskValidateText(anyToString(manifest["execution_profile"]), "execution_profile", 512); err != nil {
		return nil, legacy, err
	}
	if err := agentTaskValidateText(anyToString(manifest["risk_level"]), "risk_level", 512); err != nil {
		return nil, legacy, err
	}
	manifest["approval_policy"] = cloneAnyMap(anyMap(manifest["approval_policy"]))
	if err := agentTaskValidateStructured(manifest["approval_policy"], "approval policy", agentTaskEventMaxBytes); err != nil {
		return nil, legacy, err
	}
	contextRequest := cloneAnyMap(anyMap(manifest["context_request"]))
	manifest["context_request"] = contextRequest
	contextHash := strings.TrimSpace(anyToString(contextRequest["content_hash"]))
	contextSessionID := strings.TrimSpace(firstNonEmptyStrings(anyToString(contextRequest["session_id"]), anyToString(contextRequest["sessionId"])))
	if !legacy && (!agentTaskCanonicalSHA256(contextHash) || contextSessionID == "") {
		return nil, legacy, errors.New("context_request requires a canonical content_hash and session_id")
	}
	if contextHash != "" && !agentTaskCanonicalSHA256(contextHash) {
		return nil, legacy, errors.New("context_request content_hash must be canonical sha256")
	}
	for field, value := range map[string]string{"session_id": contextSessionID, "topic_path": strings.TrimSpace(anyToString(contextRequest["topic_path"]))} {
		if _, truncated := agentTaskBoundedString(value, 2048); truncated {
			return nil, legacy, fmt.Errorf("context_request %s exceeds the 2048 byte limit", field)
		}
	}
	if contextSessionID != "" {
		contextRequest["session_id"] = contextSessionID
		delete(contextRequest, "sessionId")
	}
	contextRequestJSON, contextRequestErr := json.Marshal(manifest["context_request"])
	if contextRequestErr != nil || len(contextRequestJSON) > agentTaskContextPackMaxBytes {
		return nil, legacy, fmt.Errorf("context_request exceeds the %d byte limit", agentTaskContextPackMaxBytes)
	}
	manifest["recipients"] = func() []any {
		out := make([]any, 0, len(recipients))
		for _, row := range recipients {
			out = append(out, row)
		}
		return out
	}()
	manifest["review_owner"] = reviewOwner
	requestingAgentID := firstNonEmptyStrings(anyToString(manifest["requesting_agent_id"]), reviewOwner)
	if _, truncated := agentTaskBoundedString(requestingAgentID, 2048); truncated {
		return nil, legacy, errors.New("requesting_agent_id exceeds the 2048 byte limit")
	}
	manifest["requesting_agent_id"] = requestingAgentID
	manifest["metadata"] = cloneAnyMap(anyMap(manifest["metadata"]))
	if err := agentTaskValidateStructured(manifest["metadata"], "task metadata", agentTaskEventMaxBytes*4); err != nil {
		return nil, legacy, err
	}
	manifest["idempotency_key"] = idempotencyKey
	manifest["status"] = "queued"
	manifest = agentTaskContractPayload(agentTaskManifestContractID, manifest)
	if err := agentTaskRequireContract(agentTaskManifestContractID, manifest); err != nil {
		return nil, legacy, err
	}
	return manifest, legacy, nil
}

func agentTaskRecipientRows(manifest map[string]any) []map[string]any {
	rows := []map[string]any{}
	if list, ok := manifest["recipients"].([]any); ok {
		for _, item := range list {
			row := cloneAnyMap(anyMap(item))
			if len(row) == 0 {
				continue
			}
			rows = append(rows, agentTaskContractPayload(agentTaskRecipientContractID, row))
		}
	}
	return rows
}

func agentTaskNow() string { return time.Now().UTC().Format(time.RFC3339Nano) }
