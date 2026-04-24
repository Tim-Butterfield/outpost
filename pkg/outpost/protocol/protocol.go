// Package protocol defines the wire-format constants of the outpost
// file-RPC protocol: version number, exit codes, sentinel filenames,
// directory names, and result-artifact extensions. No behavior.
//
// See DESIGN.md §3 for how these values appear in `dispatch.txt`,
// per-lane `status.txt`, result files, and the sentinel set.
package protocol

import "strconv"

// Version is the integer advertised as `protocol_version` in
// `inbox/dispatch.txt`. A submitter reads this on first contact and
// refuses to submit if it does not recognise the value. Bump only
// on protocol-breaking changes; additive fields do not bump.
const Version = 1

// Responder-originated exit codes returned in `<stem>.result`.
// Worker exit codes pass through unchanged; these values only
// appear when the responder itself is reporting the outcome.
const (
	ExitCodeTimeout   = 124 // worker exceeded its per-job timeout
	ExitCodeDispatch  = 125 // responder could not dispatch the job
	ExitCodeCancelled = 126 // submitter requested cancellation
)

// Global sentinel filenames at the shared-dir root. Presence, not
// content, is what the responder checks on each poll cycle.
const (
	SentinelSTOP    = "STOP"
	SentinelPAUSE   = "PAUSE"
	SentinelRESTART = "RESTART"
)

// Per-shared-dir subdirectory names. See DESIGN.md §3.1 for the
// full tree diagram.
const (
	DirInbox  = "inbox"
	DirOutbox = "outbox"
	DirCancel = "cancel"
	DirLog    = "log"
)

// Well-known filenames inside the shared-dir tree.
const (
	FileDispatch = "dispatch.txt" // <shared-dir>/inbox/dispatch.txt
	FileStatus   = "status.txt"   // <shared-dir>/inbox/<N>/status.txt
	FileEventLog = "outpost.log"  // <shared-dir>/outpost.log
)

// Extensions of the three result artifacts a responder writes to
// the lane outbox after a job completes.
const (
	ExtResult = ".result" // exit / timing / byte counts / truncation flags
	ExtStdout = ".stdout" // captured stdout, capped at MaxOutputBytes
	ExtStderr = ".stderr" // captured stderr, capped at MaxOutputBytes
)

// MaxOutputBytes is the per-stream cap on captured stdout or
// stderr per job. Workers producing more have the extra silently
// discarded and the corresponding `*_truncated=1` flag set in the
// `.result` file.
const MaxOutputBytes = 100 * 1024 * 1024

// CommentCharForExt returns the line-comment marker the dispatcher
// recognizes for per-stem timeout headers (DESIGN.md §3.4) in each
// supported extension. An empty return means "no header parsing
// for this extension" -- the submitter and responder are expected
// to agree on the set here.
//
// Shared between the subprocess dispatcher (parses the header) and
// the client's Submit (injects the header when Job.Timeout is set).
// Kept here so both sides read from one source of truth.
func CommentCharForExt(ext string) string {
	switch ext {
	case "sh", "zsh", "py", "ps1":
		return "#"
	case "cmd", "bat":
		return "REM"
	case "btm":
		return "::"
	}
	return ""
}

// FormatTimeoutHeader returns a header line that the dispatcher's
// parseTimeoutHeader will recognize and honor. Units are whole
// seconds, clamped to the parser's accepted range (1..86400).
// Returns nil when ext has no comment char, leaving the content
// untouched -- the submitter can still fall back to the CLI's
// --timeout flag being advisory for those extensions.
//
// The returned slice is a single line ending in '\n', safe to
// prepend to Job.Content via append(header, content...).
func FormatTimeoutHeader(ext string, seconds int) []byte {
	marker := CommentCharForExt(ext)
	if marker == "" {
		return nil
	}
	if seconds < 1 {
		seconds = 1
	}
	if seconds > 86400 {
		seconds = 86400
	}
	return []byte(marker + " timeout=" + strconv.Itoa(seconds) + "\n")
}
