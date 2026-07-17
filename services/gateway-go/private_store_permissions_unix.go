//go:build !windows

package main

import (
	"errors"
	"os"
)

func enforceOwnerOnlyPermissions(path string, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("owner-only POSIX permission target is a symlink")
	}
	if info.Mode().Perm() == mode.Perm() {
		return nil
	}
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	info, err = os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != mode.Perm() {
		return errors.New("owner-only POSIX permission verification failed")
	}
	return nil
}

func ownerOnlyPermissionsCompliant(path string, mode os.FileMode) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("owner-only POSIX permission target is a symlink")
	}
	return info.Mode().Perm() == mode.Perm(), nil
}
