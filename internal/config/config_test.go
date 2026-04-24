package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefault_IsEmpty(t *testing.T) {
	c := Default()
	if c == nil {
		t.Fatal("Default returned nil")
	}
	if c.Responder.Name != "" {
		t.Errorf("Responder.Name should be empty; got %q", c.Responder.Name)
	}
	if len(c.Dispatch.Enabled) != 0 {
		t.Errorf("Dispatch.Enabled should be empty; got %v", c.Dispatch.Enabled)
	}
	if len(c.Dispatch.Path) != 0 {
		t.Errorf("Dispatch.Path should be empty; got %v", c.Dispatch.Path)
	}
}

func TestLoad_MissingImplicit(t *testing.T) {
	// No --config flag, and we point XDG at a tmpdir with no file.
	// Expect Default() with no error.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("APPDATA", filepath.Join(dir, "appdata"))

	c, err := Load("")
	if err != nil {
		t.Fatalf("implicit-missing should not error: %v", err)
	}
	if c == nil {
		t.Fatal("Load returned nil config")
	}
	if c.Responder.Name != "" || len(c.Dispatch.Enabled) != 0 {
		t.Errorf("expected default config, got %+v", c)
	}
}

func TestLoad_MissingExplicit(t *testing.T) {
	// Explicit --config path that doesn't exist should error.
	_, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err == nil {
		t.Fatal("explicit-missing should return error")
	}
}

func TestLoad_ValidFull(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "outpost.toml")
	content := `
[responder]
name = "vm-win11-a"

[dispatch]
enabled = ["py", "sh", "btm"]

[dispatch.path]
py  = "C:/Python313/python.exe"
sh  = "C:/Program Files/Git/usr/bin/bash.exe"
btm = "C:/Program Files/JPSoft/TCMD36/tcc.exe"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Responder.Name != "vm-win11-a" {
		t.Errorf("Responder.Name=%q, want vm-win11-a", c.Responder.Name)
	}
	wantEnabled := []string{"py", "sh", "btm"}
	if !equalStringSlice(c.Dispatch.Enabled, wantEnabled) {
		t.Errorf("Enabled=%v, want %v", c.Dispatch.Enabled, wantEnabled)
	}
	if got := c.Dispatch.Path["py"]; got != "C:/Python313/python.exe" {
		t.Errorf("path.py=%q", got)
	}
	if got := c.Dispatch.Path["btm"]; got != "C:/Program Files/JPSoft/TCMD36/tcc.exe" {
		t.Errorf("path.btm=%q", got)
	}
}

func TestLoad_PartialOnlyResponder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "outpost.toml")
	content := `
[responder]
name = "just-name"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Responder.Name != "just-name" {
		t.Errorf("Name=%q", c.Responder.Name)
	}
	if len(c.Dispatch.Enabled) != 0 {
		t.Errorf("Dispatch.Enabled should be empty; got %v", c.Dispatch.Enabled)
	}
}

func TestLoad_MalformedReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "outpost.toml")
	// Missing closing bracket.
	content := `
[responder
name = "bad"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("malformed TOML should return error")
	}
}

func TestLoad_DiscoverDefaultLocation(t *testing.T) {
	// When no explicit path is given, Load() reads from the
	// per-platform default directory: $XDG_CONFIG_HOME/outpost/
	// on Unix/macOS, %APPDATA%\outpost\ on Windows. Setting both
	// env vars to the same tempdir lets this test exercise whichever
	// branch defaultFilePath takes on the current OS.
	dir := t.TempDir()
	configDir := filepath.Join(dir, "outpost")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "outpost.toml"),
		[]byte(`[responder]`+"\n"+`name = "via-default-location"`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("APPDATA", dir)

	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Responder.Name != "via-default-location" {
		t.Errorf("Name=%q, want via-default-location", c.Responder.Name)
	}
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
