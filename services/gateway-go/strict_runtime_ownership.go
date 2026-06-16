package main

import (
	"net/http"
	"sort"
	"strings"
)

type nativeOwnedRoute struct {
	Path     string
	Surface  string
	Owner    string
	Status   string
	Detail   string
	Required bool
}

func strictRuntimeOwnedRoutes() []nativeOwnedRoute {
	return []nativeOwnedRoute{
		{Path: "/health", Surface: "runtime", Owner: sourceOwnerGoNative, Status: "native", Detail: "gateway liveness and queue health", Required: true},
		{Path: "/status", Surface: "runtime", Owner: sourceOwnerGoNative, Status: "native", Detail: "strict runtime status and service health", Required: true},
		{Path: "/migration/runtime", Surface: "runtime", Owner: sourceOwnerGoNative, Status: "native", Detail: "Go/Rust migration flags", Required: true},
		{Path: "/ops/capabilities", Surface: "runtime", Owner: sourceOwnerGoNative, Status: "native", Detail: "agent-facing capability map", Required: true},
		{Path: "/ops/native-ownership", Surface: "runtime", Owner: sourceOwnerGoNative, Status: "native", Detail: "strict runtime native route audit", Required: true},
		{Path: "/ops/queue/status", Surface: "async", Owner: sourceOwnerGoNative, Status: "native", Detail: "continuation queue and deadletter status", Required: true},
		{Path: "/memory/search", Surface: "retrieval", Owner: sourceOwnerGoNative, Status: "native", Detail: "staged retrieval with continuation lifecycle", Required: true},
		{Path: "/memory/search/continuations/{token}", Surface: "retrieval", Owner: sourceOwnerGoNative, Status: "native", Detail: "async retrieval polling", Required: true},
		{Path: "/memory/context-pack", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "bounded prompt-ready context packages", Required: true},
		{Path: "/tools/context_pack", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "MCP/tool context package wrapper", Required: true},
		{Path: "/memory/dream", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "dream mode runtime", Required: true},
		{Path: "/tools/dream", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "MCP/tool dream mode wrapper", Required: true},
		{Path: "/v1/agents/preflight", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "generic agent preflight", Required: true},
		{Path: "/v1/codex/preflight", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "Codex-compatible preflight", Required: true},
		{Path: "/v1/agents/sessions", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "agent runtime session ledger", Required: true},
		{Path: "/telemetry/agents/runtime", Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "agent runtime telemetry", Required: true},
		{Path: "/telemetry/metrics", Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "native metrics and embedding cache telemetry", Required: true},
		{Path: "/telemetry/memory", Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "native memory write and fanout telemetry", Required: true},
		{Path: "/telemetry/retrieval", Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "retrieval source telemetry", Required: true},
		{Path: "/telemetry/retrieval/source-quality", Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "retrieval source quality matrix", Required: true},
		{Path: "/telemetry/fanout", Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "native fanout health and queue telemetry", Required: true},
		{Path: "/telemetry/sidecar-health", Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "native sidecar status projection", Required: true},
		{Path: "/telemetry/strategies", Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "native strategy telemetry contract", Required: true},
		{Path: "/telemetry/strategies/history", Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "native strategy history contract", Required: true},
		{Path: "/telemetry/storage", Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "storage governance and memory topology", Required: true},
		{Path: "/telemetry/recall", Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "recall quality telemetry", Required: true},
		{Path: "/telemetry/recall/monitor", Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "recall monitor history", Required: true},
	}
}

func pythonFallbackPathCounts(snapshot map[string]any) map[string]int {
	out := map[string]int{}
	switch typed := snapshot["byPath"].(type) {
	case map[string]uint64:
		for key, value := range typed {
			out[key] = int(value)
		}
	case map[string]any:
		for key, value := range typed {
			out[key] = anyToInt(value, 0)
		}
	}
	return out
}

func (s *server) nativeOwnershipPayload() map[string]any {
	ownership := s.pythonHotPathOwnershipSnapshot()
	byPath := pythonFallbackPathCounts(ownership)
	routes := strictRuntimeOwnedRoutes()
	rows := make([]map[string]any, 0, len(routes))
	violations := []map[string]any{}
	bySurface := map[string]int{}
	for _, route := range routes {
		fallbacks := byPath[route.Path]
		rowStatus := route.Status
		if route.Owner == sourceOwnerPythonBackendFallback || route.Owner == "" {
			rowStatus = "python_fallback"
		}
		row := map[string]any{
			"path":                    route.Path,
			"surface":                 route.Surface,
			"owner":                   route.Owner,
			"status":                  rowStatus,
			"detail":                  route.Detail,
			"required":                route.Required,
			"strictRuntimeCompatible": route.Owner != sourceOwnerPythonBackendFallback && route.Owner != "",
			"historicalFallbacks":     fallbacks,
		}
		rows = append(rows, row)
		bySurface[route.Surface] = bySurface[route.Surface] + 1
		if rowStatus == "python_fallback" || (route.Required && route.Owner != sourceOwnerGoNative && route.Owner != sourceOwnerRustNative) {
			violations = append(violations, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		left := strings.TrimSpace(anyToString(rows[i]["surface"])) + ":" + strings.TrimSpace(anyToString(rows[i]["path"]))
		right := strings.TrimSpace(anyToString(rows[j]["surface"])) + ":" + strings.TrimSpace(anyToString(rows[j]["path"]))
		return left < right
	})
	totalFallbacks := anyToInt(ownership["fallbacks"], 0)
	ok := len(violations) == 0 && totalFallbacks == 0
	status := "clean"
	if len(violations) > 0 {
		status = "route_violation"
	} else if totalFallbacks > 0 {
		status = "historical_fallback_detected"
	}
	return map[string]any{
		"ok":                     ok,
		"schema_id":              "strict_runtime_native_ownership.v1",
		"generatedAt":            nowUTCISO(),
		"status":                 status,
		"strictNoPython":         s.strictNoPythonRuntime,
		"routeOwnerClass":        sourceOwnerGoNative,
		"requiredRouteCount":     len(routes),
		"nativeRouteCount":       len(routes) - len(violations),
		"violationCount":         len(violations),
		"surfaces":               bySurface,
		"routes":                 rows,
		"violations":             violations,
		"pythonHotPathOwnership": ownership,
		"forbidden_error_markers": []string{
			"python_runtime_disabled",
			"strict_runtime_backend_forward_disabled",
			"python_hot_path_fallback_blocked",
			"array_above_max_length",
			"context length exceeded",
		},
		"agent_ready": map[string]any{
			"ok":             ok,
			"recommended":    "Agents can rely on ContextLattice only when ok=true and violationCount=0.",
			"doctor_command": "contextlattice_strict_runtime_native_ownership --pretty",
		},
	}
}

func (s *server) opsNativeOwnership(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.nativeOwnershipPayload())
}

func (s *server) telemetrySidecarHealthRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	memoryBankStatus, memoryBankEnabled := qdrantWriteFanoutPreflightStatus()
	if memoryBankStatus == "" && memoryBankEnabled {
		memoryBankStatus = "ready"
	}
	if memoryBankStatus == "" {
		memoryBankStatus = "disabled"
	}
	fastembedGate := _inferenceFastembedGateStatus()
	fastembedStatus := "blocked"
	if anyToBool(fastembedGate["effectivePassed"]) || anyToBool(fastembedGate["passed"]) {
		fastembedStatus = "healthy"
	} else if !anyToBool(fastembedGate["required"]) && !anyToBool(fastembedGate["available"]) {
		fastembedStatus = "optional_missing"
	}
	backendPolicy := defaultRustBackendPolicy()
	unhealthyMarker := strings.Contains(strings.ToLower(memoryBankStatus), "error") ||
		strings.Contains(strings.ToLower(memoryBankStatus), "failed") ||
		strings.Contains(strings.ToLower(memoryBankStatus), "unhealthy")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"healthy":      !unhealthyMarker,
		"detail":       "native strict-runtime telemetry projection",
		"history":      []any{},
		"updatedAt":    nowUTCISO(),
		"source":       "gateway-go",
		"runtimeOwner": sourceOwnerGoNative,
		"gateway-go": map[string]any{
			"status": "healthy",
			"detail": "native gateway is serving strict runtime telemetry",
			"owner":  sourceOwnerGoNative,
		},
		"orchestrator-go": map[string]any{
			"status": "healthy",
			"detail": "Go orchestrator front door is active",
			"owner":  sourceOwnerGoNative,
		},
		"fastembed-rs": map[string]any{
			"status": fastembedStatus,
			"detail": "Rust embedding sidecar gate",
			"owner":  sourceOwnerRustNative,
			"gate":   fastembedGate,
		},
		"memory-bank-spike-rs": map[string]any{
			"status": "healthy",
			"detail": "backend=" + anyToString(backendPolicy["memory_bank_backend"]),
			"owner":  sourceOwnerRustNative,
		},
		"qdrant": map[string]any{
			"status": memoryBankStatus,
			"detail": "native vector fanout/read lane",
			"owner":  sourceOwnerGoNative,
		},
	})
}

func (s *server) telemetryStrategiesRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"updatedAt":    nowUTCISO(),
		"source":       "gateway-go",
		"runtimeOwner": sourceOwnerGoNative,
		"strategies":   []any{},
		"summary": map[string]any{
			"status":              "no_strategy_samples",
			"strategyCount":       0,
			"activeStrategyCount": 0,
			"runtimeOwner":        sourceOwnerGoNative,
			"strictRuntime":       s.strictNoPythonRuntime,
		},
	})
}

func (s *server) telemetryStrategiesHistoryRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	limit := parseOptionalIntQuery(r.URL.Query().Get("limit"), 50, 1, 500)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"updatedAt":    nowUTCISO(),
		"source":       "gateway-go",
		"runtimeOwner": sourceOwnerGoNative,
		"limit":        limit,
		"history":      []any{},
		"summary": map[string]any{
			"status":       "no_strategy_history_samples",
			"sampleCount":  0,
			"runtimeOwner": sourceOwnerGoNative,
		},
	})
}
