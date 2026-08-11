package binaryadmission

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeWindowsPEFixture(t *testing.T, magic, subsystem uint16, mutate func([]byte)) string {
	t.Helper()
	data := make([]byte, 0x200)
	copy(data, "MZ")
	binary.LittleEndian.PutUint32(data[0x3c:0x40], 0x80)
	copy(data[0x80:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(data[0x80+20:0x80+22], 0xf0)
	binary.LittleEndian.PutUint16(data[0x80+24:0x80+26], magic)
	binary.LittleEndian.PutUint16(data[0x80+24+68:0x80+24+70], subsystem)
	if mutate != nil {
		mutate(data)
	}
	path := filepath.Join(t.TempDir(), "candidate.exe")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWindowsPEAdmission(t *testing.T) {
	for _, tc := range []struct {
		name  string
		magic uint16
	}{
		{"PE32", 0x10b},
		{"PE32+", 0x20b},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeWindowsPEFixture(t, tc.magic, WindowsGUISubsystem, nil)
			if err := AdmitWindowsGUI(path); err != nil {
				t.Fatalf("AdmitWindowsGUI: %v", err)
			}
		})
	}

	path := writeWindowsPEFixture(t, 0x20b, 3, nil)
	err := AdmitWindowsGUI(path)
	if err == nil || !strings.Contains(err.Error(), WindowsPESubsystemErrorID) ||
		!strings.Contains(err.Error(), "expected 2, actual 3") {
		t.Fatalf("CUI error=%v, want %s with expected/actual", err, WindowsPESubsystemErrorID)
	}
}

func TestWindowsPEMalformed(t *testing.T) {
	base := writeWindowsPEFixture(t, 0x20b, WindowsGUISubsystem, nil)
	valid, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		data []byte
	}{
		{"truncated DOS", valid[:32]},
		{"missing MZ", append([]byte("NO"), valid[2:]...)},
		{"offset before DOS end", func() []byte {
			b := append([]byte(nil), valid...)
			binary.LittleEndian.PutUint32(b[0x3c:0x40], 32)
			return b
		}()},
		{"offset beyond bound", func() []byte {
			b := append([]byte(nil), valid...)
			binary.LittleEndian.PutUint32(b[0x3c:0x40], maxWindowsPEHeaderOffset+1)
			return b
		}()},
		{"truncated PE", valid[:0x80+10]},
		{"missing signature", func() []byte { b := append([]byte(nil), valid...); copy(b[0x80:], "PX\x00\x00"); return b }()},
		{"small optional header", func() []byte {
			b := append([]byte(nil), valid...)
			binary.LittleEndian.PutUint16(b[0x80+20:0x80+22], 60)
			return b
		}()},
		{"unsupported magic", func() []byte {
			b := append([]byte(nil), valid...)
			binary.LittleEndian.PutUint16(b[0x80+24:0x80+26], 0x999)
			return b
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "candidate.exe")
			if err := os.WriteFile(path, tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			err := AdmitWindowsGUI(path)
			if err == nil || !strings.Contains(err.Error(), WindowsPEFormatErrorID) {
				t.Fatalf("error=%v, want %s", err, WindowsPEFormatErrorID)
			}
		})
	}
}

type boundedReaderAt struct {
	data    []byte
	maxRead int
	reads   int
}

func (r *boundedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.reads++
	if len(p) > r.maxRead {
		return 0, errors.New("read exceeded bound")
	}
	if off < 0 || off >= int64(len(r.data)) {
		return 0, os.ErrInvalid
	}
	n := copy(p, r.data[off:])
	if n != len(p) {
		return n, errors.New("short read")
	}
	return n, nil
}

func TestWindowsPEBoundedRead(t *testing.T) {
	data := make([]byte, 0x200)
	copy(data, "MZ")
	binary.LittleEndian.PutUint32(data[0x3c:0x40], 0x80)
	copy(data[0x80:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(data[0x80+20:0x80+22], 0xf0)
	binary.LittleEndian.PutUint16(data[0x80+24:0x80+26], 0x20b)
	binary.LittleEndian.PutUint16(data[0x80+24+68:0x80+24+70], WindowsGUISubsystem)
	r := &boundedReaderAt{data: data, maxRead: windowsPEMaxSingleRead}
	subsystem, err := ReadWindowsPESubsystem(r, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if subsystem != WindowsGUISubsystem {
		t.Fatalf("subsystem=%d", subsystem)
	}
	if r.reads != 2 {
		t.Fatalf("read calls=%d, want exactly 2 bounded reads", r.reads)
	}
}
