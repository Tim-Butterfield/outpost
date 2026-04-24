package probe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// smokeTimeout caps each per-interpreter smoke run. Generous enough
// for PowerShell's multi-second cold start without being painful
// when the whole init pipeline runs it over several interpreters.
const smokeTimeout = 5 * time.Second

// smokeSentinel is the string each smoke script prints. Chosen to
// not collide with any realistic shell prompt or error message so
// false positives on match are essentially impossible.
const smokeSentinel = "outpost-smoke-ok"

// smokeScripts maps a file extension to a trivial script body that
// (a) prints smokeSentinel and (b) exits 0 on every interpreter
// that claims that extension. Scripts are as idiomatic and minimal
// as possible for the language; the goal is "exercise the basic
// stdout path," not "test the language."
//
// Keys must match the extensions in the compiled-in probe table so
// every detected interpreter has a matching script.
var smokeScripts = map[string]string{
	"sh":  "echo " + smokeSentinel + "\n",
	"zsh": "echo " + smokeSentinel + "\n",
	"py":  "print(\"" + smokeSentinel + "\")\n",
	"js":  "console.log(\"" + smokeSentinel + "\")\n",
	"ts":  "console.log(\"" + smokeSentinel + "\")\n",
	"rb":  "puts \"" + smokeSentinel + "\"\n",
	"pl":  "print \"" + smokeSentinel + "\\n\";\n",
	"php": "<?php echo \"" + smokeSentinel + "\\n\";\n",
	"lua": "print(\"" + smokeSentinel + "\")\n",
	"ps1": "Write-Output \"" + smokeSentinel + "\"\n",
	// @echo off suppresses command echoing so we can match stdout
	// cleanly. Works identically in cmd.exe, 4NT, and TCC.
	"cmd": "@echo off\r\necho " + smokeSentinel + "\r\n",
	"bat": "@echo off\r\necho " + smokeSentinel + "\r\n",
	"btm": "@echo off\r\necho " + smokeSentinel + "\r\n",
}

// smokeArgs mirrors the subprocess dispatcher's interpArgs in
// pkg/outpost/dispatcher/subprocess/subprocess.go. Duplicated here
// intentionally so the probe package doesn't depend on the
// dispatcher — probe is a leaf by design. Any change to invocation
// conventions needs to be reflected in both places.
func smokeArgs(ext string) []string {
	switch ext {
	case "ps1":
		return []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File"}
	case "cmd", "bat", "btm":
		return []string{"/c"}
	}
	return nil
}

// VerifySmoke runs a trivial script through each working
// interpreter. Any interpreter whose script fails (non-zero exit,
// unexpected stdout, or timeout) has its Working flag cleared and
// its SmokeError populated with the failure reason. Interpreters
// already marked Working=false (from the version probe) are left
// alone. Returns a new slice; does not mutate the input.
//
// The first extension in an interpreter's Extensions list is used
// for the smoke script. This matches the dispatch table's
// "first-match-wins" semantics: if an interpreter is going to be
// used for .sh files, we smoke-test it via a .sh script.
func VerifySmoke(ctx context.Context, interps []Interpreter) []Interpreter {
	out := make([]Interpreter, len(interps))
	for i, interp := range interps {
		out[i] = interp
		if !interp.Working {
			continue
		}
		if len(interp.Extensions) == 0 {
			continue
		}
		ext := interp.Extensions[0]
		script, ok := smokeScripts[ext]
		if !ok {
			// No smoke script for this extension (e.g. new ext added
			// to the table without a smoke script entry). Treat as
			// "cannot verify" and leave Working as-is rather than
			// demoting a potentially-fine interpreter.
			continue
		}
		if err := runSmoke(ctx, interp.Path, ext, script); err != nil {
			out[i].Working = false
			out[i].SmokeError = err.Error()
		}
	}
	return out
}

// runSmoke writes script to a tempfile with the given extension,
// invokes bin with the extension's invocation args plus the
// tempfile path, and verifies stdout contains smokeSentinel and
// the process exited 0.
func runSmoke(parent context.Context, bin, ext, script string) error {
	ctx, cancel := context.WithTimeout(parent, smokeTimeout)
	defer cancel()

	tmpDir, err := os.MkdirTemp("", "outpost-smoke-*")
	if err != nil {
		return fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	scriptPath := filepath.Join(tmpDir, "smoke."+ext)
	if err := os.WriteFile(scriptPath, []byte(script), 0644); err != nil {
		return fmt.Errorf("write script: %w", err)
	}

	args := append(smokeArgs(ext), scriptPath)
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New("smoke test timed out")
	}
	if runErr != nil {
		// Prefer stderr for the error message if present; falls back
		// to the Go wait error (exit code) when stderr is empty.
		// Uses the existing firstNonEmptyLine helper in probe.go
		// which takes an io.Reader.
		msg := firstNonEmptyLine(&stderr)
		if msg == "" {
			msg = runErr.Error()
		}
		return fmt.Errorf("exit non-zero: %s", msg)
	}
	if !strings.Contains(stdout.String(), smokeSentinel) {
		return fmt.Errorf("unexpected stdout: %q", truncateForError(stdout.String(), 120))
	}
	return nil
}

// truncateForError returns s trimmed to n runes with an ellipsis
// when longer. Used to keep error strings readable when an
// interpreter prints a firehose of stdout before failing.
func truncateForError(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
