package pinstatus

import "testing"

func TestR22PortfileFailsClosedOnFetchInsideExecutableLoop(t *testing.T) {
	for name, source := range map[string]string{
		"foreach": "foreach(item IN ITEMS one)\nvcpkg_from_github(REPO owner/repo REF main)\nendforeach()\n",
		"while":   "while(ON)\nvcpkg_from_git(URL https://example.invalid/repo REF main)\nendwhile()\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := parsePortfile(source); ok {
				t.Fatalf("parsePortfile accepted executable loop source selection: %q", source)
			}
		})
	}
}
