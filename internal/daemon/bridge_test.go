package daemon

import "testing"

func TestShellQuotePreventsShellExpansion(t *testing.T) {
	got := shellQuote(`echo ok;touch${IFS}/tmp/pwned`)
	want := `'echo ok;touch${IFS}/tmp/pwned'`
	if got != want {
		t.Fatalf("shellQuote() = %q, want %q", got, want)
	}
}

func TestShellQuoteEscapesSingleQuote(t *testing.T) {
	got := shellQuote(`a'b`)
	want := `'a'"'"'b'`
	if got != want {
		t.Fatalf("shellQuote() = %q, want %q", got, want)
	}
}

func TestBuildBridgeSpecUsesQuotedTokens(t *testing.T) {
	spec := BuildBridgeSpec("cmd", []string{"arg 1", "x;y"}, 7777, map[string]string{"A": "B"}, "/tmp/log")

	if spec.Command != "npx" {
		t.Fatalf("Command = %q, want npx", spec.Command)
	}
	if got, want := spec.Args[3], `'cmd' 'arg 1' 'x;y'`; got != want {
		t.Fatalf("--stdio arg = %q, want %q", got, want)
	}
}
