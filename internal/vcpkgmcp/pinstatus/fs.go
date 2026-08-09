package pinstatus

import (
	"io"
	"os"

	"mcp-local-hub/internal/vcpkgmcp/boundedio"
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
	return readSemanticFileWithOpener(p, boundedio.OpenRegular)
}

func readSemanticFileWithOpener(p string, open func(string) (boundedio.RegularFile, error)) ([]byte, bool, error) {
	file, err := open(p)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info == nil || !info.Mode().IsRegular() {
		if err != nil {
			return nil, false, err
		}
		return nil, false, os.ErrInvalid
	}

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
