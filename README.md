# outpost

**A file-RPC bridge for remote AI execution.** Let an AI agent
execute work on hosts where it cannot natively run, and observe the
results — without opening a port, installing a daemon, or requiring
SSH.

Read the original framing at
<https://timbutterfield.com/post/running-ai-where-it-does-not-exist/>.

## Status

v1.0.0 — first public release. Download from the
[GitHub Releases](https://github.com/Tim-Butterfield/outpost/releases)
page.

## How it works in one paragraph

A **responder** on each target host polls a **shared directory**
(SMB, Syncthing, iCloud Drive, Dropbox, or any other file-sync
transport) for jobs, executes them locally, and writes results back
to the same directory. A **submitter** drops a script into that
directory and waits for the result to appear. No listening daemon.
No open port. Any filesystem both sides can see is enough.

## Installation

Download the archive for your OS and architecture from the
[GitHub Releases](https://github.com/Tim-Butterfield/outpost/releases)
page, extract it, and put `outpost` on your `PATH`.

The binaries are not signed with an Apple Developer ID or Windows
Authenticode certificate, so your OS will warn on first run:

- **macOS (Gatekeeper).** Either clear the quarantine attribute:

  ```sh
  xattr -d com.apple.quarantine /path/to/outpost
  ```

  Or right-click `outpost` in Finder, choose **Open**, and confirm the
  prompt once — subsequent runs are unblocked.

- **Windows (SmartScreen).** On the "Windows protected your PC"
  dialog, click **More info** → **Run anyway**.

Supply-chain trust is via SHA256 checksums, cosign signatures on the
checksums file, and GitHub Attestations. See the release notes for
the `cosign verify-blob` invocation.

## Quick start

```sh
# On each responder host -- probe interpreters and write config.
# <id> defaults to <os>-<arch>; pass an explicit name only when
# you have multiple same-platform VMs against the same share.
outpost target init

# Start the responder (foreground; Ctrl+C to stop).
outpost target start --lanes 2

# On the submitter host -- generate a targets.toml pointed at the
# shared target root, then use the usual submit/status commands.
outpost client init
export OUTPOST_TARGETS=./targets/targets.toml

# Submit a job and wait for the result.
outpost submit --target linux-arm64 ./build.sh

# Non-blocking submit + later cancel.
stem=$(outpost submit --target linux-arm64 --no-wait ./long.sh)
outpost cancel --target linux-arm64 "$stem"

# Diagnostic: correlates env, registry, per-target state, and
# per-target runtime; emits suggested next-step commands.
outpost doctor
```

## Commands

| Command | Purpose |
|---|---|
| `outpost target init [id]` | Probe host, smoke-verify interpreters, write per-target `outpost.toml` |
| `outpost target start [id]` | Run the responder for a target (auto-resolves id from platform match) |
| `outpost target list` | List known targets under the target root |
| `outpost target clean <id>` | Remove a target's on-disk state |
| `outpost client init` | Write a `targets.toml` with auto-discovery scan |
| `outpost client where` | Print the platform-default `targets.toml` path |
| `outpost client show` | Print the stored `targets.toml` contents |
| `outpost client check` | Enumerate all targets (explicit + scanned) plus warnings |
| `outpost run` | Raw responder loop (used under the hood by `outpost target start`) |
| `outpost submit` | Submit a job to a target and (by default) wait for the result |
| `outpost status` | Summary across registered targets, or `--target X` detail |
| `outpost pause` / `resume` | Pause or resume job dispatch on a responder |
| `outpost cancel <stem>` | Cancel an in-flight job (worker exits with code 126) |
| `outpost stop` | Ask a responder to exit cleanly (STOP sentinel) |
| `outpost clean` | Force a retention sweep of old log/outbox/cancel entries |
| `outpost doctor` | Diagnose setup state and suggest next-step commands |
| `outpost setup` | Probe + write `outpost.toml` (standalone; `outpost target init` uses this internally) |
| `outpost version` | Print the binary version and protocol version |

## Using outpost as a Go library

The `outpost` CLI is a thin wrapper around the library under
`pkg/outpost/...`. Your own Go programs can embed the same pieces
directly — either to submit jobs programmatically or to run an
embedded responder.

Full API reference on [pkg.go.dev](https://pkg.go.dev/github.com/Tim-Butterfield/outpost/pkg/outpost).

Import path root:

```go
import "github.com/Tim-Butterfield/outpost/pkg/outpost"
```

### Submitting a job

Construct a `Client` pointed at one or more shared directories, then
submit a job and wait for the result:

```go
package main

import (
    "context"
    "fmt"
    "io"
    "time"

    "github.com/Tim-Butterfield/outpost/pkg/outpost"
    "github.com/Tim-Butterfield/outpost/pkg/outpost/client"
    "github.com/Tim-Butterfield/outpost/pkg/outpost/transport/file"
)

func main() {
    c := client.NewClient(
        client.WithTarget("linux-arm64", file.New("/mnt/share/targets/linux-arm64")),
    )

    target, err := c.TargetOrError("linux-arm64")
    if err != nil {
        panic(err)
    }

    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    handle, err := target.Submit(ctx, outpost.Job{
        Ext:     "sh",
        Content: []byte("#!/bin/sh\necho hello\n"),
        Timeout: 10 * time.Second,
    })
    if err != nil {
        panic(err)
    }

    result, err := handle.Wait(ctx)
    if err != nil {
        panic(err)
    }

    // Result has ExitCode, timing, and byte counts. Use the handle
    // to fetch the actual stdout/stderr bytes.
    stdout, err := handle.Stdout(ctx)
    if err != nil {
        panic(err)
    }
    defer stdout.Close()
    body, _ := io.ReadAll(stdout)
    fmt.Printf("exit=%d stdout=%q\n", result.ExitCode, body)
}
```

`SubmitHandle` also exposes `Cancel`, `Stdout`, `Stderr`, and
`WaitWithInterval`. Multi-target clients can use `c.TargetProbes`,
`c.AvailableTargets`, `c.TargetsWithTool`, etc. to pick a target at
run time.

### Embedding a responder

The responder is configured via `responder.Config` and run via
`responder.Run`. All architectural concerns (transport, dispatcher,
authenticator, event sink) are injected:

```go
package main

import (
    "context"
    "os"
    "time"

    "github.com/Tim-Butterfield/outpost/pkg/outpost/dispatcher/subprocess"
    "github.com/Tim-Butterfield/outpost/pkg/outpost/responder"
    "github.com/Tim-Butterfield/outpost/pkg/outpost/transport/file"
)

func main() {
    tp := file.New("/mnt/share/targets/linux-arm64")
    disp := subprocess.New(subprocess.Config{
        InterpreterPaths: map[string]string{
            "sh": "/bin/sh",
            "py": "/usr/bin/python3",
        },
        DefaultTimeout: 60 * time.Second,
    })

    r, err := responder.New(responder.Config{
        Transport:      tp,
        Dispatcher:     disp,
        LaneCount:      2,
        PollInterval:   2 * time.Second,
        DefaultTimeout: 60 * time.Second,
        PlatformOS:     "linux",
        PlatformArch:   "arm64",
    }, os.Getpid())
    if err != nil {
        panic(err)
    }

    if err := r.Run(context.Background()); err != nil {
        panic(err)
    }
}
```

The `transport.Transport`, `dispatcher.Dispatcher`, `auth.Authenticator`,
and `events.EventSink` interfaces are all public — substitute your
own implementations to wire outpost into a non-file transport, a
sandboxed executor, an auth gate, or a metrics sink.

## Deploying outpost as a service

For headless hosts you manage over SSH / RDP, run the responder
under a supervisor so it survives terminal disconnects, restarts
on request, and waits for network mounts before launching.

Outpost emits three well-known exit codes the supervisor branches
on:

| Exit | Meaning | Supervisor response |
|---|---|---|
| `0` | STOP sentinel (clean exit) | Exit the supervisor too |
| `75` | RESTART sentinel | Re-exec outpost immediately |
| `74` | Shared dir not available (mount not up yet) | Back off and retry |
| other | Crash or other error | Back off and retry |

The supervisor itself must live on **local** storage (not on the
shared network mount) so init systems can find it at boot before
the mount comes up. Outpost's binary + config + shared dir can all
live on the share.

### Linux (systemd)

`/etc/default/outpost`:

```sh
OUTPOST_BIN=/mnt/share/bin/linux-arm64/outpost
OUTPOST_ROOT=/mnt/share/targets
OUTPOST_TARGET_ID=linux-arm64
OUTPOST_LANES=2
```

`/etc/systemd/system/outpost.service`:

```ini
[Unit]
Description=outpost responder
Requires=mnt-share.mount
After=mnt-share.mount

[Service]
Type=simple
EnvironmentFile=/etc/default/outpost
ExecStart=/bin/sh -c '${OUTPOST_BIN} target start ${OUTPOST_TARGET_ID} --root ${OUTPOST_ROOT} --lanes ${OUTPOST_LANES}'
Restart=on-failure
RestartSec=5
# Treat STOP (0) and RESTART (75) as clean exits; restart on others.
SuccessExitStatus=0 75

[Install]
WantedBy=multi-user.target
```

Then `systemctl daemon-reload && systemctl enable --now outpost`.

### macOS (launchd, user agent)

Parallels / Finder-mounted shares are user-session-bound, so this
runs on login, not boot. `~/Library/LaunchAgents/com.outpost.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.outpost</string>
  <key>ProgramArguments</key><array>
    <string>/bin/sh</string>
    <string>-c</string>
    <string>while [ ! -d /Volumes/share ]; do sleep 10; done; exec /Volumes/share/bin/darwin-arm64/outpost target start darwin-arm64 --root /Volumes/share/targets --lanes 2</string>
  </array>
  <key>KeepAlive</key><dict>
    <key>SuccessfulExit</key><false/>
  </dict>
  <key>RunAtLoad</key><true/>
</dict></plist>
```

Load with `launchctl load ~/Library/LaunchAgents/com.outpost.plist`.

### Windows

The simplest reliable option is a `.cmd` supervisor loop started
from Task Scheduler at logon (user-session mounts need an
interactive session):

`C:\outpost\supervisor.cmd`:

```cmd
@echo off
:wait_mount
if not exist "Z:\" (
  timeout /t 10 /nobreak >nul
  goto wait_mount
)
:run
"Z:\bin\windows-arm64\outpost.exe" target start windows-arm64 --root "Z:\targets" --lanes 2
rem Exit 0 = STOP: supervisor exits too
rem Exit 75 = RESTART: loop immediately
rem Exit 74 or other = backoff then retry
if %ERRORLEVEL%==0 exit /b 0
if %ERRORLEVEL%==75 goto run
timeout /t 5 /nobreak >nul
goto wait_mount
```

Register with Task Scheduler, trigger "At log on of any user,"
action `C:\outpost\supervisor.cmd`.

### Portable shell supervisor (for hosts without a native service manager)

```sh
#!/bin/sh
# Usage: env OUTPOST_BIN=... OUTPOST_SHARE=... OUTPOST_TARGET_ID=... supervisor.sh
: "${OUTPOST_BIN:?set OUTPOST_BIN}"
: "${OUTPOST_SHARE:?set OUTPOST_SHARE}"
: "${OUTPOST_TARGET_ID:?set OUTPOST_TARGET_ID}"

while true; do
  while [ ! -d "$OUTPOST_SHARE" ]; do sleep 10; done
  "$OUTPOST_BIN" target start "$OUTPOST_TARGET_ID" \
    --root "$OUTPOST_SHARE/targets" \
    --lanes "${OUTPOST_LANES:-1}"
  case $? in
    0)  break ;;           # STOP — exit supervisor
    75) ;;                 # RESTART — loop immediately
    74) sleep 30 ;;        # mount flapped — longer backoff
    *)  sleep 5 ;;         # crash / other — short backoff
  esac
done
```

## Documentation

- [CHANGELOG.md](CHANGELOG.md) — release notes
- [AGENTS.md](AGENTS.md) — using outpost from an AI agent
- [docs/DESIGN.md](docs/DESIGN.md) — architecture and protocol

## License

MIT. See [LICENSE](LICENSE).
