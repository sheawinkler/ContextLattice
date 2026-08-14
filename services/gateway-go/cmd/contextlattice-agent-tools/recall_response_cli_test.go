package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

func TestRecallResponseCLIUsesClientTimeoutResolution(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		extraArgs []string
		expected  float64
	}{
		{name: "repo default", env: "", expected: defaultContextLatticeClientTimeoutSecs},
		{name: "finite environment override", env: "49", expected: 49},
		{name: "explicit timeout wins", env: "49", extraArgs: []string{"--timeout", "7"}, expected: 7},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CONTEXTLATTICE_CLIENT_TIMEOUT_SECS", test.env)
			var deadline time.Time
			c := newCLI(io.Discard, ioDiscard{})
			c.client = &http.Client{Transport: testRoundTripper(func(r *http.Request) (*http.Response, error) {
				var ok bool
				deadline, ok = r.Context().Deadline()
				if !ok {
					return nil, errors.New("retrieval request did not carry a deadline")
				}
				encoded, err := json.Marshal(failureRecallResponse())
				if err != nil {
					return nil, err
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(bytes.NewReader(encoded)),
					Request:    r,
				}, nil
			})}

			var stdout bytes.Buffer
			c.stdout = &stdout
			args := append([]string{"contextlattice_recall_response", "timeout resolution", "--no-auto-session", "--raw"}, test.extraArgs...)
			if err := c.run(args); err != nil {
				t.Fatalf("run recall response: %v output=%s", err, stdout.String())
			}
			remaining := deadline.Sub(time.Now()).Seconds()
			if remaining < test.expected-2 || remaining > test.expected+1 {
				t.Fatalf("retrieval deadline remaining=%v want approximately %v", remaining, test.expected)
			}
		})
	}
}

func TestRecallResponseRetryRejectsPartialWriteAfterConnection(t *testing.T) {
	var requests atomic.Int32
	c := newCLI(io.Discard, ioDiscard{})
	c.client = &http.Client{Transport: testRoundTripper(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		trace := httptrace.ContextClientTrace(request.Context())
		if trace == nil || trace.GotConn == nil {
			return nil, errors.New("test transport did not receive client trace")
		}
		trace.GotConn(httptrace.GotConnInfo{})
		return nil, &net.OpError{Op: "write", Net: "tcp", Err: errors.New("partial write")}
	})}
	transport := c.recallResponseTransport()
	_, err := transport.requestRecallResponseWithRetries("/memory/recall/response", map[string]any{"query": "partial write"}, 1, 3, 0)
	if requests.Load() != 1 {
		t.Fatalf("partial-write recall failure was replayed: requests=%d", requests.Load())
	}
	var requestErr *cliRequestError
	if !errors.As(err, &requestErr) || !requestErr.GotConnection || requestErr.WroteRequest || requestErr.Retryable {
		t.Fatalf("partial-write recall retry evidence was incorrect: %#v", err)
	}
}

func TestRecallResponseRetryAllowsPreConnectionDialFailure(t *testing.T) {
	var requests atomic.Int32
	c := newCLI(io.Discard, ioDiscard{})
	c.client = &http.Client{Transport: testRoundTripper(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	})}
	transport := c.recallResponseTransport()
	_, err := transport.requestRecallResponseWithRetries("/memory/recall/response", map[string]any{"query": "pre-connect"}, 1, 1, 0)
	if requests.Load() != 2 {
		t.Fatalf("provable pre-connection recall failure did not use the explicit retry: requests=%d", requests.Load())
	}
	var requestErr *cliRequestError
	if !errors.As(err, &requestErr) || requestErr.GotConnection || !requestErr.PreDelivery || !requestErr.Retryable {
		t.Fatalf("pre-connection recall retry evidence was incorrect: %#v", err)
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
	if err == nil || err.Error() != "ContextLattice gateway request failed" {
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
