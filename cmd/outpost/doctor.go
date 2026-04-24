package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Tim-Butterfield/outpost/internal/config"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/capability"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/client"
)

// docStatus labels a single check. Order is significance ascending:
// ok < warn < fail. Exit code derives from the highest-seen status.
type docStatus int

const (
	docOK docStatus = iota
	docWarn
	docFail
)

// mark returns the glyph used in the doctor output for a status.
// Plain ASCII so console rendering is identical across terminals.
func (s docStatus) mark() string {
	switch s {
	case docOK:
		return "[ok]"
	case docWarn:
		return "[!]"
	case docFail:
		return "[x]"
	}
	return "[?]"
}

// newDoctorCmd returns `outpost doctor` — a diagnostic command that
// correlates environment, targets.toml, per-target init state, and
// per-target runtime state into a single report plus suggested
// next-step commands. Intended to answer "is my outpost setup
// working?" without bouncing between three other commands.
func newDoctorCmd() *cobra.Command {
	var (
		targetsPath string
		rootDir     string
		timeout     time.Duration
	)
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run a setup diagnostic and suggest next steps",
		Long: `doctor inspects the full outpost setup:

  - Which targets.toml is being used, and whether it loads cleanly.
  - Which targets are registered (explicit + scan auto-discovery).
  - Which targets are initialized (outpost.toml present) vs running
    (fresh heartbeat in status.txt).
  - Any scan warnings about skipped folders.

It then prints suggested commands to fix the most common issues
(missing init, target stopped, bad folder name, etc.).

Exit codes:
  0  all checks passed, possibly with warnings
  1  a hard error (can't read targets.toml, nothing registered, etc.)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			return runDoctor(ctx, cmd.OutOrStdout(), doctorConfig{
				targetsPath: targetsPath,
				rootDir:     rootDir,
				timeout:     timeout,
			})
		},
	}
	cmd.Flags().StringVar(&targetsPath, "targets", os.Getenv("OUTPOST_TARGETS"), "override targets.toml path")
	cmd.Flags().StringVar(&rootDir, "root", "", "target root directory (default: $OUTPOST_TARGET_ROOT or ./targets)")
	cmd.Flags().DurationVar(&timeout, "timeout", 3*time.Second, "per-target probe timeout")
	return cmd
}

type doctorConfig struct {
	targetsPath string
	rootDir     string
	timeout     time.Duration
}

// runDoctor is the extracted body of the doctor command. Tests can
// drive it without going through Cobra.
func runDoctor(ctx context.Context, w io.Writer, cfg doctorConfig) error {
	worst := docOK
	record := func(status docStatus, line string) {
		if status > worst {
			worst = status
		}
		fmt.Fprintf(w, "%s %s\n", status.mark(), line)
	}
	var suggestions []string
	suggest := func(s string) { suggestions = append(suggestions, s) }

	fmt.Fprintln(w, "outpost doctor")
	fmt.Fprintln(w)

	// --- binary ---
	record(docOK, fmt.Sprintf("Binary: outpost %s", binaryVersion))

	// Resolve the conventional target root (from --root flag,
	// $OUTPOST_TARGET_ROOT env, or the ./targets default). This is
	// the fallback scan location when targets.toml isn't reachable
	// via a more specific signal (flag/env).
	conventionalRoot, rootErr := resolveTargetRoot(cfg.rootDir)
	if rootErr != nil {
		record(docFail, fmt.Sprintf("Target root: %v", rootErr))
		return exitWithWorst(worst, suggestions, w)
	}

	// Resolve targets.toml path first. When the user points us at a
	// specific file via --targets or $OUTPOST_TARGETS, that file's
	// own directory is the authoritative "target root" -- not the
	// CWD-relative default. Without this reordering, running doctor
	// from the repo root with OUTPOST_TARGETS set elsewhere would
	// spuriously warn "./targets doesn't exist" even though the
	// real target root lives somewhere else entirely.
	tPath, resolvedVia := resolveTargetsPath(cfg.targetsPath, conventionalRoot)
	effectiveRoot := conventionalRoot
	if _, err := os.Stat(tPath); err == nil {
		// targets.toml exists -- use its directory as root.
		effectiveRoot = filepath.Dir(tPath)
	}

	// --- targets root ---
	if _, err := os.Stat(effectiveRoot); os.IsNotExist(err) {
		record(docWarn, fmt.Sprintf("Target root does not exist: %s", effectiveRoot))
		suggest("Initialize target(s): outpost target init <id>")
	} else if err != nil {
		record(docFail, fmt.Sprintf("Target root stat: %v", err))
	} else {
		record(docOK, fmt.Sprintf("Target root: %s", effectiveRoot))
	}

	// --- initialized targets under root ---
	// Scan the effective root so this list reflects what's actually
	// adjacent to targets.toml (matching the scan directive's own
	// "." default).
	initialized := listInitializedTargets(effectiveRoot)
	if len(initialized) > 0 {
		record(docOK, fmt.Sprintf("Initialized targets under root: %s", strings.Join(initialized, ", ")))
	}

	// --- targets.toml path ---
	if _, err := os.Stat(tPath); err != nil {
		record(docWarn, fmt.Sprintf("targets.toml not found at %s (via %s)", tPath, resolvedVia))
		// Suggest in dependency order: you need at least one
		// initialized target before a client registry has anything
		// meaningful to point at. If the target root has no targets
		// yet, lead with target init; otherwise the user's ready
		// for client init.
		if len(initialized) > 0 {
			suggest("Create the client registry: outpost client init")
		} else {
			suggest("Initialize target(s) first: outpost target init <id>")
			suggest("Then create the client registry: outpost client init")
		}
		return exitWithWorst(worst, suggestions, w)
	}
	record(docOK, fmt.Sprintf("targets.toml: %s (via %s)", tPath, resolvedVia))

	// --- load + parse ---
	tc, loadErr := config.LoadTargets(tPath)
	if loadErr != nil {
		record(docFail, fmt.Sprintf("Load targets.toml: %v", loadErr))
		return exitWithWorst(worst, suggestions, w)
	}
	record(docOK, fmt.Sprintf("Registry loads cleanly (%d target(s) registered)", len(tc.Target)))

	// --- scan warnings ---
	for _, warn := range tc.ScanWarnings {
		record(docWarn, warn)
		if strings.Contains(warn, "must match") {
			suggest("Rename the folder to lowercase or add an explicit [target.<name>] block")
		}
	}

	// --- default target ---
	// Single-target registries auto-default at load (so tc.Default
	// is already populated when applicable). We only warn when
	// there are multiple targets and no default -- submitter
	// commands (outpost status, outpost submit) will require
	// --target every time, which is friction worth flagging.
	if tc.Default == "" && len(tc.Target) > 1 {
		record(docWarn, fmt.Sprintf("%d targets registered but no default set", len(tc.Target)))
		suggest("Set a default: outpost client init --force --default <id>  (or edit targets.toml)")
		suggest("Without a default, submitter commands require --target <id>")
	} else if tc.Default != "" {
		record(docOK, fmt.Sprintf("Default target: %s", tc.Default))
	}

	if len(tc.Target) == 0 {
		record(docWarn, "No targets registered")
		suggest("Initialize target(s): outpost target init <id>")
		// Mention the scan coverage — a common cause is that the
		// scan root in targets.toml doesn't point where targets get
		// initialized.
		if len(tc.Scan) > 0 {
			suggest(fmt.Sprintf("targets.toml currently scans: %s — make sure `outpost target init` writes under one of these", strings.Join(tc.Scan, ", ")))
		}
		return exitWithWorst(worst, suggestions, w)
	}

	// --- per-target probe ---
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Targets:")
	c, err := client.LoadClientFromFile(tPath)
	if err != nil {
		record(docFail, fmt.Sprintf("Build client: %v", err))
		return exitWithWorst(worst, suggestions, w)
	}
	probes := c.TargetProbes(ctx, client.WithTimeout(cfg.timeout))

	// Sort by name for stable output.
	sort.Slice(probes, func(i, j int) bool { return probes[i].Name < probes[j].Name })

	for _, p := range probes {
		entry := tc.Target[p.Name]
		configFile := filepath.Join(filepath.Dir(entry.Path), "outpost.toml")
		_, configStatErr := os.Stat(configFile)
		hasConfig := configStatErr == nil

		summary, status := summarizeTargetState(p, hasConfig)
		fmt.Fprintf(w, "  %s %-15s %s\n", status.mark(), p.Name, summary)
		if status > worst {
			worst = status
		}

		// Suggestions per target.
		switch {
		case !hasConfig:
			suggest(fmt.Sprintf("Initialize %s: outpost target init %s", p.Name, p.Name))
		case !p.Reachable:
			suggest(fmt.Sprintf("Start %s: outpost target start %s", p.Name, p.Name))
		case !p.ResponderAlive:
			suggest(fmt.Sprintf("Restart %s (heartbeat stale): outpost target start %s", p.Name, p.Name))
		case len(p.CollisionWith) > 0:
			suggest(fmt.Sprintf("Resolve responder_name collision for %s (with %s)",
				p.Name, strings.Join(p.CollisionWith, ", ")))
		}
	}

	return exitWithWorst(worst, suggestions, w)
}

// summarizeTargetState returns a short human-readable string plus a
// status level for the target. Encodes the state transitions a
// first-time user is likely to hit, worst first:
//
//   - missing outpost.toml     → "NOT INITIALIZED"    (warn)
//   - no dispatch.txt at all   → "NOT STARTED"        (warn)
//   - dispatch.txt but stale   → "STALE"              (warn)
//   - collides with sibling    → "COLLISION ..."      (fail)
//   - everything green         → "AVAILABLE (N lanes)"(ok)
func summarizeTargetState(p client.TargetProbe, hasConfig bool) (string, docStatus) {
	if !hasConfig {
		return "NOT INITIALIZED (outpost.toml missing)", docWarn
	}
	if len(p.CollisionWith) > 0 {
		return fmt.Sprintf("COLLISION with %s", strings.Join(p.CollisionWith, ", ")), docFail
	}
	if !p.Reachable {
		return "NOT STARTED (no dispatch.txt)", docWarn
	}
	if !p.ResponderAlive {
		return "STALE (no fresh heartbeat)", docWarn
	}
	busy := 0
	for _, s := range p.LaneStates {
		if s == capability.StateBusy {
			busy++
		}
	}
	if busy > 0 {
		return fmt.Sprintf("AVAILABLE (%d/%d lanes busy)", busy, p.LaneCount), docOK
	}
	return fmt.Sprintf("AVAILABLE (%d lanes idle)", p.LaneCount), docOK
}

// listInitializedTargets returns the names of folders under root
// that contain an outpost.toml -- i.e., targets that have been
// `target init`'d. Sorted alphabetically. Used by doctor to both
// order first-time-user suggestions sensibly and surface progress
// the user has already made.
func listInitializedTargets(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), "outpost.toml")); err != nil {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// resolveTargetsPath mirrors the submitter-side resolution in flags.go:
// explicit flag > $OUTPOST_TARGETS env > <root>/targets.toml >
// platform default. Returns the chosen path plus a human-readable
// "via <source>" string for the doctor output.
func resolveTargetsPath(flag, root string) (string, string) {
	if flag != "" {
		// flag already absorbed $OUTPOST_TARGETS in the Cobra default;
		// we can't cleanly distinguish here, so report "flag/env".
		return flag, "flag/env"
	}
	rootLocal := filepath.Join(root, "targets.toml")
	if _, err := os.Stat(rootLocal); err == nil {
		return rootLocal, "root/targets.toml"
	}
	if defaultPath, err := config.DefaultTargetsPath(); err == nil {
		return defaultPath, "platform default"
	}
	return rootLocal, "fallback (not found)"
}

// exitWithWorst prints suggestions then returns an error shaped to
// set the process exit code. docOK/docWarn → nil (exit 0);
// docFail → non-nil (exit 1). The main.go mapper turns non-nil
// errors into exit 1 already, so we only need to return a sentinel
// when we want to fail.
func exitWithWorst(worst docStatus, suggestions []string, w io.Writer) error {
	if len(suggestions) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Suggestions:")
		for _, s := range suggestions {
			fmt.Fprintf(w, "  - %s\n", s)
		}
	}
	if worst == docFail {
		// Return a sentinel so Cobra exits with code 1. Nothing more
		// to print -- doctor already showed the failing row.
		return &exitCodeErr{code: 1}
	}
	return nil
}
