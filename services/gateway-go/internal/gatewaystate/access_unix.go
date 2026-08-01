//go:build !windows

package gatewaystate

import "golang.org/x/sys/unix"

func probeDirectoryAccess(path string) (bool, bool, error, error) {
	traverseErr := unix.Access(path, unix.X_OK)
	writeErr := unix.Access(path, unix.W_OK)
	return writeErr == nil, traverseErr == nil, writeErr, traverseErr
}
