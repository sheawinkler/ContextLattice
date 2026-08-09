package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRecallResponseCLIAliasesAndPackDelegationPreserveSemantics(t *testing.T) {
	var requests atomic.Int32
	var captured map[string]any
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/memory/recall/response" {
			t.Fatalf("unexpected recall route %s", r.URL.Path)
		}
		requests.Add(1)
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode recall request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(failureRecallResponse())
	}))
	defer gateway.Close()

	commands := [][]string{
		{"contextlattice_recall_response", "same task", "--project", "alpha", "--agent-id", "agent-a", "--task-id", "task-a", "--no-auto-session", "--raw"},
		{"contextlattice", "recall-response", "same task", "--project", "alpha", "--agent-id", "agent-a", "--task-id", "task-a", "--no-auto-session", "--raw"},
		{"contextlattice-agent-tools", "recall-response", "same task", "--project", "alpha", "--agent-id", "agent-a", "--task-id", "task-a", "--no-auto-session", "--raw"},
		{"contextlattice_pack", "same task", "--project", "alpha", "--agent-id", "agent-a", "--task-id", "task-a", "--response", "--no-auto-session", "--raw"},
	}
	var want map[string]any
	for _, command := range commands {
		var stdout bytes.Buffer
		c := newCLI(&stdout, ioDiscard{})
		c.baseURL = gateway.URL
		if err := c.run(command); err != nil {
			t.Fatalf("run %v: %v output=%s", command, err, stdout.String())
		}
		var got map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
			t.Fatalf("decode %v output: %v", command, err)
		}
		if firstString(got["schema_id"]) != "recall_response.v1" {
			t.Fatalf("%v returned unexpected schema: %#v", command, got)
		}
		if want == nil {
			want = got
		} else if firstString(want["response_id"]) != firstString(got["response_id"]) || firstString(want["response_digest"]) != firstString(got["response_digest"]) {
			t.Fatalf("%v changed response identity: want=%v got=%v", command, want, got)
		}
	}
	if requests.Load() != int32(len(commands)) {
		t.Fatalf("request count=%d want=%d", requests.Load(), len(commands))
	}
	for key, expected := range map[string]any{"query": "same task", "project": "alpha", "agent_id": "agent-a", "task_id": "task-a", "session_id": nil} {
		if captured[key] != expected {
			t.Fatalf("payload[%s]=%#v want=%#v", key, captured[key], expected)
		}
	}
	if captured["include_retrieval_debug"] != false || captured["native_cli_implementation"] != true {
		t.Fatalf("recall payload crossed the pack boundary: %#v", captured)
	}
}

func TestRecallResponseCLIRefusesRedirectWithoutCredentialResend(t *testing.T) {
	var sourceCalls atomic.Int32
	var destinationCalls atomic.Int32
	var destinationAPIKey string
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationCalls.Add(1)
		destinationAPIKey = r.Header.Get("x-api-key")
		_ = json.NewEncoder(w).Encode(failureRecallResponse())
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceCalls.Add(1)
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = source.URL
	c.apiKey = "must-not-cross-redirect"
	err := c.run([]string{"contextlattice_recall_response", "redirect task", "--no-auto-session", "--retries", "0", "--raw"})
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("redirect was not refused: %v", err)
	}
	if sourceCalls.Load() != 1 || destinationCalls.Load() != 0 || destinationAPIKey != "" {
		t.Fatalf("redirect transport followed or resent credentials: source=%d destination=%d key=%q", sourceCalls.Load(), destinationCalls.Load(), destinationAPIKey)
	}
	var fallback map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &fallback); err != nil {
		t.Fatalf("redirect fallback was not JSON: %v output=%s", err, stdout.String())
	}
	if firstString(fallback["schema_id"]) != "recall_response.v1" {
		t.Fatalf("redirect fallback crossed the response boundary: %#v", fallback)
	}
}

func TestRecallResponseCLIRejectsOversizedMalformedAndStaleBodies(t *testing.T) {
	tests := []struct {
		name string
		body func() []byte
	}{
		{name: "oversized", body: func() []byte { return []byte(strings.Repeat("x", recallResponseCLIContractMaxJSONBytes+1)) }},
		{name: "malformed", body: func() []byte { return []byte("{not-json") }},
		{name: "unexpected_field", body: func() []byte {
			response := failureRecallResponse()
			response["unexpected"] = true
			encoded, _ := json.Marshal(response)
			return encoded
		}},
		{name: "wrong_type", body: func() []byte {
			response := failureRecallResponse()
			response["evidence"] = "unexpected scalar"
			response["response_id"] = cliRecallResponseID(response)
			response["response_digest"] = cliRecallResponseDigest(response)
			encoded, _ := json.Marshal(response)
			return encoded
		}},
		{name: "stale_identity", body: func() []byte {
			response := failureRecallResponse()
			asMap(response["answer"])["summary"] = "changed after identity stamp"
			encoded, _ := json.Marshal(response)
			return encoded
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(tc.body())
			}))
			defer gateway.Close()
			var stdout bytes.Buffer
			c := newCLI(&stdout, ioDiscard{})
			c.baseURL = gateway.URL
			if err := c.run([]string{"contextlattice_recall_response", "bounded task", "--no-auto-session", "--retries", "0", "--soft", "--raw"}); err != nil {
				t.Fatalf("soft rejection returned error: %v", err)
			}
			var fallback map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &fallback); err != nil {
				t.Fatalf("rejection fallback was not JSON: %v output=%s", err, stdout.String())
			}
			if firstString(fallback["schema_id"]) != "recall_response.v1" || firstString(asMap(fallback["classification"])["posture"]) != "abstain" {
				t.Fatalf("rejection was not bounded abstention: %#v", fallback)
			}
		})
	}
}

func TestCompactRecallResponseRejectsCanonicalForbiddenNestedFieldsAndPrivatePaths(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "camel_case_secret_alias", mutate: func(response map[string]any) {
			asMap(response["answer"])["accessToken"] = "must-not-cross"
		}},
		{name: "mixed_separator_secret_alias", mutate: func(response map[string]any) {
			asMap(asMap(response["answer"])["proof_spine"])["private.Key"] = "must-not-cross"
		}},
		{name: "nested_canonical_forbidden_key", mutate: func(response map[string]any) {
			asMap(asMap(response["answer"])["progressive_disclosure"])["api-key"] = "must-not-cross"
		}},
		{name: "unknown_nested_closed_field", mutate: func(response map[string]any) {
			asMap(asMap(response["classification"])["facets"])["future_field"] = "must-not-cross"
		}},
		{name: "private_absolute_path_value", mutate: func(response map[string]any) {
			asMap(response["answer"])["summary"] = "see /private/contextlattice/secret.md"
		}},
		{name: "private_windows_path_value", mutate: func(response map[string]any) {
			asMap(response["answer"])["summary"] = `see C:\Users\sheawinkler\private\secret.md`
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			response := failureRecallResponse()
			tc.mutate(response)
			if _, err := compactRecallResponse(response); err == nil {
				t.Fatalf("adversarial response was accepted: %#v", response)
			}
		})
	}
}

func TestCompactRecallResponseAcceptsValidClosedControlProjection(t *testing.T) {
	encoded, err := json.Marshal(failureRecallResponse())
	if err != nil {
		t.Fatalf("marshal valid response: %v", err)
	}
	response := map[string]any{}
	if err := json.Unmarshal(encoded, &response); err != nil {
		t.Fatalf("decode valid response: %v", err)
	}
	if _, err := compactRecallResponse(response); err != nil {
		t.Fatalf("valid recall response was rejected: %v", err)
	}
}
