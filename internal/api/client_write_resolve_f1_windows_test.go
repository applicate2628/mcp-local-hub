//go:build windows

package api

import (
	"path/filepath"
	"strings"
	"testing"
)

// AF-1 F1 — Windows volume-root decomposition unit test.
//
// decomposeResolvedParentWindows splits a cleaned absolute Windows path into
// its volume name (drive "C:" or UNC share "\\server\share") and the list of
// INTERMEDIATE directory components between the volume root and the final
// base name (the base is dropped). The component-by-component descent in
// secureWriteThroughResolvedParentHandle anchors at vol+"\" and opens each of
// these components O_NOFOLLOW. No symlink elevation is needed to verify the
// decomposition itself.
func TestF1_DecomposeResolvedParentWindows(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantVol  string
		wantDirs []string
		wantErr  bool
	}{
		{
			name:     "drive-root with intermediates",
			in:       `C:\Users\u\.codex\config.toml`,
			wantVol:  `C:`,
			wantDirs: []string{"Users", "u", ".codex"},
		},
		{
			name:     "drive-root single intermediate",
			in:       `E:\env\config.toml`,
			wantVol:  `E:`,
			wantDirs: []string{"env"},
		},
		{
			name:     "drive-root file directly under root",
			in:       `D:\config.toml`,
			wantVol:  `D:`,
			wantDirs: []string{},
		},
		{
			name:     "UNC share root with intermediates",
			in:       `\\server\share\dir\sub\config.toml`,
			wantVol:  `\\server\share`,
			wantDirs: []string{"dir", "sub"},
		},
		{
			name:     "UNC share file directly under share root",
			in:       `\\server\share\config.toml`,
			wantVol:  `\\server\share`,
			wantDirs: []string{},
		},
		{
			name:    "no volume (relative) is rejected",
			in:      `relative\path\config.toml`,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cleaned := filepath.Clean(tc.in)
			vol, dirs, err := decomposeResolvedParentWindows(cleaned)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got vol=%q dirs=%v", tc.in, vol, dirs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.in, err)
			}
			if vol != tc.wantVol {
				t.Errorf("vol = %q, want %q", vol, tc.wantVol)
			}
			if !equalStringSlices(dirs, tc.wantDirs) {
				t.Errorf("dirComponents = %v, want %v", dirs, tc.wantDirs)
			}
			// The volume root anchor the descent opens is vol+`\`.
			anchor := vol + `\`
			if !strings.HasSuffix(anchor, `\`) {
				t.Errorf("anchor %q does not end in a separator", anchor)
			}
		})
	}
}

// equalStringSlices treats a nil and an empty non-nil slice as equal (both
// mean "no intermediate components").
func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
