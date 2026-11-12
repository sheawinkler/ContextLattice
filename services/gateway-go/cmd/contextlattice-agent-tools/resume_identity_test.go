package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func isolateResumeIdentityTest(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CONTEXTLATTICE_GLOBAL_HOME", t.TempDir())
	t.Setenv("CONTEXTLATTICE_SESSION_ID", "")
}

func TestResumeDiscoveryHoldouts(t *testing.T) {
	row := func(id, project, status string, extras map[string]any) map[string]any {
		r := map[string]any{"id": id, "project": project, "status": status}
		for k, v := range extras {
			r[k] = v
		}
		return r
	}
	for _, tc := range []struct {
		name    string
		rows    []any
		project string
		actor   string
		limit   int
		ok      any
		want    string
	}{
		{"reversed_ambiguity", []any{row("sess-z", "alpha", "paused", nil), row("sess-a", "alpha", "blocked", nil)}, "alpha", "", 20, true, ""},
		{"foreign_neighbors", []any{row("sess-done", "alpha", "completed", nil), row("sess-foreign", "beta", "active", nil), row("sess-a", "alpha", "active", nil)}, "alpha", "", 20, true, "sess-a"},
		{"paused", []any{row("sess-a", "alpha", "paused", nil)}, "alpha", "", 20, true, "sess-a"},
		{"blocked", []any{row("sess-a", "alpha", "blocked", nil)}, "alpha", "", 20, true, "sess-a"},
		{"project_case", []any{row("sess-a", "Alpha", "active", nil)}, "alpha", "", 20, true, "sess-a"},
		{"requested_actor", []any{row("sess-a", "alpha", "active", map[string]any{"agent_id": "Actor-A"})}, "alpha", "Actor-A", 20, true, "sess-a"},
		{"actor_case_is_identity", []any{row("sess-a", "alpha", "active", map[string]any{"agent_id": "actor-a"})}, "alpha", "Actor-A", 20, true, ""},
		{"missing_actor", []any{row("sess-a", "alpha", "active", nil)}, "alpha", "actor-a", 20, true, ""},
		{"stale", []any{row("sess-a", "alpha", "active", map[string]any{"stale": true})}, "alpha", "", 20, true, ""},
		{"typed_staleness", []any{row("sess-a", "alpha", "active", map[string]any{"stale": "false"})}, "alpha", "", 20, true, ""},
		{"fresh", []any{row("sess-a", "alpha", "active", map[string]any{"stale": false})}, "alpha", "", 20, true, "sess-a"},
		{"unknown_status", []any{row("sess-a", "alpha", "unknown", nil)}, "alpha", "", 20, true, ""},
		{"typed_status", []any{row("sess-a", "alpha", "active", map[string]any{"status": 1})}, "alpha", "", 20, true, ""},
		{"typed_project", []any{row("sess-a", "1", "active", map[string]any{"project": 1})}, "1", "", 20, true, ""},
		{"typed_id", []any{row("sess-a", "alpha", "active", map[string]any{"id": 1})}, "alpha", "", 20, true, ""},
		{"whitespace_identity", []any{row(" sess-a ", "alpha", "active", nil)}, "alpha", "", 20, true, ""},
		{"duplicate_identity", []any{row("sess-a", "alpha", "active", nil), row("sess-a", "alpha", "completed", nil)}, "alpha", "", 20, true, ""},
		{"full_preview_not_complete", []any{row("sess-a", "alpha", "active", nil), row("sess-other", "beta", "active", nil)}, "alpha", "", 2, true, ""},
		{"literal_success", []any{row("sess-a", "alpha", "active", nil)}, "alpha", "", 20, "true", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{"ok": tc.ok, "sessions": tc.rows, "complete": true}
			before, _ := json.Marshal(payload)
			id, err := uniqueResumeSession(payload, tc.project, tc.actor, tc.limit)
			if id != tc.want || tc.want != "" && err != nil {
				t.Fatalf("id=%q want=%q err=%v", id, tc.want, err)
			}
			after, _ := json.Marshal(payload)
			if !bytes.Equal(before, after) {
				t.Fatal("selection mutated source evidence")
			}
		})
	}
}

func TestResumePackageIdentityHoldouts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		patch  map[string]any
		wantOK bool
	}{
		{"compact", map[string]any{"session": map[string]any{"id": "sess-a", "project": "alpha", "agent_id": "actor-a"}, "outcome": map[string]any{"session_id": "sess-a"}}, true},
		{"full", map[string]any{"rollup": map[string]any{"session_id": "sess-a", "project": "alpha", "agent_id": "actor-a"}}, true},
		{"project_case", map[string]any{"project": "Alpha"}, true},
		{"false_success", map[string]any{"ok": false}, false},
		{"string_success", map[string]any{"ok": "true"}, false},
		{"wrong_session", map[string]any{"session_id": "sess-b"}, false},
		{"missing_session", map[string]any{"session_id": nil}, false},
		{"missing_project", map[string]any{"project": nil}, false},
		{"wrong_actor", map[string]any{"agent_id": "actor-b"}, false},
		{"missing_actor", map[string]any{"agent_id": nil}, false},
		{"nested_session_drift", map[string]any{"session": map[string]any{"id": "sess-b"}}, false},
		{"nested_scope_drift", map[string]any{"session": map[string]any{"project": "beta"}}, false},
		{"nested_actor_drift", map[string]any{"rollup": map[string]any{"agent_id": "actor-b"}}, false},
		{"outcome_drift", map[string]any{"outcome": map[string]any{"session_id": "sess-b"}}, false},
		{"typed_nested_identity", map[string]any{"session": map[string]any{"id": 1}}, false},
		{"malformed_nested", map[string]any{"session": nil}, false},
		{"clipped_actor_is_not_identity", map[string]any{"agent_id": "actor-"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{"ok": true, "session_id": "sess-a", "project": "alpha", "agent_id": "actor-a"}
			for k, v := range tc.patch {
				payload[k] = v
			}
			if err := validateResumePackageIdentity(payload, "sess-a", "alpha", "actor-a"); (err == nil) != tc.wantOK {
				t.Fatalf("accepted=%v want=%v err=%v", err == nil, tc.wantOK, err)
			}
		})
	}
}

func TestResumeDiscoveryDoesNotTreatUnknownRowsAsIneligible(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  any
	}{
		{"unknown_project", map[string]any{"id": "sess-other", "status": "active", "agent_id": "actor-a"}},
		{"unknown_status", map[string]any{"id": "sess-other", "project": "alpha", "agent_id": "actor-a"}},
		{"unknown_actor", map[string]any{"id": "sess-other", "project": "alpha", "status": "active"}},
		{"typed_staleness", map[string]any{"id": "sess-other", "project": "alpha", "status": "active", "agent_id": "actor-a", "stale": "false"}},
		{"typed_identity", map[string]any{"id": 7, "project": "alpha", "status": "active", "agent_id": "actor-a"}},
		{"malformed_row", "not a session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			valid := map[string]any{"id": "sess-a", "project": "alpha", "status": "active", "agent_id": "actor-a"}
			id, err := uniqueResumeSession(map[string]any{"ok": true, "sessions": []any{valid, tc.row}}, "alpha", "actor-a", 20)
			if err == nil || id != "" {
				t.Fatalf("unknown competing row certified uniqueness: id=%q err=%v", id, err)
			}
		})
	}
}

func TestResumeExplicitSelectorPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"positional_over_environment", []string{"sess-positional"}, "sess-positional"},
		{"flag_over_environment", []string{"--session-id", "sess-flag"}, "sess-flag"},
		{"flag_over_positional", []string{"sess-positional", "--session-id", "sess-flag"}, "sess-flag"},
		{"environment_fallback", nil, "sess-environment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateResumeIdentityTest(t)
			t.Setenv("CONTEXTLATTICE_SESSION_ID", "sess-environment")
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path != "/v1/agents/sessions/"+tc.want+"/context-package" {
					t.Errorf("explicit selection ignored: %s want %s", r.URL.Path, tc.want)
				}
				parts := strings.Split(r.URL.Path, "/")
				id := parts[len(parts)-2]
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "session_id": id, "project": "alpha", "session": map[string]any{"id": id, "project": "alpha", "status": "completed"}})
			}))
			defer gateway.Close()
			var out bytes.Buffer
			c := newCLI(&out, ioDiscard{})
			c.baseURL = gateway.URL
			if err := c.cmdResume(append([]string{"--project", "alpha", "--raw"}, tc.args...)); err != nil {
				t.Fatal(err)
			}
			if readSessionState("alpha")["session_id"] != tc.want {
				t.Fatalf("wrong selected ID cached: %#v", readSessionState("alpha"))
			}
		})
	}
}

func TestResumeImplicitSelectionRechecksPackageLiveness(t *testing.T) {
	for _, view := range []string{"compact", "full"} {
		for _, source := range []string{"discovery", "cache"} {
			for _, drift := range []string{"completed", "expired", "stale", "missing", "typed", "contradictory"} {
				t.Run(view+"/"+source+"/"+drift, func(t *testing.T) {
					isolateResumeIdentityTest(t)
					var before []byte
					if source == "cache" {
						writeSessionState("alpha", "sess-a", "prior intent", "actor-a")
						before, _ = os.ReadFile(sessionStatePath("alpha"))
					}
					gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						live := map[string]any{"id": "sess-a", "project": "alpha", "agent_id": "actor-a", "status": "active", "stale": false}
						if r.URL.Path == "/v1/agents/sessions" {
							_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "sessions": []any{live}})
							return
						}
						if r.URL.Path == "/v1/agents/sessions/sess-a" {
							_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "session": live})
							return
						}
						payload := map[string]any{"ok": true, "session_id": "sess-a", "project": "alpha", "agent_id": "actor-a"}
						owner := map[string]any{"project": "alpha", "agent_id": "actor-a", "status": "active"}
						if view == "full" {
							owner["session_id"] = "sess-a"
							payload["rollup"] = owner
						} else {
							owner["id"] = "sess-a"
							payload["session"] = owner
							payload["session_rollup"] = map[string]any{"status": "active"}
						}
						switch drift {
						case "completed", "expired":
							owner["status"] = drift
						case "stale":
							owner["stale"] = true
						case "missing":
							delete(owner, "status")
						case "typed":
							owner["status"] = true
						case "contradictory":
							if view == "full" {
								payload["status"] = "completed"
							} else {
								asMap(payload["session_rollup"])["status"] = "completed"
							}
						}
						_ = json.NewEncoder(w).Encode(payload)
					}))
					defer gateway.Close()
					var out bytes.Buffer
					c := newCLI(&out, ioDiscard{})
					c.baseURL = gateway.URL
					args := []string{"--project", "alpha", "--agent-id", "actor-a", "--raw"}
					if view == "full" {
						args = append(args, "--full")
					}
					if err := c.cmdResume(args); err == nil {
						t.Fatalf("implicit resume accepted final %s liveness: %s", drift, out.String())
					}
					after, _ := os.ReadFile(sessionStatePath("alpha"))
					if out.Len() != 0 || !bytes.Equal(before, after) {
						t.Fatal("implicit drift emitted or changed cached state")
					}
				})
			}
		}
	}
}

func TestResumeExactSelectionAndCacheSafety(t *testing.T) {
	for _, name := range []string{"explicit_history", "positional", "environment", "cached", "cached_foreign", "cached_stale", "cached_unknown", "refused_package_keeps_cache", "limit_cannot_hide_ambiguity"} {
		t.Run(name, func(t *testing.T) {
			isolateResumeIdentityTest(t)
			id := "sess-exact"
			args := []string{"--project", "alpha", "--agent-id", "actor-a", "--raw"}
			cached := strings.HasPrefix(name, "cached") || name == "refused_package_keeps_cache"
			switch name {
			case "explicit_history":
				args = append(args, "--session-id", id, "--full")
			case "positional":
				args = append(args, id)
			case "environment":
				t.Setenv("CONTEXTLATTICE_SESSION_ID", id)
			case "limit_cannot_hide_ambiguity":
				args = append(args, "--limit", "1")
			}
			var before []byte
			if cached {
				writeSessionState("alpha", id, "prior checkpoint", "actor-a")
				before, _ = os.ReadFile(sessionStatePath("alpha"))
			}
			var lists atomic.Int32
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				row := map[string]any{"id": id, "project": "alpha", "agent_id": "actor-a", "status": "active"}
				switch r.URL.Path {
				case "/v1/agents/sessions":
					lists.Add(1)
					if r.URL.Query().Get("agent") != "actor-a" || r.URL.Query().Get("project") != "alpha" {
						t.Error("requested scope filter was not sent")
					}
					rows := []any{map[string]any{"id": "sess-discovered", "project": "alpha", "agent_id": "actor-a", "status": "active"}}
					if name == "limit_cannot_hide_ambiguity" {
						if r.URL.Query().Get("limit") != "2" {
							t.Error("single-row preview can hide competing sessions")
						}
						rows = append(rows, row)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "sessions": rows})
					return
				case "/v1/agents/sessions/" + id:
					if name == "cached_foreign" {
						row["project"] = "beta"
					}
					if name == "cached_stale" {
						row["stale"] = true
					}
					if name == "cached_unknown" {
						delete(row, "status")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "session": row})
					return
				}
				if !strings.HasSuffix(r.URL.Path, "/context-package") {
					t.Errorf("unexpected route %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				parts := strings.Split(r.URL.Path, "/")
				selected := parts[len(parts)-2]
				row["id"] = selected
				if name == "explicit_history" {
					row["status"] = "completed"
					if r.URL.Query().Get("view") != "" {
						t.Error("explicit full historical view was changed")
					}
				}
				if name == "refused_package_keeps_cache" {
					row["project"] = "beta"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "session_id": selected, "project": "alpha", "agent_id": "actor-a", "session": row})
			}))
			defer gateway.Close()
			var out, errOut bytes.Buffer
			c := newCLI(&out, &errOut)
			c.baseURL = gateway.URL
			err := c.cmdResume(args)
			blocked := name == "refused_package_keeps_cache" || name == "limit_cannot_hide_ambiguity"
			if (err != nil) != blocked {
				t.Fatalf("err=%v blocked=%v stdout=%s", err, blocked, out.String())
			}
			if blocked {
				after, _ := os.ReadFile(sessionStatePath("alpha"))
				if out.Len() != 0 || !bytes.Equal(before, after) {
					t.Fatal("refused resume emitted or replaced cached state")
				}
				return
			}
			wantLists := int32(0)
			wantID := id
			if name == "cached_foreign" || name == "cached_stale" || name == "cached_unknown" {
				wantLists = 1
				wantID = "sess-discovered"
			}
			if lists.Load() != wantLists || readSessionState("alpha")["session_id"] != wantID {
				t.Fatalf("wrong exact selection/cache: lists=%d state=%#v", lists.Load(), readSessionState("alpha"))
			}
		})
	}
}

func TestResumeIdentityBaseline(t *testing.T) {
	for _, tc := range []struct {
		name            string
		rows            []any
		agentID         string
		responseProject string
		wantOK          bool
	}{
		{"ambiguous", []any{map[string]any{"id": "sess-a", "project": "alpha", "status": "active"}, map[string]any{"id": "sess-b", "project": "alpha", "status": "active"}}, "", "", false},
		{"foreign_project", []any{map[string]any{"id": "sess-beta", "project": "beta", "status": "active"}}, "", "", false},
		{"foreign_actor", []any{map[string]any{"id": "sess-other", "project": "alpha", "status": "active", "agent_id": "other"}}, "requested", "", false},
		{"terminal", []any{map[string]any{"id": "sess-done", "project": "alpha", "status": "completed"}}, "", "", false},
		{"missing_scope", []any{map[string]any{"id": "sess-unknown", "status": "active"}}, "", "", false},
		{"response_drift", []any{map[string]any{"id": "sess-a", "project": "alpha", "status": "active"}}, "", "beta", false},
		{"unique_positive", []any{map[string]any{"id": "sess-a", "project": "alpha", "status": "active"}}, "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateResumeIdentityTest(t)
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/agents/sessions" {
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "sessions": tc.rows})
					return
				}
				if !strings.HasSuffix(r.URL.Path, "/context-package") {
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				parts := strings.Split(r.URL.Path, "/")
				id := parts[len(parts)-2]
				row := map[string]any{}
				for _, raw := range tc.rows {
					if asMap(raw)["id"] == id {
						row = asMap(raw)
					}
				}
				project := row["project"]
				if tc.responseProject != "" {
					project = tc.responseProject
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "session_id": id, "project": project, "agent_id": row["agent_id"], "session": row})
			}))
			defer gateway.Close()
			var out, errOut bytes.Buffer
			c := newCLI(&out, &errOut)
			c.baseURL = gateway.URL
			args := []string{"--project", "alpha", "--raw"}
			if tc.agentID != "" {
				args = append(args, "--agent-id", tc.agentID)
			}
			err := c.cmdResume(args)
			if (err == nil) != tc.wantOK {
				t.Fatalf("accepted=%v want=%v err=%v stdout=%s", err == nil, tc.wantOK, err, out.String())
			}
			if !tc.wantOK {
				if out.Len() != 0 {
					t.Fatal("unproven resume leaked a package before refusal")
				}
				if _, err := os.Stat(sessionStatePath("alpha")); !os.IsNotExist(err) {
					t.Fatalf("refused resume changed cached identity: %v", err)
				}
			}
		})
	}
}
