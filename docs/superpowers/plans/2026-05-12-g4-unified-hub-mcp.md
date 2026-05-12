# G4 — Unified Hub MCP Endpoint Implementation Plan

**Spec:** `docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md` (master HEAD `41d60cd`, v3.2 amendment). All 72 cumulative findings folded in; treat the spec as final.

**Scope:** v0.3.0 G4 — single aggregated `/clients/{adapter-id}/mcp` endpoint per running `mcphub gui`, with persistent random port, persistent instance id, per-client tokens, 7-check auth gate, JSON-RPC fan-out, bidirectional install reconciler, and Settings toggle. Default-OFF; opt-in via `gui_server.hub_endpoint_enabled`.

**Effort:** ~10-12 days, decomposed into **5 phased PRs** (~2 days each, including bot + codex deep-sec cycles). Per-PR diff target ≤ ~1500 lines so the codex bot can review thoroughly. Each phase is implementable, testable, and committable independently; later phases consume types defined by earlier ones (no forward refs).

**PR workflow:** every phase follows the canonical PR workflow in `CLAUDE.md` (pre-push verification → port-9128 sweep → push → bot loop → codex deep-sec → bot re-trigger → merge). The KOSYAK examples there are load-bearing; reread before each push.

## Phase decomposition (overview)

| # | Phase | Deliverables | New files | Modified files | Estimated commits |
|---|---|---|---|---|---|
| 1 | Pre-gate + write hardening | manifest validator strict/compat modes; `SecureWriteClientConfig` handle-relative writer; state-dir DACL allowlist verifier | 4 | 5 | 6-8 |
| 2 | Endpoint state + tokens + redaction | persistent instance-id; endpoint state file; token table; `RedactToken` + golden test | 5 | 0 | 6-8 |
| 3 | Resolver + sessions + aggregator | atomic resolver snapshot; session store + idle sweeper + caps; fan-out aggregator + namespacing + partial-failure; `requestIDKey` | 4 | 0 | 6-8 |
| 4 | Handler + listener + control endpoint | 7-check auth gate; JSON-RPC dispatch; GET 405; DELETE; listener factory (windows `SO_EXCLUSIVEADDRUSE`, posix plain); bind ordering integration; `/internal/reload-tokens` | 4 | 1 | 6-8 |
| 5 | Install reconciler + CLI + UI | bidirectional reconciler with crash-safe add-before-remove; `mcphub hub-mcp` CLI; `gui --reset-port`; Settings toggle row + pending-restart badge; e2e | 2 | 4 | 6-8 |

Total ~30-40 commits across 5 PRs.

## Cross-phase invariants (apply to every phase)

- **TDD only.** Every code change starts with a failing `_test.go` file or `*.spec.ts` E2E. The order in each task is: write failing test → `go test ...` showing FAIL → write minimal impl → `go test ...` showing PASS → commit. No "implement later".
- **Verification gate** before push:
  - `go build ./...`
  - `go vet ./...`
  - `go test -count=1 -timeout 5m ./...`
  - `go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/`
  - For Phase 5: `cd internal/gui/e2e && npm test`
- **Process sweep** after every test run on Windows: `Get-Process -Name 'mcphub' -ErrorAction SilentlyContinue | Stop-Process -Force`. Tests spawn real `mcphub.exe` daemons.
- **State-dir resolver pattern.** All new state files reuse `api.OpenStateFile` for tests (with `test_state_path_env` tag) and `api.DaemonStateDir()` in production; the production binary excludes the env-fallback via the build tag (verified by the existing `state_paths_prod.go` + `state_paths_envfallback.go` split). Phase 1 adds `hub-mcp.*` to the small allowlist of permitted state-file names so `validateStateFileName` does not reject them.
- **Test style.** Plain `testing.T` with `t.Fatalf` / `t.Errorf` (no `testify`); `t.Run` for table cases when ≥ 3 cases share setup. Mirror `internal/api/health_test.go` and `internal/api/manifest_test.go` exactly. Background daemons in tests use `t.TempDir()` for state and call `t.Cleanup` to close listeners.
- **Frontend test style.** Phase 5 Playwright tests mirror `internal/gui/e2e/tests/settings.spec.ts` and `tests/secrets.spec.ts`: `import { test, expect } from "../fixtures/hub"` and `page.route("**/api/...")` stubs where the test must run on a clean `tmpHome`.
- **No backward compat for v0.3.0.** Gate default-OFF preserves every per-daemon URL; gate-ON is opt-in and clients must reinstall.

---

# Phase 1 — Pre-gate + Write Hardening

**Goal:** ship the prerequisite safety primitives every later phase depends on — manifest strict/compat validation, the handle-relative `SecureWriteClientConfig` writer, and the state-file DACL allowlist verifier. No hub endpoint yet; this PR is purely defensive.

**Acceptance:** strict mode rejects `__`-in-server-name; compat mode reads legacy manifests with a warning; `SecureWriteClientConfig(path, contents)` writes via openat-like + crypto/rand temp + handle-bound DACL; state-dir DACL verifier accepts vanilla single-user, rejects domain-user-add ACE.

**File scope:**
- Create: `internal/api/secure_write_client_config.go`, `internal/api/secure_write_windows.go` (build-tagged), `internal/api/secure_write_posix.go` (build-tagged), `internal/api/hub_mcp_state_dacl.go`, `internal/api/hub_mcp_state_dacl_windows.go`, `internal/api/hub_mcp_state_dacl_posix.go`.
- Modify: `internal/api/manifest.go` (strict/compat validation modes), `internal/config/manifest.go` (same in config loader), `internal/api/state_paths.go` (allowlist `hub-mcp.*` state-file names).

**Allowed change surface:** new files above; for modified files only the validation/path-allowlist sections. Do NOT touch `internal/api/install.go` yet — that is Phase 5.

**Must-not-break surfaces:** existing per-daemon URLs; existing manifest read paths in startup inventory + manifest listing; existing watchdog state file path resolution.

## Task 1.1 — Strict/compat manifest validation modes

**Files:**
- Modify: `internal/api/manifest.go` lines ~278-317 (`ManifestValidate` + `manifestValidationWarnings`).
- Modify: `internal/config/manifest.go` (the canonical parser).
- Create: `internal/api/manifest_validation_modes_test.go`.

**Steps:**

1. **Write failing test** in `internal/api/manifest_validation_modes_test.go`:

```go
package api

import (
	"strings"
	"testing"
)

func TestManifestValidateStrictRejectsDoubleUnderscore(t *testing.T) {
	a := NewAPI()
	yaml := "name: foo__bar\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9200\n"
	warnings, err := a.ManifestValidateMode(yaml, ValidateModeStrict)
	if err == nil {
		t.Fatalf("strict mode must reject __ in name; got nil error, warnings=%v", warnings)
	}
	if !strings.Contains(err.Error(), "__") {
		t.Errorf("error must name the offending substring '__'; got %v", err)
	}
}

func TestManifestValidateCompatWarnsOnDoubleUnderscore(t *testing.T) {
	a := NewAPI()
	yaml := "name: foo__bar\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9200\n"
	warnings, err := a.ManifestValidateMode(yaml, ValidateModeCompat)
	if err != nil {
		t.Fatalf("compat mode must accept __ in name; got err=%v", err)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "__") {
			found = true
		}
	}
	if !found {
		t.Errorf("compat mode must emit a __ warning; warnings=%v", warnings)
	}
}

func TestManifestValidateDefaultEqualsCompat(t *testing.T) {
	// Existing callers that use ManifestValidate (compat-equivalent) must keep working.
	a := NewAPI()
	yaml := "name: foo\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9200\n"
	_ = a.ManifestValidate(yaml) // must not panic, must not return non-nil err style
}
```

2. **Run + verify FAIL:** `go test -run TestManifestValidateStrictRejectsDoubleUnderscore -count=1 ./internal/api/` — expect "ManifestValidateMode undefined" or similar.

3. **Write minimal impl** in `internal/api/manifest.go`:

```go
// ValidateMode discriminates the __-substring policy in server names. Strict
// mode is applied at manifest mutation surfaces (create/edit/install + hub
// binding setup); compat mode at startup inventory + GUI manifest reads so
// legacy __-named manifests stay readable. Spec §"Pre-gate".
type ValidateMode int

const (
	ValidateModeCompat ValidateMode = iota // default; warn on __, accept
	ValidateModeStrict                     // reject __ in server names
)

// ManifestValidateMode is ManifestValidate with explicit mode. Returns
// (warnings, err). err != nil only in strict mode and only on hard
// violations (currently: __ in server name); warnings cover both modes.
// Existing ManifestValidate callers stay on compat-mode semantics.
func (a *API) ManifestValidateMode(yaml string, mode ValidateMode) ([]string, error) {
	reader := strings.NewReader(yaml)
	m, err := config.ParseManifest(reader)
	if err != nil {
		return []string{err.Error()}, nil
	}
	warnings := manifestValidationWarnings(m)
	if strings.Contains(m.Name, "__") {
		switch mode {
		case ValidateModeStrict:
			return warnings, fmt.Errorf("manifest name %q: '__' substring rejected in strict mode (reserved for tool-name namespacing)", m.Name)
		case ValidateModeCompat:
			warnings = append(warnings, fmt.Sprintf("manifest name %q contains '__' (deprecated; will be rejected in strict mode)", m.Name))
		}
	}
	return warnings, nil
}
```

4. **Apply same gate in `internal/config/manifest.go`'s `ServerManifest.Validate()`** by adding a `ValidateStrict()` variant; existing `Validate()` stays compat. Track which call sites need strict (install paths, bind-time gate). Add cross-file unit test ensuring the config and api layers agree on detection.

5. **Update `ManifestValidate` (existing) to call `ManifestValidateMode(..., ValidateModeCompat)` and discard err** — preserves all existing callers' signatures.

6. **Add bind-time gate hook** in `internal/api/manifest.go` exporting `func (a *API) ManifestValidateForHubBind(yaml string) error` that wraps strict mode and returns only `error` (warnings dropped). Phase 4 uses this from `gui/server.go` at hub-listener bind time.

7. **Run + verify PASS:** `go test -run TestManifestValidate -count=1 ./internal/api/` — all four tests green; existing `manifest_test.go` tests still green.

8. **Commit:**

```bash
git add internal/api/manifest.go internal/api/manifest_validation_modes_test.go internal/config/manifest.go
git commit -m "feat(g4-phase1): manifest validator strict + compat modes

Adds ValidateMode discriminator. Strict mode rejects '__' substring in
server names (used by hub bind-time gate + future mutation paths in
Phase 5). Compat mode warns but accepts (used by startup inventory +
manifest listing). Existing ManifestValidate stays on compat semantics
for caller compatibility. Bind-time helper ManifestValidateForHubBind
exposes strict-only error path for Phase 4's hub listener bring-up."
```

## Task 1.2 — State-file allowlist for hub-mcp.* names

**Files:**
- Modify: `internal/api/state_paths.go` (`validateStateFileName` allowlist).
- Create: `internal/api/state_paths_hubmcp_test.go`.

**Steps:**

1. **Write failing test:**

```go
package api

import (
	"errors"
	"testing"
)

func TestOpenStateFileAcceptsHubMcpNames(t *testing.T) {
	for _, name := range []string{
		"hub-mcp.lock",
		"hub-mcp-tokens.json",
		"hub-mcp.endpoint.json",
		"hub-mcp-control.token",
		"hub-mcp.log",
	} {
		if err := validateStateFileName(name); err != nil {
			t.Errorf("validateStateFileName(%q) = %v, want nil", name, err)
		}
	}
}

func TestOpenStateFileRejectsHubMcpTraversal(t *testing.T) {
	for _, name := range []string{
		"hub-mcp/../escape",
		"hub-mcp.lock/../etc",
		"../hub-mcp.lock",
	} {
		if err := validateStateFileName(name); !errors.Is(err, errStateNameInvalid) {
			t.Errorf("validateStateFileName(%q) = %v, want errStateNameInvalid", name, err)
		}
	}
}
```

2. **Run + verify PASS already** (the existing single-path-component check should already accept the bare names). If a path-separator regression appears, fix `validateStateFileName` to keep the single-component invariant.

3. **Commit** (bundled with 1.1 if minimal):

```bash
git add internal/api/state_paths.go internal/api/state_paths_hubmcp_test.go
git commit -m "chore(g4-phase1): assert validateStateFileName accepts hub-mcp.* names

Phase 2+ writes hub-mcp.lock, hub-mcp-tokens.json,
hub-mcp.endpoint.json, hub-mcp-control.token, hub-mcp.log under
DaemonStateDir(). The existing single-component check already accepts
them; this test pins the invariant so future renames stay safe."
```

## Task 1.3 — SecureWriteClientConfig (handle-relative, POSIX)

**Files:**
- Create: `internal/api/secure_write_client_config.go` (build-neutral shared types + entrypoint).
- Create: `internal/api/secure_write_posix.go` (`//go:build !windows`).
- Create: `internal/api/secure_write_client_config_test.go` (build-neutral happy-path + cross-platform DACL/perm assertions).

**Steps:**

1. **Write failing happy-path test:**

```go
package api

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSecureWriteClientConfigBasicRoundTrip(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "client-config.json")
	payload := []byte(`{"mcpServers":{"foo":{"url":"http://127.0.0.1:9200/mcp"}}}`)
	if err := SecureWriteClientConfig(target, payload); err != nil {
		t.Fatalf("SecureWriteClientConfig: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("content roundtrip = %q, want %q", got, payload)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len(payload)) {
		t.Errorf("size = %d, want %d", info.Size(), len(payload))
	}
}

func TestSecureWriteClientConfigRefusesSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	if err := os.WriteFile(real, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	err := SecureWriteClientConfig(link, []byte("{}"))
	if err == nil {
		t.Fatalf("SecureWriteClientConfig must refuse to overwrite a symlink target")
	}
}
```

2. **Run + verify FAIL** (function undefined).

3. **Write `secure_write_client_config.go`** with the shared signature + dispatch to platform impl:

```go
// SecureWriteClientConfig writes contents to path atomically via a
// handle-relative pipeline: open the parent dir, create a crypto/rand
// temp file with O_EXCL|O_NOFOLLOW, set the restrictive DACL on the
// HANDLE (Windows) or chmod the handle to 0600 (POSIX), write+fsync,
// atomic rename relative to the parent dir handle, re-open the final
// path via the SAME dir handle and re-verify the DACL.
//
// Spec §"SecureWriteClientConfig sequence". Defeats every classic
// path-based TOCTOU window: the dirHandle freezes the immediate
// parent, the temp name is unpredictable, the DACL is set on the
// handle before bytes hit disk, and the final re-verify catches
// policy ACLs that may auto-apply on certain Windows paths.
//
// Returns the first error from any step. Caller refuses to write the
// token + falls back to per-daemon URLs on any error.
func SecureWriteClientConfig(path string, contents []byte) error {
	return secureWriteClientConfigImpl(path, contents)
}
```

4. **Write `secure_write_posix.go`** with the openat + renameat sequence using `golang.org/x/sys/unix`:

```go
//go:build !windows

package api

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func secureWriteClientConfigImpl(path string, contents []byte) error {
	parentDir, base := filepath.Split(path)
	if parentDir == "" {
		parentDir = "."
	}
	dirFd, err := unix.Open(parentDir, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open parent %s: %w", parentDir, err)
	}
	defer unix.Close(dirFd)

	// POSIX: parent DACL verify reduces to owner-uid + non-loose mode.
	// The state-dir is per-user 0700; per-user client config dirs
	// (~/.claude.json's parent = $HOME, ~/.codex/, etc.) inherit the
	// same per-user trust boundary.
	if err := verifyPosixParentDirFromFd(dirFd); err != nil {
		return fmt.Errorf("parent dir %s not single-user safe: %w", parentDir, err)
	}

	randBytes := make([]byte, 8)
	if _, err := rand.Read(randBytes); err != nil {
		return fmt.Errorf("crypto/rand: %w", err)
	}
	tempName := fmt.Sprintf(".%s.tmp.%d.%s", base, os.Getpid(), hex.EncodeToString(randBytes))

	flags := unix.O_CREAT | unix.O_EXCL | unix.O_WRONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fileFd, err := unix.Openat(dirFd, tempName, flags, 0o600)
	if err != nil {
		return fmt.Errorf("openat temp %s: %w", tempName, err)
	}
	// Defense vs umask drift; 0600 was already set by O_CREAT, fchmod is a no-op
	// in the no-umask case and a fix otherwise.
	if err := unix.Fchmod(fileFd, 0o600); err != nil {
		_ = unix.Close(fileFd)
		_ = unix.Unlinkat(dirFd, tempName, 0)
		return fmt.Errorf("fchmod temp: %w", err)
	}
	if _, err := unix.Write(fileFd, contents); err != nil {
		_ = unix.Close(fileFd)
		_ = unix.Unlinkat(dirFd, tempName, 0)
		return fmt.Errorf("write temp: %w", err)
	}
	if err := unix.Fsync(fileFd); err != nil {
		_ = unix.Close(fileFd)
		_ = unix.Unlinkat(dirFd, tempName, 0)
		return fmt.Errorf("fsync temp: %w", err)
	}
	if err := unix.Renameat(dirFd, tempName, dirFd, base); err != nil {
		_ = unix.Close(fileFd)
		_ = unix.Unlinkat(dirFd, tempName, 0)
		return fmt.Errorf("renameat %s -> %s: %w", tempName, base, err)
	}
	_ = unix.Close(fileFd)

	verifyFd, err := unix.Openat(dirFd, base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("re-open %s: %w", base, err)
	}
	defer unix.Close(verifyFd)
	if err := verifyPosixFileFromFd(verifyFd); err != nil {
		return fmt.Errorf("post-rename verify %s: %w", base, err)
	}
	return nil
}

// verifyPosixParentDirFromFd + verifyPosixFileFromFd: stat from the fd,
// reject world/group-writable mode bits and non-owner uid.
func verifyPosixParentDirFromFd(fd int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("parent owned by uid %d, want %d", st.Uid, os.Getuid())
	}
	// 0o022 = group-write|world-write; parent dir at 0700 or 0750 is fine.
	if st.Mode&uint32(0o022) != 0 {
		return fmt.Errorf("parent mode %#o group/world-writable", st.Mode&0o777)
	}
	return nil
}

func verifyPosixFileFromFd(fd int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("file owned by uid %d, want %d", st.Uid, os.Getuid())
	}
	if st.Mode&uint32(0o077) != 0 {
		return fmt.Errorf("file mode %#o group/other-readable", st.Mode&0o777)
	}
	return nil
}
```

5. **Run + verify PASS** on POSIX:

```bash
GOOS=linux go test -run TestSecureWriteClientConfig -count=1 ./internal/api/
```

6. **Commit:**

```bash
git add internal/api/secure_write_client_config.go internal/api/secure_write_posix.go internal/api/secure_write_client_config_test.go
git commit -m "feat(g4-phase1): SecureWriteClientConfig — POSIX handle-relative writer

openat(parent, name, O_EXCL|O_NOFOLLOW|0600) + renameat(parent, temp,
parent, base) + post-rename Openat re-verify. Defeats path-based
TOCTOU on client config writes (~/.claude.json, ~/.codex/config.toml,
etc.). Windows impl follows in 1.4.

Refs spec section 'SecureWriteClientConfig sequence' (POSIX legs)."
```

## Task 1.4 — SecureWriteClientConfig (Windows handle-relative)

**Files:**
- Create: `internal/api/secure_write_windows.go` (`//go:build windows`).
- Modify: `internal/api/secure_write_client_config_test.go` (add Windows-only path coverage with `runtime.GOOS == "windows"` gate).

**Steps:**

1. **Write Windows-only failing test** in `secure_write_client_config_test.go`:

```go
func TestSecureWriteClientConfigWindowsHandleBoundDACL(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-specific")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "client-config.json")
	if err := SecureWriteClientConfig(target, []byte("{}")); err != nil {
		t.Fatalf("SecureWriteClientConfig: %v", err)
	}
	// Re-read DACL via x/sys/windows and assert allowlist-only.
	if err := verifyHubMcpStateDACL(target); err != nil {
		t.Fatalf("post-write DACL verify: %v", err)
	}
}
```

2. **Run + verify FAIL** (Windows impl undefined).

3. **Write `secure_write_windows.go`** with the NtCreateFile-relative-to-RootDirectory + NtSetInformationFile(FileRenameInformationEx) sequence. Use `golang.org/x/sys/windows` constants + `unsafe` for the NT-only structs. Sketch:

```go
//go:build windows

package api

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func secureWriteClientConfigImpl(path string, contents []byte) error {
	parentDir, base := filepath.Split(path)
	if parentDir == "" {
		parentDir = "."
	}
	// 1. Open parent dir with FILE_LIST_DIRECTORY + FILE_FLAG_BACKUP_SEMANTICS
	//    + FILE_FLAG_OPEN_REPARSE_POINT (refuses symlinks/junctions in
	//    the parent's last component).
	dirHandle, err := openDirHandleNoReparse(parentDir)
	if err != nil {
		return fmt.Errorf("open parent %s: %w", parentDir, err)
	}
	defer windows.CloseHandle(dirHandle)

	// 2. Handle-bound DACL verify on the parent dir.
	if err := verifyWindowsDACLFromHandle(dirHandle); err != nil {
		return fmt.Errorf("parent dir %s: %w", parentDir, err)
	}

	// 3. Compose unpredictable temp name.
	randBytes := make([]byte, 8)
	if _, err := rand.Read(randBytes); err != nil {
		return err
	}
	tempName := fmt.Sprintf(".%s.tmp.%d.%s", base, os.Getpid(), hex.EncodeToString(randBytes))

	// 4. NtCreateFile relative to dirHandle: DESIRED_ACCESS =
	//    DELETE | FILE_GENERIC_WRITE | SYNCHRONIZE | WRITE_DAC.
	//    CREATE_OPTIONS = FILE_NON_DIRECTORY_FILE | FILE_OPEN_REPARSE_POINT_FAIL.
	//    CREATE_DISPOSITION = FILE_CREATE.
	fileHandle, err := ntCreateRelative(dirHandle, tempName,
		windows.DELETE|windows.GENERIC_WRITE|windows.SYNCHRONIZE|windows.WRITE_DAC)
	if err != nil {
		return fmt.Errorf("ntcreate temp %s: %w", tempName, err)
	}
	closed := false
	defer func() { if !closed { windows.CloseHandle(fileHandle) } }()

	// 5. SetSecurityInfo(fileHandle, DACL_SECURITY_INFORMATION, nil, nil,
	//    restrictiveDACL, nil). restrictiveDACL grants GENERIC_ALL to
	//    {current-user-SID, LocalSystem, BuiltinAdministrators}.
	if err := setRestrictiveDACL(fileHandle); err != nil {
		_ = ntDeleteRelative(dirHandle, tempName)
		return fmt.Errorf("set DACL: %w", err)
	}

	// 6. WriteFile + FlushFileBuffers.
	if err := windowsWrite(fileHandle, contents); err != nil {
		_ = ntDeleteRelative(dirHandle, tempName)
		return fmt.Errorf("write temp: %w", err)
	}
	if err := windows.FlushFileBuffers(fileHandle); err != nil {
		_ = ntDeleteRelative(dirHandle, tempName)
		return fmt.Errorf("flush temp: %w", err)
	}

	// 7. NtSetInformationFile(fileHandle, FileRenameInformationEx,
	//    {Flags: REPLACE_IF_EXISTS|POSIX_SEMANTICS, RootDirectory: dirHandle, FileName: base}).
	//    fileHandle stays open across the call (codex r5 MED Windows requirement).
	if err := ntRenameRelative(fileHandle, dirHandle, base); err != nil {
		_ = ntDeleteRelative(dirHandle, tempName)
		return fmt.Errorf("ntrename %s -> %s: %w", tempName, base, err)
	}
	windows.CloseHandle(fileHandle)
	closed = true

	// 8. Re-open via dirHandle and re-verify DACL.
	verifyHandle, err := ntOpenRelative(dirHandle, base, windows.GENERIC_READ)
	if err != nil {
		return fmt.Errorf("re-open %s: %w", base, err)
	}
	defer windows.CloseHandle(verifyHandle)
	return verifyWindowsDACLFromHandle(verifyHandle)
}

// Helper signatures only — full impls cover ~250 lines of x/sys/windows
// NT syscall wiring (NtCreateFile, NtSetInformationFile,
// NtQuerySecurityObject, GetSecurityInfo + canonical DACL evaluation).
// Cross-referenced in detail in spec §"SecureWriteClientConfig sequence"
// (Windows leg) and §"Windows DACL verification".
```

The Windows helpers (`openDirHandleNoReparse`, `ntCreateRelative`, `ntRenameRelative`, `ntOpenRelative`, `ntDeleteRelative`, `setRestrictiveDACL`, `verifyWindowsDACLFromHandle`, `windowsWrite`) each wrap one `x/sys/windows.NewLazySystemDLL` call into `ntdll.dll` or use the already-exported `x/sys/windows.SetSecurityInfo` / `GetSecurityInfo` / `BuildExplicitAccessWithName` APIs. Use `unsafe.Pointer` only where the NT structs require it.

4. **Implement helpers** in the same file. For each NT-only call, define the struct + the LazyProc wrapper and document the Microsoft reference page in the comment header. Notable structs:
   - `FILE_RENAME_INFORMATION_EX` with `Flags uint32` (single bitfield holding `FILE_RENAME_REPLACE_IF_EXISTS=0x1 | FILE_RENAME_POSIX_SEMANTICS=0x2`), `RootDirectory uintptr`, `FileNameLength uint32`, then UTF-16 `FileName` inline.
   - `OBJECT_ATTRIBUTES`, `IO_STATUS_BLOCK`, `UNICODE_STRING`.

5. **Run + verify PASS on Windows:**

```powershell
Get-Process -Name 'mcphub' -ErrorAction SilentlyContinue | Stop-Process -Force
go test -run TestSecureWriteClientConfig -count=1 ./internal/api/
```

6. **Commit:**

```bash
git add internal/api/secure_write_windows.go internal/api/secure_write_client_config_test.go
git commit -m "feat(g4-phase1): SecureWriteClientConfig — Windows handle-relative writer

NtCreateFile-relative + NtSetInformationFile(FileRenameInformationEx,
RootDirectory=dirHandle) + post-rename re-open via the same dir
handle. DACL set on the file handle before any bytes hit disk; final
re-verify catches policy ACLs applied between rename and close.
Flags field uses the single bitfield form (REPLACE_IF_EXISTS |
POSIX_SEMANTICS), not the legacy non-EX bool — codex r6 MED closure.

Refs spec section 'SecureWriteClientConfig sequence' (Windows leg)
and 'Windows DACL verification'."
```

## Task 1.5 — State-dir DACL allowlist verifier

**Files:**
- Create: `internal/api/hub_mcp_state_dacl.go` (build-neutral entry + types).
- Create: `internal/api/hub_mcp_state_dacl_windows.go` (`//go:build windows`).
- Create: `internal/api/hub_mcp_state_dacl_posix.go` (`//go:build !windows`).
- Create: `internal/api/hub_mcp_state_dacl_test.go`.

**Steps:**

1. **Write failing tests** for the platform-neutral entrypoint `VerifyHubMcpStateDACL(path string) error`:

```go
func TestVerifyHubMcpStateDACLAcceptsFreshlyCreatedFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "hub-mcp-tokens.json")
	if err := SecureWriteClientConfig(target, []byte("{}")); err != nil {
		t.Fatalf("SecureWriteClientConfig: %v", err)
	}
	if err := VerifyHubMcpStateDACL(target); err != nil {
		t.Errorf("VerifyHubMcpStateDACL = %v, want nil for own freshly created file", err)
	}
}

func TestVerifyHubMcpStateDACLRejectsBroadlyReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix-specific; windows broad-SID case covered elsewhere")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "loose.json")
	if err := os.WriteFile(target, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	err := VerifyHubMcpStateDACL(target)
	if err == nil {
		t.Errorf("VerifyHubMcpStateDACL must reject 0644 mode (group/other readable)")
	}
}
```

2. **Run + verify FAIL** (entry undefined).

3. **Write the build-neutral entry** in `hub_mcp_state_dacl.go`:

```go
// VerifyHubMcpStateDACL opens path with reparse-defeat flags, stats from
// the open handle, and asserts the file is single-user-safe per the
// allowlist model in spec §"Windows DACL verification". On POSIX:
// owner uid + permission mask. On Windows: canonical DACL evaluation
// against {current-user-SID, LocalSystem, BuiltinAdministrators}.
//
// Errors: ErrIrregularFile, ErrWrongOwner, ErrTooLoose, ErrDaclOutsideAllowlist.
// Caller refuses to start the hub on any error and surfaces an
// operator-actionable diagnostic.
func VerifyHubMcpStateDACL(path string) error {
	return verifyHubMcpStateDACLImpl(path)
}

var (
	ErrIrregularFile           = errors.New("hub-mcp state file is a symlink or irregular")
	ErrWrongOwner              = errors.New("hub-mcp state file owner is not current user")
	ErrTooLoose                = errors.New("hub-mcp state file mode is group/world accessible")
	ErrDaclOutsideAllowlist    = errors.New("hub-mcp state file DACL grants read to a SID outside {current-user, LocalSystem, BuiltinAdministrators}")
)
```

4. **POSIX impl** (`hub_mcp_state_dacl_posix.go`):

```go
//go:build !windows

package api

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func verifyHubMcpStateDACLImpl(path string) error {
	f, err := os.OpenFile(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return ErrIrregularFile
		}
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Mode().Type()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return ErrIrregularFile
	}
	st := info.Sys().(*syscall.Stat_t)
	if st.Uid != uint32(os.Getuid()) {
		return ErrWrongOwner
	}
	if info.Mode().Perm()&0o077 != 0 {
		return ErrTooLoose
	}
	return nil
}
```

5. **Windows impl** (`hub_mcp_state_dacl_windows.go`): full canonical-ACE DACL evaluation. Use `GetSecurityInfo` to fetch owner SID + DACL handle; iterate ACEs via `GetAce`; map generic rights (`MapGenericMask`); skip DENY ACEs in the allowlist check unless they fully cover the unsafe ALLOW; reject if any read-capable ALLOW names a SID outside the allowlist. Roughly 200 lines; sketch:

```go
//go:build windows

func verifyHubMcpStateDACLImpl(path string) error {
	// Open with FILE_FLAG_OPEN_REPARSE_POINT|FILE_FLAG_BACKUP_SEMANTICS.
	pathW, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(pathW,
		windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return verifyWindowsDACLFromHandle(h)
}

// verifyWindowsDACLFromHandle (exported within-package; reused by
// SecureWriteClientConfig step 13).
func verifyWindowsDACLFromHandle(h windows.Handle) error {
	currentSID, err := currentUserSID()
	if err != nil {
		return err
	}
	systemSID, _ := windows.StringToSid("S-1-5-18")
	adminSID, _ := windows.StringToSid("S-1-5-32-544")
	allow := []*windows.SID{currentSID, systemSID, adminSID}

	var ownerSID *windows.SID
	var dacl *windows.ACL
	var sd windows.Handle // SECURITY_DESCRIPTOR memory must be LocalFree'd via secDescriptor.Release.
	if err := windows.GetSecurityInfo(h,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
		&ownerSID, nil, &dacl, nil, &sd); err != nil {
		return err
	}
	defer windows.LocalFree(sd)

	if !windows.EqualSid(ownerSID, currentSID) {
		return ErrWrongOwner
	}

	// Iterate ACEs via GetAce. For each read-capable ALLOW ACE:
	//   - resolve SID
	//   - if SID not in allow list, return ErrDaclOutsideAllowlist
	count := dacl.AceCount
	for i := uint32(0); i < uint32(count); i++ {
		var aceHdr *windows.ACE_HEADER
		if err := windows.GetAce(dacl, i, &aceHdr); err != nil {
			return err
		}
		// ACCESS_ALLOWED_ACE_TYPE = 0, ACCESS_DENIED_ACE_TYPE = 1.
		if aceHdr.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue // DENY ACEs don't widen access.
		}
		ace := (*accessAllowedAce)(unsafe.Pointer(aceHdr))
		mapped := mapGenericRights(ace.Mask)
		if mapped&(windows.FILE_GENERIC_READ|windows.GENERIC_READ) == 0 {
			continue // non-read ACEs don't matter for token confidentiality.
		}
		sid := sidFromAce(ace)
		if !sidInAllowlist(sid, allow) {
			return fmt.Errorf("%w: SID %s grants read", ErrDaclOutsideAllowlist, sid)
		}
	}
	return nil
}
```

6. **Run + verify PASS on POSIX:**

```bash
GOOS=linux go test -run TestVerifyHubMcpStateDACL -count=1 ./internal/api/
```

7. **Add Windows synthesis test** that builds a DACL with `BuildExplicitAccessWithName` adding an ALLOW for the Authenticated Users SID, applies it via `SetNamedSecurityInfo`, and asserts `VerifyHubMcpStateDACL` rejects with `ErrDaclOutsideAllowlist`. Reference spec §"Windows DACL verification — Tests".

8. **Run + verify PASS on Windows:**

```powershell
Get-Process -Name 'mcphub' -ErrorAction SilentlyContinue | Stop-Process -Force
go test -run TestVerifyHubMcpStateDACL -count=1 ./internal/api/
```

9. **Commit:**

```bash
git add internal/api/hub_mcp_state_dacl.go internal/api/hub_mcp_state_dacl_windows.go internal/api/hub_mcp_state_dacl_posix.go internal/api/hub_mcp_state_dacl_test.go
git commit -m "feat(g4-phase1): VerifyHubMcpStateDACL allowlist-based hardening

POSIX: owner-uid + mode mask check.
Windows: canonical DACL evaluation; read-capable ALLOW ACEs must
target only {current-user, LocalSystem, BuiltinAdministrators}. Any
inherited domain/everyone/managed-app ALLOW with read access rejects
the file — enterprise stance documented in spec.

Phase 2 wires this into the hub-mcp-tokens.json + hub-mcp.endpoint.json
load path. Refs spec section 'Windows DACL verification' allowlist
form (codex r3 security F-S3 closure)."
```

## Task 1.6 — Phase 1 verification

**Steps:**

1. `Get-Process -Name 'mcphub' -ErrorAction SilentlyContinue | Stop-Process -Force`
2. `go build ./...`
3. `go vet ./...`
4. `go test -count=1 -timeout 5m ./...`
5. `go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/`
6. Diff size sanity-check: `git diff master..HEAD --stat` should be ≤ ~1500 lines. (The bulk is `secure_write_windows.go` + `hub_mcp_state_dacl_windows.go`; tests are ~300 lines.)
7. Push, open PR, follow CLAUDE.md PR workflow steps 3-7.

**Rollback / safe fallback:** Phase 1 ships pure additions; rollback = revert PR. No production code path consumes `SecureWriteClientConfig` or `VerifyHubMcpStateDACL` yet (Phase 2 + 5 wire them in).

**Risks called out:**
- Windows NT-syscall wrappers are fiddly; budget extra time for codex deep-sec on `secure_write_windows.go`. Mandatory: pass `-c model_reasoning_effort=xhigh` to every codex review on Phase 1.
- DACL test fixtures depend on `BuildExplicitAccessWithName`; verify it's available in the x/sys version pinned in `go.mod` before starting.

---

# Phase 2 — Endpoint State + Tokens + Redaction

**Goal:** ship the on-disk state machinery: persistent instance id (generated once, rotated only by explicit CLI), endpoint state file (port + instance_id + pid + started_at), per-client token table, and the `RedactToken` helper with a golden test covering every emit surface. No HTTP yet; pure data layer.

**Acceptance:** instance_id generated once on first call, persisted, unchanged across simulated restarts; tokens generated + stored + reloaded via 0600 + DACL-verified writes; `RedactToken` replaces every `[0-9a-f]{64}` substring with `<token>`; golden test asserts zero plain-token bytes in `hub-mcp.log` + CLI stdout/stderr + install paths.

**File scope:**
- Create: `internal/api/hub_mcp_instance.go`, `internal/api/hub_mcp_state.go`, `internal/api/hub_mcp_tokens.go`, `internal/api/hub_mcp_log_redact.go`, `internal/api/hub_mcp_log.go`.
- Create tests: `internal/api/hub_mcp_instance_test.go`, `internal/api/hub_mcp_state_test.go`, `internal/api/hub_mcp_tokens_test.go`, `internal/api/hub_mcp_log_redaction_test.go`.

**Allowed change surface:** new files above only. No modifications.

**Must-not-break surfaces:** existing watchdog state files; existing log emit sites (Phase 2 only adds new redaction-aware ones).

## Task 2.1 — `hub_mcp_log_redact.go` + golden test scaffolding

**Files:**
- Create: `internal/api/hub_mcp_log_redact.go`.
- Create: `internal/api/hub_mcp_log_redaction_test.go`.

**Steps:**

1. **Write failing test:**

```go
package api

import (
	"strings"
	"testing"
)

func TestRedactTokenReplaces64HexLowercase(t *testing.T) {
	tok := strings.Repeat("a", 64)
	in := "url=http://127.0.0.1:9120/clients/claude-code/mcp token=" + tok + " ok"
	got := RedactToken(in)
	if strings.Contains(got, tok) {
		t.Errorf("token leaked: %q", got)
	}
	if !strings.Contains(got, "<token>") {
		t.Errorf("missing placeholder: %q", got)
	}
}

func TestRedactTokenLeavesShortHexAlone(t *testing.T) {
	in := "hash=" + strings.Repeat("b", 12) + " count=42"
	got := RedactToken(in)
	if got != in {
		t.Errorf("short hex must not be redacted: in=%q got=%q", in, got)
	}
}

func TestRedactTokenHandlesMultipleOccurrences(t *testing.T) {
	tok1 := strings.Repeat("a", 64)
	tok2 := strings.Repeat("b", 64)
	in := "t1=" + tok1 + " t2=" + tok2
	got := RedactToken(in)
	if strings.Count(got, "<token>") != 2 {
		t.Errorf("expected 2 placeholders; got %q", got)
	}
}
```

2. **Run + verify FAIL.**

3. **Write impl:**

```go
package api

import "regexp"

// hexTokenRE matches the 64-hex-char lowercase form of hub-mcp tokens
// and instance ids. Both share the same generated form (crypto/rand
// 32 bytes → lower-hex), so a single regex covers both. Anchored to
// word boundaries via the surrounding negative lookbehind is NOT
// available in RE2; we accept that any 64-hex run is potentially a
// token and redact it. The golden test catches false negatives.
var hexTokenRE = regexp.MustCompile(`[0-9a-f]{64}`)

// RedactToken replaces every 64-lower-hex substring with "<token>".
// Apply at every emit site: hub-mcp.log writes, CLI stdout/stderr,
// install/error paths, argv echoes, syscall error wrappers. Spec §
// "Logging hygiene + golden test" (F-S2 closure).
func RedactToken(s string) string {
	return hexTokenRE.ReplaceAllString(s, "<token>")
}
```

4. **Run + verify PASS.**

5. **Commit:**

```bash
git add internal/api/hub_mcp_log_redact.go internal/api/hub_mcp_log_redaction_test.go
git commit -m "feat(g4-phase2): RedactToken helper for log + CLI hygiene

Replaces 64-lower-hex substrings with <token> placeholder. Single
helper for every emit surface — hub-mcp.log writes, CLI stdout/stderr,
install/error wrappers, argv echoes. Golden test in 2.5 exercises
each surface end-to-end."
```

## Task 2.2 — `hub_mcp_state.go`: atomic write + load helper

**Files:**
- Create: `internal/api/hub_mcp_state.go`.
- Create: `internal/api/hub_mcp_state_test.go`.

**Steps:**

1. **Write failing test:**

```go
package api

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteHubMcpStateAtomicRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPHUB_STATE_DIR", dir) // requires test_state_path_env build tag
	if _, err := DaemonStateDir(); err != nil {
		t.Skipf("requires -tags=test_state_path_env: %v", err)
	}
	payload := []byte(`{"foo":"bar"}`)
	if err := writeHubMcpStateFile("hub-mcp.endpoint.json", payload); err != nil {
		t.Fatalf("writeHubMcpStateFile: %v", err)
	}
	target := filepath.Join(dir, "mcp-local-hub", "hub-mcp.endpoint.json")
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("roundtrip = %q want %q", got, payload)
	}
}

func TestReadHubMcpStateRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPHUB_STATE_DIR", dir)
	if _, err := DaemonStateDir(); err != nil {
		t.Skipf("requires -tags=test_state_path_env: %v", err)
	}
	sd, _ := DaemonStateDir()
	target := filepath.Join(sd, "hub-mcp.endpoint.json")
	real := filepath.Join(sd, "real.json")
	_ = os.WriteFile(real, []byte("{}"), 0600)
	if err := os.Symlink(real, target); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := readHubMcpStateFile("hub-mcp.endpoint.json"); err == nil {
		t.Fatalf("readHubMcpStateFile must reject symlink target")
	}
}
```

2. **Run + verify FAIL** (functions undefined).

3. **Write impl:**

```go
// writeHubMcpStateFile writes payload atomically: temp file at
// <stateDir>/<name>.tmp under O_CREAT|O_EXCL|0600, fsync, atomic
// rename to <name>. Caller holds flock(<stateDir>/hub-mcp.lock).
// Spec §"Token + endpoint state hardening" atomic-write block.
func writeHubMcpStateFile(name string, payload []byte) error {
	sd, err := DaemonStateDir()
	if err != nil {
		return err
	}
	if err := validateStateFileName(name); err != nil {
		return err
	}
	target := filepath.Join(sd, name)
	// Delegate to the SecureWriteClientConfig pipeline — same handle-
	// relative + DACL-bound writer used for client configs. State files
	// live inside the per-user state-dir so the parent-dir DACL is the
	// same single-user boundary.
	return SecureWriteClientConfig(target, payload)
}

// readHubMcpStateFile opens <stateDir>/<name> with reparse-defeat
// flags, stats from the handle, runs VerifyHubMcpStateDACL on the open
// handle, then returns the contents. Errors propagate without falling
// back to a parse of partial state (spec hardening §"Load-time
// validation").
func readHubMcpStateFile(name string) ([]byte, error) {
	sd, err := DaemonStateDir()
	if err != nil {
		return nil, err
	}
	if err := validateStateFileName(name); err != nil {
		return nil, err
	}
	target := filepath.Join(sd, name)
	if err := VerifyHubMcpStateDACL(target); err != nil {
		return nil, err
	}
	return os.ReadFile(target)
}

// acquireHubMcpLock acquires <stateDir>/hub-mcp.lock via gofrs/flock.
// Returns the *flock.Flock; caller defers Unlock. Used by every state-
// mutating path (token generate/rotate, endpoint-file create, install
// reconciler).
func acquireHubMcpLock() (*flock.Flock, error) { /* delegate to gofrs/flock */ }
```

4. **Run + verify PASS** with `-tags=test_state_path_env`.

5. **Commit.**

## Task 2.3 — `hub_mcp_instance.go`: persistent instance id + endpoint state

**Files:**
- Create: `internal/api/hub_mcp_instance.go`.
- Create: `internal/api/hub_mcp_instance_test.go`.

**Steps:**

1. **Write failing tests:**

```go
func TestEnsureHubInstancePersistsAcrossSimulatedRestarts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPHUB_STATE_DIR", dir)
	if _, err := DaemonStateDir(); err != nil {
		t.Skipf("requires -tags=test_state_path_env: %v", err)
	}
	// 1st start: generate.
	ep1, err := EnsureHubEndpoint(0 /* port */, 1234 /* pid */)
	if err != nil {
		t.Fatal(err)
	}
	if len(ep1.InstanceID) != 64 {
		t.Errorf("instance_id len = %d, want 64", len(ep1.InstanceID))
	}
	// 10 simulated restarts: instance_id must be unchanged.
	for i := 0; i < 10; i++ {
		epN, err := EnsureHubEndpoint(0, 1234+i)
		if err != nil {
			t.Fatalf("restart %d: %v", i, err)
		}
		if epN.InstanceID != ep1.InstanceID {
			t.Errorf("restart %d: instance_id rotated unexpectedly", i)
		}
	}
}

func TestRotateHubInstanceIDRewritesFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPHUB_STATE_DIR", dir)
	if _, err := DaemonStateDir(); err != nil {
		t.Skipf("requires -tags=test_state_path_env: %v", err)
	}
	ep1, _ := EnsureHubEndpoint(0, 1234)
	ep2, err := RotateHubInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	if ep2.InstanceID == ep1.InstanceID {
		t.Errorf("RotateHubInstanceID did not rotate")
	}
	ep3, _ := EnsureHubEndpoint(0, 1234)
	if ep3.InstanceID != ep2.InstanceID {
		t.Errorf("Post-rotate Ensure must read back the rotated value")
	}
}

func TestResetHubPortClearsPortKeepsInstanceID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPHUB_STATE_DIR", dir)
	if _, err := DaemonStateDir(); err != nil {
		t.Skipf("requires -tags=test_state_path_env: %v", err)
	}
	ep1, _ := EnsureHubEndpoint(9120, 1234)
	if err := ResetHubPort(); err != nil {
		t.Fatal(err)
	}
	ep2, _ := EnsureHubEndpoint(0, 1234)
	if ep2.InstanceID != ep1.InstanceID {
		t.Errorf("--reset-port must NOT touch instance_id")
	}
	if ep2.Port == ep1.Port {
		t.Errorf("--reset-port must clear port for next ephemeral assignment")
	}
}
```

2. **Run + verify FAIL.**

3. **Write impl:**

```go
// HubEndpoint is the on-disk shape of hub-mcp.endpoint.json. Persistent
// across restarts; rotated only by explicit RotateHubInstanceID
// (and Port re-allocated only by ResetHubPort).
type HubEndpoint struct {
	Port       int    `json:"port"`
	InstanceID string `json:"instance_id"`
	PID        int    `json:"pid"`
	StartedAt  string `json:"started_at"` // RFC3339Nano UTC
}

// EnsureHubEndpoint loads or generates hub-mcp.endpoint.json under
// flock. If the file exists and validates, returns the loaded
// endpoint. If it's missing OR Port == 0 in the loaded state OR the
// requested ephemeralPort > 0 (caller-supplied), the returned
// endpoint reflects the new port; instance_id is preserved across
// the rewrite. Spec §"Bind ordering" steps 3-7 + §"Hub instance ID".
//
// Caller convention: ephemeralPort = 0 on the first call (asks the
// loaded-state value); ephemeralPort > 0 after the listener.Addr()
// returns the OS-assigned port (rewrites file under same flock).
func EnsureHubEndpoint(ephemeralPort, pid int) (HubEndpoint, error) {
	lk, err := acquireHubMcpLock()
	if err != nil {
		return HubEndpoint{}, err
	}
	defer lk.Unlock()
	return ensureHubEndpointLocked(ephemeralPort, pid)
}

// ensureHubEndpointLocked: read existing file, generate fresh
// instance_id if absent, persist updated record.
func ensureHubEndpointLocked(ephemeralPort, pid int) (HubEndpoint, error) {
	var ep HubEndpoint
	raw, err := readHubMcpStateFile("hub-mcp.endpoint.json")
	if err == nil {
		if uerr := json.Unmarshal(raw, &ep); uerr != nil {
			return HubEndpoint{}, fmt.Errorf("hub-mcp.endpoint.json corrupt: %w", uerr)
		}
	}
	if ep.InstanceID == "" {
		var buf [32]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return HubEndpoint{}, err
		}
		ep.InstanceID = hex.EncodeToString(buf[:])
	}
	if ephemeralPort > 0 {
		ep.Port = ephemeralPort
	}
	ep.PID = pid
	ep.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
	payload, err := json.Marshal(ep)
	if err != nil {
		return HubEndpoint{}, err
	}
	if err := writeHubMcpStateFile("hub-mcp.endpoint.json", payload); err != nil {
		return HubEndpoint{}, err
	}
	return ep, nil
}

// LoadHubEndpoint reads without mutating; used by `mcphub hub-mcp status`
// and the install reconciler.
func LoadHubEndpoint() (HubEndpoint, error) {
	raw, err := readHubMcpStateFile("hub-mcp.endpoint.json")
	if err != nil {
		return HubEndpoint{}, err
	}
	var ep HubEndpoint
	if err := json.Unmarshal(raw, &ep); err != nil {
		return HubEndpoint{}, err
	}
	return ep, nil
}

// RotateHubInstanceID generates a fresh instance_id and rewrites the
// endpoint file. Triggered by `mcphub hub-mcp regenerate-instance-id`.
// Stale-id requests get 401 from the next bind. Holds hub-mcp.lock.
func RotateHubInstanceID() (HubEndpoint, error) {
	lk, err := acquireHubMcpLock()
	if err != nil {
		return HubEndpoint{}, err
	}
	defer lk.Unlock()
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return HubEndpoint{}, err
	}
	ep, _ := LoadHubEndpoint() // ignore err: missing file is fine
	ep.InstanceID = hex.EncodeToString(buf[:])
	ep.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
	payload, _ := json.Marshal(ep)
	if err := writeHubMcpStateFile("hub-mcp.endpoint.json", payload); err != nil {
		return HubEndpoint{}, err
	}
	return ep, nil
}

// ResetHubPort clears the persisted Port (but NOT instance_id) so the
// next EnsureHubEndpoint with ephemeralPort=0 produces a fresh OS
// allocation in the listener factory. Triggered by `mcphub gui
// --reset-port`. Holds hub-mcp.lock.
func ResetHubPort() error {
	lk, err := acquireHubMcpLock()
	if err != nil {
		return err
	}
	defer lk.Unlock()
	ep, err := LoadHubEndpoint()
	if err != nil {
		return err
	}
	ep.Port = 0
	payload, _ := json.Marshal(ep)
	return writeHubMcpStateFile("hub-mcp.endpoint.json", payload)
}
```

4. **Run + verify PASS.**

5. **Commit:**

```bash
git add internal/api/hub_mcp_instance.go internal/api/hub_mcp_instance_test.go
git commit -m "feat(g4-phase2): persistent hub instance id + endpoint state file

EnsureHubEndpoint generates instance_id once on first call and
preserves it across simulated restarts. RotateHubInstanceID is the
operator-driven invalidation path; ResetHubPort clears Port but
preserves InstanceID per spec §'--reset-port does not touch
instance_id'.

Spec §'Hub instance ID — persistent across restarts' (codex r3
security F-S1 closure)."
```

## Task 2.4 — `hub_mcp_tokens.go`: per-client token table

**Files:**
- Create: `internal/api/hub_mcp_tokens.go`.
- Create: `internal/api/hub_mcp_tokens_test.go`.

**Steps:**

1. **Write failing tests:**

```go
func TestEnsureHubTokensCreatesPerClientEntries(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPHUB_STATE_DIR", dir)
	if _, err := DaemonStateDir(); err != nil {
		t.Skipf("requires -tags=test_state_path_env: %v", err)
	}
	want := []string{"claude-code", "codex-cli", "cursor"}
	tbl, err := EnsureHubTokens(want)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range want {
		tok, ok := tbl.Tokens[c]
		if !ok {
			t.Errorf("missing client %s", c)
		}
		if len(tok) != 64 {
			t.Errorf("client %s token len = %d", c, len(tok))
		}
	}
}

func TestEnsureHubTokensIsIdempotent(t *testing.T) {
	// Second call with the same client set must return the same tokens.
	// Adding a new client must NOT rotate existing tokens.
	dir := t.TempDir()
	t.Setenv("MCPHUB_STATE_DIR", dir)
	if _, err := DaemonStateDir(); err != nil {
		t.Skipf("requires -tags=test_state_path_env: %v", err)
	}
	t1, _ := EnsureHubTokens([]string{"claude-code"})
	t2, _ := EnsureHubTokens([]string{"claude-code", "codex-cli"})
	if t1.Tokens["claude-code"] != t2.Tokens["claude-code"] {
		t.Errorf("claude-code token rotated on additive client install")
	}
}

func TestRotateHubTokenRotatesOnlyOneClient(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPHUB_STATE_DIR", dir)
	if _, err := DaemonStateDir(); err != nil {
		t.Skipf("requires -tags=test_state_path_env: %v", err)
	}
	t1, _ := EnsureHubTokens([]string{"claude-code", "codex-cli"})
	t2, err := RotateHubToken("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if t2.Tokens["claude-code"] == t1.Tokens["claude-code"] {
		t.Errorf("RotateHubToken did not rotate")
	}
	if t2.Tokens["codex-cli"] != t1.Tokens["codex-cli"] {
		t.Errorf("RotateHubToken should not touch other clients")
	}
}
```

2. **Run + verify FAIL.**

3. **Write impl** including the in-process `tokenTable atomic.Pointer[HubTokenTable]` swap (Phase 4 reload-tokens uses this), atomic-rename writes under flock, and constant-time compare helper:

```go
type HubTokenTable struct {
	Tokens map[string]string // client_id -> 64-hex token
}

var liveTokenTable atomic.Pointer[HubTokenTable]

// EnsureHubTokens ensures every named client has a token entry,
// generating new tokens for clients not yet present. Returns the
// current snapshot. Existing tokens are NEVER rotated by this
// function — that's RotateHubToken's job.
func EnsureHubTokens(clients []string) (HubTokenTable, error) {
	lk, err := acquireHubMcpLock()
	if err != nil {
		return HubTokenTable{}, err
	}
	defer lk.Unlock()
	return ensureHubTokensLocked(clients)
}

func ensureHubTokensLocked(clients []string) (HubTokenTable, error) {
	tbl, _ := loadHubTokensLocked()
	if tbl.Tokens == nil {
		tbl.Tokens = map[string]string{}
	}
	changed := false
	for _, c := range clients {
		if _, ok := tbl.Tokens[c]; !ok {
			var buf [32]byte
			if _, err := rand.Read(buf[:]); err != nil {
				return HubTokenTable{}, err
			}
			tbl.Tokens[c] = hex.EncodeToString(buf[:])
			changed = true
		}
	}
	if changed {
		if err := writeHubTokensLocked(tbl); err != nil {
			return HubTokenTable{}, err
		}
	}
	publishTokenTable(tbl)
	return tbl, nil
}

// RotateHubToken regenerates one client's token + atomically rewrites
// the file. Called by `mcphub hub-mcp regenerate-token --client X`
// (Phase 5). The CLI holds hub-mcp.lock continuously across steps
// 2-5 per spec §"Token-table reload on rotation" (codex r5 MED).
func RotateHubToken(client string) (HubTokenTable, error) {
	lk, err := acquireHubMcpLock()
	if err != nil {
		return HubTokenTable{}, err
	}
	defer lk.Unlock()
	tbl, _ := loadHubTokensLocked()
	if tbl.Tokens == nil {
		tbl.Tokens = map[string]string{}
	}
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return HubTokenTable{}, err
	}
	tbl.Tokens[client] = hex.EncodeToString(buf[:])
	if err := writeHubTokensLocked(tbl); err != nil {
		return HubTokenTable{}, err
	}
	publishTokenTable(tbl)
	return tbl, nil
}

// ReloadHubTokens is the in-process path called from the
// /internal/reload-tokens endpoint (Phase 4). Re-reads the file +
// publishes via the atomic pointer.
func ReloadHubTokens() (HubTokenTable, error) {
	raw, err := readHubMcpStateFile("hub-mcp-tokens.json")
	if err != nil {
		return HubTokenTable{}, err
	}
	var tbl HubTokenTable
	if err := json.Unmarshal(raw, &tbl); err != nil {
		return HubTokenTable{}, err
	}
	publishTokenTable(tbl)
	return tbl, nil
}

// CurrentTokenTable returns the live snapshot for the auth gate
// (Phase 4). Constant-time compare wraps this with subtle.ConstantTimeCompare.
func CurrentTokenTable() HubTokenTable {
	p := liveTokenTable.Load()
	if p == nil {
		return HubTokenTable{}
	}
	return *p
}

// ConstantTimeCompareToken returns 1 iff the header value matches the
// stored token for client. Wraps subtle.ConstantTimeCompare with the
// 64-hex-shape gate caller (the handler does the shape check first).
func ConstantTimeCompareToken(client, headerToken string) int {
	tbl := CurrentTokenTable()
	stored := tbl.Tokens[client]
	if len(stored) != 64 || len(headerToken) != 64 {
		return 0
	}
	return subtle.ConstantTimeCompare([]byte(stored), []byte(headerToken))
}

func publishTokenTable(tbl HubTokenTable) {
	cpy := HubTokenTable{Tokens: make(map[string]string, len(tbl.Tokens))}
	for k, v := range tbl.Tokens {
		cpy.Tokens[k] = v
	}
	liveTokenTable.Store(&cpy)
}
```

4. **Run + verify PASS.**

5. **Commit.**

## Task 2.5 — `hub_mcp_log.go`: structured event emitter + golden redaction test

**Files:**
- Create: `internal/api/hub_mcp_log.go`.
- Extend: `internal/api/hub_mcp_log_redaction_test.go` (add the multi-surface golden test).

**Steps:**

1. **Write `hub_mcp_log.go`** with a `LogHubMcpEvent(level, event string, fields map[string]any)` function that:
   - Marshals JSON Lines (`{"ts":"...","level":"...","event":"...",...}`).
   - Applies `RedactToken` to the marshalled output before writing.
   - Writes to `<state-dir>/hub-mcp.log` with 10MB rotation to `.log.1` (reuses watchdog log-rotation pattern from `internal/api/watchdog_audit.go` if it exists, else inlines the rotation logic).

2. **Write the golden test** covering every emit surface listed in spec §"Logging hygiene":

```go
func TestRedactionGoldenAcrossAllSurfaces(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPHUB_STATE_DIR", dir)
	if _, err := DaemonStateDir(); err != nil {
		t.Skipf("requires -tags=test_state_path_env: %v", err)
	}
	// Generate a fresh token through the production code path.
	tbl, err := EnsureHubTokens([]string{"claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	tok := tbl.Tokens["claude-code"]
	if len(tok) != 64 {
		t.Fatalf("token shape: %d", len(tok))
	}

	// Surface 1: hub-mcp.log via LogHubMcpEvent.
	LogHubMcpEvent("info", "test-event", map[string]any{
		"url": "http://127.0.0.1:9120/clients/claude-code/mcp?t=" + tok,
	})
	logBytes, _ := os.ReadFile(filepath.Join(dir, "mcp-local-hub", "hub-mcp.log"))
	if bytes.Contains(logBytes, []byte(tok)) {
		t.Errorf("token leaked into hub-mcp.log")
	}

	// Surface 2: error wrapper passing token-bearing path.
	errMsg := wrapHubMcpFileError("write", "/tmp/some-"+tok+"-path", os.ErrPermission).Error()
	if strings.Contains(errMsg, tok) {
		t.Errorf("token leaked into wrapped error: %q", errMsg)
	}

	// Surface 3: argv echo helper.
	echoed := redactArgvForLog([]string{"mcphub", "install", "--token", tok})
	if strings.Contains(strings.Join(echoed, " "), tok) {
		t.Errorf("token leaked into argv echo: %v", echoed)
	}

	// Surface 4: install error-path stub (Phase 5 wires the actual call;
	// here we just exercise the helper).
	stat := formatInstallStatusForLog("partial", "claude-code", "http://127.0.0.1:9120/clients/claude-code/mcp?t="+tok)
	if strings.Contains(stat, tok) {
		t.Errorf("token leaked into install status: %q", stat)
	}
}
```

3. **Write minimal helpers** (`wrapHubMcpFileError`, `redactArgvForLog`, `formatInstallStatusForLog`) — each pipes through `RedactToken` before returning. These are the central choke-points that Phases 4 + 5 use; concentrate the redaction here so no caller can leak a token by composing a string that bypasses the helpers.

4. **Run + verify PASS.**

5. **Commit:**

```bash
git add internal/api/hub_mcp_log.go internal/api/hub_mcp_log_redaction_test.go
git commit -m "feat(g4-phase2): hub-mcp.log emitter + golden redaction test

LogHubMcpEvent pipes every event through RedactToken before writing.
Golden test exercises 4 surfaces (log, wrapped error, argv echo,
install status) and asserts zero plain-token bytes across them.
Phases 4 + 5 wire their emit sites through these helpers; the
choke-point design means new emit sites cannot bypass redaction.

Spec §'Logging hygiene + golden test' (F-S2 closure)."
```

## Task 2.6 — Phase 2 verification

Same gate as Task 1.6. Diff target ≤ ~1200 lines. Push, follow PR workflow.

**Rollback / safe fallback:** still pure additions; no production code path consumes the new APIs. Revert PR = rollback.

---

# Phase 3 — Resolver + Sessions + Aggregator

**Goal:** ship the in-memory hub state: atomic resolver snapshot (route map keyed by exposed name), session store with idle sweeper + per-client caps + LRU eviction, and the fan-out aggregator with namespacing + partial-failure assembly. No HTTP listener yet; pure logic exercised by direct method calls in tests.

**Acceptance:** resolver snapshot atomic-swap is torn-read-safe under concurrent mutation; session store enforces `MaxSessionsPerClient=16` + `MaxSessionsGlobal=256` with 429 + LRU eviction; idle sweeper respects `inFlightCount`; aggregator surfaces partial failures with stage discriminators; `requestIDKey` losslessly canonicalizes JSON-RPC ids.

**File scope:**
- Create: `internal/api/hub_mcp_resolver.go`, `internal/api/hub_mcp_session.go`, `internal/api/hub_mcp_aggregator.go`, `internal/api/hub_mcp_request_id.go`.
- Create tests: `internal/api/hub_mcp_resolver_test.go`, `internal/api/hub_mcp_session_test.go`, `internal/api/hub_mcp_aggregator_test.go`, `internal/api/hub_mcp_request_id_test.go`.

**Allowed change surface:** new files only. No modifications. Phase 4 wires these to the HTTP handler.

**Must-not-break surfaces:** none — pure new code.

## Task 3.1 — `hub_mcp_request_id.go`: lossless JSON-RPC id canonicalization

**Files:**
- Create: `internal/api/hub_mcp_request_id.go`.
- Create: `internal/api/hub_mcp_request_id_test.go`.

**Steps:**

1. **Write failing tests** covering every case from spec §"requestIDKey":

```go
func TestNewRequestIDKeyStringForm(t *testing.T) {
	key, err := newRequestIDKey(json.RawMessage(`"abc"`))
	if err != nil {
		t.Fatal(err)
	}
	if key != "s:abc" {
		t.Errorf("got %q want s:abc", key)
	}
}

func TestNewRequestIDKeyIntegerCanonical(t *testing.T) {
	// Spec §requestIDKey: 1, 1.0, 1.00, 1e0, 1E+0 must all collapse to n:1.
	cases := []string{`1`, `1.0`, `1.00`, `1e0`, `1E+0`}
	for _, in := range cases {
		key, err := newRequestIDKey(json.RawMessage(in))
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if key != "n:1" {
			t.Errorf("%s -> %q, want n:1", in, key)
		}
	}
}

func TestNewRequestIDKeyFractionalPreserves(t *testing.T) {
	cases := map[string]requestIDKey{
		`1.5`:    "n:1.5",
		`1.50`:   "n:1.5",
		`1.5e0`:  "n:1.5",
	}
	for in, want := range cases {
		key, err := newRequestIDKey(json.RawMessage(in))
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if key != want {
			t.Errorf("%s -> %q, want %q", in, key, want)
		}
	}
}

func TestNewRequestIDKeyBigIntegerStaysDistinct(t *testing.T) {
	// 9007199254740993 (= 2^53+1) MUST stay distinct from 9007199254740992
	// — float64 would collapse them. Spec §requestIDKey case (7).
	k1, _ := newRequestIDKey(json.RawMessage(`9007199254740992`))
	k2, _ := newRequestIDKey(json.RawMessage(`9007199254740993`))
	if k1 == k2 {
		t.Errorf("big integers collapsed: %q == %q", k1, k2)
	}
}

func TestNewRequestIDKeyRejectsNull(t *testing.T) {
	// Spec §requestIDKey: MCP forbids null ids. -32600.
	_, err := newRequestIDKey(json.RawMessage(`null`))
	if err == nil {
		t.Errorf("must reject null")
	}
}

func TestNewRequestIDKeyRejectsArrayObjectBoolean(t *testing.T) {
	for _, in := range []string{`[]`, `{}`, `true`, `false`} {
		if _, err := newRequestIDKey(json.RawMessage(in)); err == nil {
			t.Errorf("must reject %s", in)
		}
	}
}

func TestNewRequestIDKeyRejectsLeadingPlus(t *testing.T) {
	if _, err := newRequestIDKey(json.RawMessage(`+1`)); err == nil {
		t.Errorf("must reject leading +")
	}
}
```

2. **Run + verify FAIL.**

3. **Write impl** following spec §requestIDKey canonicalization rules (1)-(7):

```go
type requestIDKey string

func newRequestIDKey(raw json.RawMessage) (requestIDKey, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", errors.New("invalid request: empty id")
	}
	// Reject null (MCP 2025-11-25 narrows base spec).
	if bytes.Equal(trimmed, []byte("null")) {
		return "", errors.New("invalid request: MCP requires non-null id")
	}
	// Strings.
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return "", fmt.Errorf("invalid request: %w", err)
		}
		return requestIDKey("s:" + s), nil
	}
	// Numbers (per spec, use json.Number not float64 — preserve precision).
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var t any
	if err := dec.Decode(&t); err != nil {
		return "", fmt.Errorf("invalid request: %w", err)
	}
	num, ok := t.(json.Number)
	if !ok {
		// arrays / objects / booleans — rejected by MCP.
		return "", errors.New("invalid request: id must be string or number")
	}
	canon, err := canonicalizeJSONNumber(string(num))
	if err != nil {
		return "", err
	}
	return requestIDKey("n:" + canon), nil
}

// canonicalizeJSONNumber implements the steps in spec §requestIDKey:
// (1) reject leading +; (2) strip leading zeros from integer part keeping
// single 0 before decimal if int part is empty; (3) normalize exponent
// sign + strip leading zeros from exponent; (4) strip trailing zeros from
// fractional part + drop decimal point if fractional becomes empty.
//
// Returns canonical string OR err for malformed numbers (json.Number
// already grammar-validated, so this only fails on grammar edges the
// stdlib accepts but spec rejects, e.g. leading +).
func canonicalizeJSONNumber(s string) (string, error) {
	// ... pure string manipulation per spec steps (1)-(7). No big.Rat
	// alloc on hot path. Roughly 80 lines of careful string slicing
	// with table-driven test coverage from the test file above.
}
```

4. **Run + verify PASS.**

5. **Commit:**

```bash
git add internal/api/hub_mcp_request_id.go internal/api/hub_mcp_request_id_test.go
git commit -m "feat(g4-phase3): requestIDKey lossless JSON-RPC id canonicalization

Discriminated comparable form of JSON-RPC ids: s:<str> | n:<canon>.
String canonicalization passes through json.Unmarshal so escape
sequences resolve. Numeric canonicalization uses json.Number + pure
string manipulation — NOT float64 — so 2^53+1 stays distinct from
2^53.

Rejects null (MCP 2025-11-25), arrays, objects, booleans, leading +.

Spec §'requestIDKey' (codex r4 F6 + r5 P1 + r6 MED closures)."
```

## Task 3.2 — `hub_mcp_resolver.go`: atomic resolver snapshot

**Files:**
- Create: `internal/api/hub_mcp_resolver.go`.
- Create: `internal/api/hub_mcp_resolver_test.go`.

**Steps:**

1. **Write failing tests:**

```go
func TestPublishResolverSnapshotAtomicSwap(t *testing.T) {
	snap1 := buildResolverSnapshotFromManifests([]config.ServerManifest{ /* a small fixture */ })
	PublishResolverSnapshot(snap1)
	got1 := LoadResolverSnapshot()
	if got1 == nil || got1.Gen != snap1.Gen {
		t.Fatalf("snap1 not published")
	}

	snap2 := buildResolverSnapshotFromManifests([]config.ServerManifest{ /* updated fixture */ })
	PublishResolverSnapshot(snap2)
	got2 := LoadResolverSnapshot()
	if got2.Gen != snap2.Gen {
		t.Errorf("snap2 not published")
	}
	if got1.Gen == got2.Gen {
		t.Errorf("Gen did not advance")
	}
}

func TestResolverSnapshotConcurrentReadersSeeConsistent(t *testing.T) {
	// 4 goroutines call LoadResolverSnapshot in a loop; one publisher swaps
	// snapshots 1000x; readers must never see a torn snapshot (every loaded
	// pointer has all three fields consistent).
}

func TestBuildResolverSnapshotNamespacing(t *testing.T) {
	// Two servers with overlapping tool names: namespacing must produce
	// distinct keys "<server>__<rawname>" in the route map.
}
```

2. **Run + verify FAIL.**

3. **Write impl** matching spec §"Resolver state is published via atomic snapshot":

```go
type canonicalDaemonRef struct {
	Server string
	Daemon string
	Port   int
}

type canonicalToolRef struct {
	Server  string
	Daemon  string
	Port    int
	RawName string
}

type ResolverSnapshot struct {
	Gen      int64
	Bindings map[string][]canonicalDaemonRef // client_id -> daemons
	Routes   map[string]canonicalToolRef     // "<server>__<rawname>" -> canonical ref
}

var (
	resolverSnapshot atomic.Pointer[ResolverSnapshot]
	resolverGen      atomic.Int64
)

func PublishResolverSnapshot(snap *ResolverSnapshot) {
	resolverSnapshot.Store(snap)
}

func LoadResolverSnapshot() *ResolverSnapshot {
	return resolverSnapshot.Load()
}

// BuildResolverSnapshotFromManifests walks every manifest in the
// supplied set, expanding (server, daemon, client) bindings into the
// Bindings + Routes maps. Namespacing key = "<server>__<rawname>"
// (Routes is shared across all clients — per-client narrowing
// happens at session-init time via the SnapshotAtInit + path-client
// match). Spec §"Tool-name namespacing".
//
// Returns a fresh snapshot with Gen = next atomic counter. Caller
// invokes PublishResolverSnapshot to swap it in.
func BuildResolverSnapshotFromManifests(manifests []config.ServerManifest) *ResolverSnapshot {
	gen := resolverGen.Add(1)
	snap := &ResolverSnapshot{
		Gen:      gen,
		Bindings: map[string][]canonicalDaemonRef{},
		Routes:   map[string]canonicalToolRef{},
	}
	// ... walk manifests; populate Bindings + Routes. Routes uses the
	// raw tool name fetched from each daemon at probe time — but at
	// snapshot-build time we may not have the live tool list. Snapshot
	// here holds (server, daemon, port) bindings; per-session
	// Routes is populated by the aggregator at initialize time from
	// the per-daemon tools/list responses.
	//
	// Per spec §"Per-hub session model" each session captures the
	// snapshot pointer at initialize. The route map keyed by
	// exposed-name lives ON THE SESSION (session.RouteMap), populated
	// from the merged tools/list responses. The package-level Routes
	// map here is the canonical raw-name index per (server, daemon)
	// available at snapshot-build time, used to verify membership
	// during tools/call revalidation.
	return snap
}

// BumpResolverOnManifestChange rebuilds + publishes a fresh snapshot
// after any manifest add/edit/uninstall. Phase 5 install reconciler
// calls this; tests call it directly.
func BumpResolverOnManifestChange(manifests []config.ServerManifest) {
	PublishResolverSnapshot(BuildResolverSnapshotFromManifests(manifests))
}
```

4. **Run + verify PASS.**

5. **Commit.**

## Task 3.3 — `hub_mcp_session.go`: session store + idle sweeper + caps

**Files:**
- Create: `internal/api/hub_mcp_session.go`.
- Create: `internal/api/hub_mcp_session_test.go`.

**Steps:**

1. **Write failing tests:**

```go
func TestCreateSessionRejectsAtPerClientCap(t *testing.T) {
	store := NewHubSessionStore(SessionStoreOpts{MaxPerClient: 2, MaxGlobal: 100})
	_, _ = store.Create("claude-code", "2025-11-25", nil)
	_, _ = store.Create("claude-code", "2025-11-25", nil)
	_, err := store.Create("claude-code", "2025-11-25", nil)
	if !errors.Is(err, ErrSessionCapExceeded) {
		t.Errorf("got %v, want ErrSessionCapExceeded", err)
	}
}

func TestCreateSessionEvictsLRUAtGlobalCap(t *testing.T) {
	// Spec §"Hard caps + 429 with Retry-After at session caps":
	// new initialize at cap → 429 with Retry-After: 30. The handler
	// returns 429; the store side returns ErrSessionCapExceeded with
	// the per-client and global counters so the handler can decide.
}

func TestIdleSweeperRespectsInFlightCount(t *testing.T) {
	store := NewHubSessionStore(SessionStoreOpts{
		MaxPerClient:  16,
		MaxGlobal:     256,
		IdleTimeout:   30 * time.Minute,
		SweepInterval: 60 * time.Second,
		now:           func() time.Time { return time.Unix(0, 0) },
	})
	s, _ := store.Create("claude-code", "2025-11-25", nil)
	s.IncInFlight() // simulate in-flight tools/call
	// Advance clock 31 min and run one sweep tick — must NOT remove the session.
	store.now = func() time.Time { return time.Unix(31*60, 0) }
	store.sweepOnce()
	if _, ok := store.Get(s.ClientSessionID); !ok {
		t.Errorf("idle sweeper removed session with non-zero inFlightCount")
	}
}

func TestSessionInFlightInsertLookupRemove(t *testing.T) {
	s := newTestSession()
	key, _ := newRequestIDKey(json.RawMessage(`42`))
	entry := inflightEntry{DaemonRef: canonicalDaemonRef{Server: "fs", Daemon: "claude-code", Port: 9200}, DaemonSessionID: "sid-1"}
	s.InsertInFlight(key, entry)
	got, ok := s.LookupInFlight(key)
	if !ok || got.DaemonSessionID != "sid-1" {
		t.Fatalf("InsertInFlight/LookupInFlight broken: ok=%v got=%+v", ok, got)
	}
	s.RemoveInFlight(key)
	if _, ok := s.LookupInFlight(key); ok {
		t.Errorf("RemoveInFlight did not remove")
	}
}
```

2. **Run + verify FAIL.**

3. **Write impl** matching spec §"Per-hub session model" + §"Concurrency + bounds":

```go
type hubSession struct {
	ClientSessionID      string
	Client               string
	ProtocolVersion      string // captured at initialize (codex r7-bot)
	SnapshotAtInit       *ResolverSnapshot
	IntendedParticipants []canonicalDaemonRef
	InitSuccesses        map[canonicalDaemonRef]string // daemon Mcp-Session-Id
	InitFailures         []DaemonFailure
	RouteMap             atomic.Pointer[map[string]canonicalToolRef]
	InFlightRequests     map[requestIDKey]inflightEntry
	inflightMu           sync.Mutex
	inFlightCount        atomic.Int32
	InitAt               time.Time
	LastUsedAt           time.Time
	mu                   sync.Mutex
}

type inflightEntry struct {
	DaemonRef       canonicalDaemonRef
	DaemonSessionID string
	DaemonRequestID json.RawMessage
	StartedAt       time.Time
}

type DaemonFailure struct {
	Server string `json:"server"`
	Daemon string `json:"daemon"`
	Stage  string `json:"stage"` // initialize | tools/list | tools/call
	Err    string `json:"err"`
}

type HubSessionStore struct {
	opts        SessionStoreOpts
	mu          sync.RWMutex // protects sessions + perClient maps
	sessions    map[string]*hubSession
	perClient   map[string]int
	lru         *list.List
	lruIndex    map[string]*list.Element
	now         func() time.Time
	sweepCtx    context.Context
	sweepStop   context.CancelFunc
}

type SessionStoreOpts struct {
	MaxPerClient  int           // 16
	MaxGlobal     int           // 256
	IdleTimeout   time.Duration // 30 min
	SweepInterval time.Duration // 60 sec
}

var ErrSessionCapExceeded = errors.New("session cap exceeded")

func (s *HubSessionStore) Create(client, protoVer string, snap *ResolverSnapshot) (*hubSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perClient[client] >= s.opts.MaxPerClient {
		return nil, fmt.Errorf("per-client cap (%d): %w", s.opts.MaxPerClient, ErrSessionCapExceeded)
	}
	if len(s.sessions) >= s.opts.MaxGlobal {
		// LRU evict eldest.
		s.evictLRULocked()
	}
	id := generateSessionID()
	now := s.now()
	sess := &hubSession{
		ClientSessionID:  id,
		Client:           client,
		ProtocolVersion:  protoVer,
		SnapshotAtInit:   snap,
		InitSuccesses:    map[canonicalDaemonRef]string{},
		InFlightRequests: map[requestIDKey]inflightEntry{},
		InitAt:           now,
		LastUsedAt:       now,
	}
	s.sessions[id] = sess
	s.perClient[client]++
	s.lruIndex[id] = s.lru.PushFront(id)
	return sess, nil
}

func (s *HubSessionStore) Get(id string) (*hubSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[id]
	return sess, ok
}

func (s *HubSessionStore) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return false
	}
	delete(s.sessions, id)
	s.perClient[sess.Client]--
	if el, ok := s.lruIndex[id]; ok {
		s.lru.Remove(el)
		delete(s.lruIndex, id)
	}
	return true
}

func (s *HubSessionStore) sweepOnce() {
	cutoff := s.now().Add(-s.opts.IdleTimeout)
	s.mu.Lock()
	ids := make([]string, 0, len(s.sessions))
	for id, sess := range s.sessions {
		if sess.inFlightCount.Load() != 0 {
			continue // fast path: in-flight work
		}
		sess.mu.Lock()
		lastUsed := sess.LastUsedAt
		sess.mu.Unlock()
		if lastUsed.Before(cutoff) {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.Delete(id)
	}
}

// IncInFlight / DecInFlight / Insert / Lookup / Remove on *hubSession.
func (s *hubSession) IncInFlight() { s.inFlightCount.Add(1) }
func (s *hubSession) DecInFlight() { s.inFlightCount.Add(-1) }

func (s *hubSession) InsertInFlight(key requestIDKey, entry inflightEntry) {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	s.InFlightRequests[key] = entry
	s.IncInFlight()
}

func (s *hubSession) LookupInFlight(key requestIDKey) (inflightEntry, bool) {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	e, ok := s.InFlightRequests[key]
	return e, ok
}

func (s *hubSession) RemoveInFlight(key requestIDKey) {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if _, ok := s.InFlightRequests[key]; ok {
		delete(s.InFlightRequests, key)
		s.DecInFlight()
	}
}

// generateSessionID = 128-bit random + hex encode. Used as Mcp-Session-Id
// header value.
```

4. **Run + verify PASS.**

5. **Commit.**

## Task 3.4 — `hub_mcp_aggregator.go`: fan-out + partial-failure assembly

**Files:**
- Create: `internal/api/hub_mcp_aggregator.go`.
- Create: `internal/api/hub_mcp_aggregator_test.go`.

**Steps:**

1. **Write failing tests** for the three fan-out methods:

```go
func TestAggregateInitializePopulatesSuccessesAndFailures(t *testing.T) {
	// Stand up 2 fake daemon endpoints via httptest; one returns 200,
	// the other returns 503. Aggregator must record one success +
	// one failure with stage="initialize".
}

func TestAggregateToolsListMergesAndNamespaces(t *testing.T) {
	// Two daemons return {tools: [{name:"read"}]}. Merged list has
	// "fs1__read" + "fs2__read" entries. Session RouteMap populated.
	// _meta.mcphub.partialFailures empty.
}

func TestAggregateToolsListReportsAllFailedAsErrorMinus32000(t *testing.T) {
	// Both daemons return 503. Aggregator returns JSON-RPC error
	// {code: -32000, message: "all participating daemons failed",
	//  data.mcphub.partialFailures: [...]}.
}

func TestAggregateToolsCallCanonicalRewrite(t *testing.T) {
	// Session RouteMap has "fs1__read" -> canonicalToolRef{RawName:"read"}.
	// tools/call with params.name="fs1__read" forwards to fs1's daemon
	// with rewritten params.name="read" + hub-generated daemonRequestID.
}

func TestAggregateToolsCallStaleResolverRefuses(t *testing.T) {
	// Session captured snapshot Gen=1. Resolver has been republished
	// at Gen=2 and the (server, daemon, client) binding is gone.
	// tools/call must return -32601 "tool moved out of scope".
}
```

2. **Run + verify FAIL.**

3. **Write impl** with the methods called by Phase 4's handler:

```go
const (
	FanOutConcurrency        = 8
	PerDaemonInitTimeout     = 5 * time.Second
	PerDaemonListTimeout     = 10 * time.Second
	PerCallWallClockCap      = 60 * time.Second
)

// AggregateInitialize fans out initialize to every daemon in the
// session's IntendedParticipants under FanOutConcurrency.
// Populates session.InitSuccesses and session.InitFailures.
// Returns the synthetic initialize result body (server-info + tool
// capabilities advertised).
func AggregateInitialize(ctx context.Context, sess *hubSession) ([]byte, error) {
	// semaphore = make(chan struct{}, FanOutConcurrency)
	// for each participant: per-daemon ctx with PerDaemonInitTimeout
	// POST /mcp with initialize body, capture Mcp-Session-Id, parse
	// SSE-or-JSON response. Mirrors internal/api/health.go:687-805.
	// Failures append to session.InitFailures with stage="initialize".
}

// AggregateToolsList fans out tools/list to every daemon in
// InitSuccesses. Merges into a flat exposed-name route map keyed
// "<server>__<rawname>". Session.RouteMap atomic-swap with the
// freshly-built map. Returns the response body.
//
// _meta.mcphub.partialFailures combines stored InitFailures + list-
// time failures with stage="tools/list". If len(InitSuccesses)==0
// AND no list-time successes, return a -32000 error envelope.
func AggregateToolsList(ctx context.Context, sess *hubSession, reqID json.RawMessage) ([]byte, error) {
	// ... fan-out parse + merge + canonical rewrite.
}

// AggregateToolsCall looks up params.name in session.RouteMap.
// Loads the CURRENT resolverSnapshot via LoadResolverSnapshot and
// revalidates (Client, Server, Daemon) against it. On stale -> -32601
// "tool moved out of scope". On hit: rewrite params.name to RawName,
// generate daemonRequestID, InsertInFlight, forward to the daemon
// using the daemon's stored Mcp-Session-Id, await response (PerCall
// WallClockCap), RemoveInFlight, return response body.
//
// requestID for the client-facing response = the ORIGINAL client-
// supplied id (preserved across the forwarding).
func AggregateToolsCall(ctx context.Context, sess *hubSession, clientReqID json.RawMessage, params toolsCallParams) ([]byte, error) {
	// ... per spec §"tools/call".
}

// ForwardCancellation looks up the client request id in
// session.InFlightRequests, forwards notifications/cancelled with
// the daemon's request id, and removes the in-flight row.
//
// Stdio-daemon caveat per spec §"notifications/cancelled" doc.
func ForwardCancellation(ctx context.Context, sess *hubSession, clientReqID json.RawMessage) {
	// ... lookup + forward + remove.
}
```

4. **Run + verify PASS.**

5. **Commit:**

```bash
git add internal/api/hub_mcp_aggregator.go internal/api/hub_mcp_aggregator_test.go
git commit -m "feat(g4-phase3): hub-mcp fan-out aggregator + partial-failure assembly

AggregateInitialize / AggregateToolsList / AggregateToolsCall /
ForwardCancellation. Reuses SSE-or-JSON parsing pattern from
internal/api/health.go:687-805. FanOutConcurrency=8, per-daemon
init 5s, per-call 60s. Canonical rewrite of params.name on
tools/call. Resolver-snapshot revalidation refuses stale routes
with -32601 'tool moved out of scope'.

Spec §'Per-hub session model' + §'Partial-failure visibility'.
Phase 4 wires these to the HTTP handler."
```

## Task 3.5 — Phase 3 verification

Same gate. Diff target ≤ ~1500 lines.

**Rollback / safe fallback:** still pure additions; revert PR = rollback.

**Risks:** the concurrent-readers test in 3.2 needs `-race` to be meaningful — run `go test -race -count=1 ./internal/api/` before pushing as a self-falsification pass.

---

# Phase 4 — Handler + Listener + Control Endpoint

**Goal:** wire the 7-check auth gate, JSON-RPC dispatch, GET 405 fallback, DELETE session termination, listener factory (windows `SO_EXCLUSIVEADDRUSE` local constant + posix plain), bind ordering in `gui/server.go`, and the `/internal/reload-tokens` control endpoint. This is the first phase that opens an HTTP socket.

**Acceptance:** every 401 returns identical empty body; loopback-guard rejects DNS-rebind; MCP-Protocol-Version mismatch returns 400 with `-32600`; GET returns 405 with `Allow: POST, DELETE`; DELETE terminates session + fans out best-effort `DELETE /mcp` to daemons; `/internal/reload-tokens` rate-limits at 5s; bind ordering steps 1-7 followed; bind failure prints credential-rotation warning.

**File scope:**
- Create: `internal/api/hub_mcp_handler.go`, `internal/api/hub_mcp_listener_windows.go`, `internal/api/hub_mcp_listener_posix.go`, `internal/api/hub_mcp_control.go`.
- Create tests: `internal/api/hub_mcp_handler_test.go`, `internal/api/hub_mcp_listener_test.go`, `internal/api/hub_mcp_internal_reload_test.go`, `internal/api/hub_mcp_e2e_test.go`.
- Modify: `internal/gui/server.go` (bind hub listener after state ready; pass listener factory).

**Allowed change surface:** new files + `internal/gui/server.go` bind sequence + `Start` method.

**Must-not-break surfaces:** existing per-daemon URLs; existing GUI HTTP server on `gui_server.port`. The hub listener is a SEPARATE socket bound only when `gui_server.hub_endpoint_enabled=true`.

## Task 4.1 — Listener factory (windows + posix)

**Files:**
- Create: `internal/api/hub_mcp_listener_windows.go` (`//go:build windows`).
- Create: `internal/api/hub_mcp_listener_posix.go` (`//go:build !windows`).
- Create: `internal/api/hub_mcp_listener_test.go`.

**Steps:**

1. **Write failing tests** asserting the factory returns a `net.Listener` bound to 127.0.0.1; on Windows, asserts SO_EXCLUSIVEADDRUSE was set (second bind to the same port must fail synchronously, not race):

```go
func TestNewHubMcpListenerBinds(t *testing.T) {
	ln, err := newListenerWithSOExclusive("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().(*net.TCPAddr)
	if !addr.IP.IsLoopback() {
		t.Errorf("not loopback: %v", addr)
	}
}

func TestNewHubMcpListenerRejectsSecondBindWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("SO_EXCLUSIVEADDRUSE is windows-only")
	}
	ln1, err := newListenerWithSOExclusive("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln1.Close()
	port := ln1.Addr().(*net.TCPAddr).Port
	_, err = newListenerWithSOExclusive(fmt.Sprintf("127.0.0.1:%d", port))
	if err == nil {
		t.Errorf("second bind must fail with SO_EXCLUSIVEADDRUSE")
	}
}
```

2. **Write Windows impl** per spec §"Bind ordering" with the local constant + SetsockoptInt error capture:

```go
//go:build windows

package api

import (
	"context"
	"net"
	"syscall"

	"golang.org/x/sys/windows"
)

// soExclusiveAddrUse is ws2def.h's ((u_int)(~SO_REUSEADDR)) = -5.
// x/sys/windows does not export this constant — define locally per
// spec §'Bind ordering' (codex r4 F4 closure).
const soExclusiveAddrUse = ^windows.SO_REUSEADDR

func newListenerWithSOExclusive(addr string) (net.Listener, error) {
	lc := net.ListenConfig{
		Control: func(_, _ string, c syscall.RawConn) error {
			var setErr error
			ctlErr := c.Control(func(fd uintptr) {
				setErr = windows.SetsockoptInt(
					windows.Handle(fd),
					windows.SOL_SOCKET,
					soExclusiveAddrUse,
					1,
				)
			})
			if ctlErr != nil {
				return ctlErr
			}
			return setErr // F4: surface SetsockoptInt err
		},
	}
	return lc.Listen(context.Background(), "tcp", addr)
}
```

3. **Write POSIX impl** — plain ListenConfig per spec.

4. **Run + verify PASS.**

5. **Commit.**

## Task 4.2 — `hub_mcp_handler.go`: 7-check auth gate + JSON-RPC dispatch

**Files:**
- Create: `internal/api/hub_mcp_handler.go`.
- Create: `internal/api/hub_mcp_handler_test.go`.

**Steps:**

1. **Write failing tests** for each of the 7 gates:

```go
func TestHandlerLoopbackGuardRejectsNonLoopbackHost(t *testing.T) {
	// h := NewHubMcpHandler(...)
	// req := httptest.NewRequest("POST", "/clients/claude-code/mcp", body)
	// req.Host = "example.com"
	// w := httptest.NewRecorder()
	// h.ServeHTTP(w, req)
	// assert w.Code == 403
}

func TestHandlerUnknownClientReturns404EmptyBody(t *testing.T) { /* ... */ }

func TestHandlerTokenShapeRejects63Hex(t *testing.T) { /* 401 + empty body */ }

func TestHandlerWrongTokenReturns401Constant(t *testing.T) {
	// shape-valid but wrong token. Identical empty body as the 63-hex case.
}

func TestHandlerWrongInstanceIDReturns401(t *testing.T) { /* ... */ }

func TestHandlerMissingSessionIDOnNonInitializeReturns400(t *testing.T) {
	// codex r7-bot-r2 P2 closure
}

func TestHandlerCrossClientSessionReuseReturns401(t *testing.T) {
	// session created by claude-code; codex-cli path with matching client_id
	// rejects.
}

func TestHandlerProtocolVersionMismatchReturns400Minus32600(t *testing.T) { /* ... */ }

func TestHandlerInitializeUnsupportedVersionReturnsSyncJSONRPCError(t *testing.T) {
	// codex r7-bot-r2 P2: no session created on unsupported version.
}

func TestHandlerGETReturns405WithAllowHeader(t *testing.T) {
	// codex r7-bot-r5 P2: GET is exempt from Mcp-Session-Id + protocol-version.
	// 405 with `Allow: POST, DELETE`.
}

func TestHandlerDELETETerminatesSessionReturns204(t *testing.T) { /* ... */ }

func TestHandlerNotificationsCancelledForwardsAndRemoves(t *testing.T) { /* ... */ }
```

2. **Run + verify FAIL.**

3. **Write impl** wiring every gate per spec §"Cross-client invariant" 1-7:

```go
type HubMcpHandler struct {
	tokens      *atomic.Pointer[HubTokenTable] // points at api.liveTokenTable
	endpoint    *atomic.Pointer[HubEndpoint]   // points at the persisted endpoint
	sessions    *HubSessionStore
	supportedVer map[string]bool // {"2025-11-25":true, "2025-06-18":true, "2025-03-26":true}
}

func NewHubMcpHandler(store *HubSessionStore) *HubMcpHandler {
	return &HubMcpHandler{
		sessions:     store,
		supportedVer: map[string]bool{"2025-11-25": true, "2025-06-18": true, "2025-03-26": true},
	}
}

func (h *HubMcpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Gate 1: loopback-guard.
	if rejectUnsafeLoopbackRequest(w, r) { // see internal/daemon/loopback_guard.go
		return
	}
	// Gate 2: path -> canonical client.
	clientID, ok := parseClientPathFromURL(r.URL.Path) // "/clients/<id>/mcp"
	if !ok || !isSupportedClient(clientID) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	// GET fallback (skip gates 6-7 per codex r7-bot-r5 P2).
	if r.Method == http.MethodGet {
		// Run gates 3-5 (token + instance id), then 405.
		if !h.checkTokenAndInstanceID(w, r, clientID) {
			return
		}
		w.Header().Set("Allow", "POST, DELETE")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !h.checkTokenAndInstanceID(w, r, clientID) {
		return
	}
	// Gate 6 + 7: session-client + protocol version.
	switch r.Method {
	case http.MethodDelete:
		h.handleDelete(w, r, clientID)
	case http.MethodPost:
		h.handlePost(w, r, clientID)
	default:
		w.Header().Set("Allow", "POST, DELETE")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *HubMcpHandler) checkTokenAndInstanceID(w http.ResponseWriter, r *http.Request, clientID string) bool {
	// Gate 3: 64-hex shape.
	tok := r.Header.Get("X-Mcphub-Hub-Token")
	if len(tok) != 64 || !isLowerHex(tok) {
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	// Gate 4: constant-time compare.
	if ConstantTimeCompareToken(clientID, tok) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	// Gate 5: instance-id match.
	wantID := h.endpoint.Load().InstanceID
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Mcphub-Instance-Id")), []byte(wantID)) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	return true
}

func (h *HubMcpHandler) handlePost(w http.ResponseWriter, r *http.Request, clientID string) {
	// Parse JSON-RPC body.
	var body json.RawMessage
	// ... unmarshal; extract method, id, params.
	method, id, params, err := splitJSONRPC(body)
	if err != nil { /* return -32700 / -32600 */ }

	if method == "initialize" {
		// Mcp-Session-Id MUST be absent.
		if r.Header.Get("Mcp-Session-Id") != "" {
			http.Error(w, "session-id only valid after initialize", http.StatusBadRequest)
			return
		}
		// Initialize-time version rejection (codex r7-bot-r2 P2).
		var initParams struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(params, &initParams)
		if !h.supportedVer[initParams.ProtocolVersion] {
			h.writeJSONRPCError(w, id, -32600, "unsupported protocolVersion", map[string]any{
				"offered":   initParams.ProtocolVersion,
				"supported": []string{"2025-11-25", "2025-06-18", "2025-03-26"},
			})
			return
		}
		// Cap check, create session, fan-out initialize.
		snap := LoadResolverSnapshot()
		sess, err := h.sessions.Create(clientID, initParams.ProtocolVersion, snap)
		if errors.Is(err, ErrSessionCapExceeded) {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if err != nil { /* 500 */ }
		w.Header().Set("Mcp-Session-Id", sess.ClientSessionID)
		respBody, err := AggregateInitialize(r.Context(), sess)
		// ... write response.
		return
	}

	// Gate 6: session required for non-initialize.
	sid := r.Header.Get("Mcp-Session-Id")
	if sid == "" {
		http.Error(w, "Mcp-Session-Id required on non-initialize requests", http.StatusBadRequest)
		return
	}
	sess, ok := h.sessions.Get(sid)
	if !ok {
		h.writeJSONRPCError(w, id, -32600, "unknown session", nil)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if sess.Client != clientID {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	// Gate 7: protocol-version.
	pv := r.Header.Get("MCP-Protocol-Version")
	if pv == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if pv != sess.ProtocolVersion || !h.supportedVer[pv] {
		h.writeJSONRPCError(w, id, -32600, "protocol-version mismatch", nil)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// Dispatch.
	switch method {
	case "notifications/initialized":
		// 202 + fan-out 202.
	case "notifications/cancelled":
		// Lookup InFlightRequests + forward + remove.
		ForwardCancellation(r.Context(), sess, params /* requestId field */)
	case "ping":
		// Hub-local echo.
	case "tools/list":
		respBody, err := AggregateToolsList(r.Context(), sess, id)
		// ... write response.
	case "tools/call":
		// Parse params; AggregateToolsCall.
	default:
		h.writeJSONRPCError(w, id, -32601, "Method not found", nil)
	}
}

func (h *HubMcpHandler) handleDelete(w http.ResponseWriter, r *http.Request, clientID string) {
	sid := r.Header.Get("Mcp-Session-Id")
	if sid == "" {
		http.Error(w, "Mcp-Session-Id required on DELETE", http.StatusBadRequest)
		return
	}
	sess, ok := h.sessions.Get(sid)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if sess.Client != clientID {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	// Fan-out best-effort DELETE /mcp to each daemon session.
	// Even if every fan-out fails, return 204 + remove the hub session.
	for ref, dsid := range sess.InitSuccesses {
		_ = bestEffortDeleteDaemonSession(ref, dsid)
	}
	h.sessions.Delete(sid)
	w.WriteHeader(http.StatusNoContent)
}
```

4. **Run + verify PASS.** Test count: 12 named tests + table-driven matrix in `TestHandlerAuthGateMatrix` covering every cross-client / wrong-shape / wrong-instance combination.

5. **Commit.**

## Task 4.3 — `hub_mcp_control.go`: /internal/reload-tokens endpoint

**Files:**
- Create: `internal/api/hub_mcp_control.go`.
- Create: `internal/api/hub_mcp_internal_reload_test.go`.

**Steps:**

1. **Write failing tests** covering every threat-model row from spec §"Control endpoint contract":

```go
func TestInternalReloadRequiresPOST(t *testing.T) {
	for _, m := range []string{"GET", "PUT", "DELETE", "OPTIONS"} {
		// Expect 405 + `Allow: POST` header.
	}
}

func TestInternalReloadConstantTimeRejectsWrongToken(t *testing.T) {
	// 64-hex shape but wrong value. 401 empty body.
}

func TestInternalReloadAcceptsCorrectTokenSwaps(t *testing.T) {
	// Rotate token file on disk, POST with control token, assert
	// CurrentTokenTable() returns the rotated values.
}

func TestInternalReloadRateLimited5s(t *testing.T) {
	// Two consecutive valid reloads within 5s: first 204, second 429
	// with `Retry-After: 5`. After 5s elapses next valid reload again 204.
}

func TestInternalReloadConcurrentSerialize(t *testing.T) {
	// Two parallel POSTs: one wins (204), the other inherits its outcome
	// (204 if cooldown opened, 429 otherwise). Verifies reloadMutex usage.
}

func TestInternalReloadIgnoresPerClientHeader(t *testing.T) {
	// X-Mcphub-Hub-Token with control-token value: 401.
	// Separate keyspace per spec §'Control endpoint contract'.
}

func TestInternalReloadNoLeaksToHubMcpLog(t *testing.T) {
	// Successful reload emits {event:"tokens-reloaded", source:"internal-reload"}
	// with NO token bytes, NO instance id, NO source PID.
}
```

2. **Run + verify FAIL.**

3. **Write impl:**

```go
type internalReloadHandler struct {
	mu         sync.Mutex
	lastReload time.Time
	controlTok atomic.Pointer[string] // freshly generated per hub start
}

func NewInternalReloadHandler() *internalReloadHandler {
	h := &internalReloadHandler{}
	tok := generate64Hex() // crypto/rand 32 bytes -> lower hex
	h.controlTok.Store(&tok)
	// Persist <state-dir>/hub-mcp-control.token (0600 + DACL).
	_ = writeHubMcpStateFile("hub-mcp-control.token", []byte(tok+"\n"))
	return h
}

func (h *internalReloadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Loopback-guard first.
	if rejectUnsafeLoopbackRequest(w, r) {
		LogHubMcpEvent("warn", "internal-reload-rejected", map[string]any{"reason": "loopback"})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		LogHubMcpEvent("warn", "internal-reload-rejected", map[string]any{"reason": "method"})
		return
	}
	tok := r.Header.Get("X-Mcphub-Control-Token")
	stored := *h.controlTok.Load()
	if len(tok) != 64 || subtle.ConstantTimeCompare([]byte(tok), []byte(stored)) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		LogHubMcpEvent("warn", "internal-reload-rejected", map[string]any{"reason": "unauth"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.lastReload.IsZero() && time.Since(h.lastReload) < 5*time.Second {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	if _, err := ReloadHubTokens(); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	h.lastReload = time.Now()
	LogHubMcpEvent("info", "tokens-reloaded", map[string]any{"source": "internal-reload"})
	w.WriteHeader(http.StatusNoContent)
}

// Shutdown removes hub-mcp-control.token under flock.
func (h *internalReloadHandler) Shutdown() error {
	lk, err := acquireHubMcpLock()
	if err != nil {
		return err
	}
	defer lk.Unlock()
	sd, _ := DaemonStateDir()
	return os.Remove(filepath.Join(sd, "hub-mcp-control.token"))
}
```

4. **Run + verify PASS.**

5. **Commit.**

## Task 4.4 — Wire hub listener into `gui/server.go`

**Files:**
- Modify: `internal/gui/server.go` lines ~525-575 (`Start` method) to bind the hub listener AFTER state validation per spec §"Bind ordering" steps 1-9.
- Create: `internal/gui/hub_listener.go` (new file in the gui package owning the hub-listener lifecycle separate from the gui-server listener).

**Steps:**

1. **Write failing integration test** in `internal/gui/hub_listener_test.go`:

```go
func TestHubListenerBoundAfterStateReadyWithGateOn(t *testing.T) {
	// 1. Set MCPHUB_STATE_DIR to t.TempDir() (requires test_state_path_env)
	// 2. Write gui-preferences.yaml with gui_server.hub_endpoint_enabled=true
	// 3. Start the gui server in a goroutine
	// 4. Wait for ready signal
	// 5. GET http://127.0.0.1:<hub-port>/clients/claude-code/mcp without token -> 401
	// 6. GET hub.endpoint.json from state dir; assert port matches.
}

func TestHubListenerSkippedWithGateOff(t *testing.T) {
	// gate=false, assert no hub listener on any port; only the gui-server port.
}

func TestHubListenerBindFailureLogsCredentialWarning(t *testing.T) {
	// Pre-bind the persisted port from a sibling goroutine.
	// Start gui — must NOT bind. Assert the bind error is surfaced + the
	// "credentials may have leaked" warning is logged via hub-mcp.log.
}
```

2. **Write impl** in `internal/gui/hub_listener.go`:

```go
package gui

// startHubMcpListener implements the spec §"Bind ordering" steps 1-9.
// Called from Server.Start AFTER the gui-server listener is up and
// AFTER gui_server.hub_endpoint_enabled is confirmed true.
//
// Crash safety: if step 7 (endpoint.json write) fails after the
// listener exists, defers listener.Close() so no traffic is accepted
// without a published endpoint file.
func (s *Server) startHubMcpListener(ctx context.Context, enabled bool) error {
	if !enabled {
		return nil
	}
	// Step 1-5: validate <state-dir> sanity, flock, load/generate tokens,
	// load endpoint file, validate participating manifests in strict mode.
	if err := validateParticipatingManifestsForHubBind(s.api); err != nil {
		return fmt.Errorf("hub-mcp bind refused: %w", err)
	}
	clients, err := participatingClientsForHub(s.api)
	if err != nil {
		return err
	}
	if _, err := api.EnsureHubTokens(clients); err != nil {
		return err
	}
	ep, err := api.LoadHubEndpoint()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("hub-mcp endpoint file: %w", err)
	}
	// Step 6: listener factory.
	ln, err := api.NewListenerWithSOExclusive(fmt.Sprintf("127.0.0.1:%d", ep.Port))
	if err != nil {
		api.LogHubMcpEvent("error", "hub-bind-failed", map[string]any{
			"port": ep.Port,
			"err":  err.Error(),
		})
		// Credential-exfil warning per spec §"Pre-bind handling".
		api.LogHubMcpEvent("warn", "credential-rotation-required", map[string]any{
			"reason": "pre-bind window — credentials may have leaked to pre-binding process",
		})
		return err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	// Step 7: persist endpoint with the OS-assigned port if it was 0.
	if _, err := api.EnsureHubEndpoint(port, os.Getpid()); err != nil {
		ln.Close()
		return fmt.Errorf("hub-mcp endpoint.json write: %w", err)
	}
	// Step 8 implicit (flock released by Ensure).
	// Step 9: serve.
	mux := http.NewServeMux()
	store := api.NewHubSessionStore(api.SessionStoreOpts{
		MaxPerClient:  16,
		MaxGlobal:     256,
		IdleTimeout:   30 * time.Minute,
		SweepInterval: 60 * time.Second,
	})
	mux.Handle("/clients/", api.NewHubMcpHandler(store))
	reloadH := api.NewInternalReloadHandler()
	mux.Handle("/internal/reload-tokens", reloadH)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	go store.RunSweeper(ctx) // ticks every SweepInterval
	s.hubMcpSrv = srv
	s.hubMcpReloadH = reloadH
	return nil
}
```

3. **Wire into `Server.Start`** between the existing `s.srv = &http.Server{...}` and `close(ready)`. Pass `enabled := s.settings.Get("gui_server.hub_endpoint_enabled") == "true"`.

4. **Add shutdown hook** in the existing `case <-ctx.Done():` branch: `if s.hubMcpSrv != nil { s.hubMcpSrv.Shutdown(shutdownCtx); s.hubMcpReloadH.Shutdown() }`.

5. **Run + verify PASS.**

6. **Commit.**

## Task 4.5 — Phase 4 verification

Same gate + integration smoke test that drives the hub end-to-end via the test in 4.4. Diff target ≤ ~1500 lines.

**Rollback / safe fallback:** if anything goes wrong post-merge, set `gui_server.hub_endpoint_enabled=false` + restart — the hub listener never binds and the gui-server port behavior is unchanged.

**Risks:**
- The SO_EXCLUSIVEADDRUSE local constant is the kind of detail a deep-sec reviewer flags. Codex pass for this PR MUST run with `-c model_reasoning_effort=xhigh` on a dedicated `.scratch/codex-pr<N>-deep-security-windows-sockopt.md` prompt.
- The bind-failure credential-rotation warning is the only operator signal that a pre-bind attack may have happened. The test in 4.4 (`TestHubListenerBindFailureLogsCredentialWarning`) is load-bearing — do NOT skip it on POSIX (the SO_EXCLUSIVEADDRUSE test can skip; the warning emit-and-fail test must pass on both platforms).
- Idle sweeper goroutine leak: ensure `store.RunSweeper(ctx)` honors ctx cancellation and the `Server.Shutdown` waits for sweep stop.

---

# Phase 5 — Install Reconciler + CLI + UI

**Goal:** ship the operator surfaces: bidirectional install reconciler with crash-safe add-before-remove ordering; `mcphub hub-mcp` CLI (status + regenerate-token + regenerate-instance-id); `mcphub gui --reset-port`; Settings registry entry + Settings.tsx toggle row + pending-restart badge; Playwright e2e covering gate-OFF → gate-ON → regenerate flows.

**Acceptance:** gate-OFF/gate-ON round-trippable; partial-crash recovery converges via idempotent reconcile; `mcphub hub-mcp status` redacts every token; `mcphub hub-mcp regenerate-token --client X` rotates one client without restart and the next request with the OLD token returns 401 within 500ms; `--reset-port` clears port + emits credential-rotation warning; Settings toggle persists + restart badge appears.

**File scope:**
- Modify: `internal/api/install.go` (extend `ClientUpdatePlan` with `EntryName` + `Headers` + `Action` enum; add `BuildHubReconcilePlan` + `ApplyHubReconcileInOrder`; ALL existing client-config writes route through `SecureWriteClientConfig`).
- Create: `cmd/mcphub/hubmcp.go` (new `hub-mcp` subcommand under cobra root).
- Modify: `internal/cli/gui.go` (add `--reset-port` flag handler).
- Modify: `internal/api/settings_registry.go` (add `gui_server.hub_endpoint_enabled` entry).
- Modify: `internal/gui/frontend/src/components/settings/SectionGuiServer.tsx` (add the toggle row + restart badge).
- Create: `internal/gui/e2e/tests/hub-mcp.spec.ts`.

**Allowed change surface:** the files above. Touching unrelated code triggers `change-surface-minimization` review.

**Must-not-break surfaces:** existing `mcphub install` per-server path (must NOT remove `mcphub-hub` entries when invoked as `mcphub install --server X`); existing gui-server port settings; existing Settings UI for the other gui_server rows.

## Task 5.1 — Extend `ClientUpdatePlan` + bidirectional planner

**Files:**
- Modify: `internal/api/install.go` lines 98-104 (extend `ClientUpdatePlan`).
- Modify: `internal/api/install.go` lines 1036-1067 (per-server planner skip `Remove EntryName="mcphub-hub"`).
- Create: `internal/api/install_hub_reconcile.go` (new file owning the full-reconcile planner).
- Create: `internal/api/install_hub_reconcile_test.go`.

**Steps:**

1. **Write failing tests:**

```go
func TestBuildHubReconcilePlanGateOnAddsMcphubHubAndRemovesPerDaemon(t *testing.T) {
	manifests := []config.ServerManifest{ /* 2 manifests, 3 clients, all hub-enabled */ }
	endpoint := api.HubEndpoint{Port: 9120, InstanceID: strings.Repeat("a", 64)}
	tokens := api.HubTokenTable{Tokens: map[string]string{"claude-code": strings.Repeat("b", 64), "codex-cli": strings.Repeat("c", 64)}}
	plan, err := api.BuildHubReconcilePlan(manifests, endpoint, tokens, api.HubReconcileOpts{GateOn: true})
	if err != nil {
		t.Fatal(err)
	}
	// Assert plan has:
	//   - 1 AddReplace EntryName="mcphub-hub" per client with non-empty participating set
	//   - 1 Remove per existing per-(server, client) entry
}

func TestBuildHubReconcilePlanGateOffRemovesMcphubHubAndRestoresPerDaemon(t *testing.T) { /* mirror */ }

func TestApplyHubReconcileAddsBeforeRemoves(t *testing.T) {
	// Spec §"Crash-safe reconcile ordering" — within one client config
	// rewrite, AddReplace ops execute before Remove ops.
}

func TestPerServerInstallSkipsHubEntryRemoval(t *testing.T) {
	// `mcphub install --server X` MUST NOT emit Remove EntryName="mcphub-hub".
}
```

2. **Run + verify FAIL.**

3. **Extend `ClientUpdatePlan`** per spec §"Bidirectional install reconciler":

```go
type ClientUpdatePlan struct {
	Client     string
	Path       string
	Action     ClientUpdateAction
	EntryName  string            // "mcphub-hub" for aggregate; "<server>" for per-daemon
	URL        string            // empty for Remove
	Headers    map[string]string // F-G5: token + instance id; empty for per-daemon
	DaemonName string            // legacy; only meaningful for per-daemon entries
}

type ClientUpdateAction string

const (
	ClientUpdateAddReplace ClientUpdateAction = "add/replace"
	ClientUpdateRemove     ClientUpdateAction = "remove"
)
```

4. **Backfill `Action` migrations** at every existing `Action: "add/replace"` site — search-replace to `Action: ClientUpdateAddReplace`. Verify via `go vet` that no callers depend on the string-literal form.

5. **Write `BuildHubReconcilePlan`** in `install_hub_reconcile.go`:

```go
type HubReconcileOpts struct {
	GateOn bool // true = ON-transition (add hub + remove per-daemon); false = OFF
}

// BuildHubReconcilePlan emits the full-reconcile plan AS A WHOLE per
// spec §"Bidirectional install reconciler". Single-server install
// paths (mcphub install --server X) MUST NOT call this — they emit
// only their own per-binding entries via BuildPlan + the per-server
// planner skips the mcphub-hub Remove (codex r3 general F2 closure).
func BuildHubReconcilePlan(
	manifests []config.ServerManifest,
	endpoint HubEndpoint,
	tokens HubTokenTable,
	opts HubReconcileOpts,
) ([]ClientUpdatePlan, error) {
	// 1. Compute per-client union of (server, daemon) bindings across ALL
	//    manifests. Use clients.SupportedClientNames() as the universe.
	perClient := map[string][]canonicalDaemonRef{}
	for _, m := range manifests {
		for _, b := range m.ClientBindings {
			d, _ := findDaemon(&m, b.Daemon)
			perClient[b.Client] = append(perClient[b.Client], canonicalDaemonRef{
				Server: m.Name, Daemon: b.Daemon, Port: d.Port,
			})
		}
	}
	var plan []ClientUpdatePlan
	for client := range perClient {
		path, err := clients.ConfigPathForName(client)
		if err != nil {
			continue
		}
		if opts.GateOn && len(perClient[client]) > 0 {
			plan = append(plan, ClientUpdatePlan{
				Client:    client,
				Path:      path,
				Action:    ClientUpdateAddReplace,
				EntryName: "mcphub-hub",
				URL:       fmt.Sprintf("http://127.0.0.1:%d/clients/%s/mcp", endpoint.Port, client),
				Headers: map[string]string{
					"X-Mcphub-Hub-Token":   tokens.Tokens[client],
					"X-Mcphub-Instance-Id": endpoint.InstanceID,
				},
			})
			// For each previously-existing per-(server, client) entry: emit Remove.
			for _, ref := range perClient[client] {
				plan = append(plan, ClientUpdatePlan{
					Client:     client,
					Path:       path,
					Action:     ClientUpdateRemove,
					EntryName:  ref.Server,
					DaemonName: ref.Daemon,
				})
			}
		} else if !opts.GateOn {
			// AddReplace each per-(server, client) FIRST.
			for _, ref := range perClient[client] {
				plan = append(plan, ClientUpdatePlan{
					Client:     client,
					Path:       path,
					Action:     ClientUpdateAddReplace,
					EntryName:  ref.Server,
					URL:        fmt.Sprintf("http://localhost:%d/mcp", ref.Port),
					DaemonName: ref.Daemon,
				})
			}
			// THEN Remove the mcphub-hub aggregate.
			plan = append(plan, ClientUpdatePlan{
				Client:    client,
				Path:      path,
				Action:    ClientUpdateRemove,
				EntryName: "mcphub-hub",
			})
		}
	}
	return plan, nil
}

// ApplyHubReconcileInOrder applies the plan per spec §"Crash-safe
// reconcile ordering": within one client, all AddReplace before any
// Remove. Each client config rewrite uses SecureWriteClientConfig.
// Per-client failures surface as partial results; the reconcile
// continues with the next client and the operator reruns to converge.
func ApplyHubReconcileInOrder(plan []ClientUpdatePlan) HubReconcileReport {
	byClient := groupByClient(plan)
	report := HubReconcileReport{}
	for client, ops := range byClient {
		adds, removes := partitionByAction(ops)
		// AddReplace first.
		err := applyOpsForClient(client, adds)
		if err != nil {
			report.Failed = append(report.Failed, HubReconcileFailure{Client: client, Phase: "add/replace", Err: err.Error()})
			continue
		}
		// THEN Remove.
		err = applyOpsForClient(client, removes)
		if err != nil {
			report.Failed = append(report.Failed, HubReconcileFailure{Client: client, Phase: "remove", Err: err.Error()})
			continue
		}
		report.Succeeded = append(report.Succeeded, client)
	}
	return report
}
```

6. **Route ALL adapter writes through `SecureWriteClientConfig`** — for each adapter in `internal/clients/*.go`, replace `os.WriteFile` and `os.OpenFile`+`Write` with a call to `api.SecureWriteClientConfig`. The adapter still owns the encode-to-bytes step; only the write-to-disk step changes. Add a test for at least one adapter (e.g., `claude-code`) asserting the post-write DACL is allowlist-conformant.

7. **Single-server install path** (line ~1036): leave unchanged EXCEPT inject the new `EntryName = m.Name` field and `Action = ClientUpdateAddReplace`. The single-server path NEVER emits `Remove EntryName="mcphub-hub"` per spec §"Bidirectional install reconciler" (codex r3 general F2 closure).

8. **Run + verify PASS.**

9. **Commit:**

```bash
git add internal/api/install.go internal/api/install_hub_reconcile.go internal/api/install_hub_reconcile_test.go internal/clients/
git commit -m "feat(g4-phase5): bidirectional install reconciler (gate ON/OFF)

Extends ClientUpdatePlan with EntryName + Headers + Action enum.
Adds BuildHubReconcilePlan (full-reconcile pass owning the gate
transition) + ApplyHubReconcileInOrder (crash-safe add-before-remove
ordering within each client config rewrite). Single-server install
paths leave 'mcphub-hub' entries untouched per codex r3 general F2.

Every adapter write now routes through SecureWriteClientConfig
(handle-relative + DACL-bound) — token-bearing config writes can no
longer race with a path-swap.

Spec §'Bidirectional install reconciler' + §'Crash-safe reconcile
ordering'."
```

## Task 5.2 — `mcphub hub-mcp` CLI

**Files:**
- Create: `cmd/mcphub/hubmcp.go` (note: actual cobra wiring lives under `internal/cli/` for consistency with existing commands; this task's "cmd/mcphub/hubmcp.go" path follows the spec's §"Files to create / modify" — but cross-check with the existing `internal/cli/watchdog.go` pattern and place it under `internal/cli/hubmcp.go` instead, with the cobra `RootCmd.AddCommand` call wired from `internal/cli/root.go`).
- Create: `internal/cli/hubmcp_test.go`.

**Steps:**

1. **Write failing tests:**

```go
func TestHubMcpStatusRedactsTokens(t *testing.T) {
	// 1. Setup state dir with tokens + endpoint via EnsureHubEndpoint + EnsureHubTokens.
	// 2. Run cobra "hub-mcp status" capturing stdout.
	// 3. Assert NO 64-hex bytes from the tokens or instance_id appear in stdout.
	// 4. Assert "instance_id PRESENT" appears (presence, not value).
}

func TestHubMcpRegenerateTokenRotatesAndConfirmsViaInternalReload(t *testing.T) {
	// Spin up the hub via the same harness as Phase 4 integration.
	// Run "hub-mcp regenerate-token --client claude-code --yes".
	// Assert: token file rotated AND POST /internal/reload-tokens fired
	// AND the next request bearing the OLD token gets 401 within 500ms.
}

func TestHubMcpRegenerateTokenNonTTYRequiresYes(t *testing.T) {
	// Non-TTY input + no --yes -> exit 6.
}

func TestHubMcpRegenerateInstanceIDRotatesAndPrintsReinstallNotice(t *testing.T) {
	// Run "hub-mcp regenerate-instance-id --yes".
	// Assert: endpoint.json instance_id changed AND stdout contains a
	// "rerun `mcphub install` for every client" instruction.
}
```

2. **Run + verify FAIL.**

3. **Write impl** following the `internal/cli/watchdog.go` pattern (cobra subcommand with separate `RunE` per leaf):

```go
// internal/cli/hubmcp.go
package cli

func newHubMcpCmd() *cobra.Command {
	c := &cobra.Command{Use: "hub-mcp", Short: "Hub MCP endpoint operations"}
	c.AddCommand(newHubMcpStatusCmd(), newHubMcpRegenTokenCmd(), newHubMcpRegenInstanceIDCmd())
	return c
}

func newHubMcpStatusCmd() *cobra.Command {
	var jsonOut bool
	return &cobra.Command{
		Use:   "status",
		Short: "Show hub-mcp endpoint state",
		RunE: func(cmd *cobra.Command, args []string) error {
			ep, _ := api.LoadHubEndpoint()
			tbl := api.CurrentTokenTable()
			out := map[string]any{
				"port":              ep.Port,
				"instance_id":       "PRESENT (redacted)", // never print value
				"pid":               ep.PID,
				"started_at":        ep.StartedAt,
				"per_client_tokens": presenceOnly(tbl.Tokens), // {"claude-code":"PRESENT",...}
				"recent_events":     api.RecentHubMcpEvents(8),
			}
			// Marshal AND pipe through RedactToken before printing.
			raw, _ := json.MarshalIndent(out, "", "  ")
			fmt.Fprintln(cmd.OutOrStdout(), api.RedactToken(string(raw)))
			return nil
		},
	}
}

func newHubMcpRegenTokenCmd() *cobra.Command {
	var client string
	var yes bool
	c := &cobra.Command{
		Use:   "regenerate-token",
		Short: "Rotate one client's hub-mcp token",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes && !inputIsTerminal(cmd.InOrStdin()) {
				return forceExitError{code: 6, msg: "non-TTY without --yes"}
			}
			// 1. Acquire flock (api.RotateHubToken acquires for us).
			tbl, err := api.RotateHubToken(client)
			if err != nil {
				return err
			}
			// 2. Read hub-mcp-control.token + POST /internal/reload-tokens
			//    so the live hub picks up the new tokens within ms.
			ep, _ := api.LoadHubEndpoint()
			controlTok, err := api.ReadHubMcpControlToken()
			if err != nil {
				// Step 8 fallback per spec §"Token-table reload on rotation".
				fmt.Fprintln(cmd.ErrOrStderr(), "rotate persisted to disk but live hub did not confirm; restart hub to apply or investigate")
				return forceExitError{code: 1, msg: "live reload failed"}
			}
			url := fmt.Sprintf("http://127.0.0.1:%d/internal/reload-tokens", ep.Port)
			req, _ := http.NewRequestWithContext(cmd.Context(), "POST", url, nil)
			req.Header.Set("X-Mcphub-Control-Token", controlTok)
			resp, err := http.DefaultClient.Do(req)
			if err != nil || resp.StatusCode != 204 {
				fmt.Fprintln(cmd.ErrOrStderr(), "rotate persisted to disk but live hub did not confirm; restart hub to apply or investigate")
				return forceExitError{code: 1, msg: "live reload failed"}
			}
			defer resp.Body.Close()
			// 3. Re-install instructions.
			fmt.Fprintf(cmd.OutOrStdout(),
				"Rotated token for client %s. Run `mcphub install --server <each> --client %s` to refresh the live config.\n",
				client, client)
			_ = tbl
			return nil
		},
	}
	c.Flags().StringVar(&client, "client", "", "client adapter id (required)")
	c.Flags().BoolVar(&yes, "yes", false, "confirm in non-TTY contexts")
	c.MarkFlagRequired("client")
	return c
}

// newHubMcpRegenInstanceIDCmd: similar shape, calls api.RotateHubInstanceID,
// prints "every client config is now stale; rerun mcphub install for each".
```

4. **Wire into `internal/cli/root.go`'s `rootCmd.AddCommand(...)` block.**

5. **Run + verify PASS.**

6. **Commit:**

```bash
git add internal/cli/hubmcp.go internal/cli/hubmcp_test.go internal/cli/root.go
git commit -m "feat(g4-phase5): mcphub hub-mcp CLI (status + regenerate-token + regenerate-instance-id)

status: redacts every token + instance_id; prints presence-only +
recent events.
regenerate-token --client X: rotates one client + POSTs
/internal/reload-tokens so the live hub picks it up within ms.
Fallback message tells the operator to restart if the live hub did
not confirm.
regenerate-instance-id: rotates the persistent instance_id; prints
'rerun mcphub install for every client' notice.

Non-TTY without --yes → exit 6 per existing watchdog CLI convention.

Spec §'Settings + CLI surface'."
```

## Task 5.3 — `mcphub gui --reset-port`

**Files:**
- Modify: `internal/cli/gui.go` add `--reset-port` flag + handler before any other gui setup runs.

**Steps:**

1. **Write failing test** in `internal/cli/gui_force_test.go` (extend existing file):

```go
func TestGuiResetPortClearsPortKeepsInstanceID(t *testing.T) {
	// 1. Setup state dir + endpoint via EnsureHubEndpoint(9120, ...).
	// 2. Run cobra "gui --reset-port --yes" — must exit 0 without binding
	//    a listener. Assert endpoint.json Port=0, instance_id unchanged.
	// 3. Stdout must contain "credentials may have leaked; rotate before
	//    reinstalling" warning per spec §--reset-port.
}

func TestGuiResetPortNonTTYRequiresYes(t *testing.T) { /* exit 6 */ }
```

2. **Run + verify FAIL.**

3. **Add flag + handler** in `internal/cli/gui.go`. Reset-port runs BEFORE the single-instance lock acquisition (it's a state-dir operation, not a runtime operation):

```go
var resetPort bool
// ... add to cobra Flags() block:
c.Flags().BoolVar(&resetPort, "reset-port", false, "discard the persistent hub-mcp port + emit credential-rotation warning")

// In RunE, before single-instance lock acquisition:
if resetPort {
	if !yes && !inputIsTerminal(cmd.InOrStdin()) {
		return forceExitError{code: 6, msg: "non-TTY without --yes"}
	}
	if err := api.ResetHubPort(); err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(),
		"WARNING: credentials may have leaked to the pre-binding process before --reset-port. "+
			"Run `mcphub hub-mcp regenerate-token --client <each>` AND "+
			"`mcphub hub-mcp regenerate-instance-id` before reinstalling.")
	fmt.Fprintln(cmd.OutOrStdout(), "Then: `mcphub install` for each affected client.")
	return nil // exit 0; do NOT continue to start the gui.
}
```

4. **Run + verify PASS.**

5. **Commit.**

## Task 5.4 — Settings registry entry + Settings.tsx toggle

**Files:**
- Modify: `internal/api/settings_registry.go` (add new entry after `gui_server.tray`).
- Modify: `internal/gui/frontend/src/components/settings/SectionGuiServer.tsx` (extend `SECTION_KEYS` + `EDITABLE_KEYS`).
- Create: `internal/gui/frontend/src/components/settings/SectionGuiServer.hubendpoint.test.tsx`.

**Steps:**

1. **Add registry entry** per spec §"Settings registry":

```go
{Key: "gui_server.hub_endpoint_enabled", Section: "gui_server", Type: TypeBool,
    Default: "false", Deferred: true,
    Help: "Expose a single aggregated hub URL per client instead of per-daemon URLs. Restart required. Hub instance ID is generated once on first start and persists across restarts; clients re-install only on explicit operator-rotation events (`mcphub hub-mcp regenerate-instance-id` or `regenerate-token`)."},
```

2. **Extend SectionGuiServer.tsx** to include the new key in `SECTION_KEYS` + `EDITABLE_KEYS`. Add a pending-restart badge that fires when `effective("gui_server.hub_endpoint_enabled") !== persisted("gui_server.hub_endpoint_enabled")`:

```tsx
const SECTION_KEYS = ["gui_server.browser_on_launch", "gui_server.port", "gui_server.hub_endpoint_enabled", "gui_server.tray"];
const EDITABLE_KEYS = ["gui_server.browser_on_launch", "gui_server.port", "gui_server.hub_endpoint_enabled"];

// ... in the render block, when k === "gui_server.hub_endpoint_enabled":
const hubDef = snapshot.data.settings.find((s) => s.key === "gui_server.hub_endpoint_enabled") as ConfigSettingDTO;
const hubEffective = flow.effective("gui_server.hub_endpoint_enabled");
const hubPersisted = hubDef.value;
const showHubRestartBadge = hubEffective !== hubPersisted;
// ... inside the map:
{k === "gui_server.hub_endpoint_enabled" && showHubRestartBadge ? (
  <span class="settings-restart-badge" data-test-id="hub-endpoint-restart-badge" role="status">
    ⚠ Restart required — hub endpoint will be {hubEffective === "true" ? "enabled" : "disabled"} after restart
  </span>
) : null}
```

3. **Write Settings test** in `SectionGuiServer.hubendpoint.test.tsx` using the existing settings-test pattern (preact-testing-library + happy-dom or jsdom).

4. **Frontend build + typecheck:**

```bash
cd internal/gui/frontend
npm run typecheck
npm run test
npm run build
cd ../../..
go generate ./internal/gui/...
```

5. **Commit:**

```bash
git add internal/api/settings_registry.go internal/gui/frontend/src/components/settings/SectionGuiServer.tsx internal/gui/frontend/src/components/settings/SectionGuiServer.hubendpoint.test.tsx internal/gui/assets/
git commit -m "feat(g4-phase5): Settings toggle for gui_server.hub_endpoint_enabled

Adds the bool entry to the registry (Deferred=true → restart required
badge). SectionGuiServer renders the toggle with a pending-restart
badge anchored to the persisted value (not the local draft) — same
pattern as the existing gui_server.port badge per codex r3 P2.1 in
the Settings spec.

Spec §'Settings + CLI surface' (registry block)."
```

## Task 5.5 — Playwright e2e for the hub-mcp flow

**Files:**
- Create: `internal/gui/e2e/tests/hub-mcp.spec.ts`.

**Steps:**

1. **Write failing tests** matching the suite in spec §"Test surface — Playwright":

```typescript
import { test, expect } from "../fixtures/hub";

test.describe("Hub MCP endpoint", () => {
  test("Gate OFF: no hub listener bound", async ({ page, hub }) => {
    // Settings shows toggle OFF (default).
    await page.goto(`${hub.url}/#/settings`);
    const toggle = page.getByLabel(/Expose a single aggregated hub URL/);
    await expect(toggle).not.toBeChecked();
  });

  test("Gate ON after restart: hub listener bound; 401 without token", async ({ page, hub }) => {
    // 1. Toggle ON via Settings.
    // 2. Restart hub via fixture restart helper.
    // 3. fetch http://127.0.0.1:<hub-port>/clients/claude-code/mcp -> 401.
    // 4. Set valid token + instance_id headers -> 200 (initialize fan-out).
  });

  test("Restart preserves instance_id", async ({ page, hub }) => {
    // After restart, the same token + URL still authenticate.
  });

  test("regenerate-token invalidates old token; fresh install works", async ({ page, hub }) => { /* ... */ });

  test("regenerate-instance-id invalidates token-only AND URL-only stale configs", async ({ page, hub }) => { /* ... */ });
});
```

2. **Extend the existing `internal/gui/e2e/fixtures/hub.ts`** with a helper that:
   - Writes `gui-preferences.yaml` with `gui_server.hub_endpoint_enabled: true`.
   - Restarts the hub fixture (binary process).
   - Returns the persisted port + token + instance_id for the test to use.

3. **Run + verify PASS:**

```bash
cd internal/gui/e2e
npm test -- hub-mcp.spec.ts
```

4. **Commit.**

## Task 5.6 — Phase 5 verification

1. Run the full gate from cross-phase invariants (build + vet + test + state_path_env + e2e).
2. Process sweep.
3. Manual smoke per spec §"Manual smoke" (add the steps to `docs/phase-3b-ii-verification.md` D2.7):
   - With claude-code: install gate-OFF → toggle ON → restart → `mcphub install --reconcile-hub-mode` → verify `~/.claude.json` has `mcphub-hub` entry pointing to `http://127.0.0.1:<port>/clients/claude-code/mcp` with headers. Start claude-code; verify it can list tools from 2+ daemons via the aggregator.
   - Repeat with codex-cli.
   - `mcphub hub-mcp regenerate-token --client claude-code` → next claude-code request fails until reinstalled.
4. Diff target ≤ ~1500 lines.

**Rollback / safe fallback:** revert PR → gate goes back to OFF default; per-daemon URLs unaffected throughout.

**Risks:**
- Adapter rewrites to use `SecureWriteClientConfig` are the highest-blast-radius change in Phase 5. If a single adapter fails to write the token file because its parent dir has a Group-Policy ACL, the operator sees the fail-closed message. Test on a domain-joined VM if possible.
- The Playwright fixture needs to talk to the real hub binary, which means `internal/gui/e2e/global-setup.ts` must rebuild the binary AFTER Phase 5 changes land. Run `npm test` against a freshly-built binary in CI.

---

## Cross-phase rollback summary

| Phase | Rollback path |
|---|---|
| 1 | Revert PR — all additions; no production code consumes them yet. |
| 2 | Revert PR — pure data layer. |
| 3 | Revert PR — pure logic, no listener. |
| 4 | Revert PR — hub listener only binds when `gui_server.hub_endpoint_enabled=true`; default-OFF means revert is a no-op for the gate-OFF user. Operators with gate-ON must toggle OFF before revert. |
| 5 | Revert PR — Settings hides the toggle; CLI subcommands disappear. Existing `mcphub-hub` entries in client configs become orphaned (clients can't reach a non-existent endpoint, but per-daemon URLs that the install reconciler removed need to be re-added). RECOVERY: ship a follow-up PR with `mcphub install --reconcile-hub-mode` running as the last step (it's idempotent and converges). For the v0.3.0 release horizon, treat Phase 5 revert as "operator must re-run `mcphub install` for every client". |

## Cross-phase risks called out for review

1. **Phase 1 Windows NT-syscall correctness.** `secure_write_windows.go` is the hardest single file in the project. Mandatory: codex pass on a deep-sec prompt focused on every NT syscall wrapper. Verify against Microsoft's `FILE_RENAME_INFORMATION_EX` struct layout (the `Flags` bitfield form, NOT the legacy `ReplaceIfExists` bool) — spec §"SecureWriteClientConfig sequence" step 10 codex r6 MED closure.
2. **Phase 2 token-table reload race.** `liveTokenTable atomic.Pointer[HubTokenTable]` swap must happen AFTER the file write, never before. The control endpoint's 5s rate limit prevents thrash but does not prevent the read-after-write race; the integration test in Phase 4 covers it.
3. **Phase 3 idle sweeper goroutine.** Cancellation propagation via context is the only escape hatch; verify `Server.Shutdown` waits for sweep stop in Phase 4.
4. **Phase 4 bind-failure path.** The credential-exfil warning is the ONLY operator signal that a pre-bind window may have happened. Phase 4 test `TestHubListenerBindFailureLogsCredentialWarning` must pass on both platforms.
5. **Phase 5 per-client write atomicity.** `SecureWriteClientConfig` provides per-file atomicity but NOT cross-client atomicity — a crash between client A's write and client B's write leaves the system in a half-reconciled state. The crash-safe add-before-remove ordering means the worst-case observable state is "two competing entries pointing to the same server" (per spec §"Why add-before-remove"); the next reconcile converges idempotently.

## Recommended next-role sequence

1. **Phase 1 implementer** ($backend-engineer, eligible for $external-worker if delegation mode favors external).
2. **Phase 1 reviewer** ($security-reviewer mandatory — DACL + TOCTOU surface).
3. **Phase 2 implementer** ($backend-engineer).
4. **Phase 2 reviewer** ($security-reviewer — token + redaction).
5. **Phase 3 implementer** ($backend-engineer).
6. **Phase 3 reviewer** ($qa-engineer + $architecture-reviewer — concurrency model self-consistency).
7. **Phase 4 implementer** ($backend-engineer).
8. **Phase 4 reviewer** ($security-reviewer + $performance-reviewer — auth gate + listener factory + control endpoint).
9. **Phase 5 implementer** ($backend-engineer for Go side; $frontend-engineer for Settings.tsx changes; $ui-test-engineer for the Playwright spec).
10. **Phase 5 reviewer** ($security-reviewer + $ux-reviewer + $qa-engineer).

Each phase ends with the canonical PR workflow (CLAUDE.md steps 1-7).

## Gate decision

**PASS** — every spec section maps to at least one task; phase boundaries respect dependency order (no task in phase N depends on a definition that only appears in phase N+M); each phase compiles + tests-green + commits independently; the bidirectional install reconciler closure (Phase 5) explicitly depends on tokens (Phase 2), endpoint state (Phase 2), and `SecureWriteClientConfig` (Phase 1) — all are defined before they're referenced.

## Terms and Abbreviations

- `DACL`: Discretionary Access Control List (Windows per-file ACL).
- `JSON-RPC`: text-based RPC protocol underlying MCP.
- `MCP`: Model Context Protocol.
- `Mcp-Session-Id`: HTTP header MCP uses for session multiplexing.
- `MCP-Protocol-Version`: HTTP header carrying the protocol version negotiated at initialize.
- `SO_EXCLUSIVEADDRUSE`: Windows-only socket option that prevents another process from binding the same address.
- `TOCTOU`: time-of-check / time-of-use race condition.
- `flock`: file-based lock primitive used to serialize state mutations.
- `KOSYAK`: Russian-derived shorthand the project uses for "documented failure mode to avoid" (per CLAUDE.md memory references).
- `gate`: the `gui_server.hub_endpoint_enabled` boolean setting.
- `participating set`: per-client union of `(server, daemon)` bindings across all manifests with at least one hub-routed binding.
- `aggregate entry`: the `mcphub-hub` entry in a client config; replaces per-daemon entries when the gate is ON.
- `per-daemon entry`: the legacy per-(server, daemon) entry in a client config; restored when the gate is OFF.
