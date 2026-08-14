package main

import (
	"os"
	"path/filepath"
	"testing"
)

func testAgentTaskPlatformRoot(t *testing.T) (string, *os.File) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "artifacts")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ensureOwnerOnlyDirectory(root, true); err != nil {
		t.Fatalf("enforce owner-only platform root: %v", err)
	}
	rootFile, err := openAgentTaskDirectoryNoFollow(root)
	if err != nil {
		t.Fatalf("open owner-only platform root: %v", err)
	}
	t.Cleanup(func() { _ = rootFile.Close() })
	return root, rootFile
}

func testAgentTaskPlatformFile(t *testing.T, parent *os.File, name string, content []byte) (*os.File, agentTaskFileStat) {
	t.Helper()
	file, err := openAgentTaskFileAt(parent, name, agentTaskFileReadWriteCreateExclusive, 0o600, int64(len(content))+16)
	if err != nil {
		t.Fatalf("create platform file %q: %v", name, err)
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		t.Fatalf("write platform file %q: %v", name, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		t.Fatalf("sync platform file %q: %v", name, err)
	}
	stat, err := validateAgentTaskRegularFile(file, int64(len(content))+16)
	if err != nil {
		_ = file.Close()
		t.Fatalf("validate platform file %q: %v", name, err)
	}
	return file, stat
}

func TestAgentTaskPlatformDescriptorPinsPathReplacement(t *testing.T) {
	root, rootFile := testAgentTaskPlatformRoot(t)
	original := []byte("immutable-original")
	pinned, pinnedStat := testAgentTaskPlatformFile(t, rootFile, "proof.blob", original)
	defer pinned.Close()

	path := filepath.Join(root, "proof.blob")
	if err := os.Rename(path, filepath.Join(root, "proof.displaced")); err != nil {
		t.Fatalf("displace pinned platform file: %v", err)
	}
	replacement, replacementStat := testAgentTaskPlatformFile(t, rootFile, "proof.blob", []byte("immutable-foreignX"))
	_ = replacement.Close()

	heldStat, err := validateAgentTaskRegularFile(pinned, int64(len(original))+16)
	if err != nil {
		t.Fatalf("revalidate pinned descriptor: %v", err)
	}
	if heldStat.Device != pinnedStat.Device || heldStat.FileID != pinnedStat.FileID {
		t.Fatalf("pinned descriptor identity changed: before=%x:%x after=%x:%x", pinnedStat.Device, pinnedStat.FileID, heldStat.Device, heldStat.FileID)
	}
	if heldStat.Device == replacementStat.Device && heldStat.FileID == replacementStat.FileID {
		t.Fatal("replacement path reused the pinned descriptor identity")
	}
	current, err := openAgentTaskFileNoFollow(path, int64(len(original))+16)
	if err != nil {
		t.Fatalf("open replacement path through platform boundary: %v", err)
	}
	defer current.Close()
	currentStat, err := validateAgentTaskRegularFile(current, int64(len(original))+16)
	if err != nil {
		t.Fatalf("validate replacement path descriptor: %v", err)
	}
	if currentStat.Device != replacementStat.Device || currentStat.FileID != replacementStat.FileID {
		t.Fatal("current path did not resolve to the replacement descriptor")
	}
}

func TestAgentTaskPlatformRejectsSymlinkFinalComponents(t *testing.T) {
	root, rootFile := testAgentTaskPlatformRoot(t)
	target, _ := testAgentTaskPlatformFile(t, rootFile, "target.blob", []byte("safe-content"))
	_ = target.Close()
	linkPath := filepath.Join(root, "link.blob")
	if err := os.Symlink(filepath.Join(root, "target.blob"), linkPath); err != nil {
		if agentTaskPlatformSymlinkUnsupported(err) {
			t.Skipf("Windows symlink creation is not enabled: %v", err)
		}
		t.Fatalf("create platform symlink fixture: %v", err)
	}
	if file, err := openAgentTaskFileNoFollow(linkPath, 4096); err == nil {
		_ = file.Close()
		t.Fatal("platform no-follow boundary accepted a symlink final component")
	}
}

func TestAgentTaskPlatformRejectsMultiplyLinkedFiles(t *testing.T) {
	root, rootFile := testAgentTaskPlatformRoot(t)
	target, _ := testAgentTaskPlatformFile(t, rootFile, "target.blob", []byte("safe-content"))
	_ = target.Close()
	if err := os.Link(filepath.Join(root, "target.blob"), filepath.Join(root, "alias.blob")); err != nil {
		t.Fatalf("create hard-link fixture: %v", err)
	}
	if file, err := openAgentTaskFileNoFollow(filepath.Join(root, "target.blob"), 4096); err == nil {
		_ = file.Close()
		t.Fatal("platform immutable-file boundary accepted a multiply linked file")
	}
}
