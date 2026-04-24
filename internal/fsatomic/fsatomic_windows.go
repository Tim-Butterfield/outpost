//go:build windows

package fsatomic

import (
	"errors"
	"syscall"
)

// Windows system error codes for transient rename failures. Both
// occur when an antivirus scanner, Explorer, a sync client, or
// another handle-holder briefly blocks access to the target during
// the rename. Both are expected to clear within milliseconds.
const (
	errAccessDenied     = syscall.Errno(5)  // ERROR_ACCESS_DENIED
	errSharingViolation = syscall.Errno(32) // ERROR_SHARING_VIOLATION
)

// isRetriableRenameErr returns true for errors that indicate a
// transient filesystem condition on Windows.
func isRetriableRenameErr(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == errAccessDenied || errno == errSharingViolation
}
