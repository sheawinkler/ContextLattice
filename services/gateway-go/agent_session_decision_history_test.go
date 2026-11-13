package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestAgentDecisionEqualEnsurePreservesDifferentHistoricalTask(t *testing.T) {
	for _, implicit := range []bool{false, true} {
		for _, restart := range []bool{false, true} {
			t.Run(fmt.Sprintf("implicit_%t_restart_%t", implicit, restart), func(t *testing.T) {
				continuity, store, payload, binding := agentDecisionFixture(t)
				current := binding.TaskIdentityID
				for index := 0; index < 2; index++ {
					target := decisionMergeTarget(t, continuity, fmt.Sprintf("different-history-%d", index))
					mergeDecisionTasks(t, continuity, current, target)
					current = target
				}
				unbound := cloneJSONMap(payload)
				delete(unbound, "decision_binding")
				unbound["task_identity_id"] = current
				s := &server{agentSessions: store, continuity: continuity}
				gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/v1/agents/sessions/start" {
						s.agentsSessionsStart(w, r)
					} else {
						s.agentsSessionsEvent(w, r, "sess_bound")
					}
				}))
				defer gateway.Close()
				raw, _ := json.Marshal(unbound)
				status, response := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/start", string(raw))
				if status != http.StatusOK {
					t.Fatal("fixture did not establish a current-task session", status, response)
				}
				row := anyMap(response["session"])
				ack := agentDecisionAckPayload(t, row, binding, "evt_different_history")
				raw, _ = json.Marshal(ack)
				status, response = postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/sess_bound/event", string(raw))
				if status != http.StatusOK {
					t.Fatal("fixture did not acknowledge original historical decision", status, response)
				}
				original := anyMap(response["session"])
				if original["task_identity_id"] == binding.TaskIdentityID {
					t.Fatal("fixture did not retain genuinely different historical task IDs")
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
				retry := cloneJSONMap(unbound)
				retry["decision_binding"], retry["ensure"] = payload["decision_binding"], true
				if implicit {
					delete(retry, "session_id")
				}
				before, _ := os.ReadFile(store.path)
				raw, _ = json.Marshal(retry)
				status, response = postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/start", string(raw))
				got := anyMap(response["session"])
				after, _ := os.ReadFile(store.path)
				if status != http.StatusOK || response["reused"] != true || got["id"] != original["id"] ||
					got["task_identity_id"] != original["task_identity_id"] ||
					agentDecisionDigest(got["decision_binding"]) != agentDecisionDigest(original["decision_binding"]) ||
					agentDecisionDigest(got["decision_baseline"]) != agentDecisionDigest(original["decision_baseline"]) ||
					len(store.sessions) != 1 || !bytes.Equal(before, after) {
					t.Fatal("selector presence changed session selection or rebound historical identities", status, response["error"])
				}
			})
		}
	}
}
