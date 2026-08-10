package main

import "testing"

func TestContextPackResponseRetrievalProofsRequireExplicitDebugForFullReceipts(t *testing.T) {
	assessment := map[string]any{
		"schema_id":        memoryTrustAssessmentContractID,
		"assessed_count":   2,
		"assessments":      []any{map[string]any{"candidate_id": "rtc_one"}},
		"quarantine_count": 1,
	}
	trace := map[string]any{
		"schema_id":         retrievalDecisionTraceContractID,
		"trace_id":          "rdt_one",
		"candidate_count":   2,
		"decision_count":    2,
		"decisions":         []any{map[string]any{"candidate_id": "rtc_one"}},
		"coverage_complete": true,
	}

	boundedAssessment, boundedTrace := contextPackResponseRetrievalProofs(assessment, trace, false)
	if _, exposed := boundedAssessment["assessments"]; exposed {
		t.Fatalf("ordinary response exposed full trust receipt: %#v", boundedAssessment)
	}
	if _, exposed := boundedTrace["decisions"]; exposed {
		t.Fatalf("ordinary response exposed full decision trace: %#v", boundedTrace)
	}
	if anyToInt(boundedAssessment["assessed_count"], 0) != 2 || anyToInt(boundedAssessment["quarantine_count"], 0) != 1 {
		t.Fatalf("bounded trust proof lost summary counts: %#v", boundedAssessment)
	}
	if anyToString(boundedTrace["trace_id"]) != "rdt_one" || !anyToBool(boundedTrace["coverage_complete"]) {
		t.Fatalf("bounded decision proof lost trace identity or coverage: %#v", boundedTrace)
	}

	debugAssessment, debugTrace := contextPackResponseRetrievalProofs(assessment, trace, true)
	if _, exposed := debugAssessment["assessments"]; !exposed {
		t.Fatalf("explicit debug response omitted full trust receipt: %#v", debugAssessment)
	}
	if _, exposed := debugTrace["decisions"]; !exposed {
		t.Fatalf("explicit debug response omitted full decision trace: %#v", debugTrace)
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
