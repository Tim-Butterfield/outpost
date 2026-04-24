//go:build unix

package platform_test

import "syscall"

// processAlive reports whether pid is currently live. Sending
// signal 0 is a standard liveness probe on Unix: the kernel
// validates the target without delivering a signal, returning
// nil if the process exists and ESRCH if it does not.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
