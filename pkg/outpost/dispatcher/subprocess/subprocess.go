// Package subprocess implements a Dispatcher that runs each job in
// a bare OS subprocess with cross-platform process-tree kill and
// bounded output capture.
//
// Interpreter paths come from the operator-supplied Config; the
// subprocess dispatcher never probes PATH at run time. That
// concern belongs to `outpost setup` and the responder's startup
// sequence (see DESIGN.md §4.6-4.7).
package subprocess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Tim-Butterfield/outpost/internal/platform"
	"github.com/Tim-Butterfield/outpost/pkg/outpost"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/protocol"
)

// Config constructs a Dispatcher with operator-resolved interpreter
// paths and defaults.
type Config struct {
	// InterpreterPaths maps a dispatch extension (no leading dot)
	// to the absolute path of the interpreter that should run it.
	// Missing entries cause Run to fail with
	// protocol.ExitCodeDispatch.
	InterpreterPaths map[string]string

	// DefaultTimeout is used when the submitted Job has a zero
	// timeout and no per-stem header override. Must be > 0.
	DefaultTimeout time.Duration

	// WorkingDir is the working directory for worker subprocesses
	// when the Job does not specify one. May be empty to inherit
	// the responder's cwd.
	WorkingDir string
}

// Dispatcher runs jobs as OS subprocesses.
type Dispatcher struct {
	// mu protects interpreterPaths. Other cfg fields are immutable
	// after New; only the path table can change (via UpdatePaths)
	// when the responder detects outpost.toml has been refreshed.
	mu               sync.RWMutex
	interpreterPaths map[string]string

	defaultTimeout time.Duration
	workingDir     string
}

// New returns a configured Dispatcher. The config is copied; later
// mutations to the caller's copy do not affect the Dispatcher.
func New(cfg Config) *Dispatcher {
	paths := copyPaths(cfg.InterpreterPaths)
	if cfg.DefaultTimeout <= 0 {
		cfg.DefaultTimeout = 60 * time.Second
	}
	return &Dispatcher{
		interpreterPaths: paths,
		defaultTimeout:   cfg.DefaultTimeout,
		workingDir:       cfg.WorkingDir,
	}
}

// UpdatePaths atomically swaps the interpreter-path table. Safe to
// call concurrently with Run: readers in Run briefly RLock to look
// up the interpreter, so the hot path isn't blocked and workers
// already dispatched continue with their resolved binary. Callers
// retain ownership of the input map; a defensive copy is made.
//
// Implements dispatcher.Reloadable.
func (d *Dispatcher) UpdatePaths(paths map[string]string) {
	copied := copyPaths(paths)
	d.mu.Lock()
	d.interpreterPaths = copied
	d.mu.Unlock()
}

// lookupInterpreter returns the configured binary path for ext, or
// ("", false) if none is configured. Holds the read lock only for
// the map lookup so UpdatePaths contention stays minimal.
func (d *Dispatcher) lookupInterpreter(ext string) (string, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	p, ok := d.interpreterPaths[ext]
	return p, ok
}

// copyPaths returns a deep copy of a paths map. Used by New and
// UpdatePaths to insulate the Dispatcher from caller mutations.
func copyPaths(src map[string]string) map[string]string {
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// Run implements dispatcher.Dispatcher.
//
// The method reports dispatch-setup failures via the returned
// Result (ExitCode=125) rather than the error return, so the
// responder can uniformly publish a Result for every job and only
// treat the error return as "I could not even report".
func (d *Dispatcher) Run(ctx context.Context, job outpost.Job) (outpost.Result, []byte, []byte, error) {
	result := outpost.Result{
		Stem:  job.Stem,
		Lane:  job.Lane,
		Ext:   job.Ext,
		Label: job.Stem.Label(),
	}

	// 1. Look up the interpreter for this extension. The read lock
	//    is brief (map lookup) and we take a copy of the resolved
	//    path so subsequent UpdatePaths calls can't invalidate it
	//    mid-dispatch.
	interp, ok := d.lookupInterpreter(job.Ext)
	if !ok {
		return dispatchError(result, fmt.Sprintf("no interpreter configured for .%s", job.Ext))
	}

	// 2. Parse per-stem timeout header; falls back through
	//    job.Timeout, then the configured default.
	timeout, err := resolveTimeout(job, d.defaultTimeout)
	if err != nil {
		return dispatchError(result, err.Error())
	}

	// 3. Materialize the script to a temp file the interpreter can
	//    read. The extension is preserved so interpreters like
	//    PowerShell that key off file extension still work.
	tmpDir, err := os.MkdirTemp("", "outpost-job-*")
	if err != nil {
		return dispatchError(result, "make tmp dir: "+err.Error())
	}
	defer os.RemoveAll(tmpDir)

	scriptPath := filepath.Join(tmpDir, string(job.Stem)+"."+job.Ext)
	if err := os.WriteFile(scriptPath, job.Content, 0600); err != nil {
		return dispatchError(result, "write script: "+err.Error())
	}

	// 4. Build command and buffers.
	args := append(interpArgs(job.Ext), scriptPath)
	cmd := exec.Command(interp, args...)
	if job.WorkingDir != "" {
		cmd.Dir = job.WorkingDir
	} else if d.workingDir != "" {
		cmd.Dir = d.workingDir
	}
	if len(job.Env) > 0 {
		cmd.Env = append(os.Environ(), job.Env...)
	}
	stdoutBuf := newBoundedWriter(protocol.MaxOutputBytes)
	stderrBuf := newBoundedWriter(protocol.MaxOutputBytes)
	cmd.Stdout = stdoutBuf
	cmd.Stderr = stderrBuf

	// 5. Wrap in ProcessGroup for cross-platform tree kill.
	pg := platform.New(cmd)
	result.Started = time.Now().UTC()

	if err := pg.Start(); err != nil {
		result.Finished = time.Now().UTC()
		return dispatchError(result, "start: "+err.Error())
	}

	// 6. Wait with timeout and ctx handling.
	doneCh := make(chan error, 1)
	go func() { doneCh <- pg.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var waitErr error
	select {
	case waitErr = <-doneCh:
		// normal exit or crash
	case <-timer.C:
		result.TimedOut = true
		_ = pg.Kill()
		waitErr = <-doneCh
	case <-ctx.Done():
		result.Cancelled = true
		_ = pg.Kill()
		waitErr = <-doneCh
	}

	result.Finished = time.Now().UTC()

	// 7. Map outcome to exit code.
	switch {
	case result.TimedOut:
		result.ExitCode = protocol.ExitCodeTimeout
	case result.Cancelled:
		result.ExitCode = protocol.ExitCodeCancelled
	case waitErr == nil:
		result.ExitCode = 0
	default:
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		} else {
			// Something other than a normal exit error: report as
			// dispatch failure with the message in stderr.
			msg := waitErr.Error()
			stderrBuf.WriteString("\n" + msg + "\n")
			result.ExitCode = protocol.ExitCodeDispatch
		}
	}

	// 8. Populate output-related fields.
	result.StdoutBytes = int64(stdoutBuf.Len())
	result.StderrBytes = int64(stderrBuf.Len())
	result.StdoutTruncated = stdoutBuf.Truncated()
	result.StderrTruncated = stderrBuf.Truncated()

	return result, stdoutBuf.Bytes(), stderrBuf.Bytes(), nil
}

// --- helpers ---

// interpArgs returns the fixed argument list placed between the
// interpreter binary and the script path for the given extension.
// Interpreters that take only a bare script path (sh, python,
// etc.) return nil here.
func interpArgs(ext string) []string {
	switch ext {
	case "ps1":
		return []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File"}
	case "cmd", "bat", "btm":
		return []string{"/c"}
	}
	return nil
}

// commentCharFor delegates to protocol.CommentCharForExt so the
// dispatcher and the client's Submit path agree on the extension
// set. Kept as a local thin wrapper for readability inside this
// package's parsers.
func commentCharFor(ext string) string {
	return protocol.CommentCharForExt(ext)
}

// resolveTimeout applies the three-tier precedence rule:
//  1. In-script `<comment> timeout=N` header (first 10 lines)
//  2. Job.Timeout caller override
//  3. Dispatcher default
//
// Returns an error when a header is present but invalid.
func resolveTimeout(job outpost.Job, defaultTimeout time.Duration) (time.Duration, error) {
	if t, ok, err := parseTimeoutHeader(job.Content, job.Ext); err != nil {
		return 0, err
	} else if ok {
		return t, nil
	}
	if job.Timeout > 0 {
		return job.Timeout, nil
	}
	return defaultTimeout, nil
}

// parseTimeoutHeader scans the first 10 lines of content for a
// per-stem timeout header. Returns (duration, true, nil) if found,
// (0, false, nil) if absent, and (0, false, err) if present but
// malformed. Extensions with no header syntax return (0, false,
// nil) unconditionally.
func parseTimeoutHeader(content []byte, ext string) (time.Duration, bool, error) {
	marker := commentCharFor(ext)
	if marker == "" {
		return 0, false, nil
	}
	// Normalize CRLF endings to make the scan portable.
	lines := strings.Split(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n")
	if len(lines) > 10 {
		lines = lines[:10]
	}
	markerLower := strings.ToLower(marker)
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(line), markerLower) {
			continue
		}
		rest := strings.TrimSpace(line[len(marker):])
		if !strings.HasPrefix(strings.ToLower(rest), "timeout=") {
			continue
		}
		value := strings.TrimSpace(rest[len("timeout="):])
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 86400 {
			return 0, false, fmt.Errorf("invalid timeout header %q (expected 1..86400)", rest)
		}
		return time.Duration(n) * time.Second, true, nil
	}
	return 0, false, nil
}

// dispatchError fills a Result representing a pre-execution
// dispatch failure. Sets exit code 125, timestamps, and writes the
// message to stderr so the responder publishes it.
func dispatchError(r outpost.Result, msg string) (outpost.Result, []byte, []byte, error) {
	now := time.Now().UTC()
	if r.Started.IsZero() {
		r.Started = now
	}
	if r.Finished.IsZero() {
		r.Finished = now
	}
	r.ExitCode = protocol.ExitCodeDispatch
	stderr := []byte(msg + "\n")
	r.StderrBytes = int64(len(stderr))
	return r, nil, stderr, nil
}

// --- boundedWriter ---

// boundedWriter is an io.Writer that records up to cap bytes and
// silently discards the rest, marking Truncated=true when the cap
// is exceeded. Always reports all bytes as written so upstream
// pipe copiers continue draining.
type boundedWriter struct {
	buf       bytes.Buffer
	cap       int64
	truncated bool
}

func newBoundedWriter(cap int64) *boundedWriter { return &boundedWriter{cap: cap} }

func (b *boundedWriter) Write(p []byte) (int, error) {
	if b.truncated {
		return len(p), nil
	}
	remaining := b.cap - int64(b.buf.Len())
	if int64(len(p)) > remaining {
		if remaining > 0 {
			b.buf.Write(p[:remaining])
		}
		b.truncated = true
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

// WriteString is a convenience used by dispatchError; the standard
// io.Writer interface only requires Write, but avoiding an
// intermediate allocation here is trivial.
func (b *boundedWriter) WriteString(s string) {
	_, _ = b.Write([]byte(s))
}

func (b *boundedWriter) Bytes() []byte  { return b.buf.Bytes() }
func (b *boundedWriter) Len() int       { return b.buf.Len() }
func (b *boundedWriter) Truncated() bool { return b.truncated }
