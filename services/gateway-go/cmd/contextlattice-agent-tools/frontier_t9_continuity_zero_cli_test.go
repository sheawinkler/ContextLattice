package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func frontierT9CLITestGit(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	commands := [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "continuity-zero@example.invalid"},
		{"config", "user.name", "Continuity Zero Test"},
		{"remote", "add", "origin", "git@github.com:sheawinkler/ContextLattice.git"},
	}
	for _, args := range commands {
		command := exec.Command("git", append([]string{"-C", root}, args...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("continuity\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-m", "seed"}} {
		command := exec.Command("git", append([]string{"-C", root}, args...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
		}
	}
	return root
}

func frontierT9CLITestResponse() map[string]any {
	return map[string]any{
		"ok": true, "schema_id": "continuity_zero.v1", "decision": "ready",
		"manifest_id":     "czero_0123456789abcdef01234567",
		"manifest_digest": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"format_contract": map[string]any{
			"schema_id": "continuity_zero.v1", "validation": map[string]any{"status": "passed"},
		},
	}
}

func TestFrontierT9ContinuityZeroCLIAutoDetectsGitWithoutTransmittingLocalPaths(t *testing.T) {
	repo := frontierT9CLITestGit(t)
	var captured map[string]any
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != frontierT9CLIContinuityZeroPath {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(frontierT9CLITestResponse())
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice", "continuity-zero", "--project", "contextlattice", "--agent", "codex", "--agent-id", "codex_gpt5", "--cwd", repo, "--raw"}); err != nil {
		t.Fatalf("run continuity-zero CLI: %v", err)
	}
	if captured["repository_id"] != "sheawinkler/ContextLattice" || captured["branch"] != "main" || len(firstString(captured["commit"])) != 40 {
		t.Fatalf("git identity was not auto-detected: %#v", captured)
	}
	encoded, _ := json.Marshal(captured)
	if bytes.Contains(encoded, []byte(repo)) || bytes.Contains(encoded, []byte("/Users/")) || bytes.Contains(encoded, []byte("/Volumes/")) {
		t.Fatalf("CLI transmitted a local path: %s", encoded)
	}
	if _, ok := nativeToolNames["contextlattice_continuity_zero"]; !ok {
		t.Fatal("canonical continuity-zero executable alias is missing")
	}
}

func TestFrontierT9ContinuityZeroCLIRedactsRemoteCredentialsBeforeTransport(t *testing.T) {
	repo := frontierT9CLITestGit(t)
	credentialedRemote := "https://operator:secret-token-value@github.com/sheawinkler/ContextLattice.git"
	command := exec.Command("git", "-C", repo, "remote", "set-url", "origin", credentialedRemote)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("set credentialed remote: %v: %s", err, output)
	}
	var captured map[string]any
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(frontierT9CLITestResponse())
	}))
	defer gateway.Close()
	c := newCLI(&bytes.Buffer{}, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice", "continuity-zero", "--project", "contextlattice", "--cwd", repo, "--raw"}); err != nil {
		t.Fatalf("run continuity-zero CLI: %v", err)
	}
	encoded, _ := json.Marshal(captured)
	for _, forbidden := range []string{"operator", "secret-token-value", "https://", repo, "/Users/", "/Volumes/"} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("CLI transported forbidden repository material %q: %s", forbidden, encoded)
		}
	}
	if captured["repository_id"] != "sheawinkler/ContextLattice" {
		t.Fatalf("sanitized repository identity=%#v", captured["repository_id"])
	}
}

func TestFrontierT9ContinuityZeroCLIWritesOwnerOnlyArtifact(t *testing.T) {
	repo := frontierT9CLITestGit(t)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(frontierT9CLITestResponse())
	}))
	defer gateway.Close()
	output := filepath.Join(t.TempDir(), "continuity-zero.json")
	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice_continuity_zero", "--project", "contextlattice", "--cwd", repo, "--output", output, "--raw"}); err != nil {
		t.Fatalf("write owner-only artifact: %v", err)
	}
	info, err := os.Lstat(output)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("artifact mode=%s want regular 0600", info.Mode())
	}
	var artifact map[string]any
	raw, err := os.ReadFile(output)
	if err != nil || json.Unmarshal(raw, &artifact) != nil || artifact["schema_id"] != "continuity_zero.v1" {
		t.Fatalf("invalid owner-only artifact err=%v body=%s", err, raw)
	}
	var summary map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil || !asBool(summary["artifact_written"]) || summary["manifest_id"] == "" {
		t.Fatalf("invalid CLI artifact summary err=%v body=%s", err, stdout.String())
	}
}

func TestFrontierT9ContinuityZeroCLIRejectsUnpassedContract(t *testing.T) {
	repo := frontierT9CLITestGit(t)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		response := frontierT9CLITestResponse()
		asMap(asMap(response["format_contract"])["validation"])["status"] = "failed"
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer gateway.Close()
	c := newCLI(&bytes.Buffer{}, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice", "continuity-zero", "--project", "contextlattice", "--cwd", repo, "--raw"}); err == nil {
		t.Fatal("CLI accepted an unpassed continuity-zero contract")
	}
}
