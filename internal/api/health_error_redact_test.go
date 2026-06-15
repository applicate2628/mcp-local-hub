package api

import (
	"strings"
	"testing"
)

// TestRedactErrorDetail verifies the g3 capability/health error-detail
// redactor scrubs filesystem paths and token-like runs that the GUI would
// otherwise render verbatim in its capability banners (info-leak via
// screenshots / browser dev-tools). The operational category portion of an
// error message (method name, HTTP status, "timeout", "parse") is benign and
// must survive so operators still get an actionable diagnostic.
//
// Bug: work-items/bugs/2026-05-08-g3-error-text-redaction.md.
func TestRedactErrorDetail(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantNot   []string // substrings that MUST be scrubbed
		wantHave  []string // category substrings that MUST survive
		wantExact string   // when non-empty, the full expected output
	}{
		{
			name:     "windows user path in daemon error message",
			in:       `tools/list: stat C:\Users\alice\secret\token.json: no such file`,
			wantNot:  []string{`C:\Users\alice`, `secret`, `token.json`},
			wantHave: []string{"tools/list", "<redacted-path>"},
		},
		{
			name:     "posix home path in error",
			in:       `initialize: open /home/alice/.config/creds/api.key: permission denied`,
			wantNot:  []string{"/home/alice/.config/creds/api.key"},
			wantHave: []string{"initialize", "<redacted-path>", "permission denied"},
		},
		{
			name:     "token-like long alnum run is scrubbed",
			in:       `auth failed: token abcdefABCDEF0123456789abcdefABCDEF0123456789 rejected`,
			wantNot:  []string{"abcdefABCDEF0123456789abcdefABCDEF0123456789"},
			wantHave: []string{"auth failed", "rejected"},
		},
		{
			name:      "plain category-only message is unchanged",
			in:        "initialize: HTTP 500",
			wantExact: "initialize: HTTP 500",
		},
		{
			name:      "timeout category unchanged",
			in:        "tools/list: timeout",
			wantExact: "tools/list: timeout",
		},
		{
			name:      "parse category unchanged",
			in:        "tools/list: parse: unexpected EOF",
			wantExact: "tools/list: parse: unexpected EOF",
		},
		{
			name:      "no-port sentinel unchanged",
			in:        "no port for daemon",
			wantExact: "no port for daemon",
		},
		{
			name:      "empty stays empty",
			in:        "",
			wantExact: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactErrorDetail(tc.in)
			if tc.wantExact != "" || tc.in == "" {
				if got != tc.wantExact {
					t.Fatalf("redactErrorDetail(%q) = %q, want exactly %q", tc.in, got, tc.wantExact)
				}
				return
			}
			for _, frag := range tc.wantNot {
				if strings.Contains(got, frag) {
					t.Errorf("redactErrorDetail(%q) = %q — must NOT contain leaked fragment %q", tc.in, got, frag)
				}
			}
			for _, frag := range tc.wantHave {
				if !strings.Contains(got, frag) {
					t.Errorf("redactErrorDetail(%q) = %q — expected to retain %q", tc.in, got, frag)
				}
			}
		})
	}
}
