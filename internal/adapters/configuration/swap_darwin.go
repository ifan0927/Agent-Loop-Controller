//go:build darwin

package configuration

import "golang.org/x/sys/unix"

func atomicSwap(left, right string) error {
	return unix.RenamexNp(left, right, unix.RENAME_SWAP)
}

func atomicPublishExclusive(source, target string) error {
	return unix.RenamexNp(source, target, unix.RENAME_EXCL)
}
