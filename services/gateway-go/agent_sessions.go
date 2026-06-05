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

	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	defaultAgentSessionsPathRel       = ".data/orchestrator/agent_sessions.json"
	defaultAgentSessionMaxRecords     = 512
	defaultAgentSessionMaxEvents      = 256
	defaultAgentSessionMaxMetadataMap = 48
)

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
	case "active", "running", "started", "open":
		return "active"
	case "completed", "complete", "succeeded", "success":
		return "completed"
	case "failed", "error":
		return "failed"
	case "blocked":
		return "blocked"
	case "paused", "waiting":
		return "paused"
	case "canceled", "cancelled":
		return "canceled"
	default:
		return "active"
	}
}

func agentSessionTerminal(status string) bool {
	switch normalizeAgentSessionStatus(status) {
	case "completed", "failed", "blocked", "canceled":
		return true
	default:
		return false
	}
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

func normalizeAgentSessionStart(payload map[string]any, fallbackID string) map[string]any {
	now := nowUTCISO()
	sessionID := strings.TrimSpace(firstNonEmptyStrings(
		anyToString(payload["session_id"]),
		anyToString(payload["sessionId"]),
		anyToString(payload["id"]),
		fallbackID,
	))
	if sessionID == "" {
		sessionID = "sess_" + primitive.NewObjectID().Hex()
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
		"cwd":                 clipText(strings.TrimSpace(anyToString(payload["cwd"])), 320),
		"objective":           clipText(strings.TrimSpace(anyToString(payload["objective"])), 1200),
		"mission":             clipText(strings.TrimSpace(anyToString(payload["mission"])), 1200),
		"goal":                clipText(strings.TrimSpace(anyToString(payload["goal"])), 1200),
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
		eventID = "evt_" + primitive.NewObjectID().Hex()
	}
	metadata := compactAgentSessionMetadata(anyMap(payload["metadata"]))
	for _, key := range []string{"source_coverage", "retrieval", "graph", "dream", "tests", "pr", "pull_request", "handoff"} {
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
		sessionID = "sess_" + primitive.NewObjectID().Hex()
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
	switch eventType {
	case "session.completed", "agent.session.completed":
		session["status"] = "completed"
		session["completed_at"] = createdAt
	case "session.failed", "agent.session.failed":
		session["status"] = "failed"
		session["completed_at"] = createdAt
	case "session.blocked", "agent.session.blocked":
		session["status"] = "blocked"
		session["completed_at"] = createdAt
	case "session.canceled", "agent.session.canceled":
		session["status"] = "canceled"
		session["completed_at"] = createdAt
	default:
		if !agentSessionTerminal(anyToString(session["status"])) {
			session["status"] = "active"
		}
	}
	session["memory_contribution"] = bumpContribution(
		anyMap(session["memory_contribution"]),
		eventType,
		anyMap(event["metadata"]),
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
		rows = append(rows, cloneAnyMap(row))
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
			"repo":   session["repo"],
			"branch": session["branch"],
			"cwd":    session["cwd"],
			"tags":   session["tags"],
		},
	})
	if eventErr == nil {
		session, _, _ = s.agentSessions.get(anyToString(session["id"]))
	}
	response := map[string]any{"ok": true, "session": session}
	if startedEvent != nil {
		response["event"] = startedEvent
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
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "session": session, "events": events})
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
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "session": session, "events": events})
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
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "session": session, "event": event})
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
