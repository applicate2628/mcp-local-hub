package pinstatus

import "os"

// FS abstracts the one filesystem operation this package needs, mirroring
// the discovery/lastfailure packages' determinism seam so tests never touch
// a real path (see the "Determinism and ambient-input control" rule).
type FS interface {
	ReadFile(path string) ([]byte, error)
}

type osFS struct{}

func (osFS) ReadFile(p string) ([]byte, error) { return os.ReadFile(p) }

// DefaultFS wires FS to the real OS. Production callers use this; tests
// build their own fake FS.
func DefaultFS() FS { return osFS{} }
