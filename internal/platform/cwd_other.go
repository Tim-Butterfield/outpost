//go:build unix && !linux && !darwin

package platform

// ResolveCWDSource is a no-op stub on BSD / Solaris / AIX builds.
// outpost does not target those platforms today; if a concrete
// target appears, extend this with the platform-specific mount
// lookup (getmntinfo on *BSD, etc.).
func ResolveCWDSource(cwd string) (string, error) {
	return "", nil
}
