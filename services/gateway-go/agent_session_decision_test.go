package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func agentDecisionFixture(t *testing.T) (*continuityStore, *agentSessionStore, map[string]any, *agentDecisionBinding) {
	t.Helper()
	continuity, owner, decision := decisionApplicabilityFixture(t)
	change, _, err := continuity.recordDecisionChangeWithOwner(decision, decisionOwnerValidator(owner))
	if err != nil {
		t.Fatal(err)
	}
	selected := &agentDecisionBinding{SchemaID: agentDecisionBindingSchemaID, Project: change.Project,
		ObjectiveID: change.ObjectiveID, TaskIdentityID: change.TaskIdentityID, DecisionChangeID: change.DecisionChangeID,
		AfterDecisionID: change.After.DecisionID, Repo: "alpha-repo", Branch: "main"}
	payload := map[string]any{"session_id": "sess_bound", "agent_id": "test", "project": "alpha", "repo": "alpha-repo",
		"branch": "main", "task_identity_id": change.TaskIdentityID, "decision_binding": agentDecisionJSONMap(selected)}
	binding, err := continuity.resolveAgentDecisionBinding(selected, payload)
	if err != nil {
		t.Fatal(err)
	}
	store := &agentSessionStore{path: filepath.Join(t.TempDir(), "sessions.json"), maxKeep: 16, maxEvents: 4,
		idleTTL: time.Hour, sessions: map[string]map[string]any{}, events: map[string][]map[string]any{}, order: []string{}}
	return continuity, store, payload, binding
}

func TestAgentDecisionBindingExactOwnerAndShape(t *testing.T) {
	continuity, _, payload, binding := agentDecisionFixture(t)
	selected, err := parseAgentDecisionStart(payload)
	if err != nil || selected.RecordDigest != "" || binding.RecordDigest == "" {
		t.Fatal("selector was not separated from server digest")
	}
	for _, mutate := range []func(map[string]any){
		func(p map[string]any) { p["Decision_Binding"] = p["decision_binding"]; delete(p, "decision_binding") },
		func(p map[string]any) { p["decision_binding"] = nil },
		func(p map[string]any) { anyMap(p["decision_binding"])["record_digest"] = binding.RecordDigest },
		func(p map[string]any) {
			a := anyMap(p["decision_binding"])
			a["Branch"] = a["branch"]
			delete(a, "branch")
		},
		func(p map[string]any) { p["task_identity_id"] = "" },
		func(p map[string]any) { p["decision_baseline"] = nil },
	} {
		p := cloneJSONMap(payload)
		mutate(p)
		if _, err := parseAgentDecisionStart(p); err == nil {
			t.Fatal("malformed presence silently treated as an unbound session")
		}
	}
	for _, field := range []string{"project", "objective_id", "task_identity_id", "decision_change_id", "after_decision_id", "repo", "branch"} {
		raw := agentDecisionJSONMap(selected)
		raw[field] = "wrong"
		bad, _ := normalizeAgentDecisionBinding(raw, false)
		if _, err := continuity.resolveAgentDecisionBinding(bad, payload); err == nil {
			t.Fatalf("owner selector field %s was inferred or ignored", field)
		}
	}
}

func TestAgentDecisionStartEnsurePreservesExactBaseline(t *testing.T) {
	_, store, payload, binding := agentDecisionFixture(t)
	row, reused, err := store.startOrReuseDecisionScoped(payload, nil, binding)
	if err != nil || reused || validateStoredAgentDecision(row) != nil {
		t.Fatalf("start: %v", err)
	}
	baseline, _ := agentDecisionBaselineForRow(row)
	if baseline.Revision != 1 || baseline.BindingDigest != agentDecisionDigest(binding) {
		t.Fatal("start did not bind exact owner record")
	}
	before, _ := os.ReadFile(store.path)
	for _, implicit := range []bool{false, true} {
		retry := cloneJSONMap(payload)
		retry["ensure"] = true
		if implicit {
			delete(retry, "session_id")
		}
		equal, reused, err := store.startOrReuseDecisionScoped(retry, nil, binding)
		if err != nil || !reused || !reflect.DeepEqual(equal["decision_baseline"], row["decision_baseline"]) {
			t.Fatalf("equal ensure changed binding or created a replacement: %v", err)
		}
		changed := *binding
		changed.RecordDigest = sha256Hex("different owner record")
		if _, _, err := store.startOrReuseDecisionScoped(retry, nil, &changed); err == nil || len(store.sessions) != 1 {
			t.Fatal("different ensure silently rebound or created another session")
		}
		delete(retry, "decision_binding")
		omitted, reused, err := store.startOrReuseScoped(retry, nil)
		if err != nil || !reused || !reflect.DeepEqual(omitted["decision_binding"], row["decision_binding"]) {
			t.Fatal("legacy omission erased existing binding")
		}
	}
	after, _ := os.ReadFile(store.path)
	if !bytes.Equal(before, after) {
		t.Fatal("ensure rewrote the baseline snapshot")
	}
}

func TestAgentDecisionEnsureCannotBindExistingUnbound(t *testing.T) {
	_, store, payload, binding := agentDecisionFixture(t)
	legacy := cloneJSONMap(payload)
	delete(legacy, "decision_binding")
	if _, err := store.start(legacy); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(store.path)
	for _, implicit := range []bool{false, true} {
		p := cloneJSONMap(payload)
		p["ensure"] = true
		if implicit {
			delete(p, "session_id")
		}
		if _, _, err := store.startOrReuseDecisionScoped(p, nil, binding); err == nil || len(store.sessions) != 1 {
			t.Fatal("ensure adopted decision without an explicit acknowledgement")
		}
	}
	after, _ := os.ReadFile(store.path)
	if !bytes.Equal(before, after) {
		t.Fatal("conflicting ensure changed unbound session")
	}
}

func TestAgentDecisionStartDurabilityFaults(t *testing.T) {
	for _, phase := range []string{"after_temp_sync", "after_replace"} {
		t.Run(phase, func(t *testing.T) {
			_, store, payload, binding := agentDecisionFixture(t)
			store.decisionWriteHook = func(at string) error {
				if at == phase {
					return errors.New("injected snapshot failure")
				}
				return nil
			}
			if _, _, err := store.startOrReuseDecisionScoped(payload, nil, binding); err == nil {
				t.Fatal("injected persistence failure reported success")
			}
			if phase == "after_temp_sync" {
				if len(store.sessions) != 0 || store.decisionUncertain {
					t.Fatal("pre-replacement failure published a volatile binding")
				}
				return
			}
			if !store.decisionUncertain {
				t.Fatal("post-replacement failure did not hold mutations")
			}
			store.decisionWriteHook = nil
			if _, _, err := store.startOrReuseDecisionScoped(payload, nil, binding); !errors.Is(err, errAgentDecisionUnavailable) {
				t.Fatal("commit-unknown blindly retried before verification")
			}
			reopened := &agentSessionStore{path: store.path, maxKeep: store.maxKeep, maxEvents: store.maxEvents, idleTTL: store.idleTTL}
			if err := reopened.load(); err != nil || len(reopened.sessions) != 1 {
				t.Fatalf("durable replacement did not survive reopen: %v", err)
			}
		})
	}
}

func TestAgentDecisionPersistedMalformedPresenceFailsClosed(t *testing.T) {
	_, store, payload, binding := agentDecisionFixture(t)
	row, _, err := store.startOrReuseDecisionScoped(payload, nil, binding)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(map[string]any){
		func(r map[string]any) { r["decision_binding"] = nil },
		func(r map[string]any) { r["decision_baseline"] = nil },
		func(r map[string]any) { r["Decision_Binding"] = r["decision_binding"]; delete(r, "decision_binding") },
		func(r map[string]any) { delete(r, "decision_binding") },
	} {
		bad := cloneJSONMap(row)
		mutate(bad)
		raw, _ := json.Marshal(map[string]any{"sessions": []any{bad}, "events": map[string]any{}})
		if err := os.WriteFile(store.path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err := store.load(); err == nil {
			t.Fatal("malformed persisted binding became legacy absence")
		}
	}
}
