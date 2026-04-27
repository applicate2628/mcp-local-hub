import { describe, expect, it } from "vitest";
import { render, screen, cleanup } from "@testing-library/preact";
import { BrokenRefsSummary } from "./BrokenRefsSummary";

describe("BrokenRefsSummary", () => {
  it("renders nothing when vaultState='ok' and brokenRefs.length <= 1", () => {
    cleanup();
    const { container } = render(<BrokenRefsSummary vaultState="ok" brokenRefs={[]} />);
    expect(container.textContent).toBe("");
  });

  it("renders nothing when vaultState='ok' and brokenRefs has exactly 1 entry", () => {
    cleanup();
    const { container } = render(<BrokenRefsSummary vaultState="ok" brokenRefs={["only_one"]} />);
    expect(container.textContent).toBe("");
  });

  it("renders summary with comma-list when vaultState='ok' and brokenRefs.length > 1", () => {
    cleanup();
    render(<BrokenRefsSummary vaultState="ok" brokenRefs={["a", "b", "c"]} />);
    const text = screen.getByRole("status").textContent ?? "";
    expect(text).toContain("3 secrets referenced but not in vault");
    expect(text).toContain("a");
    expect(text).toContain("b");
    expect(text).toContain("c");
    expect(text.toLowerCase()).toContain("daemons will fail to start");
  });

  it("renders 'Vault not readable' message for vault_state='decrypt_failed'", () => {
    cleanup();
    render(<BrokenRefsSummary vaultState="decrypt_failed" brokenRefs={[]} />);
    expect(screen.getByRole("status").textContent).toContain("Vault not readable (decrypt_failed)");
    expect(screen.getByRole("status").textContent).toContain("Fix vault on Secrets screen");
  });

  it("renders 'Vault not initialized' message for vault_state='missing'", () => {
    cleanup();
    render(<BrokenRefsSummary vaultState="missing" brokenRefs={[]} />);
    expect(screen.getByRole("status").textContent).toContain("Vault not initialized");
  });

  it("renders 'Vault file corrupted' message for vault_state='corrupt'", () => {
    cleanup();
    render(<BrokenRefsSummary vaultState="corrupt" brokenRefs={[]} />);
    expect(screen.getByRole("status").textContent).toContain("Vault file corrupted");
  });
});
