package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestAgentDecisionOrdinarySnapshotFailurePreservesCustody(t *testing.T) {
	for _, operation := range []string{"completion", "retention_eviction"} {
		t.Run(operation, func(t *testing.T) {
			continuity, store, row, binding := startUnboundDecisionFixture(t)
			request := agentDecisionAckPayload(t, row, binding, "evt_custody")
			if _, _, err := store.acknowledgeDecisionScoped(anyToString(row["id"]), request, nil, continuity.resolveAgentDecisionBinding); err != nil {
				t.Fatal(err)
			}
			path := store.path
			before, _ := os.ReadFile(path)
			store.path = t.TempDir() // Wrong object type, not a permissions failure.
			var err error
			if operation == "completion" {
				_, _, err = store.appendEvent(anyToString(row["id"]), map[string]any{"type": "session.completed"})
			} else {
				store.maxKeep = 1
				_, err = store.start(map[string]any{"session_id": "sess_eviction", "project": "alpha", "agent_id": "test"})
			}
			store.path = path
			if err == nil {
				t.Fatal("fixture did not induce snapshot replacement failure")
			}
			if _, event, err := store.acknowledgeDecisionScoped(anyToString(row["id"]), request, nil, nil); err != nil || event["idempotent_replay"] != true {
				t.Fatal("failed ordinary writer lost or hid durable historical custody", err)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("failed ordinary write changed durable root")
			}
		})
	}
}

func TestAgentDecisionOrdinarySuccessfulRewriteIsDurable(t *testing.T) {
	continuity, store, row, binding := startUnboundDecisionFixture(t)
	request := agentDecisionAckPayload(t, row, binding, "evt_before_checkpoint")
	if _, _, err := store.acknowledgeDecisionScoped(anyToString(row["id"]), request, nil, continuity.resolveAgentDecisionBinding); err != nil {
		t.Fatal(err)
	}
	directorySynced := false
	store.decisionWriteHook = func(phase string) error {
		directorySynced = directorySynced || phase == "after_directory_sync"
		return nil
	}
	if _, _, err := store.appendEvent(anyToString(row["id"]), map[string]any{"type": "checkpoint.written"}); err != nil || !directorySynced {
		t.Fatal("ordinary snapshot replaced acknowledged custody without the durable boundary", err)
	}
}

func TestAgentDecisionOmittedEnsureAfterMergeUsesExistingBinding(t *testing.T) {
	for _, implicit := range []bool{false, true} {
		for _, restart := range []bool{false, true} {
			t.Run(fmt.Sprintf("implicit_%t_restart_%t", implicit, restart), func(t *testing.T) {
				continuity, store, payload, _ := agentDecisionFixture(t)
				s := &server{agentSessions: store, continuity: continuity}
				gateway := httptest.NewServer(http.HandlerFunc(s.agentsSessionsStart))
				defer gateway.Close()
				raw, _ := json.Marshal(payload)
				status, response := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/start", string(raw))
				if status != http.StatusOK {
					t.Fatal("bound fixture start failed", status, response)
				}
				original := anyMap(response["session"])
				current := anyToString(original["task_identity_id"])
				for index := 0; index < 2; index++ {
					target := decisionMergeTarget(t, continuity, fmt.Sprintf("omit-%d", index))
					mergeDecisionTasks(t, continuity, current, target)
					current = target
				}
				if restart {
					if err := store.load(); err != nil {
						t.Fatal(err)
					}
					continuity.close()
					var err error
					continuity, err = newContinuityStoreFromEnv()
					if err != nil {
						t.Fatal(err)
					}
					defer continuity.close()
					s.continuity = continuity
				}
				delete(payload, "decision_binding")
				payload["ensure"] = true
				if implicit {
					delete(payload, "session_id")
				}
				before, _ := os.ReadFile(store.path)
				raw, _ = json.Marshal(payload)
				status, response = postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/start", string(raw))
				got := anyMap(response["session"])
				after, _ := os.ReadFile(store.path)
				if status != http.StatusOK || response["reused"] != true || got["id"] != original["id"] ||
					agentDecisionDigest(got["decision_binding"]) != agentDecisionDigest(original["decision_binding"]) ||
					agentDecisionDigest(got["decision_baseline"]) != agentDecisionDigest(original["decision_baseline"]) ||
					len(store.sessions) != 1 || !bytes.Equal(before, after) {
					t.Fatal("omitted ensure conflicted, created replacement, or rebound after legal merges", status, response["error"])
				}
			})
		}
	}
}

func TestAgentDecisionOrdinaryPostCommitFailureHoldsWholeSnapshot(t *testing.T) {
	for _, operation := range []string{"checkpoint", "completion", "retention_eviction"} {
		t.Run(operation, func(t *testing.T) {
			continuity, store, row, binding := startUnboundDecisionFixture(t)
			request := agentDecisionAckPayload(t, row, binding, "evt_before_ordinary")
			if _, _, err := store.acknowledgeDecisionScoped(anyToString(row["id"]), request, nil, continuity.resolveAgentDecisionBinding); err != nil {
				t.Fatal(err)
			}
			store.decisionWriteHook = func(phase string) error {
				if phase == "after_replace" {
					return errors.New("ordinary post-replacement failure")
				}
				return nil
			}
			var err error
			if operation == "retention_eviction" {
				store.maxKeep = 1
				_, err = store.start(map[string]any{"session_id": "sess_after_eviction", "project": "alpha", "agent_id": "test"})
			} else {
				eventType := "checkpoint.written"
				if operation == "completion" {
					eventType = "session.completed"
				}
				_, _, err = store.appendEvent(anyToString(row["id"]), map[string]any{"type": eventType})
			}
			if err == nil || !ownerOnlyAtomicWriteCommitted(err) || !store.decisionUncertain {
				t.Fatal("ordinary committed failure bypassed durable hold", err)
			}
			memory := agentDecisionDigest(map[string]any{"sessions": store.sessions, "events": store.events, "order": store.order})
			reopened := &agentSessionStore{path: store.path, maxKeep: store.maxKeep, maxEvents: store.maxEvents, idleTTL: store.idleTTL}
			if err := reopened.load(); err != nil {
				t.Fatal(err)
			}
			if memory != agentDecisionDigest(map[string]any{"sessions": reopened.sessions, "events": reopened.events, "order": reopened.order}) {
				t.Fatal("committed ordinary mutation was rolled back only in memory")
			}
		})
	}
}

func TestAgentDecisionTaskAliasProofIsBoundedAndNotCallerAuthority(t *testing.T) {
	continuity, store, payload, _ := agentDecisionFixture(t)
	payload["ensure"] = true
	delete(payload, "decision_binding")
	s := &server{agentSessions: store, continuity: continuity}
	foreign := decisionMergeTarget(t, continuity, "independent-proof")
	store.sessions["independent"] = map[string]any{"project": "alpha", "repo": "alpha-repo", "task_identity_id": foreign}
	payload["task_links"] = map[string]any{payload["task_identity_id"].(string): true, foreign: true}
	delete(payload, "session_id")
	proof, err := s.agentSessionRequestTaskLinks(payload)
	if err != nil || proof[foreign] {
		t.Fatal("caller JSON promoted an unrelated task to an authoritative alias", err)
	}
	for index := 0; index < agentSessionTaskAliasProofLimit; index++ {
		store.sessions[fmt.Sprintf("row-%d", index)] = map[string]any{"project": "alpha", "repo": "alpha-repo", "task_identity_id": fmt.Sprintf("task_candidate_%d", index)}
	}
	if _, err := s.agentSessionRequestTaskLinks(payload); !errors.Is(err, errAgentSessionReuseConflict) {
		t.Fatal("incomplete alias coverage was accepted as proof of absence", err)
	}
	payload["session_id"] = "independent"
	proof, err = s.agentSessionRequestTaskLinks(payload)
	if err != nil || proof[foreign] {
		t.Fatal("explicit selector could not bound its own owner check, or inferred a foreign task", err)
	}
}
