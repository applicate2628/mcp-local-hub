# Phase 3B-II A3-a — Secrets Registry Screen Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deliver the GUI Secrets registry screen at `#/secrets` with 6 HTTP endpoints, 6 `api.go` wrappers, manifest scan helper, 4-state UI, three modals (Add/Rotate/Delete), and 14 E2E scenarios.

**Architecture:** Backend introduces `api.Secrets*` wrappers + `ScanManifestEnv` helper (both in `internal/api`); HTTP handlers in `internal/gui/secrets.go`; D9 refactors `realRestarter` to return `[]api.RestartResult` (existing type), updates `Dashboard.tsx` consumer for 207 status. Frontend adds `Secrets.tsx` 4-state screen + 3 dialog-based modals + secrets-api.ts wrappers + `useSecretsSnapshot` hook + nav entry in `app.tsx`.

**Tech Stack:** Go 1.26.2, Preact 10 + TypeScript 5, Vite 5, Vitest, Playwright headless Chromium, age (filippo.io/age), gopkg.in/yaml.v3.

**Source memo:** [docs/superpowers/specs/2026-04-25-phase-3b-ii-a3a-secrets-screen-design.md](../specs/2026-04-25-phase-3b-ii-a3a-secrets-screen-design.md) (committed at `ee9e8ee`).

**Branch:** `feat/phase-3b-ii-a3a-secrets-screen` (create from `master` at the start of Task 1).

---

## File Structure

**Created:**

| Path | Responsibility |
|---|---|
| `internal/api/secrets.go` | 6 wrappers + types (UsageRef, ManifestError, SecretsEnvelope, SecretRow, SecretsInitResult, SecretsRotateResult, SecretsDeleteError) |
| `internal/api/secrets_scan.go` | `ScanManifestEnv()` tolerant manifest scan (embed-first/disk-fallback via `manifest_source.go` helpers) |
| `internal/api/secrets_test.go` | Unit tests for wrappers (~14 cases) |
| `internal/api/secrets_scan_test.go` | Unit tests for scan helper (5 cases) |
| `internal/gui/secrets.go` | HTTP handlers for 6 endpoints |
| `internal/gui/secrets_test.go` | HTTP handler tests (~30 cases) |
| `internal/gui/frontend/src/lib/secrets-api.ts` | Typed fetch wrappers (6 functions) |
| `internal/gui/frontend/src/lib/secrets-api.test.ts` | Vitest coverage (~14 cases) |
| `internal/gui/frontend/src/lib/use-secrets-snapshot.ts` | React hook for `GET /api/secrets` |
| `internal/gui/frontend/src/lib/use-secrets-snapshot.test.ts` | Vitest hook tests |
| `internal/gui/frontend/src/screens/Secrets.tsx` | 4-state main screen |
| `internal/gui/frontend/src/components/AddSecretModal.tsx` | Add secret dialog |
| `internal/gui/frontend/src/components/RotateSecretModal.tsx` | 3-button rotate dialog + persistent CTA + restart-now |
| `internal/gui/frontend/src/components/DeleteSecretModal.tsx` | D5 escalation flow modal |
| `internal/gui/e2e/tests/secrets.spec.ts` | 14 E2E scenarios |
| `work-items/bugs/a3a-vault-concurrent-edit-lww.md` | Documented R1 last-write-wins limitation |

**Modified:**

| Path | Reason |
|---|---|
| `internal/api/install.go:1456-1460` | Add `json:"task_name"` and `json:"error"` (NOT omitempty) tags to `RestartResult` |
| `internal/gui/server.go` | Refactor `restarter` interface to `Restart(server) ([]api.RestartResult, error)`; add `s.secrets` field |
| `internal/gui/servers.go:9-35` | Update restart handler to return 200/207/500 with `[]api.RestartResult` body |
| `internal/gui/servers_test.go` | Update existing restart tests for new shape |
| `internal/gui/frontend/src/screens/Dashboard.tsx:73-76` | Inspect `restart_results[].error` to surface partial failures |
| `internal/gui/frontend/src/app.tsx` | Add `case "secrets"` route + nav link |
| `internal/gui/assets/{index.html,app.js,style.css}` | Regenerated via `go generate ./internal/gui/...` |
| `CLAUDE.md` | E2E coverage block + count line (52 → 66) |

---

## Task 1: Backend scaffold — types, scan helper, wrappers, LWW work-items entry

**Goal:** A single self-contained commit that adds all backend types and helpers, with full unit coverage. No HTTP handlers yet (Task 3). No D9 refactor yet (Task 2).

**Files:**
- Create: `internal/api/secrets.go`
- Create: `internal/api/secrets_scan.go`
- Create: `internal/api/secrets_test.go`
- Create: `internal/api/secrets_scan_test.go`
- Create: `work-items/bugs/a3a-vault-concurrent-edit-lww.md`
- Modify: `internal/api/install.go:1456-1460` (JSON tags on `RestartResult`)

**Branch setup (only the first time you start this plan):**

- [ ] **Step 1.0a: Create branch from clean master**

```bash
cd d:/dev/mcp-local-hub
git checkout master
git pull --ff-only
git checkout -b feat/phase-3b-ii-a3a-secrets-screen
```

Expected: `Switched to a new branch 'feat/phase-3b-ii-a3a-secrets-screen'`.

- [ ] **Step 1.0b: Verify clean worktree**

Run: `git status`
Expected: `nothing to commit, working tree clean`.

### 1.A — `RestartResult` JSON tags (D9 prerequisite)

- [ ] **Step 1.A.1: Inspect current `RestartResult` shape**

Run: `grep -n "type RestartResult struct" internal/api/install.go`
Expected: `1457:type RestartResult struct {` (or close).

Read the surrounding lines to confirm fields are `TaskName string` / `Err string` with no JSON tags.

- [ ] **Step 1.A.2: Add JSON tags**

Edit `internal/api/install.go` lines 1456-1460. Replace:

```go
// RestartResult is one row in a RestartAll report.
type RestartResult struct {
	TaskName string
	Err      string
}
```

with:

```go
// RestartResult is one row in a RestartAll/Restart report. JSON tags
// added in Phase 3B-II A3-a (memo D9): the GUI restart handler now
// emits per-task results in JSON, and `error` is NOT omitempty —
// empty-string is the success discriminator the frontend parses.
type RestartResult struct {
	TaskName string `json:"task_name"`
	Err      string `json:"error"`
}
```

- [ ] **Step 1.A.3: Verify build still green and CLI restart still parses**

Run: `go build ./...`
Expected: no errors.

Run: `go test ./internal/cli/...`
Expected: existing CLI tests pass — they consume `r.TaskName` / `r.Err` via Go field access, not JSON, so the tag addition is non-breaking.

### 1.B — `secrets_scan.go` (TDD)

- [ ] **Step 1.B.1: Write failing test for happy path**

Create `internal/api/secrets_scan_test.go` with:

```go
package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// helper: write a tiny manifest YAML to <dir>/<name>/manifest.yaml.
func writeManifest(t *testing.T, dir, name, body string) {
	t.Helper()
	subdir := filepath.Join(dir, name)
	if err := os.MkdirAll(subdir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", subdir, err)
	}
	if err := os.WriteFile(filepath.Join(subdir, "manifest.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func TestScanManifestEnv_AggregatesSecretRefs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", dir)

	writeManifest(t, dir, "server-a", `name: server-a
env:
  OPENAI_API_KEY: secret:K1
  HOME: $HOME
  LITERAL: hello
`)
	writeManifest(t, dir, "server-b", `name: server-b
env:
  OPENAI_API_KEY: secret:K1
  WOLFRAM: secret:K2
  LOCAL: file:my_local_key
`)
	writeManifest(t, dir, "server-c", `name: server-c
env:
  PLAIN: just_a_value
`)

	usage, errs, err := ScanManifestEnv()
	if err != nil {
		t.Fatalf("ScanManifestEnv: %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("manifest_errors: want empty, got %v", errs)
	}
	got1 := usage["K1"]
	if len(got1) != 2 {
		t.Fatalf("usage[K1] len = %d, want 2", len(got1))
	}
	if got1[0].Server != "server-a" || got1[0].EnvVar != "OPENAI_API_KEY" {
		t.Errorf("usage[K1][0] = %+v", got1[0])
	}
	if got1[1].Server != "server-b" || got1[1].EnvVar != "OPENAI_API_KEY" {
		t.Errorf("usage[K1][1] = %+v", got1[1])
	}
	got2 := usage["K2"]
	if len(got2) != 1 || got2[0].Server != "server-b" || got2[0].EnvVar != "WOLFRAM" {
		t.Errorf("usage[K2] = %+v", got2)
	}
}
```

**Note**: The `MCPHUB_MANIFEST_DIR_OVERRIDE` env var does not exist yet. The next step adds it to `manifest_source.go`. For now, the test will fail to compile because `ScanManifestEnv` doesn't exist — that's expected.

- [ ] **Step 1.B.2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestScanManifestEnv_AggregatesSecretRefs -count=1`
Expected: FAIL with `undefined: ScanManifestEnv` (or similar).

- [ ] **Step 1.B.3: Add manifest-dir override hook to `manifest_source.go` that ALSO bypasses embed**

**Codex plan-R1 P1:** the previous draft only redirected the disk fallback, but `listManifestNamesEmbedFirst()` unions embed names with disk names — and shipped manifests (`servers/wolfram/manifest.yaml`, `servers/paper-search-mcp/manifest.yaml`) embed `secret:` refs. Without bypassing the embed under the override, scan tests that expect zero refs (`TestScanManifestEnv_IgnoresNonSecretPrefixes`) would fail because the embedded refs leak in.

Modify `internal/api/manifest_source.go`. Add the override helper at top:

```go
// manifestDirForTests is a test-only override consulted by
// ScanManifestEnv and the embed-aware helpers when set via
// MCPHUB_MANIFEST_DIR_OVERRIDE. When the override is non-empty the
// embed FS is bypassed entirely; tests get the test directory's
// manifests with no leakage from the binary's shipped set
// (which include `secret:` refs from wolfram, paper-search-mcp).
func manifestDirForTests() string {
	return os.Getenv("MCPHUB_MANIFEST_DIR_OVERRIDE")
}
```

Update `loadManifestYAMLEmbedFirst` so the override fully bypasses embed:

```go
func loadManifestYAMLEmbedFirst(name string) ([]byte, error) {
	if dir := manifestDirForTests(); dir != "" {
		// Test-only override: skip the embed FS entirely.
		return os.ReadFile(filepath.Join(dir, name, "manifest.yaml"))
	}
	if data, err := fs.ReadFile(servers.Manifests, name+"/manifest.yaml"); err == nil {
		return data, nil
	}
	path := filepath.Join(defaultManifestDir(), name, "manifest.yaml")
	return os.ReadFile(path)
}
```

Update `listManifestNamesEmbedFirst` similarly:

```go
func listManifestNamesEmbedFirst() ([]string, error) {
	if dir := manifestDirForTests(); dir != "" {
		// Test-only override: skip the embed FS entirely so tests get
		// only the manifests they explicitly seed.
		entries, err := os.ReadDir(dir)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		var names []string
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, e.Name(), "manifest.yaml")); err == nil {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		return names, nil
	}
	// Production path: union embed + disk.
	seen := map[string]bool{}
	for _, n := range embeddedManifestNames() {
		seen[n] = true
	}
	entries, err := os.ReadDir(defaultManifestDir())
	if err != nil && !os.IsNotExist(err) {
		// Disk read failure is non-fatal — return what we have from embed.
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(defaultManifestDir(), e.Name(), "manifest.yaml")); err == nil {
			seen[e.Name()] = true
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}
```

Add `"sort"` to the imports if it isn't there already.

- [ ] **Step 1.B.4: Implement `ScanManifestEnv` minimally**

Create `internal/api/secrets_scan.go`:

```go
package api

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// UsageRef records one (server, env_var) pair where a secret key is
// referenced. Used in the GET /api/secrets envelope and as the input
// to delete/rotate UI flows that need exact ref locations.
type UsageRef struct {
	Server string `json:"server"`
	EnvVar string `json:"env_var"`
}

// ManifestError surfaces a per-manifest parse failure during the
// secrets scan. Only failures of the {Name, Env} narrow projection
// are recorded — full-schema drift in unrelated fields is not
// reported (per memo §2.5).
type ManifestError struct {
	Name  string `json:"name,omitempty"`
	Path  string `json:"path"`
	Error string `json:"error"`
}

// scanProjection is the narrow YAML shape the scan parses. Anything
// outside Name and Env is ignored. This keeps the scan tolerant: a
// manifest with a malformed `daemons` block still contributes its
// secret refs.
type scanProjection struct {
	Name string            `yaml:"name"`
	Env  map[string]string `yaml:"env"`
}

// ScanManifestEnv walks the embed-first/disk-fallback manifest set and
// returns a map of secret keys to the (server, env_var) refs that use
// them, plus a list of per-manifest parse errors. The function never
// returns a non-nil error on per-manifest parse failures — callers
// must inspect the manifest_errors slice for those.
func ScanManifestEnv() (map[string][]UsageRef, []ManifestError, error) {
	names, err := listManifestNamesEmbedFirst()
	if err != nil {
		return nil, nil, fmt.Errorf("list manifests: %w", err)
	}

	usage := make(map[string][]UsageRef)
	var errs []ManifestError

	for _, name := range names {
		raw, err := loadManifestYAMLEmbedFirst(name)
		if err != nil {
			errs = append(errs, ManifestError{
				Path:  name + "/manifest.yaml",
				Error: err.Error(),
			})
			continue
		}
		var proj scanProjection
		if err := yaml.Unmarshal(raw, &proj); err != nil {
			errs = append(errs, ManifestError{
				Path:  name + "/manifest.yaml",
				Error: err.Error(),
			})
			continue
		}
		if proj.Name == "" {
			errs = append(errs, ManifestError{
				Path:  name + "/manifest.yaml",
				Error: "missing name field",
			})
			continue
		}
		for envKey, envVal := range proj.Env {
			if !strings.HasPrefix(envVal, "secret:") {
				continue
			}
			key := strings.TrimPrefix(envVal, "secret:")
			usage[key] = append(usage[key], UsageRef{
				Server: proj.Name,
				EnvVar: envKey,
			})
		}
	}

	// Sort each usage[] slice by Server name for deterministic output.
	for k := range usage {
		sort.Slice(usage[k], func(i, j int) bool {
			return usage[k][i].Server < usage[k][j].Server
		})
	}

	return usage, errs, nil
}
```

- [ ] **Step 1.B.5: Run the happy-path test to verify pass**

Run: `go test ./internal/api/ -run TestScanManifestEnv_AggregatesSecretRefs -count=1 -v`
Expected: PASS.

- [ ] **Step 1.B.6: Add malformed-env test (TDD)**

Append to `internal/api/secrets_scan_test.go`:

```go
func TestScanManifestEnv_MalformedEnvProducesManifestError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", dir)

	// env block where one value is itself a YAML mapping → strict typing
	// rejects it as map[string]string; the whole env block fails to
	// unmarshal under our narrow projection.
	writeManifest(t, dir, "broken-env", `name: broken-env
env:
  GOOD: secret:should_not_appear
  BAD:
    nested: value
`)

	usage, errs, err := ScanManifestEnv()
	if err != nil {
		t.Fatalf("ScanManifestEnv: %v", err)
	}
	if len(errs) != 1 {
		t.Fatalf("manifest_errors len = %d, want 1: %+v", len(errs), errs)
	}
	if errs[0].Path != "broken-env/manifest.yaml" {
		t.Errorf("manifest_errors[0].Path = %q", errs[0].Path)
	}
	if _, exists := usage["should_not_appear"]; exists {
		t.Errorf("usage leaked refs from a manifest whose env failed to parse: %+v", usage)
	}
}
```

- [ ] **Step 1.B.7: Run the malformed-env test**

Run: `go test ./internal/api/ -run TestScanManifestEnv_MalformedEnvProducesManifestError -count=1 -v`
Expected: PASS (the current scan recorded an error and skipped the manifest).

- [ ] **Step 1.B.8: Add top-level-YAML-error test**

Append:

```go
func TestScanManifestEnv_TolerantOnYAMLError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", dir)

	writeManifest(t, dir, "valid-server", `name: valid-server
env:
  OPENAI: secret:K1
`)
	writeManifest(t, dir, "broken-yaml", `name: broken-yaml
env:
  OPENAI: secret:K2
unclosed_list: [
`)

	usage, errs, err := ScanManifestEnv()
	if err != nil {
		t.Fatalf("ScanManifestEnv: %v", err)
	}
	if len(errs) != 1 {
		t.Fatalf("manifest_errors len = %d, want 1: %+v", len(errs), errs)
	}
	if errs[0].Path != "broken-yaml/manifest.yaml" {
		t.Errorf("manifest_errors[0].Path = %q", errs[0].Path)
	}
	if got := usage["K1"]; len(got) != 1 || got[0].Server != "valid-server" {
		t.Errorf("usage[K1] = %+v, want [{valid-server, OPENAI}]", got)
	}
	if _, exists := usage["K2"]; exists {
		t.Errorf("usage leaked K2 from broken-yaml manifest: %+v", usage)
	}
}
```

- [ ] **Step 1.B.9: Add ignores-non-secret-prefixes test**

Append:

```go
func TestScanManifestEnv_IgnoresNonSecretPrefixes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", dir)

	writeManifest(t, dir, "no-secrets", `name: no-secrets
env:
  HOME: $HOME
  LOCAL: file:my_key
  PLAIN: literal_value
`)

	usage, errs, err := ScanManifestEnv()
	if err != nil {
		t.Fatalf("ScanManifestEnv: %v", err)
	}
	if len(errs) != 0 {
		t.Errorf("manifest_errors should be empty, got %v", errs)
	}
	if len(usage) != 0 {
		t.Errorf("usage should be empty, got %v", usage)
	}
}
```

- [ ] **Step 1.B.10: Add missing-name test**

Append:

```go
func TestScanManifestEnv_MissingNameProducesError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", dir)

	writeManifest(t, dir, "no-name", `env:
  OPENAI: secret:K1
`)

	usage, errs, err := ScanManifestEnv()
	if err != nil {
		t.Fatalf("ScanManifestEnv: %v", err)
	}
	if len(errs) != 1 {
		t.Fatalf("manifest_errors len = %d, want 1: %+v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Error, "missing name field") {
		t.Errorf("manifest_errors[0].Error = %q, want containing 'missing name field'", errs[0].Error)
	}
	if _, exists := usage["K1"]; exists {
		t.Errorf("usage should not contain refs from name-less manifest")
	}
}
```

- [ ] **Step 1.B.10b: Add Name-populated assertion to malformed-env test (Codex plan-R1 P2)**

Update `TestScanManifestEnv_MalformedEnvProducesManifestError` (Step 1.B.6) to additionally assert `errs[0].Name == "broken-env"` — the memo §7.1 contract requires `Name` be populated when extractable. Modify the assertions:

```go
if errs[0].Name != "broken-env" {
    t.Errorf("manifest_errors[0].Name = %q, want broken-env (memo §7.1 + plan-R1 P2)", errs[0].Name)
}
```

This requires updating `ScanManifestEnv` impl to populate `Name` when the top-level YAML parsed enough to extract it but the env was malformed. Refine the impl:

```go
// In secrets_scan.go, in the env-failure branch, attempt a name-only
// re-parse so the ManifestError carries the server name when known.
//
// Replace:
//   if err := yaml.Unmarshal(raw, &proj); err != nil {
//       errs = append(errs, ManifestError{Path: name + "/manifest.yaml", Error: err.Error()})
//       continue
//   }
// with a two-stage parse: first try the narrow projection, then if it
// fails, re-parse with a name-only projection to see if at least the
// name is recoverable.
{
    var proj scanProjection
    err := yaml.Unmarshal(raw, &proj)
    if err != nil {
        // Try a name-only fallback so the ManifestError can carry a Name when known.
        var nameOnly struct{ Name string `yaml:"name"` }
        if nameErr := yaml.Unmarshal(raw, &nameOnly); nameErr == nil && nameOnly.Name != "" {
            errs = append(errs, ManifestError{Name: nameOnly.Name, Path: name + "/manifest.yaml", Error: err.Error()})
        } else {
            errs = append(errs, ManifestError{Path: name + "/manifest.yaml", Error: err.Error()})
        }
        continue
    }
    // … rest unchanged …
}
```

- [ ] **Step 1.B.11: Run all scan tests**

Run: `go test ./internal/api/ -run TestScanManifestEnv -count=1 -v`
Expected: 5 tests PASS. Note: because the override now fully bypasses the embed FS (Codex plan-R1 P1), `TestScanManifestEnv_IgnoresNonSecretPrefixes` correctly sees only the test-seeded manifest with no `secret:` refs and reports empty usage.

### 1.C — `secrets.go` types and wrappers (TDD)

- [ ] **Step 1.C.1: Create `internal/api/secrets.go` with types only**

```go
package api

// SecretsEnvelope is the GET /api/secrets response body. See memo §5.2.
type SecretsEnvelope struct {
	VaultState     string          `json:"vault_state"`
	Secrets        []SecretRow     `json:"secrets"`
	ManifestErrors []ManifestError `json:"manifest_errors"`
}

// SecretRow is one row in the registry. State distinguishes:
//   - "present"             — key exists in vault (vault_state == "ok")
//   - "referenced_missing"  — manifest references key, vault doesn't (vault_state == "ok")
//   - "referenced_unverified" — manifest references key, vault not readable
type SecretRow struct {
	Name   string     `json:"name"`
	State  string     `json:"state"`
	UsedBy []UsageRef `json:"used_by"`
}

// SecretsInitResult is the body of POST /api/secrets/init. VaultState
// is omitempty so case 2c (cleanup-failed 500) can omit it — the vault
// state is undefined when manual cleanup is required (memo §5.1).
type SecretsInitResult struct {
	VaultState    string `json:"vault_state,omitempty"`
	CleanupStatus string `json:"cleanup_status,omitempty"`
	Error         string `json:"error,omitempty"`
	Code          string `json:"code,omitempty"`
	OrphanPath    string `json:"orphan_path,omitempty"`
}

// SecretsRotateResult is the body of PUT /api/secrets/:key.
type SecretsRotateResult struct {
	VaultUpdated   bool            `json:"vault_updated"`
	RestartResults []RestartResult `json:"restart_results"`
}

// SecretsDeleteError is returned by SecretsDelete when the no-confirm
// path is blocked by refs or scan errors (memo §5.5). The handler
// serializes UsedBy / ManifestErrors into the 409 body.
type SecretsDeleteError struct {
	Code           string
	Message        string
	UsedBy         []UsageRef
	ManifestErrors []ManifestError
}

func (e *SecretsDeleteError) Error() string { return e.Message }
```

- [ ] **Step 1.C.2: Write failing test for `SecretsInit` idempotent path**

Create `internal/api/secrets_test.go`:

```go
package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// secretsTestEnv redirects DefaultKeyPath / DefaultVaultPath to a
// per-test tempdir so each test gets isolated vault state.
func secretsTestEnv(t *testing.T) (keyPath, vaultPath string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir) // Windows path
	t.Setenv("XDG_DATA_HOME", dir) // Linux path
	t.Setenv("HOME", dir)          // macOS Library fallback root
	keyPath = filepath.Join(dir, "mcp-local-hub", ".age-key")
	vaultPath = filepath.Join(dir, "mcp-local-hub", "secrets.age")
	return keyPath, vaultPath
}

func TestSecretsInit_IdempotentOnExistingVault(t *testing.T) {
	_, _ = secretsTestEnv(t)
	a := NewAPI()

	res1, err := a.SecretsInit()
	if err != nil {
		t.Fatalf("first SecretsInit: %v", err)
	}
	if res1.VaultState != "ok" {
		t.Errorf("first init vault_state = %q, want %q", res1.VaultState, "ok")
	}

	res2, err := a.SecretsInit()
	if err != nil {
		t.Fatalf("second SecretsInit: %v", err)
	}
	if res2.VaultState != "ok" {
		t.Errorf("second init vault_state = %q, want %q", res2.VaultState, "ok")
	}
	if res2.Code != "" || res2.CleanupStatus != "" {
		t.Errorf("idempotent path leaked extra fields: %+v", res2)
	}
}
```

- [ ] **Step 1.C.3: Run test to verify failure**

Run: `go test ./internal/api/ -run TestSecretsInit_IdempotentOnExistingVault -count=1 -v`
Expected: FAIL with `a.SecretsInit undefined`.

- [ ] **Step 1.C.4: Implement `SecretsInit` minimally**

Append to `internal/api/secrets.go`:

```go
import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"mcp-local-hub/internal/secrets"
)

// SecretsInitFailed is the typed error SecretsInit returns when
// InitVault failed mid-way and the wrapper attempted cleanup.
// Promoted to exported up front (plan-R2 P1) so Task 1 tests compile;
// fields are populated per memo §5.1 case 2b/2c.
type SecretsInitFailed struct {
	CleanupStatus string // "ok" | "failed"
	OrphanPath    string // populated only on cleanup-failed
	Cause         error
}

func (e *SecretsInitFailed) Error() string { return e.Cause.Error() }

// SecretsInit implements the D2 four-case classifier (memo §5.1):
//   case 1 → 200 ok, no-op
//   case 2a → 200 ok, fresh init
//   case 2b → returns secretsInitFailed{cleanup_status:"ok"}, handler maps to 200
//   case 2c → returns secretsInitFailed{cleanup_status:"failed"}, handler maps to 500
//   cases 3/4 → returns 409-style typed errors via wrapper return value
func (a *API) SecretsInit() (SecretsInitResult, error) {
	keyPath := secrets.DefaultKeyPath()
	vaultPath := secrets.DefaultVaultPath()

	// Case 1: vault already opens cleanly → idempotent no-op.
	if v, err := secrets.OpenVault(keyPath, vaultPath); err == nil {
		_ = v
		return SecretsInitResult{VaultState: "ok"}, nil
	}

	keyExists := fileExists(keyPath)
	vaultExists := fileExists(vaultPath)

	// Cases 3 and 4: pre-existing files we did not create. Refuse.
	if keyExists || vaultExists {
		return SecretsInitResult{}, &SecretsInitBlocked{
			KeyExists:   keyExists,
			VaultExists: vaultExists,
		}
	}

	// Case 2: both missing. Ensure parent dir exists (Codex memo-R8 P1:
	// secrets.InitVault does not MkdirAll itself; CLI does it explicitly).
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return SecretsInitResult{}, fmt.Errorf("create vault dir: %w", err)
	}

	if err := initVaultFn(keyPath, vaultPath); err != nil {
		// Partial init: clean up whatever InitVault may have created.
		// Order: vault first (because the key file alone is benign;
		// an orphan vault is the harder-to-explain artifact).
		cleanupOK := true
		var orphan string
		if rmErr := os.Remove(vaultPath); rmErr != nil && !os.IsNotExist(rmErr) {
			cleanupOK = false
			orphan = vaultPath
		}
		if rmErr := os.Remove(keyPath); rmErr != nil && !os.IsNotExist(rmErr) {
			cleanupOK = false
			if orphan == "" {
				orphan = keyPath
			}
		}
		if cleanupOK {
			return SecretsInitResult{}, &SecretsInitFailed{
				CleanupStatus: "ok",
				Cause:         err,
			}
		}
		return SecretsInitResult{}, &SecretsInitFailed{
			CleanupStatus: "failed",
			OrphanPath:    orphan,
			Cause:         err,
		}
	}
	return SecretsInitResult{VaultState: "ok"}, nil
}

// SecretsInitBlocked is the typed error for D2 cases 3 and 4 (pre-existing
// orphan or unreadable vault). Handler maps to 409 + SECRETS_INIT_BLOCKED.
type SecretsInitBlocked struct {
	KeyExists   bool
	VaultExists bool
}

func (e *SecretsInitBlocked) Error() string {
	switch {
	case e.KeyExists && e.VaultExists:
		return "vault and key files exist but cannot be opened"
	case e.KeyExists:
		return "orphan key file exists; vault file missing"
	case e.VaultExists:
		return "orphan vault file exists; key file missing"
	default:
		return "init blocked"
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// secretNameRE allows lowercase identifiers (memo §5.3 Codex memo-R8 P1:
// repo ships `secret:wolfram_app_id` and `secret:unpaywall_email`).
var secretNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
```

You also need to remove the unused `"errors"` import if it's not used. (It will be used in later wrappers; keep it for now and `go vet` later.) If `go build` complains, drop it temporarily.

- [ ] **Step 1.C.5: Verify idempotent test passes**

Run: `go test ./internal/api/ -run TestSecretsInit_IdempotentOnExistingVault -count=1 -v`
Expected: PASS.

- [ ] **Step 1.C.6: Add fresh-init test**

Append to `internal/api/secrets_test.go`:

```go
func TestSecretsInit_FreshInitCreatesFiles(t *testing.T) {
	keyPath, vaultPath := secretsTestEnv(t)
	a := NewAPI()

	if fileExists(keyPath) || fileExists(vaultPath) {
		t.Fatalf("test setup leak: files already exist")
	}

	res, err := a.SecretsInit()
	if err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	if res.VaultState != "ok" {
		t.Errorf("vault_state = %q, want ok", res.VaultState)
	}
	if !fileExists(keyPath) {
		t.Error("key file not created")
	}
	if !fileExists(vaultPath) {
		t.Error("vault file not created")
	}
}
```

Run: `go test ./internal/api/ -run TestSecretsInit_FreshInitCreatesFiles -count=1 -v`
Expected: PASS.

- [ ] **Step 1.C.6b: Add R7 partial-init test seam to `secrets.go` AND export the failure type**

**Codex plan-R1 P1 + plan-R2 P1:** the wrapper needs a deterministic way to inject an InitVault failure mid-write, AND `SecretsInitFailed` must be EXPORTED from the start (was deferred to Task 3 in the original draft, which broke compile order — Task 1 tests reference the exported name). Promote both up-front:

In `internal/api/secrets.go`, replace the unexported `secretsInitFailed` block (added in Step 1.C.4) with:

```go
// SecretsInitFailed is the typed error SecretsInit returns when
// InitVault failed mid-way and the wrapper attempted cleanup. The
// handler inspects CleanupStatus to map to 200 (cleanup ok, retryable)
// vs 500 (cleanup failed; orphan_path requires manual removal). Memo
// §5.1 + R7. Promoted to exported up front (plan-R2 P1) so Task 1
// tests compile.
type SecretsInitFailed struct {
	CleanupStatus string // "ok" | "failed"
	OrphanPath    string // populated only on cleanup-failed
	Cause         error
}

func (e *SecretsInitFailed) Error() string { return e.Cause.Error() }

// initVaultFn is the function the wrapper calls to perform the
// underlying init. Tests override this to inject failures and verify
// cleanup behavior (memo R7).
var initVaultFn = secrets.InitVault
```

Update every `&secretsInitFailed{...}` reference in `SecretsInit` to `&SecretsInitFailed{...}`. Then in the same function body, replace the InitVault call:

```go
// Old: if err := secrets.InitVault(keyPath, vaultPath); err != nil {
if err := initVaultFn(keyPath, vaultPath); err != nil {
```

Note: this means Task 3's "secretsInitFailed → SecretsInitFailed promotion" is no-op (already exported); the handler in Task 3 just consumes the exported type directly.

- [ ] **Step 1.C.6c: Add three R7 partial-init tests**

```go
func TestSecretsInit_PartialFailureCleansBothArtifacts(t *testing.T) {
	keyPath, vaultPath := secretsTestEnv(t)
	a := NewAPI()

	// Inject: simulate "key written, vault write failed" — write the key
	// ourselves, write a partial vault, then return error.
	initVaultFn = func(kp, vp string) error {
		if err := os.MkdirAll(filepath.Dir(kp), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(kp, []byte("AGE-SECRET-KEY-FAKE\n"), 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(vp, []byte("partial"), 0o600); err != nil {
			return err
		}
		return fmt.Errorf("simulated mid-init failure")
	}
	defer func() { initVaultFn = secrets.InitVault }()

	_, err := a.SecretsInit()
	var initFailed *SecretsInitFailed
	if !errors.As(err, &initFailed) {
		t.Fatalf("err = %T %v, want *SecretsInitFailed", err, err)
	}
	if initFailed.CleanupStatus != "ok" {
		t.Errorf("cleanup_status = %q, want ok", initFailed.CleanupStatus)
	}
	if fileExists(keyPath) {
		t.Errorf("orphan key file %s still exists after cleanup", keyPath)
	}
	if fileExists(vaultPath) {
		t.Errorf("orphan vault file %s still exists after cleanup", vaultPath)
	}
}

func TestSecretsInit_PartialFailureKeyOnly(t *testing.T) {
	keyPath, vaultPath := secretsTestEnv(t)
	a := NewAPI()

	// Simulate "key written, vault never created".
	initVaultFn = func(kp, vp string) error {
		if err := os.MkdirAll(filepath.Dir(kp), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(kp, []byte("AGE-SECRET-KEY-FAKE\n"), 0o600); err != nil {
			return err
		}
		return fmt.Errorf("simulated key-only failure")
	}
	defer func() { initVaultFn = secrets.InitVault }()

	_, err := a.SecretsInit()
	var initFailed *SecretsInitFailed
	if !errors.As(err, &initFailed) {
		t.Fatalf("err = %T %v, want *SecretsInitFailed", err, err)
	}
	if initFailed.CleanupStatus != "ok" {
		t.Errorf("cleanup_status = %q, want ok (vault never existed, key removed)", initFailed.CleanupStatus)
	}
	if fileExists(keyPath) {
		t.Errorf("orphan key file %s still exists after cleanup", keyPath)
	}
	_ = vaultPath
}

func TestSecretsInit_PartialFailureCleanupAlsoFails(t *testing.T) {
	keyPath, _ := secretsTestEnv(t)
	a := NewAPI()

	// Simulate "key written" then make the parent directory read-only
	// so os.Remove fails. After the test we restore perms so t.TempDir
	// cleanup works.
	parent := filepath.Dir(keyPath)
	initVaultFn = func(kp, vp string) error {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(kp, []byte("AGE-SECRET-KEY-FAKE\n"), 0o600); err != nil {
			return err
		}
		// Lock the parent so os.Remove(kp) inside the wrapper fails.
		if err := os.Chmod(parent, 0o500); err != nil {
			return err
		}
		return fmt.Errorf("simulated mid-init failure with locked parent")
	}
	defer func() {
		initVaultFn = secrets.InitVault
		_ = os.Chmod(parent, 0o700) // restore so t.TempDir cleanup works
	}()

	_, err := a.SecretsInit()
	var initFailed *SecretsInitFailed
	if !errors.As(err, &initFailed) {
		t.Fatalf("err = %T %v, want *SecretsInitFailed", err, err)
	}
	if initFailed.CleanupStatus != "failed" {
		// On Windows, chmod 0o500 may not actually deny os.Remove for
		// the owner. Skip this check on platforms that don't enforce
		// the read-only-dir-blocks-remove invariant.
		if runtime.GOOS == "windows" {
			t.Skip("skipping cleanup-failed assertion on Windows: chmod 0o500 doesn't block owner Remove")
		}
		t.Errorf("cleanup_status = %q, want failed", initFailed.CleanupStatus)
	}
	if initFailed.OrphanPath == "" && runtime.GOOS != "windows" {
		t.Errorf("orphan_path empty, want non-empty when cleanup failed")
	}
}
```

Add `"runtime"` and `"fmt"` to the test file imports if missing.

- [ ] **Step 1.C.7: Add orphan-key test (case 4)**

Append:

```go
func TestSecretsInit_OrphanKey(t *testing.T) {
	keyPath, _ := secretsTestEnv(t)
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewAPI()
	_, err := a.SecretsInit()
	var blocked *SecretsInitBlocked
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %T %v, want *SecretsInitBlocked", err, err)
	}
	if !blocked.KeyExists || blocked.VaultExists {
		t.Errorf("blocked = %+v, want KeyExists=true VaultExists=false", *blocked)
	}
}
```

Add `"errors"` to test file imports if needed.

Run: `go test ./internal/api/ -run TestSecretsInit_OrphanKey -count=1 -v`
Expected: PASS.

- [ ] **Step 1.C.8: Implement `SecretsListWithUsage` (TDD)**

Add the failing test first:

```go
func TestSecretsListWithUsage_OkVault(t *testing.T) {
	_, _ = secretsTestEnv(t)
	manifestDir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)
	if err := os.MkdirAll(filepath.Join(manifestDir, "alpha"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "alpha", "manifest.yaml"),
		[]byte("name: alpha\nenv:\n  OPENAI_API_KEY: secret:K1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewAPI()
	if _, err := a.SecretsInit(); err != nil {
		t.Fatal(err)
	}
	if err := a.SecretsSet("K1", "value-1"); err != nil {
		t.Fatal(err)
	}
	if err := a.SecretsSet("K_orphan", "value-orphan"); err != nil {
		t.Fatal(err)
	}

	env, err := a.SecretsListWithUsage()
	if err != nil {
		t.Fatalf("SecretsListWithUsage: %v", err)
	}
	if env.VaultState != "ok" {
		t.Errorf("vault_state = %q", env.VaultState)
	}
	if len(env.Secrets) != 2 {
		t.Fatalf("len(secrets) = %d, want 2 (K1 present + K_orphan present)", len(env.Secrets))
	}
	// Sorted alphabetically: K1, K_orphan.
	if env.Secrets[0].Name != "K1" || env.Secrets[0].State != "present" {
		t.Errorf("secrets[0] = %+v", env.Secrets[0])
	}
	if len(env.Secrets[0].UsedBy) != 1 || env.Secrets[0].UsedBy[0].Server != "alpha" || env.Secrets[0].UsedBy[0].EnvVar != "OPENAI_API_KEY" {
		t.Errorf("secrets[0].used_by = %+v", env.Secrets[0].UsedBy)
	}
	if env.Secrets[1].Name != "K_orphan" || env.Secrets[1].State != "present" {
		t.Errorf("secrets[1] = %+v", env.Secrets[1])
	}
	if len(env.Secrets[1].UsedBy) != 0 {
		t.Errorf("secrets[1].used_by should be empty")
	}
}
```

This test currently fails because `SecretsSet` and `SecretsListWithUsage` are undefined. Add minimal implementations:

```go
// In internal/api/secrets.go, append:

// SecretsSet writes a key/value pair to the vault. Wrapper for vault.Set.
// Validates name regex and non-empty value (memo §5.3). Returns typed
// errors the handler maps to 400/409.
func (a *API) SecretsSet(name, value string) error {
	if !secretNameRE.MatchString(name) {
		return &SecretsSetError{Code: "SECRETS_INVALID_NAME", Msg: fmt.Sprintf("name %q does not match %s", name, secretNameRE.String())}
	}
	if value == "" {
		return &SecretsSetError{Code: "SECRETS_EMPTY_VALUE", Msg: "value must not be empty"}
	}
	v, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		return &SecretsSetError{Code: "SECRETS_VAULT_NOT_INITIALIZED", Msg: err.Error()}
	}
	if _, getErr := v.Get(name); getErr == nil {
		return &SecretsSetError{Code: "SECRETS_KEY_EXISTS", Msg: fmt.Sprintf("secret %q already exists; use Rotate to update", name)}
	}
	if err := v.Set(name, value); err != nil {
		return &SecretsSetError{Code: "SECRETS_SET_FAILED", Msg: err.Error()}
	}
	return nil
}

// SecretsSetError is the typed error from SecretsSet. The handler maps
// Code → HTTP status (memo §5.7).
type SecretsSetError struct {
	Code string
	Msg  string
}

func (e *SecretsSetError) Error() string { return e.Msg }

// SecretsListWithUsage builds the registry envelope (memo §5.2).
func (a *API) SecretsListWithUsage() (SecretsEnvelope, error) {
	usage, manifestErrs, err := ScanManifestEnv()
	if err != nil {
		return SecretsEnvelope{}, fmt.Errorf("scan manifests: %w", err)
	}
	if manifestErrs == nil {
		manifestErrs = []ManifestError{}
	}

	keyPath := secrets.DefaultKeyPath()
	vaultPath := secrets.DefaultVaultPath()

	state, keys := classifyVault(keyPath, vaultPath)

	rows := buildSecretRows(state, keys, usage)
	return SecretsEnvelope{
		VaultState:     state,
		Secrets:        rows,
		ManifestErrors: manifestErrs,
	}, nil
}

// classifyVault maps OpenVault outcomes to the four-state vault model
// (memo §5.2). Returns (state, keys); keys is non-nil only when state == "ok".
//
// Codex plan-R1 P3: capture the first OpenVault error and re-use it
// instead of calling OpenVault twice (the second call is redundant).
func classifyVault(keyPath, vaultPath string) (string, []string) {
	v, err := secrets.OpenVault(keyPath, vaultPath)
	if err == nil {
		return "ok", v.List()
	}
	keyExists := fileExists(keyPath)
	vaultExists := fileExists(vaultPath)
	if !keyExists || !vaultExists {
		return "missing", nil
	}
	// Files exist but OpenVault failed: distinguish corrupt (key parse
	// error or post-decrypt JSON garbage) from decrypt_failed (key fine,
	// age decrypt rejected the cipher) by inspecting the captured error
	// string. Brittle but acceptable for the GET path.
	msg := err.Error()
	switch {
	case containsAny(msg, "parse identity", "no identity", "not X25519"):
		return "corrupt", nil
	case containsAny(msg, "unmarshal vault"):
		return "corrupt", nil
	case containsAny(msg, "age decrypt"):
		return "decrypt_failed", nil
	default:
		return "corrupt", nil
	}
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// buildSecretRows merges vault keys with manifest usage into the row
// slice. Sorted alphabetically by name.
func buildSecretRows(vaultState string, keys []string, usage map[string][]UsageRef) []SecretRow {
	keySet := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		keySet[k] = struct{}{}
	}

	var rows []SecretRow
	switch vaultState {
	case "ok":
		for _, k := range keys {
			rows = append(rows, SecretRow{Name: k, State: "present", UsedBy: nonNilUsage(usage[k])})
		}
		for k, refs := range usage {
			if _, ok := keySet[k]; ok {
				continue
			}
			rows = append(rows, SecretRow{Name: k, State: "referenced_missing", UsedBy: refs})
		}
	default:
		// Vault unavailable; only manifest-only rows, all unverified.
		for k, refs := range usage {
			rows = append(rows, SecretRow{Name: k, State: "referenced_unverified", UsedBy: refs})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows
}

func nonNilUsage(in []UsageRef) []UsageRef {
	if in == nil {
		return []UsageRef{}
	}
	return in
}
```

Add `"strings"` and `"sort"` to the file imports.

- [ ] **Step 1.C.9: Run the OK-vault list test**

Run: `go test ./internal/api/ -run TestSecretsListWithUsage_OkVault -count=1 -v`
Expected: PASS.

- [ ] **Step 1.C.10: Add referenced-missing test**

Append:

```go
func TestSecretsListWithUsage_ReferencedMissingWhenVaultOk(t *testing.T) {
	_, _ = secretsTestEnv(t)
	manifestDir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)
	if err := os.MkdirAll(filepath.Join(manifestDir, "alpha"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "alpha", "manifest.yaml"),
		[]byte("name: alpha\nenv:\n  WOLFRAM: secret:K_missing\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewAPI()
	if _, err := a.SecretsInit(); err != nil {
		t.Fatal(err)
	}
	env, err := a.SecretsListWithUsage()
	if err != nil {
		t.Fatal(err)
	}
	if env.VaultState != "ok" {
		t.Errorf("vault_state = %q", env.VaultState)
	}
	if len(env.Secrets) != 1 || env.Secrets[0].State != "referenced_missing" || env.Secrets[0].Name != "K_missing" {
		t.Errorf("secrets = %+v", env.Secrets)
	}
}
```

Run: `go test ./internal/api/ -run TestSecretsListWithUsage_ReferencedMissingWhenVaultOk -count=1 -v`
Expected: PASS.

- [ ] **Step 1.C.11: Add unverified-when-vault-missing test**

```go
func TestSecretsListWithUsage_UnverifiedWhenVaultMissing(t *testing.T) {
	_, _ = secretsTestEnv(t)
	manifestDir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)
	if err := os.MkdirAll(filepath.Join(manifestDir, "alpha"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "alpha", "manifest.yaml"),
		[]byte("name: alpha\nenv:\n  OPENAI: secret:K1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Note: do NOT call SecretsInit. Vault should be missing.
	a := NewAPI()

	env, err := a.SecretsListWithUsage()
	if err != nil {
		t.Fatal(err)
	}
	if env.VaultState != "missing" {
		t.Errorf("vault_state = %q, want missing", env.VaultState)
	}
	if len(env.Secrets) != 1 || env.Secrets[0].State != "referenced_unverified" {
		t.Errorf("secrets[0] = %+v, want referenced_unverified", env.Secrets[0])
	}
}
```

Run: PASS.

- [ ] **Step 1.C.12: Implement `SecretsRotate` and `SecretsRestart` (TDD-lite)**

Append the implementation directly (these are mostly delegations; tests in Task 1.C.13):

```go
// SecretsRotate writes value into the vault and optionally restarts the
// affected running daemons. Returns SecretsRotateResult; if restart was
// requested and orchestration crashed mid-loop, returns
// (partial-result, err) so the handler can map to 500 + RESTART_FAILED
// while still surfacing vault_updated:true.
func (a *API) SecretsRotate(name, value string, restart bool) (SecretsRotateResult, error) {
	if !secretNameRE.MatchString(name) {
		return SecretsRotateResult{}, &SecretsSetError{Code: "SECRETS_INVALID_NAME", Msg: fmt.Sprintf("name %q does not match %s", name, secretNameRE.String())}
	}
	if value == "" {
		return SecretsRotateResult{}, &SecretsSetError{Code: "SECRETS_EMPTY_VALUE", Msg: "value must not be empty"}
	}
	v, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		return SecretsRotateResult{}, &SecretsSetError{Code: "SECRETS_VAULT_NOT_INITIALIZED", Msg: err.Error()}
	}
	if _, getErr := v.Get(name); getErr != nil {
		return SecretsRotateResult{}, &SecretsSetError{Code: "SECRETS_KEY_NOT_FOUND", Msg: getErr.Error()}
	}
	if err := v.Set(name, value); err != nil {
		return SecretsRotateResult{}, &SecretsSetError{Code: "SECRETS_SET_FAILED", Msg: err.Error()}
	}

	res := SecretsRotateResult{VaultUpdated: true, RestartResults: []RestartResult{}}
	if !restart {
		return res, nil
	}
	results, err := a.restartServersForKey(name)
	res.RestartResults = results
	return res, err
}

// SecretsRestart runs the restart phase only — used by POST /api/secrets/:key/restart.
// Does NOT modify the vault.
func (a *API) SecretsRestart(name string) ([]RestartResult, error) {
	v, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		return nil, &SecretsSetError{Code: "SECRETS_VAULT_NOT_INITIALIZED", Msg: err.Error()}
	}
	if _, getErr := v.Get(name); getErr != nil {
		return nil, &SecretsSetError{Code: "SECRETS_KEY_NOT_FOUND", Msg: getErr.Error()}
	}
	return a.restartServersForKey(name)
}

// restartServersForKey iterates manifests, finds (server, daemon) pairs
// whose env references the key AND whose daemon status is "Running",
// and calls api.Restart(server, daemonName) per-daemon (memo §D9).
// Returns accumulated []RestartResult plus a non-nil error on the first
// orchestration failure (per-task failures are non-fatal).
//
// Codex plan-R2 P2: returns []RestartResult{} (empty slice, not nil)
// even on early errors so callers and the handler can serialize a
// well-formed JSON array per the memo wire contract.
func (a *API) restartServersForKey(key string) ([]RestartResult, error) {
	results := []RestartResult{}
	usage, _, err := ScanManifestEnv()
	if err != nil {
		return results, fmt.Errorf("scan manifests: %w", err)
	}
	refs := usage[key]
	if len(refs) == 0 {
		return results, nil
	}
	statuses, err := a.Status()
	if err != nil {
		return results, fmt.Errorf("read daemon status: %w", err)
	}
	runningByServer := make(map[string]map[string]bool) // server → daemon → running?
	for _, st := range statuses {
		if st.Server == "" || st.Daemon == "" {
			continue
		}
		if runningByServer[st.Server] == nil {
			runningByServer[st.Server] = map[string]bool{}
		}
		runningByServer[st.Server][st.Daemon] = (st.State == "Running") // Codex plan-R1 P1: capital R per types.go:21
	}

	// Determine the affected (server, daemon) set. Each manifest may
	// have multiple daemons; we must restart all running daemons of
	// each affected server because the env is shared across the
	// server's daemons in the current schema.
	//
	// Codex plan-R2 P2: de-duplicate on (server, daemon) so a single
	// secret referenced via multiple env vars in one manifest does
	// NOT trigger duplicate restarts.
	type sd struct{ server, daemon string }
	seen := map[sd]bool{}
	for _, ref := range refs {
		daemons := runningByServer[ref.Server]
		for daemon, running := range daemons {
			if !running {
				continue
			}
			pair := sd{ref.Server, daemon}
			if seen[pair] {
				continue
			}
			seen[pair] = true
			subres, restartErr := a.Restart(ref.Server, daemon)
			results = append(results, subres...)
			if restartErr != nil {
				return results, fmt.Errorf("restart %s/%s: %w", ref.Server, daemon, restartErr)
			}
		}
	}
	return results, nil
}
```

Note: `a.Status()` already exists in `internal/api`. `a.Restart(server, daemon)` already exists at `internal/api/install.go:1335`. Both are reused; no new public API.

- [ ] **Step 1.C.13: Test rotate happy path**

```go
func TestSecretsRotate_OverwritesExisting(t *testing.T) {
	_, _ = secretsTestEnv(t)
	a := NewAPI()
	if _, err := a.SecretsInit(); err != nil {
		t.Fatal(err)
	}
	if err := a.SecretsSet("K1", "old"); err != nil {
		t.Fatal(err)
	}
	res, err := a.SecretsRotate("K1", "new", false)
	if err != nil {
		t.Fatalf("SecretsRotate: %v", err)
	}
	if !res.VaultUpdated {
		t.Error("vault_updated = false")
	}
	if len(res.RestartResults) != 0 {
		t.Errorf("restart_results = %+v, want empty (restart=false)", res.RestartResults)
	}
}
```

Run: PASS.

- [ ] **Step 1.C.14: Implement `SecretsDelete`**

Append:

```go
// SecretsDelete enforces the D5 escalation guard. With confirm=false,
// returns *SecretsDeleteError when refs exist or scan was incomplete;
// returns nil on successful delete. With confirm=true, bypasses both
// guards.
func (a *API) SecretsDelete(name string, confirm bool) error {
	v, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		return &SecretsSetError{Code: "SECRETS_VAULT_NOT_INITIALIZED", Msg: err.Error()}
	}
	if _, getErr := v.Get(name); getErr != nil {
		return &SecretsSetError{Code: "SECRETS_KEY_NOT_FOUND", Msg: getErr.Error()}
	}
	if !confirm {
		usage, manifestErrs, scanErr := ScanManifestEnv()
		if scanErr != nil {
			return fmt.Errorf("scan manifests: %w", scanErr)
		}
		// Precedence per §5.5: scan-incomplete BEFORE refs.
		if len(manifestErrs) > 0 {
			return &SecretsDeleteError{
				Code:           "SECRETS_USAGE_SCAN_INCOMPLETE",
				Message:        fmt.Sprintf("manifest scan returned %d error(s); cannot verify refs", len(manifestErrs)),
				ManifestErrors: manifestErrs,
			}
		}
		if refs := usage[name]; len(refs) > 0 {
			return &SecretsDeleteError{
				Code:    "SECRETS_HAS_REFS",
				Message: fmt.Sprintf("secret %q is referenced by %d manifest(s)", name, len(refs)),
				UsedBy:  refs,
			}
		}
	}
	if err := v.Delete(name); err != nil {
		return &SecretsSetError{Code: "SECRETS_DELETE_FAILED", Msg: err.Error()}
	}
	return nil
}
```

- [ ] **Step 1.C.15: Test delete refuses with refs**

```go
func TestSecretsDelete_RequiresConfirmWithRefs(t *testing.T) {
	_, _ = secretsTestEnv(t)
	manifestDir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)
	if err := os.MkdirAll(filepath.Join(manifestDir, "alpha"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "alpha", "manifest.yaml"),
		[]byte("name: alpha\nenv:\n  OPENAI: secret:K1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	if _, err := a.SecretsInit(); err != nil {
		t.Fatal(err)
	}
	if err := a.SecretsSet("K1", "v"); err != nil {
		t.Fatal(err)
	}
	err := a.SecretsDelete("K1", false)
	var de *SecretsDeleteError
	if !errors.As(err, &de) {
		t.Fatalf("err = %T %v, want *SecretsDeleteError", err, err)
	}
	if de.Code != "SECRETS_HAS_REFS" {
		t.Errorf("code = %q", de.Code)
	}
	if len(de.UsedBy) != 1 || de.UsedBy[0].Server != "alpha" || de.UsedBy[0].EnvVar != "OPENAI" {
		t.Errorf("used_by = %+v", de.UsedBy)
	}
}
```

Run: PASS.

- [ ] **Step 1.C.16: Test delete with confirm bypasses guard**

```go
func TestSecretsDelete_WithConfirmBypassesRefs(t *testing.T) {
	_, _ = secretsTestEnv(t)
	manifestDir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)
	if err := os.MkdirAll(filepath.Join(manifestDir, "alpha"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "alpha", "manifest.yaml"),
		[]byte("name: alpha\nenv:\n  OPENAI: secret:K1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	if _, err := a.SecretsInit(); err != nil {
		t.Fatal(err)
	}
	if err := a.SecretsSet("K1", "v"); err != nil {
		t.Fatal(err)
	}
	if err := a.SecretsDelete("K1", true); err != nil {
		t.Fatalf("SecretsDelete confirm=true: %v", err)
	}
}
```

Run: PASS.

### 1.D — LWW work-items entry

- [ ] **Step 1.D.1: Create `work-items/bugs/a3a-vault-concurrent-edit-lww.md`**

```markdown
---
status: open
context: A3-a Secrets registry — concurrent vault edits limitation
phase: Phase 3B-II A3-a
---

# A3-a — Vault concurrent edits are last-write-wins

The age-encrypted vault has no version field analogous to manifests'
`expected_hash`. Two GUI tabs (or a GUI tab + CLI invocation) racing
on the same vault produce last-write-wins semantics. Ship A3-a with
this known limitation; future work can add an optimistic-concurrency
hash field analogous to manifest's `expected_hash`.

**Failure mode:**
- Tab A reads snapshot at T=0 (vault has `{KEY1:"oldA"}`).
- Tab B reads same snapshot at T=1.
- Tab A rotates `KEY1` to `"newA"` at T=2.
- Tab B rotates `KEY1` to `"newB"` at T=3, unaware of A's change.
- Vault now `{KEY1:"newB"}`. Tab A's `"newA"` is silently lost.

**Why we accept this for A3-a:** the failure is operator-recoverable
(the user can re-rotate). The cost of a proper fix (vault hash field,
bump on every Set, 409 on mismatch, CLI must thread the hash too) is
comparable to a small feature in itself; out of A3-a scope.

**Future fix candidates:**
- Add a `vault_version` integer header in the encrypted body; Set
  bumps it; PUT/DELETE requires `If-Match: <version>`; 409 on
  mismatch.
- CLI continues to no-op on the version field (always passes the
  fresh value), or grows the same `--if-version` flag.
- E2E test for two-tab conflict.

**Out of A3-a scope.**
```

### 1.E — Run all tests + commit

- [ ] **Step 1.E.1: Run full Task 1 suite**

```bash
go build ./...
go vet ./...
go test ./internal/api/ -run "TestScanManifestEnv|TestSecretsInit|TestSecretsListWithUsage|TestSecretsSet|TestSecretsRotate|TestSecretsDelete" -count=1 -v
```

Expected: all green; ~15 tests pass.

- [ ] **Step 1.E.2: Run wider regression to confirm nothing broke**

```bash
go test ./... -count=1
```

Expected: green.

- [ ] **Step 1.E.3: Commit**

```bash
git add internal/api/secrets.go internal/api/secrets_scan.go internal/api/secrets_test.go internal/api/secrets_scan_test.go internal/api/install.go internal/api/manifest_source.go work-items/bugs/a3a-vault-concurrent-edit-lww.md
git commit -m "feat(api): A3-a backend scaffold — Secrets wrappers + scan + LWW work-item

Adds api.Secrets{Init,ListWithUsage,Set,Rotate,Restart,Delete} wrappers
plus ScanManifestEnv helper for the Used-by aggregation, all in
internal/api (avoids import cycle with internal/secrets per memo
§D10/R8). RestartResult JSON tags task_name/error (NOT omitempty per
memo D9) added as D9 prerequisite.

Memo: docs/superpowers/specs/2026-04-25-phase-3b-ii-a3a-secrets-screen-design.md"
```

---

## Task 2: D9 restart-result granularity refactor + Dashboard.tsx consumer update

**Goal:** Refactor `realRestarter.Restart` from aggregated-error to `([]api.RestartResult, error)`, update the existing `/api/servers/:name/restart` handler to emit 200/207/500 with the per-task body, and update `Dashboard.tsx` to surface partial failures (memo §D9, Codex memo-R8 P1).

**Files:**
- Modify: `internal/gui/server.go` — `restarter` interface signature, `realRestarter` impl
- Modify: `internal/gui/servers.go:9-35` — handler returns 200/207/500
- Modify: `internal/gui/servers_test.go` — existing tests for new shape
- Modify: `internal/gui/frontend/src/screens/Dashboard.tsx:73-76` — inspect `restart_results[].error`
- Modify: `internal/gui/frontend/src/screens/Dashboard.test.tsx` (if exists) or add Vitest coverage for the new flow

### 2.A — Backend interface refactor

- [ ] **Step 2.A.1: Update `restarter` interface in `internal/gui/server.go`**

Find the existing `restarter` interface block (search for `type restarter interface`). Replace:

```go
type restarter interface {
	Restart(server string) error
}
```

with:

```go
// restarter is the narrow interface the /api/servers/:name/restart
// handler needs. Per memo D9 (Codex R8 P1), it now returns the
// per-task RestartResult slice (existing api.RestartResult{TaskName, Err}
// shape) plus an orchestration-level error. Handler maps:
//   results all empty Err  → 200 {restart_results:[…]}
//   results any non-empty  → 207 {restart_results:[…]}
//   err != nil             → 500 + RESTART_FAILED, body has partial
//                            results (memo §D9).
type restarter interface {
	Restart(server string) ([]api.RestartResult, error)
}
```

- [ ] **Step 2.A.2: Update `realRestarter` impl**

Find the existing `realRestarter` block. Replace its body with:

```go
type realRestarter struct{}

func (realRestarter) Restart(server string) ([]api.RestartResult, error) {
	results, err := api.NewAPI().Restart(server, "")
	if results == nil {
		results = []api.RestartResult{}
	}
	return results, err
}
```

(Remove the old aggregated-error glue — the handler now does the aggregation itself.)

- [ ] **Step 2.A.3: Update the handler in `internal/gui/servers.go`**

Replace the existing restart-handler body (`case "restart":` block, around lines 22-32):

```go
		case "restart":
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			results, err := s.restart.Restart(name)
			if results == nil {
				results = []api.RestartResult{}
			}
			body := map[string]any{"restart_results": results}
			if err != nil {
				body["error"] = err.Error()
				body["code"] = "RESTART_FAILED"
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(body)
				return
			}
			anyFailed := false
			for _, r := range results {
				if r.Err != "" {
					anyFailed = true
					break
				}
			}
			status := http.StatusOK
			if anyFailed {
				status = http.StatusMultiStatus // 207
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(body)
```

If `encoding/json` is not yet imported at the top of `servers.go`, add it.

- [ ] **Step 2.A.4: Update existing `internal/gui/servers_test.go` for new shape**

Search existing tests that call the restart handler — typically they assert `w.Code == 204` or `204` no-content, and check that the mock was invoked. They will fail compilation if the mock returns the old `error`-only shape.

Find existing mocks like:

```go
type fakeRestarter struct{ called string; err error }
func (f *fakeRestarter) Restart(server string) error { f.called = server; return f.err }
```

Replace with:

```go
type fakeRestarter struct {
	called  string
	results []api.RestartResult
	err     error
}

func (f *fakeRestarter) Restart(server string) ([]api.RestartResult, error) {
	f.called = server
	return f.results, f.err
}
```

Then update each test that asserted `204 No Content` to assert `200 OK` with the new body shape:

```go
// Old:
//   if rec.Code != http.StatusNoContent { t.Fatalf("status=%d", rec.Code) }
// New:
if rec.Code != http.StatusOK {
	t.Fatalf("status=%d, body=%q", rec.Code, rec.Body.String())
}
var got map[string]any
if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
	t.Fatalf("body unmarshal: %v", err)
}
if _, ok := got["restart_results"]; !ok {
	t.Errorf("body missing restart_results: %v", got)
}
```

- [ ] **Step 2.A.5: Add new test for 207 partial-failure path**

In `internal/gui/servers_test.go`, append:

```go
func TestRestart_PartialFailureReturns207(t *testing.T) {
	fr := &fakeRestarter{
		results: []api.RestartResult{
			{TaskName: "mcp-local-hub-server-a-default", Err: ""},
			{TaskName: "mcp-local-hub-server-b-default", Err: "scheduler timeout"},
		},
	}
	s := NewServer(Config{})
	s.restart = fr

	req := httptest.NewRequest(http.MethodPost, "/api/servers/server-a/restart", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("status=%d, want 207, body=%q", rec.Code, rec.Body.String())
	}
	var body struct {
		RestartResults []api.RestartResult `json:"restart_results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.RestartResults) != 2 || body.RestartResults[1].Err != "scheduler timeout" {
		t.Errorf("results = %+v", body.RestartResults)
	}
}
```

- [ ] **Step 2.A.6: Add new test for 500 orchestration-failure path**

```go
func TestRestart_OrchestrationFailureReturns500(t *testing.T) {
	fr := &fakeRestarter{
		results: []api.RestartResult{{TaskName: "x", Err: ""}},
		err:     fmt.Errorf("scheduler unavailable"),
	}
	s := NewServer(Config{})
	s.restart = fr

	req := httptest.NewRequest(http.MethodPost, "/api/servers/server-a/restart", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
	var body struct {
		RestartResults []api.RestartResult `json:"restart_results"`
		Error          string              `json:"error"`
		Code           string              `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Code != "RESTART_FAILED" {
		t.Errorf("code=%q", body.Code)
	}
	if len(body.RestartResults) != 1 {
		t.Errorf("partial results dropped: %+v", body.RestartResults)
	}
}
```

- [ ] **Step 2.A.7: Run servers tests**

```bash
go test ./internal/gui/ -run TestRestart -count=1 -v
```

Expected: all PASS (existing + 2 new tests).

### 2.B — Frontend Dashboard.tsx update

- [ ] **Step 2.B.1: Read current `Dashboard.tsx:73-76`**

```bash
sed -n '60,90p' internal/gui/frontend/src/screens/Dashboard.tsx
```

Confirm the existing pattern is:

```ts
const resp = await fetch(`/api/servers/${encodeURIComponent(server)}/restart`, { method: "POST" });
if (!resp.ok) {
  const body = (await resp.json().catch(() => ({}))) as { error?: string };
  throw new Error(body.error ?? String(resp.status));
}
```

- [ ] **Step 2.B.2: Update Dashboard.tsx to surface partial failures**

Replace that block with:

```ts
const resp = await fetch(`/api/servers/${encodeURIComponent(server)}/restart`, { method: "POST" });
const body = (await resp.json().catch(() => ({}))) as {
  error?: string;
  code?: string;
  restart_results?: Array<{ task_name: string; error: string }>;
};
// 500 → orchestration failure, throw
if (resp.status === 500) {
  throw new Error(body.error ?? String(resp.status));
}
// 207 → some tasks failed; surface them as a single error with details
if (resp.status === 207) {
  const failed = (body.restart_results ?? []).filter((r) => r.error !== "");
  const summary = failed
    .map((r) => `${r.task_name}: ${r.error}`)
    .join("; ");
  throw new Error(`partial restart failure: ${summary}`);
}
// 200 → all OK, even if restart_results is non-empty (all empty errors)
if (!resp.ok) {
  throw new Error(body.error ?? String(resp.status));
}
```

The reason we explicitly check `resp.status` instead of `resp.ok`: `Response.ok` is `true` for any 2xx (including 207), which would silently mask partial failures. By inspecting `restart_results[].error` for 207 we surface the per-task failure detail to the operator.

- [ ] **Step 2.B.3: Verify TypeScript compile**

```bash
cd internal/gui/frontend
npm run typecheck
cd ../../..
```

Expected: no errors.

### 2.C — Commit

- [ ] **Step 2.C.1: Run all backend + frontend type checks**

```bash
go build ./...
go test ./internal/gui/ -count=1
cd internal/gui/frontend && npm run typecheck && cd ../../..
```

Expected: green.

- [ ] **Step 2.C.2: Commit**

```bash
git add internal/gui/server.go internal/gui/servers.go internal/gui/servers_test.go internal/gui/frontend/src/screens/Dashboard.tsx
git commit -m "refactor(gui): D9 — restart handler returns []RestartResult, Dashboard surfaces 207

internal/gui/server.go: restarter.Restart() now returns
([]api.RestartResult, error) instead of aggregated error. Handler
maps:
  all-Err-empty results        → 200 + body
  any-Err-non-empty results    → 207 + body (Multi-Status)
  err != nil (orchestration)   → 500 + RESTART_FAILED + partial body

Dashboard.tsx restart click now inspects resp.status (200/207/500)
and surfaces 207 partial failures with per-task detail. The earlier
'!resp.ok' check would have treated 207 as success and silently
masked partial failures (Codex memo-R8 P1).

Refs: memo §D9 + §5.4."
```

---

## Task 3: HTTP handlers + routing for 6 endpoints

**Goal:** Wire the 6 endpoints through `internal/gui/secrets.go` with same-origin enforcement, error-code catalog mapping, and full handler-level test coverage.

**Files:**
- Create: `internal/gui/secrets.go`
- Create: `internal/gui/secrets_test.go`
- Modify: `internal/gui/server.go` — add `s.secrets` adapter, register routes

### 3.A — Adapter and routes

- [ ] **Step 3.A.1: Define `secretsAPI` adapter interface in `server.go`**

Add to `internal/gui/server.go` near the other narrow interfaces:

```go
// secretsAPI is the narrow interface the /api/secrets/* handlers need.
// Wraps api.API methods so tests can inject a fake. Per memo §5.6.
type secretsAPI interface {
	Init() (api.SecretsInitResult, error)
	List() (api.SecretsEnvelope, error)
	Set(name, value string) error
	Rotate(name, value string, restart bool) (api.SecretsRotateResult, error)
	Restart(name string) ([]api.RestartResult, error)
	Delete(name string, confirm bool) error
}

type realSecretsAPI struct{}

func (realSecretsAPI) Init() (api.SecretsInitResult, error)      { return api.NewAPI().SecretsInit() }
func (realSecretsAPI) List() (api.SecretsEnvelope, error)        { return api.NewAPI().SecretsListWithUsage() }
func (realSecretsAPI) Set(name, value string) error              { return api.NewAPI().SecretsSet(name, value) }
func (realSecretsAPI) Rotate(name, value string, restart bool) (api.SecretsRotateResult, error) {
	return api.NewAPI().SecretsRotate(name, value, restart)
}
func (realSecretsAPI) Restart(name string) ([]api.RestartResult, error) { return api.NewAPI().SecretsRestart(name) }
func (realSecretsAPI) Delete(name string, confirm bool) error           { return api.NewAPI().SecretsDelete(name, confirm) }
```

Add field on `Server` struct:

```go
type Server struct {
	// ... existing fields ...
	secrets secretsAPI
}
```

In `NewServer`, initialize `s.secrets = realSecretsAPI{}` alongside the other adapter fields.

In the `registerRoutes` (or equivalent) function inside `server.go`, add:

```go
registerSecretsRoutes(s)
```

- [ ] **Step 3.A.2: Create `internal/gui/secrets.go` with route registration**

```go
// internal/gui/secrets.go
package gui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"mcp-local-hub/internal/api"
)

func registerSecretsRoutes(s *Server) {
	s.mux.HandleFunc("/api/secrets/init", s.requireSameOrigin(s.secretsInitHandler))
	s.mux.HandleFunc("/api/secrets", s.requireSameOrigin(s.secretsListOrAddHandler))
	s.mux.HandleFunc("/api/secrets/", s.requireSameOrigin(s.secretsByKeyHandler))
}
```

Use a single `/api/secrets/` (trailing slash) prefix handler that branches on path components, plus dedicated `/api/secrets/init` and `/api/secrets` (no trailing slash). Note Go's `http.ServeMux` treats `/api/secrets` and `/api/secrets/` as distinct: the trailing-slash one matches all sub-paths.

- [ ] **Step 3.A.3: Implement `secretsInitHandler`**

Append:

```go
func (s *Server) secretsInitHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	res, err := s.secrets.Init()
	if err == nil {
		writeJSON(w, http.StatusOK, res)
		return
	}
	// Classify the error per memo D2.
	var blocked *api.SecretsInitBlocked
	if errors.As(err, &blocked) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": err.Error(),
			"code":  "SECRETS_INIT_BLOCKED",
		})
		return
	}
	// Test-friendly handler: api.SecretsInit's secretsInitFailed is
	// unexported. We detect partial-init by inspecting Error() prefix
	// and CleanupStatus presence in the result; for clean separation
	// the next plan iteration could promote the type. For now, a
	// bare error means generic SECRETS_INIT_FAILED.
	writeJSON(w, http.StatusInternalServerError, map[string]any{
		"error": err.Error(),
		"code":  "SECRETS_INIT_FAILED",
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
```

**Note on `SecretsInitFailed` location**: the type was already promoted to exported in Step 1.C.6b (plan-R2 P1 — original deferral broke Task 1 compile). The handler just consumes the exported type directly. No further `internal/api/secrets.go` edits needed in Task 3.

Now finalize `secretsInitHandler`:

```go
func (s *Server) secretsInitHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	res, err := s.secrets.Init()
	if err == nil {
		writeJSON(w, http.StatusOK, res)
		return
	}
	var blocked *api.SecretsInitBlocked
	if errors.As(err, &blocked) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": err.Error(),
			"code":  "SECRETS_INIT_BLOCKED",
		})
		return
	}
	var initFailed *api.SecretsInitFailed
	if errors.As(err, &initFailed) {
		body := map[string]any{
			"error":          err.Error(),
			"code":           "SECRETS_INIT_FAILED",
			"cleanup_status": initFailed.CleanupStatus,
		}
		if initFailed.OrphanPath != "" {
			body["orphan_path"] = initFailed.OrphanPath
		}
		if initFailed.CleanupStatus == "ok" {
			body["vault_state"] = "missing"
			writeJSON(w, http.StatusOK, body)
			return
		}
		writeJSON(w, http.StatusInternalServerError, body)
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{
		"error": err.Error(),
		"code":  "SECRETS_INIT_FAILED",
	})
}
```

- [ ] **Step 3.A.4: Implement `secretsListOrAddHandler`**

Routes `GET /api/secrets` and `POST /api/secrets`.

```go
func (s *Server) secretsListOrAddHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		env, err := s.secrets.List()
		if err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "SECRETS_LIST_FAILED")
			return
		}
		writeJSON(w, http.StatusOK, env)
	case http.MethodPost:
		var body struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest, "SECRETS_INVALID_JSON")
			return
		}
		if err := s.secrets.Set(body.Name, body.Value); err != nil {
			writeSecretsSetError(w, err)
			return
		}
		w.WriteHeader(http.StatusCreated)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func writeSecretsSetError(w http.ResponseWriter, err error) {
	var setErr *api.SecretsSetError
	if errors.As(err, &setErr) {
		status := setErrorStatus(setErr.Code)
		writeAPIError(w, err, status, setErr.Code)
		return
	}
	writeAPIError(w, err, http.StatusInternalServerError, "SECRETS_SET_FAILED")
}

func setErrorStatus(code string) int {
	switch code {
	case "SECRETS_INVALID_NAME", "SECRETS_EMPTY_VALUE":
		return http.StatusBadRequest
	case "SECRETS_KEY_EXISTS", "SECRETS_VAULT_NOT_INITIALIZED":
		return http.StatusConflict
	case "SECRETS_KEY_NOT_FOUND":
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}
```

- [ ] **Step 3.A.5: Implement `secretsByKeyHandler` (PUT, POST restart, DELETE)**

```go
func (s *Server) secretsByKeyHandler(w http.ResponseWriter, r *http.Request) {
	// Strip the "/api/secrets/" prefix and inspect the remainder.
	rest := strings.TrimPrefix(r.URL.Path, "/api/secrets/")
	if rest == "" || rest == "init" {
		http.NotFound(w, r)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	key := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	switch action {
	case "":
		s.secretsKeyRoot(w, r, key)
	case "restart":
		s.secretsKeyRestart(w, r, key)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) secretsKeyRoot(w http.ResponseWriter, r *http.Request, key string) {
	switch r.Method {
	case http.MethodPut:
		var body struct {
			Value   string `json:"value"`
			Restart bool   `json:"restart"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest, "SECRETS_INVALID_JSON")
			return
		}
		res, err := s.secrets.Rotate(key, body.Value, body.Restart)
		if err != nil {
			// Set/Get-stage errors carry SecretsSetError; orchestration
			// failure is a bare error after vault.Set succeeded.
			var setErr *api.SecretsSetError
			if errors.As(err, &setErr) {
				writeSecretsSetError(w, err)
				return
			}
			// Orchestration failure: vault was updated but restart aborted.
			// Codex plan-R2 P2: normalize nil restart_results to empty
			// array so the wire contract always carries a JSON array.
			results := res.RestartResults
			if results == nil {
				results = []api.RestartResult{}
			}
			full := map[string]any{
				"vault_updated":   res.VaultUpdated,
				"restart_results": results,
				"error":           err.Error(),
				"code":            "RESTART_FAILED",
			}
			writeJSON(w, http.StatusInternalServerError, full)
			return
		}
		// res.RestartResults may be nil when restart=false; ensure non-nil for JSON.
		if res.RestartResults == nil {
			res.RestartResults = []api.RestartResult{}
		}
		anyFailed := false
		for _, r := range res.RestartResults {
			if r.Err != "" {
				anyFailed = true
				break
			}
		}
		status := http.StatusOK
		if anyFailed {
			status = http.StatusMultiStatus
		}
		writeJSON(w, status, res)
	case http.MethodDelete:
		confirm := r.URL.Query().Get("confirm") == "true"
		if err := s.secrets.Delete(key, confirm); err != nil {
			writeSecretsDeleteError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) secretsKeyRestart(w http.ResponseWriter, r *http.Request, key string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	results, err := s.secrets.Restart(key)
	if results == nil {
		results = []api.RestartResult{}
	}
	if err != nil {
		var setErr *api.SecretsSetError
		if errors.As(err, &setErr) {
			writeSecretsSetError(w, err)
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":           err.Error(),
			"code":            "RESTART_FAILED",
			"restart_results": results,
		})
		return
	}
	anyFailed := false
	for _, r := range results {
		if r.Err != "" {
			anyFailed = true
			break
		}
	}
	status := http.StatusOK
	if anyFailed {
		status = http.StatusMultiStatus
	}
	writeJSON(w, status, map[string]any{"restart_results": results})
}

func writeSecretsDeleteError(w http.ResponseWriter, err error) {
	var de *api.SecretsDeleteError
	if errors.As(err, &de) {
		body := map[string]any{
			"error": de.Message,
			"code":  de.Code,
		}
		if de.Code == "SECRETS_HAS_REFS" {
			body["used_by"] = de.UsedBy
		}
		if de.Code == "SECRETS_USAGE_SCAN_INCOMPLETE" {
			body["manifest_errors"] = de.ManifestErrors
		}
		writeJSON(w, http.StatusConflict, body)
		return
	}
	var setErr *api.SecretsSetError
	if errors.As(err, &setErr) {
		writeSecretsSetError(w, err)
		return
	}
	writeAPIError(w, err, http.StatusInternalServerError, "SECRETS_DELETE_FAILED")
}
```

- [ ] **Step 3.A.6: Compile + run handler tests (failing for now — tests come next)**

Run: `go build ./...`
Expected: green compile.

### 3.B — Handler tests (TDD)

- [ ] **Step 3.B.1: Create `internal/gui/secrets_test.go` with fake adapter**

```go
package gui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"mcp-local-hub/internal/api"
)

type fakeSecretsAPI struct {
	initResult     api.SecretsInitResult
	initErr        error
	listResult     api.SecretsEnvelope
	listErr        error
	setErr         error
	rotateResult   api.SecretsRotateResult
	rotateErr      error
	restartResults []api.RestartResult
	restartErr     error
	deleteErr      error

	calledSet     []struct{ Name, Value string }
	calledRotate  []struct{ Name, Value string; Restart bool }
	calledRestart []string
	calledDelete  []struct{ Name string; Confirm bool }
}

func (f *fakeSecretsAPI) Init() (api.SecretsInitResult, error) { return f.initResult, f.initErr }
func (f *fakeSecretsAPI) List() (api.SecretsEnvelope, error)   { return f.listResult, f.listErr }
func (f *fakeSecretsAPI) Set(name, value string) error {
	f.calledSet = append(f.calledSet, struct{ Name, Value string }{name, value})
	return f.setErr
}
func (f *fakeSecretsAPI) Rotate(name, value string, restart bool) (api.SecretsRotateResult, error) {
	f.calledRotate = append(f.calledRotate, struct {
		Name, Value string
		Restart     bool
	}{name, value, restart})
	return f.rotateResult, f.rotateErr
}
func (f *fakeSecretsAPI) Restart(name string) ([]api.RestartResult, error) {
	f.calledRestart = append(f.calledRestart, name)
	return f.restartResults, f.restartErr
}
func (f *fakeSecretsAPI) Delete(name string, confirm bool) error {
	f.calledDelete = append(f.calledDelete, struct {
		Name    string
		Confirm bool
	}{name, confirm})
	return f.deleteErr
}

func newServerWithSecretsFake(t *testing.T, fake *fakeSecretsAPI) *Server {
	t.Helper()
	s := NewServer(Config{})
	s.secrets = fake
	return s
}

// --- Tests below; see Step 3.B.2+ ---
```

- [ ] **Step 3.B.2: Test `POST /api/secrets/init` happy path**

```go
func TestSecretsInit_OK(t *testing.T) {
	fake := &fakeSecretsAPI{
		initResult: api.SecretsInitResult{VaultState: "ok"},
	}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodPost, "/api/secrets/init", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	var body api.SecretsInitResult
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.VaultState != "ok" {
		t.Errorf("vault_state=%q", body.VaultState)
	}
}
```

- [ ] **Step 3.B.3: Test `POST /api/secrets/init` 409 BLOCKED**

```go
func TestSecretsInit_Blocked409(t *testing.T) {
	fake := &fakeSecretsAPI{
		initErr: &api.SecretsInitBlocked{KeyExists: true, VaultExists: true},
	}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodPost, "/api/secrets/init", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "SECRETS_INIT_BLOCKED" {
		t.Errorf("code=%q", body["code"])
	}
}
```

- [ ] **Step 3.B.4: Test `POST /api/secrets/init` 200 cleanup-ok (case 2b)**

```go
func TestSecretsInit_PartialCleanupOK_Returns200(t *testing.T) {
	fake := &fakeSecretsAPI{
		initErr: &api.SecretsInitFailed{
			CleanupStatus: "ok",
			Cause:         fmt.Errorf("disk full"),
		},
	}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodPost, "/api/secrets/init", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200, body=%q", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["cleanup_status"] != "ok" {
		t.Errorf("cleanup_status=%v", body["cleanup_status"])
	}
	if body["vault_state"] != "missing" {
		t.Errorf("vault_state=%v, want missing", body["vault_state"])
	}
	if body["code"] != "SECRETS_INIT_FAILED" {
		t.Errorf("code=%v", body["code"])
	}
}
```

- [ ] **Step 3.B.5: Test `POST /api/secrets/init` 500 cleanup-failed (case 2c)**

```go
func TestSecretsInit_PartialCleanupFailed_Returns500(t *testing.T) {
	fake := &fakeSecretsAPI{
		initErr: &api.SecretsInitFailed{
			CleanupStatus: "failed",
			OrphanPath:    "/some/path/secrets.age",
			Cause:         fmt.Errorf("disk full"),
		},
	}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodPost, "/api/secrets/init", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["cleanup_status"] != "failed" {
		t.Errorf("cleanup_status=%v", body["cleanup_status"])
	}
	if body["orphan_path"] != "/some/path/secrets.age" {
		t.Errorf("orphan_path=%v", body["orphan_path"])
	}
}
```

- [ ] **Step 3.B.6: Test `GET /api/secrets`**

```go
func TestSecretsList_OK(t *testing.T) {
	fake := &fakeSecretsAPI{
		listResult: api.SecretsEnvelope{
			VaultState: "ok",
			Secrets: []api.SecretRow{
				{Name: "K1", State: "present", UsedBy: []api.UsageRef{{Server: "s1", EnvVar: "OPENAI_API_KEY"}}},
			},
			ManifestErrors: []api.ManifestError{},
		},
	}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodGet, "/api/secrets", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var body api.SecretsEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.VaultState != "ok" || len(body.Secrets) != 1 {
		t.Errorf("body=%+v", body)
	}
	if body.Secrets[0].UsedBy[0].Server != "s1" {
		t.Errorf("used_by=%+v", body.Secrets[0].UsedBy)
	}
}
```

- [ ] **Step 3.B.7: Test `POST /api/secrets` happy + error codes**

```go
func TestSecretsAdd_201Created(t *testing.T) {
	fake := &fakeSecretsAPI{}
	s := newServerWithSecretsFake(t, fake)

	body := bytes.NewReader([]byte(`{"name":"K1","value":"v"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/secrets", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if len(fake.calledSet) != 1 || fake.calledSet[0].Name != "K1" {
		t.Errorf("calledSet=%+v", fake.calledSet)
	}
}

func TestSecretsAdd_409KeyExists(t *testing.T) {
	fake := &fakeSecretsAPI{setErr: &api.SecretsSetError{Code: "SECRETS_KEY_EXISTS", Msg: "exists"}}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodPost, "/api/secrets", bytes.NewReader([]byte(`{"name":"K1","value":"v"}`)))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestSecretsAdd_400InvalidName(t *testing.T) {
	fake := &fakeSecretsAPI{setErr: &api.SecretsSetError{Code: "SECRETS_INVALID_NAME", Msg: "bad"}}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodPost, "/api/secrets", bytes.NewReader([]byte(`{"name":"123","value":"v"}`)))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestSecretsAdd_400MalformedJSON(t *testing.T) {
	fake := &fakeSecretsAPI{}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodPost, "/api/secrets", bytes.NewReader([]byte(`{not-json`)))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "SECRETS_INVALID_JSON" {
		t.Errorf("code=%q", body["code"])
	}
}
```

- [ ] **Step 3.B.8: Test `PUT /api/secrets/:key`**

```go
func TestSecretsRotate_200WhenAllOK(t *testing.T) {
	fake := &fakeSecretsAPI{
		rotateResult: api.SecretsRotateResult{
			VaultUpdated:   true,
			RestartResults: []api.RestartResult{{TaskName: "x", Err: ""}},
		},
	}
	s := newServerWithSecretsFake(t, fake)

	body := bytes.NewReader([]byte(`{"value":"new","restart":true}`))
	req := httptest.NewRequest(http.MethodPut, "/api/secrets/K1", body)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if len(fake.calledRotate) != 1 || fake.calledRotate[0].Name != "K1" {
		t.Errorf("calledRotate=%+v", fake.calledRotate)
	}
}

func TestSecretsRotate_207WhenAnyTaskFailed(t *testing.T) {
	fake := &fakeSecretsAPI{
		rotateResult: api.SecretsRotateResult{
			VaultUpdated: true,
			RestartResults: []api.RestartResult{
				{TaskName: "a", Err: ""},
				{TaskName: "b", Err: "timeout"},
			},
		},
	}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodPut, "/api/secrets/K1", bytes.NewReader([]byte(`{"value":"v","restart":true}`)))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("status=%d, want 207", rec.Code)
	}
}

func TestSecretsRotate_500OrchestrationFailureCarriesPartial(t *testing.T) {
	fake := &fakeSecretsAPI{
		rotateResult: api.SecretsRotateResult{
			VaultUpdated:   true,
			RestartResults: []api.RestartResult{{TaskName: "a", Err: ""}},
		},
		rotateErr: errors.New("scheduler unavailable"),
	}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodPut, "/api/secrets/K1", bytes.NewReader([]byte(`{"value":"v","restart":true}`)))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "RESTART_FAILED" {
		t.Errorf("code=%v", body["code"])
	}
	if body["vault_updated"] != true {
		t.Errorf("vault_updated=%v", body["vault_updated"])
	}
	if results, _ := body["restart_results"].([]any); len(results) != 1 {
		t.Errorf("partial results dropped: %v", body["restart_results"])
	}
}
```

- [ ] **Step 3.B.9: Test `POST /api/secrets/:key/restart` (the §5.4a endpoint)**

```go
func TestSecretsRestart_200(t *testing.T) {
	fake := &fakeSecretsAPI{
		restartResults: []api.RestartResult{{TaskName: "x", Err: ""}},
	}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodPost, "/api/secrets/K1/restart", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if len(fake.calledRestart) != 1 || fake.calledRestart[0] != "K1" {
		t.Errorf("calledRestart=%+v", fake.calledRestart)
	}
}

func TestSecretsRestart_207OnPartialFailure(t *testing.T) {
	fake := &fakeSecretsAPI{
		restartResults: []api.RestartResult{
			{TaskName: "a", Err: ""},
			{TaskName: "b", Err: "timeout"},
		},
	}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodPost, "/api/secrets/K1/restart", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("status=%d", rec.Code)
	}
}
```

- [ ] **Step 3.B.10: Test `DELETE /api/secrets/:key`**

```go
func TestSecretsDelete_204OnSuccess(t *testing.T) {
	fake := &fakeSecretsAPI{}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodDelete, "/api/secrets/K1", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d", rec.Code)
	}
	if len(fake.calledDelete) != 1 || fake.calledDelete[0].Confirm {
		t.Errorf("calledDelete=%+v", fake.calledDelete)
	}
}

func TestSecretsDelete_409HasRefs(t *testing.T) {
	fake := &fakeSecretsAPI{
		deleteErr: &api.SecretsDeleteError{
			Code:    "SECRETS_HAS_REFS",
			Message: "refs",
			UsedBy:  []api.UsageRef{{Server: "alpha", EnvVar: "OPENAI"}},
		},
	}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodDelete, "/api/secrets/K1", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "SECRETS_HAS_REFS" {
		t.Errorf("code=%v", body["code"])
	}
	usedBy, _ := body["used_by"].([]any)
	if len(usedBy) != 1 {
		t.Errorf("used_by=%+v", usedBy)
	}
}

func TestSecretsDelete_409UsageScanIncomplete(t *testing.T) {
	fake := &fakeSecretsAPI{
		deleteErr: &api.SecretsDeleteError{
			Code:           "SECRETS_USAGE_SCAN_INCOMPLETE",
			Message:        "scan incomplete",
			ManifestErrors: []api.ManifestError{{Path: "broken/manifest.yaml", Error: "yaml: line 1"}},
		},
	}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodDelete, "/api/secrets/K1", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "SECRETS_USAGE_SCAN_INCOMPLETE" {
		t.Errorf("code=%v", body["code"])
	}
}

func TestSecretsDelete_204WithConfirmTrue(t *testing.T) {
	fake := &fakeSecretsAPI{}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodDelete, "/api/secrets/K1?confirm=true", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d", rec.Code)
	}
	if len(fake.calledDelete) != 1 || !fake.calledDelete[0].Confirm {
		t.Errorf("calledDelete=%+v (confirm=true expected)", fake.calledDelete)
	}
}
```

- [ ] **Step 3.B.11: Test same-origin guard smoke**

```go
func TestSecrets_RejectsCrossOrigin(t *testing.T) {
	fake := &fakeSecretsAPI{}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodPost, "/api/secrets/init", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "CROSS_ORIGIN" {
		t.Errorf("code=%q, want CROSS_ORIGIN", body["code"])
	}
}
```

- [ ] **Step 3.B.11b: Add full handler-error-matrix coverage (Codex plan-R1 P1)**

Memo §7.1 enumerates per-endpoint × error-code coverage. Add the missing cases:

```go
// GET /api/secrets — vault state branches.
func TestSecretsList_VaultMissingState(t *testing.T) {
	fake := &fakeSecretsAPI{
		listResult: api.SecretsEnvelope{
			VaultState: "missing", Secrets: []api.SecretRow{}, ManifestErrors: []api.ManifestError{},
		},
	}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodGet, "/api/secrets", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var body api.SecretsEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.VaultState != "missing" {
		t.Errorf("vault_state=%q", body.VaultState)
	}
}

func TestSecretsList_500ListFailed(t *testing.T) {
	fake := &fakeSecretsAPI{listErr: fmt.Errorf("scan blew up")}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodGet, "/api/secrets", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "SECRETS_LIST_FAILED" {
		t.Errorf("code=%q", body["code"])
	}
}

// POST /api/secrets — full SecretsSetError matrix.
func TestSecretsAdd_400EmptyValue(t *testing.T) {
	fake := &fakeSecretsAPI{setErr: &api.SecretsSetError{Code: "SECRETS_EMPTY_VALUE", Msg: "empty"}}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodPost, "/api/secrets", bytes.NewReader([]byte(`{"name":"K","value":""}`)))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestSecretsAdd_409VaultNotInitialized(t *testing.T) {
	fake := &fakeSecretsAPI{setErr: &api.SecretsSetError{Code: "SECRETS_VAULT_NOT_INITIALIZED", Msg: "no vault"}}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodPost, "/api/secrets", bytes.NewReader([]byte(`{"name":"K","value":"v"}`)))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestSecretsAdd_500SetFailed(t *testing.T) {
	fake := &fakeSecretsAPI{setErr: &api.SecretsSetError{Code: "SECRETS_SET_FAILED", Msg: "disk"}}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodPost, "/api/secrets", bytes.NewReader([]byte(`{"name":"K","value":"v"}`)))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
}

// PUT /api/secrets/:key — error matrix.
func TestSecretsRotate_404KeyNotFound(t *testing.T) {
	fake := &fakeSecretsAPI{rotateErr: &api.SecretsSetError{Code: "SECRETS_KEY_NOT_FOUND", Msg: "missing"}}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodPut, "/api/secrets/K1", bytes.NewReader([]byte(`{"value":"v","restart":false}`)))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestSecretsRotate_409VaultNotInitialized(t *testing.T) {
	fake := &fakeSecretsAPI{rotateErr: &api.SecretsSetError{Code: "SECRETS_VAULT_NOT_INITIALIZED", Msg: "no vault"}}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodPut, "/api/secrets/K1", bytes.NewReader([]byte(`{"value":"v","restart":false}`)))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestSecretsRotate_500SetFailed(t *testing.T) {
	fake := &fakeSecretsAPI{rotateErr: &api.SecretsSetError{Code: "SECRETS_SET_FAILED", Msg: "disk"}}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodPut, "/api/secrets/K1", bytes.NewReader([]byte(`{"value":"v","restart":false}`)))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestSecretsRotate_400EmptyValue(t *testing.T) {
	fake := &fakeSecretsAPI{rotateErr: &api.SecretsSetError{Code: "SECRETS_EMPTY_VALUE", Msg: "empty"}}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodPut, "/api/secrets/K1", bytes.NewReader([]byte(`{"value":"","restart":false}`)))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
}

// POST /api/secrets/:key/restart — error matrix.
func TestSecretsRestart_404KeyNotFound(t *testing.T) {
	fake := &fakeSecretsAPI{restartErr: &api.SecretsSetError{Code: "SECRETS_KEY_NOT_FOUND", Msg: "missing"}}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodPost, "/api/secrets/K1/restart", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestSecretsRestart_409VaultNotInitialized(t *testing.T) {
	fake := &fakeSecretsAPI{restartErr: &api.SecretsSetError{Code: "SECRETS_VAULT_NOT_INITIALIZED", Msg: "no vault"}}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodPost, "/api/secrets/K1/restart", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestSecretsRestart_500OrchestrationFailure(t *testing.T) {
	fake := &fakeSecretsAPI{
		restartResults: []api.RestartResult{{TaskName: "a", Err: ""}},
		restartErr:     fmt.Errorf("scheduler unavailable"),
	}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodPost, "/api/secrets/K1/restart", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "RESTART_FAILED" {
		t.Errorf("code=%v", body["code"])
	}
	if results, _ := body["restart_results"].([]any); len(results) != 1 {
		t.Errorf("partial results dropped")
	}
}

// DELETE /api/secrets/:key — error matrix.
func TestSecretsDelete_404KeyNotFound(t *testing.T) {
	fake := &fakeSecretsAPI{deleteErr: &api.SecretsSetError{Code: "SECRETS_KEY_NOT_FOUND", Msg: "missing"}}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodDelete, "/api/secrets/K1", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestSecretsDelete_409VaultNotInitialized(t *testing.T) {
	fake := &fakeSecretsAPI{deleteErr: &api.SecretsSetError{Code: "SECRETS_VAULT_NOT_INITIALIZED", Msg: "no vault"}}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodDelete, "/api/secrets/K1", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestSecretsDelete_500DeleteFailed(t *testing.T) {
	fake := &fakeSecretsAPI{deleteErr: &api.SecretsSetError{Code: "SECRETS_DELETE_FAILED", Msg: "disk"}}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodDelete, "/api/secrets/K1?confirm=true", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
}
```

- [ ] **Step 3.B.12: Run all handler tests**

```bash
go test ./internal/gui/ -run TestSecrets -count=1 -v
```

Expected: ~31 tests PASS (16 base + 15 matrix additions: GET 2 + POST 3 + PUT 4 + restart 3 + DELETE 3 per Codex plan-R1 P1).

### 3.C — Commit

- [ ] **Step 3.C.1: Run full Go test suite**

```bash
go test ./... -count=1
```

Expected: green.

- [ ] **Step 3.C.2: Commit**

```bash
git add internal/gui/secrets.go internal/gui/secrets_test.go internal/gui/server.go internal/api/secrets.go
git commit -m "feat(gui): A3-a HTTP handlers — 6 endpoints with same-origin guard

internal/gui/secrets.go: registerSecretsRoutes wires:
  POST /api/secrets/init
  GET  /api/secrets
  POST /api/secrets
  PUT  /api/secrets/:key
  POST /api/secrets/:key/restart  (§5.4a)
  DELETE /api/secrets/:key

All endpoints behind requireSameOrigin (CROSS_ORIGIN). Error
catalog mapped per memo §5.7 (SECRETS_VAULT_NOT_INITIALIZED 409,
SECRETS_HAS_REFS 409 + used_by, SECRETS_USAGE_SCAN_INCOMPLETE 409 +
manifest_errors, RESTART_FAILED 500 + partial restart_results, etc).

internal/api/secrets.go: promote secretsInitFailed → SecretsInitFailed
so handler can distinguish 200 cleanup-ok from 500 cleanup-failed.

internal/gui/secrets_test.go: 16 handler tests covering each endpoint
× error code combination + cross-origin guard."
```

---

## Task 4: Frontend scaffold — `secrets-api.ts` + `useSecretsSnapshot` hook

**Goal:** Typed fetch wrappers + a polling hook that the screen consumes. No UI yet.

**Files:**
- Create: `internal/gui/frontend/src/lib/secrets-api.ts`
- Create: `internal/gui/frontend/src/lib/secrets-api.test.ts`
- Create: `internal/gui/frontend/src/lib/use-secrets-snapshot.ts`
- Create: `internal/gui/frontend/src/lib/use-secrets-snapshot.test.ts`

### 4.A — `secrets-api.ts` (TDD)

- [ ] **Step 4.A.1: Write failing wrapper test for `getSecrets`**

Create `internal/gui/frontend/src/lib/secrets-api.test.ts`:

```ts
import { describe, expect, it, beforeEach, vi } from "vitest";
import { addSecret, deleteSecret, getSecrets, restartSecret, rotateSecret, secretsInit } from "./secrets-api";

const mockFetch = vi.fn();
beforeEach(() => {
  mockFetch.mockReset();
  globalThis.fetch = mockFetch as unknown as typeof fetch;
});

describe("getSecrets", () => {
  it("parses the envelope on 200", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({
        vault_state: "ok",
        secrets: [{ name: "K1", state: "present", used_by: [{ server: "s1", env_var: "OPENAI" }] }],
        manifest_errors: [],
      }),
    });

    const env = await getSecrets();
    expect(env.vault_state).toBe("ok");
    expect(env.secrets).toHaveLength(1);
    expect(env.secrets[0].used_by[0].server).toBe("s1");
  });

  it("throws on 5xx", async () => {
    mockFetch.mockResolvedValue({
      ok: false,
      status: 500,
      json: async () => ({ error: "boom", code: "SECRETS_LIST_FAILED" }),
    });
    await expect(getSecrets()).rejects.toThrow(/SECRETS_LIST_FAILED|boom/);
  });
});
```

Run: `cd internal/gui/frontend && npm test -- secrets-api`
Expected: FAIL (functions don't exist).

- [ ] **Step 4.A.2: Implement `secrets-api.ts`**

```ts
// internal/gui/frontend/src/lib/secrets-api.ts

export type VaultState = "ok" | "missing" | "decrypt_failed" | "corrupt";

export interface UsageRef {
  server: string;
  env_var: string;
}

export interface ManifestError {
  name?: string;
  path: string;
  error: string;
}

export interface SecretRow {
  name: string;
  state: "present" | "referenced_missing" | "referenced_unverified";
  used_by: UsageRef[];
}

export interface SecretsEnvelope {
  vault_state: VaultState;
  secrets: SecretRow[];
  manifest_errors: ManifestError[];
}

export interface RestartResult {
  task_name: string;
  error: string;
}

export interface SecretsRotateResult {
  vault_updated: boolean;
  restart_results: RestartResult[];
}

export interface APIError extends Error {
  code?: string;
  status: number;
  body: unknown;
}

function makeAPIError(status: number, code: string | undefined, message: string, body: unknown): APIError {
  const err = new Error(message) as APIError;
  err.code = code;
  err.status = status;
  err.body = body;
  return err;
}

async function parseJSONBody(resp: Response): Promise<any> {
  try {
    return await resp.json();
  } catch {
    return {};
  }
}

export async function secretsInit(): Promise<{ vault_state?: string; cleanup_status?: string; orphan_path?: string; error?: string; code?: string }> {
  const resp = await fetch("/api/secrets/init", { method: "POST" });
  const body = await parseJSONBody(resp);
  if (resp.status === 200) return body;
  throw makeAPIError(resp.status, body.code, body.error ?? `init failed: ${resp.status}`, body);
}

export async function getSecrets(): Promise<SecretsEnvelope> {
  const resp = await fetch("/api/secrets");
  const body = await parseJSONBody(resp);
  if (!resp.ok) throw makeAPIError(resp.status, body.code, body.error ?? `list failed: ${resp.status}`, body);
  return body as SecretsEnvelope;
}

export async function addSecret(name: string, value: string): Promise<void> {
  const resp = await fetch("/api/secrets", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name, value }),
  });
  if (resp.status === 201) return;
  const body = await parseJSONBody(resp);
  throw makeAPIError(resp.status, body.code, body.error ?? `add failed: ${resp.status}`, body);
}

export async function rotateSecret(name: string, value: string, restart: boolean): Promise<SecretsRotateResult> {
  const resp = await fetch(`/api/secrets/${encodeURIComponent(name)}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ value, restart }),
  });
  const body = await parseJSONBody(resp);
  // 200 + 207 = success-with-or-without-partial-failure; still parse the body.
  if (resp.status === 200 || resp.status === 207) return body as SecretsRotateResult;
  throw makeAPIError(resp.status, body.code, body.error ?? `rotate failed: ${resp.status}`, body);
}

export async function restartSecret(name: string): Promise<{ restart_results: RestartResult[] }> {
  const resp = await fetch(`/api/secrets/${encodeURIComponent(name)}/restart`, { method: "POST" });
  const body = await parseJSONBody(resp);
  if (resp.status === 200 || resp.status === 207) return body as { restart_results: RestartResult[] };
  throw makeAPIError(resp.status, body.code, body.error ?? `restart failed: ${resp.status}`, body);
}

export async function deleteSecret(name: string, opts?: { confirm?: boolean }): Promise<void> {
  const url = `/api/secrets/${encodeURIComponent(name)}` + (opts?.confirm ? "?confirm=true" : "");
  const resp = await fetch(url, { method: "DELETE" });
  if (resp.status === 204) return;
  const body = await parseJSONBody(resp);
  throw makeAPIError(resp.status, body.code, body.error ?? `delete failed: ${resp.status}`, body);
}
```

- [ ] **Step 4.A.3: Verify the failing test now passes**

```bash
cd internal/gui/frontend && npm test -- secrets-api
```

Expected: PASS.

- [ ] **Step 4.A.4: Add wrapper tests for each function**

Append to `secrets-api.test.ts`:

```ts
describe("secretsInit", () => {
  it("returns body on 200", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ vault_state: "ok" }),
    });
    const res = await secretsInit();
    expect(res.vault_state).toBe("ok");
  });

  it("throws on 409 SECRETS_INIT_BLOCKED", async () => {
    mockFetch.mockResolvedValue({
      ok: false,
      status: 409,
      json: async () => ({ error: "blocked", code: "SECRETS_INIT_BLOCKED" }),
    });
    await expect(secretsInit()).rejects.toMatchObject({ code: "SECRETS_INIT_BLOCKED", status: 409 });
  });
});

describe("addSecret", () => {
  it("returns void on 201", async () => {
    mockFetch.mockResolvedValue({ ok: true, status: 201, json: async () => ({}) });
    await expect(addSecret("K1", "v")).resolves.toBeUndefined();
  });

  it("throws on 409 SECRETS_KEY_EXISTS", async () => {
    mockFetch.mockResolvedValue({
      ok: false,
      status: 409,
      json: async () => ({ error: "dup", code: "SECRETS_KEY_EXISTS" }),
    });
    await expect(addSecret("K1", "v")).rejects.toMatchObject({ code: "SECRETS_KEY_EXISTS" });
  });
});

describe("rotateSecret", () => {
  it("returns body on 200", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ vault_updated: true, restart_results: [] }),
    });
    const res = await rotateSecret("K1", "v", false);
    expect(res.vault_updated).toBe(true);
  });

  it("returns body on 207", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      status: 207,
      json: async () => ({
        vault_updated: true,
        restart_results: [{ task_name: "x", error: "fail" }],
      }),
    });
    const res = await rotateSecret("K1", "v", true);
    expect(res.restart_results[0].error).toBe("fail");
  });
});

describe("restartSecret", () => {
  it("returns body on 200", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ restart_results: [] }),
    });
    const res = await restartSecret("K1");
    expect(res.restart_results).toEqual([]);
  });
});

describe("deleteSecret", () => {
  it("returns void on 204", async () => {
    mockFetch.mockResolvedValue({ ok: true, status: 204, json: async () => ({}) });
    await expect(deleteSecret("K1")).resolves.toBeUndefined();
  });

  it("throws with usedBy on 409 SECRETS_HAS_REFS", async () => {
    mockFetch.mockResolvedValue({
      ok: false,
      status: 409,
      json: async () => ({
        error: "refs",
        code: "SECRETS_HAS_REFS",
        used_by: [{ server: "alpha", env_var: "OPENAI" }],
      }),
    });
    await expect(deleteSecret("K1")).rejects.toMatchObject({
      code: "SECRETS_HAS_REFS",
      body: expect.objectContaining({ used_by: [{ server: "alpha", env_var: "OPENAI" }] }),
    });
  });

  it("sends ?confirm=true when opts.confirm", async () => {
    mockFetch.mockResolvedValue({ ok: true, status: 204, json: async () => ({}) });
    await deleteSecret("K1", { confirm: true });
    expect(mockFetch).toHaveBeenCalledWith("/api/secrets/K1?confirm=true", expect.objectContaining({ method: "DELETE" }));
  });
});
```

- [ ] **Step 4.A.5: Run all wrapper tests**

```bash
cd internal/gui/frontend && npm test -- secrets-api
```

Expected: ~14 tests PASS.

### 4.B — `use-secrets-snapshot.ts` hook

- [ ] **Step 4.B.1a: Write failing tests FIRST (TDD — Codex plan-R1 P2)**

Create `internal/gui/frontend/src/lib/use-secrets-snapshot.test.ts` BEFORE the impl. The test content is in Step 4.B.2 below; create the file now and run `npm test` to verify the failing import.

Run: `cd internal/gui/frontend && npm test -- use-secrets-snapshot`
Expected: FAIL with `Cannot find module ./use-secrets-snapshot` or similar.

- [ ] **Step 4.B.1: Implement the hook**

Create `internal/gui/frontend/src/lib/use-secrets-snapshot.ts`:

```ts
import { useEffect, useState, useCallback } from "preact/hooks";
import { getSecrets, type SecretsEnvelope, type APIError } from "./secrets-api";

export type SnapshotState =
  | { status: "loading"; data: null; error: null }
  | { status: "ok"; data: SecretsEnvelope; error: null }
  | { status: "error"; data: null; error: APIError | Error };

export function useSecretsSnapshot(): SnapshotState & { refresh: () => Promise<void> } {
  const [state, setState] = useState<SnapshotState>({ status: "loading", data: null, error: null });

  const refresh = useCallback(async () => {
    setState({ status: "loading", data: null, error: null });
    try {
      const data = await getSecrets();
      setState({ status: "ok", data, error: null });
    } catch (e) {
      setState({ status: "error", data: null, error: e as Error });
    }
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  // Codex plan-R1 P2: refetch on window focus so a vault edit from a
  // separate tab/CLI surfaces in the registry view without a manual
  // reload. This matches memo §3.1 frontend item #8 ("polls on focus").
  useEffect(() => {
    const onFocus = () => { void refresh(); };
    window.addEventListener("focus", onFocus);
    return () => window.removeEventListener("focus", onFocus);
  }, [refresh]);

  return { ...state, refresh };
}
```

- [ ] **Step 4.B.2: Add hook test**

Create `internal/gui/frontend/src/lib/use-secrets-snapshot.test.ts`:

```ts
import { describe, expect, it, beforeEach, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/preact";
import { useSecretsSnapshot } from "./use-secrets-snapshot";

const mockFetch = vi.fn();
beforeEach(() => {
  mockFetch.mockReset();
  globalThis.fetch = mockFetch as unknown as typeof fetch;
});

describe("useSecretsSnapshot", () => {
  it("transitions loading → ok on success", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ vault_state: "ok", secrets: [], manifest_errors: [] }),
    });

    const { result } = renderHook(() => useSecretsSnapshot());
    expect(result.current.status).toBe("loading");

    await waitFor(() => expect(result.current.status).toBe("ok"));
    expect(result.current.data?.vault_state).toBe("ok");
  });

  it("transitions to error on fetch failure", async () => {
    mockFetch.mockResolvedValue({
      ok: false,
      status: 500,
      json: async () => ({ error: "boom", code: "SECRETS_LIST_FAILED" }),
    });

    const { result } = renderHook(() => useSecretsSnapshot());
    await waitFor(() => expect(result.current.status).toBe("error"));
  });
});
```

You may need `@testing-library/preact` installed. If it isn't already in `package.json`, run:

```bash
cd internal/gui/frontend
npm install --save-dev @testing-library/preact
```

(Or if that's controversial, replace the test with a manual implementation that calls the hook function via `preact/hooks` test utilities. The plan assumes `@testing-library/preact` is available.)

- [ ] **Step 4.B.3: Run hook test**

```bash
cd internal/gui/frontend && npm test -- use-secrets-snapshot
```

Expected: 2 tests PASS.

### 4.C — Commit

- [ ] **Step 4.C.1: Run all frontend tests + typecheck**

```bash
cd internal/gui/frontend
npm test
npm run typecheck
cd ../../..
```

Expected: green.

- [ ] **Step 4.C.2: Commit**

```bash
git add internal/gui/frontend/src/lib/secrets-api.ts internal/gui/frontend/src/lib/secrets-api.test.ts internal/gui/frontend/src/lib/use-secrets-snapshot.ts internal/gui/frontend/src/lib/use-secrets-snapshot.test.ts internal/gui/frontend/package.json internal/gui/frontend/package-lock.json
git commit -m "feat(gui/frontend): A3-a — typed wrappers + useSecretsSnapshot hook

Adds:
  lib/secrets-api.ts         — 6 typed fetch wrappers (init, getSecrets,
                                addSecret, rotateSecret, restartSecret,
                                deleteSecret) + APIError type
  lib/use-secrets-snapshot   — loading/ok/error state machine hook

Wrappers parse {error, code} envelope from writeAPIError. Rotate
treats 207 as success (returns body); restart wrapper does the same;
deleteSecret returns the typed APIError carrying used_by /
manifest_errors so escalation modals can render fresh refs.

Vitest coverage: 14 cases for wrappers, 2 for the hook."
```

---

## Task 5: Secrets.tsx 4-state screen + app.tsx nav entry

**Goal:** Render 4 vault states (`not-init`/`init-empty`/`init-keyed`/`broken`) with placeholder click handlers for Add/Rotate/Delete (modals come in tasks 6-8). The Edit-vault banner from D6 ships now.

**Files:**
- Create: `internal/gui/frontend/src/screens/Secrets.tsx`
- Modify: `internal/gui/frontend/src/app.tsx` — add route + nav link

### 5.A — `Secrets.tsx`

- [ ] **Step 5.A.1: Create the screen**

```tsx
// internal/gui/frontend/src/screens/Secrets.tsx
import { useState } from "preact/hooks";
import { useSecretsSnapshot } from "../lib/use-secrets-snapshot";
import { secretsInit } from "../lib/secrets-api";
import type { SecretsEnvelope, SecretRow, UsageRef } from "../lib/secrets-api";

const MCPHUB_EDIT_CMD = "mcphub secrets edit";

export function SecretsScreen() {
  const snap = useSecretsSnapshot();

  if (snap.status === "loading") {
    return (
      <section class="secrets-screen">
        <h1>Secrets</h1>
        <p>Loading…</p>
      </section>
    );
  }
  if (snap.status === "error") {
    return (
      <section class="secrets-screen">
        <h1>Secrets</h1>
        <p class="error">Failed to load: {snap.error.message}</p>
        <button onClick={() => snap.refresh()}>Retry</button>
      </section>
    );
  }
  const env = snap.data;
  const state = env.vault_state;
  return (
    <section class="secrets-screen">
      <h1>Secrets</h1>
      <EditVaultBanner />
      {state === "missing" && <NotInitView refresh={snap.refresh} />}
      {state === "ok" && env.secrets.length === 0 && <InitEmptyView />}
      {state === "ok" && env.secrets.length > 0 && <InitKeyedView env={env} />}
      {(state === "decrypt_failed" || state === "corrupt") && <BrokenView env={env} />}
      <ManifestErrorsBanner env={env} />
    </section>
  );
}

function EditVaultBanner() {
  const [copied, setCopied] = useState(false);
  return (
    <div class="banner banner-info" data-testid="edit-vault-banner">
      <span>Need bulk operations? Run the CLI command in a terminal: </span>
      <code>{MCPHUB_EDIT_CMD}</code>
      <button
        type="button"
        onClick={async () => {
          try {
            await navigator.clipboard.writeText(MCPHUB_EDIT_CMD);
            setCopied(true);
            setTimeout(() => setCopied(false), 1500);
          } catch {
            // ignore — older browsers may reject without permission
          }
        }}
      >
        {copied ? "Copied" : "Copy command"}
      </button>
    </div>
  );
}

function NotInitView(props: { refresh: () => Promise<void> }) {
  const [err, setErr] = useState<string | null>(null);
  const [working, setWorking] = useState(false);
  return (
    <div class="empty-state">
      <p><strong>Secrets vault is not initialized.</strong></p>
      <p>
        ⚠️ Initializing creates your private encryption key at the user-data
        directory. <strong>If you lose this file, all encrypted secrets are
        unrecoverable.</strong> Back it up via password manager or secure copy.
      </p>
      <button
        type="button"
        disabled={working}
        onClick={async () => {
          setWorking(true);
          setErr(null);
          try {
            await secretsInit();
            await props.refresh();
          } catch (e) {
            setErr((e as Error).message);
          } finally {
            setWorking(false);
          }
        }}
      >
        {working ? "Initializing…" : "Initialize secrets vault"}
      </button>
      {err && <p class="error">Init failed: {err}</p>}
    </div>
  );
}

function InitEmptyView() {
  return (
    <div class="empty-state">
      <p>No secrets yet.</p>
      <button type="button" onClick={() => console.log("AddSecret modal pending — Task 6")}>
        Add secret
      </button>
    </div>
  );
}

function InitKeyedView(props: { env: SecretsEnvelope }) {
  return (
    <div class="secrets-table">
      <button type="button" onClick={() => console.log("AddSecret modal pending — Task 6")}>
        Add secret
      </button>
      <table>
        <thead>
          <tr>
            <th>Name</th>
            <th>Used by</th>
            <th>State</th>
            <th>Actions</th>
          </tr>
        </thead>
        <tbody>
          {props.env.secrets.map((s) => <SecretRowComponent key={s.name} row={s} />)}
        </tbody>
      </table>
    </div>
  );
}

function SecretRowComponent(props: { row: SecretRow }) {
  const isPresent = props.row.state === "present";
  const usedByCount = props.row.used_by.length;
  return (
    <tr data-state={props.row.state}>
      <td>{props.row.name}</td>
      <td title={formatUsedBy(props.row.used_by)}>{usedByCount}</td>
      <td>{props.row.state}</td>
      <td>
        <button type="button" disabled={!isPresent} onClick={() => console.log(`Rotate ${props.row.name} — Task 7`)}>Rotate</button>
        <button type="button" disabled={!isPresent} onClick={() => console.log(`Delete ${props.row.name} — Task 8`)}>Delete</button>
        {props.row.state === "referenced_missing" && <span class="hint">↳ <a href="#" onClick={() => console.log(`AddSecret prefilled with ${props.row.name} — Task 6`)}>Add this secret</a></span>}
      </td>
    </tr>
  );
}

function formatUsedBy(refs: UsageRef[]): string {
  return refs.map((r) => `${r.server} (env: ${r.env_var})`).join("\n");
}

function BrokenView(props: { env: SecretsEnvelope }) {
  return (
    <div class="banner banner-error">
      <p><strong>Vault unavailable</strong> ({props.env.vault_state}). Manifest references shown below as <em>referenced_unverified</em>; vault status cannot be verified.</p>
      <p>Recovery: run <code>mcphub secrets edit</code>, or remove the vault files and re-initialize. <strong>Removing the vault destroys all stored secrets.</strong></p>
      {props.env.secrets.length > 0 && (
        <table>
          <thead>
            <tr><th>Name</th><th>Used by</th></tr>
          </thead>
          <tbody>
            {props.env.secrets.map((s) => (
              <tr key={s.name}>
                <td>{s.name}</td>
                <td title={formatUsedBy(s.used_by)}>{s.used_by.length}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

function ManifestErrorsBanner(props: { env: SecretsEnvelope }) {
  if (props.env.manifest_errors.length === 0) return null;
  return (
    <div class="banner banner-warn" data-testid="manifest-errors-banner">
      <details>
        <summary>{props.env.manifest_errors.length} manifest(s) failed to scan</summary>
        <ul>
          {props.env.manifest_errors.map((e) => (
            <li key={e.path}><code>{e.path}</code>: {e.error}</li>
          ))}
        </ul>
      </details>
    </div>
  );
}
```

### 5.B — `app.tsx` route + nav

- [ ] **Step 5.B.1: Add nav link and route case**

Modify `internal/gui/frontend/src/app.tsx`:

Add the import at top:

```tsx
import { SecretsScreen } from "./screens/Secrets";
```

Add a case to the switch:

```tsx
    case "secrets":
      body = <SecretsScreen />;
      break;
```

Add the nav link in the `<nav>` block (between Add server and Dashboard, or wherever flow makes sense):

```tsx
          <a href="#/secrets" class={route.screen === "secrets" ? "active" : ""} onClick={guardClick("secrets")}>Secrets</a>
```

- [ ] **Step 5.B.2: Verify TypeScript compile + Vitest still green**

```bash
cd internal/gui/frontend
npm run typecheck
npm test
cd ../../..
```

Expected: green.

### 5.C — Commit

- [ ] **Step 5.C.1: Commit**

```bash
git add internal/gui/frontend/src/screens/Secrets.tsx internal/gui/frontend/src/app.tsx
git commit -m "feat(gui/frontend): A3-a Secrets.tsx 4-state screen + nav

Renders four vault states:
  not-init  → empty-state + 'Initialize secrets vault' (calls
              POST /api/secrets/init, refreshes snapshot on success).
  init-empty→ 'No secrets yet' + Add button (modal in Task 6).
  init-keyed→ table with name / used_by count / state / actions.
              Rotate + Delete buttons placeholder console.log
              (modals in Tasks 7-8).
  broken    → vault_state==decrypt_failed|corrupt; manifest refs
              still rendered as referenced_unverified.

mcphub secrets edit banner with copy-to-clipboard (D6 banner-not-button).
Manifest errors collapsible banner.

internal/gui/frontend/src/app.tsx: nav link and route entry."
```

---

## Task 6: AddSecretModal

**Files:**
- Create: `internal/gui/frontend/src/components/AddSecretModal.tsx`
- Modify: `internal/gui/frontend/src/screens/Secrets.tsx` — wire button to modal

### 6.A — Modal component

- [ ] **Step 6.A.1: Create `AddSecretModal.tsx`**

```tsx
// internal/gui/frontend/src/components/AddSecretModal.tsx
import { useEffect, useRef, useState } from "preact/hooks";
import { addSecret } from "../lib/secrets-api";

interface Props {
  open: boolean;
  prefillName?: string;
  onClose: () => void;
  onSaved: () => void;
}

const NAME_RE = /^[A-Za-z][A-Za-z0-9_]*$/;

export function AddSecretModal(props: Props) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const [name, setName] = useState(props.prefillName ?? "");
  const [value, setValue] = useState("");
  const [working, setWorking] = useState(false);
  const [serverErr, setServerErr] = useState<string | null>(null);

  useEffect(() => {
    if (!dialogRef.current) return;
    if (props.open && !dialogRef.current.open) {
      dialogRef.current.showModal();
      setName(props.prefillName ?? "");
      setValue("");
      setServerErr(null);
    } else if (!props.open && dialogRef.current.open) {
      dialogRef.current.close();
    }
  }, [props.open, props.prefillName]);

  const nameValid = name === "" || NAME_RE.test(name);
  const canSubmit = name !== "" && value !== "" && nameValid && !working;

  return (
    <dialog
      ref={dialogRef}
      onClose={() => props.onClose()}
      data-testid="add-secret-modal"
    >
      <form
        method="dialog"
        onSubmit={async (e) => {
          e.preventDefault();
          if (!canSubmit) return;
          setWorking(true);
          setServerErr(null);
          try {
            await addSecret(name, value);
            props.onSaved();
            props.onClose();
          } catch (err) {
            setServerErr((err as Error).message);
          } finally {
            setWorking(false);
          }
        }}
      >
        <h2>Add secret</h2>
        <label>
          Name
          <input
            type="text"
            value={name}
            onInput={(e) => setName((e.target as HTMLInputElement).value)}
            placeholder="OPENAI_API_KEY"
            required
            disabled={working || Boolean(props.prefillName)}
          />
        </label>
        {!nameValid && <p class="error">Name must match {NAME_RE.source}</p>}
        <label>
          Value
          <input
            type="password"
            value={value}
            onInput={(e) => setValue((e.target as HTMLInputElement).value)}
            required
            disabled={working}
          />
        </label>
        {serverErr && <p class="error">{serverErr}</p>}
        <menu>
          <button type="button" onClick={() => props.onClose()} disabled={working}>Cancel</button>
          <button type="submit" disabled={!canSubmit}>{working ? "Saving…" : "Save"}</button>
        </menu>
      </form>
    </dialog>
  );
}
```

- [ ] **Step 6.A.2: Wire modal into `Secrets.tsx`**

Modify the `InitEmptyView` and `InitKeyedView` so they own the modal:

```tsx
function InitEmptyView(props: { refresh: () => Promise<void> }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <div class="empty-state">
        <p>No secrets yet.</p>
        <button type="button" onClick={() => setOpen(true)}>Add secret</button>
      </div>
      <AddSecretModal open={open} onClose={() => setOpen(false)} onSaved={() => props.refresh()} />
    </>
  );
}

function InitKeyedView(props: { env: SecretsEnvelope; refresh: () => Promise<void> }) {
  const [addOpen, setAddOpen] = useState(false);
  const [prefill, setPrefill] = useState<string | undefined>(undefined);
  return (
    <div class="secrets-table">
      <button type="button" onClick={() => { setPrefill(undefined); setAddOpen(true); }}>Add secret</button>
      <table>
        <thead>
          <tr>
            <th>Name</th>
            <th>Used by</th>
            <th>State</th>
            <th>Actions</th>
          </tr>
        </thead>
        <tbody>
          {props.env.secrets.map((s) => (
            <SecretRowComponent
              key={s.name}
              row={s}
              onAddPrefill={(n) => { setPrefill(n); setAddOpen(true); }}
            />
          ))}
        </tbody>
      </table>
      <AddSecretModal open={addOpen} prefillName={prefill} onClose={() => setAddOpen(false)} onSaved={() => props.refresh()} />
    </div>
  );
}
```

Update the `SecretRowComponent` signature to accept `onAddPrefill` and call it from the "Add this secret" link:

```tsx
function SecretRowComponent(props: { row: SecretRow; onAddPrefill: (name: string) => void }) {
  // ... existing tr/td ...
  // Update the "Add this secret" link:
  // onClick={() => props.onAddPrefill(props.row.name)}
}
```

Also add the import at top of `Secrets.tsx`:

```tsx
import { AddSecretModal } from "../components/AddSecretModal";
```

And update the call sites of `InitEmptyView` and `InitKeyedView` to pass `refresh`:

```tsx
{state === "missing" && <NotInitView refresh={snap.refresh} />}
{state === "ok" && env.secrets.length === 0 && <InitEmptyView refresh={snap.refresh} />}
{state === "ok" && env.secrets.length > 0 && <InitKeyedView env={env} refresh={snap.refresh} />}
```

- [ ] **Step 6.A.3: Typecheck**

```bash
cd internal/gui/frontend && npm run typecheck && cd ../../..
```

Expected: green.

### 6.B — Commit

- [ ] **Step 6.B.1: Commit**

```bash
git add internal/gui/frontend/src/components/AddSecretModal.tsx internal/gui/frontend/src/screens/Secrets.tsx
git commit -m "feat(gui/frontend): A3-a AddSecretModal — name regex + masked value

Native <dialog> modal for adding a secret. Name validated against
^[A-Za-z][A-Za-z0-9_]* (memo §5.3). Value field type=password.
Server-side errors (409 SECRETS_KEY_EXISTS, 400 SECRETS_INVALID_NAME)
surfaced inline. On save, calls props.onSaved() so the parent can
refresh the snapshot.

InitEmptyView and InitKeyedView own the modal; \"Add this secret\"
link on referenced_missing rows opens the modal with name prefilled."
```

---

## Task 7: RotateSecretModal — 3-button + persistent CTA + restart-now

**Files:**
- Create: `internal/gui/frontend/src/components/RotateSecretModal.tsx`
- Modify: `internal/gui/frontend/src/screens/Secrets.tsx` — wire Rotate button + persistent CTA

### 7.A — Modal component

- [ ] **Step 7.A.1: Create `RotateSecretModal.tsx`**

```tsx
// internal/gui/frontend/src/components/RotateSecretModal.tsx
import { useEffect, useRef, useState } from "preact/hooks";
import type { SecretsRotateResult } from "../lib/secrets-api";
import { rotateSecret } from "../lib/secrets-api";
// (Codex plan-R1 P2: removed unused RestartResult import — the banner
// only references types via SecretsRotateResult.restart_results.)

interface Props {
  open: boolean;
  name: string;
  // For the counter copy: total reference count and how many are running.
  // Computed by parent from snapshot + status; if not available, both
  // can be undefined and the modal omits the counts.
  refCount: number;
  runningCount?: number;
  onClose: () => void;
  onSaved: (result: SecretsRotateResult, mode: "no-restart" | "with-restart") => void;
}

export function RotateSecretModal(props: Props) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const [value, setValue] = useState("");
  const [working, setWorking] = useState(false);
  const [serverErr, setServerErr] = useState<string | null>(null);

  useEffect(() => {
    if (!dialogRef.current) return;
    if (props.open && !dialogRef.current.open) {
      dialogRef.current.showModal();
      setValue("");
      setServerErr(null);
    } else if (!props.open && dialogRef.current.open) {
      dialogRef.current.close();
    }
  }, [props.open]);

  const submit = async (restart: boolean) => {
    if (value === "" || working) return;
    setWorking(true);
    setServerErr(null);
    try {
      const result = await rotateSecret(props.name, value, restart);
      props.onSaved(result, restart ? "with-restart" : "no-restart");
      props.onClose();
    } catch (err) {
      setServerErr((err as Error).message);
    } finally {
      setWorking(false);
    }
  };

  const counterCopy = props.runningCount === undefined
    ? `${props.refCount} daemon(s) reference this key.`
    : `${props.refCount} daemon(s) reference this key; ${props.runningCount} currently running.`;

  return (
    <dialog ref={dialogRef} onClose={() => props.onClose()} data-testid="rotate-secret-modal">
      <h2>Rotate {props.name}</h2>
      <p>{counterCopy}</p>
      <p>Stopped daemons will pick up the new value automatically on next start.</p>
      <label>
        New value
        <input type="password" value={value} onInput={(e) => setValue((e.target as HTMLInputElement).value)} disabled={working} />
      </label>
      {serverErr && <p class="error">{serverErr}</p>}
      <menu>
        <button type="button" onClick={() => props.onClose()} disabled={working}>Cancel</button>
        <button type="button" onClick={() => submit(false)} disabled={value === "" || working}>Save without restart</button>
        <button type="button" onClick={() => submit(true)} disabled={value === "" || working}>{working ? "Saving…" : "Save and restart"}</button>
      </menu>
    </dialog>
  );
}

export function PersistentRotateCTA(props: {
  visible: boolean;
  secretName: string;
  affectedRunning: number;
  onRestart: () => Promise<void>;
  onDismiss: () => void;
}) {
  const [working, setWorking] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  if (!props.visible) return null;
  if (props.affectedRunning === 0) {
    // Toast-only path; CTA suppressed (memo D4 + Codex memo-R1 P3).
    return null;
  }
  return (
    <div class="banner banner-info" data-testid="rotate-cta">
      <p>Vault updated for <code>{props.secretName}</code>. {props.affectedRunning} running daemon(s) still using the previous value.</p>
      <button
        type="button"
        disabled={working}
        onClick={async () => {
          setWorking(true);
          setErr(null);
          try {
            await props.onRestart();
            props.onDismiss();
          } catch (e) {
            setErr((e as Error).message);
          } finally {
            setWorking(false);
          }
        }}
      >
        {working ? "Restarting…" : "Restart now"}
      </button>
      <button type="button" onClick={() => props.onDismiss()}>Dismiss</button>
      {err && <p class="error">Restart failed: {err}</p>}
    </div>
  );
}

export function RotateResultBanner(props: {
  visible: boolean;
  result: SecretsRotateResult | null;
  onRetry: () => Promise<void>;
  onDismiss: () => void;
}) {
  const [working, setWorking] = useState(false);
  const [retryErr, setRetryErr] = useState<string | null>(null);
  if (!props.visible || !props.result) return null;
  const failed = props.result.restart_results.filter((r) => r.error !== "");
  if (failed.length === 0) {
    return (
      <div class="banner banner-success" data-testid="rotate-banner">
        <p>Vault updated. {props.result.restart_results.length} daemon(s) restarted.</p>
        <button type="button" onClick={() => props.onDismiss()}>Dismiss</button>
      </div>
    );
  }
  const total = props.result.restart_results.length;
  const ok = total - failed.length;
  return (
    <div class="banner banner-warn" data-testid="rotate-banner-partial">
      <p>Vault updated. {ok}/{total} daemons restarted. {failed.length} still need restart to use the new value.</p>
      <ul>
        {failed.map((f) => <li key={f.task_name}><code>{f.task_name}</code>: {f.error}</li>)}
      </ul>
      {retryErr && <p class="error">{retryErr}</p>}
      <button
        type="button"
        disabled={working}
        onClick={async () => {
          // Codex plan-R2 P1: do NOT dismiss after retry. The parent
          // calls setRotateResult with the fresh restart_results so the
          // banner re-renders with whatever still failed. If retry
          // throws (orchestration crash), surface the error inline.
          setWorking(true);
          setRetryErr(null);
          try {
            await props.onRetry();
          } catch (e) {
            setRetryErr((e as Error).message);
          } finally {
            setWorking(false);
          }
        }}
      >
        {working ? "Retrying…" : "Retry failed restarts"}
      </button>
      <button type="button" onClick={() => props.onDismiss()}>Dismiss</button>
    </div>
  );
}
```

- [ ] **Step 7.A.2: Wire into `Secrets.tsx`**

Add to imports:

```tsx
import { PersistentRotateCTA, RotateResultBanner, RotateSecretModal } from "../components/RotateSecretModal";
import { restartSecret } from "../lib/secrets-api";
import type { SecretsRotateResult } from "../lib/secrets-api";
```

In `InitKeyedView`, lift rotate state:

```tsx
function InitKeyedView(props: { env: SecretsEnvelope; refresh: () => Promise<void> }) {
  const [addOpen, setAddOpen] = useState(false);
  const [prefill, setPrefill] = useState<string | undefined>(undefined);
  // Codex plan-R1 P1: rotateName must NOT be cleared when the modal closes,
  // because the persistent CTA / result banner still need to know which
  // secret was rotated to call POST /api/secrets/<name>/restart. The
  // banner owns its own dismissal, which clears bannerName.
  const [rotateName, setRotateName] = useState<string | null>(null);
  const [bannerName, setBannerName] = useState<string | null>(null);
  const [rotateResult, setRotateResult] = useState<SecretsRotateResult | null>(null);
  const [rotateMode, setRotateMode] = useState<"no-restart" | "with-restart" | null>(null);
  // Codex plan-R2 P1: track running-daemon counts via /api/status so the
  // CTA logic can suppress when 0 are running (memo D4 + Codex memo-R1 P3).
  // Fetch on mount and after each rotation so the count reflects the
  // current world.
  const [runningByServer, setRunningByServer] = useState<Record<string, number>>({});

  const refreshRunning = useCallback(async () => {
    try {
      const resp = await fetch("/api/status");
      if (!resp.ok) return;
      const rows = (await resp.json()) as Array<{ server: string; daemon: string; state: string }>;
      const counts: Record<string, number> = {};
      for (const r of rows) {
        if (r.state === "Running") {
          counts[r.server] = (counts[r.server] ?? 0) + 1;
        }
      }
      setRunningByServer(counts);
    } catch {
      // Best-effort: leave existing map. CTA falls back to refCount-only mode.
    }
  }, []);
  useEffect(() => { void refreshRunning(); }, [refreshRunning]);

  const closeRotate = () => setRotateName(null);
  const dismissBanner = () => { setBannerName(null); setRotateResult(null); setRotateMode(null); };

  const refCountFor = (name: string) =>
    props.env.secrets.find((s) => s.name === name)?.used_by.length ?? 0;

  // Codex plan-R2 P1: count of *running* daemons of servers that
  // reference this key. Used for CTA suppression (0 = toast, no CTA).
  const runningCountFor = (name: string): number => {
    const refs = props.env.secrets.find((s) => s.name === name)?.used_by ?? [];
    let total = 0;
    for (const r of refs) {
      total += runningByServer[r.server] ?? 0;
    }
    return total;
  };

  return (
    <div class="secrets-table">
      {/* ... Add button + table as before ... */}

      {rotateName && (
        <RotateSecretModal
          open={true}
          name={rotateName}
          refCount={refCountFor(rotateName)}
          runningCount={runningCountFor(rotateName)}
          onClose={closeRotate}
          onSaved={(result, mode) => {
            setBannerName(rotateName);   // capture name BEFORE rotateName is cleared by closeRotate
            setRotateResult(result);
            setRotateMode(mode);
            void props.refresh();
            void refreshRunning();
          }}
        />
      )}

      {rotateMode === "no-restart" && bannerName && (
        <PersistentRotateCTA
          visible={true}
          secretName={bannerName}
          affectedRunning={runningCountFor(bannerName)}
          onRestart={async () => {
            // Codex plan-R1 P1: surface partial failures from restart-now
            // instead of dismissing unconditionally. The banner stays visible
            // when the user retries; only success or explicit Dismiss clears it.
            const res = await restartSecret(bannerName);
            const failed = res.restart_results.filter((r) => r.error !== "");
            if (failed.length > 0) {
              throw new Error(`${failed.length} of ${res.restart_results.length} daemon(s) still failed: ` +
                failed.map((f) => `${f.task_name}: ${f.error}`).join("; "));
            }
          }}
          onDismiss={dismissBanner}
        />
      )}

      {rotateMode === "with-restart" && bannerName && (
        <RotateResultBanner
          visible={true}
          result={rotateResult}
          onRetry={async () => {
            // Codex plan-R1 P1: retry must update the banner with fresh
            // results (so remaining failures stay listed) instead of
            // dismissing. We swap rotateResult so the banner re-renders.
            const res = await restartSecret(bannerName);
            // Synthesize a SecretsRotateResult-shaped result so the banner
            // renders the same partial-failure UI on retry.
            setRotateResult({ vault_updated: true, restart_results: res.restart_results });
          }}
          onDismiss={dismissBanner}
        />
      )}

      {/* SecretRowComponent click: setRotateName(s.name) */}
    </div>
  );
}
```

Update `SecretRowComponent` Rotate button to call a passed-in callback:

```tsx
function SecretRowComponent(props: { row: SecretRow; onAddPrefill: (name: string) => void; onRotate: (name: string) => void; onDelete: (name: string) => void }) {
  // ...
  // <button type="button" disabled={!isPresent} onClick={() => props.onRotate(props.row.name)}>Rotate</button>
}
```

And pass `onRotate={(n) => setRotateName(n)}` from `InitKeyedView`.

- [ ] **Step 7.A.3: Typecheck**

```bash
cd internal/gui/frontend && npm run typecheck && cd ../../..
```

Expected: green.

### 7.B — Commit

- [ ] **Step 7.B.1: Commit**

```bash
git add internal/gui/frontend/src/components/RotateSecretModal.tsx internal/gui/frontend/src/screens/Secrets.tsx
git commit -m "feat(gui/frontend): A3-a RotateSecretModal — 3-button + CTA + retry banner

RotateSecretModal: native <dialog> with three buttons:
  Cancel
  Save without restart           → PUT restart=false
  Save and restart               → PUT restart=true

PersistentRotateCTA: shown after Save-without-restart when affected
running daemons > 0; Restart-now button calls POST /api/secrets/:key/restart
(§5.4a). 0-running case suppresses CTA per memo D4.

RotateResultBanner: shown after Save-and-restart; differentiates
all-OK from partial failure (207). Retry-failed-restarts button
also calls POST /api/secrets/:key/restart."
```

---

## Task 8: DeleteSecretModal — D5 escalation flow

**Files:**
- Create: `internal/gui/frontend/src/components/DeleteSecretModal.tsx`
- Modify: `internal/gui/frontend/src/screens/Secrets.tsx` — wire Delete button

### 8.A — Modal + escalation logic

- [ ] **Step 8.A.1: Create `DeleteSecretModal.tsx`**

```tsx
// internal/gui/frontend/src/components/DeleteSecretModal.tsx
import { useEffect, useRef, useState } from "preact/hooks";
import { deleteSecret, type APIError, type ManifestError, type UsageRef } from "../lib/secrets-api";

type Stage =
  | { kind: "closed" }
  | { kind: "deleting" }
  | { kind: "confirm-refs"; usedBy: UsageRef[] }
  | { kind: "confirm-scan-incomplete"; manifestErrors: ManifestError[] }
  | { kind: "error"; message: string };

interface Props {
  name: string | null;       // null = closed
  onClose: () => void;
  onDeleted: () => void;
}

export function DeleteSecretModal(props: Props) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const [stage, setStage] = useState<Stage>({ kind: "closed" });
  const [typed, setTyped] = useState("");

  useEffect(() => {
    if (props.name === null) {
      setStage({ kind: "closed" });
      setTyped("");
      if (dialogRef.current?.open) dialogRef.current.close();
      return;
    }
    // First call: no confirm flag. Backend decides whether to escalate.
    setStage({ kind: "deleting" });
    if (dialogRef.current && !dialogRef.current.open) {
      dialogRef.current.showModal();
    }
    void firstAttempt(props.name);
  }, [props.name]);

  async function firstAttempt(name: string) {
    try {
      await deleteSecret(name);
      props.onDeleted();
      props.onClose();
    } catch (e) {
      const err = e as APIError;
      const body = (err.body ?? {}) as { used_by?: UsageRef[]; manifest_errors?: ManifestError[] };
      switch (err.code) {
        case "SECRETS_HAS_REFS":
          setStage({ kind: "confirm-refs", usedBy: body.used_by ?? [] });
          break;
        case "SECRETS_USAGE_SCAN_INCOMPLETE":
          setStage({ kind: "confirm-scan-incomplete", manifestErrors: body.manifest_errors ?? [] });
          break;
        default:
          // Codex plan-R1 P2: 404 SECRETS_KEY_NOT_FOUND on the first call
          // means another tab/CLI just deleted this key. Treat as success
          // (the user wanted it gone) and refresh.
          if (err.status === 404) {
            props.onDeleted();
            props.onClose();
            return;
          }
          setStage({ kind: "error", message: err.message });
      }
    }
  }

  async function confirmDelete() {
    if (props.name === null) return;
    try {
      await deleteSecret(props.name, { confirm: true });
      props.onDeleted();
      props.onClose();
    } catch (e) {
      // Codex plan-R2 P1: 404 on the confirmed call also means the key
      // was just deleted by another tab/CLI; treat as success per memo
      // §5.5 ("404 if just-deleted by another caller").
      const err = e as APIError;
      if (err.status === 404) {
        props.onDeleted();
        props.onClose();
        return;
      }
      setStage({ kind: "error", message: (e as Error).message });
    }
  }

  return (
    <dialog ref={dialogRef} onClose={() => props.onClose()} data-testid="delete-secret-modal">
      {stage.kind === "deleting" && (
        <div>
          <h2>Delete {props.name}?</h2>
          <p>Deleting…</p>
        </div>
      )}
      {stage.kind === "confirm-refs" && (
        <div>
          <h2>Delete {props.name}?</h2>
          <p>Deleting <code>{props.name}</code> will leave broken references in:</p>
          <ul>
            {stage.usedBy.map((u) => (
              <li key={`${u.server}/${u.env_var}`}><code>{u.server}</code> (env: <code>{u.env_var}</code>)</li>
            ))}
          </ul>
          <p>
            Manifests will not be modified. Running daemons will not restart, but
            future installs and restarts of these servers will fail until you
            provide the secret again or remove the references.
          </p>
          <p>Type <strong>DELETE</strong> to confirm.</p>
          <input
            type="text"
            value={typed}
            onInput={(e) => setTyped((e.target as HTMLInputElement).value)}
            data-testid="delete-confirm-input"
          />
          <menu>
            <button type="button" onClick={() => props.onClose()}>Cancel</button>
            <button type="button" disabled={typed !== "DELETE"} onClick={confirmDelete}>
              Delete vault key
            </button>
          </menu>
        </div>
      )}
      {stage.kind === "confirm-scan-incomplete" && (
        <div>
          <h2>Delete {props.name}?</h2>
          <p>Some manifests couldn't be scanned. We can't verify whether <code>{props.name}</code> is referenced.</p>
          <ul>
            {stage.manifestErrors.map((e) => (
              <li key={e.path}><code>{e.path}</code>: {e.error}</li>
            ))}
          </ul>
          <p>Type <strong>DELETE</strong> to delete anyway.</p>
          <input
            type="text"
            value={typed}
            onInput={(e) => setTyped((e.target as HTMLInputElement).value)}
            data-testid="delete-confirm-input"
          />
          <menu>
            <button type="button" onClick={() => props.onClose()}>Cancel</button>
            <button type="button" disabled={typed !== "DELETE"} onClick={confirmDelete}>
              Delete anyway
            </button>
          </menu>
        </div>
      )}
      {stage.kind === "error" && (
        <div>
          <h2>Delete failed</h2>
          <p class="error">{stage.message}</p>
          <menu>
            <button type="button" onClick={() => props.onClose()}>Close</button>
          </menu>
        </div>
      )}
    </dialog>
  );
}
```

- [ ] **Step 8.A.2: Wire into `Secrets.tsx`**

```tsx
import { DeleteSecretModal } from "../components/DeleteSecretModal";
```

In `InitKeyedView`:

```tsx
const [deleteName, setDeleteName] = useState<string | null>(null);
// ... and pass onDelete={(n) => setDeleteName(n)} to SecretRowComponent ...
// And render:
<DeleteSecretModal
  name={deleteName}
  onClose={() => setDeleteName(null)}
  onDeleted={() => { setDeleteName(null); void props.refresh(); }}
/>
```

- [ ] **Step 8.A.3: Typecheck + commit**

```bash
cd internal/gui/frontend && npm run typecheck && cd ../../..
```

Expected: green.

```bash
git add internal/gui/frontend/src/components/DeleteSecretModal.tsx internal/gui/frontend/src/screens/Secrets.tsx
git commit -m "feat(gui/frontend): A3-a DeleteSecretModal — strict D5 escalation

Click Delete → first DELETE /api/secrets/:key (no confirm flag).
Backend decides:
  204 → success, refresh snapshot
  409 SECRETS_HAS_REFS → modal with FRESH refs from response,
       typed-confirm input, second call with ?confirm=true
  409 SECRETS_USAGE_SCAN_INCOMPLETE → modal with manifest_errors,
       typed-confirm input, second call with ?confirm=true
  other 4xx/5xx → modal shows error

GUI never pre-decides based on cached used_by (Codex memo-R8 P1:
that reintroduces the stale-snapshot race D5 was written to remove)."
```

---

## Task 9: Asset regeneration + 14 E2E scenarios

**Files:**
- Create: `internal/gui/e2e/tests/secrets.spec.ts`
- Modify: `internal/gui/assets/{index.html,app.js,style.css}` (regenerated)

### 9.A — Regenerate embedded assets

- [ ] **Step 9.A.1: Run `go generate`**

```bash
go generate ./internal/gui/...
```

Expected: writes new `internal/gui/assets/{index.html,app.js,style.css}`.

- [ ] **Step 9.A.2: Verify the embed smoke test still passes**

```bash
go test ./internal/gui/ -count=1
```

Expected: green.

### 9.B — E2E scenarios (14 total)

- [ ] **Step 9.B.1: Create `internal/gui/e2e/tests/secrets.spec.ts` skeleton**

**Codex plan-R1 P2:** the existing E2E suite uses a Playwright `test.extend` fixture from `../fixtures/hub` that injects a per-test `hub: HubHandle` (see [internal/gui/e2e/fixtures/hub.ts:29](../../../internal/gui/e2e/fixtures/hub.ts#L29) and any existing `*.spec.ts`). Match that shape:

```ts
import { test, expect } from "../fixtures/hub";

test.describe("Secrets registry", () => {
  test("Empty-state init flow", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/secrets`);
    await expect(page.getByText("Secrets vault is not initialized")).toBeVisible();
    const initButton = page.getByRole("button", { name: "Initialize secrets vault" });
    await expect(initButton).toBeVisible();

    const responsePromise = page.waitForResponse(
      (r) => r.url().endsWith("/api/secrets/init") && r.request().method() === "POST",
    );
    await initButton.click();
    const resp = await responsePromise;
    expect(resp.status()).toBe(200);

    await expect(page.getByText("No secrets yet")).toBeVisible();
  });
});
```

The fixture is auto-applied per-test via `test.extend`; teardown (kill hub, clean tmpHome) runs in the fixture's `finally` block. Tests do NOT call any explicit `hub.stop()` — the fixture handles it.

- [ ] **Step 9.B.2-9.B.14: Add the remaining 13 scenarios**

For brevity, here are the scenario names and assertions; implement them following the same pattern as Step 9.B.1 (`startHubBin`, `await page.goto`, assert visible elements, intercept network with `page.waitForResponse`, etc.):

2. **"Add first secret"** — initialized empty vault → click Add → fill name+value → Save → assert `POST /api/secrets`, table now has 1 row.
3. **"Used-by counts populate from manifest scan"** — seed two manifests both referencing `K1` → assert `Used by: 2` and tooltip lists both servers.
4. **"Ghost ref displays for manifest-only key"** — empty vault, manifest references `WOLFRAM_APP_ID` → assert row with `referenced_missing` state.
5. **"Decrypt-failed vault → referenced_unverified"** — pre-create vault with wrong identity → assert `vault_state == decrypt_failed`, manifest refs shown as `referenced_unverified`.
6. **"Rotate Save without restart — 0 running suppresses CTA"** — vault with `K1`, no daemons running → click Rotate → Save without restart → assert no persistent CTA, only brief toast.
7. **"Rotate Save without restart — N running shows persistent CTA + Restart-now path"** — vault with `K1`, two daemons running → Save without restart → assert CTA visible → click Restart now → assert `POST /api/secrets/K1/restart`, CTA disappears.
8. **"Rotate Save and restart with partial failure"** — stub PUT to return 207 with mixed `restart_results` → assert banner "1/2 daemons restarted" + Retry button.
9. **"Delete unreferenced — single click"** — orphan `K2` → click Delete → assert ONE request `DELETE /api/secrets/K2` (no confirm), 204 → row disappears.
10. **"Delete with refs — escalation flow"** — `K1` referenced by `alpha` → click Delete → assert FIRST request returns 409 + `SECRETS_HAS_REFS` → modal opens with fresh refs → type `DELETE` → assert SECOND request `?confirm=true` → row updates.
11. **"Delete fails closed when scan incomplete"** — orphan `K1` with one corrupt manifest → click Delete → assert 409 + `SECRETS_USAGE_SCAN_INCOMPLETE` → typed-confirm modal → SECOND request succeeds.
12. **"Delete with refs — direct backend 409"** — direct `request.delete()` without confirm against referenced key → assert 409 + `SECRETS_HAS_REFS` body.
13. **"Banner shows mcphub secrets edit command"** — assert info banner contains literal text and copy button works.
14. **"Sidebar Secrets link routes correctly"** — navigate to `#/dashboard` → click Secrets → assert URL is `#/secrets` and screen renders.

Each scenario typically takes 30-60 lines. Use existing scenarios in `servers.spec.ts` and `add-server.spec.ts` as templates for fixture seeding (write manifest YAML to `hub.tmpHome`, populate vault by shelling out to `mcphub secrets set`, etc.).

- [ ] **Step 9.B.15: Run all E2E tests**

```bash
cd internal/gui/e2e && npm test
```

Expected: 14 new tests PASS, plus existing 52 still pass = 66 total.

### 9.C — Commit

- [ ] **Step 9.C.1: Verify all suites green**

```bash
go build ./...
go test ./...
cd internal/gui/frontend && npm test && npm run typecheck && cd ../../..
cd internal/gui/e2e && npm test && cd ../../..
```

Expected: all green.

- [ ] **Step 9.C.2: Commit**

```bash
git add internal/gui/assets internal/gui/e2e/tests/secrets.spec.ts
git commit -m "test(gui): A3-a E2E — 14 secrets.spec.ts scenarios + asset regen

Adds the secrets.spec.ts suite covering all 14 scenarios from memo
§7.3, including the D5 escalation flow (no cached snapshot pre-decide),
restart-now path via POST /api/secrets/:key/restart, scan-incomplete
fail-closed path, and decrypt-failed referenced_unverified rendering.

Suite count: 52 → 66."
```

---

## Task 10: Documentation alignment + manual smoke

**Files:**
- Modify: `CLAUDE.md` — E2E coverage section, count line
- Modify: `docs/superpowers/plans/phase-3b-ii-backlog.md` — mark §A row A3 done

### 10.A — `CLAUDE.md` updates

- [ ] **Step 10.A.1: Add `Secrets:` line under "What's covered"**

In `CLAUDE.md`'s GUI E2E "What's covered" block, after the "Logs:" line, add:

```markdown
- Secrets: empty-state init, Add modal, Used-by counts from manifest scan, ghost-refs for manifest-only keys, decrypt-failed degraded view, Rotate Save-without-restart with persistent CTA + Restart-now path via POST /restart, Rotate Save-and-restart with 207 partial-failure handling, Delete differential typed-confirm (single-click for unreferenced / typed DELETE for referenced) via D5 escalation flow, scan-incomplete fail-closed path, backend 409 guard verification, sidebar nav link, mcphub secrets edit banner.
```

- [ ] **Step 10.A.2: Update count line**

Replace the existing `52 smoke tests total ...` line with:

```markdown
66 smoke tests total (3 shell + 8 servers + 6 migration + 13 add-server + 17 edit-server + 2 dashboard + 3 logs + 14 secrets), ~38s wall-time on a warm machine.
```

### 10.B — Backlog mark-done

- [ ] **Step 10.B.1: Mark `§A row A3` done in `docs/superpowers/plans/phase-3b-ii-backlog.md`**

Find the `7. **A3** — Secrets screen` line and change the prefix to `7. **A3-a** — Secrets registry screen ✅` plus add a link to this plan and the resulting PR. Add a separate `**A3-b** — env.secret picker` line marked `(deferred)` so the next-up backlog item is explicit.

### 10.C — Manual smoke

- [ ] **Step 10.C.1: Run a real `mcphub gui` against an isolated home and exercise full flow**

**Codex plan-R1 P3:** point `LOCALAPPDATA` / `XDG_DATA_HOME` / `HOME` at a temporary directory so the smoke does not touch your real secrets vault.

On Windows PowerShell:

```powershell
$tmp = Join-Path $env:TEMP "mcphub-smoke-$(Get-Date -UFormat %s)"
New-Item -ItemType Directory -Path $tmp | Out-Null
$env:LOCALAPPDATA = $tmp
go run ./cmd/mcphub gui --no-browser --no-tray --port 9125
# Open http://127.0.0.1:9125/#/secrets in a browser
```

On Linux/macOS:

```bash
tmp=$(mktemp -d)
LOCALAPPDATA="$tmp" XDG_DATA_HOME="$tmp" HOME="$tmp" go run ./cmd/mcphub gui --no-browser --no-tray --port 9125
# Open http://127.0.0.1:9125/#/secrets in a browser
```

Walk through:
1. Init the vault (if not already initialized).
2. Add `OPENAI_API_KEY` with a dummy value.
3. Verify `Used by` shows the count if any seeded manifest references it.
4. Rotate it via "Save without restart" → verify persistent CTA appears (or toast if 0 running).
5. Click "Restart now" if the CTA appeared → verify network POST to `/api/secrets/OPENAI_API_KEY/restart`.
6. Delete it: if unreferenced, single click should succeed; if referenced, escalation modal should appear.

Confirm the Edit-vault banner is visible and the copy button works.

### 10.D — Final commit + PR prep

- [ ] **Step 10.D.1: Commit docs**

```bash
git add CLAUDE.md docs/superpowers/plans/phase-3b-ii-backlog.md
git commit -m "docs: A3-a coverage in CLAUDE.md + backlog mark-done

CLAUDE.md: Secrets line in 'What's covered'; count 52 → 66.
Backlog: §A row A3 split into A3-a (done) + A3-b (deferred env.secret
picker for AddServer/EditServer forms)."
```

- [ ] **Step 10.D.2: Verify branch state for PR**

```bash
git log --oneline master..HEAD
```

Expected: 10 commits matching memo §8.

```bash
git status
```

Expected: clean.

- [ ] **Step 10.D.3: Push branch (only with explicit user authorization — see below)**

> **Do NOT push without explicit user permission.** This plan reaches a clean
> committed state on a local branch; the user reviews, optionally runs the
> manual smoke, and decides when to push and open a PR.

Once authorized:

```bash
git push -u origin feat/phase-3b-ii-a3a-secrets-screen
gh pr create --title "Phase 3B-II A3-a — Secrets registry screen" --body "$(cat <<'EOF'
## Summary
- Adds the GUI Secrets registry at `#/secrets` with Add / Rotate / Delete flows.
- 6 new HTTP endpoints under `/api/secrets/*` plus 6 `api.go` wrappers.
- Tolerant manifest scan for "Used by" aggregation (embed-first / disk-fallback).
- D9 restart-result granularity refactor (existing `/api/servers/:name/restart` now returns 200/207/500 with per-task body); Dashboard.tsx consumer updated to surface partial failures.
- 14 new E2E scenarios; suite count 52 → 66.

## Test plan
- [x] `go test ./...`
- [x] Frontend Vitest: `cd internal/gui/frontend && npm test`
- [x] Frontend typecheck: `cd internal/gui/frontend && npm run typecheck`
- [x] E2E: `cd internal/gui/e2e && npm test` (66/66)
- [x] Manual smoke: init → add → rotate (both branches) → delete (both branches)

## Memo
docs/superpowers/specs/2026-04-25-phase-3b-ii-a3a-secrets-screen-design.md (rev 9, 13 Codex pre-execute review rounds, ~77 findings addressed)
EOF
)"
```

---

## Self-review checklist (run before declaring plan done)

- [ ] **Spec coverage:** every memo §3.1 deliverable has at least one task. (Backend wrappers → Task 1; D9 → Task 2; HTTP handlers → Task 3; frontend wrappers + hook → Task 4; screen → Task 5; modals → Tasks 6-8; E2E + assets → Task 9; docs → Task 10. ✅)
- [ ] **Placeholder scan:** no "TBD"/"TODO"/"implement later" outside of intentional console.log placeholders that are explicitly resolved by later tasks. ✅
- [ ] **Type consistency:** `api.RestartResult{TaskName, Err}` JSON tags `task_name`/`error` (NOT omitempty) — matches memo D9. ✅ `SecretsInitResult.VaultState` omitempty — matches memo §5.6. ✅ `SecretsDeleteError` typed with `UsedBy` and `ManifestErrors` — matches memo §5.5. ✅ Frontend types mirror Go shapes (`task_name`, `error`, `used_by` as `UsageRef[]`). ✅

---

**Plan complete.** Saved to `docs/superpowers/plans/2026-04-25-phase-3b-ii-a3a-secrets-screen.md`.
