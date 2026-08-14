package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func recallResponseCanaryTestDigest(seed string) string {
	return "sha256:" + sha256Hex("recall-response-canary-test\x00"+seed)
}

func recallResponseCanaryCandidate(t *testing.T, basisPoints int) map[string]any {
	t.Helper()
	baseline := composeRecallResponse(recallResponseTestInput(true))
	policy := fixedRecallResponseCanaryPolicy{}
	for _, raw := range contextPackAnyList(anyMap(baseline["answer"])["components"]) {
		kind := anyToString(anyMap(raw)["kind"])
		policy[kind] = recallResponseComponentPolicy{
			BasisPoints: basisPoints, PolicyVersion: "recall-response-canary-test.v1",
			ReceiptDigest: recallResponseCanaryTestDigest("policy-receipt"),
		}
	}
	input := recallResponseProductionPolicyInput()
	input.canaryPolicy = policy
	response := composeRecallResponseWithPolicy(recallResponseTestInput(true), input)
	if recallResponseIsV1Control(response) {
		t.Fatalf("valid canary fixture fell back to control: %#v", response)
	}
	return response
}

func TestRecallResponseCanaryBindingIsClosedAndDigestBound(t *testing.T) {
	response := recallResponseCanaryCandidate(t, 10000)
	binding, ok := recallResponseBindingFromResponse(response)
	if !ok {
		t.Fatalf("candidate did not produce a response binding: %#v", response)
	}
	components := contextPackAnyList(anyMap(response["answer"])["components"])
	refs := contextPackAnyList(binding["response_component_refs"])
	if len(components) == 0 || len(components) != len(refs) {
		t.Fatalf("component binding cardinality drifted: components=%d refs=%d", len(components), len(refs))
	}
	for index, raw := range components {
		component := anyMap(raw)
		componentBinding := anyMap(component["binding"])
		if !recallResponseExactFields(componentBinding, recallResponseCanonicalBindingFields) || len(componentBinding) != len(recallResponseCanonicalBindingFields) {
			t.Fatalf("component binding is not the exact closed schema: %#v", componentBinding)
		}
		if anyToString(componentBinding["arm"]) != recallResponseCanaryArmCandidate ||
			anyToString(componentBinding["component_digest"]) != anyToString(component["component_digest"]) ||
			!reflect.DeepEqual(componentBinding, anyMap(anyMap(refs[index])["binding"])) {
			t.Fatalf("component and durable response bindings diverged: component=%#v ref=%#v", component, refs[index])
		}
		changed := cloneJSONMap(component)
		anyMap(changed["binding"])["policy_version"] = "recall-response-canary-test.v2"
		if recallResponseComponentDigest(changed) == anyToString(component["component_digest"]) {
			t.Fatal("closed binding was omitted from component digest identity")
		}
	}
	changed := cloneJSONMap(response)
	first := anyMap(contextPackAnyList(anyMap(changed["answer"])["components"])[0])
	anyMap(first["binding"])["arm"] = recallResponseCanaryArmControl
	if recallResponseSemanticDigest(changed) == anyToString(response["response_digest"]) {
		t.Fatal("closed component binding was omitted from response digest identity")
	}
}

func TestRecallResponseCanaryBucketsAreStableIndependentAndBounded(t *testing.T) {
	response := composeRecallResponse(recallResponseTestInput(true))
	scope, ok := recallResponseCanaryScopeFromResponse(response)
	if !ok {
		t.Fatalf("response did not produce a canary scope: %#v", response["request_scope"])
	}
	first := recallResponseComponentBucket(scope, "exact_current_status", "policy.v1")
	if first < 0 || first >= 10000 || first != recallResponseComponentBucket(scope, "exact_current_status", "policy.v1") {
		t.Fatalf("component bucket is unstable or out of range: %d", first)
	}
	distinct := -1
	for _, module := range recallResponseModuleOrder[1:] {
		bucket := recallResponseComponentBucket(scope, module, "policy.v1")
		if bucket != first {
			distinct = bucket
			break
		}
	}
	if distinct < 0 {
		t.Fatal("independent module seed material collapsed every component bucket")
	}
	cut := first
	if distinct < cut {
		cut = distinct
	}
	cut++
	if recallResponseComponentArm(first, cut) == recallResponseComponentArm(distinct, cut) {
		t.Fatalf("independent component buckets did not permit independent arms: %d %d cut=%d", first, distinct, cut)
	}
	if recallResponseComponentArm(first, 0) != recallResponseCanaryArmControl ||
		recallResponseComponentArm(first, 10000) != recallResponseCanaryArmCandidate {
		t.Fatalf("basis-point boundaries drifted for bucket %d", first)
	}

	// Bucket collisions are expected in a 10,000-wide assignment space. They
	// must not alias the full scope identity or make the bucket self-identifying.
	seen := map[int]string{}
	collision := false
	for index := 0; index < 20000; index++ {
		candidate := scope
		candidate.OwnerRef = fmt.Sprintf("owner-collision-%d", index)
		bucket := recallResponseComponentBucket(candidate, "timeline", "policy.v1")
		if prior, exists := seen[bucket]; exists && prior != candidate.OwnerRef {
			collision = true
			break
		}
		seen[bucket] = candidate.OwnerRef
	}
	if !collision {
		t.Fatal("bounded collision fixture did not find an expected modulo collision")
	}
}

func TestRecallResponseCanaryRejectsForgedCollisionAndInexactBindings(t *testing.T) {
	forged := recallResponseRequestPayload(map[string]any{
		"project": "contextlattice", "query": "bounded",
		"canary_policy": map[string]any{"timeline": 10000}, "arm": "canary", "exposure_bucket": 0,
	})
	for _, key := range []string{"canary_policy", "arm", "exposure_bucket"} {
		if _, accepted := forged[key]; accepted {
			t.Fatalf("request-forged policy field was accepted: %s", key)
		}
	}
	response := composeRecallResponse(forged)
	for _, raw := range contextPackAnyList(anyMap(response["answer"])["components"]) {
		if anyToString(anyMap(anyMap(raw)["binding"])["arm"]) != recallResponseCanaryArmControl {
			t.Fatalf("public zero-default policy was overridden by a request: %#v", raw)
		}
	}

	candidate := recallResponseCanaryCandidate(t, 10000)
	component := anyMap(contextPackAnyList(anyMap(candidate["answer"])["components"])[0])
	closed := cloneJSONMap(anyMap(component["binding"]))
	closed["ablation"] = anyToString(component["kind"])
	if _, ok := recallResponseCanonicalComponentBinding(closed, anyToString(component["kind"])); ok {
		t.Fatal("condition/ablation component collision was accepted")
	}

	binding, ok := recallResponseBindingFromResponse(candidate)
	if !ok {
		t.Fatal("candidate binding fixture is invalid")
	}
	quality, outcome := map[string]any{}, map[string]any{"captured_at": time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)}
	if !recallResponseCopyBinding(quality, binding) || !recallResponseCopyBinding(outcome, binding) {
		t.Fatal("failed to construct exact outcome binding")
	}
	rows, eligible := recallResponseComponentOutcomeEligibility(outcome, quality, time.Now().UTC())
	if !eligible || len(rows) == 0 {
		t.Fatalf("exact retained component bindings were not outcome-eligible: %#v", rows)
	}
	for _, raw := range rows {
		row := anyMap(raw)
		if !anyToBool(row["outcome_eligible"]) || anyToBool(row["causal_credit"]) {
			t.Fatalf("whole-response outcome inflated component causal credit: %#v", row)
		}
	}

	partial := cloneJSONMap(binding)
	delete(anyMap(anyMap(contextPackAnyList(partial["response_component_refs"])[0])["binding"]), "scope_binding_digest")
	if _, ok := recallResponseBindingFromSample(partial); ok {
		t.Fatal("partial nested binding was accepted")
	}
	duplicate := cloneJSONMap(binding)
	duplicate["response_component_refs"] = append(contextPackAnyList(duplicate["response_component_refs"]), cloneJSONMap(anyMap(contextPackAnyList(duplicate["response_component_refs"])[0])))
	if _, ok := recallResponseBindingFromSample(duplicate); ok {
		t.Fatal("duplicate component binding was accepted")
	}
	other := composeRecallResponse(recallResponseTestInput(false))
	otherBinding, otherOK := recallResponseBindingFromResponse(other)
	if !otherOK || !recallResponseCopyBinding(quality, otherBinding) {
		t.Fatal("failed to construct mismatched retained binding")
	}
	if _, ok := recallResponseComponentOutcomeEligibility(outcome, quality, time.Now().UTC()); ok {
		t.Fatal("mismatched quality/outcome binding was eligible")
	}
	if _, ok := recallResponseComponentOutcomeEligibility(outcome, map[string]any{}, time.Now().UTC()); ok {
		t.Fatal("retained-row loss remained component-eligible")
	}
	if !recallResponseCopyBinding(quality, binding) {
		t.Fatal("failed to restore exact retained binding")
	}
	outcome["captured_at"] = time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	if _, ok := recallResponseComponentOutcomeEligibility(outcome, quality, time.Now().UTC()); ok {
		t.Fatal("future outcome binding was eligible")
	}
}

func TestRecallResponseCanaryClippingMismatchProjectsV1Control(t *testing.T) {
	candidate := recallResponseCanaryCandidate(t, 10000)
	component := anyMap(contextPackAnyList(anyMap(candidate["answer"])["components"])[0])
	anyMap(component["binding"])["policy_version"] = strings.Repeat("clipped-policy-identity", 400)
	projected := finalizeRecallResponseTransport(candidate, "agent-alpha", "test", "/test/recall-response")
	if !recallResponseIsV1Control(projected) || len(contextPackAnyList(anyMap(projected["answer"])["components"])) != 0 {
		t.Fatalf("clipping identity mismatch did not drop candidate identity: %#v", projected)
	}
}
