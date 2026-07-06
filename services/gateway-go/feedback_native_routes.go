package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

const defaultFeedbackHistoryRelPath = ".data/orchestrator/feedback_records.ndjson"

var feedbackTagPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9:_./-]*$`)

type feedbackIdempotencyEntry struct {
	seenAt      time.Time
	fingerprint string
	result      map[string]any
}

type feedbackStore struct {
	mu               sync.Mutex
	path             string
	maxKeep          int
	records          []map[string]any
	idempotency      map[string]feedbackIdempotencyEntry
	idempotencyOrder []string
	metrics          map[string]int
}

func feedbackHistoryPath() string {
	path := strings.TrimSpace(os.Getenv("FEEDBACK_HISTORY_PATH"))
	if path == "" {
		path = defaultFeedbackHistoryRelPath
	}
	return filepath.Clean(path)
}

func newFeedbackStoreFromEnv() (*feedbackStore, error) {
	store := &feedbackStore{
		path:             feedbackHistoryPath(),
		maxKeep:          maxInt(500, envInt("GO_FEEDBACK_MAX_IN_MEMORY", 5000)),
		records:          make([]map[string]any, 0, 256),
		idempotency:      map[string]feedbackIdempotencyEntry{},
		idempotencyOrder: []string{},
		metrics: map[string]int{
			"accepted":       0,
			"rejected":       0,
			"persisted":      0,
			"persistFailed":  0,
			"idempotentHits": 0,
		},
	}
	if err := store.load(); err != nil {
		return store, err
	}
	return store, nil
}

func (s *feedbackStore) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(s.path) == "" {
		return nil
	}
	file, err := os.Open(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 2*1024*1024)
	loaded := make([]map[string]any, 0, 256)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		loaded = append(loaded, row)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(loaded) > s.maxKeep {
		loaded = loaded[len(loaded)-s.maxKeep:]
	}
	s.records = loaded
	return nil
}

func (s *feedbackStore) append(record map[string]any) error {
	if s == nil {
		return errors.New("feedback store unavailable")
	}
	if strings.TrimSpace(s.path) == "" {
		return errors.New("feedback store path is empty")
	}
	line, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	s.mu.Lock()
	s.records = append(s.records, cloneAnyMap(record))
	if len(s.records) > s.maxKeep {
		s.records = s.records[len(s.records)-s.maxKeep:]
	}
	s.mu.Unlock()
	return nil
}

func (s *feedbackStore) list(project string, userID string, source string, limit int) []map[string]any {
	if s == nil {
		return []map[string]any{}
	}
	limit = clampInt(limit, 1, 500)
	project = strings.TrimSpace(project)
	userID = strings.TrimSpace(userID)
	source = strings.TrimSpace(strings.ToLower(source))
	s.mu.Lock()
	rows := make([]map[string]any, 0, len(s.records))
	for i := len(s.records) - 1; i >= 0; i-- {
		row := s.records[i]
		if project != "" && !strings.EqualFold(strings.TrimSpace(anyToString(row["project"])), project) {
			continue
		}
		if userID != "" && !strings.EqualFold(strings.TrimSpace(anyToString(row["user_id"])), userID) {
			continue
		}
		if source != "" && !strings.EqualFold(strings.TrimSpace(anyToString(row["source"])), source) {
			continue
		}
		rows = append(rows, cloneAnyMap(row))
		if len(rows) >= limit {
			break
		}
	}
	s.mu.Unlock()
	return rows
}

func feedbackSubmitIdempotencyTTL() time.Duration {
	secs := envInt("FEEDBACK_SUBMIT_IDEMPOTENCY_TTL_SECS", 900)
	if secs < 30 {
		secs = 30
	}
	return time.Duration(secs) * time.Second
}

func feedbackSubmitMaxIdempotencyKeys() int {
	return maxInt(128, envInt("MEMORY_WRITE_DEDUP_MAX_KEYS", 10000))
}

func (s *feedbackStore) recordMetric(name string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.metrics == nil {
		s.metrics = map[string]int{}
	}
	s.metrics[name] = s.metrics[name] + 1
	s.mu.Unlock()
}

func (s *feedbackStore) metricsSnapshot() map[string]any {
	if s == nil {
		return map[string]any{
			"accepted":       0,
			"rejected":       0,
			"persisted":      0,
			"persistFailed":  0,
			"idempotentHits": 0,
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]any{
		"accepted":       0,
		"rejected":       0,
		"persisted":      0,
		"persistFailed":  0,
		"idempotentHits": 0,
	}
	for key, value := range s.metrics {
		out[key] = value
	}
	return out
}

func (s *feedbackStore) feedbackSubmitStatus() map[string]any {
	return map[string]any{
		"idempotencyTtlSecs": int(feedbackSubmitIdempotencyTTL().Seconds()),
		"metrics":            s.metricsSnapshot(),
	}
}

func feedbackSubmitCacheKey(raw string) string {
	normalized := strings.TrimSpace(raw)
	if normalized == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("feedback:" + normalized))
	return "feedback:" + hex.EncodeToString(sum[:])
}

func feedbackSubmitFingerprint(payload map[string]any) string {
	encoded, err := json.Marshal(payload)
	if err != nil {
		sum := sha256.Sum256([]byte(nowUTCISO()))
		return hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func (s *feedbackStore) feedbackSubmitIdempotencyLookup(key string, fingerprint string) (map[string]any, bool) {
	if s == nil || strings.TrimSpace(key) == "" {
		return nil, false
	}
	now := time.Now()
	ttl := feedbackSubmitIdempotencyTTL()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idempotency == nil {
		s.idempotency = map[string]feedbackIdempotencyEntry{}
	}
	if len(s.idempotency) > 0 {
		filteredOrder := make([]string, 0, len(s.idempotencyOrder))
		for _, cacheKey := range s.idempotencyOrder {
			entry, ok := s.idempotency[cacheKey]
			if !ok {
				continue
			}
			if entry.seenAt.Add(ttl).Before(now) {
				delete(s.idempotency, cacheKey)
				continue
			}
			filteredOrder = append(filteredOrder, cacheKey)
		}
		s.idempotencyOrder = filteredOrder
	}
	entry, ok := s.idempotency[key]
	if !ok {
		return nil, false
	}
	s.idempotencyOrder = moveStringToEnd(s.idempotencyOrder, key)
	if strings.TrimSpace(entry.fingerprint) != strings.TrimSpace(fingerprint) {
		return nil, true
	}
	replay := cloneJSONMap(entry.result)
	replay["idempotentReplay"] = true
	replay["idempotencyScope"] = "request"
	return replay, false
}

func (s *feedbackStore) feedbackSubmitIdempotencyStore(key string, fingerprint string, result map[string]any) {
	if s == nil || strings.TrimSpace(key) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idempotency == nil {
		s.idempotency = map[string]feedbackIdempotencyEntry{}
	}
	if _, exists := s.idempotency[key]; !exists {
		s.idempotencyOrder = append(s.idempotencyOrder, key)
	} else {
		s.idempotencyOrder = moveStringToEnd(s.idempotencyOrder, key)
	}
	s.idempotency[key] = feedbackIdempotencyEntry{
		seenAt:      time.Now(),
		fingerprint: fingerprint,
		result:      cloneJSONMap(result),
	}
	maxKeys := feedbackSubmitMaxIdempotencyKeys()
	for len(s.idempotencyOrder) > maxKeys {
		oldest := s.idempotencyOrder[0]
		s.idempotencyOrder = s.idempotencyOrder[1:]
		delete(s.idempotency, oldest)
	}
}

func moveStringToEnd(values []string, target string) []string {
	out := values[:0]
	found := false
	for _, value := range values {
		if value == target {
			found = true
			continue
		}
		out = append(out, value)
	}
	if found {
		out = append(out, target)
	}
	return out
}

func nullableFeedbackString(value string) any {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	return trimmed
}

func feedbackNormalizeTopicPath(value string) string {
	return strings.Trim(strings.ToLower(strings.TrimSpace(value)), "/")
}

func feedbackLearningEnabled() bool {
	return envBool("LEARNING_LOOP_ENABLED", true)
}

func feedbackPreferenceMaxEntries() int {
	return clampInt(envInt("PREFERENCE_MAX_ENTRIES", 25), 1, 500)
}

func feedbackMaxContent() int {
	return maxInt(1, envInt("FEEDBACK_MAX_CONTENT", 2000))
}

func feedbackTagMaxCount() int {
	return maxInt(1, envInt("FEEDBACK_TAG_MAX_COUNT", 16))
}

func feedbackTagMaxLength() int {
	return maxInt(8, envInt("FEEDBACK_TAG_MAX_LENGTH", 64))
}

func feedbackMetadataMaxKeys() int {
	return maxInt(1, envInt("FEEDBACK_METADATA_MAX_KEYS", 32))
}

func feedbackMetadataMaxBytes() int {
	return maxInt(128, envInt("FEEDBACK_METADATA_MAX_BYTES", 4096))
}

func normalizeFeedbackRating(payload map[string]any) (any, int, string) {
	raw, exists := payload["rating"]
	if !exists || raw == nil {
		return nil, 0, ""
	}
	rating := anyToInt(raw, -1)
	if rating < 1 || rating > 5 {
		return nil, http.StatusUnprocessableEntity, "rating must be between 1 and 5"
	}
	return rating, 0, ""
}

func normalizeFeedbackSource(raw any, defaultSource string) (string, int, string) {
	source := strings.TrimSpace(strings.ToLower(anyToString(raw)))
	if source == "" {
		source = strings.TrimSpace(strings.ToLower(defaultSource))
	}
	if source == "" {
		source = "agent"
	}
	switch source {
	case "user", "agent", "system":
		return source, 0, ""
	default:
		return "", http.StatusUnprocessableEntity, "source must be one of: user, agent, system"
	}
}

func normalizeFeedbackSentiment(raw any) (any, int, string) {
	sentiment := strings.TrimSpace(strings.ToLower(anyToString(raw)))
	if sentiment == "" {
		return nil, 0, ""
	}
	switch sentiment {
	case "good", "liked":
		sentiment = "positive"
	case "bad", "disliked":
		sentiment = "negative"
	}
	switch sentiment {
	case "positive", "neutral", "negative":
		return sentiment, 0, ""
	default:
		return nil, http.StatusUnprocessableEntity, "sentiment must be one of: positive, neutral, negative"
	}
}

func normalizeFeedbackTags(raw any) (any, int, string) {
	if raw == nil {
		return nil, 0, ""
	}
	var items []any
	switch typed := raw.(type) {
	case []any:
		items = typed
	case []string:
		items = make([]any, 0, len(typed))
		for _, item := range typed {
			items = append(items, item)
		}
	default:
		return nil, http.StatusUnprocessableEntity, "tags must be a list"
	}
	seen := map[string]struct{}{}
	tags := make([]string, 0, len(items))
	maxLength := feedbackTagMaxLength()
	maxCount := feedbackTagMaxCount()
	for _, item := range items {
		tag := strings.TrimSpace(strings.ToLower(anyToString(item)))
		if tag == "" {
			continue
		}
		if len([]rune(tag)) > maxLength {
			return nil, http.StatusUnprocessableEntity, "tag '" + clipText(tag, 24) + "' exceeds FEEDBACK_TAG_MAX_LENGTH=" + strconv.Itoa(maxLength)
		}
		if !feedbackTagPattern.MatchString(tag) {
			return nil, http.StatusUnprocessableEntity, "tag '" + tag + "' is malformed"
		}
		if _, exists := seen[tag]; exists {
			continue
		}
		seen[tag] = struct{}{}
		tags = append(tags, tag)
		if len(tags) > maxCount {
			return nil, http.StatusUnprocessableEntity, "tags exceed FEEDBACK_TAG_MAX_COUNT=" + strconv.Itoa(maxCount)
		}
	}
	if len(tags) == 0 {
		return nil, 0, ""
	}
	return tags, 0, ""
}

func normalizeFeedbackMetadata(raw any) (any, int, string) {
	if raw == nil {
		return nil, 0, ""
	}
	rawMap, ok := raw.(map[string]any)
	if !ok {
		return nil, http.StatusUnprocessableEntity, "metadata must be an object"
	}
	if len(rawMap) > feedbackMetadataMaxKeys() {
		return nil, http.StatusUnprocessableEntity, "metadata exceeds FEEDBACK_METADATA_MAX_KEYS=" + strconv.Itoa(feedbackMetadataMaxKeys())
	}
	metadata := map[string]any{}
	for key, value := range rawMap {
		token := strings.TrimSpace(key)
		if token == "" {
			continue
		}
		metadata[token] = value
	}
	if len(metadata) == 0 {
		return nil, 0, ""
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return nil, http.StatusUnprocessableEntity, "metadata must be json serializable"
	}
	if len(encoded) > feedbackMetadataMaxBytes() {
		return nil, http.StatusUnprocessableEntity, "metadata exceeds FEEDBACK_METADATA_MAX_BYTES=" + strconv.Itoa(feedbackMetadataMaxBytes())
	}
	return metadata, 0, ""
}

func normalizeFeedbackPayload(payload map[string]any, defaultSource string) (map[string]any, int, string) {
	if payload == nil {
		payload = map[string]any{}
	}
	source, status, message := normalizeFeedbackSource(payload["source"], defaultSource)
	if status != 0 {
		return nil, status, message
	}
	rating, status, message := normalizeFeedbackRating(payload)
	if status != 0 {
		return nil, status, message
	}
	sentiment, status, message := normalizeFeedbackSentiment(payload["sentiment"])
	if status != 0 {
		return nil, status, message
	}
	tags, status, message := normalizeFeedbackTags(payload["tags"])
	if status != 0 {
		return nil, status, message
	}
	metadata, status, message := normalizeFeedbackMetadata(payload["metadata"])
	if status != 0 {
		return nil, status, message
	}
	content := strings.TrimSpace(anyToString(payload["content"]))
	if len([]rune(content)) > feedbackMaxContent() {
		return nil, http.StatusUnprocessableEntity, "content exceeds FEEDBACK_MAX_CONTENT=" + strconv.Itoa(feedbackMaxContent())
	}
	topicPath := feedbackNormalizeTopicPath(anyToString(payload["topic_path"]))
	normalized := map[string]any{
		"project":    nullableFeedbackString(anyToString(payload["project"])),
		"user_id":    nullableFeedbackString(anyToString(payload["user_id"])),
		"source":     source,
		"task_id":    nullableFeedbackString(anyToString(payload["task_id"])),
		"rating":     rating,
		"sentiment":  sentiment,
		"tags":       tags,
		"content":    nullableFeedbackString(content),
		"topic_path": nullableFeedbackString(topicPath),
		"metadata":   metadata,
	}
	if !hasFeedbackSignal(normalized) {
		return nil, http.StatusUnprocessableEntity, "feedback_submit requires at least one of rating/sentiment/tags/content/metadata"
	}
	return normalized, 0, ""
}

func (s *server) buildFeedbackRecord(normalized map[string]any) map[string]any {
	now := nowUTCISO()
	record := map[string]any{
		"id":         bson.NewObjectID().Hex(),
		"created_at": now,
		"updated_at": now,
		"project":    normalized["project"],
		"user_id":    normalized["user_id"],
		"source":     normalized["source"],
		"task_id":    normalized["task_id"],
		"rating":     normalized["rating"],
		"sentiment":  normalized["sentiment"],
		"tags":       normalized["tags"],
		"content":    normalized["content"],
		"topic_path": normalized["topic_path"],
		"metadata":   normalized["metadata"],
	}
	return record
}

func (s *server) buildPreferenceContext(records []map[string]any) map[string]any {
	positive := []string{}
	negative := []string{}
	notes := []string{}
	for _, entry := range records {
		content := strings.TrimSpace(anyToString(entry["content"]))
		line := content
		if line == "" && entry["metadata"] != nil {
			encoded, _ := json.Marshal(entry["metadata"])
			line = strings.TrimSpace(string(encoded))
		}
		if line == "" {
			continue
		}
		source := firstNonEmptyStrings(anyToString(entry["source"]), "user")
		topicPath := strings.TrimSpace(anyToString(entry["topic_path"]))
		tags := anyToStringSlice(entry["tags"])
		rendered := line + " (source: " + source + ")"
		if topicPath != "" {
			rendered += " [topic: " + topicPath + "]"
		}
		if len(tags) > 0 {
			rendered += " [tags: " + strings.Join(tags, ", ") + "]"
		}
		rating := anyToInt(entry["rating"], 0)
		sentiment := strings.TrimSpace(strings.ToLower(anyToString(entry["sentiment"])))
		switch {
		case rating >= 4:
			positive = append(positive, rendered)
		case rating > 0 && rating <= 2:
			negative = append(negative, rendered)
		case sentiment == "positive":
			positive = append(positive, rendered)
		case sentiment == "negative":
			negative = append(negative, rendered)
		default:
			notes = append(notes, rendered)
		}
	}
	lines := []string{}
	if len(positive) > 0 {
		lines = append(lines, "Positive preferences:")
		for _, item := range positive {
			lines = append(lines, "- "+item)
		}
	}
	if len(negative) > 0 {
		lines = append(lines, "Avoid or dislike:")
		for _, item := range negative {
			lines = append(lines, "- "+item)
		}
	}
	if len(notes) > 0 {
		lines = append(lines, "Notes:")
		for _, item := range notes {
			lines = append(lines, "- "+item)
		}
	}
	return map[string]any{
		"positive":   positive,
		"negative":   negative,
		"notes":      notes,
		"context":    strings.TrimSpace(strings.Join(lines, "\n")),
		"total":      len(records),
		"updated_at": nowUTCISO(),
	}
}

func (s *server) feedbackRoute(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
			return
		}
		project := strings.TrimSpace(r.URL.Query().Get("project"))
		userID := strings.TrimSpace(r.URL.Query().Get("user_id"))
		source := strings.TrimSpace(r.URL.Query().Get("source"))
		limit := clampInt(anyToInt(r.URL.Query().Get("limit"), 50), 1, 500)
		feedback := []map[string]any{}
		if s.feedbackStore != nil {
			feedback = s.feedbackStore.list(project, userID, source, limit)
		}
		writeJSON(w, http.StatusOK, map[string]any{"feedback": feedback})
	case http.MethodPost:
		if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
			return
		}
		bodyBytes, err := readRequestBody(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
			return
		}
		payload, err := parseJSONMap(bodyBytes)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
			return
		}
		normalized, status, message := normalizeFeedbackPayload(payload, "user")
		if status != 0 {
			if s.feedbackStore != nil {
				s.feedbackStore.recordMetric("rejected")
			}
			writeJSON(w, status, map[string]any{"error": message})
			return
		}
		record := s.buildFeedbackRecord(normalized)
		if s.feedbackStore != nil {
			if err := s.feedbackStore.append(record); err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]any{"error": "feedback_store_unavailable", "detail": err.Error()})
				return
			}
			s.feedbackStore.recordMetric("accepted")
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"feedback": record,
			"learning": map[string]any{
				"enabled":       feedbackLearningEnabled(),
				"memoryIndexed": false,
			},
		})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

func (s *server) preferencesRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	if !feedbackLearningEnabled() || s.feedbackStore == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": false,
			"preferences": map[string]any{
				"total":      0,
				"positive":   []any{},
				"negative":   []any{},
				"notes":      []any{},
				"updated_at": nil,
			},
			"reason": "go_runtime_preferences_not_enabled",
		})
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	userID := strings.TrimSpace(r.URL.Query().Get("user_id"))
	records := s.feedbackStore.list(project, userID, "", feedbackPreferenceMaxEntries())
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":     true,
		"preferences": s.buildPreferenceContext(records),
		"runtime":     sourceOwnerGoNative,
	})
}

func (s *server) toolsFeedbackSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareToolHeaders(w, r, "/tools/feedback_submit"); !ok {
		return
	}
	bodyBytes, err := readRequestBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
		return
	}
	payload := map[string]any{}
	if strings.TrimSpace(string(bodyBytes)) != "" {
		payload, err = parseJSONMap(bodyBytes)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
			return
		}
	}
	normalized, status, message := normalizeFeedbackPayload(payload, "agent")
	if status != 0 {
		if s.feedbackStore != nil {
			s.feedbackStore.recordMetric("rejected")
		}
		writeJSON(w, status, map[string]any{"error": message})
		return
	}
	fingerprint := feedbackSubmitFingerprint(normalized)
	idempotencyKey := feedbackSubmitCacheKey(anyToString(payload["idempotencyKey"]))
	if idempotencyKey != "" && s.feedbackStore != nil {
		replayed, mismatch := s.feedbackStore.feedbackSubmitIdempotencyLookup(idempotencyKey, fingerprint)
		if mismatch {
			s.feedbackStore.recordMetric("rejected")
			writeJSON(w, http.StatusConflict, map[string]any{"error": "idempotencyKey was already used with a different payload"})
			return
		}
		if replayed != nil {
			s.feedbackStore.recordMetric("idempotentHits")
			writeJSON(w, http.StatusOK, replayed)
			return
		}
	}
	record := s.buildFeedbackRecord(normalized)
	if s.feedbackStore != nil {
		if err := s.feedbackStore.append(record); err != nil {
			s.feedbackStore.recordMetric("persistFailed")
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "feedback_store_unavailable", "detail": err.Error()})
			return
		}
		s.feedbackStore.recordMetric("accepted")
	}
	preferences := map[string]any(nil)
	preferenceUpdated := false
	if feedbackLearningEnabled() && anyToBoolOrDefault(firstPresent(payload, "include_preferences", "includePreferences"), true) && s.feedbackStore != nil {
		project := anyToString(normalized["project"])
		userID := anyToString(normalized["user_id"])
		preferences = s.buildPreferenceContext(s.feedbackStore.list(project, userID, "", feedbackPreferenceMaxEntries()))
		preferenceUpdated = true
	}
	response := map[string]any{
		"ok":       true,
		"feedback": record,
		"learning": map[string]any{
			"enabled":           feedbackLearningEnabled(),
			"preferenceUpdated": preferenceUpdated,
			"memoryIndexed":     false,
		},
	}
	if preferences != nil {
		response["preferences"] = preferences
	}
	if s.feedbackStore != nil {
		s.feedbackStore.feedbackSubmitIdempotencyStore(idempotencyKey, fingerprint, response)
	}
	writeJSON(w, http.StatusOK, response)
}

func hasFeedbackSignal(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	if rating := anyToInt(payload["rating"], 0); rating >= 1 && rating <= 5 {
		return true
	}
	if strings.TrimSpace(anyToString(payload["sentiment"])) != "" {
		return true
	}
	if len(anyToStringSlice(payload["tags"])) > 0 {
		return true
	}
	if strings.TrimSpace(anyToString(payload["content"])) != "" {
		return true
	}
	if len(anyMap(payload["metadata"])) > 0 {
		return true
	}
	return false
}
