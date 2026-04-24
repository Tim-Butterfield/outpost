//go:build windows

package platform

import (
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modMpr                 = windows.NewLazySystemDLL("mpr.dll")
	procWNetGetConnectionW = modMpr.NewProc("WNetGetConnectionW")
	// WNetGetUniversalNameW is the fallback: it consults every
	// registered network provider (including third-party ones like
	// Parallels' custom provider) rather than only the provider
	// that registered the drive letter. Parallels Shared Folders
	// show up with `net use` but WNetGetConnectionW returns
	// ERROR_NOT_CONNECTED for them; GetUniversalName handles it.
	procWNetGetUniversalNameW = modMpr.NewProc("WNetGetUniversalNameW")
)

// Win32 return codes we care about.
const (
	noError                = 0
	errorMoreData          = 234
	errorNotConnected      = 2250
	errorConnectionUnavail = 1201
	errorNoNetwork         = 1222
	errorNoNetOrBadPath    = 1203
	errorNotSupported      = 50
)

// UNIVERSAL_NAME_INFO_LEVEL for WNetGetUniversalNameW. Other levels
// (REMOTE_NAME_INFO_LEVEL=2) exist but universal is what we want.
const universalNameInfoLevel = 1

// universalNameInfo mirrors the Win32 UNIVERSAL_NAME_INFOW struct
// returned by WNetGetUniversalNameW at UNIVERSAL_NAME_INFO_LEVEL:
// a single pointer into the tail of the caller-provided buffer.
type universalNameInfo struct {
	LpUniversalName *uint16
}

// ResolveCWDSource translates a Windows CWD into the UNC path of its
// backing network share. Returns empty (with nil error) for local
// drives and for paths that are already UNC.
//
// Strategy (two-step):
//
//  1. WNetGetConnectionW on the drive letter. Fast, works for all
//     standard SMB mapped drives that went through MPR.
//
//  2. WNetGetUniversalNameW on the full CWD path. Consults every
//     registered network provider. Catches third-party providers
//     (notably Parallels Shared Folders) that register drives via
//     `net use` but don't implement the GetConnection entry point.
//
// Examples:
//
//   cwd="X:\some\path"
//     X: mapped to \\server\share
//     -> "\\server\share\some\path"
//
//   cwd="C:\Users\...\work" (local NTFS)
//     -> ""
//
//   cwd="\\server\share\proj" (already UNC)
//     -> "" (nothing to resolve)
func ResolveCWDSource(cwd string) (string, error) {
	clean := filepath.Clean(cwd)
	// Already a UNC path: caller already has the share form in cwd.
	if len(clean) >= 2 && clean[0] == '\\' && clean[1] == '\\' {
		return "", nil
	}
	// Expect a drive-letter-anchored absolute path.
	if len(clean) < 3 || clean[1] != ':' {
		return "", nil
	}

	// Step 1: try WNetGetConnectionW on the drive letter. When it
	// works, we only have the share prefix, so we splice on the
	// CWD tail ourselves.
	if unc, err := getUNCForDrive(clean[:2]); err == nil && unc != "" {
		tail := clean[2:] // "\github.com\..."
		for len(unc) > 0 && unc[len(unc)-1] == '\\' {
			unc = unc[:len(unc)-1]
		}
		return unc + tail, nil
	}

	// Step 2: fall back to WNetGetUniversalNameW on the full path.
	// When this works it returns the complete UNC form, tail
	// included, so we use it verbatim.
	if unc, err := getUniversalName(clean); err == nil && unc != "" {
		return unc, nil
	}

	// Neither API recognized it — treat as local.
	return "", nil
}

// getUNCForDrive invokes WNetGetConnectionW on a local device name
// ("X:") and returns the associated UNC path, or "" if the drive is
// not a network mapping (from MPR's perspective).
func getUNCForDrive(drive string) (string, error) {
	localName, err := syscall.UTF16PtrFromString(drive)
	if err != nil {
		return "", err
	}
	// Sizing call: MSDN-documented "query size" pattern with NULL
	// buffer; API returns ERROR_MORE_DATA and fills bufLen.
	var bufLen uint32
	ret, _, _ := procWNetGetConnectionW.Call(
		uintptr(unsafe.Pointer(localName)),
		0,
		uintptr(unsafe.Pointer(&bufLen)),
	)
	switch ret {
	case errorNotConnected, errorConnectionUnavail, errorNoNetwork, errorNoNetOrBadPath, errorNotSupported:
		// Local drive or provider doesn't implement GetConnection.
		return "", nil
	case noError:
		// Empty mapping (rare, but possible); treat as local.
		return "", nil
	case errorMoreData:
		// Expected: proceed to fetch.
	default:
		return "", syscall.Errno(ret)
	}
	if bufLen == 0 {
		return "", nil
	}
	buf := make([]uint16, bufLen)
	ret, _, _ = procWNetGetConnectionW.Call(
		uintptr(unsafe.Pointer(localName)),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&bufLen)),
	)
	if ret != noError {
		// Race (mapping dropped between sizing and fetch) or
		// transient failure — fall through to the caller's next
		// strategy.
		return "", nil
	}
	return windows.UTF16ToString(buf), nil
}

// getUniversalName invokes WNetGetUniversalNameW at
// UNIVERSAL_NAME_INFO_LEVEL on a full path. On success, returns the
// full UNC equivalent (with the CWD tail already appended by the
// provider).
//
// Buffer layout at UNIVERSAL_NAME_INFO_LEVEL:
//
//   [0:N)     UNIVERSAL_NAME_INFOW struct (one pointer field)
//   [N:)      UTF-16 NUL-terminated universal-name string
//
// The struct's lpUniversalName field points into the same buffer.
// We read that pointer, then treat it as a *uint16 and stringify.
func getUniversalName(path string) (string, error) {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	// Sizing call.
	var bufLen uint32
	ret, _, _ := procWNetGetUniversalNameW.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(universalNameInfoLevel),
		0,
		uintptr(unsafe.Pointer(&bufLen)),
	)
	switch ret {
	case errorNotConnected, errorConnectionUnavail, errorNoNetwork, errorNoNetOrBadPath, errorNotSupported:
		return "", nil
	case noError:
		return "", nil
	case errorMoreData:
		// Expected.
	default:
		return "", syscall.Errno(ret)
	}
	if bufLen == 0 {
		return "", nil
	}
	buf := make([]byte, bufLen)
	ret, _, _ = procWNetGetUniversalNameW.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(universalNameInfoLevel),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&bufLen)),
	)
	if ret != noError {
		return "", nil
	}
	// Reinterpret the head of the buffer as the UNIVERSAL_NAME_INFOW
	// struct. Its LpUniversalName field points into the tail of the
	// same buffer we still hold a reference to, so the string stays
	// alive until we're done reading it.
	if len(buf) < int(unsafe.Sizeof(universalNameInfo{})) {
		return "", nil
	}
	info := (*universalNameInfo)(unsafe.Pointer(&buf[0]))
	if info.LpUniversalName == nil {
		return "", nil
	}
	return windows.UTF16PtrToString(info.LpUniversalName), nil
}
