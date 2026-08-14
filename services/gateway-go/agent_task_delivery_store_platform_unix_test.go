//go:build !windows

package main

func agentTaskPlatformSymlinkUnsupported(error) bool {
	return false
}
