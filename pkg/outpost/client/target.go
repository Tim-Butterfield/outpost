package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Tim-Butterfield/outpost/pkg/outpost"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/auth"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/capability"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/protocol"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/stem"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/transport"
)

// Target is a handle to one outpost responder. The primary
// single-target API: construct a Client with one target and call
// methods directly (`c.Target("only").Submit(...)`), or build a
// Target yourself for ad-hoc --dir usage in the CLI.
type Target struct {
	name      string
	transport transport.Transport
	auth      auth.Authenticator
}

// NewTarget builds a one-off target without a Client. Useful for
// the CLI's `--dir <path>` ad-hoc mode where there is no registry
// entry to pull from.
func NewTarget(name string, tp transport.Transport) *Target {
	return &Target{name: name, transport: tp, auth: auth.NoOp()}
}

// Name returns the target's name as registered in the Client (or
// as passed to NewTarget).
func (t *Target) Name() string { return t.name }

// Transport returns the underlying transport. CLI admin commands
// (stop / pause / resume / clean) call through this directly
// rather than requiring per-concern methods on Target itself.
func (t *Target) Transport() transport.Transport { return t.transport }

// Submit generates a fresh stem if job.Stem is empty, writes the
// job into the target's inbox, and returns a SubmitHandle that
// can be used to wait or cancel. Fast: returns as soon as the
// inbox write succeeds.
//
// If job.Lane is zero, it defaults to 1 (the default lane
// enforced by the protocol).
func (t *Target) Submit(ctx context.Context, job outpost.Job) (*SubmitHandle, error) {
	if err := t.auth.Authorize(ctx); err != nil {
		return nil, err
	}
	if job.Lane == 0 {
		job.Lane = 1
	}
	if job.Lane < 1 {
		return nil, fmt.Errorf("client: Job.Lane must be >= 1; got %d", job.Lane)
	}
	if job.Stem == "" {
		label := job.Ext
		if label == "" {
			label = "job"
		}
		s, err := stem.NewGenerator().Next(label)
		if err != nil {
			return nil, fmt.Errorf("client: generate stem: %w", err)
		}
		job.Stem = s
	}
	if job.Ext == "" {
		return nil, errors.New("client: Job.Ext is required")
	}

	// Protocol-version check. Read the target's advertised version
	// from dispatch.txt and refuse to submit when it differs — the
	// compatibility guard documented in the design. Failure to read
	// dispatch.txt at all is treated as a separate, recoverable
	// condition (it can happen while a responder is first coming
	// up) and is allowed to pass through so existing integration
	// tests that put jobs before publishing dispatch.txt still work.
	disp, err := t.transport.ReadDispatch(ctx)
	if err == nil && disp.ProtocolVersion != 0 && disp.ProtocolVersion != protocol.Version {
		return nil, fmt.Errorf(
			"client: protocol version mismatch (target advertises %d, submitter speaks %d); upgrade one side to match",
			disp.ProtocolVersion, protocol.Version,
		)
	}
	if job.Lane > disp.LaneCount && disp.LaneCount > 0 {
		return nil, fmt.Errorf(
			"client: Job.Lane=%d exceeds target lane_count=%d",
			job.Lane, disp.LaneCount,
		)
	}

	// The file transport only carries the script body to the
	// responder's inbox, so Job.Timeout wouldn't otherwise reach the
	// dispatcher. Inject a `<comment> timeout=N` line the
	// subprocess dispatcher already parses -- that way the CLI's
	// `outpost submit --timeout 2s` actually bounds worker runtime
	// without us having to invent a sidecar metadata file.
	//
	// If the extension has no comment char (nothing in
	// protocol.CommentCharForExt returns non-empty), the header
	// can't be injected and Timeout is effectively advisory for
	// that extension. Fall-through safely: the worker still runs;
	// the responder's own DefaultTimeout still applies.
	content := job.Content
	if job.Timeout > 0 {
		seconds := int(job.Timeout.Round(time.Second).Seconds())
		if header := protocol.FormatTimeoutHeader(job.Ext, seconds); header != nil {
			content = append(header, content...)
		}
	}

	if err := t.transport.PutJob(ctx, job.Lane, job.Stem, job.Ext, bytes.NewReader(content)); err != nil {
		return nil, err
	}

	return &SubmitHandle{
		target: t,
		lane:   job.Lane,
		stem:   job.Stem,
	}, nil
}

// RequestCancel writes a per-stem cancel sentinel, signaling the
// responder to stop the job identified by (lane, stem). Effect
// depends on timing:
//
//   - Pre-dispatch: lane sees the sentinel before picking up the
//     job; the job is skipped and reported as cancelled.
//   - Mid-dispatch: the running worker is killed via the platform
//     process-group tree-kill; result reports ExitCodeCancelled.
//   - Post-dispatch: sentinel is observed after the job has
//     already completed; no effect on the result.
//
// Exposed at the Target level (rather than only via SubmitHandle)
// so external tooling can cancel by stem alone -- the stem is the
// only piece a caller needs to persist between submit and cancel.
func (t *Target) RequestCancel(ctx context.Context, lane int, s stem.Stem) error {
	if err := t.auth.Authorize(ctx); err != nil {
		return err
	}
	if lane < 1 {
		lane = 1
	}
	return t.transport.RequestCancel(ctx, lane, s)
}

// Capabilities reads and returns the target's dispatch.txt. Useful
// before submitting to confirm the responder supports the intended
// extension.
func (t *Target) Capabilities(ctx context.Context) (capability.Dispatch, error) {
	if err := t.auth.Authorize(ctx); err != nil {
		return capability.Dispatch{}, err
	}
	return t.transport.ReadDispatch(ctx)
}

// LaneStatus reads one lane's status.txt. Submitters use this
// directly when they want a lane-specific snapshot; Probe reads
// every lane's status at once.
func (t *Target) LaneStatus(ctx context.Context, lane int) (capability.Status, error) {
	if err := t.auth.Authorize(ctx); err != nil {
		return capability.Status{}, err
	}
	return t.transport.ReadStatus(ctx, lane)
}

// SetSentinel creates or removes a global sentinel (STOP / PAUSE /
// RESTART) at the target's shared-dir root. Admin-side operation.
func (t *Target) SetSentinel(ctx context.Context, name string, present bool) error {
	if err := t.auth.Authorize(ctx); err != nil {
		return err
	}
	return t.transport.SetSentinel(ctx, name, present)
}

// Cleanup forces a retention sweep on the target.
func (t *Target) Cleanup(ctx context.Context, before time.Time) error {
	if err := t.auth.Authorize(ctx); err != nil {
		return err
	}
	return t.transport.Cleanup(ctx, before)
}
