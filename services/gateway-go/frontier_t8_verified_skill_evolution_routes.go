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
	input = frontierT8BindServerClock(input, now)

	var response map[string]any
	switch request.Operation {
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
