package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	memoryCurrentStateTransactionSchemaID = "contextlattice_memory_current_state_transaction.v1"
	memoryCurrentStateTransactionVersion  = 1
	memoryCurrentStateTransactionMarker   = "current-state.transaction.json"
	memoryCurrentStateTransactionMaxCards = 256
	// A single accepted card may approach the aggregate 64 MiB cap. The
	// length-prefixed payload envelope keeps project/digest metadata bounded
	// without JSON/base64 duplication; the 128 MiB artifact cap also covers the
	// worst accepted escaped-project distribution.
	memoryCurrentStateTransactionBatchPayloadMaxBytes = int64(128 * 1024 * 1024)
	memoryCurrentStateTransactionBatchPayloadCount    = (memoryCurrentStateGenerationMaxCards + memoryCurrentStateTransactionMaxCards - 1) / memoryCurrentStateTransactionMaxCards
	memoryCurrentStateTransactionQuarantineDir        = "transaction-quarantine"
	// A recovery scan must admit every bounded batch-payload file, one staged
	// card per 256-card marker, all 64 shard stages, and marker/manifest/session
	// temps. The byte limit is the corresponding worst-case sum, not a small
	// arbitrary allowance that would reject a valid crash image.
	memoryCurrentStateTransactionMaxOrphans     = memoryCurrentStateTransactionBatchPayloadCount + memoryCurrentStateTransactionMaxCards + memoryCurrentStateShardCount + 16
	memoryCurrentStateTransactionMaxOrphanBytes = 2*memoryCurrentStateGenerationMaxCardBytes + int64(memoryCurrentStateShardCount)*memoryEdgeLogMaxRecoveryBytes + 2*memoryEdgeLogMaxRecoveryBytes
	memoryCurrentStateTransactionBatchMaxBytes  = int64(128 * 1024 * 1024)
	// Startup must bound directory enumeration even for ordinary names that
	// are skipped before orphan validation. At the largest accepted commit, the
	// fixed root can simultaneously contain 64 final shards, 64 shard stages,
	// 391 batch payloads, and seven singleton entries: the final manifest,
	// generation-card directory, quarantine directory, staged manifest, batch
	// session, transaction marker, and the one temporary created by the
	// serialized atomic replacement primitive. Unix rename and Windows
	// MoveFileEx do not create backup names, so this is the exact platform-
	// independent peak rather than an arbitrary allowance.
	memoryCurrentStateTransactionMaxFixedRootEntries = 2*memoryCurrentStateShardCount + memoryCurrentStateTransactionBatchPayloadCount + 7
	memoryCurrentStateTransactionMaxCardRootEntries  = memoryCurrentStateGenerationMaxCards + memoryCurrentStateTransactionMaxCards + 16
)

var errMemoryCurrentStateTransactionCommitted = errors.New("current-state transaction is durably staged")

type memoryCurrentStateTransactionShard struct {
	Shard     int    `json:"shard"`
	StagePath string `json:"stage_path"`
	FinalPath string `json:"final_path"`
	OldDigest string `json:"old_digest,omitempty"`
	NewDigest string `json:"new_digest"`
}

type memoryCurrentStateTransactionCard struct {
	Project   string `json:"project"`
	StagePath string `json:"stage_path"`
	FinalPath string `json:"final_path"`
	OldDigest string `json:"old_digest,omitempty"`
	NewDigest string `json:"new_digest"`
}

type memoryCurrentStateTransaction struct {
	SchemaID          string                               `json:"schema_id"`
	Version           int                                  `json:"version"`
	TransactionID     string                               `json:"transaction_id"`
	State             string                               `json:"state"`
	Project           string                               `json:"project,omitempty"`
	Generation        uint64                               `json:"generation,omitempty"`
	Shards            []memoryCurrentStateTransactionShard `json:"shards"`
	Cards             []memoryCurrentStateTransactionCard  `json:"cards,omitempty"`
	ExactStagePath    string                               `json:"exact_state_stage_path,omitempty"`
	ExactFinalPath    string                               `json:"exact_state_final_path,omitempty"`
	OldExactDigest    string                               `json:"old_exact_state_digest,omitempty"`
	NewExactDigest    string                               `json:"new_exact_state_digest,omitempty"`
	StageManifestPath string                               `json:"stage_manifest_path"`
	FinalManifestPath string                               `json:"final_manifest_path"`
	OldManifestDigest string                               `json:"old_manifest_digest,omitempty"`
	NewManifestDigest string                               `json:"new_manifest_digest"`
	BatchSessionPath  string                               `json:"batch_session_path,omitempty"`
	BatchIndex        int                                  `json:"batch_index,omitempty"`
	BatchCount        int                                  `json:"batch_count,omitempty"`
	SessionDigest     string                               `json:"session_digest,omitempty"`
}

type memoryCurrentStateTransactionBatchCard struct {
	Project   string `json:"project"`
	Payload   []byte `json:"payload"`
	OldDigest string `json:"old_digest,omitempty"`
	NewDigest string `json:"new_digest"`
}

type memoryCurrentStateTransactionBatchSession struct {
	SchemaID      string `json:"schema_id"`
	Version       int    `json:"version"`
	TransactionID string `json:"transaction_id"`
	State         string `json:"state"`
	NextBatch     int    `json:"next_batch"`
	TotalBatches  int    `json:"total_batches"`
	// Cards is retained for restart compatibility with the v1 session emitted
	// by the previous implementation. New sessions use bounded payload files
	// and keep only their exact paths/count here, so session JSON does not grow
	// with card payload base64 or project metadata.
	Cards             []memoryCurrentStateTransactionBatchCard `json:"cards,omitempty"`
	CardCount         int                                      `json:"card_count,omitempty"`
	BatchPayloadPaths []string                                 `json:"batch_payload_paths,omitempty"`
	Shards            []memoryCurrentStateTransactionShard     `json:"shards"`
	StageManifestPath string                                   `json:"stage_manifest_path"`
	FinalManifestPath string                                   `json:"final_manifest_path"`
	OldManifestDigest string                                   `json:"old_manifest_digest,omitempty"`
	NewManifestDigest string                                   `json:"new_manifest_digest"`
}

const memoryCurrentStateTransactionBatchPayloadMagic = "contextlattice-memory-current-state-batch-payload.v1\x00"

func encodeMemoryCurrentStateTransactionBatchPayload(cards []memoryCurrentStateTransactionBatchCard) ([]byte, error) {
	if len(cards) == 0 || len(cards) > memoryCurrentStateTransactionMaxCards {
		return nil, errors.New("current-state transaction batch payload card count is invalid")
	}
	raw := make([]byte, 0, len(memoryCurrentStateTransactionBatchPayloadMagic)+len(cards)*128)
	raw = append(raw, memoryCurrentStateTransactionBatchPayloadMagic...)
	appendU32 := func(value int) error {
		if value < 0 || uint64(value) > uint64(^uint32(0)) {
			return errors.New("current-state transaction batch payload field is too large")
		}
		var encoded [4]byte
		binary.BigEndian.PutUint32(encoded[:], uint32(value))
		raw = append(raw, encoded[:]...)
		return nil
	}
	appendBytes := func(value []byte) error {
		if err := appendU32(len(value)); err != nil {
			return err
		}
		raw = append(raw, value...)
		return nil
	}
	for _, card := range cards {
		if err := appendBytes([]byte(card.OldDigest)); err != nil {
			return nil, err
		}
		if uint64(len(card.Payload)) > uint64(^uint32(0)) {
			return nil, errors.New("current-state transaction batch payload card bytes are too large")
		}
		if err := appendBytes(card.Payload); err != nil {
			return nil, err
		}
	}
	if int64(len(raw)) > memoryCurrentStateTransactionBatchPayloadMaxBytes {
		return nil, fmt.Errorf("current-state transaction batch payload exceeds byte cap %d", memoryCurrentStateTransactionBatchPayloadMaxBytes)
	}
	return raw, nil
}

func decodeMemoryCurrentStateTransactionBatchPayload(raw []byte) ([]memoryCurrentStateTransactionBatchCard, error) {
	if !bytes.HasPrefix(raw, []byte(memoryCurrentStateTransactionBatchPayloadMagic)) {
		// A short-lived development build emitted JSON refs. Accepting that
		// shape costs no unbounded behavior and keeps a crash image readable
		// while all new sessions use the binary length-prefixed form.
		var cards []memoryCurrentStateTransactionBatchCard
		if err := json.Unmarshal(raw, &cards); err != nil {
			return nil, err
		}
		return cards, nil
	}
	offset := len(memoryCurrentStateTransactionBatchPayloadMagic)
	readBytes := func() ([]byte, error) {
		if len(raw)-offset < 4 {
			return nil, errors.New("current-state transaction batch payload is truncated")
		}
		length := int(binary.BigEndian.Uint32(raw[offset : offset+4]))
		offset += 4
		if length < 0 || length > len(raw)-offset {
			return nil, errors.New("current-state transaction batch payload length is invalid")
		}
		value := append([]byte(nil), raw[offset:offset+length]...)
		offset += length
		return value, nil
	}
	cards := make([]memoryCurrentStateTransactionBatchCard, 0, memoryCurrentStateTransactionMaxCards)
	for offset < len(raw) {
		if len(cards) == memoryCurrentStateTransactionMaxCards {
			return nil, errors.New("current-state transaction batch payload has too many cards")
		}
		oldDigest, err := readBytes()
		if err != nil {
			return nil, err
		}
		payload, err := readBytes()
		if err != nil {
			return nil, err
		}
		var card memoryCurrentStateGenerationCard
		if err := json.Unmarshal(payload, &card); err != nil {
			return nil, fmt.Errorf("decode current-state transaction batch card payload: %w", err)
		}
		project := normalizeCurrentKeyIndexProject(card.Project)
		if project == "" || project != card.Project {
			return nil, errors.New("current-state transaction batch card project is not canonical")
		}
		cards = append(cards, memoryCurrentStateTransactionBatchCard{Project: project, OldDigest: string(oldDigest), NewDigest: memoryCurrentStateTransactionDigest(payload), Payload: payload})
	}
	if len(cards) == 0 {
		return nil, errors.New("current-state transaction batch payload has no cards")
	}
	return cards, nil
}

func (m *memoryStore) currentStateTransactionExactRootPath() string {
	if m == nil {
		return ""
	}
	return filepath.Dir(filepath.Clean(m.policy.exactStateIndexPath))
}

func (m *memoryStore) currentStateTransactionExactRelativePath(path string) (string, error) {
	if m == nil {
		return "", errors.New("memory store unavailable")
	}
	root, err := filepath.Abs(filepath.Clean(m.currentStateTransactionExactRootPath()))
	if err != nil {
		return "", err
	}
	clean, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, clean)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", errors.New("exact-state transaction path escapes its root")
	}
	return rel, nil
}

func (m *memoryStore) currentStateTransactionExactAbsolutePath(relative string) (string, error) {
	if m == nil {
		return "", errors.New("memory store unavailable")
	}
	trimmed := strings.TrimSpace(relative)
	if trimmed == "" || filepath.IsAbs(trimmed) {
		return "", errors.New("exact-state transaction path must be relative")
	}
	clean := filepath.Clean(trimmed)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", errors.New("exact-state transaction path escapes its root")
	}
	path := filepath.Join(m.currentStateTransactionExactRootPath(), clean)
	validated, err := m.currentStateTransactionExactRelativePath(path)
	if err != nil || validated != clean {
		return "", errors.New("exact-state transaction path is not root-bound")
	}
	return path, nil
}

func (m *memoryStore) currentStateTransactionPath() string {
	return filepath.Join(m.currentStateRootPath(), memoryCurrentStateTransactionMarker)
}

func memoryCurrentStateTransactionBatchSessionPath(transactionID string) string {
	return ".txn-" + transactionID + "-batch.json"
}

func memoryCurrentStateTransactionBatchPayloadPath(transactionID string, batchIndex int) string {
	return ".txn-" + transactionID + "-batch-" + fmt.Sprintf("%03d.json", batchIndex)
}

func memoryCurrentStateTransactionBatchSessionDigest(raw []byte) string {
	return memoryCurrentStateTransactionDigest(raw)
}

func validateMemoryCurrentStateGenerationCardPayload(project string, payload []byte) error {
	if len(payload) == 0 || int64(len(payload)) > memoryEdgeLogMaxRecoveryBytes {
		return errors.New("current-state generation card payload is outside its bound")
	}
	var card memoryCurrentStateGenerationCard
	if err := json.Unmarshal(payload, &card); err != nil {
		return fmt.Errorf("decode current-state generation card payload: %w", err)
	}
	project = normalizeCurrentKeyIndexProject(project)
	if card.SchemaID != memoryCurrentStateGenerationCardSchemaID || card.Version != memoryCurrentStateGenerationCardsVersion || normalizeCurrentKeyIndexProject(card.Project) != project || project == "" || card.Record.KeyGeneration != card.Record.TopicGeneration || !memoryCurrentStateGenerationDigestValid(card.Record.StateDigest) {
		return errors.New("current-state generation card payload contract is invalid")
	}
	return nil
}

func validateMemoryCurrentStateTransactionBatchSession(session memoryCurrentStateTransactionBatchSession) error {
	if session.SchemaID != memoryCurrentStateTransactionSchemaID || session.Version != memoryCurrentStateTransactionVersion || session.State != "prepared" || len(session.TransactionID) != 24 || !memoryCurrentStateTransactionHex(session.TransactionID) || session.NextBatch < 0 || session.TotalBatches < 2 || session.NextBatch > session.TotalBatches {
		return errors.New("current-state transaction batch session contract is invalid")
	}
	legacyCards := len(session.Cards) > 0
	refCards := session.CardCount > 0 || len(session.BatchPayloadPaths) > 0
	if legacyCards && refCards {
		return errors.New("current-state transaction batch session mixes legacy cards and payload references")
	}
	if !legacyCards && !refCards {
		return errors.New("current-state transaction batch session has no cards")
	}
	if legacyCards {
		if len(session.Cards) > memoryCurrentStateGenerationMaxCards || session.TotalBatches != (len(session.Cards)+memoryCurrentStateTransactionMaxCards-1)/memoryCurrentStateTransactionMaxCards {
			return errors.New("current-state transaction batch session card count is invalid")
		}
	} else {
		if session.CardCount > memoryCurrentStateGenerationMaxCards || session.TotalBatches != (session.CardCount+memoryCurrentStateTransactionMaxCards-1)/memoryCurrentStateTransactionMaxCards || len(session.BatchPayloadPaths) != session.TotalBatches {
			return errors.New("current-state transaction batch payload reference count is invalid")
		}
		seenPayloads := make(map[string]struct{}, len(session.BatchPayloadPaths))
		for index, path := range session.BatchPayloadPaths {
			expected := memoryCurrentStateTransactionBatchPayloadPath(session.TransactionID, index)
			if path != expected {
				return errors.New("current-state transaction batch payload path is foreign")
			}
			if _, exists := seenPayloads[path]; exists {
				return errors.New("current-state transaction batch payload path repeats")
			}
			seenPayloads[path] = struct{}{}
		}
	}
	if session.StageManifestPath != ".txn-"+session.TransactionID+"-generations.json" || session.FinalManifestPath != "generations.json" || !memoryCurrentStateGenerationDigestValid(session.NewManifestDigest) || (strings.TrimSpace(session.OldManifestDigest) != "" && !memoryCurrentStateGenerationDigestValid(session.OldManifestDigest)) {
		return errors.New("current-state transaction batch manifest contract is invalid")
	}
	if !legacyCards {
		if len(session.Shards) > memoryCurrentStateShardCount {
			return errors.New("current-state transaction batch shard count is invalid")
		}
		seenShards := map[int]struct{}{}
		for _, shard := range session.Shards {
			if shard.Shard < 0 || shard.Shard >= memoryCurrentStateShardCount {
				return errors.New("current-state transaction batch shard is invalid")
			}
			if _, exists := seenShards[shard.Shard]; exists {
				return errors.New("current-state transaction repeats a shard")
			}
			seenShards[shard.Shard] = struct{}{}
			if shard.StagePath != ".txn-"+session.TransactionID+"-"+fmt.Sprintf("%02x.json", shard.Shard) || shard.FinalPath != fmt.Sprintf("%02x.json", shard.Shard) || !memoryCurrentStateGenerationDigestValid(shard.NewDigest) || (strings.TrimSpace(shard.OldDigest) != "" && !memoryCurrentStateGenerationDigestValid(shard.OldDigest)) {
				return errors.New("current-state transaction batch shard paths are foreign")
			}
		}
		return nil
	}
	seenProjects := make(map[string]struct{}, len(session.Cards))
	var totalCardBytes int64
	for _, card := range session.Cards {
		project := normalizeCurrentKeyIndexProject(card.Project)
		if project == "" || project != card.Project {
			return errors.New("current-state transaction batch project is not canonical")
		}
		if _, exists := seenProjects[project]; exists {
			return errors.New("current-state transaction batch repeats a project")
		}
		seenProjects[project] = struct{}{}
		if !memoryCurrentStateGenerationDigestValid(card.NewDigest) || (strings.TrimSpace(card.OldDigest) != "" && !memoryCurrentStateGenerationDigestValid(card.OldDigest)) || memoryCurrentStateTransactionDigest(card.Payload) != card.NewDigest {
			return errors.New("current-state transaction batch card digest is invalid")
		}
		if err := validateMemoryCurrentStateGenerationCardPayload(project, card.Payload); err != nil {
			return err
		}
		totalCardBytes += int64(len(card.Payload))
		if totalCardBytes > memoryCurrentStateGenerationMaxCardBytes {
			return fmt.Errorf("current-state transaction batch cards exceed byte cap %d", memoryCurrentStateGenerationMaxCardBytes)
		}
	}
	if len(session.Shards) > memoryCurrentStateShardCount {
		return errors.New("current-state transaction batch shard count is invalid")
	}
	seenShards := map[int]struct{}{}
	for _, shard := range session.Shards {
		if shard.Shard < 0 || shard.Shard >= memoryCurrentStateShardCount {
			return errors.New("current-state transaction batch shard is invalid")
		}
		if _, exists := seenShards[shard.Shard]; exists {
			return errors.New("current-state transaction batch repeats a shard")
		}
		seenShards[shard.Shard] = struct{}{}
		if shard.StagePath != ".txn-"+session.TransactionID+"-"+fmt.Sprintf("%02x.json", shard.Shard) || shard.FinalPath != fmt.Sprintf("%02x.json", shard.Shard) || !memoryCurrentStateGenerationDigestValid(shard.NewDigest) || (strings.TrimSpace(shard.OldDigest) != "" && !memoryCurrentStateGenerationDigestValid(shard.OldDigest)) {
			return errors.New("current-state transaction batch shard paths are foreign")
		}
	}
	return nil
}

func memoryCurrentStateTransactionDigest(raw []byte) string {
	return "sha256:" + sha256Hex(string(raw))
}

func (m *memoryStore) currentStateTransactionFault(event string) error {
	if m == nil {
		return nil
	}
	if m.memoryCurrentStateTransactionFault != nil {
		if err := m.memoryCurrentStateTransactionFault(event); err != nil {
			return err
		}
	}
	return nil
}

func (m *memoryStore) currentStateTransactionRelativePath(path string) (string, error) {
	if m == nil {
		return "", errors.New("memory store unavailable")
	}
	root, err := filepath.Abs(filepath.Clean(m.currentStateRootPath()))
	if err != nil {
		return "", err
	}
	clean, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, clean)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", errors.New("current-state transaction path escapes its root")
	}
	return rel, nil
}

func (m *memoryStore) currentStateTransactionAbsolutePath(relative string) (string, error) {
	if m == nil {
		return "", errors.New("memory store unavailable")
	}
	trimmed := strings.TrimSpace(relative)
	if trimmed == "" || filepath.IsAbs(trimmed) {
		return "", errors.New("current-state transaction path must be relative")
	}
	clean := filepath.Clean(trimmed)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", errors.New("current-state transaction path escapes its root")
	}
	path := filepath.Join(m.currentStateRootPath(), clean)
	validated, err := m.currentStateTransactionRelativePath(path)
	if err != nil || validated != clean {
		return "", errors.New("current-state transaction path is not root-bound")
	}
	return path, nil
}

func memoryCurrentStateTransactionReadDigest(path string) (string, error) {
	return memoryCurrentStateTransactionReadDigestBounded(path, memoryEdgeLogMaxRecoveryBytes)
}

func memoryCurrentStateTransactionReadDigestBounded(path string, maxBytes int64) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", errors.New("current-state transaction artifact is not a regular file")
	}
	raw, err := readOwnerOnlyBoundedFile(path, maxBytes)
	if err != nil {
		return "", err
	}
	return memoryCurrentStateTransactionDigest(raw), nil
}

func (m *memoryStore) currentStateTransactionValidateArtifact(path string, expectedDigest string, allowMissing bool) ([]byte, error) {
	return m.currentStateTransactionValidateArtifactBounded(path, expectedDigest, allowMissing, memoryEdgeLogMaxRecoveryBytes)
}

func (m *memoryStore) currentStateTransactionValidateArtifactBounded(path string, expectedDigest string, allowMissing bool, maxBytes int64) ([]byte, error) {
	if m == nil {
		return nil, errors.New("memory store unavailable")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && allowMissing {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("current-state transaction artifact is not a regular file")
	}
	raw, err := readOwnerOnlyBoundedFile(path, maxBytes)
	if err != nil {
		return nil, err
	}
	if expected := strings.TrimSpace(expectedDigest); expected != "" && memoryCurrentStateTransactionDigest(raw) != expected {
		return nil, errors.New("current-state transaction artifact digest mismatch")
	}
	return raw, nil
}

func (m *memoryStore) currentStateTransactionCleanupStaged(txn memoryCurrentStateTransaction) {
	if m == nil {
		return
	}
	for _, shard := range txn.Shards {
		if path, err := m.currentStateTransactionAbsolutePath(shard.StagePath); err == nil {
			_ = os.Remove(path)
		}
	}
	for _, card := range txn.Cards {
		if path, err := m.currentStateTransactionAbsolutePath(card.StagePath); err == nil {
			_ = os.Remove(path)
		}
	}
	if path, err := m.currentStateTransactionExactAbsolutePath(txn.ExactStagePath); err == nil && strings.TrimSpace(txn.ExactStagePath) != "" {
		_ = os.Remove(path)
	}
	if path, err := m.currentStateTransactionAbsolutePath(txn.StageManifestPath); err == nil {
		_ = os.Remove(path)
	}
}

func (m *memoryStore) currentStateTransactionMarker(txn memoryCurrentStateTransaction) error {
	batchMarker := strings.TrimSpace(txn.BatchSessionPath) != ""
	if txn.SchemaID != memoryCurrentStateTransactionSchemaID || txn.Version != memoryCurrentStateTransactionVersion || len(txn.TransactionID) != 24 || !memoryCurrentStateTransactionHex(txn.TransactionID) || len(txn.Shards) > memoryCurrentStateShardCount || len(txn.Cards) > memoryCurrentStateTransactionMaxCards {
		return errors.New("current-state transaction marker contract is invalid")
	}
	if batchMarker {
		if (txn.State != "batch" && txn.State != "prepared") || txn.BatchIndex < 0 || txn.BatchCount < 2 || txn.BatchIndex >= txn.BatchCount || txn.BatchSessionPath != memoryCurrentStateTransactionBatchSessionPath(txn.TransactionID) || !memoryCurrentStateGenerationDigestValid(txn.SessionDigest) {
			return errors.New("current-state transaction batch marker contract is invalid")
		}
		if _, err := m.currentStateTransactionAbsolutePath(txn.BatchSessionPath); err != nil {
			return err
		}
		if txn.BatchIndex < txn.BatchCount-1 {
			if txn.State != "batch" || len(txn.Shards) != 0 || strings.TrimSpace(txn.StageManifestPath) != "" || strings.TrimSpace(txn.FinalManifestPath) != "" || strings.TrimSpace(txn.NewManifestDigest) != "" {
				return errors.New("current-state transaction intermediate batch marker is invalid")
			}
		} else if txn.State != "prepared" || strings.TrimSpace(txn.StageManifestPath) == "" || strings.TrimSpace(txn.FinalManifestPath) == "" || !memoryCurrentStateGenerationDigestValid(txn.NewManifestDigest) {
			return errors.New("current-state transaction final batch marker is invalid")
		}
	} else if txn.State != "prepared" || strings.TrimSpace(txn.StageManifestPath) == "" || strings.TrimSpace(txn.FinalManifestPath) == "" || !memoryCurrentStateGenerationDigestValid(txn.NewManifestDigest) || (strings.TrimSpace(txn.OldManifestDigest) != "" && !memoryCurrentStateGenerationDigestValid(txn.OldManifestDigest)) {
		return errors.New("current-state transaction marker contract is invalid")
	}
	if !batchMarker || txn.BatchIndex == txn.BatchCount-1 {
		expectedStageManifest := ".txn-" + txn.TransactionID + "-generations.json"
		if filepath.Clean(txn.FinalManifestPath) != "generations.json" || filepath.Clean(txn.StageManifestPath) != expectedStageManifest || filepath.Dir(filepath.Clean(txn.StageManifestPath)) != "." {
			return errors.New("current-state transaction manifest paths are foreign")
		}
		if _, err := m.currentStateTransactionAbsolutePath(txn.StageManifestPath); err != nil {
			return err
		}
		if _, err := m.currentStateTransactionAbsolutePath(txn.FinalManifestPath); err != nil {
			return err
		}
	}
	if (strings.TrimSpace(txn.ExactStagePath) == "") != (strings.TrimSpace(txn.ExactFinalPath) == "") {
		return errors.New("current-state transaction exact-state paths are incomplete")
	}
	if strings.TrimSpace(txn.ExactFinalPath) != "" {
		exactFinal := filepath.Clean(txn.ExactFinalPath)
		exactStage := filepath.Clean(txn.ExactStagePath)
		expectedExactStage := ".txn-" + txn.TransactionID + "-exact-state-index-stage.json"
		if exactFinal != filepath.Base(m.policy.exactStateIndexPath) || filepath.Dir(exactFinal) != "." || filepath.Dir(exactStage) != "." || exactStage != expectedExactStage || !memoryCurrentStateGenerationDigestValid(txn.NewExactDigest) || (strings.TrimSpace(txn.OldExactDigest) != "" && !memoryCurrentStateGenerationDigestValid(txn.OldExactDigest)) {
			return errors.New("current-state transaction exact-state paths are foreign")
		}
		if _, err := m.currentStateTransactionExactAbsolutePath(exactStage); err != nil {
			return err
		}
		if _, err := m.currentStateTransactionExactAbsolutePath(exactFinal); err != nil {
			return err
		}
	}
	seen := map[int]struct{}{}
	for _, shard := range txn.Shards {
		if shard.Shard < 0 || shard.Shard >= memoryCurrentStateShardCount {
			return errors.New("current-state transaction shard is invalid")
		}
		if _, exists := seen[shard.Shard]; exists {
			return errors.New("current-state transaction repeats a shard")
		}
		seen[shard.Shard] = struct{}{}
		expectedStage := ".txn-" + txn.TransactionID + "-" + fmt.Sprintf("%02x.json", shard.Shard)
		if filepath.Clean(shard.FinalPath) != fmt.Sprintf("%02x.json", shard.Shard) || filepath.Dir(filepath.Clean(shard.FinalPath)) != "." || filepath.Clean(shard.StagePath) != expectedStage || filepath.Dir(filepath.Clean(shard.StagePath)) != "." {
			return errors.New("current-state transaction shard paths are foreign")
		}
		if _, err := m.currentStateTransactionAbsolutePath(shard.StagePath); err != nil {
			return err
		}
		if _, err := m.currentStateTransactionAbsolutePath(shard.FinalPath); err != nil {
			return err
		}
		if !memoryCurrentStateGenerationDigestValid(shard.NewDigest) {
			return errors.New("current-state transaction shard digest is invalid")
		}
		if strings.TrimSpace(shard.OldDigest) != "" && !memoryCurrentStateGenerationDigestValid(shard.OldDigest) {
			return errors.New("current-state transaction old shard digest is invalid")
		}
	}
	seenProjects := map[string]struct{}{}
	for _, card := range txn.Cards {
		project := normalizeCurrentKeyIndexProject(card.Project)
		if project == "" {
			return errors.New("current-state transaction project card is invalid")
		}
		if _, exists := seenProjects[project]; exists {
			return errors.New("current-state transaction repeats a project card")
		}
		seenProjects[project] = struct{}{}
		cardName := memoryCurrentStateGenerationCardName(project)
		finalPath := filepath.ToSlash(filepath.Clean(card.FinalPath))
		stagePath := filepath.ToSlash(filepath.Clean(card.StagePath))
		stageName := filepath.Base(filepath.FromSlash(stagePath))
		if finalPath != filepath.ToSlash(filepath.Join(memoryCurrentStateGenerationCardsDir, cardName)) || filepath.Dir(filepath.FromSlash(finalPath)) != memoryCurrentStateGenerationCardsDir || filepath.Dir(filepath.FromSlash(stagePath)) != memoryCurrentStateGenerationCardsDir || stageName != ".txn-"+txn.TransactionID+"-card-"+cardName || !memoryCurrentStateGenerationDigestValid(card.NewDigest) || (strings.TrimSpace(card.OldDigest) != "" && !memoryCurrentStateGenerationDigestValid(card.OldDigest)) {
			return errors.New("current-state transaction project card paths are foreign")
		}
		if _, err := m.currentStateTransactionAbsolutePath(filepath.FromSlash(stagePath)); err != nil {
			return err
		}
		if _, err := m.currentStateTransactionAbsolutePath(filepath.FromSlash(finalPath)); err != nil {
			return err
		}
	}
	return nil
}

func (m *memoryStore) currentStateTransactionRollForwardLocked(txn memoryCurrentStateTransaction) error {
	if err := m.currentStateTransactionMarker(txn); err != nil {
		return err
	}
	if strings.TrimSpace(txn.BatchSessionPath) != "" && txn.State == "batch" {
		if err := m.currentStateTransactionRollForwardCardsLocked(txn); err != nil {
			return err
		}
		return m.currentStateTransactionFinishBatchLocked(txn)
	}
	for _, shard := range txn.Shards {
		stagePath, _ := m.currentStateTransactionAbsolutePath(shard.StagePath)
		finalPath, _ := m.currentStateTransactionAbsolutePath(shard.FinalPath)
		finalDigest, err := memoryCurrentStateTransactionReadDigest(finalPath)
		if err != nil {
			return fmt.Errorf("inspect current-state shard %d during recovery: %w", shard.Shard, err)
		}
		if finalDigest == shard.NewDigest {
			if _, err := m.currentStateTransactionValidateArtifact(stagePath, shard.NewDigest, true); err != nil {
				return fmt.Errorf("validate completed current-state shard %d: %w", shard.Shard, err)
			}
			if _, err := os.Lstat(stagePath); err == nil {
				if err := os.Remove(stagePath); err != nil {
					return fmt.Errorf("remove completed current-state shard %d stage: %w", shard.Shard, err)
				}
			}
			continue
		}
		if finalDigest != shard.OldDigest {
			return fmt.Errorf("current-state shard %d is neither old nor new", shard.Shard)
		}
		if _, err := m.currentStateTransactionValidateArtifact(stagePath, shard.NewDigest, false); err != nil {
			return fmt.Errorf("validate staged current-state shard %d: %w", shard.Shard, err)
		}
		if err := replaceOwnerOnlyFile(stagePath, finalPath); err != nil {
			return fmt.Errorf("roll forward current-state shard %d: %w", shard.Shard, err)
		}
		if err := syncOwnerOnlyDirectory(m.currentStateRootPath()); err != nil {
			return fmt.Errorf("sync current-state shard %d recovery rename: %w", shard.Shard, err)
		}
		if err := m.currentStateTransactionFault("after_shard_rename"); err != nil {
			return fmt.Errorf("%w: current-state transaction after shard %d rename: %v", errMemoryCurrentStateTransactionCommitted, shard.Shard, err)
		}
	}
	if err := m.currentStateTransactionRollForwardCardsLocked(txn); err != nil {
		return err
	}
	if err := m.currentStateTransactionRollForwardExactStateLocked(txn); err != nil {
		return err
	}
	manifestStage, _ := m.currentStateTransactionAbsolutePath(txn.StageManifestPath)
	manifestFinal, _ := m.currentStateTransactionAbsolutePath(txn.FinalManifestPath)
	manifestDigest, err := memoryCurrentStateTransactionReadDigest(manifestFinal)
	if err != nil {
		return fmt.Errorf("inspect current-state generation manifest during recovery: %w", err)
	}
	if manifestDigest == txn.NewManifestDigest {
		if _, err := m.currentStateTransactionValidateArtifact(manifestStage, txn.NewManifestDigest, true); err != nil {
			return fmt.Errorf("validate completed current-state generation manifest: %w", err)
		}
		if _, err := os.Lstat(manifestStage); err == nil {
			if err := os.Remove(manifestStage); err != nil {
				return fmt.Errorf("remove completed current-state generation manifest stage: %w", err)
			}
			if err := syncOwnerOnlyDirectory(m.currentStateRootPath()); err != nil {
				return fmt.Errorf("sync completed current-state generation manifest stage removal: %w", err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else {
		if manifestDigest != txn.OldManifestDigest {
			return errors.New("current-state generation manifest is neither old nor new")
		}
		if _, err := m.currentStateTransactionValidateArtifact(manifestStage, txn.NewManifestDigest, false); err != nil {
			return fmt.Errorf("validate staged current-state generation manifest: %w", err)
		}
		if err := replaceOwnerOnlyFile(manifestStage, manifestFinal); err != nil {
			return fmt.Errorf("roll forward current-state generation manifest: %w", err)
		}
		if err := syncOwnerOnlyDirectory(m.currentStateRootPath()); err != nil {
			return fmt.Errorf("sync current-state generation manifest recovery rename: %w", err)
		}
		if err := m.currentStateTransactionFault("after_manifest_rename"); err != nil {
			return fmt.Errorf("%w: current-state transaction after manifest rename: %v", errMemoryCurrentStateTransactionCommitted, err)
		}
	}
	if _, err := m.currentStateTransactionValidateArtifact(manifestFinal, txn.NewManifestDigest, false); err != nil {
		return fmt.Errorf("verify rolled-forward current-state generation manifest: %w", err)
	}
	if strings.TrimSpace(txn.BatchSessionPath) != "" {
		return m.currentStateTransactionFinishBatchLocked(txn)
	}
	if err := os.Remove(m.currentStateTransactionPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove recovered current-state transaction marker: %w", err)
	}
	if err := syncOwnerOnlyDirectory(m.currentStateRootPath()); err != nil {
		return fmt.Errorf("sync recovered current-state transaction marker removal: %w", err)
	}
	return nil
}

func (m *memoryStore) currentStateTransactionReadBatchSessionLocked(path string) (memoryCurrentStateTransactionBatchSession, []byte, error) {
	if m == nil {
		return memoryCurrentStateTransactionBatchSession{}, nil, errors.New("memory store unavailable")
	}
	if filepath.Base(path) != path || memoryCurrentStateTransactionOrphanKind(path) != "batch" {
		return memoryCurrentStateTransactionBatchSession{}, nil, errors.New("current-state transaction batch session path is foreign")
	}
	absolute, err := m.currentStateTransactionAbsolutePath(path)
	if err != nil {
		return memoryCurrentStateTransactionBatchSession{}, nil, err
	}
	raw, err := readOwnerOnlyBoundedFile(absolute, memoryCurrentStateTransactionBatchMaxBytes)
	if err != nil {
		return memoryCurrentStateTransactionBatchSession{}, nil, err
	}
	var session memoryCurrentStateTransactionBatchSession
	if err := json.Unmarshal(raw, &session); err != nil {
		return memoryCurrentStateTransactionBatchSession{}, nil, err
	}
	if err := validateMemoryCurrentStateTransactionBatchSession(session); err != nil {
		return memoryCurrentStateTransactionBatchSession{}, nil, err
	}
	if session.TransactionID+"-batch.json" != strings.TrimPrefix(path, ".txn-") {
		return memoryCurrentStateTransactionBatchSession{}, nil, errors.New("current-state transaction batch session identity mismatch")
	}
	return session, raw, nil
}

func (m *memoryStore) currentStateTransactionReadBatchPayloadLocked(session memoryCurrentStateTransactionBatchSession, batchIndex int) ([]memoryCurrentStateTransactionBatchCard, error) {
	if len(session.Cards) > 0 {
		start := batchIndex * memoryCurrentStateTransactionMaxCards
		end := start + memoryCurrentStateTransactionMaxCards
		if end > len(session.Cards) {
			end = len(session.Cards)
		}
		if batchIndex < 0 || start < 0 || start >= end || end > len(session.Cards) {
			return nil, errors.New("current-state transaction legacy batch index is invalid")
		}
		return append([]memoryCurrentStateTransactionBatchCard(nil), session.Cards[start:end]...), nil
	}
	if batchIndex < 0 || batchIndex >= len(session.BatchPayloadPaths) {
		return nil, errors.New("current-state transaction batch payload index is invalid")
	}
	path, err := m.currentStateTransactionAbsolutePath(session.BatchPayloadPaths[batchIndex])
	if err != nil {
		return nil, err
	}
	raw, err := readOwnerOnlyBoundedFile(path, memoryCurrentStateTransactionBatchPayloadMaxBytes)
	if err != nil {
		return nil, err
	}
	cards, err := decodeMemoryCurrentStateTransactionBatchPayload(raw)
	if err != nil {
		return nil, fmt.Errorf("decode current-state transaction batch payload: %w", err)
	}
	if len(cards) == 0 || len(cards) > memoryCurrentStateTransactionMaxCards {
		return nil, errors.New("current-state transaction batch payload card count is invalid")
	}
	seenProjects := make(map[string]struct{}, len(cards))
	for _, card := range cards {
		project := normalizeCurrentKeyIndexProject(card.Project)
		if project == "" || project != card.Project {
			return nil, errors.New("current-state transaction batch payload project is not canonical")
		}
		if _, exists := seenProjects[project]; exists || !memoryCurrentStateGenerationDigestValid(card.NewDigest) || (strings.TrimSpace(card.OldDigest) != "" && !memoryCurrentStateGenerationDigestValid(card.OldDigest)) || memoryCurrentStateTransactionDigest(card.Payload) != card.NewDigest {
			return nil, fmt.Errorf("current-state transaction batch payload card %q is invalid", project)
		}
		if err := validateMemoryCurrentStateGenerationCardPayload(project, card.Payload); err != nil {
			return nil, err
		}
		seenProjects[project] = struct{}{}
	}
	return cards, nil
}

func (m *memoryStore) currentStateTransactionValidateBatchPayloadsLocked(session memoryCurrentStateTransactionBatchSession) error {
	seenProjects := make(map[string]struct{}, session.CardCount)
	var totalCardBytes int64
	for batchIndex := range session.BatchPayloadPaths {
		cards, err := m.currentStateTransactionReadBatchPayloadLocked(session, batchIndex)
		if err != nil {
			return fmt.Errorf("read current-state transaction batch payload %d: %w", batchIndex, err)
		}
		expected := memoryCurrentStateTransactionMaxCards
		if remaining := session.CardCount - batchIndex*memoryCurrentStateTransactionMaxCards; remaining < expected {
			expected = remaining
		}
		if len(cards) != expected {
			return fmt.Errorf("current-state transaction batch payload %d has %d cards, want %d", batchIndex, len(cards), expected)
		}
		for _, card := range cards {
			project := normalizeCurrentKeyIndexProject(card.Project)
			if project == "" || project != card.Project {
				return errors.New("current-state transaction batch payload project is not canonical")
			}
			if _, exists := seenProjects[project]; exists {
				return fmt.Errorf("current-state transaction batch repeats project %q", project)
			}
			if !memoryCurrentStateGenerationDigestValid(card.NewDigest) || (strings.TrimSpace(card.OldDigest) != "" && !memoryCurrentStateGenerationDigestValid(card.OldDigest)) || memoryCurrentStateTransactionDigest(card.Payload) != card.NewDigest {
				return fmt.Errorf("current-state transaction batch card %q digest is invalid", project)
			}
			if err := validateMemoryCurrentStateGenerationCardPayload(project, card.Payload); err != nil {
				return err
			}
			seenProjects[project] = struct{}{}
			totalCardBytes += int64(len(card.Payload))
			if totalCardBytes > memoryCurrentStateGenerationMaxCardBytes {
				return fmt.Errorf("current-state transaction batch cards exceed byte cap %d", memoryCurrentStateGenerationMaxCardBytes)
			}
		}
	}
	if len(seenProjects) != session.CardCount {
		return fmt.Errorf("current-state transaction batch payload card count mismatch: have=%d want=%d", len(seenProjects), session.CardCount)
	}
	return nil
}

// currentStateTransactionCompleteBatchLocked durably retires the artifacts
// owned by a completed batch session. The session is the cleanup commit record:
// payloads and any already-installed root stages are removed and synced before
// it is removed last. A crash at any earlier point therefore leaves the exact
// bounded ownership list available for an idempotent retry.
func (m *memoryStore) currentStateTransactionCompleteBatchLocked(sessionName string, session memoryCurrentStateTransactionBatchSession) error {
	if m == nil {
		return errors.New("memory store unavailable")
	}
	if sessionName != memoryCurrentStateTransactionBatchSessionPath(session.TransactionID) || session.NextBatch < session.TotalBatches {
		return errors.New("current-state transaction batch session is not complete")
	}
	owned := make([]string, 0, len(session.BatchPayloadPaths)+len(session.Shards)+1)
	owned = append(owned, session.BatchPayloadPaths...)
	owned = append(owned, session.StageManifestPath)
	for _, shard := range session.Shards {
		owned = append(owned, shard.StagePath)
	}
	for _, relative := range owned {
		path, err := m.currentStateTransactionAbsolutePath(relative)
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove completed current-state transaction batch artifact %q: %w", relative, err)
		}
	}
	if len(owned) > 0 {
		if err := syncOwnerOnlyDirectory(m.currentStateRootPath()); err != nil {
			return fmt.Errorf("sync completed current-state transaction batch artifact removal: %w", err)
		}
	}
	m.currentStateGenerationManifestRecordsInstalledLocked()
	sessionPath, err := m.currentStateTransactionAbsolutePath(sessionName)
	if err != nil {
		return err
	}
	if err := os.Remove(sessionPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove completed current-state transaction batch session: %w", err)
	}
	if err := syncOwnerOnlyDirectory(m.currentStateRootPath()); err != nil {
		return fmt.Errorf("sync completed current-state transaction batch session removal: %w", err)
	}
	return nil
}

func (m *memoryStore) currentStateTransactionFinishBatchLocked(txn memoryCurrentStateTransaction) error {
	if m == nil {
		return errors.New("memory store unavailable")
	}
	session, raw, err := m.currentStateTransactionReadBatchSessionLocked(txn.BatchSessionPath)
	if err != nil {
		return fmt.Errorf("read current-state transaction batch session: %w", err)
	}
	if session.TransactionID != txn.TransactionID || session.NextBatch < txn.BatchIndex || session.NextBatch > txn.BatchIndex+1 {
		return errors.New("current-state transaction batch session changed before acknowledgement")
	}
	if session.NextBatch == txn.BatchIndex {
		if memoryCurrentStateTransactionBatchSessionDigest(raw) != txn.SessionDigest {
			return errors.New("current-state transaction batch session changed before acknowledgement")
		}
	} else {
		// The progress CAS may have reached durable storage immediately before
		// marker removal. Prove that the only change was this exact one-step
		// acknowledgement by reconstructing the canonical prior session bytes
		// bound into the marker; never accept a skipped or caller-edited batch.
		prior := session
		prior.NextBatch = txn.BatchIndex
		priorRaw, marshalErr := json.Marshal(prior)
		if marshalErr != nil {
			return marshalErr
		}
		priorRaw = append(priorRaw, '\n')
		if memoryCurrentStateTransactionBatchSessionDigest(priorRaw) != txn.SessionDigest {
			return errors.New("current-state transaction batch session changed before acknowledgement")
		}
	}
	if session.NextBatch == txn.BatchIndex {
		session.NextBatch = txn.BatchIndex + 1
		updated, err := json.Marshal(session)
		if err != nil {
			return err
		}
		updated = append(updated, '\n')
		if int64(len(updated)) > memoryCurrentStateTransactionBatchMaxBytes {
			return errors.New("current-state transaction batch session exceeds byte cap")
		}
		sessionPath, err := m.currentStateTransactionAbsolutePath(txn.BatchSessionPath)
		if err != nil {
			return err
		}
		if err := writeOwnerOnlyDurableAtomicFile(sessionPath, updated, true); err != nil {
			return fmt.Errorf("persist current-state transaction batch progress: %w", err)
		}
		if err := m.currentStateTransactionFault("after_batch_progress"); err != nil {
			return fmt.Errorf("%w: current-state transaction batch after progress: %v", errMemoryCurrentStateTransactionCommitted, err)
		}
	}
	if err := m.currentStateTransactionFault("before_marker_removal"); err != nil {
		return fmt.Errorf("%w: current-state transaction batch before marker removal: %v", errMemoryCurrentStateTransactionCommitted, err)
	}
	if m.memoryCurrentStateTransactionBeforeMarkerRemoval != nil {
		if err := m.memoryCurrentStateTransactionBeforeMarkerRemoval(); err != nil {
			return fmt.Errorf("%w: current-state transaction batch before marker removal: %v", errMemoryCurrentStateTransactionCommitted, err)
		}
	}
	if err := os.Remove(m.currentStateTransactionPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove current-state transaction batch marker: %w", err)
	}
	if err := syncOwnerOnlyDirectory(m.currentStateRootPath()); err != nil {
		return fmt.Errorf("sync current-state transaction batch marker removal: %w", err)
	}
	if err := m.currentStateTransactionFault("after_marker_removal"); err != nil {
		return fmt.Errorf("%w: current-state transaction batch after marker removal: %v", errMemoryCurrentStateTransactionCommitted, err)
	}
	if session.NextBatch >= session.TotalBatches {
		return m.currentStateTransactionCompleteBatchLocked(txn.BatchSessionPath, session)
	}
	return nil
}

func (m *memoryStore) currentStateTransactionBatchMarkerLocked(session memoryCurrentStateTransactionBatchSession, raw []byte, batchIndex int) (memoryCurrentStateTransaction, map[string][]byte, error) {
	if err := validateMemoryCurrentStateTransactionBatchSession(session); err != nil {
		return memoryCurrentStateTransaction{}, nil, err
	}
	if batchIndex != session.NextBatch || batchIndex < 0 || batchIndex >= session.TotalBatches {
		return memoryCurrentStateTransaction{}, nil, errors.New("current-state transaction batch progress is not monotonic")
	}
	finalBatch := batchIndex == session.TotalBatches-1
	txn := memoryCurrentStateTransaction{
		SchemaID:         memoryCurrentStateTransactionSchemaID,
		Version:          memoryCurrentStateTransactionVersion,
		TransactionID:    session.TransactionID,
		State:            "batch",
		BatchSessionPath: memoryCurrentStateTransactionBatchSessionPath(session.TransactionID),
		BatchIndex:       batchIndex,
		BatchCount:       session.TotalBatches,
		SessionDigest:    memoryCurrentStateTransactionBatchSessionDigest(raw),
		Cards:            make([]memoryCurrentStateTransactionCard, 0, memoryCurrentStateTransactionMaxCards),
	}
	if finalBatch {
		txn.State = "prepared"
		txn.Shards = append([]memoryCurrentStateTransactionShard(nil), session.Shards...)
		txn.StageManifestPath = session.StageManifestPath
		txn.FinalManifestPath = session.FinalManifestPath
		txn.OldManifestDigest = session.OldManifestDigest
		txn.NewManifestDigest = session.NewManifestDigest
	}
	cards, err := m.currentStateTransactionReadBatchPayloadLocked(session, batchIndex)
	if err != nil {
		return memoryCurrentStateTransaction{}, nil, err
	}
	cardPayloads := make(map[string][]byte, len(cards))
	for _, card := range cards {
		cardName := memoryCurrentStateGenerationCardName(card.Project)
		stagePath := filepath.Join(memoryCurrentStateGenerationCardsDir, ".txn-"+session.TransactionID+"-card-"+cardName)
		txn.Cards = append(txn.Cards, memoryCurrentStateTransactionCard{
			Project: card.Project, StagePath: stagePath,
			FinalPath: filepath.Join(memoryCurrentStateGenerationCardsDir, cardName),
			OldDigest: card.OldDigest, NewDigest: card.NewDigest,
		})
		cardPayloads[card.Project] = card.Payload
	}
	return txn, cardPayloads, nil
}

func (m *memoryStore) currentStateTransactionBatchSessionNameLocked() (string, error) {
	if m == nil {
		return "", errors.New("memory store unavailable")
	}
	dir, err := os.Open(m.currentStateRootPath())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer dir.Close()
	totalEntries := 0
	sessionName := ""
	for {
		names, readErr := dir.Readdirnames(1)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", readErr
		}
		totalEntries++
		if totalEntries > memoryCurrentStateTransactionMaxFixedRootEntries {
			return "", fmt.Errorf("current-state transaction batch root entry count exceeds cap %d", memoryCurrentStateTransactionMaxFixedRootEntries)
		}
		if memoryCurrentStateTransactionOrphanKind(names[0]) != "batch" {
			continue
		}
		if sessionName != "" {
			return "", errors.New("multiple current-state transaction batch sessions exist")
		}
		sessionName = names[0]
	}
	return sessionName, nil
}

func (m *memoryStore) currentStateTransactionResumeBatchLocked() error {
	if m == nil {
		return errors.New("memory store unavailable")
	}
	for {
		sessionName, err := m.currentStateTransactionBatchSessionNameLocked()
		if err != nil {
			return err
		}
		if sessionName == "" {
			return nil
		}
		session, raw, err := m.currentStateTransactionReadBatchSessionLocked(sessionName)
		if err != nil {
			return fmt.Errorf("read current-state transaction batch session: %w", err)
		}
		if session.NextBatch >= session.TotalBatches {
			return m.currentStateTransactionCompleteBatchLocked(sessionName, session)
		}
		txn, cardPayloads, err := m.currentStateTransactionBatchMarkerLocked(session, raw, session.NextBatch)
		if err != nil {
			return err
		}
		if err := m.currentStateTransactionMarker(txn); err != nil {
			return err
		}
		for _, card := range txn.Cards {
			path, pathErr := m.currentStateTransactionAbsolutePath(card.StagePath)
			if pathErr != nil {
				return pathErr
			}
			payload := cardPayloads[card.Project]
			var hook func(string) error
			if m.memoryCurrentStateTransactionAtomicWriteFault != nil {
				hook = func(event string) error {
					return m.memoryCurrentStateTransactionAtomicWriteFault("card_" + event)
				}
			}
			if err := writeOwnerOnlyDurableAtomicFileWithHook(path, payload, true, hook); err != nil {
				m.currentStateTransactionCleanupStaged(txn)
				return fmt.Errorf("stage current-state transaction batch card %q: %w", card.Project, err)
			}
		}
		if err := m.currentStateTransactionFault("before_marker"); err != nil {
			m.currentStateTransactionCleanupStaged(txn)
			return err
		}
		markerRaw, err := json.Marshal(txn)
		if err != nil {
			m.currentStateTransactionCleanupStaged(txn)
			return err
		}
		markerRaw = append(markerRaw, '\n')
		if err := writeOwnerOnlyDurableAtomicFile(m.currentStateTransactionPath(), markerRaw, true); err != nil {
			m.currentStateTransactionCleanupStaged(txn)
			return err
		}
		if err := m.currentStateTransactionFault("after_marker"); err != nil {
			return fmt.Errorf("%w: current-state transaction batch after marker: %v", errMemoryCurrentStateTransactionCommitted, err)
		}
		if err := m.currentStateTransactionRollForwardLocked(txn); err != nil {
			return fmt.Errorf("%w: current-state transaction batch roll-forward: %v", errMemoryCurrentStateTransactionCommitted, err)
		}
	}
}

func (m *memoryStore) currentStateTransactionRollForwardCardsLocked(txn memoryCurrentStateTransaction) error {
	for _, card := range txn.Cards {
		stagePath, _ := m.currentStateTransactionAbsolutePath(card.StagePath)
		finalPath, _ := m.currentStateTransactionAbsolutePath(card.FinalPath)
		finalDigest, err := memoryCurrentStateTransactionReadDigest(finalPath)
		if err != nil {
			return fmt.Errorf("inspect current-state project card %q during recovery: %w", card.Project, err)
		}
		if finalDigest == card.NewDigest {
			if _, err := m.currentStateTransactionValidateArtifact(stagePath, card.NewDigest, true); err != nil {
				return fmt.Errorf("validate completed current-state project card %q: %w", card.Project, err)
			}
			if _, err := os.Lstat(stagePath); err == nil {
				if err := os.Remove(stagePath); err != nil {
					return fmt.Errorf("remove completed current-state project card %q stage: %w", card.Project, err)
				}
			}
			continue
		}
		if finalDigest != card.OldDigest {
			return fmt.Errorf("current-state project card %q is neither old nor new", card.Project)
		}
		if _, err := m.currentStateTransactionValidateArtifact(stagePath, card.NewDigest, false); err != nil {
			return fmt.Errorf("validate staged current-state project card %q: %w", card.Project, err)
		}
		if err := replaceOwnerOnlyFile(stagePath, finalPath); err != nil {
			return fmt.Errorf("roll forward current-state project card %q: %w", card.Project, err)
		}
		if err := syncOwnerOnlyDirectory(m.currentStateGenerationCardsPath()); err != nil {
			return fmt.Errorf("sync current-state project card %q recovery rename: %w", card.Project, err)
		}
		if err := m.currentStateTransactionFault("after_card_rename"); err != nil {
			return fmt.Errorf("current-state transaction after project card %q rename: %w", card.Project, err)
		}
	}
	return nil
}

func (m *memoryStore) currentStateTransactionRollForwardExactStateLocked(txn memoryCurrentStateTransaction) error {
	if strings.TrimSpace(txn.ExactFinalPath) == "" {
		return nil
	}
	stagePath, _ := m.currentStateTransactionExactAbsolutePath(txn.ExactStagePath)
	finalPath, _ := m.currentStateTransactionExactAbsolutePath(txn.ExactFinalPath)
	finalDigest, err := memoryCurrentStateTransactionReadDigestBounded(finalPath, memoryExactStateIndexMaxBytes)
	if err != nil {
		return fmt.Errorf("inspect exact-state index during recovery: %w", err)
	}
	if finalDigest == txn.NewExactDigest {
		if _, err := m.currentStateTransactionValidateArtifactBounded(stagePath, txn.NewExactDigest, true, memoryExactStateIndexMaxBytes); err != nil {
			return fmt.Errorf("validate completed exact-state index: %w", err)
		}
		if _, err := os.Lstat(stagePath); err == nil {
			if err := os.Remove(stagePath); err != nil {
				return fmt.Errorf("remove completed exact-state index stage: %w", err)
			}
		}
		return nil
	}
	if finalDigest != txn.OldExactDigest {
		return errors.New("exact-state index is neither old nor new")
	}
	if _, err := m.currentStateTransactionValidateArtifactBounded(stagePath, txn.NewExactDigest, false, memoryExactStateIndexMaxBytes); err != nil {
		return fmt.Errorf("validate staged exact-state index: %w", err)
	}
	if err := replaceOwnerOnlyFile(stagePath, finalPath); err != nil {
		return fmt.Errorf("roll forward exact-state index: %w", err)
	}
	if err := syncOwnerOnlyDirectory(m.currentStateTransactionExactRootPath()); err != nil {
		return fmt.Errorf("sync exact-state index recovery rename: %w", err)
	}
	if err := m.currentStateTransactionFault("after_exact_state_rename"); err != nil {
		return fmt.Errorf("current-state transaction after exact-state index rename: %w", err)
	}
	return nil
}

func (m *memoryStore) recoverCurrentStateTransactionLocked() error {
	if m == nil {
		return nil
	}
	raw, err := readOwnerOnlyBoundedFile(m.currentStateTransactionPath(), memoryEdgeLogMaxRecoveryBytes)
	if errors.Is(err, os.ErrNotExist) {
		sessionName, readErr := m.currentStateTransactionBatchSessionNameLocked()
		if readErr != nil {
			return readErr
		}
		// Reconcile server-owned atomic temps before opening a batch session.
		// This guarantees a partial write is bounded/quarantined before any
		// payload or card is loaded for resume. The active committed session is
		// explicitly protected from quarantine and validated below.
		if err := m.reconcileCurrentStateTransactionOrphansLocked(sessionName); err != nil {
			return err
		}
		if sessionName != "" {
			session, _, sessionErr := m.currentStateTransactionReadBatchSessionLocked(sessionName)
			if sessionErr != nil {
				return sessionErr
			}
			// A completed session is the durable cleanup record. Its payloads may
			// already be partially removed by a prior cleanup attempt, so resume
			// that idempotent retirement without requiring every payload to remain.
			if session.NextBatch < session.TotalBatches && len(session.BatchPayloadPaths) > 0 {
				if err := m.currentStateTransactionValidateBatchPayloadsLocked(session); err != nil {
					return err
				}
			}
			return m.currentStateTransactionResumeBatchLocked()
		}
		return nil
	}
	if err != nil {
		return err
	}
	var txn memoryCurrentStateTransaction
	if err := json.Unmarshal(raw, &txn); err != nil {
		return fmt.Errorf("decode current-state transaction marker: %w", err)
	}
	if err := m.currentStateTransactionRollForwardLocked(txn); err != nil {
		return err
	}
	if strings.TrimSpace(txn.BatchSessionPath) != "" {
		// An intermediate marker acknowledges only one card batch. Once that
		// marker is removed, continue the same durable session before loading
		// shards or the manifest; otherwise startup could observe the old
		// manifest while later card batches remain staged.
		return m.currentStateTransactionResumeBatchLocked()
	}
	return nil
}

func memoryCurrentStateTransactionHex(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

func memoryCurrentStateTransactionOrphanKind(name string) string {
	if !strings.HasPrefix(name, ".txn-") || !strings.HasSuffix(name, ".json") {
		return ""
	}
	withoutPrefix := strings.TrimPrefix(name, ".txn-")
	parts := strings.SplitN(withoutPrefix, "-", 2)
	if len(parts) != 2 || len(parts[0]) != 24 || !memoryCurrentStateTransactionHex(parts[0]) {
		return ""
	}
	suffix := strings.TrimSuffix(parts[1], ".json")
	if suffix == "generations" {
		return "manifest"
	}
	if suffix == "batch" {
		return "batch"
	}
	if strings.HasPrefix(suffix, "batch-") {
		index := strings.TrimPrefix(suffix, "batch-")
		if len(index) == 3 && memoryCurrentStateTransactionDecimal(index) {
			batchIndex, err := strconv.Atoi(index)
			if err == nil && batchIndex < memoryCurrentStateTransactionBatchPayloadCount {
				return "batch-payload"
			}
		}
		return ""
	}
	if strings.HasPrefix(suffix, "card-") {
		cardName := strings.TrimPrefix(suffix, "card-") + ".json"
		if len(strings.TrimSuffix(cardName, ".json")) == 64 && isHexDigest(strings.TrimSuffix(cardName, ".json")) {
			return "card"
		}
		return ""
	}
	if len(suffix) == 2 && memoryCurrentStateTransactionHex(suffix) {
		shard, err := strconv.ParseUint(suffix, 16, 8)
		if err == nil && shard < memoryCurrentStateShardCount {
			return "shard"
		}
	}
	if suffix == "exact-state-index-stage" {
		return "exact"
	}
	return ""
}

func memoryCurrentStateTransactionDecimal(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

// memoryCurrentStateTransactionAtomicTemp describes only the exact names
// emitted by writeOwnerOnlyDurableAtomicFile for a .txn-* artifact. A
// double-dot prefix, a valid transaction artifact grammar, and a bounded
// CreateTemp suffix are all required; lookalikes fail closed rather than
// being silently skipped.
func memoryCurrentStateTransactionAtomicTemp(name string) (string, string, bool) {
	if !strings.HasPrefix(name, "..txn-") {
		return "", "", false
	}
	logical := strings.TrimPrefix(name, ".")
	tempMarker := strings.LastIndex(logical, ".tmp-")
	if tempMarker <= 0 || tempMarker+len(".tmp-") >= len(logical) {
		return "", "", false
	}
	suffix := logical[tempMarker+len(".tmp-"):]
	if len(suffix) == 0 || len(suffix) > 10 {
		return "", "", false
	}
	for _, char := range suffix {
		if char < '0' || char > '9' {
			return "", "", false
		}
	}
	base := logical[:tempMarker]
	kind := memoryCurrentStateTransactionOrphanKind(base)
	if kind == "" {
		return "", "", false
	}
	return base, kind, true
}

func (m *memoryStore) validateMemoryCurrentStateTransactionOrphan(path, name, kind string) error {
	if m == nil {
		return errors.New("memory store unavailable")
	}
	maxBytes := memoryEdgeLogMaxRecoveryBytes
	if kind == "exact" {
		maxBytes = memoryExactStateIndexMaxBytes
	} else if kind == "batch" || kind == "batch-payload" {
		maxBytes = memoryCurrentStateTransactionBatchMaxBytes
		if kind == "batch-payload" {
			maxBytes = memoryCurrentStateTransactionBatchPayloadMaxBytes
		}
	}
	raw, err := readOwnerOnlyBoundedFile(path, maxBytes)
	if err != nil {
		return err
	}
	switch kind {
	case "manifest":
		var manifest memoryCurrentStateGenerationManifest
		if err := json.Unmarshal(raw, &manifest); err != nil || manifest.SchemaID != memoryCurrentStateGenerationSchemaID || (manifest.Version != 1 && manifest.Version != memoryCurrentStateGenerationDigestVersion && manifest.Version != memoryCurrentStateGenerationVersion) || !memoryCurrentStateGenerationDigestValid(manifest.StateDigest) {
			return errors.New("orphan current-state generation manifest is tampered")
		}
		if manifest.Version == memoryCurrentStateGenerationVersion {
			if manifest.Projects != nil || manifest.ProjectCardsDir != memoryCurrentStateGenerationCardsDir || manifest.ProjectCardsVersion != memoryCurrentStateGenerationCardsVersion || (manifest.ProjectCardsDigestVersion != 0 && manifest.ProjectCardsDigestVersion != memoryCurrentStateGenerationCardsDigestVersion && manifest.ProjectCardsDigestVersion != memoryCurrentStateGenerationCardsLegacySparseVersion && manifest.ProjectCardsDigestVersion != memoryCurrentStateGenerationCardsLegacyTreeVersion && manifest.ProjectCardsDigestVersion != memoryCurrentStateGenerationCardsLegacyBucketVersion) || manifest.ProjectCardsCount < 0 || !memoryCurrentStateGenerationDigestValid(manifest.ProjectCardsDigest) {
				return errors.New("orphan current-state generation manifest is tampered")
			}
		} else if len(manifest.Projects) > memoryCurrentStateGenerationMaxCards {
			return errors.New("orphan current-state generation manifest exceeds project cap")
		}
	case "batch":
		if int64(len(raw)) > memoryCurrentStateTransactionBatchMaxBytes {
			return errors.New("orphan current-state transaction batch exceeds byte cap")
		}
		var session memoryCurrentStateTransactionBatchSession
		if err := json.Unmarshal(raw, &session); err != nil {
			return errors.New("orphan current-state transaction batch is tampered")
		}
		if err := validateMemoryCurrentStateTransactionBatchSession(session); err != nil {
			return fmt.Errorf("orphan current-state transaction batch is tampered: %w", err)
		}
	case "batch-payload":
		cards, decodeErr := decodeMemoryCurrentStateTransactionBatchPayload(raw)
		if decodeErr != nil || len(cards) == 0 || len(cards) > memoryCurrentStateTransactionMaxCards {
			return errors.New("orphan current-state transaction batch payload is tampered")
		}
		seen := make(map[string]struct{}, len(cards))
		var totalBytes int64
		for _, card := range cards {
			project := normalizeCurrentKeyIndexProject(card.Project)
			if project == "" || project != card.Project {
				return errors.New("orphan current-state transaction batch payload project is not canonical")
			}
			if _, exists := seen[project]; exists || !memoryCurrentStateGenerationDigestValid(card.NewDigest) || (strings.TrimSpace(card.OldDigest) != "" && !memoryCurrentStateGenerationDigestValid(card.OldDigest)) || memoryCurrentStateTransactionDigest(card.Payload) != card.NewDigest {
				return errors.New("orphan current-state transaction batch payload card is invalid")
			}
			if err := validateMemoryCurrentStateGenerationCardPayload(project, card.Payload); err != nil {
				return err
			}
			seen[project] = struct{}{}
			totalBytes += int64(len(card.Payload))
			if totalBytes > memoryCurrentStateGenerationMaxCardBytes {
				return errors.New("orphan current-state transaction batch payloads exceed byte cap")
			}
		}
	case "shard":
		var shard memoryCurrentStateShard
		suffix := strings.TrimSuffix(strings.SplitN(strings.TrimPrefix(name, ".txn-"), "-", 2)[1], ".json")
		shardNumber, _ := strconv.ParseUint(suffix, 16, 8)
		if err := json.Unmarshal(raw, &shard); err != nil || shard.SchemaID != memoryCurrentStateSchemaID || shard.Version != 1 || shard.Shard != int(shardNumber) {
			return errors.New("orphan current-state shard is tampered")
		}
		for _, state := range shard.Entries {
			project, err := sanitizeMemoryProject(state.Entry.Project)
			if err != nil {
				return errors.New("orphan current-state shard has an invalid project")
			}
			fileName, err := sanitizeMemoryFile(state.Entry.FileName)
			if err != nil || memoryCurrentStateShardForKey(memoryStoreKey(project, fileName)) != int(shardNumber) {
				return errors.New("orphan current-state shard has an invalid entry")
			}
		}
	case "card":
		var card memoryCurrentStateGenerationCard
		if err := json.Unmarshal(raw, &card); err != nil {
			return errors.New("orphan current-state generation card is tampered")
		}
		project := normalizeCurrentKeyIndexProject(card.Project)
		parts := strings.SplitN(strings.TrimPrefix(name, ".txn-"), "-card-", 2)
		cardName := ""
		if len(parts) == 2 {
			cardName = parts[1]
		}
		if card.SchemaID != memoryCurrentStateGenerationCardSchemaID || card.Version != memoryCurrentStateGenerationCardsVersion || project == "" || memoryCurrentStateGenerationCardName(project) != cardName || card.Record.KeyGeneration != card.Record.TopicGeneration || !memoryCurrentStateGenerationDigestValid(card.Record.StateDigest) {
			return errors.New("orphan current-state generation card is tampered")
		}
	case "exact":
		var index exactStateIndex
		if err := json.Unmarshal(raw, &index); err != nil || index.SchemaID != exactStateIndexSchemaID || len(index.Paths) > m.policy.exactStateMaxPaths {
			return errors.New("orphan exact-state index is tampered")
		}
		seen := make(map[string]struct{}, len(index.Paths))
		for _, token := range index.Paths {
			project, fileName, ok := parseMemoryStoreKeyToken(token)
			if !ok {
				return errors.New("orphan exact-state index has an invalid path")
			}
			cleanProject, err := sanitizeMemoryProject(project)
			if err != nil {
				return errors.New("orphan exact-state index has an invalid project")
			}
			cleanFile, err := sanitizeMemoryFile(fileName)
			if err != nil {
				return errors.New("orphan exact-state index has an invalid file")
			}
			canonical := memoryStoreKey(cleanProject, cleanFile)
			if _, exists := seen[canonical]; exists {
				return errors.New("orphan exact-state index repeats a path")
			}
			seen[canonical] = struct{}{}
		}
	}
	return nil
}

func (m *memoryStore) quarantineCurrentStateTransactionOrphan(path string, logicalNames ...string) error {
	if m == nil {
		return errors.New("memory store unavailable")
	}
	quarantine := filepath.Join(m.currentStateRootPath(), memoryCurrentStateTransactionQuarantineDir)
	if err := ensureOwnerOnlyDirectory(quarantine, true); err != nil {
		return fmt.Errorf("create current-state transaction quarantine: %w", err)
	}
	quarantineInfo, err := os.Lstat(quarantine)
	if err != nil {
		return err
	}
	if quarantineInfo.Mode()&os.ModeSymlink != 0 || !quarantineInfo.IsDir() {
		return errors.New("current-state transaction quarantine is not a real directory")
	}
	base := filepath.Base(path)
	logicalName := base
	if len(logicalNames) > 0 && strings.TrimSpace(logicalNames[0]) != "" {
		logicalName = logicalNames[0]
	}
	digest := sha256Hex(filepath.Clean(path))
	target := filepath.Join(quarantine, digest[:24]+"-"+base)
	maxBytes := memoryEdgeLogMaxRecoveryBytes
	if memoryCurrentStateTransactionOrphanKind(logicalName) == "exact" {
		maxBytes = memoryExactStateIndexMaxBytes
	} else if memoryCurrentStateTransactionOrphanKind(logicalName) == "batch" {
		maxBytes = memoryCurrentStateTransactionBatchMaxBytes
	} else if memoryCurrentStateTransactionOrphanKind(logicalName) == "batch-payload" {
		maxBytes = memoryCurrentStateTransactionBatchPayloadMaxBytes
	}
	// A process can die repeatedly at the same pre-marker boundary. If the
	// already-quarantined receipt is byte-identical, discard only the duplicate
	// live stage after verifying both bounded digests; this keeps retention
	// bounded without confusing a changed same-path stage with a valid receipt.
	orphanDigest, err := memoryCurrentStateTransactionReadDigestBounded(path, maxBytes)
	if err != nil {
		return err
	}
	if _, statErr := os.Lstat(target); statErr == nil {
		receiptDigest, digestErr := memoryCurrentStateTransactionReadDigestBounded(target, maxBytes)
		if digestErr != nil {
			return fmt.Errorf("validate existing current-state transaction quarantine receipt: %w", digestErr)
		}
		if receiptDigest != orphanDigest {
			return errors.New("current-state transaction quarantine receipt conflicts with orphan stage")
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove duplicate orphan current-state transaction stage: %w", err)
		}
		return syncOwnerOnlyDirectory(filepath.Dir(path))
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	dir, err := os.Open(quarantine)
	if err != nil {
		return err
	}
	defer dir.Close()
	quarantineCount := 0
	var quarantineBytes int64
	for {
		names, readErr := dir.Readdirnames(1)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
		quarantineCount++
		if quarantineCount > memoryCurrentStateTransactionMaxOrphans {
			return fmt.Errorf("current-state transaction quarantine exceeds file cap %d", memoryCurrentStateTransactionMaxOrphans)
		}
		entryPath := filepath.Join(quarantine, names[0])
		entryInfo, statErr := os.Lstat(entryPath)
		if statErr != nil {
			return statErr
		}
		if entryInfo.Mode()&os.ModeSymlink != 0 || !entryInfo.Mode().IsRegular() {
			return errors.New("current-state transaction quarantine contains a non-regular file")
		}
		quarantineBytes += entryInfo.Size()
		if quarantineBytes > memoryCurrentStateTransactionMaxOrphanBytes {
			return fmt.Errorf("current-state transaction quarantine exceeds byte cap %d", memoryCurrentStateTransactionMaxOrphanBytes)
		}
	}
	orphanInfo, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if orphanInfo.Mode()&os.ModeSymlink != 0 || !orphanInfo.Mode().IsRegular() {
		return errors.New("orphan current-state transaction stage is not a regular file")
	}
	if quarantineCount >= memoryCurrentStateTransactionMaxOrphans || quarantineBytes+orphanInfo.Size() > memoryCurrentStateTransactionMaxOrphanBytes {
		return errors.New("current-state transaction orphan quarantine capacity is exhausted")
	}
	if _, err := os.Lstat(target); err == nil {
		return errors.New("current-state transaction quarantine target already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(path, target); err != nil {
		return fmt.Errorf("quarantine orphan current-state transaction stage: %w", err)
	}
	if err := ensureOwnerOnlyFile(target); err != nil {
		return err
	}
	if err := syncOwnerOnlyDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	if err := syncOwnerOnlyDirectory(quarantine); err != nil {
		return err
	}
	return nil
}

type memoryCurrentStateTransactionOrphanStats struct {
	count int
	bytes int64
}

func (m *memoryStore) currentStateTransactionOrphanRootEntryCap(dir string) int {
	if m != nil && filepath.Clean(dir) == filepath.Clean(m.currentStateGenerationCardsPath()) {
		return memoryCurrentStateTransactionMaxCardRootEntries
	}
	return memoryCurrentStateTransactionMaxFixedRootEntries
}

func (m *memoryStore) reconcileCurrentStateTransactionOrphanDirLocked(dir string, exactRoot bool, stats *memoryCurrentStateTransactionOrphanStats, protectedBatchSession string, protectedNames map[string]struct{}) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("current-state transaction orphan root is not a real directory")
	}
	dirHandle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer dirHandle.Close()
	totalEntries := 0
	entryCap := m.currentStateTransactionOrphanRootEntryCap(dir)
	for {
		names, readErr := dirHandle.Readdirnames(1)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
		name := names[0]
		totalEntries++
		if totalEntries > entryCap {
			return fmt.Errorf("current-state transaction orphan root %q entry count %d exceeds cap %d", dir, totalEntries, entryCap)
		}
		if name == "." || name == ".." || name == memoryCurrentStateTransactionMarker {
			continue
		}
		if name == memoryCurrentStateTransactionQuarantineDir {
			quarantinePath := filepath.Join(dir, name)
			quarantineInfo, quarantineErr := os.Lstat(quarantinePath)
			if quarantineErr != nil {
				return quarantineErr
			}
			if quarantineInfo.Mode()&os.ModeSymlink != 0 || !quarantineInfo.IsDir() {
				return errors.New("current-state transaction quarantine is not a real directory")
			}
			continue
		}
		if protectedBatchSession != "" && filepath.Clean(dir) == filepath.Clean(m.currentStateRootPath()) {
			if _, protected := protectedNames[name]; protected {
				// A committed batch session is the durable progress record. It is
				// validated and resumed after this scan; never quarantine it as an
				// orphan while cleaning other crash debris.
				continue
			}
		}
		logicalName, atomicKind, atomicTemp := memoryCurrentStateTransactionAtomicTemp(name)
		if strings.HasPrefix(name, "..txn-") && !atomicTemp {
			return fmt.Errorf("foreign current-state transaction atomic temporary name %q", name)
		}
		if filepath.Clean(dir) == filepath.Clean(m.currentStateGenerationCardsPath()) && !strings.HasPrefix(name, ".txn-") {
			if !atomicTemp {
				continue
			}
		}
		if !strings.HasPrefix(name, ".txn-") && !atomicTemp {
			continue
		}
		path := filepath.Join(dir, name)
		pathInfo, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
			return fmt.Errorf("orphan current-state transaction stage %q is not a regular file", name)
		}
		kind := memoryCurrentStateTransactionOrphanKind(name)
		if atomicTemp {
			kind = atomicKind
			name = logicalName
		}
		if kind == "" {
			return fmt.Errorf("foreign current-state transaction stage name %q", name)
		}
		if exactRoot {
			if kind != "exact" {
				return fmt.Errorf("foreign exact-state transaction stage name %q", name)
			}
		} else if kind == "exact" {
			return fmt.Errorf("foreign current-state transaction stage name %q", name)
		} else if filepath.Clean(dir) == filepath.Clean(m.currentStateGenerationCardsPath()) && kind != "card" {
			return fmt.Errorf("foreign current-state generation card stage name %q", name)
		} else if filepath.Clean(dir) != filepath.Clean(m.currentStateGenerationCardsPath()) && kind == "card" {
			return fmt.Errorf("foreign current-state transaction card stage name %q", name)
		} else if filepath.Clean(dir) != filepath.Clean(m.currentStateRootPath()) && kind == "batch-payload" {
			return fmt.Errorf("foreign current-state transaction batch payload name %q", name)
		}
		if atomicTemp {
			maxBytes := memoryEdgeLogMaxRecoveryBytes
			if kind == "exact" {
				maxBytes = memoryExactStateIndexMaxBytes
			} else if kind == "batch" {
				maxBytes = memoryCurrentStateTransactionBatchMaxBytes
			} else if kind == "batch-payload" {
				maxBytes = memoryCurrentStateTransactionBatchPayloadMaxBytes
			}
			raw, readErr := readOwnerOnlyBoundedFile(path, maxBytes)
			if readErr != nil {
				return fmt.Errorf("validate orphan current-state transaction temporary %q: %w", name, readErr)
			}
			// A zero-length file is the expected hard-death-at-create/write
			// boundary. Non-empty temps must still prove the exact artifact
			// contract before being retained as a receipt.
			if len(raw) > 0 {
				if validateErr := m.validateMemoryCurrentStateTransactionOrphan(path, name, kind); validateErr != nil {
					// A server-owned temp with a valid grammar but a truncated or
					// otherwise incomplete payload is crash debris, not an input to
					// resume. Retain it as a bounded receipt before any session/card
					// recovery proceeds; foreign names still fail closed above.
					if stats == nil {
						return errors.New("orphan current-state transaction stats are unavailable")
					}
					stageInfo, statErr := os.Lstat(path)
					if statErr != nil {
						return statErr
					}
					if stats.count >= memoryCurrentStateTransactionMaxOrphans || stats.bytes+stageInfo.Size() > memoryCurrentStateTransactionMaxOrphanBytes {
						return fmt.Errorf("orphan current-state transaction capacity exceeded while quarantining temporary %q", name)
					}
					if err := m.quarantineCurrentStateTransactionOrphan(path, logicalName); err != nil {
						return fmt.Errorf("quarantine invalid orphan current-state transaction temporary %q: %w", name, err)
					}
					stats.count++
					stats.bytes += stageInfo.Size()
					continue
				}
			}
		} else if err := m.validateMemoryCurrentStateTransactionOrphan(path, name, kind); err != nil {
			return fmt.Errorf("validate orphan current-state transaction stage %q: %w", name, err)
		}
		if stats == nil {
			return errors.New("orphan current-state transaction stats are unavailable")
		}
		if stats.count >= memoryCurrentStateTransactionMaxOrphans {
			return fmt.Errorf("orphan current-state transaction count exceeds cap %d", memoryCurrentStateTransactionMaxOrphans)
		}
		stageInfo, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if stats.bytes+stageInfo.Size() > memoryCurrentStateTransactionMaxOrphanBytes {
			return fmt.Errorf("orphan current-state transaction bytes exceed cap %d", memoryCurrentStateTransactionMaxOrphanBytes)
		}
		if atomicTemp {
			if err := m.quarantineCurrentStateTransactionOrphan(path, logicalName); err != nil {
				return err
			}
		} else if err := m.quarantineCurrentStateTransactionOrphan(path); err != nil {
			return err
		}
		stats.count++
		stats.bytes += stageInfo.Size()
	}
	return nil
}

func (m *memoryStore) reconcileCurrentStateTransactionOrphansLocked(protectedBatchSession ...string) error {
	if m == nil {
		return nil
	}
	stats := &memoryCurrentStateTransactionOrphanStats{}
	seen := map[string]struct{}{}
	roots := []struct {
		path      string
		exactRoot bool
	}{
		{m.currentStateRootPath(), false},
		{m.currentStateGenerationCardsPath(), false},
		{m.currentStateTransactionExactRootPath(), true},
	}
	protected := ""
	if len(protectedBatchSession) > 0 {
		protected = protectedBatchSession[0]
	}
	protectedNames := map[string]struct{}{}
	if protected != "" {
		protectedNames[protected] = struct{}{}
		session, _, err := m.currentStateTransactionReadBatchSessionLocked(protected)
		if err != nil {
			return err
		}
		protectedNames[session.StageManifestPath] = struct{}{}
		for _, shard := range session.Shards {
			protectedNames[shard.StagePath] = struct{}{}
		}
		for _, payload := range session.BatchPayloadPaths {
			protectedNames[payload] = struct{}{}
		}
	}
	for _, root := range roots {
		clean := filepath.Clean(root.path)
		if _, exists := seen[clean]; exists {
			continue
		}
		seen[clean] = struct{}{}
		if err := m.reconcileCurrentStateTransactionOrphanDirLocked(clean, root.exactRoot, stats, protected, protectedNames); err != nil {
			return err
		}
	}
	return nil
}

func (m *memoryStore) currentStateTransactionAffectedProjectsLocked(shards map[int]struct{}, project string) map[string]struct{} {
	projects := map[string]struct{}{}
	if projectKey := normalizeCurrentKeyIndexProject(project); projectKey != "" {
		projects[projectKey] = struct{}{}
		return projects
	}
	for shard := range shards {
		for key := range m.currentStateByShard[shard] {
			if projectName, _, ok := parseMemoryStoreKeyToken(key); ok {
				if projectKey := normalizeCurrentKeyIndexProject(projectName); projectKey != "" {
					projects[projectKey] = struct{}{}
				}
			}
		}
	}
	return projects
}

func (m *memoryStore) buildCurrentStateTransactionLocked(shards map[int]struct{}, project string, generation uint64, exactPayload []byte) (memoryCurrentStateTransaction, []byte, map[string][]byte, error) {
	if m == nil {
		return memoryCurrentStateTransaction{}, nil, nil, errors.New("memory store unavailable")
	}
	m.ensureCurrentStateDigestIndexesLocked()
	ordered := make([]int, 0, len(shards))
	for shard := range shards {
		if shard < 0 || shard >= memoryCurrentStateShardCount {
			return memoryCurrentStateTransaction{}, nil, nil, errors.New("invalid memory current-state shard")
		}
		ordered = append(ordered, shard)
	}
	sort.Ints(ordered)
	manifest, err := m.currentStateGenerationManifestPayloadLocked()
	if err != nil {
		return memoryCurrentStateTransaction{}, nil, nil, err
	}
	manifestPayload, err := json.Marshal(manifest)
	if err != nil {
		return memoryCurrentStateTransaction{}, nil, nil, fmt.Errorf("encode current-state generation manifest: %w", err)
	}
	manifestPayload = append(manifestPayload, '\n')
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	txnID := sha256Hex(stamp + "|" + memoryCurrentStateTransactionDigest(manifestPayload))[:24]
	txn := memoryCurrentStateTransaction{
		SchemaID:          memoryCurrentStateTransactionSchemaID,
		Version:           memoryCurrentStateTransactionVersion,
		TransactionID:     txnID,
		State:             "prepared",
		Project:           normalizeCurrentKeyIndexProject(project),
		Generation:        generation,
		StageManifestPath: ".txn-" + txnID + "-generations.json",
		FinalManifestPath: "generations.json",
		NewManifestDigest: memoryCurrentStateTransactionDigest(manifestPayload),
		Shards:            make([]memoryCurrentStateTransactionShard, 0, len(ordered)),
		Cards:             make([]memoryCurrentStateTransactionCard, 0),
	}
	oldManifest, err := memoryCurrentStateTransactionReadDigest(m.currentStateGenerationPath())
	if err != nil {
		return memoryCurrentStateTransaction{}, nil, nil, err
	}
	txn.OldManifestDigest = oldManifest
	for _, shard := range ordered {
		payload := append([]byte(nil), m.currentStateShardPayloads[shard]...)
		if payload == nil {
			if err := m.refreshCurrentStateShardDigestIndexesLocked(shard); err != nil {
				return memoryCurrentStateTransaction{}, nil, nil, err
			}
			payload = append([]byte(nil), m.currentStateShardPayloads[shard]...)
		}
		payload = append(payload, '\n')
		if int64(len(payload)) > memoryEdgeLogMaxRecoveryBytes {
			return memoryCurrentStateTransaction{}, nil, nil, fmt.Errorf("%w: current-state shard bytes=%d cap=%d", errMemoryEdgeLogOversized, len(payload), memoryEdgeLogMaxRecoveryBytes)
		}
		finalPath := m.currentStateShardPath(shard)
		oldDigest, err := memoryCurrentStateTransactionReadDigest(finalPath)
		if err != nil {
			return memoryCurrentStateTransaction{}, nil, nil, err
		}
		stagePath := ".txn-" + txnID + "-" + fmt.Sprintf("%02x.json", shard)
		newDigest := memoryCurrentStateTransactionDigest(payload)
		txn.Shards = append(txn.Shards, memoryCurrentStateTransactionShard{Shard: shard, StagePath: stagePath, FinalPath: filepath.Base(finalPath), OldDigest: oldDigest, NewDigest: newDigest})
	}
	cardPayloads := map[string][]byte{}
	projects := m.currentStateTransactionAffectedProjectsLocked(shards, project)
	if len(shards) == 0 && normalizeCurrentKeyIndexProject(project) == "" && m.currentStateGenerationManifestVersion < memoryCurrentStateGenerationVersion {
		for projectKey := range m.currentStateGenerationRecords {
			projects[normalizeCurrentKeyIndexProject(projectKey)] = struct{}{}
		}
	}
	orderedProjects := make([]string, 0, len(projects))
	for projectKey := range projects {
		projectKey = normalizeCurrentKeyIndexProject(projectKey)
		if projectKey == "" {
			continue
		}
		if _, exists := m.currentStateGenerationRecords[projectKey]; !exists {
			continue
		}
		orderedProjects = append(orderedProjects, projectKey)
	}
	sort.Strings(orderedProjects)
	for _, projectKey := range orderedProjects {
		payload, err := memoryCurrentStateGenerationCardPayload(projectKey, m.currentStateGenerationRecords[projectKey])
		if err != nil {
			return memoryCurrentStateTransaction{}, nil, nil, err
		}
		finalPath := m.currentStateGenerationCardPath(projectKey)
		oldDigest, err := memoryCurrentStateTransactionReadDigest(finalPath)
		if err != nil {
			return memoryCurrentStateTransaction{}, nil, nil, err
		}
		cardName := memoryCurrentStateGenerationCardName(projectKey)
		stagePath := filepath.Join(memoryCurrentStateGenerationCardsDir, ".txn-"+txnID+"-card-"+cardName)
		txn.Cards = append(txn.Cards, memoryCurrentStateTransactionCard{
			Project: projectKey, StagePath: stagePath,
			FinalPath: filepath.Join(memoryCurrentStateGenerationCardsDir, cardName),
			OldDigest: oldDigest, NewDigest: memoryCurrentStateTransactionDigest(payload),
		})
		cardPayloads[projectKey] = payload
	}
	if len(exactPayload) > 0 {
		if int64(len(exactPayload)) > memoryExactStateIndexMaxBytes {
			return memoryCurrentStateTransaction{}, nil, nil, fmt.Errorf("%w: exact-state index bytes=%d cap=%d", errMemoryEdgeLogOversized, len(exactPayload), memoryExactStateIndexMaxBytes)
		}
		oldExactDigest, err := memoryCurrentStateTransactionReadDigestBounded(m.policy.exactStateIndexPath, memoryExactStateIndexMaxBytes)
		if err != nil {
			return memoryCurrentStateTransaction{}, nil, nil, err
		}
		exactFinalPath := filepath.Base(m.policy.exactStateIndexPath)
		txn.ExactStagePath = ".txn-" + txnID + "-exact-state-index-stage.json"
		txn.ExactFinalPath = exactFinalPath
		txn.OldExactDigest = oldExactDigest
		txn.NewExactDigest = memoryCurrentStateTransactionDigest(exactPayload)
	}
	return txn, manifestPayload, cardPayloads, nil
}

func (m *memoryStore) currentStateGenerationManifestPayloadLocked() (memoryCurrentStateGenerationManifest, error) {
	if m == nil {
		return memoryCurrentStateGenerationManifest{}, errors.New("memory store unavailable")
	}
	m.ensureCurrentStateDigestIndexesLocked()
	if !m.currentStateGenerationCardsDigestInitialized {
		if err := m.setCurrentStateGenerationCardsAccumulatorLocked(m.currentStateGenerationRecords); err != nil {
			return memoryCurrentStateGenerationManifest{}, fmt.Errorf("build current-state generation card commitment: %w", err)
		}
	}
	return memoryCurrentStateGenerationManifest{
		SchemaID:                  memoryCurrentStateGenerationSchemaID,
		Version:                   memoryCurrentStateGenerationVersion,
		StateDigest:               m.currentStateDigestLocked(""),
		ProjectCardsDir:           memoryCurrentStateGenerationCardsDir,
		ProjectCardsVersion:       memoryCurrentStateGenerationCardsVersion,
		ProjectCardsDigestVersion: memoryCurrentStateGenerationCardsDigestVersion,
		ProjectCardsCount:         m.currentStateGenerationCardCount,
		ProjectCardsDigest:        m.currentStateGenerationCardsDigest,
	}, nil
}

func (m *memoryStore) refreshCurrentStateGenerationRecordsLocked(projects map[string]struct{}) error {
	if m == nil {
		return errors.New("memory store unavailable")
	}
	m.ensureCurrentKeyIndexLocked()
	if m.currentStateGenerationRecords == nil {
		m.currentStateGenerationRecords = map[string]memoryCurrentStateGenerationRecord{}
	}
	// A missing manifest is the bounded initial migration.  Once records are
	// installed, only projects touched by the affected shard set are rebuilt.
	if len(m.currentStateGenerationRecords) == 0 && len(m.currentKeyIndexGeneration) > 0 {
		for projectKey := range m.currentKeyIndexGeneration {
			projects[normalizeCurrentKeyIndexProject(projectKey)] = struct{}{}
		}
	}
	for projectKey := range projects {
		projectKey = normalizeCurrentKeyIndexProject(projectKey)
		if projectKey == "" {
			continue
		}
		keyGeneration, keyOK := m.currentKeyIndexGeneration[projectKey]
		topicGeneration, topicOK := m.currentTopicIndexGeneration[projectKey]
		if !keyOK || !topicOK || keyGeneration != topicGeneration {
			return fmt.Errorf("current-state generation indexes are incoherent for project %q", projectKey)
		}
		previousRecord, previousExists := m.currentStateGenerationRecords[projectKey]
		nextRecord := memoryCurrentStateGenerationRecord{
			KeyGeneration: keyGeneration, TopicGeneration: topicGeneration,
			StateDigest: memoryCurrentStateRootDigest(projectKey, keyGeneration, m.currentStateProjectShardDigests[projectKey]),
		}
		if err := m.updateCurrentStateGenerationCardAccumulatorLocked(projectKey, previousRecord, previousExists, nextRecord, true); err != nil {
			return fmt.Errorf("update current-state generation card commitment for %q: %w", projectKey, err)
		}
		m.currentStateGenerationRecords[projectKey] = nextRecord
	}
	return nil
}

// validateCurrentStateGenerationCapacityLocked is deliberately separate from
// refreshCurrentStateGenerationRecordsLocked. It performs the card count and
// aggregate-byte check before that function mutates the generation map. Once
// the indexed accumulator is initialized, the check is scalar and touches
// only the affected projects; the initial/v1-v2 migration path is the only
// bounded full-map fallback.
func (m *memoryStore) validateCurrentStateGenerationCapacityLocked(projects map[string]struct{}) error {
	if m == nil {
		return errors.New("memory store unavailable")
	}
	m.ensureCurrentKeyIndexLocked()
	if !m.currentStateGenerationCardsDigestInitialized {
		candidate := cloneCurrentStateGenerationRecords(m.currentStateGenerationRecords)
		if candidate == nil {
			candidate = map[string]memoryCurrentStateGenerationRecord{}
		}
		if len(candidate) == 0 && len(m.currentKeyIndexGeneration) > 0 {
			for projectKey := range m.currentKeyIndexGeneration {
				projects[normalizeCurrentKeyIndexProject(projectKey)] = struct{}{}
			}
		}
		for projectKey := range projects {
			projectKey = normalizeCurrentKeyIndexProject(projectKey)
			if projectKey == "" {
				continue
			}
			keyGeneration, keyOK := m.currentKeyIndexGeneration[projectKey]
			topicGeneration, topicOK := m.currentTopicIndexGeneration[projectKey]
			if !keyOK || !topicOK || keyGeneration != topicGeneration {
				return fmt.Errorf("current-state generation indexes are incoherent for project %q", projectKey)
			}
			candidate[projectKey] = memoryCurrentStateGenerationRecord{
				KeyGeneration: keyGeneration, TopicGeneration: topicGeneration,
				StateDigest: memoryCurrentStateRootDigest(projectKey, keyGeneration, m.currentStateProjectShardDigests[projectKey]),
			}
		}
		if _, _, err := memoryCurrentStateGenerationCardSetCapacity(candidate); err != nil {
			return err
		}
		return nil
	}
	projectedCount := m.currentStateGenerationCardCount
	projectedBytes := m.currentStateGenerationCardBytes
	for projectKey := range projects {
		projectKey = normalizeCurrentKeyIndexProject(projectKey)
		if projectKey == "" {
			continue
		}
		keyGeneration, keyOK := m.currentKeyIndexGeneration[projectKey]
		topicGeneration, topicOK := m.currentTopicIndexGeneration[projectKey]
		if !keyOK || !topicOK || keyGeneration != topicGeneration {
			return fmt.Errorf("current-state generation indexes are incoherent for project %q", projectKey)
		}
		previousRecord, previousExists := m.currentStateGenerationRecords[projectKey]
		nextRecord := memoryCurrentStateGenerationRecord{
			KeyGeneration: keyGeneration, TopicGeneration: topicGeneration,
			StateDigest: memoryCurrentStateRootDigest(projectKey, keyGeneration, m.currentStateProjectShardDigests[projectKey]),
		}
		if previousExists {
			payload, err := memoryCurrentStateGenerationCardPayload(projectKey, previousRecord)
			if err != nil {
				return err
			}
			projectedBytes -= int64(len(payload))
		} else {
			projectedCount++
		}
		payload, err := memoryCurrentStateGenerationCardPayload(projectKey, nextRecord)
		if err != nil {
			return err
		}
		projectedBytes += int64(len(payload))
		if projectedCount > memoryCurrentStateGenerationMaxCards {
			return fmt.Errorf("current-state generation card count exceeds cap %d", memoryCurrentStateGenerationMaxCards)
		}
		if projectedBytes > memoryCurrentStateGenerationMaxCardBytes {
			return fmt.Errorf("current-state generation cards exceed byte cap %d", memoryCurrentStateGenerationMaxCardBytes)
		}
	}
	return nil
}

func (m *memoryStore) persistCurrentStateTransactionLocked(shards map[int]struct{}, project string, generation uint64) error {
	return m.persistCurrentStateTransactionWithExactStateLocked(shards, project, generation, nil)
}

func (m *memoryStore) persistCurrentStateTransactionBatchLocked(txn memoryCurrentStateTransaction, manifestPayload []byte, cardPayloads map[string][]byte) error {
	if m == nil {
		return errors.New("memory store unavailable")
	}
	if len(txn.Cards) <= memoryCurrentStateTransactionMaxCards {
		return errors.New("current-state transaction batch requires more than one card batch")
	}
	session := memoryCurrentStateTransactionBatchSession{
		SchemaID:          memoryCurrentStateTransactionSchemaID,
		Version:           memoryCurrentStateTransactionVersion,
		TransactionID:     txn.TransactionID,
		State:             "prepared",
		CardCount:         len(txn.Cards),
		TotalBatches:      (len(txn.Cards) + memoryCurrentStateTransactionMaxCards - 1) / memoryCurrentStateTransactionMaxCards,
		BatchPayloadPaths: make([]string, 0, (len(txn.Cards)+memoryCurrentStateTransactionMaxCards-1)/memoryCurrentStateTransactionMaxCards),
		Shards:            append([]memoryCurrentStateTransactionShard(nil), txn.Shards...),
		StageManifestPath: txn.StageManifestPath,
		FinalManifestPath: txn.FinalManifestPath,
		OldManifestDigest: txn.OldManifestDigest,
		NewManifestDigest: txn.NewManifestDigest,
	}
	batchPayloads := make([][]byte, 0, session.TotalBatches)
	for batchIndex := 0; batchIndex < session.TotalBatches; batchIndex++ {
		start := batchIndex * memoryCurrentStateTransactionMaxCards
		end := start + memoryCurrentStateTransactionMaxCards
		if end > len(txn.Cards) {
			end = len(txn.Cards)
		}
		cards := make([]memoryCurrentStateTransactionBatchCard, 0, end-start)
		for _, card := range txn.Cards[start:end] {
			payload := append([]byte(nil), cardPayloads[card.Project]...)
			cards = append(cards, memoryCurrentStateTransactionBatchCard{
				Project: card.Project, Payload: payload,
				OldDigest: card.OldDigest, NewDigest: card.NewDigest,
			})
		}
		payload, err := encodeMemoryCurrentStateTransactionBatchPayload(cards)
		if err != nil {
			return fmt.Errorf("encode current-state transaction batch payload %d: %w", batchIndex, err)
		}
		batchPayloads = append(batchPayloads, payload)
		session.BatchPayloadPaths = append(session.BatchPayloadPaths, memoryCurrentStateTransactionBatchPayloadPath(txn.TransactionID, batchIndex))
	}
	if err := validateMemoryCurrentStateTransactionBatchSession(session); err != nil {
		return err
	}
	sessionRaw, err := json.Marshal(session)
	if err != nil {
		return fmt.Errorf("encode current-state transaction batch session: %w", err)
	}
	sessionRaw = append(sessionRaw, '\n')
	if int64(len(sessionRaw)) > memoryCurrentStateTransactionBatchMaxBytes {
		return fmt.Errorf("current-state transaction batch session exceeds byte cap %d", memoryCurrentStateTransactionBatchMaxBytes)
	}
	cleanup := func() {
		m.currentStateTransactionCleanupStaged(txn)
		for _, relative := range session.BatchPayloadPaths {
			if path, pathErr := m.currentStateTransactionAbsolutePath(relative); pathErr == nil {
				_ = os.Remove(path)
			}
		}
	}
	for _, shard := range txn.Shards {
		path, pathErr := m.currentStateTransactionAbsolutePath(shard.StagePath)
		if pathErr != nil {
			return pathErr
		}
		payload := append([]byte(nil), m.currentStateShardPayloads[shard.Shard]...)
		payload = append(payload, '\n')
		if err := writeOwnerOnlyDurableAtomicFile(path, payload, true); err != nil {
			cleanup()
			return fmt.Errorf("stage current-state batch shard %d: %w", shard.Shard, err)
		}
	}
	manifestStage, err := m.currentStateTransactionAbsolutePath(txn.StageManifestPath)
	if err != nil {
		cleanup()
		return err
	}
	if err := writeOwnerOnlyDurableAtomicFile(manifestStage, manifestPayload, true); err != nil {
		cleanup()
		return fmt.Errorf("stage current-state batch manifest: %w", err)
	}
	for batchIndex, payload := range batchPayloads {
		path, pathErr := m.currentStateTransactionAbsolutePath(session.BatchPayloadPaths[batchIndex])
		if pathErr != nil {
			cleanup()
			return pathErr
		}
		var hook func(string) error
		if m.memoryCurrentStateTransactionAtomicWriteFault != nil {
			hook = func(event string) error {
				return m.memoryCurrentStateTransactionAtomicWriteFault("batch_payload_" + event)
			}
		}
		if err := writeOwnerOnlyDurableAtomicFileWithHook(path, payload, true, hook); err != nil {
			cleanup()
			return fmt.Errorf("stage current-state transaction batch payload %d: %w", batchIndex, err)
		}
	}
	sessionPath, err := m.currentStateTransactionAbsolutePath(memoryCurrentStateTransactionBatchSessionPath(txn.TransactionID))
	if err != nil {
		cleanup()
		return err
	}
	var sessionHook func(string) error
	if m.memoryCurrentStateTransactionAtomicWriteFault != nil {
		sessionHook = func(event string) error {
			return m.memoryCurrentStateTransactionAtomicWriteFault("batch_session_" + event)
		}
	}
	if err := writeOwnerOnlyDurableAtomicFileWithHook(sessionPath, sessionRaw, true, sessionHook); err != nil {
		if ownerOnlyAtomicWriteCommitted(err) {
			// A committed session is durable progress. Preserve the session and
			// all staged payload/shard/manifest artifacts for idempotent restart.
			return fmt.Errorf("%w: current-state transaction batch session: %v", errMemoryCurrentStateTransactionCommitted, err)
		}
		cleanup()
		return fmt.Errorf("stage current-state transaction batch session: %w", err)
	}
	if err := m.currentStateTransactionFault("batch_session_durable"); err != nil {
		return fmt.Errorf("%w: current-state transaction batch session: %v", errMemoryCurrentStateTransactionCommitted, err)
	}
	return m.currentStateTransactionResumeBatchLocked()
}

func (m *memoryStore) persistCurrentStateTransactionWithExactStateLocked(shards map[int]struct{}, project string, generation uint64, exactPayload []byte) error {
	priorTransaction := false
	if _, err := os.Stat(m.currentStateTransactionPath()); err == nil {
		priorTransaction = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !priorTransaction {
		sessionName, err := m.currentStateTransactionBatchSessionNameLocked()
		if err != nil {
			return fmt.Errorf("inspect prior current-state transaction batch session before new commit: %w", err)
		}
		priorTransaction = sessionName != ""
	}
	if priorTransaction {
		if err := m.recoverCurrentStateTransactionLocked(); err != nil {
			return fmt.Errorf("recover prior current-state transaction before new commit: %w", err)
		}
	}
	affectedProjects := m.currentStateTransactionAffectedProjectsLocked(shards, project)
	if len(shards) == 0 && normalizeCurrentKeyIndexProject(project) == "" && m.currentStateGenerationManifestVersion < memoryCurrentStateGenerationVersion {
		for projectKey := range m.currentStateGenerationRecords {
			affectedProjects[normalizeCurrentKeyIndexProject(projectKey)] = struct{}{}
		}
	}
	if err := m.validateCurrentStateGenerationCapacityLocked(affectedProjects); err != nil {
		return fmt.Errorf("validate current-state generation card capacity before mutation: %w", err)
	}
	if err := m.refreshCurrentStateGenerationRecordsLocked(affectedProjects); err != nil {
		return err
	}
	txn, manifestPayload, cardPayloads, err := m.buildCurrentStateTransactionLocked(shards, project, generation, exactPayload)
	if err != nil {
		return err
	}
	if len(txn.Cards) > memoryCurrentStateTransactionMaxCards {
		if len(exactPayload) > 0 {
			return errors.New("current-state exact-state transaction cannot batch project cards")
		}
		return m.persistCurrentStateTransactionBatchLocked(txn, manifestPayload, cardPayloads)
	}
	if err := m.currentStateTransactionMarker(txn); err != nil {
		return err
	}
	for _, shard := range txn.Shards {
		path, _ := m.currentStateTransactionAbsolutePath(shard.StagePath)
		payload := append([]byte(nil), m.currentStateShardPayloads[shard.Shard]...)
		payload = append(payload, '\n')
		if err := writeOwnerOnlyDurableAtomicFile(path, payload, true); err != nil {
			m.currentStateTransactionCleanupStaged(txn)
			return fmt.Errorf("stage current-state shard %d: %w", shard.Shard, err)
		}
	}
	for _, card := range txn.Cards {
		path, _ := m.currentStateTransactionAbsolutePath(card.StagePath)
		payload := append([]byte(nil), cardPayloads[card.Project]...)
		var hook func(string) error
		if m.memoryCurrentStateTransactionAtomicWriteFault != nil {
			hook = func(event string) error {
				return m.memoryCurrentStateTransactionAtomicWriteFault("card_" + event)
			}
		}
		if err := writeOwnerOnlyDurableAtomicFileWithHook(path, payload, true, hook); err != nil {
			m.currentStateTransactionCleanupStaged(txn)
			return fmt.Errorf("stage current-state project card %q: %w", card.Project, err)
		}
	}
	if strings.TrimSpace(txn.ExactStagePath) != "" {
		path, _ := m.currentStateTransactionExactAbsolutePath(txn.ExactStagePath)
		if err := writeOwnerOnlyDurableAtomicFile(path, exactPayload, true); err != nil {
			m.currentStateTransactionCleanupStaged(txn)
			return fmt.Errorf("stage exact-state index: %w", err)
		}
	}
	manifestStage, _ := m.currentStateTransactionAbsolutePath(txn.StageManifestPath)
	if err := writeOwnerOnlyDurableAtomicFile(manifestStage, manifestPayload, true); err != nil {
		m.currentStateTransactionCleanupStaged(txn)
		return fmt.Errorf("stage current-state generation manifest: %w", err)
	}
	if err := m.currentStateTransactionFault("before_marker"); err != nil {
		m.currentStateTransactionCleanupStaged(txn)
		return fmt.Errorf("current-state transaction before marker: %w", err)
	}
	if m.memoryCurrentStateTransactionBeforeMarker != nil {
		if err := m.memoryCurrentStateTransactionBeforeMarker(); err != nil {
			m.currentStateTransactionCleanupStaged(txn)
			return fmt.Errorf("current-state transaction before marker: %w", err)
		}
	}
	markerRaw, err := json.Marshal(txn)
	if err != nil {
		m.currentStateTransactionCleanupStaged(txn)
		return err
	}
	markerRaw = append(markerRaw, '\n')
	if err := writeOwnerOnlyDurableAtomicFile(m.currentStateTransactionPath(), markerRaw, true); err != nil {
		if _, statErr := os.Stat(m.currentStateTransactionPath()); statErr == nil {
			return fmt.Errorf("%w: persist current-state transaction marker after durable replacement: %v", errMemoryCurrentStateTransactionCommitted, err)
		} else if errors.Is(statErr, os.ErrNotExist) {
			m.currentStateTransactionCleanupStaged(txn)
		} else {
			return fmt.Errorf("%w: persist current-state transaction marker state is indeterminate: %v (stat: %v)", errMemoryCurrentStateTransactionCommitted, err, statErr)
		}
		return fmt.Errorf("persist current-state transaction marker: %w", err)
	}
	if err := m.currentStateTransactionFault("after_marker"); err != nil {
		return fmt.Errorf("%w: current-state transaction after marker: %v", errMemoryCurrentStateTransactionCommitted, err)
	}
	if m.memoryCurrentStateTransactionAfterMarker != nil {
		if err := m.memoryCurrentStateTransactionAfterMarker(); err != nil {
			return fmt.Errorf("%w: current-state transaction after marker: %v", errMemoryCurrentStateTransactionCommitted, err)
		}
	}
	if err := m.currentStateTransactionRollForwardShardsLocked(txn); err != nil {
		return fmt.Errorf("%w: current-state transaction roll-forward: %v", errMemoryCurrentStateTransactionCommitted, err)
	}
	if err := m.currentStateTransactionFault("after_manifest_rename"); err != nil {
		return fmt.Errorf("%w: current-state transaction after manifest rename: %v", errMemoryCurrentStateTransactionCommitted, err)
	}
	if m.memoryCurrentStateTransactionAfterManifestRename != nil {
		if err := m.memoryCurrentStateTransactionAfterManifestRename(); err != nil {
			return fmt.Errorf("%w: current-state transaction after manifest rename: %v", errMemoryCurrentStateTransactionCommitted, err)
		}
	}
	if err := m.currentStateTransactionFault("before_marker_removal"); err != nil {
		return fmt.Errorf("%w: current-state transaction before marker removal: %v", errMemoryCurrentStateTransactionCommitted, err)
	}
	if m.memoryCurrentStateTransactionBeforeMarkerRemoval != nil {
		if err := m.memoryCurrentStateTransactionBeforeMarkerRemoval(); err != nil {
			return fmt.Errorf("%w: current-state transaction before marker removal: %v", errMemoryCurrentStateTransactionCommitted, err)
		}
	}
	if err := os.Remove(m.currentStateTransactionPath()); err != nil {
		return fmt.Errorf("%w: remove current-state transaction marker: %v", errMemoryCurrentStateTransactionCommitted, err)
	}
	if err := syncOwnerOnlyDirectory(m.currentStateRootPath()); err != nil {
		return fmt.Errorf("%w: sync current-state transaction marker removal: %v", errMemoryCurrentStateTransactionCommitted, err)
	}
	if err := m.currentStateTransactionFault("after_marker_removal"); err != nil {
		return fmt.Errorf("%w: current-state transaction after marker removal: %v", errMemoryCurrentStateTransactionCommitted, err)
	}
	m.currentStateGenerationManifestRecordsInstalledLocked()
	return nil
}

func (m *memoryStore) currentStateTransactionRollForwardShardsLocked(txn memoryCurrentStateTransaction) error {
	for _, shard := range txn.Shards {
		stagePath, _ := m.currentStateTransactionAbsolutePath(shard.StagePath)
		finalPath, _ := m.currentStateTransactionAbsolutePath(shard.FinalPath)
		finalDigest, err := memoryCurrentStateTransactionReadDigest(finalPath)
		if err != nil {
			return err
		}
		if finalDigest == shard.NewDigest {
			if _, err := m.currentStateTransactionValidateArtifact(stagePath, shard.NewDigest, true); err != nil {
				return err
			}
			if _, err := os.Lstat(stagePath); err == nil {
				if err := os.Remove(stagePath); err != nil {
					return err
				}
			}
			continue
		}
		if finalDigest != shard.OldDigest {
			return fmt.Errorf("current-state shard %d changed before transaction rename", shard.Shard)
		}
		if _, err := m.currentStateTransactionValidateArtifact(stagePath, shard.NewDigest, false); err != nil {
			return err
		}
		if err := replaceOwnerOnlyFile(stagePath, finalPath); err != nil {
			return err
		}
		if err := syncOwnerOnlyDirectory(m.currentStateRootPath()); err != nil {
			return err
		}
		if err := m.currentStateTransactionFault("after_shard_rename"); err != nil {
			return fmt.Errorf("%w: current-state transaction after shard %d rename: %v", errMemoryCurrentStateTransactionCommitted, shard.Shard, err)
		}
		if m.memoryCurrentStateTransactionAfterShardRename != nil {
			if err := m.memoryCurrentStateTransactionAfterShardRename(shard.Shard); err != nil {
				return fmt.Errorf("%w: current-state transaction after shard %d rename: %v", errMemoryCurrentStateTransactionCommitted, shard.Shard, err)
			}
		}
	}
	if err := m.currentStateTransactionRollForwardCardsLocked(txn); err != nil {
		return err
	}
	if err := m.currentStateTransactionRollForwardExactStateLocked(txn); err != nil {
		return err
	}
	manifestStage, _ := m.currentStateTransactionAbsolutePath(txn.StageManifestPath)
	manifestFinal, _ := m.currentStateTransactionAbsolutePath(txn.FinalManifestPath)
	manifestDigest, err := memoryCurrentStateTransactionReadDigest(manifestFinal)
	if err != nil {
		return err
	}
	if manifestDigest == txn.NewManifestDigest {
		if _, err := m.currentStateTransactionValidateArtifact(manifestStage, txn.NewManifestDigest, true); err != nil {
			return err
		}
		if _, err := os.Lstat(manifestStage); err == nil {
			if err := os.Remove(manifestStage); err != nil {
				return err
			}
			return syncOwnerOnlyDirectory(m.currentStateRootPath())
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if manifestDigest != txn.OldManifestDigest {
		return errors.New("current-state generation manifest changed before transaction rename")
	}
	if _, err := m.currentStateTransactionValidateArtifact(manifestStage, txn.NewManifestDigest, false); err != nil {
		return err
	}
	if err := replaceOwnerOnlyFile(manifestStage, manifestFinal); err != nil {
		return err
	}
	return syncOwnerOnlyDirectory(m.currentStateRootPath())
}

func (m *memoryStore) currentStateGenerationManifestRecordsInstalledLocked() {
	if m == nil {
		return
	}
	m.currentStateGenerationManifestLoaded = true
	m.currentStateGenerationManifestVersion = memoryCurrentStateGenerationVersion
}
