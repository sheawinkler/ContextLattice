package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func publicSessionInvalidScopes() map[string]func(map[string]any) {
	return map[string]func(map[string]any){
		"workspace_owner": func(row map[string]any) {
			row["scope_ownership"] = map[string]any{"schema_id": "agent_session_scope_ownership.v1", "kind": "workspace_bound", "project": "alpha",
				"workspace_id": "retained_owner", "binding_id": "retained_binding", "established_at": nowUTCISO()}
		},
		"null":      func(row map[string]any) { row["scope_ownership"] = nil },
		"nonobject": func(row map[string]any) { row["scope_ownership"] = "public_unbound" },
		"wrong_schema": func(row map[string]any) {
			row["scope_ownership"] = newAgentSessionPublicScopeOwnership("alpha")
			anyMap(row["scope_ownership"])["schema_id"] = "unknown.v1"
		},
		"wrong_project": func(row map[string]any) { row["scope_ownership"] = newAgentSessionPublicScopeOwnership("foreign") },
		"unknown_kind": func(row map[string]any) {
			row["scope_ownership"] = newAgentSessionPublicScopeOwnership("alpha")
			anyMap(row["scope_ownership"])["kind"] = "unknown"
		},
		"hidden_authority": func(row map[string]any) {
			row["scope_ownership"] = newAgentSessionPublicScopeOwnership("alpha")
			anyMap(row["scope_ownership"])["workspaceId"] = "retained_owner"
		},
		"invalid_time": func(row map[string]any) {
			row["scope_ownership"] = newAgentSessionPublicScopeOwnership("alpha")
			anyMap(row["scope_ownership"])["established_at"] = "yesterday"
		},
		"camel_alias": func(row map[string]any) { row["scopeOwnership"] = newAgentSessionPublicScopeOwnership("alpha") },
		"case_alias":  func(row map[string]any) { row["Scope_Ownership"] = nil },
	}
}

func TestPublicSessionUnsupportedOwnerCannotReplayMutateOrLeak(t *testing.T) {
	for name, mutate := range publicSessionInvalidScopes() {
		t.Run(name, func(t *testing.T) {
			continuity, store, row, binding := startUnboundDecisionFixture(t)
			id := anyToString(row["id"])
			ack := agentDecisionAckPayload(t, row, binding, "evt_retained")
			if _, _, err := store.acknowledgeDecisionScoped(id, ack, nil, continuity.resolveAgentDecisionBinding); err != nil {
				t.Fatal(err)
			}
			store.mu.Lock()
			mutate(store.sessions[id])
			beforeRow := agentDecisionDigest(store.sessions[id])
			store.mu.Unlock()
			beforeDisk, _ := os.ReadFile(store.path)
			beforeEvents := agentDecisionDigest(store.events)
			for _, scope := range []map[string]any{nil, newAgentSessionPublicScopeOwnership("alpha")} {
				ensure := map[string]any{"session_id": id, "ensure": true, "project": "alpha"}
				if _, _, err := store.startOrReuseScoped(ensure, scope); err == nil {
					t.Fatal("omitted-binding ensure reinterpreted unsupported ownership")
				}
				if _, _, err := store.startOrReuseDecisionScoped(ensure, scope, binding); err == nil {
					t.Fatal("typed ensure reinterpreted unsupported ownership")
				}
				delete(ensure, "session_id")
				ensure["task_identity_id"] = row["task_identity_id"]
				if _, _, err := store.startOrReuseScoped(ensure, scope); err == nil {
					t.Fatal("implicit ensure replaced unsupported selected row")
				}
				called := false
				if _, _, err := store.acknowledgeDecisionScoped(id, ack, scope, func(*agentDecisionBinding, map[string]any) (*agentDecisionBinding, error) {
					called = true
					return binding, nil
				}); err == nil || called {
					t.Fatal("historical receipt bypassed ownership or read the owner")
				}
				if _, _, err := store.appendEventScoped(id, map[string]any{"type": "checkpoint.written"}, scope); err == nil {
					t.Fatal("ordinary event mutated unsupported row")
				}
			}
			if _, _, found := store.get(id); found || len(store.list("all", "", "", 10, false, true)) != 0 || len(store.list("all", "", "", 10, true, true)) != 0 {
				t.Fatal("unsupported row leaked through shared reads")
			}
			if snapshot := store.runtimeSnapshot(10); anyToInt(snapshot["total"], -1) != 0 || anyToInt(snapshot["active"], -1) != 0 {
				t.Fatal("hidden row leaked counts")
			}
			s := &server{agentSessions: store, continuity: continuity}
			for _, suffix := range []string{"", "/events", "/rollup", "/trace", "/proof-timeline", "/context-package", "/context-package?view=compact"} {
				w := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodGet, "/v1/agents/sessions/"+id+suffix, nil)
				s.agentsSessionsRoute(w, r)
				if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "retained_owner") {
					t.Fatalf("hidden read %s returned %d: %s", suffix, w.Code, w.Body.String())
				}
			}
			for _, payload := range []map[string]any{
				{"session_id": id, "project": "alpha", "ensure": true},
				{"session_id": id, "project": "alpha", "type": "checkpoint.written"},
				ack,
			} {
				payload["session_id"] = id
				body, _ := json.Marshal(payload)
				w := httptest.NewRecorder()
				if _, present := payload["ensure"]; present {
					s.agentsSessionsStart(w, httptest.NewRequest(http.MethodPost, "/v1/agents/sessions/start", bytes.NewReader(body)))
				} else {
					s.agentsSessionsEvent(w, httptest.NewRequest(http.MethodPost, "/v1/agents/sessions/event", bytes.NewReader(body)), "")
				}
				if w.Code < 400 {
					t.Fatalf("HTTP write accepted unsupported owner: %s", w.Body.String())
				}
			}
			afterDisk, _ := os.ReadFile(store.path)
			if len(store.sessions) != 1 || agentDecisionDigest(store.sessions[id]) != beforeRow || beforeEvents != agentDecisionDigest(store.events) || !bytes.Equal(beforeDisk, afterDisk) {
				t.Fatal("rejected operations changed custody, utility, events or disk")
			}
			// Persist the unsupported state as retained input, then verify reopen
			// refuses it without replacing or normalizing the file.
			if err := store.persistLocked(); err != nil {
				t.Fatal(err)
			}
			retained, _ := os.ReadFile(store.path)
			reopened := &agentSessionStore{path: store.path, maxKeep: 16, maxEvents: 8, idleTTL: time.Hour}
			if err := reopened.load(); err == nil || len(reopened.sessions) != 0 {
				t.Fatal("unsupported retained ownership became readable after reopen")
			}
			after, _ := os.ReadFile(store.path)
			if !bytes.Equal(retained, after) {
				t.Fatal("reopen rewrote unsupported retained history")
			}
		})
	}
}

func TestPublicSessionScopeChangesDuringOwnerWaitRejectBothViews(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(map[bool]string{true: "compact", false: "full"}[compact], func(t *testing.T) {
			s, owner, id := agentDecisionResumeFixture(t)
			row, events, _ := s.agentSessions.get(id)
			now := time.Now().UTC()
			observation := s.observeAgentDecisionResume(context.Background(), row, now)
			payload := observation.render(row, events, now, compact)
			fence, err := owner.acquireMemoryEdgeLogFence()
			if err != nil {
				t.Fatal(err)
			}
			defer fence.release()
			deadline, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ctx := &agentDecisionFenceWaitContext{Context: deadline, waiting: make(chan struct{})}
			finished := make(chan error, 1)
			go func() {
				_, err := observation.finish(ctx, s, row, events, now, compact, payload)
				finished <- err
			}()
			select {
			case <-ctx.waiting:
			case <-deadline.Done():
				t.Fatal("finish never waited on the real owner fence")
			}
			s.agentSessions.mu.Lock()
			publicSessionInvalidScopes()["workspace_owner"](s.agentSessions.sessions[id])
			s.agentSessions.mu.Unlock()
			fence.release()
			select {
			case err := <-finished:
				if err == nil {
					t.Fatal("scope changed behind owner fence but resume succeeded")
				}
			case <-deadline.Done():
				t.Fatal("finish did not release")
			}
		})
	}
}

func TestPublicSessionLegacyAndCanonicalScopesRemainUsable(t *testing.T) {
	for _, scope := range []map[string]any{nil, newAgentSessionPublicScopeOwnership("alpha")} {
		_, store, payload, binding := agentDecisionFixture(t)
		if _, _, err := store.startOrReuseDecisionScoped(payload, scope, binding); err != nil {
			t.Fatal(err)
		}
		if _, _, ok := store.get("sess_bound"); !ok {
			t.Fatal("valid manual row hidden")
		}
		if err := store.load(); err != nil {
			t.Fatal("valid legacy/public row failed reopen", err)
		}
	}
}

func TestPublicSessionValidTypedEnsureCannotReinterpretUnsupportedOwner(t *testing.T) {
	for name, mutate := range publicSessionInvalidScopes() {
		t.Run(name, func(t *testing.T) {
			continuity, store, payload, binding := agentDecisionFixture(t)
			if _, _, err := store.startOrReuseDecisionScoped(payload, nil, binding); err != nil {
				t.Fatal(err)
			}
			id := anyToString(payload["session_id"])
			mutate(store.sessions[id])
			before := agentDecisionDigest(store.sessions[id])
			disk, _ := os.ReadFile(store.path)
			s := &server{agentSessions: store, continuity: continuity}
			for _, implicit := range []bool{false, true} {
				ensure := cloneJSONMap(payload)
				ensure["ensure"] = true
				if implicit {
					delete(ensure, "session_id")
				}
				// Prove this holdout reaches the typed store seam rather than
				// merely failing the wrapper's missing-selector validation.
				if selected, err := parseAgentDecisionStart(ensure); err != nil || selected == nil {
					t.Fatal("typed denial fixture is not a valid selection", err)
				}
				for _, scope := range []map[string]any{nil, newAgentSessionPublicScopeOwnership("alpha")} {
					if _, _, err := store.startOrReuseDecisionScoped(ensure, scope, binding); err == nil {
						t.Fatal("valid typed ensure reinterpreted unsupported owner")
					}
				}
				body, _ := json.Marshal(ensure)
				w := httptest.NewRecorder()
				s.agentsSessionsStart(w, httptest.NewRequest(http.MethodPost, "/v1/agents/sessions/start", bytes.NewReader(body)))
				if w.Code < 400 {
					t.Fatal("HTTP typed ensure replaced unsupported owner", w.Body.String())
				}
			}
			after, _ := os.ReadFile(store.path)
			if len(store.sessions) != 1 || agentDecisionDigest(store.sessions[id]) != before || !bytes.Equal(disk, after) || len(store.events[id]) != 0 {
				t.Fatal("denied valid typed ensure changed custody, utility, events or disk")
			}
		})
	}
}
