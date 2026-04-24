//go:build unix

package fsatomic

import (
	"errors"
	"syscall"
)

// isRetriableRenameErr returns true for errors that indicate a
// transient filesystem condition on Unix. EBUSY is the classic
// transient NFS error: another process briefly held the target
// before release.
func isRetriableRenameErr(err error) bool {
	return errors.Is(err, syscall.EBUSY)
}
