//go:build !ios

package main

import "golang.org/x/sys/unix"

func renameDreamTransferAuthorityFile(parent int, from, to string) error {
	return unix.RenameatxNp(parent, from, parent, to, unix.RENAME_EXCL)
}
