package platform_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Tim-Butterfield/outpost/internal/platform"
)

// TestMain lets the test binary double as a process-tree-kill
// helper. When invoked with OUTPOST_PLATFORM_TEST_HELPER set, the
// binary acts as either the parent (which spawns a child of itself)
// or the child (which sleeps for a long time). The tree-kill test
// then kills the parent's group and verifies both processes are
// reaped.
func TestMain(m *testing.M) {
	switch os.Getenv("OUTPOST_PLATFORM_TEST_HELPER") {
	case "parent":
		helperParent()
		return
	case "child":
		helperChild()
		return
	}
	os.Exit(m.Run())
}

func helperParent() {
	child := exec.Command(os.Args[0])
	child.Env = append(os.Environ(), "OUTPOST_PLATFORM_TEST_HELPER=child")
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "helperParent start:", err)
		os.Exit(2)
	}
	pidFile := os.Getenv("OUTPOST_PLATFORM_TEST_PIDFILE")
	if pidFile != "" {
		line := fmt.Sprintf("%d %d\n", os.Getpid(), child.Process.Pid)
		if err := os.WriteFile(pidFile, []byte(line), 0644); err != nil {
			fmt.Fprintln(os.Stderr, "helperParent pidfile:", err)
			os.Exit(2)
		}
	}
	// Block until killed.
	_ = child.Wait()
}

func helperChild() {
	// Sleep well past any reasonable test timeout; the test kills us.
	time.Sleep(5 * time.Minute)
}

// TestProcessGroup_StartKillWait verifies the basic start -> kill
// -> wait flow with an ordinary long-running command.
func TestProcessGroup_StartKillWait(t *testing.T) {
	cmd := longRunningCmd()
	pg := platform.New(cmd)
	if err := pg.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Give the child a moment to actually start before we kill it.
	time.Sleep(200 * time.Millisecond)

	if err := pg.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- pg.Wait() }()
	select {
	case <-done:
		// expected
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return within 5s after Kill")
	}
}

// TestProcessGroup_KillBeforeStart confirms Kill on an unstarted
// ProcessGroup does not panic and returns a sensible result.
func TestProcessGroup_KillBeforeStart(t *testing.T) {
	pg := platform.New(longRunningCmd())
	// Kill before Start is defensive: behavior is allowed to be
	// "returns error" or "returns nil"; just must not panic.
	_ = pg.Kill()
}

// TestProcessGroup_KillProcessTree spawns the test binary in helper
// mode so it forks a grandchild, then kills the group and verifies
// both the parent and the grandchild are reaped.
func TestProcessGroup_KillProcessTree(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pids.txt")

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		"OUTPOST_PLATFORM_TEST_HELPER=parent",
		"OUTPOST_PLATFORM_TEST_PIDFILE="+pidFile,
	)

	pg := platform.New(cmd)
	if err := pg.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for the helper parent to record both PIDs.
	_, childPid, ok := waitForPids(t, pidFile, 5*time.Second)
	if !ok {
		// Best-effort cleanup so a broken helper doesn't leak.
		_ = pg.Kill()
		_ = pg.Wait()
		t.Fatalf("helper never wrote PID file %q", pidFile)
	}

	if err := pg.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	// Reap the direct child (our helper parent) so it leaves zombie
	// state and processAlive can't see it anymore. Wait returns when
	// the process is actually gone, not just when signaled.
	done := make(chan error, 1)
	go func() { done <- pg.Wait() }()
	select {
	case <-done:
		// parent reaped
	case <-time.After(5 * time.Second):
		t.Fatal("pg.Wait did not return within 5s after Kill")
	}

	// The grandchild (helper child) was parented by the helper
	// parent; init/launchd adopts and reaps it after SIGKILL from
	// the group propagates. Verify it's gone.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(childPid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("grandchild process (pid=%d) still alive after 5s", childPid)
}

func waitForPids(t *testing.T, path string, timeout time.Duration) (int, int, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			parts := strings.Fields(strings.TrimSpace(string(data)))
			if len(parts) == 2 {
				parent, err1 := strconv.Atoi(parts[0])
				child, err2 := strconv.Atoi(parts[1])
				if err1 == nil && err2 == nil && parent > 0 && child > 0 {
					return parent, child, true
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0, 0, false
}

// longRunningCmd returns a platform-appropriate exec.Cmd that runs
// for well longer than any test should need, so tests can kill it
// and verify the kill path works.
func longRunningCmd() *exec.Cmd {
	if runtime.GOOS == "windows" {
		// ping -n 300 127.0.0.1 -> ~5 minutes.
		return exec.Command("ping", "-n", "300", "127.0.0.1")
	}
	return exec.Command("sleep", "300")
}
