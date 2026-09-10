package main

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

type helperUnicodeString64 struct {
	length, maximum uint16
	_               uint32
	buffer          uint64
}

type helperUnicodeString32 struct {
	length, maximum uint16
	buffer          uint32
}

func main() {
	if len(os.Args) != 2 || os.Args[1] != "child" {
		os.Exit(2)
	}
	var restoreDOSPath func()
	defer func() {
		if restoreDOSPath != nil {
			restoreDOSPath()
		}
	}()
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		command := scanner.Text()
		if command == "ping" {
			fmt.Println("ok")
			continue
		}
		if target, ok := strings.CutPrefix(command, "chdir\t"); ok {
			if target == "" || os.Chdir(target) != nil {
				fmt.Println("error")
				continue
			}
			fmt.Println("ok")
			continue
		}
		if target, ok := strings.CutPrefix(command, "spoofdospath\t"); ok {
			if restoreDOSPath != nil {
				restoreDOSPath()
				restoreDOSPath = nil
			}
			var err error
			restoreDOSPath, err = spoofCurrentDirectoryDOSPath(target)
			if err != nil {
				fmt.Println("error")
				continue
			}
			fmt.Println("ok")
		}
	}
	if scanner.Err() != nil {
		os.Exit(3)
	}
}

func spoofCurrentDirectoryDOSPath(path string) (func(), error) {
	buffer, err := windows.UTF16FromString(path)
	if err != nil || len(buffer) < 2 || len(buffer)*2 > int(^uint16(0)) {
		return nil, fmt.Errorf("invalid spoof path")
	}
	var info windows.PROCESS_BASIC_INFORMATION
	var returned uint32
	if err := windows.NtQueryInformationProcess(windows.CurrentProcess(), windows.ProcessBasicInformation, unsafe.Pointer(&info), uint32(unsafe.Sizeof(info)), &returned); err != nil || info.PebBaseAddress == nil {
		return nil, fmt.Errorf("PEB unavailable")
	}
	peb := uintptr(unsafe.Pointer(info.PebBaseAddress))
	pointerSize := unsafe.Sizeof(uintptr(0))
	processParametersOffset := uintptr(0x20)
	currentDirectoryOffset := uintptr(0x38)
	if pointerSize == 4 {
		processParametersOffset = 0x10
		currentDirectoryOffset = 0x24
	}
	params := *(*uintptr)(unsafe.Pointer(peb + processParametersOffset))
	if params == 0 {
		return nil, fmt.Errorf("process parameters unavailable")
	}
	descriptor := unsafe.Pointer(params + currentDirectoryOffset)
	length := uint16((len(buffer) - 1) * 2)
	maximum := uint16(len(buffer) * 2)
	if pointerSize == 8 {
		original := *(*helperUnicodeString64)(descriptor)
		*(*helperUnicodeString64)(descriptor) = helperUnicodeString64{length: length, maximum: maximum, buffer: uint64(uintptr(unsafe.Pointer(&buffer[0])))}
		return func() {
			*(*helperUnicodeString64)(descriptor) = original
			runtime.KeepAlive(buffer)
		}, nil
	}
	original := *(*helperUnicodeString32)(descriptor)
	*(*helperUnicodeString32)(descriptor) = helperUnicodeString32{length: length, maximum: maximum, buffer: uint32(uintptr(unsafe.Pointer(&buffer[0])))}
	return func() {
		*(*helperUnicodeString32)(descriptor) = original
		runtime.KeepAlive(buffer)
	}, nil
}
