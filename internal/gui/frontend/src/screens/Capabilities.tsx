import { useEffect, useState } from "preact/hooks";
import { fetchOrThrow } from "../api";
import type { HealthSnapshot } from "../types";

// G3 — capability status display screen. Read-only view of the G2
// /api/health snapshot. NO tool execution; items render as labels only.

type LoadState =
  | { status: "loading" }
  | { status: "ok"; data: HealthSnapshot }
  | { status: "error"; error: string };

export function CapabilitiesScreen() {
  const [state, setState] = useState<LoadState>({ status: "loading" });

  useEffect(() => {
    let cancelled = false;
    fetchOrThrow<HealthSnapshot>("/api/health?include=capabilities", "object")
      .then((data) => {
        if (!cancelled) setState({ status: "ok", data });
      })
      .catch((err: Error) => {
        if (!cancelled) setState({ status: "error", error: err.message });
      });
    return () => {
      cancelled = true;
    };
  }, []);

  if (state.status === "loading") {
    return (
      <section class="capabilities-screen" data-testid="capabilities-screen">
        <h1>Capabilities</h1>
        <p>Loading…</p>
      </section>
    );
  }

  if (state.status === "error") {
    return (
      <section class="capabilities-screen" data-testid="capabilities-screen">
        <h1>Capabilities</h1>
        <p class="error" role="alert">Failed to load capabilities: {state.error}</p>
      </section>
    );
  }

  const caps = state.data.capabilities;
  const rows = caps?.items ?? [];

  if (rows.length === 0) {
    return (
      <section class="capabilities-screen" data-testid="capabilities-screen">
        <h1>Capabilities</h1>
        <p class="capabilities-empty" data-testid="capabilities-empty">
          No capabilities found — install servers via the Add server screen.
        </p>
      </section>
    );
  }

  return (
    <section class="capabilities-screen" data-testid="capabilities-screen">
      <h1>Capabilities</h1>
      <p class="capabilities-empty">{/* placeholder — Phase 4 replaces with cards */}</p>
    </section>
  );
}
