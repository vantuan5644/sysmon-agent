//go:build !windows

package main

// runAsService is a no-op on non-Windows platforms: there is no Service Control
// Manager, so the caller always falls through to the signal-driven console path.
func runAsService(name string, run serviceRunFunc) (handled bool, err error) {
	return false, nil
}
