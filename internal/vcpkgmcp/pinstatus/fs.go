package pinstatus

import (
	"io"
	"os"
)

// FS abstracts the package's fixed-budget semantic-file read. When err is nil,
// Complete is false only when the returned data was truncated at
// MaxSemanticFileBytes; callers must treat that as incomplete evidence and
// must not parse it as a complete file.
// The seam keeps production reads and tests under the same resource contract.
type FS interface {
	ReadFile(path string) (data []byte, complete bool, err error)
}

type osFS struct{}

var _ FS = osFS{}

func (osFS) ReadFile(p string) ([]byte, bool, error) {
	return readSemanticFileWithOps(p, os.Stat, func(path string) (io.ReadCloser, error) { return os.Open(path) })
}

func readSemanticFileWithOps(p string, stat func(string) (os.FileInfo, error), open func(string) (io.ReadCloser, error)) ([]byte, bool, error) {
	info, err := stat(p)
	if err != nil {
		return nil, false, err
	}
	if info == nil || !info.Mode().IsRegular() {
		return nil, false, os.ErrInvalid
	}
	file, err := open(p)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()

	// The sentinel is derived solely from the untyped package constant. Its
	// conversion is checked at compile time, so an unrepresentable future limit
	// fails the build instead of wrapping into a fail-open reader budget.
	data, err := io.ReadAll(io.LimitReader(file, int64(MaxSemanticFileBytes+1)))
	if err != nil {
		return nil, false, err
	}
	if len(data) > MaxSemanticFileBytes {
		return data[:MaxSemanticFileBytes], false, nil
	}
	return data, true, nil
}

// DefaultFS wires FS to the real OS. Production callers use this; tests
// build their own fake FS.
func DefaultFS() FS { return osFS{} }
