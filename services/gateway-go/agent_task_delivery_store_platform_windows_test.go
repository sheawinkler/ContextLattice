//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func agentTaskPlatformSymlinkUnsupported(err error) bool {
	return errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

func TestAgentTaskWindowsRejectsReparseAncestorSwapDuringTraversal(t *testing.T) {
	base := t.TempDir()
	ancestorName := "task-root-ancestor"
	ancestor := filepath.Join(base, ancestorName)
	root := filepath.Join(ancestor, "artifacts")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ensureOwnerOnlyDirectory(root, true); err != nil {
		t.Fatalf("enforce owner-only original root: %v", err)
	}
	externalAncestor := filepath.Join(base, "external-ancestor")
	externalRoot := filepath.Join(externalAncestor, "artifacts")
	if err := os.MkdirAll(externalRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ensureOwnerOnlyDirectory(externalRoot, true); err != nil {
		t.Fatalf("enforce owner-only external root: %v", err)
	}

	displaced := ancestor + ".original"
	swapped := false
	defer func() {
		if !swapped {
			return
		}
		_ = os.Remove(ancestor)
		_ = os.Rename(displaced, ancestor)
	}()
	var fixtureErr error
	hookCalled := false
	hook := func(stage string, _ int, component string) error {
		if stage != agentTaskWindowsTraversalBeforeComponent || component != ancestorName || hookCalled {
			return nil
		}
		hookCalled = true
		if err := os.Rename(ancestor, displaced); err != nil {
			fixtureErr = err
			return err
		}
		if err := os.Symlink(externalAncestor, ancestor); err != nil {
			_ = os.Rename(displaced, ancestor)
			fixtureErr = err
			return err
		}
		swapped = true
		return nil
	}
	file, err := openAgentTaskDirectoryNoFollowWithHook(root, hook)
	if file != nil {
		_ = file.Close()
	}
	if fixtureErr != nil && agentTaskPlatformSymlinkUnsupported(fixtureErr) {
		t.Skipf("Windows reparse fixture unavailable: %v", fixtureErr)
	}
	if fixtureErr != nil {
		t.Fatalf("create Windows reparse fixture: %v", fixtureErr)
	}
	if !hookCalled {
		t.Fatal("ancestor replacement hook was not reached")
	}
	if err == nil {
		t.Fatal("descriptor traversal accepted an ancestor reparse swap")
	}
}

func TestAgentTaskWindowsPinsOpenedAncestorAcrossPathReplacement(t *testing.T) {
	base := t.TempDir()
	ancestorName := "task-root-pinned-ancestor"
	ancestor := filepath.Join(base, ancestorName)
	originalRoot := filepath.Join(ancestor, "artifacts")
	if err := os.MkdirAll(originalRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ensureOwnerOnlyDirectory(originalRoot, true); err != nil {
		t.Fatalf("enforce owner-only original root: %v", err)
	}
	foreignAncestor := filepath.Join(base, "foreign-ancestor")
	foreignRoot := filepath.Join(foreignAncestor, "artifacts")
	if err := os.MkdirAll(foreignRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ensureOwnerOnlyDirectory(foreignRoot, true); err != nil {
		t.Fatalf("enforce owner-only foreign root: %v", err)
	}

	displaced := ancestor + ".original"
	hookCalled := false
	hook := func(stage string, _ int, component string) error {
		if stage != agentTaskWindowsTraversalAfterComponent || component != ancestorName || hookCalled {
			return nil
		}
		hookCalled = true
		if err := os.Rename(ancestor, displaced); err != nil {
			return err
		}
		if err := os.Rename(foreignAncestor, ancestor); err != nil {
			_ = os.Rename(displaced, ancestor)
			return err
		}
		return nil
	}
	file, err := openAgentTaskDirectoryNoFollowWithHook(originalRoot, hook)
	if err != nil {
		t.Fatalf("open root across ancestor path replacement: %v", err)
	}
	defer file.Close()
	if !hookCalled {
		t.Fatal("ancestor replacement hook was not reached")
	}
	pinnedInfo, err := file.Stat()
	if err != nil {
		t.Fatalf("stat pinned root descriptor: %v", err)
	}
	originalInfo, err := os.Stat(filepath.Join(displaced, "artifacts"))
	if err != nil {
		t.Fatalf("stat displaced original root: %v", err)
	}
	foreignInfo, err := os.Stat(filepath.Join(ancestor, "artifacts"))
	if err != nil {
		t.Fatalf("stat replacement root: %v", err)
	}
	if !os.SameFile(pinnedInfo, originalInfo) {
		t.Fatal("root traversal did not remain bound to the opened ancestor")
	}
	if os.SameFile(pinnedInfo, foreignInfo) {
		t.Fatal("root traversal followed the replacement path namespace")
	}
}
