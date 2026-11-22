//go:build darwin || linux

package main

import (
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Read without creating, chmodding or following the authority file. Recheck
// both directory and file after reading and pin the descriptor's change time.
func readDreamOwnerFile(path string) ([]byte, memoryEdgeLogFileStamp, bool) {
	var zero memoryEdgeLogFileStamp
	const limit = 4096
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, zero, false
	}
	parent := filepath.Dir(path)
	canonical, err := filepath.EvalSymlinks(parent)
	if err != nil || canonical != parent {
		return nil, zero, false
	}
	dir, err := openAgentTaskDirectoryNoFollow(parent)
	if err != nil {
		return nil, zero, false
	}
	defer dir.Close()
	dinfo, err := dir.Stat()
	if err != nil || dinfo.Mode().Perm() != 0700 {
		return nil, zero, false
	}
	fd, err := unix.Openat(int(dir.Fd()), filepath.Base(path), unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, zero, false
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	if _, err := validateAgentTaskRegularFile(f, limit); err != nil {
		return nil, zero, false
	}
	info, err := f.Stat()
	before, stampErr := memoryEdgeLogPlatformFileStampForFile(f)
	if err != nil || stampErr != nil || info.Mode().Perm() != 0600 || before.ChangeToken == "" {
		return nil, zero, false
	}
	raw, readErr := io.ReadAll(io.LimitReader(f, limit+1))
	after, stampErr := memoryEdgeLogPlatformFileStampForFile(f)
	live, fileErr := os.Lstat(path)
	dlive, dirErr := os.Lstat(parent)
	if readErr != nil || stampErr != nil || before != after || int64(len(raw)) != before.Size || len(raw) > limit ||
		fileErr != nil || !os.SameFile(info, live) || dirErr != nil || !os.SameFile(dinfo, dlive) || dlive.Mode().Perm() != 0700 {
		return nil, zero, false
	}
	return raw, after, true
}
