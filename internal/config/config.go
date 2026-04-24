// Package config loads and merges outpost's two TOML files:
// `outpost.toml` (responder-side, in this file) and `targets.toml`
// (submitter-side, in targets.go).
//
// The two files are intentionally separate. outpost.toml describes
// the responder's identity and dispatch table on this specific
// host; targets.toml is a submitter's curated list of responders it
// can reach. A single host that both runs a responder and submits
// to others has one of each.
//
// Neither file is required. Reasonable defaults apply when a file
// is absent.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/pelletier/go-toml/v2"
)

// DefaultConfigFileName is the on-disk name for the responder-side
// config file.
const DefaultConfigFileName = "outpost.toml"

// Config is the responder-side configuration loaded from
// outpost.toml. All sections are optional; a zero-value Config is
// valid and uses compiled-in defaults.
type Config struct {
	Responder    ResponderConfig    `toml:"responder"`
	Platform     PlatformConfig     `toml:"platform,omitempty"`
	Capabilities CapabilitiesConfig `toml:"capabilities,omitempty"`
	Dispatch     DispatchConfig     `toml:"dispatch"`
}

// CapabilitiesConfig declares task-level capabilities of the
// responder: the build/dev tools installed on PATH. Populated
// automatically by `outpost setup` from a compiled-in probe table
// (git, make, cmake, compilers, language toolchains, containers,
// platform-specific builders). Operators can edit the map freely
// to add tools outside the probe table or prune detected ones.
type CapabilitiesConfig struct {
	// Tools maps a tool name to the version string reported by
	// that tool at probe time. Absence from the map means the
	// tool was not detected on this host.
	Tools map[string]string `toml:"tools,omitempty"`
}

// PlatformConfig records the responder host's operating system
// and architecture. Populated automatically by `outpost setup`
// from runtime.GOOS / runtime.GOARCH; informational at runtime
// (the responder always re-derives OS/Arch from the current
// runtime when it starts, so editing these fields does not
// misrepresent the host in dispatch.txt).
type PlatformConfig struct {
	// OS is the operating system family: "linux", "darwin",
	// "windows", "freebsd", "openbsd".
	OS string `toml:"os,omitempty"`

	// Arch is the CPU architecture: "amd64", "arm64", etc.
	Arch string `toml:"arch,omitempty"`
}

// ResponderConfig holds identity-and-runtime settings for the
// responder.
//
// Name resolution at responder startup is:
//   1. --name flag
//   2. OUTPOST_NAME env
//   3. this field
//   4. os.Hostname()
//   5. empty (no responder_name advertised)
//
// Description and Tags are pure operator-declared metadata. They
// are not auto-probed or inferred. Submitters use Tags to filter
// targets by task-suitability (e.g., "dotnet-builds", "gpu",
// "production-isolated"); Description is a human-readable sentence
// about what the target is for.
type ResponderConfig struct {
	Name        string   `toml:"name"`
	Description string   `toml:"description,omitempty"`
	Tags        []string `toml:"tags,omitempty"`

	// LaneCount is how many parallel dispatcher lanes the
	// responder should serve. Precedence at startup: --lanes flag
	// > this field > default of 1. Zero or negative means
	// "unset; fall through."
	LaneCount int `toml:"lane_count,omitempty"`
}

// DispatchConfig maps file extensions to interpreter paths and
// declares an optional preferred order. A zero-value DispatchConfig
// means "probe PATH for everything the compiled-in table knows
// about, no operator override."
type DispatchConfig struct {
	// Enabled lists extensions in preferred order. When non-empty,
	// only these extensions are advertised in dispatch.txt (even if
	// other interpreters are detected on PATH).
	Enabled []string `toml:"enabled"`

	// Path is an extension -> absolute-path override map. Missing
	// entries fall through to PATH lookup of the compiled-in
	// default name for that extension.
	Path map[string]string `toml:"path"`

	// Version records the `--version` output captured at the time
	// `outpost setup` was last run, keyed by extension. Not used
	// at dispatch time; present so operators can see at a glance
	// which interpreters were verified to work (real version
	// string) versus which returned nonsense (e.g., the Microsoft
	// Store Python stub's "Python was not found" error). Absent
	// from older configs; `omitempty` keeps backward compatibility.
	Version map[string]string `toml:"version,omitempty"`
}

// Default returns a Config with no overrides: no responder name,
// no enabled list, no path overrides. Callers at startup should
// merge the loaded file (if any) on top of this.
func Default() *Config {
	return &Config{}
}

// Load reads the config file at path (or default location when
// path is empty), returning the parsed Config. A missing file at
// the default location is not an error -- Default() is returned.
// A missing file at an explicit path IS an error.
func Load(path string) (*Config, error) {
	explicit := path != ""
	if !explicit {
		discovered, ok, err := discoverConfigPath()
		if err != nil {
			return nil, err
		}
		if !ok {
			return Default(), nil
		}
		path = discovered
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !explicit {
			return Default(), nil
		}
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	var c Config
	if err := toml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return &c, nil
}

// discoverConfigPath returns the XDG/APPDATA default location for
// outpost.toml and whether the file exists there.
func discoverConfigPath() (string, bool, error) {
	path, err := defaultConfigPath()
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

// defaultConfigPath returns the platform-specific default location
// for outpost.toml. Unix/macOS: $XDG_CONFIG_HOME/outpost/outpost.toml,
// falling back to $HOME/.config/outpost/outpost.toml. Windows:
// %APPDATA%\outpost\outpost.toml.
func defaultConfigPath() (string, error) {
	return defaultFilePath(DefaultConfigFileName)
}

// DefaultConfigPath returns the per-platform default outpost.toml
// location. Exported for CLI callers that want to resolve the
// path without reading the file.
func DefaultConfigPath() (string, error) {
	return defaultConfigPath()
}

// DefaultTargetsPath returns the per-platform default targets.toml
// location. Symmetric with DefaultConfigPath.
func DefaultTargetsPath() (string, error) {
	return defaultFilePath(DefaultTargetsFileName)
}

// defaultFilePath returns the per-platform default path for a file
// in the outpost config directory. Shared between config.go (for
// outpost.toml) and targets.go (for targets.toml).
func defaultFilePath(filename string) (string, error) {
	if runtime.GOOS == "windows" {
		appdata := os.Getenv("APPDATA")
		if appdata == "" {
			return "", errors.New("config: %APPDATA% is not set")
		}
		return filepath.Join(appdata, "outpost", filename), nil
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "outpost", filename), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".config", "outpost", filename), nil
}
