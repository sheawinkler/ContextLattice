package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	ownerOnlyDirectoryMode           = os.FileMode(0o700)
	ownerOnlyFileMode                = os.FileMode(0o600)
	ownerOnlyLegacySchemaID          = "contextlattice_owner_only_store.v1"
	ownerOnlySchemaID                = "contextlattice_owner_only_store.v2"
	ownerOnlyMigrationReportSchemaID = "contextlattice_owner_only_migration_report.v1"
	ownerOnlyStateFile               = "owner_only_v1.json"
	ownerOnlyLockFile                = "owner_only_v2.lock"
	ownerOnlyWriterPolicyVersion     = "owner_only_writer.v2"
	ownerOnlyMigrationBatchMax       = 1000
	ownerOnlyMigrationBatchDefault   = 256
	ownerOnlyMigrationReadChunk      = 256
	ownerOnlyMigrationMaxDepth       = 256
)

var errOwnerOnlyMigrationYield = errors.New("owner-only migration yielded to startup")
var errOwnerOnlyMigrationLocked = errors.New("owner-only migration worker already active")

type ownerOnlyAtomicWriteError struct {
	Operation string
	Committed bool
	Err       error
}

func (e *ownerOnlyAtomicWriteError) Error() string {
	if e == nil {
		return "owner-only atomic write failed"
	}
	return fmt.Sprintf("%s: %v", e.Operation, e.Err)
}

func (e *ownerOnlyAtomicWriteError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func ownerOnlyAtomicWriteCommitted(err error) bool {
	var writeErr *ownerOnlyAtomicWriteError
	return errors.As(err, &writeErr) && writeErr.Committed
}

type ownerOnlyMigrationState struct {
	SchemaID         string `json:"schema_id"`
	Complete         bool   `json:"complete"`
	Cursor           string `json:"cursor,omitempty"`
	ProcessedEntries int64  `json:"processed_entries,omitempty"`
	EnforcedEntries  int64  `json:"enforced_entries,omitempty"`
	BatchCount       int64  `json:"batch_count,omitempty"`
	RootIdentity     string `json:"root_identity,omitempty"`
	WriterPolicy     string `json:"writer_policy,omitempty"`
	Phase            string `json:"phase,omitempty"`
	UpdatedAt        string `json:"updated_at"`
}

type ownerOnlyMigrationReport struct {
	SchemaID         string `json:"schema_id"`
	OK               bool   `json:"ok"`
	Complete         bool   `json:"complete"`
	StoreRef         string `json:"store_ref"`
	ProcessedEntries int64  `json:"processed_entries"`
	EnforcedEntries  int64  `json:"enforced_entries"`
	BatchCount       int64  `json:"batch_count"`
	MaxBatchEntries  int    `json:"max_batch_entries"`
	DurationMillis   int64  `json:"duration_ms"`
	Resumed          bool   `json:"resumed"`
}

type ownerOnlyMigrationOptions struct {
	batchLimit  int
	readChunk   int
	force       bool
	maxDuration time.Duration
	beforeEntry func(string) error
	afterBatch  func(int)
	onProgress  func(ownerOnlyMigrationReport)
}

type ownerOnlyDirectoryFrame struct {
	path    string
	handle  *os.File
	entries []os.DirEntry
	next    int
}

func ownerOnlyStoreRef(kind string) string {
	digest := sha256.Sum256([]byte("contextlattice-store:" + strings.TrimSpace(strings.ToLower(kind))))
	return "store_" + hex.EncodeToString(digest[:8])
}

func ensureOwnerOnlyDirectory(path string, tightenExisting bool) error {
	clean := filepath.Clean(strings.TrimSpace(path))
	if clean == "" || clean == "." {
		return errors.New("owner-only directory path is empty")
	}
	info, err := os.Lstat(clean)
	created := false
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(clean, ownerOnlyDirectoryMode); err != nil {
			return err
		}
		created = true
		info, err = os.Lstat(clean)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("owner-only directory is not a real directory")
	}
	if !tightenExisting && !created {
		return nil
	}
	return enforceOwnerOnlyPermissions(clean, ownerOnlyDirectoryMode)
}

func ensureOwnerOnlyFile(path string) error {
	info, err := os.Lstat(filepath.Clean(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("owner-only file is not a real regular file")
	}
	return enforceOwnerOnlyPermissions(path, ownerOnlyFileMode)
}

func prepareOwnerOnlyFile(path string, dedicatedParent bool) error {
	clean := filepath.Clean(strings.TrimSpace(path))
	if clean == "" || clean == "." {
		return errors.New("owner-only file path is empty")
	}
	if err := ensureOwnerOnlyDirectory(filepath.Dir(clean), dedicatedParent); err != nil {
		return err
	}
	return ensureOwnerOnlyFile(clean)
}

// createOwnerOnlyDurableEmptyFileIfMissing materializes an empty durable store
// without replacing an existing file. O_EXCL closes the startup race between
// the missing-file check and creation; a concurrently created path is verified
// through the same owner-only boundary and preserved byte-for-byte.
func createOwnerOnlyDurableEmptyFileIfMissing(path string, dedicatedParent bool) error {
	clean := filepath.Clean(strings.TrimSpace(path))
	if err := prepareOwnerOnlyFile(clean, dedicatedParent); err != nil {
		return err
	}
	file, err := os.OpenFile(clean, os.O_CREATE|os.O_EXCL|os.O_WRONLY, ownerOnlyFileMode)
	if errors.Is(err, os.ErrExist) {
		return prepareOwnerOnlyFile(clean, dedicatedParent)
	}
	if err != nil {
		return err
	}
	committedError := func(operation string, cause error) error {
		_ = file.Close()
		return &ownerOnlyAtomicWriteError{Operation: operation, Committed: true, Err: cause}
	}
	if err := enforceOwnerOnlyPermissions(clean, ownerOnlyFileMode); err != nil {
		return committedError("enforce owner-only empty file permissions", err)
	}
	if err := file.Sync(); err != nil {
		return committedError("sync owner-only empty file", err)
	}
	if err := file.Close(); err != nil {
		return &ownerOnlyAtomicWriteError{Operation: "close owner-only empty file", Committed: true, Err: err}
	}
	if err := syncOwnerOnlyDirectory(filepath.Dir(clean)); err != nil {
		return &ownerOnlyAtomicWriteError{Operation: "sync owner-only empty file directory", Committed: true, Err: err}
	}
	if err := ensureOwnerOnlyFile(clean); err != nil {
		return &ownerOnlyAtomicWriteError{Operation: "verify owner-only empty file", Committed: true, Err: err}
	}
	return nil
}

func openOwnerOnlyAppend(path string, dedicatedParent bool) (*os.File, error) {
	if err := prepareOwnerOnlyFile(path, dedicatedParent); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, ownerOnlyFileMode)
	if err != nil {
		return nil, err
	}
	if err := enforceOwnerOnlyPermissions(path, ownerOnlyFileMode); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func writeOwnerOnlyAtomicFile(path string, content []byte, dedicatedParent bool) error {
	if err := prepareOwnerOnlyFile(path, dedicatedParent); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := file.Name()
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(tmpPath)
	}
	if err := enforceOwnerOnlyPermissions(tmpPath, ownerOnlyFileMode); err != nil {
		cleanup()
		return err
	}
	if _, err := file.Write(content); err != nil {
		cleanup()
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := replaceOwnerOnlyFile(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := ensureOwnerOnlyFile(path); err != nil {
		return &ownerOnlyAtomicWriteError{Operation: "verify owner-only atomic replacement", Committed: true, Err: err}
	}
	return nil
}

func writeOwnerOnlyDurableAtomicFile(path string, content []byte, dedicatedParent bool) error {
	if err := prepareOwnerOnlyFile(path, dedicatedParent); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := file.Name()
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(tmpPath)
	}
	if err := enforceOwnerOnlyPermissions(tmpPath, ownerOnlyFileMode); err != nil {
		cleanup()
		return err
	}
	if _, err := file.Write(content); err != nil {
		cleanup()
		return err
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := replaceOwnerOnlyFile(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := syncOwnerOnlyDirectory(filepath.Dir(path)); err != nil {
		return &ownerOnlyAtomicWriteError{Operation: "sync owner-only atomic replacement directory", Committed: true, Err: err}
	}
	if err := ensureOwnerOnlyFile(path); err != nil {
		return &ownerOnlyAtomicWriteError{Operation: "verify owner-only durable replacement", Committed: true, Err: err}
	}
	return nil
}

func ownerOnlyPathWithinRoot(root string, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel))
}

func validateOwnerOnlyInternalSymlink(root string, path string) error {
	target, err := os.Readlink(path)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	rootResolved, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return err
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	targetResolved, err := filepath.EvalSymlinks(targetAbs)
	if err != nil {
		return err
	}
	if !ownerOnlyPathWithinRoot(rootResolved, targetResolved) {
		return errors.New("owner-only store symlink escapes declared root")
	}
	return nil
}

func ownerOnlyMigrationLimit() int {
	return clampInt(
		envInt("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_MAX_ENTRIES", ownerOnlyMigrationBatchDefault),
		1,
		ownerOnlyMigrationBatchMax,
	)
}

func ownerOnlyMigrationDepthLimit() int {
	return clampInt(
		envInt("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_MAX_DEPTH", 128),
		8,
		ownerOnlyMigrationMaxDepth,
	)
}

func ownerOnlyStatePath(root string) string {
	return filepath.Join(filepath.Clean(root), "_contextlattice", ownerOnlyStateFile)
}

func loadOwnerOnlyMigrationState(path string) (ownerOnlyMigrationState, error) {
	state := ownerOnlyMigrationState{SchemaID: ownerOnlySchemaID}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return ownerOnlyMigrationState{}, errors.New("owner-only migration state is invalid")
	}
	if state.SchemaID == ownerOnlyLegacySchemaID {
		// v1 used a lexical cursor that could skip valid path-component siblings.
		// Preserve its progress as a resume signal, but never trust completion.
		state.SchemaID = ownerOnlySchemaID
		state.Complete = false
		return state, nil
	}
	if state.SchemaID != ownerOnlySchemaID {
		return ownerOnlyMigrationState{}, errors.New("owner-only migration state schema mismatch")
	}
	return state, nil
}

func persistOwnerOnlyMigrationState(path string, state ownerOnlyMigrationState) error {
	state.SchemaID = ownerOnlySchemaID
	state.UpdatedAt = nowUTCISO()
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return writeOwnerOnlyDurableAtomicFile(path, raw, true)
}

func migrateOwnerOnlyStore(root string) error {
	_, err := migrateOwnerOnlyStoreWithOptions(root, ownerOnlyMigrationOptions{})
	return err
}

func migrateOwnerOnlyStoreWithOptions(root string, options ownerOnlyMigrationOptions) (ownerOnlyMigrationReport, error) {
	startedAt := time.Now()
	root = filepath.Clean(strings.TrimSpace(root))
	report := ownerOnlyMigrationReport{
		SchemaID:        ownerOnlyMigrationReportSchemaID,
		StoreRef:        ownerOnlyStoreRef("owner_only_migration:" + root),
		MaxBatchEntries: ownerOnlyMigrationBatchMax,
	}
	finish := func(err error) (ownerOnlyMigrationReport, error) {
		report.OK = err == nil
		report.Complete = err == nil
		report.DurationMillis = time.Since(startedAt).Milliseconds()
		return report, err
	}
	if root == "" || root == "." {
		return finish(errors.New("owner-only store root is empty"))
	}
	if err := ensureOwnerOnlyDirectory(root, true); err != nil {
		return finish(err)
	}
	statePath := ownerOnlyStatePath(root)
	if err := ensureOwnerOnlyDirectory(filepath.Dir(statePath), true); err != nil {
		return finish(err)
	}
	unlock, err := lockOwnerOnlyMigration(filepath.Join(filepath.Dir(statePath), ownerOnlyLockFile))
	if err != nil {
		return finish(err)
	}
	defer unlock()
	rootIdentity, err := ownerOnlyRootIdentity(root)
	if err != nil {
		return finish(err)
	}
	state, err := loadOwnerOnlyMigrationState(statePath)
	if err != nil {
		return finish(err)
	}
	if state.Complete && !options.force && state.RootIdentity == rootIdentity && state.WriterPolicy == ownerOnlyWriterPolicyVersion {
		// The owner-only root blocks non-owner traversal, and supported writers
		// enforce descendants. Same-owner out-of-band writers must use --force.
		err := ensureOwnerOnlyFile(statePath)
		report.ProcessedEntries = state.ProcessedEntries
		report.EnforcedEntries = state.EnforcedEntries
		report.BatchCount = state.BatchCount
		return finish(err)
	}
	report.Resumed = strings.TrimSpace(state.Cursor) != "" || state.ProcessedEntries > 0

	batchLimit := options.batchLimit
	if batchLimit <= 0 {
		batchLimit = ownerOnlyMigrationLimit()
	}
	if batchLimit > ownerOnlyMigrationBatchMax {
		batchLimit = ownerOnlyMigrationBatchMax
	}
	readChunk := options.readChunk
	if readChunk <= 0 {
		readChunk = ownerOnlyMigrationReadChunk
	}
	if readChunk > ownerOnlyMigrationBatchMax {
		readChunk = ownerOnlyMigrationBatchMax
	}
	report.MaxBatchEntries = batchLimit
	// Cursor is durable progress evidence, not a skip token. Restarted passes
	// deliberately recheck from the root so concurrent inserts cannot be missed.
	state = ownerOnlyMigrationState{
		SchemaID:     ownerOnlySchemaID,
		RootIdentity: rootIdentity,
		WriterPolicy: ownerOnlyWriterPolicyVersion,
		Phase:        "migrating",
	}
	batchEntries := 0
	lastCursor := ""
	persistBatch := func() error {
		if batchEntries == 0 {
			return nil
		}
		report.BatchCount++
		state.Cursor = lastCursor
		state.ProcessedEntries = report.ProcessedEntries
		state.EnforcedEntries = report.EnforcedEntries
		state.BatchCount = report.BatchCount
		state.Phase = "migrating"
		if err := persistOwnerOnlyMigrationState(statePath, state); err != nil {
			return err
		}
		if options.afterBatch != nil {
			options.afterBatch(batchEntries)
		}
		if options.onProgress != nil {
			progress := report
			progress.DurationMillis = time.Since(startedAt).Milliseconds()
			options.onProgress(progress)
		}
		batchEntries = 0
		runtime.Gosched()
		return nil
	}

	deadline := time.Time{}
	if options.maxDuration > 0 {
		deadline = time.Now().Add(options.maxDuration)
	}
	err = walkOwnerOnlyStoreStreaming(root, readChunk, ownerOnlyMigrationDepthLimit(), func(path string, parent *os.File, name string) (bool, error) {
		if !deadline.IsZero() && report.ProcessedEntries > 0 && time.Now().After(deadline) {
			return false, errOwnerOnlyMigrationYield
		}
		if filepath.Clean(path) == filepath.Clean(statePath) {
			return false, nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return false, relErr
		}
		rel = filepath.ToSlash(rel)
		if options.beforeEntry != nil {
			if err := options.beforeEntry(rel); err != nil {
				return false, err
			}
		}
		enforced, isDirectory, entryErr := migrateOwnerOnlyEntryAt(root, parent, name, path)
		if entryErr != nil {
			return false, entryErr
		}
		report.ProcessedEntries++
		if enforced {
			report.EnforcedEntries++
		}
		batchEntries++
		lastCursor = rel
		if batchEntries >= batchLimit {
			if err := persistBatch(); err != nil {
				return false, err
			}
		}
		return isDirectory, nil
	})
	if err != nil {
		if persistErr := persistBatch(); persistErr != nil {
			err = errors.Join(err, persistErr)
		}
		return finish(err)
	}
	if err := persistBatch(); err != nil {
		return finish(err)
	}
	state.Complete = true
	state.Cursor = ""
	state.Phase = "complete"
	state.ProcessedEntries = report.ProcessedEntries
	state.EnforcedEntries = report.EnforcedEntries
	state.BatchCount = report.BatchCount
	if err := persistOwnerOnlyMigrationState(statePath, state); err != nil {
		return finish(err)
	}
	return finish(nil)
}

func migrateOwnerOnlyEntryAt(root string, parent *os.File, name string, path string) (bool, bool, error) {
	if !ownerOnlyPathWithinRoot(root, path) {
		return false, false, errors.New("owner-only migration path escaped declared root")
	}
	return enforceOwnerOnlyEntryAt(root, parent, name, path)
}

func walkOwnerOnlyStoreStreaming(
	root string,
	readChunk int,
	maxDepth int,
	visit func(string, *os.File, string) (bool, error),
) error {
	if readChunk < 1 {
		readChunk = 1
	}
	if maxDepth < 1 {
		maxDepth = 1
	}
	rootHandle, err := openOwnerOnlyRootDirectory(root)
	if err != nil {
		return err
	}
	frames := []*ownerOnlyDirectoryFrame{{path: root, handle: rootHandle}}
	closeFrames := func() {
		for _, frame := range frames {
			_ = frame.handle.Close()
		}
	}
	defer closeFrames()

	for len(frames) > 0 {
		frame := frames[len(frames)-1]
		if frame.next >= len(frame.entries) {
			entries, readErr := frame.handle.ReadDir(readChunk)
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return readErr
			}
			frame.entries = entries
			frame.next = 0
			if len(entries) == 0 {
				_ = frame.handle.Close()
				frames = frames[:len(frames)-1]
				continue
			}
		}

		entry := frame.entries[frame.next]
		frame.next++
		path := filepath.Join(frame.path, entry.Name())
		descend, err := visit(path, frame.handle, entry.Name())
		if err != nil {
			return err
		}
		if !descend {
			continue
		}
		if len(frames) >= maxDepth {
			return errors.New("owner-only store exceeds maximum traversal depth")
		}
		handle, err := openOwnerOnlyDirectoryAt(frame.handle, entry.Name(), path)
		if err != nil {
			return err
		}
		frames = append(frames, &ownerOnlyDirectoryFrame{path: path, handle: handle})
	}
	return nil
}
