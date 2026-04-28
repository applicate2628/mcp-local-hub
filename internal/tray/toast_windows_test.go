// internal/tray/toast_windows_test.go
//go:build windows

package tray

import "testing"

func TestPsEscape(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"plain", "plain"},
		{"with 'single' quotes", "with ''single'' quotes"},
		{"newline\nhere", "newline here"},
		{"cr\rhere", "cr here"},
		{"crlf\r\nhere", "crlf  here"},
		{"", ""},
		{"all'three\nat\ronce", "all''three at once"},
	}
	for _, tc := range cases {
		got := psEscape(tc.in)
		if got != tc.want {
			t.Errorf("psEscape(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
