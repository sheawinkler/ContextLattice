package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func assertAdoptionProofScope(t *testing.T, payload map[string]any, preflightState string) {
	t.Helper()
	if payload["schema_id"] != "contextlattice_agent_adoption_proof_matrix.v1" || payload["native_cli"] != true {
		t.Fatalf("legacy envelope changed: %#v", payload)
	}
	if payload["proof_scope"] != "preflight_contract_validation_only" {
		t.Fatalf("unexpected proof scope: %#v", payload)
	}
	want := map[string]any{
		"preflight_contract_validation": preflightState,
		"handoff_persistence":           "not_observed",
		"receiving_agent_retrieval":     "not_observed",
		"host_delivery":                 "not_observed",
		"model_consumption":             "not_observed",
		"independent_task_acceptance":   "not_observed",
	}
	if !reflect.DeepEqual(payload["stages"], want) {
		t.Fatalf("unobserved stages promoted: got=%#v want=%#v", payload["stages"], want)
	}
}

func TestAdoptionProofPreflightScope(t *testing.T) {
	for _, test := range []struct {
		name   string
		args   []string
		agents []string
	}{
		{"selected", []string{"--agents", "codex,pi", "--project", "alpha", "--mode", "deep", "--pretty"}, []string{"codex", "pi"}},
		{"defaults", nil, []string{"codex", "claude-code", "opencode", "hermes-agent", "hermes-ultra", "omp", "mercury-agent", "pi", "droid", "chatgpt-web", "chatgpt-desktop", "claude-web", "claude-desktop"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			var agents []string
			c := newCLI(&output, io.Discard)
			c.baseURL, c.apiKey = "http://gateway.invalid", ""
			c.client = &http.Client{Transport: testRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/agents/preflight" {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				agents = append(agents, firstString(request["agent"]))
				project, mode := "contextlattice", "fast"
				if test.name == "selected" {
					project, mode = "alpha", "deep"
				}
				if request["project"] != project || request["retrieval_mode"] != mode {
					t.Fatalf("request defaults changed: %#v", request)
				}
				// Server claims beyond preflight must not become diagnostic observations.
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true,"session_id":"preflight-session","format_contracts":{"validation":{"status":"passed"}},"stages":{"handoff_persistence":"passed","model_consumption":"passed"}}`))}, nil
			})}
			if err := c.cmdAdoptionProof(test.args); err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal(output.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			assertAdoptionProofScope(t, payload, "passed")
			if payload["ok"] != true || asInt(payload["case_count"]) != len(test.agents) || !reflect.DeepEqual(agents, test.agents) {
				t.Fatalf("legacy matrix/defaults changed: %#v requests=%#v", payload, agents)
			}
			cases := payload["cases"].([]any)
			if len(cases) != len(test.agents) {
				t.Fatalf("unexpected cases: %#v", cases)
			}
			for i, raw := range cases {
				row := asMap(raw)
				if row["agent"] != test.agents[i] || row["ok"] != true || row["session_id"] != "preflight-session" || row["validation"] != "passed" || len(row["findings"].([]any)) != 0 {
					t.Fatalf("legacy case changed: %#v", row)
				}
			}
		})
	}
}

func TestAdoptionProofPreflightFailures(t *testing.T) {
	for _, test := range []struct {
		name   string
		body   string
		status int
		err    error
	}{
		{"rejected", `{"ok":false,"format_contracts":{"validation":{"status":"passed"}}}`, http.StatusOK, nil},
		{"invalid_contract", `{"ok":true,"format_contracts":{"validation":{"status":"failed"}}}`, http.StatusOK, nil},
		{"missing_contract", `{"ok":true}`, http.StatusOK, nil},
		{"http_error", `{"ok":true,"format_contracts":{"validation":{"status":"passed"}}}`, http.StatusServiceUnavailable, nil},
		{"transport_error", "", 0, errors.New("fake transport failure")},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			requests := 0
			c := newCLI(&output, io.Discard)
			c.baseURL, c.apiKey = "http://gateway.invalid", ""
			c.client = &http.Client{Transport: testRoundTripper(func(r *http.Request) (*http.Response, error) {
				requests++
				if r.Method != http.MethodPost || r.URL.Path != "/v1/agents/preflight" {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				if test.err != nil {
					return nil, test.err
				}
				return &http.Response{StatusCode: test.status, Body: io.NopCloser(strings.NewReader(test.body))}, nil
			})}
			if err := c.cmdAdoptionProof([]string{"--agents=codex"}); err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal(output.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			assertAdoptionProofScope(t, payload, "failed")
			if payload["ok"] != false || payload["case_count"] != float64(1) || requests != 1 {
				t.Fatalf("failure turned green: %#v requests=%d", payload, requests)
			}
			cases := payload["cases"].([]any)
			if len(cases) != 1 || asMap(cases[0])["ok"] != false {
				t.Fatalf("failure case changed: %#v", cases)
			}
			if (test.err != nil || test.status != http.StatusOK) && len(asMap(cases[0])["findings"].([]any)) == 0 {
				t.Fatalf("request error finding lost: %#v", cases[0])
			}
		})
	}
}

func TestAdoptionProofRepeatedAgentsLastOptionWins(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		ok   bool
	}{
		{"inline_empty_then_inline_valid", []string{"--agents=", "--agents=codex"}, true},
		{"inline_empty_then_space_valid", []string{"--agents=", "--agents", "codex"}, true},
		{"space_empty_then_inline_valid", []string{"--agents", "", "--agents=codex"}, true},
		{"space_empty_then_space_valid", []string{"--agents", "", "--agents", "codex"}, true},
		{"inline_valid_then_inline_empty", []string{"--agents=codex", "--agents="}, false},
		{"inline_valid_then_space_empty", []string{"--agents=codex", "--agents", ""}, false},
		{"space_valid_then_inline_empty", []string{"--agents", "codex", "--agents="}, false},
		{"space_valid_then_space_empty", []string{"--agents", "codex", "--agents", ""}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			requests := 0
			c := newCLI(&output, io.Discard)
			c.baseURL, c.apiKey = "http://gateway.invalid", ""
			c.client = &http.Client{Transport: testRoundTripper(func(r *http.Request) (*http.Response, error) {
				requests++
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				if r.Method != http.MethodPost || r.URL.Path != "/v1/agents/preflight" || request["agent"] != "codex" {
					t.Fatalf("unexpected preflight: %s %s %#v", r.Method, r.URL.Path, request)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true,"format_contracts":{"validation":{"status":"passed"}}}`))}, nil
			})}
			err := c.cmdAdoptionProof(test.args)
			if (test.ok && err != nil) || (!test.ok && !errors.Is(err, errCLIReportedFailure)) {
				t.Fatalf("unexpected command error: %v", err)
			}
			var payload map[string]any
			if err := json.Unmarshal(output.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			wantCount, state := 0, "not_observed"
			if test.ok {
				wantCount, state = 1, "passed"
			}
			assertAdoptionProofScope(t, payload, state)
			if payload["ok"] != test.ok || asInt(payload["case_count"]) != wantCount || requests != wantCount || len(payload["cases"].([]any)) != wantCount {
				t.Fatalf("last selection not preserved: %#v requests=%d", payload, requests)
			}
			if !test.ok {
				findings := payload["findings"].([]any)
				if len(findings) != 1 || asMap(findings[0])["reason"] != "empty_agent_selection" {
					t.Fatalf("missing empty selection finding: %#v", payload)
				}
			}
		})
	}
}

func TestAdoptionProofRejectsExplicitEmptyAgents(t *testing.T) {
	for _, args := range [][]string{
		{"--agents", ""}, {"--agents="}, {"--agents", " \t"}, {"--agents= , , "},
		{"--agents"}, {"--agents", "--pretty"}, {"--agents=", "codex"},
	} {
		t.Run(strings.Join(args, "/"), func(t *testing.T) {
			var output bytes.Buffer
			c := newCLI(&output, io.Discard)
			c.baseURL, c.apiKey = "http://gateway.invalid", ""
			c.client = &http.Client{Transport: testRoundTripper(func(r *http.Request) (*http.Response, error) {
				t.Fatalf("empty selection made an HTTP request: %s", r.URL.Path)
				return nil, errors.New("unexpected request")
			})}
			if err := c.cmdAdoptionProof(args); !errors.Is(err, errCLIReportedFailure) {
				t.Fatalf("expected structured failure, got %v", err)
			}
			var payload map[string]any
			if err := json.Unmarshal(output.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			assertAdoptionProofScope(t, payload, "not_observed")
			if payload["ok"] != false || payload["case_count"] != float64(0) || len(payload["cases"].([]any)) != 0 {
				t.Fatalf("empty selection turned green: %#v", payload)
			}
			findings := payload["findings"].([]any)
			if len(findings) != 1 || asMap(findings[0])["reason"] != "empty_agent_selection" {
				t.Fatalf("missing machine-readable finding: %#v", payload)
			}
		})
	}
}
