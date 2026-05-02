// internal/gui/frontend/src/components/ConfirmModal.tsx
//
// Memo D10: reusable destructive-action confirm. Used in PR #1 by:
//   - SectionBackups clean-now action
//   - SectionAdvanced kill-stuck-process flow
//
// Uses the native <dialog> element pattern (same as A3-a's
// AddSecretModal). The browser provides focus trap, Esc to cancel,
// and ARIA semantics for free; this component does NOT extract a
// useFocusTrap hook in PR #1 (memo D10 hook extraction deferred to a future PR).
import { useEffect, useRef, useState } from "preact/hooks";
import type { JSX } from "preact";

export type ConfirmModalProps = {
  open: boolean;
  title: string;
  body: JSX.Element;
  confirmLabel: string;
  danger?: boolean;
  onConfirm: () => void | Promise<void>;
  onCancel: () => void;
};

export function ConfirmModal(props: ConfirmModalProps): JSX.Element {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    const d = dialogRef.current;
    if (!d) return;
    if (props.open && !d.open) {
      d.showModal();
      setBusy(false);
    } else if (!props.open && d.open) {
      d.close();
    }
  }, [props.open]);

  async function onConfirmClick() {
    if (busy) return;
    setBusy(true);
    try {
      await props.onConfirm();
    } finally {
      setBusy(false);
    }
  }

  return (
    <dialog
      ref={dialogRef}
      class="confirm-modal"
      data-testid="confirm-modal"
      onCancel={(e) => {
        if (busy) {
          e.preventDefault();
          return;
        }
        props.onCancel();
      }}
      onClose={() => {
        // Parent's setOpen(false) drives next render; no double-fire.
      }}
    >
      <h2>{props.title}</h2>
      <div class="confirm-modal-body">{props.body}</div>
      <div class="confirm-modal-actions">
        <button
          type="button"
          onClick={() => !busy && props.onCancel()}
          disabled={busy}
          data-testid="confirm-modal-cancel"
        >
          Cancel
        </button>
        <button
          type="button"
          class={props.danger ? "danger" : ""}
          onClick={onConfirmClick}
          disabled={busy}
          data-testid="confirm-modal-confirm"
        >
          {busy ? "Working…" : props.confirmLabel}
        </button>
      </div>
    </dialog>
  );
}
