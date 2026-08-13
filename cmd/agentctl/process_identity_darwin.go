package main

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func processStartIdentity(pid int) (string, error) {
	if pid < 1 {
		return "", errors.New("process identity PID is invalid")
	}
	process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || process == nil || int(process.Proc.P_pid) != pid {
		return "", errors.New("process start identity is unavailable")
	}
	started := process.Proc.P_starttime
	if started.Sec <= 0 || started.Usec < 0 {
		return "", errors.New("process start identity is unavailable")
	}
	return fmt.Sprintf("%d:%d", started.Sec, started.Usec), nil
}
