package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode for %s = %04o, want %04o", path, got, want)
	}
}

func TestOwnerOnlyMigrationIsBoundedResumableAndIdempotent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.MkdirAll(filepath.Join(root, "project", "topic"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(root, "project", "one.md"),
		filepath.Join(root, "project", "topic", "two.md"),
	} {
		if err := os.WriteFile(path, []byte("private"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_MAX_ENTRIES", "1")

	incompleteCount := 0
	for attempts := 0; attempts < 16; attempts++ {
		err := migrateOwnerOnlyStore(root)
		if err == nil {
			break
		}
		if !errors.Is(err, errOwnerOnlyMigrationIncomplete) {
			t.Fatalf("migration failed: %v", err)
		}
		incompleteCount++
	}
	if incompleteCount == 0 {
		t.Fatal("expected bounded migration to require at least one resume")
	}
	if err := migrateOwnerOnlyStore(root); err != nil {
		t.Fatalf("idempotent completed migration failed: %v", err)
	}
	assertMode(t, root, 0o700)
	assertMode(t, filepath.Join(root, "project"), 0o700)
	assertMode(t, filepath.Join(root, "project", "topic"), 0o700)
	assertMode(t, filepath.Join(root, "project", "one.md"), 0o600)
	assertMode(t, filepath.Join(root, "project", "topic", "two.md"), 0o600)
	assertMode(t, ownerOnlyStatePath(root), 0o600)
	assertMode(t, outside, 0o644)
}

func TestOwnerOnlyMigrationRejectsEscapingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional Windows privileges")
	}
	root := filepath.Join(t.TempDir(), "store")
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := migrateOwnerOnlyStore(root); err == nil {
		t.Fatal("expected escaping symlink to fail closed")
	}
}

func TestOwnerOnlyMigrationRejectsNestedEscapingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional Windows privileges")
	}
	root := filepath.Join(t.TempDir(), "store")
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "redirect")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "redirect"), filepath.Join(root, "nested")); err != nil {
		t.Fatal(err)
	}
	if err := migrateOwnerOnlyStore(root); err == nil {
		t.Fatal("expected nested escaping symlink to fail closed")
	}
}

func TestOwnerOnlyAtomicAndAppendWrites(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	atomicPath := filepath.Join(root, "atomic.json")
	appendPath := filepath.Join(root, "events.ndjson")
	if err := writeOwnerOnlyAtomicFile(atomicPath, []byte("{}"), true); err != nil {
		t.Fatal(err)
	}
	file, err := openOwnerOnlyAppend(appendPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{}\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	assertMode(t, root, 0o700)
	assertMode(t, atomicPath, 0o600)
	assertMode(t, appendPath, 0o600)
}

func TestOwnerOnlyExplicitFileDoesNotTightenOperatorParent(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "operator-owned")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "runtime.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareOwnerOnlyFile(path, false); err != nil {
		t.Fatal(err)
	}
	assertMode(t, parent, 0o755)
	assertMode(t, path, 0o600)
}
