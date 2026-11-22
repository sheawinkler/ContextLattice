package main

import (
	"encoding/json"

	"go.mongodb.org/mongo-driver/v2/bson"
)

const correctionSessionReceiptKind = "sessions"
const correctionSessionGenerationField = "correction_receipt_generation"

func correctionSessionReceiptScope(session, generation string) string {
	return session + "/" + generation
}

func ensureCorrectionSessionGeneration(row map[string]any) string {
	generation := anyToString(row[correctionSessionGenerationField])
	if generation == "" {
		generation = bson.NewObjectID().Hex()
		row[correctionSessionGenerationField] = generation
	}
	return generation
}

func validStoredCorrectionEvent(event map[string]any, sessionID, operation string) bool {
	metadata := anyMap(event["metadata"])
	return correctionOperationPattern.MatchString(operation) && event["id"] == operation &&
		event["type"] == "correction.recorded" && event["session_id"] == sessionID &&
		metadata["operation_id"] == operation && metadata["feedback_saved"] == true
}

func (s *agentSessionStore) loadCorrectionSessionReceipts(raw []byte) error {
	var envelope struct {
		Stamp json.RawMessage `json:"correction_receipt_owner"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return err
	}
	var disk correctionReceiptStamp
	if len(envelope.Stamp) != 0 {
		if json.Unmarshal(envelope.Stamp, &disk) != nil || !disk.valid() {
			return errCorrectionUnavailable
		}
		s.correctionStamp = disk // Preserve the fence even when the sidecar is unavailable.
	}
	hasLegacy := false
	for _, row := range s.sessions {
		if len(anyMap(row["correction_receipts"])) != 0 {
			hasLegacy = true
			break
		}
	}
	if hasLegacy && disk.Owner != "" {
		return errCorrectionUnavailable
	}
	if disk.Owner == "" && !hasLegacy && ensureCorrectionReceiptMissing(s.path) {
		return nil
	}
	db, token, err := openCorrectionReceiptDB(s.path, correctionSessionReceiptKind, disk.Owner)
	if err != nil {
		return err
	}
	defer db.Close()
	var legacyCount int64
	if hasLegacy {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for sessionID, row := range s.sessions {
			// Stable across a crash after import but before the first stamped
			// snapshot. Newly created sessions never use this generation.
			generation := sha256Hex(token + "/" + sessionID)[:24]
			for id, value := range anyMap(row["correction_receipts"]) {
				event := anyMap(value)
				if !validStoredCorrectionEvent(event, sessionID, id) {
					return errCorrectionUnavailable
				}
				proof, err := makeCorrectionReceipt(id, correctionSessionReceiptScope(sessionID, generation), "", event)
				if err != nil {
					return err
				}
				if err := importCorrectionReceipt(tx, proof); err != nil {
					return err
				}
				legacyCount++
				row[correctionSessionGenerationField] = generation
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	confirmed, err := reconcileCorrectionReceipts(db, token, disk, legacyCount, func() error {
		// The snapshot stamp and event/count were installed in one replacement.
		// Unlike the visible event ring, the stamp survives retention.
		return syncCorrectionReceiptOwner(s.path)
	})
	if err != nil {
		return err
	}
	s.correctionStamp = confirmed
	for _, row := range s.sessions {
		delete(row, "correction_receipts")
	}
	return nil
}
