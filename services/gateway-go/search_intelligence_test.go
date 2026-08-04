package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSearchIntelligenceCandidateIdentityCollapsesTrueContentDuplicatesAndSeparatesPassages(t *testing.T) {
	sharedContent := "The verified release gate requires the local test receipt."
	left := searchIntelligenceCandidateIdentity(map[string]any{
		"content": sharedContent,
		"summary": "The verified release gate requires the local test receipt.",
		"project": "alpha", "file": "notes/release-a.md", "source": "qdrant",
	})
	right := searchIntelligenceCandidateIdentity(map[string]any{
		"content": sharedContent,
		"summary": "The verified release gate requires the local test receipt.",
		"project": "beta", "file": "archive/release-copy.md", "source": "mongo_raw",
	})
	otherPassage := searchIntelligenceCandidateIdentity(map[string]any{
		"content": sharedContent,
		"summary": "The release gate also requires an independent review receipt.",
		"project": "alpha", "file": "notes/release-a.md", "source": "qdrant",
	})

	if left.CandidateRef != right.CandidateRef || left.ContentRef != right.ContentRef || left.PassageRef != right.PassageRef {
		t.Fatalf("true cross-store duplicate did not collapse: left=%#v right=%#v", left, right)
	}
	if left.LocatorRef == right.LocatorRef {
		t.Fatalf("distinct locators were not retained as separate digest references: left=%#v right=%#v", left, right)
	}
	if left.CandidateRef == otherPassage.CandidateRef || left.PassageRef == otherPassage.PassageRef {
		t.Fatalf("distinct passages were not distinguished: left=%#v other=%#v", left, otherPassage)
	}
	for _, ref := range []string{left.CandidateRef, left.ContentRef, left.PassageRef, left.LocatorRef} {
		if !isSearchIntelligenceFullSHA256Ref(ref) {
			t.Fatalf("candidate identity did not use a full SHA-256 reference: %q", ref)
		}
	}
}

func TestMergeRowsKeepsDistinctPassagesFromOneFile(t *testing.T) {
	rows := map[string][]map[string]any{
		"qdrant": {
			{"content": "A release note with multiple decisions.", "summary": "first decision", "project": "alpha", "file": "notes/release.md", "score": 0.93},
			{"content": "A release note with multiple decisions.", "summary": "second decision", "project": "alpha", "file": "notes/release.md", "score": 0.91},
		},
	}
	merged := mergeRowsAll(rows)
	if len(merged) != 2 {
		t.Fatalf("same-file passages collapsed in the literal merge: %#v", merged)
	}
	if rowIdentity(merged[0]) == rowIdentity(merged[1]) {
		t.Fatalf("same-file passages share a pipeline identity: %#v", merged)
	}
}

func TestSearchIntelligenceIdentityPrefersObservedContentOverClaimedDigest(t *testing.T) {
	claimed := "sha256:" + strings.Repeat("d", 64)
	left := map[string]any{"content_ref": claimed, "content": "first full body", "summary": "shared summary"}
	right := map[string]any{"content_ref": claimed, "content": "second full body", "summary": "shared summary"}
	if searchIntelligenceCandidateIdentity(left).CandidateRef == searchIntelligenceCandidateIdentity(right).CandidateRef {
		t.Fatal("claimed content digest collapsed distinct server-observed content")
	}
}

func TestSearchIntelligenceSparseIdentityPreservesDistinctSameFileLocators(t *testing.T) {
	claimed := "sha256:" + strings.Repeat("a", 64)
	first := map[string]any{
		"content_ref": claimed, "project": "alpha", "file": "notes/release.md",
		"chunk_id": "chunk-1", "line_start": 10, "line_end": 20, "score": 0.9,
	}
	second := map[string]any{
		"content_ref": claimed, "project": "alpha", "file": "notes/release.md",
		"chunk_id": "chunk-2", "line_start": 21, "line_end": 30, "score": 0.8,
	}
	firstIdentity := searchIntelligenceCandidateIdentity(first)
	secondIdentity := searchIntelligenceCandidateIdentity(second)
	if firstIdentity.CandidateRef == secondIdentity.CandidateRef {
		t.Fatalf("sparse same-file passages collapsed despite distinct locators: first=%#v second=%#v", firstIdentity, secondIdentity)
	}
	merged := mergeRowsAll(map[string][]map[string]any{"qdrant": {first, second, cloneMap(first)}})
	if len(merged) != 2 {
		t.Fatalf("sparse locator identity did not preserve two chunks and collapse one exact replay: %#v", merged)
	}
}

func TestSearchIntelligenceShadowFusionIsBoundedAndDoesNotChangeLiteralOrdering(t *testing.T) {
	rowsBySource := map[string][]map[string]any{
		"qdrant": {
			{"content": "shared verified release evidence", "summary": "shared verified release evidence", "project": "alpha", "file": "notes/release.md", "source": "qdrant", "score": 0.61},
			{"content": "native winner", "summary": "native winner", "project": "alpha", "file": "notes/native.md", "source": "qdrant", "score": 0.99},
		},
		"topic_rollups": {
			{"content": "shared verified release evidence", "summary": "shared verified release evidence", "project": "archive", "file": "history/release.md", "source": "topic_rollups", "score": 0.54},
			{"content": "single-source history", "summary": "single-source history", "project": "archive", "file": "history/other.md", "source": "topic_rollups", "score": 0.52},
		},
	}
	allMerged := mergeRowsAll(rowsBySource)
	if len(allMerged) != 3 {
		t.Fatalf("true cross-store duplicate did not collapse in the literal pipeline: %#v", allMerged)
	}
	literal := truncateMergedRows(allMerged, 2)
	if got := anyToString(literal[0]["summary"]); got != "native winner" {
		t.Fatalf("fixture does not establish current native order: %#v", literal)
	}

	intelligence := buildSearchIntelligence(searchIntelligenceInput{
		RowsBySource: rowsBySource,
		AllMerged:    allMerged,
		Literal:      literal,
		ResultState:  "ready",
	})
	literalStatus := anyMap(intelligence["literal_results"])
	if anyToString(literalStatus["ordering"]) != "native_score_desc_preserved" || anyToInt(literalStatus["returned_count"], 0) != 2 {
		t.Fatalf("literal result status does not prove production order retention: %#v", literalStatus)
	}
	frontier := anyMap(intelligence["decision_frontier"])
	if anyToString(frontier["status"]) != "shadow_only" || anyToString(anyMap(frontier["fusion"])["method"]) != "weighted_reciprocal_rank_fusion" {
		t.Fatalf("shadow fusion contract missing: %#v", frontier)
	}
	candidates := contextPackAnyList(frontier["candidates"])
	if len(candidates) != 3 {
		t.Fatalf("expected cross-store duplicate collapse into three bounded candidates, got %#v", candidates)
	}
	first := anyMap(candidates[0])
	features := anyMap(first["features"])
	if anyToInt(features["source_count"], 0) != 2 || !containsAnyInList(contextPackAnyList(first["reasons"]), "cross_store_content_duplicate_collapsed") {
		t.Fatalf("shared evidence did not lead the shadow frontier: %#v", first)
	}
	raw, err := json.Marshal(intelligence)
	if err != nil {
		t.Fatalf("marshal search intelligence: %v", err)
	}
	for _, forbidden := range []string{"shared verified release evidence", "notes/release.md", "history/release.md"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("new receipt leaked raw content or path %q: %s", forbidden, raw)
		}
	}
	for i := 0; i < 5; i++ {
		rebuilt, err := json.Marshal(buildSearchIntelligence(searchIntelligenceInput{
			RowsBySource: rowsBySource,
			AllMerged:    allMerged,
			Literal:      literal,
			ResultState:  "ready",
		}))
		if err != nil {
			t.Fatalf("marshal rebuilt intelligence: %v", err)
		}
		if string(rebuilt) != string(raw) {
			t.Fatalf("weighted-RRF shadow receipt was not deterministic: first=%s rebuilt=%s", raw, rebuilt)
		}
	}
}

func TestSearchIntelligenceDecisionImpactRanksCurrentAlignedEvidenceWithoutSelfAwardingVerification(t *testing.T) {
	asOf := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	current := map[string]any{
		"content": "Release approval requires independent verification before rollout.",
		"summary": "current release verification evidence", "project": "alpha", "file": "notes/current.md",
		"source": "topic_rollups", "score": 0.61, "timestamp": asOf.Add(-24 * time.Hour).Format(time.RFC3339Nano),
		"projection_authority": "current_event", "source_owner": "go_native",
		"candidate_utility_verification": map[string]any{
			"independently_verified": true, "verification_status": "verified",
			"evidence_digest": "sha256:" + strings.Repeat("a", 64), "verifier_id": "verifier-a",
		},
		"memory_trust_assessment": map[string]any{
			"trust_label": "bounded", "quarantine": map[string]any{"quarantined": false},
			"provenance": map[string]any{"server_observed": true},
		},
	}
	stale := map[string]any{
		"content": "Release approval paused without supporting verification.",
		"summary": "stale unsupported release evidence", "project": "alpha", "file": "notes/stale.md",
		"source": "qdrant", "score": 0.99, "timestamp": asOf.AddDate(-2, 0, 0).Format(time.RFC3339Nano),
	}
	rowsBySource := map[string][]map[string]any{"qdrant": {stale}, "topic_rollups": {current}}
	allMerged := mergeRowsAll(rowsBySource)
	literal := truncateMergedRows(allMerged, 2)
	if anyToString(literal[0]["summary"]) != "stale unsupported release evidence" {
		t.Fatalf("fixture does not establish native ordering: %#v", literal)
	}

	intelligence := buildSearchIntelligence(searchIntelligenceInput{
		RowsBySource: rowsBySource, AllMerged: allMerged, Literal: literal, ResultState: "ready",
		Query: "release independent verification", RetrievalIntent: "release", AsOf: asOf,
	})
	frontier := anyMap(intelligence["decision_frontier"])
	candidates := contextPackAnyList(frontier["candidates"])
	if len(candidates) != 2 {
		t.Fatalf("candidate count=%d, want 2: %#v", len(candidates), frontier)
	}
	currentRef := searchIntelligenceCandidateIdentity(current).CandidateRef
	if got := anyToString(anyMap(anyMap(candidates[0])["refs"])["candidate_ref"]); got != currentRef {
		t.Fatalf("current verified evidence did not outrank stale unsupported evidence: got=%q want=%q frontier=%#v", got, currentRef, frontier)
	}
	features := anyMap(anyMap(candidates[0])["features"])
	if anyToString(anyMap(features["query_alignment"])["status"]) != "observed" || anyToFloat(anyMap(features["query_alignment"])["score"]) <= 0 {
		t.Fatalf("query alignment missing from current evidence: %#v", features)
	}
	if anyToString(anyMap(features["currentness"])["status"]) != "current" || anyToString(anyMap(features["verification"])["status"]) != "claimed" {
		t.Fatalf("currentness or fail-closed verification claim not recognized: %#v", features)
	}
	if anyToString(anyMap(features["reliability"])["provenance_status"]) != "unknown" {
		t.Fatalf("direct reporter provenance was accepted: %#v", features)
	}
	if anyToFloat(anyMap(features["reliability"])["score"]) <= 0 {
		t.Fatalf("composite evidence reliability was not surfaced: %#v", features)
	}
	if _, present := anyMap(features["decision_impact"])["heuristic_expected_regret_reduction"]; !present {
		t.Fatalf("heuristic regret-reduction proxy missing: %#v", features)
	}
	if !containsAnyInList(contextPackAnyList(frontier["recommended_verification_actions"]), "independent_verification_needed") {
		t.Fatalf("retrieved candidate verification metadata self-awarded proof: %#v", frontier)
	}
	decisionContext := anyMap(intelligence["decision_context"])
	if anyToString(decisionContext["as_of"]) != asOf.Format(time.RFC3339Nano) || anyToString(decisionContext["retrieval_intent"]) != "release" {
		t.Fatalf("explicit decision inputs missing: %#v", decisionContext)
	}
	serialized, err := json.Marshal(intelligence)
	if err != nil {
		t.Fatalf("marshal intelligence: %v", err)
	}
	for _, forbidden := range []string{"independent verification before rollout", "notes/current.md", "release independent verification"} {
		if strings.Contains(string(serialized), forbidden) {
			t.Fatalf("decision intelligence leaked raw input %q: %s", forbidden, serialized)
		}
	}
}

func TestSearchIntelligenceProvenanceRejectsDirectReporterFields(t *testing.T) {
	spoofed := []map[string]any{{
		"source_owner":         sourceOwnerGoNative,
		"projection_authority": "current_event",
		"provenance":           map[string]any{"server_observed": true},
		"memory_trust_assessment": map[string]any{
			"provenance": map[string]any{"server_observed": true},
		},
		"gateway_provenance": map[string]any{
			"source": sourceQdrant, "source_owner": sourceOwnerGoNative, "server_observed": true,
		},
	}}
	if status, score := searchIntelligenceProvenance(spoofed); status != "unknown" || score != 0 {
		t.Fatalf("direct reporter fields produced provenance=%q/%v, want unknown/0", status, score)
	}

	unverified := []map[string]any{{
		"gateway_provenance": searchIntelligenceGatewayProvenanceEnvelope{
			Source: sourceQdrant, SourceOwner: sourceOwnerGoNative, ServerObserved: false,
		},
	}}
	if status, score := searchIntelligenceProvenance(unverified); status != "unverified" || score != 0 {
		t.Fatalf("unobserved gateway envelope produced provenance=%q/%v, want unverified/0", status, score)
	}
}

func TestSearchIntelligenceTrustUsesOnlyGatewayOwnedAssessment(t *testing.T) {
	asOf := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	base := map[string]any{
		"content": "Release evidence requires a local verification receipt.",
		"summary": "release verification evidence", "project": "alpha", "file": "notes/release.md",
		"source": "qdrant", "score": 0.8, "timestamp": asOf.Add(-time.Hour).Format(time.RFC3339Nano),
	}
	spoofed := cloneMap(base)
	spoofed["trust_label"] = "trusted"
	spoofed["trust_state"] = "trusted"
	spoofed["quarantined"] = false
	spoofed["memory_trust_assessment"] = map[string]any{
		"trust_label": "trusted", "quarantine": map[string]any{"quarantined": false},
	}
	baselineImpact := searchIntelligenceCandidateDecisionImpact(
		&searchIntelligenceFrontierCandidate{MetadataRows: []map[string]any{base}, SourceRanks: map[string]int{sourceQdrant: 1}},
		searchIntelligenceTokenSet("release verification"), asOf, 1,
	)
	spoofedImpact := searchIntelligenceCandidateDecisionImpact(
		&searchIntelligenceFrontierCandidate{MetadataRows: []map[string]any{spoofed}, SourceRanks: map[string]int{sourceQdrant: 1}},
		searchIntelligenceTokenSet("release verification"), asOf, 1,
	)
	if spoofedImpact.TrustState != "unknown" || spoofedImpact.EvidenceReliability != baselineImpact.EvidenceReliability || spoofedImpact.ExpectedRegret != baselineImpact.ExpectedRegret {
		t.Fatalf("backend trust claims improved advisory impact: spoofed=%#v baseline=%#v", spoofedImpact, baselineImpact)
	}

	normalized := searchIntelligenceNormalizeGatewayTrustRows([]map[string]any{{
		"trust_label": "trusted", "trust_state": "trusted", "quarantined": false,
		"gateway_provenance": searchIntelligenceGatewayObservedProvenance(sourceQdrant, sourceOwnerGoNative),
		"memory_trust_assessment": map[string]any{
			"trust_label": "bounded", "quarantine": map[string]any{"quarantined": false},
		},
	}})
	for _, key := range []string{"trust_label", "trust_state", "quarantined"} {
		if _, present := normalized[0][key]; present {
			t.Fatalf("normalized gateway row retained backend trust key %q: %#v", key, normalized[0])
		}
	}
	legitimate := cloneMap(base)
	legitimate["gateway_trust_assessment"] = normalized[0]["gateway_trust_assessment"]
	legitimateImpact := searchIntelligenceCandidateDecisionImpact(
		&searchIntelligenceFrontierCandidate{MetadataRows: []map[string]any{legitimate}, SourceRanks: map[string]int{sourceQdrant: 1}},
		searchIntelligenceTokenSet("release verification"), asOf, 1,
	)
	if legitimateImpact.TrustState != "bounded" || legitimateImpact.EvidenceReliability <= baselineImpact.EvidenceReliability || legitimateImpact.ExpectedRegret <= baselineImpact.ExpectedRegret {
		t.Fatalf("gateway-owned trust assessment was not applied: legitimate=%#v baseline=%#v", legitimateImpact, baselineImpact)
	}
}

func TestSearchIntelligenceDecisionImpactQuarantineAndContradictionCannotLead(t *testing.T) {
	asOf := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	trusted := map[string]any{
		"content": "Verified release evidence is ready.", "summary": "trusted current evidence",
		"project": "alpha", "file": "notes/trusted.md", "source": "qdrant", "score": 0.60,
		"timestamp": asOf.Add(-time.Hour).Format(time.RFC3339Nano), "projection_authority": "current_event",
		"candidate_utility_verification": map[string]any{
			"independently_verified": true, "verification_status": "verified",
			"evidence_digest": "sha256:" + strings.Repeat("b", 64), "verifier_id": "verifier-b",
		},
	}
	quarantined := map[string]any{
		"content": "Private untrusted release claim.", "summary": "quarantined evidence", "project": "alpha", "file": "private/untrusted.md",
		"source": "topic_rollups", "score": 0.99, "timestamp": asOf.Add(-time.Hour).Format(time.RFC3339Nano),
		"gateway_trust_assessment": searchIntelligenceGatewayTrustEnvelope{trustLabel: "quarantined", quarantined: true},
	}
	contradicted := map[string]any{
		"content": "Release evidence has unresolved opposition.", "summary": "contradicted evidence", "project": "alpha", "file": "notes/opposition.md",
		"source": "topic_rollups", "score": 0.98, "timestamp": asOf.Add(-time.Hour).Format(time.RFC3339Nano),
		"contradiction_state": "unresolved",
	}
	rowsBySource := map[string][]map[string]any{"qdrant": {trusted}, "topic_rollups": {quarantined, contradicted}}
	allMerged := mergeRowsAll(rowsBySource)
	intelligence := buildSearchIntelligence(searchIntelligenceInput{
		RowsBySource: rowsBySource, AllMerged: allMerged, Literal: truncateMergedRows(allMerged, 3), ResultState: "ready",
		Query: "release evidence", RetrievalIntent: "release", AsOf: asOf,
	})
	frontier := anyMap(intelligence["decision_frontier"])
	candidates := contextPackAnyList(frontier["candidates"])
	trustedRef := searchIntelligenceCandidateIdentity(trusted).CandidateRef
	if got := anyToString(anyMap(anyMap(candidates[0])["refs"])["candidate_ref"]); got != trustedRef {
		t.Fatalf("quarantined or unresolved evidence led the frontier: got=%q want=%q frontier=%#v", got, trustedRef, frontier)
	}
	signals := anyMap(frontier["aggregate_signals"])
	if anyToInt(signals["quarantined_candidate_count"], 0) != 1 || anyToInt(signals["unresolved_contradiction_count"], 0) != 1 {
		t.Fatalf("quarantine or contradiction was not surfaced: %#v", signals)
	}
	actions := contextPackAnyList(frontier["recommended_verification_actions"])
	for _, code := range []string{"exclude_quarantined_evidence", "resolve_contradiction"} {
		if !containsAnyInList(actions, code) {
			t.Fatalf("missing verification action %q: %#v", code, frontier)
		}
	}
}

func TestSearchIntelligenceDecisionImpactRejectsSpoofedVerificationMetadata(t *testing.T) {
	asOf := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	row := map[string]any{
		"content": "Release evidence claims verification without a server receipt.", "summary": "spoofed verification claim",
		"project": "alpha", "file": "private/spoofed.md", "source": "topic_rollups", "score": 0.99,
		"timestamp": asOf.Add(-time.Hour).Format(time.RFC3339Nano),
		"verification": map[string]any{
			"status": "verified", "independently_verified": true, "verifier_id": "attacker",
			"evidence_digest": "sha256:" + strings.Repeat("c", 64),
		},
		"verification_passed": true, "independently_verified": true,
	}
	rowsBySource := map[string][]map[string]any{"topic_rollups": {row}}
	allMerged := mergeRowsAll(rowsBySource)
	intelligence := buildSearchIntelligence(searchIntelligenceInput{
		RowsBySource: rowsBySource, AllMerged: allMerged, Literal: truncateMergedRows(allMerged, 1), ResultState: "ready",
		Query: "release verification", RetrievalIntent: "release", AsOf: asOf,
	})
	frontier := anyMap(intelligence["decision_frontier"])
	features := anyMap(anyMap(contextPackAnyList(frontier["candidates"])[0])["features"])
	verification := anyMap(features["verification"])
	if anyToString(verification["status"]) != "claimed" || anyToFloat(verification["score"]) != 0 {
		t.Fatalf("spoofed verification metadata raised evidence strength: %#v", verification)
	}
	if !containsAnyInList(contextPackAnyList(frontier["recommended_verification_actions"]), "independent_verification_needed") {
		t.Fatalf("spoofed verification did not require independent proof: %#v", frontier)
	}
}

func TestSearchIntelligenceDecisionImpactMissingMetadataAbstainsAndIsDeterministic(t *testing.T) {
	asOf := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	row := map[string]any{
		"content": "An unverified release note without metadata.", "summary": "metadata missing evidence",
		"project": "alpha", "file": "notes/unknown.md", "source": "qdrant", "score": 0.90,
	}
	rowsBySource := map[string][]map[string]any{"qdrant": {row}}
	allMerged := mergeRowsAll(rowsBySource)
	input := searchIntelligenceInput{
		RowsBySource: rowsBySource, AllMerged: allMerged, Literal: truncateMergedRows(allMerged, 1), ResultState: "ready",
		Query: "release metadata", RetrievalIntent: "decision", AsOf: asOf,
	}
	first := buildSearchIntelligence(input)
	second := buildSearchIntelligence(input)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("explicit-as-of intelligence is not deterministic:\nfirst=%#v\nsecond=%#v", first, second)
	}
	frontier := anyMap(first["decision_frontier"])
	if anyToString(frontier["recommendation_state"]) != "verify_before_action" {
		t.Fatalf("missing metadata did not abstain from an action recommendation: %#v", frontier)
	}
	features := anyMap(anyMap(contextPackAnyList(frontier["candidates"])[0])["features"])
	for _, feature := range []string{"currentness", "verification", "acquisition_cost"} {
		if anyToString(anyMap(features[feature])["status"]) != "unknown" {
			t.Fatalf("missing %s was treated as favorable: %#v", feature, features)
		}
	}
	if anyToString(anyMap(features["reliability"])["provenance_status"]) != "unknown" {
		t.Fatalf("missing provenance was treated as observed: %#v", features)
	}
	actions := contextPackAnyList(frontier["recommended_verification_actions"])
	for _, code := range []string{"independent_verification_needed", "timestamp_needed", "provenance_needed", "acquisition_cost_unknown"} {
		if !containsAnyInList(actions, code) {
			t.Fatalf("missing abstention action %q: %#v", code, frontier)
		}
	}
}
