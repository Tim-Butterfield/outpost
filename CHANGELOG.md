# Changelog

All notable changes to outpost are documented in this file.

The format is loosely based on [Keep a Changelog](https://keepachangelog.com),
and this project adheres to [Semantic Versioning](https://semver.org).

The library under `pkg/outpost/` versions independently of the
binary, starting at `v0.x.y`.

## [1.0.1] -- 2026-04-28

Packaging fix release. No code changes.

### Fixed

- Release archives now include `AGENTS.md` (AI usage
  instructions) and `CHANGELOG.md` alongside `LICENSE`,
  `README.md`, and `docs/`. The v1.0.0 archives were missing
  both files.

## [1.0.0] -- 2026-04-24

First public release. File-RPC bridge for remote AI execution.

### Added

- `outpost run` -- lane-first responder serving a shared directory.
  Lane-aware protocol throughout; multi-lane (`lane_count > 1`)
  exercised end-to-end in the cross-host test harness.
- `outpost target <init|start|list|clean>` -- convention-based
  responder setup, replacing the init/start shell scripts of
  earlier drafts. `init` runs the interpreter probe + smoke
  verification and writes `outpost.toml`; `start` launches the
  responder with convention paths and auto-ID resolution via the
  `[platform]` section in each target's `outpost.toml`.
- `outpost client <init|where|show|check>` -- submitter-side
  `targets.toml` management. `init` writes a registry configured
  for auto-discovery; `check` enumerates explicit + scanned
  entries plus any scan warnings.
- `outpost submit` -- client submission with `--target <name>`
  (via `targets.toml`) or `--dir <path>` (ad-hoc). Waits for
  result by default; `--no-wait` and `--json` modes available.
  `--timeout <dur>` propagates as a script-header comment via
  `protocol.FormatTimeoutHeader`, so the responder-side parser
  honors it.
- `outpost status` -- three modes: summary table across all
  configured targets, single-target detail, `--json` at either
  scope. Detects and flags responder-name collisions.
- `outpost doctor` -- correlates env, targets.toml, per-target
  init state, and per-target runtime state into one diagnostic
  with suggested next-step commands.
- `outpost stop` / `pause` / `resume` / `cancel` -- sentinel-based
  lifecycle control. `cancel <stem>` targets a specific in-flight
  job (exits the worker with code 126 / ExitCodeCancelled).
- `outpost clean` -- force retention sweep.
- `outpost setup` -- probe installed interpreters and write
  `outpost.toml`. Includes a post-probe smoke-execution pass:
  each detected interpreter must also successfully run a trivial
  sentinel script, not just answer `--version`.
- `outpost version` -- binary and protocol version.
- Submitter-side target registry via `targets.toml` at
  `$XDG_CONFIG_HOME/outpost/` or `%APPDATA%\outpost\`. Supports a
  `scan = [...]` directive for auto-discovery of targets by
  directory walk. Single-target registries get an implicit default
  so `outpost submit` works without `--target`. Explicit entries
  override same-named scan results.
- Responder identity via operator-assigned `--name`, env var,
  or `outpost.toml`; advertised in `dispatch.txt`.
- `dispatch.txt` additionally advertises the responder's CWD,
  a cross-host CWD identifier (UNC on Windows, mount source on
  Unix for remote FS types), and Windows `%COMSPEC%`. Submitters
  can use these to correlate targets serving the same backing
  store and reason about relative paths in jobs.
- Responder-side platform check: at startup, `[platform]` in
  `outpost.toml` is verified against `runtime.GOOS/GOARCH`; a
  mismatch fails fast rather than crashing later at dispatcher
  exec. Guards against starting a cross-compiled target config
  on the wrong host.
- Windows COMSPEC normalization: the responder detects if
  `%COMSPEC%` points at a cmd.exe replacement (TCC, 4NT) and
  overrides it in-process to the canonical
  `%SystemRoot%\System32\cmd.exe`, so pipes and `for /f`
  inside worker scripts don't fork through the replacement.
- Responder-side long-job heartbeat: a background ticker
  re-stamps `status.txt` every `poll_interval` while a worker
  is running, so jobs that outlast the probe's 10s freshness
  window don't false-flip the target to STALE.
- Responder-side `outpost.toml` auto-reload: a background
  watcher polls the config file's mtime and, on change,
  re-runs the interpreter probe and atomically swaps the
  dispatcher's path table and `dispatch.txt`. Enables the
  install-then-use workflow (`brew install lua` → `outpost
  target init --force` → submit `.lua`) without restarting
  the responder. Failed reloads keep the previous state with
  a WARN event.
- Paused lanes continue to advertise `queued` counts (previously
  stuck at 0 while paused), matching submitter expectation that
  work accumulated during pause is visible.
- Availability discovery via parallel `Client.TargetProbes`;
  responder-name collision detection.
- Cross-platform process-tree kill on timeout or cancel
  (JobObject on Windows, setsid on Unix).
- Cancel via `cancel/<lane>/<stem>` sentinel; exit code 126.
- Per-stem timeout override via script-header comments.
- Bounded stdout/stderr capture at 100 MB per stream, with
  `stdout_truncated` / `stderr_truncated` flags.
- Atomic file writes via tmp + fsync + rename, with SMB-aware
  bounded retry on transient errors.
- Public Go library under `pkg/outpost/` decomposed by
  IDesign-style volatility axes:
  - `transport` (interfaces + `file` implementation).
  - `dispatcher` (interface + `subprocess` implementation).
  - `auth` (interface + `NoOp`).
  - `events` (interface + `FileLog`).
  - `client` (Target, Client, SubmitHandle, Probe).
  - `responder` (lane-first daemon).
- CI matrix: native runners on all six tier-1 targets; tier-2
  build-only.
- Release artifacts: multi-arch archives, SHA256 checksums,
  cosign signatures on checksums, SBOMs (syft), GitHub
  Attestations. Users install via direct download from the
  GitHub Releases page.

### Notes

- OS-level code signing is not included. macOS Gatekeeper /
  Windows SmartScreen workarounds are documented in the README.
- Multi-lane execution (`lane_count > 1`) is supported. Defaults
  to 1; override via `--lanes N` flag or `[responder] lane_count
  = N` in `outpost.toml` (flag wins).
- The bundled `file` transport is the only transport built in.
  `pkg/outpost/transport.Transport` is a public interface that
  third parties can implement.
