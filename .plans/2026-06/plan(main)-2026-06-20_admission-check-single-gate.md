# AdmissionCheck — unify the 4 install-admission spellings (implementation plan)

> ADR: `work-items/decisions/2026-06-19-admission-check-single-gate.md` (accepted).
> Map: 4-spelling MCP workflow `wf_35df8247-c78` (Preflight / BuildPlanWithOpts / validateDynamicPoolManifest / CheckServerReadiness) — full per-check inventory with file:line, blocking/optional, scope, and the ADR in-scope verdict.
> Goal: ONE pure owner of "will this install be admitted" so the bot's divergence-finding surface (PR #377 r12–r18, #378) collapses by construction, enforced by a corpus equivalence test.

## The shape

```go
type AdmissionScope struct {
    DaemonFilter         string   // "" = whole server; else one daemon (must match BuildPlan's filter)
    ClientsInclude       []string // explicit --clients; nil = default/override path
    DefaultClientsOverride []string // operator persisted default; nil = compile-time trio
    IncludeAllClients    bool
    // Readiness/GUI callers pass the zero scope (DaemonFilter "", default clients).
}

type AdmissionFinding struct {
    ID       string // stable, e.g. "command-on-path", "external-port-free"
    Name     string // human row label (mirrors ReadinessRequirement.Name)
    Reason   string
    Fix      string
    Optional bool   // advisory — does NOT block admission
}

// AdmissionCheck is the SINGLE owner of the pre-spawn install-admission decision.
// Pure + side-effect-free EXCEPT the same port/vault/PATH probes the current gates
// already perform (read-only environment probes, no state mutation). Appends a
// finding per failed check instead of returning early.
func AdmissionCheck(m *config.ServerManifest, scope AdmissionScope) []AdmissionFinding
```

## Union of checks (from the map — each becomes an append, in this order)

Order preserves Preflight's fail-fast sequence so the adapter's first-error is byte-identical to today.

**A. remote-http short-circuit** (when `m.Transport==remote-http`): canonical-mcphub-present (blocking) → client-predicate (mutual-exclusive + unknown-client-name, blocking) → expand-url-secret (blocking) → expand-headers-secret (blocking) → per in-scope binding: binding-names-daemon, off-matrix-client (`isRemoteHTTPCapableClient`), client-config-path. Then STOP (remote has no daemons/ports/launcher). [Preflight does only canonical-mcphub on this branch today; the secret-expansion + off-matrix REJECTIONS are BuildPlan-owned — union must include them.]

**B. local/dynamic-pool path** (everything else):
1. launcher: command-on-path (blocking, but Optional when `Kind==workspace-scoped && DaemonTemplate==nil` — per-language LSP shape) + LauncherGuidance fix.
2. runtime-behind-launcher (npx/npm→node; inherits launcherOptional).
3. server-level required_binaries (blocking) + per-language required_binaries (Optional).
4. git-for-uvx-git-source (when `manifestNeedsGit`).
5. lldb-bridge sub-case (stdio-bridge + base_args[0]=="lldb-bridge"): address-valid (blocking) → listener-or-binary (blocking).
6. entry-script-present (node/python base_args[0]; skip unresolvable-relative as Optional; skip whole gate when scriptOptional = workspace-scoped legacy-language shape).
7. canonical-mcphub-present (blocking, always).
8. **dynamic-pool template gates** (`validateDynamicPoolManifest`, when DaemonTemplate!=nil): transport-native-http, context-nonempty, no-duplicate-context-flag (all blocking, workspace scope). [Call the existing function — keep it the single owner; do NOT re-spell.]
9. **port gates** — per in-filter daemon (skipped naturally when `m.Daemons` empty = dynamic-pool): external-port-range, external-port-free, native-http-internal-range, native-http-internal-free. PLUS the **pool free-port readiness** (`checkPool`, registry-aware) for `DaemonTemplate.PortPool`/`m.PortPool` — THE KEY GAP: today this lives ONLY in CheckServerReadiness, NOT on the install path. AdmissionCheck includes it so both gates agree; whether install-time blocks on it is the convergence decision (see §port-collapse).
10. secret-vault-readable-when-refs-used (blocking only if `HasSecretRef(m.Env)` && vault unreadable).
11. no-file-env-refs (blocking).
12. **client-scope checks** (`installClientPredicate`-equivalent, from BuildPlan): predicate-mutual-exclusive, unknown-client-name (incl. persisted default override), per-binding: unknown-daemon, client-config-path, url-path-invalid. [BuildPlan-owned today; readiness validates a SUBSET — union must include them so readiness ⟺ preflight.]

## Adapters

```go
func Preflight(m *config.ServerManifest, daemonFilter string) error {
    for _, f := range AdmissionCheck(m, scopeForPreflight(daemonFilter)) {
        if !f.Optional { return errors.New(f.Reason) } // identical first-error fail-fast
    }
    return nil
}
```
- `CheckServerReadiness(m)`: `rep.Ready = !slices.ContainsFunc(AdmissionCheck(m, zeroScope), func(f) bool { return !f.Optional })`. The rich per-key `ReadinessRequirement` rows (inline secret-prompt UX, the #378 work) stay LAYERED on top — map each AdmissionFinding to a requirement row + keep the readiness-only advisory rows (the GUI rendering is additive, not the admission decision).

## Port-probe collapse (the ADR consequence)

Collapse the `fixedPortStatus` bind-probe (readiness) vs `preflightPortInUse` dial + `portHeldByOurDaemonForPortArm` (Preflight) fork into ONE canonical check inside AdmissionCheck: bind-probe + `portHeldByOurDaemonForPortArm`. Migrate the Preflight collision tests that mock the dial to the bind probe at that time. The dynamic-pool `checkPool` (registry-aware) becomes the pool branch.

## Corpus equivalence test (the by-construction guard)

`Preflight(m,"") == nil  ⟺  CheckServerReadiness(m).Ready` over a manifest corpus (every `servers/*/manifest.yaml` + synthetic edge manifests: missing binary, bad port, missing secret, remote-secret, dynamic-pool, companion-excluded). CI-enforced. Shared predicates alone can't guarantee it; this can.

## In-scope criterion (documented in the CheckServerReadiness package doc)

A check belongs in the gate iff ALL: (a) pre-spawn, (b) state-committing (failure = false install committing client+supervisor state for a guaranteed-dead daemon), (c) mcphub-fixable. Downstream-of-`cmd.Start` (registry outage, wheel build, server auth/runtime) is OUT. Known-tolerated non-convergences (advisory): relative `base_args[0]` under non-absolute daemon cwd; the readiness bind-probe vs Preflight dial+intent superset.

> NOTE on the map's `inScopePerADR` interpretation: the readers split on whether "external-dependency-presence" (command/runtime/required-binaries/git/entry-script) is mcphub-fixable. The ADR boundary is **pre-spawn AND state-committing AND mcphub-fixable** — external-dependency gates ARE pre-spawn + state-committing, and the FIX is operator-actionable (LauncherGuidance names the exact install command), so they stay IN the gate as BLOCKING findings. "mcphub-fixable" = an actionable Fix exists, not "mcphub installs it for you". Resolve this explicitly in the implementation PR.

## Implementation steps (single dedicated PR, after #378 merges)

1. `AdmissionFinding` + `AdmissionScope` types + `AdmissionCheck` skeleton (this branch — SAFE, no behavior change yet).
2. Move each Preflight check body into AdmissionCheck as an append (preserve order); `Preflight` becomes the adapter. Run the FULL existing Preflight test suite unchanged (first-error parity).
3. Fold the BuildPlan client-scope + remote-secret + off-matrix rejections into AdmissionCheck (the readiness-side gap).
4. Wire `CheckServerReadiness.Ready` to AdmissionCheck; keep the rich requirement rows.
5. Collapse the port-probe fork; migrate the dial-mock collision tests to the bind probe.
6. Add the corpus equivalence test.
7. Update the CheckServerReadiness package doc with the in-scope criterion + known-tolerated non-convergences.

## Risk

Critical install gate incl. supervisor-intent port-collision logic. Mitigations: order-preserving extraction (step 2 is mechanical + the existing Preflight tests are the parity oracle); the corpus test is the regression net; land as ONE reviewed PR, not bolted onto a converging PR (the ADR's explicit sequencing).
