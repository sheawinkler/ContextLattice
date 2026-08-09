package main

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
)

const (
	memoryContinuousCognitionPath = "/memory/continuous-cognition"
	toolsContinuousCognitionPath  = "/tools/continuous_cognition"

	continuousCognitionInvestigationScanLimit = 4096
	continuousCognitionInvestigationRefLimit  = 64
	responseIntelligenceMaxRequestBytes       = 1 << 20
)

type continuousCognitionInvestigationCandidate struct {
	Ref   string
	Score float64
}

// investigateContinuousCognitionLocalMemory performs one bounded in-memory
// snapshot search. It never calls a retrieval adapter, reads a memory file,
// updates access state, starts a goroutine, or populates a cache.
func investigateContinuousCognitionLocalMemory(
	ctx context.Context,
	store *memoryStore,
	request continuousCognitionRequest,
) (continuousCognitionInvestigation, []continuousCognitionGap) {
	if request.AsOf.IsZero() {
		return continuousCognitionInvestigation{
				State:               "degraded",
				Mode:                "read_only_investigation",
				ContextPackRef:      continuousCognitionUnavailableRef("context_pack"),
				RetrievalReceiptRef: continuousCognitionUnavailableRef("retrieval_receipt"),
				MutationsSuppressed: true,
			}, []continuousCognitionGap{{
				Code:      "investigation_as_of_required",
				Source:    "local_memory_snapshot",
				Material:  true,
				DetailRef: continuousCognitionUnavailableRef("local_memory_snapshot"),
			}}
	}
	result := continuousCognitionInvestigation{
		State:               "degraded",
		Mode:                "read_only_investigation",
		ContextPackRef:      continuousCognitionUnavailableRef("context_pack"),
		RetrievalReceiptRef: continuousCognitionUnavailableRef("retrieval_receipt"),
		RetrievalCount:      1,
		CompilerCount:       0,
		MutationsSuppressed: true,
		ExecutionPerformed:  true,
		NetworkCalls:        0,
	}
	gap := func(code string, material bool) []continuousCognitionGap {
		return []continuousCognitionGap{{
			Code:      code,
			Source:    "local_memory_snapshot",
			Material:  material,
			DetailRef: continuousCognitionUnavailableRef("local_memory_snapshot"),
		}}
	}
	if store == nil || !store.isEnabled() {
		result.RetrievalCount = 0
		result.ExecutionPerformed = false
		result.RetrievalReceiptRef = continuousCognitionDigestPrefix("ref_retrieval_receipt_", map[string]any{
			"scope_digest": requestScopeDigestForContinuousCognition(request),
			"state":        "memory_store_unavailable",
		})
		return result, gap("investigation_memory_unavailable", true)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	cleanProject := strings.TrimSpace(request.Project)
	cleanTopic := strings.Trim(strings.ToLower(strings.TrimSpace(request.TopicPath)), "/")
	includeDeep := normalizeRetrievalMode(request.RetrievalMode) == "deep"
	entries := make([]memoryStoreEntry, 0, continuousCognitionInvestigationScanLimit)
	seen := map[string]struct{}{}
	truncated := false
	timeUnverifiable := false
	cancelled := false

	store.mu.RLock()
	recentCount := len(store.recent)
	start := recentCount - 1
	stop := start - continuousCognitionInvestigationScanLimit
	if stop >= 0 {
		truncated = true
	} else {
		stop = -1
	}
scan:
	for index := start; index > stop; index-- {
		select {
		case <-ctx.Done():
			cancelled = true
			break scan
		default:
		}
		entry := store.recent[index]
		result.ScannedCount++
		key := memoryStoreKey(entry.Project, entry.FileName)
		if _, exists := seen[key]; exists {
			continue
		}
		createdAt, createdAtOK := parseTimeBestEffort(entry.CreatedAt)
		if !createdAtOK {
			// The newest retained event for this key cannot be placed relative to
			// the boundary. Fail closed instead of admitting an older version.
			seen[key] = struct{}{}
			timeUnverifiable = true
			continue
		}
		if createdAt.After(request.AsOf.UTC()) {
			// A post-boundary supersession or tombstone must not hide the newest
			// retained version that was authoritative at the requested boundary.
			continue
		}
		seen[key] = struct{}{}
		if isMemoryTombstone(entry) {
			continue
		}
		copyEntry := entry
		copyEntry.Tags = append([]string(nil), entry.Tags...)
		entries = append(entries, copyEntry)
	}
	store.mu.RUnlock()

	if cancelled {
		result.Truncated = true
		result.RetrievalReceiptRef = continuousCognitionDigestPrefix("ref_retrieval_receipt_", map[string]any{
			"scope_digest": requestScopeDigestForContinuousCognition(request),
			"state":        "cancelled",
			"scanned":      result.ScannedCount,
		})
		return result, gap("investigation_cancelled", true)
	}

	candidates := make([]continuousCognitionInvestigationCandidate, 0, len(entries))
	for _, entry := range entries {
		if !strings.EqualFold(strings.TrimSpace(entry.Project), cleanProject) {
			continue
		}
		entryTopic := strings.Trim(strings.ToLower(strings.TrimSpace(entry.TopicPath)), "/")
		if cleanTopic != "" && entryTopic != cleanTopic && !strings.HasPrefix(entryTopic, cleanTopic+"/") {
			continue
		}
		lifecycle := normalizeMemoryLifecycle(entry.Lifecycle)
		if !shouldSurfaceMemoryLifecycle(lifecycle, false) ||
			isEphemeralMemoryIdentity(entry.FileName, entry.TopicPath, entry.Summary, lifecycle) {
			continue
		}
		storageTier := normalizeMemoryStorageTier(entry.StorageTier)
		if !includeDeep && (storageTier == "deep" || storageTier == "retired") {
			continue
		}
		score := textMatchScore(request.Query, strings.Join([]string{
			entry.Project,
			entry.FileName,
			entry.TopicPath,
			entry.Summary,
		}, "\n"))
		if score <= 0 {
			continue
		}
		ref := continuousCognitionDigestPrefix("ref_evidence_", map[string]any{
			"event_id":     entry.EventID,
			"content_hash": normalizeProjectedContentHash(entry.ContentHash),
			"object_id":    entry.ObjectID,
			"project":      entry.Project,
			"file":         entry.FileName,
			"topic_path":   entry.TopicPath,
		})
		candidates = append(candidates, continuousCognitionInvestigationCandidate{Ref: ref, Score: score})
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		if candidates[left].Score != candidates[right].Score {
			return candidates[left].Score > candidates[right].Score
		}
		return candidates[left].Ref < candidates[right].Ref
	})
	refLimit := request.Limit
	if refLimit < 1 || refLimit > continuousCognitionInvestigationRefLimit {
		refLimit = continuousCognitionInvestigationRefLimit
	}
	if len(candidates) > refLimit {
		candidates = candidates[:refLimit]
		truncated = true
	}
	evidenceRefs := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		evidenceRefs = append(evidenceRefs, candidate.Ref)
	}
	result.State = "completed"
	result.EvidenceRefCount = len(evidenceRefs)
	result.Truncated = truncated
	result.SourceComplete = !truncated && !timeUnverifiable
	result.ContextPackRef = continuousCognitionDigestPrefix("ref_context_pack_", map[string]any{
		"scope_digest":  requestScopeDigestForContinuousCognition(request),
		"evidence_refs": evidenceRefs,
	})
	result.RetrievalReceiptRef = continuousCognitionDigestPrefix("ref_retrieval_receipt_", map[string]any{
		"context_pack_ref": result.ContextPackRef,
		"retrieval_count":  result.RetrievalCount,
		"compiler_count":   result.CompilerCount,
		"scanned_count":    result.ScannedCount,
		"truncated":        result.Truncated,
	})
	if truncated {
		return result, gap("investigation_snapshot_truncated", true)
	}
	if timeUnverifiable {
		return result, gap("investigation_time_unverifiable", true)
	}
	if len(evidenceRefs) == 0 {
		return result, gap("investigation_no_match", false)
	}
	return result, nil
}

func requestScopeDigestForContinuousCognition(request continuousCognitionRequest) string {
	return continuousCognitionScopeFromRequest(request).ScopeDigest
}

func applyContinuousCognitionInvestigation(
	observation *continuousCognitionObservation,
	investigation continuousCognitionInvestigation,
	gaps []continuousCognitionGap,
) {
	if observation == nil {
		return
	}
	observation.InvestigationRef = investigation.ContextPackRef
	observation.InvestigationProof = investigation.RetrievalReceiptRef
	observation.Gaps = append(observation.Gaps, gaps...)
	observation.Gaps = continuousCognitionNormalizeGaps(observation.Gaps)
	observation.SourceAnchorDigest = continuousCognitionCompositeSourceAnchorDigest(*observation)
	observation.SourceComplete = continuousCognitionSourceIsComplete(*observation) && investigation.SourceComplete
}

func finalizeContinuousCognitionTransport(
	payload map[string]any,
	agentID string,
	lane string,
	endpoint string,
) map[string]any {
	delete(payload, "format_contract")
	payload = attachPayloadFormatContract(continuousCognitionContractID, payload, agentID, lane, endpoint)
	material := cloneAnyMap(payload)
	delete(material, "format_contract")
	delete(material, "cognition_digest")
	payload["cognition_digest"] = frontierT6Digest(material)
	return payload
}

func (s *server) memoryContinuousCognition(w http.ResponseWriter, r *http.Request) {
	s.continuousCognitionRoute(w, r, false)
}

func (s *server) toolsContinuousCognition(w http.ResponseWriter, r *http.Request) {
	s.continuousCognitionRoute(w, r, true)
}

func readResponseIntelligenceRequestBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "request is required"})
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, responseIntelligenceMaxRequestBytes)
	body, err := readRequestBody(r)
	if err == nil {
		return body, true
	}
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "request body exceeds the bounded response-intelligence limit"})
		return nil, false
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
	return nil, false
}

func (s *server) continuousCognitionRoute(w http.ResponseWriter, r *http.Request, tool bool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	endpoint := memoryContinuousCognitionPath
	var ok bool
	if tool {
		endpoint = toolsContinuousCognitionPath
		_, ok = s.prepareToolHeaders(w, r, endpoint)
	} else {
		_, ok = s.prepareAuthorizedHeaders(w, r)
	}
	if !ok {
		return
	}
	body, ok := readResponseIntelligenceRequestBody(w, r)
	if !ok {
		return
	}
	payload, err := parseJSONMap(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	request, err := normalizeContinuousCognitionRequest(payload)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}

	authorization, authorizationErr := s.frontierT6OwnerAuthorization(r, frontierT6ProactiveContextPrepFeatureID, "status")
	authorizedWorkspaceRef := ""
	if authorizationErr == nil && authorization.Authorized {
		authorizedWorkspaceRef = contextPackLearnedScopeRef("workspace", authorization.WorkspaceID)
	}
	// Caller workspace fields are routing hints, never ownership authority.
	request.WorkspaceRef = authorizedWorkspaceRef
	unlockBoundary := s.lockOptionalFrontierT1ProjectBoundary()
	sessionVisibility := s.agentSessionProjectVisibilityForRequest(r)
	unlockBoundary()
	observation, proofSnapshot := snapshotContinuousCognitionWithProofForVisibility(
		s,
		request,
		request.AsOf,
		func(row map[string]any) bool {
			return strings.EqualFold(agentSessionProject(row), request.Project) && sessionVisibility.allows(row)
		},
	)
	investigation := continuousCognitionDefaultInvestigation(request.Operation, observation.SourceComplete)
	if request.Operation == continuousCognitionOperationInvestigate {
		var gaps []continuousCognitionGap
		investigation, gaps = investigateContinuousCognitionLocalMemory(r.Context(), s.memoryStore, request)
		applyContinuousCognitionInvestigation(&observation, investigation, gaps)
	}
	activation := continuousCognitionDefaultActivation()
	if authorizationErr == nil && authorization.Authorized {
		activation = projectContinuousCognitionActivation(
			s.frontierT6,
			authorization.WorkspaceID,
			authorization.AuthorizationDigest,
			request,
			request.AsOf,
		)
	}
	applyContinuousCognitionActivation(&observation, activation)
	governance, governanceGaps := projectContinuousCognitionGovernance(s, request, proofSnapshot, observation, activation, authorizedWorkspaceRef)
	applyContinuousCognitionGovernance(&observation, governance, governanceGaps)
	frontier := computeContinuousCognitionFrontier(observation, continuousCognitionFrontierPolicy{
		MaxRounds: 3, InvestigateThreshold: 0.55, ContinueThreshold: 0.70, ConsequenceHighThreshold: 0.70,
	})
	response := buildContinuousCognitionSemanticPayloadWithGovernance(request, observation, frontier, investigation, activation, governance)
	response = finalizeContinuousCognitionTransport(response, request.AgentID, "continuous_cognition", endpoint)
	if findings := validateAgentContractPayload(continuousCognitionContractID, response); len(findings) != 0 {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "continuous cognition contract validation failed"})
		return
	}
	w.Header().Set("X-ContextLattice-Native-Route", "continuous_cognition")
	if tool {
		w.Header().Set("X-ContextLattice-Tool", "continuous_cognition")
	}
	writeJSON(w, http.StatusOK, response)
}
