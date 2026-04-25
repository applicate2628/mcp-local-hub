// internal/gui/frontend/src/components/RotateSecretModal.tsx
import { useEffect, useRef, useState } from "preact/hooks";
import type { SecretsRotateResult } from "../lib/secrets-api";
import { rotateSecret } from "../lib/secrets-api";
// (Codex plan-R1 P2: removed unused RestartResult import — the banner
// only references types via SecretsRotateResult.restart_results.)

interface Props {
  open: boolean;
  name: string;
  // For the counter copy: total reference count and how many are running.
  // Computed by parent from snapshot + status; if not available, both
  // can be undefined and the modal omits the counts.
  // Codex PR #18 P2: null means status is known-unavailable (distinct from
  // undefined = not provided), but both are treated the same in the copy.
  refCount: number;
  runningCount?: number | null;
  onClose: () => void;
  onSaved: (result: SecretsRotateResult, mode: "no-restart" | "with-restart") => void;
}

export function RotateSecretModal(props: Props) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const [value, setValue] = useState("");
  const [working, setWorking] = useState(false);
  const [serverErr, setServerErr] = useState<string | null>(null);

  useEffect(() => {
    if (!dialogRef.current) return;
    if (props.open && !dialogRef.current.open) {
      dialogRef.current.showModal();
      setValue("");
      setServerErr(null);
    } else if (!props.open && dialogRef.current.open) {
      dialogRef.current.close();
    }
  }, [props.open]);

  const submit = async (restart: boolean) => {
    if (value === "" || working) return;
    setWorking(true);
    setServerErr(null);
    try {
      const result = await rotateSecret(props.name, value, restart);
      props.onSaved(result, restart ? "with-restart" : "no-restart");
      props.onClose();
    } catch (err) {
      setServerErr((err as Error).message);
    } finally {
      setWorking(false);
    }
  };

  // Codex PR #18 P2: null means status-unavailable; undefined means not
  // provided. Both produce the same fallback copy (no running count shown).
  const counterCopy = props.runningCount == null
    ? `${props.refCount} daemon(s) reference this key. Running-daemon count is unavailable.`
    : `${props.refCount} daemon(s) reference this key; ${props.runningCount} currently running.`;

  return (
    <dialog
      ref={dialogRef}
      onCancel={(e) => { if (working) e.preventDefault(); }}
      onClose={() => props.onClose()}
      data-testid="rotate-secret-modal"
    >
      <h2>Rotate {props.name}</h2>
      <p>{counterCopy}</p>
      <p>Stopped daemons will pick up the new value automatically on next start.</p>
      <label>
        New value
        <input type="password" value={value} onInput={(e) => setValue((e.target as HTMLInputElement).value)} disabled={working} />
      </label>
      {serverErr && <p class="error">{serverErr}</p>}
      <menu>
        <button type="button" onClick={() => props.onClose()} disabled={working}>Cancel</button>
        <button type="button" onClick={() => submit(false)} disabled={value === "" || working}>{working ? "Saving…" : "Save without restart"}</button>
        <button type="button" onClick={() => submit(true)} disabled={value === "" || working}>{working ? "Saving…" : "Save and restart"}</button>
      </menu>
    </dialog>
  );
}

export function PersistentRotateCTA(props: {
  secretName: string;
  affectedRunning: number;
  onRestart: () => Promise<void>;
  onDismiss: () => void;
}) {
  const [working, setWorking] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  if (props.affectedRunning === 0) {
    // Toast-only path; CTA suppressed (memo D4 + Codex memo-R1 P3).
    return null;
  }
  return (
    <div class="banner banner-info" data-testid="rotate-cta">
      <p>Vault updated for <code>{props.secretName}</code>. {props.affectedRunning} running daemon(s) still using the previous value.</p>
      <button
        type="button"
        disabled={working}
        onClick={async () => {
          setWorking(true);
          setErr(null);
          try {
            await props.onRestart();
            props.onDismiss();
          } catch (e) {
            setErr((e as Error).message);
          } finally {
            setWorking(false);
          }
        }}
      >
        {working ? "Restarting…" : "Restart now"}
      </button>
      <button type="button" onClick={() => props.onDismiss()}>Dismiss</button>
      {err && <p class="error">Restart failed: {err}</p>}
    </div>
  );
}

export function RotateResultBanner(props: {
  result: SecretsRotateResult | null;
  onRetry: () => Promise<void>;
  onDismiss: () => void;
}) {
  const [working, setWorking] = useState(false);
  const [retryErr, setRetryErr] = useState<string | null>(null);
  if (!props.result) return null;
  const failed = props.result.restart_results.filter((r) => r.error !== "");

  // Codex PR #18 P1: orchestration failure (500 RESTART_FAILED with
  // vault_updated:true). restart_results may be empty if Status()
  // crashed before any restart was attempted. Don't show the
  // "0 daemons restarted" success banner — surface the error and
  // offer retry.
  if (props.result.error || props.result.code === "RESTART_FAILED") {
    const partialOK = props.result.restart_results.length - failed.length;
    const partialFailed = failed.length;
    return (
      <div class="banner banner-error" data-testid="rotate-banner-orchestration-error">
        <p>
          Vault updated for the new value, but the restart sequence aborted: <code>{props.result.error ?? "RESTART_FAILED"}</code>.
        </p>
        {(partialOK > 0 || partialFailed > 0) && (
          <p>{partialOK}/{props.result.restart_results.length} daemons were restarted before the abort.{" "}
            {partialFailed > 0 && `${partialFailed} reported errors:`}
          </p>
        )}
        {partialFailed > 0 && (
          <ul>
            {failed.map((f) => <li key={f.task_name}><code>{f.task_name}</code>: {f.error}</li>)}
          </ul>
        )}
        {retryErr && <p class="error">{retryErr}</p>}
        <button
          type="button"
          disabled={working}
          onClick={async () => {
            setWorking(true);
            setRetryErr(null);
            try {
              await props.onRetry();
            } catch (e) {
              setRetryErr((e as Error).message);
            } finally {
              setWorking(false);
            }
          }}
        >
          {working ? "Retrying…" : "Retry restart"}
        </button>
        <button type="button" onClick={() => props.onDismiss()}>Dismiss</button>
      </div>
    );
  }

  if (failed.length === 0) {
    return (
      <div class="banner banner-success" data-testid="rotate-banner">
        <p>Vault updated. {props.result.restart_results.length} daemon(s) restarted.</p>
        <button type="button" onClick={() => props.onDismiss()}>Dismiss</button>
      </div>
    );
  }
  const total = props.result.restart_results.length;
  const ok = total - failed.length;
  return (
    <div class="banner banner-warn" data-testid="rotate-banner-partial">
      <p>Vault updated. {ok}/{total} daemons restarted. {failed.length} still need restart to use the new value.</p>
      <ul>
        {failed.map((f) => <li key={f.task_name}><code>{f.task_name}</code>: {f.error}</li>)}
      </ul>
      {retryErr && <p class="error">{retryErr}</p>}
      <button
        type="button"
        disabled={working}
        onClick={async () => {
          // Codex plan-R2 P1: do NOT dismiss after retry. The parent
          // calls setRotateResult with the fresh restart_results so the
          // banner re-renders with whatever still failed. If retry
          // throws (orchestration crash), surface the error inline.
          setWorking(true);
          setRetryErr(null);
          try {
            await props.onRetry();
          } catch (e) {
            setRetryErr((e as Error).message);
          } finally {
            setWorking(false);
          }
        }}
      >
        {working ? "Retrying…" : "Retry failed restarts"}
      </button>
      <button type="button" onClick={() => props.onDismiss()}>Dismiss</button>
    </div>
  );
}
