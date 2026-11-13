package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"time"
)

const agentDecisionAckSchemaID = "session_decision_ack.v1"
const agentDecisionReceiptSchemaID = "session_decision_receipt.v1"
const agentDecisionReceiptWindow = 16

type agentDecisionAck struct {
	SchemaID string                `json:"schema_id"`
	Sequence uint64                `json:"sequence"`
	Expected agentDecisionBaseline `json:"expected_baseline"`
}

type agentDecisionReceipt struct {
	SchemaID      string                `json:"schema_id"`
	Sequence      uint64                `json:"sequence"`
	RequestDigest string                `json:"request_digest"`
	Baseline      agentDecisionBaseline `json:"baseline"`
	Event         map[string]any        `json:"event"`
}

type agentDecisionAckRequest struct {
	selected *agentDecisionBinding
	ack      agentDecisionAck
	eventID  string
	digest   string
}

func parseAgentDecisionAck(sessionID string, payload map[string]any) (agentDecisionAckRequest, error) {
	fail := func() (agentDecisionAckRequest, error) { return agentDecisionAckRequest{}, errAgentDecisionConflict }
	allowed := map[string]bool{"type": true, "event_id": true, "decision_binding": true, "decision_ack": true,
		"session_id": true, "project": true, "agent": true, "agent_id": true}
	for key := range payload {
		if !allowed[key] {
			return fail()
		}
	}
	if payload["type"] != "decision.bound" || validateAgentSessionID(sessionID) != nil ||
		(payload["session_id"] != nil && payload["session_id"] != sessionID) {
		return fail()
	}
	eventID, ok := payload["event_id"].(string)
	if !ok || !decisionApplicabilityText(eventID, 128) || !continuityIDPattern.MatchString(eventID) {
		return fail()
	}
	selected, err := normalizeAgentDecisionBinding(payload["decision_binding"], false)
	if err != nil {
		return fail()
	}
	encoded, err := json.Marshal(payload["decision_ack"])
	if err != nil || !decisionExactJSONFields(encoded, "schema_id", "sequence", "expected_baseline") {
		return fail()
	}
	var ack agentDecisionAck
	if json.Unmarshal(encoded, &ack) != nil || ack.SchemaID != agentDecisionAckSchemaID || ack.Sequence == 0 || ack.Sequence > agentDecisionMaxRevision {
		return fail()
	}
	var object map[string]any
	_ = json.Unmarshal(encoded, &object)
	expected, err := normalizeAgentDecisionBaseline(object["expected_baseline"])
	if err != nil || expected.Revision == agentDecisionMaxRevision || ack.Sequence != expected.Revision+1 {
		return fail()
	}
	ack.Expected = expected
	raw, err := json.Marshal(payload)
	if err != nil || len(raw) > 4096 {
		return fail()
	}
	return agentDecisionAckRequest{selected: selected, ack: ack, eventID: eventID,
		digest: agentDecisionDigest(map[string]any{"session_id": sessionID, "payload": payload})}, nil
}

func agentDecisionReceipts(row map[string]any) ([]agentDecisionReceipt, error) {
	raw, present := row["decision_receipts"]
	if !present {
		return nil, nil
	}
	encoded, err := json.Marshal(raw)
	var rows []json.RawMessage
	if err != nil || len(encoded) > 65536 || json.Unmarshal(encoded, &rows) != nil || len(rows) == 0 || len(rows) > agentDecisionReceiptWindow {
		return nil, errAgentDecisionConflict
	}
	current, err := agentDecisionBaselineForRow(row)
	if err != nil {
		return nil, err
	}
	receipts := make([]agentDecisionReceipt, 0, len(rows))
	for _, encoded := range rows {
		var receipt agentDecisionReceipt
		var fields map[string]json.RawMessage
		if !decisionExactJSONFields(encoded, "schema_id", "sequence", "request_digest", "baseline", "event") || json.Unmarshal(encoded, &receipt) != nil ||
			json.Unmarshal(encoded, &fields) != nil || receipt.SchemaID != agentDecisionReceiptSchemaID || !isHexDigest(receipt.RequestDigest) ||
			receipt.Sequence == 0 || receipt.Sequence > current.Revision {
			return nil, errAgentDecisionConflict
		}
		var baselineRaw any
		_ = json.Unmarshal(fields["baseline"], &baselineRaw)
		baseline, err := normalizeAgentDecisionBaseline(baselineRaw)
		if err != nil || baseline != receipt.Baseline || baseline.Incarnation != current.Incarnation || baseline.Revision != receipt.Sequence ||
			(len(receipts) > 0 && receipts[len(receipts)-1].Sequence+1 != receipt.Sequence) {
			return nil, errAgentDecisionConflict
		}
		event := receipt.Event
		binding, bindingErr := normalizeAgentDecisionBinding(anyMap(event["metadata"])["decision_binding"], true)
		if bindingErr != nil || len(fields["event"]) > 4096 || event["session_id"] != row["id"] || event["type"] != "decision.bound" ||
			!decisionApplicabilityText(anyToString(event["id"]), 128) || !continuityIDPattern.MatchString(anyToString(event["id"])) ||
			agentDecisionDigest(binding) != baseline.BindingDigest || !reflect.DeepEqual(anyMap(event["metadata"])["decision_baseline"], agentDecisionJSONMap(baseline)) {
			return nil, errAgentDecisionConflict
		}
		receipts = append(receipts, receipt)
	}
	if receipts[len(receipts)-1].Baseline != current {
		return nil, errAgentDecisionConflict
	}
	return receipts, nil
}

func agentDecisionEventRequested(payload map[string]any) bool {
	if normalizeAgentEventType(firstNonEmptyStrings(anyToString(payload["type"]), anyToString(payload["event_type"]), anyToString(payload["eventType"]))) == "decision.bound" {
		return true
	}
	for key := range payload {
		if strings.EqualFold(key, "decision_binding") || strings.EqualFold(key, "decision_ack") {
			return true
		}
	}
	return false
}

func (s *agentSessionStore) acknowledgeDecisionScoped(sessionID string, payload map[string]any, scopeOwnership map[string]any,
	resolve func(*agentDecisionBinding, map[string]any) (*agentDecisionBinding, error)) (map[string]any, map[string]any, error) {
	request, err := parseAgentDecisionAck(sessionID, payload)
	if err != nil || s == nil {
		return nil, nil, errAgentDecisionConflict
	}
	requestedOwnership, err := agentSessionEventOwnership(payload)
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var resolved *agentDecisionBinding
	for {
		if s.decisionUncertain {
			return nil, nil, errAgentDecisionUnavailable
		}
		row, exists := s.sessions[sessionID]
		if !exists || !strings.EqualFold(agentSessionProject(row), request.selected.Project) {
			return nil, nil, errAgentDecisionConflict
		}
		if agentSessionTerminal(agentSessionEffectiveStatus(row, time.Now().UTC(), s.idleTTL)) {
			return nil, nil, errAgentSessionTerminal
		}
		if err := validateAgentSessionEventOwnership(row, requestedOwnership, true); err != nil {
			return nil, nil, err
		}
		candidate := cloneJSONMap(row)
		if _, err := establishAgentSessionScopeOwnership(candidate, scopeOwnership); err != nil {
			return nil, nil, errAgentSessionOwnershipConflict
		}
		if err := validateStoredAgentDecision(row); err != nil {
			return nil, nil, err
		}
		receipts, err := agentDecisionReceipts(row)
		if err != nil {
			return nil, nil, err
		}
		for _, receipt := range receipts {
			if receipt.Sequence == request.ack.Sequence {
				if receipt.RequestDigest != request.digest {
					return nil, nil, errAgentDecisionConflict
				}
				event := cloneJSONMap(receipt.Event)
				event["idempotent_replay"] = true
				return s.effectiveSessionLocked(row, time.Now().UTC()), event, nil
			}
			if receipt.Event["id"] == request.eventID {
				return nil, nil, errAgentDecisionConflict
			}
		}
		baseline, err := agentDecisionBaselineForRow(row)
		if err != nil || baseline != request.ack.Expected {
			return nil, nil, errAgentDecisionConflict
		}
		if resolved == nil {
			if resolve == nil {
				return nil, nil, errAgentDecisionUnavailable
			}
			ownerRow := cloneJSONMap(row)
			s.mu.Unlock()
			resolved, err = func() (*agentDecisionBinding, error) {
				defer s.mu.Lock()
				return resolve(request.selected, ownerRow)
			}()
			if err != nil || resolved == nil {
				return nil, nil, errAgentDecisionConflict
			}
			continue
		}
		selected := *request.selected
		selected.RecordDigest = resolved.RecordDigest
		if _, err := normalizeAgentDecisionBinding(resolved, true); err != nil || !reflect.DeepEqual(&selected, resolved) || !agentDecisionRowScope(candidate, resolved) {
			return nil, nil, errAgentDecisionConflict
		}
		baseline.Revision, baseline.BindingDigest = request.ack.Sequence, agentDecisionDigest(resolved)
		candidate["decision_binding"], candidate["decision_baseline"] = agentDecisionJSONMap(resolved), agentDecisionJSONMap(baseline)
		at := nowUTCISO()
		event := map[string]any{"id": request.eventID, "session_id": sessionID, "type": "decision.bound", "project": row["project"],
			"agent": row["agent"], "agent_id": row["agent_id"], "created_at": at, "summary": "Decision binding acknowledged.",
			"metadata": map[string]any{"decision_binding": agentDecisionJSONMap(resolved), "decision_baseline": agentDecisionJSONMap(baseline)}}
		receipts = append(receipts, agentDecisionReceipt{SchemaID: agentDecisionReceiptSchemaID, Sequence: baseline.Revision,
			RequestDigest: request.digest, Baseline: baseline, Event: event})
		if len(receipts) > agentDecisionReceiptWindow {
			receipts = receipts[len(receipts)-agentDecisionReceiptWindow:]
		}
		storedReceipts := make([]any, 0, len(receipts))
		for _, receipt := range receipts {
			storedReceipts = append(storedReceipts, agentDecisionJSONMap(receipt))
		}
		candidate["decision_receipts"] = storedReceipts
		candidate["updated_at"], candidate["last_event_at"], candidate["last_event_type"] = at, at, "decision.bound"
		candidate["event_count"] = anyToInt(candidate["event_count"], 0) + 1
		previous := s.stageAgentDecisionLocked()
		s.sessions[sessionID] = candidate
		events := append(append([]map[string]any{}, s.events[sessionID]...), event)
		if len(events) > s.maxEvents {
			events = events[len(events)-s.maxEvents:]
		}
		s.events[sessionID] = events
		s.decisionDurableWrite = true
		err = s.persistLocked()
		s.decisionDurableWrite = false
		s.finishAgentDecisionLocked(previous, err)
		if err != nil {
			return nil, nil, err
		}
		return cloneJSONMap(candidate), cloneJSONMap(event), nil
	}
}
