import { describe, expect, it, afterEach } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/preact";
import { InfoTip } from "./InfoTip";

const HELP = "This explains the field in long, on-demand prose.";

describe("InfoTip", () => {
  afterEach(() => cleanup());

  it("renders a button trigger that mirrors the help text on its title attribute", () => {
    render(<InfoTip text={HELP} data-testid="tip" />);
    const trigger = screen.getByTestId("tip");
    expect(trigger.tagName).toBe("BUTTON");
    // The title-attribute mirror is load-bearing — moved-inline-description
    // callers (e.g. Catalog cards) assert on it.
    expect(trigger.getAttribute("title")).toBe(HELP);
    // Closed by default: no popover, aria-expanded=false.
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByRole("tooltip")).toBeNull();
  });

  it("forwards a custom aria-label to the trigger", () => {
    render(<InfoTip text={HELP} label="Field help" data-testid="tip" />);
    expect(screen.getByTestId("tip").getAttribute("aria-label")).toBe("Field help");
  });

  it("opens the popover on click (closed → open)", () => {
    render(<InfoTip text={HELP} data-testid="tip" />);
    const trigger = screen.getByTestId("tip");
    fireEvent.click(trigger);
    const pop = screen.getByRole("tooltip");
    expect(pop.textContent).toBe(HELP);
    expect(trigger.getAttribute("aria-expanded")).toBe("true");
    // The popover is wired to the trigger for assistive tech.
    expect(trigger.getAttribute("aria-describedby")).toBe(pop.id);
  });

  it("closes the popover on a second click (open → close) — the explicit ask", () => {
    render(<InfoTip text={HELP} data-testid="tip" />);
    const trigger = screen.getByTestId("tip");
    fireEvent.click(trigger); // open
    expect(screen.queryByRole("tooltip")).not.toBeNull();
    fireEvent.click(trigger); // close
    expect(screen.queryByRole("tooltip")).toBeNull();
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
  });

  it("stays pinned open across a mouseleave once opened by click", () => {
    render(<InfoTip text={HELP} data-testid="tip" />);
    const trigger = screen.getByTestId("tip");
    const wrap = trigger.parentElement as HTMLElement;
    fireEvent.click(trigger); // pin open
    expect(screen.queryByRole("tooltip")).not.toBeNull();
    // Pointer leaving must NOT dismiss a click-pinned popover.
    fireEvent.mouseLeave(wrap);
    expect(screen.queryByRole("tooltip")).not.toBeNull();
    expect(trigger.getAttribute("aria-expanded")).toBe("true");
  });

  it("closes a pinned popover on Escape", () => {
    render(<InfoTip text={HELP} data-testid="tip" />);
    const trigger = screen.getByTestId("tip");
    fireEvent.click(trigger); // pin open
    expect(screen.queryByRole("tooltip")).not.toBeNull();
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByRole("tooltip")).toBeNull();
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
  });

  it("closes a pinned popover on a click outside", () => {
    render(
      <div>
        <InfoTip text={HELP} data-testid="tip" />
        <button data-testid="outside">elsewhere</button>
      </div>,
    );
    const trigger = screen.getByTestId("tip");
    fireEvent.click(trigger); // pin open
    expect(screen.queryByRole("tooltip")).not.toBeNull();
    // Click-outside is wired on mousedown (so it beats a re-open click).
    fireEvent.mouseDown(screen.getByTestId("outside"));
    expect(screen.queryByRole("tooltip")).toBeNull();
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
  });

  it("opens a transient hover preview that closes on mouseleave (no pin)", () => {
    render(<InfoTip text={HELP} data-testid="tip" />);
    const trigger = screen.getByTestId("tip");
    const wrap = trigger.parentElement as HTMLElement;
    fireEvent.mouseEnter(wrap); // hover preview
    expect(screen.queryByRole("tooltip")).not.toBeNull();
    fireEvent.mouseLeave(wrap); // preview ends
    expect(screen.queryByRole("tooltip")).toBeNull();
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
  });

  it("opens a focus preview that closes on blur", () => {
    render(<InfoTip text={HELP} data-testid="tip" />);
    const trigger = screen.getByTestId("tip");
    fireEvent.focus(trigger);
    expect(screen.queryByRole("tooltip")).not.toBeNull();
    fireEvent.blur(trigger);
    expect(screen.queryByRole("tooltip")).toBeNull();
  });
});
