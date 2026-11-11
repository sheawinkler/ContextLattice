package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

type unreadableStorageEntry struct{ fs.DirEntry }

func (unreadableStorageEntry) Info() (fs.FileInfo, error) { return nil, fs.ErrPermission }

func TestTrackedDirectorySizePreservesScanFailures(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "sample"), []byte("measured"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"walk", "entry_info", "walk_return"} {
		t.Run(failure, func(t *testing.T) {
			walk := func(path string, visit fs.WalkDirFunc) error {
				if err := visit(path, entries[0], nil); err != nil {
					return err
				}
				switch failure {
				case "walk":
					return visit(path, nil, fs.ErrPermission)
				case "entry_info":
					return visit(path, unreadableStorageEntry{entries[0]}, nil)
				default:
					return fs.ErrPermission
				}
			}
			size, truncated, err := trackedDirectorySize(root, 10, walk)
			if size != 8 || truncated || !errors.Is(err, fs.ErrPermission) {
				t.Fatalf("incomplete scan lost evidence: bytes=%d truncated=%v err=%v", size, truncated, err)
			}
		})
	}
}

func TestTrackedDirectorySizeStopsAtOneGlobalBudget(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"a/one", "b/two", "c/three"} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	visited := 0
	walk := func(path string, visit fs.WalkDirFunc) error {
		return filepath.WalkDir(path, func(name string, entry fs.DirEntry, err error) error {
			if entry != nil && !entry.IsDir() {
				visited++
			}
			return visit(name, entry, err)
		})
	}
	size, truncated, err := trackedDirectorySize(root, 1, walk)
	if size != 4 || !truncated || err != nil || visited != 2 {
		t.Fatalf("budget was not globally bounded: bytes=%d truncated=%v visited=%d err=%v", size, truncated, visited, err)
	}
	rows := collectTrackedStorage(map[string]string{"tree": root, "absent": filepath.Join(root, "absent")}, 10)
	tree := rows["tree"].(map[string]any)
	absent := rows["absent"].(map[string]any)
	if tree["bytes"] != int64(12) || tree["error"] != nil || tree["truncated"] != nil || absent["bytes"] != int64(0) || absent["exists"] != false {
		t.Fatalf("complete or absent measurement changed: %#v", rows)
	}
}
