package clients

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBackupSentinelWrittenOnlyFirstTime verifies the pristine-original
// sentinel (.bak-mcp-local-hub-original) is written exactly once on the
// first Backup call and never overwritten afterwards, even if the live
// config has since been modified.
func TestBackupSentinelWrittenOnlyFirstTime(t *testing.T) {
	tmp := t.TempDir()
	livePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(livePath, []byte(`{"initial":true}`), 0600); err != nil {
		t.Fatal(err)
	}

	// jsonMCPClient is the shared base adapter used by several JSON clients;
	// its Backup path exercises the same writeBackup helper that managed
	// adapters delegate to, so one adapter is enough to lock in the sentinel
	// contract.
	adapter := &jsonMCPClient{path: livePath, clientName: "claude-code", urlField: "url"}

	// First backup — should create the sentinel.
	if _, err := adapter.Backup(); err != nil {
		t.Fatalf("first backup: %v", err)
	}
	sentinel := livePath + ".bak-mcp-local-hub-original"
	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("sentinel not created: %v", err)
	}
	if string(data) != `{"initial":true}` {
		t.Errorf("sentinel content wrong: %s", data)
	}

	// Modify the live file, then back up again. The sentinel must remain
	// pinned to the ORIGINAL content — that's the whole point of the
	// one-shot pristine strategy.
	if err := os.WriteFile(livePath, []byte(`{"modified":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Backup(); err != nil {
		t.Fatalf("second backup: %v", err)
	}
	data, err = os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("sentinel disappeared after second backup: %v", err)
	}
	if string(data) != `{"initial":true}` {
		t.Errorf("sentinel got overwritten on second backup: %s", data)
	}
}

// TestBackupKeepsNLatestTimestamped verifies that after N+3 BackupKeep
// calls, only the N most recent timestamped backups remain on disk,
// plus the one pristine sentinel.
func TestBackupKeepsNLatestTimestamped(t *testing.T) {
	tmp := t.TempDir()
	livePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(livePath, []byte(`{"v":0}`), 0600); err != nil {
		t.Fatal(err)
	}

	adapter := &jsonMCPClient{path: livePath, clientName: "claude-code", urlField: "url"}

	// 8 backups with sleep between them so each lands on a distinct
	// timestamp-second (Windows FS resolves mtime only to the second).
	// keepN = 5, so after the 8th call 3 older backups should be pruned.
	for i := 1; i <= 8; i++ {
		if err := os.WriteFile(livePath, []byte(fmt.Sprintf(`{"v":%d}`, i)), 0600); err != nil {
			t.Fatalf("rewrite live %d: %v", i, err)
		}
		if _, err := adapter.BackupKeep(5); err != nil {
			t.Fatalf("backup %d: %v", i, err)
		}
		time.Sleep(1100 * time.Millisecond)
	}

	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	var timestamped, original int
	for _, e := range entries {
		name := e.Name()
		switch {
		case strings.HasSuffix(name, ".bak-mcp-local-hub-original"):
			original++
		case strings.Contains(name, ".bak-mcp-local-hub-"):
			timestamped++
		}
	}
	if original != 1 {
		t.Errorf("expected 1 sentinel, got %d", original)
	}
	if timestamped != 5 {
		t.Errorf("expected 5 timestamped backups after keep=5, got %d", timestamped)
	}
}

// TestBackupKeepN_DoesNotRemoveSentinel verifies that even when keepN is
// small and there are many timestamped backups, the pristine sentinel is
// never pruned — it must survive arbitrary rotation.
func TestBackupKeepN_DoesNotRemoveSentinel(t *testing.T) {
	tmp := t.TempDir()
	livePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(livePath, []byte(`{"pristine":true}`), 0600); err != nil {
		t.Fatal(err)
	}

	adapter := &jsonMCPClient{path: livePath, clientName: "claude-code", urlField: "url"}

	// Seed the sentinel via a first Backup call with pristine content, then
	// overwrite the live config so subsequent backups differ.
	if _, err := adapter.BackupKeep(1); err != nil {
		t.Fatalf("seed backup: %v", err)
	}
	sentinel := livePath + ".bak-mcp-local-hub-original"
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != `{"pristine":true}` {
		t.Fatalf("sentinel seeded wrong: data=%q err=%v", data, err)
	}

	// Churn the rolling window. keepN=1 is aggressive — at each call only one
	// timestamped backup should survive, plus the pristine sentinel.
	for i := 1; i <= 4; i++ {
		if err := os.WriteFile(livePath, []byte(fmt.Sprintf(`{"v":%d}`, i)), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := adapter.BackupKeep(1); err != nil {
			t.Fatalf("churn backup %d: %v", i, err)
		}
		time.Sleep(1100 * time.Millisecond)
	}

	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("sentinel was removed by pruning: %v", err)
	}
	if data, _ := os.ReadFile(sentinel); string(data) != `{"pristine":true}` {
		t.Errorf("sentinel content mutated: %s", data)
	}
}

// TestBackupKeepZero_DoesNotPrune verifies that BackupKeep(0) and
// Backup() are equivalent: they leave every existing timestamped backup
// in place. This preserves the pre-rotation contract for install.go
// and migrate.go, which still call Backup() without a keep cap.
func TestBackupKeepZero_DoesNotPrune(t *testing.T) {
	tmp := t.TempDir()
	livePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(livePath, []byte(`{"v":0}`), 0600); err != nil {
		t.Fatal(err)
	}

	adapter := &jsonMCPClient{path: livePath, clientName: "claude-code", urlField: "url"}

	for i := 1; i <= 3; i++ {
		if err := os.WriteFile(livePath, []byte(fmt.Sprintf(`{"v":%d}`, i)), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := adapter.Backup(); err != nil {
			t.Fatalf("backup %d: %v", i, err)
		}
		time.Sleep(1100 * time.Millisecond)
	}

	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	var timestamped int
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".bak-mcp-local-hub-original") {
			continue
		}
		if strings.Contains(name, ".bak-mcp-local-hub-") {
			timestamped++
		}
	}
	if timestamped != 3 {
		t.Errorf("Backup() (keepN=0) must not prune; want 3 timestamped, got %d", timestamped)
	}
}

func TestNextBackupPathAllocatesAboveHighestSameSecondSuffix(t *testing.T) {
	tmp := t.TempDir()
	livePath := filepath.Join(tmp, ".claude.json")

	var got string
	var seeded string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		removeTimestampedBackupsForTest(t, livePath)
		waitForFreshBackupSecondForTest()
		stamp := time.Now().Format("20060102-150405")
		seeded = livePath + backupSuffixPrefix + stamp + "-000000002"
		if err := os.WriteFile(seeded, []byte("seed"), 0o600); err != nil {
			t.Fatalf("seed suffixed backup: %v", err)
		}
		var err error
		got, err = nextBackupPath(livePath)
		if err != nil {
			t.Fatalf("nextBackupPath: %v", err)
		}
		if backupSecondStampForTest(got) == stamp {
			break
		}
	}
	if backupSecondStampForTest(got) != backupSecondStampForTest(seeded) {
		t.Fatalf("could not exercise same-second allocation; seeded %q got %q", seeded, got)
	}
	if want := strings.TrimSuffix(seeded, "-000000002") + "-000000003"; got != want {
		t.Fatalf("nextBackupPath = %q, want %q", got, want)
	}
}

func TestLatestBackup_PrefersMostRecentTimestamped(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "foo.json")
	if err := os.WriteFile(live, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	// Three timestamped backups. Current backup name format is
	// `<livePath>.bak-mcp-local-hub-<YYYYMMDD-HHMMSS>` (see
	// clients.go:writeBackup timestamp layout). Lexicographic order
	// matches chronological order because the digits are fixed-width.
	for _, ts := range []string{"20260101-120000", "20260201-120000", "20260115-120000"} {
		p := filepath.Join(dir, "foo.json.bak-mcp-local-hub-"+ts)
		if err := os.WriteFile(p, []byte(`{"ts":"`+ts+`"}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Pristine sentinel — must be returned only when no timestamped
	// backup exists.
	if err := os.WriteFile(filepath.Join(dir, "foo.json.bak-mcp-local-hub-original"),
		[]byte(`{"pristine":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	path, ok, err := latestBackup(live, "test-client")
	if err != nil {
		t.Fatalf("latestBackup: %v", err)
	}
	if !ok {
		t.Fatalf("latestBackup: expected backup to exist")
	}
	if !strings.HasSuffix(path, "foo.json.bak-mcp-local-hub-20260201-120000") {
		t.Errorf("expected most recent timestamped backup, got %s", path)
	}
}

func TestLatestBackup_FallsBackToOriginalSentinel(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "foo.json")
	if err := os.WriteFile(live, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	origPath := filepath.Join(dir, "foo.json.bak-mcp-local-hub-original")
	if err := os.WriteFile(origPath, []byte(`{"pristine":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	path, ok, err := latestBackup(live, "test-client")
	if err != nil {
		t.Fatalf("latestBackup: %v", err)
	}
	if !ok {
		t.Fatalf("latestBackup: expected original sentinel to be picked up")
	}
	if path != origPath {
		t.Errorf("expected %s, got %s", origPath, path)
	}
}

func TestLatestBackup_ReturnsNotOkWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "foo.json")
	if err := os.WriteFile(live, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, ok, err := latestBackup(live, "test-client")
	if err != nil {
		t.Fatalf("latestBackup: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false when no backup files present")
	}
}

// TestHubLoopbackURL pins the single owner of hub loopback URL construction:
// it must emit the explicit IPv4 loopback (127.0.0.1), NEVER "localhost" — a
// regression here reintroduces the ~2 s IPv6 ::1 connect-timeout per MCP call.
// IsHubHTTPURL must in turn accept the output (round-trip), and the host
// constant must match.
func TestHubLoopbackURL(t *testing.T) {
	if HubLoopbackHost != "127.0.0.1" {
		t.Fatalf("HubLoopbackHost = %q, want 127.0.0.1 (localhost would reintroduce the IPv6 lag)", HubLoopbackHost)
	}
	cases := []struct {
		port int
		path string
		want string
	}{
		{9200, "/mcp", "http://127.0.0.1:9200/mcp"},
		{9133, "/clients/claude-code/mcp", "http://127.0.0.1:9133/clients/claude-code/mcp"},
		{1, "/", "http://127.0.0.1:1/"},
	}
	for _, tc := range cases {
		got := HubLoopbackURL(tc.port, tc.path)
		if got != tc.want {
			t.Errorf("HubLoopbackURL(%d, %q) = %q, want %q", tc.port, tc.path, got, tc.want)
		}
		if strings.Contains(got, "localhost") {
			t.Errorf("HubLoopbackURL must never emit 'localhost': %q", got)
		}
		if !IsHubHTTPURL(got) {
			t.Errorf("IsHubHTTPURL must accept the builder's own output: %q", got)
		}
	}
}

func TestIsHubHTTPURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want bool
	}{
		{"localhost loopback hub", "http://localhost:9200/mcp", true},
		{"127.0.0.1 loopback hub", "http://127.0.0.1:9200/mcp", true},
		{"IPv6 [::1] loopback hub (PR #4 Codex R3)", "http://[::1]:9200/mcp", true},
		{"remote https", "https://api.example.com/mcp", false},
		{"remote http", "http://api.example.com/mcp", false},
		{"subdomain spoof with 127.0.0.1", "http://127.0.0.1.evil.com/mcp", false},
		{"empty", "", false},
		{"stdio scheme", "stdio:///memory", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsHubHTTPURL(tc.url)
			if got != tc.want {
				t.Errorf("IsHubHTTPURL(%q) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}

func TestDefaultInstallClientsExcludeOptInHeavyClients(t *testing.T) {
	got := DefaultInstallClientNames()
	want := []string{"claude-code", "codex-cli"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("DefaultInstallClientNames() = %v, want %v", got, want)
	}
	excluded := map[string]bool{}
	for _, c := range got {
		excluded[c] = true
	}
	for _, optIn := range []string{
		// cursor is a SUPPORTED client but OPT-IN — it must be reachable via
		// --clients cursor yet never touched by a bare/default install.
		"cursor",
		"gemini-cli", "antigravity", "qwen-cli", "vscode",
		// Wave 2: all opt-in too (mimocode is an opencode fork added alongside).
		"zed", "kiro", "windsurf", "cline", "kilocode", "opencode", "mimocode", "hermes", "openclaw",
	} {
		if excluded[optIn] {
			t.Fatalf("opt-in client %q must not be default", optIn)
		}
	}
	// cursor stays SUPPORTED — opt-in, not removed. Pin both halves of the
	// invariant the operator asked for: present in SupportedClientNames,
	// absent from DefaultInstallClientNames.
	supported := map[string]bool{}
	for _, c := range SupportedClientNames() {
		supported[c] = true
	}
	if !supported["cursor"] {
		t.Fatalf("cursor must remain a SUPPORTED client (reachable via --clients cursor); SupportedClientNames() = %v", SupportedClientNames())
	}
}

func TestSupportedClientNamesIncludesNewClients(t *testing.T) {
	got := map[string]bool{}
	for _, c := range SupportedClientNames() {
		got[c] = true
	}
	for _, want := range []string{
		"claude-code", "codex-cli", "gemini-cli", "antigravity", "cursor", "vscode", "qwen-cli",
		// Wave 2: all must be enumerated (mimocode is an opencode fork added alongside).
		"zed", "kiro", "windsurf", "cline", "kilocode", "opencode", "mimocode", "hermes", "openclaw",
	} {
		if !got[want] {
			t.Fatalf("supported client %q missing from %v", want, SupportedClientNames())
		}
	}
}

// TestConfigPathForName_MatchesAdapterConfigPath is the reconcile guard for
// the single-owner refactor: the config-file path is no longer encoded twice
// per client (once in a descriptor closure, once in the adapter's ConfigPath()).
// ConfigPathForName now resolves through the constructed adapter, so this test
// pins that the resolver and the adapter agree for EVERY client.
//
// The former TestDefaultScanConfigPaths_CoversEverySupportedClient could not
// catch a descriptor-vs-adapter divergence because both sides derived from the
// same descriptor field. This test compares the resolver against the adapter's
// OWN ConfigPath(), so a future re-introduction of a parallel path literal that
// drifts from the adapter fails here.
func TestConfigPathForName_MatchesAdapterConfigPath(t *testing.T) {
	all := AllClients()
	if len(all) == 0 {
		t.Fatal("AllClients() returned no adapters (UserHomeDir likely failed on this host)")
	}
	for name, adapter := range all {
		resolved, err := ConfigPathForName(name)
		if err != nil {
			t.Errorf("ConfigPathForName(%q) errored although the adapter constructed: %v", name, err)
			continue
		}
		if got := adapter.ConfigPath(); got != resolved {
			t.Errorf("path drift for %q: adapter.ConfigPath() = %q, ConfigPathForName() = %q "+
				"(the adapter must be the single owner; resolver must read the same write surface)",
				name, got, resolved)
		}
	}
}

func TestLatestBackup_IgnoresDirectoriesWithBackupPrefix(t *testing.T) {
	// Defensive: if something odd (a checkout side-channel, an archiver)
	// leaves a DIRECTORY whose name starts with the backup prefix,
	// latestBackup must not return that directory as the "backup path".
	dir := t.TempDir()
	live := filepath.Join(dir, "foo.json")
	if err := os.WriteFile(live, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "foo.json.bak-mcp-local-hub-20260101-000000"), 0700); err != nil {
		t.Fatal(err)
	}
	_, ok, err := latestBackup(live, "test-client")
	if err != nil {
		t.Fatalf("latestBackup: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false — directory must not count as a backup")
	}
}
