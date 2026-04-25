// internal/gui/frontend/src/screens/Secrets.tsx
import { useState, useEffect, useRef } from "preact/hooks";
import { useSecretsSnapshot } from "../lib/use-secrets-snapshot";
import { secretsInit } from "../lib/secrets-api";
import type { SecretsEnvelope, SecretRow, UsageRef } from "../lib/secrets-api";
import { AddSecretModal } from "../components/AddSecretModal";

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
            />
          ))}
        </tbody>
      </table>
      <AddSecretModal open={addOpen} prefillName={prefill} onClose={() => setAddOpen(false)} onSaved={() => props.refresh()} />
    </div>
  );
}

function SecretRowComponent(props: { row: SecretRow; onAddPrefill: (name: string) => void }) {
  const isPresent = props.row.state === "present";
  const usedByCount = props.row.used_by.length;
  return (
    <tr data-state={props.row.state}>
      <td>{props.row.name}</td>
      <td title={formatUsedBy(props.row.used_by)}>{usedByCount}</td>
      <td>{props.row.state}</td>
      <td>
        <button type="button" disabled={!isPresent} onClick={() => console.log(`Rotate ${props.row.name} — Task 7`)}>Rotate</button>
        <button type="button" disabled={!isPresent} onClick={() => console.log(`Delete ${props.row.name} — Task 8`)}>Delete</button>
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
