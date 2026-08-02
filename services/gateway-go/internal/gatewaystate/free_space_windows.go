//go:build windows

package gatewaystate

import "golang.org/x/sys/windows"

func diskAvailableBytes(path string) (uint64, string, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, "get_disk_free_space_ex", err
	}
	var freeAvailable uint64
	var total uint64
	var totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(pathPtr, &freeAvailable, &total, &totalFree); err != nil {
		return 0, "get_disk_free_space_ex", err
	}
	return freeAvailable, "get_disk_free_space_ex", nil
}
