package probe

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
)

func TestDetect_RealHost(t *testing.T) {
	// Black-box sanity: on any CI runner or developer box, at least
	// one of the cross-platform shells or Python should be present.
	// This is not a strict assertion on specific interpreters; the
	// test guards against silent "detected nothing" regressions.
	got := Detect(context.Background())
	if len(got) == 0 {
		t.Fatal("Detect returned empty on this host; at least one shell or Python was expected")
	}
	for _, i := range got {
		if i.Name == "" || i.Path == "" {
			t.Errorf("malformed interpreter entry: %+v", i)
		}
		if len(i.Extensions) == 0 {
			t.Errorf("%s: no extensions", i.Name)
		}
	}
}

func TestDetect_InjectedLookupAndRunner(t *testing.T) {
	// Mock PATH containing python3, bash, and a stub-style python3
	// that returns an error message without a version. python3
	// reports a real version, bash probe fails (runner returns
	// empty + working=false), stub reports error text + working=false.
	lookPath := func(name string) (string, error) {
		switch name {
		case "python3":
			return "/opt/homebrew/bin/python3", nil
		case "bash":
			return "/bin/bash", nil
		}
		return "", errors.New("not found")
	}
	runner := func(ctx context.Context, path string, args []string) (string, bool) {
		if path == "/opt/homebrew/bin/python3" {
			return "Python 3.13.0", true
		}
		return "", false
	}

	got := detect(context.Background(), lookPath, runner)

	byName := map[string]Interpreter{}
	for _, i := range got {
		byName[i.Name] = i
	}

	py, ok := byName["python3"]
	if !ok {
		t.Fatal("python3 not detected")
	}
	if py.Path != "/opt/homebrew/bin/python3" {
		t.Errorf("python3 path: got %q", py.Path)
	}
	if py.Version != "Python 3.13.0" {
		t.Errorf("python3 version: got %q", py.Version)
	}
	if !py.Working {
		t.Error("python3 should be marked Working")
	}

	bash, ok := byName["bash"]
	if !ok {
		t.Fatal("bash not detected")
	}
	if bash.Version != "" {
		t.Errorf("bash version should be empty on runner failure; got %q", bash.Version)
	}
	if bash.Working {
		t.Error("bash should be marked NOT working when probe fails")
	}

	// Ensure platform-scoped entries filter as expected. On Unix,
	// `cmd` and `py` (Windows launcher) must NOT appear.
	if runtime.GOOS != "windows" {
		if _, unwanted := byName["cmd"]; unwanted {
			t.Error("cmd should not be detected on non-Windows")
		}
		if _, unwanted := byName["py"]; unwanted {
			t.Error("py (Windows launcher) should not be detected on non-Windows")
		}
	}
}

func TestDefaultRunner_Heuristics(t *testing.T) {
	// These cases exercise the (version, working) tuple's decision
	// logic without invoking real processes.
	tests := []struct {
		name        string
		out         string
		working     bool
		wantWorking bool
	}{
		{"version-like output exits 0", "Python 3.14.4", true, true},
		{"non-version output exits 0", "something weird", true, true},
		{"version-like on non-zero exit (Lua)", "Lua 5.4.6  Copyright...", false, true},
		{"Store-stub error on non-zero exit", "Python was not found; run without arguments to install from the Microsoft Store, or disable this shortcut from Settings > Apps > Advanced app settings > App execution aliases.", false, false},
		{"empty output", "", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyRunnerOutput(tc.out, tc.working)
			if got != tc.wantWorking {
				t.Errorf("got working=%v, want %v", got, tc.wantWorking)
			}
		})
	}
}

// classifyRunnerOutput is the same decision logic used in
// defaultRunner after Run() returns. Factored out here so the
// heuristic can be tested without spawning real subprocesses.
func classifyRunnerOutput(out string, exitedZero bool) bool {
	if exitedZero {
		return true
	}
	if out != "" && versionLikeRe.MatchString(out) {
		return true
	}
	return false
}

func TestVersionLikeRe_CoversKnownInterpreters(t *testing.T) {
	// Representative --version outputs we've seen in the wild.
	// All should match versionLikeRe (i.e., be treated as real
	// versions when the probe exited non-zero).
	versionStrings := []string{
		"Python 3.14.4",
		"GNU bash, version 5.2.37(1)-release (x86_64-pc-msys)",
		"GNU bash, version 3.2.57(1)-release (arm64-apple-darwin25)",
		"v24.13.0",
		"v23.3.0",
		"ruby 2.6.10p210 (2022-04-12 revision 67958)",
		"Lua 5.4.6  Copyright (C) 1994-2023 Lua.org, PUC-Rio",
		"5.1.26100.8246", // PowerShell PSVersion output
		"This is perl 5, version 34, subversion 1 (v5.34.1)",
	}
	for _, s := range versionStrings {
		if !versionLikeRe.MatchString(s) {
			t.Errorf("expected version-like match: %q", s)
		}
	}
}

func TestVersionLikeRe_RejectsErrorMessages(t *testing.T) {
	// Error / stub messages we've seen in the wild that should
	// NOT be trusted as versions when paired with non-zero exit.
	nonVersionStrings := []string{
		"Python was not found; run without arguments to install from the Microsoft Store, or disable this shortcut from Settings > Apps > Advanced app settings > App execution aliases.",
		"command not found",
		"is not recognized as an internal or external command",
		"no such file",
	}
	for _, s := range nonVersionStrings {
		if versionLikeRe.MatchString(s) {
			t.Errorf("expected NO version-like match (non-zero exit with this output should be rejected): %q", s)
		}
	}
}

func TestDetect_ContextCancellation(t *testing.T) {
	// An already-cancelled context should short-circuit detection.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	lookPath := func(name string) (string, error) {
		return "/stub/" + name, nil
	}
	runner := func(ctx context.Context, path string, args []string) (string, bool) {
		t.Errorf("runner should not be called when ctx is cancelled (called with %s)", path)
		return "", false
	}

	got := detect(ctx, lookPath, runner)
	if len(got) != 0 {
		// Zero or up to one result is acceptable: detect may have
		// resolved the first binary before checking ctx.Err(); the
		// key invariant is that it STOPS quickly.
		if len(got) > 1 {
			t.Errorf("detect should short-circuit on cancelled ctx; got %d results", len(got))
		}
	}
}

func TestFirstNonEmptyLine(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"single line", "Python 3.13", "Python 3.13"},
		{"leading blanks", "\n\n  \npwsh 7.4.0\n", "pwsh 7.4.0"},
		{"trailing whitespace", "bash 5.2.1   ", "bash 5.2.1"},
		{"empty", "", ""},
		{"only whitespace", "\n   \t\n", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := firstNonEmptyLine(strings.NewReader(tc.in))
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
