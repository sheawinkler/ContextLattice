package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	policySimulationContractID              = "policy_simulation.v1"
	scopedPolicyCardContractID              = "scoped_policy_card.v1"
	policyPromotionRecommendationContractID = "policy_promotion_recommendation.v1"
	memoryRetirementContractID              = "memory_retirement.v1"
	contradictionResolutionContractID       = "contradiction_resolution.v1"
	storageTemperatureDecisionContractID    = "storage_temperature_decision.v1"
	frontierT5StatusContractID              = "frontier_t5_policy_laboratory_status.v1"

	policySimulationPath              = "/memory/policy/simulate"
	scopedPolicyCardPath              = "/memory/policy/card"
	policyPromotionRecommendationPath = "/memory/policy/promotion"
	memoryRetirementPath              = "/memory/lifecycle/retirement"
	contradictionResolutionPath       = "/memory/contradictions/resolve"
	storageTemperatureDecisionPath    = "/memory/storage/temperature"
	frontierT5StatusPath              = "/telemetry/policy-laboratory"
)

func frontierT5CanonicalDigest(prefix string, value any) string {
	encoded, _ := json.Marshal(value)
	return prefix + sha256Hex(string(encoded))
}

func frontierT5Metric(value map[string]any, key string) (float64, bool) {
	raw, exists := value[key]
	if !exists || raw == nil {
		return 0, false
	}
	metric, ok := contextPolicyNumber(raw)
	return metric, ok && !math.IsNaN(metric) && !math.IsInf(metric, 0)
}

func frontierT5PolicyMetrics(value map[string]any) map[string]any {
	out := map[string]any{}
	for _, field := range []string{"tokens", "latency_ms", "coverage", "utility", "source_count", "failure_rate"} {
		if metric, ok := frontierT5Metric(value, field); ok {
			out[field] = roundFloat(metric, 6)
		}
	}
	return out
}

func buildPolicySimulation(payload map[string]any) (map[string]any, error) {
	project, err := sanitizeMemoryProject(firstNonEmptyStrings(anyToString(payload["project"]), "contextlattice"))
	if err != nil {
		return nil, err
	}
	rawCases := contextPackAnyList(payload["cases"])
	if len(rawCases) == 0 && len(anyMap(payload["baseline"])) > 0 && len(anyMap(payload["candidate"])) > 0 {
		rawCases = []any{map[string]any{
			"case_id":     firstNonEmptyStrings(anyToString(payload["case_id"]), "single-case"),
			"snapshot_id": anyToString(payload["snapshot_id"]), "snapshot_digest": anyToString(payload["snapshot_digest"]),
			"baseline": payload["baseline"], "candidate": payload["candidate"],
		}}
	}
	if len(rawCases) == 0 {
		return nil, errors.New("cases or baseline plus candidate are required")
	}
	inputCaseCount := len(rawCases)
	limit := clampInt(anyToInt(payload["limit"], 128), 1, 256)
	truncated := len(rawCases) > limit
	if truncated {
		rawCases = rawCases[:limit]
	}
	rows := make([]any, 0, len(rawCases))
	deltaSums := map[string]float64{}
	deltaCounts := map[string]int{}
	sameSnapshotCount := 0
	for index, raw := range rawCases {
		item := anyMap(raw)
		caseID := clipText(strings.TrimSpace(firstNonEmptyStrings(anyToString(item["case_id"]), fmt.Sprintf("case-%d", index+1))), 160)
		snapshotID := clipText(strings.TrimSpace(anyToString(item["snapshot_id"])), 240)
		snapshotDigest := strings.TrimSpace(anyToString(item["snapshot_digest"]))
		if snapshotID == "" || snapshotDigest == "" {
			return nil, fmt.Errorf("cases[%d] requires immutable snapshot_id and snapshot_digest", index)
		}
		baseline := frontierT5PolicyMetrics(anyMap(item["baseline"]))
		candidate := frontierT5PolicyMetrics(anyMap(item["candidate"]))
		if len(baseline) == 0 || len(candidate) == 0 {
			return nil, fmt.Errorf("cases[%d] requires numeric baseline and candidate metrics", index)
		}
		deltas := map[string]any{}
		for _, field := range []string{"tokens", "latency_ms", "coverage", "utility", "source_count", "failure_rate"} {
			left, leftOK := frontierT5Metric(baseline, field)
			right, rightOK := frontierT5Metric(candidate, field)
			if !leftOK || !rightOK {
				continue
			}
			delta := right - left
			deltas[field] = roundFloat(delta, 6)
			deltaSums[field] += delta
			deltaCounts[field]++
		}
		sameSnapshotCount++
		rows = append(rows, map[string]any{
			"case_id": caseID, "snapshot_id": snapshotID, "snapshot_digest": snapshotDigest,
			"same_snapshot": true, "baseline": baseline, "candidate": candidate, "deltas": deltas,
		})
	}
	averages := map[string]any{}
	for field, sum := range deltaSums {
		averages[field] = roundFloat(sum/float64(deltaCounts[field]), 6)
	}
	identity := map[string]any{"project": project, "suite_id": anyToString(payload["suite_id"]), "rows": rows, "candidate_policy": anyMap(payload["policy"])}
	return map[string]any{
		"ok": true, "schema_id": policySimulationContractID, "version": 1,
		"simulation_id": frontierT5CanonicalDigest("polsim_", identity)[:31],
		"project":       project, "suite_id": clipText(strings.TrimSpace(anyToString(payload["suite_id"])), 240),
		"generated_at": nowUTCISO(), "case_count": len(rows), "input_case_count": inputCaseCount,
		"same_snapshot_count": sameSnapshotCount, "truncated": truncated, "rows": rows,
		"summary": map[string]any{"average_deltas": averages, "predicted_not_observed": true, "activation_recommended": false},
		"sensitivity": map[string]any{
			"token_budget_minus_10_percent": "replay-required", "token_budget_plus_10_percent": "replay-required",
			"reason": "Sensitivity is never fabricated from a single replay point.",
		},
		"computation":       map[string]any{"deterministic": true, "network_calls": 0, "persisted": false, "runtime_policy_mutated": false},
		"measurement_limit": "Replay predicts bounded same-snapshot deltas; observed production effects require independently verified assignments and outcomes.",
	}, nil
}

func frontierT5ShrinkInt(observed, prior int, weight float64) int {
	return int(math.Round(float64(prior)*(1-weight) + float64(observed)*weight))
}

func frontierT5PolicyDimension(raw, fallback string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(firstNonEmptyStrings(raw, fallback)))
	if value == "" || len(value) > 120 || strings.ContainsAny(value, "\r\n\x00") {
		return "", errors.New("policy dimension must be a bounded single line")
	}
	return value, nil
}

func (s *server) buildScopedPolicyCard(payload map[string]any) (map[string]any, error) {
	project, err := sanitizeMemoryProject(firstNonEmptyStrings(anyToString(payload["project"]), "contextlattice"))
	if err != nil {
		return nil, err
	}
	taskClass := clipText(strings.ToLower(strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["task_class"]), "general"))), 120)
	retrievalIntent := clipText(strings.ToLower(strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["retrieval_intent"]), "balanced"))), 120)
	minimum := clampInt(anyToInt(payload["minimum_outcomes"], 20), 10, 500)
	candidate, _, err := s.buildContextPolicyCandidate(map[string]any{
		"project": project, "task_class": taskClass, "retrieval_intent": retrievalIntent, "minimum_outcomes": minimum,
	})
	if err != nil {
		return nil, err
	}
	eligible := anyToInt(candidate["eligible_outcomes"], 0)
	priorWeight := clampInt(anyToInt(payload["global_prior_weight"], 20), 5, 200)
	weight := float64(eligible) / float64(eligible+priorWeight)
	observed := anyMap(candidate["policy"])
	prior := map[string]any{
		"target_context_tokens": 4000, "minimum_source_diversity": 2, "max_retrieval_rounds": 3,
		"prefer_graph_context": false, "temporal_claim_expansion": true, "require_proof_support": true,
		"selection_strategy": "impact_per_estimated_token_with_provenance_diversity",
	}
	policy := cloneMap(prior)
	for _, field := range []string{"target_context_tokens", "minimum_source_diversity", "max_retrieval_rounds"} {
		policy[field] = frontierT5ShrinkInt(anyToInt(observed[field], anyToInt(prior[field], 0)), anyToInt(prior[field], 0), weight)
	}
	if weight >= 0.6 {
		for _, field := range []string{"prefer_graph_context", "temporal_claim_expansion", "require_proof_support", "selection_strategy"} {
			if observed[field] != nil {
				policy[field] = observed[field]
			}
		}
	}
	generated := time.Now().UTC()
	expiryDays := clampInt(anyToInt(payload["drift_expiry_days"], 30), 1, 180)
	coldStart := eligible < minimum
	identity := map[string]any{"project": project, "task_class": taskClass, "retrieval_intent": retrievalIntent, "evidence_sha256": candidate["evidence_sha256"], "policy": policy}
	return map[string]any{
		"ok": true, "schema_id": scopedPolicyCardContractID, "version": 1,
		"card_id": frontierT5CanonicalDigest("scopedpol_", identity)[:34], "project": project,
		"task_class": taskClass, "retrieval_intent": retrievalIntent, "generated_at": generated.Format(time.RFC3339Nano),
		"expires_at": generated.Add(time.Duration(expiryDays) * 24 * time.Hour).Format(time.RFC3339Nano),
		"status":     map[bool]string{true: "cold_start", false: "evidence_bound"}[coldStart],
		"cold_start": coldStart, "sparse_data_shrinkage": true, "global_prior_weight": priorWeight,
		"project_evidence_weight": roundFloat(weight, 6), "minimum_outcomes": minimum, "eligible_outcomes": eligible,
		"global_prior": prior, "policy": policy,
		"evidence":          map[string]any{"candidate_id": candidate["candidate_id"], "evidence_sha256": candidate["evidence_sha256"], "cross_project_rows_used": 0},
		"activation":        map[string]any{"active": false, "runtime_mutation": false, "reason": "Public scoped cards are advisory until an entitled governed activation succeeds."},
		"measurement_limit": "Sparse projects shrink toward the disclosed global prior; drift expiry requires a new evidence-bound card.",
	}, nil
}

func frontierT5Wilson(successRate float64, count int) map[string]any {
	if count <= 0 {
		return map[string]any{"count": count, "lower": nil, "upper": nil, "method": "wilson_95"}
	}
	z := 1.959963984540054
	p := math.Max(0, math.Min(1, successRate))
	n := float64(count)
	denominator := 1 + z*z/n
	center := (p + z*z/(2*n)) / denominator
	margin := z * math.Sqrt((p*(1-p)+z*z/(4*n))/n) / denominator
	return map[string]any{"count": count, "lower": roundFloat(math.Max(0, center-margin), 6), "upper": roundFloat(math.Min(1, center+margin), 6), "method": "wilson_95"}
}

func frontierT5AssignmentReceipt(payload map[string]any) map[string]any {
	assignments := contextPackAnyList(payload["assignments"])
	seen := map[string]struct{}{}
	armCounts := map[string]int{}
	completed := 0
	duplicates := 0
	leakage := 0
	for _, raw := range assignments[:minInt(len(assignments), 1000)] {
		row := anyMap(raw)
		id := strings.TrimSpace(firstNonEmptyStrings(anyToString(row["assignment_id"]), anyToString(row["task_id"])))
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			duplicates++
			continue
		}
		seen[id] = struct{}{}
		arm := strings.ToLower(strings.TrimSpace(anyToString(row["arm"])))
		armCounts[arm]++
		if anyToBool(row["completed"]) {
			completed++
		}
		if anyToBool(row["training_overlap"]) || anyToBool(row["post_assignment_changed"]) {
			leakage++
		}
	}
	complete := len(seen) > 0 && armCounts["control"] > 0 && (armCounts["shadow"] > 0 || armCounts["canary"] > 0)
	return map[string]any{
		"assignment_count": len(seen), "completed_count": completed, "arm_counts": armCounts,
		"duplicate_count": duplicates, "leakage_count": leakage, "complete": complete,
		"survivor_bias_guard_passed": len(seen) > 0 && float64(completed)/float64(len(seen)) >= 0.8,
		"receipt_digest":             frontierT5CanonicalDigest("sha256:", assignments),
	}
}

func (s *server) buildPolicyPromotionRecommendation(payload map[string]any) (map[string]any, error) {
	payload = cloneMap(payload)
	payload["apply_transition"] = false
	evaluation, candidate, err := s.contextPolicyEvaluation(payload)
	if err != nil {
		return nil, err
	}
	receipt := frontierT5AssignmentReceipt(payload)
	control := anyMap(evaluation["control"])
	canary := anyMap(evaluation["canary"])
	driftCohorts := contextPackAnyList(payload["drift_cohorts"])
	driftPass := len(driftCohorts) > 0
	for _, raw := range driftCohorts[:minInt(len(driftCohorts), 64)] {
		row := anyMap(raw)
		_, utilityPresent := frontierT5Metric(row, "utility_delta")
		if strings.TrimSpace(anyToString(row["cohort"])) == "" || anyToInt(row["sample_count"], 0) < 10 || !utilityPresent || anyToBool(row["regressed"]) || anyToFloat(row["utility_delta"]) < -0.02 {
			driftPass = false
			break
		}
	}
	assignmentPass := anyToBool(receipt["complete"]) && anyToInt(receipt["leakage_count"], 0) == 0 && anyToBool(receipt["survivor_bias_guard_passed"])
	controlWilson := frontierT5Wilson(anyToFloat(control["first_pass_success_rate"]), anyToInt(control["sample_count"], 0))
	candidateWilson := frontierT5Wilson(anyToFloat(canary["first_pass_success_rate"]), anyToInt(canary["sample_count"], 0))
	controlUpper, controlUpperOK := frontierT5Metric(controlWilson, "upper")
	candidateLower, candidateLowerOK := frontierT5Metric(candidateWilson, "lower")
	uncertaintyPass := controlUpperOK && candidateLowerOK && candidateLower > controlUpper
	recommendation := anyToString(evaluation["decision"])
	controlledEvidenceRequired := anyToString(evaluation["previous_phase"]) != "candidate"
	eligible := recommendation == "advance"
	if controlledEvidenceRequired {
		eligible = eligible && assignmentPass && driftPass && uncertaintyPass
	}
	if !eligible && recommendation == "advance" {
		recommendation = "hold"
	}
	identity := map[string]any{"candidate_id": candidate["candidate_id"], "evaluation": evaluation, "assignment": receipt, "drift": driftCohorts}
	return map[string]any{
		"ok": true, "schema_id": policyPromotionRecommendationContractID, "version": 1,
		"recommendation_id": frontierT5CanonicalDigest("polpromo_", identity)[:33],
		"candidate_id":      candidate["candidate_id"], "project": candidate["project"], "generated_at": nowUTCISO(),
		"recommendation": recommendation, "recommended_phase": evaluation["recommended_phase"], "eligible": eligible,
		"assignment_exposure_receipt": receipt,
		"uncertainty": map[string]any{
			"control_first_pass": controlWilson, "candidate_first_pass": candidateWilson,
			"passed": uncertaintyPass, "required_for_transition": controlledEvidenceRequired,
		},
		"drift":             map[string]any{"cohort_count": len(driftCohorts), "passed": driftPass, "required_for_transition": controlledEvidenceRequired, "cohorts": driftCohorts[:minInt(len(driftCohorts), 64)]},
		"evaluation":        evaluation,
		"activation":        map[string]any{"runtime_mutation": false, "automatic": false, "entitled_gate_required": true},
		"measurement_limit": "Promotion is a recommendation only; runtime activation requires persisted canary evidence, entitlement, and explicit governed approval.",
	}, nil
}

func frontierT5Reason(payload map[string]any) (string, error) {
	reason := strings.TrimSpace(anyToString(payload["reason"]))
	if reason == "" || len(reason) > 500 || strings.ContainsAny(reason, "\x00") {
		return "", errors.New("reason is required and must be at most 500 bytes")
	}
	return clipText(reason, 500), nil
}

func frontierT5HasLegalHold(tags []string, payload map[string]any) bool {
	if anyToBool(payload["legal_hold"]) {
		return true
	}
	return memoryTagsHaveLegalHold(tags)
}

func frontierT5ParseStorageTier(raw string) (string, error) {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "hot":
		return "hot", nil
	case "warm":
		return "warm", nil
	case "deep":
		return "deep", nil
	case "retired":
		return "retired", nil
	default:
		return "", errors.New("tier must be hot, warm, deep, or retired")
	}
}

func frontierT5LatestEntry(store *memoryStore, project, file string) (memoryStoreEntry, bool) {
	if store == nil {
		return memoryStoreEntry{}, false
	}
	return store.currentEntry(project, file)
}

func frontierT5MemoryState(store *memoryStore, project, file string) (map[string]any, error) {
	if store == nil || !store.isEnabled() {
		return nil, errors.New("Go memory store is disabled")
	}
	project, err := sanitizeMemoryProject(project)
	if err != nil {
		return nil, err
	}
	file, err = sanitizeMemoryFile(file)
	if err != nil {
		return nil, err
	}
	current, exists := store.currentStateFor(project, file)
	if !exists || current.Tombstone {
		return nil, errors.New("authoritative current memory state is unavailable")
	}
	entry := current.Entry
	content, info, _, _, err := store.readFileUntracked(project, file)
	if err != nil {
		return nil, err
	}
	lifecycle := normalizeMemoryLifecycle(entry.Lifecycle)
	tier := normalizeMemoryStorageTier(entry.StorageTier)
	topic := entry.TopicPath
	if strings.TrimSpace(topic) == "" {
		topic = deriveTopicFromFile(file)
	}
	updatedAt := ""
	if info != nil {
		updatedAt = info.ModTime().UTC().Format(time.RFC3339Nano)
	}
	return map[string]any{
		"project": project, "file": file, "topic_path": topic, "content": content,
		"content_hash": sha256Hex(content), "content_ref": "sha256:" + sha256Hex(content),
		"lifecycle": lifecycle, "storage_tier": tier, "tags": append([]string(nil), entry.Tags...),
		"history_event_id": entry.EventID, "legal_hold": current.LegalHold,
		"agent_id": entry.AgentID, "session_id": entry.SessionID, "raw_bytes": len(content),
		"confidence": entry.Confidence, "last_accessed_at": entry.LastAccess, "updated_at": updatedAt,
	}, nil
}

func frontierT5ValidateExpectedHash(payload, state map[string]any) error {
	expected := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(anyToString(payload["expected_content_hash"]))), "sha256:")
	if expected == "" {
		return errors.New("expected_content_hash is required for a mutating lifecycle operation")
	}
	if expected != anyToString(state["content_hash"]) {
		return errors.New("expected_content_hash does not match current memory content")
	}
	return nil
}

func frontierT5ProtectedMemory(store *memoryStore, project, file string) bool {
	if store == nil {
		return true
	}
	return store.isExactStatePath(project, file) || loadWriteIngressPolicy().isDurableMemoryFile(normalizedWrite{project: project, fileName: file})
}

func (s *server) buildMemoryRetirement(payload map[string]any) (map[string]any, error) {
	operation := strings.ToLower(strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["operation"]), "propose")))
	project, err := sanitizeMemoryProject(firstNonEmptyStrings(anyToString(payload["project"]), "contextlattice"))
	if err != nil {
		return nil, err
	}
	file := strings.TrimSpace(anyToString(payload["file"]))
	base := map[string]any{
		"ok": true, "schema_id": memoryRetirementContractID, "version": 1, "operation": operation,
		"project": project, "generated_at": nowUTCISO(), "non_destructive": true, "content_deleted": false,
		"ordinary_memory_rewritten": false, "proposals": []any{}, "receipt": map[string]any{},
		"legal_hold_respected": true, "replacement_links_preserved": true,
	}
	switch operation {
	case "status":
		base["latest"] = s.frontierT5.latest("retirement", project, file)
		base["ledger"] = s.frontierT5.snapshot()
		return base, nil
	case "propose", "dry-run":
		rows := []map[string]any{}
		if file != "" {
			state, err := frontierT5MemoryState(s.memoryStore, project, file)
			if err != nil {
				return nil, err
			}
			rows = append(rows, state)
		} else {
			seen := map[string]struct{}{}
			for _, row := range s.memoryStore.recentItems(project, anyToString(payload["topic_path"]), clampInt(anyToInt(payload["limit"], 100), 1, 200), 0) {
				candidateFile := anyToString(row["file"])
				if _, exists := seen[candidateFile]; exists {
					continue
				}
				seen[candidateFile] = struct{}{}
				rows = append(rows, row)
			}
		}
		staleAfter := clampInt(anyToInt(payload["stale_after_days"], 90), 1, 3650)
		cutoff := time.Now().UTC().Add(-time.Duration(staleAfter) * 24 * time.Hour)
		proposals := []any{}
		for _, row := range rows {
			candidateFile := anyToString(row["file"])
			createdAt, parsed := parseTimeBestEffort(firstNonEmptyStrings(anyToString(row["last_accessed_at"]), anyToString(row["updated_at"]), anyToString(row["created_at"])))
			staleByAge := parsed && createdAt.Before(cutoff)
			explicitSupersession := strings.TrimSpace(anyToString(payload["superseded_by"])) != ""
			legalHold := frontierT5HasLegalHold(anyToStringList(row["tags"], 64), payload)
			protected := frontierT5ProtectedMemory(s.memoryStore, project, candidateFile)
			eligible := (staleByAge || explicitSupersession) && !legalHold && !protected && normalizeMemoryLifecycle(anyToString(row["lifecycle"])) != "retired"
			proposalCore := map[string]any{"project": project, "file": candidateFile, "content_hash": row["content_hash"], "stale_by_age": staleByAge, "superseded_by": payload["superseded_by"]}
			proposals = append(proposals, map[string]any{
				"proposal_id": frontierT5CanonicalDigest("retireprop_", proposalCore)[:35], "project": project, "file": candidateFile,
				"content_hash": row["content_hash"], "current_lifecycle": normalizeMemoryLifecycle(anyToString(row["lifecycle"])),
				"recommended_lifecycle": "retired", "eligible_for_apply": eligible, "stale_by_age": staleByAge,
				"legal_hold": legalHold, "protected_runtime_state": protected,
				"requires_explicit_approval": true, "replacement_link": anyToString(payload["superseded_by"]),
			})
		}
		base["proposals"] = proposals
		base["proposal_count"] = len(proposals)
		base["dry_run"] = true
		base["measurement_limit"] = "Age and supersession produce review proposals only; false-retirement risk requires content identity and explicit approval before apply."
		return base, nil
	case "apply":
		if !anyToBool(payload["approved"]) {
			return nil, errors.New("approved=true is required to retire memory")
		}
		if file == "" {
			return nil, errors.New("file is required to retire memory")
		}
		reason, err := frontierT5Reason(payload)
		if err != nil {
			return nil, err
		}
		state, err := frontierT5MemoryState(s.memoryStore, project, file)
		if err != nil {
			return nil, err
		}
		if err := frontierT5ValidateExpectedHash(payload, state); err != nil {
			return nil, err
		}
		if frontierT5HasLegalHold(anyToStringList(state["tags"], 64), payload) {
			return nil, errors.New("legal hold blocks memory retirement")
		}
		if frontierT5ProtectedMemory(s.memoryStore, project, file) {
			return nil, errors.New("runtime-state and protected durable files cannot be retired by this surface")
		}
		identity := map[string]any{"operation": operation, "project": project, "file": file, "content_hash": state["content_hash"], "reason": reason, "replacement": anyToString(payload["superseded_by"])}
		receiptID := frontierT5CanonicalDigest("memretire_", identity)[:34]
		if existing := s.frontierT5.receipt(receiptID); len(existing) > 0 {
			base["receipt"] = existing
			base["idempotent"] = true
			return base, nil
		}
		preparationID := frontierT5MutationPreparationID(receiptID)
		existingPreparation := s.frontierT5.receipt(preparationID)
		if normalizeMemoryLifecycle(anyToString(state["lifecycle"])) == "retired" && len(existingPreparation) == 0 {
			return nil, errors.New("memory is already retired; use status or restore with its original receipt")
		}
		preparation, _, err := s.frontierT5.prepareMutation(receiptID, map[string]any{
			"mutation": "memory_retirement", "project": project, "file": file, "content_hash": state["content_hash"],
			"previous_lifecycle": state["lifecycle"], "previous_storage_tier": state["storage_tier"],
			"target_lifecycle": "retired", "target_storage_tier": "retired", "reason": reason,
		})
		if err != nil {
			return nil, err
		}
		historyEventID := ""
		if normalizeMemoryLifecycle(anyToString(state["lifecycle"])) != "retired" || normalizeMemoryStorageTier(anyToString(state["storage_tier"])) != "retired" {
			entry, _, putErr := s.memoryStore.put(normalizedWrite{
				project: project, fileName: file, content: anyToString(state["content"]), topicPath: anyToString(state["topic_path"]),
				agentID: anyToString(state["agent_id"]), sessionID: anyToString(state["session_id"]), tags: anyToStringList(state["tags"], 128),
				lifecycle: "retired", storageTier: "retired",
			})
			if putErr != nil {
				return nil, putErr
			}
			historyEventID = entry.EventID
		} else if entry, exists := frontierT5LatestEntry(s.memoryStore, project, file); exists {
			historyEventID = entry.EventID
		}
		receipt := map[string]any{
			"schema_id": memoryRetirementContractID, "receipt_id": receiptID, "operation": operation, "project": project, "file": file,
			"content_hash": state["content_hash"], "previous_lifecycle": preparation["previous_lifecycle"], "current_lifecycle": "retired",
			"previous_storage_tier": preparation["previous_storage_tier"], "current_storage_tier": "retired", "reason": reason,
			"replacement_link": anyToString(payload["superseded_by"]), "recorded_at": nowUTCISO(), "content_deleted": false,
			"restorable": true, "history_event_id": historyEventID, "preparation_id": preparationID,
		}
		recorded, err := s.frontierT5.record(receipt)
		if err != nil {
			return nil, err
		}
		base["receipt"] = receipt
		base["recorded"] = recorded
		base["ordinary_memory_rewritten"] = true
		base["measurement_limit"] = "Retirement changes lifecycle and retrieval temperature while preserving content and immutable history."
		return base, nil
	case "restore":
		receiptID := strings.TrimSpace(anyToString(payload["receipt_id"]))
		if !anyToBool(payload["approved"]) || receiptID == "" {
			return nil, errors.New("approved=true and receipt_id are required to restore memory")
		}
		original := s.frontierT5.receipt(receiptID)
		if anyToString(original["schema_id"]) != memoryRetirementContractID || anyToString(original["operation"]) != "apply" {
			return nil, errors.New("retirement receipt not found")
		}
		if !strings.EqualFold(project, anyToString(original["project"])) {
			return nil, errors.New("retirement receipt belongs to a different project")
		}
		file = anyToString(original["file"])
		state, err := frontierT5MemoryState(s.memoryStore, project, file)
		if err != nil {
			return nil, err
		}
		if anyToString(state["content_hash"]) != anyToString(original["content_hash"]) {
			return nil, errors.New("memory changed after retirement; restore requires operator reconciliation")
		}
		reason, err := frontierT5Reason(payload)
		if err != nil {
			return nil, err
		}
		restoreIdentity := map[string]any{"operation": operation, "receipt_id": receiptID, "reason": reason}
		restoreID := frontierT5CanonicalDigest("memrestore_", restoreIdentity)[:35]
		if existing := s.frontierT5.receipt(restoreID); len(existing) > 0 {
			base["receipt"] = existing
			base["idempotent"] = true
			return base, nil
		}
		previousLifecycle := normalizeMemoryLifecycle(anyToString(original["previous_lifecycle"]))
		previousTier := normalizeMemoryStorageTier(anyToString(original["previous_storage_tier"]))
		preparationID := frontierT5MutationPreparationID(restoreID)
		existingPreparation := s.frontierT5.receipt(preparationID)
		if normalizeMemoryLifecycle(anyToString(state["lifecycle"])) != "retired" && len(existingPreparation) == 0 {
			return nil, errors.New("memory is not in the retired state recorded by this receipt")
		}
		_, _, err = s.frontierT5.prepareMutation(restoreID, map[string]any{
			"mutation": "memory_retirement_restore", "project": project, "file": file, "content_hash": state["content_hash"],
			"previous_lifecycle": "retired", "previous_storage_tier": "retired",
			"target_lifecycle": previousLifecycle, "target_storage_tier": previousTier, "reason": reason,
		})
		if err != nil {
			return nil, err
		}
		historyEventID := ""
		if normalizeMemoryLifecycle(anyToString(state["lifecycle"])) != previousLifecycle || normalizeMemoryStorageTier(anyToString(state["storage_tier"])) != previousTier {
			entry, _, putErr := s.memoryStore.put(normalizedWrite{
				project: project, fileName: file, content: anyToString(state["content"]), topicPath: anyToString(state["topic_path"]),
				agentID: anyToString(state["agent_id"]), sessionID: anyToString(state["session_id"]), tags: anyToStringList(state["tags"], 128),
				lifecycle: previousLifecycle, storageTier: previousTier,
			})
			if putErr != nil {
				return nil, putErr
			}
			historyEventID = entry.EventID
		} else if entry, exists := frontierT5LatestEntry(s.memoryStore, project, file); exists {
			historyEventID = entry.EventID
		}
		receipt := map[string]any{
			"schema_id": memoryRetirementContractID, "receipt_id": restoreID, "operation": operation, "project": project, "file": file,
			"restores_receipt_id": receiptID, "content_hash": state["content_hash"], "previous_lifecycle": "retired",
			"current_lifecycle": previousLifecycle, "previous_storage_tier": "retired", "current_storage_tier": previousTier,
			"reason": reason, "recorded_at": nowUTCISO(), "content_deleted": false, "restorable": false,
			"history_event_id": historyEventID, "preparation_id": preparationID,
		}
		recorded, err := s.frontierT5.record(receipt)
		if err != nil {
			return nil, err
		}
		base["receipt"] = receipt
		base["recorded"] = recorded
		base["ordinary_memory_rewritten"] = true
		return base, nil
	default:
		return nil, errors.New("operation must be propose, dry-run, apply, restore, or status")
	}
}

func frontierT5ClaimWeight(claim temporalClaim) float64 {
	weight := math.Max(0, math.Min(1, claim.Confidence))
	switch strings.ToLower(anyToString(claim.Verification["status"])) {
	case "verified":
		weight *= 1.25
	case "disputed":
		weight *= 0.6
	case "failed":
		weight *= 0.2
	default:
		weight *= 0.8
	}
	weight += math.Min(0.2, float64(len(claim.Support))*0.04)
	weight -= math.Min(0.3, float64(len(claim.Opposition))*0.05)
	return math.Max(0, math.Min(1.5, weight))
}

func frontierT5Claims(store *temporalClaimStore, project string, ids []string, query string) []temporalClaim {
	if store == nil || !store.enabled {
		return []temporalClaim{}
	}
	if len(ids) == 0 {
		return store.query(temporalClaimQuery{
			Project: project, Query: query, Limit: 100, IncludeExpired: true, IncludeSuperseded: true, IncludeRetracted: true,
		})
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	rows := make([]temporalClaim, 0, len(ids))
	seen := map[string]struct{}{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if _, exists := seen[id]; exists {
			continue
		}
		claim, exists := store.claims[id]
		if !exists || !strings.EqualFold(claim.Project, project) {
			continue
		}
		seen[id] = struct{}{}
		rows = append(rows, claim)
	}
	return rows
}

func frontierT5ClaimsContradict(left, right temporalClaim) bool {
	return stringSliceContains(left.Contradicts, right.ClaimID) || stringSliceContains(right.Contradicts, left.ClaimID)
}

func frontierT5DirectContradictionIDs(winner string, claims []temporalClaim) []string {
	var selected *temporalClaim
	for index := range claims {
		if claims[index].ClaimID == winner {
			selected = &claims[index]
			break
		}
	}
	if selected == nil {
		return nil
	}
	losers := make([]string, 0, len(claims)-1)
	for _, claim := range claims {
		if claim.ClaimID != winner && frontierT5ClaimsContradict(*selected, claim) {
			losers = append(losers, claim.ClaimID)
		}
	}
	sort.Strings(losers)
	return losers
}

func frontierT5ContradictionRecommendation(project string, claims []temporalClaim, threshold float64) map[string]any {
	type weightedClaim struct {
		claim  temporalClaim
		weight float64
	}
	weighted := make([]weightedClaim, 0, len(claims))
	linked := false
	for index, claim := range claims {
		weighted = append(weighted, weightedClaim{claim: claim, weight: frontierT5ClaimWeight(claim)})
		for other := index + 1; other < len(claims); other++ {
			linked = linked || frontierT5ClaimsContradict(claim, claims[other])
		}
	}
	sort.SliceStable(weighted, func(i, j int) bool {
		if weighted[i].weight == weighted[j].weight {
			return weighted[i].claim.ClaimID < weighted[j].claim.ClaimID
		}
		return weighted[i].weight > weighted[j].weight
	})
	rows := make([]any, 0, len(weighted))
	for _, item := range weighted {
		rows = append(rows, map[string]any{
			"claim_id": item.claim.ClaimID, "statement": clipText(item.claim.Statement, 500), "status": item.claim.Status,
			"confidence": item.claim.Confidence, "verification_status": anyToString(item.claim.Verification["status"]),
			"support_count": len(item.claim.Support), "opposition_count": len(item.claim.Opposition),
			"evidence_weight": roundFloat(item.weight, 6), "provenance_present": len(item.claim.Provenance) > 0,
		})
	}
	winner := ""
	margin := 0.0
	status := "abstained"
	reasons := []any{}
	if len(weighted) < 2 {
		reasons = append(reasons, "at least two claims are required")
	} else if !linked {
		reasons = append(reasons, "selected claims do not carry an explicit contradiction edge")
	} else {
		for _, candidate := range weighted {
			loserIDs := frontierT5DirectContradictionIDs(candidate.claim.ClaimID, claims)
			if len(loserIDs) == 0 {
				continue
			}
			strongestOpponent := -1.0
			for _, opponent := range weighted {
				if stringSliceContains(loserIDs, opponent.claim.ClaimID) && opponent.weight > strongestOpponent {
					strongestOpponent = opponent.weight
				}
			}
			candidateMargin := candidate.weight - strongestOpponent
			if candidateMargin >= threshold && len(candidate.claim.Support) > 0 && anyToString(candidate.claim.Verification["status"]) == "verified" {
				winner = candidate.claim.ClaimID
				margin = candidateMargin
				status = "recommended"
				break
			}
		}
		if winner == "" {
			reasons = append(reasons, "evidence margin or independent verification is insufficient")
		}
	}
	losers := []any{}
	if winner != "" {
		for _, id := range frontierT5DirectContradictionIDs(winner, claims) {
			losers = append(losers, id)
		}
	}
	identity := map[string]any{"project": project, "claims": rows, "threshold": threshold}
	return map[string]any{
		"recommendation_id": frontierT5CanonicalDigest("contrarec_", identity)[:33], "status": status,
		"winning_claim_id": winner, "losing_claim_ids": losers, "threshold": roundFloat(threshold, 6),
		"evidence_margin": roundFloat(margin, 6), "claims": rows, "reasons": reasons,
		"explicit_contradiction_link": linked, "operator_decision_required": true,
	}
}

func frontierT5ApplyClaimStatuses(store *temporalClaimStore, project, winner string, losers []string, restore map[string]string) (map[string]string, error) {
	if store == nil || !store.enabled {
		return nil, errors.New("temporal claim graph is disabled")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	ids := append([]string{winner}, losers...)
	previous := map[string]string{}
	changed := []temporalClaim{}
	now := nowUTCISO()
	for _, id := range ids {
		if id == "" {
			continue
		}
		claim, exists := store.claims[id]
		if !exists {
			return nil, fmt.Errorf("claim not found: %s", id)
		}
		if !strings.EqualFold(claim.Project, project) {
			return nil, fmt.Errorf("claim belongs to a different project: %s", id)
		}
		previous[id] = claim.Status
		if restore != nil {
			restored, err := normalizeTemporalClaimStatus(restore[id])
			if err != nil {
				return nil, fmt.Errorf("restore claim %s: %w", id, err)
			}
			claim.Status = restored
		} else if id == winner {
			claim.Status = "active"
		} else {
			claim.Status = "retracted"
		}
		claim.UpdatedAt = now
		claim.Revision++
		changed = append(changed, claim)
	}
	if err := store.appendBatchLocked(changed); err != nil {
		return nil, err
	}
	for _, claim := range changed {
		store.setClaimLocked(claim)
	}
	store.trimLocked()
	return previous, nil
}

func frontierT5CurrentClaimStatuses(store *temporalClaimStore, project, winner string, losers []string) (map[string]string, error) {
	if store == nil || !store.enabled {
		return nil, errors.New("temporal claim graph is disabled")
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	statuses := map[string]string{}
	seen := map[string]struct{}{}
	for _, id := range append([]string{winner}, losers...) {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		claim, exists := store.claims[id]
		if !exists {
			return nil, fmt.Errorf("claim not found: %s", id)
		}
		if !strings.EqualFold(claim.Project, project) {
			return nil, fmt.Errorf("claim belongs to a different project: %s", id)
		}
		seen[id] = struct{}{}
		statuses[id] = claim.Status
	}
	return statuses, nil
}

func frontierT5ClaimStatusesMatch(statuses map[string]string, winner string, losers []string, restore map[string]string) bool {
	for _, id := range append([]string{winner}, losers...) {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		expected := "retracted"
		if id == winner {
			expected = "active"
		}
		if restore != nil {
			normalized, err := normalizeTemporalClaimStatus(restore[id])
			if err != nil {
				return false
			}
			expected = normalized
		}
		if !strings.EqualFold(strings.TrimSpace(statuses[id]), expected) {
			return false
		}
	}
	return true
}

func (s *server) buildContradictionResolution(payload map[string]any) (map[string]any, error) {
	operation := strings.ToLower(strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["operation"]), "recommend")))
	project, err := sanitizeMemoryProject(firstNonEmptyStrings(anyToString(payload["project"]), "contextlattice"))
	if err != nil {
		return nil, err
	}
	ids := anyToStringList(payload["claim_ids"], 64)
	claims := frontierT5Claims(s.temporalClaims, project, ids, anyToString(payload["query"]))
	threshold := anyToFloat(payload["threshold"])
	if threshold <= 0 {
		threshold = 0.15
	}
	threshold = math.Max(0.01, math.Min(1, threshold))
	recommendation := frontierT5ContradictionRecommendation(project, claims, threshold)
	response := map[string]any{
		"ok": true, "schema_id": contradictionResolutionContractID, "version": 1, "operation": operation,
		"project": project, "generated_at": nowUTCISO(), "recommendation": recommendation,
		"resolution": map[string]any{}, "recorded": false, "ordinary_memory_mutated": false,
		"claim_lifecycle_mutated": false, "immutable_receipt": false,
		"measurement_limit": "Evidence weights are deterministic advice; ambiguous or unverified conflicts abstain and every lifecycle decision remains appealable.",
	}
	switch operation {
	case "recommend", "status":
		if operation == "status" {
			response["latest"] = s.frontierT5.latest("contradiction", project, "")
		}
		return response, nil
	case "decide":
		if !anyToBool(payload["approved"]) {
			return nil, errors.New("approved=true is required for an operator contradiction decision")
		}
		operator := clipText(strings.TrimSpace(anyToString(payload["operator"])), 160)
		if operator == "" {
			return nil, errors.New("operator is required")
		}
		if len(ids) < 2 {
			return nil, errors.New("at least two explicit claim_ids are required for a contradiction decision")
		}
		resolvedClaims := map[string]temporalClaim{}
		for _, claim := range claims {
			resolvedClaims[claim.ClaimID] = claim
		}
		for _, id := range ids {
			if _, exists := resolvedClaims[id]; !exists {
				return nil, fmt.Errorf("claim is missing or belongs to a different project: %s", id)
			}
		}
		reason, err := frontierT5Reason(payload)
		if err != nil {
			return nil, err
		}
		winner := strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["winning_claim_id"]), anyToString(recommendation["winning_claim_id"])))
		if winner == "" {
			return nil, errors.New("winning_claim_id is required; the evidence recommendation abstained")
		}
		if !stringSliceContains(ids, winner) {
			return nil, errors.New("winning_claim_id must be one of the explicit claim_ids")
		}
		losers := frontierT5DirectContradictionIDs(winner, claims)
		if len(losers) == 0 {
			return nil, errors.New("winning_claim_id has no explicit contradiction edge to the selected claims")
		}
		identity := map[string]any{"operation": operation, "project": project, "winner": winner, "losers": losers, "operator": operator, "reason": reason}
		resolutionID := frontierT5CanonicalDigest("contrares_", identity)[:33]
		if existing := s.frontierT5.receipt(resolutionID); len(existing) > 0 {
			response["resolution"] = existing
			response["idempotent"] = true
			return response, nil
		}
		currentStatuses, err := frontierT5CurrentClaimStatuses(s.temporalClaims, project, winner, losers)
		if err != nil {
			return nil, err
		}
		preparationID := frontierT5MutationPreparationID(resolutionID)
		preparation, _, err := s.frontierT5.prepareMutation(resolutionID, map[string]any{
			"mutation": "contradiction_resolution", "project": project, "winning_claim_id": winner,
			"losing_claim_ids": losers, "previous_statuses": currentStatuses, "operator": operator, "reason": reason,
		})
		if err != nil {
			return nil, err
		}
		previous := map[string]string{}
		for id, status := range anyMap(preparation["previous_statuses"]) {
			previous[id] = anyToString(status)
		}
		if !frontierT5ClaimStatusesMatch(currentStatuses, winner, losers, nil) {
			if _, err := frontierT5ApplyClaimStatuses(s.temporalClaims, project, winner, losers, nil); err != nil {
				return nil, err
			}
		}
		resolution := map[string]any{
			"schema_id": contradictionResolutionContractID, "resolution_id": resolutionID, "operation": operation,
			"project": project, "winning_claim_id": winner, "losing_claim_ids": losers, "previous_statuses": previous,
			"operator": operator, "reason": reason, "recorded_at": nowUTCISO(), "status": "resolved",
			"appealable": true, "ordinary_memory_mutated": false, "claim_lifecycle_mutated": true,
			"preparation_id": preparationID,
		}
		recorded, err := s.frontierT5.record(resolution)
		if err != nil {
			return nil, err
		}
		response["resolution"] = resolution
		response["recorded"] = recorded
		response["claim_lifecycle_mutated"] = true
		response["immutable_receipt"] = true
		return response, nil
	case "reopen", "appeal":
		if !anyToBool(payload["approved"]) {
			return nil, errors.New("approved=true is required to reopen or appeal a resolution")
		}
		operator := clipText(strings.TrimSpace(anyToString(payload["operator"])), 160)
		if operator == "" {
			return nil, errors.New("operator is required to reopen or appeal a resolution")
		}
		priorID := strings.TrimSpace(anyToString(payload["resolution_id"]))
		prior := s.frontierT5.receipt(priorID)
		if anyToString(prior["schema_id"]) != contradictionResolutionContractID || anyToString(prior["operation"]) != "decide" {
			return nil, errors.New("resolved contradiction receipt not found")
		}
		if !strings.EqualFold(project, anyToString(prior["project"])) {
			return nil, errors.New("contradiction resolution belongs to a different project")
		}
		reason, err := frontierT5Reason(payload)
		if err != nil {
			return nil, err
		}
		identity := map[string]any{"operation": operation, "resolution_id": priorID, "reason": reason}
		resolutionID := frontierT5CanonicalDigest("contrares_", identity)[:33]
		if existing := s.frontierT5.receipt(resolutionID); len(existing) > 0 {
			response["resolution"] = existing
			response["idempotent"] = true
			return response, nil
		}
		claimMutated := false
		if operation == "reopen" {
			restore := map[string]string{}
			for key, value := range anyMap(prior["previous_statuses"]) {
				restore[key] = anyToString(value)
			}
			winner := anyToString(prior["winning_claim_id"])
			losers := anyToStringList(prior["losing_claim_ids"], 64)
			currentStatuses, statusErr := frontierT5CurrentClaimStatuses(s.temporalClaims, project, winner, losers)
			if statusErr != nil {
				return nil, statusErr
			}
			preparationID := frontierT5MutationPreparationID(resolutionID)
			if _, _, err = s.frontierT5.prepareMutation(resolutionID, map[string]any{
				"mutation": "contradiction_reopen", "project": project, "winning_claim_id": winner,
				"losing_claim_ids": losers, "current_statuses": currentStatuses, "restore_statuses": restore, "reason": reason,
			}); err != nil {
				return nil, err
			}
			if !frontierT5ClaimStatusesMatch(currentStatuses, winner, losers, restore) {
				_, err = frontierT5ApplyClaimStatuses(s.temporalClaims, project, winner, losers, restore)
			}
			if err != nil {
				return nil, err
			}
			claimMutated = true
			response["preparation_id"] = preparationID
		}
		resolution := map[string]any{
			"schema_id": contradictionResolutionContractID, "resolution_id": resolutionID, "operation": operation,
			"project": project, "reopens_resolution_id": priorID, "winning_claim_id": prior["winning_claim_id"],
			"losing_claim_ids": prior["losing_claim_ids"], "operator": operator, "reason": reason, "recorded_at": nowUTCISO(),
			"status": map[string]string{"reopen": "reopened", "appeal": "appealed"}[operation], "appealable": true,
			"ordinary_memory_mutated": false, "claim_lifecycle_mutated": claimMutated,
		}
		recorded, err := s.frontierT5.record(resolution)
		if err != nil {
			return nil, err
		}
		response["resolution"] = resolution
		response["recorded"] = recorded
		response["claim_lifecycle_mutated"] = claimMutated
		response["immutable_receipt"] = true
		return response, nil
	default:
		return nil, errors.New("operation must be recommend, decide, reopen, appeal, or status")
	}
}

func frontierT5RecommendedStorageTier(state, policy map[string]any) (string, []any) {
	reasons := []any{}
	lifecycle := normalizeMemoryLifecycle(anyToString(state["lifecycle"]))
	if lifecycle == "retired" || lifecycle == "retracted" || lifecycle == "superseded" {
		return "retired", []any{"retirement precedence"}
	}
	if frontierT5HasLegalHold(anyToStringList(state["tags"], 64), policy) {
		return "hot", []any{"legal hold preserves immediate availability"}
	}
	confidence := anyToFloat(state["confidence"])
	lastTouch, parsed := parseTimeBestEffort(firstNonEmptyStrings(anyToString(state["last_accessed_at"]), anyToString(state["updated_at"])))
	ageDays := 0.0
	if parsed {
		ageDays = time.Since(lastTouch).Hours() / 24
	}
	if confidence >= 0.9 {
		reasons = append(reasons, "rare high-confidence evidence stays hot")
		return "hot", reasons
	}
	pressure := strings.ToLower(strings.TrimSpace(firstNonEmptyStrings(anyToString(policy["disk_pressure"]), "normal")))
	if pressure != "normal" && pressure != "high" && pressure != "critical" {
		pressure = "normal"
	}
	if !parsed || ageDays <= 7 {
		if parsed && pressure == "critical" && ageDays > 1 {
			return "warm", []any{"critical disk pressure", "recent evidence remains online in the warm tier"}
		}
		reasons = append(reasons, "recently written or accessed")
		return "hot", reasons
	}
	if pressure == "critical" {
		return "deep", []any{"critical disk pressure", "evidence remains reversibly retrievable"}
	}
	if pressure == "high" && ageDays > 14 {
		return "deep", []any{"high disk pressure", "low recent demand with reversible deep retrieval available"}
	}
	if ageDays <= 30 || confidence >= 0.65 {
		reasons = append(reasons, "moderate recency or confidence")
		return "warm", reasons
	}
	reasons = append(reasons, "low recent demand with reversible deep retrieval available")
	return "deep", reasons
}

func (s *server) buildStorageTemperatureDecision(payload map[string]any) (map[string]any, error) {
	operation := strings.ToLower(strings.TrimSpace(firstNonEmptyStrings(anyToString(payload["operation"]), "recommend")))
	project, err := sanitizeMemoryProject(firstNonEmptyStrings(anyToString(payload["project"]), "contextlattice"))
	if err != nil {
		return nil, err
	}
	file := strings.TrimSpace(anyToString(payload["file"]))
	response := map[string]any{
		"ok": true, "schema_id": storageTemperatureDecisionContractID, "version": 1, "operation": operation,
		"project": project, "generated_at": nowUTCISO(), "decisions": []any{}, "receipt": map[string]any{},
		"recorded": false, "content_deleted": false, "physical_store_changed": false,
		"retrieval_temperature_changed": false, "legal_hold_respected": true,
		"measurement_limit": "Temperature is reversible retrieval-tier metadata; this surface never claims physical byte movement without a configured deep-store adapter.",
	}
	switch operation {
	case "status":
		response["latest"] = s.frontierT5.latest("temperature", project, file)
		response["ledger"] = s.frontierT5.snapshot()
		return response, nil
	case "recommend", "dry-run":
		states := []map[string]any{}
		if file != "" {
			state, err := frontierT5MemoryState(s.memoryStore, project, file)
			if err != nil {
				return nil, err
			}
			states = append(states, state)
		} else {
			seen := map[string]struct{}{}
			for _, row := range s.memoryStore.recentItems(project, anyToString(payload["topic_path"]), clampInt(anyToInt(payload["limit"], 100), 1, 200), 0) {
				candidateFile := anyToString(row["file"])
				if _, exists := seen[candidateFile]; exists {
					continue
				}
				seen[candidateFile] = struct{}{}
				state, stateErr := frontierT5MemoryState(s.memoryStore, project, candidateFile)
				if stateErr == nil {
					states = append(states, state)
				}
			}
		}
		decisions := []any{}
		for _, state := range states {
			tier, reasons := frontierT5RecommendedStorageTier(state, payload)
			core := map[string]any{"project": project, "file": state["file"], "content_hash": state["content_hash"], "tier": tier}
			decisions = append(decisions, map[string]any{
				"decision_id": frontierT5CanonicalDigest("tempdec_", core)[:31], "project": project, "file": state["file"],
				"content_hash": state["content_hash"], "current_tier": state["storage_tier"], "recommended_tier": tier,
				"utility_inputs": map[string]any{"confidence": state["confidence"], "last_accessed_at": state["last_accessed_at"], "raw_bytes": state["raw_bytes"], "disk_pressure": firstNonEmptyStrings(anyToString(payload["disk_pressure"]), "normal")},
				"reasons":        reasons, "legal_hold": frontierT5HasLegalHold(anyToStringList(state["tags"], 64), payload),
				"explicit_apply_required": true, "reversible": true,
			})
		}
		response["decisions"] = decisions
		response["decision_count"] = len(decisions)
		return response, nil
	case "apply":
		if !anyToBool(payload["approved"]) || file == "" {
			return nil, errors.New("approved=true and file are required to apply a storage temperature")
		}
		target, err := frontierT5ParseStorageTier(anyToString(payload["tier"]))
		if err != nil {
			return nil, err
		}
		reason, err := frontierT5Reason(payload)
		if err != nil {
			return nil, err
		}
		state, err := frontierT5MemoryState(s.memoryStore, project, file)
		if err != nil {
			return nil, err
		}
		if err := frontierT5ValidateExpectedHash(payload, state); err != nil {
			return nil, err
		}
		if frontierT5ProtectedMemory(s.memoryStore, project, file) && (target == "deep" || target == "retired") {
			return nil, errors.New("protected runtime-state memory cannot move to deep or retired")
		}
		if frontierT5HasLegalHold(anyToStringList(state["tags"], 64), payload) && (target == "deep" || target == "retired") {
			return nil, errors.New("legal hold blocks deep or retired temperature")
		}
		identity := map[string]any{"operation": operation, "project": project, "file": file, "hash": state["content_hash"], "tier": target, "reason": reason}
		receiptID := frontierT5CanonicalDigest("tempmove_", identity)[:32]
		if existing := s.frontierT5.receipt(receiptID); len(existing) > 0 {
			response["receipt"] = existing
			response["idempotent"] = true
			return response, nil
		}
		lifecycle := normalizeMemoryLifecycle(anyToString(state["lifecycle"]))
		if target == "retired" {
			lifecycle = "retired"
		} else if lifecycle == "retired" {
			return nil, errors.New("retired memory must be restored through its retirement receipt before changing temperature")
		}
		preparationID := frontierT5MutationPreparationID(receiptID)
		_, _, err = s.frontierT5.prepareMutation(receiptID, map[string]any{
			"mutation": "storage_temperature", "project": project, "file": file, "content_hash": state["content_hash"],
			"previous_lifecycle": state["lifecycle"], "previous_storage_tier": state["storage_tier"],
			"target_lifecycle": lifecycle, "target_storage_tier": target, "reason": reason,
		})
		if err != nil {
			return nil, err
		}
		historyEventID := ""
		tierChanged := normalizeMemoryStorageTier(anyToString(state["storage_tier"])) != target || normalizeMemoryLifecycle(anyToString(state["lifecycle"])) != lifecycle
		if tierChanged {
			entry, _, putErr := s.memoryStore.put(normalizedWrite{
				project: project, fileName: file, content: anyToString(state["content"]), topicPath: anyToString(state["topic_path"]),
				agentID: anyToString(state["agent_id"]), sessionID: anyToString(state["session_id"]), tags: anyToStringList(state["tags"], 128),
				lifecycle: lifecycle, storageTier: target,
			})
			if putErr != nil {
				return nil, putErr
			}
			historyEventID = entry.EventID
		} else if entry, exists := frontierT5LatestEntry(s.memoryStore, project, file); exists {
			historyEventID = entry.EventID
		}
		receipt := map[string]any{
			"schema_id": storageTemperatureDecisionContractID, "receipt_id": receiptID, "decision_id": receiptID,
			"operation": operation, "project": project, "file": file, "content_hash": state["content_hash"],
			"previous_tier": state["storage_tier"], "current_tier": target, "previous_lifecycle": state["lifecycle"],
			"current_lifecycle": lifecycle, "reason": reason, "recorded_at": nowUTCISO(), "history_event_id": historyEventID,
			"content_deleted": false, "physical_store_changed": false, "retrieval_temperature_changed": tierChanged,
			"restorable": true, "preparation_id": preparationID,
		}
		recorded, err := s.frontierT5.record(receipt)
		if err != nil {
			return nil, err
		}
		response["receipt"] = receipt
		response["recorded"] = recorded
		response["retrieval_temperature_changed"] = tierChanged
		return response, nil
	case "restore":
		priorID := strings.TrimSpace(anyToString(payload["receipt_id"]))
		if !anyToBool(payload["approved"]) || priorID == "" {
			return nil, errors.New("approved=true and receipt_id are required to restore temperature")
		}
		prior := s.frontierT5.receipt(priorID)
		if anyToString(prior["schema_id"]) != storageTemperatureDecisionContractID || anyToString(prior["operation"]) != "apply" {
			return nil, errors.New("storage temperature receipt not found")
		}
		if !strings.EqualFold(project, anyToString(prior["project"])) {
			return nil, errors.New("storage temperature receipt belongs to a different project")
		}
		file = anyToString(prior["file"])
		state, err := frontierT5MemoryState(s.memoryStore, project, file)
		if err != nil {
			return nil, err
		}
		if anyToString(state["content_hash"]) != anyToString(prior["content_hash"]) {
			return nil, errors.New("memory changed after temperature move; restore requires reconciliation")
		}
		if normalizeMemoryStorageTier(anyToString(state["storage_tier"])) != normalizeMemoryStorageTier(anyToString(prior["current_tier"])) ||
			normalizeMemoryLifecycle(anyToString(state["lifecycle"])) != normalizeMemoryLifecycle(anyToString(prior["current_lifecycle"])) ||
			strings.TrimSpace(anyToString(state["history_event_id"])) == "" ||
			!strings.EqualFold(anyToString(state["history_event_id"]), anyToString(prior["history_event_id"])) {
			return nil, errors.New("a newer lifecycle or temperature transition superseded this receipt")
		}
		reason, err := frontierT5Reason(payload)
		if err != nil {
			return nil, err
		}
		restoreID := frontierT5CanonicalDigest("temprestore_", map[string]any{"receipt_id": priorID, "reason": reason})[:35]
		if existing := s.frontierT5.receipt(restoreID); len(existing) > 0 {
			response["receipt"] = existing
			response["idempotent"] = true
			return response, nil
		}
		previousTier := normalizeMemoryStorageTier(anyToString(prior["previous_tier"]))
		previousLifecycle := normalizeMemoryLifecycle(anyToString(prior["previous_lifecycle"]))
		preparationID := frontierT5MutationPreparationID(restoreID)
		_, _, err = s.frontierT5.prepareMutation(restoreID, map[string]any{
			"mutation": "storage_temperature_restore", "project": project, "file": file, "content_hash": state["content_hash"],
			"previous_lifecycle": state["lifecycle"], "previous_storage_tier": state["storage_tier"],
			"target_lifecycle": previousLifecycle, "target_storage_tier": previousTier, "reason": reason,
		})
		if err != nil {
			return nil, err
		}
		historyEventID := ""
		tierChanged := normalizeMemoryStorageTier(anyToString(state["storage_tier"])) != previousTier || normalizeMemoryLifecycle(anyToString(state["lifecycle"])) != previousLifecycle
		if tierChanged {
			entry, _, putErr := s.memoryStore.put(normalizedWrite{
				project: project, fileName: file, content: anyToString(state["content"]), topicPath: anyToString(state["topic_path"]),
				agentID: anyToString(state["agent_id"]), sessionID: anyToString(state["session_id"]), tags: anyToStringList(state["tags"], 128),
				lifecycle: previousLifecycle, storageTier: previousTier,
			})
			if putErr != nil {
				return nil, putErr
			}
			historyEventID = entry.EventID
		} else if entry, exists := frontierT5LatestEntry(s.memoryStore, project, file); exists {
			historyEventID = entry.EventID
		}
		receipt := map[string]any{
			"schema_id": storageTemperatureDecisionContractID, "receipt_id": restoreID, "decision_id": restoreID,
			"operation": operation, "project": project, "file": file, "restores_receipt_id": priorID,
			"content_hash": state["content_hash"], "previous_tier": state["storage_tier"], "current_tier": previousTier,
			"previous_lifecycle": state["lifecycle"], "current_lifecycle": previousLifecycle, "reason": reason,
			"recorded_at": nowUTCISO(), "history_event_id": historyEventID, "content_deleted": false,
			"physical_store_changed": false, "retrieval_temperature_changed": tierChanged, "restorable": false,
			"preparation_id": preparationID,
		}
		recorded, err := s.frontierT5.record(receipt)
		if err != nil {
			return nil, err
		}
		response["receipt"] = receipt
		response["recorded"] = recorded
		response["retrieval_temperature_changed"] = tierChanged
		return response, nil
	default:
		return nil, errors.New("operation must be recommend, dry-run, apply, restore, or status")
	}
}

func frontierT5ReadPayload(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return nil, false
	}
	if r.Method == http.MethodGet {
		if !strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("operation")), "status") {
			w.Header().Set("Allow", "POST")
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed", "detail": "GET supports operation=status only"})
			return nil, false
		}
		return queryPayload(r), true
	}
	payload, err := readOptionalJSONBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
		return nil, false
	}
	return payload, true
}

func (s *server) frontierT5PublicRoute(w http.ResponseWriter, r *http.Request, contractID string, build func(map[string]any) (map[string]any, error)) {
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	payload, ok := frontierT5ReadPayload(w, r)
	if !ok {
		return
	}
	result, err := build(payload)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "schema_id": contractID, "error": "frontier_t5_request_failed", "detail": clipText(err.Error(), 500)})
		return
	}
	writeJSON(w, http.StatusOK, attachPayloadFormatContract(contractID, result, anyToString(payload["agent_id"]), "frontier_t5_policy_laboratory", r.URL.Path))
}

func (s *server) memoryPolicySimulation(w http.ResponseWriter, r *http.Request) {
	s.frontierT5PublicRoute(w, r, policySimulationContractID, buildPolicySimulation)
}

func (s *server) memoryScopedPolicyCard(w http.ResponseWriter, r *http.Request) {
	s.frontierT5PublicRoute(w, r, scopedPolicyCardContractID, s.buildScopedPolicyCard)
}

func (s *server) memoryPolicyPromotionRecommendation(w http.ResponseWriter, r *http.Request) {
	s.frontierT5PublicRoute(w, r, policyPromotionRecommendationContractID, s.buildPolicyPromotionRecommendation)
}

func (s *server) memoryRetirement(w http.ResponseWriter, r *http.Request) {
	s.frontierT5PublicRoute(w, r, memoryRetirementContractID, s.buildMemoryRetirement)
}

func (s *server) memoryContradictionResolution(w http.ResponseWriter, r *http.Request) {
	s.frontierT5PublicRoute(w, r, contradictionResolutionContractID, s.buildContradictionResolution)
}

func (s *server) memoryStorageTemperature(w http.ResponseWriter, r *http.Request) {
	s.frontierT5PublicRoute(w, r, storageTemperatureDecisionContractID, s.buildStorageTemperatureDecision)
}

func (s *server) telemetryPolicyLaboratory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "schema_id": frontierT5StatusContractID, "version": 1, "updated_at": nowUTCISO(),
		"mode": "public_advisory_with_explicit_reversible_lifecycle_operations", "ledger": s.frontierT5.snapshot(),
		"context_policy": s.contextPolicy.snapshot(), "temporal_claims": s.temporalClaims.snapshot(),
		"network_calls": 0, "automatic_runtime_activation": false,
	})
}
