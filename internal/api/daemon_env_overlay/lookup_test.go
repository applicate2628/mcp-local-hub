package daemon_env_overlay

import "testing"

func TestLookupOverlayReturnsRowEnv(t *testing.T) {
	ov := &Overlay{
		Version: 1,
		Daemons: map[string]DaemonRow{
			`\mcp-local-hub-clangd-default`: {
				Env: map[string]string{"Path": "C:/clang/bin;${parent_path}"},
			},
		},
	}
	got := LookupOverlay(ov, `\mcp-local-hub-clangd-default`)
	if got == nil {
		t.Fatal("expected map; got nil")
	}
	if got["Path"] != "C:/clang/bin;${parent_path}" {
		t.Fatalf("Path = %q; want C:/clang/bin;${parent_path}", got["Path"])
	}
}

func TestLookupOverlayNormalizesBareKey(t *testing.T) {
	ov := &Overlay{
		Daemons: map[string]DaemonRow{
			`\mcp-local-hub-clangd-default`: {
				Env: map[string]string{"Path": "v"},
			},
		},
	}
	// Pass BARE form (no leading backslash). Helper must normalize and find it.
	got := LookupOverlay(ov, `mcp-local-hub-clangd-default`)
	if got == nil || got["Path"] != "v" {
		t.Fatalf("bare-form lookup failed; got %v", got)
	}
}

func TestLookupOverlayReturnsNilOnMiss(t *testing.T) {
	ov := &Overlay{
		Daemons: map[string]DaemonRow{
			`\mcp-local-hub-foo-default`: {Env: map[string]string{"K": "v"}},
		},
	}
	if got := LookupOverlay(ov, `\mcp-local-hub-bar-default`); got != nil {
		t.Fatalf("expected nil on miss; got %v", got)
	}
}

func TestLookupOverlayHandlesNilAndEmptyOverlay(t *testing.T) {
	if got := LookupOverlay(nil, `\anything`); got != nil {
		t.Fatalf("nil overlay should yield nil; got %v", got)
	}
	if got := LookupOverlay(&Overlay{}, `\anything`); got != nil {
		t.Fatalf("empty overlay should yield nil; got %v", got)
	}
}

func TestLookupOverlayReturnsDefensiveCopy(t *testing.T) {
	ov := &Overlay{
		Daemons: map[string]DaemonRow{
			`\mcp-local-hub-foo-default`: {Env: map[string]string{"Path": "original"}},
		},
	}
	first := LookupOverlay(ov, `\mcp-local-hub-foo-default`)
	first["Path"] = "mutated"
	second := LookupOverlay(ov, `\mcp-local-hub-foo-default`)
	if second["Path"] != "original" {
		t.Fatalf("LookupOverlay must return a defensive copy; second call returned %q", second["Path"])
	}
}
