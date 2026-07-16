//go:build !darwin && !linux

package process

import "errors"

func observedProcessStartToken(int) (string, bool, error) {
	return "", false, errors.New("kernel process identity is unsupported")
}
