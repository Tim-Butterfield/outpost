// Package probe detects interpreters installed on the local host
// for use by `outpost setup`. Detection uses PATH lookup plus an
// optional --version capture; no sandboxing or execution of worker
// scripts happens here.
//
// The detection table is compiled in. Operators override the
// results via `[dispatch.path]` entries in outpost.toml; those
// entries take precedence over probed paths.
package probe

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// versionTimeout caps how long we wait for a detected interpreter
// to respond to its --version probe. Non-compliant binaries that
// hang should not hang `outpost setup`.
const versionTimeout = 2 * time.Second

// Interpreter describes one detected interpreter: which name it
// answers to, where its binary lives, what file extensions it can
// dispatch, whether its version probe succeeded, and (best-effort)
// what the --version output was.
type Interpreter struct {
	// Name is the canonical interpreter name (matches the dispatch
	// entry key in outpost.toml).
	Name string

	// Path is the absolute path resolved on PATH.
	Path string

	// Extensions lists the file extensions this interpreter can
	// run.
	Extensions []string

	// Version is the trimmed first line of --version output.
	// Populated even when Working is false (to give operators a
	// diagnostic signal like the Microsoft Store Python stub's
	// "Python was not found" message).
	Version string

	// Working is true when the interpreter responded to the
	// version probe with either a clean exit or non-zero exit plus
	// a version-like string (the Lua convention). It is false when
	// the probe returned a non-zero exit and the output does not
	// look like a version -- the Windows Store Python stub is the
	// canonical example: it prints an installation-hint message
	// and exits non-zero, which is a reliable signal that running
	// this "interpreter" would not actually execute scripts.
	//
	// Interpreters without a version probe (like cmd on Windows)
	// are marked Working=true: they are accepted because they were
	// found on PATH, but no richer verification is possible.
	Working bool

	// SmokeError, when non-empty, means the interpreter passed the
	// --version probe but failed to actually execute a trivial
	// "print this string" script. VerifySmoke populates this and
	// sets Working=false, so downstream code (writeConfigFromDetection,
	// renderDetection) treats these interpreters exactly like the
	// Store-Python-stub case. Empty means either smoke was not run
	// or the smoke test succeeded.
	SmokeError string
}

// candidate is one row of the compiled-in detection table. Shared
// between the interpreter table (this file) and the tool table
// (tools.go).
type candidate struct {
	name       string
	extensions []string
	// versionArgs is the argument list for capturing a version
	// string. When empty, no version probe is performed.
	versionArgs []string
	// windowsOnly / unixOnly / darwinOnly scope a candidate to
	// one platform family. Zero means "all platforms". At most
	// one flag should be set per candidate.
	windowsOnly bool
	unixOnly    bool
	darwinOnly  bool
}

// table is the set of interpreters detected by default. Extending
// this table is an additive change: new entries simply start
// appearing in `outpost setup` output and in dispatch.txt when
// found on PATH.
var table = []candidate{
	// Shells. `.sh` is claimed by all three and resolves to the
	// first entry (bash) for portability. Zsh additionally claims
	// `.zsh` so operators who want zsh-specific scripting can use
	// that extension without fighting the dispatch order.
	{name: "bash", extensions: []string{"sh"}, versionArgs: []string{"--version"}},
	{name: "sh", extensions: []string{"sh"}, unixOnly: true},
	{name: "zsh", extensions: []string{"sh", "zsh"}, versionArgs: []string{"--version"}},

	// PowerShell variants. `pwsh` is cross-platform PowerShell Core;
	// `powershell.exe` is Windows' legacy PowerShell.
	{name: "pwsh", extensions: []string{"ps1"}, versionArgs: []string{"-NoProfile", "-Command", "$PSVersionTable.PSVersion.ToString()"}},
	{name: "powershell", extensions: []string{"ps1"}, versionArgs: []string{"-NoProfile", "-Command", "$PSVersionTable.PSVersion.ToString()"}, windowsOnly: true},

	// Windows command interpreters.
	{name: "cmd", extensions: []string{"cmd", "bat"}, windowsOnly: true},
	{name: "tcc", extensions: []string{"btm"}, versionArgs: []string{"/C", "ver"}, windowsOnly: true},

	// Python.
	{name: "python3", extensions: []string{"py"}, versionArgs: []string{"--version"}},
	{name: "python", extensions: []string{"py"}, versionArgs: []string{"--version"}},
	{name: "py", extensions: []string{"py"}, versionArgs: []string{"--version"}, windowsOnly: true}, // Windows Python launcher

	// Other scripting runtimes. The built-in dispatch table covers
	// .sh, .ps1, .py, .cmd/.bat, .btm directly; the entries below
	// are reported by `outpost setup` so operators can add
	// `[dispatch.path]` overrides in outpost.toml.
	{name: "node", extensions: []string{"js"}, versionArgs: []string{"--version"}},
	{name: "deno", extensions: []string{"js", "ts"}, versionArgs: []string{"--version"}},
	{name: "ruby", extensions: []string{"rb"}, versionArgs: []string{"--version"}},
	{name: "perl", extensions: []string{"pl"}, versionArgs: []string{"--version"}},
	{name: "php", extensions: []string{"php"}, versionArgs: []string{"--version"}},
	{name: "lua", extensions: []string{"lua"}, versionArgs: []string{"-v"}},
}

// Detect scans the compiled-in table against the current host's
// PATH and returns the interpreters it found. Order in the result
// matches the order in the table.
//
// The provided context bounds the total detection time and
// individual --version probes honor their own timeout
// (versionTimeout).
func Detect(ctx context.Context) []Interpreter {
	return detect(ctx, exec.LookPath, defaultRunner)
}

// detect is the dependency-injection seam used by tests. lookPath
// resolves a binary name to an absolute path; runner executes a
// bounded --version command and returns its output plus a
// "working" flag.
func detect(ctx context.Context, lookPath lookPathFunc, runner versionRunner) []Interpreter {
	out := make([]Interpreter, 0, len(table))
	for _, c := range table {
		if c.windowsOnly && runtime.GOOS != "windows" {
			continue
		}
		if c.unixOnly && runtime.GOOS == "windows" {
			continue
		}
		if ctx.Err() != nil {
			return out
		}

		path, err := lookPath(c.name)
		if err != nil {
			continue
		}

		interp := Interpreter{
			Name:       c.name,
			Path:       path,
			Extensions: append([]string(nil), c.extensions...),
		}
		if len(c.versionArgs) > 0 {
			interp.Version, interp.Working = runner(ctx, path, c.versionArgs)
		} else {
			// No way to verify; treat found-on-PATH as sufficient.
			// cmd.exe is the canonical example: there is no
			// --version, and a PATH hit is all we have to go on.
			interp.Working = true
		}
		out = append(out, interp)
	}
	return out
}

// lookPathFunc matches exec.LookPath's signature.
type lookPathFunc func(name string) (string, error)

// versionRunner matches the signature of the injected version probe
// used by detect. Implementations should never block longer than
// versionTimeout and should return ("", false) on any fatal error
// (timeout, etc.).
type versionRunner func(ctx context.Context, path string, args []string) (version string, working bool)

// versionLikeRe matches strings that look like they contain a
// version number. Intentionally lenient: "Python 3.14.4",
// "GNU bash 5.2", "v24.13.0", "5.1.26100.8246", and the "v5.34.1"
// inside Perl's verbose banner all match. The Windows Store
// Python stub's error ("Python was not found; run without
// arguments to install from the Microsoft Store, or disable this
// shortcut from Settings > Apps > Advanced app settings > App
// execution aliases.") does NOT match -- it has no digits.
var versionLikeRe = regexp.MustCompile(`\d+\.\d+`)

// defaultRunner executes path with args and returns the first
// non-empty line of stdout (or stderr) plus a "working" flag:
//
//   - Exit 0: Working=true. The output is trusted even if it
//     does not look like a version (some probes use custom
//     commands that print bare strings or numbers).
//
//   - Exit non-zero with version-like output: Working=true.
//     Covers Lua's convention of printing version to stderr and
//     exiting non-zero.
//
//   - Exit non-zero with non-version output: Working=false.
//     Covers the Windows Store Python stub and other "stub that
//     complains and exits" placeholders: we report the raw
//     output as the Version (for diagnostics) but mark the
//     interpreter as not working so setup can exclude it from
//     the config.
//
//   - Timeout: Working=false, Version="".
func defaultRunner(parent context.Context, path string, args []string) (string, bool) {
	ctx, cancel := context.WithTimeout(parent, versionTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if errors.Is(runErr, context.DeadlineExceeded) {
		return "", false
	}

	out := firstNonEmptyLine(&stdout)
	if out == "" {
		out = firstNonEmptyLine(&stderr)
	}

	if runErr == nil {
		return out, true
	}
	// Non-zero exit: trust output only if it looks like a version.
	if out != "" && versionLikeRe.MatchString(out) {
		return out, true
	}
	return out, false
}

func firstNonEmptyLine(r io.Reader) string {
	data, _ := io.ReadAll(r)
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}
