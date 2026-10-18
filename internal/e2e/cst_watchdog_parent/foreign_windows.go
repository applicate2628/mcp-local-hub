//go:build windows && cst_vptr_probe

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"mcp-local-hub/internal/process"
)

const f4MaxProtectedSourceBytes = 4 << 20

type f4SourceSnapshot struct {
	path     string
	original windows.Handle
	identity f4FileIdentity
	hash     [32]byte
	bytes    uint64
}

func f4OpenProtectedSource(path string) (windows.Handle, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return 0, errors.New("protected source path must be exact and absolute")
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
	if err := windows.NtCreateFile(&handle, windows.FILE_READ_DATA|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		oa, &iosb, &allocation, windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, 0, 0); err != nil {
		return 0, err
	}
	return handle, nil
}

func f4HashSourceHandle(handle windows.Handle, sink hash.Hash) (f4FileIdentity, uint64, error) {
	identity, err := f4Identity(handle, false)
	if err != nil {
		return identity, 0, err
	}
	var before windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &before); err != nil {
		return identity, 0, err
	}
	size := uint64(before.FileSizeHigh)<<32 | uint64(before.FileSizeLow)
	if size == 0 || size > f4MaxProtectedSourceBytes {
		return identity, size, fmt.Errorf("protected source size outside bound: %d", size)
	}
	remaining := size
	buf := make([]byte, 32*1024)
	for remaining != 0 {
		want := uint64(len(buf))
		if remaining < want {
			want = remaining
		}
		var read uint32
		if err := windows.ReadFile(handle, buf[:want], &read, nil); err != nil || read == 0 || uint64(read) > want {
			return identity, size, fmt.Errorf("protected source read: n=%d want<=%d err=%v", read, want, err)
		}
		if _, err := sink.Write(buf[:read]); err != nil {
			return identity, size, err
		}
		remaining -= uint64(read)
	}
	var after windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &after); err != nil {
		return identity, size, err
	}
	if after.VolumeSerialNumber != before.VolumeSerialNumber || after.FileIndexHigh != before.FileIndexHigh || after.FileIndexLow != before.FileIndexLow || after.FileSizeHigh != before.FileSizeHigh || after.FileSizeLow != before.FileSizeLow || after.LastWriteTime != before.LastWriteTime {
		return identity, size, errors.New("protected source changed during hash")
	}
	return identity, size, nil
}

func f4AdmitSource(path, expectedHash string) (*f4SourceSnapshot, error) {
	if len(expectedHash) != 64 {
		return nil, errors.New("missing exact source SHA-256")
	}
	handle, err := f4OpenProtectedSource(path)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = windows.CloseHandle(handle)
		}
	}()
	digest := sha256.New()
	identity, size, err := f4HashSourceHandle(handle, digest)
	if err != nil {
		return nil, err
	}
	var frozen [32]byte
	copy(frozen[:], digest.Sum(nil))
	if !strings.EqualFold(hex.EncodeToString(frozen[:]), expectedHash) {
		return nil, errors.New("protected source pre-hash differs from frozen input")
	}
	keep = true
	return &f4SourceSnapshot{path: path, original: handle, identity: identity, hash: frozen, bytes: size}, nil
}

func (source *f4SourceSnapshot) checkLiveName() error {
	if source == nil || source.original == 0 {
		return errors.New("missing retained protected-source handle")
	}
	live, err := f4OpenProtectedSource(source.path)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(live)
	digest := sha256.New()
	identity, size, err := f4HashSourceHandle(live, digest)
	if err != nil {
		return err
	}
	if identity != source.identity || size != source.bytes || !strings.EqualFold(hex.EncodeToString(digest.Sum(nil)), hex.EncodeToString(source.hash[:])) {
		return errors.New("protected source live name, identity or bytes drifted")
	}
	return nil
}

func (s *f4RunState) admitForeignAndSource() error {
	if s.config.ForeignPID == 0 {
		return errors.New("missing exact live foreign PID")
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, s.config.ForeignPID)
	if err != nil {
		return err
	}
	s.foreignOriginal = handle
	pid, err := f4ProcessID(handle)
	if err != nil || pid != s.config.ForeignPID {
		return fmt.Errorf("foreign original identity: pid=%d err=%v", pid, err)
	}
	if err := f4OriginalAlive(handle); err != nil {
		return err
	}
	s.foreignCreated, err = f4ProcessCreated(handle)
	if err != nil {
		return err
	}
	s.source, err = f4AdmitSource(s.config.SourcePath, s.config.SourceSHA256)
	return err
}

// checkForeignSource is the one parent oracle for both normal and failure
// returns. The foreign PID is never reopened after initial admission.
func (s *f4RunState) checkForeignSource(checkJobExclusion bool) (bool, bool, error) {
	if s.foreignOriginal == 0 || s.source == nil {
		return false, false, errors.New("foreign or protected source was not admitted")
	}
	pid, err := f4ProcessID(s.foreignOriginal)
	if err != nil || pid != s.config.ForeignPID {
		return false, false, fmt.Errorf("foreign original PID drift: pid=%d err=%v", pid, err)
	}
	if err := f4OriginalAlive(s.foreignOriginal); err != nil {
		return false, false, err
	}
	created, err := f4ProcessCreated(s.foreignOriginal)
	if err != nil || created != s.foreignCreated {
		return false, false, fmt.Errorf("foreign creation identity drift: %v", err)
	}
	if checkJobExclusion {
		if s.job == nil || s.job.Handle() == 0 {
			return false, false, errors.New("foreign Job exclusion uncheckable")
		}
		member, _, _, err := process.CSTObserverOriginalMemberForTest(s.job, s.foreignOriginal)
		if err != nil || member {
			return false, false, fmt.Errorf("foreign process entered outer Job or membership unknown: member=%v err=%v", member, err)
		}
	}
	if err := s.source.checkLiveName(); err != nil {
		return true, false, err
	}
	return true, true, nil
}
