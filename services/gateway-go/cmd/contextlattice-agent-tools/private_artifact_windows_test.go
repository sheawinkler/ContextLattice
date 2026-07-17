//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWritePrivateArtifactWindowsResultIsRestrictedAndAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "private.json")
	if err := writePrivateArtifact(path, []byte("secret\r\n")); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "secret\r\n" {
		t.Fatalf("unexpected private artifact content=%q err=%v", content, err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := securePrivateArtifactFile(file, 0); err != nil {
		t.Fatalf("published private artifact ACL was not preserved: %v", err)
	}
	assertNoPrivateArtifactTemps(t, filepath.Dir(path), filepath.Base(path))
}

func TestWritePrivateArtifactWindowsRejectsReparseParent(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(root, "linked")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Skipf("Windows symlink privilege unavailable: %v", err)
	}
	err := writePrivateArtifact(filepath.Join(linkedParent, "private.json"), []byte("secret\r\n"))
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "reparse") {
		t.Fatalf("reparse-point parent was not rejected: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(realParent, "private.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("reparse rejection wrote into the redirected directory: %v", statErr)
	}
}

func TestWritePrivateArtifactWindowsRejectsInvalidComponentBeforeParentMutation(t *testing.T) {
	root := t.TempDir()
	newParent := filepath.Join(root, "new-parent")
	err := writePrivateArtifact(filepath.Join(newParent, "bad:name", "private.json"), []byte("secret\r\n"))
	if err == nil {
		t.Fatal("Windows ADS-bearing parent component was accepted")
	}
	if _, statErr := os.Stat(newParent); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("invalid path created a parent before rejection: %v", statErr)
	}
}

func TestPrivateArtifactWindowsParentRejectsUntrustedWriter(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("O:" + user.User.Sid.String() + "D:P(A;;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	if err := privateArtifactWindowsParentDescriptorSafe(descriptor); err == nil || !strings.Contains(err.Error(), "untrusted") {
		t.Fatalf("untrusted parent writer was not rejected: %v", err)
	}
}

func TestPrivateArtifactWindowsParentAllowsInheritOnlyCreatorOwner(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString(
		"O:" + user.User.Sid.String() + "D:P(A;;FA;;;" + user.User.Sid.String() + ")(A;OICIIO;GA;;;CO)",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := privateArtifactWindowsParentDescriptorSafe(descriptor); err != nil {
		t.Fatalf("inherit-only creator-owner ACE was treated as an effective parent writer: %v", err)
	}
}

func assertNoPrivateArtifactTemps(t *testing.T, directory, targetName string) {
	t.Helper()
	temporary, err := filepath.Glob(filepath.Join(directory, "."+targetName+".tmp-*"))
	if err != nil || len(temporary) != 0 {
		t.Fatalf("private artifact temporary files remain: %#v err=%v", temporary, err)
	}
}
