package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/contextlattice/gateway-go/internal/decisionresearch"
)

const (
	temporalDecisionCategory = "decision_change"
	maxDecisionVersions      = 32
)

var decisionSlotPattern = regexp.MustCompile(`^dcs_[0-9a-f]{64}:[0-9]{2}$`)
var decisionPathPattern = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]*(/[A-Za-z0-9_-][A-Za-z0-9_.-]*)*$`)

func reservedDecisionSlot(id string) bool { return decisionSlotPattern.MatchString(id) }

func writeTemporalClaimNotFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "claim not found"})
}

// Topic is an exact ownership boundary. Scope is an exact applicability label
// within it, not a path selector or a credential. Neither is inferred from text.
type temporalDecisionAddress struct {
	Project, Topic, ID, Actor string
}

func (a temporalDecisionAddress) prefix() string {
	data, _ := json.Marshal(a)
	return "dcs_" + sha256Hex(string(data)) + ":"
}

type temporalDecisionInput struct {
	ID        string `json:"id"`
	Effect    string `json:"effect"`
	Expect    *int   `json:"expected_version"`
	Target    *int   `json:"target_version,omitempty"`
	Value     string `json:"value,omitempty"`
	Scope     string `json:"scope,omitempty"`
	ValidFrom string `json:"valid_from,omitempty"`
	ValidTo   string `json:"valid_to,omitempty"`
}

type temporalDecisionQuery struct {
	ID    string `json:"id"`
	Scope string `json:"scope,omitempty"`
	AsOf  string `json:"as_of,omitempty"`
}

type temporalDecisionEvent struct {
	Version int        `json:"version"`
	Target  int        `json:"target,omitempty"`
	Effect  string     `json:"effect"`
	Value   string     `json:"value,omitempty"`
	Scope   string     `json:"scope,omitempty"`
	From    time.Time  `json:"from"`
	Until   *time.Time `json:"until,omitempty"`
}

type decisionValue struct {
	Version int    `json:"version"`
	Value   string `json:"value"`
}

type temporalDecisionView struct {
	State     string          `json:"state"`
	Current   *decisionValue  `json:"current,omitempty"`
	Details   []decisionValue `json:"details,omitempty"`
	Conflicts []int           `json:"conflict_versions,omitempty"`
	base      int
	selected  int
}

func decisionTime(raw string, fallback time.Time) (time.Time, error) {
	if raw == "" {
		return fallback, nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, errCorrectionInvalid
	}
	return t.UTC(), nil
}

func validDecisionScope(scope string) bool {
	return scope == "" || (len(scope) <= 120 && decisionPathPattern.MatchString(scope))
}

func (in temporalDecisionInput) event(now time.Time) (temporalDecisionEvent, error) {
	e := temporalDecisionEvent{Effect: in.Effect, Scope: in.Scope, Value: in.Value}
	if in.Expect == nil || *in.Expect < 0 || *in.Expect >= maxDecisionVersions || !validDecisionScope(in.Scope) ||
		!temporalClaimIDPattern.MatchString(in.ID) || utf8.RuneCountInString(in.Value) > 600 || strings.TrimSpace(in.Value) != in.Value {
		return e, errCorrectionInvalid
	}
	e.Version = *in.Expect + 1
	if in.Target != nil {
		e.Target = *in.Target
		if e.Target < 1 || e.Target >= e.Version {
			return e, errCorrectionInvalid
		}
	}
	initial := in.Effect == "set"
	if initial != (e.Version == 1) || initial != (in.Target == nil) {
		return e, errCorrectionInvalid
	}
	switch in.Effect {
	case "set", "replace":
		if in.Scope != "" || in.Value == "" {
			return e, errCorrectionInvalid
		}
	case "exception", "refine":
		if in.Scope == "" || in.Value == "" {
			return e, errCorrectionInvalid
		}
	case "detail", "contradict":
		if in.Value == "" {
			return e, errCorrectionInvalid
		}
	case "retract", "expire":
		if in.Value != "" || in.ValidTo != "" {
			return e, errCorrectionInvalid
		}
	default:
		return e, errCorrectionInvalid
	}
	var err error
	if e.From, err = decisionTime(in.ValidFrom, now); err != nil {
		return e, err
	}
	if in.ValidTo != "" {
		until, err := decisionTime(in.ValidTo, now)
		if err != nil || (in.ValidFrom != "" && !until.After(e.From)) {
			return e, errCorrectionInvalid
		}
		e.Until = &until
	}
	return e, nil
}

// A replacement ends the old default permanently. Exceptions only mask the
// current default; a withdrawal or half-open expiry removes that mask.
func resolveTemporalDecision(events []temporalDecisionEvent, scope string, at time.Time) temporalDecisionView {
	v := temporalDecisionView{State: "pending"}
	ended := make(map[int]string)
	for _, e := range events {
		if at.Before(e.From) {
			break
		}
		switch e.Effect {
		case "set", "replace":
			v.base = e.Version
		case "retract":
			ended[e.Target] = "retracted"
		case "expire":
			ended[e.Target] = "expired"
		}
		if e.Until != nil && !at.Before(*e.Until) && ended[e.Version] == "" {
			ended[e.Version] = "expired"
		}
	}
	if v.base == 0 {
		return v
	}
	if reason := ended[v.base]; reason != "" {
		v.State = reason
		return v
	}
	v.selected = v.base
	live := func(e temporalDecisionEvent) bool {
		return !at.Before(e.From) && ended[e.Version] == "" && (e.Scope == "" || e.Scope == scope)
	}
	for _, e := range events {
		if live(e) && e.Target == v.base && (e.Effect == "exception" || e.Effect == "refine") {
			v.selected = e.Version
		}
	}
	for _, e := range events {
		if !live(e) || (e.Target != v.selected && e.Target != v.base) {
			continue
		}
		switch e.Effect {
		case "detail":
			v.Details = append(v.Details, decisionValue{e.Version, e.Value})
		case "contradict":
			v.Conflicts = append(v.Conflicts, e.Version)
		}
	}
	if len(v.Conflicts) != 0 {
		v.State, v.Details = "unresolved_contradiction", nil
		return v
	}
	v.State = "active"
	v.Current = &decisionValue{v.selected, events[v.selected-1].Value}
	return v
}

func admitDecisionEvent(events []temporalDecisionEvent, e temporalDecisionEvent) error {
	if e.Version != len(events)+1 {
		return errCorrectionConflict
	}
	if len(events) == 0 {
		return nil // event() already restricted the first version to set.
	}
	if e.From.Before(events[len(events)-1].From) {
		return errCorrectionConflict
	}
	v := resolveTemporalDecision(events, e.Scope, e.From)
	target := events[e.Target-1]
	switch e.Effect {
	case "expire", "retract":
		if target.Scope != e.Scope || v.selected == 0 {
			break
		}
		if e.Target == v.base || e.Target == v.selected {
			return nil
		}
		for _, id := range v.Conflicts {
			if id == e.Target {
				return nil
			}
		}
		// Details are withheld while contested, never promoted to decisions.
		for _, d := range v.Details {
			if d.Version == e.Target {
				return nil
			}
		}
	case "replace", "exception", "refine", "detail", "contradict":
		if e.Effect == "replace" {
			// A broad replacement must not silently settle narrower disputes.
			for _, candidate := range events {
				if candidate.Effect == "contradict" && candidate.Scope != "" &&
					len(resolveTemporalDecision(events, candidate.Scope, e.From).Conflicts) != 0 {
					return errCorrectionConflict
				}
			}
		}
		if v.State == "active" && v.selected == e.Target &&
			(e.Effect != "exception" && e.Effect != "refine" || v.selected == v.base) &&
			(e.Effect != "contradict" || target.Value != e.Value) {
			return nil
		}
	}
	return errCorrectionConflict
}

// One range of the existing unique alternate-ID index, never a ledger scan.
// Verify every receipt and contiguous slot before projecting any value.
func decisionHistory(db *sql.DB, a temporalDecisionAddress) ([]temporalDecisionEvent, error) {
	prefix := a.prefix()
	rows, err := db.Query("SELECT "+correctionReceiptColumns+" FROM receipts WHERE alternate BETWEEN ? AND ? ORDER BY alternate LIMIT ?", prefix+"00", prefix+"99", maxDecisionVersions+1)
	if err != nil {
		return nil, errCorrectionUnavailable
	}
	defer rows.Close()
	var history []temporalDecisionEvent
	for rows.Next() {
		var p correctionReceipt
		if err := rows.Scan(&p.seq, &p.id, &p.scope, &p.alternate, &p.body, &p.digest); err != nil ||
			p.seq < 1 || p.digest != p.bodyDigest() || p.scope != a.Project {
			return nil, errCorrectionUnavailable
		}
		r, err := decodeNamedDecisionRecord(p.body)
		n := len(history) + 1
		if err != nil || n > maxDecisionVersions || r.Category != temporalDecisionCategory || r.Apply || r.Receipt != nil ||
			r.ID != p.id || r.Claim.AgentID != a.Actor || r.Claim.Project != a.Project || r.Claim.TopicPath != a.Topic ||
			r.Claim.ClaimID != fmt.Sprintf("%s%02d", prefix, n) || p.alternate.String != r.Claim.ClaimID ||
			r.Decision == nil || r.Decision.Version != n {
			return nil, errCorrectionUnavailable
		}
		if admitDecisionEvent(history, *r.Decision) != nil {
			return nil, errCorrectionUnavailable
		}
		history = append(history, *r.Decision)
	}
	if rows.Err() != nil {
		return nil, errCorrectionUnavailable
	}
	return history, nil
}

func (s *temporalClaimStore) readDecision(a temporalDecisionAddress, scope string, at time.Time) (temporalDecisionView, int, error) {
	if s == nil || !s.enabled {
		return temporalDecisionView{}, 0, errCorrectionUnavailable
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.correctionUncertain || s.parseErrors != 0 {
		return temporalDecisionView{}, 0, errCorrectionUnavailable
	}
	if s.correctionStamp.Owner == "" {
		return temporalDecisionView{}, 0, nil
	}
	db, _, err := openCorrectionReceiptDB(s.path, correctionClaimReceiptKind, s.correctionStamp.Owner)
	if err != nil {
		return temporalDecisionView{}, 0, errCorrectionUnavailable
	}
	defer db.Close()
	if checkCorrectionReceiptFrontier(db, s.correctionStamp) != nil {
		return temporalDecisionView{}, 0, errCorrectionUnavailable
	}
	history, err := decisionHistory(db, a)
	if err != nil {
		return temporalDecisionView{}, 0, err
	}
	return resolveTemporalDecision(history, scope, at), len(history), nil
}

func (s *temporalClaimStore) changeDecision(a temporalDecisionAddress, in temporalDecisionInput) (int, bool, error) {
	if s == nil || !s.enabled {
		return 0, false, errCorrectionUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	e, err := in.event(now)
	if err != nil {
		return 0, false, err
	}
	if s.correctionUncertain || s.parseErrors != 0 {
		return 0, false, errCorrectionUnavailable
	}
	material, _ := json.Marshal([]any{temporalDecisionCategory, a, in})
	digest := sha256Hex(string(material))
	op := "cor_" + digest
	db, owner, err := openCorrectionReceiptDB(s.path, correctionClaimReceiptKind, s.correctionStamp.Owner)
	if err != nil {
		return 0, false, errCorrectionUnavailable
	}
	defer db.Close()
	tip := s.correctionStamp
	tip.Owner = owner
	if checkCorrectionReceiptFrontier(db, tip) != nil {
		return 0, false, errCorrectionUnavailable
	}
	slot := fmt.Sprintf("%s%02d", a.prefix(), e.Version)
	history, err := decisionHistory(db, a)
	if err != nil {
		return 0, false, err
	}
	if e.Version <= len(history) {
		p, ok, err := readCorrectionReceipt(db.QueryRow(correctionReceiptLookup, op))
		if err != nil {
			return 0, false, errCorrectionUnavailable
		}
		if !ok || p.scope != a.Project || p.alternate.String != slot {
			return 0, false, errCorrectionConflict
		}
		return e.Version, true, nil
	}
	if e.Until != nil && !e.Until.After(e.From) {
		return 0, false, errCorrectionInvalid
	}
	if err := admitDecisionEvent(history, e); err != nil {
		return 0, false, err
	}
	if _, collision := s.claims[slot]; collision {
		return 0, false, errCorrectionConflict
	}
	// Only this neutral stub participates in the legacy receipt envelope.
	// The original value is exclusively in the credential-scoped record.
	claim := temporalClaim{
		SchemaID: temporalClaimContractID, ClaimID: slot, Project: a.Project, TopicPath: a.Topic, AgentID: a.Actor, Revision: 1,
		Verification: map[string]any{"status": "unverified"}, Provenance: map[string]any{"source": temporalDecisionCategory, "run_id": op},
	}
	record := correctionClaimRecord{ID: op, Fingerprint: digest, Category: temporalDecisionCategory, Claim: claim, Apply: true, Decision: &e}
	if err := s.persistCorrectionRecordLocked(db, tip, record); err != nil {
		return 0, false, err
	}
	return e.Version, false, nil
}

func strictDecisionObject(raw any, out any) bool {
	obj, ok := raw.(map[string]any)
	if !ok {
		return false
	}
	for k, v := range obj {
		if v == nil || k != strings.ToLower(k) {
			return false
		}
	}
	data, err := json.Marshal(obj)
	if err != nil || len(data) > 8192 {
		return false
	}
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	return d.Decode(out) == nil
}

func (s *server) handleTemporalDecision(w http.ResponseWriter, r *http.Request, payload map[string]any, wire []byte, raw any, change, tool bool) {
	// Do not inherit the proxy's implicit credential, a worker role, query
	// credentials, or an authority claim from the request body.
	key := r.Header.Get("X-Api-Key")
	if key == "" {
		if scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " "); ok && strings.EqualFold(scheme, "Bearer") {
			key = token
		}
	}
	host, _, peerErr := net.SplitHostPort(r.RemoteAddr)
	if tool || peerErr != nil || !net.ParseIP(host).IsLoopback() || r.Header.Get("Forwarded") != "" ||
		r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Real-IP") != "" || !secureTokenEqual(key, s.orchestratorAPIKey) {
		writeTemporalClaimNotFound(w)
		return
	}
	project, pok := correctionString(payload, "project", 120, true)
	topic, tok := correctionString(payload, "topic_path", 240, true)
	var unique map[string]any
	if _, err := sanitizeMemoryProject(project); err != nil || !pok || !tok || !decisionPathPattern.MatchString(topic) || len(payload) != 5 ||
		len(wire) > 8192 || decisionresearch.Decode(wire, &unique, false) != nil {
		writeCorrectionError(w, errCorrectionInvalid)
		return
	}
	session, sok := correctionString(payload, "session_id", maxAgentSessionIDLength, true)
	actor, aok := correctionString(payload, "agent_id", 120, true)
	if !sok || !aok || validateAgentSessionID(session) != nil {
		writeTemporalClaimNotFound(w)
		return
	}
	a, release, allowed := s.manualDecisionAuthority(r, project, topic, session, actor)
	if !allowed {
		writeTemporalClaimNotFound(w)
		return
	}
	if change {
		var in temporalDecisionInput
		if !strictDecisionObject(raw, &in) {
			release()
			writeCorrectionError(w, errCorrectionInvalid)
			return
		}
		a.ID = in.ID
		version, replay, err := s.temporalClaims.changeDecision(a, in)
		release()
		if err != nil {
			writeCorrectionError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"schema_id": "temporal_decision_receipt.v1", "ok": true, "recorded": true,
			"project": a.Project, "topic_path": topic, "decision_id": a.ID, "agent_id": actor, "session_id": session, "effect": in.Effect, "version": version, "idempotent_replay": replay})
		return
	}
	var q temporalDecisionQuery
	if !strictDecisionObject(raw, &q) || !temporalClaimIDPattern.MatchString(q.ID) || !validDecisionScope(q.Scope) {
		release()
		writeCorrectionError(w, errCorrectionInvalid)
		return
	}
	at, err := decisionTime(q.AsOf, time.Now().UTC())
	if err != nil {
		release()
		writeCorrectionError(w, err)
		return
	}
	a.ID = q.ID
	view, n, err := s.temporalClaims.readDecision(a, q.Scope, at)
	release()
	if err != nil {
		writeCorrectionError(w, err)
		return
	}
	if n == 0 {
		writeTemporalClaimNotFound(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schema_id": "temporal_decision_context.v1", "ok": true,
		"project": a.Project, "topic_path": topic, "decision_id": a.ID, "agent_id": actor, "session_id": session, "scope": q.Scope, "as_of": at, "version": n, "decision": view})
}
