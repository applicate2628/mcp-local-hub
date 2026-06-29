import type { JSX } from "preact";

export interface LoadingStateProps {
  label?: string;
  className?: string;
}

export function LoadingState({
  label = "Loading",
  className = "",
}: LoadingStateProps): JSX.Element {
  const classes = ["loading-state", className].filter(Boolean).join(" ");

  return (
    <div class={classes} role="status" aria-live="polite">
      <span class="visually-hidden">{label}</span>
      <span class="loading-state-skeleton" aria-hidden="true">
        <span class="loading-state-line loading-state-line-wide" />
        <span class="loading-state-line loading-state-line-medium" />
        <span class="loading-state-line loading-state-line-short" />
      </span>
    </div>
  );
}
