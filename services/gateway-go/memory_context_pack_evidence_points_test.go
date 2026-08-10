package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContextPackEvidenceSegmentsAreDistinctBoundedClaims(t *testing.T) {
	summary := `# Release checkpoint
- Pull request 828 merged after local verification.
- Runtime rebuilt from the merged release commit.
- Rollback receipt and remote tag were read back.`
	segments := contextPackEvidenceSegments(summary, 3)
	if len(segments) != 3 {
		t.Fatalf("semantic evidence segmentation returned %d claims, want 3: %#v", len(segments), segments)
	}
	for _, segment := range segments {
		if strings.Contains(segment, "\n") || strings.HasPrefix(segment, "-") {
			t.Fatalf("evidence point was not normalized: %q", segment)
		}
	}
	if strings.Contains(strings.Join(segments, " "), "Release checkpoint") {
		t.Fatalf("short heading became a score-bearing evidence point: %#v", segments)
	}

	jsonSegments := contextPackEvidenceSegments(`{"release":{"status":"merged and locally verified","runtime":"rebuilt from the merged release commit"}}`, 3)
	jsonClaims := map[string]bool{}
	for _, segment := range jsonSegments {
		jsonClaims[segment] = true
	}
	if len(jsonSegments) != 2 ||
		!jsonClaims["release.runtime: rebuilt from the merged release commit"] ||
		!jsonClaims["release.status: merged and locally verified"] {
		t.Fatalf("structured evidence did not retain deterministic labeled claims: %#v", jsonSegments)
	}

	if got := contextPackEvidenceSegments("A single indivisible memory statement remains one cited result.", 3); len(got) != 1 {
		t.Fatalf("single statement was multiplied into score-bearing points: %#v", got)
	}
}

func TestContextPackEvidenceSegmentsPreferMeaningfulProofOverMetadata(t *testing.T) {
	segments := contextPackEvidenceSegmentsForQuery("hardening review proof", `{
  "schema_version": "hardening_eval_v1",
  "recorded_at": "2026-08-06T12:02:32Z",
  "project": "contextlattice",
  "baseline": {"known_gap": "receipts did not preserve immutable task copies"},
  "candidate": {"tests_passed": 92, "compile_check": "passed"},
  "review": {"verdict": "PASS"}
}`, 3)
	wantFragments := []string{"known_gap", "verdict"}
	for _, fragment := range wantFragments {
		found := false
		for _, segment := range segments {
			if strings.Contains(segment, fragment) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("semantic proof %q was displaced by metadata: %#v", fragment, segments)
		}
	}
	proofCheck := false
	for _, segment := range segments {
		if strings.Contains(segment, "tests_passed") || strings.Contains(segment, "compile_check") {
			proofCheck = true
		}
	}
	if !proofCheck {
		t.Fatalf("verification proof was displaced by metadata: %#v", segments)
	}
	for _, segment := range segments {
		if strings.HasPrefix(segment, "schema_version:") || strings.HasPrefix(segment, "recorded_at:") || strings.HasPrefix(segment, "project:") {
			t.Fatalf("metadata displaced meaningful evidence: %#v", segments)
		}
	}
}

func TestContextPackCompilesCitedEvidencePointsWithoutTransportDuplication(t *testing.T) {
	pack := buildContextPackPayload("release merge runtime verification", map[string]any{
		"results": []map[string]any{{
			"project": "contextlattice", "file": "notes/release.md", "source": sourceTopicRollup,
			"topic_path": "release/v4.0.11", "timestamp": "2026-08-09T12:00:00Z", "score": 0.97,
			"summary": "- Pull request merged after local verification.\n- Runtime rebuilt from the merged release commit.\n- Remote tag and rollback receipt checked.",
		}},
		"grounding": map[string]any{"facts": []any{}, "numeric_facts": []any{}},
	}, 10, 10)
	points := contextPackAnyList(pack["evidence_points"])
	if len(points) != 3 {
		t.Fatalf("cited result did not yield three bounded evidence points: %#v", points)
	}
	for index, item := range points {
		point := anyMap(item)
		if anyToString(point["project"]) != "contextlattice" ||
			anyToString(point["file"]) != "notes/release.md" ||
			anyToString(point["topic_path"]) != "release/v4.0.11" ||
			anyToInt(point["point_index"], 0) != index+1 {
			t.Fatalf("evidence point lost exact cited provenance: %#v", point)
		}
		if _, inherited := point["memory_trust_assessment"]; inherited {
			t.Fatalf("derived evidence point inherited the whole-result trust identity: %#v", point)
		}
	}

	compiled := compileContextPackForAgent(
		"release merge runtime verification",
		pack,
		map[string]any{"configured": []any{sourceTopicRollup}, "returned": []any{sourceTopicRollup}, "complete": true},
		objectiveContext{},
		contextPackTokenBudget{TargetContextPackTokens: 4096, RankedEvidenceTokens: 2048, Active: true},
	)
	selectedTexts := map[string]string{}
	for _, item := range contextPackAnyList(compiled["ranked_evidence"]) {
		evidence := anyMap(item)
		selectedTexts[anyToString(evidence["text"])] = anyToString(evidence["kind"])
	}
	for _, item := range points {
		pointText := anyToString(anyMap(item)["text"])
		if _, selected := selectedTexts[pointText]; !selected {
			t.Fatalf("cited evidence point was not selected in any truthful semantic class: point=%q ranked=%#v", pointText, compiled["ranked_evidence"])
		}
	}

	pack["ranked_evidence"] = compiled["ranked_evidence"]
	projected := projectContextPackForTransport(pack)
	if _, exists := projected["evidence_points"]; exists {
		t.Fatalf("internal evidence points were duplicated over transport: %#v", projected["evidence_points"])
	}
	if len(contextPackAnyList(projected["ranked_evidence"])) != len(contextPackAnyList(compiled["ranked_evidence"])) {
		t.Fatalf("transport projection dropped compiled evidence points")
	}
}

func TestContextPackEvidencePointsRemainGloballyAndPerResultBounded(t *testing.T) {
	line := "- verification statement %02d remains independently cited and materially useful.\n"
	results := make([]map[string]any, 0, 4)
	for resultIndex := 0; resultIndex < 4; resultIndex++ {
		var summary strings.Builder
		for lineIndex := 0; lineIndex < 100; lineIndex++ {
			summary.WriteString(strings.ReplaceAll(line, "%02d", anyToString(lineIndex)))
		}
		results = append(results, map[string]any{
			"summary": summary.String(),
			"file":    "notes/bounded-" + anyToString(resultIndex) + ".md",
		})
	}
	points := contextPackResultEvidencePoints("verification statements", results, results, 6)
	if len(points) != 6 {
		t.Fatalf("global evidence point bound changed: got=%d want=6", len(points))
	}
	perResult := map[string]int{}
	for _, item := range points {
		point := anyMap(item)
		key := anyToString(point["file"])
		perResult[key]++
	}
	for key, count := range perResult {
		if count > 3 {
			t.Fatalf("per-result evidence point bound exceeded for %q: %d", key, count)
		}
	}
}

func TestContextPackEvidencePointsUseFullRawEvidenceButNeverQuarantinedText(t *testing.T) {
	longPrefix := strings.Repeat("metadata-only-prefix ", 40)
	raw := []map[string]any{{
		"summary": longPrefix + "\nreview.verdict: PASS\ncandidate.tests_passed: 92",
	}}
	rendered := []map[string]any{{
		"summary": clipText(raw[0]["summary"].(string), 480),
		"project": "contextlattice", "file": "notes/proof.md", "source": sourceTopicRollup,
	}}
	points := contextPackResultEvidencePoints("review test proof", raw, rendered, 3)
	joined := ""
	for _, item := range points {
		joined += "\n" + anyToString(anyMap(item)["text"])
	}
	if !strings.Contains(joined, "review.verdict: PASS") || !strings.Contains(joined, "candidate.tests_passed: 92") {
		t.Fatalf("meaningful proof outside the transport preview was not compiled: %#v", points)
	}

	rendered[0]["quarantined"] = true
	rendered[0]["summary"] = "[quarantined retrieved content]"
	if quarantined := contextPackResultEvidencePoints("review test proof", raw, rendered, 3); len(quarantined) != 0 {
		t.Fatalf("quarantined raw text re-entered through evidence segmentation: %#v", quarantined)
	}
}

func TestContextPackCompilerEvidenceHydratesOnlyHashVerifiedCurrentFiles(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "contextlattice", "notes")
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	content := `{
  "schema_version": "hardening_eval_v1",
  "recorded_at": "2026-08-09T12:00:00Z",
  "baseline": {"known_gap": "mutable receipt path"},
  "candidate": {"tests_passed": 92},
  "review": {"verdict": "PASS"}
}`
	fileName := "notes/proof.json"
	if err := os.WriteFile(filepath.Join(root, "contextlattice", fileName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &memoryStore{policy: memoryStorePolicy{enabled: true, rootPath: root}}
	store.ready.Store(true)
	s := &server{memoryStore: store}
	row := map[string]any{
		"project": "contextlattice", "file": fileName, "source": sourceTopicRollup,
		"summary": clipText(content, 80), "score": 0.9, "topic_path": "release/proof",
		"retrieval_lane": "current_state_index", "projection_authority": "current_event",
		"content_hash": canonicalMemoryContentHash(content),
	}
	response := map[string]any{
		"results":   []map[string]any{row},
		"grounding": map[string]any{"facts": []any{}, "numeric_facts": []any{}},
	}
	s.attachContextPackCompilerEvidence(context.Background(), response, 1)
	if anyToString(row["_compiler_evidence_basis"]) != "verified_current_file" {
		t.Fatalf("exact current file was not hydrated: %#v", row)
	}
	pack := buildContextPackPayload("hardening review proof", response, 10, 10)
	points := contextPackAnyList(pack["evidence_points"])
	if len(points) != 3 {
		t.Fatalf("verified current file did not produce bounded semantic proof: %#v", points)
	}
	for _, item := range points {
		point := anyMap(item)
		if anyToString(point["evidence_basis"]) != "verified_current_file" ||
			anyToString(point["source_content_hash"]) != "sha256:"+canonicalMemoryContentHash(content) {
			t.Fatalf("verified evidence identity was not preserved: %#v", point)
		}
	}
	compiled := compileContextPackForAgent(
		"hardening review proof",
		pack,
		map[string]any{"configured": []any{sourceTopicRollup}, "returned": []any{sourceTopicRollup}, "complete": true},
		objectiveContext{},
		contextPackTokenBudget{TargetContextPackTokens: 4096, RankedEvidenceTokens: 2048, Active: true},
	)
	verifiedSelected := 0
	for _, item := range contextPackAnyList(compiled["ranked_evidence"]) {
		evidence := anyMap(item)
		if anyToString(evidence["evidence_basis"]) != "verified_current_file" {
			continue
		}
		if anyToString(evidence["source_content_hash"]) != "sha256:"+canonicalMemoryContentHash(content) {
			t.Fatalf("selected verified evidence lost its source hash: %#v", evidence)
		}
		verifiedSelected++
	}
	if verifiedSelected < 2 {
		t.Fatalf("verified semantic evidence lost its basis during classification: %#v", compiled["ranked_evidence"])
	}
	stripContextPackCompilerEvidence(response)
	if _, present := row["_compiler_evidence"]; present {
		t.Fatalf("private compiler evidence remained in the retrieval row")
	}

	row["content_hash"] = strings.Repeat("0", 64)
	s.attachContextPackCompilerEvidence(context.Background(), response, 1)
	if _, present := row["_compiler_evidence"]; present {
		t.Fatalf("content-hash mismatch hydrated file evidence")
	}

	target := filepath.Join(projectDir, "target.md")
	if err := os.WriteFile(target, []byte("verified target content"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(projectDir, "link.md")
	if err := os.Symlink(target, link); err == nil {
		if _, _, err := store.readVerifiedEvidencePreview(
			context.Background(), "contextlattice", "notes/link.md", canonicalMemoryContentHash("verified target content"), 32*1024, 4096,
		); err == nil {
			t.Fatal("symlinked evidence path was accepted")
		}
	}
}

func TestContextPackCurrentCorrectionCanMentionObsoleteStateWithoutSelfRetiring(t *testing.T) {
	allocation := contextPackRankedEvidence("repository relocation correction", map[string]any{
		"known_failure_modes": []any{map[string]any{
			"text":    "The obsolete nested repository was retired and must not be reconstructed; use the canonical repository.",
			"project": "contextlattice", "file": "notes/current-correction.md", "source": sourceTopicRollup,
			"topic_path": "repository/relocation", "timestamp": "2026-08-09T12:00:00Z",
		}},
	}, contextPackTokenBudget{TargetContextPackTokens: 1024, RankedEvidenceTokens: 768, Active: true})
	if len(allocation.RankedEvidence) != 1 {
		t.Fatalf("current corrective evidence self-retired because it described obsolete state: %#v", allocation.DecisionTrace)
	}
	row := anyMap(allocation.RankedEvidence[0])
	if anyToString(row["freshness"]) == "superseded" || anyToString(row["kind"]) != "risk" {
		t.Fatalf("current correction received the wrong temporal or semantic posture: %#v", row)
	}
}
