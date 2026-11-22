package main

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
)

const (
	correctionFeedbackSQLiteVersion = 2
	correctionFeedbackOffsetQuery   = `SELECT row_offset, row_length, digest FROM correction_offsets WHERE operation_id = ?`
)

type correctionFeedbackDurableFrontier struct {
	correctionFeedbackCheckpoint
	Witness string
}

// Connections are scoped to owner operations; the ledger writer lock spans
// both stores. mode=rw prevents an unavailable index becoming an empty index.
func openCorrectionFeedbackDB(path string) (*sql.DB, os.FileInfo, error) {
	file, err := openOwnerOnlyReadPlatform(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if side, err := os.Lstat(path + suffix); err == nil {
			if !side.Mode().IsRegular() {
				return nil, nil, errCorrectionUnavailable
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, nil, err
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, err
	}
	db, err := sql.Open("sqlite", agentTaskSQLiteDSN(absolute)+"&mode=rw&_pragma=busy_timeout%281000%29&_pragma=synchronous%28FULL%29")
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	if err := ownerOnlyLockPathIdentityMatches(path, file); err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return db, info, nil
}

func readCorrectionFeedbackFrontier(db *sql.DB) (correctionFeedbackDurableFrontier, error) {
	var frontier correctionFeedbackDurableFrontier
	err := db.QueryRow(`SELECT epoch_start, frontier, witness FROM correction_epoch WHERE singleton = 1`).
		Scan(&frontier.Epoch, &frontier.Frontier, &frontier.Witness)
	return frontier, err
}

func correctionFeedbackPositionValid(id string, row correctionFeedbackPosition, epoch, frontier int64) bool {
	return correctionOperationPattern.MatchString(id) && correctionDigestPattern.MatchString(row.Digest) &&
		row.Offset >= epoch && row.Length > 0 && row.Length < maxCorrectionFeedbackRecordBytes &&
		row.Offset <= frontier-int64(row.Length)
}

func (s *feedbackStore) verifyCorrectionFeedbackFrontier(history *os.File, frontier correctionFeedbackDurableFrontier) ([]byte, error) {
	if frontier.Epoch < 0 || frontier.Frontier < frontier.Epoch || frontier.Frontier > s.correctionSize {
		return nil, errCorrectionUnavailable
	}
	witness, err := s.readCorrectionFeedbackWitness(history, frontier.Frontier)
	if err != nil || frontier.Witness != sha256Hex(string(witness)) {
		return nil, errCorrectionUnavailable
	}
	return witness, nil
}

func (s *feedbackStore) openCorrectionFeedbackIndexLocked(history *os.File) (bool, error) {
	path := correctionFeedbackIndexPath(s.path)
	db, info, err := openCorrectionFeedbackDB(path)
	if errors.Is(err, os.ErrNotExist) {
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
				return true, errCorrectionUnavailable
			}
		}
		return false, nil
	}
	if err != nil {
		return true, err
	}
	defer db.Close()
	var version int
	var journal string
	if db.QueryRow(`PRAGMA user_version`).Scan(&version) != nil || version != correctionFeedbackSQLiteVersion ||
		db.QueryRow(`PRAGMA journal_mode`).Scan(&journal) != nil || journal != "wal" {
		return true, errCorrectionUnavailable
	}
	frontier, err := readCorrectionFeedbackFrontier(db)
	if err != nil {
		return true, err
	}
	witness, err := s.verifyCorrectionFeedbackFrontier(history, frontier)
	if err != nil {
		return true, err
	}
	s.setCorrectionFeedbackIndexFrontier(frontier, witness, info)
	return true, nil
}

func (s *feedbackStore) setCorrectionFeedbackIndexFrontier(frontier correctionFeedbackDurableFrontier, witness []byte, info os.FileInfo) {
	index := &s.correctionIndex
	index.checkpoint, index.witness, index.indexFile = frontier.correctionFeedbackCheckpoint, witness, info
	index.durableEnd, index.durableWitness, index.initialized = frontier.Frontier, frontier.Witness, true
}

// Publish a newly initialized DB only after its transaction, WAL
// checkpoint and file sync. On interruption the ledger is unchanged.
func installCorrectionFeedbackDB(path string, frontier correctionFeedbackDurableFrontier, entries []correctionFeedbackEntry) (os.FileInfo, error) {
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return nil, err
	}
	tempPath := temp.Name()
	defer func() {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			_ = os.Remove(tempPath + suffix)
		}
	}()
	if err := temp.Close(); err != nil {
		return nil, err
	}
	db, _, err := openCorrectionFeedbackDB(tempPath)
	if err != nil {
		return nil, err
	}
	buildErr := buildCorrectionFeedbackDB(db, frontier, entries)
	closeErr := db.Close()
	if buildErr != nil || closeErr != nil {
		return nil, errors.Join(buildErr, closeErr)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(tempPath + suffix); !errors.Is(err, os.ErrNotExist) {
			return nil, errCorrectionUnavailable
		}
	}
	synced, err := openOwnerOnlyTruncate(tempPath, false)
	if err != nil {
		return nil, err
	}
	if err := errors.Join(synced.Sync(), synced.Close()); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return nil, errCorrectionUnavailable
	}
	if err := replaceOwnerOnlyFile(tempPath, path); err != nil {
		return nil, err
	}
	if err := syncOwnerOnlyDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	return os.Lstat(path)
}

func buildCorrectionFeedbackDB(db *sql.DB, frontier correctionFeedbackDurableFrontier, entries []correctionFeedbackEntry) error {
	var journal string
	if err := db.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&journal); err != nil || journal != "wal" {
		return errCorrectionUnavailable
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, ddl := range []string{
		`CREATE TABLE correction_epoch (
			singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
			epoch_start INTEGER NOT NULL CHECK(epoch_start >= 0),
			frontier INTEGER NOT NULL CHECK(frontier >= epoch_start),
			witness TEXT NOT NULL CHECK(length(witness) = 64)
		)`,
		`CREATE TABLE correction_offsets (
			operation_id TEXT PRIMARY KEY,
			row_offset INTEGER NOT NULL UNIQUE CHECK(row_offset >= 0),
			row_length INTEGER NOT NULL CHECK(row_length > 0),
			digest TEXT NOT NULL CHECK(length(digest) = 64)
		) WITHOUT ROWID`,
		`PRAGMA user_version = 2`,
	} {
		if _, err := tx.Exec(ddl); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO correction_epoch VALUES (1, ?, ?, ?)`, frontier.Epoch, frontier.Frontier, frontier.Witness); err != nil {
		return err
	}
	if _, err := insertCorrectionFeedbackOffsets(tx, entries); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	var busy, logPages, checkpointed int
	if err := db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logPages, &checkpointed); err != nil || busy != 0 {
		return errCorrectionUnavailable
	}
	return nil
}

func insertCorrectionFeedbackOffsets(tx *sql.Tx, entries []correctionFeedbackEntry) (int64, error) {
	if len(entries) == 0 {
		return 0, nil
	}
	stmt, err := tx.Prepare(`INSERT INTO correction_offsets(operation_id, row_offset, row_length, digest) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	var inserted int64
	for _, entry := range entries {
		result, err := stmt.Exec(entry.ID, entry.Offset, entry.Length, entry.Digest)
		if err != nil {
			return 0, err
		}
		n, err := result.RowsAffected()
		if err != nil || n != 1 {
			return 0, errCorrectionUnavailable
		}
		inserted += n
	}
	return inserted, nil
}

func (s *feedbackStore) persistCorrectionFeedbackIndexLocked(entries []correctionFeedbackEntry) error {
	index := &s.correctionIndex
	if !s.correctionFeedbackIndexFileMatchesLocked() {
		return errCorrectionUnavailable
	}
	witness := sha256Hex(string(index.witness))
	path := correctionFeedbackIndexPath(s.path)
	if index.indexFile == nil {
		if len(entries) != 0 {
			return errCorrectionUnavailable
		}
		frontier := correctionFeedbackDurableFrontier{correctionFeedbackCheckpoint: index.checkpoint, Witness: witness}
		info, err := installCorrectionFeedbackDB(path, frontier, nil)
		if err != nil {
			return err
		}
		s.setCorrectionFeedbackIndexFrontier(frontier, index.witness, info)
		return nil
	}
	db, info, err := openCorrectionFeedbackDB(path)
	if err != nil {
		return err
	}
	defer db.Close()
	if !os.SameFile(index.indexFile, info) {
		return errCorrectionUnavailable
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE correction_epoch SET frontier = ?, witness = ?
		WHERE singleton = 1 AND epoch_start = ? AND frontier = ? AND witness = ?`,
		index.checkpoint.Frontier, witness, index.checkpoint.Epoch, index.durableEnd, index.durableWitness)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil || updated != 1 {
		return errCorrectionUnavailable
	}
	inserted, err := insertCorrectionFeedbackOffsets(tx, entries)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	index.durableEnd, index.durableWitness = index.checkpoint.Frontier, witness
	if observe := index.observeIndex; observe != nil {
		observe("frontier_write", updated)
		observe("offset_write", inserted)
	}
	return nil
}

func (s *feedbackStore) lookupCorrectionFeedbackIndexLocked(id string) (correctionFeedbackPosition, bool, error) {
	index := &s.correctionIndex
	db, info, err := openCorrectionFeedbackDB(correctionFeedbackIndexPath(s.path))
	if err != nil {
		return correctionFeedbackPosition{}, false, err
	}
	defer db.Close()
	if index.indexFile == nil || !os.SameFile(index.indexFile, info) {
		return correctionFeedbackPosition{}, false, errCorrectionUnavailable
	}
	frontier, err := readCorrectionFeedbackFrontier(db)
	if err != nil || frontier.Epoch != index.checkpoint.Epoch ||
		frontier.Frontier != index.durableEnd || frontier.Witness != index.durableWitness {
		return correctionFeedbackPosition{}, false, errCorrectionUnavailable
	}
	if observe := index.observeIndex; observe != nil {
		observe("frontier_read", 1)
	}
	var position correctionFeedbackPosition
	err = db.QueryRow(correctionFeedbackOffsetQuery, id).Scan(&position.Offset, &position.Length, &position.Digest)
	if errors.Is(err, sql.ErrNoRows) {
		return correctionFeedbackPosition{}, false, nil
	}
	if err != nil || !correctionFeedbackPositionValid(id, position, frontier.Epoch, frontier.Frontier) {
		return correctionFeedbackPosition{}, false, errCorrectionUnavailable
	}
	if observe := index.observeIndex; observe != nil {
		observe("offset_read", 1)
	}
	return position, true, nil
}
