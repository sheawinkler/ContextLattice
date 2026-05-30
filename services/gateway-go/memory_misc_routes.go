package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func methodAllowed(method string, allowed ...string) bool {
	normalized := strings.TrimSpace(strings.ToUpper(method))
	if normalized == "" {
		return false
	}
	for _, candidate := range allowed {
		if normalized == strings.TrimSpace(strings.ToUpper(candidate)) {
			return true
		}
	}
	return false
}

func parseNormalizedCSVSet(raw string, fallback string) map[string]struct{} {
	res := map[string]struct{}{}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		trimmed = strings.TrimSpace(fallback)
	}
	for _, part := range strings.Split(trimmed, ",") {
		normalized := strings.TrimSpace(strings.ToLower(part))
		if normalized == "" {
			continue
		}
		res[normalized] = struct{}{}
	}
	return res
}

func normalizeHTTPPath(raw string) string {
	token := strings.TrimSpace(strings.ToLower(raw))
	if token == "" {
		return ""
	}
	if !strings.HasPrefix(token, "/") {
		token = "/" + token
	}
	if len(token) > 1 {
		token = strings.TrimRight(token, "/")
	}
	return token
}

func parseHTTPPathSet(raw string, fallback string) map[string]struct{} {
	res := map[string]struct{}{}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		trimmed = strings.TrimSpace(fallback)
	}
	for _, part := range strings.Split(trimmed, ",") {
		normalized := normalizeHTTPPath(part)
		if normalized == "" {
			continue
		}
		res[normalized] = struct{}{}
	}
	return res
}

func parseAliasMap(raw string, fallback string) map[string]string {
	res := map[string]string{}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		trimmed = strings.TrimSpace(fallback)
	}
	for _, part := range strings.Split(trimmed, ",") {
		pair := strings.SplitN(strings.TrimSpace(strings.ToLower(part)), ":", 2)
		if len(pair) != 2 {
			continue
		}
		alias := strings.TrimSpace(pair[0])
		target := strings.TrimSpace(pair[1])
		if alias == "" || target == "" {
			continue
		}
		res[alias] = target
	}
	return res
}

func normalizeEntitlementPlan(raw string, aliases map[string]string) string {
	plan := strings.TrimSpace(strings.ToLower(raw))
	if plan == "" {
		return ""
	}
	if mapped, ok := aliases[plan]; ok {
		plan = strings.TrimSpace(strings.ToLower(mapped))
	}
	return plan
}

func (s *server) entitlementPathProtected(path string) bool {
	mode := strings.TrimSpace(strings.ToLower(os.Getenv("GO_V4_ENTITLEMENT_MODE")))
	switch mode {
	case "", "off":
		return false
	case "warn", "enforce":
	default:
		return false
	}
	protected := parseHTTPPathSet(
		os.Getenv("GO_V4_ENTITLEMENT_PROTECTED_PATHS"),
		"/v1/inference/route,/v1/inference/chat,/v1/inference/runtime-policy,/v1/inference/embedding-policy,/maintenance/storage/run,/maintenance/telemetry/blob-gc,/migration/runtime",
	)
	normalized := normalizeHTTPPath(path)
	if normalized == "" {
		return false
	}
	if _, ok := protected[normalized]; ok {
		return true
	}
	for candidate := range protected {
		if candidate == "" {
			continue
		}
		if strings.HasSuffix(candidate, "/*") {
			prefix := strings.TrimSuffix(candidate, "/*")
			if prefix != "" && strings.HasPrefix(normalized, prefix+"/") {
				return true
			}
		}
	}
	return false
}

func (s *server) enforceV4Entitlement(w http.ResponseWriter, r *http.Request, path string) bool {
	mode := strings.TrimSpace(strings.ToLower(os.Getenv("GO_V4_ENTITLEMENT_MODE")))
	switch mode {
	case "", "off":
		return true
	case "warn", "enforce":
	default:
		return true
	}
	if !s.entitlementPathProtected(path) {
		return true
	}

	environment := strings.TrimSpace(strings.ToLower(os.Getenv("CONTEXTLATTICE_ENV")))
	if environment == "" {
		environment = strings.TrimSpace(strings.ToLower(os.Getenv("MEMMCP_ENV")))
	}
	securityStrict := envBool("ORCH_SECURITY_STRICT", false)
	if !securityStrict && environment != "production" && envBool("GO_V4_ENTITLEMENT_DEV_ALLOW", true) {
		return true
	}

	requiredKey := strings.TrimSpace(os.Getenv("GO_V4_ENTITLEMENT_KEY"))
	if requiredKey != "" && !secureTokenEqual(r.Header.Get("X-ContextLattice-Entitlement-Key"), requiredKey) {
		if mode == "warn" {
			return true
		}
		writeJSON(w, http.StatusPaymentRequired, map[string]any{
			"ok":      false,
			"error":   "entitlement_required",
			"message": "missing or invalid entitlement key",
			"path":    path,
		})
		return false
	}

	aliases := parseAliasMap(
		os.Getenv("GO_V4_ENTITLEMENT_PLAN_ALIASES"),
		"pro:team,business:enterprise",
	)
	allowedPlans := parseNormalizedCSVSet(
		os.Getenv("GO_V4_ENTITLEMENT_ALLOWED_PLANS"),
		"team,enterprise",
	)
	plan := normalizeEntitlementPlan(r.Header.Get("X-ContextLattice-Plan"), aliases)
	if len(allowedPlans) > 0 {
		if _, ok := allowedPlans[plan]; !ok {
			if mode == "warn" {
				return true
			}
			writeJSON(w, http.StatusPaymentRequired, map[string]any{
				"ok":      false,
				"error":   "entitlement_required",
				"message": "plan is not entitled for this route",
				"path":    path,
			})
			return false
		}
	}

	allowedRoles := parseNormalizedCSVSet(
		os.Getenv("GO_V4_ENTITLEMENT_ALLOWED_ROLES"),
		"owner,admin",
	)
	role := strings.TrimSpace(strings.ToLower(r.Header.Get("X-ContextLattice-Workspace-Role")))
	if len(allowedRoles) > 0 {
		if _, ok := allowedRoles[role]; !ok {
			if mode == "warn" {
				return true
			}
			writeJSON(w, http.StatusPaymentRequired, map[string]any{
				"ok":      false,
				"error":   "entitlement_required",
				"message": "workspace role is not entitled for this route",
				"path":    path,
			})
			return false
		}
	}
	return true
}

func backendPathWithQuery(path string, rawQuery string) string {
	trimmed := strings.TrimSpace(rawQuery)
	if trimmed == "" {
		return path
	}
	return path + "?" + trimmed
}

func (s *server) forwardJSONGET(w http.ResponseWriter, r *http.Request, backendPath string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if s.strictNoPythonRuntime {
		if !s.allowPythonHotPathFallback(w, backendPath, "strict_runtime_backend_forward_disabled") {
			return
		}
	}
	incomingHeaders, ok := s.prepareAuthorizedHeaders(w, r)
	if !ok {
		return
	}
	response, status, err := s.backendJSONRequest(
		r.Context(),
		http.MethodGet,
		backendPathWithQuery(backendPath, r.URL.RawQuery),
		incomingHeaders,
		nil,
	)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":      "backend unavailable",
			"detail":     err.Error(),
			"backendUrl": s.backendURL,
		})
		return
	}
	writeJSON(w, status, response)
}

func (s *server) forwardJSONPOST(w http.ResponseWriter, r *http.Request, backendPath string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if s.strictNoPythonRuntime {
		if !s.allowPythonHotPathFallback(w, backendPath, "strict_runtime_backend_forward_disabled") {
			return
		}
	}
	incomingHeaders, ok := s.prepareAuthorizedHeaders(w, r)
	if !ok {
		return
	}
	rawBody, err := readRequestBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
		return
	}
	payload, err := parseJSONMap(rawBody)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
		return
	}
	response, status, backendErr := s.backendJSONRequest(
		r.Context(),
		http.MethodPost,
		backendPathWithQuery(backendPath, r.URL.RawQuery),
		incomingHeaders,
		payload,
	)
	if backendErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":      "backend unavailable",
			"detail":     backendErr.Error(),
			"backendUrl": s.backendURL,
		})
		return
	}
	writeJSON(w, status, response)
}

func (s *server) forwardJSONAny(w http.ResponseWriter, r *http.Request, backendPath string) {
	method := strings.TrimSpace(strings.ToUpper(r.Method))
	if method == "" {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if s.strictNoPythonRuntime {
		if !s.allowPythonHotPathFallback(w, backendPath, "strict_runtime_backend_forward_disabled") {
			return
		}
	}
	incomingHeaders, ok := s.prepareAuthorizedHeaders(w, r)
	if !ok {
		return
	}
	var payload map[string]any
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodDelete {
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
			return
		}
		trimmed := strings.TrimSpace(string(rawBody))
		if trimmed != "" {
			parsed, err := parseJSONMap(rawBody)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
				return
			}
			payload = parsed
		}
	}
	response, status, backendErr := s.backendJSONRequest(
		r.Context(),
		method,
		backendPathWithQuery(backendPath, r.URL.RawQuery),
		incomingHeaders,
		payload,
	)
	if backendErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":      "backend unavailable",
			"detail":     backendErr.Error(),
			"backendUrl": s.backendURL,
		})
		return
	}
	writeJSON(w, status, response)
}

func (s *server) memoryRecent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	if s.memoryStore == nil || !s.memoryStore.policy.enabled {
		s.forwardJSONGET(w, r, "/memory/recent")
		return
	}
	limit, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	offset, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("offset")))
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	topicPath := strings.TrimSpace(r.URL.Query().Get("topic_path"))
	items := s.memoryStore.recentItems(project, topicPath, limit, offset)
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *server) memoryTopicTree(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	if s.memoryStore == nil || !s.memoryStore.policy.enabled {
		s.forwardJSONGET(w, r, "/memory/topics")
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	writeJSON(w, http.StatusOK, s.memoryStore.topicTree(project))
}

func (s *server) memoryTopicList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	if s.memoryStore == nil || !s.memoryStore.policy.enabled {
		s.forwardJSONGET(w, r, "/memory/topics/list")
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	minCount, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("min_count")))
	limit, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	depth, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("depth")))
	writeJSON(w, http.StatusOK, s.memoryStore.topicList(project, minCount, limit, depth))
}

func (s *server) memoryTopicRollups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	if s.memoryStore == nil || !s.memoryStore.policy.enabled {
		s.forwardJSONGET(w, r, "/memory/topic-rollups")
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	minCount, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("min_count")))
	limit, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	offset, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("offset")))
	writeJSON(w, http.StatusOK, s.memoryStore.topicRollups(project, minCount, limit, offset))
}

func (s *server) memoryContinuitySnapshot(w http.ResponseWriter, r *http.Request) {
	s.forwardJSONPOST(w, r, "/memory/continuity/snapshot")
}

func (s *server) memoryContinuitySnapshots(w http.ResponseWriter, r *http.Request) {
	s.forwardJSONGET(w, r, "/memory/continuity/snapshots")
}

func (s *server) memoryContinuitySnapshotByID(w http.ResponseWriter, r *http.Request) {
	s.forwardJSONGET(w, r, r.URL.Path)
}

func (s *server) memoryRecallEvalCases(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	cfg, err := loadSavedRecallEvalConfig()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "failed to load saved recall eval cases", "detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":            cfg.Path,
		"version":         cfg.Version,
		"updatedAt":       cfg.UpdatedAt,
		"k":               cfg.K,
		"case_set_health": validateSavedRecallEvalCaseSet(cfg),
		"gate": map[string]any{
			"minRecallAtK":        cfg.Gate.MinRecallAtK,
			"minMrr":              cfg.Gate.MinMRR,
			"minNumericExactness": cfg.Gate.MinNumericExactly,
		},
		"count": len(cfg.Cases),
		"cases": cfg.Cases,
	})
}

func (s *server) memoryRecallEvalCasesRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
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
	maxCases := clampInt(anyToInt(payload["max_cases"], 12), 1, 20)
	minHits := clampInt(anyToInt(payload["min_hits"], 1), 1, 1000)
	project := strings.TrimSpace(anyToString(payload["project"]))
	topicPrefix := strings.TrimSpace(anyToString(payload["topic_prefix"]))
	refreshed := s.buildRefreshedRecallEvalCaseSet(maxCases, minHits, project, topicPrefix)
	path := resolveRecallEvalCasesPath()
	raw, err := json.MarshalIndent(refreshed, "", "  ")
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "failed to encode refreshed cases", "detail": err.Error()})
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "failed to create recall eval directory", "detail": err.Error()})
		return
	}
	if err := writeAtomicFile(path, raw, 0o644); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "failed to persist refreshed recall eval cases", "detail": err.Error()})
		return
	}
	casesAny, _ := refreshed["cases"].([]map[string]any)
	refreshedHealth := validateSavedRecallEvalCaseSet(recallEvalSavedConfig{
		Path:      path,
		Version:   refreshed["version"],
		UpdatedAt: refreshed["updatedAt"],
		K:         anyToInt(refreshed["k"], defaultRecallEvalK),
		Gate: recallEvalGate{
			MinRecallAtK:      defaultRecallEvalGateMinRecallAtK,
			MinMRR:            defaultRecallEvalGateMinMRR,
			MinNumericExactly: defaultRecallEvalGateMinNumeric,
		},
		Cases: casesAny,
	})
	response := map[string]any{
		"ok":              anyToBool(refreshedHealth["valid"]),
		"case_set_health": refreshedHealth,
		"savedCaseSet": map[string]any{
			"path":           path,
			"version":        refreshed["version"],
			"updatedAt":      refreshed["updatedAt"],
			"count":          len(casesAny),
			"maxCases":       maxCases,
			"minHits":        minHits,
			"caseSetHealthy": anyToBool(refreshedHealth["valid"]),
		},
	}
	if anyToBool(payload["run_evaluation"]) {
		response["evaluation"] = map[string]any{
			"ok":      false,
			"warning": "native refresh completed; run /memory/recall/evaluate/saved for full evaluation payload",
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *server) memoryRecallEvaluateSaved(w http.ResponseWriter, r *http.Request) {
	s.memoryRecallEvaluateSavedNative(w, r)
}

func (s *server) buildRefreshedRecallEvalCaseSet(maxCases int, minHits int, project string, topicPrefix string) map[string]any {
	maxCases = clampInt(maxCases, 1, 20)
	if minHits < 1 {
		minHits = 1
	}
	project = strings.TrimSpace(project)
	topicPrefix = recallEvalNormalizeTopicPath(topicPrefix)
	cases := make([]map[string]any, 0, maxCases)
	if s.memoryStore != nil && s.memoryStore.policy.enabled {
		docs, err := s.memoryStore.collectDocs(context.Background(), project)
		if err == nil {
			cases = recallEvalCasesFromDocs(docs, maxCases, minHits, project, topicPrefix)
		}
		if len(cases) == 0 {
			rollups := s.memoryStore.topicRollupsWithContext(context.Background(), project, minHits, maxCases*6, 0)
			if rowsAny, ok := rollups["topics"].([]any); ok {
				for _, item := range rowsAny {
					row := anyMap(item)
					topic := recallEvalNormalizeTopicPath(anyToString(row["topic_path"]))
					if topic == "" {
						topic = recallEvalNormalizeTopicPath(anyToString(row["path"]))
					}
					if topicPrefix != "" && !strings.HasPrefix(topic, topicPrefix) {
						continue
					}
					hits := anyToInt(row["event_count"], anyToInt(row["eventCount"], 0))
					if hits < minHits {
						continue
					}
					query := strings.TrimSpace(strings.ReplaceAll(topic, "/", " "))
					summarySnippets := recallEvalSummarySnippets(row)
					if query == "" && len(summarySnippets) > 0 {
						query = strings.TrimSpace(summarySnippets[0])
					}
					if query == "" {
						continue
					}
					expectedFiles := recallEvalExpectedFilesFromTopic(row)
					expectedTerms := []string{}
					for _, summary := range summarySnippets {
						expectedTerms = append(expectedTerms, clipText(strings.ToLower(summary), 64))
						if len(expectedTerms) >= 2 {
							break
						}
					}
					cases = append(cases, map[string]any{
						"id":                  recallEvalCaseID(topic, len(cases)),
						"query":               query,
						"project":             project,
						"topic_path":          topic,
						"limit":               10,
						"expected_files":      expectedFiles,
						"expected_substrings": expectedTerms,
					})
					if len(cases) >= maxCases {
						break
					}
				}
			}
		}
	}
	if len(cases) == 0 {
		cases = defaultSavedRecallEvalConfig(resolveRecallEvalCasesPath()).Cases
		if len(cases) > maxCases {
			cases = cases[:maxCases]
		}
	}
	return map[string]any{
		"version":   1,
		"updatedAt": nowUTCISO(),
		"k":         defaultRecallEvalK,
		"gate": map[string]any{
			"minRecallAtK":        defaultRecallEvalGateMinRecallAtK,
			"minMrr":              defaultRecallEvalGateMinMRR,
			"minNumericExactness": defaultRecallEvalGateMinNumeric,
		},
		"cases": cases,
	}
}

func recallEvalCasesFromDocs(docs []memoryStoreDoc, maxCases int, minHits int, project string, topicPrefix string) []map[string]any {
	topicCounts := map[string]int{}
	for _, doc := range docs {
		topic := recallEvalNormalizeTopicPath(doc.TopicPath)
		if topic == "" {
			continue
		}
		topicCounts[topic] += 1
	}
	filtered := make([]memoryStoreDoc, 0, len(docs))
	for _, doc := range docs {
		if strings.TrimSpace(doc.FileName) == "" {
			continue
		}
		topic := recallEvalNormalizeTopicPath(doc.TopicPath)
		if topicPrefix != "" && !strings.HasPrefix(topic, topicPrefix) {
			continue
		}
		if minHits > 1 && topicCounts[topic] < minHits {
			continue
		}
		if recallEvalQueryFromDoc(doc) == "" {
			continue
		}
		filtered = append(filtered, doc)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		leftTopic := recallEvalNormalizeTopicPath(filtered[i].TopicPath)
		rightTopic := recallEvalNormalizeTopicPath(filtered[j].TopicPath)
		leftDepth := topicDepth(leftTopic)
		rightDepth := topicDepth(rightTopic)
		if leftDepth != rightDepth {
			return leftDepth > rightDepth
		}
		if !filtered[i].UpdatedAt.Equal(filtered[j].UpdatedAt) {
			return filtered[i].UpdatedAt.After(filtered[j].UpdatedAt)
		}
		return strings.TrimSpace(filtered[i].FileName) < strings.TrimSpace(filtered[j].FileName)
	})
	seen := map[string]struct{}{}
	cases := make([]map[string]any, 0, maxCases)
	for _, doc := range filtered {
		fileName := strings.Trim(strings.TrimSpace(doc.FileName), "/")
		if fileName == "" {
			continue
		}
		dedupeKey := strings.ToLower(strings.TrimSpace(doc.Project + "::" + fileName))
		if _, ok := seen[dedupeKey]; ok {
			continue
		}
		seen[dedupeKey] = struct{}{}
		topic := recallEvalNormalizeTopicPath(doc.TopicPath)
		expectedTerms := []string{}
		if summary := strings.TrimSpace(doc.Summary); summary != "" {
			expectedTerms = append(expectedTerms, clipText(strings.ToLower(summary), 96))
		}
		caseProject := strings.TrimSpace(doc.Project)
		if caseProject == "" {
			caseProject = project
		}
		cases = append(cases, map[string]any{
			"id":                  recallEvalCaseID(caseProject+"::"+fileName, len(cases)),
			"query":               recallEvalQueryFromDoc(doc),
			"project":             caseProject,
			"topic_path":          topic,
			"limit":               10,
			"expected_files":      []string{fileName},
			"expected_substrings": expectedTerms,
		})
		if len(cases) >= maxCases {
			break
		}
	}
	return cases
}

func recallEvalSummarySnippets(row map[string]any) []string {
	snippets := make([]string, 0, 3)
	for _, item := range anyToStringSlice(row["summarySnippets"]) {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			snippets = append(snippets, trimmed)
		}
	}
	if summary := strings.TrimSpace(anyToString(row["summary"])); summary != "" {
		snippets = append(snippets, summary)
	}
	return snippets
}

func recallEvalExpectedFilesFromTopic(row map[string]any) []string {
	seen := map[string]struct{}{}
	files := make([]string, 0, 5)
	add := func(fileName string) {
		trimmed := strings.Trim(strings.TrimSpace(fileName), "/")
		if trimmed == "" {
			return
		}
		normalized := strings.ToLower(trimmed)
		if _, exists := seen[normalized]; exists {
			return
		}
		seen[normalized] = struct{}{}
		files = append(files, trimmed)
	}
	for _, fileName := range anyToStringSlice(row["uniqueFiles"]) {
		add(fileName)
	}
	if partitions, ok := row["filePartitions"].([]any); ok {
		for _, raw := range partitions {
			partition := anyMap(raw)
			add(anyToString(partition["file"]))
		}
	}
	if len(files) > 5 {
		files = files[:5]
	}
	return files
}

func recallEvalQueryFromDoc(doc memoryStoreDoc) string {
	topic := strings.ReplaceAll(recallEvalNormalizeTopicPath(doc.TopicPath), "/", " ")
	fileName := strings.TrimSpace(doc.FileName)
	fileStem := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	fileStem = strings.ReplaceAll(fileStem, "/", " ")
	summary := clipText(doc.Summary, 120)
	query := strings.TrimSpace(strings.Join([]string{topic, fileStem, summary}, " "))
	if query != "" {
		return query
	}
	return strings.TrimSpace(clipText(doc.Summary, 160))
}

func recallEvalCaseID(seed string, idx int) string {
	token := strings.TrimSpace(seed)
	if token == "" {
		token = strconv.Itoa(idx + 1)
	}
	return "refresh-" + sha256Hex(token)[:16]
}

func recallEvalNormalizeTopicPath(value string) string {
	return strings.Trim(strings.ToLower(strings.TrimSpace(value)), "/")
}

func (s *server) feedbackRoute(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.forwardJSONGET(w, r, "/feedback")
	case http.MethodPost:
		s.forwardJSONPOST(w, r, "/feedback")
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

func (s *server) migrationRuntime(w http.ResponseWriter, r *http.Request) {
	if !methodAllowed(r.Method, http.MethodGet, http.MethodPost) {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	flags := map[string]any{
		"use_rust_codec":     envBool("USE_RUST_CODEC", true),
		"use_rust_memory":    envBool("USE_RUST_MEMORY", true),
		"use_rust_retrieval": envBool("USE_RUST_RETRIEVAL", true),
		"use_go_orchestrator": envBool(
			"USE_GO_ORCHESTRATOR",
			true,
		),
		"engine_mode":     strings.TrimSpace(os.Getenv("CONTEXTLATTICE_ENGINE_MODE")),
		"shadow_dual_run": envBool("CONTEXTLATTICE_SHADOW_DUAL_RUN", false),
		"canary_enabled":  envBool("CONTEXTLATTICE_CANARY_ENABLED", false),
	}
	if strings.TrimSpace(anyToString(flags["engine_mode"])) == "" {
		flags["engine_mode"] = "service"
	}
	services := s.strictRuntimeServices()
	healthyServices := 0
	for _, svc := range services {
		if anyToBool(svc["healthy"]) {
			healthyServices++
		}
	}
	pythonFallbackMode := "available"
	if s.strictNoPythonRuntime {
		pythonFallbackMode = "disabled"
	}
	payload := map[string]any{
		"enabled": true,
		"flags":   flags,
		"implementations": map[string]any{
			"gateway":         sourceOwnerGoNative,
			"retrieval":       sourceOwnerGoNative,
			"topic_rollups":   sourceOwnerGoNative,
			"memory_bank":     sourceOwnerRustNative,
			"python_fallback": pythonFallbackMode,
		},
		"snapshot": map[string]any{
			"strictNoPythonRuntime": s.strictNoPythonRuntime,
			"routeOwnerClass":       sourceOwnerGoNative,
			"pythonHotPathOwnership": map[string]any{
				"mode":       anyToString(s.pythonHotPathOwnershipSnapshot()["mode"]),
				"fallbacks":  anyToInt(s.pythonHotPathOwnershipSnapshot()["fallbacks"], 0),
				"status":     anyToString(s.pythonHotPathOwnershipSnapshot()["status"]),
				"lastAt":     anyToString(s.pythonHotPathOwnershipSnapshot()["lastFallbackAt"]),
				"lastReason": anyToString(s.pythonHotPathOwnershipSnapshot()["lastReason"]),
			},
			"runtimeBackendPolicy":    defaultRustBackendPolicy(),
			"retrievalFastSources":    append([]string{}, s.retrieval.fastSources...),
			"retrievalSlowSources":    append([]string{}, s.retrieval.slowSources...),
			"retrievalDefaultSources": append([]string{}, s.retrieval.defaultSources...),
			"serviceHealth": map[string]any{
				"healthy": healthyServices,
				"total":   len(services),
			},
		},
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *server) memoryFilesByProject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	if s.memoryStore == nil || !s.memoryStore.policy.enabled {
		s.forwardJSONGET(w, r, r.URL.Path)
		return
	}
	project, fileName, err := s.memoryStore.parseProjectFileFromPath(r.URL.Path)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	content, info, readErr := s.memoryStore.readFile(project, fileName)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			writeJSON(w, http.StatusNotFound, map[string]any{"detail": "memory file not found"})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "memory file read failed", "detail": readErr.Error()})
		return
	}
	w.Header().Set("content-type", "text/plain; charset=utf-8")
	w.Header().Set("x-contextlattice-source", "go_memory_store")
	if info != nil {
		w.Header().Set("x-contextlattice-last-modified", info.ModTime().UTC().Format(time.RFC3339Nano))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(content))
}

func (s *server) agentsTasksRoute(w http.ResponseWriter, r *http.Request) {
	if !methodAllowed(r.Method, http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete, http.MethodPut) {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	s.forwardJSONAny(w, r, r.URL.Path)
}

func (s *server) telemetryRoute(w http.ResponseWriter, r *http.Request) {
	if !methodAllowed(r.Method, http.MethodGet) {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	s.forwardJSONGET(w, r, r.URL.Path)
}

func (s *server) preferencesRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
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
}

func (s *server) maintenanceRoute(w http.ResponseWriter, r *http.Request) {
	if s.entitlementPathProtected(r.URL.Path) {
		if !s.enforceV4Entitlement(w, r, r.URL.Path) {
			return
		}
	}
	if !methodAllowed(r.Method, http.MethodGet, http.MethodPost) {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	s.forwardJSONAny(w, r, r.URL.Path)
}
