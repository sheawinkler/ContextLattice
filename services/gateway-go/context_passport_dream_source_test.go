package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const dreamSourceExportTestRoute = "/memory/context-passport/export"
const dreamSourceExportTestKey = "fixture-source-service-key"

func dreamSourceExportFixtureGrant(t testing.TB, project, memoryID, eventID, contentHash string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "source-grant.json")
	raw, err := json.Marshal(map[string]string{
		"schema_id": dreamSourceExportAuthoritySchema, "project": project,
		"memory_id": memoryID, "event_id": eventID, "content_hash": contentHash,
		"expires_at": time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(dreamSourceExportFileEnv, path)
	return path
}

func TestDreamSourceExportNativeHTTP(t *testing.T) {
	testDreamSourceExportHTTP(t, true)
}

func TestDreamSourceExportHTTP(t *testing.T) {
	testDreamSourceExportHTTP(t, false)
}

func testDreamSourceExportHTTP(t *testing.T, viaCLI bool) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("source authority file requires the local POSIX owner gate")
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	binary := os.Getenv("CONTEXTLATTICE_TEST_AGENT_TOOLS_BINARY")
	if viaCLI && binary == "" {
		t.Skip("native source qualification requires the explicit source-built CLI")
	}
	if viaCLI && !filepath.IsAbs(binary) {
		t.Fatal("expected absolute native CLI path")
	}
	s := &server{orchestratorAPIKey: dreamSourceExportTestKey, memoryStore: newExactReadTestStore(t), contextPassports: newTestPassportStore(t, dir)}
	mux := http.NewServeMux()
	mux.HandleFunc("/memory/write", s.memoryWrite)
	mux.HandleFunc("/v1/memory/get", s.memoryV1Get)
	mux.HandleFunc(dreamSourceExportTestRoute, s.memoryContextPassportExport)
	gateway := httptest.NewServer(mux)
	defer gateway.Close()
	runCLI := func(args ...string) ([]byte, error) {
		t.Helper()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Dir = dir
		cmd.Env = []string{
			"HOME=" + dir, "CONTEXTLATTICE_GLOBAL_HOME=" + dir, "CONTEXTLATTICE_CLI_OUTPUT=raw",
			"CONTEXTLATTICE_ORCHESTRATOR_URL=" + gateway.URL,
			"CONTEXTLATTICE_ORCHESTRATOR_API_KEY=" + dreamSourceExportTestKey,
		}
		return cmd.CombinedOutput()
	}
	const project = "source-fixture"
	const file = "observations/cache-key.json"
	const memoryID = project + "::" + file
	const original = `{"schema_id":"dream_source_observation.v1","experience_ref":"exp:cache-key","status":"confirmed","mechanism":"cache by stable key","observed_result":"Duplicate work decreased in the synthetic queue."}`
	if viaCLI {
		if raw, err := runCLI("write", "--project", project, "--topic-path", "observations", "--file", file, "--content", original, "--raw"); err != nil {
			t.Fatalf("native write: %v: %s", err, raw)
		}
	} else if _, _, err := s.memoryStore.put(normalizedWrite{project: project, fileName: file, topicPath: "observations", content: original}); err != nil {
		t.Fatal(err)
	}
	get := func(id string) map[string]any {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, gateway.URL+"/v1/memory/get?memory_id="+url.QueryEscape(id), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Api-Key", dreamSourceExportTestKey)
		resp, err := gateway.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&body) != nil {
			t.Fatalf("raw memory GET failed: HTTP %d", resp.StatusCode)
		}
		return anyMap(body["memory"])
	}
	row := get(memoryID)
	eventID, hash := anyToString(row["event_id"]), anyToString(row["content_hash"])
	stored := original + "\n" // Native write terminates the stored content with a newline.
	if eventID == "" || hash != sha256Hex(stored) || row["content"] != stored {
		t.Fatalf("raw GET did not prove the exact source revision: %#v", row)
	}
	path := dreamSourceExportFixtureGrant(t, project, memoryID, eventID, hash)
	makePayload := func(id, event, digest string) []byte {
		t.Helper()
		data, err := json.Marshal(map[string]any{"project": project, "transfer_observation": map[string]string{
			"memory_id": id, "event_id": event, "content_hash": digest,
		}})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	valid := makePayload(memoryID, eventID, hash)
	post := func(t *testing.T, data []byte, key string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, gateway.URL+dreamSourceExportTestRoute, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if key != "" {
			req.Header.Set("X-Api-Key", key)
		}
		resp, err := gateway.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, body
	}
	var result map[string]json.RawMessage
	var published []byte
	if viaCLI {
		requestPath := filepath.Join(dir, "source.json")
		if err := os.WriteFile(requestPath, valid, 0600); err != nil {
			t.Fatal(err)
		}
		if raw, err := runCLI("passport-export", "do not synthesize", "--payload-file", requestPath); err == nil ||
			!bytes.Contains(raw, []byte("does not accept a query")) {
			t.Fatalf("native source export accepted a query: %v: %s", err, raw)
		}
		if raw, err := runCLI("passport-export", "--project", project); err == nil ||
			!bytes.Contains(raw, []byte("passport query is required")) {
			t.Fatalf("ordinary native export stopped requiring a query: %v: %s", err, raw)
		}
		artifact := filepath.Join(dir, "passport.json")
		raw, err := runCLI("passport-export", "--payload-file", requestPath, "--output", artifact, "--raw")
		if err != nil {
			t.Fatalf("native export: %v: %s", err, raw)
		}
		if json.Unmarshal(raw, &result) != nil || string(result["artifact_written"]) != "true" {
			t.Fatalf("native export did not return an artifact receipt: %s", raw)
		}
		published, err = os.ReadFile(artifact)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		status, raw := post(t, valid, dreamSourceExportTestKey)
		if status != http.StatusOK || json.Unmarshal(raw, &result) != nil {
			t.Fatalf("source export failed: status=%d body=%s", status, raw)
		}
		published = result["passport"]
	}
	if string(result["ok"]) != "true" || string(result["recorded"]) != "true" {
		t.Fatal("export was not explicitly recorded")
	}
	var passport contextPassport
	if err := strictJSONDecode(published, &passport); err != nil {
		t.Fatal(err)
	}
	if bad := verifyContextPassport(passport, time.Now().UTC(), true); len(bad) != 0 || passport.Project != project ||
		len(passport.Claims) != 1 || len(passport.Evidence) != 1 || len(passport.Replay) != 0 || len(passport.Objective) != 0 {
		t.Fatalf("unexpected scoped and signed passport: %v", bad)
	}
	claim := passport.Claims[0]
	provenance := passport.Evidence[0]
	encodedClaim, err := json.Marshal(claim)
	var receipt map[string]string
	if err != nil || json.Unmarshal(result["transfer_source"], &receipt) != nil ||
		claim["kind"] != "conditional_experience" || claim["status"] != "confirmed" ||
		claim["mechanism"] != "cache by stable key" || claim["observed_result"] != "Duplicate work decreased in the synthetic queue." ||
		len(anyToStringSlice(claim["evidence_refs"])) != 1 || anyToStringSlice(claim["evidence_refs"])[0] != provenance["evidence_id"] ||
		provenance["memory_id"] != memoryID || provenance["event_id"] != eventID || provenance["content_hash"] != hash ||
		receipt["source_passport_id"] != passport.PassportID || receipt["experience_ref"] != "exp:cache-key" ||
		receipt["source_record_hash"] != "sha256:"+sha256Hex(string(encodedClaim)) {
		t.Fatal("receipt, claim and original revision do not bind each other")
	}
	persisted, found := s.contextPassports.get(passport.PassportID)
	if !found || persisted.ContentDigest != passport.ContentDigest {
		t.Fatal("source gateway did not persist the signed passport")
	}

	assertDenied := func(t *testing.T, data []byte, key string) {
		t.Helper()
		status, body := post(t, data, key)
		if status != http.StatusForbidden || strings.TrimSpace(string(body)) != `{"error":"source_export_unavailable","ok":false}` {
			t.Fatalf("denial was not generic: status=%d body=%s", status, body)
		}
	}
	assertDenied(t, valid, "")
	assertDenied(t, makePayload(memoryID, eventID, strings.Repeat("0", 64)), dreamSourceExportTestKey)
	assertDenied(t, makePayload(memoryID, "unbound-event", hash), dreamSourceExportTestKey)
	assertDenied(t, []byte(`{"query":"no synthesis",`+string(valid[1:])), dreamSourceExportTestKey)
	assertDenied(t, []byte(strings.Replace(string(valid), `"event_id":`, `"status":"confirmed","event_id":`, 1)), dreamSourceExportTestKey)
	assertDenied(t, []byte(`{"project":"source-fixture",`+string(valid[1:])), dreamSourceExportTestKey)

	t.Run("grant pins wrong original hash", func(t *testing.T) {
		wrong := strings.Repeat("0", 64)
		dreamSourceExportFixtureGrant(t, project, memoryID, eventID, wrong)
		assertDenied(t, makePayload(memoryID, eventID, wrong), dreamSourceExportTestKey)
	})
	t.Run("invalid owner grant", func(t *testing.T) {
		for _, tc := range []struct {
			name, suffix string
		}{
			{"extra field", `,"operator":"not a grant field"}`},
			{"duplicate field", `,"project":"source-fixture"}`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				bad := dreamSourceExportFixtureGrant(t, project, memoryID, eventID, hash)
				raw, err := os.ReadFile(bad)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(bad, append(raw[:len(raw)-1], tc.suffix...), 0600); err != nil {
					t.Fatal(err)
				}
				assertDenied(t, valid, dreamSourceExportTestKey)
			})
		}
	})

	t.Run("project boundary denial under owner lock", func(t *testing.T) {
		gate, lock := optionalDreamTransferLocalProjects, optionalFrontierT1ProjectBoundaryLock
		t.Cleanup(func() { optionalDreamTransferLocalProjects, optionalFrontierT1ProjectBoundaryLock = gate, lock })
		held := false
		optionalFrontierT1ProjectBoundaryLock = func(*server) func() {
			held = true
			return func() { held = false }
		}
		optionalDreamTransferLocalProjects = func(_ *server, source, dest string) bool {
			if !held || source != project || dest != project {
				t.Error("project admission check escaped its scope or lock")
			}
			return false
		}
		assertDenied(t, valid, dreamSourceExportTestKey)
	})
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	assertDenied(t, valid, dreamSourceExportTestKey)

	t.Run("authorized target with no original", func(t *testing.T) {
		const missing = project + "::observations/unknown.json"
		dreamSourceExportFixtureGrant(t, project, missing, eventID, hash)
		assertDenied(t, makePayload(missing, eventID, hash), dreamSourceExportTestKey)
	})
	s.contextPassports.mu.RLock()
	numPassports := len(s.contextPassports.passports)
	s.contextPassports.mu.RUnlock()
	if numPassports != 1 {
		t.Fatalf("denied source requests created extra passports: %d", numPassports)
	}
	after := get(memoryID)
	if after["content"] != stored || after["content_hash"] != hash || after["event_id"] != eventID {
		t.Fatal("source export mutated the original observation")
	}
}
