package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func updateTestJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func updateTestMetadata(version string) updateMetadata {
	return updateMetadata{Schema: "contextlattice_release_payload.v2", Lane: "public", Tag: "v" + version, Commit: strings.Repeat("a", 40), SourceTree: strings.Repeat("b", 40), Version: version, Ref: "refs/tags/v" + version, Repository: updateRepository, SourceRef: "refs/heads/main"}
}

func updateTestInstall(t *testing.T) string {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "managed install")
	if err = os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, ".contextlattice-release.json"), updateTestJSON(t, updateTestMetadata("5.1.2")), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, ".env"), []byte("preserve-user-state"), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

type updateTestMember struct {
	name string
	kind byte
	data []byte
	link string
}

func updateTestTar(t *testing.T, members []updateTestMember) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, m := range members {
		h := &tar.Header{Name: m.name, Typeflag: m.kind, Mode: 0755, Size: int64(len(m.data)), Linkname: m.link}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(m.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func updateTestBundle(t *testing.T, metadata updateMetadata, nested []updateTestMember) []byte {
	t.Helper()
	b := updateTestJSON(t, metadata)
	if nested == nil {
		nested = []updateTestMember{{name: "contextlattice/.contextlattice-release.json", kind: tar.TypeReg, data: b}}
	}
	payload := updateTestTar(t, nested)
	h := sha256.Sum256(payload)
	return updateTestTar(t, []updateTestMember{
		{name: updateBundleRoot + "/", kind: tar.TypeDir},
		{name: updateBundleRoot + "/ContextLattice-Install.sh", kind: tar.TypeReg, data: []byte("fixture-never-executed")},
		{name: updateBundleRoot + "/ContextLattice-Monitor.sh", kind: tar.TypeReg, data: []byte("fixture")},
		{name: updateBundleRoot + "/README.txt", kind: tar.TypeReg, data: []byte("fixture")},
		{name: updateBundleRoot + "/payload/", kind: tar.TypeDir},
		{name: updateBundleRoot + "/payload/contextlattice-payload.tar.gz", kind: tar.TypeReg, data: payload},
		{name: updateBundleRoot + "/payload/contextlattice-payload.tar.gz.sha256", kind: tar.TypeReg, data: []byte(hex.EncodeToString(h[:]) + "  contextlattice-payload.tar.gz\n")},
		{name: updateBundleRoot + "/payload/contextlattice-release.json", kind: tar.TypeReg, data: b},
	})
}

// Only the test transport rewrites official origins to a local TLS fixture.
type updateTestTransport struct {
	server *url.URL
	inner  http.RoundTripper
}

func (x updateTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	u := *r.URL
	u.Scheme, u.Host = x.server.Scheme, x.server.Host
	clone.URL = &u
	return x.inner.RoundTrip(clone)
}

type updateTestFixture struct {
	u            managedUpdater
	release      updateRelease
	manifest     updateManifest
	metadata     updateMetadata
	files        map[string][]byte
	requests     []string
	commit       string
	manifestBody []byte
	redirect     string
}

func newUpdateTestFixture(t *testing.T) *updateTestFixture {
	t.Helper()
	return newUpdateTestFixtureVersion(t, "5.2.0")
}

func newUpdateTestFixtureVersion(t *testing.T, version string) *updateTestFixture {
	t.Helper()
	x := &updateTestFixture{metadata: updateTestMetadata(version), files: map[string][]byte{}, commit: strings.Repeat("a", 40)}
	x.release.Tag = x.metadata.Tag
	x.manifest = updateManifest{Schema: "contextlattice_release_artifact_manifest.v1", Lane: "public", Channel: "stable", Tag: x.metadata.Tag, Ref: x.metadata.Ref, Commit: x.commit, Count: 3}
	for i, name := range []string{updateBundleName, "ContextLattice-macOS-universal.dmg", "ContextLattice-windows-x64.msi"} {
		b := []byte("native-fixture")
		if name == updateBundleName {
			b = updateTestBundle(t, x.metadata, nil)
		}
		a := x.addAsset(name, b, int64(i+1))
		x.manifest.Artifacts = append(x.manifest.Artifacts, updateArtifact{Name: name, Size: a.Size, SHA256: strings.TrimPrefix(a.Digest, "sha256:")})
	}
	x.addAsset(updateManifestName, updateTestJSON(t, x.manifest), 4)
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x.requests = append(x.requests, r.URL.Path)
		if r.Header.Get("Authorization") != "" {
			t.Error("updater sent authorization")
		}
		switch r.URL.Path {
		case "/repos/" + updateRepository + "/releases/latest":
			w.Write(updateTestJSON(t, x.release))
		case "/repos/" + updateRepository + "/git/ref/tags/" + x.release.Tag:
			w.Write(updateTestJSON(t, map[string]any{"ref": "refs/tags/" + x.release.Tag, "object": map[string]string{"type": "commit", "sha": x.commit}}))
		default:
			if x.redirect != "" {
				http.Redirect(w, r, x.redirect, http.StatusFound)
				return
			}
			b, ok := x.files[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if strings.HasSuffix(r.URL.Path, updateManifestName) && x.manifestBody != nil {
				b = x.manifestBody
			}
			w.Write(b)
		}
	}))
	t.Cleanup(s.Close)
	base, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := s.Client()
	client.CheckRedirect = updateHTTPClient().CheckRedirect
	client.Transport = updateTestTransport{server: base, inner: client.Transport}
	x.u = managedUpdater{client: client, goos: "linux", invoke: func(context.Context, string, []string, []string, io.Writer) error {
		t.Fatal("unexpected installer invocation")
		return nil
	}}
	return x
}

func (x *updateTestFixture) addAsset(name string, b []byte, id int64) updateAsset {
	h := sha256.Sum256(b)
	a := updateAsset{ID: id, Name: name, Size: int64(len(b)), Digest: "sha256:" + hex.EncodeToString(h[:]), URL: "https://github.com/" + updateRepository + "/releases/download/" + x.release.Tag + "/" + name}
	x.release.Assets = append(x.release.Assets, a)
	x.files["/"+updateRepository+"/releases/download/"+x.release.Tag+"/"+name] = b
	return a
}

func updateTestCLI() (*cli, *bytes.Buffer) {
	var out bytes.Buffer
	return &cli{stdout: &out, stderr: io.Discard}, &out
}

func TestManagedUpdateCheckNeverMutates(t *testing.T) {
	root := updateTestInstall(t)
	before, err := os.ReadFile(filepath.Join(root, ".contextlattice-release.json"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	x := newUpdateTestFixture(t)
	c, out := updateTestCLI()
	if err := x.u.run(c, []string{"--check", "--install-dir", root}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"current":"5.1.2"`) || !strings.Contains(out.String(), `"latest":"5.2.0"`) || !strings.Contains(out.String(), `"applied":false`) {
		t.Fatal(out.String())
	}
	after, err := os.ReadFile(filepath.Join(root, ".contextlattice-release.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("check mutated install", err)
	}
	currentEntries, err := os.ReadDir(filepath.Dir(root))
	if err != nil || len(entries) != len(currentEntries) {
		t.Fatal("check created staging", err)
	}
	for _, r := range x.requests {
		if strings.HasSuffix(r, updateBundleName) {
			t.Fatal("check downloaded installer bundle")
		}
	}
}

func TestManagedUpdateApplyRequiresSupportedInstallerTarget(t *testing.T) {
	for _, scenario := range []struct {
		name, current, mode string
		noLaunch, refused   bool
	}{
		{"newer-legacy-apply", "5.0.2", "--apply", false, true},
		{"newer-legacy-file-only", "5.0.2", "--apply", true, true},
		{"legacy-check", "5.0.2", "--check", false, false},
		{"legacy-same-noop", "5.1.2", "--apply", false, false},
		{"legacy-older-noop", "5.2.0", "--apply", false, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			x := newUpdateTestFixtureVersion(t, "5.1.2")
			root := updateTestInstall(t)
			before := updateTestJSON(t, updateTestMetadata(scenario.current))
			if err := os.WriteFile(filepath.Join(root, ".contextlattice-release.json"), before, 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			x.u.invoke = func(context.Context, string, []string, []string, io.Writer) error {
				calls++
				return errors.New("unexpected invocation")
			}
			c, out := updateTestCLI()
			args := []string{scenario.mode, "--install-dir", root}
			if scenario.noLaunch {
				args = append(args, "--no-launch")
			}
			err := x.u.run(c, args)
			if scenario.refused {
				if err == nil || !strings.Contains(err.Error(), "requires target v5.2.0 or newer") {
					t.Fatal("legacy installer target was not refused", err)
				}
				if out.Len() != 0 {
					t.Fatal("refused apply emitted success", out.String())
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if calls != 0 {
				t.Fatal("legacy target invoked installer")
			}
			entries, err := os.ReadDir(filepath.Dir(root))
			if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(root) {
				t.Fatal("legacy target left staging/lock", err)
			}
			after, err := os.ReadFile(filepath.Join(root, ".contextlattice-release.json"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("legacy target mutated metadata", err)
			}
			for _, request := range x.requests {
				if strings.HasSuffix(request, updateBundleName) {
					t.Fatal("legacy target downloaded installer")
				}
			}
		})
	}
}

func TestManagedUpdateRefusesBeforeNetwork(t *testing.T) {
	for _, scenario := range []string{"paid", "unknown", "unmanaged", "checkout", "contradictory", "symlink", "windows-apply", "no-mode", "both-modes", "unknown-flag"} {
		t.Run(scenario, func(t *testing.T) {
			root := updateTestInstall(t)
			x := newUpdateTestFixture(t)
			m := updateTestMetadata("5.1.2")
			args := []string{"--apply", "--install-dir", root}
			switch scenario {
			case "paid", "unknown":
				m.Lane = scenario
				if err := os.WriteFile(filepath.Join(root, ".contextlattice-release.json"), updateTestJSON(t, m), 0600); err != nil {
					t.Fatal(err)
				}
			case "unmanaged":
				if err := os.Remove(filepath.Join(root, ".contextlattice-release.json")); err != nil {
					t.Fatal(err)
				}
			case "checkout":
				if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			case "contradictory":
				if err := os.MkdirAll(filepath.Join(root, "config", "runtime-license"), 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				link := filepath.Join(filepath.Dir(root), "link")
				if err := os.Symlink(root, link); err != nil {
					t.Fatal(err)
				}
				args = []string{"--check", "--install-dir", link}
			case "windows-apply":
				x.u.goos = "windows"
			case "no-mode":
				args = []string{"--install-dir", root}
			case "both-modes":
				args = append(args, "--check")
			case "unknown-flag":
				args = append(args, "--allow-paid-to-public-downgrade")
			}
			c, _ := updateTestCLI()
			if err := x.u.run(c, args); err == nil {
				t.Fatal("unsafe update accepted")
			}
			if len(x.requests) != 0 {
				t.Fatal("refused path fetched public data", x.requests)
			}
		})
	}
}

func TestManagedUpdateMetadataMismatches(t *testing.T) {
	for _, scenario := range []string{"digest", "size", "tag", "commit", "lane", "closed-name", "duplicate", "unofficial-url", "missing-digest", "draft", "oversized-json", "redirect-host"} {
		t.Run(scenario, func(t *testing.T) {
			x := newUpdateTestFixture(t)
			switch scenario {
			case "digest":
				x.release.Assets[3].Digest = "sha256:" + strings.Repeat("0", 64)
			case "size":
				x.release.Assets[3].Size++
			case "tag":
				x.manifest.Tag = "v5.3.0"
			case "commit":
				x.commit = strings.Repeat("c", 40)
			case "lane":
				x.manifest.Lane = "paid"
			case "closed-name":
				x.manifest.Artifacts[1].Name = "other.dmg"
			case "duplicate":
				x.release.Assets = append(x.release.Assets, x.release.Assets[3])
			case "unofficial-url":
				x.release.Assets[3].URL = "https://example.invalid/manifest.json"
			case "missing-digest":
				x.release.Assets[3].Digest = ""
			case "draft":
				x.release.Draft = true
			case "redirect-host":
				x.redirect = "https://example.invalid/asset"
			case "oversized-json":
				x.manifestBody = bytes.Repeat([]byte("x"), int(updateMaxJSON)+1)
			}
			if scenario == "tag" || scenario == "lane" || scenario == "closed-name" {
				b := updateTestJSON(t, x.manifest)
				h := sha256.Sum256(b)
				x.manifestBody = b
				x.release.Assets[3].Size = int64(len(b))
				x.release.Assets[3].Digest = "sha256:" + hex.EncodeToString(h[:])
			}
			c, _ := updateTestCLI()
			if err := x.u.run(c, []string{"--check", "--install-dir", updateTestInstall(t)}); err == nil {
				t.Fatal("mismatch accepted")
			}
		})
	}
}

func TestManagedUpdateArchiveAttacks(t *testing.T) {
	for _, scenario := range []string{"traversal", "symlink", "hardlink", "duplicate", "absolute", "unknown", "oversized", "member-count", "truncated", "nested-traversal", "nested-symlink", "metadata-mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			m := updateTestMetadata("5.2.0")
			members := []updateTestMember{{name: updateBundleRoot + "/README.txt", kind: tar.TypeReg, data: []byte("x")}}
			var archive []byte
			switch scenario {
			case "traversal":
				members[0].name = updateBundleRoot + "/../escape"
			case "symlink":
				members[0].kind = tar.TypeSymlink
				members[0].data = nil
				members[0].link = "/tmp/outside"
			case "hardlink":
				members[0].kind = tar.TypeLink
				members[0].data = nil
				members[0].link = "/tmp/outside"
			case "duplicate":
				members = append(members, members[0])
			case "absolute":
				members[0].name = "/tmp/outside"
			case "unknown":
				members[0].name = updateBundleRoot + "/unknown"
			case "nested-traversal":
				archive = updateTestBundle(t, m, []updateTestMember{{name: "contextlattice/../escape", kind: tar.TypeReg}})
			case "nested-symlink":
				archive = updateTestBundle(t, m, []updateTestMember{{name: "contextlattice/link", kind: tar.TypeSymlink, link: "/tmp/outside"}})
			case "metadata-mismatch":
				m.Commit = strings.Repeat("d", 40)
				archive = updateTestBundle(t, m, nil)
			}
			if archive == nil {
				archive = updateTestTar(t, members)
			}
			if scenario == "truncated" {
				archive = archive[:len(archive)-6]
			}
			if scenario == "oversized" || scenario == "member-count" {
				limit, count := int64(1024), 256
				if scenario == "oversized" {
					limit = 1
				}
				if scenario == "member-count" {
					count = 0
				}
				if err := updateWalkTar(bytes.NewReader(archive), updateBundleRoot, limit, count, func(*tar.Header, io.Reader) error { return nil }); err == nil {
					t.Fatal("archive limit ignored")
				}
				return
			}
			stage := t.TempDir()
			file := filepath.Join(stage, "bundle.tar.gz")
			if err := os.WriteFile(file, archive, 0600); err != nil {
				t.Fatal(err)
			}
			manifest := updateManifest{Tag: "v5.2.0", Commit: strings.Repeat("a", 40)}
			if err := updateExtract(file, stage, manifest); err == nil {
				t.Fatal("archive attack accepted")
			}
		})
	}
}

func TestManagedUpdateDelegatesAndPropagatesFailure(t *testing.T) {
	for _, scenario := range []string{"success", "failure", "false-success", "same-version", "older-release", "corrupt-bundle"} {
		t.Run(scenario, func(t *testing.T) {
			x := newUpdateTestFixture(t)
			root := updateTestInstall(t)
			calls := 0
			if scenario == "same-version" || scenario == "older-release" {
				version := "5.2.0"
				if scenario == "older-release" {
					version = "5.3.0"
				}
				if err := os.WriteFile(filepath.Join(root, ".contextlattice-release.json"), updateTestJSON(t, updateTestMetadata(version)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "corrupt-bundle" {
				x.files["/"+updateRepository+"/releases/download/"+x.release.Tag+"/"+updateBundleName] = []byte("corrupt")
			}
			x.u.invoke = func(_ context.Context, program string, args, env []string, _ io.Writer) error {
				calls++
				want := []string{"--install-dir", root, "--no-browser"}
				if !reflect.DeepEqual(args, want) || filepath.Base(program) != "ContextLattice-Install.sh" {
					t.Fatal("unsafe invocation", program, args)
				}
				if filepath.Dir(filepath.Dir(filepath.Dir(program))) != filepath.Dir(root) {
					t.Fatal("staging not beside install")
				}
				for _, e := range env {
					name, _, _ := strings.Cut(e, "=")
					switch name {
					case "PATH", "HOME", "TMPDIR", "FULL_MODE", "DOCKER_CONTEXT", "DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY", "CONTEXTLATTICE_GLOBAL_HOME", "GOCACHE", "GOMODCACHE", "GOPATH", "GOROOT", "GOTOOLCHAIN":
					default:
						t.Fatal("unexpected installer environment name", name)
					}
				}
				if scenario == "failure" {
					return errors.New("fixture-exit-23")
				}
				if scenario == "false-success" {
					return nil
				}
				return os.WriteFile(filepath.Join(root, ".contextlattice-release.json"), updateTestJSON(t, x.metadata), 0600)
			}
			c, out := updateTestCLI()
			err := x.u.run(c, []string{"--apply", "--install-dir", root})
			wantFailure := scenario == "failure" || scenario == "false-success" || scenario == "corrupt-bundle"
			if (err != nil) != wantFailure {
				t.Fatalf("scenario=%s err=%v", scenario, err)
			}
			if scenario == "failure" && !strings.Contains(err.Error(), "fixture-exit-23") {
				t.Fatal(err)
			}
			wantCalls := 1
			if scenario == "same-version" || scenario == "older-release" || scenario == "corrupt-bundle" {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatal("wrong invoke count", calls)
			}
			if scenario == "success" && !strings.Contains(out.String(), `"applied":true`) {
				t.Fatal(out.String())
			}
			state, err := os.ReadFile(filepath.Join(root, ".env"))
			if err != nil || string(state) != "preserve-user-state" {
				t.Fatal("user state changed", err)
			}
			entries, err := os.ReadDir(filepath.Dir(root))
			if err != nil || len(entries) != 1 {
				t.Fatal("staging not cleaned", err)
			}
		})
	}
}

func TestManagedUpdateApplyModesAndRefreshWarning(t *testing.T) {
	for _, scenario := range []string{"normal", "no-launch", "skip-agent-tools", "both", "refresh-warning"} {
		t.Run(scenario, func(t *testing.T) {
			x := newUpdateTestFixture(t)
			root := updateTestInstall(t)
			t.Setenv("GOCACHE", "fixture-reserved-cache")
			t.Setenv("DOCKER_CONTEXT", "fixture-context")
			noLaunch := scenario == "no-launch" || scenario == "both"
			skipTools := scenario == "skip-agent-tools" || scenario == "both"
			args := []string{"--apply", "--install-dir", root}
			want := []string{"--install-dir", root, "--no-browser"}
			if noLaunch {
				args = append(args, "--no-launch")
				want = append(want, "--no-launch")
			}
			if skipTools {
				args = append(args, "--skip-agent-tools")
				want = append(want, "--skip-agent-tools")
			}
			calls := 0
			x.u.invoke = func(ctx context.Context, _ string, argv, env []string, output io.Writer) error {
				calls++
				if !reflect.DeepEqual(argv, want) {
					t.Fatal("incorrect installer mode", argv, want)
				}
				if _, deadline := ctx.Deadline(); deadline {
					t.Fatal("HTTP deadline applied to installer transaction")
				}
				if !strings.Contains(strings.Join(env, "\n"), "GOCACHE=fixture-reserved-cache") || !strings.Contains(strings.Join(env, "\n"), "DOCKER_CONTEXT=fixture-context") {
					t.Fatal("selected cache/context not preserved")
				}
				if scenario == "refresh-warning" {
					if _, err := io.WriteString(output, "WARN: global agent tools "); err != nil {
						return err
					}
					if _, err := io.WriteString(output, "were not installed.\n"); err != nil {
						return err
					}
				}
				return os.WriteFile(filepath.Join(root, ".contextlattice-release.json"), updateTestJSON(t, x.metadata), 0600)
			}
			c, out := updateTestCLI()
			var diagnostics bytes.Buffer
			c.stderr = &diagnostics
			if err := x.u.run(c, args); err != nil {
				t.Fatal(err)
			}
			var result map[string]any
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || result["runtime_verified"] != !noLaunch || result["runtime_started"] != !noLaunch || result["restart_required"] != noLaunch {
				t.Fatal("untruthful runtime result", result)
			}
			refresh := "delegated_unverified"
			if noLaunch || skipTools {
				refresh = "skipped"
			}
			if scenario == "refresh-warning" {
				refresh = "failed"
			}
			if result["cli_refresh_status"] != refresh || result["current"] != "5.2.0" {
				t.Fatal("untruthful CLI/current result", result)
			}
			if scenario == "refresh-warning" && (len(asList(result["warnings"])) == 0 || !strings.Contains(diagnostics.String(), "WARN: global agent tools were not installed.")) {
				t.Fatal("refresh warning not reported", result)
			}
		})
	}
}

func TestManagedUpdateURLAndVersionPolicy(t *testing.T) {
	for _, raw := range []string{"http://github.com/x", "https://github.com.evil.invalid/x", "https://user@github.com/x", "https://github.com:443/x", "https://example.invalid/x", "https://github.com/x#fragment"} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if updateAllowedURL(u) {
			t.Fatal("unsafe URL", raw)
		}
	}
	for _, tag := range []string{"v5.2.0-paid", "v5.2.0-rc.1", "5.2.0", "v05.2.0", "v5.2.0/../../x"} {
		if _, err := updateVersion(tag); err == nil {
			t.Fatal("unsafe version", tag)
		}
	}
	if version, err := updateVersion("v5.2.0-public"); err != nil || version != "5.2.0" {
		t.Fatal(version, err)
	}
}

func TestManagedUpdateJSONAmbiguity(t *testing.T) {
	for _, raw := range []string{`{"lane":"paid","lane":"public"}`, `{"lane":"paid","Lane":"public"}`, `{"\u017fchema_id":"x"}`, `{"a":{"b":1,"b":2}}`, `{} {}`, strings.Repeat("[", 34) + strings.Repeat("]", 34)} {
		var value any
		if err := updateDecodeJSON([]byte(raw), &value); err == nil {
			t.Fatal("ambiguous JSON accepted", raw)
		}
	}
}

func TestManagedUpdateRootDefaultsAndWindowsCheck(t *testing.T) {
	root := updateTestInstall(t)
	t.Setenv("CONTEXTLATTICE_INSTALL_DIR", root)
	x := newUpdateTestFixture(t)
	x.u.goos = "windows"
	c, out := updateTestCLI()
	if err := x.u.run(c, []string{"--check"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"current":"5.1.2"`) {
		t.Fatal(out.String())
	}
	// An explicit unmanaged path must not fall back to the valid environment root.
	c, _ = updateTestCLI()
	if err := x.u.run(c, []string{"--check", "--install-dir", t.TempDir()}); err == nil {
		t.Fatal("explicit root ignored")
	}
	t.Setenv("CONTEXTLATTICE_INSTALL_DIR", "")
	t.Chdir(root)
	if err := x.u.run(c, []string{"--check"}); err != nil {
		t.Fatal("managed cwd not resolved", err)
	}
}

func TestManagedUpdateConcurrentApplyLock(t *testing.T) {
	root := updateTestInstall(t)
	lock := filepath.Join(filepath.Dir(root), "."+filepath.Base(root)+".update-lock")
	if err := os.Mkdir(lock, 0700); err != nil {
		t.Fatal(err)
	}
	x := newUpdateTestFixture(t)
	c, _ := updateTestCLI()
	if err := x.u.run(c, []string{"--apply", "--install-dir", root}); err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatal("existing lock ignored", err)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatal("another updater lock removed", err)
	}
	for _, request := range x.requests {
		if strings.HasSuffix(request, updateBundleName) {
			t.Fatal("locked updater downloaded bundle")
		}
	}
}

func TestManagedUpdateDispatchPreservesContextCommands(t *testing.T) {
	c, out := updateTestCLI()
	if err := c.run([]string{"contextlattice", "update", "--unknown-update-option"}); err == nil || strings.Contains(err.Error(), "unknown contextlattice agent tool command") {
		t.Fatal("update not dispatched", err)
	}
	if err := c.run([]string{"contextlattice", "help"}); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"update --check", "context <task>", "resume [--session-id id]", "remember <checkpoint>", "finish <summary>"} {
		if !strings.Contains(out.String(), command) {
			t.Fatal("existing command missing", command)
		}
	}
}
