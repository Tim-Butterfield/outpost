package main

import (
	"testing"

	"github.com/Tim-Butterfield/outpost/internal/config"
	"github.com/Tim-Butterfield/outpost/internal/probe"
)

func TestBuildDiff_AllFiveStatuses(t *testing.T) {
	stored := &config.Config{
		Dispatch: config.DispatchConfig{
			Path: map[string]string{
				"py":  "/old/python3",
				"sh":  "/bin/bash",
				"rb":  "/usr/bin/ruby",
				"pl":  "/usr/bin/perl",
				// "lua" intentionally absent
			},
			Version: map[string]string{
				"py": "Python 3.12.0",
				"sh": "GNU bash 5.2",
				"rb": "ruby 3.1",
				"pl": "perl 5.34",
			},
		},
	}
	detected := []probe.Interpreter{
		// python path differs -> changed (status dominates version)
		{Name: "python3", Path: "/new/python3.14", Extensions: []string{"py"}, Version: "Python 3.14.4", Working: true},
		// sh matches path AND version -> unchanged
		{Name: "bash", Path: "/bin/bash", Extensions: []string{"sh"}, Version: "GNU bash 5.2", Working: true},
		// perl: same path, different version -> version-changed
		{Name: "perl", Path: "/usr/bin/perl", Extensions: []string{"pl"}, Version: "perl 5.38", Working: true},
		// lua is new -> added
		{Name: "lua", Path: "/opt/homebrew/bin/lua", Extensions: []string{"lua"}, Version: "Lua 5.4", Working: true},
		// rb absent from detection -> removed
	}

	rows := buildDiff(stored, detected)

	byExt := map[string]DiffRow{}
	for _, r := range rows {
		byExt[r.Ext] = r
	}

	if len(rows) != 5 {
		t.Fatalf("got %d rows, want 5:\n%+v", len(rows), rows)
	}

	want := map[string]string{
		"lua": "added",
		"pl":  "version-changed",
		"py":  "changed",
		"rb":  "removed",
		"sh":  "unchanged",
	}
	for ext, status := range want {
		r, ok := byExt[ext]
		if !ok {
			t.Errorf("missing row for %s", ext)
			continue
		}
		if r.Status != status {
			t.Errorf("%s: status=%q, want %q", ext, r.Status, status)
		}
	}
}

func TestBuildDiff_VersionChangedSamePath(t *testing.T) {
	// Re-confirm with a focused case: an in-place interpreter
	// upgrade (same binary path, bumped --version output) surfaces
	// as version-changed, not as unchanged.
	stored := &config.Config{
		Dispatch: config.DispatchConfig{
			Path:    map[string]string{"py": "/opt/homebrew/bin/python3"},
			Version: map[string]string{"py": "Python 3.13.0"},
		},
	}
	detected := []probe.Interpreter{
		{Name: "python3", Path: "/opt/homebrew/bin/python3",
			Extensions: []string{"py"}, Version: "Python 3.14.4", Working: true},
	}
	rows := buildDiff(stored, detected)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].Status != "version-changed" {
		t.Errorf("status=%q, want version-changed", rows[0].Status)
	}
	if rows[0].StoredVersion != "Python 3.13.0" || rows[0].DetectedVersion != "Python 3.14.4" {
		t.Errorf("versions not captured: %+v", rows[0])
	}
}

func TestBuildDiff_AlphabeticalOrder(t *testing.T) {
	stored := &config.Config{
		Dispatch: config.DispatchConfig{
			Path: map[string]string{"z": "/z", "a": "/a", "m": "/m"},
		},
	}
	rows := buildDiff(stored, nil)
	want := []string{"a", "m", "z"}
	for i, r := range rows {
		if r.Ext != want[i] {
			t.Errorf("row[%d].Ext=%q, want %q", i, r.Ext, want[i])
		}
	}
}

func TestBuildDiff_EmptyStoredAllAdded(t *testing.T) {
	// First-time user scenario: no stored config, everything
	// detected should read as "added".
	detected := []probe.Interpreter{
		{Name: "bash", Path: "/bin/bash", Extensions: []string{"sh"}, Working: true},
		{Name: "python3", Path: "/usr/bin/python3", Extensions: []string{"py"}, Working: true},
	}
	rows := buildDiff(config.Default(), detected)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	for _, r := range rows {
		if r.Status != "added" {
			t.Errorf("%s: status=%q, want added", r.Ext, r.Status)
		}
		if r.StoredPath != "-" {
			t.Errorf("%s: StoredPath=%q, want -", r.Ext, r.StoredPath)
		}
	}
}

func TestBuildDiff_NilStored(t *testing.T) {
	// Defensive: nil stored pointer must not panic.
	rows := buildDiff(nil, []probe.Interpreter{
		{Name: "bash", Path: "/bin/bash", Extensions: []string{"sh"}, Working: true},
	})
	if len(rows) != 1 || rows[0].Status != "added" {
		t.Errorf("unexpected rows: %+v", rows)
	}
}

func TestBuildDiff_NoDetectedAllRemoved(t *testing.T) {
	// All configured interpreters gone -> all removed.
	stored := &config.Config{
		Dispatch: config.DispatchConfig{
			Path: map[string]string{"py": "/old/python3", "sh": "/bin/bash"},
		},
	}
	rows := buildDiff(stored, nil)
	for _, r := range rows {
		if r.Status != "removed" {
			t.Errorf("%s: status=%q, want removed", r.Ext, r.Status)
		}
		if r.DetectedPath != "(not found on PATH)" {
			t.Errorf("%s: DetectedPath=%q", r.Ext, r.DetectedPath)
		}
	}
}

func TestBuildDiff_BrokenInterpretersExcluded(t *testing.T) {
	// A detected interpreter marked Working=false (e.g., the
	// Windows Store Python stub) must not appear in the diff's
	// detected side. If it was previously stored, diff shows it
	// as removed; if it was never stored, it's absent entirely.
	stored := &config.Config{
		Dispatch: config.DispatchConfig{
			Path:    map[string]string{"py": "/old/python3"},
			Version: map[string]string{"py": "Python 3.12"},
		},
	}
	detected := []probe.Interpreter{
		{
			Name:       "python3",
			Path:       "C:/.../WindowsApps/python3.exe",
			Extensions: []string{"py"},
			Version:    "Python was not found; run without arguments...",
			Working:    false,
		},
	}
	rows := buildDiff(stored, detected)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].Status != "removed" {
		t.Errorf("broken detected interpreter should read as removed; got %q", rows[0].Status)
	}
}

func TestBuildDiff_FirstMatchWinsOnDetectionOrder(t *testing.T) {
	// Two interpreters both claim the "sh" extension; the first
	// in detection order wins in the diff, matching the behavior
	// of writeConfigFromDetection.
	detected := []probe.Interpreter{
		{Name: "bash", Path: "/bin/bash", Extensions: []string{"sh"}, Working: true},
		{Name: "zsh", Path: "/bin/zsh", Extensions: []string{"sh"}, Working: true},
	}
	rows := buildDiff(config.Default(), detected)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (dedup on ext)", len(rows))
	}
	if rows[0].DetectedPath != "/bin/bash" {
		t.Errorf("DetectedPath=%q, want /bin/bash", rows[0].DetectedPath)
	}
}
