//go:build (darwin && !ios) || (linux && !android)

package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const dreamTransferAuthoritySupported = true

var errDreamTransferPrivateFile = errors.New("private authority file unavailable")

func readDreamTransferWorkerKey(path string) (string, error) {
	parent, err := openDreamTransferAuthorityParent(path)
	if err != nil {
		return "", err
	}
	defer unix.Close(parent)
	fd, err := unix.Openat(parent, filepath.Base(path), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(fd), "worker key")
	defer file.Close()
	const maxKeyBytes = 4096
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG ||
		stat.Mode&0o7777 != 0o600 || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 ||
		stat.Size < 1 || stat.Size > maxKeyBytes {
		return "", errDreamTransferPrivateFile
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxKeyBytes+1))
	if err != nil || len(raw) > maxKeyBytes {
		return "", errDreamTransferPrivateFile
	}
	key := strings.TrimSpace(string(raw))
	if key == "" || strings.IndexFunc(key, func(r rune) bool { return r < 0x21 || r > 0x7e }) >= 0 {
		return "", errDreamTransferPrivateFile
	}
	return key, nil
}

type dreamTransferAuthorityFile struct {
	parent    int
	target    string
	staged    string
	handle    *os.File
	identity  unix.Stat_t
	published bool
}

func openDreamTransferAuthorityParent(path string) (int, error) {
	if !validDreamTransferAuthorityPath(path) {
		return -1, errDreamTransferPrivateFile
	}
	dir, err := normalizePrivateArtifactUnixParent(filepath.Dir(path))
	if err != nil {
		return -1, err
	}
	fd, err := openPrivateArtifactUnixParentMode(dir, false)
	if err != nil {
		return -1, err
	}
	if !dreamTransferOwnerDirectory(fd) {
		_ = unix.Close(fd)
		return -1, errDreamTransferPrivateFile
	}
	return fd, nil
}

func dreamTransferOwnerDirectory(fd int) bool {
	var stat unix.Stat_t
	return unix.Fstat(fd, &stat) == nil && stat.Mode&unix.S_IFMT == unix.S_IFDIR &&
		stat.Mode&0o7777 == 0o700 && stat.Uid == uint32(os.Geteuid())
}

func openDreamTransferAuthorityFile(path string) (*dreamTransferAuthorityFile, error) {
	fd, err := openDreamTransferAuthorityParent(path)
	if err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	name := filepath.Base(path)
	if err := unix.Fstatat(fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(err, unix.ENOENT) {
		_ = unix.Close(fd)
		return nil, errDreamTransferPrivateFile
	}
	return &dreamTransferAuthorityFile{parent: fd, target: name}, nil
}

func (f *dreamTransferAuthorityFile) stage(contents []byte) error {
	name, err := privateArtifactTempName("dream-authority")
	if err != nil {
		return err
	}
	fd, err := unix.Openat(f.parent, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	f.staged, f.handle = name, os.NewFile(uintptr(fd), name)
	if err := unix.Fstat(fd, &f.identity); err != nil {
		return err
	}
	// Apply the exact mode to our new descriptor, even under a restrictive umask.
	if f.identity.Mode&unix.S_IFMT != unix.S_IFREG || f.identity.Nlink != 1 ||
		f.identity.Uid != uint32(os.Geteuid()) || unix.Fchmod(fd, 0o600) != nil {
		return errDreamTransferPrivateFile
	}
	if n, err := f.handle.Write(contents); err != nil {
		return err
	} else if n != len(contents) {
		return io.ErrShortWrite
	}
	return f.handle.Sync()
}

func (f *dreamTransferAuthorityFile) owns(name string, exactMode bool) bool {
	var stat unix.Stat_t
	return name != "" && f.handle != nil &&
		unix.Fstatat(f.parent, name, &stat, unix.AT_SYMLINK_NOFOLLOW) == nil &&
		stat.Dev == f.identity.Dev && stat.Ino == f.identity.Ino && stat.Mode&unix.S_IFMT == unix.S_IFREG &&
		stat.Uid == uint32(os.Geteuid()) && stat.Nlink == 1 && (!exactMode || stat.Mode&0o7777 == 0o600)
}

func (f *dreamTransferAuthorityFile) publish() error {
	if !dreamTransferOwnerDirectory(f.parent) || !f.owns(f.staged, true) {
		return errDreamTransferPrivateFile
	}
	if err := renameDreamTransferAuthorityFile(f.parent, f.staged, f.target); err != nil {
		return err
	}
	f.staged, f.published = "", true
	return unix.Fsync(f.parent)
}

// Destroy only a name still bound to the file this invocation exclusively
// created. In particular, a pre-existing target is never a cleanup candidate.
func (f *dreamTransferAuthorityFile) removeOwned(name string) {
	if f.owns(name, false) && unix.Unlinkat(f.parent, name, 0) == nil {
		_ = unix.Fsync(f.parent)
	}
}

func (f *dreamTransferAuthorityFile) close() {
	f.removeOwned(f.staged)
	if f.handle != nil {
		_ = f.handle.Close()
	}
	_ = unix.Close(f.parent)
}

func publishDreamTransferAuthorityPair(proof, grant *dreamTransferAuthorityFile, expires time.Time) (err error) {
	defer func() {
		if err == nil {
			return
		}
		if grant.published {
			grant.removeOwned(grant.target)
		}
		if proof.published {
			// On failure invalidate the proof through our own open handle,
			// even if its name no longer admits a safe unlink.
			_ = proof.handle.Truncate(0)
			_ = proof.handle.Sync()
			proof.removeOwned(proof.target)
		}
	}()
	if err := proof.publish(); err != nil {
		return err
	}
	if !time.Now().Before(expires) {
		return errDreamTransferPrivateFile
	}
	return grant.publish()
}

func writeDreamTransferAuthorityPair(grantPath, proofPath string, grantBytes, proofBytes []byte, expires time.Time) error {
	proof, err := openDreamTransferAuthorityFile(proofPath)
	if err != nil {
		return err
	}
	defer proof.close()
	grant, err := openDreamTransferAuthorityFile(grantPath)
	if err != nil {
		return err
	}
	defer grant.close()
	if err := proof.stage(proofBytes); err != nil {
		return err
	}
	if err := grant.stage(grantBytes); err != nil {
		return err
	}
	return publishDreamTransferAuthorityPair(proof, grant, expires)
}
