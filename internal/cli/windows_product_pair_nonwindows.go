//go:build !windows

package cli

import "errors"

func withWindowsProductNativeDefaults(d WindowsProductTxnDeps) WindowsProductTxnDeps {
	if d.RetainLegacy == nil {
		d.RetainLegacy = func(string) (string, error) {
			return "", errors.ErrUnsupported
		}
	}
	if d.RestoreNoReplace == nil {
		d.RestoreNoReplace = func(string, string) error {
			return errors.ErrUnsupported
		}
	}
	return d
}
