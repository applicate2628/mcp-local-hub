//go:build !windows

package binaryadmission

import "os"

func publisherSourceReparse(info os.FileInfo) bool { return info.Mode() & os.ModeSymlink != 0 }
