//go:build windows && amd64

// Tests for splitCommandLineW + matchBasename. Tagged windows&&amd64
// to match probe_windows.go's tag — the implementation it tests is
// amd64-only because of the embedded PEB struct offsets.

package gui

import (
	"reflect"
	"testing"
)

// TestSplitCommandLineW_TabSeparatorBetweenExeAndArgs locks in the
// Codex iter-4 P2 #1 fix: tabs must be honored as argv separators in
// the first-token loop, not just in the remaining-args loop. Pre-fix,
// `mcphub.exe<TAB>daemon` returned a single argv element and the
// no-arg GUI branch of cmdlineIsGui passed for a non-GUI subcommand.
func TestSplitCommandLineW_TabSeparatorBetweenExeAndArgs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "tab between exe and subcommand",
			in:   "mcphub.exe\tdaemon",
			want: []string{"mcphub.exe", "daemon"},
		},
		{
			name: "tab between exe and gui subcommand",
			in:   "mcphub.exe\tgui",
			want: []string{"mcphub.exe", "gui"},
		},
		{
			name: "space still works",
			in:   "mcphub.exe daemon",
			want: []string{"mcphub.exe", "daemon"},
		},
		{
			name: "no separator (Explorer no-arg launch)",
			in:   "mcphub.exe",
			want: []string{"mcphub.exe"},
		},
		{
			name: "quoted exe path with tab to next arg",
			in:   `"C:\Program Files\mcphub\mcphub.exe"` + "\tdaemon",
			want: []string{`C:\Program Files\mcphub\mcphub.exe`, "daemon"},
		},
		{
			name: "tab inside quoted argv[0] is preserved",
			in:   `"weird` + "\t" + `name.exe"` + "\tdaemon",
			want: []string{"weird\tname.exe", "daemon"},
		},
		// Codex bot review on PR #23 P3: empty quoted argv tokens
		// must be preserved (CommandLineToArgvW behavior). Without
		// this, len(argv)==1 misclassifies `mcphub.exe ""` as the
		// no-arg auto-gui case in cmdlineIsGui.
		{
			name: "empty quoted arg preserved",
			in:   `mcphub.exe ""`,
			want: []string{"mcphub.exe", ""},
		},
		{
			name: "two empty quoted args preserved",
			in:   `mcphub.exe "" ""`,
			want: []string{"mcphub.exe", "", ""},
		},
		{
			name: "empty quoted argv[0] preserved",
			in:   `"" daemon`,
			want: []string{"", "daemon"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitCommandLineW(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("splitCommandLineW(%q) = %#v; want %#v", tc.in, got, tc.want)
			}
		})
	}
}
