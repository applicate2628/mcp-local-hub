//go:build !windows

package cli

import (
	"errors"
	"os"
)

// The parent owns freshness and containment. This checks the admitted leaf,
// not ancestor ownership or resistance to a concurrent namespace replacement.
func checkCLITestHelperPath(path string, kind cliTestHelperPathKind) error {
	info, err := os.Lstat(path)
	if err != nil {
		if (kind == cliTestHelperOptionalFile || kind == cliTestHelperAbsentFile) && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	switch kind {
	case cliTestHelperDirectory:
		if info.IsDir() {
			return nil
		}
	case cliTestHelperExistingFile, cliTestHelperOptionalFile:
		if info.Mode().IsRegular() {
			return nil
		}
	}
	return errors.New("test helper path has an unexpected type or already exists")
}
