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
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	updateRepository         = "sheawinkler/ContextLattice"
	updateAPI                = "https://api.github.com/repos/" + updateRepository
	updateBundleName         = "ContextLattice-linux-bootstrap.tar.gz"
	updateManifestName       = "ContextLattice-release-manifest-stable.json"
	updateBundleRoot         = "ContextLattice-linux-bootstrap"
	updateMaxJSON      int64 = 1 << 20
	updateMaxBundle    int64 = 256 << 20
	updateMaxExpanded  int64 = 1 << 30
)

type updateMetadata struct {
	Schema     string `json:"schema_id"`
	Lane       string `json:"lane"`
	Tag        string `json:"tag"`
	Commit     string `json:"commit"`
	SourceTree string `json:"source_tree"`
	Version    string `json:"runtime_version"`
	Ref        string `json:"release_ref"`
	Repository string `json:"approved_source_repository"`
	SourceRef  string `json:"approved_source_ref"`
}

type updateAsset struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
	URL    string `json:"browser_download_url"`
}

type updateRelease struct {
	Tag        string        `json:"tag_name"`
	Draft      bool          `json:"draft"`
	Prerelease bool          `json:"prerelease"`
	Assets     []updateAsset `json:"assets"`
}

type updateArtifact struct {
	Name   string `json:"name"`
	Size   int64  `json:"size_bytes"`
	SHA256 string `json:"sha256"`
}

type updateManifest struct {
	Schema    string           `json:"schema_id"`
	Lane      string           `json:"lane"`
	Channel   string           `json:"channel"`
	Tag       string           `json:"tag"`
	Ref       string           `json:"release_ref"`
	Commit    string           `json:"commit"`
	Count     int              `json:"artifact_count"`
	Artifacts []updateArtifact `json:"artifacts"`
}

// Dependencies are instance-scoped: tests never replace process-global HTTP or exec hooks.
type managedUpdater struct {
	client *http.Client
	goos   string
	invoke func(context.Context, string, []string, []string, io.Writer) error
}

func (c *cli) cmdUpdate(args []string) error {
	u := managedUpdater{client: updateHTTPClient(), goos: runtime.GOOS, invoke: func(ctx context.Context, program string, args, env []string, output io.Writer) error {
		cmd := exec.CommandContext(ctx, program, args...)
		cmd.Env, cmd.Stdout, cmd.Stderr = env, output, output
		return cmd.Run()
	}}
	return u.run(c, args)
}

func updateHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 2 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 4 || !updateAllowedURL(req.URL) {
				return errors.New("release redirect refused")
			}
			return nil
		},
	}
}

func updateAllowedURL(u *url.URL) bool {
	if u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Fragment != "" {
		return false
	}
	switch u.Host {
	case "api.github.com", "github.com", "release-assets.githubusercontent.com":
		return true
	}
	return false
}

func (u managedUpdater) run(c *cli, args []string) error {
	f := flag.NewFlagSet("update", flag.ContinueOnError)
	f.SetOutput(c.stderr)
	check := f.Bool("check", false, "report current/latest without writes or installer execution")
	apply := f.Bool("apply", false, "update managed public install through normal installer launch/readiness/tool refresh")
	noLaunch := f.Bool("no-launch", false, "apply files only; runtime restart/readiness remains required")
	skipTools := f.Bool("skip-agent-tools", false, "do not refresh global CLI tools/hooks/profiles")
	root := f.String("install-dir", "", "existing managed install (or CONTEXTLATTICE_INSTALL_DIR, or managed cwd)")
	pretty := f.Bool("pretty", false, "indented JSON")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 || *check == *apply {
		return errors.New("use contextlattice update --check OR --apply [--install-dir PATH]")
	}
	explicitRoot := false
	f.Visit(func(f *flag.Flag) {
		if f.Name == "install-dir" {
			explicitRoot = true
		}
	})
	if explicitRoot && *root == "" {
		return errors.New("--install-dir must not be empty")
	}
	if *root == "" {
		*root = os.Getenv("CONTEXTLATTICE_INSTALL_DIR")
	}
	if *root == "" {
		var err error
		*root, err = os.Getwd()
		if err != nil {
			return errors.New("cannot resolve managed cwd; pass --install-dir")
		}
	}
	install, current, err := updateManagedRoot(*root)
	if err != nil {
		return err
	}
	if *apply && u.goos != "linux" && u.goos != "darwin" {
		return errors.New("this platform is check-only; use its native installer to apply updates")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var release updateRelease
	if err = u.getJSON(ctx, updateAPI+"/releases/latest", &release); err != nil {
		return err
	}
	if release.Draft || release.Prerelease || len(release.Assets) > 64 {
		return errors.New("latest release is not a bounded stable release")
	}
	latest, err := updateVersion(release.Tag)
	if err != nil {
		return err
	}
	manifestAsset, err := updateFindAsset(release, updateManifestName, updateMaxJSON)
	if err != nil {
		return err
	}
	var manifestBytes bytes.Buffer
	if err = u.download(ctx, manifestAsset, &manifestBytes); err != nil {
		return err
	}
	var manifest updateManifest
	if err = updateDecodeJSON(manifestBytes.Bytes(), &manifest); err != nil {
		return errors.New("invalid release manifest JSON")
	}
	commit, err := u.tagCommit(ctx, release.Tag)
	if err != nil {
		return err
	}
	if err = updateValidateManifest(manifest, release, commit); err != nil {
		return err
	}
	newer := updateNewer(latest, current.Version)
	result := map[string]any{
		"schema_id": "contextlattice_cli_update.v1", "lane": "public",
		"current": current.Version, "current_tag": current.Tag,
		"latest": latest, "latest_tag": release.Tag, "update_available": newer,
		"applied": false, "runtime_started": false, "runtime_verified": false,
		"restart_required": nil, "cli_refresh_status": "not_attempted",
		"apply_supported": u.goos == "linux" || u.goos == "darwin",
	}
	if *check || !newer {
		return c.emit(result, *pretty)
	}
	bundle, err := updateFindAsset(release, updateBundleName, updateMaxBundle)
	if err != nil {
		return err
	}
	lock := filepath.Join(filepath.Dir(install), "."+filepath.Base(install)+".update-lock")
	if err = os.Mkdir(lock, 0700); err != nil {
		return errors.New("cannot acquire managed update lock; inspect concurrent updater or stale lock beside install")
	}
	defer os.Remove(lock)
	// Both updater staging and the installer's TMPDIR stay beside the managed root.
	stage, err := os.MkdirTemp(filepath.Dir(install), ".contextlattice-update-")
	if err != nil {
		return errors.New("cannot stage beside managed install")
	}
	defer os.RemoveAll(stage)
	archivePath := filepath.Join(stage, "bundle.tar.gz")
	archive, err := os.OpenFile(archivePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = u.download(ctx, bundle, archive)
	closeErr := archive.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = updateExtract(archivePath, stage, manifest); err != nil {
		return err
	}
	// Re-read identity immediately before delegating. The installer owns replacement/recovery.
	_, now, err := updateManagedRoot(install)
	if err != nil {
		return err
	}
	if now != current {
		return errors.New("managed metadata changed during update; retry after inspection")
	}
	program := filepath.Join(stage, updateBundleRoot, "ContextLattice-Install.sh")
	installerArgs := []string{"--install-dir", install, "--no-browser"}
	if *noLaunch {
		installerArgs = append(installerArgs, "--no-launch")
	}
	if *skipTools {
		installerArgs = append(installerArgs, "--skip-agent-tools")
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TMPDIR=" + stage}
	// Preserve explicitly selected Docker/tool/cache homes, not credentials or shell startup env.
	for _, name := range []string{"FULL_MODE", "DOCKER_CONTEXT", "DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY", "CONTEXTLATTICE_GLOBAL_HOME", "GOCACHE", "GOMODCACHE", "GOPATH", "GOROOT", "GOTOOLCHAIN"} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	diagnostics := &updateInstallerDiagnostics{destination: c.stderr}
	// HTTP deadlines must not SIGKILL the installer's file-publication transaction.
	if err = u.invoke(context.Background(), program, installerArgs, env, diagnostics); err != nil {
		return fmt.Errorf("verified installer failed: %w; inspect its diagnostics and retained transaction; no automatic runtime rollback is claimed", err)
	}
	_, installed, err := updateManagedRoot(install)
	if err != nil {
		return err
	}
	if installed.Tag != release.Tag || installed.Commit != manifest.Commit || installed.Version != latest {
		return errors.New("installer returned success but installed identity is unproven")
	}
	result["applied"] = true
	result["previous"] = current.Version
	result["current"], result["current_tag"] = installed.Version, installed.Tag
	result["update_available"] = false
	result["runtime_started"], result["runtime_verified"] = !*noLaunch, !*noLaunch
	result["restart_required"] = *noLaunch
	if !*noLaunch {
		result["runtime_verified_by"] = "release_installer_source_bound_readiness"
	}
	switch {
	case *noLaunch || *skipTools:
		result["cli_refresh_status"] = "skipped"
	case diagnostics.agentToolsWarning:
		result["cli_refresh_status"] = "failed"
		result["warnings"] = []string{"Global agent tool refresh failed; CLI replacement is unproven. Inspect installer diagnostics."}
	default:
		// The installer does not return an independent binary identity receipt.
		result["cli_refresh_status"] = "delegated_unverified"
	}
	return c.emit(result, *pretty)
}

type updateInstallerDiagnostics struct {
	destination       io.Writer
	tail              string
	agentToolsWarning bool
}

func (w *updateInstallerDiagnostics) Write(p []byte) (int, error) {
	const warning = "WARN: global agent tools were not installed."
	n, err := w.destination.Write(p)
	text := w.tail + string(p[:n])
	if strings.Contains(text, warning) {
		w.agentToolsWarning = true
	}
	if len(text) >= len(warning) {
		text = text[len(text)-len(warning)+1:]
	}
	w.tail = strings.Clone(text)
	return n, err
}

func updateVersion(tag string) (string, error) {
	v := strings.TrimSuffix(strings.TrimPrefix(tag, "v"), "-public")
	if !strings.HasPrefix(tag, "v") || !regexp.MustCompile(`^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`).MatchString(v) {
		return "", errors.New("release tag is not a stable vX.Y.Z[-public] version")
	}
	return v, nil
}

func updateNewer(a, b string) bool {
	aa, bb := strings.Split(a, "."), strings.Split(b, ".")
	for i := range aa {
		x, _ := strconv.Atoi(aa[i])
		y, _ := strconv.Atoi(bb[i])
		if x != y {
			return x > y
		}
	}
	return false
}

func updateHex(s string, n int) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b)*2 == n && strings.ToLower(s) == s
}

func updateValidateMetadata(m updateMetadata) error {
	v, err := updateVersion(m.Tag)
	if err != nil || m.Schema != "contextlattice_release_payload.v2" || m.Ref != "refs/tags/"+m.Tag || m.Version != v || !updateHex(m.Commit, 40) || !updateHex(m.SourceTree, 40) || m.Repository != updateRepository || m.SourceRef != "refs/heads/main" {
		return errors.New("unmanaged or inconsistent public release metadata")
	}
	return nil
}

func updateReadRegular(name string, limit int64) ([]byte, error) {
	i, err := os.Lstat(name)
	if err != nil || !i.Mode().IsRegular() || i.Size() <= 0 || i.Size() > limit {
		return nil, errors.New("release metadata/checksum is missing, nonregular, or oversized")
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(i, opened) {
		return nil, errors.New("metadata identity changed while opening")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("metadata read exceeded bound")
	}
	return b, nil
}

func updateManagedRoot(input string) (string, updateMetadata, error) {
	var m updateMetadata
	abs, err := filepath.Abs(input)
	if err != nil {
		return "", m, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil || resolved != abs || filepath.Dir(abs) == abs {
		return "", m, errors.New("install root must be an existing nonsymlink, non-root managed directory")
	}
	if _, err := os.Lstat(filepath.Join(abs, ".git")); err == nil || !os.IsNotExist(err) {
		return "", m, errors.New("repository checkout is not a managed install")
	}
	b, err := updateReadRegular(filepath.Join(abs, ".contextlattice-release.json"), 4096)
	if err != nil {
		return "", m, err
	}
	if err = updateDecodeJSON(b, &m); err != nil {
		return "", m, errors.New("invalid installed metadata")
	}
	if m.Lane == "paid" {
		return "", m, errors.New("this install uses the paid channel; use the matching authenticated download/update path; public updates refused")
	}
	if m.Lane != "public" {
		return "", m, errors.New("unsupported or ambiguous installed release lane")
	}
	if err = updateValidateMetadata(m); err != nil {
		return "", m, err
	}
	if _, err := os.Lstat(filepath.Join(abs, "config", "runtime-license")); err == nil || !os.IsNotExist(err) {
		return "", m, errors.New("public metadata contradicts paid runtime files")
	}
	entries, err := os.ReadDir(filepath.Join(abs, "services", "gateway-go"))
	if err != nil && !os.IsNotExist(err) {
		return "", m, err
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "_entitled") {
			return "", m, errors.New("public metadata contradicts paid runtime files")
		}
	}
	return abs, m, nil
}

func (u managedUpdater) get(ctx context.Context, raw string) (*http.Response, error) {
	parsed, err := url.Parse(raw)
	if err != nil || !updateAllowedURL(parsed) {
		return nil, errors.New("release URL refused")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ContextLattice-managed-updater")
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, errors.New("release HTTPS request failed")
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("release request returned HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

func (u managedUpdater) getJSON(ctx context.Context, raw string, target any) error {
	resp, err := u.get(ctx, raw)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, updateMaxJSON+1))
	if err != nil || int64(len(b)) > updateMaxJSON {
		return errors.New("release JSON exceeded bound or failed")
	}
	if updateDecodeJSON(b, target) != nil {
		return errors.New("invalid release JSON")
	}
	return nil
}

// Duplicate keys are ambiguous across the Go reader and the shipped shell installer.
func updateDecodeJSON(b []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 32 {
			return errors.New("release JSON exceeds nesting bound")
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		delim, container := token.(json.Delim)
		if !container {
			return nil
		}
		if delim != '{' && delim != '[' {
			return errors.New("invalid release JSON container")
		}
		keys := map[string]bool{}
		for d.More() {
			if delim == '{' {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || name != strings.ToLower(name) || keys[name] {
					return errors.New("release JSON contains duplicate/invalid keys")
				}
				for _, ch := range name {
					if ch > 127 {
						return errors.New("release JSON keys must be ASCII")
					}
				}
				keys[name] = true
			}
			if err := walk(depth + 1); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("release JSON has trailing data")
	}
	return json.Unmarshal(b, target)
}

func updateFindAsset(r updateRelease, name string, max int64) (updateAsset, error) {
	var found updateAsset
	count := 0
	for _, a := range r.Assets {
		if a.Name == name {
			found = a
			count++
		}
	}
	expected := "https://github.com/" + updateRepository + "/releases/download/" + r.Tag + "/" + name
	if count != 1 || found.ID <= 0 || found.Size <= 0 || found.Size > max || !strings.HasPrefix(found.Digest, "sha256:") || !updateHex(strings.TrimPrefix(found.Digest, "sha256:"), 64) || found.URL != expected {
		return found, errors.New("release asset has ambiguous name, missing SHA-256, invalid size, or unofficial URL")
	}
	return found, nil
}

func (u managedUpdater) download(ctx context.Context, a updateAsset, out io.Writer) error {
	resp, err := u.get(ctx, a.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.ContentLength >= 0 && resp.ContentLength != a.Size {
		return errors.New("release asset Content-Length mismatch")
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(resp.Body, a.Size+1))
	if err != nil || n != a.Size || "sha256:"+hex.EncodeToString(h.Sum(nil)) != a.Digest {
		return errors.New("release asset digest/size mismatch")
	}
	return nil
}

func (u managedUpdater) tagCommit(ctx context.Context, tag string) (string, error) {
	var ref struct {
		Ref    string `json:"ref"`
		Object struct {
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"object"`
	}
	if err := u.getJSON(ctx, updateAPI+"/git/ref/tags/"+tag, &ref); err != nil {
		return "", err
	}
	if ref.Ref != "refs/tags/"+tag {
		return "", errors.New("release tag ref mismatch")
	}
	for depth := 0; depth < 4; depth++ {
		if !updateHex(ref.Object.SHA, 40) {
			return "", errors.New("invalid release tag object")
		}
		if ref.Object.Type == "commit" {
			return ref.Object.SHA, nil
		}
		if ref.Object.Type != "tag" {
			break
		}
		var annotated struct {
			Object struct {
				Type string `json:"type"`
				SHA  string `json:"sha"`
			} `json:"object"`
		}
		if err := u.getJSON(ctx, updateAPI+"/git/tags/"+ref.Object.SHA, &annotated); err != nil {
			return "", err
		}
		ref.Object = annotated.Object
	}
	return "", errors.New("unsupported or excessive annotated tag chain")
}

func updateValidateManifest(m updateManifest, r updateRelease, commit string) error {
	if (m.Schema != "contextlattice_release_artifact_manifest.v1" && m.Schema != "contextlattice_release_manifest.v2") || m.Lane != "public" || m.Channel != "stable" || m.Tag != r.Tag || m.Ref != "refs/tags/"+r.Tag || m.Commit != commit || !updateHex(commit, 40) || m.Count != 3 || len(m.Artifacts) != 3 {
		return errors.New("release manifest identity/lane/tag/commit/count mismatch")
	}
	seen := map[string]bool{}
	for _, row := range m.Artifacts {
		switch row.Name {
		case updateBundleName, "ContextLattice-macOS-universal.dmg", "ContextLattice-windows-x64.msi":
		default:
			return errors.New("release manifest contains an unknown artifact")
		}
		if seen[row.Name] || !updateHex(row.SHA256, 64) {
			return errors.New("release manifest duplicate or invalid digest")
		}
		seen[row.Name] = true
		a, err := updateFindAsset(r, row.Name, 2<<30)
		if err != nil {
			return err
		}
		if row.Size != a.Size || "sha256:"+row.SHA256 != a.Digest {
			return errors.New("manifest and GitHub asset digest/size disagree")
		}
	}
	return nil
}

// Validate before extraction into a private staging directory, never into the install root.
func updateWalkTar(source io.Reader, prefix string, maxBytes int64, maxMembers int, visit func(*tar.Header, io.Reader) error) error {
	gz, err := gzip.NewReader(source)
	if err != nil {
		return errors.New("invalid release gzip")
	}
	defer gz.Close()
	bounded := &io.LimitedReader{R: gz, N: maxBytes + 1}
	tr := tar.NewReader(bounded)
	seen := map[string]bool{}
	for count := 0; ; count++ {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.New("invalid or oversized release tar")
		}
		name := strings.TrimSuffix(h.Name, "/")
		if count >= maxMembers || len(name) > 512 || name != path.Clean(name) || (name != prefix && !strings.HasPrefix(name, prefix+"/")) || strings.ContainsAny(name, "\\:\x00\r\n") || seen[name] || h.Size < 0 || h.Size > maxBytes || len(h.PAXRecords) != 0 || len(h.Xattrs) != 0 || (h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir) {
			return errors.New("unsafe release archive path/type/duplicate/size/member count")
		}
		seen[name] = true
		if err := visit(h, tr); err != nil {
			return err
		}
	}
	// Consume padding and gzip trailer too: truncated streams and expansion bombs fail closed.
	_, err = io.Copy(io.Discard, bounded)
	if err != nil || bounded.N <= 0 {
		return errors.New("release gzip checksum or expanded-size mismatch")
	}
	return nil
}

func updateExtract(archivePath, stage string, manifest updateManifest) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	allowed := map[string]bool{"ContextLattice-Install.sh": true, "ContextLattice-Monitor.sh": true, "README.txt": true, "payload/contextlattice-payload.tar.gz": true, "payload/contextlattice-payload.tar.gz.sha256": true, "payload/contextlattice-release.json": true}
	seen := map[string]bool{}
	err = updateWalkTar(f, updateBundleRoot, updateMaxBundle, 256, func(h *tar.Header, content io.Reader) error {
		rel := strings.TrimPrefix(strings.TrimSuffix(h.Name, "/"), updateBundleRoot)
		rel = strings.TrimPrefix(rel, "/")
		if h.Typeflag == tar.TypeDir {
			if rel != "" && rel != "payload" {
				return errors.New("unknown bundle directory")
			}
			return os.MkdirAll(filepath.Join(stage, updateBundleRoot, filepath.FromSlash(rel)), 0700)
		}
		if !allowed[rel] || h.Size > 192<<20 {
			return errors.New("unknown or oversized bundle member")
		}
		seen[rel] = true
		name := filepath.Join(stage, updateBundleRoot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
			return err
		}
		out, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, content)
		closeErr := out.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if rel == "ContextLattice-Install.sh" {
			return os.Chmod(name, 0700)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(seen) != len(allowed) {
		return errors.New("bundle is missing required files")
	}
	payloadDir := filepath.Join(stage, updateBundleRoot, "payload")
	b, err := updateReadRegular(filepath.Join(payloadDir, "contextlattice-release.json"), 4096)
	if err != nil {
		return err
	}
	var m updateMetadata
	if updateDecodeJSON(b, &m) != nil {
		return errors.New("invalid bundle metadata")
	}
	if m.Lane != "public" || m.Tag != manifest.Tag || m.Commit != manifest.Commit {
		return errors.New("bundle and manifest identity mismatch")
	}
	if err = updateValidateMetadata(m); err != nil {
		return err
	}
	sum, err := updateReadRegular(filepath.Join(payloadDir, "contextlattice-payload.tar.gz.sha256"), 256)
	if err != nil {
		return err
	}
	fields := strings.Fields(string(sum))
	if len(fields) != 2 || !updateHex(fields[0], 64) || fields[1] != "contextlattice-payload.tar.gz" {
		return errors.New("invalid payload checksum")
	}
	payload, err := os.Open(filepath.Join(payloadDir, fields[1]))
	if err != nil {
		return err
	}
	defer payload.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, payload); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != fields[0] {
		return errors.New("embedded payload checksum mismatch")
	}
	if _, err = payload.Seek(0, io.SeekStart); err != nil {
		return err
	}
	metadataFound := false
	err = updateWalkTar(payload, "contextlattice", updateMaxExpanded, 100000, func(h *tar.Header, content io.Reader) error {
		if h.Name == "contextlattice/.contextlattice-release.json" {
			if h.Typeflag != tar.TypeReg || h.Size != int64(len(b)) {
				return errors.New("embedded metadata is invalid")
			}
			embedded, err := io.ReadAll(io.LimitReader(content, 4097))
			if err != nil || !bytes.Equal(b, embedded) {
				return errors.New("embedded metadata differs from bundle")
			}
			metadataFound = true
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !metadataFound {
		return errors.New("embedded release metadata missing")
	}
	return nil
}
