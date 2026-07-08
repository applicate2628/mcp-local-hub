# Design — Native Supervised Mathcad Prime MCP Server

Status: PROPOSED (awaiting acceptance panel). Author: architect (a2170f). Date: 2026-07-08.
Target: d:/dev/mcp-local-hub. Closes: work-items/backlog/2026-06-24-mathcad-mcp-row-deferred.md.
User decision: NATIVE server (no GPL, no vendored Python fork).

## 0. Accepted facts (session-verified)
- **F1.** Mathcad Prime Automation COM is fully drivable by LATE-BINDING IDispatch. MathcadPy
  (the reference wrapper the whole ecosystem incl. the rejected GPL fork builds on) uses
  `win32com.client.Dispatch("MathcadPrime.Application")` — the exact mechanism go-ole uses.
  (MathcadPy `_application.py:23`, `:227-407`.)
- **F2.** NO SAFEARRAY / typed-enum / struct marshaling anywhere in the needed surface. Matrices
  are element-wise (`CreateMatrix(r,c)`→`SetMatrixElement`; `Rows`/`Columns`/`GetMatrixElement`).
  Units are a plain string property `.Units`. Save-options are plain ints. (`:394-406,442-452,234-336`.)
- **F3.** Value getters return a nested IDispatch value object with scalar props
  `.RealResult`/`.StringResult`/`.MatrixResult`/`.Units`/`.ErrorCode` — retrieved via go-ole
  `oleutil.GetProperty`. (`:234-336`.)
- **F4.** Hub has a first-class native-server seam: Go pkg with `Run(ctx)` serving MCP over stdio +
  hidden cobra `NewCommand()`, registered in root.go, launched `command: mcphub, base_args:[<name>]`,
  `kind: global`, `transport: stdio-bridge`. Used by gdb/lldb/godbolt/drmemory/vtune/perftools/oneapi-run.
  (`internal/godbolt/{server.go:55,cmd.go:10}`, `internal/cli/root.go:70-76`, `servers/godbolt/manifest.yaml`.)
- **F5.** Proven native-bridge-to-external-process pattern (session registry + spawn/drive/reap):
  gdb (`internal/gdb/server.go:40-80`), lldb (`internal/lldb/bridge.go`). Direct analog for "own a COM app instance."
- **F6.** stdio-bridge daemon layer (`daemon.NewStdioHost`) owns HTTP mux, restart policy, Job-Object
  orphan protection, port binding for `mcphub <subcmd>` children — server writes ZERO transport/supervision code.
  (`internal/cli/daemon.go:273-291`, `internal/daemon/host.go:25-45`.)
- **F7.** Version-agnostic Mathcad-Prime install probe EXISTS + is tested: file_globs
  `…\Mathcad Prime *\MathcadPrime.exe`; `disabled-until-probe` gate keeps the row inert (no spawn/
  no client write) until it passes, enforced in `api.AdmissionCheck`.
  (`internal/api/availability_probe_test.go:79-118`, `internal/config/manifest.go:206-242,776-800`.)
- **F8.** Hub trajectory is Python/external → Go-native-subcommand (godbolt/gdb/lldb all did it).
- **Host-verified (this session):** Mathcad Prime 11.0.1.0 installed at `C:\Program Files\PTC\Mathcad Prime 11.0.1.0\`,
  COM ProgID `MathcadPrime.Application` REGISTERED, `Ptc.MathcadPrime.Automation.dll` present. go-ole
  NOT yet a dep (MIT, cgo-free). .NET SDK 10.0.301 present (fallback path viable).

## 1. Change-Surface Contract (architect-owned; implementers CONSUME)
NEW: `internal/mathcad/` (Go pkg: MCP server + COM adapter — whole feature); `servers/mathcad/manifest.yaml`.
EDIT: `internal/cli/root.go` — ONE additive `root.AddCommand(mathcad.NewCommand())` line (seam root.go:70-76).
NEW dep: `github.com/go-ole/go-ole` (MIT, cgo-free, pure syscall).
Extension seams: S1 root.go AddCommand · S2 servers/<name>/manifest.yaml · S3 AvailabilityProbe.file_globs +
disabled-until-probe (REUSED verbatim) · S4 daemon.NewStdioHost (CONSUMED).
Protected (must-not-touch): P1 internal/daemon/* · P2 manifest.go schema (a new field = design smell →
REVISE-to-architect) · P3 api AdmissionCheck/availabilityProbePasses · P4 other server pkgs · P5
marketplace catalog.json (NO row added) · P6 no GPL source copied/vendored/derived.
Blast radius: ADDITIVE-ONLY. One leaf pkg + one manifest + one AddCommand line + one MIT dep. Zero impact
on any host without Mathcad (disabled-until-probe → fully inert). New risk owner: out-of-process COM lifecycle (§3).

## 2. CRUX — Language: Go (go-ole late-binding COM) — DECISIVE
Recommendation: build in Go, in-hub, as `internal/mathcad` native subcommand driving
`MathcadPrime.Application` via `github.com/go-ole/go-ole` late-binding IDispatch. Reject C#/.NET separate exe as primary.
Principle: reuse the hub's proven seam, unblocked by evidence (F1-F3) that the COM surface is trivially
IDispatch-drivable. The 2026-06-23 consult picked C# under an a-priori worry about Go COM marshaling
(esp. matrices/units) — EMPIRICALLY REFUTED (F1-F3: full surface via late-binding Dispatch, matrices
element-wise, units string). Tie broken by fit: Go reuses supervision/transport/probe whole + single-binary/no-runtime;
C# re-introduces the external runtime the hub is deleting (F8).
Alternatives: B. C#/.NET (official typed API, best ergonomics — but ships/supervises .NET runtime, second
toolchain, doesn't fit `command: mcphub` seam) — REJECTED as primary, RETAINED as swappable COM-helper fallback.
C. Python/uvx (GPL MathcadPy or fresh Python + runtime) — REJECTED. D. Adapt GPL fork — REJECTED (user + prior ruling).
Fallback trigger (during impl, verified by host smoke test): if go-ole `Invoke` cannot reach a required
value-object method (pure-vtable no-IDispatch — low prob per F1), OR STA+out-of-process COM deadlocks even
with a message pump and MTA doesn't resolve. Fallback = keep Go MCP/hub half; move ONLY COM marshaling into
a thin `mathcad-com-helper.exe` (C#) driven over line-delimited stdio JSON (lldb's spawn-drive pattern, F5).
Isolated behind the `comSession` interface (§3.4) so the flip is NOT a rewrite.

## 3. COM / lifecycle / threading
Mathcad Prime Automation = OUT-OF-PROCESS (LocalServer32) COM server. `Dispatch(...)` launches/attaches a
separate top-level `MathcadPrime.exe` — NOT a daemon child, NOT in its Job Object.
- **3.1 Single STA worker:** one OS thread (`runtime.LockOSThread`), `CoInitializeEx(APARTMENTTHREADED)`,
  does ALL COM calls; every MCP handler marshals onto it via channel + blocks. Serializes concurrent
  requests (NewStdioHost multiplexes several clients onto one stdio child — F6). One headless app instance/daemon.
- **3.2 Message-pump/hang risk (primary impl risk, P1):** worker runs a minimal PeekMessage/DispatchMessage
  pump between COM calls (standard go-ole desktop-automation pattern). Fallback if hangs persist: MTA +
  cross-apartment proxy. Verified only on the Mathcad host (Claim 3).
- **3.3 Crash/hang recovery (fail-loud):** COM calls not cancellable mid-flight → every request under a
  deadline; on expiry return a controlled NON-retryable MCP error (never silent hang). App-died RPC error →
  teardown + daemon exits non-zero → stdio-bridge supervisor restarts (F6). ORPHAN hygiene: MathcadPrime.exe
  is NOT a Job child, so a supervisor reap leaves it running — daemon MUST on EVERY exit path (graceful/panic/
  ctx-cancel) explicitly `CloseAll(discard)`+`Quit` AND backstop-terminate the EXACT launched PID
  (identity-gated, never a foreign/user Mathcad). Adopt-or-spawn deterministically on startup.
- **3.4 Seam:** `internal/mathcad/{server.go(Run),cmd.go(NewCommand),worker.go(STA worker),com_adapter.go
  (comSession interface + goOLESession impl),handlers.go}`. `comSession` = the swap point for the fallback.

## 4. MCP tool surface (verb_noun; explicit worksheet handles, NEVER implicit active-sheet for mutations)
`mathcad_status` {}→{available,version,app_running}. `mathcad_open_worksheet` {path}→{worksheet_id,name}.
`mathcad_list_worksheets`→[{id,name,readonly,modified}]. `mathcad_recalculate` {id}→{ok,error_count}
(Worksheet.Synchronize). `mathcad_list_inputs`/`mathcad_list_outputs` {id}→[alias]. `mathcad_get_output`
{id,alias,units?}→{type,value,units,error_code}. `mathcad_get_input`. `mathcad_set_real_input`
{id,alias,value,units?}. `mathcad_set_string_input`. `mathcad_set_matrix_input` {id,alias,rows[][],units?}
(CreateMatrix→SetMatrixElement). `mathcad_save`/`mathcad_save_as` {id[,path]}. `mathcad_export_pdf` {id,path}
(VERIFY installed-API PDF call in P5; drop with rationale if unsupported — no silent stub).
`mathcad_close_worksheet` {id,save?}. Matrices cross MCP as JSON number[][], converted element-wise (F2).
error_code/error_count surfaced verbatim (fail-transparent).

## 4a. REAL REQUIREMENTS (user, 2026-07-08 — from live near-singular-quadrature verification cases). SUPERSEDES the generic §4 surface where they conflict.
The value of this MCP is COMPUTATION, not worksheet CRUD. Number-first: **units OFF by default**
(Mathcad units get in the way of our cross-checks); **complex arithmetic MANDATORY** (kernels e^{-jkR}/R);
**fail-loud on solver non-convergence — NEVER a silent number**; **TOL reachable to the 1e-12 class**
(else useless for 10-12-digit reference checks). **Warm Prime instance** (cold start = seconds; on a sweep it kills everything).

### Mechanism (THE key implementation fact — reshapes §3.4/§4)
Prime has NO "evaluate expression" API. COM (Ptc.MathcadPrime.Automation) drives a WORKSHEET via named
input/output ALIASES. So the server ships a **template `calc.mcdx` with a fixed alias contract**
(`in_expr` string, `in_p1..pN`, `in_tol`, `out_val`, `out_err`, `out_converged`) and, per request:
SetStringValue(`in_expr`, <the expression as a Mathcad-parseable string>) + SetRealValue(`in_pk`, …) →
Synchronize (recalc) → read `out_val`/`out_err`/`out_converged`. This template is the product's core artifact.
The worksheet's math (the ∫∫, the adaptive quadrature, the convergence check) lives in the .mcdx, authored by us.

### Tool surface (computation-first; each fail-loud + per-call timeout/cancel)
CORE (without these the MCP is pointless):
- **`eval_expression`** {expr, params?, tol?} → {value (COMPLEX ok), error_estimate, warnings, converged}. units OFF.
- **`integrate_2d_region`** {integrand_expr, region: TRIANGLE as 3 coord-triples (NOT only rectangle) | rect, params, tol}
  → {value, error_estimate, converged}. Adaptive; the main tool (today's ∫∫1/R³ needed a triangle + went via solid angle
  because Wolfram's direct 2D integrator refused it).
STRONGLY DESIRED:
- **`sweep`** {integrand_expr, param_table (rows: z-ladder, frequencies), tol} → CSV in ONE call (typical = 9-point
  sweep; 9 roundtrips vs one = pain).
- **`find_root`** {expr, bracket:[a,b]} → root (resonant ka, dispersion-equation roots).
- **Special functions** available inside eval/integrand: elliptic K/E, Bessel + spherical Bessel, erf (standard set).
LOW PRIORITY (later): symbolic (Wolfram covers it); `open_worksheet` {path, inputs} → outputs (for handwritten
reference .mcdx worksheets).

### THE FEASIBILITY CRUX — gating question, must be answered by a HOST SMOKE before the full P1-P6 build
Can Prime, driven via the alias-template mechanism, actually deliver the CORE? Open unknowns to verify on the
Mathcad host FIRST (a P0.5 feasibility gate, before committing the full surface):
1. Does Mathcad's ∫ / area-integral do ADAPTIVE 2D over a TRIANGLE region to ~1e-12 for a near-singular integrand
   (1/R³ with the observer just off the plane)? (nested ∫ with variable limits, or an area-integral construct.)
2. **Does Mathcad expose CONVERGENCE, or silently return a number?** If silent, how do we fail-loud — e.g. compute
   at two TOLs and compare (Richardson/self-consistency), or read a solver error/TOL-not-met signal? This is the
   fail-loud requirement's hard part and MUST be resolved in the template's math, not assumed.
3. Complex arithmetic through the same path (e^{-jkR}/R integrands).
4. Setting an ARBITRARY expression string into a live worksheet input and having Prime PARSE+evaluate it
   (SetStringValue into a symbolic-eval alias) — confirm Prime accepts a string as a math expression, not just a value.

### ACCEPTANCE TEST (user-proposed, the go/no-go)
∫∫ 1/R³ over the unit triangle, observer at (1/3, 1/3, 1e-3) → **must return 6265.5263429603**
(exact = Ω/z solid-angle formula, Wolfram-confirmed 2026-07-08) at **TOL 1e-9**, with `converged=true`.
"If the MCP eats this, it eats everything we need." A non-converged or wrong result at this test = design not accepted.

## 5. Hub integration
Built-in native server (in-tree, like godbolt) — NOT external, NOT a vendored/marketplace row. Fixes all 3
deferral disqualifiers: no ${workspaceFolder} (kind:global subcommand, launch = `mcphub mathcad`); no absent
artifact (compiled into binary); no pending license (original Go + MIT dep, clean-room §6).
Manifest: name mathcad, kind:global, transport:stdio-bridge, command:mcphub, base_args:[mathcad],
availability:disabled-until-probe, install_probe.file_globs=[`C:\Program Files\PTC\Mathcad Prime *\MathcadPrime.exe`],
platforms:[windows/amd64], daemon default on a 91xx port (planner CONFIRMS free vs global_port_alloc.go — NOT
hardcode; 9125=GUI reserved), startup_bind_deadline_seconds:120 (slow COM cold-start), 7-client bindings.
NO vendored_source, NO required_secrets. Probe gate REUSED verbatim (no schema change). NO marketplace catalog row (P5).

## 6. Licensing
NO GPL. Original Go code; only new dep = go-ole (MIT, cgo-free). MathcadPy/GPL-fork = API-facts reference only,
never copied/vendored/derived (P6). COM method names are PTC's public interface (facts, not MathcadPy expression)
→ clean-room. Implementer works from PTC's documented COM API + §4 table, NOT by transcribing MathcadPy source.
No PTC bits redistributed (drive operator's own install). Record go-ole license in publication-safety review pre-release.

## 7. Phased plan (each falsifiable). COM phases need Mathcad host (operator has it).
- **P0.** Skeleton: internal/mathcad/{server.go,cmd.go} Run+NewCommand with only `mathcad_status` stub
  ({available:false}); servers/mathcad/manifest.yaml; one root.go AddCommand line; go-ole in go.mod.
  ACC: build+vet clean; `mcphub mathcad` serves MCP, tools/list has mathcad_status; install on non-Mathcad
  host leaves row inert (no daemon/port/client-write). No COM.
- **P1.** COM connect + STA/threading: worker.go + com_adapter.go (STA worker, CoInitializeEx, Dispatch,
  pump, deadline, teardown/PID-track); mathcad_status real {available,version,app_running}. ACC (host):
  status returns real version; exactly one headless MathcadPrime.exe launched; daemon kill leaves NO orphan.
- **P2.** open/recalc/list/close. ACC (host): open fixture .mcdx, recalc, list real aliases; concurrent calls serialized.
- **P3.** read values (scalar/string/matrix + units). ACC (host): read known scalar-w-units + matrix; match UI.
- **P4.** set inputs (real/string/matrix) + save/save_as. ACC (host): set input→recalc→dependent output changes; save round-trips.
- **P5.** export_pdf (verify installed-API call first). ACC (host): real PDF on disk matches worksheet, else absent+rationale.
- **P6.** manifest polish (port confirmed) + CLAUDE.md note + Terms + publication-safety + go-ole license record.

## 9. Claims (for architecture-reviewer)
1. Additive-only, confined to the change surface; no protected surface touched. Probe: git diff only in those paths.
2. Pure go-ole late-binding IDispatch, no SAFEARRAY/enum/struct marshaling. Probe: com_adapter.go uses only
   oleutil.CallMethod/GetProperty/PutProperty; P3/P4 host smoke reads matrix+units. Refuted by a failed oleutil.CallMethod → fallback §2.5.
3. All COM on ONE STA-locked thread + pump + per-request deadlines; concurrent serialized; hung app → controlled
   non-retryable error. Probe: P1/P2 host transcript (no re-entrancy error; deadline test returns controlled error).
4. NO manifest/config schema change; existing probe/gate reused verbatim. Probe: manifest validates under current
   ParseManifest with zero manifest.go edits. Forced schema edit = REVISE-to-architect.
5. Launched MathcadPrime.exe released/terminated on EVERY exit path; no orphan, no foreign-PID kill. Probe: P1
   host — after daemon kill the launched PID is gone; kill identity-gated (unit-tested with fake).
6. No GPL, no marketplace row; only new dep MIT go-ole; no MathcadPy-derived text. Probe: go.mod lists only
   go-ole; grep internal/mathcad no MathcadPy text; no catalog.json diff; publication-safety clean.
7. Inert on non-Mathcad host (no spawn/port/client-write). Probe: P0 install on Mathcad-free host.
