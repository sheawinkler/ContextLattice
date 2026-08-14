package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

const (
	memoryEdgeLogStateSchemaID       = "contextlattice_memory_edge_log_state.v1"
	memoryEdgeLogMaxRecoveryBytes    = int64(256 * 1024 * 1024)
	memoryEdgeLogMaxStateBytes       = int64(64 * 1024)
	memoryEdgeLogMaxReplacementBytes = memoryEdgeLogMaxRecoveryBytes
	memoryExactStateIndexMaxBytes    = int64(16 * 1024 * 1024)
)

var errMemoryEdgeLogWriterFenceRequired = errors.New("memory edge log writer fence is required")
var errMemoryEdgeLogOversized = errors.New("memory edge log exceeds its bounded recovery cap")
var errMemoryEdgeLogChangedDuringRead = errors.New("memory edge log changed during bounded read")
var errMemoryEdgeLogPartialWrite = errors.New("memory edge log append left a partial row")
var errMemoryEdgeLogDurabilityAmbiguous = errors.New("memory edge log append durability is ambiguous")

func memoryEdgeLogRecoveryCap(m *memoryStore, requested int64) int64 {
	capBytes := requested
	if capBytes <= 0 && m != nil {
		capBytes = m.policy.historyStartupTailMaxBytes
	}
	if capBytes <= 0 || capBytes > memoryEdgeLogMaxRecoveryBytes {
		capBytes = memoryEdgeLogMaxRecoveryBytes
	}
	return capBytes
}

// readOwnerOnlyBoundedFileWithInfo performs a pre-stat before allocating or
// reading. The stream is deliberately limited to cap+1 so a corrupt/oversized
// artifact is rejected without ever materializing the complete file in memory.
// The returned FileInfo belongs to the descriptor that supplied the bytes.
func readOwnerOnlyBoundedFileWithInfo(path string, maxBytes int64) ([]byte, os.FileInfo, error) {
	if maxBytes < 1 {
		return nil, nil, fmt.Errorf("bounded file cap must be positive")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("bounded owner-only file is not a regular file")
	}
	if info.Size() > maxBytes {
		return nil, nil, fmt.Errorf("%w: %d > %d", errMemoryEdgeLogOversized, info.Size(), maxBytes)
	}
	file, err := openOwnerOnlyReadPlatform(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(raw)) > maxBytes {
		return nil, nil, fmt.Errorf("%w: stream exceeded %d bytes", errMemoryEdgeLogOversized, maxBytes)
	}
	after, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	pathAfter, pathErr := os.Lstat(path)
	if pathErr != nil {
		return nil, nil, pathErr
	}
	if !os.SameFile(info, after) || !os.SameFile(info, pathAfter) || after.Size() != int64(len(raw)) {
		return nil, nil, errMemoryEdgeLogChangedDuringRead
	}
	return raw, after, nil
}

func readOwnerOnlyBoundedFile(path string, maxBytes int64) ([]byte, error) {
	raw, _, err := readOwnerOnlyBoundedFileWithInfo(path, maxBytes)
	return raw, err
}

type memoryEdgeLogReplacementError struct {
	Committed bool
	Err       error
}

func (e *memoryEdgeLogReplacementError) Error() string {
	if e == nil || e.Err == nil {
		return "memory edge log replacement failed"
	}
	return e.Err.Error()
}

func (e *memoryEdgeLogReplacementError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func memoryEdgeLogReplacementFailure(committed bool, err error) error {
	if err == nil {
		return nil
	}
	return &memoryEdgeLogReplacementError{Committed: committed, Err: err}
}

// memoryEdgeLogFenceToken is the process-local capability for mutating the
// edge log. Every supported product writer (ordinary upsert, bounded
// backfill, compaction, repair apply, and rollback) must hold the same
// cross-process owner-only fence and pass this token into its locked mutation.
// The legacy *_Locked entry points deliberately reject a missing token so a
// future writer cannot silently bypass the single-writer authority.
// A process that mutates the file directly without this owner fence is outside
// the supported product mutation authority; descriptor-bound identity/stamp
// checks detect that unsupported path and fail closed or exact-reconcile.
type memoryEdgeLogFenceToken struct {
	store    *memoryStore
	unlock   func()
	validate func() error
	released atomic.Bool
}

func (m *memoryStore) acquireMemoryEdgeLogFence() (*memoryEdgeLogFenceToken, error) {
	return m.acquireMemoryEdgeLogFenceContext(context.Background())
}

func (m *memoryStore) acquireMemoryEdgeLogFenceContext(ctx context.Context) (*memoryEdgeLogFenceToken, error) {
	unlock, validate, err := m.lockMemoryEdgeLogContext(ctx)
	if err != nil {
		return nil, err
	}
	return &memoryEdgeLogFenceToken{store: m, unlock: unlock, validate: validate}, nil
}

func (m *memoryStore) acquireMemoryEdgeLogFenceOptional() (*memoryEdgeLogFenceToken, error) {
	return m.acquireMemoryEdgeLogFenceOptionalContext(context.Background())
}

func (m *memoryStore) acquireMemoryEdgeLogFenceOptionalContext(ctx context.Context) (*memoryEdgeLogFenceToken, error) {
	if m == nil || strings.TrimSpace(m.policy.edgePath) == "" {
		return nil, nil
	}
	return m.acquireMemoryEdgeLogFenceContext(ctx)
}

func (f *memoryEdgeLogFenceToken) validFor(m *memoryStore) bool {
	return f != nil && f.store == m && !f.released.Load()
}

func (f *memoryEdgeLogFenceToken) validateFor(m *memoryStore) error {
	if !f.validFor(m) {
		return errMemoryEdgeLogWriterFenceRequired
	}
	if f.validate != nil {
		if err := f.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (f *memoryEdgeLogFenceToken) release() {
	if f == nil || !f.released.CompareAndSwap(false, true) {
		return
	}
	if f.unlock != nil {
		f.unlock()
	}
}

func requireMemoryEdgeLogFence(m *memoryStore, fence *memoryEdgeLogFenceToken) error {
	return fence.validateFor(m)
}

func requireMemoryEdgeLogFenceOptional(m *memoryStore, fence *memoryEdgeLogFenceToken) error {
	if m == nil || strings.TrimSpace(m.policy.edgePath) == "" {
		return nil
	}
	return requireMemoryEdgeLogFence(m, fence)
}

type memoryEdgeLogState struct {
	SchemaID            string `json:"schema_id"`
	Version             int    `json:"version"`
	Generation          uint64 `json:"generation"`
	Digest              string `json:"digest"`
	ContentDigest       string `json:"content_digest,omitempty"`
	ParentContentDigest string `json:"parent_content_digest,omitempty"`
	ParentFileSize      int64  `json:"parent_file_size,omitempty"`
	ParentFileIdentity  string `json:"parent_file_identity,omitempty"`
	ContentHashState    string `json:"content_hash_state,omitempty"`
	ContentHashedBytes  int64  `json:"content_hashed_bytes,omitempty"`
	FileSize            int64  `json:"file_size"`
	FileIdentity        string `json:"file_identity,omitempty"`
	FileModTimeNanos    int64  `json:"file_mod_time_nanos,omitempty"`
	FileChangeToken     string `json:"file_change_token,omitempty"`
	UpdatedAt           string `json:"updated_at"`
}

type memoryEdgeLogSnapshot struct {
	Bytes               []byte
	Generation          uint64
	Digest              string
	ContentDigest       string
	ParentContentDigest string
	ParentFileSize      int64
	ParentFileIdentity  string
	ContentHashState    string
	FileSize            int64
	FileStamp           memoryEdgeLogFileStamp
}

type memoryEdgeLogFileStamp struct {
	Exists       bool
	Size         int64
	Identity     string
	ModTimeNanos int64
	ChangeToken  string
}

// memoryEdgeLogAppender carries an exact content hash forward while the
// process-wide and cross-process writer fence is held. This avoids re-reading
// a potentially large log for every row in a bounded repair chunk without
// weakening the sidecar's exact-content contract.
type memoryEdgeLogAppender struct {
	store    *memoryStore
	fence    *memoryEdgeLogFenceToken
	state    memoryEdgeLogState
	content  hash.Hash
	syncFile bool
	failed   bool
}

func memoryEdgeLogFencePath(m *memoryStore) string {
	return m.policy.edgePath + ".writer.lock"
}

func memoryEdgeLogStatePath(m *memoryStore) string {
	return m.policy.edgePath + ".state.json"
}

// readMemoryEdgeLogBytesLocked binds the entire read to one descriptor.  The
// lstat preflight rejects an oversized or symlinked path before any allocation;
// the descriptor stamp and post-read size close the rename/truncate race.
func (m *memoryStore) readMemoryEdgeLogBytesLocked(maxBytes int64) ([]byte, memoryEdgeLogFileStamp, error) {
	return m.readMemoryEdgeLogBytesContextLocked(context.Background(), maxBytes)
}

// readMemoryEdgeLogBytesContextLocked keeps the descriptor-bound read bounded
// and checks cancellation between every read chunk.  The old io.ReadAll path
// made a cancelled repair request wait for the complete recovery cap while the
// exclusive fence was held.
func (m *memoryStore) readMemoryEdgeLogBytesContextLocked(ctx context.Context, maxBytes int64) ([]byte, memoryEdgeLogFileStamp, error) {
	if m == nil || strings.TrimSpace(m.policy.edgePath) == "" {
		return nil, memoryEdgeLogFileStamp{}, errors.New("memory edge log path is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	capBytes := memoryEdgeLogRecoveryCap(m, maxBytes)
	if err := ctx.Err(); err != nil {
		return nil, memoryEdgeLogFileStamp{}, err
	}
	info, err := os.Lstat(m.policy.edgePath)
	if errors.Is(err, os.ErrNotExist) {
		return []byte{}, memoryEdgeLogFileStamp{Identity: "absent", ChangeToken: "absent"}, nil
	}
	if err != nil {
		return nil, memoryEdgeLogFileStamp{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, memoryEdgeLogFileStamp{}, errors.New("memory edge log path is not a regular file")
	}
	if info.Size() > capBytes {
		return nil, memoryEdgeLogFileStamp{}, fmt.Errorf("%w: %d > %d", errMemoryEdgeLogOversized, info.Size(), capBytes)
	}
	file, err := openOwnerOnlyReadPlatform(m.policy.edgePath)
	if errors.Is(err, os.ErrNotExist) {
		return []byte{}, memoryEdgeLogFileStamp{Identity: "absent", ChangeToken: "absent"}, nil
	}
	if err != nil {
		return nil, memoryEdgeLogFileStamp{}, err
	}
	defer file.Close()
	descriptorInfo, err := file.Stat()
	if err != nil {
		return nil, memoryEdgeLogFileStamp{}, err
	}
	if !os.SameFile(info, descriptorInfo) {
		return nil, memoryEdgeLogFileStamp{}, errMemoryEdgeLogChangedDuringRead
	}
	before, err := memoryEdgeLogPlatformFileStampForFile(file)
	if err != nil {
		return nil, memoryEdgeLogFileStamp{}, err
	}
	if before.Size > capBytes {
		return nil, memoryEdgeLogFileStamp{}, fmt.Errorf("%w: descriptor size %d > %d", errMemoryEdgeLogOversized, before.Size, capBytes)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, memoryEdgeLogFileStamp{}, err
	}
	readCap := capBytes + 1
	raw := make([]byte, 0, minInt64(before.Size, 64*1024))
	buf := make([]byte, 64*1024)
	for readCap > 0 {
		select {
		case <-ctx.Done():
			return nil, memoryEdgeLogFileStamp{}, ctx.Err()
		default:
		}
		want := int64(len(buf))
		if want > readCap {
			want = readCap
		}
		n, readErr := file.Read(buf[:want])
		if n > 0 {
			raw = append(raw, buf[:n]...)
			readCap -= int64(n)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, memoryEdgeLogFileStamp{}, readErr
		}
		if n == 0 {
			return nil, memoryEdgeLogFileStamp{}, io.ErrNoProgress
		}
	}
	if int64(len(raw)) > capBytes {
		return nil, memoryEdgeLogFileStamp{}, fmt.Errorf("%w: stream exceeded %d bytes", errMemoryEdgeLogOversized, capBytes)
	}
	after, err := memoryEdgeLogPlatformFileStampForFile(file)
	if err != nil {
		return nil, memoryEdgeLogFileStamp{}, err
	}
	pathAfter, pathErr := memoryEdgeLogPlatformFileStamp(m.policy.edgePath)
	if pathErr != nil {
		return nil, memoryEdgeLogFileStamp{}, pathErr
	}
	if before != after || after != pathAfter || after.Size != int64(len(raw)) {
		return nil, memoryEdgeLogFileStamp{}, errMemoryEdgeLogChangedDuringRead
	}
	if m.memoryEdgeLogObserveIO != nil {
		m.memoryEdgeLogObserveIO("full_scan_read", int64(len(raw)))
	}
	return raw, after, nil
}

// readMemoryEdgeLogSuffixLocked reads only the bytes appended after offset.
// The sidecar content hash state and descriptor stamp authenticate the prefix;
// this descriptor-bound read then verifies the suffix and final stamp without
// rehashing or rematerializing the existing log.
func (m *memoryStore) readMemoryEdgeLogSuffixLocked(ctx context.Context, offset, expectedSize, maxBytes int64, expected memoryEdgeLogFileStamp) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if offset < 0 || expectedSize < offset || expectedSize > maxBytes || maxBytes < 1 {
		return nil, fmt.Errorf("memory edge log suffix bounds are invalid")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := openOwnerOnlyReadPlatform(m.policy.edgePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	before, err := memoryEdgeLogPlatformFileStampForFile(file)
	if err != nil {
		return nil, err
	}
	if before != expected || before.Size != expectedSize {
		return nil, errMemoryEdgeLogChangedDuringRead
	}
	pathBefore, err := memoryEdgeLogPlatformFileStamp(m.policy.edgePath)
	if err != nil {
		return nil, err
	}
	if pathBefore != before {
		return nil, errMemoryEdgeLogChangedDuringRead
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	remaining := expectedSize - offset
	var out bytes.Buffer
	if remaining > 0 {
		out.Grow(int(minInt64(remaining, 64*1024)))
	}
	buf := make([]byte, 64*1024)
	for remaining > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		want := int64(len(buf))
		if want > remaining {
			want = remaining
		}
		n, readErr := file.Read(buf[:want])
		if n > 0 {
			_, _ = out.Write(buf[:n])
			remaining -= int64(n)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) && remaining == 0 {
				break
			}
			return nil, readErr
		}
		if n == 0 {
			return nil, io.ErrNoProgress
		}
	}
	// A byte beyond the authenticated size means the file changed while the
	// descriptor was open; do not silently treat it as a valid append.
	var extra [1]byte
	if n, readErr := file.Read(extra[:]); n != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
		return nil, errMemoryEdgeLogChangedDuringRead
	}
	after, err := memoryEdgeLogPlatformFileStampForFile(file)
	if err != nil {
		return nil, err
	}
	pathAfter, err := memoryEdgeLogPlatformFileStamp(m.policy.edgePath)
	if err != nil {
		return nil, err
	}
	if after != before || pathAfter != before || after.Size != expectedSize {
		return nil, errMemoryEdgeLogChangedDuringRead
	}
	if m.memoryEdgeLogObserveIO != nil {
		m.memoryEdgeLogObserveIO("incremental_evidence_read", int64(out.Len()))
	}
	return out.Bytes(), nil
}

func (m *memoryStore) currentMemoryEdgeLogStateLocked(maxBytes int64) (memoryEdgeLogState, error) {
	state, err := m.readMemoryEdgeLogStateLocked()
	if err == nil {
		stamp, stampErr := memoryEdgeLogPlatformFileStamp(m.policy.edgePath)
		if stampErr == nil && memoryEdgeLogStateMatchesStamp(state, stamp) {
			if _, restoreErr := memoryEdgeLogHashFromState(state); restoreErr == nil {
				return state, nil
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return memoryEdgeLogState{}, err
	}
	snapshot, snapshotErr := m.snapshotMemoryEdgeLogLocked(maxBytes)
	if snapshotErr != nil {
		return memoryEdgeLogState{}, snapshotErr
	}
	return memoryEdgeLogState{
		SchemaID:           memoryEdgeLogStateSchemaID,
		Version:            1,
		Generation:         snapshot.Generation,
		Digest:             snapshot.Digest,
		ContentDigest:      snapshot.ContentDigest,
		ContentHashState:   snapshot.ContentHashState,
		ContentHashedBytes: snapshot.FileSize,
		FileSize:           snapshot.FileSize,
		FileIdentity:       snapshot.FileStamp.Identity,
		FileModTimeNanos:   snapshot.FileStamp.ModTimeNanos,
		FileChangeToken:    snapshot.FileStamp.ChangeToken,
	}, nil
}

func (m *memoryStore) lockMemoryEdgeLog() (func(), error) {
	unlock, _, err := m.lockMemoryEdgeLogContext(context.Background())
	return unlock, err
}

func (m *memoryStore) lockMemoryEdgeLogContext(ctx context.Context) (func(), func() error, error) {
	if m == nil || strings.TrimSpace(m.policy.edgePath) == "" {
		return nil, nil, errors.New("memory edge log path is unavailable")
	}
	if err := ensureOwnerOnlyDirectory(filepath.Dir(m.policy.edgePath), true); err != nil {
		return nil, nil, err
	}
	lease, err := lockOwnerOnlyFileContextWithValidation(ctx, memoryEdgeLogFencePath(m))
	if err != nil {
		return nil, nil, err
	}
	return lease.unlock, lease.validate, nil
}

func memoryEdgeLogContentDigest(raw []byte) string {
	return "sha256:" + sha256Hex(string(raw))
}

func memoryEdgeLogNextDigest(previous string, generation uint64, operation, contentDigest string) string {
	return "sha256:" + sha256Hex(previous+"\x00"+fmt.Sprintf("%d", generation)+"\x00"+operation+"\x00"+contentDigest)
}

func memoryEdgeLogMarshalHashState(content hash.Hash) (string, error) {
	marshaler, ok := content.(encoding.BinaryMarshaler)
	if !ok {
		return "", errors.New("memory edge log SHA-256 state is not serializable")
	}
	raw, err := marshaler.MarshalBinary()
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

func memoryEdgeLogHashForBytes(raw []byte) (hash.Hash, string, string, error) {
	content := sha256.New()
	_, _ = content.Write(raw)
	hashState, err := memoryEdgeLogMarshalHashState(content)
	if err != nil {
		return nil, "", "", err
	}
	return content, "sha256:" + hex.EncodeToString(content.Sum(nil)), hashState, nil
}

func memoryEdgeLogHashFromState(state memoryEdgeLogState) (hash.Hash, error) {
	if state.ContentHashedBytes < 0 || state.ContentHashedBytes != state.FileSize || strings.TrimSpace(state.ContentHashState) == "" {
		return nil, errors.New("memory edge log hash state byte count is invalid")
	}
	raw, err := base64.StdEncoding.DecodeString(state.ContentHashState)
	if err != nil {
		return nil, errors.New("memory edge log hash state is invalid")
	}
	content := sha256.New()
	unmarshaler, ok := content.(encoding.BinaryUnmarshaler)
	if !ok {
		return nil, errors.New("memory edge log SHA-256 state is not restorable")
	}
	if err := unmarshaler.UnmarshalBinary(raw); err != nil {
		return nil, errors.New("memory edge log hash state is invalid")
	}
	if digest := "sha256:" + hex.EncodeToString(content.Sum(nil)); digest != state.ContentDigest {
		return nil, errors.New("memory edge log hash state does not match its content digest")
	}
	return content, nil
}

func memoryEdgeLogStateMatchesStamp(state memoryEdgeLogState, stamp memoryEdgeLogFileStamp) bool {
	return state.FileSize == stamp.Size &&
		state.FileIdentity == stamp.Identity &&
		state.FileModTimeNanos == stamp.ModTimeNanos &&
		state.FileChangeToken == stamp.ChangeToken &&
		state.ContentHashedBytes == state.FileSize
}

func memoryEdgeLogAppendStampMatchesState(state memoryEdgeLogState, stamp memoryEdgeLogFileStamp) bool {
	if memoryEdgeLogStateMatchesStamp(state, stamp) {
		return true
	}
	// A zero-byte log can legitimately transition from the sidecar's explicit
	// absent identity to the descriptor created by O_CREATE. The descriptor and
	// path are still compared below before the first byte is written.
	return state.FileSize == 0 && state.ContentHashedBytes == 0 &&
		state.FileIdentity == "absent" && state.FileChangeToken == "absent" &&
		stamp.Exists && stamp.Size == 0
}

func memoryEdgeLogStateWithStamp(state memoryEdgeLogState, stamp memoryEdgeLogFileStamp) memoryEdgeLogState {
	state.FileSize = stamp.Size
	state.FileIdentity = stamp.Identity
	state.FileModTimeNanos = stamp.ModTimeNanos
	state.FileChangeToken = stamp.ChangeToken
	return state
}

func (m *memoryStore) readMemoryEdgeLogStateLocked() (memoryEdgeLogState, error) {
	raw, err := readOwnerOnlyBoundedFile(memoryEdgeLogStatePath(m), memoryEdgeLogMaxStateBytes)
	if err != nil {
		return memoryEdgeLogState{}, err
	}
	var state memoryEdgeLogState
	if json.Unmarshal(raw, &state) != nil || state.SchemaID != memoryEdgeLogStateSchemaID || state.Version != 1 || state.Generation == 0 || state.Digest == "" {
		return memoryEdgeLogState{}, errors.New("memory edge log state is invalid")
	}
	return state, nil
}

func (m *memoryStore) loadMemoryEdgeLogStateLocked(raw []byte, stamp memoryEdgeLogFileStamp) (memoryEdgeLogState, error) {
	state := memoryEdgeLogState{}
	stateRaw, err := readOwnerOnlyBoundedFile(memoryEdgeLogStatePath(m), memoryEdgeLogMaxStateBytes)
	if err == nil {
		if json.Unmarshal(stateRaw, &state) != nil || state.SchemaID != memoryEdgeLogStateSchemaID || state.Version != 1 || state.Generation == 0 || state.Digest == "" {
			return state, errors.New("memory edge log state is invalid")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return state, err
	}
	_, contentDigest, contentHashState, err := memoryEdgeLogHashForBytes(raw)
	if err != nil {
		return state, err
	}
	if stamp.Size != int64(len(raw)) {
		return state, errors.New("memory edge log changed during exact snapshot")
	}
	if state.Generation == 0 {
		state = memoryEdgeLogState{SchemaID: memoryEdgeLogStateSchemaID, Version: 1, Generation: 1, ContentDigest: contentDigest, ContentHashState: contentHashState, ContentHashedBytes: int64(len(raw))}
		state = memoryEdgeLogStateWithStamp(state, stamp)
		state.Digest = memoryEdgeLogNextDigest("", state.Generation, "initialize", contentDigest)
		if err := m.writeMemoryEdgeLogStateLocked(state); err != nil {
			return state, err
		}
		return state, nil
	}
	// Every valid sidecar is exact. A missing legacy digest, size drift, or a
	// same-size replacement proves an interrupted/out-of-band mutation. Repair
	// the generation under the writer fence; if the state write fails, callers
	// fail closed and the next fenced capture retries the same reconciliation.
	contentChanged := state.FileSize != int64(len(raw)) || state.ContentDigest != contentDigest
	stampChanged := !memoryEdgeLogStateMatchesStamp(state, stamp)
	if contentChanged || stampChanged {
		previousContentDigest := state.ContentDigest
		previousFileSize := state.FileSize
		previousFileIdentity := state.FileIdentity
		state.Generation++
		state.Digest = memoryEdgeLogNextDigest(state.Digest, state.Generation, "reconcile", contentDigest)
		state.ParentContentDigest = previousContentDigest
		state.ParentFileSize = previousFileSize
		state.ParentFileIdentity = previousFileIdentity
	}
	metadataChanged := contentChanged || stampChanged || state.ContentHashState != contentHashState || state.ContentHashedBytes != int64(len(raw))
	state.ContentDigest = contentDigest
	state.ContentHashState = contentHashState
	state.ContentHashedBytes = int64(len(raw))
	state = memoryEdgeLogStateWithStamp(state, stamp)
	if metadataChanged {
		if err := m.writeMemoryEdgeLogStateLocked(state); err != nil {
			return state, err
		}
	}
	return state, nil
}

func (m *memoryStore) writeMemoryEdgeLogStateLocked(state memoryEdgeLogState) error {
	return m.writeMemoryEdgeLogStateVerifiedLocked(state, nil, false)
}

func (m *memoryStore) writeMemoryEdgeLogStateVerifiedLocked(state memoryEdgeLogState, expectedBytes []byte, verifyFile bool) error {
	if state.Generation == 0 || strings.TrimSpace(state.Digest) == "" || strings.TrimSpace(state.ContentDigest) == "" {
		return errors.New("memory edge log state identity is incomplete")
	}
	if strings.TrimSpace(state.FileIdentity) == "" || strings.TrimSpace(state.FileChangeToken) == "" || state.FileSize < 0 {
		return errors.New("memory edge log state file stamp is incomplete")
	}
	if _, err := memoryEdgeLogHashFromState(state); err != nil {
		return err
	}
	if m.memoryEdgeLogBeforeStateWrite != nil {
		if err := m.memoryEdgeLogBeforeStateWrite(state); err != nil {
			return err
		}
	}
	if verifyFile {
		actual, stamp, err := m.readMemoryEdgeLogBytesLocked(memoryEdgeLogMaxReplacementBytes)
		if err != nil {
			return fmt.Errorf("verify replacement bytes before state commit: %w", err)
		}
		if !bytes.Equal(actual, expectedBytes) {
			return errors.New("memory edge log replacement changed before state commit")
		}
		if m.memoryEdgeLogObserveIO != nil {
			m.memoryEdgeLogObserveIO("replacement_verify_hash", int64(len(actual)))
		}
		_, contentDigest, contentHashState, err := memoryEdgeLogHashForBytes(actual)
		if err != nil {
			return err
		}
		if contentDigest != state.ContentDigest || contentHashState != state.ContentHashState || stamp.Size != state.FileSize || !memoryEdgeLogStateMatchesStamp(state, stamp) {
			return errors.New("memory edge log replacement bytes or stamp do not match state")
		}
	}
	state.SchemaID, state.Version, state.UpdatedAt = memoryEdgeLogStateSchemaID, 1, nowUTCISO()
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if int64(len(raw))+1 > memoryEdgeLogMaxStateBytes {
		return fmt.Errorf("%w: edge-log state bytes=%d cap=%d", errMemoryEdgeLogOversized, len(raw)+1, memoryEdgeLogMaxStateBytes)
	}
	return writeOwnerOnlyDurableAtomicFile(memoryEdgeLogStatePath(m), append(raw, '\n'), true)
}

func (m *memoryStore) snapshotMemoryEdgeLogLocked(maxBytes int64) (memoryEdgeLogSnapshot, error) {
	return m.snapshotMemoryEdgeLogContextLocked(context.Background(), maxBytes)
}

func (m *memoryStore) snapshotMemoryEdgeLogContextLocked(ctx context.Context, maxBytes int64) (memoryEdgeLogSnapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return memoryEdgeLogSnapshot{}, err
	}
	raw, afterStamp, err := m.readMemoryEdgeLogBytesContextLocked(ctx, maxBytes)
	if err != nil {
		return memoryEdgeLogSnapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return memoryEdgeLogSnapshot{}, err
	}
	if m.memoryEdgeLogObserveIO != nil {
		m.memoryEdgeLogObserveIO("full_scan_hash", int64(len(raw)))
	}
	state, err := m.loadMemoryEdgeLogStateLocked(raw, afterStamp)
	if err != nil {
		return memoryEdgeLogSnapshot{}, err
	}
	return memoryEdgeLogSnapshot{
		Bytes:               raw,
		Generation:          state.Generation,
		Digest:              state.Digest,
		ContentDigest:       state.ContentDigest,
		ParentContentDigest: state.ParentContentDigest,
		ParentFileSize:      state.ParentFileSize,
		ParentFileIdentity:  state.ParentFileIdentity,
		ContentHashState:    state.ContentHashState,
		FileSize:            state.FileSize,
		FileStamp:           afterStamp,
	}, nil
}

func (m *memoryStore) snapshotMemoryEdgeLog(maxBytes int64) (memoryEdgeLogSnapshot, error) {
	return m.snapshotMemoryEdgeLogContext(context.Background(), maxBytes)
}

func (m *memoryStore) snapshotMemoryEdgeLogContext(ctx context.Context, maxBytes int64) (memoryEdgeLogSnapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	fence, err := m.acquireMemoryEdgeLogFenceContext(ctx)
	if err != nil {
		return memoryEdgeLogSnapshot{}, err
	}
	defer fence.release()
	if err := ctx.Err(); err != nil {
		return memoryEdgeLogSnapshot{}, err
	}
	return m.snapshotMemoryEdgeLogContextLocked(ctx, maxBytes)
}

func newMemoryEdgeLogAppenderLocked(m *memoryStore, snapshot memoryEdgeLogSnapshot, syncFile bool) (*memoryEdgeLogAppender, error) {
	return nil, errMemoryEdgeLogWriterFenceRequired
}

func newMemoryEdgeLogAppenderWithFenceLocked(m *memoryStore, snapshot memoryEdgeLogSnapshot, syncFile bool, fence *memoryEdgeLogFenceToken) (*memoryEdgeLogAppender, error) {
	if err := requireMemoryEdgeLogFence(m, fence); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("memory edge log store is unavailable")
	}
	state := memoryEdgeLogState{
		SchemaID:            memoryEdgeLogStateSchemaID,
		Version:             1,
		Generation:          snapshot.Generation,
		Digest:              snapshot.Digest,
		ContentDigest:       snapshot.ContentDigest,
		ParentContentDigest: snapshot.ParentContentDigest,
		ParentFileSize:      snapshot.ParentFileSize,
		ParentFileIdentity:  snapshot.ParentFileIdentity,
		ContentHashState:    snapshot.ContentHashState,
		ContentHashedBytes:  snapshot.FileSize,
	}
	state = memoryEdgeLogStateWithStamp(state, snapshot.FileStamp)
	if snapshot.Generation == 0 || strings.TrimSpace(snapshot.Digest) == "" || snapshot.FileSize != int64(len(snapshot.Bytes)) || !memoryEdgeLogStateMatchesStamp(state, snapshot.FileStamp) {
		return nil, errors.New("memory edge log snapshot is not exact")
	}
	content, err := memoryEdgeLogHashFromState(state)
	if err != nil {
		return nil, err
	}
	return &memoryEdgeLogAppender{
		store:    m,
		fence:    fence,
		state:    state,
		content:  content,
		syncFile: syncFile,
	}, nil
}

func (m *memoryStore) newMemoryEdgeLogAppenderFastLocked(syncFile bool) (*memoryEdgeLogAppender, error) {
	return nil, errMemoryEdgeLogWriterFenceRequired
}

func (m *memoryStore) newMemoryEdgeLogAppenderFastWithFenceLocked(syncFile bool, fence *memoryEdgeLogFenceToken) (*memoryEdgeLogAppender, error) {
	return m.newMemoryEdgeLogAppenderFastWithFenceContextLocked(context.Background(), syncFile, fence)
}

func (m *memoryStore) newMemoryEdgeLogAppenderFastWithFenceContextLocked(ctx context.Context, syncFile bool, fence *memoryEdgeLogFenceToken) (*memoryEdgeLogAppender, error) {
	if err := requireMemoryEdgeLogFence(m, fence); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	state, err := m.readMemoryEdgeLogStateLocked()
	if err == nil {
		stamp, stampErr := memoryEdgeLogPlatformFileStamp(m.policy.edgePath)
		if stampErr != nil {
			return nil, stampErr
		}
		if memoryEdgeLogStateMatchesStamp(state, stamp) {
			content, restoreErr := memoryEdgeLogHashFromState(state)
			if restoreErr == nil {
				return &memoryEdgeLogAppender{store: m, fence: fence, state: state, content: content, syncFile: syncFile}, nil
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	snapshot, err := m.snapshotMemoryEdgeLogContextLocked(ctx, 0)
	if err != nil {
		return nil, err
	}
	return newMemoryEdgeLogAppenderWithFenceLocked(m, snapshot, syncFile, fence)
}

func (m *memoryStore) loadMemoryEdgeLogStateForRecoveryLocked(raw []byte, stamp memoryEdgeLogFileStamp) (memoryEdgeLogState, error) {
	// Recovery is itself fenced and exact.  The state-write hook models a
	// crash/fault at the acknowledgement boundary; leaving it enabled here
	// would leave a complete durable row with a stale sidecar and make a retry
	// append a duplicate.  Preserve the hook for the caller's original error,
	// but allow the fenced reconciliation commit to finish.
	hook := m.memoryEdgeLogBeforeStateWrite
	m.memoryEdgeLogBeforeStateWrite = nil
	defer func() { m.memoryEdgeLogBeforeStateWrite = hook }()
	return m.loadMemoryEdgeLogStateLocked(raw, stamp)
}

func (m *memoryStore) truncateMemoryEdgeLogLocked(size int64) error {
	if size < 0 || size > memoryEdgeLogMaxRecoveryBytes {
		return fmt.Errorf("%w: truncate size=%d", errMemoryEdgeLogOversized, size)
	}
	file, err := openOwnerOnlyTruncate(m.policy.edgePath, true)
	if err != nil {
		return err
	}
	if err := file.Truncate(size); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return syncOwnerOnlyDirectory(filepath.Dir(m.policy.edgePath))
}

// reconcileAppendFailure classifies the bytes that actually reached the
// descriptor.  A complete row is acknowledged into the projection exactly
// once; a partial suffix is truncated under the same writer fence; anything
// that does not preserve the previous content prefix is treated as an
// out-of-band mutation and loaded exactly.  All branches use the bounded
// descriptor read and repair the sidecar before returning.
func (a *memoryEdgeLogAppender) reconcileAppendFailure(cause error, line []byte, normalized memoryEdgeEntry) error {
	if a == nil || a.store == nil {
		return cause
	}
	a.failed = true
	raw, stamp, readErr := a.store.readMemoryEdgeLogBytesLocked(memoryEdgeLogMaxRecoveryBytes)
	if readErr != nil {
		return errors.Join(cause, fmt.Errorf("reconcile memory edge log after append failure: %w", readErr))
	}
	baseSize := a.state.FileSize
	baseMatches := false
	if baseSize >= 0 && baseSize <= int64(len(raw)) {
		_, prefixDigest, _, hashErr := memoryEdgeLogHashForBytes(raw[:baseSize])
		baseMatches = hashErr == nil && prefixDigest == a.state.ContentDigest
	}
	targetSize := baseSize + int64(len(line))
	if len(line) == 0 && baseMatches && int64(len(raw)) == baseSize {
		reconciled, stateErr := a.store.loadMemoryEdgeLogStateForRecoveryLocked(raw, stamp)
		if stateErr != nil {
			return errors.Join(cause, fmt.Errorf("persist pre-append reconciliation state: %w", stateErr))
		}
		a.state = reconciled
		return cause
	}
	if baseMatches && int64(len(raw)) == targetSize && bytes.Equal(raw[baseSize:], line) {
		reconciled, stateErr := a.store.loadMemoryEdgeLogStateForRecoveryLocked(raw, stamp)
		if stateErr != nil {
			return errors.Join(cause, fmt.Errorf("persist complete append recovery state: %w", stateErr))
		}
		a.state = reconciled
		if projectionErr := a.store.hydrateMemoryEdgeProjectionWithFenceLocked(normalized, a.fence); projectionErr != nil {
			return errors.Join(cause, fmt.Errorf("hydrate complete append recovery: %w", projectionErr))
		}
		return cause
	}
	if baseMatches && int64(len(raw)) > baseSize && int64(len(raw)) < targetSize {
		truncateErr := a.store.truncateMemoryEdgeLogLocked(baseSize)
		if truncateErr != nil {
			return errors.Join(cause, errMemoryEdgeLogPartialWrite, fmt.Errorf("truncate partial memory edge log row: %w", truncateErr))
		}
		raw, stamp, readErr = a.store.readMemoryEdgeLogBytesLocked(memoryEdgeLogMaxRecoveryBytes)
		if readErr != nil {
			return errors.Join(cause, errMemoryEdgeLogPartialWrite, fmt.Errorf("verify truncated memory edge log: %w", readErr))
		}
		reconciled, stateErr := a.store.loadMemoryEdgeLogStateForRecoveryLocked(raw, stamp)
		if stateErr != nil {
			return errors.Join(cause, errMemoryEdgeLogPartialWrite, fmt.Errorf("persist partial append recovery state: %w", stateErr))
		}
		a.state = reconciled
		if projectionErr := a.store.reloadMemoryEdgesFromRawWithFenceLocked(raw, a.fence); projectionErr != nil {
			return errors.Join(cause, errMemoryEdgeLogPartialWrite, fmt.Errorf("reload projection after partial append: %w", projectionErr))
		}
		return errors.Join(cause, errMemoryEdgeLogPartialWrite)
	}
	// The file may have been replaced, truncated, or otherwise mutated after
	// validation.  Never retain the incremental midstate in that case.
	reconciled, stateErr := a.store.loadMemoryEdgeLogStateForRecoveryLocked(raw, stamp)
	if stateErr != nil {
		return errors.Join(cause, fmt.Errorf("persist exact append-race state: %w", stateErr))
	}
	a.state = reconciled
	if projectionErr := a.store.reloadMemoryEdgesFromRawWithFenceLocked(raw, a.fence); projectionErr != nil {
		return errors.Join(cause, fmt.Errorf("reload projection after append race: %w", projectionErr))
	}
	return cause
}

func (a *memoryEdgeLogAppender) append(edge memoryEdgeEntry) (memoryEdgeEntry, memoryEdgeLogState, error) {
	if a == nil || a.store == nil || a.content == nil || a.failed {
		return memoryEdgeEntry{}, memoryEdgeLogState{}, errors.New("memory edge log appender is unavailable")
	}
	if err := requireMemoryEdgeLogFence(a.store, a.fence); err != nil {
		return memoryEdgeEntry{}, memoryEdgeLogState{}, err
	}
	normalized, err := edge.normalized()
	if err != nil {
		return memoryEdgeEntry{}, memoryEdgeLogState{}, err
	}
	payload, err := json.Marshal(normalized)
	if err != nil {
		return memoryEdgeEntry{}, memoryEdgeLogState{}, err
	}
	line := append(payload, '\n')
	refreshBeforeWrite := func(cause error) error {
		return a.reconcileAppendFailure(cause, nil, normalized)
	}
	// The descriptor open is itself the authority for append.  If opening
	// applies the owner-only mode to a legacy file, its ctime may change; a
	// stamp mismatch is therefore reconciled by exact content before proceeding
	// rather than being mistaken for an append race.
	file, err := openOwnerOnlyAppend(a.store.policy.edgePath, true)
	if err != nil {
		return memoryEdgeEntry{}, memoryEdgeLogState{}, err
	}
	closeFile := func() { _ = file.Close() }
	descriptorStamp, err := memoryEdgeLogPlatformFileStampForFile(file)
	if err != nil {
		closeFile()
		return memoryEdgeEntry{}, memoryEdgeLogState{}, refreshBeforeWrite(err)
	}
	pathStamp, err := memoryEdgeLogPlatformFileStamp(a.store.policy.edgePath)
	if err != nil {
		closeFile()
		return memoryEdgeEntry{}, memoryEdgeLogState{}, refreshBeforeWrite(err)
	}
	if descriptorStamp != pathStamp {
		closeFile()
		return memoryEdgeEntry{}, memoryEdgeLogState{}, refreshBeforeWrite(errors.New("memory edge log descriptor changed after appender validation"))
	}
	if !memoryEdgeLogAppendStampMatchesState(a.state, descriptorStamp) {
		closeFile()
		if err := refreshBeforeWrite(errors.New("memory edge log descriptor changed after appender validation")); err != nil {
			return memoryEdgeEntry{}, memoryEdgeLogState{}, err
		}
		return memoryEdgeEntry{}, memoryEdgeLogState{}, errors.New("memory edge log appender requires retry after exact reconciliation")
	}
	if a.store.memoryEdgeLogBeforeAppendWrite != nil {
		a.store.memoryEdgeLogBeforeAppendWrite()
	}
	descriptorStamp, err = memoryEdgeLogPlatformFileStampForFile(file)
	if err != nil {
		closeFile()
		return memoryEdgeEntry{}, memoryEdgeLogState{}, refreshBeforeWrite(err)
	}
	pathStamp, err = memoryEdgeLogPlatformFileStamp(a.store.policy.edgePath)
	if err != nil {
		closeFile()
		return memoryEdgeEntry{}, memoryEdgeLogState{}, refreshBeforeWrite(err)
	}
	if !memoryEdgeLogAppendStampMatchesState(a.state, descriptorStamp) || descriptorStamp != pathStamp {
		closeFile()
		return memoryEdgeEntry{}, memoryEdgeLogState{}, refreshBeforeWrite(errors.New("memory edge log changed after descriptor validation"))
	}
	writeAttempted := true
	writeFn := func(file *os.File, payload []byte) (int, error) { return file.Write(payload) }
	if a.store.memoryEdgeLogWrite != nil {
		writeFn = a.store.memoryEdgeLogWrite
	}
	written, writeErr := writeFn(file, line)
	if writeErr != nil || written != len(line) {
		closeFile()
		if writeErr == nil {
			writeErr = fmt.Errorf("%w: short memory edge log write: %d of %d bytes", errMemoryEdgeLogPartialWrite, written, len(line))
		}
		if writeAttempted && written != len(line) {
			writeErr = errors.Join(writeErr, errMemoryEdgeLogPartialWrite)
		}
		return memoryEdgeEntry{}, memoryEdgeLogState{}, a.reconcileAppendFailure(writeErr, line, normalized)
	}
	rowWritten := true
	if a.syncFile {
		syncFn := func(file *os.File) error { return file.Sync() }
		if a.store.memoryEdgeLogSync != nil {
			syncFn = a.store.memoryEdgeLogSync
		}
		if syncErr := syncFn(file); syncErr != nil {
			closeFile()
			return memoryEdgeEntry{}, memoryEdgeLogState{}, a.reconcileAppendFailure(errors.Join(syncErr, errMemoryEdgeLogDurabilityAmbiguous), line, normalized)
		}
	}
	descriptorStamp, err = memoryEdgeLogPlatformFileStampForFile(file)
	if err != nil {
		closeFile()
		return memoryEdgeEntry{}, memoryEdgeLogState{}, a.reconcileAppendFailure(err, line, normalized)
	}
	pathStamp, err = memoryEdgeLogPlatformFileStamp(a.store.policy.edgePath)
	if err != nil {
		closeFile()
		return memoryEdgeEntry{}, memoryEdgeLogState{}, a.reconcileAppendFailure(err, line, normalized)
	}
	if descriptorStamp != pathStamp {
		closeFile()
		return memoryEdgeEntry{}, memoryEdgeLogState{}, a.reconcileAppendFailure(errors.New("memory edge log descriptor detached during append"), line, normalized)
	}
	if err := file.Close(); err != nil {
		return memoryEdgeEntry{}, memoryEdgeLogState{}, a.reconcileAppendFailure(err, line, normalized)
	}
	if a.syncFile {
		if err := syncOwnerOnlyDirectory(filepath.Dir(a.store.policy.edgePath)); err != nil {
			return memoryEdgeEntry{}, memoryEdgeLogState{}, a.reconcileAppendFailure(errors.Join(err, errMemoryEdgeLogDurabilityAmbiguous), line, normalized)
		}
	}
	stampAfterWrite, err := memoryEdgeLogPlatformFileStamp(a.store.policy.edgePath)
	if err != nil {
		return memoryEdgeEntry{}, memoryEdgeLogState{}, a.reconcileAppendFailure(err, line, normalized)
	}
	if stampAfterWrite.Size != a.state.FileSize+int64(len(line)) {
		return memoryEdgeEntry{}, memoryEdgeLogState{}, a.reconcileAppendFailure(errors.New("memory edge log size changed during append"), line, normalized)
	}
	if !rowWritten {
		return memoryEdgeEntry{}, memoryEdgeLogState{}, a.reconcileAppendFailure(errMemoryEdgeLogPartialWrite, line, normalized)
	}
	_, _ = a.content.Write(line)
	if a.store.memoryEdgeLogObserveIO != nil {
		a.store.memoryEdgeLogObserveIO("append_content_hash", int64(len(line)))
	}
	expectedContentDigest := "sha256:" + hex.EncodeToString(a.content.Sum(nil))
	stampAfterContentHash, err := memoryEdgeLogPlatformFileStamp(a.store.policy.edgePath)
	if err != nil {
		return memoryEdgeEntry{}, memoryEdgeLogState{}, a.reconcileAppendFailure(err, line, normalized)
	}
	if stampAfterContentHash != stampAfterWrite {
		return memoryEdgeEntry{}, memoryEdgeLogState{}, a.reconcileAppendFailure(errors.New("memory edge log changed during append hash"), line, normalized)
	}
	hashState, err := memoryEdgeLogMarshalHashState(a.content)
	if err != nil {
		return memoryEdgeEntry{}, memoryEdgeLogState{}, a.reconcileAppendFailure(err, line, normalized)
	}
	generation := a.state.Generation + 1
	rowDigest := memoryEdgeLogContentDigest(line)
	if a.store.memoryEdgeLogObserveIO != nil {
		a.store.memoryEdgeLogObserveIO("append_row_hash", int64(len(line)))
	}
	stampAfterRowHash, err := memoryEdgeLogPlatformFileStamp(a.store.policy.edgePath)
	if err != nil {
		return memoryEdgeEntry{}, memoryEdgeLogState{}, a.reconcileAppendFailure(err, line, normalized)
	}
	if stampAfterRowHash != stampAfterContentHash {
		return memoryEdgeEntry{}, memoryEdgeLogState{}, a.reconcileAppendFailure(errors.New("memory edge log changed during append state preparation"), line, normalized)
	}
	state := memoryEdgeLogState{
		SchemaID:            memoryEdgeLogStateSchemaID,
		Version:             1,
		Generation:          generation,
		ContentDigest:       expectedContentDigest,
		ParentContentDigest: a.state.ContentDigest,
		ParentFileSize:      a.state.FileSize,
		ParentFileIdentity:  a.state.FileIdentity,
		ContentHashState:    hashState,
		ContentHashedBytes:  a.state.FileSize + int64(len(line)),
	}
	state = memoryEdgeLogStateWithStamp(state, stampAfterRowHash)
	if state.FileSize != state.ContentHashedBytes {
		return memoryEdgeEntry{}, memoryEdgeLogState{}, a.reconcileAppendFailure(errors.New("memory edge log size changed during append"), line, normalized)
	}
	state.Digest = memoryEdgeLogNextDigest(a.state.Digest, generation, "append", rowDigest)
	if err := a.store.writeMemoryEdgeLogStateLocked(state); err != nil {
		return memoryEdgeEntry{}, memoryEdgeLogState{}, a.reconcileAppendFailure(err, line, normalized)
	}
	a.state = state
	return normalized, state, nil
}

func (m *memoryStore) appendMemoryEdgeLogLocked(edge memoryEdgeEntry, syncFile bool) (memoryEdgeEntry, memoryEdgeLogState, error) {
	return memoryEdgeEntry{}, memoryEdgeLogState{}, errMemoryEdgeLogWriterFenceRequired
}

func (m *memoryStore) appendMemoryEdgeLogWithFenceLocked(edge memoryEdgeEntry, syncFile bool, fence *memoryEdgeLogFenceToken) (memoryEdgeEntry, memoryEdgeLogState, error) {
	return m.appendMemoryEdgeLogWithFenceContextLocked(context.Background(), edge, syncFile, fence)
}

func (m *memoryStore) appendMemoryEdgeLogWithFenceContextLocked(ctx context.Context, edge memoryEdgeEntry, syncFile bool, fence *memoryEdgeLogFenceToken) (memoryEdgeEntry, memoryEdgeLogState, error) {
	if err := requireMemoryEdgeLogFence(m, fence); err != nil {
		return memoryEdgeEntry{}, memoryEdgeLogState{}, err
	}
	appender, err := m.newMemoryEdgeLogAppenderFastWithFenceContextLocked(ctx, syncFile, fence)
	if err != nil {
		return memoryEdgeEntry{}, memoryEdgeLogState{}, err
	}
	return appender.append(edge)
}

func (m *memoryStore) appendMemoryEdgeLog(edge memoryEdgeEntry, syncFile bool) (memoryEdgeEntry, memoryEdgeLogState, error) {
	fence, err := m.acquireMemoryEdgeLogFence()
	if err != nil {
		return memoryEdgeEntry{}, memoryEdgeLogState{}, err
	}
	defer fence.release()
	return m.appendMemoryEdgeLogWithFenceLocked(edge, syncFile, fence)
}

func (m *memoryStore) replaceMemoryEdgeLogLocked(raw []byte, operation string) (memoryEdgeLogState, error) {
	return memoryEdgeLogState{}, errMemoryEdgeLogWriterFenceRequired
}

func (m *memoryStore) replaceMemoryEdgeLogWithFenceLocked(raw []byte, operation string, fence *memoryEdgeLogFenceToken) (memoryEdgeLogState, error) {
	if err := requireMemoryEdgeLogFence(m, fence); err != nil {
		return memoryEdgeLogState{}, err
	}
	if int64(len(raw)) > memoryEdgeLogMaxReplacementBytes {
		return memoryEdgeLogState{}, fmt.Errorf("%w: replacement bytes=%d cap=%d", errMemoryEdgeLogOversized, len(raw), memoryEdgeLogMaxReplacementBytes)
	}
	replacement := append([]byte(nil), raw...)
	before, err := m.currentMemoryEdgeLogStateLocked(memoryEdgeLogMaxReplacementBytes)
	if err != nil {
		return memoryEdgeLogState{}, err
	}
	path := m.policy.edgePath
	if err := prepareOwnerOnlyFile(path, true); err != nil {
		return memoryEdgeLogState{}, err
	}
	tmpPath := path + ".tmp-" + sha256Hex(nowUTCISO() + operation)[:16]
	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, ownerOnlyFileMode)
	if err != nil {
		return memoryEdgeLogState{}, err
	}
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	written, writeErr := tmp.Write(replacement)
	if writeErr != nil || written != len(replacement) {
		cleanup()
		if writeErr == nil {
			writeErr = fmt.Errorf("%w: short replacement write: %d of %d bytes", errMemoryEdgeLogPartialWrite, written, len(replacement))
		}
		return memoryEdgeLogState{}, writeErr
	}
	if err := enforceOwnerOnlyDescriptor(tmp, true); err != nil {
		cleanup()
		return memoryEdgeLogState{}, err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return memoryEdgeLogState{}, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return memoryEdgeLogState{}, err
	}
	if m.memoryEdgeLogBeforeReplacementRename != nil {
		m.memoryEdgeLogBeforeReplacementRename()
	}
	if err := replaceOwnerOnlyFile(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return memoryEdgeLogState{}, err
	}
	if m.memoryEdgeLogAfterReplacementRename != nil {
		m.memoryEdgeLogAfterReplacementRename()
	}
	directoryErr := syncOwnerOnlyDirectory(filepath.Dir(path))
	actual, stamp, readErr := m.readMemoryEdgeLogBytesLocked(memoryEdgeLogMaxReplacementBytes)
	if readErr != nil {
		return memoryEdgeLogState{}, memoryEdgeLogReplacementFailure(true, errors.Join(directoryErr, fmt.Errorf("read replacement after rename: %w", readErr)))
	}
	if !bytes.Equal(actual, replacement) {
		return memoryEdgeLogState{}, memoryEdgeLogReplacementFailure(true, errors.Join(directoryErr, errors.New("memory edge log replacement bytes changed after rename")))
	}
	_, contentDigest, contentHashState, err := memoryEdgeLogHashForBytes(actual)
	if err != nil {
		return memoryEdgeLogState{}, memoryEdgeLogReplacementFailure(true, errors.Join(directoryErr, err))
	}
	if m.memoryEdgeLogObserveIO != nil {
		m.memoryEdgeLogObserveIO("replacement_hash", int64(len(actual)))
	}
	state := memoryEdgeLogState{
		SchemaID:            memoryEdgeLogStateSchemaID,
		Version:             1,
		Generation:          before.Generation + 1,
		ContentDigest:       contentDigest,
		ParentContentDigest: before.ContentDigest,
		ParentFileSize:      before.FileSize,
		ParentFileIdentity:  before.FileIdentity,
		ContentHashState:    contentHashState,
		ContentHashedBytes:  int64(len(actual)),
	}
	state = memoryEdgeLogStateWithStamp(state, stamp)
	if state.FileSize != int64(len(actual)) {
		return memoryEdgeLogState{}, memoryEdgeLogReplacementFailure(true, errors.New("memory edge log size changed during replacement"))
	}
	state.Digest = memoryEdgeLogNextDigest(before.Digest, state.Generation, operation, contentDigest)
	if directoryErr != nil {
		_, recoveryErr := m.loadMemoryEdgeLogStateForRecoveryLocked(actual, stamp)
		return memoryEdgeLogState{}, memoryEdgeLogReplacementFailure(true, errors.Join(directoryErr, errMemoryEdgeLogDurabilityAmbiguous, recoveryErr))
	}
	if err := m.writeMemoryEdgeLogStateVerifiedLocked(state, replacement, true); err != nil {
		actualAfter, stampAfter, readAfterErr := m.readMemoryEdgeLogBytesLocked(memoryEdgeLogMaxReplacementBytes)
		if readAfterErr == nil {
			_, recoveryErr := m.loadMemoryEdgeLogStateForRecoveryLocked(actualAfter, stampAfter)
			if reloadErr := m.reloadMemoryEdgesFromRawWithFenceLocked(actualAfter, fence); reloadErr != nil {
				recoveryErr = errors.Join(recoveryErr, reloadErr)
			}
			return memoryEdgeLogState{}, memoryEdgeLogReplacementFailure(true, errors.Join(err, recoveryErr))
		}
		return memoryEdgeLogState{}, memoryEdgeLogReplacementFailure(true, errors.Join(err, readAfterErr))
	}
	finalBytes, finalStamp, finalErr := m.readMemoryEdgeLogBytesLocked(memoryEdgeLogMaxReplacementBytes)
	if finalErr != nil || !bytes.Equal(finalBytes, replacement) || !memoryEdgeLogStateMatchesStamp(state, finalStamp) {
		if finalErr == nil {
			finalErr = errors.New("memory edge log changed after verified replacement state commit")
		}
		if actualAfter, stampAfter, readAfterErr := m.readMemoryEdgeLogBytesLocked(memoryEdgeLogMaxReplacementBytes); readAfterErr == nil {
			_, recoveryErr := m.loadMemoryEdgeLogStateForRecoveryLocked(actualAfter, stampAfter)
			if reloadErr := m.reloadMemoryEdgesFromRawWithFenceLocked(actualAfter, fence); reloadErr != nil {
				recoveryErr = errors.Join(recoveryErr, reloadErr)
			}
			finalErr = errors.Join(finalErr, recoveryErr)
		}
		return memoryEdgeLogState{}, memoryEdgeLogReplacementFailure(true, finalErr)
	}
	return state, nil
}
