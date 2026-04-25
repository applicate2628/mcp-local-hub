// internal/gui/frontend/src/components/DeleteSecretModal.tsx
import { useEffect, useRef, useState } from "preact/hooks";
import { deleteSecret, type APIError, type ManifestError, type UsageRef } from "../lib/secrets-api";

type Stage =
  | { kind: "closed" }
  | { kind: "deleting" }
  | { kind: "confirm-refs"; usedBy: UsageRef[] }
  | { kind: "confirm-scan-incomplete"; manifestErrors: ManifestError[] }
  | { kind: "error"; message: string };

interface Props {
  name: string | null;       // null = closed
  onClose: () => void;
  onDeleted: () => void;
}

export function DeleteSecretModal(props: Props) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const [stage, setStage] = useState<Stage>({ kind: "closed" });
  const [typed, setTyped] = useState("");

  useEffect(() => {
    if (props.name === null) {
      setStage({ kind: "closed" });
      setTyped("");
      if (dialogRef.current?.open) dialogRef.current.close();
      return;
    }
    // First call: no confirm flag. Backend decides whether to escalate.
    setStage({ kind: "deleting" });
    if (dialogRef.current && !dialogRef.current.open) {
      dialogRef.current.showModal();
    }
    void firstAttempt(props.name);
  }, [props.name]);

  async function firstAttempt(name: string) {
    try {
      await deleteSecret(name);
      props.onDeleted();
      props.onClose();
    } catch (e) {
      const err = e as APIError;
      const body = (err.body ?? {}) as { used_by?: UsageRef[]; manifest_errors?: ManifestError[] };
      switch (err.code) {
        case "SECRETS_HAS_REFS":
          setStage({ kind: "confirm-refs", usedBy: body.used_by ?? [] });
          break;
        case "SECRETS_USAGE_SCAN_INCOMPLETE":
          setStage({ kind: "confirm-scan-incomplete", manifestErrors: body.manifest_errors ?? [] });
          break;
        default:
          // Codex plan-R1 P2: 404 SECRETS_KEY_NOT_FOUND on the first call
          // means another tab/CLI just deleted this key. Treat as success
          // (the user wanted it gone) and refresh.
          if (err.status === 404) {
            props.onDeleted();
            props.onClose();
            return;
          }
          setStage({ kind: "error", message: err.message });
      }
    }
  }

  async function confirmDelete() {
    if (props.name === null) return;
    try {
      await deleteSecret(props.name, { confirm: true });
      props.onDeleted();
      props.onClose();
    } catch (e) {
      // Codex plan-R2 P1: 404 on the confirmed call also means the key
      // was just deleted by another tab/CLI; treat as success per memo
      // §5.5 ("404 if just-deleted by another caller").
      const err = e as APIError;
      if (err.status === 404) {
        props.onDeleted();
        props.onClose();
        return;
      }
      setStage({ kind: "error", message: (e as Error).message });
    }
  }

  // Block ESC while the async DELETE is in flight so the user cannot dismiss
  // the modal mid-request (same ESC guard pattern used in RotateSecretModal
  // and AddSecretModal for working states).
  const isWorking = stage.kind === "deleting";

  return (
    <dialog
      ref={dialogRef}
      onCancel={(e) => { if (isWorking) e.preventDefault(); }}
      onClose={() => props.onClose()}
      data-testid="delete-secret-modal"
    >
      {stage.kind === "deleting" && (
        <div>
          <h2>Delete {props.name}?</h2>
          <p>Deleting…</p>
        </div>
      )}
      {stage.kind === "confirm-refs" && (
        <div>
          <h2>Delete {props.name}?</h2>
          <p>Deleting <code>{props.name}</code> will leave broken references in:</p>
          <ul>
            {stage.usedBy.map((u) => (
              <li key={`${u.server}/${u.env_var}`}><code>{u.server}</code> (env: <code>{u.env_var}</code>)</li>
            ))}
          </ul>
          <p>
            Manifests will not be modified. Running daemons will not restart, but
            future installs and restarts of these servers will fail until you
            provide the secret again or remove the references.
          </p>
          <p>Type <strong>DELETE</strong> to confirm.</p>
          <input
            type="text"
            value={typed}
            onInput={(e) => setTyped((e.target as HTMLInputElement).value)}
            data-testid="delete-confirm-input"
          />
          <menu>
            <button type="button" onClick={() => props.onClose()}>Cancel</button>
            <button type="button" disabled={typed !== "DELETE"} onClick={confirmDelete}>
              Delete vault key
            </button>
          </menu>
        </div>
      )}
      {stage.kind === "confirm-scan-incomplete" && (
        <div>
          <h2>Delete {props.name}?</h2>
          <p>Some manifests couldn't be scanned. We can't verify whether <code>{props.name}</code> is referenced.</p>
          <ul>
            {stage.manifestErrors.map((e) => (
              <li key={e.path}><code>{e.path}</code>: {e.error}</li>
            ))}
          </ul>
          <p>Type <strong>DELETE</strong> to delete anyway.</p>
          <input
            type="text"
            value={typed}
            onInput={(e) => setTyped((e.target as HTMLInputElement).value)}
            data-testid="delete-confirm-input"
          />
          <menu>
            <button type="button" onClick={() => props.onClose()}>Cancel</button>
            <button type="button" disabled={typed !== "DELETE"} onClick={confirmDelete}>
              Delete anyway
            </button>
          </menu>
        </div>
      )}
      {stage.kind === "error" && (
        <div>
          <h2>Delete failed</h2>
          <p class="error">{stage.message}</p>
          <menu>
            <button type="button" onClick={() => props.onClose()}>Close</button>
          </menu>
        </div>
      )}
    </dialog>
  );
}
