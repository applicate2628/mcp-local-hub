//go:build windows

package cli

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

const privateSerenaMigrationMaxControlBytes = 16 << 10

// privateSerenaMigrationFileCustodyError keeps the actual file reachable after one unreceived Close.
// Callers retain the complete error and stop; they must never retry Close.
type privateSerenaMigrationFileCustodyError struct {
	File  *os.File
	Cause error
}

func (err *privateSerenaMigrationFileCustodyError) Error() string {
	return "original control-file close unreceived: " + err.Cause.Error()
}

func (err *privateSerenaMigrationFileCustodyError) Unwrap() error { return err.Cause }

func closePrivateSerenaMigrationFile(file *os.File, primary error) error {
	closeErr := file.Close()
	cause := errors.Join(primary, closeErr)
	if closeErr != nil {
		return &privateSerenaMigrationFileCustodyError{File: file, Cause: cause}
	}
	return cause
}

// checkPrivateSerenaMigrationPath rejects nonordinary existing components. Freshness, protected
// DACL and exclusivity of the containing scope remain the caller's obligation.
func checkPrivateSerenaMigrationPath(path string, directory bool) error {
	const root = "D:\\dev\\ChatGPT"
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("absolute clean D-only control path required")
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.Contains(rel, ":") || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return errors.New("control path escapes the original D work area")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if err := privateSerenaMigrationOrdinaryAttributes(info); err != nil {
		return err
	}
	if info.IsDir() != directory || (!directory && !info.Mode().IsRegular()) {
		return errors.New("ordinary control file or directory required")
	}
	if strings.EqualFold(path, root) {
		return nil
	}
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		parentInfo, err := os.Lstat(parent)
		if err != nil {
			return err
		}
		if err := privateSerenaMigrationOrdinaryAttributes(parentInfo); err != nil {
			return err
		}
		if !parentInfo.IsDir() {
			return errors.New("ordinary control path ancestors required")
		}
		if strings.EqualFold(parent, root) {
			break
		}
		if parent == filepath.Dir(parent) {
			return errors.New("control path did not reach its D work root")
		}
	}
	return nil
}

func privateSerenaMigrationOrdinaryAttributes(info os.FileInfo) error {
	attributes, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok || attributes == nil {
		return errors.New("original Windows file attributes unavailable")
	}
	if info.Mode()&os.ModeSymlink != 0 || attributes.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("reparse control path component refused")
	}
	return nil
}

// publishPrivateSerenaMigrationCase preserves the existing one-shot, bounded, no-replace protocol.
// Partial .pending files are evidence and are never removed here.
func publishPrivateSerenaMigrationCase(path string, value any) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("absolute clean publication path required")
	}
	if err := checkPrivateSerenaMigrationPath(filepath.Dir(path), true); err != nil {
		return err
	}
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(body) == 0 || len(body) > privateSerenaMigrationMaxControlBytes || body[0] != '{' || body[len(body)-1] != '}' {
		return errors.New("case data must be one bounded JSON object")
	}
	staged := path + ".pending"
	file, err := os.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	n, writeErr := file.Write(body)
	if n != len(body) {
		writeErr = errors.Join(writeErr, io.ErrShortWrite)
	}
	syncErr := file.Sync()
	if err := closePrivateSerenaMigrationFile(file, errors.Join(writeErr, syncErr)); err != nil {
		var custody *privateSerenaMigrationFileCustodyError
		if errors.As(err, &custody) {
			privateSerenaMigrationFailStop(custody, "control-file-close", err)
		}
		return err
	}
	from, fromErr := windows.UTF16PtrFromString(staged)
	to, toErr := windows.UTF16PtrFromString(path)
	if err := errors.Join(fromErr, toErr); err != nil {
		return err
	}
	// No REPLACE_EXISTING: each control frame is published at most once.
	return windows.MoveFileEx(from, to, 0)
}

