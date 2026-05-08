import { render, cleanup } from "@testing-library/preact";
import { describe, it, expect, afterEach } from "vitest";
import { CapabilitiesScreen } from "./Capabilities";

afterEach(cleanup);

describe("CapabilitiesScreen — Phase 1 placeholder", () => {
  it("renders the h1 'Capabilities' heading", () => {
    const { getByRole } = render(<CapabilitiesScreen />);
    const h1 = getByRole("heading", { level: 1 });
    expect(h1.textContent).toBe("Capabilities");
  });

  it("has the .capabilities-screen container class", () => {
    const { container } = render(<CapabilitiesScreen />);
    expect(container.querySelector(".capabilities-screen")).not.toBeNull();
  });
});
