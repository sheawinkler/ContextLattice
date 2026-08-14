package main

import (
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"time"
)

const (
	recallResponseComponentBucketSeedSchema = "component_bucket_seed.v1"
	recallResponseCanaryPolicySchema        = "recall_response.canary_policy.v1"
	recallResponseCanaryZeroPolicyVersion   = "recall_response.canary_policy.zero.v1"
	recallResponseCanaryArmControl          = "control"
	recallResponseCanaryArmCandidate        = "canary"
)

var recallResponseCanonicalBindingFields = []string{
	"condition", "ablation", "arm", "exposure_bucket", "policy_version",
	"proof_digest", "scope_binding_digest", "verifier_digest", "component_digest",
}

// recallResponseCanaryScope is the typed, already-normalized input to canary
// policy. It contains only opaque server-derived identity. Public callers can
// neither construct it through JSON nor supply exposure controls.
type recallResponseCanaryScope struct {
	OwnerRef              string
	WorkspaceRef          string
	ProjectRef            string
	SessionRef            string
	TaskRef               string
	TaskIdentityRef       string
	LaneRef               string
	Intent                string
	TemporalPremiseDigest string
	SnapshotDigest        string
	ReceiptDigest         string
}

type recallResponseComponentPolicy struct {
	BasisPoints   int
	PolicyVersion string
	ReceiptDigest string
}

// recallResponseCanaryPolicy is the only public-core policy seam. The default
// implementation is immutable and zero-percent; storage, mutation, and paid
// authorization live exclusively in memory_recall_response_canary_entitled.go.
type recallResponseCanaryPolicy interface {
	ComponentPolicy(recallResponseCanaryScope, string) recallResponseComponentPolicy
}

type zeroRecallResponseCanaryPolicy struct{}

func (zeroRecallResponseCanaryPolicy) ComponentPolicy(_ recallResponseCanaryScope, _ string) recallResponseComponentPolicy {
	return recallResponseComponentPolicy{BasisPoints: 0, PolicyVersion: recallResponseCanaryZeroPolicyVersion}
}

type fixedRecallResponseCanaryPolicy map[string]recallResponseComponentPolicy

func (p fixedRecallResponseCanaryPolicy) ComponentPolicy(_ recallResponseCanaryScope, module string) recallResponseComponentPolicy {
	if value, ok := p[module]; ok {
		return value
	}
	return zeroRecallResponseCanaryPolicy{}.ComponentPolicy(recallResponseCanaryScope{}, module)
}

var optionalRecallResponseCanaryPolicy = func() recallResponseCanaryPolicy {
	return zeroRecallResponseCanaryPolicy{}
}

func recallResponseCanaryPolicyForComposition() recallResponseCanaryPolicy {
	policy := optionalRecallResponseCanaryPolicy()
	if policy == nil {
		return zeroRecallResponseCanaryPolicy{}
	}
	return policy
}

func recallResponseCanaryScopeFromResponse(response map[string]any) (recallResponseCanaryScope, bool) {
	return recallResponseCanaryScopeFromRequestScope(anyMap(response["request_scope"]))
}

func recallResponseCanaryScopeFromRequestScope(scope map[string]any) (recallResponseCanaryScope, bool) {
	value := recallResponseCanaryScope{
		OwnerRef: anyToString(scope["owner_ref"]), WorkspaceRef: anyToString(scope["workspace_ref"]),
		ProjectRef: anyToString(scope["project_ref"]), SessionRef: anyToString(scope["session_ref"]),
		TaskRef: anyToString(scope["task_ref"]), TaskIdentityRef: anyToString(scope["task_identity_ref"]),
		LaneRef: anyToString(scope["execution_lane_ref"]), Intent: anyToString(scope["retrieval_intent"]),
		TemporalPremiseDigest: anyToString(scope["temporal_premise_digest"]),
		SnapshotDigest:        anyToString(scope["snapshot_digest"]), ReceiptDigest: anyToString(scope["receipt_digest"]),
	}
	if !recallResponseValidDigest(value.WorkspaceRef) || !recallResponseValidDigest(value.ProjectRef) ||
		!recallResponseValidDigest(value.TemporalPremiseDigest) || !recallResponseValidDigest(value.SnapshotDigest) ||
		!recallResponseValidDigest(value.ReceiptDigest) || strings.TrimSpace(value.OwnerRef) == "" ||
		strings.TrimSpace(value.SessionRef) == "" || strings.TrimSpace(value.TaskRef) == "" ||
		strings.TrimSpace(value.TaskIdentityRef) == "" || strings.TrimSpace(value.LaneRef) == "" ||
		strings.TrimSpace(value.Intent) == "" {
		return recallResponseCanaryScope{}, false
	}
	return value, true
}

// recallResponseComponentBucket uses exactly the component_bucket_seed.v1
// identity. Arm, bucket, response/component digests, verifier, and outcome are
// deliberately absent so none can create a self-referential or outcome-tuned
// assignment.
func recallResponseComponentBucket(scope recallResponseCanaryScope, module, policyVersion string) int {
	material := map[string]any{
		"schema_id": recallResponseComponentBucketSeedSchema,
		"owner_ref": scope.OwnerRef, "workspace_ref": scope.WorkspaceRef, "project_ref": scope.ProjectRef,
		"session_ref": scope.SessionRef, "task_ref": scope.TaskRef, "task_identity_ref": scope.TaskIdentityRef,
		"lane_ref": scope.LaneRef, "intent": scope.Intent, "module": module, "policy_version": policyVersion,
		"temporal_premise_digest": scope.TemporalPremiseDigest, "snapshot_digest": scope.SnapshotDigest,
		"receipt_digest": scope.ReceiptDigest,
	}
	digest := sha256.Sum256([]byte(recallResponseCanonicalJSON(material)))
	return int(binary.BigEndian.Uint64(digest[:8]) % 10000)
}

func recallResponseResolveComponentPolicy(policy recallResponseCanaryPolicy, scope recallResponseCanaryScope, module string) recallResponseComponentPolicy {
	if policy == nil {
		policy = zeroRecallResponseCanaryPolicy{}
	}
	resolved := policy.ComponentPolicy(scope, module)
	if resolved.BasisPoints < 0 || resolved.BasisPoints > 10000 || strings.TrimSpace(resolved.PolicyVersion) == "" {
		return zeroRecallResponseCanaryPolicy{}.ComponentPolicy(scope, module)
	}
	if resolved.BasisPoints > 0 && !recallResponseValidDigest(resolved.ReceiptDigest) {
		return zeroRecallResponseCanaryPolicy{}.ComponentPolicy(scope, module)
	}
	return resolved
}

func recallResponseComponentArm(bucket, basisPoints int) string {
	if basisPoints > 0 && bucket >= 0 && bucket < basisPoints {
		return recallResponseCanaryArmCandidate
	}
	return recallResponseCanaryArmControl
}

func recallResponseCanaryPolicySnapshotFromResponse(response map[string]any) (recallResponseCanaryPolicy, bool) {
	resolved := fixedRecallResponseCanaryPolicy{}
	components := contextPackAnyList(anyMap(response["answer"])["components"])
	if len(components) == 0 {
		return zeroRecallResponseCanaryPolicy{}, false
	}
	for _, raw := range components {
		module := anyMap(raw)
		if !recallResponseModuleShape(module) {
			return zeroRecallResponseCanaryPolicy{}, false
		}
		kind := anyToString(module["kind"])
		binding, ok := recallResponseCanonicalComponentBinding(anyMap(module["binding"]), kind)
		if !ok {
			return zeroRecallResponseCanaryPolicy{}, false
		}
		bucket := anyToInt(binding["exposure_bucket"], 0)
		basisPoints := 0
		if anyToString(binding["arm"]) == recallResponseCanaryArmCandidate {
			basisPoints = bucket + 1
		}
		resolved[kind] = recallResponseComponentPolicy{
			BasisPoints: basisPoints, PolicyVersion: anyToString(binding["policy_version"]),
			ReceiptDigest: anyToString(anyMap(response["request_scope"])["receipt_digest"]),
		}
	}
	return resolved, true
}

func recallResponseCanonicalComponentBinding(binding map[string]any, module string) (map[string]any, bool) {
	if !recallResponseExactFields(binding, recallResponseCanonicalBindingFields) {
		return nil, false
	}
	condition := anyToString(binding["condition"])
	ablation := anyToString(binding["ablation"])
	arm := anyToString(binding["arm"])
	bucket, bucketOK := recallResponseExactInteger(binding["exposure_bucket"])
	if !recallResponseEvalConditionAllowed(condition) ||
		(ablation != "none" && !recallResponseModuleAllowed(ablation)) || ablation == module ||
		(arm != recallResponseCanaryArmControl && arm != recallResponseCanaryArmCandidate) ||
		!bucketOK || bucket < 0 || bucket >= 10000 || strings.TrimSpace(anyToString(binding["policy_version"])) == "" ||
		(anyToString(binding["policy_version"]) == recallResponseCanaryZeroPolicyVersion && arm != recallResponseCanaryArmControl) {
		return nil, false
	}
	for _, key := range []string{"proof_digest", "scope_binding_digest", "verifier_digest", "component_digest"} {
		if !recallResponseValidDigest(anyToString(binding[key])) {
			return nil, false
		}
	}
	out := make(map[string]any, len(recallResponseCanonicalBindingFields))
	for _, key := range recallResponseCanonicalBindingFields {
		out[key] = cloneJSONValue(binding[key])
	}
	return out, true
}

func recallResponseExactInteger(value any) (int, bool) {
	parsed := anyToInt(value, -1)
	if parsed < 0 {
		return 0, false
	}
	return parsed, recallResponseExactOrdinal(value, parsed)
}

func recallResponseSealComponentIdentity(component map[string]any) bool {
	if component == nil {
		return false
	}
	binding := anyMap(component["binding"])
	delete(binding, "component_digest")
	delete(component, "component_digest")
	digest := recallResponseComponentDigest(component)
	if !recallResponseValidDigest(digest) {
		return false
	}
	binding["component_digest"] = digest
	component["component_digest"] = digest
	return true
}

func recallResponseComponentBindingRows(binding map[string]any) ([]any, bool) {
	canonical, ok := recallResponseBindingFromSample(binding)
	if !ok || canonical == nil {
		return nil, false
	}
	rows := contextPackAnyList(canonical["response_component_refs"])
	if len(rows) == 0 {
		return nil, false
	}
	out := make([]any, 0, len(rows))
	for _, raw := range rows {
		row := anyMap(raw)
		componentBinding, valid := recallResponseCanonicalComponentBinding(anyMap(row["binding"]), anyToString(row["kind"]))
		if !valid || anyToString(componentBinding["component_digest"]) != anyToString(row["component_digest"]) {
			return nil, false
		}
		out = append(out, map[string]any{
			"component_ref": row["component_ref"], "kind": row["kind"], "ordinal": row["ordinal"],
			"binding": componentBinding,
		})
	}
	return out, true
}

// recallResponseComponentOutcomeEligibility is deliberately separate from
// causal credit. Exact retained equality makes a component outcome observable;
// it does not make a whole-response result causal for that component.
func recallResponseComponentOutcomeEligibility(outcome, quality map[string]any, now time.Time) ([]any, bool) {
	outcomeBinding, outcomeOK := recallResponseBindingFromSample(outcome)
	qualityBinding, qualityOK := recallResponseBindingFromSample(quality)
	if !outcomeOK || !qualityOK || outcomeBinding == nil || qualityBinding == nil ||
		!recallResponseBindingsEqual(outcomeBinding, qualityBinding) {
		return nil, false
	}
	if captured := firstNonEmptyStrings(anyToString(outcome["gateway_received_at"]), anyToString(outcome["capturedAt"]), anyToString(outcome["captured_at"])); captured != "" {
		parsed, err := time.Parse(time.RFC3339Nano, captured)
		if err != nil || parsed.After(now.UTC()) {
			return nil, false
		}
	}
	bindings, ok := recallResponseComponentBindingRows(outcomeBinding)
	if !ok {
		return nil, false
	}
	rows := make([]any, 0, len(bindings))
	for _, raw := range bindings {
		row := anyMap(raw)
		componentBinding := anyMap(row["binding"])
		rows = append(rows, map[string]any{
			"component_ref": row["component_ref"], "kind": row["kind"], "ordinal": row["ordinal"],
			"binding": cloneJSONMap(componentBinding), "outcome_eligible": true, "causal_credit": false,
			"causal_reason": "matched_control_or_same_snapshot_ablation_required",
		})
	}
	return rows, true
}

func recallResponseCanonicalComponentOutcomes(sample map[string]any) ([]any, bool) {
	raw, present := sample["recall_response_component_outcomes"]
	if !present {
		return nil, true
	}
	binding, bindingOK := recallResponseBindingFromSample(sample)
	bindings, bindingsOK := recallResponseComponentBindingRows(binding)
	rows, rowsOK := raw.([]any)
	if !bindingOK || binding == nil || !bindingsOK || !rowsOK || len(rows) != len(bindings) {
		return nil, false
	}
	out := make([]any, 0, len(rows))
	for index, item := range rows {
		row := anyMap(item)
		bound := anyMap(bindings[index])
		if !recallResponseExactFields(row, []string{"component_ref", "kind", "ordinal", "binding", "outcome_eligible", "causal_credit", "causal_reason"}) ||
			anyToString(row["component_ref"]) != anyToString(bound["component_ref"]) ||
			anyToString(row["kind"]) != anyToString(bound["kind"]) ||
			!recallResponseExactOrdinal(row["ordinal"], index+1) || !anyToBool(row["outcome_eligible"]) ||
			anyToBool(row["causal_credit"]) || anyToString(row["causal_reason"]) != "matched_control_or_same_snapshot_ablation_required" ||
			!recallResponseExactFields(anyMap(row["binding"]), recallResponseCanonicalBindingFields) ||
			recallResponseCanonicalJSON(row["binding"]) != recallResponseCanonicalJSON(bound["binding"]) {
			return nil, false
		}
		out = append(out, cloneJSONMap(row))
	}
	return out, true
}

// recallResponseFitCandidateBudget removes only optional secondary modules,
// preserving the primary and every protected conflict/negative module. Each
// retained component is resealed before the final closed-contract check.
func recallResponseFitCandidateBudget(candidate map[string]any) bool {
	answer := anyMap(candidate["answer"])
	proof := anyMap(answer["proof_spine"])
	for attempts := 0; attempts < recallResponseMaxEvidence+recallResponseMaxModules; attempts++ {
		compactBytes, compactTokens := recallResponseCompactBudget(candidate)
		if compactBytes <= recallResponseMaxCompactBytes && compactTokens <= recallResponseMaxCompactTokens {
			return validateRecallResponseU2(candidate)
		}
		// Evidence outside the already minimized proof is useful context, but it
		// must yield before a proof-bound module. Remove lowest-ranked optional
		// evidence first; primary, conflict, gap, and module witnesses are all in
		// the proof set and therefore cannot be removed here.
		if recallResponsePruneLowestUnprovedEvidence(candidate, proof) {
			continue
		}
		if recallResponsePruneDerivedInferencePresentation(candidate) {
			continue
		}
		if !recallResponsePruneOptionalSecondaryModule(candidate) {
			return false
		}
	}
	return false
}

// recallResponsePruneDerivedInferencePresentation removes only the redundant
// response-state annotation produced by recallResponseInferences. The row is
// explicitly not a memory fact, is outside every source/union membership
// digest, and repeats state/confidence values that remain in their
// authoritative fields. An unknown inference shape is retained fail-closed.
func recallResponsePruneDerivedInferencePresentation(candidate map[string]any) bool {
	inferences := contextPackAnyList(candidate["inferences"])
	if len(inferences) == 0 {
		return false
	}
	for _, raw := range inferences {
		row := anyMap(raw)
		if !recallResponseExactFields(row, []string{"inference_id", "claim_ref", "basis_refs", "status", "confidence", "disclosure"}) ||
			anyToString(row["status"]) != "deterministic_metadata_only" ||
			anyToString(row["claim_ref"]) != "response_state" {
			return false
		}
	}
	candidate["inferences"] = []any{}
	candidate["response_id"] = recallResponseIDForResponse(candidate)
	candidate["response_digest"] = recallResponseSemanticDigest(candidate)
	return true
}

// recallResponseCompactCursorPresentation reduces only deterministic
// explanatory prose when an owner-issued cursor is present and the complete
// transport envelope is over budget. It never changes refs, counts, digests,
// statuses, safety booleans, action permissions, or the continuation contract.
// The cursor's server-owned membership and all proof/omission receipts remain
// authoritative; this is presentation-only budget fitting.
func recallResponseCompactCursorPresentation(candidate map[string]any) bool {
	disclosure := recallResponseDisclosure(candidate)
	if anyToString(anyMap(disclosure["continuation_action"])["kind"]) != "continue_snapshot" {
		return false
	}
	changed := false
	answer := anyMap(candidate["answer"])
	answerMode := anyToString(answer["answer_mode"])
	compactSummary := "Bounded evidence; unresolved limits require verification."
	if answerMode == "answer" {
		compactSummary = "Bounded evidence; inspect proof before relying."
	} else if answerMode == "abstention" {
		compactSummary = "No bounded answer; verify before acting."
	}
	if anyToString(answer["summary"]) != compactSummary {
		answer["summary"] = compactSummary
		changed = true
	}
	if progressive := anyMap(answer["progressive_disclosure"]); len(progressive) > 0 {
		if anyToString(progressive["next_level_requires"]) != "Explicit proof request." {
			progressive["next_level_requires"] = "Explicit proof request."
			changed = true
		}
	}
	nextAction := anyMap(candidate["next_action"])
	compactLabel, compactReason := "Retrieve or verify", "Advisory only; verify before acting."
	if anyToString(nextAction["kind"]) == "inspect_proof" {
		compactLabel, compactReason = "Inspect proof", "Verify proof before relying."
	}
	if anyToString(nextAction["label"]) != compactLabel {
		nextAction["label"] = compactLabel
		changed = true
	}
	if anyToString(nextAction["reason"]) != compactReason {
		nextAction["reason"] = compactReason
		changed = true
	}
	actionBoundary := anyMap(candidate["action_boundary"])
	if anyToString(actionBoundary["reason"]) != "Advisory only; independently authorize mutations." {
		actionBoundary["reason"] = "Advisory only; independently authorize mutations."
		changed = true
	}
	if anyToString(disclosure["inference_boundary"]) != "Opaque refs only." {
		disclosure["inference_boundary"] = "Opaque refs only."
		changed = true
	}
	if anyToString(disclosure["omission_policy"]) != "Presentation only; union membership is count-, digest-, and continuation-bound." {
		disclosure["omission_policy"] = "Presentation only; union membership is count-, digest-, and continuation-bound."
		changed = true
	}
	if changed {
		candidate["response_id"] = recallResponseIDForResponse(candidate)
		candidate["response_digest"] = recallResponseSemanticDigest(candidate)
	}
	return changed
}

func recallResponsePruneOptionalSecondaryModule(candidate map[string]any) bool {
	answer := anyMap(candidate["answer"])
	composition := anyMap(answer["composition"])
	proof := anyMap(answer["proof_spine"])
	scope := anyMap(candidate["request_scope"])
	modules := contextPackAnyList(answer["components"])
	remove := -1
	for index := len(modules) - 1; index > 0; index-- {
		module := anyMap(modules[index])
		componentRef := anyToString(module["component_ref"])
		kind := anyToString(module["kind"])
		readyAction := kind == "memory_to_action" && recallResponseActionPayloadReady(anyMap(module["payload"]))
		if !recallResponseModuleSafety[kind] && !readyAction && !recallResponseNonExclusionProtected(candidate, componentRef) {
			remove = index
			break
		}
	}
	if remove < 0 {
		return false
	}
	removedRef := anyToString(anyMap(modules[remove])["component_ref"])
	removedKind := anyToString(anyMap(modules[remove])["kind"])
	modules = append(append([]any(nil), modules[:remove]...), modules[remove+1:]...)
	ordered := make([]string, 0, len(modules))
	for index, raw := range modules {
		module := anyMap(raw)
		module["ordinal"] = index + 1
		module["primary"] = index == 0
		if !recallResponseSealComponentIdentity(module) {
			return false
		}
		ordered = append(ordered, anyToString(module["kind"]))
	}
	answer["components"] = modules
	coverage := []any{}
	for _, raw := range contextPackAnyList(proof["coverage"]) {
		if anyToString(anyMap(raw)["obligation"]) != "module:"+removedKind {
			coverage = append(coverage, raw)
		}
	}
	proof["coverage"] = coverage
	composition["primary_module"] = ordered[0]
	composition["ordered_modules"] = recallResponseAnyStrings(ordered)
	if !recallResponseValidateModules(modules, proof, scope) {
		return false
	}
	recallResponseRecordOmission(candidate, removedRef, "component", "response_budget_secondary_module", false, recallResponseBudgetOmissionOutcome(candidate))
	candidate["response_id"] = recallResponseIDForResponse(candidate)
	candidate["response_digest"] = recallResponseSemanticDigest(candidate)
	return true
}

func recallResponsePruneLowestUnprovedEvidence(candidate, proof map[string]any) bool {
	evidence := contextPackAnyList(candidate["evidence"])
	proofSet := map[string]bool{}
	for _, raw := range contextPackAnyList(proof["proof_refs"]) {
		proofSet[anyToString(raw)] = true
	}
	remove := -1
	for index := len(evidence) - 1; index >= 0; index-- {
		row := anyMap(evidence[index])
		refID := anyToString(row["ref_id"])
		// The complete typed union remains server-owned and continues through the
		// cursor. Presentation may omit any unproved, unprotected row—regardless
		// of its display role—to keep the initial/full envelope within budget.
		// Protected rows and proof witnesses remain non-omissible.
		if !proofSet[refID] && !recallResponseNonExclusionProtected(candidate, refID) {
			remove = index
			break
		}
	}
	if remove < 0 {
		return false
	}
	removedRef := anyToString(anyMap(evidence[remove])["ref_id"])
	evidence = append(append([]any(nil), evidence[:remove]...), evidence[remove+1:]...)
	candidate["evidence"] = evidence
	answer := anyMap(candidate["answer"])
	claimRefs := make([]any, 0, len(evidence))
	for _, raw := range evidence {
		if ref := strings.TrimSpace(anyToString(anyMap(raw)["ref_id"])); ref != "" {
			claimRefs = append(claimRefs, ref)
		}
	}
	answer["claim_refs"] = claimRefs
	state := anyMap(candidate["state"])
	conflicts := contextPackAnyList(candidate["conflicts"])
	gaps := contextPackAnyList(candidate["gaps"])
	state["evidence_count"] = len(evidence)
	state["status"] = recallResponseStateStatus(len(evidence), len(conflicts), len(gaps))
	confidence := recallResponseConfidence(evidence, map[string]any{"complete": anyToBool(state["source_complete"])}, conflicts, gaps)
	candidate["confidence"] = confidence
	candidate["inferences"] = recallResponseInferences(evidence, confidence, anyToString(state["status"]))
	recallResponseRecordOmission(candidate, removedRef, "evidence", "response_budget_context", false, recallResponseBudgetOmissionOutcome(candidate))
	candidate["response_id"] = recallResponseIDForResponse(candidate)
	candidate["response_digest"] = recallResponseSemanticDigest(candidate)
	return true
}

// Budget pruning may only claim no-loss after the closed, source-bound control
// receipt compares the candidate union and proof identities in this snapshot.
// Missing authority, clipping, or a mismatched receipt remains promotion-
// blocking and is recorded as unverified.
func recallResponseBudgetOmissionOutcome(response map[string]any) string {
	counterfactual := recallResponseOmissionCounterfactual(response, "no_loss_verified", false)
	if anyToBool(counterfactual["verified"]) {
		return "no_loss_verified"
	}
	return "not_verified"
}

func projectRecallResponseV1ControlFromArtifacts(input map[string]any, policy validatedRecallResponsePolicyInput) map[string]any {
	policy.sourceBound = policy.sourceBound && recallResponseValidDigest(policy.snapshotDigest) && recallResponseValidDigest(policy.receiptDigest)
	prepared, asOf := recallResponsePrepareTemporalInput(input)
	if sourceSnapshot := recallResponseSourceSnapshotForInput(prepared); len(sourceSnapshot) > 0 {
		policy.sourceBound = policy.sourceBound && anyToBool(sourceSnapshot["complete"]) &&
			recallResponseSourceSnapshotValidForInput(prepared, sourceSnapshot)
	} else if !policy.synthetic {
		policy.sourceBound = false
	}
	control := composeRecallResponseV1Control(prepared, policy, asOf)
	policy, _ = recallResponseBindArtifactIdentity(prepared, control, policy, asOf)
	recallResponseEnsureControlReceipt(control, policy, asOf)
	return recallResponseFailClosedU2Control(control, policy, asOf)
}

func recallResponseCandidateOrControl(control, candidate map[string]any, policy validatedRecallResponsePolicyInput, asOf string, valid bool, receipts ...map[string]any) map[string]any {
	if !valid || candidate == nil {
		fallback := recallResponseFailClosedU2Control(control, policy, asOf)
		stage := ""
		if len(receipts) > 0 {
			recallResponseAttachFallbackStageReceipt(fallback, receipts[0])
			stage = anyToString(receipts[0]["stage"])
		}
		if policy.synthetic && policy.ablation != "" && policy.ablation != "none" {
			recallResponseSealAblationFailureWitness(fallback, policy, stage)
		}
		return fallback
	}
	return candidate
}
