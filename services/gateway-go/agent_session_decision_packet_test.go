package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAgentDecisionResumeNegativeSurvivesPacketIdentityAndDelta(t *testing.T) {
	s, owner, id := agentDecisionResumeFixture(t)
	now := time.Now().UTC()
	base := decisionResumeResponse(t, s, id, true)
	if _, reason := validateAgentPacketSelf(base, now.Add(time.Second), true); reason != "" {
		t.Fatal("positive resume packet identity invalid", reason)
	}
	seedMemoryGraphRepairDoc(t, owner, "alpha", "notes/input.md", "pilot/decision", "Acceptance changed before delta continuation", "test", "session")
	negative := decisionResumeResponse(t, s, id, true)
	request := map[string]any{"target_context_pack_tokens": defaultAgentPacketTargetTokens, "hard_limit_tokens": defaultAgentPacketHardTokens}
	target := finalizeAgentPacketWithIdentity(negative, anyMap(base["packet_identity"]), request, now.Add(time.Second))
	if _, reason := validateAgentPacketSelf(target, now.Add(2*time.Second), true); reason != "" {
		t.Fatal("negative marker invalidated final packet identity", reason)
	}
	transport := finalizeAgentPacketForRequestAt(negative, frontierT2DeltaRequest(t, base), now.Add(time.Second))
	reconstructed := transport
	if transport["schema_id"] == agentPacketDeltaContractID {
		var err error
		reconstructed, err = reconstructAgentPacket(base, transport, now.Add(2*time.Second), true)
		if err != nil {
			t.Fatal("eligible continuity delta could not be reconstructed", err)
		}
	} else if anyMap(transport["delta_fallback"])["full_packet_verified"] != true {
		t.Fatal("ineligible delta did not return a verified full packet")
	}
	if agentDecisionResumeProjection(reconstructed, true)["status"] != "changed" {
		t.Fatal("negative continuity status disappeared through delta reconstruction")
	}
	assertAgentDecisionResumeNoAction(t, reconstructed, true)
	tampered := cloneJSONMap(target)
	anyMap(anyMap(tampered["provenance"])["decision_continuity"])["status"] = "verified_match"
	if _, reason := validateAgentPacketSelf(tampered, now.Add(2*time.Second), true); reason == "" {
		t.Fatal("changing continuity status did not invalidate the sealed model-visible packet")
	}
}

func TestAgentDecisionNegativeMarkerEligibleDeltaIsExact(t *testing.T) {
	s, owner, id := agentDecisionResumeFixture(t)
	seedMemoryGraphRepairDoc(t, owner, "alpha", "notes/input.md", "pilot/decision", "Corrected criteria", "test", "session")
	now := time.Now().UTC()
	base := decisionResumeResponse(t, s, id, true)
	// Stable synthetic transport ballast makes a small marker-only update delta
	// eligible. This tests the wire protocol, not retrieval quality or savings.
	anyMap(base["provenance"])["transport_fixture"] = strings.Repeat("Neutral stable protocol fixture. ", 160)
	base = finalizeAgentPacketWithIdentity(base, nil, map[string]any{}, now)
	target := cloneJSONMap(base)
	anyMap(anyMap(target["provenance"])["decision_continuity"])["status"] = "unknown"
	anyMap(anyMap(target["provenance"])["decision_continuity"])["reason"] = "final_owner_or_session_drift"
	target = finalizeAgentPacketWithIdentity(target, anyMap(base["packet_identity"]), map[string]any{}, now.Add(time.Second))
	delta, err := buildAgentPacketDelta(base, target, now.Add(2*time.Second))
	if err != nil || !anyToBool(anyMap(delta["token_budget"])["delta_smaller_than_full"]) {
		t.Fatal("controlled marker-only transport fixture was not delta eligible", err)
	}
	reconstructed, err := reconstructAgentPacket(base, delta, now.Add(2*time.Second), true)
	if err != nil || !agentPacketJSONEqual(agentPacketModelVisibleProjection(reconstructed), agentPacketModelVisibleProjection(target)) {
		t.Fatal("eligible delta erased or altered sticky negative provenance", err)
	}
	assertAgentDecisionResumeNoAction(t, reconstructed, true)
}

func TestAgentDecisionResumeNegativeBudgetKeepsMarkerOrAbstains(t *testing.T) {
	s, _, id := agentDecisionResumeFixture(t)
	row, events, _ := s.agentSessions.get(id)
	row["objective"], row["next_action"] = strings.Repeat("historical intent ", 8000), strings.Repeat("old action ", 8000)
	row["objective_runtime"] = map[string]any{"next_action": "NESTED_STALE_ACTION"}
	s.memoryStore = nil
	observation := s.observeAgentDecisionResume(context.Background(), row, time.Now().UTC())
	packet := observation.render(row, events, time.Now().UTC(), true)
	if packet["ok"] == false {
		if packet["error"] != "decision_resume_budget_exceeded" {
			t.Fatal("pressure produced an unrelated failure", packet["error"])
		}
		return
	}
	if agentDecisionResumeProjection(packet, true)["status"] != "unknown" {
		t.Fatal("token pressure erased the negative status")
	}
	assertAgentDecisionResumeNoAction(t, packet, true)
	if _, reason := validateAgentPacketSelf(packet, time.Now().UTC(), true); reason != "" {
		t.Fatal("pressure emitted an invalid sealed success", reason)
	}
}
