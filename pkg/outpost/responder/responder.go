// Package responder runs the outpost daemon: polls each lane's
// inbox, dispatches jobs through the configured Dispatcher, and
// publishes results back through the configured Transport.
//
// Every architectural concern is injected via Config: Transport,
// Dispatcher, Authenticator, EventSink. The responder itself is a
// pure orchestrator with no knowledge of how jobs arrive or how
// they are executed. See DESIGN.md §4 for the volatility-boundary
// rationale.
//
// Concurrency: one goroutine per lane. Lanes share no mutable
// state; each owns its own inbox, cancel dir, outbox, log dir, and
// status.txt. Global sentinels (STOP / PAUSE / RESTART) are
// filesystem files observed independently by each lane on every
// poll cycle.
package responder

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Tim-Butterfield/outpost/pkg/outpost/auth"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/capability"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/dispatcher"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/events"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/protocol"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/transport"
)

// ErrRestart is returned by Run when the RESTART sentinel triggered
// the shutdown. A supervisor wrapper should treat this as a request
// to re-exec the responder; a plain STOP returns nil (no restart).
var ErrRestart = errors.New("responder: RESTART sentinel received")

// ErrTransportUnavailable is returned by Run when the shared
// directory could not be prepared at startup (commonly: mount not
// yet available). A supervisor wrapper should back off and retry
// rather than exit; typical deployment is on a network share whose
// availability lags boot time.
var ErrTransportUnavailable = errors.New("responder: transport unavailable")

// Config bundles everything a Responder needs at construction
// time. All fields except Authenticator and Events are required.
type Config struct {
	// Transport exposes the shared-dir operations the responder
	// needs (ListPending, OpenJob, PublishResult, etc.) and also
	// implements the Submitter side for sentinel writes, which the
	// responder does not invoke but which shape the same
	// filesystem tree.
	Transport transport.Transport

	// Dispatcher runs each worker. Synchronous Run.
	Dispatcher dispatcher.Dispatcher

	// Authenticator gates submitter identity. Typically auth.NoOp()
	// because file-RPC relies on filesystem ACLs. nil is treated
	// as NoOp.
	Authenticator auth.Authenticator

	// Events receives structured observability records. nil is
	// treated as events.Discard().
	Events events.EventSink

	// ResponderName is the operator-assigned identity advertised in
	// dispatch.txt. Empty means "no identity advertised."
	ResponderName string

	// Description, Tags, Tools are passed through from
	// outpost.toml [responder] / [capabilities] sections into
	// dispatch.txt so submitters can do task-aware target
	// selection without reading the responder's config file.
	Description string
	Tags        []string
	Tools       map[string]string

	// LaneCount is the number of lanes to serve. Must be >= 1.
	LaneCount int

	// PollInterval is the heartbeat cadence and the interval at
	// which idle lanes check for new jobs. Must be > 0.
	PollInterval time.Duration

	// DefaultTimeout is the per-job timeout used when a Job does
	// not specify one and no in-script header overrides.
	DefaultTimeout time.Duration

	// RetentionDays is the age (in days) beyond which log, outbox,
	// and cancel entries are pruned at startup. <= 0 disables
	// retention (every artifact kept forever).
	RetentionDays int

	// DispatchTable is the resolved interpreter-path map written
	// into dispatch.txt at startup. Keys are extensions without
	// leading dots; values are absolute interpreter paths.
	DispatchTable map[string]string

	// DispatchOrder is the operator's preference order, echoed into
	// dispatch.txt. Used by submitter-side AIs choosing among
	// multiple dispatchable extensions.
	DispatchOrder []string

	// PlatformOS and PlatformArch are the responder host's OS
	// family and CPU architecture, advertised in dispatch.txt so
	// submitter AIs can make platform-aware interpreter choices
	// (e.g. prefer PowerShell on Windows). Typically populated
	// from runtime.GOOS / runtime.GOARCH by the CLI layer.
	PlatformOS   string
	PlatformArch string

	// CWD is the working directory the responder process was
	// started in, advertised in dispatch.txt so submitters composing
	// relative paths (e.g. "../sibling-repo") can resolve them.
	// Typically populated from os.Getwd() by the CLI layer.
	CWD string

	// CWDSource is the network/hypervisor-share identifier of the
	// CWD's backing store (UNC on Windows; mount-source+tail on
	// Linux/macOS). Populated by internal/platform.ResolveCWDSource.
	// Empty for local storage.
	CWDSource string

	// ComSpec is the value of the Windows %COMSPEC% environment
	// variable as captured at responder startup. Advertised in
	// dispatch.txt so submitters know which shell internal subshell
	// invocations (pipes, `for /f`, `cmd /c`) in .cmd/.bat jobs
	// will fork. Typically populated from os.Getenv("COMSPEC") on
	// Windows and left empty on other platforms.
	ComSpec string

	// ConfigPath is the absolute path to outpost.toml. When non-empty
	// (and Reload is also set), the responder polls this file's
	// mtime and triggers a reload when it changes. Enables the
	// install-a-tool-then-use-it workflow without restarting the
	// responder.
	ConfigPath string

	// Reload is invoked when the responder detects that ConfigPath
	// has changed on disk. It should re-read the config, re-run
	// the interpreter probe, and return the fields that participate
	// in dispatch.txt. Errors are logged WARN and the current
	// in-memory dispatch table is retained.
	Reload func(ctx context.Context) (ReloadResult, error)
}

// ReloadResult is the subset of Config fields that can change when
// outpost.toml is refreshed mid-run. The responder treats every
// other startup-captured value (platform, CWD, ComSpec, lane count,
// poll interval, responder name) as immutable — those are baked at
// process start and only change via a full restart.
type ReloadResult struct {
	Description   string
	Tags          []string
	Tools         map[string]string
	DispatchOrder []string
	DispatchTable map[string]string
}

// Responder runs the daemon described by its Config.
type Responder struct {
	cfg Config
	pid int

	// dispatchMu guards the reload-able subset of what dispatch.txt
	// advertises. Only the config-watch goroutine writes; the initial
	// write in initialize and reload-driven rewrites both take the
	// write lock. The fields themselves (description, tags, tools,
	// dispatch order/table) are held here rather than in cfg so the
	// watch goroutine's mutations don't race with anything reading
	// cfg.
	dispatchMu    sync.RWMutex
	description   string
	tags          []string
	tools         map[string]string
	dispatchOrder []string
	dispatchPaths map[string]string
	startedAt     time.Time
}

// New validates cfg and returns a ready-to-Run Responder.
func New(cfg Config, pid int) (*Responder, error) {
	if cfg.Transport == nil {
		return nil, errors.New("responder: Transport is required")
	}
	if cfg.Dispatcher == nil {
		return nil, errors.New("responder: Dispatcher is required")
	}
	if cfg.LaneCount < 1 {
		return nil, fmt.Errorf("responder: LaneCount must be >= 1; got %d", cfg.LaneCount)
	}
	if cfg.PollInterval <= 0 {
		return nil, fmt.Errorf("responder: PollInterval must be > 0; got %v", cfg.PollInterval)
	}
	if cfg.DefaultTimeout <= 0 {
		cfg.DefaultTimeout = 60 * time.Second
	}
	if cfg.Authenticator == nil {
		cfg.Authenticator = auth.NoOp()
	}
	if cfg.Events == nil {
		cfg.Events = events.Discard()
	}
	return &Responder{
		cfg:           cfg,
		pid:           pid,
		description:   cfg.Description,
		tags:          cfg.Tags,
		tools:         cfg.Tools,
		dispatchOrder: cfg.DispatchOrder,
		dispatchPaths: cfg.DispatchTable,
	}, nil
}

// Run starts the responder and blocks until all lane goroutines
// exit. Returns:
//
//   - nil on clean exit (STOP sentinel, ctx cancellation)
//   - ErrRestart when the RESTART sentinel triggered shutdown;
//     supervisors use this to distinguish stop-and-exit from
//     stop-and-relaunch without parsing logs or filesystem state
//   - ErrTransportUnavailable when the shared-dir could not be
//     prepared (typically mount not yet available at boot)
//   - any other non-nil error only when initialization fails
//     before the lane goroutines start
func (r *Responder) Run(ctx context.Context) error {
	if err := r.initialize(ctx); err != nil {
		return err
	}

	stop := make(chan struct{})
	var once sync.Once
	// restartRequested is set by watchStopSentinel before it closes
	// stop if the trigger was a RESTART sentinel (vs. STOP). Using
	// atomic.Bool keeps the read at Run's return site safe without
	// another mutex.
	var restartRequested atomic.Bool
	triggerStop := func(restart bool) {
		once.Do(func() {
			if restart {
				restartRequested.Store(true)
			}
			close(stop)
		})
	}

	// Global sentinel watcher: closes stop when STOP/RESTART observed.
	go r.watchStopSentinel(ctx, stop, triggerStop)

	// Config-file watcher: polls outpost.toml for mtime changes and
	// reloads the dispatch table without restarting the responder.
	// Noop when ConfigPath/Reload are unset.
	go r.watchConfigFile(ctx, stop)

	var wg sync.WaitGroup
	for lane := 1; lane <= r.cfg.LaneCount; lane++ {
		wg.Add(1)
		go func(laneNum int) {
			defer wg.Done()
			r.runLane(ctx, laneNum, stop)
		}(lane)
	}

	<-stop
	wg.Wait()

	_ = r.cfg.Events.Close()
	if restartRequested.Load() {
		return ErrRestart
	}
	return nil
}

// initialize does once-per-startup setup: clear stale termination
// sentinels from a previous session, run retention sweep, then
// publish dispatch.txt. Ordering matters: we advertise capabilities
// only after cleanup has finished so probes from submitters see a
// freshly pruned tree.
//
// STOP and RESTART sentinels from a previous session are cleared
// here: they are signals to a *running* responder, not persistent
// "stay stopped" state. A fresh `outpost run` invocation is itself
// an explicit start action, and should supersede any stale signal
// left behind by an earlier `outpost stop`. PAUSE is NOT cleared:
// operators may intentionally bring up a responder paused (e.g.,
// to verify config before accepting jobs), so PAUSE persists
// across restarts.
func (r *Responder) initialize(ctx context.Context) error {
	for _, name := range []string{protocol.SentinelSTOP, protocol.SentinelRESTART} {
		present, err := r.cfg.Transport.CheckSentinel(ctx, name)
		if err != nil {
			// Stat failure is not fatal -- proceed and let the
			// sentinel watcher catch real problems later.
			continue
		}
		if !present {
			continue
		}
		if err := r.cfg.Transport.SetSentinel(ctx, name, false); err != nil {
			r.emit(ctx, events.Event{
				Level:   "WARN",
				Message: "failed to clear stale sentinel at startup",
				Fields:  map[string]string{"sentinel": name, "err": err.Error()},
			})
			continue
		}
		r.emit(ctx, events.Event{
			Level:   "INFO",
			Message: "cleared stale sentinel at startup",
			Fields:  map[string]string{"sentinel": name},
		})
	}

	if r.cfg.RetentionDays > 0 {
		cutoff := time.Now().UTC().Add(-time.Duration(r.cfg.RetentionDays) * 24 * time.Hour)
		if err := r.cfg.Transport.Cleanup(ctx, cutoff); err != nil {
			r.emit(ctx, events.Event{
				Level:   "WARN",
				Message: "retention sweep failed",
				Fields:  map[string]string{"err": err.Error()},
			})
		}
	}

	r.startedAt = time.Now().UTC()
	if err := r.publishDispatch(ctx); err != nil {
		return fmt.Errorf("responder: write dispatch.txt: %w", err)
	}

	startFields := map[string]string{
		"lanes": fmt.Sprint(r.cfg.LaneCount),
		"name":  r.cfg.ResponderName,
		"pid":   fmt.Sprint(r.pid),
	}
	if r.cfg.CWD != "" {
		startFields["cwd"] = r.cfg.CWD
	}
	if r.cfg.CWDSource != "" {
		startFields["cwd_source"] = r.cfg.CWDSource
	}
	if r.cfg.ComSpec != "" {
		startFields["comspec"] = r.cfg.ComSpec
	}
	r.emit(ctx, events.Event{
		Level:   "INFO",
		Message: "responder started",
		Fields:  startFields,
	})
	return nil
}

// watchStopSentinel polls for STOP / RESTART on the poll cadence
// and triggers shutdown on detection. The trigger callback
// distinguishes the two by its bool argument so Run can map each
// to a different return value (and thus a different exit code).
func (r *Responder) watchStopSentinel(ctx context.Context, stop <-chan struct{}, trigger func(restart bool)) {
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			trigger(false)
			return
		case <-stop:
			return
		case <-ticker.C:
			present, err := r.cfg.Transport.CheckSentinel(ctx, protocol.SentinelSTOP)
			if err == nil && present {
				r.emit(ctx, events.Event{
					Level:   "INFO",
					Message: "STOP sentinel observed; initiating shutdown",
				})
				trigger(false)
				return
			}
			// RESTART triggers the same shutdown but Run returns
			// ErrRestart so a supervisor can distinguish "exit and
			// re-exec" from "exit and stay exited".
			present, err = r.cfg.Transport.CheckSentinel(ctx, protocol.SentinelRESTART)
			if err == nil && present {
				r.emit(ctx, events.Event{
					Level:   "INFO",
					Message: "RESTART sentinel observed; exiting for supervisor relaunch",
				})
				trigger(true)
				return
			}
		}
	}
}

// emit is a small convenience that ignores event-sink errors; the
// sink is fail-soft by design.
func (r *Responder) emit(ctx context.Context, e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	_ = r.cfg.Events.Emit(ctx, e)
}

// publishDispatch writes the current dispatch.txt from the
// mutable state (description/tags/tools/order/paths) plus the
// immutable startup fields (pid, started, platform, cwd, comspec,
// responder name, lane count). Called once at initialize and again
// on every successful reload.
func (r *Responder) publishDispatch(ctx context.Context) error {
	r.dispatchMu.RLock()
	disp := capability.Dispatch{
		ProtocolVersion: protocol.Version,
		PID:             r.pid,
		Started:         r.startedAt,
		ResponderName:   r.cfg.ResponderName,
		Description:     r.description,
		Tags:            r.tags,
		Tools:           r.tools,
		PlatformOS:      r.cfg.PlatformOS,
		PlatformArch:    r.cfg.PlatformArch,
		CWD:             r.cfg.CWD,
		CWDSource:       r.cfg.CWDSource,
		ComSpec:         r.cfg.ComSpec,
		LaneCount:       r.cfg.LaneCount,
		Order:           r.dispatchOrder,
		Paths:           r.dispatchPaths,
	}
	r.dispatchMu.RUnlock()
	return r.cfg.Transport.WriteDispatch(ctx, disp)
}

// watchConfigFile polls ConfigPath's mtime on a fixed cadence
// (slower than the lane poll so re-probes don't pile up). When the
// file changes, calls Reload and, on success, updates the
// Dispatcher's paths and rewrites dispatch.txt.
//
// Noop when ConfigPath or Reload is zero. Errors from Reload are
// logged WARN; the current in-memory dispatch table stays in force.
// Stat errors (e.g., outpost.toml deleted or become unreadable)
// are also WARNed without clobbering state.
func (r *Responder) watchConfigFile(ctx context.Context, stop <-chan struct{}) {
	if r.cfg.ConfigPath == "" || r.cfg.Reload == nil {
		return
	}
	initialStat, err := os.Stat(r.cfg.ConfigPath)
	if err != nil {
		r.emit(ctx, events.Event{
			Level:   "WARN",
			Message: "config watcher: initial stat failed",
			Fields:  map[string]string{"path": r.cfg.ConfigPath, "err": err.Error()},
		})
		return
	}
	lastMtime := initialStat.ModTime()

	// Watch cadence: 5x the lane poll interval, with a 5s floor.
	// Slower than lane polling so the probe work — which re-invokes
	// every interpreter — doesn't saturate a short-polling setup.
	interval := 5 * r.cfg.PollInterval
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-ticker.C:
		}

		stat, err := os.Stat(r.cfg.ConfigPath)
		if err != nil {
			r.emit(ctx, events.Event{
				Level:   "WARN",
				Message: "config watcher: stat failed",
				Fields:  map[string]string{"path": r.cfg.ConfigPath, "err": err.Error()},
			})
			continue
		}
		if !stat.ModTime().After(lastMtime) {
			continue
		}
		lastMtime = stat.ModTime()
		r.applyReload(ctx)
	}
}

// applyReload is the single code path for config-reload work.
// Keeps failures localized: on any error, the old dispatch table
// stays in force and a WARN event captures the reason. On success,
// the mutable fields are swapped atomically, the Dispatcher's path
// map is updated (if the dispatcher implements Reloadable), and
// dispatch.txt is rewritten.
func (r *Responder) applyReload(ctx context.Context) {
	result, err := r.cfg.Reload(ctx)
	if err != nil {
		r.emit(ctx, events.Event{
			Level:   "WARN",
			Message: "config reload failed; keeping current dispatch table",
			Fields:  map[string]string{"err": err.Error()},
		})
		return
	}

	r.dispatchMu.Lock()
	r.description = result.Description
	r.tags = result.Tags
	r.tools = result.Tools
	r.dispatchOrder = result.DispatchOrder
	r.dispatchPaths = result.DispatchTable
	r.dispatchMu.Unlock()

	// Update the Dispatcher if it supports reload. Dispatchers that
	// don't hold an interpreter table simply don't implement
	// Reloadable and are skipped.
	if rel, ok := r.cfg.Dispatcher.(dispatcher.Reloadable); ok {
		rel.UpdatePaths(result.DispatchTable)
	}

	if err := r.publishDispatch(ctx); err != nil {
		r.emit(ctx, events.Event{
			Level:   "WARN",
			Message: "config reload: failed to rewrite dispatch.txt",
			Fields:  map[string]string{"err": err.Error()},
		})
		return
	}

	r.emit(ctx, events.Event{
		Level:   "INFO",
		Message: "config reloaded from outpost.toml change",
		Fields: map[string]string{
			"extensions": fmt.Sprint(len(result.DispatchTable)),
			"tools":      fmt.Sprint(len(result.Tools)),
		},
	})
}

// Event is an alias for events.Event so callers of the responder
// package don't need to import pkg/outpost/events purely for the
// emit helper signature. Exported as a convenience.
type Event = events.Event
