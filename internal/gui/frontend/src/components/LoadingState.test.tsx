import { cleanup, render, screen } from "@testing-library/preact";
import { afterEach, describe, expect, it } from "vitest";
import { LoadingState } from "./LoadingState";

describe("LoadingState", () => {
  afterEach(() => cleanup());

  it("renders an accessible live status that announces a hidden Loading label", () => {
    render(<LoadingState />);

    const status = screen.getByRole("status");
    // role=status + aria-live=polite announces the label; no aria-busy (which
    // would defer the announcement on a region that only unmounts).
    expect(status.getAttribute("aria-busy")).toBeNull();
    expect(status.getAttribute("aria-live")).toBe("polite");
    expect(status.querySelector(".visually-hidden")?.textContent).toBe("Loading");
    expect(status.querySelector(".loading-state-skeleton")).not.toBeNull();
  });

  it("allows screens to provide a more specific screen-reader label", () => {
    render(<LoadingState label="Loading servers" />);

    expect(screen.getByRole("status").querySelector(".visually-hidden")?.textContent).toBe(
      "Loading servers",
    );
  });
});
