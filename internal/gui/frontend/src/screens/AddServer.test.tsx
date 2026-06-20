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

function accordionHeader(name: RegExp): HTMLButtonElement {
  const header = screen
    .getAllByRole("button", { name })
    .find((button) => button.classList.contains("accordion-header"));
  if (!header) throw new Error(`accordion header not found for ${name}`);
  return header as HTMLButtonElement;
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
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

describe("AddServerScreen — readiness inline secrets", () => {
  it("opens the readiness accordion when the first readiness response inserts it", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = init?.method ?? "GET";
      if (url === "/api/secrets" && method === "GET") {
        return Promise.resolve(jsonResponse({ vault_state: "ok", secrets: [], manifest_errors: [] }));
      }
      if (url === "/api/server/readiness") {
        return Promise.resolve(jsonResponse({ server: "demo", ready: true, requirements: [] }));
      }
      return Promise.resolve(jsonResponse({}));
    });

    const user = userEvent.setup();
    render(<AddServerScreen />);

    const nameInput = document.querySelector<HTMLInputElement>("#field-name");
    expect(nameInput).toBeTruthy();
    await user.type(nameInput!, "demo");

    const readinessHeader = await screen.findByRole("button", { name: /Install readiness/i });
    expect(readinessHeader.getAttribute("aria-expanded")).toBe("true");
    expect(screen.queryByTestId("readiness-panel")).toBeTruthy();
  });

  it("keeps Environment accordion state when readiness is inserted before it", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = init?.method ?? "GET";
      if (url === "/api/secrets" && method === "GET") {
        return Promise.resolve(jsonResponse({ vault_state: "ok", secrets: [], manifest_errors: [] }));
      }
      if (url === "/api/server/readiness") {
        return Promise.resolve(jsonResponse({ server: "demo", ready: true, requirements: [] }));
      }
      return Promise.resolve(jsonResponse({}));
    });

    const user = userEvent.setup();
    render(<AddServerScreen />);
    await expandEnvironmentSection();
    expect(accordionHeader(/Environment/i).getAttribute("aria-expanded")).toBe("true");

    const nameInput = document.querySelector<HTMLInputElement>("#field-name");
    expect(nameInput).toBeTruthy();
    await user.type(nameInput!, "demo");
    await screen.findByRole("button", { name: /Install readiness/i });

    expect(accordionHeader(/Environment/i).getAttribute("aria-expanded")).toBe("true");
    expect(screen.queryByText(/Add environment variable/i)).toBeTruthy();
  });

  it("clears typed inline secret values once the vault snapshot reports the key present", async () => {
    const requests: Array<{ url: string; method: string; body?: string }> = [];
    let vaultHasApiKey = false;
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = init?.method ?? "GET";
      requests.push({ url, method, body: init?.body as string | undefined });
      if (url === "/api/secrets" && method === "GET") {
        return Promise.resolve(jsonResponse({
          vault_state: "ok",
          secrets: vaultHasApiKey ? [{ name: "API_KEY", state: "present", used_by: [] }] : [],
          manifest_errors: [],
        }));
      }
      if (url === "/api/secrets" && method === "POST") {
        return Promise.resolve(jsonResponse({}, 201));
      }
      if (url === "/api/server/readiness") {
        return Promise.resolve(jsonResponse({
          server: "demo",
          ready: true,
          requirements: [{
            name: "secret: API_KEY",
            ok: vaultHasApiKey,
            optional: true,
            reason: vaultHasApiKey ? undefined : "not set",
          }],
        }));
      }
      if (url === "/api/manifest/validate") {
        return Promise.resolve(jsonResponse({ warnings: [] }));
      }
      if (url === "/api/manifest/create") {
        return Promise.resolve(jsonResponse({ restart_required: false, hub_live: false }));
      }
      if (url.startsWith("/api/manifest/get")) {
        return Promise.resolve(jsonResponse({
          yaml: "name: 'demo'\nkind: global\ntransport: stdio-bridge\ncommand: 'node'\nenv:\n  API_KEY: secret:API_KEY\n",
          hash: "hash-after-create",
        }));
      }
      if (url.startsWith("/api/install")) {
        return Promise.resolve(jsonResponse({}));
      }
      return Promise.reject(new Error(`unexpected fetch: ${url}`));
    });

    const user = userEvent.setup();
    render(<AddServerScreen />);

    const nameInput = document.querySelector<HTMLInputElement>("#field-name");
    expect(nameInput).toBeTruthy();
    await user.type(nameInput!, "demo");
    await user.click(screen.getByRole("button", { name: /Command/i }));
    await user.type(screen.getByLabelText("Command"), "node");
    await expandEnvironmentSection();
    await user.click(screen.getByText(/Add environment variable/i));
    const envRow = document.querySelector<HTMLElement>('[data-env-row="0"]')!;
    await user.type(envRow.querySelector<HTMLInputElement>('input[placeholder="KEY"]')!, "API_KEY");
    const secretInput = envRow.querySelector<HTMLInputElement>("input.secret-picker-input")!;
    await user.click(secretInput);
    await user.keyboard("secret:API_KEY");
    await user.keyboard("{Tab}");

    const inlineInput = await screen.findByTestId("readiness-secret-input-API_KEY") as HTMLInputElement;
    await user.type(inlineInput, "old-hidden-value");

    vaultHasApiKey = true;
    window.dispatchEvent(new Event("focus"));
    await waitFor(() => {
      expect(screen.queryByTestId("readiness-secret-input-API_KEY")).toBeNull();
    });

    vaultHasApiKey = false;
    window.dispatchEvent(new Event("focus"));
    const reappearedInput = await screen.findByTestId("readiness-secret-input-API_KEY") as HTMLInputElement;
    expect(reappearedInput.value).toBe("");

    await user.click(document.querySelector<HTMLButtonElement>('[data-action="save-and-install"]')!);
    await waitFor(() => {
      expect(requests.filter((r) => r.url.startsWith("/api/install"))).toHaveLength(1);
    });
    expect(requests.filter((r) => r.url === "/api/secrets" && r.method === "POST")).toHaveLength(0);
  });

  it("writes an inline secret when live readiness says missing even if the local snapshot is stale-present", async () => {
    const requests: Array<{ url: string; method: string; body?: string }> = [];
    let servedStalePresentSnapshot = false;
    let stored = false;
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = init?.method ?? "GET";
      requests.push({ url, method, body: init?.body as string | undefined });
      if (url === "/api/secrets" && method === "GET") {
        const present = !servedStalePresentSnapshot || stored;
        servedStalePresentSnapshot = true;
        return Promise.resolve(jsonResponse({
          vault_state: "ok",
          secrets: present ? [{ name: "API_KEY", state: "present", used_by: [] }] : [],
          manifest_errors: [],
        }));
      }
      if (url === "/api/secrets" && method === "POST") {
        stored = true;
        return Promise.resolve(jsonResponse({}, 201));
      }
      if (url === "/api/server/readiness") {
        return Promise.resolve(jsonResponse({
          server: "demo",
          ready: true,
          requirements: [{
            name: "secret: API_KEY",
            ok: false,
            optional: true,
            reason: "not set",
          }],
        }));
      }
      if (url === "/api/manifest/validate") {
        return Promise.resolve(jsonResponse({ warnings: [] }));
      }
      if (url === "/api/manifest/create") {
        return Promise.resolve(jsonResponse({ restart_required: false, hub_live: false }));
      }
      if (url.startsWith("/api/manifest/get")) {
        return Promise.resolve(jsonResponse({
          yaml: "name: 'demo'\nkind: global\ntransport: stdio-bridge\ncommand: 'node'\nenv:\n  API_KEY: secret:API_KEY\n",
          hash: "hash-after-create",
        }));
      }
      if (url.startsWith("/api/install")) {
        return Promise.resolve(jsonResponse({}));
      }
      return Promise.reject(new Error(`unexpected fetch: ${url}`));
    });

    const user = userEvent.setup();
    render(<AddServerScreen />);

    const nameInput = document.querySelector<HTMLInputElement>("#field-name");
    expect(nameInput).toBeTruthy();
    await user.type(nameInput!, "demo");
    await user.click(screen.getByRole("button", { name: /Command/i }));
    await user.type(screen.getByLabelText("Command"), "node");
    await expandEnvironmentSection();
    await user.click(screen.getByText(/Add environment variable/i));
    const envRow = document.querySelector<HTMLElement>('[data-env-row="0"]')!;
    await user.type(envRow.querySelector<HTMLInputElement>('input[placeholder="KEY"]')!, "API_KEY");
    const secretInput = envRow.querySelector<HTMLInputElement>("input.secret-picker-input")!;
    await user.click(secretInput);
    await user.keyboard("secret:API_KEY");
    await user.keyboard("{Tab}");

    const inlineInput = await screen.findByTestId("readiness-secret-input-API_KEY") as HTMLInputElement;
    await user.type(inlineInput, "fresh-secret-value");

    await user.click(document.querySelector<HTMLButtonElement>('[data-action="save-and-install"]')!);
    await waitFor(() => {
      expect(requests.filter((r) => r.url.startsWith("/api/install"))).toHaveLength(1);
    });

    const secretPosts = requests.filter((r) => r.url === "/api/secrets" && r.method === "POST");
    expect(secretPosts).toHaveLength(1);
    expect(JSON.parse(secretPosts[0].body ?? "{}")).toEqual({
      name: "API_KEY",
      value: "fresh-secret-value",
    });
  });
});

describe("AddServerScreen — create save follow-up edits", () => {
  it("uses manifest edit after a create-mode save has committed", async () => {
    const requests: Array<{ url: string; body?: string }> = [];
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      requests.push({ url, body: init?.body as string | undefined });
      if (url.includes("/api/secrets")) {
        return Promise.resolve(jsonResponse({ vault_state: "ok", secrets: [], manifest_errors: [] }));
      }
      if (url === "/api/server/readiness") {
        return Promise.resolve(jsonResponse({ server: "demo", ready: true, requirements: [] }));
      }
      if (url === "/api/manifest/validate") {
        return Promise.resolve(jsonResponse({ warnings: [] }));
      }
      if (url === "/api/manifest/create") {
        return Promise.resolve(jsonResponse({ restart_required: false, hub_live: false }));
      }
      if (url.startsWith("/api/manifest/get")) {
        return Promise.resolve(jsonResponse({
          yaml: "name: 'demo'\nkind: global\ntransport: stdio-bridge\ncommand: 'node'\n",
          hash: "hash-after-create",
        }));
      }
      if (url === "/api/manifest/edit") {
        return Promise.resolve(jsonResponse({ hash: "hash-after-edit", restart_required: false, hub_live: false }));
      }
      return Promise.reject(new Error(`unexpected fetch: ${url}`));
    });

    const user = userEvent.setup();
    render(<AddServerScreen />);

    const nameInput = document.querySelector<HTMLInputElement>("#field-name");
    expect(nameInput).toBeTruthy();
    await user.type(nameInput!, "demo");
    await user.click(screen.getByRole("button", { name: /Command/i }));
    await user.type(screen.getByLabelText("Command"), "node");
    await user.click(document.querySelector<HTMLButtonElement>('[data-action="save"]')!);

    await waitFor(() => {
      expect(requests.filter((r) => r.url === "/api/manifest/create")).toHaveLength(1);
    });

    await user.clear(screen.getByLabelText("Command"));
    await user.type(screen.getByLabelText("Command"), "node2");
    await user.click(document.querySelector<HTMLButtonElement>('[data-action="save"]')!);

    await waitFor(() => {
      expect(requests.filter((r) => r.url === "/api/manifest/edit")).toHaveLength(1);
    });
    expect(requests.filter((r) => r.url === "/api/manifest/create")).toHaveLength(1);
    const editBody = JSON.parse(requests.find((r) => r.url === "/api/manifest/edit")?.body ?? "{}") as {
      expected_hash?: string;
      name?: string;
      yaml?: string;
    };
    expect(editBody.name).toBe("demo");
    expect(editBody.expected_hash).toBe("hash-after-create");
    expect(editBody.yaml).toContain("command: 'node2'");
  });

  it("uses manifest edit after create commits even when hash refresh fails once", async () => {
    const requests: Array<{ url: string; body?: string }> = [];
    let getCalls = 0;
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      requests.push({ url, body: init?.body as string | undefined });
      if (url.includes("/api/secrets")) {
        return Promise.resolve(jsonResponse({ vault_state: "ok", secrets: [], manifest_errors: [] }));
      }
      if (url === "/api/server/readiness") {
        return Promise.resolve(jsonResponse({ server: "demo", ready: true, requirements: [] }));
      }
      if (url === "/api/manifest/validate") {
        return Promise.resolve(jsonResponse({ warnings: [] }));
      }
      if (url === "/api/manifest/create") {
        return Promise.resolve(jsonResponse({ restart_required: false, hub_live: false }));
      }
      if (url.startsWith("/api/manifest/get")) {
        getCalls++;
        if (getCalls === 1) {
          return Promise.resolve(jsonResponse({ error: "transient read failed" }, 503));
        }
        return Promise.resolve(jsonResponse({
          yaml: "name: 'demo'\nkind: global\ntransport: stdio-bridge\ncommand: 'disk-node'\n",
          hash: "hash-after-retry",
        }));
      }
      if (url === "/api/manifest/edit") {
        return Promise.resolve(jsonResponse({ hash: "hash-after-edit", restart_required: false, hub_live: false }));
      }
      return Promise.reject(new Error(`unexpected fetch: ${url}`));
    });

    const user = userEvent.setup();
    render(<AddServerScreen />);

    const nameInput = document.querySelector<HTMLInputElement>("#field-name");
    expect(nameInput).toBeTruthy();
    await user.type(nameInput!, "demo");
    await user.click(screen.getByRole("button", { name: /Command/i }));
    await user.type(screen.getByLabelText("Command"), "node");
    const saveButton = document.querySelector<HTMLButtonElement>('[data-action="save"]')!;
    await user.click(saveButton);

    await waitFor(() => {
      expect(requests.filter((r) => r.url === "/api/manifest/create")).toHaveLength(1);
      expect(requests.filter((r) => r.url.startsWith("/api/manifest/get"))).toHaveLength(1);
    });
    await waitFor(() => {
      expect(saveButton.disabled).toBe(false);
    });

    await user.clear(screen.getByLabelText("Command"));
    await user.type(screen.getByLabelText("Command"), "node2");
    await user.click(saveButton);

    await waitFor(() => {
      expect(requests.filter((r) => r.url === "/api/manifest/edit")).toHaveLength(1);
    });
    expect(requests.filter((r) => r.url === "/api/manifest/create")).toHaveLength(1);
    expect(requests.filter((r) => r.url.startsWith("/api/manifest/get"))).toHaveLength(2);
    const editBody = JSON.parse(requests.find((r) => r.url === "/api/manifest/edit")?.body ?? "{}") as {
      expected_hash?: string;
      name?: string;
      yaml?: string;
    };
    expect(editBody.name).toBe("demo");
    expect(editBody.expected_hash).toBe("hash-after-retry");
    expect(editBody.yaml).toContain("command: 'node2'");
  });

  it("offers Force Save after a create-mode follow-up edit hits a stale hash", async () => {
    const requests: Array<{ url: string; body?: string }> = [];
    let getCalls = 0;
    let editCalls = 0;
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      requests.push({ url, body: init?.body as string | undefined });
      if (url.includes("/api/secrets")) {
        return Promise.resolve(jsonResponse({ vault_state: "ok", secrets: [], manifest_errors: [] }));
      }
      if (url === "/api/server/readiness") {
        return Promise.resolve(jsonResponse({ server: "demo", ready: true, requirements: [] }));
      }
      if (url === "/api/manifest/validate") {
        return Promise.resolve(jsonResponse({ warnings: [] }));
      }
      if (url === "/api/manifest/create") {
        return Promise.resolve(jsonResponse({ restart_required: false, hub_live: false }));
      }
      if (url.startsWith("/api/manifest/get")) {
        getCalls++;
        return Promise.resolve(jsonResponse({
          yaml: "name: 'demo'\nkind: global\ntransport: stdio-bridge\ncommand: 'disk-node'\n",
          hash: getCalls === 1 ? "hash-after-create" : "hash-external",
        }));
      }
      if (url === "/api/manifest/edit") {
        editCalls++;
        if (editCalls === 1) {
          return Promise.resolve(jsonResponse({
            code: "MANIFEST_HASH_MISMATCH",
            error: "manifest hash mismatch",
          }, 409));
        }
        return Promise.resolve(jsonResponse({ hash: "hash-after-force", restart_required: false, hub_live: false }));
      }
      return Promise.reject(new Error(`unexpected fetch: ${url}`));
    });

    const user = userEvent.setup();
    render(<AddServerScreen />);

    const nameInput = document.querySelector<HTMLInputElement>("#field-name");
    expect(nameInput).toBeTruthy();
    await user.type(nameInput!, "demo");
    await user.click(screen.getByRole("button", { name: /Command/i }));
    await user.type(screen.getByLabelText("Command"), "node");
    await user.click(document.querySelector<HTMLButtonElement>('[data-action="save"]')!);

    await waitFor(() => {
      expect(requests.filter((r) => r.url === "/api/manifest/create")).toHaveLength(1);
    });

    await user.clear(screen.getByLabelText("Command"));
    await user.type(screen.getByLabelText("Command"), "node2");
    await user.click(document.querySelector<HTMLButtonElement>('[data-action="save"]')!);

    await screen.findByText(/Manifest changed on disk since you opened it/i);
    expect(screen.getByRole("button", { name: "Reload" })).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Force Save" }));

    await screen.findByText("Force-saved.");
    const editBodies = requests
      .filter((r) => r.url === "/api/manifest/edit")
      .map((r) => JSON.parse(r.body ?? "{}") as { expected_hash?: string; yaml?: string; name?: string });
    expect(editBodies).toHaveLength(2);
    expect(editBodies[0].expected_hash).toBe("hash-after-create");
    expect(editBodies[1].name).toBe("demo");
    expect(editBodies[1].expected_hash).toBe("hash-external");
    expect(editBodies[1].yaml).toContain("command: 'node2'");
  });

  it("falls back to manifest create when a committed create follow-up edit reports not found", async () => {
    const requests: Array<{ url: string; body?: string }> = [];
    let createCalls = 0;
    let editCalls = 0;
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      requests.push({ url, body: init?.body as string | undefined });
      if (url.includes("/api/secrets")) {
        return Promise.resolve(jsonResponse({ vault_state: "ok", secrets: [], manifest_errors: [] }));
      }
      if (url === "/api/server/readiness") {
        return Promise.resolve(jsonResponse({ server: "demo", ready: true, requirements: [] }));
      }
      if (url === "/api/manifest/validate") {
        return Promise.resolve(jsonResponse({ warnings: [] }));
      }
      if (url === "/api/manifest/create") {
        createCalls++;
        return Promise.resolve(jsonResponse({ restart_required: false, hub_live: false }));
      }
      if (url.startsWith("/api/manifest/get")) {
        return Promise.resolve(jsonResponse({
          yaml: "name: 'demo'\nkind: global\ntransport: stdio-bridge\ncommand: 'node'\n",
          hash: createCalls <= 1 ? "hash-after-create" : "hash-after-recreate",
        }));
      }
      if (url === "/api/manifest/edit") {
        editCalls++;
        return Promise.resolve(jsonResponse({
          code: "MANIFEST_NOT_FOUND",
          error: 'manifest "demo" does not exist',
        }, 404));
      }
      return Promise.reject(new Error(`unexpected fetch: ${url}`));
    });

    const user = userEvent.setup();
    render(<AddServerScreen />);

    const nameInput = document.querySelector<HTMLInputElement>("#field-name");
    expect(nameInput).toBeTruthy();
    await user.type(nameInput!, "demo");
    await user.click(screen.getByRole("button", { name: /Command/i }));
    await user.type(screen.getByLabelText("Command"), "node");
    await user.click(document.querySelector<HTMLButtonElement>('[data-action="save"]')!);

    await waitFor(() => {
      expect(requests.filter((r) => r.url === "/api/manifest/create")).toHaveLength(1);
    });

    await user.clear(screen.getByLabelText("Command"));
    await user.type(screen.getByLabelText("Command"), "node2");
    await user.click(document.querySelector<HTMLButtonElement>('[data-action="save"]')!);

    await waitFor(() => {
      expect(requests.filter((r) => r.url === "/api/manifest/create")).toHaveLength(2);
    });
    expect(editCalls).toBe(1);
    const createBodies = requests
      .filter((r) => r.url === "/api/manifest/create")
      .map((r) => JSON.parse(r.body ?? "{}") as { name?: string; yaml?: string });
    expect(createBodies[1].name).toBe("demo");
    expect(createBodies[1].yaml).toContain("command: 'node2'");
  });
});
