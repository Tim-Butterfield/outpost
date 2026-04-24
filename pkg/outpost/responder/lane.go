package responder

import (
	"context"
	"io"
	"time"

	"github.com/Tim-Butterfield/outpost/pkg/outpost"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/capability"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/events"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/protocol"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/stem"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/transport"
)

// runLane is the per-lane worker loop. Runs for the life of the
// responder (or until stop is closed). Each lane owns its own
// status.txt and heartbeats independently of siblings.
func (r *Responder) runLane(ctx context.Context, lane int, stop <-chan struct{}) {
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()

	status := capability.Status{
		Lane:  lane,
		State: capability.StateReady,
	}
	r.writeHeartbeat(ctx, &status)

	for {
		select {
		case <-stop:
			status.State = capability.StateStopped
			status.BusyStem = ""
			status.Message = "stopped"
			r.writeHeartbeat(ctx, &status)
			return
		case <-ctx.Done():
			return
		default:
		}

		// Observe global PAUSE.
		paused, _ := r.cfg.Transport.CheckSentinel(ctx, protocol.SentinelPAUSE)
		if paused {
			// Still count the inbox even when paused so submitters
			// can see work accumulating. Ignoring the error matches
			// the non-paused branch: a transient ListPending failure
			// should not block heartbeat publication.
			pending, _ := r.cfg.Transport.ListPending(ctx, lane)
			status.Queued = len(pending)
			status.State = capability.StatePaused
			status.BusyStem = ""
			status.Message = "paused"
			r.writeHeartbeat(ctx, &status)
			if waitOrStop(ticker.C, stop, ctx) {
				continue
			}
			return
		}

		pending, err := r.cfg.Transport.ListPending(ctx, lane)
		if err != nil {
			r.emit(ctx, events.Event{
				Level:   "WARN",
				Lane:    lane,
				Message: "list pending failed",
				Fields:  map[string]string{"err": err.Error()},
			})
		}
		status.Queued = len(pending)

		if len(pending) == 0 {
			status.State = capability.StateIdle
			status.BusyStem = ""
			status.Message = ""
			r.writeHeartbeat(ctx, &status)
			if waitOrStop(ticker.C, stop, ctx) {
				continue
			}
			return
		}

		// Pick the oldest pending job (stem-sorted order).
		next := pending[0]

		// Pre-dispatch cancel check: a cancel sentinel placed before
		// we even pick up the job should skip execution entirely.
		if cancelled, _ := r.cfg.Transport.CheckCancel(ctx, lane, next.Stem); cancelled {
			r.handlePreDispatchCancel(ctx, lane, next)
			status.Queued--
			continue
		}

		// Transition to busy and write a heartbeat so submitters see
		// the state change promptly.
		status.State = capability.StateBusy
		status.BusyStem = next.Stem
		status.Message = ""
		r.writeHeartbeat(ctx, &status)

		// Long-running jobs would otherwise starve the submitter's
		// freshness check: the main lane goroutine is blocked in
		// dispatchOne until the worker exits, so status.txt
		// wouldn't get re-stamped. For any job that outlasts the
		// probe's default 10s freshness window, that flips the
		// target to STALE despite the worker being perfectly
		// healthy. Keep the heartbeat alive with a background
		// ticker, using a snapshot of the status so we don't race
		// with the outer loop's own updates.
		heartbeatDone := make(chan struct{})
		go func(snap capability.Status) {
			ticker := time.NewTicker(r.cfg.PollInterval)
			defer ticker.Stop()
			for {
				select {
				case <-heartbeatDone:
					return
				case <-ticker.C:
					r.writeHeartbeat(ctx, &snap)
				}
			}
		}(status)

		r.dispatchOne(ctx, lane, next)
		close(heartbeatDone)

		// Reset for the next iteration. The next heartbeat at loop
		// top (via the select case) will carry idle state.
		status.State = capability.StateIdle
		status.BusyStem = ""
	}
}

// dispatchOne reads the job content, dispatches it, watches for a
// mid-flight cancel sentinel, publishes the result, and archives
// the inbox file.
func (r *Responder) dispatchOne(ctx context.Context, lane int, pj transport.PendingJob) {
	content, err := r.readJobContent(ctx, lane, pj)
	if err != nil {
		r.emit(ctx, events.Event{
			Level:   "ERROR",
			Lane:    lane,
			Stem:    pj.Stem,
			Message: "failed to read job",
			Fields:  map[string]string{"err": err.Error()},
		})
		return
	}

	// Context scoped to this job. A mid-flight cancel sentinel
	// triggers ctx cancellation so the dispatcher kills the worker.
	jobCtx, jobCancel := context.WithCancel(ctx)
	defer jobCancel()

	watchDone := make(chan struct{})
	go r.watchMidFlightCancel(jobCtx, lane, pj.Stem, jobCancel, watchDone)

	job := outpost.Job{
		Lane:    lane,
		Stem:    pj.Stem,
		Ext:     pj.Ext,
		Content: content,
		Timeout: r.cfg.DefaultTimeout,
	}

	r.emit(ctx, events.Event{
		Level:   "INFO",
		Lane:    lane,
		Stem:    pj.Stem,
		Message: "job started",
	})

	result, stdout, stderr, runErr := r.cfg.Dispatcher.Run(jobCtx, job)
	close(watchDone)

	// Even on dispatcher-internal errors, publish a result so the
	// submitter is not left hanging.
	if runErr != nil {
		r.emit(ctx, events.Event{
			Level:   "ERROR",
			Lane:    lane,
			Stem:    pj.Stem,
			Message: "dispatcher error",
			Fields:  map[string]string{"err": runErr.Error()},
		})
		if result.ExitCode == 0 {
			result.ExitCode = protocol.ExitCodeDispatch
		}
		if result.Started.IsZero() {
			result.Started = time.Now().UTC()
		}
		if result.Finished.IsZero() {
			result.Finished = time.Now().UTC()
		}
		if len(stderr) == 0 {
			stderr = []byte(runErr.Error() + "\n")
		}
	}

	if err := r.cfg.Transport.PublishResult(ctx, lane, pj.Stem, result, stdout, stderr); err != nil {
		// Do NOT archive on publish failure: the inbox file is the
		// only remaining evidence that work is outstanding, and
		// archiving it would leave a waiting submitter with no
		// recoverable signal. Leave it in place so the next poll
		// cycle retries (or the operator can intervene). Trades
		// "possible re-execution if the script runs between a
		// successful dispatch and a failed publish" for "submitter
		// permanently blocked on a lost result file" — the former
		// is tractable for an idempotent script, the latter isn't.
		r.emit(ctx, events.Event{
			Level:   "ERROR",
			Lane:    lane,
			Stem:    pj.Stem,
			Message: "publish result failed; leaving inbox job in place for retry",
			Fields:  map[string]string{"err": err.Error()},
		})
		return
	}
	if err := r.cfg.Transport.ArchiveJob(ctx, lane, pj.Stem, pj.Ext); err != nil {
		r.emit(ctx, events.Event{
			Level:   "WARN",
			Lane:    lane,
			Stem:    pj.Stem,
			Message: "archive failed",
			Fields:  map[string]string{"err": err.Error()},
		})
	}

	r.emit(ctx, events.Event{
		Level:   "INFO",
		Lane:    lane,
		Stem:    pj.Stem,
		Message: "job finished",
		Fields: map[string]string{
			"exit":       fmtInt(result.ExitCode),
			"timeout":    boolFlag(result.TimedOut),
			"cancelled":  boolFlag(result.Cancelled),
			"duration":   result.Finished.Sub(result.Started).String(),
			"stdout_b":   fmtInt64(result.StdoutBytes),
			"stderr_b":   fmtInt64(result.StderrBytes),
			"truncated":  boolFlag(result.StdoutTruncated || result.StderrTruncated),
		},
	})
}

// handlePreDispatchCancel produces a .result for a job whose cancel
// sentinel appeared before dispatch ever started. No worker is
// spawned.
func (r *Responder) handlePreDispatchCancel(ctx context.Context, lane int, pj transport.PendingJob) {
	now := time.Now().UTC()
	result := outpost.Result{
		Stem:      pj.Stem,
		Lane:      lane,
		Ext:       pj.Ext,
		Label:     pj.Stem.Label(),
		ExitCode:  protocol.ExitCodeCancelled,
		Cancelled: true,
		Started:   now,
		Finished:  now,
	}
	if err := r.cfg.Transport.PublishResult(ctx, lane, pj.Stem, result, nil, []byte("cancelled before dispatch\n")); err != nil {
		r.emit(ctx, events.Event{
			Level:   "ERROR",
			Lane:    lane,
			Stem:    pj.Stem,
			Message: "publish pre-dispatch cancel result failed",
			Fields:  map[string]string{"err": err.Error()},
		})
		return
	}
	if err := r.cfg.Transport.ArchiveJob(ctx, lane, pj.Stem, pj.Ext); err != nil {
		r.emit(ctx, events.Event{
			Level:   "WARN",
			Lane:    lane,
			Stem:    pj.Stem,
			Message: "archive after pre-dispatch cancel failed",
			Fields:  map[string]string{"err": err.Error()},
		})
	}
	r.emit(ctx, events.Event{
		Level:   "INFO",
		Lane:    lane,
		Stem:    pj.Stem,
		Message: "job cancelled before dispatch",
	})
}

// readJobContent opens and buffers the job file. Buffered so the
// dispatcher can scan the content for the per-stem timeout header
// and repeatedly write it to a temp file without reopening.
func (r *Responder) readJobContent(ctx context.Context, lane int, pj transport.PendingJob) ([]byte, error) {
	rc, err := r.cfg.Transport.OpenJob(ctx, lane, pj.Stem, pj.Ext)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// watchMidFlightCancel polls the cancel sentinel for the busy stem
// on the poll cadence. On detection it calls jobCancel to stop the
// dispatcher; sibling lanes are unaffected.
func (r *Responder) watchMidFlightCancel(ctx context.Context, lane int, s stem.Stem, jobCancel context.CancelFunc, done <-chan struct{}) {
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			cancelled, _ := r.cfg.Transport.CheckCancel(ctx, lane, s)
			if cancelled {
				jobCancel()
				return
			}
		}
	}
}

// writeHeartbeat stamps LastHeartbeat and publishes the lane status.
// Failures are logged but do not stop the loop -- a stale heartbeat
// is the expected observability signal for a responder in trouble.
func (r *Responder) writeHeartbeat(ctx context.Context, status *capability.Status) {
	status.LastHeartbeat = time.Now().UTC()
	if err := r.cfg.Transport.WriteStatus(ctx, status.Lane, *status); err != nil {
		r.emit(ctx, events.Event{
			Level:   "WARN",
			Lane:    status.Lane,
			Message: "write status failed",
			Fields:  map[string]string{"err": err.Error()},
		})
	}
}

// waitOrStop returns true if the caller should loop again, false if
// it should exit. Multiplexes tick, stop, and ctx.Done.
func waitOrStop(tick <-chan time.Time, stop <-chan struct{}, ctx context.Context) bool {
	select {
	case <-tick:
		return true
	case <-stop:
		return false
	case <-ctx.Done():
		return false
	}
}

func fmtInt(n int) string          { return stringify(int64(n)) }
func fmtInt64(n int64) string      { return stringify(n) }
func boolFlag(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// stringify avoids pulling strconv into hot-path wrappers. A simple
// int64 decimal encoder is fine for the event-log use case.
func stringify(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
