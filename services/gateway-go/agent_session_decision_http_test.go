package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAgentDecisionHTTPStartDoesNotHideSecondarySnapshotFailure(t *testing.T) {
	for _, phase := range []string{"after_temp_sync", "after_replace"} {
		t.Run(phase, func(t *testing.T) {
			continuity, store, payload, _ := agentDecisionFixture(t)
			writes := 0
			store.decisionWriteHook = func(at string) error {
				if at == "after_temp_write" {
					writes++
				}
				if writes == 2 && at == phase {
					return errors.New("secondary session-start event persistence failure")
				}
				return nil
			}
			s := &server{agentSessions: store, continuity: continuity}
			gateway := httptest.NewServer(http.HandlerFunc(s.agentsSessionsStart))
			defer gateway.Close()
			raw, _ := json.Marshal(payload)
			status, response := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/start", string(raw))
			if status < 400 || response["ok"] != false || response["partial"] != true || response["binding_recorded"] != true {
				t.Fatalf("typed start hid its secondary persistence failure: status=%d response=%#v", status, response)
			}
			row, _, exists := store.get("sess_bound")
			if !exists || row["decision_binding"] == nil || writes != 2 {
				t.Fatal("failure reporting discarded the already committed binding")
			}
			if phase == "after_replace" && !store.decisionUncertain {
				t.Fatal("post-replacement start failure lost the durable hold")
			}
		})
	}
}
