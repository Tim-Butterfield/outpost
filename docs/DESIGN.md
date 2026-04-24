# outpost -- Design Document

This document describes outpost's scope, protocol, and architecture
in enough detail for a contributor — human or AI — to reason about
the system without reading every file. For behavior that conflicts
between this document and the code, the code is authoritative.

---

## 1. Thesis

An AI assistant does not need to run in the same place as the code it
is helping to build. It needs a way to execute work on the target and
observe the results. Outpost is that mechanism: a minimal, auditable,
transport-pluggable bridge between where the AI runs and where the
code needs to run.

This framing comes from the blog post "Running AI Where It Doesn't
Exist" (<https://timbutterfield.com/post/running-ai-where-it-does-not-exist/>).

The name *outpost* is literal: the target host becomes the AI's outpost
in an environment where the AI itself cannot live. The AI submits
work; the outpost executes it and reports back.

## 2. Positioning

### What outpost is

- A small binary that runs on both sides of a file-RPC channel.
- A well-defined, versioned protocol for submitting jobs and receiving
  results through a shared directory.
- A Go library (`pkg/outpost/...`) that third parties can build on;
  the CLI is a first-class consumer of this library.
- Polyglot on the worker side: the outpost responder dispatches
  `.sh`, `.ps1`, `.py`, `.cmd`, `.btm`, or any executable by file
  extension. Outpost does not care what language workers are written
  in.

### What outpost is *not*

| Outpost is not... | Because... |
|---|---|
| SSH | Outpost works in environments where no listening daemon can run on the target. |
| A task runner (just, make, task) | Task runners are developer-facing build orchestrators; outpost is a transport for remote execution. |
| A CI runner | Outpost is not tied to a specific SaaS provider and does not model pipelines, artifacts, or stages. |
| Ansible / Puppet / Salt | Those are configuration-management frameworks over SSH; outpost is narrower and transport-agnostic. |
| A general container orchestrator | Outpost runs one subprocess per job on an existing host. It does not manage containers. |

### Differentiators

1. **Runs where the AI cannot be installed.** That is the entire point.
2. **No listening daemon on the target.** The responder polls a shared
   directory; nothing listens on a port. Critical for locked-down
   environments where installing or exposing network services is not
   permitted.
3. **Transport-agnostic.** The shipped transport is a shared
   directory, which can be realized as SMB, NFS, Syncthing, Dropbox,
   iCloud, git-tracked folder, or physical sneakernet. The
   `Transport` interface is public; other implementations slot in
   without responder changes.
4. **Reproducibility by default.** Every job is a durable file; every
   result is a durable file. An AI's actions on a remote host leave a
   trail that a human reviewer can audit after the fact.
5. **Intermittent-connectivity tolerant.** If the shared directory is
   offline, jobs queue. When it is back, they flow.

## 3. Core protocol (file-RPC)

Key rules below.

### 3.1 Directory layout

```
<shared-dir>/
+-- inbox/
|   +-- dispatch.txt          responder identity + capabilities
|   +-- 1/                    lane 1 (default; always present)
|   |   +-- <stem>.<ext>      pending job file(s)
|   |   +-- status.txt        per-lane heartbeat + state + queue depth
|   +-- 2/                    lane 2 (present when lane_count >= 2)
|       +-- ...
+-- outbox/
|   +-- 1/
|   |   +-- <stem>.result     exit code, timing, byte counts, flags
|   |   +-- <stem>.stdout     captured stdout (capped at 100 MB)
|   |   +-- <stem>.stderr     captured stderr (capped at 100 MB)
|   +-- 2/
|       +-- ...
+-- cancel/
|   +-- 1/
|   |   +-- <stem>            empty sentinel file; "cancel this job"
|   +-- 2/
+-- log/
|   +-- 1/
|   |   +-- <stem>.<ext>.script  archived worker script
|   |   +-- <stem>.<ext>         original job file
|   +-- 2/
+-- STOP                      global sentinel: responder exits cleanly
+-- PAUSE                     global sentinel: all lanes pause
+-- RESTART                   global sentinel: supervisor relaunches
+-- outpost.log               human-readable event log
```

**Directory invariants:**

- The responder creates lane directories up to `lane_count`. Files
  in lane subdirectories are lane-scoped; the lane number is
  authoritative.
- The default lane is `1`. It always exists. Submitters that do not
  specify a lane target lane 1.
- `inbox/dispatch.txt` is written at responder startup. The
  responder watches `outpost.toml` and rewrites `dispatch.txt` when
  the config changes (interpreter paths, tools, description, tags).
  Fields captured once at startup and frozen until full restart:
  platform os/arch, cwd, cwd_source, comspec, responder_name,
  lane_count, pid, started.
- `inbox/<N>/status.txt` is rewritten atomically every heartbeat
  (~2s) by the lane's own dispatcher goroutine. No cross-lane mutex
  coordination.
- Non-numeric subdirectories under `inbox/`, `outbox/`, `cancel/`,
  `log/` are ignored and reserved.

Transport-level details (how the shared-dir is reached) are plugin
concerns; the protocol above is the same regardless.

### 3.2 Job identity (stem)

Every job has a unique *stem*:

```
YYYYMMDD_HHMMSS_uuuuuu-<label>
```

- 22-character microsecond-resolution timestamp prefix:
  8 (date) + `_` + 6 (time) + `_` + 6 (microseconds).
- Hyphen separator.
- Label matching `[a-z0-9_-]`, max length 64.

Alphabetic sort equals chronological dispatch order regardless of
timezone or clock skew. The microsecond field prevents collision for
bursts submitted in the same turn. The strict charset avoids
case-collision hazards on NTFS and default-APFS (which are
case-insensitive).

### 3.3 Job submission

Writing `inbox/<lane>/<stem>.<ext>` submits a job. Writes MUST be
atomic (write to `<stem>.<ext>.tmp` then rename) so the responder
never observes a half-written file.

Extension determines dispatch:

| Extension | Dispatched via |
|---|---|
| `.sh` | `sh` or `bash` (Unix) |
| `.ps1` | `pwsh` or `powershell.exe` |
| `.py` | `python3` (or configured Python) |
| `.cmd` / `.bat` | `cmd.exe` (Windows) |
| `.btm` | `tcc.exe` (Windows, requires TCC v36+) |

The responder ships a compiled-in dispatch table. The operator
may override per-extension interpreter paths via `outpost.toml`
(see §7). The resolved dispatch table is advertised in
`inbox/dispatch.txt` so submitters can discover it.

### 3.4 Per-stem timeout override

A worker script may override the default per-job timeout via a
comment in its first 10 lines. The comment syntax varies by
extension:

| Extension | Header syntax |
|---|---|
| `.sh`, `.py` | `# timeout=N` |
| `.ps1` | `# timeout=N` |
| `.cmd`, `.bat` | `REM timeout=N` |
| `.btm` | `:: timeout=N` |

`N` is seconds. The responder scans for the first matching line
during dispatch and uses that value. Values outside `[1, 86400]` are
rejected as invalid; the job fails with `exit=125` (dispatch error).

### 3.5 Cancel

A submitter requests cancellation by creating an empty file at
`cancel/<lane>/<stem>`. Responder behaviour:

- **Before dispatch (job still in `inbox/`):** skip dispatch, write
  `.result` with `exit=126`, `cancelled=1`, archive the inbox file
  to `log/`, remove the cancel sentinel.
- **During dispatch (job is the lane's `busy_stem`):** the cancel
  watcher signals the dispatcher, which kills the worker's process
  tree using the same machinery as timeout. `.result` carries
  `exit=126`, `cancelled=1`.
- **After completion:** stale cancel sentinels (created after the
  job already finished) are cleaned up during the retention sweep.

### 3.6 Result files

Per job, the responder writes `outbox/<lane>/<stem>.result`,
`<stem>.stdout`, and `<stem>.stderr` atomically (write-to-temp then
rename). `.result` is a `key=value` text file:

```
stem=<stem>
lane=<int>
ext=<extension>
label=<extracted from stem>
exit=<worker exit code, or 124 timeout, 125 dispatch error, 126 cancel>
timeout=0|1
cancelled=0|1
started=<yyyy-mm-dd.HH:MM:SS.mmm>
finished=<yyyy-mm-dd.HH:MM:SS.mmm>
stdout_bytes=<int, capped at 100MB>
stderr_bytes=<int, capped at 100MB>
stdout_truncated=0|1
stderr_truncated=0|1
```

The stem itself contains a UTC timestamp prefix (see §3.3), so
callers that need the job's wall-clock time extract it from
`stem` rather than a separate field.

### 3.7 Output capture caps

The responder captures at most 100 MB per stream per job. Excess
output is discarded. The `stdout_truncated` and `stderr_truncated`
flags in `.result` inform the submitter when truncation occurred.
The worker continues running after the cap is hit -- truncation does
not kill the job. A worker that needs more than 100 MB of captured
output should redirect within its own script.

### 3.8 Dispatch advertisement (`inbox/dispatch.txt`)

Written at responder startup, atomically. Rewritten when the
responder detects `outpost.toml` has changed on disk (interpreter
paths, tools, description, tags — see §3.1). Key=value format:

```
protocol_version=1
pid=<int>
started=<yyyy-mm-dd.HH:MM:SS.uuu>
responder_name=<operator-assigned id>     # optional; omitted when unset
lane_count=<int>
dispatch.order=<ext1>,<ext2>,...
dispatch.<ext>=<absolute path to interpreter>
```

**`responder_name`** is set by the operator to distinguish one
responder from another when a submitter (or a human) would otherwise
have trouble telling them apart (identical OS hostnames, cloned VMs,
multiple responders on one host serving different shared-dirs). It is
optional and additive -- submitters that don't care simply ignore it;
submitters that care can verify "I expected `vm-win11-a`; did I reach
`vm-win11-a`?" against the value advertised here.

Resolution precedence on `outpost run` (highest wins): `--name <id>`
flag, `OUTPOST_NAME` env var, `[responder] name = "..."` in
`outpost.toml`, `os.Hostname()` fallback. Empty string is a valid
value meaning "no identity advertised."

`dispatch.order` lists the extensions the responder can dispatch,
in operator preference order (from `outpost.toml` if configured,
otherwise detection order). Each `dispatch.<ext>` key gives the
resolved absolute path to the interpreter the responder will use.
An extension is listed in `dispatch.order` only if its interpreter
was found.

### 3.9 Per-lane status (`inbox/<N>/status.txt`)

Rewritten atomically every heartbeat (default 2s) by the lane's own
dispatcher goroutine. Key=value format:

```
lane=<int>
state=ready|idle|busy|paused|stopping|stopped
busy_stem=<stem, present only when state=busy>
queued=<int, count of pending jobs in this lane>
message=<free text>
last_heartbeat=<yyyy-mm-dd.HH:MM:SS.uuu>
```

`queued` is the count of dispatchable files in `inbox/<N>/` at the
time of the heartbeat write (excluding `status.txt` itself and
`.tmp` files in flight). Submitters use it to plan lane selection
without having to list the inbox themselves.

Liveness check: `age(last_heartbeat) < 10s && state != stopped`.

### 3.10 Sentinels

Global (at shared-dir root; present/absent, content not read):

- `STOP` -- responder exits cleanly on next poll.
- `PAUSE` -- all lanes heartbeat `state=paused` but do not dispatch.
- `RESTART` -- supervisor relaunches the responder after the
  responder exits with code 75. See "Deploying outpost as a
  service" in the README for supervisor patterns (systemd,
  launchd, Windows, portable shell loop).

### 3.11 Retention

At startup, the responder deletes files in `log/<N>/` and
`outbox/<N>/` whose filename's `YYYYMMDD` prefix is older than the
configured retention period (default 7 days). Cancel sentinels in
`cancel/<N>/` older than the retention period are also deleted.
`inbox/` is never touched by automatic cleanup.

Retention runs once per launch. Longer retention requires
configuration; shorter requires explicit cleanup via `outpost clean`.

## 4. Architecture (Go)

### 4.1 Public library API

outpost exposes a public Go API under `pkg/outpost/...`, decomposed
along volatility axes: transport, dispatcher, authenticator, event
sink. Each has its own interface; the shipped implementations live
behind them.

Package layout:

```
pkg/outpost/
+-- outpost.go          top-level types: Job, Result, Lane, ExitCode constants
+-- protocol/           wire-format constants, field names, sentinel names
+-- stem/               StemGenerator interface + default impl; Parse/Validate
+-- capability/         Capabilities, Dispatch, Status structs
+-- transport/          Transport interface; Submitter + Responder sub-interfaces
|   +-- file/           file-RPC implementation
+-- dispatcher/         Dispatcher interface
|   +-- subprocess/     bare-subprocess implementation (with process-tree kill)
+-- auth/               Authenticator interface (no-op impl for file)
+-- events/             EventSink interface + file-log impl
+-- client/             Target (primary single-target API) + Client (multi-target)
+-- responder/          Responder type (embeddable daemon)

cmd/outpost/           Cobra CLI: one file per subcommand (target, client,
                       submit, status, run, cancel, stop, pause, resume,
                       clean, setup, doctor, version)

internal/
+-- config/             outpost.toml load + merge precedence
+-- fsatomic/           atomic write helper (used by transport/file)
+-- probe/              interpreter detection (used by `outpost setup`)
+-- platform/           platform-specific shims (process-tree kill,
                        CWD source resolution)
```

The CLI in `cmd/outpost/` consumes `pkg/outpost/...` -- the binary
dogfoods the library.

### 4.2 Tier-1 interfaces

**`Transport`** -- bidirectional connection to one responder. Split
into `Submitter` and `Responder` sub-interfaces so that submit-only
consumers don't pull in responder-side methods. File-RPC is the
implementation shipped. Optional extension interfaces (`Watcher`,
`Streamer`) let transports advertise push/streaming capabilities;
consumers type-assert to detect support.

**`Dispatcher`** -- spawns and monitors a worker given a job.
Synchronous signature: `Run(ctx, Job) (Result, error)`. Cancellation
is via `ctx`. The shipped implementation is bare-subprocess.

**`Authenticator`** -- proves submitter identity to the responder
where the transport requires it. No-op shipped (file-RPC relies on
filesystem ACLs).

**`EventSink`** -- where structured events flow. File-log
implementation shipped.

### 4.3 Responder internals

```
+--------------+   +--------------+   +------------+   +--------+
|  supervisor  |-->|  responder   |-->|  lane 1    |-->| worker |
|  (launch.btm |   |  (process)   |   |  goroutine |   |        |
|   or         |   |              |   +------------+   +--------+
|   systemd)   |   |              |        ^
+--------------+   |              |        | per-lane status.txt
                   |              |--> lane 2 goroutine (if any)
                   |              |        ^
                   |              |        | per-lane status.txt
                   +--------------+
```

- One responder process owns all lanes.
- Each lane has its own dispatcher goroutine that: polls its
  inbox, writes its own `status.txt` heartbeat, dispatches one job
  at a time to the configured `Dispatcher`, captures output, writes
  `.result` / `.stdout` / `.stderr` atomically.
- Lanes share no mutable state, so no cross-lane mutex is needed.
- Global sentinel detection runs in the main responder goroutine
  and broadcasts state changes to lane goroutines via channels.

### 4.4 Cross-platform process-tree kill

The one genuinely hard platform concern:

- **Windows**: the subprocess dispatcher wraps the worker in a Job
  Object (`CreateJobObject`, `AssignProcessToJobObject`) with
  `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`. On timeout or cancel,
  terminating the job object kills every descendant.
- **Unix (Linux, macOS, BSD)**: `Setsid: true` in `SysProcAttr`
  creates a new process group. On timeout or cancel,
  `syscall.Kill(-pgid, SIGKILL)` signals the group.

These branches live in `internal/platform/platform_windows.go` and
`internal/platform/platform_unix.go` behind a common interface used
by `pkg/outpost/dispatcher/subprocess`.

### 4.5 Client and Target

`pkg/outpost/client` exposes two types:

- `Target` -- handle to one responder via one transport.
  `target.Submit(ctx, job)` is the primary single-target API.
- `Client` -- orchestrator composing N named Targets. Used when
  fan-out, routing policy, or failover across multiple responders
  is wanted. `client.Target("vm-tests").Submit(ctx, job)` routes
  to the named Target.

Single-target CLI commands use `Target`. Multi-target use cases
compose via `Client`.

### 4.6 Configuration

- **Flags** on `outpost run`: `--dir <shared-dir>`, `--poll <dur>`,
  `--timeout <dur>` (default per-job), `--retain-days <n>`,
  `--name <responder-id>`, `--config <path>`.
- **Config file** (`outpost.toml`):
  - Unix / macOS: `$XDG_CONFIG_HOME/outpost/outpost.toml`
    (default `~/.config/outpost/outpost.toml`)
  - Windows: `%APPDATA%\outpost\outpost.toml`
  - Override with `--config <path>`.
  - Schema:
    ```toml
    [responder]
    name = "vm-win11-a"    # optional; falls through to OS hostname

    [dispatch]
    enabled = ["py", "sh", "btm"]   # ordered: first = preferred

    [dispatch.path]
    py  = "C:/Python313/python.exe"
    sh  = "C:/Program Files/Git/usr/bin/bash.exe"
    btm = "C:/Program Files/JPSoft/TCMD36/tcc.exe"
    ```
- **Precedence:** flags override config; config overrides
  compiled-in defaults. `outpost.toml` is responder-local and never
  travels through the shared directory; the *derived* dispatch
  table is advertised in `inbox/dispatch.txt` for submitters to
  read.

### 4.7 `outpost setup`

A one-shot probe command that detects which interpreters are
installed on the target and writes `outpost.toml`.

```
outpost setup                         # probe, print report, write outpost.toml
outpost setup --check                 # probe and print only (no write)
outpost setup --write <path>          # write to a specific path
outpost setup --force                 # overwrite existing config
```

The detection table covers (at minimum): `sh`, `bash`, `zsh`,
`pwsh`, `powershell`, `cmd`, `python3`, `python`, `py` (Windows
launcher), `tcc`, `node`, `deno`, `ruby`, `perl`, `php`, `lua`.
Detection resolves the binary on PATH and captures `--version`
output when available for the report.

## 5. Security model

Outpost is not a security boundary. It is a message carrier with good
defaults.

- **The responder executes whatever lands in inbox.** Workers are
  arbitrary scripts. They run with the full privilege of the user
  running `outpost run`.
- **The submitter is the policy enforcement point.** The AI agent (or
  human) submitting jobs must scope what it submits. Outpost does not
  sandbox, sign, verify, or authenticate jobs.
- **Filesystem ACLs are the access control.** Whatever controls who
  can write to the shared directory controls who can submit jobs.
- **Audit is free.** Every job is a durable file in `log/`; every
  result is a durable file in `outbox/`. Human review after the fact
  is possible.

Recommended guidance for users:

- Run the responder under a restricted service account where possible.
- Limit write access to the shared directory.
- For higher-security use cases, wrap workers in a sandbox (firejail,
  AppContainer, etc.) at the script level; outpost does not do this
  for you.

Transports that authenticate carry their own auth models. The
`Authenticator` interface is in place so those can plug in without
responder changes; only the no-op implementation is shipped.

## 6. Versioning

- **Binary** follows SemVer (`outpost 1.0.0`).
- **Library** (`pkg/outpost/...`) versions independently, starting at
  `v0.x.y`.
- **Protocol version** is a separate integer in `inbox/dispatch.txt`
  (`protocol_version=1`). A submitter checks the responder's
  protocol version and refuses to submit if the versions are
  incompatible.
- Protocol-breaking changes bump the protocol version. Binary-only
  changes do not.

## 7. Cross-platform targets

Tier 1 (officially supported, CI-tested, release binaries shipped):

- `linux/amd64`, `linux/arm64`
- `darwin/amd64`, `darwin/arm64`
- `windows/amd64`, `windows/arm64`

Tier 2 (builds, not tested):

- `freebsd/amd64`, `openbsd/amd64`

CI runs `go build` + `go test -race ./...` on native runners for
every tier-1 target. Tier-2 targets get build-only cross-compile
checks.

## 8. Testing and release

### 8.1 Test strategy

| Level | Location | What |
|---|---|---|
| Unit | `*_test.go` next to code | Pure logic: stem generation/parsing, config merge, fsatomic helpers (with fault injection), protocol serde round-trips |
| Integration | `internal/...` + top-level `_test.go` | Full poll loop in `t.TempDir()`: spawn a goroutine responder, submit a sleep job, assert `.result` appears |
| Platform-specific | `*_test.go` with build tags | Process-tree kill on timeout |
| End-to-end | Manual, pre-release gate | macOS <-> Linux <-> Windows against a shared directory, exercising the full submit/execute/result round-trip for each supported script extension |

Defaults: table-driven tests where inputs are enumerable;
`t.TempDir()` for all filesystem tests; race detector on every CI
run; >80% line coverage on `internal/` and `pkg/outpost/` as a
signal (not a hard gate).

### 8.2 Release signing

Binaries ship unsigned at the OS level with:

- SHA256 checksums in the release notes.
- `cosign` signatures (Sigstore/Fulcio) for supply-chain
  verification.
- GitHub Attestations for provenance.

Binaries are not signed with Apple Developer ID or Windows
Authenticode. README documents the Gatekeeper and SmartScreen
workarounds for direct downloads.

## 9. Multi-target composition

A submitter may need to reach several outposts simultaneously --
e.g., a Windows VM over SMB, a build farm over a cloud-sync folder
(iCloud Drive, Dropbox, Syncthing), an air-gapped machine via a USB
drop. `pkg/outpost/client` supports this natively.

### 9.1 Submitter-side target registry (`targets.toml`)

Each submitter host maintains a curated list of known targets in
`targets.toml`:

- Unix / macOS: `$XDG_CONFIG_HOME/outpost/targets.toml`
- Windows: `%APPDATA%\outpost\targets.toml`
- Override with `--targets <path>` or `OUTPOST_TARGETS` env.

The file is hand-edited: outpost cannot discover SMB shares,
cloud-sync folders, or air-gapped drop points on its own.

```toml
default = "vm-tests"   # optional; used when no --target flag is given

[target.vm-tests]
transport = "file"
path      = "/Volumes/vm-tests/outpost"

[target.build-farm]
transport = "file"
path      = "/Volumes/build-farm/outpost"

[target.airgap]
transport = "file"
path      = "/mnt/usb-drop/outpost"
```

The current build accepts only `transport = "file"`; unknown
transports produce a clear error identifying the malformed target
entry.

### 9.2 Client API

```go
import (
    "github.com/Tim-Butterfield/outpost/pkg/outpost"
    "github.com/Tim-Butterfield/outpost/pkg/outpost/client"
    "github.com/Tim-Butterfield/outpost/pkg/outpost/transport/file"
)

// From the default location (XDG/APPDATA).
c, err := client.LoadClient()

// From a specific file.
c, err := client.LoadClientFromFile("/path/to/targets.toml")

// Programmatic (for library consumers that don't use a file).
c := client.NewClient(
    client.WithTarget("vm-tests", file.New("/Volumes/vm-share")),
    client.WithTarget("airgap",   file.New("/mnt/usb/shared")),
)

target, err := c.TargetOrError("vm-tests")
handle, err := target.Submit(ctx, outpost.Job{Lane: 1, Ext: "sh", Content: script})
result, err := handle.Wait(ctx)
```

Each `Target` owns its own transport, authenticator, and capability
cache. `Client` routes by explicit name.

### 9.3 Availability discovery

Not every target is reachable at every moment -- VMs sleep, shares
unmount, responders crash. The submitter determines the live set
via parallel probes:

```go
probes := client.TargetProbes(ctx)          // all configured targets
available := client.AvailableTargets(ctx)   // filter to .Available == true
```

A probe reads the target's `dispatch.txt` and each lane's
`status.txt` with a short timeout (default 5s). Parallel execution
means total wall time is bounded by the single per-target timeout
regardless of how many targets are configured. An idle responder
still produces fresh heartbeats every `PollInterval` (~2s), so
presence alone is the availability signal -- no active jobs
required.

### 9.4 Responder-name collisions

Non-empty `responder_name` values advertised in `dispatch.txt` MUST
be unique across all configured targets. After bulk probing,
`Client.TargetProbes` groups probes by `ResponderName`; any group of
size > 1 produces a collision. Every probe in the group is marked
`Available=false` with `CollisionWith` naming the other offenders
and `Err=ErrResponderNameCollision`.

Collisions indicate one of:

- Two `[target.*]` entries in `targets.toml` accidentally pointing
  at the same shared-dir.
- Two physically distinct responders configured with the same
  `--name`.
- An OS-level mount remap that collapsed two paths onto one physical
  responder.

Submission to a collided target is refused; the library surfaces the
error and the operator is expected to fix the misconfiguration
(either by renaming one responder via `--name` / `outpost.toml` or by
correcting the `targets.toml` entry).

Targets that advertise no `responder_name` are never flagged for
collision; the "no identity asserted" case is allowed to coexist
with others.

