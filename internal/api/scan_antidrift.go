package api

import "sort"

// UnmanagedStdioCount reports scan rows that are direct stdio client entries
// the hub cannot recognize or migrate. The predicate mirrors classify():
// Status "unknown" means stdio was present but no hub-owned manifest matched.
func UnmanagedStdioCount(entries []ScanEntry) int {
	count := 0
	for _, entry := range entries {
		if isUnmanagedStdio(entry) {
			count++
		}
	}
	return count
}

// UnmanagedStdioNames returns the sorted server names for unmanaged stdio rows.
func UnmanagedStdioNames(entries []ScanEntry) []string {
	names := make([]string, 0, UnmanagedStdioCount(entries))
	for _, entry := range entries {
		if isUnmanagedStdio(entry) {
			names = append(names, entry.Name)
		}
	}
	sort.Strings(names)
	return names
}

func isUnmanagedStdio(entry ScanEntry) bool {
	if entry.Status != "unknown" {
		return false
	}
	for _, presence := range entry.ClientPresence {
		if presence.Transport == "stdio" && !presence.Disabled {
			return true
		}
	}
	return false
}
