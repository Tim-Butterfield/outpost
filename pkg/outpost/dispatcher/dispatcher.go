// Package dispatcher defines the interface responders use to
// execute a job and collect its result.
//
// The implementation shipped is pkg/outpost/dispatcher/subprocess.
// Alternative implementations slot in behind the same interface
// with no changes to responder code.
package dispatcher

import (
	"context"

	"github.com/Tim-Butterfield/outpost/pkg/outpost"
)

// Dispatcher runs a Job and returns the observed Result along with
// the captured stdout and stderr (each capped at
// protocol.MaxOutputBytes; truncation is signalled via fields on
// the returned Result).
//
// The returned error is for dispatcher-internal faults (unable to
// spawn, unable to allocate a temp file, etc.) that cannot be
// mapped to a worker exit code. Most "job did not succeed"
// outcomes live in the Result rather than the error return: a
// non-zero exit, a timeout, or a cancel all populate Result
// fields and leave err nil. This keeps the caller's control flow
// simple: the responder always publishes Result + captured output
// when err is nil, regardless of the job's success.
//
// Cancellation is via ctx; the dispatcher is expected to kill the
// worker's full process tree on ctx cancel.
type Dispatcher interface {
	Run(ctx context.Context, job outpost.Job) (result outpost.Result, stdout, stderr []byte, err error)
}

// Reloadable is an optional capability on Dispatcher implementations
// that store interpreter-path tables resolved at startup. When a
// responder detects its `outpost.toml` has changed (e.g., an
// operator ran `outpost target init --force` after installing a new
// interpreter), it calls UpdatePaths on any Reloadable dispatcher
// with the freshly resolved map. Dispatchers that don't carry a
// path table (sandboxed, containerized, WASI runtimes) simply
// don't implement this interface and are skipped at reload time.
type Reloadable interface {
	UpdatePaths(paths map[string]string)
}
