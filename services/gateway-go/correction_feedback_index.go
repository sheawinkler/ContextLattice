package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"time"
)

// The feedback ledger remains the authority. The SQLite sidecar covers a
// verified prefix with a singleton frontier and primary-key offsets. Typed
// rows are synced before publishing their offsets. Ordinary writes advance
// the in-memory frontier and periodically sync a checkpoint.
// After reopen, one request verifies at most one suffix page and refuses
// receipt or absence claims until the frontier reaches EOF.
//
// Activation uses the legacy untyped baseline; it never rewrites history
// or adopts an unindexed typed epoch. Back up checkpointed SQLite with its ledger.
// Pause typed writes before downgrade. On missing proof, restore a matched
// owner pair or reconcile offline; never discard bytes to unblock recovery.
const (
	maxCorrectionFeedbackCatchupBytes = 4 << 20
	correctionFeedbackWitnessBytes    = 512
	maxIndexedFeedbackPreferenceBytes = 32 << 20
)

type correctionFeedbackPosition struct {
	Offset int64
	Length int
	Digest string
}

type correctionFeedbackCheckpoint struct {
	Epoch, Frontier int64
}

type correctionFeedbackEntry struct {
	ID string
	correctionFeedbackPosition
}

type correctionFeedbackIndexState struct {
	checkpoint     correctionFeedbackCheckpoint
	durableEnd     int64
	durableWitness string
	indexFile      os.FileInfo
	witness        []byte
	initialized    bool
	uncertain      bool

	observeRead     func(string, int)
	observeIndex    func(string, int64)
	afterLedgerSync func() error
}

func correctionFeedbackIndexPath(historyPath string) string {
	return historyPath + ".correction-index.v2.sqlite3"
}

func lockCorrectionFeedbackOwner(path string) (func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return lockOwnerOnlyFileContext(ctx, path+".writer.lock")
}

func (s *feedbackStore) readCorrectionFeedbackAt(file *os.File, offset int64, size int, phase string) ([]byte, error) {
	raw := make([]byte, size)
	n, err := file.ReadAt(raw, offset)
	if observe := s.correctionIndex.observeRead; observe != nil {
		observe(phase, n)
	}
	return raw, err
}

func (s *feedbackStore) readCorrectionFeedbackWitness(file *os.File, end int64) ([]byte, error) {
	size := minInt64(end, correctionFeedbackWitnessBytes)
	if size == 0 {
		return nil, nil
	}
	raw, err := s.readCorrectionFeedbackAt(file, end-size, int(size), "witness")
	if err != nil || raw[len(raw)-1] != '\n' {
		return nil, errCorrectionUnavailable
	}
	return raw, nil
}

// A missing snapshot permits epoch activation only after the startup legacy
// scan has proved there are no typed rows. Never infer a new epoch on demand.
func (s *feedbackStore) loadCorrectionFeedbackIndexLocked(history *os.File) bool {
	found, err := s.openCorrectionFeedbackIndexLocked(history)
	if err != nil {
		s.correctionIndex.uncertain = true
		return true
	}
	return found
}

func (s *feedbackStore) prepareCorrectionFeedbackEpochLocked(history *os.File, end int64) {
	witness, err := s.readCorrectionFeedbackWitness(history, end)
	if err != nil || !correctionFeedbackFileAt(s.path, s.correctionFile, end) {
		s.correctionIndex.uncertain = true
		return
	}
	s.correctionIndex.checkpoint = correctionFeedbackCheckpoint{Epoch: end, Frontier: end}
	s.correctionIndex.witness = witness
	s.correctionIndex.initialized = true
}

func correctionFeedbackFileAt(path string, expected os.FileInfo, size int64) bool {
	current, err := os.Lstat(path)
	return err == nil && expected != nil && os.SameFile(current, expected) && current.Size() == size
}

func (s *feedbackStore) correctionFeedbackIndexFileMatchesLocked() bool {
	path := correctionFeedbackIndexPath(s.path)
	expected := s.correctionIndex.indexFile
	current, err := os.Lstat(path)
	if expected == nil {
		return errors.Is(err, os.ErrNotExist)
	}
	return err == nil && os.SameFile(expected, current)
}

func (s *feedbackStore) syncAndCheckpointCorrectionFeedbackLocked(entries []correctionFeedbackEntry) error {
	if !correctionFeedbackFileAt(s.path, s.correctionFile, s.correctionSize) {
		return errCorrectionUnavailable
	}
	file, err := openOwnerOnlyTruncate(s.path, false)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !os.SameFile(s.correctionFile, info) || info.Size() != s.correctionSize ||
		file.Sync() != nil || ownerOnlyLockPathIdentityMatches(s.path, file) != nil {
		return errCorrectionUnavailable
	}
	return s.persistCorrectionFeedbackIndexLocked(entries)
}

func correctionFeedbackLedgerTyped(row map[string]any) bool {
	metadata := anyMap(row["metadata"])
	_, hasOperation := metadata["operation_id"]
	return correctionOperationPattern.MatchString(anyToString(metadata["operation_id"])) ||
		(hasOperation && metadata["schema_id"] == correctionFeedbackSchema)
}

func (s *feedbackStore) beforeLegacyFeedbackAppendLocked(file *os.File, record map[string]any, length int) {
	index := &s.correctionIndex
	if index.uncertain || !index.initialized {
		return
	}
	info, err := file.Stat()
	if err != nil || !os.SameFile(s.correctionFile, info) || info.Size() != s.correctionSize || correctionFeedbackLedgerTyped(record) {
		index.uncertain = true
		return
	}
	if index.indexFile != nil && index.checkpoint.Frontier == s.correctionSize &&
		s.correctionSize-index.durableEnd+int64(length) > maxCorrectionFeedbackCatchupBytes {
		// Keep a crash suffix bounded during ordinary in-process writes. Do
		// not turn an index failure into refusal of unrelated legacy feedback.
		if file.Sync() != nil || s.persistCorrectionFeedbackIndexLocked(nil) != nil {
			index.uncertain = true
		}
	}
}

func (s *feedbackStore) checkLegacyFeedbackRecoveryTailLocked() error {
	index := &s.correctionIndex
	if !index.uncertain && (index.indexFile == nil || index.checkpoint.Frontier == s.correctionSize) {
		return nil
	}
	file, err := openOwnerOnlyReadPlatform(s.path)
	if err != nil {
		return errCorrectionUnavailable
	}
	defer file.Close()
	if ownerOnlyLockPathIdentityMatches(s.path, file) != nil || correctionLedgerTail(file) != nil {
		// Do not concatenate an unrelated row onto an uncertain partial write.
		return errCorrectionUnavailable
	}
	return nil
}

func (s *feedbackStore) advanceCorrectionFeedbackLocked(line []byte) {
	index := &s.correctionIndex
	if !index.uncertain && index.initialized && index.checkpoint.Frontier == s.correctionSize {
		s.advanceCorrectionFeedbackFrontierLocked(line)
	}
	s.correctionSize += int64(len(line))
}

func (s *feedbackStore) advanceCorrectionFeedbackFrontierLocked(raw []byte) {
	index := &s.correctionIndex
	if len(raw) >= correctionFeedbackWitnessBytes {
		index.witness = bytes.Clone(raw[len(raw)-correctionFeedbackWitnessBytes:])
	} else {
		combined := append(index.witness, raw...)
		index.witness = bytes.Clone(combined[max(0, len(combined)-correctionFeedbackWitnessBytes):])
	}
	index.checkpoint.Frontier += int64(len(raw))
}

func (s *feedbackStore) catchUpCorrectionFeedbackLocked(file *os.File) error {
	index := &s.correctionIndex
	start := index.checkpoint.Frontier
	size := minInt64(s.correctionSize-start, maxCorrectionFeedbackCatchupBytes)
	page, err := s.readCorrectionFeedbackAt(file, start, int(size), "catchup")
	if err != nil {
		return errCorrectionUnavailable
	}
	complete := bytes.LastIndexByte(page, '\n') + 1
	if complete == 0 {
		index.uncertain = true
		return errCorrectionUnavailable
	}
	var pending []correctionFeedbackEntry
	for offset := 0; offset < complete; {
		length := bytes.IndexByte(page[offset:complete], '\n') + 1
		line := page[offset : offset+length]
		if len(bytes.TrimSpace(line)) != 0 {
			row, err := parseJSONMap(line)
			if err != nil || bytes.TrimSpace(line)[0] != '{' {
				index.uncertain = true
				return errCorrectionUnavailable
			}
			if correctionFeedbackLedgerTyped(row) {
				id := anyToString(anyMap(row["metadata"])["operation_id"])
				req, _, _, _, _, err := parseCorrectionFeedback(row)
				if _, hasID := row["id"].(string); err != nil || !hasID || req.id != id || length >= maxCorrectionFeedbackRecordBytes {
					index.uncertain = true
					return errCorrectionUnavailable
				}
				pending = append(pending, correctionFeedbackEntry{
					ID: id, correctionFeedbackPosition: correctionFeedbackPosition{
						Offset: start + int64(offset), Length: length, Digest: sha256Hex(string(line)),
					},
				})
			}
		}
		offset += length
	}
	s.advanceCorrectionFeedbackFrontierLocked(page[:complete])
	if err := s.syncAndCheckpointCorrectionFeedbackLocked(pending); err != nil {
		index.uncertain = true
		return errCorrectionUnavailable
	}
	if index.checkpoint.Frontier != s.correctionSize {
		// More verified pages (or a torn tail) remain. No miss is authoritative.
		return errCorrectionUnavailable
	}
	return nil
}

func (s *feedbackStore) findCorrectionFeedbackLocked(id string) (map[string]any, os.FileInfo, error) {
	index := &s.correctionIndex
	if index.uncertain || !index.initialized || !s.correctionFeedbackIndexFileMatchesLocked() {
		return nil, nil, errCorrectionUnavailable
	}
	file, err := openOwnerOnlyReadPlatform(s.path)
	if err != nil {
		return nil, nil, errCorrectionUnavailable
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !os.SameFile(s.correctionFile, info) || info.Size() != s.correctionSize ||
		ownerOnlyLockPathIdentityMatches(s.path, file) != nil {
		index.uncertain = true
		return nil, nil, errCorrectionUnavailable
	}
	witness, err := s.readCorrectionFeedbackWitness(file, index.checkpoint.Frontier)
	if err != nil || !bytes.Equal(index.witness, witness) {
		index.uncertain = true
		return nil, nil, errCorrectionUnavailable
	}
	if index.indexFile == nil {
		if err := s.syncAndCheckpointCorrectionFeedbackLocked(nil); err != nil {
			index.uncertain = true
			return nil, nil, errCorrectionUnavailable
		}
	}
	if index.checkpoint.Frontier < info.Size() {
		if err := s.catchUpCorrectionFeedbackLocked(file); err != nil {
			return nil, nil, err
		}
	}
	position, found, err := s.lookupCorrectionFeedbackIndexLocked(id)
	if err != nil {
		index.uncertain = true
		return nil, nil, errCorrectionUnavailable
	}
	if !found {
		return nil, info, nil
	}
	line, err := s.readCorrectionFeedbackAt(file, position.Offset, position.Length, "indexed")
	if err != nil || sha256Hex(string(line)) != position.Digest {
		return nil, nil, errCorrectionUnavailable
	}
	row, err := parseJSONMap(line)
	if err != nil || !correctionFeedbackLedgerTyped(row) || anyMap(row["metadata"])["operation_id"] != id || ownerOnlyLockPathIdentityMatches(s.path, file) != nil {
		return nil, nil, errCorrectionUnavailable
	}
	return row, info, nil
}
