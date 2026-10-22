package main

import "strings"

// storageExclusions is the -storage-exclude list (SYSMON_STORAGE_EXCLUDE):
// drives the agent leaves out of the storage panel and sends nothing to. A
// drive matches when its model, as the storage panel shows it, contains one of
// the entries, ignoring case and runs of whitespace.
//
// It exists for hardware under development. On BBLWIN (2026-10-22) an
// in-development USB recorder ("ZEPHYR USB DISK") took seconds to fail
// commands a finished drive answers at once. Reading a drive's size and free
// space is harmless to a normal drive, but a device that is still being built
// is better left to the tools working on it.
//
// Matching a part of the model rather than the whole of it keeps one entry
// working on both OSes, which name the same drive differently: Windows
// prefixes the vendor ("ZEPHYR ZEPHYR USB DISK"), Linux shows the product
// ("ZEPHYR USB DISK"). The cost is the fragment trap cpuDieTemperatureRank
// documents, so an entry should be distinctive enough not to match a real
// drive.
type storageExclusions []string

// parseStorageExclusions splits a comma-separated list, normalizing each entry
// the way excludes compares models. Empty entries are dropped.
func parseStorageExclusions(value string) storageExclusions {
	var exclusions storageExclusions
	for _, entry := range strings.Split(value, ",") {
		if entry = normalizeStorageModel(entry); entry != "" {
			exclusions = append(exclusions, entry)
		}
	}
	return exclusions
}

// excludes reports whether a drive with this model is left alone. A drive with
// no model is never excluded: there is nothing to match it by.
func (e storageExclusions) excludes(model string) bool {
	model = normalizeStorageModel(model)
	if model == "" {
		return false
	}
	for _, entry := range e {
		if strings.Contains(model, entry) {
			return true
		}
	}
	return false
}

func normalizeStorageModel(model string) string {
	return strings.ToLower(strings.Join(strings.Fields(model), " "))
}
