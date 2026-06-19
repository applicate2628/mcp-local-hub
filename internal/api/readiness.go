package api

import (
	"fmt"
	"os/exec"
	"strings"

	"mcp-local-hub/internal/config"
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
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

// ReadinessReport aggregates every prerequisite check for one server. Ready is
// true iff every Requirement is OK. Unlike Preflight (which fails fast at the
// first blocker because it gates a mutating install), CheckServerReadiness
// runs every check so the operator sees the FULL list of what to fix at once.
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
// first blocker for the install gate). Today it covers the dependency + secret
// surface that produces the cryptic-failure pain: the launcher on PATH, the
// runtime behind it, and every required `secret:` ref resolving in the vault.
// Port-conflict checking stays in Preflight, where the reinstall-tolerant
// "is this our own daemon already holding the port?" logic lives.
func CheckServerReadiness(m *config.ServerManifest) *ReadinessReport {
	rep := &ReadinessReport{Server: m.Name, Ready: true}
	add := func(r ReadinessRequirement) {
		if !r.OK {
			rep.Ready = false
		}
		rep.Requirements = append(rep.Requirements, r)
	}

	// Launcher on PATH (with guided fix), then the runtime behind it.
	if m.Command != "" {
		disp, fix := LauncherGuidance(m.Command)
		if _, err := exec.LookPath(m.Command); err != nil {
			add(ReadinessRequirement{
				Name:   "launcher: " + disp,
				OK:     false,
				Reason: fmt.Sprintf("%q not found on PATH", m.Command),
				Fix:    fix,
			})
		} else {
			add(ReadinessRequirement{Name: "launcher: " + disp, OK: true})
			if rt := runtimeBehindLauncher(m.Command); rt != "" {
				rdisp, rfix := LauncherGuidance(rt)
				if _, err := exec.LookPath(rt); err != nil {
					add(ReadinessRequirement{
						Name:   "runtime: " + rdisp,
						OK:     false,
						Reason: fmt.Sprintf("%q (needed by %s) not found on PATH", rt, m.Command),
						Fix:    rfix,
					})
				} else {
					add(ReadinessRequirement{Name: "runtime: " + rdisp, OK: true})
				}
			}
		}
	}

	// Required secrets resolve in the vault. Reuses the install preflight's
	// checkSecretRefs so the readiness report and the gate agree on "is this
	// secret set". Aggregated into one requirement listing the keys, so the
	// operator sees exactly which secrets to set.
	var secretKeys []string
	for _, v := range m.Env {
		if strings.HasPrefix(v, "secret:") {
			secretKeys = append(secretKeys, strings.TrimPrefix(v, "secret:"))
		}
	}
	if len(secretKeys) > 0 {
		name := "secrets: " + strings.Join(secretKeys, ", ")
		if err := checkSecretRefs(m.Env); err != nil {
			add(ReadinessRequirement{
				Name:   name,
				OK:     false,
				Reason: "one or more required secrets are not set in the vault",
				Fix:    "Set them in the GUI Secrets screen, or run `mcphub secrets set <key>`.",
			})
		} else {
			add(ReadinessRequirement{Name: name, OK: true})
		}
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
// "what needs fixing before this works" view. Manifests that fail to resolve
// or parse are skipped (a malformed embedded manifest is a build-time bug, not
// an operator-facing readiness concern).
func AllServerReadiness() []*ReadinessReport {
	names, err := listManifestNamesEmbedFirst()
	if err != nil {
		return nil
	}
	out := make([]*ReadinessReport, 0, len(names))
	for _, name := range names {
		if rep, err := CheckServerReadinessByName(name); err == nil {
			out = append(out, rep)
		}
	}
	return out
}
