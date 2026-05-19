package api

import "testing"

// langsForTest is the canonical 9-language slice used by the matrix
// LSP recognition algorithm (Task 3.2 / 3.3). Kept local to the test
// file so changes to the manifest defaults do not silently invalidate
// the parser contract.
var langsForTest = []string{
	"clangd",
	"fortran",
	"go",
	"javascript",
	"python",
	"rust",
	"typescript",
	"vscode-css",
	"vscode-html",
}

func TestParseEntryName(t *testing.T) {
	cases := []struct {
		name       string
		entry      string
		wantLang   string
		wantSuffix string
	}{
		{
			name:       "plain base",
			entry:      "mcp-language-server-clangd",
			wantLang:   "clangd",
			wantSuffix: "",
		},
		{
			name:       "short suffix",
			entry:      "mcp-language-server-rust-a1b2",
			wantLang:   "rust",
			wantSuffix: "a1b2",
		},
		{
			name:       "full suffix",
			entry:      "mcp-language-server-typescript-deadbeef",
			wantLang:   "typescript",
			wantSuffix: "deadbeef",
		},
		{
			name:       "non-LSP entry",
			entry:      "some-other-server",
			wantLang:   "",
			wantSuffix: "",
		},
		{
			name:       "hyphenated language exact",
			entry:      "mcp-language-server-vscode-html",
			wantLang:   "vscode-html",
			wantSuffix: "",
		},
		{
			name:       "hyphenated language with suffix",
			entry:      "mcp-language-server-vscode-css-abcd",
			wantLang:   "vscode-css",
			wantSuffix: "abcd",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			gotLang, gotSuffix := ParseEntryName(tc.entry, langsForTest)
			if gotLang != tc.wantLang || gotSuffix != tc.wantSuffix {
				t.Fatalf("ParseEntryName(%q) = (%q, %q); want (%q, %q)",
					tc.entry, gotLang, gotSuffix, tc.wantLang, tc.wantSuffix)
			}
		})
	}
}
