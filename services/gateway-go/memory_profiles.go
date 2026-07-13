package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type memoryProfileStore struct {
	path           string
	mu             sync.RWMutex
	profiles       map[string]map[string]any
	defaultProfile map[string]any
	allowedSources map[string]struct{}
}

func newMemoryProfileStore(policy retrievalPolicy) *memoryProfileStore {
	path := resolveStoragePath(
		"AGENT_MEMORY_PROFILE_PATH",
		filepath.Join("services", "orchestrator", "data", "agent_memory_profiles.json"),
	)
	defaultProfile := buildDefaultMemoryProfile(policy)
	allowed := map[string]struct{}{}
	sourcePool := append([]string{}, policy.defaultSources...)
	sourcePool = append(sourcePool, policy.fastSources...)
	sourcePool = append(sourcePool, policy.slowSources...)
	if len(sourcePool) == 0 {
		sourcePool = append(sourcePool, defaultAllSources...)
	}
	for _, source := range normalizeSourceList(sourcePool) {
		allowed[source] = struct{}{}
	}
	store := &memoryProfileStore{
		path:           path,
		profiles:       map[string]map[string]any{},
		defaultProfile: defaultProfile,
		allowedSources: allowed,
	}
	if err := prepareOwnerOnlyFile(path, strings.TrimSpace(os.Getenv("AGENT_MEMORY_PROFILE_PATH")) == ""); err == nil {
		store.load()
	}
	return store
}

func buildDefaultMemoryProfile(policy retrievalPolicy) map[string]any {
	sources := append([]string{}, policy.defaultSources...)
	if len(sources) == 0 {
		sources = append(sources, policy.fastSources...)
	}
	sources = normalizeSourceList(sources)
	if len(sources) == 0 {
		sources = []string{sourceQdrant}
	}
	escalateMinResults := envInt("ORCH_AGENT_RECALL_ESCALATE_MIN_RESULTS", 4)
	if escalateMinResults < 1 {
		escalateMinResults = 1
	}
	if escalateMinResults > 100 {
		escalateMinResults = 100
	}
	escalateMinTopScore := envFloat("ORCH_AGENT_RECALL_ESCALATE_MIN_TOP_SCORE", 0.72)
	if escalateMinTopScore < 0 {
		escalateMinTopScore = 0
	}
	if escalateMinTopScore > 1 {
		escalateMinTopScore = 1
	}
	return map[string]any{
		"retrieval_mode":         normalizeRetrievalMode(envStringAny("balanced", "ORCH_RETRIEVAL_MODE_DEFAULT")),
		"retrieval_intent":       retrievalIntentDefault(),
		"sources":                sources,
		"source_weights":         map[string]any{},
		"default_project":        nil,
		"topic_prefixes":         []string{},
		"auto_escalate":          true,
		"query_expansion":        true,
		"escalate_min_results":   escalateMinResults,
		"escalate_min_top_score": escalateMinTopScore,
	}
}

func normalizeMemoryProfileID(agentID string) string {
	text := strings.TrimSpace(strings.ToLower(agentID))
	if text == "" {
		return "default"
	}
	text = strings.ReplaceAll(text, " ", "_")
	sanitized := strings.Builder{}
	for _, r := range text {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '/' {
			sanitized.WriteRune(r)
			continue
		}
		sanitized.WriteRune('_')
	}
	text = strings.Trim(strings.TrimSpace(sanitized.String()), "/")
	if text == "" {
		return "default"
	}
	return text
}

func copyAnyMap(input map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range input {
		out[key] = value
	}
	return out
}

func normalizeRetrievalIntent(raw string, fallback string) string {
	candidate := strings.TrimSpace(strings.ToLower(raw))
	switch candidate {
	case "decision", "ops", "raw":
		return candidate
	default:
		if strings.TrimSpace(fallback) == "" {
			return "decision"
		}
		return strings.TrimSpace(strings.ToLower(fallback))
	}
}

func normalizeTopicPrefixes(values any) []string {
	items, ok := values.([]any)
	if !ok {
		if typed, okTyped := values.([]string); okTyped {
			items = make([]any, 0, len(typed))
			for _, value := range typed {
				items = append(items, value)
			}
		} else {
			return []string{}
		}
	}
	out := make([]string, 0, len(items))
	seen := map[string]struct{}{}
	for _, raw := range items {
		candidate := normalizeTopicPathCandidate(anyToString(raw))
		if candidate == "" {
			continue
		}
		if _, exists := seen[candidate]; exists {
			continue
		}
		seen[candidate] = struct{}{}
		out = append(out, candidate)
	}
	return out
}

func normalizeProfileSources(values any, fallback []string, allowed map[string]struct{}) []string {
	items, ok := values.([]any)
	if !ok {
		if typed, okTyped := values.([]string); okTyped {
			items = make([]any, 0, len(typed))
			for _, value := range typed {
				items = append(items, value)
			}
		}
	}
	if len(items) == 0 {
		out := append([]string{}, fallback...)
		return normalizeSourceList(out)
	}
	candidate := make([]string, 0, len(items))
	for _, raw := range items {
		candidate = append(candidate, anyToString(raw))
	}
	normalized := normalizeSourceList(candidate)
	if len(allowed) == 0 {
		if len(normalized) == 0 {
			return append([]string{}, fallback...)
		}
		return normalized
	}
	filtered := make([]string, 0, len(normalized))
	for _, source := range normalized {
		if _, ok := allowed[source]; ok {
			filtered = append(filtered, source)
		}
	}
	if len(filtered) == 0 {
		return append([]string{}, fallback...)
	}
	return filtered
}

func normalizeSourceWeights(values any, allowed map[string]struct{}) map[string]any {
	payload, ok := values.(map[string]any)
	if !ok {
		if typed, okTyped := values.(map[string]float64); okTyped {
			payload = map[string]any{}
			for key, value := range typed {
				payload[key] = value
			}
		} else {
			return map[string]any{}
		}
	}
	out := map[string]any{}
	for key, value := range payload {
		source := strings.TrimSpace(strings.ToLower(key))
		if source == "" {
			continue
		}
		if len(allowed) > 0 {
			if _, ok := allowed[source]; !ok {
				continue
			}
		}
		weight := anyToFloat64(value, 0)
		if weight < 0 {
			weight = 0
		}
		out[source] = weight
	}
	return out
}

func normalizeMemoryProfilePayload(payload map[string]any, base map[string]any, allowed map[string]struct{}) map[string]any {
	out := copyAnyMap(base)
	if payload == nil {
		return out
	}
	if raw, ok := payload["retrieval_mode"]; ok {
		out["retrieval_mode"] = normalizeRetrievalMode(anyToString(raw))
	}
	if raw, ok := payload["retrieval_intent"]; ok {
		out["retrieval_intent"] = normalizeRetrievalIntent(anyToString(raw), anyToString(out["retrieval_intent"]))
	}
	out["sources"] = normalizeProfileSources(payload["sources"], anyToStringSlice(out["sources"]), allowed)
	out["source_weights"] = normalizeSourceWeights(payload["source_weights"], allowed)
	if raw, ok := payload["default_project"]; ok {
		project := strings.TrimSpace(anyToString(raw))
		if project == "" {
			out["default_project"] = nil
		} else {
			out["default_project"] = project
		}
	}
	if raw, ok := payload["topic_prefixes"]; ok {
		out["topic_prefixes"] = normalizeTopicPrefixes(raw)
	}
	if raw, ok := payload["auto_escalate"]; ok {
		out["auto_escalate"] = anyToBool(raw)
	}
	if raw, ok := payload["query_expansion"]; ok {
		out["query_expansion"] = anyToBool(raw)
	}
	if raw, ok := payload["escalate_min_results"]; ok {
		value := anyToInt(raw, anyToInt(out["escalate_min_results"], 4))
		if value < 1 {
			value = 1
		}
		if value > 100 {
			value = 100
		}
		out["escalate_min_results"] = value
	}
	if raw, ok := payload["escalate_min_top_score"]; ok {
		value := anyToFloat64(raw, anyToFloat64(out["escalate_min_top_score"], 0.72))
		if value < 0 {
			value = 0
		}
		if value > 1 {
			value = 1
		}
		out["escalate_min_top_score"] = value
	}
	return out
}

func (s *memoryProfileStore) load() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.profiles = map[string]map[string]any{}
	s.profiles["default"] = copyAnyMap(s.defaultProfile)
	if strings.TrimSpace(s.path) == "" {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	payload := map[string]any{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return
	}
	candidates, ok := payload["profiles"].(map[string]any)
	if !ok {
		candidates = payload
	}
	for key, value := range candidates {
		agentID := normalizeMemoryProfileID(key)
		row, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if agentID == "default" {
			s.profiles["default"] = normalizeMemoryProfilePayload(row, s.defaultProfile, s.allowedSources)
			continue
		}
		base := s.profiles["default"]
		s.profiles[agentID] = normalizeMemoryProfilePayload(row, base, s.allowedSources)
	}
}

func (s *memoryProfileStore) persistLocked() error {
	if strings.TrimSpace(s.path) == "" {
		return nil
	}
	snapshot := map[string]any{"profiles": map[string]any{}}
	profiles := snapshot["profiles"].(map[string]any)
	keys := make([]string, 0, len(s.profiles))
	for key := range s.profiles {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		profiles[key] = copyAnyMap(s.profiles[key])
	}
	encoded, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	return writeOwnerOnlyAtomicFile(s.path, encoded, false)
}

func (s *memoryProfileStore) list() map[string]map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]map[string]any{}
	for key, value := range s.profiles {
		out[key] = copyAnyMap(value)
	}
	return out
}

func (s *memoryProfileStore) resolve(agentID string) (map[string]any, bool) {
	normalized := normalizeMemoryProfileID(agentID)
	s.mu.RLock()
	defer s.mu.RUnlock()
	defaultProfile := s.profiles["default"]
	resolved := copyAnyMap(defaultProfile)
	stored, exists := s.profiles[normalized]
	if normalized != "default" && exists {
		resolved = normalizeMemoryProfilePayload(stored, resolved, s.allowedSources)
	}
	if normalized == "default" {
		exists = true
	}
	return resolved, exists
}

func (s *memoryProfileStore) upsert(agentID string, payload map[string]any) (string, map[string]any, error) {
	normalized := normalizeMemoryProfileID(agentID)
	s.mu.Lock()
	defer s.mu.Unlock()
	base := s.profiles["default"]
	if normalized != "default" {
		if existing, ok := s.profiles[normalized]; ok {
			base = normalizeMemoryProfilePayload(existing, base, s.allowedSources)
		}
	}
	normalizedProfile := normalizeMemoryProfilePayload(payload, base, s.allowedSources)
	s.profiles[normalized] = normalizedProfile
	if normalized == "default" {
		s.profiles["default"] = normalizedProfile
	}
	if err := s.persistLocked(); err != nil {
		return normalized, nil, err
	}
	resolved := copyAnyMap(normalizedProfile)
	return normalized, resolved, nil
}

func (s *memoryProfileStore) delete(agentID string) (string, error) {
	normalized := normalizeMemoryProfileID(agentID)
	if normalized == "default" {
		return normalized, errBadRequest("default profile cannot be deleted")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.profiles[normalized]; !ok {
		return normalized, errNotFound("agent profile not found")
	}
	delete(s.profiles, normalized)
	if err := s.persistLocked(); err != nil {
		return normalized, err
	}
	return normalized, nil
}

type profileAPIError struct {
	status int
	msg    string
}

func (e profileAPIError) Error() string {
	return e.msg
}

func errBadRequest(msg string) error {
	return profileAPIError{status: http.StatusBadRequest, msg: msg}
}

func errNotFound(msg string) error {
	return profileAPIError{status: http.StatusNotFound, msg: msg}
}

func (s *server) memoryProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	profiles := s.memoryProfilesStore.list()
	writeJSON(w, http.StatusOK, map[string]any{
		"store_ref": ownerOnlyStoreRef("memory_profiles"),
		"profiles":  profiles,
		"count":     len(profiles),
	})
}

func (s *server) memoryProfilesByID(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	agentID := strings.Trim(strings.TrimPrefix(r.URL.Path, "/memory/profiles/"), "/")
	if agentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "agent id is required"})
		return
	}
	normalized := normalizeMemoryProfileID(agentID)
	switch r.Method {
	case http.MethodGet:
		profile, exists := s.memoryProfilesStore.resolve(normalized)
		writeJSON(w, http.StatusOK, map[string]any{
			"agent_id": normalized,
			"exists":   exists,
			"profile":  profile,
		})
		return
	case http.MethodPut:
		raw, err := readRequestBody(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
			return
		}
		payload := map[string]any{}
		if strings.TrimSpace(string(raw)) != "" {
			if err := json.Unmarshal(raw, &payload); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
				return
			}
		}
		resolvedID, profile, err := s.memoryProfilesStore.upsert(normalized, payload)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to persist profile", "detail": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "agent_id": resolvedID, "profile": profile})
		return
	case http.MethodDelete:
		resolvedID, err := s.memoryProfilesStore.delete(normalized)
		if err != nil {
			if apiErr, ok := err.(profileAPIError); ok {
				writeJSON(w, apiErr.status, map[string]any{"error": apiErr.msg})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to delete profile", "detail": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": resolvedID})
		return
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}
