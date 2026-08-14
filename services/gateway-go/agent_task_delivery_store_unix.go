//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type agentTaskFileStat struct {
	raw    unix.Stat_t
	Size   int64
	Device uint64
	FileID uint64
}

func openAgentTaskDirectoryNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("task artifact directory descriptor is unavailable")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = file.Close()
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != uint32(os.Getuid()) || stat.Mode&0o077 != 0 {
		_ = file.Close()
		return nil, errors.New("task artifact directory is not an owner-only real directory")
	}
	return file, nil
}

func validateAgentTaskRegularFile(file *os.File, maxBytes int64) (agentTaskFileStat, error) {
	var result agentTaskFileStat
	if file == nil {
		return result, errors.New("task artifact file descriptor is unavailable")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return result, err
	}
	result = agentTaskFileStat{
		raw:    stat,
		Size:   stat.Size,
		Device: uint64(stat.Dev),
		FileID: uint64(stat.Ino),
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Getuid()) || stat.Mode&0o077 != 0 || stat.Nlink != 1 {
		return result, errors.New("task artifact file is not an owner-only unlinked-safe regular file")
	}
	if stat.Size < 0 || stat.Size > maxBytes {
		return result, errors.New("task artifact file exceeds its configured size bound")
	}
	return result, nil
}

func agentTaskUnixOpenFlags(mode agentTaskFileOpenMode) (int, error) {
	switch mode {
	case agentTaskFileReadOnly:
		return unix.O_RDONLY, nil
	case agentTaskFileReadWrite:
		return unix.O_RDWR, nil
	case agentTaskFileReadWriteCreate:
		return unix.O_RDWR | unix.O_CREAT, nil
	case agentTaskFileReadWriteCreateExclusive:
		return unix.O_RDWR | unix.O_CREAT | unix.O_EXCL, nil
	default:
		return 0, errors.New("task artifact file open mode is invalid")
	}
}

func openAgentTaskFileAt(directory *os.File, name string, openMode agentTaskFileOpenMode, mode uint32, maxBytes int64) (*os.File, error) {
	if directory == nil || strings.TrimSpace(name) == "" || filepath.Base(name) != name {
		return nil, errors.New("task artifact file descriptor request is invalid")
	}
	flags, err := agentTaskUnixOpenFlags(openMode)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(directory.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(directory.Name(), name))
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("task artifact file descriptor is unavailable")
	}
	if _, err := validateAgentTaskRegularFile(file, maxBytes); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func openAgentTaskFileNoFollow(path string, maxBytes int64) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("task artifact file descriptor is unavailable")
	}
	if _, err := validateAgentTaskRegularFile(file, maxBytes); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func openAgentTaskDirectoryAt(parent *os.File, name string, create bool) (*os.File, error) {
	if parent == nil || filepath.Base(name) != name || strings.TrimSpace(name) == "" {
		return nil, errors.New("task artifact shard descriptor request is invalid")
	}
	if create {
		if err := unix.Mkdirat(int(parent.Fd()), name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(parent.Name(), name))
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("task artifact shard descriptor is unavailable")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = file.Close()
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != uint32(os.Getuid()) || stat.Mode&0o077 != 0 {
		_ = file.Close()
		return nil, errors.New("task artifact shard is not an owner-only real directory")
	}
	return file, nil
}

func agentTaskFlockContext(ctx context.Context, file *os.File, exclusive bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if file == nil {
		return errors.New("task artifact namespace lock file is unavailable")
	}
	mode := unix.LOCK_SH
	if exclusive {
		mode = unix.LOCK_EX
	}
	for {
		err := unix.Flock(int(file.Fd()), mode|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func agentTaskUnlock(file *os.File) error {
	if file == nil {
		return nil
	}
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}

func agentTaskRenameAt(parent *os.File, oldName, newName string) error {
	if parent == nil || filepath.Base(oldName) != oldName || filepath.Base(newName) != newName {
		return errors.New("task artifact rename descriptor request is invalid")
	}
	return unix.Renameat(int(parent.Fd()), oldName, int(parent.Fd()), newName)
}

func agentTaskLinkAt(parent, source *os.File, sourceName, targetName string) error {
	if parent == nil || source == nil || filepath.Base(sourceName) != sourceName || filepath.Base(targetName) != targetName {
		return errors.New("task artifact link descriptor request is invalid")
	}
	return unix.Linkat(int(parent.Fd()), sourceName, int(parent.Fd()), targetName, 0)
}

func agentTaskUnlinkAt(parent *os.File, name string) error {
	if parent == nil || filepath.Base(name) != name {
		return errors.New("task artifact unlink descriptor request is invalid")
	}
	return unix.Unlinkat(int(parent.Fd()), name, 0)
}

func agentTaskSyncDirectory(directory *os.File) error {
	if directory == nil {
		return fmt.Errorf("task artifact directory descriptor is unavailable")
	}
	return unix.Fsync(int(directory.Fd()))
}
