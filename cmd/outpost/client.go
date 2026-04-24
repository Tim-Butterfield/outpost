package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Tim-Butterfield/outpost/internal/config"
)

// newClientCmd assembles the `outpost client ...` subcommand tree.
// Mirrors the shape of `outpost setup`: init/where/show/check, with
// --write and --force for the mutating path.
func newClientCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "client",
		Short: "Manage the submitter-side target registry (targets.toml)",
		Long: `client operates on the submitter-side targets.toml -- the
registry of responders this host can reach. Whereas 'outpost setup'
lives on a responder host, 'outpost client' lives on a submitter
host.

Subcommands:

  outpost client init     create a targets.toml with scan auto-discovery
  outpost client where    print the default targets.toml path
  outpost client show     print the stored targets.toml contents
  outpost client check    enumerate what would be discovered by scan`,
	}
	cmd.AddCommand(
		newClientInitCmd(),
		newClientWhereCmd(),
		newClientShowCmd(),
		newClientCheckCmd(),
	)
	return cmd
}

func newClientInitCmd() *cobra.Command {
	var (
		writeTo  string
		root     string
		scanDirs []string
		defName  string
		force    bool
		global   bool
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a targets.toml configured for auto-discovery",
		Long: `init creates a new targets.toml with a scan directive pointing
at the directory where responders write their per-target state.

By default, targets.toml lands at <root>/targets.toml where <root>
is the same directory used by 'outpost target' commands
($OUTPOST_TARGET_ROOT or ./targets). This keeps the client
registry co-located with the target state it describes, so
re-running init_target elsewhere just works.

Flags:
  --root <dir>      target root (default: $OUTPOST_TARGET_ROOT or ./targets)
  --write <path>    override output path (mutually exclusive with --root)
  --global          write to the platform-default targets.toml location
                    ($XDG_CONFIG_HOME/outpost or %APPDATA%\outpost)
  --scan <dir>...   scan root(s) in the file (default: ".")
  --default <name>  set default target name
  --force           overwrite existing file`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Path precedence:
			//   1. --write (explicit file path)
			//   2. --global (platform default)
			//   3. --root/$OUTPOST_TARGET_ROOT/./targets + targets.toml
			var path string
			switch {
			case writeTo != "":
				if global {
					return errors.New("--write and --global are mutually exclusive")
				}
				path = writeTo
			case global:
				p, err := config.DefaultTargetsPath()
				if err != nil {
					return err
				}
				path = p
			default:
				rootDir, err := resolveTargetRoot(root)
				if err != nil {
					return err
				}
				path = filepath.Join(rootDir, "targets.toml")
			}

			if exists, _ := fileExists(path); exists && !force {
				return fmt.Errorf("%s already exists (use --force to overwrite)", path)
			}

			// Default scan root: if writing into a "targets" directory,
			// scan "." so siblings get picked up. Otherwise fall back
			// to the conventional "./targets" relative path.
			scan := scanDirs
			if len(scan) == 0 {
				scan = []string{defaultScanPath(path)}
			}

			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return fmt.Errorf("mkdir: %w", err)
			}
			if err := os.WriteFile(path, renderTargetsFile(scan, defName), 0644); err != nil {
				return fmt.Errorf("write %s: %w", path, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", path)
			fmt.Fprintf(cmd.OutOrStdout(), "\nTo point the outpost CLI at this file:\n")
			fmt.Fprintf(cmd.OutOrStdout(), "  export OUTPOST_TARGETS=%s\n", path)
			return nil
		},
	}
	cmd.Flags().StringVar(&writeTo, "write", "", "override output path")
	cmd.Flags().StringVar(&root, "root", "", "target root directory (default: $OUTPOST_TARGET_ROOT or ./targets)")
	cmd.Flags().BoolVar(&global, "global", false, "write to the platform-default location instead")
	cmd.Flags().StringSliceVar(&scanDirs, "scan", nil, "scan root(s) relative to targets.toml (default: inferred)")
	cmd.Flags().StringVar(&defName, "default", "", "set default target name")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite existing file")
	return cmd
}

func newClientWhereCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "where",
		Short: "Print the default targets.toml path",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := config.DefaultTargetsPath()
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), p)
			return nil
		},
	}
}

func newClientShowCmd() *cobra.Command {
	var writeTo string
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the stored targets.toml contents",
		RunE: func(cmd *cobra.Command, args []string) error {
			path := writeTo
			if path == "" {
				// Fall back to $OUTPOST_TARGETS, then the platform
				// default location. Matches the resolution order used
				// by submitter commands (status/submit).
				path = os.Getenv("OUTPOST_TARGETS")
			}
			if path == "" {
				p, err := config.DefaultTargetsPath()
				if err != nil {
					return err
				}
				path = p
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "targets.toml: %s\n\n", path)
			fmt.Fprint(cmd.OutOrStdout(), string(data))
			if !strings.HasSuffix(string(data), "\n") {
				fmt.Fprintln(cmd.OutOrStdout())
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&writeTo, "path", "", "override targets.toml path")
	return cmd
}

func newClientCheckCmd() *cobra.Command {
	var writeTo string
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Enumerate all targets (explicit + auto-discovered)",
		Long: `check loads targets.toml and prints every registered target,
including ones synthesized by scan auto-discovery. Any warnings
(folders skipped because their names violate the charset rule,
missing scan roots, etc.) are printed after the target table.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := writeTo
			if path == "" {
				path = os.Getenv("OUTPOST_TARGETS")
			}
			tc, err := config.LoadTargets(path)
			if err != nil {
				return err
			}
			names := tc.Names()
			sort.Strings(names)

			fmt.Fprintln(cmd.OutOrStdout(), "Registered targets:")
			if len(names) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "  (none)")
			} else {
				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "  NAME\tTRANSPORT\tPATH")
				for _, n := range names {
					e := tc.Target[n]
					fmt.Fprintf(tw, "  %s\t%s\t%s\n", n, e.Transport, e.Path)
				}
				_ = tw.Flush()
			}
			if tc.Default != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "\ndefault: %s\n", tc.Default)
			}
			if len(tc.ScanWarnings) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "\nWarnings:\n")
				for _, w := range tc.ScanWarnings {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", w)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&writeTo, "path", "", "override targets.toml path")
	return cmd
}

// defaultScanPath picks the initial scan root when the caller didn't
// supply --scan. The heuristic: if the targets.toml itself lives in
// a folder named "targets", set scan=["."] (siblings); otherwise
// scan=["targets"] (the conventional subfolder).
func defaultScanPath(targetsPath string) string {
	parent := filepath.Base(filepath.Dir(targetsPath))
	if parent == "targets" {
		return "."
	}
	return "targets"
}

// renderTargetsFile produces the TOML body of a fresh targets.toml,
// with the scan directive and an optional default line. Keeps the
// output shape stable so users reading the file see a predictable
// layout.
func renderTargetsFile(scan []string, defName string) []byte {
	var buf strings.Builder
	buf.WriteString("# outpost submitter-side target registry.\n")
	buf.WriteString("# Responders under any scanned directory are auto-registered.\n")
	buf.WriteString("# See `outpost client init --help` for options.\n\n")
	if defName != "" {
		fmt.Fprintf(&buf, "default = %q\n", defName)
	}
	buf.WriteString("scan = [")
	for i, s := range scan {
		if i > 0 {
			buf.WriteString(", ")
		}
		fmt.Fprintf(&buf, "%q", s)
	}
	buf.WriteString("]\n")
	buf.WriteString("\n# Explicit entries below override same-named entries from scan.\n")
	buf.WriteString("# [target.custom]\n")
	buf.WriteString("# transport = \"file\"\n")
	buf.WriteString("# path      = \"/absolute/path/to/share\"\n")
	return []byte(buf.String())
}
