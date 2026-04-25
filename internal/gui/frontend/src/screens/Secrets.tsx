// internal/gui/frontend/src/screens/Secrets.tsx
import { useCallback, useEffect, useRef, useState } from "preact/hooks";
import { useSecretsSnapshot } from "../lib/use-secrets-snapshot";
import { secretsInit, restartSecret } from "../lib/secrets-api";
import type { SecretsEnvelope, SecretRow, SecretsRotateResult, UsageRef } from "../lib/secrets-api";
import { AddSecretModal } from "../components/AddSecretModal";
import { PersistentRotateCTA, RotateResultBanner, RotateSecretModal } from "../components/RotateSecretModal";

const MCPHUB_EDIT_CMD = "mcphub secrets edit";

export function SecretsScreen() {
  const snap = useSecretsSnapshot();

  if (snap.status === "loading") {
    return (
      <section class="secrets-screen">
        <h1>Secrets</h1>
        <p>Loading…</p>
      </section>
    );
  }
  if (snap.status === "error") {
    return (
      <section class="secrets-screen">
        <h1>Secrets</h1>
        <p class="error">Failed to load: {snap.error.message}</p>
        <button type="button" onClick={() => void snap.refresh()}>Retry</button>
      </section>
    );
  }
  const env = snap.data;
  const state = env.vault_state;
  return (
    <section class="secrets-screen">
      <h1>Secrets</h1>
      <EditVaultBanner />
      {state === "missing" && <NotInitView refresh={snap.refresh} />}
      {state === "ok" && env.secrets.length === 0 && <InitEmptyView refresh={snap.refresh} />}
      {state === "ok" && env.secrets.length > 0 && <InitKeyedView env={env} refresh={snap.refresh} />}
      {(state === "decrypt_failed" || state === "corrupt") && <BrokenView env={env} />}
      <ManifestErrorsBanner env={env} />
    </section>
  );
}

function EditVaultBanner() {
  const [copied, setCopied] = useState(false);
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  // Codex Task-5 quality review D4-A: clear pending timer on unmount
  // so setCopied(false) does not run on an unmounted component.
  useEffect(() => () => {
    if (timerRef.current) clearTimeout(timerRef.current);
  }, []);
  return (
    <div class="banner banner-info" data-testid="edit-vault-banner">
      <span>Need bulk operations? Run the CLI command in a terminal: </span>
      <code>{MCPHUB_EDIT_CMD}</code>
      <button
        type="button"
        onClick={async () => {
          try {
            await navigator.clipboard.writeText(MCPHUB_EDIT_CMD);
            setCopied(true);
            if (timerRef.current) clearTimeout(timerRef.current);
            timerRef.current = setTimeout(() => {
              setCopied(false);
              timerRef.current = null;
            }, 1500);
          } catch {
            // ignore — older browsers may reject without permission
          }
        }}
      >
        {copied ? "Copied" : "Copy command"}
      </button>
    </div>
  );
}

function NotInitView(props: { refresh: () => Promise<void> }) {
  const [err, setErr] = useState<string | null>(null);
  const [working, setWorking] = useState(false);
  return (
    <div class="empty-state">
      <p><strong>Secrets vault is not initialized.</strong></p>
      <p>
        ⚠️ Initializing creates your private encryption key at the user-data
        directory. <strong>If you lose this file, all encrypted secrets are
        unrecoverable.</strong> Back it up via password manager or secure copy.
      </p>
      <button
        type="button"
        disabled={working}
        onClick={async () => {
          setWorking(true);
          setErr(null);
          try {
            await secretsInit();
            await props.refresh();
          } catch (e) {
            setErr((e as Error).message);
          } finally {
            setWorking(false);
          }
        }}
      >
        {working ? "Initializing…" : "Initialize secrets vault"}
      </button>
      {err && <p class="error">Init failed: {err}</p>}
    </div>
  );
}

function InitEmptyView(props: { refresh: () => Promise<void> }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <div class="empty-state">
        <p>No secrets yet.</p>
        <button type="button" onClick={() => setOpen(true)}>Add secret</button>
      </div>
      <AddSecretModal open={open} onClose={() => setOpen(false)} onSaved={() => props.refresh()} />
    </>
  );
}

function InitKeyedView(props: { env: SecretsEnvelope; refresh: () => Promise<void> }) {
  const [addOpen, setAddOpen] = useState(false);
  const [prefill, setPrefill] = useState<string | undefined>(undefined);
  // Codex plan-R1 P1: rotateName must NOT be cleared when the modal closes,
  // because the persistent CTA / result banner still need to know which
  // secret was rotated to call POST /api/secrets/<name>/restart. The
  // banner owns its own dismissal, which clears bannerName.
  const [rotateName, setRotateName] = useState<string | null>(null);
  const [bannerName, setBannerName] = useState<string | null>(null);
  const [rotateResult, setRotateResult] = useState<SecretsRotateResult | null>(null);
  const [rotateMode, setRotateMode] = useState<"no-restart" | "with-restart" | null>(null);
  const [_deleteName, _setDeleteName] = useState<string | null>(null); // placeholder for Task 8
  // Codex plan-R2 P1: track running-daemon counts via /api/status so the
  // CTA logic can suppress when 0 are running (memo D4 + Codex memo-R1 P3).
  // Fetch on mount and after each rotation so the count reflects the
  // current world.
  const [runningByServer, setRunningByServer] = useState<Record<string, number>>({});

  const refreshRunning = useCallback(async () => {
    try {
      const resp = await fetch("/api/status");
      if (!resp.ok) return;
      const rows = (await resp.json()) as Array<{ server: string; daemon: string; state: string }>;
      const counts: Record<string, number> = {};
      for (const r of rows) {
        if (r.state === "Running") {
          counts[r.server] = (counts[r.server] ?? 0) + 1;
        }
      }
      setRunningByServer(counts);
    } catch {
      // Best-effort: leave existing map. CTA falls back to refCount-only mode.
    }
  }, []);
  useEffect(() => { void refreshRunning(); }, [refreshRunning]);

  const closeRotate = () => setRotateName(null);
  const dismissBanner = () => { setBannerName(null); setRotateResult(null); setRotateMode(null); };

  const refCountFor = (name: string) =>
    props.env.secrets.find((s) => s.name === name)?.used_by.length ?? 0;

  // Codex plan-R2 P1 + plan-R3 P2: count of *running* daemons of distinct
  // servers that reference this key. Dedupe on server so a manifest with
  // multiple env vars referencing the same secret does not multi-count
  // running daemons.
  const runningCountFor = (name: string): number => {
    const refs = props.env.secrets.find((s) => s.name === name)?.used_by ?? [];
    const distinctServers = new Set<string>();
    for (const r of refs) distinctServers.add(r.server);
    let total = 0;
    for (const server of distinctServers) {
      total += runningByServer[server] ?? 0;
    }
    return total;
  };

  return (
    <div class="secrets-table">
      <button type="button" onClick={() => { setPrefill(undefined); setAddOpen(true); }}>Add secret</button>
      <table>
        <thead>
          <tr>
            <th>Name</th>
            <th>Used by</th>
            <th>State</th>
            <th>Actions</th>
          </tr>
        </thead>
        <tbody>
          {props.env.secrets.map((s) => (
            <SecretRowComponent
              key={s.name}
              row={s}
              onAddPrefill={(n) => { setPrefill(n); setAddOpen(true); }}
              onRotate={(n) => setRotateName(n)}
            />
          ))}
        </tbody>
      </table>
      <AddSecretModal open={addOpen} prefillName={prefill} onClose={() => setAddOpen(false)} onSaved={() => props.refresh()} />

      {rotateName && (
        <RotateSecretModal
          open={true}
          name={rotateName}
          refCount={refCountFor(rotateName)}
          runningCount={runningCountFor(rotateName)}
          onClose={closeRotate}
          onSaved={(result, mode) => {
            setBannerName(rotateName);   // capture name BEFORE rotateName is cleared by closeRotate
            setRotateResult(result);
            setRotateMode(mode);
            void props.refresh();
            void refreshRunning();
          }}
        />
      )}

      {rotateMode === "no-restart" && bannerName && (
        <PersistentRotateCTA
          visible={true}
          secretName={bannerName}
          affectedRunning={runningCountFor(bannerName)}
          onRestart={async () => {
            // Codex plan-R1 P1: surface partial failures from restart-now
            // instead of dismissing unconditionally. The banner stays visible
            // when the user retries; only success or explicit Dismiss clears it.
            const res = await restartSecret(bannerName);
            const failed = res.restart_results.filter((r) => r.error !== "");
            if (failed.length > 0) {
              throw new Error(`${failed.length} of ${res.restart_results.length} daemon(s) still failed: ` +
                failed.map((f) => `${f.task_name}: ${f.error}`).join("; "));
            }
          }}
          onDismiss={dismissBanner}
        />
      )}

      {rotateMode === "with-restart" && bannerName && (
        <RotateResultBanner
          visible={true}
          result={rotateResult}
          onRetry={async () => {
            // Codex plan-R1 P1: retry must update the banner with fresh
            // results (so remaining failures stay listed) instead of
            // dismissing. We swap rotateResult so the banner re-renders.
            const res = await restartSecret(bannerName);
            // Synthesize a SecretsRotateResult-shaped result so the banner
            // renders the same partial-failure UI on retry.
            setRotateResult({ vault_updated: true, restart_results: res.restart_results });
          }}
          onDismiss={dismissBanner}
        />
      )}
    </div>
  );
}

function SecretRowComponent(props: {
  row: SecretRow;
  onAddPrefill: (name: string) => void;
  onRotate: (name: string) => void;
  onDelete?: (name: string) => void;        // optional in Task 7; Task 8 wires it
}) {
  const isPresent = props.row.state === "present";
  const usedByCount = props.row.used_by.length;
  return (
    <tr data-state={props.row.state}>
      <td>{props.row.name}</td>
      <td title={formatUsedBy(props.row.used_by)}>{usedByCount}</td>
      <td>{props.row.state}</td>
      <td>
        <button type="button" disabled={!isPresent} onClick={() => props.onRotate(props.row.name)}>Rotate</button>
        <button type="button" disabled={!isPresent || !props.onDelete} onClick={() => props.onDelete?.(props.row.name)}>Delete</button>
        {props.row.state === "referenced_missing" && (
          <span class="hint">
            {"↳ "}
            <button
              type="button"
              class="linklike"
              onClick={() => props.onAddPrefill(props.row.name)}
            >
              Add this secret
            </button>
          </span>
        )}
      </td>
    </tr>
  );
}

function formatUsedBy(refs: UsageRef[]): string {
  return refs.map((r) => `${r.server} (env: ${r.env_var})`).join("\n");
}

function BrokenView(props: { env: SecretsEnvelope }) {
  return (
    <div class="banner banner-error">
      <p><strong>Vault unavailable</strong> ({props.env.vault_state}). Manifest references shown below as <em>referenced_unverified</em>; vault status cannot be verified.</p>
      <p>Recovery: run <code>mcphub secrets edit</code>, or remove the vault files and re-initialize. <strong>Removing the vault destroys all stored secrets.</strong></p>
      {props.env.secrets.length > 0 && (
        <table>
          <thead>
            <tr><th>Name</th><th>Used by</th></tr>
          </thead>
          <tbody>
            {props.env.secrets.map((s) => (
              <tr key={s.name}>
                <td>{s.name}</td>
                <td title={formatUsedBy(s.used_by)}>{s.used_by.length}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

function ManifestErrorsBanner(props: { env: SecretsEnvelope }) {
  if (props.env.manifest_errors.length === 0) return null;
  return (
    <div class="banner banner-warn" data-testid="manifest-errors-banner">
      <details>
        <summary>{props.env.manifest_errors.length} manifest(s) failed to scan</summary>
        <ul>
          {props.env.manifest_errors.map((e) => (
            <li key={e.path}><code>{e.path}</code>: {e.error}</li>
          ))}
        </ul>
      </details>
    </div>
  );
}
