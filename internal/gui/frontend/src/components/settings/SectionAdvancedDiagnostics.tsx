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
// iota (0=Healthy..7=RaceLost), and excludes PIDCmdline entirely
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

// Verdict mirrors the JSON-tagged fields of internal/gui.Verdict.
// PIDCmdline is intentionally absent — encoding/json strips it via
// `json:"-"`. pid_subcommand carries argv[1] only and is the
// gate-relevant token for explaining a refusal without leaking the
// rest of the command line.
type Verdict = {
  class: number; // 0=Healthy, 1=LiveUnreachable, 2=DeadPID, 3=Malformed,
                 // 4=KilledRecovered, 5=KillRefused, 6=KillFailed, 7=RaceLost
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
function classLabel(v: Verdict): string {
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
        };
        setErr(`Kill failed: ${body.detail ?? body.error ?? `HTTP ${r.status}`}`);
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
    <div class="diagnostics-block" data-section="advanced-diagnostics">
      <h3>Diagnostics</h3>
      <p class="settings-section-help">
        Diagnose the single-instance lock. Read-only — does not kill anything.
      </p>
      <button
        type="button"
        onClick={() => void probe()}
        disabled={busy}
        data-testid="probe-button"
      >
        {busy ? "Probing…" : "Diagnose lock state"}
      </button>
      {verdict ? (
        <div class="verdict-strip" data-testid="verdict-strip">
          <p>{classLabel(verdict)}</p>
          <details>
            <summary>Details</summary>
            <ul>
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
          class="danger"
          onClick={() => setConfirmOpen(true)}
          data-testid="kill-button"
        >
          Kill stuck PID {verdict?.pid}
        </button>
      ) : null}
      {err ? (
        <p class="error-banner" role="alert">
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
