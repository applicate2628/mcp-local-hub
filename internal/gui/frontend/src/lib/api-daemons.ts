// Typed clients for the three new daemon-lifecycle routes added in A4-b
// PR #1 (Task 11). These wrap fetch with the correct credentials policy and
// surface the wire envelopes documented in memo D5 (membership) and D8
// (weekly-schedule).
//
// Why a dedicated module instead of folding into settings-api.ts: the
// daemon routes carry richer error envelopes than the simple
// {error, reason} settings shape. WeeklyScheduleSwapFailure in particular
// carries restore_status + manual_recovery which the SectionDaemons banner
// must surface verbatim — a single jsonOrThrow that flattens to a string
// would lose that structure.

export type MembershipRow = {
  workspace_key: string;
  workspace_path: string;
  language: string;
  weekly_refresh: boolean;
};

export type MembershipDelta = {
  workspace_key: string;
  language: string;
  enabled: boolean;
};

/**
 * GET /api/daemons/weekly-refresh-membership — registry-order snapshot of
 * every (workspace_key, language) row plus its current enrollment flag.
 * The frontend renders the response in the order the server returns; do
 * NOT sort client-side (memo D6).
 */
export async function fetchMembership(): Promise<MembershipRow[]> {
  const r = await fetch("/api/daemons/weekly-refresh-membership", {
    credentials: "same-origin",
  });
  if (!r.ok) throw new Error(`fetchMembership: HTTP ${r.status}`);
  const j = await r.json();
  return (j?.rows ?? []) as MembershipRow[];
}

/**
 * PUT /api/daemons/weekly-refresh-membership — apply a structured-array
 * delta body. Idempotent partial update per memo D5: entries listed are
 * updated, entries not listed are unchanged.
 */
export async function putMembership(
  deltas: MembershipDelta[],
): Promise<{ updated: number; warnings: string[] }> {
  const r = await fetch("/api/daemons/weekly-refresh-membership", {
    method: "PUT",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(deltas),
  });
  if (!r.ok) {
    let body: any = {};
    try {
      body = await r.json();
    } catch {
      /* ignore non-JSON */
    }
    throw new Error(body?.detail ?? body?.error ?? `putMembership: HTTP ${r.status}`);
  }
  const j = await r.json();
  return {
    updated: Number(j?.updated ?? 0),
    warnings: (j?.warnings ?? []) as string[],
  };
}

// --- weekly-schedule envelopes (memo D8) ----------------------------------

/** 200 success — schedule swap committed cleanly; no rollback work. */
export type WeeklyScheduleSuccess = {
  updated: true;
  schedule: string;
  restore_status: "n/a";
};

/** 400 parse error — never crossed the destructive boundary. */
export type WeeklyScheduleParseError = {
  error: "parse_error";
  detail: string;
  example: string;
};

/**
 * 5xx transactional failure envelope. restore_status reports the combined
 * outcome of the helper's scheduler-XML restore + the route handler's
 * settings-YAML rollback (memo D8 step 7 truth table). manual_recovery is
 * present iff restore_status is "degraded" or "failed".
 */
export type WeeklyScheduleSwapFailure = {
  error: "snapshot_unavailable" | "settings_write_failed" | "scheduler_swap_failed";
  detail: string;
  updated: false;
  restore_status: "ok" | "degraded" | "failed" | "n/a";
  manual_recovery?: string;
};

/**
 * PUT /api/daemons/weekly-schedule — transactional swap of the persisted
 * cron string and the Task Scheduler trigger. On 200, returns the success
 * envelope. On 400 parse failures the parse-error envelope is THROWN; on
 * 5xx the swap-failure envelope is THROWN. Callers MUST distinguish via
 * the `error` discriminator (parse_error vs scheduler_swap_failed/etc).
 */
export async function putWeeklySchedule(
  schedule: string,
): Promise<WeeklyScheduleSuccess> {
  const r = await fetch("/api/daemons/weekly-schedule", {
    method: "PUT",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ schedule }),
  });
  let j: any = null;
  try {
    j = await r.json();
  } catch {
    // No JSON body — fabricate a minimal envelope so callers don't
    // crash on JSON.parse; the discriminator path will treat this as a
    // generic swap failure with empty detail.
    j = {};
  }
  if (r.status === 200) return j as WeeklyScheduleSuccess;
  if (r.status === 400) throw j as WeeklyScheduleParseError;
  // 5xx — propagate the structured swap-failure envelope unchanged.
  throw j as WeeklyScheduleSwapFailure;
}
