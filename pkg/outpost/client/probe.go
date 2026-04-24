package client

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/Tim-Butterfield/outpost/pkg/outpost/capability"
)

// TargetProbe is a point-in-time snapshot of one target's
// reachability and readiness. Cheap to compute; intended for bulk
// "which targets can I use right now?" queries.
type TargetProbe struct {
	// Name is the target's registered name.
	Name string

	// Available is true when the probe succeeded, the responder is
	// alive, at least one lane can accept work, and there is no
	// responder_name collision with a sibling probe.
	Available bool

	// Reachable is true when dispatch.txt could be read.
	Reachable bool

	// ResponderAlive is true when at least one lane's status.txt
	// last_heartbeat is within the probe's freshness window.
	ResponderAlive bool

	// ResponderName is the name advertised by the responder in
	// dispatch.txt. Empty when the operator did not set --name.
	ResponderName string

	// Description is the operator-written sentence about the
	// target's purpose. Empty when unset.
	Description string

	// Tags are operator-declared semantic labels. Submitters use
	// these to filter targets by task suitability.
	Tags []string

	// Tools is the map of auto-probed build/dev tools on the
	// target ("git" -> "git version 2.42.0", "dotnet" -> "8.0.100").
	// Submitters consult this to choose task-appropriate targets.
	Tools map[string]string

	// PlatformOS is the OS family advertised by the responder
	// ("linux", "darwin", "windows"). Submitters use this to make
	// platform-aware interpreter choices.
	PlatformOS string

	// PlatformArch is the CPU architecture advertised by the
	// responder ("amd64", "arm64", ...).
	PlatformArch string

	// CWD is the responder's working directory in its OS-native
	// form (e.g. "X:\\github.com\\..." on Windows, "/home/u/..." on
	// Linux). Submitters reading this can reason about the scope of
	// relative paths in jobs they submit to this target.
	CWD string

	// CWDSource is the cross-host identifier of the CWD's backing
	// store (UNC on Windows; "<mount-source>/<tail>" on Unix for
	// remote FS types). Empty when CWD is on local storage.
	// Submitters correlating targets can match by CWDSource (or by
	// CWDSource tail) to identify shared-store groups.
	CWDSource string

	// ComSpec is the responder's %COMSPEC% (Windows only). On
	// normalized targets this is always cmd.exe; a non-cmd.exe
	// value here indicates outpost failed to override it and .cmd
	// jobs with pipes may misbehave.
	ComSpec string

	// ProtocolVersion is the integer the responder advertised.
	// Submitters should refuse to submit if this does not match
	// their own expected version.
	ProtocolVersion int

	// LaneCount is the number of lanes the responder serves.
	LaneCount int

	// LaneStates[i] is the State of lane (i+1). len == LaneCount
	// when Reachable; empty otherwise.
	LaneStates []capability.State

	// Queued[i] is the pending-job count for lane (i+1). Aligned
	// with LaneStates.
	Queued []int

	// CollisionWith names the other configured targets that
	// advertise the same non-empty ResponderName as this probe.
	// Non-nil and sorted alphabetically when a collision is
	// detected. Empty otherwise.
	CollisionWith []string

	// CheckedAt is the UTC time the probe was initiated.
	CheckedAt time.Time

	// Latency is the wall-clock duration between probe start and
	// result finalization.
	Latency time.Duration

	// Err is populated on probe failure or for collided probes
	// (ErrResponderNameCollision).
	Err error
}

// ProbeOption customizes Probe / TargetProbes behavior.
type ProbeOption func(*probeConfig)

type probeConfig struct {
	timeout   time.Duration
	freshness time.Duration
}

// WithTimeout caps the per-target probe at d. Default: 5s.
func WithTimeout(d time.Duration) ProbeOption {
	return func(c *probeConfig) { c.timeout = d }
}

// WithFreshness sets the liveness window: a lane is alive when its
// last_heartbeat is younger than d. Default: 10s.
func WithFreshness(d time.Duration) ProbeOption {
	return func(c *probeConfig) { c.freshness = d }
}

func defaultProbeConfig() probeConfig {
	return probeConfig{timeout: 5 * time.Second, freshness: 10 * time.Second}
}

// Probe reads the target's dispatch.txt and each lane's status.txt
// within the configured timeout, and returns a TargetProbe
// describing what was observed.
//
// This method does NOT perform collision detection; call
// Client.TargetProbes or Client.AvailableTargets for that.
//
// Named return `p` is required so the deferred Latency-update is
// visible to the caller (a non-named return would copy p into the
// return slot before defers run).
func (t *Target) Probe(ctx context.Context, opts ...ProbeOption) (p TargetProbe) {
	cfg := defaultProbeConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	p = TargetProbe{Name: t.name, CheckedAt: time.Now().UTC()}
	start := time.Now()
	defer func() { p.Latency = time.Since(start) }()

	pctx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()

	disp, err := t.transport.ReadDispatch(pctx)
	if err != nil {
		p.Err = err
		return p
	}
	p.Reachable = true
	p.ResponderName = disp.ResponderName
	p.Description = disp.Description
	p.Tags = disp.Tags
	p.Tools = disp.Tools
	p.PlatformOS = disp.PlatformOS
	p.PlatformArch = disp.PlatformArch
	p.CWD = disp.CWD
	p.CWDSource = disp.CWDSource
	p.ComSpec = disp.ComSpec
	p.ProtocolVersion = disp.ProtocolVersion
	p.LaneCount = disp.LaneCount
	p.LaneStates = make([]capability.State, disp.LaneCount)
	p.Queued = make([]int, disp.LaneCount)

	alive := false
	accepts := false
	now := time.Now()
	for i := 0; i < disp.LaneCount; i++ {
		lane := i + 1
		status, err := t.transport.ReadStatus(pctx, lane)
		if err != nil {
			continue
		}
		p.LaneStates[i] = status.State
		p.Queued[i] = status.Queued
		if !status.LastHeartbeat.IsZero() && now.Sub(status.LastHeartbeat) < cfg.freshness {
			alive = true
		}
		switch status.State {
		case capability.StatePaused, capability.StateStopping, capability.StateStopped:
			// not accepting
		default:
			accepts = true
		}
	}
	p.ResponderAlive = alive
	p.Available = p.Reachable && p.ResponderAlive && accepts
	return p
}

// IsAvailable is a convenience: Probe(...).Available.
func (t *Target) IsAvailable(ctx context.Context, opts ...ProbeOption) bool {
	return t.Probe(ctx, opts...).Available
}

// TargetProbes runs Probe against every configured target
// concurrently, then runs collision detection on the results.
//
// Total wall time is bounded by the per-target timeout, not the
// sum of all probes: an unreachable target that hits its timeout
// does not delay sibling probes.
//
// Returned probes are in the same alphabetical order as
// TargetNames.
func (c *Client) TargetProbes(ctx context.Context, opts ...ProbeOption) []TargetProbe {
	names := c.TargetNames()
	probes := make([]TargetProbe, len(names))

	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			probes[i] = c.targets[name].Probe(ctx, opts...)
		}(i, name)
	}
	wg.Wait()

	// Collision detection: group non-empty ResponderNames, flag
	// groups of size > 1.
	byName := map[string][]int{}
	for i := range probes {
		if probes[i].ResponderName == "" {
			continue
		}
		byName[probes[i].ResponderName] = append(byName[probes[i].ResponderName], i)
	}
	for _, group := range byName {
		if len(group) < 2 {
			continue
		}
		groupNames := make([]string, 0, len(group))
		for _, idx := range group {
			groupNames = append(groupNames, probes[idx].Name)
		}
		for _, idx := range group {
			others := make([]string, 0, len(groupNames)-1)
			for _, gn := range groupNames {
				if gn != probes[idx].Name {
					others = append(others, gn)
				}
			}
			sort.Strings(others)
			probes[idx].CollisionWith = others
			probes[idx].Available = false
			if probes[idx].Err == nil {
				probes[idx].Err = ErrResponderNameCollision
			}
		}
	}

	return probes
}

// AvailableTargets returns the names of configured targets that
// pass the availability check in TargetProbes.
func (c *Client) AvailableTargets(ctx context.Context, opts ...ProbeOption) []string {
	probes := c.TargetProbes(ctx, opts...)
	out := make([]string, 0, len(probes))
	for _, p := range probes {
		if p.Available {
			out = append(out, p.Name)
		}
	}
	return out
}

// TargetsWithTag runs a bulk probe and returns the names of
// available targets whose advertised tags include the given tag.
// Returns names in the same alphabetical order as TargetNames.
func (c *Client) TargetsWithTag(ctx context.Context, tag string, opts ...ProbeOption) []string {
	probes := c.TargetProbes(ctx, opts...)
	out := make([]string, 0, len(probes))
	for _, p := range probes {
		if !p.Available {
			continue
		}
		for _, t := range p.Tags {
			if t == tag {
				out = append(out, p.Name)
				break
			}
		}
	}
	return out
}

// TargetsWithTool is symmetric to TargetsWithTag: returns the
// names of available targets on which the given build/dev tool
// was detected.
func (c *Client) TargetsWithTool(ctx context.Context, tool string, opts ...ProbeOption) []string {
	probes := c.TargetProbes(ctx, opts...)
	out := make([]string, 0, len(probes))
	for _, p := range probes {
		if !p.Available {
			continue
		}
		if _, ok := p.Tools[tool]; ok {
			out = append(out, p.Name)
		}
	}
	return out
}
