package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	defaultAgentSessionsPathRel       = ".data/orchestrator/agent_sessions.json"
	defaultAgentSessionMaxRecords     = 512
	defaultAgentSessionMaxEvents      = 256
	defaultAgentSessionMaxMetadataMap = 48
)

func parseISOTime(raw string) (time.Time, bool) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, text); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

func readOptionalJSONBody(r *http.Request) (map[string]any, error) {
	bodyBytes, err := readRequestBody(r)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(bodyBytes)) == "" {
		return map[string]any{}, nil
	}
	return parseJSONMap(bodyBytes)
}

type agentSessionStore struct {
	mu        sync.Mutex
	path      string
	maxKeep   int
	maxEvents int
	sessions  map[string]map[string]any
	order     []string
	events    map[string][]map[string]any
}

func agentSessionsPath() string {
	path := strings.TrimSpace(os.Getenv("GO_AGENT_SESSIONS_PATH"))
	if path == "" {
		path = strings.TrimSpace(os.Getenv("GO_AGENT_SESSION_LEDGER_PATH"))
	}
	if path == "" {
		path = defaultAgentSessionsPathRel
	}
	return filepath.Clean(path)
}

func newAgentSessionStoreFromEnv() (*agentSessionStore, error) {
	store := &agentSessionStore{
		path:      agentSessionsPath(),
		maxKeep:   maxInt(16, envInt("GO_AGENT_SESSION_MAX_RECORDS", defaultAgentSessionMaxRecords)),
		maxEvents: maxInt(16, envInt("GO_AGENT_SESSION_MAX_EVENTS_PER_SESSION", defaultAgentSessionMaxEvents)),
		sessions:  map[string]map[string]any{},
		order:     []string{},
		events:    map[string][]map[string]any{},
	}
	if err := store.load(); err != nil {
		return store, err
	}
	return store, nil
}

func (s *agentSessionStore) load() error {
	if s == nil {
		return errors.New("agent session store unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(s.path) == "" {
		return nil
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	payload := map[string]any{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	sessionsRaw, _ := payload["sessions"].([]any)
	eventsRaw := anyMap(payload["events"])
	loadedSessions := map[string]map[string]any{}
	order := make([]string, 0, len(sessionsRaw))
	for _, item := range sessionsRaw {
		row := anyMap(item)
		id := strings.TrimSpace(anyToString(row["id"]))
		if id == "" {
			continue
		}
		row["memory_contribution"] = normalizeAgentContribution(anyMap(row["memory_contribution"]))
		loadedSessions[id] = row
		order = append(order, id)
	}
	loadedEvents := map[string][]map[string]any{}
	for sessionID, rawList := range eventsRaw {
		listAny, ok := rawList.([]any)
		if !ok {
			continue
		}
		rows := make([]map[string]any, 0, minInt(len(listAny), s.maxEvents))
		for _, item := range listAny {
			row := anyMap(item)
			if len(row) == 0 {
				continue
			}
			rows = append(rows, row)
		}
		if len(rows) > s.maxEvents {
			rows = rows[len(rows)-s.maxEvents:]
		}
		loadedEvents[sessionID] = rows
	}
	s.sessions = loadedSessions
	s.order = order
	s.events = loadedEvents
	s.enforceBoundsLocked()
	return nil
}

func (s *agentSessionStore) persistLocked() error {
	if s == nil {
		return errors.New("agent session store unavailable")
	}
	if strings.TrimSpace(s.path) == "" {
		return errors.New("agent session store path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	sessions := make([]map[string]any, 0, len(s.order))
	for _, id := range s.order {
		if row, ok := s.sessions[id]; ok {
			sessions = append(sessions, cloneAnyMap(row))
		}
	}
	events := map[string]any{}
	for id, rows := range s.events {
		items := make([]any, 0, len(rows))
		for _, row := range rows {
			items = append(items, cloneAnyMap(row))
		}
		events[id] = items
	}
	raw, err := json.MarshalIndent(map[string]any{
		"updated_at": nowUTCISO(),
		"sessions":   sessions,
		"events":     events,
	}, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomicFile(s.path, raw, 0o644)
}

func (s *agentSessionStore) enforceBoundsLocked() {
	if s == nil {
		return
	}
	if s.maxKeep < 1 {
		s.maxKeep = defaultAgentSessionMaxRecords
	}
	if s.maxEvents < 1 {
		s.maxEvents = defaultAgentSessionMaxEvents
	}
	seen := map[string]struct{}{}
	keptOrder := make([]string, 0, len(s.order))
	for _, id := range s.order {
		if strings.TrimSpace(id) == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		if _, ok := s.sessions[id]; !ok {
			delete(s.events, id)
			continue
		}
		seen[id] = struct{}{}
		keptOrder = append(keptOrder, id)
	}
	if len(keptOrder) > s.maxKeep {
		drop := keptOrder[:len(keptOrder)-s.maxKeep]
		keptOrder = keptOrder[len(keptOrder)-s.maxKeep:]
		for _, id := range drop {
			delete(s.sessions, id)
			delete(s.events, id)
		}
	}
	for id, rows := range s.events {
		if _, ok := s.sessions[id]; !ok {
			delete(s.events, id)
			continue
		}
		if len(rows) > s.maxEvents {
			s.events[id] = rows[len(rows)-s.maxEvents:]
		}
	}
	s.order = keptOrder
}

func normalizeAgentSessionStatus(raw string) string {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "active", "running", "started", "open", "working":
		return "active"
	case "completed", "complete", "succeeded", "success", "done":
		return "completed"
	case "failed", "error":
		return "failed"
	case "blocked":
		return "blocked"
	case "paused", "waiting", "awaiting_user", "needs_user":
		return "paused"
	case "canceled", "cancelled":
		return "canceled"
	default:
		return "active"
	}
}

func agentSessionTerminal(status string) bool {
	switch normalizeAgentSessionStatus(status) {
	case "completed", "failed", "canceled":
		return true
	default:
		return false
	}
}

func normalizeAgentLifecycleState(raw string) string {
	switch strings.TrimSpace(strings.ToLower(strings.ReplaceAll(raw, "-", "_"))) {
	case "idle", "ready", "standby":
		return "idle"
	case "working", "running", "active", "started", "busy":
		return "working"
	case "awaiting_user", "awaiting", "waiting", "paused", "needs_user", "need_user", "approval":
		return "awaiting_user"
	case "blocked", "stuck", "failed", "error":
		return "blocked"
	case "done", "completed", "complete", "succeeded", "success":
		return "done"
	default:
		return "working"
	}
}

func normalizeAgentStateAuthority(raw string) string {
	switch strings.TrimSpace(strings.ToLower(strings.ReplaceAll(raw, "-", "_"))) {
	case "hook", "plugin", "self_report", "process_probe", "manual", "none":
		return strings.TrimSpace(strings.ToLower(strings.ReplaceAll(raw, "-", "_")))
	case "process", "probe", "ps":
		return "process_probe"
	case "self", "agent", "agent_report":
		return "self_report"
	default:
		return "self_report"
	}
}

func agentLifecycleStateFromSessionStatus(status string) string {
	switch normalizeAgentSessionStatus(status) {
	case "completed":
		return "done"
	case "blocked", "failed":
		return "blocked"
	case "paused":
		return "awaiting_user"
	default:
		return "working"
	}
}

func normalizeAgentLifecyclePayload(value any, fallbackStatus string) map[string]any {
	raw := anyMap(value)
	state := normalizeAgentLifecycleState(firstNonEmptyStrings(anyToString(raw["state"]), agentLifecycleStateFromSessionStatus(fallbackStatus)))
	out := map[string]any{
		"schema_id": "contextlattice_agent_lifecycle_state.v1",
		"state":     state,
		"authority": normalizeAgentStateAuthority(anyToString(raw["authority"])),
		"source":    clipText(firstNonEmptyStrings(anyToString(raw["source"]), "session_status"), 96),
	}
	for _, key := range []string{"ttl_seconds", "expires_at", "updated_at", "task_id", "repo", "branch", "worktree", "cwd", "native_session_id", "needs_user", "blocked_by"} {
		if rawValue, ok := raw[key]; ok {
			out[key] = compactAgentSessionValue(rawValue, 2)
		}
	}
	return out
}

func agentSessionOwnership(session map[string]any) map[string]any {
	state := anyMap(session["agent_state"])
	out := map[string]any{}
	for _, key := range []string{"task_id", "repo", "worktree", "branch", "cwd", "native_session_id"} {
		value := firstNonEmptyStrings(anyToString(session[key]), anyToString(state[key]))
		if value != "" {
			out[key] = clipText(value, 360)
		}
	}
	if len(out) > 0 {
		out["ownership_key"] = clipText(strings.Join([]string{
			anyToString(out["repo"]),
			anyToString(out["worktree"]),
			anyToString(out["branch"]),
			anyToString(out["task_id"]),
			anyToString(session["id"]),
		}, "|"), 720)
	}
	return out
}

func agentSessionObjectiveHierarchy(session map[string]any) map[string]any {
	if hierarchy := anyMap(session["objective_hierarchy"]); len(hierarchy) > 0 {
		return hierarchy
	}
	if runtime := anyMap(session["objective_runtime"]); len(runtime) > 0 {
		if hierarchy := anyMap(runtime["objective_hierarchy"]); len(hierarchy) > 0 {
			return hierarchy
		}
	}
	ctx := objectiveContext{
		Mission:   anyToString(session["mission"]),
		Objective: anyToString(session["objective"]),
		Goal:      anyToString(session["goal"]),
	}.withDefaults()
	metadata := anyMap(session["metadata"])
	return ctx.hierarchy(
		anyToString(session["project"]),
		anyToString(metadata["topic_path"]),
		anyToString(session["id"]),
		anyToString(session["objective"]),
	)
}

func agentSessionObjectiveLineage(session map[string]any) map[string]any {
	if lineage := anyMap(session["objective_lineage"]); len(lineage) > 0 {
		return lineage
	}
	if runtime := anyMap(session["objective_runtime"]); len(runtime) > 0 {
		if lineage := anyMap(runtime["objective_lineage"]); len(lineage) > 0 {
			return lineage
		}
	}
	ctx := objectiveContext{
		Mission:   anyToString(session["mission"]),
		Objective: anyToString(session["objective"]),
		Goal:      anyToString(session["goal"]),
	}.withDefaults()
	metadata := anyMap(session["metadata"])
	return ctx.lineage(
		anyToString(session["project"]),
		anyToString(metadata["topic_path"]),
		anyToString(session["id"]),
		anyToString(session["objective"]),
	)
}

func normalizeAgentEventType(raw string) string {
	text := strings.TrimSpace(strings.ToLower(raw))
	text = strings.ReplaceAll(text, " ", "_")
	text = strings.ReplaceAll(text, "-", "_")
	text = strings.Trim(text, "._:")
	if text == "" {
		return "agent.event"
	}
	var b strings.Builder
	for _, ch := range text {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '.' || ch == ':' {
			b.WriteRune(ch)
		}
	}
	normalized := strings.Trim(b.String(), "._:")
	if normalized == "" {
		return "agent.event"
	}
	return clipText(normalized, 96)
}

func normalizeAgentSessionTags(value any) []any {
	tags := anyToStringList(value, 16)
	out := make([]any, 0, len(tags))
	seen := map[string]struct{}{}
	for _, tag := range tags {
		tag = clipText(strings.TrimSpace(tag), 64)
		if tag == "" {
			continue
		}
		if _, exists := seen[strings.ToLower(tag)]; exists {
			continue
		}
		seen[strings.ToLower(tag)] = struct{}{}
		out = append(out, tag)
	}
	return out
}

func compactAgentSessionValue(value any, depth int) any {
	if depth <= 0 {
		return clipText(anyToString(value), 240)
	}
	switch typed := value.(type) {
	case map[string]any:
		out := map[string]any{}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if len(keys) > defaultAgentSessionMaxMetadataMap {
			keys = keys[:defaultAgentSessionMaxMetadataMap]
		}
		for _, key := range keys {
			cleanKey := clipText(strings.TrimSpace(key), 96)
			if cleanKey == "" {
				continue
			}
			out[cleanKey] = compactAgentSessionValue(typed[key], depth-1)
		}
		return out
	case []any:
		limit := minInt(len(typed), 24)
		out := make([]any, 0, limit)
		for _, item := range typed[:limit] {
			out = append(out, compactAgentSessionValue(item, depth-1))
		}
		return out
	case []string:
		limit := minInt(len(typed), 24)
		out := make([]any, 0, limit)
		for _, item := range typed[:limit] {
			out = append(out, clipText(item, 360))
		}
		return out
	case string:
		return clipText(typed, 720)
	case bool:
		return typed
	case int, int64, int32, uint, uint64, uint32, float64, float32, json.Number:
		return value
	default:
		if rendered := strings.TrimSpace(anyToString(value)); rendered != "" {
			return clipText(rendered, 360)
		}
		return nil
	}
}

func compactAgentSessionMetadata(value map[string]any) map[string]any {
	compacted, _ := compactAgentSessionValue(value, 4).(map[string]any)
	if compacted == nil {
		return map[string]any{}
	}
	return compacted
}

func normalizeAgentContribution(input map[string]any) map[string]any {
	out := map[string]any{
		"score":               clampInt(anyToInt(input["score"], 0), 0, 100),
		"context_packs":       maxInt(0, anyToInt(input["context_packs"], 0)),
		"memory_hits":         maxInt(0, anyToInt(input["memory_hits"], 0)),
		"graph_touches":       maxInt(0, anyToInt(input["graph_touches"], 0)),
		"dream_outputs":       maxInt(0, anyToInt(input["dream_outputs"], 0)),
		"decisions":           maxInt(0, anyToInt(input["decisions"], 0)),
		"tests":               maxInt(0, anyToInt(input["tests"], 0)),
		"handoffs":            maxInt(0, anyToInt(input["handoffs"], 0)),
		"writebacks":          maxInt(0, anyToInt(input["writebacks"], 0)),
		"risks":               maxInt(0, anyToInt(input["risks"], 0)),
		"prs":                 maxInt(0, anyToInt(input["prs"], 0)),
		"skills_or_rules":     maxInt(0, anyToInt(input["skills_or_rules"], 0)),
		"last_contribution":   anyToString(input["last_contribution"]),
		"last_contributed_at": anyToString(input["last_contributed_at"]),
	}
	return out
}

func agentSessionListLimit[T any](items []T, limit int) []T {
	if limit < 0 {
		limit = 0
	}
	if len(items) <= limit {
		return items
	}
	return items[:limit]
}

func sortedStringSet(values map[string]struct{}, limit int) []any {
	items := make([]string, 0, len(values))
	for value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			items = append(items, value)
		}
	}
	sort.Strings(items)
	items = agentSessionListLimit(items, limit)
	out := make([]any, 0, len(items))
	for _, item := range items {
		out = append(out, item)
	}
	return out
}

func addAgentSessionStrings(set map[string]struct{}, value any, limit int) {
	if len(set) >= limit {
		return
	}
	switch typed := value.(type) {
	case string:
		if text := strings.TrimSpace(typed); text != "" {
			set[clipText(text, 96)] = struct{}{}
		}
	case []any:
		for _, item := range typed {
			addAgentSessionStrings(set, item, limit)
			if len(set) >= limit {
				return
			}
		}
	case []string:
		for _, item := range typed {
			addAgentSessionStrings(set, item, limit)
			if len(set) >= limit {
				return
			}
		}
	}
}

func addAgentSessionSourceCoverage(returned map[string]struct{}, pending map[string]struct{}, failed map[string]struct{}, value any, limit int) {
	coverage := anyMap(value)
	for _, key := range []string{
		"returned",
		"returned_now",
		"sources",
	} {
		addAgentSessionStrings(returned, coverage[key], limit)
	}
	for _, key := range []string{
		"pending",
		"pending_sources",
		"warming",
		"deferred",
	} {
		addAgentSessionStrings(pending, coverage[key], limit)
	}
	for _, key := range []string{
		"failed",
		"failed_sources",
		"timed_out",
		"budget_exceeded",
	} {
		addAgentSessionStrings(failed, coverage[key], limit)
	}
}

func bumpContribution(contribution map[string]any, eventType string, metadata map[string]any, at string) map[string]any {
	out := normalizeAgentContribution(contribution)
	kind := "event"
	lower := strings.ToLower(eventType + " " + strings.Join(anyToStringList(metadata["tags"], 12), " "))
	add := func(key string, points int, label string) {
		out[key] = maxInt(0, anyToInt(out[key], 0)) + 1
		out["score"] = clampInt(anyToInt(out["score"], 0)+points, 0, 100)
		out["last_contribution"] = label
		out["last_contributed_at"] = at
		kind = label
	}
	switch {
	case strings.Contains(lower, "context_pack") || strings.Contains(lower, "context.request") || strings.Contains(lower, "retrieval"):
		add("context_packs", 14, "context_pack")
		if hits := anyToInt(metadata["memory_hits"], -1); hits >= 0 {
			out["memory_hits"] = maxInt(anyToInt(out["memory_hits"], 0), hits)
		}
	case strings.Contains(lower, "graph") || strings.Contains(lower, "edge") || strings.Contains(lower, "neighbor"):
		add("graph_touches", 18, "graph")
	case strings.Contains(lower, "dream"):
		add("dream_outputs", 20, "dream")
	case strings.Contains(lower, "decision"):
		add("decisions", 10, "decision")
	case strings.Contains(lower, "test") || strings.Contains(lower, "verify") || strings.Contains(lower, "check.ran"):
		add("tests", 10, "test")
	case strings.Contains(lower, "handoff") || strings.Contains(lower, "compact"):
		add("handoffs", 10, "handoff")
	case strings.Contains(lower, "writeback") || strings.Contains(lower, "checkpoint"):
		add("writebacks", 10, "writeback")
	case strings.Contains(lower, "risk") || strings.Contains(lower, "blocker") || strings.Contains(lower, "fail"):
		add("risks", 5, "risk")
	case strings.Contains(lower, "pr") || strings.Contains(lower, "pull_request") || strings.Contains(lower, "merge"):
		add("prs", 8, "pr")
	case strings.Contains(lower, "skill") || strings.Contains(lower, "rule") || strings.Contains(lower, "policy"):
		add("skills_or_rules", 8, "skills_or_rules")
	}
	if kind == "event" && strings.TrimSpace(anyToString(out["last_contributed_at"])) == "" {
		out["last_contributed_at"] = at
	}
	return out
}

func agentSessionPhase(eventType string) string {
	lower := strings.ToLower(strings.TrimSpace(eventType))
	switch {
	case lower == "session.started" || strings.Contains(lower, "preflight") || strings.Contains(lower, "bootstrap"):
		return "bootstrap"
	case strings.Contains(lower, "context_pack") || strings.Contains(lower, "context.package") || strings.Contains(lower, "context_package") || strings.Contains(lower, "retrieval"):
		return "context"
	case strings.Contains(lower, "writeback") || strings.Contains(lower, "checkpoint"):
		return "memory_write"
	case strings.Contains(lower, "graph") || strings.Contains(lower, "edge") || strings.Contains(lower, "neighbor"):
		return "graph"
	case strings.Contains(lower, "dream"):
		return "dream"
	case strings.Contains(lower, "decision"):
		return "decision"
	case strings.Contains(lower, "test") || strings.Contains(lower, "verify") || strings.Contains(lower, "check"):
		return "verification"
	case strings.Contains(lower, "handoff") || strings.Contains(lower, "compact"):
		return "handoff"
	case strings.Contains(lower, "complete"):
		return "completion"
	case strings.Contains(lower, "fail") || strings.Contains(lower, "block") || strings.Contains(lower, "risk"):
		return "risk"
	default:
		return "event"
	}
}

func compactAgentSessionRecentEvent(event map[string]any) map[string]any {
	metadata := anyMap(event["metadata"])
	out := map[string]any{
		"id":         clipText(anyToString(event["id"]), 128),
		"type":       clipText(anyToString(event["type"]), 96),
		"phase":      agentSessionPhase(anyToString(event["type"])),
		"summary":    clipText(anyToString(event["summary"]), 240),
		"status":     normalizeAgentSessionStatus(anyToString(event["status"])),
		"created_at": anyToString(event["created_at"]),
		"memory_hits": func() int {
			return anyToInt(metadata["memory_hits"], -1)
		}(),
		"result_count": func() int {
			return anyToInt(metadata["result_count"], -1)
		}(),
	}
	if state := anyMap(metadata["agent_state"]); len(state) > 0 {
		out["agent_state"] = normalizeAgentLifecyclePayload(state, anyToString(event["status"]))
	}
	if ownership := anyMap(metadata["ownership"]); len(ownership) > 0 {
		out["ownership"] = compactAgentSessionMetadata(ownership)
	}
	return out
}

func agentSessionSteeringInbox(sessionID string, events []map[string]any) map[string]any {
	items := []any{}
	for i := len(events) - 1; i >= 0 && len(items) < 8; i-- {
		event := events[i]
		metadata := anyMap(event["metadata"])
		comment := anyMap(metadata["steering_comment"])
		if len(comment) == 0 {
			continue
		}
		progress := anyMap(comment["retrieval_progress"])
		progressModel := anyMap(progress["modeled_progress"])
		sourceSummary := anyMap(progress["source_summary"])
		items = append(items, map[string]any{
			"event_id":         clipText(anyToString(event["id"]), 128),
			"type":             clipText(anyToString(event["type"]), 96),
			"created_at":       anyToString(event["created_at"]),
			"severity":         clipText(anyToString(comment["severity"]), 40),
			"message":          clipText(anyToString(comment["message"]), 720),
			"suggested_action": clipText(anyToString(comment["suggested_action"]), 720),
			"token":            clipText(anyToString(progress["token"]), 128),
			"status":           clipText(anyToString(progress["status"]), 80),
			"result_state":     clipText(anyToString(progress["result_state"]), 80),
			"progress_pct":     progressModel["progress_pct"],
			"pending_sources":  anyToStringList(sourceSummary["pending_sources"], 8),
			"failed_sources":   anyToStringList(sourceSummary["failed_sources"], 8),
			"delivery":         compactAgentSessionValue(comment["delivery"], 2),
		})
	}
	latest := map[string]any{}
	if len(items) > 0 {
		latest = anyMap(items[0])
	}
	return map[string]any{
		"label":           "Agent steering inbox",
		"pending_count":   len(items),
		"latest":          latest,
		"items":           items,
		"watch_command":   "contextlattice_agent_session watch --session-id <session_id> --pretty",
		"drain_command":   "contextlattice_async_inbox_drain --session-id " + clipText(sessionID, 128),
		"poll_endpoint":   "/v1/agents/sessions/{session_id}/events",
		"delivery_policy": "agents should drain this bounded inbox after normal tool boundaries; live app hosts may also watch session events or continuation SSE",
	}
}

func agentSessionDurationSecs(session map[string]any, now time.Time) float64 {
	started, ok := parseISOTime(anyToString(session["started_at"]))
	if !ok {
		return 0
	}
	end := now
	if completed, ok := parseISOTime(anyToString(session["completed_at"])); ok {
		end = completed
	} else if updated, ok := parseISOTime(anyToString(session["updated_at"])); ok {
		end = updated
	}
	if end.Before(started) {
		return 0
	}
	return roundFloat(end.Sub(started).Seconds(), 3)
}

func agentSessionRollupConfidence(session map[string]any, contribution map[string]any, phaseCounts map[string]int, risks []any) int {
	score := 10
	if anyToString(session["objective"]) != "" {
		score += 10
	}
	if phaseCounts["bootstrap"] > 0 {
		score += 8
	}
	if phaseCounts["context"] > 0 {
		score += 16
	}
	if anyToInt(contribution["writebacks"], 0) > 0 || phaseCounts["memory_write"] > 0 {
		score += 14
	}
	if anyToInt(contribution["graph_touches"], 0) > 0 {
		score += 8
	}
	if phaseCounts["verification"] > 0 {
		score += 10
	}
	if phaseCounts["handoff"] > 0 {
		score += 12
	}
	if normalizeAgentSessionStatus(anyToString(session["status"])) == "completed" {
		score += 12
	}
	if len(risks) > 0 {
		score -= minInt(len(risks)*8, 24)
	}
	status := normalizeAgentSessionStatus(anyToString(session["status"]))
	if status == "failed" || status == "blocked" {
		score -= 12
	}
	return clampInt(score, 0, 100)
}

func agentSessionPhaseCountsObject(phaseCounts map[string]int) map[string]any {
	out := make(map[string]any, len(phaseCounts))
	for key, value := range phaseCounts {
		out[key] = value
	}
	return out
}

func buildAgentSessionRollup(session map[string]any, events []map[string]any, now time.Time) map[string]any {
	session = cloneAnyMap(session)
	contribution := normalizeAgentContribution(anyMap(session["memory_contribution"]))
	phaseCounts := map[string]int{}
	returnedSources := map[string]struct{}{}
	pendingSources := map[string]struct{}{}
	failedSources := map[string]struct{}{}
	artifacts := []any{}
	risks := []any{}
	recent := []any{}
	memoryHits := anyToInt(contribution["memory_hits"], 0)
	resultCount := 0
	for _, event := range events {
		eventType := anyToString(event["type"])
		phase := agentSessionPhase(eventType)
		phaseCounts[phase]++
		metadata := anyMap(event["metadata"])
		if hits := anyToInt(metadata["memory_hits"], -1); hits > memoryHits {
			memoryHits = hits
		}
		if results := anyToInt(metadata["result_count"], -1); results > resultCount {
			resultCount = results
		}
		addAgentSessionSourceCoverage(returnedSources, pendingSources, failedSources, metadata["source_coverage"], 32)
		addAgentSessionSourceCoverage(returnedSources, pendingSources, failedSources, metadata["retrieval"], 32)
		addAgentSessionStrings(returnedSources, anyMap(metadata["source_summary"])["returned_now"], 32)
		addAgentSessionStrings(pendingSources, anyMap(metadata["source_summary"])["pending_sources"], 32)
		addAgentSessionStrings(failedSources, anyMap(metadata["source_summary"])["failed_sources"], 32)
		lower := strings.ToLower(eventType + " " + anyToString(event["summary"]))
		if strings.Contains(lower, "handoff") || strings.Contains(lower, "checkpoint") || strings.Contains(lower, "writeback") || strings.Contains(lower, "pr") || strings.Contains(lower, "test") {
			artifacts = append(artifacts, map[string]any{
				"type":       clipText(eventType, 96),
				"summary":    clipText(anyToString(event["summary"]), 240),
				"created_at": anyToString(event["created_at"]),
			})
		}
		if phase == "risk" {
			risks = append(risks, map[string]any{
				"type":       clipText(eventType, 96),
				"summary":    clipText(anyToString(event["summary"]), 240),
				"created_at": anyToString(event["created_at"]),
			})
		}
	}
	recentStart := maxInt(0, len(events)-8)
	for _, event := range events[recentStart:] {
		recent = append(recent, compactAgentSessionRecentEvent(event))
	}
	lastAgeSecs := 0.0
	if lastAt, ok := parseISOTime(anyToString(session["last_event_at"])); ok {
		lastAgeSecs = roundFloat(now.Sub(lastAt).Seconds(), 3)
	}
	missing := []any{}
	if phaseCounts["context"] == 0 {
		missing = append(missing, "context_pack")
	}
	if anyToInt(contribution["writebacks"], 0) == 0 && phaseCounts["memory_write"] == 0 {
		missing = append(missing, "checkpoint_or_writeback")
	}
	if phaseCounts["handoff"] == 0 && normalizeAgentSessionStatus(anyToString(session["status"])) == "completed" {
		missing = append(missing, "handoff")
	}
	promptReady := phaseCounts["context"] > 0 || memoryHits > 0 || anyToInt(contribution["score"], 0) >= 20
	if len(risks) > 0 {
		promptReady = promptReady && normalizeAgentSessionStatus(anyToString(session["status"])) != "failed"
	}
	objectiveHierarchy := agentSessionObjectiveHierarchy(session)
	objectiveLineage := agentSessionObjectiveLineage(session)
	agentLifecycle := normalizeAgentLifecyclePayload(session["agent_state"], anyToString(session["status"]))
	ownership := agentSessionOwnership(session)
	rollup := map[string]any{
		"ok":                  true,
		"schema_id":           agentSessionRollupContractID,
		"session_id":          anyToString(session["id"]),
		"agent":               anyToString(session["agent"]),
		"agent_id":            anyToString(session["agent_id"]),
		"project":             anyToString(session["project"]),
		"status":              normalizeAgentSessionStatus(anyToString(session["status"])),
		"agent_lifecycle":     agentLifecycle,
		"ownership":           ownership,
		"objective":           clipText(anyToString(session["objective"]), 1200),
		"mission":             clipText(anyToString(session["mission"]), 1200),
		"goal":                clipText(anyToString(session["goal"]), 1200),
		"objective_hierarchy": objectiveHierarchy,
		"objective_lineage":   objectiveLineage,
		"objective_state":     clipText(firstNonEmptyStrings(anyToString(session["objective_state"]), anyToString(anyMap(session["objective_runtime"])["objective_state"])), 80),
		"next_action":         clipText(firstNonEmptyStrings(anyToString(session["next_action"]), anyToString(anyMap(session["objective_runtime"])["next_action"])), 720),
		"started_at":          anyToString(session["started_at"]),
		"updated_at":          anyToString(session["updated_at"]),
		"completed_at":        anyToString(session["completed_at"]),
		"duration_secs":       agentSessionDurationSecs(session, now),
		"last_event_type":     anyToString(session["last_event_type"]),
		"last_event_at":       anyToString(session["last_event_at"]),
		"last_event_age_secs": lastAgeSecs,
		"event_count":         anyToInt(session["event_count"], len(events)),
		"phase_counts":        agentSessionPhaseCountsObject(phaseCounts),
		"memory_contribution": contribution,
		"retrieval_summary": map[string]any{
			"memory_hits":      memoryHits,
			"result_count":     resultCount,
			"returned_sources": sortedStringSet(returnedSources, 16),
			"pending_sources":  sortedStringSet(pendingSources, 16),
			"failed_sources":   sortedStringSet(failedSources, 8),
		},
		"artifact_summary": map[string]any{
			"checkpoints":   anyToInt(contribution["writebacks"], 0),
			"handoffs":      anyToInt(contribution["handoffs"], 0),
			"graph_touches": anyToInt(contribution["graph_touches"], 0),
			"dream_outputs": anyToInt(contribution["dream_outputs"], 0),
			"tests":         anyToInt(contribution["tests"], 0),
			"prs":           anyToInt(contribution["prs"], 0),
			"recent":        agentSessionListLimit(artifacts, 12),
		},
		"risk_summary": map[string]any{
			"missing": missing,
			"risks":   agentSessionListLimit(risks, 8),
		},
		"prompt_package": map[string]any{
			"ready":        promptReady,
			"endpoint":     "/v1/agents/sessions/" + anyToString(session["id"]) + "/context-package",
			"cli_command":  "contextlattice_agent_session context-package --session-id " + anyToString(session["id"]) + " --pretty",
			"best_surface": "cli_for_local_agents_http_for_apps_mcp_for_tool_calling_hosts",
		},
		"agent_inbox":   agentSessionSteeringInbox(anyToString(session["id"]), events),
		"confidence":    agentSessionRollupConfidence(session, contribution, phaseCounts, risks),
		"recent_events": recent,
	}
	return attachPayloadFormatContract(agentSessionRollupContractID, rollup, anyToString(session["agent_id"]), "session_rollup", "/v1/agents/sessions/{session_id}/rollup")
}

func buildAgentPromptContextPackage(session map[string]any, events []map[string]any, now time.Time) map[string]any {
	rollup := buildAgentSessionRollup(session, events, now)
	retrievalSummary := anyMap(rollup["retrieval_summary"])
	artifactSummary := anyMap(rollup["artifact_summary"])
	riskSummary := anyMap(rollup["risk_summary"])
	agentInbox := anyMap(rollup["agent_inbox"])
	agentLifecycle := anyMap(rollup["agent_lifecycle"])
	ownership := anyMap(rollup["ownership"])
	latestSteering := anyMap(agentInbox["latest"])
	sessionID := anyToString(rollup["session_id"])
	objective := firstNonEmptyStrings(anyToString(rollup["objective"]), anyToString(rollup["goal"]), "Continue the agent objective using available evidence.")
	nextAction := firstNonEmptyStrings(anyToString(rollup["next_action"]), "Inspect the rollup, retrieve missing context, and execute the smallest evidence-backed next action.")
	objectiveHierarchy := anyMap(rollup["objective_hierarchy"])
	objectiveLineage := anyMap(rollup["objective_lineage"])
	projectPrimary := anyToString(anyMap(objectiveHierarchy["project"])["primary_objective"])
	topicObjective := anyToString(anyMap(objectiveHierarchy["topic"])["objective"])
	sessionObjective := anyToString(anyMap(objectiveHierarchy["session"])["objective"])
	lineageStatus := anyToString(anyMap(objectiveLineage["drift"])["status"])
	referencePrompt := strings.Join([]string{
		"Use this ContextLattice session package as the factual context for the next reasoning step.",
		"Session: " + sessionID,
		"Agent: " + firstNonEmptyStrings(anyToString(rollup["agent"]), anyToString(rollup["agent_id"]), "agent"),
		"Project: " + anyToString(rollup["project"]),
		"Project primary objective: " + projectPrimary,
		"Topic objective: " + topicObjective,
		"Session objective: " + firstNonEmptyStrings(sessionObjective, objective),
		"Objective lineage: " + firstNonEmptyStrings(lineageStatus, "unknown"),
		"Objective: " + objective,
		"Status: " + anyToString(rollup["status"]) + " / last event " + anyToString(rollup["last_event_type"]),
		"Agent lifecycle: " + firstNonEmptyStrings(anyToString(agentLifecycle["state"]), "unknown") + " via " + firstNonEmptyStrings(anyToString(agentLifecycle["authority"]), "unknown"),
		"Ownership: repo=" + anyToString(ownership["repo"]) + " worktree=" + anyToString(ownership["worktree"]) + " branch=" + anyToString(ownership["branch"]) + " task=" + anyToString(ownership["task_id"]),
		"Memory contribution score: " + anyToString(anyMap(rollup["memory_contribution"])["score"]),
		"Retrieved sources: " + strings.Join(anyToStringList(retrievalSummary["returned_sources"], 16), ", "),
		"Latest agent steering: " + firstNonEmptyStrings(anyToString(latestSteering["message"]), "none"),
		"Artifacts: checkpoints=" + anyToString(artifactSummary["checkpoints"]) + ", handoffs=" + anyToString(artifactSummary["handoffs"]) + ", graph=" + anyToString(artifactSummary["graph_touches"]) + ", tests=" + anyToString(artifactSummary["tests"]),
		"Risks or missing evidence: " + strings.Join(anyToStringList(riskSummary["missing"], 12), ", "),
		"Next action: " + nextAction,
		"Do not treat this package as proof beyond the listed evidence; retrieve or inspect artifacts before making new claims.",
	}, "\n")
	payload := map[string]any{
		"ok":         true,
		"schema_id":  agentPromptContextPackageContractID,
		"session_id": sessionID,
		"agent":      anyToString(rollup["agent"]),
		"agent_id":   anyToString(rollup["agent_id"]),
		"project":    anyToString(rollup["project"]),
		"rollup":     rollup,
		"context_package": map[string]any{
			"objective":           objective,
			"mission":             anyToString(rollup["mission"]),
			"goal":                anyToString(rollup["goal"]),
			"objective_hierarchy": objectiveHierarchy,
			"objective_lineage":   objectiveLineage,
			"current_state":       anyToString(rollup["objective_state"]),
			"agent_lifecycle":     agentLifecycle,
			"ownership":           ownership,
			"next_action":         nextAction,
			"retrieval_summary":   retrievalSummary,
			"agent_inbox":         agentInbox,
			"artifact_summary":    artifactSummary,
			"risk_summary":        riskSummary,
			"recent_events":       rollup["recent_events"],
			"confidence":          rollup["confidence"],
			"source":              "contextlattice_agent_session_rollup",
			"intended_use":        "repackage the next agent/model prompt with bounded evidence, state, risks, and next action",
			"recommended_surface": "cli_for_local_agents",
			"alternate_surfaces": []any{
				"http_for_app_integrations",
				"mcp_for_tool_calling_hosts",
			},
		},
		"reference_prompt": clipText(referencePrompt, 5000),
	}
	return attachPayloadFormatContract(agentPromptContextPackageContractID, payload, anyToString(rollup["agent_id"]), "prompt_context_package", "/v1/agents/sessions/{session_id}/context-package")
}

func compactAgentTraceSourceSummary(metadata map[string]any) map[string]any {
	returnedSources := map[string]struct{}{}
	pendingSources := map[string]struct{}{}
	failedSources := map[string]struct{}{}
	addAgentSessionSourceCoverage(returnedSources, pendingSources, failedSources, metadata["source_coverage"], 16)
	addAgentSessionSourceCoverage(returnedSources, pendingSources, failedSources, metadata["retrieval"], 16)
	addAgentSessionStrings(returnedSources, anyMap(metadata["source_summary"])["returned_now"], 16)
	addAgentSessionStrings(pendingSources, anyMap(metadata["source_summary"])["pending_sources"], 16)
	addAgentSessionStrings(failedSources, anyMap(metadata["source_summary"])["failed_sources"], 16)
	out := map[string]any{}
	if len(returnedSources) > 0 {
		out["returned_sources"] = sortedStringSet(returnedSources, 12)
	}
	if len(pendingSources) > 0 {
		out["pending_sources"] = sortedStringSet(pendingSources, 12)
	}
	if len(failedSources) > 0 {
		out["failed_sources"] = sortedStringSet(failedSources, 8)
	}
	return out
}

func compactAgentTraceEvent(event map[string]any) map[string]any {
	metadata := anyMap(event["metadata"])
	out := map[string]any{
		"id":         clipText(anyToString(event["id"]), 96),
		"type":       clipText(anyToString(event["type"]), 96),
		"phase":      agentSessionPhase(anyToString(event["type"])),
		"summary":    clipText(anyToString(event["summary"]), 280),
		"status":     normalizeAgentSessionStatus(anyToString(event["status"])),
		"created_at": anyToString(event["created_at"]),
	}
	if hits := anyToInt(metadata["memory_hits"], -1); hits >= 0 {
		out["memory_hits"] = hits
	}
	if count := anyToInt(metadata["result_count"], -1); count >= 0 {
		out["result_count"] = count
	}
	if topicPath := clipText(anyToString(metadata["topic_path"]), 180); topicPath != "" {
		out["topic_path"] = topicPath
	}
	if mode := clipText(anyToString(metadata["retrieval_mode"]), 48); mode != "" {
		out["retrieval_mode"] = mode
	}
	if sources := compactAgentTraceSourceSummary(metadata); len(sources) > 0 {
		out["source_summary"] = sources
	}
	if edgeCount := anyToInt(metadata["edge_count"], -1); edgeCount >= 0 {
		out["edge_count"] = edgeCount
	}
	if skillsReturned := anyToInt(metadata["skills_index_returned"], -1); skillsReturned >= 0 {
		out["skills_index_returned"] = skillsReturned
	}
	if state := anyMap(metadata["agent_state"]); len(state) > 0 {
		out["agent_state"] = normalizeAgentLifecyclePayload(state, anyToString(event["status"]))
	}
	if ownership := anyMap(metadata["ownership"]); len(ownership) > 0 {
		out["ownership"] = compactAgentSessionMetadata(ownership)
	}
	return out
}

func agentTraceTimeline(events []map[string]any, limit int) []any {
	if limit < 1 {
		limit = 1
	}
	start := maxInt(0, len(events)-limit)
	out := make([]any, 0, len(events)-start)
	for _, event := range events[start:] {
		out = append(out, compactAgentTraceEvent(event))
	}
	return out
}

func agentTraceEventsForPhase(events []map[string]any, phase string, limit int) []any {
	out := []any{}
	for i := len(events) - 1; i >= 0 && len(out) < limit; i-- {
		event := events[i]
		if agentSessionPhase(anyToString(event["type"])) == phase {
			out = append(out, compactAgentTraceEvent(event))
		}
	}
	return out
}

func agentTraceHelpfulSkills(events []map[string]any) map[string]any {
	items := []any{}
	returned := 0
	for i := len(events) - 1; i >= 0; i-- {
		metadata := anyMap(events[i]["metadata"])
		skillsIndex := anyMap(metadata["skills_index"])
		if len(skillsIndex) == 0 && anyToInt(metadata["skills_index_returned"], -1) >= 0 {
			skillsIndex = map[string]any{"returned": metadata["skills_index_returned"]}
		}
		if len(skillsIndex) == 0 {
			continue
		}
		returned = maxInt(returned, anyToInt(skillsIndex["returned"], 0))
		topItems, _ := asAnySlice(skillsIndex["top"])
		for _, raw := range topItems {
			row := anyMap(raw)
			name := clipText(anyToString(row["name"]), 120)
			if name == "" {
				continue
			}
			items = append(items, map[string]any{
				"name":   name,
				"source": clipText(anyToString(row["source"]), 64),
				"path":   clipText(anyToString(row["path"]), 220),
				"score":  anyToInt(row["score"], 0),
			})
			if len(items) >= 8 {
				break
			}
		}
		if len(items) >= 8 || returned > 0 {
			break
		}
	}
	return map[string]any{
		"label":          "Skills that may be helpful for this work",
		"returned_count": returned,
		"items":          items,
		"lookup_command": "contextlattice_skills_index search '<objective>' --pretty",
	}
}

func buildAgentRunCardMarkdown(trace map[string]any) string {
	session := anyMap(trace["session"])
	runShaping := anyMap(trace["run_shaping"])
	advisor := anyMap(trace["run_advisor"])
	if len(advisor) == 0 {
		advisor = anyMap(runShaping["run_advisor"])
	}
	promptQuality := anyMap(advisor["prompt_quality"])
	continuation := anyMap(advisor["continuation"])
	objectiveCoherence := anyMap(advisor["objective_coherence"])
	objectiveHierarchy := anyMap(session["objective_hierarchy"])
	objectiveLineage := anyMap(session["objective_lineage"])
	agentInbox := anyMap(runShaping["agent_inbox"])
	skills := anyMap(runShaping["skills"])
	sources := anyMap(runShaping["sources"])
	graph := anyMap(runShaping["graph"])
	handoffs := anyToInt(anyMap(runShaping["handoffs"])["count"], 0)
	checkpoints := anyToInt(anyMap(runShaping["checkpoints"])["count"], 0)
	var b strings.Builder
	b.WriteString("# ContextLattice Agent Run Card\n\n")
	b.WriteString("- Session: `" + clipText(anyToString(session["id"]), 120) + "`\n")
	b.WriteString("- Agent: `" + clipText(firstNonEmptyStrings(anyToString(session["agent"]), anyToString(session["agent_id"]), "agent"), 120) + "`\n")
	b.WriteString("- Project: `" + clipText(anyToString(session["project"]), 120) + "`\n")
	b.WriteString("- Status: `" + clipText(anyToString(session["status"]), 80) + "`\n")
	if lifecycle := anyMap(session["agent_lifecycle"]); len(lifecycle) > 0 {
		b.WriteString("- Agent lifecycle: `" + clipText(anyToString(lifecycle["state"]), 80) + "` via `" + clipText(anyToString(lifecycle["authority"]), 80) + "`\n")
	}
	if ownership := anyMap(session["ownership"]); len(ownership) > 0 {
		b.WriteString("- Ownership: repo `" + clipText(anyToString(ownership["repo"]), 160) + "`, worktree `" + clipText(anyToString(ownership["worktree"]), 160) + "`, branch `" + clipText(anyToString(ownership["branch"]), 80) + "`, task `" + clipText(anyToString(ownership["task_id"]), 80) + "`\n")
	}
	b.WriteString("- Objective: " + clipText(anyToString(session["objective"]), 420) + "\n")
	if len(objectiveHierarchy) > 0 {
		projectObjective := clipText(anyToString(anyMap(objectiveHierarchy["project"])["primary_objective"]), 420)
		topicObjective := clipText(anyToString(anyMap(objectiveHierarchy["topic"])["objective"]), 420)
		sessionObjective := clipText(anyToString(anyMap(objectiveHierarchy["session"])["objective"]), 420)
		b.WriteString("- Project primary objective: " + projectObjective + "\n")
		b.WriteString("- Topic objective: " + topicObjective + "\n")
		b.WriteString("- Session objective: " + sessionObjective + "\n")
	}
	if drift := anyMap(objectiveLineage["drift"]); len(drift) > 0 {
		b.WriteString("- Objective lineage: `" + clipText(anyToString(drift["status"]), 80) + "`\n")
	}
	b.WriteString("- Next action: " + clipText(anyToString(session["next_action"]), 420) + "\n\n")
	if len(advisor) > 0 {
		b.WriteString("## Run Advisor\n\n")
		b.WriteString("- Posture: `" + clipText(anyToString(advisor["posture"]), 80) + "`\n")
		b.WriteString("- Prompt quality: `" + anyToString(promptQuality["score"]) + "` / `" + clipText(anyToString(promptQuality["state"]), 80) + "`\n")
		b.WriteString("- Objective coherence: `" + anyToString(objectiveCoherence["score"]) + "` / `" + clipText(anyToString(objectiveCoherence["status"]), 80) + "`\n")
		b.WriteString("- Continuation: `" + clipText(anyToString(continuation["status"]), 80) + "`\n")
		if repair := clipText(anyToString(continuation["repair_instruction"]), 420); repair != "" {
			b.WriteString("- Repair: " + repair + "\n")
		}
		b.WriteString("\n")
	}
	if latest := anyMap(agentInbox["latest"]); len(latest) > 0 {
		b.WriteString("## Agent Steering\n\n")
		b.WriteString("- Latest: " + clipText(anyToString(latest["message"]), 420) + "\n")
		if action := clipText(anyToString(latest["suggested_action"]), 420); action != "" {
			b.WriteString("- Suggested action: " + action + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("## Run-Shaping Evidence\n\n")
	b.WriteString("- Sources returned: " + strings.Join(anyToStringList(sources["returned_sources"], 12), ", ") + "\n")
	b.WriteString("- Pending sources: " + strings.Join(anyToStringList(sources["pending_sources"], 12), ", ") + "\n")
	b.WriteString("- Graph touches: " + anyToString(graph["touches"]) + "\n")
	b.WriteString("- Checkpoints: " + anyToString(checkpoints) + "\n")
	b.WriteString("- Handoffs: " + anyToString(handoffs) + "\n\n")
	b.WriteString("## Skills That May Be Helpful\n\n")
	skillItems, _ := asAnySlice(skills["items"])
	if len(skillItems) == 0 {
		b.WriteString("- No specific skill candidates were captured; run `" + clipText(anyToString(skills["lookup_command"]), 180) + "`.\n")
	} else {
		for _, item := range agentSessionListLimit(skillItems, 8) {
			row := anyMap(item)
			b.WriteString("- " + clipText(anyToString(row["name"]), 120))
			if source := clipText(anyToString(row["source"]), 64); source != "" {
				b.WriteString(" (" + source + ")")
			}
			b.WriteString("\n")
		}
	}
	return clipText(b.String(), 6000)
}

func buildAgentRunTrace(session map[string]any, events []map[string]any, now time.Time) map[string]any {
	rollup := buildAgentSessionRollup(session, events, now)
	promptPackage := buildAgentPromptContextPackage(session, events, now)
	retrievalSummary := anyMap(rollup["retrieval_summary"])
	artifactSummary := anyMap(rollup["artifact_summary"])
	prompt := anyMap(rollup["prompt_package"])
	runAdvisor := buildRunAdvisorFromTraceRollup(session, rollup, events)
	trace := map[string]any{
		"ok":        true,
		"schema_id": agentRunTraceContractID,
		"session": map[string]any{
			"id":                  anyToString(rollup["session_id"]),
			"agent":               anyToString(rollup["agent"]),
			"agent_id":            anyToString(rollup["agent_id"]),
			"project":             anyToString(rollup["project"]),
			"status":              anyToString(rollup["status"]),
			"agent_lifecycle":     rollup["agent_lifecycle"],
			"ownership":           rollup["ownership"],
			"objective":           anyToString(rollup["objective"]),
			"objective_hierarchy": rollup["objective_hierarchy"],
			"objective_lineage":   rollup["objective_lineage"],
			"objective_state":     anyToString(rollup["objective_state"]),
			"next_action":         anyToString(rollup["next_action"]),
			"started_at":          anyToString(rollup["started_at"]),
			"updated_at":          anyToString(rollup["updated_at"]),
			"completed_at":        anyToString(rollup["completed_at"]),
			"duration_secs":       rollup["duration_secs"],
			"event_count":         rollup["event_count"],
			"confidence":          rollup["confidence"],
		},
		"phase_counts":        rollup["phase_counts"],
		"memory_contribution": rollup["memory_contribution"],
		"run_shaping": map[string]any{
			"run_advisor":     runAdvisor,
			"agent_lifecycle": rollup["agent_lifecycle"],
			"ownership":       rollup["ownership"],
			"objective": map[string]any{
				"hierarchy": rollup["objective_hierarchy"],
				"lineage":   rollup["objective_lineage"],
			},
			"context": map[string]any{
				"validation":             anyToString(anyMap(anyMap(promptPackage["format_contract"])["validation"])["status"]),
				"recommended_surface":    anyToString(anyMap(promptPackage["context_package"])["recommended_surface"]),
				"reference_prompt_chars": len(anyToString(promptPackage["reference_prompt"])),
				"prompt_ready":           anyToBool(prompt["ready"]),
				"endpoint":               anyToString(prompt["endpoint"]),
				"cli_command":            anyToString(prompt["cli_command"]),
			},
			"skills":      agentTraceHelpfulSkills(events),
			"sources":     retrievalSummary,
			"agent_inbox": anyMap(rollup["agent_inbox"]),
			"graph":       map[string]any{"touches": anyToInt(artifactSummary["graph_touches"], 0), "recent": agentTraceEventsForPhase(events, "graph", 8)},
			"handoffs":    map[string]any{"count": anyToInt(artifactSummary["handoffs"], 0), "recent": agentTraceEventsForPhase(events, "handoff", 8)},
			"checkpoints": map[string]any{"count": anyToInt(artifactSummary["checkpoints"], 0), "recent": agentTraceEventsForPhase(events, "memory_write", 8)},
			"risks":       rollup["risk_summary"],
		},
		"run_advisor": runAdvisor,
		"timeline":    agentTraceTimeline(events, 32),
		"run_card": map[string]any{
			"markdown":      "",
			"json_endpoint": "/v1/agents/sessions/" + anyToString(rollup["session_id"]) + "/trace",
			"cli_tree":      "contextlattice_agent_trace --session-id " + anyToString(rollup["session_id"]) + " --tree",
			"cli_markdown":  "contextlattice_agent_trace --session-id " + anyToString(rollup["session_id"]) + " --markdown",
		},
		"limits": map[string]any{
			"timeline_events": 32,
			"skills":          8,
			"graph_events":    8,
			"handoffs":        8,
			"checkpoints":     8,
		},
	}
	anyMap(trace["run_card"])["markdown"] = buildAgentRunCardMarkdown(trace)
	return attachPayloadFormatContract(agentRunTraceContractID, trace, anyToString(rollup["agent_id"]), "agent_run_trace", "/v1/agents/sessions/{session_id}/trace")
}

func normalizeAgentSessionStart(payload map[string]any, fallbackID string) map[string]any {
	now := nowUTCISO()
	sessionID := strings.TrimSpace(firstNonEmptyStrings(
		anyToString(payload["session_id"]),
		anyToString(payload["sessionId"]),
		anyToString(payload["id"]),
		fallbackID,
	))
	if sessionID == "" {
		sessionID = "sess_" + bson.NewObjectID().Hex()
	}
	status := normalizeAgentSessionStatus(anyToString(payload["status"]))
	record := map[string]any{
		"id":                  clipText(sessionID, 128),
		"agent":               clipText(strings.TrimSpace(anyToString(payload["agent"])), 80),
		"agent_id":            clipText(strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["agent_id"]), anyToString(payload["agentId"]))), 120),
		"agent_kind":          clipText(strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["agent_kind"]), anyToString(payload["agentKind"]), anyToString(payload["runner"]))), 80),
		"project":             clipText(strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["project"]), anyToString(payload["project_name"]))), 120),
		"repo":                clipText(strings.TrimSpace(anyToString(payload["repo"])), 240),
		"branch":              clipText(strings.TrimSpace(anyToString(payload["branch"])), 160),
		"worktree":            clipText(strings.TrimSpace(anyToString(payload["worktree"])), 320),
		"cwd":                 clipText(strings.TrimSpace(anyToString(payload["cwd"])), 320),
		"task_id":             clipText(strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["task_id"]), anyToString(payload["taskId"]))), 160),
		"native_session_id":   clipText(strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["native_session_id"]), anyToString(payload["nativeSessionId"]))), 180),
		"agent_state":         normalizeAgentLifecyclePayload(payload["agent_state"], status),
		"objective":           clipText(strings.TrimSpace(anyToString(payload["objective"])), 1200),
		"mission":             clipText(strings.TrimSpace(anyToString(payload["mission"])), 1200),
		"goal":                clipText(strings.TrimSpace(anyToString(payload["goal"])), 1200),
		"objective_hierarchy": compactAgentSessionMetadata(anyMap(payload["objective_hierarchy"])),
		"objective_lineage":   compactAgentSessionMetadata(anyMap(payload["objective_lineage"])),
		"status":              status,
		"started_at":          firstNonEmptyStrings(anyToString(payload["started_at"]), anyToString(payload["startedAt"]), now),
		"updated_at":          now,
		"completed_at":        "",
		"last_event_type":     "session.started",
		"last_event_at":       now,
		"event_count":         0,
		"tags":                normalizeAgentSessionTags(payload["tags"]),
		"metadata":            compactAgentSessionMetadata(anyMap(payload["metadata"])),
		"memory_contribution": normalizeAgentContribution(anyMap(payload["memory_contribution"])),
	}
	return record
}

func normalizeAgentSessionEvent(sessionID string, payload map[string]any) map[string]any {
	now := nowUTCISO()
	eventType := normalizeAgentEventType(firstNonEmptyStrings(
		anyToString(payload["type"]),
		anyToString(payload["event_type"]),
		anyToString(payload["eventType"]),
	))
	eventID := strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["id"]), anyToString(payload["event_id"])))
	if eventID == "" {
		eventID = "evt_" + bson.NewObjectID().Hex()
	}
	metadata := compactAgentSessionMetadata(anyMap(payload["metadata"]))
	for _, key := range []string{"source_coverage", "retrieval", "graph", "dream", "tests", "pr", "pull_request", "handoff", "objective_hierarchy", "objective_lineage", "agent_state", "ownership"} {
		if value, ok := payload[key]; ok {
			metadata[key] = compactAgentSessionValue(value, 3)
		}
	}
	return map[string]any{
		"id":         clipText(eventID, 128),
		"session_id": clipText(sessionID, 128),
		"type":       eventType,
		"agent":      clipText(strings.TrimSpace(anyToString(payload["agent"])), 80),
		"agent_id":   clipText(strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["agent_id"]), anyToString(payload["agentId"]))), 120),
		"project":    clipText(strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["project"]), anyToString(payload["project_name"]))), 120),
		"summary":    clipText(strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["summary"]), anyToString(payload["message"]), anyToString(payload["query"]))), 720),
		"status":     normalizeAgentSessionStatus(anyToString(payload["status"])),
		"metadata":   metadata,
		"created_at": firstNonEmptyStrings(anyToString(payload["created_at"]), anyToString(payload["createdAt"]), now),
	}
}

func (s *agentSessionStore) start(payload map[string]any) (map[string]any, error) {
	if s == nil {
		return nil, errors.New("agent session store unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record := normalizeAgentSessionStart(payload, "")
	id := anyToString(record["id"])
	if existing, ok := s.sessions[id]; ok {
		for key, value := range record {
			if key == "started_at" || key == "event_count" {
				continue
			}
			if value == "" || value == nil {
				continue
			}
			existing[key] = value
		}
		existing["updated_at"] = nowUTCISO()
		record = existing
	} else {
		s.sessions[id] = record
		s.order = append(s.order, id)
	}
	s.enforceBoundsLocked()
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return cloneAnyMap(record), nil
}

func (s *agentSessionStore) appendEvent(sessionID string, payload map[string]any) (map[string]any, map[string]any, error) {
	if s == nil {
		return nil, nil, errors.New("agent session store unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sessionID = strings.TrimSpace(firstNonEmptyStrings(
		sessionID,
		anyToString(payload["session_id"]),
		anyToString(payload["sessionId"]),
	))
	if sessionID == "" {
		sessionID = "sess_" + bson.NewObjectID().Hex()
	}
	session, ok := s.sessions[sessionID]
	if !ok {
		session = normalizeAgentSessionStart(payload, sessionID)
		session["last_event_type"] = ""
		session["event_count"] = 0
		s.sessions[sessionID] = session
		s.order = append(s.order, sessionID)
	}
	event := normalizeAgentSessionEvent(sessionID, payload)
	eventType := anyToString(event["type"])
	createdAt := anyToString(event["created_at"])
	if anyToString(event["agent"]) == "" && anyToString(session["agent"]) != "" {
		event["agent"] = session["agent"]
	}
	if anyToString(event["agent_id"]) == "" && anyToString(session["agent_id"]) != "" {
		event["agent_id"] = session["agent_id"]
	}
	if anyToString(event["project"]) == "" && anyToString(session["project"]) != "" {
		event["project"] = session["project"]
	}
	if strings.TrimSpace(anyToString(event["summary"])) == "" {
		event["summary"] = eventType
	}
	s.events[sessionID] = append(s.events[sessionID], event)
	if len(s.events[sessionID]) > s.maxEvents {
		s.events[sessionID] = s.events[sessionID][len(s.events[sessionID])-s.maxEvents:]
	}
	if anyToString(session["agent"]) == "" && anyToString(event["agent"]) != "" {
		session["agent"] = event["agent"]
	}
	if anyToString(session["agent_id"]) == "" && anyToString(event["agent_id"]) != "" {
		session["agent_id"] = event["agent_id"]
	}
	if anyToString(session["project"]) == "" && anyToString(event["project"]) != "" {
		session["project"] = event["project"]
	}
	session["last_event_type"] = eventType
	session["last_event_at"] = createdAt
	session["updated_at"] = createdAt
	session["event_count"] = anyToInt(session["event_count"], 0) + 1
	metadata := anyMap(event["metadata"])
	if runtime := anyMap(metadata["objective_runtime"]); len(runtime) > 0 {
		session["objective_runtime"] = compactAgentSessionMetadata(runtime)
		if hierarchy := anyMap(runtime["objective_hierarchy"]); len(hierarchy) > 0 {
			session["objective_hierarchy"] = compactAgentSessionMetadata(hierarchy)
		}
		if lineage := anyMap(runtime["objective_lineage"]); len(lineage) > 0 {
			session["objective_lineage"] = compactAgentSessionMetadata(lineage)
		}
	}
	if hierarchy := anyMap(metadata["objective_hierarchy"]); len(hierarchy) > 0 {
		session["objective_hierarchy"] = compactAgentSessionMetadata(hierarchy)
	}
	if lineage := anyMap(metadata["objective_lineage"]); len(lineage) > 0 {
		session["objective_lineage"] = compactAgentSessionMetadata(lineage)
	}
	if objectiveState := strings.TrimSpace(anyToString(metadata["objective_state"])); objectiveState != "" {
		session["objective_state"] = clipText(objectiveState, 80)
	}
	if nextAction := strings.TrimSpace(anyToString(metadata["next_action"])); nextAction != "" {
		session["next_action"] = clipText(nextAction, 720)
	}
	if statePayload := anyMap(metadata["agent_state"]); len(statePayload) > 0 {
		state := normalizeAgentLifecyclePayload(statePayload, anyToString(event["status"]))
		session["agent_state"] = state
		for _, key := range []string{"task_id", "repo", "branch", "worktree", "cwd", "native_session_id"} {
			if value := strings.TrimSpace(anyToString(state[key])); value != "" {
				session[key] = clipText(value, 360)
			}
		}
	}
	if ownership := anyMap(metadata["ownership"]); len(ownership) > 0 {
		for _, key := range []string{"task_id", "repo", "branch", "worktree", "cwd", "native_session_id"} {
			if value := strings.TrimSpace(anyToString(ownership[key])); value != "" {
				session[key] = clipText(value, 360)
			}
		}
	}
	switch eventType {
	case "session.completed", "agent.session.completed":
		session["status"] = "completed"
		session["agent_state"] = normalizeAgentLifecyclePayload(firstNonEmptyAny(metadata["agent_state"], map[string]any{"state": "done", "authority": "self_report", "source": eventType}), "completed")
		session["completed_at"] = createdAt
	case "session.failed", "agent.session.failed":
		session["status"] = "failed"
		session["agent_state"] = normalizeAgentLifecyclePayload(firstNonEmptyAny(metadata["agent_state"], map[string]any{"state": "blocked", "authority": "self_report", "source": eventType}), "failed")
		session["completed_at"] = createdAt
	case "session.blocked", "agent.session.blocked":
		session["status"] = "blocked"
		session["agent_state"] = normalizeAgentLifecyclePayload(firstNonEmptyAny(metadata["agent_state"], map[string]any{"state": "blocked", "authority": "self_report", "source": eventType}), "blocked")
		session["completed_at"] = ""
	case "session.canceled", "agent.session.canceled":
		session["status"] = "canceled"
		session["completed_at"] = createdAt
	default:
		if !agentSessionTerminal(anyToString(session["status"])) {
			if state := anyToString(anyMap(session["agent_state"])["state"]); state != "" {
				session["status"] = normalizeAgentSessionStatus(state)
			} else {
				session["status"] = "active"
			}
			if !agentSessionTerminal(anyToString(session["status"])) {
				session["completed_at"] = ""
			}
		}
	}
	session["memory_contribution"] = bumpContribution(
		anyMap(session["memory_contribution"]),
		eventType,
		metadata,
		createdAt,
	)
	s.enforceBoundsLocked()
	if err := s.persistLocked(); err != nil {
		return nil, nil, err
	}
	return cloneAnyMap(session), cloneAnyMap(event), nil
}

func (s *agentSessionStore) list(status string, project string, agent string, limit int) []map[string]any {
	if s == nil {
		return []map[string]any{}
	}
	status = strings.TrimSpace(strings.ToLower(status))
	project = strings.TrimSpace(project)
	agent = strings.TrimSpace(agent)
	limit = clampInt(limit, 1, 500)
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := make([]map[string]any, 0, limit)
	for i := len(s.order) - 1; i >= 0; i-- {
		id := s.order[i]
		row, ok := s.sessions[id]
		if !ok {
			continue
		}
		if status != "" && status != "all" && normalizeAgentSessionStatus(anyToString(row["status"])) != normalizeAgentSessionStatus(status) {
			continue
		}
		if project != "" && !strings.EqualFold(anyToString(row["project"]), project) {
			continue
		}
		if agent != "" && !strings.EqualFold(anyToString(row["agent"]), agent) && !strings.EqualFold(anyToString(row["agent_id"]), agent) {
			continue
		}
		copyRow := cloneAnyMap(row)
		copyRow["rollup"] = buildAgentSessionRollup(copyRow, s.events[id], now)
		rows = append(rows, copyRow)
		if len(rows) >= limit {
			break
		}
	}
	return rows
}

func (s *agentSessionStore) get(sessionID string) (map[string]any, []map[string]any, bool) {
	if s == nil {
		return nil, nil, false
	}
	sessionID = strings.TrimSpace(sessionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.sessions[sessionID]
	if !ok {
		return nil, nil, false
	}
	events := make([]map[string]any, 0, len(s.events[sessionID]))
	for _, event := range s.events[sessionID] {
		events = append(events, cloneAnyMap(event))
	}
	return cloneAnyMap(row), events, true
}

func (s *agentSessionStore) runtimeSnapshot(limit int) map[string]any {
	if s == nil {
		return map[string]any{"enabled": false, "sessions": []any{}}
	}
	limit = clampInt(limit, 1, 100)
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := map[string]int{}
	scoreSum := 0
	scoreCount := 0
	active := make([]map[string]any, 0, limit)
	for i := len(s.order) - 1; i >= 0; i-- {
		id := s.order[i]
		row, ok := s.sessions[id]
		if !ok {
			continue
		}
		status := normalizeAgentSessionStatus(anyToString(row["status"]))
		counts[status] += 1
		contribution := normalizeAgentContribution(anyMap(row["memory_contribution"]))
		score := anyToInt(contribution["score"], 0)
		scoreSum += score
		scoreCount += 1
		if len(active) < limit {
			copyRow := cloneAnyMap(row)
			copyRow["memory_contribution"] = contribution
			if lastAt, ok := parseISOTime(anyToString(row["last_event_at"])); ok {
				copyRow["last_event_age_secs"] = roundFloat(now.Sub(lastAt).Seconds(), 3)
			}
			copyRow["rollup"] = buildAgentSessionRollup(copyRow, s.events[id], now)
			active = append(active, copyRow)
		}
	}
	avg := 0.0
	if scoreCount > 0 {
		avg = roundFloat(float64(scoreSum)/float64(scoreCount), 3)
	}
	return map[string]any{
		"enabled":                 true,
		"path":                    s.path,
		"total":                   len(s.sessions),
		"active":                  counts["active"],
		"completed":               counts["completed"],
		"failed":                  counts["failed"],
		"blocked":                 counts["blocked"],
		"paused":                  counts["paused"],
		"canceled":                counts["canceled"],
		"avg_memory_contribution": avg,
		"max_records":             s.maxKeep,
		"max_events_per_session":  s.maxEvents,
		"sessions":                active,
	}
}

func (s *server) agentRuntimeTelemetryRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	limit := parseOptionalIntQuery(r.URL.Query().Get("limit"), 16, 1, 100)
	writeJSON(w, http.StatusOK, s.agentSessions.runtimeSnapshot(limit))
}

func (s *server) agentsSessionsRoute(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	if s.agentSessions == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "agent session store unavailable"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/agents/sessions")
	path = strings.Trim(path, "/")
	switch {
	case path == "":
		s.agentsSessionsCollection(w, r)
	case path == "start":
		s.agentsSessionsStart(w, r)
	case path == "event":
		s.agentsSessionsEvent(w, r, "")
	default:
		parts := strings.Split(path, "/")
		sessionID := strings.TrimSpace(parts[0])
		if len(parts) == 1 {
			s.agentsSessionItem(w, r, sessionID)
			return
		}
		if len(parts) == 2 && parts[1] == "events" {
			s.agentsSessionsEvent(w, r, sessionID)
			return
		}
		if len(parts) == 2 && parts[1] == "rollup" {
			s.agentsSessionRollup(w, r, sessionID)
			return
		}
		if len(parts) == 2 && (parts[1] == "context-package" || parts[1] == "prompt-package") {
			s.agentsSessionContextPackage(w, r, sessionID)
			return
		}
		if len(parts) == 2 && parts[1] == "trace" {
			s.agentsSessionTrace(w, r, sessionID)
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "agent session route not found"})
	}
}

func (s *server) agentsSessionsCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		limit := parseOptionalIntQuery(r.URL.Query().Get("limit"), 25, 1, 500)
		status := strings.TrimSpace(r.URL.Query().Get("status"))
		project := strings.TrimSpace(r.URL.Query().Get("project"))
		agent := strings.TrimSpace(r.URL.Query().Get("agent"))
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":       true,
			"sessions": s.agentSessions.list(status, project, agent, limit),
		})
	case http.MethodPost:
		s.agentsSessionsStart(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

func (s *server) agentsSessionsStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	payload, err := readOptionalJSONBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
		return
	}
	session, err := s.agentSessions.start(payload)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "agent session start failed", "detail": err.Error()})
		return
	}
	_, startedEvent, eventErr := s.agentSessions.appendEvent(anyToString(session["id"]), map[string]any{
		"type":     "session.started",
		"agent":    session["agent"],
		"agent_id": session["agent_id"],
		"project":  session["project"],
		"summary":  session["objective"],
		"metadata": map[string]any{
			"repo":              session["repo"],
			"branch":            session["branch"],
			"worktree":          session["worktree"],
			"cwd":               session["cwd"],
			"task_id":           session["task_id"],
			"native_session_id": session["native_session_id"],
			"agent_state":       session["agent_state"],
			"ownership":         agentSessionOwnership(session),
			"tags":              session["tags"],
		},
	})
	if eventErr == nil {
		session, _, _ = s.agentSessions.get(anyToString(session["id"]))
	}
	response := map[string]any{"ok": true, "session": session}
	if startedEvent != nil {
		response["event"] = startedEvent
	}
	if refreshed, events, ok := s.agentSessions.get(anyToString(session["id"])); ok {
		response["session"] = refreshed
		response["rollup"] = buildAgentSessionRollup(refreshed, events, time.Now().UTC())
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *server) agentsSessionItem(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	session, events, ok := s.agentSessions.get(sessionID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "agent session not found"})
		return
	}
	rollup := buildAgentSessionRollup(session, events, time.Now().UTC())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "session": session, "events": events, "rollup": rollup})
}

func (s *server) agentsSessionRollup(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	session, events, ok := s.agentSessions.get(sessionID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "agent session not found"})
		return
	}
	rollup := buildAgentSessionRollup(session, events, time.Now().UTC())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "rollup": rollup})
}

func (s *server) agentsSessionContextPackage(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	session, events, ok := s.agentSessions.get(sessionID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "agent session not found"})
		return
	}
	payload := buildAgentPromptContextPackage(session, events, time.Now().UTC())
	writeJSON(w, http.StatusOK, payload)
}

func (s *server) agentsSessionTrace(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	session, events, ok := s.agentSessions.get(sessionID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "agent session not found"})
		return
	}
	trace := buildAgentRunTrace(session, events, time.Now().UTC())
	writeJSON(w, http.StatusOK, trace)
}

func (s *server) agentsSessionsEvent(w http.ResponseWriter, r *http.Request, sessionID string) {
	switch r.Method {
	case http.MethodGet:
		if strings.TrimSpace(sessionID) == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "session_id is required"})
			return
		}
		session, events, ok := s.agentSessions.get(sessionID)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "agent session not found"})
			return
		}
		rollup := buildAgentSessionRollup(session, events, time.Now().UTC())
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "session": session, "events": events, "rollup": rollup})
	case http.MethodPost:
		payload, err := readOptionalJSONBody(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
			return
		}
		session, event, err := s.agentSessions.appendEvent(sessionID, payload)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "agent session event failed", "detail": err.Error()})
			return
		}
		_, events, _ := s.agentSessions.get(anyToString(session["id"]))
		rollup := buildAgentSessionRollup(session, events, time.Now().UTC())
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "session": session, "event": event, "rollup": rollup})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

func (s *server) recordAgentSessionEvent(sessionID string, eventType string, payload map[string]any) map[string]any {
	if s == nil || s.agentSessions == nil {
		return nil
	}
	payload = cloneAnyMap(payload)
	payload["type"] = eventType
	session, _, err := s.agentSessions.appendEvent(sessionID, payload)
	if err != nil {
		return nil
	}
	return session
}
