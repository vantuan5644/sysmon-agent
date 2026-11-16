//go:build !windows

package main

// syncInstalledVersionRecord has nothing to do off Windows: no installer keeps
// a version record there.
func syncInstalledVersionRecord(func(string, ...any)) {}
