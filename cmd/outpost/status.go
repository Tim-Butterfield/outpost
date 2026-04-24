package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Tim-Butterfield/outpost/pkg/outpost/capability"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/client"
)

func newStatusCmd() *cobra.Command {
	var (
		jsonOut   bool
		timeout   time.Duration
		filterTag string
		tf        *targetFlags
	)

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show responder status (summary across targets or single-target detail)",
		Long: `status has three modes:

  outpost status                    probe all targets in targets.toml
  outpost status --target <name>    detailed view of one named target
  outpost status --dir <path>       detailed view of one ad-hoc target

Filtering (summary mode only):

  outpost status --tag <tag>        show only targets carrying <tag>

Add --json to emit structured output at any scope.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}

			if tf.Target == "" && tf.Dir == "" {
				// Multi-target summary mode.
				c, err := tf.resolveClient(ctx)
				if err != nil {
					return err
				}
				probes := c.TargetProbes(ctx, client.WithTimeout(timeout))
				if filterTag != "" {
					probes = filterProbesByTag(probes, filterTag)
				}
				return renderProbes(cmd.OutOrStdout(), probes, jsonOut)
			}

			// Single-target detail.
			target, err := tf.resolveTarget(ctx)
			if err != nil {
				return err
			}
			probe := target.Probe(ctx, client.WithTimeout(timeout))
			return renderProbeDetail(cmd.OutOrStdout(), probe, jsonOut)
		},
	}

	tf = bindTargetFlags(cmd)
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Second, "per-target probe timeout")
	cmd.Flags().StringVar(&filterTag, "tag", "", "filter summary to targets carrying this tag")

	return cmd
}

// filterProbesByTag returns the subset of probes whose advertised
// tags include tag. Preserves input order.
func filterProbesByTag(probes []client.TargetProbe, tag string) []client.TargetProbe {
	out := make([]client.TargetProbe, 0, len(probes))
	for _, p := range probes {
		for _, t := range p.Tags {
			if t == tag {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

func renderProbes(w io.Writer, probes []client.TargetProbe, jsonOut bool) error {
	if jsonOut {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(probes)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATUS\tRESPONDER\tPLATFORM\tLANES\tHEARTBEAT\tQUEUED\tTAGS\tNOTE")
	for _, p := range probes {
		status := probeStatusWord(p)
		responder := p.ResponderName
		if responder == "" {
			responder = "-"
		}
		platform := platformWord(p)
		lanes := "-"
		queued := "-"
		heartbeat := "-"
		if p.Reachable && p.LaneCount > 0 {
			lanes = fmt.Sprintf("%d", p.LaneCount)
			total := 0
			for _, q := range p.Queued {
				total += q
			}
			queued = fmt.Sprintf("%d", total)
			if p.ResponderAlive {
				heartbeat = fmt.Sprintf("%s ago", p.Latency.Truncate(time.Millisecond))
			} else {
				heartbeat = "stale"
			}
		}
		tags := "-"
		if len(p.Tags) > 0 {
			tags = strings.Join(p.Tags, ",")
		}
		note := ""
		if len(p.CollisionWith) > 0 {
			note = "collides with " + strings.Join(p.CollisionWith, ", ")
		} else if p.Err != nil {
			note = truncate(p.Err.Error(), 60)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			p.Name, status, responder, platform, lanes, heartbeat, queued, tags, note)
	}
	return tw.Flush()
}

func renderProbeDetail(w io.Writer, p client.TargetProbe, jsonOut bool) error {
	if jsonOut {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(p)
	}
	fmt.Fprintf(w, "target:            %s\n", p.Name)
	fmt.Fprintf(w, "status:            %s\n", probeStatusWord(p))
	if p.ResponderName != "" {
		fmt.Fprintf(w, "responder:         %s\n", p.ResponderName)
	}
	if p.Description != "" {
		fmt.Fprintf(w, "description:       %s\n", p.Description)
	}
	if len(p.Tags) > 0 {
		fmt.Fprintf(w, "tags:              %s\n", strings.Join(p.Tags, ", "))
	}
	if p.PlatformOS != "" || p.PlatformArch != "" {
		platform := p.PlatformOS
		if p.PlatformArch != "" {
			if platform != "" {
				platform += "/"
			}
			platform += p.PlatformArch
		}
		fmt.Fprintf(w, "platform:          %s\n", platform)
	}
	if p.CWD != "" {
		fmt.Fprintf(w, "cwd:               %s\n", p.CWD)
	}
	if p.CWDSource != "" {
		fmt.Fprintf(w, "cwd source:        %s\n", p.CWDSource)
	}
	if p.ComSpec != "" {
		fmt.Fprintf(w, "comspec:           %s\n", p.ComSpec)
	}
	if p.Reachable {
		fmt.Fprintf(w, "protocol version:  %d\n", p.ProtocolVersion)
		fmt.Fprintf(w, "lane count:        %d\n", p.LaneCount)
		for i, st := range p.LaneStates {
			q := 0
			if i < len(p.Queued) {
				q = p.Queued[i]
			}
			fmt.Fprintf(w, "  lane %d:          %s (queued: %d)\n", i+1, st, q)
		}
	}
	fmt.Fprintf(w, "probe latency:     %s\n", p.Latency.Truncate(time.Millisecond))
	if len(p.Tools) > 0 {
		fmt.Fprintf(w, "tools:\n")
		names := make([]string, 0, len(p.Tools))
		for k := range p.Tools {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, name := range names {
			version := p.Tools[name]
			if version == "" {
				version = "-"
			}
			fmt.Fprintf(w, "  %-16s %s\n", name, version)
		}
	}
	if len(p.CollisionWith) > 0 {
		fmt.Fprintf(w, "collision with:    %s\n", strings.Join(p.CollisionWith, ", "))
	}
	if p.Err != nil {
		fmt.Fprintf(w, "error:             %s\n", p.Err)
	}
	return nil
}

// probeStatusWord maps a TargetProbe to the terse status column in
// the summary table.
func probeStatusWord(p client.TargetProbe) string {
	if len(p.CollisionWith) > 0 {
		return "COLLISION"
	}
	if !p.Reachable {
		return "UNREACHABLE"
	}
	if !p.ResponderAlive {
		return "STALE"
	}
	// Look for paused lanes.
	allPaused := true
	for _, s := range p.LaneStates {
		if s != capability.StatePaused {
			allPaused = false
			break
		}
	}
	if p.LaneCount > 0 && allPaused {
		return "PAUSED"
	}
	if p.Available {
		return "AVAILABLE"
	}
	return "UNAVAILABLE"
}

// platformWord condenses a TargetProbe's OS and arch into one
// short table cell ("linux/arm64", "darwin/arm64", "-" when
// unknown).
func platformWord(p client.TargetProbe) string {
	switch {
	case p.PlatformOS != "" && p.PlatformArch != "":
		return p.PlatformOS + "/" + p.PlatformArch
	case p.PlatformOS != "":
		return p.PlatformOS
	case p.PlatformArch != "":
		return p.PlatformArch
	}
	return "-"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// ensure we use os import (silences the linter if we trim further)
var _ = os.Stdout
