package main

import (
	"net/http"
	"strings"
)

const (
	memoryRecallResponsePath = "/memory/recall/response"
	toolsRecallResponsePath  = "/tools/recall_response"
)

// recallResponseRequestFields is intentionally a closed allowlist. The recall
// response surface may reuse the context-pack retrieval/compiler controls, but
// it must never pass through caller-supplied transport, packet, projection,
// evidence, or internal ledger fields.
var recallResponseRequestFields = []string{
	"query",
	"project",
	"topic_path",
	"limit",
	"max_facts",
	"retrieval_mode",
	"retrieval_intent",
	"task_class",
	"agent_id",
	"agentId",
	"session_id",
	"sessionId",
	"task_id",
	"taskId",
	"task_identity_id",
	"taskIdentityId",
	"execution_lane_id",
	"executionLaneId",
	"workspace_ref",
	"workspace_id",
	"workspaceId",
	"user_id",
	"include_preferences",
	"sources",
	"source_weights",
	"auto_escalate",
	"query_expansion",
	"include_ephemeral",
	"include_ephemeral_memory",
	"include_test_memory",
	"combined_sources",
	"blocking",
	"wait_for_slow_sources",
	"sync_slow_sources",
	"target_context_pack_tokens",
	"targetContextPackTokens",
	"budget_tokens",
	"budgetTokens",
	"agent_context_budget_tokens",
	"model_context_window_tokens",
	"reserved_response_tokens",
	"already_loaded_tokens",
	"ranked_evidence_tokens",
	"tokenizer_encoding",
	"tokenizer_exact",
	"objective_context",
}

func recallResponseRequestPayload(payload map[string]any) map[string]any {
	request := make(map[string]any, len(recallResponseRequestFields)+2)
	for _, key := range recallResponseRequestFields {
		if value, ok := payload[key]; ok {
			request[key] = cloneJSONValue(value)
		}
	}
	// The underlying context-pack compiler may still account for its own
	// bounded work, but this response surface owns the final projection and
	// must not record a second transport token-impact sample.
	request["include_retrieval_debug"] = false
	request["_suppress_token_impact_recording"] = true
	request["_suppress_final_token_impact_recording"] = true
	return request
}

func recallResponseCompositionInput(request, contextResponse map[string]any) map[string]any {
	input := map[string]any{
		"query":                    request["query"],
		"project":                  request["project"],
		"topic_path":               request["topic_path"],
		"agent_id":                 firstNonEmptyStrings(anyToString(request["agent_id"]), anyToString(request["agentId"])),
		"session_id":               firstNonEmptyStrings(anyToString(request["session_id"]), anyToString(request["sessionId"])),
		"task_id":                  firstNonEmptyStrings(anyToString(request["task_id"]), anyToString(request["taskId"])),
		"task_identity_id":         firstNonEmptyStrings(anyToString(request["task_identity_id"]), anyToString(request["taskIdentityId"])),
		"execution_lane_id":        firstNonEmptyStrings(anyToString(request["execution_lane_id"]), anyToString(request["executionLaneId"])),
		"workspace_ref":            request["workspace_ref"],
		"workspace_id":             request["workspace_id"],
		"workspaceId":              request["workspaceId"],
		"retrieval_intent":         request["retrieval_intent"],
		"retrieval_mode":           request["retrieval_mode"],
		"context_pack":             contextResponse["context_pack"],
		"source_coverage":          contextResponse["source_coverage"],
		"memory_trust_assessment":  contextResponse["memory_trust_assessment"],
		"retrieval_decision_trace": contextResponse["retrieval_decision_trace"],
		"context_pack_quality":     contextResponse["context_pack_quality"],
	}
	// No caller-controlled classification, evidence, conflicts, or durable
	// quality envelope crosses this boundary. The composer derives those values
	// only from the server-produced context-pack projection.
	return input
}

// recallResponseCompositionInputFromCompilation projects the exact retrieval
// and compilation artifacts received by the pre-persistence hook. The native
// recall route must not perform a second retrieval (or rebuild its evidence
// from the returned context-pack transport envelope), because learned fallback
// and durable attribution both depend on the artifact set for this attempt.
func recallResponseCompositionInputFromCompilation(
	request map[string]any,
	input contextPackCompilationInput,
	artifacts contextPackCompilationArtifacts,
	durable bool,
) map[string]any {
	qualityResponse := cloneJSONMap(artifacts.Quality)
	delete(qualityResponse, "selection_receipt")
	// Response binding is an internal quality-ledger field. It is useful to the
	// hook and durable row, but it must never become part of the closed response
	// projection (including through a nested context-pack quality copy).
	for _, key := range []string{"recall_response_id", "recall_response_digest", "response_component_refs"} {
		delete(qualityResponse, key)
	}
	contextPack := cloneJSONMap(artifacts.ContextPack)
	contextPack["contextPackQuality"] = qualityResponse
	contextPack["context_pack_quality"] = qualityResponse
	contextResponse := map[string]any{
		"context_pack":             contextPack,
		"source_coverage":          input.SourceCoverage,
		"memory_trust_assessment":  artifacts.TrustAssessment,
		"retrieval_decision_trace": artifacts.DecisionTrace,
		"context_pack_quality":     qualityResponse,
	}
	composition := recallResponseCompositionInput(request, contextResponse)
	// The normalized compilation input is the authoritative scope for this
	// attempt. Request aliases are retained only as input to the closed-field
	// request normalizer above.
	composition["query"] = input.Query
	composition["project"] = input.Project
	composition["topic_path"] = input.TopicPath
	composition["agent_id"] = input.AgentID
	composition["session_id"] = input.SessionID
	composition["retrieval_mode"] = input.RetrievalMode
	composition["retrieval_intent"] = input.RetrievalIntent
	// Compilation owns the canonical workspace. Caller aliases may select no
	// ownership scope and must never relabel server-produced evidence.
	composition["workspace_ref"] = input.WorkspaceRef
	delete(composition, "workspace_id")
	delete(composition, "workspaceId")
	if durable {
		// This is an internal candidate for attribution only. The persisted row
		// is re-read after the append before its outcome can become public.
		composition["_durable_context_pack_quality"] = cloneJSONMap(artifacts.Quality)
	} else {
		delete(composition, "_durable_context_pack_quality")
	}
	return composition
}

func recallResponseSemanticDigest(payload map[string]any) string {
	material := cloneJSONMap(payload)
	delete(material, "format_contract")
	delete(material, "response_digest")
	return "sha256:" + sha256Hex(recallResponseCanonicalJSON(material))
}

func finalizeRecallResponseTransport(payload map[string]any, agentID, lane, endpoint string) map[string]any {
	delete(payload, "format_contract")
	// Attach the contract first so its shared projection has finished clipping
	// semantic fields. Both semantic identities are then recomputed because a
	// future clipping pass may change component or evidence material. Their
	// fixed-width replacements cannot change the stamped byte count.
	payload = attachPayloadFormatContract(recallResponseContractID, payload, agentID, lane, endpoint)
	payload["response_id"] = recallResponseIDForResponse(payload)
	payload["response_digest"] = recallResponseSemanticDigest(payload)
	return payload
}

func (s *server) memoryRecallResponse(w http.ResponseWriter, r *http.Request) {
	s.recallResponseRoute(w, r, false)
}

func (s *server) toolsRecallResponse(w http.ResponseWriter, r *http.Request) {
	s.recallResponseRoute(w, r, true)
}

func (s *server) recallResponseRoute(w http.ResponseWriter, r *http.Request, tool bool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	var incomingHeaders http.Header
	var ok bool
	endpoint := memoryRecallResponsePath
	if tool {
		endpoint = toolsRecallResponsePath
		incomingHeaders, ok = s.prepareToolHeaders(w, r, endpoint)
	} else {
		incomingHeaders, ok = s.prepareAuthorizedHeaders(w, r)
	}
	if !ok {
		return
	}

	bodyBytes, ok := readResponseIntelligenceRequestBody(w, r)
	if !ok {
		return
	}
	payload, err := parseJSONMap(bodyBytes)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	request := recallResponseRequestPayload(payload)
	requestCtx := r.Context()
	requestCtx = s.contextWithContextPackLearnedRequestAuthority(requestCtx, r, request, bodyBytes)
	agentID := strings.TrimSpace(firstNonEmptyStrings(anyToString(request["agent_id"]), anyToString(request["agentId"])))
	var hookedResponse map[string]any
	var hookedBinding map[string]any
	var hookedDurable bool
	var hookedSampleID string
	var hookedReceiptDigest string
	compilationHook := func(input contextPackCompilationInput, artifacts contextPackCompilationArtifacts, durable bool) contextPackCompilationArtifacts {
		if !durable {
			// A failed durable attempt may have copied a provisional binding into
			// the quality map. Local fallback rows are explicitly unbound.
			for _, key := range []string{"recall_response_id", "recall_response_digest", "response_component_refs"} {
				delete(artifacts.Quality, key)
			}
			delete(artifacts.Quality, "selection_receipt")
		}
		composition := recallResponseCompositionInputFromCompilation(request, input, artifacts, durable)
		response := composeRecallResponse(composition)
		// Apply the recall boundary before deriving identity. The captured
		// response is the exact public projection if this persistence attempt and
		// its retained proof both succeed.
		response = finalizeRecallResponseTransport(response, agentID, "recall_response", endpoint)
		var binding map[string]any
		if durable {
			if canonical, ok := recallResponseBindingFromResponse(response); ok && recallResponseCopyBinding(artifacts.Quality, canonical) {
				binding = canonical
			} else {
				// A response that cannot produce a complete binding is not allowed
				// to enter a durable quality row. Removing its receipt forces the
				// existing persistence path through the receipt-free fallback.
				for _, key := range []string{"recall_response_id", "recall_response_digest", "response_component_refs"} {
					delete(artifacts.Quality, key)
				}
				delete(artifacts.Quality, "selection_receipt")
				delete(composition, "_durable_context_pack_quality")
				response = composeRecallResponse(composition)
				response = finalizeRecallResponseTransport(response, agentID, "recall_response", endpoint)
			}
		}
		hookedResponse = response
		hookedBinding = binding
		hookedDurable = durable && binding != nil
		hookedSampleID = anyToString(artifacts.Quality["sample_id"])
		hookedReceiptDigest = ""
		if durable {
			hookedReceiptDigest = anyToString(contextPackSelectionReceiptFromSample(artifacts.Quality["selection_receipt"])["receipt_digest"])
		}
		return artifacts
	}
	contextResponse, status, execErr := s.buildContextPackResponseForSurfaceWithOptions(
		requestCtx, incomingHeaders, request, "recall_response", contextPackResponseBuildOptions{compilationHook: compilationHook},
	)

	if execErr != nil {
		contextResponse = map[string]any{
			"source_coverage": map[string]any{
				"complete": false,
				"failed":   []any{"context_pack"},
			},
		}
		status = http.StatusBadGateway
	}
	if contextResponse == nil {
		contextResponse = map[string]any{
			"source_coverage": map[string]any{
				"complete": false,
				"failed":   []any{"context_pack"},
			},
		}
	}
	if status >= http.StatusBadRequest || !anyToBool(contextResponse["ok"]) {
		// The response contract is also the safe failure surface: do not copy
		// the context-pack error envelope or any backend detail into it.
		contextResponse = map[string]any{
			"source_coverage": map[string]any{
				"complete": false,
				"failed":   []any{"context_pack"},
			},
		}
		if status < http.StatusBadRequest {
			status = http.StatusBadGateway
		}
	}

	response := hookedResponse
	if response == nil || status >= http.StatusBadRequest || !anyToBool(contextResponse["ok"]) {
		response = composeRecallResponse(recallResponseCompositionInput(request, contextResponse))
		response = finalizeRecallResponseTransport(response, agentID, "recall_response", endpoint)
		hookedDurable = false
		hookedBinding = nil
	}
	if hookedDurable && hookedBinding != nil && s != nil && s.contextPackQuality != nil {
		// Durable attribution is admitted only from the exact retained quality
		// row and receipt, never from the hook's in-memory candidate alone.
		sample, found, sampleErr := s.contextPackQuality.durableQualitySampleForOutcome(hookedSampleID)
		receiptSample, receiptFound := s.contextPackQuality.durableReceiptSampleForUtility(hookedSampleID)
		retainedBinding, bindingOK := recallResponseBindingFromSample(sample)
		retainedReceipt := contextPackSelectionReceiptFromSample(receiptSample["selection_receipt"])
		if sampleErr != nil || !found || !bindingOK || retainedBinding == nil || !utilitySHA256DigestValid(hookedReceiptDigest) ||
			!recallResponseBindingsEqual(hookedBinding, retainedBinding) ||
			!recallResponseBindingMatchesResponse(response, retainedBinding) || !receiptFound || len(retainedReceipt) == 0 ||
			(hookedReceiptDigest != "" && anyToString(retainedReceipt["receipt_digest"]) != hookedReceiptDigest) {
			// If the retained durable proof is unavailable or changed, publish a
			// fresh unbound projection from the same artifacts. No retrieval is
			// repeated and no failed identity is reused.
			for _, key := range []string{"recall_response_id", "recall_response_digest", "response_component_refs"} {
				delete(anyMap(contextResponse["context_pack_quality"]), key)
			}
			response = composeRecallResponse(recallResponseCompositionInput(request, contextResponse))
			response = finalizeRecallResponseTransport(response, agentID, "recall_response", endpoint)
		}
	}
	w.Header().Set("X-ContextLattice-Native-Route", "recall_response")
	if tool {
		w.Header().Set("X-ContextLattice-Tool", "recall_response")
	}
	writeJSON(w, status, response)
}
