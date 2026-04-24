//go:build darwin

package platform

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ResolveCWDSource returns a composed identifier of the CWD's
// backing store for remote / virtual-shared mounts, or empty when
// the CWD is on local storage. See cwd_common.go for the contract.
//
// Implementation: calls getfsstat(2) to enumerate mounts, then
// applies the same longest-prefix + remote-type logic as Linux.
//
// Examples:
//
//   SMB mount:
//     cwd="/Volumes/Repos/proj"
//     -> "//user@server/Repos/proj"
//
//   Local APFS:
//     -> ""
func ResolveCWDSource(cwd string) (string, error) {
	clean := filepath.Clean(cwd)

	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return "", fmt.Errorf("platform: getfsstat count: %w", err)
	}
	if n <= 0 {
		return "", nil
	}
	// Small headroom so a concurrent mount/unmount doesn't truncate
	// our view; getfsstat returns the actual count written.
	stats := make([]unix.Statfs_t, n+4)
	got, err := unix.Getfsstat(stats, unix.MNT_NOWAIT)
	if err != nil {
		return "", fmt.Errorf("platform: getfsstat: %w", err)
	}
	stats = stats[:got]

	var bestMount, bestSource, bestFS string
	for i := range stats {
		mount := cString(stats[i].Mntonname[:])
		if !pathUnder(clean, mount) {
			continue
		}
		if len(mount) > len(bestMount) {
			bestMount = mount
			bestSource = cString(stats[i].Mntfromname[:])
			bestFS = cString(stats[i].Fstypename[:])
		}
	}
	return composeSource(clean, bestMount, bestSource, bestFS), nil
}
