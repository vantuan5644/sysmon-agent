package main

import (
	"path/filepath"
	"strings"
)

// Windows' Installed apps list reads the version from the uninstall entry the
// release installer writes (DisplayVersion). The dashboard's Update button
// swaps only the binary, so after one the list kept naming the version the
// installer put there. The service rewrites that value at start
// (syncInstalledVersionRecord, install_record_windows.go) when it runs from
// the folder that entry describes.

// installRecordUpdate returns the DisplayVersion to write, if any: only for
// a released build, only for the install this binary runs from, and only when
// the value differs.
func installRecordUpdate(running, exeDir, installLocation, displayVersion string) (string, bool) {
	next := strings.TrimPrefix(strings.TrimSpace(running), "v")
	if next == "" || next == "dev" {
		return "", false
	}
	if strings.TrimSpace(installLocation) == "" || !sameInstallDir(exeDir, installLocation) {
		return "", false
	}
	if strings.TrimSpace(displayVersion) == next {
		return "", false
	}
	return next, true
}

func sameInstallDir(a, b string) bool {
	clean := func(p string) string {
		p = filepath.Clean(strings.Trim(strings.TrimSpace(p), `"`))
		return strings.TrimRight(p, `\/`)
	}
	return strings.EqualFold(clean(a), clean(b))
}
