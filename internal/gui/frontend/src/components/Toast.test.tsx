import { describe, expect, it, afterEach, beforeEach } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/preact";
import { ToastContainer, ToastItem } from "./Toast";
import { pushToast, clearAllToasts, type Toast } from "../lib/toast-store";

describe("ToastItem (Flowbite Toast markup)", () => {
  afterEach(() => cleanup());

  function make(variant: Toast["variant"], message: string): Toast {
    return { id: 1, variant, message, timeoutMs: 0 };
  }

  it("renders the Flowbite toast shell classes + message", () => {
    render(<ToastItem toast={make("success", "Saved.")} />);
    const toast = screen.getByTestId("toast");
    // Flowbite Toast vocabulary present on the shell.
    expect(toast.className).toContain("max-w-xs");
    expect(toast.className).toContain("rounded-lg");
    expect(toast.className).toContain("shadow-sm");
    expect(toast.getAttribute("role")).toBe("alert");
    expect(screen.getByTestId("toast-message").textContent).toBe("Saved.");
  });

  it("applies the success variant icon-chip color", () => {
    render(<ToastItem toast={make("success", "ok")} />);
    const toast = screen.getByTestId("toast");
    expect(toast.getAttribute("data-toast-variant")).toBe("success");
    // The green chip color from Flowbite's success toast.
    expect(toast.querySelector(".bg-green-100")).not.toBeNull();
  });

  it("applies the danger variant icon-chip color", () => {
    render(<ToastItem toast={make("danger", "boom")} />);
    expect(screen.getByTestId("toast").querySelector(".bg-red-100")).not.toBeNull();
  });

  it("renders the Flowbite dismiss button with data-dismiss-target parity", () => {
    render(<ToastItem toast={make("info", "hi")} />);
    const close = screen.getByTestId("toast-dismiss");
    expect(close.getAttribute("data-dismiss-target")).toBe("#toast-1");
    expect(close.getAttribute("aria-label")).toBe("Close");
  });
});

describe("ToastContainer (store-driven live stack)", () => {
  beforeEach(() => clearAllToasts());
  afterEach(() => {
    cleanup();
    clearAllToasts();
  });

  it("renders nothing when there are no toasts", () => {
    render(<ToastContainer />);
    expect(screen.queryByTestId("toast-container")).toBeNull();
  });

  it("renders a pushed toast and dismisses it on close-button click", async () => {
    render(<ToastContainer />);
    pushToast("success", "Applied 3 changes.", { timeoutMs: 0 });
    expect(await screen.findByTestId("toast")).not.toBeNull();
    expect(screen.getByTestId("toast-message").textContent).toBe("Applied 3 changes.");
    fireEvent.click(screen.getByTestId("toast-dismiss"));
    expect(screen.queryByTestId("toast")).toBeNull();
  });

  it("stacks multiple toasts in the fixed container", async () => {
    render(<ToastContainer />);
    pushToast("success", "a", { timeoutMs: 0 });
    pushToast("danger", "b", { timeoutMs: 0 });
    await screen.findByTestId("toast-container");
    expect(screen.getAllByTestId("toast")).toHaveLength(2);
  });
});
