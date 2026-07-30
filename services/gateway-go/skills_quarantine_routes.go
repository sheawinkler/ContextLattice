package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type skillsQuarantineSearchRequest struct {
	Query     string
	Limit     int
	MinScore  string
	ShowTerms bool
	JSON      bool
}

type skillsIndexSearchResult struct {
	Score        int                     `json:"score"`
	Coverage     float64                 `json:"coverage"`
	MatchedTerms []string                `json:"matched_terms"`
	Name         string                  `json:"name"`
	Description  string                  `json:"description"`
	Path         string                  `json:"path"`
	Root         string                  `json:"root"`
	Source       string                  `json:"source"`
	Harness      string                  `json:"harness"`
	Tags         []string                `json:"tags"`
	Digest       string                  `json:"digest"`
	Provenance   []skillsIndexProvenance `json:"provenance"`
	RootOrder    int                     `json:"-"`
}

type skillsIndexProvenance struct {
	Path    string `json:"path"`
	Root    string `json:"root"`
	Source  string `json:"source"`
	Harness string `json:"harness"`
}

type skillsIndexTermSet struct {
	Base     []string
	Expanded []string
	Ignored  []string
	Variants map[string][]string
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

func skillsIndexRoots() []string {
	raw := firstNonEmptyStrings(
		os.Getenv("ORCH_SKILLS_INDEX_ROOTS"),
		os.Getenv("CONTEXTLATTICE_SKILLS_INDEX_ROOTS"),
		os.Getenv("CODEX_SKILLS_INDEX_ROOTS"),
	)
	if strings.TrimSpace(raw) == "" {
		raw = strings.Join([]string{
			"/opt/contextlattice/skills_active",
			"/opt/contextlattice/skills_system",
			"/opt/contextlattice/skills_hermes",
			"/opt/contextlattice/skills_hermes_ultra",
			"/opt/contextlattice/skills_shared_agents",
		}, string(os.PathListSeparator))
	}
	seen := map[string]struct{}{}
	roots := []string{}
	separator := string(os.PathListSeparator)
	normalizedList := strings.NewReplacer(",", separator, "\n", separator, "\t", separator).Replace(raw)
	for _, item := range filepath.SplitList(normalizedList) {
		root := strings.TrimSpace(item)
		if root == "" {
			continue
		}
		if strings.HasPrefix(root, "~") {
			home, _ := os.UserHomeDir()
			if home != "" {
				root = filepath.Join(home, strings.TrimPrefix(root, "~"))
			}
		}
		root = filepath.Clean(root)
		if _, ok := seen[root]; ok {
			continue
		}
		seen[root] = struct{}{}
		roots = append(roots, root)
	}
	return roots
}

func skillsIndexRootSource(root string) string {
	lower := strings.ToLower(filepath.Clean(root))
	switch {
	case strings.Contains(lower, "skills_quarantine"):
		return "quarantine"
	case strings.Contains(lower, "skills_system"), strings.Contains(lower, ".system"):
		return "system"
	case strings.Contains(lower, "memory-bank") && strings.Contains(lower, "skills_active"):
		return "foundry"
	case strings.Contains(lower, "skills_active"),
		strings.Contains(lower, "skills_hermes"),
		strings.Contains(lower, "skills_shared_agents"),
		strings.Contains(lower, "/.codex/skills"),
		strings.Contains(lower, "/.hermes/skills"),
		strings.Contains(lower, "/.hermes-agent-ultra/skills"),
		strings.Contains(lower, "/.agents/skills"):
		return "active"
	default:
		return "configured"
	}
}

func skillsIndexRootHarness(root string) string {
	lower := strings.ToLower(filepath.Clean(root))
	switch {
	case strings.Contains(lower, "skills_quarantine"):
		return "quarantine"
	case strings.Contains(lower, "skills_system"), strings.Contains(lower, ".system"):
		return "codex_system"
	case strings.Contains(lower, "skills_hermes_ultra"), strings.Contains(lower, "/.hermes-agent-ultra/skills"):
		return "hermes_agent_ultra"
	case strings.Contains(lower, "skills_hermes"), strings.Contains(lower, "/.hermes/skills"):
		return "hermes"
	case strings.Contains(lower, "skills_shared_agents"), strings.Contains(lower, "/.agents/skills"):
		return "shared_agents"
	case strings.Contains(lower, "memory-bank"):
		return "contextlattice_foundry"
	case strings.Contains(lower, "skills_active"), strings.Contains(lower, "/.codex/skills"):
		return "codex"
	default:
		return "configured"
	}
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

var skillsIndexStopwords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "audit": {}, "for": {}, "in": {}, "index": {},
	"of": {}, "on": {}, "or": {}, "skill": {}, "skills": {}, "the": {}, "to": {},
	"use": {}, "using": {}, "with": {}, "agent": {}, "agents": {},
}

func skillsIndexTermVariants(term string) []string {
	variants := []string{term}
	appendVariant := func(candidate string) {
		candidate = strings.TrimSpace(candidate)
		if len(candidate) < 2 {
			return
		}
		for _, existing := range variants {
			if existing == candidate {
				return
			}
		}
		variants = append(variants, candidate)
	}
	switch {
	case strings.HasSuffix(term, "ies") && len(term) > 4:
		appendVariant(strings.TrimSuffix(term, "ies") + "y")
	case strings.HasSuffix(term, "s") && len(term) > 3 && !strings.HasSuffix(term, "ss"):
		appendVariant(strings.TrimSuffix(term, "s"))
	case strings.HasSuffix(term, "y") && len(term) > 3:
		appendVariant(strings.TrimSuffix(term, "y") + "ies")
	case len(term) > 3:
		appendVariant(term + "s")
	}
	return variants
}

func analyzeSkillsIndexTerms(query string) skillsIndexTermSet {
	result := skillsIndexTermSet{Variants: map[string][]string{}}
	seenBase := map[string]struct{}{}
	seenExpanded := map[string]struct{}{}
	seenIgnored := map[string]struct{}{}
	for _, raw := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_')
	}) {
		term := strings.Trim(raw, "-_")
		if len(term) < 2 {
			continue
		}
		if _, ignored := skillsIndexStopwords[term]; ignored {
			if _, seen := seenIgnored[term]; !seen {
				seenIgnored[term] = struct{}{}
				result.Ignored = append(result.Ignored, term)
			}
			continue
		}
		if _, ok := seenBase[term]; ok {
			continue
		}
		seenBase[term] = struct{}{}
		result.Base = append(result.Base, term)
		variants := skillsIndexTermVariants(term)
		result.Variants[term] = variants
		for _, variant := range variants {
			if _, seen := seenExpanded[variant]; !seen {
				seenExpanded[variant] = struct{}{}
				result.Expanded = append(result.Expanded, variant)
			}
		}
	}
	return result
}

func skillsIndexTerms(query string) []string {
	return analyzeSkillsIndexTerms(query).Expanded
}

func skillsIndexMinTermCoverage() float64 {
	return clampFloat(envFloat("ORCH_SKILLS_INDEX_MIN_TERM_COVERAGE", 0.5), 0, 1)
}

func parseSkillFrontmatterValue(text string, key string) string {
	if !strings.HasPrefix(text, "---") {
		return ""
	}
	end := strings.Index(text[3:], "\n---")
	if end < 0 {
		return ""
	}
	header := text[3 : 3+end]
	prefix := strings.ToLower(key) + ":"
	for _, line := range strings.Split(header, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(trimmed), prefix) {
			return strings.Trim(strings.TrimSpace(trimmed[len(prefix):]), `"'`)
		}
	}
	return ""
}

func parseSkillFrontmatterList(text string, key string) []string {
	value := parseSkillFrontmatterValue(text, key)
	if value == "" {
		return []string{}
	}
	value = strings.Trim(value, "[]")
	out := []string{}
	for _, item := range strings.Split(value, ",") {
		item = strings.Trim(strings.TrimSpace(item), `"'`)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

func skillsIndexScore(
	name string,
	description string,
	tags []string,
	body string,
	relPath string,
	source string,
	terms skillsIndexTermSet,
) (int, []string, float64) {
	score := 0
	matched := []string{}
	nameLower := strings.ToLower(name)
	descLower := strings.ToLower(description)
	bodyLower := strings.ToLower(body)
	pathLower := strings.ToLower(relPath)
	tagLower := strings.ToLower(strings.Join(tags, " "))
	for _, base := range terms.Base {
		bestVariantScore := 0
		for _, variant := range terms.Variants[base] {
			variantScore := 0
			if strings.Contains(nameLower, variant) {
				variantScore += 30
			}
			if strings.Contains(tagLower, variant) {
				variantScore += 18
			}
			if strings.Contains(descLower, variant) {
				variantScore += 14
			}
			if strings.Contains(pathLower, variant) {
				variantScore += 8
			}
			if strings.Contains(bodyLower, variant) {
				variantScore += 3
			}
			if variantScore > bestVariantScore {
				bestVariantScore = variantScore
			}
		}
		if bestVariantScore > 0 {
			score += bestVariantScore
			matched = append(matched, base)
		}
	}
	if source == "active" && score > 0 {
		score += 12
	}
	coverage := 0.0
	if len(terms.Base) > 0 {
		coverage = float64(len(matched)) / float64(len(terms.Base))
	}
	return score, matched, coverage
}

func nativeSkillsIndexSearch(request skillsQuarantineSearchRequest) map[string]any {
	terms := analyzeSkillsIndexTerms(request.Query)
	minCoverage := skillsIndexMinTermCoverage()
	candidates := []skillsIndexSearchResult{}
	rootStats := []map[string]any{}
	for rootOrder, root := range skillsIndexRoots() {
		source := skillsIndexRootSource(root)
		harness := skillsIndexRootHarness(root)
		stat := map[string]any{
			"path": root, "source": source, "harness": harness, "exists": false,
			"skills": 0, "skills_seen": 0, "matches": 0, "unique_matches": 0,
		}
		info, statErr := os.Stat(root)
		if statErr != nil || !info.IsDir() {
			rootStats = append(rootStats, stat)
			continue
		}
		stat["exists"] = true
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil || entry == nil {
				return nil
			}
			if entry.IsDir() {
				name := entry.Name()
				if strings.HasPrefix(name, ".") && name != "." {
					return filepath.SkipDir
				}
				if name == "node_modules" || name == "__pycache__" || name == "index" {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.Name() != "SKILL.md" {
				return nil
			}
			stat["skills"] = anyToInt(stat["skills"], 0) + 1
			stat["skills_seen"] = anyToInt(stat["skills_seen"], 0) + 1
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}
			text := string(raw)
			name := parseSkillFrontmatterValue(text, "name")
			if name == "" {
				name = filepath.Base(filepath.Dir(path))
			}
			description := parseSkillFrontmatterValue(text, "description")
			tags := parseSkillFrontmatterList(text, "tags")
			relPath, _ := filepath.Rel(root, path)
			score, matched, coverage := skillsIndexScore(name, description, tags, text, relPath, source, terms)
			if coverage < minCoverage {
				return nil
			}
			if request.MinScore != "" {
				minScore, _ := strconv.ParseFloat(request.MinScore, 64)
				if float64(score) < minScore {
					return nil
				}
			} else if score <= 0 {
				return nil
			}
			stat["matches"] = anyToInt(stat["matches"], 0) + 1
			digest := "sha256:" + sha256Hex(text)
			candidates = append(candidates, skillsIndexSearchResult{
				Score:        score,
				Coverage:     coverage,
				MatchedTerms: matched,
				Name:         name,
				Description:  clipText(description, 700),
				Path:         path,
				Root:         root,
				Source:       source,
				Harness:      harness,
				Tags:         tags,
				Digest:       digest,
				Provenance: []skillsIndexProvenance{{
					Path: path, Root: root, Source: source, Harness: harness,
				}},
				RootOrder: rootOrder,
			})
			return nil
		})
		rootStats = append(rootStats, stat)
	}
	results := make([]skillsIndexSearchResult, 0, len(candidates))
	resultByDigest := map[string]int{}
	for _, candidate := range candidates {
		if index, exists := resultByDigest[candidate.Digest]; exists {
			results[index].Provenance = append(results[index].Provenance, candidate.Provenance...)
			continue
		}
		resultByDigest[candidate.Digest] = len(results)
		results = append(results, candidate)
		rootStats[candidate.RootOrder]["unique_matches"] = anyToInt(rootStats[candidate.RootOrder]["unique_matches"], 0) + 1
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			if results[i].Coverage == results[j].Coverage {
				if results[i].RootOrder != results[j].RootOrder {
					return results[i].RootOrder < results[j].RootOrder
				}
				if results[i].Name == results[j].Name {
					return results[i].Path < results[j].Path
				}
				return results[i].Name < results[j].Name
			}
			return results[i].Coverage > results[j].Coverage
		}
		return results[i].Score > results[j].Score
	})
	total := len(results)
	if len(results) > request.Limit {
		results = results[:request.Limit]
	}
	resultItems := make([]any, 0, len(results))
	for _, item := range results {
		provenance := make([]any, 0, len(item.Provenance))
		for _, source := range item.Provenance {
			provenance = append(provenance, map[string]any{
				"path": source.Path, "root": source.Root, "source": source.Source, "harness": source.Harness,
			})
		}
		resultItems = append(resultItems, map[string]any{
			"score":           item.Score,
			"coverage":        roundFloat(item.Coverage, 6),
			"matched_terms":   item.MatchedTerms,
			"name":            item.Name,
			"description":     item.Description,
			"path":            item.Path,
			"root":            item.Root,
			"source":          item.Source,
			"harness":         item.Harness,
			"tags":            item.Tags,
			"digest":          item.Digest,
			"provenance":      provenance,
			"duplicate_count": maxInt(0, len(item.Provenance)-1),
		})
	}
	warnings := []string{}
	if len(terms.Base) == 0 {
		warnings = append(warnings, "query contains no discriminating terms after stopword filtering")
	}
	parsed := map[string]any{
		"query":                 request.Query,
		"discriminating_terms":  stringSliceAny(terms.Base),
		"expanded_terms":        stringSliceAny(terms.Expanded),
		"ignored_terms":         stringSliceAny(terms.Ignored),
		"minimum_term_coverage": minCoverage,
		"total_matches":         total,
		"total_candidates":      len(candidates),
		"returned":              len(resultItems),
		"results":               resultItems,
		"roots":                 rootStats,
		"warnings":              stringSliceAny(warnings),
		"index":                 "native_active_skills",
	}
	payload := map[string]any{
		"ok":                    true,
		"index":                 "native_active_skills",
		"query":                 request.Query,
		"limit":                 request.Limit,
		"show_terms":            request.ShowTerms,
		"min_score":             request.MinScore,
		"minimum_term_coverage": minCoverage,
		"json":                  request.JSON,
		"roots":                 rootStats,
		"results":               resultItems,
		"returned":              len(resultItems),
		"total_matches":         total,
		"total_candidates":      len(candidates),
		"ignored_terms":         stringSliceAny(terms.Ignored),
		"discriminating_terms":  stringSliceAny(terms.Base),
		"warnings":              stringSliceAny(warnings),
		"parsed":                parsed,
	}
	if request.ShowTerms {
		payload["expanded_terms"] = stringSliceAny(terms.Expanded)
	}
	return payload
}

func nativeSkillsIndexStatus() map[string]any {
	request := skillsQuarantineSearchRequest{Query: "skill", Limit: 1, JSON: true}
	payload := nativeSkillsIndexSearch(request)
	roots, _ := payload["roots"].([]map[string]any)
	totalSkills := 0
	for _, root := range roots {
		totalSkills += anyToInt(root["skills"], 0)
	}
	payload["reindex_required"] = false
	payload["reindex_mode"] = "live_native_scan"
	payload["total_skills_seen"] = totalSkills
	return payload
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

func (s *server) skillsIndexSearchRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if !skillsQuarantineEnabled() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok":    false,
			"error": "skills_index_disabled",
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
	writeJSON(w, http.StatusOK, nativeSkillsIndexSearch(request))
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

func (s *server) skillsIndexReindexRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if !skillsQuarantineEnabled() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok":    false,
			"error": "skills_index_disabled",
		})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, nativeSkillsIndexStatus())
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

func (s *server) toolsSkillsIndexSearch(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.prepareToolHeaders(w, r, "/tools/skills_index_search"); !ok {
		return
	}
	s.skillsIndexSearchRoute(w, r)
}

func (s *server) toolsSkillsIndexReindex(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.prepareToolHeaders(w, r, "/tools/skills_index_reindex"); !ok {
		return
	}
	s.skillsIndexReindexRoute(w, r)
}
