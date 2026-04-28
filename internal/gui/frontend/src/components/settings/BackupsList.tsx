import { useEffect, useMemo, useState } from "preact/hooks";
import { getBackups, getBackupsCleanPreview } from "../../lib/settings-api";
import type { BackupInfo } from "../../lib/settings-types";
import { BACKUPS_COPY } from "./backups-copy";

export type BackupsListProps = {
  // The keep_n value to preview against. -1 means "no preview yet".
  keepN: number;
};

const CLIENT_ORDER = ["claude-code", "codex-cli", "gemini-cli", "antigravity"];

export function BackupsList({ keepN }: BackupsListProps): preact.JSX.Element {
  const [backups, setBackups] = useState<BackupInfo[] | null>(null);
  const [loadErr, setLoadErr] = useState<string | null>(null);
  const [wouldRemove, setWouldRemove] = useState<Set<string>>(new Set());
  const [previewFailed, setPreviewFailed] = useState(false);

  useEffect(() => {
    let cancelled = false;
    getBackups()
      .then((rows) => { if (!cancelled) setBackups(rows); })
      .catch((e) => { if (!cancelled) setLoadErr(String(e?.message ?? e)); });
    return () => { cancelled = true; };
  }, []);

  // Debounced preview refetch on keepN change.
  useEffect(() => {
    if (keepN < 0) return;
    let cancelled = false;
    const id = setTimeout(async () => {
      try {
        const paths = await getBackupsCleanPreview(keepN);
        if (cancelled) return;
        setWouldRemove(new Set(paths));
        setPreviewFailed(false);
      } catch {
        if (cancelled) return;
        setPreviewFailed(true);
      }
    }, 250);
    return () => { cancelled = true; clearTimeout(id); };
  }, [keepN]);

  const groups = useMemo(() => {
    const m = new Map<string, BackupInfo[]>();
    for (const c of CLIENT_ORDER) m.set(c, []);
    for (const b of backups ?? []) {
      if (!m.has(b.client)) m.set(b.client, []);
      m.get(b.client)!.push(b);
    }
    // Sort each client's backups: originals last, timestamped newest-first.
    for (const arr of m.values()) {
      arr.sort((a, b) => {
        if (a.kind === b.kind) return b.mod_time.localeCompare(a.mod_time);
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
      {Array.from(groups.entries()).map(([client, rows]) => (
        <details key={client} class="backups-client-group" open>
          <summary>{client} ({rows.length} backup{rows.length === 1 ? "" : "s"})</summary>
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
                </li>
              );
            })}
            {rows.length === 0 ? <li class="backups-row empty"><span>No backups for this client.</span></li> : null}
          </ul>
        </details>
      ))}
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
  return new Date(t).toISOString().replace("T", " ").slice(0, 16);
}
