//go:build !windows

package main

import "os/exec"

func hideCodexQuotaWindow(cmd *exec.Cmd) {}
