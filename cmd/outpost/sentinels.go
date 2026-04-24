package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Tim-Butterfield/outpost/pkg/outpost/protocol"
)

// Shared implementation for stop / pause / resume: resolve the
// target and write-or-remove the named sentinel.
func setSentinelCmd(use, short, sentinelName string, present bool) *cobra.Command {
	var tf *targetFlags
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			target, err := tf.resolveTarget(ctx)
			if err != nil {
				return err
			}
			if err := target.SetSentinel(ctx, sentinelName, present); err != nil {
				return fmt.Errorf("set %s sentinel: %w", sentinelName, err)
			}
			verb := "created"
			if !present {
				verb = "removed"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s sentinel %s (target: %s)\n", verb, sentinelName, target.Name())
			return nil
		},
	}
	tf = bindTargetFlags(cmd)
	return cmd
}

func newStopCmd() *cobra.Command {
	return setSentinelCmd(
		"stop",
		"Signal a responder to exit cleanly (STOP sentinel)",
		protocol.SentinelSTOP, true,
	)
}

func newPauseCmd() *cobra.Command {
	return setSentinelCmd(
		"pause",
		"Pause job dispatch on a responder (PAUSE sentinel)",
		protocol.SentinelPAUSE, true,
	)
}

func newResumeCmd() *cobra.Command {
	return setSentinelCmd(
		"resume",
		"Resume job dispatch on a paused responder",
		protocol.SentinelPAUSE, false,
	)
}
