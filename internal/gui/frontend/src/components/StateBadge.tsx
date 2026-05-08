type State = "ok" | "empty" | "unsupported" | "error" | "stale";

const labels: Record<State, string> = {
  ok: "ok",
  empty: "empty",
  unsupported: "unsupported",
  error: "error",
  stale: "stale",
};

export function StateBadge({ state }: { state: State }) {
  return (
    <span class={`state-badge state-badge-${state}`} data-testid={`state-badge-${state}`}>
      {labels[state]}
    </span>
  );
}
