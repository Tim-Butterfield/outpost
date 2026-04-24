package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Tim-Butterfield/outpost/internal/config"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/client"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/transport/file"
)

// targetFlags bundles the three flags submitter-side commands use
// to identify which outpost to talk to. Exactly one of Target or
// Dir must be set.
type targetFlags struct {
	Target  string
	Dir     string
	Targets string // override for targets.toml
}

// bindTargetFlags adds the standard submitter-side flags to cmd
// and returns a struct pointer callers dereference in their RunE.
func bindTargetFlags(cmd *cobra.Command) *targetFlags {
	tf := &targetFlags{}
	cmd.Flags().StringVar(&tf.Target, "target", defaultFromEnv("OUTPOST_TARGET"), "named target from targets.toml (mutually exclusive with --dir)")
	cmd.Flags().StringVar(&tf.Dir, "dir", defaultFromEnv("OUTPOST_DIR"), "ad-hoc shared-dir path (mutually exclusive with --target)")
	cmd.Flags().StringVar(&tf.Targets, "targets", defaultFromEnv("OUTPOST_TARGETS"), "override path to targets.toml")
	return tf
}

// defaultFromEnv returns os.Getenv(key); used inline so flag
// defaults read env at flag-definition time.
func defaultFromEnv(key string) string { return os.Getenv(key) }

// resolveTarget implements the --target XOR --dir rule and
// returns a ready-to-use Target. When neither flag is set, falls
// back to targets.toml's `default` (explicit entry or the
// single-target auto-default applied at load time). Only fails
// with "specify --target" when the registry has no default.
func (tf *targetFlags) resolveTarget(ctx context.Context) (*client.Target, error) {
	if tf.Target != "" && tf.Dir != "" {
		return nil, errors.New("--target and --dir are mutually exclusive")
	}
	if tf.Dir != "" {
		return client.NewTarget("ad-hoc", file.New(tf.Dir)), nil
	}
	name := tf.Target
	if name == "" {
		// Consult targets.toml for a default. We need the raw
		// TargetsConfig (not just a Client) to read Default.
		tc, err := config.LoadTargets(tf.Targets)
		if err != nil {
			return nil, fmt.Errorf("load targets.toml: %w", err)
		}
		if tc.Default == "" {
			return nil, errors.New("one of --target or --dir is required (no default set in targets.toml)")
		}
		name = tc.Default
	}
	c, err := client.LoadClientFromFile(tf.Targets)
	if err != nil {
		return nil, fmt.Errorf("load targets.toml: %w", err)
	}
	target, err := c.TargetOrError(name)
	if err != nil {
		return nil, fmt.Errorf("target %q: %w", name, err)
	}
	return target, nil
}

// resolveClient returns a multi-target Client: from targets.toml
// (or its override), optionally augmented with an ad-hoc --dir
// target if Dir is set.
func (tf *targetFlags) resolveClient(ctx context.Context) (*client.Client, error) {
	if tf.Target != "" && tf.Dir != "" {
		return nil, errors.New("--target and --dir are mutually exclusive")
	}
	if tf.Dir != "" {
		// Build a client with just the ad-hoc target so the summary
		// command can probe it using the same code path.
		return client.NewClient(
			client.WithTarget("ad-hoc", file.New(tf.Dir)),
		), nil
	}
	return client.LoadClientFromFile(tf.Targets)
}
