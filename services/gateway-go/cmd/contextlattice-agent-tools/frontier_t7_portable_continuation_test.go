package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func frontierT7PortableContinuationTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CONTEXTLATTICE_CONFIG_HOME", t.TempDir())
	for _, name := range []string{
		"CONTEXTLATTICE_RUNTIME_LICENSE",
		"CONTEXTLATTICE_ENTITLEMENT_KEY",
		"GO_V4_ENTITLEMENT_KEY",
		"CONTEXTLATTICE_PLAN",
		"CONTEXTLATTICE_WORKSPACE_ROLE",
		"CONTEXTLATTICE_MACHINE_ID",
	} {
		t.Setenv(name, "")
	}
}

func frontierT7WritePortableContinuationTestFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFrontierT7PortableContinuationOperationRoutes(t *testing.T) {
	frontierT7PortableContinuationTestEnv(t)
	payloadPath := frontierT7WritePortableContinuationTestFile(t, `{"request":"bounded"}`)
	type requestRecord struct {
		method    string
		path      string
		operation string
	}
	var requests []requestRecord
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected status method=%s", r.Method)
			return
		}
		payload := map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode %s: %v", r.URL.Path, err)
			return
		}
		requests = append(requests, requestRecord{method: r.Method, path: r.URL.Path, operation: firstString(payload["operation"])})
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer gateway.Close()

	cases := []struct {
		operation string
		path      string
		wire      string
	}{
		{operation: "grant-create", path: frontierT7PortableContinuationGrantsPath, wire: "create"},
		{operation: "grant-authorize", path: frontierT7PortableContinuationGrantsPath, wire: "authorize"},
		{operation: "grant-revoke", path: frontierT7PortableContinuationGrantsPath, wire: "revoke"},
		{operation: "import-plan", path: frontierT7PortableContinuationImportsPath, wire: "plan"},
		{operation: "import-commit", path: frontierT7PortableContinuationImportsPath, wire: "commit"},
		{operation: "manifest-create", path: frontierT7PortableContinuationManifestsPath, wire: "create"},
		{operation: "manifest-reconcile", path: frontierT7PortableContinuationManifestsPath, wire: "reconcile"},
	}
	for _, test := range cases {
		t.Run(test.operation, func(t *testing.T) {
			var stdout bytes.Buffer
			c := newCLI(&stdout, ioDiscard{})
			c.baseURL = gateway.URL
			c.apiKey = ""
			if err := c.run([]string{"contextlattice_agent_tools", "portable-continuation", test.operation, "--payload-file", payloadPath, "--raw"}); err != nil {
				t.Fatalf("run: %v", err)
			}
			request := requests[len(requests)-1]
			if request.method != http.MethodPost || request.path != test.path || request.operation != test.wire {
				t.Fatalf("request=%#v, want POST %s operation=%s", request, test.path, test.wire)
			}
		})
	}
	if len(requests) != len(cases) {
		t.Fatalf("request count=%d, want %d", len(requests), len(cases))
	}
}

func TestFrontierT7PortableContinuationStatusUsesGETRoute(t *testing.T) {
	frontierT7PortableContinuationTestEnv(t)
	var method, path string
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read status body: %v", err)
		}
		if len(body) != 0 {
			t.Errorf("status request body=%q", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "status": "ready"})
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	c.apiKey = ""
	if err := c.run([]string{"contextlattice_agent_tools", "portable-continuation", "status", "--raw"}); err != nil {
		t.Fatalf("status: %v", err)
	}
	if method != http.MethodGet || path != frontierT7PortableContinuationStatusPath {
		t.Fatalf("request=%s %s", method, path)
	}
}

func TestFrontierT7PortableContinuationRejectsInvalidAndConflictingPayloads(t *testing.T) {
	frontierT7PortableContinuationTestEnv(t)
	requestCount := 0
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer gateway.Close()

	tests := []struct {
		name    string
		content string
		secret  string
	}{
		{name: "invalid json", content: `{"safe":`, secret: `{"safe":`},
		{name: "conflicting operation", content: `{"operation":"revoke","secret":"do-not-print"}`, secret: "do-not-print"},
		{name: "non object", content: `[1,2,3]`, secret: "1,2,3"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payloadPath := frontierT7WritePortableContinuationTestFile(t, test.content)
			var stdout bytes.Buffer
			c := newCLI(&stdout, ioDiscard{})
			c.baseURL = gateway.URL
			c.apiKey = ""
			err := c.run([]string{"contextlattice_agent_tools", "portable-continuation", "grant-create", "--payload-file", payloadPath, "--raw"})
			if err == nil {
				t.Fatal("expected validation error")
			}
			if strings.Contains(err.Error(), test.secret) || strings.Contains(err.Error(), payloadPath) || stdout.Len() != 0 {
				t.Fatalf("unsafe validation result: err=%q stdout=%q", err, stdout.String())
			}
		})
	}
	if requestCount != 0 {
		t.Fatalf("gateway received %d invalid requests", requestCount)
	}
}

func TestFrontierT7PortableContinuationMergesEnvelopeFile(t *testing.T) {
	frontierT7PortableContinuationTestEnv(t)
	payloadPath := frontierT7WritePortableContinuationTestFile(t, `{"manifest_id":"manifest_1"}`)
	envelopePath := frontierT7WritePortableContinuationTestFile(t, `{"envelope_id":"envelope_1","ciphertext":"ciphertext_1"}`)
	var captured map[string]any
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decode manifest reconcile: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	c.apiKey = ""
	if err := c.run([]string{"contextlattice_agent_tools", "portable-continuation", "manifest-reconcile", "--payload-file", payloadPath, "--envelope-file", envelopePath, "--raw"}); err != nil {
		t.Fatalf("manifest reconcile: %v", err)
	}
	if captured["operation"] != "reconcile" || firstString(asMap(captured["envelope"])["envelope_id"]) != "envelope_1" {
		t.Fatalf("captured payload=%#v", captured)
	}
}

func TestFrontierT7PortableContinuationErrorsAndOutputDoNotLeakSecrets(t *testing.T) {
	frontierT7PortableContinuationTestEnv(t)
	payloadPath := frontierT7WritePortableContinuationTestFile(t, `{"request":"bounded"}`)
	var stdout bytes.Buffer
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"ok":false,"envelope":{"plaintext":"secret-envelope"},"api_key":"api-key-value","path":"/Users/owner/private.json"}`))
	}))
	defer gateway.Close()
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	c.apiKey = ""
	err := c.run([]string{"contextlattice_agent_tools", "portable-continuation", "grant-create", "--payload-file", payloadPath})
	if err == nil || strings.Contains(err.Error(), "secret-envelope") || strings.Contains(err.Error(), "api-key-value") || strings.Contains(err.Error(), "/Users/owner/private.json") {
		t.Fatalf("unsafe request error=%v", err)
	}

	gateway.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":       true,
			"envelope": map[string]any{"plaintext": "secret-envelope"},
			"api_key":  "api-key-value",
			"path":     "/Users/owner/private.json",
		})
	})
	stdout.Reset()
	if err := c.run([]string{"contextlattice_agent_tools", "portable-continuation", "grant-create", "--payload-file", payloadPath}); err != nil {
		t.Fatalf("safe output: %v", err)
	}
	if output := stdout.String(); strings.Contains(output, "secret-envelope") || strings.Contains(output, "api-key-value") || strings.Contains(output, "/Users/owner/private.json") {
		t.Fatalf("unsafe output=%s", output)
	}
}

func TestFrontierT7PortableContinuationRequiresOwnerOnlyInput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("owner-only mode is a Unix convention")
	}
	frontierT7PortableContinuationTestEnv(t)
	payloadPath := frontierT7WritePortableContinuationTestFile(t, `{"request":"bounded"}`)
	if err := os.Chmod(payloadPath, 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	if err := c.run([]string{"contextlattice_agent_tools", "portable-continuation", "grant-create", "--payload-file", payloadPath}); err == nil || strings.Contains(err.Error(), payloadPath) {
		t.Fatalf("expected safe owner-only error, got %v", err)
	}
}
