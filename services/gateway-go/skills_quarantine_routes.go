package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type skillsQuarantineSearchRequest struct {
	Query     string
	Limit     int
	MinScore  string
	ShowTerms bool
	JSON      bool
}

func skillsQuarantineEnabled() bool {
	return envBool("ORCH_SKILLS_QUARANTINE_ENABLED", true)
}

func skillsQuarantineReindexEnabled() bool {
	return envBool("ORCH_SKILLS_QUARANTINE_REINDEX_ENABLED", false)
}

func skillsQuarantineSearchCommand() string {
	value := strings.TrimSpace(os.Getenv("ORCH_SKILLS_QUARANTINE_SEARCH_CMD"))
	if value == "" {
		value = "codex-skills-quarantine-search"
	}
	return value
}

func skillsQuarantineReindexCommand() string {
	value := strings.TrimSpace(os.Getenv("ORCH_SKILLS_QUARANTINE_REINDEX_CMD"))
	if value == "" {
		value = "codex-skills-quarantine-reindex"
	}
	return value
}

func skillsQuarantineTimeout() time.Duration {
	timeout := envDurationSeconds("ORCH_SKILLS_QUARANTINE_TIMEOUT_SECS", 8)
	if timeout < 500*time.Millisecond {
		return 500 * time.Millisecond
	}
	return timeout
}

func skillsQuarantineSearchDefaultLimit() int {
	return envInt("ORCH_SKILLS_QUARANTINE_DEFAULT_LIMIT", 20)
}

func skillsQuarantineSearchMaxLimit() int {
	return envInt("ORCH_SKILLS_QUARANTINE_MAX_LIMIT", 100)
}

func clampSkillsQuarantineLimit(value int) int {
	limit := value
	if limit <= 0 {
		limit = skillsQuarantineSearchDefaultLimit()
	}
	maxLimit := skillsQuarantineSearchMaxLimit()
	if maxLimit < 1 {
		maxLimit = 1
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	if limit < 1 {
		limit = 1
	}
	return limit
}

func parseSkillsQuarantineSearchRequest(r *http.Request) (skillsQuarantineSearchRequest, error) {
	queryValues := r.URL.Query()
	request := skillsQuarantineSearchRequest{
		Query:     strings.TrimSpace(queryValues.Get("query")),
		Limit:     parseOptionalIntQuery(queryValues.Get("limit"), skillsQuarantineSearchDefaultLimit(), 1, skillsQuarantineSearchMaxLimit()),
		MinScore:  strings.TrimSpace(queryValues.Get("min_score")),
		ShowTerms: parseOptionalBoolQuery(queryValues.Get("show_terms"), false),
		JSON:      parseOptionalBoolQuery(queryValues.Get("json"), true),
	}
	if request.MinScore != "" {
		if _, err := strconv.ParseFloat(request.MinScore, 64); err != nil {
			return skillsQuarantineSearchRequest{}, errors.New("min_score must be numeric")
		}
	}

	if r.Method == http.MethodPost {
		raw, err := readRequestBody(r)
		if err != nil {
			return skillsQuarantineSearchRequest{}, errors.New("failed to read request body")
		}
		if strings.TrimSpace(string(raw)) != "" {
			payload, err := parseJSONMap(raw)
			if err != nil {
				return skillsQuarantineSearchRequest{}, errors.New("invalid json")
			}
			if value := strings.TrimSpace(anyToString(payload["query"])); value != "" {
				request.Query = value
			}
			if _, ok := payload["limit"]; ok {
				request.Limit = anyToInt(payload["limit"], request.Limit)
			}
			if _, ok := payload["min_score"]; ok {
				request.MinScore = strings.TrimSpace(anyToString(payload["min_score"]))
				if request.MinScore != "" {
					if _, err := strconv.ParseFloat(request.MinScore, 64); err != nil {
						return skillsQuarantineSearchRequest{}, errors.New("min_score must be numeric")
					}
				}
			}
			if _, ok := payload["show_terms"]; ok {
				request.ShowTerms = anyToBool(payload["show_terms"])
			}
			if _, ok := payload["json"]; ok {
				request.JSON = anyToBool(payload["json"])
			}
		}
	}

	request.Query = strings.TrimSpace(request.Query)
	if request.Query == "" {
		return skillsQuarantineSearchRequest{}, errors.New("query is required")
	}
	request.Limit = clampSkillsQuarantineLimit(request.Limit)
	return request, nil
}

func runSkillsQuarantineCommand(ctx context.Context, command string, args []string) (map[string]any, int, error) {
	binaryPath, err := exec.LookPath(command)
	if err != nil {
		return nil, 0, err
	}
	execCmd := exec.CommandContext(ctx, binaryPath, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	execCmd.Stdout = &stdout
	execCmd.Stderr = &stderr

	start := time.Now()
	runErr := execCmd.Run()
	duration := time.Since(start)

	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	result := map[string]any{
		"ok":          runErr == nil,
		"command":     command,
		"resolved":    binaryPath,
		"args":        append([]string{}, args...),
		"duration_ms": roundFloat(float64(duration.Milliseconds()), 3),
		"exit_code":   exitCode,
		"stdout":      strings.TrimSpace(stdout.String()),
		"stderr":      strings.TrimSpace(stderr.String()),
	}
	if runErr != nil {
		result["error"] = runErr.Error()
	}
	return result, exitCode, runErr
}

func (s *server) skillsQuarantineSearchRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if !skillsQuarantineEnabled() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok":    false,
			"error": "skills_quarantine_disabled",
		})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	request, err := parseSkillsQuarantineSearchRequest(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":     false,
			"error":  "invalid_request",
			"detail": err.Error(),
		})
		return
	}

	args := []string{"--limit", strconv.Itoa(request.Limit)}
	if request.ShowTerms {
		args = append(args, "--show-terms")
	}
	if request.MinScore != "" {
		args = append(args, "--min-score", request.MinScore)
	}
	if request.JSON {
		args = append(args, "--json")
	}
	args = append(args, request.Query)

	ctx, cancel := context.WithTimeout(r.Context(), skillsQuarantineTimeout())
	defer cancel()

	result, _, runErr := runSkillsQuarantineCommand(ctx, skillsQuarantineSearchCommand(), args)
	if result == nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok":      false,
			"error":   "skills_quarantine_unavailable",
			"detail":  "command lookup failed",
			"command": skillsQuarantineSearchCommand(),
		})
		return
	}
	result["query"] = request.Query
	result["limit"] = request.Limit
	result["show_terms"] = request.ShowTerms
	result["min_score"] = request.MinScore
	result["json"] = request.JSON
	if request.JSON {
		var parsed any
		if err := json.Unmarshal([]byte(anyToString(result["stdout"])), &parsed); err == nil {
			result["parsed"] = parsed
		} else {
			result["parse_error"] = err.Error()
		}
	}
	if runErr != nil {
		writeJSON(w, http.StatusBadGateway, result)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) skillsQuarantineReindexRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if !skillsQuarantineEnabled() || !skillsQuarantineReindexEnabled() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok":    false,
			"error": "skills_quarantine_reindex_disabled",
		})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), skillsQuarantineTimeout())
	defer cancel()
	result, _, runErr := runSkillsQuarantineCommand(ctx, skillsQuarantineReindexCommand(), []string{})
	if runErr != nil {
		writeJSON(w, http.StatusBadGateway, result)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) toolsSkillsQuarantineSearch(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.prepareToolHeaders(w, r, "/tools/skills_quarantine_search"); !ok {
		return
	}
	s.skillsQuarantineSearchRoute(w, r)
}

func (s *server) toolsSkillsQuarantineReindex(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.prepareToolHeaders(w, r, "/tools/skills_quarantine_reindex"); !ok {
		return
	}
	s.skillsQuarantineReindexRoute(w, r)
}
