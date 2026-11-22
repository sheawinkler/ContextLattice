package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	correctionClaimRecordPrefix = `{"correction_operation_id":`
)

var (
	correctionOperationPattern = regexp.MustCompile(`^cor_[0-9a-f]{64}$`)
	correctionDigestPattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	errCorrectionScope         = errors.New("correction session not found")
	errCorrectionInvalid       = errors.New("invalid correction request")
	errCorrectionConflict      = errors.New("correction conflicts with an existing operation or lacks matching receipts")
	errCorrectionUnavailable   = errors.New("correction persistence or replay proof is unavailable")
)

type correctionRequest struct {
	id, category, target, feedbackID string
	factual                          bool
}

type correctionClaimRecord struct {
	ID          string                  `json:"correction_operation_id"`
	Fingerprint string                  `json:"fingerprint"`
	FeedbackID  string                  `json:"feedback_id"`
	Category    string                  `json:"category"`
	Target      string                  `json:"target_claim_id,omitempty"`
	Claim       temporalClaim           `json:"claim"`
	Apply       bool                    `json:"apply,omitempty"`
	Receipt     *correctionReceiptStamp `json:"receipt_owner,omitempty"`
}

func parseCorrectionRequest(raw any) (correctionRequest, error) {
	row, _ := raw.(map[string]any)
	id, _ := row["operation_id"].(string)
	category, _ := row["category"].(string)
	target, targetOK := row["target_claim_id"].(string)
	feedbackID, _ := row["feedback_id"].(string)
	factual, factualOK := row["factual"].(bool)
	if len(row) != 5 || !correctionOperationPattern.MatchString(id) || !targetOK ||
		(target != "" && !temporalClaimIDPattern.MatchString(target)) || !factualOK || !temporalClaimIDPattern.MatchString(feedbackID) {
		return correctionRequest{}, errCorrectionInvalid
	}
	switch category {
	case "useful", "wrong", "stale", "superseded":
	default:
		return correctionRequest{}, errCorrectionInvalid
	}
	if factual && category != "useful" && target == "" {
		return correctionRequest{}, errCorrectionInvalid
	}
	return correctionRequest{id, category, target, feedbackID, factual}, nil
}

func correctionOperationID(project, session, actor, category string, factual bool, target, content string) string {
	material, _ := json.Marshal([]string{project, session, actor, category, strconv.FormatBool(factual), target, content})
	return "cor_" + sha256Hex(string(material))
}

func writeCorrectionError(w http.ResponseWriter, err error) bool {
	var status int
	var code string
	switch {
	case errors.Is(err, errCorrectionScope):
		writeCorrectionSessionNotFound(w)
		return true
	case errors.Is(err, errCorrectionInvalid):
		status, code = http.StatusUnprocessableEntity, "invalid_correction"
	case errors.Is(err, errCorrectionConflict):
		status, code = http.StatusConflict, "correction_conflict"
	case errors.Is(err, errCorrectionUnavailable):
		status, code = http.StatusServiceUnavailable, "correction_unavailable"
	default:
		return false
	}
	writeJSON(w, status, map[string]any{"ok": false, "error": code})
	return true
}

func correctionString(payload map[string]any, key string, max int, required bool) (string, bool) {
	value, ok := payload[key].(string)
	return value, ok && value == strings.TrimSpace(value) && utf8.RuneCountInString(value) <= max && (!required || value != "")
}

func (s *server) handleCorrectionClaim(w http.ResponseWriter, r *http.Request, payload map[string]any, raw any, tool bool) {
	req, err := parseCorrectionRequest(raw)
	project, projectOK := correctionString(payload, "project", 120, true)
	session, sessionOK := correctionString(payload, "session_id", maxAgentSessionIDLength, false)
	actor, actorOK := correctionString(payload, "agent_id", 120, true)
	subject, subjectOK := correctionString(payload, "subject", 300, true)
	predicate, predicateOK := correctionString(payload, "predicate", 120, true)
	object, objectOK := correctionString(payload, "object", 600, true)
	topic, topicOK := correctionString(payload, "topic_path", 240, true)
	claimID, idOK := correctionString(payload, "claim_id", 128, false)
	if err != nil || !req.factual || len(payload) != 9 ||
		!projectOK || !sessionOK || !actorOK || !subjectOK || !predicateOK || !objectOK || !topicOK || !idOK {
		writeCorrectionError(w, errCorrectionInvalid)
		return
	}
	if _, err := sanitizeMemoryProject(project); err != nil {
		writeCorrectionError(w, errCorrectionInvalid)
		return
	}
	if !s.publicCorrectionScopeAllowed(project, session, actor) {
		writeCorrectionSessionNotFound(w)
		return
	}
	note, err := s.feedbackStore.correctionContent(req, project, session, actor)
	if err != nil {
		writeCorrectionError(w, err)
		return
	}
	if utf8.RuneCountInString(note) > 1200 {
		writeCorrectionError(w, errCorrectionInvalid)
		return
	}
	claimPayload := map[string]any{
		"claim_id": claimID, "project": project, "subject": subject, "predicate": predicate, "object": object,
		"statement": note, "topic_path": topic, "agent_id": actor, "session_id": session,
		"status": "active", "verification": map[string]any{"status": "unverified", "method": "reporter_assertion"},
		"provenance": map[string]any{"source": "correction_reporter_assertion", "source_id": req.target, "run_id": req.id},
	}
	// An assertion may register explicit opposition, but cannot retire its target.
	if req.category == "wrong" {
		claimPayload["contradicts"] = []any{req.target}
	}
	claim, replayed, err := s.temporalClaims.recordCorrectionClaim(req, claimPayload)
	if err != nil {
		writeCorrectionError(w, err)
		return
	}
	response := map[string]any{
		"ok": true, "schema_id": temporalClaimContractID, "claim": temporalClaimMaps([]temporalClaim{claim})[0], "recorded": true,
		"correction_operation_id": req.id, "idempotent_replay": replayed,
	}
	if tool {
		response["tool"] = "claim_write"
	}
	writeJSON(w, http.StatusOK, attachPayloadFormatContract(temporalClaimContractID, response, actor, "temporal_claim", r.URL.Path))
}

func correctionLedgerTail(file *os.File) error {
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return err
	}
	var tail [1]byte
	if _, err := file.ReadAt(tail[:], info.Size()-1); err != nil {
		return err
	}
	if tail[0] != '\n' {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func scanTemporalClaimLedgerLine(data []byte, atEOF bool) (int, []byte, error) {
	// A record is committed only with its terminating newline. Keep legacy
	// ScanLines behavior for old claim rows that lack a final newline.
	if atEOF && bytes.IndexByte(data, '\n') < 0 && bytes.HasPrefix(bytes.TrimSpace(data), []byte(correctionClaimRecordPrefix)) {
		return 0, nil, nil
	}
	return bufio.ScanLines(data, atEOF)
}

func (s *temporalClaimStore) recordCorrectionClaim(req correctionRequest, payload map[string]any) (temporalClaim, bool, error) {
	if s == nil || !s.enabled {
		return temporalClaim{}, false, errCorrectionUnavailable
	}
	claim, err := normalizeTemporalClaim(payload)
	if err != nil || claim.ClaimID == req.target {
		return temporalClaim{}, false, errCorrectionInvalid
	}
	material, err := json.Marshal(payload)
	if err != nil {
		return temporalClaim{}, false, errCorrectionInvalid
	}
	fingerprint := sha256Hex(string(material))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.correctionUncertain || s.parseErrors != 0 {
		return temporalClaim{}, false, errCorrectionUnavailable
	}
	db, token, err := openCorrectionReceiptDB(s.path, correctionClaimReceiptKind, s.correctionStamp.Owner)
	if err != nil {
		return temporalClaim{}, false, errCorrectionUnavailable
	}
	defer db.Close()
	tip := s.correctionStamp
	tip.Owner = token
	if err := checkCorrectionReceiptFrontier(db, tip); err != nil {
		return temporalClaim{}, false, errCorrectionUnavailable
	}
	record, exists, err := readCorrectionClaimReceipt(db, req.id)
	if err != nil {
		return temporalClaim{}, false, errCorrectionUnavailable
	}
	if exists {
		if record.Fingerprint != fingerprint || record.FeedbackID != req.feedbackID || record.Category != req.category || record.Target != req.target {
			return temporalClaim{}, false, errCorrectionConflict
		}
		return record.Claim, true, nil
	}
	if _, exists := s.claims[claim.ClaimID]; exists {
		return temporalClaim{}, false, errCorrectionConflict
	}
	// A trimmed correction's claim ID remains reserved by its operation receipt.
	var reserved int
	if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM receipts WHERE alternate = ?)", claim.ClaimID).Scan(&reserved); err != nil {
		return temporalClaim{}, false, errCorrectionUnavailable
	}
	if reserved != 0 {
		return temporalClaim{}, false, errCorrectionConflict
	}
	if req.target != "" {
		target, found := s.claims[req.target]
		if !found || target.Project != claim.Project {
			return temporalClaim{}, false, errCorrectionConflict
		}
	}
	claim.CreatedAt = nowUTCISO()
	claim.UpdatedAt = claim.CreatedAt
	record = correctionClaimRecord{
		ID: req.id, Fingerprint: fingerprint, FeedbackID: req.feedbackID, Category: req.category,
		Target: req.target, Claim: claim, Apply: true,
	}
	proof, err := record.receipt()
	if err != nil {
		return temporalClaim{}, false, errCorrectionInvalid
	}
	// Open the owner before reserving. A clean open failure is retryable
	// without quarantining the current process.
	if tip.Seq == 0 {
		if err := createOwnerOnlyDurableEmptyFileIfMissing(s.path, false); err != nil {
			return temporalClaim{}, false, errCorrectionUnavailable
		}
	} else if info, err := os.Lstat(s.path); err != nil || !info.Mode().IsRegular() {
		return temporalClaim{}, false, errCorrectionUnavailable
	}
	file, err := openOwnerOnlyAppend(s.path, false)
	if err != nil {
		s.lastError = err.Error()
		return temporalClaim{}, false, errCorrectionUnavailable
	}
	defer file.Close()
	stamp, err := reserveCorrectionReceipt(db, tip, proof)
	if err != nil {
		return temporalClaim{}, false, errCorrectionUnavailable
	}
	record.Receipt = &stamp
	encoded, err := json.Marshal(record)
	if err == nil {
		err = s.appendCorrectionClaimLocked(file, append(encoded, '\n'))
	}
	if err == nil {
		err = commitCorrectionReceipt(db, stamp.Seq)
	}
	if err != nil {
		s.correctionUncertain = true
		s.lastError = err.Error()
		return temporalClaim{}, false, errCorrectionUnavailable
	}
	s.correctionStamp = stamp
	s.setClaimLocked(claim)
	s.logEntries++
	s.lastPersistedAt = nowUTCISO()
	s.trimLocked()
	if s.compactEvery > 0 && s.logEntries >= s.maxClaims*2 && s.logEntries%s.compactEvery == 0 {
		if err := s.compactLocked(); err != nil {
			s.lastError = err.Error()
		}
	}
	return claim, false, nil
}

func (s *temporalClaimStore) appendCorrectionClaimLocked(file *os.File, line []byte) error {
	reader, err := openOwnerOnlyReadPlatform(s.path)
	if err != nil {
		return err
	}
	defer reader.Close()
	readInfo, readErr := reader.Stat()
	writeInfo, writeErr := file.Stat()
	if readErr != nil || writeErr != nil || !os.SameFile(readInfo, writeInfo) {
		return errCorrectionUnavailable
	}
	tailErr := correctionLedgerTail(reader)
	if tailErr != nil {
		s.correctionUncertain = true
		return tailErr
	}
	// One synced record is both the mutation and its replay proof. Any ambiguous
	// write fences corrections until reloading the owner ledger.
	if n, err := file.Write(line); err != nil || n != len(line) {
		s.correctionUncertain = true
		if err != nil {
			return err
		}
		return io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		s.correctionUncertain = true
		return err
	}
	if err := file.Close(); err != nil {
		s.correctionUncertain = true
		return err
	}
	if err := syncOwnerOnlyDirectory(filepath.Dir(s.path)); err != nil {
		s.correctionUncertain = true
		return err
	}
	return nil
}

func (s *temporalClaimStore) correctionClaimReceipt(req correctionRequest, project, session, actor string) (temporalClaim, error) {
	if s == nil || !s.enabled {
		return temporalClaim{}, errCorrectionUnavailable
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.correctionUncertain || s.parseErrors != 0 {
		return temporalClaim{}, errCorrectionUnavailable
	}
	if s.correctionStamp.Owner == "" {
		return temporalClaim{}, errCorrectionConflict
	}
	db, _, err := openCorrectionReceiptDB(s.path, correctionClaimReceiptKind, s.correctionStamp.Owner)
	if err != nil {
		return temporalClaim{}, errCorrectionUnavailable
	}
	defer db.Close()
	if err := checkCorrectionReceiptFrontier(db, s.correctionStamp); err != nil {
		return temporalClaim{}, errCorrectionUnavailable
	}
	record, ok, err := readCorrectionClaimReceipt(db, req.id)
	if err != nil {
		return temporalClaim{}, errCorrectionUnavailable
	}
	if !ok || record.FeedbackID != req.feedbackID || record.Category != req.category || record.Target != req.target ||
		record.Claim.Project != project || record.Claim.SessionID != session || record.Claim.AgentID != actor {
		return temporalClaim{}, errCorrectionConflict
	}
	return record.Claim, nil
}

var correctionReceiptFieldNormalizer = strings.NewReplacer("_", "", "-", "")

func untypedCorrectionReceiptRequested(payload map[string]any) bool {
	id := firstNonEmptyStrings(anyToString(payload["id"]), anyToString(payload["event_id"]))
	if correctionOperationPattern.MatchString(id) {
		return true
	}
	kind := firstNonEmptyStrings(anyToString(payload["type"]), anyToString(payload["event_type"]), anyToString(payload["eventType"]))
	if normalizeAgentEventType(kind) != "correction.recorded" {
		return false
	}
	for _, fields := range []map[string]any{payload, anyMap(payload["metadata"])} {
		for key := range fields {
			switch correctionReceiptFieldNormalizer.Replace(strings.ToLower(key)) {
			case "operationid", "correctionstatus", "feedbacksaved", "feedbackid", "claimsuccess", "claimpersisted", "claimid", "claimrevision":
				return true
			}
		}
	}
	return false
}

func (s *server) recordCorrectionSessionEvent(sessionID, project string, payload map[string]any, raw any, scope map[string]any) (map[string]any, map[string]any, error) {
	req, err := parseCorrectionRequest(raw)
	actor, actorOK := correctionString(payload, "agent_id", 120, true)
	if err != nil || !actorOK || sessionID == "" || project == "" || len(payload) != 5 || payload["type"] != "correction.recorded" {
		return nil, nil, errCorrectionInvalid
	}
	if payload["session_id"] != sessionID || payload["project"] != project || !s.publicCorrectionScopeAllowed(project, sessionID, actor) {
		return nil, nil, errCorrectionScope
	}
	if _, err := s.feedbackStore.correctionContent(req, project, sessionID, actor); err != nil {
		return nil, nil, err
	}
	var claim temporalClaim
	if req.factual {
		claim, err = s.temporalClaims.correctionClaimReceipt(req, project, sessionID, actor)
		if err != nil {
			return nil, nil, err
		}
	}
	return s.agentSessions.appendCorrectionEvent(sessionID, project, actor, req, claim, scope)
}

func (s *agentSessionStore) appendCorrectionEvent(sessionID, project, actor string, req correctionRequest, claim temporalClaim, scope map[string]any) (map[string]any, map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.decisionUncertain || s.correctionUncertain {
		return nil, nil, errCorrectionUnavailable
	}
	row, found := s.sessions[sessionID]
	if !found || !publicCorrectionSessionMatches(row, project, actor) {
		return nil, nil, errCorrectionConflict
	}
	candidate := cloneJSONMap(row)
	if _, err := establishAgentSessionScopeOwnership(candidate, scope); err != nil {
		return nil, nil, errCorrectionConflict
	}
	db, token, err := openCorrectionReceiptDB(s.path, correctionSessionReceiptKind, s.correctionStamp.Owner)
	if err != nil {
		return nil, nil, errCorrectionUnavailable
	}
	defer db.Close()
	tip := s.correctionStamp
	tip.Owner = token
	if err := checkCorrectionReceiptFrontier(db, tip); err != nil {
		return nil, nil, errCorrectionUnavailable
	}
	saved, exists, err := readCorrectionReceipt(db.QueryRow(correctionReceiptLookup, req.id))
	if err != nil {
		return nil, nil, errCorrectionUnavailable
	}
	if exists {
		event := map[string]any{}
		if saved.scope != correctionSessionReceiptScope(sessionID, anyToString(row[correctionSessionGenerationField])) {
			return nil, nil, errCorrectionConflict
		}
		if saved.alternate.Valid || json.Unmarshal(saved.body, &event) != nil {
			return nil, nil, errCorrectionUnavailable
		}
		metadata := anyMap(event["metadata"])
		if event["id"] != req.id || event["type"] != "correction.recorded" || event["session_id"] != sessionID ||
			event["agent_id"] != actor || event["project"] != project ||
			metadata["operation_id"] != req.id || metadata["category"] != req.category || metadata["factual"] != req.factual ||
			metadata["target_claim_id"] != req.target || metadata["feedback_saved"] != true || metadata["feedback_id"] != req.feedbackID ||
			metadata["claim_persisted"] != req.factual ||
			(req.factual && (metadata["claim_id"] != claim.ClaimID || anyToInt(metadata["claim_revision"], 0) != claim.Revision)) {
			return nil, nil, errCorrectionUnavailable
		}
		event["idempotent_replay"] = true
		return s.effectiveSessionLocked(row, time.Now().UTC()), event, nil
	}
	metadata := map[string]any{
		"operation_id": req.id, "category": req.category, "factual": req.factual, "target_claim_id": req.target,
		"feedback_saved": true, "feedback_id": req.feedbackID, "claim_persisted": req.factual, "correction_status": "feedback_only",
	}
	if req.factual {
		metadata["claim_id"] = claim.ClaimID
		metadata["claim_revision"] = claim.Revision
		metadata["correction_status"] = "factual_persisted"
	}
	eventPayload := map[string]any{
		"id": req.id, "session_id": sessionID, "project": agentSessionProject(row), "agent_id": actor, "agent": row["agent"],
		"type": "correction.recorded", "summary": "Correction recorded: " + req.category, "metadata": metadata, "created_at": nowUTCISO(),
	}
	generation := ensureCorrectionSessionGeneration(candidate)
	proof, err := makeCorrectionReceipt(req.id, correctionSessionReceiptScope(sessionID, generation), "", normalizeAgentSessionEvent(sessionID, eventPayload))
	if err != nil {
		return nil, nil, errCorrectionUnavailable
	}
	stamp, err := reserveCorrectionReceipt(db, tip, proof)
	if err != nil {
		return nil, nil, errCorrectionUnavailable
	}
	before := s.correctionStamp
	s.correctionStamp = stamp
	var session, event map[string]any
	err = s.withAgentSessionMutationLocked(true, func() error {
		s.sessions[sessionID] = candidate
		var appendErr error
		session, event, appendErr = s.appendEventScopedLocked(sessionID, eventPayload, scope)
		return appendErr
	})
	if err != nil {
		if !s.decisionUncertain {
			s.correctionStamp = before
			if _, err := db.Exec("DELETE FROM pending_receipt WHERE seq = ?", stamp.Seq); err != nil {
				s.correctionUncertain = true
			}
		}
		return nil, nil, errCorrectionUnavailable
	}
	if err := commitCorrectionReceipt(db, stamp.Seq); err != nil {
		// The snapshot is durable. Fence typed writes until reopen confirms
		// the pending receipt; ordinary snapshots preserve the witness.
		s.correctionUncertain = true
		return nil, nil, errCorrectionUnavailable
	}
	return session, event, nil
}
