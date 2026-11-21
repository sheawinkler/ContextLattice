//go:build !windows

package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func acquireConflictSourceLock(path string) (func(), error) {
	bad := errors.New("private research lock unavailable")
	dir, err := normalizePrivateArtifactUnixParent(filepath.Dir(path))
	if err != nil {
		return nil, bad
	}
	parent, err := openPrivateArtifactUnixParentMode(dir, false)
	if err != nil {
		return nil, bad
	}
	defer unix.Close(parent)
	var st unix.Stat_t
	if unix.Fstat(parent, &st) != nil || st.Mode&0777 != 0700 {
		return nil, bad
	}
	fd, err := unix.Openat(parent, filepath.Base(path), unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, bad
	}
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0777 != 0600 ||
		st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 || unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB) != nil {
		unix.Close(fd)
		return nil, bad
	}
	return func() { unix.Close(fd) }, nil
}

// A read must never create missing directories, follow a link, or repair a grant.
func readConflictPrivateFile(path string, limit int64) ([]byte, error) {
	bad := errors.New("private research file unavailable")
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || limit < 1 {
		return nil, bad
	}
	dir, err := normalizePrivateArtifactUnixParent(filepath.Dir(path))
	if err != nil {
		return nil, bad
	}
	parent, err := openPrivateArtifactUnixParentMode(dir, false)
	if err != nil {
		return nil, bad
	}
	defer unix.Close(parent)
	var st unix.Stat_t
	if unix.Fstat(parent, &st) != nil || st.Mode&0777 != 0700 {
		return nil, bad
	}
	fd, err := unix.Openat(parent, filepath.Base(path), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0777 != 0600 ||
		st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 || st.Size > limit {
		return nil, bad
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, bad
	}
	return data, nil
}
