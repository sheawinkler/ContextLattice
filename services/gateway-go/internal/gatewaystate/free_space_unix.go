//go:build !windows

package gatewaystate

import "syscall"

func diskAvailableBytes(path string) (uint64, string, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, "statfs", err
	}
	return fs.Bavail * uint64(fs.Bsize), "statfs", nil
}
