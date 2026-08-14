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
	"response_shape",
	"project",
	"topic_path",
	"limit",
	"max_facts",
	"retrieval_mode",
	"retrieval_intent",
	"task_class",
	"as_of",
	"asOf",
	"consequence_hint",
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
		"retrieval_intent":         request["retrieval_intent"],
		"retrieval_mode":           request["retrieval_mode"],
		"task_class":               request["task_class"],
		"as_of":                    firstNonEmptyStrings(anyToString(request["as_of"]), anyToString(request["asOf"])),
		"consequence_hint":         request["consequence_hint"],
		"context_pack":             contextResponse["context_pack"],
		"source_coverage":          contextResponse["source_coverage"],
		"memory_trust_assessment":  contextResponse["memory_trust_assessment"],
		"retrieval_decision_trace": contextResponse["retrieval_decision_trace"],
		"context_pack_quality":     contextResponse["context_pack_quality"],
	}
	// No caller-controlled ownership, classification, evidence, conflicts, or
	// durable quality envelope crosses this boundary. Successful compilation
	// installs its authoritative workspace below; pre-compilation failure stays
	// explicitly unowned instead of stamping a caller-provided workspace.
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
	composition["task_class"] = input.TaskClass
	// Compilation owns the canonical workspace. Caller aliases may select no
	// ownership scope and must never relabel server-produced evidence.
	composition["workspace_ref"] = input.WorkspaceRef
	delete(composition, "workspace_id")
	delete(composition, "workspaceId")
	if len(artifacts.ServerProactiveObservation) > 0 {
		// This private carrier is filled by the compiler from the server's
		// materialized retrieval response. It never crosses the public response
		// boundary and cannot be supplied by the caller allowlist.
		composition["_server_proactive_observation"] = cloneJSONMap(artifacts.ServerProactiveObservation)
	}
	if durable {
		// This is an internal candidate for attribution only. The persisted row
		// is re-read after the append before its outcome can become public.
		composition["_durable_context_pack_quality"] = cloneJSONMap(artifacts.Quality)
	} else {
		delete(composition, "_durable_context_pack_quality")
	}
	return composition
}

// recallResponseServerPolicyInputFromCompilation turns one normalized,
// server-owned selection receipt into a typed evidence-binding policy. The
// public request allowlist cannot construct this value. A valid-looking
// candidate ID, source, or digest that is absent from the receipt remains
// derived and unbound.
func recallResponseServerPolicyInputFromCompilation(
	composition map[string]any,
	artifacts contextPackCompilationArtifacts,
	durable bool,
) validatedRecallResponsePolicyInput {
	policy := recallResponseProductionPolicyInput()
	if !durable {
		return policy
	}
	receipt := contextPackSelectionReceiptFromSample(artifacts.Quality["selection_receipt"])
	receiptDigest := anyToString(receipt["receipt_digest"])
	if !recallResponseValidDigest(receiptDigest) {
		return policy
	}
	allowed := map[string]bool{}
	for _, raw := range contextPackAnyList(receipt["candidates"]) {
		row := anyMap(raw)
		if anyToString(row["selection_state"]) != "selected" {
			continue
		}
		if ref := contextPackOpaqueCandidateRef(row["candidate_ref"]); ref != "" {
			allowed[ref] = true
		}
	}
	snapshotMaterial := map[string]any{
		"context_pack":             composition["context_pack"],
		"source_coverage":          composition["source_coverage"],
		"memory_trust_assessment":  composition["memory_trust_assessment"],
		"retrieval_decision_trace": composition["retrieval_decision_trace"],
		"workspace_ref":            composition["workspace_ref"],
		"receipt_digest":           receiptDigest,
	}
	policy.sourceBound = true
	policy.snapshotDigest = "sha256:" + sha256Hex(recallResponseCanonicalJSON(snapshotMaterial))
	policy.receiptDigest = receiptDigest
	policy.evidenceBindings = recallResponseValidatedEvidenceBindings(composition, "server_receipt", allowed)
	return policy
}

func recallResponseSemanticDigest(payload map[string]any) string {
	material := recallResponseStableIdentityMaterial(payload)
	delete(material, "format_contract")
	delete(material, "response_digest")
	delete(material, recallResponseFallbackStageReceiptKey)
	return "sha256:" + sha256Hex(recallResponseCanonicalJSON(material))
}

func (s *server) recallResponseRoutePolicyFor(request map[string]any) (validatedRecallResponsePolicyInput, bool) {
	if s == nil {
		return validatedRecallResponsePolicyInput{}, false
	}
	requestDigest := recallResponseContinuationRequestDigest(request)
	if !recallResponseValidDigest(requestDigest) {
		return validatedRecallResponsePolicyInput{}, false
	}
	s.recallResponseRoutePolicyMu.RLock()
	policy, ok := s.recallResponseRoutePolicyOverrides[requestDigest]
	s.recallResponseRoutePolicyMu.RUnlock()
	return policy, ok
}

// recallResponseStableIdentityMaterial removes transport-only cursor state from
// product identities. Opaque tokens are server state and are bound by the
// continuation receipt; they are never part of the semantic response,
// snapshot/membership, control, or omission identities.
func recallResponseStableIdentityMaterial(payload map[string]any) map[string]any {
	material := cloneJSONMap(payload)
	delete(material, "continuation_action")
	if disclosure := anyMap(material["disclosure"]); len(disclosure) > 0 {
		delete(disclosure, "continuation_action")
	}
	return material
}

func finalizeRecallResponseTransport(payload map[string]any, agentID, lane, endpoint string) map[string]any {
	for attempts := 0; attempts < recallResponseMaxEvidence+recallResponseMaxModules+1; attempts++ {
		// Fallback-stage diagnostics are server-internal and must never cross the
		// agent-facing transport boundary.
		delete(payload, recallResponseFallbackStageReceiptKey)
		delete(payload, "format_contract")
		// Attach the contract first so its shared projection has finished clipping
		// semantic fields. Both semantic identities are then recomputed because a
		// future clipping pass may change component or evidence material. Their
		// fixed-width replacements cannot change the stamped byte count.
		payload = attachPayloadFormatContract(recallResponseContractID, payload, agentID, lane, endpoint)
		if recallResponseRecomputeClippedIdentity(payload) {
			delete(payload, "format_contract")
			payload = attachPayloadFormatContract(recallResponseContractID, payload, agentID, lane, endpoint)
			recallResponseRecomputeClippedIdentity(payload)
		}
		payload["response_id"] = recallResponseIDForResponse(payload)
		payload["response_digest"] = recallResponseSemanticDigest(payload)
		delete(payload, recallResponseFallbackStageReceiptKey)
		compactBytes, compactTokens := recallResponseCompactBudget(payload)
		if compactBytes <= recallResponseMaxCompactBytes && compactTokens <= recallResponseMaxCompactTokens {
			return payload
		}

		// The compact contract applies to the complete agent-facing payload, not
		// only the pre-format candidate. First reduce deterministic explanatory
		// prose while preserving every evidence, proof, union, and omission receipt;
		// only then shed presentation rows or modules if the stamped envelope still
		// cannot fit.
		if recallResponseCompactCursorPresentation(payload) {
			continue
		}
		if recallResponsePruneDerivedInferencePresentation(payload) {
			continue
		}
		proof := anyMap(anyMap(payload["answer"])["proof_spine"])
		if recallResponsePruneLowestUnprovedEvidence(payload, proof) {
			continue
		}
		if recallResponsePruneOptionalSecondaryModule(payload) {
			continue
		}
		break
	}
	delete(payload, recallResponseFallbackStageReceiptKey)
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
	if _, continuation := payload["continuation_token"]; continuation {
		response, status := s.resolveRecallResponseContinuation(payload, endpoint)
		w.Header().Set("X-ContextLattice-Native-Route", "recall_response")
		if tool {
			w.Header().Set("X-ContextLattice-Tool", "recall_response")
		}
		writeJSON(w, status, response)
		return
	}
	responseShape := strings.TrimSpace(anyToString(payload["response_shape"]))
	if responseShape != "" && responseShape != recallResponseInitialShape {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid response_shape"})
		return
	}
	request := recallResponseRequestPayload(payload)
	requestCtx := r.Context()
	requestCtx = s.contextWithContextPackLearnedRequestAuthority(requestCtx, r, request, bodyBytes)
	agentID := strings.TrimSpace(firstNonEmptyStrings(anyToString(request["agent_id"]), anyToString(request["agentId"])))
	var hookedResponse map[string]any
	var hookedFallbackResponse func() map[string]any
	var hookedBinding map[string]any
	var hookedDurable bool
	var hookedSampleID string
	var hookedReceiptDigest string
	var hookedComposition map[string]any
	var hookedPolicy validatedRecallResponsePolicyInput
	var hookedCompactReady bool
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
		policy := recallResponseServerPolicyInputFromCompilation(composition, artifacts, durable)
		if routePolicy, ok := s.recallResponseRoutePolicyFor(request); ok {
			policy = routePolicy
		}
		response := composeRecallResponseWithPolicy(composition, policy)
		if durable {
			s.installRecallResponseContinuationWithFit(
				response, composition, request, policy, agentID, endpoint, responseShape != recallResponseInitialShape,
			)
		}
		fallbackComposition := cloneJSONMap(composition)
		delete(fallbackComposition, "_durable_context_pack_quality")
		var fallbackResponse map[string]any
		fallback := func() map[string]any {
			if fallbackResponse == nil {
				fallbackResponse = recallResponseProjectFallbackWithServerSilence(fallbackComposition, recallResponseProductionPolicyInput())
				fallbackResponse = finalizeRecallResponseTransport(fallbackResponse, agentID, "recall_response", endpoint)
			}
			return cloneJSONMap(fallbackResponse)
		}
		// Apply the same production transport boundary before deriving identity for
		// either response shape. The initial projection is a compact view of this
		// exact retained source artifact; skipping finalization here would bind
		// quality/continuation proof to a pre-clipping candidate and can make the
		// later retained-row check fail closed.
		response = finalizeRecallResponseTransport(response, agentID, "recall_response", endpoint)
		for attempts := 0; attempts < recallResponseMaxEvidence+recallResponseMaxModules+1; attempts++ {
			if !s.reconcileRecallResponseContinuation(response, composition, request, policy, agentID, endpoint) {
				break
			}
			response = finalizeRecallResponseTransport(response, agentID, "recall_response", endpoint)
		}
		candidateValid := recallResponseTransportCandidateValid(response)
		if !durable || !candidateValid {
			s.discardRecallResponseContinuation(response)
			response = fallback()
		}
		compactReady := durable && candidateValid &&
			anyToBool(anyMap(response["request_scope"])["source_bound"])
		artifacts.SideEffectsSuppressed = recallResponseServerSilenced(response)
		if artifacts.SideEffectsSuppressed {
			// A silent response is advisory output only. Remove any provisional
			// attribution before the persistence seam sees the artifacts.
			for _, key := range []string{"recall_response_id", "recall_response_digest", "response_component_refs"} {
				delete(artifacts.Quality, key)
			}
			delete(artifacts.Quality, "selection_receipt")
			delete(composition, "_durable_context_pack_quality")
		}
		var binding map[string]any
		if durable && !artifacts.SideEffectsSuppressed && !recallResponseIsV1Control(response) {
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
				s.discardRecallResponseContinuation(response)
				response = fallback()
				compactReady = false
			}
		}
		hookedResponse = response
		hookedFallbackResponse = fallback
		hookedBinding = binding
		hookedDurable = durable && !artifacts.SideEffectsSuppressed && binding != nil
		hookedSampleID = anyToString(artifacts.Quality["sample_id"])
		hookedComposition = composition
		hookedPolicy = policy
		hookedCompactReady = compactReady
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
		s.discardRecallResponseContinuation(response)
		if hookedFallbackResponse != nil {
			// Compilation succeeded and the hook already projected the exact
			// artifact set. A later status/error envelope may change the HTTP
			// status, but it must not replace that snapshot or trigger a second
			// projection from sanitized error data.
			response = hookedFallbackResponse()
		} else {
			response = recallResponseProjectFallbackWithServerSilence(recallResponseCompositionInput(request, contextResponse), recallResponseProductionPolicyInput())
			response = finalizeRecallResponseTransport(response, agentID, "recall_response", endpoint)
		}
		hookedDurable = false
		hookedBinding = nil
		hookedCompactReady = false
	}
	if hookedDurable && hookedBinding != nil && s != nil && s.contextPackQuality != nil {
		if s.recallResponseRetainedProofHook != nil {
			s.recallResponseRetainedProofHook(hookedSampleID)
		}
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
			s.discardRecallResponseContinuation(response)
			if hookedFallbackResponse != nil {
				response = hookedFallbackResponse()
			} else {
				response = recallResponseProjectFallbackWithServerSilence(recallResponseCompositionInput(request, contextResponse), recallResponseProductionPolicyInput())
				response = finalizeRecallResponseTransport(response, agentID, "recall_response", endpoint)
			}
			hookedCompactReady = false
		}
	}
	if s != nil && s.recallResponseRouteResponseHook != nil {
		// The observer receives the exact server-produced source response before
		// an initial_compact projection. It cannot alter route state or wire data.
		s.recallResponseRouteResponseHook(cloneJSONMap(response))
	}
	if s != nil && s.recallResponseRouteCompositionHook != nil {
		// The composition observer is paired with the source response observer so
		// internal verification can recompute continuation membership from the
		// exact server-owned compilation snapshot. It cannot alter route state or
		// wire data.
		s.recallResponseRouteCompositionHook(cloneJSONMap(hookedComposition))
	}
	if responseShape == recallResponseInitialShape && hookedCompactReady {
		if initial, ok := s.projectRecallResponseInitial(
			response, hookedComposition, request, hookedPolicy, agentID, endpoint,
		); ok {
			response = initial
		} else if !recallResponseTransportCandidateValid(response) {
			s.discardRecallResponseContinuation(response)
			if hookedFallbackResponse != nil {
				response = hookedFallbackResponse()
			}
		}
	}
	w.Header().Set("X-ContextLattice-Native-Route", "recall_response")
	if tool {
		w.Header().Set("X-ContextLattice-Tool", "recall_response")
	}
	writeJSON(w, status, response)
}

func recallResponseIsV1Control(response map[string]any) bool {
	return anyToString(anyMap(anyMap(response["answer"])["composition"])["primary_module"]) == "v1_control" &&
		len(contextPackAnyList(anyMap(response["answer"])["components"])) == 0
}

func recallResponseTransportCandidateValid(response map[string]any) bool {
	if recallResponseIsV1Control(response) {
		return true
	}
	contract := anyMap(response["format_contract"])
	compactBytes, compactTokens := recallResponseCompactBudget(response)
	if !anyToBool(contract["contract_valid"]) || anyToBool(contract["truncated"]) ||
		compactBytes > recallResponseMaxCompactBytes || compactTokens > recallResponseMaxCompactTokens || !validateRecallResponseU2(response) {
		return false
	}
	_, ok := recallResponseBindingFromResponse(response)
	return ok
}
