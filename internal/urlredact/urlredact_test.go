package urlredact

import "testing"

func TestMarketplaceURLForErrorRedactsSchemeRelativeUserinfo(t *testing.T) {
	got := MarketplaceURLForError("//user:pass@host.example/mcp")
	want := "//host.example/mcp"
	if got != want {
		t.Fatalf("MarketplaceURLForError scheme-relative URL = %q, want %q", got, want)
	}
}
