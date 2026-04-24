package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadTargets_MissingImplicit(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("APPDATA", filepath.Join(dir, "appdata"))

	tc, err := LoadTargets("")
	if err != nil {
		t.Fatalf("implicit-missing should not error: %v", err)
	}
	if tc == nil {
		t.Fatal("LoadTargets returned nil")
	}
	if len(tc.Target) != 0 {
		t.Errorf("expected empty Target map; got %v", tc.Target)
	}
}

func TestLoadTargets_MissingExplicit(t *testing.T) {
	_, err := LoadTargets(filepath.Join(t.TempDir(), "nope.toml"))
	if err == nil {
		t.Fatal("explicit-missing should return error")
	}
}

func TestLoadTargets_ValidFull(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "targets.toml")
	content := `
default = "vm-tests"

[target.vm-tests]
transport = "file"
path      = "/Volumes/vm-tests/outpost"

[target.build-farm]
transport = "file"
path      = "/Volumes/build-farm/outpost"
expected_name = "vm-win11-b"

[target.airgap]
transport = "file"
path      = "/mnt/usb-drop/outpost"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	tc, err := LoadTargets(path)
	if err != nil {
		t.Fatalf("LoadTargets: %v", err)
	}
	if tc.Default != "vm-tests" {
		t.Errorf("Default=%q, want vm-tests", tc.Default)
	}
	if len(tc.Target) != 3 {
		t.Errorf("got %d targets, want 3", len(tc.Target))
	}
	bf := tc.Target["build-farm"]
	if bf.Transport != "file" || bf.Path != "/Volumes/build-farm/outpost" || bf.ExpectedName != "vm-win11-b" {
		t.Errorf("build-farm entry: %+v", bf)
	}

	names := tc.Names()
	want := []string{"airgap", "build-farm", "vm-tests"}
	if !equalStringSlice(names, want) {
		t.Errorf("Names()=%v, want %v", names, want)
	}
}

func TestLoadTargets_UnknownTransportRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "targets.toml")
	content := `
[target.cloud-api]
transport = "http"
url       = "https://example.com"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadTargets(path)
	if err == nil {
		t.Fatal("non-file transport should fail validation")
	}
	if !strings.Contains(err.Error(), "transport=") {
		t.Errorf("error should name the offending transport: %v", err)
	}
	if !strings.Contains(err.Error(), "cloud-api") {
		t.Errorf("error should name the offending target: %v", err)
	}
}

func TestLoadTargets_FileMissingPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "targets.toml")
	content := `
[target.bad]
transport = "file"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadTargets(path)
	if err == nil {
		t.Fatal("missing path should fail validation")
	}
	if !strings.Contains(err.Error(), "path is required") {
		t.Errorf("error should explain missing path: %v", err)
	}
}

func TestLoadTargets_InvalidNameCharset(t *testing.T) {
	tests := []struct {
		name, bad string
	}{
		{"uppercase", "VM-Tests"},
		{"space", "vm tests"},
		{"dot", "vm.tests"},
		{"empty-via-quoting", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "targets.toml")
			content := `
[target."` + tc.bad + `"]
transport = "file"
path      = "/tmp/x"
`
			if err := os.WriteFile(path, []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadTargets(path); err == nil {
				t.Errorf("expected validation error for name %q", tc.bad)
			}
		})
	}
}

func TestLoadTargets_DefaultMustMatchTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "targets.toml")
	content := `
default = "does-not-exist"

[target.vm-tests]
transport = "file"
path      = "/tmp/outpost"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadTargets(path)
	if err == nil {
		t.Fatal("default pointing at unknown target should fail")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error should name the bad default: %v", err)
	}
}

func TestLoadTargets_DuplicateKeyRejectedByTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "targets.toml")
	content := `
[target.dup]
transport = "file"
path      = "/tmp/a"

[target.dup]
transport = "file"
path      = "/tmp/b"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadTargets(path)
	if err == nil {
		t.Fatal("duplicate [target.*] keys should be a TOML parse error")
	}
}

func TestLoadTargets_Empty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "targets.toml")
	if err := os.WriteFile(path, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	tc, err := LoadTargets(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(tc.Target) != 0 {
		t.Errorf("empty file should yield empty TargetsConfig; got %+v", tc)
	}
	if tc.Default != "" {
		t.Errorf("Default should be empty; got %q", tc.Default)
	}
}

func TestLoadTargets_DiscoverXDG(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "outpost")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	content := `
default = "from-xdg"

[target.from-xdg]
transport = "file"
path      = "/tmp/xdg"
`
	if err := os.WriteFile(filepath.Join(configDir, "targets.toml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("APPDATA", filepath.Join(t.TempDir(), "appdata-unused"))

	tc, err := LoadTargets("")
	if err != nil {
		t.Fatal(err)
	}
	if tc.Default != "from-xdg" {
		t.Errorf("Default=%q, want from-xdg", tc.Default)
	}
	if _, ok := tc.Target["from-xdg"]; !ok {
		t.Errorf("from-xdg target not loaded; targets=%v", tc.Target)
	}
}

// --- scan auto-discovery ---

// makeTargetFolder creates baseDir/<name>/outpost.toml plus an empty
// share/ directory, mirroring what `outpost target init` produces.
// The scan matches on outpost.toml's presence, so that's the file
// that has to exist for discovery to trigger.
func makeTargetFolder(t *testing.T, baseDir, name string) string {
	t.Helper()
	targetDir := filepath.Join(baseDir, name)
	shareDir := filepath.Join(targetDir, "share")
	if err := os.MkdirAll(shareDir, 0755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(targetDir, "outpost.toml")
	if err := os.WriteFile(configPath, []byte("[responder]\nname = ''\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return shareDir
}

func TestLoadTargets_ScanAutoDiscovers(t *testing.T) {
	dir := t.TempDir()
	targetsDir := filepath.Join(dir, "targets")
	_ = makeTargetFolder(t, targetsDir, "linux-arm64")
	_ = makeTargetFolder(t, targetsDir, "win_11")
	_ = makeTargetFolder(t, targetsDir, "win_11_dev")

	path := filepath.Join(dir, "targets.toml")
	if err := os.WriteFile(path, []byte(`scan = ["targets"]`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tc, err := LoadTargets(path)
	if err != nil {
		t.Fatalf("LoadTargets: %v", err)
	}
	for _, want := range []string{"linux-arm64", "win_11", "win_11_dev"} {
		got, ok := tc.Target[want]
		if !ok {
			t.Errorf("target %q not auto-discovered; targets=%v", want, tc.Target)
			continue
		}
		if got.Transport != TransportFile {
			t.Errorf("target %q: transport=%q, want file", want, got.Transport)
		}
		wantPath := filepath.Join(targetsDir, want, "share")
		if got.Path != wantPath {
			t.Errorf("target %q: path=%q, want %q", want, got.Path, wantPath)
		}
	}
}

func TestLoadTargets_ExplicitBeatsScan(t *testing.T) {
	dir := t.TempDir()
	targetsDir := filepath.Join(dir, "targets")
	_ = makeTargetFolder(t, targetsDir, "win_11")

	path := filepath.Join(dir, "targets.toml")
	overridePath := "/custom/override/path"
	content := `scan = ["targets"]
[target.win_11]
transport = "file"
path      = "` + overridePath + `"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	tc, err := LoadTargets(path)
	if err != nil {
		t.Fatalf("LoadTargets: %v", err)
	}
	got := tc.Target["win_11"]
	if got.Path != overridePath {
		t.Errorf("explicit should win: got path=%q, want %q", got.Path, overridePath)
	}
}

func TestLoadTargets_ScanRejectsMixedCaseFolders(t *testing.T) {
	dir := t.TempDir()
	targetsDir := filepath.Join(dir, "targets")
	_ = makeTargetFolder(t, targetsDir, "macOS") // mixed case
	_ = makeTargetFolder(t, targetsDir, "good-one")

	path := filepath.Join(dir, "targets.toml")
	if err := os.WriteFile(path, []byte(`scan = ["targets"]`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tc, err := LoadTargets(path)
	if err != nil {
		t.Fatalf("LoadTargets: %v", err)
	}
	if _, ok := tc.Target["macOS"]; ok {
		t.Error("macOS should be skipped (violates charset)")
	}
	if _, ok := tc.Target["macos"]; ok {
		t.Error("auto-lowercasing should NOT happen; no macos should be registered")
	}
	if _, ok := tc.Target["good-one"]; !ok {
		t.Error("good-one should be auto-discovered")
	}
	foundMacWarn := false
	for _, w := range tc.ScanWarnings {
		if strings.Contains(w, "macOS") {
			foundMacWarn = true
		}
	}
	if !foundMacWarn {
		t.Errorf("warnings should mention macOS skip; got %v", tc.ScanWarnings)
	}
}

func TestLoadTargets_ScanSkipsTrulyInvalidNames(t *testing.T) {
	dir := t.TempDir()
	targetsDir := filepath.Join(dir, "targets")
	_ = makeTargetFolder(t, targetsDir, "has.dots")
	_ = makeTargetFolder(t, targetsDir, "ok-one")

	path := filepath.Join(dir, "targets.toml")
	if err := os.WriteFile(path, []byte(`scan = ["targets"]`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tc, err := LoadTargets(path)
	if err != nil {
		t.Fatalf("LoadTargets: %v", err)
	}
	if _, ok := tc.Target["has.dots"]; ok {
		t.Error("has.dots should be skipped (charset violation)")
	}
	if _, ok := tc.Target["ok-one"]; !ok {
		t.Error("ok-one should be auto-discovered")
	}
}

func TestLoadTargets_ScanIgnoresNonTargetFolders(t *testing.T) {
	dir := t.TempDir()
	targetsDir := filepath.Join(dir, "targets")
	// A real target.
	_ = makeTargetFolder(t, targetsDir, "real")
	// A folder with no share/inbox/dispatch.txt — should be ignored.
	if err := os.MkdirAll(filepath.Join(targetsDir, "not-a-target"), 0755); err != nil {
		t.Fatal(err)
	}
	// A non-directory file in scan root — should be ignored.
	if err := os.WriteFile(filepath.Join(targetsDir, "readme.txt"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "targets.toml")
	if err := os.WriteFile(path, []byte(`scan = ["targets"]`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tc, err := LoadTargets(path)
	if err != nil {
		t.Fatalf("LoadTargets: %v", err)
	}
	if len(tc.Target) != 1 {
		t.Errorf("expected exactly 1 auto-target, got %d: %v", len(tc.Target), tc.Target)
	}
	if _, ok := tc.Target["real"]; !ok {
		t.Error("real target should be discovered")
	}
}

func TestLoadTargets_SingleTargetAutoDefault(t *testing.T) {
	dir := t.TempDir()
	targetsDir := filepath.Join(dir, "targets")
	_ = makeTargetFolder(t, targetsDir, "solo")

	path := filepath.Join(dir, "targets.toml")
	if err := os.WriteFile(path, []byte(`scan = ["targets"]`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tc, err := LoadTargets(path)
	if err != nil {
		t.Fatalf("LoadTargets: %v", err)
	}
	if tc.Default != "solo" {
		t.Errorf("single target should become implicit default; got Default=%q", tc.Default)
	}
}

func TestLoadTargets_MultipleTargetsNoAutoDefault(t *testing.T) {
	dir := t.TempDir()
	targetsDir := filepath.Join(dir, "targets")
	_ = makeTargetFolder(t, targetsDir, "one")
	_ = makeTargetFolder(t, targetsDir, "two")

	path := filepath.Join(dir, "targets.toml")
	if err := os.WriteFile(path, []byte(`scan = ["targets"]`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tc, err := LoadTargets(path)
	if err != nil {
		t.Fatalf("LoadTargets: %v", err)
	}
	if tc.Default != "" {
		t.Errorf("multiple targets should leave Default empty; got %q", tc.Default)
	}
}

func TestLoadTargets_ExplicitDefaultBeatsAuto(t *testing.T) {
	dir := t.TempDir()
	targetsDir := filepath.Join(dir, "targets")
	_ = makeTargetFolder(t, targetsDir, "solo")

	path := filepath.Join(dir, "targets.toml")
	content := `default = "solo"
scan = ["targets"]
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	tc, err := LoadTargets(path)
	if err != nil {
		t.Fatalf("LoadTargets: %v", err)
	}
	if tc.Default != "solo" {
		t.Errorf("explicit default should win; got %q", tc.Default)
	}
}

func TestLoadTargets_ScanMissingDirWarns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "targets.toml")
	if err := os.WriteFile(path, []byte(`scan = ["does-not-exist"]`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tc, err := LoadTargets(path)
	if err != nil {
		t.Fatalf("LoadTargets: %v", err)
	}
	if len(tc.ScanWarnings) == 0 {
		t.Error("expected warning about missing scan dir")
	}
}
