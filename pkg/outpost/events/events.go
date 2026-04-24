// Package events defines the observability sink abstraction used
// by the responder (and anywhere else in outpost that wants to
// emit structured events for operators or programmatic consumers).
//
// The implementation shipped is FileLog, which appends a single
// human-readable line per event to a file on disk. Other sinks can
// plug in under the same interface without changing the responder.
package events

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Tim-Butterfield/outpost/pkg/outpost/stem"
)

// Event is one structured observation. Keep the fields small; the
// sink's formatter decides which to surface.
type Event struct {
	// Time is the event's wall-clock time, in UTC. Zero value is
	// replaced with time.Now().UTC() by the sink.
	Time time.Time

	// Level is a short string like "INFO", "WARN", "ERROR". Empty
	// is treated as "INFO" at format time.
	Level string

	// Message is a human-readable description of the event.
	Message string

	// Lane is the lane number if the event is lane-scoped; zero
	// means "not lane-scoped".
	Lane int

	// Stem is populated for events about a specific job. Empty for
	// non-job events (startup, sentinel detection, etc.).
	Stem stem.Stem

	// Fields carries arbitrary additional key/value data. Emitted
	// in sorted key order for deterministic output.
	Fields map[string]string
}

// EventSink is the destination for emitted events. Implementations
// must be safe for concurrent Emit calls.
type EventSink interface {
	// Emit records the event. Returning an error does not stop the
	// caller; most callers log-and-continue. Honors ctx.
	Emit(ctx context.Context, e Event) error

	// Close releases sink resources. After Close, Emit returns an
	// error. Idempotent.
	io.Closer
}

// NewFileLog opens path for append-writes and returns a sink that
// writes one line per event. The file is created with 0644 mode if
// it does not exist.
func NewFileLog(path string) (EventSink, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("events: open %s: %w", path, err)
	}
	return &fileLog{f: f}, nil
}

type fileLog struct {
	mu sync.Mutex
	f  *os.File
}

func (fl *fileLog) Emit(ctx context.Context, e Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	line := FormatEvent(e)
	fl.mu.Lock()
	defer fl.mu.Unlock()
	if fl.f == nil {
		return fmt.Errorf("events: sink closed")
	}
	_, err := fl.f.WriteString(line + "\n")
	return err
}

func (fl *fileLog) Close() error {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	if fl.f == nil {
		return nil
	}
	err := fl.f.Close()
	fl.f = nil
	return err
}

// FormatEvent renders an Event as a single line of text. Exported
// so tests and alternative sinks that want the same formatting can
// reuse it.
func FormatEvent(e Event) string {
	t := e.Time
	if t.IsZero() {
		t = time.Now().UTC()
	}
	level := e.Level
	if level == "" {
		level = "INFO"
	}
	var sb strings.Builder
	sb.WriteString(t.UTC().Format("2006-01-02 15:04:05.000"))
	sb.WriteString(" [")
	sb.WriteString(strings.ToUpper(level))
	sb.WriteString("]")
	if e.Lane > 0 {
		fmt.Fprintf(&sb, " lane=%d", e.Lane)
	}
	if e.Stem != "" {
		fmt.Fprintf(&sb, " stem=%s", e.Stem)
	}
	if e.Message != "" {
		sb.WriteString(" ")
		sb.WriteString(e.Message)
	}
	if len(e.Fields) > 0 {
		keys := make([]string, 0, len(e.Fields))
		for k := range e.Fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&sb, " %s=%s", k, e.Fields[k])
		}
	}
	return sb.String()
}

// Discard is an EventSink that drops every event. Useful for tests
// and code paths that want a nil-safe default.
func Discard() EventSink { return discardSink{} }

type discardSink struct{}

func (discardSink) Emit(context.Context, Event) error { return nil }
func (discardSink) Close() error                      { return nil }

// ConsoleSink writes one formatted line per event to w (typically
// the terminal running `outpost run`, via stderr). Intended to
// sit alongside a file log so operators see lifecycle events
// in their console without tailing the event log. w is not
// owned; Close is a no-op.
func ConsoleSink(w io.Writer) EventSink {
	return &consoleSink{w: w}
}

type consoleSink struct {
	mu sync.Mutex
	w  io.Writer
}

func (c *consoleSink) Emit(ctx context.Context, e Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	line := FormatEvent(e)
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := fmt.Fprintln(c.w, line)
	return err
}

func (c *consoleSink) Close() error { return nil }

// MultiSink fans Emit / Close calls out to each wrapped sink.
// Returns the first non-nil error from any child sink; all other
// sinks still receive the event. Ideal for combining a file log
// with a console sink so every event goes to both.
func MultiSink(sinks ...EventSink) EventSink {
	return &multiSink{sinks: sinks}
}

type multiSink struct {
	sinks []EventSink
}

func (m *multiSink) Emit(ctx context.Context, e Event) error {
	var firstErr error
	for _, s := range m.sinks {
		if err := s.Emit(ctx, e); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *multiSink) Close() error {
	var firstErr error
	for _, s := range m.sinks {
		if err := s.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
