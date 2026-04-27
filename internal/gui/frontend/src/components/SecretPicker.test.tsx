import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/preact";
import userEvent from "@testing-library/user-event";
import { useState } from "preact/hooks";
import { SecretPicker } from "./SecretPicker";
import type { SecretsSnapshot } from "../lib/use-secrets-snapshot";

beforeEach(() => { cleanup(); });
afterEach(() => { cleanup(); });

function snapshotOk(secrets: { name: string; state?: "present" }[] = []): SecretsSnapshot {
  return {
    status: "ok",
    data: {
      vault_state: "ok",
      secrets: secrets.map((s) => ({ name: s.name, state: s.state ?? "present", used_by: [] })),
      manifest_errors: [],
    },
    error: null,
    fetchedAt: Date.now(),
    refresh: vi.fn(async () => {}),
  };
}

describe("SecretPicker — case 1: render with ARIA combobox", () => {
  it("renders value <input> with role=combobox and the toggle button with aria-label", () => {
    const onChange = vi.fn();
    const onRequestCreate = vi.fn();
    render(
      <SecretPicker
        value=""
        onChange={onChange}
        envKey="API_KEY"
        snapshot={snapshotOk()}
        onRequestCreate={onRequestCreate}
      />
    );
    const combo = screen.getByRole("combobox");
    expect(combo.getAttribute("aria-expanded")).toBe("false");
    expect(combo.getAttribute("aria-controls")).toBeTruthy();
    const button = screen.getByRole("button", { name: /Pick secret/i });
    expect(button.getAttribute("aria-haspopup")).toBe("listbox");
  });
});

describe("SecretPicker — case 2: click toggle opens/closes dropdown", () => {
  it("clicking 🔑 button opens the dropdown; clicking it again closes it", async () => {
    const user = userEvent.setup();
    render(
      <SecretPicker
        value=""
        onChange={vi.fn()}
        envKey="API_KEY"
        snapshot={snapshotOk([{ name: "wolfram_app_id" }])}
        onRequestCreate={vi.fn()}
      />
    );
    const button = screen.getByRole("button", { name: /Pick secret/i });
    expect(screen.queryByRole("listbox")).toBeNull();

    await user.click(button);
    expect(screen.getByRole("listbox")).toBeTruthy();
    expect(screen.getByRole("combobox").getAttribute("aria-expanded")).toBe("true");

    await user.click(button);
    expect(screen.queryByRole("listbox")).toBeNull();
    expect(screen.getByRole("combobox").getAttribute("aria-expanded")).toBe("false");
  });

  it("clicking outside the wrapper closes the dropdown", async () => {
    const user = userEvent.setup();
    render(
      <div>
        <SecretPicker
          value=""
          onChange={vi.fn()}
          envKey="API_KEY"
          snapshot={snapshotOk([{ name: "wolfram_app_id" }])}
          onRequestCreate={vi.fn()}
        />
        <button data-testid="outside-button">Outside</button>
      </div>
    );
    await user.click(screen.getByRole("button", { name: /Pick secret/i }));
    expect(screen.getByRole("listbox")).toBeTruthy();

    await user.click(screen.getByTestId("outside-button"));
    expect(screen.queryByRole("listbox")).toBeNull();
  });
});

describe("SecretPicker — case 3: typing 'secret:' auto-opens + filter narrows", () => {
  it("typing 'secret:' into the input auto-opens the dropdown", async () => {
    const user = userEvent.setup();
    const snap = snapshotOk([{ name: "wolfram_app_id" }]);

    function TestHarness() {
      const [v, setV] = useState("");
      return (
        <SecretPicker
          value={v}
          onChange={setV}
          envKey="API_KEY"
          snapshot={snap}
          onRequestCreate={vi.fn()}
        />
      );
    }
    render(<TestHarness />);

    const combo = screen.getByRole("combobox");
    await user.click(combo);
    await user.keyboard("secret:");
    expect(screen.getByRole("listbox")).toBeTruthy();
  });

  it("further typing narrows the dropdown via case-insensitive substring after stripping `secret:`", async () => {
    const user = userEvent.setup();
    const snap = snapshotOk([
      { name: "wolfram_app_id" },
      { name: "openai_api_key" },
      { name: "unpaywall_email" },
    ]);

    function TestHarness() {
      const [v, setV] = useState("");
      return (
        <SecretPicker
          value={v}
          onChange={setV}
          envKey="API_KEY"
          snapshot={snap}
          onRequestCreate={vi.fn()}
        />
      );
    }
    render(<TestHarness />);

    const combo = screen.getByRole("combobox");
    await user.click(combo);
    await user.keyboard("secret:wolf");
    const options = screen.getAllByRole("option");
    const filteredKeys = options.map((o) => o.textContent || "").filter((t) => t.includes("wolfram") || t.includes("openai") || t.includes("unpaywall"));
    expect(filteredKeys.some((t) => t.includes("wolfram_app_id"))).toBe(true);
    expect(filteredKeys.some((t) => t.includes("openai_api_key"))).toBe(false);
    expect(filteredKeys.some((t) => t.includes("unpaywall_email"))).toBe(false);
  });
});

describe("SecretPicker — case 4: matchTier sort + 'matches KEY name' badge", () => {
  it("sorts the matching key to top with badge when env KEY normalizes to vault key", async () => {
    const user = userEvent.setup();
    render(
      <SecretPicker
        value=""
        onChange={vi.fn()}
        envKey="OPENAI_API_KEY"
        snapshot={snapshotOk([
          { name: "wolfram_app_id" },
          { name: "openai_api_key" },
          { name: "unpaywall_email" },
        ])}
        onRequestCreate={vi.fn()}
      />
    );
    await user.click(screen.getByRole("button", { name: /Pick secret/i }));
    const options = screen.getAllByRole("option");
    const dataOptions = options.filter((o) => !o.classList.contains("dropdown-create"));
    expect(dataOptions[0].textContent).toContain("openai_api_key");
    expect(dataOptions[0].textContent).toContain("matches KEY name");
    const otherBadges = options.slice(1).map((o) => o.textContent?.includes("matches KEY name"));
    expect(otherBadges.every((b) => !b)).toBe(true);
  });

  it("does not show 'matches KEY name' badge for tier-1 prefix matches", async () => {
    const user = userEvent.setup();
    render(
      <SecretPicker
        value=""
        onChange={vi.fn()}
        envKey="OPENAI_API_KEY"
        snapshot={snapshotOk([{ name: "openai" }])}
        onRequestCreate={vi.fn()}
      />
    );
    await user.click(screen.getByRole("button", { name: /Pick secret/i }));
    const options = screen.getAllByRole("option");
    const dataOption = options.find((o) => o.textContent?.includes("openai"));
    expect(dataOption?.textContent).not.toContain("matches KEY name");
  });
});

describe("SecretPicker — case 5: click selects and commits secret:<key>", () => {
  it("clicking a vault key option commits secret:<key> via onChange and closes dropdown", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(
      <SecretPicker
        value=""
        onChange={onChange}
        envKey="API_KEY"
        snapshot={snapshotOk([{ name: "wolfram_app_id" }])}
        onRequestCreate={vi.fn()}
      />
    );
    await user.click(screen.getByRole("button", { name: /Pick secret/i }));
    const option = screen.getAllByRole("option").find((o) => o.textContent?.includes("wolfram_app_id"))!;
    await user.click(option);
    expect(onChange).toHaveBeenCalledWith("secret:wolfram_app_id");
    expect(screen.queryByRole("listbox")).toBeNull();
  });
});

describe("SecretPicker — case 6: keyboard navigation (editable-combobox)", () => {
  it("ArrowDown opens dropdown when closed and highlights first item", async () => {
    const user = userEvent.setup();
    render(
      <SecretPicker
        value=""
        onChange={vi.fn()}
        envKey="API_KEY"
        snapshot={snapshotOk([{ name: "a" }, { name: "b" }])}
        onRequestCreate={vi.fn()}
      />
    );
    const combo = screen.getByRole("combobox");
    combo.focus();
    await user.keyboard("{ArrowDown}");
    expect(screen.getByRole("listbox")).toBeTruthy();
    const activeId = combo.getAttribute("aria-activedescendant");
    expect(activeId).toBeTruthy();
  });

  it("ArrowDown wraps from last to first item", async () => {
    const user = userEvent.setup();
    render(
      <SecretPicker
        value=""
        onChange={vi.fn()}
        envKey="API_KEY"
        snapshot={snapshotOk([{ name: "a" }, { name: "b" }])}
        onRequestCreate={vi.fn()}
      />
    );
    const combo = screen.getByRole("combobox");
    combo.focus();
    await user.keyboard("{ArrowDown}{ArrowDown}{ArrowDown}");
    const activeId = combo.getAttribute("aria-activedescendant");
    const options = screen.getAllByRole("option").filter((o) => !o.classList.contains("dropdown-create") && o.id);
    const activeOpt = options.find((o) => o.id === activeId);
    expect(activeOpt?.textContent).toContain("a");
  });

  it("Enter selects highlighted item and closes dropdown", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(
      <SecretPicker
        value=""
        onChange={onChange}
        envKey="API_KEY"
        snapshot={snapshotOk([{ name: "wolfram_app_id" }])}
        onRequestCreate={vi.fn()}
      />
    );
    const combo = screen.getByRole("combobox");
    combo.focus();
    await user.keyboard("{ArrowDown}{Enter}");
    expect(onChange).toHaveBeenCalledWith("secret:wolfram_app_id");
    expect(screen.queryByRole("listbox")).toBeNull();
  });

  it("Esc closes the dropdown without selecting", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(
      <SecretPicker
        value=""
        onChange={onChange}
        envKey="API_KEY"
        snapshot={snapshotOk([{ name: "wolfram_app_id" }])}
        onRequestCreate={vi.fn()}
      />
    );
    const combo = screen.getByRole("combobox");
    combo.focus();
    await user.keyboard("{ArrowDown}");
    expect(screen.getByRole("listbox")).toBeTruthy();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("listbox")).toBeNull();
    expect(onChange).not.toHaveBeenCalled();
  });

  it("Tab closes the dropdown but does not preventDefault (focus moves normally)", async () => {
    const user = userEvent.setup();
    render(
      <SecretPicker
        value=""
        onChange={vi.fn()}
        envKey="API_KEY"
        snapshot={snapshotOk([{ name: "wolfram_app_id" }])}
        onRequestCreate={vi.fn()}
      />
    );
    const combo = screen.getByRole("combobox");
    combo.focus();
    await user.keyboard("{ArrowDown}");
    expect(screen.getByRole("listbox")).toBeTruthy();
    await user.keyboard("{Tab}");
    expect(screen.queryByRole("listbox")).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: /Pick secret/i }));
  });
});

describe("SecretPicker — case 9: editing prefix only does not show missing", () => {
  // NOTE (Task 4 quality review D): this test is a pre-condition anchor for
  // Task 5. The current scaffold never applies broken/unverified classes
  // because classifyRefState is not yet wired — the assertion below passes
  // trivially. When Task 5 wires the classifier (memo §5.5 hasSecretKey
  // guard returns false for "secret:" alone → returns "literal" → no class),
  // this test guards the invariant that an "editing prefix" value with no
  // committed key does NOT trigger a missing/unverified marker. Strengthen
  // by adding additional assertions in Task 5 if needed (e.g., aria-describedby
  // is absent).
  it("with value='secret:' (just prefix), classifyRefState falls to literal — no broken indicators on input", () => {
    render(
      <SecretPicker
        value="secret:"
        onChange={vi.fn()}
        envKey="API_KEY"
        snapshot={snapshotOk([{ name: "wolfram_app_id" }])}
        onRequestCreate={vi.fn()}
      />
    );
    const combo = screen.getByRole("combobox");
    expect(combo.classList.contains("broken")).toBe(false);
    expect(combo.classList.contains("unverified")).toBe(false);
  });
});
