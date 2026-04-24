package stem

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGenerator_ProducesValidStems(t *testing.T) {
	g := NewGenerator()
	s, err := g.Next("hello-world")
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if err := s.Valid(); err != nil {
		t.Errorf("generated stem failed validation: %v", err)
	}
}

func TestGenerator_TightLoopUnique(t *testing.T) {
	// Hammer the generator: 10k sequential Next calls must all be
	// distinct. With the default clock, this stresses the
	// microsecond-uniqueness guarantee in the worst case.
	const n = 10000
	g := NewGenerator()
	seen := make(map[Stem]struct{}, n)
	for i := 0; i < n; i++ {
		s, err := g.Next("probe")
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if _, dup := seen[s]; dup {
			t.Fatalf("duplicate stem at iteration %d: %s", i, s)
		}
		seen[s] = struct{}{}
	}
}

func TestGenerator_ConcurrentlyUnique(t *testing.T) {
	const workers = 16
	const perWorker = 500
	g := NewGenerator()

	var mu sync.Mutex
	seen := make(map[Stem]struct{}, workers*perWorker)
	var wg sync.WaitGroup
	wg.Add(workers)

	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			local := make([]Stem, 0, perWorker)
			for i := 0; i < perWorker; i++ {
				s, err := g.Next("probe")
				if err != nil {
					t.Errorf("Next: %v", err)
					return
				}
				local = append(local, s)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, s := range local {
				if _, dup := seen[s]; dup {
					t.Errorf("duplicate stem across workers: %s", s)
					return
				}
				seen[s] = struct{}{}
			}
		}()
	}
	wg.Wait()
}

func TestGenerator_MonotonicUnderFrozenClock(t *testing.T) {
	// A clock that always returns the same instant would cause the
	// naive implementation to collide. The default generator must
	// still produce distinct stems by advancing one microsecond per
	// call.
	frozen := time.Date(2026, 4, 23, 10, 0, 0, 0, time.UTC)
	g := NewGeneratorWithClock(func() time.Time { return frozen })

	var prev Stem
	for i := 0; i < 5; i++ {
		s, err := g.Next("x")
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if i > 0 && s <= prev {
			t.Errorf("stems not strictly increasing: prev=%s, got=%s", prev, s)
		}
		prev = s
	}
}

func TestGenerator_RejectsBadLabel(t *testing.T) {
	g := NewGenerator()
	bad := []string{
		"",
		"UPPERCASE",
		"has space",
		"has.dot",
		"has/slash",
		strings.Repeat("a", MaxLabelLen+1),
	}
	for _, label := range bad {
		if _, err := g.Next(label); err == nil {
			t.Errorf("expected error for label %q", label)
		}
	}
}

func TestParse_RoundTrip(t *testing.T) {
	g := NewGenerator()
	original, err := g.Next("round-trip")
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	parsed, err := Parse(string(original))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed != original {
		t.Errorf("mismatch: got %s, want %s", parsed, original)
	}
}

func TestParse_Malformed(t *testing.T) {
	tests := []struct {
		name, input string
	}{
		{"empty", ""},
		{"too short", "20260423-x"},
		{"missing separators", "20260423T103000x123456yprobe"},
		{"non-digit in date", "2026A423_103000_123456-probe"},
		{"non-digit in time", "20260423_10300A_123456-probe"},
		{"non-digit in micros", "20260423_103000_12345A-probe"},
		{"uppercase in label", "20260423_103000_123456-HeLLo"},
		{"space in label", "20260423_103000_123456-hello world"},
		{"empty label", "20260423_103000_123456-"},
		{"invalid month", "20261323_103000_123456-probe"},
		{"invalid hour", "20260423_253000_123456-probe"},
		{"label too long", "20260423_103000_123456-" + strings.Repeat("a", MaxLabelLen+1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(tc.input); err == nil {
				t.Errorf("expected error for input %q", tc.input)
			}
		})
	}
}

func TestStem_TimestampAndLabel(t *testing.T) {
	s := Stem("20260423_103045_123456-compile-all")
	if err := s.Valid(); err != nil {
		t.Fatalf("Valid: %v", err)
	}

	ts, err := s.Timestamp()
	if err != nil {
		t.Fatalf("Timestamp: %v", err)
	}
	want := time.Date(2026, 4, 23, 10, 30, 45, 123456000, time.UTC)
	if !ts.Equal(want) {
		t.Errorf("Timestamp = %v, want %v", ts, want)
	}

	if got := s.Label(); got != "compile-all" {
		t.Errorf("Label = %q, want compile-all", got)
	}
}

func TestStem_LabelOnInvalidReturnsEmpty(t *testing.T) {
	bad := Stem("this-is-not-a-stem")
	if got := bad.Label(); got != "" {
		t.Errorf("invalid stem should return empty label; got %q", got)
	}
}

func TestGenerator_StemSortsChronologically(t *testing.T) {
	// Three stems generated in real time must sort in the same
	// order they were generated. This is the invariant that lets
	// responders process jobs in submission order by alphabetic
	// sort of inbox entries.
	g := NewGenerator()
	a, _ := g.Next("first")
	b, _ := g.Next("second")
	c, _ := g.Next("third")
	if !(a < b && b < c) {
		t.Errorf("stems not chronologically ordered: %s, %s, %s", a, b, c)
	}
}

func TestValidate_BoundaryLabels(t *testing.T) {
	// Shortest valid label: 1 char. Longest: MaxLabelLen chars.
	shortest := "20260423_103045_123456-a"
	if err := (Stem(shortest)).Valid(); err != nil {
		t.Errorf("single-char label rejected: %v", err)
	}
	longest := "20260423_103045_123456-" + strings.Repeat("x", MaxLabelLen)
	if err := (Stem(longest)).Valid(); err != nil {
		t.Errorf("max-length label rejected: %v", err)
	}
}
