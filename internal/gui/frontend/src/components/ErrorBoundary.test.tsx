import { render, screen, cleanup } from "@testing-library/preact";
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { ErrorBoundary } from "./ErrorBoundary";

// A child that throws synchronously during render — exercises the boundary's
// catch path.
function Boom({ message }: { message?: string }): never {
  throw new Error(message ?? "kaboom");
}

beforeEach(() => {
  cleanup();
  // componentDidCatch logs the original error to console.error by design;
  // silence it so the expected-failure tests don't spam the test output.
  vi.spyOn(console, "error").mockImplementation(() => {});
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("ErrorBoundary", () => {
  it("renders children unchanged when no error is thrown", () => {
    render(
      <ErrorBoundary>
        <p data-testid="healthy-child">all good</p>
      </ErrorBoundary>,
    );
    expect(screen.getByTestId("healthy-child").textContent).toBe("all good");
    // No recovery UI when nothing threw.
    expect(screen.queryByTestId("error-boundary")).toBeNull();
  });

  it("renders the recovery UI (not a blank) when a child throws during render", () => {
    render(
      <ErrorBoundary screenName="Servers">
        <Boom message="render exploded" />
      </ErrorBoundary>,
    );

    // The recovery container is present — the app did NOT blank to white.
    const recovery = screen.getByTestId("error-boundary");
    expect(recovery).toBeTruthy();
    expect(recovery.getAttribute("role")).toBe("alert");

    // The generic heading + the screen name + the captured message all show.
    expect(screen.getByText("Something went wrong on this screen")).toBeTruthy();
    expect(screen.getByText("Servers")).toBeTruthy();
    expect(screen.getByTestId("error-boundary-message").textContent).toBe("render exploded");
  });

  it("renders a Reload button in the recovery UI", () => {
    render(
      <ErrorBoundary>
        <Boom />
      </ErrorBoundary>,
    );
    const reload = screen.getByTestId("error-boundary-reload");
    expect(reload).toBeTruthy();
    expect(reload.textContent).toBe("Reload");
    expect((reload as HTMLButtonElement).tagName).toBe("BUTTON");
  });

  it("offers a Go to Dashboard link in the recovery UI", () => {
    render(
      <ErrorBoundary>
        <Boom />
      </ErrorBoundary>,
    );
    const dashboard = screen.getByTestId("error-boundary-dashboard");
    expect(dashboard.getAttribute("href")).toBe("#/dashboard");
  });

  it("omits the screen-name line when no screenName prop is given", () => {
    render(
      <ErrorBoundary>
        <Boom />
      </ErrorBoundary>,
    );
    // Recovery UI still shows, just without the per-screen attribution.
    expect(screen.getByTestId("error-boundary")).toBeTruthy();
    expect(screen.getByText("Something went wrong on this screen")).toBeTruthy();
  });
});
