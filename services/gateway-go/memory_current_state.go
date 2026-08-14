package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

const (
	memoryCurrentStateSchemaID           = "contextlattice_memory_current_state.v1"
	memoryCurrentStateShardCount         = 64
	memoryCurrentStateGenerationSchemaID = "contextlattice_memory_current_state_generation.v1"
	// Version two is retained for the indexed root digest encoding. Version
	// three changes only the durable manifest representation: the unbounded
	// project map moves to one bounded card per project.
	memoryCurrentStateGenerationDigestVersion = 2
	memoryCurrentStateGenerationVersion       = 3
	memoryCurrentStateGenerationCardsVersion  = 1
	// Digest version five is a canonical compressed Patricia commitment keyed by
	// the full project hash. Its authenticated skip edges bound every incremental
	// mutation to at most 257 branch/skip hashes, including insertion.
	// Version four is the preceding sparse commitment, version three the treap,
	// version two the fixed 256-bucket commitment, and version zero the older
	// XOR accumulator; all legacy roots are migration-only inputs.
	memoryCurrentStateGenerationCardsDigestVersion       = 5
	memoryCurrentStateGenerationCardsLegacySparseVersion = 4
	memoryCurrentStateGenerationCardsLegacyTreeVersion   = 3
	memoryCurrentStateGenerationCardsLegacyBucketVersion = 2
	memoryCurrentStateGenerationCardLegacyBucketCount    = 256
	memoryCurrentStateGenerationCardsDir                 = "generation-cards"
	memoryCurrentStateGenerationCardSchemaID             = "contextlattice_memory_current_state_generation_card.v1"
	memoryCurrentStateGenerationMaxCards                 = 100000
	memoryCurrentStateGenerationMaxCardBytes             = int64(64 * 1024 * 1024)
)

// readMemoryCurrentStateShard keeps startup recovery descriptor-bound and
// capped. Current-state shards are regular durable artifacts; rejecting a
// FIFO before opening it is deliberate so startup cannot hold the common edge
// fence on an uncancellable special-file read.
func readMemoryCurrentStateShard(path string, maxBytes int64) ([]byte, error) {
	if maxBytes < 1 {
		return nil, fmt.Errorf("current-state shard cap must be positive")
	}
	return readOwnerOnlyBoundedFile(path, maxBytes)
}

type memoryCurrentState struct {
	Entry     memoryStoreEntry `json:"entry"`
	LegalHold bool             `json:"legal_hold,omitempty"`
	Tombstone bool             `json:"tombstone,omitempty"`
}

type memoryCurrentStateShard struct {
	SchemaID string               `json:"schema_id"`
	Version  int                  `json:"version"`
	Shard    int                  `json:"shard"`
	Entries  []memoryCurrentState `json:"entries"`
}

type memoryCurrentStateGenerationRecord struct {
	KeyGeneration   uint64 `json:"key_generation"`
	TopicGeneration uint64 `json:"topic_generation"`
	StateDigest     string `json:"state_digest"`
}

type memoryCurrentStateGenerationManifest struct {
	SchemaID                  string                                        `json:"schema_id"`
	Version                   int                                           `json:"version"`
	StateDigest               string                                        `json:"state_digest"`
	ProjectCardsDir           string                                        `json:"project_cards_dir,omitempty"`
	ProjectCardsVersion       int                                           `json:"project_cards_version,omitempty"`
	ProjectCardsDigestVersion int                                           `json:"project_cards_digest_version,omitempty"`
	ProjectCardsCount         int                                           `json:"project_cards_count"`
	ProjectCardsDigest        string                                        `json:"project_cards_digest"`
	Projects                  map[string]memoryCurrentStateGenerationRecord `json:"projects,omitempty"`
}

type memoryCurrentStateGenerationCard struct {
	SchemaID string                             `json:"schema_id"`
	Version  int                                `json:"version"`
	Project  string                             `json:"project"`
	Record   memoryCurrentStateGenerationRecord `json:"record"`
}

type memoryCurrentStateDigestRow struct {
	Key   string             `json:"key"`
	State memoryCurrentState `json:"state"`
}

type memoryCurrentStateDigestRoot struct {
	SchemaID   string   `json:"schema_id"`
	Version    int      `json:"version"`
	Project    string   `json:"project,omitempty"`
	Generation uint64   `json:"generation,omitempty"`
	Leaves     []string `json:"leaves"`
}

func (m *memoryStore) currentStateGenerationPath() string {
	return filepath.Join(m.currentStateRootPath(), "generations.json")
}

func memoryCurrentStateDigest(states map[string]memoryCurrentState, projectKey string) string {
	keys := make([]string, 0, len(states))
	for key := range states {
		if projectKey != "" {
			project, _, ok := parseMemoryStoreKeyToken(key)
			if !ok || normalizeCurrentKeyIndexProject(project) != projectKey {
				continue
			}
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	rows := make([]memoryCurrentStateDigestRow, 0, len(keys))
	for _, key := range keys {
		state := states[key]
		state.Entry.Tags = append([]string(nil), state.Entry.Tags...)
		rows = append(rows, memoryCurrentStateDigestRow{Key: key, State: state})
	}
	return "sha256:" + sha256Hex(string(mustJSON(rows)))
}

// memoryCurrentStateRowsDigest is the canonical leaf digest used by the
// indexed current-state projection.  The caller supplies keys in canonical
// order; no unrelated state is copied or sorted here.
func memoryCurrentStateRowsDigest(rows []memoryCurrentStateDigestRow) (string, []byte, error) {
	if rows == nil {
		rows = []memoryCurrentStateDigestRow{}
	}
	payload, err := json.Marshal(rows)
	if err != nil {
		return "", nil, err
	}
	return "sha256:" + sha256Hex(string(payload)), payload, nil
}

func memoryCurrentStateEmptyShardDigest() string {
	digest, _, err := memoryCurrentStateRowsDigest(nil)
	if err != nil {
		return "sha256:" + strings.Repeat("0", 64)
	}
	return digest
}

func memoryCurrentStateRootDigest(project string, generation uint64, leaves map[int]string) string {
	orderedLeaves := make([]string, memoryCurrentStateShardCount)
	empty := memoryCurrentStateEmptyShardDigest()
	for shard := range orderedLeaves {
		orderedLeaves[shard] = empty
		if digest := strings.TrimSpace(leaves[shard]); digest != "" {
			orderedLeaves[shard] = digest
		}
	}
	root := memoryCurrentStateDigestRoot{
		SchemaID:   memoryCurrentStateGenerationSchemaID,
		Version:    memoryCurrentStateGenerationDigestVersion,
		Project:    normalizeCurrentKeyIndexProject(project),
		Generation: generation,
		Leaves:     orderedLeaves,
	}
	raw, err := json.Marshal(root)
	if err != nil {
		return "sha256:" + sha256Hex(fmt.Sprintf("root:%s:%d", project, generation))
	}
	return "sha256:" + sha256Hex(string(raw))
}

func memoryCurrentStateGenerationSeed(stateDigest string) uint64 {
	sum := sha256.Sum256([]byte(stateDigest))
	seed := binary.BigEndian.Uint64(sum[:8])
	if seed == 0 {
		return 1
	}
	return seed
}

func memoryCurrentStateGenerationDigestValid(value string) bool {
	value = strings.TrimSpace(strings.ToLower(value))
	return strings.HasPrefix(value, "sha256:") && len(strings.TrimPrefix(value, "sha256:")) == 64 && isHexDigest(strings.TrimPrefix(value, "sha256:"))
}

func memoryTagsHaveLegalHold(tags []string) bool {
	for _, raw := range tags {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "legal_hold", "legal-hold", "hold:legal", "retention:legal-hold":
			return true
		}
	}
	return false
}

func memoryTagsEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (m *memoryStore) currentStateRootPath() string {
	if m == nil {
		return ""
	}
	if path := strings.TrimSpace(m.policy.currentStatePath); path != "" {
		return filepath.Clean(path)
	}
	return filepath.Join(m.policy.rootPath, "_contextlattice", "memory_current_state")
}

func memoryCurrentStateShardForKey(key string) int {
	sum := sha256.Sum256([]byte(strings.TrimSpace(strings.ToLower(key))))
	return int(sum[0]) % memoryCurrentStateShardCount
}

func (m *memoryStore) currentStateShardPath(shard int) string {
	return filepath.Join(m.currentStateRootPath(), fmt.Sprintf("%02x.json", shard))
}

func memoryCurrentStateGenerationCardName(project string) string {
	projectKey := normalizeCurrentKeyIndexProject(project)
	return sha256Hex("contextlattice-generation-card:"+projectKey) + ".json"
}

func (m *memoryStore) currentStateGenerationCardsPath() string {
	return filepath.Join(m.currentStateRootPath(), memoryCurrentStateGenerationCardsDir)
}

func (m *memoryStore) currentStateGenerationCardPath(project string) string {
	return filepath.Join(m.currentStateGenerationCardsPath(), memoryCurrentStateGenerationCardName(project))
}

func memoryCurrentStateGenerationCardPayload(project string, record memoryCurrentStateGenerationRecord) ([]byte, error) {
	payload, err := json.Marshal(memoryCurrentStateGenerationCard{
		SchemaID: memoryCurrentStateGenerationCardSchemaID,
		Version:  memoryCurrentStateGenerationCardsVersion,
		Project:  normalizeCurrentKeyIndexProject(project),
		Record:   record,
	})
	if err != nil {
		return nil, err
	}
	payload = append(payload, '\n')
	if int64(len(payload)) > memoryEdgeLogMaxRecoveryBytes {
		return nil, fmt.Errorf("%w: current-state generation card bytes=%d cap=%d", errMemoryEdgeLogOversized, len(payload), memoryEdgeLogMaxRecoveryBytes)
	}
	return payload, nil
}

func memoryCurrentStateGenerationCardSetCapacity(records map[string]memoryCurrentStateGenerationRecord) (int, int64, error) {
	canonical := make(map[string]memoryCurrentStateGenerationRecord, len(records))
	for project, record := range records {
		project = normalizeCurrentKeyIndexProject(project)
		if project == "" {
			continue
		}
		canonical[project] = record
	}
	if len(canonical) > memoryCurrentStateGenerationMaxCards {
		return 0, 0, fmt.Errorf("current-state generation card count exceeds cap %d", memoryCurrentStateGenerationMaxCards)
	}
	var totalBytes int64
	for project, record := range canonical {
		payload, err := memoryCurrentStateGenerationCardPayload(project, record)
		if err != nil {
			return 0, 0, err
		}
		totalBytes += int64(len(payload))
		if totalBytes > memoryCurrentStateGenerationMaxCardBytes {
			return 0, 0, fmt.Errorf("current-state generation cards exceed byte cap %d", memoryCurrentStateGenerationMaxCardBytes)
		}
	}
	return len(canonical), totalBytes, nil
}

// preflightCurrentStateGenerationEntry checks the only project whose card a
// normal write can change before the file and history append happen. It is
// intentionally scalar on the v3 indexed path; uninitialized legacy state is
// the bounded startup fallback.
func (m *memoryStore) preflightCurrentStateGenerationEntry(project string) error {
	if m == nil {
		return errors.New("memory store unavailable")
	}
	project = normalizeCurrentKeyIndexProject(project)
	if project == "" {
		return errors.New("current-state generation project is empty")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.preflightCurrentStateGenerationEntryLocked(project)
}

func (m *memoryStore) preflightCurrentStateGenerationEntryLocked(project string) error {
	if m == nil {
		return errors.New("memory store unavailable")
	}
	if !m.currentStateGenerationCardsDigestInitialized {
		candidate := cloneCurrentStateGenerationRecords(m.currentStateGenerationRecords)
		if candidate == nil {
			candidate = map[string]memoryCurrentStateGenerationRecord{}
		}
		if len(candidate) == 0 && len(m.currentKeyIndexGeneration) > 0 {
			for projectKey, generation := range m.currentKeyIndexGeneration {
				projectKey = normalizeCurrentKeyIndexProject(projectKey)
				if projectKey == "" {
					continue
				}
				candidate[projectKey] = memoryCurrentStateGenerationRecord{
					KeyGeneration: generation, TopicGeneration: m.currentTopicIndexGeneration[projectKey],
					StateDigest: memoryCurrentStateRootDigest(projectKey, generation, m.currentStateProjectShardDigests[projectKey]),
				}
			}
		}
		if _, exists := candidate[project]; !exists {
			generation := m.currentKeyIndexGeneration[project]
			if generation == ^uint64(0) {
				return errors.New("current-state generation has reached its non-wrapping limit")
			}
			generation++
			candidate[project] = memoryCurrentStateGenerationRecord{KeyGeneration: generation, TopicGeneration: generation, StateDigest: memoryCurrentStateRootDigest(project, generation, m.currentStateProjectShardDigests[project])}
		} else {
			record := candidate[project]
			if record.KeyGeneration == ^uint64(0) {
				return errors.New("current-state generation has reached its non-wrapping limit")
			}
			record.KeyGeneration++
			record.TopicGeneration = record.KeyGeneration
			record.StateDigest = memoryCurrentStateRootDigest(project, record.KeyGeneration, m.currentStateProjectShardDigests[project])
			candidate[project] = record
		}
		_, _, err := memoryCurrentStateGenerationCardSetCapacity(candidate)
		return err
	}
	projectedCount := m.currentStateGenerationCardCount
	projectedBytes := m.currentStateGenerationCardBytes
	previous, previousExists := m.currentStateGenerationRecords[project]
	if previousExists {
		payload, err := memoryCurrentStateGenerationCardPayload(project, previous)
		if err != nil {
			return err
		}
		projectedBytes -= int64(len(payload))
	} else {
		projectedCount++
	}
	keyGeneration := m.currentKeyIndexGeneration[project]
	if keyGeneration == ^uint64(0) {
		return errors.New("current-state generation has reached its non-wrapping limit")
	}
	// addCurrentKeyLocked/recordEntryWithState advances the project
	// generation exactly once for the accepted write. Preflight the post-write
	// record, including decimal-boundary changes such as 9 -> 10, before the
	// file or history append occurs.
	keyGeneration++
	if keyGeneration == 0 {
		keyGeneration = 1
	}
	candidate := memoryCurrentStateGenerationRecord{
		KeyGeneration: keyGeneration, TopicGeneration: keyGeneration,
		StateDigest: memoryCurrentStateRootDigest(project, keyGeneration, m.currentStateProjectShardDigests[project]),
	}
	payload, err := memoryCurrentStateGenerationCardPayload(project, candidate)
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
	return nil
}

func memoryCurrentStateGenerationCardLeaf(project string, record memoryCurrentStateGenerationRecord) [32]byte {
	// Length-prefix every variable field so the leaf encoding is injective even
	// if a future project or digest alphabet changes. The project is normalized
	// before both the filename and commitment are derived.
	project = normalizeCurrentKeyIndexProject(project)
	payload := make([]byte, 0, 96+len(project)+len(record.StateDigest))
	payload = append(payload, "contextlattice-generation-card-leaf:v2"...)
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(project)))
	payload = append(payload, length[:]...)
	payload = append(payload, project...)
	binary.BigEndian.PutUint64(length[:], record.KeyGeneration)
	payload = append(payload, length[:]...)
	binary.BigEndian.PutUint64(length[:], record.TopicGeneration)
	payload = append(payload, length[:]...)
	binary.BigEndian.PutUint64(length[:], uint64(len(record.StateDigest)))
	payload = append(payload, length[:]...)
	payload = append(payload, record.StateDigest...)
	return sha256.Sum256(payload)
}

type memoryCurrentStateGenerationCardTreeNode struct {
	keyHash  [32]byte
	project  string
	record   memoryCurrentStateGenerationRecord
	left     *memoryCurrentStateGenerationCardTreeNode
	right    *memoryCurrentStateGenerationCardTreeNode
	priority uint64
	digest   [32]byte
}

var memoryCurrentStateGenerationCardTreeEmptyDigest = sha256.Sum256([]byte("contextlattice-generation-card-treap-empty:v2"))

func memoryCurrentStateGenerationCardTreeKey(project string) [32]byte {
	return sha256.Sum256([]byte("contextlattice-generation-card-key:v2:" + normalizeCurrentKeyIndexProject(project)))
}

func memoryCurrentStateGenerationCardTreeCompare(hash [32]byte, project string, node *memoryCurrentStateGenerationCardTreeNode) int {
	if order := bytes.Compare(hash[:], node.keyHash[:]); order != 0 {
		return order
	}
	return strings.Compare(project, node.project)
}

func memoryCurrentStateGenerationCardTreeDigest(node *memoryCurrentStateGenerationCardTreeNode) [32]byte {
	if node == nil {
		return memoryCurrentStateGenerationCardTreeEmptyDigest
	}
	return node.digest
}

func memoryCurrentStateGenerationCardTreeRefresh(node *memoryCurrentStateGenerationCardTreeNode) {
	if node == nil {
		return
	}
	encoded := make([]byte, 0, 128)
	encoded = append(encoded, "contextlattice-generation-card-treap-node:v2"...)
	var value [8]byte
	binary.BigEndian.PutUint64(value[:], node.priority)
	encoded = append(encoded, value[:]...)
	leftDigest := memoryCurrentStateGenerationCardTreeDigest(node.left)
	rightDigest := memoryCurrentStateGenerationCardTreeDigest(node.right)
	encoded = append(encoded, leftDigest[:]...)
	encoded = append(encoded, rightDigest[:]...)
	leaf := memoryCurrentStateGenerationCardLeaf(node.project, node.record)
	encoded = append(encoded, node.keyHash[:]...)
	encoded = append(encoded, leaf[:]...)
	node.digest = sha256.Sum256(encoded)
}

func memoryCurrentStateGenerationCardTreeRotateRight(node *memoryCurrentStateGenerationCardTreeNode) *memoryCurrentStateGenerationCardTreeNode {
	child := node.left
	node.left = child.right
	child.right = node
	memoryCurrentStateGenerationCardTreeRefresh(node)
	memoryCurrentStateGenerationCardTreeRefresh(child)
	return child
}

func memoryCurrentStateGenerationCardTreeRotateLeft(node *memoryCurrentStateGenerationCardTreeNode) *memoryCurrentStateGenerationCardTreeNode {
	child := node.right
	node.right = child.left
	child.left = node
	memoryCurrentStateGenerationCardTreeRefresh(node)
	memoryCurrentStateGenerationCardTreeRefresh(child)
	return child
}

func memoryCurrentStateGenerationCardTreePriority(hash [32]byte) uint64 {
	priority := binary.BigEndian.Uint64(hash[:8])
	if priority == 0 {
		return 1
	}
	return priority
}

func memoryCurrentStateGenerationCardTreeHigherPriority(left, right *memoryCurrentStateGenerationCardTreeNode) bool {
	if left.priority != right.priority {
		return left.priority > right.priority
	}
	return bytes.Compare(left.keyHash[:], right.keyHash[:]) > 0
}

// memoryCurrentStateGenerationCardTreeInsert is a deterministic treap. The
// full-hash-derived priority makes the Cartesian tree shape independent of
// map iteration or replay order while preserving logarithmic expected update
// work; no caller-controlled prefix bucket can concentrate updates.
func memoryCurrentStateGenerationCardTreeInsert(node *memoryCurrentStateGenerationCardTreeNode, hash [32]byte, project string, record memoryCurrentStateGenerationRecord) *memoryCurrentStateGenerationCardTreeNode {
	if node == nil {
		node = &memoryCurrentStateGenerationCardTreeNode{keyHash: hash, project: project, record: record, priority: memoryCurrentStateGenerationCardTreePriority(hash)}
		memoryCurrentStateGenerationCardTreeRefresh(node)
		return node
	}
	if order := memoryCurrentStateGenerationCardTreeCompare(hash, project, node); order < 0 {
		node.left = memoryCurrentStateGenerationCardTreeInsert(node.left, hash, project, record)
	} else if order > 0 {
		node.right = memoryCurrentStateGenerationCardTreeInsert(node.right, hash, project, record)
	} else {
		node.record = record
	}
	if node.left != nil && memoryCurrentStateGenerationCardTreeHigherPriority(node.left, node) {
		return memoryCurrentStateGenerationCardTreeRotateRight(node)
	}
	if node.right != nil && memoryCurrentStateGenerationCardTreeHigherPriority(node.right, node) {
		return memoryCurrentStateGenerationCardTreeRotateLeft(node)
	}
	memoryCurrentStateGenerationCardTreeRefresh(node)
	return node
}

func memoryCurrentStateGenerationCardTreeFind(node *memoryCurrentStateGenerationCardTreeNode, hash [32]byte, project string) (*memoryCurrentStateGenerationCardTreeNode, bool) {
	for node != nil {
		order := memoryCurrentStateGenerationCardTreeCompare(hash, project, node)
		if order == 0 {
			return node, true
		}
		if order < 0 {
			node = node.left
		} else {
			node = node.right
		}
	}
	return nil, false
}

func memoryCurrentStateGenerationCardTreeDelete(node *memoryCurrentStateGenerationCardTreeNode, hash [32]byte, project string) *memoryCurrentStateGenerationCardTreeNode {
	if node == nil {
		return nil
	}
	order := memoryCurrentStateGenerationCardTreeCompare(hash, project, node)
	if order < 0 {
		node.left = memoryCurrentStateGenerationCardTreeDelete(node.left, hash, project)
	} else if order > 0 {
		node.right = memoryCurrentStateGenerationCardTreeDelete(node.right, hash, project)
	} else {
		if node.left == nil {
			return node.right
		}
		if node.right == nil {
			return node.left
		}
		if memoryCurrentStateGenerationCardTreeHigherPriority(node.left, node.right) {
			node = memoryCurrentStateGenerationCardTreeRotateRight(node)
			node.right = memoryCurrentStateGenerationCardTreeDelete(node.right, hash, project)
		} else {
			node = memoryCurrentStateGenerationCardTreeRotateLeft(node)
			node.left = memoryCurrentStateGenerationCardTreeDelete(node.left, hash, project)
		}
	}
	memoryCurrentStateGenerationCardTreeRefresh(node)
	return node
}

func sortMemoryCurrentStateGenerationCardTreeProjects(projects []string) {
	sort.Slice(projects, func(left, right int) bool {
		leftHash := memoryCurrentStateGenerationCardTreeKey(projects[left])
		rightHash := memoryCurrentStateGenerationCardTreeKey(projects[right])
		if order := bytes.Compare(leftHash[:], rightHash[:]); order != 0 {
			return order < 0
		}
		return projects[left] < projects[right]
	})
}

// memoryCurrentStateGenerationCardTreeBuildCanonical constructs the same
// deterministic Cartesian tree as repeated treap insertion, but in one
// linear pass over key-sorted projects. This is bounded initial
// migration/recovery work and avoids O(P log P) startup allocations for the
// 100,000-card ceiling; hot writes still update one logarithmic path.
func memoryCurrentStateGenerationCardTreeBuildCanonical(projects []string, records map[string]memoryCurrentStateGenerationRecord) *memoryCurrentStateGenerationCardTreeNode {
	if len(projects) == 0 {
		return nil
	}
	stack := make([]*memoryCurrentStateGenerationCardTreeNode, 0, 64)
	for _, project := range projects {
		node := &memoryCurrentStateGenerationCardTreeNode{
			keyHash:  memoryCurrentStateGenerationCardTreeKey(project),
			project:  project,
			record:   records[project],
			priority: memoryCurrentStateGenerationCardTreePriority(memoryCurrentStateGenerationCardTreeKey(project)),
		}
		var last *memoryCurrentStateGenerationCardTreeNode
		for len(stack) > 0 && memoryCurrentStateGenerationCardTreeHigherPriority(node, stack[len(stack)-1]) {
			last = stack[len(stack)-1]
			stack = stack[:len(stack)-1]
		}
		node.left = last
		if len(stack) > 0 {
			stack[len(stack)-1].right = node
		}
		stack = append(stack, node)
	}
	root := stack[0]
	var refresh func(*memoryCurrentStateGenerationCardTreeNode)
	refresh = func(node *memoryCurrentStateGenerationCardTreeNode) {
		if node == nil {
			return
		}
		refresh(node.left)
		refresh(node.right)
		memoryCurrentStateGenerationCardTreeRefresh(node)
	}
	refresh(root)
	return root
}

const memoryCurrentStateGenerationCardSparseDepth = 256

// memoryCurrentStateGenerationCardSparseNode is a compressed sparse-Merkle
// Patricia node. Unary paths are derived from the fixed empty hashes instead
// of being stored, so memory is O(P) while every update still rehashes a
// fixed 256-bit path. Branch depth is the first differing full-hash bit.
type memoryCurrentStateGenerationCardSparseNode struct {
	keyHash [32]byte
	project string
	record  memoryCurrentStateGenerationRecord
	depth   uint16
	leaf    bool
	left    *memoryCurrentStateGenerationCardSparseNode
	right   *memoryCurrentStateGenerationCardSparseNode
	digest  [32]byte
	// cachedDigest is the subtree digest expanded to the one parent context
	// that currently consumes this node. Patricia compression omits unary
	// levels, so retaining that parent-context value prevents an unchanged
	// sibling from rehashing its omitted levels on every ancestor refresh.
	cachedStartDepth  uint16
	cachedDigest      [32]byte
	cachedDigestValid bool
}

func memoryCurrentStateGenerationCardSparseKey(project string) [32]byte {
	return sha256.Sum256([]byte("contextlattice-generation-card-key:v4:" + normalizeCurrentKeyIndexProject(project)))
}

func memoryCurrentStateGenerationCardSparseBranchHashCount(hashCount ...*int) {
	if len(hashCount) == 0 || hashCount[0] == nil {
		return
	}
	(*hashCount[0])++
}

func memoryCurrentStateGenerationCardSparseBranchDigest(depth int, left, right [32]byte, hashCount ...*int) [32]byte {
	memoryCurrentStateGenerationCardSparseBranchHashCount(hashCount...)
	payload := make([]byte, 0, 64+64)
	payload = append(payload, "contextlattice-generation-card-sparse-branch:v1"...)
	payload = append(payload, byte(depth>>8), byte(depth))
	payload = append(payload, left[:]...)
	payload = append(payload, right[:]...)
	return sha256.Sum256(payload)
}

var memoryCurrentStateGenerationCardSparseEmptyDigests = func() [memoryCurrentStateGenerationCardSparseDepth + 1][32]byte {
	var empty [memoryCurrentStateGenerationCardSparseDepth + 1][32]byte
	empty[memoryCurrentStateGenerationCardSparseDepth] = sha256.Sum256([]byte("contextlattice-generation-card-sparse-empty-leaf:v1"))
	for depth := memoryCurrentStateGenerationCardSparseDepth - 1; depth >= 0; depth-- {
		empty[depth] = memoryCurrentStateGenerationCardSparseBranchDigest(depth, empty[depth+1], empty[depth+1])
	}
	return empty
}()

func memoryCurrentStateGenerationCardSparseBit(hash [32]byte, depth int) byte {
	return (hash[depth/8] >> uint(7-(depth%8))) & 1
}

func memoryCurrentStateGenerationCardSparseFirstDifference(left, right [32]byte) int {
	for depth := 0; depth < memoryCurrentStateGenerationCardSparseDepth; depth++ {
		if memoryCurrentStateGenerationCardSparseBit(left, depth) != memoryCurrentStateGenerationCardSparseBit(right, depth) {
			return depth
		}
	}
	return memoryCurrentStateGenerationCardSparseDepth
}

func memoryCurrentStateGenerationCardSparseRepresentativeHash(node *memoryCurrentStateGenerationCardSparseNode) [32]byte {
	for node != nil && !node.leaf {
		node = node.left
	}
	if node == nil {
		return [32]byte{}
	}
	return node.keyHash
}

func memoryCurrentStateGenerationCardSparseDigestAt(node *memoryCurrentStateGenerationCardSparseNode, startDepth int, hashCount ...*int) [32]byte {
	if startDepth < 0 {
		startDepth = 0
	}
	if startDepth > memoryCurrentStateGenerationCardSparseDepth {
		return memoryCurrentStateGenerationCardSparseEmptyDigests[memoryCurrentStateGenerationCardSparseDepth]
	}
	if node == nil {
		return memoryCurrentStateGenerationCardSparseEmptyDigests[startDepth]
	}
	if startDepth >= int(node.depth) {
		return node.digest
	}
	if node.cachedDigestValid && int(node.cachedStartDepth) == startDepth {
		return node.cachedDigest
	}
	digest := node.digest
	representative := memoryCurrentStateGenerationCardSparseRepresentativeHash(node)
	for depth := int(node.depth) - 1; depth >= startDepth; depth-- {
		empty := memoryCurrentStateGenerationCardSparseEmptyDigests[depth+1]
		if memoryCurrentStateGenerationCardSparseBit(representative, depth) == 0 {
			digest = memoryCurrentStateGenerationCardSparseBranchDigest(depth, digest, empty, hashCount...)
		} else {
			digest = memoryCurrentStateGenerationCardSparseBranchDigest(depth, empty, digest, hashCount...)
		}
	}
	node.cachedStartDepth = uint16(startDepth)
	node.cachedDigest = digest
	node.cachedDigestValid = true
	return digest
}

func memoryCurrentStateGenerationCardSparseLeafDigest(project string, record memoryCurrentStateGenerationRecord) [32]byte {
	keyHash := memoryCurrentStateGenerationCardSparseKey(project)
	cardLeaf := memoryCurrentStateGenerationCardLeaf(project, record)
	payload := make([]byte, 0, 96)
	payload = append(payload, "contextlattice-generation-card-sparse-leaf:v1"...)
	payload = append(payload, keyHash[:]...)
	payload = append(payload, cardLeaf[:]...)
	return sha256.Sum256(payload)
}

func memoryCurrentStateGenerationCardSparseRefresh(node *memoryCurrentStateGenerationCardSparseNode, hashCount ...*int) {
	if node == nil {
		return
	}
	node.cachedDigestValid = false
	if node.leaf {
		node.digest = memoryCurrentStateGenerationCardSparseLeafDigest(node.project, node.record)
		return
	}
	depth := int(node.depth)
	left := memoryCurrentStateGenerationCardSparseDigestAt(node.left, depth+1, hashCount...)
	right := memoryCurrentStateGenerationCardSparseDigestAt(node.right, depth+1, hashCount...)
	node.digest = memoryCurrentStateGenerationCardSparseBranchDigest(depth, left, right, hashCount...)
}

func memoryCurrentStateGenerationCardSparseLeaf(hash [32]byte, project string, record memoryCurrentStateGenerationRecord, hashCount ...*int) *memoryCurrentStateGenerationCardSparseNode {
	node := &memoryCurrentStateGenerationCardSparseNode{keyHash: hash, project: project, record: record, depth: memoryCurrentStateGenerationCardSparseDepth, leaf: true}
	memoryCurrentStateGenerationCardSparseRefresh(node, hashCount...)
	return node
}

func memoryCurrentStateGenerationCardSparseBranch(depth int, left, right *memoryCurrentStateGenerationCardSparseNode, hashCount ...*int) *memoryCurrentStateGenerationCardSparseNode {
	node := &memoryCurrentStateGenerationCardSparseNode{depth: uint16(depth), left: left, right: right}
	memoryCurrentStateGenerationCardSparseRefresh(node, hashCount...)
	return node
}

func memoryCurrentStateGenerationCardSparseInsert(node *memoryCurrentStateGenerationCardSparseNode, hash [32]byte, project string, record memoryCurrentStateGenerationRecord, hashCount ...*int) (*memoryCurrentStateGenerationCardSparseNode, error) {
	if node == nil {
		return memoryCurrentStateGenerationCardSparseLeaf(hash, project, record, hashCount...), nil
	}
	if node.leaf {
		if node.keyHash == hash {
			if node.project != project {
				return nil, fmt.Errorf("current-state generation project hash collision between %q and %q", node.project, project)
			}
			node.record = record
			memoryCurrentStateGenerationCardSparseRefresh(node, hashCount...)
			return node, nil
		}
		difference := memoryCurrentStateGenerationCardSparseFirstDifference(hash, node.keyHash)
		newLeaf := memoryCurrentStateGenerationCardSparseLeaf(hash, project, record, hashCount...)
		if memoryCurrentStateGenerationCardSparseBit(hash, difference) == 0 {
			return memoryCurrentStateGenerationCardSparseBranch(difference, newLeaf, node, hashCount...), nil
		}
		return memoryCurrentStateGenerationCardSparseBranch(difference, node, newLeaf, hashCount...), nil
	}
	difference := memoryCurrentStateGenerationCardSparseFirstDifference(hash, memoryCurrentStateGenerationCardSparseRepresentativeHash(node))
	if difference < int(node.depth) {
		newLeaf := memoryCurrentStateGenerationCardSparseLeaf(hash, project, record, hashCount...)
		if memoryCurrentStateGenerationCardSparseBit(hash, difference) == 0 {
			return memoryCurrentStateGenerationCardSparseBranch(difference, newLeaf, node, hashCount...), nil
		}
		return memoryCurrentStateGenerationCardSparseBranch(difference, node, newLeaf, hashCount...), nil
	}
	if memoryCurrentStateGenerationCardSparseBit(hash, int(node.depth)) == 0 {
		updated, err := memoryCurrentStateGenerationCardSparseInsert(node.left, hash, project, record, hashCount...)
		if err != nil {
			return nil, err
		}
		node.left = updated
	} else {
		updated, err := memoryCurrentStateGenerationCardSparseInsert(node.right, hash, project, record, hashCount...)
		if err != nil {
			return nil, err
		}
		node.right = updated
	}
	memoryCurrentStateGenerationCardSparseRefresh(node, hashCount...)
	return node, nil
}

func memoryCurrentStateGenerationCardSparseDelete(node *memoryCurrentStateGenerationCardSparseNode, hash [32]byte, project string, hashCount ...*int) (*memoryCurrentStateGenerationCardSparseNode, bool, error) {
	if node == nil {
		return nil, false, nil
	}
	if node.leaf {
		if node.keyHash != hash {
			return node, false, nil
		}
		if node.project != project {
			return nil, false, fmt.Errorf("current-state generation project hash collision between %q and %q", node.project, project)
		}
		return nil, true, nil
	}
	var removed bool
	var err error
	if memoryCurrentStateGenerationCardSparseBit(hash, int(node.depth)) == 0 {
		node.left, removed, err = memoryCurrentStateGenerationCardSparseDelete(node.left, hash, project, hashCount...)
	} else {
		node.right, removed, err = memoryCurrentStateGenerationCardSparseDelete(node.right, hash, project, hashCount...)
	}
	if err != nil || !removed {
		return node, removed, err
	}
	if node.left == nil {
		return node.right, true, nil
	}
	if node.right == nil {
		return node.left, true, nil
	}
	memoryCurrentStateGenerationCardSparseRefresh(node, hashCount...)
	return node, true, nil
}

func memoryCurrentStateGenerationCardSparseDigest(node *memoryCurrentStateGenerationCardSparseNode, hashCount ...*int) [32]byte {
	return memoryCurrentStateGenerationCardSparseDigestAt(node, 0, hashCount...)
}

func memoryCurrentStateGenerationCardTreeBuildSparse(projects []string, records map[string]memoryCurrentStateGenerationRecord, hashCount ...*int) (*memoryCurrentStateGenerationCardSparseNode, error) {
	var root *memoryCurrentStateGenerationCardSparseNode
	for _, project := range projects {
		var err error
		root, err = memoryCurrentStateGenerationCardSparseInsert(root, memoryCurrentStateGenerationCardSparseKey(project), project, records[project], hashCount...)
		if err != nil {
			return nil, err
		}
	}
	return root, nil
}

// memoryCurrentStateGenerationCardPatriciaNode is the live card commitment.
// Unlike the migration-only sparse implementation above, a unary path is
// represented by one authenticated skip edge. Consequently a mutation never
// expands both compressed leaves into 256 empty levels when a new branch is
// inserted. The exact fixed work contract is declared below.
type memoryCurrentStateGenerationCardPatriciaNode struct {
	keyHash [32]byte
	project string
	record  memoryCurrentStateGenerationRecord
	depth   uint16
	leaf    bool
	left    *memoryCurrentStateGenerationCardPatriciaNode
	right   *memoryCurrentStateGenerationCardPatriciaNode
	digest  [32]byte
	// Each child is normally consumed at one parent depth. Keeping that
	// context digest makes an unchanged sibling free on later hot updates.
	cachedStartDepth  uint16
	cachedDigest      [32]byte
	cachedDigestValid bool
}

const (
	memoryCurrentStateGenerationCardPatriciaDepth = 256
	// A new split can hash one branch/skip per prefix bit plus both child skip
	// edges. The tight maximum is therefore depth+1: a split at depth 254 with
	// existing branches at depths 0..253 performs 254 ancestor branch hashes,
	// one new branch hash, and two child skip hashes.
	memoryCurrentStateGenerationCardPatriciaMaxMutationHashes = memoryCurrentStateGenerationCardPatriciaDepth + 1
)

var memoryCurrentStateGenerationCardPatriciaEmptyDigest = sha256.Sum256([]byte("contextlattice-generation-card-patricia-empty:v1"))

func memoryCurrentStateGenerationCardPatriciaKey(project string) [32]byte {
	return sha256.Sum256([]byte("contextlattice-generation-card-key:v5:" + normalizeCurrentKeyIndexProject(project)))
}

func memoryCurrentStateGenerationCardPatriciaHashCount(hashCount ...*int) {
	if len(hashCount) == 0 || hashCount[0] == nil {
		return
	}
	(*hashCount[0])++
}

func memoryCurrentStateGenerationCardPatriciaSkipDigest(startDepth, endDepth int, keyHash, child [32]byte, hashCount ...*int) [32]byte {
	memoryCurrentStateGenerationCardPatriciaHashCount(hashCount...)
	payload := make([]byte, 0, 128)
	payload = append(payload, "contextlattice-generation-card-patricia-skip:v1"...)
	payload = append(payload, byte(startDepth>>8), byte(startDepth), byte(endDepth>>8), byte(endDepth))
	payload = append(payload, keyHash[:]...)
	payload = append(payload, child[:]...)
	return sha256.Sum256(payload)
}

func memoryCurrentStateGenerationCardPatriciaBranchDigest(depth int, left, right [32]byte, hashCount ...*int) [32]byte {
	memoryCurrentStateGenerationCardPatriciaHashCount(hashCount...)
	payload := make([]byte, 0, 128)
	payload = append(payload, "contextlattice-generation-card-patricia-branch:v1"...)
	payload = append(payload, byte(depth>>8), byte(depth))
	payload = append(payload, left[:]...)
	payload = append(payload, right[:]...)
	return sha256.Sum256(payload)
}

func memoryCurrentStateGenerationCardPatriciaBit(hash [32]byte, depth int) byte {
	return (hash[depth/8] >> uint(7-(depth%8))) & 1
}

func memoryCurrentStateGenerationCardPatriciaFirstDifference(left, right [32]byte) int {
	for depth := 0; depth < memoryCurrentStateGenerationCardPatriciaDepth; depth++ {
		if memoryCurrentStateGenerationCardPatriciaBit(left, depth) != memoryCurrentStateGenerationCardPatriciaBit(right, depth) {
			return depth
		}
	}
	return memoryCurrentStateGenerationCardPatriciaDepth
}

func memoryCurrentStateGenerationCardPatriciaRepresentativeHash(node *memoryCurrentStateGenerationCardPatriciaNode) [32]byte {
	for node != nil && !node.leaf {
		node = node.left
	}
	if node == nil {
		return [32]byte{}
	}
	return node.keyHash
}

func memoryCurrentStateGenerationCardPatriciaLeafDigest(keyHash [32]byte, project string, record memoryCurrentStateGenerationRecord) [32]byte {
	cardLeaf := memoryCurrentStateGenerationCardLeaf(project, record)
	payload := make([]byte, 0, 128)
	payload = append(payload, "contextlattice-generation-card-patricia-leaf:v1"...)
	payload = append(payload, keyHash[:]...)
	payload = append(payload, cardLeaf[:]...)
	return sha256.Sum256(payload)
}

func memoryCurrentStateGenerationCardPatriciaDigestAt(node *memoryCurrentStateGenerationCardPatriciaNode, startDepth int, hashCount ...*int) [32]byte {
	if startDepth < 0 {
		startDepth = 0
	}
	if startDepth > memoryCurrentStateGenerationCardPatriciaDepth {
		return memoryCurrentStateGenerationCardPatriciaEmptyDigest
	}
	if node == nil {
		return memoryCurrentStateGenerationCardPatriciaEmptyDigest
	}
	if startDepth >= int(node.depth) {
		return node.digest
	}
	if node.cachedDigestValid && int(node.cachedStartDepth) == startDepth {
		return node.cachedDigest
	}
	digest := memoryCurrentStateGenerationCardPatriciaSkipDigest(startDepth, int(node.depth), memoryCurrentStateGenerationCardPatriciaRepresentativeHash(node), node.digest, hashCount...)
	node.cachedStartDepth = uint16(startDepth)
	node.cachedDigest = digest
	node.cachedDigestValid = true
	return digest
}

func memoryCurrentStateGenerationCardPatriciaRefresh(node *memoryCurrentStateGenerationCardPatriciaNode, hashCount ...*int) {
	if node == nil {
		return
	}
	node.cachedDigestValid = false
	if node.leaf {
		node.digest = memoryCurrentStateGenerationCardPatriciaLeafDigest(node.keyHash, node.project, node.record)
		return
	}
	depth := int(node.depth)
	left := memoryCurrentStateGenerationCardPatriciaDigestAt(node.left, depth+1, hashCount...)
	right := memoryCurrentStateGenerationCardPatriciaDigestAt(node.right, depth+1, hashCount...)
	node.digest = memoryCurrentStateGenerationCardPatriciaBranchDigest(depth, left, right, hashCount...)
}

func memoryCurrentStateGenerationCardPatriciaLeaf(hash [32]byte, project string, record memoryCurrentStateGenerationRecord, hashCount ...*int) *memoryCurrentStateGenerationCardPatriciaNode {
	node := &memoryCurrentStateGenerationCardPatriciaNode{keyHash: hash, project: project, record: record, depth: memoryCurrentStateGenerationCardPatriciaDepth, leaf: true}
	memoryCurrentStateGenerationCardPatriciaRefresh(node, hashCount...)
	return node
}

func memoryCurrentStateGenerationCardPatriciaBranch(depth int, left, right *memoryCurrentStateGenerationCardPatriciaNode, hashCount ...*int) *memoryCurrentStateGenerationCardPatriciaNode {
	node := &memoryCurrentStateGenerationCardPatriciaNode{depth: uint16(depth), left: left, right: right}
	memoryCurrentStateGenerationCardPatriciaRefresh(node, hashCount...)
	return node
}

func memoryCurrentStateGenerationCardPatriciaInsert(node *memoryCurrentStateGenerationCardPatriciaNode, hash [32]byte, project string, record memoryCurrentStateGenerationRecord, hashCount ...*int) (*memoryCurrentStateGenerationCardPatriciaNode, error) {
	if node == nil {
		return memoryCurrentStateGenerationCardPatriciaLeaf(hash, project, record, hashCount...), nil
	}
	if node.leaf {
		if node.keyHash == hash {
			if node.project != project {
				return nil, fmt.Errorf("current-state generation project hash collision between %q and %q", node.project, project)
			}
			node.record = record
			memoryCurrentStateGenerationCardPatriciaRefresh(node, hashCount...)
			return node, nil
		}
		difference := memoryCurrentStateGenerationCardPatriciaFirstDifference(hash, node.keyHash)
		if difference >= memoryCurrentStateGenerationCardPatriciaDepth {
			return nil, fmt.Errorf("current-state generation project hash collision between %q and %q", node.project, project)
		}
		newLeaf := memoryCurrentStateGenerationCardPatriciaLeaf(hash, project, record, hashCount...)
		if memoryCurrentStateGenerationCardPatriciaBit(hash, difference) == 0 {
			return memoryCurrentStateGenerationCardPatriciaBranch(difference, newLeaf, node, hashCount...), nil
		}
		return memoryCurrentStateGenerationCardPatriciaBranch(difference, node, newLeaf, hashCount...), nil
	}
	difference := memoryCurrentStateGenerationCardPatriciaFirstDifference(hash, memoryCurrentStateGenerationCardPatriciaRepresentativeHash(node))
	if difference < int(node.depth) {
		newLeaf := memoryCurrentStateGenerationCardPatriciaLeaf(hash, project, record, hashCount...)
		if memoryCurrentStateGenerationCardPatriciaBit(hash, difference) == 0 {
			return memoryCurrentStateGenerationCardPatriciaBranch(difference, newLeaf, node, hashCount...), nil
		}
		return memoryCurrentStateGenerationCardPatriciaBranch(difference, node, newLeaf, hashCount...), nil
	}
	if int(node.depth) >= memoryCurrentStateGenerationCardPatriciaDepth {
		return nil, fmt.Errorf("current-state generation project hash collision for %q", project)
	}
	if memoryCurrentStateGenerationCardPatriciaBit(hash, int(node.depth)) == 0 {
		updated, err := memoryCurrentStateGenerationCardPatriciaInsert(node.left, hash, project, record, hashCount...)
		if err != nil {
			return nil, err
		}
		node.left = updated
	} else {
		updated, err := memoryCurrentStateGenerationCardPatriciaInsert(node.right, hash, project, record, hashCount...)
		if err != nil {
			return nil, err
		}
		node.right = updated
	}
	memoryCurrentStateGenerationCardPatriciaRefresh(node, hashCount...)
	return node, nil
}

func memoryCurrentStateGenerationCardPatriciaDelete(node *memoryCurrentStateGenerationCardPatriciaNode, hash [32]byte, project string, hashCount ...*int) (*memoryCurrentStateGenerationCardPatriciaNode, bool, error) {
	if node == nil {
		return nil, false, nil
	}
	if node.leaf {
		if node.keyHash != hash {
			return node, false, nil
		}
		if node.project != project {
			return nil, false, fmt.Errorf("current-state generation project hash collision between %q and %q", node.project, project)
		}
		return nil, true, nil
	}
	var removed bool
	var err error
	if int(node.depth) >= memoryCurrentStateGenerationCardPatriciaDepth {
		return node, false, nil
	}
	if memoryCurrentStateGenerationCardPatriciaBit(hash, int(node.depth)) == 0 {
		node.left, removed, err = memoryCurrentStateGenerationCardPatriciaDelete(node.left, hash, project, hashCount...)
	} else {
		node.right, removed, err = memoryCurrentStateGenerationCardPatriciaDelete(node.right, hash, project, hashCount...)
	}
	if err != nil || !removed {
		return node, removed, err
	}
	if node.left == nil {
		return node.right, true, nil
	}
	if node.right == nil {
		return node.left, true, nil
	}
	memoryCurrentStateGenerationCardPatriciaRefresh(node, hashCount...)
	return node, true, nil
}

func memoryCurrentStateGenerationCardPatriciaDigest(node *memoryCurrentStateGenerationCardPatriciaNode, hashCount ...*int) [32]byte {
	return memoryCurrentStateGenerationCardPatriciaDigestAt(node, 0, hashCount...)
}

func memoryCurrentStateGenerationCardTreeBuildPatricia(projects []string, records map[string]memoryCurrentStateGenerationRecord, hashCount ...*int) (*memoryCurrentStateGenerationCardPatriciaNode, error) {
	var root *memoryCurrentStateGenerationCardPatriciaNode
	for _, project := range projects {
		var err error
		root, err = memoryCurrentStateGenerationCardPatriciaInsert(root, memoryCurrentStateGenerationCardPatriciaKey(project), project, records[project], hashCount...)
		if err != nil {
			return nil, err
		}
	}
	return root, nil
}

func memoryCurrentStateGenerationCardsLegacySparseDigest(records map[string]memoryCurrentStateGenerationRecord) (int, string) {
	projects := make([]string, 0, len(records))
	canonical := make(map[string]memoryCurrentStateGenerationRecord, len(records))
	for project, record := range records {
		project = normalizeCurrentKeyIndexProject(project)
		if project == "" {
			continue
		}
		if _, exists := canonical[project]; !exists {
			projects = append(projects, project)
		}
		canonical[project] = record
	}
	sortMemoryCurrentStateGenerationCardTreeProjects(projects)
	root, err := memoryCurrentStateGenerationCardTreeBuildSparse(projects, canonical)
	if err != nil {
		return len(projects), ""
	}
	digest := memoryCurrentStateGenerationCardSparseDigest(root)
	return len(projects), "sha256:" + hex.EncodeToString(digest[:])
}

func memoryCurrentStateGenerationCardsDigest(records map[string]memoryCurrentStateGenerationRecord) (int, string) {
	projects := make([]string, 0, len(records))
	canonical := make(map[string]memoryCurrentStateGenerationRecord, len(records))
	for project, record := range records {
		project = normalizeCurrentKeyIndexProject(project)
		if project == "" {
			continue
		}
		if _, exists := canonical[project]; !exists {
			projects = append(projects, project)
		}
		canonical[project] = record
	}
	sort.Slice(projects, func(left, right int) bool {
		leftHash := memoryCurrentStateGenerationCardPatriciaKey(projects[left])
		rightHash := memoryCurrentStateGenerationCardPatriciaKey(projects[right])
		if order := bytes.Compare(leftHash[:], rightHash[:]); order != 0 {
			return order < 0
		}
		return projects[left] < projects[right]
	})
	root, err := memoryCurrentStateGenerationCardTreeBuildPatricia(projects, canonical)
	if err != nil {
		return len(projects), ""
	}
	digest := memoryCurrentStateGenerationCardPatriciaDigest(root)
	return len(projects), "sha256:" + hex.EncodeToString(digest[:])
}

// memoryCurrentStateGenerationCardsLegacyTreeDigest verifies the v3 treap
// commitment emitted by the previous bounded-card implementation. It is a
// migration-only reader; live accumulators use the Patricia root above and
// never rebuild this treap on a write.
func memoryCurrentStateGenerationCardsLegacyTreeDigest(records map[string]memoryCurrentStateGenerationRecord) (int, string) {
	projects := make([]string, 0, len(records))
	canonical := make(map[string]memoryCurrentStateGenerationRecord, len(records))
	for project, record := range records {
		project = normalizeCurrentKeyIndexProject(project)
		if project == "" {
			continue
		}
		if _, exists := canonical[project]; !exists {
			projects = append(projects, project)
		}
		canonical[project] = record
	}
	sortMemoryCurrentStateGenerationCardTreeProjects(projects)
	root := memoryCurrentStateGenerationCardTreeBuildCanonical(projects, canonical)
	digest := memoryCurrentStateGenerationCardTreeDigest(root)
	return len(projects), "sha256:" + hex.EncodeToString(digest[:])
}

// memoryCurrentStateGenerationCardsLegacyBucketDigest verifies the v2 root
// emitted by the immediately preceding bounded-card implementation. It is
// migration-only: no live accumulator stores buckets or recomputes one on a
// normal write.
func memoryCurrentStateGenerationCardsLegacyBucketDigest(records map[string]memoryCurrentStateGenerationRecord) (int, string) {
	buckets := make([]map[string]memoryCurrentStateGenerationRecord, memoryCurrentStateGenerationCardLegacyBucketCount)
	count := 0
	for project, record := range records {
		project = normalizeCurrentKeyIndexProject(project)
		if project == "" {
			continue
		}
		sum := sha256.Sum256([]byte("contextlattice-generation-card-bucket:v1:" + project))
		bucket := int(sum[0])
		if buckets[bucket] == nil {
			buckets[bucket] = map[string]memoryCurrentStateGenerationRecord{}
		}
		if _, exists := buckets[bucket][project]; !exists {
			count++
		}
		buckets[bucket][project] = record
	}
	var bucketDigests [memoryCurrentStateGenerationCardLegacyBucketCount][32]byte
	for bucket, bucketRecords := range buckets {
		projects := make([]string, 0, len(bucketRecords))
		for project := range bucketRecords {
			projects = append(projects, project)
		}
		sort.Strings(projects)
		payload := make([]byte, 0, 64+(len(projects)*32))
		payload = append(payload, "contextlattice-generation-card-bucket-leaves:v2"...)
		var value [8]byte
		binary.BigEndian.PutUint64(value[:], uint64(bucket))
		payload = append(payload, value[:]...)
		binary.BigEndian.PutUint64(value[:], uint64(len(projects)))
		payload = append(payload, value[:]...)
		for _, project := range projects {
			leaf := memoryCurrentStateGenerationCardLeaf(project, bucketRecords[project])
			payload = append(payload, leaf[:]...)
		}
		bucketDigests[bucket] = sha256.Sum256(payload)
	}
	payload := make([]byte, 0, 64+(memoryCurrentStateGenerationCardLegacyBucketCount*40))
	payload = append(payload, "contextlattice-generation-card-root:v2"...)
	var value [8]byte
	binary.BigEndian.PutUint64(value[:], uint64(memoryCurrentStateGenerationCardLegacyBucketCount))
	payload = append(payload, value[:]...)
	binary.BigEndian.PutUint64(value[:], uint64(count))
	payload = append(payload, value[:]...)
	for bucket := 0; bucket < memoryCurrentStateGenerationCardLegacyBucketCount; bucket++ {
		binary.BigEndian.PutUint64(value[:], uint64(bucket))
		payload = append(payload, value[:]...)
		payload = append(payload, bucketDigests[bucket][:]...)
	}
	digest := sha256.Sum256(payload)
	return count, "sha256:" + hex.EncodeToString(digest[:])
}

// memoryCurrentStateGenerationCardsLegacyDigest is retained solely to accept
// the immediately preceding v3 manifest during bounded migration. New
// manifests never write this XOR accumulator and never use it as a commitment.
func memoryCurrentStateGenerationCardsLegacyDigest(records map[string]memoryCurrentStateGenerationRecord) (int, string) {
	var accumulator [32]byte
	count := 0
	for project, record := range records {
		project = normalizeCurrentKeyIndexProject(project)
		if project == "" {
			continue
		}
		token := sha256.Sum256([]byte(fmt.Sprintf("contextlattice-generation-card-set:%s:%d:%d:%s", project, record.KeyGeneration, record.TopicGeneration, record.StateDigest)))
		for index := range accumulator {
			accumulator[index] ^= token[index]
		}
		count++
	}
	return count, "sha256:" + fmt.Sprintf("%x", accumulator[:])
}

func (m *memoryStore) setCurrentStateGenerationCardsAccumulatorLocked(records map[string]memoryCurrentStateGenerationRecord) error {
	if m == nil {
		return errors.New("memory store unavailable")
	}
	projects := make([]string, 0, len(records))
	canonical := make(map[string]memoryCurrentStateGenerationRecord, len(records))
	for project, record := range records {
		project = normalizeCurrentKeyIndexProject(project)
		if project == "" {
			continue
		}
		if _, exists := canonical[project]; !exists {
			projects = append(projects, project)
		}
		canonical[project] = record
	}
	sort.Slice(projects, func(left, right int) bool {
		leftHash := memoryCurrentStateGenerationCardPatriciaKey(projects[left])
		rightHash := memoryCurrentStateGenerationCardPatriciaKey(projects[right])
		if order := bytes.Compare(leftHash[:], rightHash[:]); order != 0 {
			return order < 0
		}
		return projects[left] < projects[right]
	})
	root, err := memoryCurrentStateGenerationCardTreeBuildPatricia(projects, canonical)
	if err != nil {
		return err
	}
	var bytesTotal int64
	for _, project := range projects {
		payload, err := memoryCurrentStateGenerationCardPayload(project, canonical[project])
		if err != nil {
			return err
		}
		bytesTotal += int64(len(payload))
	}
	m.currentStateGenerationCardCount = len(projects)
	m.currentStateGenerationCardBytes = bytesTotal
	digest := memoryCurrentStateGenerationCardPatriciaDigest(root)
	m.currentStateGenerationCardTree = root
	m.currentStateGenerationCardsDigest = "sha256:" + hex.EncodeToString(digest[:])
	m.currentStateGenerationCardsDigestInitialized = true
	return nil
}

func (m *memoryStore) updateCurrentStateGenerationCardAccumulatorLocked(project string, oldRecord memoryCurrentStateGenerationRecord, oldExists bool, newRecord memoryCurrentStateGenerationRecord, newExists bool) error {
	if m == nil || !m.currentStateGenerationCardsDigestInitialized {
		return nil
	}
	project = normalizeCurrentKeyIndexProject(project)
	if project == "" {
		return nil
	}
	var authenticatedHashCount int
	hash := memoryCurrentStateGenerationCardPatriciaKey(project)
	if oldExists && !newExists {
		updated, _, err := memoryCurrentStateGenerationCardPatriciaDelete(m.currentStateGenerationCardTree, hash, project, &authenticatedHashCount)
		if err != nil {
			return err
		}
		m.currentStateGenerationCardTree = updated
		if m.currentStateGenerationCardCount > 0 {
			m.currentStateGenerationCardCount--
		}
	} else if !oldExists && newExists {
		updated, err := memoryCurrentStateGenerationCardPatriciaInsert(m.currentStateGenerationCardTree, hash, project, newRecord, &authenticatedHashCount)
		if err != nil {
			return err
		}
		m.currentStateGenerationCardTree = updated
		m.currentStateGenerationCardCount++
	} else if oldExists && newExists {
		updated, err := memoryCurrentStateGenerationCardPatriciaInsert(m.currentStateGenerationCardTree, hash, project, newRecord, &authenticatedHashCount)
		if err != nil {
			return err
		}
		m.currentStateGenerationCardTree = updated
	}
	if oldExists {
		if payload, err := memoryCurrentStateGenerationCardPayload(project, oldRecord); err == nil {
			m.currentStateGenerationCardBytes -= int64(len(payload))
		}
	}
	if newExists {
		if payload, err := memoryCurrentStateGenerationCardPayload(project, newRecord); err == nil {
			m.currentStateGenerationCardBytes += int64(len(payload))
		}
	}
	digest := memoryCurrentStateGenerationCardPatriciaDigest(m.currentStateGenerationCardTree, &authenticatedHashCount)
	m.currentStateGenerationCardsDigest = "sha256:" + hex.EncodeToString(digest[:])
	if m.memoryCurrentStateGenerationCardActualObserve != nil {
		m.memoryCurrentStateGenerationCardActualObserve(authenticatedHashCount)
	}
	return nil
}

// rebuildCurrentStateGenerationCardTreeLocked is used only while restoring a
// failed in-memory mutation. Normal writes update one tree path incrementally;
// rollback restores the one project captured by the constant-size snapshot.
func (m *memoryStore) rebuildCurrentStateGenerationCardTreeLocked(project string) {
	if m == nil || !m.currentStateGenerationCardsDigestInitialized {
		return
	}
	// Rollback is outside the hot write path. Rebuilding from the authoritative
	// generation map keeps the recovery path simple and guarantees that a
	// failed Patricia update cannot leave a partially repaired commitment.
	_ = m.setCurrentStateGenerationCardsAccumulatorLocked(m.currentStateGenerationRecords)
}

func memoryCurrentStateLegacyDigests(states map[string]memoryCurrentState) (string, map[string]string) {
	keys := make([]string, 0, len(states))
	for key := range states {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	globalRows := make([]memoryCurrentStateDigestRow, 0, len(keys))
	projectRows := map[string][]memoryCurrentStateDigestRow{}
	for _, key := range keys {
		state := states[key]
		state.Entry.Tags = append([]string(nil), state.Entry.Tags...)
		row := memoryCurrentStateDigestRow{Key: key, State: state}
		globalRows = append(globalRows, row)
		if project, _, ok := parseMemoryStoreKeyToken(key); ok {
			projectKey := normalizeCurrentKeyIndexProject(project)
			projectRows[projectKey] = append(projectRows[projectKey], row)
		}
	}
	globalDigest, _, globalErr := memoryCurrentStateRowsDigest(globalRows)
	if globalErr != nil {
		globalDigest = memoryCurrentStateDigest(states, "")
	}
	digests := make(map[string]string, len(projectRows))
	for project, rows := range projectRows {
		digest, _, err := memoryCurrentStateRowsDigest(rows)
		if err != nil {
			digest = memoryCurrentStateDigest(states, project)
		}
		digests[project] = digest
	}
	return globalDigest, digests
}

func (m *memoryStore) loadCurrentStateGenerationCardsLocked() (map[string]memoryCurrentStateGenerationRecord, error) {
	if m == nil {
		return nil, errors.New("memory store unavailable")
	}
	cardsPath := m.currentStateGenerationCardsPath()
	info, err := os.Lstat(cardsPath)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]memoryCurrentStateGenerationRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("current-state generation card directory is not a real directory")
	}
	dir, err := os.Open(cardsPath)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	projects := make(map[string]memoryCurrentStateGenerationRecord)
	knownProjects := map[string]struct{}{}
	for key := range m.currentState {
		if project, _, ok := parseMemoryStoreKeyToken(key); ok {
			knownProjects[normalizeCurrentKeyIndexProject(project)] = struct{}{}
		}
	}
	for key := range m.exactStatePaths {
		if project, _, ok := parseMemoryStoreKeyToken(key); ok {
			knownProjects[normalizeCurrentKeyIndexProject(project)] = struct{}{}
		}
	}
	var totalBytes int64
	totalEntries := 0
	for {
		names, readErr := dir.Readdirnames(1)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
		name := names[0]
		totalEntries++
		if totalEntries > memoryCurrentStateTransactionMaxCardRootEntries {
			return nil, fmt.Errorf("current-state generation card directory entry count exceeds cap %d", memoryCurrentStateTransactionMaxCardRootEntries)
		}
		if len(projects) >= memoryCurrentStateGenerationMaxCards {
			return nil, fmt.Errorf("current-state generation card count exceeds cap %d", memoryCurrentStateGenerationMaxCards)
		}
		if name == "." || name == ".." || filepath.Ext(name) != ".json" || len(strings.TrimSuffix(name, ".json")) != 64 || !isHexDigest(strings.TrimSuffix(name, ".json")) {
			return nil, fmt.Errorf("current-state generation card has a foreign name %q", name)
		}
		path := filepath.Join(cardsPath, name)
		cardInfo, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if cardInfo.Mode()&os.ModeSymlink != 0 || !cardInfo.Mode().IsRegular() {
			return nil, fmt.Errorf("current-state generation card %q is not a regular file", name)
		}
		totalBytes += cardInfo.Size()
		if totalBytes > memoryCurrentStateGenerationMaxCardBytes {
			return nil, fmt.Errorf("current-state generation cards exceed byte cap %d", memoryCurrentStateGenerationMaxCardBytes)
		}
		raw, err := readOwnerOnlyBoundedFile(path, memoryEdgeLogMaxRecoveryBytes)
		if err != nil {
			return nil, fmt.Errorf("read current-state generation card %q: %w", name, err)
		}
		var card memoryCurrentStateGenerationCard
		if err := json.Unmarshal(raw, &card); err != nil {
			return nil, fmt.Errorf("decode current-state generation card %q: %w", name, err)
		}
		project := normalizeCurrentKeyIndexProject(card.Project)
		if card.SchemaID != memoryCurrentStateGenerationCardSchemaID || card.Version != memoryCurrentStateGenerationCardsVersion || project == "" || memoryCurrentStateGenerationCardName(project) != name || card.Record.KeyGeneration != card.Record.TopicGeneration || !memoryCurrentStateGenerationDigestValid(card.Record.StateDigest) {
			return nil, fmt.Errorf("current-state generation card %q has an invalid contract", name)
		}
		if _, exists := projects[project]; exists {
			return nil, fmt.Errorf("current-state generation cards repeat project %q", project)
		}
		if _, exists := knownProjects[project]; !exists {
			return nil, fmt.Errorf("current-state generation card %q names a stale project", name)
		}
		projects[project] = card.Record
	}
	if len(projects) != len(knownProjects) {
		return nil, fmt.Errorf("current-state generation card set is missing durable project cards: have=%d want=%d", len(projects), len(knownProjects))
	}
	for project := range knownProjects {
		if _, exists := projects[project]; !exists {
			return nil, fmt.Errorf("current-state generation card set is missing durable project %q", project)
		}
	}
	return projects, nil
}

func (m *memoryStore) migrateCurrentStateGenerationCardsLocked() error {
	if m == nil {
		return errors.New("memory store unavailable")
	}
	migratedRecords := make(map[string]memoryCurrentStateGenerationRecord, len(m.currentStateGenerationRecords))
	knownProjects := map[string]struct{}{}
	for key := range m.currentState {
		if project, _, ok := parseMemoryStoreKeyToken(key); ok {
			knownProjects[normalizeCurrentKeyIndexProject(project)] = struct{}{}
		}
	}
	for key := range m.exactStatePaths {
		if project, _, ok := parseMemoryStoreKeyToken(key); ok {
			knownProjects[normalizeCurrentKeyIndexProject(project)] = struct{}{}
		}
	}
	for project := range m.currentStateGenerationRecords {
		project = normalizeCurrentKeyIndexProject(project)
		if _, exists := knownProjects[project]; exists {
			migratedRecords[project] = m.currentStateGenerationRecords[project]
		}
	}
	if _, _, err := memoryCurrentStateGenerationCardSetCapacity(migratedRecords); err != nil {
		return fmt.Errorf("validate current-state generation card migration: %w", err)
	}
	previousRecords := cloneCurrentStateGenerationRecords(m.currentStateGenerationRecords)
	previousVersion := m.currentStateGenerationManifestVersion
	previousTree := m.currentStateGenerationCardTree
	previousCount := m.currentStateGenerationCardCount
	previousBytes := m.currentStateGenerationCardBytes
	previousDigest := m.currentStateGenerationCardsDigest
	previousDigestInitialized := m.currentStateGenerationCardsDigestInitialized
	m.currentStateGenerationRecords = migratedRecords
	// The migration transaction intentionally presents itself as older than the
	// v3 card manifest until its bounded card session has installed every
	// project card.  The transaction builder uses this marker to include the
	// complete known-project set; setting it to a newer digest version here
	// would incorrectly emit only the root and brick the next restart because
	// the card directory is still empty.
	m.currentStateGenerationManifestVersion = memoryCurrentStateGenerationVersion - 1
	m.currentStateGenerationCardTree = nil
	m.currentStateGenerationCardCount = 0
	m.currentStateGenerationCardBytes = 0
	m.currentStateGenerationCardsDigest = ""
	m.currentStateGenerationCardsDigestInitialized = false
	err := m.persistCurrentStateTransactionLocked(nil, "", 0)
	if err == nil || errors.Is(err, errMemoryCurrentStateTransactionCommitted) {
		return err
	}
	// No marker means no durable card/manifest commit. Restore the in-memory
	// legacy authority so a caller can retry without a half-migrated projection.
	m.currentStateGenerationRecords = previousRecords
	m.currentStateGenerationManifestVersion = previousVersion
	m.currentStateGenerationCardTree = previousTree
	m.currentStateGenerationCardCount = previousCount
	m.currentStateGenerationCardBytes = previousBytes
	m.currentStateGenerationCardsDigest = previousDigest
	m.currentStateGenerationCardsDigestInitialized = previousDigestInitialized
	return fmt.Errorf("persist current-state generation card migration: %w", err)
}

func (m *memoryStore) loadCurrentStateGenerationManifestLocked() (bool, error) {
	if m == nil {
		return false, nil
	}
	raw, err := readOwnerOnlyBoundedFile(m.currentStateGenerationPath(), memoryEdgeLogMaxRecoveryBytes)
	if errors.Is(err, os.ErrNotExist) {
		m.currentStateGenerationManifestLoaded = false
		m.currentStateGenerationManifestVersion = 0
		m.currentStateGenerationRecords = nil
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read current-state generation manifest: %w", err)
	}
	var manifest memoryCurrentStateGenerationManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return false, fmt.Errorf("decode current-state generation manifest: %w", err)
	}
	if manifest.SchemaID != memoryCurrentStateGenerationSchemaID || (manifest.Version != 1 && manifest.Version != memoryCurrentStateGenerationDigestVersion && manifest.Version != memoryCurrentStateGenerationVersion) || !memoryCurrentStateGenerationDigestValid(manifest.StateDigest) {
		return false, errors.New("current-state generation manifest contract is invalid")
	}
	m.ensureCurrentStateDigestIndexesLocked()
	legacyManifest := manifest.Version == 1
	legacyCardsDigest := false
	var projects map[string]memoryCurrentStateGenerationRecord
	if manifest.Version == memoryCurrentStateGenerationVersion {
		if manifest.Projects != nil || manifest.ProjectCardsDir != memoryCurrentStateGenerationCardsDir || manifest.ProjectCardsVersion != memoryCurrentStateGenerationCardsVersion || (manifest.ProjectCardsDigestVersion != 0 && manifest.ProjectCardsDigestVersion != memoryCurrentStateGenerationCardsDigestVersion && manifest.ProjectCardsDigestVersion != memoryCurrentStateGenerationCardsLegacySparseVersion && manifest.ProjectCardsDigestVersion != memoryCurrentStateGenerationCardsLegacyTreeVersion && manifest.ProjectCardsDigestVersion != memoryCurrentStateGenerationCardsLegacyBucketVersion) || manifest.ProjectCardsCount < 0 || !memoryCurrentStateGenerationDigestValid(manifest.ProjectCardsDigest) {
			return false, errors.New("current-state generation card manifest contract is invalid")
		}
		var err error
		projects, err = m.loadCurrentStateGenerationCardsLocked()
		if err != nil {
			return false, err
		}
		cardCount, cardDigest := memoryCurrentStateGenerationCardsDigest(projects)
		if manifest.ProjectCardsCount != cardCount || manifest.ProjectCardsDigest != cardDigest {
			// Accept exact legacy roots once so an upgrade does not brick restart:
			// v4 sparse, v3 treap, v2 fixed bucket, and the older omitted-version
			// XOR accumulator. The next bounded transaction emits the v5 Patricia
			// commitment.
			legacyCount, legacyDigest := memoryCurrentStateGenerationCardsLegacyDigest(projects)
			switch manifest.ProjectCardsDigestVersion {
			case memoryCurrentStateGenerationCardsLegacySparseVersion:
				legacyCount, legacyDigest = memoryCurrentStateGenerationCardsLegacySparseDigest(projects)
			case memoryCurrentStateGenerationCardsLegacyTreeVersion:
				legacyCount, legacyDigest = memoryCurrentStateGenerationCardsLegacyTreeDigest(projects)
			case memoryCurrentStateGenerationCardsLegacyBucketVersion:
				legacyCount, legacyDigest = memoryCurrentStateGenerationCardsLegacyBucketDigest(projects)
			}
			if manifest.ProjectCardsCount != legacyCount || manifest.ProjectCardsDigest != legacyDigest {
				return false, errors.New("current-state generation card set does not match its manifest")
			}
			legacyCardsDigest = true
		}
	} else if manifest.Projects == nil {
		return false, errors.New("current-state generation manifest project map is missing")
	} else {
		if len(manifest.Projects) > memoryCurrentStateGenerationMaxCards {
			return false, fmt.Errorf("current-state generation manifest project count exceeds cap %d", memoryCurrentStateGenerationMaxCards)
		}
		projects = make(map[string]memoryCurrentStateGenerationRecord, len(manifest.Projects))
		for projectKey, record := range manifest.Projects {
			projectKey = normalizeCurrentKeyIndexProject(projectKey)
			if projectKey == "" {
				return false, errors.New("current-state generation manifest contains an empty project")
			}
			if _, exists := projects[projectKey]; exists {
				return false, fmt.Errorf("current-state generation manifest repeats project %q", projectKey)
			}
			projects[projectKey] = record
		}
	}
	globalDigest := m.currentStateDigestLocked("")
	legacyProjectDigests := map[string]string{}
	if legacyManifest {
		globalDigest, legacyProjectDigests = memoryCurrentStateLegacyDigests(m.currentState)
	}
	if manifest.StateDigest != globalDigest {
		return false, errors.New("current-state generation manifest does not match durable state")
	}
	for projectKey, record := range projects {
		projectKey = normalizeCurrentKeyIndexProject(projectKey)
		projectDigest := memoryCurrentStateRootDigest(projectKey, record.KeyGeneration, m.currentStateProjectShardDigests[projectKey])
		if legacyManifest {
			projectDigest = legacyProjectDigests[projectKey]
		}
		if projectKey == "" || record.KeyGeneration != record.TopicGeneration || !memoryCurrentStateGenerationDigestValid(record.StateDigest) || record.StateDigest != projectDigest {
			return false, fmt.Errorf("current-state generation manifest has stale project %q", projectKey)
		}
	}
	if _, _, err := memoryCurrentStateGenerationCardSetCapacity(projects); err != nil {
		return false, fmt.Errorf("validate current-state generation card set: %w", err)
	}
	m.currentStateGenerationRecords = projects
	if manifest.Version == memoryCurrentStateGenerationVersion {
		if err := m.setCurrentStateGenerationCardsAccumulatorLocked(projects); err != nil {
			return false, fmt.Errorf("build current-state generation card commitment: %w", err)
		}
	}
	m.currentStateGenerationManifestLoaded = true
	m.currentStateGenerationManifestVersion = manifest.Version
	if legacyCardsDigest {
		// Manifest version two is the indexed-root migration marker. The next
		// bounded transaction includes all cards and emits manifest version three
		// with the v5 Patricia card digest.
		m.currentStateGenerationManifestVersion = memoryCurrentStateGenerationDigestVersion
	}
	// Legacy map manifests and legacy card-root versions are bounded migration
	// inputs. The next successful persistence rewrites them as the v3
	// fixed-root/card manifest with the v5 Patricia commitment.
	return true, nil
}

func (m *memoryStore) persistCurrentStateGenerationManifestLocked() error {
	if m == nil {
		return nil
	}
	if err := m.persistCurrentStateTransactionLocked(nil, "", 0); err != nil {
		return fmt.Errorf("persist current-state generation manifest: %w", err)
	}
	return nil
}

func (m *memoryStore) durableCurrentStateGeneration(project string) (uint64, uint64, string, error) {
	if m == nil {
		return 0, 0, "", errors.New("memory store unavailable")
	}
	projectKey := normalizeCurrentKeyIndexProject(project)
	m.mu.RLock()
	record, ok := m.currentStateGenerationRecords[projectKey]
	loaded := m.currentStateGenerationManifestLoaded
	keyGeneration, keyOK := m.currentKeyIndexGeneration[projectKey]
	topicGeneration, topicOK := m.currentTopicIndexGeneration[projectKey]
	m.mu.RUnlock()
	if !loaded || !ok || !keyOK || !topicOK || keyGeneration != record.KeyGeneration || topicGeneration != record.TopicGeneration || record.KeyGeneration != record.TopicGeneration || !memoryCurrentStateGenerationDigestValid(record.StateDigest) {
		return 0, 0, "", errors.New("durable current-state generation manifest is unavailable")
	}
	return record.KeyGeneration, record.TopicGeneration, record.StateDigest, nil
}

func memoryCurrentStateFromEntry(entry memoryStoreEntry) memoryCurrentState {
	copyEntry := entry
	copyEntry.Tags = append([]string(nil), entry.Tags...)
	copyEntry.Lifecycle = normalizeMemoryLifecycle(entry.Lifecycle)
	copyEntry.StorageTier = normalizeMemoryStorageTier(entry.StorageTier)
	return memoryCurrentState{
		Entry:     copyEntry,
		LegalHold: memoryTagsHaveLegalHold(copyEntry.Tags),
		Tombstone: isMemoryTombstone(copyEntry),
	}
}

func memoryCurrentStateSupersedes(candidate memoryCurrentState, current memoryCurrentState) bool {
	if reflect.DeepEqual(candidate, current) {
		return false
	}
	candidateAt, candidateOK := parseTimeBestEffort(candidate.Entry.CreatedAt)
	currentAt, currentOK := parseTimeBestEffort(current.Entry.CreatedAt)
	if candidateOK && currentOK {
		if candidateAt.After(currentAt) {
			return true
		}
		if candidateAt.Before(currentAt) {
			return false
		}
	}
	if candidateOK != currentOK {
		return candidateOK
	}
	if candidate.Entry.EventID == current.Entry.EventID {
		return true
	}
	return strings.TrimSpace(candidate.Entry.EventID) > strings.TrimSpace(current.Entry.EventID)
}

func (m *memoryStore) ensureCurrentStateMapLocked() {
	if m.currentState == nil {
		m.currentState = map[string]memoryCurrentState{}
	}
}

func (m *memoryStore) ensureCurrentStateDigestIndexesLocked() {
	if m == nil {
		return
	}
	if m.currentStateByShard == nil {
		m.currentStateByShard = map[int]map[string]memoryCurrentState{}
		m.currentStateDigestIndexesInitialized = false
	}
	if m.currentStateShardPayloads == nil {
		m.currentStateShardPayloads = map[int][]byte{}
		m.currentStateDigestIndexesInitialized = false
	}
	if m.currentStateShardDigests == nil {
		m.currentStateShardDigests = map[int]string{}
		m.currentStateDigestIndexesInitialized = false
	}
	if m.currentStateProjectShardDigests == nil {
		m.currentStateProjectShardDigests = map[string]map[int]string{}
		m.currentStateDigestIndexesInitialized = false
	}
	if m.currentStateDigestIndexesInitialized {
		return
	}
	for shard := 0; shard < memoryCurrentStateShardCount; shard++ {
		m.currentStateByShard[shard] = map[string]memoryCurrentState{}
	}
	for key, state := range m.currentState {
		shard := memoryCurrentStateShardForKey(key)
		m.currentStateByShard[shard][key] = state
	}
	m.currentStateShardPayloads = map[int][]byte{}
	m.currentStateShardDigests = map[int]string{}
	m.currentStateProjectShardDigests = map[string]map[int]string{}
	for shard := 0; shard < memoryCurrentStateShardCount; shard++ {
		if err := m.refreshCurrentStateShardDigestIndexesLocked(shard); err != nil {
			// The in-memory states have already passed JSON validation.  Keep a
			// deterministic empty cache only for the impossible marshal failure;
			// persistence will return the concrete error when it next encodes.
			m.currentStateShardDigests[shard] = memoryCurrentStateEmptyShardDigest()
		}
	}
	m.currentStateDigestIndexesInitialized = true
}

func (m *memoryStore) refreshCurrentStateShardDigestIndexesLocked(shard int) error {
	if m == nil || shard < 0 || shard >= memoryCurrentStateShardCount {
		return errors.New("invalid memory current-state shard")
	}
	m.ensureCurrentStateDigestIndexesMapsOnlyLocked()
	states := m.currentStateByShard[shard]
	if m.memoryCurrentStateDigestObserve != nil {
		m.memoryCurrentStateDigestObserve(shard, len(states))
	}
	keys := make([]string, 0, len(states))
	for key := range states {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	rows := make([]memoryCurrentStateDigestRow, 0, len(keys))
	entries := make([]memoryCurrentState, 0, len(keys))
	projectRows := map[string][]memoryCurrentStateDigestRow{}
	for _, key := range keys {
		state := states[key]
		state.Entry.Tags = append([]string(nil), state.Entry.Tags...)
		rows = append(rows, memoryCurrentStateDigestRow{Key: key, State: state})
		entries = append(entries, state)
		project, _, ok := parseMemoryStoreKeyToken(key)
		if ok {
			projectKey := normalizeCurrentKeyIndexProject(project)
			projectRows[projectKey] = append(projectRows[projectKey], memoryCurrentStateDigestRow{Key: key, State: state})
		}
	}
	shardDigest, _, err := memoryCurrentStateRowsDigest(rows)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(memoryCurrentStateShard{
		SchemaID: memoryCurrentStateSchemaID,
		Version:  1,
		Shard:    shard,
		Entries:  entries,
	})
	if err != nil {
		return err
	}
	m.currentStateShardDigests[shard] = shardDigest
	m.currentStateShardPayloads[shard] = append([]byte(nil), payload...)
	for projectKey, projectDigestRows := range projectRows {
		projectDigests := m.currentStateProjectShardDigests[projectKey]
		if projectDigests == nil {
			projectDigests = map[int]string{}
			m.currentStateProjectShardDigests[projectKey] = projectDigests
		}
		projectDigest, _, err := memoryCurrentStateRowsDigest(projectDigestRows)
		if err != nil {
			return err
		}
		projectDigests[shard] = projectDigest
	}
	return nil
}

// ensureCurrentStateDigestIndexesMapsOnlyLocked initializes cache maps without
// triggering a corpus rebuild. It is used by the one-shard refresh path.
func (m *memoryStore) ensureCurrentStateDigestIndexesMapsOnlyLocked() {
	if m.currentStateByShard == nil {
		m.currentStateByShard = map[int]map[string]memoryCurrentState{}
	}
	if m.currentStateShardPayloads == nil {
		m.currentStateShardPayloads = map[int][]byte{}
	}
	if m.currentStateShardDigests == nil {
		m.currentStateShardDigests = map[int]string{}
	}
	if m.currentStateProjectShardDigests == nil {
		m.currentStateProjectShardDigests = map[string]map[int]string{}
	}
	if m.currentStateByShard[0] == nil {
		for shard := 0; shard < memoryCurrentStateShardCount; shard++ {
			if m.currentStateByShard[shard] == nil {
				m.currentStateByShard[shard] = map[string]memoryCurrentState{}
			}
		}
	}
}

func (m *memoryStore) currentStateDigestLocked(project string) string {
	if m == nil {
		return ""
	}
	m.ensureCurrentStateDigestIndexesLocked()
	projectKey := normalizeCurrentKeyIndexProject(project)
	if projectKey == "" {
		return memoryCurrentStateRootDigest("", 0, m.currentStateShardDigests)
	}
	generation := m.currentKeyIndexGeneration[projectKey]
	return memoryCurrentStateRootDigest(projectKey, generation, m.currentStateProjectShardDigests[projectKey])
}

// currentStateDigestReadLocked is the read-lock counterpart used by graph
// snapshots.  The cache is materialized during initialization and writes, so
// this path only combines the bounded digest leaves and never mutates maps
// under an RLock.
func (m *memoryStore) currentStateDigestReadLocked(project string) string {
	if m == nil {
		return ""
	}
	projectKey := normalizeCurrentKeyIndexProject(project)
	if projectKey == "" {
		return memoryCurrentStateRootDigest("", 0, m.currentStateShardDigests)
	}
	return memoryCurrentStateRootDigest(projectKey, m.currentKeyIndexGeneration[projectKey], m.currentStateProjectShardDigests[projectKey])
}

func (m *memoryStore) ensureCurrentKeyIndexLocked() {
	if m == nil {
		return
	}
	if m.currentKeysByProject == nil {
		m.currentKeysByProject = map[string]map[string]struct{}{}
	}
	if m.currentKeyCountsByProject == nil {
		m.currentKeyCountsByProject = map[string]int{}
	}
	if m.currentKeysByProjectTopic == nil {
		m.currentKeysByProjectTopic = map[string]map[string]map[string]struct{}{}
	}
	if m.currentTopicKeyCountsByProject == nil {
		m.currentTopicKeyCountsByProject = map[string]int{}
	}
	if m.currentKeyIndexGeneration == nil {
		m.currentKeyIndexGeneration = map[string]uint64{}
	}
	if m.currentTopicIndexGeneration == nil {
		m.currentTopicIndexGeneration = map[string]uint64{}
	}
}

func normalizeCurrentKeyIndexProject(project string) string {
	return strings.ToLower(strings.TrimSpace(project))
}

const currentStateUnscopedTopicBucket = "\x00"

func currentStateTopicBucket(topicPath string) string {
	normalized := normalizeTopicPathLoose(topicPath)
	if normalized == "" {
		return currentStateUnscopedTopicBucket
	}
	return normalized
}

func (m *memoryStore) ensureCurrentProjectIndexLocked(projectKey string) {
	if m == nil || projectKey == "" {
		return
	}
	m.ensureCurrentKeyIndexLocked()
	if m.currentKeysByProject[projectKey] == nil {
		m.currentKeysByProject[projectKey] = map[string]struct{}{}
	}
	if m.currentKeysByProjectTopic[projectKey] == nil {
		m.currentKeysByProjectTopic[projectKey] = map[string]map[string]struct{}{}
	}
	if _, exists := m.currentKeyCountsByProject[projectKey]; !exists {
		m.currentKeyCountsByProject[projectKey] = 0
	}
	if _, exists := m.currentTopicKeyCountsByProject[projectKey]; !exists {
		m.currentTopicKeyCountsByProject[projectKey] = 0
	}
	if _, exists := m.currentKeyIndexGeneration[projectKey]; !exists {
		m.currentKeyIndexGeneration[projectKey] = 0
	}
	if _, exists := m.currentTopicIndexGeneration[projectKey]; !exists {
		m.currentTopicIndexGeneration[projectKey] = 0
	}
}

func (m *memoryStore) advanceCurrentIndexGenerationLocked(projectKey string) {
	if m == nil || projectKey == "" {
		return
	}
	keyGeneration := m.currentKeyIndexGeneration[projectKey]
	topicGeneration := m.currentTopicIndexGeneration[projectKey]
	if keyGeneration != topicGeneration {
		return
	}
	if keyGeneration == ^uint64(0) {
		// Saturation is an explicit fail-closed state; never wrap and make a
		// stale topic projection look coherent with the primary index.
		m.currentTopicIndexGeneration[projectKey] = 0
		return
	}
	keyGeneration++
	m.currentKeyIndexGeneration[projectKey] = keyGeneration
	m.currentTopicIndexGeneration[projectKey] = keyGeneration
}

func (m *memoryStore) addCurrentKeyLocked(project string, key string, topicPath string) {
	if m == nil || key == "::" {
		return
	}
	projectKey := normalizeCurrentKeyIndexProject(project)
	if projectKey == "" {
		return
	}
	m.ensureCurrentProjectIndexLocked(projectKey)
	keys := m.currentKeysByProject[projectKey]
	changed := false
	if _, exists := keys[key]; !exists {
		keys[key] = struct{}{}
		m.currentKeyCountsByProject[projectKey]++
		changed = true
	}

	newTopic := currentStateTopicBucket(topicPath)
	oldTopic := currentStateTopicBucket(m.latestTopic[key])
	topics := m.currentKeysByProjectTopic[projectKey]
	if oldTopic != newTopic {
		if oldKeys := topics[oldTopic]; oldKeys != nil {
			if _, exists := oldKeys[key]; exists {
				delete(oldKeys, key)
				m.currentTopicKeyCountsByProject[projectKey]--
				changed = true
				if len(oldKeys) == 0 {
					delete(topics, oldTopic)
				}
			}
		}
	}
	newKeys := topics[newTopic]
	if newKeys == nil {
		newKeys = map[string]struct{}{}
		topics[newTopic] = newKeys
	}
	if _, exists := newKeys[key]; !exists {
		newKeys[key] = struct{}{}
		m.currentTopicKeyCountsByProject[projectKey]++
		changed = true
	}
	if changed {
		m.advanceCurrentIndexGenerationLocked(projectKey)
	}
}

func (m *memoryStore) removeCurrentKeyLocked(project string, key string) {
	if m == nil || key == "::" || m.currentKeysByProject == nil {
		return
	}
	projectKey := normalizeCurrentKeyIndexProject(project)
	keys := m.currentKeysByProject[projectKey]
	if keys == nil {
		return
	}
	if _, exists := keys[key]; !exists {
		return
	}
	delete(keys, key)
	oldTopic := currentStateTopicBucket(m.latestTopic[key])
	if topics := m.currentKeysByProjectTopic[projectKey]; topics != nil {
		if topicKeys := topics[oldTopic]; topicKeys != nil {
			if _, exists := topicKeys[key]; exists {
				delete(topicKeys, key)
				m.currentTopicKeyCountsByProject[projectKey]--
				if len(topicKeys) == 0 {
					delete(topics, oldTopic)
				}
			}
		}
	}
	if count := m.currentKeyCountsByProject[projectKey] - 1; count > 0 {
		m.currentKeyCountsByProject[projectKey] = count
	} else {
		m.currentKeyCountsByProject[projectKey] = 0
	}
	m.advanceCurrentIndexGenerationLocked(projectKey)
}

func (m *memoryStore) applyCurrentStateEntryLocked(entry memoryStoreEntry) bool {
	if m == nil {
		return false
	}
	m.ensureCurrentStateMapLocked()
	m.ensureCurrentStateDigestIndexesLocked()
	key := memoryStoreKey(entry.Project, entry.FileName)
	if key == "::" {
		return false
	}
	candidate := memoryCurrentStateFromEntry(entry)
	if current, exists := m.currentState[key]; exists && !memoryCurrentStateSupersedes(candidate, current) {
		return false
	}
	m.currentState[key] = candidate
	shard := memoryCurrentStateShardForKey(key)
	m.currentStateByShard[shard][key] = candidate
	if !m.currentStateDigestIndexDeferred {
		if err := m.refreshCurrentStateShardDigestIndexesLocked(shard); err != nil {
			// The candidate remains in-memory until the caller's durable transaction
			// can report the exact encoding failure; no digest leaf is fabricated.
			delete(m.currentStateShardPayloads, shard)
			delete(m.currentStateShardDigests, shard)
		}
	}
	return true
}

func (m *memoryStore) restoreLatestIndexesFromCurrentStateLocked() {
	_ = m.restoreLatestIndexesFromCurrentStateChecked()
}

func (m *memoryStore) restoreLatestIndexesFromCurrentStateChecked() error {
	if m == nil || !m.isConfigured() {
		return nil
	}
	fence, err := m.acquireMemoryEdgeLogFenceOptional()
	if err != nil {
		return err
	}
	if fence != nil {
		defer fence.release()
	}
	return m.restoreLatestIndexesFromCurrentStateWithFenceLockedChecked(fence)
}

func (m *memoryStore) restoreLatestIndexesFromCurrentStateWithFenceLocked(fence *memoryEdgeLogFenceToken) {
	_ = m.restoreLatestIndexesFromCurrentStateWithFenceLockedChecked(fence)
}

func (m *memoryStore) restoreLatestIndexesFromCurrentStateWithFenceLockedChecked(fence *memoryEdgeLogFenceToken) error {
	if m == nil {
		return nil
	}
	if err := requireMemoryEdgeLogFenceOptional(m, fence); err != nil {
		return err
	}
	m.ensureCurrentStateMapLocked()
	m.ensureCurrentKeyIndexLocked()
	m.currentKeysByProject = map[string]map[string]struct{}{}
	m.currentKeyCountsByProject = map[string]int{}
	m.currentKeysByProjectTopic = map[string]map[string]map[string]struct{}{}
	m.currentTopicKeyCountsByProject = map[string]int{}
	m.currentKeyIndexGeneration = map[string]uint64{}
	m.currentTopicIndexGeneration = map[string]uint64{}
	m.latestTopic = map[string]string{}
	generationRecords := m.currentStateGenerationRecords
	if generationRecords == nil {
		generationRecords = map[string]memoryCurrentStateGenerationRecord{}
	}
	projectKeys := map[string]struct{}{}
	for key, state := range m.currentState {
		entry := state.Entry
		if project, _, ok := parseMemoryStoreKeyToken(key); ok {
			projectKeys[normalizeCurrentKeyIndexProject(project)] = struct{}{}
		}
		exact, err := exactStatePathSetContainsChecked(m.exactStatePaths, entry.Project, entry.FileName)
		if err != nil {
			return fmt.Errorf("restore current-state exact-state validation: %w", err)
		}
		if exact {
			delete(m.latestTopic, key)
			delete(m.latestHash, key)
			delete(m.latestHorizon, key)
			delete(m.latestLifecycle, key)
			delete(m.latestStorageTier, key)
			delete(m.lastAccess, key)
			delete(m.confidence, key)
			continue
		}
		if state.Tombstone {
			projectKey := normalizeCurrentKeyIndexProject(entry.Project)
			if projectKey != "" {
				m.ensureCurrentProjectIndexLocked(projectKey)
			}
			delete(m.latestTopic, key)
			delete(m.latestHash, key)
			delete(m.latestHorizon, key)
			delete(m.latestLifecycle, key)
			delete(m.latestStorageTier, key)
			delete(m.lastAccess, key)
			delete(m.confidence, key)
			continue
		}
		m.addCurrentKeyLocked(entry.Project, key, entry.TopicPath)
		m.latestTopic[key] = entry.TopicPath
		m.latestLifecycle[key] = normalizeMemoryLifecycle(entry.Lifecycle)
		m.latestStorageTier[key] = normalizeMemoryStorageTier(entry.StorageTier)
		if strings.TrimSpace(entry.ContentHash) != "" {
			m.latestHash[key] = entry.ContentHash
		}
		if entry.HorizonDays != 0 {
			m.latestHorizon[key] = entry.HorizonDays
		}
		if accessedAt, ok := parseTimeBestEffort(entry.LastAccess); ok {
			m.lastAccess[key] = accessedAt
		}
		if entry.Confidence > 0 {
			weight := m.policy.confidenceReadWeight + m.policy.confidenceWriteWeight
			m.confidence[key] = confidenceState{
				alpha: m.policy.confidencePriorAlpha + (entry.Confidence * weight),
				beta:  m.policy.confidencePriorBeta + ((1.0 - entry.Confidence) * weight),
			}
		}
	}
	if m.currentStateGenerationManifestLoaded {
		for projectKey := range projectKeys {
			record, exists := generationRecords[projectKey]
			projectDigest := memoryCurrentStateRootDigest(projectKey, record.KeyGeneration, m.currentStateProjectShardDigests[projectKey])
			if m.currentStateGenerationManifestVersion == 1 {
				projectDigest = memoryCurrentStateDigest(m.currentState, projectKey)
			}
			if !exists || record.KeyGeneration != record.TopicGeneration || !memoryCurrentStateGenerationDigestValid(record.StateDigest) || record.StateDigest != projectDigest {
				return fmt.Errorf("current-state generation manifest is missing or stale for project %q", projectKey)
			}
			m.ensureCurrentProjectIndexLocked(projectKey)
			m.currentKeyIndexGeneration[projectKey] = record.KeyGeneration
			m.currentTopicIndexGeneration[projectKey] = record.TopicGeneration
		}
		for projectKey, record := range generationRecords {
			if _, exists := projectKeys[projectKey]; exists {
				continue
			}
			projectDigest := memoryCurrentStateRootDigest(projectKey, record.KeyGeneration, m.currentStateProjectShardDigests[projectKey])
			if m.currentStateGenerationManifestVersion == 1 {
				projectDigest = memoryCurrentStateDigest(m.currentState, projectKey)
			}
			if record.KeyGeneration != record.TopicGeneration || !memoryCurrentStateGenerationDigestValid(record.StateDigest) || record.StateDigest != projectDigest {
				return fmt.Errorf("current-state generation manifest has stale empty project %q", projectKey)
			}
			m.ensureCurrentProjectIndexLocked(projectKey)
			m.currentKeyIndexGeneration[projectKey] = record.KeyGeneration
			m.currentTopicIndexGeneration[projectKey] = record.TopicGeneration
		}
	} else {
		generationRecords = map[string]memoryCurrentStateGenerationRecord{}
		for projectKey := range projectKeys {
			stateDigest := m.currentStateDigestLocked(projectKey)
			generation := memoryCurrentStateGenerationSeed(stateDigest)
			m.ensureCurrentProjectIndexLocked(projectKey)
			m.currentKeyIndexGeneration[projectKey] = generation
			m.currentTopicIndexGeneration[projectKey] = generation
			generationRecords[projectKey] = memoryCurrentStateGenerationRecord{KeyGeneration: generation, TopicGeneration: generation, StateDigest: stateDigest}
		}
		m.currentStateGenerationRecords = generationRecords
		m.currentStateGenerationManifestVersion = memoryCurrentStateGenerationVersion
	}
	return nil
}

func (m *memoryStore) loadCurrentState() error {
	if m == nil || !m.isConfigured() {
		return nil
	}
	fence, err := m.acquireMemoryEdgeLogFenceOptional()
	if err != nil {
		return err
	}
	if fence != nil {
		defer fence.release()
	}
	return m.loadCurrentStateWithFenceLocked(fence)
}

func (m *memoryStore) loadCurrentStateWithFenceLocked(fence *memoryEdgeLogFenceToken) error {
	if m == nil || !m.isConfigured() {
		return nil
	}
	if err := requireMemoryEdgeLogFenceOptional(m, fence); err != nil {
		return err
	}
	if err := m.recoverCurrentStateTransactionLocked(); err != nil {
		return fmt.Errorf("recover current-state transaction: %w", err)
	}
	// Exact registration may be part of the recovered current-state
	// transaction. The store constructor loads the registry before current
	// state recovery, so refresh it under the same fence before rebuilding
	// semantic latest indexes; otherwise a committed exact row could be
	// resurrected in memory until the next restart.
	if err := m.loadExactStateIndexWithFenceLocked(fence); err != nil {
		return fmt.Errorf("reload exact state index after current-state recovery: %w", err)
	}
	m.currentState = map[string]memoryCurrentState{}
	m.currentStateByShard = map[int]map[string]memoryCurrentState{}
	m.currentStateShardPayloads = map[int][]byte{}
	m.currentStateShardDigests = map[int]string{}
	m.currentStateProjectShardDigests = map[string]map[int]string{}
	m.currentStateDigestIndexesInitialized = false
	for shard := 0; shard < memoryCurrentStateShardCount; shard++ {
		m.currentStateByShard[shard] = map[string]memoryCurrentState{}
	}
	for shard := 0; shard < memoryCurrentStateShardCount; shard++ {
		path := m.currentStateShardPath(shard)
		raw, err := readMemoryCurrentStateShard(path, memoryEdgeLogMaxRecoveryBytes)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read memory current-state shard %d: %w", shard, err)
		}
		payload := memoryCurrentStateShard{}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("decode memory current-state shard %d: %w", shard, err)
		}
		if payload.SchemaID != memoryCurrentStateSchemaID || payload.Version != 1 || payload.Shard != shard {
			return fmt.Errorf("memory current-state shard %d has an invalid contract", shard)
		}
		for _, state := range payload.Entries {
			project, err := sanitizeMemoryProject(state.Entry.Project)
			if err != nil {
				return fmt.Errorf("memory current-state shard %d has invalid project: %w", shard, err)
			}
			fileName, err := sanitizeMemoryFile(state.Entry.FileName)
			if err != nil {
				return fmt.Errorf("memory current-state shard %d has invalid file: %w", shard, err)
			}
			state.Entry.Project = project
			state.Entry.FileName = fileName
			state.Entry.Tags = append([]string(nil), state.Entry.Tags...)
			state.Entry.Lifecycle = normalizeMemoryLifecycle(state.Entry.Lifecycle)
			state.Entry.StorageTier = normalizeMemoryStorageTier(state.Entry.StorageTier)
			state.LegalHold = state.LegalHold || memoryTagsHaveLegalHold(state.Entry.Tags)
			state.Tombstone = state.Tombstone || isMemoryTombstone(state.Entry)
			key := memoryStoreKey(project, fileName)
			if memoryCurrentStateShardForKey(key) != shard {
				return fmt.Errorf("memory current-state shard %d contains a misplaced entry", shard)
			}
			if current, exists := m.currentState[key]; !exists || memoryCurrentStateSupersedes(state, current) {
				m.currentState[key] = state
				m.currentStateByShard[shard][key] = state
			}
		}
	}
	m.ensureCurrentStateDigestIndexesLocked()
	manifestLoaded, manifestErr := m.loadCurrentStateGenerationManifestLocked()
	if manifestErr != nil {
		return manifestErr
	}
	if err := m.restoreLatestIndexesFromCurrentStateWithFenceLockedChecked(fence); err != nil {
		return fmt.Errorf("restore current-state indexes: %w", err)
	}
	if !manifestLoaded {
		if err := m.migrateCurrentStateGenerationCardsLocked(); err != nil {
			return err
		}
		m.currentStateGenerationManifestVersion = memoryCurrentStateGenerationVersion
		if err := m.persistCurrentStateGenerationManifestLocked(); err != nil {
			return err
		}
	} else if m.currentStateGenerationManifestVersion != memoryCurrentStateGenerationVersion {
		if err := m.migrateCurrentStateGenerationCardsLocked(); err != nil {
			return err
		}
		// The old map remains the durable authority until all cards are
		// materialized. Switching the in-memory representation only changes the
		// bounded manifest transaction that follows.
		m.currentStateGenerationManifestVersion = memoryCurrentStateGenerationVersion
		if err := m.persistCurrentStateGenerationManifestLocked(); err != nil {
			return err
		}
	}
	return nil
}

func (m *memoryStore) persistCurrentStateShardLocked(shard int) error {
	if m == nil || shard < 0 || shard >= memoryCurrentStateShardCount {
		return errors.New("invalid memory current-state shard")
	}
	return m.persistCurrentStateTransactionLocked(map[int]struct{}{shard: {}}, "", 0)
}

func (m *memoryStore) persistCurrentStateShardsLocked(shards map[int]struct{}) error {
	return m.persistCurrentStateTransactionLocked(shards, "", 0)
}

type memoryCurrentStateMutationSnapshot struct {
	key                          string
	shard                        int
	project                      string
	previousState                memoryCurrentState
	previousStateExists          bool
	previousShardStateExists     bool
	previousShardState           memoryCurrentState
	projectKeyMapExists          bool
	projectKeyPresent            bool
	projectTopicMapExists        bool
	projectTopicOld              string
	projectTopicOldBucketExists  bool
	projectTopicOldKeyPresent    bool
	projectTopicNew              string
	projectTopicNewBucketExists  bool
	projectTopicNewKeyPresent    bool
	projectKeyCount              int
	projectTopicKeyCount         int
	projectKeyGeneration         uint64
	projectKeyGenerationExists   bool
	projectTopicGeneration       uint64
	projectTopicGenerationExists bool
	latestTopic                  string
	latestTopicExists            bool
	latestHash                   string
	latestHashExists             bool
	latestHorizon                int
	latestHorizonExists          bool
	latestLifecycle              string
	latestLifecycleExists        bool
	latestStorageTier            string
	latestStorageTierExists      bool
	lastAccess                   time.Time
	lastAccessExists             bool
	confidence                   confidenceState
	confidenceExists             bool
	recentLen                    int
	recentWasNil                 bool
	recentFirst                  memoryStoreEntry
	recentFirstExists            bool
	recentFallback               []memoryStoreEntry
	rollupCacheMapExists         bool
	rollupCacheProjectEntries    map[string]topicRollupCacheEntry
	generationRecordsMapExists   bool
	generationRecord             memoryCurrentStateGenerationRecord
	generationRecordExists       bool
	generationRecordsFallback    map[string]memoryCurrentStateGenerationRecord
	generationRecordsFallbackOK  bool
	generationManifestLoaded     bool
	generationManifestVersion    int
	generationCardCount          int
	generationCardBytes          int64
	generationCardsDigest        string
	generationCardsDigestInit    bool
	shardPayload                 []byte
	shardPayloadExists           bool
	shardDigest                  string
	shardDigestExists            bool
	projectShardDigests          map[int]string
	projectShardDigestsExists    bool
}

func cloneCurrentStateGenerationRecords(input map[string]memoryCurrentStateGenerationRecord) map[string]memoryCurrentStateGenerationRecord {
	if input == nil {
		return nil
	}
	output := make(map[string]memoryCurrentStateGenerationRecord, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func (m *memoryStore) captureCurrentStateMutationSnapshotLocked(key, project, topicPath string) memoryCurrentStateMutationSnapshot {
	projectKey := normalizeCurrentKeyIndexProject(project)
	m.ensureCurrentStateDigestIndexesLocked()
	snapshot := memoryCurrentStateMutationSnapshot{
		key:                        key,
		shard:                      memoryCurrentStateShardForKey(key),
		project:                    projectKey,
		recentLen:                  len(m.recent),
		recentWasNil:               m.recent == nil,
		generationManifestLoaded:   m.currentStateGenerationManifestLoaded,
		generationManifestVersion:  m.currentStateGenerationManifestVersion,
		rollupCacheMapExists:       m.rollupCache != nil,
		generationRecordsMapExists: m.currentStateGenerationRecords != nil,
		generationCardCount:        m.currentStateGenerationCardCount,
		generationCardBytes:        m.currentStateGenerationCardBytes,
		generationCardsDigest:      m.currentStateGenerationCardsDigest,
		generationCardsDigestInit:  m.currentStateGenerationCardsDigestInitialized,
	}
	if len(m.recent) > m.policy.maxRecent {
		// This is only a defensive fallback for legacy fixtures that violate the
		// bounded recent invariant. Normal writes retain at most maxRecent rows,
		// so the success path below does not copy the history tail.
		snapshot.recentFallback = append([]memoryStoreEntry(nil), m.recent...)
	} else if len(m.recent) == m.policy.maxRecent && len(m.recent) > 0 {
		snapshot.recentFirst = m.recent[0]
		snapshot.recentFirstExists = true
	}
	if m.rollupCache != nil {
		cacheProject := normalizeRollupProject(projectKey)
		for cacheKey, entry := range m.rollupCache {
			parts := strings.SplitN(cacheKey, "|", 2)
			if len(parts) == 2 && strings.EqualFold(parts[0], cacheProject) {
				if snapshot.rollupCacheProjectEntries == nil {
					snapshot.rollupCacheProjectEntries = map[string]topicRollupCacheEntry{}
				}
				snapshot.rollupCacheProjectEntries[cacheKey] = entry
			}
		}
	}
	if m.currentStateGenerationRecords != nil {
		snapshot.generationRecord, snapshot.generationRecordExists = m.currentStateGenerationRecords[projectKey]
		if len(m.currentStateGenerationRecords) == 0 && len(m.currentKeyIndexGeneration) > 0 {
			// Initial manifest migration may materialize every project record. It
			// is bounded startup work; preserve the exact map only in that path.
			snapshot.generationRecordsFallback = cloneCurrentStateGenerationRecords(m.currentStateGenerationRecords)
			snapshot.generationRecordsFallbackOK = true
		}
	}
	snapshot.previousState, snapshot.previousStateExists = m.currentState[key]
	if states := m.currentStateByShard[snapshot.shard]; states != nil {
		snapshot.previousShardState, snapshot.previousShardStateExists = states[key]
	}
	if keys, exists := m.currentKeysByProject[projectKey]; exists && keys != nil {
		snapshot.projectKeyMapExists = true
		_, snapshot.projectKeyPresent = keys[key]
	}
	if topics, exists := m.currentKeysByProjectTopic[projectKey]; exists && topics != nil {
		snapshot.projectTopicMapExists = true
	}
	snapshot.projectTopicOld = currentStateTopicBucket(m.latestTopic[key])
	snapshot.projectTopicNew = currentStateTopicBucket(topicPath)
	if strings.TrimSpace(topicPath) == "" {
		if _, fileName, ok := parseMemoryStoreKeyToken(key); ok {
			snapshot.projectTopicNew = currentStateTopicBucket(deriveTopicFromFile(fileName))
		}
	}
	if topics := m.currentKeysByProjectTopic[projectKey]; topics != nil {
		if keys, exists := topics[snapshot.projectTopicOld]; exists && keys != nil {
			snapshot.projectTopicOldBucketExists = true
			_, snapshot.projectTopicOldKeyPresent = keys[key]
		}
		if snapshot.projectTopicNew != snapshot.projectTopicOld {
			if keys, exists := topics[snapshot.projectTopicNew]; exists && keys != nil {
				snapshot.projectTopicNewBucketExists = true
				_, snapshot.projectTopicNewKeyPresent = keys[key]
			}
		} else {
			snapshot.projectTopicNewBucketExists = snapshot.projectTopicOldBucketExists
			snapshot.projectTopicNewKeyPresent = snapshot.projectTopicOldKeyPresent
		}
	}
	snapshot.projectKeyCount = m.currentKeyCountsByProject[projectKey]
	snapshot.projectTopicKeyCount = m.currentTopicKeyCountsByProject[projectKey]
	snapshot.projectKeyGeneration, snapshot.projectKeyGenerationExists = m.currentKeyIndexGeneration[projectKey]
	snapshot.projectTopicGeneration, snapshot.projectTopicGenerationExists = m.currentTopicIndexGeneration[projectKey]
	snapshot.latestTopic, snapshot.latestTopicExists = m.latestTopic[key]
	snapshot.latestHash, snapshot.latestHashExists = m.latestHash[key]
	snapshot.latestHorizon, snapshot.latestHorizonExists = m.latestHorizon[key]
	snapshot.latestLifecycle, snapshot.latestLifecycleExists = m.latestLifecycle[key]
	snapshot.latestStorageTier, snapshot.latestStorageTierExists = m.latestStorageTier[key]
	snapshot.lastAccess, snapshot.lastAccessExists = m.lastAccess[key]
	snapshot.confidence, snapshot.confidenceExists = m.confidence[key]
	snapshot.shardPayload, snapshot.shardPayloadExists = m.currentStateShardPayloads[snapshot.shard]
	snapshot.shardPayload = append([]byte(nil), snapshot.shardPayload...)
	snapshot.shardDigest, snapshot.shardDigestExists = m.currentStateShardDigests[snapshot.shard]
	if digests := m.currentStateProjectShardDigests[projectKey]; digests != nil {
		snapshot.projectShardDigests = make(map[int]string, len(digests))
		for shard, digest := range digests {
			snapshot.projectShardDigests[shard] = digest
		}
		snapshot.projectShardDigestsExists = true
	}
	return snapshot
}

func (m *memoryStore) restoreCurrentStateMutationSnapshotLocked(snapshot memoryCurrentStateMutationSnapshot) {
	if m == nil {
		return
	}
	if snapshot.previousStateExists {
		m.currentState[snapshot.key] = snapshot.previousState
	} else {
		delete(m.currentState, snapshot.key)
	}
	if m.currentStateByShard[snapshot.shard] == nil {
		m.currentStateByShard[snapshot.shard] = map[string]memoryCurrentState{}
	}
	if snapshot.previousShardStateExists {
		m.currentStateByShard[snapshot.shard][snapshot.key] = snapshot.previousShardState
	} else {
		delete(m.currentStateByShard[snapshot.shard], snapshot.key)
	}
	if snapshot.projectKeyMapExists {
		keys := m.currentKeysByProject[snapshot.project]
		if keys == nil {
			keys = map[string]struct{}{}
			m.currentKeysByProject[snapshot.project] = keys
		}
		if snapshot.projectKeyPresent {
			keys[snapshot.key] = struct{}{}
		} else {
			delete(keys, snapshot.key)
		}
	} else {
		delete(m.currentKeysByProject, snapshot.project)
	}
	if snapshot.projectTopicMapExists {
		topics := m.currentKeysByProjectTopic[snapshot.project]
		if topics == nil {
			topics = map[string]map[string]struct{}{}
			m.currentKeysByProjectTopic[snapshot.project] = topics
		}
		restoreTopicBucket := func(topic string, bucketExists, keyPresent bool) {
			if !bucketExists {
				delete(topics, topic)
				return
			}
			keys := topics[topic]
			if keys == nil {
				keys = map[string]struct{}{}
				topics[topic] = keys
			}
			if keyPresent {
				keys[snapshot.key] = struct{}{}
			} else {
				delete(keys, snapshot.key)
			}
		}
		restoreTopicBucket(snapshot.projectTopicOld, snapshot.projectTopicOldBucketExists, snapshot.projectTopicOldKeyPresent)
		if snapshot.projectTopicNew != snapshot.projectTopicOld {
			restoreTopicBucket(snapshot.projectTopicNew, snapshot.projectTopicNewBucketExists, snapshot.projectTopicNewKeyPresent)
		}
	} else {
		delete(m.currentKeysByProjectTopic, snapshot.project)
	}
	m.currentKeyCountsByProject[snapshot.project] = snapshot.projectKeyCount
	m.currentTopicKeyCountsByProject[snapshot.project] = snapshot.projectTopicKeyCount
	if snapshot.projectKeyGenerationExists {
		m.currentKeyIndexGeneration[snapshot.project] = snapshot.projectKeyGeneration
	} else {
		delete(m.currentKeyIndexGeneration, snapshot.project)
	}
	if snapshot.projectTopicGenerationExists {
		m.currentTopicIndexGeneration[snapshot.project] = snapshot.projectTopicGeneration
	} else {
		delete(m.currentTopicIndexGeneration, snapshot.project)
	}
	restoreString := func(values map[string]string, value string, exists bool) {
		if exists {
			values[snapshot.key] = value
		} else {
			delete(values, snapshot.key)
		}
	}
	restoreString(m.latestTopic, snapshot.latestTopic, snapshot.latestTopicExists)
	restoreString(m.latestHash, snapshot.latestHash, snapshot.latestHashExists)
	restoreString(m.latestLifecycle, snapshot.latestLifecycle, snapshot.latestLifecycleExists)
	restoreString(m.latestStorageTier, snapshot.latestStorageTier, snapshot.latestStorageTierExists)
	if snapshot.latestHorizonExists {
		m.latestHorizon[snapshot.key] = snapshot.latestHorizon
	} else {
		delete(m.latestHorizon, snapshot.key)
	}
	if snapshot.lastAccessExists {
		m.lastAccess[snapshot.key] = snapshot.lastAccess
	} else {
		delete(m.lastAccess, snapshot.key)
	}
	if snapshot.confidenceExists {
		m.confidence[snapshot.key] = snapshot.confidence
	} else {
		delete(m.confidence, snapshot.key)
	}
	if snapshot.recentFallback != nil {
		m.recent = snapshot.recentFallback
	} else if snapshot.recentWasNil && snapshot.recentLen == 0 {
		m.recent = nil
	} else if snapshot.recentLen == m.policy.maxRecent && snapshot.recentFirstExists {
		restored := make([]memoryStoreEntry, snapshot.recentLen)
		restored[0] = snapshot.recentFirst
		copy(restored[1:], m.recent[:minInt(len(m.recent), snapshot.recentLen-1)])
		m.recent = restored
	} else if len(m.recent) >= snapshot.recentLen {
		m.recent = m.recent[:snapshot.recentLen]
	}
	if !snapshot.rollupCacheMapExists {
		m.rollupCache = nil
	} else {
		if m.rollupCache == nil {
			m.rollupCache = map[string]topicRollupCacheEntry{}
		}
		cacheProject := normalizeRollupProject(snapshot.project)
		for cacheKey := range m.rollupCache {
			parts := strings.SplitN(cacheKey, "|", 2)
			if len(parts) == 2 && strings.EqualFold(parts[0], cacheProject) {
				delete(m.rollupCache, cacheKey)
			}
		}
		for cacheKey, entry := range snapshot.rollupCacheProjectEntries {
			m.rollupCache[cacheKey] = entry
		}
	}
	if snapshot.generationRecordsFallbackOK {
		m.currentStateGenerationRecords = snapshot.generationRecordsFallback
	} else if !snapshot.generationRecordsMapExists {
		m.currentStateGenerationRecords = nil
	} else {
		if m.currentStateGenerationRecords == nil {
			m.currentStateGenerationRecords = map[string]memoryCurrentStateGenerationRecord{}
		}
		if snapshot.generationRecordExists {
			m.currentStateGenerationRecords[snapshot.project] = snapshot.generationRecord
		} else {
			delete(m.currentStateGenerationRecords, snapshot.project)
		}
	}
	m.currentStateGenerationManifestLoaded = snapshot.generationManifestLoaded
	m.currentStateGenerationManifestVersion = snapshot.generationManifestVersion
	if !snapshot.generationCardsDigestInit {
		m.currentStateGenerationCardTree = nil
	} else if snapshot.generationRecordsFallbackOK || !snapshot.generationRecordsMapExists || snapshot.project == "" {
		// These are bounded migration/initialization paths. Rebuild the
		// authenticated tree from the restored map because there is no single
		// hot project snapshot that identifies the prior map shape.
		m.setCurrentStateGenerationCardsAccumulatorLocked(m.currentStateGenerationRecords)
	} else {
		m.rebuildCurrentStateGenerationCardTreeLocked(snapshot.project)
	}
	// Keep the scalar snapshot authoritative after the tree path repair. This
	// also preserves exact rollback semantics if a test fixture intentionally
	// starts with an uninitialized accumulator.
	m.currentStateGenerationCardCount = snapshot.generationCardCount
	m.currentStateGenerationCardBytes = snapshot.generationCardBytes
	m.currentStateGenerationCardsDigest = snapshot.generationCardsDigest
	m.currentStateGenerationCardsDigestInitialized = snapshot.generationCardsDigestInit
	if snapshot.shardPayloadExists {
		m.currentStateShardPayloads[snapshot.shard] = snapshot.shardPayload
	} else {
		delete(m.currentStateShardPayloads, snapshot.shard)
	}
	if snapshot.shardDigestExists {
		m.currentStateShardDigests[snapshot.shard] = snapshot.shardDigest
	} else {
		delete(m.currentStateShardDigests, snapshot.shard)
	}
	if snapshot.projectShardDigestsExists {
		m.currentStateProjectShardDigests[snapshot.project] = snapshot.projectShardDigests
	} else {
		delete(m.currentStateProjectShardDigests, snapshot.project)
	}
}

func (m *memoryStore) persistAndRecordEntry(entry memoryStoreEntry) error {
	if m == nil {
		return errors.New("memory store unavailable")
	}
	m.currentStateGenerationAdmissionMu.Lock()
	defer m.currentStateGenerationAdmissionMu.Unlock()
	return m.persistAndRecordEntryLocked(entry)
}

func (m *memoryStore) persistAndRecordEntryLocked(entry memoryStoreEntry) error {
	if m == nil {
		return errors.New("memory store unavailable")
	}
	key := memoryStoreKey(entry.Project, entry.FileName)
	shard := memoryCurrentStateShardForKey(key)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureCurrentStateMapLocked()
	m.ensureCurrentKeyIndexLocked()
	m.ensureCurrentStateDigestIndexesLocked()
	if err := m.preflightCurrentStateGenerationEntryLocked(entry.Project); err != nil {
		return fmt.Errorf("validate current-state generation card capacity before state mutation: %w", err)
	}
	snapshot := m.captureCurrentStateMutationSnapshotLocked(key, entry.Project, entry.TopicPath)
	changed := m.applyCurrentStateEntryLocked(entry)
	m.recordEntryWithState(entry, changed)
	if changed {
		projectKey := normalizeCurrentKeyIndexProject(entry.Project)
		generation := m.currentKeyIndexGeneration[projectKey]
		if err := m.persistCurrentStateTransactionLocked(map[int]struct{}{shard: {}}, entry.Project, generation); err != nil {
			if !errors.Is(err, errMemoryCurrentStateTransactionCommitted) {
				m.restoreCurrentStateMutationSnapshotLocked(snapshot)
			}
			return err
		}
	}
	return nil
}

func (m *memoryStore) currentStateFor(project, fileName string) (memoryCurrentState, bool) {
	if m == nil {
		return memoryCurrentState{}, false
	}
	key := memoryStoreKey(project, fileName)
	m.mu.RLock()
	state, ok := m.currentState[key]
	m.mu.RUnlock()
	if !ok {
		return memoryCurrentState{}, false
	}
	state.Entry.Tags = append([]string(nil), state.Entry.Tags...)
	return state, true
}

func (m *memoryStore) currentEntry(project, fileName string) (memoryStoreEntry, bool) {
	state, ok := m.currentStateFor(project, fileName)
	if !ok || state.Tombstone {
		return memoryStoreEntry{}, false
	}
	return state.Entry, true
}

func requestIncludesColdMemory(request map[string]any) bool {
	return anyToBool(request["include_cold"]) ||
		strings.EqualFold(strings.TrimSpace(anyToString(request["retrieval_mode"])), "deep")
}

func normalizeProjectedContentHash(raw string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(raw)), "sha256:")
}

type vectorReconcileStats struct {
	Suppressed       int
	CurrentEvent     int
	CurrentHash      int
	LegacyPathOnly   int
	StaleEvent       int
	HashMismatch     int
	MissingAuthority int
	LifecycleHidden  int
	DuplicatePath    int
}

func (stats vectorReconcileStats) warning(source string) string {
	return fmt.Sprintf(
		"%s authoritative memory state suppressed %d fallback result(s) (stale_event=%d hash_mismatch=%d missing_authority=%d lifecycle_hidden=%d duplicate_path=%d); accepted current_event=%d current_hash=%d legacy_path_only=%d",
		source,
		stats.Suppressed,
		stats.StaleEvent,
		stats.HashMismatch,
		stats.MissingAuthority,
		stats.LifecycleHidden,
		stats.DuplicatePath,
		stats.CurrentEvent,
		stats.CurrentHash,
		stats.LegacyPathOnly,
	)
}

type reconciledVectorCandidate struct {
	row      map[string]any
	priority int
	class    string
	order    int
}

// reconcileVectorRows performs one bounded in-memory authority pass over an
// already-bounded vector result set. Vector projections never become authority.
func (s *server) reconcileVectorRows(request map[string]any, rows []map[string]any) ([]map[string]any, int) {
	filtered, stats := s.reconcileVectorRowsDetailed(request, rows)
	return filtered, stats.Suppressed
}

func (s *server) reconcileVectorRowsDetailed(request map[string]any, rows []map[string]any) ([]map[string]any, vectorReconcileStats) {
	stats := vectorReconcileStats{}
	if len(rows) == 0 {
		return rows, stats
	}
	if s == nil || s.memoryStore == nil {
		stats.Suppressed = len(rows)
		stats.MissingAuthority = len(rows)
		return []map[string]any{}, stats
	}
	// Explicit vector-only deployments have no local lifecycle authority to
	// reconcile. Missing authority in an enabled deployment still fails closed.
	if !s.memoryStore.isEnabled() {
		return rows, stats
	}
	includeCold := requestIncludesColdMemory(request)
	includeEphemeral := requestIncludesEphemeralMemory(request)
	chosen := map[string]reconciledVectorCandidate{}
	s.memoryStore.mu.RLock()
	defer s.memoryStore.mu.RUnlock()
	for order, row := range rows {
		if row == nil {
			continue
		}
		key := memoryStoreKey(anyToString(row["project"]), anyToString(row["file"]))
		state, ok := s.memoryStore.currentState[key]
		if !ok || state.Tombstone {
			stats.Suppressed++
			stats.MissingAuthority++
			continue
		}
		lifecycle := normalizeMemoryLifecycle(state.Entry.Lifecycle)
		tier := normalizeMemoryStorageTier(state.Entry.StorageTier)
		if !shouldSurfaceMemoryLifecycle(lifecycle, includeEphemeral) ||
			(!includeCold && (tier == "deep" || tier == "retired")) {
			stats.Suppressed++
			stats.LifecycleHidden++
			continue
		}
		projectedEventID := strings.TrimSpace(anyToString(row["event_id"]))
		currentEventID := strings.TrimSpace(state.Entry.EventID)
		projectedHash := normalizeProjectedContentHash(anyToString(row["content_hash"]))
		currentHash := normalizeProjectedContentHash(state.Entry.ContentHash)
		authorityClass := "legacy_path_only"
		priority := 1
		switch {
		case projectedEventID != "":
			if currentEventID == "" || projectedEventID != currentEventID {
				stats.Suppressed++
				stats.StaleEvent++
				continue
			}
			authorityClass = "current_event"
			priority = 3
		case projectedHash != "":
			if currentHash == "" || projectedHash != currentHash {
				stats.Suppressed++
				stats.HashMismatch++
				continue
			}
			authorityClass = "current_hash"
			priority = 2
		}
		resolved := cloneAnyMap(row)
		resolved["project"] = state.Entry.Project
		resolved["file"] = state.Entry.FileName
		resolved["summary"] = state.Entry.Summary
		resolved["topic_path"] = state.Entry.TopicPath
		resolved["event_id"] = state.Entry.EventID
		resolved["content_hash"] = state.Entry.ContentHash
		resolved["lifecycle"] = lifecycle
		resolved["storage_tier"] = tier
		resolved["legal_hold"] = state.LegalHold
		resolved["projection_authority"] = authorityClass
		candidate := reconciledVectorCandidate{
			row:      resolved,
			priority: priority,
			class:    authorityClass,
			order:    order,
		}
		if existing, exists := chosen[key]; exists {
			stats.Suppressed++
			stats.DuplicatePath++
			if candidate.priority > existing.priority {
				candidate.order = existing.order
				chosen[key] = candidate
			}
			continue
		}
		chosen[key] = candidate
	}
	candidates := make([]reconciledVectorCandidate, 0, len(chosen))
	for _, candidate := range chosen {
		candidates = append(candidates, candidate)
		switch candidate.class {
		case "current_event":
			stats.CurrentEvent++
		case "current_hash":
			stats.CurrentHash++
		default:
			stats.LegacyPathOnly++
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].order < candidates[j].order
	})
	filtered := make([]map[string]any, 0, len(candidates))
	for _, candidate := range candidates {
		filtered = append(filtered, candidate.row)
	}
	return filtered, stats
}
