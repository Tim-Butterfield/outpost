package probe

import (
	"context"
	"os/exec"
	"runtime"
)

// Tool is an installed build / dev tool detected on PATH. Tools
// are distinct from interpreters (probed via a separate table)
// because they describe what kinds of work this target can do --
// "build a .NET app" depends on `dotnet` being installed, not on
// any scripting interpreter. Submitter AIs consume the tools
// advertised in dispatch.txt to pick targets for task-specific
// work (e.g. send a .NET build to a target where `dotnet` is
// present).
type Tool struct {
	// Name matches the binary name on PATH (the lookup key).
	Name string

	// Path is the absolute path the lookup resolved to.
	Path string

	// Version is the trimmed first line of the tool's version
	// output. Empty when the tool has no version probe or the
	// probe failed (tool still appears; Version is just blank).
	Version string
}

// toolTable is the compiled-in catalog of build / dev tools the
// probe looks for. Extending this list is additive: new entries
// simply start appearing on hosts that have them installed.
//
// Keep this list modest. Each entry costs one subprocess spawn
// per `outpost setup` invocation; too many tools makes init slow.
// Tools that are trivially derivable from others (e.g. rustc when
// cargo is present) can be considered redundant.
var toolTable = []candidate{
	// Source control / SCM hosting.
	{name: "git", versionArgs: []string{"--version"}},
	{name: "gh", versionArgs: []string{"--version"}},

	// Build systems.
	{name: "make", versionArgs: []string{"--version"}},
	{name: "cmake", versionArgs: []string{"--version"}},
	{name: "ninja", versionArgs: []string{"--version"}},

	// C / C++ compilers.
	{name: "gcc", versionArgs: []string{"--version"}},
	{name: "clang", versionArgs: []string{"--version"}},

	// Language toolchains (Go, Rust).
	{name: "go", versionArgs: []string{"version"}},
	{name: "cargo", versionArgs: []string{"--version"}},
	{name: "rustc", versionArgs: []string{"--version"}},

	// .NET ecosystem.
	{name: "dotnet", versionArgs: []string{"--version"}},
	{name: "msbuild", versionArgs: []string{"-version"}, windowsOnly: true},

	// Apple ecosystem.
	{name: "xcodebuild", versionArgs: []string{"-version"}, darwinOnly: true},
	{name: "swift", versionArgs: []string{"--version"}, darwinOnly: true},

	// Containers.
	{name: "docker", versionArgs: []string{"--version"}},
	{name: "podman", versionArgs: []string{"--version"}},
}

// DetectTools scans the compiled-in tool table against the
// current host's PATH and returns the tools that are present.
// Order matches the table.
func DetectTools(ctx context.Context) []Tool {
	return detectTools(ctx, exec.LookPath, defaultRunner)
}

// detectTools is the DI seam used by tests.
func detectTools(ctx context.Context, lookPath lookPathFunc, runner versionRunner) []Tool {
	out := make([]Tool, 0, len(toolTable))
	for _, c := range toolTable {
		if c.windowsOnly && runtime.GOOS != "windows" {
			continue
		}
		if c.unixOnly && runtime.GOOS == "windows" {
			continue
		}
		if c.darwinOnly && runtime.GOOS != "darwin" {
			continue
		}
		if ctx.Err() != nil {
			return out
		}
		path, err := lookPath(c.name)
		if err != nil {
			continue
		}
		t := Tool{Name: c.name, Path: path}
		if len(c.versionArgs) > 0 {
			version, working := runner(ctx, path, c.versionArgs)
			// For tools we are less strict than interpreters: we
			// still include the tool if it was found on PATH, even
			// when the version probe was noisy -- the operator
			// cares that the tool is present. Capture whatever we
			// got for diagnostic display.
			_ = working
			t.Version = version
		}
		out = append(out, t)
	}
	return out
}
