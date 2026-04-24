// Package fsatomic provides atomic file writes with SMB/NFS-aware
// rename retry.
//
// Every durable artifact outpost writes -- dispatch.txt, per-lane
// status.txt, result files, stdout/stderr captures -- goes through
// this package. Writes are atomic in the sense that a concurrent
// reader never observes a half-written file: data is first written
// to a temp file in the same directory, fsync'd to disk, then
// renamed over the target path.
//
// On network filesystems (SMB, some NFS clients) the rename can
// fail transiently when antivirus, Explorer, or a sync client
// briefly holds a handle on the target. Rename retries on these
// transient errors with a short backoff schedule before giving up.
//
// See DESIGN.md section 3.1 for where in the protocol atomic writes
// are required.
package fsatomic

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// retryDelays holds the sleep durations between rename attempts on
// transient error. Four attempts total: an initial attempt, then
// three retries with these delays between them.
var retryDelays = []time.Duration{
	10 * time.Millisecond,
	50 * time.Millisecond,
	250 * time.Millisecond,
}

// WriteFile writes data to path atomically: write to a temp file in
// the same directory, fsync, then rename over the target. If any
// step fails before the rename succeeds, the temp file is removed
// and the original path is left untouched.
//
// Data is guaranteed durable (fsync'd) before the rename publishes
// it, so a crash mid-write never exposes a partial file to readers
// of the final path.
//
// The rename transparently retries on transient filesystem errors
// common to network shares; see Rename.
func WriteFile(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmp, err := os.CreateTemp(dir, base+".*.tmp")
	if err != nil {
		return fmt.Errorf("fsatomic: create tmp in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()

	// Clean up the temp file unless rename succeeds.
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, werr := tmp.Write(data); werr != nil {
		_ = tmp.Close()
		return fmt.Errorf("fsatomic: write %s: %w", tmpPath, werr)
	}
	if serr := tmp.Sync(); serr != nil {
		_ = tmp.Close()
		return fmt.Errorf("fsatomic: sync %s: %w", tmpPath, serr)
	}
	if cerr := tmp.Close(); cerr != nil {
		return fmt.Errorf("fsatomic: close %s: %w", tmpPath, cerr)
	}
	if rerr := Rename(tmpPath, path); rerr != nil {
		return rerr
	}

	cleanup = false
	return nil
}

// Rename moves oldpath to newpath, retrying on transient filesystem
// errors common to network shares. On Unix, EBUSY is retried; on
// Windows, ERROR_ACCESS_DENIED and ERROR_SHARING_VIOLATION are
// retried.
//
// The retry schedule is four attempts total, with 10ms, 50ms, and
// 250ms sleeps between them (worst-case wall time ~310ms before
// failing).
//
// The returned error wraps the final underlying error; callers can
// use errors.Unwrap or errors.Is to reach it.
func Rename(oldpath, newpath string) error {
	return renameWithRetry(os.Rename, oldpath, newpath)
}

// renameFunc is the signature of os.Rename; injected by tests to
// simulate transient failures.
type renameFunc func(oldpath, newpath string) error

// renameWithRetry runs rf with the standard retry schedule. Factored
// out so tests can inject a fault-injecting renamer.
func renameWithRetry(rf renameFunc, oldpath, newpath string) error {
	for attempt := 0; attempt <= len(retryDelays); attempt++ {
		err := rf(oldpath, newpath)
		if err == nil {
			return nil
		}
		if !isRetriableRenameErr(err) {
			return fmt.Errorf("fsatomic: rename %s -> %s: %w", oldpath, newpath, err)
		}
		if attempt == len(retryDelays) {
			return fmt.Errorf("fsatomic: rename %s -> %s (after %d retries): %w", oldpath, newpath, len(retryDelays), err)
		}
		time.Sleep(retryDelays[attempt])
	}
	// Unreachable: the loop always returns via success, non-retriable
	// error, or exhausted retries.
	return errors.New("fsatomic: unreachable")
}
