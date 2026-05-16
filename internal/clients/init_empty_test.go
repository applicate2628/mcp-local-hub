// internal/clients/init_empty_test.go
//
// Per-adapter coverage for InitEmpty() (v0.4.5 init-button feature).
// Each adapter's empty stub bytes are pinned so a future stub-shape
// change cannot silently produce a config the parent CLI would
// reject. The adapter pointer constructors (`&vscodeClient{path:p}`)
// match the language-server-test pattern so the test does not depend
// on HOME / USERPROFILE / APPDATA env vars resolving on the host.
package clients

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestInitEmpty_PerAdapter_StubBytes(t *testing.T) {
	cases := []struct {
		name string
		make func(path string) Client
		rel  string
		want string
	}{
		{
			name: "claude-code",
			make: func(p string) Client { return &claudeCode{path: p} },
			rel:  ".claude.json",
			want: "{\n  \"mcpServers\": {}\n}\n",
		},
		{
			name: "codex-cli",
			make: func(p string) Client { return &codexCLI{path: p} },
			rel:  ".codex/config.toml",
			want: "[mcp_servers]\n",
		},
		{
			name: "cursor",
			make: func(p string) Client {
				return &cursorClient{jsonMCPClient: &jsonMCPClient{path: p, clientName: "cursor", urlField: "url"}}
			},
			rel:  ".cursor/mcp.json",
			want: "{\n  \"mcpServers\": {}\n}\n",
		},
		{
			name: "vscode",
			make: func(p string) Client { return &vscodeClient{path: p} },
			rel:  "AppData/Roaming/Code/User/mcp.json",
			want: "{\n  \"servers\": {}\n}\n",
		},
		{
			name: "qwen-cli",
			make: func(p string) Client {
				return &qwenCLI{jsonMCPClient: &jsonMCPClient{path: p, clientName: "qwen-cli", urlField: "httpUrl"}}
			},
			rel:  ".qwen/settings.json",
			want: "{\n  \"mcpServers\": {}\n}\n",
		},
		{
			name: "json-mcp-gemini-like",
			make: func(p string) Client {
				return &jsonMCPClient{path: p, clientName: "gemini-cli", urlField: "url"}
			},
			rel:  ".gemini/settings.json",
			want: "{\n  \"mcpServers\": {}\n}\n",
		},
		{
			name: "antigravity-via-base",
			make: func(p string) Client {
				return &antigravityClient{
					jsonMCPClient: &jsonMCPClient{path: p, clientName: "antigravity", urlField: "command"},
				}
			},
			rel:  ".gemini/antigravity/mcp_config.json",
			want: "{\n  \"mcpServers\": {}\n}\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, tc.rel)
			c := tc.make(path)

			if err := c.InitEmpty(); err != nil {
				t.Fatalf("InitEmpty: %v", err)
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read stub: %v", err)
			}
			if string(body) != tc.want {
				t.Errorf("stub=%q, want %q", body, tc.want)
			}
		})
	}
}

// TestInitEmpty_Idempotent guards the second-click contract: a stub
// already on disk MUST NOT be overwritten. The test seeds a custom
// payload, calls InitEmpty, and verifies the file bytes are
// unchanged.
func TestInitEmpty_Idempotent(t *testing.T) {
	cases := []struct {
		name string
		make func(path string) Client
		rel  string
	}{
		{"claude-code", func(p string) Client { return &claudeCode{path: p} }, ".claude.json"},
		{"codex-cli", func(p string) Client { return &codexCLI{path: p} }, ".codex/config.toml"},
		{"cursor", func(p string) Client {
			return &cursorClient{jsonMCPClient: &jsonMCPClient{path: p, clientName: "cursor", urlField: "url"}}
		}, ".cursor/mcp.json"},
		{"vscode", func(p string) Client { return &vscodeClient{path: p} }, "AppData/Roaming/Code/User/mcp.json"},
		{"qwen-cli", func(p string) Client {
			return &qwenCLI{jsonMCPClient: &jsonMCPClient{path: p, clientName: "qwen-cli", urlField: "httpUrl"}}
		}, ".qwen/settings.json"},
		{"json-mcp", func(p string) Client {
			return &jsonMCPClient{path: p, clientName: "gemini-cli", urlField: "url"}
		}, ".gemini/settings.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, tc.rel)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("mkdir parent: %v", err)
			}
			seed := []byte(`{"mcpServers":{"existing":{"command":"foo"}}}`)
			if err := os.WriteFile(path, seed, 0o600); err != nil {
				t.Fatalf("seed: %v", err)
			}

			c := tc.make(path)
			if err := c.InitEmpty(); err != nil {
				t.Fatalf("InitEmpty: %v", err)
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(body) != string(seed) {
				t.Errorf("idempotent InitEmpty rewrote bytes: got=%q want=%q", body, seed)
			}
		})
	}
}

// TestInitEmpty_CreatesMissingParentDirs guards the adapter-level
// contract: WriteConfigFile mkdir-p's the immediate parent so a
// fresh `~/.cursor/`, `%APPDATA%\Code\User\`, etc. tree is created
// alongside the stub. The /api/init-client-config endpoint adds a
// separate parent-presence gate to prevent surprising tree creation
// on hosts where the client is not installed — but at the adapter
// level the helper is permissive so BackupKeep's seed-then-backup
// path keeps working on a never-installed host.
func TestInitEmpty_CreatesMissingParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "tree", "config.json")
	c := &claudeCode{path: path}
	if err := c.InitEmpty(); err != nil {
		t.Fatalf("InitEmpty: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("stub not created at nested path: %v", err)
	}
}

// TestEnsureClientConfigStub_RefusesPreExistingSymlink pins the PR
// #208 deep-sec Lane C defense: an attacker-planted symlink at the
// init destination must NOT be followed. The function returns an
// explicit error containing "symlink" so the operator can take
// action; the symlink target is never touched.
//
// The test is POSIX-only because creating a symlink on Windows
// requires either Developer Mode or admin rights (CreateSymbolicLinkW
// rejects unprivileged callers by default), and a t.Skip() under
// that condition would silently let the regression slip through.
// The Lstat-based defense in EnsureClientConfigStub is platform-
// agnostic; the POSIX test exercises it through the symlink path
// while the Windows test below exercises the same path via the
// non-regular-entry branch (a junction or directory at destination).
func TestEnsureClientConfigStub_RefusesPreExistingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows; see RefusesPreExistingDirectory for the equivalent Windows-side defense")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "attacker-controlled-target.dat")
	if err := os.WriteFile(target, []byte("pre-existing target content"), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(dir, "mcp.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	err := EnsureClientConfigStub(link, []byte(`{"servers": {}}`))
	if err == nil {
		t.Fatal("EnsureClientConfigStub through symlink: got nil, want refusal error")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error message %q does not mention symlink", err)
	}

	body, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("read target after refused init: %v", readErr)
	}
	if string(body) != "pre-existing target content" {
		t.Errorf("symlink target was modified despite refusal: got %q", body)
	}
}

// TestEnsureClientConfigStub_RefusesPreExistingDirectory pins the
// cross-platform defense against non-regular entries at the
// destination. A directory (or junction on Windows) at the path must
// NOT be silently treated as success.
func TestEnsureClientConfigStub_RefusesPreExistingDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("seed directory: %v", err)
	}

	err := EnsureClientConfigStub(path, []byte(`{"servers": {}}`))
	if err == nil {
		t.Fatal("EnsureClientConfigStub on a directory: got nil, want refusal error")
	}
	if !strings.Contains(err.Error(), "non-regular") {
		t.Errorf("error message %q does not mention non-regular entry", err)
	}
}

// TestEnsureClientConfigStub_AtomicConcurrentCreate pins the PR #208
// deep-sec Lane A defense: two concurrent EnsureClientConfigStub
// calls on the same missing path converge to a single write (the
// loser observes EEXIST from the O_EXCL race and treats it as
// idempotent success). No corruption, no half-written content.
//
// A more interesting concurrent-writer test (Init racing with a
// real AddEntry) is hard to write deterministically without
// injecting an explicit synchronization seam — that scenario is
// covered structurally by the new O_CREAT|O_EXCL contract: any
// concurrent writer that wins the create observes EEXIST in the
// loser branch.
func TestEnsureClientConfigStub_AtomicConcurrentCreate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	stub := []byte(`{"servers": {}}`)

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make([]error, goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			errs[i] = EnsureClientConfigStub(path, stub)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	if string(body) != string(stub) {
		t.Errorf("body=%q, want %q (concurrent writers corrupted content)", body, stub)
	}
}
