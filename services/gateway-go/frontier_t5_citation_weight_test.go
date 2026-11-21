package main

import (
	"fmt"
	"testing"
)

func TestFrontierT5ContradictionDuplicateCitations(t *testing.T) {
	s := newTemporalClaimEvidenceTestServer(t)
	issuer := newTemporalClaimTestSigner(t, "publisher", "verifier")
	mirror := newTemporalClaimTestSigner(t, "publisher", "verifier")
	independent := []temporalClaimTestSigner{issuer, mirror}
	for i := 0; i < 4; i++ {
		independent = append(independent, newTemporalClaimTestSigner(t, fmt.Sprintf("experiment-%d", i), "verifier"))
	}
	configureTemporalClaimTestSigners(t, independent...)
	left, right := temporalClaimTestPayload("a", "ready"), temporalClaimTestPayload("b", "blocked")
	left["contradicts"], right["contradicts"] = []any{"b"}, []any{"a"}
	leftRef := signedTemporalClaimTestEvidence(t, issuer, left, "support", "a-run", temporalClaimTestEvidence("a"))
	rightRef := signedTemporalClaimTestEvidence(t, issuer, right, "support", "b-run", temporalClaimTestEvidence("b"))
	naysayer := signedTemporalClaimTestEvidence(t, issuer, right, "opposition", "c-run", temporalClaimTestEvidence("c"))
	left["support"], right["support"], right["opposition"] = []any{leftRef}, []any{rightRef}, []any{naysayer}

	recommend := func() map[string]any {
		writeTemporalClaimTestHTTP(t, s, left, false)
		writeTemporalClaimTestHTTP(t, s, right, true)
		r, err := s.buildContradictionResolution(map[string]any{"project": left["project"], "claim_ids": []any{"a", "b"}})
		if err != nil {
			t.Fatal(err)
		}
		return anyMap(r["recommendation"])
	}
	baseline := recommend()
	// A second key for the same publisher signs a different URL, but the same observed run.
	mirroredRef := temporalClaimTestEvidence("mirrored")
	mirroredRef["uri"] = "https://mirror.example/result"
	mirrored := signedTemporalClaimTestEvidence(t, mirror, left, "support", "a-run", mirroredRef)
	left["support"] = []any{leftRef, leftRef, leftRef, leftRef, mirrored}
	right["opposition"] = []any{naysayer, naysayer, naysayer, naysayer, naysayer}
	repeated := recommend()
	if repeated["status"] != "abstained" || repeated["recommendation_id"] != baseline["recommendation_id"] {
		t.Fatalf("duplicates changed advice: baseline=%#v repeated=%#v", baseline, repeated)
	}
	a, b := temporalClaimTestRecommendationRow(repeated, "a"), temporalClaimTestRecommendationRow(repeated, "b")
	if a["support_count"] != 5 || a["bound_support_count"] != 1 ||
		b["opposition_count"] != 5 || b["bound_opposition_count"] != 1 {
		t.Fatalf("duplicates added weight: a=%#v b=%#v", a, b)
	}

	// A bound observation on both sides of the same claim must not count on either.
	both := signedTemporalClaimTestEvidence(t, mirror, left, "opposition", "a-run", mirroredRef)
	left["opposition"] = []any{both}
	a = temporalClaimTestRecommendationRow(recommend(), "a")
	if a["bound_support_count"] != 0 || a["bound_opposition_count"] != 0 {
		t.Fatalf("one contested observation counted on both sides: %#v", a)
	}
	delete(left, "opposition")
	delete(right, "opposition")
	// Reset both weights before adding four independent observations with identical bytes.
	left["support"] = []any{leftRef}
	if base := recommend(); base["status"] != "abstained" {
		t.Fatalf("equal-strength claims did not abstain: %#v", base)
	}
	for i, signer := range independent[2:] {
		id := fmt.Sprintf("run-%d", i)
		left["support"] = append(left["support"].([]any), signedTemporalClaimTestEvidence(t, signer, left, "support", id, temporalClaimTestEvidence(id)))
	}
	result := recommend()
	if result["winning_claim_id"] != "a" || temporalClaimTestRecommendationRow(result, "a")["bound_support_count"] != 5 {
		t.Fatalf("independently bound observations did not affect advice: %#v", result)
	}
	statuses, err := frontierT5CurrentClaimStatuses(s.temporalClaims, "test-project", "a", []string{"b"})
	if err != nil || statuses["a"] != "active" || statuses["b"] != "active" {
		t.Fatalf("recommendation changed lifecycle: statuses=%v err=%v", statuses, err)
	}
}
