package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestStorageTelemetryAllowsMissingAPIKeyWhenGatewayHasConfiguredKey(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_GATEWAY_TEST_KEEP_ORCH_KEY", "true")
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "secret")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Get(gateway.URL + "/telemetry/storage")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestStorageTelemetryReturnsSnapshot(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_GATEWAY_TEST_KEEP_ORCH_KEY", "true")
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "secret")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	req, err := http.NewRequest(http.MethodGet, gateway.URL+"/telemetry/storage", nil)
	if err != nil {
		t.Fatalf("request build failed: %v", err)
	}
	req.Header.Set("X-Api-Key", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !anyToBool(payload["ok"]) {
		t.Fatalf("expected ok=true payload=%v", payload)
	}
	storageGov, ok := payload["storageGovernance"].(map[string]any)
	if !ok {
		t.Fatalf("missing storageGovernance payload=%v", payload)
	}
	if anyToString(storageGov["pressureBand"]) == "" {
		t.Fatalf("expected pressureBand in payload=%v", payload)
	}
}

func TestStorageMaintenanceRunRequiresAPIKey(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_GATEWAY_TEST_KEEP_ORCH_KEY", "true")
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "secret")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	req, err := http.NewRequest(http.MethodPost, gateway.URL+"/maintenance/storage/run", nil)
	if err != nil {
		t.Fatalf("request build failed: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestStorageMaintenanceRunReturnsOkWhenTelemetrySinkDisabled(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_GATEWAY_TEST_KEEP_ORCH_KEY", "true")
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "secret")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	req, err := http.NewRequest(http.MethodPost, gateway.URL+"/maintenance/storage/run?force=true", nil)
	if err != nil {
		t.Fatalf("request build failed: %v", err)
	}
	req.Header.Set("X-Api-Key", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !anyToBool(payload["ok"]) {
		t.Fatalf("expected ok=true payload=%v", payload)
	}
	tasks, ok := payload["tasks"].(map[string]any)
	if !ok {
		t.Fatalf("missing tasks payload=%v", payload)
	}
	row, ok := tasks["telemetry_blob_gc"].(map[string]any)
	if !ok {
		t.Fatalf("missing telemetry_blob_gc payload=%v", payload)
	}
	if !anyToBool(row["skipped"]) {
		t.Fatalf("expected skipped telemetry gc when sink disabled payload=%v", row)
	}
}

func TestStorageTelemetryLedgerReturnsTailRows(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_GATEWAY_TEST_KEEP_ORCH_KEY", "true")
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "secret")

	tmpDir := t.TempDir()
	ledgerPath := filepath.Join(tmpDir, "storage_ledger.ndjson")
	ledger := `{"captured_at":"2026-05-13T00:00:00Z","v":1}
{"captured_at":"2026-05-13T01:00:00Z","v":2}
{"captured_at":"2026-05-13T02:00:00Z","v":3}
`
	if err := os.WriteFile(ledgerPath, []byte(ledger), 0o644); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
	t.Setenv("ORCH_STORAGE_LEDGER_PATH", ledgerPath)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	req, err := http.NewRequest(http.MethodGet, gateway.URL+"/telemetry/storage/ledger?limit=2", nil)
	if err != nil {
		t.Fatalf("request build failed: %v", err)
	}
	req.Header.Set("X-Api-Key", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !anyToBool(payload["ok"]) {
		t.Fatalf("expected ok=true payload=%v", payload)
	}
	if anyToInt64(payload["count"], 0) != 2 {
		t.Fatalf("expected count=2 payload=%v", payload)
	}
	rows, ok := payload["rows"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("expected 2 rows payload=%v", payload)
	}
	row0, ok := rows[0].(map[string]any)
	if !ok {
		t.Fatalf("row[0] type mismatch payload=%v", payload)
	}
	row1, ok := rows[1].(map[string]any)
	if !ok {
		t.Fatalf("row[1] type mismatch payload=%v", payload)
	}
	if anyToInt64(row0["v"], 0) != 2 || anyToInt64(row1["v"], 0) != 3 {
		t.Fatalf("expected tail rows v=2,3 payload=%v", payload)
	}
}

func TestStorageTelemetryLedgerInvalidSinceReturnsBadRequest(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_GATEWAY_TEST_KEEP_ORCH_KEY", "true")
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "secret")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	req, err := http.NewRequest(http.MethodGet, gateway.URL+"/telemetry/storage/ledger?since=not-a-timestamp", nil)
	if err != nil {
		t.Fatalf("request build failed: %v", err)
	}
	req.Header.Set("X-Api-Key", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestStorageTelemetryLedgerMissingFileReturnsEmpty(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_GATEWAY_TEST_KEEP_ORCH_KEY", "true")
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "secret")
	t.Setenv("ORCH_STORAGE_LEDGER_PATH", filepath.Join(t.TempDir(), "missing.ndjson"))

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	req, err := http.NewRequest(http.MethodGet, gateway.URL+"/telemetry/storage/ledger", nil)
	if err != nil {
		t.Fatalf("request build failed: %v", err)
	}
	req.Header.Set("X-Api-Key", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !anyToBool(payload["ok"]) {
		t.Fatalf("expected ok=true payload=%v", payload)
	}
	if anyToBool(payload["exists"]) {
		t.Fatalf("expected exists=false payload=%v", payload)
	}
	if anyToInt64(payload["count"], 1) != 0 {
		t.Fatalf("expected count=0 payload=%v", payload)
	}
}
