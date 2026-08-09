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

func TestR22InactiveLoopDoesNotPoisonKnownSourceSelection(t *testing.T) {
	source := "if(OFF)\nforeach(item IN ITEMS one)\nvcpkg_from_github(REPO ignored/repo REF ignored)\nendforeach()\nendif()\nvcpkg_from_github(REPO owner/repo REF main)\n"
	parsed, ok := parsePortfile(source)
	if !ok || parsed.Remote.Repo != "owner/repo" {
		t.Fatalf("inactive loop poisoned active source selection: ok=%v parsed=%+v", ok, parsed)
	}
}
