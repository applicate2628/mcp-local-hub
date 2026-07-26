// Package cli — `mcphub workspace {register, unregister, list, set-default,
// bootstrap}` subcommands for the dynamic-pool serena flow (Phases B.2 +
// B.3 of docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md).
//
// Distinct from the existing `mcphub workspaces` (plural) command, which
// lists every (workspace, language) tuple in the registry across all
// backends. The singular `workspace` command group is the serena-specific
// operator surface for one-workspace-one-daemon dynamic-pool entries
// (Language == api.SerenaLanguageSentinel).
//
// The set-default flag is persisted in a sidecar file
// (`<state-dir>/default-workspace.txt`) carrying one canonical workspace
// path. This is intentionally a separate file rather than a field on
// WorkspaceEntry so the change stays inside the Phase B.2 file boundary
// (`internal/cli/workspace_cmd.go` only) and avoids registry schema
// churn — Phase F is the consumer and can promote it to a registry
// field if needed when the routing seam lands.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
)

// defaultWorkspaceFilename is the sidecar file alongside workspaces.yaml
// that records the operator-selected default serena workspace by its
// canonical path. Absent file = no default. Empty file = no default.
// Phase F (no-path-args routing) consumes this.
//
// The marker read/write/clear logic is owned by internal/api
// (api.DefaultWorkspaceFilename + the api.*DefaultWorkspace* helpers) so the
// GUI auto-prune sweeper — which cannot import internal/cli — shares ONE owner.
// This constant aliases the api owner's name so the existing CLI call sites and
// tests keep compiling against the cli-package identifier.
const defaultWorkspaceFilename = api.DefaultWorkspaceFilename

// loadSerenaManifestForCLI is the test-injectable manifest loader. The
// production form goes through the embed-first manifest pipeline
// (`api.NewAPI().ManifestGet`); the override seam below lets tests
// hand-shape a manifest (e.g. the legacy `kind: global` embed shape, or a
// daemon_template.port_pool block). The loaded manifest is the CATALOG input
// to the shared dynamic-pool service (api.EffectiveSerenaPortPool), which
// supplies a built-in default port pool when the embed has no daemon_template
// — so `register` works against the current `kind: global` embed without
// requiring a manifest migration first (Phase 2 cycle-break, finding #3).
var loadSerenaManifestForCLI = loadSerenaManifestFromDisk

// unregisterLSPWorkspaceFn is the test-injectable seam over the paired LSP
// teardown. The production form is (*api.API).Unregister, which removes each
// LSP (workspace_key, language) row TOGETHER WITH its supervisor-intent
// descriptor (removeLSPSupervisorIntent + reconcile + kill-by-port + scheduler
// delete + client-entry removal), so a removed registry row never leaves an
// orphaned intent descriptor behind for the reconciler to spawn-and-quarantine.
// `mcphub workspace unregister` previously removed LSP rows via the bare
// reg.RemoveByBackend, which dropped the registry row WITHOUT the paired intent
// teardown — the source of the orphaned-LSP-daemon quarantine bug. CLI tests
// stub this so they stay hermetic (no real scheduler / netstat / IPC dial).
// The workspace argument is the operator-supplied path, not the resolved
// canonical path, so api.Unregister can compute its own legacy-key fallback for
// pre-symlink-canonicalization registry rows.
var unregisterLSPWorkspaceFn = func(workspacePath string, languages []string) (*api.UnregisterReport, error) {
	return api.NewAPI().Unregister(workspacePath, languages)
}

// removeSerenaSupervisorIntentFn is the test-injectable seam over the serena
// supervisor-descriptor teardown. The production form is
// (*api.API).RemoveSerenaSupervisorIntentForWorkspace, which on a LIVE
// supervisor nudges a reconcile and — on a reconcile failure — RESTORES the
// descriptor and returns a retry-asking error. The unregister flow runs this
// teardown BEFORE committing the registry-row delete (bot r32 P2): the registry
// row is the durable record that drives the paired teardown, so it must outlive
// a failed teardown — otherwise a restored descriptor + a gone registry row
// leaves an orphan that no retry can clean up. CLI tests stub this so they stay
// hermetic and can drive the teardown-failure branch deterministically.
var removeSerenaSupervisorIntentFn = func(workspacePath string) (bool, error) {
	return api.NewAPI().RemoveSerenaSupervisorIntentForWorkspace(workspacePath)
}

// loadSerenaManifestFromDisk is the production manifest loader. It uses
// the same MCPHUB_MANIFEST_DIR_OVERRIDE seam the api package honors, so
// tests that set the override env get hermetic manifests.
func loadSerenaManifestFromDisk() (*config.ServerManifest, error) {
	a := api.NewAPI()
	data, err := a.ManifestGet("serena")
	if err != nil {
		return nil, fmt.Errorf("load serena manifest: %w", err)
	}
	m, err := config.ParseManifest(strings.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("parse serena manifest: %w", err)
	}
	return m, nil
}

// serenaPortPool resolves the port pool to allocate serena workspace
// daemons from. It delegates to the shared dynamic-pool service
// (api.EffectiveSerenaPortPool — internal/api/serena_dynamic_pool.go), the
// single owner of serena port-pool + template policy (Phase 2, finding #3).
//
// The service prefers the embed's daemon_template.port_pool when present, else
// falls back to a built-in dynamic-pool default. This is the cycle-break: the
// resolver NO LONGER fails closed on the legacy `kind: global` embed (which has
// no daemon_template) — `register` allocates from the service's effective pool
// instead. Migrating the embed to the dynamic-pool shape later automatically
// switches the source to its own declared pool with no change here.
func serenaPortPool(m *config.ServerManifest) (config.PortPool, error) {
	return api.EffectiveSerenaPortPool(m)
}

// newWorkspaceCmd builds the `mcphub workspace` parent command. The
// subcommands wire B.1 registry primitives (`PutSerena` / `SerenaEntries`
// / `RemoveByBackend` / `AllocateSerenaPort`) into the operator surface.
func newWorkspaceCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "workspace",
		Short: "Manage serena dynamic-pool workspaces (one daemon per workspace)",
		Long: `Group of subcommands for the serena dynamic-pool architecture (Phase B
of the v0.5.x serena-supervisor unified plan). Each registered workspace
gets its own long-lived serena daemon bootstrapped on --project <abs-path>
with languages snapshotted from .serena/project.yml at register time.

Distinct from the existing ` + "`mcphub workspaces`" + ` (plural) command, which
enumerates every (workspace, language) tuple across all backends. The
singular ` + "`mcphub workspace`" + ` group manages only the serena rows.

Subcommands:
  bootstrap     Initialize .serena/project.yml from a directory survey
  register      Register a workspace + allocate a serena port
  unregister    Remove a workspace from the registry
  list          List registered serena workspaces
  set-default   Mark a workspace as default for no-path-args routing
  prune         Bulk-remove registry rows for orphaned (dead) workspaces
`,
	}
	c.AddCommand(newWorkspaceRegisterCmd())
	c.AddCommand(newWorkspaceUnregisterCmd())
	c.AddCommand(newWorkspaceListCmd())
	c.AddCommand(newWorkspaceSetDefaultCmd())
	c.AddCommand(newWorkspaceBootstrapCmd())
	c.AddCommand(newWorkspacePruneCmd())
	return c
}

// newWorkspaceRegisterCmd builds `mcphub workspace register <path>
// [--default] [--languages cpp,typescript,markdown]`.
func newWorkspaceRegisterCmd() *cobra.Command {
	var setDefault bool
	var languagesFlag string
	c := &cobra.Command{
		Use:   "register <path>",
		Short: "Register a workspace for serena dynamic-pool routing",
		Long: `Allocate a serena port from the effective dynamic-pool port range
and persist the workspace as a serena (sentinel) row in workspaces.yaml.
The port range comes from the embedded serena manifest's daemon_template
when it declares one, else from a built-in dynamic-pool default.

Behavior:
  - Reads .serena/project.yml under <path> for the languages snapshot.
  - --languages <list> overrides the .serena/project.yml read.
  - If .serena/project.yml is missing AND --languages is empty, the
    command refuses with an explicit guidance to run
    ` + "`mcphub workspace bootstrap <path>`" + ` first (B.3).
  - --default marks this workspace as the default for no-path-args routing
    (Phase F). Replaces any prior default. The marker lives in a sidecar
    file next to workspaces.yaml.
  - A second register against the same workspace_key is rejected; use
    ` + "`mcphub workspace unregister`" + ` first if you intend to re-register.

Examples:
  mcphub workspace register D:\dev\PaperPane
  mcphub workspace register D:\dev\PaperPane --default
  mcphub workspace register D:\dev\PaperPane --languages cpp,typescript,markdown
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceRegister(cmd, args[0], setDefault, languagesFlag)
		},
	}
	c.Flags().BoolVar(&setDefault, "default", false,
		"mark this workspace as the default for no-path-args routing (Phase F)")
	c.Flags().StringVar(&languagesFlag, "languages", "",
		"comma-separated language list (overrides .serena/project.yml)")
	return c
}

func runWorkspaceRegister(cmd *cobra.Command, rawPath string, setDefault bool, languagesFlag string) error {
	canonical, err := api.CanonicalWorkspacePath(rawPath)
	if err != nil {
		return err
	}
	wsKey := api.WorkspaceKey(canonical)

	// 1. Resolve languages: flag overrides, otherwise read .serena/project.yml.
	var languages []string
	if languagesFlag != "" {
		languages = splitAndTrim(languagesFlag, ",")
	} else {
		langs, err := readSerenaProjectLanguages(canonical)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf(".serena/project.yml not found in %s — "+
					"run `mcphub workspace bootstrap %s` first, "+
					"or pass --languages explicitly", canonical, canonical)
			}
			return fmt.Errorf("read .serena/project.yml: %w", err)
		}
		languages = langs
	}
	if len(languages) == 0 {
		return fmt.Errorf("no languages resolved for workspace %s "+
			"(empty .serena/project.yml or --languages= flag)", canonical)
	}
	sort.Strings(languages)

	// 2. Resolve serena's EFFECTIVE port pool via the shared dynamic-pool
	// service (Phase 2). The embed is the catalog input; the service supplies
	// a built-in default pool when the embed has no daemon_template, so this
	// no longer fails closed on the legacy `kind: global` embed.
	m, err := loadSerenaManifestForCLI()
	if err != nil {
		return err
	}
	pool, err := serenaPortPool(m)
	if err != nil {
		return err
	}

	// 3. Acquire registry flock and load.
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		return err
	}
	reg := api.NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		return err
	}
	// releaseRegLock is called explicitly right after reg.Save() below (step
	// 6), BEFORE the supervisor-materialization step (8) — never held across
	// it. RepairSerenaIntentFromRegistry (invoked server-side, inside the
	// running supervisor's handleReconcile, by the reconcile-apply call at
	// step 8) takes its OWN registry flock with only a brief bounded retry
	// (~250ms total, tryLockRegistryBrief); holding THIS command's lock
	// across that call would starve it into a silent no-op on every single
	// register — which was the concrete mechanism behind the P1 this fixes
	// (see the commit message). The idempotent nil-guard makes the deferred
	// call below a harmless no-op once the explicit release has run, and
	// still unlocks on every early-return path before that point (mirrors
	// the releaseUnlock idiom in registerOneLanguageSupervised,
	// register_supervisor.go).
	releaseRegLock := func() {
		if unlock != nil {
			unlock()
			unlock = nil
		}
	}
	defer releaseRegLock()
	if err := reg.Load(); err != nil {
		return err
	}

	// 4. Reject duplicate registration. B.1 default-unregister-LSP-only
	// semantics make it ambiguous to silently re-register a serena row
	// (it would clobber the Languages snapshot + RegisteredAt timestamp
	// from a prior `register --default` invocation).
	if _, exists := reg.GetSerena(wsKey); exists {
		return fmt.Errorf("workspace %s (key %s) is already registered for serena — "+
			"run `mcphub workspace unregister %s --backend serena` first if you intend to re-register",
			canonical, wsKey, canonical)
	}

	// 5. Allocate port from the serena pool.
	port, err := reg.AllocateSerenaPort(pool)
	if err != nil {
		return err
	}

	// 6. Build entry and PutSerena. Task name follows the serena-per-
	// workspace convention used by Phase D.2 (`mcp-local-hub-serena-<key>`).
	entry := api.WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: canonical,
		Language:      api.SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          port,
		TaskName:      fmt.Sprintf("mcp-local-hub-serena-%s", wsKey),
		RegisteredAt:  time.Now().UTC(),
		RegisteredVia: "manual",
		Languages:     languages,
	}
	if err := reg.PutSerena(entry); err != nil {
		return err
	}
	if err := reg.Save(); err != nil {
		return err
	}
	// Release the registry lock NOW — see the releaseRegLock doc comment at
	// acquisition time (step 3) for why this must happen before step 8 below.
	releaseRegLock()

	// 6b. EXPLICIT serena register → bless this workspace's canonical root as a
	//     trusted root (area-5 co-design — REGRESSION-SAFETY). This is the serena
	//     counterpart of the LSP explicit-register bless (internal/api/register.go
	//     registerBlessTrustedRootFn). Without it the area-5 serena trust gate
	//     (AutoRegisterSerenaWorkspace step 2.5) would block the out-of-box serena
	//     auto-introduce that the install-and-it-works epic shipped: an explicit
	//     register would NOT seed trust for the tree, so a sibling `.serena`
	//     project under the same root could never auto-introduce. Blessing the
	//     canonical root here seeds the tree exactly as the LSP path does.
	//
	//     CRITICAL INVARIANT: this bless is reachable ONLY from EXPLICIT operator
	//     actions (this register command + `mcphub trust` + `mcphub setup
	//     --trusted-root` + the GUI Trusted-Roots panel), NEVER from the router's
	//     AutoRegisterSerenaWorkspace path — a self-blessing router would let an
	//     untrusted tool-call path bless itself and pass the gate on the very next
	//     request, re-opening the vulnerability (area-5 claim 10).
	//
	//     Bless the CANONICAL root the row stores (the same value the trust gate
	//     canonicalizes + compares). Best-effort / warn-only: a bless failure does
	//     NOT fail the register (the workspace IS registered); the worst case is a
	//     sibling needing its own explicit register/trust.
	if err := serenaRegisterBlessTrustedRootFn(canonical); err != nil {
		fmt.Fprintf(cmd.OutOrStderr(),
			"warning: workspace registered but could not record %s as a trusted root "+
				"(serena auto-introduce of sibling projects under this tree may require "+
				"`mcphub trust %s` or explicit register): %v\n", canonical, canonical, err)
	}

	// 7. Optionally write the default marker. Errors here are non-fatal
	// — registration already succeeded; the default is a UX nicety.
	if setDefault {
		if err := writeDefaultWorkspace(filepath.Dir(regPath), canonical); err != nil {
			fmt.Fprintf(cmd.OutOrStderr(),
				"warning: workspace registered but failed to write default marker: %v\n", err)
		}
	}

	// 8. Materialize the registration through the supervisor. Nudge an
	//    apply-mode reconcile: the running supervisor's handleReconcile
	//    self-heals any registry/intent split (RepairSerenaIntentFromRegistry,
	//    called server-side) BEFORE computing drift — so THIS workspace's
	//    just-committed registry row is appended to supervisor-intent.json
	//    and reconciled in the same round trip.
	//
	//    This is the fix for the register/unregister asymmetry: unregister
	//    already has a supervisor-materialization counterpart
	//    (removeSerenaSupervisorIntentFn ->
	//    RemoveSerenaSupervisorIntentForWorkspace), but register had none —
	//    it only ever wrote workspaces.yaml and printed an unconditional
	//    success, regardless of whether any daemon would ever spawn.
	//
	//    Gate the final success message on BOTH: the reconcile request being
	//    acknowledged (no error) AND a spec-bearing supervisor-intent row
	//    actually present for this workspace key afterward. Printing an
	//    unqualified success without both would reproduce the exact defect
	//    this fixes. On anything short of full success, KEEP the registry
	//    row (it is a valid, durable registration — the supervisor's own
	//    startup self-heal or a later `mcphub reconcile --apply` will still
	//    pick it up) and report an explicit, actionable partial-state error
	//    instead of rolling back.
	reconcileCtx, cancel := context.WithTimeout(cmd.Context(), api.DefaultReconcileTimeout)
	defer cancel()
	_, reconcileErr := serenaRegisterReconcileFn(reconcileCtx, true)
	materialized, checkErr := serenaRegisterIntentCheckFn(wsKey)
	if reconcileErr != nil || checkErr != nil || !materialized {
		return workspaceRegisterPartialStateError(canonical, wsKey, entry, reconcileErr, checkErr)
	}

	fmt.Fprintf(cmd.OutOrStdout(),
		"Registered serena workspace %s (key %s)\n  port: %d\n  task: %s\n  languages: %s\n",
		canonical, wsKey, entry.Port, entry.TaskName, strings.Join(languages, ", "))
	if setDefault {
		fmt.Fprintln(cmd.OutOrStdout(), "  default: yes")
	}
	return nil
}

// serenaRegisterReconcileFn is the test-injectable seam over the
// post-register supervisor materialization nudge (step 8 above). Production:
// api.DialSupervisorIPCReconcile with apply=true — mirrors the api-package
// seam shape (registerSupervisorReconcileFn, internal/api/register_supervisor.go:18).
// Tests stub this to avoid a live supervisor / IPC transport; a stub that
// wants to faithfully model "the supervisor received the request and
// self-healed" calls the real api.RepairSerenaIntentFromRegistry itself
// before returning its canned response — see
// TestWorkspaceRegisterSerena_LiveSupervisorMaterializesBeforeSuccess.
var serenaRegisterReconcileFn = api.DialSupervisorIPCReconcile

// serenaRegisterIntentCheckFn is the test-injectable seam that checks
// whether a spec-bearing supervisor-intent daemon row exists for a serena
// workspace key, called AFTER the reconcile-apply nudge (step 8 above) to
// gate the final success message. Defaults to the named production function
// below (rather than an inline closure) so a test that stubs it for one
// scenario can restore the REAL check for another by referencing
// realSerenaRegisterIntentCheck directly, instead of duplicating its body.
var serenaRegisterIntentCheckFn = realSerenaRegisterIntentCheck

// realSerenaRegisterIntentCheck is the production body of
// serenaRegisterIntentCheckFn: it reads supervisor-intent.json fresh through
// the single-owner predicate
// (api.SupervisorIntentFile).HasSpecBearingSerenaDaemonForWorkspaceKey. A
// missing intent file means "not yet materialized" (false, nil) rather than
// an error — the caller already has an explicit reconcileErr to report when
// the IPC call itself failed; this seam only answers the observable
// after-the-fact question "is the daemon row there now".
func realSerenaRegisterIntentCheck(wsKey string) (bool, error) {
	intentPath, err := api.DefaultSupervisorIntentPath()
	if err != nil {
		return false, err
	}
	intent, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return intent.HasSpecBearingSerenaDaemonForWorkspaceKey(wsKey), nil
}

// workspaceRegisterPartialStateError builds the operator-facing error for a
// serena register whose registry row is committed and KEPT, but whose
// supervisor-intent daemon row is not (yet) confirmed spec-bearing. Per the
// design this fixes: never print an unqualified success while the workspace
// is unusable — either it converged, or the message says plainly what did
// not happen and what to run next.
func workspaceRegisterPartialStateError(canonical, wsKey string, entry api.WorkspaceEntry, reconcileErr, checkErr error) error {
	var b strings.Builder
	fmt.Fprintf(&b, "workspace %s (key %s) is registered in workspaces.yaml (port %d, task %s), "+
		"but no spec-bearing supervisor daemon row is confirmed yet — the registration was kept, "+
		"nothing was rolled back.\n", canonical, wsKey, entry.Port, entry.TaskName)
	switch {
	case reconcileErr != nil && errors.Is(reconcileErr, api.ErrSupervisorIPCUnavailable):
		b.WriteString("  reason: no supervisor is running. Start it with `mcphub supervise` " +
			"(or wait for autostart) — the supervisor's own startup self-heal will pick up this " +
			"registration automatically.\n")
	case reconcileErr != nil:
		fmt.Fprintf(&b, "  reason: the supervisor reconcile request failed: %v\n", reconcileErr)
		b.WriteString("  next step: once the supervisor is healthy, run `mcphub reconcile --apply` " +
			"to retry materializing this workspace.\n")
	case checkErr != nil:
		fmt.Fprintf(&b, "  reason: could not verify the supervisor intent afterward: %v\n", checkErr)
	default:
		b.WriteString("  reason: the supervisor acknowledged the reconcile request but reported no " +
			"spec-bearing daemon row for this workspace yet — this happens when this is the FIRST " +
			"serena workspace and the dynamic pool has not been introduced (run `mcphub migrate serena " +
			"legacy-to-dynamic-pool`), or when the self-heal was skipped due to a momentarily contended " +
			"lock (retry with `mcphub reconcile --apply`). Check supervisor-events.log for a " +
			"`serena-intent-repair-*` entry naming the exact cause.\n")
	}
	return errors.New(b.String())
}

// serenaRegisterBlessTrustedRootFn is the explicit-serena-register bless seam
// (area-5 co-design). Production blesses the workspace's canonical root in the
// shared trusted-roots store via api.BlessDefaultTrustedRoot — the SAME owner
// the LSP register / `mcphub trust` / `mcphub setup --trusted-root` / GUI panel
// use, so the CLI never hand-rolls a store write. Tests override it (to assert
// the bless fired with the canonical root, and to keep register tests off the
// real store). The ROUTER auto-register path (AutoRegisterSerenaWorkspace) does
// NOT use this seam — a self-blessing router would re-open the vulnerability
// (area-5 claim 10).
var serenaRegisterBlessTrustedRootFn = func(canonicalWorkspaceRoot string) error {
	return api.BlessDefaultTrustedRoot(canonicalWorkspaceRoot)
}

// newWorkspaceUnregisterCmd builds `mcphub workspace unregister <path>
// [--backend serena|all|<name>]`.
func newWorkspaceUnregisterCmd() *cobra.Command {
	var backend string
	c := &cobra.Command{
		Use:   "unregister <path>",
		Short: "Remove a workspace from the registry",
		Long: `Drop registry rows for <path> via B.1's RemoveByBackend semantics.

--backend handling:
  (omitted)       remove only LSP rows; the serena (sentinel) row stays.
                  This matches the B.1 v5 default that lets operators
                  disable LSP routing while keeping the long-lived
                  serena daemon registered.
  --backend serena   remove only the serena (sentinel) row; LSP rows stay.
  --backend all      remove every row for <path>.
  --backend NAME     remove only LSP rows whose Backend or Language field
                     equals NAME (e.g. "mcp-language-server" / "go" /
                     "gopls-mcp"). Sentinel rows are NOT included.

The .serena/ directory on disk is never touched — disk state survives
unregister so re-registering later replays the same languages snapshot.

Examples:
  mcphub workspace unregister D:\dev\PaperPane
  mcphub workspace unregister D:\dev\PaperPane --backend serena
  mcphub workspace unregister D:\dev\PaperPane --backend all
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceUnregister(cmd, args[0], backend)
		},
	}
	c.Flags().StringVar(&backend, "backend", "",
		"backend filter: empty (LSP-only), serena, all, backend name, or LSP language name")
	return c
}

func runWorkspaceUnregister(cmd *cobra.Command, rawPath, backend string) error {
	// Use the existence-tolerant variant so an operator can unregister a
	// workspace whose directory has since been deleted or moved.
	canonical, err := api.CanonicalWorkspacePathForCleanup(rawPath)
	if err != nil {
		return err
	}
	wsKey := api.WorkspaceKey(canonical)
	legacyCanonical, err := api.CanonicalWorkspacePathLegacyCompat(rawPath)
	if err != nil {
		return err
	}
	legacyWSKey := api.WorkspaceKey(legacyCanonical)

	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		return err
	}

	// Decide, under a brief registry read, which LSP languages this
	// --backend filter targets and whether the serena (sentinel) row is in
	// scope — WITHOUT mutating yet. The mutation is split into two phases so
	// each owner does its own paired teardown:
	//
	//   - LSP rows go through unregisterLSPWorkspaceFn ((*api.API).Unregister),
	//     which removes each (workspace_key, language) registry row TOGETHER
	//     WITH its supervisor-intent descriptor (removeLSPSupervisorIntent +
	//     reconcile). The bare reg.RemoveByBackend used previously dropped the
	//     row but left the intent descriptor behind, so the supervisor would
	//     later spawn the now-unbacked proxy → "not registered" exit 1 →
	//     restart-backoff → quarantine. (*api.API).Unregister acquires its OWN
	//     registry lock, so we must NOT hold the lock across that call.
	//   - The serena (sentinel) row owns a per-workspace supervisor-intent
	//     descriptor keyed by api.SerenaTaskNameForWorkspace(canonical). The
	//     registry row is removed directly via RemoveByBackend("serena"), then
	//     the descriptor teardown nudges a running supervisor to reconcile.
	lspLangs, removeSerena, err := classifyWorkspaceUnregister(regPath, wsKey, legacyWSKey, backend)
	if err != nil {
		return err
	}
	if len(lspLangs) == 0 && !removeSerena {
		return fmt.Errorf("no registry rows match workspace %s (key %s) with --backend=%q",
			canonical, wsKey, backend)
	}

	// The two mutation phases run through api.PruneWorkspacePhases — the SHARED
	// two-phase sequencer that also backs the GUI auto-prune sweeper — so the
	// LSP-then-serena ordering + the bot-r32-P2 teardown-before-delete invariant
	// live in ONE owner (no logic duplication). The CLI keeps its own classify
	// (above), output, default-marker clearing (below), and test-injectable seams
	// (unregisterLSPWorkspaceFn / removeSerenaSupervisorIntentFn) by passing them
	// as the teardown closures here.
	//
	// Phase-split atomicity tradeoff (deep-sec P3-a). The classify (above) and
	// the two mutation phases run under SEPARATE registry locks — they have to,
	// because (*api.API).Unregister acquires its own lock and would deadlock
	// against a held one. A concurrent same-user actor (a second `mcphub
	// workspace unregister`, an auto-register, a migrate) that empties the LSP
	// rows BETWEEN classify and Phase 1 can make Phase 1 error; for `--backend
	// all` we then return WITHOUT having removed the serena (Phase 2) row, so the
	// two backends are no longer dropped atomically the way the old
	// single-lock RemoveByBackend("all") did. This is a SAFE, fail-loud,
	// RETRYABLE outcome — not corruption: the serena row simply remains and a
	// re-run of the same command removes it (classify will then see only the
	// serena row in scope). The window is a same-user race only (the registry is
	// a per-user 0600 boundary), so the operator who hit it is the one who can
	// re-run.
	report := &api.PruneReport{Workspace: canonical, WorkspaceKey: wsKey}
	td := api.PruneWorkspaceTeardown{
		// The CLI seam takes the operator-supplied rawPath (not the resolved
		// canonical) so api.Unregister can compute its own legacy-key fallback.
		LSPUnregister: func(_ string, langs []string) (*api.UnregisterReport, error) {
			return unregisterLSPWorkspaceFn(rawPath, langs)
		},
		RemoveSerenaIntent: removeSerenaSupervisorIntentFn,
		DeleteSerenaRow: func() (int, error) {
			reg := api.NewRegistry(regPath)
			unlock, err := reg.Lock()
			if err != nil {
				return 0, err
			}
			defer unlock()
			if err := reg.Load(); err != nil {
				return 0, err
			}
			n := reg.RemoveByBackend(wsKey, "serena")
			if legacyWSKey != wsKey {
				n += reg.RemoveByBackend(legacyWSKey, "serena")
			}
			if err := reg.Save(); err != nil {
				return 0, err
			}
			return n, nil
		},
	}
	if err := api.PruneWorkspacePhases(rawPath, canonical, lspLangs, len(lspLangs) > 0, removeSerena, td, report); err != nil {
		return err
	}
	for _, warn := range report.Warnings {
		fmt.Fprintf(cmd.OutOrStderr(), "warning: %s\n", warn)
	}
	removed := len(report.LSPRemoved) + report.SerenaRemoved

	// If the default marker pointed at this workspace AND we removed the
	// serena row (or --backend all), clear the marker. Otherwise stale
	// default would route Phase F to a workspace that no longer exists.
	if backend == "all" || backend == "serena" {
		_ = clearDefaultIfMatches(filepath.Dir(regPath), canonical)
	}

	fmt.Fprintf(cmd.OutOrStdout(),
		"Removed %d registry row(s) for workspace %s (key %s) with --backend=%q\n",
		removed, canonical, wsKey, backend)
	return nil
}

// classifyWorkspaceUnregister reads the registry under a brief lock and returns
// the LSP languages and serena-row scope a `mcphub workspace unregister
// --backend <filter>` invocation targets, WITHOUT mutating. The drop logic
// mirrors (*api.Registry).RemoveByBackend exactly so the unregister surface
// keeps identical --backend semantics while routing LSP rows through the paired
// teardown instead of a bare row delete:
//
//   - ""        → every LSP row (Language != serena sentinel); serena stays.
//   - "all"     → every LSP row + the serena row.
//   - "serena"  → the serena row only.
//   - <name>    → LSP rows whose Backend or Language equals <name>.
func classifyWorkspaceUnregister(regPath, wsKey, legacyWSKey, backend string) (lspLangs []string, removeSerena bool, err error) {
	reg := api.NewRegistry(regPath)
	unlock, lerr := reg.Lock()
	if lerr != nil {
		return nil, false, lerr
	}
	defer unlock()
	if lerr := reg.Load(); lerr != nil {
		return nil, false, lerr
	}
	for _, e := range workspaceRowsForUnregister(reg, wsKey, legacyWSKey) {
		isSerena := e.Language == api.SerenaLanguageSentinel
		switch backend {
		case "":
			if !isSerena {
				lspLangs = appendUniqueString(lspLangs, e.Language)
			}
		case "all":
			if isSerena {
				removeSerena = true
			} else {
				lspLangs = appendUniqueString(lspLangs, e.Language)
			}
		case "serena":
			if isSerena {
				removeSerena = true
			}
		default:
			if !isSerena && (e.Backend == backend || e.Language == backend) {
				lspLangs = appendUniqueString(lspLangs, e.Language)
			}
		}
	}
	return lspLangs, removeSerena, nil
}

func workspaceRowsForUnregister(reg *api.Registry, wsKey, legacyWSKey string) []api.WorkspaceEntry {
	rows := append([]api.WorkspaceEntry{}, reg.ListByWorkspace(wsKey)...)
	if legacyWSKey != "" && legacyWSKey != wsKey {
		rows = append(rows, reg.ListByWorkspace(legacyWSKey)...)
	}
	return rows
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// newWorkspaceListCmd builds `mcphub workspace list [--json]`.
func newWorkspaceListCmd() *cobra.Command {
	var jsonOut bool
	c := &cobra.Command{
		Use:   "list",
		Short: "List registered serena workspaces",
		Long: `Enumerate every serena (sentinel) row in the registry. Default output
is a human-readable table; --json emits the full WorkspaceEntry array
plus the per-row "default" flag derived from the sidecar marker.

Columns: WORKSPACE | LANGUAGES | DEFAULT | PORT | LAST_SPAWN

LAST_SPAWN is the LastMaterializedAt timestamp (set by Phase D's
supervisor reconciler when the daemon is first spawned). Until Phase D
lands, the column reads "-" for freshly-registered workspaces.
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceList(cmd, jsonOut)
		},
	}
	c.Flags().BoolVar(&jsonOut, "json", false, "machine-readable JSON output")
	return c
}

// workspaceListJSONRow is the JSON shape returned by `workspace list --json`.
// It embeds the full WorkspaceEntry verbatim plus a synthesized "default" flag.
type workspaceListJSONRow struct {
	api.WorkspaceEntry
	Default bool `json:"default"`
}

func runWorkspaceList(cmd *cobra.Command, jsonOut bool) error {
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		return err
	}
	reg := api.NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := reg.Load(); err != nil {
		return err
	}
	defaultPath, _ := readDefaultWorkspace(filepath.Dir(regPath))

	entries := reg.SerenaEntries()
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].WorkspacePath < entries[j].WorkspacePath
	})

	if jsonOut {
		rows := make([]workspaceListJSONRow, 0, len(entries))
		for _, e := range entries {
			rows = append(rows, workspaceListJSONRow{
				WorkspaceEntry: e,
				Default:        e.WorkspacePath == defaultPath,
			})
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	return printWorkspaceTable(cmd.OutOrStdout(), entries, defaultPath)
}

// workspaceTablePathWidth is the column width for the WORKSPACE column
// in `mcphub workspace list` output. Wide enough for typical
// project paths (deep nested temp dirs in CI tests can exceed this and
// will be truncated via the shared truncate helper).
const workspaceTablePathWidth = 80

// printWorkspaceTable renders the column-aligned table form. Extracted so
// tests can exercise the layout independently of cobra dispatch.
func printWorkspaceTable(w io.Writer, entries []api.WorkspaceEntry, defaultPath string) error {
	fmt.Fprintf(w, "%-*s %-30s %-7s %-6s %-12s\n",
		workspaceTablePathWidth,
		"WORKSPACE", "LANGUAGES", "DEFAULT", "PORT", "LAST_SPAWN")
	for _, e := range entries {
		def := ""
		if e.WorkspacePath == defaultPath {
			def = "*"
		}
		fmt.Fprintf(w, "%-*s %-30s %-7s %-6d %-12s\n",
			workspaceTablePathWidth,
			truncateWorkspacePath(e.WorkspacePath, workspaceTablePathWidth),
			truncate(strings.Join(e.Languages, ","), 30),
			def,
			e.Port,
			formatLastSpawn(e.LastMaterializedAt))
	}
	return nil
}

// formatLastSpawn renders the LastMaterializedAt timestamp. Zero = "-".
func formatLastSpawn(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02")
}

// newWorkspaceSetDefaultCmd builds `mcphub workspace set-default <path>`.
func newWorkspaceSetDefaultCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "set-default <path>",
		Short: "Mark a registered serena workspace as the default for no-path-args routing",
		Long: `Persist the canonical path of <path> as the operator-selected default
for Phase F's no-path-args routing fallback. The marker lives in a
sidecar file (` + defaultWorkspaceFilename + `) next to workspaces.yaml.

The workspace MUST already be registered via ` + "`mcphub workspace register`" + `;
the command refuses an unknown workspace_key with an explicit error.

To clear the default, pass an empty string (` + "`mcphub workspace set-default ''`" + `)
or unregister the workspace via ` + "`mcphub workspace unregister --backend serena|all`" + `,
which clears the marker as a side effect.
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceSetDefault(cmd, args[0])
		},
	}
	return c
}

func runWorkspaceSetDefault(cmd *cobra.Command, rawPath string) error {
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		return err
	}
	stateDir := filepath.Dir(regPath)
	reg := api.NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	// Empty string clears the marker.
	if strings.TrimSpace(rawPath) == "" {
		if err := writeDefaultWorkspace(stateDir, ""); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Cleared default workspace.")
		return nil
	}

	canonical, err := api.CanonicalWorkspacePath(rawPath)
	if err != nil {
		return err
	}
	wsKey := api.WorkspaceKey(canonical)

	if err := reg.Load(); err != nil {
		return err
	}
	if _, ok := reg.GetSerena(wsKey); !ok {
		return fmt.Errorf("workspace %s (key %s) is not registered for serena — "+
			"run `mcphub workspace register %s` first",
			canonical, wsKey, canonical)
	}
	if err := writeDefaultWorkspace(stateDir, canonical); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Set default serena workspace: %s\n", canonical)
	return nil
}

// newWorkspaceBootstrapCmd builds `mcphub workspace bootstrap <path>
// [--force]` per Phase B.3.
func newWorkspaceBootstrapCmd() *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   "bootstrap <path>",
		Short: "Initialize .serena/project.yml from a file-extension survey",
		Long: `Survey <path> (depth-bounded at 5, gitignore-aware, hardcoded skip
list for node_modules/target/dist/.git) and write a .serena/project.yml
with a detected languages list, read_only=false, and excluded_dirs.

Detection map (mcphub LSP identifiers projected to Serena on write):
  .cpp .hpp .cc .cxx .h     -> cpp
  .go                       -> go
  .ts .tsx                  -> typescript
  .js .jsx                  -> javascript -> typescript
  .py                       -> python
  .rs                       -> rust
  .md                       -> markdown
  .css                      -> vscode-css (omitted: no Serena equivalent)
  .html .htm                -> vscode-html (omitted: no Serena equivalent)
  .f90 .f95 .f03 .f         -> fortran

--force overwrites an existing .serena/project.yml. Without --force, the
command refuses to clobber.
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceBootstrap(cmd, args[0], force)
		},
	}
	c.Flags().BoolVar(&force, "force", false, "overwrite an existing .serena/project.yml")
	return c
}

func runWorkspaceBootstrap(cmd *cobra.Command, rawPath string, force bool) error {
	canonical, err := api.CanonicalWorkspacePath(rawPath)
	if err != nil {
		return err
	}
	projectYmlPath := filepath.Join(canonical, ".serena", "project.yml")
	if !force {
		if _, err := os.Stat(projectYmlPath); err == nil {
			return fmt.Errorf("%s already exists; pass --force to overwrite", projectYmlPath)
		}
	}

	mcphubLanguages, err := surveyLanguages(canonical, 5)
	if err != nil {
		return fmt.Errorf("survey languages: %w", err)
	}
	sort.Strings(mcphubLanguages)
	languages := projectSerenaLanguages(mcphubLanguages, cmd.ErrOrStderr())

	// Write .serena/project.yml. Use a stable schema matching upstream
	// serena's expectations (languages, read_only, excluded_dirs).
	doc := map[string]any{
		"languages":     languages,
		"read_only":     false,
		"excluded_dirs": []string{"node_modules", "target", "dist", ".git"},
	}
	body, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal project.yml: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(projectYmlPath), 0o700); err != nil {
		return fmt.Errorf("mkdir .serena: %w", err)
	}
	if err := os.WriteFile(projectYmlPath, body, 0o600); err != nil {
		return fmt.Errorf("write project.yml: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n  languages: %s\n",
		projectYmlPath, strings.Join(languages, ", "))
	return nil
}

// ------------------------------------------------------------------
// File-extension survey for Phase B.3 bootstrap
// ------------------------------------------------------------------

// alwaysSkipDirs is the hardcoded skip list applied regardless of
// .gitignore content. Plan §B.3 acceptance criterion.
var alwaysSkipDirs = map[string]bool{
	"node_modules": true,
	"target":       true,
	"dist":         true,
	".git":         true,
}

// extensionToLanguage maps file extensions (lower-case, with leading dot) to
// mcphub's own LSP/backend language identifiers. These identifiers are not a
// Serena contract and must pass through projectSerenaLanguages before they are
// written to .serena/project.yml.
var extensionToLanguage = map[string]string{
	".cpp": "cpp", ".hpp": "cpp", ".cc": "cpp", ".cxx": "cpp", ".h": "cpp",
	".go": "go",
	".ts": "typescript", ".tsx": "typescript",
	".js": "javascript", ".jsx": "javascript",
	".py":   "python",
	".rs":   "rust",
	".md":   "markdown",
	".css":  "vscode-css",
	".html": "vscode-html", ".htm": "vscode-html",
	".f90": "fortran", ".f95": "fortran", ".f03": "fortran", ".f": "fortran",
}

// mcphubToSerenaLanguage is the single owner of the cross-tool language
// taxonomy boundary. Values must be members of Serena's Language enum. An
// absent key means Serena has no safe equivalent and is omitted fail-closed.
//
// clangd is not emitted by the extension survey today (C/C++ maps directly to
// cpp), but it is a shipped mcphub LSP language and belongs in this complete
// boundary mapping alongside javascript.
var mcphubToSerenaLanguage = map[string]string{
	"clangd":     "cpp",
	"cpp":        "cpp",
	"fortran":    "fortran",
	"go":         "go",
	"javascript": "typescript",
	"markdown":   "markdown",
	"python":     "python",
	"rust":       "rust",
	"typescript": "typescript",
}

// projectSerenaLanguages maps mcphub language identifiers onto Serena's enum,
// de-duplicates converging mappings (notably javascript + typescript), and
// omits unknown identifiers so a future mcphub backend cannot crash Serena.
func projectSerenaLanguages(mcphubLanguages []string, debug io.Writer) []string {
	seen := make(map[string]struct{}, len(mcphubLanguages))
	projected := make([]string, 0, len(mcphubLanguages))
	for _, mcphubLanguage := range mcphubLanguages {
		serenaLanguage, ok := mcphubToSerenaLanguage[mcphubLanguage]
		if !ok {
			if debug != nil {
				fmt.Fprintf(debug, "mcphub: debug: Serena bootstrap omitted unsupported mcphub language %q\n", mcphubLanguage)
			}
			continue
		}
		if _, duplicate := seen[serenaLanguage]; duplicate {
			continue
		}
		seen[serenaLanguage] = struct{}{}
		projected = append(projected, serenaLanguage)
	}
	sort.Strings(projected)
	return projected
}

// surveyLanguages walks <root> bounded at maxDepth, gitignore-aware,
// and returns a deterministic-order list of unique languages it found.
// Returns an empty slice if no recognized extensions found.
func surveyLanguages(root string, maxDepth int) ([]string, error) {
	if maxDepth <= 0 {
		maxDepth = 5
	}
	rootIgnore := readGitignoreDirs(filepath.Join(root, ".gitignore"))
	ignoreByDir := map[string]map[string]bool{
		root: rootIgnore,
	}

	seen := map[string]bool{}
	rootDepth := strings.Count(filepath.ToSlash(root), "/")

	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			// Tolerate per-entry permission errors so a single unreadable
			// subdir does not abort the entire survey.
			return nil
		}
		// Depth calculation: count the number of separators relative to
		// root. filepath.Walk gives us absolute-ish paths; converting to
		// slash form makes this OS-portable.
		curDepth := strings.Count(filepath.ToSlash(path), "/") - rootDepth
		if info.IsDir() {
			name := info.Name()
			if path != root {
				if alwaysSkipDirs[name] {
					return filepath.SkipDir
				}
				parentIgnore := ignoreByDir[filepath.Dir(path)]
				if parentIgnore[name] {
					return filepath.SkipDir
				}
				subIgnore := cloneGitignoreDirs(parentIgnore)
				for k := range readGitignoreDirs(filepath.Join(path, ".gitignore")) {
					subIgnore[k] = true
				}
				ignoreByDir[path] = subIgnore
			}
			if path == root {
				// Root rules are seeded before the walk so they apply to
				// immediate child directories without affecting siblings
				// of nested .gitignore files.
				ignoreByDir[path] = cloneGitignoreDirs(rootIgnore)
			}
			if curDepth >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		// File: classify by extension.
		ext := strings.ToLower(filepath.Ext(info.Name()))
		if lang, ok := extensionToLanguage[ext]; ok {
			seen[lang] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	langs := make([]string, 0, len(seen))
	for l := range seen {
		langs = append(langs, l)
	}
	return langs, nil
}

func cloneGitignoreDirs(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// readGitignoreDirs reads a .gitignore file and returns the set of
// directory-name patterns it lists. Empty lines, comments, negation
// (`!`) entries, root-anchored entries, path entries, and glob entries
// are ignored. This is a deliberate simplification: Phase B.3 only needs
// "skip this directory name" behavior, not full gitignore parsing.
func readGitignoreDirs(path string) map[string]bool {
	out := map[string]bool{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		if strings.HasPrefix(line, "/") {
			continue
		}
		line = strings.TrimRight(line, "/")
		if line == "" {
			continue
		}
		if strings.ContainsAny(line, "/\\") {
			continue
		}
		if strings.ContainsAny(line, "*?[") {
			continue
		}
		out[line] = true
	}
	// A scan error (e.g. bufio.ErrTooLong on a pathologically long .gitignore
	// line) ends the loop early and silently truncates the ignore-dir set.
	// Surface it rather than swallow it; the entries parsed so far are still
	// returned best-effort.
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "mcphub: warning: .gitignore scan ended early (%s): %v\n", path, err)
	}
	return out
}

// ------------------------------------------------------------------
// .serena/project.yml read helper for `workspace register`
// ------------------------------------------------------------------

// serenaProjectYml is the minimal struct we need from .serena/project.yml.
// We only consume the languages list; other serena fields are preserved
// verbatim on disk (we never rewrite project.yml from register).
type serenaProjectYml struct {
	Languages []string `yaml:"languages"`
}

func readSerenaProjectLanguages(canonical string) ([]string, error) {
	path := filepath.Join(canonical, ".serena", "project.yml")
	// The marker is untrusted clone input — read it through the SAME single
	// hardened reader the auto-register path uses (api.ReadUntrustedSerenaProjectYML:
	// regular-file-only, 64 KiB size cap, TOCTOU-safe open). This call is a
	// synchronous CLI command with no cancellation source, so a background ctx
	// is correct. The reader returns the bare not-found error (unwrapped), so the
	// caller's os.IsNotExist(err) "run bootstrap first" branch keeps working.
	data, err := api.ReadUntrustedSerenaProjectYML(context.Background(), path)
	if err != nil {
		return nil, err
	}
	var doc serenaProjectYml
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return doc.Languages, nil
}

// ------------------------------------------------------------------
// default-workspace.txt sidecar marker
// ------------------------------------------------------------------

// writeDefaultWorkspace / readDefaultWorkspace / clearDefaultIfMatches are thin
// CLI-package wrappers over the single api owner (api.*DefaultWorkspace*). They
// exist only so the existing CLI call sites and tests keep their
// cli-package-local identifiers; the implementation lives in
// internal/api/default_workspace_marker.go (shared with the GUI sweeper).
func writeDefaultWorkspace(stateDir, canonical string) error {
	return api.WriteDefaultWorkspace(stateDir, canonical)
}

func readDefaultWorkspace(stateDir string) (string, error) {
	return api.ReadDefaultWorkspace(stateDir)
}

func clearDefaultIfMatches(stateDir, canonical string) error {
	return api.ClearDefaultWorkspaceIfMatches(stateDir, canonical)
}

// ------------------------------------------------------------------
// small helpers
// ------------------------------------------------------------------

func splitAndTrim(s, sep string) []string {
	parts := strings.Split(s, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// (truncate lives in cleanup.go; same package, reused here.)
