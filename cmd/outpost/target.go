package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Tim-Butterfield/outpost/internal/config"
	"github.com/Tim-Butterfield/outpost/internal/probe"
)

// targetIDRe is the charset rule for target IDs, a counterpart of
// config.targetNameRe kept here so target.go has no upward
// dependency on internal/config.
var targetIDRe = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// defaultTargetID returns the conventional one-per-platform ID
// ("darwin-arm64", "linux-arm64", "windows-arm64", ...) for use
// when the user omits <id> on target init. Matches the GOOS-GOARCH
// slug the release binaries live under (bin/<os>-<arch>/).
func defaultTargetID() string {
	return runtime.GOOS + "-" + runtime.GOARCH
}

// findPlatformMatches returns the folder names under root whose
// outpost.toml [platform] section matches the current host's
// GOOS/GOARCH. Used by `target start` to default the <id> when
// the user omits it — the folder whose config says "I belong to
// this kind of host" is the one we start.
//
// Folder name is an arbitrary operator choice ("macos", "primary",
// "my-mac"); it's the [platform] section inside outpost.toml that
// identifies "what kind of host this target was init'd on," and
// that's what we match.
//
// Unreadable configs are silently skipped — a stale or corrupt
// outpost.toml shouldn't block starting other targets.
func findPlatformMatches(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var matches []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		configPath := filepath.Join(root, e.Name(), "outpost.toml")
		if _, err := os.Stat(configPath); err != nil {
			continue
		}
		cfg, err := config.Load(configPath)
		if err != nil {
			continue
		}
		if cfg.Platform.OS == runtime.GOOS && cfg.Platform.Arch == runtime.GOARCH {
			matches = append(matches, e.Name())
		}
	}
	sort.Strings(matches)
	return matches
}

// resolveStartID decides which target ID `target start` should use
// when the caller didn't pass one positionally. Precedence:
//
//   1. Unique platform match in <root> (folder whose outpost.toml's
//      [platform] == runtime.GOOS/GOARCH) — the common one-target-
//      per-host case.
//   2. Error on multiple matches so the user explicitly picks
//      (multi-VM-same-platform case).
//   3. Fall back to <os>-<arch> naming when nothing matches — lets
//      the failure cascade produce a clear "config not found"
//      error rather than a silent wrong-target pick.
func resolveStartID(rootDir string) (string, error) {
	matches := findPlatformMatches(rootDir)
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return defaultTargetID(), nil
	default:
		return "", fmt.Errorf(
			"multiple targets match this platform (%s/%s): %s\n"+
				"specify one explicitly: outpost target start <id>",
			runtime.GOOS, runtime.GOARCH, strings.Join(matches, ", "))
	}
}

// defaultTargetRoot is where target state (per-target outpost.toml
// and share dir) lives when --root and $OUTPOST_TARGET_ROOT are
// unset. Relative to the invocation CWD.
const defaultTargetRoot = "./targets"

// newTargetCmd assembles `outpost target ...` — the responder-side
// target-management subcommand tree. Replaces the old harness
// scripts (init_target, start_target) with native subcommands so
// we don't have to maintain .sh / .cmd parity.
func newTargetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "target",
		Short: "Manage per-target state and run responders",
		Long: `target is the responder-side counterpart to 'outpost client':
it creates and operates the per-target folders under <root>/ that
each hold an outpost.toml (responder config) and a share/
directory (submitter exchange area).

Target state layout:

  <root>/<id>/
    outpost.toml     responder config (written by 'target init')
    share/           shared-dir served by 'target start'

<root> defaults to ./targets but is configurable via --root or
$OUTPOST_TARGET_ROOT (useful for dev harnesses that sit under
.scratch/).

Subcommands:

  outpost target init  <id>   probe host and write per-target outpost.toml
  outpost target start <id>   run the responder for this target
  outpost target list         list known targets under <root>
  outpost target clean <id>   remove a target's state`,
	}
	cmd.AddCommand(
		newTargetInitCmd(),
		newTargetStartCmd(),
		newTargetListCmd(),
		newTargetCleanCmd(),
	)
	return cmd
}

// --- helpers ---

// resolveTargetRoot applies the flag > env > default precedence and
// returns an absolute path. Relative paths are resolved against the
// invocation CWD.
func resolveTargetRoot(flagValue string) (string, error) {
	root := flagValue
	if root == "" {
		root = os.Getenv("OUTPOST_TARGET_ROOT")
	}
	if root == "" {
		root = defaultTargetRoot
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve --root: %w", err)
	}
	return abs, nil
}

// validateTargetID runs the same charset check as `outpost
// validate-id` but in-process (no subcommand round-trip).
func validateTargetID(id string) error {
	if !targetIDRe.MatchString(id) {
		return fmt.Errorf("target ID %q must match [a-z0-9_-]{1,64} (lowercase letters, digits, hyphen, underscore)", id)
	}
	return nil
}

// --- outpost target init ---

func newTargetInitCmd() *cobra.Command {
	var (
		root  string
		force bool
	)
	cmd := &cobra.Command{
		Use:   "init [id]",
		Short: "Probe host and write a per-target outpost.toml",
		Long: `init creates <root>/<id>/ on this host, probes the installed
interpreters (via the same logic as 'outpost setup'), writes the
result into <root>/<id>/outpost.toml, and ensures <root>/<id>/share/
exists so a submitter can drop the first job into the inbox.

<id> defaults to <os>-<arch> (e.g. darwin-arm64, linux-arm64,
windows-arm64) -- the one-VM-per-platform convention. Pass an
explicit ID when running multiple VMs of the same platform
against the same repo.

Re-run with --force to overwrite the config (e.g., after
installing new interpreters).`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			explicitID := len(args) == 1
			id := defaultTargetID()
			if explicitID {
				id = args[0]
			}
			if err := validateTargetID(id); err != nil {
				return err
			}
			rootDir, err := resolveTargetRoot(root)
			if err != nil {
				return err
			}
			targetDir := filepath.Join(rootDir, id)
			configPath := filepath.Join(targetDir, "outpost.toml")
			shareDir := filepath.Join(targetDir, "share")
			if err := os.MkdirAll(targetDir, 0755); err != nil {
				return fmt.Errorf("mkdir %s: %w", targetDir, err)
			}
			if err := os.MkdirAll(shareDir, 0755); err != nil {
				return fmt.Errorf("mkdir %s: %w", shareDir, err)
			}
			if exists, _ := fileExists(configPath); exists && !force {
				return fmt.Errorf("%s already exists (use --force to overwrite)", configPath)
			}

			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			detected := probe.Detect(ctx)
			// Smoke-execute each detected interpreter: run a trivial
			// script through it and confirm it actually produces the
			// expected stdout. Catches the "responds to --version
			// but can't run real scripts" class of breakage (corrupt
			// installs, stale PATH entries, etc.) that a pure PATH
			// + version probe misses. Interpreters that fail smoke
			// get Working=false and are excluded from the written
			// config just like version-broken interpreters.
			detected = probe.VerifySmoke(ctx, detected)

			fmt.Fprintf(cmd.OutOrStdout(), "=== initializing outpost target: %s ===\n", id)
			fmt.Fprintf(cmd.OutOrStdout(), "config: %s\n", configPath)
			fmt.Fprintf(cmd.OutOrStdout(), "share:  %s\n", shareDir)
			renderDetection(cmd.OutOrStdout(), detected)

			if err := writeConfigFromDetection(configPath, detected); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\nwrote %s\n", configPath)
			// Echo the same invocation shape the user just used: if
			// they took the default, suggest the terse "target start"
			// too. If they were explicit, keep the ID in the
			// suggestion so copy-paste stays unambiguous.
			if explicitID {
				fmt.Fprintf(cmd.OutOrStdout(), "start with: outpost target start %s\n", id)
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "start with: outpost target start")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&root, "root", "", "target root directory (default: $OUTPOST_TARGET_ROOT or ./targets)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite existing outpost.toml")
	return cmd
}

// --- outpost target start ---

func newTargetStartCmd() *cobra.Command {
	var (
		root           string
		lanes          int
		workdir        string
		pollInterval   time.Duration
		defTimeout     time.Duration
		retainDays     int
		nameOverride   string
	)
	cmd := &cobra.Command{
		Use:   "start [id]",
		Short: "Run the responder for a target",
		Long: `start runs the outpost responder for the named target, using
convention-based paths:

  --dir    <root>/<id>/share
  --config <root>/<id>/outpost.toml
  --name   <id> (unless --name is passed)

When <id> is omitted, start scans <root>/ for an outpost.toml
whose [platform] section matches this host's GOOS/GOARCH. The
folder name of the unique match becomes the default ID. If
multiple folders match (two VMs of the same platform sharing the
same root), start errors with the list and asks for an explicit
ID. If zero match, falls back to the <os>-<arch> convention so
the user gets a "config not found" error naming a predictable
folder to create.

Before starting, it chdir's to the nearest git repository root (or
--workdir if given) so workers see a stable, useful CWD -- typically
the root of the repo containing the target, which lets jobs resolve
sibling-repo paths like ../sibling-repo predictably.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rootDir, err := resolveTargetRoot(root)
			if err != nil {
				return err
			}
			var id string
			if len(args) == 1 {
				id = args[0]
			} else {
				id, err = resolveStartID(rootDir)
				if err != nil {
					return err
				}
			}
			if err := validateTargetID(id); err != nil {
				return err
			}
			targetDir := filepath.Join(rootDir, id)
			configPath := filepath.Join(targetDir, "outpost.toml")
			shareDir := filepath.Join(targetDir, "share")
			if _, err := os.Stat(configPath); err != nil {
				return fmt.Errorf("%s not found. Run 'outpost target init %s' first", configPath, id)
			}
			if err := os.MkdirAll(shareDir, 0755); err != nil {
				return fmt.Errorf("ensure share dir: %w", err)
			}

			// Resolve worker CWD. Default: walk up from the target
			// directory looking for a .git folder; falls back to the
			// target root's parent if no git repo is found. Explicit
			// --workdir wins.
			if workdir == "" {
				workdir = detectWorkRoot(targetDir)
			}
			if workdir != "" {
				if err := os.Chdir(workdir); err != nil {
					return fmt.Errorf("chdir %s: %w", workdir, err)
				}
			}

			// Resolve responder name: explicit --name > target ID.
			// Skipping this and letting runResponder fall through to
			// config / hostname would lose the convention that a
			// target's name IS its ID (matching folder, CLI label,
			// and advertised responder_name).
			name := nameOverride
			if name == "" {
				name = id
			}

			return runResponder(cmd, responderParams{
				Dir:            shareDir,
				ConfigPath:     configPath,
				Name:           name,
				Lanes:          lanes,
				PollInterval:   pollInterval,
				DefaultTimeout: defTimeout,
				RetainDays:     retainDays,
			})
		},
	}
	cmd.Flags().StringVar(&root, "root", "", "target root directory (default: $OUTPOST_TARGET_ROOT or ./targets)")
	cmd.Flags().IntVar(&lanes, "lanes", 0, "number of lanes (0 = use config lane_count or default 1)")
	cmd.Flags().StringVar(&workdir, "workdir", "", "working directory for worker processes (default: nearest git root)")
	cmd.Flags().DurationVar(&pollInterval, "poll", 2*time.Second, "poll / heartbeat cadence")
	cmd.Flags().DurationVar(&defTimeout, "timeout", 60*time.Second, "default per-job timeout")
	cmd.Flags().IntVar(&retainDays, "retain-days", 7, "days to retain log/, outbox/, cancel/ artifacts")
	cmd.Flags().StringVar(&nameOverride, "name", "", "override responder_name (default: target ID)")
	return cmd
}

// detectWorkRoot walks up from start looking for a .git directory;
// returns the first ancestor containing one. Returns "" if no .git
// is found before hitting the filesystem root, leaving caller CWD
// unchanged.
func detectWorkRoot(start string) string {
	dir, err := filepath.Abs(start)
	if err != nil {
		return ""
	}
	for {
		if st, err := os.Stat(filepath.Join(dir, ".git")); err == nil && (st.IsDir() || !st.IsDir()) {
			// .git can be a dir (normal checkout) or a file (git
			// worktree pointer). Either counts.
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// --- outpost target list ---

func newTargetListCmd() *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List known targets under <root>",
		RunE: func(cmd *cobra.Command, args []string) error {
			rootDir, err := resolveTargetRoot(root)
			if err != nil {
				return err
			}
			entries, err := os.ReadDir(rootDir)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					fmt.Fprintf(cmd.OutOrStdout(), "no targets under %s (does not exist)\n", rootDir)
					return nil
				}
				return fmt.Errorf("read %s: %w", rootDir, err)
			}
			type row struct {
				name, status string
			}
			var rows []row
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				name := e.Name()
				configPath := filepath.Join(rootDir, name, "outpost.toml")
				dispatchPath := filepath.Join(rootDir, name, "share", "inbox", "dispatch.txt")
				status := "config only"
				if _, err := os.Stat(dispatchPath); err == nil {
					status = "has dispatch.txt"
				}
				if _, err := os.Stat(configPath); err != nil {
					status = "missing outpost.toml"
				}
				rows = append(rows, row{name: name, status: status})
			}
			sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })

			fmt.Fprintf(cmd.OutOrStdout(), "target root: %s\n", rootDir)
			if len(rows) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "  (no targets initialized)")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "  NAME\tSTATUS\tVALID-ID")
			for _, r := range rows {
				validID := "yes"
				if !targetIDRe.MatchString(r.name) {
					validID = "NO — rename to conform"
				}
				fmt.Fprintf(tw, "  %s\t%s\t%s\n", r.name, r.status, validID)
			}
			_ = tw.Flush()
			return nil
		},
	}
	cmd.Flags().StringVar(&root, "root", "", "target root directory (default: $OUTPOST_TARGET_ROOT or ./targets)")
	return cmd
}

// --- outpost target clean ---

func newTargetCleanCmd() *cobra.Command {
	var (
		root    string
		confirm bool
	)
	cmd := &cobra.Command{
		Use:   "clean <id>",
		Short: "Remove a target's on-disk state",
		Long: `clean deletes <root>/<id>/ entirely -- both the outpost.toml and
the share directory (including any pending jobs and log files).
Requires --yes to actually delete; without it, it prints what
would be removed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if err := validateTargetID(id); err != nil {
				return err
			}
			rootDir, err := resolveTargetRoot(root)
			if err != nil {
				return err
			}
			targetDir := filepath.Join(rootDir, id)
			if _, err := os.Stat(targetDir); err != nil {
				return fmt.Errorf("%s: %w", targetDir, err)
			}
			if !confirm {
				fmt.Fprintf(cmd.OutOrStdout(), "would remove: %s\n", targetDir)
				fmt.Fprintln(cmd.OutOrStdout(), "pass --yes to actually delete")
				return nil
			}
			if err := os.RemoveAll(targetDir); err != nil {
				return fmt.Errorf("remove %s: %w", targetDir, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", targetDir)
			return nil
		},
	}
	cmd.Flags().StringVar(&root, "root", "", "target root directory (default: $OUTPOST_TARGET_ROOT or ./targets)")
	cmd.Flags().BoolVar(&confirm, "yes", false, "actually delete (without this, only prints what would be removed)")
	return cmd
}

