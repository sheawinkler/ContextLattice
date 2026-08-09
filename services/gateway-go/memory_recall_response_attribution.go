package main

import (
	"encoding/json"
	"strings"
)

// Recall-response identity is carried through the existing quality ledger as
// opaque binding data. It is deliberately not a second ledger or a public
// quality payload: the binding only proves which bounded response projection a
// quality/outcome row refers to.
const recallResponseBindingMaxComponents = 24

var recallResponseBindingKinds = map[string]struct{}{
	"exact_current_status": {}, "decision_rationale": {}, "project_continuation": {},
	"preference_constraint": {}, "timeline": {}, "procedure": {},
	"multi_memory_synthesis": {}, "conflict_supersession": {}, "negative_abstention": {},
	"memory_to_action": {},
}

func recallResponseBindingHasAnyFields(value map[string]any) bool {
	if value == nil {
		return false
	}
	for _, key := range []string{"recall_response_id", "recall_response_digest", "response_component_refs"} {
		if _, present := value[key]; present {
			return true
		}
	}
	return false
}

// recallResponseBindingFromResponse derives the canonical response binding and
// revalidates the response identity, component order, component refs, and
// component digests. It is intentionally strict so a forged response cannot
// become a durable attribution source.
func recallResponseBindingFromResponse(response map[string]any) (map[string]any, bool) {
	if response == nil || !recallResponseExactOpaqueID(anyToString(response["response_id"]), "rr_") ||
		!recallResponseValidDigest(anyToString(response["response_digest"])) {
		return nil, false
	}
	if anyToString(response["response_id"]) != recallResponseIDForResponse(response) ||
		anyToString(response["response_digest"]) != recallResponseSemanticDigest(response) {
		return nil, false
	}
	_, refs, ok := recallResponseComponentBinding(response)
	if !ok {
		return nil, false
	}
	return map[string]any{
		"recall_response_id":      anyToString(response["response_id"]),
		"recall_response_digest":  anyToString(response["response_digest"]),
		"response_component_refs": refs,
	}, true
}

// recallResponseBindingFromSample is the strict quality/outcome boundary. A
// caller may omit all binding fields for a legacy/unbound row, but a partial
// or malformed attempt is never normalized into a plausible binding.
func recallResponseBindingFromSample(sample map[string]any) (map[string]any, bool) {
	if !recallResponseBindingHasAnyFields(sample) {
		return nil, true
	}
	id := strings.TrimSpace(anyToString(sample["recall_response_id"]))
	digest := strings.TrimSpace(anyToString(sample["recall_response_digest"]))
	refsRaw, refsPresent := sample["response_component_refs"]
	if !recallResponseExactOpaqueID(id, "rr_") || !recallResponseValidDigest(digest) || !refsPresent {
		return nil, false
	}
	refs, ok := recallResponseBindingRefs(refsRaw)
	if !ok {
		return nil, false
	}
	return map[string]any{
		"recall_response_id":      id,
		"recall_response_digest":  digest,
		"response_component_refs": refs,
	}, true
}

func recallResponseBindingRefs(value any) ([]any, bool) {
	rows, ok := value.([]any)
	if !ok || len(rows) == 0 || len(rows) > recallResponseBindingMaxComponents {
		return nil, false
	}
	refs := make([]any, 0, len(rows))
	seen := map[string]struct{}{}
	for index, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok || !recallResponseBindingRefFieldsOnly(row) {
			return nil, false
		}
		ref := strings.TrimSpace(anyToString(row["component_ref"]))
		kind := strings.ToLower(strings.TrimSpace(anyToString(row["kind"])))
		digest := strings.TrimSpace(anyToString(row["component_digest"]))
		componentBinding, bindingOK := recallResponseCanonicalComponentBinding(anyMap(row["binding"]), kind)
		ordinal := anyToInt(row["ordinal"], 0)
		if !recallResponseExactOpaqueID(ref, "rrc_") || !recallResponseBindingKindsContains(kind) ||
			!recallResponseValidDigest(digest) || !bindingOK || anyToString(componentBinding["component_digest"]) != digest ||
			!recallResponseExactOrdinal(row["ordinal"], index+1) || ordinal != index+1 {
			return nil, false
		}
		if _, duplicate := seen[ref]; duplicate {
			return nil, false
		}
		seen[ref] = struct{}{}
		refs = append(refs, map[string]any{
			"component_ref":    ref,
			"component_digest": digest,
			"ordinal":          ordinal,
			"kind":             kind,
			"binding":          componentBinding,
		})
	}
	return refs, true
}

func recallResponseBindingRefFieldsOnly(row map[string]any) bool {
	if row == nil || len(row) != 5 {
		return false
	}
	for _, key := range []string{"component_ref", "component_digest", "ordinal", "kind", "binding"} {
		if _, present := row[key]; !present {
			return false
		}
	}
	return true
}

func recallResponseBindingKindsContains(kind string) bool {
	_, ok := recallResponseBindingKinds[kind]
	return ok
}

func recallResponseComponentBinding(response map[string]any) ([]any, []any, bool) {
	answer := anyMap(response["answer"])
	rows := contextPackAnyList(answer["components"])
	if len(rows) > recallResponseBindingMaxComponents {
		return nil, nil, false
	}
	refs := make([]any, 0, len(rows))
	seen := map[string]struct{}{}
	scopeDigest := anyToString(anyMap(response["request_scope"])["scope_digest"])
	for index, raw := range rows {
		component, ok := raw.(map[string]any)
		if !ok || !recallResponseComponentFieldsOnly(component) {
			return nil, nil, false
		}
		kind := strings.ToLower(strings.TrimSpace(anyToString(component["kind"])))
		ref := strings.TrimSpace(anyToString(component["component_ref"]))
		ordinal := anyToInt(component["ordinal"], 0)
		digest := strings.TrimSpace(anyToString(component["component_digest"]))
		componentBinding, bindingOK := recallResponseCanonicalComponentBinding(anyMap(component["binding"]), kind)
		if !recallResponseBindingKindsContains(kind) || !recallResponseExactOpaqueID(ref, "rrc_") ||
			!recallResponseValidDigest(digest) || !bindingOK || anyToString(componentBinding["component_digest"]) != digest ||
			!recallResponseExactOrdinal(component["ordinal"], index+1) || ordinal != index+1 {
			return nil, nil, false
		}
		expectedRef := "rrc_" + sha256Hex(scopeDigest + "\x00" + kind)[:24]
		if !recallResponseValidDigest(scopeDigest) || ref != expectedRef {
			return nil, nil, false
		}
		if _, duplicate := seen[ref]; duplicate {
			return nil, nil, false
		}
		seen[ref] = struct{}{}
		if digest != recallResponseComponentDigest(component) {
			return nil, nil, false
		}
		refs = append(refs, map[string]any{
			"component_ref":    ref,
			"component_digest": digest,
			"ordinal":          ordinal,
			"kind":             kind,
			"binding":          componentBinding,
		})
	}
	return rows, refs, true
}

func recallResponseExactOrdinal(value any, expected int) bool {
	switch typed := value.(type) {
	case int:
		return typed == expected
	case int8:
		return int(typed) == expected
	case int16:
		return int(typed) == expected
	case int32:
		return int(typed) == expected
	case int64:
		return typed == int64(expected)
	case uint:
		return typed == uint(expected)
	case uint8:
		return int(typed) == expected
	case uint16:
		return int(typed) == expected
	case uint32:
		return typed == uint32(expected)
	case uint64:
		return typed == uint64(expected)
	case float32:
		return typed == float32(expected)
	case float64:
		return typed == float64(expected)
	case json.Number:
		parsed, err := typed.Int64()
		return err == nil && parsed == int64(expected)
	default:
		return false
	}
}

func recallResponseComponentFieldsOnly(component map[string]any) bool {
	return recallResponseModuleShape(component)
}

// recallResponseComponentDigest intentionally excludes component_digest. It
// binds the complete canonical component content and its ordinal, making order
// changes and field tampering observable without a circular hash.
func recallResponseComponentDigest(component map[string]any) string {
	if component == nil {
		return ""
	}
	material := cloneJSONMap(component)
	delete(material, "component_digest")
	if binding := anyMap(material["binding"]); len(binding) > 0 {
		delete(binding, "component_digest")
	}
	return "sha256:" + sha256Hex(recallResponseCanonicalJSON(material))
}

func recallResponseIDForResponse(response map[string]any) string {
	material := cloneJSONMap(response)
	delete(material, "response_id")
	delete(material, "response_digest")
	delete(material, "format_contract")
	return "rr_" + sha256Hex(recallResponseCanonicalJSON(material))[:24]
}

func recallResponseBindingMatchesResponse(response, binding map[string]any) bool {
	canonical, ok := recallResponseBindingFromSample(binding)
	if !ok || canonical == nil {
		return false
	}
	derived, ok := recallResponseBindingFromResponse(response)
	return ok && recallResponseCanonicalJSON(canonical) == recallResponseCanonicalJSON(derived)
}

func recallResponseBindingsEqual(left, right map[string]any) bool {
	leftCanonical, leftOK := recallResponseBindingFromSample(left)
	rightCanonical, rightOK := recallResponseBindingFromSample(right)
	if !leftOK || !rightOK || leftCanonical == nil || rightCanonical == nil {
		return false
	}
	return recallResponseCanonicalJSON(leftCanonical) == recallResponseCanonicalJSON(rightCanonical)
}

func recallResponseBindingKey(binding map[string]any) string {
	canonical, ok := recallResponseBindingFromSample(binding)
	if !ok || canonical == nil {
		return ""
	}
	return "sha256:" + sha256Hex(recallResponseCanonicalJSON(canonical))
}

func recallResponseApplyBinding(response, binding map[string]any) bool {
	canonical, ok := recallResponseBindingFromSample(binding)
	if !ok || canonical == nil || !recallResponseBindingMatchesResponse(response, canonical) {
		return false
	}
	response["response_id"] = anyToString(canonical["recall_response_id"])
	response["response_digest"] = anyToString(canonical["recall_response_digest"])
	return true
}

func recallResponseCopyBinding(dst, binding map[string]any) bool {
	if dst == nil {
		return false
	}
	canonical, ok := recallResponseBindingFromSample(binding)
	if !ok || canonical == nil {
		return false
	}
	for key, value := range canonical {
		dst[key] = cloneJSONValue(value)
	}
	return true
}
