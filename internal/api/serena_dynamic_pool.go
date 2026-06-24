// Package api — Phase 2 shared serena dynamic-pool builder/service.
//
// This file is the SINGLE OWNER of the serena default port-pool + template
// policy. Three consumers read it so none re-implements the embed-first
// fail-closed read that finding #3 identified as the register↔migrate
// bootstrap cycle:
//
//   - `mcphub workspace register` (internal/cli/workspace_cmd.go) — allocates a
//     per-workspace port from the EFFECTIVE port-pool.
//   - the redesigned `mcphub migrate serena legacy-to-dynamic-pool` (Phase 4) —
//     builds the in-memory dynamic-pool manifest passed to InstallParsedManifest.
//   - E.2 auto-register-on-miss (Phase 5) — synthesizes the same per-workspace
//     descriptor in memory.
//
// The shipped (embedded) serena manifest stays the CATALOG / default input.
// It is `kind: global` today (the legacy 2-daemon claude/codex shape), so a
// consumer that demanded a `daemon_template.port_pool` from it would fail
// closed — which is exactly the cycle this service breaks. The service answers
// "what is the EFFECTIVE serena DaemonTemplate?" from the embed when the embed
// already declares one, else from a built-in dynamic-pool default baked in
// here.
//
// Design ref: docs/superpowers/specs/2026-05-29-serena-migrate-redesign-descriptor-proxy.md §5 (finding #3).
// Plan ref:   docs/superpowers/plans/2026-05-29-serena-migrate-redesign.md Phase 2.
package api

import (
	"fmt"
	"strings"

	"mcp-local-hub/internal/config"
)

// SerenaServerName is the canonical server name for the serena dynamic-pool
// manifest. Matches the embedded servers/serena/manifest.yaml `name:` field
// and the literal used across the install/registry/router surfaces.
const SerenaServerName = "serena"

// Built-in dynamic-pool default template policy. Used ONLY when the embedded
// serena manifest does not already declare a daemon_template (it is `kind:
// global` today — see servers/serena/manifest.yaml). When the embed migrates to
// the dynamic-pool shape (parent plan §G.1) its own daemon_template wins and
// these defaults stop being consulted.
const (
	// serenaDefaultContext is the resolved O1 value — the single authoritative
	// serena context for every per-workspace daemon. The materializer APPENDS
	// `--context <serenaDefaultContext>` to each child argv; it is NOT a token
	// in extra_args_template. HEAD servers/serena/manifest.yaml mandates
	// `codex` (design §9 O1 / plan §591); the dynamic-pool model fronts all
	// clients through the /serena/mcp router, so a single context per workspace
	// is structurally required (per-client context is unreachable — design §9
	// O1 (a)).
	serenaDefaultContext = "codex"

	// serenaDefaultPortPoolStart / serenaDefaultPortPoolEnd is the default port
	// range serena workspace daemons allocate from. It starts ABOVE the ENTIRE
	// shipped global-daemon band, not just serena's own legacy ports (bot PR #247
	// P1). The shipped global servers occupy 9121–9132 today (serena 9121/9122,
	// memory 9123, sequential-thinking 9124, godbolt 9126, paper-search 9127,
	// time 9128, gdb 9129, lldb 9130, perftools 9131, wolfram 9132); they are
	// hand-assigned and grow incrementally. AllocateSerenaPort only skips ports
	// already in workspaces.yaml — it does NOT probe OS-bound or manifest-bound
	// ports — so a pool overlapping that band would hand a new workspace daemon a
	// port a global daemon already owns, and the proxy would collide on its
	// external listen (`mcphub workspace register` can run before the Phase 4
	// cutover, while every shipped global is live).
	//
	// Repo port-map convention: globals 9121–9149 (room to grow), serena dynamic
	// pool 9150–9199, LSP workspace-proxy pool 9200–9299. So start at 9150 (clears
	// 9121–9132 with a growth buffer) and end at 9199. The upstream child binds
	// external+NativeHTTPInternalPortOffset (19150+), so the upstream side never
	// collides either.
	//
	// A static band is the proportionate fix here; a future hardening could make
	// AllocateSerenaPort skip manifest-bound ports so the pool is collision-proof
	// against any port layout (tracked follow-up).
	serenaDefaultPortPoolStart = 9150
	serenaDefaultPortPoolEnd   = 9199
)

// serenaDefaultExtraArgsTemplate is the built-in default daemon_template
// extra_args_template: `--project ${workspace.path}` only. There is NO
// `--context` token here — config.ExpandWorkspacePathTokens only resolves
// ${workspace.path}, so a ${context} token would emit a literal invalid
// argument. `--context` is appended by the materializer from
// DaemonTemplate.Context (design §5 / finding #4). Returned as a fresh slice
// so callers cannot mutate the package-level default.
func serenaDefaultExtraArgsTemplate() []string {
	return []string{"--project", config.WorkspacePathToken}
}

// serenaDefaultPortPool returns a fresh *config.PortPool carrying the built-in
// default range. Fresh allocation so callers own their copy.
func serenaDefaultPortPool() *config.PortPool {
	return &config.PortPool{Start: serenaDefaultPortPoolStart, End: serenaDefaultPortPoolEnd}
}

// EffectiveSerenaDaemonTemplate returns the EFFECTIVE serena DaemonTemplate
// (port_pool, context, extra_args_template) for the dynamic-pool flow:
//
//   - If the embedded manifest m already declares a daemon_template, that
//     template wins verbatim (the embed has migrated to the dynamic-pool shape).
//   - Otherwise the built-in dynamic-pool default is returned (the embed is the
//     legacy `kind: global` shape; this is the cycle-break — the consumer does
//     NOT fail closed on the absent daemon_template.port_pool).
//
// m may be nil (treated the same as "no embed daemon_template" → built-in
// default), so a caller that cannot load the embed still gets a usable policy.
//
// The returned template is a value copy with freshly-allocated PortPool and
// ExtraArgsTemplate so the caller cannot mutate either the embed's struct or
// the package-level defaults.
//
// Design ref: docs/superpowers/specs/2026-05-29-serena-migrate-redesign-descriptor-proxy.md §5 (finding #3).
func EffectiveSerenaDaemonTemplate(m *config.ServerManifest) config.DaemonTemplate {
	if m != nil && m.DaemonTemplate != nil {
		// The embed already declares a dynamic-pool template — prefer it.
		// Defensive-copy the pointer + slice so callers own independent state.
		src := m.DaemonTemplate
		out := config.DaemonTemplate{Context: src.Context}
		if src.PortPool != nil {
			pool := *src.PortPool
			out.PortPool = &pool
		}
		if src.ExtraArgsTemplate != nil {
			out.ExtraArgsTemplate = append([]string(nil), src.ExtraArgsTemplate...)
		}
		return out
	}
	// Built-in dynamic-pool default (embed is kind: global today).
	return config.DaemonTemplate{
		Context:           serenaDefaultContext,
		PortPool:          serenaDefaultPortPool(),
		ExtraArgsTemplate: serenaDefaultExtraArgsTemplate(),
	}
}

// EffectiveSerenaPortPool is a thin convenience over
// EffectiveSerenaDaemonTemplate for the `workspace register` allocation path:
// it returns the effective port pool to allocate serena workspace daemons from.
// It NEVER fails closed on the legacy `kind: global` embed — the built-in
// default supplies the range when the embed has no daemon_template. Returns an
// error only if the effective template is internally inconsistent (a nil
// port_pool, which cannot happen for the built-in default and only arises from
// a malformed embed daemon_template that somehow reached here — the parse-time
// Validate already rejects that shape, so this is defense-in-depth).
//
// Design ref: docs/superpowers/specs/2026-05-29-serena-migrate-redesign-descriptor-proxy.md §5 (finding #3).
func EffectiveSerenaPortPool(m *config.ServerManifest) (config.PortPool, error) {
	tpl := EffectiveSerenaDaemonTemplate(m)
	if tpl.PortPool == nil {
		return config.PortPool{}, fmt.Errorf(
			"serena effective daemon_template has no port_pool — the embedded manifest declares " +
				"a daemon_template without port_pool (run `mcphub manifest edit serena` to add " +
				"daemon_template.port_pool start/end)")
	}
	return *tpl.PortPool, nil
}

// BuildInMemorySerenaDynamicPoolManifest constructs the in-memory dynamic-pool
// `*config.ServerManifest` consumed by the redesigned migrate (Phase 4) and E.2
// auto-register (Phase 5). The manifest is `kind: workspace-scoped` +
// `transport: native-http` with the EFFECTIVE daemon_template and NO top-level
// port_pool / languages / daemons. It MUST pass config.ServerManifest.Validate.
//
// The command + base_args are sourced from the embedded manifest m (e.g. `uvx`
// + the serena `start-mcp-server --transport streamable-http` launch args) so
// the synthesized manifest can actually spawn the serena child. The legacy
// per-daemon `--context` lives in the embed's daemons[].extra_args, NOT in
// base_args, so reusing base_args keeps the synthesized manifest free of a
// duplicate --context flag (the context comes solely from
// daemon_template.context and is appended at spawn — design §5).
//
// m MUST be non-nil and carry a non-empty Command — that is the catalog input
// the dynamic-pool manifest needs. A nil/command-less embed is a build error
// rather than a silent empty-command manifest.
//
// Design ref: docs/superpowers/specs/2026-05-29-serena-migrate-redesign-descriptor-proxy.md §6 (step 2).
// Plan ref:   docs/superpowers/plans/2026-05-29-serena-migrate-redesign.md Phase 2.
func BuildInMemorySerenaDynamicPoolManifest(m *config.ServerManifest) (*config.ServerManifest, error) {
	if m == nil {
		return nil, fmt.Errorf("build serena dynamic-pool manifest: embedded manifest is nil")
	}
	if m.Command == "" {
		return nil, fmt.Errorf("build serena dynamic-pool manifest: embedded manifest %q has no command", m.Name)
	}
	name := m.Name
	if name == "" {
		name = SerenaServerName
	}
	tpl := EffectiveSerenaDaemonTemplate(m)
	// Reject a blank effective context BEFORE returning (bot PR #247 P2).
	// ServerManifest.Validate does NOT check DaemonTemplate.Context, so an embed
	// that declares a daemon_template but omits/blanks context would otherwise
	// yield a "valid" manifest here and only fail later at Phase 4
	// materialization (the proxy would get --context ""). The built-in default
	// always carries a non-empty context; this guards the embed-wins branch.
	if strings.TrimSpace(tpl.Context) == "" {
		return nil, fmt.Errorf("build serena dynamic-pool manifest: effective daemon_template.context is empty (embedded manifest %q declares a daemon_template without a context); the per-workspace serena proxy materializes --context <value>, so a non-empty context is required", name)
	}
	out := &config.ServerManifest{
		Name:      name,
		Kind:      config.KindWorkspaceScoped,
		Transport: config.TransportNativeHTTP,
		Command:   m.Command,
		BaseArgs:  append([]string(nil), m.BaseArgs...),
		Env:       cloneStringMap(m.Env),
		DaemonTemplate: &config.DaemonTemplate{
			Context:           tpl.Context,
			PortPool:          tpl.PortPool,
			ExtraArgsTemplate: tpl.ExtraArgsTemplate,
		},
		// D-2 + D-3 (Tier-0) carry-through: the synthesized manifest is what the
		// install/admission gates actually see (the embed m is discarded after
		// this build). DROPPING these fields here would make an inert
		// (availability=watch / disabled-until-probe) source manifest become
		// effectively READY before InstallParsedManifest, and an unpinned
		// vendored source bypass the D-2 pin gate. Deep-copy them so out.Validate()
		// below sees the pin (D-2) and the downstream Preflight→AdmissionCheck sees
		// the inert availability (D-3). ADDITIVE: an embed with these fields nil/
		// empty (the shipped serena manifest) produces a byte-identical out.
		VendoredSource: cloneVendoredSource(m.VendoredSource),
		Availability:   m.Availability,
		InstallProbe:   cloneAvailabilityProbe(m.InstallProbe),
		// Required-secret install gate carry-through: the synthesized manifest is
		// what the install/admission gates see, so DROPPING RequiredSecrets here
		// would let a dynamic-pool source manifest with required_secrets install
		// with the gate silently lost (codex finding 3). Deep-copy the slice so out
		// is independent of the embed. ADDITIVE: a nil/empty embed (the shipped
		// serena manifest) produces a byte-identical out.
		RequiredSecrets: append([]string(nil), m.RequiredSecrets...),
	}
	// Validate the synthesized shape eagerly so a malformed embed (e.g. a
	// --context token smuggled into base_args, an empty effective context, or an
	// unpinned vendored_source / availability-typo) is caught at build time with
	// the canonical Validate error rather than surfacing later at
	// descriptor-materialization or spawn time.
	if err := out.Validate(); err != nil {
		return nil, fmt.Errorf("build serena dynamic-pool manifest: %w", err)
	}
	return out, nil
}

// cloneVendoredSource deep-copies the D-2 vendored_source descriptor for the
// serena dynamic-pool projection so the synthesized manifest carries the pin +
// license-status independently of the embed. All fields are strings, so a struct
// copy is a full deep copy; nil in → nil out (additive: a non-vendored embed
// stays non-vendored).
func cloneVendoredSource(v *config.VendoredSource) *config.VendoredSource {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

// cloneAvailabilityProbe deep-copies the D-3 install probe (its Binaries/Files
// are slices, so the slices are copied — not aliased — to keep the synthesized
// manifest fully independent of the embed). nil in → nil out.
func cloneAvailabilityProbe(p *config.AvailabilityProbe) *config.AvailabilityProbe {
	if p == nil {
		return nil
	}
	return &config.AvailabilityProbe{
		Binaries:  append([]string(nil), p.Binaries...),
		Files:     append([]string(nil), p.Files...),
		FileGlobs: append([]string(nil), p.FileGlobs...),
	}
}
