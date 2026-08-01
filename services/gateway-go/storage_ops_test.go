package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func storageTestStringSliceContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

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
	gatewayState, ok := payload["gatewayState"].(map[string]any)
	if !ok || anyToString(gatewayState["schema_id"]) != "contextlattice_gateway_state_inventory.v1" {
		t.Fatalf("missing canonical gateway state inventory payload=%v", payload)
	}
	if !anyToBool(gatewayState["ok"]) || anyToInt(gatewayState["unhealthy_entries"], -1) != 0 {
		t.Fatalf("expected healthy canonical gateway state inventory=%v", gatewayState)
	}
	root, _ := gatewayState["root"].(map[string]any)
	if anyToString(root["source_env"]) != "CONTEXTLATTICE_GATEWAY_STATE_ROOT" || !filepath.IsAbs(anyToString(root["path"])) {
		t.Fatalf("expected absolute canonical state root payload=%v", root)
	}
	storageGov, ok := payload["storageGovernance"].(map[string]any)
	if !ok {
		t.Fatalf("missing storageGovernance payload=%v", payload)
	}
	if anyToString(storageGov["pressureBand"]) == "" {
		t.Fatalf("expected pressureBand in payload=%v", payload)
	}
	topology, ok := payload["memoryTopology"].(map[string]any)
	if !ok {
		t.Fatalf("missing memoryTopology payload=%v", payload)
	}
	if anyToString(topology["schema_id"]) != "contextlattice_memory_topology.v1" {
		t.Fatalf("unexpected memory topology schema payload=%v", topology)
	}
	if anyToString(topology["default_app_profile"]) != "base_default" {
		t.Fatalf("expected base_default topology profile payload=%v", topology)
	}
	hotPath := anyToStringList(topology["base_default_hot_path"], 10)
	if len(hotPath) != 2 || hotPath[0] != "topic_rollups" || hotPath[1] != "qdrant" {
		t.Fatalf("expected base hot path topic_rollups/qdrant, got %v", hotPath)
	}
	partitioning, ok := topology["partitioning"].(map[string]any)
	if !ok {
		t.Fatalf("missing partitioning payload=%v", topology)
	}
	writeKeys := anyToStringList(partitioning["default_write_partition_keys"], 20)
	for _, required := range []string{"project", "topic_path", "session_id", "agent_id", "content_hash"} {
		if !storageTestStringSliceContains(writeKeys, required) {
			t.Fatalf("missing topology write partition key %q in %v", required, writeKeys)
		}
	}
	clusters, ok := topology["clusters"].([]any)
	if !ok || len(clusters) == 0 {
		t.Fatalf("missing topology clusters payload=%v", topology)
	}
	clusterIDs := []string{}
	for _, raw := range clusters {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		clusterIDs = append(clusterIDs, anyToString(row["id"]))
	}
	for _, required := range []string{"base_default", "vector_semantic", "raw_audit", "graph_relationships", "deep_recall", "agent_runtime"} {
		if !storageTestStringSliceContains(clusterIDs, required) {
			t.Fatalf("missing topology cluster %q in %v", required, clusterIDs)
		}
	}
	profiles, ok := topology["deployment_profiles"].(map[string]any)
	if !ok {
		t.Fatalf("missing deployment profiles payload=%v", topology)
	}
	for _, profileName := range []string{"hosted_lite", "local_lite", "full", "paid_local"} {
		profile, ok := profiles[profileName].(map[string]any)
		if !ok {
			t.Fatalf("missing deployment profile %q in %v", profileName, profiles)
		}
		coreSurfaces := anyToStringList(profile["core_surfaces"], 40)
		for _, required := range []string{"context_pack", "preflight", "policy_context_package", "memory_edges", "graph_neighbors"} {
			if !storageTestStringSliceContains(coreSurfaces, required) {
				t.Fatalf("profile %q missing core surface %q in %v", profileName, required, coreSurfaces)
			}
		}
	}
	localLite := profiles["local_lite"].(map[string]any)
	localDefaults := anyToStringList(localLite["default_sources"], 20)
	if storageTestStringSliceContains(localDefaults, "postgres_pgvector") || storageTestStringSliceContains(localDefaults, "llama-cpp") {
		t.Fatalf("local lite should not default pgvector or llama.cpp, got %v", localDefaults)
	}
	localConnectors := anyToStringList(localLite["connector_only_inference_runtimes"], 20)
	if !storageTestStringSliceContains(localConnectors, "llama-cpp") {
		t.Fatalf("expected llama.cpp as local lite connector runtime, got %v", localConnectors)
	}
	fullProfile := profiles["full"].(map[string]any)
	fullHotPath := anyToStringList(fullProfile["hot_path"], 20)
	for _, required := range []string{"topic_rollups", "qdrant", "postgres_pgvector"} {
		if !storageTestStringSliceContains(fullHotPath, required) {
			t.Fatalf("full profile missing hot path source %q in %v", required, fullHotPath)
		}
	}
	paidProfile := profiles["paid_local"].(map[string]any)
	paidPremiumSurfaces := anyToStringList(paidProfile["premium_surfaces"], 20)
	if !storageTestStringSliceContains(paidPremiumSurfaces, "premium_behavior_pack") {
		t.Fatalf("paid profile missing premium behavior surface in %v", paidPremiumSurfaces)
	}
	if !storageTestStringSliceContains(paidPremiumSurfaces, "premium_runtime_policy") {
		t.Fatalf("paid profile missing premium runtime policy surface in %v", paidPremiumSurfaces)
	}
	agentPolicy, ok := paidProfile["agent_policy"].(map[string]any)
	if !ok {
		t.Fatalf("paid profile missing agent policy payload=%v", paidProfile)
	}
	if !anyToBool(agentPolicy["premium_behavior_required"]) || anyToString(agentPolicy["policy_mode"]) != "paid_runtime_policy" || anyToBool(agentPolicy["public_contents"]) {
		t.Fatalf("paid profile agent policy exposes invalid premium behavior boundary: %v", agentPolicy)
	}
	paidConnectors := anyToStringList(paidProfile["connector_surfaces"], 20)
	if !storageTestStringSliceContains(paidConnectors, "obsidian_import_export") {
		t.Fatalf("paid profile missing Obsidian connector surface in %v", paidConnectors)
	}
}

func TestPressureBandUsesConfiguredMinimumFreeSpaceAsHighThresholdOnly(t *testing.T) {
	policy := storageGovernancePolicy{
		warnUsedRatio: 0.85,
		highUsedRatio: 0.92,
		minFreeBytes:  40 * 1024 * 1024 * 1024,
	}

	if got := pressureBand(0.45, 50*1024*1024*1024, policy); got != "healthy" {
		t.Fatalf("expected healthy above configured minimum free space, got %q", got)
	}
	if got := pressureBand(0.86, 50*1024*1024*1024, policy); got != "warn" {
		t.Fatalf("expected warn above used-ratio threshold, got %q", got)
	}
	if got := pressureBand(0.45, 40*1024*1024*1024, policy); got != "high" {
		t.Fatalf("expected high at configured minimum free space, got %q", got)
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
