package main

import (
	"io"
	"net/http"
	"os"
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
		"/v1/inference/route,/v1/inference/chat,/v1/inference/embedding-policy,/maintenance/storage/run,/maintenance/telemetry/blob-gc,/migration/runtime",
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
	s.forwardJSONGET(w, r, "/memory/recall/eval-cases")
}

func (s *server) memoryRecallEvalCasesRefresh(w http.ResponseWriter, r *http.Request) {
	s.forwardJSONPOST(w, r, "/memory/recall/eval-cases/refresh")
}

func (s *server) memoryRecallEvaluateSaved(w http.ResponseWriter, r *http.Request) {
	s.memoryRecallEvaluateSavedNative(w, r)
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
