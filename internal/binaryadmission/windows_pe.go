// Package binaryadmission owns admission of Windows product binaries before
// any build, install, staging, promotion, scheduler, or supervisor mutation.
// It is intentionally host-neutral so Linux release jobs can inspect Windows
// PE artifacts without executing them or importing debug/pe.
package binaryadmission

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

const (
	WindowsGUISubsystem       uint16 = 2
	WindowsCUISubsystem       uint16 = 3
	WindowsPEFormatErrorID           = "E_WINDOWS_PE_FORMAT"
	WindowsPESubsystemErrorID        = "E_WINDOWS_PE_SUBSYSTEM"

	maxWindowsPEHeaderOffset = 1 << 20
	windowsPEMaxSingleRead   = 94
)

type Error struct {
	ID          string
	Path        string
	Expected    uint16
	ExpectedAny []uint16
	Actual      uint16
	Cause       error
}

func (e *Error) Error() string {
	if e.ID == WindowsPESubsystemErrorID {
		if len(e.ExpectedAny) > 0 {
			return fmt.Sprintf("%s: %s: expected one of %v, actual %d", e.ID, e.Path, e.ExpectedAny, e.Actual)
		}
		return fmt.Sprintf("%s: %s: expected %d, actual %d", e.ID, e.Path, e.Expected, e.Actual)
	}
	return fmt.Sprintf("%s: %s: %v", e.ID, e.Path, e.Cause)
}

func (e *Error) Unwrap() error     { return e.Cause }
func (e *Error) FailureID() string { return e.ID }

// AdmitWindowsGUI validates path without executing it and admits only a
// regular PE32 or PE32+ image whose Subsystem is WINDOWS_GUI (2).
func AdmitWindowsGUI(path string) error {
	subsystem, err := readWindowsPESubsystemFile(path)
	if err != nil {
		return err
	}
	if subsystem != WindowsGUISubsystem {
		return &Error{ID: WindowsPESubsystemErrorID, Path: path, Expected: WindowsGUISubsystem, Actual: subsystem}
	}
	return nil
}

// AdmitWindowsUpgradePrior validates a retained canonical binary without
// executing it. Historical product binaries may use either WINDOWS_GUI (2)
// or WINDOWS_CUI (3); all other subsystem values and malformed PE images are
// rejected.
func AdmitWindowsUpgradePrior(path string) error {
	subsystem, err := readWindowsPESubsystemFile(path)
	if err != nil {
		return err
	}
	if subsystem != WindowsGUISubsystem && subsystem != WindowsCUISubsystem {
		return &Error{
			ID:          WindowsPESubsystemErrorID,
			Path:        path,
			ExpectedAny: []uint16{WindowsGUISubsystem, WindowsCUISubsystem},
			Actual:      subsystem,
		}
	}
	return nil
}

func readWindowsPESubsystemFile(path string) (uint16, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, formatError(path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, formatError(path, err)
	}
	if !info.Mode().IsRegular() {
		return 0, formatError(path, fmt.Errorf("not a regular file"))
	}
	subsystem, err := ReadWindowsPESubsystem(f, info.Size())
	if err != nil {
		return 0, formatError(path, err)
	}
	return subsystem, nil
}

func formatError(path string, err error) error {
	return &Error{ID: WindowsPEFormatErrorID, Path: path, Cause: err}
}

// ReadWindowsPESubsystem performs exactly two bounded random-access reads. It
// accepts only the PE32 and PE32+ optional-header shapes and never allocates
// from, maps, or executes candidate-controlled offsets.
func ReadWindowsPESubsystem(r io.ReaderAt, size int64) (uint16, error) {
	if r == nil {
		return 0, fmt.Errorf("nil reader")
	}
	var dos [64]byte
	if size < int64(len(dos)) {
		return 0, fmt.Errorf("truncated DOS header: size %d", size)
	}
	if _, err := r.ReadAt(dos[:], 0); err != nil {
		return 0, fmt.Errorf("read DOS header: %w", err)
	}
	if dos[0] != 'M' || dos[1] != 'Z' {
		return 0, fmt.Errorf("missing MZ signature")
	}
	peOffset := int64(binary.LittleEndian.Uint32(dos[0x3c:0x40]))
	const bytesThroughSubsystem = 24 + 70
	if peOffset < int64(len(dos)) || peOffset > maxWindowsPEHeaderOffset ||
		peOffset > size-bytesThroughSubsystem {
		return 0, fmt.Errorf("invalid PE header offset %d for size %d", peOffset, size)
	}
	var header [bytesThroughSubsystem]byte
	if _, err := r.ReadAt(header[:], peOffset); err != nil {
		return 0, fmt.Errorf("read PE header: %w", err)
	}
	if string(header[:4]) != "PE\x00\x00" {
		return 0, fmt.Errorf("missing PE signature")
	}
	optionalSize := binary.LittleEndian.Uint16(header[20:22])
	if optionalSize < 70 {
		return 0, fmt.Errorf("optional header too small: %d", optionalSize)
	}
	magic := binary.LittleEndian.Uint16(header[24:26])
	if magic != 0x10b && magic != 0x20b {
		return 0, fmt.Errorf("unsupported optional-header magic %#x", magic)
	}
	return binary.LittleEndian.Uint16(header[24+68 : 24+70]), nil
}
