//go:build unix

package client_test

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"testing"
	"time"

	"github.com/Tim-Butterfield/outpost/pkg/outpost"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/capability"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/client"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/dispatcher/subprocess"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/events"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/responder"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/transport/file"
)

// startResponder spins a responder with the given name against a
// tmpdir and returns the shared-dir path plus a teardown func.
func startResponder(t *testing.T, name string) (string, func()) {
	t.Helper()
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available")
	}
	dir := t.TempDir()
	tp := file.New(dir)
	if err := tp.Prepare(1); err != nil {
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
		ResponderName:  name,
		LaneCount:      1,
		PollInterval:   50 * time.Millisecond,
		DefaultTimeout: 2 * time.Second,
		DispatchTable:  map[string]string{"sh": "/bin/sh"},
		DispatchOrder:  []string{"sh"},
	}, 1234)
	if err != nil {
		t.Fatalf("responder.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.Run(ctx); close(done) }()
	time.Sleep(150 * time.Millisecond)
	teardown := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}
	return dir, teardown
}

func TestEndToEnd_SubmitAndWait(t *testing.T) {
	dir, teardown := startResponder(t, "e2e-solo")
	defer teardown()

	c := client.NewClient(
		client.WithTarget("solo", file.New(dir)),
	)
	target := c.Target("solo")

	h, err := target.Submit(context.Background(), outpost.Job{
		Lane:    1,
		Ext:     "sh",
		Content: []byte("echo done\n"),
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := h.WaitWithInterval(ctx, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit=%d, want 0", result.ExitCode)
	}

	// stdout readable.
	rc, err := h.Stdout(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != "done\n" {
		t.Errorf("stdout=%q, want 'done\\n'", data)
	}
}

func TestEndToEnd_ProbeDetectsLiveResponder(t *testing.T) {
	dir, teardown := startResponder(t, "probe-live")
	defer teardown()

	c := client.NewClient(
		client.WithTarget("live", file.New(dir)),
	)
	probe := c.Target("live").Probe(context.Background())
	if !probe.Available {
		t.Errorf("probe should be Available; got %+v", probe)
	}
	if probe.ResponderName != "probe-live" {
		t.Errorf("ResponderName=%q", probe.ResponderName)
	}
	if probe.LaneCount != 1 {
		t.Errorf("LaneCount=%d", probe.LaneCount)
	}
	if len(probe.LaneStates) != 1 {
		t.Errorf("LaneStates len=%d", len(probe.LaneStates))
	}
	if probe.Latency <= 0 {
		t.Error("Latency should be positive")
	}
}

func TestEndToEnd_ProbeMissingTargetIsUnreachable(t *testing.T) {
	// Point at a directory that has no responder running.
	c := client.NewClient(
		client.WithTarget("dead", file.New(t.TempDir())),
	)
	probe := c.Target("dead").Probe(
		context.Background(),
		client.WithTimeout(500*time.Millisecond),
	)
	if probe.Available {
		t.Error("probe should not be Available")
	}
	if probe.Reachable {
		t.Error("probe should not be Reachable")
	}
	if probe.Err == nil {
		t.Error("probe should carry an error")
	}
}

func TestEndToEnd_TargetProbes_DetectsCollision(t *testing.T) {
	// Two responders with the same --name pointing at different
	// shared-dirs. Client.TargetProbes must flag both as collided.
	dir1, t1 := startResponder(t, "shared-identity")
	defer t1()
	dir2, t2 := startResponder(t, "shared-identity")
	defer t2()

	c := client.NewClient(
		client.WithTarget("first", file.New(dir1)),
		client.WithTarget("second", file.New(dir2)),
	)

	probes := c.TargetProbes(
		context.Background(),
		client.WithTimeout(2*time.Second),
	)
	if len(probes) != 2 {
		t.Fatalf("expected 2 probes, got %d", len(probes))
	}
	for _, p := range probes {
		if len(p.CollisionWith) == 0 {
			t.Errorf("%s should be collided; got %+v", p.Name, p)
		}
		if p.Available {
			t.Errorf("%s should not be Available due to collision", p.Name)
		}
		if !errors.Is(p.Err, client.ErrResponderNameCollision) {
			t.Errorf("%s err=%v, want ErrResponderNameCollision", p.Name, p.Err)
		}
	}

	// AvailableTargets should be empty under collision.
	if got := c.AvailableTargets(context.Background()); len(got) != 0 {
		t.Errorf("AvailableTargets under collision: %v, want empty", got)
	}
}

func TestEndToEnd_TargetProbes_NoCollisionWhenNamesDiffer(t *testing.T) {
	dir1, t1 := startResponder(t, "name-a")
	defer t1()
	dir2, t2 := startResponder(t, "name-b")
	defer t2()

	c := client.NewClient(
		client.WithTarget("first", file.New(dir1)),
		client.WithTarget("second", file.New(dir2)),
	)
	probes := c.TargetProbes(context.Background())
	for _, p := range probes {
		if len(p.CollisionWith) != 0 {
			t.Errorf("%s unexpectedly collided: %v", p.Name, p.CollisionWith)
		}
	}
	got := c.AvailableTargets(context.Background())
	if len(got) != 2 {
		t.Errorf("AvailableTargets=%v, want both", got)
	}
}

func TestEndToEnd_EmptyResponderNameDoesNotCollide(t *testing.T) {
	// Two responders both with empty --name. Empty ResponderName is
	// "no identity asserted" and must not flag collisions.
	dir1, t1 := startResponder(t, "")
	defer t1()
	dir2, t2 := startResponder(t, "")
	defer t2()

	c := client.NewClient(
		client.WithTarget("first", file.New(dir1)),
		client.WithTarget("second", file.New(dir2)),
	)
	probes := c.TargetProbes(context.Background())
	for _, p := range probes {
		if len(p.CollisionWith) != 0 {
			t.Errorf("%s: empty name should not collide; got %v", p.Name, p.CollisionWith)
		}
	}
}

func TestEndToEnd_SubmitHandleCancel(t *testing.T) {
	dir, teardown := startResponder(t, "cancel-test")
	defer teardown()

	c := client.NewClient(
		client.WithTarget("x", file.New(dir)),
	)
	h, err := c.Target("x").Submit(context.Background(), outpost.Job{
		Lane:    1,
		Ext:     "sh",
		Content: []byte("sleep 30\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := h.Cancel(context.Background()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := h.WaitWithInterval(ctx, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !result.Cancelled {
		t.Errorf("Cancelled should be true; got %+v", result)
	}
}

func TestEndToEnd_LaneStatus(t *testing.T) {
	dir, teardown := startResponder(t, "status-test")
	defer teardown()

	c := client.NewClient(
		client.WithTarget("x", file.New(dir)),
	)
	status, err := c.Target("x").LaneStatus(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != capability.StateIdle && status.State != capability.StateReady {
		t.Errorf("idle lane state=%q", status.State)
	}
	if status.Lane != 1 {
		t.Errorf("Lane=%d", status.Lane)
	}
}

func TestEndToEnd_Capabilities(t *testing.T) {
	dir, teardown := startResponder(t, "cap-test")
	defer teardown()

	c := client.NewClient(
		client.WithTarget("x", file.New(dir)),
	)
	caps, err := c.Target("x").Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.ResponderName != "cap-test" {
		t.Errorf("ResponderName=%q", caps.ResponderName)
	}
	if _, ok := caps.Paths["sh"]; !ok {
		t.Errorf("expected sh in Paths: %v", caps.Paths)
	}
}
