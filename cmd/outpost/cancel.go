package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Tim-Butterfield/outpost/pkg/outpost/stem"
)

// newCancelCmd returns `outpost cancel` -- the CLI surface for the
// per-stem cancel sentinel. Mirrors the pause / stop / resume
// pattern: resolve a target, then poke a sentinel.
//
// Cancel is per-job (unlike STOP / PAUSE which are target-wide),
// so it takes the stem as a positional argument and accepts a
// --lane flag to match the lane the job was submitted to. Lane
// defaults to 1 so the common single-lane case needs no flag.
func newCancelCmd() *cobra.Command {
	var (
		tf   *targetFlags
		lane int
	)
	cmd := &cobra.Command{
		Use:   "cancel <stem>",
		Short: "Request cancellation of an in-flight job (per-stem sentinel)",
		Long: `cancel writes a per-job cancel sentinel the responder observes on
its next poll cycle. Effect depends on timing:

  - Before the lane picks up the job: execution is skipped and the
    result reports ExitCodeCancelled (126).
  - While the worker is running: the responder tree-kills the
    worker and reports ExitCodeCancelled.
  - After the job completed: the sentinel is cleaned up by the
    retention sweep with no effect on the already-written result.

The <stem> is the identifier returned by ` + "`outpost submit --no-wait`" + `
(or logged in the responder's event log). The --lane flag must
match the lane the job was submitted to; defaults to 1.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			target, err := tf.resolveTarget(ctx)
			if err != nil {
				return err
			}
			parsed, err := stem.Parse(args[0])
			if err != nil {
				return fmt.Errorf("invalid stem %q: %w", args[0], err)
			}
			if err := target.RequestCancel(ctx, lane, parsed); err != nil {
				return fmt.Errorf("request cancel: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"cancel requested for %s (target: %s, lane: %d)\n",
				parsed, target.Name(), lane)
			return nil
		},
	}
	tf = bindTargetFlags(cmd)
	cmd.Flags().IntVar(&lane, "lane", 1, "lane the job is on (must match the lane used at submit)")
	return cmd
}
