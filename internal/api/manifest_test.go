package api

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManifestListReturnsAllYAML(t *testing.T) {
	tmp := t.TempDir()
	_ = os.MkdirAll(filepath.Join(tmp, "foo"), 0755)
	_ = os.WriteFile(filepath.Join(tmp, "foo", "manifest.yaml"),
		[]byte("name: foo\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9200\n"), 0644)
	_ = os.MkdirAll(filepath.Join(tmp, "bar"), 0755)
	_ = os.WriteFile(filepath.Join(tmp, "bar", "manifest.yaml"),
		[]byte("name: bar\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9201\n"), 0644)
	_ = os.MkdirAll(filepath.Join(tmp, "draft"), 0755)
	// draft dir has no manifest.yaml — should be skipped.

	a := NewAPI()
	names, err := a.ManifestListIn(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Errorf("expected 2 manifests, got %v", names)
	}
}

func TestManifestValidateCatchesMissingFields(t *testing.T) {
	a := NewAPI()
	warnings := a.ManifestValidate("name: foo\n") // missing kind, transport, command, daemons
	if len(warnings) == 0 {
		t.Error("expected warnings for incomplete manifest, got none")
	}
}

func TestManifestValidateWorkspaceScopedAllowsNoDaemons(t *testing.T) {
	a := NewAPI()
	yaml := "name: mcp-language-server\nkind: workspace-scoped\ntransport: stdio-bridge\ncommand: mcp-language-server\nport_pool: {start: 9200, end: 9299}\nlanguages:\n  - name: go\n    backend: gopls-mcp\n    transport: stdio\n    lsp_command: gopls\n"
	warnings := a.ManifestValidate(yaml)
	for _, w := range warnings {
		if w == "no daemons declared" {
			t.Fatalf("unexpected warning for workspace-scoped manifest: %v", warnings)
		}
	}
}

// TestManifestValidateRemoteHTTPAllowsNoDaemons pins codex bot r3
// P1 closure (PR #169): remote-http manifests legitimately have
// no local daemon (client connects directly to the remote URL),
// so the "no daemons declared" soft warning MUST be exempted.
// Without this exemption, ManifestCreateIn/ManifestEditIn would
// reject every valid remote-http manifest at the soft-warning gate.
func TestManifestValidateRemoteHTTPAllowsNoDaemons(t *testing.T) {
	a := NewAPI()
	yaml := `name: ctx7
kind: global
transport: remote-http
url: https://mcp.context7.com/mcp
headers:
  Authorization: Bearer ${secret:CONTEXT7_TOKEN}
client_bindings:
  - client: claude-code
`
	warnings := a.ManifestValidate(yaml)
	for _, w := range warnings {
		if w == "no daemons declared" {
			t.Fatalf("unexpected warning for remote-http manifest: %v", warnings)
		}
	}
}

// TestManifestValidateRemoteHTTPWeeklyRefreshTrueWarns pins codex
// bot r6 P2 closure (PR #169) + G6 spec §"Validation rules":
// remote-http has no local daemon to refresh, so weekly_refresh:
// true is a no-op. Emit a non-fatal warning so operators don't
// believe weekly refresh is active when it's ignored. The YAML
// bool collapses absent / false into the same Go value (false),
// so we can only flag the explicit-true case.
func TestManifestValidateRemoteHTTPWeeklyRefreshTrueWarns(t *testing.T) {
	a := NewAPI()
	yaml := `name: ctx7
kind: global
transport: remote-http
url: https://mcp.context7.com/mcp
weekly_refresh: true
client_bindings:
  - client: claude-code
`
	warnings := a.ManifestValidate(yaml)
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "weekly_refresh") && strings.Contains(w, "remote-http") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected weekly_refresh-on-remote-http warning; got %v", warnings)
	}
}

// TestManifestValidateRemoteHTTPWeeklyRefreshFalseSilent pins that
// the warning ONLY fires for explicit true. Default (absent) maps
// to false → no warning.
func TestManifestValidateRemoteHTTPWeeklyRefreshFalseSilent(t *testing.T) {
	a := NewAPI()
	yaml := `name: ctx7
kind: global
transport: remote-http
url: https://mcp.context7.com/mcp
client_bindings:
  - client: claude-code
`
	warnings := a.ManifestValidate(yaml)
	for _, w := range warnings {
		if strings.Contains(w, "weekly_refresh") {
			t.Errorf("unexpected weekly_refresh warning when key absent: %v", warnings)
		}
	}
}

// TestManifestValidateGlobalStdioBridgeStillWarnsOnNoDaemons pins
// the negative case: ordinary global stdio-bridge / native-http
// manifests with no daemons SHOULD still emit the warning. The
// remote-http exemption is narrow.
func TestManifestValidateGlobalStdioBridgeStillWarnsOnNoDaemons(t *testing.T) {
	a := NewAPI()
	yaml := "name: bad-stdio\nkind: global\ntransport: stdio-bridge\ncommand: echo\nclient_bindings: []\nweekly_refresh: false\n"
	warnings := a.ManifestValidate(yaml)
	found := false
	for _, w := range warnings {
		if w == "no daemons declared" {
			found = true
		}
	}
	if !found {
		t.Errorf("global stdio-bridge manifest with no daemons must still emit warning; got %v", warnings)
	}
}
func TestManifestCreateWritesYAML(t *testing.T) {
	tmp := t.TempDir()
	a := NewAPI()
	body := "name: newsrv\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9202\nclient_bindings: []\nweekly_refresh: false\n"
	if err := a.ManifestCreateIn(tmp, "newsrv", body); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tmp, "newsrv", "manifest.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "name: newsrv") {
		t.Error("manifest content not written")
	}
}

func TestManifestCreateRejectsYAMLNameMismatch(t *testing.T) {
	tmp := t.TempDir()
	a := NewAPI()
	body := "name: other\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9202\nclient_bindings: []\nweekly_refresh: false\n"
	err := a.ManifestCreateIn(tmp, "newsrv", body)
	if err == nil {
		t.Fatal("expected YAML name mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), `manifest yaml name "other" must match requested server "newsrv"`) {
		t.Fatalf("error = %v, want YAML/requested name mismatch", err)
	}
	if _, statErr := os.Stat(filepath.Join(tmp, "newsrv", "manifest.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("mismatched manifest was written, stat err = %v", statErr)
	}
}

func TestManifestDeleteRemovesDir(t *testing.T) {
	tmp := t.TempDir()
	_ = os.MkdirAll(filepath.Join(tmp, "doomed"), 0755)
	_ = os.WriteFile(filepath.Join(tmp, "doomed", "manifest.yaml"),
		[]byte("name: doomed\nkind: global\ntransport: stdio-bridge\ncommand: x\ndaemons:\n  - name: default\n    port: 9203\n"), 0644)

	a := NewAPI()
	if err := a.ManifestDeleteIn(tmp, "doomed"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "doomed")); !os.IsNotExist(err) {
		t.Error("manifest dir not removed")
	}
}

func TestManifestGetIn_ReturnsContentHash(t *testing.T) {
	dir := t.TempDir()
	a := &API{}
	name := "memory"
	// Must satisfy api.ManifestValidate (which ManifestCreateIn gates on):
	// requires kind, transport, command, and at least one daemon.
	yaml := "name: memory\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9210\n"
	if err := a.ManifestCreateIn(dir, name, yaml); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, hash, err := a.ManifestGetInWithHash(dir, name)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != yaml {
		t.Errorf("yaml = %q, want %q", got, yaml)
	}
	want := ManifestHashContent([]byte(yaml))
	if hash != want {
		t.Errorf("hash = %q, want %q", hash, want)
	}
}

func TestManifestGetIn_HashChangesOnExternalWrite(t *testing.T) {
	dir := t.TempDir()
	a := &API{}
	name := "demo"
	initial := "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9211\n"
	if err := a.ManifestCreateIn(dir, name, initial); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, h1, _ := a.ManifestGetInWithHash(dir, name)
	// External write — different bytes (port change) to simulate another
	// editor touching the file between Load and Save.
	path := filepath.Join(dir, name, "manifest.yaml")
	mutated := "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9212\n"
	if err := os.WriteFile(path, []byte(mutated), 0600); err != nil {
		t.Fatalf("external write: %v", err)
	}
	_, h2, _ := a.ManifestGetInWithHash(dir, name)
	if h1 == h2 {
		t.Errorf("hash unchanged after external write: %q", h1)
	}
}

// Test YAML invariant: all strings passed to ManifestCreateIn AND to
// ManifestEditInWithHash (with non-empty expectedHash matching, or empty
// expectedHash — any path that reaches ManifestValidate) must pass
// api.ManifestValidate, which requires name + kind + transport + command
// + at least one daemon. See Task 2 for the same validator gate.
func TestManifestEditIn_RejectsHashMismatch(t *testing.T) {
	dir := t.TempDir()
	a := &API{}
	name := "demo"
	if err := a.ManifestCreateIn(dir, name, "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9220\n"); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, hash, _ := a.ManifestGetInWithHash(dir, name)
	path := filepath.Join(dir, name, "manifest.yaml")
	// External write bypasses validate (direct os.WriteFile); needs to
	// differ from the original bytes so the hash check trips.
	if err := os.WriteFile(path, []byte("name: demo\nkind: workspace-scoped\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9220\n"), 0600); err != nil {
		t.Fatalf("external write: %v", err)
	}
	// Edit yaml never reaches ManifestValidate because the hash-check
	// short-circuits first; still kept well-formed for clarity.
	_, err := a.ManifestEditInWithHash(dir, name, "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9220\n", hash)
	if err == nil {
		t.Fatalf("expected hash-mismatch error, got nil")
	}
	if !errors.Is(err, ErrManifestHashMismatch) {
		t.Errorf("err = %v, want ErrManifestHashMismatch", err)
	}
}

func TestManifestEditIn_AcceptsMatchingHash_ReturnsNewHash(t *testing.T) {
	dir := t.TempDir()
	a := &API{}
	name := "demo"
	orig := "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9221\n"
	if err := a.ManifestCreateIn(dir, name, orig); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, hash, _ := a.ManifestGetInWithHash(dir, name)
	// Edit path reaches ManifestValidate — yaml must be well-formed.
	updated := "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9221\n"
	newHash, err := a.ManifestEditInWithHash(dir, name, updated, hash)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	wantHash := ManifestHashContent([]byte(updated))
	if newHash != wantHash {
		t.Errorf("returned hash = %q, want %q", newHash, wantHash)
	}
	got, diskHash, _ := a.ManifestGetInWithHash(dir, name)
	if got != updated {
		t.Errorf("yaml = %q, want %q", got, updated)
	}
	if diskHash != newHash {
		t.Errorf("disk hash = %q does not match returned newHash %q", diskHash, newHash)
	}
}

func TestManifestEditIn_RejectsYAMLNameMismatch(t *testing.T) {
	dir := t.TempDir()
	a := &API{}
	name := "demo"
	orig := "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9221\n"
	if err := a.ManifestCreateIn(dir, name, orig); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, hash, _ := a.ManifestGetInWithHash(dir, name)
	mismatched := "name: other\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9221\n"
	_, err := a.ManifestEditInWithHash(dir, name, mismatched, hash)
	if err == nil {
		t.Fatal("expected YAML name mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), `manifest yaml name "other" must match requested server "demo"`) {
		t.Fatalf("error = %v, want YAML/requested name mismatch", err)
	}
	got, _, _ := a.ManifestGetInWithHash(dir, name)
	if got != orig {
		t.Fatalf("target yaml changed on rejected mismatch: %q", got)
	}
}

func TestManifestEditIn_EmptyExpectedHash_SkipsCheck(t *testing.T) {
	dir := t.TempDir()
	a := &API{}
	name := "demo"
	orig := "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9222\n"
	if err := a.ManifestCreateIn(dir, name, orig); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Empty expectedHash skips the check but still runs ManifestValidate
	// on the new yaml — must remain well-formed.
	updated := "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9222\n"
	if _, err := a.ManifestEditInWithHash(dir, name, updated, ""); err != nil {
		t.Fatalf("empty-hash edit should succeed: %v", err)
	}
}

func TestManifestEditIn_AtomicWrite_TargetUnchangedOnFailure(t *testing.T) {
	dir := t.TempDir()
	a := &API{}
	name := "demo"
	orig := "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9223\n"
	if err := a.ManifestCreateIn(dir, name, orig); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Inject write failure between tmp-close and rename.
	ManifestSetFailWriteHook(func() bool { return true })
	defer ManifestSetFailWriteHook(nil)
	updated := "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9223\n"
	_, err := a.ManifestEditInWithHash(dir, name, updated, "")
	if err == nil {
		t.Fatalf("expected injected failure, got nil")
	}
	// Target content must be UNCHANGED.
	got, _, _ := a.ManifestGetInWithHash(dir, name)
	if got != orig {
		t.Errorf("target yaml changed on failure: %q, want %q", got, orig)
	}
	// No stale tmp file left.
	files, _ := os.ReadDir(filepath.Join(dir, name))
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".tmp") {
			t.Errorf("stale tmp file left: %q", f.Name())
		}
	}
}

// TestManifestCRUD_RejectsPathTraversalNames guards the regression
// where an attacker-controlled (or typo'd) name like "..",
// "../escaped", or an absolute path could escape the manifest root —
// for ManifestDeleteIn that would have meant os.RemoveAll on an
// arbitrary directory. The name validator rejects anything outside
// [a-z0-9][a-z0-9._-]*. It also rejects reserved Windows device names
// (CON/PRN/AUX/NUL/COMn/LPTn, case-insensitive, with-or-without
// extension) and trailing-dot/space aliases that Windows would rewrite.
func TestManifestCRUD_RejectsPathTraversalNames(t *testing.T) {
	a := NewAPI()
	tmp := t.TempDir()

	cases := []string{
		"..",
		"../escaped",
		"../../etc",
		"/abs/path",
		`\abs\path`,
		"name/with/slash",
		"name\\with\\bs",
		"CapitalLetters",
		".leading-dot",
		"-leading-dash",
		"",
		" space",
		"name with spaces",
		// Reserved Windows device names — bare. Lower-case is what
		// validManifestName allows; CON/Con etc. are already rejected
		// by the regex's charset, but the new rejector defends in depth
		// at the post-regex layer.
		"con",
		"prn",
		"aux",
		"nul",
		"com1",
		"com9",
		"lpt1",
		"lpt9",
		// Reserved-with-extension: Windows treats `nul.yaml` as the
		// nul device too (extension is immaterial for these names).
		"con.txt",
		"nul.yaml",
		"aux.json",
		"lpt1.json",
		"com1.dat",
		// Trailing-dot alias: `foo.` is a regex-valid name but Windows
		// rewrites it to `foo`, leading to ambiguous targets.
		"foo.",
	}

	for _, bad := range cases {
		if err := a.ManifestDeleteIn(tmp, bad); err == nil {
			t.Errorf("ManifestDeleteIn(_, %q): expected rejection, got nil", bad)
		}
		if err := a.ManifestCreateIn(tmp, bad, "name: x\nkind: global\ntransport: stdio-bridge\ncommand: x\ndaemons: [{name: default, port: 9999}]\n"); err == nil {
			t.Errorf("ManifestCreateIn(_, %q): expected rejection, got nil", bad)
		}
		if err := a.ManifestEditIn(tmp, bad, "name: x\nkind: global\ntransport: stdio-bridge\ncommand: x\n"); err == nil {
			t.Errorf("ManifestEditIn(_, %q): expected rejection, got nil", bad)
		}
		if _, err := a.ManifestGet(bad); err == nil {
			t.Errorf("ManifestGet(%q): expected rejection, got nil", bad)
		}
		// ManifestGetIn / ManifestGetInWithHash are reachable directly
		// (not just via ManifestGet's wrapper), so they MUST also reject
		// bad names. Otherwise a future caller bypassing the production
		// wrapper would raw-join name into the path and read whatever
		// ../escape resolves to.
		if _, err := a.ManifestGetIn(tmp, bad); err == nil {
			t.Errorf("ManifestGetIn(_, %q): expected rejection, got nil", bad)
		}
		if _, _, err := a.ManifestGetInWithHash(tmp, bad); err == nil {
			t.Errorf("ManifestGetInWithHash(_, %q): expected rejection, got nil", bad)
		}
	}
}

// TestCheckManifestName_AcceptsLegitNamesWithReservedPrefix asserts
// that the new Windows-reserved-name layer does NOT have a
// false-positive on legitimate names whose lower-case form merely
// STARTS with a reserved device name (e.g. `confidence` starts with
// `con`, `nullptr-helper` starts with `nul`). The rule is exact match
// of the part-before-first-dot, not a prefix.
func TestCheckManifestName_AcceptsLegitNamesWithReservedPrefix(t *testing.T) {
	cases := []string{
		"confidence",
		"console-bridge",
		"nullptr-helper",
		"auxiliary",
		"prndaemon",
		"com10", // not in [com0..com9]
		"lpt10",
		// The reserved-name rule is exact-match-on-base; a legit
		// name like `console.txt` (regex-valid) must also pass.
		"console.txt",
	}
	for _, n := range cases {
		if err := checkManifestName(n); err != nil {
			t.Errorf("checkManifestName(%q): expected ok, got %v", n, err)
		}
	}
}

// TestRejectWindowsReservedManifestName_TableDriven exercises the
// helper directly — covers the case-insensitive contract (the helper
// must work even when called with mixed-case input that bypassed the
// regex for some reason) and the trailing-space variant that the
// regex already rejects on the wrapper side.
func TestRejectWindowsReservedManifestName_TableDriven(t *testing.T) {
	bad := []string{
		"con", "CON", "Con",
		"prn", "aux", "nul",
		"com1", "COM9",
		"lpt1", "LPT9",
		"con.txt", "CON.TXT",
		"nul.yaml",
		"aux.json",
		"foo.",
		"foo ",
	}
	for _, n := range bad {
		if err := rejectWindowsReservedManifestName(n); err == nil {
			t.Errorf("rejectWindowsReservedManifestName(%q): expected rejection, got nil", n)
		}
	}
	good := []string{
		"",
		"foo",
		"confidence",
		"console-bridge",
		"nullptr",
		"com10",
		"lpt10",
		"my-server",
	}
	for _, n := range good {
		if err := rejectWindowsReservedManifestName(n); err != nil {
			t.Errorf("rejectWindowsReservedManifestName(%q): expected ok, got %v", n, err)
		}
	}
}

// TestManifestGetIn_RejectsPathTraversal asserts the entry-point guard
// in detail: error matches the same "manifest name" wording the rest
// of the *In family produces, so an attacker-controlled name returns
// the same error envelope regardless of the API surface used.
func TestManifestGetIn_RejectsPathTraversal(t *testing.T) {
	tmp := t.TempDir()
	a := NewAPI()

	bad := "../escape"
	_, err := a.ManifestGetIn(tmp, bad)
	if err == nil {
		t.Fatalf("ManifestGetIn(_, %q): expected rejection, got nil", bad)
	}
	if !strings.Contains(err.Error(), "manifest name") {
		t.Errorf("ManifestGetIn error = %v, want 'manifest name' wording", err)
	}

	_, _, err = a.ManifestGetInWithHash(tmp, bad)
	if err == nil {
		t.Fatalf("ManifestGetInWithHash(_, %q): expected rejection, got nil", bad)
	}
	if !strings.Contains(err.Error(), "manifest name") {
		t.Errorf("ManifestGetInWithHash error = %v, want 'manifest name' wording", err)
	}
}
