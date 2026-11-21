package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxCorrectionFeedbackRecordBytes = 2 << 20
	correctionFeedbackSchema         = "contextlattice_correction.v1"
)

func correctionFeedbackRequested(payload, normalized map[string]any) bool {
	metadata := anyMap(normalized["metadata"])
	_, hasOperation := metadata["operation_id"]
	return correctionOperationPattern.MatchString(anyToString(payload["idempotencyKey"])) ||
		correctionOperationPattern.MatchString(anyToString(metadata["operation_id"])) ||
		(hasOperation && metadata["schema_id"] == correctionFeedbackSchema)
}

func parseCorrectionFeedback(row map[string]any) (req correctionRequest, project, session, actor, note string, err error) {
	metadata := anyMap(row["metadata"])
	project, projectOK := correctionString(row, "project", 120, true)
	session, sessionOK := correctionString(metadata, "session_id", maxAgentSessionIDLength, false)
	actor, actorOK := correctionString(metadata, "agent_id", 120, true)
	note, noteOK := row["content"].(string)
	if len(metadata) != 7 || metadata["schema_id"] != correctionFeedbackSchema || row["source"] != "agent" ||
		!projectOK || !sessionOK || !actorOK || !noteOK || note == "" || note != strings.TrimSpace(note) {
		return req, "", "", "", "", errCorrectionInvalid
	}
	if _, err := sanitizeMemoryProject(project); err != nil || (session != "" && validateAgentSessionID(session) != nil) {
		return req, "", "", "", "", errCorrectionInvalid
	}
	feedbackID := row["id"]
	if feedbackID == nil { // The submit request has not been assigned a record ID yet.
		feedbackID = "pending"
	}
	req, err = parseCorrectionRequest(map[string]any{
		"operation_id": metadata["operation_id"], "category": metadata["category"], "factual": metadata["factual"],
		"target_claim_id": metadata["target_claim_id"], "feedback_id": feedbackID,
	})
	if err != nil {
		return req, "", "", "", "", err
	}
	if req.id != correctionOperationID(project, session, actor, req.category, req.factual, req.target, note) {
		return req, "", "", "", "", errCorrectionConflict
	}
	return req, project, session, actor, note, nil
}

func (s *server) submitCorrectionFeedback(w http.ResponseWriter, r *http.Request, payload, normalized map[string]any, fingerprint string) (string, map[string]any, bool, error) {
	req, project, session, actor, _, err := parseCorrectionFeedback(normalized)
	if err != nil {
		return "", nil, false, err
	}
	if payload["idempotencyKey"] != req.id {
		return "", nil, false, errCorrectionConflict
	}
	if !s.publicCorrectionScopeAllowed(project, session, actor) {
		writeCorrectionSessionNotFound(w)
		return "", nil, false, nil
	}

	record, replayed, err := s.feedbackStore.recordCorrectionFeedback(req.id, fingerprint, s.buildFeedbackRecord(normalized))
	switch {
	case err == errCorrectionUnavailable:
		s.feedbackStore.recordMetric("persistFailed")
	case err != nil:
		s.feedbackStore.recordMetric("rejected")
	case replayed:
		s.feedbackStore.recordMetric("idempotentHits")
	default:
		s.feedbackStore.recordMetric("accepted")
	}
	return req.id, record, replayed, err
}

func (s *feedbackStore) recordCorrectionFeedback(id, fingerprint string, candidate map[string]any) (map[string]any, bool, error) {
	if s == nil {
		return nil, false, errCorrectionUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := lockCorrectionFeedbackOwner(s.path)
	if err != nil {
		return nil, false, errCorrectionUnavailable
	}
	defer unlock()
	existing, info, err := s.findCorrectionFeedbackLocked(id)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		// Compare the same normalized fields that were persisted, excluding the
		// generated record identity and timestamps.
		material := make(map[string]any, len(existing))
		for key, value := range existing {
			switch key {
			case "id", "created_at", "updated_at":
			default:
				material[key] = value
			}
		}
		if feedbackSubmitFingerprint(material) != fingerprint {
			return nil, false, errCorrectionConflict
		}
		return existing, true, nil
	}
	line, err := json.Marshal(candidate)
	if err != nil || len(line)+1 >= maxCorrectionFeedbackRecordBytes {
		return nil, false, errCorrectionUnavailable
	}
	line = append(line, '\n')
	if info.Size()-s.correctionIndex.durableEnd+int64(len(line)) > maxCorrectionFeedbackCatchupBytes {
		if err := s.syncAndCheckpointCorrectionFeedbackLocked(nil); err != nil {
			s.correctionIndex.uncertain = true
			return nil, false, errCorrectionUnavailable
		}
	}
	if err := s.appendCorrectionFeedbackLocked(info, line); err != nil {
		return nil, false, err
	}
	s.retainFeedbackLocked(candidate)
	index := &s.correctionIndex
	if index.afterLedgerSync != nil {
		if err := index.afterLedgerSync(); err != nil {
			index.uncertain = true
			return nil, false, errCorrectionUnavailable
		}
	}
	entry := correctionFeedbackEntry{
		ID: id, correctionFeedbackPosition: correctionFeedbackPosition{
			Offset: info.Size(), Length: len(line), Digest: sha256Hex(string(line)),
		},
	}
	if err := s.persistCorrectionFeedbackIndexLocked([]correctionFeedbackEntry{entry}); err != nil {
		index.uncertain = true
		return nil, false, errCorrectionUnavailable
	}
	return candidate, false, nil
}

func (s *feedbackStore) appendCorrectionFeedbackLocked(expected os.FileInfo, line []byte) error {
	file, err := openOwnerOnlyAppend(s.path, false)
	if err != nil {
		return errCorrectionUnavailable
	}
	info, err := file.Stat()
	if err != nil || !os.SameFile(expected, info) || expected.Size() != info.Size() {
		_ = file.Close()
		return errCorrectionUnavailable
	}

	// The feedback row itself is the replay receipt. Never acknowledge it
	// before the owner syncs the complete newline-terminated record.
	n, writeErr := file.Write(line)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || n != len(line) || syncErr != nil || closeErr != nil {
		s.correctionIndex.uncertain = true
		return errCorrectionUnavailable
	}
	current, err := os.Lstat(s.path)
	if err != nil || !os.SameFile(expected, current) || current.Size() != expected.Size()+int64(len(line)) ||
		syncOwnerOnlyDirectory(filepath.Dir(s.path)) != nil {
		s.correctionIndex.uncertain = true
		return errCorrectionUnavailable
	}
	s.advanceCorrectionFeedbackLocked(line)
	return nil
}

func (s *feedbackStore) correctionContent(req correctionRequest, project, session, actor string) (string, error) {
	if s == nil {
		return "", errCorrectionUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := lockCorrectionFeedbackOwner(s.path)
	if err != nil {
		return "", errCorrectionUnavailable
	}
	defer unlock()
	record, _, err := s.findCorrectionFeedbackLocked(req.id)
	if err != nil {
		return "", err
	}
	stored, storedProject, storedSession, storedActor, note, err := parseCorrectionFeedback(record)
	if err != nil || stored != req || storedProject != project || storedSession != session || storedActor != actor {
		return "", errCorrectionConflict
	}
	return note, nil
}
