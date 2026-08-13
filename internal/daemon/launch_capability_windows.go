//go:build windows

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const bcryptUseSystemPreferredRNG = 0x00000002

var (
	bcryptDLL           = windows.NewLazySystemDLL("bcrypt.dll")
	bcryptGenRandomProc = bcryptDLL.NewProc("BCryptGenRandom")
)

type windowsLaunchCapabilityPipe struct {
	read  *os.File
	write *os.File
}

func productionLaunchCapabilityOps() launchCapabilityOps {
	return launchCapabilityOps{random32: windowsCNGFill32, zero32: windowsSecureZero32, openPipe: newWindowsLaunchCapabilityPipe}
}

func windowsCNGFill32(dst *[32]byte) error {
	if dst == nil {
		return fmt.Errorf("nil capability buffer")
	}
	r1, _, callErr := bcryptGenRandomProc.Call(0, uintptr(unsafe.Pointer(&dst[0])), uintptr(len(dst)), bcryptUseSystemPreferredRNG)
	if int32(r1) < 0 {
		return fmt.Errorf("BCryptGenRandom status 0x%08x: %v", uint32(r1), callErr)
	}
	return nil
}

func windowsSecureZero32(dst *[32]byte) {
	if dst != nil {
		for i := range dst {
			dst[i] = 0
		}
		runtime.KeepAlive(dst)
	}
}

func newWindowsLaunchCapabilityPipe() (launchCapabilityPipe, error) {
	read, write, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	readHandle := windows.Handle(read.Fd())
	writeHandle := windows.Handle(write.Fd())
	if err := windows.SetHandleInformation(readHandle, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
		_ = read.Close()
		_ = write.Close()
		return nil, err
	}
	if err := windows.SetHandleInformation(writeHandle, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		_ = read.Close()
		_ = write.Close()
		return nil, err
	}
	return &windowsLaunchCapabilityPipe{read: read, write: write}, nil
}

func (p *windowsLaunchCapabilityPipe) writeAndClose(value []byte) error {
	if p == nil || p.write == nil || len(value) != 32 {
		return fmt.Errorf("launch capability write requires exactly 32 bytes")
	}
	n, err := p.write.Write(value)
	closeErr := p.write.Close()
	p.write = nil
	if err != nil || n != 32 {
		return fmt.Errorf("write launch capability: wrote %d: %w", n, err)
	}
	if closeErr != nil {
		return fmt.Errorf("close launch capability write handle: %w", closeErr)
	}
	return nil
}

func (p *windowsLaunchCapabilityPipe) close() error {
	if p == nil {
		return nil
	}
	var first error
	if p.read != nil {
		first = p.read.Close()
		p.read = nil
	}
	if p.write != nil {
		if err := p.write.Close(); first == nil {
			first = err
		}
		p.write = nil
	}
	return first
}

func (p *windowsLaunchCapabilityPipe) apply(cmd *exec.Cmd) error {
	if p == nil || p.read == nil || cmd == nil {
		return fmt.Errorf("launch capability handle unavailable")
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	if cmd.SysProcAttr.Token != 0 || cmd.SysProcAttr.ParentProcess != 0 ||
		cmd.SysProcAttr.ProcessAttributes != nil || cmd.SysProcAttr.ThreadAttributes != nil ||
		cmd.SysProcAttr.NoInheritHandles || len(cmd.SysProcAttr.AdditionalInheritedHandles) != 0 ||
		cmd.SysProcAttr.CmdLine != "" || !cmd.SysProcAttr.HideWindow ||
		cmd.SysProcAttr.CreationFlags != windows.CREATE_NO_WINDOW {
		return fmt.Errorf("cst-direct-v1 rejects conflicting SysProcAttr")
	}
	cmd.SysProcAttr.NoInheritHandles = false
	cmd.SysProcAttr.AdditionalInheritedHandles = []syscall.Handle{syscall.Handle(p.read.Fd())}
	return nil
}

func openCstDirectIdentityFile(path string) (*os.File, error) {
	if !canonicalAbsolutePath(path) {
		return nil, fmt.Errorf("path is not canonical absolute")
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil || info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("identity path is not a regular non-reparse file")
	}
	finalBuf := make([]uint16, 32768)
	n, err := windows.GetFinalPathNameByHandle(h, &finalBuf[0], uint32(len(finalBuf)), 0)
	if err != nil || n == 0 || n >= uint32(len(finalBuf)) {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("resolve final identity path")
	}
	finalPath := windows.UTF16ToString(finalBuf[:n])
	finalPath = strings.TrimPrefix(finalPath, `\\?\`)
	if !strings.EqualFold(filepath.Clean(finalPath), filepath.Clean(path)) {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("identity path traverses an alias or reparse ancestor")
	}
	return os.NewFile(uintptr(h), path), nil
}

func (p *windowsLaunchCapabilityPipe) locator() uintptr {
	if p == nil || p.read == nil {
		return 0
	}
	return p.read.Fd()
}
