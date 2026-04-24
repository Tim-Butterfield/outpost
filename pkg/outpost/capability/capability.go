// Package capability defines the structured types backing the two
// capability-and-state files outpost writes into the shared
// directory: `inbox/dispatch.txt` (responder identity and
// capabilities, written once at startup) and `inbox/<N>/status.txt`
// (per-lane liveness, rewritten every heartbeat).
//
// The on-disk format is key=value text, chosen so a human with
// `cat` can inspect either file. Round-trip through
// Marshal/Unmarshal is the source of truth for the wire format.
//
// See DESIGN.md §§3.8-3.9 for the file shapes and semantics.
package capability

import (
	"bufio"
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Tim-Butterfield/outpost/pkg/outpost/stem"
)

// TimeFormat is the canonical timestamp layout used in both
// dispatch.txt and status.txt: yyyy-mm-dd.HH:MM:SS.uuu (millisecond
// precision). Matches DESIGN.md §3.
const TimeFormat = "2006-01-02.15:04:05.000"

// State names the runtime condition of one lane. Values match the
// strings written into status.txt.
type State string

const (
	StateReady    State = "ready"
	StateIdle     State = "idle"
	StateBusy     State = "busy"
	StatePaused   State = "paused"
	StateStopping State = "stopping"
	StateStopped  State = "stopped"
)

// Valid reports whether s is one of the defined states.
func (s State) Valid() bool {
	switch s {
	case StateReady, StateIdle, StateBusy, StatePaused, StateStopping, StateStopped:
		return true
	}
	return false
}

// Dispatch is the contents of `inbox/dispatch.txt`: responder
// identity plus resolved dispatch table. Written once at responder
// startup; rewritten only on restart.
type Dispatch struct {
	// ProtocolVersion is the wire-format version. Submitters check
	// this on first contact and refuse to submit on mismatch.
	ProtocolVersion int

	// PID is the responder process's operating-system process ID.
	// Useful for diagnostics; a new PID after a restart confirms
	// the responder was restarted.
	PID int

	// Started is the time the responder process started, in UTC.
	Started time.Time

	// ResponderName is the operator-assigned identity. Optional.
	// Empty string is serialized by omitting the key entirely.
	ResponderName string

	// Description is a free-form operator-written sentence about
	// what the target is for. Surfaced in `outpost status` detail
	// views and available to submitter AIs as context when
	// choosing a target for a task.
	Description string

	// Tags are operator-declared semantic labels for task-level
	// target routing ("dotnet-builds", "gpu", "production-isolated").
	// Submitters filter targets by tag to pick hosts appropriate
	// for a given kind of work.
	Tags []string

	// Tools maps build/dev tool name to version string, populated
	// by `outpost setup` from a probe of common PATH tools
	// (git, make, cmake, gcc, clang, go, cargo, dotnet, docker,
	// msbuild[win], xcodebuild[mac], etc.). Submitters consult
	// this to choose a target where a specific tool is present
	// (e.g. a .NET build needs a target with `dotnet` in Tools).
	Tools map[string]string

	// PlatformOS names the operating system family
	// ("linux", "darwin", "windows", ...). Populated from
	// runtime.GOOS at responder startup. Submitters use this to
	// make platform-aware interpreter choices (e.g. prefer
	// PowerShell on Windows even when Python is also available).
	PlatformOS string

	// PlatformArch names the CPU architecture ("amd64", "arm64", ...).
	PlatformArch string

	// CWD is the working directory the responder process was started
	// in, in the OS-native form the responder sees (with drive letter
	// on Windows). Submitters compose relative paths in submitted
	// jobs against this directory, so making it visible lets an AI
	// submitter reason about sibling directories (e.g. "../sibling-repo")
	// without guessing.
	CWD string

	// ComSpec is the value of the Windows %COMSPEC% environment
	// variable as seen by the responder at startup. Windows-only;
	// empty on Unix.
	//
	// Distinct from dispatch.cmd/.bat (the top-level interpreter
	// outpost invokes to run a script): ComSpec is what that
	// interpreter's pipes, `for /f`, and `cmd /c` sub-invocations
	// internally fork. On a system where operators use a cmd.exe
	// replacement (TCC, 4NT), this value lets submitters know that
	// internal pipe behavior in their .cmd jobs will go through
	// that shell rather than stock cmd.exe.
	ComSpec string

	// CWDSource is a stable identifier of the CWD's backing store
	// when CWD sits on a network or hypervisor-shared mount. Intended
	// for cross-target correlation: if two targets report the same
	// CWDSource tail, they're serving the same bytes.
	//
	// Populated as:
	//   Windows:     UNC form of a mapped drive (\\server\share\...)
	//   Linux:       "<mountinfo-source>/<cwd-tail>" for remote FS types
	//                (cifs, nfs, prl_fs, 9p, vboxsf, fuse.sshfs, ...)
	//   macOS:       "<getfsstat-source>/<cwd-tail>" for smbfs/afpfs/nfs
	//   local/unknown: empty
	//
	// Empty is the common case for non-shared setups and should not
	// be treated as an error.
	CWDSource string

	// LaneCount is the number of lanes this responder is serving
	// (directories `inbox/1/` through `inbox/<LaneCount>/`).
	LaneCount int

	// Order lists dispatchable file extensions in operator
	// preference order. Submitter AIs use the first match when
	// multiple interpreters could run a given job.
	Order []string

	// Paths maps each extension in Order (and any other advertised
	// extension) to its resolved absolute interpreter path.
	Paths map[string]string
}

// Status is the contents of a per-lane `inbox/<N>/status.txt`:
// liveness heartbeat plus immediate runtime state. Rewritten on
// every poll cycle (~2s by default) by the lane's own dispatcher
// goroutine.
type Status struct {
	// Lane is the lane number this status file describes (1..N).
	Lane int

	// State is the current runtime condition of this lane.
	State State

	// BusyStem names the job currently executing when State==busy.
	// Empty for all other states.
	BusyStem stem.Stem

	// Queued is the count of dispatchable files pending in this
	// lane's inbox at the time the heartbeat was written. Excludes
	// the file currently in flight (once dispatched, it has moved
	// to log/<N>/).
	Queued int

	// Message is free-form operator text for human readers of the
	// status file. Not parsed by submitters.
	Message string

	// LastHeartbeat is the UTC time at which this file was last
	// rewritten. Submitters check `age(LastHeartbeat) < 10s` to
	// infer liveness.
	LastHeartbeat time.Time
}

// --- Marshal / Unmarshal ---

// Marshal renders a Dispatch into its on-disk key=value form.
func (d Dispatch) Marshal() []byte {
	var buf bytes.Buffer
	writeLine(&buf, "protocol_version", strconv.Itoa(d.ProtocolVersion))
	writeLine(&buf, "pid", strconv.Itoa(d.PID))
	writeLine(&buf, "started", formatTime(d.Started))
	if d.ResponderName != "" {
		writeLine(&buf, "responder_name", d.ResponderName)
	}
	if d.Description != "" {
		writeLine(&buf, "responder_description", d.Description)
	}
	if len(d.Tags) > 0 {
		writeLine(&buf, "responder_tags", strings.Join(d.Tags, ","))
	}
	if d.PlatformOS != "" {
		writeLine(&buf, "platform.os", d.PlatformOS)
	}
	if d.PlatformArch != "" {
		writeLine(&buf, "platform.arch", d.PlatformArch)
	}
	if d.CWD != "" {
		writeLine(&buf, "cwd", d.CWD)
	}
	if d.CWDSource != "" {
		writeLine(&buf, "cwd_source", d.CWDSource)
	}
	if d.ComSpec != "" {
		writeLine(&buf, "comspec", d.ComSpec)
	}
	writeLine(&buf, "lane_count", strconv.Itoa(d.LaneCount))
	// Tools in sorted order for deterministic output.
	if len(d.Tools) > 0 {
		toolKeys := make([]string, 0, len(d.Tools))
		for k := range d.Tools {
			toolKeys = append(toolKeys, k)
		}
		sort.Strings(toolKeys)
		for _, k := range toolKeys {
			writeLine(&buf, "tools."+k, d.Tools[k])
		}
	}
	if len(d.Order) > 0 {
		writeLine(&buf, "dispatch.order", strings.Join(d.Order, ","))
	}
	// Emit per-extension paths in Order first (so the file reads
	// top-down in preference order), then any remainder in sorted
	// order for determinism.
	seen := make(map[string]struct{}, len(d.Order))
	for _, ext := range d.Order {
		if p, ok := d.Paths[ext]; ok {
			writeLine(&buf, "dispatch."+ext, p)
			seen[ext] = struct{}{}
		}
	}
	extras := make([]string, 0)
	for ext := range d.Paths {
		if _, inOrder := seen[ext]; !inOrder {
			extras = append(extras, ext)
		}
	}
	sort.Strings(extras)
	for _, ext := range extras {
		writeLine(&buf, "dispatch."+ext, d.Paths[ext])
	}
	return buf.Bytes()
}

// UnmarshalDispatch parses a dispatch.txt body into a Dispatch
// struct. Unknown keys are ignored to allow additive protocol
// evolution.
func UnmarshalDispatch(data []byte) (Dispatch, error) {
	kv, err := parseKV(data)
	if err != nil {
		return Dispatch{}, fmt.Errorf("capability: dispatch: %w", err)
	}
	d := Dispatch{Paths: map[string]string{}}

	if v, ok := kv["protocol_version"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Dispatch{}, fmt.Errorf("capability: dispatch: protocol_version: %w", err)
		}
		d.ProtocolVersion = n
	}
	if v, ok := kv["pid"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Dispatch{}, fmt.Errorf("capability: dispatch: pid: %w", err)
		}
		d.PID = n
	}
	if v, ok := kv["started"]; ok {
		t, err := parseTime(v)
		if err != nil {
			return Dispatch{}, fmt.Errorf("capability: dispatch: started: %w", err)
		}
		d.Started = t
	}
	if v, ok := kv["responder_name"]; ok {
		d.ResponderName = v
	}
	if v, ok := kv["responder_description"]; ok {
		d.Description = v
	}
	if v, ok := kv["responder_tags"]; ok && v != "" {
		d.Tags = strings.Split(v, ",")
	}
	if v, ok := kv["platform.os"]; ok {
		d.PlatformOS = v
	}
	if v, ok := kv["platform.arch"]; ok {
		d.PlatformArch = v
	}
	if v, ok := kv["cwd"]; ok {
		d.CWD = v
	}
	if v, ok := kv["cwd_source"]; ok {
		d.CWDSource = v
	}
	if v, ok := kv["comspec"]; ok {
		d.ComSpec = v
	}
	// Collect any tools.<name> keys into the Tools map.
	for k, v := range kv {
		if name, ok := strings.CutPrefix(k, "tools."); ok {
			if d.Tools == nil {
				d.Tools = map[string]string{}
			}
			d.Tools[name] = v
		}
	}
	if v, ok := kv["lane_count"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Dispatch{}, fmt.Errorf("capability: dispatch: lane_count: %w", err)
		}
		d.LaneCount = n
	}
	if v, ok := kv["dispatch.order"]; ok && v != "" {
		d.Order = strings.Split(v, ",")
	}
	for k, v := range kv {
		if ext, ok := strings.CutPrefix(k, "dispatch."); ok && ext != "order" {
			d.Paths[ext] = v
		}
	}
	return d, nil
}

// Marshal renders a Status into its on-disk key=value form.
func (s Status) Marshal() []byte {
	var buf bytes.Buffer
	writeLine(&buf, "lane", strconv.Itoa(s.Lane))
	writeLine(&buf, "state", string(s.State))
	writeLine(&buf, "busy_stem", string(s.BusyStem))
	writeLine(&buf, "queued", strconv.Itoa(s.Queued))
	writeLine(&buf, "message", s.Message)
	writeLine(&buf, "last_heartbeat", formatTime(s.LastHeartbeat))
	return buf.Bytes()
}

// UnmarshalStatus parses a status.txt body into a Status struct.
// Unknown keys are ignored.
func UnmarshalStatus(data []byte) (Status, error) {
	kv, err := parseKV(data)
	if err != nil {
		return Status{}, fmt.Errorf("capability: status: %w", err)
	}
	var s Status

	if v, ok := kv["lane"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Status{}, fmt.Errorf("capability: status: lane: %w", err)
		}
		s.Lane = n
	}
	if v, ok := kv["state"]; ok {
		state := State(v)
		if v != "" && !state.Valid() {
			return Status{}, fmt.Errorf("capability: status: state=%q not recognized", v)
		}
		s.State = state
	}
	if v, ok := kv["busy_stem"]; ok && v != "" {
		// Validate the stem rather than accept arbitrary strings.
		parsed, err := stem.Parse(v)
		if err != nil {
			return Status{}, fmt.Errorf("capability: status: busy_stem: %w", err)
		}
		s.BusyStem = parsed
	}
	if v, ok := kv["queued"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Status{}, fmt.Errorf("capability: status: queued: %w", err)
		}
		s.Queued = n
	}
	if v, ok := kv["message"]; ok {
		s.Message = v
	}
	if v, ok := kv["last_heartbeat"]; ok {
		t, err := parseTime(v)
		if err != nil {
			return Status{}, fmt.Errorf("capability: status: last_heartbeat: %w", err)
		}
		s.LastHeartbeat = t
	}
	return s, nil
}

// --- internals ---

// parseKV reads key=value lines, skipping blanks and `#` comments,
// and returns the last value seen for each key (matching the
// behavior of cheap operator hand-edits that append to a file).
func parseKV(data []byte) (map[string]string, error) {
	kv := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.IndexByte(line, '=')
		if idx < 0 {
			return nil, fmt.Errorf("line %d: missing '=': %q", lineNo, line)
		}
		key := strings.TrimSpace(line[:idx])
		if key == "" {
			return nil, fmt.Errorf("line %d: empty key", lineNo)
		}
		kv[key] = strings.TrimSpace(line[idx+1:])
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return kv, nil
}

func writeLine(w *bytes.Buffer, key, value string) {
	w.WriteString(key)
	w.WriteByte('=')
	w.WriteString(value)
	w.WriteByte('\n')
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(TimeFormat)
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(TimeFormat, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}
