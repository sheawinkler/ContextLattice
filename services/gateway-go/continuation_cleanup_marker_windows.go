//go:build windows

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const evaluationCleanupWindowsFileRenameInformationEx = 65

var evaluationCleanupWindowsTempCounter atomic.Uint64

// acquireEvaluationCleanupMarkerMigrationOwnerLock is the Windows counterpart
// to the Unix descriptor-relative fence. The lock file is opened beneath a
// verified root handle with OBJ_DONT_REPARSE, then held with LockFileEx for the
// complete pointer/receipt publication transaction.
func acquireEvaluationCleanupMarkerMigrationOwnerLock(root string) (func(), error) {
	rootFile, err := openEvaluationCleanupWindowsRoot(root, true)
	if err != nil {
		return nil, err
	}
	lockName := continuationEvaluationCleanupMarkerMigrationOwnerLockFile
	handle, err := openEvaluationCleanupWindowsRelative(windows.Handle(rootFile.Fd()), lockName, false, false, true)
	if errors.Is(err, os.ErrNotExist) {
		handle, err = openEvaluationCleanupWindowsRelative(windows.Handle(rootFile.Fd()), lockName, false, true, true)
		if status, ok := err.(windows.NTStatus); ok && status == windows.STATUS_OBJECT_NAME_COLLISION {
			handle, err = openEvaluationCleanupWindowsRelative(windows.Handle(rootFile.Fd()), lockName, false, false, true)
		}
	}
	if err != nil {
		_ = rootFile.Close()
		return nil, err
	}
	file := os.NewFile(uintptr(handle), filepath.Join(root, lockName))
	if file == nil {
		_ = windows.CloseHandle(handle)
		_ = rootFile.Close()
		return nil, errors.New("evaluation cleanup marker owner lock descriptor is unavailable")
	}
	closeFiles := func() {
		_ = file.Close()
		_ = rootFile.Close()
	}
	if err := verifyEvaluationCleanupWindowsRegular(file); err != nil {
		closeFiles()
		return nil, err
	}
	overlapped := &windows.Overlapped{}
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped); err != nil {
		closeFiles()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, errOwnerOnlyMigrationLocked
		}
		return nil, err
	}
	return func() {
		_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped)
		closeFiles()
	}, nil
}

// NtCreateFile's RootDirectory is the Windows equivalent of openat(2).  The
// queue root is opened once by path; every index, shard, temporary file, and
// final marker below is then opened relative to an already verified handle.
// This matters because OPEN_REPARSE_POINT on a full descendant path still
// permits an ancestor junction to redirect the lookup.
func openEvaluationCleanupWindowsRoot(path string, write bool) (*os.File, error) {
	clean := filepath.Clean(strings.TrimSpace(path))
	if clean == "" || !filepath.IsAbs(clean) {
		return nil, errors.New("evaluation cleanup marker root must be an absolute path")
	}
	volume := filepath.VolumeName(clean)
	if volume == "" {
		return nil, errors.New("evaluation cleanup marker root must have an explicit volume")
	}
	remainder := strings.TrimPrefix(clean, volume)
	remainder = strings.TrimLeft(remainder, `\\/`)
	volumeRoot := volume + string(filepath.Separator)
	name, err := windows.UTF16PtrFromString(volumeRoot)
	if err != nil {
		return nil, err
	}
	access := uint32(windows.FILE_GENERIC_READ)
	if write {
		access |= windows.FILE_GENERIC_WRITE | windows.DELETE
	}
	handle, err := windows.CreateFile(
		name,
		access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), volumeRoot)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("evaluation cleanup marker root descriptor is unavailable")
	}
	if err := verifyEvaluationCleanupWindowsDirectory(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	parts := strings.FieldsFunc(remainder, func(r rune) bool { return r == '\\' || r == '/' })
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			_ = file.Close()
			return nil, errors.New("evaluation cleanup marker root contains an invalid ancestor component")
		}
		nextHandle, openErr := openEvaluationCleanupWindowsRelative(windows.Handle(file.Fd()), part, true, false, write)
		if errors.Is(openErr, os.ErrNotExist) {
			_ = file.Close()
			return nil, os.ErrNotExist
		}
		if openErr != nil {
			_ = file.Close()
			return nil, openErr
		}
		next := os.NewFile(uintptr(nextHandle), filepath.Join(file.Name(), part))
		if next == nil {
			_ = windows.CloseHandle(nextHandle)
			_ = file.Close()
			return nil, errors.New("evaluation cleanup marker ancestor descriptor is unavailable")
		}
		if verifyErr := verifyEvaluationCleanupWindowsDirectory(next); verifyErr != nil {
			_ = next.Close()
			_ = file.Close()
			return nil, verifyErr
		}
		_ = file.Close()
		file = next
	}
	return file, nil
}

func verifyEvaluationCleanupWindowsDirectory(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("evaluation cleanup marker ancestor is not a real directory")
	}
	var byHandle windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &byHandle); err != nil {
		return err
	}
	if byHandle.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("evaluation cleanup marker ancestor is a reparse point")
	}
	return nil
}

func openEvaluationCleanupWindowsRelative(parent windows.Handle, name string, directory, create, write bool) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: parent,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	access := uint32(windows.FILE_GENERIC_READ)
	if write {
		access |= windows.FILE_GENERIC_WRITE | windows.DELETE
	}
	options := uint32(windows.FILE_OPEN_REPARSE_POINT | windows.FILE_OPEN_FOR_BACKUP_INTENT | windows.FILE_SYNCHRONOUS_IO_NONALERT)
	if directory {
		options |= windows.FILE_DIRECTORY_FILE
		access |= windows.FILE_LIST_DIRECTORY
	} else {
		options |= windows.FILE_NON_DIRECTORY_FILE | windows.FILE_WRITE_THROUGH
	}
	disposition := uint32(windows.FILE_OPEN)
	if create {
		disposition = windows.FILE_CREATE
	}
	var handle windows.Handle
	if err := windows.NtCreateFile(
		&handle,
		access,
		attributes,
		&windows.IO_STATUS_BLOCK{},
		nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		disposition,
		options,
		0,
		0,
	); err != nil {
		if status, ok := err.(windows.NTStatus); ok && status == windows.STATUS_OBJECT_NAME_NOT_FOUND {
			return windows.InvalidHandle, os.ErrNotExist
		}
		return windows.InvalidHandle, err
	}
	return handle, nil
}

func openEvaluationCleanupWindowsDirectoryAt(parent *os.File, name, path string, write, create bool) (*os.File, error) {
	handle, err := openEvaluationCleanupWindowsRelative(windows.Handle(parent.Fd()), name, true, false, write)
	if errors.Is(err, os.ErrNotExist) && create {
		handle, err = openEvaluationCleanupWindowsRelative(windows.Handle(parent.Fd()), name, true, true, write)
		if err == nil {
			if flushErr := evaluationCleanupWindowsFlush(parent); flushErr != nil {
				_ = windows.CloseHandle(handle)
				return nil, &ownerOnlyAtomicWriteError{Operation: "sync descriptor-relative marker directory parent", Committed: true, Err: flushErr}
			}
		}
	}
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("evaluation cleanup marker directory descriptor is unavailable")
	}
	if err := verifyEvaluationCleanupWindowsDirectory(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func evaluationCleanupWindowsFlush(file *os.File) error {
	if file == nil {
		return nil
	}
	return windows.FlushFileBuffers(windows.Handle(file.Fd()))
}

func ensureEvaluationCleanupMarkerDirectoriesDurable(root, index, shard string, syncHook func(string) error) error {
	if !evaluationCleanupMarkerComponentValid(index, false) || !evaluationCleanupMarkerComponentValid(shard, true) {
		return errors.New("evaluation cleanup marker directory component is invalid")
	}
	rootFile, err := openEvaluationCleanupWindowsRoot(root, true)
	if err != nil {
		return err
	}
	defer rootFile.Close()
	rootInfo, err := rootFile.Stat()
	if err != nil {
		return err
	}
	indexFile, err := openEvaluationCleanupWindowsDirectoryAt(rootFile, index, filepath.Join(root, index), true, true)
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
	shardFile, err := openEvaluationCleanupWindowsDirectoryAt(indexFile, shard, filepath.Join(root, index, shard), true, true)
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

type evaluationCleanupWindowsRenameInformationEx struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

func renameEvaluationCleanupWindowsRelative(file, parent *os.File, name string) error {
	encoded, err := windows.UTF16FromString(name)
	if err != nil {
		return err
	}
	encoded = encoded[:len(encoded)-1]
	nameOffset := int(unsafe.Offsetof(evaluationCleanupWindowsRenameInformationEx{}.FileName))
	buffer := make([]byte, nameOffset+len(encoded)*2)
	info := (*evaluationCleanupWindowsRenameInformationEx)(unsafe.Pointer(&buffer[0]))
	info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	info.RootDirectory = windows.Handle(parent.Fd())
	info.FileNameLength = uint32(len(encoded) * 2)
	for index, value := range encoded {
		buffer[nameOffset+index*2] = byte(value)
		buffer[nameOffset+index*2+1] = byte(value >> 8)
	}
	return windows.NtSetInformationFile(
		windows.Handle(file.Fd()),
		&windows.IO_STATUS_BLOCK{},
		&buffer[0],
		uint32(len(buffer)),
		evaluationCleanupWindowsFileRenameInformationEx,
	)
}

type evaluationCleanupWindowsDispositionInformationEx struct {
	Flags uint32
}

func deleteEvaluationCleanupWindowsHandle(file *os.File) {
	if file == nil {
		return
	}
	disposition := evaluationCleanupWindowsDispositionInformationEx{Flags: windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_POSIX_SEMANTICS | windows.FILE_DISPOSITION_ON_CLOSE}
	_ = windows.NtSetInformationFile(
		windows.Handle(file.Fd()),
		&windows.IO_STATUS_BLOCK{},
		(*byte)(unsafe.Pointer(&disposition)),
		uint32(unsafe.Sizeof(disposition)),
		windows.FileDispositionInformationEx,
	)
}

func reconcileEvaluationCleanupWindowsOrphans(parent *os.File, targetName string) error {
	entries, err := parent.ReadDir(1024 + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(entries) > 1024 {
		return errors.New("evaluation cleanup marker temporary inventory exceeds bounded orphan limit")
	}
	prefix := "." + targetName + ".tmp-marker-"
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		handle, openErr := openEvaluationCleanupWindowsRelative(windows.Handle(parent.Fd()), entry.Name(), false, false, true)
		if openErr != nil {
			if errors.Is(openErr, os.ErrNotExist) {
				continue
			}
			return openErr
		}
		file := os.NewFile(uintptr(handle), entry.Name())
		if file == nil {
			_ = windows.CloseHandle(handle)
			return errors.New("evaluation cleanup marker orphan descriptor is unavailable")
		}
		if verifyErr := verifyEvaluationCleanupWindowsRegular(file); verifyErr != nil {
			_ = file.Close()
			return verifyErr
		}
		deleteEvaluationCleanupWindowsHandle(file)
		_ = file.Close()
	}
	return nil
}

func writeEvaluationCleanupMarkerDurable(root, index, shard, name string, content []byte) error {
	if !evaluationCleanupMarkerComponentValid(index, false) || !evaluationCleanupMarkerComponentValid(shard, true) || !evaluationCleanupMarkerComponentValid(name, false) {
		return errors.New("evaluation cleanup marker write component is invalid")
	}
	rootFile, err := openEvaluationCleanupWindowsRoot(root, true)
	if err != nil {
		return err
	}
	defer rootFile.Close()
	rootInfo, err := rootFile.Stat()
	if err != nil {
		return err
	}
	indexFile, err := openEvaluationCleanupWindowsDirectoryAt(rootFile, index, filepath.Join(root, index), true, true)
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
		shardFile, err = openEvaluationCleanupWindowsDirectoryAt(indexFile, shard, filepath.Join(root, index, shard), true, true)
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
	if err := reconcileEvaluationCleanupWindowsOrphans(parentFile, name); err != nil {
		return err
	}
	// A crash may leave a temporary handle/name behind. Unique names make that
	// orphan harmless to the next transaction; the prefix is intentionally
	// exact and bounded so an operator can inventory/remove only abandoned
	// temporary artifacts without touching a marker or control record.
	tempName := fmt.Sprintf(".%s.tmp-marker-%x-%x", name, uint64(os.Getpid()), uint64(time.Now().UnixNano())^evaluationCleanupWindowsTempCounter.Add(1))
	tempHandle, err := openEvaluationCleanupWindowsRelative(windows.Handle(parentFile.Fd()), tempName, false, true, true)
	if err != nil {
		return err
	}
	tempFile := os.NewFile(uintptr(tempHandle), filepath.Join(root, index, shard, tempName))
	if tempFile == nil {
		_ = windows.CloseHandle(tempHandle)
		return errors.New("evaluation cleanup marker temporary descriptor is unavailable")
	}
	removeTemp := true
	defer func() {
		if removeTemp {
			deleteEvaluationCleanupWindowsHandle(tempFile)
		}
		_ = tempFile.Close()
	}()
	if _, err := tempFile.Write(content); err != nil {
		return err
	}
	if err := tempFile.Sync(); err != nil {
		return err
	}
	if err := renameEvaluationCleanupWindowsRelative(tempFile, parentFile, name); err != nil {
		return err
	}
	removeTemp = false
	// Flush the exact descriptor chain, including every ancestor newly opened
	// for this transaction. A directory flush failure is reported as committed
	// custody so startup reconciliation can retry the durable receipt.
	if err := evaluationCleanupWindowsFlush(rootFile); err != nil {
		return &ownerOnlyAtomicWriteError{Operation: "sync Windows marker root", Committed: true, Err: err}
	}
	if err := evaluationCleanupWindowsFlush(indexFile); err != nil {
		return &ownerOnlyAtomicWriteError{Operation: "sync Windows marker index", Committed: true, Err: err}
	}
	if err := evaluationCleanupWindowsFlush(shardFile); err != nil {
		return &ownerOnlyAtomicWriteError{Operation: "sync Windows marker shard", Committed: true, Err: err}
	}
	markerHandle, err := openEvaluationCleanupWindowsRelative(windows.Handle(parentFile.Fd()), name, false, false, false)
	if err != nil {
		return &ownerOnlyAtomicWriteError{Operation: "verify Windows marker replacement", Committed: true, Err: err}
	}
	markerFile := os.NewFile(uintptr(markerHandle), filepath.Join(root, index, shard, name))
	if markerFile == nil {
		_ = windows.CloseHandle(markerHandle)
		return &ownerOnlyAtomicWriteError{Operation: "verify Windows marker replacement", Committed: true, Err: errors.New("marker descriptor unavailable")}
	}
	defer markerFile.Close()
	if err := verifyEvaluationCleanupWindowsRegular(markerFile); err != nil {
		return &ownerOnlyAtomicWriteError{Operation: "verify Windows marker replacement", Committed: true, Err: err}
	}
	if pathErr := evaluationCleanupMarkerVerifyStablePath(root, rootInfo); pathErr != nil {
		return &ownerOnlyAtomicWriteError{Operation: "verify Windows marker root", Committed: true, Err: pathErr}
	}
	if pathErr := evaluationCleanupMarkerVerifyStablePath(filepath.Join(root, index), indexInfo); pathErr != nil {
		return &ownerOnlyAtomicWriteError{Operation: "verify Windows marker index", Committed: true, Err: pathErr}
	}
	if shardFile != nil {
		if pathErr := evaluationCleanupMarkerVerifyStablePath(filepath.Join(root, index, shard), shardInfo); pathErr != nil {
			return &ownerOnlyAtomicWriteError{Operation: "verify Windows marker shard", Committed: true, Err: pathErr}
		}
	}
	return nil
}

func verifyEvaluationCleanupWindowsRegular(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("evaluation cleanup marker descriptor is not a regular file")
	}
	var byHandle windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &byHandle); err != nil {
		return err
	}
	if byHandle.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("evaluation cleanup marker descriptor is a reparse point")
	}
	return nil
}

func readEvaluationCleanupMarkerFileBounded(root, index, shard, marker string, maxBytes int64) ([]byte, error) {
	return readEvaluationCleanupMarkerFileBoundedWithExpectedIndex(root, index, shard, marker, maxBytes, nil)
}

func readEvaluationCleanupMarkerFileBoundedWithExpectedIndex(root, index, shard, marker string, maxBytes int64, expectedIndex os.FileInfo) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, errContinuationDurableFileOversized
	}
	if !evaluationCleanupMarkerComponentValid(index, false) || !evaluationCleanupMarkerComponentValid(shard, true) || !evaluationCleanupMarkerComponentValid(marker, false) {
		return nil, errors.New("evaluation cleanup marker path component is invalid")
	}
	rootFile, err := openEvaluationCleanupWindowsRoot(root, false)
	if err != nil {
		return nil, err
	}
	defer rootFile.Close()
	rootInfo, err := rootFile.Stat()
	if err != nil {
		return nil, err
	}
	indexFile, err := openEvaluationCleanupWindowsDirectoryAt(rootFile, index, filepath.Join(root, index), false, false)
	if err != nil {
		return nil, err
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
		shardFile, err = openEvaluationCleanupWindowsDirectoryAt(indexFile, shard, filepath.Join(root, index, shard), false, false)
		if err != nil {
			return nil, err
		}
		defer shardFile.Close()
		shardInfo, err = shardFile.Stat()
		if err != nil {
			return nil, err
		}
		parentFile = shardFile
	}
	handle, err := openEvaluationCleanupWindowsRelative(windows.Handle(parentFile.Fd()), marker, false, false, false)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), filepath.Join(root, index, shard, marker))
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("evaluation cleanup marker descriptor is unavailable")
	}
	defer file.Close()
	if err := verifyEvaluationCleanupWindowsRegular(file); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
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

// Keep the RootDirectory handle chain alive through enumeration and every
// child read. This closes the Windows equivalent of the Unix ABA window: a
// junction/reparse replacement cannot redirect an already-open parent handle.
func readEvaluationCleanupMarkerFilesBounded(root, index, shard string, limit int, maxBytes int64, hook func(string) error) ([]evaluationCleanupMarkerDirectoryFile, bool, error) {
	if limit <= 0 || maxBytes <= 0 || !evaluationCleanupMarkerComponentValid(index, false) || !evaluationCleanupMarkerComponentValid(shard, true) {
		return nil, false, errors.New("evaluation cleanup marker directory bound or component is invalid")
	}
	rootFile, err := openEvaluationCleanupWindowsRoot(root, false)
	if err != nil {
		return nil, false, err
	}
	defer rootFile.Close()
	rootInfo, err := rootFile.Stat()
	if err != nil {
		return nil, false, err
	}
	indexFile, err := openEvaluationCleanupWindowsDirectoryAt(rootFile, index, filepath.Join(root, index), false, false)
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
		shardFile, err = openEvaluationCleanupWindowsDirectoryAt(indexFile, shard, filepath.Join(root, index, shard), false, false)
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
			handle, openErr := openEvaluationCleanupWindowsRelative(windows.Handle(parentFile.Fd()), item.name, false, false, false)
			if openErr != nil {
				return nil, false, openErr
			}
			markerFile := os.NewFile(uintptr(handle), filepath.Join(root, index, shard, item.name))
			if markerFile == nil {
				_ = windows.CloseHandle(handle)
				return nil, false, errors.New("evaluation cleanup marker descriptor is unavailable")
			}
			if verifyErr := verifyEvaluationCleanupWindowsRegular(markerFile); verifyErr != nil {
				_ = markerFile.Close()
				return nil, false, verifyErr
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

// readEvaluationCleanupMarkerTreeBounded keeps the verified RootDirectory
// chain alive from shard enumeration through every child open/read. A full
// descendant path reopen would allow a junction replacement (including an
// ABA replacement/restore) to redirect a rebuild after its initial check.
func readEvaluationCleanupMarkerTreeBounded(root, index string, shardLimit, markerLimit int, maxBytes int64, expectedIndex os.FileInfo, hook func(string) error) ([]evaluationCleanupMarkerDirectoryTree, bool, error) {
	if shardLimit <= 0 || markerLimit <= 0 || maxBytes <= 0 || !evaluationCleanupMarkerComponentValid(index, false) {
		return nil, false, errors.New("evaluation cleanup marker tree bound or component is invalid")
	}
	rootFile, err := openEvaluationCleanupWindowsRoot(root, false)
	if err != nil {
		return nil, false, err
	}
	defer rootFile.Close()
	rootInfo, err := rootFile.Stat()
	if err != nil {
		return nil, false, err
	}
	indexFile, err := openEvaluationCleanupWindowsDirectoryAt(rootFile, index, filepath.Join(root, index), false, false)
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
		shardFile, openErr := openEvaluationCleanupWindowsDirectoryAt(indexFile, name, filepath.Join(root, index, name), false, false)
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
				handle, openMarkerErr := openEvaluationCleanupWindowsRelative(windows.Handle(shardFile.Fd()), item.name, false, false, false)
				if errors.Is(openMarkerErr, os.ErrNotExist) {
					_ = shardFile.Close()
					return nil, rootOverflow, os.ErrNotExist
				}
				if openMarkerErr != nil {
					_ = shardFile.Close()
					return nil, rootOverflow, openMarkerErr
				}
				markerFile := os.NewFile(uintptr(handle), filepath.Join(root, index, name, item.name))
				if markerFile == nil {
					_ = windows.CloseHandle(handle)
					_ = shardFile.Close()
					return nil, rootOverflow, errors.New("evaluation cleanup marker descriptor is unavailable")
				}
				if verifyErr := verifyEvaluationCleanupWindowsRegular(markerFile); verifyErr != nil {
					_ = markerFile.Close()
					_ = shardFile.Close()
					return nil, rootOverflow, verifyErr
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
