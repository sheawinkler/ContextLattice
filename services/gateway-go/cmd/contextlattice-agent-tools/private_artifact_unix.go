//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

func createPrivateArtifact(path string) (*privateArtifactPublication, error) {
	parentPath := filepath.Clean(filepath.Dir(path))
	var err error
	parentPath, err = normalizePrivateArtifactUnixParent(parentPath)
	if err != nil {
		return nil, err
	}
	parent, err := openPrivateArtifactUnixParent(parentPath)
	if err != nil {
		return nil, err
	}
	targetName := filepath.Base(filepath.Clean(path))
	if targetName == "" || targetName == "." || targetName == string(filepath.Separator) {
		_ = unix.Close(parent)
		return nil, errors.New("private artifact target name is invalid")
	}

	var file *os.File
	var tempName string
	for attempt := 0; attempt < privateArtifactTempAttempts; attempt++ {
		tempName, err = privateArtifactTempName(targetName)
		if err != nil {
			_ = unix.Close(parent)
			return nil, err
		}
		fd, openErr := unix.Openat(parent, tempName, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if errors.Is(openErr, unix.EEXIST) {
			continue
		}
		if openErr != nil {
			_ = unix.Close(parent)
			return nil, openErr
		}
		file = os.NewFile(uintptr(fd), filepath.Join(parentPath, tempName))
		if file == nil {
			_ = unix.Close(fd)
			_ = unix.Close(parent)
			return nil, errors.New("private artifact Unix handle conversion failed")
		}
		break
	}
	if file == nil {
		_ = unix.Close(parent)
		return nil, errors.New("private artifact temporary name attempts exhausted")
	}

	publication := &privateArtifactPublication{file: file}
	publication.commit = func() error {
		if err := unix.Renameat(parent, tempName, parent, targetName); err != nil {
			return err
		}
		publication.committed = true
		if err := unix.Fsync(parent); err != nil {
			return fmt.Errorf("private artifact committed but parent directory sync failed: %w", err)
		}
		return nil
	}
	publication.cleanup = func() {
		if !publication.committed {
			_ = unix.Unlinkat(parent, tempName, 0)
		}
		_ = file.Close()
		_ = unix.Close(parent)
	}
	return publication, nil
}

func normalizePrivateArtifactUnixParent(parentPath string) (string, error) {
	parentPath = filepath.Clean(parentPath)
	if runtime.GOOS != "darwin" || (parentPath != "/var" && !strings.HasPrefix(parentPath, "/var/")) {
		return parentPath, nil
	}

	// macOS exposes its per-user temporary tree through the immutable system
	// alias /var -> /private/var. Resolve only that root-owned alias, then keep
	// using descriptor-relative O_NOFOLLOW traversal for every remaining path.
	var rootStat unix.Stat_t
	if err := unix.Stat(string(filepath.Separator), &rootStat); err != nil {
		return "", err
	}
	if rootStat.Uid != 0 || os.FileMode(rootStat.Mode&0o777)&0o022 != 0 {
		return "", errors.New("private artifact macOS root alias parent is not trusted")
	}
	var aliasStat unix.Stat_t
	if err := unix.Lstat("/var", &aliasStat); err != nil {
		return "", err
	}
	if aliasStat.Mode&unix.S_IFMT != unix.S_IFLNK || aliasStat.Uid != 0 {
		return "", errors.New("private artifact macOS /var alias is not a root-owned symlink")
	}
	aliasTarget, err := os.Readlink("/var")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(aliasTarget) {
		aliasTarget = filepath.Join(string(filepath.Separator), aliasTarget)
	}
	aliasTarget = filepath.Clean(aliasTarget)
	var targetStat unix.Stat_t
	if err := unix.Stat(aliasTarget, &targetStat); err != nil {
		return "", err
	}
	if targetStat.Mode&unix.S_IFMT != unix.S_IFDIR || targetStat.Uid != 0 || os.FileMode(targetStat.Mode&0o777)&0o022 != 0 {
		return "", errors.New("private artifact macOS /var alias target is not trusted")
	}
	if parentPath == "/var" {
		return aliasTarget, nil
	}
	return filepath.Join(aliasTarget, strings.TrimPrefix(parentPath, "/var/")), nil
}

func openPrivateArtifactUnixParent(parentPath string) (int, error) {
	start := "."
	components := strings.Split(filepath.Clean(parentPath), string(filepath.Separator))
	if filepath.IsAbs(parentPath) {
		start = string(filepath.Separator)
	}
	current, err := unix.Open(start, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	for _, component := range components {
		if component == "" || component == "." {
			continue
		}
		next, openErr := unix.Openat(current, component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if errors.Is(openErr, unix.ENOENT) {
			if mkdirErr := unix.Mkdirat(current, component, 0o700); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				_ = unix.Close(current)
				return -1, mkdirErr
			}
			next, openErr = unix.Openat(current, component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		}
		if openErr != nil {
			_ = unix.Close(current)
			return -1, fmt.Errorf("open private artifact parent component %q without symlink traversal: %w", component, openErr)
		}
		_ = unix.Close(current)
		current = next
	}
	var stat unix.Stat_t
	if err := unix.Fstat(current, &stat); err != nil {
		_ = unix.Close(current)
		return -1, err
	}
	if err := privateArtifactUnixParentSafe(&stat); err != nil {
		_ = unix.Close(current)
		return -1, err
	}
	return current, nil
}

func privateArtifactUnixParentSafe(stat *unix.Stat_t) error {
	if stat == nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("private artifact parent is not a directory")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("private artifact parent owner does not match the current user: uid %d", stat.Uid)
	}
	permissions := os.FileMode(stat.Mode & 0o777)
	if permissions&0o022 != 0 {
		return fmt.Errorf("private artifact parent is group/other-writable: mode %04o", permissions)
	}
	return nil
}

func securePrivateArtifactFile(file *os.File, mode os.FileMode) error {
	mode = mode.Perm()
	if err := file.Chmod(mode); err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || os.FileMode(stat.Mode&0o777) != mode || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("private artifact POSIX handle verification failed")
	}
	return nil
}
