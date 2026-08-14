package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMarkerCapMigrationCLIUsesAuthenticatedNativeRoute(t *testing.T) {
	var seenHeaders http.Header
	var seenBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/ops/evaluation-cleanup/marker-cap-migration" {
			t.Fatalf("CLI used unexpected route: %s %s", r.Method, r.URL.Path)
		}
		seenHeaders = r.Header.Clone()
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &seenBody); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"ok":true,"operation":"extend"}`))
	}))
	defer server.Close()
	var stdout bytes.Buffer
	cli := newCLI(&stdout, &bytes.Buffer{})
	cli.baseURL = server.URL
	if err := cli.cmdMarkerCapMigration([]string{
		"--operation", "extend", "--new-max-marker-count", "100001", "--new-max-marker-bytes", "67112960",
		"--expected-generation", "0",
		"--capability", "capability", "--principal", "operator-1", "--workspace-id", "workspace-1",
		"--operator-ref", "migration-1", "--reason", "verified", "--raw",
	}); err != nil {
		t.Fatalf("CLI invocation failed: %v", err)
	}
	if seenHeaders.Get("X-ContextLattice-Evaluation-Cleanup-Capability") != "capability" || seenHeaders.Get("X-ContextLattice-Operator-Principal") != "operator-1" || seenHeaders.Get("X-ContextLattice-Workspace-ID") != "workspace-1" {
		t.Fatalf("CLI omitted migration authority headers: %#v", seenHeaders)
	}
	if seenBody["operation"] != "extend" || seenBody["operator_ref"] != "migration-1" {
		t.Fatalf("CLI sent an unexpected migration payload: %#v", seenBody)
	}
}
