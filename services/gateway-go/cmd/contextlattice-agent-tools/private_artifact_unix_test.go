//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestWritePrivateArtifactDoesNotChangeExistingParentMode(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writePrivateArtifact(filepath.Join(directory, "private.json"), []byte("secret\n")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("private artifact write changed its existing parent mode: %o", info.Mode().Perm())
	}
}

func TestNormalizePrivateArtifactUnixParentSupportsMacOSVarAlias(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS system alias contract")
	}
	parent, err := normalizePrivateArtifactUnixParent("/var/folders/example")
	if err != nil {
		t.Fatal(err)
	}
	if parent != "/private/var/folders/example" {
		t.Fatalf("normalized macOS private artifact parent = %q", parent)
	}
}

func TestWritePrivateArtifactRejectsUnsafeWritableParent(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o777); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "private.json")
	err := writePrivateArtifact(path, []byte("secret\n"))
	if err == nil || !strings.Contains(err.Error(), "group/other-writable") {
		t.Fatalf("unsafe parent was not rejected: %v", err)
	}
	info, statErr := os.Stat(directory)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm() != 0o777 {
		t.Fatalf("unsafe parent mode was mutated: %o", info.Mode().Perm())
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unsafe parent produced an artifact: %v", statErr)
	}
	assertNoPrivateArtifactTemps(t, directory, "private.json")
}

func TestWritePrivateArtifactRejectsSymlinkedParentComponents(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real", "nested")
	if err := os.MkdirAll(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		name string
		path string
		link string
		to   string
	}{
		{name: "final parent", link: filepath.Join(root, "linked-parent"), to: realParent, path: filepath.Join(root, "linked-parent", "private.json")},
		{name: "ancestor", link: filepath.Join(root, "linked-ancestor"), to: filepath.Join(root, "real"), path: filepath.Join(root, "linked-ancestor", "nested", "private.json")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if err := os.Symlink(testCase.to, testCase.link); err != nil {
				t.Fatal(err)
			}
			err := writePrivateArtifact(testCase.path, []byte("secret\n"))
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "symlink") {
				t.Fatalf("symlinked private artifact parent was not rejected: %v", err)
			}
			_ = os.Remove(testCase.link)
		})
	}
	if _, err := os.Stat(filepath.Join(realParent, "private.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink rejection wrote into the redirected directory: %v", err)
	}
}

func TestPrivateArtifactUnixParentRejectsForeignOwner(t *testing.T) {
	stat := &unix.Stat_t{Mode: unix.S_IFDIR | 0o700, Uid: uint32(os.Geteuid() + 1)}
	if err := privateArtifactUnixParentSafe(stat); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("foreign-owned parent was not rejected: %v", err)
	}
}

func TestWritePrivateArtifactCreatesMissingParentSecurely(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "new", "nested")
	path := filepath.Join(parent, "private.json")
	if err := writePrivateArtifact(path, []byte("secret\n")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("new private artifact parent mode is %o, want 700", info.Mode().Perm())
	}
}

func TestWritePrivateArtifactResultIsOwnerOnly(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "private.json")
	if err := writePrivateArtifact(path, []byte("secret\n")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("private artifact mode is %o, want 600", info.Mode().Perm())
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "secret\n" {
		t.Fatalf("unexpected private artifact: content=%q err=%v", content, err)
	}
	assertNoPrivateArtifactTemps(t, directory, "private.json")
}

func TestWritePrivateArtifactPreCommitFailurePreservesTargetAndRemovesTemp(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "private.json")
	if err := os.WriteFile(path, []byte("previous\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	originalReplace := privateArtifactReplace
	privateArtifactReplace = func(*privateArtifactPublication) error {
		return errors.New("synthetic pre-commit failure")
	}
	t.Cleanup(func() { privateArtifactReplace = originalReplace })

	err := writePrivateArtifact(path, []byte("replacement\n"))
	if err == nil || !strings.Contains(err.Error(), "synthetic pre-commit failure") {
		t.Fatalf("pre-commit failure was not returned: %v", err)
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil || string(content) != "previous\n" {
		t.Fatalf("pre-commit failure damaged the target: content=%q err=%v", content, readErr)
	}
	assertNoPrivateArtifactTemps(t, directory, "private.json")
}

func TestWritePrivateArtifactHasNoFallibleHardeningAfterCommit(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "private.json")
	originalSecure := privateArtifactSecure
	originalReplace := privateArtifactReplace
	committed := false
	secureCalls := 0
	privateArtifactSecure = func(file *os.File, mode os.FileMode) error {
		if committed {
			return errors.New("synthetic post-commit hardening failure")
		}
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if info.Size() != 0 {
			return errors.New("private artifact security was not verified before content")
		}
		secureCalls++
		return originalSecure(file, mode)
	}
	privateArtifactReplace = func(publication *privateArtifactPublication) error {
		if err := originalReplace(publication); err != nil {
			return err
		}
		committed = true
		return nil
	}
	t.Cleanup(func() {
		privateArtifactSecure = originalSecure
		privateArtifactReplace = originalReplace
	})

	if err := writePrivateArtifact(path, []byte("secret\n")); err != nil {
		t.Fatal(err)
	}
	if !committed || secureCalls != 1 {
		t.Fatalf("unexpected publication ordering: committed=%t secure_calls=%d", committed, secureCalls)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "secret\n" {
		t.Fatalf("unexpected committed artifact: content=%q err=%v", content, err)
	}
}

func assertNoPrivateArtifactTemps(t *testing.T, directory, targetName string) {
	t.Helper()
	temporary, err := filepath.Glob(filepath.Join(directory, "."+targetName+".tmp-*"))
	if err != nil || len(temporary) != 0 {
		t.Fatalf("private artifact temporary files remain: %#v err=%v", temporary, err)
	}
}
