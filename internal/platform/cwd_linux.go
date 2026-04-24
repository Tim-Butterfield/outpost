//go:build linux

package platform

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolveCWDSource returns a composed identifier of the CWD's
// backing store for remote / virtual-shared mounts, or empty when
// the CWD is on local storage. See cwd_common.go for the contract.
//
// Implementation: walks /proc/self/mountinfo for the longest mount
// point that prefixes CWD, then — if the fs type is in
// remoteFSTypes — returns "<source>/<cwd-tail>".
//
// Examples in Tim's dev setup:
//
//   Parallels Shared Folders (prl_fs):
//     cwd="/media/psf/repos/github.com/.../outpost"
//     -> "repos/github.com/.../outpost"
//
//   CIFS mount (//win-host/share -> /mnt/win):
//     cwd="/mnt/win/proj" -> "//win-host/share/proj"
//
//   Local ext4:
//     -> ""
func ResolveCWDSource(cwd string) (string, error) {
	clean := filepath.Clean(cwd)
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return "", fmt.Errorf("platform: read mountinfo: %w", err)
	}

	var best mountinfoEntry
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		m, ok := parseMountinfoLine(scanner.Text())
		if !ok {
			continue
		}
		if !pathUnder(clean, m.mountPoint) {
			continue
		}
		if len(m.mountPoint) > len(best.mountPoint) {
			best = m
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("platform: scan mountinfo: %w", err)
	}

	return composeSource(clean, best.mountPoint, best.source, best.fsType), nil
}

// mountinfoEntry is the parsed form of one /proc/self/mountinfo line.
type mountinfoEntry struct {
	mountPoint string
	fsType     string
	source     string
}

// parseMountinfoLine parses a single mountinfo record. The format is
// documented in proc(5); the key trick is that fields before " - "
// are mount descriptors and fields after are (fsType, source, opts).
// We reject lines that don't conform rather than inventing data.
func parseMountinfoLine(line string) (mountinfoEntry, bool) {
	const sep = " - "
	idx := strings.Index(line, sep)
	if idx < 0 {
		return mountinfoEntry{}, false
	}
	pre := strings.Fields(line[:idx])
	post := strings.Fields(line[idx+len(sep):])
	// mountinfo guarantees at least: id, parent, major:minor, root,
	// mountpoint before " - ", and fsType, source after it.
	if len(pre) < 5 || len(post) < 2 {
		return mountinfoEntry{}, false
	}
	return mountinfoEntry{
		mountPoint: unescapeMountinfo(pre[4]),
		fsType:     post[0],
		source:     unescapeMountinfo(post[1]),
	}, true
}

// unescapeMountinfo undoes the mountinfo octal escapes:
// \040 -> ' ', \011 -> '\t', \012 -> '\n', \134 -> '\'. Leaves any
// non-\OOO sequence alone.
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var buf strings.Builder
	buf.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '\\' && i+3 < len(s) {
			n := 0
			ok := true
			for j := 0; j < 3; j++ {
				c := s[i+1+j]
				if c < '0' || c > '7' {
					ok = false
					break
				}
				n = n*8 + int(c-'0')
			}
			if ok {
				buf.WriteByte(byte(n))
				i += 4
				continue
			}
		}
		buf.WriteByte(s[i])
		i++
	}
	return buf.String()
}
