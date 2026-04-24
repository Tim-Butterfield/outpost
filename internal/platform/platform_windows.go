//go:build windows

package platform

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// platformState holds Windows-specific Job Object state. The job
// handle owns the worker and any descendants it spawns after being
// assigned to the job.
type platformState struct {
	jobHandle windows.Handle
}

// configure is a no-op on Windows. Job Object setup happens after
// Start so we have a process handle to assign.
func (pg *ProcessGroup) configure() {}

// afterStart creates a Job Object configured to kill all contained
// processes when the handle is closed, then assigns the freshly
// started worker to it.
func (pg *ProcessGroup) afterStart() error {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("platform: CreateJobObject: %w", err)
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		h,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(h)
		return fmt.Errorf("platform: SetInformationJobObject: %w", err)
	}

	procHandle, err := windows.OpenProcess(
		windows.PROCESS_TERMINATE|windows.PROCESS_SET_QUOTA,
		false,
		uint32(pg.cmd.Process.Pid),
	)
	if err != nil {
		_ = windows.CloseHandle(h)
		return fmt.Errorf("platform: OpenProcess(pid=%d): %w", pg.cmd.Process.Pid, err)
	}
	defer windows.CloseHandle(procHandle)

	if err := windows.AssignProcessToJobObject(h, procHandle); err != nil {
		_ = windows.CloseHandle(h)
		return fmt.Errorf("platform: AssignProcessToJobObject: %w", err)
	}

	pg.state.jobHandle = h
	return nil
}

// teardown releases the Job Object handle. Called after Wait
// returns. Closing the handle while KILL_ON_JOB_CLOSE is set kills
// any still-running processes in the job, which is the backstop
// behavior we want.
func (pg *ProcessGroup) teardown() {
	if pg.state.jobHandle != 0 {
		_ = windows.CloseHandle(pg.state.jobHandle)
		pg.state.jobHandle = 0
	}
}

// kill terminates every process in the Job Object via
// TerminateJobObject. Exit code 1 is reported to waiters.
func (pg *ProcessGroup) kill() error {
	if pg.state.jobHandle == 0 {
		// Never started or already torn down; nothing to kill.
		return nil
	}
	if err := windows.TerminateJobObject(pg.state.jobHandle, 1); err != nil {
		return fmt.Errorf("platform: TerminateJobObject: %w", err)
	}
	return nil
}
