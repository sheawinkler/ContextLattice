package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	memoryStructuredReferenceSchemaVersion   = "memory_reference_claim.v1"
	memoryStructuredReferenceParserVersion   = "memory_reference_parser.v1"
	memoryStructuredReferenceMaxClaims       = 32
	memoryStructuredReferenceMaxTargetBytes  = 512
	memoryReferenceBackfillMaxBlobBytes      = 64 * 1024
	memoryReferenceBackfillMaxTotalBytes     = 2 * 1024 * 1024
	memoryReferenceBackfillMaxBlobCount      = 128
	memoryReferenceSnapshotMaxDocs           = 250000
	memoryReferenceTransactionMaxStartup     = 4096
	memoryReferenceTransactionAdmissionSlack = 64
	memoryReferenceTransactionMaxStored      = memoryReferenceTransactionMaxStartup + memoryReferenceTransactionAdmissionSlack
	memoryReferenceTransactionEdgeIndexMax   = memoryReferenceTransactionMaxStored * memoryStructuredReferenceMaxClaims
	memoryReferenceCursorMaxState            = 4096
	memoryReferenceCursorTTL                 = 15 * time.Minute
	memoryReferenceCursorReservationTTL      = time.Minute
	memoryReferenceCursorStateMaxBytes       = 4 * 1024 * 1024
	memoryReferenceTransactionMaxBytes       = 512 * 1024
	memoryReferenceReceiptMaxBytes           = 64 * 1024
	memoryReferenceHistoryIndexMaxBytes      = 64 * 1024
)

func readBoundedRegularFileNoFollow(path string, maxBytes int64) ([]byte, error) {
	file, descriptorSize, err := openBoundedRegularFileNoFollow(path, maxBytes)
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, maxBytes+1))
	identityErr := ownerOnlyLockPathIdentityMatches(path, file)
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if identityErr != nil {
		return nil, fmt.Errorf("bounded regular file path changed while it was read: %w", identityErr)
	}
	if int64(len(raw)) != descriptorSize || int64(len(raw)) > maxBytes {
		return nil, errors.New("bounded regular file changed while it was read")
	}
	return raw, nil
}

func memoryReferenceTransactionIDFromEdge(edge memoryEdgeEntry) string {
	return strings.TrimSpace(anyToString(edge.Metadata["reference_transaction_id"]))
}

func (m *memoryStore) refreshMemoryReferenceEdgeIndexLocked(snapshot memoryEdgeLogSnapshot) error {
	transactions, err := m.loadMemoryReferenceTransactions()
	if err != nil {
		return err
	}
	activeTransactions := make(map[string]struct{}, len(transactions))
	for _, transaction := range transactions {
		activeTransactions[transaction.TransactionID] = struct{}{}
	}
	index := map[string]map[string]string{}
	taggedRows := 0
	scanner := bufio.NewScanner(bytes.NewReader(snapshot.Bytes))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var edge memoryEdgeEntry
		if json.Unmarshal(scanner.Bytes(), &edge) != nil {
			continue
		}
		transactionID := memoryReferenceTransactionIDFromEdge(edge)
		if transactionID == "" {
			continue
		}
		// Retired transactions leave immutable historical rows in the canonical
		// append-only edge log. Only discoverable pending transactions can be
		// reconciled, so indexing retired IDs would eventually exhaust the
		// bounded active-transaction row cap after ordinary replacement churn.
		if _, active := activeTransactions[transactionID]; !active {
			continue
		}
		taggedRows++
		if taggedRows > memoryReferenceTransactionEdgeIndexMax {
			return errors.New("reference transaction edge index exceeds bounded row cap")
		}
		normalized, err := edge.normalized()
		if err != nil {
			return errors.New("durable reference transaction contains a noncanonical edge")
		}
		rows := index[transactionID]
		if rows == nil {
			rows = map[string]string{}
			index[transactionID] = rows
		}
		digest := memoryReferenceEdgeDigest(normalized)
		if previous, exists := rows[normalized.EdgeID]; exists && previous != digest {
			return errors.New("durable reference transaction contains conflicting duplicate edges")
		}
		rows[normalized.EdgeID] = digest
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	m.referenceEdgeIndex = index
	m.referenceEdgeIndexGeneration = snapshot.Generation
	m.referenceEdgeIndexDigest = snapshot.Digest
	return nil
}

func (m *memoryStore) retainMemoryReferenceEdgeIndexTransactions(transactions []memoryReferenceTransaction) {
	if m == nil {
		return
	}
	active := make(map[string]struct{}, len(transactions))
	for _, transaction := range transactions {
		active[transaction.TransactionID] = struct{}{}
	}
	m.referenceEdgeIndexMu.Lock()
	defer m.referenceEdgeIndexMu.Unlock()
	for transactionID := range m.referenceEdgeIndex {
		if _, ok := active[transactionID]; !ok {
			delete(m.referenceEdgeIndex, transactionID)
		}
	}
}

// appendMemoryReferenceEdgeSet completes a transaction with the canonical
// edge-log appender while holding its single writer fence. Rows already
// persisted by an interrupted attempt are verified and skipped; the closed
// receipt separately controls visibility of the complete set.
func (m *memoryStore) appendMemoryReferenceEdgeSet(transactionID string, edges []memoryEdgeEntry) (memoryEdgeLogState, error) {
	ctx := context.Background()
	fence, err := m.acquireMemoryEdgeLogFenceContext(ctx)
	if err != nil {
		return memoryEdgeLogState{}, err
	}
	defer fence.release()
	return m.appendMemoryReferenceEdgeSetWithFence(ctx, transactionID, edges, fence)
}

func (m *memoryStore) appendMemoryReferenceEdgeSetWithFence(ctx context.Context, transactionID string, edges []memoryEdgeEntry, fence *memoryEdgeLogFenceToken) (memoryEdgeLogState, error) {
	if err := requireMemoryEdgeLogFence(m, fence); err != nil {
		return memoryEdgeLogState{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return memoryEdgeLogState{}, err
	}
	transactionID = strings.TrimSpace(transactionID)
	if transactionID == "" || len(edges) == 0 {
		return memoryEdgeLogState{}, errors.New("reference edge transaction and edge set are required")
	}
	expected := make(map[string]memoryEdgeEntry, len(edges))
	for _, edge := range edges {
		normalized, err := edge.normalized()
		if err != nil {
			return memoryEdgeLogState{}, err
		}
		if memoryReferenceTransactionIDFromEdge(normalized) != transactionID {
			return memoryEdgeLogState{}, errors.New("reference edge transaction metadata mismatch")
		}
		expected[normalized.EdgeID] = normalized
	}

	m.referenceEdgeIndexMu.Lock()
	defer m.referenceEdgeIndexMu.Unlock()
	appender, err := m.newMemoryEdgeLogAppenderFastWithFenceContextLocked(ctx, true, fence)
	if err != nil {
		return memoryEdgeLogState{}, err
	}
	if m.referenceEdgeIndex == nil || m.referenceEdgeIndexGeneration != appender.state.Generation || m.referenceEdgeIndexDigest != appender.state.Digest {
		snapshot, snapshotErr := m.snapshotMemoryEdgeLogContextLocked(ctx, 0)
		if snapshotErr != nil {
			return memoryEdgeLogState{}, snapshotErr
		}
		if err := m.refreshMemoryReferenceEdgeIndexLocked(snapshot); err != nil {
			return memoryEdgeLogState{}, err
		}
		appender, err = newMemoryEdgeLogAppenderWithFenceLocked(m, snapshot, true, fence)
		if err != nil {
			return memoryEdgeLogState{}, err
		}
	}
	found := m.referenceEdgeIndex[transactionID]
	for edgeID, digest := range found {
		want, ok := expected[edgeID]
		if !ok || digest != memoryReferenceEdgeDigest(want) {
			return memoryEdgeLogState{}, errors.New("durable reference transaction edge conflicts with pending edge set")
		}
	}
	state := appender.state
	ordered := append([]memoryEdgeEntry(nil), edges...)
	sortMemoryReferenceEdges(ordered)
	appendCount := 0
	for _, edge := range ordered {
		if _, ok := found[edge.EdgeID]; ok {
			continue
		}
		_, state, err = appender.append(edge)
		if err != nil {
			return memoryEdgeLogState{}, err
		}
		if found == nil {
			found = map[string]string{}
			m.referenceEdgeIndex[transactionID] = found
		}
		found[edge.EdgeID] = memoryReferenceEdgeDigest(edge)
		m.referenceEdgeIndexGeneration = state.Generation
		m.referenceEdgeIndexDigest = state.Digest
		appendCount++
		// This fault boundary deliberately leaves a durable partial set. The
		// missing receipt keeps it invisible and restart reconciliation resumes
		// from the verified rows above without duplicating them.
		if appendCount == 1 && m.beforeReferenceEdgeSync != nil {
			if err := m.beforeReferenceEdgeSync(); err != nil {
				return memoryEdgeLogState{}, err
			}
		}
	}
	return state, nil
}

func clampMemoryReferenceInt64(value int64, lower int64, upper int64) int64 {
	if value < lower {
		return lower
	}
	if value > upper {
		return upper
	}
	return value
}

// memoryStructuredReference is the typed write-time claim. TargetID is
// canonicalized before it is stored; callers must provide project::file so a
// claim cannot silently resolve against an unrelated project.
type memoryStructuredReference struct {
	TargetID   string  `json:"target_id"`
	Relation   string  `json:"relation"`
	Confidence float64 `json:"confidence,omitempty"`
}

type memoryReferenceCursorState struct {
	CursorID    string `json:"cursor_id"`
	TokenDigest string `json:"token_digest"`
	IssuedAt    string `json:"issued_at"`
	ExpiresAt   string `json:"expires_at"`
	Reservation string `json:"reservation,omitempty"`
	ReservedAt  string `json:"reserved_at,omitempty"`
	ConsumedAt  string `json:"consumed_at,omitempty"`
}

type memoryReferenceCursorStateFile struct {
	SchemaID string                       `json:"schema_id"`
	Version  int                          `json:"version"`
	Entries  []memoryReferenceCursorState `json:"entries"`
}

type memoryReferenceCursorPayload struct {
	Version          int    `json:"v"`
	CursorID         string `json:"cursor_id"`
	RequestDigest    string `json:"request_digest"`
	Project          string `json:"project"`
	Corpus           string `json:"corpus"`
	RelationDigest   string `json:"relation_digest"`
	SnapshotDigest   string `json:"snapshot_digest"`
	GenerationDigest string `json:"generation_digest"`
	DocSetDigest     string `json:"doc_set_digest"`
	LastDocKey       string `json:"last_doc_key"`
	LastEdgeID       string `json:"last_edge_id,omitempty"`
	IssuedAt         string `json:"issued_at"`
	ExpiresAt        string `json:"expires_at"`
}

func (m *memoryStore) memoryReferenceCursorKeyPath() string {
	return filepath.Join(m.policy.rootPath, "_contextlattice", "memory_reference_cursor.key")
}

func (m *memoryStore) memoryReferenceCursorStatePath() string {
	return filepath.Join(m.policy.rootPath, "_contextlattice", "memory_reference_cursor_state.json")
}

func (m *memoryStore) memoryReferenceCursorSigningKey() ([]byte, error) {
	m.referenceCursorKeyOnce.Do(func() {
		path := m.memoryReferenceCursorKeyPath()
		if err := ensureOwnerOnlyDirectory(filepath.Dir(path), true); err != nil {
			m.referenceCursorKeyErr = err
			return
		}
		unlock, err := lockOwnerOnlyFile(path + ".writer.lock")
		if err != nil {
			m.referenceCursorKeyErr = err
			return
		}
		defer unlock()
		if raw, err := readBoundedRegularFileNoFollow(path, 32); err == nil {
			if len(raw) != 32 {
				m.referenceCursorKeyErr = errors.New("reference cursor signing key is invalid")
				return
			}
			m.referenceCursorKey = append([]byte(nil), raw...)
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			m.referenceCursorKeyErr = err
			return
		}
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			m.referenceCursorKeyErr = err
			return
		}
		if err := writeOwnerOnlyDurableAtomicFile(path, key, true); err != nil {
			m.referenceCursorKeyErr = err
			return
		}
		m.referenceCursorKey = key
	})
	return append([]byte(nil), m.referenceCursorKey...), m.referenceCursorKeyErr
}

func memoryReferenceCursorTokenDigest(token string) string {
	return "sha256:" + sha256Hex(token)
}

func (m *memoryStore) withMemoryReferenceCursorState(update func(map[string]memoryReferenceCursorState) error) error {
	m.referenceCursorMu.Lock()
	defer m.referenceCursorMu.Unlock()
	path := m.memoryReferenceCursorStatePath()
	unlock, err := lockOwnerOnlyFile(path + ".writer.lock")
	if err != nil {
		return err
	}
	defer unlock()
	state := map[string]memoryReferenceCursorState{}
	if raw, err := readBoundedRegularFileNoFollow(path, memoryReferenceCursorStateMaxBytes); err == nil {
		var payload memoryReferenceCursorStateFile
		if json.Unmarshal(raw, &payload) != nil || payload.SchemaID != "contextlattice_memory_reference_cursor_state.v1" || payload.Version != 1 || len(payload.Entries) > memoryReferenceCursorMaxState {
			return errors.New("reference cursor state is invalid")
		}
		for _, entry := range payload.Entries {
			state[entry.CursorID] = entry
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	now := time.Now().UTC()
	for id, entry := range state {
		expires, ok := parseTimeBestEffort(entry.ExpiresAt)
		if !ok || !now.Before(expires) {
			delete(state, id)
		}
	}
	if err := update(state); err != nil {
		return err
	}
	if len(state) > memoryReferenceCursorMaxState {
		return errors.New("reference cursor state capacity reached")
	}
	entries := make([]memoryReferenceCursorState, 0, len(state))
	for _, entry := range state {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IssuedAt != entries[j].IssuedAt {
			return entries[i].IssuedAt < entries[j].IssuedAt
		}
		return entries[i].CursorID < entries[j].CursorID
	})
	raw, err := json.MarshalIndent(memoryReferenceCursorStateFile{SchemaID: "contextlattice_memory_reference_cursor_state.v1", Version: 1, Entries: entries}, "", "  ")
	if err != nil {
		return err
	}
	return writeOwnerOnlyDurableAtomicFile(path, append(raw, '\n'), true)
}

func (m *memoryStore) encodeMemoryReferenceCursor(payload memoryReferenceCursorPayload) (string, error) {
	key, err := m.memoryReferenceCursorSigningKey()
	if err != nil || len(key) != 32 {
		return "", errors.New("reference cursor signing key is unavailable")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(encoded))
	token := encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	state := memoryReferenceCursorState{CursorID: payload.CursorID, TokenDigest: memoryReferenceCursorTokenDigest(token), IssuedAt: payload.IssuedAt, ExpiresAt: payload.ExpiresAt}
	if err := m.withMemoryReferenceCursorState(func(entries map[string]memoryReferenceCursorState) error {
		if len(entries) >= memoryReferenceCursorMaxState {
			return errors.New("reference cursor state capacity reached")
		}
		if _, exists := entries[state.CursorID]; exists {
			return errors.New("reference cursor identity collision")
		}
		entries[state.CursorID] = state
		return nil
	}); err != nil {
		return "", err
	}
	return token, nil
}

func (m *memoryStore) decodeAndReserveMemoryReferenceCursor(token string, expected memoryReferenceCursorPayload) (memoryReferenceCursorPayload, string, error) {
	if len(token) < 32 || len(token) > 4096 || strings.ContainsAny(token, " \t\r\n") {
		return memoryReferenceCursorPayload{}, "", errors.New("reference continuation is malformed")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return memoryReferenceCursorPayload{}, "", errors.New("reference continuation is malformed")
	}
	key, err := m.memoryReferenceCursorSigningKey()
	if err != nil || len(key) != 32 {
		return memoryReferenceCursorPayload{}, "", errors.New("reference cursor signing key is unavailable")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return memoryReferenceCursorPayload{}, "", errors.New("reference continuation signature is malformed")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(parts[0]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return memoryReferenceCursorPayload{}, "", errors.New("reference continuation signature is invalid")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return memoryReferenceCursorPayload{}, "", errors.New("reference continuation payload is malformed")
	}
	var payload memoryReferenceCursorPayload
	if json.Unmarshal(raw, &payload) != nil || payload.Version != 1 || payload.CursorID == "" {
		return memoryReferenceCursorPayload{}, "", errors.New("reference continuation payload is invalid")
	}
	expires, ok := parseTimeBestEffort(payload.ExpiresAt)
	if !ok || !time.Now().UTC().Before(expires) {
		return memoryReferenceCursorPayload{}, "", errors.New("reference continuation is stale")
	}
	if payload.RequestDigest != expected.RequestDigest || payload.Project != expected.Project || payload.Corpus != expected.Corpus || payload.RelationDigest != expected.RelationDigest ||
		payload.SnapshotDigest != expected.SnapshotDigest || payload.GenerationDigest != expected.GenerationDigest || payload.DocSetDigest != expected.DocSetDigest {
		return memoryReferenceCursorPayload{}, "", errors.New("reference continuation does not match the canonical request or current snapshot")
	}
	reservationBytes := make([]byte, 16)
	if _, err := rand.Read(reservationBytes); err != nil {
		return memoryReferenceCursorPayload{}, "", errors.New("reference cursor reservation is unavailable")
	}
	reservation := base64.RawURLEncoding.EncodeToString(reservationBytes)
	if err := m.withMemoryReferenceCursorState(func(entries map[string]memoryReferenceCursorState) error {
		state, exists := entries[payload.CursorID]
		if !exists || state.TokenDigest != memoryReferenceCursorTokenDigest(token) {
			return errors.New("reference continuation is unknown or stale")
		}
		if state.ConsumedAt != "" {
			return errors.New("reference continuation replay is not allowed")
		}
		if state.Reservation != "" {
			reservedAt, ok := parseTimeBestEffort(state.ReservedAt)
			if ok && time.Now().UTC().Before(reservedAt.Add(memoryReferenceCursorReservationTTL)) {
				return errors.New("reference continuation is already in progress")
			}
		}
		state.Reservation = reservation
		state.ReservedAt = nowUTCISO()
		entries[payload.CursorID] = state
		return nil
	}); err != nil {
		return memoryReferenceCursorPayload{}, "", err
	}
	return payload, reservation, nil
}

func (m *memoryStore) finishMemoryReferenceCursor(token, cursorID, reservation string, consume bool) error {
	if token == "" || cursorID == "" || reservation == "" {
		return nil
	}
	return m.withMemoryReferenceCursorState(func(entries map[string]memoryReferenceCursorState) error {
		state, exists := entries[cursorID]
		if !exists || state.TokenDigest != memoryReferenceCursorTokenDigest(token) || state.Reservation != reservation || state.ConsumedAt != "" {
			return errors.New("reference continuation reservation is no longer current")
		}
		state.Reservation = ""
		state.ReservedAt = ""
		if consume {
			state.ConsumedAt = nowUTCISO()
		}
		entries[cursorID] = state
		return nil
	})
}

func normalizeMemoryStructuredReferences(raw map[string]any) ([]memoryStructuredReference, error) {
	if raw == nil {
		return nil, nil
	}
	claims := make([]memoryStructuredReference, 0)
	if value, exists := raw["references"]; exists {
		rows, err := parseMemoryStructuredReferenceList(value, "references")
		if err != nil {
			return nil, err
		}
		claims = append(claims, rows...)
	}
	if value, exists := raw["relations"]; exists {
		rows, err := parseMemoryStructuredReferenceList(value, "relations")
		if err != nil {
			return nil, err
		}
		claims = append(claims, rows...)
	}
	return canonicalizeMemoryStructuredReferences(claims)
}

func parseMemoryStructuredReferenceList(value any, field string) ([]memoryStructuredReference, error) {
	rows, ok := memoryStructuredReferenceAnySlice(value)
	if !ok {
		return nil, fmt.Errorf("%s must be an array", field)
	}
	claims := make([]memoryStructuredReference, 0, len(rows))
	for index, raw := range rows {
		if text, ok := raw.(string); ok {
			if field != "references" {
				return nil, fmt.Errorf("%s[%d] must be an object with target and relation", field, index)
			}
			claims = append(claims, memoryStructuredReference{TargetID: text, Relation: "references", Confidence: 1})
			continue
		}
		object, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s[%d] must be a string or object", field, index)
		}
		target, err := uniqueMemoryReferenceTarget(object, field, index)
		if err != nil {
			return nil, err
		}
		relation := strings.TrimSpace(anyToString(object["relation"]))
		if relation == "" {
			relation = strings.TrimSpace(anyToString(object["type"]))
		}
		if field == "references" {
			if relation == "" {
				relation = "references"
			}
			if normalized, normalizeErr := normalizeMemoryEdgeRelation(relation); normalizeErr != nil || normalized != "references" {
				return nil, fmt.Errorf("%s[%d] relation must be references", field, index)
			}
		} else if relation == "" {
			return nil, fmt.Errorf("%s[%d] relation is required", field, index)
		}
		confidence := 1.0
		if _, present := object["confidence"]; present {
			confidence = anyToFloat64(object["confidence"], -1)
			if confidence < 0 || confidence > 1 {
				return nil, fmt.Errorf("%s[%d] confidence must be between 0 and 1", field, index)
			}
		}
		claims = append(claims, memoryStructuredReference{TargetID: target, Relation: relation, Confidence: confidence})
	}
	return claims, nil
}

func memoryStructuredReferenceAnySlice(value any) ([]any, bool) {
	if rows, ok := value.([]any); ok {
		return rows, true
	}
	if rows, ok := value.([]string); ok {
		out := make([]any, 0, len(rows))
		for _, row := range rows {
			out = append(out, row)
		}
		return out, true
	}
	if rows, ok := value.([]map[string]any); ok {
		out := make([]any, 0, len(rows))
		for _, row := range rows {
			out = append(out, row)
		}
		return out, true
	}
	if rows, ok := value.([]memoryStructuredReference); ok {
		out := make([]any, 0, len(rows))
		for _, row := range rows {
			out = append(out, map[string]any{
				"target_id":  row.TargetID,
				"relation":   row.Relation,
				"confidence": row.Confidence,
			})
		}
		return out, true
	}
	return nil, false
}

func uniqueMemoryReferenceTarget(object map[string]any, field string, index int) (string, error) {
	aliases := []string{"target_id", "target", "memory_id", "memoryId"}
	values := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		if value, present := object[alias]; present {
			trimmed := strings.TrimSpace(anyToString(value))
			if trimmed == "" {
				return "", fmt.Errorf("%s[%d] %s must be non-empty", field, index, alias)
			}
			values = append(values, trimmed)
		}
	}
	if len(values) == 0 {
		return "", fmt.Errorf("%s[%d] target is required", field, index)
	}
	canonical := ""
	for _, value := range values {
		if !strings.Contains(value, "::") {
			return "", fmt.Errorf("%s[%d] target must use project::file form", field, index)
		}
		_, _, normalized, _, err := canonicalMemoryID(value)
		if err != nil {
			return "", fmt.Errorf("%s[%d] target: %w", field, index, err)
		}
		if canonical == "" {
			canonical = normalized
		} else if canonical != normalized {
			return "", fmt.Errorf("%s[%d] contains ambiguous target aliases", field, index)
		}
	}
	if len(canonical) > memoryStructuredReferenceMaxTargetBytes {
		return "", fmt.Errorf("%s[%d] target exceeds %d bytes", field, index, memoryStructuredReferenceMaxTargetBytes)
	}
	return canonical, nil
}

func canonicalizeMemoryStructuredReferences(claims []memoryStructuredReference) ([]memoryStructuredReference, error) {
	if len(claims) == 0 {
		return nil, nil
	}
	if len(claims) > memoryStructuredReferenceMaxClaims {
		return nil, fmt.Errorf("references exceed maximum of %d claims", memoryStructuredReferenceMaxClaims)
	}
	seen := make(map[string]struct{}, len(claims))
	out := make([]memoryStructuredReference, 0, len(claims))
	for index, claim := range claims {
		if !strings.Contains(strings.TrimSpace(claim.TargetID), "::") {
			return nil, fmt.Errorf("reference %d target must use project::file form", index)
		}
		_, _, targetID, _, err := canonicalMemoryID(claim.TargetID)
		if err != nil {
			return nil, fmt.Errorf("reference %d target: %w", index, err)
		}
		relation, err := normalizeMemoryEdgeRelation(claim.Relation)
		if err != nil {
			return nil, fmt.Errorf("reference %d relation: %w", index, err)
		}
		confidence := claim.Confidence
		if confidence == 0 && claim.Relation == "" {
			confidence = 1
		}
		if confidence < 0 || confidence > 1 {
			return nil, fmt.Errorf("reference %d confidence must be between 0 and 1", index)
		}
		key := strings.ToLower(relation + "\x00" + targetID)
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("reference %d duplicates target and relation", index)
		}
		seen[key] = struct{}{}
		out = append(out, memoryStructuredReference{TargetID: targetID, Relation: relation, Confidence: confidence})
	}
	sort.Slice(out, func(i, j int) bool {
		left := strings.ToLower(out[i].Relation + "\x00" + out[i].TargetID)
		right := strings.ToLower(out[j].Relation + "\x00" + out[j].TargetID)
		return left < right
	})
	return out, nil
}

func memoryStructuredReferencesEqual(left, right []memoryStructuredReference) bool {
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

func memoryReferenceBindingValid(binding *memoryReferenceBinding) bool {
	return binding != nil &&
		binding.SchemaVersion == memoryStructuredReferenceSchemaVersion &&
		binding.ParserVersion == memoryStructuredReferenceParserVersion &&
		strings.TrimSpace(binding.RelationSemantic) != "" &&
		isHexDigest(strings.TrimPrefix(strings.ToLower(binding.SourceContentHash), "sha256:")) &&
		isHexDigest(strings.TrimPrefix(strings.ToLower(binding.TargetContentHash), "sha256:")) &&
		strings.TrimSpace(binding.SourceEventID) != "" &&
		strings.TrimSpace(binding.TargetEventID) != "" &&
		strings.TrimSpace(binding.SourceTopicPath) != "" &&
		strings.TrimSpace(binding.TargetTopicPath) != "" &&
		strings.TrimSpace(binding.SourceLifecycle) != "" &&
		strings.TrimSpace(binding.TargetLifecycle) != "" &&
		memoryReferenceDigestValid(binding.SemanticDigest) &&
		binding.SourceIndexGeneration > 0 &&
		binding.TargetIndexGeneration > 0 &&
		memoryReferenceDigestValid(binding.DocSetDigest) &&
		memoryReferenceDigestValid(binding.ExclusionPolicyDigest) &&
		strings.TrimSpace(binding.BoundAt) != ""
}

// memoryReferenceBindingsSameCurrentState compares pair-local liveness and
// semantic identity. Project-wide generation/doc-set custody and BoundAt are
// intentionally excluded: unrelated writes may advance them without changing
// either endpoint, while every source/target version change is represented by
// the event, content, topic, lifecycle, session/agent, and semantic fields.
func memoryReferenceBindingsSameCurrentState(left, right *memoryReferenceBinding) bool {
	if !memoryReferenceBindingValid(left) || !memoryReferenceBindingValid(right) {
		return false
	}
	return left.SchemaVersion == right.SchemaVersion &&
		left.ParserVersion == right.ParserVersion &&
		left.RelationSemantic == right.RelationSemantic &&
		strings.EqualFold(left.SourceContentHash, right.SourceContentHash) &&
		strings.EqualFold(left.TargetContentHash, right.TargetContentHash) &&
		left.SourceEventID == right.SourceEventID &&
		left.TargetEventID == right.TargetEventID &&
		strings.EqualFold(left.SourceTopicPath, right.SourceTopicPath) &&
		strings.EqualFold(left.TargetTopicPath, right.TargetTopicPath) &&
		left.SourceSessionID == right.SourceSessionID &&
		left.TargetSessionID == right.TargetSessionID &&
		strings.EqualFold(left.SourceAgentID, right.SourceAgentID) &&
		strings.EqualFold(left.TargetAgentID, right.TargetAgentID) &&
		left.SourceLifecycle == right.SourceLifecycle &&
		left.TargetLifecycle == right.TargetLifecycle &&
		left.SemanticDigest == right.SemanticDigest &&
		left.ExclusionPolicyDigest == right.ExclusionPolicyDigest
}

func memoryGraphBindingSemanticDigest(relation string, source, target memoryStoreEntry) string {
	material := strings.Join([]string{
		strings.TrimSpace(relation),
		memoryReferenceSnapshotRow(source),
		memoryReferenceSnapshotRow(target),
	}, "\x00")
	return "sha256:" + sha256Hex(material)
}

func memoryReferenceDigestValid(value string) bool {
	value = strings.TrimSpace(strings.ToLower(value))
	return strings.HasPrefix(value, "sha256:") && isHexDigest(strings.TrimPrefix(value, "sha256:"))
}

func (m *memoryStore) referenceExclusionPolicyDigest() string {
	if m == nil {
		return "sha256:" + sha256Hex("nil")
	}
	payload := map[string]any{
		"graph_exclude_low_value":      m.policy.graphExcludeLowValue,
		"graph_exclude_topic_prefixes": append([]string(nil), m.policy.graphExcludeTopicPrefixes...),
		"graph_exclude_file_patterns":  append([]string(nil), m.policy.graphExcludeFilePatterns...),
		"graph_exclude_file_suffixes":  append([]string(nil), m.policy.graphExcludeFileSuffixes...),
		"graph_exclude_root_json":      append([]string(nil), m.policy.graphExcludeRootJSON...),
	}
	raw, _ := json.Marshal(payload)
	return "sha256:" + sha256Hex(string(raw))
}

type memoryReferenceSnapshot struct {
	Entries                    map[string]memoryStoreEntry
	Generations                map[string]uint64
	IndexSizes                 map[string]int
	IndexCounts                map[string]int
	ExcludedGraphDocsByProject map[string]int
	IndexAvailable             bool
	DocSetDigest               string
	GenerationDigest           string
	ExclusionDigest            string
	CapturedAt                 string
	DocCount                   int
}

func (snapshot *memoryReferenceSnapshot) excludedGraphDocCount(project string) int {
	if snapshot == nil {
		return 0
	}
	project = normalizeCurrentKeyIndexProject(project)
	if project != "" {
		return snapshot.ExcludedGraphDocsByProject[project]
	}
	total := 0
	for _, count := range snapshot.ExcludedGraphDocsByProject {
		total += count
	}
	return total
}

func memoryReferenceSnapshotRow(entry memoryStoreEntry) string {
	return strings.Join([]string{
		strings.ToLower(memoryStoreKey(entry.Project, entry.FileName)),
		strings.TrimSpace(entry.EventID),
		strings.ToLower(strings.TrimPrefix(entry.ContentHash, "sha256:")),
		strings.ToLower(strings.Trim(entry.TopicPath, "/")),
		normalizeMemoryLifecycle(entry.Lifecycle),
		strings.TrimSpace(entry.SessionID),
		strings.TrimSpace(entry.AgentID),
	}, "\x00")
}

func (m *memoryStore) captureMemoryReferenceSnapshot(ctx context.Context, sourceOverride *memoryStoreEntry) (*memoryReferenceSnapshot, error) {
	if m == nil || !m.isEnabled() {
		return nil, errors.New("go memory store is disabled")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	limit := m.policy.scanLimit
	if limit < 1 || limit > memoryReferenceSnapshotMaxDocs {
		limit = memoryReferenceSnapshotMaxDocs
	}
	m.mu.RLock()
	if len(m.currentState) > limit {
		m.mu.RUnlock()
		return nil, fmt.Errorf("current-state reference snapshot exceeds bounded document cap: %d > %d", len(m.currentState), limit)
	}
	states := make([]memoryCurrentState, 0, len(m.currentState))
	for _, state := range m.currentState {
		if len(states)%256 == 0 {
			select {
			case <-ctx.Done():
				m.mu.RUnlock()
				return nil, ctx.Err()
			default:
			}
		}
		state.Entry.Tags = append([]string(nil), state.Entry.Tags...)
		state.Entry.References = append([]memoryStructuredReference(nil), state.Entry.References...)
		states = append(states, state)
	}
	generations := make(map[string]uint64, len(m.currentKeyIndexGeneration))
	for project, generation := range m.currentKeyIndexGeneration {
		generations[project] = generation
	}
	indexSizes := make(map[string]int, len(m.currentKeysByProject))
	for project, keys := range m.currentKeysByProject {
		indexSizes[project] = len(keys)
	}
	indexCounts := make(map[string]int, len(m.currentKeyCountsByProject))
	for project, count := range m.currentKeyCountsByProject {
		indexCounts[project] = count
	}
	indexAvailable := m.currentKeysByProject != nil && m.currentKeyCountsByProject != nil
	exactPaths := make(map[string]struct{}, len(m.exactStatePaths))
	for key := range m.exactStatePaths {
		exactPaths[key] = struct{}{}
	}
	m.mu.RUnlock()

	entries := make(map[string]memoryStoreEntry, len(states)+1)
	excludedGraphDocsByProject := map[string]int{}
	for index, state := range states {
		if index%256 == 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
		}
		entry := state.Entry
		if state.Tombstone || isMemoryTombstone(entry) || exactStatePathSetContains(exactPaths, entry.Project, entry.FileName) || !shouldSurfaceMemoryLifecycle(entry.Lifecycle, false) {
			continue
		}
		if excluded, _ := m.memoryGraphArtifactExcluded(entry.Project, entry.FileName, entry.TopicPath); excluded {
			excludedGraphDocsByProject[normalizeCurrentKeyIndexProject(entry.Project)]++
			continue
		}
		if strings.TrimSpace(entry.EventID) == "" || !isHexDigest(strings.TrimPrefix(strings.ToLower(entry.ContentHash), "sha256:")) {
			continue
		}
		entries[strings.ToLower(entry.Project+"::"+entry.FileName)] = entry
	}
	if sourceOverride != nil {
		entry := *sourceOverride
		entry.Tags = append([]string(nil), sourceOverride.Tags...)
		entry.References = append([]memoryStructuredReference(nil), sourceOverride.References...)
		if excluded, reason := m.memoryGraphArtifactExcluded(entry.Project, entry.FileName, entry.TopicPath); excluded {
			return nil, fmt.Errorf("reference source is not graph-addressable (%s)", reason)
		}
		key := strings.ToLower(entry.Project + "::" + entry.FileName)
		previous, existed := entries[key]
		entries[key] = entry
		projectKey := normalizeCurrentKeyIndexProject(entry.Project)
		generation := generations[projectKey]
		if !existed || previous.EventID != entry.EventID || previous.ContentHash != entry.ContentHash {
			generation++
		}
		if generation == 0 {
			generation = 1
		}
		generations[projectKey] = generation
	}
	rows := make([]string, 0, len(entries))
	for _, entry := range entries {
		rows = append(rows, memoryReferenceSnapshotRow(entry))
	}
	sort.Strings(rows)
	generationRows := make([]string, 0, len(generations))
	for project, generation := range generations {
		generationRows = append(generationRows, strings.ToLower(project)+"\x00"+fmt.Sprintf("%d", generation))
	}
	sort.Strings(generationRows)
	snapshot := &memoryReferenceSnapshot{
		Entries:                    entries,
		Generations:                generations,
		IndexSizes:                 indexSizes,
		IndexCounts:                indexCounts,
		ExcludedGraphDocsByProject: excludedGraphDocsByProject,
		IndexAvailable:             indexAvailable,
		DocSetDigest:               "sha256:" + sha256Hex(strings.Join(rows, "\n")),
		GenerationDigest:           "sha256:" + sha256Hex(strings.Join(generationRows, "\n")),
		ExclusionDigest:            m.referenceExclusionPolicyDigest(),
		CapturedAt:                 nowUTCISO(),
		DocCount:                   len(entries),
	}
	if m.referenceSnapshotCaptured != nil {
		m.referenceSnapshotCaptured(snapshot.DocCount)
	}
	return snapshot, nil
}

func (snapshot *memoryReferenceSnapshot) entry(memoryID string) (memoryStoreEntry, uint64, error) {
	if snapshot == nil {
		return memoryStoreEntry{}, 0, errors.New("reference snapshot is unavailable")
	}
	project, _, canonical, _, err := canonicalMemoryID(memoryID)
	if err != nil {
		return memoryStoreEntry{}, 0, err
	}
	entry, ok := snapshot.Entries[strings.ToLower(canonical)]
	if !ok {
		return memoryStoreEntry{}, 0, errors.New("current-state endpoint is unavailable in the bounded snapshot")
	}
	generation := snapshot.Generations[normalizeCurrentKeyIndexProject(project)]
	if generation == 0 {
		generation = 1
	}
	return entry, generation, nil
}

func (m *memoryStore) buildMemoryReferenceEdge(source memoryStoreEntry, target memoryStoreEntry, sourceGeneration uint64, targetGeneration uint64, docSetDigest string, exclusionDigest string, claim memoryStructuredReference, claimKind string) (memoryEdgeEntry, error) {
	sourceID := source.Project + "::" + source.FileName
	targetID := target.Project + "::" + target.FileName
	if sourceID == targetID {
		return memoryEdgeEntry{}, errors.New("reference claim cannot target its source")
	}
	now := nowUTCISO()
	return memoryEdgeEntry{
		EdgeID:     deterministicMemoryEdgeID(sourceID, claim.Relation, targetID),
		SourceID:   sourceID,
		TargetID:   targetID,
		Relation:   claim.Relation,
		Project:    source.Project,
		TopicPath:  source.TopicPath,
		Confidence: claim.Confidence,
		Provenance: map[string]any{"kind": claimKind, "schema_version": memoryStructuredReferenceSchemaVersion, "parser_version": memoryStructuredReferenceParserVersion},
		Metadata:   map[string]any{"structured": claimKind == "structured_write", "claim_kind": claimKind},
		AgentID:    source.AgentID,
		SessionID:  source.SessionID,
		Lifecycle:  normalizeMemoryLifecycle(source.Lifecycle),
		CreatedAt:  now,
		Source:     memoryEdgeSource,
		Binding: &memoryReferenceBinding{
			SchemaVersion:         memoryStructuredReferenceSchemaVersion,
			ParserVersion:         memoryStructuredReferenceParserVersion,
			RelationSemantic:      claim.Relation,
			SourceContentHash:     strings.TrimPrefix(strings.ToLower(source.ContentHash), "sha256:"),
			TargetContentHash:     strings.TrimPrefix(strings.ToLower(target.ContentHash), "sha256:"),
			SourceEventID:         source.EventID,
			TargetEventID:         target.EventID,
			SourceTopicPath:       sanitizeTopicPath(source.TopicPath, source.FileName),
			TargetTopicPath:       sanitizeTopicPath(target.TopicPath, target.FileName),
			SourceSessionID:       strings.TrimSpace(source.SessionID),
			TargetSessionID:       strings.TrimSpace(target.SessionID),
			SourceAgentID:         strings.TrimSpace(source.AgentID),
			TargetAgentID:         strings.TrimSpace(target.AgentID),
			SourceLifecycle:       normalizeMemoryLifecycle(source.Lifecycle),
			TargetLifecycle:       normalizeMemoryLifecycle(target.Lifecycle),
			SemanticDigest:        memoryGraphBindingSemanticDigest(claim.Relation, source, target),
			SourceIndexGeneration: sourceGeneration,
			TargetIndexGeneration: targetGeneration,
			DocSetDigest:          docSetDigest,
			ExclusionPolicyDigest: exclusionDigest,
			BoundAt:               now,
		},
	}, nil
}

func memoryGraphRelationRequiresBinding(relation string) bool {
	switch strings.TrimSpace(relation) {
	case "references", "same_session", "same_topic", "same_agent", "inferred_related":
		return true
	default:
		return false
	}
}

func memoryGraphEdgeRequiresBinding(edge memoryEdgeEntry) bool {
	if memoryGraphRepairEdgeRetired(edge) {
		return false
	}
	if edge.Binding != nil {
		return true
	}
	return memoryGraphRelationRequiresBinding(edge.Relation)
}

// Explicit legacy evidence may remain in the append-only custody log so graph
// repair can classify it. It is never loaded into or surfaced from the current
// graph because every active promoted relation still requires a valid binding.
func memoryGraphEdgeAllowsUnboundEvidence(edge memoryEdgeEntry) bool {
	kind := strings.ToLower(strings.TrimSpace(anyToString(edge.Provenance["kind"])))
	if strings.Contains(kind, "legacy") {
		return true
	}
	return edge.Relation == "inferred_related" && anyToBool(edge.Metadata["inferred"]) && !anyToBool(edge.Metadata["backfill"])
}

func (m *memoryStore) memoryReferenceSnapshotFromGraphRepair(snapshot memoryGraphRepairSnapshot, capturedAt string) *memoryReferenceSnapshot {
	entries := make(map[string]memoryStoreEntry, len(snapshot.Docs))
	generations := map[string]uint64{}
	generation := snapshot.KeyGeneration
	if generation == 0 {
		generation = 1
	}
	for _, doc := range snapshot.Docs {
		entry := memoryStoreEntry{
			EventID: doc.EventID, Project: doc.Project, FileName: doc.FileName, TopicPath: doc.TopicPath,
			Summary: doc.Summary, ContentHash: strings.TrimPrefix(strings.ToLower(doc.ContentHash), "sha256:"),
			ContentRef: "sha256:" + strings.TrimPrefix(strings.ToLower(doc.ContentHash), "sha256:"),
			ObjectID:   doc.ObjectID, AgentID: doc.AgentID, SessionID: doc.SessionID,
			Lifecycle: normalizeMemoryLifecycle(doc.Lifecycle), StorageTier: normalizeMemoryStorageTier(doc.StorageTier),
			CreatedAt: doc.CreatedAt, LastAccess: doc.LastAccess,
			References: append([]memoryStructuredReference(nil), doc.References...),
		}
		entries[strings.ToLower(doc.MemoryID)] = entry
		generations[normalizeCurrentKeyIndexProject(doc.Project)] = generation
	}
	generationDigest := "sha256:" + sha256Hex(fmt.Sprintf("%d\x00%d", snapshot.KeyGeneration, snapshot.TopicGeneration))
	return &memoryReferenceSnapshot{
		Entries: entries, Generations: generations, DocSetDigest: snapshot.SnapshotDigest,
		GenerationDigest: generationDigest, ExclusionDigest: m.referenceExclusionPolicyDigest(),
		CapturedAt: capturedAt, DocCount: len(entries),
	}
}

func (m *memoryStore) bindPromotedMemoryEdge(snapshot *memoryReferenceSnapshot, edge memoryEdgeEntry) (memoryEdgeEntry, error) {
	normalized, err := edge.normalized()
	if err != nil {
		return memoryEdgeEntry{}, err
	}
	if !memoryGraphRelationRequiresBinding(normalized.Relation) {
		return normalized, nil
	}
	source, sourceGeneration, err := snapshot.entry(normalized.SourceID)
	if err != nil {
		return memoryEdgeEntry{}, err
	}
	target, targetGeneration, err := snapshot.entry(normalized.TargetID)
	if err != nil {
		return memoryEdgeEntry{}, err
	}
	sourceTopic := sanitizeTopicPath(source.TopicPath, source.FileName)
	targetTopic := sanitizeTopicPath(target.TopicPath, target.FileName)
	switch normalized.Relation {
	case "same_session":
		if strings.TrimSpace(source.SessionID) == "" || source.SessionID != target.SessionID {
			return memoryEdgeEntry{}, errors.New("same_session endpoints do not share an exact current session")
		}
	case "same_topic":
		if !strings.EqualFold(strings.Trim(sourceTopic, "/"), strings.Trim(targetTopic, "/")) {
			return memoryEdgeEntry{}, errors.New("same_topic endpoints do not share an exact current topic")
		}
	case "same_agent":
		if strings.TrimSpace(source.AgentID) == "" || !strings.EqualFold(source.AgentID, target.AgentID) || !strings.EqualFold(strings.Trim(sourceTopic, "/"), strings.Trim(targetTopic, "/")) {
			return memoryEdgeEntry{}, errors.New("same_agent endpoints do not share exact current agent and topic semantics")
		}
	case "inferred_related":
		sourceInferred := memoryEdgeInferredDoc{doc: memoryEdgeBackfillDoc{Project: source.Project, FileName: source.FileName, MemoryID: normalized.SourceID, TopicPath: sourceTopic, Summary: source.Summary}, tokens: memoryEdgeInferenceTokens(memoryEdgeBackfillDoc{Project: source.Project, FileName: source.FileName, TopicPath: sourceTopic, Summary: source.Summary})}
		targetInferred := memoryEdgeInferredDoc{doc: memoryEdgeBackfillDoc{Project: target.Project, FileName: target.FileName, MemoryID: normalized.TargetID, TopicPath: targetTopic, Summary: target.Summary}, tokens: memoryEdgeInferenceTokens(memoryEdgeBackfillDoc{Project: target.Project, FileName: target.FileName, TopicPath: targetTopic, Summary: target.Summary})}
		shared := 0
		for token := range sourceInferred.tokens {
			if _, ok := targetInferred.tokens[token]; ok {
				shared++
			}
		}
		minimumShared := anyToInt(normalized.Metadata["min_shared_terms"], anyToInt(normalized.Provenance["min_shared_terms"], 1))
		score, _ := inferredMemoryEdgeScore(sourceInferred, targetInferred, shared)
		minimumScore := anyToFloat64(normalized.Metadata["min_score"], anyToFloat64(normalized.Provenance["min_score"], normalized.Confidence))
		if shared < minimumShared || score < minimumScore {
			return memoryEdgeEntry{}, errors.New("inferred_related endpoints do not satisfy exact current scoring semantics")
		}
	}
	if normalized.Binding == nil {
		normalized.Binding = &memoryReferenceBinding{}
	}
	*normalized.Binding = memoryReferenceBinding{
		SchemaVersion: memoryStructuredReferenceSchemaVersion, ParserVersion: memoryStructuredReferenceParserVersion,
		RelationSemantic: normalized.Relation, SourceContentHash: strings.TrimPrefix(strings.ToLower(source.ContentHash), "sha256:"), TargetContentHash: strings.TrimPrefix(strings.ToLower(target.ContentHash), "sha256:"),
		SourceEventID: source.EventID, TargetEventID: target.EventID, SourceTopicPath: sourceTopic, TargetTopicPath: targetTopic,
		SourceSessionID: strings.TrimSpace(source.SessionID), TargetSessionID: strings.TrimSpace(target.SessionID), SourceAgentID: strings.TrimSpace(source.AgentID), TargetAgentID: strings.TrimSpace(target.AgentID),
		SourceLifecycle: normalizeMemoryLifecycle(source.Lifecycle), TargetLifecycle: normalizeMemoryLifecycle(target.Lifecycle), SemanticDigest: memoryGraphBindingSemanticDigest(normalized.Relation, source, target),
		SourceIndexGeneration: sourceGeneration, TargetIndexGeneration: targetGeneration, DocSetDigest: snapshot.DocSetDigest, ExclusionPolicyDigest: snapshot.ExclusionDigest, BoundAt: snapshot.CapturedAt,
	}
	return normalized, nil
}

const (
	memoryReferenceTransactionSchemaID = "contextlattice_memory_graph_reference_transaction.v1"
	memoryReferenceReceiptSchemaID     = "contextlattice_memory_graph_reference_receipt.v1"
)

type memoryReferenceTransaction struct {
	SchemaID       string            `json:"schema_id"`
	Version        int               `json:"version"`
	TransactionID  string            `json:"transaction_id"`
	RequestDigest  string            `json:"request_digest"`
	PreparedAt     string            `json:"prepared_at"`
	ContentRef     string            `json:"content_ref"`
	HistoryDigest  string            `json:"history_digest"`
	EdgeSetDigest  string            `json:"edge_set_digest"`
	SnapshotDigest string            `json:"snapshot_digest"`
	Entry          memoryStoreEntry  `json:"entry"`
	Edges          []memoryEdgeEntry `json:"edges"`
}

type memoryReferenceTransactionReceipt struct {
	SchemaID          string `json:"schema_id"`
	Version           int    `json:"version"`
	TransactionID     string `json:"transaction_id"`
	RequestDigest     string `json:"request_digest"`
	HistoryDigest     string `json:"history_digest"`
	EdgeSetDigest     string `json:"edge_set_digest"`
	EdgeLogGeneration uint64 `json:"edge_log_generation"`
	EdgeLogDigest     string `json:"edge_log_digest"`
	ClosedAt          string `json:"closed_at"`
	ReceiptDigest     string `json:"receipt_digest"`
}

const memoryReferenceHistoryIndexSchemaID = "contextlattice_memory_graph_reference_history_index.v1"

// memoryReferenceHistoryIndex binds one transaction event to its exact byte
// range in the append-only history. It is persisted before the append, so a
// crash after the history fsync but before any later acknowledgement can be
// recovered with one bounded descriptor read instead of rescanning history.
type memoryReferenceHistoryIndex struct {
	SchemaID       string `json:"schema_id"`
	Version        int    `json:"version"`
	TransactionID  string `json:"transaction_id"`
	EventID        string `json:"event_id"`
	HistoryDigest  string `json:"history_digest"`
	Offset         int64  `json:"offset"`
	Length         int64  `json:"length"`
	PreparedAt     string `json:"prepared_at"`
	AcknowledgedAt string `json:"acknowledged_at,omitempty"`
	IndexDigest    string `json:"index_digest"`
}

func sortMemoryReferenceEdges(edges []memoryEdgeEntry) {
	sort.Slice(edges, func(i, j int) bool {
		return edges[i].EdgeID < edges[j].EdgeID
	})
}

func memoryReferenceEdgeDigest(edge memoryEdgeEntry) string {
	raw, _ := json.Marshal(edge)
	return "sha256:" + sha256Hex(string(raw))
}

func memoryReferenceEdgeSetDigest(edges []memoryEdgeEntry) string {
	ordered := append([]memoryEdgeEntry(nil), edges...)
	sortMemoryReferenceEdges(ordered)
	rows := make([]string, 0, len(ordered))
	for _, edge := range ordered {
		rows = append(rows, edge.EdgeID+"\x00"+memoryReferenceEdgeDigest(edge))
	}
	return "sha256:" + sha256Hex(strings.Join(rows, "\n"))
}

func memoryReferenceTransactionRequestDigest(entry memoryStoreEntry, snapshot *memoryReferenceSnapshot) string {
	targetRows := make([]string, 0, len(entry.References))
	for _, claim := range entry.References {
		target, _, err := snapshot.entry(claim.TargetID)
		if err != nil {
			targetRows = append(targetRows, "missing:"+strings.ToLower(claim.TargetID))
			continue
		}
		targetRows = append(targetRows, strings.ToLower(claim.Relation)+"\x00"+memoryReferenceSnapshotRow(target))
	}
	sort.Strings(targetRows)
	material := map[string]any{
		"project": entry.Project, "file": entry.FileName, "topic_path": entry.TopicPath,
		"event_id": entry.EventID, "created_at": entry.CreatedAt,
		"agent_id": entry.AgentID, "session_id": entry.SessionID, "tags": entry.Tags,
		"content_hash": entry.ContentHash, "lifecycle": entry.Lifecycle, "storage_tier": entry.StorageTier,
		"horizon_days": entry.HorizonDays, "task_attribution": entry.TaskAttribution, "references": entry.References,
		"target_snapshot_digest":  "sha256:" + sha256Hex(strings.Join(targetRows, "\n")),
		"source_index_generation": snapshot.Generations[normalizeCurrentKeyIndexProject(entry.Project)],
		"generation_digest":       snapshot.GenerationDigest,
		"doc_set_digest":          snapshot.DocSetDigest,
		"exclusion_policy_digest": snapshot.ExclusionDigest,
	}
	raw, _ := json.Marshal(material)
	return "sha256:" + sha256Hex(string(raw))
}

func (m *memoryStore) memoryReferenceTransactionRoot() string {
	return filepath.Join(m.policy.rootPath, "_contextlattice", "memory_reference_transactions")
}

func (m *memoryStore) memoryReferenceTransactionPath(transactionID string) string {
	return filepath.Join(m.memoryReferenceTransactionRoot(), transactionID+".pending.json")
}

func (m *memoryStore) memoryReferenceReceiptPath(transactionID string) string {
	return filepath.Join(m.memoryReferenceTransactionRoot(), transactionID+".receipt.json")
}

func (m *memoryStore) memoryReferenceHistoryIndexPath(transactionID string) string {
	return filepath.Join(m.memoryReferenceTransactionRoot(), transactionID+".history-index.json")
}

func memoryReferenceHistoryIndexDigest(index memoryReferenceHistoryIndex) string {
	index.IndexDigest = ""
	raw, _ := json.Marshal(index)
	return "sha256:" + sha256Hex(string(raw))
}

func (m *memoryStore) loadMemoryReferenceHistoryIndex(transaction memoryReferenceTransaction) (memoryReferenceHistoryIndex, bool, error) {
	raw, err := readBoundedRegularFileNoFollow(m.memoryReferenceHistoryIndexPath(transaction.TransactionID), memoryReferenceHistoryIndexMaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		return memoryReferenceHistoryIndex{}, false, nil
	}
	if err != nil {
		return memoryReferenceHistoryIndex{}, false, err
	}
	var index memoryReferenceHistoryIndex
	if json.Unmarshal(raw, &index) != nil || index.SchemaID != memoryReferenceHistoryIndexSchemaID || index.Version != 1 ||
		index.TransactionID != transaction.TransactionID || index.EventID != transaction.Entry.EventID || index.HistoryDigest != transaction.HistoryDigest ||
		index.Offset < 0 || index.Length < 1 || index.Length > memoryReferenceTransactionMaxBytes || strings.TrimSpace(index.PreparedAt) == "" ||
		index.IndexDigest != memoryReferenceHistoryIndexDigest(index) {
		return index, false, errors.New("reference transaction history index is invalid")
	}
	if _, ok := parseTimeBestEffort(index.PreparedAt); !ok {
		return index, false, errors.New("reference transaction history index preparation time is invalid")
	}
	if strings.TrimSpace(index.AcknowledgedAt) != "" {
		if _, ok := parseTimeBestEffort(index.AcknowledgedAt); !ok {
			return index, false, errors.New("reference transaction history index acknowledgement time is invalid")
		}
	}
	return index, true, nil
}

func (m *memoryStore) writeMemoryReferenceHistoryIndex(transaction memoryReferenceTransaction, offset int64, length int64) (memoryReferenceHistoryIndex, error) {
	index := memoryReferenceHistoryIndex{
		SchemaID: memoryReferenceHistoryIndexSchemaID, Version: 1, TransactionID: transaction.TransactionID,
		EventID: transaction.Entry.EventID, HistoryDigest: transaction.HistoryDigest, Offset: offset, Length: length, PreparedAt: nowUTCISO(),
	}
	return m.persistMemoryReferenceHistoryIndex(index)
}

func (m *memoryStore) persistMemoryReferenceHistoryIndex(index memoryReferenceHistoryIndex) (memoryReferenceHistoryIndex, error) {
	index.IndexDigest = memoryReferenceHistoryIndexDigest(index)
	raw, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return index, err
	}
	if err := writeOwnerOnlyDurableAtomicFile(m.memoryReferenceHistoryIndexPath(index.TransactionID), append(raw, '\n'), true); err != nil {
		return index, err
	}
	return index, nil
}

func (m *memoryStore) acknowledgeMemoryReferenceHistoryIndex(index memoryReferenceHistoryIndex) (memoryReferenceHistoryIndex, error) {
	if strings.TrimSpace(index.AcknowledgedAt) != "" {
		return index, nil
	}
	index.AcknowledgedAt = nowUTCISO()
	return m.persistMemoryReferenceHistoryIndex(index)
}

func memoryReferenceTransactionValid(transaction memoryReferenceTransaction) bool {
	if transaction.SchemaID != memoryReferenceTransactionSchemaID || transaction.Version != 1 || !strings.HasPrefix(transaction.TransactionID, "ref_tx_") ||
		!memoryReferenceDigestValid(transaction.RequestDigest) || !memoryReferenceDigestValid(transaction.HistoryDigest) || !memoryReferenceDigestValid(transaction.EdgeSetDigest) ||
		!memoryReferenceDigestValid(transaction.SnapshotDigest) || !memoryReferenceDigestValid(transaction.ContentRef) || len(transaction.Edges) == 0 || len(transaction.Edges) > memoryStructuredReferenceMaxClaims {
		return false
	}
	if transaction.TransactionID != "ref_tx_"+sha256Hex(transaction.RequestDigest + "\x00" + transaction.Entry.Project + "::" + transaction.Entry.FileName)[:32] ||
		transaction.Entry.ReferenceTransactionID != transaction.TransactionID ||
		!isHexDigest(strings.TrimPrefix(strings.ToLower(transaction.Entry.ContentHash), "sha256:")) || transaction.ContentRef != "sha256:"+strings.TrimPrefix(strings.ToLower(transaction.Entry.ContentHash), "sha256:") ||
		transaction.Entry.RawBytes < 1 || len(transaction.Entry.References) != len(transaction.Edges) {
		return false
	}
	entryJSON, err := json.Marshal(transaction.Entry)
	if err != nil || transaction.HistoryDigest != "sha256:"+sha256Hex(string(entryJSON)) || transaction.EdgeSetDigest != memoryReferenceEdgeSetDigest(transaction.Edges) {
		return false
	}
	seenClaims := map[string]struct{}{}
	for _, edge := range transaction.Edges {
		claimKey := strings.ToLower(edge.Relation + "\x00" + edge.TargetID)
		if _, duplicate := seenClaims[claimKey]; duplicate || !memoryReferenceClaimPresent(transaction.Entry, edge) || memoryReferenceTransactionIDFromEdge(edge) != transaction.TransactionID || !memoryReferenceBindingValid(edge.Binding) ||
			edge.Binding.SourceEventID != transaction.Entry.EventID || !strings.EqualFold(edge.Binding.SourceContentHash, transaction.Entry.ContentHash) || edge.Binding.DocSetDigest != transaction.SnapshotDigest {
			return false
		}
		seenClaims[claimKey] = struct{}{}
	}
	return true
}

func (m *memoryStore) ensureReferenceTransactionBlob(contentHash, content string) (string, error) {
	path, err := m.blobPathForHash(contentHash)
	if err != nil {
		return "", err
	}
	if file, size, openErr := openBoundedRegularFileNoFollow(path, int64(len(content))); openErr == nil {
		if m.memoryReferenceBlobDescriptorOpened != nil {
			m.memoryReferenceBlobDescriptorOpened()
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, int64(len(content))+1))
		info, statErr := file.Stat()
		identityErr := ownerOnlyLockPathIdentityMatches(path, file)
		closeErr := file.Close()
		if readErr == nil && statErr == nil && identityErr == nil && closeErr == nil &&
			int64(len(raw)) <= int64(len(content)) && size == int64(len(raw)) && info.Size() == size &&
			string(raw) == content && canonicalMemoryContentHash(string(raw)) == contentHash {
			return path, nil
		}
		return "", errors.New("existing reference transaction blob failed descriptor-bound validation")
	} else if !errors.Is(openErr, os.ErrNotExist) {
		return "", openErr
	}
	if err := ensureOwnerOnlyDirectory(filepath.Dir(path), true); err != nil {
		return "", err
	}
	if err := writeOwnerOnlyDurableAtomicFile(path, []byte(content), true); err != nil {
		return "", err
	}
	return path, nil
}

func (m *memoryStore) cleanupMemoryReferenceTransactionOrphansWithFence(transactions []memoryReferenceTransaction, fence *memoryEdgeLogFenceToken) error {
	if err := requireMemoryEdgeLogFenceOptional(m, fence); err != nil {
		return err
	}
	root := m.memoryReferenceTransactionRoot()
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	active := make(map[string]struct{}, len(transactions))
	for _, transaction := range transactions {
		active[transaction.TransactionID] = struct{}{}
	}
	removed := false
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		orphan := false
		for _, suffix := range []string{".receipt.json", ".history-index.json"} {
			if strings.HasSuffix(name, suffix) {
				_, live := active[strings.TrimSuffix(name, suffix)]
				orphan = !live
				break
			}
		}
		// Durable atomic writes use same-directory dot-prefixed temporaries. No
		// transaction artifact writer can be active while this fence is held, so
		// an observed transaction temp is necessarily left by an interrupted
		// process and is safe to retire.
		if strings.HasPrefix(name, ".ref_tx_") && strings.Contains(name, ".tmp-") {
			orphan = true
		}
		if !orphan {
			continue
		}
		if err := os.Remove(filepath.Join(root, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		removed = true
	}
	if removed {
		return syncOwnerOnlyDirectory(root)
	}
	return nil
}

// retireSupersededMemoryReferenceTransactionsWithFence removes transactions
// whose source has a newer/tombstoned current state or whose bound endpoint
// snapshot is stale. Missing-source transactions remain recoverable because a
// crash may have occurred before their current-state commit. The canonical
// edge-log fence makes admission, retirement, and orphan cleanup one
// cross-process authority.
func (m *memoryStore) retireSupersededMemoryReferenceTransactionsWithFence(fence *memoryEdgeLogFenceToken) (int, error) {
	if err := requireMemoryEdgeLogFenceOptional(m, fence); err != nil {
		return 0, err
	}
	transactions, err := m.loadMemoryReferenceTransactions()
	if err != nil {
		return 0, err
	}
	retired := make([]memoryReferenceTransaction, 0)
	for _, transaction := range transactions {
		current, ok := m.currentStateFor(transaction.Entry.Project, transaction.Entry.FileName)
		sourceSuperseded := ok && current.Entry.EventID != transaction.Entry.EventID &&
			memoryCurrentStateSupersedes(current, memoryCurrentStateFromEntry(transaction.Entry))
		if !sourceSuperseded && m.memoryReferenceTransactionEndpointsCurrent(transaction) {
			continue
		}
		retired = append(retired, transaction)
	}
	if len(retired) == 0 {
		if err := m.cleanupMemoryReferenceTransactionOrphansWithFence(transactions, fence); err != nil {
			return len(transactions), err
		}
		m.retainMemoryReferenceEdgeIndexTransactions(transactions)
		return len(transactions), nil
	}
	root := m.memoryReferenceTransactionRoot()
	// Pending records are the discoverability authority. Remove every selected
	// pending record and durably sync that deactivation before discarding any
	// acknowledgement. A crash can leave only inert receipt/index orphans.
	for _, transaction := range retired {
		if err := os.Remove(m.memoryReferenceTransactionPath(transaction.TransactionID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return len(transactions), err
		}
	}
	if err := syncOwnerOnlyDirectory(root); err != nil {
		return len(transactions), err
	}
	for _, transaction := range retired {
		for _, path := range []string{m.memoryReferenceReceiptPath(transaction.TransactionID), m.memoryReferenceHistoryIndexPath(transaction.TransactionID)} {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return len(transactions) - len(retired), err
			}
		}
	}
	if err := syncOwnerOnlyDirectory(root); err != nil {
		return len(transactions) - len(retired), err
	}
	remaining := make([]memoryReferenceTransaction, 0, len(transactions)-len(retired))
	retiredIDs := make(map[string]struct{}, len(retired))
	for _, transaction := range retired {
		retiredIDs[transaction.TransactionID] = struct{}{}
	}
	for _, transaction := range transactions {
		if _, wasRetired := retiredIDs[transaction.TransactionID]; !wasRetired {
			remaining = append(remaining, transaction)
		}
	}
	if err := m.cleanupMemoryReferenceTransactionOrphansWithFence(remaining, fence); err != nil {
		return len(remaining), err
	}
	m.retainMemoryReferenceEdgeIndexTransactions(remaining)
	return len(remaining), nil
}

func (m *memoryStore) admitMemoryReferenceTransactionWithFence(entry memoryStoreEntry, fence *memoryEdgeLogFenceToken) error {
	if err := requireMemoryEdgeLogFenceOptional(m, fence); err != nil {
		return err
	}
	remaining, err := m.retireSupersededMemoryReferenceTransactionsWithFence(fence)
	if err != nil {
		return err
	}
	if remaining < memoryReferenceTransactionMaxStartup {
		return nil
	}
	if remaining >= memoryReferenceTransactionMaxStored {
		return errors.New("reference transaction store has exhausted its bounded admission capacity")
	}
	if remaining > memoryReferenceTransactionMaxStartup {
		return errors.New("reference transaction store contains recovery slack that must be reconciled before admission")
	}
	// One bounded slack slot may replace the source's exact current closed
	// transaction. A crash in that interval is restart-safe because the loader
	// accepts the same explicit slack and reconciliation retires the old source
	// version after the new receipt/current-state commit.
	current, ok := m.currentEntry(entry.Project, entry.FileName)
	if !ok || !memoryCurrentStateSupersedes(memoryCurrentStateFromEntry(entry), memoryCurrentStateFromEntry(current)) {
		return errors.New("reference transaction store is at its bounded current-source capacity")
	}
	transactions, err := m.loadMemoryReferenceTransactions()
	if err != nil {
		return err
	}
	for _, transaction := range transactions {
		if !strings.EqualFold(transaction.Entry.Project, current.Project) || !strings.EqualFold(transaction.Entry.FileName, current.FileName) || transaction.Entry.EventID != current.EventID {
			continue
		}
		if _, closed, receiptErr := m.loadMemoryReferenceReceipt(transaction); receiptErr != nil {
			return receiptErr
		} else if closed {
			return nil
		}
	}
	return errors.New("reference transaction store is at its bounded current-source capacity")
}

func (m *memoryStore) prepareMemoryReferenceTransaction(entry memoryStoreEntry, content string, snapshot *memoryReferenceSnapshot, fence *memoryEdgeLogFenceToken) (memoryReferenceTransaction, error) {
	if err := requireMemoryEdgeLogFenceOptional(m, fence); err != nil {
		return memoryReferenceTransaction{}, err
	}
	requestDigest := memoryReferenceTransactionRequestDigest(entry, snapshot)
	transactionID := "ref_tx_" + sha256Hex(requestDigest + "\x00" + entry.Project + "::" + entry.FileName)[:32]
	entry.ReferenceTransactionID = transactionID
	path := m.memoryReferenceTransactionPath(transactionID)
	if raw, err := readBoundedRegularFileNoFollow(path, memoryReferenceTransactionMaxBytes); err == nil {
		var existing memoryReferenceTransaction
		if json.Unmarshal(raw, &existing) != nil || !memoryReferenceTransactionValid(existing) || existing.RequestDigest != requestDigest {
			return memoryReferenceTransaction{}, errors.New("immutable reference transaction conflicts with the requested write")
		}
		return existing, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return memoryReferenceTransaction{}, err
	}
	edges := make([]memoryEdgeEntry, 0, len(entry.References))
	for _, claim := range entry.References {
		target, targetGeneration, err := snapshot.entry(claim.TargetID)
		if err != nil {
			return memoryReferenceTransaction{}, fmt.Errorf("reference %s: %w", claim.TargetID, err)
		}
		sourceGeneration := snapshot.Generations[normalizeCurrentKeyIndexProject(entry.Project)]
		if sourceGeneration == 0 {
			sourceGeneration = 1
		}
		edge, err := m.buildMemoryReferenceEdge(entry, target, sourceGeneration, targetGeneration, snapshot.DocSetDigest, snapshot.ExclusionDigest, claim, "structured_write")
		if err != nil {
			return memoryReferenceTransaction{}, err
		}
		edge, err = m.bindPromotedMemoryEdge(snapshot, edge)
		if err != nil {
			return memoryReferenceTransaction{}, fmt.Errorf("reference %s: %w", claim.TargetID, err)
		}
		if edge.Metadata == nil {
			edge.Metadata = map[string]any{}
		}
		edge.Metadata["reference_transaction_id"] = transactionID
		edges = append(edges, edge)
	}
	sortMemoryReferenceEdges(edges)
	content = canonicalMemoryContent(content)
	if canonicalMemoryContentHash(content) != strings.TrimPrefix(strings.ToLower(entry.ContentHash), "sha256:") {
		return memoryReferenceTransaction{}, errors.New("reference transaction content hash mismatch")
	}
	if err := m.admitMemoryReferenceTransactionWithFence(entry, fence); err != nil {
		return memoryReferenceTransaction{}, err
	}
	if _, err := m.ensureReferenceTransactionBlob(entry.ContentHash, content); err != nil {
		return memoryReferenceTransaction{}, err
	}
	entryJSON, err := json.Marshal(entry)
	if err != nil {
		return memoryReferenceTransaction{}, err
	}
	transaction := memoryReferenceTransaction{
		SchemaID: memoryReferenceTransactionSchemaID, Version: 1, TransactionID: transactionID, RequestDigest: requestDigest,
		PreparedAt: nowUTCISO(), ContentRef: "sha256:" + entry.ContentHash, HistoryDigest: "sha256:" + sha256Hex(string(entryJSON)),
		EdgeSetDigest: memoryReferenceEdgeSetDigest(edges), SnapshotDigest: snapshot.DocSetDigest, Entry: entry, Edges: edges,
	}
	if err := ensureOwnerOnlyDirectory(m.memoryReferenceTransactionRoot(), true); err != nil {
		return memoryReferenceTransaction{}, err
	}
	raw, err := json.MarshalIndent(transaction, "", "  ")
	if err != nil {
		return memoryReferenceTransaction{}, err
	}
	if err := writeOwnerOnlyDurableAtomicFile(path, append(raw, '\n'), true); err != nil {
		return memoryReferenceTransaction{}, err
	}
	return transaction, nil
}

func (m *memoryStore) repairMemoryReferenceHistoryTailLocked(file *os.File) (int64, error) {
	if file == nil {
		return 0, errors.New("memory history descriptor is unavailable")
	}
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, errors.New("memory history is not a regular file")
	}
	size := info.Size()
	if size == 0 {
		return 0, nil
	}
	last := []byte{0}
	read, err := file.ReadAt(last, size-1)
	if m.memoryReferenceHistoryObserveIO != nil {
		m.memoryReferenceHistoryObserveIO("tail_probe", int64(read))
	}
	if err != nil {
		return 0, err
	}
	if last[0] == '\n' {
		return size, nil
	}
	const maxTail = int64(4 * 1024 * 1024)
	remaining := minInt64(size, maxTail)
	end := size
	buffer := make([]byte, 32*1024)
	truncateAt := int64(0)
	for remaining > 0 {
		chunk := minInt64(int64(len(buffer)), remaining)
		start := end - chunk
		read, readErr := file.ReadAt(buffer[:chunk], start)
		if m.memoryReferenceHistoryObserveIO != nil {
			m.memoryReferenceHistoryObserveIO("tail_repair", int64(read))
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return 0, readErr
		}
		for index := read - 1; index >= 0; index-- {
			if buffer[index] == '\n' {
				truncateAt = start + int64(index) + 1
				remaining = 0
				break
			}
		}
		if remaining == 0 {
			break
		}
		remaining -= chunk
		end = start
	}
	if truncateAt == 0 && size > maxTail {
		return 0, errors.New("memory history trailing row exceeds recovery bound")
	}
	if err := file.Truncate(truncateAt); err != nil {
		return 0, err
	}
	if err := file.Sync(); err != nil {
		return 0, err
	}
	return truncateAt, nil
}

func rollbackMemoryReferenceHistory(file *os.File, size int64, cause error) error {
	truncateErr := file.Truncate(size)
	syncErr := file.Sync()
	closeErr := file.Close()
	if truncateErr != nil || syncErr != nil || closeErr != nil {
		return fmt.Errorf("memory reference history append failed (%v) and rollback was incomplete: truncate=%v sync=%v close=%v", cause, truncateErr, syncErr, closeErr)
	}
	return cause
}

func (m *memoryStore) appendMemoryReferenceHistory(transaction memoryReferenceTransaction) error {
	unlock, err := lockOwnerOnlyFile(m.policy.historyPath + ".writer.lock")
	if err != nil {
		return err
	}
	defer unlock()
	file, err := openOwnerOnlyTruncate(m.policy.historyPath, true)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	if m.memoryReferenceHistoryDescriptorOpened != nil {
		m.memoryReferenceHistoryDescriptorOpened()
	}
	if err := ownerOnlyLockPathIdentityMatches(m.policy.historyPath, file); err != nil {
		return fmt.Errorf("memory history path changed after descriptor open: %w", err)
	}
	historySize, err := m.repairMemoryReferenceHistoryTailLocked(file)
	if err != nil {
		return err
	}
	if err := ownerOnlyLockPathIdentityMatches(m.policy.historyPath, file); err != nil {
		return fmt.Errorf("memory history path changed during tail repair: %w", err)
	}
	entryJSON, err := json.Marshal(transaction.Entry)
	if err != nil {
		return err
	}
	payload := append(entryJSON, '\n')
	index, indexed, err := m.loadMemoryReferenceHistoryIndex(transaction)
	if err != nil {
		return err
	}
	if !indexed {
		index, err = m.writeMemoryReferenceHistoryIndex(transaction, historySize, int64(len(payload)))
		if err != nil {
			return err
		}
	}
	if index.Length != int64(len(payload)) {
		return errors.New("reference transaction history index length conflicts with its immutable entry")
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() < index.Offset {
		return errors.New("memory history is shorter than its durable reference transaction index")
	}
	if info.Size() > index.Offset {
		readLength := minInt64(index.Length, info.Size()-index.Offset)
		observed := make([]byte, int(readLength))
		read, readErr := file.ReadAt(observed, index.Offset)
		if m.memoryReferenceHistoryObserveIO != nil {
			m.memoryReferenceHistoryObserveIO("exact_index_read", int64(read))
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if read == len(payload) && bytes.Equal(observed, payload) {
			if strings.TrimSpace(index.AcknowledgedAt) == "" {
				index, err = m.acknowledgeMemoryReferenceHistoryIndex(index)
				if err != nil {
					return err
				}
			}
			if err := ownerOnlyLockPathIdentityMatches(m.policy.historyPath, file); err != nil {
				return fmt.Errorf("memory history path changed during indexed verification: %w", err)
			}
			if err := file.Close(); err != nil {
				return err
			}
			closed = true
			return nil
		}
		if strings.TrimSpace(index.AcknowledgedAt) != "" {
			return errors.New("acknowledged reference transaction history row conflicts with durable history")
		}
		// A failed attempt may have durably reserved an offset before another
		// ordinary writer occupied it. Because the writer lock and tail repair
		// prove a complete current EOF, relocate the immutable transaction's
		// acknowledgement to that EOF instead of scanning historical rows.
		index, err = m.writeMemoryReferenceHistoryIndex(transaction, info.Size(), int64(len(payload)))
		if err != nil {
			return err
		}
	}
	if strings.TrimSpace(index.AcknowledgedAt) != "" {
		return errors.New("acknowledged reference transaction history row is missing")
	}
	if err := ownerOnlyLockPathIdentityMatches(m.policy.historyPath, file); err != nil {
		return fmt.Errorf("memory history path changed before indexed append: %w", err)
	}
	if m.beforeReferenceHistoryAppend != nil {
		if err := m.beforeReferenceHistoryAppend(); err != nil {
			return err
		}
	}
	if written, err := file.WriteAt(payload, index.Offset); err != nil || written != len(payload) {
		if err != nil {
			closed = true
			return rollbackMemoryReferenceHistory(file, index.Offset, err)
		}
		closed = true
		return rollbackMemoryReferenceHistory(file, index.Offset, io.ErrShortWrite)
	}
	if m.beforeReferenceHistorySync != nil {
		if err := m.beforeReferenceHistorySync(); err != nil {
			closed = true
			return rollbackMemoryReferenceHistory(file, index.Offset, err)
		}
	}
	if err := file.Sync(); err != nil {
		closed = true
		return rollbackMemoryReferenceHistory(file, index.Offset, err)
	}
	if err := ownerOnlyLockPathIdentityMatches(m.policy.historyPath, file); err != nil {
		return fmt.Errorf("memory history path changed during indexed append: %w", err)
	}
	if _, err := m.acknowledgeMemoryReferenceHistoryIndex(index); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	closed = true
	return syncOwnerOnlyDirectory(filepath.Dir(m.policy.historyPath))
}

func memoryReferenceReceiptDigest(receipt memoryReferenceTransactionReceipt) string {
	receipt.ReceiptDigest = ""
	raw, _ := json.Marshal(receipt)
	return "sha256:" + sha256Hex(string(raw))
}

func (m *memoryStore) loadMemoryReferenceReceipt(transaction memoryReferenceTransaction) (memoryReferenceTransactionReceipt, bool, error) {
	raw, err := readBoundedRegularFileNoFollow(m.memoryReferenceReceiptPath(transaction.TransactionID), memoryReferenceReceiptMaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		return memoryReferenceTransactionReceipt{}, false, nil
	}
	if err != nil {
		return memoryReferenceTransactionReceipt{}, false, err
	}
	var receipt memoryReferenceTransactionReceipt
	if json.Unmarshal(raw, &receipt) != nil || receipt.SchemaID != memoryReferenceReceiptSchemaID || receipt.Version != 1 || receipt.TransactionID != transaction.TransactionID ||
		receipt.RequestDigest != transaction.RequestDigest || receipt.HistoryDigest != transaction.HistoryDigest || receipt.EdgeSetDigest != transaction.EdgeSetDigest || receipt.ReceiptDigest != memoryReferenceReceiptDigest(receipt) {
		return receipt, false, errors.New("reference transaction receipt is invalid")
	}
	return receipt, true, nil
}

func (m *memoryStore) closeMemoryReferenceTransaction(transaction memoryReferenceTransaction, edgeLog memoryEdgeLogState) (memoryReferenceTransactionReceipt, error) {
	if receipt, ok, err := m.loadMemoryReferenceReceipt(transaction); err != nil || ok {
		return receipt, err
	}
	if m.beforeReferenceReceiptClose != nil {
		if err := m.beforeReferenceReceiptClose(); err != nil {
			return memoryReferenceTransactionReceipt{}, err
		}
	}
	receipt := memoryReferenceTransactionReceipt{
		SchemaID: memoryReferenceReceiptSchemaID, Version: 1, TransactionID: transaction.TransactionID, RequestDigest: transaction.RequestDigest,
		HistoryDigest: transaction.HistoryDigest, EdgeSetDigest: transaction.EdgeSetDigest, EdgeLogGeneration: edgeLog.Generation, EdgeLogDigest: edgeLog.Digest, ClosedAt: nowUTCISO(),
	}
	receipt.ReceiptDigest = memoryReferenceReceiptDigest(receipt)
	raw, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return receipt, err
	}
	if err := writeOwnerOnlyDurableAtomicFile(m.memoryReferenceReceiptPath(transaction.TransactionID), append(raw, '\n'), true); err != nil {
		return receipt, err
	}
	return receipt, nil
}

func (m *memoryStore) memoryReferenceTransactionEndpointsCurrent(transaction memoryReferenceTransaction) bool {
	currentExclusionDigest := m.referenceExclusionPolicyDigest()
	for _, edge := range transaction.Edges {
		if edge.Binding == nil || edge.Binding.SourceEventID != transaction.Entry.EventID || !strings.EqualFold(edge.Binding.SourceContentHash, transaction.Entry.ContentHash) ||
			edge.Binding.RelationSemantic != edge.Relation || edge.Binding.ExclusionPolicyDigest != currentExclusionDigest || edge.Binding.SemanticDigest == "" {
			return false
		}
		targetProject, targetFile, _, _, err := canonicalMemoryID(edge.TargetID)
		if err != nil {
			return false
		}
		target, ok := m.currentEntry(targetProject, targetFile)
		if !ok || target.EventID != edge.Binding.TargetEventID || !strings.EqualFold(strings.TrimPrefix(target.ContentHash, "sha256:"), strings.TrimPrefix(edge.Binding.TargetContentHash, "sha256:")) || normalizeMemoryLifecycle(target.Lifecycle) != edge.Binding.TargetLifecycle ||
			edge.Binding.SemanticDigest != memoryGraphBindingSemanticDigest(edge.Relation, transaction.Entry, target) {
			return false
		}
		if excluded, _ := m.memoryGraphArtifactExcluded(transaction.Entry.Project, transaction.Entry.FileName, transaction.Entry.TopicPath); excluded {
			return false
		}
		if excluded, _ := m.memoryGraphArtifactExcluded(target.Project, target.FileName, target.TopicPath); excluded {
			return false
		}
	}
	return true
}

func (m *memoryStore) persistAndRecordMemoryReferenceTransaction(transaction memoryReferenceTransaction) error {
	if m.beforeReferenceCurrentCommit != nil {
		if err := m.beforeReferenceCurrentCommit(); err != nil {
			return err
		}
	}
	entry := transaction.Entry
	key := memoryStoreKey(entry.Project, entry.FileName)
	m.currentStateGenerationAdmissionMu.Lock()
	defer m.currentStateGenerationAdmissionMu.Unlock()
	m.mu.RLock()
	if current, exists := m.currentState[key]; exists && current.Entry.EventID != entry.EventID && memoryCurrentStateSupersedes(current, memoryCurrentStateFromEntry(entry)) {
		m.mu.RUnlock()
		return nil
	}
	m.mu.RUnlock()
	if err := m.persistAndRecordEntryLocked(entry); err != nil {
		return err
	}
	m.mu.Lock()
	current, currentExists := m.currentState[key]
	if currentExists && current.Entry.EventID == entry.EventID {
		for _, edge := range transaction.Edges {
			m.recordEdgeLocked(edge)
		}
	}
	m.mu.Unlock()
	return nil
}

func (m *memoryStore) reconcileMemoryReferenceTransaction(ctx context.Context, transaction memoryReferenceTransaction) error {
	if ctx == nil {
		ctx = context.Background()
	}
	fence, err := m.acquireMemoryEdgeLogFenceOptionalContext(ctx)
	if err != nil {
		return err
	}
	if fence != nil {
		defer fence.release()
	}
	if err := m.reconcileMemoryReferenceTransactionWithFence(ctx, transaction, fence); err != nil {
		return err
	}
	_, err = m.retireSupersededMemoryReferenceTransactionsWithFence(fence)
	return err
}

func (m *memoryStore) reconcileMemoryReferenceTransactionWithFence(ctx context.Context, transaction memoryReferenceTransaction, fence *memoryEdgeLogFenceToken) error {
	if err := requireMemoryEdgeLogFenceOptional(m, fence); err != nil {
		return err
	}
	if !memoryReferenceTransactionValid(transaction) {
		return errors.New("reference transaction is invalid")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if current, ok := m.currentEntry(transaction.Entry.Project, transaction.Entry.FileName); ok {
		if current.EventID != transaction.Entry.EventID {
			if memoryCurrentStateSupersedes(memoryCurrentStateFromEntry(current), memoryCurrentStateFromEntry(transaction.Entry)) {
				return nil
			}
		} else {
			if _, closed, err := m.loadMemoryReferenceReceipt(transaction); err != nil {
				return err
			} else if closed {
				if !m.memoryReferenceTransactionEndpointsCurrent(transaction) {
					return nil
				}
				_, err := m.appendMemoryReferenceEdgeSetWithFence(ctx, transaction.TransactionID, transaction.Edges, fence)
				return err
			}
		}
	}
	content, _, err := m.readBoundedContentBlob(ctx, transaction.ContentRef, int64(transaction.Entry.RawBytes))
	if err != nil {
		return err
	}
	blobPath, err := m.blobPathForHash(transaction.Entry.ContentHash)
	if err != nil {
		return err
	}
	filePath := filepath.Join(m.policy.rootPath, transaction.Entry.Project, filepath.FromSlash(transaction.Entry.FileName))
	if err := m.linkOrCopyBlob(blobPath, filePath); err != nil {
		return err
	}
	if canonicalMemoryContentHash(content) != transaction.Entry.ContentHash {
		return errors.New("reference transaction source blob changed during reconciliation")
	}
	if err := m.appendMemoryReferenceHistory(transaction); err != nil {
		return err
	}
	edgeLog, err := m.appendMemoryReferenceEdgeSetWithFence(ctx, transaction.TransactionID, transaction.Edges, fence)
	if err != nil {
		return err
	}
	if !m.memoryReferenceTransactionEndpointsCurrent(transaction) {
		return errors.New("reference transaction target snapshot is stale")
	}
	if _, err := m.closeMemoryReferenceTransaction(transaction, edgeLog); err != nil {
		return err
	}
	return m.persistAndRecordMemoryReferenceTransaction(transaction)
}

func (m *memoryStore) loadMemoryReferenceTransactions() ([]memoryReferenceTransaction, error) {
	root := m.memoryReferenceTransactionRoot()
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return []memoryReferenceTransaction{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(entries) > memoryReferenceTransactionMaxStored*4 {
		return nil, fmt.Errorf("reference transaction directory exceeds bounded startup inventory: %d", len(entries))
	}
	transactions := make([]memoryReferenceTransaction, 0, minInt(len(entries), memoryReferenceTransactionMaxStored))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pending.json") {
			continue
		}
		if len(transactions) >= memoryReferenceTransactionMaxStored {
			return nil, errors.New("reference transaction pending set exceeds bounded startup cap")
		}
		raw, err := readBoundedRegularFileNoFollow(filepath.Join(root, entry.Name()), memoryReferenceTransactionMaxBytes)
		if err != nil {
			return nil, err
		}
		var transaction memoryReferenceTransaction
		if json.Unmarshal(raw, &transaction) != nil || !memoryReferenceTransactionValid(transaction) || entry.Name() != transaction.TransactionID+".pending.json" {
			return nil, fmt.Errorf("reference transaction %s is invalid", entry.Name())
		}
		transactions = append(transactions, transaction)
	}
	sort.Slice(transactions, func(i, j int) bool {
		if transactions[i].PreparedAt != transactions[j].PreparedAt {
			return transactions[i].PreparedAt < transactions[j].PreparedAt
		}
		return transactions[i].TransactionID < transactions[j].TransactionID
	})
	return transactions, nil
}

func (m *memoryStore) reconcileMemoryReferenceTransactions(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	fence, err := m.acquireMemoryEdgeLogFenceOptionalContext(ctx)
	if err != nil {
		return err
	}
	if fence != nil {
		defer fence.release()
	}
	return m.reconcileMemoryReferenceTransactionsWithFence(ctx, fence)
}

func (m *memoryStore) reconcileMemoryReferenceTransactionsWithFence(ctx context.Context, fence *memoryEdgeLogFenceToken) error {
	if err := requireMemoryEdgeLogFenceOptional(m, fence); err != nil {
		return err
	}
	transactions, err := m.loadMemoryReferenceTransactions()
	if err != nil {
		return err
	}
	for _, transaction := range transactions {
		if err := m.reconcileMemoryReferenceTransactionWithFence(ctx, transaction, fence); err != nil {
			if strings.Contains(err.Error(), "target snapshot is stale") {
				continue
			}
			return fmt.Errorf("reconcile reference transaction %s: %w", transaction.TransactionID, err)
		}
	}
	_, err = m.retireSupersededMemoryReferenceTransactionsWithFence(fence)
	return err
}

func (m *memoryStore) closedMemoryReferenceTransactions() (map[string]map[string]string, error) {
	transactions, err := m.loadMemoryReferenceTransactions()
	if err != nil {
		return nil, err
	}
	closed := make(map[string]map[string]string, len(transactions))
	for _, transaction := range transactions {
		if _, ok, err := m.loadMemoryReferenceReceipt(transaction); err != nil {
			return nil, err
		} else if ok {
			edges := make(map[string]string, len(transaction.Edges))
			for _, edge := range transaction.Edges {
				edges[edge.EdgeID] = memoryReferenceEdgeDigest(edge)
			}
			closed[transaction.TransactionID] = edges
		}
	}
	return closed, nil
}

// currentClosedMemoryReferenceEdges reconstructs complete current transaction
// sets from their immutable pending records and closed receipts. The receipt is
// written only after the complete edge set is durable, so bounded startup-tail
// projection cannot make older siblings in the same set disappear.
func (m *memoryStore) currentClosedMemoryReferenceEdges() ([]memoryEdgeEntry, error) {
	transactions, err := m.loadMemoryReferenceTransactions()
	if err != nil {
		return nil, err
	}
	edges := make([]memoryEdgeEntry, 0)
	for _, transaction := range transactions {
		if _, closed, err := m.loadMemoryReferenceReceipt(transaction); err != nil {
			return nil, err
		} else if !closed {
			continue
		}
		current, ok := m.currentEntry(transaction.Entry.Project, transaction.Entry.FileName)
		if !ok || current.EventID != transaction.Entry.EventID || !m.memoryReferenceTransactionEndpointsCurrent(transaction) {
			continue
		}
		edges = append(edges, transaction.Edges...)
		if len(edges) > memoryReferenceTransactionEdgeIndexMax {
			return nil, errors.New("current closed reference edge set exceeds bounded startup cap")
		}
	}
	return edges, nil
}

func memoryReferenceClaimPresent(entry memoryStoreEntry, edge memoryEdgeEntry) bool {
	for _, claim := range entry.References {
		if strings.EqualFold(claim.TargetID, edge.TargetID) && claim.Relation == edge.Relation {
			return true
		}
	}
	return false
}

func (m *memoryStore) referenceEdgeCurrent(edge memoryEdgeEntry) bool {
	if m == nil {
		return false
	}
	fence, err := m.acquireMemoryEdgeLogFenceOptional()
	if err != nil {
		return false
	}
	if fence != nil {
		defer fence.release()
	}
	return m.referenceEdgeCurrentWithFence(edge, fence)
}

func (m *memoryStore) referenceEdgeCurrentWithFence(edge memoryEdgeEntry, fence *memoryEdgeLogFenceToken) bool {
	if m == nil {
		return false
	}
	if err := requireMemoryEdgeLogFenceOptional(m, fence); err != nil {
		return false
	}
	if !memoryReferenceBindingValid(edge.Binding) {
		return false
	}
	sourceProject, sourceFile, _, _, sourceErr := canonicalMemoryID(edge.SourceID)
	targetProject, targetFile, _, _, targetErr := canonicalMemoryID(edge.TargetID)
	if sourceErr != nil || targetErr != nil {
		return false
	}
	source, sourceOK := m.currentEntry(sourceProject, sourceFile)
	target, targetOK := m.currentEntry(targetProject, targetFile)
	if !sourceOK || !targetOK {
		return false
	}
	sourceExact, sourceExactErr := m.isExactStatePathWithFenceChecked(sourceProject, sourceFile, fence)
	targetExact, targetExactErr := m.isExactStatePathWithFenceChecked(targetProject, targetFile, fence)
	if sourceExactErr != nil || targetExactErr != nil || sourceExact || targetExact {
		return false
	}
	if source.EventID != edge.Binding.SourceEventID || target.EventID != edge.Binding.TargetEventID {
		return false
	}
	if !strings.EqualFold(strings.TrimPrefix(source.ContentHash, "sha256:"), strings.TrimPrefix(edge.Binding.SourceContentHash, "sha256:")) ||
		!strings.EqualFold(strings.TrimPrefix(target.ContentHash, "sha256:"), strings.TrimPrefix(edge.Binding.TargetContentHash, "sha256:")) {
		return false
	}
	sourceTopic := sanitizeTopicPath(source.TopicPath, source.FileName)
	targetTopic := sanitizeTopicPath(target.TopicPath, target.FileName)
	if edge.Binding.RelationSemantic != edge.Relation ||
		!strings.EqualFold(sourceTopic, edge.Binding.SourceTopicPath) || !strings.EqualFold(targetTopic, edge.Binding.TargetTopicPath) ||
		normalizeMemoryLifecycle(source.Lifecycle) != edge.Binding.SourceLifecycle || normalizeMemoryLifecycle(target.Lifecycle) != edge.Binding.TargetLifecycle ||
		edge.Binding.SemanticDigest != memoryGraphBindingSemanticDigest(edge.Relation, source, target) {
		return false
	}
	if excluded, _ := m.memoryGraphArtifactExcluded(source.Project, source.FileName, sourceTopic); excluded {
		return false
	}
	if excluded, _ := m.memoryGraphArtifactExcluded(target.Project, target.FileName, targetTopic); excluded {
		return false
	}
	switch edge.Relation {
	case "same_session":
		if strings.TrimSpace(source.SessionID) == "" || source.SessionID != target.SessionID || source.SessionID != edge.Binding.SourceSessionID || target.SessionID != edge.Binding.TargetSessionID {
			return false
		}
	case "same_topic":
		if !strings.EqualFold(strings.Trim(sourceTopic, "/"), strings.Trim(targetTopic, "/")) {
			return false
		}
	case "same_agent":
		if strings.TrimSpace(source.AgentID) == "" || !strings.EqualFold(source.AgentID, target.AgentID) || !strings.EqualFold(source.AgentID, edge.Binding.SourceAgentID) || !strings.EqualFold(target.AgentID, edge.Binding.TargetAgentID) || !strings.EqualFold(strings.Trim(sourceTopic, "/"), strings.Trim(targetTopic, "/")) {
			return false
		}
	case "inferred_related":
		sourceDoc := memoryEdgeBackfillDoc{Project: source.Project, FileName: source.FileName, MemoryID: edge.SourceID, TopicPath: sourceTopic, Summary: source.Summary}
		targetDoc := memoryEdgeBackfillDoc{Project: target.Project, FileName: target.FileName, MemoryID: edge.TargetID, TopicPath: targetTopic, Summary: target.Summary}
		sourceInferred := memoryEdgeInferredDoc{doc: sourceDoc, tokens: memoryEdgeInferenceTokens(sourceDoc)}
		targetInferred := memoryEdgeInferredDoc{doc: targetDoc, tokens: memoryEdgeInferenceTokens(targetDoc)}
		shared := 0
		for token := range sourceInferred.tokens {
			if _, ok := targetInferred.tokens[token]; ok {
				shared++
			}
		}
		minimumShared := anyToInt(edge.Metadata["min_shared_terms"], anyToInt(edge.Provenance["min_shared_terms"], 1))
		minimumScore := anyToFloat64(edge.Metadata["min_score"], anyToFloat64(edge.Provenance["min_score"], edge.Confidence))
		score, _ := inferredMemoryEdgeScore(sourceInferred, targetInferred, shared)
		if shared < minimumShared || score < minimumScore {
			return false
		}
	}
	claimKind := anyToString(edge.Metadata["claim_kind"])
	if claimKind == "structured_write" && !memoryReferenceClaimPresent(source, edge) {
		return false
	}
	if claimKind == "textual_summary" || claimKind == "textual_content_blob" {
		known := map[string]memoryEdgeBackfillDoc{}
		known[strings.ToLower(source.Project+"::"+source.FileName)] = memoryEdgeBackfillDoc{Project: source.Project, FileName: source.FileName, MemoryID: source.Project + "::" + source.FileName}
		known[strings.ToLower(target.Project+"::"+target.FileName)] = memoryEdgeBackfillDoc{Project: target.Project, FileName: target.FileName, MemoryID: target.Project + "::" + target.FileName}
		text := source.Summary
		if claimKind == "textual_content_blob" {
			content, _, err := m.readReferenceContentBlob(context.Background(), source.ContentRef, memoryReferenceBackfillMaxBlobBytes)
			if err != nil {
				return false
			}
			text = content
		}
		found := referencedMemoryIDs(source.Project, text, known)
		matched := false
		for _, id := range found {
			if strings.EqualFold(id, edge.TargetID) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

var memoryReferenceQuarantineMu sync.Mutex

func (m *memoryStore) quarantineMemoryReferenceEdge(edge memoryEdgeEntry, reason string) {
	if m == nil || strings.TrimSpace(m.policy.edgePath) == "" {
		return
	}
	path := m.policy.edgePath + ".quarantine.ndjson"
	row := map[string]any{
		"schema_version": memoryStructuredReferenceSchemaVersion,
		"quarantined_at": nowUTCISO(),
		"reason":         reason,
		"edge":           edge,
	}
	payload, err := json.Marshal(row)
	if err != nil {
		return
	}
	memoryReferenceQuarantineMu.Lock()
	defer memoryReferenceQuarantineMu.Unlock()
	if err := ensureOwnerOnlyDirectory(filepath.Dir(path), true); err != nil {
		return
	}
	file, err := openOwnerOnlyAppend(path, true)
	if err != nil {
		return
	}
	_, _ = file.Write(append(payload, '\n'))
	_ = file.Close()
}

func (m *memoryStore) readReferenceContentBlob(ctx context.Context, contentRef string, maxBytes int64) (string, int, error) {
	maxBytes = minInt64(maxBytes, memoryReferenceBackfillMaxBlobBytes)
	if maxBytes < 1 {
		return "", 0, errors.New("content blob bound must be positive")
	}
	return m.readBoundedContentBlob(ctx, contentRef, maxBytes)
}

func (m *memoryStore) readBoundedContentBlob(ctx context.Context, contentRef string, maxBytes int64) (string, int, error) {
	if m == nil || !m.isConfigured() {
		return "", 0, errors.New("go memory store is disabled")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	hash := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(contentRef)), "sha256:")
	if !isHexDigest(hash) {
		return "", 0, errors.New("content_ref must contain a sha256 digest")
	}
	if maxBytes < 1 {
		return "", 0, errors.New("content blob bound must be positive")
	}
	path, err := m.blobPathForHash(hash)
	if err != nil {
		return "", 0, err
	}
	file, descriptorSize, err := openBoundedRegularFileNoFollow(path, maxBytes)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	limited := io.LimitReader(file, maxBytes+1)
	var builder strings.Builder
	buffer := make([]byte, 32*1024)
	for {
		select {
		case <-ctx.Done():
			return "", 0, ctx.Err()
		default:
		}
		read, readErr := limited.Read(buffer)
		if read > 0 {
			builder.Write(buffer[:read])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", 0, readErr
		}
	}
	content := builder.String()
	if int64(len(content)) != descriptorSize || int64(len(content)) > maxBytes || !strings.EqualFold(canonicalMemoryContentHash(content), hash) {
		return "", 0, errors.New("content blob hash or bound mismatch")
	}
	return content, len(content), nil
}

func (m *memoryStore) appendMemoryReferenceBackfillLedger(report map[string]any) (map[string]any, error) {
	if m == nil || !m.isEnabled() {
		return nil, errors.New("go memory store is disabled")
	}
	population := anyMap(report["reference_population"])
	requestDigest := sha256Hex(fmt.Sprintf("%v\x00%v\x00%v\x00%v", report["project"], report["corpus"], report["dry_run"], report["reference_population"]))
	closedAt := nowUTCISO()
	runID := "reference-backfill-" + sha256Hex(closedAt + "\x00" + requestDigest)[:24]
	row := map[string]any{
		"schema_version": memoryStructuredReferenceSchemaVersion,
		"run_id":         runID,
		"closed":         true,
		"closed_at":      closedAt,
		"project":        report["project"],
		"corpus":         report["corpus"],
		"dry_run":        report["dry_run"],
		"request_digest": "sha256:" + requestDigest,
		"scanned_docs":   report["scanned_docs"],
		"generated":      report["generated"],
		"eligible":       report["eligible"],
		"written":        report["written"],
		"existing":       report["existing"],
		"errors":         len(anyToStringSlice(report["errors"])),
		"reference_population": map[string]any{
			"structured_claims":      population["structured_claims"],
			"textual_summary_claims": population["textual_summary_claims"],
			"content_blob_claims":    population["content_blob_claims"],
			"rejected_claims":        population["rejected_claims"],
			"content_bytes":          population["content_bytes"],
			"content_blobs":          population["content_blobs"],
			"continuation_start":     population["continuation_start"],
			"continuation_next":      population["continuation_next"],
			"continuation_complete":  population["continuation_complete"],
		},
	}
	payload, err := json.Marshal(row)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(m.policy.rootPath, "_contextlattice", "memory_reference_backfill_ledger.ndjson")
	if err := ensureOwnerOnlyDirectory(filepath.Dir(path), true); err != nil {
		return nil, err
	}
	file, err := openOwnerOnlyAppend(path, true)
	if err != nil {
		return nil, err
	}
	if _, err := file.Write(append(payload, '\n')); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	return map[string]any{
		"schema_version": memoryStructuredReferenceSchemaVersion,
		"run_id":         runID,
		"closed":         true,
		"ledger_ref":     ownerOnlyStoreRef("memory_reference_backfill_ledger"),
		"request_digest": "sha256:" + requestDigest,
		"closed_at":      closedAt,
	}, nil
}
