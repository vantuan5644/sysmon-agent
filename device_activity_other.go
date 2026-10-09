//go:build !windows && !linux

package main

import (
	"context"
	"errors"
)

type unsupportedDeviceBackend struct{}

func newDeviceBackend() deviceBackend { return unsupportedDeviceBackend{} }
func (unsupportedDeviceBackend) Activity(context.Context) (DeviceActivity, DeviceActivity) {
	a := unknownDeviceActivity("device activity is unsupported on this platform")
	return a, a
}
func (unsupportedDeviceBackend) Cameras(context.Context) ([]cameraDevice, int, error) {
	return nil, 0, errors.New("camera control is unsupported on this platform")
}
func (unsupportedDeviceBackend) SetCamera(context.Context, cameraDevice, bool) error {
	return errors.New("camera control is unsupported on this platform")
}
