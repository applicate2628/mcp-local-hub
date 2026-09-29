package clients

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

const attachOriginDirectFixture = `{"mcp":{"codegraph":{"type":"local","command":["node","server.js"],"enabled":true}}}`
const attachOriginHTTPFixture = `{"mcp":{"codegraph":{"type":"remote","url":"http://127.0.0.1:9303/mcp","enabled":true}}}`

func TestAttachExisting_MiMoOriginTargetOwnedHTTPAndHigherSources(t *testing.T) {
	for _, tc := range []struct {
		name, higher string
		want         SelectedEntryOriginVerdict
	}{
		{name: "target-only", want: SelectedEntryOriginVerdict("target-owned-http")},
		{name: "higher-same-url", higher: attachOriginHTTPFixture, want: SelectedEntryOriginHigherOrMixed},
		{name: "higher-enabled-only", higher: `{"mcp":{"codegraph":{"enabled":true}}}`, want: SelectedEntryOriginHigherOrMixed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "mimocode.json")
			if err := os.WriteFile(target, []byte(attachOriginHTTPFixture), 0600); err != nil {
				t.Fatal(err)
			}
			if tc.higher != "" {
				if err := os.WriteFile(filepath.Join(dir, "mimocode.jsonc"), []byte(tc.higher), 0600); err != nil {
					t.Fatal(err)
				}
			}
			got := (&mimoCodeClient{path: target}).ReadSelectedEntryOrigin(context.Background(), "codegraph")
			if got.Verdict != tc.want {
				t.Fatalf("HTTP source verdict=%q want=%q", got.Verdict, tc.want)
			}
			if tc.higher == "" {
				if got.TargetPathInternal != target || got.TargetBytesSHA256 == "" || got.EntryTokenSHA256 == "" || got.EffectiveDirectEntryInternal != nil {
					t.Fatalf("target-owned HTTP physical proof missing or direct source retained: %+v", got.Redacted())
				}
			} else if got.TargetPathInternal != "" || got.TargetBytesSHA256 != "" || got.EntryTokenSHA256 != "" {
				t.Fatalf("higher HTTP source retained target proof: %+v", got.Redacted())
			}
		})
	}
}

func TestAttachExisting_MiMoOriginTargetOwnedDirect(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "mimocode.json")
	if err := os.WriteFile(target, []byte(attachOriginDirectFixture), 0600); err != nil {
		t.Fatal(err)
	}
	reader := &mimoCodeClient{path: target}
	got := reader.ReadSelectedEntryOrigin(context.Background(), "codegraph")
	if got.Verdict != SelectedEntryOriginTargetOwnedDirect || got.TargetPathInternal != target || got.TargetBytesSHA256 == "" || got.EntryTokenSHA256 == "" {
		t.Fatalf("target-owned direct proof missing: %+v", got.Redacted())
	}
	if got.EffectiveDirectEntryInternal == nil || got.EffectiveDirectEntryInternal.Raw == nil {
		t.Fatal("direct physical entry was not retained for internal recheck")
	}
}

func TestAttachExisting_MiMoOriginHigherEnabledOnlyIsMixed(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "mimocode.json")
	if err := os.WriteFile(target, []byte(attachOriginDirectFixture), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mimocode.jsonc"), []byte(`{"mcp":{"codegraph":{"enabled":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	got := (&mimoCodeClient{path: target}).ReadSelectedEntryOrigin(context.Background(), "codegraph")
	if got.Verdict != SelectedEntryOriginHigherOrMixed {
		t.Fatalf("enabled-only higher contribution admitted: %q", got.Verdict)
	}
}

func TestAttachExisting_MiMoOriginDistinctHigherHardlinkIsAmbiguous(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "mimocode.json")
	if err := os.WriteFile(target, []byte(attachOriginDirectFixture), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, filepath.Join(dir, "mimocode.jsonc")); err != nil {
		t.Skipf("hardlink unavailable in test-owned directory: %v", err)
	}
	got := (&mimoCodeClient{path: target}).ReadSelectedEntryOrigin(context.Background(), "codegraph")
	if got.Verdict != SelectedEntryOriginAliasOrAmbiguous {
		t.Fatalf("distinct higher hardlink admitted: %q", got.Verdict)
	}
}

func TestAttachExisting_MiMoOriginRefusesOtherSourceLayers(t *testing.T) {
	for _, tc := range []struct {
		name, target, lower, higher string
		want                        SelectedEntryOriginVerdict
	}{
		{name: "higher-jsonc", target: attachOriginDirectFixture, higher: attachOriginDirectFixture, want: SelectedEntryOriginHigherOrMixed},
		{name: "lower-only", target: `{}`, lower: attachOriginDirectFixture, want: SelectedEntryOriginBelowOrImport},
		{name: "missing", target: `{}`, want: SelectedEntryOriginMissing},
		{name: "malformed-target", target: `{`, want: SelectedEntryOriginMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "mimocode.json")
			if err := os.WriteFile(target, []byte(tc.target), 0600); err != nil {
				t.Fatal(err)
			}
			if tc.lower != "" {
				if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(tc.lower), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.higher != "" {
				if err := os.WriteFile(filepath.Join(dir, "mimocode.jsonc"), []byte(tc.higher), 0600); err != nil {
					t.Fatal(err)
				}
			}
			got := (&mimoCodeClient{path: target}).ReadSelectedEntryOrigin(context.Background(), "codegraph")
			if got.Verdict != tc.want || got.TargetPathInternal != "" || got.TargetBytesSHA256 != "" || got.EntryTokenSHA256 != "" {
				t.Fatalf("source layer was admitted: verdict=%q want=%q proof=%+v", got.Verdict, tc.want, got.Redacted())
			}
		})
	}
	// The physical write target must be regular even if the link resolves to
	// byte-identical operator data. This is a read-only origin proof.
	t.Run("symlink-target", func(t *testing.T) {
		dir := t.TempDir()
		foreign := filepath.Join(dir, "foreign.json")
		if err := os.WriteFile(foreign, []byte(attachOriginDirectFixture), 0600); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(dir, "mimocode.json")
		if err := os.Symlink(foreign, target); err != nil {
			t.Skipf("test host cannot create file symlink: %v", err)
		}
		got := (&mimoCodeClient{path: target}).ReadSelectedEntryOrigin(context.Background(), "codegraph")
		if got.Verdict != SelectedEntryOriginAliasOrAmbiguous || got.TargetPathInternal != "" {
			t.Fatalf("symlink target was admitted: %+v", got.Redacted())
		}
	})
}
