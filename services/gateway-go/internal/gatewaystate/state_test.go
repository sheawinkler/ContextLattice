package gatewaystate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func clearStateRootEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{RootEnv, "GO_MEMORY_STORE_ROOT", "MEMORY_BANK_ROOT", "CONTEXTLATTICE_GLOBAL_HOME"} {
		t.Setenv(name, "")
	}
}

func TestResolveRootPrefersCanonicalThenCompatibilityRoots(t *testing.T) {
	clearStateRootEnv(t)
	explicit := filepath.Join(t.TempDir(), "canonical")
	t.Setenv(RootEnv, explicit)
	t.Setenv("GO_MEMORY_STORE_ROOT", filepath.Join(t.TempDir(), "memory"))
	root := ResolveRoot()
	if root.Path != explicit || root.SourceEnv != RootEnv || root.Source != "explicit_state_root" {
		t.Fatalf("canonical root must win: %#v", root)
	}

	t.Setenv(RootEnv, "")
	memoryRoot := filepath.Join(t.TempDir(), "memory")
	t.Setenv("GO_MEMORY_STORE_ROOT", memoryRoot)
	root = ResolveRoot()
	want := filepath.Join(memoryRoot, "_contextlattice")
	if root.Path != want || root.Source != "memory_store_compatibility" {
		t.Fatalf("memory compatibility root mismatch: got=%#v want=%s", root, want)
	}
}

func TestResolvePathRootsLegacyDefaultsAndPreservesExplicitOverrides(t *testing.T) {
	clearStateRootEnv(t)
	root := filepath.Join(t.TempDir(), "state")
	t.Setenv(RootEnv, root)

	resolved := ResolvePath([]string{"SURFACE_PATH"}, filepath.Join(".data", "orchestrator", "agent_sessions.json"))
	if resolved.Path != filepath.Join(root, "agent_sessions.json") || resolved.Override {
		t.Fatalf("legacy default did not resolve beneath canonical root: %#v", resolved)
	}

	explicit := filepath.Join(t.TempDir(), "override.json")
	t.Setenv("SURFACE_PATH", explicit)
	resolved = ResolvePath([]string{"SURFACE_PATH"}, filepath.Join("services", "orchestrator", "data", "agent_sessions.json"))
	if resolved.Path != explicit || !resolved.Override || resolved.SourceEnv != "SURFACE_PATH" {
		t.Fatalf("explicit surface override was not preserved: %#v", resolved)
	}
}

func TestResolvePathIsIndependentOfWorkingDirectory(t *testing.T) {
	clearStateRootEnv(t)
	root := filepath.Join(t.TempDir(), "state")
	t.Setenv(RootEnv, root)
	firstCWD := t.TempDir()
	secondCWD := t.TempDir()
	originalCWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	defer func() {
		if err := os.Chdir(originalCWD); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	}()
	if err := os.Chdir(firstCWD); err != nil {
		t.Fatalf("chdir first: %v", err)
	}
	first := ResolvePath(nil, filepath.Join(".data", "orchestrator", "feedback.ndjson"))
	if err := os.Chdir(secondCWD); err != nil {
		t.Fatalf("chdir second: %v", err)
	}
	second := ResolvePath(nil, filepath.Join(".data", "orchestrator", "feedback.ndjson"))
	if first.Path != second.Path || first.Path != filepath.Join(root, "feedback.ndjson") {
		t.Fatalf("working directory changed canonical resolution: first=%#v second=%#v", first, second)
	}
}

func TestRelativeCanonicalRootIsRejectedWithoutCreatingState(t *testing.T) {
	clearStateRootEnv(t)
	t.Setenv(RootEnv, filepath.Join("relative", "gateway-state"))
	root := ResolveRoot()
	if filepath.IsAbs(root.Path) || root.Warning == "" {
		t.Fatalf("relative canonical root must remain explicit and unhealthy: %#v", root)
	}
	if _, err := EnsureRoot(); err == nil {
		t.Fatal("relative canonical root unexpectedly passed startup validation")
	}
}

func TestFailureClassDistinguishesPermissionFromPathErrors(t *testing.T) {
	if got := failureClass(os.ErrPermission); got != "permission_denied" {
		t.Fatalf("permission failure class=%q", got)
	}
	if got := failureClass(os.ErrNotExist); got != "path_error" {
		t.Fatalf("ordinary path failure class=%q", got)
	}
	if got := failureClass(errors.New("configuration mismatch")); got != "path_error" {
		t.Fatalf("configuration failure class=%q", got)
	}
}

func TestEnsureRootAndInventoryAreNonMutatingForEntries(t *testing.T) {
	clearStateRootEnv(t)
	rootPath := filepath.Join(t.TempDir(), "state")
	t.Setenv(RootEnv, rootPath)
	root, err := EnsureRoot()
	if err != nil {
		t.Fatalf("ensure root: %v", err)
	}
	if root.Path != rootPath {
		t.Fatalf("unexpected root: %#v", root)
	}
	entryPath := filepath.Join(rootPath, "future", "sessions.json")
	payload := Inventory([]EntryInput{{
		ID: "agent_sessions", Path: entryPath, Source: "state_root", StorageTier: root.StorageTier,
		Kind: "file", PersistenceClass: "durable_file",
	}})
	if payload["ok"] != true {
		t.Fatalf("expected healthy inventory: %#v", payload)
	}
	if _, err := os.Stat(entryPath); !os.IsNotExist(err) {
		t.Fatalf("inventory must not create entry paths, err=%v", err)
	}
}
