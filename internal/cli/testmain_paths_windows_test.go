//go:build windows

package cli

import (
	"errors"

	"golang.org/x/sys/windows"
)

// The parent owns freshness and containment. This checks the admitted leaf,
// not ancestor ownership or resistance to a concurrent namespace replacement.
func checkCLITestHelperPath(path string, kind cliTestHelperPathKind) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributes, err := windows.GetFileAttributes(name)
	if err != nil {
		if (kind == cliTestHelperOptionalFile || kind == cliTestHelperAbsentFile) && errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return nil
		}
		return err
	}
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("test helper path is a reparse point")
	}
	switch kind {
	case cliTestHelperDirectory:
		if attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
			return nil
		}
	case cliTestHelperExistingFile, cliTestHelperOptionalFile:
		if attributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_DEVICE) == 0 {
			return nil
		}
	}
	return errors.New("test helper path has an unexpected type or already exists")
}
