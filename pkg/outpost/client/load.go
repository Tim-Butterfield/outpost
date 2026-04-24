package client

import (
	"fmt"

	"github.com/Tim-Butterfield/outpost/internal/config"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/transport/file"
)

// LoadClient builds a Client from the default targets.toml
// location (per-platform XDG / APPDATA path). A missing file
// yields an empty Client -- callers can add targets
// programmatically or fall back to ad-hoc --dir usage.
func LoadClient() (*Client, error) {
	return LoadClientFromFile("")
}

// LoadClientFromFile builds a Client from an explicit targets.toml
// path. Empty path defers to the default location.
func LoadClientFromFile(path string) (*Client, error) {
	tc, err := config.LoadTargets(path)
	if err != nil {
		return nil, err
	}
	opts := make([]Option, 0, len(tc.Target))
	for name, entry := range tc.Target {
		switch entry.Transport {
		case config.TransportFile:
			tp := file.New(entry.Path)
			opts = append(opts, WithTarget(name, tp))
		default:
			return nil, fmt.Errorf("client: target %q: transport=%q not supported", name, entry.Transport)
		}
	}
	return NewClient(opts...), nil
}
