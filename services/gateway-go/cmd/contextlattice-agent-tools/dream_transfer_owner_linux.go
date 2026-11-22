//go:build !android

package main

import "golang.org/x/sys/unix"

func renameDreamTransferAuthorityFile(parent int, from, to string) error {
	return unix.Renameat2(parent, from, parent, to, unix.RENAME_NOREPLACE)
}
