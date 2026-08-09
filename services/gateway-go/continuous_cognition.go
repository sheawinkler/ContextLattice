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

func continuousCognitionStableValue(value any, depth int) any {
	if depth > 8 {
		return "[depth-clipped]"
	}
	switch typed := value.(type) {
	case map[string]any:
		result := map[string]any{}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			switch strings.ToLower(key) {
			case "timestamp", "exact_timestamp", "generated_at", "created_at", "updated_at", "projection_ms", "as_of":
				continue
			}
			result[key] = continuousCognitionStableValue(typed[key], depth+1)
		}
		return result
	case []any:
		result := make([]any, 0, len(typed))
		for _, item := range typed {
			result = append(result, continuousCognitionStableValue(item, depth+1))
		}
		return result
	case []map[string]any:
		result := make([]any, 0, len(typed))
		for _, item := range typed {
			result = append(result, continuousCognitionStableValue(item, depth+1))
		}
		return result
	default:
		return value
	}
}

func continuousCognitionStableDigest(value any) string {
	return frontierT6Digest(continuousCognitionStableValue(value, 0))
}

// continuousCognitionHistoricalSession rebuilds only the session state that is
// evidenced by events at or before the requested boundary. The session store
// keeps a mutable latest row, so using that row after a later event would make
// an old cognition proof change when the future-only state changes.
func continuousCognitionHistoricalSession(row map[string]any, events []map[string]any, asOf time.Time) map[string]any {
	projected := map[string]any{
		"id":                  strings.TrimSpace(anyToString(row["id"])),
		"started_at":          anyToString(row["started_at"]),
		"status":              "active",
		"completed_at":        "",
		"last_event_type":     "session.started",
		"last_event_at":       anyToString(row["started_at"]),
		"updated_at":          anyToString(row["started_at"]),
		"event_count":         0,
		"memory_contribution": map[string]any{},
	}
	for _, event := range events {
		metadata := anyMap(event["metadata"])
		identity := proofTimelineIdentityFromMaps(
			event,
			metadata,
			anyMap(metadata["agent_state"]),
			anyMap(metadata["ownership"]),
		)
		for _, key := range []string{"agent_id", "project", "task_id", "task_identity_id", "execution_lane_id"} {
			if strings.TrimSpace(anyToString(projected[key])) == "" && strings.TrimSpace(anyToString(identity[key])) != "" {
				projected[key] = identity[key]
			}
		}
		if strings.TrimSpace(anyToString(projected["agent"])) == "" && strings.TrimSpace(anyToString(event["agent"])) != "" {
			projected["agent"] = event["agent"]
		}

		eventType := strings.TrimSpace(anyToString(event["type"]))
		createdAt := anyToString(event["created_at"])
		projected["last_event_type"] = eventType
		projected["last_event_at"] = createdAt
		projected["updated_at"] = createdAt
		projected["event_count"] = anyToInt(projected["event_count"], 0) + 1
		projected["memory_contribution"] = bumpContribution(
			anyMap(projected["memory_contribution"]),
			eventType,
			metadata,
			createdAt,
		)
		if objectiveState := strings.TrimSpace(anyToString(metadata["objective_state"])); objectiveState != "" {
			projected["objective_state"] = clipText(objectiveState, 80)
		}
		if nextAction := strings.TrimSpace(anyToString(metadata["next_action"])); nextAction != "" {
			projected["next_action"] = clipText(nextAction, 720)
		}
		if statePayload := anyMap(metadata["agent_state"]); len(statePayload) > 0 {
			projected["agent_state"] = normalizeAgentLifecyclePayload(statePayload, anyToString(event["status"]))
		}
		switch strings.ToLower(eventType) {
		case "session.completed", "agent.session.completed":
			projected["status"] = "completed"
			projected["completed_at"] = createdAt
		case "session.failed", "agent.session.failed":
			projected["status"] = "failed"
			projected["completed_at"] = createdAt
		case "session.blocked", "agent.session.blocked":
			projected["status"] = "blocked"
			projected["completed_at"] = ""
		case "session.canceled", "agent.session.canceled":
			projected["status"] = "canceled"
			projected["completed_at"] = createdAt
		default:
			if !agentSessionTerminal(anyToString(projected["status"])) {
				if state := anyToString(anyMap(projected["agent_state"])["state"]); state != "" {
					projected["status"] = normalizeAgentSessionStatus(state)
				} else {
					projected["status"] = "active"
				}
			}
		}
	}
	projected["as_of"] = asOf.UTC().Format(time.RFC3339Nano)
	return projected
}

func continuousCognitionSessionAt(store *agentSessionStore, sessionID string, asOf time.Time) (map[string]any, []map[string]any, bool, bool) {
	return continuousCognitionSessionAtVisible(store, sessionID, asOf, nil)
}

func continuousCognitionSessionAtVisible(
	store *agentSessionStore,
	sessionID string,
	asOf time.Time,
	visible func(map[string]any) bool,
) (map[string]any, []map[string]any, bool, bool) {
	if store == nil || strings.TrimSpace(sessionID) == "" || asOf.IsZero() {
		return nil, nil, false, false
	}
	sessionID = strings.TrimSpace(sessionID)
	store.mu.RLock()
	row, ok := store.sessions[sessionID]
	if !ok || (visible != nil && !visible(row)) {
		store.mu.RUnlock()
		return nil, nil, false, false
	}
	row = cloneAnyMap(row)
	retainedEvents := make([]map[string]any, 0, len(store.events[sessionID]))
	for _, event := range store.events[sessionID] {
		retainedEvents = append(retainedEvents, cloneAnyMap(event))
	}
	store.mu.RUnlock()
	startedAt, startedOK := parseTimeBestEffort(anyToString(row["started_at"]))
	if startedOK && startedAt.After(asOf.UTC()) {
		return nil, nil, false, true
	}
	temporalComplete := startedOK
	if updatedAt, parsed := parseTimeBestEffort(firstNonEmptyStrings(anyToString(row["updated_at"]), anyToString(row["last_event_at"]))); !parsed || updatedAt.After(asOf.UTC()) {
		temporalComplete = false
	}
	events := make([]map[string]any, 0, len(retainedEvents))
	for _, event := range retainedEvents {
		createdAt, parsed := parseTimeBestEffort(anyToString(event["created_at"]))
		if !parsed {
			temporalComplete = false
			continue
		}
		if createdAt.After(asOf.UTC()) {
			continue
		}
		events = append(events, event)
	}
	if temporalComplete {
		return store.effectiveSessionLocked(row, asOf.UTC()), events, true, true
	}
	projected := continuousCognitionHistoricalSession(row, events, asOf)
	return store.effectiveSessionLocked(projected, asOf.UTC()), events, true, false
}

func continuousCognitionObjectiveProjection(graph map[string]any, objectiveID string) (string, string, bool, bool) {
	if len(graph) == 0 || !anyToBool(graph["ok"]) {
		return "", continuousCognitionUnavailableRef("objective_graph"), false, false
	}
	nodes, ok := graph["nodes"].([]objectiveGraphNode)
	if !ok {
		return "", continuousCognitionUnavailableRef("objective_graph"), false, false
	}
	orderedNodes := append([]objectiveGraphNode(nil), nodes...)
	sort.SliceStable(orderedNodes, func(i, j int) bool {
		return orderedNodes[i].ObjectiveID < orderedNodes[j].ObjectiveID
	})
	stableNodes := make([]any, 0, len(orderedNodes))
	state := "unknown"
	available := false
	for _, node := range orderedNodes {
		if node.ObjectiveID == objectiveID {
			state = strings.TrimSpace(node.Status)
			available = true
		}
		stableNodes = append(stableNodes, map[string]any{
			"objective_id":        node.ObjectiveID,
			"status":              node.Status,
			"parent_objective_id": node.ParentObjectiveID,
			"task_identity_ids":   continuousCognitionSortedStrings(node.TaskIdentityIDs),
			"execution_lane_ids":  continuousCognitionSortedStrings(node.ExecutionLaneIDs),
			"session_ids":         continuousCognitionSortedStrings(node.SessionIDs),
			"decision_change_ids": continuousCognitionSortedStrings(node.DecisionChangeIDs),
			"outcome_ids":         continuousCognitionSortedStrings(node.OutcomeIDs),
			"checkpoint_ids":      continuousCognitionSortedStrings(node.CheckpointIDs),
		})
	}
	stableEdges := make([]any, 0)
	if edges, ok := graph["edges"].([]objectiveGraphEdge); ok {
		orderedEdges := append([]objectiveGraphEdge(nil), edges...)
		sort.SliceStable(orderedEdges, func(i, j int) bool {
			return orderedEdges[i].EdgeID < orderedEdges[j].EdgeID
		})
		for _, edge := range orderedEdges {
			stableEdges = append(stableEdges, map[string]any{
				"edge_id": edge.EdgeID, "from_id": edge.FromID, "to_id": edge.ToID, "type": edge.Type,
			})
		}
	}
	material := map[string]any{
		"complete": graph["complete"], "graph_truncated": graph["graph_truncated"],
		"node_count": graph["node_count"], "edge_count": graph["edge_count"],
		"transition_count": graph["transition_count"], "nodes": stableNodes, "edges": stableEdges,
	}
	return state, continuousCognitionDigestPrefix("ref_objective_graph_", material), available, anyToBool(graph["complete"])
}

func continuousCognitionSessionProjection(session map[string]any, events []map[string]any, asOf time.Time) string {
	rollup := buildAgentSessionRollup(session, events, asOf.UTC())
	stable := map[string]any{}
	for _, key := range []string{"session_id", "agent_id", "status", "objective_state", "next_action", "confidence", "source_coverage", "risk_summary", "artifact_summary", "event_count", "retained_event_count"} {
		if value, exists := rollup[key]; exists {
			stable[key] = continuousCognitionStableValue(value, 0)
		}
	}
	return continuousCognitionDigestPrefix("ref_session_rollup_", stable)
}

func continuousCognitionCaptureProofSnapshot(s *server, session map[string]any, events []map[string]any) agentProofTimelineSnapshot {
	scope := proofTimelineScopeFromSession(session, events)
	continuityRows, continuityAnchor, continuityIntegrity, continuityAvailable, continuityOmitted := s.continuity.proofTimelineRows(scope)
	claims, claimAnchor, claimsAvailable, claimOmitted := s.temporalClaims.proofTimelineRows(scope)
	qualitySamples, qualityOutcomes, qualityAnchor, qualityAvailable, qualityOmitted := s.contextPackQuality.proofTimelineRows(scope)
	tokenRows, tokenAnchor, tokenAvailable, tokenOmitted := s.tokenImpact.proofTimelineRows(scope)
	before := map[string]any{
		"agent_session":        proofTimelineSessionAnchor(session, events),
		"continuity":           continuityAnchor,
		"temporal_claim":       claimAnchor,
		"context_pack_quality": qualityAnchor,
		"token_impact":         tokenAnchor,
	}
	// Do not call proofTimelineAnchors here: its agent-session branch calls
	// agentSessionStore.get, which re-evaluates state against wall-clock time.
	after := map[string]any{
		"agent_session":        proofTimelineSessionAnchor(session, events),
		"continuity":           s.continuity.proofTimelineAnchor(scope),
		"temporal_claim":       s.temporalClaims.proofTimelineAnchor(scope),
		"context_pack_quality": s.contextPackQuality.proofTimelineAnchor(scope),
		"token_impact":         s.tokenImpact.proofTimelineAnchor(scope),
	}
	return agentProofTimelineSnapshot{
		Session: cloneAnyMap(session), Events: cloneMapSlice(events), ContinuityEntries: continuityRows,
		ContinuityIntegrity: continuityIntegrity, Claims: claims,
		QualitySamples: qualitySamples, QualityOutcomes: qualityOutcomes, TokenImpacts: tokenRows,
		Availability: map[string]bool{
			"continuity": continuityAvailable, "temporal_claim": claimsAvailable,
			"context_pack_quality": qualityAvailable, "token_impact": tokenAvailable,
		},
		SourceOmitted: map[string]int{
			"continuity": continuityOmitted, "temporal_claim": claimOmitted,
			"context_pack_quality": qualityOmitted, "token_impact": tokenOmitted,
		},
		SourceAnchorsBefore: before,
		SourceAnchorsAfter:  after,
	}
}

func continuousCognitionMapTimeAt(row map[string]any, fields ...string) (time.Time, bool) {
	for _, field := range fields {
		if value := strings.TrimSpace(anyToString(row[field])); value != "" {
			return parseTimeBestEffort(value)
		}
	}
	return time.Time{}, false
}

func continuousCognitionMapRowsAt(rows []map[string]any, asOf time.Time, fields ...string) ([]map[string]any, int) {
	filtered := make([]map[string]any, 0, len(rows))
	ambiguous := 0
	for _, row := range rows {
		occurredAt, ok := continuousCognitionMapTimeAt(row, fields...)
		if !ok {
			ambiguous++
			continue
		}
		if occurredAt.After(asOf.UTC()) {
			continue
		}
		filtered = append(filtered, cloneAnyMap(row))
	}
	return filtered, ambiguous
}

// continuousCognitionProofSnapshotAt removes evidence that did not yet exist at
// the requested boundary. Latest-only temporal-claim revisions that cross the
// boundary are excluded and surfaced as ambiguous instead of being backdated.
func continuousCognitionProofSnapshotAt(snapshot agentProofTimelineSnapshot, asOf time.Time) (agentProofTimelineSnapshot, int) {
	if asOf.IsZero() {
		return snapshot, 0
	}
	originalBefore := continuousCognitionStableDigest(snapshot.SourceAnchorsBefore)
	originalAfter := continuousCognitionStableDigest(snapshot.SourceAnchorsAfter)
	ambiguous := 0
	snapshot.Events, ambiguous = continuousCognitionMapRowsAt(snapshot.Events, asOf, "created_at")

	continuity := make([]continuityLedgerEntry, 0, len(snapshot.ContinuityEntries))
	for _, row := range snapshot.ContinuityEntries {
		recordedAt, ok := parseTimeBestEffort(row.RecordedAt)
		if !ok {
			ambiguous++
			continue
		}
		if !recordedAt.After(asOf.UTC()) {
			continuity = append(continuity, row)
		}
	}
	snapshot.ContinuityEntries = continuity

	claims := make([]temporalClaim, 0, len(snapshot.Claims))
	for _, claim := range snapshot.Claims {
		createdAt, createdOK := parseTimeBestEffort(claim.CreatedAt)
		updatedAt, updatedOK := parseTimeBestEffort(firstNonEmptyStrings(claim.UpdatedAt, claim.ObservedAt, claim.CreatedAt))
		if !createdOK || !updatedOK {
			ambiguous++
			continue
		}
		if createdAt.After(asOf.UTC()) {
			continue
		}
		if updatedAt.After(asOf.UTC()) {
			ambiguous++
			continue
		}
		claims = append(claims, claim)
	}
	snapshot.Claims = claims

	var count int
	snapshot.QualitySamples, count = continuousCognitionMapRowsAt(snapshot.QualitySamples, asOf, "capturedAt", "captured_at")
	ambiguous += count
	snapshot.QualityOutcomes, count = continuousCognitionMapRowsAt(snapshot.QualityOutcomes, asOf, "gateway_received_at", "capturedAt", "captured_at")
	ambiguous += count
	snapshot.TokenImpacts, count = continuousCognitionMapRowsAt(snapshot.TokenImpacts, asOf, "capturedAt", "captured_at")
	ambiguous += count

	anchors := map[string]any{
		"agent_session": map[string]any{
			"available": len(snapshot.Session) > 0, "event_count": len(snapshot.Events),
			"digest": continuousCognitionStableDigest(map[string]any{"session": snapshot.Session, "events": snapshot.Events}),
		},
		"continuity": map[string]any{
			"available": snapshot.Availability["continuity"], "row_count": len(snapshot.ContinuityEntries),
			"digest": continuousCognitionStableDigest(snapshot.ContinuityEntries),
		},
		"temporal_claim": map[string]any{
			"available": snapshot.Availability["temporal_claim"], "row_count": len(snapshot.Claims),
			"digest": continuousCognitionStableDigest(snapshot.Claims),
		},
		"context_pack_quality": map[string]any{
			"available": snapshot.Availability["context_pack_quality"], "sample_count": len(snapshot.QualitySamples),
			"outcome_count": len(snapshot.QualityOutcomes),
			"digest":        continuousCognitionStableDigest(map[string]any{"samples": snapshot.QualitySamples, "outcomes": snapshot.QualityOutcomes}),
		},
		"token_impact": map[string]any{
			"available": snapshot.Availability["token_impact"], "sample_count": len(snapshot.TokenImpacts),
			"digest": continuousCognitionStableDigest(snapshot.TokenImpacts),
		},
	}
	snapshot.SourceAnchorsBefore = anchors
	snapshot.SourceAnchorsAfter = cloneAnyMap(anchors)
	if originalBefore != originalAfter {
		snapshot.SourceAnchorsAfter["concurrent_snapshot"] = true
	}
	if snapshot.SourceOmitted == nil {
		snapshot.SourceOmitted = map[string]int{}
	}
	if ambiguous > 0 {
		snapshot.SourceOmitted["temporal_projection"] += ambiguous
	}
	return snapshot, ambiguous
}

func continuousCognitionProofProjectionFromSnapshot(snapshot agentProofTimelineSnapshot) (string, string, bool, string) {
	if len(snapshot.Session) == 0 {
		return continuousCognitionUnavailableRef("proof_timeline"), "unavailable", false, continuousCognitionUnavailableRef("source_anchor")
	}
	before := continuousCognitionStableValue(snapshot.SourceAnchorsBefore, 0)
	after := continuousCognitionStableValue(snapshot.SourceAnchorsAfter, 0)
	anchorMaterial := map[string]any{"before": before, "after": after, "availability": snapshot.Availability, "source_omitted": snapshot.SourceOmitted}
	anchorDigest := frontierT6Digest(anchorMaterial)
	complete := true
	for _, available := range snapshot.Availability {
		if !available {
			complete = false
		}
	}
	for _, omitted := range snapshot.SourceOmitted {
		if omitted > 0 {
			complete = false
		}
	}
	material := len(snapshot.Events) > 0 || len(snapshot.ContinuityEntries) > 0 || len(snapshot.Claims) > 0 ||
		len(snapshot.QualitySamples) > 0 || len(snapshot.QualityOutcomes) > 0 || len(snapshot.TokenImpacts) > 0
	if !material {
		for _, anchor := range []any{snapshot.SourceAnchorsBefore, snapshot.SourceAnchorsAfter} {
			if continuousCognitionProofAnchorHasMaterial(anchor) {
				material = true
				break
			}
		}
	}
	if !material || frontierT6Digest(before) != frontierT6Digest(after) {
		complete = false
	}
	status := "verified"
	if !complete {
		status = "degraded"
	}
	return continuousCognitionDigestPrefix("ref_proof_timeline_", anchorMaterial), status, complete, anchorDigest
}

func continuousCognitionProofProjection(s *server, session map[string]any, events []map[string]any) (string, string, bool, string) {
	if s == nil || len(session) == 0 {
		return continuousCognitionUnavailableRef("proof_timeline"), "unavailable", false, continuousCognitionUnavailableRef("source_anchor")
	}
	return continuousCognitionProofProjectionFromSnapshot(continuousCognitionCaptureProofSnapshot(s, session, events))
}

func continuousCognitionProofAnchorHasMaterial(value any) bool {
	anchor := anyMap(value)
	if len(anchor) == 0 {
		return false
	}
	if available, present := anchor["available"]; present && !anyToBool(available) {
		return false
	}
	for _, key := range []string{"event_count", "retained_event_count", "selected_count", "sample_count", "outcome_count", "row_count"} {
		if anyToInt(anchor[key], 0) > 0 {
			return true
		}
	}
	for _, nested := range anchor {
		if continuousCognitionProofAnchorHasMaterial(nested) {
			return true
		}
	}
	return false
}

func continuousCognitionRetrievalProjection(s *server, request continuousCognitionRequest) (string, bool) {
	if s == nil {
		return continuousCognitionUnavailableRef("retrieval_plan"), false
	}
	plan := s.buildAdaptiveRetrievalPlanAt(map[string]any{
		"query": request.Query, "project": request.Project,
		"topic_path": request.TopicPath, "retrieval_intent": request.RetrievalIntent,
		"retrieval_mode": request.RetrievalMode, "token_budget": request.TokenBudget,
	}, request.AsOf.UTC().Format(time.RFC3339Nano))
	if len(plan) == 0 {
		return continuousCognitionUnavailableRef("retrieval_plan"), false
	}
	stable := map[string]any{}
	for _, key := range []string{"mode", "activation_state", "task_phase", "retrieval_intent", "retrieval_mode", "token_budget", "evidence_obligations", "source_plan", "expansion", "budget_allocation", "stop_conditions", "calibration", "proof"} {
		if value, exists := plan[key]; exists {
			stable[key] = continuousCognitionStableValue(value, 0)
		}
	}
	return continuousCognitionDigestPrefix("ref_retrieval_plan_", stable), true
}

func continuousCognitionUtilityProjection(s *server, request continuousCognitionRequest) (string, string, bool, float64, continuousCognitionExpectedUtility, bool) {
	if s == nil || s.utility == nil {
		return continuousCognitionUnavailableRef("utility_snapshot"), "unavailable", false, 0, continuousCognitionExpectedUtility{}, false
	}
	rows := s.utility.rows(utilityQuery{
		Project: request.Project, RetrievalIntent: request.RetrievalIntent,
		WorkspaceRef: contextPackLearnedDigestRef(request.WorkspaceRef), To: request.AsOf.UTC(), Limit: request.Limit,
	})
	if len(rows) == 0 {
		return continuousCognitionUnavailableRef("utility_snapshot"), "unavailable", false, 0, continuousCognitionExpectedUtility{}, false
	}
	projected, pairs, pairExclusions := utilityPairProjection(rows)
	summary := utilityAggregate(projected, pairs, pairExclusions)
	stable := map[string]any{
		"observation_count":             summary["observation_count"],
		"independently_verified_count":  summary["independently_verified_count"],
		"observed_yield_eligible_count": summary["observed_yield_eligible_count"],
		"causal_pair_count":             summary["causal_pair_count"],
		"utility_unit_count":            summary["utility_unit_count"],
		"claim_status":                  summary["claim_status"],
		"denominators":                  continuousCognitionStableValue(summary["denominators"], 0),
		"observation_exclusions":        continuousCognitionStableValue(summary["observation_exclusions"], 0),
		"causal_exclusions":             continuousCognitionStableValue(summary["causal_exclusions"], 0),
	}
	// Commit 1 may observe cohort Utility rows for context, but they cannot
	// authorize the current cognition cycle or become a verified expectation.
	verified := false
	status := "contextual_unverified"
	expected := continuousCognitionExpectedUtility{}
	ref := continuousCognitionDigestPrefix("ref_utility_snapshot_", stable)
	return ref, status, verified, 0, expected, true
}

func continuousCognitionDefaultGovernance() continuousCognitionGovernance {
	return continuousCognitionGovernance{
		Outcome: continuousCognitionOutcome{
			State: "not_requested", OutcomeRef: continuousCognitionUnavailableRef("outcome"),
			ProofRef:              continuousCognitionUnavailableRef("proof"),
			UtilityObservationRef: continuousCognitionUnavailableRef("utility_observation"),
		},
		Evaluation: continuousCognitionEvaluation{
			State: "not_requested", UtilityStatus: "not_evaluated", Reason: "outcome_evaluation_not_requested",
		},
		Rollback: continuousCognitionLifecycleAdvice{
			State: "not_requested", ReasonRef: continuousCognitionUnavailableRef("rollback_reason"),
			TargetRef: continuousCognitionUnavailableRef("rollback_target"),
		},
		Retirement: continuousCognitionLifecycleAdvice{
			State: "not_requested", ReasonRef: continuousCognitionUnavailableRef("retirement_reason"),
			TargetRef: continuousCognitionUnavailableRef("retirement_target"),
		},
		ProjectionRef: continuousCognitionUnavailableRef("lifecycle_proof"),
	}
}

func continuousCognitionFinalizeGovernance(governance continuousCognitionGovernance) continuousCognitionGovernance {
	governance.ProjectionRef = continuousCognitionDigestPrefix("ref_lifecycle_proof_", map[string]any{
		"outcome":    continuousCognitionOutcomeMap(governance.Outcome),
		"evaluation": continuousCognitionEvaluationMap(governance.Evaluation),
		"rollback":   continuousCognitionLifecycleAdviceMap(governance.Rollback),
		"retirement": continuousCognitionLifecycleAdviceMap(governance.Retirement),
	})
	return governance
}

func continuousCognitionCanonicalIdentityMatches(row map[string]any, request continuousCognitionRequest, authorizedWorkspaceRef string) bool {
	if len(row) == 0 || strings.TrimSpace(request.SessionID) == "" || strings.TrimSpace(request.TaskID) == "" ||
		strings.TrimSpace(request.AgentID) == "" || strings.TrimSpace(request.TaskIdentityID) == "" ||
		strings.TrimSpace(request.ExecutionLaneID) == "" || strings.TrimSpace(request.RetrievalIntent) == "" ||
		contextPackLearnedDigestRef(authorizedWorkspaceRef) == "" {
		return false
	}
	if !strings.EqualFold(anyToString(row["project"]), request.Project) ||
		anyToString(row["session_id"]) != request.SessionID || anyToString(row["task_id"]) != request.TaskID ||
		anyToString(row["agent_id"]) != request.AgentID ||
		!strings.EqualFold(anyToString(row["retrieval_intent"]), request.RetrievalIntent) ||
		anyToString(row["task_identity_id"]) != request.TaskIdentityID ||
		anyToString(row["execution_lane_id"]) != request.ExecutionLaneID ||
		contextPackLearnedDigestRef(anyToString(row["workspace_ref"])) != contextPackLearnedDigestRef(authorizedWorkspaceRef) {
		return false
	}
	binding, valid := recallResponseBindingFromSample(row)
	return valid && binding != nil
}

func continuousCognitionOutcomeTimestamp(row map[string]any) (time.Time, bool) {
	return continuousCognitionMapTimeAt(row, "gateway_received_at", "capturedAt", "captured_at")
}

func continuousCognitionSelectBoundOutcome(
	snapshot agentProofTimelineSnapshot,
	request continuousCognitionRequest,
	authorizedWorkspaceRef string,
) (map[string]any, map[string]any, bool, bool) {
	samples := make(map[string]map[string]any, len(snapshot.QualitySamples))
	for _, sample := range snapshot.QualitySamples {
		sampleID := strings.TrimSpace(anyToString(sample["sample_id"]))
		if sampleID == "" || !continuousCognitionCanonicalIdentityMatches(sample, request, authorizedWorkspaceRef) {
			continue
		}
		if existing, duplicate := samples[sampleID]; duplicate && contextPackQualitySampleAdmissionRef(existing) != contextPackQualitySampleAdmissionRef(sample) {
			return nil, nil, false, true
		}
		samples[sampleID] = cloneAnyMap(sample)
	}
	var selectedOutcome, selectedSample map[string]any
	var selectedAt time.Time
	for _, outcome := range snapshot.QualityOutcomes {
		sample := samples[anyToString(outcome["sample_id"])]
		if len(sample) == 0 || !continuousCognitionCanonicalIdentityMatches(outcome, request, authorizedWorkspaceRef) ||
			!contextPackOutcomeHasAuthoritativeSampleAdmission(outcome) ||
			contextPackQualitySampleAdmissionRef(sample) != anyToString(outcome["quality_sample_admission_ref"]) ||
			!contextPackQualityResponseBindingsEqual(sample, outcome) {
			continue
		}
		capturedAt, ok := continuousCognitionOutcomeTimestamp(outcome)
		if !ok || capturedAt.After(request.AsOf.UTC()) {
			continue
		}
		outcomeID := anyToString(outcome["outcome_id"])
		selectedID := anyToString(selectedOutcome["outcome_id"])
		if selectedOutcome == nil || capturedAt.After(selectedAt) || (capturedAt.Equal(selectedAt) && outcomeID > selectedID) {
			selectedOutcome, selectedSample, selectedAt = cloneAnyMap(outcome), cloneAnyMap(sample), capturedAt
		}
	}
	return selectedSample, selectedOutcome, selectedOutcome != nil, false
}

func continuousCognitionProjectOutcomeEvaluation(
	s *server,
	request continuousCognitionRequest,
	snapshot agentProofTimelineSnapshot,
	authorizedWorkspaceRef string,
) (continuousCognitionOutcome, continuousCognitionEvaluation, []continuousCognitionGap) {
	defaults := continuousCognitionDefaultGovernance()
	outcomeProjection, evaluation := defaults.Outcome, defaults.Evaluation
	outcomeProjection.State = "absent"
	evaluation.State = "unavailable"
	evaluation.UtilityStatus = "not_observed"
	evaluation.Reason = "exact_response_bound_outcome_unavailable"
	materialGap := request.Operation == continuousCognitionOperationOutcome || request.Operation == continuousCognitionOperationEvaluate
	gap := func(code, source string) []continuousCognitionGap {
		return []continuousCognitionGap{{Code: code, Source: source, Material: materialGap, DetailRef: continuousCognitionUnavailableRef(source)}}
	}
	if s == nil || s.contextPackQuality == nil || s.utility == nil {
		outcomeProjection.State = "source_unavailable"
		return outcomeProjection, evaluation, gap("outcome_authority_unavailable", "outcome")
	}
	if strings.TrimSpace(request.SessionID) == "" || strings.TrimSpace(request.TaskID) == "" || strings.TrimSpace(request.AgentID) == "" ||
		contextPackLearnedDigestRef(authorizedWorkspaceRef) == "" {
		outcomeProjection.State = "identity_incomplete"
		evaluation.Reason = "exact_identity_incomplete"
		return outcomeProjection, evaluation, gap("outcome_identity_incomplete", "outcome")
	}
	proofSample, proofOutcome, selected, sourceConflict := continuousCognitionSelectBoundOutcome(snapshot, request, authorizedWorkspaceRef)
	if sourceConflict {
		outcomeProjection.State = "source_conflict"
		evaluation.Reason = "conflicting_quality_receipts"
		return outcomeProjection, evaluation, gap("source_conflict", "outcome")
	}
	if !selected {
		return outcomeProjection, evaluation, gap("response_bound_outcome_not_found", "outcome")
	}
	sampleID := anyToString(proofSample["sample_id"])
	durableSample, sampleFound, sampleErr := s.contextPackQuality.durableQualitySampleForOutcome(sampleID)
	durableOutcome, outcomeFound, outcomeErr := s.contextPackQuality.authoritativeOutcomeForSample(sampleID)
	if sampleErr != nil || outcomeErr != nil || !sampleFound || !outcomeFound ||
		contextPackQualitySampleAdmissionRef(durableSample) != anyToString(durableOutcome["quality_sample_admission_ref"]) ||
		contextPackQualitySampleAdmissionRef(durableSample) != contextPackQualitySampleAdmissionRef(proofSample) ||
		contextPackOutcomeLogicalClaimDigest(durableOutcome) != contextPackOutcomeLogicalClaimDigest(proofOutcome) ||
		!contextPackQualityResponseBindingsEqual(durableSample, durableOutcome) ||
		!continuousCognitionCanonicalIdentityMatches(durableSample, request, authorizedWorkspaceRef) ||
		!continuousCognitionCanonicalIdentityMatches(durableOutcome, request, authorizedWorkspaceRef) {
		outcomeProjection.State = "source_conflict"
		evaluation.Reason = "canonical_outcome_join_failed"
		return outcomeProjection, evaluation, gap("source_conflict", "outcome")
	}
	if capturedAt, ok := continuousCognitionOutcomeTimestamp(durableOutcome); !ok || capturedAt.After(request.AsOf.UTC()) {
		outcomeProjection.State = "temporal_projection_unavailable"
		evaluation.Reason = "outcome_time_unverifiable"
		return outcomeProjection, evaluation, gap("outcome_time_unverifiable", "outcome")
	}

	binding, _ := recallResponseBindingFromSample(durableOutcome)
	outcomeID := anyToString(durableOutcome["outcome_id"])
	outcomeProjection.State = "recorded"
	outcomeProjection.OutcomeRef = continuousCognitionOpaqueRef("outcome", outcomeID)
	outcomeProjection.ProofRef = continuousCognitionDigestPrefix("ref_outcome_proof_", map[string]any{
		"sample_admission_ref": anyToString(durableOutcome["quality_sample_admission_ref"]),
		"source_claim_digest":  contextPackOutcomeLogicalClaimDigest(durableOutcome),
		"response_binding_key": recallResponseBindingKey(binding),
	})

	rows := s.utility.rowsForOutcomeIDs(utilityQuery{
		Project: request.Project, TaskClass: anyToString(durableOutcome["task_class"]),
		WorkspaceRef: authorizedWorkspaceRef, To: request.AsOf.UTC(), Limit: request.Limit,
	}, map[string]struct{}{outcomeID: {}})
	if len(rows) == 0 {
		evaluation.State = "evidence_incomplete"
		evaluation.Reason = "utility_observation_unavailable"
		return outcomeProjection, evaluation, gap("utility_observation_unavailable", "utility")
	}
	projected, pairs, _ := utilityPairProjection(rows)
	var utilityRow map[string]any
	for _, row := range projected {
		if anyToString(row["outcome_id"]) == outcomeID {
			utilityRow = row
			break
		}
	}
	if len(utilityRow) == 0 || utilitySourceClaimDigest(durableOutcome) != anyToString(utilityRow["source_claim_digest"]) ||
		!recallResponseBindingsEqual(durableOutcome, utilityRow) ||
		!continuousCognitionCanonicalIdentityMatches(utilityRow, request, authorizedWorkspaceRef) {
		outcomeProjection.State = "source_conflict"
		evaluation.State = "unavailable"
		evaluation.Reason = "canonical_utility_join_failed"
		return outcomeProjection, evaluation, gap("source_conflict", "utility")
	}
	utilityClaim := anyMap(utilityRow["utility"])
	eligibility := anyMap(utilityRow["eligibility"])
	verified := anyToBool(utilityClaim["independently_verified"]) && anyToString(utilityClaim["verification_status"]) == "verified"
	causalEligible := anyToBool(eligibility["causal_gain_eligible"])
	if causalEligible {
		causalEligible = false
		for _, pair := range pairs {
			if pair.TreatmentOutcomeID == outcomeID {
				causalEligible = true
				break
			}
		}
	}
	outcomeProjection.UtilityObservationRef = continuousCognitionDigestPrefix("ref_utility_observation_", map[string]any{
		"observation_id": utilityRow["observation_id"], "observation_digest": utilityRow["observation_digest"],
		"revision": utilityRow["revision"], "source_claim_digest": utilityRow["source_claim_digest"],
	})
	outcomeProjection.IndependentlyVerified = verified
	outcomeProjection.CausalEligible = causalEligible
	evaluation.State = "evaluated"
	evaluation.UtilityStatus = firstNonEmptyStrings(anyToString(utilityRow["status"]), "excluded")
	evaluation.Verified = verified
	evaluation.CausalEligible = causalEligible
	evaluation.Reason = "exact_canonical_utility_observation"
	return outcomeProjection, evaluation, nil
}

func projectContinuousCognitionGovernance(
	s *server,
	request continuousCognitionRequest,
	snapshot agentProofTimelineSnapshot,
	observation continuousCognitionObservation,
	activation continuousCognitionActivation,
	authorizedWorkspaceRef string,
) (continuousCognitionGovernance, []continuousCognitionGap) {
	governance := continuousCognitionDefaultGovernance()
	if !continuousCognitionLifecycleOperation(request.Operation) {
		return governance, nil
	}
	var gaps []continuousCognitionGap
	governance.Outcome, governance.Evaluation, gaps = continuousCognitionProjectOutcomeEvaluation(s, request, snapshot, authorizedWorkspaceRef)
	if request.Operation == continuousCognitionOperationRollback {
		governance.Rollback.State = "not_applicable"
		governance.Rollback.ReasonRef = continuousCognitionDigestPrefix("ref_rollback_reason_", map[string]any{"activation_state": activation.State})
		if activation.Persisted && !continuousCognitionActivationTerminalState(activation.State) {
			governance.Rollback.State = "recommended"
			governance.Rollback.TargetRef = activation.PrepID
		}
	}
	if request.Operation == continuousCognitionOperationRetire {
		governance.Retirement.State = "not_ready"
		governance.Retirement.ReasonRef = continuousCognitionDigestPrefix("ref_retirement_reason_", map[string]any{
			"objective_state": observation.ObjectiveState, "activation_state": activation.State,
		})
		if observation.ObjectiveTerminal || continuousCognitionActivationTerminalState(activation.State) {
			governance.Retirement.State = "recommended"
			governance.Retirement.TargetRef = observation.ObjectiveGraphRef
			if activation.Persisted {
				governance.Retirement.TargetRef = activation.PrepID
			}
		}
	}
	return continuousCognitionFinalizeGovernance(governance), gaps
}

func applyContinuousCognitionGovernance(observation *continuousCognitionObservation, governance continuousCognitionGovernance, gaps []continuousCognitionGap) {
	if observation == nil {
		return
	}
	observation.LifecycleProofRef = governance.ProjectionRef
	observation.Gaps = continuousCognitionNormalizeGaps(append(observation.Gaps, gaps...))
	observation.SourceAnchorDigest = continuousCognitionCompositeSourceAnchorDigest(*observation)
	observation.SourceComplete = continuousCognitionSourceIsComplete(*observation)
}

func continuousCognitionAddGap(observation *continuousCognitionObservation, code, source string, material bool) {
	if observation == nil {
		return
	}
	observation.Gaps = append(observation.Gaps, continuousCognitionGap{
		Code: code, Source: source, Material: material, DetailRef: continuousCognitionUnavailableRef(source),
	})
}

func continuousCognitionCompositeSourceAnchorDigest(observation continuousCognitionObservation) string {
	return frontierT6Digest(map[string]any{
		"proof_anchor_digest":  observation.ProofAnchorDigest,
		"objective_graph_ref":  observation.ObjectiveGraphRef,
		"session_rollup_ref":   observation.SessionRollupRef,
		"retrieval_plan_ref":   observation.RetrievalPlanRef,
		"investigation_ref":    observation.InvestigationRef,
		"investigation_proof":  observation.InvestigationProof,
		"utility_snapshot_ref": observation.UtilitySnapshotRef,
		"activation_ref":       observation.ActivationRef,
		"activation_state":     observation.ActivationState,
		"lifecycle_proof_ref":  observation.LifecycleProofRef,
		"normalized_gaps":      continuousCognitionGapMaps(continuousCognitionNormalizeGaps(observation.Gaps)),
		"continuity_zero_ref":  observation.ContinuityZeroRef,
		"proof_timeline_ref":   observation.ProofTimelineRef,
	})
}

func continuousCognitionSourceIsComplete(observation continuousCognitionObservation) bool {
	for _, gap := range observation.Gaps {
		if gap.Material {
			return false
		}
	}
	return observation.ObjectiveAvailable && observation.SessionPresent && observation.ProofComplete
}

func continuousCognitionFinalizeActivation(activation continuousCognitionActivation) continuousCognitionActivation {
	activation.ProjectionRef = continuousCognitionDigestPrefix("ref_activation_", map[string]any{
		"state": activation.State, "prep_id": activation.PrepID,
		"approval_ref": activation.ApprovalRef, "authorization_ref": activation.AuthorizationRef,
		"consumption_ref": activation.ConsumptionRef, "persisted": activation.Persisted,
	})
	return activation
}

// projectContinuousCognitionActivation takes one bounded read-only snapshot of
// the existing T6 preparation store. The authorized workspace is supplied by
// the server rather than accepted from the cognition request.
func projectContinuousCognitionActivation(
	store *frontierT6AgentFitStore,
	authorizedWorkspaceID string,
	currentAuthorizationDigest string,
	request continuousCognitionRequest,
	asOf time.Time,
) continuousCognitionActivation {
	activation := continuousCognitionDefaultActivation()
	if asOf.IsZero() {
		activation.State = "as_of_required"
		return continuousCognitionFinalizeActivation(activation)
	}
	if store == nil {
		activation.State = "unavailable"
		return continuousCognitionFinalizeActivation(activation)
	}
	currentAuthorizationDigest = strings.ToLower(strings.TrimSpace(currentAuthorizationDigest))
	if !frontierT6ValidDigest(currentAuthorizationDigest) {
		activation.State = "authorization_unavailable"
		return continuousCognitionFinalizeActivation(activation)
	}
	scope, err := frontierT6NormalizeContextPrepScope(frontierT6Scope{
		WorkspaceID: authorizedWorkspaceID,
		Project:     request.Project,
		SessionID:   request.SessionID,
		AgentID:     request.AgentID,
	})
	if err != nil || strings.TrimSpace(request.TaskID) == "" {
		activation.State = "identity_incomplete"
		return continuousCognitionFinalizeActivation(activation)
	}
	taskID, err := frontierT6NormalizeID(request.TaskID, "task_id", 160)
	if err != nil {
		activation.State = "identity_invalid"
		return continuousCognitionFinalizeActivation(activation)
	}

	store.mu.RLock()
	enabled := store.enabled
	var newestCandidate *frontierT6ContextPrepRecord
	var newestAuthorizedCandidate *frontierT6ContextPrepRecord
	newer := func(candidate frontierT6ContextPrepRecord, current *frontierT6ContextPrepRecord) bool {
		return current == nil || candidate.CreatedAt > current.CreatedAt ||
			(candidate.CreatedAt == current.CreatedAt && candidate.PrepID > current.PrepID)
	}
	for _, candidate := range store.state.ContextPreps {
		if candidate.Scope != scope || candidate.TaskID != taskID {
			continue
		}
		createdAt, ok := frontierT6ParseTime(candidate.CreatedAt)
		if !ok || createdAt.After(asOf.UTC()) {
			continue
		}
		candidate = frontierT6CopyContextPrepRecord(candidate)
		if newer(candidate, newestCandidate) {
			copyCandidate := candidate
			newestCandidate = &copyCandidate
		}
		if strings.EqualFold(strings.TrimSpace(candidate.AuthorizationDigest), currentAuthorizationDigest) && newer(candidate, newestAuthorizedCandidate) {
			copyCandidate := candidate
			newestAuthorizedCandidate = &copyCandidate
		}
	}
	store.mu.RUnlock()
	if !enabled {
		activation.State = "unavailable"
		return continuousCognitionFinalizeActivation(activation)
	}
	if newestCandidate == nil {
		activation.State = "absent"
		return continuousCognitionFinalizeActivation(activation)
	}
	selected := *newestCandidate
	authorizationCurrent := newestAuthorizedCandidate != nil
	if authorizationCurrent {
		selected = *newestAuthorizedCandidate
	}
	activation.Persisted = true
	activation.PrepID = continuousCognitionOpaqueRef("prep", selected.PrepID)
	activation.ApprovalRef = continuousCognitionOpaqueRef("approval", selected.ApprovalDigest)
	activation.AuthorizationRef = continuousCognitionOpaqueRef("authorization", selected.AuthorizationDigest)
	if frontierT6ValidDigest(selected.ConsumptionDigest) {
		activation.ConsumptionRef = continuousCognitionOpaqueRef("consumption", selected.ConsumptionDigest)
	}
	updatedAt, updatedOK := frontierT6ParseTime(selected.UpdatedAt)
	if !updatedOK {
		activation.State = "state_invalid"
		return continuousCognitionFinalizeActivation(activation)
	}
	if updatedAt.After(asOf.UTC()) {
		activation.State = "temporal_projection_unavailable"
		return continuousCognitionFinalizeActivation(activation)
	}
	if !authorizationCurrent {
		activation.State = "authorization_changed"
		return continuousCognitionFinalizeActivation(activation)
	}
	activation.State = selected.Status
	if selected.Status != "consumed" && selected.Status != "failed" && selected.Status != "canceled" {
		if expiresAt, ok := frontierT6ParseTime(selected.ExpiresAt); !ok || !asOf.UTC().Before(expiresAt) {
			activation.State = "expired"
		} else if selected.Status == "ready" && selected.Artifact != nil {
			if artifactExpires, ok := frontierT6ParseTime(selected.Artifact.ExpiresAt); !ok || !asOf.UTC().Before(artifactExpires) {
				activation.State = "expired"
			}
		}
	}
	return continuousCognitionFinalizeActivation(activation)
}

func applyContinuousCognitionActivation(observation *continuousCognitionObservation, activation continuousCognitionActivation) {
	if observation == nil {
		return
	}
	observation.ActivationRef = activation.ProjectionRef
	observation.ActivationState = activation.State
	observation.SourceAnchorDigest = continuousCognitionCompositeSourceAnchorDigest(*observation)
}

func snapshotContinuousCognitionWithProof(s *server, request continuousCognitionRequest, asOf time.Time) (continuousCognitionObservation, agentProofTimelineSnapshot) {
	return snapshotContinuousCognitionWithProofForVisibility(s, request, asOf, nil)
}

func snapshotContinuousCognitionWithProofForVisibility(
	s *server,
	request continuousCognitionRequest,
	asOf time.Time,
	sessionVisible func(map[string]any) bool,
) (continuousCognitionObservation, agentProofTimelineSnapshot) {
	var proofSnapshot agentProofTimelineSnapshot
	if asOf.IsZero() {
		asOf = request.AsOf
	}
	if !asOf.IsZero() {
		request.AsOf = asOf.UTC()
		asOf = request.AsOf
	}
	observation := continuousCognitionObservation{
		Scope:              continuousCognitionScopeFromRequest(request),
		ObjectiveGraphRef:  continuousCognitionUnavailableRef("objective_graph"),
		ObjectiveState:     "unknown",
		SessionRollupRef:   continuousCognitionUnavailableRef("session_rollup"),
		ContinuityZeroRef:  continuousCognitionUnavailableRef("continuity_zero_not_requested"),
		ProofTimelineRef:   continuousCognitionUnavailableRef("proof_timeline"),
		ProofStatus:        "unavailable",
		RetrievalPlanRef:   continuousCognitionUnavailableRef("retrieval_plan"),
		InvestigationRef:   continuousCognitionUnavailableRef("investigation"),
		InvestigationProof: continuousCognitionUnavailableRef("investigation_receipt"),
		UtilitySnapshotRef: continuousCognitionUnavailableRef("utility_snapshot"),
		UtilityStatus:      "unavailable",
		ActivationRef:      continuousCognitionUnavailableRef("activation"),
		ActivationState:    "not_requested",
		LifecycleProofRef:  continuousCognitionUnavailableRef("lifecycle_proof"),
		ExpectedUtility:    continuousCognitionExpectedUtility{},
		ProofAnchorDigest:  continuousCognitionUnavailableRef("proof_anchor"),
	}
	if asOf.IsZero() {
		continuousCognitionAddGap(&observation, "as_of_required", "snapshot", true)
		observation.Gaps = continuousCognitionNormalizeGaps(observation.Gaps)
		observation.SourceAnchorDigest = continuousCognitionCompositeSourceAnchorDigest(observation)
		return observation, proofSnapshot
	}
	if s == nil {
		continuousCognitionAddGap(&observation, "server_unavailable", "snapshot", true)
		observation.Gaps = continuousCognitionNormalizeGaps(observation.Gaps)
		observation.SourceAnchorDigest = continuousCognitionCompositeSourceAnchorDigest(observation)
		return observation, proofSnapshot
	}
	if strings.TrimSpace(request.ObjectiveID) == "" {
		continuousCognitionAddGap(&observation, "objective_id_required", "objective_graph", true)
	} else if s.continuity == nil {
		continuousCognitionAddGap(&observation, "objective_graph_unavailable", "objective_graph", true)
	} else {
		graph := s.continuity.objectiveGraph(request.Project, request.ObjectiveID, asOf.UTC(), false, request.Limit)
		state, graphRef, available, complete := continuousCognitionObjectiveProjection(graph, request.ObjectiveID)
		observation.ObjectiveState = state
		observation.ObjectiveGraphRef = graphRef
		observation.ObjectiveAvailable = available
		observation.ObjectiveTerminal = continuousCognitionTerminalState(state)
		if !available {
			continuousCognitionAddGap(&observation, "objective_not_found", "objective_graph", true)
		}
		if !complete {
			continuousCognitionAddGap(&observation, "objective_graph_incomplete", "objective_graph", true)
		}
	}
	if strings.TrimSpace(request.SessionID) == "" {
		continuousCognitionAddGap(&observation, "session_id_required", "agent_session", true)
	} else {
		session, events, found, temporalComplete := continuousCognitionSessionAtVisible(s.agentSessions, request.SessionID, asOf, sessionVisible)
		if !found {
			continuousCognitionAddGap(&observation, "session_not_found", "agent_session", true)
		} else {
			observation.SessionPresent = true
			observation.SessionAmbiguous = !temporalComplete
			observation.SessionRollupRef = continuousCognitionSessionProjection(session, events, asOf)
			proofSnapshot = continuousCognitionCaptureProofSnapshot(s, session, events)
			var temporalOmitted int
			proofSnapshot, temporalOmitted = continuousCognitionProofSnapshotAt(proofSnapshot, asOf)
			proofRef, proofStatus, proofComplete, anchorDigest := continuousCognitionProofProjectionFromSnapshot(proofSnapshot)
			observation.ProofTimelineRef = proofRef
			observation.ProofStatus = proofStatus
			observation.ProofComplete = proofComplete
			observation.ProofAnchorDigest = anchorDigest
			if proofStatus == "unavailable" {
				continuousCognitionAddGap(&observation, "proof_timeline_unavailable", "proof_timeline", true)
			}
			if !proofComplete {
				continuousCognitionAddGap(&observation, "proof_timeline_incomplete", "proof_timeline", true)
			}
			if !temporalComplete || temporalOmitted > 0 {
				continuousCognitionAddGap(&observation, "proof_temporal_projection_incomplete", "proof_timeline", true)
			}
		}
	}
	if retrievalRef, available := continuousCognitionRetrievalProjection(s, request); available {
		observation.RetrievalPlanRef = retrievalRef
	} else {
		continuousCognitionAddGap(&observation, "retrieval_plan_unavailable", "retrieval_plan", false)
	}
	utilityRef, utilityStatus, utilityVerified, utilityScore, expected, utilityAvailable := continuousCognitionUtilityProjection(s, request)
	observation.UtilitySnapshotRef = utilityRef
	observation.UtilityStatus = utilityStatus
	observation.UtilityVerified = utilityVerified
	observation.UtilityScore = utilityScore
	observation.ExpectedUtility = expected
	if !utilityAvailable {
		continuousCognitionAddGap(&observation, "utility_observation_unavailable", "utility", false)
	}
	observation.Gaps = continuousCognitionNormalizeGaps(observation.Gaps)
	observation.SourceAnchorDigest = continuousCognitionCompositeSourceAnchorDigest(observation)
	observation.SourceComplete = continuousCognitionSourceIsComplete(observation)
	return observation, proofSnapshot
}

func snapshotContinuousCognition(s *server, request continuousCognitionRequest, asOf time.Time) continuousCognitionObservation {
	observation, _ := snapshotContinuousCognitionWithProof(s, request, asOf)
	return observation
}

func continuousCognitionNormalizeGaps(gaps []continuousCognitionGap) []continuousCognitionGap {
	copyGaps := append([]continuousCognitionGap{}, gaps...)
	sort.SliceStable(copyGaps, func(i, j int) bool {
		left := strings.Join([]string{copyGaps[i].Code, copyGaps[i].Source, copyGaps[i].DetailRef}, "\x00")
		right := strings.Join([]string{copyGaps[j].Code, copyGaps[j].Source, copyGaps[j].DetailRef}, "\x00")
		return left < right
	})
	result := make([]continuousCognitionGap, 0, len(copyGaps))
	seen := map[string]struct{}{}
	for _, gap := range copyGaps {
		key := strings.Join([]string{gap.Code, gap.Source, gap.DetailRef}, "\x00")
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, gap)
	}
	return result
}

func continuousCognitionExpectedUtilityMap(value continuousCognitionExpectedUtility) map[string]any {
	value = continuousCognitionExpectedUtilityValue(value)
	return map[string]any{
		"action_change_probability": value.ActionChangeProbability,
		"consequence_if_wrong":      value.ConsequenceIfWrong,
		"evidence_reliability":      value.EvidenceReliability,
		"acquisition_cost":          value.AcquisitionCost,
		"score":                     value.Score,
	}
}

func continuousCognitionOutcomeMap(outcome continuousCognitionOutcome) map[string]any {
	defaults := continuousCognitionDefaultGovernance().Outcome
	if strings.TrimSpace(outcome.State) == "" {
		outcome.State = defaults.State
	}
	if strings.TrimSpace(outcome.OutcomeRef) == "" {
		outcome.OutcomeRef = defaults.OutcomeRef
	}
	if strings.TrimSpace(outcome.ProofRef) == "" {
		outcome.ProofRef = defaults.ProofRef
	}
	if strings.TrimSpace(outcome.UtilityObservationRef) == "" {
		outcome.UtilityObservationRef = defaults.UtilityObservationRef
	}
	return map[string]any{
		"state": outcome.State, "outcome_ref": outcome.OutcomeRef, "proof_ref": outcome.ProofRef,
		"utility_observation_ref": outcome.UtilityObservationRef,
		"independently_verified":  outcome.IndependentlyVerified, "causal_eligible": outcome.CausalEligible,
	}
}

func continuousCognitionEvaluationMap(evaluation continuousCognitionEvaluation) map[string]any {
	defaults := continuousCognitionDefaultGovernance().Evaluation
	if strings.TrimSpace(evaluation.State) == "" {
		evaluation.State = defaults.State
	}
	if strings.TrimSpace(evaluation.UtilityStatus) == "" {
		evaluation.UtilityStatus = defaults.UtilityStatus
	}
	if strings.TrimSpace(evaluation.Reason) == "" {
		evaluation.Reason = defaults.Reason
	}
	return map[string]any{
		"state": evaluation.State, "utility_status": evaluation.UtilityStatus,
		"verified": evaluation.Verified, "causal_eligible": evaluation.CausalEligible, "reason": evaluation.Reason,
	}
}

func continuousCognitionLifecycleAdviceMap(advice continuousCognitionLifecycleAdvice) map[string]any {
	if strings.TrimSpace(advice.State) == "" {
		advice.State = "not_requested"
	}
	if strings.TrimSpace(advice.ReasonRef) == "" {
		advice.ReasonRef = continuousCognitionUnavailableRef("lifecycle_reason")
	}
	if strings.TrimSpace(advice.TargetRef) == "" {
		advice.TargetRef = continuousCognitionUnavailableRef("lifecycle_target")
	}
	return map[string]any{"state": advice.State, "reason_ref": advice.ReasonRef, "target_ref": advice.TargetRef}
}

func buildContinuousCognitionSemanticPayload(request continuousCognitionRequest, observation continuousCognitionObservation, frontier continuousCognitionFrontier) map[string]any {
	return buildContinuousCognitionSemanticPayloadWithInvestigation(
		request,
		observation,
		frontier,
		continuousCognitionInvestigation{},
	)
}

func buildContinuousCognitionSemanticPayloadWithInvestigation(
	request continuousCognitionRequest,
	observation continuousCognitionObservation,
	frontier continuousCognitionFrontier,
	investigation continuousCognitionInvestigation,
) map[string]any {
	return buildContinuousCognitionSemanticPayloadWithLifecycle(
		request,
		observation,
		frontier,
		investigation,
		continuousCognitionDefaultActivation(),
	)
}

func buildContinuousCognitionSemanticPayloadWithLifecycle(
	request continuousCognitionRequest,
	observation continuousCognitionObservation,
	frontier continuousCognitionFrontier,
	investigation continuousCognitionInvestigation,
	activation continuousCognitionActivation,
) map[string]any {
	return buildContinuousCognitionSemanticPayloadWithGovernance(
		request, observation, frontier, investigation, activation, continuousCognitionDefaultGovernance(),
	)
}

func buildContinuousCognitionSemanticPayloadWithGovernance(
	request continuousCognitionRequest,
	observation continuousCognitionObservation,
	frontier continuousCognitionFrontier,
	investigation continuousCognitionInvestigation,
	activation continuousCognitionActivation,
	governance continuousCognitionGovernance,
) map[string]any {
	request.Operation = strings.ToLower(strings.TrimSpace(request.Operation))
	if _, allowed := continuousCognitionOperations[request.Operation]; !allowed {
		request.Operation = continuousCognitionOperationObserve
	}
	if observation.Scope.ScopeDigest == "" {
		observation.Scope = continuousCognitionScopeFromRequest(request)
	}
	observation.Scope.RetrievalIntent = normalizeRetrievalIntent(observation.Scope.RetrievalIntent, "decision")
	observation.Gaps = continuousCognitionNormalizeGaps(observation.Gaps)
	frontier.ExpectedUtility = continuousCognitionExpectedUtilityValue(frontier.ExpectedUtility)
	frontier.UtilityScore = continuousCognitionFinite01(frontier.UtilityScore)
	cognitionID := continuousCognitionDigestPrefix("cc_", map[string]any{
		"scope_digest":  observation.Scope.ScopeDigest,
		"cycle_ref":     observation.Scope.CycleRef,
		"objective_ref": continuousCognitionOpaqueRef("objective", request.ObjectiveID),
		"operation":     request.Operation,
	})
	frontier.FrontierID = continuousCognitionDigestPrefix("frontier_", map[string]any{
		"cognition_id":         cognitionID,
		"decision":             frontier.Decision,
		"source_anchor_digest": observation.SourceAnchorDigest,
	})
	observationMap := map[string]any{
		"objective_graph_ref":  observation.ObjectiveGraphRef,
		"session_rollup_ref":   observation.SessionRollupRef,
		"continuity_zero_ref":  observation.ContinuityZeroRef,
		"proof_timeline_ref":   observation.ProofTimelineRef,
		"retrieval_plan_ref":   observation.RetrievalPlanRef,
		"utility_snapshot_ref": observation.UtilitySnapshotRef,
		"lifecycle_proof_ref":  observation.LifecycleProofRef,
		"source_anchor_digest": observation.SourceAnchorDigest,
		"source_complete":      observation.SourceComplete,
		"gaps":                 continuousCognitionGapMaps(observation.Gaps),
	}
	phase := "frontier"
	progressStatus := "observed"
	if request.Operation == continuousCognitionOperationInvestigate {
		phase = "investigation"
		progressStatus = "investigated"
	} else if request.Operation == continuousCognitionOperationStatus {
		phase = "status"
		progressStatus = "status"
	} else if request.Operation == continuousCognitionOperationOutcome {
		phase = "outcome"
		progressStatus = "outcome_projected"
	} else if request.Operation == continuousCognitionOperationEvaluate {
		phase = "evaluation"
		progressStatus = "evaluation_projected"
	} else if request.Operation == continuousCognitionOperationRollback {
		phase = "rollback"
		progressStatus = "rollback_advisory"
	} else if request.Operation == continuousCognitionOperationRetire {
		phase = "retirement"
		progressStatus = "retirement_advisory"
	}
	if strings.TrimSpace(investigation.State) == "" {
		investigation = continuousCognitionDefaultInvestigation(request.Operation, observation.SourceComplete)
	}
	if strings.TrimSpace(activation.State) == "" {
		activation = continuousCognitionDefaultActivation()
	}
	progress := continuousCognitionProgressMap(request.Operation, phase, progressStatus, observation, investigation, activation)
	payload := map[string]any{
		"ok": true, "schema_id": continuousCognitionContractID, "version": 1,
		"cognition_id": cognitionID, "operation": request.Operation,
		"phase": phase, "decision": frontier.Decision,
		"request_scope": map[string]any{
			"scope_digest": observation.Scope.ScopeDigest, "query_digest": observation.Scope.QueryDigest,
			"workspace_ref": observation.Scope.WorkspaceRef, "project_ref": observation.Scope.ProjectRef,
			"topic_ref": observation.Scope.TopicRef, "agent_ref": observation.Scope.AgentRef,
			"session_ref": observation.Scope.SessionRef, "task_ref": observation.Scope.TaskRef,
			"task_identity_ref": observation.Scope.TaskIdentityRef, "execution_lane_ref": observation.Scope.ExecutionLaneRef,
			"retrieval_intent": observation.Scope.RetrievalIntent, "cycle_ref": observation.Scope.CycleRef,
		},
		"observation": observationMap,
		"frontier": map[string]any{
			"frontier_id": frontier.FrontierID, "objective_state": frontier.ObjectiveState,
			"uncertainty": frontier.Uncertainty, "next_action_class": frontier.NextActionClass,
			"utility_score": frontier.UtilityScore, "expected_utility": continuousCognitionExpectedUtilityMap(frontier.ExpectedUtility),
			"stop_reason": frontier.StopReason,
		},
		"investigation": continuousCognitionInvestigationMap(investigation),
		"activation":    continuousCognitionActivationMap(activation),
		"outcome":       continuousCognitionOutcomeMap(governance.Outcome),
		"evaluation":    continuousCognitionEvaluationMap(governance.Evaluation),
		"rollback":      continuousCognitionLifecycleAdviceMap(governance.Rollback),
		"retirement":    continuousCognitionLifecycleAdviceMap(governance.Retirement),
		"progress":      progress,
		"safety": map[string]any{
			"advisory_only": true, "automatic_model_execution": false, "automatic_external_mutation": false,
			"runner_dispatch": false, "filesystem_mutation": false, "gateway_execution_performed": false,
			"requires_explicit_authorization": true, "requires_external_worker": true, "network_calls": 0,
		},
		"gaps": continuousCognitionGapMaps(observation.Gaps), "writeback_required": true,
	}
	digestMaterial := cloneAnyMap(payload)
	delete(digestMaterial, "cognition_digest")
	payload["cognition_digest"] = frontierT6Digest(digestMaterial)
	return payload
}

func continuousCognitionDefaultActivation() continuousCognitionActivation {
	return continuousCognitionActivation{
		State:            "not_requested",
		PrepID:           continuousCognitionUnavailableRef("prep"),
		ApprovalRef:      continuousCognitionUnavailableRef("approval"),
		AuthorizationRef: continuousCognitionUnavailableRef("authorization"),
		ConsumptionRef:   continuousCognitionUnavailableRef("consumption"),
		ProjectionRef:    continuousCognitionUnavailableRef("activation"),
		ExecutionOwner:   "external_cli_worker",
	}
}

func continuousCognitionActivationMap(activation continuousCognitionActivation) map[string]any {
	defaults := continuousCognitionDefaultActivation()
	if strings.TrimSpace(activation.State) == "" {
		activation.State = defaults.State
	}
	if strings.TrimSpace(activation.PrepID) == "" {
		activation.PrepID = defaults.PrepID
	}
	if strings.TrimSpace(activation.ApprovalRef) == "" {
		activation.ApprovalRef = defaults.ApprovalRef
	}
	if strings.TrimSpace(activation.AuthorizationRef) == "" {
		activation.AuthorizationRef = defaults.AuthorizationRef
	}
	if strings.TrimSpace(activation.ConsumptionRef) == "" {
		activation.ConsumptionRef = defaults.ConsumptionRef
	}
	if strings.TrimSpace(activation.ExecutionOwner) == "" {
		activation.ExecutionOwner = defaults.ExecutionOwner
	}
	return map[string]any{
		"state": activation.State, "prep_id": activation.PrepID,
		"approval_ref": activation.ApprovalRef, "authorization_ref": activation.AuthorizationRef,
		"consumption_ref": activation.ConsumptionRef,
		"execution_owner": activation.ExecutionOwner, "one_shot": true,
		"requires_explicit_cli_use": true, "gateway_execution_performed": false,
	}
}

func continuousCognitionProgressMap(
	operation string,
	phase string,
	status string,
	observation continuousCognitionObservation,
	investigation continuousCognitionInvestigation,
	activation continuousCognitionActivation,
) map[string]any {
	round := 0
	if operation == continuousCognitionOperationInvestigate && investigation.ExecutionPerformed {
		round = 1
	}
	dedupeDecision := "not_persisted"
	persisted := false
	if activation.Persisted {
		persisted = true
		dedupeDecision = "existing_one_shot_preparation"
		if operation == continuousCognitionOperationObserve || operation == continuousCognitionOperationInvestigate || operation == continuousCognitionOperationStatus {
			phase = "activation"
			switch activation.State {
			case "queued", "retry_pending":
				status = "activation_pending"
			case "preparing":
				status = "activation_preparing"
			case "ready":
				status = "activation_ready"
			case "consumed":
				status = "activation_consumed"
			case "failed", "expired", "canceled":
				status = "activation_terminal"
			default:
				status = "activation_state_unavailable"
			}
		}
	}
	return map[string]any{
		"status": status, "stage": phase, "round": round, "max_rounds": 3,
		"proof_timeline_ref": observation.ProofTimelineRef,
		"loop_guard": map[string]any{
			"cycle_ref": observation.Scope.CycleRef, "source_anchor_digest": observation.SourceAnchorDigest,
			"round": round, "max_rounds": 3, "dedupe_decision": dedupeDecision, "persisted": persisted,
		},
	}
}

func continuousCognitionDefaultInvestigation(operation string, sourceComplete bool) continuousCognitionInvestigation {
	state := "not_requested"
	mode := "read_only"
	if operation == continuousCognitionOperationInvestigate {
		state = "not_executed"
		mode = "read_only_investigation"
	}
	return continuousCognitionInvestigation{
		State:               state,
		Mode:                mode,
		ContextPackRef:      continuousCognitionUnavailableRef("context_pack"),
		RetrievalReceiptRef: continuousCognitionUnavailableRef("retrieval_receipt"),
		SourceComplete:      sourceComplete,
		MutationsSuppressed: true,
	}
}

func continuousCognitionInvestigationMap(investigation continuousCognitionInvestigation) map[string]any {
	if strings.TrimSpace(investigation.State) == "" {
		investigation = continuousCognitionDefaultInvestigation(continuousCognitionOperationObserve, false)
	}
	if strings.TrimSpace(investigation.Mode) == "" {
		investigation.Mode = "read_only"
	}
	if strings.TrimSpace(investigation.ContextPackRef) == "" {
		investigation.ContextPackRef = continuousCognitionUnavailableRef("context_pack")
	}
	if strings.TrimSpace(investigation.RetrievalReceiptRef) == "" {
		investigation.RetrievalReceiptRef = continuousCognitionUnavailableRef("retrieval_receipt")
	}
	return map[string]any{
		"state":                 investigation.State,
		"mode":                  investigation.Mode,
		"context_pack_ref":      investigation.ContextPackRef,
		"retrieval_receipt_ref": investigation.RetrievalReceiptRef,
		"source_coverage": map[string]any{
			"complete":              investigation.SourceComplete,
			"retrieval_count":       investigation.RetrievalCount,
			"compiler_count":        investigation.CompilerCount,
			"evidence_ref_count":    investigation.EvidenceRefCount,
			"scanned_count":         investigation.ScannedCount,
			"truncated":             investigation.Truncated,
			"learned_ranking_state": "control_shadow_only",
			"raw_material_exposed":  false,
		},
		"mutations_suppressed": investigation.MutationsSuppressed,
		"execution_performed":  investigation.ExecutionPerformed,
		"network_calls":        investigation.NetworkCalls,
	}
}
