package main

import (
	"path/filepath"
	"testing"
)

func TestExecutionIdentityReuseAndUntrustedLabel(t *testing.T) {
	t.Setenv("GO_AGENT_SESSIONS_PATH", filepath.Join(t.TempDir(), "sessions.json"))
	s, err := newAgentSessionStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	p := map[string]any{"project": "alpha", "agent": "pi", "agent_id": "owner", "native_session_id": "local-session", "ensure": true, "metadata": map[string]any{}}
	put := func(model string) {
		anyMap(p["metadata"])["caller_attribution"] = map[string]any{"source": "caller_reported", "model_known": true, "model": model, "harness": "pi", "reasoning_effort": "high", "execution_label": "spoofed", "admin": true}
	}
	put("lab-1.0-ash")
	row, _, err := s.startOrReuse(p)
	if err != nil {
		t.Fatal(err)
	}
	id := row["id"]
	started := row["started_at"]
	for i, model := range []string{"lab-2.0-birch", "lab-3.0-pine"} {
		if i == 1 {
			p["session_id"] = id
		}
		put(model)
		r, reused, err := s.startOrReuse(p)
		if err != nil || !reused || r["id"] != id || r["agent_id"] != "owner" || r["started_at"] != started {
			t.Fatalf("changed session binding: %v %v", reused, err)
		}
		facts := anyMap(r["execution_identity"])
		if facts["model"] != model || facts["execution_label"] == "spoofed" || facts["admin"] != nil || facts["execution_status"] != "complete" {
			t.Fatalf("bad normalized observation: %#v", facts)
		}
	}
	reopened, err := newAgentSessionStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	r, _, ok := reopened.get(anyToString(id))
	if !ok || anyMap(r["execution_identity"])["execution_label"] != "pi-lab-pine-3.0-high" {
		t.Fatal("lost reuse observation on restart")
	}
}
