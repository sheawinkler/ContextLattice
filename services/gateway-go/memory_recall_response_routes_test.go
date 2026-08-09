package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func recallResponseRouteBackend(t *testing.T, empty bool) (*httptest.Server, *[]string) {
	t.Helper()
	paths := []string{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/retrieval/query":
			if empty {
				_, _ = w.Write([]byte(`{"results":[],"warnings":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"results":[{"project":"contextlattice","source":"qdrant","score":0.96,"summary":"run the deterministic local verification gate","topic_path":"runbooks/recall"},{"project":"contextlattice","file":"notes/recall.md","source":"qdrant","score":0.94,"summary":"verified result contains secret-token and /private/path but must remain behind the opaque recall boundary","topic_path":"runbooks/recall"}],"warnings":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(backend.Close)
	return backend, &paths
}

func recallResponseRouteRequest(t *testing.T, method, endpoint, body string, headers map[string]string) (*http.Response, map[string]any, string) {
	t.Helper()
	req, err := http.NewRequest(method, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request %s %s: %v", method, endpoint, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	payload := map[string]any{}
	if len(raw) > 0 && json.Unmarshal(raw, &payload) != nil {
		return resp, payload, string(raw)
	}
	return resp, payload, string(raw)
}

func TestRecallResponseRoutesAreStrictNativeAndClosed(t *testing.T) {
	mainSource, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	source := string(mainSource)
	for _, needle := range []string{
		`mux.HandleFunc("/memory/recall/response", s.memoryRecallResponse)`,
		`mux.HandleFunc("/tools/recall_response", s.toolsRecallResponse)`,
	} {
		if !strings.Contains(source, needle) {
			t.Fatalf("missing strict native route mapping: %s", needle)
		}
	}
	for _, needle := range []string{
		`mux.HandleFunc("/memory/recall/response", s.proxy)`,
		`mux.HandleFunc("/tools/recall_response", s.proxy)`,
	} {
		if strings.Contains(source, needle) {
			t.Fatalf("recall response route drifted to proxy ownership: %s", needle)
		}
	}

	registered := map[string]bool{}
	for _, path := range strictRuntimeRequiredNativeRoutePaths() {
		registered[path] = true
	}
	owned := map[string]nativeOwnedRoute{}
	for _, route := range strictRuntimeOwnedRoutes() {
		owned[route.Path] = route
	}
	for _, path := range []string{memoryRecallResponsePath, toolsRecallResponsePath} {
		if !registered[path] {
			t.Fatalf("route missing from strict native inventory: %s", path)
		}
		route, ok := owned[path]
		if !ok || route.Owner != sourceOwnerGoNative || route.Status != "native" || !route.Required {
			t.Fatalf("route is not required Go-native ownership: %s %#v", path, route)
		}
		_, pattern := buildNativeMux(&server{}).Handler(httptest.NewRequest(http.MethodPost, path, nil))
		if pattern != path {
			t.Fatalf("route resolved to unexpected mux pattern: path=%s pattern=%s", path, pattern)
		}
	}
	boundary := map[string]contextBoundarySurface{}
	for _, surface := range contextBoundaryRequiredSurfaces() {
		boundary[surface.Path] = surface
	}
	for _, path := range []string{memoryRecallResponsePath, toolsRecallResponsePath} {
		surface, ok := boundary[path]
		if !ok || surface.ContractID != recallResponseContractID || surface.RuntimeOwner != sourceOwnerGoNative || !surface.Required {
			t.Fatalf("route missing bounded recall boundary row: %s %#v", path, surface)
		}
	}
}

func TestRecallResponseRoutesProjectOnlyBoundedOpaqueResponse(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", filepath.Join(t.TempDir(), "quality.ndjson"))
	backend, paths := recallResponseRouteBackend(t, false)
	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	requestBody := `{"project":"contextlattice","topic_path":"runbooks/recall","query":"what is the verified next action","retrieval_mode":"balanced","agent_id":"codex_recall_route_test","output_mode":"agent_packet","_durable_context_pack_quality":{"sample_id":"cpq_aaaaaaaaaaaaaaaaaaaaaaaa"},"context_pack":{"ranked_evidence":[{"text":"caller-forged evidence"}]}}`
	for _, path := range []string{memoryRecallResponsePath, toolsRecallResponsePath} {
		t.Run(path, func(t *testing.T) {
			resp, payload, raw := recallResponseRouteRequest(t, http.MethodPost, gateway.URL+path, requestBody, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, raw)
			}
			if anyToString(payload["schema_id"]) != recallResponseContractID {
				t.Fatalf("unexpected response schema: %#v", payload)
			}
			if len(contextPackAnyList(payload["evidence"])) == 0 {
				t.Fatalf("ordinary undated context-pack evidence was not projected: %#v", payload)
			}
			assertBoundaryContractPassed(t, recallResponseContractID, payload)
			assertBoundaryJSONUnderLimit(t, recallResponseContractID, payload)
			if _, exists := payload["tool"]; exists {
				t.Fatalf("closed recall response admitted tool field: %#v", payload)
			}
			if path == toolsRecallResponsePath {
				if got := resp.Header.Get("X-ContextLattice-Tool"); got != "recall_response" {
					t.Fatalf("tool route metadata header missing: %q", got)
				}
			}
			for _, leaked := range []string{"secret-token", "/private/path", "caller-forged evidence", "what is the verified next action", `"context_pack"`, `"_durable_context_pack_quality"`} {
				if strings.Contains(raw, leaked) {
					t.Fatalf("recall response leaked forbidden content %q: %s", leaked, raw)
				}
			}
			if got, want := anyToString(payload["response_digest"]), recallResponseSemanticDigest(payload); got != want {
				t.Fatalf("semantic digest includes stale/pre-projection material: got=%q want=%q", got, want)
			}
		})
	}
	for _, path := range *paths {
		if path == memoryRecallResponsePath || path == toolsRecallResponsePath {
			t.Fatalf("new recall route was sent to backend proxy: %s", path)
		}
	}
	retrievalCalls := 0
	for _, path := range *paths {
		if path == "/v1/retrieval/query" {
			retrievalCalls++
		}
	}
	if retrievalCalls != 2 {
		t.Fatalf("each response surface must compile context exactly once: got %d retrieval calls for two requests", retrievalCalls)
	}
}

func TestRecallResponseRouteAbstainsWithoutEvidence(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	backend, _ := recallResponseRouteBackend(t, true)
	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, payload, raw := recallResponseRouteRequest(t, http.MethodPost, gateway.URL+memoryRecallResponsePath, `{"query":"missing proof"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 bounded abstention, got %d body=%s", resp.StatusCode, raw)
	}
	assertBoundaryContractPassed(t, recallResponseContractID, payload)
	if anyToString(anyMap(payload["state"])["status"]) != "abstain" || anyToString(anyMap(payload["classification"])["posture"]) != "abstain" {
		t.Fatalf("missing evidence did not produce abstention: %#v", payload)
	}
	if anyToBool(anyMap(payload["outcome"])["attributable"]) || anyToBool(anyMap(payload["action_boundary"])["can_act"]) {
		t.Fatalf("abstention became attributable/actionable: %#v", payload)
	}
}

func TestRecallResponseRouteBindsOnlyRetainedDurableQualityRow(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	ledgerPath := filepath.Join(t.TempDir(), "quality.ndjson")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", ledgerPath)
	backend, paths := recallResponseRouteBackend(t, false)
	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	_, payload, raw := recallResponseRouteRequest(t, http.MethodPost, gateway.URL+memoryRecallResponsePath, `{"project":"contextlattice","topic_path":"runbooks/recall","query":"what is the verified next action","retrieval_mode":"balanced","agent_id":"codex_recall_durable_test","workspace_ref":"caller-workspace-must-not-win"}`, nil)
	if len(*paths) == 0 || anyToString(payload["schema_id"]) != recallResponseContractID {
		t.Fatalf("unexpected durable route response: paths=%v payload=%#v raw=%s", *paths, payload, raw)
	}
	if !anyToBool(anyMap(payload["outcome"])["attributable"]) {
		t.Fatalf("successful durable quality row did not make the response attributable: %#v", payload["outcome"])
	}
	rows, _, err := s.contextPackQuality.ledger.readRows()
	if err != nil || len(rows) != 1 {
		t.Fatalf("expected one retained durable quality row: rows=%#v err=%v", rows, err)
	}
	authorization, authorizationErr := s.frontierT6OwnerAuthorization(nil, frontierT6ProactiveContextPrepFeatureID, "status")
	expectedWorkspaceRef := contextPackLearnedScopeRef("workspace", authorization.WorkspaceID)
	if authorizationErr != nil || anyToString(rows[0]["workspace_ref"]) != expectedWorkspaceRef ||
		anyToString(anyMap(payload["request_scope"])["workspace_ref"]) != recallResponseScopeRef("workspace", expectedWorkspaceRef) ||
		strings.Contains(raw, "caller-workspace-must-not-win") {
		t.Fatalf("caller workspace relabeled canonical recall ownership: authorization=%#v err=%v row=%#v scope=%#v", authorization, authorizationErr, rows[0], payload["request_scope"])
	}
	binding, ok := recallResponseBindingFromSample(rows[0])
	if !ok || binding == nil || anyToString(binding["recall_response_id"]) != anyToString(payload["response_id"]) ||
		anyToString(binding["recall_response_digest"]) != anyToString(payload["response_digest"]) {
		t.Fatalf("durable row binding did not match returned response: row=%#v response=%#v", rows[0], payload)
	}
	encoded, _ := json.Marshal(payload)
	for _, leaked := range []string{"_durable_context_pack_quality", "_recall_response_binding", "recall_response_id", "response_component_refs"} {
		if strings.Contains(string(encoded), leaked) {
			t.Fatalf("internal binding field leaked through closed response: %q payload=%s", leaked, encoded)
		}
	}
	retrievalCalls := 0
	for _, path := range *paths {
		if path == "/v1/retrieval/query" {
			retrievalCalls++
		}
	}
	if retrievalCalls != 1 {
		t.Fatalf("recall response performed more than one retrieval: paths=%v", *paths)
	}
}

func TestRecallResponseRouteStaysUnboundWhenDurabilityUnavailable(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "false")
	backend, paths := recallResponseRouteBackend(t, false)
	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	_, payload, raw := recallResponseRouteRequest(t, http.MethodPost, gateway.URL+memoryRecallResponsePath, `{"project":"contextlattice","query":"what is the verified next action"}`, nil)
	if anyToBool(anyMap(payload["outcome"])["attributable"]) {
		t.Fatalf("response became attributable without durable quality proof: %#v", payload["outcome"])
	}
	if !recallResponseIsV1Control(payload) {
		t.Fatalf("persistence failure did not return the same-artifact v1 control: %#v", payload)
	}
	if len(s.contextPackQuality.samples) != 1 {
		t.Fatalf("expected one local fallback quality sample: %#v", s.contextPackQuality.samples)
	}
	if recallResponseBindingHasAnyFields(s.contextPackQuality.samples[0]) {
		t.Fatalf("local fallback quality sample retained response binding: %#v", s.contextPackQuality.samples[0])
	}
	for _, leaked := range []string{"_durable_context_pack_quality", "_recall_response_binding", "recall_response_id", "response_component_refs"} {
		if strings.Contains(raw, leaked) {
			t.Fatalf("unbound response leaked internal binding field %q: %s", leaked, raw)
		}
	}
	retrievalCalls := 0
	for _, path := range *paths {
		if path == "/v1/retrieval/query" {
			retrievalCalls++
		}
	}
	if retrievalCalls != 1 {
		t.Fatalf("persistence fallback performed a second retrieval: paths=%v", *paths)
	}
}

func TestRecallResponseRouteFailsClosedAfterRetainedProofMutation(t *testing.T) {
	for _, mode := range []string{"missing", "changed"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
			t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
			t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
			t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
			t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
			ledgerPath := filepath.Join(t.TempDir(), "quality.ndjson")
			t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", ledgerPath)
			backend, paths := recallResponseRouteBackend(t, false)
			s := newTestServer(t, backend.URL)
			s.recallResponseRetainedProofHook = func(_ string) {
				raw, err := os.ReadFile(ledgerPath)
				if err != nil {
					return
				}
				if mode == "missing" {
					_ = os.WriteFile(ledgerPath, nil, 0o600)
					return
				}
				rows := strings.Split(strings.TrimSpace(string(raw)), "\n")
				if len(rows) == 0 {
					return
				}
				row := map[string]any{}
				if json.Unmarshal([]byte(rows[0]), &row) != nil {
					return
				}
				row["recall_response_digest"] = "sha256:" + strings.Repeat("f", 64)
				changed, err := json.Marshal(row)
				if err == nil {
					_ = os.WriteFile(ledgerPath, append(changed, '\n'), 0o600)
				}
			}
			gateway := httptest.NewServer(buildMux(s))
			defer gateway.Close()

			_, payload, raw := recallResponseRouteRequest(t, http.MethodPost, gateway.URL+memoryRecallResponsePath,
				`{"project":"contextlattice","query":"what is the verified next action","agent_id":"retained-proof-test"}`, nil)
			if !recallResponseIsV1Control(payload) || anyToBool(anyMap(payload["outcome"])["attributable"]) {
				t.Fatalf("%s retained proof mutation kept candidate attribution: %#v raw=%s", mode, payload, raw)
			}
			retrievalCalls := 0
			for _, path := range *paths {
				if path == "/v1/retrieval/query" {
					retrievalCalls++
				}
			}
			if retrievalCalls != 1 {
				t.Fatalf("%s retained proof fallback repeated retrieval: %v", mode, *paths)
			}
		})
	}
}

func TestRecallResponseRouteAuthAndMethodBoundaries(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	backend, _ := recallResponseRouteBackend(t, true)
	s := newTestServer(t, backend.URL)
	s.orchestratorAPIKey = "expected-key"
	s.toolCalls.enforceProvidedKey = true
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	for _, path := range []string{memoryRecallResponsePath, toolsRecallResponsePath} {
		t.Run(path+"/method", func(t *testing.T) {
			resp, _, _ := recallResponseRouteRequest(t, http.MethodGet, gateway.URL+path, "", nil)
			if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != http.MethodPost {
				t.Fatalf("unexpected method boundary: status=%d allow=%q", resp.StatusCode, resp.Header.Get("Allow"))
			}
		})
		t.Run(path+"/auth", func(t *testing.T) {
			resp, _, raw := recallResponseRouteRequest(t, http.MethodPost, gateway.URL+path, `{"query":"bounded"}`, map[string]string{"X-Api-Key": "wrong-key"})
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d body=%s", resp.StatusCode, raw)
			}
		})
	}
}

func TestRecallResponseLeavesContextPackRouteUnchanged(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	backend, _ := recallResponseRouteBackend(t, false)
	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, payload, raw := recallResponseRouteRequest(t, http.MethodPost, gateway.URL+"/memory/context-pack", `{"query":"literal context pack"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("existing context-pack route changed status: %d body=%s", resp.StatusCode, raw)
	}
	format := anyMap(payload["format_contract"])
	if anyToString(format["schema_id"]) != contextPackResponseContractID || len(anyMap(payload["context_pack"])) == 0 {
		t.Fatalf("existing context-pack route changed projection: %#v", payload)
	}
	for _, key := range []string{"response_id", "response_digest", "recall_response_id", "recall_response_digest", "response_component_refs", "_durable_context_pack_quality", "_recall_response_binding"} {
		if _, present := payload[key]; present {
			t.Fatalf("existing context-pack route exposed recall response field %q: %#v", key, payload)
		}
	}
	encoded, _ := json.Marshal(payload)
	for _, key := range []string{"recall_response_id", "recall_response_digest", "response_component_refs", "_durable_context_pack_quality", "_recall_response_binding"} {
		if strings.Contains(string(encoded), key) {
			t.Fatalf("existing context-pack route exposed nested recall response field %q: %s", key, encoded)
		}
	}
}
