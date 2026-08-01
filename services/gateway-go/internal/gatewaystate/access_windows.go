//go:build windows

package gatewaystate

import "golang.org/x/sys/windows"

func probeDirectoryAccess(path string) (bool, bool, error, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, false, err, err
	}
	handle, traverseErr := windows.CreateFile(
		pathPtr,
		windows.FILE_TRAVERSE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if traverseErr == nil {
		_ = windows.CloseHandle(handle)
	}
	handle, writeErr := windows.CreateFile(
		pathPtr,
		// FILE_WRITE_DATA has the FILE_ADD_FILE meaning when the target is a
		// directory. x/sys/windows exposes the shared numeric access right under
		// its file name rather than the directory alias used by Win32 docs.
		windows.FILE_WRITE_DATA,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if writeErr == nil {
		_ = windows.CloseHandle(handle)
	}
	return writeErr == nil, traverseErr == nil, writeErr, traverseErr
}
