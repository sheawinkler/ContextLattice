package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const agentDecisionResumeAction = "Verify or recompute the selected decision against current owner evidence before acting."

var errAgentDecisionResumeScope = errors.New("agent_session_resume_scope_changed")

type agentDecisionResumeObservation struct {
	projection map[string]any
	guard      *memoryDerivationGuard
}

func agentDecisionResumeScope(row map[string]any) string {
	fields := map[string]any{}
	for _, key := range []string{"id", "project", "repo", "branch", "task_identity_id", "agent_id", "scope_ownership"} {
		fields[key] = row[key]
	}
	return agentDecisionDigest(fields)
}

func agentDecisionResumeState(row map[string]any) string {
	fields := map[string]any{}
	for _, key := range []string{"decision_binding", "decision_baseline", "decision_store_unavailable", "status", "event_count", "last_event_at", "next_action", "objective_runtime"} {
		fields[key] = row[key]
	}
	return agentDecisionDigest(fields)
}

// Copy the selected immutable record; no continuity lock is held during memory
// owner I/O. A second exact digest comparison closes the two read sections.
func (s *continuityStore) agentDecisionResumeRecord(binding *agentDecisionBinding, row map[string]any) (decisionChange, error) {
	var change decisionChange
	resolved, err := s.resolveAgentDecisionBinding(binding, row)
	if err != nil || resolved.RecordDigest != binding.RecordDigest {
		return change, errAgentDecisionConflict
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	index, found := s.decisionChangeIndex[continuityScopedIndexKey(binding.Project, binding.DecisionChangeID)]
	if !s.enabled || s.lastError != "" || !found || index < 0 || index >= len(s.decisionChanges) || agentDecisionDigest(s.decisionChanges[index]) != binding.RecordDigest {
		return change, errAgentDecisionUnavailable
	}
	raw, err := json.Marshal(s.decisionChanges[index])
	if err != nil || json.Unmarshal(raw, &change) != nil {
		return change, errAgentDecisionUnavailable
	}
	return change, nil
}

func (s *server) observeAgentDecisionResume(ctx context.Context, row map[string]any, now time.Time) agentDecisionResumeObservation {
	projection := map[string]any{"schema_id": "session_decision_continuity.v1", "status": "legacy_unpinned",
		"meaning": "owner_version_only_not_truth_or_execution_permission", "acceptance_criteria": "not_provided",
		"assumption_predicates": "not_evaluated"}
	observation := agentDecisionResumeObservation{projection: projection}
	if _, present := row["decision_binding"]; !present {
		return observation
	}
	projection["status"], projection["reason"] = "unknown", "selected_record_or_owner_unavailable"
	binding, err := normalizeAgentDecisionBinding(row["decision_binding"], true)
	if err != nil || validateStoredAgentDecision(row) != nil {
		return observation
	}
	projection["decision_change_id"], projection["after_decision_id"], projection["record_digest"] = binding.DecisionChangeID, binding.AfterDecisionID, binding.RecordDigest
	if anyToBool(row["decision_store_unavailable"]) || s == nil || s.continuity == nil {
		return observation
	}
	change, err := s.continuity.agentDecisionResumeRecord(binding, row)
	if err != nil {
		return observation
	}
	if change.Applicability == nil {
		projection["status"], projection["reason"] = "legacy_unpinned", "selected_record_has_no_owner_pins"
		return observation
	}
	for _, timestamp := range []string{change.RecordedAt, change.OccurredAt} {
		when, parseErr := time.Parse(time.RFC3339Nano, timestamp)
		if parseErr != nil || when.After(now) {
			projection["status"], projection["reason"] = "not_applicable", "record_not_yet_effective_or_recorded"
			return observation
		}
	}
	a := change.Applicability
	if a.Scope.Project != binding.Project || a.Scope.ObjectiveID != binding.ObjectiveID || a.Scope.Branch != binding.Branch || !strings.EqualFold(a.Scope.Repo, binding.Repo) {
		projection["status"], projection["reason"] = "not_applicable", "selected_record_scope_mismatch"
		return observation
	}
	if !s.memoryDerivationAvailable() {
		return observation
	}
	owner := s.memoryStore
	fence, err := owner.acquireMemoryEdgeLogFenceOptionalContext(ctx)
	if err != nil || fence == nil {
		return observation
	}
	defer fence.release()
	guard := &memoryDerivationGuard{store: owner}
	walk := newMemoryDerivationWalk()
	changed := false
	for _, input := range a.Inputs {
		project, file, _, _, parseErr := canonicalMemoryID(input.MemoryID)
		if parseErr != nil || !strings.EqualFold(project, binding.Project) {
			return observation
		}
		entry, found := owner.currentEntry(project, file)
		if !found || owner.checkMemoryDerivation(ctx, entry, fence, walk, 0) != nil {
			return observation
		}
		pin := memoryStructuredReference{TargetID: input.MemoryID, TargetEventID: input.TargetEventID, TargetContentHash: input.TargetContentHash}
		changed = changed || !memoryDerivationReferenceMatchesTarget(pin, entry)
		guard.roots = append(guard.roots, entry)
	}
	if changed {
		projection["status"], projection["reason"] = "changed", "original_owner_version_changed"
		return observation
	}
	projection["status"], projection["reason"] = "verified_match", "original_owner_versions_match"
	projection["input_count"] = len(a.Inputs)
	for _, assumption := range a.Assumptions {
		if assumption.Role == "acceptance_criteria" {
			projection["acceptance_criteria"] = "owner_version_match_not_satisfaction"
		}
	}
	observation.guard = guard
	return observation
}

func (o agentDecisionResumeObservation) render(row map[string]any, events []map[string]any, now time.Time, compact bool) map[string]any {
	projection := cloneJSONMap(o.projection)
	if projection["status"] != "verified_match" {
		// Build negative views from a fixed allowlist, not arbitrary nested action
		// aliases or event text. Original intent has one explicitly historical home.
		projection["historical_intent"] = map[string]any{"not_current_instructions": true,
			"objective": clipText(anyToString(row["objective"]), 240), "next_action": clipText(anyToString(row["next_action"]), 240)}
		projection["omitted_event_count"] = len(events)
		safe := map[string]any{}
		for _, key := range []string{"id", "agent", "agent_id", "project", "repo", "branch", "task_id", "task_identity_id", "execution_lane_id", "native_session_id", "started_at", "updated_at", "status", "scope_ownership", "decision_binding", "decision_baseline", "decision_store_unavailable"} {
			if value, exists := row[key]; exists {
				safe[key] = value
			}
		}
		state := anyMap(row["agent_state"])
		safeState := map[string]any{}
		for _, key := range []string{"state", "authority", "source", "updated_at"} {
			if value, exists := state[key]; exists {
				safeState[key] = value
			}
		}
		safe["agent_state"] = safeState
		safe["objective"], safe["next_action"] = agentDecisionResumeAction, agentDecisionResumeAction
		row, events = safe, nil
	}
	if !compact {
		payload := buildAgentPromptContextPackage(row, events, now)
		payload["decision_continuity"] = projection
		return attachPayloadFormatContract(agentPromptContextPackageContractID, payload, anyToString(row["agent_id"]), "prompt_context_package", "/v1/agents/sessions/{session_id}/context-package")
	}
	packet := buildAgentSessionPacket(row, events, now)
	provenance := cloneAnyMap(anyMap(packet["provenance"]))
	provenance["decision_continuity"] = projection
	packet["provenance"] = provenance
	packet = finalizeAgentPacket(packet)
	if !anyToBool(anyMap(packet["token_budget"])["within_hard_limit"]) {
		// Never enlarge the transport ceiling or emit an oversized success after
		// adding a sticky negative marker.
		return map[string]any{"ok": false, "error": "decision_resume_budget_exceeded", "decision_continuity": projection}
	}
	return packet
}

func (o agentDecisionResumeObservation) finish(ctx context.Context, s *server, row map[string]any, events []map[string]any, now time.Time, compact bool, payload map[string]any) (map[string]any, error) {
	drift := false
	checkedWithOwner := false
	checkSession := func() error {
		current, _, found := s.agentSessions.get(anyToString(row["id"]))
		if !found || agentDecisionResumeScope(current) != agentDecisionResumeScope(row) {
			return errAgentDecisionResumeScope
		}
		drift = drift || agentDecisionResumeState(current) != agentDecisionResumeState(row)
		return nil
	}
	if o.projection["status"] == "verified_match" {
		binding, err := normalizeAgentDecisionBinding(row["decision_binding"], true)
		if err != nil || s.continuity == nil || o.guard == nil {
			drift = true
		} else {
			err = o.guard.verifyAndThen(ctx, func() error {
				if _, err := s.continuity.agentDecisionResumeRecord(binding, row); err != nil {
					return err
				}
				if err := checkSession(); err != nil {
					return err
				}
				checkedWithOwner = true
				return nil
			})
			if errors.Is(err, errAgentDecisionResumeScope) {
				return nil, err
			}
			drift = drift || err != nil
		}
	}
	// Also protect negative views and failed owner checks from scope changes.
	if !checkedWithOwner {
		if err := checkSession(); err != nil {
			return nil, err
		}
	}
	if drift {
		o.projection = cloneJSONMap(o.projection)
		o.projection["status"], o.projection["reason"] = "unknown", "final_owner_or_session_drift"
		if o.projection["acceptance_criteria"] != "not_provided" {
			o.projection["acceptance_criteria"] = "unknown"
		}
		return o.render(row, events, now, compact), nil
	}
	return payload, nil
}

func (s *server) buildAgentDecisionContextPackage(ctx context.Context, row map[string]any, events []map[string]any, now time.Time, compact bool) (map[string]any, error) {
	observation := s.observeAgentDecisionResume(ctx, row, now)
	payload := observation.render(row, events, now, compact)
	return observation.finish(ctx, s, row, events, now, compact, payload)
}
