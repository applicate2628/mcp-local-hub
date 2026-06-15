import { useState, useId, useRef, useEffect } from "preact/hooks";
import { ALL_CLIENTS } from "../lib/routing";
import { InfoTip } from "./InfoTip";

export type MatrixColumnsMenuProps = {
  /**
   * The currently-visible client column ids (the EFFECTIVE set after
   * applying overrides). Drives each row's checkbox `checked` state.
   */
  visible: readonly string[];
  /**
   * Toggle one client's visibility. The parent owns the pref record +
   * persistence; this component just reports intent (client id, show?).
   */
  onToggle: (client: string, show: boolean) => void;
  /** Clear every override → revert to auto-detection. */
  onReset: () => void;
};

// MatrixColumnsMenu — the "Columns (N/15)" affordance beside the Servers
// matrix. Clicking opens a popover listing all 15 known clients, each a
// checkbox the operator ticks/unticks to show/hide that matrix column.
// "Reset to auto" clears the persisted overrides. This is a pure VIEW
// filter — hiding a column does not uninstall or change any binding.
//
// Native Preact + ARIA, deliberately NOT a component library (same
// rationale as InfoTip): a checkbox-list popover is simple enough to own.
// Dismissal: Escape, or click outside the wrapper. Styling rides the
// app's design tokens so it follows the live light/dark palette.
export function MatrixColumnsMenu(props: MatrixColumnsMenuProps): preact.JSX.Element {
  const { visible, onToggle, onReset } = props;
  const [open, setOpen] = useState(false);
  const rawId = useId();
  const popId = `matrix-columns-${rawId.replace(/[:]/g, "")}`;
  const wrapRef = useRef<HTMLDivElement>(null);
  const visibleSet = new Set(visible);

  // Escape + click-outside close the popover. Listeners wired only while
  // open so we never leak global handlers. mousedown (not click) so a
  // dismiss fires before a re-open click can re-toggle the trigger.
  useEffect(() => {
    if (!open) return;
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") setOpen(false);
    }
    function onDoc(e: MouseEvent) {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) {
        setOpen(false);
      }
    }
    document.addEventListener("keydown", onKey);
    document.addEventListener("mousedown", onDoc);
    return () => {
      document.removeEventListener("keydown", onKey);
      document.removeEventListener("mousedown", onDoc);
    };
  }, [open]);

  return (
    <div ref={wrapRef} class="matrix-columns-menu relative inline-flex items-center align-middle">
      <button
        type="button"
        data-testid="matrix-columns-button"
        class="matrix-columns-trigger inline-flex items-center gap-1 rounded border border-app-border bg-app-card px-2 py-1 text-xs font-medium text-app-text transition-colors hover:border-app-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-app-accent/50"
        aria-haspopup="true"
        aria-expanded={open}
        aria-controls={open ? popId : undefined}
        title="Show or hide which MCP client columns appear in the matrix"
        onClick={() => setOpen((o) => !o)}
      >
        Clients ({visibleSet.size}/{ALL_CLIENTS.length})
      </button>
      {open ? (
        <div
          id={popId}
          data-testid="matrix-columns-popover"
          role="dialog"
          aria-label="Show or hide client columns"
          class="matrix-columns-popover absolute left-0 top-[calc(100%+6px)] z-50 w-max min-w-[16rem] max-w-[20rem] rounded-lg border border-app-border bg-app-card p-3 text-left text-sm text-app-text shadow-xl"
        >
          <div class="mb-2 flex items-center justify-between gap-2">
            <span class="flex items-center gap-1 font-semibold text-app-text">
              Show / hide clients
              <InfoTip
                label="About hidden columns"
                text="Hidden columns are a view filter only — they don't uninstall anything or change any client config. Toggling a column only changes what this matrix displays."
              />
            </span>
            <button
              type="button"
              data-testid="matrix-columns-reset"
              class="matrix-columns-reset rounded border border-app-border px-2 py-0.5 text-xs text-app-muted transition-colors hover:border-app-accent hover:text-app-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-app-accent/50"
              onClick={onReset}
            >
              Reset to auto
            </button>
          </div>
          <ul class="matrix-columns-list flex flex-col gap-0.5" role="group" aria-label="Client columns">
            {ALL_CLIENTS.map((client) => {
              const checked = visibleSet.has(client);
              return (
                <li key={client}>
                  <label class="matrix-columns-item flex cursor-pointer items-center gap-2 rounded px-1 py-0.5 hover:bg-app-border/30">
                    <input
                      type="checkbox"
                      data-testid={`matrix-columns-toggle-${client}`}
                      checked={checked}
                      onChange={(ev) =>
                        onToggle(client, (ev.currentTarget as HTMLInputElement).checked)
                      }
                    />
                    <span class="text-app-text">{client}</span>
                  </label>
                </li>
              );
            })}
          </ul>
        </div>
      ) : null}
    </div>
  );
}
