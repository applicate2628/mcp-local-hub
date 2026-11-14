//go:build windows

package process

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestObservedAttributeStartupInfoNativeLayout(t *testing.T) {
	var actual procThreadAttributeStartupInfo
	var provider windows.StartupInfoEx
	if unsafe.Sizeof(actual) != unsafe.Sizeof(provider) ||
		unsafe.Alignof(actual) != unsafe.Alignof(provider) ||
		unsafe.Offsetof(actual.StartupInfo) != 0 ||
		unsafe.Offsetof(actual.ProcThreadAttributeList) != unsafe.Offsetof(provider.ProcThreadAttributeList) {
		t.Fatal("native attribute-list address changed the STARTUPINFOEX ABI")
	}
}
