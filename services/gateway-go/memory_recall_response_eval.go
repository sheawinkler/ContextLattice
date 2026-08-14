package main

import "strings"

const recallResponseSyntheticSnapshotSchema = "recall_response.compositional_fixture_projection.v1"

type recallResponseEvalCondition string

const (
	recallResponseConditionRawRetrieval      recallResponseEvalCondition = "raw_retrieval"
	recallResponseConditionUniversalTemplate recallResponseEvalCondition = "universal_template"
	recallResponseConditionFlatRouter        recallResponseEvalCondition = "flat_category_router"
	recallResponseConditionCompositional     recallResponseEvalCondition = "compositional_router"
)

type recallResponseModuleType string

type recallResponseValidatedEvidenceBinding struct {
	authority string
}

// recallResponseFrozenSnapshot is the only input accepted by the local
// condition projector. The fixture loader is responsible for converting the
// synthetic policy input into Input once; this seam then proves that every
// condition and ablation is bound to the same frozen identities.
type recallResponseFrozenSnapshot struct {
	SchemaID          string
	Input             map[string]any
	PolicyInputDigest string
	RequestDigest     string
	SnapshotDigest    string
	ReceiptDigest     string
	InputDigest       string
}

type validatedRecallResponsePolicyInput struct {
	condition        string
	ablation         string
	synthetic        bool
	sourceBound      bool
	snapshotDigest   string
	receiptDigest    string
	evidenceBindings map[string]recallResponseValidatedEvidenceBinding
	ablationWitness  map[string]any
	canaryPolicy     recallResponseCanaryPolicy
}

func recallResponseProductionPolicyInput() validatedRecallResponsePolicyInput {
	return validatedRecallResponsePolicyInput{
		condition: string(recallResponseConditionCompositional), ablation: "none",
		canaryPolicy: recallResponseCanaryPolicyForComposition(),
	}
}

func validateRecallResponseFrozenSnapshot(
	snapshot recallResponseFrozenSnapshot,
	condition recallResponseEvalCondition,
	omitted recallResponseModuleType,
) (validatedRecallResponsePolicyInput, bool) {
	conditionValue := strings.ToLower(strings.TrimSpace(string(condition)))
	ablation := strings.ToLower(strings.TrimSpace(string(omitted)))
	if snapshot.SchemaID != recallResponseSyntheticSnapshotSchema || len(snapshot.Input) == 0 ||
		!recallResponseEvalConditionAllowed(conditionValue) || !recallResponseValidDigest(snapshot.PolicyInputDigest) ||
		!recallResponseValidDigest(snapshot.RequestDigest) || !recallResponseValidDigest(snapshot.SnapshotDigest) ||
		!recallResponseValidDigest(snapshot.ReceiptDigest) || !recallResponseValidDigest(snapshot.InputDigest) {
		return validatedRecallResponsePolicyInput{}, false
	}
	if ablation == "" {
		ablation = "none"
	}
	if ablation != "none" && !recallResponseBindingKindsContains(ablation) {
		return validatedRecallResponsePolicyInput{}, false
	}
	identity := map[string]any{
		"condition":           conditionValue,
		"ablation":            ablation,
		"policy_input_digest": snapshot.PolicyInputDigest,
		"request_digest":      snapshot.RequestDigest,
		"snapshot_digest":     snapshot.SnapshotDigest,
		"receipt_digest":      snapshot.ReceiptDigest,
	}
	if snapshot.InputDigest != "sha256:"+sha256Hex(recallResponseCanonicalJSON(identity)) {
		return validatedRecallResponsePolicyInput{}, false
	}
	policy := validatedRecallResponsePolicyInput{
		condition: conditionValue, ablation: ablation, synthetic: true, sourceBound: true,
		snapshotDigest: snapshot.SnapshotDigest, receiptDigest: snapshot.ReceiptDigest,
		canaryPolicy: zeroRecallResponseCanaryPolicy{},
	}
	policy.evidenceBindings = recallResponseValidatedEvidenceBindings(snapshot.Input, "validated_policy", nil)
	return policy, true
}

// projectRecallResponseCondition is a synthetic evaluator seam. Its unexported
// typed input cannot be constructed from production JSON request keys, and the
// live route never calls it.
func projectRecallResponseCondition(
	snapshot recallResponseFrozenSnapshot,
	condition recallResponseEvalCondition,
	omitted recallResponseModuleType,
) map[string]any {
	policy, ok := validateRecallResponseFrozenSnapshot(snapshot, condition, omitted)
	if !ok || !policy.synthetic {
		return nil
	}
	if policy.ablation != "none" {
		var witnessOK bool
		policy, witnessOK = recallResponseBindSyntheticAblationWitness(snapshot.Input, policy)
		if !witnessOK {
			return nil
		}
	}
	return composeRecallResponseWithPolicy(cloneJSONMap(snapshot.Input), policy)
}

func recallResponseBindSyntheticAblationWitness(
	input map[string]any,
	policy validatedRecallResponsePolicyInput,
) (validatedRecallResponsePolicyInput, bool) {
	if !policy.synthetic || policy.ablation == "" || policy.ablation == "none" {
		return policy, false
	}
	baselinePolicy := policy
	baselinePolicy.ablation = "none"
	baselinePolicy.ablationWitness = nil
	baseline := composeRecallResponseWithPolicy(cloneJSONMap(input), baselinePolicy)
	if baseline == nil || recallResponseIsV1Control(baseline) || !validateRecallResponseU2(baseline) {
		return policy, false
	}
	componentRef := ""
	for _, raw := range contextPackAnyList(recallResponseDisclosure(baseline)["component_union"]) {
		row := anyMap(raw)
		if anyToString(row["kind"]) == policy.ablation {
			componentRef = anyToString(row["component_ref"])
			break
		}
	}
	if strings.TrimSpace(componentRef) == "" {
		return policy, false
	}
	expectedStage, stageOK := recallResponseSyntheticAblationExpectedStage(input, policy)
	if !stageOK {
		return policy, false
	}
	policy.ablationWitness = map[string]any{
		"baseline_component_ref":     componentRef,
		"component_kind":             policy.ablation,
		"baseline_union_digest":      anyToString(recallResponseDisclosure(baseline)["union_digest"]),
		"baseline_response_identity": anyToString(baseline["response_id"]),
		"expected_failure_stage":     expectedStage,
	}
	return policy, recallResponseValidDigest(anyToString(policy.ablationWitness["baseline_union_digest"])) &&
		recallResponseExactOpaqueID(anyToString(policy.ablationWitness["baseline_response_identity"]), "rr_")
}

// recallResponseSyntheticAblationExpectedStage derives only the product stage
// selected by removing one component. The baseline identity comes from the
// actual evaluated seam; this probe cannot substitute a locally composed
// baseline for a route-owned response.
func recallResponseSyntheticAblationExpectedStage(
	input map[string]any,
	policy validatedRecallResponsePolicyInput,
) (string, bool) {
	if !policy.synthetic || policy.ablation == "" || policy.ablation == "none" {
		return "", false
	}
	probePolicy := policy
	probePolicy.synthetic = false
	probePolicy.ablationWitness = nil
	probe := composeRecallResponseWithPolicy(cloneJSONMap(input), probePolicy)
	if recallResponseIsV1Control(probe) {
		productStage := anyToString(anyMap(probe[recallResponseFallbackStageReceiptKey])["stage"])
		if !recallResponseFallbackStages[productStage] {
			return "", false
		}
		return recallResponseAblationExpectedStage(productStage), true
	}
	return "none", true
}

func recallResponseEvalConditionAllowed(value string) bool {
	switch recallResponseEvalCondition(value) {
	case recallResponseConditionRawRetrieval, recallResponseConditionUniversalTemplate,
		recallResponseConditionFlatRouter, recallResponseConditionCompositional:
		return true
	default:
		return false
	}
}

func recallResponseComposition(policy validatedRecallResponsePolicyInput, proof map[string]any) map[string]any {
	coverageStatus := recallResponseProofCoverageStatus(proof)
	fallbackReason := ""
	if coverageStatus != "satisfied" {
		fallbackReason = "unsatisfied_proof_obligation"
	}
	return map[string]any{
		"condition":       policy.condition,
		"ablation":        policy.ablation,
		"primary_module":  "v1_control",
		"ordered_modules": []any{},
		"proof_strategy":  recallResponseProofStrategy,
		"coverage_status": coverageStatus,
		"fallback_reason": fallbackReason,
	}
}
