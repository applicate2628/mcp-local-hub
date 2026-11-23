package runtimeidentity

import (
	"slices"
	"sort"
	"testing"
)

func TestWindowsProcessOwnerBorrowProfileIsExactAndAdditive(t *testing.T) {
	legacy := WindowsProcessOwnerExportsV1()
	members := WindowsProcessOwnerMemberExportsV1()
	borrow := WindowsProcessOwnerBorrowExportsV1()
	want := append(slices.Clone(members), "mh_borrow_process1")
	sort.Strings(want)
	if len(legacy) != 13 || len(members) != 17 || len(borrow) != 18 || !slices.Equal(borrow, want) {
		t.Fatalf("profile extension changed compatibility: legacy=%v members=%v borrow=%v", legacy, members, borrow)
	}
	for _, tc := range []struct {
		name           string
		exports        []string
		member, borrow bool
	}{
		{"legacy13", legacy, false, false},
		{"member17", members, true, false},
		{"borrow18", borrow, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !IsWindowsProcessOwnerExportProfileV1(tc.exports) ||
				IsWindowsProcessOwnerMemberExportProfileV1(tc.exports) != tc.member ||
				IsWindowsProcessOwnerBorrowExportProfileV1(tc.exports) != tc.borrow {
				t.Fatalf("capabilities do not match complete admitted profile: %v", tc.exports)
			}
		})
	}
	borrow[0] = "caller mutation"
	if !slices.Equal(WindowsProcessOwnerBorrowExportsV1(), want) {
		t.Fatal("caller changed canonical borrow profile")
	}
}

func TestWindowsProcessOwnerBorrowProfileRejectsPartialMixedAndUnknownSets(t *testing.T) {
	borrow := WindowsProcessOwnerBorrowExportsV1()
	mixed := append(WindowsProcessOwnerExportsV1(), "mh_borrow_process1")
	sort.Strings(mixed)
	cases := map[string][]string{
		"nil":                   nil,
		"partial":               slices.Clone(borrow[:len(borrow)-1]),
		"mixed-without-members": mixed,
		"extra":                 append(slices.Clone(borrow), "mh_unknown1"),
		"duplicate":             slices.Clone(borrow),
		"reordered":             slices.Clone(borrow),
		"unknown":               slices.Clone(borrow),
	}
	cases["duplicate"][1] = cases["duplicate"][0]
	cases["reordered"][0], cases["reordered"][1] = cases["reordered"][1], cases["reordered"][0]
	cases["unknown"][3] = "mh_future_process1"
	sort.Strings(cases["unknown"])
	for name, exports := range cases {
		t.Run(name, func(t *testing.T) {
			before := slices.Clone(exports)
			if IsWindowsProcessOwnerExportProfileV1(exports) ||
				IsWindowsProcessOwnerMemberExportProfileV1(exports) ||
				IsWindowsProcessOwnerBorrowExportProfileV1(exports) {
				t.Fatalf("accepted unrecognized capability set: %v", exports)
			}
			if !slices.Equal(exports, before) {
				t.Fatal("profile predicates normalized untrusted input")
			}
		})
	}
}
