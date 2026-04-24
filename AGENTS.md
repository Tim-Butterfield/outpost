# AGENTS.md

**Audience:** AI agents using outpost as a tool to execute work on
remote hosts. Human contributors: see [README.md](README.md).

## Mental model

Outpost lets you run a script on a remote host by writing that
script into a shared directory. A responder process on the target
polls the directory, executes the script, and writes the result
back. You invoke `outpost submit` as the client; outpost handles
the round-trip.

No daemon, no port, no SSH. Any filesystem both sides can see
(SMB, Syncthing, Dropbox, etc.) is the transport.

Every command is synchronous-by-default: `outpost submit` blocks
until the result is ready and propagates the worker's exit code,
stdout, and stderr.

## When to use outpost

- You need to execute on a specific host (Windows-only tooling on
  Windows, macOS-signed builds on macOS, etc.)
- The host runs a responder you can reach via a shared directory
- The job completes in a bounded time and produces text output

## When NOT to use outpost

- The command would run fine locally — outpost adds ~1–3s of
  round-trip overhead per job
- The tool is interactive (needs TTY, prompts for input) — workers
  get `/dev/null` on stdin
- You need real-time streaming — stdout/stderr are captured and
  returned at job end, not streamed
- Stdout will exceed 100 MB — output is truncated with a flag set
- You need sub-second latency — polling cadence is typically 2s

## Commands (reference)

Commands you'll use frequently:

| Command | Purpose |
|---|---|
| `outpost status` | Summary of all registered targets |
| `outpost status --target <id>` | Detailed view of one target |
| `outpost status --json` | Same, as JSON (for parsing) |
| `outpost submit --target <id> <script-file>` | Run a script on a target, wait for result |
| `outpost submit --target <id> --no-wait <script-file>` | Submit; prints the stem; don't wait |
| `outpost submit --target <id> --timeout <dur> <script-file>` | Override per-job timeout (e.g. `30s`, `5m`) |
| `outpost submit --target <id> --lane <N> <script-file>` | Submit to a specific lane (default 1) |
| `outpost cancel --target <id> [--lane N] <stem>` | Cancel an in-flight job by stem |
| `outpost doctor` | Setup diagnostic; prints next-step commands if anything is off |

Commands that are usually operator-run, not agent-run:

| Command | Purpose |
|---|---|
| `outpost target init/start/list/clean` | Responder setup and management |
| `outpost client init/check/show/where` | Submitter-side `targets.toml` management |
| `outpost stop/pause/resume` | Responder lifecycle |
| `outpost setup` | Interpreter probe + write `outpost.toml` |
| `outpost run` | Low-level responder loop (target start wraps it) |

## Exit code dictionary

Every value you'll observe from `outpost submit`:

| Code | Meaning | Your response |
|---|---|---|
| 0 | Job completed successfully | Proceed |
| 1 | Generic failure — outpost error or worker exit 1 | Read stderr; retry if transient |
| 124 | Job hit its timeout | Increase `--timeout` or fix the script; don't retry with same timeout |
| 125 | Dispatch failure — responder has no interpreter for this extension | Check `outpost status --target <id> --json` for supported extensions; use a different extension or a different target |
| 126 | Job cancelled via `outpost cancel` | Expected when you initiated cancel |
| Other | Worker exit code passed through | Interpret in the context of what the script does |

## Recipes

### R1. Run a command and capture output

```sh
cat > /tmp/job.sh <<'EOF'
#!/bin/sh
uname -a
EOF
outpost submit --target linux-arm64 /tmp/job.sh
echo "exit=$?"
```

### R2. Run a .NET build on Windows from macOS

```sh
cat > /tmp/build.cmd <<'EOF'
@echo off
cd ..\MyProject
dotnet build --nologo --verbosity minimal
EOF
outpost submit --target win_11 --timeout 5m /tmp/build.cmd
```

The worker's CWD is the nearest ancestor `.git/` of the target's
share directory. Relative paths like `..\MyProject` resolve from
there, making sibling-repo access predictable.

### R3. Submit in background; cancel if it runs too long

```sh
stem=$(outpost submit --target linux-arm64 --no-wait /tmp/long.sh)
# Poll for completion or work on other things
sleep 60
# Still going? Cancel.
outpost cancel --target linux-arm64 "$stem"
# Worker receives SIGKILL on Unix / TerminateJobObject on Windows;
# result reports exit code 126.
```

### R4. Discover which targets have a specific tool

```sh
outpost status --json | jq -r '
  [ .[] | select(.Available) | select(.Tools.dotnet) | .Name ] | .[]
'
```

`Tools` maps tool-name → version-string for each target; auto-probed
at `outpost target init`. A job that needs `dotnet` should target one
of the returned names.

### R5. Check target readiness before submitting

```sh
status=$(outpost status --target linux-arm64 --json | jq -r '.Status')
case "$status" in
  AVAILABLE) outpost submit --target linux-arm64 /tmp/job.sh ;;
  PAUSED)    echo "target is paused; work will queue until resumed" ;;
  STALE)     echo "target heartbeat gone stale; responder may be down" ;;
  UNREACHABLE) echo "cannot read dispatch.txt; share mount issue" ;;
  COLLISION) echo "two targets advertise same responder_name; operator intervention needed" ;;
esac
```

### R6. Chain two jobs using the shared filesystem

Worker CWD is a real directory visible to the submitter. Artifacts
land there; the submitter reads them back directly.

```sh
# Job A writes an artifact
cat > /tmp/build.sh <<'EOF'
mkdir -p artifacts
echo "payload" > artifacts/result.txt
EOF
outpost submit --target linux-arm64 /tmp/build.sh || exit 1

# Submitter reads the artifact via the same path
cat artifacts/result.txt
```

### R7. Per-job timeout

Two equivalent ways:

```sh
# CLI flag
outpost submit --target linux-arm64 --timeout 30s /tmp/job.sh
```

```sh
# In-script header (first 10 lines, comment char matches the ext)
cat > /tmp/job.sh <<'EOF'
# timeout=30
do-some-work
EOF
outpost submit --target linux-arm64 /tmp/job.sh
```

Header comment characters by extension: `sh` / `zsh` / `py` / `ps1`
use `#`; `cmd` / `bat` use `REM`; `btm` uses `::`. Integer seconds
only (no duration syntax). Header wins when both are set.

## Gotchas

- **Dispatch table refreshes from `outpost.toml`.** The responder
  watches `outpost.toml` for changes and auto-reloads within about
  5 seconds. The full install-then-use flow is: submit a job that
  installs a tool → run `outpost target init --force` to refresh
  the config → wait ~5s for the responder to pick it up → submit
  a job using the new extension. No responder restart needed.
  Look for `[INFO] config reloaded from outpost.toml change` in
  the responder's event log to confirm the reload fired.

- **Workers have no TTY.** `stdin` is `/dev/null`. Any tool that
  detects a non-TTY and behaves differently (most do) is affected.
  Tools that auto-disable pagers/colors: fine. Tools that prompt
  for confirmation: hang or fail. Use non-interactive flags
  explicitly where available (`--yes`, `--no-input`, etc.).

- **Target ID charset is strict.** IDs must match `[a-z0-9_-]{1,64}`.
  Folder names, CLI labels, and advertised `responder_name` are all
  the same string. Mixed case like `macOS` is rejected at init time.

- **`responder_name` may differ from target ID.** The ID is what
  the submitter types (`--target <id>`); `responder_name` is what
  the responder advertises in `dispatch.txt` and may be set
  separately by the operator. Don't rely on them being equal.

- **`dispatch.txt` auto-refreshes from `outpost.toml`.** Interpreter
  paths, tools, description, and tags are re-read when
  `outpost.toml`'s mtime changes. Platform, CWD, `%COMSPEC%`, and
  responder name are captured once at startup and don't update
  until a full restart.

- **Submit reads a file or stdin.** `outpost submit /path/to/script.sh`
  reads from a file; `outpost submit -` reads the script body
  from stdin. When reading from stdin, pass `--ext <ext>` so
  outpost knows which interpreter to dispatch with.

- **Extension drives interpreter.** The file's extension (`.sh`,
  `.py`, `.cmd`, etc.) selects the interpreter via the target's
  dispatch table. Rename your temp file accordingly, or pass
  `--ext` to override.

- **Worker CWD is the nearest `.git/` ancestor.** Scripts that
  use relative paths get reproducible results only when the target
  was started inside the expected repo tree. `--workdir` overrides
  at `target start` time (operator setting).

- **Multi-lane targets run lanes in parallel.** A target with
  `lane_count=2` accepts two concurrent jobs. Default submit lane
  is 1; use `--lane N` to target a specific lane.

- **Long-running jobs are fine.** The responder emits a background
  heartbeat during execution, so jobs >10s don't false-flip the
  target to STALE.

- **Submit is synchronous by default.** `outpost submit foo.sh`
  blocks until the worker exits. If that's not what you want,
  use `--no-wait` and poll separately. Ctrl+C on a waiting submit
  abandons the wait but does NOT cancel the remote job — use
  `outpost cancel` explicitly.

- **Large output is truncated.** stdout/stderr caps at 100 MB per
  stream. If exceeded, the result's `stdout_truncated` /
  `stderr_truncated` flags are set and the tail is lost. Pipe
  heavy output through `| tail -N` in your script, or write it
  to a file on the shared directory instead.

## Diagnostics

When something's off, run `outpost doctor` first. It correlates:

- Binary version
- Resolved `targets.toml` path and whether it loads
- Which targets are initialized (have `outpost.toml`) vs. running
  (have fresh `dispatch.txt` + status heartbeats)
- Any scan warnings
- Suggested next-step commands

Typical failures and reads:

| Doctor output | What it means |
|---|---|
| `Target root does not exist` | No targets have been init'd in the resolved root; or `OUTPOST_TARGETS` points somewhere else |
| `targets.toml not found` | Operator hasn't run `outpost client init` |
| `No targets registered` | Registry is empty; init targets or add explicit entries |
| `NOT INITIALIZED` per-target | `outpost.toml` missing; operator needs `target init <id>` |
| `NOT STARTED` per-target | `dispatch.txt` missing; operator needs `target start <id>` |
| `STALE` per-target | Last heartbeat is beyond the freshness window; responder may have crashed |
| `COLLISION` per-target | Two targets advertise the same `responder_name`; operator intervention needed |

## One-line safety rules

1. **Don't hang on stdin.** Use non-interactive flags in every tool you invoke.
2. **Pick a bounded timeout** for every submit. Default is the responder's `--timeout` (typically 60s); be explicit for long work.
3. **Check exit codes.** Don't treat any non-zero exit as a "probably fine" outcome.
4. **Verify target availability** before heavy work (recipe R5).
5. **Capture artifacts to the shared filesystem**, not to stdout if they're non-trivial size.
6. **Don't assume** installed tools become dispatchable in the same session.
