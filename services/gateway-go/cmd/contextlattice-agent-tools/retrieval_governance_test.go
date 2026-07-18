package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

type retrievalGovernanceRequest struct {
	method string
	path   string
	query  url.Values
	body   map[string]any
}

func runRetrievalGovernanceCLI(t *testing.T, args []string) (retrievalGovernanceRequest, map[string]any) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	captured := retrievalGovernanceRequest{}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.query = r.URL.Query()
		captured.body = map[string]any{}
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&captured.body); err != nil {
				t.Fatalf("decode retrieval governance request: %v", err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "schema_id": retrievalGovernanceContractID,
		})
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, io.Discard)
	c.baseURL = gateway.URL
	if err := c.run(args); err != nil {
		t.Fatalf("run retrieval governance CLI: %v", err)
	}
	output := map[string]any{}
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode retrieval governance output: %v output=%s", err, stdout.String())
	}
	return captured, output
}

func TestRetrievalGovernanceCLIRoutesAndShapes(t *testing.T) {
	tests := []struct {
		feature string
		path    string
	}{
		{"receipts", "/memory/retrieval/receipts/governance"},
		{"causal-bridges", "/memory/causal-bridges/governance"},
		{"counterfactual", "/memory/retrieval/ablation/operations"},
		{"reputation", "/memory/evidence-reputation/activation"},
		{"regressions", "/memory/recall/regressions/operations"},
		{"defense", "/memory/trust/defense/operations"},
	}
	for _, test := range tests {
		t.Run(test.feature+"/status", func(t *testing.T) {
			captured, output := runRetrievalGovernanceCLI(t, []string{
				"contextlattice", "retrieval-governance", "status",
				"--feature", test.feature, "--project", "alpha", "--raw",
			})
			if captured.method != http.MethodGet || captured.path != test.path {
				t.Fatalf("route mismatch: method=%s path=%s", captured.method, captured.path)
			}
			if captured.query.Get("project") != "alpha" || len(captured.query) != 1 || len(captured.body) != 0 {
				t.Fatalf("status request was not project-only GET: query=%#v body=%#v", captured.query, captured.body)
			}
			if output["schema_id"] != retrievalGovernanceContractID {
				t.Fatalf("unexpected output contract: %#v", output)
			}
		})

		t.Run(test.feature+"/configure", func(t *testing.T) {
			captured, _ := runRetrievalGovernanceCLI(t, []string{
				"contextlattice_retrieval_governance", "configure",
				"--feature", test.feature, "--project", "alpha",
				"--reason", "activate bounded paid governance",
				"--retention-days", "21", "--schedule", "weekly",
				"--incident-review", "required", "--raw",
			})
			if captured.method != http.MethodPost || captured.path != test.path || len(captured.query) != 0 {
				t.Fatalf("route mismatch: method=%s path=%s query=%#v", captured.method, captured.path, captured.query)
			}
			if captured.body["operation"] != "configure" || captured.body["project"] != "alpha" || captured.body["reason"] != "activate bounded paid governance" {
				t.Fatalf("configure envelope mismatch: %#v", captured.body)
			}
			policy, ok := captured.body["policy"].(map[string]any)
			if !ok || int(policy["retention_days"].(float64)) != 21 || policy["schedule"] != "weekly" || policy["incident_review"] != "required" {
				t.Fatalf("configure policy mismatch: %#v", captured.body)
			}
			for _, forbidden := range []string{"workspace", "workspace_id", "plan", "key", "token", "candidate", "raw_candidate", "defense_bypass"} {
				if _, exists := captured.body[forbidden]; exists {
					t.Fatalf("forbidden field %q reached request: %#v", forbidden, captured.body)
				}
			}
		})
	}
}

func TestRetrievalGovernanceCLIMutationOperations(t *testing.T) {
	for _, operation := range []string{"evaluate", "deactivate"} {
		t.Run(operation, func(t *testing.T) {
			captured, _ := runRetrievalGovernanceCLI(t, []string{
				"contextlattice", "retrieval-governance", operation,
				"--feature", "receipts", "--project", "alpha",
				"--reason", "bounded operator request", "--raw",
			})
			if captured.method != http.MethodPost || captured.path != "/memory/retrieval/receipts/governance" {
				t.Fatalf("%s route mismatch: %#v", operation, captured)
			}
			if captured.body["operation"] != operation || captured.body["project"] != "alpha" || captured.body["reason"] != "bounded operator request" {
				t.Fatalf("%s payload mismatch: %#v", operation, captured.body)
			}
			if _, exists := captured.body["policy"]; exists {
				t.Fatalf("%s unexpectedly sent policy: %#v", operation, captured.body)
			}
		})
	}
}

func TestRetrievalGovernanceCLILocalValidationBeforeNetwork(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var calls atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer gateway.Close()

	tests := []struct {
		name string
		args []string
	}{
		{"missing feature", []string{"contextlattice", "retrieval-governance", "status", "--project", "alpha"}},
		{"unknown feature", []string{"contextlattice", "retrieval-governance", "status", "--feature", "memory", "--project", "alpha"}},
		{"missing project", []string{"contextlattice", "retrieval-governance", "status", "--feature", "receipts"}},
		{"unknown operation", []string{"contextlattice", "retrieval-governance", "destroy", "--feature", "receipts", "--project", "alpha"}},
		{"missing reason", []string{"contextlattice", "retrieval-governance", "configure", "--feature", "receipts", "--project", "alpha"}},
		{"multiline reason", []string{"contextlattice", "retrieval-governance", "evaluate", "--feature", "receipts", "--project", "alpha", "--reason", "line one\nline two"}},
		{"long reason", []string{"contextlattice", "retrieval-governance", "deactivate", "--feature", "receipts", "--project", "alpha", "--reason", strings.Repeat("r", 241)}},
		{"retention zero", []string{"contextlattice", "retrieval-governance", "configure", "--feature", "receipts", "--project", "alpha", "--reason", "invalid policy", "--retention-days", "0"}},
		{"retention too high", []string{"contextlattice", "retrieval-governance", "configure", "--feature", "receipts", "--project", "alpha", "--reason", "invalid policy", "--retention-days", "366"}},
		{"retention not integer", []string{"contextlattice", "retrieval-governance", "configure", "--feature", "receipts", "--project", "alpha", "--reason", "invalid policy", "--retention-days", "daily"}},
		{"unknown schedule", []string{"contextlattice", "retrieval-governance", "configure", "--feature", "receipts", "--project", "alpha", "--reason", "invalid policy", "--schedule", "hourly"}},
		{"unknown incident review", []string{"contextlattice", "retrieval-governance", "configure", "--feature", "receipts", "--project", "alpha", "--reason", "invalid policy", "--incident-review", "optional"}},
		{"policy outside configure", []string{"contextlattice", "retrieval-governance", "evaluate", "--feature", "receipts", "--project", "alpha", "--reason", "invalid policy", "--schedule", "daily"}},
		{"reason on status", []string{"contextlattice", "retrieval-governance", "status", "--feature", "receipts", "--project", "alpha", "--reason", "not a mutation"}},
		{"workspace override", []string{"contextlattice", "retrieval-governance", "status", "--feature", "receipts", "--project", "alpha", "--workspace", "other"}},
		{"plan override", []string{"contextlattice", "retrieval-governance", "status", "--feature", "receipts", "--project", "alpha", "--plan", "enterprise"}},
		{"key override", []string{"contextlattice", "retrieval-governance", "status", "--feature", "receipts", "--project", "alpha", "--key", "secret"}},
		{"token override", []string{"contextlattice", "retrieval-governance", "status", "--feature", "receipts", "--project", "alpha", "--token", "secret"}},
		{"raw candidate", []string{"contextlattice", "retrieval-governance", "status", "--feature", "receipts", "--project", "alpha", "--raw-candidate", "text"}},
		{"defense bypass", []string{"contextlattice", "retrieval-governance", "status", "--feature", "defense", "--project", "alpha", "--defense-bypass"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := calls.Load()
			c := newCLI(io.Discard, io.Discard)
			c.baseURL = gateway.URL
			if err := c.run(test.args); err == nil {
				t.Fatalf("expected local validation failure")
			}
			if after := calls.Load(); after != before {
				t.Fatalf("validation reached network: before=%d after=%d", before, after)
			}
		})
	}
}

func TestRetrievalGovernanceCLIInventoryAndHelp(t *testing.T) {
	if nativeToolNames["contextlattice_retrieval_governance"] != "retrieval-governance" {
		t.Fatalf("basename wrapper is not registered: %#v", nativeToolNames)
	}
	if len(retrievalGovernanceRoutes) != 6 || retrievalGovernanceContractID != "frontier_t4_retrieval_governance.v1" {
		t.Fatalf("retrieval governance metadata is incomplete")
	}
	var stdout bytes.Buffer
	c := newCLI(&stdout, io.Discard)
	if err := c.run([]string{"contextlattice", "retrieval-governance", "--help"}); err != nil {
		t.Fatalf("render retrieval governance help: %v", err)
	}
	for _, value := range []string{"status|configure|evaluate|deactivate", "receipts|causal-bridges|counterfactual|reputation|regressions|defense", "--project"} {
		if !strings.Contains(stdout.String(), value) {
			t.Fatalf("help omitted %q: %s", value, stdout.String())
		}
	}
}
