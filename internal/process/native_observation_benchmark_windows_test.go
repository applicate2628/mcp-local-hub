//go:build windows

package process

import (
	"fmt"
	"os"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type nativeObservation struct {
	Created       time.Time
	ResidentBytes uint64
}

// Test-only comparison using the same native APIs/struct as the existing Go
// helpers, but borrowing one handle for both fields. It never closes that handle.
func readRetainedObservation(h windows.Handle) (nativeObservation, error) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return nativeObservation{}, err
	}
	var counters processMemoryCounters
	counters.CB = uint32(unsafe.Sizeof(counters))
	ok, _, callErr := procGetProcessMemoryInfo.Call(
		uintptr(h), uintptr(unsafe.Pointer(&counters)), uintptr(counters.CB),
	)
	if ok == 0 {
		return nativeObservation{}, fmt.Errorf("native memory observation failed: %v", callErr)
	}
	return nativeObservation{
		Created:       time.Unix(0, creation.Nanoseconds()).UTC(),
		ResidentBytes: uint64(counters.WorkingSetSize),
	}, nil
}

func TestNativeObservationMatchesSelfCreation(t *testing.T) {
	pid := os.Getpid()
	expected, ok := ProcessStartTime(pid)
	if !ok {
		t.Fatal("existing creation observation unavailable")
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := windows.CloseHandle(h); err != nil {
			t.Error(err)
		}
	}()
	for i := 0; i < 32; i++ {
		sample, err := readRetainedObservation(h)
		if err != nil {
			t.Fatal(err)
		}
		if !sample.Created.Equal(expected) || sample.ResidentBytes == 0 {
			t.Fatalf("observation mismatch: created=%s want=%s rss=%d", sample.Created, expected, sample.ResidentBytes)
		}
	}
	// A diagnostic read borrows the handle; it must not consume or close it.
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		t.Fatalf("borrowed handle no longer live: %v", err)
	}
}
func TestNativeObservationFailureIsNotAbsent(t *testing.T) {
	if sample, err := readRetainedObservation(0); err == nil {
		t.Fatalf("invalid handle became a valid zero observation: %+v", sample)
	}
	for _, pid := range []int{0, -1} {
		if _, ok := ProcessStartTime(pid); ok {
			t.Fatal("invalid PID time accepted")
		}
		if _, ok := ResidentSetSizeByPID(pid); ok {
			t.Fatal("invalid PID memory accepted")
		}
	}
}

// Measures only this process. No machine enumeration, command-line collection,
// WMI/CIM, shell, subprocess, policy decision, or permission to terminate exists.
func BenchmarkNativeProcessObservation(b *testing.B) {
	pid := os.Getpid()
	expected, ok := ProcessStartTime(pid)
	if !ok {
		b.Fatal("existing start-time unavailable")
	}
	for _, mode := range []string{"existing_two_open", "one_open_per_sample", "retained_handle"} {
		b.Run(mode, func(b *testing.B) {
			var retained windows.Handle
			if mode == "retained_handle" {
				var err error
				retained, err = windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() {
					if err := windows.CloseHandle(retained); err != nil {
						b.Error(err)
					}
				})
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var sample nativeObservation
				var err error
				switch mode {
				case "existing_two_open":
					var valid bool
					sample.Created, valid = ProcessStartTime(pid)
					if !valid {
						b.Fatal("existing start-time failed")
					}
					sample.ResidentBytes, valid = ResidentSetSizeByPID(pid)
					if !valid {
						b.Fatal("existing memory failed")
					}
				case "one_open_per_sample":
					h, openErr := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
					if openErr != nil {
						b.Fatal(openErr)
					}
					sample, err = readRetainedObservation(h)
					closeErr := windows.CloseHandle(h)
					if closeErr != nil {
						b.Fatal(closeErr)
					}
				case "retained_handle":
					sample, err = readRetainedObservation(retained)
				default:
					b.Fatal(fmt.Errorf("unknown mode %s", mode))
				}
				if err != nil || !sample.Created.Equal(expected) || sample.ResidentBytes == 0 {
					b.Fatalf("invalid sample: %+v err=%v", sample, err)
				}
			}
			b.ReportMetric(2, "fields/op")
		})
	}
}
