package main

import "os"

// ownerOnlyWindowsDescriptorModePermitted treats Go's Windows mode bits as an
// access hint, not a Unix ACL. Windows commonly reports ordinary writable
// files as 0666 and read-only files as 0444; requiring exactly 0600 rejects
// valid descriptors before their handle access has been exercised. The
// descriptor open itself remains authoritative for ACL/access denial, while a
// writable descriptor must not advertise the Windows read-only bit.
func ownerOnlyWindowsDescriptorModePermitted(mode os.FileMode, writable bool) bool {
	if !mode.IsRegular() {
		return false
	}
	if writable && mode.Perm()&0o200 == 0 {
		return false
	}
	return true
}
