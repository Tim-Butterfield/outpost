package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

func newCleanCmd() *cobra.Command {
	var (
		retainDays int
		tf         *targetFlags
	)
	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Force retention sweep of log/ and outbox/",
		Long: `clean removes artifacts in log/, outbox/, and cancel/ older
than --retain-days days. Mirrors the sweep the responder runs at
startup; useful when the responder has been offline for a while
and the operator wants to reclaim space without restarting.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			target, err := tf.resolveTarget(ctx)
			if err != nil {
				return err
			}
			cutoff := time.Now().UTC().Add(-time.Duration(retainDays) * 24 * time.Hour)
			if err := target.Cleanup(ctx, cutoff); err != nil {
				return fmt.Errorf("cleanup: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "cleanup complete (kept <= %d days)\n", retainDays)
			return nil
		},
	}
	tf = bindTargetFlags(cmd)
	cmd.Flags().IntVar(&retainDays, "retain-days", 7, "retain artifacts no older than this many days")
	return cmd
}
