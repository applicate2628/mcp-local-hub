//go:build windows

package binaryadmission

import (
	"errors"
	"reflect"
	"testing"

	"golang.org/x/sys/windows"
)

func TestObservedInstalledProductRetainsUndefinedOpenAndOriginalCloses(t *testing.T) {
	openErr := errors.New("fixture original NT open failed")
	closeErr := errors.New("fixture first prior close failed")
	original := &InstalledCanonicalEntryAcquisitionObservation{}
	owner := &WindowsCanonicalEntryDispatchSetV2{acquisition: original}
	original.owner = owner
	var opens int
	var closes []windows.Handle
	native := observeCanonicalEntryOpen(t.Context(), &original.opens,
		func(windows.Handle, string, bool, uint32, uint32, uint32) (windows.Handle, error) {
			opens++
			switch opens {
			case 1:
				return 7, nil
			case 2:
				return 9, nil
			default:
				return 8, openErr
			}
		}, func(handle windows.Handle) error {
			closes = append(closes, handle)
			if handle == 9 {
				return closeErr
			}
			return nil
		})
	if _, err := native(0, "root", true, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := native(7, "parent", true, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	_, original.err = native(9, "product", false, 0, 0, 0)
	original.returned = true
	if original.ResourcesFinalized() {
		t.Fatal("prior Product handles were credited before original close")
	}
	if err := owner.Close(); !errors.Is(err, closeErr) || !reflect.DeepEqual(closes, []windows.Handle{9, 7}) {
		t.Fatalf("original closes were lost or undefined handle was closed: %v, %v", err, closes)
	}
	slots := original.opens.slots
	if len(slots) != 3 || !slots[0].closeReturned || !slots[1].closeReturned || !errors.Is(slots[1].closeErr, closeErr) ||
		slots[2].native != 8 || !errors.Is(slots[2].openErr, openErr) || slots[2].closeAttempted ||
		!errors.Is(original.err, openErr) || original.ResourcesFinalized() {
		t.Fatal("original partial native evidence was erased or incorrectly settled")
	}
	if err := owner.Close(); !errors.Is(err, closeErr) || len(closes) != 2 {
		t.Fatal("original Product close was replaced or dispatched twice")
	}
}
