package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
)

const agentDecisionBindingSchemaID = "session_decision_binding.v1"
const agentDecisionBaselineSchemaID = "session_decision_baseline.v1"
const agentDecisionMaxRevision = uint64(9007199254740991)

var errAgentDecisionConflict = errors.New("agent_session_decision_conflict")
var errAgentDecisionUnavailable = errors.New("agent_session_decision_unavailable: restart and verify durable state before retry")

func agentDecisionContinuityCapability() map[string]any {
	return map[string]any{"version": 1, "manual_binding": true,
		"applicability_schema": decisionApplicabilitySchemaID, "session_binding_schema": agentDecisionBindingSchemaID,
		"owner_version_only": true, "automatic_execution": false}
}

func writeAgentDecisionPersistenceFailure(w http.ResponseWriter, err error) bool {
	if !ownerOnlyAtomicWriteCommitted(err) && !errors.Is(err, errAgentDecisionUnavailable) {
		return false
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "agent_session_decision_commit_unknown",
		"detail": "Session snapshot replacement requires restart and durable-state verification before retry; do not assume the operation was rolled back."})
	return true
}

type agentSessionTaskLinks map[string]bool

const agentSessionTaskAliasProofLimit = 1024

func agentSessionTasksLinked(left, right string, proofs ...agentSessionTaskLinks) bool {
	if left == right {
		return true
	}
	return len(proofs) == 1 && proofs[0][left] && proofs[0][right]
}

// Resolve a bounded set of existing session IDs through the trusted identity
// owner. This ephemeral proof is never read from or written into caller JSON.
func (s *server) agentSessionRequestTaskLinks(payload map[string]any) (agentSessionTaskLinks, error) {
	requested := anyToString(payload["task_identity_id"])
	project, repo := agentSessionProject(payload), anyToString(payload["repo"])
	ensure := anyToBool(firstNonEmptyAny(payload["ensure"], payload["reuse_existing"], payload["reuseExisting"]))
	if s == nil || s.continuity == nil || s.agentSessions == nil || !ensure || requested == "" || project == "" {
		return nil, nil
	}
	ids := map[string]bool{requested: true}
	complete := true
	s.agentSessions.mu.RLock()
	collect := func(row map[string]any) bool {
		if strings.EqualFold(agentSessionProject(row), project) && (repo == "" || strings.EqualFold(anyToString(row["repo"]), repo)) {
			if id := anyToString(row["task_identity_id"]); id != "" && !ids[id] {
				if len(ids) >= agentSessionTaskAliasProofLimit {
					return false
				}
				ids[id] = true
			}
		}
		return true
	}
	if selectedID := anyToString(payload["session_id"]); selectedID != "" {
		complete = collect(s.agentSessions.sessions[selectedID])
	} else {
		for _, row := range s.agentSessions.sessions {
			if !collect(row) {
				complete = false
				break
			}
		}
	}
	s.agentSessions.mu.RUnlock()
	if !complete {
		return nil, fmt.Errorf("%w: task alias proof exceeds its bound; select an explicit session id", errAgentSessionReuseConflict)
	}
	s.continuity.mu.RLock()
	defer s.continuity.mu.RUnlock()
	if !s.continuity.enabled || s.continuity.lastError != "" {
		return nil, nil
	}
	canonical, err := s.continuity.resolveTaskIdentityLinkLocked(requested, project)
	if err != nil || canonical == "" {
		return nil, nil
	}
	owner := s.continuity.taskIdentities[canonical]
	if repo != "" && !strings.EqualFold(owner.Repo, repo) {
		return nil, nil
	}
	proof := agentSessionTaskLinks{requested: true}
	for id := range ids {
		linked, err := s.continuity.resolveTaskIdentityLinkLocked(id, project)
		if err == nil && linked == canonical {
			proof[id] = true
		}
	}
	return proof, nil
}

type agentDecisionBinding struct {
	SchemaID         string `json:"schema_id"`
	Project          string `json:"project"`
	ObjectiveID      string `json:"objective_id"`
	TaskIdentityID   string `json:"task_identity_id"`
	DecisionChangeID string `json:"decision_change_id"`
	AfterDecisionID  string `json:"after_decision_id"`
	Repo             string `json:"repo"`
	Branch           string `json:"branch"`
	RecordDigest     string `json:"record_digest,omitempty"`
}

type agentDecisionBaseline struct {
	SchemaID      string `json:"schema_id"`
	Incarnation   string `json:"incarnation"`
	Revision      uint64 `json:"revision"`
	BindingDigest string `json:"binding_digest"`
}

func agentDecisionJSONMap(value any) map[string]any {
	raw, _ := json.Marshal(value)
	var object map[string]any
	_ = json.Unmarshal(raw, &object)
	return object
}

func agentDecisionDigest(value any) string {
	raw, _ := json.Marshal(value)
	return sha256Hex(string(raw))
}

func rejectAgentDecisionAliases(payload map[string]any) error {
	for key := range payload {
		for _, canonical := range []string{"decision_binding", "decision_ack", "decision_baseline", "decision_receipts"} {
			if key != canonical && strings.EqualFold(key, canonical) {
				return errAgentDecisionConflict
			}
		}
	}
	return nil
}

func parseAgentDecisionStart(payload map[string]any) (*agentDecisionBinding, error) {
	if err := rejectAgentDecisionAliases(payload); err != nil {
		return nil, err
	}
	for _, field := range []string{"decision_ack", "decision_baseline", "decision_receipts"} {
		if _, present := payload[field]; present {
			return nil, errAgentDecisionConflict
		}
	}
	raw, present := payload["decision_binding"]
	if !present {
		return nil, nil
	}
	binding, err := normalizeAgentDecisionBinding(raw, false)
	if err != nil || !agentDecisionRowScope(payload, binding) {
		return nil, errAgentDecisionConflict
	}
	task, ok := payload["task_identity_id"].(string)
	if !ok || !decisionApplicabilityText(task, 160) || !continuityIDPattern.MatchString(task) {
		return nil, errAgentDecisionConflict
	}
	return binding, nil
}

func normalizeAgentDecisionBinding(raw any, stored bool) (*agentDecisionBinding, error) {
	fields := []string{"schema_id", "project", "objective_id", "task_identity_id", "decision_change_id", "after_decision_id", "repo", "branch"}
	if stored {
		fields = append(fields, "record_digest")
	}
	encoded, err := json.Marshal(raw)
	if err != nil || len(encoded) > 2048 || !decisionExactJSONFields(encoded, fields...) {
		return nil, errAgentDecisionConflict
	}
	var binding agentDecisionBinding
	if json.Unmarshal(encoded, &binding) != nil || binding.SchemaID != agentDecisionBindingSchemaID {
		return nil, errAgentDecisionConflict
	}
	project, err := sanitizeMemoryProject(binding.Project)
	if err != nil || project != binding.Project || !decisionApplicabilityText(binding.Repo, 240) || !decisionApplicabilityText(binding.Branch, 160) {
		return nil, errAgentDecisionConflict
	}
	for _, id := range []string{binding.ObjectiveID, binding.TaskIdentityID, binding.DecisionChangeID, binding.AfterDecisionID} {
		if !decisionApplicabilityText(id, 160) || !continuityIDPattern.MatchString(id) {
			return nil, errAgentDecisionConflict
		}
	}
	if stored && (!isHexDigest(binding.RecordDigest) || binding.RecordDigest != strings.ToLower(binding.RecordDigest)) {
		return nil, errAgentDecisionConflict
	}
	return &binding, nil
}

func normalizeAgentDecisionBaseline(raw any) (agentDecisionBaseline, error) {
	var baseline agentDecisionBaseline
	encoded, err := json.Marshal(raw)
	if err != nil || !decisionExactJSONFields(encoded, "schema_id", "incarnation", "revision", "binding_digest") ||
		json.Unmarshal(encoded, &baseline) != nil || baseline.SchemaID != agentDecisionBaselineSchemaID ||
		!decisionApplicabilityText(baseline.Incarnation, 80) || !continuityIDPattern.MatchString(baseline.Incarnation) ||
		baseline.Revision > agentDecisionMaxRevision || (baseline.Revision == 0) != (baseline.BindingDigest == "") ||
		(baseline.BindingDigest != "" && (!isHexDigest(baseline.BindingDigest) || baseline.BindingDigest != strings.ToLower(baseline.BindingDigest))) {
		return agentDecisionBaseline{}, errAgentDecisionConflict
	}
	return baseline, nil
}

func agentDecisionBaselineForRow(row map[string]any) (agentDecisionBaseline, error) {
	if raw, present := row["decision_baseline"]; present {
		return normalizeAgentDecisionBaseline(raw)
	}
	// Legacy reads do not mutate the snapshot. New rows use random incarnations,
	// so recreating the same session ID cannot reuse this historical baseline.
	return agentDecisionBaseline{SchemaID: agentDecisionBaselineSchemaID,
		Incarnation: "legacy_" + agentDecisionDigest(map[string]any{"id": row["id"], "started_at": row["started_at"],
			"project": row["project"], "agent_id": row["agent_id"], "scope_ownership": row["scope_ownership"]})[:32]}, nil
}

func initializeAgentDecisionBaseline(row map[string]any, binding *agentDecisionBinding) {
	baseline := agentDecisionBaseline{SchemaID: agentDecisionBaselineSchemaID, Incarnation: "sdi_" + randomHex(16)}
	if binding != nil {
		row["decision_binding"] = agentDecisionJSONMap(binding)
		baseline.Revision, baseline.BindingDigest = 1, agentDecisionDigest(binding)
	}
	row["decision_baseline"] = agentDecisionJSONMap(baseline)
}

func validateStoredAgentDecision(row map[string]any) error {
	if err := rejectAgentDecisionAliases(row); err != nil {
		return err
	}
	baseline, err := agentDecisionBaselineForRow(row)
	if err != nil {
		return err
	}
	if raw, present := row["decision_binding"]; present {
		binding, err := normalizeAgentDecisionBinding(raw, true)
		if err != nil || baseline.BindingDigest != agentDecisionDigest(binding) || baseline.Revision == 0 || !agentDecisionRowScope(row, binding) {
			return errAgentDecisionConflict
		}
	} else if baseline.Revision != 0 {
		return errAgentDecisionConflict
	}
	if _, err := agentDecisionReceipts(row); err != nil {
		return err
	}
	return nil
}

func agentDecisionRowScope(row map[string]any, binding *agentDecisionBinding) bool {
	return binding != nil && strings.EqualFold(agentSessionProject(row), binding.Project) &&
		strings.EqualFold(anyToString(row["repo"]), binding.Repo) && anyToString(row["branch"]) == binding.Branch &&
		anyToString(row["task_identity_id"]) != ""
}

func (s *continuityStore) resolveAgentDecisionBinding(selected *agentDecisionBinding, row map[string]any) (*agentDecisionBinding, error) {
	if s == nil || selected == nil || !agentDecisionRowScope(row, selected) {
		return nil, errAgentDecisionConflict
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.enabled || s.lastError != "" {
		return nil, errAgentDecisionUnavailable
	}
	index, found := s.decisionChangeIndex[continuityScopedIndexKey(selected.Project, selected.DecisionChangeID)]
	if !found || index < 0 || index >= len(s.decisionChanges) {
		return nil, errAgentDecisionConflict
	}
	change := s.decisionChanges[index]
	transitionIndex, found := s.decisionTransitionIndex[continuityScopedIndexKey(selected.Project, selected.DecisionChangeID)]
	if !found || transitionIndex < 0 || transitionIndex >= len(s.objectiveTransitions) ||
		change.ObjectiveID != selected.ObjectiveID || change.TaskIdentityID != selected.TaskIdentityID || change.After.DecisionID != selected.AfterDecisionID {
		return nil, errAgentDecisionConflict
	}
	transition := s.objectiveTransitions[transitionIndex]
	if transition.DecisionChangeID != change.DecisionChangeID || transition.ObjectiveID != change.ObjectiveID ||
		transition.TaskIdentityID != change.TaskIdentityID || !strings.EqualFold(transition.Project, change.Project) {
		return nil, errAgentDecisionConflict
	}
	identity, err := s.resolveTaskIdentityLinkLocked(change.TaskIdentityID, change.Project)
	rowIdentity, rowErr := s.resolveTaskIdentityLinkLocked(anyToString(row["task_identity_id"]), selected.Project)
	if err != nil || rowErr != nil || identity == "" || identity != rowIdentity {
		return nil, errAgentDecisionConflict
	}
	task, exists := s.taskIdentities[identity]
	if !exists || !strings.EqualFold(task.Repo, selected.Repo) {
		return nil, errAgentDecisionConflict
	}
	if change.Applicability != nil && (change.Applicability.Scope.Branch != selected.Branch || !strings.EqualFold(change.Applicability.Scope.Repo, selected.Repo)) {
		return nil, errAgentDecisionConflict
	}
	resolved := *selected
	resolved.RecordDigest = agentDecisionDigest(change)
	return &resolved, nil
}

func agentDecisionStartReuse(row map[string]any, binding *agentDecisionBinding) error {
	if err := validateStoredAgentDecision(row); err != nil {
		return err
	}
	if binding == nil {
		return nil
	}
	existing, err := normalizeAgentDecisionBinding(row["decision_binding"], true)
	if err != nil || !reflect.DeepEqual(existing, binding) {
		return errAgentDecisionConflict
	}
	return nil
}

type agentDecisionStoreSnapshot struct {
	sessions map[string]map[string]any
	events   map[string][]map[string]any
	order    []string
	writes   uint64
}

func (s *agentSessionStore) stageAgentDecisionLocked() agentDecisionStoreSnapshot {
	previous := agentDecisionStoreSnapshot{s.sessions, s.events, s.order, s.successfulSnapshotWrites}
	s.sessions = make(map[string]map[string]any, len(previous.sessions))
	for key, row := range previous.sessions {
		s.sessions[key] = row
	}
	s.events = make(map[string][]map[string]any, len(previous.events))
	for key, events := range previous.events {
		s.events[key] = events
	}
	s.order = append([]string{}, previous.order...)
	return previous
}

func (s *agentSessionStore) finishAgentDecisionLocked(previous agentDecisionStoreSnapshot, err error) {
	if err == nil {
		return
	}
	if ownerOnlyAtomicWriteCommitted(err) {
		s.decisionUncertain = true
		return
	}
	if s.successfulSnapshotWrites != previous.writes {
		return // A completed replacement may report a separate reconciliation problem.
	}
	s.sessions, s.events, s.order = previous.sessions, previous.events, previous.order
}

func (s *agentSessionStore) hasAgentDecisionCustodyLocked() bool {
	for _, row := range s.sessions {
		if _, bound := row["decision_binding"]; bound {
			return true
		}
		if _, receipts := row["decision_receipts"]; receipts {
			return true
		}
	}
	return false
}

// Every writer shares this snapshot root, including ordinary events and
// retention. Preserve custody even when this particular request is untyped.
func (s *agentSessionStore) withAgentSessionMutationLocked(forceDurable bool, mutate func() error) error {
	if s.decisionUncertain {
		return errAgentDecisionUnavailable
	}
	durableBefore := s.decisionDurableWrite
	s.decisionDurableWrite = forceDurable || durableBefore || s.hasAgentDecisionCustodyLocked()
	defer func() { s.decisionDurableWrite = durableBefore }()
	previous := s.stageAgentDecisionLocked()
	err := mutate()
	s.finishAgentDecisionLocked(previous, err)
	return err
}

func (s *agentSessionStore) startOrReuseDecisionScoped(payload map[string]any, scopeOwnership map[string]any, binding *agentDecisionBinding, taskLinks ...agentSessionTaskLinks) (map[string]any, bool, error) {
	if s == nil {
		return nil, false, errAgentDecisionUnavailable
	}
	selected, err := parseAgentDecisionStart(payload)
	if err != nil || selected == nil || binding == nil {
		return nil, false, errAgentDecisionConflict
	}
	selected.RecordDigest = binding.RecordDigest
	if !reflect.DeepEqual(selected, binding) {
		return nil, false, errAgentDecisionConflict
	}
	if _, err := normalizeAgentDecisionBinding(binding, true); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.decisionUncertain {
		return nil, false, errAgentDecisionUnavailable
	}
	var row map[string]any
	var reused bool
	err = s.withAgentSessionMutationLocked(true, func() error {
		var err error
		row, reused, err = s.startOrReuseScopedLocked(payload, scopeOwnership, binding, taskLinks...)
		return err
	})
	return row, reused, err
}
