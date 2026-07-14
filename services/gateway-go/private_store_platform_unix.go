//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func ownerOnlyRootIdentity(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("owner-only root identity is unavailable")
	}
	return fmt.Sprintf("unix:%x:%x", uint64(stat.Dev), uint64(stat.Ino)), nil
}

func openOwnerOnlyRootDirectory(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func openOwnerOnlyDirectoryAt(parent *os.File, name string, path string) (*os.File, error) {
	fd, err := unix.Openat(
		int(parent.Fd()),
		name,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		0,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func enforceOwnerOnlyEntryAt(root string, parent *os.File, name string, path string) (bool, bool, error) {
	var before unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false, false, err
	}
	typeBits := before.Mode & unix.S_IFMT
	if typeBits == unix.S_IFLNK {
		return false, false, validateOwnerOnlyInternalSymlink(root, path)
	}
	isDirectory := typeBits == unix.S_IFDIR
	if !isDirectory && typeBits != unix.S_IFREG {
		return false, false, fmt.Errorf("owner-only store contains unsupported filesystem entry")
	}
	targetMode := ownerOnlyFileMode
	if isDirectory {
		targetMode = ownerOnlyDirectoryMode
	}
	if os.FileMode(before.Mode).Perm() == targetMode.Perm() {
		return false, isDirectory, nil
	}
	if err := unix.Fchmodat(
		int(parent.Fd()),
		name,
		uint32(targetMode.Perm()),
		unix.AT_SYMLINK_NOFOLLOW,
	); err != nil {
		return false, false, err
	}
	var after unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &after, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false, false, err
	}
	if after.Dev != before.Dev || after.Ino != before.Ino || after.Mode&unix.S_IFMT != typeBits {
		return false, false, fmt.Errorf("owner-only migration entry changed during enforcement")
	}
	if os.FileMode(after.Mode).Perm() != targetMode.Perm() {
		return false, false, fmt.Errorf("owner-only descriptor-relative permission verification failed")
	}
	return true, isDirectory, nil
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
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, errOwnerOnlyMigrationLocked
		}
		return nil, err
	}
	return func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	}, nil
}

func syncOwnerOnlyDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func replaceOwnerOnlyFile(from string, to string) error {
	return os.Rename(from, to)
}
