//go:build unix

package platform

import "testing"

func TestComposeSource(t *testing.T) {
	cases := []struct {
		name    string
		cwd     string
		mount   string
		source  string
		fsType  string
		want    string
	}{
		{"local ext4", "/home/u/x", "/", "/dev/sda1", "ext4", ""},
		{"local apfs", "/Users/u/x", "/", "/dev/disk3s1", "apfs", ""},
		{"prl_fs with tail", "/media/psf/repos/proj", "/media/psf/repos", "repos", "prl_fs", "repos/proj"},
		{"prl_fs at root", "/media/psf/repos", "/media/psf/repos", "repos", "prl_fs", "repos"},
		{"cifs with tail", "/mnt/win/proj", "/mnt/win", "//winhost/Share", "cifs", "//winhost/Share/proj"},
		{"smbfs macOS", "/Volumes/Repos/proj", "/Volumes/Repos", "//u@server/Repos", "smbfs", "//u@server/Repos/proj"},
		{"empty mountpoint", "/x", "", "src", "prl_fs", ""},
		{"unknown fs type", "/x", "/", "/dev/sda1", "xfs", ""},
		{"trailing slash in source stripped", "/mnt/win/p", "/mnt/win", "//host/share/", "cifs", "//host/share/p"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := composeSource(tc.cwd, tc.mount, tc.source, tc.fsType)
			if got != tc.want {
				t.Errorf("composeSource(%q,%q,%q,%q)=%q, want %q",
					tc.cwd, tc.mount, tc.source, tc.fsType, got, tc.want)
			}
		})
	}
}

func TestPathUnder_Common(t *testing.T) {
	cases := []struct {
		path, mount string
		want        bool
	}{
		{"/a", "/", true},
		{"/a", "/a", true},
		{"/a/b", "/a", true},
		{"/ab", "/a", false},
		{"/a", "/b", false},
		{"/a", "", false},
	}
	for _, tc := range cases {
		if got := pathUnder(tc.path, tc.mount); got != tc.want {
			t.Errorf("pathUnder(%q,%q)=%v want %v", tc.path, tc.mount, got, tc.want)
		}
	}
}

func TestCString(t *testing.T) {
	b := []byte{'/', 'V', 'o', 'l', 0, 's', 'h', 'o', 'u', 'l', 'd', 'N', 'o', 'S', 'h', 'o', 'w'}
	if got := cString(b); got != "/Vol" {
		t.Errorf("cString trailing-NUL: got %q, want %q", got, "/Vol")
	}
	b = []byte("no-null")
	if got := cString(b); got != "no-null" {
		t.Errorf("cString no-NUL: got %q, want %q", got, "no-null")
	}
}
