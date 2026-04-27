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

  it("ArrowDown wraps from last selectable to first selectable item", async () => {
    // Task 5: with the unified item model the selectable cycle is
    // [key "a", key "b", create-generic]. Pressing ArrowDown four times
    // (open+highlight first, advance to "b", advance to create-generic,
    // wrap back to "a") returns the highlight to "a".
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
    await user.keyboard("{ArrowDown}{ArrowDown}{ArrowDown}{ArrowDown}");
    const activeId = combo.getAttribute("aria-activedescendant");
    const activeOpt = activeId ? document.getElementById(activeId) : null;
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

function snapshotVaultBad(state: "missing" | "decrypt_failed" | "corrupt"): SecretsSnapshot {
  return {
    status: "ok",
    data: { vault_state: state, secrets: [], manifest_errors: [] },
    error: null,
    fetchedAt: Date.now(),
    refresh: vi.fn(async () => {}),
  };
}

describe("SecretPicker — case 7: missing ref shows red marker + dropdown surface", () => {
  it("with value='secret:never_added' and vault_state=ok, key absent: input has 'broken' class and aria-describedby points to a status node", async () => {
    render(
      <SecretPicker
        value="secret:never_added"
        onChange={vi.fn()}
        envKey="API_KEY"
        snapshot={snapshotOk([{ name: "wolfram_app_id" }])}
        onRequestCreate={vi.fn()}
      />
    );
    const combo = screen.getByRole("combobox");
    expect(combo.classList.contains("broken")).toBe(true);
    const describedById = combo.getAttribute("aria-describedby");
    expect(describedById).toBeTruthy();
    const statusEl = document.getElementById(describedById!);
    expect(statusEl?.textContent).toContain("never_added");
    expect(statusEl?.textContent?.toLowerCase()).toContain("not found");
  });

  it("dropdown opens with 'Currently referenced' section showing missing key + '+ Create' contextual entry enabled", async () => {
    const user = userEvent.setup();
    const onRequestCreate = vi.fn();
    render(
      <SecretPicker
        value="secret:never_added"
        onChange={vi.fn()}
        envKey="API_KEY"
        snapshot={snapshotOk([{ name: "wolfram_app_id" }])}
        onRequestCreate={onRequestCreate}
      />
    );
    await user.click(screen.getByRole("button", { name: /Pick secret/i }));
    const listbox = screen.getByRole("listbox");
    expect(listbox.textContent).toContain("Currently referenced");
    expect(listbox.textContent).toContain("never_added");
    expect(listbox.textContent).toContain("missing");
    const createEntry = screen.getByText(/Create 'never_added' in vault/i);
    expect(createEntry.closest("li")?.getAttribute("aria-disabled")).not.toBe("true");
    await user.click(createEntry);
    expect(onRequestCreate).toHaveBeenCalledWith("never_added");
  });
});

describe("SecretPicker — case 8: vault_state=decrypt_failed + secret ref → unverified marker + [CR] + [CN-disabled]", () => {
  it("input has 'unverified' class; dropdown shows [CR] (yellow unverified) + disabled '+ Create'", async () => {
    const user = userEvent.setup();
    const onRequestCreate = vi.fn();
    render(
      <SecretPicker
        value="secret:foo"
        onChange={vi.fn()}
        envKey="API_KEY"
        snapshot={snapshotVaultBad("decrypt_failed")}
        onRequestCreate={onRequestCreate}
      />
    );
    const combo = screen.getByRole("combobox");
    expect(combo.classList.contains("unverified")).toBe(true);
    expect(combo.classList.contains("broken")).toBe(false);

    await user.click(screen.getByRole("button", { name: /Pick secret/i }));
    const listbox = screen.getByRole("listbox");
    expect(listbox.textContent).toContain("Currently referenced");
    expect(listbox.textContent).toContain("foo");
    expect(listbox.textContent).toContain("unverified");
    const createEntry = listbox.querySelector('[data-action="create-disabled"]');
    expect(createEntry).toBeTruthy();
    expect(createEntry?.getAttribute("aria-disabled")).toBe("true");
    await user.click(createEntry as Element);
    expect(onRequestCreate).not.toHaveBeenCalled();
  });
});

describe("SecretPicker — case 8b: vault_state=decrypt_failed + literal value → [CN-disabled] only, no [CR]", () => {
  it("input has no broken/unverified class (literal); dropdown body is just [CN-disabled] — no Currently-referenced section", async () => {
    const user = userEvent.setup();
    render(
      <SecretPicker
        value="some literal value"
        onChange={vi.fn()}
        envKey="DEBUG"
        snapshot={snapshotVaultBad("decrypt_failed")}
        onRequestCreate={vi.fn()}
      />
    );
    const combo = screen.getByRole("combobox");
    expect(combo.classList.contains("broken")).toBe(false);
    expect(combo.classList.contains("unverified")).toBe(false);

    await user.click(screen.getByRole("button", { name: /Pick secret/i }));
    const listbox = screen.getByRole("listbox");
    expect(listbox.textContent).not.toContain("Currently referenced");
    const createEntry = listbox.querySelector('[data-action="create-disabled"]');
    expect(createEntry).toBeTruthy();
  });
});

describe("SecretPicker — case 10: snapshot.status=error → Retry-only dropdown body", () => {
  it("with status=error and fetchedAt=null, dropdown shows 'Retry loading vault' button only", async () => {
    const user = userEvent.setup();
    const refresh = vi.fn(async () => {});
    const snapshot: SecretsSnapshot = {
      status: "error",
      data: null,
      error: new Error("network"),
      fetchedAt: null,
      refresh,
    };
    render(
      <SecretPicker
        value="secret:foo"
        onChange={vi.fn()}
        envKey="API_KEY"
        snapshot={snapshot}
        onRequestCreate={vi.fn()}
      />
    );
    await user.click(screen.getByRole("button", { name: /Pick secret/i }));
    const listbox = screen.getByRole("listbox");
    expect(listbox.querySelector("[data-action='retry-load-vault']")).toBeTruthy();
    expect(listbox.querySelector('[data-action="create-new-secret"]')).toBeNull();
    expect(listbox.querySelector('[data-action="create-contextual"]')).toBeNull();
    const liveRegion = document.querySelector('[role="status"]');
    expect(liveRegion?.textContent?.toLowerCase()).toContain("could not load");
  });

  it("with status=error and fetchedAt=<timestamp>, error message mentions 'last loaded'", async () => {
    const user = userEvent.setup();
    const t = Date.now() - 60_000;
    const snapshot: SecretsSnapshot = {
      status: "error",
      data: null,
      error: new Error("network"),
      fetchedAt: t,
      refresh: vi.fn(async () => {}),
    };
    render(
      <SecretPicker
        value="secret:foo"
        onChange={vi.fn()}
        envKey="API_KEY"
        snapshot={snapshot}
        onRequestCreate={vi.fn()}
      />
    );
    await user.click(screen.getByRole("button", { name: /Pick secret/i }));
    const liveRegion = document.querySelector('[role="status"]');
    expect(liveRegion?.textContent?.toLowerCase()).toContain("last loaded");
  });
});

describe("SecretPicker — case 11: missing reserved name → [CRE-reserved-disabled]", () => {
  it("with value='secret:init' and key absent in vault, '+ Create init in vault' renders as disabled with reserved-name tooltip", async () => {
    const user = userEvent.setup();
    const onRequestCreate = vi.fn();
    render(
      <SecretPicker
        value="secret:init"
        onChange={vi.fn()}
        envKey="X"
        snapshot={snapshotOk()}
        onRequestCreate={onRequestCreate}
      />
    );
    await user.click(screen.getByRole("button", { name: /Pick secret/i }));
    const createEntry = document.querySelector('[data-action="create-contextual-disabled"]');
    expect(createEntry).toBeTruthy();
    expect(createEntry?.getAttribute("aria-disabled")).toBe("true");
    expect(createEntry?.getAttribute("title")).toMatch(/reserved name/i);
    await user.click(createEntry as Element);
    expect(onRequestCreate).not.toHaveBeenCalled();
  });
});

describe("SecretPicker — Codex PR-r1 P1: contextual create gated on valid secret name", () => {
  it("with value='secret:foo-bar' (hyphen — invalid backend NAME_RE), '+ Create' is rendered as DISABLED with valid-name tooltip", async () => {
    const user = userEvent.setup();
    const onRequestCreate = vi.fn();
    render(
      <SecretPicker
        value="secret:foo-bar"
        onChange={vi.fn()}
        envKey="X"
        snapshot={snapshotOk()}
        onRequestCreate={onRequestCreate}
      />
    );
    await user.click(screen.getByRole("button", { name: /Pick secret/i }));
    const createEntry = document.querySelector('[data-action="create-contextual-disabled"]');
    expect(createEntry).toBeTruthy();
    expect(createEntry?.getAttribute("aria-disabled")).toBe("true");
    expect(createEntry?.getAttribute("title")?.toLowerCase()).toMatch(/not a valid secret name|letters, digits/);
    await user.click(createEntry as Element);
    expect(onRequestCreate).not.toHaveBeenCalled();
  });

  it("with value='secret:_leading_underscore' (starts with underscore — invalid), '+ Create' is disabled", async () => {
    const user = userEvent.setup();
    render(
      <SecretPicker
        value="secret:_leading_underscore"
        onChange={vi.fn()}
        envKey="X"
        snapshot={snapshotOk()}
        onRequestCreate={vi.fn()}
      />
    );
    await user.click(screen.getByRole("button", { name: /Pick secret/i }));
    expect(document.querySelector('[data-action="create-contextual-disabled"]')).toBeTruthy();
  });
});

describe("SecretPicker — Codex PR-r1 P2: only state='present' keys appear in Available", () => {
  it("filters out state='referenced_missing' rows from Available secrets", async () => {
    const user = userEvent.setup();
    const snap: SecretsSnapshot = {
      status: "ok",
      data: {
        vault_state: "ok",
        secrets: [
          { name: "real_key", state: "present", used_by: [] },
          { name: "ghost_ref", state: "referenced_missing", used_by: [] },
        ],
        manifest_errors: [],
      },
      error: null,
      fetchedAt: Date.now(),
      refresh: vi.fn(async () => {}),
    };
    render(<SecretPicker value="" onChange={vi.fn()} envKey="K" snapshot={snap} onRequestCreate={vi.fn()} />);
    await user.click(screen.getByRole("button", { name: /Pick secret/i }));
    const listbox = screen.getByRole("listbox");
    expect(listbox.textContent).toContain("real_key");
    expect(listbox.textContent).not.toContain("ghost_ref");
  });

  it("filters out state='referenced_unverified' rows from Available secrets", async () => {
    const user = userEvent.setup();
    const snap: SecretsSnapshot = {
      status: "ok",
      data: {
        vault_state: "ok",
        secrets: [
          { name: "real_key", state: "present", used_by: [] },
          { name: "unverified_ref", state: "referenced_unverified", used_by: [] },
        ],
        manifest_errors: [],
      },
      error: null,
      fetchedAt: Date.now(),
      refresh: vi.fn(async () => {}),
    };
    render(<SecretPicker value="" onChange={vi.fn()} envKey="K" snapshot={snap} onRequestCreate={vi.fn()} />);
    await user.click(screen.getByRole("button", { name: /Pick secret/i }));
    const listbox = screen.getByRole("listbox");
    expect(listbox.textContent).toContain("real_key");
    expect(listbox.textContent).not.toContain("unverified_ref");
  });
});

describe("SecretPicker — D8 dropdown-open stale-refresh", () => {
  it("calls snapshot.refresh() when dropdown opens via 🔑 click and fetchedAt is older than 30s", async () => {
    const user = userEvent.setup();
    const refresh = vi.fn(async () => {});
    const snap: SecretsSnapshot = {
      status: "ok",
      data: { vault_state: "ok", secrets: [], manifest_errors: [] },
      error: null,
      fetchedAt: Date.now() - 31_000,
      refresh,
    };
    render(<SecretPicker value="" onChange={vi.fn()} envKey="K" snapshot={snap} onRequestCreate={vi.fn()} />);
    await user.click(screen.getByRole("button", { name: /Pick secret/i }));
    expect(refresh).toHaveBeenCalledTimes(1);
  });

  it("does NOT call refresh() when fetchedAt is fresh (<30s)", async () => {
    const user = userEvent.setup();
    const refresh = vi.fn(async () => {});
    const snap: SecretsSnapshot = {
      status: "ok",
      data: { vault_state: "ok", secrets: [], manifest_errors: [] },
      error: null,
      fetchedAt: Date.now() - 5_000,
      refresh,
    };
    render(<SecretPicker value="" onChange={vi.fn()} envKey="K" snapshot={snap} onRequestCreate={vi.fn()} />);
    await user.click(screen.getByRole("button", { name: /Pick secret/i }));
    expect(refresh).not.toHaveBeenCalled();
  });

  it("calls refresh() when dropdown auto-opens via 'secret:' typing AND data is stale", async () => {
    const user = userEvent.setup();
    const refresh = vi.fn(async () => {});
    const snap: SecretsSnapshot = {
      status: "ok",
      data: { vault_state: "ok", secrets: [{ name: "foo", state: "present", used_by: [] }], manifest_errors: [] },
      error: null,
      fetchedAt: Date.now() - 60_000,
      refresh,
    };
    function Harness() {
      const [v, setV] = useState("");
      return <SecretPicker value={v} onChange={setV} envKey="K" snapshot={snap} onRequestCreate={vi.fn()} />;
    }
    render(<Harness />);
    await user.click(screen.getByRole("combobox"));
    await user.keyboard("secret:");
    expect(refresh).toHaveBeenCalledTimes(1);
  });
});

describe("SecretPicker — unified item model + keyboard nav for non-key rows", () => {
  it("typing 'secret:' then narrowing keys while open resets highlight to first selectable key (Codex plan-R2 P1-3)", async () => {
    const user = userEvent.setup();
    const snap = snapshotOk([{ name: "alpha" }, { name: "beta" }, { name: "gamma" }]);
    function Harness() {
      const [v, setV] = useState("");
      return <SecretPicker value={v} onChange={setV} envKey="K" snapshot={snap} onRequestCreate={vi.fn()} />;
    }
    render(<Harness />);
    const combo = screen.getByRole("combobox");
    combo.focus();
    await user.keyboard("{ArrowDown}{ArrowDown}");
    expect(combo.getAttribute("aria-activedescendant")?.endsWith("-2")).toBe(true);

    await user.keyboard("secret:al");
    expect(combo.getAttribute("aria-activedescendant")?.endsWith("-1")).toBe(true);
  });

  it("ArrowDown can highlight '+ Create new secret…' row and Enter selects it", async () => {
    const user = userEvent.setup();
    const onRequestCreate = vi.fn();
    const snap = snapshotOk([{ name: "alpha" }]);
    render(<SecretPicker value="" onChange={vi.fn()} envKey="K" snapshot={snap} onRequestCreate={onRequestCreate} />);
    const combo = screen.getByRole("combobox");
    combo.focus();
    await user.keyboard("{ArrowDown}{ArrowDown}{Enter}");
    expect(onRequestCreate).toHaveBeenCalledWith(null);
  });

  it("Enter on a disabled '[CRE-reserved-disabled]' is a no-op", async () => {
    const user = userEvent.setup();
    const onRequestCreate = vi.fn();
    const snap = snapshotOk();
    render(<SecretPicker value="secret:init" onChange={vi.fn()} envKey="K" snapshot={snap} onRequestCreate={onRequestCreate} />);
    await user.click(screen.getByRole("button", { name: /Pick secret/i }));
    const disabled = document.querySelector('[data-action="create-contextual-disabled"]');
    expect(disabled).toBeTruthy();
    expect(disabled?.getAttribute("aria-disabled")).toBe("true");
    await user.click(disabled as Element);
    expect(onRequestCreate).not.toHaveBeenCalled();
  });

  it("Retry row in error state is keyboard-reachable (ArrowDown → Enter triggers refresh)", async () => {
    const user = userEvent.setup();
    const refresh = vi.fn(async () => {});
    const snap: SecretsSnapshot = { status: "error", data: null, error: new Error("x"), fetchedAt: null, refresh };
    render(<SecretPicker value="secret:foo" onChange={vi.fn()} envKey="K" snapshot={snap} onRequestCreate={vi.fn()} />);
    const combo = screen.getByRole("combobox");
    combo.focus();
    await user.keyboard("{ArrowDown}{Enter}");
    expect(refresh).toHaveBeenCalled();
  });

  it("Closed-input Enter is a no-op AND does NOT submit a parent form (Codex plan-R2 P1-2)", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn((e: Event) => e.preventDefault());
    const snap = snapshotOk([{ name: "alpha" }]);
    render(
      <form onSubmit={onSubmit}>
        <SecretPicker value="" onChange={vi.fn()} envKey="K" snapshot={snap} onRequestCreate={vi.fn()} />
        <button type="submit">Submit</button>
      </form>
    );
    const combo = screen.getByRole("combobox");
    combo.focus();
    await user.keyboard("{Enter}");
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it("Open-input Enter with no selectable highlight is a no-op (Codex plan-R2 P1-2)", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn((e: Event) => e.preventDefault());
    const onChange = vi.fn();
    const snap: SecretsSnapshot = { status: "loading", data: null, error: null, fetchedAt: null, refresh: vi.fn(async () => {}) };
    render(
      <form onSubmit={onSubmit}>
        <SecretPicker value="secret:foo" onChange={onChange} envKey="K" snapshot={snap} onRequestCreate={vi.fn()} />
        <button type="submit">Submit</button>
      </form>
    );
    const combo = screen.getByRole("combobox");
    combo.focus();
    await user.keyboard("{ArrowDown}{Enter}");
    expect(onSubmit).not.toHaveBeenCalled();
    expect(onChange).not.toHaveBeenCalled();
  });
});

describe("SecretPicker — legacy reserved init key in vault (memo R4)", () => {
  it("renders existing 'init' vault key as a SELECTABLE option with '(legacy reserved name)' chip + tooltip", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    const snap = snapshotOk([{ name: "init" }, { name: "openai_api_key" }]);
    render(<SecretPicker value="" onChange={onChange} envKey="K" snapshot={snap} onRequestCreate={vi.fn()} />);
    await user.click(screen.getByRole("button", { name: /Pick secret/i }));
    const initRow = screen.getByText("init").closest("li");
    expect(initRow).toBeTruthy();
    expect(initRow!.textContent).toContain("legacy reserved name");
    expect(initRow!.getAttribute("title")).toMatch(/reserved/i);
    await user.click(initRow as Element);
    expect(onChange).toHaveBeenCalledWith("secret:init");
  });

  it("does NOT filter out legacy 'init' from the Available list", async () => {
    const user = userEvent.setup();
    const snap = snapshotOk([{ name: "init" }]);
    render(<SecretPicker value="" onChange={vi.fn()} envKey="K" snapshot={snap} onRequestCreate={vi.fn()} />);
    await user.click(screen.getByRole("button", { name: /Pick secret/i }));
    expect(screen.queryByText("init")).toBeTruthy();
  });
});

describe("SecretPicker — [AV] 'Available secrets' header shown for literal/present", () => {
  it("renders 'Available secrets' section header for literal value when vault has keys", async () => {
    const user = userEvent.setup();
    const snap = snapshotOk([{ name: "alpha" }]);
    render(<SecretPicker value="literal-not-secret" onChange={vi.fn()} envKey="K" snapshot={snap} onRequestCreate={vi.fn()} />);
    await user.click(screen.getByRole("button", { name: /Pick secret/i }));
    expect(screen.getByText(/Available secrets/i)).toBeTruthy();
  });

  it("renders 'Available secrets' section header for present-state value", async () => {
    const user = userEvent.setup();
    const snap = snapshotOk([{ name: "foo" }, { name: "bar" }]);
    render(<SecretPicker value="secret:foo" onChange={vi.fn()} envKey="K" snapshot={snap} onRequestCreate={vi.fn()} />);
    await user.click(screen.getByRole("button", { name: /Pick secret/i }));
    expect(screen.getByText(/Available secrets/i)).toBeTruthy();
  });

  it("does NOT render 'Available secrets' header when vault is empty", async () => {
    const user = userEvent.setup();
    const snap = snapshotOk([]);
    render(<SecretPicker value="literal" onChange={vi.fn()} envKey="K" snapshot={snap} onRequestCreate={vi.fn()} />);
    await user.click(screen.getByRole("button", { name: /Pick secret/i }));
    expect(screen.queryByText(/Available secrets/i)).toBeNull();
  });
});
