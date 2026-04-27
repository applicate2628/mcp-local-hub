// internal/gui/frontend/src/components/AddSecretModal.tsx
import { useEffect, useRef, useState } from "preact/hooks";
import { addSecret } from "../lib/secrets-api";
import { isReservedName } from "../lib/reserved-names";

interface Props {
  open: boolean;
  prefillName?: string;
  onClose: () => void;
  onSaved: () => void | Promise<void>; // A3-b §2.2: parent may return a Promise (e.g. snapshot.refresh())
}

const NAME_RE = /^[A-Za-z][A-Za-z0-9_]*$/;

export function AddSecretModal(props: Props) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const [name, setName] = useState(props.prefillName ?? "");
  const [value, setValue] = useState("");
  const [working, setWorking] = useState(false);
  const [serverErr, setServerErr] = useState<string | null>(null);

  useEffect(() => {
    if (!dialogRef.current) return;
    if (props.open && !dialogRef.current.open) {
      dialogRef.current.showModal();
      setName(props.prefillName ?? "");
      setValue("");
      setServerErr(null);
    } else if (!props.open && dialogRef.current.open) {
      dialogRef.current.close();
    }
  }, [props.open, props.prefillName]);

  const nameValid = name === "" || NAME_RE.test(name);
  const nameReserved = name !== "" && isReservedName(name);
  const canSubmit = name !== "" && value !== "" && nameValid && !nameReserved && !working;

  return (
    <dialog
      ref={dialogRef}
      onCancel={(e) => { if (working) e.preventDefault(); }}
      onClose={() => props.onClose()}
      data-testid="add-secret-modal"
    >
      <form
        method="dialog"
        onSubmit={async (e) => {
          e.preventDefault();
          if (!canSubmit) return;
          setWorking(true);
          setServerErr(null);
          try {
            await addSecret(name, value);
            // A3-b §2.2: await onSaved BEFORE closing so the parent's
            // snapshot.refresh() resolves while the modal is still open.
            // onSaved may return void (Secrets.tsx) or a Promise (AddServer).
            await props.onSaved();
            // Single-close-path: close via dialogRef only. The native
            // <dialog> onClose event will fire props.onClose() exactly
            // once. Do NOT call props.onClose() explicitly here — that
            // would double-fire (explicit + native).
            dialogRef.current?.close();
          } catch (err) {
            setServerErr((err as Error).message);
          } finally {
            setWorking(false);
          }
        }}
      >
        <h2>Add secret</h2>
        <label>
          Name
          <input
            type="text"
            value={name}
            onInput={(e) => setName((e.target as HTMLInputElement).value)}
            placeholder="OPENAI_API_KEY"
            required
            disabled={working || Boolean(props.prefillName)}
          />
        </label>
        {!nameValid && <p class="error">Must start with a letter and contain only letters, digits, or underscores.</p>}
        {nameReserved && <p class="error">'{name}' is a reserved name (HTTP routing). Choose a different name.</p>}
        <label>
          Value
          <input
            type="password"
            value={value}
            onInput={(e) => setValue((e.target as HTMLInputElement).value)}
            required
            disabled={working}
          />
        </label>
        {serverErr && <p class="error">{serverErr}</p>}
        <menu>
          <button type="button" onClick={() => dialogRef.current?.close()} disabled={working}>Cancel</button>
          <button type="submit" disabled={!canSubmit}>{working ? "Saving…" : "Save"}</button>
        </menu>
        {/* A3-b D4: Manage all secrets escape hatch — opens #/secrets in a new tab so the
            host form's dirty state stays untouched. Use a real <a target="_blank"> so screen
            readers announce it as a link, but call window.open onClick to keep the SPA
            hash routing predictable. */}
        <a
          href="#/secrets"
          onClick={(e) => {
            e.preventDefault();
            window.open("#/secrets", "_blank");
          }}
          class="modal-footer-link"
        >
          ⤴ Manage all secrets (new tab)
        </a>
      </form>
    </dialog>
  );
}
