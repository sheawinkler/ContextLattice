package main

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var browserSlugPattern = regexp.MustCompile(`[^a-z0-9]+`)

func slugToken(value string, fallback string, maxLen int) string {
	lowered := strings.TrimSpace(strings.ToLower(value))
	if lowered == "" {
		return fallback
	}
	slug := browserSlugPattern.ReplaceAllString(lowered, "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		return fallback
	}
	limit := maxLen
	if limit < 8 {
		limit = 8
	}
	if len(slug) > limit {
		return slug[:limit]
	}
	return slug
}

func normalizeBrowserTopicPath(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", true
	}
	trimmed = strings.ReplaceAll(trimmed, "\\", "/")
	parts := strings.Split(trimmed, "/")
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		token := strings.TrimSpace(part)
		if token == "" || token == "." {
			continue
		}
		if token == ".." {
			return "", false
		}
		clean = append(clean, token)
	}
	if len(clean) == 0 {
		return "", true
	}
	return strings.Join(clean, "/"), true
}

func buildBrowserContextFileName(pageURL string, title string) string {
	parsed, _ := url.Parse(strings.TrimSpace(pageURL))
	host := slugToken(parsed.Host, "unknown-host", 48)
	titleToken := slugToken(title, "snapshot", 64)
	timestamp := time.Now().UTC().Format("20060102T150405Z")
	return "browser/" + host + "/" + timestamp + "_" + titleToken + ".md"
}

func (s *server) memoryBrowserContext(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	enabled := envBoolAny(true, "GO_BROWSER_CONTEXT_INGEST_ENABLED", "ORCH_BROWSER_CONTEXT_INGEST_ENABLED")
	if !enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "browser context ingest is disabled"})
		return
	}
	incomingHeaders, ok := s.prepareAuthorizedHeaders(w, r)
	if !ok {
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
	projectName := strings.TrimSpace(anyToString(payload["projectName"]))
	if projectName == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "projectName is required"})
		return
	}
	pageURL := strings.TrimSpace(anyToString(payload["pageUrl"]))
	if pageURL == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "pageUrl is required"})
		return
	}
	textSnapshot := strings.TrimSpace(anyToString(payload["textSnapshot"]))
	if textSnapshot == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "textSnapshot is required"})
		return
	}
	maxChars := anyToInt(payload["maxChars"], 12000)
	if maxChars < 200 {
		maxChars = 200
	}
	if maxChars > 40000 {
		maxChars = 40000
	}
	if len(textSnapshot) > maxChars {
		textSnapshot = textSnapshot[:maxChars]
	}

	title := strings.TrimSpace(anyToString(payload["title"]))
	agentID := strings.TrimSpace(anyToString(payload["agentId"]))
	normalizedTopicPath, validTopicPath := normalizeBrowserTopicPath(anyToString(payload["topicPath"]))
	if !validTopicPath {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "invalid topicPath"})
		return
	}
	if normalizedTopicPath == "" {
		normalizedTopicPath = "browser/context"
	}
	fileName := buildBrowserContextFileName(pageURL, title)
	parsedURL, _ := url.Parse(pageURL)
	host := strings.TrimSpace(parsedURL.Host)
	titleHeader := title
	if titleHeader == "" {
		if host != "" {
			titleHeader = host
		} else {
			titleHeader = "untitled"
		}
	}
	lines := []string{
		"# Browser Context Snapshot: " + titleHeader,
		"",
		"- url: " + pageURL,
		"- host: " + strings.TrimSpace(host),
		"- captured_at: " + nowUTCISO(),
	}
	if strings.TrimSpace(host) == "" {
		lines[3] = "- host: unknown"
	}
	if agentID != "" {
		lines = append(lines, "- agent_id: "+agentID)
	}
	lines = append(lines, "", "## Snapshot", "", textSnapshot)
	content := strings.Join(lines, "\n")
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}

	writePayload := map[string]any{
		"projectName": projectName,
		"fileName":    fileName,
		"content":     content,
		"topicPath":   normalizedTopicPath,
	}
	var (
		response map[string]any
		status   int
	)
	if s.memoryStore != nil && s.memoryStore.isEnabled() {
		entry, deduped, storeErr := s.memoryStore.put(normalizedWrite{
			project:   projectName,
			fileName:  fileName,
			content:   content,
			topicPath: normalizedTopicPath,
		})
		if storeErr != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error":  "memory store write failed",
				"detail": storeErr.Error(),
			})
			return
		}
		status = http.StatusOK
		response = map[string]any{
			"ok":           true,
			"event_id":     entry.EventID,
			"source":       "go_memory_store",
			"deduped":      deduped,
			"content_hash": entry.ContentHash,
			"content_ref":  entry.ContentRef,
			"warnings":     []string{},
			"fanout": map[string]any{
				"go_memory_store": "succeeded",
			},
		}
	} else {
		backendResponse, backendStatus, backendErr := s.callBackendJSON(r.Context(), incomingHeaders, http.MethodPost, "/memory/write", writePayload)
		if backendErr != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error":      "backend unavailable",
				"detail":     backendErr.Error(),
				"backendUrl": s.backendURL,
			})
			return
		}
		response = backendResponse
		status = backendStatus
	}
	if response == nil {
		response = map[string]any{}
	}
	warnings := parseWarnings(response["warnings"])
	warnings = dedupeWarnings(append(warnings, "browser context persisted as text snapshot for retrieval/rerank"))
	response["warnings"] = warnings
	response["browserContext"] = map[string]any{
		"project":   projectName,
		"file":      fileName,
		"topicPath": normalizedTopicPath,
		"url":       pageURL,
	}
	writeJSON(w, status, response)
}
