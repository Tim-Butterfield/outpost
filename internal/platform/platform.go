// Package platform provides cross-platform process-tree kill for
// outpost workers.
//
// On Unix, a new session (setsid) is created so the worker and all
// descendants share a process group; SIGKILL to the negative pgid
// takes them all down.
//
// On Windows, the worker is assigned to a Job Object configured
// with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE; terminating the job
// object terminates every process in it. A small race exists
// between Start and Assign where a very fast-forking worker could
// have children that escape the job; workers in this codebase are
// treated as ordinary scripts and this race has not been a problem
// in the v0 bridge over many months of use.
package platform

import (
	"errors"
	"os/exec"
)

// ProcessGroup wraps an exec.Cmd with cross-platform process-tree
// kill semantics. Build the Cmd normally, then pass it to New, then
// use ProcessGroup.Start / Wait / Kill instead of the Cmd's own.
//
// A ProcessGroup is single-use: once Start has been called, a new
// ProcessGroup is required for subsequent runs.
type ProcessGroup struct {
	cmd   *exec.Cmd
	state platformState // platform-specific, defined in platform_{unix,windows}.go
}

// New returns a ProcessGroup wrapping cmd. cmd must not have been
// started yet.
func New(cmd *exec.Cmd) *ProcessGroup {
	return &ProcessGroup{cmd: cmd}
}

// Cmd returns the underlying exec.Cmd for inspection (e.g. reading
// cmd.Process.Pid after Start).
func (pg *ProcessGroup) Cmd() *exec.Cmd { return pg.cmd }

// Start configures process-group isolation and starts the command.
// Returns the error from exec.Cmd.Start or from platform setup.
func (pg *ProcessGroup) Start() error {
	if pg.cmd.Process != nil {
		return errors.New("platform: already started")
	}
	pg.configure() // platform-specific pre-Start setup
	if err := pg.cmd.Start(); err != nil {
		return err
	}
	if err := pg.afterStart(); err != nil {
		// Our post-start setup failed; try to kill the process so we
		// don't leave an orphan.
		_ = pg.cmd.Process.Kill()
		return err
	}
	return nil
}

// Wait waits for the command to exit and returns any wait error.
// It is safe to call Wait after Kill; Wait reports the kill-induced
// exit in that case.
func (pg *ProcessGroup) Wait() error {
	err := pg.cmd.Wait()
	pg.teardown()
	return err
}

// Kill terminates the entire process tree rooted at the wrapped
// command. It is idempotent; calling Kill on an already-exited
// group returns nil.
func (pg *ProcessGroup) Kill() error {
	return pg.kill()
}
