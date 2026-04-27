import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, screen, cleanup, waitFor } from "@testing-library/preact";
import userEvent from "@testing-library/user-event";
import { AddServerScreen } from "./AddServer";

const mockFetch = vi.fn();
beforeEach(() => {
  cleanup();
  mockFetch.mockReset();
  globalThis.fetch = mockFetch as unknown as typeof fetch;
  if (!HTMLDialogElement.prototype.showModal) {
    HTMLDialogElement.prototype.showModal = function () { this.setAttribute("open", ""); };
    HTMLDialogElement.prototype.close = function () { this.removeAttribute("open"); this.dispatchEvent(new Event("close")); };
  }
});
afterEach(() => { cleanup(); });

function mockSecretsResponse(envelope: { vault_state: string; secrets: any[]; manifest_errors: any[] }) {
  mockFetch.mockImplementation((url: string) => {
    if (typeof url === "string" && url.includes("/api/secrets")) {
      return Promise.resolve({ ok: true, status: 200, json: async () => envelope });
    }
    return Promise.resolve({ ok: true, status: 200, json: async () => ({}) });
  });
}

async function expandEnvironmentSection() {
  // The Environment accordion is closed by default — click its header to
  // reveal the env rows + "Add environment variable" button.
  const envHeader = screen.getByRole("button", { name: /Environment/i });
  await userEvent.click(envHeader);
}

describe("AddServerScreen — A3-b SecretPicker integration (Codex plan-R1 P2-1 + plan-R2 P2)", () => {
  it("hosts EXACTLY ONE useSecretsSnapshot per form mount (one and only one GET /api/secrets)", async () => {
    mockSecretsResponse({ vault_state: "ok", secrets: [], manifest_errors: [] });
    render(<AddServerScreen />);
    await waitFor(() => {
      const secretsCalls = mockFetch.mock.calls.filter(([url]) => typeof url === "string" && url.includes("/api/secrets"));
      expect(secretsCalls.length).toBeGreaterThanOrEqual(1);
    });
    await expandEnvironmentSection();
    const addEnvBtn = screen.getByText(/Add environment variable/i);
    await userEvent.click(addEnvBtn);
    await userEvent.click(addEnvBtn);
    await userEvent.click(addEnvBtn);
    const secretsCalls = mockFetch.mock.calls.filter(([url]) => typeof url === "string" && url.includes("/api/secrets"));
    expect(secretsCalls.length).toBe(1);
  });

  it("renders BrokenRefsSummary above env section when count > 1", async () => {
    mockSecretsResponse({
      vault_state: "ok",
      secrets: [{ name: "wolfram_app_id", state: "present", used_by: [] }],
      manifest_errors: [],
    });
    const user = userEvent.setup();
    render(<AddServerScreen />);
    await expandEnvironmentSection();
    await user.click(screen.getByText(/Add environment variable/i));
    await user.click(screen.getByText(/Add environment variable/i));
    // Filter to SecretPicker inputs only — the kind <select> also has the
    // implicit combobox role.
    const inputs = Array.from(
      document.querySelectorAll<HTMLInputElement>('input.secret-picker-input'),
    );
    await user.click(inputs[0]);
    await user.keyboard("secret:never_added");
    await user.keyboard("{Escape}");
    await user.click(inputs[1]);
    await user.keyboard("secret:also_missing");
    await user.keyboard("{Tab}");
    await screen.findByText(/2 secrets referenced but not in vault/i);
    const summary = screen.getByText(/2 secrets referenced but not in vault/i);
    const firstEnvRow = document.querySelector('[data-env-row="0"]');
    expect(summary.compareDocumentPosition(firstEnvRow!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeGreaterThan(0);
  });

  it("hosts EXACTLY ONE AddSecretModal at form level after Create flow opens (Codex plan-R2 P2)", async () => {
    mockSecretsResponse({ vault_state: "ok", secrets: [], manifest_errors: [] });
    render(<AddServerScreen />);
    await waitFor(() => {
      const calls = mockFetch.mock.calls.filter(([url]) => typeof url === "string" && url.includes("/api/secrets"));
      expect(calls.length).toBeGreaterThanOrEqual(1);
    });
    await expandEnvironmentSection();
    await userEvent.click(screen.getByText(/Add environment variable/i));
    await userEvent.click(screen.getByText(/Add environment variable/i));

    expect(document.querySelectorAll('[data-testid="add-secret-modal"][open]').length).toBe(0);

    const pickBtns = screen.getAllByRole("button", { name: /Pick secret/i });
    await userEvent.click(pickBtns[0]);
    const createEntry = await screen.findByText(/Create new secret/i);
    await userEvent.click(createEntry);

    await waitFor(() => {
      const open = document.querySelectorAll('[data-testid="add-secret-modal"][open]');
      expect(open.length).toBe(1);
    });

    const allModals = document.querySelectorAll('[data-testid="add-secret-modal"]');
    expect(allModals.length).toBe(1);
  });
});
