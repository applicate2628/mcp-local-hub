package urlredact

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestMarketplaceURLForErrorRedactsSchemeRelativeUserinfo(t *testing.T) {
	got := MarketplaceURLForError("//user:pass@host.example/mcp")
	want := "//host.example/mcp"
	if got != want {
		t.Fatalf("MarketplaceURLForError scheme-relative URL = %q, want %q", got, want)
	}
}

func TestScrubParseErrorRedactsCredentialsInURLParseError(t *testing.T) {
	for _, raw := range []string{
		"https://user:pass@example.com:abc/mcp", // invalid port -> url.Parse fails
		"https://user:token@example.com/%zz",    // bad escape -> url.Parse fails
	} {
		_, parseErr := url.Parse(raw)
		if parseErr == nil {
			t.Fatalf("test precondition: url.Parse(%q) should fail", raw)
		}
		scrubbed := ScrubParseError(parseErr)
		msg := scrubbed.Error()
		for _, leaked := range []string{"user:pass", "user:token", "user:pass@", "user:token@"} {
			if strings.Contains(msg, leaked) {
				t.Fatalf("ScrubParseError leaked credential %q in %q", leaked, msg)
			}
		}
		// errors.Is/As must still traverse to the original *url.Error.
		var ue *url.Error
		if !errors.As(scrubbed, &ue) {
			t.Fatalf("ScrubParseError broke the *url.Error unwrap chain for %q", raw)
		}
	}
}

func TestScrubParseErrorPassesThroughNonURLErrors(t *testing.T) {
	plain := errors.New("not a url error")
	if got := ScrubParseError(plain); got != plain {
		t.Fatalf("ScrubParseError mutated a non-url.Error: got %v", got)
	}
	if ScrubParseError(nil) != nil {
		t.Fatal("ScrubParseError(nil) should be nil")
	}
}
