package main

import (
	"strings"
	"time"
)

func continuousCognitionCaptureProofSnapshot(s *server, session map[string]any, events []map[string]any) agentProofTimelineSnapshot {
	scope := proofTimelineScopeFromSession(session, events)
	continuityRows, continuityAnchor, continuityIntegrity, continuityAvailable, continuityOmitted := s.continuity.proofTimelineRowsWithRevision(scope)
	claims, claimAnchor, claimsAvailable, claimOmitted := s.temporalClaims.proofTimelineRowsWithRevision(scope)
	qualitySamples, qualityOutcomes, qualityAnchor, qualityAvailable, qualityOmitted := s.contextPackQuality.proofTimelineRowsWithRevision(scope)
	tokenRows, tokenAnchor, tokenAvailable, tokenOmitted := s.tokenImpact.proofTimelineRowsWithRevision(scope)
	before := map[string]any{
		"agent_session":        proofTimelineSessionAnchor(session, events),
		"continuity":           continuityAnchor,
		"temporal_claim":       claimAnchor,
		"context_pack_quality": qualityAnchor,
		"token_impact":         tokenAnchor,
	}
	// Do not call proofTimelineAnchors here: its agent-session branch calls
	// agentSessionStore.get, which re-evaluates state against wall-clock time.
	after := map[string]any{
		"agent_session":        proofTimelineSessionAnchor(session, events),
		"continuity":           proofTimelineAnchorAtRevision(continuityAnchor, s.continuity.proofTimelineCurrentRevision()),
		"temporal_claim":       proofTimelineAnchorAtRevision(claimAnchor, s.temporalClaims.proofTimelineCurrentRevision()),
		"context_pack_quality": proofTimelineAnchorAtRevision(qualityAnchor, s.contextPackQuality.proofTimelineCurrentRevision()),
		"token_impact":         proofTimelineAnchorAtRevision(tokenAnchor, s.tokenImpact.proofTimelineCurrentRevision()),
	}
	return agentProofTimelineSnapshot{
		Session: cloneAnyMap(session), Events: cloneMapSlice(events), ContinuityEntries: continuityRows,
		ContinuityIntegrity: continuityIntegrity, Claims: claims,
		QualitySamples: qualitySamples, QualityOutcomes: qualityOutcomes, TokenImpacts: tokenRows,
		Availability: map[string]bool{
			"continuity": continuityAvailable, "temporal_claim": claimsAvailable,
			"context_pack_quality": qualityAvailable, "token_impact": tokenAvailable,
		},
		SourceOmitted: map[string]int{
			"continuity": continuityOmitted, "temporal_claim": claimOmitted,
			"context_pack_quality": qualityOmitted, "token_impact": tokenOmitted,
		},
		SourceAnchorsBefore: before,
		SourceAnchorsAfter:  after,
	}
}

func continuousCognitionMapTimeAt(row map[string]any, fields ...string) (time.Time, bool) {
	for _, field := range fields {
		if value := strings.TrimSpace(anyToString(row[field])); value != "" {
			return parseTimeBestEffort(value)
		}
	}
	return time.Time{}, false
}

func continuousCognitionMapRowsAt(rows []map[string]any, asOf time.Time, fields ...string) ([]map[string]any, int) {
	filtered := make([]map[string]any, 0, len(rows))
	ambiguous := 0
	for _, row := range rows {
		occurredAt, ok := continuousCognitionMapTimeAt(row, fields...)
		if !ok {
			ambiguous++
			continue
		}
		if occurredAt.After(asOf.UTC()) {
			continue
		}
		filtered = append(filtered, cloneAnyMap(row))
	}
	return filtered, ambiguous
}

// continuousCognitionProofSnapshotAt removes evidence that did not yet exist at
// the requested boundary. Latest-only temporal-claim revisions that cross the
// boundary are excluded and surfaced as ambiguous instead of being backdated.
func continuousCognitionProofSnapshotAt(snapshot agentProofTimelineSnapshot, asOf time.Time) (agentProofTimelineSnapshot, int) {
	if asOf.IsZero() {
		return snapshot, 0
	}
	originalBefore := continuousCognitionStableDigest(snapshot.SourceAnchorsBefore)
	originalAfter := continuousCognitionStableDigest(snapshot.SourceAnchorsAfter)
	ambiguous := 0
	snapshot.Events, ambiguous = continuousCognitionMapRowsAt(snapshot.Events, asOf, "created_at")

	continuity := make([]continuityLedgerEntry, 0, len(snapshot.ContinuityEntries))
	for _, row := range snapshot.ContinuityEntries {
		recordedAt, ok := parseTimeBestEffort(row.RecordedAt)
		if !ok {
			ambiguous++
			continue
		}
		if !recordedAt.After(asOf.UTC()) {
			continuity = append(continuity, row)
		}
	}
	snapshot.ContinuityEntries = continuity

	claims := make([]temporalClaim, 0, len(snapshot.Claims))
	for _, claim := range snapshot.Claims {
		createdAt, createdOK := parseTimeBestEffort(claim.CreatedAt)
		updatedAt, updatedOK := parseTimeBestEffort(firstNonEmptyStrings(claim.UpdatedAt, claim.ObservedAt, claim.CreatedAt))
		if !createdOK || !updatedOK {
			ambiguous++
			continue
		}
		if createdAt.After(asOf.UTC()) {
			continue
		}
		if updatedAt.After(asOf.UTC()) {
			ambiguous++
			continue
		}
		claims = append(claims, claim)
	}
	snapshot.Claims = claims

	var count int
	snapshot.QualitySamples, count = continuousCognitionMapRowsAt(snapshot.QualitySamples, asOf, "capturedAt", "captured_at")
	ambiguous += count
	snapshot.QualityOutcomes, count = continuousCognitionMapRowsAt(snapshot.QualityOutcomes, asOf, "gateway_received_at", "capturedAt", "captured_at")
	ambiguous += count
	snapshot.TokenImpacts, count = continuousCognitionMapRowsAt(snapshot.TokenImpacts, asOf, "capturedAt", "captured_at")
	ambiguous += count

	anchors := map[string]any{
		"agent_session": map[string]any{
			"available": len(snapshot.Session) > 0, "event_count": len(snapshot.Events),
			"digest": continuousCognitionStableDigest(map[string]any{"session": snapshot.Session, "events": snapshot.Events}),
		},
		"continuity": map[string]any{
			"available": snapshot.Availability["continuity"], "row_count": len(snapshot.ContinuityEntries),
			"digest": continuousCognitionStableDigest(snapshot.ContinuityEntries),
		},
		"temporal_claim": map[string]any{
			"available": snapshot.Availability["temporal_claim"], "row_count": len(snapshot.Claims),
			"digest": continuousCognitionStableDigest(snapshot.Claims),
		},
		"context_pack_quality": map[string]any{
			"available": snapshot.Availability["context_pack_quality"], "sample_count": len(snapshot.QualitySamples),
			"outcome_count": len(snapshot.QualityOutcomes),
			"digest":        continuousCognitionStableDigest(map[string]any{"samples": snapshot.QualitySamples, "outcomes": snapshot.QualityOutcomes}),
		},
		"token_impact": map[string]any{
			"available": snapshot.Availability["token_impact"], "sample_count": len(snapshot.TokenImpacts),
			"digest": continuousCognitionStableDigest(snapshot.TokenImpacts),
		},
	}
	snapshot.SourceAnchorsBefore = anchors
	snapshot.SourceAnchorsAfter = cloneAnyMap(anchors)
	if originalBefore != originalAfter {
		snapshot.SourceAnchorsAfter["concurrent_snapshot"] = true
	}
	if snapshot.SourceOmitted == nil {
		snapshot.SourceOmitted = map[string]int{}
	}
	if ambiguous > 0 {
		snapshot.SourceOmitted["temporal_projection"] += ambiguous
	}
	return snapshot, ambiguous
}

func continuousCognitionProofProjectionFromSnapshot(snapshot agentProofTimelineSnapshot) (string, string, bool, string) {
	if len(snapshot.Session) == 0 {
		return continuousCognitionUnavailableRef("proof_timeline"), "unavailable", false, continuousCognitionUnavailableRef("source_anchor")
	}
	before := continuousCognitionStableValue(snapshot.SourceAnchorsBefore, 0)
	after := continuousCognitionStableValue(snapshot.SourceAnchorsAfter, 0)
	anchorMaterial := map[string]any{"before": before, "after": after, "availability": snapshot.Availability, "source_omitted": snapshot.SourceOmitted}
	anchorDigest := frontierT6Digest(anchorMaterial)
	complete := true
	for _, available := range snapshot.Availability {
		if !available {
			complete = false
		}
	}
	for _, omitted := range snapshot.SourceOmitted {
		if omitted > 0 {
			complete = false
		}
	}
	material := len(snapshot.Events) > 0 || len(snapshot.ContinuityEntries) > 0 || len(snapshot.Claims) > 0 ||
		len(snapshot.QualitySamples) > 0 || len(snapshot.QualityOutcomes) > 0 || len(snapshot.TokenImpacts) > 0
	if !material {
		for _, anchor := range []any{snapshot.SourceAnchorsBefore, snapshot.SourceAnchorsAfter} {
			if continuousCognitionProofAnchorHasMaterial(anchor) {
				material = true
				break
			}
		}
	}
	if !material || frontierT6Digest(before) != frontierT6Digest(after) {
		complete = false
	}
	status := "verified"
	if !complete {
		status = "degraded"
	}
	return continuousCognitionDigestPrefix("ref_proof_timeline_", anchorMaterial), status, complete, anchorDigest
}

func continuousCognitionProofProjection(s *server, session map[string]any, events []map[string]any) (string, string, bool, string) {
	if s == nil || len(session) == 0 {
		return continuousCognitionUnavailableRef("proof_timeline"), "unavailable", false, continuousCognitionUnavailableRef("source_anchor")
	}
	return continuousCognitionProofProjectionFromSnapshot(continuousCognitionCaptureProofSnapshot(s, session, events))
}

func continuousCognitionProofAnchorHasMaterial(value any) bool {
	anchor := anyMap(value)
	if len(anchor) == 0 {
		return false
	}
	if available, present := anchor["available"]; present && !anyToBool(available) {
		return false
	}
	for _, key := range []string{"event_count", "retained_event_count", "selected_count", "sample_count", "outcome_count", "row_count"} {
		if anyToInt(anchor[key], 0) > 0 {
			return true
		}
	}
	for _, nested := range anchor {
		if continuousCognitionProofAnchorHasMaterial(nested) {
			return true
		}
	}
	return false
}
