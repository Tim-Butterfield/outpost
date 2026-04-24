package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Tim-Butterfield/outpost/pkg/outpost"
)

func newSubmitCmd() *cobra.Command {
	var (
		lane    int
		ext     string
		timeout time.Duration
		noWait  bool
		jsonOut bool
		tf      *targetFlags
	)

	cmd := &cobra.Command{
		Use:            "submit [script-path]",
		Short:          "Submit a job to a target responder",
		// SilenceErrors: Cobra's default "Error: ..." print is
		// suppressed here because submit has a dual-error contract:
		//   * worker exit codes come back as *exitCodeErr and the
		//     exit code IS the signal — no "Error:" line needed
		//   * genuine CLI errors (bad flag, unreachable target,
		//     protocol mismatch, etc.) DO need a message, which
		//     the RunE prints directly to stderr before returning
		SilenceErrors: true,
		SilenceUsage:  true,
		Long: `Submit reads a script from the given path (or stdin when the
argument is "-") and writes it into the target's inbox as a new
job. By default it waits for the responder to publish a result,
then prints stdout/stderr and exits with the worker's exit code.

Target selection:
  --target <name>   look up <name> in targets.toml
  --dir <path>      ad-hoc shared-dir without a registry entry
Exactly one must be specified.

Extension inference:
  If the script-path has an extension, it is used as the dispatch
  extension. Override with --ext for stdin or when the filename
  does not match.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			// Print a user-visible message to stderr for anything
			// that ISN'T a worker exit-code passthrough. Without
			// this, SilenceErrors=true would suppress even the
			// "target not found" / "protocol mismatch" messages a
			// caller needs to debug their invocation.
			defer func() {
				if err == nil {
					return
				}
				var ec *exitCodeErr
				if errors.As(err, &ec) {
					return
				}
				fmt.Fprintln(cmd.ErrOrStderr(), "outpost submit:", err)
			}()

			ctx := cmd.Context()
			path := args[0]

			// Read content.
			content, effectiveExt, err := readScript(path, ext)
			if err != nil {
				return err
			}

			target, err := tf.resolveTarget(ctx)
			if err != nil {
				return err
			}

			job := outpost.Job{
				Lane:    lane,
				Ext:     effectiveExt,
				Content: content,
				Timeout: timeout,
			}
			h, err := target.Submit(ctx, job)
			if err != nil {
				// Don't re-wrap as "submit: ..." — the deferred
				// stderr handler already prefixes with "outpost
				// submit:", and target.Submit's own error text is
				// already specific enough.
				return err
			}

			if noWait {
				fmt.Fprintln(cmd.OutOrStdout(), h.Stem())
				return nil
			}

			result, err := h.WaitWithInterval(ctx, 100*time.Millisecond)
			if err != nil {
				return fmt.Errorf("wait: %w", err)
			}

			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				_ = enc.Encode(result)
				return exitCodeError(result.ExitCode)
			}

			// Relay stdout / stderr to the caller's streams.
			if rc, err := h.Stdout(ctx); err == nil {
				_, _ = io.Copy(cmd.OutOrStdout(), rc)
				rc.Close()
			}
			if rc, err := h.Stderr(ctx); err == nil {
				_, _ = io.Copy(cmd.ErrOrStderr(), rc)
				rc.Close()
			}
			return exitCodeError(result.ExitCode)
		},
	}

	tf = bindTargetFlags(cmd)

	cmd.Flags().IntVar(&lane, "lane", 1, "lane number on the target (must be <= responder's lane_count)")
	cmd.Flags().StringVar(&ext, "ext", "", "dispatch extension (inferred from filename if omitted)")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "per-job timeout override (0 = use responder default)")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "submit and exit; print stem to stdout")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print result as JSON instead of relaying stdout/stderr")

	// Ensure tf is the same pointer used by cmd's flags.
	return cmd
}

// readScript loads content from path (or stdin when "-") and
// resolves the dispatch extension: prefer the explicit override,
// otherwise the filename's extension.
func readScript(path, extOverride string) ([]byte, string, error) {
	var content []byte
	var err error
	if path == "-" {
		content, err = io.ReadAll(os.Stdin)
		if err != nil {
			return nil, "", fmt.Errorf("read stdin: %w", err)
		}
	} else {
		content, err = os.ReadFile(path)
		if err != nil {
			return nil, "", fmt.Errorf("read %s: %w", path, err)
		}
	}
	ext := extOverride
	if ext == "" && path != "-" {
		ext = strings.TrimPrefix(filepath.Ext(path), ".")
	}
	if ext == "" {
		return nil, "", errors.New("cannot infer dispatch extension; use --ext")
	}
	return content, ext, nil
}

// exitCodeError wraps a worker exit code so Cobra's RunE exits
// with the right status. nil for zero, a dedicated error type for
// non-zero.
func exitCodeError(code int) error {
	if code == 0 {
		return nil
	}
	return &exitCodeErr{code: code}
}

type exitCodeErr struct{ code int }

func (e *exitCodeErr) Error() string { return fmt.Sprintf("job exit %d", e.code) }

// Ensure main() surfaces the exit code. Cobra returns err from
// Execute; main.go currently exits 1 on any error. We extend to
// honor the specific code here.
func init() {
	// Install an exit-code handler by wrapping os.Exit via the
	// root command's SilenceErrors pattern. The cleanest way: in
	// main() after Execute, check for *exitCodeErr. We do that in
	// main.go by adjusting the exit logic.
	// (Left as a placeholder for documentation; main.go handles it.)
	_ = context.Background
}
