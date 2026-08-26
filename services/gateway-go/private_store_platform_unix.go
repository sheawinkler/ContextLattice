//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

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

// openOwnerOnlyReadPlatform binds reads to the descriptor that was opened with
// O_NOFOLLOW.  Callers must use fstat/read on this descriptor rather than
// reopening the path after validation; this closes the final-component
// symlink/rename race for the edge log and its recovery snapshots.
func openOwnerOnlyReadPlatform(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if err := enforceOwnerOnlyDescriptor(file, false); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// openMemoryCollectorReadPlatform provides the collector's descriptor-bound
// no-follow read without requiring that an out-of-band file has already gone
// through owner-only migration. The caller still verifies descriptor/path
// identity and content before publishing a result.
func openMemoryCollectorReadPlatform(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func openOwnerOnlyAppendPlatform(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_APPEND|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(ownerOnlyFileMode.Perm()))
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if err := enforceOwnerOnlyDescriptor(file, true); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func openOwnerOnlyTruncatePlatform(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(ownerOnlyFileMode.Perm()))
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if err := enforceOwnerOnlyDescriptor(file, true); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func enforceOwnerOnlyDescriptor(file *os.File, writable bool) error {
	if file == nil {
		return fmt.Errorf("owner-only descriptor is unavailable")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("owner-only descriptor is not a regular file")
	}
	if writable {
		if os.FileMode(stat.Mode).Perm() != ownerOnlyFileMode.Perm() {
			if err := unix.Fchmod(int(file.Fd()), uint32(ownerOnlyFileMode.Perm())); err != nil {
				return err
			}
			if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
				return err
			}
		}
	}
	if os.FileMode(stat.Mode).Perm() != ownerOnlyFileMode.Perm() {
		return fmt.Errorf("owner-only descriptor permission verification failed")
	}
	return nil
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
	// Linux only gained flag-aware fchmodat2 in kernel 6.5. Calling
	// Fchmodat with AT_SYMLINK_NOFOLLOW therefore fails with ENOSYS on still
	// supported 6.1 hosts. Bind the entry to a no-follow descriptor first and
	// apply the mode to that descriptor instead. The identity checks before and
	// after chmod preserve the same rename/symlink race protection without a
	// kernel-version dependency.
	openFlags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if isDirectory {
		openFlags |= unix.O_DIRECTORY
	}
	fd, err := unix.Openat(int(parent.Fd()), name, openFlags, 0)
	if err != nil {
		return false, false, err
	}
	defer unix.Close(fd)

	var bound unix.Stat_t
	if err := unix.Fstat(fd, &bound); err != nil {
		return false, false, err
	}
	if bound.Dev != before.Dev || bound.Ino != before.Ino || bound.Mode&unix.S_IFMT != typeBits {
		return false, false, fmt.Errorf("owner-only migration entry changed during enforcement")
	}
	if err := unix.Fchmod(fd, uint32(targetMode.Perm())); err != nil {
		return false, false, err
	}
	if err := unix.Fstat(fd, &bound); err != nil {
		return false, false, err
	}
	var after unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &after, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false, false, err
	}
	if after.Dev != before.Dev || after.Ino != before.Ino || after.Mode&unix.S_IFMT != typeBits ||
		bound.Dev != before.Dev || bound.Ino != before.Ino || bound.Mode&unix.S_IFMT != typeBits {
		return false, false, fmt.Errorf("owner-only migration entry changed during enforcement")
	}
	if os.FileMode(bound.Mode).Perm() != targetMode.Perm() || os.FileMode(after.Mode).Perm() != targetMode.Perm() {
		return false, false, fmt.Errorf("owner-only descriptor-relative permission verification failed")
	}
	return true, isDirectory, nil
}

func lockOwnerOnlyMigration(path string) (func(), error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(ownerOnlyFileMode.Perm()))
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if err := enforceOwnerOnlyDescriptor(file, true); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := ownerOnlyLockPathIdentityMatches(path, file); err != nil {
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
	if err := ownerOnlyLockPathIdentityMatches(path, file); err != nil {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
		return nil, err
	}
	return func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	}, nil
}

func ownerOnlyLockPathIdentityMatches(path string, file *os.File) error {
	if file == nil {
		return errOwnerOnlyLockPathChanged
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
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

// lockOwnerOnlyFileContext acquires the descriptor-bound lock without an
// uncancellable blocking flock.  Both pre-lock and post-lock identity checks
// bind the pathname to the descriptor, so a rename/replacement cannot create a
// second lock object behind an already-open descriptor.
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
		fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(ownerOnlyFileMode.Perm()))
		if err != nil {
			return nil, err
		}
		file := os.NewFile(uintptr(fd), path)
		if err := enforceOwnerOnlyDescriptor(file, true); err != nil {
			_ = file.Close()
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
		lockErr := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if lockErr == nil {
			if identityErr := ownerOnlyLockPathIdentityMatches(path, file); identityErr == nil {
				return &ownerOnlyFileLock{
					unlock: func() {
						_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
						_ = file.Close()
					},
					validate: func() error { return ownerOnlyLockPathIdentityMatches(path, file) },
				}, nil
			} else {
				_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
				_ = file.Close()
				identityRetries++
				if identityRetries >= maxIdentityRetries {
					return nil, fmt.Errorf("%w: post-lock identity verification retries exhausted", errOwnerOnlyLockPathChanged)
				}
				continue
			}
		}
		_ = file.Close()
		if !errors.Is(lockErr, unix.EAGAIN) && !errors.Is(lockErr, unix.EWOULDBLOCK) {
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

// lockOwnerOnlyFile waits for an exclusive advisory lock for legacy callers.
func lockOwnerOnlyFile(path string) (func(), error) {
	return lockOwnerOnlyFileContext(context.Background(), path)
}

// openBoundedRegularFileNoFollow returns a descriptor whose identity and type
// were validated after an O_NOFOLLOW open. Callers must use this descriptor for
// every byte read; no path-based reopen is permitted after validation.
func openBoundedRegularFileNoFollow(path string, maxBytes int64) (*os.File, int64, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, 0, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, 0, errors.New("bounded regular file descriptor is unavailable")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = file.Close()
		return nil, 0, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Size < 0 || stat.Size > maxBytes {
		_ = file.Close()
		return nil, 0, errors.New("content blob is not a bounded regular file")
	}
	return file, stat.Size, nil
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
