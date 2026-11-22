package main

import "testing"

func TestScopedSearchSurvivesUnrelatedCorrectionAndClaimCompaction(t *testing.T) {
	s := correctionTestServer(t)
	mux := buildMux(s)
	const project, topic = correctionTestProject, "memory"
	left, right, revised := sha256Hex("original draft"), sha256Hex("original approval"), sha256Hex("revised approval")
	claim := func(id, file, value, digest string) map[string]any {
		return map[string]any{"claim_id": id, "project": project, "topic_path": topic, "subject": "proposal", "predicate": "status", "object": value,
			"support": []any{map[string]any{"memory_id": project + "::" + file, "content_hash": digest}}}
	}
	leftClaim := claim("claim:left", "notes/left.md", "submitted", left)
	leftClaim["contradicts"] = []string{"claim:right"}
	rightClaim := claim("claim:right", "notes/right.md", "accepted", right)
	correctionTestMustPost(t, mux, "/memory/claims", leftClaim)
	correctionTestMustPost(t, mux, "/memory/claims", rightClaim)
	lookup := func(digest string) string {
		out, _ := scopedSearchConflictSourceCheck(s.temporalClaims, project, topic, searchEvidenceRows(project, topic, left, digest))
		return out.Ref
	}
	before := lookup(right)
	if before == "" {
		t.Fatal("registered original pair did not yield a scoped pointer")
	}
	correctionTestMustPost(t, mux, "/memory/claims", correctionTestLegacyClaim(project))
	req := correctionTestFeedback(t, mux, "wrong", "A different release claim was wrong.", correctionTestTarget, true)
	correct := correctionTestClaim(req)
	correctionTestMustPost(t, mux, "/memory/claims", correct)

	compactAndReopen := func() {
		t.Helper()
		s.temporalClaims.mu.Lock()
		err := s.temporalClaims.compactLocked()
		s.temporalClaims.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if s.temporalClaims, err = newTemporalClaimStoreFromEnv(); err != nil {
			t.Fatal(err)
		}
	}
	compactAndReopen()
	if lookup(right) != before || correctionTestMustPost(t, mux, "/memory/claims", correct)["idempotent_replay"] != true {
		t.Fatal("unrelated correction or compaction changed the case, or lost the original correction receipt")
	}
	correctionTestMustPost(t, mux, "/memory/claims", claim("claim:right", "notes/right.md", "accepted", revised))
	compactAndReopen()
	if current := lookup(revised); lookup(right) != "" || current == "" || current == before {
		t.Fatal("indexed search did not distinguish current evidence from the superseded revision")
	}
}
