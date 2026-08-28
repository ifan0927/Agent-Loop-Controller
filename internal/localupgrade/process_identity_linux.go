//go:build linux

package localupgrade

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func processStartIdentity(pid int) (string, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", errors.New("process start identity is unavailable")
	}
	closing := strings.LastIndexByte(string(raw), ')')
	if closing < 1 {
		return "", errors.New("process start identity is unavailable")
	}
	fields := strings.Fields(string(raw[closing+1:]))
	if len(fields) < 20 {
		return "", errors.New("process start identity is unavailable")
	}
	started, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || started == 0 {
		return "", errors.New("process start identity is unavailable")
	}
	return strconv.FormatUint(started, 10), nil
}
