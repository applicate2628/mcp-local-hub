package cli

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
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
