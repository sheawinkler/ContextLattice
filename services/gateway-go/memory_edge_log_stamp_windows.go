//go:build windows

package main

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type memoryEdgeLogWindowsBasicInfo struct {
	CreationTime   int64
	LastAccessTime int64
	LastWriteTime  int64
	ChangeTime     int64
	FileAttributes uint32
	_              uint32
}

func memoryEdgeLogPlatformFileStamp(path string) (memoryEdgeLogFileStamp, error) {
	file, err := openOwnerOnlyReadPlatform(path)
	if os.IsNotExist(err) {
		return memoryEdgeLogFileStamp{Identity: "absent", ChangeToken: "absent"}, nil
	}
	if err != nil {
		return memoryEdgeLogFileStamp{}, err
	}
	defer file.Close()
	return memoryEdgeLogPlatformFileStampForFile(file)
}

func memoryEdgeLogPlatformFileStampForFile(file *os.File) (memoryEdgeLogFileStamp, error) {
	if file == nil {
		return memoryEdgeLogFileStamp{}, fmt.Errorf("memory edge log descriptor is unavailable")
	}
	info, err := file.Stat()
	if err != nil {
		return memoryEdgeLogFileStamp{}, err
	}
	handle := windows.Handle(file.Fd())
	var identity windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &identity); err != nil {
		return memoryEdgeLogFileStamp{}, err
	}
	var basic memoryEdgeLogWindowsBasicInfo
	if err := windows.GetFileInformationByHandleEx(
		handle,
		windows.FileBasicInfo,
		(*byte)(unsafe.Pointer(&basic)),
		uint32(unsafe.Sizeof(basic)),
	); err != nil {
		return memoryEdgeLogFileStamp{}, err
	}
	return memoryEdgeLogFileStamp{
		Exists: true,
		Size:   info.Size(),
		Identity: fmt.Sprintf(
			"windows:%x:%x:%x",
			identity.VolumeSerialNumber,
			identity.FileIndexHigh,
			identity.FileIndexLow,
		),
		ModTimeNanos: info.ModTime().UnixNano(),
		ChangeToken:  fmt.Sprintf("change:%x", uint64(basic.ChangeTime)),
	}, nil
}
