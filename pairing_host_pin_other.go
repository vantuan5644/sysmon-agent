//go:build !linux && !windows

package main

type noHostPINDisplay struct{}

func newHostPINDisplay() hostPINDisplay { return noHostPINDisplay{} }

func (noHostPINDisplay) Ready() (bool, string) {
	return false, "the agent cannot show one on this OS"
}

func (noHostPINDisplay) Show(string, string) error { return nil }
