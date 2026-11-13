package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"testing"
)

func agentDecisionAckPayload(t *testing.T, row map[string]any, binding *agentDecisionBinding, eventID string) map[string]any {
	t.Helper()
	baseline, err := agentDecisionBaselineForRow(row)
	if err != nil {
		t.Fatal(err)
	}
	selector := agentDecisionJSONMap(binding)
	delete(selector, "record_digest")
	return map[string]any{"type": "decision.bound", "event_id": eventID, "project": row["project"], "agent_id": row["agent_id"],
		"decision_binding": selector, "decision_ack": agentDecisionJSONMap(agentDecisionAck{SchemaID: agentDecisionAckSchemaID,
			Sequence: baseline.Revision + 1, Expected: baseline})}
}

func startUnboundDecisionFixture(t *testing.T) (*continuityStore, *agentSessionStore, map[string]any, *agentDecisionBinding) {
	t.Helper()
	continuity, store, payload, binding := agentDecisionFixture(t)
	delete(payload, "decision_binding")
	row, err := store.start(payload)
	if err != nil {
		t.Fatal(err)
	}
	return continuity, store, row, binding
}

func TestAgentDecisionAckHistoricalReplayBeforeOwnerAndAfterRestart(t *testing.T) {
	continuity, store, row, binding := startUnboundDecisionFixture(t)
	payload := agentDecisionAckPayload(t, row, binding, "evt_bind")
	acknowledged, event, err := store.acknowledgeDecisionScoped(anyToString(row["id"]), payload, nil, continuity.resolveAgentDecisionBinding)
	if err != nil || event["type"] != "decision.bound" || !reflect.DeepEqual(acknowledged["memory_contribution"], row["memory_contribution"]) {
		t.Fatalf("acknowledgement failed or produced utility: %v", err)
	}
	before, _ := os.ReadFile(store.path)
	assertReplay := func(s *agentSessionStore) {
		called := false
		current, replay, err := s.acknowledgeDecisionScoped(anyToString(row["id"]), payload, nil,
			func(*agentDecisionBinding, map[string]any) (*agentDecisionBinding, error) {
				called = true
				return nil, errors.New("owner unavailable")
			})
		if err != nil || called || replay["idempotent_replay"] != true || anyToInt(current["event_count"], -1) != anyToInt(acknowledged["event_count"], -2) {
			t.Fatalf("historical replay looked up owner, appended or conflicted: %v", err)
		}
	}
	assertReplay(store)
	reopened := &agentSessionStore{path: store.path, maxKeep: store.maxKeep, maxEvents: store.maxEvents, idleTTL: store.idleTTL}
	if err := reopened.load(); err != nil {
		t.Fatal(err)
	}
	assertReplay(reopened)
	after, _ := os.ReadFile(store.path)
	if !bytes.Equal(before, after) {
		t.Fatal("historical receipt changed disk")
	}
	conflict := cloneJSONMap(payload)
	conflict["event_id"] = "evt_different"
	if _, _, err := reopened.acknowledgeDecisionScoped(anyToString(row["id"]), conflict, nil, continuity.resolveAgentDecisionBinding); err == nil {
		t.Fatal("conflicting sequence accepted")
	}
}

func alternateAgentDecisionBinding(t *testing.T, continuity *continuityStore, row map[string]any) *agentDecisionBinding {
	t.Helper()
	old := continuity.decisionChanges[0]
	change, _, err := continuity.recordDecisionChange(map[string]any{"project": old.Project, "objective_id": old.ObjectiveID,
		"task_identity_id": old.TaskIdentityID, "idempotency_key": "alternate-binding", "before": old.After.Summary,
		"after": "Use the retained alternative", "confidence_before": .8, "confidence_after": .6,
		"trigger_evidence": []any{"eval:alternate"}, "actor": "test", "rationale": "Explicitly selected alternative.", "reason_code": "review"})
	if err != nil {
		t.Fatal(err)
	}
	selected := &agentDecisionBinding{SchemaID: agentDecisionBindingSchemaID, Project: change.Project, ObjectiveID: change.ObjectiveID,
		TaskIdentityID: change.TaskIdentityID, DecisionChangeID: change.DecisionChangeID, AfterDecisionID: change.After.DecisionID,
		Repo: "alpha-repo", Branch: "main"}
	binding, err := continuity.resolveAgentDecisionBinding(selected, row)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func TestAgentDecisionAckABARetainsCurrentBaselineAndBoundsReplay(t *testing.T) {
	continuity, store, row, first := startUnboundDecisionFixture(t)
	second := alternateAgentDecisionBinding(t, continuity, row)
	var original map[string]any
	var firstAcknowledgement map[string]any
	for index := 0; index < agentDecisionReceiptWindow+2; index++ {
		binding := first
		if index%2 == 1 {
			binding = second
		}
		payload := agentDecisionAckPayload(t, row, binding, fmt.Sprintf("evt_ack_%d", index))
		if index == 0 {
			original = cloneJSONMap(payload)
		}
		var err error
		row, _, err = store.acknowledgeDecisionScoped(anyToString(row["id"]), payload, nil, continuity.resolveAgentDecisionBinding)
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			firstAcknowledgement = cloneJSONMap(row)
		}
		if index == 2 {
			before, _ := os.ReadFile(store.path)
			current, replay, err := store.acknowledgeDecisionScoped(anyToString(row["id"]), original, nil, nil)
			if err != nil || replay["idempotent_replay"] != true || !reflect.DeepEqual(current["decision_baseline"], row["decision_baseline"]) {
				t.Fatal("late historical replay rolled back the current A-B-A baseline")
			}
			after, _ := os.ReadFile(store.path)
			if !bytes.Equal(before, after) {
				t.Fatal("late replay mutated state")
			}
			stale := agentDecisionAckPayload(t, firstAcknowledgement, second, "evt_stale_aba")
			if _, _, err := store.acknowledgeDecisionScoped(anyToString(row["id"]), stale, nil, continuity.resolveAgentDecisionBinding); err == nil {
				t.Fatal("digest-only ABA acknowledgement accepted")
			}
		}
	}
	receipts, err := agentDecisionReceipts(row)
	if err != nil || len(receipts) != agentDecisionReceiptWindow || len(store.events[anyToString(row["id"])]) != store.maxEvents {
		t.Fatal("binding receipts were lost with event eviction or grew beyond bound", err)
	}
	if _, _, err := store.acknowledgeDecisionScoped(anyToString(row["id"]), original, nil, nil); err == nil {
		t.Fatal("evicted old sequence accepted as a new acknowledgement")
	}
}

func TestAgentDecisionAckConcurrentCAS(t *testing.T) {
	for _, same := range []bool{true, false} {
		t.Run(fmt.Sprintf("same_%t", same), func(t *testing.T) {
			continuity, store, row, binding := startUnboundDecisionFixture(t)
			first := agentDecisionAckPayload(t, row, binding, "evt_first")
			second := cloneJSONMap(first)
			if !same {
				second["event_id"] = "evt_second"
			}
			var wg sync.WaitGroup
			results := make(chan error, 2)
			for _, payload := range []map[string]any{first, second} {
				wg.Add(1)
				go func(p map[string]any) {
					defer wg.Done()
					_, _, err := store.acknowledgeDecisionScoped(anyToString(row["id"]), p, nil, continuity.resolveAgentDecisionBinding)
					results <- err
				}(payload)
			}
			wg.Wait()
			close(results)
			failures := 0
			for err := range results {
				if err != nil {
					failures++
				}
			}
			current, events, _ := store.get(anyToString(row["id"]))
			baseline, _ := agentDecisionBaselineForRow(current)
			if baseline.Revision != 1 || len(events) != 1 || (failures != 0) == same || failures > 1 {
				t.Fatalf("CAS did not preserve one receipt: failures=%d baseline=%#v events=%d", failures, baseline, len(events))
			}
		})
	}
}

func TestAgentDecisionAckRejectsMalformedAndTerminal(t *testing.T) {
	continuity, store, row, binding := startUnboundDecisionFixture(t)
	payload := agentDecisionAckPayload(t, row, binding, "evt_valid")
	for _, mutate := range []func(map[string]any){
		func(p map[string]any) { p["event_id"] = "" },
		func(p map[string]any) { anyMap(p["decision_ack"])["sequence"] = 2 },
		func(p map[string]any) {
			anyMap(anyMap(p["decision_ack"])["expected_baseline"])["incarnation"] = "other"
		},
		func(p map[string]any) {
			p["metadata"] = map[string]any{"utility_verification": map[string]any{"passed": true}}
		},
		func(p map[string]any) { p["Decision_Ack"] = p["decision_ack"]; delete(p, "decision_ack") },
	} {
		bad := cloneJSONMap(payload)
		mutate(bad)
		before, _ := os.ReadFile(store.path)
		if _, _, err := store.acknowledgeDecisionScoped(anyToString(row["id"]), bad, nil, continuity.resolveAgentDecisionBinding); err == nil {
			t.Fatal("invalid acknowledgement accepted")
		}
		after, _ := os.ReadFile(store.path)
		if !bytes.Equal(before, after) {
			t.Fatal("invalid acknowledgement changed state")
		}
	}
	if _, _, err := store.appendEvent(anyToString(row["id"]), map[string]any{"type": "session.completed"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.acknowledgeDecisionScoped(anyToString(row["id"]), payload, nil, continuity.resolveAgentDecisionBinding); !errors.Is(err, errAgentSessionTerminal) {
		t.Fatal("terminal session accepted binding")
	}
}

func TestAgentDecisionAckDurableFaultAndRecreation(t *testing.T) {
	for _, phase := range []string{"after_temp_sync", "after_replace"} {
		t.Run(phase, func(t *testing.T) {
			continuity, store, row, binding := startUnboundDecisionFixture(t)
			payload := agentDecisionAckPayload(t, row, binding, "evt_durable")
			before, _ := os.ReadFile(store.path)
			store.decisionWriteHook = func(at string) error {
				if at == phase {
					return errors.New("injected acknowledgement failure")
				}
				return nil
			}
			if _, _, err := store.acknowledgeDecisionScoped(anyToString(row["id"]), payload, nil, continuity.resolveAgentDecisionBinding); err == nil {
				t.Fatal("ambiguous or failed commit reported acknowledgement")
			}
			current, events, _ := store.get(anyToString(row["id"]))
			if phase == "after_temp_sync" {
				after, _ := os.ReadFile(store.path)
				if !bytes.Equal(before, after) || len(events) != 0 || !reflect.DeepEqual(row["decision_baseline"], current["decision_baseline"]) {
					t.Fatal("pre-commit failure published volatile binding/receipt")
				}
				return
			}
			if current["decision_store_unavailable"] != true {
				t.Fatal("uncertain replacement exposed currentness")
			}
			if _, _, err := store.appendEvent(anyToString(row["id"]), map[string]any{"type": "checkpoint.written"}); !errors.Is(err, errAgentDecisionUnavailable) {
				t.Fatal("normal event overwrote uncertain decision snapshot")
			}
			reopened := &agentSessionStore{path: store.path, maxKeep: store.maxKeep, maxEvents: store.maxEvents, idleTTL: store.idleTTL}
			if err := reopened.load(); err != nil {
				t.Fatal(err)
			}
			if _, replay, err := reopened.acknowledgeDecisionScoped(anyToString(row["id"]), payload, nil, nil); err != nil || replay["idempotent_replay"] != true {
				t.Fatal("post-replacement restart lost its durable historical receipt", err)
			}
		})
	}
	continuity, store, row, binding := startUnboundDecisionFixture(t)
	old := agentDecisionAckPayload(t, row, binding, "evt_old_incarnation")
	store.maxKeep = 1
	if _, err := store.start(map[string]any{"session_id": "sess_replacement", "project": "alpha", "agent_id": "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.start(map[string]any{"session_id": row["id"], "project": "alpha", "agent_id": "test", "repo": "alpha-repo",
		"branch": "main", "task_identity_id": binding.TaskIdentityID}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.acknowledgeDecisionScoped(anyToString(row["id"]), old, nil, continuity.resolveAgentDecisionBinding); err == nil {
		t.Fatal("recreated row accepted an old incarnation's acknowledgement")
	}
}

func TestAgentDecisionBoundEnsureRetainsHistoricalTaskAfterMerge(t *testing.T) {
	continuity, store, payload, binding := agentDecisionFixture(t)
	s := &server{agentSessions: store, continuity: continuity}
	first, _, err := store.startOrReuseDecisionScoped(payload, nil, binding)
	if err != nil {
		t.Fatal(err)
	}
	target := decisionMergeTarget(t, continuity, "bound-ensure")
	mergeDecisionTasks(t, continuity, binding.TaskIdentityID, target)
	for _, implicit := range []bool{false, true} {
		retry := cloneJSONMap(payload)
		retry["task_identity_id"], retry["ensure"] = target, true
		if implicit {
			delete(retry, "session_id")
		}
		selected, _ := parseAgentDecisionStart(retry)
		resolved, err := continuity.resolveAgentDecisionBinding(selected, retry)
		if err != nil {
			t.Fatal(err)
		}
		links, err := s.agentSessionRequestTaskLinks(retry)
		if err != nil {
			t.Fatal(err)
		}
		got, reused, err := store.startOrReuseDecisionScoped(retry, nil, resolved, links)
		if err != nil || !reused || got["task_identity_id"] != first["task_identity_id"] || !reflect.DeepEqual(got["decision_binding"], first["decision_binding"]) {
			t.Fatal("ensure rebound or replaced a legally merged historical task", err)
		}
	}
}

func TestAgentDecisionHTTPBindingAndAcknowledgement(t *testing.T) {
	continuity, store, payload, binding := agentDecisionFixture(t)
	s := &server{agentSessions: store, continuity: continuity}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/agents/sessions/start" {
			s.agentsSessionsStart(w, r)
		} else {
			s.agentsSessionsEvent(w, r, "sess_bound")
		}
	}))
	defer gateway.Close()
	raw, _ := json.Marshal(payload)
	status, response := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/start", string(raw))
	if status != http.StatusOK || response["ok"] != true || anyMap(response["session"])["decision_binding"] == nil {
		t.Fatalf("actual start handler lost typed binding: status=%d response=%#v", status, response)
	}
	row := anyMap(response["session"])
	ack := agentDecisionAckPayload(t, row, binding, "evt_http")
	raw, _ = json.Marshal(ack)
	status, response = postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/sess_bound/event", string(raw))
	if status != http.StatusOK || response["ok"] != true || anyMap(response["event"])["type"] != "decision.bound" {
		t.Fatalf("actual event handler did not acknowledge: status=%d response=%#v", status, response)
	}
	before, _ := os.ReadFile(store.path)
	status, response = postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/sess_bound/event", string(raw))
	if status != http.StatusOK || anyMap(response["event"])["idempotent_replay"] != true || response["utility_reconciliation"] != nil {
		t.Fatal("HTTP replay produced an extra acknowledgement or utility effect")
	}
	for _, field := range []string{"project", "agent_id"} {
		bad := cloneJSONMap(ack)
		bad[field] = "foreign"
		body, _ := json.Marshal(bad)
		status, _ := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/sess_bound/event", string(body))
		if status == http.StatusOK {
			t.Fatalf("foreign %s accepted", field)
		}
	}
	after, _ := os.ReadFile(store.path)
	if !bytes.Equal(before, after) {
		t.Fatal("HTTP replay or foreign ownership changed snapshot")
	}
}
