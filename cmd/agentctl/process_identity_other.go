//go:build !darwin && !linux

package main

import "errors"

func processStartIdentity(int) (string, error) {
	return "", errors.New("process start identity is unavailable")
}
