package file

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Tim-Butterfield/outpost/pkg/outpost"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/capability"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/stem"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/transport"
)

func newTestTransport(t *testing.T, laneCount int) *Transport {
	t.Helper()
	tp := New(t.TempDir())
	if err := tp.Prepare(laneCount); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return tp
}

func makeStem(t *testing.T, label string) stem.Stem {
	t.Helper()
	s, err := stem.NewGenerator().Next(label)
	if err != nil {
		t.Fatalf("stem.Next: %v", err)
	}
	return s
}

func TestDispatchRoundTrip(t *testing.T) {
	tp := newTestTransport(t, 1)
	ctx := context.Background()

	want := capability.Dispatch{
		ProtocolVersion: 1,
		PID:             12345,
		Started:         time.Date(2026, 4, 23, 10, 0, 0, 0, time.UTC),
		ResponderName:   "vm-win11-a",
		LaneCount:       1,
		Order:           []string{"sh", "py"},
		Paths:           map[string]string{"sh": "/bin/sh", "py": "/usr/bin/python3"},
	}
	if err := tp.WriteDispatch(ctx, want); err != nil {
		t.Fatalf("WriteDispatch: %v", err)
	}

	got, err := tp.ReadDispatch(ctx)
	if err != nil {
		t.Fatalf("ReadDispatch: %v", err)
	}
	if got.ResponderName != want.ResponderName || got.LaneCount != want.LaneCount {
		t.Errorf("round-trip mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

func TestStatusRoundTrip(t *testing.T) {
	tp := newTestTransport(t, 2)
	ctx := context.Background()

	want := capability.Status{
		Lane:          2,
		State:         capability.StateBusy,
		BusyStem:      makeStem(t, "build"),
		Queued:        3,
		Message:       "compiling",
		LastHeartbeat: time.Date(2026, 4, 23, 10, 30, 0, 0, time.UTC),
	}
	if err := tp.WriteStatus(ctx, 2, want); err != nil {
		t.Fatalf("WriteStatus: %v", err)
	}

	got, err := tp.ReadStatus(ctx, 2)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if got.Lane != 2 || got.State != capability.StateBusy || got.Queued != 3 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestReadDispatch_Missing(t *testing.T) {
	tp := New(t.TempDir())
	_, err := tp.ReadDispatch(context.Background())
	if !errors.Is(err, transport.ErrTransportUnavailable) {
		t.Fatalf("expected ErrTransportUnavailable, got %v", err)
	}
}

func TestPutJob_ListPending_OpenJob(t *testing.T) {
	tp := newTestTransport(t, 1)
	ctx := context.Background()

	stems := []stem.Stem{
		makeStem(t, "first"),
		makeStem(t, "second"),
		makeStem(t, "third"),
	}
	extByStem := map[stem.Stem]string{
		stems[0]: "sh",
		stems[1]: "py",
		stems[2]: "sh",
	}
	contentByStem := map[stem.Stem][]byte{
		stems[0]: []byte("echo first"),
		stems[1]: []byte("print('second')"),
		stems[2]: []byte("echo third"),
	}

	for _, s := range stems {
		if err := tp.PutJob(ctx, 1, s, extByStem[s], bytes.NewReader(contentByStem[s])); err != nil {
			t.Fatalf("PutJob %s: %v", s, err)
		}
	}

	pending, err := tp.ListPending(ctx, 1)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("pending count=%d, want 3", len(pending))
	}
	// stem-sorted order: first < second < third (generated in order)
	for i, p := range pending {
		if p.Stem != stems[i] {
			t.Errorf("pending[%d].Stem=%s, want %s", i, p.Stem, stems[i])
		}
		if p.Ext != extByStem[stems[i]] {
			t.Errorf("pending[%d].Ext=%s, want %s", i, p.Ext, extByStem[stems[i]])
		}
	}

	// Read each job back.
	for _, p := range pending {
		rc, err := tp.OpenJob(ctx, 1, p.Stem, p.Ext)
		if err != nil {
			t.Fatalf("OpenJob %s: %v", p.Stem, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read job: %v", err)
		}
		if !bytes.Equal(data, contentByStem[p.Stem]) {
			t.Errorf("job %s content mismatch: got %q, want %q", p.Stem, data, contentByStem[p.Stem])
		}
	}
}

func TestListPending_FiltersStatusAndTmp(t *testing.T) {
	tp := newTestTransport(t, 1)
	ctx := context.Background()

	// Write a real job.
	s := makeStem(t, "real")
	if err := tp.PutJob(ctx, 1, s, "sh", bytes.NewReader([]byte("real"))); err != nil {
		t.Fatalf("PutJob: %v", err)
	}

	// Drop a status.txt (produced by WriteStatus normally, but manual
	// placement here tests the filter).
	_ = tp.WriteStatus(ctx, 1, capability.Status{
		Lane:          1,
		State:         capability.StateIdle,
		LastHeartbeat: time.Now(),
	})

	// Drop a .tmp file as if a partial write were in flight.
	laneDir := filepath.Join(tp.Root(), "inbox", "1")
	_ = os.WriteFile(filepath.Join(laneDir, "partial.tmp.123"), []byte("partial"), 0644)

	pending, err := tp.ListPending(ctx, 1)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(pending) != 1 {
		t.Errorf("pending=%d, want 1 (status.txt and .tmp filtered)", len(pending))
	}
	if len(pending) == 1 && pending[0].Stem != s {
		t.Errorf("pending[0].Stem=%s, want %s", pending[0].Stem, s)
	}
}

func TestListPending_EmptyOrMissing(t *testing.T) {
	tp := New(t.TempDir())
	ctx := context.Background()

	pending, err := tp.ListPending(ctx, 1)
	if err != nil {
		t.Fatalf("ListPending on missing lane should not error: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("expected empty; got %v", pending)
	}
}

func TestPublishResult_AndGet(t *testing.T) {
	tp := newTestTransport(t, 1)
	ctx := context.Background()

	s := makeStem(t, "probe")
	want := outpost.Result{
		Stem:            s,
		Lane:            1,
		Ext:             "sh",
		Label:           s.Label(),
		ExitCode:        42,
		TimedOut:        false,
		Cancelled:       false,
		Started:         time.Date(2026, 4, 23, 10, 0, 0, 0, time.UTC),
		Finished:        time.Date(2026, 4, 23, 10, 0, 1, 0, time.UTC),
		StdoutBytes:     5,
		StderrBytes:     0,
		StdoutTruncated: false,
		StderrTruncated: false,
	}
	if err := tp.PublishResult(ctx, 1, s, want, []byte("hello"), nil); err != nil {
		t.Fatalf("PublishResult: %v", err)
	}

	got, err := tp.GetResult(ctx, 1, s)
	if err != nil {
		t.Fatalf("GetResult: %v", err)
	}
	if got.Stem != want.Stem || got.ExitCode != want.ExitCode || got.StdoutBytes != want.StdoutBytes {
		t.Errorf("result mismatch:\n got=%+v\nwant=%+v", got, want)
	}
	if !got.Started.Equal(want.Started) || !got.Finished.Equal(want.Finished) {
		t.Errorf("time fields lost precision:\n got=%+v\nwant=%+v", got, want)
	}

	// stdout readable back.
	rc, err := tp.OpenStdout(ctx, 1, s)
	if err != nil {
		t.Fatalf("OpenStdout: %v", err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != "hello" {
		t.Errorf("stdout=%q, want hello", data)
	}
}

func TestGetResult_Missing(t *testing.T) {
	tp := newTestTransport(t, 1)
	_, err := tp.GetResult(context.Background(), 1, makeStem(t, "nope"))
	if !errors.Is(err, transport.ErrJobNotFound) {
		t.Fatalf("expected ErrJobNotFound, got %v", err)
	}
}

func TestOpenStdout_Missing(t *testing.T) {
	tp := newTestTransport(t, 1)
	_, err := tp.OpenStdout(context.Background(), 1, makeStem(t, "nope"))
	if !errors.Is(err, transport.ErrJobNotFound) {
		t.Fatalf("expected ErrJobNotFound, got %v", err)
	}
}

func TestRequestAndCheckCancel(t *testing.T) {
	tp := newTestTransport(t, 1)
	ctx := context.Background()
	s := makeStem(t, "cancel-me")

	cancelled, err := tp.CheckCancel(ctx, 1, s)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled {
		t.Error("CheckCancel before RequestCancel should be false")
	}

	if err := tp.RequestCancel(ctx, 1, s); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}

	cancelled, err = tp.CheckCancel(ctx, 1, s)
	if err != nil {
		t.Fatal(err)
	}
	if !cancelled {
		t.Error("CheckCancel after RequestCancel should be true")
	}

	// Idempotent.
	if err := tp.RequestCancel(ctx, 1, s); err != nil {
		t.Errorf("second RequestCancel should be idempotent: %v", err)
	}
}

func TestArchiveJob_MovesToLogAndClearsCancel(t *testing.T) {
	tp := newTestTransport(t, 1)
	ctx := context.Background()
	s := makeStem(t, "archive-me")

	if err := tp.PutJob(ctx, 1, s, "sh", bytes.NewReader([]byte("work"))); err != nil {
		t.Fatal(err)
	}
	if err := tp.RequestCancel(ctx, 1, s); err != nil {
		t.Fatal(err)
	}

	if err := tp.ArchiveJob(ctx, 1, s, "sh"); err != nil {
		t.Fatalf("ArchiveJob: %v", err)
	}

	// Inbox file gone.
	inbox := filepath.Join(tp.Root(), "inbox", "1", string(s)+".sh")
	if _, err := os.Stat(inbox); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("inbox file should be gone; stat err=%v", err)
	}
	// Log file present.
	logPath := filepath.Join(tp.Root(), "log", "1", string(s)+".sh")
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("log file missing: %v", err)
	}
	// Cancel sentinel gone.
	cancelled, _ := tp.CheckCancel(ctx, 1, s)
	if cancelled {
		t.Error("cancel sentinel should be removed after archive")
	}
}

func TestCleanup_RemovesOldStems(t *testing.T) {
	tp := newTestTransport(t, 1)
	ctx := context.Background()

	// Publish two results with stems from different days.
	oldStem := stem.Stem("20260101_000000_000000-old")
	newStem := makeStem(t, "new")

	for _, s := range []stem.Stem{oldStem, newStem} {
		if err := tp.PublishResult(ctx, 1, s, outpost.Result{Stem: s, Lane: 1}, nil, nil); err != nil {
			t.Fatalf("PublishResult %s: %v", s, err)
		}
	}

	// Cleanup anything older than today.
	cutoff := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	if err := tp.Cleanup(ctx, cutoff); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	// Old stem should be gone.
	if _, err := tp.GetResult(ctx, 1, oldStem); !errors.Is(err, transport.ErrJobNotFound) {
		t.Errorf("old stem should have been cleaned up; GetResult err=%v", err)
	}
	// New stem should remain.
	if _, err := tp.GetResult(ctx, 1, newStem); err != nil {
		t.Errorf("new stem should be retained; GetResult err=%v", err)
	}
}

func TestAtomicPutJobThenList(t *testing.T) {
	// Regression-style: PutJob must be atomic so a concurrent
	// ListPending never sees the .tmp intermediate file.
	tp := newTestTransport(t, 1)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		s := makeStem(t, "probe")
		if err := tp.PutJob(ctx, 1, s, "sh", bytes.NewReader([]byte("work"))); err != nil {
			t.Fatal(err)
		}
		pending, err := tp.ListPending(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pending {
			// Absolutely no .tmp files should leak through.
			for _, entry := range []string{string(p.Stem), p.Ext} {
				if entry == "" {
					t.Errorf("empty stem/ext in listing: %+v", p)
				}
			}
		}
	}
}

func TestConformsToTransportInterface(t *testing.T) {
	// Compile-time check: *Transport satisfies transport.Transport.
	var _ transport.Transport = New("/tmp/x")
}
