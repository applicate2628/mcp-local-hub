import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { BLANK_FORM, hasNestedUnknown, parseYAMLToForm, toYAML } from "../lib/manifest-yaml";
import type { RouterState } from "../hooks/useRouter";
import { useDebouncedValue } from "../hooks/useDebouncedValue";
import {
  getCatalogNames,
  getExtractManifest,
  getManifest,
  postManifestCreate,
  postManifestEdit,
  postManifestValidate,
  checkDraftReadiness,
  ManifestHashMismatchError,
  type ReadinessReport,
} from "../api";
import { generateUUID } from "../lib/uuid";
import type { BindingFormEntry, DaemonFormEntry, LanguageFormEntry, ManifestFormState } from "../types";
import { useSecretsSnapshot } from "../lib/use-secrets-snapshot";
import { AddSecretModal } from "../components/AddSecretModal";
import { SecretPicker } from "../components/SecretPicker";
import { BrokenRefsSummary } from "../components/BrokenRefsSummary";
import { ReadinessPanel } from "../components/ReadinessPanel";
import { addSecret, getSecrets, secretsInit } from "../lib/secrets-api";
import { inlineSecretsToWrite, secretRefKeys } from "../lib/inline-secrets";
import { hasSecretKey, isSecretRef } from "../lib/secret-ref";
import { ALL_CLIENTS, CORE_CLIENTS, NON_CORE_CLIENTS } from "../lib/routing";
import { pushToast } from "../lib/toast-store";

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
// KNOWN_CLIENTS is the full superset of client ids a GUI-authored manifest
// may bind. It is the single source of truth shared with the Servers matrix /
// scan routing (internal/gui/frontend/src/lib/routing.ts ALL_CLIENTS) and the
// backend registry (internal/clients SupportedClientNames). The binding editor
// exposes ALL of them — the original seven CORE_CLIENTS plus every opt-in
// NON_CORE_CLIENTS adapter — so a new manifest can target any supported
// client. The opt-in distinction is preserved in the <select> UI via a
// separate "opt-in clients" optgroup (see BindingsList), but every client is
// selectable.
const KNOWN_CLIENTS = ALL_CLIENTS;

// deepEqualForm compares two ManifestFormState instances structurally. Used
// by the Q8 dirty check. JSON.stringify is defensible for this shape: all
// fields are serializable primitives, arrays, and plain objects with no
// Date/Map/Set/functions. If a future field breaks that assumption, switch
// to a proper deep-equal import and update the test.
function deepEqualForm(a: ManifestFormState, b: ManifestFormState): boolean {
  return JSON.stringify(a) === JSON.stringify(b);
}

// parseAddServerQuery extracts the create-mode prefill params from the current
// hash. Two DISJOINT branches:
//   • A1 Migration Create-manifest: ?server=<name>&from-client=<client> — both
//     present → the extract-manifest prefill fetch (getExtractManifest).
//   • D2 cold re-enable Re-add: ?readd=<name> — a NAME-ONLY hint. The disabled
//     cursor/vscode object-member was HARD-DELETED on disable, so mcphub never
//     held its value; the ONLY secret-safe source is the catalog by server-name.
//     This branch NEVER fires getExtractManifest (the extract path is dead
//     post-delete — it would 404, and it carries the client's env VERBATIM,
//     which is exactly the literal-secret echo D2 must not do).
export function parseAddServerQuery(): { server: string; fromClient: string; readd: string } {
  const hash = window.location.hash;
  const q = hash.split("?")[1] ?? "";
  const params = new URLSearchParams(q);
  return {
    server: params.get("server") ?? "",
    fromClient: params.get("from-client") ?? "",
    readd: params.get("readd") ?? "",
  };
}

function isManifestNotFoundError(err: unknown): boolean {
  const status = (err as { status?: unknown } | null)?.status;
  const code = (err as { code?: unknown } | null)?.code;
  const message = err instanceof Error ? err.message : String(err ?? "");
  // A typed APIError (status/code present) is AUTHORITATIVE — do not fall through
  // to the brittle message regex, which could wrongly match a coded
  // non-not-found error whose message happens to contain "not found".
  if (typeof status === "number" || typeof code === "string") {
    return status === 404 || code === "MANIFEST_NOT_FOUND";
  }
  // Legacy untyped error (no status/code): the message heuristic is the only signal.
  return /\b404\b/i.test(message) || /manifest_not_found|not found|does not exist/i.test(message);
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
  const [committedCreate, setCommittedCreate] = useState<{ name: string; hash: string } | null>(null);
  // editName is derived from route.query so that a dirty-declined name=a →
  // name=b navigation does not fire a stale load (the memo dep stays stable).
  const editName = useMemo(() => {
    if (props.mode !== "edit") return "";
    const params = new URLSearchParams(props.route?.query ?? "");
    return params.get("name") ?? "";
  }, [props.mode, props.route?.query]);
  const readinessEditName = mode === "edit" ? editName : committedCreate?.name ?? "";
  const debouncedState = useDebouncedValue(formState, 150);
  const yamlPreview = toYAML(debouncedState);

  const snapshot = useSecretsSnapshot();

  // Live install-readiness on the debounced draft YAML (epic install-and-it-
  // works, area 1): the ReadinessPanel shows what blocks/advises the install
  // BEFORE it fails later, and renders inline fields for unset optional secrets.
  const [readiness, setReadiness] = useState<ReadinessReport | null>(null);
  const [readinessLoading, setReadinessLoading] = useState(false);
  const readinessReqRef = useRef(0);
  // inlineSecrets maps a vault key → the plaintext the operator typed in the
  // readiness panel; persisted to the vault just before install.
  const [inlineSecrets, setInlineSecrets] = useState<Record<string, string>>({});
  const presentSecretKeyToken = useMemo(
    () => JSON.stringify(
      (snapshot.data?.secrets ?? [])
        .filter((s) => s.state === "present")
        .map((s) => s.name)
        .sort(),
    ),
    [snapshot.data?.secrets],
  );
  const presentSecretKeys = useMemo(
    () => new Set(JSON.parse(presentSecretKeyToken) as string[]),
    [presentSecretKeyToken],
  );
  // manifestDirty = the editable form differs from the last saved snapshot. It
  // gates Reinstall (which installs the LAST SAVED manifest) — once the form
  // diverges, a Reinstall would install stale config while persisting current
  // refs, so it must be disabled until re-saved (Codex #378 r6).
  const manifestDirty = !deepEqualForm(formState, initialSnapshot);
  // hasPendingInlineSecret counts inline values that persistInlineSecrets would
  // carry into its save-time confirmation: current valid secret: refs with typed
  // plaintext. A local present snapshot must not suppress a write when the live
  // readiness report is already saying the ref is missing; persistInlineSecrets
  // rechecks the vault at click time before deciding to skip or write.
  const hasPendingInlineSecret = pendingInlineSecretEntries(formState).length > 0;
  // isDirty (sidebar/close navigation guard) — a value typed into a readiness
  // inline secret field is unsaved data too, so warn before discarding it (r4),
  // but only while it is still a live ref (r6).
  const isDirty = manifestDirty || hasPendingInlineSecret;
  const currentSecretRefToken = useMemo(() => JSON.stringify(secretRefKeys(formState)), [formState]);
  useEffect(() => {
    if (!yamlPreview || !debouncedState.name.trim()) {
      // Invalidate any in-flight check AND clear loading, so a late response for
      // a since-cleared draft cannot land a stale report (Codex #378 r2).
      readinessReqRef.current++;
      setReadiness(null);
      setReadinessLoading(false);
      return;
    }
    const reqId = ++readinessReqRef.current;
    setReadinessLoading(true);
    checkDraftReadiness(yamlPreview, { mode, editName: readinessEditName })
      .then((rep) => {
        if (reqId === readinessReqRef.current) setReadiness(rep);
      })
      .catch(() => {
        // The draft is currently unparseable (400) or unreachable → drop the
        // now-stale report rather than leaving an out-of-date panel up (Codex
        // #378 r2).
        if (reqId === readinessReqRef.current) setReadiness(null);
      })
      .finally(() => {
        if (reqId === readinessReqRef.current) setReadinessLoading(false);
      });
    // snapshot.fetchedAt changes on every successful vault refresh, so writing a
    // secret (inline OR via the AddSecretModal) re-runs this effect and the
    // panel reflects the now-resolved secret instead of a stale advisory
    // (Codex #378).
  }, [yamlPreview, debouncedState.name, snapshot.fetchedAt, mode, readinessEditName]);

  // pendingInlineSecretEntries is the SINGLE OWNER of "which inline values are
  // candidates for persistence": current valid-named secret: refs
  // (inlineSecretsToWrite drops a value typed for a ref the user later
  // removed/renamed, or a non-conforming/reserved key). It deliberately does NOT
  // subtract local snapshot-present keys: the snapshot can be stale-present while
  // live readiness is already missing that key. persistInlineSecrets performs the
  // save-time presence check and otherwise lets addSecret race-safely return
  // SECRETS_KEY_EXISTS.
  function pendingInlineSecretEntries(state: ManifestFormState): [string, string][] {
    return inlineSecretsToWrite(inlineSecrets, state);
  }

  useEffect(() => {
    const refs = new Set(JSON.parse(currentSecretRefToken) as string[]);
    setInlineSecrets((prev) => {
      let changed = false;
      const next: Record<string, string> = {};
      for (const [key, value] of Object.entries(prev)) {
        if (refs.has(key)) {
          next[key] = value;
        } else {
          changed = true;
        }
      }
      return changed ? next : prev;
    });
  }, [currentSecretRefToken]);

  useEffect(() => {
    if (presentSecretKeys.size === 0) return;
    setInlineSecrets((prev) => {
      let changed = false;
      const next = { ...prev };
      for (const key of Object.keys(next)) {
        if (presentSecretKeys.has(key)) {
          delete next[key];
          changed = true;
        }
      }
      return changed ? next : prev;
    });
  }, [presentSecretKeyToken, presentSecretKeys]);

  function clearInlineSecret(key: string) {
    setInlineSecrets((prev) => {
      const next = { ...prev };
      delete next[key];
      return next;
    });
  }

  async function persistInlineSecrets(state: ManifestFormState): Promise<void> {
    const entries = pendingInlineSecretEntries(state);
    if (entries.length === 0) return;
    let vaultState = snapshot.data?.vault_state;
    let confirmedPresentKeys = new Set<string>();
    try {
      const latest = await getSecrets();
      vaultState = latest.vault_state;
      confirmedPresentKeys = new Set(
        (latest.secrets ?? [])
          .filter((s) => s.state === "present")
          .map((s) => s.name),
      );
    } catch {
      // If the save-time list check is unavailable, do not trust a stale local
      // present snapshot to suppress the user's typed value. Continue to the
      // write path; addSecret will surface the real vault error or a benign 409.
    }
    // A fresh profile has no vault yet — initialize it before the first write, or
    // addSecret fails on the uninitialized vault (Codex #378 r2).
    if (vaultState !== "ok") {
      await secretsInit();
    }
    for (const [key, value] of entries) {
      if (confirmedPresentKeys.has(key)) {
        clearInlineSecret(key);
        continue;
      }
      try {
        await addSecret(key, value);
      } catch (err) {
        // A concurrent creator (another tab / the CLI) may have created the key
        // between the snapshot and now → SECRETS_KEY_EXISTS means it is ALREADY
        // satisfied, so treat it as success instead of aborting the install
        // (Codex #378 r3). Any other error is real and propagates.
        if ((err as { code?: string } | null)?.code !== "SECRETS_KEY_EXISTS") throw err;
      }
      // Clear each on success/already-present so a LATER failure's retry does not
      // re-attempt an already-written key (Codex #378 r2/r3).
      clearInlineSecret(key);
    }
    // Bumps snapshot.fetchedAt → re-runs the readiness effect above so the panel
    // drops the now-resolved secret prompts (Codex #378).
    await snapshot.refresh();
  }
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

  // A1 prefill path (Q8 baseline gotcha): fetch extract-manifest when the
  // user arrives from the Migration Create-manifest button, parse → set form
  // state → take the snapshot AFTER normalization so dirty is false on first
  // render. The `readd` guard keeps this branch DISJOINT from the D2 cold
  // re-enable Re-add branch below (a Re-add hash carries no server/from-client,
  // but the guard makes the no-extract-on-readd invariant explicit + greppable).
  useEffect(() => {
    const { server, fromClient, readd } = parseAddServerQuery();
    if (readd) return; // D2 Re-add owns this mount — never run the extract fetch.
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

  // D2 cold re-enable Re-add path (#/add-server?readd=<name>): a disabled
  // cursor/vscode object-member is HARD-DELETED on disable, so mcphub never held
  // its value — the ONLY secret-safe value source is the catalog by server-name.
  //
  // INVARIANT (no-literal-secret-echo): this branch NEVER calls
  // getExtractManifest. The extract path is dead post-delete (404), and it
  // carries the client's env VERBATIM — re-leaking the literal secret D2 exists
  // to never echo. Two sub-paths, both secret-safe by construction:
  //   • Catalog MATCH → prefill from the SHIPPED manifest (getManifest), whose
  //     env is published with `secret:`/`${env:}` placeholders, never a resolved
  //     literal (e.g. servers/wolfram/manifest.yaml: WOLFRAM_LLM_APP_ID =
  //     "secret:wolfram_app_id"). Command + args come along; secrets stay refs.
  //   • NO catalog match → seed ONLY the name (blank command/args/env) + an
  //     honest banner telling the operator to re-enter command/args/secrets.
  useEffect(() => {
    const { readd } = parseAddServerQuery();
    if (!readd) return;
    let cancelled = false;
    (async () => {
      let inCatalog = false;
      try {
        const names = await getCatalogNames();
        inCatalog = names.has(readd);
      } catch {
        // A catalog lookup failure must not strand the operator: fall through to
        // the honest name-only branch (re-enter manually). The form NEVER
        // receives a literal secret on either branch, so failing closed to blank
        // is safe — it just asks for one extra re-entry.
      }
      if (cancelled) return;
      if (inCatalog) {
        try {
          // Secret-safe: the shipped manifest carries `secret:`/`${env:}`
          // placeholders, not resolved values.
          const { yaml } = await getManifest(readd);
          if (cancelled) return;
          const parsed = parseYAMLToForm(yaml);
          setFormState(parsed);
          setInitialSnapshot(parsed);
          return;
        } catch {
          if (cancelled) return;
          // The catalog claimed it but the manifest read failed — degrade to
          // the same name-only blank + honest banner rather than the extract path.
        }
      }
      // No catalog match (or a catalog/manifest read failure): name-only seed +
      // honest banner. BLANK command/args/env — the operator re-enters secrets
      // via the existing AddSecretModal / secret:<key> refs.
      const seeded = { ...BLANK_FORM, name: readd };
      setFormState(seeded);
      setInitialSnapshot(seeded);
      setBanner({
        kind: "error",
        text: `Re-adding ${readd} — it was removed from your config, so re-enter its command/args/secrets. (Not found in the catalog.)`,
      });
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  const staleRecoveryName = editName || committedCreate?.name || "";

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
    setCommittedCreate(null);
    // Clear inline secrets typed for the PRIOR draft so a value entered for
    // manifest a cannot reappear prefilled in manifest b that references the
    // same missing key (Codex #378 r4 — a→b edit navigation would otherwise
    // write a's secret value for b).
    setInlineSecrets({});
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
    field: "context" | "extra_args" | "cwd",
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
    // retryName is the install TARGET name, NOT a callback: the Retry button
    // invokes the CURRENT render's retryInstall(retryName) so it reads the latest
    // formState / inlineSecrets. Storing a `() => retryInstall(name)` closure
    // instead froze the inline-secret map at banner-creation time, so a value the
    // operator typed AFTER the failure was lost on Retry (Codex #378 r6).
    retryName?: string;
    reinstall?: boolean;
    staleReload?: boolean;
    staleForceSave?: boolean;
    // R4-2 (bot R4): true when the manifest write committed but the live
    // gate-ON hub's in-place republish failed, so /clients + /g are serving a
    // stale snapshot until the operator restarts the hub. Renders a restart
    // notice line under the success banner (mirrors the Groups restart-notice).
    restartHub?: boolean;
  };

  const [banner, setBanner] = useState<Banner | null>(null);
  const [busy, setBusy] = useState<"" | "validate" | "save" | "install">("");
  // submissionVersion: bumped every time a Save/Save&Install/Reload/Force Save
  // click starts its own async mutation pipeline. If a second click
  // happens while the first is still in flight, the older pipeline sees
  // submissionCounter.current != its own captured value and bails before
  // writing to state. (Q3 Codex-identified gotcha.)
  const submissionCounter = useRef(0);
  // validateVersion: same pattern for the async Validate button path. A
  // newer Validate click invalidates an older in-flight validate's result
  // so stale warnings don't paint over fresh state. (Q5.)
  const validateCounter = useRef(0);

  function showStaleHashRecovery() {
    setBanner({
      kind: "error",
      text: "Manifest changed on disk since you opened it. Reload will discard your edits and show the new version. Force Save will overwrite with your version.",
      staleReload: true,
      staleForceSave: true,
    });
  }

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
      // A preempted validation (a newer runValidate bumped validateCounter)
      // must NOT clear busy — that would re-enable Save/Install while the newer
      // validation is still in flight. Only the current validation owns the clear.
      if (version === validateCounter.current) setBusy("");
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
      // R4-2: captures the gate-ON in-place republish-failure signal from the
      // create/edit response so the success banner can prompt a hub restart.
      let restartHub = false;
      const createManifestFromDraft = async (): Promise<boolean> => {
        const { restartRequired } = await postManifestCreate(name, payload);
        if (version !== submissionCounter.current) return false;
        restartHub = restartRequired;
        let hash = "";
        try {
          ({ hash } = await getManifest(name));
        } catch {
          // The create already committed; an empty hash tells the follow-up
          // edit path to re-read before saving instead of attempting create.
          hash = "";
        }
        if (version !== submissionCounter.current) return false;
        setCommittedCreate({ name, hash });
        const postSave: ManifestFormState = { ...payloadState, loadedHash: hash };
        setFormState(postSave);
        setInitialSnapshot(postSave);
        return true;
      };
      if (mode === "edit") {
        try {
          const expectedHash = initialSnapshot.loadedHash;
          const { hash: newHash, restartRequired } = await postManifestEdit(name, payload, expectedHash);
          if (version !== submissionCounter.current) return;
          restartHub = restartRequired;
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
            showStaleHashRecovery();
            return;
          }
          throw err;
        }
      } else {
        if (committedCreate?.name === name) {
          try {
            let expectedHash = committedCreate.hash;
            if (!expectedHash) {
              const fresh = await getManifest(name);
              if (version !== submissionCounter.current) return;
              if (!fresh.hash) {
                throw new Error("/api/manifest/get: success response missing hash field");
              }
              expectedHash = fresh.hash;
            }
            const { hash: newHash, restartRequired } = await postManifestEdit(name, payload, expectedHash);
            if (version !== submissionCounter.current) return;
            restartHub = restartRequired;
            setCommittedCreate({ name, hash: newHash });
            const postSave: ManifestFormState = { ...payloadState, loadedHash: newHash };
            setFormState(postSave);
            setInitialSnapshot(postSave);
          } catch (err) {
            if (version !== submissionCounter.current) return;
            if (err instanceof ManifestHashMismatchError) {
              showStaleHashRecovery();
              return;
            }
            if (isManifestNotFoundError(err)) {
              setCommittedCreate(null);
              if (!(await createManifestFromDraft())) return;
            } else {
              throw err;
            }
          }
        } else {
          if (!(await createManifestFromDraft())) return;
        }
      }
      setWarnings(null);
      if (!opts.install) {
        setBanner({
          kind: "success",
          text: mode === "edit"
            ? `Saved. Daemon still running old config.`
            : `Saved servers/${name}/manifest.yaml. Click Reinstall to install it now.`,
          // Show the Reinstall (persist-pending-secrets + install) path after a
          // plain Save in BOTH modes. In create mode a plain Save otherwise left
          // any inline secret stuck: there was no install path, and the next
          // Save & Install hit "already exists" for the just-created name before
          // reaching the persist step (Codex #378 r6). Reinstall → retryInstall
          // installs the saved manifest with the pending secret.
          reinstall: true,
          restartHub,
        });
        // Flowbite success toast mirroring the inline save banner.
        pushToast("success", `Saved ${name} manifest.`);
        return;
      }
      // Install phase — the manifest is now COMMITTED. Persist inline secrets
      // (AFTER the commit so a failed commit never orphans a vault key — Codex
      // #378 r4) then install. A failure HERE is retryable: the manifest is
      // saved, so a transient vault init/write failure must surface a Retry path
      // (retryInstall persists + re-installs), not a dead-end error that strands
      // the saved manifest behind a future "already exists" (Codex #378 r6). The
      // outer catch is left to handle only manifest-COMMIT failures (manifest not
      // saved → plain error, correctly no Retry).
      try {
        await persistInlineSecrets(payloadState);
        if (version !== submissionCounter.current) return;
        await runInstallNow(name, version);
      } catch (installErr) {
        if (version === submissionCounter.current) {
          setBanner({
            kind: "error",
            text: `Saved servers/${name}/manifest.yaml, but install could not complete: ${(installErr as Error).message}`,
            retryName: name,
          });
        }
      }
    } catch (err) {
      if (version !== submissionCounter.current) return;
      setBanner({ kind: "error", text: (err as Error).message });
    } finally {
      if (version === submissionCounter.current) setBusy("");
    }
  }

  async function runReload() {
    const version = ++submissionCounter.current;
    const name = staleRecoveryName;
    if (!name) return;
    setBusy("save");
    setBanner(null);
    try {
      const { yaml, hash } = await getManifest(name);
      if (version !== submissionCounter.current) return;
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
      if (committedCreate?.name === name) {
        setCommittedCreate({ name, hash });
      }
      // Reload replaces the draft from disk → drop inline secrets typed for the
      // REJECTED local draft so a value entered for the old version cannot linger
      // and later reappear/be written if the reloaded manifest references the same
      // missing key (Codex #378 r6 — same clear as the edit-mount + paste paths).
      setInlineSecrets({});
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
      if (version !== submissionCounter.current) return;
      setBanner({ kind: "error", text: (err as Error).message });
    } finally {
      if (version === submissionCounter.current) setBusy("");
    }
  }

  async function runForceSave() {
    const version = ++submissionCounter.current;
    setBusy("save");
    setBanner(null);
    try {
      // Anchor to the persisted manifest identity (edit URL name or the
      // create-mode name already committed), not mutable formState.name.
      const name = staleRecoveryName;
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
      const { hash: newHash, restartRequired } = await postManifestEdit(name, payload, fresh.hash);
      if (version !== submissionCounter.current) return;
      // 6. Atomic baseline update.
      const postSave: ManifestFormState = { ...merged, loadedHash: newHash };
      setFormState(postSave);
      setInitialSnapshot(postSave);
      if (committedCreate?.name === name) {
        setCommittedCreate({ name, hash: newHash });
      }
      const preservedKeys = Object.keys(freshParsed._preservedRaw);
      setBanner({
        kind: "success",
        text:
          preservedKeys.length > 0
            ? `Force-saved. Preserved external fields: ${preservedKeys.join(", ")}.`
            : `Force-saved.`,
        reinstall: true,
        restartHub: restartRequired,
      });
      pushToast("success", `Force-saved ${name} manifest.`);
    } catch (err) {
      if (version !== submissionCounter.current) return;
      setBanner({ kind: "error", text: `Force Save failed: ${(err as Error).message}` });
      pushToast("danger", `Force Save failed: ${(err as Error).message}`);
    } finally {
      if (version === submissionCounter.current) setBusy("");
    }
  }

  // retryInstall is the SINGLE owner of "persist any pending inline secrets, then
  // install". Used by both the install-failure Retry banner AND the edit-success
  // Reinstall banner: each re-enters install OUTSIDE the runSave pipeline, so
  // without this an inline secret the operator entered/changed while fixing the
  // failure (or before a plain Save) would stay only in component state and the
  // install would run without it (Codex #378 r4/r6).
  async function retryInstall(name: string) {
    const version = ++submissionCounter.current;
    // Set busy so the readiness inline inputs + Save/Install buttons (all gated on
    // busy !== "") are disabled WHILE the retry persists + installs. Without it the
    // operator could edit a password mid-retry; the running persist already
    // snapshotted the old inlineSecrets, so the new value would be dropped/clobbered
    // (Codex #378 r6).
    setBusy("install");
    try {
      await persistInlineSecrets(formState);
      if (version !== submissionCounter.current) return;
      await runInstallNow(name, version);
    } catch (err) {
      if (version === submissionCounter.current) {
        // The manifest is already saved at this point, so a persist/install
        // failure (transient vault init/write, connection reset) must stay
        // RETRYABLE — a dead-end error would strand the saved manifest, and a
        // fresh Save & Install would hit "already exists" (Codex #378 r6).
        setBanner({
          kind: "error",
          text: `Saved servers/${name}/manifest.yaml, but install could not complete: ${(err as Error).message}`,
          retryName: name,
        });
      }
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
          retryName: name,
        });
        pushToast("danger", `Install failed for ${name}: ${err}`);
        return;
      }
      setWarnings(null);
      setBanner({ kind: "success", text: `Installed ${name}. Daemons will start at next logon (or run "mcphub restart --server ${name}" now).` });
      pushToast("success", `Installed ${name}.`);
    } catch (err) {
      if (version !== submissionCounter.current) return;
      setBanner({
        kind: "error",
        text: `Saved servers/${name}/manifest.yaml, but install threw: ${(err as Error).message}`,
        // Route through retryInstall (not runInstallNow) so a Retry persists any
        // inline secret the operator enters/edits while fixing the failure before
        // re-installing the saved manifest (Codex #378 r6).
        retryName: name,
      });
      pushToast("danger", `Install failed for ${name}: ${(err as Error).message}`);
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
    // A wholesale paste replaces the draft → drop inline secrets typed for the
    // previous draft so they cannot be written under the pasted manifest's name
    // (Codex #378 r4).
    setInlineSecrets({});
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
      // A preempted validation (a newer runValidate bumped validateCounter)
      // must NOT clear busy — that would re-enable Save/Install while the newer
      // validation is still in flight. Only the current validation owns the clear.
      if (version === validateCounter.current) setBusy("");
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
            <button type="button" class="btn btn-secondary" onClick={() => { setLoadError(null); window.location.reload(); }}>Retry</button>
            <button type="button" class="btn btn-secondary" onClick={() => { window.location.hash = "#/servers"; }}>Back to Servers</button>
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
            <button type="button" class="btn btn-secondary" onClick={() => { window.location.hash = "#/servers"; }}>Back to Servers</button>
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
          {banner.restartHub && (
            <p class="restart-hub-notice" data-testid="restart-hub-notice">
              The aggregated hub is running but could not refresh its routing in place — restart the hub (Settings → Expose a single aggregated hub URL, toggle off and on) to apply this change to /clients and /g endpoints.
            </p>
          )}
          {banner.retryName && (
            <button
              type="button"
              // Retry re-installs the LAST SAVED manifest, and retryInstall persists
              // inline secrets from the CURRENT form. Once the operator edits the
              // draft (manifestDirty), retrying would write secrets for unsaved refs
              // that the saved-manifest install never uses (orphans). Disable it —
              // the edited manifest must go through Save & Install, not Retry (#378 r6).
              disabled={busy !== "" || manifestDirty}
              title={manifestDirty ? "Save your changes first — Retry re-installs the last saved manifest." : undefined}
              // Call the CURRENT render's retryInstall with the stored target so it
              // reads the latest inlineSecrets, not a closure frozen at failure time.
              onClick={() => retryInstall(banner.retryName!)}
              data-action="retry-install"
              class="btn btn-secondary"
            >
              Retry Install
            </button>
          )}
          {banner.reinstall && (
            <button
              type="button"
              // Reinstall installs the LAST SAVED manifest via retryInstall (which
              // persists pending inline secrets first — Codex #378 r3). Disabled
              // once the form diverges from the saved snapshot (manifestDirty): a
              // Reinstall then would install stale config while persisting the
              // current, unsaved refs — an orphaned secret + a mismatched install.
              // The operator must Save again (which re-arms a clean banner) (r6).
              disabled={busy !== "" || manifestDirty}
              title={manifestDirty ? "Save your changes first — Reinstall applies the last saved manifest." : undefined}
              onClick={() => retryInstall(formState.name.trim())}
              data-action="reinstall"
              class="btn btn-secondary"
            >
              Reinstall
            </button>
          )}
          {banner.staleReload && (
            <button type="button" class="btn btn-secondary" disabled={busy !== ""} onClick={() => runReload()} data-action="reload">Reload</button>
          )}
          {banner.staleForceSave && (
            <button type="button" class="btn btn-danger" disabled={busy !== ""} onClick={() => runForceSave()} data-action="force-save">Force Save</button>
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
      <div class="card">
        <div class="add-server-grid">
          <div class="add-server-form">
          <AccordionSection key="basics" title="Basics" open={true}>
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

          <AccordionSection key="command" title="Command">
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
                    <button type="button" class="danger" onClick={() => deleteBaseArg(i)} disabled={readOnly} data-action="delete-base-arg">×</button>
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

          {(readiness || readinessLoading) && (
            <AccordionSection key="install-readiness" title="Install readiness" open>
              <ReadinessPanel
                report={readiness}
                loading={readinessLoading}
                error={null}
                inlineSecrets={inlineSecrets}
                onInlineSecretChange={(key, value) =>
                  setInlineSecrets((prev) => ({ ...prev, [key]: value }))
                }
                readOnly={readOnly}
                inputsDisabled={busy !== ""}
              />
            </AccordionSection>
          )}

          <AccordionSection key="environment" title="Environment">
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
                  <button type="button" class="danger" onClick={() => deleteEnv(i)} disabled={readOnly} data-action="delete-env">×</button>
                </div>
              ))}
              <button type="button" onClick={addEnv} disabled={readOnly} data-action="add-env">+ Add environment variable</button>
            </div>
          </AccordionSection>
          <AccordionSection key="daemons" title="Daemons">
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
                  <button type="button" class="danger" onClick={() => deleteDaemon(i)} disabled={readOnly} data-action="delete-daemon">×</button>
                </div>
              ))}
              <button type="button" onClick={addDaemon} disabled={readOnly} data-action="add-daemon">+ Add daemon</button>
            </div>
          </AccordionSection>
          <AccordionSection key="client-bindings" title="Client bindings">
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
          <AccordionSection key="advanced" title="Advanced">
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
              <optgroup label="Default clients">
                {CORE_CLIENTS.map((c) => (
                  <option key={c} value={c}>{c}</option>
                ))}
              </optgroup>
              <optgroup label="Opt-in clients">
                {NON_CORE_CLIENTS.map((c) => (
                  <option key={c} value={c}>{c} (opt-in)</option>
                ))}
              </optgroup>
            </select>
            <input
              type="text"
              value={b.url_path}
              placeholder="/mcp"
              data-field="binding-url-path"
              onInput={(e) => onUpdate(absIdx, "url_path", (e.currentTarget as HTMLInputElement).value)}
              disabled={readOnly}
            />
            <button type="button" class="danger" onClick={() => onDelete(absIdx)} disabled={readOnly} data-action="delete-binding">×</button>
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
            class="danger"
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
                class="danger"
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
// Rows = KNOWN_CLIENTS (all supported clients: CORE_CLIENTS + opt-in
// NON_CORE_CLIENTS, the latter flagged with an "(opt-in)" tag), columns =
// daemons. Each cell holds a checkbox; when
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
          <tr key={c} class={(NON_CORE_CLIENTS as readonly string[]).includes(c) ? "binding-row-optin" : undefined}>
            <td>{c}{(NON_CORE_CLIENTS as readonly string[]).includes(c) ? <small class="optin-tag"> (opt-in)</small> : null}</td>
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

// DaemonExtrasSubsection renders per-daemon context + cwd + extra_args fields.
// context is only rendered when kind === "workspace-scoped"; cwd applies to
// every daemon (global + workspace-scoped) and must be an absolute path.
function DaemonExtrasSubsection(props: {
  daemons: DaemonFormEntry[];
  kind: ManifestFormState["kind"];
  onUpdate: (id: string, field: "context" | "extra_args" | "cwd", value: string | string[] | undefined) => void;
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
            <label>Working dir (cwd)</label>
            <input
              type="text"
              placeholder="(inherit) — absolute path"
              value={d.cwd ?? ""}
              onInput={(e) => {
                const v = (e.currentTarget as HTMLInputElement).value;
                onUpdate(d._id, "cwd", v === "" ? undefined : v);
              }}
              disabled={disabled}
              data-field="daemon-cwd"
            />
          </div>
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
