package api

import (
	"fmt"
	"os/exec"
	"strings"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/secrets"
)

// ReadinessRequirement is one checked prerequisite for a server to run. It is
// the unit the GUI renders and the install flow summarizes: a human Name,
// whether it is OK, and — when not — a Reason plus an actionable Fix (the
// exact command or next step). This is the DETECT half of the install
// "detect + guided prompt" UX
// (work-items/epics/2026-06-19-install-and-it-works-ux.md): a missing
// dependency or unset secret yields a guided fix here instead of a bare
// "command not found" or a downstream cryptic HTTP-502 at the client.
type ReadinessRequirement struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	// Optional marks an ADVISORY requirement that does NOT block readiness:
	// an unmet optional requirement (a not-yet-set `secret:` ref) still lets
	// the server install + spawn, so it does NOT flip ReadinessReport.Ready
	// to false. The GUI renders these as "set to enable" prompt fields at
	// install rather than blockers (install-and-it-works: secrets optional).
	Optional bool   `json:"optional,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Fix      string `json:"fix,omitempty"`
}

// ReadinessReport aggregates every prerequisite check for one server. Ready is
// true iff every NON-optional Requirement is OK (an unmet Optional requirement
// — e.g. an unset secret — is advisory and does not block Ready). Unlike
// Preflight (which fails fast at the first BLOCKING issue because it gates a
// mutating install), CheckServerReadiness runs every check so the operator
// sees the FULL list — blockers and optional prompts — at once.
type ReadinessReport struct {
	Server       string                 `json:"server"`
	Ready        bool                   `json:"ready"`
	Requirements []ReadinessRequirement `json:"requirements"`
}

// LauncherGuidance maps a manifest launcher command to its (human display
// name, actionable install guidance). It is the SINGLE OWNER of "how do I get
// this dependency" knowledge so the install preflight, the GUI readiness
// panel, and any future onboarding flow all surface the SAME guided fix
// rather than each re-deriving (or omitting) it. Unknown launchers get a
// generic but still actionable fallback. Per-OS hints are inline because a
// single mcphub binary serves Windows GA / Linux beta / macOS preview and the
// operator should see the line for their platform without a lookup.
func LauncherGuidance(command string) (display, fix string) {
	switch command {
	case "uvx", "uv":
		return "uv (Python tool runner)",
			"Install uv — Windows: `winget install astral-sh.uv`; macOS/Linux: `curl -LsSf https://astral.sh/uv/install.sh | sh`; or `pip install uv`. Docs: https://docs.astral.sh/uv/"
	case "npx", "npm", "node":
		return "Node.js (npx/npm/node)",
			"Install Node.js (ships npx/npm) — Windows: `winget install OpenJS.NodeJS.LTS`; macOS: `brew install node`; all: https://nodejs.org/"
	case "python", "python3":
		return "Python 3",
			"Install Python 3 — Windows: `winget install Python.Python.3.12`; macOS: `brew install python`; all: https://www.python.org/downloads/"
	case "go":
		return "Go toolchain",
			"Install Go — Windows: `winget install GoLang.Go`; macOS: `brew install go`; all: https://go.dev/dl/"
	case "mcp-language-server":
		return "mcp-language-server",
			"Install — `go install github.com/isaacphi/mcp-language-server@latest` (needs Go on PATH), or rely on the bundled mcphub LSP router."
	case "mcphub":
		return "mcphub (self)",
			"Run `mcphub setup` to install the canonical mcphub binary on PATH."
	case "gdb":
		return "gdb (GNU debugger)",
			"Install gdb — Windows (MSYS2 ucrt64): `pacman -S mingw-w64-ucrt-x86_64-gdb`; Linux: `apt install gdb`; macOS: `brew install gdb`."
	case "lldb":
		return "lldb (LLVM debugger)",
			"Install lldb — Windows (MSYS2 clang64): `pacman -S mingw-w64-clang-x86_64-lldb`; Linux: `apt install lldb`; macOS: ships with Xcode CLT (`xcode-select --install`)."
	case "clang", "clang++", "clang-cl":
		return "clang (LLVM)",
			"Install clang/LLVM — Windows: `winget install LLVM.LLVM`; Linux: `apt install clang`; macOS: `xcode-select --install`."
	default:
		return command,
			fmt.Sprintf("Install %q and ensure it is on PATH, then re-run install.", command)
	}
}

// runtimeBehindLauncher returns the deeper runtime command a launcher needs to
// actually fetch + run its package, when that differs from the launcher
// itself. LookPath(launcher) succeeding does NOT prove the runtime can fetch +
// run the target package; npx in particular delegates to node. Empty string
// means the launcher IS self-contained (uvx bootstraps its own Python; go,
// node, mcphub are themselves the runtime), so no deeper check is added.
func runtimeBehindLauncher(command string) string {
	switch command {
	case "npx":
		return "node"
	default:
		return ""
	}
}

// CheckServerReadiness runs every prerequisite check for a server WITHOUT
// failing fast and returns a structured, GUI-renderable report. It is the
// non-fatal, all-requirements companion to Preflight (which fails fast at the
// first blocker for the install gate). It MIRRORS what install + run actually
// require, so /api/server/readiness never reports Ready while the install gate
// (or the running server) would fail (Codex #377): the launcher on PATH + the
// runtime behind it, the canonical mcphub binary, declared required_binaries
// (incl. per-language), foreign daemon-port collisions, optional stdio env
// `secret:` refs (advisory), and required remote-http url/header
// ${secret:KEY} placeholders (blocking).
func CheckServerReadiness(m *config.ServerManifest) *ReadinessReport {
	rep := &ReadinessReport{Server: m.Name, Ready: true}
	add := func(r ReadinessRequirement) {
		// An unmet OPTIONAL requirement (an unset secret) is advisory — it
		// prompts the operator but does not block install/spawn, so it does
		// NOT flip Ready.
		if !r.OK && !r.Optional {
			rep.Ready = false
		}
		rep.Requirements = append(rep.Requirements, r)
	}

	// Launcher on PATH (with guided fix), then the runtime behind it. For a
	// workspace-scoped (dynamic-pool LSP) manifest the launcher is ADVISORY:
	// the actual backend is selected per workspace/language and some paths do
	// NOT use m.Command at all — a Go workspace runs the gopls-mcp backend
	// (launches gopls directly), so a missing mcp-language-server must not
	// mark a gopls-ready Go workspace not-ready (Codex #377). The per-language
	// required_binaries (already advisory) cover the real per-language tools.
	// Advisory ONLY for the per-LANGUAGE LSP shape (m.Languages, no
	// daemon_template), where the backend is selected per language and a Go
	// workspace runs gopls-mcp without m.Command. A daemon_template
	// workspace-scoped manifest (e.g. serena) DOES use m.Command as the child
	// launcher and Preflight rejects it missing, so keep that blocking
	// (Codex #377 r5).
	launcherOptional := m.Kind == config.KindWorkspaceScoped && m.DaemonTemplate == nil
	if m.Command != "" {
		disp, fix := LauncherGuidance(m.Command)
		if _, err := exec.LookPath(m.Command); err != nil {
			add(ReadinessRequirement{
				Name:     "launcher: " + disp,
				OK:       false,
				Optional: launcherOptional,
				Reason:   fmt.Sprintf("%q not found on PATH", m.Command),
				Fix:      fix,
			})
		} else {
			add(ReadinessRequirement{Name: "launcher: " + disp, OK: true})
			if rt := runtimeBehindLauncher(m.Command); rt != "" {
				rdisp, rfix := LauncherGuidance(rt)
				if _, err := exec.LookPath(rt); err != nil {
					add(ReadinessRequirement{
						Name:     "runtime: " + rdisp,
						OK:       false,
						Optional: launcherOptional,
						Reason:   fmt.Sprintf("%q (needed by %s) not found on PATH", rt, m.Command),
						Fix:      rfix,
					})
				} else {
					add(ReadinessRequirement{Name: "runtime: " + rdisp, OK: true})
				}
			}
		}
	}

	// Canonical mcphub binary — Preflight ALWAYS requires it (even on the
	// remote-http branch), so a fresh / dev-run host without `mcphub setup`
	// passes the launcher checks yet fails the install immediately. Mirror it
	// (Codex #377). In the live GUI mcphub is the running binary, so this is
	// only red on a not-yet-set-up host.
	if _, err := ensureCanonicalMcphubPresent(); err != nil {
		_, mfix := LauncherGuidance("mcphub")
		add(ReadinessRequirement{Name: "mcphub binary", OK: false, Reason: "canonical mcphub binary not installed", Fix: mfix})
	} else {
		add(ReadinessRequirement{Name: "mcphub binary", OK: true})
	}

	// Declared required external binaries (e.g. gdb-mcp → required_binaries:
	// [gdb]). These run THROUGH mcphub, so the launcher check passes even when
	// the actual backend tool is absent; surface them so the server is not
	// reported ready while its tool is missing (Codex #377).
	//
	// Server-level RequiredBinaries are BLOCKING (the server cannot function
	// without them). Per-LANGUAGE RequiredBinaries (workspace-scoped LSP
	// manifests) are ADVISORY/Optional: a manifest's `languages` are
	// ALTERNATIVES selected per workspace, so a Go-only workspace must not be
	// marked not-ready because rust-analyzer / fortls / tsserver are absent
	// (Codex #377). They are surfaced (with a Fix) but do not block Ready.
	seenBin := map[string]bool{}
	addBin := func(bin string, optional bool) {
		if bin == "" || seenBin[bin] {
			return
		}
		seenBin[bin] = true
		bdisp, bfix := LauncherGuidance(bin)
		if _, err := exec.LookPath(bin); err != nil {
			add(ReadinessRequirement{Name: "binary: " + bdisp, OK: false, Optional: optional,
				Reason: fmt.Sprintf("%q not found on PATH", bin), Fix: bfix})
		} else {
			add(ReadinessRequirement{Name: "binary: " + bdisp, OK: true})
		}
	}
	for _, bin := range m.RequiredBinaries {
		addBin(bin, false) // server-level: blocking
	}
	for _, lang := range m.Languages {
		for _, bin := range lang.RequiredBinaries {
			addBin(bin, true) // per-language: advisory (alternatives per workspace)
		}
	}

	// Daemon ports free — mirror Preflight's FOREIGN-collision check (a port
	// held by OUR own daemon is fine: idempotent reinstall). A green report
	// followed by an install that fails on a held port is exactly the lie this
	// surface must not tell (Codex #377).
	for _, d := range m.Daemons {
		if preflightPortInUse(d.Port) && !portHeldByOurDaemonForPortArm(d.Port, m.Name, d.Name, false) {
			add(ReadinessRequirement{Name: fmt.Sprintf("port %d (%s)", d.Port, d.Name), OK: false,
				Reason: "already in use by another process", Fix: "Free the port (close the other process) or change the daemon port in the manifest."})
		} else {
			add(ReadinessRequirement{Name: fmt.Sprintf("port %d (%s)", d.Port, d.Name), OK: true})
		}
		if m.Transport == config.TransportNativeHTTP {
			internal := d.Port + config.NativeHTTPInternalPortOffset
			if preflightPortInUse(internal) && !portHeldByOurDaemonForPortArm(internal, m.Name, d.Name, true) {
				add(ReadinessRequirement{Name: fmt.Sprintf("internal port %d (%s)", internal, d.Name), OK: false,
					Reason: "native-http upstream port already in use", Fix: "Free the port or change the daemon port (internal = external + offset)."})
			}
		}
	}

	// Workspace-scoped (dynamic-pool) registration allocates from a PortPool;
	// m.Daemons is EMPTY for these (e.g. mcp-language-server), so the
	// daemon-port loop above checks nothing. A port counts as TAKEN when it is
	// OS-bound OR already recorded in the registry (workspaces.yaml) — register's
	// AllocatePort skips registry-allocated ports and returns
	// ErrPortPoolExhausted even if nothing is currently listening (Codex #377
	// r4). For a native-http pool the materialized proxy also binds
	// external+offset upstream, so BOTH must be free.
	var registryTaken map[int]bool
	var registryLoadErr error
	// register resolves the path (DefaultRegistryPath) AND loads it before
	// allocating; BOTH can fail (no resolvable home/state dir in a headless
	// session → path error; corrupt file → load error). Either leaves register
	// unable to allocate, so capture it and let checkPool surface it as
	// blocking rather than silently falling back to OS-bound ports — which
	// would report the pool ready while register fails (Codex #377 r5/r7).
	if regPath, perr := DefaultRegistryPath(); perr != nil {
		registryLoadErr = fmt.Errorf("resolve registry path: %w", perr)
	} else {
		reg := NewRegistry(regPath)
		if lerr := reg.Load(); lerr != nil {
			registryLoadErr = fmt.Errorf("load registry: %w", lerr)
		} else {
			registryTaken = reg.AllocatedPorts()
		}
	}
	// portTaken mirrors the allocator EXACTLY: AllocatePort skips a port when
	// registry-allocated OR when portAvailable (the OS bind probe) is false. A
	// TCP dial (preflightPortInUse) differs — a port bound but not yet
	// accepting reads as free to a dial yet fails the allocator's bind, so use
	// the SAME portAvailable probe (Codex #377 r7).
	portTaken := func(port int) bool { return !portAvailable(port) || registryTaken[port] }
	checkPool := func(p *config.PortPool, nativeHTTP bool) {
		if p == nil || p.End < p.Start {
			return
		}
		if registryLoadErr != nil {
			add(ReadinessRequirement{
				Name:   fmt.Sprintf("port pool %d-%d", p.Start, p.End),
				OK:     false,
				Reason: fmt.Sprintf("workspace registry unreadable, cannot verify pool allocation: %v", registryLoadErr),
				Fix:    "Fix or remove the corrupt workspaces.yaml registry (register reads it before allocating a pool port).",
			})
			return
		}
		name := fmt.Sprintf("port pool %d-%d", p.Start, p.End)
		for port := p.Start; port <= p.End; port++ {
			if portTaken(port) {
				continue
			}
			if nativeHTTP && portTaken(port+config.NativeHTTPInternalPortOffset) {
				continue
			}
			add(ReadinessRequirement{Name: name, OK: true})
			return
		}
		add(ReadinessRequirement{Name: name, OK: false,
			Reason: "no port in the workspace pool is free (OS-bound or registry-allocated)",
			Fix:    "Free a pool port (or its native-http +offset upstream), or widen the pool in the manifest."})
	}
	// Serena's dynamic-pool manifest is transport: native-http, so its
	// materialized proxies bind external+offset upstream — mNative drives the
	// offset check for both the server pool and the daemon-template pool.
	mNative := m.Transport == config.TransportNativeHTTP
	checkPool(m.PortPool, mNative)
	if m.DaemonTemplate != nil {
		checkPool(m.DaemonTemplate.PortPool, mNative)
	}

	// A vault that EXISTS but is unreadable/undecryptable is BLOCKING for a
	// manifest that uses secret refs — the daemon (OpenVaultOptional +
	// HasSecretRef) fails the spawn, so readiness must NOT report it ready as a
	// merely-optional unset key. A truly-absent vault is fine (secrets optional)
	// (Codex #377 r5).
	if secrets.HasSecretRef(m.Env) {
		if _, verr := secrets.OpenVaultOptional(secrets.DefaultKeyPath(), secrets.DefaultVaultPath()); verr != nil {
			add(ReadinessRequirement{
				Name:   "secrets vault",
				OK:     false,
				Reason: fmt.Sprintf("vault exists but unreadable: %v", verr),
				Fix:    "Fix or remove the corrupt vault — a secret-using server fails to start when it cannot be read.",
			})
		}
	}

	// Declared secrets — reported PER KEY so the GUI can offer each as an
	// inline "fill this field at install" prompt (the operator's request:
	// "секреты нужно явно предлагать в конкретные поля при установке").
	// Secrets are OPTIONAL: an unset key is advisory (Optional=true), NOT a
	// blocker — the server still installs + spawns (the env var is omitted)
	// and reports its own missing-key if it actually needs it.
	for k, v := range m.Env {
		if !strings.HasPrefix(v, "secret:") {
			continue
		}
		key := strings.TrimPrefix(v, "secret:")
		req := ReadinessRequirement{Name: "secret: " + key, Optional: true}
		if err := checkSecretRefs(map[string]string{k: v}); err != nil {
			req.OK = false
			req.Reason = "not set in the vault (optional — the server runs without it, or reports its own missing-key)"
			req.Fix = fmt.Sprintf("Enter %s at install, or set it later via the Secrets screen / `mcphub secrets set %s`.", key, key)
		} else {
			req.OK = true
		}
		add(req)
	}
	// Non-secret env refs (file:KEY, $VAR) are NOT optional: at spawn,
	// ResolveMapBestEffort keeps them FATAL (only secret: refs are omitted), so
	// an unresolvable file:/$VAR makes the daemon fail to start. Run the SAME
	// resolver here (single owner, like the install-plan dry-run) — a nil vault
	// is fine because secret: refs are already surfaced per-key above and are
	// omitted by best-effort, so ONLY a non-secret failure (file: with no local
	// config, or an unset $VAR) errors — and surface it as a BLOCKING "env"
	// requirement, mirroring the spawn gate (Codex pre-catch r6).
	if _, _, err := secrets.NewResolver(nil, nil).ResolveMapBestEffort(m.Env); err != nil {
		add(ReadinessRequirement{
			Name:   "env",
			OK:     false,
			Reason: fmt.Sprintf("a non-secret env ref is fatal at spawn: %v", err),
			Fix:    "Fix the file:/$VAR env ref in the manifest (file: refs need a local config; $VAR must be set in the environment).",
		})
	}

	// Remote-http manifests carry REQUIRED vault values as ${secret:KEY}
	// placeholders in url + headers (NOT m.Env). buildRemoteHTTPPlan expands
	// them and FAILS the install on a missing one, so — unlike the optional
	// stdio env secrets above — these are BLOCKING (Codex #377).
	remoteSecretKeys := map[string]struct{}{}
	scan := func(s string) {
		for _, mt := range SecretPlaceholderRE.FindAllStringSubmatch(s, -1) {
			remoteSecretKeys[mt[1]] = struct{}{}
		}
	}
	scan(m.URL)
	for _, hv := range m.Headers {
		scan(hv)
	}
	for key := range remoteSecretKeys {
		req := ReadinessRequirement{Name: "secret (remote): " + key}
		if err := checkSecretRefs(map[string]string{"_": "secret:" + key}); err != nil {
			req.OK = false
			req.Reason = "required for the remote endpoint URL/headers but not set in the vault"
			req.Fix = fmt.Sprintf("Enter %s at install, or `mcphub secrets set %s` — the install fails without it.", key, key)
		} else {
			req.OK = true
		}
		add(req)
	}
	// Install-plan dry-run — the SINGLE-OWNER authoritative check. BuildPlan
	// is pure (validates + returns before any side effect) and runs the exact
	// binding / url_path / remote-matrix validation the real install performs:
	// non-remote client_bindings (unknown daemon, unsupported client, invalid
	// url_path), remote-http client_bindings against the adapter matrix, and
	// ExpandSecrets over url+headers (malformed `${secret:}`, CR/LF, missing
	// remote secret). Calling it here instead of re-deriving each check keeps
	// readiness from ever green-lighting an install the planner rejects, and
	// avoids duplicating gate logic that drifts (Codex #377 r2/r3/r4). The
	// per-key secret + dependency requirements above stay for the structured,
	// user-actionable inline prompts; this is the one catch-all blocker.
	// Validate EXACTLY the operator's effective default-install client set —
	// the same scope a normal `mcphub install` uses. NOT the bare
	// BuildPlan(m,"") compile-time trio (which would MISS a bad binding on a
	// client the operator persisted into their default set), and NOT
	// IncludeAllClients (which would validate OPT-IN bindings a default install
	// never touches → a false Ready=false on a bad opt-in binding, Codex #377
	// r7). DefaultInstallClientNamesEffectiveIn reads gui-preferences.yaml (it
	// derefs no *API state, so the zero value calls it); fall back to the
	// compile-time default on a read error.
	clientScope := clients.DefaultInstallClientNames()
	if eff, cerr := (&API{}).DefaultInstallClientNamesEffectiveIn(SettingsPath()); cerr == nil && len(eff) > 0 {
		clientScope = eff
	}
	if _, err := BuildPlanWithOpts(m, BuildPlanOpts{DefaultClientsOverride: clientScope}); err != nil {
		add(ReadinessRequirement{
			Name:   "install plan",
			OK:     false,
			Reason: fmt.Sprintf("the install planner rejects this manifest: %v", err),
			Fix:    "Fix the manifest client_bindings / url / headers per the error above (the install gate runs the same validation).",
		})
	}

	// daemon_template (dynamic-pool) manifests: BuildPlan does NOT exercise the
	// InstallParsedManifest admission gates (native-http transport, non-empty
	// daemon_template.context, no duplicate --context). Run the SAME validator
	// (single owner) so readiness mirrors that install path too (Codex #377 r6).
	if err := validateDynamicPoolManifest(m); err != nil {
		add(ReadinessRequirement{
			Name:   "dynamic-pool",
			OK:     false,
			Reason: err.Error(),
			Fix:    "Fix the daemon_template manifest: native-http transport, a non-empty daemon_template.context, and no --context token in base_args/extra_args_template.",
		})
	}

	return rep
}

// CheckServerReadinessByName resolves a server's manifest embed-first (the same
// source the installer uses) and runs CheckServerReadiness. The GUI
// /api/server/readiness endpoint calls this so the operator sees per-server
// readiness — with guided fixes — BEFORE installing, instead of discovering a
// missing dependency as a downstream cryptic failure. Returns an error only
// when the manifest cannot be resolved or parsed (an unknown server name); a
// resolvable manifest always yields a report (possibly Ready=false).
func CheckServerReadinessByName(name string) (*ReadinessReport, error) {
	data, err := loadManifestYAMLEmbedFirst(name)
	if err != nil {
		return nil, fmt.Errorf("resolve manifest %q: %w", name, err)
	}
	m, err := parseManifestForName(name, data)
	if err != nil {
		return nil, fmt.Errorf("parse manifest %q: %w", name, err)
	}
	return CheckServerReadiness(m), nil
}

// AllServerReadiness resolves every embedded/installed server manifest and
// returns a readiness report per server, so the GUI can show a fleet-wide
// "what needs fixing before this works" view. A manifest that fails to
// resolve/parse is SURFACED as a not-ready report with a "manifest parse"
// requirement rather than dropped: listManifestNamesEmbedFirst unions embedded
// AND disk manifests, so a parse failure can be an operator-facing broken
// CUSTOM server, and silently omitting it would make the fleet view look
// complete while hiding the server that needs attention (Codex #377).
func AllServerReadiness() []*ReadinessReport {
	names, err := listManifestNamesEmbedFirst()
	if err != nil {
		return nil
	}
	out := make([]*ReadinessReport, 0, len(names))
	for _, name := range names {
		rep, err := CheckServerReadinessByName(name)
		if err != nil {
			out = append(out, &ReadinessReport{
				Server: name,
				Ready:  false,
				Requirements: []ReadinessRequirement{{
					Name:   "manifest: " + name,
					OK:     false,
					Reason: fmt.Sprintf("manifest failed to load/parse: %v", err),
					Fix:    "Fix the manifest YAML (a custom server under the manifest dir), or remove it.",
				}},
			})
			continue
		}
		out = append(out, rep)
	}
	return out
}
