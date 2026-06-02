package clients

import (
	"os"
	"path/filepath"
	"testing"
)

// writeBak is a tiny helper: writes content to livePath + suffix and
// returns the full path. Used to seed backup fixtures.
func writeBak(t *testing.T, livePath, suffix, content string) string {
	t.Helper()
	p := livePath + suffix
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("seed backup %s: %v", p, err)
	}
	return p
}

// TestBackupEntryIsHubManaged_PerAdapterShape locks each adapter's
// hub-shape detection: hub-managed form → true, pre-hub/direct form →
// false, absent → false. Mirrors the §"Quality: Iterate timestamped
// backups newest-first" per-adapter predicate requirement in
// work-items/bugs/2026-05-15-demigrate-fallback-when-no-pre-hub-form.md.
func TestBackupEntryIsHubManaged_PerAdapterShape(t *testing.T) {
	type adapterCase struct {
		name      string
		newClient func(path string) Client
		// hubBody is the backup file content with `srv` in hub-managed
		// shape; preBody has it in pre-hub (stdio/relay-absent) shape;
		// absentBody lacks `srv` entirely.
		hubBody    string
		preBody    string
		absentBody string
	}

	const srv = "srv"
	cases := []adapterCase{
		{
			name:       "claude-code (mcpServers, url-shape)",
			newClient:  func(p string) Client { return &claudeCode{path: p} },
			hubBody:    `{"mcpServers":{"srv":{"type":"http","url":"http://localhost:9200/mcp"}}}`,
			preBody:    `{"mcpServers":{"srv":{"type":"stdio","command":"npx","args":["-y","x"]}}}`,
			absentBody: `{"mcpServers":{"other":{"type":"http","url":"http://localhost:9200/mcp"}}}`,
		},
		{
			name: "gemini-cli (jsonMCPClient url-shape)",
			newClient: func(p string) Client {
				return &geminiCLI{jsonMCPClient: &jsonMCPClient{path: p, clientName: "gemini-cli", urlField: "url"}}
			},
			hubBody:    `{"mcpServers":{"srv":{"url":"http://127.0.0.1:9200/mcp","type":"http"}}}`,
			preBody:    `{"mcpServers":{"srv":{"command":"uvx","args":["mcp-server-time"]}}}`,
			absentBody: `{"mcpServers":{}}`,
		},
		{
			name: "cursor (jsonMCPClient url-shape)",
			newClient: func(p string) Client {
				return &cursorClient{jsonMCPClient: &jsonMCPClient{path: p, clientName: "cursor", urlField: "url"}}
			},
			hubBody:    `{"mcpServers":{"srv":{"type":"http","url":"http://[::1]:9200/mcp"}}}`,
			preBody:    `{"mcpServers":{"srv":{"command":"node","args":["s.js"]}}}`,
			absentBody: `{"mcpServers":{"unrelated":{"command":"x"}}}`,
		},
		{
			name:       "vscode (top-level servers key)",
			newClient:  func(p string) Client { return &vscodeClient{path: p} },
			hubBody:    `{"servers":{"srv":{"type":"http","url":"http://localhost:9200/mcp"}}}`,
			preBody:    `{"servers":{"srv":{"command":"npx","args":["-y","x"]}}}`,
			absentBody: `{"servers":{}}`,
		},
		{
			name:       "codex-cli (TOML mcp_servers url-shape)",
			newClient:  func(p string) Client { return &codexCLI{path: p} },
			hubBody:    "[mcp_servers.srv]\nurl = \"http://localhost:9200/mcp\"\n",
			preBody:    "[mcp_servers.srv]\ncommand = \"uvx\"\nargs = [\"mcp-server-time\"]\n",
			absentBody: "[mcp_servers.other]\nurl = \"http://localhost:9200/mcp\"\n",
		},
		{
			name: "antigravity (relay shape)",
			newClient: func(p string) Client {
				base := &jsonMCPClient{path: p, clientName: "antigravity", urlField: "command"}
				return &antigravityClient{jsonMCPClient: base}
			},
			// Relay shape: command is mcphub, args[0] == "relay".
			hubBody: `{"mcpServers":{"srv":{"command":"C:\\bin\\mcphub.exe","args":["relay","--server","srv","--daemon","default"],"disabled":false}}}`,
			// Pre-hub stdio: a user-direct command that is NOT mcphub relay.
			preBody:    `{"mcpServers":{"srv":{"command":"uvx","args":["mcp-server-time"]}}}`,
			absentBody: `{"mcpServers":{}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			live := filepath.Join(dir, "config")
			c := tc.newClient(live)

			hub := writeBak(t, live, ".hub", tc.hubBody)
			pre := writeBak(t, live, ".pre", tc.preBody)
			absent := writeBak(t, live, ".absent", tc.absentBody)

			if got, err := c.BackupEntryIsHubManaged(hub, srv); err != nil || !got {
				t.Errorf("hub-managed backup: got (%v,%v), want (true,nil)", got, err)
			}
			if got, err := c.BackupEntryIsHubManaged(pre, srv); err != nil || got {
				t.Errorf("pre-hub backup: got (%v,%v), want (false,nil)", got, err)
			}
			if got, err := c.BackupEntryIsHubManaged(absent, srv); err != nil || got {
				t.Errorf("entry-absent backup: got (%v,%v), want (false,nil)", got, err)
			}
		})
	}
}

// TestBackupEntryIsHubManaged_RemoteHTTPUserEntryNotHubManaged guards
// the loopback-only heuristic: a user-configured REMOTE HTTP MCP server
// (non-loopback url) must NOT be classified as hub-managed, so demigrate
// never treats it as a deletable mcphub install.
func TestBackupEntryIsHubManaged_RemoteHTTPUserEntryNotHubManaged(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, ".claude.json")
	c := &claudeCode{path: live}
	bak := writeBak(t, live, ".bak-mcp-local-hub-20260101-000000",
		`{"mcpServers":{"ctx7":{"type":"http","url":"https://api.example.com/mcp"}}}`)
	if got, err := c.BackupEntryIsHubManaged(bak, "ctx7"); err != nil || got {
		t.Errorf("remote user HTTP entry: got (%v,%v), want (false,nil) — loopback-only heuristic", got, err)
	}
}

// TestBackupEntryIsHubManaged_ReadAndParseErrors verifies the predicate
// surfaces I/O and parse failures (so the demigrate caller can skip a
// malformed legacy backup rather than silently treat it as pre-hub).
func TestBackupEntryIsHubManaged_ReadAndParseErrors(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, ".claude.json")
	c := &claudeCode{path: live}

	// Missing file → read error.
	if _, err := c.BackupEntryIsHubManaged(filepath.Join(dir, "does-not-exist"), "srv"); err == nil {
		t.Error("missing backup: want read error, got nil")
	}
	// Malformed JSON → parse error.
	bad := writeBak(t, live, ".bad", `{not json`)
	if _, err := c.BackupEntryIsHubManaged(bad, "srv"); err == nil {
		t.Error("malformed JSON backup: want parse error, got nil")
	}
	// Empty file → (false, nil) — treated as absent, not an error.
	empty := writeBak(t, live, ".empty", ``)
	if got, err := c.BackupEntryIsHubManaged(empty, "srv"); err != nil || got {
		t.Errorf("empty backup: got (%v,%v), want (false,nil)", got, err)
	}
}

// TestLegacyBackupsNewestFirst_BucketOrderAndRecency verifies the helper
// returns legacy-codename backups grouped by bucket priority (mcp-sync,
// plain-timestamp, phase2, underscore-date) and newest-first within a
// bucket, while excluding the current mcp-local-hub backups and the
// -original sentinel.
func TestLegacyBackupsNewestFirst_BucketOrderAndRecency(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(live, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Seed a mix. Names chosen so bucket + recency ordering is checkable.
	mustWrite := func(suffix string) string {
		p := live + suffix
		if err := os.WriteFile(p, []byte(`{}`), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		return p
	}
	// Current-codename — MUST be excluded.
	mustWrite(".bak-mcp-local-hub-20260501-000000")
	mustWrite(".bak-mcp-local-hub-original")
	// mcp-sync bucket (date-suffixed real-world shape + prefix shape).
	syncOld := mustWrite(".bak-2026-04-10-mcp-sync")
	syncNew := mustWrite(".bak-2026-04-15-mcp-sync")
	// plain-timestamp bucket.
	plainOld := mustWrite(".bak-20260101-090000")
	plainNew := mustWrite(".bak-20260102-090000")
	// phase2 bucket.
	phase2 := mustWrite(".bak-phase2-install")
	// underscore-date bucket.
	dash := mustWrite(".bak-2026-03-01_12-00-00")
	// Unrelated file — MUST be ignored (not a .bak- backup).
	mustWrite(".keep")

	got, err := LegacyBackupsNewestFirst(live, "gemini-cli")
	if err != nil {
		t.Fatalf("LegacyBackupsNewestFirst: %v", err)
	}

	want := []string{
		syncNew, syncOld, // mcp-sync bucket, newest first
		plainNew, plainOld, // plain bucket, newest first
		phase2, // phase2 bucket
		dash,   // underscore-date bucket
	}
	if len(got) != len(want) {
		t.Fatalf("got %d legacy backups, want %d.\n got=%v\nwant=%v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d: got %s, want %s\nfull got=%v", i, got[i], want[i], got)
		}
	}
}

// TestLegacyBackupsNewestFirst_ExcludesCurrentCodename is a tight guard
// that no mcp-local-hub backup (timestamped or sentinel) ever leaks into
// the legacy result — they belong to BackupsNewestFirst.
func TestLegacyBackupsNewestFirst_ExcludesCurrentCodename(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(live, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		".bak-mcp-local-hub-20260501-000000",
		".bak-mcp-local-hub-20260601-000000",
		".bak-mcp-local-hub-original",
	} {
		if err := os.WriteFile(live+s, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := LegacyBackupsNewestFirst(live, "claude-code")
	if err != nil {
		t.Fatalf("LegacyBackupsNewestFirst: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 legacy backups (only mcp-local-hub present), got %v", got)
	}
}

// TestLegacyBackupsNewestFirst_NoDirReturnsEmpty verifies a missing parent
// directory yields an empty slice, not an error.
func TestLegacyBackupsNewestFirst_NoDirReturnsEmpty(t *testing.T) {
	got, err := LegacyBackupsNewestFirst(filepath.Join(t.TempDir(), "nope", ".claude.json"), "claude-code")
	if err != nil {
		t.Fatalf("expected nil error for missing dir, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice for missing dir, got %v", got)
	}
}
