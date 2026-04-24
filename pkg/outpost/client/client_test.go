package client_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Tim-Butterfield/outpost/pkg/outpost"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/client"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/transport/file"
)

func TestClient_ConstructionAndLookup(t *testing.T) {
	tp1 := file.New(t.TempDir())
	tp2 := file.New(t.TempDir())
	c := client.NewClient(
		client.WithTarget("alpha", tp1),
		client.WithTarget("beta", tp2),
	)
	names := c.TargetNames()
	if len(names) != 2 || names[0] != "alpha" || names[1] != "beta" {
		t.Errorf("names=%v, want [alpha beta]", names)
	}
	if c.Target("alpha") == nil {
		t.Error("alpha not found")
	}
	if c.Target("gamma") != nil {
		t.Error("gamma should not exist")
	}
	if _, err := c.TargetOrError("gamma"); !errors.Is(err, client.ErrUnknownTarget) {
		t.Errorf("TargetOrError(gamma) err=%v, want ErrUnknownTarget", err)
	}
}

func TestClient_DuplicateNameLastWins(t *testing.T) {
	tp1 := file.New("/tmp/a")
	tp2 := file.New("/tmp/b")
	c := client.NewClient(
		client.WithTarget("x", tp1),
		client.WithTarget("x", tp2),
	)
	if len(c.TargetNames()) != 1 {
		t.Errorf("duplicate should collapse; names=%v", c.TargetNames())
	}
	if c.Target("x").Transport() != tp2 {
		t.Error("last WithTarget should win")
	}
}

func TestLoadClientFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "targets.toml")
	content := `
default = "alpha"

[target.alpha]
transport = "file"
path = "/tmp/alpha-share"

[target.beta]
transport = "file"
path = "/tmp/beta-share"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	c, err := client.LoadClientFromFile(path)
	if err != nil {
		t.Fatalf("LoadClientFromFile: %v", err)
	}
	names := c.TargetNames()
	if len(names) != 2 {
		t.Errorf("got %d targets, want 2", len(names))
	}
	if c.Target("alpha") == nil || c.Target("beta") == nil {
		t.Error("missing expected targets")
	}
}

func TestLoadClientFromFile_RejectsUnknownTransport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "targets.toml")
	content := `
[target.cloud]
transport = "http"
url = "https://example.com"
`
	_ = os.WriteFile(path, []byte(content), 0644)
	_, err := client.LoadClientFromFile(path)
	if err == nil {
		t.Fatal("expected unknown-transport error")
	}
}

func TestLoadClientFromFile_MissingPath(t *testing.T) {
	_, err := client.LoadClientFromFile(filepath.Join(t.TempDir(), "nope.toml"))
	if err == nil {
		t.Error("missing explicit path should error")
	}
}

func TestNewTarget_AdHoc(t *testing.T) {
	tp := file.New(t.TempDir())
	target := client.NewTarget("ad-hoc", tp)
	if target.Name() != "ad-hoc" {
		t.Errorf("name=%q", target.Name())
	}
	if target.Transport() != tp {
		t.Error("transport mismatch")
	}
}

func TestTarget_Submit_RejectsEmptyExt(t *testing.T) {
	tp := file.New(t.TempDir())
	target := client.NewTarget("x", tp)
	_, err := target.Submit(context.Background(), outpost.Job{
		Content: []byte("x"),
		// no Ext
	})
	if err == nil {
		t.Error("submit without ext should error")
	}
}
