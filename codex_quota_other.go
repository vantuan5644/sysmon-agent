//go:build !windows

package main

import "os/exec"

func hideChildWindow(cmd *exec.Cmd) {}
