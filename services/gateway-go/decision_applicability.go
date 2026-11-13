package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"unicode"
)

const decisionApplicabilitySchemaID = "decision_applicability.v1"

var errDecisionApplicabilityOwner = errors.New("decision_applicability_owner_unavailable_or_changed")

type decisionApplicabilityScope struct {
	Project        string `json:"project"`
	ObjectiveID    string `json:"objective_id"`
	TaskIdentityID string `json:"task_identity_id"`
	Repo           string `json:"repo"`
	Branch         string `json:"branch"`
}

type decisionInputPin struct {
	MemoryID          string `json:"memory_id"`
	TargetEventID     string `json:"target_event_id"`
	TargetContentHash string `json:"target_content_hash"`
}

type decisionAssumption struct {
	AssumptionID string `json:"assumption_id"`
	InputID      string `json:"input_id"`
	Role         string `json:"role"`
}

type decisionApplicability struct {
	SchemaID    string                     `json:"schema_id"`
	Scope       decisionApplicabilityScope `json:"scope"`
	Inputs      []decisionInputPin         `json:"inputs"`
	Assumptions []decisionAssumption       `json:"assumptions"`
}

func decisionApplicabilityText(value string, limit int) bool {
	return value != "" && len(value) <= limit && value == strings.TrimSpace(value) &&
		!strings.ContainsAny(value, "\u2028\u2029") && strings.IndexFunc(value, unicode.IsControl) < 0
}

func decisionExactJSONFields(raw json.RawMessage, fields ...string) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) != len(fields) {
		return false
	}
	for _, field := range fields {
		if _, present := object[field]; !present {
			return false
		}
	}
	return true
}

func decisionApplicabilityExactFields(encoded []byte) bool {
	if !decisionExactJSONFields(encoded, "schema_id", "scope", "inputs", "assumptions") {
		return false
	}
	var root map[string]json.RawMessage
	_ = json.Unmarshal(encoded, &root)
	if !decisionExactJSONFields(root["scope"], "project", "objective_id", "task_identity_id", "repo", "branch") {
		return false
	}
	for _, spec := range []struct {
		key    string
		fields []string
	}{
		{"inputs", []string{"memory_id", "target_event_id", "target_content_hash"}},
		{"assumptions", []string{"assumption_id", "input_id", "role"}},
	} {
		var rows []json.RawMessage
		if json.Unmarshal(root[spec.key], &rows) != nil || rows == nil {
			return false
		}
		for _, row := range rows {
			if !decisionExactJSONFields(row, spec.fields...) {
				return false
			}
		}
	}
	return true
}

func normalizeDecisionApplicability(raw any) (*decisionApplicability, error) {
	encoded, err := json.Marshal(raw)
	if err != nil || len(encoded) > 8192 || !decisionApplicabilityExactFields(encoded) {
		return nil, errors.New("applicability must be a bounded non-null object")
	}
	var value decisionApplicability
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || value.SchemaID != decisionApplicabilitySchemaID {
		return nil, errors.New("invalid decision applicability schema or fields")
	}
	scope := value.Scope
	project, projectErr := sanitizeMemoryProject(scope.Project)
	if projectErr != nil || project != scope.Project || !continuityIDPattern.MatchString(scope.ObjectiveID) ||
		!continuityIDPattern.MatchString(scope.TaskIdentityID) || !decisionApplicabilityText(scope.Repo, 320) ||
		!decisionApplicabilityText(scope.Branch, 160) {
		return nil, errors.New("applicability requires exact project, objective, task, repo and branch scope")
	}
	if len(value.Inputs) < 1 || len(value.Inputs) > 8 || value.Assumptions == nil || len(value.Assumptions) > 4 {
		return nil, errors.New("applicability requires 1-8 inputs and an explicit list of at most 4 assumptions")
	}
	inputs := map[string]bool{}
	exactInputs := map[string]bool{}
	for i := range value.Inputs {
		input := &value.Inputs[i]
		ownerProject, _, canonical, _, err := canonicalMemoryID(input.MemoryID)
		key := strings.ToLower(input.MemoryID)
		if err != nil || canonical != input.MemoryID || !strings.EqualFold(ownerProject, scope.Project) || inputs[key] ||
			!continuityIDPattern.MatchString(input.TargetEventID) {
			return nil, errors.New("applicability inputs require distinct canonical same-project owners and original event pins")
		}
		input.TargetContentHash = normalizeProjectedContentHash(input.TargetContentHash)
		if !isHexDigest(input.TargetContentHash) {
			return nil, errors.New("applicability input content hash must be SHA256")
		}
		inputs[key] = true
		exactInputs[input.MemoryID] = true
	}
	assumptions := map[string]bool{}
	for _, assumption := range value.Assumptions {
		if !continuityIDPattern.MatchString(assumption.AssumptionID) || len(assumption.AssumptionID) > 80 ||
			assumptions[assumption.AssumptionID] || !exactInputs[assumption.InputID] ||
			(assumption.Role != "acceptance_criteria" && assumption.Role != "source_condition") {
			return nil, errors.New("assumptions require unique names, pinned input owners and an explicit supported role")
		}
		assumptions[assumption.AssumptionID] = true
	}
	sort.Slice(value.Inputs, func(i, j int) bool { return value.Inputs[i].MemoryID < value.Inputs[j].MemoryID })
	sort.Slice(value.Assumptions, func(i, j int) bool { return value.Assumptions[i].AssumptionID < value.Assumptions[j].AssumptionID })
	return &value, nil
}

func validateRawDecisionApplicability(payload map[string]any) error {
	for key := range payload {
		for _, canonical := range []string{"applicability", "request_task_identity_id"} {
			if key != canonical && strings.EqualFold(key, canonical) {
				return errors.New("decision applicability fields require exact canonical names")
			}
		}
	}
	if raw, present := payload["applicability"]; present {
		_, err := normalizeDecisionApplicability(raw)
		return err
	}
	return nil
}

func validateDecisionApplicabilityShape(change decisionChange) error {
	if change.Applicability == nil {
		if change.RequestTaskIdentityID != "" {
			return errors.New("request task selection requires typed applicability")
		}
		return nil
	}
	a, err := normalizeDecisionApplicability(change.Applicability)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(a, change.Applicability) || !strings.EqualFold(a.Scope.Project, change.Project) ||
		a.Scope.ObjectiveID != change.ObjectiveID || change.TaskIdentityID == "" ||
		!decisionApplicabilityText(change.RequestTaskIdentityID, 160) || !continuityIDPattern.MatchString(change.RequestTaskIdentityID) {
		return errors.New("decision applicability is noncanonical or contradicts decision scope")
	}
	return nil
}

func validateStoredDecisionApplicability(payload map[string]any) error {
	if err := validateRawDecisionApplicability(payload); err != nil {
		return err
	}
	_, typed := payload["applicability"]
	request, selected := payload["request_task_identity_id"]
	if typed != selected {
		return errors.New("stored applicability requires its original request task selection")
	}
	if selected {
		value, ok := request.(string)
		if !ok || !decisionApplicabilityText(value, 160) || !continuityIDPattern.MatchString(value) {
			return errors.New("invalid stored request task selection")
		}
	}
	return nil
}

func (s *continuityStore) validateDecisionApplicabilityScopeLocked(change decisionChange) error {
	if err := validateDecisionApplicabilityShape(change); err != nil || change.Applicability == nil {
		return err
	}
	scope := change.Applicability.Scope
	identity, err := s.resolveTaskIdentityLinkLocked(scope.TaskIdentityID, change.Project)
	if err != nil || identity != change.TaskIdentityID {
		return errors.New("applicability task must resolve to the decision's known same-project task")
	}
	requested, err := s.resolveTaskIdentityLinkLocked(change.RequestTaskIdentityID, change.Project)
	if err != nil || requested != identity {
		return errors.New("original request task must resolve to the decision's known same-project task")
	}
	record, ok := s.taskIdentities[identity]
	if !ok || record.Repo == "" || !strings.EqualFold(record.Repo, scope.Repo) {
		return errors.New("applicability repo must match its authoritative task identity")
	}
	return nil
}

// This is an owner observation, not semantic truth, permission or serving
// authorization. Append retains the checked tuple even if its owner later changes.
func (m *memoryStore) validateDecisionApplicabilityOwner(ctx context.Context, a *decisionApplicability) error {
	if m == nil || !m.isEnabled() || strings.TrimSpace(m.policy.edgePath) == "" || a == nil {
		return errDecisionApplicabilityOwner
	}
	fence, err := m.acquireMemoryEdgeLogFenceOptionalContext(ctx)
	if err != nil || fence == nil {
		return errDecisionApplicabilityOwner
	}
	defer fence.release()
	walk := newMemoryDerivationWalk()
	for _, input := range a.Inputs {
		project, file, _, _, err := canonicalMemoryID(input.MemoryID)
		if err != nil || !strings.EqualFold(project, a.Scope.Project) {
			return errDecisionApplicabilityOwner
		}
		entry, ok := m.currentEntry(project, file)
		pin := memoryStructuredReference{TargetID: input.MemoryID, TargetEventID: input.TargetEventID, TargetContentHash: input.TargetContentHash}
		if !ok || !memoryDerivationReferenceMatchesTarget(pin, entry) || m.checkMemoryDerivation(ctx, entry, fence, walk, 0) != nil {
			return errDecisionApplicabilityOwner
		}
	}
	return nil
}
