// Package client is outpost's submitter-side API.
//
// Two types are exported:
//
//   - Target represents one outpost responder reached through one
//     transport. It is the primary single-target API: create, call
//     Submit / Probe / Status / SetSentinel, done.
//
//   - Client composes N named Targets. It is the multi-target
//     orchestrator: load from targets.toml or build programmatically
//     with WithTarget; run parallel probes; pick an available
//     target by name.
//
// See DESIGN.md §9 for the decomposition rationale.
package client

import (
	"errors"
	"sort"

	"github.com/Tim-Butterfield/outpost/pkg/outpost/auth"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/transport"
)

// ErrResponderNameCollision indicates that two or more configured
// targets advertise the same non-empty `responder_name` in their
// dispatch.txt. Populated into TargetProbe.Err for every probe in
// the offending group; Submit against a collided target returns
// this error too.
var ErrResponderNameCollision = errors.New("client: responder_name collision between configured targets")

// ErrUnknownTarget indicates Client.Target was called with a name
// that does not match any configured target.
var ErrUnknownTarget = errors.New("client: unknown target")

// Client is an orchestrator over N named Targets.
type Client struct {
	targets map[string]*Target
}

// Option configures a Client.
type Option func(*Client)

// WithTarget registers a Target under name, using the provided
// transport and the no-op authenticator. To supply a custom
// Authenticator, use WithAuthenticatedTarget.
func WithTarget(name string, tp transport.Transport) Option {
	return WithAuthenticatedTarget(name, tp, auth.NoOp())
}

// WithAuthenticatedTarget registers a Target with a custom
// authenticator. Useful for transports that require authentication.
func WithAuthenticatedTarget(name string, tp transport.Transport, a auth.Authenticator) Option {
	return func(c *Client) {
		c.targets[name] = &Target{
			name:      name,
			transport: tp,
			auth:      a,
		}
	}
}

// NewClient returns a Client configured by the given options.
// Duplicate names overwrite earlier entries in order, matching
// "last wins" semantics familiar from other Go option patterns.
func NewClient(opts ...Option) *Client {
	c := &Client{targets: map[string]*Target{}}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Target returns the target registered under name, or nil if no
// such target exists. Callers that want an error-returning lookup
// can use TargetOrError.
func (c *Client) Target(name string) *Target {
	return c.targets[name]
}

// TargetOrError returns the target registered under name, or
// ErrUnknownTarget if absent.
func (c *Client) TargetOrError(name string) (*Target, error) {
	if t, ok := c.targets[name]; ok {
		return t, nil
	}
	return nil, ErrUnknownTarget
}

// TargetNames returns the configured target names in
// alphabetical order. Useful for iterating deterministically in
// CLI output and tests.
func (c *Client) TargetNames() []string {
	names := make([]string, 0, len(c.targets))
	for name := range c.targets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
