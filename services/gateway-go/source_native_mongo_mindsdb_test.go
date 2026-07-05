package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQueryMongoRawSourceUsesSpoolFallbackWithCanonicalMetadata(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer backend.Close()

	tempDir := t.TempDir()
	spoolPath := filepath.Join(tempDir, "telemetry_spool.jsonl")
	line := `{"project":"alpha","file_name":"notes/plan.md","topic_path":"runbooks/testing","content":"profitability baseline ladder","timestamp":"2026-04-01T00:00:00Z","spool_ref":"spool://alpha-1","agent_id":"codex_gpt5","session_id":"sess-42","tags":["runtime:go","lane:mongo_raw"]}` + "\n"
	if err := os.WriteFile(spoolPath, []byte(line), 0o600); err != nil {
		t.Fatalf("write spool fixture: %v", err)
	}

	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_TELEMETRY_SPOOL_ENABLED", "true")
	t.Setenv("GO_TELEMETRY_SPOOL_PATH", spoolPath)
	t.Setenv("GO_TELEMETRY_SPOOL_BACKUP_PATH", filepath.Join(tempDir, "telemetry_spool.backup.jsonl"))
	t.Setenv("GO_RETRIEVAL_NATIVE_MONGO_RAW_ENABLED", "true")

	s := newTestServer(t, backend.URL)
	rows, warnings, err := s.queryMongoRawSource(context.Background(), map[string]any{
		"query":      "baseline ladder",
		"project":    "alpha",
		"topic_path": "runbooks/testing",
		"limit":      5,
	})
	if err != nil {
		t.Fatalf("queryMongoRawSource returned error: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row from spool fallback, got %d (%#v)", len(rows), rows)
	}
	row := rows[0]
	if strings.TrimSpace(anyToString(row["source"])) != sourceMongoRaw {
		t.Fatalf("expected source=%s, got %#v", sourceMongoRaw, row["source"])
	}
	if strings.TrimSpace(anyToString(row["agent_id"])) != "codex_gpt5" {
		t.Fatalf("expected canonical agent_id, got %#v", row["agent_id"])
	}
	if strings.TrimSpace(anyToString(row["session_id"])) != "sess-42" {
		t.Fatalf("expected canonical session_id, got %#v", row["session_id"])
	}
	if strings.TrimSpace(anyToString(row["content_ref"])) != "spool://alpha-1" {
		t.Fatalf("expected content_ref from spool, got %#v", row["content_ref"])
	}
	if len(warnings) == 0 || !strings.Contains(strings.ToLower(strings.Join(warnings, " | ")), "spool fallback") {
		t.Fatalf("expected spool fallback warning, got %v", warnings)
	}
}

func TestQueryMongoRawSpoolHonorsContextDeadline(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer backend.Close()

	tempDir := t.TempDir()
	spoolPath := filepath.Join(tempDir, "telemetry_spool.jsonl")
	line := `{"project":"alpha","file_name":"notes/plan.md","topic_path":"runbooks/testing","content":"profitability baseline ladder","timestamp":"2026-04-01T00:00:00Z","spool_ref":"spool://alpha-1"}` + "\n"
	payload := strings.Repeat(line, 20000)
	if err := os.WriteFile(spoolPath, []byte(payload), 0o600); err != nil {
		t.Fatalf("write spool fixture: %v", err)
	}

	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_TELEMETRY_SPOOL_ENABLED", "true")
	t.Setenv("GO_TELEMETRY_SPOOL_PATH", spoolPath)
	t.Setenv("GO_TELEMETRY_SPOOL_BACKUP_PATH", filepath.Join(tempDir, "telemetry_spool.backup.jsonl"))
	t.Setenv("GO_RETRIEVAL_NATIVE_MONGO_RAW_ENABLED", "true")
	t.Setenv("GO_RETRIEVAL_MONGO_SPOOL_MAX_SCAN_LINES", "500000")
	t.Setenv("GO_RETRIEVAL_MONGO_SPOOL_MAX_SCAN_BYTES", "268435456")

	s := newTestServer(t, backend.URL)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-10*time.Millisecond))
	defer cancel()
	_, err := s.queryMongoRawSpool(ctx, "baseline", 5, 50, "alpha", "runbooks/testing")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded from spool fallback, got %v", err)
	}
}

func TestQueryMindsdbSourceNativeSQLAdapter(t *testing.T) {
	var capturedBody string
	mindsdb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/sql/query" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		capturedBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"type":"table",
			"data":[
				{
					"project":"alpha",
					"file":"runbooks/testing/plan.md",
					"summary":"profitability baseline ladder",
					"created_at":"2026-04-01T01:02:03Z",
					"agent_id":"codex_gpt5",
					"session_id":"sess-99",
					"tags":["lane:mindsdb"],
					"content_ref":"sha256:abc123"
				}
			]
		}`))
	}))
	defer mindsdb.Close()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer backend.Close()

	t.Setenv("GO_RETRIEVAL_NATIVE_MINDSDB_ENABLED", "true")
	t.Setenv("MINDSDB_ENABLED", "true")
	t.Setenv("MINDSDB_AUTOSYNC", "true")
	t.Setenv("GO_MINDSDB_SQL_URL", mindsdb.URL+"/api/sql/query")
	t.Setenv("MINDSDB_AUTOSYNC_DB", "files")
	t.Setenv("MINDSDB_AUTOSYNC_TABLE", "memory_events")

	s := newTestServer(t, backend.URL)
	rows, warnings, err := s.queryMindsdbSource(context.Background(), map[string]any{
		"query":      "baseline ladder",
		"project":    "alpha",
		"topic_path": "runbooks/testing",
		"limit":      5,
	})
	if err != nil {
		t.Fatalf("queryMindsdbSource returned error: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", warnings)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row from mindsdb adapter, got %d (%#v)", len(rows), rows)
	}
	row := rows[0]
	if strings.TrimSpace(anyToString(row["source"])) != sourceMindsdb {
		t.Fatalf("expected source=%s, got %#v", sourceMindsdb, row["source"])
	}
	if strings.TrimSpace(anyToString(row["agent_id"])) != "codex_gpt5" {
		t.Fatalf("expected canonical agent_id, got %#v", row["agent_id"])
	}
	if strings.TrimSpace(anyToString(row["session_id"])) != "sess-99" {
		t.Fatalf("expected canonical session_id, got %#v", row["session_id"])
	}
	if strings.TrimSpace(anyToString(row["content_ref"])) != "sha256:abc123" {
		t.Fatalf("expected content_ref passthrough, got %#v", row["content_ref"])
	}
	if !strings.Contains(strings.ToLower(capturedBody), "select project, file, summary") {
		t.Fatalf("expected SQL query body in mindsdb request, got %q", capturedBody)
	}
}

func TestStatusStrictRuntimeAdvertisesOnlyExecutableRetrievalLanes(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	tempDir := t.TempDir()
	t.Setenv("BACKEND_URL", backend.URL)
	t.Setenv("GATEWAY_PROXY_TIMEOUT_SECS", "2")
	t.Setenv("GO_RUNTIME_STRICT_NO_PYTHON", "true")
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_TELEMETRY_SPOOL_ENABLED", "true")
	t.Setenv("GO_TELEMETRY_SPOOL_PATH", filepath.Join(tempDir, "telemetry_spool.jsonl"))
	t.Setenv("GO_TELEMETRY_SPOOL_BACKUP_PATH", filepath.Join(tempDir, "telemetry_spool.backup.jsonl"))
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "topic_rollups,mongo_raw,mindsdb,weaviate")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "topic_rollups,mongo_raw,mindsdb,weaviate")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("ORCH_WEAVIATE_ENABLED", "false")
	t.Setenv("MINDSDB_ENABLED", "true")
	t.Setenv("MINDSDB_AUTOSYNC", "true")
	t.Setenv("GO_MINDSDB_SQL_URL", "http://mindsdb.invalid/api/sql/query")
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "")

	s := newServer()
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Get(gateway.URL + "/status")
	if err != nil {
		t.Fatalf("status request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 from strict status, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode status payload: %v", err)
	}
	services, _ := payload["services"].([]any)
	names := map[string]bool{}
	for _, item := range services {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		names[strings.TrimSpace(anyToString(row["name"]))] = true
	}
	if !names["retrieval/mongo_raw"] {
		t.Fatalf("expected executable retrieval/mongo_raw lane in strict status, got %#v", services)
	}
	if !names["retrieval/mindsdb"] {
		t.Fatalf("expected executable retrieval/mindsdb lane in strict status, got %#v", services)
	}
	if names["retrieval/weaviate"] {
		t.Fatalf("expected non-executable retrieval/weaviate lane to be excluded in strict status")
	}
	metadataContract, ok := payload["metadataContract"].(map[string]any)
	if !ok {
		t.Fatalf("expected metadataContract snapshot on status payload, got %#v", payload["metadataContract"])
	}
	contract := anyToStringSlice(metadataContract["contract"])
	if len(contract) == 0 || contract[len(contract)-1] != "content_ref" {
		t.Fatalf("expected canonical contract list with content_ref, got %#v", metadataContract["contract"])
	}
}
