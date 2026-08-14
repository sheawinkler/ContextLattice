package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Retrieval promotion is deliberately a presentation boundary.  The native
// retrieval result and the post-policy-safe candidate union remain the source
// of truth; a learned arm can only change the bounded presentation after the
// exact evidence and canary receipts below have cleared every guard.
const (
	retrievalPromotionContractID                 = "retrieval_intelligence_promotion.v1"
	retrievalPromotionVersion                    = 1
	retrievalPromotionCanaryReceiptSchemaID      = "retrieval_intelligence.canary_receipt.v1"
	retrievalPromotionCanaryGovernanceContractID = "retrieval_intelligence_promotion_canary.v1"
	retrievalPromotionCanaryReceiptMaxBytes      = 512 * 1024
	retrievalPromotionMaxCandidates              = retrievalReceiptMaxCandidates
	retrievalPromotionMaxReasons                 = 8
	retrievalPromotionTotalBasisPoints           = 10000

	retrievalPromotionCanaryReceiptPathEnv    = "CONTEXTLATTICE_RETRIEVAL_PROMOTION_CANARY_RECEIPT_PATH"
	retrievalPromotionCanaryReceiptJSONEnv    = "CONTEXTLATTICE_RETRIEVAL_PROMOTION_CANARY_RECEIPT_JSON"
	retrievalPromotionCanaryLedgerPathEnv     = "CONTEXTLATTICE_RETRIEVAL_PROMOTION_CANARY_LEDGER_PATH"
	retrievalPromotionCanaryLedgerDefaultPath = ".data/orchestrator/retrieval_promotion_canary.ndjson"
	retrievalPromotionCanaryLedgerMaxBytes    = 2 * 1024 * 1024
	retrievalPromotionCanaryMaxBasisPoints    = 1000
	retrievalPromotionCanaryMinimumSamples    = 30
	retrievalPromotionCanaryMinimumDwell      = 15 * time.Minute
	retrievalPromotionCanaryMaximumAge        = 24 * time.Hour
	retrievalPromotionCanaryGovernancePath    = "/memory/retrieval/promotion/canary"
	retrievalPromotionCanaryGovernanceFeature = "retrieval_intelligence_promotion_canary"
)

type retrievalPromotionCandidateInput struct {
	CandidateRef string
	Occurrence   int
	Protected    bool
	Relevant     bool
}

type retrievalPromotionCohortWeights struct {
	ShadowBasisPoints  int
	ControlBasisPoints int
	CanaryBasisPoints  int
}

func (weights retrievalPromotionCohortWeights) valid() bool {
	return weights.ShadowBasisPoints >= 0 && weights.ControlBasisPoints >= 0 && weights.CanaryBasisPoints >= 0 &&
		weights.ShadowBasisPoints+weights.ControlBasisPoints+weights.CanaryBasisPoints == retrievalPromotionTotalBasisPoints
}

func (weights retrievalPromotionCohortWeights) mapValue() map[string]any {
	return map[string]any{
		"shadow_basis_points":  weights.ShadowBasisPoints,
		"control_basis_points": weights.ControlBasisPoints,
		"canary_basis_points":  weights.CanaryBasisPoints,
		"total_basis_points":   retrievalPromotionTotalBasisPoints,
	}
}

func retrievalPromotionWeightsFromCanaryPercent(percent int) retrievalPromotionCohortWeights {
	percent = clampInt(percent, 0, 100)
	return retrievalPromotionCohortWeights{
		ControlBasisPoints: retrievalPromotionTotalBasisPoints - percent*100,
		CanaryBasisPoints:  percent * 100,
	}
}

func retrievalPromotionWeightsFromMap(raw map[string]any) (retrievalPromotionCohortWeights, bool) {
	if len(raw) == 0 {
		return retrievalPromotionCohortWeights{}, false
	}
	read := func(keys ...string) (int, bool) {
		for _, key := range keys {
			value, present := raw[key]
			if !present {
				continue
			}
			parsed, ok := retrievalPromotionStrictInt(value)
			if !ok {
				return 0, false
			}
			return parsed, true
		}
		return 0, true
	}
	shadow, ok := read("shadow_basis_points", "shadow_weight_basis_points")
	if !ok {
		return retrievalPromotionCohortWeights{}, false
	}
	control, ok := read("control_basis_points", "control_weight_basis_points")
	if !ok {
		return retrievalPromotionCohortWeights{}, false
	}
	canary, ok := read("canary_basis_points", "canary_weight_basis_points", "treatment_basis_points")
	if !ok {
		return retrievalPromotionCohortWeights{}, false
	}
	weights := retrievalPromotionCohortWeights{ShadowBasisPoints: shadow, ControlBasisPoints: control, CanaryBasisPoints: canary}
	return weights, weights.valid()
}

func retrievalPromotionStrictInt(value any) (int, bool) {
	maxInt := int(^uint(0) >> 1)
	switch typed := value.(type) {
	case int:
		return typed, typed >= 0
	case int8:
		return int(typed), typed >= 0
	case int16:
		return int(typed), typed >= 0
	case int32:
		return int(typed), typed >= 0
	case int64:
		if typed < 0 || typed > int64(maxInt) {
			return 0, false
		}
		return int(typed), true
	case uint:
		if typed > uint(maxInt) {
			return 0, false
		}
		return int(typed), true
	case uint8:
		return int(typed), true
	case uint16:
		return int(typed), true
	case uint32:
		if uint64(typed) > uint64(maxInt) {
			return 0, false
		}
		return int(typed), true
	case uint64:
		if typed > uint64(maxInt) {
			return 0, false
		}
		return int(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return retrievalPromotionStrictInt(parsed)
	case float64:
		// float64(maxInt) rounds up to 2^63 on 64-bit platforms, so it
		// cannot be used as an inclusive upper bound. The first integer that
		// is not representable by int is exact in float64 on every supported
		// Go platform and is therefore a safe exclusive bound.
		maxIntExclusive := float64(uint64(maxInt) + 1)
		if math.IsNaN(typed) || math.IsInf(typed, 0) || math.Trunc(typed) != typed || typed < 0 || typed >= maxIntExclusive {
			return 0, false
		}
		converted := int(typed)
		if converted < 0 || float64(converted) != typed {
			return 0, false
		}
		return converted, true
	case float32:
		return retrievalPromotionStrictInt(float64(typed))
	default:
		return 0, false
	}
}

func retrievalPromotionCanonicalTimestamp(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && !parsed.IsZero()
}

// retrievalPromotionAssignmentSubjectRef is the one canonical assignment
// subject identity used by authorization, cohort allocation, and the signed
// canary receipt. A raw subject is accepted only as a compatibility input and
// is immediately converted to the same opaque reference used by the server.
func retrievalPromotionAssignmentSubjectRef(subject string) string {
	subject = strings.TrimSpace(subject)
	if isSearchIntelligenceFullSHA256Ref(subject) {
		return contextPackLearnedDigestRef(subject)
	}
	return contextPackLearnedScopeRef("assignment_subject", subject)
}

// retrievalPromotionStableCohort uses only the canonical opaque assignment
// subject reference and exact policy/snapshot references. Query shape, task
// prose, scores, and candidate content cannot influence assignment.
func retrievalPromotionStableCohort(subject, snapshotRef, policyRef string, weights retrievalPromotionCohortWeights) map[string]any {
	subject = retrievalPromotionAssignmentSubjectRef(subject)
	snapshotRef = contextPackLearnedDigestRef(snapshotRef)
	policyRef = contextPackLearnedDigestRef(policyRef)
	if subject == "" || snapshotRef == "" || policyRef == "" || !weights.valid() {
		return map[string]any{
			"status": "unavailable", "arm": "control", "reason": "stable_cohort_inputs_invalid",
			"weights": weights.mapValue(), "bucket_basis_points": -1,
		}
	}
	seed := strings.Join([]string{retrievalPromotionContractID, subject, snapshotRef, policyRef}, "\x00")
	bucketRaw, err := strconv.ParseUint(sha256Hex(seed)[:8], 16, 32)
	if err != nil {
		return map[string]any{
			"status": "unavailable", "arm": "control", "reason": "stable_cohort_assignment_unavailable",
			"weights": weights.mapValue(), "bucket_basis_points": -1,
		}
	}
	bucket := int(bucketRaw % retrievalPromotionTotalBasisPoints)
	arm := "control"
	if bucket < weights.ShadowBasisPoints {
		arm = "shadow"
	} else if bucket < weights.ShadowBasisPoints+weights.CanaryBasisPoints {
		arm = "canary"
	}
	return map[string]any{
		"status": "assigned", "arm": arm, "reason": "stable_opaque_subject_assignment",
		"assignment_basis": "sha256(assignment_subject_ref_snapshot_policy)", "bucket_basis_points": bucket,
		"weights":      weights.mapValue(),
		"subject_ref":  subject,
		"snapshot_ref": snapshotRef, "policy_ref": policyRef,
	}
}

func retrievalPromotionCandidateKey(candidateRef string, occurrence int) string {
	return strings.TrimSpace(candidateRef) + "\x00" + strconv.Itoa(occurrence)
}

func retrievalPromotionNormalizeCandidates(items []retrievalPromotionCandidateInput) []retrievalPromotionCandidateInput {
	seen := map[string]struct{}{}
	out := make([]retrievalPromotionCandidateInput, 0, minInt(len(items), retrievalPromotionMaxCandidates))
	for _, item := range items {
		item.CandidateRef = strings.TrimSpace(item.CandidateRef)
		if item.CandidateRef == "" || item.Occurrence < 1 {
			continue
		}
		key := retrievalPromotionCandidateKey(item.CandidateRef, item.Occurrence)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
		if len(out) >= retrievalPromotionMaxCandidates {
			break
		}
	}
	return out
}

// retrievalPromotionLossGuard compares identities, not scores.  A learned
// score may change presentation, but it may never remove a protected or
// relevant member of the same post-policy-safe union.
func retrievalPromotionLossGuard(control, treatment []retrievalPromotionCandidateInput) map[string]any {
	control = retrievalPromotionNormalizeCandidates(control)
	treatment = retrievalPromotionNormalizeCandidates(treatment)
	treatmentSet := make(map[string]retrievalPromotionCandidateInput, len(treatment))
	for _, item := range treatment {
		treatmentSet[retrievalPromotionCandidateKey(item.CandidateRef, item.Occurrence)] = item
	}
	controlSet := make(map[string]retrievalPromotionCandidateInput, len(control))
	for _, item := range control {
		controlSet[retrievalPromotionCandidateKey(item.CandidateRef, item.Occurrence)] = item
	}
	missing := []string{}
	protectedMissing := []string{}
	relevantMissing := []string{}
	for key, item := range controlSet {
		if _, present := treatmentSet[key]; present {
			continue
		}
		missing = append(missing, key)
		if item.Protected {
			protectedMissing = append(protectedMissing, key)
		}
		if item.Relevant {
			relevantMissing = append(relevantMissing, key)
		}
	}
	extra := []string{}
	for key := range treatmentSet {
		if _, present := controlSet[key]; !present {
			extra = append(extra, key)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	sort.Strings(protectedMissing)
	sort.Strings(relevantMissing)
	pass := len(missing) == 0 && len(extra) == 0 && len(control) == len(treatment)
	return map[string]any{
		"pass": pass, "control_count": len(control), "treatment_count": len(treatment),
		"missing_identities": missing, "unexpected_identities": extra,
		"protected_missing_identities": protectedMissing, "relevant_missing_identities": relevantMissing,
		"reason": map[bool]string{true: "safe_union_preserved", false: "safe_union_identity_loss"}[pass],
	}
}

func retrievalPromotionCandidateProjection(item retrievalPromotionCandidateInput, nativeRank, controlRank, selectedRank, omittedRank int, reasons []string, hardClass string) map[string]any {
	if len(reasons) > retrievalPromotionMaxReasons {
		reasons = reasons[:retrievalPromotionMaxReasons]
	}
	row := map[string]any{
		"candidate_ref": item.CandidateRef, "occurrence": item.Occurrence,
		"native_rank": nativeRank, "control_rank": controlRank,
		"selected_rank": selectedRank, "omitted_rank": omittedRank,
		"protected": item.Protected, "relevant": item.Relevant,
		"reasons": append([]string(nil), reasons...),
	}
	if hardClass != "" {
		row["authoritative_exclusion"] = true
		row["exclusion_class"] = hardClass
	}
	return row
}

func retrievalPromotionContextItemInput(item contextPackEvidenceItem) retrievalPromotionCandidateInput {
	protected := contextPackLearnedProtectedEvidence(item)
	// Every post-policy-safe item is relevant to the preservation guard.  This
	// is intentionally broader than lexical relevance: a request classifier or
	// learned score is never allowed to silently shrink the safe union.
	return retrievalPromotionCandidateInput{
		CandidateRef: item.CandidateID, Occurrence: item.Occurrence,
		Protected: protected, Relevant: true,
	}
}

func retrievalPromotionContextItemsProjection(items []contextPackEvidenceItem) []retrievalPromotionCandidateInput {
	out := make([]retrievalPromotionCandidateInput, 0, len(items))
	for _, item := range items {
		out = append(out, retrievalPromotionContextItemInput(item))
	}
	return retrievalPromotionNormalizeCandidates(out)
}

func retrievalPromotionNormalizeRef(value any) string {
	if ref := contextPackOpaqueCandidateRef(value); ref != "" {
		return ref
	}
	return ""
}

func retrievalPromotionNormalizeStringList(value any, limit int) ([]string, bool) {
	values := contextPackAnyList(value)
	if len(values) > limit {
		return nil, false
	}
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, raw := range values {
		text := strings.TrimSpace(anyToString(raw))
		if text == "" || len(text) > 120 || strings.ContainsAny(text, "\r\n\x00") {
			return nil, false
		}
		if _, duplicate := seen[text]; duplicate {
			continue
		}
		seen[text] = struct{}{}
		out = append(out, text)
	}
	return out, true
}

func retrievalPromotionNormalizeOrder(value any) ([]any, bool) {
	rows := contextPackAnyList(value)
	if len(rows) > retrievalPromotionMaxCandidates {
		return nil, false
	}
	out := make([]any, 0, len(rows))
	seen := map[string]struct{}{}
	for _, raw := range rows {
		row := anyMap(raw)
		ref := retrievalPromotionNormalizeRef(row["candidate_ref"])
		occurrence, occurrenceOK := retrievalPromotionStrictInt(row["occurrence"])
		order, orderOK := retrievalPromotionStrictInt(row["order"])
		if ref == "" || !occurrenceOK || occurrence < 1 || !orderOK || order != len(out)+1 || order > retrievalPromotionMaxCandidates {
			return nil, false
		}
		key := retrievalPromotionCandidateKey(ref, occurrence)
		if _, duplicate := seen[key]; duplicate {
			return nil, false
		}
		seen[key] = struct{}{}
		out = append(out, map[string]any{"candidate_ref": ref, "occurrence": occurrence, "order": order})
	}
	return out, true
}

func retrievalPromotionNormalizeCandidateUnion(raw map[string]any) (map[string]any, bool) {
	if len(raw) == 0 || !anyToBool(raw["bounded"]) || anyToInt(raw["maximum_candidates"], 0) != retrievalPromotionMaxCandidates {
		return nil, false
	}
	safeRows := contextPackAnyList(raw["candidates"])
	hardRows := contextPackAnyList(raw["hard_exclusions"])
	safeCount, safeCountOK := retrievalPromotionStrictInt(raw["safe_count"])
	hardCount, hardCountOK := retrievalPromotionStrictInt(raw["hard_excluded_count"])
	if !safeCountOK || !hardCountOK || safeCount != len(safeRows) || hardCount != len(hardRows) ||
		len(safeRows) > retrievalPromotionMaxCandidates || len(hardRows) > retrievalPromotionMaxCandidates {
		return nil, false
	}
	allKeys := map[string]struct{}{}
	normalizeRows := func(rows []any, hard bool) ([]any, bool) {
		out := make([]any, 0, len(rows))
		seen := map[string]struct{}{}
		for _, value := range rows {
			row := anyMap(value)
			ref := retrievalPromotionNormalizeRef(row["candidate_ref"])
			occurrence, occurrenceOK := retrievalPromotionStrictInt(row["occurrence"])
			if ref == "" || !occurrenceOK || occurrence < 1 {
				return nil, false
			}
			key := retrievalPromotionCandidateKey(ref, occurrence)
			if _, duplicate := seen[key]; duplicate {
				return nil, false
			}
			if _, duplicate := allKeys[key]; duplicate {
				return nil, false
			}
			seen[key] = struct{}{}
			allKeys[key] = struct{}{}
			protected, protectedOK := row["protected"].(bool)
			relevant, relevantOK := row["relevant"].(bool)
			if !protectedOK || !relevantOK {
				return nil, false
			}
			reasons, reasonsOK := retrievalPromotionNormalizeStringList(row["reasons"], retrievalPromotionMaxReasons)
			if !reasonsOK {
				return nil, false
			}
			projection := map[string]any{
				"candidate_ref": ref, "occurrence": occurrence,
				"native_rank": anyToInt(row["native_rank"], 0), "control_rank": anyToInt(row["control_rank"], 0),
				"selected_rank": anyToInt(row["selected_rank"], 0), "omitted_rank": anyToInt(row["omitted_rank"], 0),
				"protected": protected, "relevant": relevant, "reasons": reasons,
			}
			if hard {
				if !anyToBool(row["authoritative_exclusion"]) {
					return nil, false
				}
				class := strings.TrimSpace(anyToString(row["exclusion_class"]))
				if class == "" || len(class) > 80 {
					return nil, false
				}
				projection["authoritative_exclusion"] = true
				projection["exclusion_class"] = class
			} else if anyToBool(row["authoritative_exclusion"]) || strings.TrimSpace(anyToString(row["exclusion_class"])) != "" {
				return nil, false
			}
			out = append(out, projection)
		}
		return out, true
	}
	safe, safeOK := normalizeRows(safeRows, false)
	hard, hardOK := normalizeRows(hardRows, true)
	if !safeOK || !hardOK {
		return nil, false
	}
	return map[string]any{
		"bounded": true, "maximum_candidates": retrievalPromotionMaxCandidates,
		"safe_count": len(safe), "hard_excluded_count": len(hard),
		"candidates": safe, "hard_exclusions": hard,
	}, true
}

func retrievalPromotionOrderKeys(rows []any) ([]string, bool) {
	keys := make([]string, 0, len(rows))
	seen := map[string]struct{}{}
	for _, value := range rows {
		row := anyMap(value)
		ref := retrievalPromotionNormalizeRef(row["candidate_ref"])
		occurrence, ok := retrievalPromotionStrictInt(row["occurrence"])
		if ref == "" || !ok || occurrence < 1 {
			return nil, false
		}
		key := retrievalPromotionCandidateKey(ref, occurrence)
		if _, duplicate := seen[key]; duplicate {
			return nil, false
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys, true
}

func retrievalPromotionSameKeySequence(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func retrievalPromotionKeySubset(subset, superset []string) bool {
	allowed := map[string]struct{}{}
	for _, key := range superset {
		allowed[key] = struct{}{}
	}
	seen := map[string]struct{}{}
	for _, key := range subset {
		if _, ok := allowed[key]; !ok {
			return false
		}
		if _, duplicate := seen[key]; duplicate {
			return false
		}
		seen[key] = struct{}{}
	}
	return true
}

func retrievalPromotionOpaqueContinuationToken(value string) bool {
	value = strings.TrimSpace(value)
	return len(value) >= 12 && len(value) <= 512 &&
		!strings.ContainsAny(value, "\r\n\x00") &&
		(strings.HasPrefix(value, "sha256:") || strings.HasPrefix(value, "opaque-") || strings.HasPrefix(value, "continuation_"))
}

func retrievalPromotionCandidateInputsFromUnion(union map[string]any, order []string) ([]retrievalPromotionCandidateInput, bool) {
	rows := contextPackAnyList(union["candidates"])
	byKey := map[string]retrievalPromotionCandidateInput{}
	for _, rawRow := range rows {
		row := anyMap(rawRow)
		ref := retrievalPromotionNormalizeRef(row["candidate_ref"])
		occurrence, ok := retrievalPromotionStrictInt(row["occurrence"])
		protected, protectedOK := row["protected"].(bool)
		relevant, relevantOK := row["relevant"].(bool)
		if ref == "" || !ok || occurrence < 1 || !protectedOK || !relevantOK {
			return nil, false
		}
		byKey[retrievalPromotionCandidateKey(ref, occurrence)] = retrievalPromotionCandidateInput{CandidateRef: ref, Occurrence: occurrence, Protected: protected, Relevant: relevant}
	}
	out := make([]retrievalPromotionCandidateInput, 0, len(order))
	for _, key := range order {
		item, ok := byKey[key]
		if !ok {
			return nil, false
		}
		out = append(out, item)
	}
	if len(out) != len(byKey) {
		return nil, false
	}
	return out, true
}

// retrievalPromotionNormalizeReceipt is the privacy boundary for the
// promotion envelope carried in a durable selection receipt. It validates the
// complete identity/rank/partition contract before any outcome can credit the
// learned arm.
func retrievalPromotionNormalizeReceipt(raw map[string]any) map[string]any {
	if len(raw) == 0 || anyToString(raw["schema_id"]) != retrievalPromotionContractID || anyToInt(raw["version"], 0) != retrievalPromotionVersion {
		return nil
	}
	status := strings.TrimSpace(anyToString(raw["status"]))
	if status != "native_control" && status != "bounded_learned_presentation" && status != "shadow_only" {
		return nil
	}
	union, unionOK := retrievalPromotionNormalizeCandidateUnion(anyMap(raw["candidate_union"]))
	if !unionOK {
		return nil
	}
	ordering := anyMap(raw["ordering"])
	nativeOrder, nativeOK := retrievalPromotionNormalizeOrder(ordering["native_order"])
	controlOrder, controlOK := retrievalPromotionNormalizeOrder(ordering["control_order"])
	controlSelected, controlSelectedOK := retrievalPromotionNormalizeOrder(ordering["control_selected_order"])
	selectedOrder, selectedOK := retrievalPromotionNormalizeOrder(ordering["selected_order"])
	omittedOrder, omittedOK := retrievalPromotionNormalizeOrder(ordering["omitted_order"])
	if !nativeOK || !controlOK || !controlSelectedOK || !selectedOK || !omittedOK {
		return nil
	}
	nativeKeys, nativeKeysOK := retrievalPromotionOrderKeys(nativeOrder)
	controlKeys, controlKeysOK := retrievalPromotionOrderKeys(controlOrder)
	controlSelectedKeys, controlSelectedKeysOK := retrievalPromotionOrderKeys(controlSelected)
	selectedKeys, selectedKeysOK := retrievalPromotionOrderKeys(selectedOrder)
	omittedKeys, omittedKeysOK := retrievalPromotionOrderKeys(omittedOrder)
	if !nativeKeysOK || !controlKeysOK || !controlSelectedKeysOK || !selectedKeysOK || !omittedKeysOK ||
		!retrievalPromotionSameKeySequence(nativeKeys, controlKeys) {
		return nil
	}
	unionRows := contextPackAnyList(union["candidates"])
	unionKeys, unionKeysOK := retrievalPromotionOrderKeys(unionRows)
	if !unionKeysOK || !retrievalPromotionSameKeySet(nativeKeys, unionKeys) || !retrievalPromotionKeySubset(controlSelectedKeys, nativeKeys) {
		return nil
	}
	selectedAndOmitted := append(append([]string{}, selectedKeys...), omittedKeys...)
	if !retrievalPromotionSameKeySet(selectedAndOmitted, unionKeys) || len(selectedAndOmitted) != len(unionKeys) {
		return nil
	}
	// Every normalized rank is derived from the corresponding order array;
	// trusting a scalar rank would permit an equal-length identity swap.
	rankFor := func(rows []any) map[string]int {
		result := map[string]int{}
		for index, value := range rows {
			row := anyMap(value)
			ref := retrievalPromotionNormalizeRef(row["candidate_ref"])
			occurrence, _ := retrievalPromotionStrictInt(row["occurrence"])
			result[retrievalPromotionCandidateKey(ref, occurrence)] = index + 1
		}
		return result
	}
	nativeRank, controlRank := rankFor(nativeOrder), rankFor(controlOrder)
	selectedRank, omittedRank := rankFor(selectedOrder), rankFor(omittedOrder)
	for _, rawRow := range unionRows {
		row := anyMap(rawRow)
		ref := retrievalPromotionNormalizeRef(row["candidate_ref"])
		occurrence, occurrenceOK := retrievalPromotionStrictInt(row["occurrence"])
		if ref == "" || !occurrenceOK {
			return nil
		}
		key := retrievalPromotionCandidateKey(ref, occurrence)
		for field, expected := range map[string]int{"native_rank": nativeRank[key], "control_rank": controlRank[key], "selected_rank": selectedRank[key], "omitted_rank": omittedRank[key]} {
			actual, actualOK := retrievalPromotionStrictInt(row[field])
			if !actualOK || actual != expected {
				return nil
			}
		}
	}
	for _, rawRow := range contextPackAnyList(union["hard_exclusions"]) {
		row := anyMap(rawRow)
		for _, field := range []string{"native_rank", "control_rank", "selected_rank", "omitted_rank"} {
			value, ok := retrievalPromotionStrictInt(row[field])
			if !ok || value != 0 {
				return nil
			}
		}
	}
	loss := anyMap(raw["loss_guard"])
	lossPass, lossPassOK := loss["pass"].(bool)
	if !lossPassOK {
		return nil
	}
	lossReason := strings.TrimSpace(anyToString(loss["reason"]))
	if lossReason == "" || len(lossReason) > 120 {
		return nil
	}
	normalizeKeyList := func(value any) ([]string, bool) {
		values := contextPackAnyList(value)
		if len(values) > retrievalPromotionMaxCandidates {
			return nil, false
		}
		out := make([]string, 0, len(values))
		seen := map[string]struct{}{}
		for _, rawKey := range values {
			key := strings.TrimSpace(anyToString(rawKey))
			if key == "" || len(key) > 200 || strings.ContainsAny(key, "\r\n\x00") {
				return nil, false
			}
			if _, duplicate := seen[key]; duplicate {
				return nil, false
			}
			seen[key] = struct{}{}
			out = append(out, key)
		}
		return out, true
	}
	missing, missingOK := normalizeKeyList(loss["missing_identities"])
	extra, extraOK := normalizeKeyList(loss["unexpected_identities"])
	protectedMissing, protectedOK := normalizeKeyList(loss["protected_missing_identities"])
	relevantMissing, relevantOK := normalizeKeyList(loss["relevant_missing_identities"])
	controlCount, controlCountOK := retrievalPromotionStrictInt(loss["control_count"])
	treatmentCount, treatmentCountOK := retrievalPromotionStrictInt(loss["treatment_count"])
	if !missingOK || !extraOK || !protectedOK || !relevantOK || !controlCountOK || !treatmentCountOK || controlCount != len(controlKeys) || treatmentCount != len(selectedAndOmitted) {
		return nil
	}
	controlInputs, controlInputsOK := retrievalPromotionCandidateInputsFromUnion(union, controlKeys)
	treatmentInputs, treatmentInputsOK := retrievalPromotionCandidateInputsFromUnion(union, selectedAndOmitted)
	if !controlInputsOK || !treatmentInputsOK {
		return nil
	}
	computedLoss := retrievalPromotionLossGuard(controlInputs, treatmentInputs)
	computedMissing, _ := normalizeKeyList(computedLoss["missing_identities"])
	computedExtra, _ := normalizeKeyList(computedLoss["unexpected_identities"])
	computedProtected, _ := normalizeKeyList(computedLoss["protected_missing_identities"])
	computedRelevant, _ := normalizeKeyList(computedLoss["relevant_missing_identities"])
	if lossPass != anyToBool(computedLoss["pass"]) || lossReason != anyToString(computedLoss["reason"]) ||
		!stringSlicesEqual(missing, computedMissing) || !stringSlicesEqual(extra, computedExtra) ||
		!stringSlicesEqual(protectedMissing, computedProtected) || !stringSlicesEqual(relevantMissing, computedRelevant) {
		return nil
	}
	omission := anyMap(raw["omission_receipt"])
	omittedCount, omittedCountOK := retrievalPromotionStrictInt(omission["presentation_omitted_count"])
	omittedRefs, omittedRefsOK := retrievalPromotionNormalizeOrder(omission["presentation_omitted_refs"])
	if !omittedCountOK || omittedCount != len(omittedOrder) || !omittedRefsOK || !retrievalPromotionSameKeySequence(mustRetrievalPromotionOrderKeys(omittedRefs), omittedKeys) || !anyToBool(omission["hard_exclusions_separate"]) {
		return nil
	}
	continuation := anyMap(raw["continuation_receipt"])
	available, availableOK := continuation["available"].(bool)
	durable, durableOK := continuation["durable"].(bool)
	continuationOmitted, continuationOmittedOK := retrievalPromotionStrictInt(continuation["omitted_count"])
	token := strings.TrimSpace(anyToString(continuation["token"]))
	if !availableOK || !durableOK || !continuationOmittedOK || continuationOmitted < 0 || continuationOmitted > retrievalPromotionMaxCandidates ||
		(available && (!durable || !retrievalPromotionOpaqueContinuationToken(token))) || (!available && (durable || token != "" || continuationOmitted != 0)) {
		return nil
	}
	cohort := anyMap(raw["cohort"])
	cohortStatus := strings.TrimSpace(anyToString(cohort["status"]))
	if cohortStatus == "" || len(cohortStatus) > 80 {
		return nil
	}
	weights, weightsOK := retrievalPromotionWeightsFromMap(anyMap(cohort["weights"]))
	if !weightsOK {
		return nil
	}
	if cohortStatus == "assigned" && (!isSearchIntelligenceFullSHA256Ref(anyToString(cohort["subject_ref"])) || !isSearchIntelligenceFullSHA256Ref(anyToString(cohort["snapshot_ref"])) || !isSearchIntelligenceFullSHA256Ref(anyToString(cohort["policy_ref"]))) {
		return nil
	}
	preserved, preservedOK := raw["native_order_preserved"].(bool)
	if !preservedOK || preserved != retrievalPromotionSameKeySequence(nativeKeys, selectedAndOmitted) {
		return nil
	}
	cohortOut := map[string]any{"status": cohortStatus, "arm": anyToString(cohort["arm"]), "weights": weights.mapValue()}
	if cohortStatus == "assigned" {
		arm := anyToString(cohort["arm"])
		bucket, bucketOK := retrievalPromotionStrictInt(cohort["bucket_basis_points"])
		if (arm != "shadow" && arm != "control" && arm != "canary") || !bucketOK || bucket >= retrievalPromotionTotalBasisPoints {
			return nil
		}
		cohortOut["bucket_basis_points"] = bucket
	}
	for _, key := range []string{"subject_ref", "snapshot_ref", "policy_ref"} {
		if value := strings.TrimSpace(anyToString(cohort[key])); value != "" {
			if !isSearchIntelligenceFullSHA256Ref(value) {
				return nil
			}
			cohortOut[key] = value
		}
	}
	if caseSetRef := strings.TrimSpace(anyToString(cohort["case_set_ref"])); caseSetRef != "" {
		if !isSearchIntelligenceFullSHA256Ref(caseSetRef) {
			return nil
		}
		cohortOut["case_set_ref"] = caseSetRef
	}
	// Preserve only the bounded, opaque signed-ledger snapshot needed by the
	// production evidence seam. The raw activation envelope is never copied.
	// A normalized receipt is itself persisted inside the quality selection
	// receipt, so its already-normalized cohort snapshot must be accepted on a
	// later read as well. The exact signed owner-ledger check remains at the
	// production authority seam; this branch only preserves the bounded shape.
	promotionEvidence := anyMap(anyMap(raw["activation"])["promotion_evidence"])
	canarySnapshot := anyMap(promotionEvidence["canary_receipt"])
	if len(canarySnapshot) == 0 {
		canarySnapshot = anyMap(cohort["canary_receipt"])
	}
	if len(canarySnapshot) > 0 {
		generation, generationOK := retrievalPromotionStrictInt(canarySnapshot["generation"])
		canaryDigest := contextPackLearnedDigestRef(anyToString(canarySnapshot["receipt_digest"]))
		workspaceRef := contextPackLearnedDigestRef(anyToString(canarySnapshot["workspace_ref"]))
		projectRef := contextPackLearnedDigestRef(anyToString(canarySnapshot["project_ref"]))
		taskClassRef := contextPackLearnedDigestRef(anyToString(canarySnapshot["task_class_ref"]))
		retrievalIntentRef := contextPackLearnedDigestRef(anyToString(canarySnapshot["retrieval_intent_ref"]))
		policyRef := contextPackLearnedDigestRef(anyToString(canarySnapshot["policy_ref"]))
		recordedAt := strings.TrimSpace(anyToString(canarySnapshot["recorded_at"]))
		expiresAt := strings.TrimSpace(anyToString(canarySnapshot["expires_at"]))
		snapshotRef := contextPackLearnedDigestRef(anyToString(canarySnapshot["snapshot_ref"]))
		caseSetRef := contextPackLearnedDigestRef(anyToString(canarySnapshot["case_set_ref"]))
		assignmentRef := contextPackLearnedDigestRef(anyToString(canarySnapshot["assignment_subject_ref"]))
		shadowBasisPoints, shadowBasisPointsOK := retrievalPromotionStrictInt(canarySnapshot["shadow_basis_points"])
		controlBasisPoints, controlBasisPointsOK := retrievalPromotionStrictInt(canarySnapshot["control_basis_points"])
		canaryBasisPoints, canaryBasisPointsOK := retrievalPromotionStrictInt(canarySnapshot["canary_basis_points"])
		minimumSamples, minimumSamplesOK := retrievalPromotionStrictInt(canarySnapshot["minimum_canary_samples"])
		minimumDwell, minimumDwellOK := retrievalPromotionStrictInt(canarySnapshot["minimum_dwell_seconds"])
		if !generationOK || generation < 1 || canaryDigest == "" || canaryDigest != contextPackLearnedDigestRef(anyToString(cohort["receipt_digest"])) ||
			workspaceRef == "" || projectRef == "" || taskClassRef == "" || retrievalIntentRef == "" || policyRef == "" ||
			!retrievalPromotionCanonicalTimestamp(recordedAt) || !retrievalPromotionCanonicalTimestamp(expiresAt) || snapshotRef == "" || caseSetRef == "" || assignmentRef == "" ||
			snapshotRef != contextPackLearnedDigestRef(anyToString(cohort["snapshot_ref"])) ||
			caseSetRef != contextPackLearnedDigestRef(anyToString(cohort["case_set_ref"])) ||
			assignmentRef != contextPackLearnedDigestRef(anyToString(cohort["subject_ref"])) || policyRef != contextPackLearnedDigestRef(anyToString(cohort["policy_ref"])) ||
			!shadowBasisPointsOK || !controlBasisPointsOK || !canaryBasisPointsOK || shadowBasisPoints != weights.ShadowBasisPoints ||
			controlBasisPoints != weights.ControlBasisPoints || canaryBasisPoints != weights.CanaryBasisPoints ||
			!minimumSamplesOK || minimumSamples < retrievalPromotionCanaryMinimumSamples || !minimumDwellOK || minimumDwell < int(retrievalPromotionCanaryMinimumDwell/time.Second) {
			return nil
		}
		cohortOut["canary_receipt"] = map[string]any{
			"generation": generation, "workspace_ref": workspaceRef, "project_ref": projectRef,
			"task_class_ref": taskClassRef, "retrieval_intent_ref": retrievalIntentRef, "policy_ref": policyRef,
			"snapshot_ref": snapshotRef, "case_set_ref": caseSetRef, "assignment_subject_ref": assignmentRef,
			"shadow_basis_points": shadowBasisPoints, "control_basis_points": controlBasisPoints,
			"canary_basis_points": canaryBasisPoints, "minimum_canary_samples": minimumSamples, "minimum_dwell_seconds": minimumDwell,
			"recorded_at": recordedAt, "expires_at": expiresAt, "receipt_digest": canaryDigest,
		}
	}
	out := map[string]any{
		"schema_id": retrievalPromotionContractID, "version": retrievalPromotionVersion,
		"status": status, "learned_ordering_applied": anyToBool(raw["learned_ordering_applied"]),
		"native_order_preserved": preserved,
		"candidate_union":        union,
		"ordering": map[string]any{
			"native_order": nativeOrder, "control_order": controlOrder,
			"control_selected_order": controlSelected, "selected_order": selectedOrder, "omitted_order": omittedOrder,
		},
		"loss_guard": map[string]any{
			"pass": lossPass, "reason": lossReason, "control_count": controlCount, "treatment_count": treatmentCount,
			"missing_identities": missing, "unexpected_identities": extra,
			"protected_missing_identities": protectedMissing, "relevant_missing_identities": relevantMissing,
		},
		"omission_receipt": map[string]any{
			"presentation_omitted_count": omittedCount, "presentation_omitted_refs": omittedRefs, "hard_exclusions_separate": true,
		},
		"continuation_receipt": map[string]any{
			"available": available, "durable": durable, "omitted_count": continuationOmitted,
		},
		"cohort":    cohortOut,
		"redaction": map[string]any{"raw_query_or_content_stored": false, "raw_paths_stored": false},
	}
	if token != "" {
		out["continuation_receipt"].(map[string]any)["token"] = token
	}
	if receiptDigest := contextPackLearnedDigestRef(anyToString(cohort["receipt_digest"])); receiptDigest != "" {
		out["cohort"].(map[string]any)["receipt_digest"] = receiptDigest
	}
	return out
}

func retrievalPromotionSameKeySet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	seen := map[string]struct{}{}
	for _, key := range left {
		seen[key] = struct{}{}
	}
	for _, key := range right {
		if _, ok := seen[key]; !ok {
			return false
		}
	}
	return true
}

func mustRetrievalPromotionOrderKeys(rows []any) []string {
	keys, _ := retrievalPromotionOrderKeys(rows)
	return keys
}

func retrievalPromotionHardDecisionClass(decision retrievalDecisionRecord) string {
	switch decision.Decision {
	case "quarantined":
		return "quarantine"
	case "omitted":
		return "temporal_or_policy"
	case "deduplicated":
		return "duplicate_content"
	default:
		return ""
	}
}

func retrievalPromotionDecisionKey(decision retrievalDecisionRecord) string {
	return retrievalPromotionCandidateKey(decision.CandidateID, decision.Occurrence)
}

func retrievalPromotionContextItemKeys(items []contextPackEvidenceItem) []string {
	keys := make([]string, 0, minInt(len(items), retrievalPromotionMaxCandidates))
	seen := map[string]struct{}{}
	for _, item := range items {
		key := retrievalPromotionCandidateKey(item.CandidateID, item.Occurrence)
		if strings.TrimSpace(item.CandidateID) == "" || item.Occurrence < 1 {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
		if len(keys) >= retrievalPromotionMaxCandidates {
			break
		}
	}
	return keys
}

func retrievalPromotionContextOrderPreserved(native, treatment []contextPackEvidenceItem) bool {
	left, right := retrievalPromotionContextItemKeys(native), retrievalPromotionContextItemKeys(treatment)
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// retrievalPromotionBuildContextEnvelope retains the complete bounded
// post-policy-safe union while keeping hard policy exclusions separate from
// presentation omission.  It is safe to persist in the opaque selection
// receipt: no text, path, or query is copied.
func retrievalPromotionBuildContextEnvelope(
	trust retrievalTrustResult,
	nativeEligible, nativeSelected, nativeOmitted []contextPackEvidenceItem,
	treatmentEligible, treatmentSelected, treatmentOmitted []contextPackEvidenceItem,
	tokenBudget contextPackTokenBudget,
	inputBoundary map[string]any,
	learned contextPackLearnedActivationDecision,
) map[string]any {
	controlInputs := retrievalPromotionContextItemsProjection(nativeEligible)
	treatmentInputs := retrievalPromotionContextItemsProjection(treatmentEligible)
	lossGuard := retrievalPromotionLossGuard(controlInputs, treatmentInputs)
	nativeIndex := map[string]int{}
	controlIndex := map[string]int{}
	controlSelectedSet := map[string]struct{}{}
	selectedIndex := map[string]int{}
	omittedIndex := map[string]int{}
	for index, item := range nativeEligible {
		nativeIndex[retrievalPromotionCandidateKey(item.CandidateID, item.Occurrence)] = index + 1
		controlIndex[retrievalPromotionCandidateKey(item.CandidateID, item.Occurrence)] = index + 1
	}
	for _, item := range nativeSelected {
		controlSelectedSet[retrievalPromotionCandidateKey(item.CandidateID, item.Occurrence)] = struct{}{}
	}
	for index, item := range treatmentSelected {
		selectedIndex[retrievalPromotionCandidateKey(item.CandidateID, item.Occurrence)] = index + 1
	}
	for index, item := range treatmentOmitted {
		omittedIndex[retrievalPromotionCandidateKey(item.CandidateID, item.Occurrence)] = index + 1
	}
	byKey := map[string]retrievalPromotionCandidateInput{}
	for _, item := range controlInputs {
		byKey[retrievalPromotionCandidateKey(item.CandidateRef, item.Occurrence)] = item
	}
	for _, item := range treatmentInputs {
		byKey[retrievalPromotionCandidateKey(item.CandidateRef, item.Occurrence)] = item
	}
	candidates := make([]any, 0, minInt(len(byKey), retrievalPromotionMaxCandidates))
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		left, right := byKey[keys[i]], byKey[keys[j]]
		leftRank, rightRank := nativeIndex[keys[i]], nativeIndex[keys[j]]
		if leftRank == 0 {
			leftRank = retrievalPromotionMaxCandidates + left.Occurrence
		}
		if rightRank == 0 {
			rightRank = retrievalPromotionMaxCandidates + right.Occurrence
		}
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		return left.CandidateRef < right.CandidateRef
	})
	for _, key := range keys[:minInt(len(keys), retrievalPromotionMaxCandidates)] {
		item := byKey[key]
		reasons := []string{"post_policy_safe_union_member", "native_control_candidate"}
		if _, controlSelected := controlSelectedSet[key]; controlSelected {
			reasons = append(reasons, "control_selected")
		} else {
			reasons = append(reasons, "control_presentation_omission")
		}
		if selectedIndex[key] > 0 {
			reasons = append(reasons, "selected_order_recorded")
		}
		if omittedIndex[key] > 0 {
			reasons = append(reasons, "presentation_omission")
		}
		candidates = append(candidates, retrievalPromotionCandidateProjection(item, nativeIndex[key], controlIndex[key], selectedIndex[key], omittedIndex[key], reasons, ""))
	}
	excluded := make([]any, 0, minInt(len(trust.PreDecisions), retrievalPromotionMaxCandidates))
	for _, decision := range trust.PreDecisions {
		if len(excluded) >= retrievalPromotionMaxCandidates {
			break
		}
		if strings.TrimSpace(decision.CandidateID) == "" || decision.Occurrence < 1 {
			continue
		}
		reason := append([]string{"authoritative_policy_decision"}, decision.Reasons...)
		input := retrievalPromotionCandidateInput{CandidateRef: decision.CandidateID, Occurrence: decision.Occurrence, Relevant: false}
		excluded = append(excluded, retrievalPromotionCandidateProjection(input, 0, 0, 0, 0, reason, retrievalPromotionHardDecisionClass(decision)))
	}
	boundary := cloneJSONMap(inputBoundary)
	if len(boundary) == 0 {
		boundary = cloneJSONMap(anyMap(trust.TrustEnvelope["input_boundary"]))
	}
	if len(boundary) == 0 {
		boundary = map[string]any{"truncated": trust.InputTruncatedCount > 0, "omitted_count": trust.InputTruncatedCount}
	}
	continuation := map[string]any{
		"available":     anyToBool(boundary["continuation_available"]) || anyToBool(boundary["continuation_durable"]),
		"durable":       anyToBool(boundary["continuation_durable"]),
		"token":         anyToString(boundary["continuation_token"]),
		"omitted_count": maxInt(0, anyToInt(boundary["omitted_count"], 0)),
	}
	if sources := contextPackAnyList(boundary["continuation_sources"]); len(sources) > 0 {
		continuation["sources"] = append([]any(nil), sources...)
	}
	cohort := map[string]any{
		"status": "unavailable", "arm": "control", "reason": "signed_canary_cohort_unavailable",
		"weights": retrievalPromotionWeightsFromCanaryPercent(0).mapValue(), "bucket_basis_points": -1,
	}
	if learned.Promotion != nil {
		if signedCohort := anyMap(learned.Promotion["cohort"]); len(signedCohort) > 0 {
			cohort = cloneJSONMap(signedCohort)
		}
	}
	presentation := "native_control"
	if learned.Performed && anyToBool(lossGuard["pass"]) {
		presentation = "bounded_learned_presentation"
	}
	activation := map[string]any{
		"eligible": learned.Eligible, "assigned_treatment": learned.AssignedTreatment,
		"performed": learned.Performed, "reason": learned.Reason,
		"canary_receipt_required": true, "canary_receipt_verified": len(learned.Promotion) > 0,
	}
	if len(learned.Promotion) > 0 {
		activation["promotion_evidence"] = cloneJSONMap(learned.Promotion)
	}
	presentationOrder := append([]contextPackEvidenceItem(nil), treatmentSelected...)
	presentationOrder = append(presentationOrder, treatmentOmitted...)
	return map[string]any{
		"schema_id": retrievalPromotionContractID, "version": retrievalPromotionVersion,
		"status": presentation, "learned_ordering_applied": presentation == "bounded_learned_presentation",
		"native_order_preserved": retrievalPromotionContextOrderPreserved(nativeEligible, presentationOrder),
		"candidate_union": map[string]any{
			"bounded": true, "maximum_candidates": retrievalPromotionMaxCandidates,
			"safe_count": len(candidates), "hard_excluded_count": len(excluded),
			"candidates": candidates, "hard_exclusions": excluded,
		},
		"ordering": map[string]any{
			"native_order":           retrievalPromotionOrderRefs(nativeEligible),
			"control_order":          retrievalPromotionOrderRefs(nativeEligible),
			"control_selected_order": retrievalPromotionOrderRefs(nativeSelected),
			"selected_order":         retrievalPromotionOrderRefs(treatmentSelected),
			"omitted_order":          retrievalPromotionOrderRefs(treatmentOmitted),
		},
		"loss_guard": lossGuard,
		"omission_receipt": map[string]any{
			"presentation_omitted_count": len(treatmentOmitted),
			"presentation_omitted_refs":  retrievalPromotionOrderRefs(treatmentOmitted),
			"hard_exclusions_separate":   true,
			"token_budget_active":        tokenBudget.Active,
			"compression_level":          contextPackPromotionCompressionLevel(tokenBudget, treatmentOmitted),
		},
		"continuation_receipt": continuation,
		"cohort":               cohort,
		"activation":           activation,
		"redaction":            map[string]any{"raw_query_or_content_stored": false, "raw_paths_stored": false},
	}
}

func retrievalPromotionOrderRefs(items []contextPackEvidenceItem) []any {
	limit := minInt(len(items), retrievalPromotionMaxCandidates)
	out := make([]any, 0, limit)
	for index := 0; index < limit; index++ {
		item := items[index]
		out = append(out, map[string]any{"candidate_ref": item.CandidateID, "occurrence": item.Occurrence, "order": index + 1})
	}
	return out
}

func contextPackPromotionCompressionLevel(tokenBudget contextPackTokenBudget, omitted []contextPackEvidenceItem) string {
	if len(omitted) == 0 {
		return "none"
	}
	if tokenBudget.Active {
		return "token_budget_or_marginal_value"
	}
	return "candidate_limit"
}

func retrievalPromotionSearchRowHardClass(row map[string]any) string {
	trust, ok := row["gateway_trust_assessment"].(searchIntelligenceGatewayTrustEnvelope)
	if ok && trust.quarantined {
		return "quarantine"
	}
	return ""
}

func retrievalPromotionSearchRowPolicySafe(row map[string]any) bool {
	trust, ok := row["gateway_trust_assessment"].(searchIntelligenceGatewayTrustEnvelope)
	return ok && !trust.quarantined
}

func retrievalPromotionSearchEnvelope(input searchIntelligenceInput) map[string]any {
	rows := input.AllMerged
	if len(rows) > retrievalPromotionMaxCandidates {
		rows = rows[:retrievalPromotionMaxCandidates]
	}
	native := make([]any, 0, len(rows))
	union := make([]any, 0, len(rows))
	hardExcluded := make([]any, 0)
	policySafeCount := 0
	safeLiteral := make([]map[string]any, 0, len(input.Literal))
	seen := map[string]struct{}{}
	for _, row := range rows {
		identity := searchIntelligenceCandidateIdentity(row)
		if _, duplicate := seen[identity.CandidateRef]; duplicate {
			continue
		}
		class := retrievalPromotionSearchRowHardClass(row)
		policySafe := retrievalPromotionSearchRowPolicySafe(row)
		if policySafe {
			policySafeCount++
		}
		seen[identity.CandidateRef] = struct{}{}
		reasons := []string{"native_literal_candidate"}
		if policySafe {
			reasons = append(reasons, "post_policy_safe_union_member")
		} else {
			reasons = append(reasons, "policy_safety_unverified_native_candidate")
		}
		projection := map[string]any{
			"candidate_ref": identity.CandidateRef, "occurrence": 1,
			"native_rank": 0, "control_rank": 0, "selected_rank": 0, "omitted_rank": 0,
			"relevant": true, "protected": false, "policy_safe": policySafe,
			"reasons": reasons,
		}
		if class != "" {
			projection["authoritative_exclusion"] = true
			projection["exclusion_class"] = class
			hardExcluded = append(hardExcluded, projection)
			continue
		}
		safeRank := len(native) + 1
		projection["native_rank"] = safeRank
		projection["control_rank"] = safeRank
		native = append(native, map[string]any{"candidate_ref": identity.CandidateRef, "occurrence": 1, "order": safeRank})
		union = append(union, projection)
	}
	for _, row := range input.Literal {
		if retrievalPromotionSearchRowHardClass(row) == "" {
			safeLiteral = append(safeLiteral, row)
		}
	}
	safeSet := map[string]struct{}{}
	for _, raw := range native {
		row := anyMap(raw)
		safeSet[retrievalPromotionCandidateKey(anyToString(row["candidate_ref"]), anyToInt(row["occurrence"], 1))] = struct{}{}
	}
	selectedOrder := make([]any, 0, len(safeLiteral))
	selectedSet := map[string]struct{}{}
	for _, row := range safeLiteral {
		identity := searchIntelligenceCandidateIdentity(row)
		key := retrievalPromotionCandidateKey(identity.CandidateRef, 1)
		if _, inSafeUnion := safeSet[key]; !inSafeUnion {
			continue
		}
		if _, duplicate := selectedSet[key]; duplicate {
			continue
		}
		selectedSet[key] = struct{}{}
		selectedOrder = append(selectedOrder, map[string]any{"candidate_ref": identity.CandidateRef, "occurrence": 1, "order": len(selectedOrder) + 1})
	}
	omittedOrder := make([]any, 0, len(native)-len(selectedOrder))
	for _, raw := range native {
		row := anyMap(raw)
		key := retrievalPromotionCandidateKey(anyToString(row["candidate_ref"]), anyToInt(row["occurrence"], 1))
		if _, selected := selectedSet[key]; selected {
			continue
		}
		omittedOrder = append(omittedOrder, map[string]any{"candidate_ref": anyToString(row["candidate_ref"]), "occurrence": 1, "order": len(omittedOrder) + 1})
	}
	selectedRanks, omittedRanks := map[string]int{}, map[string]int{}
	for index, raw := range selectedOrder {
		row := anyMap(raw)
		selectedRanks[retrievalPromotionCandidateKey(anyToString(row["candidate_ref"]), 1)] = index + 1
	}
	for index, raw := range omittedOrder {
		row := anyMap(raw)
		omittedRanks[retrievalPromotionCandidateKey(anyToString(row["candidate_ref"]), 1)] = index + 1
	}
	for _, raw := range union {
		row := anyMap(raw)
		key := retrievalPromotionCandidateKey(anyToString(row["candidate_ref"]), anyToInt(row["occurrence"], 1))
		row["selected_rank"] = selectedRanks[key]
		row["omitted_rank"] = omittedRanks[key]
	}
	selectedAndOmitted := make([]string, 0, len(native))
	for _, raw := range selectedOrder {
		row := anyMap(raw)
		selectedAndOmitted = append(selectedAndOmitted, retrievalPromotionCandidateKey(anyToString(row["candidate_ref"]), 1))
	}
	for _, raw := range omittedOrder {
		row := anyMap(raw)
		selectedAndOmitted = append(selectedAndOmitted, retrievalPromotionCandidateKey(anyToString(row["candidate_ref"]), 1))
	}
	nativeKeys := make([]string, 0, len(native))
	for _, raw := range native {
		row := anyMap(raw)
		nativeKeys = append(nativeKeys, retrievalPromotionCandidateKey(anyToString(row["candidate_ref"]), 1))
	}
	preserved := retrievalPromotionSameKeySequence(nativeKeys, selectedAndOmitted)
	return map[string]any{
		"schema_id": retrievalPromotionContractID, "version": retrievalPromotionVersion,
		"status": "shadow_only", "learned_ordering_applied": false,
		"cohort":                 retrievalPromotionStableCohort("", "", "", retrievalPromotionWeightsFromCanaryPercent(0)),
		"safe_union_status":      map[bool]string{true: "post_policy_safe", false: "policy_safety_unverified"}[policySafeCount == len(union)],
		"safe_union_verified":    policySafeCount == len(union),
		"native_order_preserved": preserved,
		"candidate_union": map[string]any{
			"bounded": true, "maximum_candidates": retrievalPromotionMaxCandidates,
			"input_truncated_count": maxInt(0, len(input.AllMerged)-len(rows)), "safe_count": len(union),
			"policy_safe_count": policySafeCount,
			"candidates":        union, "hard_exclusions": hardExcluded, "hard_excluded_count": len(hardExcluded),
		},
		"ordering": map[string]any{
			"native_order": native, "control_order": native, "control_selected_order": native,
			"selected_order": selectedOrder, "omitted_order": omittedOrder,
		},
		"loss_guard": map[string]any{"pass": true, "control_count": len(native), "treatment_count": len(native), "missing_identities": []string{}, "unexpected_identities": []string{}, "protected_missing_identities": []string{}, "relevant_missing_identities": []string{}, "reason": "safe_union_preserved"},
		"activation": map[string]any{
			"performed": false, "canary_receipt_required": true,
			"reason": "search_intelligence_shadow_only",
		},
		"omission_receipt":     map[string]any{"presentation_omitted_count": len(omittedOrder), "presentation_omitted_refs": omittedOrder, "hard_exclusions_separate": true},
		"continuation_receipt": map[string]any{"available": false, "durable": false, "omitted_count": 0},
		"redaction":            map[string]any{"raw_query_or_content_stored": false, "raw_paths_stored": false},
	}
}

func searchIntelligenceNativeRank(candidateRef string, literal []map[string]any) int {
	for index, row := range literal {
		if searchIntelligenceCandidateIdentity(row).CandidateRef == candidateRef {
			return index + 1
		}
	}
	return 0
}

func retrievalPromotionLiteralOrder(items []map[string]any) []any {
	limit := minInt(len(items), retrievalPromotionMaxCandidates)
	out := make([]any, 0, limit)
	for index := 0; index < limit; index++ {
		identity := searchIntelligenceCandidateIdentity(items[index])
		out = append(out, map[string]any{"candidate_ref": identity.CandidateRef, "occurrence": 1, "order": index + 1})
	}
	return out
}

// The canary receipt is intentionally independent of response-composition
// governance.  It provides a signed, replayable configure/readback/rollback
// envelope for retrieval promotion without mutating runtime state here.
type retrievalPromotionCanaryReceipt struct {
	SchemaID             string                   `json:"schema_id"`
	Version              int                      `json:"version"`
	Operation            string                   `json:"operation"`
	Generation           uint64                   `json:"generation"`
	WorkspaceRef         string                   `json:"workspace_ref"`
	ProjectRef           string                   `json:"project_ref"`
	TaskClassRef         string                   `json:"task_class_ref"`
	RetrievalIntentRef   string                   `json:"retrieval_intent_ref"`
	PolicyRef            string                   `json:"policy_ref"`
	SnapshotRef          string                   `json:"snapshot_ref"`
	CaseSetRef           string                   `json:"case_set_ref"`
	AssignmentSubjectRef string                   `json:"assignment_subject_ref"`
	ShadowBasisPoints    int                      `json:"shadow_basis_points"`
	ControlBasisPoints   int                      `json:"control_basis_points"`
	CanaryBasisPoints    int                      `json:"canary_basis_points"`
	MinimumCanarySamples int                      `json:"minimum_canary_samples"`
	MinimumDwellSeconds  int                      `json:"minimum_dwell_seconds"`
	RecordedAt           string                   `json:"recorded_at"`
	ExpiresAt            string                   `json:"expires_at"`
	IdempotencyKey       string                   `json:"idempotency_key"`
	ReasonDigest         string                   `json:"reason_digest,omitempty"`
	PreviousHash         string                   `json:"previous_hash,omitempty"`
	ReceiptDigest        string                   `json:"receipt_digest"`
	Issuer               contextPassportIssuer    `json:"issuer"`
	Signature            contextPassportSignature `json:"signature"`
}

func retrievalPromotionCanaryReceiptUnsigned(receipt retrievalPromotionCanaryReceipt) retrievalPromotionCanaryReceipt {
	receipt.ReceiptDigest = ""
	receipt.Issuer = contextPassportIssuer{}
	receipt.Signature = contextPassportSignature{}
	return receipt
}

func retrievalPromotionCanaryReceiptDigest(receipt retrievalPromotionCanaryReceipt) string {
	return "sha256:" + sha256Hex(recallResponseCanonicalJSON(retrievalPromotionCanaryReceiptUnsigned(receipt)))
}

func retrievalPromotionSignCanaryReceipt(receipt *retrievalPromotionCanaryReceipt, identity *contextIdentityKeys) error {
	if receipt == nil || identity == nil || validateContextIdentity(identity) != nil {
		return errors.New("retrieval promotion canary signing identity is unavailable")
	}
	receipt.Issuer = contextPassportIssuer{InstanceID: identity.InstanceID, SigningKeyID: identity.SigningKeyID, SigningPublicKey: identity.SigningPublicKey}
	receipt.ReceiptDigest = retrievalPromotionCanaryReceiptDigest(*receipt)
	signature, err := signBytesWithIdentity(struct {
		ReceiptDigest string                          `json:"receipt_digest"`
		Receipt       retrievalPromotionCanaryReceipt `json:"receipt"`
	}{receipt.ReceiptDigest, retrievalPromotionCanaryReceiptUnsigned(*receipt)}, identity)
	if err != nil {
		return err
	}
	receipt.Signature = signature
	return nil
}

type retrievalPromotionCanaryLedger struct {
	mu                  sync.RWMutex
	path                string
	maxBytes            int64
	identity            *contextIdentityKeys
	generation          uint64
	anchor              string
	active              *retrievalPromotionCanaryReceipt
	rolledBack          bool
	idempotency         map[string]retrievalPromotionCanaryReceipt
	commitUnknown       bool
	lastError           string
	loadCount           atomic.Uint64
	automaticRollbackMu sync.Mutex
	unlock              func()
}

// retrievalPromotionExposureLease pins a verified, server-owned canary head
// for the bounded ranking mutation. The ledger read lock is held while the
// caller applies ranking, so a concurrent configure/rollback either happens
// before verification (and forces control) or after the current request has
// completed its presentation mutation.
type retrievalPromotionExposureLease struct {
	store      *retrievalPromotionCanaryLedger
	generation uint64
	headDigest string
	receipt    retrievalPromotionCanaryReceipt
	now        time.Time
}

func retrievalPromotionCanaryLedgerPath() string {
	return resolveStoragePath(retrievalPromotionCanaryLedgerPathEnv, retrievalPromotionCanaryLedgerDefaultPath)
}

// retrievalPromotionCanaryOwner initializes the server-owned ledger once. The
// owner keeps the durable file lock for its lifetime, so request paths read a
// verified in-memory head instead of replaying or relocking the append log.
func (s *server) retrievalPromotionCanaryOwner() (*retrievalPromotionCanaryLedger, error) {
	if s == nil {
		return nil, errors.New("retrieval promotion canary owner is unavailable")
	}
	s.retrievalPromotionCanaryOnce.Do(func() {
		identity := (*contextIdentityKeys)(nil)
		if s.contextMesh != nil {
			identity = s.contextMesh.identity
		}
		if identity == nil || validateContextIdentity(identity) != nil {
			s.retrievalPromotionCanaryErr = errors.New("trusted canary signer unavailable")
			return
		}
		store, err := newRetrievalPromotionCanaryLedger(retrievalPromotionCanaryLedgerPath(), identity)
		if err != nil {
			s.retrievalPromotionCanaryErr = err
			return
		}
		s.retrievalPromotionCanary = store
	})
	if s.retrievalPromotionCanaryErr != nil {
		return nil, s.retrievalPromotionCanaryErr
	}
	if s.retrievalPromotionCanary == nil {
		return nil, errors.New("retrieval promotion canary owner is unavailable")
	}
	return s.retrievalPromotionCanary, nil
}

func newRetrievalPromotionCanaryLedger(path string, trusted *contextIdentityKeys) (*retrievalPromotionCanaryLedger, error) {
	if trusted == nil || validateContextIdentity(trusted) != nil {
		return nil, errors.New("retrieval promotion trusted signer is unavailable")
	}
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "" || path == "." {
		return nil, errors.New("retrieval promotion canary ledger path is empty")
	}
	if err := prepareOwnerOnlyFile(path, strings.TrimSpace(os.Getenv(retrievalPromotionCanaryLedgerPathEnv)) == ""); err != nil {
		return nil, fmt.Errorf("prepare retrieval promotion canary ledger: %w", err)
	}
	if err := createOwnerOnlyDurableEmptyFileIfMissing(path, strings.TrimSpace(os.Getenv(retrievalPromotionCanaryLedgerPathEnv)) == ""); err != nil {
		return nil, fmt.Errorf("create retrieval promotion canary ledger: %w", err)
	}
	unlock, err := lockOwnerOnlyMigration(path + ".lock")
	if err != nil {
		return nil, fmt.Errorf("lock retrieval promotion canary ledger: %w", err)
	}
	store := &retrievalPromotionCanaryLedger{path: path, maxBytes: retrievalPromotionCanaryLedgerMaxBytes, identity: trusted, unlock: unlock, idempotency: map[string]retrievalPromotionCanaryReceipt{}}
	if err := store.load(); err != nil {
		store.close()
		return nil, err
	}
	return store, nil
}

func (s *retrievalPromotionCanaryLedger) close() {
	if s != nil && s.unlock != nil {
		s.unlock()
		s.unlock = nil
	}
}

func (s *retrievalPromotionCanaryLedger) unavailable() bool {
	if s == nil {
		return true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.commitUnknown || strings.TrimSpace(s.lastError) != ""
}

func (s *retrievalPromotionCanaryLedger) load() error {
	s.loadCount.Add(1)
	info, err := os.Stat(s.path)
	if err != nil {
		return fmt.Errorf("stat retrieval promotion canary ledger: %w", err)
	}
	if info.IsDir() || info.Size() > s.maxBytes {
		return errors.New("retrieval promotion canary ledger is invalid or oversized")
	}
	file, err := os.Open(s.path)
	if err != nil {
		return fmt.Errorf("open retrieval promotion canary ledger: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), retrievalPromotionCanaryReceiptMaxBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var receipt retrievalPromotionCanaryReceipt
		decoder := json.NewDecoder(strings.NewReader(line))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&receipt); err != nil {
			return fmt.Errorf("decode retrieval promotion canary ledger: %w", err)
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			return errors.New("retrieval promotion canary ledger line has trailing JSON")
		}
		recordedAt, parseErr := time.Parse(time.RFC3339Nano, receipt.RecordedAt)
		if parseErr != nil || receipt.Generation != s.generation+1 || receipt.PreviousHash != s.anchor ||
			strings.TrimSpace(receipt.IdempotencyKey) == "" || len(receipt.IdempotencyKey) > 160 ||
			!retrievalPromotionVerifyCanaryReceipt(receipt, recordedAt, s.identity) {
			return errors.New("retrieval promotion canary ledger chain is invalid")
		}
		if previous, duplicate := s.idempotency[receipt.IdempotencyKey]; duplicate && previous.ReceiptDigest != receipt.ReceiptDigest {
			return errors.New("retrieval promotion canary ledger idempotency conflict")
		}
		s.idempotency[receipt.IdempotencyKey] = receipt
		s.applyLocked(receipt)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan retrieval promotion canary ledger: %w", err)
	}
	return nil
}

func (s *retrievalPromotionCanaryLedger) applyLocked(receipt retrievalPromotionCanaryReceipt) {
	s.generation = receipt.Generation
	s.anchor = receipt.ReceiptDigest
	if receipt.Operation == "rollback" {
		s.active = nil
		s.rolledBack = true
		return
	}
	if receipt.Operation == "configure" {
		copy := receipt
		s.active = &copy
		s.rolledBack = false
	}
}

func (s *retrievalPromotionCanaryLedger) append(receipt retrievalPromotionCanaryReceipt, now time.Time) error {
	if s == nil || s.identity == nil {
		return errors.New("retrieval promotion canary ledger unavailable")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, duplicate := s.idempotency[receipt.IdempotencyKey]; duplicate {
		if previous.ReceiptDigest == receipt.ReceiptDigest {
			return nil
		}
		return errors.New("retrieval promotion canary ledger idempotency conflict")
	}
	if s.commitUnknown || s.lastError != "" || receipt.Generation != s.generation+1 || receipt.PreviousHash != s.anchor ||
		strings.TrimSpace(receipt.IdempotencyKey) == "" || len(receipt.IdempotencyKey) > 160 ||
		!retrievalPromotionVerifyCanaryReceipt(receipt, now, s.identity) {
		return errors.New("retrieval promotion canary ledger append rejected")
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	info, err := os.Stat(s.path)
	if err != nil || info.Size()+int64(len(raw)+1) > s.maxBytes {
		return errors.New("retrieval promotion canary ledger capacity unavailable")
	}
	file, err := openOwnerOnlyAppend(s.path, strings.TrimSpace(os.Getenv(retrievalPromotionCanaryLedgerPathEnv)) == "")
	if err == nil {
		_, err = file.Write(append(raw, '\n'))
		if err == nil {
			err = file.Sync()
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		s.commitUnknown = true
		s.lastError = "append_or_fsync_failed"
		return errors.New("retrieval promotion canary ledger commit state is unknown")
	}
	s.applyLocked(receipt)
	s.idempotency[receipt.IdempotencyKey] = receipt
	return nil
}

func (s *retrievalPromotionCanaryLedger) generationHead() (uint64, string, bool) {
	if s == nil {
		return 0, "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.generation, s.anchor, !s.commitUnknown && s.lastError == ""
}

func (s *retrievalPromotionCanaryLedger) currentState(now time.Time) (uint64, string, bool, bool, string) {
	if s == nil {
		return 0, "", false, false, "canary_ledger_unavailable"
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.commitUnknown || s.lastError != "" {
		return s.generation, s.anchor, false, false, "canary_ledger_unavailable"
	}
	if s.rolledBack || s.active == nil {
		return s.generation, s.anchor, true, false, "canary_rolled_back"
	}
	if !retrievalPromotionVerifyCanaryReceipt(*s.active, now, s.identity) {
		return s.generation, s.anchor, true, false, "canary_receipt_expired_or_invalid"
	}
	return s.generation, s.anchor, true, true, ""
}

// currentHeadMatches fences a previously captured authority against a later
// configure or rollback. The comparison is one read-locked owner-ledger
// observation, so a stale signed receipt cannot cross the exposure boundary
// after the active head has changed.
func (s *retrievalPromotionCanaryLedger) currentHeadMatches(receipt retrievalPromotionCanaryReceipt, now time.Time) bool {
	if s == nil {
		return false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.commitUnknown && s.lastError == "" && !s.rolledBack && s.active != nil &&
		s.generation == receipt.Generation && s.anchor == receipt.ReceiptDigest &&
		s.active.Generation == receipt.Generation && s.active.ReceiptDigest == receipt.ReceiptDigest &&
		retrievalPromotionVerifyCanaryReceipt(*s.active, now, s.identity)
}

func (s *retrievalPromotionCanaryLedger) idempotentReceipt(key string) (retrievalPromotionCanaryReceipt, bool) {
	if s == nil || strings.TrimSpace(key) == "" {
		return retrievalPromotionCanaryReceipt{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	receipt, ok := s.idempotency[strings.TrimSpace(key)]
	return receipt, ok
}

func (s *retrievalPromotionCanaryLedger) acquireExposureLease(expected retrievalPromotionCanaryReceipt, now time.Time) (*retrievalPromotionExposureLease, string) {
	if s == nil {
		return nil, "canary_ledger_unavailable"
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.commitUnknown || s.lastError != "" {
		return nil, "canary_ledger_unavailable"
	}
	if s.generation != expected.Generation || s.anchor != expected.ReceiptDigest || s.active == nil || s.rolledBack || s.active.ReceiptDigest != expected.ReceiptDigest ||
		!retrievalPromotionVerifyCanaryReceipt(expected, now, s.identity) {
		return nil, "canary_head_changed_before_ranking"
	}
	return &retrievalPromotionExposureLease{store: s, generation: s.generation, headDigest: s.anchor, receipt: expected, now: now.UTC()}, ""
}

func (lease *retrievalPromotionExposureLease) withVerifiedHead(apply func()) bool {
	if lease == nil || lease.store == nil || apply == nil {
		return false
	}
	store := lease.store
	now := lease.now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.commitUnknown || store.lastError != "" || store.generation != lease.generation || store.anchor != lease.headDigest || store.active == nil || store.rolledBack || store.active.ReceiptDigest != lease.receipt.ReceiptDigest ||
		!retrievalPromotionVerifyCanaryReceipt(lease.receipt, now, store.identity) {
		return false
	}
	apply()
	return true
}

func (s *retrievalPromotionCanaryLedger) configure(receipt retrievalPromotionCanaryReceipt, now time.Time) error {
	if receipt.Operation != "configure" {
		return errors.New("retrieval promotion configure receipt operation required")
	}
	return s.append(receipt, now)
}

func (s *retrievalPromotionCanaryLedger) rollback(receipt retrievalPromotionCanaryReceipt, now time.Time) error {
	if receipt.Operation != "rollback" || receipt.CanaryBasisPoints != 0 {
		return errors.New("retrieval promotion rollback receipt operation required")
	}
	return s.append(receipt, now)
}

func (s *retrievalPromotionCanaryLedger) activeReceipt(now time.Time) (retrievalPromotionCanaryReceipt, bool, string) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.commitUnknown || s.lastError != "" {
		return retrievalPromotionCanaryReceipt{}, false, "canary_ledger_unavailable"
	}
	if s.rolledBack || s.active == nil {
		return retrievalPromotionCanaryReceipt{}, false, "canary_rolled_back"
	}
	active := *s.active
	if !retrievalPromotionVerifyCanaryReceipt(active, now, s.identity) {
		return retrievalPromotionCanaryReceipt{}, false, "canary_receipt_expired_or_invalid"
	}
	return active, true, ""
}

func retrievalPromotionConfiguredCanaryReceiptFromLedger(trusted *contextIdentityKeys, now time.Time) (retrievalPromotionCanaryReceipt, bool, string) {
	path := retrievalPromotionCanaryLedgerPath()
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return retrievalPromotionCanaryReceipt{}, false, "canary_ledger_missing"
	}
	if err != nil || info.IsDir() || info.Size() <= 0 {
		return retrievalPromotionCanaryReceipt{}, false, "canary_ledger_unavailable"
	}
	store, err := newRetrievalPromotionCanaryLedger(path, trusted)
	if err != nil {
		return retrievalPromotionCanaryReceipt{}, false, "canary_ledger_invalid"
	}
	defer store.close()
	return retrievalPromotionConfiguredCanaryReceiptFromStore(store, now)
}

func retrievalPromotionConfiguredCanaryReceiptFromStore(store *retrievalPromotionCanaryLedger, now time.Time) (retrievalPromotionCanaryReceipt, bool, string) {
	if store == nil {
		return retrievalPromotionCanaryReceipt{}, false, "canary_ledger_unavailable"
	}
	return store.activeReceipt(now)
}

func retrievalPromotionAutomaticRollbackOnLedger(store *retrievalPromotionCanaryLedger, trusted *contextIdentityKeys, active retrievalPromotionCanaryReceipt, now time.Time, reason string) (retrievalPromotionCanaryReceipt, bool, string) {
	if trusted == nil || validateContextIdentity(trusted) != nil || active.ReceiptDigest == "" || store == nil || store.identity == nil ||
		store.identity.InstanceID != trusted.InstanceID || store.identity.SigningKeyID != trusted.SigningKeyID || store.identity.SigningPublicKey != trusted.SigningPublicKey {
		return retrievalPromotionCanaryReceipt{}, false, "trusted_canary_signer_unavailable"
	}
	store.automaticRollbackMu.Lock()
	defer store.automaticRollbackMu.Unlock()
	rollbackKey := "auto-rollback-" + sha256Hex(active.ReceiptDigest + "\x00" + reason)[:32]
	store.mu.RLock()
	if existing, exists := store.idempotency[rollbackKey]; exists && existing.Operation == "rollback" {
		store.mu.RUnlock()
		return existing, true, "automatic_rollback_already_committed"
	}
	store.mu.RUnlock()
	generation, anchor, headOK := store.generationHead()
	if !headOK || generation != active.Generation || anchor != active.ReceiptDigest {
		store.mu.RLock()
		if existing, exists := store.idempotency[rollbackKey]; exists && existing.Operation == "rollback" {
			store.mu.RUnlock()
			return existing, true, "automatic_rollback_already_committed"
		}
		store.mu.RUnlock()
		return retrievalPromotionCanaryReceipt{}, false, "canary_rollback_head_mismatch"
	}
	rollback := active
	rollback.Operation = "rollback"
	rollback.Generation = generation + 1
	rollback.PreviousHash = anchor
	rollback.ControlBasisPoints = retrievalPromotionTotalBasisPoints
	rollback.ShadowBasisPoints = 0
	rollback.CanaryBasisPoints = 0
	rollback.RecordedAt = now.UTC().Format(time.RFC3339Nano)
	rollback.ExpiresAt = now.Add(retrievalPromotionCanaryMaximumAge).Format(time.RFC3339Nano)
	rollback.IdempotencyKey = rollbackKey
	rollback.ReasonDigest = "sha256:" + sha256Hex(strings.TrimSpace(reason))
	rollback.ReceiptDigest = ""
	rollback.Issuer = contextPassportIssuer{}
	rollback.Signature = contextPassportSignature{}
	if err := retrievalPromotionSignCanaryReceipt(&rollback, trusted); err != nil {
		return retrievalPromotionCanaryReceipt{}, false, "canary_rollback_signing_failed"
	}
	if err := store.rollback(rollback, now); err != nil {
		return retrievalPromotionCanaryReceipt{}, false, "canary_rollback_commit_failed"
	}
	return rollback, true, "automatic_rollback_committed"
}

func retrievalPromotionAutomaticRollback(trusted *contextIdentityKeys, active retrievalPromotionCanaryReceipt, now time.Time, reason string) (retrievalPromotionCanaryReceipt, bool, string) {
	if trusted == nil || validateContextIdentity(trusted) != nil || active.ReceiptDigest == "" {
		return retrievalPromotionCanaryReceipt{}, false, "trusted_canary_signer_unavailable"
	}
	store, err := newRetrievalPromotionCanaryLedger(retrievalPromotionCanaryLedgerPath(), trusted)
	if err != nil {
		return retrievalPromotionCanaryReceipt{}, false, "canary_ledger_unavailable"
	}
	defer store.close()
	return retrievalPromotionAutomaticRollbackOnLedger(store, trusted, active, now, reason)
}

func (s *retrievalPromotionCanaryLedger) signedReadback(now time.Time) (retrievalPromotionCanaryReceipt, error) {
	active, ok, reason := s.activeReceipt(now)
	if !ok {
		return retrievalPromotionCanaryReceipt{}, errors.New(reason)
	}
	active.Operation = "readback"
	active.ReceiptDigest = ""
	active.Signature = contextPassportSignature{}
	if err := retrievalPromotionSignCanaryReceipt(&active, s.identity); err != nil {
		return retrievalPromotionCanaryReceipt{}, err
	}
	return active, nil
}

func retrievalPromotionVerifyCanaryReceipt(receipt retrievalPromotionCanaryReceipt, now time.Time, trusted ...*contextIdentityKeys) bool {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	recordedAt, err := time.Parse(time.RFC3339Nano, receipt.RecordedAt)
	expiresAt, expiresErr := time.Parse(time.RFC3339Nano, receipt.ExpiresAt)
	weights := retrievalPromotionCohortWeights{ShadowBasisPoints: receipt.ShadowBasisPoints, ControlBasisPoints: receipt.ControlBasisPoints, CanaryBasisPoints: receipt.CanaryBasisPoints}
	if receipt.SchemaID != retrievalPromotionCanaryReceiptSchemaID || receipt.Version != 1 || receipt.Generation == 0 ||
		(receipt.Operation != "configure" && receipt.Operation != "readback" && receipt.Operation != "rollback") ||
		!weights.valid() || (receipt.Operation == "configure" && (receipt.CanaryBasisPoints <= 0 || receipt.CanaryBasisPoints > retrievalPromotionCanaryMaxBasisPoints)) ||
		(receipt.Operation == "rollback" && receipt.CanaryBasisPoints != 0) ||
		(receipt.Operation != "rollback" && (receipt.MinimumCanarySamples < retrievalPromotionCanaryMinimumSamples || receipt.MinimumDwellSeconds < int(retrievalPromotionCanaryMinimumDwell/time.Second))) ||
		(receipt.Operation != "rollback" && (expiresErr != nil || !expiresAt.After(recordedAt) || expiresAt.Sub(recordedAt) > retrievalPromotionCanaryMaximumAge)) ||
		!isSearchIntelligenceFullSHA256Ref(receipt.AssignmentSubjectRef) ||
		!isSearchIntelligenceFullSHA256Ref(receipt.WorkspaceRef) || !isSearchIntelligenceFullSHA256Ref(receipt.ProjectRef) ||
		!isSearchIntelligenceFullSHA256Ref(receipt.TaskClassRef) || !isSearchIntelligenceFullSHA256Ref(receipt.RetrievalIntentRef) ||
		!isSearchIntelligenceFullSHA256Ref(receipt.PolicyRef) || !isSearchIntelligenceFullSHA256Ref(receipt.SnapshotRef) ||
		!isSearchIntelligenceFullSHA256Ref(receipt.CaseSetRef) || !isSearchIntelligenceFullSHA256Ref(receipt.ReceiptDigest) ||
		(receipt.Generation > 1 && !isSearchIntelligenceFullSHA256Ref(receipt.PreviousHash)) ||
		err != nil || recordedAt.After(now.Add(time.Minute)) || now.Sub(recordedAt) > retrievalPromotionCanaryMaximumAge ||
		(receipt.ExpiresAt != "" && !expiresAt.After(now)) || receipt.ReceiptDigest != retrievalPromotionCanaryReceiptDigest(receipt) {
		return false
	}
	if len(trusted) == 0 || trusted[0] == nil || validateContextIdentity(trusted[0]) != nil ||
		receipt.Issuer.InstanceID != trusted[0].InstanceID || receipt.Issuer.SigningKeyID != trusted[0].SigningKeyID || receipt.Issuer.SigningPublicKey != trusted[0].SigningPublicKey {
		return false
	}
	return verifySignedBytes(struct {
		ReceiptDigest string                          `json:"receipt_digest"`
		Receipt       retrievalPromotionCanaryReceipt `json:"receipt"`
	}{receipt.ReceiptDigest, retrievalPromotionCanaryReceiptUnsigned(receipt)}, receipt.Signature, receipt.Issuer)
}

func retrievalPromotionCanaryReceiptMap(raw map[string]any, now time.Time, trusted *contextIdentityKeys) (retrievalPromotionCanaryReceipt, bool) {
	var receipt retrievalPromotionCanaryReceipt
	encoded, err := json.Marshal(raw)
	if err != nil || json.Unmarshal(encoded, &receipt) != nil || !retrievalPromotionVerifyCanaryReceipt(receipt, now, trusted) {
		return retrievalPromotionCanaryReceipt{}, false
	}
	return receipt, true
}

func retrievalPromotionConfiguredCanaryReceipt(trusted *contextIdentityKeys, now time.Time) (retrievalPromotionCanaryReceipt, bool, string) {
	if trusted == nil || validateContextIdentity(trusted) != nil {
		return retrievalPromotionCanaryReceipt{}, false, "trusted_canary_signer_unavailable"
	}
	ledgerReceipt, ledgerOK, ledgerReason := retrievalPromotionConfiguredCanaryReceiptFromLedger(trusted, now)
	if !ledgerOK {
		return retrievalPromotionCanaryReceipt{}, false, ledgerReason
	}
	// Optional receipt JSON/file values are readback transports only. They must
	// match the owner-only ledger head exactly; a self-signed value can never
	// configure or activate an arm.
	transport := strings.TrimSpace(os.Getenv(retrievalPromotionCanaryReceiptJSONEnv))
	path := strings.TrimSpace(os.Getenv(retrievalPromotionCanaryReceiptPathEnv))
	if transport != "" || path != "" {
		var encoded []byte
		if transport != "" {
			if len(transport) > retrievalPromotionCanaryReceiptMaxBytes {
				return retrievalPromotionCanaryReceipt{}, false, "canary_receipt_oversized"
			}
			encoded = []byte(transport)
		} else {
			info, err := os.Stat(path)
			if err != nil || info.IsDir() || info.Size() <= 0 || info.Size() > retrievalPromotionCanaryReceiptMaxBytes {
				return retrievalPromotionCanaryReceipt{}, false, "canary_receipt_transport_invalid"
			}
			encoded, err = os.ReadFile(path)
			if err != nil {
				return retrievalPromotionCanaryReceipt{}, false, "canary_receipt_transport_unavailable"
			}
		}
		var raw map[string]any
		if json.Unmarshal(encoded, &raw) != nil {
			return retrievalPromotionCanaryReceipt{}, false, "canary_receipt_transport_invalid"
		}
		transportReceipt, ok := retrievalPromotionCanaryReceiptMap(raw, now, trusted)
		if !ok || transportReceipt.ReceiptDigest != ledgerReceipt.ReceiptDigest {
			return retrievalPromotionCanaryReceipt{}, false, "canary_receipt_not_active_head"
		}
	}
	return ledgerReceipt, true, ""
}

func retrievalPromotionReceiptMatchesScope(receipt retrievalPromotionCanaryReceipt, project, taskClass, intent, workspaceRef, policyRef string) bool {
	scopeRef := func(kind, value string) string {
		if isSearchIntelligenceFullSHA256Ref(strings.TrimSpace(value)) {
			return strings.TrimSpace(value)
		}
		return contextPackLearnedScopeRef(kind, value)
	}
	return receipt.ProjectRef == scopeRef("project", project) &&
		receipt.TaskClassRef == scopeRef("task_class", taskClass) &&
		receipt.RetrievalIntentRef == scopeRef("retrieval_intent", intent) &&
		receipt.WorkspaceRef == scopeRef("workspace", workspaceRef) &&
		receipt.PolicyRef == contextPackLearnedDigestRef(policyRef)
}

func retrievalPromotionSameSnapshotHoldoutGate(impact map[string]any, receipt retrievalPromotionCanaryReceipt, nowArg ...time.Time) (bool, string) {
	now := time.Now().UTC()
	if len(nowArg) > 0 && !nowArg[0].IsZero() {
		now = nowArg[0].UTC()
	}
	evidence := anyMap(impact["activation_evidence"])
	if len(evidence) == 0 {
		return false, "same_snapshot_evidence_missing"
	}
	if production := anyMap(evidence["promotion_evidence"]); len(production) > 0 {
		if !anyToBool(production["available"]) {
			return false, "production_promotion_evidence_unavailable:" + firstNonEmptyStrings(anyToString(production["reason"]), "unknown")
		}
		if anyToString(production["snapshot_ref"]) != anyToString(evidence["snapshot_ref"]) ||
			anyToInt(production["train_count"], -1) != anyToInt(evidence["train_count"], -2) ||
			anyToInt(production["holdout_count"], -1) != anyToInt(evidence["holdout_count"], -2) ||
			anyToInt(production["canary_sample_count"], -1) != anyToInt(evidence["canary_sample_count"], -2) {
			return false, "production_promotion_evidence_inconsistent"
		}
	}
	if same, present := evidence["same_snapshot"].(bool); !present || !same {
		return false, "same_snapshot_required"
	}
	split := strings.ToLower(strings.TrimSpace(firstNonEmptyStrings(anyToString(evidence["time_split"]), anyToString(evidence["split"]))))
	if split != "chronological_80_20" {
		return false, "chronological_holdout_required"
	}
	caseSetRef := contextPackLearnedDigestRef(anyToString(evidence["case_set_ref"]))
	snapshotRef := contextPackLearnedDigestRef(anyToString(evidence["snapshot_ref"]))
	if caseSetRef == "" || snapshotRef == "" || receipt.CaseSetRef != caseSetRef || receipt.SnapshotRef != snapshotRef {
		return false, "canary_snapshot_or_case_set_mismatch"
	}
	if anyToInt(evidence["holdout_count"], 0) < searchImpactMinHoldoutOutcomes || anyToInt(evidence["train_count"], 0) < searchImpactMinTrainOutcomes {
		return false, "time_split_holdout_minimums_not_met"
	}
	canarySamples, canarySamplesOK := retrievalPromotionStrictInt(evidence["canary_sample_count"])
	startedAt, startedErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(anyToString(evidence["canary_started_at"])))
	if !canarySamplesOK || canarySamples < receipt.MinimumCanarySamples || startedErr != nil || startedAt.After(now.Add(time.Minute)) || now.Sub(startedAt) < time.Duration(receipt.MinimumDwellSeconds)*time.Second {
		return false, "canary_dwell_or_sample_minimums_not_met"
	}
	return true, "same_snapshot_time_split_verified"
}

func retrievalPromotionMetricValue(values map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		value, ok := searchImpactStrictFiniteNumber(values, key)
		if ok {
			return value, true
		}
	}
	return 0, false
}

func retrievalPromotionExactZeroResourceReceipt(impact map[string]any) (map[string]any, bool) {
	evidence := anyMap(impact["activation_evidence"])
	receipt := anyMap(evidence["execution_receipt"])
	if len(receipt) == 0 {
		receipt = anyMap(evidence["provider_usage_receipt"])
	}
	if len(receipt) == 0 {
		receipt = anyMap(impact["execution_receipt"])
	}
	if len(receipt) == 0 {
		receipt = anyMap(anyMap(impact["impact_intelligence"])["execution_receipt"])
	}
	if len(receipt) == 0 || !anyToBool(receipt["complete"]) || !anyToBool(receipt["exact_zero"]) {
		return nil, false
	}
	readZero := func(keys ...string) bool {
		for _, key := range keys {
			if value, ok := searchImpactStrictFiniteNumber(receipt, key); ok {
				return value == 0
			}
		}
		return false
	}
	if !readZero("provider_calls", "provider_call_count") || !readZero("provider_tokens", "provider_token_count") ||
		!readZero("provider_cost", "provider_cost_usd", "cost") || !readZero("external_network_calls", "network_calls", "external_calls") {
		return nil, false
	}
	return receipt, true
}

func retrievalPromotionSampleSufficiencyGate(impact map[string]any, signedReceipt *retrievalPromotionCanaryReceipt) (bool, string, map[string]any) {
	evidence := anyMap(impact["activation_evidence"])
	sufficiency := anyMap(evidence["sample_sufficiency"])
	if len(sufficiency) == 0 {
		sufficiency = anyMap(impact["sample_sufficiency"])
	}
	if len(sufficiency) == 0 {
		sufficiency = anyMap(anyMap(impact["impact_intelligence"])["sample_sufficiency"])
	}
	required, requiredOK := retrievalPromotionStrictInt(sufficiency["required_sample_count"])
	observed, observedOK := retrievalPromotionStrictInt(sufficiency["observed_sample_count"])
	generation, generationOK := retrievalPromotionStrictInt(sufficiency["canary_generation"])
	canarySamples, canarySamplesOK := retrievalPromotionStrictInt(evidence["canary_sample_count"])
	version, versionOK := retrievalPromotionStrictInt(sufficiency["version"])
	statisticalPowerAvailable, statisticalPowerFlagPresent := sufficiency["statistical_power_available"].(bool)
	passed, passFlagPresent := sufficiency["pass"].(bool)
	digest := contextPackLearnedDigestRef(anyToString(sufficiency["canary_receipt_digest"]))
	if len(sufficiency) == 0 || anyToString(sufficiency["schema_id"]) != "contextlattice_search_impact_sample_sufficiency.v1" ||
		!versionOK || version != 1 || anyToString(sufficiency["source"]) != "server_reconciled_canary_outcomes" ||
		anyToString(sufficiency["method"]) != "exact_count_against_signed_authority_minimum_v1" ||
		anyToString(sufficiency["promotion_authority"]) != "trusted_signed_canary_ledger" ||
		!statisticalPowerFlagPresent || statisticalPowerAvailable || !passFlagPresent ||
		anyToString(sufficiency["limits"]) != "count_threshold_only_no_power_or_effect_size_estimate" ||
		!requiredOK || required < retrievalPromotionCanaryMinimumSamples || !observedOK || !canarySamplesOK || observed != canarySamples ||
		!generationOK || generation <= 0 || digest == "" {
		return false, "sample_sufficiency_missing_or_invalid", sufficiency
	}
	for _, forbidden := range []string{"power", "statistical_power", "minimum_effect_size", "effect_size"} {
		if _, present := sufficiency[forbidden]; present {
			return false, "sample_sufficiency_contains_statistical_claim", sufficiency
		}
	}
	allowed := map[string]struct{}{}
	for _, field := range []string{
		"schema_id", "version", "source", "method", "pass", "required_sample_count", "observed_sample_count",
		"promotion_authority", "canary_receipt_digest", "canary_generation", "statistical_power_available", "limits",
	} {
		allowed[field] = struct{}{}
	}
	for field := range sufficiency {
		if _, ok := allowed[field]; !ok {
			return false, "sample_sufficiency_contains_unknown_claim", sufficiency
		}
	}
	if signedReceipt != nil && (required != signedReceipt.MinimumCanarySamples || uint64(generation) != signedReceipt.Generation || digest != signedReceipt.ReceiptDigest) {
		return false, "sample_sufficiency_authority_mismatch", sufficiency
	}
	if !passed || observed < required {
		return false, "signed_authority_sample_minimum_not_met", sufficiency
	}
	return true, "signed_authority_sample_sufficiency_verified", sufficiency
}

// retrievalPromotionMetricGuard is stricter than the advisory shadow
// evaluator. Every comparator metric, absolute floor, zero-resource receipt,
// causal interval, and signed-authority sample-count minimum is required before
// activation. Statistical power and effect size are intentionally unavailable.
func retrievalPromotionMetricGuard(shadow, impact map[string]any) map[string]any {
	return retrievalPromotionMetricGuardForReceipt(shadow, impact, nil)
}

func retrievalPromotionMetricGuardForReceipt(shadow, impact map[string]any, signedReceipt *retrievalPromotionCanaryReceipt) map[string]any {
	baseline := anyMap(shadow["baseline"])
	treatment := anyMap(shadow["shadow"])
	if len(treatment) == 0 {
		treatment = anyMap(shadow["treatment"])
	}
	if len(baseline) == 0 || len(treatment) == 0 {
		return map[string]any{"pass": false, "rollback": true, "reason": "comparator_metrics_missing"}
	}
	type metricRule struct {
		name          string
		keys          []string
		lowerIsBetter bool
		floor         float64
		exactOne      bool
	}
	rules := []metricRule{
		{"recall", []string{"decision_impact_recall_at_5", "recall_at_k", "recall"}, false, 0.90, false},
		{"mrr", []string{"mrr"}, false, 0.90, false},
		{"numeric_exactness", []string{"numeric_exactness"}, false, 1.0, true},
		{"citation_coverage", []string{"citation_coverage"}, false, 0.90, false},
		{"citation_exactness", []string{"citation_exactness"}, false, 0.90, false},
		{"quality", []string{"quality_score", "first_pass_success_rate", "quality_rate"}, false, 0.90, false},
		{"latency", []string{"p95_latency_ms", "latency_p95_ms"}, true, 0, false},
		{"repair", []string{"repair_required_rate", "repair_rate"}, true, 0, false},
	}
	values := map[string]any{}
	pass := true
	reasons := []string{}
	for _, rule := range rules {
		base, baseOK := retrievalPromotionMetricValue(baseline, rule.keys...)
		candidate, candidateOK := retrievalPromotionMetricValue(treatment, rule.keys...)
		if !baseOK || !candidateOK || math.IsNaN(base) || math.IsNaN(candidate) || math.IsInf(base, 0) || math.IsInf(candidate, 0) {
			pass = false
			reasons = append(reasons, rule.name+"_missing")
			continue
		}
		floor := rule.floor
		if rule.name == "quality" && (containsString(rule.keys, "quality_score") && baseline["quality_score"] != nil || treatment["quality_score"] != nil) {
			floor = 90
		}
		noRegression := candidate >= base
		if rule.lowerIsBetter {
			noRegression = candidate <= base
		}
		floorPass := rule.lowerIsBetter || candidate >= floor
		if rule.exactOne {
			floorPass = math.Abs(candidate-1.0) <= 1e-9
		}
		values[rule.name] = map[string]any{"baseline": roundFloat(base, 8), "treatment": roundFloat(candidate, 8), "no_regression": noRegression, "floor": floor, "floor_pass": floorPass}
		if !noRegression {
			pass = false
			reasons = append(reasons, rule.name+"_regression")
		}
		if !floorPass {
			pass = false
			reasons = append(reasons, rule.name+"_floor")
		}
	}
	if _, ok := retrievalPromotionExactZeroResourceReceipt(impact); !ok {
		pass = false
		reasons = append(reasons, "exact_zero_provider_network_receipt_missing")
	}
	utility := anyMap(anyMap(impact["impact_intelligence"])["utility_reconciliation"])
	if len(utility) == 0 {
		utility = anyMap(impact["utility_reconciliation"])
	}
	interval := anyMap(utility["causal_interval"])
	low, lowOK := retrievalPromotionMetricValue(interval, "low")
	high, highOK := retrievalPromotionMetricValue(interval, "high")
	point, pointOK := retrievalPromotionMetricValue(interval, "point", "estimate")
	if len(interval) == 0 || !lowOK || !highOK || !pointOK || low <= 0 || high < low || point < low || point > high {
		pass = false
		reasons = append(reasons, "utility_causal_interval_missing_or_regressed")
	}
	utilityBaseline, utilityBaselineOK := retrievalPromotionMetricValue(utility, "baseline_utility", "baseline", "control_utility", "control")
	utilityTreatment, utilityTreatmentOK := retrievalPromotionMetricValue(utility, "treatment_utility", "treatment", "shadow_utility", "canary_utility")
	if anyToBool(utility["regression"]) || anyToBool(utility["utility_regression"]) || (utilityBaselineOK != utilityTreatmentOK) || (utilityBaselineOK && utilityTreatment < utilityBaseline) {
		pass = false
		reasons = append(reasons, "utility_regression")
	}
	sufficiencyPass, sufficiencyReason, sufficiency := retrievalPromotionSampleSufficiencyGate(impact, signedReceipt)
	if !sufficiencyPass {
		pass = false
		reasons = append(reasons, sufficiencyReason)
	}
	if len(reasons) == 0 {
		reasons = []string{"all_metric_floors_and_guards_pass"}
	}
	return map[string]any{"pass": pass, "rollback": !pass, "reason": strings.Join(reasons, ","), "metrics": values, "sample_sufficiency": sufficiency, "absolute_floors": true}
}

func retrievalPromotionImpactProjection(shadow, impact map[string]any) map[string]any {
	guard := retrievalPromotionMetricGuard(shadow, impact)
	return map[string]any{
		"schema_id": retrievalPromotionContractID, "version": retrievalPromotionVersion,
		"mode": "advisory_until_signed_canary", "promotion_eligible": false,
		"automatic_rollback": map[string]any{"enabled": true, "triggered": anyToBool(guard["rollback"]), "reason": anyToString(guard["reason"])},
		"metric_guard":       guard,
		"required_gates":     []any{"same_snapshot_time_split", "verified_outcome_receipts", "loss_guard", "trusted_signed_canary_ledger_head", "absolute_metric_floors", "exact_zero_provider_network_receipt", "utility_causal_interval", "signed_authority_sample_sufficiency"},
	}
}

type retrievalPromotionActivationInput struct {
	Project          string
	TaskClass        string
	RetrievalIntent  string
	WorkspaceRef     string
	PolicyRef        string
	Assignment       string
	Impact           map[string]any
	Shadow           map[string]any
	Receipt          retrievalPromotionCanaryReceipt
	TrustedSigner    *contextIdentityKeys
	CanaryHeadDigest string
	Now              time.Time
}

func retrievalPromotionAuthorizeActivation(input retrievalPromotionActivationInput) (bool, string, map[string]any) {
	if input.Now.IsZero() {
		input.Now = time.Now().UTC()
	}
	if input.Receipt.Operation != "configure" || !retrievalPromotionVerifyCanaryReceipt(input.Receipt, input.Now, input.TrustedSigner) {
		return false, "canary_receipt_signature_invalid", map[string]any{"promotion_eligible": false}
	}
	if input.CanaryHeadDigest == "" || input.CanaryHeadDigest != input.Receipt.ReceiptDigest {
		return false, "canary_receipt_not_active_head", map[string]any{"promotion_eligible": false}
	}
	if !retrievalPromotionReceiptMatchesScope(input.Receipt, input.Project, input.TaskClass, input.RetrievalIntent, input.WorkspaceRef, input.PolicyRef) {
		return false, "canary_receipt_scope_mismatch", map[string]any{"promotion_eligible": false}
	}
	shadowEvidence := searchImpactShadowEvaluation(input.Shadow)
	if !anyToBool(shadowEvidence["pass"]) {
		return false, "comparative_shadow_receipt_invalid", map[string]any{"promotion_eligible": false, "comparative_shadow": shadowEvidence}
	}
	expectedAssignmentRef := retrievalPromotionAssignmentSubjectRef(input.Assignment)
	if expectedAssignmentRef == "" || input.Receipt.AssignmentSubjectRef != expectedAssignmentRef {
		return false, "canary_receipt_assignment_subject_mismatch", map[string]any{"promotion_eligible": false}
	}
	if pass, reason := retrievalPromotionSameSnapshotHoldoutGate(input.Impact, input.Receipt, input.Now); !pass {
		return false, reason, map[string]any{"promotion_eligible": false}
	}
	if !contextPackLearnedProofGatesPass(input.Impact) {
		return false, "verified_outcome_proof_gates_failed", map[string]any{"promotion_eligible": false}
	}
	guard := retrievalPromotionMetricGuardForReceipt(input.Shadow, input.Impact, &input.Receipt)
	if !anyToBool(guard["pass"]) {
		return false, "metric_regression_or_guard_missing", map[string]any{"promotion_eligible": false, "metric_guard": guard}
	}
	weights := retrievalPromotionCohortWeights{ShadowBasisPoints: input.Receipt.ShadowBasisPoints, ControlBasisPoints: input.Receipt.ControlBasisPoints, CanaryBasisPoints: input.Receipt.CanaryBasisPoints}
	cohort := retrievalPromotionStableCohort(expectedAssignmentRef, input.Receipt.SnapshotRef, input.Receipt.PolicyRef, weights)
	if anyToString(cohort["arm"]) != "canary" {
		return false, "stable_cohort_control", map[string]any{"promotion_eligible": true, "cohort": cohort, "metric_guard": guard}
	}
	if anyToString(cohort["subject_ref"]) != input.Receipt.AssignmentSubjectRef {
		return false, "canary_receipt_assignment_subject_mismatch", map[string]any{"promotion_eligible": false, "cohort": cohort}
	}
	return true, "signed_canary_verified", map[string]any{
		"promotion_eligible": true, "cohort": cohort, "metric_guard": guard,
		"canary_receipt_digest":     input.Receipt.ReceiptDigest,
		"canary_receipt_generation": input.Receipt.Generation,
		"canary_snapshot":           retrievalPromotionCanaryReceiptSnapshot(input.Receipt),
		"cohort_authority":          "trusted_signed_canary_ledger",
	}
}

func retrievalPromotionCanaryReceiptSnapshot(receipt retrievalPromotionCanaryReceipt) map[string]any {
	return map[string]any{
		"generation": receipt.Generation, "workspace_ref": receipt.WorkspaceRef, "project_ref": receipt.ProjectRef,
		"task_class_ref": receipt.TaskClassRef, "retrieval_intent_ref": receipt.RetrievalIntentRef,
		"policy_ref": receipt.PolicyRef, "snapshot_ref": receipt.SnapshotRef, "case_set_ref": receipt.CaseSetRef,
		"assignment_subject_ref": receipt.AssignmentSubjectRef,
		"shadow_basis_points":    receipt.ShadowBasisPoints, "control_basis_points": receipt.ControlBasisPoints,
		"canary_basis_points": receipt.CanaryBasisPoints, "minimum_canary_samples": receipt.MinimumCanarySamples,
		"minimum_dwell_seconds": receipt.MinimumDwellSeconds, "recorded_at": receipt.RecordedAt, "expires_at": receipt.ExpiresAt,
		"receipt_digest": receipt.ReceiptDigest,
	}
}

// retrievalPromotionApplySignedCohortToDecision overwrites every final
// exposure field from the exact verified ledger snapshot. The environment
// cohort calculated by the cheap activation classifier is never authoritative
// once production activation reaches this seam.
func retrievalPromotionApplySignedCohortToDecision(
	decision *contextPackLearnedActivationDecision,
	receipt retrievalPromotionCanaryReceipt,
	evidence map[string]any,
	lease *retrievalPromotionExposureLease,
) bool {
	if decision == nil || lease == nil || receipt.Operation != "configure" || receipt.AssignmentSubjectRef == "" {
		return false
	}
	weights := retrievalPromotionCohortWeights{ShadowBasisPoints: receipt.ShadowBasisPoints, ControlBasisPoints: receipt.ControlBasisPoints, CanaryBasisPoints: receipt.CanaryBasisPoints}
	cohort := retrievalPromotionStableCohort(receipt.AssignmentSubjectRef, receipt.SnapshotRef, receipt.PolicyRef, weights)
	if anyToString(cohort["arm"]) != "canary" || anyToString(cohort["subject_ref"]) != receipt.AssignmentSubjectRef ||
		anyToString(cohort["snapshot_ref"]) != receipt.SnapshotRef || anyToString(cohort["policy_ref"]) != receipt.PolicyRef {
		return false
	}
	evidenceCohort := anyMap(evidence["cohort"])
	if len(evidenceCohort) == 0 || anyToString(evidenceCohort["arm"]) != anyToString(cohort["arm"]) ||
		anyToInt(anyMap(evidenceCohort["weights"])["canary_basis_points"], -1) != receipt.CanaryBasisPoints ||
		anyToString(evidenceCohort["subject_ref"]) != receipt.AssignmentSubjectRef ||
		contextPackLearnedDigestRef(anyToString(evidence["canary_receipt_digest"])) != receipt.ReceiptDigest {
		return false
	}
	decision.AssignmentSubjectRef = receipt.AssignmentSubjectRef
	decision.RequestRef = receipt.AssignmentSubjectRef
	decision.CanaryBasisPoints = receipt.CanaryBasisPoints
	decision.CanaryPercent = clampInt((receipt.CanaryBasisPoints+99)/100, 1, 10)
	decision.ExposureBucket = anyToInt(cohort["bucket_basis_points"], -1)
	decision.Eligible = true
	decision.AssignedTreatment = true
	decision.Arm = "canary"
	decision.Reason = "signed_canary_verified"
	decision.ActivationReceiptID = contextPackLearnedActivationReceiptID(*decision)
	decision.Promotion = cloneJSONMap(evidence)
	cohortWithReceipt := cloneJSONMap(cohort)
	cohortWithReceipt["receipt_digest"] = receipt.ReceiptDigest
	cohortWithReceipt["case_set_ref"] = receipt.CaseSetRef
	decision.Promotion["cohort"] = cohortWithReceipt
	decision.Promotion["canary_receipt"] = retrievalPromotionCanaryReceiptSnapshot(receipt)
	decision.Promotion["canary_basis_points"] = receipt.CanaryBasisPoints
	decision.Promotion["cohort_authority"] = "trusted_signed_canary_ledger"
	decision.PromotionLease = lease
	return decision.ExposureBucket >= 0
}

func retrievalPromotionConfiguredActivationGateWithLedger(
	project, taskClass, retrievalIntent, workspaceRef, policyRef, assignment string,
	impact, shadow map[string]any,
	now time.Time,
	trusted *contextIdentityKeys,
	store *retrievalPromotionCanaryLedger,
) (bool, string, map[string]any) {
	receipt, configured, reason := retrievalPromotionConfiguredCanaryReceiptFromStore(store, now)
	if !configured {
		return false, reason, map[string]any{"promotion_eligible": false, "canary_receipt_required": true}
	}
	eligible, authorizationReason, evidence := retrievalPromotionAuthorizeActivation(retrievalPromotionActivationInput{
		Project: project, TaskClass: taskClass, RetrievalIntent: retrievalIntent,
		WorkspaceRef: workspaceRef, PolicyRef: policyRef, Assignment: assignment,
		Impact: impact, Shadow: shadow, Receipt: receipt, TrustedSigner: trusted, CanaryHeadDigest: receipt.ReceiptDigest, Now: now,
	})
	if !eligible && anyToBool(anyMap(evidence["metric_guard"])["rollback"]) {
		rollback, rolledBack, rollbackReason := retrievalPromotionAutomaticRollbackOnLedger(store, trusted, receipt, now, anyToString(anyMap(evidence["metric_guard"])["reason"]))
		evidence["automatic_rollback"] = map[string]any{"attempted": true, "committed": rolledBack, "reason": rollbackReason}
		if rolledBack {
			evidence["automatic_rollback_receipt_digest"] = rollback.ReceiptDigest
			authorizationReason = "automatic_rollback_committed"
		} else {
			authorizationReason = "metric_guard_failed_rollback_unavailable"
		}
	}
	return eligible, authorizationReason, evidence
}

func retrievalPromotionConfiguredActivationGate(
	project, taskClass, retrievalIntent, workspaceRef, policyRef, assignment string,
	impact, shadow map[string]any,
	now time.Time,
	trusted *contextIdentityKeys,
) (bool, string, map[string]any) {
	if trusted == nil || validateContextIdentity(trusted) != nil {
		return false, "trusted_canary_signer_unavailable", map[string]any{"promotion_eligible": false}
	}
	store, err := newRetrievalPromotionCanaryLedger(retrievalPromotionCanaryLedgerPath(), trusted)
	if err != nil {
		return false, "canary_ledger_unavailable", map[string]any{"promotion_eligible": false}
	}
	defer store.close()
	return retrievalPromotionConfiguredActivationGateWithLedger(project, taskClass, retrievalIntent, workspaceRef, policyRef, assignment, impact, shadow, now, trusted, store)
}

func retrievalPromotionGovernanceRef(payload map[string]any, key string) string {
	return contextPackLearnedDigestRef(anyToString(payload[key]))
}

func retrievalPromotionGovernanceScopeMatches(left, right retrievalPromotionCanaryReceipt) bool {
	return left.WorkspaceRef == right.WorkspaceRef && left.ProjectRef == right.ProjectRef && left.TaskClassRef == right.TaskClassRef &&
		left.RetrievalIntentRef == right.RetrievalIntentRef && left.PolicyRef == right.PolicyRef && left.SnapshotRef == right.SnapshotRef &&
		left.CaseSetRef == right.CaseSetRef && left.AssignmentSubjectRef == right.AssignmentSubjectRef
}

func retrievalPromotionGovernanceRetryMatches(
	receipt retrievalPromotionCanaryReceipt,
	operation string,
	expectedGeneration int,
	reason string,
	workspaceRef, projectRef, taskClassRef, intentRef, policyRef, snapshotRef, caseSetRef, assignmentRef string,
	payload map[string]any,
) bool {
	if receipt.Operation != operation || expectedGeneration < 0 || receipt.Generation != uint64(expectedGeneration+1) ||
		receipt.WorkspaceRef != workspaceRef || receipt.ProjectRef != projectRef || receipt.TaskClassRef != taskClassRef ||
		receipt.RetrievalIntentRef != intentRef || receipt.PolicyRef != policyRef || receipt.SnapshotRef != snapshotRef ||
		receipt.CaseSetRef != caseSetRef || receipt.AssignmentSubjectRef != assignmentRef ||
		receipt.ReasonDigest != "sha256:"+sha256Hex(reason) {
		return false
	}
	if operation != "configure" {
		return true
	}
	shadow, shadowOK := retrievalPromotionStrictInt(payload["shadow_basis_points"])
	control, controlOK := retrievalPromotionStrictInt(payload["control_basis_points"])
	canary, canaryOK := retrievalPromotionStrictInt(payload["canary_basis_points"])
	minimumSamples, samplesOK := retrievalPromotionStrictInt(payload["minimum_canary_samples"])
	minimumDwell, dwellOK := retrievalPromotionStrictInt(payload["minimum_dwell_seconds"])
	expiresAt := strings.TrimSpace(anyToString(payload["expires_at"]))
	return shadowOK && controlOK && canaryOK && samplesOK && dwellOK &&
		shadow == receipt.ShadowBasisPoints && control == receipt.ControlBasisPoints && canary == receipt.CanaryBasisPoints &&
		minimumSamples == receipt.MinimumCanarySamples && minimumDwell == receipt.MinimumDwellSeconds && expiresAt == receipt.ExpiresAt
}

func retrievalPromotionCanaryResponse(store *retrievalPromotionCanaryLedger, now time.Time) map[string]any {
	if store == nil {
		return map[string]any{"active": false, "reason": "canary_ledger_unavailable"}
	}
	generation, headDigest, headOK := store.generationHead()
	active, activeOK, reason := store.activeReceipt(now)
	response := map[string]any{
		"schema_id": retrievalPromotionCanaryGovernanceContractID, "version": 1,
		"active": activeOK, "generation": generation, "head_digest": headDigest, "head_verified": headOK,
	}
	if !activeOK {
		response["reason"] = reason
		return response
	}
	readback, readbackErr := store.signedReadback(now)
	response["configure_receipt"] = active
	if readbackErr == nil {
		response["readback_receipt"] = readback
	}
	return response
}

// retrievalPromotionCanaryReplayResponse is derived only from the durable
// idempotency receipt. A retry must reproduce the operation's original result
// even if a later configure/rollback has advanced the current head.
func retrievalPromotionCanaryReplayResponse(store *retrievalPromotionCanaryLedger, receipt retrievalPromotionCanaryReceipt, now time.Time) map[string]any {
	currentGeneration, currentHead, currentHeadVerified, currentActive, currentReason := store.currentState(now)
	response := map[string]any{
		"schema_id": retrievalPromotionCanaryGovernanceContractID, "version": 1,
		"replayed": true,
		// The legacy top-level state fields are intentionally current-state
		// fields. Historical idempotency data is carried under the explicit
		// as_of_* fields below, so a replay after rollback cannot claim that an
		// old configure remains active.
		"active": currentActive, "generation": currentGeneration,
		"head_digest": currentHead, "head_verified": currentHeadVerified,
		"current_active": currentActive, "current_generation": currentGeneration,
		"current_head_digest": currentHead, "current_head_verified": currentHeadVerified,
		"as_of_active": receipt.Operation == "configure", "as_of_generation": receipt.Generation,
		"as_of_head_digest": receipt.ReceiptDigest, "as_of_head_verified": true,
		"operation": receipt.Operation, "receipt": receipt,
	}
	if currentReason != "" {
		response["reason"] = currentReason
	}
	if receipt.Operation == "configure" {
		response["configure_receipt"] = receipt
		if store != nil && store.identity != nil {
			readback := receipt
			readback.Operation = "readback"
			readback.ReceiptDigest = ""
			readback.Signature = contextPassportSignature{}
			if err := retrievalPromotionSignCanaryReceipt(&readback, store.identity); err == nil {
				response["readback_receipt"] = readback
			}
		}
	} else {
		if currentReason == "" {
			response["reason"] = "canary_rolled_back"
		}
	}
	return response
}

// retrievalPromotionCanaryGovernance is the entitled server-owned lifecycle
// for configure, signed readback, and rollback. Caller-provided metrics or
// public keys are not accepted as authority; the local context identity and
// owner-only ledger are the only mutation surfaces.
func (s *server) retrievalPromotionCanaryGovernance(w http.ResponseWriter, r *http.Request) {
	if s == nil || r == nil || r.URL.Path != retrievalPromotionCanaryGovernancePath {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "route_not_found"})
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method_not_allowed"})
		return
	}
	payload := map[string]any{}
	if r.Method == http.MethodPost {
		body, err := readRequestBody(r)
		if err != nil || len(body) > retrievalPromotionCanaryReceiptMaxBytes {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_request_body"})
			return
		}
		payload, err = parseJSONMap(body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_json"})
			return
		}
	}
	workspaceID, authorization, authorized := optionalRetrievalPromotionGovernanceAuthorization(s, w, r, payload, retrievalPromotionCanaryGovernanceFeature, retrievalPromotionCanaryGovernancePath)
	if !authorized {
		return
	}
	identity := (*contextIdentityKeys)(nil)
	if s.contextMesh != nil {
		identity = s.contextMesh.identity
	}
	if identity == nil || validateContextIdentity(identity) != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "trusted_canary_signer_unavailable"})
		return
	}
	store, storeErr := s.retrievalPromotionCanaryOwner()
	if storeErr != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "canary_ledger_unavailable"})
		return
	}
	now := time.Now().UTC()
	if r.Method == http.MethodGet {
		response := retrievalPromotionCanaryResponse(store, now)
		response["ok"] = true
		response["workspace_ref"] = contextPackLearnedScopeRef("workspace", workspaceID)
		writeJSON(w, http.StatusOK, attachPayloadFormatContract(retrievalPromotionCanaryGovernanceContractID, response, "", "retrieval_promotion_canary_governance", r.URL.Path))
		return
	}
	operation := strings.ToLower(strings.TrimSpace(anyToString(payload["operation"])))
	approved, approvedOK := payload["operator_approved"].(bool)
	actor := strings.TrimSpace(anyToString(payload["actor"]))
	reason := strings.TrimSpace(anyToString(payload["reason"]))
	expectedGeneration, expectedOK := retrievalPromotionStrictInt(payload["expected_generation"])
	idempotencyKey := strings.TrimSpace(anyToString(payload["idempotency_key"]))
	operatorSubject := strings.TrimSpace(anyToString(authorization["runtime_license_subject"]))
	if operatorSubject == "" && optionalFrontierT1ExplicitDevBypassActive() {
		operatorSubject = "private-development:" + strings.ToLower(workspaceID)
	}
	if (operation != "configure" && operation != "rollback") || !approvedOK || !approved || actor == "" || actor != operatorSubject || reason == "" || len(reason) > 1000 ||
		!expectedOK || expectedGeneration < 0 || idempotencyKey == "" || len(idempotencyKey) > 160 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "invalid_canary_mutation"})
		return
	}
	project, projectErr := sanitizeMemoryProject(anyToString(payload["project"]))
	if projectErr != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "invalid_project_scope"})
		return
	}
	workspaceRef := contextPackLearnedScopeRef("workspace", workspaceID)
	projectRef := contextPackLearnedScopeRef("project", project)
	taskClassRef := retrievalPromotionGovernanceRef(payload, "task_class_ref")
	intentRef := retrievalPromotionGovernanceRef(payload, "retrieval_intent_ref")
	policyRef := retrievalPromotionGovernanceRef(payload, "policy_ref")
	snapshotRef := retrievalPromotionGovernanceRef(payload, "snapshot_ref")
	caseSetRef := retrievalPromotionGovernanceRef(payload, "case_set_ref")
	assignmentRef := retrievalPromotionGovernanceRef(payload, "assignment_subject_ref")
	if taskClassRef == "" || intentRef == "" || policyRef == "" || snapshotRef == "" || caseSetRef == "" || assignmentRef == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "exact_scope_snapshot_case_set_assignment_refs_required"})
		return
	}
	// Idempotency is resolved before the optimistic generation check. A retry
	// after a successful append must replay its original signed receipt even
	// when the caller still carries the now-stale expected generation; reusing
	// the key with any different request remains a hard conflict.
	if existing, exists := store.idempotentReceipt(idempotencyKey); exists {
		if !retrievalPromotionGovernanceRetryMatches(existing, operation, expectedGeneration, reason, workspaceRef, projectRef, taskClassRef, intentRef, policyRef, snapshotRef, caseSetRef, assignmentRef, payload) {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "canary_idempotency_conflict"})
			return
		}
		response := retrievalPromotionCanaryReplayResponse(store, existing, now)
		response["ok"] = true
		writeJSON(w, http.StatusOK, attachPayloadFormatContract(retrievalPromotionCanaryGovernanceContractID, response, "", "retrieval_promotion_canary_governance", r.URL.Path))
		return
	}
	generation, anchor, headOK := store.generationHead()
	if !headOK || expectedGeneration != int(generation) {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "stale_canary_generation", "generation": generation, "head_digest": anchor})
		return
	}
	receipt := retrievalPromotionCanaryReceipt{
		SchemaID: retrievalPromotionCanaryReceiptSchemaID, Version: 1, Operation: operation, Generation: generation + 1,
		WorkspaceRef: workspaceRef, ProjectRef: projectRef, TaskClassRef: taskClassRef, RetrievalIntentRef: intentRef,
		PolicyRef: policyRef, SnapshotRef: snapshotRef, CaseSetRef: caseSetRef, AssignmentSubjectRef: assignmentRef,
		RecordedAt: now.Format(time.RFC3339Nano), PreviousHash: anchor, IdempotencyKey: idempotencyKey,
		ReasonDigest: "sha256:" + sha256Hex(reason),
	}
	if operation == "configure" {
		shadow, shadowOK := retrievalPromotionStrictInt(payload["shadow_basis_points"])
		control, controlOK := retrievalPromotionStrictInt(payload["control_basis_points"])
		canary, canaryOK := retrievalPromotionStrictInt(payload["canary_basis_points"])
		minimumSamples, minimumSamplesOK := retrievalPromotionStrictInt(payload["minimum_canary_samples"])
		minimumDwell, minimumDwellOK := retrievalPromotionStrictInt(payload["minimum_dwell_seconds"])
		expiresAt := strings.TrimSpace(anyToString(payload["expires_at"]))
		if !shadowOK || !controlOK || !canaryOK || !minimumSamplesOK || !minimumDwellOK || shadow+control+canary != retrievalPromotionTotalBasisPoints || canary <= 0 || canary > retrievalPromotionCanaryMaxBasisPoints ||
			minimumSamples < retrievalPromotionCanaryMinimumSamples || minimumDwell < int(retrievalPromotionCanaryMinimumDwell/time.Second) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "staged_canary_weights_and_minimums_required"})
			return
		}
		expires, expiresErr := time.Parse(time.RFC3339Nano, expiresAt)
		if expiresErr != nil || !expires.After(now) || expires.Sub(now) > retrievalPromotionCanaryMaximumAge {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "canary_expiry_required_and_bounded"})
			return
		}
		receipt.ShadowBasisPoints, receipt.ControlBasisPoints, receipt.CanaryBasisPoints = shadow, control, canary
		receipt.MinimumCanarySamples, receipt.MinimumDwellSeconds, receipt.ExpiresAt = minimumSamples, minimumDwell, expiresAt
	} else {
		active, activeOK, activeReason := store.activeReceipt(now)
		if !activeOK || !retrievalPromotionGovernanceScopeMatches(active, receipt) {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "rollback_scope_or_active_canary_mismatch", "reason": activeReason})
			return
		}
		receipt.ControlBasisPoints = retrievalPromotionTotalBasisPoints
		receipt.CanaryBasisPoints = 0
		receipt.MinimumCanarySamples = active.MinimumCanarySamples
		receipt.MinimumDwellSeconds = active.MinimumDwellSeconds
		receipt.ExpiresAt = now.Add(retrievalPromotionCanaryMaximumAge).Format(time.RFC3339Nano)
	}
	var err error
	if err = retrievalPromotionSignCanaryReceipt(&receipt, identity); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "canary_receipt_signing_failed"})
		return
	}
	if operation == "configure" {
		err = store.configure(receipt, now)
	} else {
		err = store.rollback(receipt, now)
	}
	if err != nil {
		if existing, exists := store.idempotentReceipt(idempotencyKey); exists &&
			retrievalPromotionGovernanceRetryMatches(existing, operation, expectedGeneration, reason, workspaceRef, projectRef, taskClassRef, intentRef, policyRef, snapshotRef, caseSetRef, assignmentRef, payload) {
			response := retrievalPromotionCanaryReplayResponse(store, existing, now)
			response["ok"] = true
			writeJSON(w, http.StatusOK, attachPayloadFormatContract(retrievalPromotionCanaryGovernanceContractID, response, "", "retrieval_promotion_canary_governance", r.URL.Path))
			return
		}
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "canary_mutation_rejected"})
		return
	}
	response := retrievalPromotionCanaryResponse(store, now)
	response["ok"] = true
	response["operation"] = operation
	response["receipt"] = receipt
	writeJSON(w, http.StatusOK, attachPayloadFormatContract(retrievalPromotionCanaryGovernanceContractID, response, "", "retrieval_promotion_canary_governance", r.URL.Path))
}

func retrievalPromotionFormatError(prefix string, value any) error {
	return fmt.Errorf("%s: %s", prefix, clipText(anyToString(value), 160))
}
