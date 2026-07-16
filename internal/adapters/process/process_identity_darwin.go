//go:build darwin

package process

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

func observedProcessStartToken(pid int) (string, bool, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.ENOENT) {
			return "", false, nil
		}
		// kern.proc.pid can report EIO while a just-exited process is being
		// reaped. Only collapse it to absence when the kernel also reports that
		// the exact PID no longer exists.
		if errors.Is(err, syscall.EIO) && errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return "", false, nil
		}
		return "", false, err
	}
	start := info.Proc.P_starttime
	if start.Sec == 0 && start.Usec == 0 {
		return "", false, nil
	}
	return fmt.Sprintf("darwin:%d:%d", start.Sec, start.Usec), true, nil
}
