package main

import (
	"context"
	"net/http"
	"strings"
)

func (s *server) statusPayload() map[string]any {
	services := s.strictRuntimeServices()
	healthyServiceCount := 0
	for _, row := range services {
		if anyToBool(row["healthy"]) {
			healthyServiceCount++
		}
	}
	return map[string]any{
		"ok":                            true,
		"statusSource":                  "gateway-go",
		"backendStatusSource":           "disabled_by_strict_runtime",
		"routeOwnerClass":               sourceOwnerGoNative,
		"pythonHotPathOwnership":        s.pythonHotPathOwnershipSnapshot(),
		"gatewayPythonHotPathOwnership": s.pythonHotPathOwnershipSnapshot(),
		"backendPythonHotPathOwnership": map[string]any{"status": "disabled_by_strict_runtime", "fallbacks": 0},
		"strictNoPythonRuntime":         true,
		"sourceOwnershipMode":           s.retrieval.sourceOwnershipMode,
		"services":                      services,
		"serviceHealth": map[string]any{
			"healthy": healthyServiceCount,
			"total":   len(services),
		},
		"runtimeBackendPolicy":    defaultRustBackendPolicy(),
		"retrievalFastSources":    append([]string{}, s.retrieval.fastSources...),
		"retrievalSlowSources":    append([]string{}, s.retrieval.slowSources...),
		"retrievalDefaultSources": append([]string{}, s.retrieval.defaultSources...),
		"metadataContract":        metadataContractSnapshot(),
	}
}

func (s *server) agentPreflightNative(ctx context.Context, headers http.Header, reqBody agentPreflightRequest, objectiveCtx objectiveContext) map[string]any {
	healthPayload := map[string]any{"ok": true, "service": "gateway-go", "timestamp": nowUTCISO()}
	statusPayload := s.statusPayload()
	if objectiveCtx.empty() {
		objectiveCtx = objectiveContextFromPreflightRequest(reqBody, nil)
	}
	scopedSearchReq := map[string]any{
		"project":                 reqBody.Project,
		"query":                   reqBody.Query,
		"topic_path":              reqBody.TopicPath,
		"retrieval_mode":          reqBody.RetrievalMode,
		"include_grounding":       true,
		"include_retrieval_debug": true,
		"agent_id":                reqBody.AgentID,
		"session_id":              reqBody.SessionID,
		"objective_context":       objectiveCtx.toMap(),
	}
	scopedPayload, scopedStatus, scopedErr := s.executeRetrieval(ctx, headers, scopedSearchReq, true)

	var broadenedPayload map[string]any
	broadenedStatus := 0
	var broadenedErr error
	needsBroaden := scopedErr != nil
	if !needsBroaden && scopedPayload != nil {
		if anyToBool(scopedPayload["degraded"]) || resultCount(scopedPayload) == 0 {
			needsBroaden = true
		}
	}
	if needsBroaden {
		broadenedReq := cloneAnyMap(scopedSearchReq)
		delete(broadenedReq, "topic_path")
		broadenedPayload, broadenedStatus, broadenedErr = s.executeRetrieval(ctx, headers, broadenedReq, true)
	}

	contextPackReq := map[string]any{
		"project":                 reqBody.Project,
		"query":                   reqBody.Query,
		"topic_path":              reqBody.TopicPath,
		"retrieval_mode":          reqBody.RetrievalMode,
		"include_retrieval_debug": true,
		"agent_id":                reqBody.AgentID,
		"session_id":              reqBody.SessionID,
		"objective_context":       objectiveCtx.toMap(),
	}
	contextPackPayload, contextPackStatus, contextPackErr := s.buildContextPackResponse(ctx, headers, contextPackReq)
	missionQuery := "mission objective goal cross-project synthesis longitudinal learning policy context package retrieval discipline"
	missionReq := map[string]any{
		"project":                 reqBody.Project,
		"query":                   missionQuery,
		"topic_path":              reqBody.TopicPath,
		"retrieval_mode":          reqBody.RetrievalMode,
		"include_retrieval_debug": true,
		"agent_id":                reqBody.AgentID,
		"session_id":              reqBody.SessionID,
		"objective_context":       objectiveCtx.toMap(),
	}
	missionPackPayload, missionPackStatus, missionPackErr := s.buildContextPackResponse(ctx, headers, missionReq)
	if missionPackErr == nil && len(contextPackEvidence(missionPackPayload, 1)) == 0 {
		missionReqBroad := cloneAnyMap(missionReq)
		delete(missionReqBroad, "topic_path")
		missionPackPayload, missionPackStatus, missionPackErr = s.buildContextPackResponse(ctx, headers, missionReqBroad)
	}
	objectiveRuntime := buildObjectiveRuntimeState(
		reqBody.Agent,
		reqBody.AgentID,
		reqBody.Project,
		reqBody.TopicPath,
		reqBody.Query,
		reqBody.RetrievalMode,
		reqBody.SessionID,
		objectiveCtx,
		"agent.preflight.completed",
	)
	policyPack := buildPolicyContextPackage(reqBody.Agent, reqBody.AgentID, reqBody.Project, reqBody.TopicPath, reqBody.Query, reqBody.RetrievalMode, contextPackPayload, missionPackPayload, missionPackErr, objectiveRuntime, objectiveCtx)
	return map[string]any{
		"ok":                  true,
		"agent":               strings.TrimSpace(reqBody.Agent),
		"agent_id":            reqBody.AgentID,
		"session_id":          reqBody.SessionID,
		"orchestrator_url":    "http://127.0.0.1:8075",
		"objective_context":   objectiveCtx.toMap(),
		"objective_runtime":   objectiveRuntime,
		"objective_hierarchy": objectiveRuntime["objective_hierarchy"],
		"objective_lineage":   objectiveRuntime["objective_lineage"],
		"health":              healthPayload,
		"status":              statusPayload,
		"scoped_search": map[string]any{
			"ok":             scopedErr == nil,
			"query":          reqBody.Query,
			"project":        reqBody.Project,
			"retrieval_mode": reqBody.RetrievalMode,
			"results":        scopedPayload,
			"status":         scopedStatus,
			"error":          errString(scopedErr),
		},
		"broadened_search": map[string]any{
			"attempted": needsBroaden,
			"status":    broadenedStatus,
			"results":   broadenedPayload,
			"error":     errString(broadenedErr),
		},
		"context_pack": map[string]any{
			"status":  contextPackStatus,
			"payload": contextPackPayload,
			"error":   errString(contextPackErr),
		},
		"mission_pack": map[string]any{
			"query":   missionQuery,
			"status":  missionPackStatus,
			"payload": missionPackPayload,
			"error":   errString(missionPackErr),
		},
		"policy_context_package": policyPack,
	}
}
