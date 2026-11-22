//go:build (!darwin || ios) && (!linux || android)

package main

import (
	"errors"
	"time"
)

const dreamTransferAuthoritySupported = false

func readDreamTransferWorkerKey(string) (string, error) {
	return "", errors.ErrUnsupported
}

func writeDreamTransferAuthorityPair(string, string, []byte, []byte, time.Time) error {
	return errors.ErrUnsupported
}
