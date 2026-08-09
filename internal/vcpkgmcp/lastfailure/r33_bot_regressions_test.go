package lastfailure

import "testing"

func TestR33MSVCDiagnosticAllowsParenthesesBeforeFinalLocation(t *testing.T) {
	cases := []struct {
		line     string
		wantFile string
		wantLine int
	}{
		{
			line:     `C:\Program Files (x86)\SDK\header.h(10): error C2065: undeclared identifier`,
			wantFile: `C:\Program Files (x86)\SDK\header.h`,
			wantLine: 10,
		},
		{
			line:     `C:\src\part(7)\file.cpp(42,9): warning C4996: deprecated`,
			wantFile: `C:\src\part(7)\file.cpp`,
			wantLine: 42,
		},
	}
	for _, tc := range cases {
		diagnostics := ScanDiagnostics([]byte(tc.line + "\n"))
		if len(diagnostics) != 1 {
			t.Fatalf("ScanDiagnostics(%q)=%+v, want one MSVC diagnostic", tc.line, diagnostics)
		}
		if diagnostics[0].File != tc.wantFile || diagnostics[0].Line != tc.wantLine {
			t.Fatalf("ScanDiagnostics(%q)[0]=%+v, want file=%q line=%d", tc.line, diagnostics[0], tc.wantFile, tc.wantLine)
		}
	}
}
