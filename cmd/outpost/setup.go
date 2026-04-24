package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/pelletier/go-toml/v2"
	"github.com/spf13/cobra"

	"github.com/Tim-Butterfield/outpost/internal/config"
	"github.com/Tim-Butterfield/outpost/internal/probe"
)

func newSetupCmd() *cobra.Command {
	var (
		check   bool
		where   bool
		show    bool
		diff    bool
		writeTo string
		force   bool
	)
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Probe the host for installed interpreters and manage outpost.toml",
		Long: `setup handles the responder-side outpost.toml:

  outpost setup               probe and write config to the default location
                              (fails if a file is already there; use --force)
  outpost setup --force       overwrite an existing config
  outpost setup --write PATH  write to a specific path

Inspection (no side effects):

  outpost setup --where       print the default config path and exit
  outpost setup --show        print the stored config contents (or exit 1
                              with a notice if no config exists)
  outpost setup --check       probe interpreters and print report; no write
  outpost setup --diff        compare stored config vs. current probe and
                              show which extensions would change

Flags combine where it makes sense (e.g., --show --diff prints both the
current contents and the drift report).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			// Resolve the effective config path once; all flags
			// consult it.
			path := writeTo
			if path == "" {
				defaultPath, err := config.DefaultConfigPath()
				if err != nil {
					return err
				}
				path = defaultPath
			}

			readOnly := where || show || diff || check

			// --where: print the path and exit.
			if where {
				fmt.Fprintln(cmd.OutOrStdout(), path)
				if !show && !diff && !check {
					return nil
				}
			}

			// Load stored config if any read flag needs it.
			var stored *config.Config
			if show || diff {
				exists, err := fileExists(path)
				if err != nil {
					return err
				}
				if !exists {
					// Show a clear notice; diff also reads it, so
					// we treat "no stored config" as "all detected
					// items would be new".
					if show {
						fmt.Fprintf(cmd.ErrOrStderr(),
							"no config found at %s; run 'outpost setup' to create\n", path)
					}
					if !diff {
						return fmt.Errorf("no config at %s", path)
					}
					stored = config.Default()
				} else {
					c, err := config.Load(path)
					if err != nil {
						return fmt.Errorf("load config: %w", err)
					}
					stored = c
				}
			}

			// Probe the host if any flag needs it (including the
			// default write path). Follow the probe with a smoke
			// execution pass so interpreters that pass --version
			// but can't actually run scripts get filtered out of
			// both the report and the written config.
			var detected []probe.Interpreter
			if check || diff || !readOnly {
				detected = probe.Detect(ctx)
				detected = probe.VerifySmoke(ctx, detected)
			}

			// Render outputs in a stable order: --show first, then
			// --check, then --diff. Each is independent so combined
			// flags produce sensible composite output.
			if show && stored != nil {
				if err := renderStored(cmd.OutOrStdout(), path, stored); err != nil {
					return err
				}
			}
			if check {
				renderDetection(cmd.OutOrStdout(), detected)
			}
			if diff {
				renderDiff(cmd.OutOrStdout(), path, stored, detected)
			}

			if readOnly {
				return nil
			}

			// Default: write (the original behavior).
			if exists, _ := fileExists(path); exists && !force {
				return fmt.Errorf("%s already exists (use --force to overwrite)", path)
			}
			renderDetection(cmd.OutOrStdout(), detected)
			if err := writeConfigFromDetection(path, detected); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\nwrote %s\n", path)
			return nil
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "probe and print report; no write")
	cmd.Flags().BoolVar(&where, "where", false, "print the default config path and exit")
	cmd.Flags().BoolVar(&show, "show", false, "print the stored config (exit 1 if missing)")
	cmd.Flags().BoolVar(&diff, "diff", false, "show drift between stored config and current probe")
	cmd.Flags().StringVar(&writeTo, "write", "", "override config path (for read or write)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite existing config file when writing")
	return cmd
}

// fileExists returns (true, nil) if path exists, (false, nil) if it
// doesn't, and (false, err) on any other stat error.
func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// renderStored prints the stored config's raw on-disk bytes so the
// output matches exactly what's in the file (including any
// operator edits). If the file cannot be read for display, falls
// back to re-marshaling the parsed struct.
func renderStored(w io.Writer, path string, stored *config.Config) error {
	fmt.Fprintf(w, "config: %s\n", path)
	data, err := os.ReadFile(path)
	if err == nil {
		fmt.Fprint(w, string(data))
		if !strings.HasSuffix(string(data), "\n") {
			fmt.Fprintln(w)
		}
		return nil
	}
	// Fallback: file disappeared between existence check and read
	// (race), or config was populated programmatically with no
	// on-disk source. Emit the marshaled form.
	marshaled, mErr := toml.Marshal(stored)
	if mErr != nil {
		return fmt.Errorf("render config: %w", mErr)
	}
	fmt.Fprint(w, string(marshaled))
	return nil
}

func renderDetection(w io.Writer, detected []probe.Interpreter) {
	fmt.Fprintln(w, "Interpreters:")
	if len(detected) == 0 {
		fmt.Fprintln(w, "  (none detected on PATH)")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  NAME\tSTATUS\tPATH\tEXTENSIONS\tVERSION")
		for _, i := range detected {
			exts := strings.Join(i.Extensions, ",")
			version := i.Version
			if version == "" {
				version = "-"
			}
			status := "ok"
			if !i.Working {
				status = "BROKEN"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", i.Name, status, i.Path, exts, version)
		}
		_ = tw.Flush()
	}

	// Explicit summary: if any detected interpreters are broken,
	// name them with the reason so the operator sees at a glance
	// what will be excluded from outpost.toml.
	var broken []probe.Interpreter
	for _, i := range detected {
		if !i.Working {
			broken = append(broken, i)
		}
	}
	if len(broken) > 0 {
		fmt.Fprintf(w, "\n  %d interpreter(s) skipped:\n", len(broken))
		for _, i := range broken {
			reason := i.SmokeError
			if reason == "" {
				reason = "no usable version string"
			}
			fmt.Fprintf(w, "    - %s at %s: %s\n", i.Name, i.Path, reason)
		}
	}

	// Tools: the same probe, against the build/dev tool table.
	tools := probe.DetectTools(context.Background())
	fmt.Fprintln(w, "\nTools:")
	if len(tools) == 0 {
		fmt.Fprintln(w, "  (no build/dev tools detected on PATH)")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  NAME\tPATH\tVERSION")
	for _, tl := range tools {
		version := tl.Version
		if version == "" {
			version = "-"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", tl.Name, tl.Path, version)
	}
	_ = tw.Flush()
}

// renderDiff compares the stored dispatch paths and versions
// against a fresh probe, printing two rows per extension (path
// row, version row) to keep the table narrow enough for a
// standard terminal. Status is repeated on the version row so
// the user can scan statuses without alignment tricks.
func renderDiff(w io.Writer, path string, stored *config.Config, detected []probe.Interpreter) {
	fmt.Fprintf(w, "config: %s\n", path)
	rows := buildDiff(stored, detected)
	if len(rows) == 0 {
		fmt.Fprintln(w, "(no differences to show)")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "EXT\tSTATUS\tFIELD\tSTORED\tDETECTED")
	for _, r := range rows {
		sVer := r.StoredVersion
		if sVer == "" {
			sVer = "-"
		}
		dVer := r.DetectedVersion
		if dVer == "" {
			dVer = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\tpath\t%s\t%s\n",
			r.Ext, r.Status, r.StoredPath, r.DetectedPath)
		fmt.Fprintf(tw, "%s\t%s\tversion\t%s\t%s\n",
			r.Ext, r.Status, sVer, dVer)
	}
	_ = tw.Flush()
}

// DiffRow describes how one extension differs between stored
// config and fresh probe. Status values:
//
//   - unchanged:       path and version both match
//   - changed:         path differs (version ignored in this label)
//   - version-changed: path matches but version differs (e.g., in-place
//                      interpreter upgrade)
//   - added:           extension detected now, not in stored config
//   - removed:         extension in stored config, no longer on PATH
type DiffRow struct {
	Ext             string
	Status          string
	StoredPath      string
	StoredVersion   string
	DetectedPath    string
	DetectedVersion string
}

// buildDiff is the pure-logic helper behind renderDiff, exported
// for testing.
func buildDiff(stored *config.Config, detected []probe.Interpreter) []DiffRow {
	storedPaths := map[string]string{}
	storedVersions := map[string]string{}
	if stored != nil {
		for ext, p := range stored.Dispatch.Path {
			storedPaths[ext] = p
		}
		for ext, v := range stored.Dispatch.Version {
			storedVersions[ext] = v
		}
	}

	// First-match-wins detected paths per extension, matching the
	// behavior of writeConfigFromDetection. Skip broken
	// interpreters so the diff reflects what setup would actually
	// write: a broken entry shows up as "removed" (if it was in
	// the stored config) or is absent entirely.
	detectedPaths := map[string]string{}
	detectedVersions := map[string]string{}
	for _, interp := range detected {
		if !interp.Working {
			continue
		}
		for _, ext := range interp.Extensions {
			if _, seen := detectedPaths[ext]; !seen {
				detectedPaths[ext] = interp.Path
				detectedVersions[ext] = interp.Version
			}
		}
	}

	// Union of extensions, sorted for stable output.
	extSet := map[string]struct{}{}
	for ext := range storedPaths {
		extSet[ext] = struct{}{}
	}
	for ext := range detectedPaths {
		extSet[ext] = struct{}{}
	}
	exts := make([]string, 0, len(extSet))
	for ext := range extSet {
		exts = append(exts, ext)
	}
	sort.Strings(exts)

	rows := make([]DiffRow, 0, len(exts))
	for _, ext := range exts {
		sPath, inStored := storedPaths[ext]
		dPath, inDetected := detectedPaths[ext]
		sVer := storedVersions[ext]
		dVer := detectedVersions[ext]
		row := DiffRow{
			Ext:             ext,
			StoredVersion:   sVer,
			DetectedVersion: dVer,
		}
		switch {
		case inStored && inDetected && sPath == dPath && sVer == dVer:
			row.Status = "unchanged"
			row.StoredPath = sPath
			row.DetectedPath = dPath
		case inStored && inDetected && sPath == dPath && sVer != dVer:
			row.Status = "version-changed"
			row.StoredPath = sPath
			row.DetectedPath = dPath
		case inStored && inDetected && sPath != dPath:
			row.Status = "changed"
			row.StoredPath = sPath
			row.DetectedPath = dPath
		case !inStored && inDetected:
			row.Status = "added"
			row.StoredPath = "-"
			row.DetectedPath = dPath
			row.StoredVersion = "-"
		case inStored && !inDetected:
			row.Status = "removed"
			row.StoredPath = sPath
			row.DetectedPath = "(not found on PATH)"
			row.DetectedVersion = "-"
		}
		rows = append(rows, row)
	}
	return rows
}

// writeConfigFromDetection renders a minimal outpost.toml using
// the detected interpreters, one entry per extension (first match
// wins in detection order). Operators edit the file afterward to
// reorder or remove entries.
func writeConfigFromDetection(path string, detected []probe.Interpreter) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	extToPath := map[string]string{}
	extToVersion := map[string]string{}
	var enabled []string
	for _, interp := range detected {
		// Broken interpreters -- version probe rejected (e.g., the
		// Microsoft Store Python stub) -- do not go into the
		// dispatch table. They are still visible in the detection
		// report with STATUS=BROKEN.
		if !interp.Working {
			continue
		}
		for _, ext := range interp.Extensions {
			if _, seen := extToPath[ext]; seen {
				continue
			}
			extToPath[ext] = interp.Path
			if interp.Version != "" {
				extToVersion[ext] = interp.Version
			}
			enabled = append(enabled, ext)
		}
	}
	sort.Strings(enabled)

	// Auto-probe build / dev tools too. These inform task-level
	// target routing (e.g. "this host has `dotnet` so it can
	// build .NET apps").
	toolMap := map[string]string{}
	for _, tl := range probe.DetectTools(context.Background()) {
		toolMap[tl.Name] = tl.Version
	}

	cfg := config.Config{
		Platform: config.PlatformConfig{
			OS:   runtime.GOOS,
			Arch: runtime.GOARCH,
		},
		Capabilities: config.CapabilitiesConfig{
			Tools: toolMap,
		},
		Dispatch: config.DispatchConfig{
			Enabled: enabled,
			Path:    extToPath,
			Version: extToVersion,
		},
	}
	data, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}
