//go:build !windows

package main

import (
	"os"
	"syscall"
)

// ensurePrivateDir creates dir (and its parents) and makes it owner-only.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// matchDirOwner gives a file the folder's owner when root writes it, so
// `sudo sysmon-agent -pair` leaves a window.json the service's own account
// can still read.
func matchDirOwner(f *os.File, dir string) {
	if os.Geteuid() != 0 {
		return
	}
	info, err := os.Stat(dir)
	if err != nil {
		return
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		_ = f.Chown(int(st.Uid), int(st.Gid))
	}
}
