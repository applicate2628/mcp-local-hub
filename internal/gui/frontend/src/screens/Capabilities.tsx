// G3 — capability status display screen. Read-only view of the G2
// /api/health snapshot. NO tool execution; items render as labels only.

export function CapabilitiesScreen() {
  return (
    <section class="capabilities-screen" data-testid="capabilities-screen">
      <h1>Capabilities</h1>
      <p>Loading…</p>
    </section>
  );
}
