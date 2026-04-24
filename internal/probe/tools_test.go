package probe

import (
	"context"
	"errors"
	"runtime"
	"testing"
)

func TestDetectTools_RealHost(t *testing.T) {
	// Sanity: any CI runner or developer box should have at least
	// one well-known tool. git is the safest bet on any modern
	// system. Skip if even git is missing (minimal containers).
	got := DetectTools(context.Background())
	if len(got) == 0 {
		t.Skip("no detectable tools on this host; probably a minimal environment")
	}
	for _, tl := range got {
		if tl.Name == "" || tl.Path == "" {
			t.Errorf("malformed tool: %+v", tl)
		}
	}
}

func TestDetectTools_Injected(t *testing.T) {
	// Mock PATH containing git + docker; cmake is absent.
	lookPath := func(name string) (string, error) {
		switch name {
		case "git":
			return "/usr/bin/git", nil
		case "docker":
			return "/usr/local/bin/docker", nil
		}
		return "", errors.New("not found")
	}
	runner := func(ctx context.Context, path string, args []string) (string, bool) {
		switch path {
		case "/usr/bin/git":
			return "git version 2.42.0", true
		case "/usr/local/bin/docker":
			return "Docker version 24.0.6, build ed223bc", true
		}
		return "", false
	}

	got := detectTools(context.Background(), lookPath, runner)

	byName := map[string]Tool{}
	for _, tl := range got {
		byName[tl.Name] = tl
	}

	git, ok := byName["git"]
	if !ok {
		t.Fatal("git not detected")
	}
	if git.Path != "/usr/bin/git" || git.Version != "git version 2.42.0" {
		t.Errorf("git: %+v", git)
	}

	docker, ok := byName["docker"]
	if !ok {
		t.Fatal("docker not detected")
	}
	if docker.Version != "Docker version 24.0.6, build ed223bc" {
		t.Errorf("docker version: %q", docker.Version)
	}

	if _, unwanted := byName["cmake"]; unwanted {
		t.Error("cmake should not appear when lookPath says 'not found'")
	}
}

func TestDetectTools_PlatformScoped(t *testing.T) {
	// msbuild is windowsOnly; xcodebuild is darwinOnly. On the
	// current host they should filter correctly even if the mock
	// lookPath would "find" them.
	lookPath := func(name string) (string, error) { return "/stub/" + name, nil }
	runner := func(ctx context.Context, path string, args []string) (string, bool) {
		return "stub", true
	}
	got := detectTools(context.Background(), lookPath, runner)

	byName := map[string]Tool{}
	for _, tl := range got {
		byName[tl.Name] = tl
	}

	if runtime.GOOS != "windows" {
		if _, unwanted := byName["msbuild"]; unwanted {
			t.Error("msbuild must not appear on non-Windows")
		}
	}
	if runtime.GOOS != "darwin" {
		if _, unwanted := byName["xcodebuild"]; unwanted {
			t.Error("xcodebuild must not appear on non-macOS")
		}
		if _, unwanted := byName["swift"]; unwanted {
			t.Error("swift must not appear on non-macOS in this table")
		}
	}
}

func TestDetectTools_ContextCancelShortCircuits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lookPath := func(name string) (string, error) { return "/stub/" + name, nil }
	runner := func(ctx context.Context, path string, args []string) (string, bool) {
		t.Errorf("runner called after ctx cancel (path=%s)", path)
		return "", false
	}
	got := detectTools(ctx, lookPath, runner)
	if len(got) > 1 {
		t.Errorf("cancelled ctx should short-circuit; got %d", len(got))
	}
}
