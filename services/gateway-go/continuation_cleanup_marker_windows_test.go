//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// This is a runtime fixture, not merely a cross-compile assertion. It is
// skipped only when the Windows host cannot create the junction/symlink needed
// by the adversarial case (for example, a CI token without the required
// privilege). The implementation under test must reject the replacement via
// RootDirectory-bound NtCreateFile when the fixture is available.
func TestContinuationCleanupMarkerWindowsRejectsReparseAncestorReplacement(t *testing.T) {
	root := t.TempDir()
	index := filepath.Join(root, continuationEvaluationCleanupIndexDirectory)
	shard := filepath.Join(index, "aa")
	if err := os.MkdirAll(shard, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shard, "marker"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readEvaluationCleanupMarkerFilesBounded(root, continuationEvaluationCleanupIndexDirectory, "aa", 8, 1024, nil); err != nil {
		t.Fatalf("initial handle-relative fixture read failed: %v", err)
	}
	external := filepath.Join(root, "external-shard")
	if err := os.Mkdir(external, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(external, "marker"), []byte("external\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backup := shard + ".original"
	if err := os.Rename(shard, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, shard); err != nil {
		_ = os.Rename(backup, shard)
		t.Skipf("Windows reparse fixture unavailable: %v", err)
	}
	defer func() {
		_ = os.Remove(shard)
		_ = os.Rename(backup, shard)
	}()
	if _, _, err := readEvaluationCleanupMarkerFilesBounded(root, continuationEvaluationCleanupIndexDirectory, "aa", 8, 1024, nil); err == nil {
		t.Fatal("reparse ancestor replacement was accepted")
	}
	if err := writeEvaluationCleanupMarkerDurable(root, continuationEvaluationCleanupIndexDirectory, "aa", "marker", []byte("replacement\n")); err == nil {
		t.Fatal("reparse ancestor replacement redirected a marker write")
	}
	if _, err := os.Stat(filepath.Join(external, "marker")); err != nil {
		t.Fatalf("reparse write fixture lost external marker: %v", err)
	}
}
