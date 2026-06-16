import { describe, expect, it, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/preact";
import { FlowbiteExample } from "./FlowbiteExample";

// Proof-of-integration test: confirms the Flowbite-classed reference component
// mounts (which exercises the initFlowbite() useEffect) and renders the static
// Badge + Button + the interactive tooltip trigger markup. This is the
// regression guard that Flowbite stays importable + usable in this Preact app.
describe("FlowbiteExample (Flowbite proof-of-integration)", () => {
  afterEach(() => cleanup());

  it("mounts without throwing (initFlowbite() runs in the mount effect)", () => {
    render(<FlowbiteExample />);
    expect(screen.getByTestId("flowbite-example")).not.toBeNull();
  });

  it("renders the static Flowbite Badge with Flowbite utility classes", () => {
    render(<FlowbiteExample />);
    const badge = screen.getByTestId("flowbite-badge");
    expect(badge.textContent).toContain("Flowbite enabled");
    // Flowbite's badge class vocabulary (Tailwind utilities) is present.
    expect(badge.className).toContain("bg-blue-100");
    expect(badge.className).toContain("rounded-sm");
  });

  it("renders the static Flowbite Button", () => {
    render(<FlowbiteExample />);
    const btn = screen.getByTestId("flowbite-button");
    expect(btn.tagName).toBe("BUTTON");
    expect(btn.className).toContain("bg-blue-700");
  });

  it("renders the interactive tooltip trigger wired by its data attribute", () => {
    render(<FlowbiteExample />);
    const trigger = screen.getByTestId("flowbite-tooltip-trigger");
    // The data attribute is the contract initFlowbite() scans for.
    expect(trigger.getAttribute("data-tooltip-target")).toBe("flowbite-tooltip");
    // The tooltip body exists with the matching id + role.
    const body = document.getElementById("flowbite-tooltip");
    expect(body).not.toBeNull();
    expect(body?.getAttribute("role")).toBe("tooltip");
  });
});
