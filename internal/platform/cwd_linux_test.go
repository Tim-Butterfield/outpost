//go:build linux

package platform

import "testing"

func TestParseMountinfoLine(t *testing.T) {
	cases := []struct {
		name       string
		line       string
		wantMount  string
		wantFS     string
		wantSource string
		wantOK     bool
	}{
		{
			name:       "ext4 root",
			line:       "36 35 8:1 / / rw,noatime shared:1 - ext4 /dev/sda1 rw",
			wantMount:  "/",
			wantFS:     "ext4",
			wantSource: "/dev/sda1",
			wantOK:     true,
		},
		{
			name:       "parallels shared folder",
			line:       "52 36 0:42 / /media/psf/repos rw,nosuid,nodev,noatime shared:2 - prl_fs repos rw,share",
			wantMount:  "/media/psf/repos",
			wantFS:     "prl_fs",
			wantSource: "repos",
			wantOK:     true,
		},
		{
			name:       "cifs with octal-escaped spaces in mountpoint",
			line:       `88 36 0:99 / /mnt/win\040share rw,noatime - cifs //winhost/Share rw,vers=3.1.1`,
			wantMount:  "/mnt/win share",
			wantFS:     "cifs",
			wantSource: "//winhost/Share",
			wantOK:     true,
		},
		{
			name:   "no separator, rejected",
			line:   "36 35 8:1 / / rw,noatime shared:1 ext4 /dev/sda1 rw",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseMountinfoLine(tc.line)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got.mountPoint != tc.wantMount {
				t.Errorf("mountPoint=%q want %q", got.mountPoint, tc.wantMount)
			}
			if got.fsType != tc.wantFS {
				t.Errorf("fsType=%q want %q", got.fsType, tc.wantFS)
			}
			if got.source != tc.wantSource {
				t.Errorf("source=%q want %q", got.source, tc.wantSource)
			}
		})
	}
}

func TestUnescapeMountinfo(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"no-escapes", "no-escapes"},
		{`with\040space`, "with space"},
		{`tab\011here`, "tab\there"},
		{`back\134slash`, `back\slash`},
		{`\040leading`, " leading"},
		{`trailing\040`, "trailing "}, // full 4-char octal at end
		{`bogus\X`, `bogus\X`},        // not 3 octal digits, pass through
	}
	for _, tc := range cases {
		got := unescapeMountinfo(tc.in)
		if got != tc.want {
			t.Errorf("unescape(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

