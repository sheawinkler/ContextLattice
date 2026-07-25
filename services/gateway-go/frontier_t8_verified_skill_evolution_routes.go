package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const frontierT8SkillEvolutionPath = "/memory/skills/foundry/evolution"

const (
	frontierT8OperationDeriveReusable   = "derive_reusable_candidate"
	frontierT8OperationHandoffReusable  = "handoff_reusable_candidate"
	frontierT8OperationDeriveRetirement = "derive_retirement_candidate"
	frontierT8OperationRecordUsage      = "record_usage_receipt"
	frontierT8OperationReviewEfficacy   = "derive_efficacy_review"
)

type frontierT8EvolutionRouteRequest struct {
	Operation       string          `json:"operation"`
	AgentID         string          `json:"agent_id,omitempty"`
	Input           json.RawMessage `json:"input"`
	ExplicitHandoff *bool           `json:"explicit_handoff,omitempty"`
}

func (s *server) memorySkillFoundryEvolution(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		frontierT8WriteRouteError(w, http.StatusMethodNotAllowed, "method_not_allowed", nil)
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	request, input, err := frontierT8DecodeEvolutionRequest(r)
	if err != nil {
		frontierT8WriteRouteError(w, http.StatusBadRequest, "invalid_evolution_request", err)
		return
	}
	now := time.Now().UTC()
	if request.Operation == frontierT8OperationDeriveReusable ||
		request.Operation == frontierT8OperationHandoffReusable ||
		request.Operation == frontierT8OperationDeriveRetirement {
		input = frontierT8BindServerClock(input, now)
	}

	var response map[string]any
	switch request.Operation {
	case frontierT8OperationRecordUsage:
		if request.ExplicitHandoff != nil {
			frontierT8WriteRouteError(w, http.StatusBadRequest, "invalid_evolution_request", errors.New("explicit_handoff is not valid for record_usage_receipt"))
			return
		}
		usageResponse, usageErr := s.frontierT8RecordSkillUsage(input, now)
		if usageErr != nil {
			status := http.StatusUnprocessableEntity
			if strings.Contains(usageErr.Error(), "store") || strings.Contains(usageErr.Error(), "persistence") {
				status = http.StatusServiceUnavailable
			}
			frontierT8WriteRouteError(w, status, "skill_usage_receipt_rejected", usageErr)
			return
		}
		response = usageResponse
	case frontierT8OperationReviewEfficacy:
		if request.ExplicitHandoff != nil {
			frontierT8WriteRouteError(w, http.StatusBadRequest, "invalid_evolution_request", errors.New("explicit_handoff is not valid for derive_efficacy_review"))
			return
		}
		reviewResponse, reviewErr := s.frontierT8RecordSkillEfficacyReview(input, now)
		if reviewErr != nil {
			status := http.StatusUnprocessableEntity
			if strings.Contains(reviewErr.Error(), "store") || strings.Contains(reviewErr.Error(), "persistence") {
				status = http.StatusServiceUnavailable
			}
			frontierT8WriteRouteError(w, status, "skill_efficacy_review_rejected", reviewErr)
			return
		}
		response = reviewResponse
	case frontierT8OperationDeriveReusable:
		if request.ExplicitHandoff != nil {
			frontierT8WriteRouteError(w, http.StatusBadRequest, "invalid_evolution_request", errors.New("explicit_handoff is only valid for handoff_reusable_candidate"))
			return
		}
		candidate, candidateErr := frontierT8ReusableSkillCandidate(input)
		if candidateErr != nil {
			frontierT8WriteRouteError(w, http.StatusUnprocessableEntity, "reusable_candidate_rejected", candidateErr)
			return
		}
		authority, authorityErr := s.frontierT8ResolveEvidenceAuthority(input, candidate, now)
		if authorityErr != nil {
			frontierT8WriteRouteError(w, http.StatusUnprocessableEntity, "reusable_candidate_rejected", authorityErr)
			return
		}
		frontierT8AttachEvidenceAuthority(candidate, authority)
		if boundedErr := frontierT8EnsureBounded(candidate); boundedErr != nil {
			frontierT8WriteRouteError(w, http.StatusUnprocessableEntity, "reusable_candidate_rejected", boundedErr)
			return
		}
		response = frontierT8ReusableResponse(request.Operation, candidate, frontierT8ReusableSkillsIndex(candidate), nil)
	case frontierT8OperationHandoffReusable:
		if request.ExplicitHandoff == nil || !*request.ExplicitHandoff {
			frontierT8WriteRouteError(w, http.StatusBadRequest, "explicit_handoff_required", nil)
			return
		}
		candidate, candidateErr := frontierT8ReusableSkillCandidate(input)
		if candidateErr != nil {
			frontierT8WriteRouteError(w, http.StatusUnprocessableEntity, "reusable_candidate_rejected", candidateErr)
			return
		}
		authority, authorityErr := s.frontierT8ResolveEvidenceAuthority(input, candidate, now)
		if authorityErr != nil {
			frontierT8WriteRouteError(w, http.StatusUnprocessableEntity, "reusable_candidate_rejected", authorityErr)
			return
		}
		frontierT8AttachEvidenceAuthority(candidate, authority)
		if boundedErr := frontierT8EnsureBounded(candidate); boundedErr != nil {
			frontierT8WriteRouteError(w, http.StatusUnprocessableEntity, "reusable_candidate_rejected", boundedErr)
			return
		}
		index := frontierT8ReusableSkillsIndex(candidate)
		handoff, handoffErr := s.frontierT8HandoffToFoundry(candidate)
		if handoffErr != nil {
			frontierT8WriteRouteError(w, http.StatusServiceUnavailable, "skill_foundry_handoff_failed", nil)
			return
		}
		response = frontierT8ReusableResponse(request.Operation, candidate, index, handoff)
	case frontierT8OperationDeriveRetirement:
		if request.ExplicitHandoff != nil {
			frontierT8WriteRouteError(w, http.StatusBadRequest, "invalid_evolution_request", errors.New("explicit_handoff is only valid for handoff_reusable_candidate"))
			return
		}
		candidate, candidateErr := frontierT8SkillRetirementCandidate(input)
		if candidateErr != nil {
			frontierT8WriteRouteError(w, http.StatusUnprocessableEntity, "retirement_candidate_rejected", candidateErr)
			return
		}
		authority, authorityErr := s.frontierT8ResolveEvidenceAuthority(input, candidate, now)
		if authorityErr != nil {
			frontierT8WriteRouteError(w, http.StatusUnprocessableEntity, "retirement_candidate_rejected", authorityErr)
			return
		}
		frontierT8AttachEvidenceAuthority(candidate, authority)
		index := frontierT8RetirementSkillsIndex(candidate)
		frontierT8ApplyIndexRetirementProtections(candidate, index)
		if err := frontierT8EnsureBounded(candidate); err != nil {
			frontierT8WriteRouteError(w, http.StatusUnprocessableEntity, "retirement_candidate_rejected", err)
			return
		}
		response = frontierT8RetirementResponse(request.Operation, candidate, index)
	default:
		frontierT8WriteRouteError(w, http.StatusBadRequest, "unsupported_evolution_operation", nil)
		return
	}

	contractID := anyToString(response["schema_id"])
	response = attachPayloadFormatContract(contractID, response, request.AgentID, "skill_evolution", r.URL.Path)
	if anyToString(anyMap(anyMap(response["format_contract"])["validation"])["status"]) != "passed" {
		frontierT8WriteRouteError(w, http.StatusInternalServerError, "evolution_contract_validation_failed", nil)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func skillEfficacyRequestDigest(input map[string]any) string {
	raw, err := json.Marshal(input)
	if err != nil {
		return ""
	}
	return "sha256:" + sha256Hex(string(raw))
}

func skillEfficacyTransactionID(kind, scope, idempotencyKey string) string {
	return "skilltxn_" + sha256Hex("skill-efficacy\x00" + kind + "\x00" + scope + "\x00" + idempotencyKey)[:24]
}

func skillEfficacyTransactionReplay(store *skillFoundryStore, transactionID, requestDigest string) (map[string]any, bool, error) {
	transaction := store.transaction(transactionID)
	if len(transaction) == 0 {
		return nil, false, nil
	}
	metadata := anyMap(transaction["metadata"])
	if anyToString(metadata["request_digest"]) != requestDigest {
		return nil, true, errors.New("idempotency key conflicts with existing material")
	}
	rows := contextPackAnyList(transaction["rows"])
	if len(rows) == 1 && len(anyMap(rows[0])) > 0 {
		return cloneMap(anyMap(rows[0])), true, nil
	}
	// Older compacted ledgers may preserve the transaction identity and latest
	// material as separate rows. Resolve that legacy shape only when it still
	// represents the exact recorded stage.
	if usageID := anyToString(metadata["usage_id"]); usageID != "" {
		if receipt := store.usageReceipt(usageID); len(receipt) > 0 {
			if stage := anyToString(metadata["stage"]); stage == "" || anyToString(receipt["stage"]) == stage {
				return receipt, true, nil
			}
		}
	}
	if reviewID := anyToString(metadata["review_id"]); reviewID != "" {
		if review := store.efficacyReview(reviewID); len(review) > 0 {
			return review, true, nil
		}
	}
	return nil, true, errors.New("persisted skill efficacy transaction is incomplete")
}

func (s *server) frontierT8RecordSkillUsage(input map[string]any, now time.Time) (map[string]any, error) {
	if s == nil || s.skillFoundry == nil || !s.skillFoundry.enabled {
		return nil, errors.New("skill Foundry store is unavailable")
	}
	idempotencyKey, err := skillEfficacyRequiredIdentifier(input["idempotency_key"], "idempotency_key")
	if err != nil {
		return nil, err
	}
	usageID, err := skillEfficacyRequiredIdentifier(input["usage_id"], "usage_id")
	if err != nil {
		return nil, err
	}
	requestDigest := skillEfficacyRequestDigest(input)
	transactionID := skillEfficacyTransactionID("usage", usageID, idempotencyKey)
	if replay, found, replayErr := skillEfficacyTransactionReplay(s.skillFoundry, transactionID, requestDigest); found {
		if replayErr != nil {
			return nil, replayErr
		}
		return skillUsageReceiptResponse(replay, true, transactionID), nil
	}
	s.skillLifecycleMu.Lock()
	defer s.skillLifecycleMu.Unlock()
	if replay, found, replayErr := skillEfficacyTransactionReplay(s.skillFoundry, transactionID, requestDigest); found {
		if replayErr != nil {
			return nil, replayErr
		}
		return skillUsageReceiptResponse(replay, true, transactionID), nil
	}
	existing := s.skillFoundry.usageReceipt(anyToString(input["usage_id"]))
	receipt, _, err := s.buildSkillUsageReceipt(input, existing, now)
	if err != nil {
		return nil, err
	}
	transaction, replayed, err := s.skillFoundry.recordTransaction(transactionID, "skill_efficacy_usage", map[string]any{
		"request_digest": requestDigest, "usage_id": receipt["usage_id"], "stage": receipt["stage"],
	}, receipt)
	if err != nil {
		return nil, err
	}
	rows := contextPackAnyList(transaction["rows"])
	if len(rows) == 1 {
		receipt = cloneMap(anyMap(rows[0]))
	}
	return skillUsageReceiptResponse(receipt, replayed, transactionID), nil
}

func skillUsageReceiptResponse(receipt map[string]any, replayed bool, transactionID string) map[string]any {
	return map[string]any{
		"ok": true, "schema_id": skillUsageReceiptContractID,
		"operation": frontierT8OperationRecordUsage, "receipt": receipt,
		"recorded": true, "replayed": replayed,
		"persistence": map[string]any{
			"store": "skill_foundry", "transaction_id": transactionID,
			"append_only": true, "active_skill_mutated": false,
		},
		"safety": map[string]any{
			"provider_calls": 0, "network_calls": 0, "subprocess_calls": 0,
			"filesystem_mutations": 1, "ledger_writes": 1, "activation_performed": false,
		},
	}
}

func (s *server) frontierT8RecordSkillEfficacyReview(input map[string]any, now time.Time) (map[string]any, error) {
	if s == nil || s.skillFoundry == nil || !s.skillFoundry.enabled {
		return nil, errors.New("skill Foundry store is unavailable")
	}
	idempotencyKey, err := skillEfficacyRequiredIdentifier(input["idempotency_key"], "idempotency_key")
	if err != nil {
		return nil, err
	}
	project, err := sanitizeMemoryProject(anyToString(input["project"]))
	if err != nil {
		return nil, err
	}
	skillID, err := skillEfficacyRequiredIdentifier(input["skill_id"], "skill_id")
	if err != nil {
		return nil, err
	}
	requestDigest := skillEfficacyRequestDigest(input)
	transactionID := skillEfficacyTransactionID("review", strings.ToLower(project+"\x00"+skillID), idempotencyKey)
	if replay, found, replayErr := skillEfficacyTransactionReplay(s.skillFoundry, transactionID, requestDigest); found {
		if replayErr != nil {
			return nil, replayErr
		}
		return skillEfficacyReviewResponse(replay, true, transactionID), nil
	}
	s.skillLifecycleMu.Lock()
	defer s.skillLifecycleMu.Unlock()
	if replay, found, replayErr := skillEfficacyTransactionReplay(s.skillFoundry, transactionID, requestDigest); found {
		if replayErr != nil {
			return nil, replayErr
		}
		return skillEfficacyReviewResponse(replay, true, transactionID), nil
	}
	review, _, err := s.buildSkillEfficacyReview(input, now)
	if err != nil {
		return nil, err
	}
	transaction, replayed, err := s.skillFoundry.recordTransaction(transactionID, "skill_efficacy_review", map[string]any{
		"request_digest": requestDigest, "review_id": review["review_id"], "skill_id": review["skill_id"],
	}, review)
	if err != nil {
		return nil, err
	}
	rows := contextPackAnyList(transaction["rows"])
	if len(rows) == 1 {
		review = cloneMap(anyMap(rows[0]))
	}
	return skillEfficacyReviewResponse(review, replayed, transactionID), nil
}

func skillEfficacyReviewResponse(review map[string]any, replayed bool, transactionID string) map[string]any {
	return map[string]any{
		"ok": true, "schema_id": skillEfficacyReviewContractID,
		"operation": frontierT8OperationReviewEfficacy, "review": review,
		"recorded": true, "replayed": replayed,
		"persistence": map[string]any{
			"store": "skill_foundry", "transaction_id": transactionID,
			"append_only": true, "candidate_status": "inactive",
		},
		"safety": map[string]any{
			"provider_calls": 0, "network_calls": 0, "subprocess_calls": 0,
			"filesystem_mutations": 1, "ledger_writes": 1, "active_skill_mutations": 0,
			"activation_performed": false, "retirement_performed": false,
		},
	}
}

func frontierT8DecodeEvolutionRequest(r *http.Request) (frontierT8EvolutionRouteRequest, map[string]any, error) {
	var request frontierT8EvolutionRouteRequest
	raw, err := io.ReadAll(io.LimitReader(r.Body, frontierT8MaxInputBytes+4097))
	if err != nil {
		return request, nil, errors.New("failed to read bounded request")
	}
	if len(raw) > frontierT8MaxInputBytes+4096 {
		return request, nil, fmt.Errorf("request exceeds %d bytes", frontierT8MaxInputBytes+4096)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return request, nil, errors.New("request body is required")
	}
	if err := strictJSONDecode(raw, &request); err != nil {
		return request, nil, fmt.Errorf("strict request decode failed: %w", err)
	}
	if request.Operation == "" || request.Operation != strings.TrimSpace(request.Operation) {
		return request, nil, errors.New("operation is required without surrounding whitespace")
	}
	if request.AgentID != "" {
		if _, err := frontierT8Identifier(request.AgentID, "agent_id"); err != nil {
			return request, nil, err
		}
	}
	if len(bytes.TrimSpace(request.Input)) == 0 || bytes.Equal(bytes.TrimSpace(request.Input), []byte("null")) {
		return request, nil, errors.New("input object is required")
	}
	var input map[string]any
	if err := strictJSONDecode(request.Input, &input); err != nil || input == nil {
		return request, nil, errors.New("input must be one JSON object")
	}
	return request, input, nil
}

func frontierT8ReusableResponse(operation string, candidate, skillsIndex, handoff map[string]any) map[string]any {
	requested := handoff != nil
	if handoff == nil {
		handoff = map[string]any{
			"requested": false, "explicit": false, "performed": false,
			"draft_recorded": false, "evaluation_recorded": false,
			"export_performed": false, "installation_performed": false,
			"activation_performed": false, "deactivation_performed": false,
			"terminal_retirement_performed": false,
		}
	}
	return map[string]any{
		"ok": true, "schema_id": frontierT8ReusableSkillCandidateSchemaID,
		"operation": operation, "candidate": candidate, "skills_index": skillsIndex,
		"persistence": map[string]any{
			"candidate_persisted": false, "ordinary_memory_mutated": false,
			"foundry_handoff_requested": requested, "foundry_handoff_performed": anyToBool(handoff["performed"]),
		},
		"foundry_handoff": handoff,
	}
}

func frontierT8RetirementResponse(operation string, candidate, skillsIndex map[string]any) map[string]any {
	return map[string]any{
		"ok": true, "schema_id": frontierT8SkillRetirementCandidateSchemaID,
		"operation": operation, "candidate": candidate, "skills_index": skillsIndex,
		"persistence": map[string]any{
			"candidate_persisted": false, "ordinary_memory_mutated": false,
			"deactivation_performed": false, "terminal_retirement_performed": false,
		},
	}
}

func frontierT8ReusableSkillsIndex(candidate map[string]any) map[string]any {
	return map[string]any{
		"index": "native_active_skills", "lookup_mode": "read_only", "mutation_performed": false,
		"candidate_name": frontierT8SkillsIndexLookup(anyToString(candidate["name"])),
	}
}

func frontierT8RetirementSkillsIndex(candidate map[string]any) map[string]any {
	result := map[string]any{
		"index": "native_active_skills", "lookup_mode": "read_only", "mutation_performed": false,
		"current_skill": frontierT8SkillsIndexLookup(anyToString(candidate["name"])),
		"replacement":   map[string]any{"queried": false, "detected": false, "matches": []any{}},
	}
	replacement := anyMap(candidate["replacement"])
	if anyToBool(replacement["present"]) {
		result["replacement"] = frontierT8SkillsIndexLookup(anyToString(replacement["skill_id"]))
	}
	return result
}

func frontierT8SkillsIndexLookup(name string) map[string]any {
	collision := foundrySkillCollision(name)
	return map[string]any{
		"queried": true, "query_name": name, "detected": anyToBool(collision["detected"]),
		"matches": contextPackAnyList(collision["matches"]),
	}
}

func frontierT8ApplyIndexRetirementProtections(candidate, index map[string]any) {
	if !anyToBool(anyMap(index["current_skill"])["detected"]) {
		frontierT8ProtectRetirementCandidate(candidate, "native_skills_index_identity_unresolved")
	}
	replacement := anyMap(candidate["replacement"])
	if anyToBool(replacement["present"]) && !anyToBool(anyMap(index["replacement"])["detected"]) {
		frontierT8ProtectRetirementCandidate(candidate, "native_skills_index_replacement_unresolved")
	}
}

func frontierT8ProtectRetirementCandidate(candidate map[string]any, reason string) {
	protections := contextPackAnyList(candidate["protections"])
	for _, raw := range protections {
		if anyToString(raw) == reason {
			return
		}
	}
	candidate["protections"] = append(protections, reason)
	candidate["status"] = "protected"
	candidate["recommendation"] = "retain_pending_protection_review"
}

func (s *server) frontierT8HandoffToFoundry(candidate map[string]any) (map[string]any, error) {
	if s == nil || s.skillFoundry == nil || !s.skillFoundry.enabled {
		return nil, errors.New("skill Foundry store is unavailable")
	}
	provenance := anyMap(candidate["provenance"])
	if !anyToBool(provenance["authoritative_evidence_resolved"]) || len(anyMap(provenance["evidence_authority"])) == 0 {
		return nil, errors.New("authoritative evidence resolution is required before Foundry handoff")
	}
	candidateID := strings.TrimSpace(anyToString(candidate["candidate_id"]))
	if candidateID == "" {
		return nil, errors.New("candidate identity is required")
	}
	transactionID := "skilltxn_" + sha256Hex("frontier-t8\x00" + candidateID)[:24]
	if transaction := s.skillFoundry.transaction(transactionID); len(transaction) > 0 {
		return frontierT8FoundryTransactionResult(transaction, true), nil
	}
	handoff := anyMap(candidate["skill_foundry_handoff"])
	draftPayload := cloneJSONMap(anyMap(handoff["draft_payload"]))
	evaluationPayload := cloneJSONMap(anyMap(handoff["evaluation_template"]))
	if len(draftPayload) == 0 || len(evaluationPayload) == 0 {
		return nil, errors.New("candidate lacks a bounded Foundry handoff")
	}

	s.skillLifecycleMu.Lock()
	defer s.skillLifecycleMu.Unlock()
	draft, err := s.buildSkillDraft(draftPayload)
	if err != nil {
		return nil, err
	}
	if state := anyToString(draft["status"]); state == "exported" || state == "retired" {
		return nil, errors.New("terminal or exported drafts cannot accept an evolution handoff")
	}
	evaluationPayload["draft_id"] = draft["draft_id"]
	evaluation, updatedDraft, err := s.evaluateSkillDraftRecord(draft, evaluationPayload)
	if err != nil {
		return nil, err
	}
	draftRef := map[string]any{
		"schema_id": draft["schema_id"], "draft_id": draft["draft_id"],
		"draft_fingerprint": draft["draft_fingerprint"], "project": draft["project"],
		"name": draft["name"], "status": updatedDraft["status"],
		"activation_state":         anyMap(updatedDraft["activation"])["state"],
		"existing_skill_collision": updatedDraft["existing_skill_collision"],
	}
	evaluationRef := map[string]any{
		"schema_id": evaluation["schema_id"], "evaluation_id": evaluation["evaluation_id"],
		"draft_id": evaluation["draft_id"], "passed": evaluation["passed"],
		"holdout_count": evaluation["holdout_count"], "recommendation": evaluation["recommendation"],
		"activation_state": evaluation["activation_state"],
	}
	transaction, replayed, err := s.skillFoundry.recordTransaction(transactionID, "frontier_t8_evolution_handoff", map[string]any{
		"candidate_id": candidateID, "project": candidate["project"], "name": candidate["name"],
		"draft_ref": draftRef, "evaluation_ref": evaluationRef,
	}, draft, evaluation, updatedDraft)
	if err != nil {
		return nil, err
	}
	return frontierT8FoundryTransactionResult(transaction, replayed), nil
}

func frontierT8FoundryTransactionResult(transaction map[string]any, replayed bool) map[string]any {
	metadata := anyMap(transaction["metadata"])
	return map[string]any{
		"requested": true, "explicit": true, "performed": true,
		"draft_recorded": true, "evaluation_recorded": true,
		"idempotent": replayed, "transaction_id": transaction["transaction_id"],
		"transaction_digest": transaction["transaction_digest"],
		"export_performed":   false, "installation_performed": false,
		"activation_performed": false, "deactivation_performed": false,
		"terminal_retirement_performed": false,
		"draft_ref":                     metadata["draft_ref"], "evaluation_ref": metadata["evaluation_ref"],
	}
}

func frontierT8WriteRouteError(w http.ResponseWriter, status int, code string, err error) {
	payload := map[string]any{"ok": false, "error": code}
	if err != nil {
		payload["detail"] = err.Error()
	}
	writeJSON(w, status, payload)
}
