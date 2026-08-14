//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type ownerOnlyWindowsAttributeTagInfo struct {
	FileAttributes uint32
	ReparseTag     uint32
}

const ownerOnlyAppendAccessMask = windows.GENERIC_READ | windows.FILE_APPEND_DATA

func openOwnerOnlyWindows(path string, access uint32, creation uint32) (*os.File, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pathPtr,
		access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		creation,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if err := enforceOwnerOnlyDescriptor(file, access != windows.GENERIC_READ); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func openOwnerOnlyReadPlatform(path string) (*os.File, error) {
	return openOwnerOnlyWindows(path, windows.GENERIC_READ, windows.OPEN_EXISTING)
}

func openMemoryCollectorReadPlatform(path string) (*os.File, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.GENERIC_READ|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if _, _, err := ownerOnlyWindowsHandleKind(handle); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func openOwnerOnlyAppendPlatform(path string) (*os.File, error) {
	// FILE_APPEND_DATA makes the kernel ignore the caller's current file
	// pointer and place each write at EOF atomically.  A generic read right is
	// retained for descriptor-bound Stat/Sync callers; FILE_WRITE_DATA is
	// deliberately absent so os.File.Write cannot fall back to seek-then-write.
	return openOwnerOnlyWindows(path, ownerOnlyAppendAccessMask, windows.OPEN_ALWAYS)
}

func openOwnerOnlyTruncatePlatform(path string) (*os.File, error) {
	return openOwnerOnlyWindows(path, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.OPEN_EXISTING)
}

func enforceOwnerOnlyDescriptor(file *os.File, writable bool) error {
	if file == nil {
		return fmt.Errorf("owner-only descriptor is unavailable")
	}
	handle := windows.Handle(file.Fd())
	var tag ownerOnlyWindowsAttributeTagInfo
	if err := windows.GetFileInformationByHandleEx(
		handle,
		windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&tag)),
		uint32(unsafe.Sizeof(tag)),
	); err != nil {
		return err
	}
	if tag.ReparseTag != 0 {
		return fmt.Errorf("owner-only descriptor is a reparse point")
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("owner-only descriptor is not a regular file")
	}
	if !ownerOnlyWindowsDescriptorModePermitted(info.Mode(), writable) {
		if writable {
			return fmt.Errorf("owner-only writable descriptor is read-only")
		}
		return fmt.Errorf("owner-only descriptor permission verification failed")
	}
	return nil
}

func ownerOnlyRootIdentity(path string) (string, error) {
	file, err := openOwnerOnlyRootDirectory(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"windows:%x:%x:%x",
		info.VolumeSerialNumber,
		info.FileIndexHigh,
		info.FileIndexLow,
	), nil
}

func openOwnerOnlyRootDirectory(path string) (*os.File, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.GENERIC_READ|windows.READ_CONTROL|windows.WRITE_DAC|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if err := verifyOwnerOnlyWindowsDirectoryHandle(handle, true); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func openOwnerOnlyDirectoryAt(parent *os.File, name string, path string) (*os.File, error) {
	if parent == nil {
		return nil, fmt.Errorf("owner-only parent directory handle is unavailable")
	}
	file, err := openOwnerOnlyRelativeHandle(parent, name, path, true)
	if err != nil {
		return nil, err
	}
	if err := verifyOwnerOnlyWindowsDirectoryHandle(windows.Handle(file.Fd()), true); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func enforceOwnerOnlyEntryAt(_ string, parent *os.File, name string, path string) (bool, bool, error) {
	if parent == nil {
		return false, false, fmt.Errorf("owner-only parent directory handle is unavailable")
	}
	file, err := openOwnerOnlyRelativeHandle(parent, name, path, false)
	if err != nil {
		return false, false, err
	}
	defer file.Close()
	isDirectory, isRegular, err := ownerOnlyWindowsHandleKind(windows.Handle(file.Fd()))
	if err != nil {
		return false, false, err
	}
	if !isDirectory && !isRegular {
		return false, false, fmt.Errorf("owner-only store contains unsupported filesystem entry")
	}
	if err := enforceOwnerOnlyHandle(windows.Handle(file.Fd()), isDirectory); err != nil {
		return false, false, err
	}
	if isDirectory {
		return true, true, nil
	}
	return true, false, nil
}

func openOwnerOnlyRelativeHandle(parent *os.File, name string, path string, directoryOnly bool) (*os.File, error) {
	if parent == nil {
		return nil, fmt.Errorf("owner-only parent directory handle is unavailable")
	}
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, err
	}
	attributes := uint32(windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE)
	oa := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: windows.Handle(parent.Fd()),
		ObjectName:    objectName,
		Attributes:    attributes,
	}
	options := uint32(windows.FILE_SYNCHRONOUS_IO_NONALERT | windows.FILE_OPEN_REPARSE_POINT | windows.FILE_OPEN_FOR_BACKUP_INTENT)
	if directoryOnly {
		options |= windows.FILE_DIRECTORY_FILE
	}
	var iosb windows.IO_STATUS_BLOCK
	var allocationSize int64
	var handle windows.Handle
	if err := windows.NtCreateFile(
		&handle,
		windows.FILE_GENERIC_READ|windows.READ_CONTROL|windows.WRITE_DAC|windows.FILE_READ_ATTRIBUTES,
		oa,
		&iosb,
		&allocationSize,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		options,
		0,
		0,
	); err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	return file, nil
}

func verifyOwnerOnlyWindowsDirectoryHandle(handle windows.Handle, requireDirectory bool) error {
	isDirectory, isRegular, err := ownerOnlyWindowsHandleKind(handle)
	if err != nil {
		return err
	}
	if requireDirectory && !isDirectory {
		return fmt.Errorf("owner-only directory handle is not a directory")
	}
	if !isDirectory && !isRegular {
		return fmt.Errorf("owner-only handle is not a supported filesystem entry")
	}
	return enforceOwnerOnlyHandle(handle, isDirectory)
}

func ownerOnlyWindowsHandleKind(handle windows.Handle) (bool, bool, error) {
	if handle == windows.InvalidHandle {
		return false, false, fmt.Errorf("owner-only handle is invalid")
	}
	var tag ownerOnlyWindowsAttributeTagInfo
	if err := windows.GetFileInformationByHandleEx(
		handle,
		windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&tag)),
		uint32(unsafe.Sizeof(tag)),
	); err != nil {
		return false, false, err
	}
	if tag.ReparseTag != 0 {
		return false, false, fmt.Errorf("owner-only store entry is a reparse point")
	}
	isDirectory := tag.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	return isDirectory, !isDirectory, nil
}

func lockOwnerOnlyMigration(path string) (func(), error) {
	file, err := openOwnerOnlyWindows(path, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.OPEN_ALWAYS)
	if err != nil {
		return nil, err
	}
	if err := ownerOnlyLockPathIdentityMatches(path, file); err != nil {
		_ = file.Close()
		return nil, err
	}
	overlapped := &windows.Overlapped{}
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped); err != nil {
		_ = file.Close()
		if err == windows.ERROR_LOCK_VIOLATION {
			return nil, errOwnerOnlyMigrationLocked
		}
		return nil, err
	}
	if err := ownerOnlyLockPathIdentityMatches(path, file); err != nil {
		_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped)
		_ = file.Close()
		return nil, err
	}
	return func() {
		_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped)
		_ = file.Close()
	}, nil
}

func ownerOnlyLockPathIdentityMatches(path string, file *os.File) error {
	if file == nil {
		return errOwnerOnlyLockPathChanged
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return errOwnerOnlyLockPathChanged
		}
		return err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return errOwnerOnlyLockPathChanged
	}
	descriptorInfo, err := file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(pathInfo, descriptorInfo) {
		return errOwnerOnlyLockPathChanged
	}
	return nil
}

func lockOwnerOnlyFileContextWithValidation(ctx context.Context, path string) (*ownerOnlyFileLock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	const pollInterval = 10 * time.Millisecond
	const maxIdentityRetries = 16
	identityRetries := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file, err := openOwnerOnlyWindows(path, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.OPEN_ALWAYS)
		if err != nil {
			return nil, err
		}
		if err := ownerOnlyLockPathIdentityMatches(path, file); err != nil {
			_ = file.Close()
			identityRetries++
			if identityRetries >= maxIdentityRetries {
				return nil, fmt.Errorf("%w: pre-lock identity verification retries exhausted", errOwnerOnlyLockPathChanged)
			}
			continue
		}
		overlapped := &windows.Overlapped{}
		lockErr := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
		if lockErr == nil {
			if identityErr := ownerOnlyLockPathIdentityMatches(path, file); identityErr == nil {
				return &ownerOnlyFileLock{
					unlock: func() {
						_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped)
						_ = file.Close()
					},
					validate: func() error { return ownerOnlyLockPathIdentityMatches(path, file) },
				}, nil
			}
			_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped)
			_ = file.Close()
			identityRetries++
			if identityRetries >= maxIdentityRetries {
				return nil, fmt.Errorf("%w: post-lock identity verification retries exhausted", errOwnerOnlyLockPathChanged)
			}
			continue
		}
		_ = file.Close()
		if lockErr != windows.ERROR_LOCK_VIOLATION {
			return nil, lockErr
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func lockOwnerOnlyFileContext(ctx context.Context, path string) (func(), error) {
	lease, err := lockOwnerOnlyFileContextWithValidation(ctx, path)
	if err != nil {
		return nil, err
	}
	return lease.unlock, nil
}

func lockOwnerOnlyFile(path string) (func(), error) {
	return lockOwnerOnlyFileContext(context.Background(), path)
}

func openBoundedRegularFileNoFollow(path string, maxBytes int64) (*os.File, int64, error) {
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, 0, err
	}
	handle, err := windows.CreateFile(
		encoded,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, 0, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, 0, err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		_ = windows.CloseHandle(handle)
		return nil, 0, fmt.Errorf("content blob is not a bounded regular file")
	}
	size := int64(uint64(info.FileSizeHigh)<<32 | uint64(info.FileSizeLow))
	if size < 0 || size > maxBytes {
		_ = windows.CloseHandle(handle)
		return nil, 0, fmt.Errorf("content blob is not a bounded regular file")
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, 0, fmt.Errorf("bounded regular file descriptor is unavailable")
	}
	return file, size, nil
}
func syncOwnerOnlyDirectory(_ string) error {
	// Windows rename durability is provided by the flushed temporary file and
	// protected state-file handle; directories cannot be opened with os.Open.
	return nil
}

func replaceOwnerOnlyFile(from string, to string) error {
	fromPtr, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	toPtr, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(
		fromPtr,
		toPtr,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}
