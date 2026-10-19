//go:build unix

package workspace

import (
	"errors"
	"syscall"
)

// processAlive reports whether a process with this ID exists. Signal 0 checks
// for the process without disturbing it; EPERM means it exists but belongs
// to another user.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
