package cli

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
	"mcp-local-hub/internal/process"
)

func productSnapshotEntry(pid uint32, name string) windows.ProcessEntry32 {
	entry := windows.ProcessEntry32{ProcessID: pid}
	copy(entry.ExeFile[:], utf16.Encode([]rune(name)))
	return entry
}

func TestProductImageSnapshotMatchesRecordedNames(t *testing.T) {
	// The fixture's foreign Python argv mentions mcphub.exe. Toolhelp's
	// recorded executable is still python.exe and cannot select that row.
	entries := []windows.ProcessEntry32{
		productSnapshotEntry(0, ""),
		productSnapshotEntry(77, "python.exe"),
		productSnapshotEntry(78, "MCPHUB.EXE"),
		productSnapshotEntry(79, "issued-stage.exe"),
		productSnapshotEntry(80, "renamed-mcphub.exe"),
	}
	ops, closed := productSnapshotFixture(t, entries, nil, nil)
	got, err := productImageSnapshotPIDs(t.Context(), []string{"mcphub.exe", "ISSUED-STAGE.EXE"}, ops)
	if err != nil || !reflect.DeepEqual(got, []int{78, 79}) || *closed != 1 {
		t.Fatalf("native image candidates = %v, %v; closes = %d", got, err, *closed)
	}

	ops, closed = productSnapshotFixture(t, entries[:2], nil, nil)
	got, err = productImageSnapshotPIDs(t.Context(), []string{"mcphub.exe"}, ops)
	if err != nil || len(got) != 0 || *closed != 1 {
		t.Fatalf("complete snapshot without matching images = %v, %v; closes = %d", got, err, *closed)
	}
}

func TestProductImageSnapshotRefusesIncompleteInventory(t *testing.T) {
	nativeErr := errors.New("fixture native snapshot failure")
	actor := productSnapshotEntry(78, "mcphub.exe")
	for _, tc := range []struct {
		name     string
		entries  []windows.ProcessEntry32
		nextErr  error
		closeErr error
		want     error
	}{
		{name: "empty first result", want: windows.ERROR_NO_MORE_FILES},
		{name: "idle row only", entries: []windows.ProcessEntry32{productSnapshotEntry(0, "")}},
		{name: "unnamed real process", entries: []windows.ProcessEntry32{productSnapshotEntry(78, "")}},
		{name: "partial inventory", entries: []windows.ProcessEntry32{actor}, nextErr: nativeErr, want: nativeErr},
		{name: "failed handle close", entries: []windows.ProcessEntry32{actor}, closeErr: nativeErr, want: nativeErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops, closed := productSnapshotFixture(t, tc.entries, tc.nextErr, tc.closeErr)
			got, err := productImageSnapshotPIDs(t.Context(), []string{"mcphub.exe"}, ops)
			if got != nil || err == nil || (tc.want != nil && !errors.Is(err, tc.want)) || *closed != 1 {
				t.Fatalf("incomplete snapshot returned %v, %v; closes = %d", got, err, *closed)
			}
		})
	}
}

func TestProductImageSnapshotCancellationDiscardsCandidates(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ops, closed := productSnapshotFixture(t, []windows.ProcessEntry32{productSnapshotEntry(78, "mcphub.exe")}, nil, nil)
	end := ops.next
	ops.next = func(handle windows.Handle, entry *windows.ProcessEntry32) error {
		cancel()
		return end(handle, entry)
	}
	got, err := productImageSnapshotPIDs(ctx, []string{"mcphub.exe"}, ops)
	if got != nil || !errors.Is(err, context.Canceled) || *closed != 1 {
		t.Fatalf("canceled snapshot returned %v, %v; closes = %d", got, err, *closed)
	}
}

func TestProductImageAbsenceReceivesHeldIdentity(t *testing.T) {
	root := t.TempDir()
	launcher := filepath.Join(root, "mcphub.exe")
	product := filepath.Join(root, "mcphub-product", "mcphub.exe")
	legacy := filepath.Join(root, "mcphub-windowless.exe")
	images := []string{product, launcher, legacy}
	birth := time.Unix(1700000000, 0)
	unknown := windows.ERROR_ACCESS_DENIED
	for _, tc := range []struct {
		name      string
		path      string
		base      string
		absent    bool
		err       error
		wantCheck windowsProductAbsenceCheck
	}{
		{name: "foreign same basename", path: filepath.Join(root, "different", "mcphub.exe"), base: "mcphub.exe"},
		{name: "live exact Product with only image name", path: strings.ToUpper(product), base: "mcphub.exe", wantCheck: windowsProductAbsenceProcessPresent},
		{name: "live exact Launcher", path: launcher, base: "mcphub.exe", wantCheck: windowsProductAbsenceProcessPresent},
		{name: "live exact legacy image", path: legacy, base: "mcphub-windowless.exe", wantCheck: windowsProductAbsenceProcessPresent},
		{name: "same held object ended", base: "mcphub.exe", absent: true},
		{name: "unknown identity", base: "mcphub.exe", err: unknown, wantCheck: windowsProductAbsenceProcessIdentity},
		{name: "incomplete identity", base: "mcphub.exe", wantCheck: windowsProductAbsenceProcessIdentity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A native row has an executable basename and PID. No command
			// line is available to admit or exclude either row.
			ops, closed := productSnapshotFixture(t, []windows.ProcessEntry32{
				productSnapshotEntry(77, "python.exe"),
				productSnapshotEntry(78, tc.base),
			}, nil, nil)
			calls := 0
			err := observeWindowsProductImageAbsence(t.Context(), images, nil, ops, func(_ context.Context, pid int) (process.PIDHandleObservation, error) {
				calls++
				if pid != 78 || *closed != 1 {
					t.Fatalf("received foreign PID or unsettled snapshot: %d, %d", pid, *closed)
				}
				if tc.absent || tc.err != nil {
					return process.PIDHandleObservation{ConfirmedAbsent: tc.absent}, tc.err
				}
				return process.PIDHandleObservation{Identity: process.PIDHandleIdentity{PID: pid, ExecutablePath: tc.path, StartedAt: birth}}, nil
			})
			if calls != 1 {
				t.Fatalf("selected image identity received %d times", calls)
			}
			if tc.wantCheck == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var observed *windowsProductAbsenceError
			if !errors.As(err, &observed) || observed.check != tc.wantCheck {
				t.Fatalf("wrong absence result: %v", err)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatalf("original native failure lost: %v", err)
			}
		})
	}
}

func TestCleanInstallProcessAbsenceRequiresAllImagePaths(t *testing.T) {
	root := t.TempDir()
	product := filepath.Join(root, "mcphub-product", "mcphub.exe")
	launcher := filepath.Join(root, "mcphub.exe")
	err := validateCleanInstallProcessAbsence(t.Context(), product, launcher, "")
	var refusal *windowsProductAbsenceError
	if !errors.As(err, &refusal) || refusal.check != windowsProductAbsenceIssuedImages {
		t.Fatalf("missing legacy path was credited: %v", err)
	}
}

func productSnapshotFixture(t *testing.T, rows []windows.ProcessEntry32, nextErr, closeErr error) (productProcessSnapshotOps, *int) {
	t.Helper()
	const owner windows.Handle = 91
	index, closed := 0, 0
	receive := func(handle windows.Handle, entry *windows.ProcessEntry32) error {
		if handle != owner || entry.Size != uint32(unsafe.Sizeof(*entry)) {
			t.Fatal("wrong original snapshot handle or native process record size")
		}
		if index == len(rows) {
			return windows.ERROR_NO_MORE_FILES
		}
		size := entry.Size
		*entry = rows[index]
		entry.Size = size
		index++
		return nil
	}
	ops := productProcessSnapshotOps{
		open: func(flags, pid uint32) (windows.Handle, error) {
			if flags != windows.TH32CS_SNAPPROCESS || pid != 0 {
				t.Fatal("fixture expected an all-process Toolhelp snapshot")
			}
			return owner, nil
		},
		first: receive,
		next: func(handle windows.Handle, entry *windows.ProcessEntry32) error {
			if nextErr != nil {
				return nextErr
			}
			return receive(handle, entry)
		},
		close: func(handle windows.Handle) error {
			if handle != owner {
				t.Fatal("fixture received a different close handle")
			}
			closed++
			return closeErr
		},
	}
	return ops, &closed
}
