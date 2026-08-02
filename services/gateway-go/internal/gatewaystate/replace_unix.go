//go:build !windows

package gatewaystate

import "os"

func replaceFileAtomic(source string, destination string) error {
	return os.Rename(source, destination)
}
