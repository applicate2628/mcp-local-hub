// internal/gui/frontend/src/components/settings/SectionAdvancedDiagnostics.tsx
//
// Memo D12 + D13: two-click force-kill flow inside SectionAdvanced.
// First click runs Probe (read-only); if the lock holder is "Stuck"
// (live but not responding) AND the identity gate passes (D12
// index-safe + clock-aware), a "Kill stuck PID N" button appears that
// opens ConfirmModal → POST /api/force-kill.
//
// IMPORTANT — wire-shape bridge translation:
// The plan's reference Verdict shape (PascalCase keys, "Stuck" string
// class, PIDCmdline:[]string) is fictional. The real wire shape from
// /api/force-kill/probe (handler in internal/gui/force_kill.go) is
// what encoding/json marshals from internal/gui/single_instance.go's
// Verdict struct. That shape is snake_case, uses numeric VerdictClass
// iota (0=Healthy..8=Indeterminate), and excludes PIDCmdline entirely
// (json:"-" because argv may carry secrets like
// `mcphub secrets set --value <SECRET>`). Only pid_subcommand —
// argv[1] — is exposed.
//
// Translation of the memo D12 invariant:
//   "Stuck" predicate  — pid_alive===true && ping_match===false
//                        (== VerdictLiveUnreachable, the only kill-eligible class)
//   cmdline guard      — pid_subcommand === "gui" || pid_subcommand === ""
//                        (empty subcmd corresponds to len(argv) <= 1, i.e.
//                        Explorer/Start-menu launches that default to gui
//                        via cmd/mcphub/main.go:32)
//   clock semantics    — pid_start < mtime, strict, fail-closed on
//                        equality or missing fields (memo D12 verbatim)
//
// The server re-enforces every check via C1's KillRecordedHolder
// three-part identity gate. The client check is UX-only — it gates
// whether the Kill button renders, not whether the kill is allowed.
import { useState } from "preact/hooks";
import { ConfirmModal } from "../ConfirmModal";
import { InfoTip } from "../InfoTip";

// Verdict mirrors the JSON-tagged fields of internal/gui.Verdict.
// PIDCmdline is intentionally absent — encoding/json strips it via
// `json:"-"`. pid_subcommand carries argv[1] only and is the
// gate-relevant token for explaining a refusal without leaking the
// rest of the command line.
type Verdict = {
  class: number; // 0=Healthy, 1=LiveUnreachable, 2=DeadPID, 3=Malformed,
                 // 4=KilledRecovered, 5=KillRefused, 6=KillFailed, 7=RaceLost,
                 // 8=Indeterminate
  pid?: number;
  port?: number;
  mtime?: string;
  pid_alive?: boolean;
  pid_image?: string;
  pid_subcommand?: string;
  pid_start?: string;
  ping_match?: boolean;
};

const MCPHUB_BASENAMES = new Set(["mcphub.exe", "mcphub"]);

// VERDICT_INDETERMINATE is single_instance.go's VerdictIndeterminate (appended
// LAST in the iota precisely so the wire numbers of already-shipped classes
// never shift). It marks an identity probe that hit an ambiguous PLATFORM
// error which is NOT the platform's own "no such process" signal.
//
// It is the ONE class whose (pid_alive, ping_match) tuple does NOT summarize
// it: the server force-sets pid_alive:false because it has no liveness fact to
// report, NOT because the holder is dead. classLabel must therefore branch on
// the numeric class BEFORE that tuple — see its comment.
const VERDICT_INDETERMINATE = 8;

// canKill applies the memo-D12 identity gate client-side.
//
// All four predicates MUST hold:
//   1. "Stuck" predicate: pid_alive===true && ping_match===false.
//      Equivalent to VerdictLiveUnreachable (class:1) — the only
//      kill-eligible state. We do NOT kill on DeadPID (vacant; a
//      stale-lock cleanup, not a kill), Malformed (parse error),
//      Healthy (no kill needed), or any post-kill class.
//   2. Image basename ∈ {mcphub.exe, mcphub} (case-insensitive).
//   3. pid_subcommand guard: subcmd === "gui" || subcmd === "".
//   4. Clock semantics: pid_start strictly less than mtime; missing
//      fields fail closed.
function canKill(v: Verdict | null): boolean {
  if (!v) return false;

  // (1) Stuck predicate against the actual wire shape.
  if (!(v.pid_alive === true && v.ping_match === false)) return false;

  // (2) Image basename check (case-insensitive on Windows).
  const image = (v.pid_image ?? "").replaceAll("\\", "/");
  const base = image.split("/").pop()?.toLowerCase() ?? "";
  if (!MCPHUB_BASENAMES.has(base)) return false;

  // (3) Memo D12 cmdline guard, translated to wire-available
  //     pid_subcommand (full PIDCmdline is json:"-" for security).
  //     Empty subcmd corresponds to Explorer/Start-menu launch
  //     (cmd/mcphub/main.go:32 defaults to "gui"); explicit "gui"
  //     subcommand also passes. Non-empty other values fail closed.
  const subcmd = v.pid_subcommand ?? "";
  if (!(subcmd === "gui" || subcmd === "")) return false;

  // (4) Memo D12 clock semantics — strict <, fail-closed on
  //     equality or missing.
  if (!v.pid_start || !v.mtime) return false;
  if (new Date(v.pid_start).getTime() >= new Date(v.mtime).getTime()) return false;

  return true;
}

// classLabel renders the verdict description from the wire shape.
// We branch on the (pid_alive, ping_match) tuple rather than the
// numeric class field because the strip's user-visible state map
// (Healthy / Stuck / Vacant / Mismatched) is observable in those two
// booleans without re-encoding the iota. The numeric class remains
// the canonical source for the kill-eligibility decision in canKill.
//
// Indeterminate is the ONE exception to that tuple rule and MUST be checked
// first (review finding). The server sets pid_alive:false on that class
// because the probe produced NO liveness fact, so the tuple rule rendered
// "Vacant — lock file present but no live holder" — the exact opposite of the
// class's defining guarantee that the ambiguous platform error is NOT proof of
// death. Telling an operator the lock is vacant when a live GUI may still hold
// it invites them to force a second instance.
function classLabel(v: Verdict): string {
  if (v.class === VERDICT_INDETERMINATE) {
    return `Indeterminate — could not determine whether PID ${v.pid ?? "?"} is alive (a transient platform error, not a confirmed exit). Retry, or check the PID with Task Manager / ps.`;
  }
  if (v.pid_alive === true && v.ping_match === true) {
    return "Healthy — lock holder alive and responding.";
  }
  if (v.pid_alive === true && v.ping_match === false) {
    return `Stuck — lock held by PID ${v.pid ?? "?"} (${v.pid_image ?? "?"}).`;
  }
  if (v.pid_alive === false) {
    return "Vacant — lock file present but no live holder.";
  }
  // Defensive fallthrough — pid_alive undefined. canKill will refuse,
  // but the strip still describes the state.
  return `Mismatched — lock holder image is not mcphub (${v.pid_image ?? "?"}).`;
}

export function SectionAdvancedDiagnostics(): preact.JSX.Element {
  const [verdict, setVerdict] = useState<Verdict | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [killBusy, setKillBusy] = useState(false);

  async function probe() {
    setBusy(true);
    setErr(null);
    try {
      const r = await fetch("/api/force-kill/probe", { method: "POST" });
      if (r.status === 501) {
        // macOS short-circuit (memo D13). Server returns
        // {error:"not_supported_on_macos", detail:"…"}.
        const j = await r.json().catch(() => ({}));
        setErr((j as { detail?: string }).detail ?? "Not supported on this platform.");
        setVerdict(null);
        return;
      }
      if (!r.ok) {
        setErr(`Probe failed: HTTP ${r.status}`);
        return;
      }
      const v = (await r.json()) as Verdict;
      setVerdict(v);
    } catch (e: unknown) {
      const msg = e instanceof Error ? e.message : String(e);
      setErr(msg);
    } finally {
      setBusy(false);
    }
  }

  async function doKill() {
    if (killBusy) return;
    setKillBusy(true);
    try {
      const r = await fetch("/api/force-kill", { method: "POST" });
      if (r.ok) {
        const v = (await r.json()) as Verdict;
        setVerdict(v);
      } else {
        const body = (await r.json().catch(() => ({}))) as {
          detail?: string;
          error?: string;
          verdict?: Verdict;
        };
        if (body.error === "liveness_indeterminate") {
          // 503 from force_kill.go: the kill was SKIPPED because liveness
          // could not be established. Saying "Kill failed" would imply a
          // kill was attempted; refresh the strip from the returned verdict
          // so the operator sees the Indeterminate label and its retry
          // guidance instead.
          if (body.verdict) setVerdict(body.verdict);
          setErr("Kill skipped — could not determine whether the lock holder is alive. Retry, or check the PID with Task Manager / ps.");
        } else {
          setErr(`Kill failed: ${body.detail ?? body.error ?? `HTTP ${r.status}`}`);
        }
      }
    } catch (e: unknown) {
      const msg = e instanceof Error ? e.message : String(e);
      setErr(msg);
    } finally {
      setKillBusy(false);
      setConfirmOpen(false);
    }
  }

  const showKill = canKill(verdict);

  return (
    <div data-section="advanced-diagnostics">
      <header class="mb-2 flex items-center gap-1.5">
        <h3 class="m-0 text-xs font-semibold uppercase tracking-wide text-app-muted">Diagnostics</h3>
        <InfoTip text="Diagnose the single-instance lock. Read-only — does not kill anything." />
      </header>
      <button
        type="button"
        onClick={() => void probe()}
        disabled={busy}
        data-testid="probe-button"
      >
        {busy ? "Probing…" : "Diagnose lock state"}
      </button>
      {verdict ? (
        <div class="mt-3 rounded-lg border border-app-border/60 p-3 text-sm text-app-text" data-testid="verdict-strip">
          <p class="m-0">{classLabel(verdict)}</p>
          <details class="mt-2">
            <summary class="cursor-pointer text-xs text-app-muted">Details</summary>
            <ul class="mt-1 list-disc pl-5 text-xs text-app-muted">
              <li>PID: {verdict.pid ?? "?"}</li>
              <li>Port: {verdict.port ?? "?"}</li>
              <li>Image: {verdict.pid_image ?? "?"}</li>
              <li>Subcommand: {verdict.pid_subcommand ?? ""}</li>
              <li>Start: {verdict.pid_start ?? "?"}</li>
              <li>Lock mtime: {verdict.mtime ?? "?"}</li>
              <li>Ping match: {String(verdict.ping_match ?? false)}</li>
            </ul>
          </details>
        </div>
      ) : null}
      {showKill ? (
        <button
          type="button"
          class="btn-danger mt-3"
          onClick={() => setConfirmOpen(true)}
          data-testid="kill-button"
        >
          Kill stuck PID {verdict?.pid}
        </button>
      ) : null}
      {err ? (
        <p class="mt-2 text-sm text-app-danger" role="alert">
          {err}
        </p>
      ) : null}

      <ConfirmModal
        open={confirmOpen}
        title="Kill stuck mcphub process?"
        body={
          <>
            PID <b>{verdict?.pid}</b> ({verdict?.pid_image}, started{" "}
            {verdict?.pid_start}). The process will be terminated immediately.
          </>
        }
        confirmLabel={`Kill PID ${verdict?.pid ?? ""}`}
        danger
        onConfirm={doKill}
        onCancel={() => setConfirmOpen(false)}
      />
    </div>
  );
}
