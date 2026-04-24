package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Tim-Butterfield/outpost/pkg/outpost/protocol"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print outpost binary and protocol version",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "outpost %s (protocol v%d)\n", binaryVersion, protocol.Version)
			return nil
		},
	}
}
