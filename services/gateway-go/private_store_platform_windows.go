//go:build windows

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func ownerOnlyRootIdentity(path string) (string, error) {
	file, err := os.Open(path)
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
	return os.Open(path)
}

func openOwnerOnlyDirectoryAt(_ *os.File, _ string, path string) (*os.File, error) {
	return os.Open(path)
}

func enforceOwnerOnlyEntryAt(root string, _ *os.File, _ string, path string) (bool, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, false, err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return false, false, validateOwnerOnlyInternalSymlink(root, path)
	case info.IsDir():
		if err := enforceOwnerOnlyPermissions(path, ownerOnlyDirectoryMode); err != nil {
			return false, false, err
		}
		return true, true, nil
	case info.Mode().IsRegular():
		if err := enforceOwnerOnlyPermissions(path, ownerOnlyFileMode); err != nil {
			return false, false, err
		}
		return true, false, nil
	default:
		return false, false, fmt.Errorf("owner-only store contains unsupported filesystem entry")
	}
}

func lockOwnerOnlyMigration(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, ownerOnlyFileMode)
	if err != nil {
		return nil, err
	}
	if err := enforceOwnerOnlyPermissions(path, ownerOnlyFileMode); err != nil {
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
	return func() {
		_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped)
		_ = file.Close()
	}, nil
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
