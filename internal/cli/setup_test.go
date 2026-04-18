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
func TestDirOnPath(t *testing.T) {
	sep := string(os.PathListSeparator)
	cases := []struct {
		name    string
		dir     string
		pathEnv string
		want    bool
	}{
		{
			name:    "present in middle",
			dir:     `C:\Users\dima_\.local\bin`,
			pathEnv: strings.Join([]string{`C:\Go\bin`, `C:\Users\dima_\.local\bin`, `C:\Windows`}, sep),
			want:    true,
		},
		{
			name:    "absent",
			dir:     `C:\Users\dima_\.local\bin`,
			pathEnv: strings.Join([]string{`C:\Go\bin`, `C:\Windows`}, sep),
			want:    false,
		},
		{
			name:    "empty PATH",
			dir:     `C:\Users\dima_\.local\bin`,
			pathEnv: "",
			want:    false,
		},
		{
			name:    "trailing separator (empty entry tolerated)",
			dir:     `C:\Users\dima_\.local\bin`,
			pathEnv: `C:\Users\dima_\.local\bin` + sep,
			want:    true,
		},
		{
			name:    "single entry match",
			dir:     `C:\Users\dima_\.local\bin`,
			pathEnv: `C:\Users\dima_\.local\bin`,
			want:    true,
		},
	}

	if runtime.GOOS == "windows" {
		// samePath folds case on Windows — a mixed-case/separator entry
		// must still match the canonical target.
		cases = append(cases, struct {
			name    string
			dir     string
			pathEnv string
			want    bool
		}{
			name:    "mixed slashes and case (Windows semantics)",
			dir:     `C:\Users\dima_\.local\bin`,
			pathEnv: `C:\Users\dima_/.local/bin`,
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
