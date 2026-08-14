package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestContextPackResponseRetrievalProofsKeepOrdinaryReferencesAndProjectDebugReceipts(t *testing.T) {
	assessment := attachPayloadFormatContract(
		memoryTrustAssessmentContractID, testValidMemoryTrustAssessmentReceipt(2), "test", "test", "/memory/context-pack",
	)
	trace := attachPayloadFormatContract(
		retrievalDecisionTraceContractID, testValidRetrievalDecisionTraceReceipt(2), "test", "test", "/memory/context-pack",
	)

	boundedAssessment, boundedTrace := contextPackResponseRetrievalProofs(assessment, trace, false)
	if _, exposed := boundedAssessment["assessments"]; exposed {
		t.Fatalf("ordinary response exposed full trust receipt: %#v", boundedAssessment)
	}
	if _, exposed := boundedTrace["decisions"]; exposed {
		t.Fatalf("ordinary response exposed full decision trace: %#v", boundedTrace)
	}
	if anyToInt(boundedAssessment["assessed_count"], 0) != 2 {
		t.Fatalf("bounded trust proof lost summary counts: %#v", boundedAssessment)
	}
	if anyToString(boundedTrace["trace_id"]) == "" || !anyToBool(boundedTrace["coverage_complete"]) {
		t.Fatalf("bounded decision proof lost trace identity or coverage: %#v", boundedTrace)
	}

	debugAssessment, debugTrace := contextPackResponseRetrievalProofs(assessment, trace, true)
	if _, exposed := debugAssessment["assessments"]; exposed || !anyToBool(debugAssessment["bounded_projection"]) {
		t.Fatalf("explicit debug response exposed unbounded trust receipt: %#v", debugAssessment)
	}
	if _, exposed := debugTrace["decisions"]; exposed || !anyToBool(debugTrace["bounded_projection"]) {
		t.Fatalf("explicit debug response exposed unbounded decision trace: %#v", debugTrace)
	}
	for label, proof := range map[string]map[string]any{"assessment": debugAssessment, "trace": debugTrace} {
		if !strings.HasPrefix(anyToString(proof["canonical_digest"]), "sha256:") {
			t.Fatalf("debug %s projection lost receipt digest: %#v", label, proof)
		}
	}
}

func TestContextPackResponseRetrievalProofsFailClosedBeforeOrdinarySummaryProjection(t *testing.T) {
	validEmptyAssessment := attachPayloadFormatContract(
		memoryTrustAssessmentContractID, testValidMemoryTrustAssessmentReceipt(0), "test", "test", "/memory/context-pack",
	)
	validEmptyTrace := attachPayloadFormatContract(
		retrievalDecisionTraceContractID, testValidRetrievalDecisionTraceReceipt(0), "test", "test", "/memory/context-pack",
	)
	assessment, trace := contextPackResponseRetrievalProofs(validEmptyAssessment, validEmptyTrace, false)
	if _, unavailable := assessment["available"]; unavailable || anyToInt(assessment["assessed_count"], -1) != 0 {
		t.Fatalf("valid zero-candidate trust receipt did not retain authoritative summary: %#v", assessment)
	}
	if _, unavailable := trace["available"]; unavailable || anyToInt(trace["decision_count"], -1) != 0 {
		t.Fatalf("valid zero-candidate decision receipt did not retain authoritative summary: %#v", trace)
	}

	for _, debug := range []bool{false, true} {
		for label, pair := range map[string][2]map[string]any{
			"missing trust":   {{}, validEmptyTrace},
			"malformed trust": {{"schema_id": memoryTrustAssessmentContractID, "assessments": []any{}}, validEmptyTrace},
			"missing trace":   {validEmptyAssessment, {}},
			"malformed trace": {validEmptyAssessment, {"schema_id": retrievalDecisionTraceContractID, "decisions": []any{}}},
		} {
			assessment, trace := contextPackResponseRetrievalProofs(pair[0], pair[1], debug)
			if assessment["available"] != false || trace["available"] != false {
				t.Fatalf("%s debug=%v retained an asymmetric proof claim: assessment=%#v trace=%#v", label, debug, assessment, trace)
			}
		}
	}
}

func TestContextPackTransportHashesFullProofBeforeOuterListBoundary(t *testing.T) {
	digests := make([]string, 0, 2)
	for _, tail := range []string{"tail-alpha", "tail-beta"} {
		assessment := testValidMemoryTrustAssessmentReceipt(65)
		rows := contextPackAnyList(assessment["assessments"])
		anyMap(rows[64])["tail_marker"] = tail
		assessment = attachPayloadFormatContract(
			memoryTrustAssessmentContractID, assessment, "test", "test", "/memory/context-pack",
		)
		trace := testValidRetrievalDecisionTraceReceipt(65)
		canonical, err := contextPackRetrievalProofCanonicalJSON(assessment)
		if err != nil {
			t.Fatalf("canonical full proof: %v", err)
		}
		expectedDigest := "sha256:" + sha256Hex(canonical)

		rootAssessment, rootTrace := contextPackResponseRetrievalProofs(assessment, trace, true)
		pack := testContextPackFixture(nil)
		payload := map[string]any{
			"ok": true, "context_pack": pack,
			"context_compiler":        cloneContractMap(anyMap(pack["context_compiler"])),
			"source_coverage":         map[string]any{"configured": []any{}, "returned": []any{}, "complete": true},
			"reference_prompt":        "pre-boundary retrieval proof custody",
			"writeback_required":      true,
			"memory_trust_assessment": rootAssessment, "retrieval_decision_trace": rootTrace,
		}
		finalized := finalizeFullTransport(
			payload, attachContextPackFormatContract, "test_context_pack_transport", "serialized_test_context_pack_json",
		)
		proof := anyMap(finalized["memory_trust_assessment"])
		if !anyToBool(proof["bounded_projection"]) || anyToString(proof["canonical_digest"]) != expectedDigest {
			t.Fatalf("pre-boundary proof digest mismatch: proof=%#v expected=%s", proof, expectedDigest)
		}
		if _, exposed := proof["assessments"]; exposed {
			t.Fatalf("full proof tail crossed outer transport boundary: %#v", proof)
		}
		if encoded := recallResponseCanonicalJSON(finalized); strings.Contains(encoded, tail) {
			t.Fatalf("full proof tail marker crossed transport: %s", encoded)
		}
		if !anyToBool(anyMap(finalized["format_contract"])["contract_valid"]) {
			t.Fatalf("projected transport lost context-pack contract validity: %#v", finalized["format_contract"])
		}
		digests = append(digests, expectedDigest)
	}
	if digests[0] == digests[1] {
		t.Fatalf("distinct full proof tails collapsed to one digest: %v", digests)
	}
}

func TestContextPackTransportRejectsMismatchedFullProofBeforeOuterProjection(t *testing.T) {
	assessment := testValidMemoryTrustAssessmentReceipt(65)
	trace := testValidRetrievalDecisionTraceReceipt(65)
	rows := contextPackAnyList(trace["decisions"])
	anyMap(rows[len(rows)-1])["candidate_id"] = "rtc_ffffffffffffffffffffffff"
	trace = attachPayloadFormatContract(
		retrievalDecisionTraceContractID, trace, "test", "test", "/memory/context-pack",
	)
	if findings := validateAgentContractPayload(memoryTrustAssessmentContractID, assessment); len(findings) != 0 {
		t.Fatalf("independently valid trust proof failed validation: %#v", findings)
	}
	if findings := validateAgentContractPayload(retrievalDecisionTraceContractID, trace); len(findings) != 0 {
		t.Fatalf("independently valid trace proof failed validation: %#v", findings)
	}
	rootAssessment, rootTrace := contextPackResponseRetrievalProofs(assessment, trace, true)
	if rootAssessment["available"] != false || rootTrace["available"] != false {
		t.Fatalf("mismatched full proof pair crossed the outer projection: assessment=%#v trace=%#v", rootAssessment, rootTrace)
	}
}

func TestContextPackProductionReceiptsRetainProofCustodyAfterNormalization(t *testing.T) {
	items := make([]contextPackEvidenceItem, 65)
	for index := range items {
		items[index] = contextPackEvidenceItem{
			Occurrence: index + 1, Kind: "memory", Text: "production receipt item " + anyToString(index),
			Score: 1, ImpactScore: 1, QueryRelevance: 1, Confidence: 0.9, EstimatedTokens: 1,
			Project: "contextlattice", Source: "fixture", MemoryID: "memory-" + anyToString(index),
		}
	}
	trust := applyMemoryTrustPolicy(items)
	trust.TrustEnvelope = attachPayloadFormatContract(
		memoryTrustAssessmentContractID, trust.TrustEnvelope, "", "memory_trust_assessment", "/memory/context-pack",
	)
	trace := attachPayloadFormatContract(
		retrievalDecisionTraceContractID,
		buildRetrievalDecisionTrace(trust, trust.Eligible, nil, contextPackTokenBudget{}),
		"", "retrieval_decision_trace", "/memory/context-pack",
	)
	if _, ok := trust.TrustEnvelope["assessed_count"].(json.Number); !ok {
		t.Fatalf("production trust receipt normalization did not preserve its integer lexeme: %T", trust.TrustEnvelope["assessed_count"])
	}
	if _, ok := trace["decision_count"].(json.Number); !ok {
		t.Fatalf("production trace normalization did not preserve its integer lexeme: %T", trace["decision_count"])
	}

	assessmentProjection, traceProjection := contextPackResponseRetrievalProofs(trust.TrustEnvelope, trace, true)
	for label, proof := range map[string]map[string]any{"assessment": assessmentProjection, "trace": traceProjection} {
		if !anyToBool(proof["bounded_projection"]) || !strings.HasPrefix(anyToString(proof["canonical_digest"]), "sha256:") {
			t.Fatalf("production %s receipt lost pre-boundary proof custody: %#v", label, proof)
		}
		if available, ok := proof["available"].(bool); !ok || !available {
			t.Fatalf("production %s receipt was marked unavailable: %#v", label, proof)
		}
	}
}

func TestContextPackTransportProjectionPreservesCompiledPacket(t *testing.T) {
	items := []any{
		map[string]any{"text": "one"},
		map[string]any{"text": "two"},
		map[string]any{"text": "three"},
		map[string]any{"text": "four"},
		map[string]any{"text": "five"},
	}
	ranked := append([]any{}, items...)
	pack := map[string]any{
		"facts":                   append([]any{}, items...),
		"numeric_facts":           append([]any{}, items...),
		"numericFacts":            append([]any{}, items...),
		"citations":               append(append([]any{}, items...), items...),
		"results":                 append([]any{}, items...),
		"evidence_points":         append([]any{}, items...),
		"relevant_decisions":      append([]any{}, items...),
		"relevantDecisions":       append([]any{}, items...),
		"known_failure_modes":     append([]any{}, items...),
		"knownFailureModes":       append([]any{}, items...),
		"commands":                append([]any{}, items...),
		"acceptance_criteria":     append([]any{}, items...),
		"acceptanceCriteria":      append([]any{}, items...),
		"runbooks":                append([]any{}, items...),
		"capabilities_to_use":     append([]any{}, items...),
		"capabilitiesToUse":       append([]any{}, items...),
		"graph_neighbors":         append([]any{}, items...),
		"graphNeighbors":          append([]any{}, items...),
		"ranked_evidence":         ranked,
		"rankedEvidence":          ranked,
		"token_budget":            map[string]any{"active": true},
		"tokenBudget":             map[string]any{"active": true},
		"context_compiler":        map[string]any{"complete": true},
		"contextCompiler":         map[string]any{"complete": true},
		"prompt_sections":         map[string]any{"evidence": ranked},
		"promptSections":          map[string]any{"evidence": ranked},
		"token_impact":            map[string]any{"transport_inclusive": true},
		"tokenImpact":             map[string]any{"transport_inclusive": true},
		"context_pack_quality":    map[string]any{"quality_score": 95},
		"contextPackQuality":      map[string]any{"quality_score": 95},
		"filesToRead":             []any{"notes/current.md"},
		"files_to_read":           []any{"notes/current.md"},
		"memory_trust_assessment": map[string]any{"schema_id": "memory_trust_assessment.v1"},
		"retrieval_decision_trace": map[string]any{
			"schema_id": "retrieval_decision_trace.v1",
		},
	}

	projected := projectContextPackForTransport(pack)
	if got := len(contextPackAnyList(projected["ranked_evidence"])); got != len(ranked) {
		t.Fatalf("ranked packet changed during transport projection: got=%d want=%d", got, len(ranked))
	}
	for _, key := range []string{"facts", "numeric_facts", "results", "relevant_decisions", "known_failure_modes", "commands", "acceptance_criteria", "runbooks", "capabilities_to_use", "graph_neighbors"} {
		if got := len(contextPackAnyList(projected[key])); got != contextPackTransportLegacyEvidenceLimit {
			t.Fatalf("legacy preview %s retained %d items, want %d", key, got, contextPackTransportLegacyEvidenceLimit)
		}
	}
	for canonical, legacy := range map[string]string{
		"numeric_facts":       "numericFacts",
		"relevant_decisions":  "relevantDecisions",
		"known_failure_modes": "knownFailureModes",
		"acceptance_criteria": "acceptanceCriteria",
		"capabilities_to_use": "capabilitiesToUse",
		"graph_neighbors":     "graphNeighbors",
	} {
		if got, want := len(contextPackAnyList(projected[legacy])), len(contextPackAnyList(projected[canonical])); got != want {
			t.Fatalf("legacy preview %s retained %d items, canonical %s retained %d", legacy, got, canonical, want)
		}
	}
	if got := len(contextPackAnyList(projected["citations"])); got != contextPackTransportLegacyEvidenceLimit*2 {
		t.Fatalf("citation preview retained %d items, want %d", got, contextPackTransportLegacyEvidenceLimit*2)
	}
	for _, key := range []string{"rankedEvidence", "tokenBudget", "contextCompiler", "promptSections", "tokenImpact", "contextPackQuality"} {
		if _, exists := projected[key]; exists {
			t.Fatalf("redundant compiled alias %s survived transport projection", key)
		}
	}
	for _, key := range []string{"token_impact", "context_pack_quality", "run_advisor", "sourceCoverage", "combinedSources"} {
		if _, exists := projected[key]; exists {
			t.Fatalf("root-owned nested artifact %s survived transport projection", key)
		}
	}
	if _, exists := projected["evidence_points"]; exists {
		t.Fatalf("internal compiler evidence points survived transport projection")
	}
	prompt := anyMap(projected["prompt_sections"])
	if got := len(contextPackAnyList(prompt["evidence"])); got != contextPackTransportLegacyEvidenceLimit {
		t.Fatalf("prompt evidence preview retained %d items, want %d", got, contextPackTransportLegacyEvidenceLimit)
	}
	if anyToInt(prompt["ranked_evidence_count"], -1) != len(ranked) || anyToString(prompt["ranked_evidence_path"]) != "$.context_pack.ranked_evidence" {
		t.Fatalf("prompt evidence pointer does not bind the complete ranked packet: %#v", prompt)
	}
	if _, leakedText := anyMap(contextPackAnyList(prompt["evidence"])[0])["text"]; leakedText {
		t.Fatalf("prompt evidence pointer duplicated ranked text: %#v", prompt["evidence"])
	}
	if got := len(contextPackAnyList(pack["results"])); got != len(items) {
		t.Fatalf("transport projection mutated compiler input: got=%d want=%d", got, len(items))
	}
	projection := anyMap(projected["transport_projection"])
	if anyToString(projection["schema_id"]) != "contextlattice_context_pack_transport_projection.v1" ||
		!anyToBool(projection["ranked_evidence_preserved"]) {
		t.Fatalf("transport projection receipt missing: %#v", projection)
	}
	if got := anyToInt(anyMap(projection["pre_projection_counts"])["results"], -1); got != len(items) {
		t.Fatalf("transport projection pre-count mismatch: got=%d want=%d", got, len(items))
	}
}

func TestFinalizeFullTransportDoesNotRecreateOmittedTokenImpactAlias(t *testing.T) {
	payload := map[string]any{
		"token_impact": map[string]any{"baseline_tokens_estimate": 1000},
		"context_pack": map[string]any{"facts": []any{}},
	}
	attach := func(value map[string]any) map[string]any { return value }
	finalized := finalizeFullTransport(payload, attach, "test", "test_json")
	pack := anyMap(finalized["context_pack"])
	if _, exists := pack["tokenImpact"]; exists {
		t.Fatalf("finalizer recreated an intentionally omitted tokenImpact alias: %#v", pack)
	}
	if _, exists := pack["token_impact"]; exists {
		t.Fatalf("finalizer recreated an intentionally omitted canonical token impact: %#v", pack)
	}
}

func TestReconcileContextPackBoundaryPreservesCanonicalProjectionShape(t *testing.T) {
	ranked := []any{
		map[string]any{"rank": 1, "candidate_id": "rtc_one", "text": "first"},
		map[string]any{"rank": 2, "candidate_id": "rtc_two", "text": "second"},
	}
	pack := projectContextPackForTransport(map[string]any{
		"facts":            []any{},
		"numeric_facts":    []any{},
		"citations":        []any{},
		"results":          []any{},
		"ranked_evidence":  ranked,
		"rankedEvidence":   ranked,
		"prompt_sections":  map[string]any{"evidence": ranked},
		"promptSections":   map[string]any{"evidence": ranked},
		"context_compiler": map[string]any{"ranked_evidence_count": 2, "complete": true},
		"contextCompiler":  map[string]any{"ranked_evidence_count": 2, "complete": true},
	})
	payload := map[string]any{
		"context_pack":     pack,
		"context_compiler": cloneJSONMap(anyMap(pack["context_compiler"])),
	}
	reconcileContextPackBoundaryMetadata(payload, true, nil)
	pack = anyMap(payload["context_pack"])
	for _, alias := range []string{"rankedEvidence", "contextCompiler", "promptSections", "tokenBudget"} {
		if _, exposed := pack[alias]; exposed {
			t.Fatalf("boundary reconcile recreated canonical projection alias %s", alias)
		}
	}
	prompt := anyMap(pack["prompt_sections"])
	if got := len(contextPackAnyList(prompt["evidence"])); got != contextPackTransportLegacyEvidenceLimit {
		t.Fatalf("boundary reconcile expanded pointer preview: got=%d want=%d", got, contextPackTransportLegacyEvidenceLimit)
	}
	if _, duplicatedText := anyMap(contextPackAnyList(prompt["evidence"])[0])["text"]; duplicatedText {
		t.Fatalf("boundary reconcile duplicated ranked evidence text: %#v", prompt["evidence"])
	}
	if !anyToBool(anyMap(payload["context_compiler"])["complete"]) {
		t.Fatalf("boundary reconcile marked an unchanged packet incomplete: %#v", payload["context_compiler"])
	}
}
