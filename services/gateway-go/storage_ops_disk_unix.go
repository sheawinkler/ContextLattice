//go:build !windows

package main

import "syscall"

func diskUsageBytes(root string) (uint64, uint64, string, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(root, &fs); err != nil {
		return 0, 0, "statfs", err
	}
	total := fs.Blocks * uint64(fs.Bsize)
	free := fs.Bavail * uint64(fs.Bsize)
	return total, free, "statfs", nil
}
