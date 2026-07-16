//go:build linux

package process

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

func observedProcessStartToken(pid int) (string, bool, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	// The comm field may contain spaces and parentheses. Field 22 (starttime)
	// is the twentieth whitespace-delimited field after its final ')'.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", false, errors.New("kernel process identity is invalid")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 || fields[19] == "" {
		return "", false, errors.New("kernel process identity is incomplete")
	}
	return "linux:" + fields[19], true, nil
}
