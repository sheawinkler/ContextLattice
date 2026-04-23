package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkillsSearchStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "skills-search-stub.py")
	content := `#!/usr/bin/env python3
import json,sys
print(json.dumps({"argv": sys.argv[1:]}))
`
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

func TestSkillsQuarantineSearchRouteReturnsParsedJSON(t *testing.T) {
	t.Setenv("ORCH_SKILLS_QUARANTINE_ENABLED", "true")
	t.Setenv("ORCH_SKILLS_QUARANTINE_SEARCH_CMD", writeSkillsSearchStub(t))
	t.Setenv("ORCH_SKILLS_QUARANTINE_TIMEOUT_SECS", "3")
	t.Setenv("ORCH_SKILLS_QUARANTINE_DEFAULT_LIMIT", "20")
	t.Setenv("ORCH_SKILLS_QUARANTINE_MAX_LIMIT", "100")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	req, err := http.NewRequest(
		http.MethodGet,
		gateway.URL+"/v1/skills/quarantine/search?query=rust+graph&limit=7&min_score=15&show_terms=true&json=true",
		nil,
	)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("search request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if !anyToBool(payload["ok"]) {
		t.Fatalf("expected ok=true payload, got %#v", payload)
	}
	parsed, ok := payload["parsed"].(map[string]any)
	if !ok {
		t.Fatalf("expected parsed json in response, got %#v", payload["parsed"])
	}
	argv, ok := parsed["argv"].([]any)
	if !ok {
		t.Fatalf("expected argv array, got %#v", parsed["argv"])
	}
	joined := make([]string, 0, len(argv))
	for _, item := range argv {
		joined = append(joined, strings.TrimSpace(anyToString(item)))
	}
	joinedArgs := strings.Join(joined, " ")
	required := []string{"--limit", "7", "--show-terms", "--min-score", "15", "--json", "rust graph"}
	for _, token := range required {
		if !strings.Contains(joinedArgs, token) {
			t.Fatalf("expected token %q in argv %q", token, joinedArgs)
		}
	}
}

func TestSkillsQuarantineReindexDisabledByDefault(t *testing.T) {
	t.Setenv("ORCH_SKILLS_QUARANTINE_ENABLED", "true")
	t.Setenv("ORCH_SKILLS_QUARANTINE_REINDEX_ENABLED", "false")
	t.Setenv("ORCH_SKILLS_QUARANTINE_REINDEX_CMD", writeSkillsSearchStub(t))

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	req, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/skills/quarantine/reindex", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("reindex request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}
}
