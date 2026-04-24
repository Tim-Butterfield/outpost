//go:build unix

package responder_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Tim-Butterfield/outpost/pkg/outpost"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/capability"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/dispatcher/subprocess"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/events"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/protocol"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/responder"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/stem"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/transport"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/transport/file"
)

// newTestResponder wires a real file transport + subprocess
// dispatcher (running /bin/sh) in a tmpdir.
func newTestResponder(t *testing.T, laneCount int, pollInterval time.Duration) (*responder.Responder, *file.Transport) {
	t.Helper()
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available")
	}
	dir := t.TempDir()
	tp := file.New(dir)
	if err := tp.Prepare(laneCount); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	disp := subprocess.New(subprocess.Config{
		InterpreterPaths: map[string]string{"sh": "/bin/sh"},
		DefaultTimeout:   2 * time.Second,
	})
	r, err := responder.New(responder.Config{
		Transport:      tp,
		Dispatcher:     disp,
		Events:         events.Discard(),
		ResponderName:  "test-responder",
		LaneCount:      laneCount,
		PollInterval:   pollInterval,
		DefaultTimeout: 2 * time.Second,
		DispatchTable:  map[string]string{"sh": "/bin/sh"},
		DispatchOrder:  []string{"sh"},
	}, 42)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r, tp
}

// runInBackground starts the responder goroutine and returns
// (done channel, cancel func). Gives the responder a moment to
// publish dispatch.txt.
func runInBackground(t *testing.T, r *responder.Responder) (<-chan error, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	time.Sleep(100 * time.Millisecond)
	return done, cancel
}

func submitJob(t *testing.T, tp *file.Transport, lane int, label, content string) stem.Stem {
	t.Helper()
	s, err := stem.NewGenerator().Next(label)
	if err != nil {
		t.Fatal(err)
	}
	if err := tp.PutJob(context.Background(), lane, s, "sh", bytes.NewReader([]byte(content))); err != nil {
		t.Fatalf("PutJob: %v", err)
	}
	return s
}

func waitForResult(t *testing.T, tp *file.Transport, lane int, s stem.Stem, timeout time.Duration) (outpost.Result, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		result, err := tp.GetResult(context.Background(), lane, s)
		if err == nil {
			return result, true
		}
		if !errors.Is(err, transport.ErrJobNotFound) {
			t.Fatalf("GetResult: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return outpost.Result{}, false
}

func TestResponder_HappyPath(t *testing.T) {
	r, tp := newTestResponder(t, 1, 50*time.Millisecond)
	done, cancel := runInBackground(t, r)

	s := submitJob(t, tp, 1, "hello", "echo hi\n")

	result, ok := waitForResult(t, tp, 1, s, 5*time.Second)
	if !ok {
		t.Fatalf("result for %s never appeared", s)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit=%d, want 0", result.ExitCode)
	}

	// Clean shutdown via ctx cancel.
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("responder did not exit after ctx cancel")
	}
}

func TestResponder_DispatchAdvertisedAtStartup(t *testing.T) {
	r, tp := newTestResponder(t, 1, 50*time.Millisecond)
	done, cancel := runInBackground(t, r)
	defer func() { cancel(); <-done }()

	disp, err := tp.ReadDispatch(context.Background())
	if err != nil {
		t.Fatalf("ReadDispatch: %v", err)
	}
	if disp.ResponderName != "test-responder" {
		t.Errorf("ResponderName=%q", disp.ResponderName)
	}
	if disp.LaneCount != 1 {
		t.Errorf("LaneCount=%d", disp.LaneCount)
	}
	if disp.Paths["sh"] != "/bin/sh" {
		t.Errorf("sh path=%q", disp.Paths["sh"])
	}
	if disp.ProtocolVersion != protocol.Version {
		t.Errorf("ProtocolVersion=%d", disp.ProtocolVersion)
	}
}

func TestResponder_HeartbeatsEvenWhenIdle(t *testing.T) {
	r, tp := newTestResponder(t, 1, 100*time.Millisecond)
	done, cancel := runInBackground(t, r)
	defer func() { cancel(); <-done }()

	time.Sleep(300 * time.Millisecond)
	status, err := tp.ReadStatus(context.Background(), 1)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if status.State != capability.StateIdle && status.State != capability.StateReady {
		t.Errorf("expected idle/ready when no jobs; got %q", status.State)
	}
	if time.Since(status.LastHeartbeat) > 1*time.Second {
		t.Errorf("heartbeat is stale: %v", status.LastHeartbeat)
	}
	if status.Queued != 0 {
		t.Errorf("Queued=%d, want 0", status.Queued)
	}
}

func TestResponder_StopSentinelTriggersShutdown(t *testing.T) {
	r, tp := newTestResponder(t, 1, 100*time.Millisecond)
	done, cancel := runInBackground(t, r)
	defer cancel()

	if err := tp.SetSentinel(context.Background(), protocol.SentinelSTOP, true); err != nil {
		t.Fatalf("SetSentinel STOP: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned err after STOP: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("responder did not exit on STOP sentinel")
	}
}

func TestResponder_PauseSentinelHaltsDispatch(t *testing.T) {
	r, tp := newTestResponder(t, 1, 100*time.Millisecond)
	done, cancel := runInBackground(t, r)
	defer func() { cancel(); <-done }()

	// Engage PAUSE.
	if err := tp.SetSentinel(context.Background(), protocol.SentinelPAUSE, true); err != nil {
		t.Fatalf("SetSentinel PAUSE: %v", err)
	}
	// Wait for the lane to observe the sentinel.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status, _ := tp.ReadStatus(context.Background(), 1)
		if status.State == capability.StatePaused {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Submit a job while paused; it must NOT execute.
	s := submitJob(t, tp, 1, "paused-job", "echo hi\n")
	time.Sleep(500 * time.Millisecond)
	if _, err := tp.GetResult(context.Background(), 1, s); !errors.Is(err, transport.ErrJobNotFound) {
		t.Errorf("paused responder should not dispatch; GetResult err=%v", err)
	}

	// Remove PAUSE; job should now execute.
	if err := tp.SetSentinel(context.Background(), protocol.SentinelPAUSE, false); err != nil {
		t.Fatalf("SetSentinel PAUSE false: %v", err)
	}
	if _, ok := waitForResult(t, tp, 1, s, 3*time.Second); !ok {
		t.Errorf("job did not execute after resume")
	}
}

func TestResponder_CancelBeforeDispatch(t *testing.T) {
	r, tp := newTestResponder(t, 1, 100*time.Millisecond)
	done, cancel := runInBackground(t, r)
	defer func() { cancel(); <-done }()

	// Pause so the job queues but doesn't execute.
	_ = tp.SetSentinel(context.Background(), protocol.SentinelPAUSE, true)
	time.Sleep(200 * time.Millisecond)

	s := submitJob(t, tp, 1, "cancel-pre", "echo hi\n")
	if err := tp.RequestCancel(context.Background(), 1, s); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}

	// Resume; wait for cancel result.
	_ = tp.SetSentinel(context.Background(), protocol.SentinelPAUSE, false)

	res, ok := waitForResult(t, tp, 1, s, 3*time.Second)
	if !ok {
		t.Fatalf("no result published for pre-dispatch cancel")
	}
	if !res.Cancelled || res.ExitCode != protocol.ExitCodeCancelled {
		t.Errorf("expected cancelled result; got %+v", res)
	}
}

func TestResponder_CancelDuringDispatch(t *testing.T) {
	r, tp := newTestResponder(t, 1, 100*time.Millisecond)
	done, cancel := runInBackground(t, r)
	defer func() { cancel(); <-done }()

	s := submitJob(t, tp, 1, "cancel-mid", "sleep 30\n")

	// Wait for the worker to actually start.
	time.Sleep(500 * time.Millisecond)

	if err := tp.RequestCancel(context.Background(), 1, s); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}

	res, ok := waitForResult(t, tp, 1, s, 5*time.Second)
	if !ok {
		t.Fatalf("no result published for mid-dispatch cancel")
	}
	if !res.Cancelled || res.ExitCode != protocol.ExitCodeCancelled {
		t.Errorf("expected cancelled result; got %+v", res)
	}
}

func TestResponder_TimeoutKillsWorker(t *testing.T) {
	r, tp := newTestResponder(t, 1, 100*time.Millisecond)
	done, cancel := runInBackground(t, r)
	defer func() { cancel(); <-done }()

	// Default timeout is 2s; the script wants 30s.
	s := submitJob(t, tp, 1, "timeout-me", "sleep 30\n")

	res, ok := waitForResult(t, tp, 1, s, 6*time.Second)
	if !ok {
		t.Fatalf("no result for timed-out job")
	}
	if !res.TimedOut || res.ExitCode != protocol.ExitCodeTimeout {
		t.Errorf("expected timeout result; got %+v", res)
	}
}

func TestResponder_StartupClearsStaleStopSentinel(t *testing.T) {
	// Simulate a stale STOP sentinel from an earlier session:
	// create it before constructing the responder. The fresh
	// startup must clear it and run normally.
	r, tp := newTestResponder(t, 1, 100*time.Millisecond)

	if err := tp.SetSentinel(context.Background(), protocol.SentinelSTOP, true); err != nil {
		t.Fatalf("pre-seed STOP: %v", err)
	}

	done, cancel := runInBackground(t, r)
	defer func() { cancel(); <-done }()

	// Give it time to (a) clear STOP and (b) keep running.
	time.Sleep(400 * time.Millisecond)

	// STOP should be gone.
	present, err := tp.CheckSentinel(context.Background(), protocol.SentinelSTOP)
	if err != nil {
		t.Fatal(err)
	}
	if present {
		t.Error("startup should have cleared stale STOP")
	}

	// Responder should still be running and dispatching.
	s := submitJob(t, tp, 1, "alive", "echo alive\n")
	if _, ok := waitForResult(t, tp, 1, s, 5*time.Second); !ok {
		t.Fatal("responder exited despite cleared STOP; job never ran")
	}
}

func TestResponder_StartupPreservesPauseSentinel(t *testing.T) {
	// A PAUSE sentinel carried over from a previous session (or
	// placed intentionally before start) must survive startup;
	// operators may deliberately start paused to inspect config
	// before accepting jobs.
	r, tp := newTestResponder(t, 1, 100*time.Millisecond)

	if err := tp.SetSentinel(context.Background(), protocol.SentinelPAUSE, true); err != nil {
		t.Fatal(err)
	}

	done, cancel := runInBackground(t, r)
	defer func() { cancel(); <-done }()

	// Wait for the lane to observe the sentinel (would be
	// idle-fast if PAUSE were cleared).
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		status, _ := tp.ReadStatus(context.Background(), 1)
		if status.State == capability.StatePaused {
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("responder should have remained paused; PAUSE was wrongly cleared at startup")
}

func TestResponder_FIFOWithinLane(t *testing.T) {
	r, tp := newTestResponder(t, 1, 50*time.Millisecond)
	done, cancel := runInBackground(t, r)
	defer func() { cancel(); <-done }()

	const n = 5
	stems := make([]stem.Stem, n)
	for i := 0; i < n; i++ {
		stems[i] = submitJob(t, tp, 1, "concurrent", "echo one\n")
	}

	for _, s := range stems {
		if _, ok := waitForResult(t, tp, 1, s, 10*time.Second); !ok {
			t.Errorf("result missing for %s", s)
		}
	}
}

// TestResponder_ReloadOnConfigChange verifies that when ConfigPath
// + Reload are wired, a modification to the config file on disk
// triggers a re-publish of dispatch.txt with the reloaded fields.
func TestResponder_ReloadOnConfigChange(t *testing.T) {
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available")
	}
	dir := t.TempDir()
	tp := file.New(dir)
	if err := tp.Prepare(1); err != nil {
		t.Fatal(err)
	}
	disp := subprocess.New(subprocess.Config{
		InterpreterPaths: map[string]string{"sh": "/bin/sh"},
		DefaultTimeout:   time.Second,
	})

	// A dummy config file whose mtime we'll bump to trigger reload.
	cfgPath := filepath.Join(dir, "outpost.toml")
	if err := os.WriteFile(cfgPath, []byte("initial"), 0644); err != nil {
		t.Fatal(err)
	}

	reloadCalls := make(chan struct{}, 4)
	reload := func(ctx context.Context) (responder.ReloadResult, error) {
		reloadCalls <- struct{}{}
		return responder.ReloadResult{
			Description:   "after-reload",
			Tags:          []string{"reloaded"},
			Tools:         map[string]string{"newtool": "1.0"},
			DispatchOrder: []string{"sh", "py"},
			DispatchTable: map[string]string{
				"sh": "/bin/sh",
				"py": "/usr/bin/python3",
			},
		}, nil
	}

	// Watcher cadence is 5 * PollInterval with a 5s floor. Use a
	// generous PollInterval to keep test short — the 5s floor still
	// applies, so we'll need to wait for one watch tick.
	r, err := responder.New(responder.Config{
		Transport:      tp,
		Dispatcher:     disp,
		Events:         events.Discard(),
		ResponderName:  "test-reload",
		LaneCount:      1,
		PollInterval:   50 * time.Millisecond,
		DefaultTimeout: time.Second,
		DispatchTable:  map[string]string{"sh": "/bin/sh"},
		DispatchOrder:  []string{"sh"},
		ConfigPath:     cfgPath,
		Reload:         reload,
	}, 42)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	done, cancel := runInBackground(t, r)

	// Sanity-check the initial dispatch.txt.
	d0, err := tp.ReadDispatch(context.Background())
	if err != nil {
		t.Fatalf("ReadDispatch (pre): %v", err)
	}
	if _, ok := d0.Paths["py"]; ok {
		t.Fatal("py should not be in pre-reload dispatch")
	}

	// Bump the config's mtime to a future time so the watcher
	// notices on its next tick. Going backwards can race with
	// the initial-stat timestamp rounding.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(cfgPath, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// Wait up to one watch cycle (5s floor) + a grace margin for
	// the reload to complete and dispatch.txt to be rewritten.
	select {
	case <-reloadCalls:
		// Reload function was invoked; give publishDispatch a
		// moment to finish.
		time.Sleep(200 * time.Millisecond)
	case <-time.After(7 * time.Second):
		t.Fatal("reload was not invoked within watcher window")
	}

	d1, err := tp.ReadDispatch(context.Background())
	if err != nil {
		t.Fatalf("ReadDispatch (post): %v", err)
	}
	if got := d1.Paths["py"]; got != "/usr/bin/python3" {
		t.Errorf("expected py=/usr/bin/python3 after reload, got %q", got)
	}
	if d1.Description != "after-reload" {
		t.Errorf("description not updated: %q", d1.Description)
	}
	if got := d1.Tools["newtool"]; got != "1.0" {
		t.Errorf("tools not updated: %v", d1.Tools)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("responder Run: %v", err)
	}
}
