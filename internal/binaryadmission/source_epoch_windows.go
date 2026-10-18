//go:build windows

package binaryadmission

import("os";"syscall")

func publisherSourceReparse(info os.FileInfo) bool {
	if info.Mode() & os.ModeSymlink != 0 { return true }
	data,ok:=info.Sys().(*syscall.Win32FileAttributeData)
	return !ok || data.FileAttributes & syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}
