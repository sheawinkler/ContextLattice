package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func agentDecisionResumeProjection(payload map[string]any, compact bool) map[string]any {
	if compact {
		return anyMap(anyMap(payload["provenance"])["decision_continuity"])
	}
	return anyMap(payload["decision_continuity"])
}

func assertAgentDecisionResumeNoAction(t *testing.T, payload map[string]any, compact bool) {
	t.Helper()
	copy := cloneJSONMap(payload)
	projection := agentDecisionResumeProjection(copy, compact)
	if anyMap(projection["historical_intent"])["not_current_instructions"] != true {
		t.Fatal("negative view lost explicit historical intent labeling")
	}
	delete(projection, "historical_intent")
	raw, _ := json.Marshal(copy)
	for _, forbidden := range []string{"Deploy the pinned build immediately", "NESTED_STALE_ACTION", "EVENT_STALE_ACTION", "decision_receipts"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatal("negative view retained an executable or unbounded historical alias", forbidden)
		}
	}
	action := anyToString(anyMap(payload["context_package"])["next_action"])
	if compact {
		action = anyToString(anyMap(payload["session_rollup"])["next_action"])
		if contextPackCountAnyTokens(payload).Tokens > defaultAgentPacketHardTokens {
			t.Fatal("sticky negative exceeded the existing transport ceiling")
		}
	}
	if action != agentDecisionResumeAction {
		t.Fatal("negative view did not require fresh verification", action)
	}
}

func TestAgentDecisionResumeNegativeOwnerAndScopeHoldouts(t *testing.T) {
	for _, mode := range []string{"missing_owner", "missing_record", "no_fence", "exact_input", "excluded", "future_effective", "legacy_unbound", "corrupt_digest", "held_snapshot"} {
		t.Run(mode, func(t *testing.T) {
			s, owner, id := agentDecisionResumeFixture(t, func(payload map[string]any) {
				if mode == "future_effective" {
					payload["occurred_at"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
				}
			})
			switch mode {
			case "missing_owner":
				s.memoryStore = nil
			case "missing_record":
				s.continuity.decisionChangeIndex = map[string]int{}
			case "no_fence":
				owner.policy.edgePath = ""
			case "exact_input":
				if err := owner.registerExactStatePath("alpha", "notes/input.md"); err != nil {
					t.Fatal(err)
				}
			case "excluded":
				owner.policy.graphExcludeLowValue = true
				owner.policy.graphExcludeFilePatterns = append(owner.policy.graphExcludeFilePatterns, "notes/input.md")
			case "legacy_unbound":
				delete(s.agentSessions.sessions[id], "decision_binding")
				delete(s.agentSessions.sessions[id], "decision_baseline")
			case "corrupt_digest":
				anyMap(s.agentSessions.sessions[id]["decision_binding"])["record_digest"] = strings.Repeat("a", 64)
			case "held_snapshot":
				s.agentSessions.decisionUncertain = true
			}
			s.agentSessions.sessions[id]["objective_runtime"] = map[string]any{"next_action": "NESTED_STALE_ACTION"}
			for _, compact := range []bool{false, true} {
				payload := decisionResumeResponse(t, s, id, compact)
				projection := agentDecisionResumeProjection(payload, compact)
				expected := "unknown"
				if mode == "legacy_unbound" {
					expected = "legacy_unpinned"
				} else if mode == "future_effective" {
					expected = "not_applicable"
				}
				if projection["status"] != expected {
					t.Fatalf("%s promoted unavailable evidence: %#v", mode, projection)
				}
				assertAgentDecisionResumeNoAction(t, payload, compact)
			}
		})
	}
}

func TestAgentDecisionResumeFinalFenceHoldouts(t *testing.T) {
	for _, mode := range []string{"owner_correction", "ordinary_event", "acknowledgement", "scope", "unrelated_owner_write"} {
		t.Run(mode, func(t *testing.T) {
			for _, compact := range []bool{false, true} {
				s, owner, id := agentDecisionResumeFixture(t)
				row, events, _ := s.agentSessions.get(id)
				now := time.Now().UTC()
				observation := s.observeAgentDecisionResume(context.Background(), row, now)
				if observation.projection["status"] != "verified_match" {
					t.Fatal("fixture did not establish positive custody")
				}
				payload := observation.render(row, events, now, compact)
				switch mode {
				case "owner_correction":
					seedMemoryGraphRepairDoc(t, owner, "alpha", "notes/input.md", "pilot/decision", "Correction during compilation", "test", "session")
				case "ordinary_event":
					if _, _, err := s.agentSessions.appendEvent(id, map[string]any{"type": "checkpoint.written", "summary": "EVENT_STALE_ACTION", "metadata": map[string]any{"next_action": "NESTED_STALE_ACTION"}}); err != nil {
						t.Fatal(err)
					}
				case "acknowledgement":
					binding := alternateAgentDecisionBinding(t, s.continuity, row)
					ack := agentDecisionAckPayload(t, row, binding, "evt_during_compile")
					if _, _, err := s.agentSessions.acknowledgeDecisionScoped(id, ack, nil, s.continuity.resolveAgentDecisionBinding); err != nil {
						t.Fatal(err)
					}
				case "scope":
					s.agentSessions.sessions[id]["scope_ownership"] = newAgentSessionPublicScopeOwnership("other")
				case "unrelated_owner_write":
					seedMemoryGraphRepairDoc(t, owner, "alpha", "notes/unrelated.md", "other", "Independent current work", "test", "session")
				}
				final, err := observation.finish(context.Background(), s, row, events, now, compact, payload)
				if mode == "scope" {
					if !errors.Is(err, errAgentDecisionResumeScope) || final != nil {
						t.Fatal("changed authorization scope exposed an old package", err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				projection := agentDecisionResumeProjection(final, compact)
				if mode == "unrelated_owner_write" {
					if projection["status"] != "verified_match" {
						t.Fatal("unrelated owner change invalidated unchanged original pins")
					}
					continue
				}
				if projection["status"] != "unknown" || projection["record_digest"] != observation.projection["record_digest"] || projection["acceptance_criteria"] != "unknown" {
					t.Fatal("final drift refreshed selection or retained a positive claim", projection)
				}
				assertAgentDecisionResumeNoAction(t, final, compact)
			}
		})
	}
}

func TestAgentDecisionResumeCriteriaOmissionAndRestart(t *testing.T) {
	s, _, id := agentDecisionResumeFixture(t, func(payload map[string]any) { anyMap(payload["applicability"])["assumptions"] = []any{} })
	s.continuity.close()
	var err error
	s.continuity, err = newContinuityStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer s.continuity.close()
	if err := s.agentSessions.load(); err != nil {
		t.Fatal(err)
	}
	for _, compact := range []bool{false, true} {
		projection := agentDecisionResumeProjection(decisionResumeResponse(t, s, id, compact), compact)
		if projection["status"] != "verified_match" || projection["acceptance_criteria"] != "not_provided" || projection["assumption_predicates"] != "not_evaluated" {
			t.Fatal("restart or absent criteria invented semantic satisfaction", projection)
		}
	}
}
