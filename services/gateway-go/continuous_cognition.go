package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	continuousCognitionOperationObserve     = "observe"
	continuousCognitionOperationInvestigate = "investigate"
	continuousCognitionOperationStatus      = "status"
	continuousCognitionOperationOutcome     = "outcome"
	continuousCognitionOperationEvaluate    = "evaluate"
	continuousCognitionOperationRollback    = "rollback"
	continuousCognitionOperationRetire      = "retire"
	// Keep the original name as the observe default for callers that build the
	// pure projection directly.
	continuousCognitionOperation = continuousCognitionOperationObserve

	continuousCognitionMaxQueryBytes      = 2400
	continuousCognitionMaxProjectBytes    = 512
	continuousCognitionMaxTopicBytes      = 512
	continuousCognitionMaxReferenceBytes  = 512
	continuousCognitionDefaultLimit       = 32
	continuousCognitionMaxLimit           = 500
	continuousCognitionDefaultTokenBudget = 4000
	continuousCognitionMinTokenBudget     = 512
	continuousCognitionMaxTokenBudget     = 64000
)

var continuousCognitionOperations = map[string]struct{}{
	continuousCognitionOperationObserve:     {},
	continuousCognitionOperationInvestigate: {},
	continuousCognitionOperationStatus:      {},
	continuousCognitionOperationOutcome:     {},
	continuousCognitionOperationEvaluate:    {},
	continuousCognitionOperationRollback:    {},
	continuousCognitionOperationRetire:      {},
}

var continuousCognitionRequestFields = map[string]struct{}{
	"operation": {}, "query": {}, "project": {}, "workspace_ref": {}, "topic_path": {},
	"retrieval_intent": {}, "retrieval_mode": {}, "agent_id": {}, "session_id": {},
	"task_id": {}, "task_identity_id": {}, "execution_lane_id": {}, "cycle_ref": {},
	"objective_id": {}, "limit": {}, "token_budget": {}, "as_of": {},
}

type continuousCognitionRequest struct {
	Operation       string
	Query           string
	Project         string
	WorkspaceRef    string
	TopicPath       string
	RetrievalIntent string
	RetrievalMode   string
	AgentID         string
	SessionID       string
	TaskID          string
	TaskIdentityID  string
	ExecutionLaneID string
	CycleRef        string
	ObjectiveID     string
	Limit           int
	TokenBudget     int
	AsOf            time.Time
}

type continuousCognitionScope struct {
	ScopeDigest      string
	QueryDigest      string
	WorkspaceRef     string
	ProjectRef       string
	TopicRef         string
	AgentRef         string
	SessionRef       string
	TaskRef          string
	TaskIdentityRef  string
	ExecutionLaneRef string
	RetrievalIntent  string
	CycleRef         string
}

type continuousCognitionGap struct {
	Code      string
	Source    string
	Material  bool
	DetailRef string
}

type continuousCognitionExpectedUtility struct {
	ActionChangeProbability float64
	ConsequenceIfWrong      float64
	EvidenceReliability     float64
	AcquisitionCost         float64
	Score                   float64
}

type continuousCognitionObservation struct {
	Scope              continuousCognitionScope
	ObjectiveGraphRef  string
	ObjectiveState     string
	ObjectiveAvailable bool
	ObjectiveTerminal  bool
	SessionRollupRef   string
	SessionPresent     bool
	SessionAmbiguous   bool
	ContinuityZeroRef  string
	ProofTimelineRef   string
	ProofStatus        string
	ProofComplete      bool
	RetrievalPlanRef   string
	InvestigationRef   string
	InvestigationProof string
	UtilitySnapshotRef string
	UtilityStatus      string
	UtilityVerified    bool
	UtilityScore       float64
	ExpectedUtility    continuousCognitionExpectedUtility
	ActivationRef      string
	ActivationState    string
	LifecycleProofRef  string
	ProofAnchorDigest  string
	SourceAnchorDigest string
	SourceComplete     bool
	Gaps               []continuousCognitionGap
}

type continuousCognitionInvestigation struct {
	State               string
	Mode                string
	ContextPackRef      string
	RetrievalReceiptRef string
	SourceComplete      bool
	RetrievalCount      int
	CompilerCount       int
	EvidenceRefCount    int
	ScannedCount        int
	Truncated           bool
	MutationsSuppressed bool
	ExecutionPerformed  bool
	NetworkCalls        int
}

type continuousCognitionActivation struct {
	State            string
	PrepID           string
	ApprovalRef      string
	AuthorizationRef string
	ConsumptionRef   string
	ProjectionRef    string
	ExecutionOwner   string
	Persisted        bool
}

type continuousCognitionOutcome struct {
	State                 string
	OutcomeRef            string
	ProofRef              string
	UtilityObservationRef string
	IndependentlyVerified bool
	CausalEligible        bool
}

type continuousCognitionEvaluation struct {
	State          string
	UtilityStatus  string
	Verified       bool
	CausalEligible bool
	Reason         string
}

type continuousCognitionLifecycleAdvice struct {
	State     string
	ReasonRef string
	TargetRef string
}

type continuousCognitionGovernance struct {
	Outcome       continuousCognitionOutcome
	Evaluation    continuousCognitionEvaluation
	Rollback      continuousCognitionLifecycleAdvice
	Retirement    continuousCognitionLifecycleAdvice
	ProjectionRef string
}

type continuousCognitionFrontierPolicy struct {
	MaxRounds                int
	InvestigateThreshold     float64
	ContinueThreshold        float64
	ConsequenceHighThreshold float64
}

type continuousCognitionFrontier struct {
	FrontierID      string
	Decision        string
	ObjectiveState  string
	Uncertainty     float64
	NextActionClass string
	UtilityScore    float64
	ExpectedUtility continuousCognitionExpectedUtility
	StopReason      string
}

func continuousCognitionUnavailableRef(kind string) string {
	kind = strings.TrimSpace(strings.ToLower(kind))
	if kind == "" {
		kind = "value"
	}
	return "ref_" + kind + "_unavailable"
}

func continuousCognitionDigestPrefix(prefix string, value any) string {
	digest := strings.TrimPrefix(frontierT6Digest(value), "sha256:")
	if len(digest) > 24 {
		digest = digest[:24]
	}
	return prefix + digest
}

func continuousCognitionOpaqueRef(kind, value string) string {
	kind = strings.TrimSpace(strings.ToLower(kind))
	value = strings.TrimSpace(value)
	if value == "" {
		return continuousCognitionUnavailableRef(kind)
	}
	return continuousCognitionDigestPrefix("ref_"+kind+"_", map[string]any{"kind": kind, "value": value})
}

func continuousCognitionReadString(payload map[string]any, field string, maximum int, required bool) (string, error) {
	raw, present := payload[field]
	if !present || raw == nil {
		if required {
			return "", fmt.Errorf("%s is required", field)
		}
		return "", nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", field)
	}
	value = strings.TrimSpace(value)
	if len([]byte(value)) > maximum {
		return "", fmt.Errorf("%s exceeds the bounded length", field)
	}
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("%s contains unsupported control characters", field)
	}
	if required && value == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	return value, nil
}

func continuousCognitionReadInt(payload map[string]any, field string, fallback int) (int, error) {
	raw, present := payload[field]
	if !present || raw == nil {
		return fallback, nil
	}
	var value int64
	switch typed := raw.(type) {
	case int:
		return typed, nil
	case int8:
		return int(typed), nil
	case int16:
		return int(typed), nil
	case int32:
		return int(typed), nil
	case int64:
		value = typed
	case uint:
		if uint64(typed) > uint64(^uint(0)>>1) {
			return 0, fmt.Errorf("%s is out of range", field)
		}
		return int(typed), nil
	case uint8:
		return int(typed), nil
	case uint16:
		return int(typed), nil
	case uint32:
		return int(typed), nil
	case uint64:
		if typed > uint64(^uint(0)>>1) {
			return 0, fmt.Errorf("%s is out of range", field)
		}
		return int(typed), nil
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || math.Trunc(typed) != typed {
			return 0, fmt.Errorf("%s must be an integer", field)
		}
		if typed > float64(int64(^uint(0)>>1)) || typed < float64(-int64(^uint(0)>>1)-1) {
			return 0, fmt.Errorf("%s is out of range", field)
		}
		return int(typed), nil
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer", field)
		}
		value = parsed
	default:
		return 0, fmt.Errorf("%s must be an integer", field)
	}
	if int64(int(value)) != value {
		return 0, fmt.Errorf("%s is out of range", field)
	}
	return int(value), nil
}

func normalizeContinuousCognitionRequest(payload map[string]any) (continuousCognitionRequest, error) {
	if payload == nil {
		return continuousCognitionRequest{}, errors.New("request is required")
	}
	for field := range payload {
		if _, allowed := continuousCognitionRequestFields[field]; !allowed {
			return continuousCognitionRequest{}, errors.New("unsupported request field")
		}
	}
	operation, err := continuousCognitionReadString(payload, "operation", 32, true)
	if err != nil {
		return continuousCognitionRequest{}, err
	}
	operation = strings.ToLower(operation)
	if _, allowed := continuousCognitionOperations[operation]; !allowed {
		return continuousCognitionRequest{}, errors.New("operation must be one of observe, investigate, status, outcome, evaluate, rollback, or retire")
	}
	query, err := continuousCognitionReadString(payload, "query", continuousCognitionMaxQueryBytes, true)
	if err != nil {
		return continuousCognitionRequest{}, err
	}
	project, err := continuousCognitionReadString(payload, "project", continuousCognitionMaxProjectBytes, true)
	if err != nil {
		return continuousCognitionRequest{}, err
	}
	project, err = sanitizeMemoryProject(project)
	if err != nil {
		return continuousCognitionRequest{}, errors.New("project is invalid")
	}
	workspaceRef, err := continuousCognitionReadString(payload, "workspace_ref", continuousCognitionMaxReferenceBytes, false)
	if err != nil {
		return continuousCognitionRequest{}, err
	}
	topicPath, err := continuousCognitionReadString(payload, "topic_path", continuousCognitionMaxTopicBytes, false)
	if err != nil {
		return continuousCognitionRequest{}, err
	}
	retrievalIntent, err := continuousCognitionReadString(payload, "retrieval_intent", 128, false)
	if err != nil {
		return continuousCognitionRequest{}, err
	}
	retrievalMode, err := continuousCognitionReadString(payload, "retrieval_mode", 64, false)
	if err != nil {
		return continuousCognitionRequest{}, err
	}
	retrievalIntent = normalizeRetrievalIntent(retrievalIntent, "decision")
	retrievalMode = normalizeRetrievalMode(retrievalMode)
	readReference := func(field string) (string, error) {
		return continuousCognitionReadString(payload, field, continuousCognitionMaxReferenceBytes, false)
	}
	agentID, err := readReference("agent_id")
	if err != nil {
		return continuousCognitionRequest{}, err
	}
	sessionID, err := readReference("session_id")
	if err != nil {
		return continuousCognitionRequest{}, err
	}
	taskID, err := readReference("task_id")
	if err != nil {
		return continuousCognitionRequest{}, err
	}
	taskIdentityID, err := readReference("task_identity_id")
	if err != nil {
		return continuousCognitionRequest{}, err
	}
	executionLaneID, err := readReference("execution_lane_id")
	if err != nil {
		return continuousCognitionRequest{}, err
	}
	cycleRef, err := readReference("cycle_ref")
	if err != nil {
		return continuousCognitionRequest{}, err
	}
	objectiveID, err := readReference("objective_id")
	if err != nil {
		return continuousCognitionRequest{}, err
	}
	limit, err := continuousCognitionReadInt(payload, "limit", continuousCognitionDefaultLimit)
	if err != nil {
		return continuousCognitionRequest{}, err
	}
	if limit < 1 || limit > continuousCognitionMaxLimit {
		return continuousCognitionRequest{}, fmt.Errorf("limit must be between 1 and %d", continuousCognitionMaxLimit)
	}
	tokenBudget, err := continuousCognitionReadInt(payload, "token_budget", continuousCognitionDefaultTokenBudget)
	if err != nil {
		return continuousCognitionRequest{}, err
	}
	if tokenBudget < continuousCognitionMinTokenBudget || tokenBudget > continuousCognitionMaxTokenBudget {
		return continuousCognitionRequest{}, fmt.Errorf("token_budget must be between %d and %d", continuousCognitionMinTokenBudget, continuousCognitionMaxTokenBudget)
	}
	asOfRaw, err := continuousCognitionReadString(payload, "as_of", 64, false)
	if err != nil {
		return continuousCognitionRequest{}, err
	}
	var asOf time.Time
	if asOfRaw != "" {
		asOf, err = time.Parse(time.RFC3339Nano, asOfRaw)
		if err != nil {
			return continuousCognitionRequest{}, errors.New("as_of must be RFC3339")
		}
		asOf = asOf.UTC()
	}
	return continuousCognitionRequest{
		Operation: operation, Query: query, Project: project, WorkspaceRef: workspaceRef,
		TopicPath: topicPath, RetrievalIntent: retrievalIntent, RetrievalMode: retrievalMode,
		AgentID: agentID, SessionID: sessionID, TaskID: taskID, TaskIdentityID: taskIdentityID,
		ExecutionLaneID: executionLaneID, CycleRef: cycleRef, ObjectiveID: objectiveID,
		Limit: limit, TokenBudget: tokenBudget, AsOf: asOf,
	}, nil
}

func continuousCognitionScopeFromRequest(request continuousCognitionRequest) continuousCognitionScope {
	request.Operation = strings.ToLower(strings.TrimSpace(request.Operation))
	request.RetrievalIntent = normalizeRetrievalIntent(request.RetrievalIntent, "decision")
	request.RetrievalMode = normalizeRetrievalMode(request.RetrievalMode)
	asOf := ""
	if !request.AsOf.IsZero() {
		asOf = request.AsOf.UTC().Format(time.RFC3339Nano)
	}
	queryDigest := frontierT6Digest(map[string]any{
		"query": request.Query, "retrieval_intent": request.RetrievalIntent,
		"retrieval_mode": request.RetrievalMode, "limit": request.Limit,
		"token_budget": request.TokenBudget, "as_of": asOf,
	})
	scope := continuousCognitionScope{
		QueryDigest:      queryDigest,
		WorkspaceRef:     continuousCognitionOpaqueRef("workspace", request.WorkspaceRef),
		ProjectRef:       continuousCognitionOpaqueRef("project", request.Project),
		TopicRef:         continuousCognitionOpaqueRef("topic", request.TopicPath),
		AgentRef:         continuousCognitionOpaqueRef("agent", request.AgentID),
		SessionRef:       continuousCognitionOpaqueRef("session", request.SessionID),
		TaskRef:          continuousCognitionOpaqueRef("task", request.TaskID),
		TaskIdentityRef:  continuousCognitionOpaqueRef("task_identity", request.TaskIdentityID),
		ExecutionLaneRef: continuousCognitionOpaqueRef("execution_lane", request.ExecutionLaneID),
		RetrievalIntent:  request.RetrievalIntent,
	}
	scope.ScopeDigest = frontierT6Digest(map[string]any{
		"operation":     request.Operation,
		"query_digest":  scope.QueryDigest,
		"workspace_ref": scope.WorkspaceRef, "project_ref": scope.ProjectRef,
		"topic_ref": scope.TopicRef, "agent_ref": scope.AgentRef,
		"session_ref": scope.SessionRef, "task_ref": scope.TaskRef,
		"task_identity_ref": scope.TaskIdentityRef, "execution_lane_ref": scope.ExecutionLaneRef,
		"retrieval_intent": scope.RetrievalIntent, "as_of": asOf,
	})
	scope.CycleRef = continuousCognitionDigestPrefix("cycle_", map[string]any{
		"scope_digest": scope.ScopeDigest, "cycle_ref": request.CycleRef,
		"objective_id": request.ObjectiveID,
	})
	return scope
}

func continuousCognitionFinite01(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return math.Round(value*1000000) / 1000000
}

func continuousCognitionExpectedUtilityValue(value continuousCognitionExpectedUtility) continuousCognitionExpectedUtility {
	actionChangeProbability := continuousCognitionFinite01(value.ActionChangeProbability)
	consequenceIfWrong := continuousCognitionFinite01(value.ConsequenceIfWrong)
	evidenceReliability := continuousCognitionFinite01(value.EvidenceReliability)
	acquisitionCost := continuousCognitionFinite01(value.AcquisitionCost)
	return continuousCognitionExpectedUtility{
		ActionChangeProbability: actionChangeProbability,
		ConsequenceIfWrong:      consequenceIfWrong,
		EvidenceReliability:     evidenceReliability,
		AcquisitionCost:         acquisitionCost,
		Score: continuousCognitionFinite01(
			actionChangeProbability*consequenceIfWrong*evidenceReliability - acquisitionCost,
		),
	}
}

func continuousCognitionSortedStrings(values []string) []string {
	result := append([]string{}, values...)
	sort.Strings(result)
	return result
}

func continuousCognitionGapMaps(gaps []continuousCognitionGap) []any {
	normalized := continuousCognitionNormalizeGaps(gaps)
	result := make([]any, 0, len(normalized))
	for _, gap := range normalized {
		result = append(result, map[string]any{
			"code": gap.Code, "source": gap.Source, "material": gap.Material, "detail_ref": gap.DetailRef,
		})
	}
	return result
}

func continuousCognitionHasGap(gaps []continuousCognitionGap, codes ...string) bool {
	allowed := map[string]struct{}{}
	for _, code := range codes {
		allowed[code] = struct{}{}
	}
	for _, gap := range gaps {
		if _, exists := allowed[gap.Code]; exists {
			return true
		}
	}
	return false
}

func continuousCognitionTerminalState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "completed", "complete", "succeeded", "success", "failed", "cancelled", "canceled", "retired":
		return true
	default:
		return false
	}
}

func continuousCognitionLifecycleOperation(operation string) bool {
	switch strings.ToLower(strings.TrimSpace(operation)) {
	case continuousCognitionOperationStatus, continuousCognitionOperationOutcome,
		continuousCognitionOperationEvaluate, continuousCognitionOperationRollback,
		continuousCognitionOperationRetire:
		return true
	default:
		return false
	}
}

func continuousCognitionActivationTerminalState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "consumed", "failed", "expired", "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func computeContinuousCognitionFrontier(observation continuousCognitionObservation, policy continuousCognitionFrontierPolicy) continuousCognitionFrontier {
	if policy.MaxRounds <= 0 {
		policy.MaxRounds = 3
	}
	policy.InvestigateThreshold = continuousCognitionFinite01(policy.InvestigateThreshold)
	policy.ContinueThreshold = continuousCognitionFinite01(policy.ContinueThreshold)
	policy.ConsequenceHighThreshold = continuousCognitionFinite01(policy.ConsequenceHighThreshold)
	expected := continuousCognitionExpectedUtilityValue(observation.ExpectedUtility)
	frontier := continuousCognitionFrontier{
		ObjectiveState:  strings.TrimSpace(observation.ObjectiveState),
		UtilityScore:    continuousCognitionFinite01(observation.UtilityScore),
		ExpectedUtility: expected,
		Uncertainty:     continuousCognitionFinite01(1 - expected.EvidenceReliability),
	}
	switch {
	case observation.ObjectiveTerminal || continuousCognitionTerminalState(observation.ObjectiveState):
		frontier.Decision = "retire"
		frontier.NextActionClass = "none"
		frontier.StopReason = "objective_terminal"
	case observation.SessionAmbiguous:
		frontier.Decision = "abstain"
		frontier.NextActionClass = "request_explicit_identity"
		frontier.StopReason = "session_ambiguous"
	case !observation.SessionPresent:
		frontier.Decision = "abstain"
		frontier.NextActionClass = "request_explicit_identity"
		frontier.StopReason = "session_required"
	case continuousCognitionHasGap(observation.Gaps, "identity_conflict", "source_conflict", "corrupt_rows", "concurrent_snapshot"):
		frontier.Decision = "abstain"
		frontier.NextActionClass = "repair_source_identity"
		frontier.StopReason = "source_identity_conflict"
	case !observation.SourceComplete || !observation.ProofComplete:
		if expected.Score >= policy.InvestigateThreshold || expected.ConsequenceIfWrong >= policy.ConsequenceHighThreshold {
			frontier.Decision = "investigate"
			frontier.NextActionClass = "bounded_read_only_investigation"
			frontier.StopReason = "material_evidence_gap"
		} else {
			frontier.Decision = "abstain"
			frontier.NextActionClass = "request_more_evidence"
			frontier.StopReason = "insufficient_verified_evidence"
		}
	case observation.UtilityVerified && expected.Score >= policy.ContinueThreshold:
		frontier.Decision = "continue"
		frontier.NextActionClass = "await_authorized_next_step"
		frontier.StopReason = "verified_frontier"
	case expected.Score >= policy.InvestigateThreshold:
		frontier.Decision = "investigate"
		frontier.NextActionClass = "bounded_read_only_investigation"
		frontier.StopReason = "expected_value_exceeds_investigation_floor"
	default:
		frontier.Decision = "abstain"
		frontier.NextActionClass = "request_more_evidence"
		frontier.StopReason = "insufficient_expected_value"
	}
	frontier.Uncertainty = continuousCognitionFinite01(frontier.Uncertainty)
	frontier.FrontierID = continuousCognitionDigestPrefix("frontier_", map[string]any{
		"decision": frontier.Decision, "objective_state": frontier.ObjectiveState,
		"next_action_class": frontier.NextActionClass, "source_anchor_digest": observation.SourceAnchorDigest,
	})
	return frontier
}
