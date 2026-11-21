//go:build windows

package main

import "errors"

func acquireConflictSourceLock(string) (func(), error) {
	return nil, errors.New("unattended conflict grant is not enabled on this host")
}

// The ordinary packet and manual review remain usable without a qualified ACL grant.
func readConflictPrivateFile(string, int64) ([]byte, error) {
	return nil, errors.New("unattended conflict grant is not enabled on this host")
}
