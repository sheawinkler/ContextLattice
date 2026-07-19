package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFrontierT5PolicyLabCLIContracts(t *testing.T) {
	t.Setenv("CONTEXTLATTICE_CONFIG_HOME", t.TempDir())
	payloadFile := filepath.Join(t.TempDir(), "policy-lab.json")
	if err := os.WriteFile(payloadFile, []byte(`{"project":"file-project","candidate_id":"candidate-file","claim_ids":["claim-file"],"limit":99}`), 0o600); err != nil {
		t.Fatal(err)
	}

	type request struct {
		method  string
		payload map[string]any
	}
	requests := map[string]request{}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/telemetry/policy-laboratory" {
			if r.Method != http.MethodGet {
				t.Fatalf("status method=%s", r.Method)
			}
			requests[r.URL.Path] = request{method: r.Method}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "status": "ok"})
			return
		}
		if r.Method != http.MethodPost {
			t.Fatalf("%s method=%s", r.URL.Path, r.Method)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode %s request: %v", r.URL.Path, err)
		}
		requests[r.URL.Path] = request{method: r.Method, payload: payload}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "path": r.URL.Path})
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	cases := []struct {
		name string
		args []string
		path string
	}{
		{
			name: "simulate alias and payload overrides",
			args: []string{"contextlattice_policy_lab", "simulate", "--payload-file", payloadFile, "--project", "cli-project", "--operation", "replay", "--claim-ids", "claim-a,claim-b", "--limit", "3", "--approved=false", "--raw"},
			path: "/memory/policy/simulate",
		},
		{
			name: "card command dispatch",
			args: []string{"contextlattice", "policy-lab", "card", "--task-class", "coding", "--retrieval-intent", "focused", "--threshold", "0.3", "--raw"},
			path: "/memory/policy/card",
		},
		{name: "promotion", args: []string{"contextlattice_policy_lab", "promotion", "--candidate-id", "candidate-cli", "--raw"}, path: "/memory/policy/promotion"},
		{name: "retirement", args: []string{"contextlattice_policy_lab", "retirement", "--file", "memory.json", "--stale-after-days", "14", "--legal-hold", "--raw"}, path: "/memory/lifecycle/retirement"},
		{name: "contradiction", args: []string{"contextlattice_policy_lab", "contradiction", "--claim-ids", "claim-a,claim-b", "--winning-claim-id", "claim-a", "--resolution-id", "resolution-1", "--raw"}, path: "/memory/contradictions/resolve"},
		{name: "temperature", args: []string{"contextlattice_policy_lab", "temperature", "--file", "memory.json", "--tier", "warm", "--reason", "bounded review", "--expected-content-hash", "sha256:test", "--receipt-id", "receipt-1", "--raw"}, path: "/memory/storage/temperature"},
		{name: "status", args: []string{"contextlattice_policy_lab", "status", "--raw"}, path: "/telemetry/policy-laboratory"},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			stdout.Reset()
			if err := c.run(test.args); err != nil {
				t.Fatalf("run policy-lab: %v", err)
			}
			captured, ok := requests[test.path]
			if !ok {
				t.Fatalf("no request captured for %s", test.path)
			}
			if test.path == "/telemetry/policy-laboratory" {
				if captured.method != http.MethodGet {
					t.Fatalf("status request=%#v", captured)
				}
				return
			}
			if captured.method != http.MethodPost {
				t.Fatalf("request=%#v", captured)
			}
		})
	}

	simulate := requests["/memory/policy/simulate"].payload
	if simulate["project"] != "cli-project" || simulate["operation"] != "replay" || simulate["candidate_id"] != "candidate-file" || int(simulate["limit"].(float64)) != 3 {
		t.Fatalf("explicit flags did not override or preserve payload fields: %#v", simulate)
	}
	if simulate["approved"] != false {
		t.Fatalf("explicit false approval was not preserved safely: %#v", simulate)
	}
	claimIDs, ok := simulate["claim_ids"].([]any)
	if !ok || len(claimIDs) != 2 || claimIDs[0] != "claim-a" || claimIDs[1] != "claim-b" {
		t.Fatalf("claim ids=%#v", simulate["claim_ids"])
	}
	card := requests["/memory/policy/card"].payload
	if card["task_class"] != "coding" || card["retrieval_intent"] != "focused" {
		t.Fatalf("card payload=%#v", card)
	}
	retirement := requests["/memory/lifecycle/retirement"].payload
	if retirement["legal_hold"] != true || int(retirement["stale_after_days"].(float64)) != 14 {
		t.Fatalf("retirement payload=%#v", retirement)
	}
}
