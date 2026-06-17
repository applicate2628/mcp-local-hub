import { useEffect, useMemo, useState } from "preact/hooks";
import {
  cleanBackupsForClient,
  deleteBackup,
  getBackups,
  getBackupsCleanPreview,
  restoreBackup,
} from "../../lib/settings-api";
import type { BackupInfo } from "../../lib/settings-types";
import { BACKUPS_COPY } from "./backups-copy";
import { CORE_CLIENTS, WAVE2_CLIENTS } from "../../lib/routing";
import { ConfirmModal } from "../ConfirmModal";

export type BackupsListProps = {
  // The keep_n value to preview against. -1 means "no preview yet".
  keepN: number;
  // Bug-bash B2 closure (#21): notify parent (SectionBackups) that a
  // per-client Clean fired so it can re-trigger the settings snapshot
  // refresh (which re-fetches /api/backups via this component). Optional;
  // defaults to no-op so consumers that don't care about the refresh
  // can omit it.
  onClientCleaned?: (client: string) => void;
};

// CLIENT_ORDER seeds the always-visible group order. The seven CORE_CLIENTS
// render unconditionally (stable group set, even with zero backups) — same
// always-on posture the Servers matrix gives core columns. The eight opt-in
// WAVE2_CLIENTS (zed/kiro/windsurf/cline/kilocode/opencode/hermes/openclaw)
// are DETECTION-GATED: a wave-2 group appears only when that client actually
// has backups on disk (see the grouping pass below), so an operator who never
// installed a niche client sees no empty group for it. The shared client-list
// constants live in internal/gui/frontend/src/lib/routing.ts (the single
// source of truth also used by the matrix) — do not re-hardcode the list here.
const CLIENT_ORDER: readonly string[] = CORE_CLIENTS;
const WAVE2_ORDER: readonly string[] = WAVE2_CLIENTS;

export function BackupsList({
  keepN,
  onClientCleaned = () => {},
}: BackupsListProps): preact.JSX.Element {
  const [backups, setBackups] = useState<BackupInfo[] | null>(null);
  const [loadErr, setLoadErr] = useState<string | null>(null);
  const [wouldRemove, setWouldRemove] = useState<Set<string>>(new Set());
  const [previewFailed, setPreviewFailed] = useState(false);
  // Bug-bash B2 closure (#21): per-client clean state. `cleaningClient`
  // disables ALL clean buttons (avoid concurrent prunes); `cleanErr`
  // holds the last per-client error keyed by client id.
  const [cleaningClient, setCleaningClient] = useState<string | null>(null);
  const [perClientErr, setPerClientErr] = useState<Record<string, string>>({});
  // refreshTick bumps when a per-client clean succeeds, so the
  // backups list re-fetches without depending on the parent's
  // snapshot.refresh cycle (which is gated on the keepN dirty state).
  const [refreshTick, setRefreshTick] = useState(0);
  // #2 per-timestamp restore/delete. Each is gated behind a ConfirmModal:
  // `pending*` holds the {client, row} a dialog is open for (null = closed),
  // `rowBusy` disables every row action while one is in-flight (avoid
  // concurrent live-config writes), and `rowErr` keys the last error by
  // backup path so it renders inline on the offending row.
  const [pendingRestore, setPendingRestore] = useState<BackupInfo | null>(null);
  const [pendingDelete, setPendingDelete] = useState<BackupInfo | null>(null);
  const [rowBusy, setRowBusy] = useState(false);
  const [rowErr, setRowErr] = useState<Record<string, string>>({});
  const [rowOk, setRowOk] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    getBackups()
      .then((rows) => { if (!cancelled) setBackups(rows); })
      .catch((e) => { if (!cancelled) setLoadErr(String(e?.message ?? e)); });
    return () => { cancelled = true; };
  }, [refreshTick]);

  async function cleanThisClient(client: string) {
    setCleaningClient(client);
    setPerClientErr((prev) => {
      const next = { ...prev };
      delete next[client];
      return next;
    });
    try {
      // Pass the live preview value (keepN) so the clean deletes exactly the
      // rows the per-client "(N)" badge counted — WYSIWYG with the preview,
      // independent of whether keep_n was Saved (Bug #2: pre-fix the clean read
      // the persisted setting and silently pruned nothing when the unsaved
      // slider diverged from it).
      await cleanBackupsForClient(client, keepN);
      setRefreshTick((n) => n + 1);
      onClientCleaned(client);
    } catch (e: unknown) {
      const msg =
        e instanceof Error
          ? e.message
          : typeof e === "string"
          ? e
          : "clean failed";
      setPerClientErr((prev) => ({ ...prev, [client]: msg }));
    } finally {
      setCleaningClient(null);
    }
  }

  function rowErrMsg(e: unknown): string {
    return e instanceof Error
      ? e.message
      : typeof e === "string"
      ? e
      : "action failed";
  }

  async function doRestore(b: BackupInfo) {
    setRowBusy(true);
    setRowOk(null);
    setRowErr((prev) => {
      const next = { ...prev };
      delete next[b.path];
      return next;
    });
    try {
      const res = await restoreBackup(b.client, b.path);
      setPendingRestore(null);
      // A restore both rewrites the live config AND (server-side) writes a
      // fresh safety snapshot of the prior config, so the backup list
      // changed — re-fetch it.
      setRefreshTick((n) => n + 1);
      onClientCleaned(b.client);
      setRowOk(
        res.snapshot
          ? `Restored ${b.client}. Previous config saved as a new backup.`
          : `Restored ${b.client}.`,
      );
    } catch (e: unknown) {
      setPendingRestore(null);
      setRowErr((prev) => ({ ...prev, [b.path]: rowErrMsg(e) }));
    } finally {
      setRowBusy(false);
    }
  }

  async function doDelete(b: BackupInfo) {
    setRowBusy(true);
    setRowOk(null);
    setRowErr((prev) => {
      const next = { ...prev };
      delete next[b.path];
      return next;
    });
    try {
      await deleteBackup(b.client, b.path);
      setPendingDelete(null);
      setRefreshTick((n) => n + 1);
      onClientCleaned(b.client);
    } catch (e: unknown) {
      setPendingDelete(null);
      setRowErr((prev) => ({ ...prev, [b.path]: rowErrMsg(e) }));
    } finally {
      setRowBusy(false);
    }
  }

  // Debounced preview refetch on keepN change.
  // Codex pre-push P2: clear `wouldRemove` synchronously when keepN changes
  // AND on fetch failure. Without this, the previous keep_n's eligible-row
  // markers linger across the new keep_n's debounce window OR alongside the
  // "Preview unavailable" inline message — both surface stale "Would be
  // eligible for cleanup" badges that contradict the current UI state.
  useEffect(() => {
    if (keepN < 0) return;
    setWouldRemove(new Set()); // clear stale markers immediately on keepN change
    let cancelled = false;
    const id = setTimeout(async () => {
      try {
        const paths = await getBackupsCleanPreview(keepN);
        if (cancelled) return;
        setWouldRemove(new Set(paths));
        setPreviewFailed(false);
      } catch {
        if (cancelled) return;
        setWouldRemove(new Set()); // clear stale markers on preview failure
        setPreviewFailed(true);
      }
    }, 250);
    return () => { cancelled = true; clearTimeout(id); };
  }, [keepN]);

  const groups = useMemo(() => {
    const m = new Map<string, BackupInfo[]>();
    // Seed the seven CORE_CLIENTS unconditionally so the core group set is
    // stable even on a host with no backups for some of them.
    for (const c of CLIENT_ORDER) m.set(c, []);
    // Detection-gate the eight opt-in WAVE2_CLIENTS: pre-seed a wave-2 group
    // ONLY when that client actually has at least one backup row, and do so in
    // the canonical WAVE2_ORDER (the data-driven insertion order below is not
    // deterministic across /api/backups responses). A wave-2 client with zero
    // backups adds no empty group. Any future/unknown client id still in the
    // payload falls through to the per-row m.has() insert so it is never dropped.
    const clientsWithBackups = new Set((backups ?? []).map((b) => b.client));
    for (const c of WAVE2_ORDER) {
      if (clientsWithBackups.has(c)) m.set(c, []);
    }
    for (const b of backups ?? []) {
      if (!m.has(b.client)) m.set(b.client, []);
      m.get(b.client)!.push(b);
    }
    // Sort each client's backups: originals last, timestamped newest-first.
    for (const arr of m.values()) {
      arr.sort((a, b) => {
        if (a.kind === b.kind) {
          // Codex r8 P3: parse to epoch — RFC3339 string compare is wrong
          // across timezone-offset differences (DST -07:00 vs -08:00).
          const ta = Date.parse(a.mod_time);
          const tb = Date.parse(b.mod_time);
          return tb - ta; // newest first
        }
        return a.kind === "original" ? 1 : -1;
      });
    }
    return m;
  }, [backups]);

  if (loadErr) {
    return <p class="error-banner">Could not load backups: {loadErr}</p>;
  }
  if (backups === null) {
    return <p>Loading backups…</p>;
  }

  return (
    <div class="backups-list">
      <p class="backups-group-note">{BACKUPS_COPY.groupNote}</p>
      {previewFailed ? (
        <p class="backups-preview-unavailable" data-testid="preview-unavailable">{BACKUPS_COPY.previewFailureInline}</p>
      ) : null}
      {Array.from(groups.entries()).map(([client, rows]) => {
        // Bug-bash B2 closure (#21): count this client's eligible rows
        // for the per-client Clean button label. Disable when there's
        // nothing to prune, when ANOTHER client is currently cleaning,
        // or when the preview hasn't loaded yet.
        const clientEligibleCount = rows.filter(
          (b) => b.kind === "timestamped" && wouldRemove.has(b.path),
        ).length;
        const cleanDisabled =
          cleaningClient !== null ||
          clientEligibleCount === 0 ||
          previewFailed;
        const clientErr = perClientErr[client];
        return (
          <details key={client} class="backups-client-group" open>
            <summary>
              {client} ({rows.length} backup{rows.length === 1 ? "" : "s"})
            </summary>
            <ul>
              {rows.map((b) => {
                const eligible = b.kind === "timestamped" && wouldRemove.has(b.path);
                return (
                  <li
                    key={b.path}
                    class={`backups-row ${b.kind} ${eligible ? "eligible" : ""}`}
                    data-eligible={eligible ? "true" : "false"}
                  >
                    <span class="backups-row-when">{relTime(b.mod_time)}</span>
                    <span class={`backups-row-kind kind-${b.kind}`}>{b.kind}</span>
                    <span class="backups-row-size">{formatBytes(b.size_byte)}</span>
                    {eligible ? (
                      <span class="backups-eligible-badge" data-testid="eligible-badge">
                        {BACKUPS_COPY.rowBadge}
                      </span>
                    ) : null}
                    <span class="backups-row-actions">
                      <button
                        type="button"
                        class="backups-row-restore"
                        disabled={rowBusy}
                        data-testid={`restore-backup-${b.path}`}
                        title={`Overwrite ${b.client}'s live config from this backup. Your current config is saved as a new backup first.`}
                        onClick={() => { setRowOk(null); setPendingRestore(b); }}
                      >
                        Restore
                      </button>
                      <button
                        type="button"
                        class="btn-danger backups-row-delete"
                        disabled={rowBusy}
                        data-testid={`delete-backup-${b.path}`}
                        title={`Permanently delete this backup file for ${b.client}.`}
                        onClick={() => { setRowOk(null); setPendingDelete(b); }}
                      >
                        Delete
                      </button>
                    </span>
                    {rowErr[b.path] ? (
                      <span class="error-banner backups-row-error" role="alert">
                        {rowErr[b.path]}
                      </span>
                    ) : null}
                  </li>
                );
              })}
              {rows.length === 0 ? (
                <li class="backups-row empty">
                  <span>No backups for this client.</span>
                </li>
              ) : null}
            </ul>
            <div class="backups-client-actions">
              <button
                type="button"
                class="btn-danger"
                disabled={cleanDisabled}
                data-testid={`clean-now-${client}`}
                onClick={() => void cleanThisClient(client)}
                title={
                  clientEligibleCount === 0
                    ? "Nothing to prune at the current keep_n setting."
                    : cleaningClient === client
                    ? "Cleaning…"
                    : `Prune ${clientEligibleCount} eligible backup(s) for ${client} only.`
                }
              >
                {cleaningClient === client
                  ? "Cleaning…"
                  : `Clean ${client} only (${clientEligibleCount})`}
              </button>
              {clientErr ? (
                <span class="error-banner" role="alert">
                  {clientErr}
                </span>
              ) : null}
            </div>
          </details>
        );
      })}

      {rowOk ? (
        <p class="save-banner ok backups-row-ok" role="status">{rowOk}</p>
      ) : null}

      <ConfirmModal
        testId="restore-confirm-modal"
        open={pendingRestore !== null}
        title="Restore this backup?"
        body={
          <>
            This overwrites <strong>{pendingRestore?.client}</strong>'s live
            config with the selected backup. Your current config is saved as a
            new backup first, so you can undo this.
          </>
        }
        confirmLabel="Restore"
        danger
        onConfirm={() => (pendingRestore ? doRestore(pendingRestore) : undefined)}
        onCancel={() => setPendingRestore(null)}
      />

      <ConfirmModal
        testId="delete-confirm-modal"
        open={pendingDelete !== null}
        title="Delete this backup?"
        body={
          <>
            Permanently delete this backup file for{" "}
            <strong>{pendingDelete?.client}</strong>. This cannot be undone. The
            live config and other backups are not affected.
          </>
        }
        confirmLabel="Delete"
        danger
        onConfirm={() => (pendingDelete ? doDelete(pendingDelete) : undefined)}
        onCancel={() => setPendingDelete(null)}
      />
    </div>
  );
}

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`;
  return `${(n / 1024 / 1024).toFixed(1)} MiB`;
}

function relTime(rfc3339: string): string {
  const t = Date.parse(rfc3339);
  if (Number.isNaN(t)) return rfc3339;
  const d = new Date(t);
  const pad = (n: number) => String(n).padStart(2, "0");
  // Codex PR #20 r6 P3: use local time components so users in non-UTC
  // timezones see the time in their own zone, not UTC.
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}
