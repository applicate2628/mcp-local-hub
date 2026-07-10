# Test-Leftover Reaper V1 Preview Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the read-only `mcphub cleanup test-leftovers` preview command defined by the accepted work-item design.

**Architecture:** `internal/cli` parses the two diagnostic scope flags plus `--json`, then calls one typed `internal/api` evidence method. The API reuses `runProcessSnapshot`, `parseProcessRows`, the existing parent-state seam, `rootContains`, and the existing command-line redaction owner; `internal/process` supplies the design-approved strict-error path canonicalizer. The returned data has no action method or dependency on a termination owner.

**Tech Stack:** Go, Cobra, `debug/buildinfo`, existing WMIC/PowerShell census parser.

## Global Constraints

- V1 is preview/diagnostics only and cannot terminate, signal, or mutate a process.
- Do not add apply, confirm-token, tree-reap, Process Environment Block, environment-read, or hidden-toggle behavior.
- Preserve existing `CleanupOrphans`, `AggressiveCleanup`, and their own-binary safeguards unchanged.
- Tests use synthetic census text and injected read-only dependencies; they do not enumerate or act on host processes.
- Run only the build, vet, and focused test gates requested in the user directive.

---

### Task 1: Strict path and evidence API contract

**Files:**

- Create: `internal/process/path_canonical.go`
- Create: `internal/api/test_leftover_preview.go`
- Create: `internal/api/test_leftover_preview_test.go`

**Interfaces:**

- Produces: `process.CanonicalizePathStrict(path string) (string, error)`.
- Produces: `api.TestLeftoverPreviewOpts`, `api.TestLeftoverPreview`, and `api.TestLeftoverCandidate` JSON-safe contracts.
- Produces: `func (a *API) PreviewTestLeftovers(opts TestLeftoverPreviewOpts) (TestLeftoverPreview, error)`.

- [x] **Step 1: Write failing contract and family tests**

```go
func TestLeftoverPreviewClassifiesDiagnosticFamilies(t *testing.T)
func TestLeftoverPreviewStandaloneSuperviseIsManualOnly(t *testing.T)
func TestLeftoverPreviewOmitsUnrelatedRows(t *testing.T)
```

The fixtures contain complete WMIC-shaped rows for reliability-temp, GUI end-to-end, Go build cache, explicit operator root, standalone `supervise`, and one unrelated image. They assert one record per recognized row, stable family/argv/age/parent/buildinfo/environment/apply fields, the exact manual-only note, and no unrelated record.

- [x] **Step 2: Run the API focus gate and observe RED**

Run: `go test ./internal/api/ -run 'TestLeftover|Preview' -count=1`

Expected: compile failure because the preview API and types do not exist.

- [x] **Step 3: Implement the minimal strict path and API contracts**

```go
func CanonicalizePathStrict(path string) (string, error)

type TestLeftoverPreviewOpts struct {
	MinAgeSec int64
	TempRoot  string
}

func (a *API) PreviewTestLeftovers(opts TestLeftoverPreviewOpts) (TestLeftoverPreview, error)
```

The method obtains one snapshot through its API-owned read-only dependency, passes it to `parseProcessRows`, classifies only recognized display signatures, and returns structured evidence. Raw executable paths and command lines remain server-side-only JSON fields; JSON exposes basename/shape redactions.

- [x] **Step 4: Run the API focus gate and observe GREEN**

Run: `go test ./internal/api/ -run 'TestLeftover|Preview' -count=1`

Expected: the initial family, manual-only, and unrelated-row tests pass.

### Task 2: Failure evidence and no-action invariant

**Files:**

- Modify: `internal/api/test_leftover_preview.go`
- Modify: `internal/api/test_leftover_preview_test.go`

**Interfaces:**

- Consumes: the Task 1 API and strict canonicalizer.
- Produces: stable snapshot, path, buildinfo, parent, identity, age, and hypothetical-refusal diagnostics.

- [x] **Step 1: Write failing evidence tests**

```go
func TestLeftoverPreviewLabelsDegradedSnapshot(t *testing.T)
func TestLeftoverPreviewSnapshotAcquisitionFailureIsVisible(t *testing.T)
func TestLeftoverPreviewKeepsCandidateWhenEvidenceIsMissing(t *testing.T)
func TestLeftoverPreviewBuildInfoFindings(t *testing.T)
func TestLeftoverPreviewPathClassificationIsStrictAndDisplayOnly(t *testing.T)
func TestLeftoverPreviewJSONRedactsRawPathAndArgv(t *testing.T)
func TestLeftoverPreviewNeverCallsTerminateSeam(t *testing.T)
```

The termination test installs a fail-on-call `orphanTerminateFn`, invokes the public preview API with a synthetic candidate, and asserts the call count remains zero. The degraded fixture includes one valid record followed by a scanner-overflow record so `parseProcessRows` remains the completeness owner.

- [x] **Step 2: Run the API focus gate and observe RED**

Run: `go test ./internal/api/ -run 'TestLeftover|Preview' -count=1`

Expected: assertion failures for the unimplemented diagnostic fields and refusal precedence.

- [x] **Step 3: Implement independent evidence collection**

```go
const TestLeftoverApplyDeferred = "apply-deferred-v1"
const TestLeftoverEnvironmentNotCollected = "not-collected-v1"

func testLeftoverBuildInfoFinding(path string, readFn buildInfoReadFunc) string
func testLeftoverParentVerdict(row procRow, byPID map[int]procRow, query parentStateFunc) string
func testLeftoverWouldRefuse(candidate TestLeftoverCandidate, opts TestLeftoverPreviewOpts) string
```

Every evidence error keeps the candidate visible. The code scans `debug.BuildInfo.Settings` for the exact `test_state_path_env` tag, never reads process memory or environment, and never imports or references a process action owner.

- [x] **Step 4: Run the API focus gate and observe GREEN**

Run: `go test ./internal/api/ -run 'TestLeftover|Preview' -count=1`

Expected: all V1 API tests pass.

### Task 3: Preview-only CLI and renderers

**Files:**

- Create: `internal/cli/cleanup_test_leftovers.go`
- Create: `internal/cli/cleanup_test_leftovers_test.go`
- Modify: `internal/cli/cleanup.go`

**Interfaces:**

- Consumes: `api.PreviewTestLeftovers` and its structured result.
- Produces: `mcphub cleanup test-leftovers [--min-age-sec N] [--temp-root PATH] [--json]`.

- [x] **Step 1: Write failing CLI tests**

```go
func TestCleanupTestLeftoversRejectsDestructiveFlags(t *testing.T)
func TestCleanupTestLeftoversPassesDiagnosticOptions(t *testing.T)
func TestCleanupTestLeftoversPreviewHumanOutput(t *testing.T)
func TestCleanupTestLeftoversPreviewJSONOutput(t *testing.T)
func TestCleanupTestLeftoversIsWiredUnderCleanup(t *testing.T)
```

The destructive-flag table covers `--apply`, `--confirm-token`, and `--confirm` both after the child command and before it as a parent-local flag. Output tests inject a deterministic API runner and assert the human lifecycle warning plus clean machine-readable JSON.

- [x] **Step 2: Run the CLI focus gate and observe RED**

Run: `go test ./internal/cli/ -run 'TestLeftover|CleanupTestLeftover|Preview' -count=1`

Expected: compile failure because the child command constructor does not exist.

- [x] **Step 3: Implement the Cobra adapter and renderers**

```go
func newCleanupTestLeftoversCmdReal() *cobra.Command
func newCleanupTestLeftoversCmd(run testLeftoverPreviewRunner) *cobra.Command
func printTestLeftoverPreviewHuman(cmd *cobra.Command, result api.TestLeftoverPreview)
```

The command has no destructive flags. Its pre-run validation rejects any visited parent cleanup flag so `cleanup --confirm test-leftovers` cannot smuggle a destructive parent option into the preview route. JSON uses `json.Encoder`; human output prints full local paths with stable evidence labels.

- [x] **Step 4: Run the CLI focus gate and observe GREEN**

Run: `go test ./internal/cli/ -run 'TestLeftover|CleanupTestLeftover|Preview' -count=1`

Expected: all focused CLI tests pass.

### Task 4: Verification and handoff

**Files:**

- Create: `.reports/2026-07/report(backend-engineer)-2026-07-10_01-28_test-leftover-reaper-v1-preview.md`

**Interfaces:**

- Consumes: the complete working-tree diff.
- Produces: one backend implementation package and verification record; no commit or push.

- [x] **Step 1: Format and run focused tests**

Run: `gofmt -w internal/process/path_canonical.go internal/api/test_leftover_preview.go internal/api/test_leftover_preview_test.go internal/cli/cleanup_test_leftovers.go internal/cli/cleanup_test_leftovers_test.go` followed by the two focused test commands from Tasks 2 and 3.

- [x] **Step 2: Run build and vet gates**

Run: `go build ./...`

Run: `go vet ./internal/api/ ./internal/cli/`

- [x] **Step 3: Inspect the V1 production call surface**

Run a diff-scoped search for `TerminatePIDWithIdentity`, `reapOneOrphan`, `orphanTerminateFn`, `Stop-Process`, `taskkill`, `.Kill(`, and signal calls. The required result is no production call edge from the new API/CLI files; the test-only fail-on-call seam reference is reported separately.

- [x] **Step 4: Reconcile scope and report**

Confirm every V1-T1 through V1-T10 assertion is represented, existing destructive cleanup files are unchanged except the additive child-command registration, no Process Environment Block/environment/apply implementation exists, and no commit/push occurred.
