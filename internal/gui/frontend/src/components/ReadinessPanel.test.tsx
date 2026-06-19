import { describe, expect, it, beforeEach, afterEach, vi } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/preact";
import { ReadinessPanel } from "./ReadinessPanel";
import type { ReadinessReport } from "../api";

beforeEach(() => {
  cleanup();
});
afterEach(() => {
  cleanup();
});

function report(reqs: ReadinessReport["requirements"], ready = false): ReadinessReport {
  return { server: "demo", ready, requirements: reqs };
}

const noop = () => {};

describe("ReadinessPanel", () => {
  it("renders nothing when there is no report and it is not loading", () => {
    const { container } = render(
      <ReadinessPanel report={null} loading={false} error={null} inlineSecrets={{}} onInlineSecretChange={noop} />,
    );
    expect(container.innerHTML).toBe("");
  });

  it("shows a loading state before the first report arrives", () => {
    render(
      <ReadinessPanel report={null} loading={true} error={null} inlineSecrets={{}} onInlineSecretChange={noop} />,
    );
    expect(screen.getByTestId("readiness-panel").textContent).toContain("Checking install readiness");
  });

  it("renders the error string when one is set", () => {
    render(
      <ReadinessPanel report={null} loading={false} error={"vault unreadable"} inlineSecrets={{}} onInlineSecretChange={noop} />,
    );
    expect(screen.getByTestId("readiness-panel").textContent).toContain("vault unreadable");
  });

  it("shows a blocked badge with the blocker count and sorts blockers first", () => {
    const rep = report(
      [
        { name: "launcher: Node.js", ok: true },
        { name: "binary: gdb", ok: false, optional: false, fix: "Install gdb" },
      ],
      false,
    );
    render(
      <ReadinessPanel report={rep} loading={false} error={null} inlineSecrets={{}} onInlineSecretChange={noop} />,
    );
    expect(screen.getByTestId("readiness-badge").textContent).toContain("1 blocker");
    const rows = screen.getByTestId("readiness-panel").querySelectorAll(".readiness-row");
    expect(rows[0].textContent).toContain("binary: gdb"); // blocker sorts first
    expect(screen.getByText("Install gdb")).toBeTruthy();
  });

  it("shows a ready badge when the report is ready", () => {
    const rep = report([{ name: "launcher: Node.js", ok: true }], true);
    render(
      <ReadinessPanel report={rep} loading={false} error={null} inlineSecrets={{}} onInlineSecretChange={noop} />,
    );
    expect(screen.getByTestId("readiness-badge").textContent).toContain("Ready to install");
  });

  it("renders an inline password field for an unset optional secret and fires onInlineSecretChange", () => {
    const onChange = vi.fn();
    const rep = report(
      [{ name: "secret: OPENAI_API_KEY", ok: false, optional: true, reason: "not set" }],
      true,
    );
    render(
      <ReadinessPanel report={rep} loading={false} error={null} inlineSecrets={{}} onInlineSecretChange={onChange} />,
    );
    const input = screen.getByTestId("readiness-secret-input-OPENAI_API_KEY") as HTMLInputElement;
    expect(input.type).toBe("password");
    fireEvent.input(input, { target: { value: "sk-123" } });
    expect(onChange).toHaveBeenCalledWith("OPENAI_API_KEY", "sk-123");
  });

  it("does not render an inline field for a secret that is already set", () => {
    const rep = report([{ name: "secret: SET_KEY", ok: true, optional: true }], true);
    render(
      <ReadinessPanel report={rep} loading={false} error={null} inlineSecrets={{}} onInlineSecretChange={noop} />,
    );
    expect(screen.queryByTestId("readiness-secret-input-SET_KEY")).toBeNull();
  });

  it("does not render an inline field for a non-conforming vault key (shows the Fix instead)", () => {
    const rep = report(
      [{ name: "secret: bad-key", ok: false, optional: true, fix: "use the secret picker" }],
      true,
    );
    render(
      <ReadinessPanel report={rep} loading={false} error={null} inlineSecrets={{}} onInlineSecretChange={noop} />,
    );
    expect(screen.queryByTestId("readiness-secret-input-bad-key")).toBeNull();
    expect(screen.getByText("use the secret picker")).toBeTruthy();
  });
});
