package main

import (
	"database/sql"
	"encoding/json"
	"strings"
)

const correctionClaimReceiptKind = "claims"

func decodeCorrectionClaimRecord(raw []byte) (correctionClaimRecord, error) {
	var r correctionClaimRecord
	if err := json.Unmarshal(raw, &r); err != nil || !correctionOperationPattern.MatchString(r.ID) ||
		!correctionDigestPattern.MatchString(r.Fingerprint) || !temporalClaimIDPattern.MatchString(r.Claim.ClaimID) || !temporalClaimIDPattern.MatchString(r.FeedbackID) ||
		r.Claim.Provenance["run_id"] != r.ID || r.Claim.Verification["status"] != "unverified" || r.Claim.Verification["method"] != "reporter_assertion" {
		return r, errCorrectionUnavailable
	}
	r.Claim.searchText = temporalClaimSearchText(r.Claim)
	return r, nil
}

func (r correctionClaimRecord) receipt() (correctionReceipt, error) {
	r.Apply, r.Receipt = false, nil
	return makeCorrectionReceipt(r.ID, r.Claim.Project, r.Claim.ClaimID, r)
}

func readCorrectionClaimReceipt(db *sql.DB, id string) (correctionClaimRecord, bool, error) {
	saved, found, err := readCorrectionReceipt(db.QueryRow(correctionReceiptLookup, id))
	if err != nil || !found {
		return correctionClaimRecord{}, found, err
	}
	r, err := decodeCorrectionClaimRecord(saved.body)
	if err != nil || r.ID != id || r.Claim.Project != saved.scope || r.Claim.ClaimID != saved.alternate.String || r.Apply || r.Receipt != nil {
		return r, false, errCorrectionUnavailable
	}
	return r, true, nil
}

// Fold old inline proofs into SQLite during the existing claim materialization
// pass. New generations carry a constant-size frontier; only the pending row
// can need recovery. No receipt history is held in memory or copied at compaction.
type correctionClaimReplay struct {
	stamp  correctionReceiptStamp
	legacy int64
	db     *sql.DB
	tx     *sql.Tx
}

func (r *correctionClaimReplay) close() {
	if r.tx != nil {
		_ = r.tx.Rollback()
	}
	if r.db != nil {
		_ = r.db.Close()
	}
}

func (r *correctionClaimReplay) consume(s *temporalClaimStore, line string) bool {
	if strings.HasPrefix(line, correctionReceiptOwnerPrefix) {
		var envelope struct {
			Stamp correctionReceiptStamp `json:"correction_receipt_owner"`
		}
		if s.logEntries != 0 || r.stamp.Owner != "" || json.Unmarshal([]byte(line), &envelope) != nil || !envelope.Stamp.valid() {
			s.correctionUncertain = true
		} else {
			r.stamp = envelope.Stamp
		}
		return true
	}
	if !strings.HasPrefix(line, correctionClaimRecordPrefix) {
		return false
	}
	record, err := decodeCorrectionClaimRecord([]byte(line))
	if err != nil {
		s.correctionUncertain = true
		return true
	}
	err = r.index(s.path, record)
	if err != nil {
		s.correctionUncertain = true
	}
	if record.Apply && (record.Receipt == nil || err == nil) {
		s.setClaimLocked(record.Claim)
	}
	return true
}

func (r *correctionClaimReplay) index(path string, record correctionClaimRecord) error {
	proof, err := record.receipt()
	if err != nil {
		return err
	}
	if next := record.Receipt; next != nil {
		proof.seq = next.Seq
		if !record.Apply || !next.valid() || next.Seq < 1 || proof.stamp(next.Owner) != *next ||
			(r.stamp.Owner != "" && (r.stamp.Owner != next.Owner || next.Seq-1 != r.stamp.Seq)) {
			return errCorrectionUnavailable
		}
		r.stamp = *next
		return nil
	}
	if r.stamp.Owner != "" {
		return errCorrectionUnavailable
	}
	if r.tx == nil {
		db, _, err := openCorrectionReceiptDB(path, correctionClaimReceiptKind, "")
		if err != nil {
			return err
		}
		r.db = db
		r.tx, err = db.Begin()
		if err != nil {
			return err
		}
	}
	if err := importCorrectionReceipt(r.tx, proof); err != nil {
		return err
	}
	r.legacy++
	return nil
}

func (r *correctionClaimReplay) finish(s *temporalClaimStore) error {
	if r.tx != nil {
		if err := r.tx.Commit(); err != nil {
			return err
		}
	}
	if r.stamp.Owner == "" && r.db == nil && ensureCorrectionReceiptMissing(s.path) {
		return nil
	}
	db, token, err := openCorrectionReceiptDB(s.path, correctionClaimReceiptKind, r.stamp.Owner)
	if err != nil {
		return err
	}
	defer db.Close()
	s.correctionStamp, err = reconcileCorrectionReceipts(db, token, r.stamp, r.legacy, func() error {
		return syncCorrectionReceiptOwner(s.path)
	})
	return err
}
