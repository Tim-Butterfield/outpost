// Package stem defines outpost's per-job identity format.
//
// A stem has the shape:
//
//	YYYYMMDD_HHMMSS_uuuuuu-<label>
//
// Fixed-width fields give alphabetic = chronological sort regardless
// of timezone or clock skew. The microsecond field prevents
// collisions in bursts submitted within the same wall second. The
// label (matching [a-z0-9_-]{1,64}) describes the job's intent and
// appears in logs, the dispatch record, and the filename.
//
// See DESIGN.md §3.2.
package stem

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// MaxLabelLen is the maximum length of the label suffix.
const MaxLabelLen = 64

// labelRe is the label charset: lowercase alphanumerics with hyphens
// and underscores. Uppercase is rejected so that case-insensitive
// filesystems (NTFS, default-APFS) don't collapse two distinct stems
// onto the same on-disk name.
var labelRe = regexp.MustCompile(`^[a-z0-9_-]+$`)

// timestampFormat is the Go layout for the fixed prefix of a stem.
// The microsecond field is formatted separately because Go's
// fractional-second directives interact awkwardly with the
// underscore separator required by the stem format.
const timestampFormat = "20060102_150405"

// Stem is a validated outpost job identifier. The zero value is not
// valid. Construct via Generator.Next or Parse.
type Stem string

// String renders the stem as its wire-format string.
func (s Stem) String() string { return string(s) }

// Timestamp returns the microsecond-precision UTC time encoded in
// the stem's prefix. Returns an error if the stem is malformed.
func (s Stem) Timestamp() (time.Time, error) {
	if err := s.Valid(); err != nil {
		return time.Time{}, err
	}
	return parseTimestamp(string(s))
}

// Label returns the free-form label suffix. Returns an empty
// string if the stem is malformed.
func (s Stem) Label() string {
	if err := s.Valid(); err != nil {
		return ""
	}
	return string(s)[23:]
}

// Valid reports whether s conforms to the stem format. A nil
// return indicates validity; any other error describes the first
// problem found.
func (s Stem) Valid() error {
	return validate(string(s))
}

// Generator produces fresh, unique stems. Implementations must be
// safe for concurrent use. The default implementation guarantees
// monotonic timestamps even under tight bursts.
type Generator interface {
	Next(label string) (Stem, error)
}

// NewGenerator returns a Generator backed by the real clock with
// monotonic-microsecond collision avoidance.
func NewGenerator() Generator {
	return &clockGenerator{now: time.Now}
}

// NewGeneratorWithClock returns a Generator backed by the provided
// now function. Useful in tests to inject deterministic time.
func NewGeneratorWithClock(now func() time.Time) Generator {
	if now == nil {
		now = time.Now
	}
	return &clockGenerator{now: now}
}

// clockGenerator advances time monotonically by one microsecond
// whenever two consecutive calls would otherwise produce the same
// stem prefix. This preserves the chronological-sort property
// under any rate of submission.
type clockGenerator struct {
	now func() time.Time

	mu   sync.Mutex
	last time.Time
}

// Next returns a fresh stem using the supplied label.
func (g *clockGenerator) Next(label string) (Stem, error) {
	if err := validateLabel(label); err != nil {
		return "", err
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	t := g.now().UTC().Truncate(time.Microsecond)
	if !t.After(g.last) {
		t = g.last.Add(time.Microsecond)
	}
	g.last = t

	micros := t.Nanosecond() / 1000
	return Stem(fmt.Sprintf("%s_%06d-%s", t.Format(timestampFormat), micros, label)), nil
}

// Parse validates s and returns it as a Stem. Call sites that
// already have a Stem value do not need Parse; it exists for
// reading stems back from filenames or protocol files.
func Parse(s string) (Stem, error) {
	if err := validate(s); err != nil {
		return "", err
	}
	return Stem(s), nil
}

// --- internals ---

// validate checks every structural property of a stem string.
// Ordering (length, separators, digits, label) gives a useful
// first-error message when debugging malformed input.
func validate(s string) error {
	// Minimum length: 8 + 1 + 6 + 1 + 6 + 1 + 1 = 24 characters
	// (timestamp + shortest possible one-char label).
	if len(s) < 24 {
		return fmt.Errorf("stem: too short (%d chars, need >= 24): %q", len(s), s)
	}
	if s[8] != '_' {
		return fmt.Errorf("stem: expected '_' at position 8: %q", s)
	}
	if s[15] != '_' {
		return fmt.Errorf("stem: expected '_' at position 15: %q", s)
	}
	if s[22] != '-' {
		return fmt.Errorf("stem: expected '-' at position 22: %q", s)
	}
	// Positions 0-7 (date), 9-14 (time), 16-21 (micros) must all be
	// decimal digits.
	for _, i := range []int{0, 1, 2, 3, 4, 5, 6, 7, 9, 10, 11, 12, 13, 14, 16, 17, 18, 19, 20, 21} {
		if s[i] < '0' || s[i] > '9' {
			return fmt.Errorf("stem: non-digit at position %d: %q", i, s)
		}
	}
	// Semantic timestamp check: parser rejects invalid month / day /
	// hour etc.
	if _, err := parseTimestamp(s); err != nil {
		return err
	}
	// Label charset.
	if err := validateLabel(s[23:]); err != nil {
		return err
	}
	return nil
}

func validateLabel(label string) error {
	if label == "" {
		return errors.New("stem: empty label")
	}
	if len(label) > MaxLabelLen {
		return fmt.Errorf("stem: label too long (%d > %d): %q", len(label), MaxLabelLen, label)
	}
	if !labelRe.MatchString(label) {
		return fmt.Errorf("stem: label must match [a-z0-9_-]+: %q", label)
	}
	return nil
}

// parseTimestamp pulls the wall-clock time out of a stem's prefix.
// Assumes the prefix has already passed structural validation.
func parseTimestamp(s string) (time.Time, error) {
	t, err := time.Parse(timestampFormat, s[:15])
	if err != nil {
		return time.Time{}, fmt.Errorf("stem: parse timestamp: %w", err)
	}
	micros, err := strconv.Atoi(s[16:22])
	if err != nil {
		return time.Time{}, fmt.Errorf("stem: parse microseconds: %w", err)
	}
	return t.Add(time.Duration(micros) * time.Microsecond).UTC(), nil
}
