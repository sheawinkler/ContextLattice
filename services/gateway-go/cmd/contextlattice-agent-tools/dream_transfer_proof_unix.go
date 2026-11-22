//go:build !windows

package main

import (
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func readCLIDreamTransferProof(path string) string {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return ""
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG ||
		stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 ||
		(stat.Mode&0777 != 0600 && stat.Mode&0777 != 0400) || (stat.Size != 64 && stat.Size != 65) {
		return ""
	}
	raw, err := io.ReadAll(io.LimitReader(file, 66))
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(string(raw), "\n")
}
