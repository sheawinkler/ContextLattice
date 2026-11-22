package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Every owner serializes mutations with its existing writer lock. The SQLite
// receipt and pending row are bounded point operations; only legacy migration
// streams historical proofs. A shared backup must include the owner file and
// its SQLite database (including any SQLite journal).
const correctionReceiptOwnerField = "correction_receipt_owner"
const correctionReceiptOwnerPrefix = `{"correction_receipt_owner":`

type correctionReceiptStamp struct {
	Owner string `json:"owner"`
	Seq   int64  `json:"sequence"`
	Hash  string `json:"digest,omitempty"`
}

func (s correctionReceiptStamp) valid() bool {
	return validCorrectionReceiptToken(s.Owner) && (s.Seq == 0 && s.Hash == "" || s.Seq > 0 && correctionDigestPattern.MatchString(s.Hash))
}

type correctionReceipt struct {
	seq       int64
	id, scope string
	alternate sql.NullString
	body      []byte
	digest    string
}

func makeCorrectionReceipt(id, scope, alternate string, value any) (correctionReceipt, error) {
	body, err := json.Marshal(value)
	r := correctionReceipt{id: id, scope: scope, alternate: sql.NullString{String: alternate, Valid: alternate != ""}, body: body}
	r.digest = r.bodyDigest()
	return r, err
}

func (r correctionReceipt) bodyDigest() string {
	return sha256Hex(r.id + "\x00" + r.scope + "\x00" + r.alternate.String + "\x00" + string(r.body))
}

func (r correctionReceipt) stamp(owner string) correctionReceiptStamp {
	return correctionReceiptStamp{owner, r.seq, r.digest}
}

func (r correctionReceipt) equal(other correctionReceipt) bool {
	return r.id == other.id && r.scope == other.scope && r.alternate == other.alternate && bytes.Equal(r.body, other.body)
}

func correctionReceiptDBPath(ownerPath string) string {
	return ownerPath + ".correction-receipts.v1.sqlite"
}

func createCorrectionReceiptDB(path, kind string) error {
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err := temp.Close(); err != nil {
		return err
	}
	db, _, err := openCorrectionFeedbackDB(temp.Name())
	if err != nil {
		return err
	}
	_, err = db.Exec(`
CREATE TABLE receipt_owner (singleton INTEGER PRIMARY KEY CHECK (singleton = 1), kind TEXT NOT NULL, token TEXT NOT NULL);
CREATE TABLE receipts (seq INTEGER PRIMARY KEY, operation TEXT NOT NULL UNIQUE, scope TEXT NOT NULL,
	alternate TEXT UNIQUE, body BLOB NOT NULL, digest TEXT NOT NULL);
CREATE TABLE pending_receipt (seq INTEGER PRIMARY KEY, operation TEXT NOT NULL, scope TEXT NOT NULL,
	alternate TEXT, body BLOB NOT NULL, digest TEXT NOT NULL, singleton INTEGER NOT NULL DEFAULT 1 UNIQUE CHECK (singleton = 1));
INSERT INTO receipt_owner VALUES (1, ?, ?);`, kind, bson.NewObjectID().Hex())
	if err := errors.Join(err, db.Close()); err != nil {
		return err
	}
	file, err := openOwnerOnlyTruncate(temp.Name(), false)
	if err != nil {
		return err
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return err
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return errCorrectionUnavailable
	}
	if err := replaceOwnerOnlyFile(temp.Name(), path); err != nil {
		return err
	}
	return syncOwnerOnlyDirectory(filepath.Dir(path))
}

type correctionReceiptQueries interface {
	QueryRow(string, ...any) *sql.Row
	Exec(string, ...any) (sql.Result, error)
}

const correctionReceiptColumns = "seq, operation, scope, alternate, body, digest"
const correctionReceiptLookup = "SELECT " + correctionReceiptColumns + " FROM receipts WHERE operation = ?"

func readCorrectionReceipt(row *sql.Row) (correctionReceipt, bool, error) {
	var r correctionReceipt
	err := row.Scan(&r.seq, &r.id, &r.scope, &r.alternate, &r.body, &r.digest)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err == nil && (r.seq < 1 || r.digest != r.bodyDigest()) {
		return r, false, errCorrectionUnavailable
	}
	return r, err == nil, err
}

func correctionReceiptFrontier(q correctionReceiptQueries, owner string) (correctionReceiptStamp, error) {
	tip := correctionReceiptStamp{Owner: owner}
	err := q.QueryRow("SELECT seq, digest FROM receipts ORDER BY seq DESC LIMIT 1").Scan(&tip.Seq, &tip.Hash)
	if errors.Is(err, sql.ErrNoRows) {
		return tip, nil
	}
	if err == nil && !tip.valid() {
		err = errCorrectionUnavailable
	}
	return tip, err
}

func openCorrectionReceiptDB(ownerPath, kind, owner string) (*sql.DB, string, error) {
	path := correctionReceiptDBPath(ownerPath)
	db, _, err := openCorrectionFeedbackDB(path)
	if errors.Is(err, os.ErrNotExist) && owner == "" {
		if err = createCorrectionReceiptDB(path, kind); err != nil {
			return nil, "", err
		}
		db, _, err = openCorrectionFeedbackDB(path)
	}
	if err != nil {
		return nil, "", err
	}
	var token string
	err = db.QueryRow("SELECT token FROM receipt_owner WHERE singleton = 1 AND kind = ?", kind).Scan(&token)
	if err != nil || !validCorrectionReceiptToken(token) || owner != "" && token != owner {
		_ = db.Close()
		return nil, "", errCorrectionUnavailable
	}
	return db, token, nil
}

func reserveCorrectionReceipt(db *sql.DB, tip correctionReceiptStamp, r correctionReceipt) (correctionReceiptStamp, error) {
	if tip.Seq == math.MaxInt64 {
		return correctionReceiptStamp{}, errCorrectionUnavailable
	}
	r.seq = tip.Seq + 1
	_, err := db.Exec("INSERT INTO pending_receipt (seq, operation, scope, alternate, body, digest) VALUES (?, ?, ?, ?, ?, ?)",
		r.seq, r.id, r.scope, r.alternate, r.body, r.digest)
	return r.stamp(tip.Owner), err
}

// Publish the immutable proof and clear the reservation in the same SQLite
// transaction, only after the ledger append or session replacement is durable.
func commitCorrectionReceipt(db *sql.DB, seq int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec("INSERT INTO receipts SELECT "+correctionReceiptColumns+" FROM pending_receipt WHERE seq = ?", seq)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return errCorrectionUnavailable
	}
	if _, err := tx.Exec("DELETE FROM pending_receipt WHERE seq = ?", seq); err != nil {
		return err
	}
	return tx.Commit()
}

func importCorrectionReceipt(tx *sql.Tx, r correctionReceipt) error {
	saved, found, err := readCorrectionReceipt(tx.QueryRow(correctionReceiptLookup, r.id))
	if err != nil {
		return err
	}
	if found {
		if !saved.equal(r) {
			return errCorrectionUnavailable
		}
		return nil
	}
	_, err = tx.Exec("INSERT INTO receipts (operation, scope, alternate, body, digest) VALUES (?, ?, ?, ?, ?)",
		r.id, r.scope, r.alternate, r.body, r.digest)
	return err
}

// A matching stamped owner image proves whether the single pending mutation
// committed. If neither image matches, leave the pending row and fail closed.
// Before cutover, the legacy owner's inline receipts must cover the entire DB.
func reconcileCorrectionReceipts(db *sql.DB, token string, disk correctionReceiptStamp, legacyCount int64, syncOwner func() error) (correctionReceiptStamp, error) {
	tip, err := correctionReceiptFrontier(db, token)
	if err != nil {
		return tip, err
	}
	pending, exists, err := readCorrectionReceipt(db.QueryRow("SELECT " + correctionReceiptColumns + " FROM pending_receipt WHERE singleton = 1"))
	if err != nil {
		return tip, err
	}
	if disk.Owner == "" && tip.Seq == legacyCount {
		disk = tip
	}
	if exists && (tip.Seq == math.MaxInt64 || pending.seq != tip.Seq+1) {
		return tip, errCorrectionUnavailable
	}
	if disk == tip {
		if exists {
			_, err = db.Exec("DELETE FROM pending_receipt WHERE seq = ?", pending.seq)
		}
		return tip, err
	}
	if !exists || pending.stamp(token) != disk {
		return tip, errCorrectionUnavailable
	}
	if err := syncOwner(); err != nil {
		return tip, err
	}
	return disk, commitCorrectionReceipt(db, pending.seq)
}

func checkCorrectionReceiptFrontier(db *sql.DB, stamp correctionReceiptStamp) error {
	tip, err := correctionReceiptFrontier(db, stamp.Owner)
	if err != nil || tip != stamp {
		return errCorrectionUnavailable
	}
	var pending int
	if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM pending_receipt WHERE singleton = 1)").Scan(&pending); err != nil || pending != 0 {
		return errCorrectionUnavailable
	}
	return nil
}

func syncCorrectionReceiptOwner(path string) error {
	file, err := openOwnerOnlyTruncate(path, false)
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close(), syncOwnerOnlyDirectory(filepath.Dir(path)))
}

func validCorrectionReceiptToken(value string) bool {
	_, err := bson.ObjectIDFromHex(value)
	return err == nil
}

func ensureCorrectionReceiptMissing(ownerPath string) bool {
	_, err := os.Lstat(correctionReceiptDBPath(ownerPath))
	return errors.Is(err, os.ErrNotExist)
}

func emptyCorrectionReceiptOwner(ownerPath, kind string) bool {
	if ensureCorrectionReceiptMissing(ownerPath) {
		return true
	}
	db, token, err := openCorrectionReceiptDB(ownerPath, kind, "")
	if err != nil {
		return false
	}
	defer db.Close()
	// An empty DB can be left by a clean rejection before the first append.
	// Anything committed or pending requires the original owner file.
	return checkCorrectionReceiptFrontier(db, correctionReceiptStamp{Owner: token}) == nil
}
