// Package api — Register/Unregister for workspace-scoped MCP servers.
//
// Lazy-mode contract (M3 of the 2026-04-20 workspace-scoped plan):
//   - Register creates one scheduler task per (workspace, language) whose
//     command is `mcphub daemon workspace-proxy --port <p> --workspace <ws>
//     --language <lang>`. The proxy itself answers initialize/tools/list
//     synthetically and materializes the heavy backend on first tools/call.
//   - NO LSP binary preflight at register time. Missing binaries surface
//     later at first tools/call via LifecycleMissing.
//   - Each new registry entry starts with Lifecycle=LifecycleConfigured.
//     The proxy itself may re-stamp this on startup, but Register
//     pre-seeds it so `mcphub workspaces` shows a sensible state
//     immediately.
//   - Rollback: if any per-language step fails, every side effect applied
//     so far is reversed in LIFO order (client entries, scheduler tasks,
//     port allocations, registry entries).
//   - Default-all when caller passes an empty languages slice.
package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

// defaultClientBindingsNow resolves the implicit client binding set used when a
// workspace-scoped manifest does not declare client_bindings.
//
// It reads the operator's EFFECTIVE default-install set AT CALL TIME through
// DefaultInstallClientNamesEffectiveIn, which client_install_prefs.go declares
// to be "the single owner of what is the default-install set once an operator
// override may exist". This used to be a package-level `var` snapshot of
// clients.DefaultInstallClientNames() evaluated at package init, which made
// register the ONE consumer that ignored the persisted `clients.default_install`
// override every other consumer honors — install via resolveDefaultClientsOverride
// (install.go), the shared LSP router via ClientInstallEnabledSet
// (lsp_client_router.go), and readiness via this very accessor (readiness.go).
//
// Concretely: an operator who ticks Cursor in Settings → Clients got cursor
// from `mcphub install` but NOT from `mcphub register` or the GUI project-LSP
// toggle, and had no lever to close the gap — register exposes no --clients
// flag, RegisterOpts has no client field, and the only manifest register ever
// loads is the EMBEDDED mcp-language-server manifest, which declares no
// client_bindings (and manifest_source.go deliberately IGNORES a disk copy of a
// shipped server's manifest). That makes this fallback not a rare edge but the
// ONLY path register takes, so the stale snapshot silently governed every
// workspace registration.
//
// Fallback posture mirrors readiness.go's own client-scope resolution: on a
// read error (corrupt or unreadable gui-preferences.yaml) fall back to the
// compile-time default set
// rather than failing the register. The override is a convenience layer and
// must never make `mcphub register` harder to complete than before it existed.
//
// Relay-stdio adapters (e.g. Antigravity's stdio-relay model) are excluded by
// the IsRelayStdio filter because they require relay context, not a URL-only
// workspace binding. Those dropped names are RETURNED alongside the bindings —
// see relayStdioBindingWarning for why they must never be dropped silently.
func defaultClientBindingsNow() ([]config.ClientBinding, []string) {
	names := clients.DefaultInstallClientNames()
	if eff, err := (&API{}).DefaultInstallClientNamesEffectiveIn(SettingsPath()); err == nil && len(eff) > 0 {
		names = eff
	}
	bindings := make([]config.ClientBinding, 0, len(names))
	var droppedRelayStdio []string
	for _, name := range names {
		if clients.IsRelayStdio(name) {
			droppedRelayStdio = append(droppedRelayStdio, name)
			continue
		}
		bindings = append(bindings, config.ClientBinding{Client: name, URLPath: "/mcp"})
	}
	return bindings, droppedRelayStdio
}

// relayStdioBindingWarning renders the operator-visible warning for
// default-install clients this register structurally CANNOT bind.
//
// SetDefaultInstallClientNames accepts any non-empty subset of
// clients.SupportedClientNames(), and the Settings → Clients panel
// (ClientInstallToggleViewIn) renders EVERY supported client as a toggle —
// including the relay-stdio ones. So `clients.default_install = zed` is a
// reachable, VALID operator state in which the IsRelayStdio filter above drops
// every name and register writes ZERO client entries. Before register learned
// to read the override this state was unreachable (the compile-time fallback is
// {claude-code, codex-cli}, both URL-native); honoring the override made it
// reachable, so honoring it must also make it visible.
//
// POSTURE: warn, do NOT substitute. The tempting alternative — mirror
// DefaultInstallClientNamesOverrideIn's "nothing valid survived ⇒ no override"
// fallback to the compile-time set — does NOT apply here and would be worse.
// That fallback fires when the operator's intent is UNRECOVERABLE (every
// persisted name blank/unknown, i.e. nothing was validly expressed). Here the
// intent is valid and precise; it is register that cannot honor part of it.
// Substituting {claude-code, codex-cli} would write entries into clients the
// operator explicitly DESELECTED, and would re-open the very cross-lane
// divergence this branch closed — pointing the other way, with register binding
// clients `mcphub install` would not. Guessing an intent the operator did not
// express is not a fix; saying plainly what was skipped is.
//
// The string goes to BOTH surfaces: the writer (CLI stderr) and
// RegisterReport.Warnings, which the GUI project-LSP toggle returns to the
// browser (internal/gui/projects_toggle.go → projectToggleResponse.Warnings).
func relayStdioBindingWarning(bindings []config.ClientBinding, droppedRelayStdio []string) string {
	if len(droppedRelayStdio) == 0 {
		return ""
	}
	msg := fmt.Sprintf(
		"default-install client(s) %s are relay-stdio and cannot take the URL-only entry `mcphub register` writes (they need a stdio relay entry); they were skipped",
		strings.Join(droppedRelayStdio, ", "))
	if len(bindings) > 0 {
		return msg + " — install them with `mcphub install` instead"
	}
	return msg + ", leaving NO client bound by this registration: the workspace proxy will run with no client config pointing at it. " +
		"Add a URL-capable client (claude-code, codex-cli, cursor, …) in Settings → Clients, or reach the relay-stdio client(s) via `mcphub install`"
}

// effectiveClientBindings is the SINGLE owner of "which clients does THIS
// registration WRITE to". Both consumers must read it: the write path (which
// creates the managed entries) and the post-register direct-LSP cleanup
// (which deletes the entries those managed ones replace).
//
// Splitting that question between two call sites is what made cursor's move to
// opt-in unsafe: the write path narrowed to the bound set while
// cleanupDirectLanguageServerEntriesAfterRegister kept looping over EVERY
// installed client, so an opt-in client's working direct entry was backed up
// and removed with nothing put in its place — leaving that client disconnected
// (bot PR #583 finding 7). While cursor was still a default the two agreed by
// accident and the divergence was invisible.
//
// NOTE the cleanup's question is the STRICTLY WIDER "does this client end up
// with a working hub-managed replacement": a binding here is one way to get
// one, an already-live shared LSP-router entry is the other. See
// lspCleanupAliasesForClient.
//
// RESOLVE ONCE PER REGISTER. Being the single owner is not enough if the owner
// is CONSULTED repeatedly: this reads gui-preferences.yaml at call time, and a
// register spans tens of seconds (sch.Create + sch.Run + a readiness probe with
// a 10s ceiling, per language). Inside the GUI process `POST
// /api/client-install-prefs` and the project-LSP toggle that calls Register are
// independent handlers sharing no lock, so an operator ticking a client mid-loop
// used to make the write path and the cleanup gate disagree ACROSS TIME: early
// languages written without the new client, cleanup then judging every language
// against the NEW set and deleting that client's live direct entries with no
// replacement. Same defect class as the original list divergence, reached
// through timing instead. So registerWithManifest calls this ONCE, before the
// language loop, and threads the result into registerOneLanguage /
// registerOneLanguageSupervised and the cleanup — exactly as routerPort is
// already resolved once per cleanup run.
//
// The second return value is the relay-stdio client names dropped from the
// operator's selection; see relayStdioBindingWarning.
func effectiveClientBindings(m *config.ServerManifest) ([]config.ClientBinding, []string) {
	if len(m.ClientBindings) > 0 {
		return m.ClientBindings, nil
	}
	return defaultClientBindingsNow()
}

// boundClientNames is the set of client names the supplied bindings write to.
// The post-register cleanup consults it to answer "did THIS registration just
// write this client a managed entry for every registered language".
func boundClientNames(bindings []config.ClientBinding) map[string]bool {
	out := make(map[string]bool, len(bindings))
	for _, b := range bindings {
		out[b.Client] = true
	}
	return out
}

type routerReplacementCandidateKind string

const (
	routerReplacementNotCandidate  routerReplacementCandidateKind = "not-candidate"
	routerReplacementStructural    routerReplacementCandidateKind = "structural-candidate"
	routerReplacementIndeterminate routerReplacementCandidateKind = "indeterminate"
)

type routerReplacementCandidate struct {
	kind      routerReplacementCandidateKind
	entryPort int
}

// inspectClientLSPRouterReplacement is the single configured-entry inspection
// owner. It validates entry presence, enablement, route shape, language,
// ownership, and the positive observed port. No configured GUI port participates
// in candidate selection or authorization.
func inspectClientLSPRouterReplacement(client registerClient, language string) routerReplacementCandidate {
	entryName := LSPRouterEntryName(language)
	live, err := client.GetEntry(entryName)
	if err != nil {
		return routerReplacementCandidate{kind: routerReplacementIndeterminate}
	}
	if live == nil || live.Disabled {
		return routerReplacementCandidate{kind: routerReplacementNotCandidate}
	}
	entryLanguage, entryPort, ok := lspRouterURLLanguagePort(entryLSPRouterURL(live))
	if !ok || entryPort <= 0 || !strings.EqualFold(entryLanguage, language) {
		return routerReplacementCandidate{kind: routerReplacementNotCandidate}
	}
	owned, _ := entryIsOwnedLSPRouterForLanguage(entryName, live, language, 0)
	if !owned {
		return routerReplacementCandidate{kind: routerReplacementNotCandidate}
	}
	return routerReplacementCandidate{
		kind:      routerReplacementStructural,
		entryPort: entryPort,
	}
}

const (
	managedRouterProbeTimeout   = 500 * time.Millisecond
	managedRouterRouteBodyMax   = 256 * 1024
	managedRouterRequestIDBytes = 16
)

// ManagedRouterLease is the opaque retained process-generation authority used
// by destructive direct-LSP cleanup. The API can revalidate or close it, but
// cannot inspect or reproduce GUI process identity policy.
type ManagedRouterLease interface {
	Revalidate(context.Context) string
	Close() error
}

// ManagedRouterAuthorizer is the injected managed-GUI lease factory used by
// destructive direct-LSP cleanup. The only production implementation lives in
// internal/gui. A nil authorizer is the fail-closed zero value.
type ManagedRouterAuthorizer func(context.Context, int) ManagedRouterAuthorization

// ManagedRouterAuthorization deliberately exposes only an opaque retained
// lease or a stable, sanitized refusal discriminator. Process paths, argv,
// pidport contents, and raw HTTP bodies never cross this boundary.
type ManagedRouterAuthorization struct {
	Lease        ManagedRouterLease
	FailureClass string
}

const (
	ManagedRouterFailurePortInvalid              = "port-invalid"
	ManagedRouterFailurePIDPortUnavailable       = "pidport-unavailable"
	ManagedRouterFailurePIDPortPortMismatch      = "pidport-port-mismatch"
	ManagedRouterFailureSocketOwnerUnavailable   = "socket-owner-unavailable"
	ManagedRouterFailureSocketOwnerMismatch      = "socket-owner-mismatch"
	ManagedRouterFailureProcessUnavailable       = "process-unavailable"
	ManagedRouterFailureExecutableUnavailable    = "executable-unavailable"
	ManagedRouterFailureExecutableMismatch       = "executable-mismatch"
	ManagedRouterFailureArgvRoleMismatch         = "argv-role-mismatch"
	ManagedRouterFailureProcessGenerationInvalid = "process-generation-invalid"
	ManagedRouterFailureProcessOwnerMismatch     = "process-owner-mismatch"
	ManagedRouterFailureProcessOwnerUnavailable  = "process-owner-unavailable"
	ManagedRouterFailureVersionUninformative     = "version-uninformative"
	ManagedRouterFailurePingTransport            = "ping-transport"
	ManagedRouterFailurePingHTTPStatus           = "ping-http-status"
	ManagedRouterFailurePingContentType          = "ping-content-type"
	ManagedRouterFailurePingResponseTooLarge     = "ping-response-too-large"
	ManagedRouterFailurePingMalformed            = "ping-malformed"
	ManagedRouterFailurePingIdentityMismatch     = "ping-identity-mismatch"
	ManagedRouterFailureIdentityChanged          = "identity-changed"
	ManagedRouterFailureIdentityNotSupplied      = "identity-not-supplied"
	ManagedRouterFailureIdentityUnavailable      = "identity-unavailable"
)

type clientLanguageKey struct {
	Client   string
	Language string
}

type clientWriteReceipt struct {
	Key       clientLanguageKey
	EntryName string
}

type registeredLanguageResult struct {
	Entry    WorkspaceEntry
	Receipts []clientWriteReceipt
}

// RegisterOpts controls a Register invocation.
type RegisterOpts struct {
	// WeeklyRefreshExplicit selects between two interpretation modes:
	//   - true:  use WeeklyRefresh literally (caller has decided).
	//   - false: ignore WeeklyRefresh; read daemons.weekly_refresh_default
	//            from settings and use that. Memo D1 (opt-in knob).
	// CLI surface: --weekly-refresh / --no-weekly-refresh both flip this
	// to true; absent flag leaves it false (knob path).
	WeeklyRefreshExplicit bool

	// WeeklyRefresh is the value persisted on each created entry when
	// WeeklyRefreshExplicit==true. Ignored otherwise.
	WeeklyRefresh bool

	// SupervisedProxy routes workspace LSP proxies through supervisor-intent
	// instead of the legacy per-language Windows Scheduled Task path. The
	// supervisor path makes the proxy process a Job-protected child of the
	// hub supervisor, matching Serena's daemon ownership model.
	SupervisedProxy bool

	// ManagedRouterAuthorizer verifies that a structurally observed router port
	// belongs to the current managed GUI process. The configured GUI port and
	// caller-owned identity tuples are intentionally not accepted as authority.
	ManagedRouterAuthorizer ManagedRouterAuthorizer

	// probeManagedLanguageRoute is the per-call route-proof seam. Production
	// leaves it nil and uses ProbeManagedLanguageRoute; focused tests inject an
	// immutable closure for only this Register invocation. It is deliberately
	// unexported so callers cannot replace the route policy across the process.
	probeManagedLanguageRoute func(context.Context, int, string, string) managedRouteProof

	// supervisorPostWriteDeps is normalized once per supervised Register call
	// and carries the four existing post-AddEntry owners into the private
	// supervised transaction. It is unexported, non-persisted, and never shared
	// between calls; tests may replace only their invocation's dependencies.
	supervisorPostWriteDeps supervisorPostWriteDeps

	Writer io.Writer // progress output; nil = os.Stderr
}

// resolveWeeklyRefresh picks the effective WeeklyRefresh value for a
// new entry per memo D1: explicit caller override beats the persisted
// knob; absent explicit override, read daemons.weekly_refresh_default
// from settings (default "false").
//
// Error/empty handling: if SettingsGet returns an error (settings file
// absent on first install, or unreadable due to corruption), the
// fallback is false (the opt-out default). A corrupt-YAML error is
// treated the same as "key absent" and is not surfaced here; the
// caller may surface it separately if diagnosability is required.
func resolveWeeklyRefresh(a *API, opts RegisterOpts) bool {
	if opts.WeeklyRefreshExplicit {
		return opts.WeeklyRefresh
	}
	v, err := a.SettingsGet("daemons.weekly_refresh_default")
	if err != nil || v == "" {
		return false
	}
	return v == "true"
}

// RegisterReport summarizes what Register actually created.
type RegisterReport struct {
	Workspace    string           `json:"workspace"`
	WorkspaceKey string           `json:"workspace_key"`
	Entries      []WorkspaceEntry `json:"entries"`
	Warnings     []string         `json:"warnings,omitempty"`
}

// UnregisterReport summarizes what Unregister actually removed.
type UnregisterReport struct {
	Workspace    string   `json:"workspace"`
	WorkspaceKey string   `json:"workspace_key"`
	Removed      []string `json:"removed"` // language names
	Warnings     []string `json:"warnings,omitempty"`
}

// Register ensures workspace-scoped lazy proxies exist for each requested
// language in workspacePath. An empty languages slice defaults to every
// language declared in the manifest.
//
// Lazy mode: this function DOES NOT preflight LSP binaries. Missing LSP
// binaries are surfaced later at first tools/call via LifecycleMissing.
//
// Side effects per language (rolled back on later failure):
//  1. Allocate port from registry (first-free in the manifest's pool).
//  2. Create scheduler task whose command is
//     `mcphub daemon workspace-proxy --port <p> --workspace <ws> --language <lang>`.
//  3. Write managed entries into each client config (the registry-derived
//     defaults claude-code and codex-cli, or whatever the manifest declares in
//     client_bindings). Cursor requires an explicit binding.
//
// Registry is saved once at the end; a mid-loop failure leaves the registry
// untouched on disk.
func (a *API) Register(workspacePath string, languages []string, opts RegisterOpts) (*RegisterReport, error) {
	data, err := loadManifestYAMLEmbedFirst("mcp-language-server")
	if err != nil {
		return nil, fmt.Errorf("load manifest mcp-language-server: %w", err)
	}
	m, err := parseManifestForName("mcp-language-server", data)
	if err != nil {
		return nil, err
	}
	return a.registerWithManifest(m, workspacePath, languages, opts)
}

// registerWithManifest is the test-seam variant: production callers pass
// through Register (which loads the embedded manifest); tests inject a
// synthetic manifest to exercise rollback and edge cases hermetically.
func (a *API) registerWithManifest(m *config.ServerManifest, workspacePath string, languages []string, opts RegisterOpts) (*RegisterReport, error) {
	if m.Kind != config.KindWorkspaceScoped {
		return nil, fmt.Errorf("manifest %s: not workspace-scoped", m.Name)
	}
	// D-3 availability admission gate (shared single owner). A watch /
	// disabled-until-probe LSP manifest whose install-probe has not passed must
	// NOT create scheduler tasks (EnsureWeeklyRefreshTask + per-language tasks) or
	// write client entries. Run it at the START — before any side effect — so an
	// inert manifest is refused with the same typed availability error Install /
	// Preflight returns, not after a partial registration. ADDITIVE: the shipped
	// mcp-language-server manifest carries no availability/install_probe, so this
	// returns nil immediately and the path is byte-identical to before.
	if err := AvailabilityAdmission(m); err != nil {
		return nil, err
	}
	w := opts.Writer
	if w == nil {
		w = os.Stderr
	}
	// 0. Canonical workspace + key.
	canonical, err := CanonicalWorkspacePath(workspacePath)
	if err != nil {
		return nil, err
	}
	wsKey := WorkspaceKey(canonical)
	// 1. Default-all when caller passed an empty slice. Sort for
	// deterministic iteration order — tests and rollback ordering both
	// depend on it.
	if len(languages) == 0 {
		for _, l := range m.Languages {
			languages = append(languages, l.Name)
		}
		sort.Strings(languages)
	}
	// 2. Validate every requested language is declared in the manifest
	// BEFORE any side effect. Unknown language → manifest-integrity error.
	bySpec := map[string]config.LanguageSpec{}
	for _, l := range m.Languages {
		bySpec[l.Name] = l
	}
	for _, lang := range languages {
		if _, ok := bySpec[lang]; !ok {
			return nil, fmt.Errorf("unknown language %q (manifest %s supports: %v)",
				lang, m.Name, sortedLanguageNames(m))
		}
	}
	// 2.4 Preflight the canonical mcphub binary BEFORE any scheduler
	// side effect. Register's per-language loop does the same check
	// inside registerOneLanguage, but EnsureWeeklyRefreshTask (below)
	// would fire first and could leak a shared "mcp-local-hub-workspace-
	// weekly-refresh" task pointing at a missing binary if setup wasn't
	// run yet. Fail fast instead — the user sees the same "run mcphub
	// setup once" message install uses, no orphan shared state created.
	canonicalExeForPreflight, err := canonicalMcphubPath()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(canonicalExeForPreflight); err != nil {
		return nil, fmt.Errorf("%s not present — run `mcphub setup` once: %w", canonicalExeForPreflight, err)
	}
	// 3. Per-language register with rollback stack.
	//
	// Registry flock lifetime is scoped to `registerOneLanguage`'s Phase 1
	// (port alloc + task create + registry write) and is explicitly
	// released BEFORE `sch.Run` triggers the proxy subprocess. Holding it
	// across sch.Run + readiness probe deadlocks the proxy: the proxy's
	// own `reg.Lock()` in `daemon_workspace.go` would block until the
	// readiness probe times out, then register's rollback would delete
	// the registry entry, and the unblocked proxy would exit with
	// "not registered". Rollback closures that touch the registry each
	// re-acquire the lock themselves.
	regPath, err := registryPathForRegister()
	if err != nil {
		return nil, err
	}
	reg := NewRegistry(regPath)
	sch, err := schedulerNewForRegister()
	var schedulerUnavailableErr error
	if err != nil {
		if schedulerUnavailableError(err) {
			opts.SupervisedProxy = true
			schedulerUnavailableErr = err
			sch = legacyTaskUnavailableScheduler{}
		} else {
			return nil, err
		}
	}
	if opts.SupervisedProxy {
		opts.supervisorPostWriteDeps = normalizeSupervisorPostWriteDeps(a, opts.supervisorPostWriteDeps)
	}
	// 3.1 Ensure the shared weekly-refresh task exists only for the legacy
	// scheduler-backed path. Supervised registration owns its lifecycle
	// through the supervisor intent file; touching the shared scheduler task
	// there would mutate durable state before per-language preconditions
	// such as legacy-port kill/adoption have been proven.
	if !opts.SupervisedProxy {
		if err := a.EnsureWeeklyRefreshTask(); err != nil {
			fmt.Fprintf(w, "warning: ensure shared weekly-refresh task: %v\n", err)
		}
	}
	allClients := clientsAllForRegister()
	transaction := newRegistrationTransaction()
	report := &RegisterReport{Workspace: canonical, WorkspaceKey: wsKey}
	if schedulerUnavailableErr != nil {
		report.Warnings = append(report.Warnings,
			fmt.Sprintf("scheduler unavailable (%v); using supervised LSP proxy path and skipping legacy task handling", schedulerUnavailableErr))
		if err := preflightSchedulerlessRegisterSupervisor(); err != nil {
			return report, err
		}
	}
	// ONE client-scope decision for the WHOLE registration (see
	// effectiveClientBindings' RESOLVE ONCE PER REGISTER note). Resolved here —
	// before the first language, before any client write, before the cleanup
	// gate — and threaded down, so no consumer can observe a different answer
	// than another because the operator changed Settings → Clients mid-loop.
	bindings, droppedRelayStdio := effectiveClientBindings(m)
	if warning := relayStdioBindingWarning(bindings, droppedRelayStdio); warning != "" {
		fmt.Fprintf(w, "warning: %s\n", warning)
		report.Warnings = append(report.Warnings, warning)
	}
	var committedReceipts []clientWriteReceipt
	for _, lang := range languages {
		result, err := a.registerOneLanguage(m, canonical, wsKey, lang, opts, reg, sch, allClients, bindings, w, transaction)
		if err != nil {
			outcome := transaction.Fail(fmt.Errorf("register language %s: %w", lang, err))
			return report, outcome.Err
		}
		report.Entries = append(report.Entries, result.Entry)
		committedReceipts = append(committedReceipts, result.Receipts...)
	}
	// Cleanup runs only after every language reached its final success point.
	// The selected bindings remain intent; committedReceipts are the only proof
	// of writes performed by this invocation.
	cleanupWarnings, cleanupErr := a.cleanupDirectLanguageServerEntriesAfterRegister(
		bySpec, languages, canonical, allClients, selectedClientLanguageKeys(bindings, languages), committedReceipts,
		opts.ManagedRouterAuthorizer, opts.probeManagedLanguageRoute, w, transaction)
	report.Warnings = append(report.Warnings, cleanupWarnings...)
	if cleanupErr != nil {
		return report, transaction.Fail(cleanupErr).Err
	}
	outcome := transaction.Commit()
	if !outcome.Committed() {
		return report, outcome.Err
	}
	if outcome.ObserverErr != nil {
		fmt.Fprintf(w, "warning: registration committed but a post-commit observer failed: %v\n", outcome.ObserverErr)
		report.Warnings = append(
			report.Warnings,
			"register-post-commit-observer-failed: registration succeeded, but a completion record could not be written",
		)
	}
	// EXPLICIT register → bless this workspace's canonical root as a
	// trusted root for the GUI LSP router's first-touch auto-register
	// gate. This is the operator-action seed: after an explicit register
	// of a workspace, sibling/child workspaces under the same tree may
	// auto-register through the router without re-registering. The
	// router's own auto-register path does NOT call this (it goes through
	// EnsureLSPRegistered, not Register), so an untrusted tool-call path
	// can never bless itself. Best-effort: a bless failure only warns —
	// the register itself succeeded; the worst case is a sibling needing
	// its own explicit register. See internal/api/lsp_trusted_roots.go.
	if len(report.Entries) > 0 {
		if err := registerBlessTrustedRootFn(canonical); err != nil {
			fmt.Fprintf(w, "warning: could not record %s as an LSP trusted root (router auto-register of sibling workspaces under this tree may require explicit register): %v\n", canonical, err)
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("could not record %s as an LSP trusted root: %v", canonical, err))
		}
	}
	return report, nil
}

// registerBlessTrustedRootFn is the explicit-register bless seam. In
// production it blesses the workspace's canonical root in the default
// trusted-roots store; tests override it (newRegisterHarness defaults it
// to a no-op so register tests never write the real %LOCALAPPDATA%
// store). The ROUTER auto-register path does NOT use this seam — it goes
// through EnsureLSPRegistered, which never blesses.
var registerBlessTrustedRootFn = func(canonicalWorkspaceRoot string) error {
	return BlessDefaultTrustedRoot(canonicalWorkspaceRoot)
}

func preflightSchedulerlessRegisterSupervisor() error {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultReconcileTimeout)
	defer cancel()
	if _, err := registerSupervisorReconcileFn(ctx, false); err != nil {
		return fmt.Errorf("schedulerless LSP register requires a running supervisor before writing intent: %w; run `mcphub supervise` from another shell and retry", err)
	}
	return nil
}

type directCleanupMatchResult struct {
	matches      []clients.LanguageServerStdioEntry
	failureClass directCleanupWarningClass
	complete     bool
}

type cleanupEvidence string

const (
	cleanupEvidenceNone          cleanupEvidence = "none"
	cleanupEvidenceReceipt       cleanupEvidence = "receipt"
	cleanupEvidenceManagedRouter cleanupEvidence = "managed-router-and-route"
)

type directCleanupPlan struct {
	key                clientLanguageKey
	client             registerClient
	selected           bool
	receiptEntry       string
	backend            string
	observedRouterPort int
	matches            []clients.LanguageServerStdioEntry
	evidence           cleanupEvidence
	failureClass       string
}

type managedRouteProof struct {
	OK           bool
	FailureClass string
}

type directCleanupWarningClass string

const (
	cleanupWarningWriteEntryNameEmpty      directCleanupWarningClass = "write-entry-name-empty"
	cleanupWarningWriteReceiptConflict     directCleanupWarningClass = "write-receipt-conflict"
	cleanupWarningWriteReceiptMissing      directCleanupWarningClass = "write-receipt-missing"
	cleanupWarningClientEntryIndeterminate directCleanupWarningClass = "client-entry-indeterminate"
	cleanupWarningDirectScanFailed         directCleanupWarningClass = "direct-scan-failed"
	cleanupWarningSurvivorScanFailed       directCleanupWarningClass = "survivor-scan-failed"
	cleanupWarningBackupFailed             directCleanupWarningClass = "backup-failed"
	cleanupWarningRemoveFailed             directCleanupWarningClass = "remove-failed"
)

type directCleanupWarningRecord struct {
	class    directCleanupWarningClass
	client   string
	language string
	entry    string
}

type directCleanupPlanDeps struct {
	authorizeRouter ManagedRouterAuthorizer
	probeRoute      func(context.Context, int, string, string) managedRouteProof
	matchDirect     func(registerClient, string, map[string]bool, string) directCleanupMatchResult
}

type managedRouteHTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type managedRouteProbeDeps struct {
	do      managedRouteHTTPDoer
	entropy io.Reader
	catalog func(string) (ToolCatalog, bool)
}

func receiptEntryNames(receipts []clientWriteReceipt) (map[clientLanguageKey]string, *directCleanupWarningRecord) {
	out := make(map[clientLanguageKey]string, len(receipts))
	for _, receipt := range receipts {
		name := strings.TrimSpace(receipt.EntryName)
		if name == "" {
			return nil, &directCleanupWarningRecord{
				class: cleanupWarningWriteEntryNameEmpty, client: receipt.Key.Client, language: receipt.Key.Language,
			}
		}
		if prior, exists := out[receipt.Key]; exists && prior != name {
			return nil, &directCleanupWarningRecord{
				class: cleanupWarningWriteReceiptConflict, client: receipt.Key.Client, language: receipt.Key.Language,
			}
		}
		out[receipt.Key] = name
	}
	return out, nil
}

func cleanupAliasesForLanguage(language string, spec config.LanguageSpec) map[string]bool {
	aliases := map[string]bool{}
	addLSPCleanupAlias(aliases, language)
	addLSPCleanupAlias(aliases, spec.LspCommand)
	return aliases
}

func buildDirectCleanupPlans(
	bySpec map[string]config.LanguageSpec,
	languages []string,
	canonicalWorkspace string,
	allClients map[string]registerClient,
	selected map[clientLanguageKey]bool,
	receipts map[clientLanguageKey]string,
	deps directCleanupPlanDeps,
) ([]directCleanupPlan, []directCleanupWarningRecord) {
	var plans []directCleanupPlan
	var warningRecords []directCleanupWarningRecord
	clientNames := make([]string, 0, len(allClients))
	for clientName := range allClients {
		clientNames = append(clientNames, clientName)
	}
	sort.Strings(clientNames)

	for _, clientName := range clientNames {
		client := allClients[clientName]
		if client == nil || !client.Exists() {
			continue
		}
		clientStart := len(plans)
		clientWideFailure := ""
		for _, language := range languages {
			spec, ok := bySpec[language]
			if !ok {
				continue
			}
			key := clientLanguageKey{Client: clientName, Language: language}
			plan := directCleanupPlan{
				key:          key,
				client:       client,
				selected:     selected[key],
				receiptEntry: receipts[key],
				backend:      spec.Backend,
				evidence:     cleanupEvidenceNone,
			}

			candidate := routerReplacementCandidate{kind: routerReplacementNotCandidate}
			if plan.receiptEntry == "" {
				// Router inspection is staged. A failure is observable only if the
				// same plan has a real direct candidate; otherwise it describes no
				// state that cleanup could have mutated.
				candidate = inspectClientLSPRouterReplacement(client, language)
			}

			if plan.receiptEntry == "" && candidate.kind == routerReplacementNotCandidate && !plan.selected {
				plans = append(plans, plan)
				continue
			}
			result := deps.matchDirect(client, clientName, cleanupAliasesForLanguage(language, spec), canonicalWorkspace)
			if !result.complete {
				failureClass := result.failureClass
				if failureClass == "" {
					failureClass = cleanupWarningDirectScanFailed
				}
				clientWideFailure = string(failureClass)
				plan.failureClass = clientWideFailure
				warningRecords = append(warningRecords, directCleanupWarningRecord{
					class: failureClass, client: clientName,
				})
			} else {
				plan.matches = result.matches
				if len(plan.matches) > 0 {
					switch candidate.kind {
					case routerReplacementIndeterminate:
						plan.failureClass = string(cleanupWarningClientEntryIndeterminate)
						warningRecords = append(warningRecords, directCleanupWarningRecord{
							class: cleanupWarningClientEntryIndeterminate, client: clientName, language: language,
						})
					case routerReplacementStructural:
						plan.observedRouterPort = candidate.entryPort
					}
				}
			}
			plans = append(plans, plan)
		}
		if clientWideFailure != "" {
			for i := clientStart; i < len(plans); i++ {
				plans[i].failureClass = clientWideFailure
			}
		}
	}
	return plans, warningRecords
}

// ProbeManagedLanguageRoute proves that the exact production language route
// serves the complete expected backend tool catalog. It is exported only
// within the repository's internal tree so the real GUI mux can exercise this
// production contract without creating an api -> gui dependency.
func ProbeManagedLanguageRoute(ctx context.Context, candidatePort int, language, backend string) (bool, string) {
	proof := probeManagedLanguageRoute(ctx, candidatePort, language, backend)
	return proof.OK, proof.FailureClass
}

func probeManagedLanguageRoute(ctx context.Context, candidatePort int, language, backend string) managedRouteProof {
	client := &http.Client{
		Timeout:       managedRouterProbeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return probeManagedLanguageRouteWithDeps(ctx, candidatePort, language, backend, managedRouteProbeDeps{
		do:      client,
		entropy: rand.Reader,
		catalog: ToolCatalogForBackend,
	})
}

func probeManagedLanguageRouteWithDeps(ctx context.Context, candidatePort int, language, backend string, deps managedRouteProbeDeps) managedRouteProof {
	if candidatePort <= 0 || candidatePort > 65535 {
		return managedRouteProof{FailureClass: "route-transport"}
	}
	if deps.do == nil || deps.entropy == nil || deps.catalog == nil {
		return managedRouteProof{FailureClass: "route-transport"}
	}
	catalog, ok := deps.catalog(backend)
	if !ok || len(catalog.Tools) == 0 {
		return managedRouteProof{FailureClass: "route-catalog-mismatch"}
	}
	requestIDBytes := make([]byte, managedRouterRequestIDBytes)
	if _, err := io.ReadFull(deps.entropy, requestIDBytes); err != nil {
		return managedRouteProof{FailureClass: "route-transport"}
	}
	requestID := hex.EncodeToString(requestIDBytes)
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      requestID,
		"method":  "tools/list",
		"params":  map[string]any{},
	})
	if err != nil {
		return managedRouteProof{FailureClass: "route-malformed"}
	}

	requestCtx, cancel := context.WithTimeout(ctx, managedRouterProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(
		requestCtx,
		http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/lsp/%s/mcp", candidatePort, url.PathEscape(language)),
		bytes.NewReader(body),
	)
	if err != nil {
		return managedRouteProof{FailureClass: "route-transport"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := deps.do.Do(req)
	if err != nil {
		return managedRouteProof{FailureClass: "route-transport"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return managedRouteProof{FailureClass: "route-http-status"}
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		return managedRouteProof{FailureClass: "route-content-type"}
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, managedRouterRouteBodyMax+1))
	if err != nil {
		return managedRouteProof{FailureClass: "route-transport"}
	}
	if len(responseBody) > managedRouterRouteBodyMax {
		return managedRouteProof{FailureClass: "route-response-too-large"}
	}
	var payload struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	if err := decoder.Decode(&payload); err != nil {
		return managedRouteProof{FailureClass: "route-malformed"}
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return managedRouteProof{FailureClass: "route-malformed"}
	}
	if payload.JSONRPC != "2.0" {
		return managedRouteProof{FailureClass: "route-malformed"}
	}
	var responseID string
	if err := json.Unmarshal(payload.ID, &responseID); err != nil || responseID != requestID {
		return managedRouteProof{FailureClass: "route-id-mismatch"}
	}
	if len(bytes.TrimSpace(payload.Error)) > 0 && string(bytes.TrimSpace(payload.Error)) != "null" {
		return managedRouteProof{FailureClass: "route-jsonrpc-error"}
	}
	if len(payload.Result.Tools) == 0 {
		return managedRouteProof{FailureClass: "route-tools-invalid"}
	}
	want := make(map[string]bool, len(catalog.Tools))
	for _, tool := range catalog.Tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" || want[name] {
			return managedRouteProof{FailureClass: "route-catalog-mismatch"}
		}
		want[name] = true
	}
	got := make(map[string]bool, len(payload.Result.Tools))
	for _, tool := range payload.Result.Tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" || got[name] || !want[name] {
			return managedRouteProof{FailureClass: "route-tools-invalid"}
		}
		got[name] = true
	}
	if len(got) != len(want) {
		return managedRouteProof{FailureClass: "route-catalog-mismatch"}
	}
	return managedRouteProof{OK: true}
}

func directLanguageServerCleanupMatches(
	client registerClient,
	_ string,
	aliases map[string]bool,
	canonicalWorkspace string,
) directCleanupMatchResult {
	entries, err := findStdioLanguageServerCandidatesForDirectCleanup(client)
	if err != nil {
		return directCleanupMatchResult{
			failureClass: cleanupWarningDirectScanFailed,
			complete:     false,
		}
	}
	matches, err := matchingDirectLanguageServerEntries(client, entries, aliases, canonicalWorkspace)
	if err != nil {
		return directCleanupMatchResult{
			failureClass: cleanupWarningSurvivorScanFailed,
			complete:     false,
		}
	}

	if aliases["go"] || aliases["gopls"] {
		stdioEntries, err := removableStdioEntriesForDirectCleanup(client)
		if err != nil {
			return directCleanupMatchResult{
				failureClass: cleanupWarningDirectScanFailed,
				complete:     false,
			}
		}
		goplsMatches, err := matchingDirectGoplsMCPEntries(client, stdioEntries, aliases, canonicalWorkspace)
		if err != nil {
			return directCleanupMatchResult{
				failureClass: cleanupWarningSurvivorScanFailed,
				complete:     false,
			}
		}
		matches = append(matches, goplsMatches...)
	}
	return directCleanupMatchResult{
		matches:  dedupeDirectLanguageServerEntries(matches),
		complete: true,
	}
}

type directCleanupWarningAccumulator struct {
	writer     io.Writer
	seen       map[string]bool
	proofPlans map[directCleanupWarningClass]map[directCleanupPlanIdentity]struct{}
	warnings   []string
}

type directCleanupPlanIdentity struct {
	client   string
	language string
	port     int
}

func newDirectCleanupWarningAccumulator(writer io.Writer) *directCleanupWarningAccumulator {
	return &directCleanupWarningAccumulator{
		writer:     writer,
		seen:       map[string]bool{},
		proofPlans: map[directCleanupWarningClass]map[directCleanupPlanIdentity]struct{}{},
	}
}

func (a *directCleanupWarningAccumulator) addRecord(record directCleanupWarningRecord) {
	warning := renderDirectCleanupWarning(record)
	if warning == "" || a.seen[warning] {
		return
	}
	a.seen[warning] = true
	a.warnings = append(a.warnings, warning)
}

func (a *directCleanupWarningAccumulator) addProofFailure(failureClass directCleanupWarningClass, plan directCleanupPlan) {
	if failureClass == "" {
		return
	}
	if a.proofPlans[failureClass] == nil {
		a.proofPlans[failureClass] = map[directCleanupPlanIdentity]struct{}{}
	}
	a.proofPlans[failureClass][directCleanupPlanIdentity{
		client:   sanitizeCleanupPlanIdentifier(plan.key.Client),
		language: sanitizeCleanupPlanIdentifier(plan.key.Language),
		port:     plan.observedRouterPort,
	}] = struct{}{}
}

func (a *directCleanupWarningAccumulator) flushProofWarnings() {
	classes := make([]directCleanupWarningClass, 0, len(a.proofPlans))
	for failureClass := range a.proofPlans {
		classes = append(classes, failureClass)
	}
	sort.Slice(classes, func(i, j int) bool { return classes[i] < classes[j] })
	for _, failureClass := range classes {
		plans := make([]directCleanupPlanIdentity, 0, len(a.proofPlans[failureClass]))
		for plan := range a.proofPlans[failureClass] {
			plans = append(plans, plan)
		}
		sort.Slice(plans, func(i, j int) bool {
			if plans[i].client != plans[j].client {
				return plans[i].client < plans[j].client
			}
			if plans[i].language != plans[j].language {
				return plans[i].language < plans[j].language
			}
			return plans[i].port < plans[j].port
		})
		parts := make([]string, 0, len(plans))
		for _, plan := range plans {
			port := plan.port
			if port < 0 || port > 65535 {
				port = 0
			}
			parts = append(parts, fmt.Sprintf("client=%s,language=%s,port=%d", plan.client, plan.language, port))
		}
		warning := fmt.Sprintf("%s: affected_plans=%d [%s]; keeping matching direct LSP entries", failureClass, len(plans), strings.Join(parts, "; "))
		if failureClass == directCleanupWarningClass(ManagedRouterFailureProcessOwnerUnavailable) {
			warning += "; retry after verifying the managed GUI is running as the current user"
		}
		a.warnings = append(a.warnings, warning)
		fmt.Fprintf(a.writer, "warning: %s\n", warning)
	}
}

func renderDirectCleanupWarning(record directCleanupWarningRecord) string {
	client := sanitizeCleanupPlanIdentifier(record.client)
	language := sanitizeCleanupPlanIdentifier(record.language)
	entry := sanitizeCleanupPlanIdentifier(record.entry)
	switch record.class {
	case cleanupWarningWriteEntryNameEmpty:
		return fmt.Sprintf("%s: client=%s,language=%s; direct LSP cleanup skipped and matching entries were kept; retry registration", record.class, client, language)
	case cleanupWarningWriteReceiptConflict:
		return fmt.Sprintf("%s: client=%s,language=%s; direct LSP cleanup skipped and matching entries were kept; retry registration", record.class, client, language)
	case cleanupWarningClientEntryIndeterminate:
		return fmt.Sprintf("%s: client=%s,language=%s; direct LSP cleanup skipped and matching entries were kept; verify client configuration is readable and retry", record.class, client, language)
	case cleanupWarningDirectScanFailed, cleanupWarningSurvivorScanFailed:
		return fmt.Sprintf("%s: client=%s; direct LSP cleanup skipped and matching entries were kept; verify client configuration is readable and retry", record.class, client)
	case cleanupWarningBackupFailed:
		return fmt.Sprintf("%s: client=%s; direct LSP cleanup skipped and matching entries were kept; verify client-config write permissions and retry", record.class, client)
	case cleanupWarningRemoveFailed:
		return fmt.Sprintf("%s: client=%s,language=%s,entry=%s; backup completed but entry removal failed; use the printed backup path if recovery is needed, then retry", record.class, client, language, entry)
	default:
		return ""
	}
}

func normalizeManagedRouterFailureClass(value string, fallback directCleanupWarningClass) directCleanupWarningClass {
	switch value {
	case ManagedRouterFailurePortInvalid,
		ManagedRouterFailurePIDPortUnavailable,
		ManagedRouterFailurePIDPortPortMismatch,
		ManagedRouterFailureSocketOwnerUnavailable,
		ManagedRouterFailureSocketOwnerMismatch,
		ManagedRouterFailureProcessUnavailable,
		ManagedRouterFailureExecutableUnavailable,
		ManagedRouterFailureExecutableMismatch,
		ManagedRouterFailureArgvRoleMismatch,
		ManagedRouterFailureProcessGenerationInvalid,
		ManagedRouterFailureProcessOwnerMismatch,
		ManagedRouterFailureProcessOwnerUnavailable,
		ManagedRouterFailureVersionUninformative,
		ManagedRouterFailurePingTransport,
		ManagedRouterFailurePingHTTPStatus,
		ManagedRouterFailurePingContentType,
		ManagedRouterFailurePingResponseTooLarge,
		ManagedRouterFailurePingMalformed,
		ManagedRouterFailurePingIdentityMismatch,
		ManagedRouterFailureIdentityChanged,
		ManagedRouterFailureIdentityNotSupplied,
		ManagedRouterFailureIdentityUnavailable:
		return directCleanupWarningClass(value)
	default:
		return fallback
	}
}

func normalizeManagedRouteFailureClass(value string) directCleanupWarningClass {
	switch value {
	case "route-transport",
		"route-catalog-mismatch",
		"route-http-status",
		"route-content-type",
		"route-response-too-large",
		"route-malformed",
		"route-id-mismatch",
		"route-jsonrpc-error",
		"route-tools-invalid":
		return directCleanupWarningClass(value)
	default:
		return "route-transport"
	}
}

func sanitizeCleanupPlanIdentifier(value string) string {
	const (
		maxIdentifierLen = 64
		maxPrefixLen     = 24
	)
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	valid := len(value) <= maxIdentifierLen
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		valid = false
		break
	}
	if valid {
		return value
	}
	var prefix strings.Builder
	for _, r := range value {
		if prefix.Len() >= maxPrefixLen {
			break
		}
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			prefix.WriteRune(r)
		} else if prefix.Len() > 0 && !strings.HasSuffix(prefix.String(), "-") {
			prefix.WriteByte('-')
		}
	}
	cleanPrefix := strings.Trim(prefix.String(), "-._")
	if cleanPrefix == "" {
		cleanPrefix = "value"
	}
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("sanitized-%s-%x", cleanPrefix, sum[:6])
}

func (a *API) cleanupDirectLanguageServerEntriesAfterRegister(
	bySpec map[string]config.LanguageSpec,
	languages []string,
	canonicalWorkspace string,
	allClients map[string]registerClient,
	selected map[clientLanguageKey]bool,
	receipts []clientWriteReceipt,
	authorizeRouter ManagedRouterAuthorizer,
	probeRoute func(context.Context, int, string, string) managedRouteProof,
	w io.Writer,
	transaction *registrationTransaction,
) ([]string, error) {
	if probeRoute == nil {
		probeRoute = probeManagedLanguageRoute
	}
	return a.cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDepsTransaction(
		bySpec,
		languages,
		canonicalWorkspace,
		allClients,
		selected,
		receipts,
		w,
		directCleanupPlanDeps{
			authorizeRouter: authorizeRouter,
			probeRoute:      probeRoute,
			matchDirect:     directLanguageServerCleanupMatches,
		},
		transaction,
	)
}

func (a *API) cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDeps(
	bySpec map[string]config.LanguageSpec,
	languages []string,
	canonicalWorkspace string,
	allClients map[string]registerClient,
	selected map[clientLanguageKey]bool,
	receipts []clientWriteReceipt,
	w io.Writer,
	deps directCleanupPlanDeps,
) []string {
	transaction := newRegistrationTransaction()
	warnings, err := a.cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDepsTransaction(
		bySpec, languages, canonicalWorkspace, allClients, selected, receipts, w, deps, transaction,
	)
	if err == nil {
		if outcome := transaction.Commit(); !outcome.Committed() {
			err = outcome.Err
		}
	} else {
		err = transaction.Fail(err).Err
	}
	if err != nil {
		warnings = append(warnings, "direct-cleanup-failed: direct entries were preserved because transactional cleanup did not settle")
	}
	return warnings
}

func (a *API) cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDepsTransaction(
	bySpec map[string]config.LanguageSpec,
	languages []string,
	canonicalWorkspace string,
	allClients map[string]registerClient,
	selected map[clientLanguageKey]bool,
	receipts []clientWriteReceipt,
	w io.Writer,
	deps directCleanupPlanDeps,
	transaction *registrationTransaction,
) ([]string, error) {
	if transaction == nil {
		return nil, errors.New("direct cleanup requires a registration transaction")
	}
	keepN := a.EffectiveBackupKeepN()
	warnings := newDirectCleanupWarningAccumulator(w)
	receiptNames, receiptWarning := receiptEntryNames(receipts)
	if receiptWarning != nil {
		warnings.addRecord(*receiptWarning)
		return warnings.warnings, nil
	}
	plans, warningRecords := buildDirectCleanupPlans(
		bySpec,
		languages,
		canonicalWorkspace,
		allClients,
		selected,
		receiptNames,
		deps,
	)
	for _, record := range warningRecords {
		warnings.addRecord(record)
	}

	for i := range plans {
		plan := &plans[i]
		if len(plan.matches) == 0 || plan.failureClass != "" {
			continue
		}
		if plan.receiptEntry != "" {
			plan.evidence = cleanupEvidenceReceipt
			continue
		}
		if plan.observedRouterPort <= 0 {
			if plan.selected {
				class := cleanupWarningWriteReceiptMissing
				plan.failureClass = string(class)
				warnings.addProofFailure(class, *plan)
			}
			continue
		}
		if deps.authorizeRouter == nil {
			class := directCleanupWarningClass(ManagedRouterFailureIdentityNotSupplied)
			plan.failureClass = string(class)
			warnings.addProofFailure(class, *plan)
			continue
		}
		plan.evidence = cleanupEvidenceManagedRouter
	}

	clientNames := make([]string, 0, len(allClients))
	for clientName := range allClients {
		clientNames = append(clientNames, clientName)
	}
	sort.Strings(clientNames)
	for _, clientName := range clientNames {
		clientMark := transaction.Mark()
		var client registerClient
		leases := map[int]ManagedRouterLease{}
		plansByEntry := map[string]*directCleanupPlan{}
		refusedPorts := map[int]directCleanupWarningClass{}
		for i := range plans {
			plan := &plans[i]
			if plan.key.Client != clientName || plan.failureClass != "" || plan.evidence == cleanupEvidenceNone {
				continue
			}
			client = plan.client
			if plan.evidence == cleanupEvidenceManagedRouter {
				lease := leases[plan.observedRouterPort]
				if lease == nil {
					if class, refused := refusedPorts[plan.observedRouterPort]; refused {
						plan.failureClass = string(class)
						warnings.addProofFailure(class, *plan)
						continue
					}
					authorization := deps.authorizeRouter(context.Background(), plan.observedRouterPort)
					if authorization.Lease == nil {
						class := normalizeManagedRouterFailureClass(
							authorization.FailureClass,
							directCleanupWarningClass(ManagedRouterFailureIdentityUnavailable),
						)
						refusedPorts[plan.observedRouterPort] = class
						plan.failureClass = string(class)
						warnings.addProofFailure(class, *plan)
						continue
					}
					lease = authorization.Lease
					leases[plan.observedRouterPort] = lease
					port := plan.observedRouterPort
					transaction.AddFinalizer(
						fmt.Sprintf("close managed-router lease for %s port %d", sanitizeCleanupPlanIdentifier(clientName), port),
						lease.Close,
					)
				}
			}
			for _, match := range plan.matches {
				plansByEntry[match.Name] = plan
			}
		}
		if client == nil {
			continue
		}
		if len(plansByEntry) == 0 {
			if rollbackErr := transaction.RollbackTo(clientMark); rollbackErr != nil {
				return warnings.warnings, fmt.Errorf("direct cleanup %s proof refusal rollback: %w", sanitizeCleanupPlanIdentifier(clientName), rollbackErr)
			}
			continue
		}

		matchesByName := map[string]clients.LanguageServerStdioEntry{}
		languageByName := map[string]string{}
		for i := range plans {
			plan := &plans[i]
			if plan.key.Client != clientName || plan.failureClass != "" || plan.evidence == cleanupEvidenceNone {
				continue
			}
			for _, match := range plan.matches {
				matchesByName[match.Name] = match
				languageByName[match.Name] = plan.key.Language
			}
		}
		invalidatePlan := func(plan *directCleanupPlan, failure string) {
			class := directCleanupWarningClass(failure)
			plan.failureClass = string(class)
			warnings.addProofFailure(class, *plan)
			for _, match := range plan.matches {
				if plansByEntry[match.Name] == plan {
					delete(plansByEntry, match.Name)
					delete(matchesByName, match.Name)
					delete(languageByName, match.Name)
				}
			}
		}
		planHasEntries := func(plan *directCleanupPlan) bool {
			for _, match := range plan.matches {
				if plansByEntry[match.Name] == plan {
					return true
				}
			}
			return false
		}
		// Exact destructive checkpoint: retained generation first, exact route
		// second, immediately before the shared client backup.
		for i := range plans {
			plan := &plans[i]
			if plan.key.Client != clientName ||
				plan.evidence != cleanupEvidenceManagedRouter ||
				!planHasEntries(plan) {
				continue
			}
			if failure := validateManagedCleanupPlan(leases[plan.observedRouterPort], deps.probeRoute, *plan); failure != "" {
				invalidatePlan(plan, failure)
			}
		}
		if len(matchesByName) == 0 {
			if rollbackErr := transaction.RollbackTo(clientMark); rollbackErr != nil {
				return warnings.warnings, fmt.Errorf("direct cleanup %s pre-backup proof rollback: %w", sanitizeCleanupPlanIdentifier(clientName), rollbackErr)
			}
			continue
		}
		backupPath, err := client.BackupKeep(keepN)
		if err != nil {
			if rollbackErr := transaction.RollbackTo(clientMark); rollbackErr != nil {
				return warnings.warnings, fmt.Errorf("direct cleanup %s backup refusal rollback: %w", sanitizeCleanupPlanIdentifier(clientName), rollbackErr)
			}
			warnings.addRecord(directCleanupWarningRecord{class: cleanupWarningBackupFailed, client: clientName})
			continue
		}
		transaction.AddSuccessOutput(
			"cleanup backup "+sanitizeCleanupPlanIdentifier(clientName),
			w,
			"✓ %s backup before direct LSP cleanup: %s\n",
			clientName,
			backupPath,
		)
		// Exact destructive checkpoint immediately after BackupKeep. A refused
		// plan is removed from this client's batch without suppressing sibling
		// plans whose own evidence remains valid.
		for i := range plans {
			plan := &plans[i]
			if plan.key.Client != clientName ||
				plan.evidence != cleanupEvidenceManagedRouter ||
				!planHasEntries(plan) {
				continue
			}
			if failure := validateManagedCleanupPlan(leases[plan.observedRouterPort], deps.probeRoute, *plan); failure != "" {
				invalidatePlan(plan, failure)
			}
		}
		if len(matchesByName) == 0 {
			if rollbackErr := transaction.RollbackTo(clientMark); rollbackErr != nil {
				return warnings.warnings, fmt.Errorf("direct cleanup %s post-backup proof rollback: %w", sanitizeCleanupPlanIdentifier(clientName), rollbackErr)
			}
			continue
		}

		completedManagedPlans := map[int][]directCleanupPlan{}
		for i := range plans {
			plan := &plans[i]
			if plan.key.Client != clientName || !planHasEntries(plan) {
				continue
			}
			planMark := transaction.Mark()
			planFailed := false
			entryNames := make([]string, 0, len(plan.matches))
			for _, match := range plan.matches {
				if plansByEntry[match.Name] == plan {
					entryNames = append(entryNames, match.Name)
				}
			}
			sort.Strings(entryNames)
			for _, entryName := range entryNames {
				entry := matchesByName[entryName]
				if plan.evidence == cleanupEvidenceManagedRouter {
					if failure := validateManagedCleanupPlan(leases[plan.observedRouterPort], deps.probeRoute, *plan); failure != "" {
						invalidatePlan(plan, failure)
						if rollbackErr := transaction.RollbackTo(planMark); rollbackErr != nil {
							return warnings.warnings, fmt.Errorf("direct cleanup %s pre-remove proof rollback: %w", sanitizeCleanupPlanIdentifier(clientName), rollbackErr)
						}
						planFailed = true
						break
					}
				}
				entryMark := transaction.Mark()
				capturedClient := client
				capturedBackup := backupPath
				capturedEntry := entry.Name
				transaction.AddCompensation(
					fmt.Sprintf("restore %s entry %s from cleanup backup", sanitizeCleanupPlanIdentifier(clientName), sanitizeCleanupPlanIdentifier(entry.Name)),
					func() error {
						return restoreRegisterClientEntryFromBackup(capturedClient, capturedBackup, capturedEntry)
					},
				)
				if err := client.RemoveEntry(entry.Name); err != nil {
					warnings.addRecord(directCleanupWarningRecord{
						class: cleanupWarningRemoveFailed, client: clientName,
						language: languageByName[entry.Name], entry: entry.Name,
					})
					if rollbackErr := transaction.RollbackTo(entryMark); rollbackErr != nil {
						return warnings.warnings, fmt.Errorf("direct cleanup %s remove rollback: %w", sanitizeCleanupPlanIdentifier(clientName), errors.Join(err, rollbackErr))
					}
					continue
				}
				if plan.evidence == cleanupEvidenceManagedRouter {
					if failure := validateManagedCleanupPlan(leases[plan.observedRouterPort], deps.probeRoute, *plan); failure != "" {
						invalidatePlan(plan, failure)
						if rollbackErr := transaction.RollbackTo(planMark); rollbackErr != nil {
							return warnings.warnings, fmt.Errorf("direct cleanup %s post-remove proof rollback: %w", sanitizeCleanupPlanIdentifier(clientName), rollbackErr)
						}
						planFailed = true
						break
					}
				}
				transaction.AddSuccessOutput(
					"cleanup removal "+sanitizeCleanupPlanIdentifier(clientName)+"/"+sanitizeCleanupPlanIdentifier(entry.Name),
					w,
					"✓ %s removed direct LSP entry %s (--lsp %s)\n",
					clientName,
					entry.Name,
					entry.Language,
				)
			}
			if !planFailed && plan.evidence == cleanupEvidenceManagedRouter {
				completedManagedPlans[plan.observedRouterPort] = append(
					completedManagedPlans[plan.observedRouterPort],
					*plan,
				)
			}
		}
		ports := make([]int, 0, len(completedManagedPlans))
		for port := range completedManagedPlans {
			ports = append(ports, port)
		}
		sort.Sort(sort.Reverse(sort.IntSlice(ports)))
		for _, port := range ports {
			capturedLease := leases[port]
			capturedPort := port
			clientPlans := append([]directCleanupPlan(nil), completedManagedPlans[port]...)
			transaction.AddFinalizer(
				fmt.Sprintf("final managed-router validation for %s port %d", sanitizeCleanupPlanIdentifier(clientName), capturedPort),
				func() error {
					for _, plan := range clientPlans {
						if failure := validateManagedCleanupPlan(capturedLease, deps.probeRoute, plan); failure != "" {
							return fmt.Errorf("%s", failure)
						}
					}
					return nil
				},
			)
		}
	}
	warnings.flushProofWarnings()
	return warnings.warnings, nil
}

func validateManagedCleanupPlan(
	lease ManagedRouterLease,
	probeRoute func(context.Context, int, string, string) managedRouteProof,
	plan directCleanupPlan,
) string {
	if lease == nil {
		return ManagedRouterFailureIdentityUnavailable
	}
	if failure := lease.Revalidate(context.Background()); failure != "" {
		return string(normalizeManagedRouterFailureClass(
			failure,
			directCleanupWarningClass(ManagedRouterFailureIdentityChanged),
		))
	}
	proof := probeRoute(context.Background(), plan.observedRouterPort, plan.key.Language, plan.backend)
	if !proof.OK {
		return string(normalizeManagedRouteFailureClass(proof.FailureClass))
	}
	return ""
}

type registerClientRollbackRestorer interface {
	RestoreEntryFromBackupForRollback(backupPath, name string) error
}

func restoreRegisterClientEntryFromBackup(client registerClient, backupPath, entryName string) error {
	restorer, ok := client.(registerClientRollbackRestorer)
	if !ok {
		return fmt.Errorf("client adapter does not support exact rollback restore")
	}
	return restorer.RestoreEntryFromBackupForRollback(backupPath, entryName)
}

func addLSPCleanupAlias(aliases map[string]bool, value string) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value != "" {
		aliases[value] = true
	}
}

// directLSPSurvivorMatchesWorkspace is the SINGLE-OWNER cross-kind survivor
// predicate for the post-register direct-LSP cleanup (bot PR #425 follow-up
// FINDING 2, architect GATE PASS). Both caller families
// (matchingDirectGoplsMCPEntries and matchingDirectLanguageServerEntries) now
// consume the SAME all-stdio survivor reader
// (ActiveStdioEntriesExcludingWriteTarget) and run THIS one predicate, so a
// re-emergent direct LSP of EITHER command family for THIS workspace blocks the
// destructive removal. Before the fix the two families used two readers with two
// blind rechecks: the gopls path only blocked on a gopls-mcp survivor (a
// re-emergent mcp-language-server for the same workspace was invisible) and the
// mcp-language-server path read the LSP-only reader that DROPS gopls-mcp (a
// re-emergent gopls-mcp for the same workspace was invisible) — a cross-kind
// false-cleanup deleting a write-target entry while the OTHER family survives.
//
// s blocks removal when it is, for canonicalWorkspace:
//   - a true gopls-MCP survivor (isCommandBasename gopls + "mcp" arg), OR
//   - a true mcp-language-server survivor whose classified --lsp language
//     matches the cleanup aliases.
//
// Both classifications REUSE existing single owners (NO new shape logic):
// gopls via isCommandBasename + stringSliceContainsFold; mcp-language-server via
// clients.ClassifyLanguageServerStdioEntry (which wraps matchLanguageServerStdio
// = isLanguageServerBinary + extractLspLanguageArg). Workspace via the shared
// directEntryWorkspaceMatches. BOTH branches are now alias-gated (bot PR #425
// follow-up FINDING 2): the gopls branch requires lspCleanupAliasMatches("gopls",
// aliases) and the mcp-language-server branch requires the classified --lsp language
// to match the aliases. A Go cleanup adds BOTH "go" and "gopls" to aliases
// (cleanupDirectLanguageServerEntriesAfterRegister → addLSPCleanupAlias(lang) +
// addLSPCleanupAlias(spec.LspCommand="gopls")), so the gopls branch still fires for a
// Go cleanup; but a NON-Go cleanup (e.g. python) whose aliases do NOT carry gopls no
// longer has a same-name lower gopls-mcp-for-W survivor wrongly block removal of the
// non-Go entry (matchingDirectLanguageServerEntries runs UNCONDITIONALLY, outside the
// go/gopls candidate guard). The matchingDirectGoplsMCPEntries candidate path is
// unaffected: it only runs when aliases already carry go/gopls, so the alias gate is
// always satisfied there.
func directLSPSurvivorMatchesWorkspace(s clients.StdioEntry, aliases map[string]bool, canonicalWorkspace string) bool {
	if !directEntryWorkspaceMatches(s.Args, canonicalWorkspace) {
		return false
	}
	if isCommandBasename(s.Command, "gopls") && stringSliceContainsFold(s.Args, "mcp") && lspCleanupAliasMatches("gopls", aliases) {
		return true
	}
	if lang, ok := clients.ClassifyLanguageServerStdioEntry(s); ok && lspCleanupAliasMatches(lang, aliases) {
		return true
	}
	return false
}

// matchingDirectLanguageServerEntries narrows the FindStdioLanguageServerEntries
// candidates to alias-matching entries scoped to THIS workspace, then applies the
// SAME WORKSPACE-SCOPED CROSS-KIND post-removal survivor recheck as the gopls path
// (bot PR #425 follow-up FINDING 2, architect GATE PASS). FindStdioLanguageServerEntries'
// own branch (b) is now MANAGED-only and workspace-blind, so a same-name lower-layer
// mcp-language-server for a DIFFERENT workspace no longer wrongly blocks removal of
// the real workspace-A entry at the adapter. Here, for each candidate, fetch the
// active ALL-STDIO entries that re-emerge after a hypothetical RemoveEntry(name)
// (ActiveStdioEntriesExcludingWriteTarget — NOT the LSP-only reader, which DROPS
// gopls-mcp and was blind to a cross-kind gopls-for-THIS-workspace survivor) and
// BLOCK only if a re-emergent SAME-NAME entry satisfies the shared cross-kind
// predicate directLSPSurvivorMatchesWorkspace (a gopls-mcp OR an alias-matching
// mcp-language-server, both scoped to THIS workspace). Both survivor
// classifications reuse existing single owners — never re-derived caller-side.
func matchingDirectLanguageServerEntries(client registerClient, entries []clients.LanguageServerStdioEntry, aliases map[string]bool, canonicalWorkspace string) ([]clients.LanguageServerStdioEntry, error) {
	var out []clients.LanguageServerStdioEntry
	for _, entry := range entries {
		if !lspCleanupAliasMatches(entry.Language, aliases) || !directEntryWorkspaceMatches(entry.Args, canonicalWorkspace) {
			continue
		}
		survivors, err := activeStdioExcludingForDirectCleanup(client, entry.Name)
		if err != nil {
			return nil, err
		}
		blocked := false
		for _, s := range survivors {
			if s.Name == entry.Name && directLSPSurvivorMatchesWorkspace(s, aliases, canonicalWorkspace) {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		out = append(out, entry)
	}
	return out, nil
}

// matchingDirectGoplsMCPEntries narrows the branch-(a)-owned removable stdio
// candidates to direct gopls-MCP entries scoped to THIS workspace, then applies the
// WORKSPACE-SCOPED CROSS-KIND post-removal survivor recheck (bot PR #425 follow-up
// FINDING 2, architect GATE PASS). The adapter's branch (b) is now MANAGED-only and
// workspace-blind, so a same-name lower-layer DIFFERENT stdio (npx / a different
// LSP language / gopls for a DIFFERENT workspace) no longer wrongly blocks removal
// at the adapter. The workspace decision lives HERE, where canonicalWorkspace is
// known: for each candidate, fetch the active stdio entries that re-emerge after a
// hypothetical RemoveEntry(name) (ActiveStdioEntriesExcludingWriteTarget) and BLOCK
// removal only if a re-emergent SAME-NAME entry satisfies the shared cross-kind
// predicate directLSPSurvivorMatchesWorkspace — a true gopls-for-THIS-workspace OR
// an alias-matching mcp-language-server-for-THIS-workspace survivor (the latter was
// invisible to the old inline gopls-only filter, the FINDING 2 cross-kind gap). A
// re-emergent npx / different-language / different-workspace entry does NOT block.
// (Reader returns nil for non-mimo / test fakes → never blocks; a reader error
// propagates so the destructive cleanup aborts and deletes nothing.)
func matchingDirectGoplsMCPEntries(client registerClient, entries []clients.StdioEntry, aliases map[string]bool, canonicalWorkspace string) ([]clients.LanguageServerStdioEntry, error) {
	var out []clients.LanguageServerStdioEntry
	for _, entry := range entries {
		if !isCommandBasename(entry.Command, "gopls") || !stringSliceContainsFold(entry.Args, "mcp") {
			continue
		}
		if !directEntryWorkspaceMatches(entry.Args, canonicalWorkspace) {
			continue
		}
		survivors, err := activeStdioExcludingForDirectCleanup(client, entry.Name)
		if err != nil {
			return nil, err
		}
		blocked := false
		for _, s := range survivors {
			if s.Name == entry.Name && directLSPSurvivorMatchesWorkspace(s, aliases, canonicalWorkspace) {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		out = append(out, clients.LanguageServerStdioEntry{
			Name:     entry.Name,
			Command:  entry.Command,
			Language: "gopls",
			Args:     entry.Args,
		})
	}
	return out, nil
}

func directEntryWorkspaceMatches(args []string, canonicalWorkspace string) bool {
	raw := directEntryWorkspaceArg(args)
	if raw == "" {
		return false
	}
	canonical, err := CanonicalWorkspacePathForCleanup(raw)
	if err != nil {
		return false
	}
	return canonical == canonicalWorkspace
}

func directEntryWorkspaceArg(args []string) string {
	for i, arg := range args {
		for _, flag := range []string{"--workspace", "-workspace", "--dir", "-dir", "--directory", "-directory"} {
			if arg == flag && i+1 < len(args) {
				return args[i+1]
			}
			if value, ok := strings.CutPrefix(arg, flag+"="); ok {
				return value
			}
		}
	}
	return ""
}

func dedupeDirectLanguageServerEntries(entries []clients.LanguageServerStdioEntry) []clients.LanguageServerStdioEntry {
	seen := map[string]bool{}
	var out []clients.LanguageServerStdioEntry
	for _, entry := range entries {
		if entry.Name == "" || seen[entry.Name] {
			continue
		}
		seen[entry.Name] = true
		out = append(out, entry)
	}
	return out
}

func lspCleanupAliasMatches(value string, aliases map[string]bool) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return false
	}
	return aliases[value]
}

func isCommandBasename(command, want string) bool {
	base := strings.ReplaceAll(command, "\\", "/")
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(strings.ToLower(base), ".exe")
	return base == want
}

func stringSliceContainsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

func selectedClientLanguageKeys(bindings []config.ClientBinding, languages []string) map[clientLanguageKey]bool {
	selected := make(map[clientLanguageKey]bool, len(bindings)*len(languages))
	for _, binding := range bindings {
		for _, language := range languages {
			selected[clientLanguageKey{Client: binding.Client, Language: language}] = true
		}
	}
	return selected
}

// writeRegisteredClientEntries is the single owner of transaction-local write
// receipts for both normal and supervised registration. A receipt is appended
// only after AddEntry succeeds and its rollback has been registered. A client
// that appeared after entry-name composition is skipped rather than receiving
// an empty write target.
func writeRegisteredClientEntries(
	bindings []config.ClientBinding,
	allClients map[string]registerClient,
	entryNameByClient map[string]string,
	port int,
	language string,
	w io.Writer,
	transaction *registrationTransaction,
) ([]clientWriteReceipt, error) {
	receipts := make([]clientWriteReceipt, 0, len(bindings))
	seen := map[clientLanguageKey]string{}
	for _, binding := range bindings {
		client, ok := allClients[binding.Client]
		if !ok || !client.Exists() {
			continue
		}
		entryName := strings.TrimSpace(entryNameByClient[binding.Client])
		if entryName == "" {
			continue
		}
		key := clientLanguageKey{Client: binding.Client, Language: language}
		if priorName, duplicate := seen[key]; duplicate {
			if priorName != entryName {
				return nil, fmt.Errorf("write-receipt-conflict: client %s language %s resolved both %q and %q", binding.Client, language, priorName, entryName)
			}
			continue
		}

		priorEntry, err := client.GetEntry(entryName)
		if err != nil {
			return nil, fmt.Errorf("snapshot prior %s entry in %s: %w", entryName, binding.Client, err)
		}
		urlPath := binding.URLPath
		if urlPath == "" {
			urlPath = "/mcp"
		}
		entry := clients.MCPEntry{
			Name: entryName,
			URL:  fmt.Sprintf("http://127.0.0.1:%d%s", port, urlPath),
		}
		clientRef := client
		savedPrior := priorEntry
		capturedName := entryName
		capturedClientName := binding.Client
		transaction.AddCompensation("restore client entry "+capturedClientName+"/"+capturedName, func() error {
			if savedPrior != nil && !savedPrior.SourceBelowWriteTarget {
				if err := clientRef.AddEntry(*savedPrior); err != nil {
					return fmt.Errorf("restore prior %s entry in %s: %w", capturedName, capturedClientName, err)
				}
				fmt.Fprintf(w, "  rollback: restored prior %s entry in %s\n", capturedName, capturedClientName)
				return nil
			}
			if err := clientRef.RemoveEntry(capturedName); err != nil {
				return fmt.Errorf("remove %s entry from %s: %w", capturedName, capturedClientName, err)
			}
			fmt.Fprintf(w, "  rollback: removed %s entry from %s\n", capturedName, capturedClientName)
			return nil
		})
		if err := client.AddEntry(entry); err != nil {
			return nil, fmt.Errorf("write %s entry: %w", binding.Client, err)
		}

		seen[key] = entryName
		receipts = append(receipts, clientWriteReceipt{Key: key, EntryName: entryName})
		transaction.AddSuccessOutput(
			"client entry written "+capturedClientName+"/"+capturedName,
			w,
			"\u2713 %s \u2192 %s (entry %s)\n",
			binding.Client,
			entry.URL,
			entryName,
		)
	}
	return receipts, nil
}

// registerOneLanguage is the per-language unit of work. It (a) allocates a
// free port (or reuses the existing one for idempotent re-register),
// (b) creates the scheduler task, (c) writes each client entry, and
// accumulates rollback closures in order. Returns the entry ready to be
// Put in the registry.
//
// bindings is the registration-wide client scope resolved ONCE by
// registerWithManifest. It is a PARAMETER, not a re-resolution, so language N+1
// binds exactly what language 1 did even when the operator edits Settings →
// Clients while this loop is running (see effectiveClientBindings).
func (a *API) registerOneLanguage(
	m *config.ServerManifest,
	canonical, wsKey, lang string,
	opts RegisterOpts,
	reg *Registry,
	sch testScheduler,
	allClients map[string]registerClient,
	bindings []config.ClientBinding,
	w io.Writer,
	transaction *registrationTransaction,
) (result registeredLanguageResult, err error) {
	var spec config.LanguageSpec
	for _, l := range m.Languages {
		if l.Name == lang {
			spec = l
			break
		}
	}
	taskName := LSPTaskNameForWorkspaceLanguage(wsKey, lang)
	enrollRegisterWorkspaceAudit(transaction, taskName)
	if opts.SupervisedProxy {
		return a.registerOneLanguageSupervised(m, spec, canonical, wsKey, lang, opts, reg, sch, allClients, bindings, w, transaction)
	}
	// Phase 1: registry write window — acquire flock, load current state,
	// do all port/task/registry work, release flock BEFORE sch.Run so the
	// spawned proxy subprocess can acquire it. The rollback closures that
	// touch registry each re-acquire the flock themselves so rollback is
	// safe whether it runs during Phase 1 (flock still held) or Phase 2/3
	// (flock released).
	unlock, err := reg.LockWithRelease()
	if err != nil {
		return registeredLanguageResult{}, fmt.Errorf("acquire registry lock: %w", err)
	}
	lockToken := transaction.AddFinalizer("release registry lock for "+wsKey+"/"+lang, unlock)
	releaseUnlock := func() error {
		return transaction.Release(lockToken)
	}
	defer func() {
		err = errors.Join(err, releaseUnlock())
	}()
	if err := reg.Load(); err != nil {
		return registeredLanguageResult{}, fmt.Errorf("load registry: %w", err)
	}
	// Reuse existing entry port (idempotent re-register) or allocate new.
	prior, had := reg.Get(wsKey, lang)
	var port int
	if had {
		port = prior.Port
	} else {
		p, err := AllocatePort(reg, *m.PortPool)
		if err != nil {
			return registeredLanguageResult{}, err
		}
		port = p
		// Tentatively pin the port into the registry's in-memory set so
		// subsequent AllocatePort calls within the same Register loop don't
		// return the same port again.
		//
		// B.1: this is an LSP-row write; PutLSP enforces the @-prefix gate.
		capturedKey := wsKey
		capturedLang := lang
		transaction.AddCompensation("remove tentative registry row "+capturedKey+"/"+capturedLang, func() error {
			reg.Remove(capturedKey, capturedLang)
			return nil
		})
		if err := reg.PutLSP(WorkspaceEntry{WorkspaceKey: wsKey, WorkspacePath: canonical, Language: lang, Port: port}); err != nil {
			return registeredLanguageResult{}, fmt.Errorf("register: tentative LSP-row write rejected: %w", err)
		}
	}
	// 1. Create scheduler task (or replace — snapshot the prior XML so
	// rollback can restore it).
	canonicalExe, err := canonicalMcphubPath()
	if err != nil {
		return registeredLanguageResult{}, err
	}
	// Verify the canonical mcphub binary actually exists before creating a
	// scheduler task pointing at it. Without this preflight, a fresh user
	// who skipped `mcphub setup` could get a successful-looking register
	// that persists registry/client state for a non-existent binary path;
	// Windows schtasks /run only starts the task and never verifies the
	// action actually executed, so the registration appears to succeed
	// while no proxy ever comes up. Install does the same preflight in
	// installUsingEmbedFirst (see install.go:298-300).
	if _, err := os.Stat(canonicalExe); err != nil {
		return registeredLanguageResult{}, fmt.Errorf("%s not present — run `mcphub setup` once: %w", canonicalExe, err)
	}
	args := []string{
		"daemon", "workspace-proxy",
		"--port", fmt.Sprintf("%d", port),
		"--workspace", canonical,
		"--language", lang,
	}
	var priorXML []byte
	if xml, err := sch.ExportXML(taskName); err == nil {
		priorXML = xml
	} else if !errors.Is(err, scheduler.ErrTaskNotFound) {
		// Only "task not found" is safe to ignore — any other export error
		// (permission, scheduler service down, XML corruption) means we do
		// NOT have a reliable priorXML snapshot. Proceeding would Delete
		// the existing task and leave rollback unable to restore it on a
		// later failure, turning a recoverable re-register error into a
		// persistent outage.
		return registeredLanguageResult{}, fmt.Errorf("export prior task %s: %w", taskName, err)
	}
	// Register the scheduler rollback BEFORE the destructive Delete+Create
	// so a Create failure on a re-register path does not orphan the old
	// task. Previously the rollback was appended after Create: a transient
	// Create error left the prior task deleted with no restoration, turning
	// a recoverable registration error into a hard workspace outage.
	capturedTaskName := taskName
	capturedPriorXML := priorXML
	capturedPort := port
	startedNewTask := false
	transaction.AddCompensation("restore scheduler task "+capturedTaskName, func() error {
		var joined error
		// Kill the running proxy BEFORE deleting the task. On Windows,
		// sch.Delete removes the task definition but does NOT terminate
		// the already-started process. If sch.Run above succeeded and
		// a later step (client-config write, registry save) failed, the
		// rollback stack runs — without this kill, an orphan proxy would
		// keep the allocated port bound and break immediate re-register
		// attempts. killByPortFn is a no-op if nothing is listening.
		if startedNewTask && capturedPort > 0 {
			if killErr := killByPortFn(capturedPort, 5*time.Second); killErr != nil {
				joined = errors.Join(joined, fmt.Errorf("kill replacement proxy on port %d: %w", capturedPort, killErr))
			}
		}
		if deleteErr := sch.Delete(capturedTaskName); deleteErr != nil && !errors.Is(deleteErr, scheduler.ErrTaskNotFound) {
			joined = errors.Join(joined, fmt.Errorf("delete replacement task %s: %w", capturedTaskName, deleteErr))
		}
		if len(capturedPriorXML) > 0 {
			if importErr := sch.ImportXML(capturedTaskName, capturedPriorXML); importErr != nil {
				joined = errors.Join(joined, fmt.Errorf("restore prior task %s: %w", capturedTaskName, importErr))
			}
			// Restart the prior proxy. Without this, re-register rollback
			// would restore the old task definition but leave no process
			// running (we just killed the live proxy above), turning a
			// recoverable re-register error into a hard outage for the
			// language until next logon/manual restart.
			if runErr := sch.Run(capturedTaskName); runErr != nil {
				joined = errors.Join(joined, fmt.Errorf("restart prior task %s: %w", capturedTaskName, runErr))
			}
			if joined == nil {
				fmt.Fprintf(w, "  rollback: restored + restarted scheduler task %s\n", capturedTaskName)
			}
		} else {
			if joined == nil {
				fmt.Fprintf(w, "  rollback: deleted scheduler task %s\n", capturedTaskName)
			}
		}
		return joined
	})
	// Destructive replace: prior task (if any) is Deleted and the new task
	// Created. A Create failure triggers runRollback which fires the
	// closure above to restore priorXML (or no-op if there was no prior).
	//
	// Kill the currently-running proxy FIRST when replacing. Windows Task
	// Scheduler's Delete does NOT terminate the running child — without
	// this kill, the old proxy keeps `port` bound and sch.Run below fails
	// to bind. Only meaningful when we're actually replacing (priorXML
	// non-empty); on a first-time registration the port is unbound.
	if len(priorXML) > 0 && port > 0 {
		if err := killByPortFn(port, 5*time.Second); err != nil {
			return registeredLanguageResult{}, fmt.Errorf("kill prior proxy on port %d before replacement: %w", port, err)
		}
	}
	if err := sch.Delete(taskName); err != nil && !errors.Is(err, scheduler.ErrTaskNotFound) {
		return registeredLanguageResult{}, fmt.Errorf("delete prior task %s before replacement: %w", taskName, err)
	}
	taskSpec := scheduler.TaskSpec{
		Name:        taskName,
		Description: fmt.Sprintf("mcp-local-hub: workspace %s lang %s", canonical, lang),
		Command:     canonicalExe,
		Args:        args,
		// WorkingDir is the canonical workspace path, NOT the install
		// directory. Two reasons:
		//
		// 1. LSP backends (clangd, rust-analyzer, gopls, …) expect cwd to
		//    be the project root for compile_commands.json / Cargo.toml /
		//    go.mod discovery. Running them from ~/.local/bin/ broke that.
		//
		// 2. v0.3.0-blockers bug #1: Go 1.19+ exec.LookPath enforces
		//    CVE-2022-30580 — refuses to return a cwd-relative match. The
		//    install dir ~/.local/bin/ may contain a stale copy of the
		//    wrapper binary (mcp-language-server.exe), and Windows lookup
		//    semantics check cwd FIRST, so the cwd-relative match
		//    shadows the on-PATH copy and triggers ErrDot. Setting cwd
		//    to the workspace removes the shadow: the workspace
		//    contains source files only, never the wrapper binary, so
		//    LookPath falls through to PATH and finds the canonical
		//    install (~/go/bin or wherever the user has it).
		WorkingDir:       canonical,
		RestartOnFailure: true,
		LogonTrigger:     true,
	}
	if err := sch.Create(taskSpec); err != nil {
		return registeredLanguageResult{}, fmt.Errorf("create task %s: %w", taskName, err)
	}
	transaction.AddSuccessOutput(
		"scheduler task created "+taskName,
		w,
		"\u2713 Scheduler task created: %s\n",
		taskName,
	)
	// Pre-compute client entry names so the registry entry can be fully
	// composed BEFORE we start the proxy. The daemon launched by sch.Run
	// loads workspaces.yaml on startup and exits if its (workspaceKey,
	// language) is absent — persisting-before-Run closes that race.
	entryNameByClient := map[string]string{}
	if had {
		for k, v := range prior.ClientEntries {
			entryNameByClient[k] = v
		}
	}
	for _, b := range bindings {
		client, ok := allClients[b.Client]
		if !ok || !client.Exists() {
			continue
		}
		if _, already := entryNameByClient[b.Client]; !already {
			entryNameByClient[b.Client] = resolveWorkspaceScopedLSPEntryName(reg, m.Name, lang, wsKey)
		}
	}
	// On re-register (idempotent path, had == true), preserve the prior
	// weekly_refresh value. Otherwise a user who previously registered
	// with --no-weekly-refresh would have it silently re-enabled by any
	// later `mcphub register` invocation. A caller that wants to CHANGE
	// the setting on re-register must use a dedicated path (e.g., a
	// future `mcphub workspaces set weekly-refresh=...`).
	// Memo D1: knob-aware default. Explicit caller flag still wins.
	weeklyRefresh := resolveWeeklyRefresh(a, opts)
	if had {
		weeklyRefresh = prior.WeeklyRefresh
	}
	composedEntry := WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: canonical,
		Language:      lang,
		Backend:       spec.Backend,
		Port:          port,
		TaskName:      taskName,
		ClientEntries: entryNameByClient,
		WeeklyRefresh: weeklyRefresh,
		Lifecycle:     LifecycleConfigured,
	}
	capturedRegKey := wsKey
	capturedRegLang := lang
	capturedHad := had
	capturedPrior := prior
	transaction.AddCompensation("restore registry row "+capturedRegKey+"/"+capturedRegLang, func() error {
		return restoreRegistryRowForRollback(reg.path, capturedRegKey, capturedRegLang, capturedPrior, capturedHad)
	})
	// B.1: composedEntry is an LSP-row write; PutLSP enforces the @-prefix
	// gate (Language is the per-LSP language string, never the sentinel).
	if err := reg.PutLSP(composedEntry); err != nil {
		return registeredLanguageResult{}, fmt.Errorf("register: composed LSP-row write rejected: %w", err)
	}
	if err := reg.Save(); err != nil {
		return registeredLanguageResult{}, fmt.Errorf("persist registry: %w", err)
	}

	// Phase 1 complete: the registry row is persisted to disk. Release the
	// flock BEFORE sch.Run so the proxy subprocess launched by the scheduler
	// task can acquire it. Holding the flock through sch.Run + readiness
	// probe deadlocks the proxy: daemon_workspace.go's reg.Lock() blocks on
	// us, its port never binds, our readiness probe times out, and we then
	// roll back the registry row the already-blocked proxy was waiting to
	// read. Net result: "error: not registered" from the proxy and a
	// consistent 10s register failure. Regression-guarded by
	// TestRegisterOneLanguage_ReleasesFlockBeforeSchRun.
	if err := releaseUnlock(); err != nil {
		return registeredLanguageResult{}, fmt.Errorf("release registry lock before task start: %w", err)
	}

	// Start the proxy. Registry is already persisted, so daemon startup
	// finds the entry. Logon-triggered tasks only fire at the next logon,
	// so this sch.Run prevents the port from advertising dead until reboot.
	if err := sch.Run(taskName); err != nil {
		return registeredLanguageResult{}, fmt.Errorf("run task %s: %w", taskName, err)
	}
	startedNewTask = true
	// Verify readiness. Windows schtasks /Run only triggers the task
	// action; it does not report whether the action actually succeeded.
	// Without this probe, a bad binary path, port contention, startup
	// crash, or task-XML drift would produce a successful-looking
	// register whose client configs point at a dead port. The probe
	// polls 127.0.0.1:<port>/mcp with synthetic initialize until it
	// succeeds OR the bounded timeout elapses. Rollback stack fires
	// on timeout so registry / scheduler / client entries do not leak
	// for a proxy that never came up.
	if err := proxyReadinessFn(port, 10*time.Second); err != nil {
		return registeredLanguageResult{}, fmt.Errorf("proxy readiness on port %d: %w", port, err)
	}
	transaction.AddSuccessOutput(
		"scheduler task started "+taskName,
		w,
		"\u2713 Scheduler task started: %s\n",
		taskName,
	)
	if canonicalTaskName, err := a.writeRegisterRunningIntentForTask(
		taskName,
		transaction.AddCompensation,
	); err != nil {
		return registeredLanguageResult{}, fmt.Errorf(
			"write register running intent for %s: %w",
			canonicalTaskName,
			err,
		)
	}
	// Phase 3: re-acquire flock before client config writes. Client
	// adapters perform read-modify-write updates, so these writes must be
	// serialized against concurrent register/unregister operations.
	unlock, err = reg.LockWithRelease()
	if err != nil {
		return registeredLanguageResult{}, fmt.Errorf("re-acquire registry lock: %w", err)
	}
	lockToken = transaction.AddFinalizer("release registry lock before client updates for "+wsKey+"/"+lang, unlock)
	if err := reg.Load(); err != nil {
		return registeredLanguageResult{}, fmt.Errorf("reload registry: %w", err)
	}
	if _, ok := reg.Get(wsKey, lang); !ok {
		return registeredLanguageResult{}, fmt.Errorf("registry entry disappeared before client updates for %s/%s", wsKey, lang)
	}
	receipts, err := writeRegisteredClientEntries(bindings, allClients, entryNameByClient, port, lang, w, transaction)
	if err != nil {
		return registeredLanguageResult{}, err
	}
	return registeredLanguageResult{Entry: composedEntry, Receipts: receipts}, nil
}

// Unregister removes scheduler tasks, client-config entries, and registry
// rows for the named languages in workspacePath. If languages is empty/nil,
// every language for the workspace is removed. Cleanup is atomic in the
// sense that the registry is saved once at the end; scheduler and client
// operations are best-effort and captured in Warnings.
//
// Unknown workspaces (no entries matching workspace_key) return an error;
// unknown individual languages inside an otherwise-known workspace surface
// as warnings so the caller gets a best-effort teardown.
func (a *API) Unregister(workspacePath string, languages []string) (*UnregisterReport, error) {
	data, err := loadManifestYAMLEmbedFirst("mcp-language-server")
	if err != nil {
		return nil, fmt.Errorf("load manifest mcp-language-server: %w", err)
	}
	m, err := parseManifestForName("mcp-language-server", data)
	if err != nil {
		return nil, err
	}
	return a.unregisterWithManifest(m, workspacePath, languages, os.Stderr)
}

func (a *API) unregisterWithManifest(_ *config.ServerManifest, workspacePath string, languages []string, w io.Writer) (*UnregisterReport, error) {
	// Use the existence-tolerant variant: the operator may be cleaning up
	// a registration whose workspace directory has since been deleted,
	// moved, or is on an unavailable drive. Without this weakening, an
	// orphaned scheduler task / client entry / registry row survives
	// forever because the user cannot run `mcphub unregister` against a
	// missing path. Registration paths still use the strict form.
	canonical, err := CanonicalWorkspacePathForCleanup(workspacePath)
	if err != nil {
		return nil, err
	}
	wsKey := WorkspaceKey(canonical)
	// Legacy fallback: registry rows written before EvalSymlinks-based
	// canonicalization landed used the abs+clean+drive-normalized form
	// only. Keep both key sets available below so mixed-key workspaces
	// (some rows canonical, some legacy) clean up every targeted row.
	legacyCanonical, err := CanonicalWorkspacePathLegacyCompat(workspacePath)
	if err != nil {
		return nil, err
	}
	legacyWSKey := WorkspaceKey(legacyCanonical)
	regPath, err := registryPathForRegister()
	if err != nil {
		return nil, err
	}
	reg := NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := reg.Load(); err != nil {
		return nil, err
	}
	// B.1: default unregister removes only LSP rows; serena rows live under
	// SerenaLanguageSentinel and are removed via `--backend serena` /
	// `--backend all` (mapped to RemoveByBackend in the CLI). Using
	// ListByWorkspaceLSP centralises the sentinel filter at both the
	// canonical-key lookup and the legacy-key fallback.
	canonicalExisting := reg.ListByWorkspaceLSP(wsKey)
	legacyExisting := []WorkspaceEntry{}
	activeWSKey := wsKey
	if legacyWSKey != wsKey {
		legacyExisting = reg.ListByWorkspaceLSP(legacyWSKey)
		if len(canonicalExisting) == 0 && len(legacyExisting) > 0 {
			activeWSKey = legacyWSKey
		}
	}
	if len(canonicalExisting) == 0 && len(legacyExisting) == 0 {
		return nil, fmt.Errorf("workspace %s (key %s) is not registered", canonical, wsKey)
	}
	targets := languages
	if len(targets) == 0 {
		for _, e := range canonicalExisting {
			targets = appendUniqueUnregisterLanguage(targets, e.Language)
		}
		for _, e := range legacyExisting {
			targets = appendUniqueUnregisterLanguage(targets, e.Language)
		}
	}
	allClients := clientsAllForRegister()
	report := &UnregisterReport{Workspace: canonical, WorkspaceKey: activeWSKey}
	sch, schErr := schedulerNewForRegister()
	// Non-Windows backends return not-implemented; supervised LSP rows have no
	// scheduled task to delete there, so tolerate absence and skip only the
	// legacy-task Delete below.
	if schErr != nil {
		if schedulerUnavailableError(schErr) {
			sch = nil
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("scheduler unavailable (%v); skipping legacy task deletion (supervised rows have none)", schErr))
		} else {
			return nil, schErr
		}
	}
	for _, lang := range targets {
		var entries []WorkspaceEntry
		if entry, ok := reg.Get(wsKey, lang); ok {
			entries = append(entries, entry)
		}
		if legacyWSKey != wsKey {
			if entry, ok := reg.Get(legacyWSKey, lang); ok {
				entries = append(entries, entry)
			}
		}
		if len(entries) == 0 {
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("language %s not registered for workspace %s", lang, canonical))
			continue
		}
		for _, entry := range entries {
			targetWSKey := entry.WorkspaceKey
			intentTaskName := LSPIntentTaskNameForWorkspaceLanguage(targetWSKey, lang)
			if restoreSupervisorIntent, supervisorManaged, err := a.removeLSPSupervisorIntent(targetWSKey, lang); err != nil {
				report.Warnings = append(report.Warnings,
					fmt.Sprintf("remove supervisor intent %s: %v", intentTaskName, err))
				continue
			} else if supervisorManaged {
				ctx, cancel := context.WithTimeout(context.Background(), DefaultReconcileTimeout)
				_, err := registerSupervisorReconcileFn(ctx, true)
				cancel()
				if err != nil {
					if !errors.Is(err, ErrSupervisorIPCUnavailable) {
						restoreErr := error(nil)
						if restoreSupervisorIntent != nil {
							restoreErr = restoreSupervisorIntent()
						}
						return report, errors.Join(
							fmt.Errorf("supervisor reconcile after removing %s failed while supervisor is alive; retry unregister: %w", intentTaskName, err),
							labeledTransactionError("compensation", "restore supervisor intent "+intentTaskName, restoreErr),
						)
					}
					report.Warnings = append(report.Warnings,
						fmt.Sprintf("supervisor reconcile after removing %s: %v", intentTaskName, err))
				} else {
					fmt.Fprintf(w, "✓ removed supervisor intent %s\n", intentTaskName)
				}
			}
			// 1. Kill any live proxy bound to this language's port BEFORE we
			// Delete the scheduler task. sch.Delete removes the task record
			// but does NOT terminate the running child — without this kill,
			// the proxy keeps the port bound until the next reboot, which
			// breaks immediate re-register and leaves the registry/scheduler
			// disagreeing with what's actually on the network. The outcome
			// path proves the listener is mcphub-owned before killing; stale
			// registry rows must not terminate a foreign process that reused
			// the old port. Kill-on-absent (nothing listening) remains a
			// benign no-op for cold workspaces.
			if forceKillByPortFn != nil && entry.Port != 0 {
				outcome, err := forceKillByPortFn(entry.Port, 5*time.Second)
				if outcome == portKillIdentityMismatch {
					report.Warnings = append(report.Warnings,
						fmt.Sprintf("kill proxy on port %d (task %s): port owned by foreign process, not killing: %v",
							entry.Port, entry.TaskName, err))
				} else if err != nil {
					report.Warnings = append(report.Warnings,
						fmt.Sprintf("kill proxy on port %d (task %s): %v",
							entry.Port, entry.TaskName, err))
				}
			}
			// 2. Remove scheduler task. Task Scheduler's Delete is the
			// supported way to stop a logon-triggered task from respawning.
			// The kill-by-port above already terminated any live proxy; this
			// Delete prevents it from being re-launched at next logon.
			if sch != nil {
				if err := sch.Delete(entry.TaskName); err != nil {
					report.Warnings = append(report.Warnings,
						fmt.Sprintf("delete task %s: %v", entry.TaskName, err))
				} else {
					fmt.Fprintf(w, "\u2713 deleted scheduler task %s\n", entry.TaskName)
				}
			}
			// 2. Remove client entries.
			for clientName, entryName := range entry.ClientEntries {
				client, ok := allClients[clientName]
				if !ok || !client.Exists() {
					continue
				}
				if shouldPreserveSharedLSPRouterEntry(client, entryName, lang) {
					fmt.Fprintf(w, "✓ preserved shared LSP router entry %s in %s\n", entryName, clientName)
					continue
				}
				if err := client.RemoveEntry(entryName); err != nil {
					report.Warnings = append(report.Warnings,
						fmt.Sprintf("remove entry %s from %s: %v", entryName, clientName, err))
				} else {
					fmt.Fprintf(w, "\u2713 removed %s entry from %s\n", entryName, clientName)
				}
			}
			// 3. Drop registry row.
			reg.Remove(targetWSKey, lang)
			report.Removed = append(report.Removed, lang)
		}
	}
	if err := reg.Save(); err != nil {
		return report, fmt.Errorf("persist registry: %w", err)
	}
	return report, nil
}

func appendUniqueUnregisterLanguage(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func shouldPreserveSharedLSPRouterEntry(client registerClient, entryName, language string) bool {
	if entryName != LSPRouterEntryName(language) {
		return false
	}
	live, err := client.GetEntry(entryName)
	if err != nil {
		return false
	}
	owned, _ := entryIsOwnedLSPRouterForLanguage(entryName, live, language, 0)
	return owned
}

// ResolveEntryName returns the client-config entry name to use for a given
// (server, language, workspaceKey). The default name is "<server>-<lang>"
// (e.g. mcp-language-server-python). If a DIFFERENT workspace in the
// registry already owns that base name, append "-<4hex>" from the
// workspace key. If the SAME workspace owns it (idempotent re-register),
// return the base name — we never rename an existing managed entry.
func ResolveEntryName(reg *Registry, serverName, language, workspaceKey string) string {
	base := serverName + "-" + language
	// Walk every registry entry; any OTHER workspace using the base name
	// → collision, suffix ours. Our own prior entry → idempotent, same
	// name.
	for _, e := range reg.Workspaces {
		for _, name := range e.ClientEntries {
			if name == base && e.WorkspaceKey != workspaceKey {
				short := workspaceKey
				if len(short) > 4 {
					short = short[:4]
				}
				candidate := base + "-" + short
				// Two workspaces sharing the first 4 hex chars of their
				// keys AND colliding on the same language would otherwise
				// get the same candidate name, causing one register to
				// overwrite the other's client entry. Fall back to the
				// full 8-char key when the short form is also taken.
				if entryNameTakenByOtherWorkspace(reg, candidate, workspaceKey) {
					return base + "-" + workspaceKey
				}
				return candidate
			}
		}
	}
	return base
}

// entryNameTakenByOtherWorkspace returns true iff some registry entry
// (other than our own workspaceKey) already uses the given client entry
// name. Used by ResolveEntryName to escape short-suffix collisions.
func entryNameTakenByOtherWorkspace(reg *Registry, candidate, workspaceKey string) bool {
	for _, e := range reg.Workspaces {
		if e.WorkspaceKey == workspaceKey {
			continue
		}
		for _, name := range e.ClientEntries {
			if name == candidate {
				return true
			}
		}
	}
	return false
}

func sortedLanguageNames(m *config.ServerManifest) []string {
	out := make([]string, 0, len(m.Languages))
	for _, l := range m.Languages {
		out = append(out, l.Name)
	}
	sort.Strings(out)
	return out
}

// proxyReadinessFn is the test seam for verifyProxyReady. Production
// callers go through this hook. Tests override it to return nil
// immediately (skip the real HTTP probe against the fake scheduler
// whose Run doesn't actually spawn a proxy) or to inject failures.
var proxyReadinessFn = verifyProxyReady

// verifyProxyReady polls 127.0.0.1:<port>/mcp with a minimal MCP
// initialize request until the proxy answers OR the bounded timeout
// elapses. Returns nil on first successful 200 response, error
// otherwise. 200ms polling interval balances quick-success latency
// against thrashing the port during slower startups. Called right
// after sch.Run so register can error-and-rollback instead of
// reporting a successful registration whose proxy never came up.
func verifyProxyReady(port int, timeout time.Duration) error {
	return verifyProxyReadyForServerNames(port, timeout, nil)
}

func verifySerenaProxyReady(port int, timeout time.Duration) error {
	return verifyProxyReadyForServerNames(port, timeout, map[string]struct{}{"serena": {}})
}

const readinessResponseMaxBytes = 1 << 20

func verifyProxyReadyForServerNames(port int, timeout time.Duration, allowedServerNames map[string]struct{}) error {
	deadline := time.Now().Add(timeout)
	url := clients.HubLoopbackURL(port, "/mcp")
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","clientInfo":{"name":"mcphub-register-readiness","version":"1.0.0"},"capabilities":{}}}`)
	client := &http.Client{Timeout: 2 * time.Second}
	var lastErr error
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("build readiness request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if resp.StatusCode == http.StatusOK {
			if len(allowedServerNames) == 0 {
				_ = resp.Body.Close()
				return nil
			}
			respBody, readErr := readReadinessResponse(resp)
			_ = resp.Body.Close()
			if readErr != nil {
				lastErr = fmt.Errorf("read readiness response: %w", readErr)
			} else if err := verifyReadinessServerInfo(respBody, allowedServerNames); err != nil {
				lastErr = err
			} else {
				return nil
			}
		} else {
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("readiness probe status %d", resp.StatusCode)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timeout after %s", timeout)
	}
	return lastErr
}

func readReadinessResponse(resp *http.Response) ([]byte, error) {
	if isSSEContentType(resp.Header.Get("Content-Type")) {
		return readReadinessSSEResponse(resp.Body, readinessResponseMaxBytes)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, readinessResponseMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > readinessResponseMaxBytes {
		return nil, fmt.Errorf("readiness response too large (> %d bytes)", readinessResponseMaxBytes)
	}
	return raw, nil
}

func verifyReadinessServerInfo(body []byte, allowedServerNames map[string]struct{}) error {
	payload := bytes.TrimSpace(body)
	if looksLikeSSE(payload) {
		var err error
		payload, err = readReadinessSSEResponse(bytes.NewReader(payload), len(payload)+1)
		if err != nil {
			return err
		}
	}
	var env readinessJSONRPCEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return fmt.Errorf("decode readiness response: %w", err)
	}
	if err := validateReadinessJSONRPCEnvelope(env); err != nil {
		return err
	}
	if env.Error != nil {
		return fmt.Errorf("readiness response JSON-RPC error code=%d: %s", env.Error.Code, env.Error.Message)
	}
	name := strings.ToLower(strings.TrimSpace(env.Result.ServerInfo.Name))
	if name == "" {
		return fmt.Errorf("readiness response missing serverInfo.name")
	}
	if _, ok := allowedServerNames[name]; !ok {
		return fmt.Errorf("readiness response serverInfo.name %q not allowed", env.Result.ServerInfo.Name)
	}
	return nil
}

type readinessJSONRPCEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Result *struct {
		ServerInfo struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	} `json:"result"`
}

func looksLikeSSE(payload []byte) bool {
	return bytes.HasPrefix(payload, []byte("data:")) || bytes.Contains(payload, []byte("\ndata:"))
}

func readReadinessSSEResponse(r io.Reader, maxBytes int) ([]byte, error) {
	return readSSESelectedResponse(r, maxBytes, selectReadinessJSONRPCResponse, "readiness JSON-RPC response event")
}

func selectReadinessJSONRPCResponse(dataLines [][]byte) ([]byte, bool) {
	payload := bytes.Join(dataLines, []byte("\n"))
	var env readinessJSONRPCEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, false
	}
	if validateReadinessJSONRPCEnvelope(env) != nil {
		return nil, false
	}
	return payload, true
}

func validateReadinessJSONRPCEnvelope(env readinessJSONRPCEnvelope) error {
	if env.JSONRPC != "2.0" {
		return fmt.Errorf("readiness response missing JSON-RPC 2.0 envelope")
	}
	if !bytes.Equal(bytes.TrimSpace(env.ID), []byte("1")) {
		return fmt.Errorf("readiness response missing matching JSON-RPC id 1")
	}
	if env.Method != "" {
		return fmt.Errorf("readiness response is a JSON-RPC notification, not a response")
	}
	if env.Result == nil && env.Error == nil {
		return fmt.Errorf("readiness response missing result or error")
	}
	return nil
}

// --- Test seams ---------------------------------------------------------
//
// The register path depends on a scheduler backend, a map of client
// adapters, and a registry file path. All three are injected through
// package-scoped function hooks that default to the real production
// implementations. Tests assign replacements via newRegisterHarness and
// restore them in defer.

// testScheduler is the subset of scheduler.Scheduler the register/unregister
// paths use. Keeping the interface narrow makes fake implementations
// trivial.
type testScheduler interface {
	Create(spec scheduler.TaskSpec) error
	Delete(name string) error
	Run(name string) error
	ExportXML(name string) ([]byte, error)
	ImportXML(name string, xml []byte) error
}

type legacyTaskUnavailableScheduler struct{}

func (legacyTaskUnavailableScheduler) Create(scheduler.TaskSpec) error {
	return errors.New("scheduler unavailable for legacy task creation")
}
func (legacyTaskUnavailableScheduler) Delete(string) error { return scheduler.ErrTaskNotFound }
func (legacyTaskUnavailableScheduler) Run(string) error    { return scheduler.ErrTaskNotFound }
func (legacyTaskUnavailableScheduler) ExportXML(string) ([]byte, error) {
	return nil, scheduler.ErrTaskNotFound
}
func (legacyTaskUnavailableScheduler) ImportXML(string, []byte) error {
	return errors.New("scheduler unavailable for legacy task restore")
}

// registerClient is the subset of clients.Client the register path
// consumes. Lets tests substitute an in-memory map.
type registerClient interface {
	Exists() bool
	BackupKeep(keepN int) (string, error)
	AddEntry(clients.MCPEntry) error
	RemoveEntry(name string) error
	GetEntry(name string) (*clients.MCPEntry, error)
	AllStdioEntries() ([]clients.StdioEntry, error)
	FindStdioLanguageServerEntries() ([]clients.LanguageServerStdioEntry, error)
}

// removableStdioCandidatesClient / findStdioLanguageServerCandidatesClient are the
// OPTIONAL WORKSPACE-AWARE register-grain candidate sources (architect REVISE →
// Option C, bot PR #425 follow-up GAP 2): the destructive direct-gopls/LSP cleanup
// candidate set with branch (a) + the MANAGED-only survivor, OMITTING the
// FILE/inline/import-WIDE survivor (which the grain re-applies caller-side,
// workspace-scoped). Only the mimo client implements them. They are DISTINCT from
// the conservative full-survivor RemovableStdioEntries / FindStdioLanguageServerEntries
// that the WORKSPACE-FREE `mcphub language-server cleanup` (cli/language_server.go)
// keeps consuming — so that workspace-free consumer never wrong-deletes a
// file/import re-emergence.
type removableStdioCandidatesClient interface {
	RemovableStdioCandidatesWriteTargetOwned() ([]clients.StdioEntry, error)
}

type findStdioLanguageServerCandidatesClient interface {
	FindStdioLanguageServerCandidatesWriteTargetOwned() ([]clients.LanguageServerStdioEntry, error)
}

// removableStdioEntriesForDirectCleanup picks the destructive-safe WORKSPACE-AWARE
// stdio CANDIDATE view for the direct-gopls cleanup. Two type-assert gates compose:
//
//   - OUTER gate (this function): test-fake isolation. The in-memory fakeClient
//     used by register_test.go does NOT implement the candidate method, so it falls
//     back to AllStdioEntries() — tests drive removability via their own
//     allStdioEntries fixture, not the mimo layer-merge logic. The production
//     realClientAdapter DOES implement it, so it takes the discriminating path.
//   - INNER gate (realClientAdapter.RemovableStdioCandidatesWriteTargetOwned): real
//     mimo discrimination. It asserts the wrapped clients.Client (a lockingClient
//     over *mimoCodeClient) implements the optional candidate method; only mimo
//     does, so only mimo gets the branch (a) + managed-only candidate scoping. Every
//     other real client falls back to AllStdioEntries (unchanged behavior). The
//     FILE-layer survivor is then re-applied workspace-scoped in
//     matchingDirectGoplsMCPEntries.
func removableStdioEntriesForDirectCleanup(client registerClient) ([]clients.StdioEntry, error) {
	if c, ok := client.(removableStdioCandidatesClient); ok {
		return c.RemovableStdioCandidatesWriteTargetOwned()
	}
	return client.AllStdioEntries()
}

// findStdioLanguageServerCandidatesForDirectCleanup is the LSP sibling: the
// WORKSPACE-AWARE register-grain candidate source (branch (a) + managed-only), with
// the same outer/inner gate composition. Non-mimo clients (and the test fake) fall
// back to the conservative FindStdioLanguageServerEntries — behavior-unchanged.
func findStdioLanguageServerCandidatesForDirectCleanup(client registerClient) ([]clients.LanguageServerStdioEntry, error) {
	if c, ok := client.(findStdioLanguageServerCandidatesClient); ok {
		return c.FindStdioLanguageServerCandidatesWriteTargetOwned()
	}
	return client.FindStdioLanguageServerEntries()
}

// activeStdioExcludingClient is the OPTIONAL post-removal active-reader capability
// (bot PR #425 follow-up, architect GATE PASS). It returns the active ALL-STDIO
// entries that re-emerge after a hypothetical RemoveEntry(name) from the write
// target — workspace-FREE (the adapter never sees a workspace). Only the mimo
// client implements it; every other client (and the register_test.go fakeClient)
// lacks it and activeStdioExcludingForDirectCleanup falls back to an EMPTY set =
// "no re-emergent survivor" (correct default for a single-file adapter where
// RemoveEntry deletes the sole definition). The CALLER then applies the shared
// WORKSPACE-SCOPED CROSS-KIND survivor predicate (directLSPSurvivorMatchesWorkspace)
// over the returned all-stdio entries, keeping the workspace decision out of the
// adapter. FINDING 2 collapsed the former LSP-only sibling reader into this single
// all-stdio reader so a cross-kind (gopls-mcp vs mcp-language-server) survivor for
// the same workspace is never invisible.
type activeStdioExcludingClient interface {
	ActiveStdioEntriesExcludingWriteTarget(name string) ([]clients.StdioEntry, error)
}

// activeStdioExcludingForDirectCleanup returns the post-removal active stdio set
// for `name`, or (nil, nil) when the client does not implement the optional reader
// (non-mimo / test fake) — an empty set means the caller never blocks removal.
func activeStdioExcludingForDirectCleanup(client registerClient, name string) ([]clients.StdioEntry, error) {
	if c, ok := client.(activeStdioExcludingClient); ok {
		return c.ActiveStdioEntriesExcludingWriteTarget(name)
	}
	return nil, nil
}

// Package-level hooks — nil in production (fall back to real schedulers /
// clients); tests assign replacements via newRegisterHarness and restore
// them in defer.
var (
	testSchedulerFactory     func() (testScheduler, error)
	testClientFactory        func() map[string]registerClient
	testRegistryPathOverride string
)

func schedulerNewForRegister() (testScheduler, error) {
	if testSchedulerFactory != nil {
		return testSchedulerFactory()
	}
	real, err := scheduler.New()
	if err != nil {
		return nil, err
	}
	return realSchedulerAdapter{real}, nil
}

func clientsAllForRegister() map[string]registerClient {
	if testClientFactory != nil {
		return testClientFactory()
	}
	out := map[string]registerClient{}
	for name, c := range clients.AllClients() {
		out[name] = realClientAdapter{c}
	}
	return out
}

func registryPathForRegister() (string, error) {
	if testRegistryPathOverride != "" {
		return testRegistryPathOverride, nil
	}
	return DefaultRegistryPath()
}

type realSchedulerAdapter struct{ s scheduler.Scheduler }

func (a realSchedulerAdapter) Create(spec scheduler.TaskSpec) error  { return a.s.Create(spec) }
func (a realSchedulerAdapter) Delete(name string) error              { return a.s.Delete(name) }
func (a realSchedulerAdapter) Run(name string) error                 { return a.s.Run(name) }
func (a realSchedulerAdapter) ExportXML(name string) ([]byte, error) { return a.s.ExportXML(name) }
func (a realSchedulerAdapter) ImportXML(name string, xml []byte) error {
	return a.s.ImportXML(name, xml)
}

type realClientAdapter struct{ c clients.Client }

func (a realClientAdapter) Exists() bool { return a.c.Exists() }
func (a realClientAdapter) BackupKeep(keepN int) (string, error) {
	return a.c.BackupKeep(keepN)
}
func (a realClientAdapter) AddEntry(e clients.MCPEntry) error { return a.c.AddEntry(e) }
func (a realClientAdapter) RemoveEntry(name string) error     { return a.c.RemoveEntry(name) }
func (a realClientAdapter) GetEntry(name string) (*clients.MCPEntry, error) {
	return a.c.GetEntry(name)
}
func (a realClientAdapter) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	return a.c.RestoreEntryFromBackupForRollback(backupPath, name)
}
func (a realClientAdapter) AllStdioEntries() ([]clients.StdioEntry, error) {
	return a.c.AllStdioEntries()
}
func (a realClientAdapter) RemovableStdioEntries() ([]clients.StdioEntry, error) {
	if c, ok := a.c.(interface {
		RemovableStdioEntries() ([]clients.StdioEntry, error)
	}); ok {
		return c.RemovableStdioEntries()
	}
	return a.c.AllStdioEntries()
}
func (a realClientAdapter) ActiveStdioEntriesExcludingWriteTarget(name string) ([]clients.StdioEntry, error) {
	if c, ok := a.c.(interface {
		ActiveStdioEntriesExcludingWriteTarget(string) ([]clients.StdioEntry, error)
	}); ok {
		return c.ActiveStdioEntriesExcludingWriteTarget(name)
	}
	return nil, nil
}
func (a realClientAdapter) RemovableStdioCandidatesWriteTargetOwned() ([]clients.StdioEntry, error) {
	if c, ok := a.c.(interface {
		RemovableStdioCandidatesWriteTargetOwned() ([]clients.StdioEntry, error)
	}); ok {
		return c.RemovableStdioCandidatesWriteTargetOwned()
	}
	return a.c.AllStdioEntries()
}
func (a realClientAdapter) FindStdioLanguageServerCandidatesWriteTargetOwned() ([]clients.LanguageServerStdioEntry, error) {
	if c, ok := a.c.(interface {
		FindStdioLanguageServerCandidatesWriteTargetOwned() ([]clients.LanguageServerStdioEntry, error)
	}); ok {
		return c.FindStdioLanguageServerCandidatesWriteTargetOwned()
	}
	return a.c.FindStdioLanguageServerEntries()
}
func (a realClientAdapter) FindStdioLanguageServerEntries() ([]clients.LanguageServerStdioEntry, error) {
	return a.c.FindStdioLanguageServerEntries()
}
