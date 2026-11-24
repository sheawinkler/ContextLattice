package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const namedFixturePin = `{"project":"alpha","topic_path":"release/review","decision_id":"approval","scope":"preview","agent_id":"owner"}`

func namedFixtureReceipt() map[string]any {
	return map[string]any{"ok": true, "schema_id": namedDecisionSchema, "project": "alpha", "topic_path": "release/review",
		"session_id": "s-fresh", "agent_id": "owner", "decision_id": "approval", "scope": "preview", "version": 3, "as_of": time.Now().UTC().Format(time.RFC3339Nano),
		"decision": map[string]any{"state": "active", "current": map[string]any{"version": 2, "value": "Ask before publishing."}}}
}

func namedFixtureEnv(t *testing.T, url string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "CONTEXTLATTICE_") || strings.HasPrefix(key, "GO_V4_") {
			t.Setenv(key, "")
		}
	}
	for _, k := range []string{"CONTEXTLATTICE_GLOBAL_HOME", "CONTEXTLATTICE_CONFIG_HOME"} {
		t.Setenv(k, t.TempDir())
	}
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_URL", url)
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "fixture-only")
	t.Setenv("CONTEXTLATTICE_AGENT_ID", "owner")
	t.Setenv("CONTEXTLATTICE_SESSION_ID", "s-fresh")
	t.Setenv(namedDecisionEnv, namedFixturePin)
}

func TestNamedDecisionFrozenScopeHoldouts(t *testing.T) {
	for _, tc := range []struct {
		name, project, topic, scope, pin string
		match                            bool
	}{
		{"unbound_actor", "alpha", "release/review", "preview", strings.Replace(namedFixturePin, `,"agent_id":"owner"`, "", 1), false},
		{"other_actor", "alpha", "release/review", "preview", strings.Replace(namedFixturePin, "owner", "another", 1), false},
		{"exact", "alpha", "release/review", "preview", namedFixturePin, true},
		{"no_pin", "alpha", "release/review", "preview", "", false},
		{"missing_topic", "alpha", "", "preview", namedFixturePin, false},
		{"ancestor", "alpha", "release", "preview", namedFixturePin, false},
		{"descendant", "alpha", "release/review/sub", "preview", namedFixturePin, false},
		{"project", "beta", "release/review", "preview", namedFixturePin, false},
		{"case", "Alpha", "release/review", "preview", namedFixturePin, false},
		{"missing_scope", "alpha", "release/review", "", namedFixturePin, false},
		{"different_scope", "alpha", "release/review", "preview/mobile", namedFixturePin, false},
		{"unknown_pointer_field", "alpha", "release/review", "preview", strings.Replace(namedFixturePin, "{", `{"url":"https://invalid.example",`, 1), false},
		{"duplicate", "alpha", "release/review", "preview", strings.Replace(namedFixturePin, "{", `{"project":"beta",`, 1), false},
		{"oversize", "alpha", "release/review", "preview", strings.Repeat(" ", 1024) + namedFixturePin, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(namedDecisionEnv, tc.pin)
			p := parsedArgs{values: map[string]string{"project": tc.project, "topic_path": tc.topic, "agent_id": "owner", "decision_scope": tc.scope}}
			if got := selectedNamedDecision(p, tc.project) != nil; got != tc.match {
				t.Fatalf("match=%t", got)
			}
		})
	}
}

func TestNamedDecisionFreshReadsAndBaseline(t *testing.T) {
	var reads atomic.Int32
	g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agents/sessions/s-fresh/rollup":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "rollup": map[string]any{}})

		case "/v1/agents/sessions/s-fresh":
			namedSession(t, w, r, "")
		case "/memory/synthesis-pack/v2":
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["topic_path"] != "release/review" {
				t.Error("host topic not scoped on synthesis request")
			}
			_ = json.NewEncoder(w).Encode(testAgentPacketResponse("s-fresh", "", map[string]any{"topic_path": "release/review"}))
		case "/v1/agents/sessions/s-fresh/context-package":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "project": "alpha", "session_id": "s-fresh"})
		case "/memory/claims/query":
			n := reads.Add(1)
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			if r.Method != "POST" || r.Header.Get("X-Api-Key") != "fixture-only" || !reflect.DeepEqual(in, map[string]any{
				"session_id": "s-fresh", "agent_id": "owner", "project": "alpha", "topic_path": "release/review", "decision_context": map[string]any{"id": "approval", "scope": "preview"},
			}) {
				t.Error("claim owner request differed from baseline")
			}
			rec := namedFixtureReceipt()
			if n == 3 {
				rec["version"] = 4
				rec["decision"] = map[string]any{"state": "active", "current": map[string]any{"version": 4, "value": "Use the new review."}}
			}
			if n == 4 {
				w.WriteHeader(403)
				io.WriteString(w, "withdrawn secret")
				return
			}
			_ = json.NewEncoder(w).Encode(rec)
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer g.Close()
	namedFixtureEnv(t, g.URL)
	run := func(args ...string) map[string]any {
		t.Helper()
		var out bytes.Buffer
		c := newCLI(&out, io.Discard)
		if err := c.run(append([]string{"contextlattice"}, args...)); err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := json.Unmarshal(out.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	const topic = "release/review"
	baseline := run("claim-query", "--session-id", "s-fresh", "--agent-id", "owner", "--project", "alpha", "--topic-path", topic, "--decision-id", "approval", "--scope", "preview")
	pack := run("context", "review build", "--project", "alpha", "--topic-path", topic, "--scope", "preview", "--no-auto-session")
	d := asMap(pack["named_decision"])
	if d["recorded_value"] != asMap(asMap(baseline["decision"])["current"])["value"] || d["status"] != "needs_source_check" ||
		d["current_version"] != float64(2) || d["receipt_version"] != float64(3) || asMap(pack["context"])["schema_id"] != agentPacketContractID {
		t.Fatalf("invalid scoped context: %v", d)
	}
	resume := []string{"resume", "--session-id", "s-fresh", "--project", "alpha", "--topic-path", topic, "--scope", "preview"}
	d = asMap(run(resume...)["named_decision"])
	if d["recorded_value"] != "Use the new review." || d["current_version"] != float64(4) {
		t.Fatal("did not recheck current version")
	}
	d = asMap(run(resume...)["named_decision"])
	if d["status"] != "unavailable" || d["recorded_value"] != nil {
		t.Fatal("reused revoked content")
	}
	if state, err := os.ReadFile(sessionStatePath("alpha")); err == nil && (bytes.Contains(state, []byte("new review")) || bytes.Contains(state, []byte("named_decision"))) {
		t.Fatal("decision cached")
	}
	for _, args := range [][]string{
		{"resume", "--session-id", "s-fresh", "--project", "alpha"},
		{"resume", "--session-id", "s-fresh", "--project", "alpha", "--topic-path", topic},
		{"resume", "--session-id", "s-fresh", "--project", "alpha", "--topic-path", topic, "--scope", "other"},
	} {
		if out := run(args...); out["schema_id"] == "cli_scoped_recall.v1" {
			t.Fatal("out-of-scope wrapper")
		}
	}
	if reads.Load() != 4 {
		t.Fatalf("owner reads=%d", reads.Load())
	}
}

func TestNamedDecisionHostSkillAndWarmPacket(t *testing.T) {
	var reads atomic.Int32
	var warming atomic.Bool
	g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agents/sessions/s-fresh/rollup":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "rollup": map[string]any{}})

		case "/v1/agents/sessions/s-fresh":
			namedSession(t, w, r, "")
		case "/memory/synthesis-pack/v2":
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			extras := map[string]any{"topic_path": firstString(in["topic_path"])}
			if warming.Load() {
				extras["continuation"] = map[string]any{"status": "running", "result_state": "warming", "token": "fixture", "pending_sources": []any{"fixture"}}
			}
			_ = json.NewEncoder(w).Encode(testAgentPacketResponse("s-fresh", "", extras))
		case "/memory/claims/query":
			reads.Add(1)
			r := namedFixtureReceipt()
			r["scope"] = ""
			_ = json.NewEncoder(w).Encode(r)
		default:
			t.Error("unexpected route")
			w.WriteHeader(404)
		}
	}))
	defer g.Close()
	namedFixtureEnv(t, g.URL)
	t.Setenv(namedDecisionEnv, strings.Replace(namedFixturePin, "preview", "", 1))
	run := func() map[string]any {
		var out bytes.Buffer
		c := newCLI(&out, io.Discard)
		if err := c.run([]string{"contextlattice", "context", "release/review", "--project", "alpha", "--no-auto-session"}); err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if json.Unmarshal(out.Bytes(), &v) != nil {
			t.Fatal("bad output")
		}
		return v
	}
	if run()["schema_id"] != agentPacketContractID || reads.Load() != 0 {
		t.Fatal("inferred topic from query")
	}
	t.Setenv("CONTEXTLATTICE_ACTIVE_TOPIC_PATH", "release/review")
	if asMap(run()["named_decision"])["status"] != "needs_source_check" || reads.Load() != 1 {
		t.Fatal("missing host-selected decision")
	}
	warming.Store(true)
	d := asMap(run()["named_decision"])
	if d["status"] != "warming_or_unavailable" || d["recorded_value"] != nil || reads.Load() != 2 {
		t.Fatal("warming must recheck but not carry text")
	}
}

func TestNamedDecisionAuthorityHoldouts(t *testing.T) {
	for _, kind := range []string{"old_gateway", "contested", "pending", "expired", "retracted", "foreign_project", "foreign_topic", "wrong_id", "wrong_scope", "invalid_version", "future_version", "stale_time", "details", "oversize", "wrong_key", "redirect", "extra_state", "session_actor", "session_project", "session_status", "stale_session", "different_native", "session_echo", "actor_echo"} {
		t.Run(kind, func(t *testing.T) {
			var hits atomic.Int32
			sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("redirect followed") }))
			defer sink.Close()
			g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if r.Method == "GET" {
					namedSession(t, w, r, kind)
					return
				}
				if strings.HasPrefix(kind, "session_") && kind != "session_echo" || kind == "stale_session" || kind == "different_native" {
					t.Error("owner POST after invalid session")
				}
				v := namedFixtureReceipt()
				d := asMap(v["decision"])
				switch kind {
				case "actor_echo":
					v["agent_id"] = "other"
				case "session_echo":
					v["session_id"] = "another"
				case "old_gateway":
					v = map[string]any{"ok": true, "claims": []any{"generic result"}}
				case "contested":
					d["state"] = "unresolved_contradiction"
				case "pending", "expired", "retracted":
					d["state"] = kind
				case "foreign_project":
					v["project"] = "beta"
				case "foreign_topic":
					v["topic_path"] = "release"
				case "wrong_id":
					v["decision_id"] = "other"
				case "wrong_scope":
					v["scope"] = ""
				case "invalid_version":
					v["version"] = "3"
				case "future_version":
					asMap(d["current"])["version"] = 4
				case "stale_time":
					v["as_of"] = "2000-01-01T00:00:00Z"
				case "details":
					d["details"] = []any{map[string]any{"version": 3, "value": "except when blocked"}}
				case "oversize":
					asMap(d["current"])["value"] = strings.Repeat("x", 20000)
				case "redirect":
					http.Redirect(w, r, sink.URL, 307)
					return
				case "extra_state":
					v["continuation_async"] = map[string]any{"status": "running"}
				}
				_ = json.NewEncoder(w).Encode(v)
			}))
			defer g.Close()
			namedFixtureEnv(t, g.URL)
			c := newCLI(io.Discard, io.Discard)
			if kind == "wrong_key" {
				c.apiKey = "stale"
			}
			out := map[string]any{"status": "unavailable"}
			c.readNamedDecision(&namedDecisionPin{"alpha", "release/review", "approval", "preview", "owner", "host"}, "s-fresh", out)
			if out["recorded_value"] != nil {
				t.Fatal("unqualified value emitted")
			}
			want := "unavailable"
			switch kind {
			case "contested":
				want = "unresolved_contradiction"
			case "pending", "retracted", "expired":
				want = kind
			case "details":
				want = "needs_source_check"
			}
			if out["status"] != want || kind == "wrong_key" && hits.Load() != 0 {
				t.Fatalf("status %v reads %d", out["status"], hits.Load())
			}
		})
	}
}

func TestNamedDecisionCredentialRotation(t *testing.T) {
	var key atomic.Value
	key.Store("first-key")
	g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != key.Load() {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.Method == "GET" {
			namedSession(t, w, r, "")
			return
		}
		_ = json.NewEncoder(w).Encode(namedFixtureReceipt())
	}))
	defer g.Close()
	namedFixtureEnv(t, g.URL)
	lookup := func(key string) map[string]any {
		t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", key)
		c := newCLI(io.Discard, io.Discard)
		out := map[string]any{"status": "unavailable"}
		c.readNamedDecision(&namedDecisionPin{"alpha", "release/review", "approval", "preview", "owner", "host"}, "s-fresh", out)
		return out
	}
	if lookup("first-key")["current_version"] != 2 {
		t.Fatal("initial authority")
	}
	key.Store("next-key")
	if out := lookup("first-key"); out["status"] != "unavailable" || out["recorded_value"] != nil {
		t.Fatal("revoked credential retained authority")
	}
	if lookup("next-key")["status"] != "needs_source_check" {
		t.Fatal("rotated valid credential")
	}
}

func namedSession(t *testing.T, w http.ResponseWriter, r *http.Request, kind string) {
	t.Helper()
	if r.Method != "GET" || r.URL.Path != "/v1/agents/sessions/s-fresh" {
		t.Error("unexpected session lookup")
	}
	row := map[string]any{"id": "s-fresh", "project": "alpha", "agent_id": "owner", "status": "active", "native_session_id": "host"}
	switch kind {
	case "session_actor":
		row["agent_id"] = "Owner"
	case "session_project":
		row["project"] = "Alpha"
	case "session_status":
		row["status"] = "paused"
	case "stale_session":
		row["stale"] = true
	case "different_native":
		row["native_session_id"] = "other"
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "session": row})
}
