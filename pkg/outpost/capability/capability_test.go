package capability

import (
	"strings"
	"testing"
	"time"

	"github.com/Tim-Butterfield/outpost/pkg/outpost/stem"
)

func TestState_Valid(t *testing.T) {
	valid := []State{
		StateReady, StateIdle, StateBusy,
		StatePaused, StateStopping, StateStopped,
	}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("%q should be valid", s)
		}
	}
	invalid := []State{"", "pending", "running", "error"}
	for _, s := range invalid {
		if s.Valid() {
			t.Errorf("%q should not be valid", s)
		}
	}
}

func TestDispatch_RoundTrip(t *testing.T) {
	d := Dispatch{
		ProtocolVersion: 1,
		PID:             12345,
		Started:         time.Date(2026, 4, 23, 10, 0, 0, 0, time.UTC),
		ResponderName:   "vm-win11-a",
		LaneCount:       2,
		Order:           []string{"py", "sh", "btm"},
		Paths: map[string]string{
			"py":  "C:/Python313/python.exe",
			"sh":  "C:/Program Files/Git/usr/bin/bash.exe",
			"btm": "C:/Program Files/JPSoft/TCMD36/tcc.exe",
		},
	}
	data := d.Marshal()

	parsed, err := UnmarshalDispatch(data)
	if err != nil {
		t.Fatalf("UnmarshalDispatch: %v", err)
	}
	if parsed.ProtocolVersion != 1 || parsed.PID != 12345 {
		t.Errorf("numeric fields mismatch: %+v", parsed)
	}
	if !parsed.Started.Equal(d.Started) {
		t.Errorf("Started mismatch: got %v, want %v", parsed.Started, d.Started)
	}
	if parsed.ResponderName != "vm-win11-a" {
		t.Errorf("ResponderName: got %q", parsed.ResponderName)
	}
	if parsed.LaneCount != 2 {
		t.Errorf("LaneCount: got %d", parsed.LaneCount)
	}
	if !equalStrings(parsed.Order, d.Order) {
		t.Errorf("Order: got %v, want %v", parsed.Order, d.Order)
	}
	if len(parsed.Paths) != len(d.Paths) {
		t.Errorf("Paths length: got %d, want %d", len(parsed.Paths), len(d.Paths))
	}
	for k, v := range d.Paths {
		if parsed.Paths[k] != v {
			t.Errorf("Paths[%q]: got %q, want %q", k, parsed.Paths[k], v)
		}
	}
}

func TestDispatch_PlatformRoundTrip(t *testing.T) {
	d := Dispatch{
		ProtocolVersion: 1,
		PID:             1,
		Started:         time.Date(2026, 4, 23, 10, 0, 0, 0, time.UTC),
		PlatformOS:      "linux",
		PlatformArch:    "arm64",
		LaneCount:       1,
	}
	data := d.Marshal()
	if !strings.Contains(string(data), "platform.os=linux") {
		t.Errorf("marshal missing platform.os: %s", data)
	}
	if !strings.Contains(string(data), "platform.arch=arm64") {
		t.Errorf("marshal missing platform.arch: %s", data)
	}
	parsed, err := UnmarshalDispatch(data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.PlatformOS != "linux" || parsed.PlatformArch != "arm64" {
		t.Errorf("platform round-trip: got %+v", parsed)
	}
}

func TestDispatch_CWDRoundTrip(t *testing.T) {
	d := Dispatch{
		ProtocolVersion: 1,
		PID:             1,
		Started:         time.Date(2026, 4, 23, 10, 0, 0, 0, time.UTC),
		CWD:             `X:\proj\app`,
		CWDSource:       `\\server\share\proj\app`,
		LaneCount:       1,
	}
	data := d.Marshal()
	if !strings.Contains(string(data), `cwd=X:\proj\app`) {
		t.Errorf("marshal missing cwd: %s", data)
	}
	if !strings.Contains(string(data), `cwd_source=\\server\share\proj\app`) {
		t.Errorf("marshal missing cwd_source: %s", data)
	}
	parsed, err := UnmarshalDispatch(data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.CWD != d.CWD {
		t.Errorf("CWD round-trip: got %q, want %q", parsed.CWD, d.CWD)
	}
	if parsed.CWDSource != d.CWDSource {
		t.Errorf("CWDSource round-trip: got %q, want %q", parsed.CWDSource, d.CWDSource)
	}
}

func TestDispatch_OmitsEmptyCWD(t *testing.T) {
	d := Dispatch{
		ProtocolVersion: 1,
		PID:             1,
		Started:         time.Date(2026, 4, 23, 10, 0, 0, 0, time.UTC),
		LaneCount:       1,
	}
	data := string(d.Marshal())
	if strings.Contains(data, "cwd=") {
		t.Errorf("empty CWD should be omitted; got:\n%s", data)
	}
	if strings.Contains(data, "cwd_source=") {
		t.Errorf("empty CWDSource should be omitted; got:\n%s", data)
	}
}

func TestDispatch_ComSpecRoundTrip(t *testing.T) {
	d := Dispatch{
		ProtocolVersion: 1,
		PID:             1,
		Started:         time.Date(2026, 4, 23, 10, 0, 0, 0, time.UTC),
		ComSpec:         `C:\WINDOWS\system32\cmd.exe`,
		LaneCount:       1,
	}
	data := d.Marshal()
	if !strings.Contains(string(data), `comspec=C:\WINDOWS\system32\cmd.exe`) {
		t.Errorf("marshal missing comspec: %s", data)
	}
	parsed, err := UnmarshalDispatch(data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ComSpec != d.ComSpec {
		t.Errorf("ComSpec round-trip: got %q, want %q", parsed.ComSpec, d.ComSpec)
	}
}

func TestDispatch_OmitsEmptyComSpec(t *testing.T) {
	d := Dispatch{
		ProtocolVersion: 1,
		PID:             1,
		Started:         time.Date(2026, 4, 23, 10, 0, 0, 0, time.UTC),
		LaneCount:       1,
	}
	data := string(d.Marshal())
	if strings.Contains(data, "comspec=") {
		t.Errorf("empty ComSpec should be omitted; got:\n%s", data)
	}
}

func TestDispatch_OmitsEmptyPlatform(t *testing.T) {
	d := Dispatch{
		ProtocolVersion: 1,
		PID:             1,
		Started:         time.Date(2026, 4, 23, 10, 0, 0, 0, time.UTC),
		LaneCount:       1,
	}
	data := string(d.Marshal())
	if strings.Contains(data, "platform.os=") {
		t.Errorf("empty PlatformOS should be omitted; got:\n%s", data)
	}
	if strings.Contains(data, "platform.arch=") {
		t.Errorf("empty PlatformArch should be omitted; got:\n%s", data)
	}
}

func TestDispatch_OmitsEmptyResponderName(t *testing.T) {
	d := Dispatch{
		ProtocolVersion: 1,
		PID:             12345,
		Started:         time.Date(2026, 4, 23, 10, 0, 0, 0, time.UTC),
		LaneCount:       1,
	}
	data := string(d.Marshal())
	if strings.Contains(data, "responder_name=") {
		t.Errorf("responder_name key should be absent when unset; got:\n%s", data)
	}
}

func TestDispatch_DescriptionTagsToolsRoundTrip(t *testing.T) {
	d := Dispatch{
		ProtocolVersion: 1,
		PID:             12345,
		Started:         time.Date(2026, 4, 23, 10, 0, 0, 0, time.UTC),
		Description:     "Windows ARM dev VM with .NET 8 SDK",
		Tags:            []string{"dotnet-builds", "windows-native"},
		Tools: map[string]string{
			"git":    "git version 2.42.0",
			"dotnet": "8.0.100",
			"docker": "Docker version 24.0.6",
		},
		LaneCount: 1,
	}
	data := d.Marshal()
	text := string(data)
	for _, want := range []string{
		"responder_description=Windows ARM dev VM with .NET 8 SDK",
		"responder_tags=dotnet-builds,windows-native",
		"tools.git=git version 2.42.0",
		"tools.dotnet=8.0.100",
		"tools.docker=Docker version 24.0.6",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("marshal missing %q:\n%s", want, text)
		}
	}
	parsed, err := UnmarshalDispatch(data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Description != d.Description {
		t.Errorf("Description round-trip: %q -> %q", d.Description, parsed.Description)
	}
	if len(parsed.Tags) != 2 {
		t.Errorf("Tags round-trip: %v", parsed.Tags)
	}
	if parsed.Tools["dotnet"] != "8.0.100" {
		t.Errorf("Tools[dotnet]: %q", parsed.Tools["dotnet"])
	}
}

func TestDispatch_OmitsEmptyDescriptionAndTags(t *testing.T) {
	d := Dispatch{
		ProtocolVersion: 1,
		PID:             1,
		Started:         time.Date(2026, 4, 23, 10, 0, 0, 0, time.UTC),
		LaneCount:       1,
	}
	text := string(d.Marshal())
	if strings.Contains(text, "responder_description=") {
		t.Errorf("empty description should be omitted: %s", text)
	}
	if strings.Contains(text, "responder_tags=") {
		t.Errorf("empty tags should be omitted: %s", text)
	}
}

func TestDispatch_PreferredOrderInFile(t *testing.T) {
	// The on-disk layout should emit dispatch.<ext> lines in Order
	// before any extras, so the file reads top-down with the
	// preferred interpreter first.
	d := Dispatch{
		ProtocolVersion: 1,
		LaneCount:       1,
		Order:           []string{"btm", "py"},
		Paths: map[string]string{
			"btm": "/path/to/tcc",
			"py":  "/path/to/python3",
			"rb":  "/path/to/ruby", // extra, not in Order
		},
	}
	data := string(d.Marshal())
	btmIdx := strings.Index(data, "dispatch.btm=")
	pyIdx := strings.Index(data, "dispatch.py=")
	rbIdx := strings.Index(data, "dispatch.rb=")
	if btmIdx < 0 || pyIdx < 0 || rbIdx < 0 {
		t.Fatalf("missing keys in output:\n%s", data)
	}
	if !(btmIdx < pyIdx && pyIdx < rbIdx) {
		t.Errorf("expected btm < py < rb positions; got %d, %d, %d", btmIdx, pyIdx, rbIdx)
	}
}

func TestDispatch_Malformed(t *testing.T) {
	tests := []struct {
		name, input string
	}{
		{"missing equals", "protocol_version 1\n"},
		{"empty key", "=oops\n"},
		{"non-numeric protocol_version", "protocol_version=oops\n"},
		{"non-numeric pid", "pid=xyz\n"},
		{"bad started", "started=not-a-timestamp\n"},
		{"non-numeric lane_count", "lane_count=many\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := UnmarshalDispatch([]byte(tc.input)); err == nil {
				t.Errorf("expected error for input %q", tc.input)
			}
		})
	}
}

func TestDispatch_UnknownKeysIgnored(t *testing.T) {
	// Additive protocol evolution: unknown keys must be tolerated
	// so an older submitter can read a newer dispatch.txt without
	// failing.
	input := []byte(`protocol_version=1
pid=1
started=2026-04-23.10:00:00.000
lane_count=1
future_field=some_value
nested.future=another
`)
	d, err := UnmarshalDispatch(input)
	if err != nil {
		t.Fatalf("unknown keys should not cause errors: %v", err)
	}
	if d.LaneCount != 1 {
		t.Errorf("known fields should parse alongside unknowns")
	}
}

func TestDispatch_Comments(t *testing.T) {
	input := []byte(`# header comment
protocol_version=1

# blank-line tolerant
pid=42
lane_count=1
`)
	d, err := UnmarshalDispatch(input)
	if err != nil {
		t.Fatalf("comments should be tolerated: %v", err)
	}
	if d.PID != 42 {
		t.Errorf("PID: got %d", d.PID)
	}
}

func TestStatus_RoundTrip(t *testing.T) {
	g := stem.NewGenerator()
	st, _ := g.Next("build-all")
	s := Status{
		Lane:          1,
		State:         StateBusy,
		BusyStem:      st,
		Queued:        3,
		Message:       "compiling pkg/outpost",
		LastHeartbeat: time.Date(2026, 4, 23, 10, 30, 0, 500*int(time.Millisecond), time.UTC),
	}
	data := s.Marshal()

	parsed, err := UnmarshalStatus(data)
	if err != nil {
		t.Fatalf("UnmarshalStatus: %v", err)
	}
	if parsed.Lane != 1 {
		t.Errorf("Lane: %d", parsed.Lane)
	}
	if parsed.State != StateBusy {
		t.Errorf("State: %q", parsed.State)
	}
	if parsed.BusyStem != st {
		t.Errorf("BusyStem mismatch: %q vs %q", parsed.BusyStem, st)
	}
	if parsed.Queued != 3 {
		t.Errorf("Queued: %d", parsed.Queued)
	}
	if parsed.Message != "compiling pkg/outpost" {
		t.Errorf("Message: %q", parsed.Message)
	}
	if !parsed.LastHeartbeat.Equal(s.LastHeartbeat) {
		t.Errorf("LastHeartbeat mismatch: got %v, want %v", parsed.LastHeartbeat, s.LastHeartbeat)
	}
}

func TestStatus_IdleNoBusyStem(t *testing.T) {
	s := Status{
		Lane:          1,
		State:         StateIdle,
		Queued:        0,
		LastHeartbeat: time.Date(2026, 4, 23, 10, 0, 0, 0, time.UTC),
	}
	data := s.Marshal()
	parsed, err := UnmarshalStatus(data)
	if err != nil {
		t.Fatalf("UnmarshalStatus: %v", err)
	}
	if parsed.BusyStem != "" {
		t.Errorf("BusyStem should be empty for idle lane; got %q", parsed.BusyStem)
	}
}

func TestStatus_RejectsUnknownState(t *testing.T) {
	input := []byte(`lane=1
state=fabulous
queued=0
last_heartbeat=2026-04-23.10:00:00.000
`)
	if _, err := UnmarshalStatus(input); err == nil {
		t.Fatal("unknown state should be rejected")
	}
}

func TestStatus_RejectsMalformedBusyStem(t *testing.T) {
	input := []byte(`lane=1
state=busy
busy_stem=this-is-not-a-valid-stem
queued=0
last_heartbeat=2026-04-23.10:00:00.000
`)
	if _, err := UnmarshalStatus(input); err == nil {
		t.Fatal("malformed busy_stem should be rejected")
	}
}

func TestStatus_RejectsNonNumericLane(t *testing.T) {
	input := []byte(`lane=one
state=idle
`)
	if _, err := UnmarshalStatus(input); err == nil {
		t.Fatal("non-numeric lane should be rejected")
	}
}

func TestTimeFormat_PreservesMilliseconds(t *testing.T) {
	// The canonical TimeFormat should round-trip millisecond
	// precision without drift.
	orig := time.Date(2026, 4, 23, 10, 23, 5, 11_000_000, time.UTC) // 011 ms
	s := formatTime(orig)
	got, err := parseTime(s)
	if err != nil {
		t.Fatalf("parseTime: %v", err)
	}
	if !got.Equal(orig) {
		t.Errorf("time round-trip lost precision: got %v, want %v (via %q)", got, orig, s)
	}
}

func TestTimeFormat_ZeroTimeSerializesEmpty(t *testing.T) {
	if got := formatTime(time.Time{}); got != "" {
		t.Errorf("zero time should serialize to empty; got %q", got)
	}
	got, err := parseTime("")
	if err != nil {
		t.Errorf("empty string should parse cleanly: %v", err)
	}
	if !got.IsZero() {
		t.Errorf("empty string should parse to zero time; got %v", got)
	}
}

func TestDispatch_CarriageReturnTolerant(t *testing.T) {
	// Operators editing on Windows may leave \r\n line endings.
	// The parser must handle them.
	input := []byte("protocol_version=1\r\npid=1\r\nlane_count=1\r\n")
	d, err := UnmarshalDispatch(input)
	if err != nil {
		t.Fatalf("CRLF should not cause errors: %v", err)
	}
	if d.ProtocolVersion != 1 || d.PID != 1 || d.LaneCount != 1 {
		t.Errorf("CRLF parse lost fields: %+v", d)
	}
}

// --- helpers ---

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

