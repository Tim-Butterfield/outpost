//go:build unix

package platform

import (
	"errors"
	"syscall"
)

// platformState is empty on Unix; everything we need is accessible
// through cmd.Process and cmd.SysProcAttr.
type platformState struct{}

// configure arranges for the command to start in a new session,
// giving it a fresh process group whose pgid equals its own pid.
// Descendants inherit the group unless they explicitly break away,
// which is exactly what we want for tree-kill.
func (pg *ProcessGroup) configure() {
	if pg.cmd.SysProcAttr == nil {
		pg.cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	pg.cmd.SysProcAttr.Setsid = true
}

// afterStart is a no-op on Unix -- Setsid handled everything before
// the process started.
func (pg *ProcessGroup) afterStart() error { return nil }

// teardown is a no-op on Unix.
func (pg *ProcessGroup) teardown() {}

// kill sends SIGKILL to the entire process group. A negative pid
// targets the group instead of a single process.
func (pg *ProcessGroup) kill() error {
	if pg.cmd.Process == nil {
		return errors.New("platform: process not started")
	}
	pgid := pg.cmd.Process.Pid
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		// ESRCH ("no such process") means the group is already gone;
		// treat it as success.
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	return nil
}
