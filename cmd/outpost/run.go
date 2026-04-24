package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Tim-Butterfield/outpost/internal/config"
	"github.com/Tim-Butterfield/outpost/internal/platform"
	"github.com/Tim-Butterfield/outpost/internal/probe"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/dispatcher/subprocess"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/events"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/protocol"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/responder"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/transport/file"
)

// responderParams bundles the knobs runResponder consumes. Built
// either from `outpost run` flags or from `outpost target start`'s
// convention-based resolution.
type responderParams struct {
	Dir            string
	ConfigPath     string
	Name           string
	Lanes          int // 0 means "fall back to config, then default 1"
	PollInterval   time.Duration
	DefaultTimeout time.Duration
	RetainDays     int
}

func newRunCmd() *cobra.Command {
	var (
		dir          string
		pollInterval time.Duration
		defTimeout   time.Duration
		retainDays   int
		name         string
		cfgPath      string
		laneCount    int
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the outpost responder (polls inbox, dispatches jobs)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if dir == "" {
				dir = os.Getenv("OUTPOST_DIR")
			}
			if dir == "" {
				return errors.New("--dir is required")
			}
			// --lanes flag "not set" maps to Lanes=0 so runResponder
			// falls through to the config or the default of 1. When
			// the user did pass --lanes explicitly, use whatever
			// they gave (including 1).
			lanesEffective := 0
			if cmd.Flags().Changed("lanes") {
				lanesEffective = laneCount
			}
			return runResponder(cmd, responderParams{
				Dir:            dir,
				ConfigPath:     cfgPath,
				Name:           name,
				Lanes:          lanesEffective,
				PollInterval:   pollInterval,
				DefaultTimeout: defTimeout,
				RetainDays:     retainDays,
			})
		},
	}

	cmd.Flags().StringVar(&dir, "dir", os.Getenv("OUTPOST_DIR"), "shared directory root (required)")
	cmd.Flags().DurationVar(&pollInterval, "poll", 2*time.Second, "poll / heartbeat cadence")
	cmd.Flags().DurationVar(&defTimeout, "timeout", 60*time.Second, "default per-job timeout")
	cmd.Flags().IntVar(&retainDays, "retain-days", 7, "days to retain log/, outbox/, cancel/ artifacts")
	cmd.Flags().StringVar(&name, "name", os.Getenv("OUTPOST_NAME"), "responder identity advertised in dispatch.txt (falls back to hostname)")
	cmd.Flags().StringVar(&cfgPath, "config", os.Getenv("OUTPOST_CONFIG"), "path to outpost.toml (default: XDG/APPDATA location)")
	cmd.Flags().IntVar(&laneCount, "lanes", 1, "number of lanes to serve")
	return cmd
}

// runResponder is the extracted responder-startup body, shared
// between `outpost run` and `outpost target start`. It loads the
// config, runs the interpreter probe, sets up the transport +
// dispatcher + event sink, builds the responder, and blocks until
// shutdown.
func runResponder(cmd *cobra.Command, p responderParams) error {
	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cfg, err := config.Load(p.ConfigPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Verify the config's [platform] section matches the host we're
	// running on. `outpost target init` records runtime.GOOS/GOARCH
	// into the config, so a mismatch means either the config was
	// init'd on a different kind of host, or we're trying to start
	// a cross-platform target — which would fail at dispatcher-exec
	// time (interpreter paths won't resolve) but with a confusing
	// error. Surfacing it here gives a clear signal up front.
	//
	// Configs predating the platform field (or hand-written configs
	// that omit it) are accepted with no check.
	if err := checkPlatformMatch(cfg); err != nil {
		return err
	}

	resolvedName := resolveResponderName(p.Name, cfg)

	order, paths, err := resolveDispatch(ctx, cfg)
	if err != nil {
		return fmt.Errorf("resolve dispatch: %w", err)
	}

	// Lane count precedence: explicit (p.Lanes > 0) > config > 1.
	effectiveLaneCount := p.Lanes
	if effectiveLaneCount <= 0 {
		if cfg.Responder.LaneCount > 0 {
			effectiveLaneCount = cfg.Responder.LaneCount
		} else {
			effectiveLaneCount = 1
		}
	}

	// Capture the responder's CWD plus a cross-host identifier
	// (UNC on Windows; mount source on Unix) when the CWD sits on
	// a shared mount. Both are advertised in dispatch.txt so
	// submitters can compose relative paths and correlate targets
	// that share a backing store.
	cwd, cwdErr := os.Getwd()
	if cwdErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warn: Getwd failed: %v\n", cwdErr)
		cwd = ""
	}
	cwdSource, cwdSourceErr := platform.ResolveCWDSource(cwd)
	if cwdSourceErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warn: resolve cwd_source: %v\n", cwdSourceErr)
		cwdSource = ""
	}

	// Windows-only: normalize %COMSPEC% so that cmd.exe subshells
	// spawned by .cmd/.bat workers always run stock cmd.exe, even
	// when the operator launched outpost from TCC/4NT. See the
	// detailed rationale in the original run-command comments
	// (preserved in git history).
	var comSpec string
	if runtime.GOOS == "windows" {
		comSpec = os.Getenv("COMSPEC")
		if !isCmdExePath(comSpec) {
			fixed, err := canonicalCmdExePath()
			switch {
			case err != nil:
				fmt.Fprintf(cmd.ErrOrStderr(),
					"warn: COMSPEC=%q is not cmd.exe and the standard cmd.exe could not be located: %v\n"+
						"      pipe-shaped .cmd/.bat jobs may misbehave.\n",
					comSpec, err)
			default:
				old := comSpec
				if setErr := os.Setenv("COMSPEC", fixed); setErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(),
						"warn: failed to set COMSPEC=%q: %v\n", fixed, setErr)
				} else {
					comSpec = fixed
					fmt.Fprintf(cmd.OutOrStderr(),
						"normalized COMSPEC: %q -> %q (outpost standardizes on cmd.exe)\n",
						old, comSpec)
				}
			}
		}
	}

	tp := file.New(p.Dir)
	if err := tp.Prepare(effectiveLaneCount); err != nil {
		// Wrap in responder.ErrTransportUnavailable so supervisors
		// can branch on exit code 74 vs. a generic error.
		return fmt.Errorf("%w: %v", responder.ErrTransportUnavailable, err)
	}

	disp := subprocess.New(subprocess.Config{
		InterpreterPaths: paths,
		DefaultTimeout:   p.DefaultTimeout,
	})

	fileSink, err := events.NewFileLog(filepath.Join(p.Dir, protocol.FileEventLog))
	if err != nil {
		return fmt.Errorf("open event log: %w", err)
	}
	sink := events.MultiSink(fileSink, events.ConsoleSink(cmd.ErrOrStderr()))

	// Resolve ConfigPath to an absolute path so the watcher stats
	// the right file regardless of any chdir elsewhere. Reload
	// re-runs the same pipeline as startup: Load + resolveDispatch.
	// If either fails, the responder keeps its current dispatch
	// table and surfaces a WARN event.
	absConfigPath := ""
	if p.ConfigPath != "" {
		if abs, err := filepath.Abs(p.ConfigPath); err == nil {
			absConfigPath = abs
		} else {
			absConfigPath = p.ConfigPath
		}
	}
	reload := func(rctx context.Context) (responder.ReloadResult, error) {
		if absConfigPath == "" {
			return responder.ReloadResult{}, errors.New("no config path to reload")
		}
		fresh, err := config.Load(absConfigPath)
		if err != nil {
			return responder.ReloadResult{}, fmt.Errorf("load: %w", err)
		}
		newOrder, newPaths, err := resolveDispatch(rctx, fresh)
		if err != nil {
			return responder.ReloadResult{}, fmt.Errorf("resolve dispatch: %w", err)
		}
		return responder.ReloadResult{
			Description:   fresh.Responder.Description,
			Tags:          fresh.Responder.Tags,
			Tools:         fresh.Capabilities.Tools,
			DispatchOrder: newOrder,
			DispatchTable: newPaths,
		}, nil
	}

	r, err := responder.New(responder.Config{
		Transport:      tp,
		Dispatcher:     disp,
		Events:         sink,
		ResponderName:  resolvedName,
		Description:    cfg.Responder.Description,
		Tags:           cfg.Responder.Tags,
		Tools:          cfg.Capabilities.Tools,
		PlatformOS:     runtime.GOOS,
		PlatformArch:   runtime.GOARCH,
		CWD:            cwd,
		CWDSource:      cwdSource,
		ComSpec:        comSpec,
		LaneCount:      effectiveLaneCount,
		PollInterval:   p.PollInterval,
		DefaultTimeout: p.DefaultTimeout,
		RetentionDays:  p.RetainDays,
		DispatchTable:  paths,
		DispatchOrder:  order,
		ConfigPath:     absConfigPath,
		Reload:         reload,
	}, os.Getpid())
	if err != nil {
		return fmt.Errorf("build responder: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStderr(), "outpost %s: serving %s (lane_count=%d, name=%q)\n",
		binaryVersion, p.Dir, effectiveLaneCount, resolvedName)
	if cwd != "" {
		if cwdSource != "" {
			fmt.Fprintf(cmd.OutOrStderr(), "cwd: %s (via %s)\n", cwd, cwdSource)
		} else {
			fmt.Fprintf(cmd.OutOrStderr(), "cwd: %s\n", cwd)
		}
	}
	if comSpec != "" {
		fmt.Fprintf(cmd.OutOrStderr(), "comspec: %s\n", comSpec)
	}

	return r.Run(ctx)
}

// checkPlatformMatch verifies that the loaded config's [platform]
// section agrees with the running host's GOOS/GOARCH. Returns nil
// when either matches, or when the config doesn't declare a
// platform (older or hand-written configs). Returns a descriptive
// error that names both sides of the mismatch and suggests the fix.
func checkPlatformMatch(cfg *config.Config) error {
	if cfg == nil {
		return nil
	}
	if cfg.Platform.OS == "" && cfg.Platform.Arch == "" {
		// No platform declared — pre-platform config; accept.
		return nil
	}
	osMismatch := cfg.Platform.OS != "" && cfg.Platform.OS != runtime.GOOS
	archMismatch := cfg.Platform.Arch != "" && cfg.Platform.Arch != runtime.GOARCH
	if !osMismatch && !archMismatch {
		return nil
	}
	return fmt.Errorf(
		"config platform mismatch: config declares os=%q arch=%q, but this host is os=%q arch=%q\n"+
			"re-init on this host (outpost target init --force) or use the correct config",
		cfg.Platform.OS, cfg.Platform.Arch, runtime.GOOS, runtime.GOARCH,
	)
}

// resolveResponderName applies --name > OUTPOST_NAME > toml > hostname.
// The flag already defaults to OUTPOST_NAME, so `flag` holds the
// first two tiers; this helper picks up the config and hostname
// fallbacks.
func resolveResponderName(flag string, cfg *config.Config) string {
	if flag != "" {
		return flag
	}
	if cfg != nil && cfg.Responder.Name != "" {
		return cfg.Responder.Name
	}
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return ""
}

// resolveDispatch decides which extensions this responder will
// advertise and at what interpreter paths. Config overrides take
// precedence; missing entries fall back to PATH-probed defaults.
func resolveDispatch(ctx context.Context, cfg *config.Config) ([]string, map[string]string, error) {
	// Probe the host to learn what's installed.
	detected := probe.Detect(ctx)

	// Build ext -> first-match-binary-path from the detected list.
	// Walk in table order so the first match per extension wins;
	// the detection table is ordered by operator preference (e.g.
	// bash before sh for .sh).
	detectedByExt := map[string]string{}
	for _, interp := range detected {
		for _, ext := range interp.Extensions {
			if _, seen := detectedByExt[ext]; !seen {
				detectedByExt[ext] = interp.Path
			}
		}
	}

	// Apply config overrides.
	merged := map[string]string{}
	for ext, p := range detectedByExt {
		merged[ext] = p
	}
	if cfg != nil {
		for ext, p := range cfg.Dispatch.Path {
			merged[ext] = p
		}
	}

	// Build the order: config `enabled` list first (if set,
	// filtered to entries we have paths for), else all detected
	// extensions in a stable order.
	var order []string
	if cfg != nil && len(cfg.Dispatch.Enabled) > 0 {
		seen := map[string]bool{}
		for _, ext := range cfg.Dispatch.Enabled {
			if _, ok := merged[ext]; ok && !seen[ext] {
				order = append(order, ext)
				seen[ext] = true
			}
		}
	} else {
		// Preserve detection order to make the default output
		// deterministic across hosts.
		seen := map[string]bool{}
		for _, interp := range detected {
			for _, ext := range interp.Extensions {
				if _, ok := merged[ext]; ok && !seen[ext] {
					order = append(order, ext)
					seen[ext] = true
				}
			}
		}
	}

	// Filter merged down to only the enabled extensions (so
	// dispatch.txt advertises exactly what the operator declared).
	final := map[string]string{}
	for _, ext := range order {
		if p, ok := merged[ext]; ok {
			final[ext] = p
		}
	}
	return order, final, nil
}

// isCmdExePath reports whether a path's basename looks like cmd.exe
// (case-insensitive). Used by the Windows COMSPEC normalization to
// leave well-formed values alone and only override shell-replacement
// binaries like tcc.exe or 4nt.exe.
func isCmdExePath(path string) bool {
	if path == "" {
		return false
	}
	return strings.EqualFold(filepath.Base(path), "cmd.exe")
}

// canonicalCmdExePath returns %SystemRoot%\System32\cmd.exe when
// that file exists (the overwhelming common case), or the bare
// string "cmd.exe" as a fallback so cmd.exe's own subshell search
// picks it up from PATH. Errors only when neither is usable --
// pathological Windows installs we don't try to paper over further.
func canonicalCmdExePath() (string, error) {
	if root := os.Getenv("SystemRoot"); root != "" {
		candidate := filepath.Join(root, "System32", "cmd.exe")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	// Last resort: rely on PATH. cmd.exe forking subshells will
	// search its own PATH for the value we hand it; a bare
	// "cmd.exe" works as long as System32 is on PATH (which it is
	// on every normal Windows install).
	return "cmd.exe", nil
}
