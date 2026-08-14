//go:build windows

package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// This fixture intentionally exercises the same parent-handle-relative opens
// used by migration.  It does not use a pathname reopen for either the child
// directory or the file, so a Windows build always type-checks the verified
// traversal contract even when the package is compiled off-host.
func TestWindowsOwnerOnlyRelativeTraversalFixture(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	child := filepath.Join(root, "project")
	filePath := filepath.Join(child, "notes.md")
	if err := os.MkdirAll(child, ownerOnlyDirectoryMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, []byte("fixture"), ownerOnlyFileMode); err != nil {
		t.Fatal(err)
	}
	rootHandle, err := openOwnerOnlyRootDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer rootHandle.Close()
	childHandle, err := openOwnerOnlyDirectoryAt(rootHandle, "project", child)
	if err != nil {
		t.Fatal(err)
	}
	defer childHandle.Close()
	isDir, isRegular, err := ownerOnlyWindowsHandleKind(childHandleHandle(childHandle))
	if err != nil || !isDir || isRegular {
		t.Fatalf("relative child handle kind = dir:%v regular:%v err:%v", isDir, isRegular, err)
	}
	enforced, isDirectory, err := enforceOwnerOnlyEntryAt(root, rootHandle, "project", child)
	if err != nil || !enforced || !isDirectory {
		t.Fatalf("relative directory enforcement = enforced:%v directory:%v err:%v", enforced, isDirectory, err)
	}
	enforced, isDirectory, err = enforceOwnerOnlyEntryAt(root, childHandle, "notes.md", filePath)
	if err != nil || !enforced || isDirectory {
		t.Fatalf("relative file enforcement = enforced:%v directory:%v err:%v", enforced, isDirectory, err)
	}
	readHandle, err := openOwnerOnlyReadPlatform(filePath)
	if err != nil {
		t.Fatalf("normal writable descriptor read open: %v", err)
	}
	readBytes, err := io.ReadAll(readHandle)
	_ = readHandle.Close()
	if err != nil || string(readBytes) != "fixture" {
		t.Fatalf("normal writable descriptor read = %q/%v", string(readBytes), err)
	}
	appendHandle, err := openOwnerOnlyAppendPlatform(filePath)
	if err != nil {
		t.Fatalf("normal writable descriptor append open: %v", err)
	}
	if _, err := appendHandle.WriteString("-append"); err != nil {
		_ = appendHandle.Close()
		t.Fatalf("normal writable descriptor append: %v", err)
	}
	if err := appendHandle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filePath, 0o444); err != nil {
		t.Fatalf("set readonly fixture: %v", err)
	}
	if appendHandle, err := openOwnerOnlyAppendPlatform(filePath); err == nil {
		_ = appendHandle.Close()
		t.Fatal("readonly descriptor unexpectedly opened for append")
	}
}

func childHandleHandle(file *os.File) windows.Handle {
	return windows.Handle(file.Fd())
}
