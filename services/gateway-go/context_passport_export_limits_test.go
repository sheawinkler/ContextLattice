package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestContextPassportExportBoundsUnknownLengthBody(t *testing.T) {
	const prefix = "{\"project\":\"alpha\",\"transfer_observation\":{\"memory_id\":\""
	body := prefix + strings.Repeat("x", contextPassportMaxItemBytes+4096) + "\"}}"
	stream := strings.NewReader(body)
	req := httptest.NewRequest(http.MethodPost, "/memory/context-passport/export", io.NopCloser(stream))
	if req.ContentLength != -1 {
		t.Fatal("fixture must present an unknown-length stream")
	}
	recorder := httptest.NewRecorder()
	(&server{}).memoryContextPassportExport(recorder, req)
	if recorder.Code != http.StatusRequestEntityTooLarge || !strings.Contains(recorder.Body.String(), "request_body_too_large") {
		t.Fatalf("oversized export returned %d: %s", recorder.Code, recorder.Body.String())
	}
	if n := len(body) - stream.Len(); n != contextPassportMaxItemBytes+1 {
		t.Fatalf("oversized export consumed %d bytes, want limit plus one", n)
	}
}

func TestContextPassportExportPreservesOrdinaryRequests(t *testing.T) {
	const route = "/memory/context-passport/export"
	for _, body := range []io.Reader{nil, strings.NewReader(" \n\t")} {
		req := httptest.NewRequest(http.MethodPost, route, body)
		recorder := httptest.NewRecorder()
		(&server{}).memoryContextPassportExport(recorder, req)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "project_and_query_required") {
			t.Fatalf("empty export returned %d: %s", recorder.Code, recorder.Body.String())
		}
	}

	t.Setenv("GO_RETRIEVAL_NATIVE_QDRANT_ENABLED", "false")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", sourceQdrant)
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", sourceQdrant)
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/retrieval/query" {
			t.Errorf("unexpected backend route: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": []any{map[string]any{
			"project": "alpha", "file": "notes/cache.md", "source": sourceQdrant, "score": 0.92,
			"summary": "Cache by stable key to avoid duplicate work.",
		}}, "warnings": []any{}})
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	s.contextPassports = newTestPassportStore(t, t.TempDir())
	payload := "{\"project\":\"alpha\",\"query\":\"cache stable key\"}"
	req := httptest.NewRequest(http.MethodPost, route, strings.NewReader(payload))
	recorder := httptest.NewRecorder()
	s.memoryContextPassportExport(recorder, req)
	var result map[string]any
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &result) != nil ||
		result["recorded"] != true || result["replayable"] != true || result["transfer_source"] != nil {
		t.Fatalf("ordinary export returned %d: %s", recorder.Code, recorder.Body.String())
	}
	passport, err := decodeContextPassport(result["passport"])
	if err != nil || passport.Project != "alpha" || passport.Replay["query"] != "cache stable key" ||
		len(verifyContextPassport(passport, time.Now().UTC(), true)) != 0 {
		t.Fatal("ordinary export did not retain its signed replay scope")
	}
}
