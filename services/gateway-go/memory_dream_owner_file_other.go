//go:build !darwin && !linux

package main

// Local transfers are unavailable where OS custody cannot yet be verified.
func readDreamOwnerFile(string) ([]byte, memoryEdgeLogFileStamp, bool) {
	return nil, memoryEdgeLogFileStamp{}, false
}
