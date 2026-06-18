import { Component } from "preact";
import type { ComponentChildren, JSX } from "preact";

// ErrorBoundary — G12 resilience.
//
// The Preact frontend previously had NO error boundary anywhere, so a single
// render-time throw in any screen unwound the whole tree and blanked the app
// to a white page with no recovery. For a tool whose only health signal IS
// the GUI, that is a severe gap: one bad screen took out the sidebar nav too,
// leaving the operator no way to reach a working screen short of a manual
// reload they have no reason to expect.
//
// Preact has no hooks-based error boundary; the supported mechanism is a
// CLASS component implementing the lifecycle hooks. Preact 10 supports both
// the static `getDerivedStateFromError` (declarative state transition) and
// the instance `componentDidCatch` (side-effect / info capture); we use the
// static form to flip into the error state and `componentDidCatch` to capture
// the message for display + log it to the console so the failure is still
// diagnosable in devtools. (Verified against preact@10.29.2 in package.json,
// which ships both hooks.)
//
// App wraps the per-SCREEN body with this boundary (keyed on the screen name)
// so a crashing screen renders the recovery UI INSIDE <main> while the
// sidebar/topbar shell stays alive — the operator can still navigate to a
// healthy screen. Keying on the screen name resets the boundary on
// navigation, so a crashed screen does not stay stuck after the user moves
// away.

export type ErrorBoundaryProps = {
  children: ComponentChildren;
  /**
   * Human-readable name of the screen being wrapped, surfaced in the recovery
   * UI so the operator knows which screen failed. Optional — the recovery UI
   * degrades to a generic message when absent.
   */
  screenName?: string;
};

type ErrorBoundaryState = {
  hasError: boolean;
  message: string;
};

export class ErrorBoundary extends Component<ErrorBoundaryProps, ErrorBoundaryState> {
  state: ErrorBoundaryState = { hasError: false, message: "" };

  static getDerivedStateFromError(error: unknown): Partial<ErrorBoundaryState> {
    const message = error instanceof Error ? error.message : String(error);
    return { hasError: true, message };
  }

  componentDidCatch(error: unknown): void {
    // Keep the failure diagnosable: getDerivedStateFromError already flipped
    // the state for the UI, but log here so the original error + stack are
    // still visible in devtools / the captured console instead of being
    // silently swallowed by the boundary.
    // eslint-disable-next-line no-console
    console.error("ErrorBoundary caught a render error:", error);
  }

  render(): JSX.Element {
    if (!this.state.hasError) {
      // Preact's render must return a single node; wrapping children in a
      // fragment keeps callers free to pass multiple children.
      return <>{this.props.children}</>;
    }

    const { screenName } = this.props;
    return (
      <div
        role="alert"
        data-testid="error-boundary"
        class="mx-auto mt-8 max-w-xl rounded-xl border border-app-danger bg-app-card p-6 shadow-sm"
      >
        <h2 class="mb-2 text-lg font-semibold text-app-danger">Something went wrong on this screen</h2>
        {screenName ? (
          <p class="mb-2 text-sm text-app-muted">
            The <strong class="text-app-text">{screenName}</strong> screen failed to render.
          </p>
        ) : null}
        {this.state.message ? (
          <p
            data-testid="error-boundary-message"
            class="mb-4 break-words font-mono text-xs text-app-text"
          >
            {this.state.message}
          </p>
        ) : null}
        <div class="flex flex-wrap gap-3">
          <button
            type="button"
            data-testid="error-boundary-reload"
            class="rounded-lg bg-app-accent px-4 py-2 text-sm font-medium text-white transition-colors hover:opacity-90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-app-accent/50"
            // A render-time throw can leave component state corrupted in ways
            // a soft re-render cannot recover from, so a full reload is the
            // honest, reliable recovery — it re-fetches the bundle and remounts
            // the whole app from a clean slate.
            onClick={() => location.reload()}
          >
            Reload
          </button>
          <a
            href="#/dashboard"
            data-testid="error-boundary-dashboard"
            class="rounded-lg border border-app-border px-4 py-2 text-sm font-medium text-app-text transition-colors hover:border-app-accent hover:text-app-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-app-accent/50"
          >
            Go to Dashboard
          </a>
        </div>
      </div>
    );
  }
}
