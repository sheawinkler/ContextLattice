//go:build windows

package main

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsOwnerOnlyAppendUsesKernelAppendData(t *testing.T) {
	if ownerOnlyAppendAccessMask&windows.FILE_APPEND_DATA == 0 {
		t.Fatal("Windows append opener does not request FILE_APPEND_DATA")
	}
	if ownerOnlyAppendAccessMask&windows.FILE_WRITE_DATA != 0 {
		t.Fatal("Windows append opener requests FILE_WRITE_DATA and can bypass append-only semantics")
	}
}
