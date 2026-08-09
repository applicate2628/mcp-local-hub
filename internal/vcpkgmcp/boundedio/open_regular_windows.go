//go:build windows

package boundedio

import (
	"fmt"
	"os"
)

// OpenRegular opens a path and validates the resulting handle, never a
// separately resolved path identity.
func OpenRegular(path string) (RegularFile, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if statErr != nil {
			return nil, statErr
		}
		return nil, fmt.Errorf("boundedio: refuse non-regular file %q (%s)", path, info.Mode().Type())
	}
	return file, nil
}
