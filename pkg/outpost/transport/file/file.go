// Package file implements the file-RPC Transport for outpost.
// The transport operates entirely through a shared directory --
// mounted SMB/NFS, synchronized via Dropbox/Syncthing/iCloud, a
// git-tracked folder, or even a USB stick moved between hosts.
//
// All writes go through internal/fsatomic so concurrent readers
// on the other side never observe a half-written file. The
// directory layout implemented here matches DESIGN.md §3.1.
package file

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Tim-Butterfield/outpost/internal/fsatomic"
	"github.com/Tim-Butterfield/outpost/pkg/outpost"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/capability"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/protocol"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/stem"
	"github.com/Tim-Butterfield/outpost/pkg/outpost/transport"
)

// Transport is the file-RPC implementation of transport.Transport.
// Construct via New(root) where root is the shared-directory path.
type Transport struct {
	root string
}

// New returns a file-RPC transport rooted at path. The directory
// need not exist yet; the first responder-side call creates the
// required subdirectories.
func New(root string) *Transport {
	return &Transport{root: root}
}

// Close is a no-op; file-RPC holds no persistent handles.
func (t *Transport) Close() error { return nil }

// Root returns the shared-directory path this transport operates on.
// Useful for logging and diagnostics.
func (t *Transport) Root() string { return t.root }

// --- path helpers ---

func (t *Transport) inboxDir() string                  { return filepath.Join(t.root, protocol.DirInbox) }
func (t *Transport) dispatchFile() string              { return filepath.Join(t.inboxDir(), protocol.FileDispatch) }
func (t *Transport) laneInboxDir(lane int) string      { return filepath.Join(t.inboxDir(), strconv.Itoa(lane)) }
func (t *Transport) laneStatusFile(lane int) string    { return filepath.Join(t.laneInboxDir(lane), protocol.FileStatus) }
func (t *Transport) laneOutboxDir(lane int) string     { return filepath.Join(t.root, protocol.DirOutbox, strconv.Itoa(lane)) }
func (t *Transport) laneCancelDir(lane int) string     { return filepath.Join(t.root, protocol.DirCancel, strconv.Itoa(lane)) }
func (t *Transport) laneLogDir(lane int) string        { return filepath.Join(t.root, protocol.DirLog, strconv.Itoa(lane)) }

func (t *Transport) jobFile(lane int, s stem.Stem, ext string) string {
	return filepath.Join(t.laneInboxDir(lane), string(s)+"."+ext)
}

func (t *Transport) resultFile(lane int, s stem.Stem) string {
	return filepath.Join(t.laneOutboxDir(lane), string(s)+protocol.ExtResult)
}

func (t *Transport) stdoutFile(lane int, s stem.Stem) string {
	return filepath.Join(t.laneOutboxDir(lane), string(s)+protocol.ExtStdout)
}

func (t *Transport) stderrFile(lane int, s stem.Stem) string {
	return filepath.Join(t.laneOutboxDir(lane), string(s)+protocol.ExtStderr)
}

func (t *Transport) cancelFile(lane int, s stem.Stem) string {
	return filepath.Join(t.laneCancelDir(lane), string(s))
}

// Prepare creates the directory tree required to host a responder
// with the given lane count. Called once at responder startup
// before WriteDispatch. Missing directories are created; existing
// directories are left alone.
func (t *Transport) Prepare(laneCount int) error {
	for _, d := range []string{
		t.root,
		t.inboxDir(),
		filepath.Join(t.root, protocol.DirOutbox),
		filepath.Join(t.root, protocol.DirCancel),
		filepath.Join(t.root, protocol.DirLog),
	} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return fmt.Errorf("file: mkdir %s: %w", d, err)
		}
	}
	for lane := 1; lane <= laneCount; lane++ {
		for _, d := range []string{
			t.laneInboxDir(lane),
			t.laneOutboxDir(lane),
			t.laneCancelDir(lane),
			t.laneLogDir(lane),
		} {
			if err := os.MkdirAll(d, 0755); err != nil {
				return fmt.Errorf("file: mkdir %s: %w", d, err)
			}
		}
	}
	return nil
}

// --- Submitter methods ---

// ReadDispatch reads inbox/dispatch.txt and parses it into a
// capability.Dispatch value.
func (t *Transport) ReadDispatch(ctx context.Context) (capability.Dispatch, error) {
	data, err := readFileCtx(ctx, t.dispatchFile())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return capability.Dispatch{}, fmt.Errorf("%w: dispatch.txt not present", transport.ErrTransportUnavailable)
		}
		return capability.Dispatch{}, fmt.Errorf("file: read dispatch: %w", err)
	}
	return capability.UnmarshalDispatch(data)
}

// ReadStatus reads inbox/<lane>/status.txt and parses it.
func (t *Transport) ReadStatus(ctx context.Context, lane int) (capability.Status, error) {
	data, err := readFileCtx(ctx, t.laneStatusFile(lane))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return capability.Status{}, fmt.Errorf("%w: lane %d status.txt not present", transport.ErrTransportUnavailable, lane)
		}
		return capability.Status{}, fmt.Errorf("file: read status lane=%d: %w", lane, err)
	}
	return capability.UnmarshalStatus(data)
}

// PutJob writes content atomically to inbox/<lane>/<stem>.<ext>.
func (t *Transport) PutJob(ctx context.Context, lane int, s stem.Stem, ext string, content io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(t.laneInboxDir(lane), 0755); err != nil {
		return fmt.Errorf("file: mkdir lane %d inbox: %w", lane, err)
	}
	data, err := io.ReadAll(content)
	if err != nil {
		return fmt.Errorf("file: read job content: %w", err)
	}
	return fsatomic.WriteFile(t.jobFile(lane, s, ext), data)
}

// GetResult reads outbox/<lane>/<stem>.result. Returns
// transport.ErrJobNotFound when the file has not been published.
func (t *Transport) GetResult(ctx context.Context, lane int, s stem.Stem) (outpost.Result, error) {
	data, err := readFileCtx(ctx, t.resultFile(lane, s))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return outpost.Result{}, transport.ErrJobNotFound
		}
		return outpost.Result{}, fmt.Errorf("file: read result lane=%d stem=%s: %w", lane, s, err)
	}
	return unmarshalResult(data)
}

// OpenStdout / OpenStderr return the matching file for streaming
// read. Returns transport.ErrJobNotFound if the file does not
// exist.
func (t *Transport) OpenStdout(ctx context.Context, lane int, s stem.Stem) (io.ReadCloser, error) {
	return t.openOutputFile(ctx, t.stdoutFile(lane, s))
}

func (t *Transport) OpenStderr(ctx context.Context, lane int, s stem.Stem) (io.ReadCloser, error) {
	return t.openOutputFile(ctx, t.stderrFile(lane, s))
}

func (t *Transport) openOutputFile(ctx context.Context, path string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, transport.ErrJobNotFound
		}
		return nil, fmt.Errorf("file: open %s: %w", path, err)
	}
	return f, nil
}

// RequestCancel creates an empty sentinel file at
// cancel/<lane>/<stem>. Idempotent: existing sentinels are left in
// place.
func (t *Transport) RequestCancel(ctx context.Context, lane int, s stem.Stem) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(t.laneCancelDir(lane), 0755); err != nil {
		return fmt.Errorf("file: mkdir lane %d cancel: %w", lane, err)
	}
	path := t.cancelFile(lane, s)
	if _, err := os.Stat(path); err == nil {
		return nil // already requested
	}
	return fsatomic.WriteFile(path, nil)
}

// SetSentinel creates or removes a global sentinel file at the
// shared-dir root.
func (t *Transport) SetSentinel(ctx context.Context, name string, present bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !isKnownSentinel(name) {
		return fmt.Errorf("file: unknown sentinel %q", name)
	}
	path := filepath.Join(t.root, name)
	if present {
		if _, err := os.Stat(path); err == nil {
			return nil // already present
		}
		if err := os.MkdirAll(t.root, 0755); err != nil {
			return fmt.Errorf("file: mkdir root: %w", err)
		}
		return fsatomic.WriteFile(path, nil)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("file: remove sentinel %s: %w", name, err)
	}
	return nil
}

// --- Responder methods ---

// WriteDispatch publishes the responder's dispatch capabilities.
func (t *Transport) WriteDispatch(ctx context.Context, d capability.Dispatch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(t.inboxDir(), 0755); err != nil {
		return fmt.Errorf("file: mkdir inbox: %w", err)
	}
	return fsatomic.WriteFile(t.dispatchFile(), d.Marshal())
}

// WriteStatus publishes a per-lane status atomically.
func (t *Transport) WriteStatus(ctx context.Context, lane int, s capability.Status) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(t.laneInboxDir(lane), 0755); err != nil {
		return fmt.Errorf("file: mkdir lane %d inbox: %w", lane, err)
	}
	return fsatomic.WriteFile(t.laneStatusFile(lane), s.Marshal())
}

// ListPending enumerates dispatchable files in the lane's inbox in
// stem-sorted order. `status.txt` and any `.tmp` files in flight
// are filtered out.
func (t *Transport) ListPending(ctx context.Context, lane int) ([]transport.PendingJob, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(t.laneInboxDir(lane))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("file: list pending lane=%d: %w", lane, err)
	}
	out := make([]transport.PendingJob, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == protocol.FileStatus {
			continue
		}
		if strings.HasSuffix(name, ".tmp") || strings.Contains(name, ".tmp.") {
			continue
		}
		s, ext, ok := splitStemExt(name)
		if !ok {
			continue
		}
		if _, err := stem.Parse(string(s)); err != nil {
			continue
		}
		out = append(out, transport.PendingJob{Stem: s, Ext: ext})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Stem < out[j].Stem })
	return out, nil
}

// OpenJob opens inbox/<lane>/<stem>.<ext> for read.
func (t *Transport) OpenJob(ctx context.Context, lane int, s stem.Stem, ext string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := t.jobFile(lane, s, ext)
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, transport.ErrJobNotFound
		}
		return nil, fmt.Errorf("file: open %s: %w", path, err)
	}
	return f, nil
}

// PublishResult writes stdout, stderr, then the result file
// atomically. The result file is written last so its presence is
// an unambiguous "done" signal for any polling submitter.
func (t *Transport) PublishResult(ctx context.Context, lane int, s stem.Stem, r outpost.Result, stdout, stderr []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(t.laneOutboxDir(lane), 0755); err != nil {
		return fmt.Errorf("file: mkdir lane %d outbox: %w", lane, err)
	}
	if err := fsatomic.WriteFile(t.stdoutFile(lane, s), stdout); err != nil {
		return err
	}
	if err := fsatomic.WriteFile(t.stderrFile(lane, s), stderr); err != nil {
		return err
	}
	return fsatomic.WriteFile(t.resultFile(lane, s), marshalResult(r))
}

// CheckSentinel reports whether a global sentinel file is present
// at the shared-dir root.
func (t *Transport) CheckSentinel(ctx context.Context, name string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !isKnownSentinel(name) {
		return false, fmt.Errorf("file: unknown sentinel %q", name)
	}
	_, err := os.Stat(filepath.Join(t.root, name))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("file: stat sentinel %s: %w", name, err)
}

// isKnownSentinel guards against typos in CLI or responder code
// that would otherwise silently poll for a misspelled sentinel.
func isKnownSentinel(name string) bool {
	switch name {
	case protocol.SentinelSTOP, protocol.SentinelPAUSE, protocol.SentinelRESTART:
		return true
	}
	return false
}

// CheckCancel reports whether cancel/<lane>/<stem> exists.
func (t *Transport) CheckCancel(ctx context.Context, lane int, s stem.Stem) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	_, err := os.Stat(t.cancelFile(lane, s))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("file: stat cancel lane=%d stem=%s: %w", lane, s, err)
}

// ArchiveJob moves the inbox file to log/<lane>/, then removes any
// cancel sentinel for the same stem.
func (t *Transport) ArchiveJob(ctx context.Context, lane int, s stem.Stem, ext string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(t.laneLogDir(lane), 0755); err != nil {
		return fmt.Errorf("file: mkdir lane %d log: %w", lane, err)
	}
	src := t.jobFile(lane, s, ext)
	dst := filepath.Join(t.laneLogDir(lane), string(s)+"."+ext)
	if err := fsatomic.Rename(src, dst); err != nil {
		// Missing source is not fatal: already archived or never
		// existed; caller probably called us after a shortcut path.
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	// Remove cancel sentinel, if any. Absence is fine.
	if err := os.Remove(t.cancelFile(lane, s)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("file: remove cancel sentinel: %w", err)
	}
	return nil
}

// Cleanup removes entries in log/, outbox/, and cancel/ older than
// the cutoff. Age is derived from the stem's timestamp prefix in
// the filename, not the file's mtime: this keeps retention
// deterministic across filesystem clock skew.
func (t *Transport) Cleanup(ctx context.Context, before time.Time) error {
	cutoffStem := before.UTC().Format("20060102_150405_000000")
	for _, root := range []string{
		filepath.Join(t.root, protocol.DirLog),
		filepath.Join(t.root, protocol.DirOutbox),
		filepath.Join(t.root, protocol.DirCancel),
	} {
		if err := cleanupTree(ctx, root, cutoffStem); err != nil {
			return err
		}
	}
	return nil
}

func cleanupTree(ctx context.Context, root, cutoff string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("file: readdir %s: %w", root, err)
	}
	for _, e := range entries {
		sub := filepath.Join(root, e.Name())
		if e.IsDir() {
			if err := cleanupTree(ctx, sub, cutoff); err != nil {
				return err
			}
			continue
		}
		// Extract stem-prefix portion (first 22 chars) and compare.
		name := e.Name()
		if len(name) < 22 {
			continue
		}
		prefix := name[:22]
		if prefix < cutoff {
			_ = os.Remove(sub)
		}
	}
	return nil
}

// --- helpers ---

// readFileCtx honors ctx cancellation before touching the
// filesystem. Filesystems don't natively support ctx cancellation,
// so this is a best-effort pre-check.
func readFileCtx(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// splitStemExt splits a filename like "20260423_..._probe.sh" into
// its stem ("20260423_..._probe") and extension ("sh"). Returns
// ok=false if the name has no extension.
func splitStemExt(name string) (stem.Stem, string, bool) {
	i := strings.LastIndexByte(name, '.')
	if i < 0 || i == len(name)-1 {
		return "", "", false
	}
	return stem.Stem(name[:i]), name[i+1:], true
}

// marshalResult serializes an outpost.Result to the result file's
// key=value format. This encoding lives in the file transport
// because it is the transport's concern; other transports may
// encode results differently.
func marshalResult(r outpost.Result) []byte {
	var buf bytes.Buffer
	writeResult(&buf, "stem", string(r.Stem))
	writeResult(&buf, "lane", strconv.Itoa(r.Lane))
	writeResult(&buf, "ext", r.Ext)
	writeResult(&buf, "label", r.Label)
	writeResult(&buf, "exit", strconv.Itoa(r.ExitCode))
	writeResult(&buf, "timeout", boolInt(r.TimedOut))
	writeResult(&buf, "cancelled", boolInt(r.Cancelled))
	writeResult(&buf, "started", formatResultTime(r.Started))
	writeResult(&buf, "finished", formatResultTime(r.Finished))
	writeResult(&buf, "stdout_bytes", strconv.FormatInt(r.StdoutBytes, 10))
	writeResult(&buf, "stderr_bytes", strconv.FormatInt(r.StderrBytes, 10))
	writeResult(&buf, "stdout_truncated", boolInt(r.StdoutTruncated))
	writeResult(&buf, "stderr_truncated", boolInt(r.StderrTruncated))
	return buf.Bytes()
}

func writeResult(w *bytes.Buffer, key, value string) {
	w.WriteString(key)
	w.WriteByte('=')
	w.WriteString(value)
	w.WriteByte('\n')
}

func boolInt(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func formatResultTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(capability.TimeFormat)
}

// unmarshalResult parses a .result file body. Symmetric with
// marshalResult; belongs in the same package so the wire format
// evolves in one place.
func unmarshalResult(data []byte) (outpost.Result, error) {
	kv := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.IndexByte(line, '=')
		if idx < 0 {
			return outpost.Result{}, fmt.Errorf("file: result: malformed line %q", line)
		}
		kv[strings.TrimSpace(line[:idx])] = strings.TrimSpace(line[idx+1:])
	}

	r := outpost.Result{
		Stem:  stem.Stem(kv["stem"]),
		Ext:   kv["ext"],
		Label: kv["label"],
	}
	var err error
	if r.Lane, err = atoi("lane", kv["lane"]); err != nil {
		return outpost.Result{}, err
	}
	if r.ExitCode, err = atoi("exit", kv["exit"]); err != nil {
		return outpost.Result{}, err
	}
	if r.StdoutBytes, err = atoi64("stdout_bytes", kv["stdout_bytes"]); err != nil {
		return outpost.Result{}, err
	}
	if r.StderrBytes, err = atoi64("stderr_bytes", kv["stderr_bytes"]); err != nil {
		return outpost.Result{}, err
	}
	r.TimedOut = kv["timeout"] == "1"
	r.Cancelled = kv["cancelled"] == "1"
	r.StdoutTruncated = kv["stdout_truncated"] == "1"
	r.StderrTruncated = kv["stderr_truncated"] == "1"
	if v := kv["started"]; v != "" {
		if r.Started, err = time.Parse(capability.TimeFormat, v); err != nil {
			return outpost.Result{}, fmt.Errorf("file: result: started: %w", err)
		}
	}
	if v := kv["finished"]; v != "" {
		if r.Finished, err = time.Parse(capability.TimeFormat, v); err != nil {
			return outpost.Result{}, fmt.Errorf("file: result: finished: %w", err)
		}
	}
	return r, nil
}

func atoi(key, value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("file: result: %s: %w", key, err)
	}
	return n, nil
}

func atoi64(key, value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("file: result: %s: %w", key, err)
	}
	return n, nil
}
