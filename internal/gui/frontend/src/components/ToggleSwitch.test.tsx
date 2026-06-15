import { describe, expect, it, afterEach, vi } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/preact";
import { ToggleSwitch } from "./ToggleSwitch";

describe("ToggleSwitch", () => {
  afterEach(() => cleanup());

  it("renders a real <input type='checkbox'> under the hood (selectors keep working)", () => {
    const { container } = render(<ToggleSwitch checked={false} />);
    const input = container.querySelector('input[type="checkbox"]');
    expect(input).not.toBeNull();
    // role=switch so assistive tech announces it as a toggle, matching the
    // visual; aria-checked mirrors the checked prop.
    expect(input!.getAttribute("role")).toBe("switch");
    expect(input!.getAttribute("aria-checked")).toBe("false");
  });

  it("reflects the checked prop on the input", () => {
    const { container } = render(<ToggleSwitch checked={true} />);
    const input = container.querySelector<HTMLInputElement>('input[type="checkbox"]')!;
    expect(input.checked).toBe(true);
    expect(input.getAttribute("aria-checked")).toBe("true");
  });

  it("reflects the disabled prop and dims the wrapper", () => {
    const { container } = render(<ToggleSwitch checked={false} disabled />);
    const input = container.querySelector<HTMLInputElement>('input[type="checkbox"]')!;
    expect(input.disabled).toBe(true);
    expect(container.querySelector(".toggle-switch--disabled")).not.toBeNull();
  });

  it("fires onChange with the native input event when clicked", () => {
    const onChange = vi.fn();
    const { container } = render(<ToggleSwitch checked={false} onChange={onChange} />);
    const input = container.querySelector<HTMLInputElement>('input[type="checkbox"]')!;
    // A direct click on the (visually hidden but clickable) input toggles it
    // and fires onChange — the exact path the Servers matrix tests exercise.
    fireEvent.click(input);
    expect(onChange).toHaveBeenCalledTimes(1);
    expect(input.checked).toBe(true);
  });

  it("does not fire onChange when disabled", () => {
    const onChange = vi.fn();
    const { container } = render(
      <ToggleSwitch checked={false} disabled onChange={onChange} />,
    );
    const input = container.querySelector<HTMLInputElement>('input[type="checkbox"]')!;
    fireEvent.click(input);
    expect(onChange).not.toHaveBeenCalled();
  });

  it("applies the pending variant class + marker when pending", () => {
    const { container } = render(<ToggleSwitch checked={false} pending />);
    const wrap = container.querySelector(".toggle-switch")!;
    expect(wrap.classList.contains("toggle-switch--pending")).toBe(true);
    expect(wrap.getAttribute("data-pending")).toBe("true");
  });

  it("omits the pending class + marker when not pending", () => {
    const { container } = render(<ToggleSwitch checked={false} />);
    const wrap = container.querySelector(".toggle-switch")!;
    expect(wrap.classList.contains("toggle-switch--pending")).toBe(false);
    expect(wrap.getAttribute("data-pending")).toBeNull();
  });

  it("passes through data-*, aria-*, and title onto the real input", () => {
    render(
      <ToggleSwitch
        checked={false}
        data-testid="my-toggle"
        aria-label="Enable thing"
        title="Click to enable"
      />,
    );
    const input = screen.getByTestId("my-toggle");
    expect(input.tagName).toBe("INPUT");
    expect(input.getAttribute("aria-label")).toBe("Enable thing");
    expect(input.getAttribute("title")).toBe("Click to enable");
  });
});
