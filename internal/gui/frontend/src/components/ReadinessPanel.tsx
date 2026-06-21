import type { VNode } from "preact";
import type { ReadinessReport, ReadinessRequirement } from "../api";
import { SECRET_NAME_RE, isReservedName } from "../lib/reserved-names";

const SECRET_PREFIX = "secret: ";

// secretKeyOf returns the vault key a secret requirement names, or "".
export function secretKeyOf(req: ReadinessRequirement): string {
  if (req.name.startsWith(SECRET_PREFIX)) return req.name.slice(SECRET_PREFIX.length);
  return "";
}

// isBlocker is the SINGLE source of truth for the "this requirement blocks
// install" predicate — mirroring the backend `!OK && !Optional` rule (a
// non-optional unmet requirement; an unmet optional is advisory). Both the
// panel's blocker count and a caller gating its Install button must read THIS,
// never re-derive the predicate, so the GUI gate can never drift from what the
// panel renders.
function isBlocker(req: ReadinessRequirement): boolean {
  return !req.ok && !req.optional;
}

// readinessBlockerCount returns how many requirements block install. A caller
// (e.g. the Catalog pre-install flow) disables its Install button while this is
// > 0 — the honest "you cannot install yet" UX — on the same predicate the
// panel's badge uses. Null report → 0 (nothing checked yet does not block; the
// caller gates on loading separately).
export function readinessBlockerCount(report: ReadinessReport | null): number {
  if (!report) return 0;
  return report.requirements.filter(isBlocker).length;
}

// rank orders requirements: blockers first, then unmet advisories (the inline
// secret prompts), then satisfied requirements.
function rank(req: ReadinessRequirement): number {
  if (isBlocker(req)) return 0;
  if (!req.ok && req.optional) return 1;
  return 2;
}

export interface ReadinessPanelProps {
  report: ReadinessReport | null;
  loading: boolean;
  error: string | null;
  // inlineSecrets maps a vault key → the plaintext the operator typed inline at
  // install; the parent writes these to the vault before install.
  inlineSecrets: Record<string, string>;
  onInlineSecretChange: (key: string, value: string) => void;
  // readOnly suppresses the inline secret inputs when the manifest cannot be
  // saved from this screen (an edit forced read-only by hasNestedUnknown). Save /
  // Save & Install are disabled there, so an inline value could never be
  // persisted — offering an editable field would only invite the operator to type
  // a secret that is then silently discarded on navigation (Codex #378 r6).
  readOnly?: boolean;
  // inputsDisabled locks the inline secret inputs while a Save/Save & Install is
  // in flight (busy). Editing the field mid-save would mutate inlineSecrets under
  // the running persist path (which read the pre-click map), so the new value
  // would be silently dropped or clobbered — disable input until the save settles
  // (Codex #378 r6).
  inputsDisabled?: boolean;
  // renderSecretAction, when provided, REPLACES the inline password input for an
  // inlineable unset optional `secret:` requirement with caller-supplied
  // affordances. This is the seam the Catalog pre-install flow (epic area 2)
  // uses to offer "Set <key>" (open AddSecretModal — the single owner of POST
  // /api/secrets) + "Open Secrets" (deep-link) INSTEAD of the AddServer
  // inline-write-via-parent-save model — without forking a second panel.
  // AddServer never passes it, so its inline-input behavior is unchanged
  // (inlineSecrets / onInlineSecretChange stay the persist path there).
  renderSecretAction?: (key: string, req: ReadinessRequirement) => VNode | null;
}

// ReadinessPanel renders the install-readiness report as actionable rows so the
// operator sees exactly what blocks (or merely advises) an install BEFORE it
// fails later as a cryptic HTTP-502 — and, for an unset optional `secret:` ref,
// an INLINE field to set the value right here at install (epic install-and-it-
// works, area 1).
export function ReadinessPanel(props: ReadinessPanelProps) {
  const {
    report,
    loading,
    error,
    inlineSecrets,
    onInlineSecretChange,
    readOnly,
    inputsDisabled,
    renderSecretAction,
  } = props;

  if (loading && !report) {
    return (
      <div class="readiness-panel" data-testid="readiness-panel">
        <div class="readiness-panel-loading">Checking install readiness…</div>
      </div>
    );
  }
  if (error) {
    return (
      <div class="readiness-panel readiness-panel-error" data-testid="readiness-panel" role="status">
        <span class="readiness-panel-icon" aria-hidden="true">⚠</span>
        <span>{error}</span>
      </div>
    );
  }
  if (!report) return null;

  const reqs = [...report.requirements].sort((a, b) => rank(a) - rank(b));
  const blockers = reqs.filter(isBlocker).length;
  // When the vault itself is unreadable (decrypt-failed / corrupt), the "secrets
  // vault" requirement is not ok. An inline write then CANNOT succeed: the save
  // path calls secretsInit() for any non-ok vault and init refuses pre-existing
  // unreadable vault/key files, so Save & Install would fail AFTER the UI offered
  // a field that looks like it fixes the problem. Suppress the inline entry while
  // the vault blocker is present and fall back to the guided Fix text (Codex #378 r4).
  const vaultBlocked = reqs.some((r) => r.name === "secrets vault" && !r.ok);

  return (
    <div class="readiness-panel" data-testid="readiness-panel" role="group" aria-label="Install readiness">
      <div class="readiness-panel-header">
        <span
          class={"readiness-badge " + (report.ready ? "readiness-badge-ready" : "readiness-badge-blocked")}
          data-testid="readiness-badge"
        >
          {report.ready
            ? "✓ Ready to install"
            : `✗ ${blockers} blocker${blockers === 1 ? "" : "s"} to fix`}
        </span>
      </div>
      <ul class="readiness-rows">
        {reqs.map((req) => {
          const key = secretKeyOf(req);
          // Only offer the inline field for a key the /api/secrets endpoint can
          // actually create (SECRET_NAME_RE) — a non-conforming key (e.g.
          // `foo-bar`, `123`) falls back to the guided Fix text instead of an
          // unusable input (Codex #378 r2).
          const inlineable =
            key !== "" &&
            !!req.optional &&
            !req.ok &&
            SECRET_NAME_RE.test(key) &&
            !isReservedName(key) &&
            !vaultBlocked &&
            !readOnly;
          const cls = req.ok
            ? "readiness-row-ok"
            : req.optional
              ? "readiness-row-advisory"
              : "readiness-row-blocker";
          const icon = req.ok ? "✓" : req.optional ? "⚠" : "✗";
          return (
            <li
              class={"readiness-row " + cls}
              key={req.name}
              data-testid={"readiness-row-" + req.name}
            >
              <span class="readiness-row-icon" aria-hidden="true">{icon}</span>
              <div class="readiness-row-body">
                <span class="readiness-row-name">{req.name}</span>
                {req.reason ? <span class="readiness-row-reason">{req.reason}</span> : null}
                {inlineable && renderSecretAction ? (
                  // Catalog pre-install flow (epic area 2): the caller supplies
                  // the affordance — "Set <key>" (AddSecretModal) + "Open Secrets"
                  // deep-link — instead of the inline-write input below.
                  renderSecretAction(key, req)
                ) : inlineable ? (
                  <label class="readiness-secret-inline">
                    <span class="readiness-secret-inline-label">Set {key} now:</span>
                    <input
                      type="password"
                      class="readiness-secret-inline-input"
                      data-testid={"readiness-secret-input-" + key}
                      placeholder={`enter ${key}…`}
                      value={inlineSecrets[key] ?? ""}
                      disabled={inputsDisabled}
                      onInput={(e) =>
                        onInlineSecretChange(key, (e.target as HTMLInputElement).value)
                      }
                      autoComplete="off"
                    />
                  </label>
                ) : req.fix && !req.ok ? (
                  <span class="readiness-row-fix">{req.fix}</span>
                ) : null}
              </div>
            </li>
          );
        })}
      </ul>
    </div>
  );
}
