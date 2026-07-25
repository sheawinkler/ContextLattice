package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	skillUsageReceiptContractID   = "skill_usage_receipt.v1"
	skillEfficacyReviewContractID = "skill_efficacy_review.v1"

	skillUsageStageSearched        = "searched"
	skillUsageStageSelected        = "selected"
	skillUsageStageInvoked         = "invoked"
	skillUsageStageVerifiedOutcome = "verified_outcome"

	skillEfficacyMinBaselineUses    = 3
	skillEfficacyMinHoldoutUses     = 3
	skillEfficacyMinRetireUses      = 6
	skillEfficacyMaxUsageIDs        = 24
	skillEfficacyMaxMatchedTerms    = 16
	skillEfficacyNoteMaxLines       = 8
	skillEfficacyNoteMaxTokens      = 160
	skillEfficacyRevisionMaxLines   = 40
	skillEfficacyRevisionMaxTokens  = 800
	skillEfficacyMaxDeltaBytes      = 8 * 1024
	skillEfficacyMaxRegressionRatio = 0.20
	skillEfficacyMinNovelLineRatio  = 0.50
)

var skillUsageStageRank = map[string]int{
	skillUsageStageSearched:        1,
	skillUsageStageSelected:        2,
	skillUsageStageInvoked:         3,
	skillUsageStageVerifiedOutcome: 4,
}

var skillUsageSources = map[string]struct{}{
	"local":       {},
	"system":      {},
	"third_party": {},
	"quarantined": {},
}

var skillSelectionReasons = map[string]struct{}{
	"top_match":      {},
	"explicit_user":  {},
	"agent_judgment": {},
	"policy":         {},
}

var skillInvocationModes = map[string]struct{}{
	"read":      {},
	"reference": {},
	"tool":      {},
	"workflow":  {},
}

var skillReviewKinds = map[string]struct{}{
	"none":       {},
	"note":       {},
	"revision":   {},
	"retirement": {},
}

type skillEfficacyGroupMetrics struct {
	Count        int
	Successes    int
	FirstPass    int
	Repairs      int64
	Retries      int64
	Corrections  int64
	LatencyMS    int64
	CostMicrousd int64
	ToolCalls    int64
	Failures     int64
	UtilityTotal float64
	UtilityUnit  string
	TaskClass    string
	OutcomeIDs   map[string]struct{}
	SessionIDs   map[string]struct{}
	UtilityRows  []map[string]any
}

func (s *skillFoundryStore) trimSkillEfficacyLocked() {
	if s == nil {
		return
	}
	trim := func(rows map[string]map[string]any, timeField string) {
		if len(rows) <= s.maxEntries {
			return
		}
		type rowAge struct {
			id string
			at string
		}
		ages := make([]rowAge, 0, len(rows))
		for id, row := range rows {
			ages = append(ages, rowAge{id: id, at: anyToString(row[timeField])})
		}
		sort.Slice(ages, func(i, j int) bool {
			if ages[i].at == ages[j].at {
				return ages[i].id > ages[j].id
			}
			return ages[i].at > ages[j].at
		})
		for _, item := range ages[s.maxEntries:] {
			delete(rows, item.id)
		}
	}
	trim(s.usageReceipts, "recorded_at")
	trim(s.efficacyReviews, "created_at")
}

func (s *skillFoundryStore) usageReceipt(usageID string) map[string]any {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneMap(s.usageReceipts[strings.TrimSpace(usageID)])
}

func (s *skillFoundryStore) efficacyReview(reviewID string) map[string]any {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneMap(s.efficacyReviews[strings.TrimSpace(reviewID)])
}

func (s *skillFoundryStore) usageReceiptsForSkill(project, skillID string) []map[string]any {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows := make([]map[string]any, 0, len(s.usageReceipts))
	for _, row := range s.usageReceipts {
		if !strings.EqualFold(anyToString(row["project"]), project) {
			continue
		}
		if !strings.EqualFold(anyToString(anyMap(row["skill"])["id"]), skillID) {
			continue
		}
		rows = append(rows, cloneMap(row))
	}
	sort.Slice(rows, func(i, j int) bool {
		return anyToString(rows[i]["recorded_at"]) < anyToString(rows[j]["recorded_at"])
	})
	return rows
}

func skillEfficacyDigest(row map[string]any, field string) string {
	copyRow := cloneMap(row)
	delete(copyRow, field)
	raw, err := json.Marshal(copyRow)
	if err != nil {
		return ""
	}
	return "sha256:" + sha256Hex(string(raw))
}

func skillEfficacyRequiredIdentifier(raw any, field string) (string, error) {
	value, err := frontierT8Identifier(raw, field)
	if err != nil {
		return "", err
	}
	return value, nil
}

func skillEfficacyRequiredDigest(raw any, field string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(anyToString(raw)))
	if !frontierT8SHA256Pattern.MatchString(value) {
		return "", fmt.Errorf("%s must be a sha256 digest", field)
	}
	return value, nil
}

func skillEfficacyStringList(raw any, field string, limit int, itemBytes int) ([]string, error) {
	if raw != nil {
		switch raw.(type) {
		case []any, []string:
		default:
			return nil, fmt.Errorf("%s must be an array of strings", field)
		}
	}
	items := contextPackAnyList(raw)
	if len(items) > limit {
		return nil, fmt.Errorf("%s exceeds %d items", field, limit)
	}
	out := make([]string, 0, len(items))
	seen := map[string]struct{}{}
	for index, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("%s[%d] must be a string", field, index)
		}
		value := strings.Join(strings.Fields(text), " ")
		if value == "" {
			return nil, fmt.Errorf("%s[%d] is required", field, index)
		}
		if len(value) > itemBytes {
			return nil, fmt.Errorf("%s[%d] exceeds %d bytes", field, index, itemBytes)
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("%s contains duplicate values", field)
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out, nil
}

func skillEfficacyUsageIDList(raw any, field string, minimum int) ([]string, error) {
	values, err := skillEfficacyStringList(raw, field, skillEfficacyMaxUsageIDs, 160)
	if err != nil {
		return nil, err
	}
	if len(values) < minimum {
		return nil, fmt.Errorf("%s requires at least %d usage identities", field, minimum)
	}
	for _, value := range values {
		if _, err := skillEfficacyRequiredIdentifier(value, field); err != nil {
			return nil, err
		}
	}
	return values, nil
}

func skillEfficacySameIdentity(input, existing map[string]any) error {
	for _, field := range []string{"project", "session_id", "agent_id"} {
		if value := strings.TrimSpace(anyToString(input[field])); value != "" && !strings.EqualFold(value, anyToString(existing[field])) {
			return fmt.Errorf("%s conflicts with the established usage identity", field)
		}
	}
	if rawSkill := anyMap(input["skill"]); len(rawSkill) > 0 {
		existingSkill := anyMap(existing["skill"])
		for _, field := range []string{"id", "name", "version", "digest", "source_kind", "source_ref"} {
			if value := strings.TrimSpace(anyToString(rawSkill[field])); value != "" && !strings.EqualFold(value, anyToString(existingSkill[field])) {
				return fmt.Errorf("skill.%s conflicts with the established usage identity", field)
			}
		}
	}
	return nil
}

func skillEfficacyNormalizeSkill(raw any) (map[string]any, error) {
	skill := anyMap(raw)
	if err := frontierT8RejectUnknownFields(skill, "skill", "id", "name", "version", "digest", "source_kind", "source_ref"); err != nil {
		return nil, err
	}
	id, err := skillEfficacyRequiredIdentifier(skill["id"], "skill.id")
	if err != nil {
		return nil, err
	}
	name := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(anyToString(skill["name"])), "_", "-"))
	if !skillFoundryNamePattern.MatchString(name) {
		return nil, errors.New("skill.name must be 2-64 lowercase letters, digits, or hyphens")
	}
	version := clipText(strings.Join(strings.Fields(anyToString(skill["version"])), " "), 80)
	if version == "" {
		return nil, errors.New("skill.version is required")
	}
	digest, err := skillEfficacyRequiredDigest(skill["digest"], "skill.digest")
	if err != nil {
		return nil, err
	}
	sourceKind := strings.ToLower(strings.TrimSpace(anyToString(skill["source_kind"])))
	if _, ok := skillUsageSources[sourceKind]; !ok {
		return nil, errors.New("skill.source_kind must be local, system, third_party, or quarantined")
	}
	sourceRef, err := frontierT8BoundedText(skill["source_ref"], "skill.source_ref", 240, true)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id": id, "name": name, "version": version, "digest": digest,
		"source_kind": sourceKind, "source_ref": sourceRef,
	}, nil
}

func skillEfficacyNormalizeSearch(raw any) (map[string]any, error) {
	search := anyMap(raw)
	if err := frontierT8RejectUnknownFields(search, "search", "query_digest", "rank", "matched_terms"); err != nil {
		return nil, err
	}
	queryDigest, err := skillEfficacyRequiredDigest(search["query_digest"], "search.query_digest")
	if err != nil {
		return nil, err
	}
	rank := anyToInt(search["rank"], 0)
	if rank < 1 || rank > 1000 {
		return nil, errors.New("search.rank must be between 1 and 1000")
	}
	matched, err := skillEfficacyStringList(search["matched_terms"], "search.matched_terms", skillEfficacyMaxMatchedTerms, 80)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"query_digest": queryDigest, "rank": rank, "matched_terms": stringSliceAny(matched),
		"raw_query_stored": false, "efficacy_credit": false,
	}, nil
}

func skillEfficacySessionOutcome(events []map[string]any, outcomeID string) map[string]any {
	result := map[string]any{
		"found": false, "corrections": 0, "repairs": 0,
	}
	for _, event := range events {
		eventType := strings.ToLower(strings.TrimSpace(anyToString(event["type"])))
		if strings.Contains(eventType, "correction") {
			result["corrections"] = anyToInt(result["corrections"], 0) + 1
		}
		if strings.Contains(eventType, "repair") {
			result["repairs"] = anyToInt(result["repairs"], 0) + 1
		}
		if eventType != "context_pack.outcome_reported" {
			continue
		}
		metadata := anyMap(event["metadata"])
		outcome := anyMap(firstNonEmptyAny(metadata["outcome"], anyMap(metadata["context_pack"])["outcome"]))
		if anyToString(outcome["outcome_id"]) != outcomeID {
			continue
		}
		result["found"] = true
		for _, field := range []string{"first_pass_success", "repair_required", "retry_count"} {
			if value, present := firstPresentValue(outcome[field]); present {
				result[field] = value
			}
		}
	}
	return result
}

func (s *server) skillEfficacyOutcomeAttribution(existing, rawOutcome map[string]any) (map[string]any, error) {
	if err := frontierT8RejectUnknownFields(rawOutcome, "outcome", "outcome_id"); err != nil {
		return nil, err
	}
	outcomeID, err := skillEfficacyRequiredIdentifier(rawOutcome["outcome_id"], "outcome.outcome_id")
	if err != nil {
		return nil, err
	}
	if s == nil || s.utility == nil || s.agentSessions == nil {
		return nil, errors.New("Utility Ledger and agent-session proof are required")
	}
	observation, exists := s.utility.observation(outcomeID)
	if !exists || len(observation) == 0 {
		return nil, errors.New("outcome was not found in the Utility Ledger")
	}
	for _, field := range []string{"project", "session_id", "agent_id"} {
		if !strings.EqualFold(anyToString(observation[field]), anyToString(existing[field])) {
			return nil, fmt.Errorf("outcome %s does not match the usage receipt", field)
		}
	}
	utility := anyMap(observation["utility"])
	if !anyToBool(utility["independently_verified"]) || anyToString(utility["verification_status"]) != "verified" {
		return nil, errors.New("outcome is not independently verified")
	}
	session, events, ok := s.agentSessions.get(anyToString(existing["session_id"]))
	if !ok || len(session) == 0 {
		return nil, errors.New("usage session was not found")
	}
	if !strings.EqualFold(anyToString(session["project"]), anyToString(existing["project"])) {
		return nil, errors.New("usage session project does not match")
	}
	if !strings.EqualFold(anyToString(session["agent_id"]), anyToString(existing["agent_id"])) {
		return nil, errors.New("usage session agent does not match")
	}
	sessionOutcome := skillEfficacySessionOutcome(events, outcomeID)
	if !anyToBool(sessionOutcome["found"]) {
		return nil, errors.New("usage session lacks a matching context_pack.outcome_reported event")
	}
	economics := anyMap(observation["economics"])
	success := anyToFloat(utility["value"]) > 0 && anyToInt(economics["failures"], 0) == 0
	successBasis := "verified_utility_and_zero_failures"
	if value, present := firstPresentValue(sessionOutcome["first_pass_success"]); present {
		success = anyToBool(value)
		successBasis = "session_outcome_report_bound_to_verified_utility"
	}
	retries := maxInt(0, anyToInt(sessionOutcome["retry_count"], 0))
	repairs := maxInt(anyToInt(sessionOutcome["repairs"], 0), boolInt(anyToBool(sessionOutcome["repair_required"])))
	return map[string]any{
		"outcome_id": outcomeID, "observation_id": observation["observation_id"],
		"observation_digest": observation["observation_digest"], "captured_at": observation["captured_at"],
		"task_class": observation["task_class"], "success": success, "success_basis": successBasis,
		"first_pass_success": firstPresentAny(sessionOutcome["first_pass_success"], false),
		"repair_required":    anyToBool(sessionOutcome["repair_required"]), "repairs": repairs,
		"retries": retries, "corrections": maxInt(0, anyToInt(sessionOutcome["corrections"], 0)),
		"latency_ms":    maxInt(0, anyToInt(economics["latency_ms"], 0)),
		"cost_microusd": maxInt(0, anyToInt(economics["cost_microusd"], 0)),
		"tool_calls":    maxInt(0, anyToInt(economics["tool_calls"], 0)),
		"failures":      maxInt(0, anyToInt(economics["failures"], 0)),
		"utility": map[string]any{
			"value": utility["value"], "unit": utility["unit"],
			"verification_status":    utility["verification_status"],
			"independently_verified": utility["independently_verified"],
			"verification_event_id":  utility["verification_event_id"],
			"evidence_digest":        utility["evidence_digest"],
			"verifier_kind":          utility["verifier_kind"], "verifier_id": utility["verifier_id"],
		},
		"eligibility":         cloneMap(anyMap(observation["eligibility"])),
		"pairing":             cloneMap(anyMap(observation["pairing"])),
		"denominator":         cloneMap(anyMap(observation["denominator"])),
		"source_claim_digest": observation["source_claim_digest"],
	}, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func skillEfficacyReceiptPosture(stage string) (string, bool) {
	switch stage {
	case skillUsageStageSearched:
		return "discoverability_only", false
	case skillUsageStageSelected:
		return "selection_only", false
	case skillUsageStageInvoked:
		return "invocation_unverified", false
	case skillUsageStageVerifiedOutcome:
		return "verified_efficacy", true
	default:
		return "invalid", false
	}
}

func (s *server) buildSkillUsageReceipt(input, existing map[string]any, now time.Time) (map[string]any, string, error) {
	if err := frontierT8RejectUnsafeValue(input, "input", 0); err != nil {
		return nil, "", err
	}
	if err := frontierT8RejectUnknownFields(input, "input",
		"project", "usage_id", "idempotency_key", "stage", "expected_previous_receipt_digest",
		"session_id", "agent_id", "skill", "search", "selection", "invocation", "outcome"); err != nil {
		return nil, "", err
	}
	usageID, err := skillEfficacyRequiredIdentifier(input["usage_id"], "usage_id")
	if err != nil {
		return nil, "", err
	}
	idempotencyKey, err := skillEfficacyRequiredIdentifier(input["idempotency_key"], "idempotency_key")
	if err != nil {
		return nil, "", err
	}
	stage := strings.ToLower(strings.TrimSpace(anyToString(input["stage"])))
	rank, validStage := skillUsageStageRank[stage]
	if !validStage {
		return nil, "", errors.New("stage must be searched, selected, invoked, or verified_outcome")
	}
	if len(existing) == 0 {
		if stage != skillUsageStageSearched {
			return nil, "", errors.New("a usage chain must begin at searched")
		}
		project, err := sanitizeMemoryProject(anyToString(input["project"]))
		if err != nil {
			return nil, "", err
		}
		sessionID, err := skillEfficacyRequiredIdentifier(input["session_id"], "session_id")
		if err != nil {
			return nil, "", err
		}
		agentID, err := skillEfficacyRequiredIdentifier(input["agent_id"], "agent_id")
		if err != nil {
			return nil, "", err
		}
		skill, err := skillEfficacyNormalizeSkill(input["skill"])
		if err != nil {
			return nil, "", err
		}
		search, err := skillEfficacyNormalizeSearch(input["search"])
		if err != nil {
			return nil, "", err
		}
		currentSkill, indexSource, found := skillEfficacyCurrentSkill(anyToString(skill["name"]))
		if !found || "sha256:"+sha256Hex(currentSkill) != anyToString(skill["digest"]) {
			return nil, "", errors.New("searched skill does not match the current native Skills Index")
		}
		search["skill_resolved"] = true
		search["index_source"] = indexSource
		search["rank_authority"] = "agent_search_receipt"
		existing = map[string]any{
			"schema_id": skillUsageReceiptContractID, "version": 1,
			"usage_id": usageID, "project": project, "session_id": sessionID, "agent_id": agentID,
			"skill": skill, "search": search, "stage_events": []any{},
		}
	} else {
		if anyToString(existing["usage_id"]) != usageID {
			return nil, "", errors.New("usage identity mismatch")
		}
		if err := skillEfficacySameIdentity(input, existing); err != nil {
			return nil, "", err
		}
		currentRank := skillUsageStageRank[anyToString(existing["stage"])]
		if rank != currentRank+1 {
			return nil, "", errors.New("usage receipt stages cannot be skipped, repeated, or reversed")
		}
		expected, err := skillEfficacyRequiredDigest(input["expected_previous_receipt_digest"], "expected_previous_receipt_digest")
		if err != nil {
			return nil, "", err
		}
		if expected != anyToString(existing["receipt_digest"]) {
			return nil, "", errors.New("expected_previous_receipt_digest is stale")
		}
	}

	receipt := cloneMap(existing)
	receipt["schema_id"] = skillUsageReceiptContractID
	receipt["version"] = 1
	receipt["revision"] = rank
	receipt["stage"] = stage
	receipt["recorded_at"] = now.UTC().Format(time.RFC3339Nano)
	receipt["previous_receipt_digest"] = "genesis"
	if rank > 1 {
		receipt["previous_receipt_digest"] = existing["receipt_digest"]
	}
	eventID := "skillevt_" + sha256Hex(strings.Join([]string{usageID, stage, idempotencyKey}, "\x00"))[:24]
	events := contextPackAnyList(receipt["stage_events"])
	events = append(events, map[string]any{"stage": stage, "event_id": eventID, "recorded_at": receipt["recorded_at"]})
	receipt["stage_events"] = events

	switch stage {
	case skillUsageStageSearched:
		if len(anyMap(input["selection"])) > 0 || len(anyMap(input["invocation"])) > 0 || len(anyMap(input["outcome"])) > 0 {
			return nil, "", errors.New("searched accepts search material only")
		}
	case skillUsageStageSelected:
		if len(anyMap(input["search"])) > 0 || len(anyMap(input["invocation"])) > 0 || len(anyMap(input["outcome"])) > 0 {
			return nil, "", errors.New("selected accepts selection material only")
		}
		selection := anyMap(input["selection"])
		if err := frontierT8RejectUnknownFields(selection, "selection", "reason_code"); err != nil {
			return nil, "", err
		}
		reason := strings.ToLower(strings.TrimSpace(anyToString(selection["reason_code"])))
		if _, ok := skillSelectionReasons[reason]; !ok {
			return nil, "", errors.New("selection.reason_code is invalid")
		}
		receipt["selection"] = map[string]any{"reason_code": reason}
	case skillUsageStageInvoked:
		if len(anyMap(input["search"])) > 0 || len(anyMap(input["selection"])) > 0 || len(anyMap(input["outcome"])) > 0 {
			return nil, "", errors.New("invoked accepts invocation material only")
		}
		invocation := anyMap(input["invocation"])
		if err := frontierT8RejectUnknownFields(invocation, "invocation", "mode"); err != nil {
			return nil, "", err
		}
		mode := strings.ToLower(strings.TrimSpace(anyToString(invocation["mode"])))
		if _, ok := skillInvocationModes[mode]; !ok {
			return nil, "", errors.New("invocation.mode must be read, reference, tool, or workflow")
		}
		receipt["invocation"] = map[string]any{"mode": mode, "skill_digest": anyMap(receipt["skill"])["digest"]}
	case skillUsageStageVerifiedOutcome:
		if len(anyMap(input["search"])) > 0 || len(anyMap(input["selection"])) > 0 || len(anyMap(input["invocation"])) > 0 {
			return nil, "", errors.New("verified_outcome accepts outcome material only")
		}
		attribution, err := s.skillEfficacyOutcomeAttribution(existing, anyMap(input["outcome"]))
		if err != nil {
			return nil, "", err
		}
		receipt["outcome_attribution"] = attribution
	}
	posture, eligible := skillEfficacyReceiptPosture(stage)
	receipt["evidence_class"] = posture
	receipt["discoverability_evidence"] = true
	receipt["efficacy_eligible"] = eligible
	receipt["active_skill_mutated"] = false
	receipt["receipt_digest"] = skillEfficacyDigest(receipt, "receipt_digest")
	if !frontierT8SHA256Pattern.MatchString(anyToString(receipt["receipt_digest"])) {
		return nil, "", errors.New("usage receipt digest failed")
	}
	return receipt, idempotencyKey, nil
}

func skillEfficacyReceiptSet(store *skillFoundryStore, project, skillID string, usageIDs []string, requireEligible bool) ([]map[string]any, error) {
	rows := make([]map[string]any, 0, len(usageIDs))
	for _, usageID := range usageIDs {
		row := store.usageReceipt(usageID)
		if len(row) == 0 {
			return nil, fmt.Errorf("usage receipt %q was not found", usageID)
		}
		if !strings.EqualFold(anyToString(row["project"]), project) || !strings.EqualFold(anyToString(anyMap(row["skill"])["id"]), skillID) {
			return nil, fmt.Errorf("usage receipt %q is outside the requested skill scope", usageID)
		}
		if requireEligible && (anyToString(row["stage"]) != skillUsageStageVerifiedOutcome || !anyToBool(row["efficacy_eligible"])) {
			return nil, fmt.Errorf("usage receipt %q is not efficacy eligible", usageID)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func skillEfficacyEligibleRows(rows []map[string]any) []map[string]any {
	eligible := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if anyToString(row["stage"]) == skillUsageStageVerifiedOutcome && anyToBool(row["efficacy_eligible"]) {
			eligible = append(eligible, row)
		}
	}
	return eligible
}

func skillEfficacyUsageIDs(rows []map[string]any) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		if usageID := anyToString(row["usage_id"]); usageID != "" {
			ids = append(ids, usageID)
		}
	}
	return ids
}

func skillEfficacyGroup(receipts []map[string]any, utility *utilityTelemetry) (skillEfficacyGroupMetrics, error) {
	result := skillEfficacyGroupMetrics{
		OutcomeIDs: map[string]struct{}{}, SessionIDs: map[string]struct{}{},
		UtilityRows: make([]map[string]any, 0, len(receipts)),
	}
	for _, receipt := range receipts {
		attribution := anyMap(receipt["outcome_attribution"])
		utilityClaim := anyMap(attribution["utility"])
		outcomeID := anyToString(attribution["outcome_id"])
		if outcomeID == "" {
			return result, errors.New("efficacy receipt lacks an outcome identity")
		}
		if _, duplicate := result.OutcomeIDs[outcomeID]; duplicate {
			return result, errors.New("efficacy groups cannot reuse an outcome")
		}
		observation, exists := utility.observation(outcomeID)
		if !exists || len(observation) == 0 {
			return result, fmt.Errorf("utility outcome %q is unavailable", outcomeID)
		}
		if anyToString(observation["observation_digest"]) != anyToString(attribution["observation_digest"]) {
			return result, fmt.Errorf("utility outcome %q changed after attribution", outcomeID)
		}
		unit := anyToString(utilityClaim["unit"])
		taskClass := anyToString(attribution["task_class"])
		if result.Count > 0 && (result.UtilityUnit != unit || result.TaskClass != taskClass) {
			return result, errors.New("efficacy groups require one utility unit and task class")
		}
		result.UtilityUnit = unit
		result.TaskClass = taskClass
		result.Count++
		if anyToBool(attribution["success"]) {
			result.Successes++
		}
		if anyToBool(attribution["first_pass_success"]) {
			result.FirstPass++
		}
		result.Repairs += int64(anyToInt(attribution["repairs"], 0))
		result.Retries += int64(anyToInt(attribution["retries"], 0))
		result.Corrections += int64(anyToInt(attribution["corrections"], 0))
		result.LatencyMS += int64(anyToInt(attribution["latency_ms"], 0))
		result.CostMicrousd += int64(anyToInt(attribution["cost_microusd"], 0))
		result.ToolCalls += int64(anyToInt(attribution["tool_calls"], 0))
		result.Failures += int64(anyToInt(attribution["failures"], 0))
		result.UtilityTotal += anyToFloat(utilityClaim["value"])
		result.OutcomeIDs[outcomeID] = struct{}{}
		result.SessionIDs[anyToString(receipt["session_id"])] = struct{}{}
		result.UtilityRows = append(result.UtilityRows, observation)
	}
	return result, nil
}

func skillEfficacyMean(total float64, count int) float64 {
	if count <= 0 {
		return 0
	}
	return utilityRound(total/float64(count), 6)
}

func skillEfficacyIntMean(total int64, count int) float64 {
	return skillEfficacyMean(float64(total), count)
}

func skillEfficacyGroupPayload(group skillEfficacyGroupMetrics) map[string]any {
	return map[string]any{
		"count": group.Count, "successes": group.Successes,
		"success_rate":         skillEfficacyMean(float64(group.Successes), group.Count),
		"first_pass_successes": group.FirstPass,
		"first_pass_rate":      skillEfficacyMean(float64(group.FirstPass), group.Count),
		"repairs":              group.Repairs, "average_repairs": skillEfficacyIntMean(group.Repairs, group.Count),
		"retries": group.Retries, "average_retries": skillEfficacyIntMean(group.Retries, group.Count),
		"corrections": group.Corrections, "average_corrections": skillEfficacyIntMean(group.Corrections, group.Count),
		"latency_ms": group.LatencyMS, "average_latency_ms": skillEfficacyIntMean(group.LatencyMS, group.Count),
		"cost_microusd": group.CostMicrousd, "average_cost_microusd": skillEfficacyIntMean(group.CostMicrousd, group.Count),
		"tool_calls": group.ToolCalls, "average_tool_calls": skillEfficacyIntMean(group.ToolCalls, group.Count),
		"failures": group.Failures, "average_failures": skillEfficacyIntMean(group.Failures, group.Count),
		"utility_total":   utilityRound(group.UtilityTotal, 6),
		"average_utility": skillEfficacyMean(group.UtilityTotal, group.Count),
		"utility_unit":    group.UtilityUnit, "task_class": group.TaskClass,
	}
}

func skillEfficacyDisjoint(left, right map[string]struct{}) bool {
	for key := range left {
		if _, exists := right[key]; exists {
			return false
		}
	}
	return true
}

func skillEfficacyRegressionAllowed(baselineTotal, holdoutTotal int64, baselineCount, holdoutCount int) bool {
	if holdoutCount <= 0 || baselineCount <= 0 {
		return false
	}
	baselineMean := float64(baselineTotal) / float64(baselineCount)
	holdoutMean := float64(holdoutTotal) / float64(holdoutCount)
	if baselineMean == 0 {
		return holdoutMean == 0
	}
	return holdoutMean <= baselineMean*(1+skillEfficacyMaxRegressionRatio)
}

func skillEfficacyNoRegression(baseline, holdout skillEfficacyGroupMetrics) bool {
	if baseline.Count <= 0 || holdout.Count <= 0 {
		return false
	}
	if skillEfficacyIntMean(holdout.Failures, holdout.Count) > skillEfficacyIntMean(baseline.Failures, baseline.Count) ||
		skillEfficacyIntMean(holdout.Retries, holdout.Count) > skillEfficacyIntMean(baseline.Retries, baseline.Count) ||
		skillEfficacyIntMean(holdout.Corrections, holdout.Count) > skillEfficacyIntMean(baseline.Corrections, baseline.Count) {
		return false
	}
	if skillEfficacyMean(float64(holdout.FirstPass), holdout.Count) < skillEfficacyMean(float64(baseline.FirstPass), baseline.Count) {
		return false
	}
	if !skillEfficacyRegressionAllowed(baseline.LatencyMS, holdout.LatencyMS, baseline.Count, holdout.Count) {
		return false
	}
	if !skillEfficacyRegressionAllowed(baseline.CostMicrousd, holdout.CostMicrousd, baseline.Count, holdout.Count) {
		return false
	}
	return true
}

func skillEfficacyNormalizedLines(value string) []string {
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		normalized := strings.ToLower(strings.Join(strings.Fields(line), " "))
		if normalized != "" {
			out = append(out, normalized)
		}
	}
	return out
}

func skillEfficacyApproxTokens(value string) int {
	runes := utf8.RuneCountInString(value)
	if runes == 0 {
		return 0
	}
	return (runes + 3) / 4
}

func skillEfficacyCurrentSkill(name string) (string, string, bool) {
	payload := nativeSkillsIndexSearch(skillsQuarantineSearchRequest{Query: name, Limit: 100, JSON: true})
	for _, raw := range contextPackAnyList(payload["results"]) {
		row := anyMap(raw)
		if !strings.EqualFold(anyToString(row["name"]), name) {
			continue
		}
		path := strings.TrimSpace(anyToString(row["path"]))
		if path == "" {
			continue
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 512*1024 {
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		openedInfo, statErr := file.Stat()
		if statErr != nil || !os.SameFile(info, openedInfo) || openedInfo.Mode()&os.ModeSymlink != 0 || !openedInfo.Mode().IsRegular() {
			_ = file.Close()
			continue
		}
		rawSkill, readErr := io.ReadAll(io.LimitReader(file, 512*1024+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(rawSkill) > 512*1024 {
			continue
		}
		return string(rawSkill), anyToString(row["source"]), true
	}
	return "", "", false
}

func skillEfficacyNovelty(delta, current string) (float64, int, int) {
	deltaLines := skillEfficacyNormalizedLines(delta)
	currentLines := skillEfficacyNormalizedLines(current)
	currentSet := map[string]struct{}{}
	for _, line := range currentLines {
		currentSet[line] = struct{}{}
	}
	novel := 0
	for _, line := range deltaLines {
		if _, exists := currentSet[line]; !exists {
			novel++
		}
	}
	if len(deltaLines) == 0 {
		return 0, 0, 0
	}
	return utilityRound(float64(novel)/float64(len(deltaLines)), 6), novel, len(deltaLines)
}

func skillEfficacyProposal(raw any, skill map[string]any) (map[string]any, map[string]bool, error) {
	proposal := anyMap(raw)
	if err := frontierT8RejectUnknownFields(proposal, "proposal", "kind", "summary", "bounded_delta", "delivery"); err != nil {
		return nil, nil, err
	}
	kindValue, ok := proposal["kind"].(string)
	if !ok {
		return nil, nil, errors.New("proposal.kind must be a string")
	}
	kind := strings.ToLower(strings.TrimSpace(kindValue))
	if _, ok := skillReviewKinds[kind]; !ok {
		return nil, nil, errors.New("proposal.kind must be none, note, revision, or retirement")
	}
	summary, err := frontierT8BoundedText(proposal["summary"], "proposal.summary", 500, false)
	if err != nil {
		return nil, nil, err
	}
	delta := ""
	if rawDelta, present := proposal["bounded_delta"]; present && rawDelta != nil {
		text, stringValue := rawDelta.(string)
		if !stringValue {
			return nil, nil, errors.New("proposal.bounded_delta must be a string")
		}
		delta = strings.TrimSpace(text)
	}
	deliveryValue, ok := proposal["delivery"].(string)
	if !ok {
		return nil, nil, errors.New("proposal.delivery must be a string")
	}
	delivery := strings.ToLower(strings.TrimSpace(deliveryValue))
	gates := map[string]bool{"novel": true, "budget": true, "source_policy": true, "source_current": true, "secret_free": true}
	current, indexSource, found := skillEfficacyCurrentSkill(anyToString(skill["name"]))
	currentDigest := ""
	if found {
		currentDigest = "sha256:" + sha256Hex(current)
	}
	if !found || currentDigest != anyToString(skill["digest"]) {
		gates["source_current"] = false
	}
	if kind == "none" {
		if summary != "" || delta != "" || (delivery != "" && delivery != "none") {
			return nil, nil, errors.New("proposal.kind=none cannot carry change material")
		}
		return map[string]any{
			"kind": kind, "summary": "", "bounded_delta": "", "delivery": "none",
			"added_lines": 0, "estimated_tokens": 0, "index_source": indexSource,
			"existing_skill_resolved": found, "current_skill_digest": currentDigest,
		}, gates, nil
	}
	if kind == "retirement" {
		if summary == "" || delta != "" || delivery != "foundry_retirement" {
			return nil, nil, errors.New("retirement requires a summary, no bounded_delta, and delivery=foundry_retirement")
		}
		return map[string]any{
			"kind": kind, "summary": summary, "bounded_delta": "", "delivery": delivery,
			"added_lines": 0, "estimated_tokens": 0, "index_source": indexSource,
			"existing_skill_resolved": found, "current_skill_digest": currentDigest,
		}, gates, nil
	}
	if summary == "" || delta == "" {
		return nil, nil, errors.New("note and revision proposals require summary and bounded_delta")
	}
	if len(delta) > skillEfficacyMaxDeltaBytes {
		gates["budget"] = false
	}
	filter := writeSecretFilterResult{Mode: "block"}
	_ = scrubWriteSecrets(delta, &filter, 0)
	if filter.Findings > 0 {
		gates["secret_free"] = false
	}
	if err := frontierT8RejectUnsafeValue(delta, "proposal.bounded_delta", 0); err != nil {
		return nil, nil, err
	}
	lines := skillEfficacyNormalizedLines(delta)
	tokens := skillEfficacyApproxTokens(delta)
	maxLines, maxTokens := skillEfficacyRevisionMaxLines, skillEfficacyRevisionMaxTokens
	if kind == "note" {
		maxLines, maxTokens = skillEfficacyNoteMaxLines, skillEfficacyNoteMaxTokens
	}
	if len(lines) == 0 || len(lines) > maxLines || tokens > maxTokens {
		gates["budget"] = false
	}
	ratio, novelLines, totalLines := skillEfficacyNovelty(delta, current)
	normalizedCurrent := strings.Join(skillEfficacyNormalizedLines(current), " ")
	normalizedDelta := strings.Join(skillEfficacyNormalizedLines(delta), " ")
	if !found || normalizedDelta == "" || strings.Contains(normalizedCurrent, normalizedDelta) || ratio < skillEfficacyMinNovelLineRatio {
		gates["novel"] = false
	}
	sourceKind := anyToString(skill["source_kind"])
	switch sourceKind {
	case "third_party", "quarantined":
		if delivery != "local_overlay" && delivery != "upstream_pr_candidate" {
			gates["source_policy"] = false
		}
	case "system":
		if delivery != "local_overlay" {
			gates["source_policy"] = false
		}
	case "local":
		if delivery != "foundry_revision" && delivery != "local_overlay" {
			gates["source_policy"] = false
		}
	default:
		gates["source_policy"] = false
	}
	return map[string]any{
		"kind": kind, "summary": summary, "bounded_delta": delta, "delivery": delivery,
		"added_lines": len(lines), "estimated_tokens": tokens,
		"content_digest":   "sha256:" + sha256Hex(delta),
		"novel_line_ratio": ratio, "novel_lines": novelLines, "compared_lines": totalLines,
		"index_source": indexSource, "existing_skill_resolved": found, "current_skill_digest": currentDigest,
		"line_budget": maxLines, "token_budget": maxTokens,
	}, gates, nil
}

func skillEfficacyArtifact(reviewID string, skill, proposal map[string]any) map[string]any {
	delivery := anyToString(proposal["delivery"])
	skillID := anyToString(skill["id"])
	suggested := ""
	switch delivery {
	case "local_overlay":
		suggested = filepath.ToSlash(filepath.Join(".contextlattice", "skill-overlays", skillID, reviewID+".md"))
	case "upstream_pr_candidate":
		suggested = filepath.ToSlash(filepath.Join("upstream-pr-candidates", skillID, reviewID+".md"))
	case "foundry_revision":
		suggested = "skill-foundry:" + reviewID
	case "foundry_retirement":
		suggested = "skill-foundry-retirement:" + reviewID
	}
	return map[string]any{
		"mode": delivery, "state": "inactive", "suggested_target": suggested,
		"bounded_delta": proposal["bounded_delta"], "content_digest": proposal["content_digest"],
		"stored_in_review_ledger": true, "filesystem_write_performed": false,
		"vendor_source_mutated": false, "upstream_request_created": false,
	}
}

func skillEfficacyDiscoverability(rows []map[string]any) map[string]any {
	counts := map[string]int{
		"searched": 0, "selected": 0, "invoked": 0, "verified_outcome": 0,
	}
	for _, row := range rows {
		rank := skillUsageStageRank[anyToString(row["stage"])]
		if rank >= 1 {
			counts["searched"]++
		}
		if rank >= 2 {
			counts["selected"]++
		}
		if rank >= 3 {
			counts["invoked"]++
		}
		if rank >= 4 {
			counts["verified_outcome"]++
		}
	}
	return map[string]any{
		"searched_count": counts["searched"], "selected_count": counts["selected"],
		"invoked_count": counts["invoked"], "verified_outcome_count": counts["verified_outcome"],
		"search_only_count":      counts["searched"] - counts["selected"],
		"search_efficacy_credit": false,
		"measurement_limit":      "Search and ranking evidence measures discoverability only; selection, invocation, and independently verified outcomes are required for efficacy.",
	}
}

func (s *server) buildSkillEfficacyReview(input map[string]any, now time.Time) (map[string]any, string, error) {
	if err := frontierT8RejectUnsafeValue(input, "input", 0); err != nil {
		return nil, "", err
	}
	if err := frontierT8RejectUnknownFields(input, "input",
		"project", "skill_id", "name", "idempotency_key", "baseline_usage_ids", "holdout_usage_ids", "proposal"); err != nil {
		return nil, "", err
	}
	project, err := sanitizeMemoryProject(anyToString(input["project"]))
	if err != nil {
		return nil, "", err
	}
	skillID, err := skillEfficacyRequiredIdentifier(input["skill_id"], "skill_id")
	if err != nil {
		return nil, "", err
	}
	name := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(anyToString(input["name"])), "_", "-"))
	if !skillFoundryNamePattern.MatchString(name) {
		return nil, "", errors.New("name must be 2-64 lowercase letters, digits, or hyphens")
	}
	idempotencyKey, err := skillEfficacyRequiredIdentifier(input["idempotency_key"], "idempotency_key")
	if err != nil {
		return nil, "", err
	}
	if s == nil || s.skillFoundry == nil || !s.skillFoundry.enabled || s.utility == nil {
		return nil, "", errors.New("Skill Foundry and Utility Ledger are required")
	}
	allRows := s.skillFoundry.usageReceiptsForSkill(project, skillID)
	if len(allRows) == 0 {
		return nil, "", errors.New("no usage receipts exist for the requested skill")
	}
	skill := cloneMap(anyMap(allRows[0]["skill"]))
	if !strings.EqualFold(anyToString(skill["name"]), name) {
		return nil, "", errors.New("skill name does not match the usage ledger")
	}
	proposal, proposalGates, err := skillEfficacyProposal(input["proposal"], skill)
	if err != nil {
		return nil, "", err
	}
	kind := anyToString(proposal["kind"])
	baselineEvidenceMinimum := skillEfficacyMinBaselineUses
	baselineInputMinimum := baselineEvidenceMinimum
	requireEligibleBaseline := true
	if kind == "none" {
		baselineInputMinimum = 0
		requireEligibleBaseline = false
	}
	if kind == "retirement" {
		baselineEvidenceMinimum = skillEfficacyMinRetireUses
		baselineInputMinimum = baselineEvidenceMinimum
	}
	baselineIDs, err := skillEfficacyUsageIDList(input["baseline_usage_ids"], "baseline_usage_ids", baselineInputMinimum)
	if err != nil {
		return nil, "", err
	}
	holdoutMinimum := 0
	if kind == "note" || kind == "revision" {
		holdoutMinimum = skillEfficacyMinHoldoutUses
	}
	holdoutIDs, err := skillEfficacyUsageIDList(input["holdout_usage_ids"], "holdout_usage_ids", holdoutMinimum)
	if err != nil {
		return nil, "", err
	}
	if holdoutMinimum == 0 && len(holdoutIDs) > 0 {
		return nil, "", errors.New("holdout_usage_ids are only valid for note or revision proposals")
	}
	idSet := map[string]struct{}{}
	for _, usageID := range baselineIDs {
		idSet[usageID] = struct{}{}
	}
	for _, usageID := range holdoutIDs {
		if _, duplicate := idSet[usageID]; duplicate {
			return nil, "", errors.New("baseline and holdout usage identities must be disjoint")
		}
		idSet[usageID] = struct{}{}
	}
	baselineConsideredRows, err := skillEfficacyReceiptSet(s.skillFoundry, project, skillID, baselineIDs, requireEligibleBaseline)
	if err != nil {
		return nil, "", err
	}
	baselineRows := skillEfficacyEligibleRows(baselineConsideredRows)
	holdoutRows, err := skillEfficacyReceiptSet(s.skillFoundry, project, skillID, holdoutIDs, true)
	if err != nil {
		return nil, "", err
	}
	baseline, err := skillEfficacyGroup(baselineRows, s.utility)
	if err != nil {
		return nil, "", err
	}
	holdout := skillEfficacyGroupMetrics{OutcomeIDs: map[string]struct{}{}, SessionIDs: map[string]struct{}{}}
	if len(holdoutRows) > 0 {
		holdout, err = skillEfficacyGroup(holdoutRows, s.utility)
		if err != nil {
			return nil, "", err
		}
		if baseline.UtilityUnit != holdout.UtilityUnit || baseline.TaskClass != holdout.TaskClass {
			return nil, "", errors.New("baseline and holdout require the same utility unit and task class")
		}
	}
	sessionDisjoint := skillEfficacyDisjoint(baseline.SessionIDs, holdout.SessionIDs)
	if len(holdoutRows) > 0 && !sessionDisjoint {
		return nil, "", errors.New("baseline and holdout sessions must be disjoint")
	}

	combinedUtility := append(append([]map[string]any{}, baseline.UtilityRows...), holdout.UtilityRows...)
	_, pairs, _ := utilityPairProjection(combinedUtility)
	exactPairs := 0
	totalLift := 0.0
	for _, pair := range pairs {
		_, controlOK := baseline.OutcomeIDs[pair.ControlOutcomeID]
		_, treatmentOK := holdout.OutcomeIDs[pair.TreatmentOutcomeID]
		if controlOK && treatmentOK {
			exactPairs++
			totalLift += pair.UtilityGain
		}
	}
	meanLift := skillEfficacyMean(totalLift, exactPairs)
	baselineSessionsUnique := len(baseline.SessionIDs) == baseline.Count
	holdoutSessionsUnique := len(holdout.SessionIDs) == holdout.Count
	repeatedEvidence := baseline.Count >= baselineEvidenceMinimum && baselineSessionsUnique
	holdoutEvidence := holdout.Count >= holdoutMinimum && holdoutSessionsUnique
	exactMatchedLift := holdoutMinimum == 0 || (exactPairs >= skillEfficacyMinHoldoutUses && meanLift > 0)
	noRegression := holdoutMinimum == 0 || skillEfficacyNoRegression(baseline, holdout)
	changeGatesPass := proposalGates["novel"] && proposalGates["budget"] && proposalGates["source_policy"] && proposalGates["source_current"] && proposalGates["secret_free"]

	decision := "abstain"
	switch kind {
	case "none":
		if repeatedEvidence && proposalGates["source_current"] &&
			skillEfficacyMean(float64(baseline.Successes), baseline.Count) >= 0.8 && baseline.UtilityTotal > 0 {
			decision = "retain"
		}
	case "note":
		if repeatedEvidence && holdoutEvidence && sessionDisjoint && exactMatchedLift && noRegression && changeGatesPass {
			decision = "add_bounded_note"
		}
	case "revision":
		if repeatedEvidence && holdoutEvidence && sessionDisjoint && exactMatchedLift && noRegression && changeGatesPass {
			decision = "revision_candidate"
		}
	case "retirement":
		failureSignal := skillEfficacyMean(float64(baseline.Successes), baseline.Count) <= 0.5 &&
			(baseline.Failures > 0 || baseline.Retries > 0 || baseline.Corrections > 0)
		if repeatedEvidence && failureSignal && proposalGates["source_current"] {
			decision = "retirement_candidate"
		}
	}

	seed := map[string]any{
		"project": project, "skill_id": skillID, "skill_digest": skill["digest"],
		"baseline_usage_ids": baselineIDs, "holdout_usage_ids": holdoutIDs,
		"proposal": proposal,
	}
	seedRaw, _ := json.Marshal(seed)
	reviewID := "skillreview_" + sha256Hex(string(seedRaw))[:24]
	gates := map[string]any{
		"repeated_verified_evidence": map[string]any{"passed": repeatedEvidence, "minimum": baselineEvidenceMinimum, "observed": baseline.Count, "sessions_unique": baselineSessionsUnique},
		"independent_holdouts":       map[string]any{"applicable": holdoutMinimum > 0, "passed": holdoutEvidence && sessionDisjoint, "minimum": holdoutMinimum, "observed": holdout.Count, "sessions_unique": holdoutSessionsUnique, "sessions_disjoint": sessionDisjoint},
		"exact_matched_lift":         map[string]any{"applicable": holdoutMinimum > 0, "passed": exactMatchedLift, "minimum_pairs": skillEfficacyMinHoldoutUses, "observed_pairs": exactPairs, "mean_utility_lift": meanLift},
		"no_material_regression":     map[string]any{"applicable": holdoutMinimum > 0, "passed": noRegression, "maximum_latency_or_cost_ratio": skillEfficacyMaxRegressionRatio},
		"novelty":                    map[string]any{"passed": proposalGates["novel"], "minimum_novel_line_ratio": skillEfficacyMinNovelLineRatio},
		"bounded_delta":              map[string]any{"passed": proposalGates["budget"], "note_max_lines": skillEfficacyNoteMaxLines, "note_max_tokens": skillEfficacyNoteMaxTokens, "revision_max_lines": skillEfficacyRevisionMaxLines, "revision_max_tokens": skillEfficacyRevisionMaxTokens},
		"source_policy":              map[string]any{"passed": proposalGates["source_policy"], "source_kind": skill["source_kind"], "delivery": proposal["delivery"]},
		"source_current":             map[string]any{"passed": proposalGates["source_current"], "recorded_digest": skill["digest"], "current_digest": proposal["current_skill_digest"]},
		"secret_free":                map[string]any{"passed": proposalGates["secret_free"]},
	}
	review := map[string]any{
		"schema_id": skillEfficacyReviewContractID, "version": 1,
		"review_id": reviewID, "project": project, "skill_id": skillID, "name": name,
		"skill": skill, "status": "inactive", "decision": decision, "created_at": now.UTC().Format(time.RFC3339Nano),
		"discoverability": skillEfficacyDiscoverability(allRows),
		"attribution": map[string]any{
			"baseline": skillEfficacyGroupPayload(baseline), "holdout": skillEfficacyGroupPayload(holdout),
			"exact_matched_pairs": exactPairs, "mean_utility_lift": meanLift,
			"baseline_usage_ids": stringSliceAny(skillEfficacyUsageIDs(baselineRows)), "holdout_usage_ids": stringSliceAny(skillEfficacyUsageIDs(holdoutRows)),
			"considered_usage_ids": stringSliceAny(baselineIDs),
			"outcome_authority":    "utility_ledger+agent_session_ledger",
		},
		"gates": gates, "proposal": proposal,
		"artifact": skillEfficacyArtifact(reviewID, skill, proposal),
		"governance": map[string]any{
			"human_review_required": decision != "retain" && decision != "abstain",
			"human_approved":        false, "promotion_allowed": false, "automatic_promotion": false,
			"active_skill_mutated": false, "installation_performed": false,
			"next_surface": map[string]any{
				"add_bounded_note":     "Skill Foundry export then explicit Skills Index promotion",
				"revision_candidate":   "Verified Skill Evolution Foundry handoff",
				"retirement_candidate": "protected Skill Foundry retirement review",
			}[decision],
		},
		"safety": map[string]any{
			"advisory_only": true, "provider_calls": 0, "network_calls": 0, "subprocess_calls": 0,
			"filesystem_mutations": 1, "ledger_writes": 1, "active_skill_mutations": 0, "vendor_source_mutations": 0,
			"activation_performed": false, "deactivation_performed": false, "retirement_performed": false,
		},
		"measurement_limit": "The review attributes observed outcomes to recorded skill invocations. Only exact matched controls support a lift decision; search-only evidence never supports efficacy.",
	}
	review["review_digest"] = skillEfficacyDigest(review, "review_digest")
	return review, idempotencyKey, nil
}
