package cbuild

import "testing"

func TestParseDiagnostics(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []Diagnostic
	}{
		{
			name: "msvc error with code and col",
			raw:  `C:\proj\src\main.cpp(12,5): error C2065: 'foo': undeclared identifier`,
			want: []Diagnostic{{
				File: `C:\proj\src\main.cpp`, Line: 12, Col: 5,
				Severity: "error", Code: "C2065", Message: "'foo': undeclared identifier",
			}},
		},
		{
			name: "msvc warning without col",
			raw:  `C:\proj\a.cpp(7): warning C4101: 'x': unreferenced local variable`,
			want: []Diagnostic{{
				File: `C:\proj\a.cpp`, Line: 7,
				Severity: "warning", Code: "C4101", Message: "'x': unreferenced local variable",
			}},
		},
		{
			name: "msvc fatal error normalizes to error",
			raw:  `main.cpp(1): fatal error C1083: Cannot open include file: 'missing.h'`,
			want: []Diagnostic{{
				File: "main.cpp", Line: 1,
				Severity: "error", Code: "C1083", Message: "Cannot open include file: 'missing.h'",
			}},
		},
		{
			name: "gcc clang error with col",
			raw:  `src/main.cpp:12:5: error: use of undeclared identifier 'foo'`,
			want: []Diagnostic{{
				File: "src/main.cpp", Line: 12, Col: 5,
				Severity: "error", Message: "use of undeclared identifier 'foo'",
			}},
		},
		{
			name: "gcc clang warning captures -W flag as code",
			raw:  `src/a.cpp:3:9: warning: unused variable 'x' [-Wunused-variable]`,
			want: []Diagnostic{{
				File: "src/a.cpp", Line: 3, Col: 9,
				Severity: "warning", Code: "-Wunused-variable",
				Message: "unused variable 'x' [-Wunused-variable]",
			}},
		},
		{
			name: "gcc clang note without col",
			raw:  `include/h.hpp:40: note: candidate function not viable`,
			want: []Diagnostic{{
				File: "include/h.hpp", Line: 40,
				Severity: "note", Message: "candidate function not viable",
			}},
		},
		{
			name: "cmake error with location",
			raw:  "CMake Error at CMakeLists.txt:10 (add_subdirectory):\n  The source directory does not exist.\n",
			want: []Diagnostic{{
				File: "CMakeLists.txt", Line: 10, Code: "add_subdirectory",
				Severity: "error", Message: "The source directory does not exist.",
			}},
		},
		{
			name: "cmake warning without location",
			raw:  "CMake Warning:\n  Manually-specified variables were not used by the project.\n",
			want: []Diagnostic{{
				Severity: "warning", Message: "Manually-specified variables were not used by the project.",
			}},
		},
		{
			name: "msvc linker LNK error",
			raw:  `main.obj : error LNK2019: unresolved external symbol "int __cdecl bar(void)"`,
			want: []Diagnostic{{
				File: "main.obj", Severity: "error", Code: "LNK2019",
				Message: `unresolved external symbol "int __cdecl bar(void)"`,
			}},
		},
		{
			name: "gnu ld undefined reference best effort",
			raw:  `/usr/bin/ld: main.o: undefined reference to ` + "`bar()'",
			want: []Diagnostic{{
				Severity: "error",
				Message:  "/usr/bin/ld: main.o: undefined reference to `bar()'",
			}},
		},
		{
			name: "unrecognized lines are dropped",
			raw:  "Building CXX object CMakeFiles/hello.dir/main.cpp.o\n[100%] Linked target hello\n",
			want: []Diagnostic{},
		},
		{
			name: "order preserved across mixed formats",
			raw: "a.cpp:1:1: warning: first [-Wall]\n" +
				`b.cpp(2,3): error C2143: second` + "\n" +
				"CMake Error at c.txt:4 (foo):\n  third\n",
			want: []Diagnostic{
				{File: "a.cpp", Line: 1, Col: 1, Severity: "warning", Code: "-Wall", Message: "first [-Wall]"},
				{File: "b.cpp", Line: 2, Col: 3, Severity: "error", Code: "C2143", Message: "second"},
				{File: "c.txt", Line: 4, Code: "foo", Severity: "error", Message: "third"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseDiagnostics(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d diagnostics, want %d\ngot:  %+v\nwant: %+v", len(got), len(tc.want), got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("diag[%d]\n got: %+v\nwant: %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}
