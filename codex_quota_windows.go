package main

import (
	"os/exec"
	"syscall"
)

func hideCodexQuotaWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
