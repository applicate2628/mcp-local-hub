import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { BLANK_FORM, hasNestedUnknown, parseYAMLToForm, toYAML } from "../lib/manifest-yaml";
import type { RouterState } from "../hooks/useRouter";
import { useDebouncedValue } from "../hooks/useDebouncedValue";
import {
  getExtractManifest,
  getManifest,
  postManifestCreate,
  postManifestEdit,
  postManifestValidate,
  ManifestHashMismatchError,
} from "../api";
import { generateUUID } from "../lib/uuid";
import type { BindingFormEntry, DaemonFormEntry, LanguageFormEntry, ManifestFormState } from "../types";
import { useSecretsSnapshot } from "../lib/use-secrets-snapshot";
import { AddSecretModal } from "../components/AddSecretModal";
import { SecretPicker } from "../components/SecretPicker";
import { BrokenRefsSummary } from "../components/BrokenRefsSummary";
import { hasSecretKey, isSecretRef } from "../lib/secret-ref";

// MANIFEST_NAME_REGEX mirrors internal/api/manifest.go:23 validManifestName.
// Live client-side regex check provides instant feedback; the backend still
// authoritatively validates at create time.
const MANIFEST_NAME_REGEX = /^[a-z0-9][a-z0-9._-]*$/;

// KIND_OPTIONS and TRANSPORT_OPTIONS mirror the enum values accepted by
// internal/config/manifest.go. Keeping them as const tuples lets TS narrow
// them into the literal-union fields of ManifestFormState.
const KIND_OPTIONS = [
  { value: "global", label: "global (shared across all projects)" },
  { value: "workspace-scoped", label: "workspace-scoped (per-workspace lazy proxy)" },
] as const;
const TRANSPORT_OPTIONS = [
  { value: "stdio-bridge", label: "stdio-bridge (daemon multiplexes stdio child)" },
  { value: "native-http", label: "native-http (upstream speaks HTTP directly)" },
] as const;
const KNOWN_CLIENTS = ["claude-code", "codex-cli", "gemini-cli", "antigravity"] as const;

// deepEqualForm compares two ManifestFormState instances structurally. Used
// by the Q8 dirty check. JSON.stringify is defensible for this shape: all
// fields are serializable primitives, arrays, and plain objects with no
// Date/Map/Set/functions. If a future field breaks that assumption, switch
// to a proper deep-equal import and update the test.
function deepEqualForm(a: ManifestFormState, b: ManifestFormState): boolean {
  return JSON.stringify(a) === JSON.stringify(b);
}

// parseAddServerQuery extracts ?server=...&from-client=... from the current
// hash. A1's Create-manifest button navigates to
// #/add-server?server=<name>&from-client=<client> — we pick those up on
// mount and run the prefill fetch.
function parseAddServerQuery(): { server: string; fromClient: string } {
  const hash = window.location.hash;
  const q = hash.split("?")[1] ?? "";
  const params = new URLSearchParams(q);
  return {
    server: params.get("server") ?? "",
    fromClient: params.get("from-client") ?? "",
  };
}

export function AddServerScreen(props: {
  mode?: "create" | "edit";
  route?: RouterState;
  onDirtyChange?: (dirty: boolean) => void;
} = {}) {
  const mode = props.mode ?? "create";
  const [formState, setFormState] = useState<ManifestFormState>(BLANK_FORM);
  // initialSnapshot is the post-normalization baseline the dirty check
  // compares against. Updated on mount (after any prefill path) and on
  // successful Save. Critically NOT updated on Paste YAML import (Q8
  // anti-silent-data-loss: paste must not move the baseline).
  const [initialSnapshot, setInitialSnapshot] = useState<ManifestFormState>(BLANK_FORM);
  const debouncedState = useDebouncedValue(formState, 150);
  const yamlPreview = toYAML(debouncedState);

  const isDirty = !deepEqualForm(formState, initialSnapshot);

  const snapshot = useSecretsSnapshot();
  const [createModalState, setCreateModalState] = useState<{ open: boolean; prefill: string | null }>({ open: false, prefill: null });
  // savedFiredRef tracks whether onSaved (which already does a refresh)
  // has fired during this modal lifecycle. If it has, the on-close
  // refresh is skipped to prevent a stale-replacing-fresh race.
  // Memo §5.7 + Codex memo-R2 P2-A.
  const savedFiredRef = useRef(false);

  function openCreateModal(prefill: string | null) {
    savedFiredRef.current = false;
    setCreateModalState({ open: true, prefill });
  }

  // Compute broken-ref list for summary line.
  const brokenRefs: string[] = (() => {
    if (snapshot.status !== "ok") return [];
    if (snapshot.data.vault_state !== "ok") return [];
    const presentSet = new Set(
      snapshot.data.secrets.filter((s) => s.state === "present").map((s) => s.name)
    );
    const refs = formState.env
      .filter((row) => isSecretRef(row.value) && hasSecretKey(row.value))
      .map((row) => row.value.slice("secret:".length))
      .filter((k) => !presentSet.has(k));
    return Array.from(new Set(refs));
  })();
  // Codex Task-7 quality: deliberately "ok" on loading/error snapshots. The
  // BrokenRefsSummary surfaces aggregated broken-ref counts ONLY when the
  // vault is reachable and we have authoritative data. During loading or
  // error, brokenRefs is also forced to [] above, so BrokenRefsSummary
  // renders null. Per-row vault unreachability is announced by each
  // SecretPicker's own statusText (memo §5.3 / D3 — summary is for vault-
  // reachable broken-ref aggregation, not for vault-state announcements).
  const summaryVaultState = snapshot.status === "ok" ? snapshot.data.vault_state : "ok";

  const [loadError, setLoadError] = useState<string | null>(null);
  const [readOnlyReason, setReadOnlyReason] = useState<string | null>(null);

  useEffect(() => {
    props.onDirtyChange?.(isDirty);
  }, [isDirty]);

  // Prefill path (Q8 baseline gotcha): fetch extract-manifest when the
  // user arrives from A1, parse → set form state → take the snapshot
  // AFTER normalization so dirty is false on first render.
  useEffect(() => {
    const { server, fromClient } = parseAddServerQuery();
    if (!server || !fromClient) return;
    let cancelled = false;
    (async () => {
      try {
        const yaml = await getExtractManifest(fromClient, server);
        if (cancelled) return;
        const parsed = parseYAMLToForm(yaml);
        setFormState(parsed);
        setInitialSnapshot(parsed);
      } catch (err) {
        if (cancelled) return;
        setBanner({
          kind: "error",
          text: `Could not prefill from ${fromClient}/${server}: ${(err as Error).message}. Continuing with empty form.`,
        });
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  // editName is derived from route.query so that a dirty-declined name=a →
  // name=b navigation does not fire a stale load (the memo dep stays stable).
  const editName = useMemo(() => {
    if (props.mode !== "edit") return "";
    const params = new URLSearchParams(props.route?.query ?? "");
    return params.get("name") ?? "";
  }, [props.mode, props.route?.query]);

  // Mount effect for edit mode: reset per-manifest state BEFORE the new load
  // (R3 invariant) then fetch, apply hash, and detect nested-unknown fields.
  useEffect(() => {
    if (props.mode !== "edit") return;
    // R3 correction: reset prior per-manifest state BEFORE the new load.
    // Without this, navigating a→b in edit mode inherits a's loadError
    // or readOnlyReason (e.g., a had nested unknowns, b is clean, b
    // would render in read-only mode). Also blank the form while
    // fetching so we don't flash a's data in b's UI.
    setLoadError(null);
    setReadOnlyReason(null);
    setFormState(BLANK_FORM);
    setInitialSnapshot(BLANK_FORM);
    if (!editName) {
      setLoadError("No manifest name specified");
      return;
    }
    let cancelled = false;
    (async () => {
      try {
        const { yaml, hash } = await getManifest(editName);
        if (cancelled) return;
        const nested = hasNestedUnknown(yaml);
        const parsed = parseYAMLToForm(yaml);
        parsed.loadedHash = hash;
        setFormState(parsed);
        setInitialSnapshot(parsed);
        if (nested) {
          setReadOnlyReason(
            "This manifest contains fields the GUI cannot handle. Editing via GUI would drop them.",
          );
        }
      } catch (err) {
        if (cancelled) return;
        setLoadError((err as Error).message);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [props.mode, editName]);

  const readOnly = readOnlyReason !== null;

  const nameError = formState.name.length > 0 && !MANIFEST_NAME_REGEX.test(formState.name)
    ? "Must match [a-z0-9][a-z0-9._-]* (lowercase, digits, '.', '_', '-')"
    : "";

  function updateField<K extends keyof ManifestFormState>(key: K, value: ManifestFormState[K]) {
    setFormState((prev) => ({ ...prev, [key]: value }));
  }

  function updateBaseArg(index: number, value: string) {
    setFormState((prev) => {
      const next = prev.base_args.slice();
      next[index] = value;
      return { ...prev, base_args: next };
    });
  }

  function addBaseArg() {
    setFormState((prev) => ({ ...prev, base_args: [...prev.base_args, ""] }));
  }

  function deleteBaseArg(index: number) {
    setFormState((prev) => ({
      ...prev,
      base_args: prev.base_args.filter((_, i) => i !== index),
    }));
  }

  function addEnv() {
    setFormState((prev) => ({ ...prev, env: [...prev.env, { key: "", value: "" }] }));
  }

  function updateEnv(index: number, field: "key" | "value", value: string) {
    setFormState((prev) => {
      const next = prev.env.slice();
      next[index] = { ...next[index], [field]: value };
      return { ...prev, env: next };
    });
  }

  function deleteEnv(index: number) {
    setFormState((prev) => ({
      ...prev,
      env: prev.env.filter((_, i) => i !== index),
    }));
  }

  function addDaemon() {
    setFormState((prev) => ({
      ...prev,
      daemons: [...prev.daemons, { _id: generateUUID(), name: "", port: 0 }],
    }));
  }

  // updateDaemon handles port updates. Bindings reference daemons by _id
  // (identity-stable UUID), so rename no longer needs cascade — the binding
  // automatically resolves to the updated name at toYAML time.
  function updateDaemon(index: number, field: "name" | "port", value: string) {
    setFormState((prev) => {
      const target = prev.daemons[index];
      if (!target) return prev;
      const nextDaemon: DaemonFormEntry = field === "name"
        ? { ...target, name: value }
        : { ...target, port: parsePort(value) };
      const nextDaemons = prev.daemons.slice();
      nextDaemons[index] = nextDaemon;
      // No cascade needed — bindings key by _id, which is identity-stable.
      return { ...prev, daemons: nextDaemons };
    });
  }

  // deleteDaemon cascades to bindings: if any bindings reference this
  // daemon by _id, the user is prompted; on confirm both the daemon row and
  // every binding that pointed at it are removed in one state update.
  function deleteDaemon(index: number) {
    setFormState((prev) => {
      const target = prev.daemons[index];
      if (!target) return prev;
      const orphans = prev.client_bindings.filter((b) => b.daemonId === target._id);
      if (orphans.length > 0) {
        // eslint-disable-next-line no-alert
        const ok = window.confirm(
          `Delete daemon "${target.name}" and its ${orphans.length} client binding${orphans.length === 1 ? "" : "s"}?`,
        );
        if (!ok) return prev;
      }
      return {
        ...prev,
        daemons: prev.daemons.filter((_, i) => i !== index),
        client_bindings: prev.client_bindings.filter((b) => b.daemonId !== target._id),
      };
    });
  }

  function updateDaemonExtras(
    id: string,
    field: "context" | "extra_args",
    value: string | string[] | undefined,
  ) {
    setFormState((prev) => ({
      ...prev,
      daemons: prev.daemons.map((d) =>
        d._id === id ? { ...d, [field]: value } : d,
      ),
    }));
  }

  function parsePort(raw: string): number {
    const n = Number(raw);
    return Number.isFinite(n) && n >= 0 ? Math.trunc(n) : 0;
  }

  function toggleBinding(daemonId: string, client: string, checked: boolean) {
    setFormState((prev) => {
      if (checked) {
        return {
          ...prev,
          client_bindings: [
            ...prev.client_bindings,
            { client, daemonId, url_path: "/mcp" },
          ],
        };
      }
      return {
        ...prev,
        client_bindings: prev.client_bindings.filter(
          (b) => !(b.client === client && b.daemonId === daemonId),
        ),
      };
    });
  }

  // addBinding creates a new binding referencing the daemon by its stable _id.
  function addBinding(daemonId: string) {
    setFormState((prev) => ({
      ...prev,
      client_bindings: [
        ...prev.client_bindings,
        { client: KNOWN_CLIENTS[0], daemonId, url_path: "/mcp" },
      ],
    }));
  }

  function updateBinding(index: number, field: "client" | "daemonId" | "url_path", value: string) {
    setFormState((prev) => {
      const next = prev.client_bindings.slice();
      const target = next[index];
      if (!target) return prev;
      next[index] = { ...target, [field]: value };
      return { ...prev, client_bindings: next };
    });
  }

  function deleteBinding(index: number) {
    setFormState((prev) => ({
      ...prev,
      client_bindings: prev.client_bindings.filter((_, i) => i !== index),
    }));
  }

  const [warnings, setWarnings] = useState<string[] | null>(null);

  type Banner = {
    kind: "error" | "success";
    text: string;
    retry?: () => Promise<void>;
    reinstall?: boolean;
    staleReload?: boolean;
    staleForceSave?: boolean;
  };

  const [banner, setBanner] = useState<Banner | null>(null);
  const [busy, setBusy] = useState<"" | "validate" | "save" | "install">("");
  // submissionVersion: bumped every time a Save/Save&Install click starts
  // its own inline serialize-validate-submit pipeline. If a second click
  // happens while the first is still in flight, the older pipeline sees
  // submissionCounter.current != its own captured value and bails before
  // writing to state. (Q3 Codex-identified gotcha.)
  const submissionCounter = useRef(0);
  // validateVersion: same pattern for the async Validate button path. A
  // newer Validate click invalidates an older in-flight validate's result
  // so stale warnings don't paint over fresh state. (Q5.)
  const validateCounter = useRef(0);

  async function runValidate() {
    const version = ++validateCounter.current;
    setBusy("validate");
    setBanner(null);
    try {
      const payload = toYAML(formState); // FRESH, not debounced
      const out = await postManifestValidate(payload);
      if (version !== validateCounter.current) return; // preempted
      setWarnings(out);
      if (out.length === 0) {
        setBanner({ kind: "success", text: "Validation passed — no warnings." });
      } else {
        setBanner({ kind: "error", text: `${out.length} validation warning${out.length === 1 ? "" : "s"}.` });
      }
    } catch (err) {
      if (version !== validateCounter.current) return;
      setBanner({ kind: "error", text: `/api/manifest/validate: ${(err as Error).message}` });
    } finally {
      setBusy("");
    }
  }

  async function runSave(opts: { install: boolean }) {
    const version = ++submissionCounter.current;
    setBusy(opts.install ? "install" : "save");
    setBanner(null);
    try {
      // Codex R1 correction: in edit mode, anchor identity + concurrency guard
      // to IMMUTABLE sources. formState.name and formState.loadedHash are
      // mutable — handlePasteYAML overwrites them — so using them would let
      // a mid-session Paste YAML retarget the Save to a different manifest
      // with expected_hash="" (no stale protection). editName comes from the
      // URL and is fixed per edit session; initialSnapshot.loadedHash is set
      // on Load/Save and is NOT touched by Paste.
      const name = mode === "edit" ? editName : formState.name.trim();
      if (!name) {
        setBanner({ kind: "error", text: "Name is required." });
        return;
      }
      // In edit mode, toYAML must serialize against the original identity.
      // If a Paste slipped through (it shouldn't — Paste is disabled in
      // edit mode — but defense in depth), force the target name back into
      // the payload so the written YAML matches the target path.
      const payloadState = mode === "edit" ? { ...formState, name } : formState;
      const payload = toYAML(payloadState);
      const warnings = await postManifestValidate(payload);
      if (version !== submissionCounter.current) return;
      if (warnings.length > 0) {
        setWarnings(warnings);
        setBanner({
          kind: "error",
          text: `Cannot save: ${warnings.length} validation warning${warnings.length === 1 ? "" : "s"}.`,
        });
        return;
      }
      if (mode === "edit") {
        try {
          const expectedHash = initialSnapshot.loadedHash;
          const { hash: newHash } = await postManifestEdit(name, payload, expectedHash);
          if (version !== submissionCounter.current) return;
          // Atomic snapshot update: build one post-save object carrying the
          // fresh hash AND the user's just-persisted form state; set both
          // formState and initialSnapshot from the same reference so dirty
          // is false (P1-2 fix: no separate getManifest refresh, no ordering race).
          const postSave: ManifestFormState = { ...payloadState, loadedHash: newHash };
          setFormState(postSave);
          setInitialSnapshot(postSave);
        } catch (err) {
          if (version !== submissionCounter.current) return;
          if (err instanceof ManifestHashMismatchError) {
            setBanner({
              kind: "error",
              text: "Manifest changed on disk since you opened it. Reload will discard your edits and show the new version. Force Save will overwrite with your version.",
              staleReload: true,
              staleForceSave: true,
            });
            return;
          }
          throw err;
        }
      } else {
        await postManifestCreate(name, payload);
        if (version !== submissionCounter.current) return;
        setInitialSnapshot(formState);
      }
      setWarnings(null);
      if (!opts.install) {
        setBanner({
          kind: "success",
          text: mode === "edit"
            ? `Saved. Daemon still running old config.`
            : `Saved servers/${name}/manifest.yaml.`,
          reinstall: mode === "edit",
        });
        return;
      }
      await runInstallNow(name, version);
    } catch (err) {
      if (version !== submissionCounter.current) return;
      setBanner({ kind: "error", text: (err as Error).message });
    } finally {
      if (version === submissionCounter.current) setBusy("");
    }
  }

  async function runReload() {
    if (!editName) return;
    setBusy("save");
    setBanner(null);
    try {
      const { yaml, hash } = await getManifest(editName);
      // Codex R1 correction: re-run hasNestedUnknown on the reloaded YAML.
      // The external write that caused the stale-hash mismatch may have
      // introduced unsupported nested fields (e.g. a new daemons[].extra_*
      // key). Without this check, Reload bypasses the read-only guard that
      // the initial mount effect enforces, and a subsequent Save would drop
      // the unsupported fields silently.
      const nested = hasNestedUnknown(yaml);
      const parsed = parseYAMLToForm(yaml);
      parsed.loadedHash = hash;
      setFormState(parsed);
      setInitialSnapshot(parsed);
      if (nested) {
        setReadOnlyReason(
          "This manifest contains fields the GUI cannot handle. Editing via GUI would drop them.",
        );
      } else {
        // Clear any stale read-only reason from a prior load so the form
        // becomes editable again when the external write removed the
        // problematic nested-unknown field.
        setReadOnlyReason(null);
      }
      setBanner({ kind: "success", text: "Reloaded fresh manifest from disk." });
    } catch (err) {
      setBanner({ kind: "error", text: (err as Error).message });
    } finally {
      setBusy("");
    }
  }

  async function runForceSave() {
    const version = ++submissionCounter.current;
    setBusy("save");
    setBanner(null);
    try {
      // Codex R1 correction: Force Save is only reachable in edit mode;
      // anchor to editName (URL-derived, immutable) rather than
      // formState.name which a Paste YAML could have retargeted.
      const name = editName;
      if (!name) return;
      // 1. Re-read disk to get fresh hash + fresh _preservedRaw.
      const fresh = await getManifest(name);
      if (version !== submissionCounter.current) return;
      const freshParsed = parseYAMLToForm(fresh.yaml);
      // 2. Merge: user's known-field edits win; fresh disk _preservedRaw wins.
      // Force the target name back into the merged payload so serialization
      // matches the target path even if a Paste slipped through.
      const merged: ManifestFormState = {
        ...formState,
        name,
        _preservedRaw: freshParsed._preservedRaw,
      };
      // 3. Serialize FINAL payload AFTER merge.
      const payload = toYAML(merged);
      // 4. Validate the FINAL payload (P1-4 fix: validate the exact bytes
      // that will be written, not pre-merge).
      const warnings = await postManifestValidate(payload);
      if (version !== submissionCounter.current) return;
      if (warnings.length > 0) {
        setWarnings(warnings);
        setBanner({
          kind: "error",
          text: `Cannot Force Save: ${warnings.length} validation warning${warnings.length === 1 ? "" : "s"} in merged payload.`,
        });
        return;
      }
      // 5. Write with fresh hash as expectedHash; consume returned new hash.
      const { hash: newHash } = await postManifestEdit(name, payload, fresh.hash);
      if (version !== submissionCounter.current) return;
      // 6. Atomic baseline update.
      const postSave: ManifestFormState = { ...merged, loadedHash: newHash };
      setFormState(postSave);
      setInitialSnapshot(postSave);
      const preservedKeys = Object.keys(freshParsed._preservedRaw);
      setBanner({
        kind: "success",
        text:
          preservedKeys.length > 0
            ? `Force-saved. Preserved external fields: ${preservedKeys.join(", ")}.`
            : `Force-saved.`,
        reinstall: true,
      });
    } catch (err) {
      if (version !== submissionCounter.current) return;
      setBanner({ kind: "error", text: `Force Save failed: ${(err as Error).message}` });
    } finally {
      if (version === submissionCounter.current) setBusy("");
    }
  }

  async function runInstallNow(name: string, version: number) {
    try {
      const resp = await fetch(`/api/install?name=${encodeURIComponent(name)}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
      });
      if (version !== submissionCounter.current) return;
      if (!resp.ok) {
        const body = await resp.json().catch(() => ({}));
        const err = (body as { error?: string }).error ?? resp.statusText;
        setBanner({
          kind: "error",
          text: `Saved servers/${name}/manifest.yaml, but install failed: ${err}`,
          retry: () => runInstallNow(name, ++submissionCounter.current),
        });
        return;
      }
      setWarnings(null);
      setBanner({ kind: "success", text: `Installed ${name}. Daemons will start at next logon (or run "mcphub restart --server ${name}" now).` });
    } catch (err) {
      if (version !== submissionCounter.current) return;
      setBanner({
        kind: "error",
        text: `Saved servers/${name}/manifest.yaml, but install threw: ${(err as Error).message}`,
        retry: () => runInstallNow(name, ++submissionCounter.current),
      });
    }
  }

  async function handlePasteYAML() {
    const pasted = window.prompt("Paste YAML manifest:", "");
    if (pasted == null || pasted.trim() === "") return;
    let parsed: ManifestFormState;
    try {
      parsed = parseYAMLToForm(pasted);
    } catch (err) {
      setBanner({ kind: "error", text: `Paste failed: ${(err as Error).message}` });
      return;
    }
    setFormState(parsed);
    // Per Q8 decision: paste does NOT reset the dirty baseline. Only
    // successful Save does. We DO auto-run structural validate since
    // paste is a mode switch and users expect "this parsed / this
    // mapped" feedback (Codex xhigh memo).
    //
    // Inline the validate against `parsed` — NOT via runValidate(),
    // whose closure would see the pre-paste formState (Task 12 review
    // must-fix).
    const version = ++validateCounter.current;
    setBusy("validate");
    setBanner(null);
    try {
      const payload = toYAML(parsed);
      const out = await postManifestValidate(payload);
      if (version !== validateCounter.current) return;
      setWarnings(out);
      if (out.length === 0) {
        setBanner({ kind: "success", text: "Pasted YAML passed validation." });
      } else {
        setBanner({ kind: "error", text: `Pasted YAML has ${out.length} validation warning${out.length === 1 ? "" : "s"}.` });
      }
    } catch (err) {
      if (version !== validateCounter.current) return;
      setBanner({ kind: "error", text: `/api/manifest/validate: ${(err as Error).message}` });
    } finally {
      setBusy("");
    }
  }

  async function handleCopyYAML() {
    const yaml = toYAML(formState); // fresh, not debounced
    try {
      await navigator.clipboard.writeText(yaml);
      setBanner({ kind: "success", text: "YAML copied to clipboard." });
    } catch {
      // Fallback for environments without clipboard API (older E2E setup etc.)
      setBanner({ kind: "error", text: "Clipboard API unavailable — copy manually from the preview pane." });
    }
  }

  return (
    <section class="screen add-server">
      <h1>Add server</h1>
      {loadError && (
        <div class="banner error" data-testid="load-error-banner">
          <p>Failed to load <code>{editName || "(unnamed)"}</code>: {loadError}</p>
          <div class="banner-actions">
            <button type="button" onClick={() => { setLoadError(null); window.location.reload(); }}>Retry</button>
            <button type="button" onClick={() => { window.location.hash = "#/servers"; }}>Back to Servers</button>
          </div>
        </div>
      )}
      {readOnlyReason && (
        <div class="banner warning" data-testid="readonly-banner">
          <p>{readOnlyReason}</p>
          <p>
            Edit via CLI (<code>mcphub manifest edit {editName}</code>) or
            delete + recreate via Add Server.
          </p>
          <div class="banner-actions">
            <button type="button" onClick={() => { window.location.hash = "#/servers"; }}>Back to Servers</button>
          </div>
        </div>
      )}
      <div class="toolbar" data-testid="add-server-toolbar">
        <button
          type="button"
          onClick={runValidate}
          disabled={readOnly || busy !== ""}
          data-action="validate"
        >
          {busy === "validate" ? "Validating…" : "Validate"}
        </button>
        <button
          type="button"
          onClick={() => runSave({ install: false })}
          disabled={readOnly || busy !== "" || (mode !== "edit" && !!nameError)}
          data-action="save"
        >
          {busy === "save" ? "Saving…" : "Save"}
        </button>
        <button
          type="button"
          class="primary"
          onClick={() => runSave({ install: true })}
          disabled={readOnly || busy !== "" || (mode !== "edit" && !!nameError)}
          data-action="save-and-install"
        >
          {busy === "install" ? "Installing…" : "Save & Install"}
        </button>
        <button
          type="button"
          onClick={handlePasteYAML}
          disabled={readOnly || mode === "edit" || busy !== ""}
          data-action="paste-yaml"
          title={mode === "edit" ? "Paste YAML is disabled in edit mode to prevent replacing the target manifest's identity mid-session. To replace a manifest wholesale, delete it from Servers and create a new one." : undefined}
        >
          Paste YAML
        </button>
        <button
          type="button"
          onClick={handleCopyYAML}
          disabled={busy !== ""}
          data-action="copy-yaml"
        >
          Copy YAML
        </button>
      </div>
      {banner && (
        <div class={`banner ${banner.kind}`} data-testid="banner">
          <p>{banner.text}</p>
          {banner.retry && (
            <button type="button" onClick={() => banner.retry?.()} data-action="retry-install">Retry Install</button>
          )}
          {banner.reinstall && (
            <button
              type="button"
              onClick={() => runInstallNow(formState.name.trim(), ++submissionCounter.current)}
              data-action="reinstall"
            >
              Reinstall
            </button>
          )}
          {banner.staleReload && (
            <button type="button" onClick={() => runReload()} data-action="reload">Reload</button>
          )}
          {banner.staleForceSave && (
            <button type="button" onClick={() => runForceSave()} data-action="force-save">Force Save</button>
          )}
        </div>
      )}
      {warnings && warnings.length > 0 && (
        <ul class="validation-warnings" data-testid="validation-warnings">
          {warnings.map((w, i) => (
            <li key={i}>{w}</li>
          ))}
        </ul>
      )}
      <div class="add-server-grid">
        <div class="add-server-form">
          <AccordionSection title="Basics" open={true}>
            <div class="form-row">
              <label for="field-name">Name</label>
              <input
                id="field-name"
                type="text"
                value={formState.name}
                placeholder="memory"
                onInput={(e) => updateField("name", (e.currentTarget as HTMLInputElement).value)}
                disabled={readOnly || mode === "edit"}
                title={mode === "edit" ? "Kind and name are immutable after first install. Delete and recreate the server to change them." : undefined}
              />
              {nameError && <span class="inline-error">{nameError}</span>}
            </div>
            <div class="form-row">
              <label for="field-kind">Kind</label>
              <select
                id="field-kind"
                value={formState.kind}
                onChange={(e) => updateField("kind", (e.currentTarget as HTMLSelectElement).value as ManifestFormState["kind"])}
                disabled={readOnly || mode === "edit"}
                title={mode === "edit" ? "Kind and name are immutable after first install. Delete and recreate the server to change them." : undefined}
              >
                {KIND_OPTIONS.map((opt) => (
                  <option key={opt.value} value={opt.value}>{opt.label}</option>
                ))}
              </select>
            </div>
          </AccordionSection>

          <AccordionSection title="Command">
            <div class="form-row">
              <label for="field-transport">Transport</label>
              <select
                id="field-transport"
                value={formState.transport}
                onChange={(e) => updateField("transport", (e.currentTarget as HTMLSelectElement).value as ManifestFormState["transport"])}
                disabled={readOnly}
              >
                {TRANSPORT_OPTIONS.map((opt) => (
                  <option key={opt.value} value={opt.value}>{opt.label}</option>
                ))}
              </select>
            </div>
            <div class="form-row">
              <label for="field-command">Command</label>
              <input
                id="field-command"
                type="text"
                value={formState.command}
                placeholder="npx"
                onInput={(e) => updateField("command", (e.currentTarget as HTMLInputElement).value)}
                disabled={readOnly}
              />
            </div>
            <div class="form-row">
              <label>Base args</label>
              <div class="repeatable-rows" data-testid="base-args">
                {formState.base_args.map((arg, i) => (
                  <div class="form-row" key={i}>
                    <input
                      type="text"
                      value={arg}
                      onInput={(e) => updateBaseArg(i, (e.currentTarget as HTMLInputElement).value)}
                      disabled={readOnly}
                    />
                    <button type="button" onClick={() => deleteBaseArg(i)} disabled={readOnly} data-action="delete-base-arg">×</button>
                  </div>
                ))}
                <button type="button" onClick={addBaseArg} disabled={readOnly} data-action="add-base-arg">+ Add arg</button>
              </div>
            </div>
            <div class="form-row">
              <label for="field-weekly">Weekly refresh</label>
              <input
                id="field-weekly"
                type="checkbox"
                checked={formState.weekly_refresh}
                onChange={(e) => updateField("weekly_refresh", (e.currentTarget as HTMLInputElement).checked)}
                disabled={readOnly}
              />
            </div>
          </AccordionSection>

          <AccordionSection title="Environment">
            <BrokenRefsSummary vaultState={summaryVaultState} brokenRefs={brokenRefs} />
            <div class="repeatable-rows" data-testid="env-rows">
              {formState.env.map((row, i) => (
                <div class="form-row env-row" key={i} data-env-row={i}>
                  <input
                    type="text"
                    placeholder="KEY"
                    value={row.key}
                    onInput={(e) => updateEnv(i, "key", (e.currentTarget as HTMLInputElement).value)}
                    disabled={readOnly}
                  />
                  <SecretPicker
                    value={row.value}
                    onChange={(next) => updateEnv(i, "value", next)}
                    envKey={row.key}
                    snapshot={snapshot}
                    onRequestCreate={openCreateModal}
                    disabled={readOnly}
                  />
                  <button type="button" onClick={() => deleteEnv(i)} disabled={readOnly} data-action="delete-env">×</button>
                </div>
              ))}
              <button type="button" onClick={addEnv} disabled={readOnly} data-action="add-env">+ Add environment variable</button>
            </div>
          </AccordionSection>
          <AccordionSection title="Daemons">
            <div class="repeatable-rows" data-testid="daemon-rows">
              {formState.daemons.map((d, i) => (
                <div class="form-row daemon-row" key={d._id} data-daemon-row={i}>
                  <input
                    type="text"
                    placeholder="name (e.g. default)"
                    value={d.name}
                    onInput={(e) => updateDaemon(i, "name", (e.currentTarget as HTMLInputElement).value)}
                    disabled={readOnly}
                    data-field="daemon-name"
                  />
                  <input
                    type="number"
                    min={0}
                    max={65535}
                    placeholder="9100"
                    value={d.port}
                    onInput={(e) => updateDaemon(i, "port", (e.currentTarget as HTMLInputElement).value)}
                    disabled={readOnly}
                    data-field="daemon-port"
                  />
                  <button type="button" onClick={() => deleteDaemon(i)} disabled={readOnly} data-action="delete-daemon">×</button>
                </div>
              ))}
              <button type="button" onClick={addDaemon} disabled={readOnly} data-action="add-daemon">+ Add daemon</button>
            </div>
          </AccordionSection>
          <AccordionSection title="Client bindings">
            <ClientBindingsSection
              daemons={formState.daemons}
              bindings={formState.client_bindings}
              onAdd={addBinding}
              onUpdate={updateBinding}
              onDelete={deleteBinding}
              onToggle={toggleBinding}
              readOnly={readOnly}
            />
          </AccordionSection>
          <AccordionSection title="Advanced">
            <div class="form-row">
              <label for="field-idle-timeout">Idle timeout (min)</label>
              <input
                id="field-idle-timeout"
                type="number"
                min={0}
                value={formState.idle_timeout_min ?? ""}
                placeholder="(unset)"
                disabled={readOnly}
                onInput={(e) => {
                  const v = (e.currentTarget as HTMLInputElement).value;
                  updateField("idle_timeout_min", v === "" ? undefined : Number(v));
                }}
              />
            </div>
            <div class="form-row">
              <label>Base args template</label>
              <RepeatableStringRows
                label="arg"
                value={formState.base_args_template ?? []}
                onChange={(next) =>
                  updateField("base_args_template", next.length > 0 ? next : undefined)
                }
                disabled={readOnly}
                dataTestId="base-args-template"
              />
            </div>
            {formState.kind === "workspace-scoped" && (
              <>
                <PortPoolField
                  value={formState.port_pool}
                  onChange={(pp) => updateField("port_pool", pp)}
                  disabled={readOnly}
                />
                <LanguagesSubsection
                  languages={formState.languages ?? []}
                  onChange={(next) =>
                    updateField("languages", next.length > 0 ? next : undefined)
                  }
                  disabled={readOnly}
                />
              </>
            )}
            {formState.daemons.length > 0 && (
              <div class="form-row" data-testid="daemon-extras">
                <label>Per-daemon extras</label>
                <DaemonExtrasSubsection
                  daemons={formState.daemons}
                  kind={formState.kind}
                  onUpdate={(id, field, value) => updateDaemonExtras(id, field, value)}
                  disabled={readOnly}
                />
              </div>
            )}
          </AccordionSection>
        </div>
        <aside class="add-server-preview">
          <h2>YAML preview</h2>
          <pre data-testid="yaml-preview">{yamlPreview}</pre>
        </aside>
      </div>
      <AddSecretModal
        open={createModalState.open}
        prefillName={createModalState.prefill ?? undefined}
        onSaved={async () => {
          savedFiredRef.current = true;
          await snapshot.refresh();
        }}
        onClose={async () => {
          setCreateModalState({ open: false, prefill: null });
          // Skip refresh on the success path — onSaved already did it.
          if (savedFiredRef.current) return;
          await snapshot.refresh();
        }}
      />
    </section>
  );
}

// AccordionSection is the reusable collapsible container used by every form
// section. `open` controls initial state; clicking the header toggles.
function AccordionSection(props: { title: string; open?: boolean; children: preact.ComponentChildren }) {
  const [expanded, setExpanded] = useState(props.open ?? false);
  return (
    <section class={`accordion ${expanded ? "open" : "closed"}`}>
      <button
        type="button"
        class="accordion-header"
        aria-expanded={expanded}
        onClick={() => setExpanded((x) => !x)}
      >
        <span class="chevron">{expanded ? "▾" : "▸"}</span>
        <span>{props.title}</span>
      </button>
      {expanded && <div class="accordion-body">{props.children}</div>}
    </section>
  );
}

// ClientBindingsSection adaptively renders the bindings list:
//   - When there's exactly one daemon: flat [client][url_path][x] rows,
//     no inner accordion chrome. New bindings are added under that daemon.
//   - When there are 0 or 2+ daemons: grouped by daemon, each group is
//     its own collapsible inner subsection. Zero-daemon case shows a
//     helpful empty-state instructing the user to add a daemon first.
function ClientBindingsSection(props: {
  daemons: DaemonFormEntry[];
  bindings: BindingFormEntry[];
  onAdd: (daemonId: string) => void;
  onUpdate: (index: number, field: "client" | "daemonId" | "url_path", value: string) => void;
  onDelete: (index: number) => void;
  onToggle: (daemonId: string, client: string, checked: boolean) => void;
  readOnly?: boolean;
}) {
  const { daemons, bindings, onAdd, onUpdate, onDelete, onToggle, readOnly } = props;
  if (daemons.length === 0) {
    return (
      <p class="placeholder">
        Add at least one daemon (in the section above) before creating
        client bindings — each binding must reference a daemon by name.
      </p>
    );
  }
  if (daemons.length === 1) {
    const only = daemons[0]._id;
    return (
      <BindingsList
        bindings={bindings}
        onAdd={() => onAdd(only)}
        onUpdate={onUpdate}
        onDelete={onDelete}
        readOnly={readOnly}
      />
    );
  }
  if (daemons.length >= 4) {
    return <BindingsMatrix daemons={daemons} bindings={bindings} onToggle={onToggle} onUpdate={onUpdate} readOnly={readOnly} />;
  }
  return (
    <div data-testid="bindings-adaptive-multi">
      {daemons.map((d) => {
        const indices: number[] = [];
        const group = bindings.filter((b, idx) => {
          if (b.daemonId === d._id) { indices.push(idx); return true; }
          return false;
        });
        return (
          <section class="bindings-daemon-group" key={d._id} data-daemon-group={d.name}>
            <h3>daemon: {d.name} (port {d.port})</h3>
            <BindingsList
              bindings={group}
              indices={indices}
              onAdd={() => onAdd(d._id)}
              onUpdate={onUpdate}
              onDelete={onDelete}
              readOnly={readOnly}
            />
          </section>
        );
      })}
    </div>
  );
}

// BindingsList renders a flat list of bindings. When the `indices` prop
// is supplied (multi-daemon path), it maps each displayed row to its
// absolute index in the parent client_bindings array, so the onUpdate /
// onDelete calls operate on the correct slot. Single-daemon path supplies
// the whole bindings array without an indices map.
function BindingsList(props: {
  bindings: BindingFormEntry[];
  indices?: number[];
  onAdd: () => void;
  onUpdate: (index: number, field: "client" | "daemonId" | "url_path", value: string) => void;
  onDelete: (index: number) => void;
  readOnly?: boolean;
}) {
  const { bindings, indices, onAdd, onUpdate, onDelete, readOnly } = props;
  return (
    <div class="repeatable-rows bindings-list" data-testid="bindings-list">
      {bindings.map((b, displayIdx) => {
        const absIdx = indices ? indices[displayIdx] : displayIdx;
        return (
          <div class="form-row binding-row" key={absIdx} data-binding-row={absIdx}>
            <select
              value={b.client}
              data-field="binding-client"
              onChange={(e) => onUpdate(absIdx, "client", (e.currentTarget as HTMLSelectElement).value)}
              disabled={readOnly}
            >
              {KNOWN_CLIENTS.map((c) => (
                <option key={c} value={c}>{c}</option>
              ))}
            </select>
            <input
              type="text"
              value={b.url_path}
              placeholder="/mcp"
              data-field="binding-url-path"
              onInput={(e) => onUpdate(absIdx, "url_path", (e.currentTarget as HTMLInputElement).value)}
              disabled={readOnly}
            />
            <button type="button" onClick={() => onDelete(absIdx)} disabled={readOnly} data-action="delete-binding">×</button>
          </div>
        );
      })}
      <button type="button" onClick={onAdd} disabled={readOnly} data-action="add-binding">+ Add binding</button>
    </div>
  );
}

// RepeatableStringRows renders an add/delete list of plain string inputs.
// Used for base_args_template and LanguageFormEntry.extra_flags.
function RepeatableStringRows(props: {
  label: string;
  value: string[];
  onChange: (next: string[]) => void;
  disabled?: boolean;
  dataTestId?: string;
}) {
  const { label, value, onChange, disabled, dataTestId } = props;
  return (
    <div class="repeatable-rows" data-testid={dataTestId}>
      {value.map((v, i) => (
        <div class="form-row" key={i}>
          <input
            type="text"
            placeholder={label}
            value={v}
            onInput={(e) => {
              const next = value.slice();
              next[i] = (e.currentTarget as HTMLInputElement).value;
              onChange(next);
            }}
            disabled={disabled}
          />
          <button
            type="button"
            onClick={() => onChange(value.filter((_, j) => j !== i))}
            disabled={disabled}
            data-action={`delete-${dataTestId ?? label}-row`}
          >
            ×
          </button>
        </div>
      ))}
      <button
        type="button"
        onClick={() => onChange([...value, ""])}
        disabled={disabled}
        data-action={`add-${dataTestId ?? label}-row`}
      >
        + Add {label}
      </button>
    </div>
  );
}

// PortPoolField renders the port_pool { start, end } pair.
// Only visible when kind === "workspace-scoped".
function PortPoolField(props: {
  value: { start: number; end: number } | undefined;
  onChange: (pp: { start: number; end: number } | undefined) => void;
  disabled?: boolean;
}) {
  const { value, onChange, disabled } = props;
  const start = value?.start ?? 0;
  const end = value?.end ?? 0;
  function parseN(raw: string): number {
    const n = Number(raw);
    return Number.isFinite(n) && n >= 0 ? Math.trunc(n) : 0;
  }
  return (
    <div class="form-row">
      <label>Port pool</label>
      <input
        type="number"
        min={0}
        max={65535}
        placeholder="start"
        value={value ? start : ""}
        disabled={disabled}
        onInput={(e) => {
          const v = (e.currentTarget as HTMLInputElement).value;
          if (v === "" && end === 0) { onChange(undefined); return; }
          onChange({ start: parseN(v), end });
        }}
        data-field="port-pool-start"
      />
      <span>–</span>
      <input
        type="number"
        min={0}
        max={65535}
        placeholder="end"
        value={value ? end : ""}
        disabled={disabled}
        onInput={(e) => {
          const v = (e.currentTarget as HTMLInputElement).value;
          if (v === "" && start === 0) { onChange(undefined); return; }
          onChange({ start, end: parseN(v) });
        }}
        data-field="port-pool-end"
      />
    </div>
  );
}

// LanguagesSubsection renders a list of LanguageFormEntry rows.
// Each entry has a stable _id assigned at creation time via generateUUID().
// Only visible when kind === "workspace-scoped".
function LanguagesSubsection(props: {
  languages: LanguageFormEntry[];
  onChange: (next: LanguageFormEntry[]) => void;
  disabled?: boolean;
}) {
  const { languages, onChange, disabled } = props;
  function addLanguage() {
    onChange([
      ...languages,
      { _id: generateUUID(), name: "", backend: "", transport: undefined, lsp_command: "", extra_flags: [] },
    ]);
  }
  function updateLanguage<K extends keyof LanguageFormEntry>(idx: number, field: K, value: LanguageFormEntry[K]) {
    const next = languages.slice();
    next[idx] = { ...next[idx], [field]: value };
    onChange(next);
  }
  function deleteLanguage(idx: number) {
    onChange(languages.filter((_, i) => i !== idx));
  }
  return (
    <div class="form-row" data-testid="languages-subsection">
      <label>Languages</label>
      <div style={{ flex: 1 }}>
        {languages.map((lang, idx) => (
          <fieldset class="language-entry" key={lang._id}>
            <legend>Language {idx + 1}</legend>
            <div class="form-row">
              <label>Name</label>
              <input
                type="text"
                placeholder="typescript"
                value={lang.name}
                onInput={(e) => updateLanguage(idx, "name", (e.currentTarget as HTMLInputElement).value)}
                disabled={disabled}
                data-field="language-name"
              />
              <button
                type="button"
                onClick={() => deleteLanguage(idx)}
                disabled={disabled}
                data-action="delete-language"
              >
                ×
              </button>
            </div>
            <div class="form-row">
              <label>Backend</label>
              <input
                type="text"
                placeholder="ts-morph"
                value={lang.backend}
                onInput={(e) => updateLanguage(idx, "backend", (e.currentTarget as HTMLInputElement).value)}
                disabled={disabled}
                data-field="language-backend"
              />
            </div>
            <div class="form-row">
              <label>Transport</label>
              <select
                value={lang.transport ?? ""}
                onChange={(e) => {
                  const v = (e.currentTarget as HTMLSelectElement).value;
                  updateLanguage(idx, "transport", v === "" ? undefined : v as LanguageFormEntry["transport"]);
                }}
                disabled={disabled}
                data-field="language-transport"
              >
                <option value="">(unset)</option>
                <option value="stdio">stdio</option>
                <option value="http_listen">http_listen</option>
                <option value="native_http">native_http</option>
              </select>
            </div>
            <div class="form-row">
              <label>LSP command</label>
              <input
                type="text"
                placeholder="typescript-language-server --stdio"
                value={lang.lsp_command ?? ""}
                onInput={(e) => updateLanguage(idx, "lsp_command", (e.currentTarget as HTMLInputElement).value)}
                disabled={disabled}
                data-field="language-lsp-command"
              />
            </div>
            <div class="form-row">
              <label>Extra flags</label>
              <RepeatableStringRows
                label="flag"
                value={lang.extra_flags ?? []}
                onChange={(next) => updateLanguage(idx, "extra_flags", next.length > 0 ? next : [])}
                disabled={disabled}
                dataTestId={`language-${idx}-extra-flags`}
              />
            </div>
          </fieldset>
        ))}
        <button
          type="button"
          onClick={addLanguage}
          disabled={disabled}
          data-action="add-language"
        >
          + Add language
        </button>
      </div>
    </div>
  );
}

// BindingsMatrix renders a client × daemon matrix for servers with 4+ daemons.
// Rows = KNOWN_CLIENTS, columns = daemons. Each cell holds a checkbox; when
// checked, an inline url_path text input appears in the cell. Toggling a
// checkbox adds or removes the corresponding BindingFormEntry via onToggle.
function BindingsMatrix(props: {
  daemons: DaemonFormEntry[];
  bindings: BindingFormEntry[];
  onToggle: (daemonId: string, client: string, checked: boolean) => void;
  onUpdate: (index: number, field: "client" | "daemonId" | "url_path", value: string) => void;
  readOnly?: boolean;
}) {
  const { daemons, bindings, onToggle, onUpdate } = props;
  return (
    <table class="bindings-matrix" data-testid="bindings-matrix">
      <thead>
        <tr>
          <th>Client</th>
          {daemons.map((d) => (
            <th key={d._id}>{d.name || "(unnamed)"}<br /><small>:{d.port}</small></th>
          ))}
        </tr>
      </thead>
      <tbody>
        {KNOWN_CLIENTS.map((c) => (
          <tr key={c}>
            <td>{c}</td>
            {daemons.map((d) => {
              const absIdx = bindings.findIndex(
                (b) => b.client === c && b.daemonId === d._id,
              );
              const bound = absIdx !== -1;
              const urlPath = bound ? bindings[absIdx].url_path : "";
              return (
                <td key={d._id}>
                  <input
                    type="checkbox"
                    checked={bound}
                    data-action="binding-toggle"
                    data-daemon={d._id}
                    data-client={c}
                    disabled={props.readOnly}
                    onChange={(e) =>
                      onToggle(d._id, c, (e.currentTarget as HTMLInputElement).checked)
                    }
                  />
                  {bound && (
                    <input
                      type="text"
                      value={urlPath}
                      placeholder="/mcp"
                      disabled={props.readOnly}
                      onInput={(e) =>
                        onUpdate(absIdx, "url_path", (e.currentTarget as HTMLInputElement).value)
                      }
                    />
                  )}
                </td>
              );
            })}
          </tr>
        ))}
      </tbody>
    </table>
  );
}

// DaemonExtrasSubsection renders per-daemon context + extra_args fields.
// context is only rendered when kind === "workspace-scoped".
function DaemonExtrasSubsection(props: {
  daemons: DaemonFormEntry[];
  kind: ManifestFormState["kind"];
  onUpdate: (id: string, field: "context" | "extra_args", value: string | string[] | undefined) => void;
  disabled?: boolean;
}) {
  const { daemons, kind, onUpdate, disabled } = props;
  return (
    <div style={{ flex: 1 }}>
      {daemons.map((d) => (
        <fieldset class="daemon-extras-entry" key={d._id}>
          <legend>{d.name || "(unnamed daemon)"}</legend>
          {kind === "workspace-scoped" && (
            <div class="form-row">
              <label>Context</label>
              <input
                type="text"
                placeholder="(unset)"
                value={d.context ?? ""}
                onInput={(e) => {
                  const v = (e.currentTarget as HTMLInputElement).value;
                  onUpdate(d._id, "context", v === "" ? undefined : v);
                }}
                disabled={disabled}
                data-field="daemon-context"
              />
            </div>
          )}
          <div class="form-row">
            <label>Extra args</label>
            <RepeatableStringRows
              label="arg"
              value={d.extra_args ?? []}
              onChange={(next) => onUpdate(d._id, "extra_args", next.length > 0 ? next : undefined)}
              disabled={disabled}
              dataTestId={`daemon-${d._id}-extra-args`}
            />
          </div>
        </fieldset>
      ))}
    </div>
  );
}
