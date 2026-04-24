// Package transport defines the interface outpost uses to move
// jobs, results, status, and control-plane signals between a
// submitter and a responder.
//
// The implementation shipped is the file-RPC transport in the
// pkg/outpost/transport/file sub-package. Third parties can
// implement the same interface against other substrates.
//
// The interface is split into two narrower sub-interfaces:
// Submitter (what a client uses to submit and observe jobs) and
// Responder (what a daemon uses to publish its state and run
// incoming work). A concrete Transport implements both so a single
// shared directory / endpoint can be driven from either side.
//
// Optional extension interfaces (Watcher, Streamer) let transports
// that can push events do so without forcing the file-RPC
// implementation to fake them. Consumers type-assert to detect
// support.
//
// See DESIGN.md §4.1-4.2 for the volatility rationale behind this
// decomposition.
package transport

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/Tim-Butterfield/outpost/pkg/outpost"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/capability"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/stem"
)

// Transport is a bidirectional connection to one outpost responder.
// A single Transport value is used by both sides (submitter and
// responder); narrower code paths depend on the Submitter or
// Responder interface alone for clearer layering and easier
// mocking.
type Transport interface {
	Submitter
	Responder
	io.Closer
}

// Submitter is the submitter-side view of a transport. Clients
// (pkg/outpost/client) and third-party submitters depend on this
// interface.
type Submitter interface {
	// ReadDispatch returns the responder's identity and capability
	// advertisement (the content of inbox/dispatch.txt).
	ReadDispatch(ctx context.Context) (capability.Dispatch, error)

	// ReadStatus returns the per-lane liveness status (the content
	// of inbox/<lane>/status.txt).
	ReadStatus(ctx context.Context, lane int) (capability.Status, error)

	// PutJob writes content as a new job file into the given lane's
	// inbox, using the provided stem and dispatch extension. Writes
	// are atomic: a concurrent responder never observes a partial
	// file.
	PutJob(ctx context.Context, lane int, s stem.Stem, ext string, content io.Reader) error

	// GetResult reads and parses the result file for the given job.
	// Returns ErrJobNotFound if no result file has been published
	// yet; callers poll this method.
	GetResult(ctx context.Context, lane int, s stem.Stem) (outpost.Result, error)

	// OpenStdout / OpenStderr return readers for the captured
	// output streams. Returns ErrJobNotFound if the job has not
	// published output yet.
	OpenStdout(ctx context.Context, lane int, s stem.Stem) (io.ReadCloser, error)
	OpenStderr(ctx context.Context, lane int, s stem.Stem) (io.ReadCloser, error)

	// RequestCancel creates a cancel sentinel for the given job.
	// The responder observes the sentinel on its next poll cycle
	// (or mid-dispatch for an in-flight job). Idempotent: requesting
	// cancel twice has the same effect as once.
	RequestCancel(ctx context.Context, lane int, s stem.Stem) error

	// SetSentinel ensures the named global sentinel (STOP, PAUSE,
	// RESTART) is present (present=true) or absent (present=false).
	// Idempotent: setting an already-present sentinel returns nil.
	// Admin CLI commands (outpost stop / pause / resume) use this.
	SetSentinel(ctx context.Context, name string, present bool) error
}

// Responder is the responder-side view of a transport. The
// pkg/outpost/responder daemon depends on this interface.
type Responder interface {
	// WriteDispatch publishes the responder's identity and
	// capabilities. Called once at startup.
	WriteDispatch(ctx context.Context, d capability.Dispatch) error

	// WriteStatus publishes a per-lane status (heartbeat) atomically.
	// Called on every poll cycle by the lane's dispatcher goroutine.
	WriteStatus(ctx context.Context, lane int, s capability.Status) error

	// ListPending returns the pending jobs in the lane's inbox, in
	// stem-sorted order (chronological). Each PendingJob carries
	// the stem and the dispatch extension for routing.
	ListPending(ctx context.Context, lane int) ([]PendingJob, error)

	// OpenJob returns a reader on the job file's content. Callers
	// are expected to buffer or stream as needed before invoking
	// the dispatcher.
	OpenJob(ctx context.Context, lane int, s stem.Stem, ext string) (io.ReadCloser, error)

	// PublishResult writes the result file together with captured
	// stdout and stderr. All three writes are atomic; the
	// implementation may choose the order, but the caller may rely
	// on the result file appearing last as a "done" signal.
	PublishResult(ctx context.Context, lane int, s stem.Stem, r outpost.Result, stdout, stderr []byte) error

	// CheckCancel reports whether a cancel sentinel exists for the
	// given job. Called by the responder at dispatch boundaries and
	// during in-flight checks.
	CheckCancel(ctx context.Context, lane int, s stem.Stem) (bool, error)

	// ArchiveJob moves a completed job's inbox file to the log
	// directory and removes any cancel sentinel. Called by the
	// responder after PublishResult succeeds.
	ArchiveJob(ctx context.Context, lane int, s stem.Stem, ext string) error

	// Cleanup removes artifacts (log, outbox, cancel) older than
	// the given cutoff time. Implementations determine "age" by
	// the stem's timestamp prefix, not file mtime, so the policy is
	// deterministic across clock skew.
	Cleanup(ctx context.Context, before time.Time) error

	// CheckSentinel reports whether a global sentinel (STOP, PAUSE,
	// RESTART) is present. Called by the responder on each poll
	// cycle to decide whether to dispatch, pause, or exit.
	CheckSentinel(ctx context.Context, name string) (bool, error)
}

// PendingJob pairs a stem with its dispatch extension. Returned by
// ListPending so the responder doesn't need to re-enumerate the
// inbox directory to find the extension for a given stem.
type PendingJob struct {
	Stem stem.Stem
	Ext  string
}

// Watcher is an optional extension interface. Transports with
// push semantics implement it to replace the submitter's
// file-level polling with an event-driven wait. File-RPC does not
// implement this; consumers type-assert.
type Watcher interface {
	WatchResult(ctx context.Context, lane int, s stem.Stem) (<-chan ResultEvent, error)
}

// Streamer is an optional extension interface. Transports that
// can carry intermediate byte chunks implement it to let submitters
// observe output as it happens. File-RPC does not implement this;
// consumers type-assert.
type Streamer interface {
	StreamStdout(ctx context.Context, lane int, s stem.Stem) (<-chan []byte, error)
	StreamStderr(ctx context.Context, lane int, s stem.Stem) (<-chan []byte, error)
}

// ResultEvent is delivered on the channel returned by Watcher.
// Either Result is populated (job completed) or Err is populated
// (transport-level failure during the watch).
type ResultEvent struct {
	Result outpost.Result
	Err    error
}

// Error sentinels. Concrete transports return or wrap these so
// callers can branch on outcome without parsing error strings.
var (
	// ErrJobNotFound indicates the requested artifact (result,
	// stdout, stderr) does not yet exist for the given stem. Not
	// necessarily an error in normal use -- callers that poll will
	// see this until the responder publishes.
	ErrJobNotFound = errors.New("outpost: job not found")

	// ErrNotSupported indicates the transport does not implement an
	// optional capability (typically surfaced from type-assertion
	// misses, but also from transports that disable a feature).
	ErrNotSupported = errors.New("outpost: operation not supported by transport")

	// ErrTransportUnavailable indicates the underlying transport is
	// not reachable -- SMB share unmounted, HTTP endpoint down,
	// network partition, etc.
	ErrTransportUnavailable = errors.New("outpost: transport unavailable")

	// ErrAuthFailed indicates the submitter's identity was rejected
	// by the responder. file-RPC relies on filesystem ACLs and does
	// not surface this; other transports that authenticate would.
	ErrAuthFailed = errors.New("outpost: authentication failed")
)
