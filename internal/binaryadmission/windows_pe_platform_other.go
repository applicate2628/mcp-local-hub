//go:build !windows

package binaryadmission

import "os"

func openWindowsRuntimeProduct(path string) (*os.File, error) { return os.Open(path) }
