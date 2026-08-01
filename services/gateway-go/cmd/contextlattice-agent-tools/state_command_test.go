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

func TestStateCommandDryRunDoesNotMutateLegacyOrDestination(t *testing.T) {
	base := t.TempDir()
	legacyRoot := filepath.Join(base, "legacy")
	stateRoot := filepath.Join(base, "state")
	if err := os.MkdirAll(legacyRoot, 0o700); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}
	legacyFile := filepath.Join(legacyRoot, "sessions.json")
	if err := os.WriteFile(legacyFile, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	command := newCLI(stdout, stderr)
	if err := command.run([]string{
		"contextlattice", "state", "migrate",
		"--legacy-root", legacyRoot,
		"--state-root", stateRoot,
	}); err != nil {
		t.Fatalf("state migrate dry run: %v stderr=%s", err, stderr.String())
	}
	payload := map[string]any{}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode state dry run: %v output=%s", err, stdout.String())
	}
	if !asBool(payload["ok"]) || !asBool(payload["dry_run"]) || firstString(payload["status"]) != "dry_run_ready" {
		t.Fatalf("unexpected state dry run: %#v", payload)
	}
	if _, err := os.Stat(legacyFile); err != nil {
		t.Fatalf("dry run changed legacy file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, "sessions.json")); !os.IsNotExist(err) {
		t.Fatalf("dry run created destination: %v", err)
	}
}

func TestStateStatusReturnsGatewayInventory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/telemetry/storage" {
			t.Fatalf("unexpected state status path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"gatewayState":{"ok":true,"schema_id":"contextlattice_gateway_state_inventory.v1","root":{"path":"/tmp/state"}}}`))
	}))
	defer server.Close()
	stdout := &bytes.Buffer{}
	command := newCLI(stdout, &bytes.Buffer{})
	if err := command.run([]string{"contextlattice_state", "status", "--base-url", server.URL}); err != nil {
		t.Fatalf("state status: %v", err)
	}
	payload := map[string]any{}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode state status: %v output=%s", err, stdout.String())
	}
	if !asBool(payload["ok"]) || firstString(payload["schema_id"]) != "contextlattice_gateway_state_status.v1" {
		t.Fatalf("unexpected state status: %#v", payload)
	}
	if nativeToolNames["contextlattice_state"] != "state" {
		t.Fatalf("state native alias missing: %#v", nativeToolNames["contextlattice_state"])
	}
}
