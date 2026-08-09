package patchesapply

import "testing"

func TestR30CommandNameCannotCrossLineEnding(t *testing.T) {
	for _, source := range []string{
		"vcpkg_from_github\n(PATCHES fix.patch)\n",
		"set\r\n(VCPKG_LIBRARY_LINKAGE static)\n",
	} {
		statements, ok := splitStatementsChecked(source)
		if !ok {
			t.Fatalf("splitStatementsChecked(%q) reported structural truncation", source)
		}
		if len(statements) != 0 {
			t.Fatalf("splitStatementsChecked(%q)=%+v, want no executable command across a line ending", source, statements)
		}
	}
}
