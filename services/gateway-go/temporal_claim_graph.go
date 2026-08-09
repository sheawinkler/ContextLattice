package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	temporalClaimContractID      = "temporal_claim.v1"
	temporalClaimQueryContractID = "temporal_claim_query.v1"
)

var temporalClaimIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

type temporalClaimEvidence struct {
	RefID       string `json:"ref_id"`
	Kind        string `json:"kind"`
	MemoryID    string `json:"memory_id,omitempty"`
	URI         string `json:"uri,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
	Excerpt     string `json:"excerpt,omitempty"`
	ObservedAt  string `json:"observed_at,omitempty"`
}

type temporalClaim struct {
	SchemaID     string                  `json:"schema_id"`
	ClaimID      string                  `json:"claim_id"`
	Project      string                  `json:"project"`
	Subject      string                  `json:"subject"`
	Predicate    string                  `json:"predicate"`
	Object       string                  `json:"object"`
	Statement    string                  `json:"statement"`
	TopicPath    string                  `json:"topic_path,omitempty"`
	Status       string                  `json:"status"`
	ValidFrom    string                  `json:"valid_from,omitempty"`
	ValidTo      string                  `json:"valid_to,omitempty"`
	ObservedAt   string                  `json:"observed_at"`
	Confidence   float64                 `json:"confidence"`
	Supersedes   []string                `json:"supersedes"`
	Contradicts  []string                `json:"contradicts"`
	CausedBy     []string                `json:"caused_by"`
	Support      []temporalClaimEvidence `json:"support"`
	Opposition   []temporalClaimEvidence `json:"opposition"`
	Verification map[string]any          `json:"verification"`
	Provenance   map[string]any          `json:"provenance"`
	Branch       string                  `json:"branch,omitempty"`
	Commit       string                  `json:"commit,omitempty"`
	AgentID      string                  `json:"agent_id,omitempty"`
	SessionID    string                  `json:"session_id,omitempty"`
	Revision     int                     `json:"revision"`
	CreatedAt    string                  `json:"created_at"`
	UpdatedAt    string                  `json:"updated_at"`
	searchText   string                  `json:"-"`
}

type temporalClaimStore struct {
	mu                sync.RWMutex
	enabled           bool
	path              string
	maxClaims         int
	compactEvery      int
	fsync             bool
	claims            map[string]temporalClaim
	proofSessionIndex map[string][]string
	proofRevision     uint64
	logEntries        int
	parseErrors       int
	lastPersistedAt   string
	lastError         string
}

type temporalClaimQuery struct {
	Project           string
	Query             string
	Subject           string
	Predicate         string
	Status            string
	AsOf              time.Time
	Limit             int
	IncludeExpired    bool
	IncludeSuperseded bool
	IncludeRetracted  bool
}

func newTemporalClaimStoreFromEnv() (*temporalClaimStore, error) {
	store := &temporalClaimStore{
		enabled:           envBool("CONTEXTLATTICE_TEMPORAL_CLAIMS_ENABLED", true),
		path:              resolveStoragePath("CONTEXTLATTICE_TEMPORAL_CLAIMS_PATH", filepath.Join(".data", "orchestrator", "temporal_claims.ndjson")),
		maxClaims:         clampInt(envInt("CONTEXTLATTICE_TEMPORAL_CLAIMS_MAX", 10000), 128, 100000),
		compactEvery:      clampInt(envInt("CONTEXTLATTICE_TEMPORAL_CLAIMS_COMPACT_EVERY", 512), 32, 10000),
		fsync:             envBool("CONTEXTLATTICE_TEMPORAL_CLAIMS_FSYNC", true),
		claims:            map[string]temporalClaim{},
		proofSessionIndex: map[string][]string{},
	}
	if !store.enabled || strings.TrimSpace(store.path) == "" {
		store.enabled = false
		return store, nil
	}
	if err := prepareOwnerOnlyFile(store.path, strings.TrimSpace(os.Getenv("CONTEXTLATTICE_TEMPORAL_CLAIMS_PATH")) == ""); err != nil {
		return store, err
	}
	if err := store.load(); err != nil {
		return store, err
	}
	return store, nil
}

func (s *temporalClaimStore) load() error {
	file, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open temporal claim ledger: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var claim temporalClaim
		if err := json.Unmarshal([]byte(line), &claim); err != nil || claim.ClaimID == "" {
			s.parseErrors++
			continue
		}
		claim.searchText = temporalClaimSearchText(claim)
		s.setClaimLocked(claim)
		s.logEntries++
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan temporal claim ledger: %w", err)
	}
	s.trimLocked()
	return nil
}

func normalizeTemporalClaim(payload map[string]any, previous *temporalClaim) (temporalClaim, error) {
	project, err := sanitizeMemoryProject(anyToString(payload["project"]))
	if err != nil {
		return temporalClaim{}, fmt.Errorf("project: %w", err)
	}
	subject := clipText(strings.TrimSpace(anyToString(payload["subject"])), 300)
	predicate := strings.TrimSpace(strings.ToLower(anyToString(payload["predicate"])))
	predicate = strings.ReplaceAll(predicate, " ", "_")
	object := clipText(strings.TrimSpace(anyToString(payload["object"])), 600)
	statement := clipText(strings.TrimSpace(anyToString(payload["statement"])), 1200)
	if subject == "" || predicate == "" || object == "" {
		return temporalClaim{}, errors.New("subject, predicate, and object are required")
	}
	if statement == "" {
		statement = strings.TrimSpace(subject + " " + strings.ReplaceAll(predicate, "_", " ") + " " + object)
	}
	for _, ch := range predicate {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == ':' || ch == '.' {
			continue
		}
		return temporalClaim{}, errors.New("predicate may contain only letters, digits, underscore, colon, or dot")
	}
	claimID := strings.TrimSpace(anyToString(payload["claim_id"]))
	if claimID == "" {
		claimID = "claim_" + sha256Hex(project + "\x00" + subject + "\x00" + predicate + "\x00" + object)[:32]
	}
	if !temporalClaimIDPattern.MatchString(claimID) {
		return temporalClaim{}, errors.New("claim_id must contain only letters, digits, dot, colon, underscore, or hyphen")
	}
	validFrom, err := normalizeOptionalClaimTime(anyToString(payload["valid_from"]), "valid_from")
	if err != nil {
		return temporalClaim{}, err
	}
	validTo, err := normalizeOptionalClaimTime(anyToString(payload["valid_to"]), "valid_to")
	if err != nil {
		return temporalClaim{}, err
	}
	if validFrom != "" && validTo != "" {
		from, _ := time.Parse(time.RFC3339Nano, validFrom)
		to, _ := time.Parse(time.RFC3339Nano, validTo)
		if !to.After(from) {
			return temporalClaim{}, errors.New("valid_to must be after valid_from")
		}
	}
	observedAt, err := normalizeOptionalClaimTime(anyToString(payload["observed_at"]), "observed_at")
	if err != nil {
		return temporalClaim{}, err
	}
	if observedAt == "" {
		observedAt = nowUTCISO()
	}
	status, err := normalizeTemporalClaimStatus(anyToString(payload["status"]))
	if err != nil {
		return temporalClaim{}, err
	}
	confidence := anyToFloat64(payload["confidence"], 0.7)
	if confidence < 0 || confidence > 1 {
		return temporalClaim{}, errors.New("confidence must be between 0 and 1")
	}
	now := nowUTCISO()
	createdAt := now
	revision := 1
	if previous != nil {
		createdAt = previous.CreatedAt
		revision = previous.Revision + 1
	}
	claim := temporalClaim{
		SchemaID:     temporalClaimContractID,
		ClaimID:      claimID,
		Project:      project,
		Subject:      subject,
		Predicate:    predicate,
		Object:       object,
		Statement:    statement,
		TopicPath:    sanitizeTopicPath(anyToString(payload["topic_path"]), "claims"),
		Status:       status,
		ValidFrom:    validFrom,
		ValidTo:      validTo,
		ObservedAt:   observedAt,
		Confidence:   confidence,
		Supersedes:   normalizeClaimIDs(payload["supersedes"], claimID, 32),
		Contradicts:  normalizeClaimIDs(payload["contradicts"], claimID, 32),
		CausedBy:     normalizeClaimIDs(payload["caused_by"], claimID, 32),
		Support:      normalizeClaimEvidence(payload["support"], 24),
		Opposition:   normalizeClaimEvidence(payload["opposition"], 24),
		Verification: normalizeClaimVerification(payload["verification"]),
		Provenance:   normalizeClaimProvenance(payload["provenance"]),
		Branch:       clipText(strings.TrimSpace(anyToString(payload["branch"])), 200),
		Commit:       clipText(strings.TrimSpace(anyToString(payload["commit"])), 128),
		AgentID:      clipText(strings.TrimSpace(anyToString(payload["agent_id"])), 128),
		SessionID:    clipText(strings.TrimSpace(anyToString(payload["session_id"])), maxAgentSessionIDLength),
		Revision:     revision,
		CreatedAt:    createdAt,
		UpdatedAt:    now,
	}
	claim.searchText = temporalClaimSearchText(claim)
	return claim, nil
}

func (s *temporalClaimStore) setClaimLocked(claim temporalClaim) {
	if s.claims == nil {
		s.claims = map[string]temporalClaim{}
	}
	if s.proofSessionIndex == nil {
		s.proofSessionIndex = map[string][]string{}
	}
	if previous, exists := s.claims[claim.ClaimID]; exists {
		s.removeProofClaimRefLocked(previous)
	}
	s.claims[claim.ClaimID] = claim
	s.proofRevision = nextProofTimelineRevision(s.proofRevision)
	if key := temporalClaimProofIndexKey(claim.Project, claim.SessionID); key != "" {
		refs := append(s.proofSessionIndex[key], claim.ClaimID)
		limit := maxAgentProofTimelineSourceScans * 2
		if len(refs) > limit {
			refs = append([]string(nil), refs[len(refs)-limit:]...)
		}
		s.proofSessionIndex[key] = refs
	}
}

func temporalClaimProofIndexKey(project, sessionID string) string {
	project = strings.ToLower(strings.TrimSpace(project))
	sessionID = strings.TrimSpace(sessionID)
	if project == "" || sessionID == "" {
		return ""
	}
	return project + "\x00" + sessionID
}

func (s *temporalClaimStore) removeProofClaimRefLocked(claim temporalClaim) {
	if s == nil || s.proofSessionIndex == nil {
		return
	}
	key := temporalClaimProofIndexKey(claim.Project, claim.SessionID)
	if key == "" {
		return
	}
	refs := s.proofSessionIndex[key]
	kept := make([]string, 0, len(refs))
	for _, claimID := range refs {
		if claimID != claim.ClaimID {
			kept = append(kept, claimID)
		}
	}
	if len(kept) == 0 {
		delete(s.proofSessionIndex, key)
		return
	}
	s.proofSessionIndex[key] = kept
}

func normalizeOptionalClaimTime(raw string, field string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return "", fmt.Errorf("%s must be RFC3339: %w", field, err)
	}
	return parsed.UTC().Format(time.RFC3339Nano), nil
}

func normalizeTemporalClaimStatus(raw string) (string, error) {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "", "active", "current":
		return "active", nil
	case "superseded", "retracted", "expired":
		return strings.TrimSpace(strings.ToLower(raw)), nil
	default:
		return "", errors.New("status must be active, superseded, retracted, or expired")
	}
}

func normalizeClaimIDs(raw any, self string, limit int) []string {
	out := []string{}
	seen := map[string]struct{}{}
	for _, value := range anyToStringList(raw, limit*2) {
		value = strings.TrimSpace(value)
		if value == "" || value == self || !temporalClaimIDPattern.MatchString(value) {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
		if len(out) >= limit {
			break
		}
	}
	sort.Strings(out)
	return out
}

func normalizeClaimEvidence(raw any, limit int) []temporalClaimEvidence {
	out := []temporalClaimEvidence{}
	for _, value := range contextPackAnyList(raw) {
		item := anyMap(value)
		ref := temporalClaimEvidence{
			RefID:       clipText(strings.TrimSpace(firstNonEmptyStrings(anyToString(item["ref_id"]), anyToString(item["citation"]))), 240),
			Kind:        clipText(strings.TrimSpace(firstNonEmptyStrings(anyToString(item["kind"]), "memory")), 64),
			MemoryID:    clipText(strings.TrimSpace(anyToString(item["memory_id"])), 300),
			URI:         clipText(strings.TrimSpace(anyToString(item["uri"])), 800),
			ContentHash: clipText(strings.TrimSpace(anyToString(item["content_hash"])), 128),
			Excerpt:     clipText(strings.TrimSpace(anyToString(item["excerpt"])), 500),
			ObservedAt:  clipText(strings.TrimSpace(anyToString(item["observed_at"])), 64),
		}
		if ref.RefID == "" {
			ref.RefID = firstNonEmptyStrings(ref.MemoryID, ref.URI, ref.ContentHash)
		}
		if ref.RefID == "" {
			continue
		}
		out = append(out, ref)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func normalizeClaimVerification(raw any) map[string]any {
	value := anyMap(raw)
	status := strings.TrimSpace(strings.ToLower(anyToString(value["status"])))
	switch status {
	case "verified", "failed", "disputed":
	default:
		status = "unverified"
	}
	return map[string]any{
		"status":     status,
		"method":     clipText(strings.TrimSpace(anyToString(value["method"])), 240),
		"checked_at": clipText(strings.TrimSpace(anyToString(value["checked_at"])), 64),
		"checker":    clipText(strings.TrimSpace(anyToString(value["checker"])), 128),
	}
}

func normalizeClaimProvenance(raw any) map[string]any {
	value := anyMap(raw)
	out := map[string]any{}
	for _, key := range []string{"source", "source_id", "file", "uri", "content_hash", "created_by", "tool", "run_id"} {
		if text := clipText(strings.TrimSpace(anyToString(value[key])), 800); text != "" {
			out[key] = text
		}
	}
	return out
}

func (s *temporalClaimStore) upsert(payload map[string]any) (temporalClaim, error) {
	if s == nil || !s.enabled {
		return temporalClaim{}, errors.New("temporal claim graph is disabled")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	claimID := strings.TrimSpace(anyToString(payload["claim_id"]))
	var previous *temporalClaim
	if existing, ok := s.claims[claimID]; ok {
		copy := existing
		previous = &copy
	}
	claim, err := normalizeTemporalClaim(payload, previous)
	if err != nil {
		return temporalClaim{}, err
	}
	if previous == nil {
		if existing, ok := s.claims[claim.ClaimID]; ok {
			copy := existing
			previous = &copy
			claim, err = normalizeTemporalClaim(payload, previous)
			if err != nil {
				return temporalClaim{}, err
			}
		}
	}
	changed := []temporalClaim{claim}
	for _, supersededID := range claim.Supersedes {
		target, ok := s.claims[supersededID]
		if !ok || target.Status == "superseded" {
			continue
		}
		target.Status = "superseded"
		target.UpdatedAt = claim.UpdatedAt
		target.Revision++
		changed = append(changed, target)
	}
	if err := s.appendBatchLocked(changed); err != nil {
		s.lastError = err.Error()
		return temporalClaim{}, err
	}
	for _, row := range changed {
		s.setClaimLocked(row)
	}
	s.trimLocked()
	if s.logEntries >= s.maxClaims*2 && s.logEntries%s.compactEvery < len(changed) {
		if err := s.compactLocked(); err != nil {
			s.lastError = err.Error()
		}
	}
	return claim, nil
}

func (s *temporalClaimStore) appendLocked(claim temporalClaim) error {
	return s.appendBatchLocked([]temporalClaim{claim})
}

func (s *temporalClaimStore) appendBatchLocked(claims []temporalClaim) error {
	if len(claims) == 0 {
		return nil
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	for _, claim := range claims {
		if err := encoder.Encode(claim); err != nil {
			return fmt.Errorf("encode temporal claim: %w", err)
		}
	}
	file, err := openOwnerOnlyAppend(s.path, false)
	if err != nil {
		return fmt.Errorf("open temporal claim ledger for append: %w", err)
	}
	if _, err := file.Write(encoded.Bytes()); err != nil {
		_ = file.Close()
		return fmt.Errorf("append temporal claim: %w", err)
	}
	if s.fsync {
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return fmt.Errorf("sync temporal claim ledger: %w", err)
		}
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temporal claim ledger: %w", err)
	}
	s.logEntries += len(claims)
	s.lastPersistedAt = nowUTCISO()
	return nil
}

func (s *temporalClaimStore) trimLocked() {
	if len(s.claims) <= s.maxClaims {
		return
	}
	rows := make([]temporalClaim, 0, len(s.claims))
	for _, claim := range s.claims {
		rows = append(rows, claim)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].UpdatedAt > rows[j].UpdatedAt })
	for _, claim := range rows[s.maxClaims:] {
		s.removeProofClaimRefLocked(claim)
		delete(s.claims, claim.ClaimID)
		s.proofRevision = nextProofTimelineRevision(s.proofRevision)
	}
}

func (s *temporalClaimStore) compactLocked() error {
	tmp := s.path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	rows := make([]temporalClaim, 0, len(s.claims))
	for _, claim := range s.claims {
		rows = append(rows, claim)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ClaimID < rows[j].ClaimID })
	encoder := json.NewEncoder(file)
	for _, claim := range rows {
		if err := encoder.Encode(claim); err != nil {
			_ = file.Close()
			_ = os.Remove(tmp)
			return err
		}
	}
	if s.fsync {
		_ = file.Sync()
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	s.logEntries = len(rows)
	s.lastPersistedAt = nowUTCISO()
	return nil
}

func (s *temporalClaimStore) query(q temporalClaimQuery) []temporalClaim {
	if s == nil || !s.enabled {
		return []temporalClaim{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	asOf := q.AsOf
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	terms := synthesisPackQueryTokens(q.Query)
	limit := clampInt(q.Limit, 1, 200)
	type scoredClaim struct {
		claim temporalClaim
		score int
	}
	rows := make([]scoredClaim, 0, limit)
	for _, claim := range s.claims {
		status := temporalClaimStatusAt(claim, asOf)
		if q.Project != "" && !strings.EqualFold(claim.Project, q.Project) {
			continue
		}
		if q.Subject != "" && !strings.Contains(strings.ToLower(claim.Subject), strings.ToLower(q.Subject)) {
			continue
		}
		if q.Predicate != "" && claim.Predicate != strings.ToLower(q.Predicate) {
			continue
		}
		if q.Status != "" && status != q.Status {
			continue
		}
		if status == "expired" && !q.IncludeExpired {
			continue
		}
		if status == "superseded" && !q.IncludeSuperseded {
			continue
		}
		if status == "retracted" && q.Status != "retracted" && !q.IncludeRetracted {
			continue
		}
		score := temporalClaimTermScore(claim, terms)
		if len(terms) > 0 && score == 0 {
			continue
		}
		copy := claim
		copy.Status = status
		candidate := scoredClaim{claim: copy, score: score}
		if len(rows) < limit {
			rows = append(rows, candidate)
			continue
		}
		worst := 0
		for idx := 1; idx < len(rows); idx++ {
			if temporalScoredClaimBetter(rows[worst].claim, rows[worst].score, rows[idx].claim, rows[idx].score) {
				worst = idx
			}
		}
		if temporalScoredClaimBetter(candidate.claim, candidate.score, rows[worst].claim, rows[worst].score) {
			rows[worst] = candidate
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return temporalScoredClaimBetter(rows[i].claim, rows[i].score, rows[j].claim, rows[j].score)
	})
	out := make([]temporalClaim, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.claim)
	}
	return out
}

func temporalScoredClaimBetter(left temporalClaim, leftScore int, right temporalClaim, rightScore int) bool {
	if leftScore != rightScore {
		return leftScore > rightScore
	}
	if left.Confidence != right.Confidence {
		return left.Confidence > right.Confidence
	}
	if left.UpdatedAt != right.UpdatedAt {
		return left.UpdatedAt > right.UpdatedAt
	}
	return left.ClaimID < right.ClaimID
}

func temporalClaimStatusAt(claim temporalClaim, asOf time.Time) string {
	if claim.Status == "superseded" || claim.Status == "retracted" || claim.Status == "expired" {
		return claim.Status
	}
	if claim.ValidFrom != "" {
		if from, err := time.Parse(time.RFC3339Nano, claim.ValidFrom); err == nil && asOf.Before(from) {
			return "pending"
		}
	}
	if claim.ValidTo != "" {
		if to, err := time.Parse(time.RFC3339Nano, claim.ValidTo); err == nil && !asOf.Before(to) {
			return "expired"
		}
	}
	return "active"
}

func temporalClaimCanInfluence(claim temporalClaim) bool {
	return strings.EqualFold(strings.TrimSpace(claim.Status), "active")
}

func temporalClaimIsHistoricalOpposition(claim temporalClaim) bool {
	switch strings.ToLower(strings.TrimSpace(claim.Status)) {
	case "expired", "superseded", "retracted":
		return true
	default:
		return false
	}
}

func temporalClaimInfluenceRank(claim temporalClaim) int {
	if temporalClaimCanInfluence(claim) {
		return 0
	}
	if temporalClaimIsHistoricalOpposition(claim) {
		return 1
	}
	return 2
}

func temporalClaimTermScore(claim temporalClaim, terms []string) int {
	if len(terms) == 0 {
		return 1
	}
	haystack := claim.searchText
	if haystack == "" {
		haystack = temporalClaimSearchText(claim)
	}
	score := 0
	for _, term := range terms {
		if strings.Contains(haystack, term) {
			score++
		}
	}
	return score
}

func temporalClaimSearchText(claim temporalClaim) string {
	return strings.ToLower(strings.Join([]string{claim.Subject, claim.Predicate, claim.Object, claim.Statement, claim.TopicPath}, " "))
}

func temporalClaimMaps(rows []temporalClaim) []any {
	out := make([]any, 0, len(rows))
	for _, claim := range rows {
		var row map[string]any
		encoded, _ := json.Marshal(claim)
		_ = json.Unmarshal(encoded, &row)
		row["contradiction_state"] = "clear"
		if len(claim.Contradicts) > 0 || len(claim.Opposition) > 0 {
			row["contradiction_state"] = "contested"
		}
		out = append(out, row)
	}
	return out
}

func (s *temporalClaimStore) snapshot() map[string]any {
	if s == nil {
		return map[string]any{
			"schema_id": "temporal_claim_graph_status.v1", "enabled": false, "claim_count": 0,
			"status_counts": map[string]int{}, "verification_counts": map[string]int{},
			"contradiction_links": 0, "log_entries": 0, "parse_errors": 0, "max_claims": 0,
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	statusCounts := map[string]int{}
	verificationCounts := map[string]int{}
	conflicts := 0
	for _, claim := range s.claims {
		statusCounts[temporalClaimStatusAt(claim, time.Now().UTC())]++
		verificationCounts[anyToString(claim.Verification["status"])]++
		conflicts += len(claim.Contradicts)
	}
	return map[string]any{
		"schema_id":           "temporal_claim_graph_status.v1",
		"enabled":             s.enabled,
		"claim_count":         len(s.claims),
		"status_counts":       statusCounts,
		"verification_counts": verificationCounts,
		"contradiction_links": conflicts,
		"log_entries":         s.logEntries,
		"parse_errors":        s.parseErrors,
		"max_claims":          s.maxClaims,
		"last_persisted_at":   s.lastPersistedAt,
		"last_error":          s.lastError,
	}
}

func claimQueryFromPayload(payload map[string]any) (temporalClaimQuery, error) {
	q := temporalClaimQuery{
		Project:           strings.TrimSpace(anyToString(payload["project"])),
		Query:             strings.TrimSpace(anyToString(payload["query"])),
		Subject:           strings.TrimSpace(anyToString(payload["subject"])),
		Predicate:         strings.TrimSpace(strings.ToLower(anyToString(payload["predicate"]))),
		Status:            strings.TrimSpace(strings.ToLower(anyToString(payload["status"]))),
		Limit:             clampInt(anyToInt(payload["limit"], 20), 1, 200),
		IncludeExpired:    anyToBool(payload["include_expired"]),
		IncludeSuperseded: anyToBool(payload["include_superseded"]),
	}
	if raw := strings.TrimSpace(anyToString(payload["as_of"])); raw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return q, errors.New("as_of must be RFC3339")
		}
		q.AsOf = parsed.UTC()
	}
	return q, nil
}

func (s *server) memoryClaimsWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	s.handleClaimWrite(w, r, false)
}

func (s *server) toolsClaimWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareToolHeaders(w, r, "/tools/claim_write"); !ok {
		return
	}
	s.handleClaimWrite(w, r, true)
}

func (s *server) handleClaimWrite(w http.ResponseWriter, r *http.Request, tool bool) {
	payload, err := readOptionalJSONBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
		return
	}
	if nested := anyMap(payload["claim"]); len(nested) > 0 {
		payload = nested
	}
	claim, err := s.temporalClaims.upsert(payload)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "invalid_temporal_claim", "detail": err.Error()})
		return
	}
	response := map[string]any{"ok": true, "schema_id": temporalClaimContractID, "claim": temporalClaimMaps([]temporalClaim{claim})[0], "recorded": true}
	if tool {
		response["tool"] = "claim_write"
	}
	writeJSON(w, http.StatusOK, attachPayloadFormatContract(temporalClaimContractID, response, claim.AgentID, "temporal_claim", r.URL.Path))
}

func (s *server) memoryClaimsQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	s.handleClaimQuery(w, r, false)
}

func (s *server) toolsClaimQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareToolHeaders(w, r, "/tools/claim_query"); !ok {
		return
	}
	s.handleClaimQuery(w, r, true)
}

func (s *server) handleClaimQuery(w http.ResponseWriter, r *http.Request, tool bool) {
	payload, err := readOptionalJSONBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
		return
	}
	query, err := claimQueryFromPayload(payload)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "invalid_claim_query", "detail": err.Error()})
		return
	}
	claims := temporalClaimMaps(s.temporalClaims.query(query))
	response := map[string]any{
		"ok": true, "schema_id": temporalClaimQueryContractID, "project": query.Project,
		"query": query.Query, "as_of": firstNonEmptyStrings(anyToString(payload["as_of"]), nowUTCISO()),
		"claims": claims, "claim_count": len(claims), "graph_status": s.temporalClaims.snapshot(),
	}
	if tool {
		response["tool"] = "claim_query"
	}
	writeJSON(w, http.StatusOK, attachPayloadFormatContract(temporalClaimQueryContractID, response, anyToString(payload["agent_id"]), "temporal_claim_query", r.URL.Path))
}

func (s *server) telemetryClaimGraph(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.temporalClaims.snapshot())
}
