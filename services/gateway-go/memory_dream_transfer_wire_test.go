package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDreamTransferWireBoundsAndCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		code       int
	}{
		{"one", `{"project":"b","experience_transfer":{"action":"record_result","evidence_refs":["b::e"]}}`, 200},
		{"duplicate", `{"project":"a","project":"b"}`, 400},
		{"nested duplicate", `{"experience_transfer":{"result":"one","result":"two"}}`, 400},
		{"case alias", `{"Project":"b"}`, 400},
		{"second value", `{}{}`, 400},
		{"deep", `{"a":[[[[["x"]]]]]}`, 400},
		{"invalid encoding", "{\"a\":\"" + string([]byte{0xff}) + "\"}", 400},
		{"too large", strings.Repeat(" ", dreamExperienceTransferMaxBytes) + "{}", 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, code := readDreamTransferWire(httptest.NewRequest(http.MethodPost, "/memory/dream", strings.NewReader(tc.body)))
			if code != tc.code {
				t.Fatalf("status=%d want=%d", code, tc.code)
			}
		})
	}
	s := &server{orchestratorAPIKey: "service", toolCalls: toolCallPolicy{roleSplitEnabled: true, workerKey: "worker",
		workerRole: toolRolePolicy{allowAll: true}}}
	req := httptest.NewRequest(http.MethodPost, "/memory/dream", nil)
	req.Header.Set("X-Api-Key", "worker")
	req.Header.Set("X-ContextLattice-Dream-Proof", strings.Repeat("a", 64))
	if _, _, ok := dreamTransferWorkerCredentials(s, req); !ok {
		t.Fatal("dedicated role and proof were refused")
	}
	for _, change := range []func(*http.Request){
		func(r *http.Request) { r.Header.Set("X-Api-Key", "service") },
		func(r *http.Request) { r.Header.Add("X-Api-Key", "worker") },
		func(r *http.Request) { r.Header.Add("X-ContextLattice-Dream-Proof", strings.Repeat("b", 64)) },
		func(r *http.Request) { r.Header.Set("Authorization", "Bearer other") },
		func(r *http.Request) { r.URL.RawQuery = "api_key=other" },
	} {
		r := req.Clone(req.Context())
		change(r)
		if _, _, ok := dreamTransferWorkerCredentials(s, r); ok {
			t.Fatal("shared or ambiguous caller was accepted")
		}
	}
	s.toolCalls.workerRole.denylist = map[string]struct{}{normalizeToolPath("/tools/dream"): {}}
	if _, _, ok := dreamTransferWorkerCredentials(s, req); ok {
		t.Fatal("transfer bypassed the worker tool policy")
	}
}

func TestDreamTransferPermissionDoesNotGrantOrdinaryDream(t *testing.T) {
	s := &server{orchestratorAPIKey: "service", toolCalls: toolCallPolicy{roleSplitEnabled: true, workerKey: "worker",
		workerRole: toolRolePolicy{allowlist: parseToolPathSet("dream_transfer")}}}
	for _, route := range []string{"/tools/dream", "/memory/dream"} {
		req := httptest.NewRequest(http.MethodPost, route, strings.NewReader(`{"project":"demo","query":"ordinary Dream"}`))
		req.Header.Set("X-Api-Key", "worker")
		rr := httptest.NewRecorder()
		if route == "/tools/dream" {
			s.toolsDream(rr, req)
		} else {
			s.memoryDream(rr, req)
		}
		if rr.Code != http.StatusForbidden {
			t.Fatalf("transfer permission unlocked ordinary %s: %d", route, rr.Code)
		}
	}
}
