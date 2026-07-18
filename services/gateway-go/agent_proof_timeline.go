package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	agentProofTimelineFeatureEnv       = "CONTEXTLATTICE_AGENT_PROOF_TIMELINE_ENABLED"
	maxAgentProofTimelineRows          = 256
	maxAgentProofTimelineSourceRows    = 512
	maxAgentProofTimelineSourceScans   = 2048
	maxAgentProofTimelineEvidenceBytes = 2 * 1024
	maxAgentProofTimelineRowsBytes     = 96 * 1024
	maxAgentProofTimelineDroppedKeys   = 8192
	proofTimelineClockSkew             = 5 * time.Minute
)

var agentProofTimelineStages = []string{"context", "action", "correction", "verification", "outcome", "learning"}

var proofTimelineBearerPattern = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]{12,}`)
var proofTimelineSecretPattern = regexp.MustCompile(`(?i)\b(?:sk|rk|pk)-[A-Za-z0-9_-]{12,}`)
var proofTimelineJWTPattern = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)
var proofTimelineGitHubTokenPattern = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`)
var proofTimelineSlackTokenPattern = regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{20,}\b`)
var proofTimelineAWSAccessKeyPattern = regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)
var proofTimelineNPMTokenPattern = regexp.MustCompile(`\bnpm_[A-Za-z0-9]{20,}\b`)
var proofTimelineCommonTokenPattern = regexp.MustCompile(`(?i)\b(?:glpat-|hf_|lin_api_|AIza)[A-Za-z0-9_-]{16,}\b`)
var proofTimelineCredentialAssignmentPattern = regexp.MustCompile(`(?i)\b(?:api[_-]?key|access[_-]?token|refresh[_-]?token|session[_-]?token|id[_-]?token|authorization|password|secret)\s*[:=]\s*["']?[A-Za-z0-9._~+/-]{6,}["']?`)
var proofTimelinePersonalPathPattern = regexp.MustCompile(`(?i)(?:/(?:Users|Volumes|home|root|private|tmp|var/folders|mnt|media)/[^\s"']*|[A-Z]:\\Users\\[^\\\s"']+(?:\\[^\s"']*)?)`)
var proofTimelineHomePathPattern = regexp.MustCompile(`(?:^|\s)~/(?:[^\s"']+)`)

type proofTimelineMapRing struct {
	rows           []map[string]any
	next           int
	dropped        int64
	droppedByKey   map[string]int
	droppedUnknown bool
}

func (r *proofTimelineMapRing) add(row map[string]any) {
	if r == nil || len(row) == 0 {
		return
	}
	if len(r.rows) < maxAgentProofTimelineSourceScans {
		r.rows = append(r.rows, row)
		return
	}
	r.recordDrop(r.rows[r.next])
	r.rows[r.next] = row
	r.next = (r.next + 1) % maxAgentProofTimelineSourceScans
	r.dropped++
}

func (r *proofTimelineMapRing) ordered() []map[string]any {
	if r == nil || len(r.rows) == 0 {
		return nil
	}
	if len(r.rows) < maxAgentProofTimelineSourceScans || r.next == 0 {
		return append([]map[string]any(nil), r.rows...)
	}
	rows := make([]map[string]any, 0, len(r.rows))
	rows = append(rows, r.rows[r.next:]...)
	rows = append(rows, r.rows[:r.next]...)
	return rows
}

func (r *proofTimelineMapRing) recordDrop(row map[string]any) {
	if r == nil {
		return
	}
	keys := proofTimelineIdentityIndexKeys(proofTimelineIdentityFromMaps(row))
	if len(keys) == 0 {
		r.droppedUnknown = true
		return
	}
	if r.droppedByKey == nil {
		r.droppedByKey = map[string]int{}
	}
	for _, key := range keys {
		if _, exists := r.droppedByKey[key]; !exists && len(r.droppedByKey) >= maxAgentProofTimelineDroppedKeys {
			r.droppedUnknown = true
			continue
		}
		r.droppedByKey[key]++
	}
}

func (r *proofTimelineMapRing) droppedForScope(scope proofTimelineScope) int {
	if r == nil {
		return 0
	}
	omitted := 0
	for _, key := range proofTimelineScopeIndexKeys(scope, true) {
		omitted = maxInt(omitted, r.droppedByKey[key])
	}
	if r.droppedUnknown {
		omitted = maxInt(omitted, 1)
	}
	return omitted
}

type proofTimelineScope struct {
	SessionID      string
	Project        string
	TaskID         string
	TaskIdentityID string
	SampleIDs      map[string]struct{}
}

type agentProofTimelineSnapshot struct {
	Session             map[string]any
	Events              []map[string]any
	ContinuityEntries   []continuityLedgerEntry
	ContinuityIntegrity bool
	Claims              []temporalClaim
	QualitySamples      []map[string]any
	QualityOutcomes     []map[string]any
	TokenImpacts        []map[string]any
	Availability        map[string]bool
	SourceOmitted       map[string]int
	SourceAnchorsBefore map[string]any
	SourceAnchorsAfter  map[string]any
}

type proofTimelineCandidate struct {
	Source     string
	SourceID   string
	Type       string
	Stage      string
	Status     string
	Summary    string
	OccurredAt string
	RecordedAt string
	Links      map[string]any
	Evidence   map[string]any
}

type proofTimelineBuilder struct {
	scope              proofTimelineScope
	now                time.Time
	rows               []map[string]any
	seen               map[string]string
	gaps               map[string]map[string]any
	stageCounts        map[string]int
	sourceCounts       map[string]int
	eligibleRows       int
	joinedRows         int
	duplicateCount     int
	conflictCount      int
	crossScopeRejected int
	missingJoinKeys    int
	invalidClockCount  int
	recordedClockCount int
	redactionCount     int
	displayCompacted   int
	evidenceCompacted  int
	projectionOmitted  int
}

func agentProofTimelineEnabled() bool {
	return envBool(agentProofTimelineFeatureEnv, true)
}

func copyProofTimelineIdentity(dst map[string]any, source map[string]any) {
	if dst == nil || source == nil {
		return
	}
	aliases := []struct {
		canonical string
		keys      []string
		limit     int
	}{
		{canonical: "sample_id", keys: []string{"sample_id", "context_pack_quality_sample_id"}, limit: 200},
		{canonical: "session_id", keys: []string{"session_id", "sessionId"}, limit: maxAgentSessionIDLength},
		{canonical: "task_id", keys: []string{"task_id", "taskId"}, limit: 160},
		{canonical: "task_identity_id", keys: []string{"task_identity_id", "taskIdentityId"}, limit: 160},
		{canonical: "execution_lane_id", keys: []string{"execution_lane_id", "executionLaneId"}, limit: 160},
		{canonical: "project", keys: []string{"project", "project_name", "projectName"}, limit: 160},
		{canonical: "agent_id", keys: []string{"agent_id", "agentId"}, limit: 160},
	}
	for _, alias := range aliases {
		if strings.TrimSpace(anyToString(dst[alias.canonical])) != "" {
			continue
		}
		values := make([]string, 0, len(alias.keys))
		for _, key := range alias.keys {
			values = append(values, anyToString(source[key]))
		}
		if value := strings.TrimSpace(firstNonEmptyStrings(values...)); value != "" {
			dst[alias.canonical] = clipText(value, alias.limit)
		}
	}
}

func proofTimelineIdentityFromMaps(values ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, value := range values {
		copyProofTimelineIdentity(out, value)
	}
	return out
}

func proofTimelineScopeFromSession(session map[string]any, events []map[string]any) proofTimelineScope {
	ownership := agentSessionOwnership(session)
	scope := proofTimelineScope{
		SessionID:      strings.TrimSpace(anyToString(session["id"])),
		Project:        strings.TrimSpace(anyToString(session["project"])),
		TaskID:         strings.TrimSpace(firstNonEmptyStrings(anyToString(session["task_id"]), anyToString(ownership["task_id"]))),
		TaskIdentityID: strings.TrimSpace(firstNonEmptyStrings(anyToString(session["task_identity_id"]), anyToString(ownership["task_identity_id"]))),
		SampleIDs:      map[string]struct{}{},
	}
	for _, event := range events {
		metadata := anyMap(event["metadata"])
		for _, value := range []string{
			anyToString(metadata["context_pack_quality_sample_id"]),
			anyToString(anyMap(metadata["outcome"])["sample_id"]),
		} {
			if value = strings.TrimSpace(value); value != "" {
				scope.SampleIDs[value] = struct{}{}
			}
		}
	}
	return scope
}

func proofTimelineIdentityRelevant(identity map[string]any, scope proofTimelineScope) bool {
	if sessionID := strings.TrimSpace(anyToString(identity["session_id"])); sessionID != "" && sessionID == scope.SessionID {
		return true
	}
	if taskIdentityID := strings.TrimSpace(anyToString(identity["task_identity_id"])); taskIdentityID != "" && taskIdentityID == scope.TaskIdentityID {
		return true
	}
	if taskID := strings.TrimSpace(anyToString(identity["task_id"])); taskID != "" && taskID == scope.TaskID {
		return true
	}
	if sampleID := strings.TrimSpace(anyToString(identity["sample_id"])); sampleID != "" {
		_, ok := scope.SampleIDs[sampleID]
		return ok
	}
	return false
}

func proofTimelineSensitiveKey(key string) bool {
	normalized := strings.ToLower(strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(strings.TrimSpace(key)))
	compact := strings.ReplaceAll(normalized, "_", "")
	if compact == "tokenimpact" || compact == "tokenizerencoding" {
		return false
	}
	if compact == "token" || compact == "bearer" || strings.HasSuffix(compact, "token") || strings.Contains(compact, "prompt") {
		return true
	}
	for _, exact := range []string{"content", "input", "output", "requestbody", "responsebody"} {
		if compact == exact {
			return true
		}
	}
	for _, fragment := range []string{
		"apikey", "accesstoken", "authtoken", "secret", "password", "credential",
		"privatekey", "authorization", "rawprompt", "messages", "toolcalls", "functioncall",
		"hookspecificoutput", "rawcontextlatticejson", "webhooksecret", "signingsecret",
	} {
		if strings.Contains(compact, fragment) {
			return true
		}
	}
	return false
}

func proofTimelineMarkDisplayCompacted(displayCompacted *int) {
	if displayCompacted != nil {
		*displayCompacted++
	}
}

func proofTimelineRedactString(value string, redactions *int, displayCompacted *int) string {
	redacted := value
	for _, pattern := range []*regexp.Regexp{
		proofTimelineBearerPattern,
		proofTimelineSecretPattern,
		proofTimelineJWTPattern,
		proofTimelineGitHubTokenPattern,
		proofTimelineSlackTokenPattern,
		proofTimelineAWSAccessKeyPattern,
		proofTimelineNPMTokenPattern,
		proofTimelineCommonTokenPattern,
		proofTimelineCredentialAssignmentPattern,
		proofTimelinePersonalPathPattern,
		proofTimelineHomePathPattern,
	} {
		matches := pattern.FindAllStringIndex(redacted, -1)
		if len(matches) == 0 {
			continue
		}
		*redactions += len(matches)
		redacted = pattern.ReplaceAllString(redacted, "[REDACTED]")
	}
	clipped := clipText(redacted, 1200)
	if clipped != redacted {
		proofTimelineMarkDisplayCompacted(displayCompacted)
	}
	return clipped
}

func proofTimelineRedact(value any, depth int, redactions *int, displayCompacted *int) any {
	if depth > 6 {
		// Depth overflow is a fail-closed boundary. Stringifying the subtree here
		// would bypass both sensitive-key filtering and recursive secret redaction.
		*redactions++
		proofTimelineMarkDisplayCompacted(displayCompacted)
		return "[REDACTED]"
	}
	switch typed := value.(type) {
	case map[string]any:
		out := map[string]any{}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if len(keys) > 64 {
			proofTimelineMarkDisplayCompacted(displayCompacted)
			keys = keys[:64]
		}
		for _, key := range keys {
			if proofTimelineSensitiveKey(key) {
				*redactions++
				continue
			}
			clippedKey := clipText(key, 96)
			if clippedKey != key {
				proofTimelineMarkDisplayCompacted(displayCompacted)
			}
			out[clippedKey] = proofTimelineRedact(typed[key], depth+1, redactions, displayCompacted)
		}
		return out
	case []any:
		limit := minInt(len(typed), 32)
		if limit < len(typed) {
			proofTimelineMarkDisplayCompacted(displayCompacted)
		}
		out := make([]any, 0, limit)
		for _, item := range typed[:limit] {
			out = append(out, proofTimelineRedact(item, depth+1, redactions, displayCompacted))
		}
		return out
	case []string:
		limit := minInt(len(typed), 32)
		if limit < len(typed) {
			proofTimelineMarkDisplayCompacted(displayCompacted)
		}
		out := make([]any, 0, limit)
		for _, item := range typed[:limit] {
			out = append(out, proofTimelineRedactString(item, redactions, displayCompacted))
		}
		return out
	case string:
		return proofTimelineRedactString(typed, redactions, displayCompacted)
	case nil, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
		return value
	default:
		return proofTimelineRedactString(anyToString(value), redactions, displayCompacted)
	}
}

func proofTimelineDigest(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func proofTimelineCompactEvidence(value any) (any, bool) {
	size := jsonByteLen(value)
	if size <= maxAgentProofTimelineEvidenceBytes {
		return value, false
	}
	compact := map[string]any{
		"compacted": true, "evidence_digest": proofTimelineDigest(value), "json_bytes_before": size,
	}
	if object, ok := value.(map[string]any); ok {
		keys := sortedMapKeys(object)
		if len(keys) > 16 {
			keys = keys[:16]
		}
		compact["keys"] = stringSliceAny(keys)
	}
	return compact, true
}

func proofTimelineBoundRows(rows []map[string]any) ([]map[string]any, int) {
	start := len(rows)
	bytesUsed := 0
	for index := len(rows) - 1; index >= 0; index-- {
		if len(rows)-start >= maxAgentProofTimelineRows {
			break
		}
		rowBytes := jsonByteLen(rows[index])
		if bytesUsed+rowBytes > maxAgentProofTimelineRowsBytes {
			break
		}
		bytesUsed += rowBytes
		start = index
	}
	if start == len(rows) && len(rows) > 0 {
		latest := cloneAnyMap(rows[len(rows)-1])
		latest["summary"] = clipText(anyToString(latest["summary"]), 320)
		latest["evidence"] = map[string]any{"compacted": true, "reason": "row_exceeded_projection_budget"}
		return []map[string]any{latest}, len(rows) - 1
	}
	return append([]map[string]any{}, rows[start:]...), start
}

func newProofTimelineBuilder(scope proofTimelineScope, now time.Time) *proofTimelineBuilder {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return &proofTimelineBuilder{
		scope:        scope,
		now:          now.UTC(),
		rows:         []map[string]any{},
		seen:         map[string]string{},
		gaps:         map[string]map[string]any{},
		stageCounts:  map[string]int{},
		sourceCounts: map[string]int{},
	}
}

func (b *proofTimelineBuilder) addGap(code string, source string, stage string, detail string) {
	code = clipText(strings.TrimSpace(code), 80)
	if code == "" {
		return
	}
	key := strings.Join([]string{code, source, stage, detail}, "\x00")
	if _, exists := b.gaps[key]; exists {
		return
	}
	gap := map[string]any{
		"code":     code,
		"source":   clipText(strings.TrimSpace(source), 80),
		"material": true,
		"detail":   clipText(strings.TrimSpace(detail), 320),
	}
	if stage != "" {
		gap["stage"] = stage
	}
	b.gaps[key] = gap
}

func (b *proofTimelineBuilder) candidateJoinable(candidate proofTimelineCandidate) (bool, string) {
	links := candidate.Links
	project := strings.TrimSpace(anyToString(links["project"]))
	if project != "" && b.scope.Project != "" && !strings.EqualFold(project, b.scope.Project) {
		return false, "cross_scope_rejected"
	}
	checks := []struct {
		key   string
		scope string
	}{
		{key: "session_id", scope: b.scope.SessionID},
		{key: "task_identity_id", scope: b.scope.TaskIdentityID},
		{key: "task_id", scope: b.scope.TaskID},
	}
	joined := false
	for _, check := range checks {
		value := strings.TrimSpace(anyToString(links[check.key]))
		if value == "" {
			continue
		}
		if check.scope != "" && value != check.scope {
			return false, "cross_scope_rejected"
		}
		if check.scope != "" && value == check.scope {
			joined = true
		}
	}
	if sampleID := strings.TrimSpace(anyToString(links["sample_id"])); sampleID != "" {
		if _, ok := b.scope.SampleIDs[sampleID]; ok {
			joined = true
		} else if candidate.Source == "context_pack_quality" || candidate.Source == "context_pack_outcome" || candidate.Source == "token_impact" {
			return false, "cross_scope_rejected"
		}
	}
	if !joined {
		return false, "missing_join_key"
	}
	return true, ""
}

func (b *proofTimelineBuilder) orderedClock(candidate proofTimelineCandidate) (time.Time, string, string) {
	parse := func(raw string) (time.Time, bool) {
		parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
		if err != nil || parsed.After(b.now.Add(proofTimelineClockSkew)) || parsed.Before(time.Unix(0, 0).UTC()) {
			return time.Time{}, false
		}
		return parsed.UTC(), true
	}
	if occurred, ok := parse(candidate.OccurredAt); ok {
		return occurred, occurred.Format(time.RFC3339Nano), "occurred_at"
	}
	if strings.TrimSpace(candidate.OccurredAt) != "" {
		b.invalidClockCount++
		b.addGap("invalid_clock", candidate.Source, candidate.Stage, "invalid or future occurred_at; deterministic ordering used recorded_at")
	}
	if recorded, ok := parse(candidate.RecordedAt); ok {
		b.recordedClockCount++
		return recorded, recorded.Format(time.RFC3339Nano), "recorded_at"
	}
	b.invalidClockCount++
	b.addGap("invalid_clock", candidate.Source, candidate.Stage, "no valid occurred_at or recorded_at; deterministic epoch fallback used")
	return time.Unix(0, 0).UTC(), time.Unix(0, 0).UTC().Format(time.RFC3339), "epoch_fallback"
}

func (b *proofTimelineBuilder) addCandidate(candidate proofTimelineCandidate) {
	b.eligibleRows++
	joinable, reason := b.candidateJoinable(candidate)
	if !joinable {
		if reason == "cross_scope_rejected" {
			b.crossScopeRejected++
			b.addGap(reason, candidate.Source, candidate.Stage, "source row carried a foreign project, session, task, or sample identity and was not emitted")
		} else {
			b.missingJoinKeys++
			b.addGap(reason, candidate.Source, candidate.Stage, "source row lacked an exact session, task, task-identity, or linked sample key and was not inferred by time")
		}
		return
	}
	b.joinedRows++
	if strings.TrimSpace(candidate.SourceID) == "" {
		b.addGap("missing_source_id", candidate.Source, candidate.Stage, "joined source row lacked a stable source identifier")
		return
	}
	// Conflict identity is computed from the full source record before any
	// display clipping. Only the one-way digest crosses the response boundary.
	digest := proofTimelineDigest(map[string]any{
		"source": candidate.Source, "source_id": candidate.SourceID, "type": candidate.Type,
		"stage": candidate.Stage, "status": candidate.Status, "summary": candidate.Summary,
		"occurred_at": candidate.OccurredAt, "recorded_at": candidate.RecordedAt,
		"links": candidate.Links, "evidence": candidate.Evidence,
	})
	compactedBefore := b.displayCompacted
	redactedEvidence := proofTimelineRedact(candidate.Evidence, 0, &b.redactionCount, &b.displayCompacted)
	redactedSummary := proofTimelineRedactString(candidate.Summary, &b.redactionCount, &b.displayCompacted)
	redactedLinks := proofTimelineRedact(candidate.Links, 0, &b.redactionCount, &b.displayCompacted)
	emittedEvidence, compacted := proofTimelineCompactEvidence(redactedEvidence)
	if compacted {
		b.evidenceCompacted++
		b.displayCompacted++
	}
	displayCompacted := b.displayCompacted > compactedBefore
	dedupeKey := candidate.Source + "\x00" + candidate.SourceID
	if previous, exists := b.seen[dedupeKey]; exists {
		if previous == digest {
			b.duplicateCount++
			return
		}
		b.conflictCount++
		b.addGap("identity_collision", candidate.Source, candidate.Stage, "the same source identifier carried a different canonical digest")
		return
	}
	b.seen[dedupeKey] = digest
	ordered, orderedAt, clockSource := b.orderedClock(candidate)
	links := anyMap(redactedLinks)
	for key, value := range map[string]string{
		"session_id": b.scope.SessionID, "project": b.scope.Project,
		"task_id": b.scope.TaskID, "task_identity_id": b.scope.TaskIdentityID,
	} {
		if strings.TrimSpace(anyToString(links[key])) == "" && value != "" {
			links[key] = value
		}
	}
	row := map[string]any{
		"source":        candidate.Source,
		"source_id":     candidate.SourceID,
		"source_digest": digest,
		"type":          clipText(candidate.Type, 96),
		"stage":         candidate.Stage,
		"status":        clipText(candidate.Status, 64),
		"summary":       redactedSummary,
		"occurred_at":   clipText(candidate.OccurredAt, 80),
		"recorded_at":   clipText(candidate.RecordedAt, 80),
		"ordered_at":    orderedAt,
		"clock_source":  clockSource,
		"links":         links,
		"evidence":      emittedEvidence,
		"_ordered_unix": ordered.UnixNano(),
	}
	if displayCompacted {
		row["display_compacted"] = true
	}
	b.rows = append(b.rows, row)
	b.stageCounts[candidate.Stage]++
	b.sourceCounts[candidate.Source]++
}

func proofTimelineStageForEvent(eventType string) string {
	lower := strings.ToLower(strings.TrimSpace(eventType))
	switch {
	case strings.Contains(lower, "correct") || strings.Contains(lower, "repair") || strings.Contains(lower, "decision_changed"):
		return "correction"
	case strings.Contains(lower, "writeback") || strings.Contains(lower, "checkpoint") || strings.Contains(lower, "claim") || strings.Contains(lower, "skill") || strings.Contains(lower, "learn"):
		return "learning"
	case strings.Contains(lower, "verif") || strings.Contains(lower, "test") || strings.Contains(lower, "audit") || strings.Contains(lower, "check"):
		return "verification"
	case strings.Contains(lower, "outcome") || strings.Contains(lower, "feedback"):
		return "outcome"
	case strings.Contains(lower, "context_pack") || strings.Contains(lower, "context.package") || strings.Contains(lower, "context_package") || strings.Contains(lower, "retrieval"):
		return "context"
	case strings.Contains(lower, "complete"):
		return "outcome"
	default:
		return "action"
	}
}

func proofTimelineSessionCandidate(event map[string]any) proofTimelineCandidate {
	metadata := anyMap(event["metadata"])
	links := proofTimelineIdentityFromMaps(
		event,
		metadata,
		anyMap(event["ownership"]),
		anyMap(metadata["ownership"]),
		anyMap(event["agent_state"]),
		anyMap(metadata["agent_state"]),
		anyMap(metadata["outcome"]),
	)
	return proofTimelineCandidate{
		Source:     "agent_session",
		SourceID:   anyToString(event["id"]),
		Type:       anyToString(event["type"]),
		Stage:      proofTimelineStageForEvent(anyToString(event["type"])),
		Status:     anyToString(event["status"]),
		Summary:    anyToString(event["summary"]),
		OccurredAt: anyToString(event["created_at"]),
		RecordedAt: anyToString(event["created_at"]),
		Links:      links,
		Evidence:   metadata,
	}
}

func proofTimelineContinuityCandidate(entry continuityLedgerEntry) proofTimelineCandidate {
	payload := entry.Payload
	links := proofTimelineIdentityFromMaps(payload)
	typeName := entry.Kind
	stage := "action"
	summary := anyToString(payload["reason"])
	status := "recorded"
	occurredAt := firstNonEmptyStrings(anyToString(payload["occurred_at"]), anyToString(payload["created_at"]), entry.RecordedAt)
	evidence := cloneAnyMap(payload)
	switch entry.Kind {
	case continuityLedgerKindTaskIdentity:
		typeName = "task_identity." + firstNonEmptyStrings(anyToString(payload["operation"]), "recorded")
		summary = firstNonEmptyStrings(summary, anyToString(payload["objective"]), "task identity recorded")
	case continuityLedgerKindObjectiveTransition:
		transitionType := anyToString(payload["transition_type"])
		typeName = "objective." + firstNonEmptyStrings(transitionType, "transitioned")
		summary = firstNonEmptyStrings(anyToString(payload["summary"]), anyToString(payload["objective"]), "objective transition recorded")
		status = firstNonEmptyStrings(anyToString(payload["to_status"]), status)
		switch transitionType {
		case "decision_changed":
			stage = "correction"
		case "completed", "outcome_recorded":
			stage = "outcome"
		case "checkpointed":
			stage = "learning"
		}
	case continuityLedgerKindDecisionChange:
		typeName = "decision.changed"
		stage = "correction"
		summary = firstNonEmptyStrings(anyToString(payload["rationale"]), anyToString(anyMap(payload["after"])["summary"]), "decision changed")
	case continuityLedgerKindDecisionBundle:
		change := anyMap(payload["decision_change"])
		transition := anyMap(payload["objective_transition"])
		links = proofTimelineIdentityFromMaps(change, transition, payload)
		typeName = "decision.bundle_recorded"
		stage = "correction"
		summary = firstNonEmptyStrings(anyToString(change["rationale"]), anyToString(transition["summary"]), "decision correction bundle recorded")
		occurredAt = firstNonEmptyStrings(anyToString(change["occurred_at"]), anyToString(transition["occurred_at"]), entry.RecordedAt)
	}
	return proofTimelineCandidate{
		Source: "continuity", SourceID: entry.EntryID, Type: typeName, Stage: stage, Status: status,
		Summary: summary, OccurredAt: occurredAt, RecordedAt: entry.RecordedAt, Links: links,
		Evidence: evidence,
	}
}

func proofTimelineClaimCandidate(claim temporalClaim) proofTimelineCandidate {
	raw, _ := json.Marshal(claim)
	row := map[string]any{}
	_ = json.Unmarshal(raw, &row)
	links := proofTimelineIdentityFromMaps(row, anyMap(row["provenance"]))
	return proofTimelineCandidate{
		Source: "temporal_claim", SourceID: claim.ClaimID + ":r" + fmt.Sprint(claim.Revision),
		Type: "claim." + firstNonEmptyStrings(claim.Status, "recorded"), Stage: "learning", Status: claim.Status,
		Summary:    firstNonEmptyStrings(claim.Statement, strings.TrimSpace(claim.Subject+" "+claim.Predicate+" "+claim.Object)),
		OccurredAt: claim.ObservedAt, RecordedAt: claim.UpdatedAt, Links: links, Evidence: row,
	}
}

func proofTimelineQualityCandidate(row map[string]any) proofTimelineCandidate {
	return proofTimelineCandidate{
		Source: "context_pack_quality", SourceID: anyToString(row["sample_id"]), Type: "context.quality_measured",
		Stage: "context", Status: firstNonEmptyStrings(anyToString(row["confidence"]), "recorded"),
		Summary:    fmt.Sprintf("context pack quality %d; exact prompt tokens saved %d", anyToInt(row["quality_score"], 0), anyToInt(row["exact_prompt_tokens_saved"], 0)),
		OccurredAt: anyToString(row["capturedAt"]), RecordedAt: anyToString(row["capturedAt"]),
		Links: proofTimelineIdentityFromMaps(row), Evidence: row,
	}
}

func proofTimelineOutcomeCandidate(row map[string]any) proofTimelineCandidate {
	return proofTimelineCandidate{
		Source: "context_pack_outcome", SourceID: anyToString(row["outcome_id"]), Type: "context.outcome_reported",
		Stage: "outcome", Status: firstNonEmptyStrings(anyToString(row["outcome_class"]), "recorded"),
		Summary:    fmt.Sprintf("context outcome %s; retries %d", firstNonEmptyStrings(anyToString(row["outcome_class"]), "reported"), anyToInt(row["retry_count"], 0)),
		OccurredAt: anyToString(row["capturedAt"]), RecordedAt: anyToString(row["capturedAt"]),
		Links: proofTimelineIdentityFromMaps(row), Evidence: row,
	}
}

func proofTimelineTokenImpactCandidate(row map[string]any) proofTimelineCandidate {
	return proofTimelineCandidate{
		Source: "token_impact", SourceID: firstNonEmptyStrings(anyToString(row["sample_id"]), anyToString(row["impact_id"])), Type: "context.token_impact_measured",
		Stage: "context", Status: firstNonEmptyStrings(anyToString(row["calibration_grade"]), "recorded"),
		Summary:    fmt.Sprintf("token impact measured; exact transport %d; saved estimate %d", anyToInt(row["transport_tokens_exact"], 0), anyToInt(row["saved_tokens_estimate"], 0)),
		OccurredAt: anyToString(row["capturedAt"]), RecordedAt: anyToString(row["capturedAt"]),
		Links: proofTimelineIdentityFromMaps(row), Evidence: row,
	}
}

func proofTimelineSortedGaps(gaps map[string]map[string]any) []any {
	rows := make([]map[string]any, 0, len(gaps))
	for _, gap := range gaps {
		rows = append(rows, gap)
	}
	sort.Slice(rows, func(i, j int) bool {
		left := strings.Join([]string{anyToString(rows[i]["code"]), anyToString(rows[i]["source"]), anyToString(rows[i]["stage"]), anyToString(rows[i]["detail"])}, "\x00")
		right := strings.Join([]string{anyToString(rows[j]["code"]), anyToString(rows[j]["source"]), anyToString(rows[j]["stage"]), anyToString(rows[j]["detail"])}, "\x00")
		return left < right
	})
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, row)
	}
	return out
}

func proofTimelineRatio(numerator int, denominator int) float64 {
	if denominator <= 0 {
		return 1
	}
	return roundFloat(float64(numerator)/float64(denominator), 6)
}

func buildAgentProofTimeline(snapshot agentProofTimelineSnapshot, now time.Time) map[string]any {
	started := time.Now()
	if len(snapshot.Session) == 0 {
		return missingAgentProofTimeline("", now)
	}
	scope := proofTimelineScopeFromSession(snapshot.Session, snapshot.Events)
	builder := newProofTimelineBuilder(scope, now)
	for source, available := range snapshot.Availability {
		if !available {
			builder.addGap("source_unavailable", source, "", "authoritative source was unavailable during proof projection")
		}
	}
	for source, omitted := range snapshot.SourceOmitted {
		if omitted > 0 {
			builder.addGap("source_scan_truncated", source, "", fmt.Sprintf("bounded exact-identity projection omitted at least %d older source references", omitted))
		}
	}
	if anyToInt(snapshot.Session["event_count"], len(snapshot.Events)) > len(snapshot.Events) {
		builder.addGap("retention_truncated", "agent_session", "", "session event_count exceeds the retained event rows")
	}
	if snapshot.Availability["continuity"] && !snapshot.ContinuityIntegrity {
		builder.addGap("corrupt_rows", "continuity", "", "continuity source integrity verification failed")
	}
	if proofTimelineDigest(snapshot.SourceAnchorsBefore) != proofTimelineDigest(snapshot.SourceAnchorsAfter) {
		builder.addGap("concurrent_snapshot", "projection", "", "one or more source anchors advanced while the read model was assembled")
	}
	for _, event := range snapshot.Events {
		builder.addCandidate(proofTimelineSessionCandidate(event))
	}
	for _, entry := range snapshot.ContinuityEntries {
		builder.addCandidate(proofTimelineContinuityCandidate(entry))
	}
	for _, claim := range snapshot.Claims {
		builder.addCandidate(proofTimelineClaimCandidate(claim))
		if claim.Revision > 1 {
			builder.addGap("history_compacted", "temporal_claim", "learning", "only the latest claim revision is retained in the in-memory claim read model")
		}
	}
	for _, row := range snapshot.QualitySamples {
		builder.addCandidate(proofTimelineQualityCandidate(row))
	}
	for _, row := range snapshot.QualityOutcomes {
		builder.addCandidate(proofTimelineOutcomeCandidate(row))
	}
	for _, row := range snapshot.TokenImpacts {
		builder.addCandidate(proofTimelineTokenImpactCandidate(row))
	}
	sort.SliceStable(builder.rows, func(i, j int) bool {
		left := anyToInt64(builder.rows[i]["_ordered_unix"], 0)
		right := anyToInt64(builder.rows[j]["_ordered_unix"], 0)
		if left != right {
			return left < right
		}
		leftKey := anyToString(builder.rows[i]["source"]) + "\x00" + anyToString(builder.rows[i]["source_id"])
		rightKey := anyToString(builder.rows[j]["source"]) + "\x00" + anyToString(builder.rows[j]["source_id"])
		return leftKey < rightKey
	})
	var omitted int
	builder.rows, omitted = proofTimelineBoundRows(builder.rows)
	if omitted > 0 {
		builder.projectionOmitted = omitted
		builder.addGap("projection_truncated", "projection", "", fmt.Sprintf("the deterministic read model omitted %d oldest rows to honor row and byte limits", omitted))
	}
	timeline := make([]any, 0, len(builder.rows))
	for _, row := range builder.rows {
		delete(row, "_ordered_unix")
		timeline = append(timeline, row)
	}
	stages := map[string]any{}
	missingStages := []any{}
	for _, stage := range agentProofTimelineStages {
		count := builder.stageCounts[stage]
		status := "present"
		if count == 0 {
			status = "missing"
			missingStages = append(missingStages, stage)
			builder.addGap("stage_missing", "timeline", stage, "required proof stage has no exact-linked evidence")
		}
		stages[stage] = map[string]any{"status": status, "count": count}
	}
	gaps := proofTimelineSortedGaps(builder.gaps)
	status := "verified"
	if len(gaps) > 0 {
		status = "degraded"
	}
	for _, raw := range gaps {
		if anyToString(anyMap(raw)["code"]) == "corrupt_rows" {
			status = "failed"
			break
		}
	}
	complete := len(gaps) == 0 && len(missingStages) == 0
	anchorsStable := proofTimelineDigest(snapshot.SourceAnchorsBefore) == proofTimelineDigest(snapshot.SourceAnchorsAfter)
	objectiveCompactionBefore := builder.displayCompacted
	objective := proofTimelineRedactString(anyToString(snapshot.Session["objective"]), &builder.redactionCount, &builder.displayCompacted)
	sessionSummary := map[string]any{
		"id": scope.SessionID, "project": scope.Project, "agent": anyToString(snapshot.Session["agent"]),
		"agent_id": anyToString(snapshot.Session["agent_id"]), "status": anyToString(snapshot.Session["status"]),
		"task_id": scope.TaskID, "task_identity_id": scope.TaskIdentityID,
		"objective":            objective,
		"event_count":          anyToInt(snapshot.Session["event_count"], len(snapshot.Events)),
		"retained_event_count": len(snapshot.Events),
	}
	if builder.displayCompacted > objectiveCompactionBefore {
		sessionSummary["objective_compacted"] = true
	}
	redactedAnchorsBefore := proofTimelineRedact(snapshot.SourceAnchorsBefore, 0, &builder.redactionCount, &builder.displayCompacted)
	redactedAnchorsAfter := proofTimelineRedact(snapshot.SourceAnchorsAfter, 0, &builder.redactionCount, &builder.displayCompacted)
	payload := map[string]any{
		"ok":        status != "failed",
		"schema_id": agentProofTimelineContractID,
		"version":   1,
		"session":   sessionSummary,
		"integrity": map[string]any{
			"status": status, "complete": complete, "deterministic_ordering": true,
			"exact_identity_joins": true, "timestamp_inference_used": false,
			"ordering_uses_recorded_at": builder.recordedClockCount > 0,
			"explicit_gaps":             true, "source_anchors_stable": anchorsStable,
			"authoritative_ledger_mutations": 0, "provider_calls": 0, "external_network_calls": 0,
		},
		"source_anchors": map[string]any{
			"before": redactedAnchorsBefore,
			"after":  redactedAnchorsAfter,
			"stable": anchorsStable,
		},
		"stage_order":    agentProofTimelineStages,
		"stages":         stages,
		"missing_stages": missingStages,
		"timeline":       timeline,
		"gaps":           gaps,
		"metrics": map[string]any{
			"source_row_count": builder.eligibleRows, "joined_row_count": builder.joinedRows,
			"emitted_row_count": len(timeline), "eligible_exact_link_coverage": proofTimelineRatio(builder.joinedRows, builder.eligibleRows),
			"event_link_fidelity": proofTimelineRatio(builder.joinedRows, builder.eligibleRows), "ordering_fidelity": 1,
			"duplicate_count": builder.duplicateCount, "conflict_count": builder.conflictCount,
			"cross_scope_rejected": builder.crossScopeRejected, "missing_join_key_count": builder.missingJoinKeys,
			"invalid_clock_count": builder.invalidClockCount, "redaction_count": builder.redactionCount,
			"display_compacted_count": builder.displayCompacted,
			"missing_stage_count":     len(missingStages), "silent_gap_count": 0,
			"evidence_compacted_count": builder.evidenceCompacted, "projection_omitted_count": builder.projectionOmitted,
			"source_counts": builder.sourceCounts, "projection_ms": roundFloat(float64(time.Since(started).Microseconds())/1000, 3),
		},
		"rollback": map[string]any{
			"env": agentProofTimelineFeatureEnv + "=false", "fallback_schema": agentRunTraceContractID,
			"fallback_route": "/v1/agents/sessions/{session_id}/trace", "authoritative_ledgers_unchanged": true,
		},
		"limits": map[string]any{
			"timeline_rows": maxAgentProofTimelineRows, "timeline_json_bytes": maxAgentProofTimelineRowsBytes,
			"evidence_json_bytes": maxAgentProofTimelineEvidenceBytes, "projection": "bounded_read_only", "raw_prompt_persisted": false,
		},
	}
	return attachPayloadFormatContract(agentProofTimelineContractID, payload, anyToString(snapshot.Session["agent_id"]), "agent_proof_timeline", "/v1/agents/sessions/{session_id}/proof-timeline")
}

func missingAgentProofTimeline(sessionID string, now time.Time) map[string]any {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	payload := map[string]any{
		"ok": false, "schema_id": agentProofTimelineContractID, "version": 1,
		"session": map[string]any{"id": clipText(strings.TrimSpace(sessionID), maxAgentSessionIDLength), "event_count": 0, "retained_event_count": 0},
		"integrity": map[string]any{
			"status": "unavailable", "complete": false, "deterministic_ordering": true,
			"exact_identity_joins": true, "timestamp_inference_used": false, "explicit_gaps": true,
			"source_anchors_stable": true, "authoritative_ledger_mutations": 0, "provider_calls": 0, "external_network_calls": 0,
		},
		"source_anchors": map[string]any{"before": map[string]any{}, "after": map[string]any{}, "stable": true},
		"stage_order":    agentProofTimelineStages, "stages": map[string]any{}, "missing_stages": []any{}, "timeline": []any{},
		"gaps": []any{map[string]any{"code": "session_missing", "source": "agent_session", "material": true, "detail": "agent session was not found"}},
		"metrics": map[string]any{
			"source_row_count": 0, "joined_row_count": 0, "emitted_row_count": 0, "eligible_exact_link_coverage": 1,
			"event_link_fidelity": 1, "ordering_fidelity": 1, "duplicate_count": 0, "conflict_count": 0,
			"cross_scope_rejected": 0, "missing_join_key_count": 0, "invalid_clock_count": 0,
			"redaction_count": 0, "display_compacted_count": 0, "missing_stage_count": 0, "silent_gap_count": 0, "projection_ms": 0,
		},
		"rollback": map[string]any{"env": agentProofTimelineFeatureEnv + "=false", "fallback_schema": agentRunTraceContractID, "fallback_route": "/v1/agents/sessions/{session_id}/trace", "authoritative_ledgers_unchanged": true},
		"limits":   map[string]any{"timeline_rows": maxAgentProofTimelineRows, "projection": "bounded_read_only", "raw_prompt_persisted": false},
	}
	return attachPayloadFormatContract(agentProofTimelineContractID, payload, "", "agent_proof_timeline", "/v1/agents/sessions/{session_id}/proof-timeline")
}

func proofTimelineSessionAnchor(session map[string]any, events []map[string]any) map[string]any {
	lastEventID := ""
	if len(events) > 0 {
		lastEventID = anyToString(events[len(events)-1]["id"])
	}
	anchor := map[string]any{
		"session_id": anyToString(session["id"]), "event_count": anyToInt(session["event_count"], len(events)),
		"retained_event_count": len(events), "updated_at": anyToString(session["updated_at"]), "last_event_id": lastEventID,
	}
	anchor["digest"] = proofTimelineDigest(anchor)
	return anchor
}

func proofTimelineIdentityIndexKey(kind string, value string, project string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	project = strings.ToLower(strings.TrimSpace(project))
	if project == "" {
		return kind + ":" + value
	}
	return "project:" + project + "\x00" + kind + ":" + value
}

func proofTimelineIdentityIndexKeys(identity map[string]any) []string {
	project := anyToString(identity["project"])
	keys := make([]string, 0, 4)
	for _, item := range []struct {
		kind string
		key  string
	}{
		{kind: "session", key: "session_id"},
		{kind: "task_identity", key: "task_identity_id"},
		{kind: "task", key: "task_id"},
		{kind: "sample", key: "sample_id"},
	} {
		if key := proofTimelineIdentityIndexKey(item.kind, anyToString(identity[item.key]), project); key != "" {
			keys = append(keys, key)
		}
	}
	return keys
}

func proofTimelineScopeIndexKeys(scope proofTimelineScope, includeSamples bool) []string {
	keys := []string{}
	for _, item := range []struct {
		kind  string
		value string
	}{
		{kind: "session", value: scope.SessionID},
		{kind: "task_identity", value: scope.TaskIdentityID},
		{kind: "task", value: scope.TaskID},
	} {
		if key := proofTimelineIdentityIndexKey(item.kind, item.value, scope.Project); key != "" {
			keys = append(keys, key)
		}
		if key := proofTimelineIdentityIndexKey(item.kind, item.value, ""); key != "" {
			keys = append(keys, key)
		}
	}
	if includeSamples {
		sampleIDs := make([]string, 0, len(scope.SampleIDs))
		for sampleID := range scope.SampleIDs {
			sampleIDs = append(sampleIDs, sampleID)
		}
		sort.Strings(sampleIDs)
		for _, sampleID := range sampleIDs {
			if key := proofTimelineIdentityIndexKey("sample", sampleID, scope.Project); key != "" {
				keys = append(keys, key)
			}
			if key := proofTimelineIdentityIndexKey("sample", sampleID, ""); key != "" {
				keys = append(keys, key)
			}
		}
	}
	return keys
}

func proofTimelineIdentityWithinScope(identity map[string]any, scope proofTimelineScope) bool {
	project := strings.TrimSpace(anyToString(identity["project"]))
	if project != "" && scope.Project != "" && !strings.EqualFold(project, scope.Project) {
		return false
	}
	return proofTimelineIdentityRelevant(identity, scope)
}

func (s *continuityStore) indexProofTimelineEntryLocked(entry continuityLedgerEntry, index int) {
	if s == nil || index < 0 {
		return
	}
	if s.proofIdentityIndex == nil {
		s.proofIdentityIndex = map[string][]int{}
	}
	identity := proofTimelineContinuityCandidate(entry).Links
	for _, key := range proofTimelineIdentityIndexKeys(identity) {
		s.proofIdentityIndex[key] = append(s.proofIdentityIndex[key], index)
	}
}

func (s *continuityStore) proofTimelineEntryIndexesLocked(scope proofTimelineScope) ([]int, int, int) {
	if len(s.entries) == 0 {
		return nil, 0, 0
	}
	if len(s.proofIdentityIndex) == 0 {
		return nil, 1, 0
	}
	candidates := map[int]struct{}{}
	omitted := 0
	scanned := 0
	for _, key := range proofTimelineScopeIndexKeys(scope, false) {
		indexes := s.proofIdentityIndex[key]
		start := maxInt(0, len(indexes)-maxAgentProofTimelineSourceScans)
		if start > 0 {
			omitted = maxInt(omitted, 1)
		}
		for position := len(indexes) - 1; position >= start; position-- {
			scanned++
			index := indexes[position]
			if index < 0 || index >= len(s.entries) {
				continue
			}
			identity := proofTimelineContinuityCandidate(s.entries[index]).Links
			if proofTimelineIdentityWithinScope(identity, scope) {
				candidates[index] = struct{}{}
			}
		}
	}
	indexes := make([]int, 0, len(candidates))
	for index := range candidates {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	if len(indexes) > maxAgentProofTimelineSourceRows {
		omitted += len(indexes) - maxAgentProofTimelineSourceRows
		indexes = indexes[len(indexes)-maxAgentProofTimelineSourceRows:]
	}
	return indexes, omitted, scanned
}

func (s *continuityStore) proofTimelineAnchorForIndexesLocked(indexes []int, omitted int) map[string]any {
	available := s.enabled && strings.TrimSpace(s.lastError) == ""
	refs := make([]any, 0, len(indexes))
	for _, index := range indexes {
		if index < 0 || index >= len(s.entries) {
			continue
		}
		entry := s.entries[index]
		refs = append(refs, map[string]any{
			"sequence": entry.Sequence, "entry_id": entry.EntryID, "entry_hash": entry.EntryHash,
		})
	}
	anchor := map[string]any{
		"available": available, "selected_count": len(refs), "omitted_count": omitted,
		"rows_digest": proofTimelineDigest(refs),
	}
	anchor["digest"] = proofTimelineDigest(anchor)
	return anchor
}

func (s *continuityStore) proofTimelineAnchorLocked(scope proofTimelineScope) map[string]any {
	indexes, omitted, _ := s.proofTimelineEntryIndexesLocked(scope)
	return s.proofTimelineAnchorForIndexesLocked(indexes, omitted)
}

func (s *continuityStore) proofTimelineAnchor(scope proofTimelineScope) map[string]any {
	if s == nil {
		return map[string]any{"available": false}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.proofTimelineAnchorLocked(scope)
}

func (s *continuityStore) proofTimelineRows(scope proofTimelineScope) ([]continuityLedgerEntry, map[string]any, bool, bool, int) {
	if s == nil {
		return nil, map[string]any{"available": false}, false, false, 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	available := s.enabled && strings.TrimSpace(s.lastError) == ""
	valid := available
	indexes, omitted, _ := s.proofTimelineEntryIndexesLocked(scope)
	rows := []continuityLedgerEntry{}
	for _, index := range indexes {
		entry := s.entries[index]
		expectedPrevious := ""
		if index > 0 {
			expectedPrevious = s.entries[index-1].EntryHash
		}
		if entry.Sequence != uint64(index+1) || entry.PreviousHash != expectedPrevious {
			valid = false
		}
		expected, err := continuityEntryHash(entry)
		if err != nil || expected != entry.EntryHash {
			valid = false
		}
		entry.Payload = cloneAnyMap(entry.Payload)
		rows = append(rows, entry)
	}
	if len(s.entries) > 0 && s.lastHash != s.entries[len(s.entries)-1].EntryHash {
		valid = false
	}
	anchor := s.proofTimelineAnchorForIndexesLocked(indexes, omitted)
	return rows, anchor, valid, available, omitted
}

func (s *temporalClaimStore) proofTimelineClaimsLocked(scope proofTimelineScope) ([]temporalClaim, int, int) {
	refs := s.proofSessionIndex[temporalClaimProofIndexKey(scope.Project, scope.SessionID)]
	start := maxInt(0, len(refs)-maxAgentProofTimelineSourceScans)
	omitted := start
	seen := map[string]struct{}{}
	rows := make([]temporalClaim, 0, minInt(len(refs), maxAgentProofTimelineSourceRows))
	scanned := 0
	for index := len(refs) - 1; index >= start; index-- {
		scanned++
		claimID := refs[index]
		if _, exists := seen[claimID]; exists {
			continue
		}
		seen[claimID] = struct{}{}
		claim, exists := s.claims[claimID]
		if !exists || claim.SessionID != scope.SessionID || (scope.Project != "" && !strings.EqualFold(claim.Project, scope.Project)) {
			continue
		}
		if len(rows) >= maxAgentProofTimelineSourceRows {
			omitted = maxInt(omitted, 1)
			continue
		}
		rows = append(rows, claim)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].UpdatedAt != rows[j].UpdatedAt {
			return rows[i].UpdatedAt < rows[j].UpdatedAt
		}
		return rows[i].ClaimID < rows[j].ClaimID
	})
	return rows, omitted, scanned
}

func (s *temporalClaimStore) proofTimelineAnchorForRowsLocked(rows []temporalClaim, omitted int) map[string]any {
	refs := make([]any, 0, len(rows))
	for _, claim := range rows {
		refs = append(refs, map[string]any{
			"claim_id": claim.ClaimID, "revision": claim.Revision, "updated_at": claim.UpdatedAt,
		})
	}
	anchor := map[string]any{
		"available": s.enabled && strings.TrimSpace(s.lastError) == "", "selected_count": len(rows),
		"omitted_count": omitted, "rows_digest": proofTimelineDigest(refs),
	}
	anchor["digest"] = proofTimelineDigest(anchor)
	return anchor
}

func (s *temporalClaimStore) proofTimelineAnchorLocked(scope proofTimelineScope) map[string]any {
	rows, omitted, _ := s.proofTimelineClaimsLocked(scope)
	return s.proofTimelineAnchorForRowsLocked(rows, omitted)
}

func (s *temporalClaimStore) proofTimelineAnchor(scope proofTimelineScope) map[string]any {
	if s == nil {
		return map[string]any{"available": false}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.proofTimelineAnchorLocked(scope)
}

func (s *temporalClaimStore) proofTimelineRows(scope proofTimelineScope) ([]temporalClaim, map[string]any, bool, int) {
	if s == nil {
		return nil, map[string]any{"available": false}, false, 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	available := s.enabled && strings.TrimSpace(s.lastError) == ""
	rows, omitted, _ := s.proofTimelineClaimsLocked(scope)
	anchor := s.proofTimelineAnchorForRowsLocked(rows, omitted)
	return rows, anchor, available, omitted
}

func proofTimelineMapRowsLocked(source []map[string]any, scope proofTimelineScope) ([]map[string]any, int, int) {
	start := maxInt(0, len(source)-maxAgentProofTimelineSourceScans)
	omitted := start
	rows := make([]map[string]any, 0, minInt(len(source), maxAgentProofTimelineSourceRows))
	scanned := 0
	for index := len(source) - 1; index >= start; index-- {
		scanned++
		row := source[index]
		if !proofTimelineIdentityWithinScope(proofTimelineIdentityFromMaps(row), scope) {
			continue
		}
		if len(rows) >= maxAgentProofTimelineSourceRows {
			omitted++
			continue
		}
		rows = append(rows, row)
	}
	for left, right := 0, len(rows)-1; left < right; left, right = left+1, right-1 {
		rows[left], rows[right] = rows[right], rows[left]
	}
	return rows, omitted, scanned
}

func proofTimelineRingSource(ring *proofTimelineMapRing, fallback []map[string]any, total int64, scope proofTimelineScope) ([]map[string]any, int) {
	if ring != nil && len(ring.rows) > 0 {
		return ring.ordered(), ring.droppedForScope(scope)
	}
	omitted := int64(0)
	if total > int64(len(fallback)) {
		omitted = total - int64(len(fallback))
	}
	return fallback, int(omitted)
}

func proofTimelineMapRowsAnchor(rows []map[string]any) string {
	refs := make([]any, 0, len(rows))
	for _, row := range rows {
		refs = append(refs, proofTimelineDigest(row))
	}
	return proofTimelineDigest(refs)
}

func (t *contextPackQualityTelemetry) proofTimelineAnchorForRowsLocked(samples []map[string]any, outcomes []map[string]any, omitted int) map[string]any {
	anchor := map[string]any{
		"available": true, "sample_count": len(samples), "outcome_count": len(outcomes),
		"omitted_count":      omitted,
		"sample_rows_digest": proofTimelineMapRowsAnchor(samples), "outcome_rows_digest": proofTimelineMapRowsAnchor(outcomes),
	}
	anchor["digest"] = proofTimelineDigest(anchor)
	return anchor
}

func (t *contextPackQualityTelemetry) proofTimelineAnchorLocked(scope proofTimelineScope) map[string]any {
	sampleSource, sampleRetentionOmitted := proofTimelineRingSource(&t.proofSamples, t.samples, t.sampleCount, scope)
	outcomeSource, outcomeRetentionOmitted := proofTimelineRingSource(&t.proofOutcomes, t.outcomes, t.outcomeCount, scope)
	samples, sampleOmitted, sampleScanned := proofTimelineMapRowsLocked(sampleSource, scope)
	outcomes, outcomeOmitted, outcomeScanned := proofTimelineMapRowsLocked(outcomeSource, scope)
	_, _ = sampleScanned, outcomeScanned
	return t.proofTimelineAnchorForRowsLocked(samples, outcomes, sampleOmitted+outcomeOmitted+sampleRetentionOmitted+outcomeRetentionOmitted)
}

func (t *contextPackQualityTelemetry) proofTimelineAnchor(scope proofTimelineScope) map[string]any {
	if t == nil {
		return map[string]any{"available": false}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.proofTimelineAnchorLocked(scope)
}

func (t *contextPackQualityTelemetry) proofTimelineRows(scope proofTimelineScope) ([]map[string]any, []map[string]any, map[string]any, bool, int) {
	if t == nil {
		return nil, nil, map[string]any{"available": false}, false, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	sampleSource, sampleRetentionOmitted := proofTimelineRingSource(&t.proofSamples, t.samples, t.sampleCount, scope)
	outcomeSource, outcomeRetentionOmitted := proofTimelineRingSource(&t.proofOutcomes, t.outcomes, t.outcomeCount, scope)
	sampleRefs, sampleOmitted, sampleScanned := proofTimelineMapRowsLocked(sampleSource, scope)
	outcomeRefs, outcomeOmitted, outcomeScanned := proofTimelineMapRowsLocked(outcomeSource, scope)
	omitted := sampleOmitted + outcomeOmitted + sampleRetentionOmitted + outcomeRetentionOmitted
	_, _ = sampleScanned, outcomeScanned
	anchor := t.proofTimelineAnchorForRowsLocked(sampleRefs, outcomeRefs, omitted)
	samples := cloneMapSlice(sampleRefs)
	outcomes := cloneMapSlice(outcomeRefs)
	return samples, outcomes, anchor, true, omitted
}

func (t *tokenImpactTelemetry) proofTimelineAnchorForRowsLocked(rows []map[string]any, omitted int) map[string]any {
	anchor := map[string]any{
		"available": true, "sample_count": len(rows), "omitted_count": omitted,
		"rows_digest": proofTimelineMapRowsAnchor(rows),
	}
	anchor["digest"] = proofTimelineDigest(anchor)
	return anchor
}

func (t *tokenImpactTelemetry) proofTimelineAnchorLocked(scope proofTimelineScope) map[string]any {
	source, retentionOmitted := proofTimelineRingSource(&t.proofSamples, t.samples, t.sampleCount, scope)
	rows, omitted, scanned := proofTimelineMapRowsLocked(source, scope)
	_ = scanned
	return t.proofTimelineAnchorForRowsLocked(rows, omitted+retentionOmitted)
}

func (t *tokenImpactTelemetry) proofTimelineAnchor(scope proofTimelineScope) map[string]any {
	if t == nil {
		return map[string]any{"available": false}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.proofTimelineAnchorLocked(scope)
}

func (t *tokenImpactTelemetry) proofTimelineRows(scope proofTimelineScope) ([]map[string]any, map[string]any, bool, int) {
	if t == nil {
		return nil, map[string]any{"available": false}, false, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	source, retentionOmitted := proofTimelineRingSource(&t.proofSamples, t.samples, t.sampleCount, scope)
	refs, omitted, scanned := proofTimelineMapRowsLocked(source, scope)
	_ = scanned
	omitted += retentionOmitted
	anchor := t.proofTimelineAnchorForRowsLocked(refs, omitted)
	return cloneMapSlice(refs), anchor, true, omitted
}

func (s *server) proofTimelineAnchors(scope proofTimelineScope) map[string]any {
	anchors := map[string]any{}
	if s == nil {
		return anchors
	}
	if session, events, ok := s.agentSessions.get(scope.SessionID); ok {
		anchors["agent_session"] = proofTimelineSessionAnchor(session, events)
	} else {
		anchors["agent_session"] = map[string]any{"available": false}
	}
	anchors["continuity"] = s.continuity.proofTimelineAnchor(scope)
	anchors["temporal_claim"] = s.temporalClaims.proofTimelineAnchor(scope)
	anchors["context_pack_quality"] = s.contextPackQuality.proofTimelineAnchor(scope)
	anchors["token_impact"] = s.tokenImpact.proofTimelineAnchor(scope)
	return anchors
}

func (s *server) captureAgentProofTimelineSnapshot(session map[string]any, events []map[string]any) agentProofTimelineSnapshot {
	scope := proofTimelineScopeFromSession(session, events)
	continuityRows, continuityAnchor, continuityIntegrity, continuityAvailable, continuityOmitted := s.continuity.proofTimelineRows(scope)
	claims, claimAnchor, claimsAvailable, claimOmitted := s.temporalClaims.proofTimelineRows(scope)
	qualitySamples, qualityOutcomes, qualityAnchor, qualityAvailable, qualityOmitted := s.contextPackQuality.proofTimelineRows(scope)
	tokenRows, tokenAnchor, tokenAvailable, tokenOmitted := s.tokenImpact.proofTimelineRows(scope)
	before := map[string]any{
		"agent_session":        proofTimelineSessionAnchor(session, events),
		"continuity":           continuityAnchor,
		"temporal_claim":       claimAnchor,
		"context_pack_quality": qualityAnchor,
		"token_impact":         tokenAnchor,
	}
	return agentProofTimelineSnapshot{
		Session: cloneAnyMap(session), Events: cloneMapSlice(events), ContinuityEntries: continuityRows,
		ContinuityIntegrity: continuityIntegrity, Claims: claims,
		QualitySamples: qualitySamples, QualityOutcomes: qualityOutcomes, TokenImpacts: tokenRows,
		Availability: map[string]bool{
			"continuity": continuityAvailable, "temporal_claim": claimsAvailable,
			"context_pack_quality": qualityAvailable, "token_impact": tokenAvailable,
		},
		SourceOmitted: map[string]int{
			"continuity": continuityOmitted, "temporal_claim": claimOmitted,
			"context_pack_quality": qualityOmitted, "token_impact": tokenOmitted,
		},
		SourceAnchorsBefore: before,
		SourceAnchorsAfter:  s.proofTimelineAnchors(scope),
	}
}

func cloneMapSlice(rows []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, cloneAnyMap(row))
	}
	return out
}

func (s *server) agentsSessionProofTimeline(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if !agentProofTimelineEnabled() {
		s.agentsSessionTrace(w, r, sessionID)
		return
	}
	session, events, ok := s.agentSessions.get(sessionID)
	if !ok {
		writeJSON(w, http.StatusNotFound, missingAgentProofTimeline(sessionID, time.Now().UTC()))
		return
	}
	snapshot := s.captureAgentProofTimelineSnapshot(session, events)
	writeJSON(w, http.StatusOK, buildAgentProofTimeline(snapshot, time.Now().UTC()))
}
