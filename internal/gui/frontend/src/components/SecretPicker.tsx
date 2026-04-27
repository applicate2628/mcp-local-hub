import { useEffect, useId, useRef, useState } from "preact/hooks";
import type { SecretsSnapshot } from "../lib/use-secrets-snapshot";
import { isSecretRef } from "../lib/secret-ref";
import { matchTier, normalizeForMatch } from "../lib/name-match";

export interface SecretPickerProps {
  value: string;
  onChange: (next: string) => void;
  envKey: string;
  snapshot: SecretsSnapshot;
  onRequestCreate: (prefillName: string | null) => void;
  disabled?: boolean;
  ariaLabel?: string;
}

const SECRET_PREFIX = "secret:";

export function SecretPicker(props: SecretPickerProps) {
  const { value, onChange, envKey, snapshot, onRequestCreate, disabled } = props;
  const [open, setOpen] = useState(false);
  const [highlightIdx, setHighlightIdx] = useState(0);

  const wrapperRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const listboxId = useId();
  const optionIdPrefix = useId();

  // Outside-click close: documenting Codex memo-R3 P2-2 (1) close mechanism.
  useEffect(() => {
    if (!open) return;
    const onMouseDown = (e: MouseEvent) => {
      if (!wrapperRef.current) return;
      if (!wrapperRef.current.contains(e.target as Node)) {
        setOpen(false);
      }
    };
    document.addEventListener("mousedown", onMouseDown);
    return () => document.removeEventListener("mousedown", onMouseDown);
  }, [open]);

  // Filter logic. Reads value (the prop) directly — single source of truth
  // per memo §5.2. For a `secret:<query>` value, strip the prefix and filter
  // by substring on normalized vault key (case-insensitive after
  // normalizeForMatch). For non-secret values, no filtering (show all).
  function getFilteredKeys(): string[] {
    if (snapshot.status !== "ok" || snapshot.data.vault_state !== "ok") return [];
    const allKeys = snapshot.data.secrets.map((s) => s.name);
    let query = "";
    if (isSecretRef(value)) {
      query = value.slice(SECRET_PREFIX.length);
    }
    const normalizedQuery = normalizeForMatch(query);
    const filtered = normalizedQuery === ""
      ? allKeys
      : allKeys.filter((k) => normalizeForMatch(k).includes(normalizedQuery));
    // Sort by matchTier(vaultKey, envKey), then alphabetically.
    return filtered.slice().sort((a, b) => {
      const ta = matchTier(a, envKey);
      const tb = matchTier(b, envKey);
      if (ta !== tb) return ta - tb;
      return a.localeCompare(b);
    });
  }

  function selectKey(key: string) {
    onChange(SECRET_PREFIX + key);
    setOpen(false);
    inputRef.current?.focus();
  }

  function onInputKeyDown(e: KeyboardEvent) {
    const filtered = getFilteredKeys();
    if (e.key === "Tab") {
      // Close on Tab without preventDefault so focus moves normally.
      // Codex memo-R3 P2-2 (2): Tab-close mechanism on input.
      if (open) setOpen(false);
      return;
    }
    if (e.key === "Escape") {
      if (open) {
        e.preventDefault();
        setOpen(false);
      }
      return;
    }
    if (e.key === "ArrowDown") {
      e.preventDefault();
      if (!open) {
        setOpen(true);
        setHighlightIdx(0);
      } else {
        setHighlightIdx((i) => filtered.length > 0 ? (i + 1) % filtered.length : 0);
      }
      return;
    }
    if (e.key === "ArrowUp") {
      e.preventDefault();
      if (!open) {
        setOpen(true);
        setHighlightIdx(Math.max(0, filtered.length - 1));
      } else {
        setHighlightIdx((i) => filtered.length > 0 ? (i - 1 + filtered.length) % filtered.length : 0);
      }
      return;
    }
    if (e.key === "Enter") {
      if (open && filtered.length > 0) {
        e.preventDefault();
        const idx = Math.min(highlightIdx, filtered.length - 1);
        selectKey(filtered[idx] ?? filtered[0]);
      }
      return;
    }
  }

  function onInputChange(e: Event) {
    const next = (e.target as HTMLInputElement).value;
    onChange(next);
    // Auto-open on `secret:` prefix typing (Q1 / D1).
    if (isSecretRef(next) && !open) {
      setOpen(true);
      setHighlightIdx(0);
    }
  }

  function onToggleClick() {
    setOpen((prev) => !prev);
    inputRef.current?.focus();
  }

  const filtered = getFilteredKeys();
  // Codex Task-4 P1: clamp highlightIdx to [0, filtered.length-1] at render
  // time. When the filter narrows (user types `secret:wolf` and only 1 item
  // remains), the raw highlightIdx state may be stale (still pointing at idx 2
  // from before the narrow). Clamping here keeps aria-activedescendant pointing
  // at a valid option and the visual `highlight` class on the right item.
  const safeHighlightIdx = filtered.length > 0
    ? Math.min(highlightIdx, filtered.length - 1)
    : 0;
  const activeOptionId = open && filtered.length > 0
    ? `${optionIdPrefix}-${safeHighlightIdx}`
    : undefined;

  return (
    <div class="secret-picker-wrap" ref={wrapperRef}>
      <input
        ref={inputRef}
        type="text"
        class="secret-picker-input"
        value={value}
        placeholder="value (literal, $HOME/..., or pick a secret)"
        role="combobox"
        aria-expanded={open}
        aria-controls={listboxId}
        aria-autocomplete="list"
        aria-activedescendant={activeOptionId}
        aria-label={props.ariaLabel ?? "Secret value picker"}
        disabled={disabled}
        onInput={onInputChange}
        onKeyDown={onInputKeyDown}
      />
      <button
        type="button"
        class={`secret-picker-toggle ${open ? "open" : ""}`}
        aria-label="Pick secret"
        aria-haspopup="listbox"
        title="Pick secret"
        disabled={disabled}
        onClick={onToggleClick}
      >
        🔑
      </button>
      {open && (
        <ul
          id={listboxId}
          class="secret-picker-dropdown"
          role="listbox"
        >
          {filtered.length === 0 && (
            <li class="dropdown-empty" role="option" aria-disabled="true" aria-selected="false">
              {snapshot.status === "ok" && snapshot.data.vault_state === "ok"
                ? "No secrets in vault yet"
                : "Vault unavailable"}
            </li>
          )}
          {filtered.map((k, idx) => {
            const id = `${optionIdPrefix}-${idx}`;
            const isExactMatch = matchTier(k, envKey) === 0;
            return (
              <li
                id={id}
                key={k}
                class={`dropdown-item ${idx === safeHighlightIdx ? "highlight" : ""}`}
                role="option"
                aria-selected={idx === safeHighlightIdx}
                onMouseDown={(e) => { e.preventDefault(); selectKey(k); }}
              >
                <span class="dropdown-key">{k}</span>
                {isExactMatch && <span class="match-badge">matches KEY name</span>}
              </li>
            );
          })}
          <li
            class="dropdown-create"
            role="presentation"
            data-action="create-new-secret"
            onMouseDown={(e) => {
              e.preventDefault();
              setOpen(false);
              onRequestCreate(null);
            }}
          >
            + Create new secret…
          </li>
        </ul>
      )}
    </div>
  );
}
