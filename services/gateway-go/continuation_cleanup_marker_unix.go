//go:build !windows

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// acquireEvaluationCleanupMarkerMigrationOwnerLock is the process-shared
// mutation fence for the cap pointer and its immutable plan/receipt chain.
// The lock file is opened relative to a verified queue-root descriptor so a
// replaced ancestor or symlink cannot create a second fence elsewhere.
func acquireEvaluationCleanupMarkerMigrationOwnerLock(root string) (func(), error) {
	rootFile, err := evaluationCleanupMarkerOpenRootDirectory(root)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(rootFile.Fd()), continuationEvaluationCleanupMarkerMigrationOwnerLockFile, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(ownerOnlyFileMode.Perm()))
	if err != nil {
		_ = rootFile.Close()
		return nil, err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(root, continuationEvaluationCleanupMarkerMigrationOwnerLockFile))
	if file == nil {
		_ = unix.Close(fd)
		_ = rootFile.Close()
		return nil, errors.New("evaluation cleanup marker owner lock descriptor is unavailable")
	}
	closeFiles := func() {
		_ = file.Close()
		_ = rootFile.Close()
	}
	info, err := file.Stat()
	if err != nil {
		closeFiles()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		closeFiles()
		return nil, errors.New("evaluation cleanup marker owner lock is not a regular file")
	}
	if err := unix.Fchmod(int(file.Fd()), uint32(ownerOnlyFileMode.Perm())); err != nil {
		closeFiles()
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		closeFiles()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, errOwnerOnlyMigrationLocked
		}
		return nil, err
	}
	var lockedStat, pathStat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &lockedStat); err != nil || unix.Fstatat(int(rootFile.Fd()), continuationEvaluationCleanupMarkerMigrationOwnerLockFile, &pathStat, unix.AT_SYMLINK_NOFOLLOW) != nil || lockedStat.Dev != pathStat.Dev || lockedStat.Ino != pathStat.Ino {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		closeFiles()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("evaluation cleanup marker owner lock path changed")
	}
	return func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		closeFiles()
	}, nil
}

func evaluationCleanupMarkerOpenDirectoryAt(parent *os.File, name, path string, create bool) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		if !create {
			return nil, os.ErrNotExist
		}
		if mkdirErr := unix.Mkdirat(int(parent.Fd()), name, uint32(ownerOnlyDirectoryMode.Perm())); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
			return nil, mkdirErr
		}
		if syncErr := parent.Sync(); syncErr != nil {
			return nil, &ownerOnlyAtomicWriteError{Operation: "sync descriptor-relative marker directory parent", Committed: true, Err: syncErr}
		}
		fd, err = unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("evaluation cleanup marker directory descriptor is unavailable")
	}
	if err := unix.Fchmod(int(file.Fd()), uint32(ownerOnlyDirectoryMode.Perm())); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func ensureEvaluationCleanupMarkerDirectoriesDurable(root, index, shard string, syncHook func(string) error) error {
	if !evaluationCleanupMarkerComponentValid(index, false) || !evaluationCleanupMarkerComponentValid(shard, true) {
		return errors.New("evaluation cleanup marker directory component is invalid")
	}
	rootFile, err := evaluationCleanupMarkerOpenRootDirectory(root)
	if err != nil {
		return err
	}
	defer rootFile.Close()
	rootInfo, err := rootFile.Stat()
	if err != nil {
		return err
	}
	indexFile, err := evaluationCleanupMarkerOpenDirectoryAt(rootFile, index, filepath.Join(root, index), true)
	if err != nil {
		return err
	}
	defer indexFile.Close()
	indexInfo, err := indexFile.Stat()
	if err != nil {
		return err
	}
	if syncHook != nil {
		if err := syncHook(filepath.Join(root, index)); err != nil {
			return err
		}
	}
	if shard == "" {
		if err := evaluationCleanupMarkerVerifyStablePath(root, rootInfo); err != nil {
			return err
		}
		return evaluationCleanupMarkerVerifyStablePath(filepath.Join(root, index), indexInfo)
	}
	shardFile, err := evaluationCleanupMarkerOpenDirectoryAt(indexFile, shard, filepath.Join(root, index, shard), true)
	if err != nil {
		return err
	}
	defer shardFile.Close()
	shardInfo, err := shardFile.Stat()
	if err != nil {
		return err
	}
	if syncHook != nil {
		if err := syncHook(filepath.Join(root, index, shard)); err != nil {
			return err
		}
	}
	if err := evaluationCleanupMarkerVerifyStablePath(root, rootInfo); err != nil {
		return err
	}
	if err := evaluationCleanupMarkerVerifyStablePath(filepath.Join(root, index), indexInfo); err != nil {
		return err
	}
	return evaluationCleanupMarkerVerifyStablePath(filepath.Join(root, index, shard), shardInfo)
}

func evaluationCleanupMarkerOpenRootDirectory(path string) (*os.File, error) {
	clean := filepath.Clean(strings.TrimSpace(path))
	if clean == "" || !filepath.IsAbs(clean) {
		return nil, errors.New("evaluation cleanup marker root must be an absolute path")
	}
	// Open every ancestor relative to the descriptor opened before it.  A
	// single O_NOFOLLOW on the final path is insufficient: an attacker can
	// replace an ancestor with a symlink between the path lookup components.
	fd, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	current := os.NewFile(uintptr(fd), string(filepath.Separator))
	if current == nil {
		_ = unix.Close(fd)
		return nil, errors.New("evaluation cleanup marker root descriptor is unavailable")
	}
	parts := strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			continue
		}
		nextFD, openErr := unix.Openat(int(current.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(openErr, unix.ENOENT) {
			_ = current.Close()
			return nil, os.ErrNotExist
		}
		if openErr != nil {
			_ = current.Close()
			return nil, openErr
		}
		next := os.NewFile(uintptr(nextFD), filepath.Join(current.Name(), part))
		if next == nil {
			_ = unix.Close(nextFD)
			_ = current.Close()
			return nil, errors.New("evaluation cleanup marker ancestor descriptor is unavailable")
		}
		_ = current.Close()
		current = next
	}
	return current, nil
}

func evaluationCleanupMarkerSyncDirectories(files ...*os.File) error {
	for _, file := range files {
		if file == nil {
			continue
		}
		if err := file.Sync(); err != nil {
			return err
		}
	}
	return nil
}

func writeEvaluationCleanupMarkerDurable(root, index, shard, name string, content []byte) error {
	if !evaluationCleanupMarkerComponentValid(index, false) || !evaluationCleanupMarkerComponentValid(shard, true) || !evaluationCleanupMarkerComponentValid(name, false) {
		return errors.New("evaluation cleanup marker write component is invalid")
	}
	rootFile, err := evaluationCleanupMarkerOpenRootDirectory(root)
	if err != nil {
		return err
	}
	defer rootFile.Close()
	rootInfo, err := rootFile.Stat()
	if err != nil {
		return err
	}
	indexFile, err := evaluationCleanupMarkerOpenDirectoryAt(rootFile, index, filepath.Join(root, index), true)
	if err != nil {
		return err
	}
	defer indexFile.Close()
	indexInfo, err := indexFile.Stat()
	if err != nil {
		return err
	}
	parentFile := indexFile
	var shardFile *os.File
	var shardInfo os.FileInfo
	if shard != "" {
		shardFile, err = evaluationCleanupMarkerOpenDirectoryAt(indexFile, shard, filepath.Join(root, index, shard), true)
		if err != nil {
			return err
		}
		defer shardFile.Close()
		shardInfo, err = shardFile.Stat()
		if err != nil {
			return err
		}
		parentFile = shardFile
	}

	// The temporary name is created inside the verified parent descriptor.  A
	// pathname race cannot redirect either creation or replacement.
	var tempName string
	var tempFile *os.File
	for attempt := 0; attempt < 16; attempt++ {
		tempName = fmt.Sprintf(".%s.tmp-%x-%x", name, uint64(os.Getpid()), uint64(timeNowUnixNano()))
		fd, openErr := unix.Openat(int(parentFile.Fd()), tempName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(ownerOnlyFileMode.Perm()))
		if errors.Is(openErr, unix.EEXIST) {
			continue
		}
		if openErr != nil {
			return openErr
		}
		tempFile = os.NewFile(uintptr(fd), filepath.Join(root, index, shard, tempName))
		if tempFile == nil {
			_ = unix.Close(fd)
			_ = unix.Unlinkat(int(parentFile.Fd()), tempName, 0)
			return errors.New("evaluation cleanup marker temporary descriptor is unavailable")
		}
		break
	}
	if tempFile == nil {
		return errors.New("evaluation cleanup marker temporary name allocation failed")
	}
	removeTemp := true
	defer func() {
		_ = tempFile.Close()
		if removeTemp {
			_ = unix.Unlinkat(int(parentFile.Fd()), tempName, 0)
		}
	}()
	if err := unix.Fchmod(int(tempFile.Fd()), uint32(ownerOnlyFileMode.Perm())); err != nil {
		return err
	}
	if _, err := tempFile.Write(content); err != nil {
		return err
	}
	if err := tempFile.Sync(); err != nil {
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}
	if err := unix.Renameat(int(parentFile.Fd()), tempName, int(parentFile.Fd()), name); err != nil {
		return err
	}
	removeTemp = false
	// Flush every verified ancestor, including the queue root, so creation of
	// an index/shard and replacement of its marker have one durable boundary.
	if err := evaluationCleanupMarkerSyncDirectories(rootFile, indexFile, shardFile, parentFile); err != nil {
		return &ownerOnlyAtomicWriteError{Operation: "sync descriptor-relative marker ancestors", Committed: true, Err: err}
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(int(parentFile.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return &ownerOnlyAtomicWriteError{Operation: "verify descriptor-relative marker", Committed: true, Err: err}
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || uint32(stat.Mode&0777) != uint32(ownerOnlyFileMode.Perm()) {
		return &ownerOnlyAtomicWriteError{Operation: "verify descriptor-relative marker", Committed: true, Err: errors.New("marker replacement is not owner-only regular storage")}
	}
	if pathErr := evaluationCleanupMarkerVerifyStablePath(root, rootInfo); pathErr != nil {
		return &ownerOnlyAtomicWriteError{Operation: "verify descriptor-relative marker root", Committed: true, Err: pathErr}
	}
	if pathErr := evaluationCleanupMarkerVerifyStablePath(filepath.Join(root, index), indexInfo); pathErr != nil {
		return &ownerOnlyAtomicWriteError{Operation: "verify descriptor-relative marker index", Committed: true, Err: pathErr}
	}
	if shardFile != nil {
		if pathErr := evaluationCleanupMarkerVerifyStablePath(filepath.Join(root, index, shard), shardInfo); pathErr != nil {
			return &ownerOnlyAtomicWriteError{Operation: "verify descriptor-relative marker shard", Committed: true, Err: pathErr}
		}
	}
	return nil
}

// readEvaluationCleanupMarkerFilesBounded keeps the verified parent directory
// descriptor open from enumeration through every child open/read. A path
// reopen after enumeration would permit an ABA replacement to swap in a
// different tree and restore the old pathname before an identity check.
func readEvaluationCleanupMarkerFilesBounded(root, index, shard string, limit int, maxBytes int64, hook func(string) error) ([]evaluationCleanupMarkerDirectoryFile, bool, error) {
	if limit <= 0 || maxBytes <= 0 || !evaluationCleanupMarkerComponentValid(index, false) || !evaluationCleanupMarkerComponentValid(shard, true) {
		return nil, false, errors.New("evaluation cleanup marker directory bound or component is invalid")
	}
	rootFile, err := evaluationCleanupMarkerOpenRootDirectory(root)
	if err != nil {
		return nil, false, err
	}
	defer rootFile.Close()
	rootInfo, err := rootFile.Stat()
	if err != nil {
		return nil, false, err
	}
	indexFile, err := evaluationCleanupMarkerOpenDirectoryAt(rootFile, index, filepath.Join(root, index), false)
	if err != nil {
		return nil, false, err
	}
	defer indexFile.Close()
	indexInfo, err := indexFile.Stat()
	if err != nil {
		return nil, false, err
	}
	parentFile := indexFile
	var shardFile *os.File
	var shardInfo os.FileInfo
	if shard != "" {
		shardFile, err = evaluationCleanupMarkerOpenDirectoryAt(indexFile, shard, filepath.Join(root, index, shard), false)
		if err != nil {
			return nil, false, err
		}
		defer shardFile.Close()
		shardInfo, err = shardFile.Stat()
		if err != nil {
			return nil, false, err
		}
		parentFile = shardFile
	}
	entries, readErr := parentFile.ReadDir(limit + 1)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, false, readErr
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	overflow := len(entries) > limit
	if overflow {
		entries = entries[:limit]
	}
	if hook != nil {
		stage := "after_directory_enumeration"
		if shard != "" {
			stage += ":" + shard
		}
		if err := hook(stage); err != nil {
			return nil, false, err
		}
	}
	result := make([]evaluationCleanupMarkerDirectoryFile, 0, len(entries))
	for _, entry := range entries {
		item := evaluationCleanupMarkerDirectoryFile{name: entry.Name(), isDir: entry.IsDir()}
		if !item.isDir {
			markerFD, openErr := unix.Openat(int(parentFile.Fd()), item.name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if errors.Is(openErr, unix.ENOENT) {
				return nil, false, os.ErrNotExist
			}
			if openErr != nil {
				return nil, false, openErr
			}
			markerFile := os.NewFile(uintptr(markerFD), filepath.Join(root, index, shard, item.name))
			if markerFile == nil {
				_ = unix.Close(markerFD)
				return nil, false, errors.New("evaluation cleanup marker descriptor is unavailable")
			}
			info, statErr := markerFile.Stat()
			if statErr != nil {
				_ = markerFile.Close()
				return nil, false, statErr
			}
			if !info.Mode().IsRegular() || info.Mode().Perm() != ownerOnlyFileMode.Perm() {
				_ = markerFile.Close()
				return nil, false, errors.New("evaluation cleanup marker descriptor is not owner-only regular storage")
			}
			raw, readMarkerErr := io.ReadAll(io.LimitReader(markerFile, maxBytes+1))
			_ = markerFile.Close()
			if readMarkerErr != nil {
				return nil, false, readMarkerErr
			}
			if int64(len(raw)) > maxBytes {
				return nil, false, errContinuationDurableFileOversized
			}
			item.raw = raw
		}
		result = append(result, item)
	}
	if hook != nil && shard != "" {
		if err := hook("after_marker_read:" + shard); err != nil {
			return nil, false, err
		}
	}
	finalIndexInfo, statErr := indexFile.Stat()
	if statErr != nil || !os.SameFile(indexInfo, finalIndexInfo) {
		if statErr != nil {
			return nil, false, statErr
		}
		return nil, false, errors.New("evaluation cleanup marker index descriptor changed during read")
	}
	if pathErr := evaluationCleanupMarkerVerifyStablePath(root, rootInfo); pathErr != nil {
		return nil, false, pathErr
	}
	if pathErr := evaluationCleanupMarkerVerifyStablePath(filepath.Join(root, index), indexInfo); pathErr != nil {
		return nil, false, pathErr
	}
	if shardFile != nil {
		finalShardInfo, shardStatErr := shardFile.Stat()
		if shardStatErr != nil || !os.SameFile(shardInfo, finalShardInfo) {
			if shardStatErr != nil {
				return nil, false, shardStatErr
			}
			return nil, false, errors.New("evaluation cleanup marker shard descriptor changed during read")
		}
		if pathErr := evaluationCleanupMarkerVerifyStablePath(filepath.Join(root, index, shard), shardInfo); pathErr != nil {
			return nil, false, pathErr
		}
	}
	return result, overflow, nil
}

// readEvaluationCleanupMarkerTreeBounded keeps the root and index descriptors
// open while it enumerates every shard, and keeps each shard descriptor open
// while it opens and reads all of that shard's children. This is the
// descriptor-relative rebuild/inventory session; splitting it into one call
// per directory would reintroduce an ABA pathname race between enumeration
// and child reads.
func readEvaluationCleanupMarkerTreeBounded(root, index string, shardLimit, markerLimit int, maxBytes int64, expectedIndex os.FileInfo, hook func(string) error) ([]evaluationCleanupMarkerDirectoryTree, bool, error) {
	if shardLimit <= 0 || markerLimit <= 0 || maxBytes <= 0 || !evaluationCleanupMarkerComponentValid(index, false) {
		return nil, false, errors.New("evaluation cleanup marker tree bound or component is invalid")
	}
	rootFile, err := evaluationCleanupMarkerOpenRootDirectory(root)
	if err != nil {
		return nil, false, err
	}
	defer rootFile.Close()
	rootInfo, err := rootFile.Stat()
	if err != nil {
		return nil, false, err
	}
	indexFile, err := evaluationCleanupMarkerOpenDirectoryAt(rootFile, index, filepath.Join(root, index), false)
	if err != nil {
		return nil, false, err
	}
	defer indexFile.Close()
	indexInfo, err := indexFile.Stat()
	if err != nil {
		return nil, false, err
	}
	if expectedIndex != nil && !os.SameFile(expectedIndex, indexInfo) {
		return nil, false, errors.New("evaluation cleanup marker index was replaced before rebuild")
	}
	shardEntries, readErr := indexFile.ReadDir(shardLimit + 1)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, false, readErr
	}
	sort.Slice(shardEntries, func(i, j int) bool { return shardEntries[i].Name() < shardEntries[j].Name() })
	rootOverflow := len(shardEntries) > shardLimit
	if rootOverflow {
		shardEntries = shardEntries[:shardLimit]
	}
	if hook != nil {
		if err := hook("after_tree_enumeration"); err != nil {
			return nil, rootOverflow, err
		}
	}
	trees := make([]evaluationCleanupMarkerDirectoryTree, 0, len(shardEntries))
	for _, shardEntry := range shardEntries {
		name := shardEntry.Name()
		tree := evaluationCleanupMarkerDirectoryTree{name: name, isDir: shardEntry.IsDir()}
		if !tree.isDir {
			trees = append(trees, tree)
			continue
		}
		shardFile, openErr := evaluationCleanupMarkerOpenDirectoryAt(indexFile, name, filepath.Join(root, index, name), false)
		if errors.Is(openErr, os.ErrNotExist) {
			return nil, rootOverflow, os.ErrNotExist
		}
		if openErr != nil {
			return nil, rootOverflow, openErr
		}
		shardInfo, statErr := shardFile.Stat()
		if statErr != nil {
			_ = shardFile.Close()
			return nil, rootOverflow, statErr
		}
		markerEntries, markerReadErr := shardFile.ReadDir(markerLimit + 1)
		if markerReadErr != nil && !errors.Is(markerReadErr, io.EOF) {
			_ = shardFile.Close()
			return nil, rootOverflow, markerReadErr
		}
		sort.Slice(markerEntries, func(i, j int) bool { return markerEntries[i].Name() < markerEntries[j].Name() })
		tree.overflow = len(markerEntries) > markerLimit
		if tree.overflow {
			markerEntries = markerEntries[:markerLimit]
		}
		if hook != nil {
			if hookErr := hook("after_directory_enumeration:" + name); hookErr != nil {
				_ = shardFile.Close()
				return nil, rootOverflow, hookErr
			}
		}
		for _, markerEntry := range markerEntries {
			item := evaluationCleanupMarkerDirectoryFile{name: markerEntry.Name(), isDir: markerEntry.IsDir()}
			if !item.isDir {
				markerFD, openMarkerErr := unix.Openat(int(shardFile.Fd()), item.name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
				if errors.Is(openMarkerErr, unix.ENOENT) {
					_ = shardFile.Close()
					return nil, rootOverflow, os.ErrNotExist
				}
				if openMarkerErr != nil {
					_ = shardFile.Close()
					return nil, rootOverflow, openMarkerErr
				}
				markerFile := os.NewFile(uintptr(markerFD), filepath.Join(root, index, name, item.name))
				if markerFile == nil {
					_ = unix.Close(markerFD)
					_ = shardFile.Close()
					return nil, rootOverflow, errors.New("evaluation cleanup marker descriptor is unavailable")
				}
				info, markerStatErr := markerFile.Stat()
				if markerStatErr != nil {
					_ = markerFile.Close()
					_ = shardFile.Close()
					return nil, rootOverflow, markerStatErr
				}
				if !info.Mode().IsRegular() || info.Mode().Perm() != ownerOnlyFileMode.Perm() {
					_ = markerFile.Close()
					_ = shardFile.Close()
					return nil, rootOverflow, errors.New("evaluation cleanup marker descriptor is not owner-only regular storage")
				}
				raw, markerReadErr := io.ReadAll(io.LimitReader(markerFile, maxBytes+1))
				_ = markerFile.Close()
				if markerReadErr != nil {
					_ = shardFile.Close()
					return nil, rootOverflow, markerReadErr
				}
				if int64(len(raw)) > maxBytes {
					_ = shardFile.Close()
					return nil, rootOverflow, errContinuationDurableFileOversized
				}
				item.raw = raw
			}
			tree.entries = append(tree.entries, item)
		}
		if hook != nil {
			if hookErr := hook("after_marker_read:" + name); hookErr != nil {
				_ = shardFile.Close()
				return nil, rootOverflow, hookErr
			}
		}
		finalShardInfo, finalStatErr := shardFile.Stat()
		if finalStatErr != nil || !os.SameFile(shardInfo, finalShardInfo) {
			_ = shardFile.Close()
			if finalStatErr != nil {
				return nil, rootOverflow, finalStatErr
			}
			return nil, rootOverflow, errors.New("evaluation cleanup marker shard descriptor changed during rebuild")
		}
		if pathErr := evaluationCleanupMarkerVerifyStablePath(filepath.Join(root, index, name), shardInfo); pathErr != nil {
			_ = shardFile.Close()
			return nil, rootOverflow, pathErr
		}
		_ = shardFile.Close()
		trees = append(trees, tree)
	}
	finalIndexInfo, finalStatErr := indexFile.Stat()
	if finalStatErr != nil {
		return nil, rootOverflow, finalStatErr
	}
	if !os.SameFile(indexInfo, finalIndexInfo) {
		return nil, rootOverflow, errors.New("evaluation cleanup marker index descriptor changed during rebuild")
	}
	if pathErr := evaluationCleanupMarkerVerifyStablePath(root, rootInfo); pathErr != nil {
		return nil, rootOverflow, pathErr
	}
	if pathErr := evaluationCleanupMarkerVerifyStablePath(filepath.Join(root, index), indexInfo); pathErr != nil {
		return nil, rootOverflow, pathErr
	}
	return trees, rootOverflow, nil
}

// timeNowUnixNano is a variable-free wrapper kept here so tests can reason
// about the descriptor algorithm without making the temporary name a stable
// externally observable identity.
func timeNowUnixNano() int64 { return time.Now().UnixNano() }

func readEvaluationCleanupMarkerFileBounded(root, index, shard, marker string, maxBytes int64) ([]byte, error) {
	return readEvaluationCleanupMarkerFileBoundedWithExpectedIndex(root, index, shard, marker, maxBytes, nil)
}

func readEvaluationCleanupMarkerFileBoundedWithExpectedIndex(root, index, shard, marker string, maxBytes int64, expectedIndex os.FileInfo) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, errContinuationDurableFileOversized
	}
	for _, component := range []string{index, marker} {
		if component == "" || component == "." || component == ".." || filepath.Base(component) != component {
			return nil, errors.New("evaluation cleanup marker path component is invalid")
		}
	}
	if shard != "" && (shard == "." || shard == ".." || filepath.Base(shard) != shard) {
		return nil, errors.New("evaluation cleanup marker path component is invalid")
	}
	// Open the root through the same descriptor-relative ancestor walk used by
	// writes and rebuild.  Reopening the pathname directly here would leave the
	// marker read vulnerable to an ancestor replacement between lookup steps.
	rootFile, err := evaluationCleanupMarkerOpenRootDirectory(root)
	if err != nil {
		return nil, err
	}
	defer rootFile.Close()
	rootInfo, err := rootFile.Stat()
	if err != nil {
		return nil, err
	}
	indexFD, err := unix.Openat(int(rootFile.Fd()), index, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	indexFile := os.NewFile(uintptr(indexFD), filepath.Join(root, index))
	if indexFile == nil {
		_ = unix.Close(indexFD)
		return nil, errors.New("evaluation cleanup marker index descriptor is unavailable")
	}
	defer indexFile.Close()
	indexInfo, err := indexFile.Stat()
	if err != nil {
		return nil, err
	}
	if expectedIndex != nil && !os.SameFile(expectedIndex, indexInfo) {
		return nil, errors.New("evaluation cleanup marker index was replaced before read")
	}
	parentFile := indexFile
	var shardFile *os.File
	var shardInfo os.FileInfo
	if shard != "" {
		shardFD, openErr := unix.Openat(int(indexFile.Fd()), shard, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(openErr, unix.ENOENT) {
			return nil, os.ErrNotExist
		}
		if openErr != nil {
			return nil, openErr
		}
		shardFile = os.NewFile(uintptr(shardFD), filepath.Join(root, index, shard))
		if shardFile == nil {
			_ = unix.Close(shardFD)
			return nil, errors.New("evaluation cleanup marker shard descriptor is unavailable")
		}
		defer shardFile.Close()
		shardInfo, err = shardFile.Stat()
		if err != nil {
			return nil, err
		}
		parentFile = shardFile
	}
	markerFD, err := unix.Openat(int(parentFile.Fd()), marker, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	markerFile := os.NewFile(uintptr(markerFD), filepath.Join(root, index, shard, marker))
	if markerFile == nil {
		_ = unix.Close(markerFD)
		return nil, errors.New("evaluation cleanup marker descriptor is unavailable")
	}
	defer markerFile.Close()
	info, err := markerFile.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != ownerOnlyFileMode.Perm() {
		return nil, errors.New("evaluation cleanup marker descriptor is not owner-only regular storage")
	}
	raw, err := io.ReadAll(io.LimitReader(markerFile, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxBytes {
		return nil, errContinuationDurableFileOversized
	}
	if shardFile != nil {
		finalShardInfo, statErr := shardFile.Stat()
		if statErr != nil {
			return nil, statErr
		}
		if !os.SameFile(shardInfo, finalShardInfo) {
			return nil, errors.New("evaluation cleanup marker shard descriptor changed during read")
		}
	}
	finalIndexInfo, statErr := indexFile.Stat()
	if statErr != nil {
		return nil, statErr
	}
	if !os.SameFile(indexInfo, finalIndexInfo) {
		return nil, errors.New("evaluation cleanup marker index descriptor changed during read")
	}
	if pathErr := evaluationCleanupMarkerVerifyStablePath(root, rootInfo); pathErr != nil {
		return nil, pathErr
	}
	if pathErr := evaluationCleanupMarkerVerifyStablePath(filepath.Join(root, index), indexInfo); pathErr != nil {
		return nil, pathErr
	}
	if shardFile != nil {
		if pathErr := evaluationCleanupMarkerVerifyStablePath(filepath.Join(root, index, shard), shardInfo); pathErr != nil {
			return nil, pathErr
		}
	}
	return raw, nil
}
