//go:build unix

package subprocess

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Tim-Butterfield/outpost/pkg/outpost"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/protocol"
)

// findShOrSkip returns the absolute path to /bin/sh or skips the
// test if unavailable. All Unix CI runners have /bin/sh; this is
// mostly a guard for exotic environments.
func findShOrSkip(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available")
	}
	return "/bin/sh"
}

func newShDispatcher(t *testing.T, defaultTimeout time.Duration) *Dispatcher {
	t.Helper()
	return New(Config{
		InterpreterPaths: map[string]string{"sh": findShOrSkip(t)},
		DefaultTimeout:   defaultTimeout,
	})
}

func TestRunSh_HelloExitZero(t *testing.T) {
	d := newShDispatcher(t, 5*time.Second)
	job := outpost.Job{
		Stem:    validTestStem(t, "hello"),
		Lane:    1,
		Ext:     "sh",
		Content: []byte("echo hello\n"),
	}
	res, stdout, stderr, err := d.Run(t.Context(), job)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit=%d, want 0", res.ExitCode)
	}
	if !bytes.Equal(stdout, []byte("hello\n")) {
		t.Errorf("stdout=%q, want %q", stdout, "hello\n")
	}
	if len(stderr) != 0 {
		t.Errorf("stderr should be empty: %q", stderr)
	}
	if res.StdoutBytes != 6 {
		t.Errorf("StdoutBytes=%d, want 6", res.StdoutBytes)
	}
	if res.TimedOut || res.Cancelled || res.StdoutTruncated || res.StderrTruncated {
		t.Errorf("flags should be clear: %+v", res)
	}
	if res.Finished.Before(res.Started) {
		t.Errorf("Finished before Started: %+v", res)
	}
}

func TestRunSh_NonZeroExit(t *testing.T) {
	d := newShDispatcher(t, 5*time.Second)
	job := outpost.Job{
		Stem:    validTestStem(t, "fail"),
		Lane:    1,
		Ext:     "sh",
		Content: []byte("echo to-stderr 1>&2\nexit 7\n"),
	}
	res, _, stderr, err := d.Run(t.Context(), job)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("exit=%d, want 7", res.ExitCode)
	}
	if !bytes.Contains(stderr, []byte("to-stderr")) {
		t.Errorf("stderr should contain script's stderr: %q", stderr)
	}
}

func TestRunSh_Timeout(t *testing.T) {
	d := newShDispatcher(t, 200*time.Millisecond)
	job := outpost.Job{
		Stem:    validTestStem(t, "hang"),
		Lane:    1,
		Ext:     "sh",
		Content: []byte("while :; do sleep 0.1; done\n"),
	}
	start := time.Now()
	res, _, _, err := d.Run(t.Context(), job)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	elapsed := time.Since(start)
	if !res.TimedOut {
		t.Errorf("TimedOut should be true")
	}
	if res.ExitCode != protocol.ExitCodeTimeout {
		t.Errorf("exit=%d, want %d", res.ExitCode, protocol.ExitCodeTimeout)
	}
	if elapsed > 3*time.Second {
		t.Errorf("timeout took too long: %v", elapsed)
	}
}

func TestRunSh_TimeoutHeaderOverride(t *testing.T) {
	// Default timeout is 10s; header says 200ms. Header wins.
	d := newShDispatcher(t, 10*time.Second)
	job := outpost.Job{
		Stem:    validTestStem(t, "header"),
		Lane:    1,
		Ext:     "sh",
		Content: []byte("# timeout=1\nwhile :; do sleep 0.1; done\n"),
	}
	start := time.Now()
	res, _, _, err := d.Run(t.Context(), job)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	elapsed := time.Since(start)
	if !res.TimedOut {
		t.Errorf("TimedOut should be true")
	}
	// Minimum 1s from header; bound above at ~3s to detect that
	// default (10s) didn't incorrectly override.
	if elapsed < 900*time.Millisecond || elapsed > 3*time.Second {
		t.Errorf("elapsed=%v not in [1s..3s]; header timeout not honored?", elapsed)
	}
}

func TestRunSh_CtxCancel(t *testing.T) {
	d := newShDispatcher(t, 30*time.Second)
	job := outpost.Job{
		Stem:    validTestStem(t, "cancel"),
		Lane:    1,
		Ext:     "sh",
		Content: []byte("while :; do sleep 0.1; done\n"),
	}
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	res, _, _, err := d.Run(ctx, job)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	elapsed := time.Since(start)
	if !res.Cancelled {
		t.Errorf("Cancelled should be true")
	}
	if res.ExitCode != protocol.ExitCodeCancelled {
		t.Errorf("exit=%d, want %d", res.ExitCode, protocol.ExitCodeCancelled)
	}
	if elapsed > 3*time.Second {
		t.Errorf("cancel took too long: %v", elapsed)
	}
}

func TestRunSh_OutputTruncation(t *testing.T) {
	// Override MaxOutputBytes indirectly by asking the shell to
	// print more than protocol.MaxOutputBytes would comfortably
	// admit. 100 MB is too large to test quickly; the dispatcher
	// hardcodes that cap. We verify the mechanism by using a
	// smaller cap via a custom dispatcher configuration. Since
	// protocol.MaxOutputBytes isn't currently configurable, the test
	// relies on the bounded writer unit tests (TestBoundedWriter_*)
	// for the cap logic and only verifies end-to-end wiring here
	// with a very small write that does not actually truncate.
	d := newShDispatcher(t, 5*time.Second)
	job := outpost.Job{
		Stem:    validTestStem(t, "smallout"),
		Lane:    1,
		Ext:     "sh",
		Content: []byte("printf small\n"),
	}
	res, stdout, _, err := d.Run(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	if res.StdoutTruncated {
		t.Errorf("small output should not be truncated")
	}
	if !strings.HasPrefix(string(stdout), "small") {
		t.Errorf("stdout=%q", stdout)
	}
}

func TestRunSh_EnvOverride(t *testing.T) {
	d := newShDispatcher(t, 5*time.Second)
	job := outpost.Job{
		Stem:    validTestStem(t, "env"),
		Lane:    1,
		Ext:     "sh",
		Content: []byte("echo \"$OUTPOST_TEST_VALUE\"\n"),
		Env:     []string{"OUTPOST_TEST_VALUE=hello-from-env"},
	}
	res, stdout, _, err := d.Run(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit=%d", res.ExitCode)
	}
	if !bytes.Contains(stdout, []byte("hello-from-env")) {
		t.Errorf("env var not propagated; stdout=%q", stdout)
	}
}
