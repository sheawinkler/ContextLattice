package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"
)

const namedDecisionPrefix = `{"named_decision":`

func readManualClaimBody(w http.ResponseWriter, r *http.Request) (map[string]any, []byte, error) {
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	}
	wire, err := readRequestBody(r)
	if err != nil {
		return nil, nil, err
	}
	if len(bytes.TrimSpace(wire)) == 0 {
		return map[string]any{}, wire, nil
	}
	payload, err := parseJSONMap(wire)
	return payload, wire, err
}

// Share atomic receipt custody, not feedback semantics. In particular a
// manual instruction has no feedback ID and must not pass as a factual claim.
type namedDecisionEnvelope struct {
	Record correctionClaimRecord `json:"named_decision"`
}

func decodeNamedDecisionRecord(raw []byte) (correctionClaimRecord, error) {
	var r correctionClaimRecord
	if err := json.Unmarshal(raw, &r); err != nil || !correctionDigestPattern.MatchString(r.Fingerprint) ||
		r.ID != "cor_"+r.Fingerprint || r.Category != temporalDecisionCategory || r.Target != "" ||
		!reservedDecisionSlot(r.Claim.ClaimID) || r.Claim.Provenance["run_id"] != r.ID ||
		r.Claim.Provenance["source"] != temporalDecisionCategory || r.Claim.Verification["status"] != "unverified" ||
		r.Claim.Project == "" || r.Claim.TopicPath == "" || r.Claim.AgentID == "" || r.Decision == nil {
		return r, errCorrectionUnavailable
	}
	e := r.Decision
	n := e.Version - 1
	in := temporalDecisionInput{ID: r.Claim.ClaimID, Expect: &n, Effect: e.Effect, Scope: e.Scope, Value: e.Value}
	if e.Target != 0 {
		in.Target = &e.Target
	}
	if e.Until != nil {
		in.ValidTo = e.Until.Format(time.RFC3339Nano)
	}
	if _, err := in.event(e.From); err != nil || e.From.IsZero() || (e.Until != nil && !e.Until.After(e.From)) {
		return r, errCorrectionUnavailable
	}
	return r, nil
}

func (replay *correctionClaimReplay) consumeNamedDecision(path, line string) error {
	var row struct {
		Record json.RawMessage `json:"named_decision"`
	}
	if json.Unmarshal([]byte(line), &row) != nil {
		return errCorrectionUnavailable
	}
	r, err := decodeNamedDecisionRecord(row.Record)
	if err != nil || !r.Apply || r.Receipt == nil {
		return errCorrectionUnavailable
	}
	return replay.index(path, r)
}

// The selected session is evidence of the public actor/project scope. Hold
// its owner lock from validation through the scoped receipt operation.
func (s *server) manualDecisionAuthority(_ *http.Request, project, topic, session, actor string) (temporalDecisionAddress, func(), bool) {
	store := s.agentSessions
	if store == nil {
		return temporalDecisionAddress{}, nil, false
	}
	store.mu.Lock()
	row := store.sessions[session]
	if !publicCorrectionSessionMatches(row, project, actor) || row["id"] != session || row["status"] != "active" ||
		agentSessionEffectiveStatus(row, time.Now(), store.idleTTL) != "active" || store.decisionUncertain || store.correctionUncertain {
		store.mu.Unlock()
		return temporalDecisionAddress{}, nil, false
	}
	return temporalDecisionAddress{Project: project, Topic: topic, Actor: actor}, store.mu.Unlock, true
}
