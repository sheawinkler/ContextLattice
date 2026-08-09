package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestRecallResponseU3ModulesAreUsefulClosedAndBounded(t *testing.T) {
	response := composeRecallResponse(recallResponseTestInput(true))
	answer := anyMap(response["answer"])
	modules := contextPackAnyList(answer["components"])
	if len(modules) == 0 {
		t.Fatal("compositional response emitted no modules")
	}
	primary := 0
	for index, raw := range modules {
		module := anyMap(raw)
		if anyToBool(module["primary"]) {
			primary++
		}
		if anyToInt(module["ordinal"], 0) != index+1 || anyToString(module["kind"]) != anyToString(module["module_type"]) {
			t.Fatalf("module order/identity drifted: %#v", module)
		}
		if got := anyToString(module["component_digest"]); got != recallResponseComponentDigest(module) || !recallResponseValidDigest(got) {
			t.Fatalf("module digest is not stable: %#v", module)
		}
		if !recallResponseExactFields(anyMap(module["payload"]), recallResponseModulePayloadFields[anyToString(module["kind"])]) {
			t.Fatalf("module payload is not closed: %#v", module)
		}
		if anyToString(anyMap(module["binding"])["arm"]) != "control" {
			t.Fatalf("U3 module was allowed to self-arm: %#v", module["binding"])
		}
	}
	if primary != 1 || !recallResponseValidateModules(modules, anyMap(answer["proof_spine"]), anyMap(response["request_scope"])) {
		t.Fatalf("module selection failed closed validation: primary=%d modules=%#v", primary, modules)
	}
	if findings := validateAgentContractPayload(recallResponseContractID, finalizeRecallResponseTransport(response, "agent-alpha", "test", "/test/recall-response")); len(findings) != 0 {
		t.Fatalf("U3 response failed shared contract: %#v", findings)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"super-secret", "/private/path", "Do not expose"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("module projection leaked raw evidence: %q", forbidden)
		}
	}
}

func TestRecallResponseU3PayloadsCoverAllTenTypes(t *testing.T) {
	response := composeRecallResponse(recallResponseTestInput(true))
	response["conflicts"] = []any{map[string]any{"conflict_id": "sha256:" + strings.Repeat("a", 64)}}
	response["gaps"] = []any{map[string]any{"code": "missing_proof"}}
	for _, kind := range recallResponseModuleOrder {
		payload := recallResponseModulePayload(kind, response, []string{"sha256:" + strings.Repeat("b", 64)})
		if !recallResponseExactFields(payload, recallResponseModulePayloadFields[kind]) {
			t.Fatalf("payload for %s is not closed: %#v", kind, payload)
		}
	}
}

func TestRecallResponseU3NegativePlaceholderDoesNotOverclaimExcludedEvidence(t *testing.T) {
	response := composeRecallResponse(recallResponseTestInput(true))
	response["evidence"] = []any{}
	response["gaps"] = []any{
		map[string]any{"code": "excluded_evidence"},
		map[string]any{"code": "no_bounded_evidence"},
	}
	anyMap(anyMap(response["classification"])["facets"])["evidence_state"] = "superseded"
	payload := recallResponseModulePayload("negative_abstention", response, nil)
	if anyToString(payload["terminal"]) != "unknown" {
		t.Fatalf("excluded evidence was converted into false bounded absence: %#v", payload)
	}
}

func TestRecallResponseU3HybridOrderAndProtectedSafety(t *testing.T) {
	input := recallResponseTestInput(true)
	input["query"] = "continue the project and explain why the decision was made"
	input["task_class"] = "continuation"
	input["source_coverage"] = map[string]any{"complete": false, "pending": []any{"archive"}}
	input["context_pack"].(map[string]any)["proof_claims"] = []any{map[string]any{
		"claim_id": "claim-current", "proof_status": "contested",
		"support":    []any{map[string]any{"ref_id": "rtc_" + strings.Repeat("a", 24)}},
		"opposition": []any{map[string]any{"ref_id": "claim-opposing"}},
	}}
	response := composeRecallResponse(input)
	modules := contextPackAnyList(anyMap(response["answer"])["components"])
	if len(modules) == 0 {
		t.Fatal("hybrid response emitted no modules")
	}
	seenSafety := map[string]bool{}
	for _, raw := range modules {
		kind := anyToString(anyMap(raw)["kind"])
		if recallResponseModuleSafety[kind] {
			seenSafety[kind] = true
		}
	}
	if !seenSafety["conflict_supersession"] || !seenSafety["negative_abstention"] {
		t.Fatalf("protected safety modules were dropped: %#v", modules)
	}
	proof := anyMap(anyMap(response["answer"])["proof_spine"])
	if len(contextPackAnyList(proof["conflict_refs"])) == 0 || len(contextPackAnyList(proof["gap_refs"])) == 0 {
		t.Fatalf("protected proof references were not retained: %#v", proof)
	}
	composition := anyMap(anyMap(response["answer"])["composition"])
	if anyToString(composition["coverage_status"]) != "unsatisfied" || anyToString(anyMap(response["next_action"])["kind"]) != "retrieve_or_verify" || anyToString(anyMap(response["classification"])["posture"]) != "verify_before_action" {
		t.Fatalf("unresolved protected obligations claimed a supported action: composition=%#v next=%#v classification=%#v", composition, response["next_action"], response["classification"])
	}
}

func TestRecallResponseU3ModuleTamperAndDigestFailure(t *testing.T) {
	response := composeRecallResponse(recallResponseTestInput(true))
	modules := contextPackAnyList(anyMap(response["answer"])["components"])
	if len(modules) == 0 {
		t.Fatal("expected modules")
	}
	tampered := cloneJSONMap(response)
	first := contextPackAnyList(anyMap(tampered["answer"])["components"])[0].(map[string]any)
	first["payload"].(map[string]any)["status"] = "raw_prompt"
	if validateRecallResponseU2(tampered) {
		t.Fatal("payload tamper remained valid")
	}
	tampered = cloneJSONMap(response)
	rows := contextPackAnyList(anyMap(tampered["answer"])["components"])
	if len(rows) > 1 {
		rows[0], rows[1] = rows[1], rows[0]
		if validateRecallResponseU2(tampered) {
			t.Fatal("module order tamper remained valid")
		}
	}
	if reflect.DeepEqual(response, tampered) {
		t.Fatal("tamper test did not mutate a copy")
	}
}

func TestRecallResponseU3RejectsRehashedBindingAndPrimaryTamper(t *testing.T) {
	response := composeRecallResponse(recallResponseTestInput(true))
	tampered := cloneJSONMap(response)
	module := anyMap(contextPackAnyList(anyMap(tampered["answer"])["components"])[0])
	anyMap(module["binding"])["snapshot_digest"] = "sha256:" + strings.Repeat("e", 64)
	module["component_digest"] = recallResponseComponentDigest(module)
	if validateRecallResponseU2(tampered) {
		t.Fatal("rehashed cross-snapshot module binding remained valid")
	}

	tampered = cloneJSONMap(response)
	modules := contextPackAnyList(anyMap(tampered["answer"])["components"])
	first := anyMap(modules[0])
	first["primary"] = false
	first["component_digest"] = recallResponseComponentDigest(first)
	if validateRecallResponseU2(tampered) {
		t.Fatal("rehashed non-leading primary marker remained valid")
	}
}

func TestRecallResponseU3ProtectedModuleCannotBeRehashedAway(t *testing.T) {
	input := recallResponseTestInput(true)
	input["source_coverage"] = map[string]any{"complete": false, "pending": []any{"archive"}}
	input["context_pack"].(map[string]any)["proof_claims"] = []any{map[string]any{
		"claim_id": "claim-current", "proof_status": "contested",
		"support":    []any{map[string]any{"ref_id": "rtc_" + strings.Repeat("a", 24)}},
		"opposition": []any{map[string]any{"ref_id": "claim-opposing"}},
	}}
	response := composeRecallResponse(input)
	tampered := cloneJSONMap(response)
	answer := anyMap(tampered["answer"])
	kept := []any{}
	ordered := []string{}
	for _, raw := range contextPackAnyList(answer["components"]) {
		module := anyMap(raw)
		if anyToString(module["kind"]) == "conflict_supersession" {
			continue
		}
		module["ordinal"] = len(kept) + 1
		module["primary"] = len(kept) == 0
		module["component_digest"] = recallResponseComponentDigest(module)
		kept = append(kept, module)
		ordered = append(ordered, anyToString(module["kind"]))
	}
	answer["components"] = kept
	composition := anyMap(answer["composition"])
	composition["primary_module"] = ordered[0]
	composition["ordered_modules"] = recallResponseAnyStrings(ordered)
	if validateRecallResponseU2(tampered) {
		t.Fatal("protected conflict module could be removed by rehashing the remaining candidate")
	}
}

func TestRecallResponseU3AblationAndPostClipRecomposition(t *testing.T) {
	base := composeRecallResponse(recallResponseTestInput(true))
	baseModules := contextPackAnyList(anyMap(base["answer"])["components"])
	if len(baseModules) < 2 {
		t.Fatalf("fixture needs at least two modules for ablation: %#v", baseModules)
	}
	omitted := recallResponseModuleType(anyToString(anyMap(baseModules[0])["kind"]))
	snapshot := recallResponseTestFrozenSnapshot(recallResponseConditionCompositional, omitted)
	ablated := projectRecallResponseCondition(snapshot, recallResponseConditionCompositional, omitted)
	for _, raw := range contextPackAnyList(anyMap(ablated["answer"])["components"]) {
		if anyToString(anyMap(raw)["kind"]) == string(omitted) {
			t.Fatalf("non-safety ablation %q remained selected: %#v", omitted, ablated)
		}
	}

	finalized := finalizeRecallResponseTransport(base, "agent-alpha", "test", "/test/recall-response")
	if !validateRecallResponseU2(finalized) {
		t.Fatalf("post-clipping response lost its U3 contract: %#v", finalized)
	}
	proof := anyMap(anyMap(finalized["answer"])["proof_spine"])
	foundMinimality := false
	for _, raw := range contextPackAnyList(proof["coverage"]) {
		if anyToString(anyMap(raw)["obligation"]) == "minimal_sufficient_proof" {
			foundMinimality = true
		}
	}
	if !foundMinimality {
		t.Fatalf("post-clipping recomposition dropped minimality proof: %#v", proof)
	}
}
