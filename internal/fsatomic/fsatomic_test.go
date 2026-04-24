package fsatomic

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// transientErrno returns the platform-appropriate errno that
// isRetriableRenameErr considers retriable.
func transientErrno() syscall.Errno {
	if runtime.GOOS == "windows" {
		return syscall.Errno(5) // ERROR_ACCESS_DENIED
	}
	return syscall.EBUSY
}

// transientErr wraps transientErrno in a PathError so it looks like
// something os.Rename would return.
func transientErr() error {
	return &fs.PathError{Op: "rename", Path: "test", Err: transientErrno()}
}

func TestWriteFile_Success(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "status.txt")
	data := []byte("lane=1\nstate=idle\n")

	if err := WriteFile(path, data); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("data mismatch:\n  got: %q\n want: %q", got, data)
	}

	// No leftover .tmp files in the directory.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover tmp file: %s", e.Name())
		}
	}
}

func TestWriteFile_Overwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "status.txt")

	if err := WriteFile(path, []byte("first")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := WriteFile(path, []byte("second")); err != nil {
		t.Fatalf("second write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("got %q, want %q", got, "second")
	}
}

func TestWriteFile_EmptyData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")

	if err := WriteFile(path, nil); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("size=%d, want 0", info.Size())
	}
}

func TestRenameWithRetry_ImmediateSuccess(t *testing.T) {
	calls := 0
	rf := func(o, n string) error {
		calls++
		return nil
	}
	if err := renameWithRetry(rf, "a", "b"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls=%d, want 1", calls)
	}
}

func TestRenameWithRetry_SuccessAfterRetries(t *testing.T) {
	tests := []struct {
		name       string
		failFirstN int
		wantCalls  int
	}{
		{"after 1 retry", 1, 2},
		{"after 2 retries", 2, 3},
		{"after 3 retries (last attempt)", 3, 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			rf := func(o, n string) error {
				calls++
				if calls <= tc.failFirstN {
					return transientErr()
				}
				return nil
			}
			if err := renameWithRetry(rf, "a", "b"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if calls != tc.wantCalls {
				t.Errorf("calls=%d, want %d", calls, tc.wantCalls)
			}
		})
	}
}

func TestRenameWithRetry_BudgetExhausted(t *testing.T) {
	calls := 0
	rf := func(o, n string) error {
		calls++
		return transientErr()
	}
	err := renameWithRetry(rf, "a", "b")
	if err == nil {
		t.Fatal("expected error after budget exhaustion")
	}
	if !errors.Is(err, transientErrno()) {
		t.Errorf("err does not wrap transient errno: %v", err)
	}
	if calls != 4 {
		t.Errorf("calls=%d, want 4 (initial + 3 retries)", calls)
	}
	// Error message should mention the retry count.
	if !strings.Contains(err.Error(), "after 3 retries") {
		t.Errorf("error should name retry count: %v", err)
	}
}

func TestRenameWithRetry_NonRetriable(t *testing.T) {
	calls := 0
	rf := func(o, n string) error {
		calls++
		return &fs.PathError{Op: "rename", Path: "test", Err: syscall.ENOENT}
	}
	err := renameWithRetry(rf, "a", "b")
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Errorf("calls=%d, want 1 (no retries for non-retriable)", calls)
	}
	if !errors.Is(err, syscall.ENOENT) {
		t.Errorf("err does not wrap ENOENT: %v", err)
	}
}

func TestRenameWithRetry_NilError(t *testing.T) {
	// Defensive: the factored-out function should behave correctly
	// even if a bizarre rename implementation returned nil on the
	// first try (normal path, but worth asserting once).
	rf := func(o, n string) error { return nil }
	if err := renameWithRetry(rf, "a", "b"); err != nil {
		t.Errorf("nil rename should succeed: %v", err)
	}
}
