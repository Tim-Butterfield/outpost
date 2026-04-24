//go:build unix

package platform

import (
	"strings"
)

// remoteFSTypes enumerates filesystem types considered "network or
// virtual shared storage" for the purpose of advertising cwd_source
// in dispatch.txt. Local block-device filesystems (apfs, ext4, xfs,
// ntfs, zfs, btrfs, ...) deliberately return an empty source so we
// don't spam the dispatch file with useless device nodes.
//
// The set is intentionally permissive: any protocol or hypervisor
// share-like mount. A submitter-AI correlating targets across hosts
// gets more utility from a false positive (unnecessary source info)
// than a false negative (missing the only field that tied two hosts
// together).
var remoteFSTypes = map[string]bool{
	"cifs":       true, // Linux CIFS / SMB
	"smbfs":      true, // macOS SMB
	"smb":        true,
	"nfs":        true,
	"nfs4":       true,
	"afpfs":      true, // macOS AFP (legacy)
	"9p":         true, // Plan 9 / QEMU / virtio-9p
	"fuse.sshfs": true,
	"prl_fs":     true, // Parallels Shared Folders (Linux guest, legacy kernel module)
	"fuse.prl_fsd": true, // Parallels Shared Folders (Linux guest, current FUSE implementation)
	"vboxsf":     true, // VirtualBox Shared Folders
	"vmhgfs":     true, // VMware Shared Folders (legacy)
	"fuse.hgfs":  true, // VMware Shared Folders (fuse variant)
}

// composeSource returns "<source>/<cwd-tail>" when fsType is in the
// remote set; otherwise empty. A single helper keeps Linux and
// Darwin paths producing identical-shaped output.
func composeSource(cwd, mountPoint, source, fsType string) string {
	if mountPoint == "" || !remoteFSTypes[fsType] {
		return ""
	}
	tail := strings.TrimPrefix(cwd, mountPoint)
	tail = strings.TrimPrefix(tail, "/")
	source = strings.TrimRight(source, "/")
	if tail == "" {
		return source
	}
	return source + "/" + tail
}

// pathUnder reports whether path sits at or beneath mount. Handles
// the root mount and avoids the classic "/foo" matching "/foobar"
// prefix bug.
func pathUnder(path, mount string) bool {
	if mount == "" {
		return false
	}
	if mount == "/" {
		return true
	}
	if path == mount {
		return true
	}
	return strings.HasPrefix(path, mount+"/")
}

// cString returns the substring of b up to its first NUL byte, or
// the whole slice if no NUL is found. getfsstat fields are fixed-
// size char arrays, so we always need to truncate.
func cString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
