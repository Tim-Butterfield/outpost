package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/pelletier/go-toml/v2"
)

// DefaultTargetsFileName is the on-disk name for the submitter-side
// target registry file.
const DefaultTargetsFileName = "targets.toml"

// TransportFile is the transport identifier used in targets.toml.
// Only "file" is accepted; any other value fails validation loudly.
const TransportFile = "file"

// targetNameRe constrains target names to the same character set as
// stem labels: lowercase alphanumerics with hyphens and underscores,
// up to 64 characters. Keeps CLI usage predictable across
// case-insensitive filesystems.
var targetNameRe = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// TargetsConfig is the submitter-side target registry. Loaded from
// targets.toml; absent file means "no registered targets, ad-hoc
// --dir submit is still fine."
type TargetsConfig struct {
	// Default names the target used when no --target or --dir flag
	// is provided. Optional. Must match an entry in Target (after
	// Scan auto-discovery has populated it, if applicable).
	Default string `toml:"default"`

	// Scan lists directories to walk at load time, looking for
	// auto-discoverable targets. Each listed directory's immediate
	// children that contain "share/inbox/dispatch.txt" are treated
	// as targets. Relative paths resolve against the directory
	// containing targets.toml.
	//
	// Folder names must match [a-z0-9_-]{1,64} (the same rule as
	// explicit target keys) to be auto-registered; folders that
	// violate the charset are skipped with a warning surfaced
	// through ScanWarnings after load. Explicit [target.xxx]
	// entries always win over an auto-discovered entry of the same
	// name.
	//
	// Empty or absent Scan disables auto-discovery -- registry is
	// whatever is in the explicit [target.xxx] blocks.
	Scan []string `toml:"scan,omitempty"`

	// Target maps each target's name to its transport + settings.
	// Keys are the names used with --target on the CLI.
	Target map[string]TargetEntry `toml:"target"`

	// ScanWarnings records non-fatal issues encountered during
	// auto-discovery (e.g., folders skipped because their name
	// violated the charset rule). Populated by LoadTargets; not
	// part of the TOML schema.
	ScanWarnings []string `toml:"-"`
}

// TargetEntry describes how to reach one specific responder.
type TargetEntry struct {
	// Transport is the transport type. Currently only "file".
	Transport string `toml:"transport"`

	// Path is the shared-dir path on the submitter host. Required
	// when Transport == "file".
	Path string `toml:"path"`

	// ExpectedName, if non-empty, is the responder_name the
	// submitter expects to see advertised in dispatch.txt. Parsed
	// but not currently enforced.
	ExpectedName string `toml:"expected_name"`
}

// LoadTargets reads the registry at path (or default location when
// path is empty). A missing file at the default location is not an
// error -- an empty TargetsConfig is returned. A missing file at an
// explicit path IS an error.
func LoadTargets(path string) (*TargetsConfig, error) {
	explicit := path != ""
	if !explicit {
		discovered, ok, err := discoverTargetsPath()
		if err != nil {
			return nil, err
		}
		if !ok {
			return &TargetsConfig{}, nil
		}
		path = discovered
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !explicit {
			return &TargetsConfig{}, nil
		}
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	var tc TargetsConfig
	if err := toml.Unmarshal(data, &tc); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}

	// Auto-discovery runs before Validate so the discovered entries
	// participate in the same integrity checks as explicit ones.
	// Explicit entries win on name collision -- DiscoverAndMerge
	// skips any name already present in tc.Target.
	baseDir := filepath.Dir(path)
	if err := tc.DiscoverAndMerge(baseDir); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}

	// When the user didn't specify default= and the registry has
	// exactly one target (from any source -- explicit or scanned),
	// that target becomes the implicit default so submitter
	// commands can run without --target. Adding a second target
	// later will leave Default empty again (user must pick).
	tc.ApplySingleTargetDefault()

	if err := tc.Validate(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return &tc, nil
}

// ApplySingleTargetDefault sets tc.Default to the one registered
// target's name when the registry has exactly one target and no
// explicit default. Idempotent -- does nothing if Default is
// already set or the target count is not 1. Exposed so callers
// that construct a TargetsConfig programmatically can opt into the
// same convenience.
func (tc *TargetsConfig) ApplySingleTargetDefault() {
	if tc.Default != "" {
		return
	}
	if len(tc.Target) != 1 {
		return
	}
	for name := range tc.Target {
		tc.Default = name
		return
	}
}

// DiscoverAndMerge walks each path in tc.Scan (resolved against
// baseDir) and registers any child folder that contains
// "share/inbox/dispatch.txt" as an auto-discovered target. Merges
// into tc.Target; explicit entries already present take precedence.
// Folders whose names fail the charset rule are skipped and recorded
// in ScanWarnings.
//
// Exposed so programmatic constructors of TargetsConfig (tests,
// tools) can trigger the same discovery logic without re-reading a
// file.
func (tc *TargetsConfig) DiscoverAndMerge(baseDir string) error {
	if len(tc.Scan) == 0 {
		return nil
	}
	if tc.Target == nil {
		tc.Target = map[string]TargetEntry{}
	}
	for _, scanPath := range tc.Scan {
		abs := scanPath
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(baseDir, scanPath)
		}
		entries, err := os.ReadDir(abs)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				tc.ScanWarnings = append(tc.ScanWarnings,
					fmt.Sprintf("scan path %q does not exist; skipping", scanPath))
				continue
			}
			return fmt.Errorf("scan %q: %w", scanPath, err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			childName := e.Name()
			// Trigger on outpost.toml (configuration artifact written
			// by `outpost target init`) rather than dispatch.txt
			// (runtime artifact written by `outpost target start`).
			// This means a target is discoverable as soon as it's
			// initialized — init order between client and targets
			// becomes irrelevant. Un-started targets show up in
			// status as UNREACHABLE, which is the correct signal.
			configFile := filepath.Join(abs, childName, "outpost.toml")
			if _, err := os.Stat(configFile); err != nil {
				// Not a target folder: no outpost.toml.
				continue
			}
			// Strict match: folder name IS the CLI label. No silent
			// normalization. init_target enforces the same rule on
			// the way in, so a mixed-case folder means either (a) a
			// pre-validation init_target was used or (b) the folder
			// was renamed after init. Either way, surface it.
			if !targetNameRe.MatchString(childName) {
				tc.ScanWarnings = append(tc.ScanWarnings, fmt.Sprintf(
					"scan: skipping folder %q under %q — name must match [a-z0-9_-]{1,64}; rename or add an explicit [target.<name>] entry",
					childName, scanPath))
				continue
			}
			if _, exists := tc.Target[childName]; exists {
				// Explicit entry wins silently — common idiom is to
				// override just the path for one special folder.
				continue
			}
			tc.Target[childName] = TargetEntry{
				Transport: TransportFile,
				Path:      filepath.Join(abs, childName, "share"),
			}
		}
	}
	return nil
}

// Validate applies schema rules to a TargetsConfig. Called
// automatically by LoadTargets; exposed for programmatic
// construction of registries.
func (tc *TargetsConfig) Validate() error {
	for name, entry := range tc.Target {
		if !targetNameRe.MatchString(name) {
			return fmt.Errorf(
				"target %q: name must match [a-z0-9_-]{1,64}", name,
			)
		}
		if entry.Transport == "" {
			return fmt.Errorf("target %q: transport is required", name)
		}
		if entry.Transport != TransportFile {
			return fmt.Errorf(
				"target %q: transport=%q not supported (accepted: %q)",
				name, entry.Transport, TransportFile,
			)
		}
		if entry.Path == "" {
			return fmt.Errorf(
				"target %q: path is required for transport=%q",
				name, TransportFile,
			)
		}
	}
	if tc.Default != "" {
		if _, ok := tc.Target[tc.Default]; !ok {
			return fmt.Errorf(
				"default=%q does not match any declared target",
				tc.Default,
			)
		}
	}
	return nil
}

// Names returns the target names in stable alphabetical order.
// Useful for iterating the registry deterministically in CLI output
// and tests.
func (tc *TargetsConfig) Names() []string {
	names := make([]string, 0, len(tc.Target))
	for name := range tc.Target {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// discoverTargetsPath returns the XDG/APPDATA default location for
// targets.toml and whether the file exists there.
func discoverTargetsPath() (string, bool, error) {
	path, err := defaultFilePath(DefaultTargetsFileName)
	if err != nil {
		return "", false, err
	}
	_, err = os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("config: stat %s: %w", path, err)
	}
	return path, true, nil
}
