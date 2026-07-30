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

func TestSkillsIndexRootsUseNativePathListSeparator(t *testing.T) {
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	t.Setenv("ORCH_SKILLS_INDEX_ROOTS", first+string(os.PathListSeparator)+second)
	roots := skillsIndexRoots()
	if len(roots) != 2 || roots[0] != filepath.Clean(first) || roots[1] != filepath.Clean(second) {
		t.Fatalf("unexpected native path-list parsing: %#v", roots)
	}
}

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

func writeSkillIndexFixture(t *testing.T, root string, dirName string, name string, description string) string {
	t.Helper()
	dir := filepath.Join(root, dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir skill fixture: %v", err)
	}
	path := filepath.Join(dir, "SKILL.md")
	body := "---\nname: " + name + "\ndescription: " + description + "\ntags: [contextlattice, agent]\n---\n\n" + description + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write skill fixture: %v", err)
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

func TestSkillsIndexSearchAliasReturnsParsedJSON(t *testing.T) {
	t.Setenv("ORCH_SKILLS_QUARANTINE_ENABLED", "true")
	activeRoot := t.TempDir()
	quarantineRoot := t.TempDir()
	activeSkill := writeSkillIndexFixture(t, activeRoot, "objective-loop", "objective-loop", "Use when an agent needs to stay focused through an objective loop.")
	writeSkillIndexFixture(t, quarantineRoot, "vendor-loop", "vendor-loop", "Use when searching quarantined vendor skills.")
	t.Setenv("ORCH_SKILLS_INDEX_ROOTS", activeRoot+":"+quarantineRoot)
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
		http.MethodPost,
		gateway.URL+"/v1/skills/index/search",
		strings.NewReader(`{"query":"objective loop focus","limit":4,"show_terms":false,"json":true}`),
	)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("search alias request failed: %v", err)
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
	if strings.TrimSpace(anyToString(payload["index"])) != "native_active_skills" {
		t.Fatalf("expected native skills index payload, got %#v", payload["index"])
	}
	results, _ := payload["results"].([]any)
	if len(results) == 0 {
		t.Fatalf("expected native skill results, got %#v", payload)
	}
	first, _ := results[0].(map[string]any)
	if strings.TrimSpace(anyToString(first["name"])) != "objective-loop" {
		t.Fatalf("expected active objective-loop first, got %#v", first)
	}
	if strings.TrimSpace(anyToString(first["path"])) != activeSkill {
		t.Fatalf("expected active skill path %s, got %#v", activeSkill, first["path"])
	}
}

func TestSkillsIndexReindexReturnsNativeRootStatus(t *testing.T) {
	t.Setenv("ORCH_SKILLS_QUARANTINE_ENABLED", "true")
	activeRoot := t.TempDir()
	writeSkillIndexFixture(t, activeRoot, "objective", "objective", "Use when an agent needs objective focus.")
	t.Setenv("ORCH_SKILLS_INDEX_ROOTS", activeRoot)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	req, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/skills/index/reindex", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("reindex alias request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if !anyToBool(payload["ok"]) || strings.TrimSpace(anyToString(payload["reindex_mode"])) != "live_native_scan" {
		t.Fatalf("expected live native scan status, got %#v", payload)
	}
}

func TestSkillsIndexFiltersGenericStopwords(t *testing.T) {
	root := t.TempDir()
	writeSkillIndexFixture(t, root, "wallet-token", "wallet-token", "Inspect token balances and wallet state.")
	t.Setenv("ORCH_SKILLS_INDEX_ROOTS", root)

	payload := nativeSkillsIndexSearch(skillsQuarantineSearchRequest{
		Query: "skill index audit for agents", Limit: 20, JSON: true, ShowTerms: true,
	})
	if anyToInt(payload["total_matches"], -1) != 0 {
		t.Fatalf("generic stopword query returned unrelated skills: %#v", payload["results"])
	}
	if len(contextPackAnyList(payload["discriminating_terms"])) != 0 {
		t.Fatalf("generic terms survived stopword filtering: %#v", payload["discriminating_terms"])
	}
	if len(contextPackAnyList(payload["warnings"])) == 0 {
		t.Fatalf("expected explicit no-discriminating-terms warning: %#v", payload)
	}
}

func TestSkillsIndexRequiresMinimumConceptCoverage(t *testing.T) {
	root := t.TempDir()
	writeSkillIndexFixture(t, root, "objective-only", "objective-only", "Keep work focused on an objective.")
	writeSkillIndexFixture(t, root, "objective-release", "objective-release", "Verify an objective before a release.")
	t.Setenv("ORCH_SKILLS_INDEX_ROOTS", root)
	t.Setenv("ORCH_SKILLS_INDEX_MIN_TERM_COVERAGE", "0.5")

	payload := nativeSkillsIndexSearch(skillsQuarantineSearchRequest{
		Query: "objective release deployment", Limit: 20, JSON: true,
	})
	results := contextPackAnyList(payload["results"])
	if len(results) != 1 || anyToString(anyMap(results[0])["name"]) != "objective-release" {
		t.Fatalf("coverage gate did not reject one-of-three concept match: %#v", results)
	}
	if got := anyToFloat(anyMap(results[0])["coverage"]); got < 0.66 || got > 0.67 {
		t.Fatalf("unexpected concept coverage %v in %#v", got, results[0])
	}
}

func TestSkillsIndexDeduplicatesDigestAndPreservesHarnessProvenance(t *testing.T) {
	base := t.TempDir()
	codexRoot := filepath.Join(base, "skills_active")
	hermesRoot := filepath.Join(base, "skills_hermes")
	ultraRoot := filepath.Join(base, "skills_hermes_ultra")
	sharedRoot := filepath.Join(base, "skills_shared_agents")
	writeSkillIndexFixture(t, codexRoot, "verified-release", "verified-release", "Verify release evidence deterministically.")
	writeSkillIndexFixture(t, hermesRoot, "verified-release-copy", "verified-release", "Verify release evidence deterministically.")
	writeSkillIndexFixture(t, ultraRoot, "runtime-proof", "runtime-proof", "Verify runtime release identity.")
	writeSkillIndexFixture(t, sharedRoot, "release-checkpoint", "release-checkpoint", "Record verified release checkpoints.")
	t.Setenv("ORCH_SKILLS_INDEX_ROOTS", strings.Join(
		[]string{codexRoot, hermesRoot, ultraRoot, sharedRoot},
		string(os.PathListSeparator),
	))

	payload := nativeSkillsIndexSearch(skillsQuarantineSearchRequest{
		Query: "verified release", Limit: 20, JSON: true,
	})
	results := contextPackAnyList(payload["results"])
	if len(results) != 3 || anyToInt(payload["total_candidates"], 0) != 4 {
		t.Fatalf("digest dedupe mismatch: candidates=%v results=%#v", payload["total_candidates"], results)
	}
	var duplicate map[string]any
	harnesses := map[string]bool{}
	for _, raw := range results {
		result := anyMap(raw)
		harnesses[anyToString(result["harness"])] = true
		if anyToInt(result["duplicate_count"], 0) == 1 {
			duplicate = result
		}
	}
	if duplicate == nil || anyToString(duplicate["root"]) != codexRoot ||
		anyToString(duplicate["harness"]) != "codex" ||
		!strings.HasPrefix(anyToString(duplicate["digest"]), "sha256:") {
		t.Fatalf("canonical duplicate identity mismatch: %#v", duplicate)
	}
	provenance := contextPackAnyList(duplicate["provenance"])
	if len(provenance) != 2 ||
		anyToString(anyMap(provenance[0])["harness"]) != "codex" ||
		anyToString(anyMap(provenance[1])["harness"]) != "hermes" {
		t.Fatalf("cross-harness provenance was not preserved: %#v", provenance)
	}
	for _, harness := range []string{"codex", "hermes_agent_ultra", "shared_agents"} {
		if !harnesses[harness] {
			t.Fatalf("missing indexed harness %q in %#v", harness, results)
		}
	}
}

func TestSkillsIndexRootStatsCountAllSeenSkills(t *testing.T) {
	root := t.TempDir()
	writeSkillIndexFixture(t, root, "matching", "matching", "Verify a deployment release.")
	writeSkillIndexFixture(t, root, "unrelated", "unrelated", "Analyze a wallet balance.")
	t.Setenv("ORCH_SKILLS_INDEX_ROOTS", root)

	payload := nativeSkillsIndexSearch(skillsQuarantineSearchRequest{
		Query: "deployment release", Limit: 20, JSON: true,
	})
	roots := contextPackAnyList(payload["roots"])
	if len(roots) != 1 {
		t.Fatalf("unexpected root stats: %#v", roots)
	}
	stat := anyMap(roots[0])
	if anyToInt(stat["skills_seen"], 0) != 2 || anyToInt(stat["matches"], 0) != 1 {
		t.Fatalf("root inventory conflated scanned skills with matches: %#v", stat)
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
