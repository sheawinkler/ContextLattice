package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// evaluationCleanupMarkerDirectoryEntry is the small, platform-neutral view
// used while rebuilding the content-addressed marker index.  Platform code
// fills this from a verified directory descriptor; the rebuild code never
// trusts a pathname-derived os.DirEntry after the descriptor is closed.
type evaluationCleanupMarkerDirectoryFile struct {
	name  string
	isDir bool
	raw   []byte
}

// evaluationCleanupMarkerDirectoryTree is returned by the rebuild/inventory
// traversal. The platform implementation keeps the verified root/index/shard
// descriptors alive while filling each entry, so callers never have to reopen
// a pathname between enumeration and child reads.
type evaluationCleanupMarkerDirectoryTree struct {
	name     string
	isDir    bool
	overflow bool
	entries  []evaluationCleanupMarkerDirectoryFile
}

func evaluationCleanupMarkerComponentValid(component string, allowEmpty bool) bool {
	if allowEmpty && component == "" {
		return true
	}
	return component != "" && component != "." && component != ".." && filepath.Base(component) == component
}

// evaluationCleanupMarkerDirectoryComponents accepts only the two directory
// levels owned by the marker index.  The platform implementations use these
// components with descriptor-relative open/create operations; callers never
// need to turn this into a pathname mutation.
func evaluationCleanupMarkerDirectoryComponents(root, path string) (string, string, error) {
	cleanRoot := filepath.Clean(root)
	cleanPath := filepath.Clean(path)
	rel, err := filepath.Rel(cleanRoot, cleanPath)
	if err != nil || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", errors.New("evaluation cleanup marker directory escapes queue root")
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) == 1 && parts[0] == continuationEvaluationCleanupIndexDirectory {
		return parts[0], "", nil
	}
	if len(parts) == 2 && parts[0] == continuationEvaluationCleanupIndexDirectory && evaluationCleanupMarkerComponentValid(parts[1], false) {
		return parts[0], parts[1], nil
	}
	return "", "", errors.New("evaluation cleanup marker directory path is not an index or shard")
}

func evaluationCleanupMarkerVerifyStablePath(path string, expected os.FileInfo) error {
	if expected == nil {
		return errors.New("evaluation cleanup marker stable identity is missing")
	}
	actual, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(expected, actual) {
		return errors.New("evaluation cleanup marker path identity changed")
	}
	return nil
}
