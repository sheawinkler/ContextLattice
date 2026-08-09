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
	condition      string
	ablation       string
	synthetic      bool
	snapshotDigest string
	receiptDigest  string
	canaryPolicy   recallResponseCanaryPolicy
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
	return validatedRecallResponsePolicyInput{
		condition: conditionValue, ablation: ablation, synthetic: true,
		snapshotDigest: snapshot.SnapshotDigest, receiptDigest: snapshot.ReceiptDigest,
		canaryPolicy: zeroRecallResponseCanaryPolicy{},
	}, true
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
	return composeRecallResponseWithPolicy(cloneJSONMap(snapshot.Input), policy)
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
