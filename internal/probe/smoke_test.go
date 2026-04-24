package probe

import (
	"context"
	"os/exec"
	"runtime"
	"testing"
)

// TestVerifySmoke_RealShell exercises the full smoke path end-to-end
// using a shell that every test host has: /bin/sh on Unix,
// cmd.exe on Windows. If this test fails, something in smoke.go is
// broken at the exec level, not just a Go unit-level issue.
func TestVerifySmoke_RealShell(t *testing.T) {
	var bin string
	var ext string
	switch runtime.GOOS {
	case "windows":
		p, err := exec.LookPath("cmd.exe")
		if err != nil {
			t.Skipf("cmd.exe not on PATH: %v", err)
		}
		bin = p
		ext = "cmd"
	default:
		p, err := exec.LookPath("sh")
		if err != nil {
			t.Skipf("sh not on PATH: %v", err)
		}
		bin = p
		ext = "sh"
	}

	interps := []Interpreter{{
		Name:       "sh-or-cmd",
		Path:       bin,
		Extensions: []string{ext},
		Working:    true,
	}}
	out := VerifySmoke(context.Background(), interps)
	if len(out) != 1 {
		t.Fatalf("expected 1 result, got %d", len(out))
	}
	if !out[0].Working {
		t.Errorf("expected smoke to pass; Working=false, SmokeError=%q", out[0].SmokeError)
	}
	if out[0].SmokeError != "" {
		t.Errorf("expected empty SmokeError on pass; got %q", out[0].SmokeError)
	}
}

// TestVerifySmoke_BrokenInterpreter wires up an interpreter that
// resolves to /bin/false (or equivalent). The smoke run must flip
// Working to false and populate SmokeError.
func TestVerifySmoke_BrokenInterpreter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no reliable always-false binary on Windows PATH")
	}
	p, err := exec.LookPath("false")
	if err != nil {
		t.Skipf("no false binary: %v", err)
	}

	interps := []Interpreter{{
		Name:       "false",
		Path:       p,
		Extensions: []string{"sh"}, // smokeScripts["sh"] exists
		Working:    true,
	}}
	out := VerifySmoke(context.Background(), interps)
	if out[0].Working {
		t.Errorf("expected smoke to fail on /bin/false")
	}
	if out[0].SmokeError == "" {
		t.Errorf("expected SmokeError to be populated")
	}
}

// TestVerifySmoke_LeavesNonWorkingAlone ensures we don't
// accidentally promote a version-probe-broken interpreter by
// running smoke on something we shouldn't.
func TestVerifySmoke_LeavesNonWorkingAlone(t *testing.T) {
	interps := []Interpreter{{
		Name:       "already-broken",
		Path:       "/nonexistent",
		Extensions: []string{"sh"},
		Working:    false,
	}}
	out := VerifySmoke(context.Background(), interps)
	if out[0].Working {
		t.Error("VerifySmoke should not change Working when input is false")
	}
	if out[0].SmokeError != "" {
		t.Error("VerifySmoke should not write SmokeError when Working is already false")
	}
}

// TestVerifySmoke_UnknownExtension keeps Working=true for an
// interpreter whose extension has no smoke script entry. Conservative
// choice — don't demote without a test.
func TestVerifySmoke_UnknownExtension(t *testing.T) {
	interps := []Interpreter{{
		Name:       "fictional",
		Path:       "/usr/bin/bash",
		Extensions: []string{"nosuchext"},
		Working:    true,
	}}
	out := VerifySmoke(context.Background(), interps)
	if !out[0].Working {
		t.Error("VerifySmoke should leave Working=true when no smoke script exists for the extension")
	}
}

func TestSmokeArgs(t *testing.T) {
	cases := []struct {
		ext      string
		wantArgs []string
	}{
		{"sh", nil},
		{"py", nil},
		{"js", nil},
		{"ps1", []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File"}},
		{"cmd", []string{"/c"}},
		{"bat", []string{"/c"}},
		{"btm", []string{"/c"}},
	}
	for _, tc := range cases {
		got := smokeArgs(tc.ext)
		if len(got) != len(tc.wantArgs) {
			t.Errorf("smokeArgs(%q) = %v, want %v", tc.ext, got, tc.wantArgs)
			continue
		}
		for i, w := range tc.wantArgs {
			if got[i] != w {
				t.Errorf("smokeArgs(%q)[%d] = %q, want %q", tc.ext, i, got[i], w)
			}
		}
	}
}
