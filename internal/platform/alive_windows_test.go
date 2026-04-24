//go:build windows

package platform_test

import "golang.org/x/sys/windows"

// stillActive is the value GetExitCodeProcess returns for a
// process that has not yet exited (Windows STILL_ACTIVE = 259).
const stillActive = 259

// processAlive reports whether pid is currently live. Opens a
// limited-information handle and checks whether the process has
// an exit code yet.
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
