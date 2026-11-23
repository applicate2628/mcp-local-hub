//go:build !windows

package binaryadmission

import "os"

func runtimeAttributes(info os.FileInfo) uint32 {
	if info.Mode().Perm()&0200 == 0 {
		return 1
	}
	return 0
}
func runtimeReparse(info os.FileInfo) bool { return info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 }
