//go:build !windows

package main

import (
	"fmt"
	"os"
	"reflect"
	"syscall"
)

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
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return memoryEdgeLogFileStamp{}, fmt.Errorf("memory edge log file identity is unavailable")
	}
	statValue := reflect.ValueOf(stat).Elem()
	var changeToken string
	for _, fieldName := range []string{"Ctimespec", "Ctim"} {
		field := statValue.FieldByName(fieldName)
		if !field.IsValid() {
			continue
		}
		sec, nsec := field.FieldByName("Sec"), field.FieldByName("Nsec")
		if sec.IsValid() && nsec.IsValid() && sec.CanInt() && nsec.CanInt() {
			changeToken = fmt.Sprintf("ctime:%d:%d", sec.Int(), nsec.Int())
			break
		}
	}
	if changeToken == "" {
		return memoryEdgeLogFileStamp{}, fmt.Errorf("memory edge log change time is unavailable")
	}
	return memoryEdgeLogFileStamp{
		Exists:       true,
		Size:         info.Size(),
		Identity:     fmt.Sprintf("unix:%x:%x", uint64(stat.Dev), uint64(stat.Ino)),
		ModTimeNanos: info.ModTime().UnixNano(),
		ChangeToken:  changeToken,
	}, nil
}
