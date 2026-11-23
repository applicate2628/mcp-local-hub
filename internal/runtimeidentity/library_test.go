package runtimeidentity

import (
	"slices"
	"sort"
	"testing"
)

func TestWindowsProcessOwnerExportProfilesV1(t *testing.T) {
	legacy := []string{
		"mh_admit1", "mh_admit_checked1", "mh_admit_identity1", "mh_close1",
		"mh_create1", "mh_create_env1", "mh_create_io1", "mh_finish1",
		"mh_query_identity1", "mh_release1", "mh_stop1", "mh_version1", "mh_wait1",
	}
	members := append(slices.Clone(legacy),
		"mh_members_close1", "mh_members_observe1", "mh_members_revalidate1", "mh_members_snapshot1")
	sort.Strings(members)
	for _, tc := range []struct {
		name string
		get  func() []string
		want []string
	}{
		{"owner-v1", WindowsProcessOwnerExportsV1, legacy},
		{"owner-v1-members-v1", WindowsProcessOwnerMemberExportsV1, members},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.get()
			if !slices.Equal(got, tc.want) || !IsWindowsProcessOwnerExportProfileV1(got) {
				t.Fatalf("export profile=%v, want %v", got, tc.want)
			}
			got[0] = "caller-mutation"
			if next := tc.get(); !slices.Equal(next, tc.want) || !IsWindowsProcessOwnerExportProfileV1(next) {
				t.Fatal("caller changed the canonical export profile")
			}
		})
	}
}

func TestWindowsProcessOwnerExportProfilesRejectUnrecognizedSets(t *testing.T) {
	legacy := WindowsProcessOwnerExportsV1()
	members := WindowsProcessOwnerMemberExportsV1()
	cases := map[string][]string{
		"nil":           nil,
		"empty":         {},
		"legacy subset": slices.Clone(legacy[:len(legacy)-1]),
		"member subset": slices.Clone(members[:len(members)-1]),
		"duplicate":     slices.Clone(members),
		"unknown":       slices.Clone(members),
		"reordered":     slices.Clone(members),
		"extra":         append(slices.Clone(members), "mh_unknown1"),
	}
	cases["duplicate"][1] = cases["duplicate"][0]
	cases["unknown"][1] = "mh_unknown1"
	sort.Strings(cases["unknown"])
	cases["reordered"][0], cases["reordered"][1] = cases["reordered"][1], cases["reordered"][0]
	extra := []string{"mh_members_close1", "mh_members_observe1", "mh_members_revalidate1"}
	for i := range extra {
		partial := append(slices.Clone(legacy), extra[:i+1]...)
		sort.Strings(partial)
		cases[extra[i]] = partial
	}
	for name, exports := range cases {
		t.Run(name, func(t *testing.T) {
			before := slices.Clone(exports)
			if IsWindowsProcessOwnerExportProfileV1(exports) {
				t.Fatalf("accepted unrecognized export profile: %v", exports)
			}
			if !slices.Equal(exports, before) {
				t.Fatal("profile validation mutated caller input")
			}
		})
	}
}
