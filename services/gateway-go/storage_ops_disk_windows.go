//go:build windows

package main

import "golang.org/x/sys/windows"

func diskUsageBytes(root string) (uint64, uint64, string, error) {
	path, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return 0, 0, "get_disk_free_space_ex", err
	}
	var freeAvailable uint64
	var total uint64
	var totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(path, &freeAvailable, &total, &totalFree); err != nil {
		return 0, 0, "get_disk_free_space_ex", err
	}
	return total, freeAvailable, "get_disk_free_space_ex", nil
}
