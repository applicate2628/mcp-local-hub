import { useEffect, useId, useMemo, useRef, useState } from "preact/hooks";
import type { SecretsSnapshot } from "../lib/use-secrets-snapshot";
import { hasSecretKey, isSecretRef } from "../lib/secret-ref";
import { matchTier, normalizeForMatch } from "../lib/name-match";
import { isReservedName, isValidSecretName } from "../lib/reserved-names";

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

type RefState =
  | "literal"
  | "present"
  | "missing"
  | "unverified"
  | "loading";

function classifyRefState(value: string, snapshot: SecretsSnapshot, isEditingQuery: boolean): RefState {
  if (!isSecretRef(value) || !hasSecretKey(value)) return "literal";
  const key = value.slice(SECRET_PREFIX.length);

  // Codex plan-R3 P1: suppress missing/unverified marker ONLY while the user
  // is actively editing a search query in the picker — NOT for the entire
  // duration the picker is open. If the user opened the picker on an
  // existing committed value (e.g., #/edit-server loaded a manifest with
  // secret:never_added and the user clicked 🔑), the marker MUST stay
  // visible because the value hasn't been touched. Caller computes
  // isEditingQuery = (valueAtOpen !== null && value !== valueAtOpen).
  if (isEditingQuery) return "literal";

  if (snapshot.status === "loading" && snapshot.fetchedAt === null) return "loading";
  if (snapshot.status === "error") return "unverified";
  if (snapshot.data.vault_state !== "ok") return "unverified";

  const found = snapshot.data.secrets.some((s) => s.name === key && s.state === "present");
  return found ? "present" : "missing";
}

function ageString(fetchedAtMs: number): string {
  const seconds = Math.floor((Date.now() - fetchedAtMs) / 1000);
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes} min ago`;
  const hours = Math.floor(minutes / 60);
  return `${hours}h ago`;
}

// DropdownItem is the unified discriminated union that drives BOTH render and
// keyboard navigation. Selectable items have `selectable: true`; section
// labels / dividers / disabled rows have `selectable: false` so keyboard
// nav skips them.
type DropdownItem =
  | { kind: "section-label"; text: string; selectable: false }
  | { kind: "divider"; selectable: false }
  | { kind: "key"; key: string; isMatch: boolean; isLegacyReserved: boolean; isCurrentRef?: "missing" | "unverified"; selectable: true; onSelect: () => void }
  | { kind: "create-contextual"; targetKey: string; selectable: true; onSelect: () => void }
  | { kind: "create-contextual-disabled"; targetKey: string; reason: string; selectable: false }
  | { kind: "create-generic"; selectable: true; onSelect: () => void }
  | { kind: "create-generic-disabled"; reason: string; selectable: false }
  | { kind: "retry"; selectable: true; onSelect: () => void }
  | { kind: "loading"; selectable: false }
  | { kind: "empty"; text: string; selectable: false };

const STALE_THRESHOLD_MS = 30_000;

export function SecretPicker(props: SecretPickerProps) {
  const { value, onChange, envKey, snapshot, onRequestCreate, disabled } = props;
  const [open, setOpen] = useState(false);
  const [highlightIdx, setHighlightIdx] = useState(0);
  // valueAtOpen captures the value as it was when the picker opened.
  // Codex plan-R3 P1: classifyRefState uses (value !== valueAtOpen) to
  // detect "actively editing query" and suppress the missing/unverified
  // marker only during typing — NOT for the entire open lifetime.
  const [valueAtOpen, setValueAtOpen] = useState<string | null>(null);
  const wrapperRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const listboxId = useId();
  const optionIdPrefix = useId();
  const statusId = useId();
  const lastOpenRefreshedRef = useRef(false);

  const isEditingQuery = open && valueAtOpen !== null && value !== valueAtOpen;
  const refState = useMemo(() => classifyRefState(value, snapshot, isEditingQuery), [value, snapshot, isEditingQuery]);

  function openPicker(captureValue: string = value) {
    if (!open) {
      // Codex Task-5 quality P2: callers in event handlers may pass `next`
      // (the value the user just typed) so valueAtOpen reflects what's
      // actually about to be the value, not the stale prop.
      setValueAtOpen(captureValue);
      setOpen(true);
    }
  }
  function closePicker() {
    // Codex Task-5 quality P2: use functional updater so we don't rely on
    // the captured `open` from this render's closure (which could be stale
    // if a re-render happened between event registration and dispatch).
    setOpen((wasOpen) => {
      if (wasOpen) {
        setValueAtOpen(null);
      }
      return false;
    });
  }
  function togglePicker() {
    if (open) closePicker();
    else openPicker();
  }

  useEffect(() => {
    if (!open) return;
    const onMouseDown = (e: MouseEvent) => {
      if (!wrapperRef.current) return;
      if (!wrapperRef.current.contains(e.target as Node)) {
        closePicker();
      }
    };
    document.addEventListener("mousedown", onMouseDown);
    return () => document.removeEventListener("mousedown", onMouseDown);
  }, [open]);

  useEffect(() => {
    if (!open) {
      lastOpenRefreshedRef.current = false;
      return;
    }
    if (lastOpenRefreshedRef.current) return;
    if (snapshot.status !== "ok") return;
    if (snapshot.fetchedAt == null) return;
    if (Date.now() - snapshot.fetchedAt <= STALE_THRESHOLD_MS) return;
    lastOpenRefreshedRef.current = true;
    void snapshot.refresh();
  }, [open, snapshot]);

  const canCreate = snapshot.status === "ok" && snapshot.data.vault_state === "ok";
  const currentSecretKey = isSecretRef(value) && hasSecretKey(value) ? value.slice(SECRET_PREFIX.length) : null;
  // Codex PR #19 P1: also gate on isValidSecretName — backend NAME_RE
  // would reject creation, and AddSecretModal locks the name field when
  // prefilled, leaving Save permanently disabled (dead-end UX). Surface
  // the disabled state in the dropdown instead.
  const canCreateContextual = (k: string) => canCreate && !isReservedName(k) && isValidSecretName(k);

  function getFilteredKeys(): string[] {
    if (snapshot.status !== "ok" || snapshot.data.vault_state !== "ok") return [];
    // Codex PR #19 P2: filter to state="present" only. The backend may
    // include rows with state="referenced_missing" (vault key absent but
    // referenced from a manifest) — selecting one would write a broken
    // secret:<key> reference that fails at install/start time.
    const allKeys = snapshot.data.secrets.filter((s) => s.state === "present").map((s) => s.name);
    let query = "";
    if (isSecretRef(value)) {
      query = value.slice(SECRET_PREFIX.length);
    }
    const normalizedQuery = normalizeForMatch(query);
    const filtered = normalizedQuery === ""
      ? allKeys
      : allKeys.filter((k) => normalizeForMatch(k).includes(normalizedQuery));
    return filtered.slice().sort((a, b) => {
      const ta = matchTier(a, envKey);
      const tb = matchTier(b, envKey);
      if (ta !== tb) return ta - tb;
      return a.localeCompare(b);
    });
  }

  function selectKey(key: string) {
    onChange(SECRET_PREFIX + key);
    closePicker();
    inputRef.current?.focus();
  }

  function buildItems(): DropdownItem[] {
    const items: DropdownItem[] = [];
    const isInitialLoading = snapshot.status === "loading" && snapshot.fetchedAt === null;
    const isError = snapshot.status === "error";
    const isVaultNotOk = snapshot.status === "ok" && snapshot.data.vault_state !== "ok";
    const isOk = snapshot.status === "ok" && snapshot.data.vault_state === "ok";

    if (isInitialLoading) {
      items.push({ kind: "loading", selectable: false });
      return items;
    }
    if (isError) {
      items.push({
        kind: "retry",
        selectable: true,
        onSelect: () => { void snapshot.refresh(); },
      });
      return items;
    }
    if (isVaultNotOk && refState === "unverified" && currentSecretKey != null) {
      items.push({ kind: "section-label", text: "Currently referenced", selectable: false });
      items.push({
        kind: "key",
        key: currentSecretKey,
        isMatch: false,
        isLegacyReserved: false,
        isCurrentRef: "unverified",
        selectable: true,
        onSelect: () => selectKey(currentSecretKey),
      });
    }
    if (isVaultNotOk) {
      items.push({
        kind: "create-generic-disabled",
        reason: snapshot.status === "ok" ? `Vault ${snapshot.data.vault_state} — fix on Secrets screen first` : "",
        selectable: false,
      });
      return items;
    }
    if (isOk && refState === "missing" && currentSecretKey != null) {
      items.push({ kind: "section-label", text: "Currently referenced", selectable: false });
      items.push({
        kind: "key",
        key: currentSecretKey,
        isMatch: false,
        isLegacyReserved: isReservedName(currentSecretKey),
        isCurrentRef: "missing",
        selectable: true,
        onSelect: () => selectKey(currentSecretKey),
      });
      if (canCreateContextual(currentSecretKey)) {
        items.push({
          kind: "create-contextual",
          targetKey: currentSecretKey,
          selectable: true,
          onSelect: () => {
            closePicker();
            onRequestCreate(currentSecretKey);
          },
        });
      } else {
        items.push({
          kind: "create-contextual-disabled",
          targetKey: currentSecretKey,
          reason: isReservedName(currentSecretKey)
            ? `Cannot create — '${currentSecretKey}' is a reserved name (HTTP routing). Choose a different name.`
            : !isValidSecretName(currentSecretKey)
              ? `Cannot create — '${currentSecretKey}' is not a valid secret name. Names must start with a letter and contain only letters, digits, or underscores.`
              : "Cannot create",
          selectable: false,
        });
      }
      items.push({ kind: "divider", selectable: false });
    }
    if (isOk) {
      const filtered = getFilteredKeys();
      if (filtered.length > 0) {
        items.push({ kind: "section-label", text: "Available secrets", selectable: false });
        for (const k of filtered) {
          items.push({
            kind: "key",
            key: k,
            isMatch: matchTier(k, envKey) === 0,
            isLegacyReserved: isReservedName(k),
            selectable: true,
            onSelect: () => selectKey(k),
          });
        }
      } else if (refState === "literal" || refState === "present") {
        items.push({ kind: "empty", text: "No secrets in vault yet", selectable: false });
      }
      if (refState !== "missing") {
        items.push({
          kind: "create-generic",
          selectable: true,
          onSelect: () => {
            closePicker();
            onRequestCreate(null);
          },
        });
      }
    }
    return items;
  }

  function getSelectableIndices(items: DropdownItem[]): number[] {
    return items.flatMap((item, i) => (item.selectable ? [i] : []));
  }

  // Memoize the dropdown items so event handlers read the same list as the
  // upcoming render (Codex Task-5 quality P1: previously buildItems() was
  // called imperatively in event handlers using the *pre-update* `value`
  // prop, which produced stale items lists and wrong highlight indices on
  // fast-typing paths). Gate on `open` so the list is empty when closed.
  // Placed after buildItems and all its closure dependencies to avoid TDZ.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const items = useMemo(() => open ? buildItems() : [], [value, snapshot, isEditingQuery, refState, open, envKey]);

  function moveHighlight(direction: 1 | -1) {
    const selectable = getSelectableIndices(items);
    if (selectable.length === 0) { setHighlightIdx(0); return; }
    const currentSel = selectable.indexOf(highlightIdx);
    let nextSel: number;
    if (currentSel === -1) {
      nextSel = direction === 1 ? 0 : selectable.length - 1;
    } else {
      nextSel = (currentSel + direction + selectable.length) % selectable.length;
    }
    setHighlightIdx(selectable[nextSel]);
  }

  function onInputKeyDown(e: KeyboardEvent) {
    if (e.key === "Tab") {
      if (open) closePicker();
      return;
    }
    if (e.key === "Escape") {
      if (open) {
        e.preventDefault();
        closePicker();
      }
      return;
    }
    if (e.key === "ArrowDown") {
      e.preventDefault();
      if (!open) {
        openPicker();
        // After openPicker, the next render will recompute items via useMemo
        // (open changed). For the current setHighlightIdx we want the first
        // selectable in the items computed for the about-to-open state.
        // Because items is memoized on [open, ...], we compute on the fly:
        const willOpenItems = buildItems();
        const sel = getSelectableIndices(willOpenItems);
        setHighlightIdx(sel[0] ?? 0);
      } else {
        moveHighlight(1);
      }
      return;
    }
    if (e.key === "ArrowUp") {
      e.preventDefault();
      if (!open) {
        openPicker();
        // Same rationale as ArrowDown: items memo is on old open=false state;
        // compute on the fly so the initial highlightIdx lands correctly.
        const willOpenItems = buildItems();
        const sel = getSelectableIndices(willOpenItems);
        setHighlightIdx(sel[sel.length - 1] ?? 0);
      } else {
        moveHighlight(-1);
      }
      return;
    }
    if (e.key === "Enter") {
      e.preventDefault();
      if (open) {
        const item = items[highlightIdx];
        if (item && item.selectable) {
          (item as Extract<DropdownItem, { selectable: true }>).onSelect();
        }
      }
      return;
    }
  }

  function onInputChange(e: Event) {
    const next = (e.target as HTMLInputElement).value;
    onChange(next);
    if (isSecretRef(next) && !open) {
      // Codex Task-5 quality P2: pass `next` so valueAtOpen captures the
      // value actually being typed, not the stale `value` prop.
      openPicker(next);
    }
    // Reset highlight to the first selectable after filter changes. We call
    // buildItems() here against the current closure (value is still the old
    // prop; snapshot/envKey are current). The first-selectable *index* is
    // stable across a single-key filter change (section-label structure
    // doesn't change per character), so this gives the correct index for
    // the upcoming render. The items useMemo handles all other read paths.
    // (Codex Task-5 quality P1)
    const nextItems = buildItems();
    const sel = getSelectableIndices(nextItems);
    setHighlightIdx(sel[0] ?? 0);
  }

  function onToggleClick() {
    togglePicker();
    inputRef.current?.focus();
  }

  const inputClass = (() => {
    if (refState === "missing") return "secret-picker-input broken";
    if (refState === "unverified") return "secret-picker-input unverified";
    return "secret-picker-input";
  })();
  const statusText = (() => {
    if (refState === "missing" && currentSecretKey != null) {
      return `Secret '${currentSecretKey}' not found in current vault`;
    }
    if (refState === "unverified") {
      if (snapshot.status === "error") {
        return snapshot.fetchedAt != null
          ? `Could not refresh vault state — last loaded ${ageString(snapshot.fetchedAt)}`
          : "Could not load vault state";
      }
      if (snapshot.status === "ok" && snapshot.data.vault_state !== "ok") {
        return `Cannot verify — vault ${snapshot.data.vault_state}`;
      }
    }
    return "";
  })();

  const activeOptionId = open && items.length > 0 && items[highlightIdx]?.selectable
    ? `${optionIdPrefix}-${highlightIdx}`
    : undefined;

  return (
    <div class="secret-picker-wrap" ref={wrapperRef}>
      <input
        ref={inputRef}
        type="text"
        class={inputClass}
        value={value}
        placeholder="value (literal, $HOME/..., or pick a secret)"
        role="combobox"
        aria-expanded={open}
        aria-controls={listboxId}
        aria-autocomplete="list"
        aria-activedescendant={activeOptionId}
        aria-label={props.ariaLabel ?? "Secret value picker"}
        aria-describedby={statusText ? statusId : undefined}
        disabled={disabled}
        onInput={onInputChange}
        onKeyDown={onInputKeyDown}
      />
      {statusText && (
        <span id={statusId} class="secret-picker-status visually-hidden" role="status" aria-live="polite">
          {statusText}
        </span>
      )}
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
        <ul id={listboxId} class="secret-picker-dropdown" role="listbox">
          {items.map((item, idx) => {
            const id = `${optionIdPrefix}-${idx}`;
            const highlighted = idx === highlightIdx;
            switch (item.kind) {
              case "section-label":
                // Codex branch-review P2: section labels are decoration, not
                // listbox-owned options. role="presentation" tells AT to skip
                // them (per WAI-ARIA listbox owned-element rules).
                return <li key={`sl-${idx}`} class="dropdown-section-label" role="presentation">{item.text}</li>;
              case "divider":
                // Codex branch-review P2: dividers are visual-only; aria-hidden
                // hides them from AT entirely (no semantic role to expose).
                return <li key={`div-${idx}`} class="dropdown-divider" role="presentation" aria-hidden="true"></li>;
              case "loading":
                return <li key="ld" class="dropdown-loading" role="option" aria-disabled="true" aria-selected="false">Loading vault…</li>;
              case "empty":
                return <li key="emp" class="dropdown-empty" role="option" aria-disabled="true" aria-selected="false">{item.text}</li>;
              case "retry":
                return (
                  <li
                    key="rt"
                    id={id}
                    class={`dropdown-retry ${highlighted ? "highlight" : ""}`}
                    role="option"
                    aria-selected={highlighted}
                    data-action="retry-load-vault"
                    onMouseDown={(e) => { e.preventDefault(); item.onSelect(); }}
                  >
                    Retry loading vault
                  </li>
                );
              case "key": {
                const dataAction =
                  item.isCurrentRef === "missing" ? "key-current-missing"
                  : item.isCurrentRef === "unverified" ? "key-current-unverified"
                  : "key";
                const itemClass =
                  item.isCurrentRef === "missing" ? "dropdown-item-current dropdown-item-current-missing"
                  : item.isCurrentRef === "unverified" ? "dropdown-item-current dropdown-item-current-unverified"
                  : "dropdown-item";
                return (
                  <li
                    key={`k-${item.key}-${idx}`}
                    id={id}
                    class={`${itemClass} ${highlighted ? "highlight" : ""}`}
                    role="option"
                    aria-selected={highlighted}
                    data-action={dataAction}
                    title={item.isLegacyReserved ? "This name is reserved for HTTP routing; new vault keys cannot use it. Existing entry may still be referenced." : undefined}
                    onMouseDown={(e) => { e.preventDefault(); item.onSelect(); }}
                  >
                    <span class="dropdown-key">{item.key}</span>
                    {item.isCurrentRef === "missing" && <span class="missing-badge">missing</span>}
                    {item.isCurrentRef === "unverified" && <span class="unverified-badge">unverified</span>}
                    {item.isMatch && <span class="match-badge">matches KEY name</span>}
                    {item.isLegacyReserved && <span class="legacy-reserved-badge">legacy reserved name</span>}
                    {item.isCurrentRef === "missing" && <span class="dropdown-meta">not in current vault</span>}
                    {item.isCurrentRef === "unverified" && <span class="dropdown-meta">vault unavailable</span>}
                  </li>
                );
              }
              case "create-contextual":
                return (
                  <li
                    key={`cre-${idx}`}
                    id={id}
                    class={`dropdown-create ${highlighted ? "highlight" : ""}`}
                    role="option"
                    aria-selected={highlighted}
                    data-action="create-contextual"
                    onMouseDown={(e) => { e.preventDefault(); item.onSelect(); }}
                  >
                    + Create '{item.targetKey}' in vault…
                  </li>
                );
              case "create-contextual-disabled":
                return (
                  <li
                    key={`cred-${idx}`}
                    class="dropdown-create dropdown-create-disabled"
                    role="option"
                    aria-disabled="true"
                    aria-selected="false"
                    data-action="create-contextual-disabled"
                    title={item.reason}
                    onMouseDown={(e) => e.preventDefault()}
                  >
                    + Create '{item.targetKey}' in vault… (reserved)
                  </li>
                );
              case "create-generic":
                return (
                  <li
                    key={`cn-${idx}`}
                    id={id}
                    class={`dropdown-create ${highlighted ? "highlight" : ""}`}
                    role="option"
                    aria-selected={highlighted}
                    data-action="create-new-secret"
                    onMouseDown={(e) => { e.preventDefault(); item.onSelect(); }}
                  >
                    + Create new secret…
                  </li>
                );
              case "create-generic-disabled":
                return (
                  <li
                    key={`cnd-${idx}`}
                    class="dropdown-create dropdown-create-disabled"
                    role="option"
                    aria-disabled="true"
                    aria-selected="false"
                    data-action="create-disabled"
                    title={item.reason}
                    onMouseDown={(e) => e.preventDefault()}
                  >
                    + Create new (disabled — {snapshot.status === "ok" && snapshot.data.vault_state !== "ok" ? `vault ${snapshot.data.vault_state}` : ""})
                  </li>
                );
            }
          })}
        </ul>
      )}
    </div>
  );
}
