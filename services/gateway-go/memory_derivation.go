package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
)

const memoryDerivationRelation = "derived_from"

const (
	memoryDerivationMaxNodes = 64
	memoryDerivationMaxDepth = 8
	memoryDerivationMaxBytes = 2 * 1024 * 1024
)

var errMemoryDerivationUnavailable = errors.New("derived_memory_source_unavailable: original inputs require verification or recomputation")

func (s *server) memoryDerivationAvailable() bool {
	return s != nil && s.retrieval.enabled && s.memoryStore != nil && s.memoryStore.isEnabled() && strings.TrimSpace(s.memoryStore.policy.edgePath) != ""
}

func (s *server) memoryDerivationCapability() map[string]any {
	return map[string]any{"version": 1, "available": s.memoryDerivationAvailable(), "relation": memoryDerivationRelation,
		"scope": "same_project", "input_pins": []string{"target_event_id", "target_content_hash"},
		"automatic_recomputation": false, "max_nodes": memoryDerivationMaxNodes, "max_depth": memoryDerivationMaxDepth}
}

func (s *server) rejectUnavailableMemoryDerivations(w http.ResponseWriter, items []normalizedWrite) bool {
	if s.memoryDerivationAvailable() {
		return false
	}
	for _, item := range items {
		if memoryEntryHasDerivation(memoryStoreEntry{References: item.references}) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "derivation_custody_unavailable", "detail": "Native retrieval and durable Go memory/reference custody must be ready; no backend forwarding or unprotected write was attempted."})
			return true
		}
	}
	return false
}

func addMemoryDerivationWriteReceipt(payload map[string]any, entry memoryStoreEntry) map[string]any {
	count := 0
	for _, ref := range entry.References {
		if ref.Relation == memoryDerivationRelation {
			count++
		}
	}
	if count > 0 {
		if !memoryDerivationTransactionIDValid(entry.ReferenceTransactionID) {
			payload["ok"] = false
			payload["error"] = "derivation_custody_unconfirmed"
			payload["derivation_custody"] = map[string]any{"version": 1, "registered": false}
			return payload
		}
		payload["derivation_custody"] = map[string]any{"version": 1, "registered": true, "scope": "same_project", "input_count": count, "transaction_id": entry.ReferenceTransactionID}
	}
	return payload
}

func memoryDerivationTransactionIDValid(id string) bool {
	if !strings.HasPrefix(id, "ref_tx_") || len(id) != 39 {
		return false
	}
	_, err := hex.DecodeString(id[7:])
	return err == nil
}

func memoryEntryHasDerivation(entry memoryStoreEntry) bool {
	for _, ref := range entry.References {
		if ref.Relation == memoryDerivationRelation {
			return true
		}
	}
	return false
}

func memoryDerivationPinsValid(ref memoryStructuredReference) bool {
	return strings.TrimSpace(ref.TargetEventID) != "" && len(ref.TargetEventID) <= 160 &&
		isHexDigest(normalizeProjectedContentHash(ref.TargetContentHash))
}

func memoryDerivationReferenceMatchesTarget(ref memoryStructuredReference, target memoryStoreEntry) bool {
	return memoryDerivationPinsValid(ref) && strings.EqualFold(ref.TargetID, target.Project+"::"+target.FileName) &&
		ref.TargetEventID == target.EventID && normalizeProjectedContentHash(ref.TargetContentHash) == normalizeProjectedContentHash(target.ContentHash)
}

func memoryDerivationEntryDeclaresTarget(source, target memoryStoreEntry) bool {
	if !strings.EqualFold(source.Project, target.Project) {
		return false
	}
	for _, ref := range source.References {
		if ref.Relation == memoryDerivationRelation && memoryDerivationReferenceMatchesTarget(ref, target) {
			return true
		}
	}
	return false
}

func memoryDerivationBindingMatchesPins(source memoryStoreEntry, edge memoryEdgeEntry, allowLegacy bool) bool {
	for _, ref := range source.References {
		if ref.Relation != memoryDerivationRelation || !strings.EqualFold(ref.TargetID, edge.TargetID) {
			continue
		}
		// Older arbitrary relation labels remain structurally readable, but are
		// never current derivation custody. Do not turn one old row into a store-wide startup failure.
		if allowLegacy && ref.TargetEventID == "" && ref.TargetContentHash == "" {
			return true
		}
		return edge.Binding != nil && memoryDerivationPinsValid(ref) && edge.Binding.TargetEventID == ref.TargetEventID &&
			normalizeProjectedContentHash(edge.Binding.TargetContentHash) == normalizeProjectedContentHash(ref.TargetContentHash)
	}
	return false
}

func (m *memoryStore) validateMemoryDerivationRegistration(entry memoryStoreEntry, fence *memoryEdgeLogFenceToken) error {
	if memoryEntryHasDerivation(entry) && strings.TrimSpace(m.policy.edgePath) == "" {
		return errors.New("derivation requires durable reference custody")
	}
	if memoryEntryHasDerivation(entry) {
		if entry.DataClass == dataClassRuntimeStateMirror {
			return errors.New("runtime-state mirrors cannot register memory derivations")
		}
		exact, err := m.isExactStatePathWithFenceChecked(entry.Project, entry.FileName, fence)
		excluded, _ := m.memoryGraphArtifactExcluded(entry.Project, entry.FileName, entry.TopicPath)
		if err != nil || exact || excluded {
			return errors.New("derivative is outside graph-addressable memory")
		}
	}
	var walk *memoryDerivationWalk
	for _, ref := range entry.References {
		if ref.Relation != memoryDerivationRelation {
			continue
		}
		project, file, _, _, err := canonicalMemoryID(ref.TargetID)
		if err != nil || !strings.EqualFold(project, entry.Project) || strings.EqualFold(file, entry.FileName) {
			return errors.New("derivation requires a distinct same-project input")
		}
		target, ok := m.currentEntry(project, file)
		if !ok || !memoryDerivationReferenceMatchesTarget(ref, target) || !lifecycleSurfacesByDefault(target.Lifecycle) {
			return errors.New("derivation input is unavailable or no longer matches its pinned owner version")
		}
		exact, err := m.isExactStatePathWithFenceChecked(project, file, fence)
		excluded, _ := m.memoryGraphArtifactExcluded(project, file, target.TopicPath)
		if err != nil || exact || excluded {
			return errors.New("derivation input is outside graph-addressable memory")
		}
		if memoryEntryHasDerivation(target) {
			if walk == nil {
				walk = newMemoryDerivationWalk()
			}
			if err := m.checkMemoryDerivation(context.Background(), target, fence, walk, 0); err != nil {
				return errMemoryDerivationUnavailable
			}
		}
	}
	return nil
}

type memoryDerivationWalk struct {
	visiting map[string]bool
	verified map[string]bool
	projects map[string]bool
	nodes    int
	bytes    int
}

func newMemoryDerivationWalk() *memoryDerivationWalk {
	return &memoryDerivationWalk{visiting: map[string]bool{}, verified: map[string]bool{}, projects: map[string]bool{}, bytes: memoryDerivationMaxBytes}
}

// Check the owner projection against its durable project card while the common
// writer fence is held. A stale second process must abstain, not certify its cached map.
func (m *memoryStore) derivationOwnerCurrent(project string, walk *memoryDerivationWalk) error {
	key := normalizeCurrentKeyIndexProject(project)
	if walk.projects[key] {
		return nil
	}
	if _, err := os.Lstat(m.currentStateTransactionPath()); !errors.Is(err, os.ErrNotExist) {
		return errMemoryDerivationUnavailable
	}
	keys, topics, digest, err := m.durableCurrentStateGeneration(project)
	if err != nil {
		return errMemoryDerivationUnavailable
	}
	raw, err := readBoundedRegularFileNoFollow(m.currentStateGenerationCardPath(project), 4096)
	if err != nil {
		return errMemoryDerivationUnavailable
	}
	var card memoryCurrentStateGenerationCard
	if json.Unmarshal(raw, &card) != nil || card.SchemaID != memoryCurrentStateGenerationCardSchemaID || card.Version != memoryCurrentStateGenerationCardsVersion || card.Project != key ||
		card.Record.KeyGeneration != keys || card.Record.TopicGeneration != topics || card.Record.StateDigest != digest {
		return errMemoryDerivationUnavailable
	}
	walk.projects[key] = true
	return nil
}

func (m *memoryStore) checkMemoryDerivation(ctx context.Context, entry memoryStoreEntry, fence *memoryEdgeLogFenceToken, walk *memoryDerivationWalk, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	identity := strings.ToLower(entry.Project+"::"+entry.FileName) + "\x00" + entry.EventID
	if walk.visiting[identity] || depth > memoryDerivationMaxDepth {
		return errMemoryDerivationUnavailable
	}
	if walk.verified[identity] {
		return nil
	}
	walk.nodes++
	if walk.nodes > memoryDerivationMaxNodes || !lifecycleSurfacesByDefault(entry.Lifecycle) {
		return errMemoryDerivationUnavailable
	}
	if excluded, _ := m.memoryGraphArtifactExcluded(entry.Project, entry.FileName, entry.TopicPath); excluded {
		return errMemoryDerivationUnavailable
	}
	assessment := memoryTrustAssessmentForCandidate("memory", entry.Summary, map[string]any{"project": entry.Project, "file": entry.FileName, "source": "go_memory_store"})
	if anyToBool(anyMap(assessment["quarantine"])["quarantined"]) {
		return errMemoryDerivationUnavailable
	}
	if err := m.derivationOwnerCurrent(entry.Project, walk); err != nil {
		return err
	}
	current, ok := m.currentEntry(entry.Project, entry.FileName)
	if !ok || memoryReferenceSnapshotRow(current) != memoryReferenceSnapshotRow(entry) || current.ReferenceTransactionID != entry.ReferenceTransactionID ||
		!memoryStructuredReferencesEqual(current.References, entry.References) {
		return errMemoryDerivationUnavailable
	}
	if !memoryEntryHasDerivation(entry) {
		walk.verified[identity] = true
		return nil
	}
	if fence == nil || fence.validateFor(m) != nil || !memoryDerivationTransactionIDValid(entry.ReferenceTransactionID) {
		return errMemoryDerivationUnavailable
	}
	raw, err := readBoundedRegularFileNoFollow(m.memoryReferenceTransactionPath(entry.ReferenceTransactionID), int64(minInt(walk.bytes, memoryReferenceTransactionMaxBytes)))
	if err != nil {
		return errMemoryDerivationUnavailable
	}
	walk.bytes -= len(raw) + memoryReferenceReceiptMaxBytes
	if walk.bytes < 0 {
		return errMemoryDerivationUnavailable
	}
	var transaction memoryReferenceTransaction
	if json.Unmarshal(raw, &transaction) != nil || !memoryReferenceTransactionValid(transaction) || transaction.TransactionID != entry.ReferenceTransactionID ||
		memoryReferenceSnapshotRow(transaction.Entry) != memoryReferenceSnapshotRow(entry) || !memoryStructuredReferencesEqual(transaction.Entry.References, entry.References) {
		return errMemoryDerivationUnavailable
	}
	if _, closed, err := m.loadMemoryReferenceReceipt(transaction); err != nil || !closed {
		return errMemoryDerivationUnavailable
	}
	walk.visiting[identity] = true
	defer delete(walk.visiting, identity)
	for _, ref := range entry.References {
		if ref.Relation != memoryDerivationRelation {
			continue
		}
		project, file, _, _, err := canonicalMemoryID(ref.TargetID)
		if err != nil || !strings.EqualFold(project, entry.Project) {
			return errMemoryDerivationUnavailable
		}
		target, ok := m.currentEntry(project, file)
		if !ok || !memoryDerivationReferenceMatchesTarget(ref, target) {
			return errMemoryDerivationUnavailable
		}
		bound := false
		for _, edge := range transaction.Edges {
			if edge.Relation == memoryDerivationRelation && strings.EqualFold(edge.TargetID, ref.TargetID) && memoryDerivationBindingMatchesPins(entry, edge, false) && m.referenceEdgeCurrentWithFence(edge, fence) {
				bound = true
				break
			}
		}
		if !bound {
			return errMemoryDerivationUnavailable
		}
		if err := m.checkMemoryDerivation(ctx, target, fence, walk, depth+1); err != nil {
			return err
		}
	}
	walk.verified[identity] = true
	return nil
}

// Only owner-local declared derivations opt into this path; arbitrary retrieved
// metadata cannot grant provenance or turn every citation into a dependency.
func (s *server) filterDerivedMemoryRows(ctx context.Context, rows []map[string]any, referenceOnly bool) ([]map[string]any, *memoryDerivationGuard, int) {
	guard := &memoryDerivationGuard{}
	if s == nil || s.memoryStore == nil || !s.memoryStore.isEnabled() {
		return rows, guard, 0
	}
	m := s.memoryStore
	guard.store = m
	needsCheck := false
	for _, row := range rows {
		entry, ok := m.currentEntry(anyToString(row["project"]), anyToString(row["file"]))
		if ok && memoryEntryHasDerivation(entry) || memoryDerivationRowOwnerMismatch(row, entry, ok) {
			needsCheck = true
			break
		}
	}
	if !needsCheck {
		return rows, guard, 0
	}
	if ctx == nil {
		ctx = context.Background()
	}
	fence, err := m.acquireMemoryEdgeLogFenceOptionalContext(ctx)
	if fence != nil {
		defer fence.release()
	}
	walk := newMemoryDerivationWalk()
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		// The pre-scan is only a fast-path hint. Classify again under the writer
		// fence so a rewrite/removal cannot erase a previously derived row's version.
		entry, ok := m.currentEntry(anyToString(row["project"]), anyToString(row["file"]))
		if memoryDerivationRowOwnerMismatch(row, entry, ok) {
			continue
		}
		if !ok || !memoryEntryHasDerivation(entry) {
			out = append(out, row)
			continue
		}
		text := strings.TrimSpace(anyToString(row["summary"]))
		if err != nil || fence == nil || (!referenceOnly && (text == "" || !strings.HasPrefix(strings.TrimSpace(entry.Summary), text))) ||
			(anyToString(row["event_id"]) != "" && anyToString(row["event_id"]) != entry.EventID) ||
			(anyToString(row["content_hash"]) != "" && normalizeProjectedContentHash(anyToString(row["content_hash"])) != normalizeProjectedContentHash(entry.ContentHash)) ||
			(anyToString(row["content_ref"]) != "" && normalizeProjectedContentHash(anyToString(row["content_ref"])) != normalizeProjectedContentHash(entry.ContentHash)) ||
			m.checkMemoryDerivation(ctx, entry, fence, walk, 0) != nil {
			continue
		}
		copy := cloneAnyMap(row)
		copy["event_id"], copy["content_hash"] = entry.EventID, entry.ContentHash
		out = append(out, copy)
		guard.roots = append(guard.roots, entry)
	}
	if requestGuard, ok := ctx.Value(memoryDerivationContextKey{}).(*memoryDerivationGuard); ok {
		// Retain the first owner observation, not merely whatever declaration
		// happens to exist when the compiler later rediscovers the same file.
		requestGuard.capture(guard)
	}
	return out, guard, len(rows) - len(out)
}

func memoryDerivationRowOwnerMismatch(row map[string]any, entry memoryStoreEntry, present bool) bool {
	if !present {
		return row["projection_authority"] == "current_event" || row["projection_authority"] == "current_hash"
	}
	return anyToString(row["event_id"]) != "" && anyToString(row["event_id"]) != entry.EventID ||
		anyToString(row["content_hash"]) != "" && normalizeProjectedContentHash(anyToString(row["content_hash"])) != normalizeProjectedContentHash(entry.ContentHash)
}

type memoryDerivationContextKey struct{}

type memoryDerivationGuard struct {
	mu      sync.Mutex
	store   *memoryStore
	roots   []memoryStoreEntry
	invalid bool
}

func withMemoryDerivationGuard(ctx context.Context, store *memoryStore) (context.Context, *memoryDerivationGuard) {
	if ctx == nil {
		ctx = context.Background()
	}
	if guard, ok := ctx.Value(memoryDerivationContextKey{}).(*memoryDerivationGuard); ok {
		return ctx, guard
	}
	guard := &memoryDerivationGuard{store: store}
	return context.WithValue(ctx, memoryDerivationContextKey{}, guard), guard
}

func (g *memoryDerivationGuard) capture(other *memoryDerivationGuard) {
	if other == nil || len(other.roots) == 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.store != other.store {
		g.invalid = true
		return
	}
	for _, root := range other.roots {
		found := false
		for _, existing := range g.roots {
			found = found || memoryReferenceSnapshotRow(root) == memoryReferenceSnapshotRow(existing)
		}
		if !found {
			if len(g.roots) >= memoryDerivationMaxNodes {
				g.invalid = true
				return
			}
			g.roots = append(g.roots, root)
		}
	}
}

func (g *memoryDerivationGuard) verify(ctx context.Context) error {
	g.mu.Lock()
	roots := append([]memoryStoreEntry(nil), g.roots...)
	invalid := g.invalid
	g.mu.Unlock()
	if invalid {
		return errMemoryDerivationUnavailable
	}
	if len(roots) == 0 {
		return nil
	}
	if g.store == nil || len(roots) > memoryDerivationMaxNodes {
		return errMemoryDerivationUnavailable
	}
	fence, err := g.store.acquireMemoryEdgeLogFenceOptionalContext(ctx)
	if err != nil || fence == nil {
		return errMemoryDerivationUnavailable
	}
	defer fence.release()
	walk := newMemoryDerivationWalk()
	for _, root := range roots {
		if err := g.store.checkMemoryDerivation(ctx, root, fence, walk, 0); err != nil {
			return errMemoryDerivationUnavailable
		}
	}
	return nil
}

func (s *server) captureContextPackDerivations(ctx context.Context, searchResponse map[string]any, guard *memoryDerivationGuard) error {
	rows := parseRows(searchResponse["results"])
	if raw, present := searchResponse[recallResponseSourceInputKey]; present {
		if captured, valid := recallResponseSourceInputRows(raw); valid {
			rows = captured
		}
	}
	_, observed, suppressed := s.filterDerivedMemoryRows(ctx, rows, false)
	if suppressed > 0 {
		return errMemoryDerivationUnavailable
	}
	guard.capture(observed)
	return guard.verify(ctx)
}
