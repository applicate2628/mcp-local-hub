//go:build windows

package scheduler

import (
	"strings"
	"testing"
)

func TestWindowsTaskSpecMatchesXMLUsesCurrentUserAndExistingWriter(t *testing.T) {
	owner, err := currentWindowsTaskOwner()
	if err != nil {
		t.Fatal(err)
	}
	spec := TaskSpec{Name: `\fixture`, Command: `D:\hub\mcphub.exe`, Args: []string{"gui", "--no-browser"}, LogonTrigger: true}
	canonical := buildCreateXML(spec, owner)
	for _, tc := range []struct {
		name string
		xml  string
		want bool
	}{
		{name: "writer_output", xml: canonical, want: true},
		{name: "native_metadata", xml: strings.Replace(canonical, "<RegistrationInfo>", `<RegistrationInfo><URI>\fixture</URI>`, 1), want: true},
		{name: "different_user", xml: buildCreateXML(spec, "S-1-5-21-fixture-other"), want: false},
		{name: "disabled", xml: strings.Replace(canonical, "<Enabled>true</Enabled>", "<Enabled>false</Enabled>", -1), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			match, err := WindowsTaskSpecMatchesXML([]byte(tc.xml), spec)
			if err != nil || match != tc.want {
				t.Fatalf("match=%v err=%v, want %v", match, err, tc.want)
			}
		})
	}
}
