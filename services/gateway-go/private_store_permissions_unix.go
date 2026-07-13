//go:build !windows

package main

import (
	"errors"
	"os"
)

func enforceOwnerOnlyPermissions(path string, mode os.FileMode) error {
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != mode.Perm() {
		return errors.New("owner-only POSIX permission verification failed")
	}
	return nil
}
