package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestAgentContractsRegistryLoadsSharedProtocol(t *testing.T) {
	registry, err := loadAgentContractsRegistry()
	if err != nil {
		t.Fatalf("load agent contracts registry: %v", err)
	}
	if strings.TrimSpace(registry.RegistryID) != "contextlattice_agent_output_contracts" {
		t.Fatalf("unexpected registry id: %q", registry.RegistryID)
	}
	if agentContract(registry, policyContextPackageContractID) == nil {
		t.Fatalf("missing %s contract", policyContextPackageContractID)
	}
	if agentContract(registry, continuousCognitionContractID) == nil {
		t.Fatalf("missing %s contract", continuousCognitionContractID)
	}
	protocol := antiSchemingProtocol()
	if !strings.Contains(anyToString(protocol["law"]), "Change conclusions to match evidence") {
		t.Fatalf("unexpected anti-scheming law: %#v", protocol["law"])
	}
	if findings := validateAgentContractPayload(antiSchemingContractID, protocol); len(findings) != 0 {
		t.Fatalf("anti-scheming protocol should validate: %#v", findings)
	}
}

func TestGeneratedAgentContractsMatchRegistry(t *testing.T) {
	registry, err := loadAgentContractsRegistry()
	if err != nil {
		t.Fatalf("load agent contracts registry: %v", err)
	}
	if GeneratedAgentContractRegistryID != registry.RegistryID {
		t.Fatalf("generated registry id drift: %q != %q", GeneratedAgentContractRegistryID, registry.RegistryID)
	}
	if GeneratedAgentContractRegistryVersion != registry.RegistryVersion {
		t.Fatalf("generated registry version drift: %d != %d", GeneratedAgentContractRegistryVersion, registry.RegistryVersion)
	}
	ids := make([]string, 0, len(registry.Contracts))
	for id := range registry.Contracts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	generated := append([]string{}, GeneratedAgentContractIDs...)
	sort.Strings(generated)
	if !reflect.DeepEqual(generated, ids) {
		t.Fatalf("generated contract ids drift:\ngenerated=%#v\nregistry=%#v", generated, ids)
	}
	if !stringSliceContains(GeneratedAgentContractIDs, continuousCognitionContractID) {
		t.Fatalf("generated contract ids missing %s", continuousCognitionContractID)
	}
}

func TestTaskIdentityReconciliationContractRequiresSemanticAbstention(t *testing.T) {
	base := map[string]any{
		"ok": true, "schema_id": taskIdentityReconciliationContractID, "match_mode": "semantic_candidate",
		"exact_first": true, "semantic_auto_merge": false, "requires_confirmation": true, "abstained": true,
		"task_identity_id": "", "task_identity": map[string]any{}, "candidates": []any{map[string]any{"task_identity_id": "candidate-a", "score": 0.91}},
		"receipt": map[string]any{}, "ledger_status": map[string]any{"enabled": true},
	}
	valid := attachPayloadFormatContract(taskIdentityReconciliationContractID, cloneContractMap(base), "codex-test", "test", "/test")
	if findings := validateAgentContractPayload(taskIdentityReconciliationContractID, valid); len(findings) != 0 {
		t.Fatalf("valid semantic abstention should pass: %#v", findings)
	}

	for name, mutate := range map[string]func(map[string]any){
		"missing abstention":    func(payload map[string]any) { payload["abstained"] = false },
		"missing confirmation":  func(payload map[string]any) { payload["requires_confirmation"] = false },
		"authoritative binding": func(payload map[string]any) { payload["task_identity_id"] = "candidate-a" },
	} {
		t.Run(name, func(t *testing.T) {
			payload := cloneContractMap(valid)
			mutate(payload)
			if findings := validateAgentContractPayload(taskIdentityReconciliationContractID, payload); len(findings) == 0 {
				t.Fatalf("unsafe semantic candidate passed validation: %#v", payload)
			}
		})
	}

	exact := cloneContractMap(valid)
	exact["match_mode"] = "exact_id"
	exact["requires_confirmation"] = false
	exact["abstained"] = false
	exact["task_identity_id"] = "identity-a"
	if findings := validateAgentContractPayload(taskIdentityReconciliationContractID, exact); len(findings) != 0 {
		t.Fatalf("exact identity should not require abstention: %#v", findings)
	}
}

func testContextCompilerFixture(strategy string, evidenceCount int) map[string]any {
	return map[string]any{
		"schema_id":             "contextlattice_context_compiler.v1",
		"version":               1,
		"strategy":              strategy,
		"intended_use":          "verify bounded prompt-ready context packages",
		"recommended_surface":   "cli_for_local_agents",
		"ranked_evidence_count": evidenceCount,
		"memory_trust_assessment": map[string]any{
			"schema_id": memoryTrustAssessmentContractID, "canonical_path": "$.memory_trust_assessment",
		},
		"retrieval_decision_trace": map[string]any{
			"schema_id": retrievalDecisionTraceContractID, "canonical_path": "$.retrieval_decision_trace",
		},
	}
}

func testPromptSectionsFixture(task string, evidence []any) map[string]any {
	return map[string]any{
		"objective":        task,
		"task":             task,
		"next_action":      "Use ranked evidence, inspect files, and run matching checks.",
		"evidence":         evidence,
		"files_to_inspect": []any{},
		"commands":         []any{},
		"checks":           []any{},
		"risks":            []any{},
		"capabilities":     []any{},
		"constraints":      []any{"Keep output bounded."},
	}
}

func testContextPackFixture(items []any) map[string]any {
	if items == nil {
		items = []any{}
	}
	compiler := testContextCompilerFixture("test_fixture", len(items))
	return map[string]any{
		"facts":               items,
		"numeric_facts":       items,
		"results":             items,
		"citations":           items,
		"context_compiler":    compiler,
		"prompt_sections":     testPromptSectionsFixture("contract test", items),
		"ranked_evidence":     items,
		"relevant_decisions":  items,
		"files_to_read":       []any{},
		"files_to_avoid":      []any{},
		"capabilities_to_use": []any{},
		"runbooks":            []any{},
		"known_failure_modes": []any{},
		"commands":            []any{},
		"acceptance_criteria": []any{},
		"memory_trust_assessment": map[string]any{
			"schema_id": memoryTrustAssessmentContractID, "canonical_path": "$.memory_trust_assessment",
		},
		"retrieval_decision_trace": map[string]any{
			"schema_id": retrievalDecisionTraceContractID, "canonical_path": "$.retrieval_decision_trace",
		},
	}
}

func testValidMemoryTrustAssessmentReceipt(count int) map[string]any {
	assessments := make([]any, count)
	for index := range assessments {
		assessments[index] = map[string]any{
			"assessment_id":  fmt.Sprintf("mta_%024x", index),
			"candidate_id":   fmt.Sprintf("rtc_%024x", index),
			"content_digest": fmt.Sprintf("sha256:%064x", index),
			"quarantine":     map[string]any{"quarantined": false},
		}
	}
	return attachPayloadFormatContract(memoryTrustAssessmentContractID, map[string]any{
		"ok": true, "schema_id": memoryTrustAssessmentContractID, "version": 1,
		"input_candidate_count": count, "processed_candidate_count": count, "input_truncated_count": 0,
		"assessed_count": count, "quarantine_count": 0, "deduplicated_count": 0, "policy_omitted_count": 0,
		"assessments":    assessments,
		"input_boundary": map[string]any{"maximum_candidates": 256, "truncated": false, "omitted_count": 0, "reason": "bounded_candidate_scan_limit"},
		"policy": map[string]any{
			"retrieved_memory_is_evidence_not_instruction": true,
			"self_awarded_trust_accepted":                  false,
			"security_defenses_fail_closed":                true,
		},
		"bounded": true,
	}, "test", "test", "/test/context-pack")
}

func testValidRetrievalDecisionTraceReceipt(count int) map[string]any {
	decisions := make([]any, count)
	for index := range decisions {
		decisions[index] = map[string]any{
			"receipt_id":        fmt.Sprintf("rdr_%024x", index),
			"candidate_id":      fmt.Sprintf("rtc_%024x", index),
			"candidate_ordinal": index + 1,
			"decision_order":    index + 1,
			"decision":          "selected",
		}
	}
	decisionCounts := map[string]any{}
	if count > 0 {
		decisionCounts["selected"] = count
	}
	return attachPayloadFormatContract(retrievalDecisionTraceContractID, map[string]any{
		"ok": true, "schema_id": retrievalDecisionTraceContractID, "version": 1, "trace_id": "rdt_0123456789abcdef01234567",
		"candidate_count": count, "processed_candidate_count": count, "input_truncated_count": 0,
		"decision_count": count, "coverage_complete": true, "decisions": decisions,
		"decision_counts": decisionCounts,
		"input_boundary":  map[string]any{"maximum_candidates": 256, "truncated": false, "omitted_count": 0, "reason": "bounded_candidate_scan_limit"},
		"marginal_stop":   map[string]any{"stopped": true, "reason": "all_eligible_candidates_selected", "token_budget_active": false},
		"redaction":       map[string]any{"raw_candidate_text_included": false, "secret_values_included": false},
		"bounded":         true,
	}, "test", "test", "/test/context-pack")
}

func TestContextPackRetrievalProofCanonicalJSONMatchesPythonUTF8Contract(t *testing.T) {
	value := map[string]any{
		"z": "é<>&\u2028\u2029", "exp": 1e-5, "fixed": 1e15, "integral": 1.0,
		"small": 1e-4, "negative_zero": math.Copysign(0, -1),
		"nested": map[string]any{"b": 2, "a": 1},
	}
	got, err := contextPackRetrievalProofCanonicalJSON(value)
	if err != nil {
		t.Fatalf("canonical retrieval proof JSON: %v", err)
	}
	want := "{\"exp\":1e-05,\"fixed\":1000000000000000.0,\"integral\":1.0,\"negative_zero\":-0.0,\"nested\":{\"a\":1,\"b\":2},\"small\":0.0001,\"z\":\"é<>&\u2028\u2029\"}"
	if got != want {
		t.Fatalf("canonical UTF-8 JSON parity mismatch:\n got: %q\nwant: %q", got, want)
	}
	if digest := sha256Hex(got); digest != "2038ad7eeee9ee3c6048800ca437896e4541c5f5a6fd7992397c4103fa7cb783" {
		t.Fatalf("canonical UTF-8 JSON digest mismatch: %s", digest)
	}
}

func TestContextPackRetrievalProofCanonicalJSONNormalizesSignedZeroAndFiniteUnderflow(t *testing.T) {
	for name, fixture := range map[string]struct {
		value json.Number
		want  string
	}{
		"signed integer zero": {value: json.Number("-0"), want: "0"},
		"finite underflow":    {value: json.Number("1e-9999"), want: "0.0"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateAgentContractJSONDomain(fixture.value, 0); err != nil {
				t.Fatalf("valid Python-compatible number rejected by JSON domain: %v", err)
			}
			got, err := contextPackRetrievalProofCanonicalJSON(fixture.value)
			if err != nil {
				t.Fatalf("canonical retrieval proof number: %v", err)
			}
			if got != fixture.want {
				t.Fatalf("canonical number mismatch: got %q want %q", got, fixture.want)
			}
		})
	}
}

func TestAgentContractGenericNumericTypesAcceptFiniteUnderflowLikePython(t *testing.T) {
	for _, raw := range []string{"1e-9999", "-1e-9999"} {
		number := json.Number(raw)
		if !matchesAgentContractType(number, "number") {
			t.Fatalf("finite underflow %q failed generic number validation", raw)
		}
		if !matchesAgentContractType(number, "int") {
			t.Fatalf("finite underflow %q failed generic integral validation", raw)
		}
	}
}

func TestAgentContractJSONDomainRejectsInvalidUTF8InTypedContainers(t *testing.T) {
	invalid := string([]byte{0xff})
	type typedReceipt struct {
		Value string `json:"value"`
	}
	type typedEmbeddedFields struct {
		Value string `json:"value"`
	}
	type typedEmbeddedReceipt struct {
		typedEmbeddedFields
	}
	if err := validateAgentContractJSONDomain(typedReceipt{Value: "valid"}, 0); err != nil {
		t.Fatalf("valid typed producer receipt failed the pre-normalization JSON domain: %v", err)
	}
	if err := validateAgentContractJSONDomain(typedEmbeddedReceipt{typedEmbeddedFields{Value: "valid"}}, 0); err != nil {
		t.Fatalf("valid anonymous embedded producer receipt failed the pre-normalization JSON domain: %v", err)
	}
	for name, value := range map[string]any{
		"typed list":      []string{invalid},
		"typed map":       map[string]string{"value": invalid},
		"typed struct":    typedReceipt{Value: invalid},
		"embedded struct": typedEmbeddedReceipt{typedEmbeddedFields{Value: invalid}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateAgentContractJSONDomain(value, 0); err == nil {
				t.Fatal("invalid UTF-8 passed the pre-normalization JSON domain")
			}
			if _, err := contextPackRetrievalProofCanonicalJSON(value); err == nil {
				t.Fatal("invalid UTF-8 received a canonical proof encoding")
			}
		})
	}
}

func TestAgentContractJSONDomainRejectsInvalidCommonJSONValues(t *testing.T) {
	invalid := string([]byte{0xff})
	for name, value := range map[string]any{
		"map key":          map[string]any{invalid: true},
		"nested string":    map[string]any{"nested": []any{invalid}},
		"nested nonfinite": map[string]any{"nested": []any{math.NaN()}},
		"unsigned overflow": map[string]any{
			"nested": uint64(1) << 63,
		},
		"number overflow": map[string]any{"nested": json.Number("9223372036854775808")},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateAgentContractJSONDomain(value, 0); err == nil {
				t.Fatal("invalid common JSON value passed the fast domain validator")
			}
		})
	}
}

func TestStabilizeAgentContractActualJSONBytesSolvesDecimalWidthFixedPoint(t *testing.T) {
	for _, paddingBytes := range []int{0, 8, 80, 800, 8000, 80000} {
		t.Run(fmt.Sprintf("padding_%d", paddingBytes), func(t *testing.T) {
			payload := map[string]any{"ok": true, "padding": strings.Repeat("x", paddingBytes)}
			metadata := map[string]any{"schema_id": "test_contract.v1"}
			stabilizeAgentContractActualJSONBytes(payload, "format_contract", metadata)
			payload["format_contract"] = metadata
			reported := anyToInt(metadata["actual_json_bytes"], 0)
			if actual := jsonByteLen(payload); reported != actual {
				t.Fatalf("fixed-point byte accounting mismatch: reported=%d actual=%d", reported, actual)
			}
		})
	}
}

func TestContextPackRetrievalProofReferencesStayCanonicalAndBounded(t *testing.T) {
	assessment := testValidMemoryTrustAssessmentReceipt(3)
	trace := testValidRetrievalDecisionTraceReceipt(3)
	pack := testContextPackFixture(nil)
	compiler := cloneContractMap(anyMap(pack["context_compiler"]))
	payload := map[string]any{
		"context_pack":             pack,
		"context_compiler":         compiler,
		"memory_trust_assessment":  assessment,
		"retrieval_decision_trace": trace,
	}

	ensureContextPackRetrievalProofReferences(payload)
	if _, ok := anyMap(payload["memory_trust_assessment"])["assessments"]; !ok {
		t.Fatal("root trust assessment must retain the canonical receipt")
	}
	if _, ok := anyMap(payload["retrieval_decision_trace"])["decisions"]; !ok {
		t.Fatal("root decision trace must retain the canonical receipt")
	}
	nested := anyMap(payload["context_pack"])
	if _, ok := anyMap(nested["memory_trust_assessment"])["assessments"]; ok {
		t.Fatal("nested trust proof must remain a bounded canonical reference")
	}
	if _, ok := anyMap(nested["retrieval_decision_trace"])["decisions"]; ok {
		t.Fatal("nested decision proof must remain a bounded canonical reference")
	}
	if anyToString(anyMap(nested["memory_trust_assessment"])["canonical_path"]) != "$.memory_trust_assessment" ||
		anyToString(anyMap(nested["retrieval_decision_trace"])["canonical_path"]) != "$.retrieval_decision_trace" {
		t.Fatalf("nested proof references lost canonical paths: %#v", nested)
	}
	compiler = anyMap(payload["context_compiler"])
	if _, ok := anyMap(compiler["memory_trust_assessment"])["assessments"]; ok {
		t.Fatal("compiler trust proof must remain a bounded canonical reference")
	}
	if _, ok := anyMap(compiler["retrieval_decision_trace"])["decisions"]; ok {
		t.Fatal("compiler decision proof must remain a bounded canonical reference")
	}
	if anyToString(anyMap(compiler["memory_trust_assessment"])["canonical_path"]) != "$.memory_trust_assessment" ||
		anyToString(anyMap(compiler["retrieval_decision_trace"])["canonical_path"]) != "$.retrieval_decision_trace" {
		t.Fatalf("compiler proof references lost canonical paths: %#v", compiler)
	}
}

func TestContextPackRetrievalProofReferencesCannotBecomeCanonicalRootReceipts(t *testing.T) {
	pack := testContextPackFixture(nil)
	compiler := cloneContractMap(anyMap(pack["context_compiler"]))
	payload := map[string]any{
		"ok":                 true,
		"context_pack":       pack,
		"context_compiler":   compiler,
		"source_coverage":    map[string]any{"configured": []any{}, "returned": []any{}, "complete": false},
		"reference_prompt":   "No canonical retrieval proof was supplied.",
		"writeback_required": true,
	}

	ensureContextPackRetrievalProofReferences(payload)
	assessment := anyMap(payload["memory_trust_assessment"])
	trace := anyMap(payload["retrieval_decision_trace"])
	if available, exists := assessment["available"]; !exists || anyToBool(available) {
		t.Fatalf("nested trust reference was promoted to canonical root custody: %#v", assessment)
	}
	if available, exists := trace["available"]; !exists || anyToBool(available) {
		t.Fatalf("nested trace reference was promoted to canonical root custody: %#v", trace)
	}
	if anyToString(assessment["canonical_path"]) != "$.memory_trust_assessment" ||
		anyToString(trace["canonical_path"]) != "$.retrieval_decision_trace" {
		t.Fatalf("unavailable roots lost their canonical paths: assessment=%#v trace=%#v", assessment, trace)
	}

	attached := attachContextPackFormatContract(payload)
	if findings := validateAgentContractPayload(contextPackResponseContractID, attached); len(findings) != 0 {
		t.Fatalf("typed unavailable proof roots must preserve a valid context-pack response: %#v", findings)
	}
}

func TestContextPackMalformedReceiptListsCannotClaimCanonicalRootCustody(t *testing.T) {
	for _, owner := range []string{"root", "nested", "compiler"} {
		t.Run(owner, func(t *testing.T) {
			pack := testContextPackFixture(nil)
			compiler := cloneContractMap(anyMap(pack["context_compiler"]))
			assessment := map[string]any{"schema_id": memoryTrustAssessmentContractID, "assessments": []any{}}
			trace := map[string]any{"schema_id": retrievalDecisionTraceContractID, "decisions": []any{}}
			payload := map[string]any{
				"ok": true, "context_pack": pack, "context_compiler": compiler,
				"source_coverage":  map[string]any{"configured": []any{}, "returned": []any{}, "complete": false},
				"reference_prompt": "malformed receipt custody", "writeback_required": true,
			}
			switch owner {
			case "root":
				payload["memory_trust_assessment"] = assessment
				payload["retrieval_decision_trace"] = trace
			case "nested":
				pack["memory_trust_assessment"] = assessment
				pack["retrieval_decision_trace"] = trace
			case "compiler":
				compiler["memory_trust_assessment"] = assessment
				compiler["retrieval_decision_trace"] = trace
			}

			ensureContextPackRetrievalProofReferences(payload)
			rootAssessment := anyMap(payload["memory_trust_assessment"])
			rootTrace := anyMap(payload["retrieval_decision_trace"])
			if available, exists := rootAssessment["available"]; !exists || anyToBool(available) {
				t.Fatalf("malformed trust receipt claimed canonical root custody: %#v", rootAssessment)
			}
			if available, exists := rootTrace["available"]; !exists || anyToBool(available) {
				t.Fatalf("malformed trace receipt claimed canonical root custody: %#v", rootTrace)
			}
		})
	}
}

func TestContextPackMalformedProjectedAndUnavailableProofsAreCanonicalizedFailClosed(t *testing.T) {
	for _, owner := range []string{"root", "nested", "compiler"} {
		for _, shape := range []string{"projected", "unavailable"} {
			t.Run(owner+"/"+shape, func(t *testing.T) {
				pack := testContextPackFixture(nil)
				compiler := cloneContractMap(anyMap(pack["context_compiler"]))
				assessment := map[string]any{
					"schema_id": memoryTrustAssessmentContractID, "canonical_path": "$.memory_trust_assessment",
				}
				trace := map[string]any{
					"schema_id": retrievalDecisionTraceContractID, "canonical_path": "$.retrieval_decision_trace",
				}
				if shape == "projected" {
					assessment["bounded_projection"] = true
					assessment["canonical_digest"] = "sha256:" + strings.Repeat("a", 64)
					trace["bounded_projection"] = true
					trace["canonical_digest"] = "sha256:" + strings.Repeat("b", 64)
				} else {
					assessment["available"] = false
					assessment["reason"] = "caller supplied"
					assessment["note"] = "must not survive"
					trace["available"] = false
					trace["reason"] = "caller supplied"
					trace["note"] = "must not survive"
				}
				payload := map[string]any{"context_pack": pack, "context_compiler": compiler}
				switch owner {
				case "root":
					payload["memory_trust_assessment"] = assessment
					payload["retrieval_decision_trace"] = trace
				case "nested":
					pack["memory_trust_assessment"] = assessment
					pack["retrieval_decision_trace"] = trace
				case "compiler":
					compiler["memory_trust_assessment"] = assessment
					compiler["retrieval_decision_trace"] = trace
				}
				ensureContextPackRetrievalProofReferences(payload)
				for label, proof := range map[string]map[string]any{
					"assessment": anyMap(payload["memory_trust_assessment"]),
					"trace":      anyMap(payload["retrieval_decision_trace"]),
				} {
					if available, ok := proof["available"].(bool); !ok || available || len(proof) != 4 {
						t.Fatalf("%s malformed proof did not fail closed: %#v", label, proof)
					}
					if _, leaked := proof["note"]; leaked {
						t.Fatalf("%s caller proof metadata crossed unavailable boundary: %#v", label, proof)
					}
				}
			})
		}
	}
}

func TestContextPackFullProofCustodyRequiresExactCardinalityAndMetadataTypes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any, map[string]any)
	}{
		{name: "trust cardinality", mutate: func(assessment, _ map[string]any) { assessment["assessed_count"] = 1 }},
		{name: "trace cardinality", mutate: func(_, trace map[string]any) { trace["decision_count"] = 1 }},
		{name: "ok type", mutate: func(assessment, _ map[string]any) { assessment["ok"] = "true" }},
		{name: "bounded type", mutate: func(_, trace map[string]any) { trace["bounded"] = 1 }},
		{name: "contract valid type", mutate: func(assessment, _ map[string]any) {
			anyMap(assessment["format_contract"])["contract_valid"] = "true"
		}},
		{name: "errors type", mutate: func(_, trace map[string]any) {
			anyMap(anyMap(trace["format_contract"])["validation"])["errors"] = "[]"
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assessment := testValidMemoryTrustAssessmentReceipt(2)
			trace := testValidRetrievalDecisionTraceReceipt(2)
			tc.mutate(assessment, trace)
			if canonicalContextPackRetrievalProof(assessment, memoryTrustAssessmentContractID, "assessments", "$.memory_trust_assessment", false) != nil &&
				canonicalContextPackRetrievalProof(trace, retrievalDecisionTraceContractID, "decisions", "$.retrieval_decision_trace", false) != nil {
				t.Fatal("malformed proof metadata or cardinality claimed canonical custody")
			}
		})
	}
}

func TestContextPackImpossibleRetrievalCountRelationshipsFailClosedAtEveryOrigin(t *testing.T) {
	for _, owner := range []string{"root", "nested", "compiler"} {
		t.Run(owner, func(t *testing.T) {
			assessment := testValidMemoryTrustAssessmentReceipt(1)
			trace := testValidRetrievalDecisionTraceReceipt(1)
			assessment["quarantine_count"] = 1
			assessment["deduplicated_count"] = 1
			trace["processed_candidate_count"] = 0
			trace["coverage_complete"] = false

			if len(validateAgentContractPayload(memoryTrustAssessmentContractID, assessment)) == 0 ||
				len(validateAgentContractPayload(retrievalDecisionTraceContractID, trace)) == 0 {
				t.Fatal("impossible proof count relationships passed registered validation")
			}

			pack := testContextPackFixture(nil)
			compiler := cloneContractMap(anyMap(pack["context_compiler"]))
			payload := map[string]any{"context_pack": pack, "context_compiler": compiler}
			switch owner {
			case "root":
				payload["memory_trust_assessment"] = assessment
				payload["retrieval_decision_trace"] = trace
			case "nested":
				pack["memory_trust_assessment"] = assessment
				pack["retrieval_decision_trace"] = trace
			case "compiler":
				compiler["memory_trust_assessment"] = assessment
				compiler["retrieval_decision_trace"] = trace
			}
			ensureContextPackRetrievalProofReferences(payload)
			for label, proof := range map[string]map[string]any{
				"assessment": anyMap(payload["memory_trust_assessment"]),
				"trace":      anyMap(payload["retrieval_decision_trace"]),
			} {
				if proof["available"] != false {
					t.Fatalf("%s impossible count receipt claimed custody: %#v", label, proof)
				}
			}
		})
	}
}

func TestContextPackRetrievalReceiptCardinalityAndCategoryHistogramFailClosedAtEveryOrigin(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(map[string]any, map[string]any)
	}{
		{name: "trust list cardinality", mutate: func(assessment, _ map[string]any) {
			assessment["assessments"] = []any{}
		}},
		{name: "trace list cardinality", mutate: func(_ map[string]any, trace map[string]any) {
			trace["decisions"] = []any{}
		}},
		{name: "trace category histogram", mutate: func(_ map[string]any, trace map[string]any) {
			anyMap(contextPackAnyList(trace["decisions"])[0])["decision"] = "omitted"
		}},
	}
	for _, owner := range []string{"root", "nested", "compiler"} {
		for _, tc := range mutations {
			t.Run(owner+"/"+tc.name, func(t *testing.T) {
				assessment := testValidMemoryTrustAssessmentReceipt(1)
				trace := testValidRetrievalDecisionTraceReceipt(1)
				tc.mutate(assessment, trace)
				assessmentInvalid := len(validateAgentContractPayload(memoryTrustAssessmentContractID, assessment)) > 0
				traceInvalid := len(validateAgentContractPayload(retrievalDecisionTraceContractID, trace)) > 0
				if !assessmentInvalid && !traceInvalid {
					t.Fatal("receipt cardinality or category mismatch passed registered validation")
				}

				pack := testContextPackFixture(nil)
				compiler := cloneContractMap(anyMap(pack["context_compiler"]))
				payload := map[string]any{"context_pack": pack, "context_compiler": compiler}
				switch owner {
				case "root":
					payload["memory_trust_assessment"] = assessment
					payload["retrieval_decision_trace"] = trace
				case "nested":
					pack["memory_trust_assessment"] = assessment
					pack["retrieval_decision_trace"] = trace
				case "compiler":
					compiler["memory_trust_assessment"] = assessment
					compiler["retrieval_decision_trace"] = trace
				}
				ensureContextPackRetrievalProofReferences(payload)
				if assessmentInvalid && anyMap(payload["memory_trust_assessment"])["available"] != false {
					t.Fatalf("invalid assessment claimed custody: %#v", payload["memory_trust_assessment"])
				}
				if traceInvalid && anyMap(payload["retrieval_decision_trace"])["available"] != false {
					t.Fatalf("invalid trace claimed custody: %#v", payload["retrieval_decision_trace"])
				}
			})
		}
	}
}

func TestContextPackRetrievalProofPairMismatchesFailClosedAtEveryOrigin(t *testing.T) {
	for _, owner := range []string{"root", "nested", "compiler"} {
		for _, mismatch := range []string{"candidate counts", "candidate identity", "large candidate identity", "quarantine disposition"} {
			t.Run(owner+"/"+mismatch, func(t *testing.T) {
				count := 1
				if mismatch == "large candidate identity" {
					count = 65
				}
				assessment := testValidMemoryTrustAssessmentReceipt(count)
				traceCount := count
				if mismatch == "candidate counts" {
					traceCount = 2
				}
				trace := testValidRetrievalDecisionTraceReceipt(traceCount)
				switch mismatch {
				case "candidate identity":
					anyMap(contextPackAnyList(trace["decisions"])[0])["candidate_id"] = "rtc_ffffffffffffffffffffffff"
				case "large candidate identity":
					rows := contextPackAnyList(trace["decisions"])
					anyMap(rows[len(rows)-1])["candidate_id"] = "rtc_ffffffffffffffffffffffff"
				case "quarantine disposition":
					anyMap(anyMap(contextPackAnyList(assessment["assessments"])[0])["quarantine"])["quarantined"] = true
					assessment["quarantine_count"] = 1
				}
				assessment = attachPayloadFormatContract(
					memoryTrustAssessmentContractID, assessment, "test", "test", "/test/context-pack",
				)
				trace = attachPayloadFormatContract(
					retrievalDecisionTraceContractID, trace, "test", "test", "/test/context-pack",
				)
				if findings := validateAgentContractPayload(memoryTrustAssessmentContractID, assessment); len(findings) != 0 {
					t.Fatalf("independently valid trust proof failed validation: %#v", findings)
				}
				if findings := validateAgentContractPayload(retrievalDecisionTraceContractID, trace); len(findings) != 0 {
					t.Fatalf("independently valid trace proof failed validation: %#v", findings)
				}

				pack := testContextPackFixture(nil)
				compiler := cloneContractMap(anyMap(pack["context_compiler"]))
				payload := map[string]any{"context_pack": pack, "context_compiler": compiler}
				switch owner {
				case "root":
					payload["memory_trust_assessment"] = assessment
					payload["retrieval_decision_trace"] = trace
				case "nested":
					pack["memory_trust_assessment"] = assessment
					pack["retrieval_decision_trace"] = trace
				case "compiler":
					compiler["memory_trust_assessment"] = assessment
					compiler["retrieval_decision_trace"] = trace
				}
				ensureContextPackRetrievalProofReferences(payload)
				if anyMap(payload["memory_trust_assessment"])["available"] != false ||
					anyMap(payload["retrieval_decision_trace"])["available"] != false {
					t.Fatalf("mismatched proof pair claimed custody: assessment=%#v trace=%#v", payload["memory_trust_assessment"], payload["retrieval_decision_trace"])
				}
			})
		}
	}
}

func TestContextPackRetrievalProofPairNeverCombinesDifferentOrigins(t *testing.T) {
	nestedAssessment := testValidMemoryTrustAssessmentReceipt(1)
	nestedTrace := testValidRetrievalDecisionTraceReceipt(1)
	rootAssessment := memoryTrustAssessmentReference(nestedAssessment)
	rootTrace := retrievalDecisionTraceReference(nestedTrace)
	for name, root := range map[string]map[string]any{
		"root trust only": {"memory_trust_assessment": rootAssessment},
		"root trace only": {"retrieval_decision_trace": rootTrace},
	} {
		t.Run(name, func(t *testing.T) {
			pack := testContextPackFixture(nil)
			pack["memory_trust_assessment"] = cloneAnyMap(nestedAssessment)
			pack["retrieval_decision_trace"] = cloneAnyMap(nestedTrace)
			payload := map[string]any{"context_pack": pack}
			for key, value := range root {
				payload[key] = cloneAnyMap(anyMap(value))
			}
			ensureContextPackRetrievalProofReferences(payload)
			if anyMap(payload["memory_trust_assessment"])["available"] != false ||
				anyMap(payload["retrieval_decision_trace"])["available"] != false {
				t.Fatalf("mixed origins claimed proof custody: assessment=%#v trace=%#v", payload["memory_trust_assessment"], payload["retrieval_decision_trace"])
			}
		})
	}
}

func TestContextPackRetrievalProofProjectionAndRootReferencePairsReconcile(t *testing.T) {
	fullAssessment := testValidMemoryTrustAssessmentReceipt(1)
	fullTrace := testValidRetrievalDecisionTraceReceipt(1)
	projectedAssessment := contextPackRetrievalProofForOuterBoundary(fullAssessment, memoryTrustAssessmentContractID)
	projectedTrace := contextPackRetrievalProofForOuterBoundary(fullTrace, retrievalDecisionTraceContractID)
	projectedTrace["candidate_count"] = 2
	projectedTrace["decision_count"] = 1
	projectedTrace["input_truncated_count"] = 1
	projectedTrace["coverage_complete"] = false
	referenceAssessment := memoryTrustAssessmentReference(fullAssessment)
	referenceTrace := retrievalDecisionTraceReference(fullTrace)
	referenceTrace["candidate_count"] = 3
	referenceTrace["decision_count"] = 2
	referenceTrace["input_truncated_count"] = 1
	referenceTrace["coverage_complete"] = false

	for name, pair := range map[string][2]map[string]any{
		"projection": {projectedAssessment, projectedTrace},
		"reference":  {referenceAssessment, referenceTrace},
		"reference max-int dispositions": {
			map[string]any{
				"schema_id": memoryTrustAssessmentContractID, "canonical_path": "$.memory_trust_assessment",
				"assessed_count": int64(1), "quarantine_count": int64(math.MaxInt64),
				"deduplicated_count": int64(math.MaxInt64), "policy_omitted_count": int64(math.MaxInt64),
				"input_truncated_count": int64(0),
			},
			map[string]any{
				"schema_id": retrievalDecisionTraceContractID, "canonical_path": "$.retrieval_decision_trace",
				"trace_id": "rdt_0123456789abcdef01234567", "candidate_count": int64(1),
				"decision_count": int64(1), "input_truncated_count": int64(0), "coverage_complete": true,
			},
		},
		"available trust with unavailable trace": {
			referenceAssessment,
			contextPackUnavailableRetrievalProof(retrievalDecisionTraceContractID, "trace missing"),
		},
		"unavailable trust with available trace": {
			contextPackUnavailableRetrievalProof(memoryTrustAssessmentContractID, "assessment missing"),
			referenceTrace,
		},
	} {
		t.Run(name, func(t *testing.T) {
			pack := testContextPackFixture(nil)
			payload := map[string]any{
				"context_pack":             pack,
				"memory_trust_assessment":  cloneAnyMap(pair[0]),
				"retrieval_decision_trace": cloneAnyMap(pair[1]),
			}
			ensureContextPackRetrievalProofReferences(payload)
			if anyMap(payload["memory_trust_assessment"])["available"] != false ||
				anyMap(payload["retrieval_decision_trace"])["available"] != false {
				t.Fatalf("mismatched %s pair claimed custody: assessment=%#v trace=%#v", name, payload["memory_trust_assessment"], payload["retrieval_decision_trace"])
			}
		})
	}
}

func TestContextPackRetrievalPolicyAndInputBoundarySemanticsFailClosed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any, map[string]any)
	}{
		{name: "retrieved memory remains evidence", mutate: func(assessment, _ map[string]any) {
			anyMap(assessment["policy"])["retrieved_memory_is_evidence_not_instruction"] = false
		}},
		{name: "security defenses remain fail closed", mutate: func(assessment, _ map[string]any) {
			anyMap(assessment["policy"])["security_defenses_fail_closed"] = false
		}},
		{name: "trust omitted count reconciles", mutate: func(assessment, _ map[string]any) {
			anyMap(assessment["input_boundary"])["omitted_count"] = 1
		}},
		{name: "trace truncated flag reconciles", mutate: func(_ map[string]any, trace map[string]any) {
			anyMap(trace["input_boundary"])["truncated"] = true
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assessment := testValidMemoryTrustAssessmentReceipt(1)
			trace := testValidRetrievalDecisionTraceReceipt(1)
			tc.mutate(assessment, trace)
			assessmentValid := len(validateAgentContractPayload(memoryTrustAssessmentContractID, assessment)) == 0
			traceValid := len(validateAgentContractPayload(retrievalDecisionTraceContractID, trace)) == 0
			if assessmentValid && traceValid {
				t.Fatal("invalid policy or input-boundary semantics passed registered validation")
			}
			assessmentCanonical := canonicalContextPackRetrievalProof(
				assessment, memoryTrustAssessmentContractID, "assessments", "$.memory_trust_assessment", false,
			)
			traceCanonical := canonicalContextPackRetrievalProof(
				trace, retrievalDecisionTraceContractID, "decisions", "$.retrieval_decision_trace", false,
			)
			if assessmentValid && assessmentCanonical == nil || traceValid && traceCanonical == nil {
				t.Fatal("unchanged valid companion proof unexpectedly failed custody")
			}
			if !assessmentValid && assessmentCanonical != nil || !traceValid && traceCanonical != nil {
				t.Fatal("invalid policy or input-boundary semantics claimed canonical custody")
			}
		})
	}
}

func TestContextPackDuplicateRetrievalReceiptIdentitiesFailClosed(t *testing.T) {
	assessment := testValidMemoryTrustAssessmentReceipt(2)
	trace := testValidRetrievalDecisionTraceReceipt(2)
	assessmentRows := contextPackAnyList(assessment["assessments"])
	traceRows := contextPackAnyList(trace["decisions"])
	anyMap(assessmentRows[1])["assessment_id"] = anyMap(assessmentRows[0])["assessment_id"]
	anyMap(assessmentRows[1])["candidate_id"] = anyMap(assessmentRows[0])["candidate_id"]
	anyMap(traceRows[1])["receipt_id"] = anyMap(traceRows[0])["receipt_id"]
	anyMap(traceRows[1])["candidate_id"] = anyMap(traceRows[0])["candidate_id"]
	anyMap(traceRows[1])["candidate_ordinal"] = anyMap(traceRows[0])["candidate_ordinal"]
	if len(validateAgentContractPayload(memoryTrustAssessmentContractID, assessment)) == 0 ||
		len(validateAgentContractPayload(retrievalDecisionTraceContractID, trace)) == 0 {
		t.Fatal("duplicate logical retrieval receipt identities passed registered validation")
	}
	if canonicalContextPackRetrievalProof(
		assessment, memoryTrustAssessmentContractID, "assessments", "$.memory_trust_assessment", false,
	) != nil || canonicalContextPackRetrievalProof(
		trace, retrievalDecisionTraceContractID, "decisions", "$.retrieval_decision_trace", false,
	) != nil {
		t.Fatal("duplicate logical retrieval receipt identities claimed canonical custody")
	}
}

func TestContextPackFullRetrievalProofRequiresExactFormatContractProvenance(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "registry id", mutate: func(contract map[string]any) { contract["registry_id"] = "other_registry" }},
		{name: "registry version", mutate: func(contract map[string]any) {
			contract["registry_version"] = GeneratedAgentContractRegistryVersion - 1
		}},
		{name: "contract version", mutate: func(contract map[string]any) { contract["contract_version"] = 2 }},
		{name: "output mode", mutate: func(contract map[string]any) { contract["required_output_mode"] = "text" }},
		{name: "validator", mutate: func(contract map[string]any) { contract["validator"] = "other.validator" }},
		{name: "maximum total bytes", mutate: func(contract map[string]any) { contract["max_total_json_bytes"] = 1 }},
		{name: "maximum string bytes", mutate: func(contract map[string]any) { contract["max_string_bytes"] = 1 }},
		{name: "maximum list items", mutate: func(contract map[string]any) { contract["max_list_items"] = 1 }},
		{name: "actual bytes", mutate: func(contract map[string]any) { contract["actual_json_bytes"] = 1 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assessment := cloneContractMap(testValidMemoryTrustAssessmentReceipt(1))
			trace := cloneContractMap(testValidRetrievalDecisionTraceReceipt(1))
			tc.mutate(anyMap(assessment["format_contract"]))
			tc.mutate(anyMap(trace["format_contract"]))
			if canonicalContextPackRetrievalProof(
				assessment, memoryTrustAssessmentContractID, "assessments", "$.memory_trust_assessment", false,
			) != nil || canonicalContextPackRetrievalProof(
				trace, retrievalDecisionTraceContractID, "decisions", "$.retrieval_decision_trace", false,
			) != nil {
				t.Fatal("false format-contract provenance claimed canonical proof custody")
			}
		})
	}
}

func TestContextPackProjectedTraceOmissionRequiresTypedEmptyIdentity(t *testing.T) {
	projection := map[string]any{
		"schema_id": retrievalDecisionTraceContractID, "canonical_path": "$.retrieval_decision_trace",
		"available": true, "bounded_projection": true, "canonical_digest": "sha256:" + strings.Repeat("a", 64),
		"candidate_count": 1, "decision_count": 1, "input_truncated_count": 0,
		"trace_id": nil, "trace_id_omitted": true, "coverage_complete": true,
	}
	if canonicalContextPackRetrievalProof(
		projection, retrievalDecisionTraceContractID, "decisions", "$.retrieval_decision_trace", true,
	) != nil {
		t.Fatal("null projected trace identity passed the typed omission contract")
	}
	projection["trace_id"] = ""
	if canonicalContextPackRetrievalProof(
		projection, retrievalDecisionTraceContractID, "decisions", "$.retrieval_decision_trace", true,
	) == nil {
		t.Fatal("typed empty projected trace identity did not pass its omission contract")
	}
}

func TestContextPackFractionalReceiptIntegersCannotClaimCanonicalRootCustody(t *testing.T) {
	for _, owner := range []string{"root", "nested", "compiler"} {
		t.Run(owner, func(t *testing.T) {
			pack := testContextPackFixture(nil)
			compiler := cloneContractMap(anyMap(pack["context_compiler"]))
			assessment := testValidMemoryTrustAssessmentReceipt(1)
			trace := testValidRetrievalDecisionTraceReceipt(1)
			assessment["version"] = 1.5
			trace["candidate_count"] = 1.5
			if len(validateAgentContractPayload(memoryTrustAssessmentContractID, assessment)) == 0 ||
				len(validateAgentContractPayload(retrievalDecisionTraceContractID, trace)) == 0 {
				t.Fatal("fractional integer fields unexpectedly passed the Go contract validator")
			}
			payload := map[string]any{
				"ok": true, "context_pack": pack, "context_compiler": compiler,
				"source_coverage":  map[string]any{"configured": []any{}, "returned": []any{}, "complete": false},
				"reference_prompt": "fractional receipt custody", "writeback_required": true,
			}
			switch owner {
			case "root":
				payload["memory_trust_assessment"] = assessment
				payload["retrieval_decision_trace"] = trace
			case "nested":
				pack["memory_trust_assessment"] = assessment
				pack["retrieval_decision_trace"] = trace
			case "compiler":
				compiler["memory_trust_assessment"] = assessment
				compiler["retrieval_decision_trace"] = trace
			}

			ensureContextPackRetrievalProofReferences(payload)
			rootAssessment := anyMap(payload["memory_trust_assessment"])
			rootTrace := anyMap(payload["retrieval_decision_trace"])
			if available, exists := rootAssessment["available"]; !exists || anyToBool(available) {
				t.Fatalf("fractional trust receipt claimed canonical root custody: %#v", rootAssessment)
			}
			if available, exists := rootTrace["available"]; !exists || anyToBool(available) {
				t.Fatalf("fractional trace receipt claimed canonical root custody: %#v", rootTrace)
			}
		})
	}
}

func TestContextPackIntegralFloatReceiptIntegersFailRegisteredValidationAndCustody(t *testing.T) {
	assessment := testValidMemoryTrustAssessmentReceipt(1)
	trace := testValidRetrievalDecisionTraceReceipt(1)
	assessment["version"] = 1.0
	trace["candidate_count"] = 1.0
	if len(validateAgentContractPayload(memoryTrustAssessmentContractID, assessment)) == 0 ||
		len(validateAgentContractPayload(retrievalDecisionTraceContractID, trace)) == 0 {
		t.Fatal("integral float receipt integers passed strict registered proof validation")
	}
	if canonicalContextPackRetrievalProof(assessment, memoryTrustAssessmentContractID, "assessments", "$.memory_trust_assessment", true) != nil ||
		canonicalContextPackRetrievalProof(trace, retrievalDecisionTraceContractID, "decisions", "$.retrieval_decision_trace", true) != nil {
		t.Fatal("integral float receipt claimed canonical root custody")
	}
	for _, value := range []any{json.Number("9223372036854775808"), json.Number("-9223372036854775809")} {
		if _, ok := agentContractInteger(value); ok {
			t.Fatalf("out-of-range plain JSON integer passed generic contract validation: %v", value)
		}
	}
}

func TestAgentContractNormalizationPreservesJSONNumbersForNumberFields(t *testing.T) {
	normalized, err := normalizeAgentContractJSONObject(map[string]any{
		"score":  0.75,
		"labels": []string{"bounded"},
	})
	if err != nil {
		t.Fatalf("normalize mixed typed payload: %v", err)
	}
	score, ok := normalized["score"].(json.Number)
	if !ok {
		t.Fatalf("normalization did not preserve numeric lexical form: %T", normalized["score"])
	}
	if !matchesAgentContractType(score, "number") {
		t.Fatalf("normalized finite number failed generic number validation: %v", score)
	}
	if matchesAgentContractType(json.Number("1e9999"), "number") ||
		matchesAgentContractType(json.Number("9223372036854775808"), "number") ||
		matchesAgentContractType(uint64(1)<<63, "number") ||
		matchesAgentContractType(math.Inf(1), "number") ||
		matchesAgentContractType(math.NaN(), "number") {
		t.Fatal("nonfinite or overflowing value passed generic number validation")
	}
}

func TestContextPackOutOfRangeReceiptIntegersCannotClaimCanonicalRootCustody(t *testing.T) {
	for _, owner := range []string{"root", "nested", "compiler"} {
		t.Run(owner, func(t *testing.T) {
			pack := testContextPackFixture(nil)
			compiler := cloneContractMap(anyMap(pack["context_compiler"]))
			assessment := testValidMemoryTrustAssessmentReceipt(1)
			trace := testValidRetrievalDecisionTraceReceipt(1)
			assessment["assessed_count"] = uint64(1) << 63
			trace["decision_count"] = uint64(1) << 63
			if len(validateAgentContractPayload(memoryTrustAssessmentContractID, assessment)) == 0 ||
				len(validateAgentContractPayload(retrievalDecisionTraceContractID, trace)) == 0 {
				t.Fatal("out-of-range integer fields unexpectedly passed the Go contract validator")
			}
			payload := map[string]any{
				"ok": true, "context_pack": pack, "context_compiler": compiler,
				"source_coverage":  map[string]any{"configured": []any{}, "returned": []any{}, "complete": false},
				"reference_prompt": "out-of-range receipt custody", "writeback_required": true,
			}
			switch owner {
			case "root":
				payload["memory_trust_assessment"] = assessment
				payload["retrieval_decision_trace"] = trace
			case "nested":
				pack["memory_trust_assessment"] = assessment
				pack["retrieval_decision_trace"] = trace
			case "compiler":
				compiler["memory_trust_assessment"] = assessment
				compiler["retrieval_decision_trace"] = trace
			}

			ensureContextPackRetrievalProofReferences(payload)
			rootAssessment := anyMap(payload["memory_trust_assessment"])
			rootTrace := anyMap(payload["retrieval_decision_trace"])
			if available, exists := rootAssessment["available"]; !exists || anyToBool(available) {
				t.Fatalf("out-of-range trust receipt claimed canonical root custody: %#v", rootAssessment)
			}
			if available, exists := rootTrace["available"]; !exists || anyToBool(available) {
				t.Fatalf("out-of-range trace receipt claimed canonical root custody: %#v", rootTrace)
			}
		})
	}
}

func TestPolicyContextPackageContractValidationPassesAndFails(t *testing.T) {
	pack := map[string]any{
		"context_pack": testContextPackFixture([]any{map[string]any{"text": "f1", "source": "test"}}),
	}
	policy := buildPolicyContextPackage(
		"codex",
		"codex_gpt5_test",
		"contextlattice",
		"runbooks/codex-integration",
		"contract test",
		"fast",
		pack,
		pack,
		nil,
		nil,
		objectiveContext{},
	)
	format, ok := policy["format_contract"].(map[string]any)
	if !ok {
		t.Fatalf("missing format_contract: %#v", policy["format_contract"])
	}
	if strings.TrimSpace(anyToString(format["schema_id"])) != policyContextPackageContractID {
		t.Fatalf("unexpected schema_id: %#v", format["schema_id"])
	}
	validation, _ := format["validation"].(map[string]any)
	if strings.TrimSpace(anyToString(validation["status"])) != "passed" {
		t.Fatalf("expected policy validation passed, got %#v", validation)
	}
	assertBoundaryMetadata(t, policy, "format_contract", false)
	if findings := validateAgentContractPayload(policyContextPackageContractID, policy); len(findings) != 0 {
		t.Fatalf("policy package should validate: %#v", findings)
	}
	if findings := validateAgentContractPayload(objectiveRuntimeStateContractID, anyMap(policy["objective_runtime"])); len(findings) != 0 {
		t.Fatalf("objective runtime should validate: %#v", findings)
	}
	if hierarchy := anyMap(policy["objective_hierarchy"]); anyToString(hierarchy["schema_id"]) != "contextlattice_objective_hierarchy.v1" {
		t.Fatalf("expected policy objective hierarchy, got %#v", policy["objective_hierarchy"])
	}
	if lineage := anyMap(policy["objective_lineage"]); anyToString(lineage["schema_id"]) != "contextlattice_objective_lineage.v1" {
		t.Fatalf("expected policy objective lineage, got %#v", policy["objective_lineage"])
	}

	badPolicy := cloneContractMap(policy)
	delete(badPolicy, "format_contract")
	badPolicy["hookSpecificOutput"] = map[string]any{"unsafe": true}
	findings := validateAgentContractPayload(policyContextPackageContractID, badPolicy)
	if len(findings) == 0 {
		t.Fatalf("expected malformed policy package to fail validation")
	}
	joined := ""
	for _, finding := range findings {
		joined += anyToString(finding["reason"]) + " " + anyToString(finding["path"]) + " " + anyToString(finding["field"]) + "\n"
	}
	if !strings.Contains(joined, "missing_required_field") || !strings.Contains(joined, "forbidden_field_present") {
		t.Fatalf("expected missing required and forbidden findings, got %#v", findings)
	}
}

func TestAgentContractForbiddenFieldsRejectCanonicalAliases(t *testing.T) {
	timeline := make([]any, 512)
	for index := range timeline {
		timeline[index] = map[string]any{"safe": index}
	}
	timeline[len(timeline)-1] = map[string]any{"privateKey": "unsafe-late-list-value"}
	payload := map[string]any{
		"schema_id": agentProofTimelineContractID,
		"timeline":  timeline,
		"evidence": map[string]any{
			"requestBody": "unsafe", "toolCalls": []any{"unsafe"}, "privateKey": "unsafe",
		},
	}
	findings := validateAgentContractPayload(agentProofTimelineContractID, payload)
	joined := ""
	for _, finding := range findings {
		if anyToString(finding["reason"]) == "forbidden_field_present" {
			joined += anyToString(finding["path"]) + "\n"
		}
	}
	for _, path := range []string{"evidence.requestBody", "evidence.toolCalls", "evidence.privateKey", "timeline.privateKey"} {
		if !strings.Contains(joined, path) {
			t.Fatalf("canonical forbidden alias %q was not rejected: %#v", path, findings)
		}
	}
}

func TestObjectiveRuntimeStateContractValidationPassesAndFails(t *testing.T) {
	runtime := buildObjectiveRuntimeState(
		"generic-agent",
		"generic_agent_test",
		"contextlattice",
		"runbooks/generic-agent",
		"runtime contract test",
		"balanced",
		"sess-runtime-test",
		objectiveContext{Mission: "ship durable coordination", Objective: "prove objective runtime state", Goal: "keep output bounded"},
		"agent.preflight.completed",
	)
	format, ok := runtime["format_contract"].(map[string]any)
	if !ok {
		t.Fatalf("missing objective runtime format_contract: %#v", runtime)
	}
	if strings.TrimSpace(anyToString(format["schema_id"])) != objectiveRuntimeStateContractID {
		t.Fatalf("unexpected objective runtime schema_id: %#v", format["schema_id"])
	}
	validation, _ := format["validation"].(map[string]any)
	if strings.TrimSpace(anyToString(validation["status"])) != "passed" {
		t.Fatalf("expected objective runtime validation passed, got %#v", validation)
	}
	assertBoundaryMetadata(t, runtime, "format_contract", false)
	if findings := validateAgentContractPayload(objectiveRuntimeStateContractID, runtime); len(findings) != 0 {
		t.Fatalf("objective runtime should validate: %#v", findings)
	}
	if hierarchy := anyMap(runtime["objective_hierarchy"]); anyToString(anyMap(hierarchy["project"])["primary_objective"]) == "" {
		t.Fatalf("expected runtime project primary objective, got %#v", runtime["objective_hierarchy"])
	}
	if lineage := anyMap(runtime["objective_lineage"]); anyToString(anyMap(lineage["drift"])["status"]) == "" {
		t.Fatalf("expected runtime objective lineage drift status, got %#v", runtime["objective_lineage"])
	}
	bad := cloneContractMap(runtime)
	delete(bad, "next_action")
	bad["raw_prompt"] = "unsafe"
	findings := validateAgentContractPayload(objectiveRuntimeStateContractID, bad)
	if len(findings) == 0 {
		t.Fatalf("expected malformed objective runtime state to fail")
	}
	joined := ""
	for _, finding := range findings {
		joined += anyToString(finding["reason"]) + " " + anyToString(finding["path"]) + " " + anyToString(finding["field"]) + "\n"
	}
	if !strings.Contains(joined, "missing_required_field") || !strings.Contains(joined, "forbidden_field_present") {
		t.Fatalf("expected missing required and forbidden findings, got %#v", findings)
	}
}

func TestAgentPreflightFormatContractValidationPassesAndFails(t *testing.T) {
	contextPack := testContextPackFixture([]any{})
	pack := map[string]any{
		"ok":                 true,
		"context_pack":       contextPack,
		"context_compiler":   contextPack["context_compiler"],
		"reference_prompt":   "Use this ContextLattice compiled context package as the factual packet for the next reasoning step.",
		"source_coverage":    map[string]any{"configured": []any{"fixture"}, "returned": []any{"fixture"}, "complete": true},
		"writeback_required": true,
	}
	objectiveRuntime := buildObjectiveRuntimeState("codex", "codex_gpt5_test", "contextlattice", "runbooks/codex-integration", "preflight", "fast", "sess-test", objectiveContext{}, "agent.preflight.completed")
	policy := buildPolicyContextPackage("codex", "codex_gpt5_test", "contextlattice", "runbooks/codex-integration", "preflight", "fast", pack, pack, nil, objectiveRuntime, objectiveContext{})
	response := attachAgentPreflightFormatContracts(map[string]any{
		"ok":                     true,
		"service":                "gateway-go",
		"agent":                  "codex",
		"agent_id":               "codex_gpt5_test",
		"project":                "contextlattice",
		"query":                  "preflight",
		"topic_path":             "runbooks/codex-integration",
		"retrieval_mode":         "fast",
		"context_pack":           pack,
		"objective_runtime":      objectiveRuntime,
		"policy_context_package": policy,
	})
	contracts, ok := response["format_contracts"].(map[string]any)
	if !ok {
		t.Fatalf("missing format_contracts: %#v", response["format_contracts"])
	}
	validation, _ := contracts["validation"].(map[string]any)
	if strings.TrimSpace(anyToString(validation["status"])) != "passed" {
		t.Fatalf("expected preflight validation passed, got %#v", validation)
	}
	listed := contextPackAnyList(contracts["contracts"])
	if len(listed) != 23 {
		t.Fatalf("expected 23 preflight-relevant contracts, got %d: %#v", len(listed), listed)
	}
	if !containsString(anyToStringList(listed, 32), recallResponseContractID) {
		t.Fatalf("preflight contract summary omitted recall response contract: %#v", listed)
	}
	registry, err := loadAgentContractsRegistry()
	if err != nil {
		t.Fatalf("load agent contracts registry: %v", err)
	}
	if len(listed) >= len(registry.Contracts) {
		t.Fatalf("preflight metadata must not echo the full contract registry: listed=%d", len(listed))
	}
	assertBoundaryMetadata(t, response, "format_contracts", false)

	bad := cloneContractMap(response)
	delete(bad, "policy_context_package")
	findings := validateAgentContractPayload(agentPreflightResponseContractID, bad)
	if len(findings) == 0 {
		t.Fatalf("expected malformed preflight response to fail validation")
	}
}

func TestAgentPreflightProfileMatrixCoversLocalAgents(t *testing.T) {
	cases := []struct {
		agent   string
		key     string
		agentID string
		topic   string
	}{
		{"codex", "codex", "codex_gpt5", "runbooks/codex-integration"},
		{"claude-code", "claude-code", "claude_code_agent", "runbooks/claude-code-integration"},
		{"opencode", "opencode", "opencode_agent", "runbooks/opencode-integration"},
		{"hermes", "hermes-agent", "hermes_agent", "runbooks/hermes-agent-integration"},
		{"hermes-ultra", "hermes-ultra", "hermes_ultra_agent", "runbooks/hermes-ultra-integration"},
		{"hermes-agent-ultra", "hermes-ultra", "hermes_ultra_agent", "runbooks/hermes-ultra-integration"},
		{"omp", "omp", "omp_agent", "runbooks/omp-integration"},
		{"oh-my-pi", "omp", "omp_agent", "runbooks/omp-integration"},
		{"mercury", "mercury-agent", "mercury_agent", "runbooks/mercury-agent-integration"},
		{"mercury-agent", "mercury-agent", "mercury_agent", "runbooks/mercury-agent-integration"},
		{"pi", "pi", "pi_agent", "runbooks/pi-integration"},
		{"pi-agent", "pi", "pi_agent", "runbooks/pi-integration"},
		{"droid", "droid", "droid_agent", "runbooks/droid-integration"},
		{"droid-agent", "droid", "droid_agent", "runbooks/droid-integration"},
		{"chatgpt-web", "chatgpt-web", "chatgpt_web_agent", "runbooks/chatgpt-web-integration"},
		{"chatgpt-desktop", "chatgpt-desktop", "chatgpt_desktop_agent", "runbooks/chatgpt-desktop-integration"},
		{"claude-web", "claude-web", "claude_web_agent", "runbooks/claude-web-integration"},
		{"claude-desktop", "claude-desktop", "claude_desktop_agent", "runbooks/claude-desktop-integration"},
	}
	for _, tc := range cases {
		t.Run(tc.agent, func(t *testing.T) {
			key, profile := resolveAgentPreflightProfile(tc.agent)
			if key != tc.key {
				t.Fatalf("expected key=%q, got %q", tc.key, key)
			}
			if profile.AgentID != tc.agentID {
				t.Fatalf("expected agent_id=%q, got %q", tc.agentID, profile.AgentID)
			}
			if profile.TopicPath != tc.topic {
				t.Fatalf("expected topic=%q, got %q", tc.topic, profile.TopicPath)
			}
			if strings.TrimSpace(profile.Query) == "" {
				t.Fatalf("expected non-empty query")
			}
			if profile.RetrievalMode != "balanced" {
				t.Fatalf("expected balanced retrieval mode, got %q", profile.RetrievalMode)
			}
		})
	}
}

func TestContextPackAndWritebackFormatContractsValidate(t *testing.T) {
	pack := testContextPackFixture([]any{})
	coverage := map[string]any{"configured": []any{"postgres_pgvector"}, "returned": []any{"postgres_pgvector"}, "complete": true}
	contextResponse := attachContextPackFormatContract(map[string]any{
		"ok":                 true,
		"agent_id":           "codex_gpt5_test",
		"context_pack":       pack,
		"context_compiler":   pack["context_compiler"],
		"reference_prompt":   "Use this ContextLattice compiled context package as the factual packet for the next reasoning step.",
		"source_coverage":    coverage,
		"writeback_required": true,
		"memory_trust_assessment": map[string]any{
			"schema_id": memoryTrustAssessmentContractID,
		},
		"retrieval_decision_trace": map[string]any{
			"schema_id": retrievalDecisionTraceContractID,
		},
	})
	contextFormat, _ := contextResponse["format_contract"].(map[string]any)
	contextValidation, _ := contextFormat["validation"].(map[string]any)
	if strings.TrimSpace(anyToString(contextValidation["status"])) != "passed" {
		t.Fatalf("expected context-pack validation passed, got %#v", contextValidation)
	}
	assertBoundaryMetadata(t, contextResponse, "format_contract", false)

	dream := attachPayloadFormatContract(dreamModeResponseContractID, map[string]any{
		"ok":                  true,
		"mode":                "dream",
		"dream_available":     true,
		"intelligence_source": "llm_synthesis",
		"agent_id":            "codex_gpt5_test",
		"project":             "contextlattice",
		"goal":                "invent a better memory primitive",
		"query":               "invent a better memory primitive",
		"topic_path":          "contextlattice/dream-mode",
		"retrieval_mode":      "balanced",
		"novelty_level":       3,
		"risk_tolerance":      "balanced",
		"hypotheses":          []any{map[string]any{"id": "h1", "title": "t", "claim": "c", "supporting_evidence": []any{"e1"}}},
		"experiments":         []any{map[string]any{"id": "x1", "hypothesis_id": "h1", "method": "test"}},
		"evidence": map[string]any{
			"facts":     []any{map[string]any{"id": "e1", "text": "fact"}},
			"results":   []any{},
			"citations": []any{},
			"counts":    map[string]any{"facts": 1},
		},
		"source_coverage":    coverage,
		"llm":                map[string]any{"enabled": true, "used": true, "provider": "ollama", "model": "qwen3.5:9b"},
		"writeback_required": true,
	}, "codex_gpt5_test", "dream", "/memory/dream")
	dreamFormat, _ := dream["format_contract"].(map[string]any)
	dreamValidation, _ := dreamFormat["validation"].(map[string]any)
	if strings.TrimSpace(anyToString(dreamValidation["status"])) != "passed" {
		t.Fatalf("expected dream validation passed, got %#v", dreamValidation)
	}
	assertBoundaryMetadata(t, dream, "format_contract", false)

	writeback := attachWritebackFormatContract(
		map[string]any{"ok": true, "event_id": "evt_test"},
		normalizedWrite{project: "contextlattice", fileName: "notes/test.md", topicPath: "runbooks/codex-integration", agentID: "codex_gpt5_test"},
		"/memory/write",
		200,
	)
	writebackFormat, _ := writeback["format_contract"].(map[string]any)
	writebackValidation, _ := writebackFormat["validation"].(map[string]any)
	if strings.TrimSpace(anyToString(writebackValidation["status"])) != "passed" {
		t.Fatalf("expected writeback validation passed, got %#v", writebackValidation)
	}
}

func TestAgentBoundaryContractClipsOversizedContextPackPayload(t *testing.T) {
	oversized := strings.Repeat("array_above_max_length context length exceeded ", 400)
	items := make([]any, 0, 140)
	for idx := 0; idx < 140; idx++ {
		items = append(items, map[string]any{
			"text":       oversized,
			"summary":    oversized,
			"file":       "notes/oversized.md",
			"source":     "fixture",
			"topic_path": "runbooks/boundary",
		})
	}
	pack := testContextPackFixture(items)
	pack["files_to_read"] = items
	pack["files_to_avoid"] = items
	pack["capabilities_to_use"] = items
	pack["runbooks"] = items
	pack["known_failure_modes"] = items
	pack["commands"] = items
	pack["acceptance_criteria"] = items
	payload := attachContextPackFormatContract(map[string]any{
		"ok":                 true,
		"agent_id":           "codex_gpt5_test",
		"context_pack":       pack,
		"context_compiler":   pack["context_compiler"],
		"reference_prompt":   oversized,
		"source_coverage":    map[string]any{"configured": items, "returned": items, "complete": true},
		"retrieval":          map[string]any{"debug": oversized},
		"writeback_required": true,
	})
	assertBoundaryContractPassed(t, contextPackResponseContractID, payload)
	assertBoundaryJSONUnderLimit(t, contextPackResponseContractID, payload)
	assertNoRawProviderOverflowShape(t, payload)
	assertBoundaryMetadata(t, payload, "format_contract", true)
	clippedPack, _ := payload["context_pack"].(map[string]any)
	if results, _ := clippedPack["results"].([]any); len(results) > agentBoundaryLimitsForContract(contextPackResponseContractID).MaxListItems {
		t.Fatalf("expected context_pack.results clipped, got %d", len(results))
	}
}

func TestMinimalContextPackBoundaryPreservesContextAndSynthesisContracts(t *testing.T) {
	pack := minimalContextPackBoundary(map[string]any{"query": "sparse synthesis boundary"})
	coverage := map[string]any{"configured": []any{"qdrant"}, "returned": []any{}, "complete": false}
	runAdvisor := buildRunAdvisor(runAdvisorInput{
		Query:          "sparse synthesis boundary",
		Project:        "contextlattice",
		RetrievalMode:  "fast",
		SourceCoverage: coverage,
		RankedEvidence: []any{},
		Surface:        "/memory/context-pack",
	})
	contextResponse := attachContextPackFormatContract(map[string]any{
		"ok":                 true,
		"context_pack":       pack,
		"source_coverage":    coverage,
		"context_compiler":   pack["context_compiler"],
		"reference_prompt":   "No ranked evidence was available; broaden retrieval.",
		"run_advisor":        runAdvisor,
		"writeback_required": true,
	})
	assertBoundaryContractPassed(t, contextPackResponseContractID, contextResponse)

	s := newTestServer(t, "http://127.0.0.1:1")
	synthesis := s.buildSynthesisPack(context.Background(), contextResponse, map[string]any{
		"project":        "contextlattice",
		"query":          "sparse synthesis boundary",
		"retrieval_mode": "fast",
	}, "/memory/synthesis-pack")
	synthesisResponse := attachPayloadFormatContract(synthesisPackContractID, map[string]any{
		"ok":                   true,
		"schema_id":            synthesisPackContractID,
		"version":              1,
		"project":              "contextlattice",
		"query":                "sparse synthesis boundary",
		"topic_path":           "",
		"retrieval_mode":       "fast",
		"retrieval_intent":     "synthesis",
		"synthesis_pack":       synthesis,
		"context_pack":         pack,
		"source_coverage":      coverage,
		"reference_prompt":     synthesisPackReferencePrompt(synthesis),
		"token_impact":         map[string]any{},
		"context_pack_quality": map[string]any{},
		"run_advisor":          runAdvisor,
		"writeback_required":   true,
	}, "codex_gpt5_test", "synthesis_pack", "/memory/synthesis-pack")
	assertBoundaryContractPassed(t, synthesisPackContractID, synthesisResponse)
}

func TestForceMinimalContextPackBoundaryPreservesEvidenceKernel(t *testing.T) {
	items := make([]any, 0, 7)
	for idx := 0; idx < 7; idx++ {
		items = append(items, map[string]any{
			"rank":             idx + 1,
			"kind":             "decision",
			"text":             "Preserve evidence before lower-value prompt structure.",
			"project":          "contextlattice",
			"file":             fmt.Sprintf("notes/evidence-%d.md", idx),
			"source":           "qdrant",
			"topic_path":       "architecture/synthesis",
			"score":            0.99 - float64(idx)*0.01,
			"confidence":       0.9,
			"estimated_tokens": 50,
			"value_density":    0.8,
			"why_selected":     []any{"decision", "query_match"},
		})
	}
	pack := testContextPackFixture(items)
	pack["query"] = "preserve sparse evidence"
	pack["omitted_high_value_refs"] = items[4:]
	compiler := anyMap(pack["context_compiler"])
	compiler["token_budget"] = map[string]any{
		"active":                        true,
		"selected_count":                len(items),
		"used_tokens_estimate":          350,
		"ranked_evidence_budget_tokens": 500,
		"compression_level":             "compact",
	}
	payload := map[string]any{
		"ok":                 true,
		"context_pack":       pack,
		"context_compiler":   compiler,
		"reference_prompt":   strings.Repeat("lower-value prompt structure ", 500),
		"source_coverage":    map[string]any{"configured": []any{"qdrant"}, "returned": []any{"qdrant"}, "complete": true},
		"writeback_required": true,
	}
	stats := agentBoundaryStats{}
	forceMinimalContextPackResponseBoundary(payload, &stats)

	minimal := anyMap(payload["context_pack"])
	ranked := contextPackAnyList(minimal["ranked_evidence"])
	if len(ranked) != 4 {
		t.Fatalf("expected four-item evidence kernel, got %#v", ranked)
	}
	if anyToString(anyMap(ranked[0])["file"]) != "notes/evidence-0.md" {
		t.Fatalf("expected highest-ranked evidence first, got %#v", ranked[0])
	}
	minimalCompiler := anyMap(payload["context_compiler"])
	if anyToInt(minimalCompiler["ranked_evidence_count"], 0) != 4 || anyToInt(minimalCompiler["pre_boundary_ranked_evidence_count"], 0) != 7 {
		t.Fatalf("expected reconciled compiler counts, got %#v", minimalCompiler)
	}
	minimalBudget := anyMap(minimalCompiler["token_budget"])
	if anyToInt(minimalBudget["selected_count"], 0) != 4 || anyToInt(minimalBudget["pre_boundary_selected_count"], 0) != 7 {
		t.Fatalf("expected reconciled token-budget counts, got %#v", minimalBudget)
	}
	if promptEvidence := contextPackAnyList(anyMap(minimal["prompt_sections"])["evidence"]); len(promptEvidence) != 4 {
		t.Fatalf("expected prompt sections to preserve evidence kernel, got %#v", promptEvidence)
	}
	if prompt := anyToString(payload["reference_prompt"]); !strings.Contains(prompt, "notes/evidence-0.md") {
		t.Fatalf("expected rebuilt reference prompt to cite preserved evidence, got %q", prompt)
	}

	contextResponse := attachContextPackFormatContract(payload)
	assertBoundaryContractPassed(t, contextPackResponseContractID, contextResponse)
	assertBoundaryJSONUnderLimit(t, contextPackResponseContractID, contextResponse)
	s := newTestServer(t, "http://127.0.0.1:1")
	synthesis := s.buildSynthesisPack(context.Background(), contextResponse, map[string]any{
		"project": "contextlattice",
		"query":   "preserve sparse evidence",
	}, "/memory/synthesis-pack")
	if findings := contextPackAnyList(synthesis["high_signal_findings"]); len(findings) == 0 {
		t.Fatalf("expected grounded synthesis findings from evidence kernel, got %#v", synthesis)
	}
	if evidenceTrail := contextPackAnyList(synthesis["evidence_trail"]); len(evidenceTrail) == 0 {
		t.Fatalf("expected grounded synthesis evidence trail, got %#v", synthesis)
	}
}

func TestContextPackBoundaryReconcilesIntermediateEvidenceTrim(t *testing.T) {
	items := make([]any, 0, 11)
	for idx := 0; idx < 11; idx++ {
		items = append(items, map[string]any{
			"rank":             idx + 1,
			"kind":             "check",
			"text":             fmt.Sprintf("evidence-%d", idx),
			"file":             fmt.Sprintf("notes/check-%d.md", idx),
			"estimated_tokens": 25,
		})
	}
	pack := testContextPackFixture(items)
	compiler := anyMap(pack["context_compiler"])
	compiler["token_budget"] = map[string]any{
		"active":               true,
		"selected_count":       11,
		"used_tokens_estimate": 275,
	}
	pack["ranked_evidence"] = items[:8]
	pack["rankedEvidence"] = items[:8]
	payload := map[string]any{
		"context_pack":     pack,
		"context_compiler": compiler,
		"reference_prompt": "stale pre-boundary prompt",
		"warnings":         []any{},
	}
	stats := agentBoundaryStats{}
	reconcileContextPackBoundaryMetadata(payload, true, &stats)

	reconciledCompiler := anyMap(payload["context_compiler"])
	if anyToInt(reconciledCompiler["ranked_evidence_count"], 0) != 8 || anyToInt(reconciledCompiler["pre_boundary_ranked_evidence_count"], 0) != 11 {
		t.Fatalf("expected truthful ranked evidence counts, got %#v", reconciledCompiler)
	}
	reconciledBudget := anyMap(reconciledCompiler["token_budget"])
	if anyToInt(reconciledBudget["selected_count"], 0) != 8 || anyToInt(reconciledBudget["pre_boundary_selected_count"], 0) != 11 || anyToInt(reconciledBudget["used_tokens_estimate"], 0) != 200 {
		t.Fatalf("expected truthful token budget counts, got %#v", reconciledBudget)
	}
	reconciledPack := anyMap(payload["context_pack"])
	if promptEvidence := contextPackAnyList(anyMap(reconciledPack["prompt_sections"])["evidence"]); len(promptEvidence) != 8 {
		t.Fatalf("expected prompt evidence to match returned evidence, got %d", len(promptEvidence))
	}
	if prompt := anyToString(payload["reference_prompt"]); strings.Contains(prompt, "notes/check-8.md") || !strings.Contains(prompt, "notes/check-0.md") {
		t.Fatalf("expected rebuilt prompt to cite only returned evidence, got %q", prompt)
	}
	if warnings := contextPackAnyList(payload["warnings"]); len(warnings) != 1 {
		t.Fatalf("expected one boundary warning, got %#v", warnings)
	}
}

func TestAgentBoundaryContractClipsOversizedDreamPayload(t *testing.T) {
	oversized := strings.Repeat("array_above_max_length context length exceeded nonlinear hypothesis ", 500)
	items := make([]any, 0, 140)
	for idx := 0; idx < 140; idx++ {
		items = append(items, map[string]any{
			"id":                  "h",
			"title":               oversized,
			"claim":               oversized,
			"why_novel":           oversized,
			"supporting_evidence": []any{"e1", "e2"},
			"experiment":          oversized,
			"expected_signal":     oversized,
		})
	}
	payload := attachPayloadFormatContract(dreamModeResponseContractID, map[string]any{
		"ok":                  true,
		"mode":                "dream",
		"dream_available":     true,
		"intelligence_source": "llm_synthesis",
		"agent_id":            "codex_gpt5_test",
		"project":             "contextlattice",
		"goal":                oversized,
		"query":               oversized,
		"topic_path":          "contextlattice/dream-mode",
		"retrieval_mode":      "balanced",
		"novelty_level":       5,
		"risk_tolerance":      "experimental",
		"hypotheses":          items,
		"experiments":         items,
		"evidence": map[string]any{
			"facts":     items,
			"results":   items,
			"citations": items,
			"combined":  items,
			"counts":    map[string]any{"facts": 140, "results": 140, "citations": 140},
		},
		"source_coverage":    map[string]any{"configured": items, "returned": items, "complete": true},
		"llm":                map[string]any{"enabled": true, "used": true, "provider": "ollama", "model": "qwen3.5:9b", "synthesis_text": oversized, "parsed": map[string]any{"hypotheses": items}},
		"retrieval":          map[string]any{"debug": oversized},
		"writeback":          map[string]any{"debug": oversized},
		"writeback_required": true,
	}, "codex_gpt5_test", "dream", "/memory/dream")
	assertBoundaryContractPassed(t, dreamModeResponseContractID, payload)
	assertBoundaryJSONUnderLimit(t, dreamModeResponseContractID, payload)
	assertNoRawProviderOverflowShape(t, payload)
	assertBoundaryMetadata(t, payload, "format_contract", true)
	if hypotheses, _ := payload["hypotheses"].([]any); len(hypotheses) > agentBoundaryLimitsForContract(dreamModeResponseContractID).MaxListItems {
		t.Fatalf("expected dream hypotheses clipped, got %d", len(hypotheses))
	}
}

func TestAgentBoundaryContractClipsOversizedPreflightPayload(t *testing.T) {
	oversized := strings.Repeat("context length exceeded array_above_max_length ", 600)
	item := map[string]any{"text": oversized, "summary": oversized, "source": "fixture", "file": "oversized.md"}
	items := []any{}
	for idx := 0; idx < 120; idx++ {
		items = append(items, cloneContractMap(item))
	}
	contextPack := testContextPackFixture(items)
	contextPack["files_to_read"] = items
	contextPack["capabilities_to_use"] = items
	contextPack["runbooks"] = items
	contextPack["known_failure_modes"] = items
	contextPack["commands"] = items
	contextPack["acceptance_criteria"] = items
	pack := attachContextPackFormatContract(map[string]any{
		"ok":                 true,
		"agent_id":           "codex_gpt5_test",
		"context_pack":       contextPack,
		"context_compiler":   contextPack["context_compiler"],
		"reference_prompt":   oversized,
		"source_coverage":    map[string]any{"configured": []any{"fixture"}, "returned": []any{"fixture"}, "complete": true},
		"writeback_required": true,
	})
	objectiveRuntime := buildObjectiveRuntimeState("codex", "codex_gpt5_test", "contextlattice", "runbooks/codex-integration", oversized, "balanced", "sess-test", objectiveContext{}, "agent.preflight.completed")
	policy := buildPolicyContextPackage("codex", "codex_gpt5_test", "contextlattice", "runbooks/codex-integration", oversized, "balanced", pack, pack, nil, objectiveRuntime, objectiveContext{})
	statusPayload := map[string]any{
		"raw":   oversized,
		"items": items,
		"frontierT1Governance": map[string]any{
			"schema_id": "frontier_t1_governance_state.v1", "configured": true,
			"enabled": true, "force_disabled": false, "ready": true, "store_ready": true,
			"authorization_ready": true, "authorization_mode": "explicit_private_development",
			"entitlement_mode": "enforce", "release_gate": "pass", "release_gate_satisfied": true,
			"required_route_count": 3, "protected_route_count": 3, "eligible_route_count": 3,
			"event_count": 31, "last_sequence": 31, "bytes": 26726,
		},
	}
	for idx := 0; idx < 300; idx++ {
		statusPayload[fmt.Sprintf("debug_%03d", idx)] = strings.Repeat("bounded preflight status pressure ", 300)
	}
	response := attachAgentPreflightFormatContracts(map[string]any{
		"ok":                     true,
		"service":                "gateway-go",
		"agent":                  "codex",
		"agent_id":               "codex_gpt5_test",
		"project":                "contextlattice",
		"query":                  oversized,
		"topic_path":             "runbooks/codex-integration",
		"retrieval_mode":         "balanced",
		"status":                 statusPayload,
		"scoped_search":          map[string]any{"results": items, "degraded": false},
		"broadened_search":       map[string]any{"results": items, "degraded": false},
		"context_pack":           pack,
		"mission_context_pack":   pack,
		"objective_runtime":      objectiveRuntime,
		"policy_context_package": policy,
	})
	assertBoundaryContractPassed(t, agentPreflightResponseContractID, response)
	assertBoundaryJSONUnderLimit(t, agentPreflightResponseContractID, response)
	assertBoundaryJSONHeadroom(t, agentPreflightResponseContractID, response, 1024)
	assertNoRawProviderOverflowShape(t, response)
	assertBoundaryMetadata(t, response, "format_contracts", true)
	status := anyMap(response["status"])
	if !anyToBool(status["omitted_by_boundary"]) {
		t.Fatalf("expected oversized preflight status to be compacted, got %#v", status)
	}
	frontier := anyMap(status["frontierT1Governance"])
	if !anyToBool(frontier["ready"]) || anyToString(frontier["authorization_mode"]) != "explicit_private_development" ||
		anyToString(frontier["release_gate"]) != "pass" || anyToInt(frontier["protected_route_count"], 0) != 3 {
		t.Fatalf("expected compacted preflight to retain bounded Frontier truth, got %#v", frontier)
	}
}

func TestAgentPreflightBoundarySanitizesProviderOverflowSearchTextUnderBudget(t *testing.T) {
	phrase := "documentation mentions array_above_max_length and context length exceeded as historical provider errors"
	contextPack := testContextPackFixture([]any{})
	pack := attachContextPackFormatContract(map[string]any{
		"ok":                 true,
		"agent_id":           "codex_gpt5_test",
		"context_pack":       contextPack,
		"context_compiler":   contextPack["context_compiler"],
		"reference_prompt":   "Use this ContextLattice compiled context package as the factual packet for the next reasoning step.",
		"source_coverage":    map[string]any{"configured": []any{"fixture"}, "returned": []any{"fixture"}, "complete": true},
		"writeback_required": true,
	})
	objectiveRuntime := buildObjectiveRuntimeState("codex", "codex_gpt5_test", "contextlattice", "runbooks/codex-integration", "preflight", "balanced", "sess-test", objectiveContext{}, "agent.preflight.completed")
	policy := buildPolicyContextPackage("codex", "codex_gpt5_test", "contextlattice", "runbooks/codex-integration", "preflight", "balanced", pack, pack, nil, objectiveRuntime, objectiveContext{})
	response := attachAgentPreflightFormatContracts(map[string]any{
		"ok":             true,
		"service":        "gateway-go",
		"agent":          "codex",
		"agent_id":       "codex_gpt5_test",
		"project":        "contextlattice",
		"query":          "preflight",
		"topic_path":     "runbooks/codex-integration",
		"retrieval_mode": "balanced",
		"broadened_search": map[string]any{
			"results": map[string]any{"results": []map[string]any{{"summary": phrase}}},
		},
		"context_pack":           pack,
		"objective_runtime":      objectiveRuntime,
		"policy_context_package": policy,
	})
	assertBoundaryContractPassed(t, agentPreflightResponseContractID, response)
	assertNoRawProviderOverflowShape(t, response)
	assertBoundaryMetadata(t, response, "format_contracts", false)
}

func TestCompactPreflightStatusBoundaryRejectsCompositeValues(t *testing.T) {
	hugeMap := map[string]any{}
	for idx := 0; idx < 1000; idx++ {
		hugeMap[fmt.Sprintf("key_%04d", idx)] = strings.Repeat("unbounded composite value ", 100)
	}
	status := compactPreflightStatusBoundary(map[string]any{
		"frontierT1Governance": map[string]any{
			"schema_id":             strings.Repeat("schema", 100),
			"ready":                 hugeMap,
			"bytes":                 hugeMap,
			"authorization_mode":    []any{strings.Repeat("mode", 1000)},
			"protected_route_count": 3,
		},
	})
	frontier := anyMap(status["frontierT1Governance"])
	if _, ok := frontier["ready"]; ok {
		t.Fatalf("composite boolean escaped preflight status boundary: %#v", frontier["ready"])
	}
	if _, ok := frontier["bytes"]; ok {
		t.Fatalf("composite integer escaped preflight status boundary: %#v", frontier["bytes"])
	}
	if _, ok := frontier["authorization_mode"]; ok {
		t.Fatalf("composite string escaped preflight status boundary: %#v", frontier["authorization_mode"])
	}
	if got := anyToInt(frontier["protected_route_count"], 0); got != 3 {
		t.Fatalf("valid scalar route count was not preserved: %#v", frontier)
	}
	if got := len(anyToString(frontier["schema_id"])); got > 128 {
		t.Fatalf("schema_id escaped string boundary: %d bytes", got)
	}
	if size := jsonByteLen(status); size > 1024 {
		t.Fatalf("compacted preflight status exceeded fixed capsule budget: %d bytes", size)
	}
}

func TestContextBoundaryPayloadCoversAgentSurfaces(t *testing.T) {
	payload := contextBoundaryPayload()
	if !anyToBool(payload["ok"]) || anyToInt(payload["violationCount"], -1) != 0 {
		t.Fatalf("expected clean context boundary payload, got %#v", payload)
	}
	routes, _ := payload["routes"].([]map[string]any)
	if len(routes) == 0 {
		rawRoutes, ok := payload["routes"].([]any)
		if !ok || len(rawRoutes) == 0 {
			t.Fatalf("expected context boundary routes, got %#v", payload["routes"])
		}
		for _, raw := range rawRoutes {
			routes = append(routes, anyMap(raw))
		}
	}
	byPath := map[string][]map[string]any{}
	for _, route := range routes {
		path := anyToString(route["path"])
		byPath[path] = append(byPath[path], route)
		if anyToBool(route["required"]) && !anyToBool(route["bounded"]) {
			t.Fatalf("expected required boundary route bounded, got %#v", route)
		}
	}
	for _, path := range []string{
		"/memory/context-pack",
		"/tools/context_pack",
		"/memory/continuity/reconcile",
		"/memory/objectives/transition",
		"/memory/objectives/graph",
		"/memory/decision-changes",
		"/v1/agents/preflight",
		"/v1/codex/preflight",
		"policy_context_package",
		"scripts/agent/contextlattice-pack",
		"scripts/agent/contextlattice-recall-response",
		"scripts/agent/contextlattice-pack --response",
		"contextlattice_pack --response",
		"contextlattice_recall_response",
		"contextlattice recall-response",
		"contextlattice-agent-tools recall-response",
		"contextlattice_continuity_reconcile",
		"contextlattice_objective_transition",
		"contextlattice_objective_graph",
		"contextlattice_decision_change",
		"contextlattice_decision_change list",
		"scripts/agent/compaction-handoff-payload",
		"scripts/agent_hooks/contextlattice_pre_compaction_write.sh",
		"scripts/agent_hooks/contextlattice_post_compaction_read.sh",
	} {
		rows := byPath[path]
		if len(rows) == 0 {
			t.Fatalf("expected context boundary path %s in payload %#v", path, payload["routes"])
		}
		for _, route := range rows {
			if anyToInt(route["max_total_json_bytes"], 0) <= 0 || anyToInt(route["max_string_bytes"], 0) <= 0 || anyToInt(route["max_list_items"], 0) <= 0 {
				t.Fatalf("expected boundary limits for %s, got %#v", path, route)
			}
		}
	}
	for _, required := range []struct {
		path       string
		contractID string
	}{
		{"/memory/decision-changes", decisionChangeContractID},
		{"/memory/decision-changes", decisionChangeQueryContractID},
		{"scripts/agent/contextlattice-recall-response", recallResponseContractID},
		{"scripts/agent/contextlattice-pack --response", recallResponseContractID},
		{"contextlattice_pack --response", recallResponseContractID},
		{"contextlattice_decision_change", decisionChangeContractID},
		{"contextlattice_decision_change list", decisionChangeQueryContractID},
		{"contextlattice_recall_response", recallResponseContractID},
		{"contextlattice recall-response", recallResponseContractID},
		{"contextlattice-agent-tools recall-response", recallResponseContractID},
	} {
		found := false
		for _, route := range byPath[required.path] {
			if anyToString(route["contract_id"]) == required.contractID {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected %s to expose contract %s: %#v", required.path, required.contractID, byPath[required.path])
		}
	}
}

func assertBoundaryMetadata(t *testing.T, payload map[string]any, key string, wantTruncated bool) {
	t.Helper()
	metadata, ok := payload[key].(map[string]any)
	if !ok {
		t.Fatalf("expected %s metadata object, got %#v", key, payload[key])
	}
	if anyToBool(metadata["contract_valid"]) != true {
		t.Fatalf("expected %s contract_valid=true, got %#v", key, metadata)
	}
	if anyToBool(metadata["truncated"]) != wantTruncated {
		t.Fatalf("expected %s truncated=%v, got %#v", key, wantTruncated, metadata)
	}
	if anyToInt(metadata["max_total_json_bytes"], 0) <= 0 {
		t.Fatalf("expected %s max_total_json_bytes, got %#v", key, metadata)
	}
	if anyToInt(metadata["max_string_bytes"], 0) <= 0 {
		t.Fatalf("expected %s max_string_bytes, got %#v", key, metadata)
	}
	if anyToInt(metadata["max_list_items"], 0) <= 0 {
		t.Fatalf("expected %s max_list_items, got %#v", key, metadata)
	}
	if anyToInt(metadata["actual_json_bytes"], 0) <= 0 {
		t.Fatalf("expected %s actual_json_bytes, got %#v", key, metadata)
	}
	counts, ok := metadata["omitted_counts"].(map[string]any)
	if !ok {
		t.Fatalf("expected %s omitted_counts object, got %#v", key, metadata["omitted_counts"])
	}
	if wantTruncated {
		total := anyToInt(counts["strings_clipped"], 0) + anyToInt(counts["lists_clipped"], 0) + anyToInt(counts["optional_fields_compacted"], 0) + anyToInt(counts["boundary_passes"], 0) + anyToInt(counts["json_bytes_reduced"], 0)
		if total <= 0 {
			t.Fatalf("expected %s omitted_counts to record clipping, got %#v", key, counts)
		}
	}
}

func assertBoundaryContractPassed(t *testing.T, contractID string, payload map[string]any) {
	t.Helper()
	findings := validateAgentContractPayload(contractID, payload)
	if len(findings) != 0 {
		t.Fatalf("expected %s validation passed, got %#v", contractID, findings)
	}
}

func TestAgentPreflightContractKeepsUnavailableContextPackTyped(t *testing.T) {
	contextPack := testContextPackFixture([]any{})
	pack := map[string]any{"ok": true, "context_pack": contextPack, "context_compiler": contextPack["context_compiler"]}
	objectiveRuntime := buildObjectiveRuntimeState("codex", "codex_test", "contextlattice", "tests", "preflight", "balanced", "sess-test", objectiveContext{}, "agent.preflight.completed")
	policy := buildPolicyContextPackage("codex", "codex_test", "contextlattice", "tests", "preflight", "balanced", pack, pack, nil, objectiveRuntime, objectiveContext{})
	payload := map[string]any{
		"ok": true, "service": "gateway-go", "agent": "codex", "agent_id": "codex_test",
		"project": "contextlattice", "query": "preflight", "topic_path": "tests",
		"retrieval_mode": "balanced", "context_pack": nil,
		"objective_runtime": objectiveRuntime, "policy_context_package": policy,
	}
	attachAgentPreflightFormatContracts(payload)
	assertBoundaryContractPassed(t, agentPreflightResponseContractID, payload)
	returnedPack := anyMap(payload["context_pack"])
	if len(returnedPack) == 0 || !anyToBool(returnedPack["degraded"]) || anyToString(returnedPack["result_state"]) != "unavailable" {
		t.Fatalf("unavailable context pack lost typed degraded envelope: %#v", payload["context_pack"])
	}
}

func assertBoundaryJSONUnderLimit(t *testing.T, contractID string, payload map[string]any) {
	t.Helper()
	limits := agentBoundaryLimitsForContract(contractID)
	if limits.MaxTotalJSONBytes <= 0 {
		t.Fatalf("expected %s to define max_total_json_bytes", contractID)
	}
	if size := jsonByteLen(payload); size > limits.MaxTotalJSONBytes {
		t.Fatalf("expected %s JSON bytes <= %d, got %d", contractID, limits.MaxTotalJSONBytes, size)
	}
}

func assertBoundaryJSONHeadroom(t *testing.T, contractID string, payload map[string]any, minHeadroom int) {
	t.Helper()
	limits := agentBoundaryLimitsForContract(contractID)
	if limits.MaxTotalJSONBytes <= 0 {
		t.Fatalf("expected %s to define max_total_json_bytes", contractID)
	}
	headroom := limits.MaxTotalJSONBytes - jsonByteLen(payload)
	if headroom < minHeadroom {
		t.Fatalf("expected %s JSON headroom >= %d bytes, got %d", contractID, minHeadroom, headroom)
	}
}

func assertBoundaryMetadataActualUnderLimit(t *testing.T, contractID string, payload map[string]any, metadataKey string) {
	t.Helper()
	limits := agentBoundaryLimitsForContract(contractID)
	if limits.MaxTotalJSONBytes <= 0 {
		t.Fatalf("expected %s to define max_total_json_bytes", contractID)
	}
	metadata, ok := payload[metadataKey].(map[string]any)
	if !ok {
		t.Fatalf("expected %s metadata object, got %#v", metadataKey, payload[metadataKey])
	}
	actual := anyToInt(metadata["actual_json_bytes"], 0)
	if actual <= 0 {
		t.Fatalf("expected %s actual_json_bytes > 0, got %#v", metadataKey, metadata["actual_json_bytes"])
	}
	if actual > limits.MaxTotalJSONBytes {
		t.Fatalf("expected %s actual_json_bytes <= %d, got %d", contractID, limits.MaxTotalJSONBytes, actual)
	}
}

func assertNoRawProviderOverflowShape(t *testing.T, payload any) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	lower := strings.ToLower(string(encoded))
	for _, forbidden := range []string{"array_above_max_length", "context length exceeded", "maximum context length"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("payload still contains raw provider overflow phrase %q", forbidden)
		}
	}
}

func TestAgentContractTelemetryRecordsValidationReasons(t *testing.T) {
	before := anyToInt(agentContractTelemetrySnapshot()["total"], 0)
	recordAgentContractBoundary("codex_gpt5_test", writebackResultContractID, "writeback", "/memory/write", []map[string]any{{"reason": "missing_required_field"}})
	snapshot := agentContractTelemetrySnapshot()
	after := anyToInt(snapshot["total"], 0)
	if after <= before {
		t.Fatalf("expected telemetry total to increase, before=%d after=%d snapshot=%#v", before, after, snapshot)
	}
}
