package subprocess

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/Tim-Butterfield/outpost/pkg/outpost"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/protocol"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/stem"
)

// --- boundedWriter ---

func TestBoundedWriter_BelowCap(t *testing.T) {
	b := newBoundedWriter(100)
	n, err := b.Write([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("n=%d, want 5", n)
	}
	if b.Truncated() {
		t.Error("should not be truncated")
	}
	if got := string(b.Bytes()); got != "hello" {
		t.Errorf("bytes=%q", got)
	}
}

func TestBoundedWriter_AtCap(t *testing.T) {
	b := newBoundedWriter(5)
	n, err := b.Write([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("n=%d, want 5", n)
	}
	if b.Truncated() {
		t.Error("should not be truncated when exactly at cap")
	}
}

func TestBoundedWriter_OverCapSingleWrite(t *testing.T) {
	b := newBoundedWriter(5)
	n, err := b.Write([]byte("hello world"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 11 {
		t.Errorf("Write must report full len even when truncating; n=%d, want 11", n)
	}
	if !b.Truncated() {
		t.Error("should be truncated")
	}
	if got := string(b.Bytes()); got != "hello" {
		t.Errorf("truncated bytes=%q, want hello", got)
	}
}

func TestBoundedWriter_OverCapMultipleWrites(t *testing.T) {
	b := newBoundedWriter(5)
	_, _ = b.Write([]byte("abc"))
	_, _ = b.Write([]byte("de"))
	if b.Truncated() {
		t.Error("should not be truncated yet")
	}
	_, _ = b.Write([]byte("fghij"))
	if !b.Truncated() {
		t.Error("should be truncated after exceeding cap")
	}
	if got := string(b.Bytes()); got != "abcde" {
		t.Errorf("bytes=%q, want abcde", got)
	}
}

func TestBoundedWriter_AfterTruncationIgnores(t *testing.T) {
	b := newBoundedWriter(3)
	_, _ = b.Write([]byte("abcdefg"))
	if got := string(b.Bytes()); got != "abc" {
		t.Errorf("first truncation: bytes=%q", got)
	}
	// Further writes must be silently discarded.
	n, _ := b.Write([]byte("xyz"))
	if n != 3 {
		t.Errorf("Write after truncation should still report full len")
	}
	if got := string(b.Bytes()); got != "abc" {
		t.Errorf("bytes grew after truncation: %q", got)
	}
}

// --- timeout header parsing ---

func TestParseTimeoutHeader_Unscannable(t *testing.T) {
	// Extensions with no comment convention return (0, false, nil).
	for _, ext := range []string{"", "xyz", "rb"} {
		t.Run(ext, func(t *testing.T) {
			_, ok, err := parseTimeoutHeader([]byte("# timeout=10\n"), ext)
			if err != nil || ok {
				t.Errorf("unexpected: err=%v ok=%v", err, ok)
			}
		})
	}
}

func TestParseTimeoutHeader_Sh(t *testing.T) {
	tests := []struct {
		name, in string
		want     time.Duration
		found    bool
	}{
		{"hash simple", "# timeout=30\nexit 0\n", 30 * time.Second, true},
		{"hash with prefix", "#!/bin/sh\n# timeout=5\n", 5 * time.Second, true},
		{"no header", "echo hello\n", 0, false},
		{"beyond 10th line", strings.Repeat("\n", 15) + "# timeout=10\n", 0, false},
		{"tight whitespace", "#timeout=7\n", 7 * time.Second, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := parseTimeoutHeader([]byte(tc.in), "sh")
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if ok != tc.found || got != tc.want {
				t.Errorf("got (%v, %v), want (%v, %v)", got, ok, tc.want, tc.found)
			}
		})
	}
}

func TestParseTimeoutHeader_Cmd(t *testing.T) {
	got, ok, err := parseTimeoutHeader([]byte("REM timeout=45\n@echo hi\n"), "cmd")
	if err != nil || !ok || got != 45*time.Second {
		t.Errorf("got (%v, %v, %v), want (45s, true, nil)", got, ok, err)
	}
	// Lowercase 'rem' also accepted.
	got, ok, err = parseTimeoutHeader([]byte("rem timeout=60\n"), "cmd")
	if err != nil || !ok || got != 60*time.Second {
		t.Errorf("lowercase rem: got (%v, %v, %v)", got, ok, err)
	}
}

func TestParseTimeoutHeader_Btm(t *testing.T) {
	got, ok, err := parseTimeoutHeader([]byte(":: timeout=120\n"), "btm")
	if err != nil || !ok || got != 120*time.Second {
		t.Errorf("got (%v, %v, %v), want (120s, true, nil)", got, ok, err)
	}
}

func TestParseTimeoutHeader_CRLF(t *testing.T) {
	got, ok, err := parseTimeoutHeader([]byte("# timeout=15\r\nexit 0\r\n"), "sh")
	if err != nil || !ok || got != 15*time.Second {
		t.Errorf("CRLF failed: got (%v, %v, %v)", got, ok, err)
	}
}

func TestParseTimeoutHeader_InvalidValue(t *testing.T) {
	cases := []string{
		"# timeout=0\n",
		"# timeout=-5\n",
		"# timeout=abc\n",
		"# timeout=86401\n",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			_, _, err := parseTimeoutHeader([]byte(in), "sh")
			if err == nil {
				t.Errorf("expected error for %q", in)
			}
		})
	}
}

// --- resolveTimeout precedence ---

func TestResolveTimeout_Precedence(t *testing.T) {
	job := outpost.Job{
		Ext:     "sh",
		Content: []byte("# timeout=5\n"),
		Timeout: 10 * time.Second,
	}
	got, err := resolveTimeout(job, 60*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got != 5*time.Second {
		t.Errorf("header should win; got %v", got)
	}

	// Without header: Job.Timeout wins.
	job.Content = []byte("echo hi\n")
	got, _ = resolveTimeout(job, 60*time.Second)
	if got != 10*time.Second {
		t.Errorf("Job.Timeout should win when no header; got %v", got)
	}

	// Without header or Job.Timeout: default wins.
	job.Timeout = 0
	got, _ = resolveTimeout(job, 60*time.Second)
	if got != 60*time.Second {
		t.Errorf("default should win; got %v", got)
	}
}

// --- dispatcher behavior that doesn't need a real subprocess ---

func TestRun_UnknownExtension(t *testing.T) {
	d := New(Config{InterpreterPaths: map[string]string{"sh": "/bin/sh"}})
	job := outpost.Job{
		Stem: validTestStem(t, "probe"),
		Lane: 1,
		Ext:  "rb", // not in InterpreterPaths
	}
	res, stdout, stderr, err := d.Run(t.Context(), job)
	if err != nil {
		t.Fatalf("Run should not return err for dispatch failure: %v", err)
	}
	if res.ExitCode != protocol.ExitCodeDispatch {
		t.Errorf("exit=%d, want %d", res.ExitCode, protocol.ExitCodeDispatch)
	}
	if len(stdout) != 0 {
		t.Errorf("stdout should be empty: %q", stdout)
	}
	if !bytes.Contains(stderr, []byte("no interpreter configured")) {
		t.Errorf("stderr should explain: %q", stderr)
	}
}

func TestRun_InvalidTimeoutHeader(t *testing.T) {
	d := New(Config{InterpreterPaths: map[string]string{"sh": "/bin/sh"}})
	job := outpost.Job{
		Stem:    validTestStem(t, "probe"),
		Lane:    1,
		Ext:     "sh",
		Content: []byte("# timeout=99999\necho hi\n"), // > 86400
	}
	res, _, stderr, err := d.Run(t.Context(), job)
	if err != nil {
		t.Fatalf("Run should not return err: %v", err)
	}
	if res.ExitCode != protocol.ExitCodeDispatch {
		t.Errorf("exit=%d, want %d", res.ExitCode, protocol.ExitCodeDispatch)
	}
	if !bytes.Contains(stderr, []byte("invalid timeout")) {
		t.Errorf("stderr should explain: %q", stderr)
	}
}

// --- test helpers ---

// validTestStem constructs a stem through the real generator so the
// test does not depend on the stem's internal string layout.
func validTestStem(t *testing.T, label string) stem.Stem {
	t.Helper()
	s, err := stem.NewGenerator().Next(label)
	if err != nil {
		t.Fatalf("stem.Next: %v", err)
	}
	return s
}

// --- Config plumbing ---

func TestNew_CopiesInterpreterPaths(t *testing.T) {
	paths := map[string]string{"sh": "/bin/sh"}
	d := New(Config{InterpreterPaths: paths, DefaultTimeout: time.Second})
	// Mutating caller's map after construction must not affect the
	// dispatcher's internal copy.
	paths["sh"] = "/tmp/evil"
	paths["py"] = "/tmp/evil"

	job := outpost.Job{
		Stem: validTestStem(t, "probe"),
		Ext:  "py",
	}
	res, _, _, err := d.Run(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	// py should still be unknown because it wasn't in the original
	// map at construction time.
	if res.ExitCode != protocol.ExitCodeDispatch {
		t.Errorf("py should be unknown; exit=%d", res.ExitCode)
	}
}

func TestNew_DefaultTimeoutFloorsToSaneValue(t *testing.T) {
	d := New(Config{})
	if d.defaultTimeout <= 0 {
		t.Errorf("defaultTimeout must be positive; got %v", d.defaultTimeout)
	}
}

func TestUpdatePaths_SwapsAtomically(t *testing.T) {
	d := New(Config{
		InterpreterPaths: map[string]string{"sh": "/bin/bash"},
		DefaultTimeout:   time.Second,
	})
	if got, ok := d.lookupInterpreter("sh"); !ok || got != "/bin/bash" {
		t.Fatalf("initial lookup: got (%q,%v), want (/bin/bash,true)", got, ok)
	}
	d.UpdatePaths(map[string]string{
		"sh": "/bin/sh",
		"py": "/usr/bin/python3",
	})
	if got, ok := d.lookupInterpreter("sh"); !ok || got != "/bin/sh" {
		t.Errorf("after update (sh): got (%q,%v), want (/bin/sh,true)", got, ok)
	}
	if got, ok := d.lookupInterpreter("py"); !ok || got != "/usr/bin/python3" {
		t.Errorf("after update (py): got (%q,%v), want (/usr/bin/python3,true)", got, ok)
	}
	if _, ok := d.lookupInterpreter("lua"); ok {
		t.Error("lua should not be in the updated map")
	}
}

func TestUpdatePaths_CallerMutationDoesNotAffectDispatcher(t *testing.T) {
	src := map[string]string{"sh": "/bin/bash"}
	d := New(Config{
		InterpreterPaths: src,
		DefaultTimeout:   time.Second,
	})
	next := map[string]string{"sh": "/bin/sh"}
	d.UpdatePaths(next)
	// Mutate after UpdatePaths returns; defensive copy must prevent
	// this from reaching the dispatcher.
	next["sh"] = "/something/else"
	if got, _ := d.lookupInterpreter("sh"); got != "/bin/sh" {
		t.Errorf("caller mutation leaked through: got %q, want /bin/sh", got)
	}
}
