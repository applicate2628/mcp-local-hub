package evidence

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// Presence is the tri-state outcome of asking the filesystem whether
// something is there. It exists because a plain bool cannot distinguish the
// two answers a caller must treat completely differently:
//
//   - PresenceAbsent is a VERIFIED fact ("the OS told us there is no such
//     entry"), and may legitimately drive a confident conclusion such as
//     "this buildtree was cleaned" or "this declared patch is missing".
//   - PresenceUnreadable is a NON-answer ("the OS refused to tell us"):
//     permission denied, a sharing violation, a transient I/O error, a
//     disconnected network path. It must never be reported as absence.
//
// Collapsing the second into the first is the single most repeated defect
// class across the vcpkg-mcp tools (`err == nil && fi.IsDir()` and friends):
// an access-denied Stat became "cleaned"/"not found"/"missing" and the tool
// then answered confidently from evidence it never actually read. Presence
// is the single owner of that distinction — no tool package re-derives it
// from a raw Stat error, exactly as Status is the single owner of the
// ok/failed/unknown vocabulary above.
type Presence string

const (
	// PresenceExists: the entry is there and has the expected kind.
	PresenceExists Presence = "exists"
	// PresenceAbsent: the OS positively reported no such entry (or an entry
	// of the wrong kind, e.g. a file where a directory was required). This
	// is a verified negative, safe to conclude from.
	PresenceAbsent Presence = "absent"
	// PresenceUnreadable: the probe itself failed, so existence is UNKNOWN.
	// Never report this as absence.
	PresenceUnreadable Presence = "unreadable"
)

// StatFunc is the one filesystem primitive probing needs. Every vcpkg-mcp
// package already injects its own os.Stat seam for determinism; this type
// lets them all share the probe logic without this package depending on any
// of their Deps structs (dependency direction stays pointing at this leaf).
type StatFunc func(path string) (os.FileInfo, error)

// ProbeDir reports whether path is an existing DIRECTORY.
//
// The returned error is non-nil for both PresenceAbsent and
// PresenceUnreadable and carries the operator-actionable detail (which path,
// which OS error); it is nil only for PresenceExists.
func ProbeDir(stat StatFunc, path string) (Presence, error) {
	return probe(stat, path, true)
}

// ProbeFile reports whether path is an existing NON-directory entry.
func ProbeFile(stat StatFunc, path string) (Presence, error) {
	return probe(stat, path, false)
}

func probe(stat StatFunc, path string, wantDir bool) (Presence, error) {
	if path == "" {
		return PresenceAbsent, errors.New("empty path")
	}
	if stat == nil {
		return PresenceUnreadable, errors.New("no stat function supplied")
	}
	fi, err := stat(path)
	if err == nil {
		if fi.IsDir() == wantDir {
			return PresenceExists, nil
		}
		// The entry is there but is the wrong kind. That is still a VERIFIED
		// negative for the question asked ("is there a directory here?"), not
		// a failure to read.
		kind := "a directory"
		if wantDir {
			kind = "not a directory"
		}
		return PresenceAbsent, fmt.Errorf("%s exists but is %s", path, kind)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return PresenceAbsent, err
	}
	// Permission denied, sharing violation, I/O error, disconnected network
	// path, name too long, ... — the filesystem declined to answer. Existence
	// is genuinely unknown.
	return PresenceUnreadable, err
}
