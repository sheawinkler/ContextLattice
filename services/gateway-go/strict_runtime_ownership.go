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
	routes := []nativeOwnedRoute{
		{Path: "/health", Surface: "runtime", Owner: sourceOwnerGoNative, Status: "native", Detail: "gateway liveness and queue health", Required: true},
		{Path: "/status", Surface: "runtime", Owner: sourceOwnerGoNative, Status: "native", Detail: "strict runtime status and service health", Required: true},
		{Path: "/migration/runtime", Surface: "runtime", Owner: sourceOwnerGoNative, Status: "native", Detail: "Go/Rust migration flags", Required: true},
		{Path: "/ops/capabilities", Surface: "runtime", Owner: sourceOwnerGoNative, Status: "native", Detail: "agent-facing capability map", Required: true},
		{Path: "/ops/context-boundary", Surface: "runtime", Owner: sourceOwnerGoNative, Status: "native", Detail: "agent-facing context boundary audit", Required: true},
		{Path: "/ops/native-ownership", Surface: "runtime", Owner: sourceOwnerGoNative, Status: "native", Detail: "strict runtime native route audit", Required: true},
		{Path: "/ops/queue/status", Surface: "async", Owner: sourceOwnerGoNative, Status: "native", Detail: "continuation queue and deadletter status", Required: true},
		{Path: "/memory/search", Surface: "retrieval", Owner: sourceOwnerGoNative, Status: "native", Detail: "staged retrieval with continuation lifecycle", Required: true},
		{Path: "/memory/search/continuations/{token}", Surface: "retrieval", Owner: sourceOwnerGoNative, Status: "native", Detail: "async retrieval polling", Required: true},
		{Path: "/memory/recall/eval-cases", Surface: "retrieval", Owner: sourceOwnerGoNative, Status: "native", Detail: "saved recall evaluation cases", Required: true},
		{Path: "/memory/recall/eval-cases/refresh", Surface: "retrieval", Owner: sourceOwnerGoNative, Status: "native", Detail: "saved recall case refresh", Required: true},
		{Path: "/memory/recall/evaluate/saved", Surface: "retrieval", Owner: sourceOwnerGoNative, Status: "native", Detail: "saved recall evaluation execution", Required: true},
		{Path: "/memory/context-pack", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "bounded prompt-ready context packages", Required: true},
		{Path: agentPacketReconstructionRoute, Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "digest-verified Agent Packet delta reconstruction", Required: true},
		{Path: frontierT4RetrievalReceiptGovernancePath, Surface: "operator", Owner: sourceOwnerGoNative, Status: "native", Detail: "paid retrieval receipt governance discovery without OSS state mutation", Required: true},
		{Path: frontierT4CausalBridgeGovernancePath, Surface: "operator", Owner: sourceOwnerGoNative, Status: "native", Detail: "paid causal bridge governance discovery without OSS state mutation", Required: true},
		{Path: frontierT4CounterfactualEvalPath, Surface: "operator", Owner: sourceOwnerGoNative, Status: "native", Detail: "paid counterfactual operations discovery without OSS state mutation", Required: true},
		{Path: frontierT4EvidenceReputationPath, Surface: "operator", Owner: sourceOwnerGoNative, Status: "native", Detail: "paid evidence reputation activation discovery without OSS state mutation", Required: true},
		{Path: frontierT4RetrievalRegressionPath, Surface: "operator", Owner: sourceOwnerGoNative, Status: "native", Detail: "paid regression operations discovery without OSS state mutation", Required: true},
		{Path: frontierT4DefenseOperationsPath, Surface: "operator", Owner: sourceOwnerGoNative, Status: "native", Detail: "paid defense operations discovery without weakening OSS defenses", Required: true},
		{Path: policySimulationPath, Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "same-snapshot no-persist retrieval policy replay", Required: true},
		{Path: scopedPolicyCardPath, Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "project and task-scoped sparse-data policy card", Required: true},
		{Path: policyPromotionRecommendationPath, Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "assignment-bound advisory policy promotion recommendation", Required: true},
		{Path: memoryRetirementPath, Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "explicit reversible non-destructive memory retirement", Required: true},
		{Path: contradictionResolutionPath, Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "evidence-weighted contradiction recommendation and appeal", Required: true},
		{Path: storageTemperatureDecisionPath, Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "utility-based reversible retrieval temperature decision", Required: true},
		{Path: frontierT5StatusPath, Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "Policy Laboratory lifecycle and storage telemetry", Required: true},
		{Path: frontierT6SteeringPath, Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "bounded async steering replay and acknowledgement", Required: true},
		{Path: frontierT6SteeringEventsPath, Surface: "agent_stream", Owner: sourceOwnerGoNative, Status: "native", Detail: "resumable SSE steering delivery with pull fallback", Required: true},
		{Path: frontierT6SelectionPath, Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "advisor-only runner and model selection", Required: true},
		{Path: frontierT6ProfilePath, Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "effective agent context profile resolution", Required: true},
		{Path: frontierT6ContextPrepPath, Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "opt-in external-worker context preparation", Required: true},
		{Path: frontierT6TelemetryPath, Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "bounded Agent Fit runtime status", Required: true},
		{Path: "/tools/context_pack", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "MCP/tool context package wrapper", Required: true},
		{Path: "/memory/continuity/reconcile", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "exact-first task identity reconciliation with semantic abstention", Required: true},
		{Path: "/memory/objectives/transition", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "append-only typed objective transition", Required: true},
		{Path: "/memory/objectives/graph", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "as-of longitudinal objective graph", Required: true},
		{Path: "/memory/decision-changes", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "evidence-linked decision provenance", Required: true},
		{Path: "/memory/synthesis-pack", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "deterministic retrieval synthesis pack", Required: true},
		{Path: "/tools/synthesis_pack", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "MCP/tool retrieval synthesis wrapper", Required: true},
		{Path: "/memory/synthesis-pack/v2", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "proof-carrying deterministic synthesis", Required: true},
		{Path: "/tools/synthesis_pack_v2", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "tool proof-carrying synthesis wrapper", Required: true},
		{Path: "/memory/retrieval/plan", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "advisor-only adaptive retrieval plan", Required: true},
		{Path: "/tools/retrieval_plan", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "tool retrieval-plan wrapper", Required: true},
		{Path: "/memory/claims", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "temporal claim write", Required: true},
		{Path: "/memory/claims/query", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "temporal claim query", Required: true},
		{Path: "/tools/claim_write", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "tool temporal claim write", Required: true},
		{Path: "/tools/claim_query", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "tool temporal claim query", Required: true},
		{Path: "/telemetry/claim-graph", Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "temporal claim graph status", Required: true},
		{Path: "/memory/context-policy/candidate", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "outcome-trained advisory policy candidate", Required: true},
		{Path: "/memory/context-policy/evaluate", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "one-step shadow and canary policy evaluation", Required: true},
		{Path: "/tools/context_policy_candidate", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "tool advisory policy candidate", Required: true},
		{Path: "/tools/context_policy_evaluate", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "tool one-step policy evaluation", Required: true},
		{Path: "/telemetry/context-policy", Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "advisory context-policy lifecycle telemetry", Required: true},
		{Path: "/memory/skills/foundry/draft", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "verified workflow skill drafting", Required: true},
		{Path: "/memory/skills/foundry/evaluate", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "independent skill holdout evaluation", Required: true},
		{Path: "/memory/skills/foundry/export", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "human-approved inactive skill export", Required: true},
		{Path: "/memory/skills/foundry/retire", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "immutable inactive-draft retirement", Required: true},
		{Path: frontierT8SkillEvolutionPath, Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "verified reusable-skill and non-terminal retirement candidates", Required: true},
		{Path: frontierT9ContinuityZeroPath, Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "path-free fail-closed active-objective continuity manifest", Required: true},
		{Path: frontierT10AggregatePath, Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "bounded opt-in local Aggregate Signal preview, accounting, reports, and opt-out", Required: true},
		{Path: "/tools/skill_foundry_draft", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "tool verified workflow skill drafting", Required: true},
		{Path: "/tools/skill_foundry_evaluate", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "tool independent skill holdout evaluation", Required: true},
		{Path: "/tools/skill_foundry_export", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "tool human-approved inactive skill export", Required: true},
		{Path: "/telemetry/skills/foundry", Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "Skill Foundry draft and evaluation telemetry", Required: true},
		{Path: "/memory/context-passport/export", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "signed bounded context passport export", Required: true},
		{Path: "/memory/context-passport/verify", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "context passport signature and expiry verification", Required: true},
		{Path: "/memory/context-passport/diff", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "deterministic context passport diff", Required: true},
		{Path: "/memory/context-passport/replay", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "validated non-executing passport replay", Required: true},
		{Path: "/memory/context-passport/import", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "explicit conflict-safe passport import", Required: true},
		{Path: "/telemetry/context-passport", Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "bounded passport lineage and storage telemetry", Required: true},
		{Path: "/memory/context-mesh/identity", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "public mesh identity without private key export", Required: true},
		{Path: "/memory/context-mesh/grants", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "signed project-scoped mesh recipient grants", Required: true},
		{Path: "/memory/context-mesh/grants/revoke", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "local mesh grant revocation tombstone", Required: true},
		{Path: "/memory/context-mesh/export", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "transport-neutral age X25519 envelope export", Required: true},
		{Path: "/memory/context-mesh/import", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "decrypt, verify, and reconcile mesh envelope", Required: true},
		{Path: "/telemetry/context-mesh", Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "mesh grant, receipt, and transport-boundary telemetry", Required: true},
		{Path: frontierT7GrantsPath, Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "signed least-privilege portable continuation grants", Required: true},
		{Path: frontierT7ImportsPath, Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "provenance-preserving external-worker imports", Required: true},
		{Path: frontierT7ManifestsPath, Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "signed encrypted cross-machine continuation manifests", Required: true},
		{Path: frontierT7TelemetryPath, Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "bounded path-free portable continuation status", Required: true},
		{Path: "/memory/dream", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "LLM-backed Dream Mode runtime", Required: true},
		{Path: "/tools/dream", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "MCP/tool LLM-backed Dream Mode wrapper", Required: true},
		{Path: "/memory/review", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "deterministic review mode runtime", Required: true},
		{Path: "/tools/review", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "MCP/tool review mode wrapper", Required: true},
		{Path: "/v1/agents/preflight", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "generic agent preflight", Required: true},
		{Path: "/v1/codex/preflight", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "Codex-compatible preflight", Required: true},
		{Path: "/v1/agents/sessions", Surface: "agent", Owner: sourceOwnerGoNative, Status: "native", Detail: "agent runtime session ledger", Required: true},
		{Path: "/telemetry/agents/runtime", Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "agent runtime telemetry", Required: true},
		{Path: "/telemetry/metrics", Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "native metrics and embedding cache telemetry", Required: true},
		{Path: "/telemetry/token-impact", Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "context-pack token impact samples and aggregate prompt economics", Required: true},
		{Path: "/telemetry/context-pack-quality", Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "context-pack quality, counterfactual inference avoidance, and outcome calibration", Required: true},
		{Path: evidenceReputationPath, Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "bounded local evidence reputation from independently verified attribution", Required: true},
		{Path: utilityTelemetryPath, Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "bounded verified utility ledger and exact token economics", Required: true},
		{Path: utilityAnalyticsPath, Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "entitlement-gated utility cohorts and task-class economics", Required: true},
		{Path: utilityPolicyPath, Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "entitlement-gated advisory utility policy evaluation", Required: true},
		{Path: "/telemetry/runner-quality", Surface: "telemetry", Owner: sourceOwnerGoNative, Status: "native", Detail: "adapter runner quality samples and advisor-only recommendations", Required: true},
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
	return routes
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
		"registry_id":            GeneratedAgentContractRegistryID,
		"registry_version":       GeneratedAgentContractRegistryVersion,
		"generatedAt":            nowUTCISO(),
		"build":                  contextLatticeBuildIdentity(),
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
