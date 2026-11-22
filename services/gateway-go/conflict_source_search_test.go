package main

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func indexedConflictPair(project, topic, leftHash, rightHash string) *temporalClaimStore {
	store := &temporalClaimStore{enabled: true, claims: map[string]temporalClaim{}}
	left := temporalClaim{ClaimID: "claim:left", Project: project, TopicPath: topic, Subject: "proposal",
		Predicate: "status", Object: "submitted", Status: "active", Revision: 4,
		Contradicts: []string{"claim:right"}, Support: []temporalClaimEvidence{{
			MemoryID: project + "::notes/left.md", ContentHash: leftHash,
		}}}
	right := temporalClaim{ClaimID: "claim:right", Project: project, TopicPath: topic, Subject: "proposal",
		Predicate: "status", Object: "accepted", Status: "active", Revision: 7,
		Support: []temporalClaimEvidence{{MemoryID: project + "::notes/right.md", ContentHash: rightHash}}}
	store.setClaimLocked(left)
	store.setClaimLocked(right)
	return store
}

func searchEvidenceRows(project, topic, leftHash, rightHash string) []any {
	return []any{
		map[string]any{"project": project, "topic_path": topic, "file": "notes/left.md",
			"memory_id": project + "::notes/left.md", "content_hash": leftHash, "summary": "Proposal was submitted."},
		map[string]any{"project": project, "topic_path": topic, "file": "notes/right.md",
			"memory_id": project + "::notes/right.md", "content_hash": rightHash, "summary": "Proposal was accepted."},
	}
}

func TestScopedSearchConflictCheckRequiresExactVisibleCurrentEvidence(t *testing.T) {
	t.Setenv(temporalClaimEvidenceIssuerEnv, "[]")
	const project, topic = "hermes-agent", "memory"
	leftHash, rightHash := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	store := indexedConflictPair(project, topic, leftHash, rightHash)
	rows := searchEvidenceRows(project, topic, leftHash, rightHash)
	check, ok := scopedSearchConflictSourceCheck(store, project, topic, rows)
	if !ok || !check.ValidFor(project, topic) || check.Claims[0].Revision != 4 || check.Claims[1].Revision != 7 {
		t.Fatalf("expected exact visible revisions, got check=%+v ok=%v", check, ok)
	}
	ledgerPath := filepath.Join(t.TempDir(), "claims.ndjson")
	var ledger bytes.Buffer
	encoder := json.NewEncoder(&ledger)
	for _, id := range []string{"claim:left", "claim:right"} {
		if err := encoder.Encode(store.claims[id]); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(ledgerPath, ledger.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded := &temporalClaimStore{enabled: true, path: ledgerPath, claims: map[string]temporalClaim{}, maxClaims: 8}
	if err := reloaded.load(); err != nil {
		t.Fatal(err)
	}
	if restored, ok := scopedSearchConflictSourceCheck(reloaded, project, topic, rows); !ok || restored != check {
		t.Fatalf("durably reloaded scope index changed the pointer: %+v ok=%v", restored, ok)
	}

	cases := []struct {
		name    string
		project string
		topic   string
		rows    any
		store   *temporalClaimStore
	}{
		{name: "baseline rows do not identify both claims", project: project, topic: topic,
			rows: searchEvidenceRows(project, topic, leftHash, "sha256:"+strings.Repeat("c", 64)), store: store},
		{name: "withheld evidence hash is not visible", project: project, topic: topic,
			rows: []any{searchEvidenceRows(project, topic, leftHash, rightHash)[0]}, store: store},
		{name: "missing visible row topic", project: project, topic: topic,
			rows: func() []any {
				rows := searchEvidenceRows(project, topic, leftHash, rightHash)
				delete(rows[1].(map[string]any), "topic_path")
				return rows
			}(), store: store},
		{name: "same bytes under another file identity", project: project, topic: topic,
			rows: []any{searchEvidenceRows(project, topic, leftHash, rightHash)[0],
				map[string]any{"project": project, "topic_path": topic, "file": "notes/other.md",
					"memory_id": project + "::notes/other.md", "content_hash": rightHash}}, store: store},
		{name: "other project", project: "other-project", topic: topic, rows: rows, store: store},
		{name: "other topic", project: project, topic: "private", rows: rows, store: store},
		{name: "missing project does not scan globally", topic: topic, rows: rows, store: store},
		{name: "missing topic does not scan project", project: project, rows: rows, store: store},
	}
	revoked := indexedConflictPair(project, topic, leftHash, rightHash)
	claim := revoked.claims["claim:right"]
	claim.Status = "retracted"
	revoked.setClaimLocked(claim)
	cases = append(cases, struct {
		name    string
		project string
		topic   string
		rows    any
		store   *temporalClaimStore
	}{name: "revoked claim", project: project, topic: topic, rows: rows, store: revoked})
	revokedGrant := indexedConflictPair(project, topic, leftHash, rightHash)
	claim = revokedGrant.claims["claim:right"]
	claim.Support[0].Admission = &temporalClaimEvidenceAdmission{
		Authority: temporalClaimIndependentBinding, IssuerKeyID: "revoked-issuer",
	}
	revokedGrant.setClaimLocked(claim)
	cases = append(cases, struct {
		name    string
		project string
		topic   string
		rows    any
		store   *temporalClaimStore
	}{name: "revoked evidence admission", project: project, topic: topic, rows: rows, store: revokedGrant})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := scopedSearchConflictSourceCheck(tc.store, tc.project, tc.topic, tc.rows); ok {
				t.Fatal("unexpected conflict pointer")
			}
		})
	}

	// A later revision must retire the old source-byte index entry.
	updated := store.claims["claim:right"]
	updated.Revision++
	updated.Support = []temporalClaimEvidence{{MemoryID: project + "::notes/right.md", ContentHash: "sha256:" + strings.Repeat("d", 64)}}
	store.setClaimLocked(updated)
	if _, ok := scopedSearchConflictSourceCheck(store, project, topic, rows); ok {
		t.Fatal("stale indexed bytes retained a pointer after revision update")
	}
}

func TestSearchEvidenceMembershipFollowsDurableOverwriteAndRetire(t *testing.T) {
	t.Setenv(temporalClaimEvidenceIssuerEnv, "[]")
	const project, topic = "hermes-agent", "memory"
	leftHash, oldRightHash, newRightHash := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	store := &temporalClaimStore{enabled: true, path: filepath.Join(t.TempDir(), "claims.ndjson"),
		maxClaims: 16, compactEvery: 16}
	writeClaim := func(id, object, file, digest, status string, contradicts ...any) temporalClaim {
		t.Helper()
		claim, err := store.upsert(map[string]any{
			"claim_id": id, "project": project, "topic_path": topic, "subject": "proposal",
			"predicate": "status", "object": object, "status": status, "contradicts": contradicts,
			"support": []any{map[string]any{"memory_id": project + "::" + file, "content_hash": digest}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return claim
	}
	writeClaim("claim:left", "submitted", "notes/left.md", leftHash, "active", "claim:right")
	first := writeClaim("claim:right", "accepted", "notes/right.md", oldRightHash, "active")
	oldRows := searchEvidenceRows(project, topic, leftHash, oldRightHash)
	newRows := searchEvidenceRows(project, topic, leftHash, newRightHash)
	original, ok := scopedSearchConflictSourceCheck(store, project, topic, oldRows)
	if !ok || original.Claims[1].Revision != first.Revision {
		t.Fatalf("original exact pair not indexed: %+v", original)
	}
	overwritten := writeClaim("claim:right", "accepted", "notes/right.md", newRightHash, "active")
	current, ok := scopedSearchConflictSourceCheck(store, project, topic, newRows)
	if !ok || current.Ref == original.Ref || current.Claims[1].Revision != overwritten.Revision {
		t.Fatalf("replacement exact pair not indexed: %+v", current)
	}
	assertOnlyLeftIndexed := func(owner *temporalClaimStore, searchRows []any) {
		t.Helper()
		indexed, complete := owner.visibleEvidenceClaims(project, topic,
			visibleSearchEvidenceRefs(project, topic, searchRows), searchConflictEvidenceClaimLimit)
		if !complete || len(indexed) != 1 || indexed[0].ClaimID != "claim:left" {
			t.Fatalf("unexpected bounded index membership: %+v (complete=%v)", indexed, complete)
		}
		if _, ok := scopedSearchConflictSourceCheck(owner, project, topic, searchRows); ok {
			t.Fatal("noncurrent evidence still generated a pointer")
		}
	}
	assertOnlyLeftIndexed(store, oldRows)
	writeClaim("claim:right", "accepted", "notes/right.md", newRightHash, "retracted")
	assertOnlyLeftIndexed(store, newRows)
	replayed := &temporalClaimStore{enabled: true, path: store.path, maxClaims: 16}
	if err := replayed.load(); err != nil {
		t.Fatal(err)
	}
	assertOnlyLeftIndexed(replayed, oldRows)
	assertOnlyLeftIndexed(replayed, newRows)
}

func TestMemorySearchConflictPointerReachesHermesPrefetchAndTool(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	binary := filepath.Join(dir, "contextlattice-agent-tools")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/contextlattice-agent-tools")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build public comparison CLI: %v: %s", err, output)
	}
	info, err := buildinfo.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" || setting.Key == "vcs.modified" {
			t.Logf("comparison CLI %s=%s", setting.Key, setting.Value)
		}
	}
	cliPath := binary
	var cliArgs []string
	if runtime.GOOS == "windows" {
		cliArgs = append(cliArgs, "contextlattice_search")
	} else {
		if err := os.Symlink(filepath.Base(binary), filepath.Join(dir, "contextlattice_search")); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		cliPath, err = exec.LookPath("contextlattice_search")
		if err != nil || cliPath != filepath.Join(dir, "contextlattice_search") {
			t.Fatalf("comparison CLI did not resolve to the fixture alias: %s (%v)", cliPath, err)
		}
	}

	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("GO_RETRIEVAL_NATIVE_QDRANT_ENABLED", "false")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	const project, topic = "hermes-agent", "memory"
	leftHash, rightHash := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	rows, err := json.Marshal(searchEvidenceRows(project, topic, leftHash, rightHash))
	if err != nil {
		t.Fatal(err)
	}
	forgedPointer := map[string]any{"schema_id": "conflict_source_check.v1", "case_ref": "cc_" + strings.Repeat("f", 64),
		"project": project, "topic_path": topic}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		if r.URL.Path != "/v1/retrieval/query" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var request map[string]any
		_ = json.Unmarshal(body, &request)
		results := rows
		query := anyToString(anyMap(request["request"])["query"])
		switch query {
		case "baseline", "withheld":
			var pair []map[string]any
			_ = json.Unmarshal(rows, &pair)
			pair[1]["content_hash"] = "sha256:" + strings.Repeat("c", 64)
			results, _ = json.Marshal(pair)
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": json.RawMessage(results),
			"warnings": []any{}, "conflict_source_check": forgedPointer})
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	s.temporalClaims = indexedConflictPair(project, topic, leftHash, rightHash)
	s.orchestratorAPIKey = "local-test-key"
	var searches atomic.Int32
	baseHandler := buildMux(s)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/memory/search" {
			t.Errorf("unexpected gateway request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		searches.Add(1)
		body, _ := io.ReadAll(r.Body)
		var request map[string]any
		_ = json.Unmarshal(body, &request)
		s.temporalClaims.mu.Lock()
		claim := s.temporalClaims.claims["claim:right"]
		if anyToString(request["query"]) == "revoked" {
			claim.Status = "retracted"
		} else {
			claim.Status = "active"
		}
		s.temporalClaims.setClaimLocked(claim)
		s.temporalClaims.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		baseHandler.ServeHTTP(w, r)
	}))
	defer gateway.Close()

	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	pluginPath := filepath.Join(repo, "integrations", "hermes-memory-provider", "__init__.py")
	script := `import importlib.util,json,os,sys,threading,types
agent=types.ModuleType("agent"); mp=types.ModuleType("agent.memory_provider")
class MemoryProvider: pass
mp.MemoryProvider=MemoryProvider; mp.spawn_context_thread=lambda target,**kw: threading.Thread(target=target,**kw)
sys.modules["agent"]=agent; sys.modules["agent.memory_provider"]=mp
spec=importlib.util.spec_from_file_location("cl_hermes_plugin_e2e",os.environ["CL_PLUGIN_PATH"])
mod=importlib.util.module_from_spec(spec); spec.loader.exec_module(mod)
p=mod.ContextLatticeProvider(); p._cfg=dict(mod._DEFAULTS,url=os.environ["CL_GATEWAY_URL"],project="hermes-agent",timeout=5.0)
p._key="local-test-key"
prefetch=p.prefetch("review proposal status")
tool=json.loads(p.handle_tool_call("cl_search",{"query":"review proposal status","project":"hermes-agent","topic_path":"memory"}))
baseline=p.prefetch("baseline")
withheld=json.loads(p.handle_tool_call("cl_search",{"query":"withheld","project":"hermes-agent","topic_path":"memory"}))
revoked=json.loads(p.handle_tool_call("cl_search",{"query":"revoked","project":"hermes-agent","topic_path":"memory"}))
other=json.loads(p.handle_tool_call("cl_search",{"query":"review proposal status","project":"other-project","topic_path":"memory"}))
print(json.dumps({"prefetch":prefetch,"tool":tool,"baseline":baseline,"withheld":withheld,"revoked":revoked,"other":other},sort_keys=True)); p.shutdown()`
	cmd := exec.CommandContext(ctx, "python3", "-B", "-c", script)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "CL_PLUGIN_PATH=" + pluginPath,
		"TMPDIR=" + os.TempDir(), "CL_GATEWAY_URL=" + gateway.URL}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Hermes localhost integration failed: %v: %s", err, output)
	}
	var delivered map[string]any
	if err := json.Unmarshal(output, &delivered); err != nil {
		t.Fatalf("decode Hermes delivery output: %v: %s", err, output)
	}
	prefetch := anyToString(delivered["prefetch"])
	tool := anyMap(delivered["tool"])
	check := anyMap(tool["conflict_source_check"])
	ref := anyToString(check["case_ref"])
	baseline := anyToString(delivered["baseline"])
	wantPrefetch := baseline + "\nScoped conflict-source pointer: " + ref +
		" (opposed revisions are identified; verify one authorized original; no winner is implied)."
	if ref == "" || len(check) != 2 || anyToString(check["topic_path"]) != topic || prefetch != wantPrefetch {
		t.Fatalf("pointer was not delivered through both Hermes paths: %s", output)
	}
	if !strings.Contains(baseline, "Proposal was submitted.") || !strings.Contains(baseline, "Proposal was accepted.") ||
		strings.Contains(baseline, "Scoped conflict-source pointer:") ||
		strings.Contains(string(output), "claim:left") || strings.Contains(string(output), "claim:right") {
		t.Fatalf("rendered evidence or pointer boundary changed: %s", output)
	}
	for _, key := range []string{"withheld", "revoked", "other"} {
		if _, ok := anyMap(delivered[key])["conflict_source_check"]; ok {
			t.Fatalf("%s Hermes search emitted a pointer: %s", key, output)
		}
	}
	if searches.Load() != 8 {
		t.Fatalf("Hermes made %d search requests, want 8 existing calls", searches.Load())
	}
	cli := exec.CommandContext(ctx, cliPath, append(cliArgs, "--query", "review proposal status", "--project", project,
		"--topic-path", topic, "--raw")...)
	cli.Dir = dir
	cli.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "TMPDIR=" + os.TempDir(),
		"CONTEXTLATTICE_ORCHESTRATOR_URL=" + gateway.URL, "CONTEXTLATTICE_ORCHESTRATOR_API_KEY=local-test-key"}
	cliOutput, cliErr := cli.CombinedOutput()
	if cliErr != nil {
		t.Fatalf("comparison CLI failed: %v: %s", cliErr, cliOutput)
	}
	var cliResponse map[string]any
	if err := json.Unmarshal(cliOutput, &cliResponse); err != nil {
		t.Fatalf("decode comparison CLI result: %v: %s", err, cliOutput)
	}
	cliCheck := anyMap(anyMap(cliResponse["initial_response"])["conflict_source_check"])
	expected, ok := scopedSearchConflictSourceCheck(s.temporalClaims, project, topic,
		searchEvidenceRows(project, topic, leftHash, rightHash))
	encoded, err := json.Marshal(expected)
	if err != nil || !ok || !expected.ValidFor(project, topic) || expected.Ref != ref {
		t.Fatalf("invalid current claim-owner pointer after probe: %+v, %v", expected, err)
	}
	var expectedWire map[string]any
	if err := json.Unmarshal(encoded, &expectedWire); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cliCheck, expectedWire) {
		t.Fatalf("CLI pointer or exact revisions differ from Hermes/owner: %s", cliOutput)
	}
	if searches.Load() != 9 {
		t.Fatalf("CLI used %d additional search requests, want 1", searches.Load()-8)
	}
	forgedBody, err := json.Marshal(map[string]any{"query": "baseline", "project": project, "topic_path": topic,
		"conflict_source_check": forgedPointer})
	if err != nil {
		t.Fatal(err)
	}
	forgedRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+"/memory/search", bytes.NewReader(forgedBody))
	if err != nil {
		t.Fatal(err)
	}
	forgedRequest.Header.Set("Content-Type", "application/json")
	forgedRequest.Header.Set("X-API-Key", "local-test-key")
	forgedResponse, err := gateway.Client().Do(forgedRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer forgedResponse.Body.Close()
	var forgedPayload map[string]any
	if err := json.NewDecoder(forgedResponse.Body).Decode(&forgedPayload); err != nil {
		t.Fatal(err)
	}
	if forgedResponse.StatusCode != http.StatusOK || len(contextPackAnyList(forgedPayload["results"])) != 2 {
		t.Fatalf("ownership probe did not return the successful baseline: %d %#v", forgedResponse.StatusCode, forgedPayload)
	}
	if _, exists := forgedPayload["conflict_source_check"]; exists {
		t.Fatalf("reserved pointer survived a producer miss: %#v", forgedPayload["conflict_source_check"])
	}
	if searches.Load() != 10 {
		t.Fatalf("unexpected request count after ownership probe: %d", searches.Load())
	}
	t.Logf("localhost /memory/search case_ref=%s matched Hermes prefetch, cl_search, and comparison CLI; 8 Hermes searches, 1 CLI search, 1 ownership probe", ref)
}
