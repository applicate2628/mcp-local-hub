//go:build windows && cst_vptr_probe

package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"mcp-local-hub/internal/e2e/cstwatchdogwire"
	"mcp-local-hub/internal/process"
)

const f4MarkerName = cstwatchdogwire.MarkerName
const f4EvidenceName = cstwatchdogwire.EvidenceName

type f4FileIdentity = cstwatchdogwire.FileIdentity
type f4RootMarker = cstwatchdogwire.RootMarker

type f4OwnedRoot struct {
	Path             string
	Token            [16]byte
	Identity         f4FileIdentity
	EvidenceIdentity f4FileIdentity
	dir              windows.Handle
}

func f4Identity(handle windows.Handle, directory bool) (f4FileIdentity, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return f4FileIdentity{}, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory || (!directory && info.NumberOfLinks != 1) {
		return f4FileIdentity{}, errors.New("linked, wrong-kind or ambiguous file identity")
	}
	return f4FileIdentity{Volume: info.VolumeSerialNumber, IndexHigh: info.FileIndexHigh, IndexLow: info.FileIndexLow}, nil
}

func f4OpenRootNoFollow(path string) (windows.Handle, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return 0, errors.New("root parent must be an exact absolute path")
	}
	native := `\??\` + path
	if strings.HasPrefix(path, `\\`) {
		native = `\??\UNC\` + strings.TrimPrefix(path, `\\`)
	}
	name, err := windows.NewNTUnicodeString(native)
	if err != nil {
		return 0, err
	}
	oa := &windows.OBJECT_ATTRIBUTES{ObjectName: name, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE}
	oa.Length = uint32(unsafe.Sizeof(*oa))
	var iosb windows.IO_STATUS_BLOCK
	var allocation int64
	var handle windows.Handle
	err = windows.NtCreateFile(&handle, windows.FILE_TRAVERSE|windows.FILE_READ_DATA|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		oa, &iosb, &allocation, windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, 0, 0)
	return handle, err
}

func f4CreateRelative(parent windows.Handle, leaf string, directory bool, data []byte) (windows.Handle, error) {
	if leaf == "" || leaf == "." || leaf == ".." || strings.ContainsAny(leaf, `\/:`) {
		return 0, errors.New("invalid one-component leaf")
	}
	name, err := windows.NewNTUnicodeString(leaf)
	if err != nil {
		return 0, err
	}
	oa := &windows.OBJECT_ATTRIBUTES{RootDirectory: parent, ObjectName: name, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE}
	oa.Length = uint32(unsafe.Sizeof(*oa))
	access := uint32(windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE)
	options := uint32(windows.FILE_DIRECTORY_FILE | windows.FILE_SYNCHRONOUS_IO_NONALERT | windows.FILE_OPEN_REPARSE_POINT)
	if !directory {
		access |= windows.FILE_WRITE_DATA
		options = windows.FILE_NON_DIRECTORY_FILE | windows.FILE_SYNCHRONOUS_IO_NONALERT | windows.FILE_OPEN_REPARSE_POINT
	}
	var iosb windows.IO_STATUS_BLOCK
	var allocation int64
	var handle windows.Handle
	if err := windows.NtCreateFile(&handle, access, oa, &iosb, &allocation, windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_CREATE, options, 0, 0); err != nil {
		return 0, err
	}
	if _, err := f4Identity(handle, directory); err != nil {
		_ = windows.CloseHandle(handle)
		return 0, err
	}
	if !directory && len(data) != 0 {
		var written uint32
		if err := windows.WriteFile(handle, data, &written, nil); err != nil || int(written) != len(data) {
			_ = windows.CloseHandle(handle)
			return 0, fmt.Errorf("write root leaf: wrote=%d want=%d: %w", written, len(data), err)
		}
		if err := windows.FlushFileBuffers(handle); err != nil {
			_ = windows.CloseHandle(handle)
			return 0, err
		}
	}
	return handle, nil
}

// f4CreateBoundRoot creates one exact directory before any root-writing child.
// If leaf creation fails, a non-nil root still names the owned quarantine.
func f4CreateBoundRoot(parentPath, cell string) (*f4OwnedRoot, error) {
	if cell == "" {
		return nil, errors.New("missing selected cell")
	}
	parent, err := f4OpenRootNoFollow(parentPath)
	if err != nil {
		return nil, fmt.Errorf("open exact root parent: %w", err)
	}
	defer windows.CloseHandle(parent)
	if _, err := f4Identity(parent, true); err != nil {
		return nil, fmt.Errorf("root parent identity: %w", err)
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	leaf := "cst-f4-" + hex.EncodeToString(token[:])
	handle, err := f4CreateRelative(parent, leaf, true, nil)
	if err != nil {
		return nil, fmt.Errorf("create exact root: %w", err)
	}
	identity, err := f4Identity(handle, true)
	if err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	root := &f4OwnedRoot{Path: filepath.Join(parentPath, leaf), Token: token, Identity: identity, dir: handle}
	marker, err := json.Marshal(f4RootMarker{Version: cstwatchdogwire.RootMarkerVersion, Cell: cell, Token: hex.EncodeToString(token[:]), Identity: identity, State: "pending"})
	if err != nil {
		return root, err
	}
	markerHandle, err := f4CreateRelative(handle, f4MarkerName, false, marker)
	if err != nil {
		return root, fmt.Errorf("create pending marker: %w", err)
	}
	if err := windows.CloseHandle(markerHandle); err != nil {
		return root, err
	}
	evidenceHandle, err := f4CreateRelative(handle, f4EvidenceName, false, nil)
	if err != nil {
		return root, fmt.Errorf("pre-register evidence leaf: %w", err)
	}
	root.EvidenceIdentity, err = f4Identity(evidenceHandle, false)
	if err != nil {
		_ = windows.CloseHandle(evidenceHandle)
		return root, fmt.Errorf("bind pre-registered evidence leaf: %w", err)
	}
	if err := windows.CloseHandle(evidenceHandle); err != nil {
		return root, err
	}
	return root, nil
}

func (r *f4OwnedRoot) Close() error {
	if r == nil || r.dir == 0 {
		return nil
	}
	handle := r.dir
	r.dir = 0
	return windows.CloseHandle(handle)
}

func f4NewOuterJob() (*process.Job, error) {
	job, err := process.NewJob(process.JobOptions{KillOnClose: true, BreakawayOK: false})
	if err != nil {
		return nil, err
	}
	var flags uint32
	getInfo := syscall.NewLazyDLL("kernel32.dll").NewProc("GetHandleInformation")
	result, _, callErr := getInfo.Call(uintptr(job.Handle()), uintptr(unsafe.Pointer(&flags)))
	if result == 0 || flags&windows.HANDLE_FLAG_INHERIT != 0 {
		_ = job.Close()
		return nil, fmt.Errorf("outer Job inheritability unavailable or unsafe: result=%d flags=%d cause=%v", result, flags, callErr)
	}
	return job, nil
}
