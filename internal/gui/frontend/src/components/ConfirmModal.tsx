// internal/gui/frontend/src/components/ConfirmModal.tsx
//
// Memo D10: reusable destructive-action confirm. Used in PR #1 by:
//   - SectionBackups clean-now action
//   - SectionAdvanced kill-stuck-process flow
//
// Uses the native <dialog> element pattern (same as A3-a's
// AddSecretModal) for modal semantics and Escape cancellation. Keyboard
// focus wrapping and return to the invoking control are owned here.
import { useEffect, useId, useRef, useState } from "preact/hooks";
import type { JSX } from "preact";

export type ConfirmModalProps = {
  open: boolean;
  title: string;
  body: JSX.Element;
  confirmLabel: string;
  danger?: boolean;
  onConfirm: () => void | Promise<void>;
  onCancel: () => void;
  /**
   * Optional data-testid override for the dialog and its action buttons.
   * Defaults to "confirm-modal". A screen that renders more than one
   * ConfirmModal at once (e.g. BackupsList's per-row restore + delete)
   * MUST pass distinct ids so getByTestId stays unambiguous — the
   * <dialog> element is always present in the DOM regardless of `open`.
   */
  testId?: string;
};

export function ConfirmModal(props: ConfirmModalProps): JSX.Element {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const titleId = useId();
  const [busy, setBusy] = useState(false);
  const testId = props.testId ?? "confirm-modal";

  useEffect(() => {
    const d = dialogRef.current;
    if (!d) return;
    if (props.open && !d.open) {
      const invoker = document.activeElement;
      d.showModal();
      setBusy(false);
      return () => {
        if (d.open) d.close();
        if (invoker instanceof HTMLElement && invoker.isConnected && !invoker.matches(":disabled")) {
          invoker.focus();
        }
      };
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
      data-testid={testId}
      aria-labelledby={titleId}
      onKeyDown={(e) => {
        if (e.key !== "Tab" || !dialogRef.current?.open) return;
        const focusable = Array.from(dialogRef.current.querySelectorAll<HTMLElement>(
          "a[href], button, input, select, textarea, [tabindex]",
        )).filter((element) => element.tabIndex >= 0 && !element.matches(":disabled") && !element.closest("[hidden], [inert]"));
        if (focusable.length === 0) return;
        const first = focusable[0];
        const last = focusable[focusable.length - 1];
        if (e.shiftKey && document.activeElement === first) {
          e.preventDefault();
          last.focus();
        } else if (!e.shiftKey && document.activeElement === last) {
          e.preventDefault();
          first.focus();
        }
      }}
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
      <h2 id={titleId}>{props.title}</h2>
      <div class="confirm-modal-body">{props.body}</div>
      <div class="confirm-modal-actions">
        <button
          type="button"
          onClick={() => !busy && props.onCancel()}
          disabled={busy}
          data-testid={`${testId}-cancel`}
        >
          Cancel
        </button>
        <button
          type="button"
          class={props.danger ? "danger" : ""}
          onClick={onConfirmClick}
          disabled={busy}
          data-testid={`${testId}-confirm`}
        >
          {busy ? "Working…" : props.confirmLabel}
        </button>
      </div>
    </dialog>
  );
}
