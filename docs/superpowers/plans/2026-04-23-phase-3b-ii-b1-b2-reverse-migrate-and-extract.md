# Phase 3B-II B1 + B2 — Reverse-migrate API + Extend ExtractManifestFromClient

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `api.Demigrate(DemigrateOpts)` for reverse-migrate (unblocks Servers matrix uncheck UI). Extend the existing `api.ExtractManifestFromClient` switch to support codex-cli / gemini-cli / antigravity (currently only claude-code) and reject Antigravity's mcphub-relay entries as "not user stdio" (unblocks A1 Migration screen's "Create manifest" action for entries configured via those three clients).

**Architecture:** Demigrate uses per-entry rollback driven by manifest `ClientBindings` (same iteration pattern as `MigrateFrom`). Each binding points at a specific client; only that client's most-recent backup is consulted, and only the named server entry is restored — unrelated entries in the same config file and entries in unrelated clients are untouched. Adds two new Client interface methods (`LatestBackupPath`, `RestoreEntryFromBackup`) shared across all JSON/TOML adapters. B2 reuses the existing `renderDraftManifestYAML` + `pickNextFreePort` helpers in `internal/api/scan.go`; the only change to `ExtractManifestFromClient` is adding 3 more switch cases plus two bug fixes (relay-reject for Antigravity, no-type-stdio acceptance for Claude).

**Tech Stack:** Go 1.26 only. No new dependencies. Existing `gopkg.in/yaml.v3` (for manifest roundtrip in tests) and `github.com/pelletier/go-toml/v2` (for Codex CLI config) already on the dep list.

---

## Codex CLI plan-review fixes absorbed

Before execution, the first draft of this plan went through `codex review` and was revised to address:

1. **[P1]** `api.ExtractManifestFromClient` already exists at `internal/api/scan.go:427` with signature `(client, serverName, opts ScanOpts) (string, error)`. Plan now **extends** the existing switch statement; does not introduce a new 2-arg method.
2. **[P1]** Draft-manifest YAML shape must match `config.ServerManifest` (top-level `kind`, `transport`, `command`, `base_args`, `env`; `DaemonSpec` only has `name`/`context`/`port`/`extra_args`). Plan now reuses the existing `renderDraftManifestYAML` helper (`internal/api/scan.go:509`) which already emits the correct shape.
3. **[P1]** Antigravity relay entries must not be returned as "user stdio". Plan now explicitly rejects entries where `command` is the mcphub binary + `args[0]=="relay"`.
4. **[P2]** Claude stdio entries don't always carry `type:"stdio"` (older configs omit the field). Plan accepts `type` absent or `type:"stdio"` with non-empty `command` and no `url`.
5. **[P2]** Demigrate iterates **manifest `ClientBindings`**, not every installed client. Unrelated clients with same-name entries are never touched.
6. **[P3]** Draft-manifest tests parse output with `config.ParseManifest` + `ManifestValidate`, not just substring-match.
7. **[P3]** Adapter rollback tests include absent-in-backup cases + "other entries untouched" assertions.
8. **[P3]** Timestamp format in backup names is `20060102-150405` (see `internal/clients/clients.go:153`). Plan uses that layout in fixtures.

### Codex CLI R2 fixes absorbed

R2 of `codex review` against the R1 rewrite surfaced four additional P1 findings. All addressed in this plan:

9. **[P1]** `LatestBackupPath()` is a no-arg method on the `Client` interface — `c.path` is already stored on each adapter. The R2 verify prompt erroneously listed a `(path string)` parameter; the no-arg form is the approved signature and is what every adapter implements.
10. **[P1]** Antigravity relay-reject in `ExtractManifestFromClient` now checks BOTH that `command` is the mcphub binary (via shared exported `clients.IsMcphubBinary` helper) AND that `args[0] == "relay"`. A user with a genuine stdio server whose first arg is literally `"relay"` (but whose `command` is something else like `/usr/local/bin/custom`) is accepted — a new test `TestExtractManifestFromClient_Antigravity_AcceptsRelayFirstArgWithNonMcphubCmd` locks in that behavior.
11. **[P1]** Multi-server rollback correctness — if servers A and B are migrated from the same client in order, the latest backup captures state *between* migrations, so it holds A in hub-HTTP form and B in stdio form. Demigrating A from that backup would silently re-write the hub-HTTP entry. Fix: each adapter's `RestoreEntryFromBackup` now defensively detects hub-HTTP/relay shape in the backup entry and returns `ErrBackupEntryAlreadyMigrated`; `api.Demigrate` propagates that error as a `Failed` row with a clear message telling the operator to demigrate newest-first or restore manually from the `-original` sentinel. New tests `TestDemigrate_MultiServerNewestFirstSucceeds` and `TestDemigrate_MultiServerRejectsOlderMigrationClearly` exercise both paths. The constraint is documented in `Demigrate`'s godoc.
12. **[P1]** Tasks 4 and 6 restructured to five steps each (previously six). Task 4 merges "write failing tests in `gemini_cli_test.go`" and "write failing tests in `antigravity_test.go`" into one step. Task 6 folds "read the current `ExtractManifestFromClient`" into the Task preamble instead of a numbered step.

---

## File structure

```
internal/clients/
├── clients.go                  # +latestBackup helper, +2 interface methods
├── claude_code.go              # +2 method impls
├── codex_cli.go                # +2 method impls
├── json_mcp.go                 # +2 method impls (Gemini + Antigravity inherit)
├── clients_test.go             # +TestLatestBackup*
├── claude_code_test.go         # +rollback + latest-backup tests
├── codex_cli_test.go           # +rollback + latest-backup tests
├── gemini_cli_test.go          # +rollback tests (inheritance)
└── antigravity_test.go         # +rollback tests (inheritance)

internal/api/
├── demigrate.go                # NEW: api.Demigrate()
├── demigrate_test.go           # NEW
├── scan.go                     # extend ExtractManifestFromClient switch (+3 cases, +2 bug fixes)
└── scan_extract_test.go        # +tests for new cases, Antigravity relay-reject, Claude no-type
```

Not touched: `internal/gui/*` — HTTP handlers that surface these APIs to the browser land with A1 Migration screen's plan.

---

## Task 1: Client interface additions + shared latestBackup helper

**Files:**
- Modify: `internal/clients/clients.go` (add 2 interface methods + `latestBackup` helper)
- Modify: `internal/clients/clients_test.go` (add `TestLatestBackup_*`)

### Step 1: Write the failing test in `internal/clients/clients_test.go`

Add at the end of the file (keep existing tests untouched):

```go
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
```

If `filepath`, `strings`, `os` are not already in the import block of `clients_test.go`, add them.

### Step 2: Run — confirm compile failure

```bash
go test ./internal/clients/ -run TestLatestBackup -count=1
```

Expected: `undefined: latestBackup`.

### Step 3: Add interface methods + helper to `internal/clients/clients.go`

Extend the `Client` interface — add these methods inside the `interface { ... }` block, after the existing methods (right after `GetEntry`):

```go
	// LatestBackupPath returns the absolute path to the most recent
	// mcp-local-hub backup of this client's config. Timestamped
	// backups (.bak-mcp-local-hub-<YYYYMMDD-HHMMSS>) take precedence
	// over the pristine -original sentinel. Returns (path, true, nil)
	// when a backup exists, ("", false, nil) when none do, (_, _, err)
	// on a filesystem error.
	LatestBackupPath() (string, bool, error)

	// RestoreEntryFromBackup reads the backup file at backupPath,
	// extracts the entry named `name`, and writes that raw pre-migrate
	// shape to the live config — overwriting any current entry with
	// the same name. If the backup does NOT contain the entry (i.e.
	// migrate added it from scratch and there was no prior entry),
	// removes the current entry. Returns an error if the backup file
	// cannot be opened or parsed. Idempotent if the live config is
	// already in the backup's shape. Other entries in the live config
	// are untouched.
	RestoreEntryFromBackup(backupPath, name string) error
```

Add the sentinel error near the existing `ErrClientNotInstalled` declaration:

```go
// ErrBackupEntryAlreadyMigrated is returned by RestoreEntryFromBackup
// when the backup file's copy of the named entry is already in
// hub-HTTP form (for JSON/TOML clients) or hub-relay form (for
// Antigravity). This happens when a backup was taken AFTER an earlier
// migrate of the same client had already rewritten the entry —
// typically the "newest" backup when multiple servers are migrated
// sequentially from the same client. Restoring from such a backup
// would silently re-write the hub-managed form, defeating demigrate.
// Callers (Demigrate) must surface this as a Failed row and instruct
// the operator to demigrate newest-first or restore manually from
// the `-original` sentinel.
var ErrBackupEntryAlreadyMigrated = errors.New("clients: backup copy of entry is already in hub-managed shape")

// IsMcphubBinary reports whether cmd's basename matches the mcphub
// executable name. Case-insensitive to cover Windows (mcphub.exe) and
// POSIX (mcphub). Used by both internal/api/scan.go's Antigravity
// relay-reject branch and the per-adapter RestoreEntryFromBackup
// hub-relay detection to avoid false positives against user stdio
// entries whose first argument happens to be the literal string
// "relay". Exported so internal/api/scan.go (package api) can call it
// via clients.IsMcphubBinary; within package clients it is called as
// the unqualified IsMcphubBinary.
func IsMcphubBinary(cmd string) bool {
	if cmd == "" {
		return false
	}
	base := strings.ToLower(filepath.Base(cmd))
	return base == "mcphub" || base == "mcphub.exe"
}
```

Add `"errors"` to the imports of `clients.go` if not already present (it already imports `os`, `path/filepath`, `strings`; `errors` is the new one).

Add the backup-discovery helper at the end of `clients.go`:

```go
// latestBackup returns the most recent mcp-local-hub backup path for
// livePath. Timestamped copies (livePath + ".bak-mcp-local-hub-<ts>")
// take precedence over the pristine "-original" sentinel; within
// timestamped copies the lexicographically-largest name wins (timestamps
// use the 20060102-150405 layout, which sorts correctly as a string).
// Directories with matching names are ignored. Returns ("", false, nil)
// when no backup files are present and (_, _, err) on filesystem error.
// The second parameter (clientName) is currently unused but reserved for
// future per-client log/diagnostic context.
func latestBackup(livePath, _ string) (string, bool, error) {
	dir := filepath.Dir(livePath)
	prefix := filepath.Base(livePath) + ".bak-mcp-local-hub-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	var timestamped []string
	var original string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(name, prefix)
		if suffix == "original" {
			original = filepath.Join(dir, name)
			continue
		}
		timestamped = append(timestamped, filepath.Join(dir, name))
	}
	if len(timestamped) > 0 {
		sort.Strings(timestamped)
		return timestamped[len(timestamped)-1], true, nil
	}
	if original != "" {
		return original, true, nil
	}
	return "", false, nil
}
```

Add `"sort"` + `"strings"` to `clients.go` imports if missing.

### Step 4: Run helper tests — they pass; adapter compliance is now broken

```bash
go test ./internal/clients/ -run TestLatestBackup -count=1
```

Expected: 4 new tests PASS.

```bash
go build ./...
```

Expected: **compile fails** in `internal/clients/*_test.go` and anywhere `AllClients()` is consumed — adapters don't yet implement the 2 new interface methods. Intended TDD red state; Tasks 2-4 repair it.

### Step 5: Commit

```bash
git add internal/clients/clients.go internal/clients/clients_test.go
git commit -m "feat(clients): add LatestBackupPath + RestoreEntryFromBackup interface methods

Plus shared latestBackup helper that returns the most recent backup
(timestamped win over -original sentinel, directories ignored).
Adapters do not yet implement the new methods — go build is broken
until Tasks 2-4 add each adapter's methods."
```

---

## Task 2: Claude Code adapter — LatestBackupPath + RestoreEntryFromBackup

**Files:**
- Modify: `internal/clients/claude_code.go` (+2 methods)
- Modify: `internal/clients/claude_code_test.go` (+4 tests)

### Step 1: Write failing tests

Append to `claude_code_test.go`:

```go
func TestClaudeCode_LatestBackupPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := &claudeCode{path: path}
	got, ok, err := c.LatestBackupPath()
	if err != nil || !ok || got != backup {
		t.Errorf("LatestBackupPath = %q ok=%v err=%v", got, ok, err)
	}
}

func TestClaudeCode_RestoreEntryFromBackup_RestoresStdioShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	// Live config is in post-migrate hub-HTTP state.
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9123/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	// Backup has pre-migrate stdio.
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"type":"stdio","command":"npx","args":["-y","mem"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	c := &claudeCode{path: path}
	if err := c.RestoreEntryFromBackup(backup, "memory"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	live, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(live, &m); err != nil {
		t.Fatal(err)
	}
	entry := m["mcpServers"].(map[string]any)["memory"].(map[string]any)
	if entry["type"] != "stdio" {
		t.Errorf("type=%v, want stdio", entry["type"])
	}
	if entry["command"] != "npx" {
		t.Errorf("command=%v, want npx", entry["command"])
	}
}

func TestClaudeCode_RestoreEntryFromBackup_RemovesEntryIfBackupLacksIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	// Live has two entries; only `memory` exists in backup.
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"newserver":{"type":"http","url":"x"},"memory":{"type":"http","url":"y"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"type":"stdio","command":"npx","args":["-y","mem"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	c := &claudeCode{path: path}
	if err := c.RestoreEntryFromBackup(backup, "newserver"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	live, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(live, &m); err != nil {
		t.Fatal(err)
	}
	servers := m["mcpServers"].(map[string]any)
	if _, present := servers["newserver"]; present {
		t.Error("newserver should have been removed — backup predates it")
	}
	// memory must survive because the call targeted only `newserver`.
	if _, present := servers["memory"]; !present {
		t.Error("memory was touched but should be untouched — call targeted newserver")
	}
}

func TestClaudeCode_RestoreEntryFromBackup_PreservesUnrelatedEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	// Live has `memory` (migrated) + `other` (user added since migrate).
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"y"},"other":{"type":"stdio","command":"echo"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	// Backup predates `other`, has stdio `memory`.
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"type":"stdio","command":"npx","args":["-y","mem"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	c := &claudeCode{path: path}
	if err := c.RestoreEntryFromBackup(backup, "memory"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	live, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(live, &m); err != nil {
		t.Fatal(err)
	}
	servers := m["mcpServers"].(map[string]any)
	// memory is rolled back to stdio.
	memEntry := servers["memory"].(map[string]any)
	if memEntry["type"] != "stdio" {
		t.Errorf("memory.type=%v, want stdio", memEntry["type"])
	}
	// `other` entry (added after migrate, not in backup) is preserved.
	if _, present := servers["other"]; !present {
		t.Error("unrelated 'other' entry lost — per-entry rollback must preserve it")
	}
}

func TestClaudeCode_RestoreEntryFromBackup_RefusesHubHTTPBackupEntry(t *testing.T) {
	// Backup was taken AFTER an earlier migrate already rewrote this
	// entry to hub-HTTP form (typical when two servers are migrated
	// sequentially from the same client). Restoring from this backup
	// would silently re-write the hub-HTTP entry. Defensive refuse.
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	// Backup has memory ALREADY migrated — this is the bug we must catch.
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	c := &claudeCode{path: path}
	err := c.RestoreEntryFromBackup(backup, "memory")
	if !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
}
```

If `json` is not already in imports, add `"encoding/json"`; also add `"errors"` for the new test.

### Step 2: Run — confirm red

```bash
go test ./internal/clients/ -run "TestClaudeCode_LatestBackupPath|TestClaudeCode_RestoreEntryFromBackup" -count=1
```

Expected: `c.LatestBackupPath undefined` / `c.RestoreEntryFromBackup undefined`.

### Step 3: Implement in `internal/clients/claude_code.go`

Append at the end of the file:

```go
// LatestBackupPath delegates to the shared helper.
func (c *claudeCode) LatestBackupPath() (string, bool, error) {
	return latestBackup(c.path, c.Name())
}

// RestoreEntryFromBackup reads the raw per-name entry from the backup
// at backupPath and writes it (or removes the current live entry, if
// the backup had none) into the live config. Other entries in the
// live config are untouched.
//
// Defensively refuses if the backup's copy of the named entry is
// already in hub-HTTP form (has a `url` field). That situation arises
// when the backup was taken AFTER an earlier migrate of the same
// client already rewrote this entry — restoring would silently
// re-apply hub-HTTP data. See ErrBackupEntryAlreadyMigrated.
func (c *claudeCode) RestoreEntryFromBackup(backupPath, name string) error {
	backupData, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	var backupMap map[string]any
	if len(backupData) == 0 {
		backupMap = map[string]any{}
	} else if err := json.Unmarshal(backupData, &backupMap); err != nil {
		return fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	backupServers, _ := backupMap["mcpServers"].(map[string]any)
	liveMap, err := c.readJSON()
	if err != nil {
		return err
	}
	liveServers, _ := liveMap["mcpServers"].(map[string]any)
	if liveServers == nil {
		liveServers = map[string]any{}
	}
	if backupServers != nil {
		if backupEntry, present := backupServers[name]; present {
			// Defensive: refuse hub-HTTP-shaped backup entries. The
			// canonical hub-HTTP shape in .claude.json has a `url`
			// field and no `command` field.
			if rawMap, ok := backupEntry.(map[string]any); ok {
				if _, hasURL := rawMap["url"]; hasURL {
					if _, hasCmd := rawMap["command"]; !hasCmd {
						return ErrBackupEntryAlreadyMigrated
					}
				}
			}
			liveServers[name] = backupEntry
			liveMap["mcpServers"] = liveServers
			return c.writeJSON(liveMap)
		}
	}
	delete(liveServers, name)
	liveMap["mcpServers"] = liveServers
	return c.writeJSON(liveMap)
}
```

### Step 4: Run — green

```bash
go test ./internal/clients/ -run TestClaudeCode_ -count=1
```

Expected: 4 new Claude tests PASS. Existing `TestClaudeCode_*` tests still PASS.

### Step 5: Commit

```bash
git add internal/clients/claude_code.go internal/clients/claude_code_test.go
git commit -m "feat(clients/claude-code): LatestBackupPath + RestoreEntryFromBackup"
```

---

## Task 3: Codex CLI adapter — LatestBackupPath + RestoreEntryFromBackup

**Files:**
- Modify: `internal/clients/codex_cli.go` (+2 methods)
- Modify: `internal/clients/codex_cli_test.go` (+3 tests)

### Step 1: Write failing tests

Append to `codex_cli_test.go`:

```go
func TestCodexCLI_LatestBackupPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(``), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(``), 0600); err != nil {
		t.Fatal(err)
	}
	c := &codexCLI{path: path}
	got, ok, err := c.LatestBackupPath()
	if err != nil || !ok || got != backup {
		t.Errorf("LatestBackupPath = %q ok=%v err=%v", got, ok, err)
	}
}

func TestCodexCLI_RestoreEntryFromBackup_RestoresStdio(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	live := `[mcp_servers.memory]
url = "http://localhost:9123/mcp"
startup_timeout_sec = 10.0
`
	if err := os.WriteFile(path, []byte(live), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	backupBody := `[mcp_servers.memory]
command = "npx"
args = ["-y", "mem"]
`
	if err := os.WriteFile(backup, []byte(backupBody), 0600); err != nil {
		t.Fatal(err)
	}
	c := &codexCLI{path: path}
	if err := c.RestoreEntryFromBackup(backup, "memory"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `command = "npx"`) {
		t.Errorf("expected restored stdio command, got:\n%s", string(data))
	}
	if strings.Contains(string(data), `url = "http`) {
		t.Errorf("hub-HTTP url should be gone after restore, got:\n%s", string(data))
	}
}

func TestCodexCLI_RestoreEntryFromBackup_RemovesOnAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	live := `[mcp_servers.newserver]
url = "http://localhost:9999/mcp"
`
	if err := os.WriteFile(path, []byte(live), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(``), 0600); err != nil {
		t.Fatal(err)
	}
	c := &codexCLI{path: path}
	if err := c.RestoreEntryFromBackup(backup, "newserver"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "newserver") {
		t.Errorf("newserver should have been removed, got:\n%s", string(data))
	}
}

func TestCodexCLI_RestoreEntryFromBackup_RefusesHubHTTPBackupEntry(t *testing.T) {
	// Backup was taken AFTER an earlier migrate already rewrote this
	// entry to hub-HTTP form. Defensive refuse.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(`[mcp_servers.memory]
url = "http://localhost:9200/mcp"
startup_timeout_sec = 10.0
`), 0600); err != nil {
		t.Fatal(err)
	}
	// Backup has the entry already migrated.
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`[mcp_servers.memory]
url = "http://localhost:9200/mcp"
startup_timeout_sec = 10.0
`), 0600); err != nil {
		t.Fatal(err)
	}
	c := &codexCLI{path: path}
	err := c.RestoreEntryFromBackup(backup, "memory")
	if !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
}
```

Add `"errors"` to imports if missing.

### Step 2: Run — red

```bash
go test ./internal/clients/ -run TestCodexCLI_ -count=1
```

Expected: undefined `c.LatestBackupPath` / `c.RestoreEntryFromBackup`.

### Step 3: Implement in `internal/clients/codex_cli.go`

Append at the end of the file:

```go
// LatestBackupPath delegates to the shared helper.
func (c *codexCLI) LatestBackupPath() (string, bool, error) {
	return latestBackup(c.path, c.Name())
}

// RestoreEntryFromBackup reads the TOML backup, extracts the
// [mcp_servers.<name>] table (if present), and writes it over the live
// config's corresponding entry. Other [mcp_servers.*] tables are left
// untouched.
//
// Defensively refuses if the backup's copy of the named entry is
// already in hub-HTTP form (has a `url` key and no `command` key) —
// see ErrBackupEntryAlreadyMigrated.
func (c *codexCLI) RestoreEntryFromBackup(backupPath, name string) error {
	backupData, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	var backupMap map[string]any
	if len(backupData) > 0 {
		if err := toml.Unmarshal(backupData, &backupMap); err != nil {
			return fmt.Errorf("parse backup %s: %w", backupPath, err)
		}
	}
	if backupMap == nil {
		backupMap = map[string]any{}
	}
	backupServers, _ := backupMap["mcp_servers"].(map[string]any)
	liveMap, err := c.readTOML()
	if err != nil {
		return err
	}
	liveServers, _ := liveMap["mcp_servers"].(map[string]any)
	if liveServers == nil {
		liveServers = map[string]any{}
	}
	if backupServers != nil {
		if backupEntry, present := backupServers[name]; present {
			// Defensive: refuse hub-HTTP-shaped backup entries for
			// Codex CLI (has `url`, no `command`).
			if rawMap, ok := backupEntry.(map[string]any); ok {
				if _, hasURL := rawMap["url"]; hasURL {
					if _, hasCmd := rawMap["command"]; !hasCmd {
						return ErrBackupEntryAlreadyMigrated
					}
				}
			}
			liveServers[name] = backupEntry
			liveMap["mcp_servers"] = liveServers
			return c.writeTOML(liveMap)
		}
	}
	delete(liveServers, name)
	liveMap["mcp_servers"] = liveServers
	return c.writeTOML(liveMap)
}
```

### Step 4: Green

```bash
go test ./internal/clients/ -run TestCodexCLI_ -count=1
```

Expected: 3 new + all existing TestCodexCLI_ tests PASS.

### Step 5: Commit

```bash
git add internal/clients/codex_cli.go internal/clients/codex_cli_test.go
git commit -m "feat(clients/codex-cli): LatestBackupPath + RestoreEntryFromBackup"
```

---

## Task 4: jsonMCPClient base — shared for Gemini CLI + Antigravity

Both Gemini CLI and Antigravity embed `*jsonMCPClient`. Implementing the 2 new methods on the base struct exposes them uniformly via Go's method promotion; no per-embed overrides.

**Files:**
- Modify: `internal/clients/json_mcp.go` (+2 methods)
- Modify: `internal/clients/gemini_cli_test.go` (+2 tests)
- Modify: `internal/clients/antigravity_test.go` (+2 tests)

### Step 1: Write failing tests in BOTH `gemini_cli_test.go` AND `antigravity_test.go`

Both Gemini CLI and Antigravity embed `*jsonMCPClient`, so the new methods land once on the base and both adapters inherit them via Go's method promotion. The tests go into each adapter's existing `_test.go` to document the inherited behavior from the caller's perspective.

Append to `gemini_cli_test.go`:

```go
func TestGeminiCLI_RestoreEntryFromBackup_RestoresStdio(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	// Live is post-migrate hub-HTTP.
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"memory":{"url":"http://localhost:9001/mcp","type":"http","timeout":10000}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"command":"npx","args":["-y","mem"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	g := &geminiCLI{jsonMCPClient: &jsonMCPClient{path: path, clientName: "gemini-cli", urlField: "url"}}
	if err := g.RestoreEntryFromBackup(backup, "memory"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	live, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(live, &m); err != nil {
		t.Fatal(err)
	}
	entry := m["mcpServers"].(map[string]any)["memory"].(map[string]any)
	if entry["command"] != "npx" {
		t.Errorf("command=%v, want npx", entry["command"])
	}
	if _, hasURL := entry["url"]; hasURL {
		t.Errorf("hub-http url should be gone, got %v", entry)
	}
}

func TestGeminiCLI_RestoreEntryFromBackup_RemovesOnAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"newserver":{"url":"x","type":"http"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	g := &geminiCLI{jsonMCPClient: &jsonMCPClient{path: path, clientName: "gemini-cli", urlField: "url"}}
	if err := g.RestoreEntryFromBackup(backup, "newserver"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	live, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(live, &m); err != nil {
		t.Fatal(err)
	}
	servers := m["mcpServers"].(map[string]any)
	if _, present := servers["newserver"]; present {
		t.Error("newserver should have been removed")
	}
}

func TestGeminiCLI_RestoreEntryFromBackup_RefusesHubHTTPBackupEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"memory":{"url":"http://localhost:9200/mcp","type":"http"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	// Backup has memory ALREADY in hub-HTTP form.
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"url":"http://localhost:9200/mcp","type":"http"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	g := &geminiCLI{jsonMCPClient: &jsonMCPClient{path: path, clientName: "gemini-cli", urlField: "url"}}
	err := g.RestoreEntryFromBackup(backup, "memory")
	if !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
}
```

Append to `antigravity_test.go`:

```go
func TestAntigravity_RestoreEntryFromBackup_RestoresOrRemovesPerBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp_config.json")
	// Live has a relay-stdio entry written by mcphub migrate.
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"serena":{"command":"C:/mcphub.exe","args":["relay","--server","serena","--daemon","claude"],"disabled":false}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	// Backup predates the install — no serena entry.
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	a := &antigravityClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "antigravity", urlField: "command"}}
	if err := a.RestoreEntryFromBackup(backup, "serena"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	live, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(live, &m); err != nil {
		t.Fatal(err)
	}
	servers := m["mcpServers"].(map[string]any)
	if _, present := servers["serena"]; present {
		t.Error("serena should have been removed")
	}
}

func TestAntigravity_LatestBackupPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp_config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	a := &antigravityClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "antigravity", urlField: "command"}}
	got, ok, err := a.LatestBackupPath()
	if err != nil || !ok || got != backup {
		t.Errorf("LatestBackupPath = %q ok=%v err=%v", got, ok, err)
	}
}

func TestAntigravity_RestoreEntryFromBackup_RefusesHubRelayBackupEntry(t *testing.T) {
	// Antigravity's hub-managed form is a RELAY entry: command points
	// at the mcphub binary and args[0] == "relay". Refuse restoring
	// from a backup that already contains a relay-shaped entry.
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp_config.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"serena":{"command":"C:/mcphub.exe","args":["relay","--server","serena","--daemon","claude"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"serena":{"command":"C:/mcphub.exe","args":["relay","--server","serena","--daemon","claude"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	a := &antigravityClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "antigravity", urlField: "command"}}
	err := a.RestoreEntryFromBackup(backup, "serena")
	if !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
}
```

Add `"errors"` to imports of both `gemini_cli_test.go` and `antigravity_test.go` if missing.

### Step 2: Run — red

```bash
go test ./internal/clients/ -run "TestGeminiCLI_Restore|TestAntigravity_Restore|TestGeminiCLI_LatestBackupPath|TestAntigravity_LatestBackupPath" -count=1
```

Expected: compile error — methods not yet on `jsonMCPClient`.

### Step 3: Implement in `internal/clients/json_mcp.go`

Append at the end of the file:

```go
// LatestBackupPath delegates to the shared helper.
func (j *jsonMCPClient) LatestBackupPath() (string, bool, error) {
	return latestBackup(j.path, j.clientName)
}

// RestoreEntryFromBackup reads the JSON backup, extracts mcpServers[name]
// (if present), and writes it (or removes the current live entry) to
// the live config. Other entries in mcpServers are untouched.
// Inherited by geminiCLI and antigravityClient via struct embedding.
//
// Defensively refuses if the backup's copy of the named entry is
// already in hub-managed shape. Shape detection is adapter-specific:
//   - For Gemini CLI (urlField = "url"): entry has `url` and no `command`.
//   - For Antigravity (urlField = "command"): entry's `command` is the
//     mcphub binary AND args[0] == "relay".
// Both paths return ErrBackupEntryAlreadyMigrated so Demigrate can
// surface a clear operator-facing failure row.
func (j *jsonMCPClient) RestoreEntryFromBackup(backupPath, name string) error {
	backupData, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	var backupMap map[string]any
	if len(backupData) == 0 {
		backupMap = map[string]any{}
	} else if err := json.Unmarshal(backupData, &backupMap); err != nil {
		return fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	backupServers, _ := backupMap["mcpServers"].(map[string]any)
	liveMap, err := j.readJSON()
	if err != nil {
		return err
	}
	liveServers, _ := liveMap["mcpServers"].(map[string]any)
	if liveServers == nil {
		liveServers = map[string]any{}
	}
	if backupServers != nil {
		if backupEntry, present := backupServers[name]; present {
			if rawMap, ok := backupEntry.(map[string]any); ok {
				if j.urlField == "url" {
					// Gemini CLI hub-HTTP shape: `url` present, `command` absent.
					if _, hasURL := rawMap["url"]; hasURL {
						if _, hasCmd := rawMap["command"]; !hasCmd {
							return ErrBackupEntryAlreadyMigrated
						}
					}
				} else {
					// Antigravity hub-relay shape: command is mcphub,
					// args[0] == "relay".
					if cmd, _ := rawMap["command"].(string); IsMcphubBinary(cmd) {
						if args, ok := rawMap["args"].([]any); ok && len(args) > 0 {
							if first, _ := args[0].(string); first == "relay" {
								return ErrBackupEntryAlreadyMigrated
							}
						}
					}
				}
			}
			liveServers[name] = backupEntry
			liveMap["mcpServers"] = liveServers
			return j.writeJSON(liveMap)
		}
	}
	delete(liveServers, name)
	liveMap["mcpServers"] = liveServers
	return j.writeJSON(liveMap)
}
```

### Step 4: Full package green + build

```bash
go test ./internal/clients/ -count=1
go build ./...
```

Expected: all client tests PASS, `go build ./...` clean. All 4 adapters now satisfy the extended interface.

### Step 5: Commit

```bash
git add internal/clients/json_mcp.go internal/clients/gemini_cli_test.go internal/clients/antigravity_test.go
git commit -m "feat(clients/json-mcp): LatestBackupPath + RestoreEntryFromBackup

Gemini CLI + Antigravity inherit both methods via struct embedding.
Defensive hub-managed-shape check (HTTP url for Gemini, mcphub+relay
for Antigravity) returns ErrBackupEntryAlreadyMigrated so Demigrate
can surface a clear failure on multi-server rollback out of order.
All 4 adapters now satisfy the extended Client interface."
```

---

## Task 5: `api.Demigrate` with manifest-driven bindings + tests

**Files:**
- Create: `internal/api/demigrate.go`
- Create: `internal/api/demigrate_test.go`

### Step 1: Write failing tests

```go
package api

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
)

// setupTmpHomeAndClaude redirects UserHomeDir to tmp and seeds
// .claude.json with the given body. Returns the claude config path.
func setupTmpHomeAndClaude(t *testing.T, body string) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	claude := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(claude, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return claude
}

// TestDemigrate_RestoresStdioPerEntry round-trips a claude-code stdio
// entry through migrate → demigrate using a real manifest (so the
// client-bindings iteration is exercised, not the naive
// "iterate every installed adapter" pattern).
func TestDemigrate_RestoresStdioPerEntry(t *testing.T) {
	claudePath := setupTmpHomeAndClaude(t,
		`{"mcpServers":{"memory":{"type":"stdio","command":"npx","args":["-y","mem"]}}}`)

	// Stage a manifest dir with one `memory` manifest pointing at
	// claude-code. Demigrate uses this dir to find ClientBindings.
	manifestDir := t.TempDir()
	memDir := filepath.Join(manifestDir, "memory")
	if err := os.MkdirAll(memDir, 0700); err != nil {
		t.Fatal(err)
	}
	manifestBody := `name: memory
kind: global
transport: stdio-bridge
command: npx
base_args:
  - "-y"
  - "mem"

daemons:
  - name: default
    port: 9200

client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`
	if err := os.WriteFile(filepath.Join(memDir, "manifest.yaml"), []byte(manifestBody), 0600); err != nil {
		t.Fatal(err)
	}

	// Take a backup of the pristine stdio state, THEN simulate migrate
	// by rewriting live to hub-HTTP.
	cc, _ := clients.NewClaudeCode()
	if _, err := cc.Backup(); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if err := os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}

	// Demigrate.
	a := NewAPI()
	report, err := a.Demigrate(DemigrateOpts{
		Servers:  []string{"memory"},
		ScanOpts: ScanOpts{ManifestDir: manifestDir},
		Writer:   io.Discard,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}
	if len(report.Failed) > 0 {
		t.Fatalf("unexpected failures: %+v", report.Failed)
	}
	if len(report.Restored) != 1 {
		t.Fatalf("expected 1 restored row, got %d", len(report.Restored))
	}

	// Live config is stdio again.
	live, _ := os.ReadFile(claudePath)
	var m map[string]any
	if err := json.Unmarshal(live, &m); err != nil {
		t.Fatal(err)
	}
	entry := m["mcpServers"].(map[string]any)["memory"].(map[string]any)
	if entry["type"] != "stdio" {
		t.Errorf("type=%v, want stdio", entry["type"])
	}
}

// TestDemigrate_OnlyIteratesManifestBindings verifies that a binding
// targeting claude-code alone does NOT touch gemini-cli's config, even
// if gemini-cli happens to have an entry with the same name. This is
// the per-manifest-binding scope test the review flagged.
func TestDemigrate_OnlyIteratesManifestBindings(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)

	claudePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://x/mcp"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	geminiDir := filepath.Join(tmp, ".gemini")
	if err := os.MkdirAll(geminiDir, 0700); err != nil {
		t.Fatal(err)
	}
	// Gemini has a same-name entry. Demigrate MUST NOT touch it when
	// the manifest only binds claude-code.
	geminiPath := filepath.Join(geminiDir, "settings.json")
	geminiBefore := `{"mcpServers":{"memory":{"url":"http://x/mcp","type":"http","timeout":10000}}}`
	if err := os.WriteFile(geminiPath, []byte(geminiBefore), 0600); err != nil {
		t.Fatal(err)
	}

	// Seed Claude's backup so the demigrate has somewhere to restore from.
	ccBackup := claudePath + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(ccBackup, []byte(
		`{"mcpServers":{"memory":{"type":"stdio","command":"npx","args":["-y","mem"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}

	// Manifest bindings only mention claude-code.
	manifestDir := t.TempDir()
	memDir := filepath.Join(manifestDir, "memory")
	_ = os.MkdirAll(memDir, 0700)
	if err := os.WriteFile(filepath.Join(memDir, "manifest.yaml"), []byte(
		`name: memory
kind: global
transport: stdio-bridge
command: npx
base_args: ["-y","mem"]
daemons:
  - name: default
    port: 9200
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`), 0600); err != nil {
		t.Fatal(err)
	}

	a := NewAPI()
	_, err := a.Demigrate(DemigrateOpts{
		Servers:  []string{"memory"},
		ScanOpts: ScanOpts{ManifestDir: manifestDir},
		Writer:   io.Discard,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}

	// Gemini config must be byte-identical to before Demigrate.
	geminiAfter, _ := os.ReadFile(geminiPath)
	if string(geminiAfter) != geminiBefore {
		t.Errorf("gemini config was touched — manifest bindings only mention claude-code.\nbefore: %s\nafter:  %s",
			geminiBefore, string(geminiAfter))
	}
}

// TestDemigrate_ClientsIncludeFilter narrows the binding set.
func TestDemigrate_ClientsIncludeFilter(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	claudePath := filepath.Join(tmp, ".claude.json")
	// Live has a hub-HTTP entry that should be rolled back.
	_ = os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://x/mcp"}}}`), 0600)
	// Claude backup.
	_ = os.WriteFile(claudePath+".bak-mcp-local-hub-20260101-000000", []byte(
		`{"mcpServers":{"memory":{"type":"stdio","command":"npx"}}}`), 0600)

	manifestDir := t.TempDir()
	memDir := filepath.Join(manifestDir, "memory")
	_ = os.MkdirAll(memDir, 0700)
	// Manifest binds BOTH claude-code and gemini-cli.
	_ = os.WriteFile(filepath.Join(memDir, "manifest.yaml"), []byte(
		`name: memory
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: default
    port: 9200
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
  - client: gemini-cli
    daemon: default
    url_path: /mcp
`), 0600)

	a := NewAPI()
	report, err := a.Demigrate(DemigrateOpts{
		Servers:        []string{"memory"},
		ClientsInclude: []string{"claude-code"}, // narrow to claude-code only
		ScanOpts:       ScanOpts{ManifestDir: manifestDir},
		Writer:         io.Discard,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}
	// Exactly one restored row for claude-code. gemini-cli was in the
	// manifest binding set but was filtered out by ClientsInclude.
	if len(report.Restored) != 1 || report.Restored[0].Client != "claude-code" {
		t.Errorf("expected single claude-code restore, got %+v", report.Restored)
	}
}

// TestDemigrate_MultiServerNewestFirstSucceeds mirrors the typical
// operator workflow: the backup captured right before the LAST
// migration has the last-migrated server in stdio form, so demigrating
// it from the latest backup succeeds.
func TestDemigrate_MultiServerNewestFirstSucceeds(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	claudePath := filepath.Join(tmp, ".claude.json")
	// Simulate the final live state after migrating A then B.
	if err := os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://x/mcp"},"fs":{"type":"http","url":"http://y/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	// Latest backup captures state AFTER memory migrate, BEFORE fs migrate.
	// fs is still in stdio form here — demigrating fs must succeed.
	latest := claudePath + ".bak-mcp-local-hub-20260201-120000"
	if err := os.WriteFile(latest, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://x/mcp"},"fs":{"type":"stdio","command":"npx","args":["-y","fs"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}

	manifestDir := t.TempDir()
	fsDir := filepath.Join(manifestDir, "fs")
	_ = os.MkdirAll(fsDir, 0700)
	_ = os.WriteFile(filepath.Join(fsDir, "manifest.yaml"), []byte(
		`name: fs
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: default
    port: 9201
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`), 0600)

	a := NewAPI()
	report, err := a.Demigrate(DemigrateOpts{
		Servers:  []string{"fs"},
		ScanOpts: ScanOpts{ManifestDir: manifestDir},
		Writer:   io.Discard,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}
	if len(report.Failed) > 0 {
		t.Fatalf("unexpected failures: %+v", report.Failed)
	}
	if len(report.Restored) != 1 {
		t.Fatalf("expected 1 restored row, got %d", len(report.Restored))
	}
}

// TestDemigrate_MultiServerRejectsOlderMigrationClearly covers the
// failure mode codex flagged: the latest backup has the target
// server ALREADY in hub-HTTP form because it was migrated FIRST,
// before a later server's migrate took a fresh backup. The adapter
// must refuse (ErrBackupEntryAlreadyMigrated) and Demigrate must
// surface that as a clear Failed row instead of silently succeeding.
func TestDemigrate_MultiServerRejectsOlderMigrationClearly(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	claudePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://x/mcp"},"fs":{"type":"http","url":"http://y/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	// Latest backup = pre-fs-migrate, so memory is ALREADY in hub-HTTP form here.
	// Trying to demigrate memory from this backup would re-write hub-HTTP
	// silently — the adapter must return ErrBackupEntryAlreadyMigrated.
	latest := claudePath + ".bak-mcp-local-hub-20260201-120000"
	if err := os.WriteFile(latest, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://x/mcp"},"fs":{"type":"stdio","command":"npx"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}

	manifestDir := t.TempDir()
	memDir := filepath.Join(manifestDir, "memory")
	_ = os.MkdirAll(memDir, 0700)
	_ = os.WriteFile(filepath.Join(memDir, "manifest.yaml"), []byte(
		`name: memory
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: default
    port: 9200
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`), 0600)

	a := NewAPI()
	report, err := a.Demigrate(DemigrateOpts{
		Servers:  []string{"memory"},
		ScanOpts: ScanOpts{ManifestDir: manifestDir},
		Writer:   io.Discard,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}
	if len(report.Restored) != 0 {
		t.Fatalf("expected 0 restored (backup holds already-migrated entry), got %+v", report.Restored)
	}
	if len(report.Failed) != 1 {
		t.Fatalf("expected 1 failure, got %d: %+v", len(report.Failed), report.Failed)
	}
	if !strings.Contains(strings.ToLower(report.Failed[0].Err), "already") &&
		!strings.Contains(strings.ToLower(report.Failed[0].Err), "newest-first") {
		t.Errorf("failure message should mention already-migrated/newest-first: got %q", report.Failed[0].Err)
	}
}

// TestDemigrate_NoBackupReportsFailure asserts the report shape when
// a binding exists but the client has no mcp-local-hub backup.
func TestDemigrate_NoBackupReportsFailure(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	claudePath := filepath.Join(tmp, ".claude.json")
	// Installed but no backup files exist.
	_ = os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://x/mcp"}}}`), 0600)

	manifestDir := t.TempDir()
	memDir := filepath.Join(manifestDir, "memory")
	_ = os.MkdirAll(memDir, 0700)
	_ = os.WriteFile(filepath.Join(memDir, "manifest.yaml"), []byte(
		`name: memory
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: default
    port: 9200
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`), 0600)

	a := NewAPI()
	buf := &bytes.Buffer{}
	report, err := a.Demigrate(DemigrateOpts{
		Servers:  []string{"memory"},
		ScanOpts: ScanOpts{ManifestDir: manifestDir},
		Writer:   buf,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}
	if len(report.Failed) != 1 {
		t.Errorf("expected 1 failure, got %d: %+v", len(report.Failed), report.Failed)
	}
	if len(report.Restored) != 0 {
		t.Errorf("expected 0 restored, got %d", len(report.Restored))
	}
}
```

### Step 2: Run — red

```bash
go test ./internal/api/ -run TestDemigrate -count=1
```

Expected: `DemigrateOpts undefined`.

### Step 3: Implement `internal/api/demigrate.go`

```go
package api

import (
	"errors"
	"fmt"
	"io"

	"mcp-local-hub/internal/clients"
)

// DemigrateOpts controls a reverse-migration invocation. Semantics mirror
// MigrateOpts: the manifest drives the client-binding set, ClientsInclude
// narrows that set, and Writer receives human-readable progress.
type DemigrateOpts struct {
	Servers        []string
	ClientsInclude []string
	ScanOpts       ScanOpts
	Writer         io.Writer
}

// DemigrateReport carries per-(server, client) outcomes.
type DemigrateReport struct {
	Restored []RestoredMigration `json:"restored"`
	Failed   []FailedMigration   `json:"failed"` // reuses migrate's failure shape
}

// RestoredMigration is one successfully rolled-back (server, client) pair.
type RestoredMigration struct {
	Server string `json:"server"`
	Client string `json:"client"`
}

// Demigrate rolls (server, client) pairs back from hub-HTTP to their
// pre-migrate shape by reading each client's most recent backup and
// writing the named entry (or removing it, if the backup predates
// migrate adding it). The set of (server, client) pairs is derived
// from each server's manifest.client_bindings intersected with
// ClientsInclude — mirroring MigrateFrom's shape so Demigrate reverses
// exactly the rows Migrate would produce. Entries in other clients with
// the same server name are NOT touched.
//
// Multi-server ordering constraint: when multiple servers were migrated
// from the same client, the latest backup captures state BETWEEN those
// migrations. It holds earlier-migrated servers in hub-HTTP form and
// later-migrated servers in stdio form. Demigrating must proceed
// newest-first (or target only the last-migrated server) — older
// servers hit ErrBackupEntryAlreadyMigrated and surface as Failed rows
// with a message directing the operator to restore from the
// `-original` sentinel manually. This is intentional: silently
// re-writing hub-HTTP data is strictly worse than a clear failure.
//
// Errors per-(server, client) are captured in the report; the function
// returns nil unless a setup-level problem applies to every row.
func (a *API) Demigrate(opts DemigrateOpts) (*DemigrateReport, error) {
	if opts.Writer == nil {
		opts.Writer = io.Discard
	}
	report := &DemigrateReport{}
	allClients := clients.AllClients()

	includedClient := func(c string) bool {
		if len(opts.ClientsInclude) == 0 {
			return true
		}
		for _, x := range opts.ClientsInclude {
			if x == c {
				return true
			}
		}
		return false
	}

	for _, server := range opts.Servers {
		m, err := loadManifestForServer(opts.ScanOpts.ManifestDir, server)
		if err != nil {
			report.Failed = append(report.Failed, FailedMigration{
				Server: server, Err: err.Error(),
			})
			continue
		}
		for _, binding := range m.ClientBindings {
			if !includedClient(binding.Client) {
				continue
			}
			adapter := allClients[binding.Client]
			if adapter == nil {
				// No adapter constructed on this host (e.g. UserHomeDir
				// failed). Matches MigrateFrom: silent skip.
				continue
			}
			if !adapter.Exists() {
				// Client not installed — nothing to restore. Skip
				// quietly, same as MigrateFrom.
				continue
			}
			backupPath, ok, err := adapter.LatestBackupPath()
			if err != nil {
				report.Failed = append(report.Failed, FailedMigration{
					Server: server, Client: binding.Client, Err: err.Error(),
				})
				continue
			}
			if !ok {
				report.Failed = append(report.Failed, FailedMigration{
					Server: server, Client: binding.Client,
					Err: "no backup found (migration may never have run on this machine)",
				})
				continue
			}
			if err := adapter.RestoreEntryFromBackup(backupPath, server); err != nil {
				errMsg := err.Error()
				if errors.Is(err, clients.ErrBackupEntryAlreadyMigrated) {
					errMsg = fmt.Sprintf(
						"latest backup holds %q already in hub-managed form — demigrate newest-first, or restore manually from the -original sentinel (%s)",
						server, backupPath)
				}
				report.Failed = append(report.Failed, FailedMigration{
					Server: server, Client: binding.Client, Err: errMsg,
				})
				continue
			}
			report.Restored = append(report.Restored, RestoredMigration{
				Server: server, Client: binding.Client,
			})
			fmt.Fprintf(opts.Writer, "restored %s for %s from %s\n", server, binding.Client, backupPath)
		}
	}
	return report, nil
}
```

### Step 4: Green

```bash
go test ./internal/api/ -run TestDemigrate -count=1
```

Expected: 4 new `TestDemigrate*` tests PASS.

### Step 5: Commit

```bash
git add internal/api/demigrate.go internal/api/demigrate_test.go
git commit -m "feat(api): Demigrate per-entry rollback driven by manifest bindings (B1)

Mirrors MigrateFrom's iteration: load each server's manifest, iterate
ClientBindings, apply ClientsInclude filter. Each (server, client)
pair reads that client's latest backup and restores the named entry
only — other entries in the same config and entries in unrelated
clients are untouched."
```

---

## Task 6: Extend `ExtractManifestFromClient` switch + bug fixes

**Files:**
- Modify: `internal/api/scan.go` (extend switch, fix Claude no-type detection, reject Antigravity relay)
- Modify: `internal/api/scan_extract_test.go` (+tests for each new branch + bug fixes)

**Preamble — know the surface before editing:**

The existing `ExtractManifestFromClient` at `internal/api/scan.go:427` has a `switch client` with one case for `claude-code` and a default returning an error. The draft-manifest helper `renderDraftManifestYAML` at `:509` produces the correct `config.ServerManifest` shape (`kind: global`, `transport: stdio-bridge`, top-level `command`/`base_args`/`env`, `daemons: [{name: default, port: N}]`, `client_bindings` for all four clients). Task 6 extends the switch with codex-cli / gemini-cli / antigravity cases, tightens the Antigravity relay rejection (uses the shared exported `clients.IsMcphubBinary` helper from Task 1 so it does not false-reject genuine user stdio with first arg `"relay"`), and implicitly accepts Claude entries that omit `type` but carry `command` (which the existing scan classifier at `scan.go:196-202` already treats as stdio).

### Step 1: Write failing tests

Append to `internal/api/scan_extract_test.go`:

```go
func TestExtractManifestFromClient_ClaudeCode_AcceptsNoTypeField(t *testing.T) {
	// Older Claude configs sometimes omit `type` for stdio entries.
	// scan.go's existing scanner accepts no-type + command as stdio
	// (scan.go:196-202); extract must agree.
	tmp := t.TempDir()
	claudePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"command":"npx","args":["-y","mem"],"env":{"DEBUG":"1"}}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	yaml, err := a.ExtractManifestFromClient("claude-code", "memory", ScanOpts{
		ClaudeConfigPath: claudePath,
		ManifestDir:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ExtractManifestFromClient: %v", err)
	}
	// Parse + validate. No substring matching.
	m, err := config.ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v\nyaml:\n%s", err, yaml)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("manifest invalid: %v", err)
	}
	if m.Command != "npx" {
		t.Errorf("Command = %q, want npx", m.Command)
	}
}

func TestExtractManifestFromClient_CodexCLI(t *testing.T) {
	tmp := t.TempDir()
	codexPath := filepath.Join(tmp, "config.toml")
	body := `[mcp_servers.memory]
command = "npx"
args = ["-y", "@modelcontextprotocol/server-memory"]

[mcp_servers.memory.env]
DEBUG = "1"
`
	if err := os.WriteFile(codexPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	yaml, err := a.ExtractManifestFromClient("codex-cli", "memory", ScanOpts{
		CodexConfigPath: codexPath,
		ManifestDir:     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ExtractManifestFromClient: %v", err)
	}
	m, err := config.ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v\n%s", err, yaml)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if m.Command != "npx" {
		t.Errorf("Command = %q", m.Command)
	}
	if m.Env["DEBUG"] != "1" {
		t.Errorf("Env[DEBUG] = %q", m.Env["DEBUG"])
	}
}

func TestExtractManifestFromClient_GeminiCLI(t *testing.T) {
	tmp := t.TempDir()
	geminiPath := filepath.Join(tmp, ".gemini", "settings.json")
	_ = os.MkdirAll(filepath.Dir(geminiPath), 0700)
	if err := os.WriteFile(geminiPath, []byte(
		`{"mcpServers":{"memory":{"command":"npx","args":["-y","mem"]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	yaml, err := a.ExtractManifestFromClient("gemini-cli", "memory", ScanOpts{
		GeminiConfigPath: geminiPath,
		ManifestDir:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ExtractManifestFromClient: %v", err)
	}
	m, err := config.ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v\n%s", err, yaml)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if m.Command != "npx" {
		t.Errorf("Command = %q, want npx", m.Command)
	}
}

func TestExtractManifestFromClient_Antigravity_RejectsHubRelay(t *testing.T) {
	tmp := t.TempDir()
	agPath := filepath.Join(tmp, "mcp_config.json")
	// This is an mcphub-managed relay entry (command is mcphub, args[0]
	// is "relay"). Extracting a manifest from this would produce an
	// infinite loop (manifest → install → same relay entry → manifest
	// → ...). Reject explicitly.
	if err := os.WriteFile(agPath, []byte(
		`{"mcpServers":{"serena":{"command":"C:/mcphub.exe","args":["relay","--server","serena","--daemon","claude"],"disabled":false}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	_, err := a.ExtractManifestFromClient("antigravity", "serena", ScanOpts{
		AntigravityConfigPath: agPath,
		ManifestDir:           t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected rejection for mcphub-relay entry, got nil")
	}
	if !strings.Contains(err.Error(), "relay") {
		t.Errorf("expected error to mention relay, got %v", err)
	}
}

func TestExtractManifestFromClient_Antigravity_AcceptsGenuineStdio(t *testing.T) {
	// A user could (hypothetically) configure Antigravity with a
	// non-relay stdio entry. That IS a user stdio config and is valid
	// to extract. This test documents that the reject is narrow.
	tmp := t.TempDir()
	agPath := filepath.Join(tmp, "mcp_config.json")
	if err := os.WriteFile(agPath, []byte(
		`{"mcpServers":{"custom":{"command":"/usr/local/bin/custom-mcp","args":["--flag"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	yaml, err := a.ExtractManifestFromClient("antigravity", "custom", ScanOpts{
		AntigravityConfigPath: agPath,
		ManifestDir:           t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ExtractManifestFromClient: %v", err)
	}
	m, err := config.ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v\n%s", err, yaml)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if m.Command != "/usr/local/bin/custom-mcp" {
		t.Errorf("Command = %q", m.Command)
	}
}

func TestExtractManifestFromClient_Antigravity_AcceptsRelayFirstArgWithNonMcphubCmd(t *testing.T) {
	// This is the Codex R2 finding #2: a user could have a genuine
	// stdio server whose first argument happens to be the literal
	// string "relay" (e.g., a custom relay tool at /usr/local/bin/mymcp).
	// The hub-relay reject must check BOTH command (is mcphub binary)
	// AND args[0] == "relay" — not args[0] alone.
	tmp := t.TempDir()
	agPath := filepath.Join(tmp, "mcp_config.json")
	if err := os.WriteFile(agPath, []byte(
		`{"mcpServers":{"custom":{"command":"/usr/local/bin/mymcp","args":["relay","--target","remote"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	yaml, err := a.ExtractManifestFromClient("antigravity", "custom", ScanOpts{
		AntigravityConfigPath: agPath,
		ManifestDir:           t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ExtractManifestFromClient: %v (expected accept — command is not mcphub, relay-first-arg alone must not reject)", err)
	}
	m, err := config.ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v\n%s", err, yaml)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if m.Command != "/usr/local/bin/mymcp" {
		t.Errorf("Command = %q, want /usr/local/bin/mymcp", m.Command)
	}
	if len(m.BaseArgs) == 0 || m.BaseArgs[0] != "relay" {
		t.Errorf("BaseArgs = %v, want [relay, --target, remote]", m.BaseArgs)
	}
}
```

If `config` / `strings` are not already in the test file's imports, add `"strings"` and `"mcp-local-hub/internal/config"`.

**Read current `ScanOpts`** — if it does not already have `CodexConfigPath` / `GeminiConfigPath` / `AntigravityConfigPath` fields, the tests will not compile. Check:

```bash
grep -n "CodexConfigPath\|GeminiConfigPath\|AntigravityConfigPath\|type ScanOpts" internal/api/scan.go
```

If any of the three paths is missing, add them to `ScanOpts` struct in `scan.go`. Use the same naming convention as `ClaudeConfigPath`.

### Step 2: Run — red

```bash
go test ./internal/api/ -run TestExtractManifestFromClient -count=1
```

Expected: compile errors or assertion failures (default case in the existing switch still fires for new clients).

### Step 3: Extend `ExtractManifestFromClient` in `internal/api/scan.go`

Inside the existing function (starting `:427`), change the switch. Reuse the same raw-map parsing pattern as the claude-code branch. Note: the existing claude-code branch already does `raw = cfg.MCPServers[serverName]` — simply drop through to the shared `cmd, args, env` extraction block that follows the switch.

Replace the `switch client` block with:

```go
	switch client {
	case "claude-code":
		if opts.ClaudeConfigPath == "" {
			return "", fmt.Errorf("ClaudeConfigPath empty")
		}
		data, err := os.ReadFile(opts.ClaudeConfigPath)
		if err != nil {
			return "", err
		}
		var cfg struct {
			MCPServers map[string]map[string]any `json:"mcpServers"`
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return "", err
		}
		raw = cfg.MCPServers[serverName]

	case "codex-cli":
		if opts.CodexConfigPath == "" {
			return "", fmt.Errorf("CodexConfigPath empty")
		}
		data, err := os.ReadFile(opts.CodexConfigPath)
		if err != nil {
			return "", err
		}
		var root map[string]any
		if err := toml.Unmarshal(data, &root); err != nil {
			return "", err
		}
		servers, _ := root["mcp_servers"].(map[string]any)
		if servers != nil {
			raw, _ = servers[serverName].(map[string]any)
		}

	case "gemini-cli":
		if opts.GeminiConfigPath == "" {
			return "", fmt.Errorf("GeminiConfigPath empty")
		}
		data, err := os.ReadFile(opts.GeminiConfigPath)
		if err != nil {
			return "", err
		}
		var cfg struct {
			MCPServers map[string]map[string]any `json:"mcpServers"`
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return "", err
		}
		raw = cfg.MCPServers[serverName]

	case "antigravity":
		if opts.AntigravityConfigPath == "" {
			return "", fmt.Errorf("AntigravityConfigPath empty")
		}
		data, err := os.ReadFile(opts.AntigravityConfigPath)
		if err != nil {
			return "", err
		}
		var cfg struct {
			MCPServers map[string]map[string]any `json:"mcpServers"`
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return "", err
		}
		raw = cfg.MCPServers[serverName]
		// Antigravity entries written by mcphub migrate use command=mcphub,
		// args[0]="relay". Extracting a manifest from THAT would loop:
		// manifest → install → relay entry → manifest → ... Reject narrowly:
		// command must be the mcphub binary AND args[0] must equal "relay".
		// A user's genuine stdio server whose first arg happens to be "relay"
		// but whose command is not mcphub passes through unchanged. Uses the
		// shared clients.IsMcphubBinary helper (also used by RestoreEntryFromBackup
		// for hub-relay detection in adapter defensive checks).
		if raw != nil {
			cmd, _ := raw["command"].(string)
			if clients.IsMcphubBinary(cmd) {
				if args, ok := raw["args"].([]any); ok && len(args) > 0 {
					if first, ok := args[0].(string); ok && first == "relay" {
						return "", fmt.Errorf("entry %q is a mcphub-managed relay stdio (command is mcphub binary + args[0]==\"relay\") — not user-configured stdio, cannot extract a manifest from it", serverName)
					}
				}
			}
		}

	default:
		return "", fmt.Errorf("extract not yet supported for client %q (extend here when needed)", client)
	}
```

Add `"github.com/pelletier/go-toml/v2"` to imports of `scan.go` if not already present (it is used elsewhere in the package; likely already available via the codex adapter, but scan.go imports explicitly).

Also add `"mcp-local-hub/internal/clients"` to imports (new — scan.go has not needed it previously, but the Antigravity relay-reject now calls `clients.IsMcphubBinary`).

If `ScanOpts` is missing the three new path fields, add:

```go
	CodexConfigPath       string
	GeminiConfigPath      string
	AntigravityConfigPath string
```

### Step 4: Green + full suite

```bash
go test ./internal/api/ -count=1
```

Expected: all 6 new `TestExtractManifestFromClient_*` tests PASS (5 new branches + the AcceptsRelayFirstArgWithNonMcphubCmd narrow-reject test from R2 fix #2). Existing `TestExtractManifestFromClientPreservesCommandAndEnv` test still PASS (it exercises the claude-code path, which is unchanged except for the no-type acceptance).

### Step 5: Commit

```bash
git add internal/api/scan.go internal/api/scan_extract_test.go
git commit -m "feat(api): ExtractManifestFromClient supports codex/gemini/antigravity + narrow relay-reject (B2)

Adds codex-cli / gemini-cli / antigravity branches to the switch in
ExtractManifestFromClient, reusing the existing renderDraftManifestYAML
helper (scan.go:509) so all produced manifests pass config.Validate.

Antigravity branch narrowly rejects mcphub-managed relay entries
(command IS mcphub binary AND args[0]=\"relay\") — uses the shared
clients.IsMcphubBinary helper so a genuine user stdio whose first arg
happens to be \"relay\" with a non-mcphub command is still accepted.

Claude-code branch implicitly accepts no-type stdio (which the existing
scan classifier already treats as stdio). Tests parse with
config.ParseManifest + Validate instead of substring-matching."
```

---

## Task 7: Full-suite smoke + merge-readiness

**Files:** none (verification only)

### Step 1: Full Go test suite

```bash
go test ./... -count=1
```

Expected: everything from master + the new tests all PASS. Any pre-existing flakes (`internal/daemon/TestHostStopUnblocksPendingHandlers`, `internal/api/TestInstallAllInstallsEverything` port 9131 collision) are not regressions from this plan — re-run once to filter.

### Step 2: Build

```bash
go build ./...
```

Expected: clean.

### Step 3: Frontend + E2E sanity (no plan changes here but worth verifying nothing drifted)

```bash
cd internal/gui/frontend
npm run typecheck
cd ../e2e
npm test
```

Expected: typecheck clean; 11/11 Playwright tests green.

### Step 4: Self-review PR-ready state

```bash
git log master..HEAD --oneline
```

Expected: 6 commits (Tasks 1 through 6). `git status` clean.

### Step 5: No commit. Proceed to Codex CLI review → PR open → GitHub Codex review → merge per overnight plan rules.

---

## Dependency order summary

Task 1 (interface + helper) → Tasks 2 / 3 / 4 (adapters, independent) → Task 5 (Demigrate, depends on all 4 adapters implementing the new interface) → Task 6 (ExtractManifestFromClient extension, independent of Tasks 1-5 code-wise but lands last for reviewer ergonomics) → Task 7 (smoke, no changes).

- Tasks 2, 3, 4 can be reordered but all three must land before Task 5 because `go build` fails until every adapter satisfies the extended interface.
- Task 6 changes only `internal/api/scan.go` + its tests; it could technically precede Tasks 1-5 in any order. Sequenced last for commit-history readability ("interface + adapters + Demigrate" reads as one story; "scan.go extension" is a related but separable second story).
