package main

import (
	"container/heap"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

type objectiveTransition struct {
	SchemaID           string           `json:"schema_id"`
	TransitionID       string           `json:"transition_id"`
	ObjectiveID        string           `json:"objective_id"`
	Project            string           `json:"project"`
	Objective          string           `json:"objective"`
	TransitionType     string           `json:"transition_type"`
	FromStatus         string           `json:"from_status,omitempty"`
	ToStatus           string           `json:"to_status"`
	ParentObjectiveID  string           `json:"parent_objective_id,omitempty"`
	DependsOn          []string         `json:"depends_on"`
	Supersedes         []string         `json:"supersedes"`
	TaskIdentityID     string           `json:"task_identity_id,omitempty"`
	ExecutionLaneID    string           `json:"execution_lane_id,omitempty"`
	SessionID          string           `json:"session_id,omitempty"`
	DecisionChangeID   string           `json:"decision_change_id,omitempty"`
	OutcomeID          string           `json:"outcome_id,omitempty"`
	CheckpointID       string           `json:"checkpoint_id,omitempty"`
	IdempotencyKey     string           `json:"idempotency_key"`
	Actor              string           `json:"actor"`
	Summary            string           `json:"summary"`
	Evidence           []map[string]any `json:"evidence"`
	Metadata           map[string]any   `json:"metadata"`
	OccurredAt         string           `json:"occurred_at"`
	RecordedAt         string           `json:"recorded_at"`
	ledgerSequence     uint64
	idempotentReplay   bool
	occurredAtExplicit bool
}

type objectiveGraphNode struct {
	ObjectiveID       string   `json:"objective_id"`
	Project           string   `json:"project"`
	Objective         string   `json:"objective"`
	Status            string   `json:"status"`
	ParentObjectiveID string   `json:"parent_objective_id,omitempty"`
	TaskIdentityIDs   []string `json:"task_identity_ids"`
	ExecutionLaneIDs  []string `json:"execution_lane_ids"`
	SessionIDs        []string `json:"session_ids"`
	DecisionChangeIDs []string `json:"decision_change_ids"`
	OutcomeIDs        []string `json:"outcome_ids"`
	CheckpointIDs     []string `json:"checkpoint_ids"`
	LastSummary       string   `json:"last_summary"`
	CreatedAt         string   `json:"created_at"`
	UpdatedAt         string   `json:"updated_at"`
}

type objectiveGraphEdge struct {
	EdgeID     string `json:"edge_id"`
	FromID     string `json:"from_id"`
	ToID       string `json:"to_id"`
	Type       string `json:"type"`
	OccurredAt string `json:"occurred_at"`
}

type objectiveGraphRelationRef struct {
	RelatedObjectiveID string
	TransitionIndex    int
}

type objectiveReplayCursor struct {
	objectiveID string
	indexes     []int
	position    int
}

type objectiveReplayHeap []objectiveReplayCursor

func (h objectiveReplayHeap) Len() int { return len(h) }

func (h objectiveReplayHeap) Less(i, j int) bool {
	left := h[i].indexes[h[i].position]
	right := h[j].indexes[h[j].position]
	if left != right {
		return left > right
	}
	return h[i].objectiveID < h[j].objectiveID
}

func (h objectiveReplayHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *objectiveReplayHeap) Push(value any) {
	*h = append(*h, value.(objectiveReplayCursor))
}

func (h *objectiveReplayHeap) Pop() any {
	old := *h
	last := len(old) - 1
	value := old[last]
	old[last] = objectiveReplayCursor{}
	*h = old[:last]
	return value
}

const (
	objectiveGraphRelationInspectionsPerNode = 128
	objectiveGraphMaxRelationInspections     = 500000
	objectiveGraphMaxSelectionInspections    = 100000
	objectiveGraphMaxReplayInspections       = 100000
	decisionChangeMaxQueryInspections        = 100000
	decisionChangeCursorSchemaID             = "decision_change_cursor.v1"
)

type decisionRef struct {
	DecisionID string `json:"decision_id"`
	Summary    string `json:"summary"`
}

type decisionChange struct {
	SchemaID           string           `json:"schema_id"`
	DecisionChangeID   string           `json:"decision_change_id"`
	Project            string           `json:"project"`
	ObjectiveID        string           `json:"objective_id"`
	TaskIdentityID     string           `json:"task_identity_id,omitempty"`
	SessionID          string           `json:"session_id,omitempty"`
	IdempotencyKey     string           `json:"idempotency_key"`
	Before             decisionRef      `json:"before"`
	After              decisionRef      `json:"after"`
	TriggerEvidence    []map[string]any `json:"trigger_evidence"`
	ConfidenceBefore   float64          `json:"confidence_before"`
	ConfidenceAfter    float64          `json:"confidence_after"`
	ConfidenceDelta    float64          `json:"confidence_delta"`
	Alternatives       []decisionRef    `json:"alternatives"`
	Actor              string           `json:"actor"`
	Rationale          string           `json:"rationale"`
	ReasonCode         string           `json:"reason_code"`
	Verification       map[string]any   `json:"verification"`
	OccurredAt         string           `json:"occurred_at"`
	RecordedAt         string           `json:"recorded_at"`
	PageCursor         string           `json:"page_cursor,omitempty"`
	ledgerSequence     uint64
	idempotentReplay   bool
	occurredAtExplicit bool
}

type decisionChangeCursor struct {
	SchemaID         string `json:"schema_id"`
	Project          string `json:"project"`
	ObjectiveID      string `json:"objective_id"`
	AsOf             string `json:"as_of"`
	OccurredAt       string `json:"occurred_at"`
	RecordedAt       string `json:"recorded_at"`
	LedgerSequence   uint64 `json:"ledger_sequence"`
	DecisionChangeID string `json:"decision_change_id"`
}

type decisionChangeQueryResult struct {
	Rows              []decisionChange
	AsOf              time.Time
	MatchedCount      int
	MatchedCountExact bool
	InspectionCount   int
	InspectionLimit   int
	NextCursor        string
}

var objectiveTransitionTypes = map[string]struct{}{
	"created": {}, "started": {}, "progressed": {}, "blocked": {}, "resumed": {},
	"completed": {}, "abandoned": {}, "reopened": {}, "superseded": {}, "depends_on": {},
	"decision_changed": {}, "outcome_recorded": {}, "checkpointed": {}, "session_linked": {}, "task_linked": {},
}

func normalizeObjectiveStatus(raw string) string {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "planned", "active", "blocked", "completed", "abandoned", "superseded", "unchanged":
		return strings.TrimSpace(strings.ToLower(raw))
	default:
		return ""
	}
}

func objectiveStatusForTransition(transitionType string, supplied string) string {
	if normalized := normalizeObjectiveStatus(supplied); normalized != "" {
		return normalized
	}
	switch transitionType {
	case "created":
		return "planned"
	case "started", "progressed", "resumed", "reopened":
		return "active"
	case "decision_changed", "outcome_recorded", "checkpointed", "session_linked", "task_linked", "depends_on":
		return "unchanged"
	case "blocked":
		return "blocked"
	case "completed":
		return "completed"
	case "abandoned":
		return "abandoned"
	case "superseded":
		return "superseded"
	default:
		return "active"
	}
}

func normalizeObjectiveEvidence(raw any, limit int) []map[string]any {
	out := []map[string]any{}
	for _, value := range contextPackAnyList(raw) {
		var row map[string]any
		switch typed := value.(type) {
		case string:
			if strings.TrimSpace(typed) == "" {
				continue
			}
			row = map[string]any{"ref_id": clipText(strings.TrimSpace(typed), 800), "kind": "evidence"}
		default:
			row = map[string]any{}
			for _, key := range []string{"ref_id", "kind", "memory_id", "uri", "content_hash", "excerpt", "observed_at", "verification"} {
				if item, ok := anyMap(value)[key]; ok {
					row[key] = compactAgentSessionValue(item, 2)
				}
			}
		}
		if strings.TrimSpace(anyToString(row["ref_id"])) == "" {
			row["ref_id"] = clipText(firstNonEmptyStrings(anyToString(row["memory_id"]), anyToString(row["uri"]), anyToString(row["content_hash"])), 800)
		}
		if strings.TrimSpace(anyToString(row["ref_id"])) == "" {
			continue
		}
		if strings.TrimSpace(anyToString(row["kind"])) == "" {
			row["kind"] = "evidence"
		}
		out = append(out, row)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func normalizeContinuityIdempotencyKey(raw string, fallbackPrefix string) (string, error) {
	key := strings.TrimSpace(raw)
	if key == "" {
		key = fallbackPrefix + randomHex(16)
	}
	if !continuityIDPattern.MatchString(key) {
		return "", errors.New("idempotency_key contains unsupported characters")
	}
	return key, nil
}

func normalizeObjectiveTransition(payload map[string]any) (objectiveTransition, error) {
	if err := rejectContinuitySecrets(payload); err != nil {
		return objectiveTransition{}, err
	}
	project, err := sanitizeMemoryProject(firstNonEmptyStrings(anyToString(payload["project"]), anyToString(payload["project_name"])))
	if err != nil {
		return objectiveTransition{}, fmt.Errorf("project: %w", err)
	}
	transitionType := strings.TrimSpace(strings.ToLower(firstNonEmptyStrings(anyToString(payload["transition_type"]), anyToString(payload["type"]))))
	if _, ok := objectiveTransitionTypes[transitionType]; !ok {
		return objectiveTransition{}, errors.New("transition_type is not supported")
	}
	objective := clipText(strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["objective"]), anyToString(payload["title"]), anyToString(payload["summary"]))), 2000)
	parentID := strings.TrimSpace(anyToString(payload["parent_objective_id"]))
	if parentID != "" && !continuityIDPattern.MatchString(parentID) {
		return objectiveTransition{}, errors.New("parent_objective_id contains unsupported characters")
	}
	objectiveID := strings.TrimSpace(anyToString(payload["objective_id"]))
	if objectiveID == "" {
		if objective == "" {
			return objectiveTransition{}, errors.New("objective_id or objective is required")
		}
		objectiveID = "obj_" + sha256Hex(strings.ToLower(project) + "\x00" + parentID + "\x00" + normalizeContinuityObjective(objective))[:32]
	}
	if !continuityIDPattern.MatchString(objectiveID) {
		return objectiveTransition{}, errors.New("objective_id contains unsupported characters")
	}
	occurredAt := strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["occurred_at"]), anyToString(payload["at"])))
	occurredAtExplicit := occurredAt != ""
	if occurredAt == "" {
		occurredAt = nowUTCISO()
	} else if parsed, parseErr := time.Parse(time.RFC3339Nano, occurredAt); parseErr != nil {
		return objectiveTransition{}, errors.New("occurred_at must be RFC3339")
	} else {
		occurredAt = parsed.UTC().Format(time.RFC3339Nano)
	}
	transitionID := strings.TrimSpace(anyToString(payload["transition_id"]))
	idempotencyKey := strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["idempotency_key"]), anyToString(payload["idempotencyKey"])))
	if idempotencyKey == "" && transitionID != "" {
		idempotencyKey = "transition_" + sha256Hex(transitionID)[:32]
	}
	idempotencyKey, err = normalizeContinuityIdempotencyKey(idempotencyKey, "objective_")
	if err != nil {
		return objectiveTransition{}, err
	}
	if transitionID == "" {
		transitionID = "ot_" + sha256Hex(strings.ToLower(project) + "\x00" + idempotencyKey)[:24]
	}
	if !continuityIDPattern.MatchString(transitionID) {
		return objectiveTransition{}, errors.New("transition_id contains unsupported characters")
	}
	actor := clipText(strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["actor"]), anyToString(payload["agent_id"]))), 160)
	if actor == "" {
		return objectiveTransition{}, errors.New("actor or agent_id is required")
	}
	fromStatusRaw := strings.TrimSpace(anyToString(payload["from_status"]))
	fromStatus := normalizeObjectiveStatus(fromStatusRaw)
	if fromStatusRaw != "" && fromStatus == "" {
		return objectiveTransition{}, errors.New("from_status is not supported")
	}
	toStatusRaw := strings.TrimSpace(anyToString(payload["to_status"]))
	toStatus := normalizeObjectiveStatus(toStatusRaw)
	if toStatusRaw != "" && toStatus == "" {
		return objectiveTransition{}, errors.New("to_status is not supported")
	}
	taskIdentityID := strings.TrimSpace(anyToString(payload["task_identity_id"]))
	if taskIdentityID != "" && !continuityIDPattern.MatchString(taskIdentityID) {
		return objectiveTransition{}, errors.New("task_identity_id contains unsupported characters")
	}
	transition := objectiveTransition{
		SchemaID: objectiveTransitionContractID, TransitionID: transitionID, ObjectiveID: objectiveID,
		Project: project, Objective: objective, TransitionType: transitionType,
		FromStatus:        fromStatus,
		ToStatus:          objectiveStatusForTransition(transitionType, toStatus),
		ParentObjectiveID: parentID,
		DependsOn:         normalizeContinuityIDs(payload["depends_on"], objectiveID, 64),
		Supersedes:        normalizeContinuityIDs(payload["supersedes"], objectiveID, 64),
		TaskIdentityID:    taskIdentityID,
		ExecutionLaneID:   strings.TrimSpace(anyToString(payload["execution_lane_id"])),
		SessionID:         clipText(strings.TrimSpace(anyToString(payload["session_id"])), 160),
		DecisionChangeID:  clipText(strings.TrimSpace(anyToString(payload["decision_change_id"])), 160),
		OutcomeID:         clipText(strings.TrimSpace(anyToString(payload["outcome_id"])), 160),
		CheckpointID:      clipText(strings.TrimSpace(anyToString(payload["checkpoint_id"])), 160),
		IdempotencyKey:    idempotencyKey,
		Actor:             actor, Summary: clipText(strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["summary"]), transitionType)), 1200),
		Evidence: normalizeObjectiveEvidence(payload["evidence"], 32),
		Metadata: compactAgentSessionMetadata(anyMap(payload["metadata"])), OccurredAt: occurredAt,
		occurredAtExplicit: occurredAtExplicit,
	}
	if transition.ExecutionLaneID != "" && !continuityIDPattern.MatchString(transition.ExecutionLaneID) {
		return objectiveTransition{}, errors.New("execution_lane_id contains unsupported characters")
	}
	for field, value := range map[string]string{
		"decision_change_id": transition.DecisionChangeID,
		"outcome_id":         transition.OutcomeID,
		"checkpoint_id":      transition.CheckpointID,
	} {
		if value != "" && !continuityIDPattern.MatchString(value) {
			return objectiveTransition{}, fmt.Errorf("%s contains unsupported characters", field)
		}
	}
	return transition, nil
}

func (s *continuityStore) recordObjectiveTransition(payload map[string]any) (objectiveTransition, error) {
	transition, err := normalizeObjectiveTransition(payload)
	if err != nil {
		return objectiveTransition{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	transition.TaskIdentityID, err = s.resolveTaskIdentityLinkLocked(transition.TaskIdentityID, transition.Project)
	if err != nil {
		return objectiveTransition{}, err
	}
	if existingIndex, exists := s.objectiveIdempotency[continuityIdempotencyIndexKey(transition.Project, transition.IdempotencyKey)]; exists {
		if existingIndex < 0 || existingIndex >= len(s.objectiveTransitions) {
			return objectiveTransition{}, errors.New("objective transition idempotency index is corrupt")
		}
		existing := s.objectiveTransitions[existingIndex]
		if !objectiveTransitionEquivalent(existing, transition) {
			return objectiveTransition{}, errors.New("idempotency_key already exists with a different objective transition")
		}
		existing.idempotentReplay = true
		return existing, nil
	}
	if _, exists := s.objectiveTransitionIDs[transition.TransitionID]; exists {
		return objectiveTransition{}, errors.New("transition_id already exists")
	}
	if transition.DecisionChangeID != "" {
		changeIndex, exists := s.decisionChangeIndex[continuityScopedIndexKey(transition.Project, transition.DecisionChangeID)]
		if !exists || changeIndex < 0 || changeIndex >= len(s.decisionChanges) || s.decisionChanges[changeIndex].ObjectiveID != transition.ObjectiveID {
			return objectiveTransition{}, errors.New("decision_change_id must reference a known decision in the same project and objective")
		}
	}
	entries, err := s.appendLocked([]continuityLedgerAppend{{kind: continuityLedgerKindObjectiveTransition, payload: transition}})
	if err != nil {
		return objectiveTransition{}, err
	}
	transition.RecordedAt = entries[0].RecordedAt
	transition.ledgerSequence = entries[0].Sequence
	return transition, nil
}

func objectiveTransitionBeforeOrAt(transition objectiveTransition, asOf time.Time) bool {
	asOf = continuityAsOfOrNow(asOf)
	occurred, err := time.Parse(time.RFC3339Nano, transition.OccurredAt)
	if err != nil || occurred.After(asOf) {
		return false
	}
	recordedAt := firstNonEmptyStrings(transition.RecordedAt, transition.OccurredAt)
	recorded, err := time.Parse(time.RFC3339Nano, recordedAt)
	return err == nil && !recorded.After(asOf)
}

func objectiveGraphEdgeID(fromID string, toID string, edgeType string, occurredAt string) string {
	return "oge_" + sha256Hex(fromID + "\x00" + toID + "\x00" + edgeType + "\x00" + occurredAt)[:24]
}

func objectiveGraphAddEdgeBounded(edges *[]objectiveGraphEdge, seen map[string]struct{}, limit int, fromID string, toID string, edgeType string, occurredAt string) bool {
	if fromID == "" || toID == "" {
		return true
	}
	edge := objectiveGraphEdge{EdgeID: objectiveGraphEdgeID(fromID, toID, edgeType, occurredAt), FromID: fromID, ToID: toID, Type: edgeType, OccurredAt: occurredAt}
	if _, exists := seen[edge.EdgeID]; exists {
		return true
	}
	if len(*edges) >= limit {
		return false
	}
	seen[edge.EdgeID] = struct{}{}
	*edges = append(*edges, edge)
	return true
}

func objectiveGraphEnsureNode(nodes map[string]objectiveGraphNode, objectiveID string, project string) objectiveGraphNode {
	if node, ok := nodes[objectiveID]; ok {
		return node
	}
	return objectiveGraphNode{
		ObjectiveID: objectiveID, Project: project, Status: "planned",
		TaskIdentityIDs: []string{}, ExecutionLaneIDs: []string{}, SessionIDs: []string{}, DecisionChangeIDs: []string{},
		OutcomeIDs: []string{}, CheckpointIDs: []string{},
	}
}

func objectiveGraphTransitionEligible(transition objectiveTransition, project string, asOf time.Time) bool {
	return (project == "" || strings.EqualFold(transition.Project, project)) &&
		objectiveTransitionBeforeOrAt(transition, asOf)
}

func continuityTimestampCompare(left string, right string) int {
	leftTime, leftErr := time.Parse(time.RFC3339Nano, left)
	rightTime, rightErr := time.Parse(time.RFC3339Nano, right)
	if leftErr == nil && rightErr == nil {
		if leftTime.Before(rightTime) {
			return -1
		}
		if leftTime.After(rightTime) {
			return 1
		}
		return 0
	}
	return strings.Compare(left, right)
}

func objectiveTransitionChronologyLess(left objectiveTransition, right objectiveTransition) bool {
	if compared := continuityTimestampCompare(left.OccurredAt, right.OccurredAt); compared != 0 {
		return compared < 0
	}
	if compared := continuityTimestampCompare(left.RecordedAt, right.RecordedAt); compared != 0 {
		return compared < 0
	}
	if left.ledgerSequence != right.ledgerSequence {
		return left.ledgerSequence < right.ledgerSequence
	}
	return left.TransitionID < right.TransitionID
}

func (s *continuityStore) objectiveGraphSelectionLocked(project string, objectiveID string, asOf time.Time, limit int) (map[string]struct{}, int, int, int, int, bool) {
	selected := map[string]struct{}{}
	selectionLimit := minInt(maxInt(limit*objectiveGraphRelationInspectionsPerNode, objectiveGraphRelationInspectionsPerNode), objectiveGraphMaxSelectionInspections)
	selectionInspections := 0
	if objectiveID == "" {
		truncated := false
		indexes := s.objectiveProjectIndex[strings.ToLower(strings.TrimSpace(project))]
		for position := len(indexes) - 1; position >= 0; position-- {
			if selectionInspections >= selectionLimit {
				truncated = true
				break
			}
			selectionInspections++
			index := indexes[position]
			if index < 0 || index >= len(s.objectiveTransitions) {
				continue
			}
			transition := s.objectiveTransitions[index]
			if !objectiveGraphTransitionEligible(transition, project, asOf) {
				continue
			}
			if _, exists := selected[transition.ObjectiveID]; exists {
				continue
			}
			if len(selected) >= limit {
				truncated = true
				break
			}
			selected[transition.ObjectiveID] = struct{}{}
		}
		return selected, 0, 0, selectionInspections, selectionLimit, truncated
	}

	known := false
	objectiveKey := continuityScopedIndexKey(project, objectiveID)
	for position := len(s.objectiveTransitionIndex[objectiveKey]) - 1; position >= 0 && selectionInspections < selectionLimit; position-- {
		selectionInspections++
		index := s.objectiveTransitionIndex[objectiveKey][position]
		if index >= 0 && index < len(s.objectiveTransitions) && objectiveGraphTransitionEligible(s.objectiveTransitions[index], project, asOf) {
			known = true
			break
		}
	}
	if !known {
		for position := len(s.objectiveRelationIndex[objectiveKey]) - 1; position >= 0 && selectionInspections < selectionLimit; position-- {
			selectionInspections++
			index := s.objectiveRelationIndex[objectiveKey][position].TransitionIndex
			if index >= 0 && index < len(s.objectiveTransitions) && objectiveGraphTransitionEligible(s.objectiveTransitions[index], project, asOf) {
				known = true
				break
			}
		}
	}
	if !known {
		return selected, 0, 0, selectionInspections, selectionLimit, selectionInspections >= selectionLimit
	}
	selected[objectiveID] = struct{}{}
	queue := []string{objectiveID}
	inspectionLimit := minInt(
		maxInt(limit*objectiveGraphRelationInspectionsPerNode, objectiveGraphRelationInspectionsPerNode),
		objectiveGraphMaxRelationInspections,
	)
	inspections := 0
	truncated := false

traversal:
	for queueIndex := 0; queueIndex < len(queue); queueIndex++ {
		current := queue[queueIndex]
		relations := s.objectiveRelationIndex[continuityScopedIndexKey(project, current)]
		for relationIndex := len(relations) - 1; relationIndex >= 0; relationIndex-- {
			if inspections >= inspectionLimit {
				truncated = true
				break traversal
			}
			inspections++
			relation := relations[relationIndex]
			if relation.TransitionIndex < 0 || relation.TransitionIndex >= len(s.objectiveTransitions) {
				continue
			}
			transition := s.objectiveTransitions[relation.TransitionIndex]
			if !objectiveGraphTransitionEligible(transition, project, asOf) {
				continue
			}
			if _, exists := selected[relation.RelatedObjectiveID]; exists {
				continue
			}
			if len(selected) >= limit {
				truncated = true
				break traversal
			}
			selected[relation.RelatedObjectiveID] = struct{}{}
			queue = append(queue, relation.RelatedObjectiveID)
		}
	}
	return selected, inspections, inspectionLimit, selectionInspections, selectionLimit, truncated
}

func (s *continuityStore) objectiveGraphReplaySelectedLocked(
	project string,
	selected map[string]struct{},
	asOf time.Time,
	inspectionLimit int,
) ([]objectiveTransition, int, bool, int) {
	objectiveIDs := make([]string, 0, len(selected))
	for objectiveID := range selected {
		objectiveIDs = append(objectiveIDs, objectiveID)
	}
	sort.Strings(objectiveIDs)
	cursors := make(objectiveReplayHeap, 0, len(objectiveIDs))
	for _, objectiveID := range objectiveIDs {
		indexes := s.objectiveTransitionIndex[continuityScopedIndexKey(project, objectiveID)]
		if len(indexes) == 0 {
			continue
		}
		cursors = append(cursors, objectiveReplayCursor{
			objectiveID: objectiveID,
			indexes:     indexes,
			position:    len(indexes) - 1,
		})
	}
	heap.Init(&cursors)
	filtered := make([]objectiveTransition, 0, minInt(inspectionLimit, maxInt(len(selected), 64)))
	seenIndexes := make(map[int]struct{}, minInt(inspectionLimit, 1024))
	inspections := 0
	invalidIndexes := 0
	for cursors.Len() > 0 && inspections < inspectionLimit {
		cursor := heap.Pop(&cursors).(objectiveReplayCursor)
		index := cursor.indexes[cursor.position]
		inspections++
		if _, duplicate := seenIndexes[index]; duplicate {
			invalidIndexes++
		} else {
			seenIndexes[index] = struct{}{}
			if index < 0 || index >= len(s.objectiveTransitions) {
				invalidIndexes++
			} else {
				transition := s.objectiveTransitions[index]
				if transition.ObjectiveID != cursor.objectiveID || !strings.EqualFold(transition.Project, project) {
					invalidIndexes++
				} else if objectiveGraphTransitionEligible(transition, project, asOf) {
					filtered = append(filtered, transition)
				}
			}
		}
		cursor.position--
		if cursor.position >= 0 {
			heap.Push(&cursors, cursor)
		}
	}
	return filtered, inspections, cursors.Len() > 0, invalidIndexes
}

func (s *continuityStore) objectiveGraph(project string, objectiveID string, asOf time.Time, includeTransitions bool, limit int) map[string]any {
	asOf = continuityAsOfOrNow(asOf)
	limit = clampInt(limit, 1, 5000)
	project = strings.TrimSpace(project)
	if project == "" {
		return map[string]any{"ok": false, "schema_id": objectiveGraphContractID, "error": "project is required"}
	}
	s.mu.RLock()
	selected, relationInspections, relationInspectionLimit, selectionInspections, selectionInspectionLimit, traversalTruncated := s.objectiveGraphSelectionLocked(project, objectiveID, asOf, limit)
	replayInspectionLimit := objectiveGraphMaxReplayInspections
	filtered, replayInspections, replayTruncated, invalidReplayIndexes := s.objectiveGraphReplaySelectedLocked(
		project, selected, asOf, replayInspectionLimit,
	)
	s.mu.RUnlock()
	if invalidReplayIndexes > 0 {
		return map[string]any{
			"ok": false, "schema_id": objectiveGraphContractID, "error": "objective_transition_index_invalid",
			"project": project, "objective_id": objectiveID, "as_of": asOf.UTC().Format(time.RFC3339Nano),
			"complete": false, "graph_truncated": true, "index_integrity_valid": false,
			"invalid_index_count":     invalidReplayIndexes,
			"replay_inspection_count": replayInspections, "replay_inspection_limit": replayInspectionLimit,
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		return objectiveTransitionChronologyLess(filtered[i], filtered[j])
	})

	nodes := map[string]objectiveGraphNode{}
	nodeLinkLimit := minInt(limit, 64)
	nodeLinkTruncated := false
	mergeNodeLink := func(values []string, value string) []string {
		value = strings.TrimSpace(value)
		if value == "" {
			return values
		}
		for _, existing := range values {
			if strings.EqualFold(existing, value) {
				return values
			}
		}
		if len(values) >= nodeLinkLimit {
			nodeLinkTruncated = true
			return values
		}
		return mergeContinuityStrings(values, []string{value}, nodeLinkLimit)
	}
	for _, transition := range filtered {
		node := objectiveGraphEnsureNode(nodes, transition.ObjectiveID, transition.Project)
		if transition.Objective != "" {
			node.Objective = transition.Objective
		}
		if node.CreatedAt == "" {
			node.CreatedAt = transition.OccurredAt
		}
		if transition.ToStatus != "unchanged" {
			node.Status = transition.ToStatus
		}
		node.UpdatedAt = transition.OccurredAt
		node.LastSummary = transition.Summary
		if transition.ParentObjectiveID != "" {
			node.ParentObjectiveID = transition.ParentObjectiveID
		}
		if transition.TaskIdentityID != "" {
			node.TaskIdentityIDs = mergeNodeLink(node.TaskIdentityIDs, transition.TaskIdentityID)
		}
		if transition.ExecutionLaneID != "" {
			node.ExecutionLaneIDs = mergeNodeLink(node.ExecutionLaneIDs, transition.ExecutionLaneID)
		}
		if transition.SessionID != "" {
			node.SessionIDs = mergeNodeLink(node.SessionIDs, transition.SessionID)
		}
		if transition.DecisionChangeID != "" {
			node.DecisionChangeIDs = mergeNodeLink(node.DecisionChangeIDs, transition.DecisionChangeID)
		}
		if transition.OutcomeID != "" {
			node.OutcomeIDs = mergeNodeLink(node.OutcomeIDs, transition.OutcomeID)
		}
		if transition.CheckpointID != "" {
			node.CheckpointIDs = mergeNodeLink(node.CheckpointIDs, transition.CheckpointID)
		}
		nodes[node.ObjectiveID] = node
	}
	for selectedID := range selected {
		if _, exists := nodes[selectedID]; !exists {
			nodes[selectedID] = objectiveGraphEnsureNode(nodes, selectedID, project)
		}
	}

	edges := make([]objectiveGraphEdge, 0, minInt(limit, len(filtered)))
	edgeSeen := map[string]struct{}{}
	edgeTruncated := false
	addEdge := func(fromID string, toID string, edgeType string, occurredAt string) bool {
		if !objectiveGraphAddEdgeBounded(&edges, edgeSeen, limit, fromID, toID, edgeType, occurredAt) {
			edgeTruncated = true
			return false
		}
		return true
	}

edgeCollection:
	for index := len(filtered) - 1; index >= 0; index-- {
		transition := filtered[index]
		if _, parentSelected := selected[transition.ParentObjectiveID]; parentSelected {
			if !addEdge(transition.ParentObjectiveID, transition.ObjectiveID, "parent_of", transition.OccurredAt) {
				break edgeCollection
			}
		}
		for _, dependency := range transition.DependsOn {
			if _, relatedSelected := selected[dependency]; relatedSelected &&
				!addEdge(transition.ObjectiveID, dependency, "depends_on", transition.OccurredAt) {
				break edgeCollection
			}
		}
		for _, superseded := range transition.Supersedes {
			if _, relatedSelected := selected[superseded]; relatedSelected &&
				!addEdge(transition.ObjectiveID, superseded, "supersedes", transition.OccurredAt) {
				break edgeCollection
			}
		}
		if !addEdge(transition.ObjectiveID, transition.TaskIdentityID, "task_link", transition.OccurredAt) ||
			!addEdge(transition.ObjectiveID, transition.ExecutionLaneID, "execution_lane_link", transition.OccurredAt) ||
			!addEdge(transition.ObjectiveID, transition.SessionID, "session_link", transition.OccurredAt) ||
			!addEdge(transition.ObjectiveID, transition.DecisionChangeID, "decision_change", transition.OccurredAt) ||
			!addEdge(transition.ObjectiveID, transition.OutcomeID, "outcome_link", transition.OccurredAt) ||
			!addEdge(transition.ObjectiveID, transition.CheckpointID, "checkpoint_link", transition.OccurredAt) {
			break edgeCollection
		}
	}

	nodeRows := make([]objectiveGraphNode, 0, len(nodes))
	for _, node := range nodes {
		nodeRows = append(nodeRows, node)
	}
	sort.Slice(nodeRows, func(i, j int) bool { return nodeRows[i].ObjectiveID < nodeRows[j].ObjectiveID })
	sort.Slice(edges, func(i, j int) bool {
		if compared := continuityTimestampCompare(edges[i].OccurredAt, edges[j].OccurredAt); compared != 0 {
			return compared < 0
		}
		return edges[i].EdgeID < edges[j].EdgeID
	})
	transitionRows := filtered
	transitionOmitted := 0
	if len(transitionRows) > limit {
		transitionOmitted = len(transitionRows) - limit
		transitionRows = transitionRows[len(transitionRows)-limit:]
	}
	transitionTruncated := includeTransitions && transitionOmitted > 0
	if !includeTransitions {
		transitionRows = nil
		transitionOmitted = len(filtered)
	}
	graphTruncated := traversalTruncated || replayTruncated || nodeLinkTruncated || edgeTruncated || transitionTruncated

	response := map[string]any{
		"ok": true, "schema_id": objectiveGraphContractID, "project": project, "objective_id": objectiveID,
		"as_of":    asOf.UTC().Format(time.RFC3339Nano),
		"complete": !graphTruncated, "limit": limit, "graph_truncated": graphTruncated, "boundary_compacted": false,
		"traversal_truncated": traversalTruncated, "node_link_truncated": nodeLinkTruncated,
		"edge_truncated": edgeTruncated, "transition_truncated": transitionTruncated,
		"replay_truncated":          replayTruncated,
		"index_integrity_valid":     true,
		"invalid_index_count":       0,
		"transitions_included":      includeTransitions,
		"relation_inspection_count": relationInspections, "relation_inspection_limit": relationInspectionLimit,
		"selection_inspection_count": selectionInspections, "selection_inspection_limit": selectionInspectionLimit,
		"replay_inspection_count": replayInspections, "replay_inspection_limit": replayInspectionLimit,
		"nodes": nodeRows, "edges": edges, "node_count": len(nodeRows), "edge_count": len(edges),
		"transition_count": len(filtered), "transition_count_exact": !replayTruncated,
		"transition_omitted_count": transitionOmitted, "ledger_status": s.snapshot(),
	}
	if includeTransitions {
		response["transitions"] = transitionRows
	} else {
		response["transitions"] = []any{}
	}
	return response
}

func containsForbiddenDecisionReasoning(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			normalized := strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(key)))
			switch normalized {
			case "chainofthought", "rawreasoning", "internalreasoning", "hiddenreasoning", "reasoningtrace", "analysistrace":
				return true
			}
			if containsForbiddenDecisionReasoning(item) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if containsForbiddenDecisionReasoning(item) {
				return true
			}
		}
	}
	return false
}

func normalizeDecisionRef(raw any, prefix string, project string, objectiveID string) (decisionRef, error) {
	row := anyMap(raw)
	summary := ""
	decisionID := ""
	if len(row) > 0 {
		summary = clipText(strings.TrimSpace(firstNonEmptyStrings(anyToString(row["summary"]), anyToString(row["decision"]), anyToString(row["value"]))), 1200)
		decisionID = strings.TrimSpace(anyToString(row["decision_id"]))
	} else {
		summary = clipText(strings.TrimSpace(anyToString(raw)), 1200)
	}
	if summary == "" && decisionID == "" {
		return decisionRef{}, errors.New(prefix + " decision is required")
	}
	if decisionID == "" {
		decisionID = "decision_" + sha256Hex(project + "\x00" + objectiveID + "\x00" + normalizeContinuityObjective(summary))[:32]
	}
	if !continuityIDPattern.MatchString(decisionID) {
		return decisionRef{}, errors.New(prefix + " decision_id contains unsupported characters")
	}
	return decisionRef{DecisionID: decisionID, Summary: summary}, nil
}

func decisionRefsEquivalentMeaning(before decisionRef, after decisionRef) bool {
	if before.DecisionID == after.DecisionID || normalizeContinuityObjective(before.Summary) == normalizeContinuityObjective(after.Summary) {
		return true
	}
	beforeTokens := continuityObjectiveTokenSlice(before.Summary)
	afterTokens := continuityObjectiveTokenSlice(after.Summary)
	return len(beforeTokens) > 0 && len(afterTokens) > 0 && continuitySemanticTokenScore(beforeTokens, afterTokens) == 1
}

func normalizeDecisionAlternatives(raw any, project string, objectiveID string) []decisionRef {
	out := []decisionRef{}
	for index, value := range contextPackAnyList(raw) {
		row, err := normalizeDecisionRef(value, fmt.Sprintf("alternative_%d", index+1), project, objectiveID)
		if err != nil {
			continue
		}
		out = append(out, row)
		if len(out) >= 24 {
			break
		}
	}
	return out
}

func decisionConfidence(payload map[string]any, name string, nested map[string]any) (float64, error) {
	value, exists := payload[name]
	if !exists {
		value, exists = nested["confidence"]
	}
	if !exists || value == nil || strings.TrimSpace(anyToString(value)) == "" {
		return 0, fmt.Errorf("%s is required", name)
	}
	confidence := anyToFloat64(value, -1)
	if confidence < 0 || confidence > 1 {
		return 0, fmt.Errorf("%s must be between 0 and 1", name)
	}
	return confidence, nil
}

func normalizeDecisionChange(payload map[string]any) (decisionChange, objectiveTransition, error) {
	if err := rejectContinuitySecrets(payload); err != nil {
		return decisionChange{}, objectiveTransition{}, err
	}
	if containsForbiddenDecisionReasoning(payload) {
		return decisionChange{}, objectiveTransition{}, errors.New("hidden chain-of-thought or raw reasoning fields are forbidden; provide concise rationale and evidence references")
	}
	project, err := sanitizeMemoryProject(firstNonEmptyStrings(anyToString(payload["project"]), anyToString(payload["project_name"])))
	if err != nil {
		return decisionChange{}, objectiveTransition{}, fmt.Errorf("project: %w", err)
	}
	objectiveID := strings.TrimSpace(anyToString(payload["objective_id"]))
	objective := clipText(strings.TrimSpace(anyToString(payload["objective"])), 2000)
	if objectiveID == "" && objective != "" {
		objectiveID = "obj_" + sha256Hex(strings.ToLower(project) + "\x00" + normalizeContinuityObjective(objective))[:32]
	}
	if objectiveID == "" || !continuityIDPattern.MatchString(objectiveID) {
		return decisionChange{}, objectiveTransition{}, errors.New("valid objective_id or objective is required")
	}
	beforeRaw := firstNonEmptyAny(payload["before"], payload["before_decision"])
	afterRaw := firstNonEmptyAny(payload["after"], payload["after_decision"])
	before, err := normalizeDecisionRef(beforeRaw, "before", project, objectiveID)
	if err != nil {
		return decisionChange{}, objectiveTransition{}, err
	}
	after, err := normalizeDecisionRef(afterRaw, "after", project, objectiveID)
	if err != nil {
		return decisionChange{}, objectiveTransition{}, err
	}
	confidenceBefore, err := decisionConfidence(payload, "confidence_before", anyMap(beforeRaw))
	if err != nil {
		return decisionChange{}, objectiveTransition{}, err
	}
	confidenceAfter, err := decisionConfidence(payload, "confidence_after", anyMap(afterRaw))
	if err != nil {
		return decisionChange{}, objectiveTransition{}, err
	}
	confidenceDelta := math.Round((confidenceAfter-confidenceBefore)*10000) / 10000
	if decisionRefsEquivalentMeaning(before, after) && confidenceDelta == 0 {
		return decisionChange{}, objectiveTransition{}, errors.New("decision change must alter the conclusion or its confidence; wording-only or evidence-only restatements belong in ordinary provenance")
	}
	evidence := normalizeObjectiveEvidence(firstNonEmptyAny(payload["trigger_evidence"], payload["evidence"]), 32)
	if len(evidence) == 0 {
		return decisionChange{}, objectiveTransition{}, errors.New("trigger_evidence requires at least one bounded reference")
	}
	actor := clipText(strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["actor"]), anyToString(payload["agent_id"]))), 160)
	if actor == "" {
		return decisionChange{}, objectiveTransition{}, errors.New("actor or agent_id is required")
	}
	rationale := clipText(strings.TrimSpace(anyToString(payload["rationale"])), 1200)
	if rationale == "" {
		return decisionChange{}, objectiveTransition{}, errors.New("concise rationale is required")
	}
	reasonCode := strings.ToLower(strings.TrimSpace(anyToString(payload["reason_code"])))
	reasonCode = strings.ReplaceAll(reasonCode, " ", "_")
	if reasonCode == "" || !continuityIDPattern.MatchString(reasonCode) {
		return decisionChange{}, objectiveTransition{}, errors.New("reason_code is required and must be machine-safe")
	}
	occurredAt := strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["occurred_at"]), anyToString(payload["at"])))
	occurredAtExplicit := occurredAt != ""
	if occurredAt == "" {
		occurredAt = nowUTCISO()
	} else if parsed, parseErr := time.Parse(time.RFC3339Nano, occurredAt); parseErr != nil {
		return decisionChange{}, objectiveTransition{}, errors.New("occurred_at must be RFC3339")
	} else {
		occurredAt = parsed.UTC().Format(time.RFC3339Nano)
	}
	changeID := strings.TrimSpace(anyToString(payload["decision_change_id"]))
	idempotencyKey := strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["idempotency_key"]), anyToString(payload["idempotencyKey"])))
	if idempotencyKey == "" && changeID != "" {
		idempotencyKey = "decision_" + sha256Hex(changeID)[:32]
	}
	idempotencyKey, err = normalizeContinuityIdempotencyKey(idempotencyKey, "decision_")
	if err != nil {
		return decisionChange{}, objectiveTransition{}, err
	}
	if changeID == "" {
		changeID = "dc_" + sha256Hex(strings.ToLower(project) + "\x00" + idempotencyKey)[:24]
	}
	if !continuityIDPattern.MatchString(changeID) {
		return decisionChange{}, objectiveTransition{}, errors.New("decision_change_id contains unsupported characters")
	}
	verification := normalizeClaimVerification(payload["verification"])
	change := decisionChange{
		SchemaID: decisionChangeContractID, DecisionChangeID: changeID, Project: project, ObjectiveID: objectiveID,
		TaskIdentityID: clipText(strings.TrimSpace(anyToString(payload["task_identity_id"])), 160),
		SessionID:      clipText(strings.TrimSpace(anyToString(payload["session_id"])), 160),
		IdempotencyKey: idempotencyKey,
		Before:         before, After: after, TriggerEvidence: evidence,
		ConfidenceBefore: confidenceBefore, ConfidenceAfter: confidenceAfter,
		ConfidenceDelta: confidenceDelta,
		Alternatives:    normalizeDecisionAlternatives(payload["alternatives"], project, objectiveID),
		Actor:           actor, Rationale: rationale, ReasonCode: reasonCode, Verification: verification, OccurredAt: occurredAt,
		occurredAtExplicit: occurredAtExplicit,
	}
	transition, err := normalizeObjectiveTransition(map[string]any{
		"project": project, "objective_id": objectiveID, "objective": objective,
		"transition_id":   "ot_" + sha256Hex(strings.ToLower(project) + "\x00decision-transition\x00" + idempotencyKey)[:24],
		"idempotency_key": "decision-transition:" + sha256Hex(strings.ToLower(project) + "\x00" + idempotencyKey)[:24],
		"transition_type": "decision_changed", "decision_change_id": changeID,
		"task_identity_id": change.TaskIdentityID, "session_id": change.SessionID,
		"execution_lane_id": anyToString(payload["execution_lane_id"]), "actor": actor,
		"summary": "Decision changed: " + clipText(after.Summary, 800), "evidence": evidence,
		"occurred_at": occurredAt, "metadata": map[string]any{"reason_code": reasonCode, "confidence_delta": change.ConfidenceDelta},
	})
	if err != nil {
		return decisionChange{}, objectiveTransition{}, err
	}
	transition.occurredAtExplicit = occurredAtExplicit
	return change, transition, nil
}

func (s *continuityStore) recordDecisionChange(payload map[string]any) (decisionChange, objectiveTransition, error) {
	change, transition, err := normalizeDecisionChange(payload)
	if err != nil {
		return decisionChange{}, objectiveTransition{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	change.TaskIdentityID, err = s.resolveTaskIdentityLinkLocked(change.TaskIdentityID, change.Project)
	if err != nil {
		return decisionChange{}, objectiveTransition{}, err
	}
	transition.TaskIdentityID = change.TaskIdentityID
	if existingIndex, exists := s.decisionIdempotency[continuityIdempotencyIndexKey(change.Project, change.IdempotencyKey)]; exists {
		if existingIndex < 0 || existingIndex >= len(s.decisionChanges) {
			return decisionChange{}, objectiveTransition{}, errors.New("decision change idempotency index is corrupt")
		}
		existingChange := s.decisionChanges[existingIndex]
		transitionIndex, transitionExists := s.decisionTransitionIndex[continuityScopedIndexKey(change.Project, existingChange.DecisionChangeID)]
		if !transitionExists || transitionIndex < 0 || transitionIndex >= len(s.objectiveTransitions) {
			return decisionChange{}, objectiveTransition{}, errors.New("decision objective transition index is corrupt")
		}
		existingTransition := s.objectiveTransitions[transitionIndex]
		if !decisionChangeEquivalent(existingChange, change) || !objectiveTransitionEquivalent(existingTransition, transition) {
			return decisionChange{}, objectiveTransition{}, errors.New("idempotency_key already exists with different decision provenance")
		}
		existingChange.idempotentReplay = true
		existingTransition.idempotentReplay = true
		return existingChange, existingTransition, nil
	}
	if _, exists := s.decisionChangeIDs[change.DecisionChangeID]; exists {
		return decisionChange{}, objectiveTransition{}, errors.New("decision_change_id already exists")
	}
	if _, exists := s.objectiveTransitionIDs[transition.TransitionID]; exists {
		return decisionChange{}, objectiveTransition{}, errors.New("decision objective transition id already exists")
	}
	bundle := continuityDecisionBundle{
		SchemaID:            continuityDecisionBundleSchemaID,
		DecisionChange:      change,
		ObjectiveTransition: transition,
	}
	entries, err := s.appendLocked([]continuityLedgerAppend{{kind: continuityLedgerKindDecisionBundle, payload: bundle}})
	if err != nil {
		return decisionChange{}, objectiveTransition{}, err
	}
	change.RecordedAt = entries[0].RecordedAt
	transition.RecordedAt = entries[0].RecordedAt
	change.ledgerSequence = entries[0].Sequence
	transition.ledgerSequence = entries[0].Sequence
	return change, transition, nil
}

func decisionChangeChronologyNewer(left decisionChange, right decisionChange) bool {
	if compared := continuityTimestampCompare(left.OccurredAt, right.OccurredAt); compared != 0 {
		return compared > 0
	}
	if compared := continuityTimestampCompare(left.RecordedAt, right.RecordedAt); compared != 0 {
		return compared > 0
	}
	if left.ledgerSequence != right.ledgerSequence {
		return left.ledgerSequence > right.ledgerSequence
	}
	return left.DecisionChangeID < right.DecisionChangeID
}

func decisionChangeChronologyOlder(left decisionChange, right decisionChange) bool {
	return decisionChangeChronologyNewer(right, left)
}

func encodeDecisionChangeCursor(change decisionChange, project string, objectiveID string, asOf time.Time) string {
	raw, err := json.Marshal(decisionChangeCursor{
		SchemaID: decisionChangeCursorSchemaID, Project: strings.ToLower(strings.TrimSpace(project)),
		ObjectiveID: strings.TrimSpace(objectiveID), AsOf: asOf.UTC().Format(time.RFC3339Nano),
		OccurredAt: change.OccurredAt, RecordedAt: change.RecordedAt,
		LedgerSequence: change.ledgerSequence, DecisionChangeID: change.DecisionChangeID,
	})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeDecisionChangeCursor(raw string) (decisionChangeCursor, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return decisionChangeCursor{}, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(decoded) > 2048 {
		return decisionChangeCursor{}, errors.New("cursor is invalid")
	}
	var cursor decisionChangeCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil || cursor.SchemaID != decisionChangeCursorSchemaID ||
		cursor.Project == "" || cursor.AsOf == "" || cursor.DecisionChangeID == "" || cursor.LedgerSequence == 0 {
		return decisionChangeCursor{}, errors.New("cursor is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, cursor.OccurredAt); err != nil {
		return decisionChangeCursor{}, errors.New("cursor occurred_at is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, cursor.RecordedAt); err != nil {
		return decisionChangeCursor{}, errors.New("cursor recorded_at is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, cursor.AsOf); err != nil {
		return decisionChangeCursor{}, errors.New("cursor as_of is invalid")
	}
	return cursor, nil
}

func (s *continuityStore) queryDecisionChangesPage(project string, objectiveID string, asOf time.Time, limit int, cursorRaw string) (decisionChangeQueryResult, error) {
	project = strings.TrimSpace(project)
	objectiveID = strings.TrimSpace(objectiveID)
	if project == "" {
		return decisionChangeQueryResult{}, errors.New("project is required")
	}
	cursor, err := decodeDecisionChangeCursor(cursorRaw)
	if err != nil {
		return decisionChangeQueryResult{}, err
	}
	if cursor.DecisionChangeID != "" {
		if !strings.EqualFold(cursor.Project, project) || cursor.ObjectiveID != objectiveID {
			return decisionChangeQueryResult{}, errors.New("cursor does not match the project and objective query")
		}
		cursorAsOf, _ := time.Parse(time.RFC3339Nano, cursor.AsOf)
		if !asOf.IsZero() && !asOf.UTC().Equal(cursorAsOf.UTC()) {
			return decisionChangeQueryResult{}, errors.New("cursor does not match the as_of query")
		}
		asOf = cursorAsOf.UTC()
	} else {
		asOf = continuityAsOfOrNow(asOf)
	}
	limit = clampInt(limit, 1, 500)
	s.mu.RLock()
	indexes := s.decisionProjectIndex[strings.ToLower(project)]
	startPosition := len(indexes) - 1
	if cursor.DecisionChangeID != "" {
		cursorIndex, exists := s.decisionChangeIndex[continuityScopedIndexKey(project, cursor.DecisionChangeID)]
		if !exists || cursorIndex < 0 || cursorIndex >= len(s.decisionChanges) {
			s.mu.RUnlock()
			return decisionChangeQueryResult{}, errors.New("cursor decision is unavailable")
		}
		cursorChange := s.decisionChanges[cursorIndex]
		if cursorChange.OccurredAt != cursor.OccurredAt || cursorChange.RecordedAt != cursor.RecordedAt || cursorChange.ledgerSequence != cursor.LedgerSequence {
			s.mu.RUnlock()
			return decisionChangeQueryResult{}, errors.New("cursor decision does not match durable provenance")
		}
		cursorPosition := sort.Search(len(indexes), func(position int) bool {
			index := indexes[position]
			return index < 0 || index >= len(s.decisionChanges) || !decisionChangeChronologyOlder(s.decisionChanges[index], cursorChange)
		})
		if cursorPosition >= len(indexes) || indexes[cursorPosition] != cursorIndex {
			s.mu.RUnlock()
			return decisionChangeQueryResult{}, errors.New("cursor decision is not in the project chronology index")
		}
		startPosition = cursorPosition - 1
	}
	rows := make([]decisionChange, 0, minInt(maxInt(limit+1, 32), decisionChangeMaxQueryInspections))
	inspections := 0
	position := startPosition
	lastInspected := decisionChange{}
	for ; position >= 0 && inspections < decisionChangeMaxQueryInspections; position-- {
		inspections++
		index := indexes[position]
		if index < 0 || index >= len(s.decisionChanges) {
			s.mu.RUnlock()
			return decisionChangeQueryResult{}, errors.New("decision chronology index is corrupt")
		}
		change := s.decisionChanges[index]
		lastInspected = change
		if objectiveID != "" && change.ObjectiveID != objectiveID {
			continue
		}
		occurred, occurredErr := time.Parse(time.RFC3339Nano, change.OccurredAt)
		recorded, recordedErr := time.Parse(time.RFC3339Nano, firstNonEmptyStrings(change.RecordedAt, change.OccurredAt))
		if occurredErr != nil || recordedErr != nil || occurred.After(asOf) || recorded.After(asOf) {
			continue
		}
		rows = append(rows, change)
	}
	inspectionTruncated := position >= 0
	s.mu.RUnlock()
	matchedCount := len(rows)
	nextCursor := ""
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	for index := range rows {
		rows[index].PageCursor = encodeDecisionChangeCursor(rows[index], project, objectiveID, asOf)
	}
	if hasMore && len(rows) > 0 {
		nextCursor = rows[len(rows)-1].PageCursor
	} else if inspectionTruncated && lastInspected.DecisionChangeID != "" {
		nextCursor = encodeDecisionChangeCursor(lastInspected, project, objectiveID, asOf)
	}
	return decisionChangeQueryResult{
		Rows: rows, AsOf: asOf, MatchedCount: matchedCount, MatchedCountExact: !inspectionTruncated,
		InspectionCount: inspections, InspectionLimit: decisionChangeMaxQueryInspections, NextCursor: nextCursor,
	}, nil

}

func (s *continuityStore) queryDecisionChanges(project string, objectiveID string, asOf time.Time, limit int) []decisionChange {
	result, _ := s.queryDecisionChangesPage(project, objectiveID, asOf, limit, "")
	return result.Rows
}

func continuityAsOfOrNow(asOf time.Time) time.Time {
	if asOf.IsZero() {
		return time.Now().UTC()
	}
	return asOf.UTC()
}

func parseContinuityAsOf(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, errors.New("as_of must be RFC3339")
	}
	return parsed.UTC(), nil
}

func (s *server) memoryObjectiveTransition(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	if s.continuity == nil || !s.continuity.enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "continuity_ledger_unavailable", "status": s.continuity.snapshot()})
		return
	}
	payload, err := readOptionalJSONBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
		return
	}
	if !s.enforceOptionalFrontierT1ProjectBoundary(w, r, "objective") {
		return
	}
	if strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["idempotency_key"]), anyToString(payload["idempotencyKey"]))) == "" {
		if headerKey := strings.TrimSpace(r.Header.Get("Idempotency-Key")); headerKey != "" {
			payload["idempotency_key"] = headerKey
		}
	}
	if strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["idempotency_key"]), anyToString(payload["transition_id"]))) == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "objective_transition_idempotency_required", "detail": "provide idempotency_key, Idempotency-Key, or transition_id"})
		return
	}
	transition, err := s.continuity.recordObjectiveTransition(payload)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "invalid_objective_transition", "detail": err.Error()})
		return
	}
	response := map[string]any{
		"ok": true, "schema_id": objectiveTransitionContractID, "transition": transition,
		"recorded": !transition.idempotentReplay, "idempotent_replay": transition.idempotentReplay,
		"ledger_status": s.continuity.snapshot(),
	}
	writeJSON(w, http.StatusOK, attachPayloadFormatContract(objectiveTransitionContractID, response, anyToString(payload["agent_id"]), "objective_transition", r.URL.Path))
}

func (s *server) memoryObjectiveGraph(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	if s.continuity == nil || !s.continuity.enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "continuity_ledger_unavailable", "status": s.continuity.snapshot()})
		return
	}
	if !s.enforceOptionalFrontierT1ProjectBoundary(w, r, "objective") {
		return
	}
	asOf, err := parseContinuityAsOf(r.URL.Query().Get("as_of"))
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "invalid_objective_graph_query", "detail": err.Error()})
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	if project == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "invalid_objective_graph_query", "detail": "project is required"})
		return
	}
	includeTransitions := true
	if raw := strings.TrimSpace(r.URL.Query().Get("include_transitions")); raw != "" {
		includeTransitions = anyToBool(raw)
	}
	response := s.continuity.objectiveGraph(
		project, strings.TrimSpace(r.URL.Query().Get("objective_id")),
		asOf, includeTransitions, parseOptionalIntQuery(r.URL.Query().Get("limit"), 500, 1, 5000),
	)
	writeJSON(w, http.StatusOK, attachPayloadFormatContract(objectiveGraphContractID, response, "", "objective_graph", r.URL.Path))
}

func (s *server) memoryDecisionChanges(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	if s.continuity == nil || !s.continuity.enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "continuity_ledger_unavailable", "status": s.continuity.snapshot()})
		return
	}
	payload := map[string]any{}
	if r.Method == http.MethodPost {
		var err error
		payload, err = readOptionalJSONBody(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
			return
		}
	}
	if !s.enforceOptionalFrontierT1ProjectBoundary(w, r, "decision") {
		return
	}
	switch r.Method {
	case http.MethodPost:
		if strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["idempotency_key"]), anyToString(payload["idempotencyKey"]))) == "" {
			if headerKey := strings.TrimSpace(r.Header.Get("Idempotency-Key")); headerKey != "" {
				payload["idempotency_key"] = headerKey
			}
		}
		if strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["idempotency_key"]), anyToString(payload["decision_change_id"]))) == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "decision_change_idempotency_required", "detail": "provide idempotency_key, Idempotency-Key, or decision_change_id"})
			return
		}
		change, transition, err := s.continuity.recordDecisionChange(payload)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "invalid_decision_change", "detail": err.Error()})
			return
		}
		response := map[string]any{
			"ok": true, "schema_id": decisionChangeContractID, "decision_change": change,
			"objective_transition": transition, "recorded": !change.idempotentReplay,
			"idempotent_replay": change.idempotentReplay, "ledger_status": s.continuity.snapshot(),
		}
		writeJSON(w, http.StatusOK, attachPayloadFormatContract(decisionChangeContractID, response, anyToString(payload["agent_id"]), "decision_change", r.URL.Path))
	case http.MethodGet:
		project := strings.TrimSpace(r.URL.Query().Get("project"))
		if project == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "invalid_decision_change_query", "detail": "project is required"})
			return
		}
		asOf, err := parseContinuityAsOf(r.URL.Query().Get("as_of"))
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "invalid_decision_change_query", "detail": err.Error()})
			return
		}
		limit := parseOptionalIntQuery(r.URL.Query().Get("limit"), 50, 1, 500)
		query, err := s.continuity.queryDecisionChangesPage(
			project, strings.TrimSpace(r.URL.Query().Get("objective_id")), asOf, limit, r.URL.Query().Get("cursor"),
		)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "invalid_decision_change_query", "detail": err.Error()})
			return
		}
		omitted := maxInt(query.MatchedCount-len(query.Rows), 0)
		complete := query.MatchedCountExact && omitted == 0
		response := map[string]any{
			"ok": true, "schema_id": decisionChangeQueryContractID,
			"project": project, "objective_id": strings.TrimSpace(r.URL.Query().Get("objective_id")),
			"as_of": query.AsOf.Format(time.RFC3339Nano), "limit": limit, "changes": query.Rows,
			"change_count": len(query.Rows), "total_change_count": query.MatchedCount,
			"total_count_exact": query.MatchedCountExact, "omitted_count": omitted,
			"complete": complete, "query_truncated": !complete, "boundary_compacted": false,
			"next_cursor": query.NextCursor, "inspection_count": query.InspectionCount, "inspection_limit": query.InspectionLimit,
			"ledger_status": s.continuity.snapshot(),
		}
		writeJSON(w, http.StatusOK, attachPayloadFormatContract(decisionChangeQueryContractID, response, "", "decision_change_query", r.URL.Path))
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

func objectiveTransitionMap(transition objectiveTransition) map[string]any {
	raw, _ := json.Marshal(transition)
	row := map[string]any{}
	_ = json.Unmarshal(raw, &row)
	return row
}
