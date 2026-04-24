package client

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/Tim-Butterfield/outpost/pkg/outpost"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/stem"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/transport"
)

// SubmitHandle tracks an in-flight submission. Callers use it to
// wait for a result or to cancel.
type SubmitHandle struct {
	target *Target
	lane   int
	stem   stem.Stem
}

// Stem returns the generated (or caller-supplied) stem that
// identifies this submission on the wire.
func (h *SubmitHandle) Stem() stem.Stem { return h.stem }

// Lane returns the lane the submission targeted.
func (h *SubmitHandle) Lane() int { return h.lane }

// Wait polls the outbox until a result file appears (or ctx
// cancels). The caller decides the total timeout via ctx;
// SubmitHandle does not impose one.
//
// Between polls the handle sleeps `interval` (default 200ms).
func (h *SubmitHandle) Wait(ctx context.Context) (outpost.Result, error) {
	return h.WaitWithInterval(ctx, 200*time.Millisecond)
}

// WaitWithInterval is Wait with a caller-specified poll interval.
// Useful in tests that want tighter polling.
func (h *SubmitHandle) WaitWithInterval(ctx context.Context, interval time.Duration) (outpost.Result, error) {
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		result, err := h.target.transport.GetResult(ctx, h.lane, h.stem)
		if err == nil {
			return result, nil
		}
		if !errors.Is(err, transport.ErrJobNotFound) {
			return outpost.Result{}, err
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return outpost.Result{}, ctx.Err()
		}
	}
}

// Cancel writes a cancel sentinel for this submission. The
// responder honors it either before or during dispatch.
func (h *SubmitHandle) Cancel(ctx context.Context) error {
	return h.target.transport.RequestCancel(ctx, h.lane, h.stem)
}

// Stdout opens the captured stdout for this submission. Callers
// typically wait for completion before reading; polling before a
// result is published returns transport.ErrJobNotFound.
func (h *SubmitHandle) Stdout(ctx context.Context) (io.ReadCloser, error) {
	return h.target.transport.OpenStdout(ctx, h.lane, h.stem)
}

// Stderr opens the captured stderr for this submission.
func (h *SubmitHandle) Stderr(ctx context.Context) (io.ReadCloser, error) {
	return h.target.transport.OpenStderr(ctx, h.lane, h.stem)
}
