package main

import (
	"strings"
	"testing"
)

func TestRecallResponseAttributionBindingIsCanonicalAndOrdered(t *testing.T) {
	response := composeRecallResponse(recallResponseTestInput(true))
	binding, ok := recallResponseBindingFromResponse(response)
	if !ok {
		t.Fatalf("composed response did not produce a canonical binding: %#v", response)
	}
	if !recallResponseExactOpaqueID(anyToString(binding["recall_response_id"]), "rr_") {
		t.Fatalf("response id is not an exact rr_ id: %#v", binding)
	}
	digest := anyToString(binding["recall_response_digest"])
	if !recallResponseValidDigest(digest) || digest != strings.ToLower(digest) {
		t.Fatalf("response digest is not a lowercase full sha256: %q", digest)
	}

	components := contextPackAnyList(anyMap(response["answer"])["components"])
	refs := contextPackAnyList(binding["response_component_refs"])
	if len(components) != len(refs) || len(components) == 0 || len(components) > recallResponseBindingMaxComponents {
		t.Fatalf("component/ref cardinality is invalid: components=%d refs=%d", len(components), len(refs))
	}
	for index, raw := range components {
		component := anyMap(raw)
		ref := anyMap(refs[index])
		if got := anyToInt(component["ordinal"], 0); got != index+1 {
			t.Fatalf("component ordinal is not contiguous: index=%d ordinal=%d", index, got)
		}
		if anyToString(component["component_ref"]) != anyToString(ref["component_ref"]) ||
			anyToString(component["component_digest"]) != anyToString(ref["component_digest"]) ||
			anyToInt(ref["ordinal"], 0) != index+1 {
			t.Fatalf("public component and binding ref diverged: component=%#v ref=%#v", component, ref)
		}
		if !recallResponseBindingKindsContains(anyToString(component["kind"])) {
			t.Fatalf("component kind is outside the bounded taxonomy: %#v", component)
		}
		if got := anyToString(component["component_digest"]); got != recallResponseComponentDigest(component) || !recallResponseValidDigest(got) {
			t.Fatalf("component digest is invalid: component=%#v digest=%q", component, got)
		}
	}
	if !recallResponseBindingMatchesResponse(response, binding) {
		t.Fatal("canonical binding did not match its response")
	}
	copyTarget := map[string]any{}
	if !recallResponseCopyBinding(copyTarget, binding) {
		t.Fatal("canonical binding was not copied")
	}
	if !recallResponseBindingMatchesResponse(response, copyTarget) {
		t.Fatalf("copied binding did not preserve canonical identity: %#v", copyTarget)
	}
	if !recallResponseBindingsEqual(binding, copyTarget) || recallResponseBindingKey(binding) == "" || recallResponseBindingKey(binding) != recallResponseBindingKey(copyTarget) {
		t.Fatalf("canonical equality/key drifted across copy: binding=%#v copy=%#v", binding, copyTarget)
	}
	untouched := map[string]any{"sentinel": true}
	if recallResponseCopyBinding(untouched, map[string]any{"recall_response_id": binding["recall_response_id"]}) || len(untouched) != 1 {
		t.Fatalf("partial binding mutated its destination: %#v", untouched)
	}
}

func TestRecallResponseAttributionSurvivesFinalTransportProjection(t *testing.T) {
	response := composeRecallResponse(recallResponseTestInput(true))
	response = finalizeRecallResponseTransport(response, "agent-alpha", "test", "/test/recall-response")
	binding, ok := recallResponseBindingFromResponse(response)
	if !ok || anyToString(binding["recall_response_id"]) != anyToString(response["response_id"]) || anyToString(binding["recall_response_digest"]) != anyToString(response["response_digest"]) {
		t.Fatalf("final transport left stale response identity: response=%#v binding=%#v", response, binding)
	}
	assertBoundaryContractPassed(t, recallResponseContractID, response)
}

func TestRecallResponseAttributionComponentDigestExcludesDigestField(t *testing.T) {
	response := composeRecallResponse(recallResponseTestInput(true))
	components := contextPackAnyList(anyMap(response["answer"])["components"])
	if len(components) == 0 {
		t.Fatal("expected at least one component")
	}
	component := cloneJSONMap(anyMap(components[0]))
	original := recallResponseComponentDigest(component)
	component["component_digest"] = "sha256:" + strings.Repeat("f", 64)
	if got := recallResponseComponentDigest(component); got != original {
		t.Fatalf("component digest became circular: original=%q after=%q", original, got)
	}
}

func TestRecallResponseAttributionBindingRejectsTamperOrderAndDuplicates(t *testing.T) {
	response := composeRecallResponse(recallResponseTestInput(true))
	binding, ok := recallResponseBindingFromResponse(response)
	if !ok {
		t.Fatal("failed to create canonical binding")
	}

	t.Run("component digest tamper", func(t *testing.T) {
		tampered := cloneJSONMap(response)
		answer := anyMap(tampered["answer"])
		components := contextPackAnyList(answer["components"])
		components[0].(map[string]any)["component_digest"] = "sha256:" + strings.Repeat("0", 64)
		if recallResponseBindingMatchesResponse(tampered, binding) {
			t.Fatal("tampered component digest remained attributable")
		}
	})

	if len(contextPackAnyList(anyMap(response["answer"])["components"])) > 1 {
		t.Run("component order tamper", func(t *testing.T) {
			tampered := cloneJSONMap(response)
			components := contextPackAnyList(anyMap(tampered["answer"])["components"])
			components[0], components[1] = components[1], components[0]
			if recallResponseBindingMatchesResponse(tampered, binding) {
				t.Fatal("reordered components remained attributable")
			}
		})
	}

	t.Run("duplicate component ref", func(t *testing.T) {
		tampered := cloneJSONMap(response)
		components := contextPackAnyList(anyMap(tampered["answer"])["components"])
		components = append(components, cloneJSONMap(anyMap(components[0])))
		anyMap(tampered["answer"])["components"] = components
		if _, ok := recallResponseBindingFromResponse(tampered); ok {
			t.Fatal("duplicate component ref was accepted")
		}
	})

	t.Run("partial binding", func(t *testing.T) {
		partial := map[string]any{"recall_response_id": binding["recall_response_id"]}
		if _, ok := recallResponseBindingFromSample(partial); ok {
			t.Fatal("partial reporter binding was accepted")
		}
	})

	t.Run("noncontiguous binding ordinal", func(t *testing.T) {
		tampered := cloneJSONMap(binding)
		refs := contextPackAnyList(tampered["response_component_refs"])
		refs[0].(map[string]any)["ordinal"] = 2
		if _, ok := recallResponseBindingFromSample(tampered); ok {
			t.Fatal("noncontiguous component ordinal was accepted")
		}
	})
}
