// Package outpost is the top-level user-facing API for the outpost
// file-RPC bridge.
//
// It exports the concrete types shared across the transport and
// dispatcher interfaces: Job (what submitters send; what
// dispatchers execute) and Result (what dispatchers produce; what
// submitters observe).
package outpost

import (
	"time"

	"github.com/Tim-Butterfield/outpost/pkg/outpost/protocol"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/stem"
)

// Version is the wire protocol version exported at the package
// root for caller convenience. Equal to protocol.Version.
const Version = protocol.Version

// Responder-originated exit codes. Re-exported at the package root
// so callers writing checks like `if result.ExitCode ==
// outpost.ExitCodeTimeout { ... }` do not have to import the
// protocol sub-package.
const (
	ExitCodeTimeout   = protocol.ExitCodeTimeout   // 124
	ExitCodeDispatch  = protocol.ExitCodeDispatch  // 125
	ExitCodeCancelled = protocol.ExitCodeCancelled // 126
)

// NewStem is a convenience wrapper around stem.NewGenerator().Next
// for the common case of a one-off submission where the caller
// does not need to keep a generator around. Thread-safe per-call
// by virtue of constructing a fresh generator each time.
//
// For bursts of submissions, construct a generator once and
// reuse it: the shared generator enforces monotonic-microsecond
// uniqueness, whereas independent generators could theoretically
// collide (in practice: not in any realistic rate).
func NewStem(label string) (stem.Stem, error) {
	return stem.NewGenerator().Next(label)
}

// Job is a single unit of work submitted to a responder. It names
// which lane to target, identifies itself via a Stem, declares the
// file extension that drives interpreter dispatch on the responder,
// and carries the raw script content.
//
// A caller-side per-job timeout override appears here; the
// responder's own per-stem timeout header in the script body takes
// precedence (see DESIGN.md §3.4).
type Job struct {
	// Lane is the target lane on the responder (1..LaneCount).
	Lane int

	// Stem is the unique identifier for this job. Callers use
	// stem.Generator.Next to produce a fresh stem.
	Stem stem.Stem

	// Ext is the dispatch extension without a leading dot. Examples:
	// "sh", "py", "ps1", "cmd", "btm".
	Ext string

	// Content is the raw script body. The submitter writes this
	// verbatim to the inbox; the dispatcher reads it at execution
	// time.
	Content []byte

	// Timeout is the caller-requested per-job timeout. A value of
	// zero means "use the responder's default". The per-stem header
	// in Content overrides this if present.
	Timeout time.Duration

	// Env is a set of NAME=VALUE entries appended to the worker
	// process's environment. Optional.
	Env []string

	// WorkingDir, if non-empty, overrides the dispatcher's default
	// working directory for this job's subprocess.
	WorkingDir string
}

// Result is the responder's observation of a completed job.
// Populated by the dispatcher, propagated to the submitter through
// the transport, and serialized into `.result` on disk (via
// capability-equivalent key=value encoding in pkg/outpost/capability
// when the transport writes it).
type Result struct {
	// Stem, Lane, Ext, Label echo the Job's identity. Label is
	// extracted from Stem.Label() for convenience.
	Stem  stem.Stem
	Lane  int
	Ext   string
	Label string

	// ExitCode is the worker's exit code on normal completion, or
	// one of the responder-originated codes on timeout (124),
	// dispatch error (125), or cancel (126). See
	// pkg/outpost/protocol.
	ExitCode int

	// TimedOut is true when the dispatcher killed the worker for
	// exceeding its timeout.
	TimedOut bool

	// Cancelled is true when the dispatcher killed the worker in
	// response to a submitter-initiated cancel.
	Cancelled bool

	// Started and Finished bracket the worker subprocess's wall
	// time, both in UTC.
	Started  time.Time
	Finished time.Time

	// StdoutBytes and StderrBytes count captured output bytes, up
	// to protocol.MaxOutputBytes per stream. When a stream exceeded
	// the cap, the corresponding *Truncated flag is set.
	StdoutBytes     int64
	StderrBytes     int64
	StdoutTruncated bool
	StderrTruncated bool
}
