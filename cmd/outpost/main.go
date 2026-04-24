// Package main is the outpost CLI entry point.
//
// Each subcommand lives in its own file in this package. main.go
// assembles the Cobra command tree and hands control to
// cmd.Execute. Real subcommand bodies consume pkg/outpost/... --
// the binary dogfoods the library.
package main

import (
	"errors"
	"os"

	"github.com/spf13/cobra"

	"github.com/Tim-Butterfield/outpost/pkg/outpost/responder"
)

// Well-known process exit codes outpost emits beyond the usual
// 0 (success) and 1 (generic error). Documented here so
// supervisor scripts can branch on them (see README "Deploying
// outpost as a service").
const (
	// ExitTransportUnavailable — typically "shared dir not yet
	// mounted." Supervisor should back off and retry rather than
	// exit.
	ExitTransportUnavailable = 74

	// ExitRestartRequested — responder observed the RESTART
	// sentinel and exited cleanly. Supervisor should re-exec.
	ExitRestartRequested = 75
)

// binaryVersion is set via `-ldflags "-X main.binaryVersion=..."`
// at release time.
var binaryVersion = "0.0.0-dev"

func main() {
	err := newRootCmd().Execute()
	os.Exit(exitCodeFromError(err))
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "outpost",
		Short: "File-RPC bridge for remote AI execution",
		Long: `outpost lets an AI agent execute work on hosts where it cannot
natively run, communicating through a shared directory (SMB, NFS,
Syncthing, Dropbox, iCloud, or any other file-sync transport).

No listening daemon on the target. Nothing on a port.

See https://github.com/Tim-Butterfield/outpost for documentation.`,
		SilenceUsage: true,
	}
	cmd.AddCommand(
		newRunCmd(),
		newSubmitCmd(),
		newStatusCmd(),
		newStopCmd(),
		newPauseCmd(),
		newResumeCmd(),
		newCancelCmd(),
		newCleanCmd(),
		newSetupCmd(),
		newTargetCmd(),
		newClientCmd(),
		newDoctorCmd(),
		newVersionCmd(),
	)
	return cmd
}

// exitCodeFromError converts a command error into the process
// exit code.
//
//   - nil → 0
//   - *exitCodeErr → its embedded code (used by `outpost submit` to
//     propagate the worker's own exit)
//   - wrapping responder.ErrRestart → 75 (supervisor re-exec)
//   - wrapping responder.ErrTransportUnavailable → 74 (supervisor
//     backs off and retries)
//   - anything else → 1
//
// Keeping these codes stable is part of outpost's wire contract
// with external supervisors.
func exitCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	var ec *exitCodeErr
	if errors.As(err, &ec) {
		return ec.code
	}
	if errors.Is(err, responder.ErrRestart) {
		return ExitRestartRequested
	}
	if errors.Is(err, responder.ErrTransportUnavailable) {
		return ExitTransportUnavailable
	}
	return 1
}
