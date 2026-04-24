package main

import (
	"runtime"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/outpost/internal/config"
)

func TestCheckPlatformMatch_Match(t *testing.T) {
	cfg := &config.Config{Platform: config.PlatformConfig{
		OS:   runtime.GOOS,
		Arch: runtime.GOARCH,
	}}
	if err := checkPlatformMatch(cfg); err != nil {
		t.Errorf("matching platform should be accepted, got: %v", err)
	}
}

func TestCheckPlatformMatch_NoPlatformDeclared(t *testing.T) {
	cfg := &config.Config{} // empty [platform]
	if err := checkPlatformMatch(cfg); err != nil {
		t.Errorf("missing platform should be accepted (pre-platform config): %v", err)
	}
}

func TestCheckPlatformMatch_OSMismatch(t *testing.T) {
	// Deliberately pick an OS that can't match any Go target host.
	cfg := &config.Config{Platform: config.PlatformConfig{
		OS:   "plan9",
		Arch: runtime.GOARCH,
	}}
	err := checkPlatformMatch(cfg)
	if err == nil {
		t.Fatal("expected error for OS mismatch")
	}
	if !strings.Contains(err.Error(), "plan9") || !strings.Contains(err.Error(), runtime.GOOS) {
		t.Errorf("error should name both sides; got %v", err)
	}
}

func TestCheckPlatformMatch_ArchMismatch(t *testing.T) {
	cfg := &config.Config{Platform: config.PlatformConfig{
		OS:   runtime.GOOS,
		Arch: "mips",
	}}
	err := checkPlatformMatch(cfg)
	if err == nil {
		t.Fatal("expected error for arch mismatch")
	}
	if !strings.Contains(err.Error(), "mips") || !strings.Contains(err.Error(), runtime.GOARCH) {
		t.Errorf("error should name both sides; got %v", err)
	}
}

func TestCheckPlatformMatch_NilConfig(t *testing.T) {
	if err := checkPlatformMatch(nil); err != nil {
		t.Errorf("nil config should be accepted, got: %v", err)
	}
}
