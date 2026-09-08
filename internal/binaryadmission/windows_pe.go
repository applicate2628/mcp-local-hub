// Package binaryadmission owns admission of Windows product binaries before
// any build, install, staging, promotion, scheduler, or supervisor mutation.
// It is intentionally host-neutral so Linux release jobs can inspect Windows
// PE artifacts without executing them or importing debug/pe.
package binaryadmission

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	WindowsGUISubsystem       uint16 = 2
	WindowsCUISubsystem       uint16 = 3
	WindowsPEFormatErrorID           = "E_WINDOWS_PE_FORMAT"
	WindowsPESubsystemErrorID        = "E_WINDOWS_PE_SUBSYSTEM"
	WindowsProductPairErrorID        = "E_WINDOWS_PRODUCT_PAIR_INCOMPLETE"

	maxWindowsPEHeaderOffset   = 1 << 20
	windowsPEMaxSingleRead     = 94
	windowsPEDLLCharacteristic = 0x2000
)

// WindowsArtifactRole names one member of the installed Windows product pair.
// The role is part of persisted identity; subsystem inference is validation,
// never a replacement for an explicit role.
type WindowsArtifactRole string

const (
	WindowsArtifactRoleCLI        WindowsArtifactRole = "cli"
	WindowsArtifactRoleWindowless WindowsArtifactRole = "windowless"
)

// WindowsArtifact binds product metadata and exact bytes to one explicit role.
type WindowsArtifact struct {
	Path      string              `json:"-"`
	Role      WindowsArtifactRole `json:"role"`
	Version   string              `json:"version,omitempty"`
	Commit    string              `json:"commit,omitempty"`
	BuildDate string              `json:"build_date,omitempty"`
	SHA256    string              `json:"sha256"`
}

// WindowsProductPair is the admitted role-keyed Windows product identity.
type WindowsProductPair struct {
	CLI        WindowsArtifact `json:"cli"`
	Windowless WindowsArtifact `json:"windowless"`
}

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

// ValidateWindowsNativeImage admits a regular PE32 or PE32+ process image
// without executing it. It is generic: no product role, VERSIONINFO, hash, or
// filename policy is imposed. DLL images are rejected because they are not
// launchable process roots.
func ValidateWindowsNativeImage(path string) error {
	if _, err := readWindowsPESubsystemFile(path); err != nil {
		return err
	}
	characteristics, err := readWindowsPECharacteristicsFile(path)
	if err != nil {
		return err
	}
	if characteristics&windowsPEDLLCharacteristic != 0 {
		return formatError(path, fmt.Errorf("PE image is a DLL"))
	}
	return nil
}

// AdmitWindowsRole validates one exact Windows artifact role and returns its
// SHA-256 after the PE role has been proven without executing the image.
func AdmitWindowsRole(artifact WindowsArtifact) (WindowsArtifact, error) {
	expected := uint16(0)
	switch artifact.Role {
	case WindowsArtifactRoleCLI:
		expected = WindowsCUISubsystem
	case WindowsArtifactRoleWindowless:
		expected = WindowsGUISubsystem
	default:
		return WindowsArtifact{}, fmt.Errorf("%s: %s: invalid artifact role %q", WindowsProductPairErrorID, artifact.Path, artifact.Role)
	}
	subsystem, err := readWindowsPESubsystemFile(artifact.Path)
	if err != nil {
		return WindowsArtifact{}, err
	}
	if subsystem != expected {
		return WindowsArtifact{}, &Error{ID: WindowsPESubsystemErrorID, Path: artifact.Path, Expected: expected, Actual: subsystem}
	}
	identity, err := readWindowsBuildIdentity(artifact.Path)
	if err != nil {
		return WindowsArtifact{}, fmt.Errorf("%s: %s: VERSIONINFO: %w", WindowsProductPairErrorID, artifact.Path, err)
	}
	if identity.Role != artifact.Role {
		return WindowsArtifact{}, fmt.Errorf("%s: %s: VERSIONINFO role %q disagrees with expected role %q", WindowsProductPairErrorID, artifact.Path, identity.Role, artifact.Role)
	}
	f, err := os.Open(artifact.Path)
	if err != nil {
		return WindowsArtifact{}, formatError(artifact.Path, err)
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, f)
	closeErr := f.Close()
	if copyErr != nil {
		return WindowsArtifact{}, formatError(artifact.Path, copyErr)
	}
	if closeErr != nil {
		return WindowsArtifact{}, formatError(artifact.Path, closeErr)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if artifact.SHA256 != "" && !strings.EqualFold(artifact.SHA256, actual) {
		return WindowsArtifact{}, fmt.Errorf("%s: %s: SHA-256 mismatch", WindowsProductPairErrorID, artifact.Path)
	}
	artifact.Version = identity.Version
	artifact.Commit = identity.Commit
	artifact.BuildDate = identity.BuildDate
	artifact.SHA256 = actual
	return artifact, nil
}

// AdmitWindowsProductPair requires the canonical CLI CUI role and the
// companion windowless GUI role to carry identical, non-empty build metadata.
// Both hashes are computed from the files read during this call.
func AdmitWindowsProductPair(cli, windowless WindowsArtifact) (WindowsProductPair, error) {
	if cli.Role != WindowsArtifactRoleCLI || windowless.Role != WindowsArtifactRoleWindowless {
		return WindowsProductPair{}, fmt.Errorf("%s: expected roles %q and %q, got %q and %q", WindowsProductPairErrorID, WindowsArtifactRoleCLI, WindowsArtifactRoleWindowless, cli.Role, windowless.Role)
	}
	admittedCLI, err := AdmitWindowsRole(cli)
	if err != nil {
		return WindowsProductPair{}, fmt.Errorf("%s: cli: %w", WindowsProductPairErrorID, err)
	}
	admittedWindowless, err := AdmitWindowsRole(windowless)
	if err != nil {
		return WindowsProductPair{}, fmt.Errorf("%s: windowless: %w", WindowsProductPairErrorID, err)
	}
	if admittedCLI.Version != admittedWindowless.Version || admittedCLI.Commit != admittedWindowless.Commit || admittedCLI.BuildDate != admittedWindowless.BuildDate {
		return WindowsProductPair{}, fmt.Errorf("%s: artifact-derived identity differs between roles", WindowsProductPairErrorID)
	}
	return WindowsProductPair{CLI: admittedCLI, Windowless: admittedWindowless}, nil
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

func readWindowsPECharacteristicsFile(path string) (uint16, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, formatError(path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = fmt.Errorf("not a regular file")
		}
		return 0, formatError(path, err)
	}
	var dos [64]byte
	if _, err := f.ReadAt(dos[:], 0); err != nil || string(dos[:2]) != "MZ" {
		if err == nil {
			err = fmt.Errorf("missing MZ signature")
		}
		return 0, formatError(path, err)
	}
	off := int64(binary.LittleEndian.Uint32(dos[0x3c:0x40]))
	if off < 64 || off > maxWindowsPEHeaderOffset || off > info.Size()-24 {
		return 0, formatError(path, fmt.Errorf("invalid PE header offset %d", off))
	}
	var coff [24]byte
	if _, err := f.ReadAt(coff[:], off); err != nil || string(coff[:4]) != "PE\x00\x00" {
		if err == nil {
			err = fmt.Errorf("missing PE signature")
		}
		return 0, formatError(path, err)
	}
	return binary.LittleEndian.Uint16(coff[22:24]), nil
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
