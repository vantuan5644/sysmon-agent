package main

import "strings"

// Windows privacy records (CapabilityAccessManager\ConsentStore) mark a running
// capture with LastUsedTimeStart set and LastUsedTimeStop zero. Windows writes
// the stop when a capture ends, but not always when the capturing process dies:
// on BBLWIN (2026-10-18) Discord was restarted mid-call, its record kept a zero
// stop for three hours, and the dashboard showed the mic busy with no capture
// session open anywhere. A capture cannot outlive its process, so a record only
// counts while its app has a process that was already running when the capture
// started. Whatever cannot be checked keeps the record: for a privacy indicator
// a missed real capture is worse than a stale "busy".

// privacyRecord is one ConsentStore record that claims a running capture.
type privacyRecord struct {
	// key is the record's path below the capability key: a package family name
	// for a packaged app, or "NonPackaged" then the image path with '#' for '\'.
	key   []string
	start uint64 // LastUsedTimeStart, FILETIME ticks
}

// privacyProcess is what privacyRecordRunning needs to know about one process.
type privacyProcess struct {
	exe      string // image file name from the process snapshot
	created  uint64 // creation time in FILETIME ticks; 0 when unreadable
	family   string // package family name; "" for an unpackaged process
	familyOK bool   // family was read, including a definite "no package"
}

// privacyRecordRunning reports whether rec can still describe a running
// capture. It returns false only when no running process can own the record, or
// every one that can was created after the capture started.
func privacyRecordRunning(rec privacyRecord, processes []privacyProcess) bool {
	nonPackaged := len(rec.key) == 2 && strings.EqualFold(rec.key[0], "NonPackaged")
	packaged := len(rec.key) == 1 && !strings.EqualFold(rec.key[0], "NonPackaged")
	if !nonPackaged && !packaged {
		return true // a key shape this check does not know
	}
	unverified := false
	for _, p := range processes {
		if nonPackaged {
			// Matched on the image file name, as a suffix of the key. Comparing
			// full paths would need the key and the process to spell every path
			// alike (SUBST drives, shares, 8.3 names); a name match can only add
			// owners, which keeps the record. Splitting the key on '#' instead
			// would cut short a file name that contains one.
			if p.exe == "" || !strings.HasSuffix(strings.ToLower(rec.key[1]), "#"+strings.ToLower(p.exe)) {
				continue
			}
		} else if !p.familyOK {
			unverified = true
			continue
		} else if !strings.EqualFold(p.family, rec.key[0]) {
			continue
		}
		if p.created == 0 || p.created <= rec.start {
			return true
		}
	}
	return unverified
}
