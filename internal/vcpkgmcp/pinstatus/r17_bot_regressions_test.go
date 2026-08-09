package pinstatus

import "testing"

func TestR17PortfileRejectsDuplicateElseAndElseifAfterElse(t *testing.T) {
	for name, source := range map[string]string{
		"duplicate_else":    "if(ON)\nelse()\nelse()\nendif()\n",
		"elseif_after_else": "if(OFF)\nelse()\nelseif(ON)\nendif()\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := parsePortfile(source); ok {
				t.Fatalf("parsePortfile accepted structurally invalid branch chain: %q", source)
			}
		})
	}
}

func TestR17PortfileFailsClosedOnIndirectExecutableSourcePaths(t *testing.T) {
	for name, source := range map[string]string{
		"include":             "include(source-helper.cmake)\nvcpkg_from_github(REPO owner/repo REF main SHA512 abc)\n",
		"transitive_function": "function(fetch_impl)\nvcpkg_from_github(REPO owner/repo REF main SHA512 abc)\nendfunction()\nfunction(select_source)\nfetch_impl()\nendfunction()\nselect_source()\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := parsePortfile(source); ok {
				t.Fatalf("parsePortfile accepted executable source path it cannot model: %q", source)
			}
		})
	}
}

func TestR17InactiveIncludeDoesNotPoisonKnownSourceSelection(t *testing.T) {
	source := "if(OFF)\ninclude(source-helper.cmake)\nendif()\nvcpkg_from_github(REPO owner/repo REF main SHA512 abc)\n"
	if _, ok := parsePortfile(source); !ok {
		t.Fatal("inactive include must not make a known active source selection unresolvable")
	}
}
