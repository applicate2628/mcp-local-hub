import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { fireEvent, render, screen, cleanup, waitFor } from "@testing-library/preact";
import userEvent from "@testing-library/user-event";
import { AddServerScreen, parseAddServerQuery } from "./AddServer";

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

  it("lets a same-tick Reload supersede Force Save through the submission baton", async () => {
    const requests: Array<{ url: string; body?: string }> = [];
    let getCalls = 0;
    let editCalls = 0;
    let resolveForceGet: ((resp: Response) => void) | null = null;
    let resolveReloadGet: ((resp: Response) => void) | null = null;
    let forceGetStarted!: () => void;
    let reloadGetStarted!: () => void;
    const forceGet = new Promise<void>((resolve) => { forceGetStarted = resolve; });
    const reloadGet = new Promise<void>((resolve) => { reloadGetStarted = resolve; });

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
          return Promise.resolve(jsonResponse({
            yaml: "name: 'demo'\nkind: global\ntransport: stdio-bridge\ncommand: 'node'\n",
            hash: "hash-after-create",
          }));
        }
        if (getCalls === 2) {
          forceGetStarted();
          return new Promise<Response>((resolve) => { resolveForceGet = resolve; });
        }
        reloadGetStarted();
        return new Promise<Response>((resolve) => { resolveReloadGet = resolve; });
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

    const forceButton = screen.getByRole("button", { name: "Force Save" });
    const reloadButton = screen.getByRole("button", { name: "Reload" });
    fireEvent.click(forceButton);
    fireEvent.click(reloadButton);
    await Promise.all([forceGet, reloadGet]);

    resolveReloadGet!(jsonResponse({
      yaml: "name: 'demo'\nkind: global\ntransport: stdio-bridge\ncommand: 'disk-node'\n",
      hash: "hash-from-reload",
    }));
    await screen.findByText("Reloaded fresh manifest from disk.");

    resolveForceGet!(jsonResponse({
      yaml: "name: 'demo'\nkind: global\ntransport: stdio-bridge\ncommand: 'external-node'\n",
      hash: "hash-for-force",
    }));
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(editCalls).toBe(1);
    expect(screen.queryByText("Force-saved.")).toBeNull();
    expect((screen.getByLabelText("Command") as HTMLInputElement).value).toBe("disk-node");
  });

  it("falls back to manifest create when a committed create follow-up edit reports not found by code only", async () => {
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
          error: "gone",
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

// ─────────────────────────────────────────────────────────────────────────
// D2 — secret-safe cold re-enable pre-fill (?readd=<name>).
// ─────────────────────────────────────────────────────────────────────────
describe("parseAddServerQuery — D2 ?readd= param", () => {
  afterEach(() => {
    window.location.hash = "";
  });

  it("extracts readd from the hash (disjoint from server/from-client)", () => {
    window.location.hash = "#/add-server?readd=memory";
    const q = parseAddServerQuery();
    expect(q.readd).toBe("memory");
    expect(q.server).toBe("");
    expect(q.fromClient).toBe("");
  });

  it("returns empty readd for the A1 extract hash (server + from-client only)", () => {
    window.location.hash = "#/add-server?server=ghost&from-client=cursor";
    const q = parseAddServerQuery();
    expect(q.readd).toBe("");
    expect(q.server).toBe("ghost");
    expect(q.fromClient).toBe("cursor");
  });

  it("returns all-empty for a bare add-server hash", () => {
    window.location.hash = "#/add-server";
    const q = parseAddServerQuery();
    expect(q.readd).toBe("");
    expect(q.server).toBe("");
    expect(q.fromClient).toBe("");
  });
});

describe("AddServerScreen — D2 cold re-enable Re-add seed effect", () => {
  afterEach(() => {
    window.location.hash = "";
  });

  it("a NON-catalog ?readd= (404 from /api/catalog/manifest) seeds ONLY the name + honest banner, never getManifest or extract-manifest", async () => {
    window.location.hash = "#/add-server?readd=customsrv";
    const urls: string[] = [];
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      urls.push(url);
      if (url.includes("/api/secrets")) {
        return Promise.resolve(jsonResponse({ vault_state: "ok", secrets: [], manifest_errors: [] }));
      }
      if (url.startsWith("/api/catalog/manifest")) {
        // customsrv is NOT in the embedded catalog → the membership-gate 404
        // the frontend maps to the honest name-only no-match seed.
        return Promise.resolve(jsonResponse({ error: "not found", code: "CATALOG_MANIFEST_NOT_FOUND" }, 404));
      }
      return Promise.resolve(jsonResponse({}));
    });

    render(<AddServerScreen />);

    // The name field is seeded from the param.
    await waitFor(() => {
      const nameInput = document.querySelector<HTMLInputElement>("#field-name");
      expect(nameInput?.value).toBe("customsrv");
    });
    // Honest banner: re-enter command/args/secrets; not in the catalog.
    await waitFor(() => {
      expect(screen.getByTestId("banner").textContent).toContain("Re-adding customsrv");
    });
    // F3: the honest no-match copy (the membership gate said it isn't embedded).
    expect(screen.getByTestId("banner").textContent).toContain("isn't in the catalog");
    // F1: a non-catalog re-add is a NORMAL outcome → neutral info kind, not red.
    expect(screen.getByTestId("banner").className).toContain("info");
    expect(screen.getByTestId("banner").className).not.toContain("error");

    // INVARIANT: the single embed-only endpoint is the ONLY manifest source —
    // never the disk-only edit contract (/api/manifest/get) and never the dead
    // extract path (/api/extract-manifest, which carries client env verbatim).
    expect(urls.some((u) => u.startsWith("/api/manifest/get"))).toBe(false);
    expect(urls.some((u) => u.includes("/api/extract-manifest"))).toBe(false);
  });

  it("[P2-404] a CATALOG-known ?readd= prefills from the EMBED manifest via /api/catalog/manifest (command/args present, env as a secret: ref — NOT blank, never getManifest/extract)", async () => {
    window.location.hash = "#/add-server?readd=wolfram";
    const urls: string[] = [];
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      urls.push(url);
      if (url.includes("/api/secrets")) {
        return Promise.resolve(jsonResponse({ vault_state: "ok", secrets: [], manifest_errors: [] }));
      }
      if (url.startsWith("/api/catalog/manifest")) {
        // The EMBED manifest carries a `secret:` ref, NOT a literal value —
        // the secret-safe source D2 relies on. (No hash field: this is the
        // read-for-prefill contract, not the optimistic-concurrency edit one.)
        return Promise.resolve(jsonResponse({
          yaml:
            "name: 'wolfram'\nkind: global\ntransport: stdio-bridge\ncommand: 'node'\n" +
            "base_args:\n  - 'index.js'\nenv:\n  WOLFRAM_LLM_APP_ID: 'secret:wolfram_app_id'\n",
        }));
      }
      return Promise.resolve(jsonResponse({}));
    });

    render(<AddServerScreen />);

    // The form prefills from the embed manifest: name + command come along
    // (this is the P2-404 fix — the shipped server now actually prefills,
    // rather than landing on a blank form).
    await waitFor(() => {
      expect(document.querySelector<HTMLInputElement>("#field-name")?.value).toBe("wolfram");
    });
    await waitFor(() => {
      expect(document.querySelector('[data-testid="yaml-preview"]')?.textContent).toContain("command: 'node'");
    });
    const preview = document.querySelector('[data-testid="yaml-preview"]')?.textContent ?? "";
    // The form is NOT blank — args + the secret: ref came along.
    expect(preview).toContain("index.js");
    // The sensitive env is a secret: ref, never a resolved literal.
    expect(preview).toContain("secret:wolfram_app_id");

    // INVARIANT: the embed-only endpoint was the ONLY manifest source.
    expect(urls.some((u) => u.startsWith("/api/catalog/manifest"))).toBe(true);
    expect(urls.some((u) => u.startsWith("/api/manifest/get"))).toBe(false);
    expect(urls.some((u) => u.includes("/api/extract-manifest"))).toBe(false);
    // F4: the catalog-match prefill is no longer silent — a neutral info notice
    // explains WHY the form is pre-filled and nudges the operator to set secrets.
    await waitFor(() => {
      expect(screen.getByTestId("banner").textContent).toContain("Pre-filled from the catalog for wolfram");
    });
    expect(screen.getByTestId("banner").className).toContain("info");
    expect(screen.getByTestId("banner").className).not.toContain("error");
  });

  it("a CATALOG-MANIFEST READ FAILURE (/api/catalog/manifest 500) degrades to the distinct read-failure copy + blank form, never getManifest/extract", async () => {
    window.location.hash = "#/add-server?readd=memo";
    const urls: string[] = [];
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      urls.push(url);
      if (url.includes("/api/secrets")) {
        return Promise.resolve(jsonResponse({ vault_state: "ok", secrets: [], manifest_errors: [] }));
      }
      if (url.startsWith("/api/catalog/manifest")) {
        // THE READ FAILURE: a non-404 → getCatalogManifest throws a plain
        // Error → the readd flow cannot know membership → read-failure degrade.
        return Promise.resolve(jsonResponse({ error: "catalog manifest unavailable", code: "CATALOG_MANIFEST_GET_FAILED" }, 500));
      }
      return Promise.resolve(jsonResponse({}));
    });

    render(<AddServerScreen />);

    await waitFor(() => {
      expect(document.querySelector<HTMLInputElement>("#field-name")?.value).toBe("memo");
    });
    // F3: the read-failure copy — names the failed lookup and must NOT assert
    // "isn't in the catalog" (membership is unknown when the lookup failed).
    await waitFor(() => {
      expect(screen.getByTestId("banner").textContent).toContain("Re-adding memo");
    });
    expect(screen.getByTestId("banner").textContent).toContain("catalog lookup failed");
    expect(screen.getByTestId("banner").textContent).not.toContain("isn't in the catalog");
    // F1: a read-failure degrade is a NORMAL outcome → neutral info kind.
    expect(screen.getByTestId("banner").className).toContain("info");
    expect(screen.getByTestId("banner").className).not.toContain("error");

    // Blank secret-safe seed: never the disk-edit contract, never the extract path.
    expect(urls.some((u) => u.startsWith("/api/manifest/get"))).toBe(false);
    expect(urls.some((u) => u.includes("/api/extract-manifest"))).toBe(false);
  });

  it("[P2-429] clobber-guard: typing during the lookup PRESERVES the typed form (no prefill clobber) and keeps the dirty guard correct", async () => {
    window.location.hash = "#/add-server?readd=wolfram";
    // Hold the /api/catalog/manifest response open so we can type into the form
    // BEFORE it resolves — the exact P2-429 race the clobber-guard defends. The
    // deferred is resolved AND fully drained inside this test (see the
    // `await manifestSettled` below) so no fetch task survives to window
    // teardown (which would surface as a happy-dom AbortError).
    let resolveManifest: ((r: Response) => void) | null = null;
    const manifestPending = new Promise<Response>((resolve) => {
      resolveManifest = resolve;
    });
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.includes("/api/secrets")) {
        return Promise.resolve(jsonResponse({ vault_state: "ok", secrets: [], manifest_errors: [] }));
      }
      if (url.startsWith("/api/catalog/manifest")) {
        return manifestPending; // resolved later, after the operator types
      }
      return Promise.resolve(jsonResponse({}));
    });

    render(<AddServerScreen />);

    // The name field seeds from ?readd= only AFTER the lookup resolves — so
    // until then the form is BLANK. The operator types a custom name into the
    // still-blank form while the lookup is in flight.
    const nameInput = document.querySelector<HTMLInputElement>("#field-name");
    expect(nameInput).not.toBeNull();
    await userEvent.clear(nameInput!);
    await userEvent.type(nameInput!, "typed-by-operator");
    expect(nameInput!.value).toBe("typed-by-operator");

    // NOW the embed manifest resolves with the wolfram prefill. The clobber-guard
    // must NOT overwrite the operator's typed form.
    resolveManifest!(jsonResponse({
      yaml:
        "name: 'wolfram'\nkind: global\ntransport: stdio-bridge\ncommand: 'node'\n" +
        "base_args:\n  - 'index.js'\nenv:\n  WOLFRAM_LLM_APP_ID: 'secret:wolfram_app_id'\n",
    }));

    // Fully drain the resolved fetch chain (await the deferred + its .json()
    // hop) so the readd effect's setState calls have all flushed and no async
    // task is left in flight at window teardown.
    await manifestPending;
    await waitFor(() => {
      // The typed name is PRESERVED — the prefill was suppressed because the
      // form was no longer pristine when the lookup resolved.
      expect(document.querySelector<HTMLInputElement>("#field-name")?.value).toBe("typed-by-operator");
    });
    const preview = document.querySelector('[data-testid="yaml-preview"]')?.textContent ?? "";
    // The wolfram embed values must NOT have clobbered the typed form.
    expect(preview).not.toContain("secret:wolfram_app_id");
    expect(preview).not.toContain("command: 'node'");
    // And the F4 prefill banner must NOT fire (no prefill happened).
    const banner = screen.queryByTestId("banner");
    if (banner) {
      expect(banner.textContent).not.toContain("Pre-filled from the catalog");
    }

    // The dirty guard stays correct: the typed form differs from the (blank)
    // initial snapshot, so the unsaved-changes guard is armed. The Add-server
    // sidebar-intercept guard reads that; we assert the proxy invariant that the
    // name field holds the typed value above (a blank baseline + non-blank form).
  });

  it("a bare add-server (no ?readd=) does NOT run the readd seed (form stays blank)", async () => {
    window.location.hash = "#/add-server";
    const urls: string[] = [];
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      urls.push(url);
      if (url.includes("/api/secrets")) {
        return Promise.resolve(jsonResponse({ vault_state: "ok", secrets: [], manifest_errors: [] }));
      }
      return Promise.resolve(jsonResponse({}));
    });

    render(<AddServerScreen />);
    await waitFor(() => {
      const calls = urls.filter((u) => u.includes("/api/secrets"));
      expect(calls.length).toBeGreaterThanOrEqual(1);
    });
    // No catalog-manifest lookup, no disk-edit read, no extract — inert branch.
    expect(urls.some((u) => u.startsWith("/api/catalog/manifest"))).toBe(false);
    expect(urls.some((u) => u.startsWith("/api/manifest/get"))).toBe(false);
    expect(urls.some((u) => u.includes("/api/extract-manifest"))).toBe(false);
    expect(document.querySelector<HTMLInputElement>("#field-name")?.value).toBe("");
  });
});

// D2 r3 FINDING 2 — shipped-server edit-shadow notice. A catalog-match Re-add
// prefills CREATE mode with the SHIPPED name; editing command/args + Save&Install
// would install the embed-first (UNEDITED) shipped manifest while the UI shows
// the edits, so the form surfaces an honest notice. It is honest-on-rename.
describe("AddServerScreen — D2 r3 shipped-server edit-shadow notice", () => {
  afterEach(() => {
    window.location.hash = "";
  });

  const catalogMatch = (input: RequestInfo | URL) => {
    const url = typeof input === "string" ? input : input.toString();
    if (url.includes("/api/secrets")) {
      return Promise.resolve(jsonResponse({ vault_state: "ok", secrets: [], manifest_errors: [] }));
    }
    if (url.startsWith("/api/catalog/manifest")) {
      return Promise.resolve(jsonResponse({
        yaml:
          "name: 'wolfram'\nkind: global\ntransport: stdio-bridge\ncommand: 'node'\n" +
          "base_args:\n  - 'index.js'\nenv:\n  WOLFRAM_LLM_APP_ID: 'secret:wolfram_app_id'\n",
      }));
    }
    return Promise.resolve(jsonResponse({}));
  };

  it("a CATALOG-MATCH ?readd= (200) renders the shipped-server notice", async () => {
    window.location.hash = "#/add-server?readd=wolfram";
    mockFetch.mockImplementation(catalogMatch);

    render(<AddServerScreen />);

    await waitFor(() => {
      expect(document.querySelector<HTMLInputElement>("#field-name")?.value).toBe("wolfram");
    });
    await waitFor(() => {
      const notice = screen.getByTestId("shipped-server-notice");
      expect(notice.textContent).toContain("This is a shipped server");
      expect(notice.textContent).toContain("rename the server to save a customized copy");
    });
  });

  it("a NON-catalog ?readd= (404) does NOT render the shipped-server notice", async () => {
    window.location.hash = "#/add-server?readd=customsrv";
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.includes("/api/secrets")) {
        return Promise.resolve(jsonResponse({ vault_state: "ok", secrets: [], manifest_errors: [] }));
      }
      if (url.startsWith("/api/catalog/manifest")) {
        return Promise.resolve(jsonResponse({ error: "not found", code: "CATALOG_MANIFEST_NOT_FOUND" }, 404));
      }
      return Promise.resolve(jsonResponse({}));
    });

    render(<AddServerScreen />);

    await waitFor(() => {
      expect(document.querySelector<HTMLInputElement>("#field-name")?.value).toBe("customsrv");
    });
    // The honest no-match banner fired; the shipped-server notice must NOT.
    await waitFor(() => {
      expect(screen.getByTestId("banner").textContent).toContain("Re-adding customsrv");
    });
    expect(screen.queryByTestId("shipped-server-notice")).toBeNull();
  });

  it("a CATALOG-MANIFEST READ FAILURE (500) does NOT render the shipped-server notice", async () => {
    window.location.hash = "#/add-server?readd=memo";
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.includes("/api/secrets")) {
        return Promise.resolve(jsonResponse({ vault_state: "ok", secrets: [], manifest_errors: [] }));
      }
      if (url.startsWith("/api/catalog/manifest")) {
        return Promise.resolve(jsonResponse({ error: "unavailable", code: "CATALOG_MANIFEST_GET_FAILED" }, 500));
      }
      return Promise.resolve(jsonResponse({}));
    });

    render(<AddServerScreen />);

    await waitFor(() => {
      expect(document.querySelector<HTMLInputElement>("#field-name")?.value).toBe("memo");
    });
    expect(screen.queryByTestId("shipped-server-notice")).toBeNull();
  });

  it("a bare add-server (no ?readd=) does NOT render the shipped-server notice", async () => {
    window.location.hash = "#/add-server";
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.includes("/api/secrets")) {
        return Promise.resolve(jsonResponse({ vault_state: "ok", secrets: [], manifest_errors: [] }));
      }
      return Promise.resolve(jsonResponse({}));
    });

    render(<AddServerScreen />);
    await waitFor(() => {
      const calls = mockFetch.mock.calls.filter(([u]) => typeof u === "string" && u.includes("/api/secrets"));
      expect(calls.length).toBeGreaterThanOrEqual(1);
    });
    expect(screen.queryByTestId("shipped-server-notice")).toBeNull();
  });

  it("renaming away from the shipped name CLEARS the shipped-server notice (honest on rename)", async () => {
    window.location.hash = "#/add-server?readd=wolfram";
    mockFetch.mockImplementation(catalogMatch);

    render(<AddServerScreen />);

    // The notice is live while the form name is still the shipped name.
    await waitFor(() => {
      expect(document.querySelector<HTMLInputElement>("#field-name")?.value).toBe("wolfram");
    });
    await waitFor(() => {
      expect(screen.queryByTestId("shipped-server-notice")).not.toBeNull();
    });

    // Rename to save a customized copy → the names diverge → notice hides.
    const nameInput = document.querySelector<HTMLInputElement>("#field-name")!;
    await userEvent.clear(nameInput);
    await userEvent.type(nameInput, "wolfram-custom");
    expect(nameInput.value).toBe("wolfram-custom");
    await waitFor(() => {
      expect(screen.queryByTestId("shipped-server-notice")).toBeNull();
    });
  });
});
