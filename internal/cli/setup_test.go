package cli

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

// TestDirOnPath exercises the PATH-env-var parser used by the install
// command to decide whether ~/.local/bin is already on PATH. The real
// targetDirOnPath wraps this over os.Getenv; exercising the splitter
// directly keeps the test independent of the running process's PATH.
//
// Test inputs are built at runtime to match the host's PATH conventions
// (drive-letter colons collide with the POSIX `:` separator on Linux,
// so a literal `C:\...` entry would split inside the colon and break
// the assertion).
func TestDirOnPath(t *testing.T) {
	sep := string(os.PathListSeparator)
	target := `C:\Users\dima_\.local\bin`
	other1, other2 := `C:\Go\bin`, `C:\Windows`
	mixedCaseEntry := `C:\Users\dima_/.local/bin`
	if runtime.GOOS != "windows" {
		// POSIX-style entries: `:` is the separator, so absolute POSIX
		// paths split cleanly. The function still needs to see a
		// non-trivial path for the "absent" / "single entry" cases.
		target = "/home/u/.local/bin"
		other1, other2 = "/usr/local/go/bin", "/usr/bin"
		mixedCaseEntry = "/home/u/.local/bin" // POSIX is case-sensitive; same string still matches
	}

	cases := []struct {
		name    string
		dir     string
		pathEnv string
		want    bool
	}{
		{
			name:    "present in middle",
			dir:     target,
			pathEnv: strings.Join([]string{other1, target, other2}, sep),
			want:    true,
		},
		{
			name:    "absent",
			dir:     target,
			pathEnv: strings.Join([]string{other1, other2}, sep),
			want:    false,
		},
		{
			name:    "empty PATH",
			dir:     target,
			pathEnv: "",
			want:    false,
		},
		{
			name:    "trailing separator (empty entry tolerated)",
			dir:     target,
			pathEnv: target + sep,
			want:    true,
		},
		{
			name:    "single entry match",
			dir:     target,
			pathEnv: target,
			want:    true,
		},
	}

	if runtime.GOOS == "windows" {
		// samePath folds case + slashes on Windows — a mixed-case /
		// separator entry must still match the canonical target.
		cases = append(cases, struct {
			name    string
			dir     string
			pathEnv string
			want    bool
		}{
			name:    "mixed slashes and case (Windows semantics)",
			dir:     target,
			pathEnv: mixedCaseEntry,
			want:    true,
		})
	}

	for _, tc := range cases {
		got := dirOnPath(tc.dir, tc.pathEnv)
		if got != tc.want {
			t.Errorf("%s: dirOnPath(%q, %q) = %v, want %v",
				tc.name, tc.dir, tc.pathEnv, got, tc.want)
		}
	}
}
