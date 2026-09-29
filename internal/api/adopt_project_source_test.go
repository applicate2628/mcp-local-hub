package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
)

const validProjectAdoptConfig = `[mcp_servers.demo]
command = "tool"
args = ["one", "two"]
tool_timeout_sec = 9

[mcp_servers.demo.env]
OK = "yes"
`

func writeProjectAdoptConfig(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	configDir := filepath.Join(root, ".codex")
	if err := os.Mkdir(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestExtractCodexStdioEntryFromBytesPreservesGlobalSemantics(t *testing.T) {
	data := []byte(`[mcp_servers.demo]
command = "tool"
url = "https://example.invalid/mcp"
args = ["one", 7, "two"]
disabled = true
tool_timeout_sec = 9

[mcp_servers.demo.env]
OK = "yes"
BAD = 7
`)

	entry, err := extractCodexStdioEntryFromBytes(data, "demo", codexStdioEntryGlobal)
	if err != nil {
		t.Fatalf("extractCodexStdioEntryFromBytes(global): %v", err)
	}
	if entry.Command != "tool" || !reflect.DeepEqual(entry.Args, []string{"one", "two"}) ||
		!reflect.DeepEqual(entry.Env, map[string]string{"OK": "yes"}) || entry.ToolTimeoutSec != 9 || !entry.Disabled {
		t.Fatalf("global entry = %+v", entry)
	}
}

func TestExtractCodexStdioEntryFromBytesStrictProjectValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		wantIs error
		want   string
	}{
		{
			name:   "entry absent",
			body:   `[mcp_servers.other]` + "\n" + `command = "tool"`,
			wantIs: ErrClientEntryNotPresent,
		},
		{
			name:   "mixed command and url",
			body:   `[mcp_servers.demo]` + "\n" + `command = "tool"` + "\n" + `url = "https://example.invalid/mcp"`,
			wantIs: ErrClientEntryNotStdio,
		},
		{
			name:   "command absent",
			body:   `[mcp_servers.demo]` + "\n" + `url = "https://example.invalid/mcp"`,
			wantIs: ErrClientEntryNotStdio,
		},
		{
			name:   "command empty",
			body:   `[mcp_servers.demo]` + "\n" + `command = ""`,
			wantIs: ErrClientEntryNotStdio,
		},
		{
			name: "disabled",
			body: `[mcp_servers.demo]` + "\n" + `command = "tool"` + "\n" + `disabled = true`,
			want: "disabled",
		},
		{
			name: "non-string argument",
			body: `[mcp_servers.demo]` + "\n" + `command = "tool"` + "\n" + `args = ["one", 7]`,
			want: "argument",
		},
		{
			name: "non-string environment value",
			body: `[mcp_servers.demo]` + "\n" + `command = "tool"` + "\n" + `env = { OK = "yes", BAD = 7 }`,
			want: "environment",
		},
		{
			name: "invalid timeout",
			body: `[mcp_servers.demo]` + "\n" + `command = "tool"` + "\n" + `tool_timeout_sec = -1`,
			want: "tool_timeout_sec",
		},
		{
			name:   "hub relay",
			body:   `[mcp_servers.demo]` + "\n" + `command = "mcphub"` + "\n" + `args = ["relay", "--url", "http://127.0.0.1:9300/mcp"]`,
			wantIs: ErrClientEntryHubRelay,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := extractCodexStdioEntryFromBytes([]byte(tc.body), "demo", codexStdioEntryProject)
			if err == nil {
				t.Fatal("strict project extraction succeeded, want failure")
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Fatalf("error = %v, want errors.Is(%v)", err, tc.wantIs)
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestExtractCodexProjectAdoptSourceBindsOneImmutableSnapshot(t *testing.T) {
	root := writeProjectAdoptConfig(t, validProjectAdoptConfig)
	wantBytes := []byte(validProjectAdoptConfig)
	wantConfigHashBytes := sha256.Sum256(wantBytes)
	wantConfigHash := hex.EncodeToString(wantConfigHashBytes[:])
	wantIdentityHash, err := clients.CodexProjectIdentitySHA256ForRoot(root)
	if err != nil {
		t.Fatalf("CodexProjectIdentitySHA256ForRoot: %v", err)
	}

	source, err := extractCodexProjectAdoptSource(root, "demo")
	if err != nil {
		t.Fatalf("extractCodexProjectAdoptSource: %v", err)
	}
	if source.identity.CanonicalRoot() != root || source.identity.ProjectIdentitySHA256() != wantIdentityHash ||
		source.configSHA256 != wantConfigHash || source.logicalEntryName != "demo" {
		t.Fatalf("source identity = root %q identity %q config %q entry %q", source.identity.CanonicalRoot(), source.identity.ProjectIdentitySHA256(), source.configSHA256, source.logicalEntryName)
	}
	if !bytes.Equal(source.configBytes, wantBytes) || source.entry.Command != "tool" ||
		!reflect.DeepEqual(source.entry.Args, []string{"one", "two"}) ||
		!reflect.DeepEqual(source.entry.Env, map[string]string{"OK": "yes"}) || source.entry.ToolTimeoutSec != 9 {
		t.Fatalf("source snapshot = bytes %q entry %+v", source.configBytes, source.entry)
	}

	if err := os.WriteFile(filepath.Join(root, ".codex", "config.toml"), []byte("[mcp_servers.changed]\ncommand=\"other\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(source.configBytes, wantBytes) || source.entry.Command != "tool" || source.configSHA256 != wantConfigHash {
		t.Fatalf("source changed after config rewrite: bytes %q entry %+v hash %q", source.configBytes, source.entry, source.configSHA256)
	}
	rootEntries, err := os.ReadDir(root)
	if err != nil || len(rootEntries) != 1 || rootEntries[0].Name() != ".codex" {
		t.Fatalf("project root entries = %v, err=%v", rootEntries, err)
	}
	configEntries, err := os.ReadDir(filepath.Join(root, ".codex"))
	if err != nil || len(configEntries) != 1 || configEntries[0].Name() != "config.toml" {
		t.Fatalf("project config entries = %v, err=%v", configEntries, err)
	}
}

func TestCodexProjectSourcePreimageTokenFraming(t *testing.T) {
	identityHash := strings.Repeat("00", 32)
	configHash := strings.Repeat("11", 32)
	got, err := codexProjectSourcePreimageToken(identityHash, "graphify", configHash)
	if err != nil {
		t.Fatalf("codexProjectSourcePreimageToken: %v", err)
	}
	const want = "a10ea13fca0670b9ccbe71c27c10381cf01e34676e793d5512622fcc5fd8f848"
	if got != want {
		t.Fatalf("token = %q, want %q", got, want)
	}

	for _, tc := range []struct {
		name         string
		identityHash string
		entry        string
		configHash   string
	}{
		{name: "identity", identityHash: strings.Repeat("01", 32), entry: "graphify", configHash: configHash},
		{name: "entry", identityHash: identityHash, entry: "other", configHash: configHash},
		{name: "config", identityHash: identityHash, entry: "graphify", configHash: strings.Repeat("22", 32)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed, err := codexProjectSourcePreimageToken(tc.identityHash, tc.entry, tc.configHash)
			if err != nil || changed == want {
				t.Fatalf("changed token = %q, err=%v", changed, err)
			}
		})
	}
}

func TestCodexProjectSourcePreimageTokenRejectsMalformedMaterial(t *testing.T) {
	for _, tc := range []struct {
		name         string
		identityHash string
		entry        string
		configHash   string
	}{
		{name: "short identity", identityHash: strings.Repeat("0", 63), entry: "demo", configHash: strings.Repeat("1", 64)},
		{name: "uppercase identity", identityHash: strings.Repeat("A", 64), entry: "demo", configHash: strings.Repeat("1", 64)},
		{name: "empty entry", identityHash: strings.Repeat("0", 64), entry: "", configHash: strings.Repeat("1", 64)},
		{name: "non-hex config", identityHash: strings.Repeat("0", 64), entry: "demo", configHash: strings.Repeat("1", 63) + "g"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := codexProjectSourcePreimageToken(tc.identityHash, tc.entry, tc.configHash)
			if err == nil || !errors.Is(err, clients.ErrCodexProjectSourceChanged) {
				t.Fatalf("error = %v, want CODEX_PROJECT_SOURCE_CHANGED", err)
			}
		})
	}
}
