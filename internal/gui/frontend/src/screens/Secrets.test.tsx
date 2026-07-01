// internal/gui/frontend/src/screens/Secrets.test.tsx
//
// Secrets screen — pins the `mcphub secrets edit` shell-out affordance
// (roadmap "Secrets — shell-out to mcphub secrets edit").
//
// VERIFIED design decision (internal/cli/secrets.go:234-325): `mcphub
// secrets edit` is an INTERACTIVE-TERMINAL-only command — it exports the
// decrypted vault to a temp file, launches $EDITOR (or `notepad`) wired to
// os.Stdin/os.Stdout/os.Stderr, and BLOCKS on the editor process until it
// exits, then re-encrypts on save. The GUI backend has no controlling
// terminal, so spawning it from an HTTP handler would be a broken, hung
// spawn. The honest implementation is therefore an INSTRUCTIONAL CTA: show
// the exact command and let the operator run it in their own terminal —
// implemented by EditVaultBanner in Secrets.tsx. These tests assert that
// instructional-CTA contract (the command text + clipboard copy), and that
// no spawn/exec round-trip is issued.
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/preact";
import { SecretsScreen } from "./Secrets";
import type { SecretsEnvelope } from "../lib/secrets-api";

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

// A "missing" vault envelope keeps the screen on the NotInitView branch so
// no extra /api/status fetch fires (InitKeyedView's running-daemon count).
// EditVaultBanner renders regardless of vault_state, which is the surface
// under test here.
const missingVault: SecretsEnvelope = {
  vault_state: "missing",
  secrets: [],
  manifest_errors: [],
};

// fetchRouter dispatches each fetch call to the matching response based on
// the request URL prefix (same helper idiom as Catalog.test.tsx /
// Servers.test.tsx).
function fetchRouter(routes: Record<string, (init?: RequestInit) => Response>) {
  return vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input.toString();
    for (const prefix of Object.keys(routes)) {
      if (url.startsWith(prefix)) {
        return routes[prefix](init);
      }
    }
    throw new Error(`unexpected fetch: ${url}`);
  });
}

const EXPECTED_CMD = "mcphub secrets edit";

describe("SecretsScreen — `mcphub secrets edit` instructional CTA", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });
  afterEach(() => {
    cleanup();
  });

  it("renders the exact `mcphub secrets edit` command in the edit-vault banner", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/secrets": () => jsonResponse(200, missingVault),
      }) as unknown as typeof fetch,
    );

    render(<SecretsScreen />);
    const banner = await screen.findByTestId("edit-vault-banner");
    // The banner instructs the operator to run the command in a terminal —
    // the honest path for an interactive-terminal-only command — and shows
    // the EXACT command string, not a spawn affordance.
    expect(banner.textContent).toContain(EXPECTED_CMD);
    expect(banner.textContent).toContain("in a terminal");
    expect(banner.querySelector("code")?.textContent).toBe(EXPECTED_CMD);
  });

  it("copies the exact command to the clipboard and shows 'Copied' (no spawn round-trip)", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    // happy-dom does not provide navigator.clipboard by default; install a
    // spec-shaped stub so the copy handler resolves.
    Object.defineProperty(navigator, "clipboard", {
      value: { writeText },
      configurable: true,
      writable: true,
    });

    const fetchMock = fetchRouter({
      "/api/secrets": () => jsonResponse(200, missingVault),
    });
    vi.spyOn(globalThis, "fetch").mockImplementation(fetchMock as unknown as typeof fetch);

    render(<SecretsScreen />);
    await screen.findByTestId("edit-vault-banner");

    const copyBtn = screen.getByRole("button", { name: /Copy command/i });
    fireEvent.click(copyBtn);

    // The button writes the EXACT command to the clipboard for the operator
    // to paste into their own terminal — the instructional-CTA contract.
    await waitFor(() => {
      expect(writeText).toHaveBeenCalledWith(EXPECTED_CMD);
    });
    // Affordance feedback flips to "Copied".
    await waitFor(() => {
      expect(screen.getByRole("button", { name: /Copied/i })).toBeTruthy();
    });

    // The copy is purely client-side: only the initial /api/secrets snapshot
    // load was issued. No spawn/exec POST (no /api/secrets/edit or similar)
    // was fired — proving this is an instructional CTA, not a broken spawn.
    const urls = (fetchMock.mock.calls as unknown[][]).map((c) =>
      typeof c[0] === "string" ? (c[0] as string) : String(c[0]),
    );
    expect(urls.every((u) => u.startsWith("/api/secrets"))).toBe(true);
    expect(urls.some((u) => u.includes("edit"))).toBe(false);
  });
});

describe("SecretsScreen — access_denied vault state", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });
  afterEach(() => {
    cleanup();
  });

  // The `remediation` value below mirrors the shape vaultAccessDeniedFix
  // (internal/api/secrets.go) actually produces for the DACL-broadening /
  // canonical-vault branch — the backend is the single owner of this text
  // (deep-review P3); the fixture pins the CONTRACT (what fields the backend
  // is expected to send), not a re-authored copy of the wording.
  const daclBreadthRemediation =
    "Tighten the vault files .age-key + secrets.age to owner-only (Windows: icacls; Linux/macOS: chmod 600) and their parent directory too (Windows: icacls <dir> /inheritance:r /grant:r ...; Linux/macOS: chmod 700 <dir>). To repair just the vault FILE permissions you may also run `mcphub repair-state-dacl --path <file>` (it repairs a state file, not a directory). See the \"secret daemons exit 1 on a sandbox-broadened %LOCALAPPDATA%\" runbook.";

  it("renders owner-only DACL remediation and does not suggest deleting vault files", async () => {
    const accessDeniedVault: SecretsEnvelope = {
      vault_state: "access_denied",
      secrets: [{ name: "WOLFRAM_APP_ID", state: "referenced_unverified", used_by: [{ server: "wolfram", env_var: "WOLFRAM_APP_ID" }] }],
      manifest_errors: [],
      remediation: daclBreadthRemediation,
    };
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/secrets": () => jsonResponse(200, accessDeniedVault),
      }) as unknown as typeof fetch,
    );

    render(<SecretsScreen />);
    const banner = await screen.findByTestId("vault-access-denied-banner");
    const text = banner.textContent ?? "";
    expect(text).toContain("Vault access denied");
    expect(text).toContain("owner-only");
    expect(text).toContain("icacls");
    expect(text).toContain("chmod 600");
    expect(text).toContain("mcphub repair-state-dacl --path");
    expect(text.toLowerCase()).not.toContain("remove the vault files");
    expect(text.toLowerCase()).not.toContain("destroys all stored secrets");
    // Confidentiality hedge (security-reviewer #468): the banner must NOT claim
    // unconditional "secrets are intact" — a co-resident with read on .age-key
    // could have decrypted the vault — so it must tell the operator to rotate.
    expect(text.toLowerCase()).toContain("rotat");
    expect(text.toLowerCase()).not.toContain("decrypt fine");
  });

  it("renders the backend-owned env.remediation verbatim (single-owner refactor, deep-review P3)", async () => {
    const accessDeniedVault: SecretsEnvelope = {
      vault_state: "access_denied",
      secrets: [],
      manifest_errors: [],
      remediation: daclBreadthRemediation,
    };
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/secrets": () => jsonResponse(200, accessDeniedVault),
      }) as unknown as typeof fetch,
    );

    render(<SecretsScreen />);
    const remediationEl = await screen.findByTestId("vault-access-denied-remediation");
    expect(remediationEl.textContent).toBe(daclBreadthRemediation);
  });

  it("falls back to a generic pointer when the backend omits remediation (older backend / defensive)", async () => {
    const accessDeniedVault: SecretsEnvelope = {
      vault_state: "access_denied",
      secrets: [],
      manifest_errors: [],
      // remediation intentionally omitted
    };
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/secrets": () => jsonResponse(200, accessDeniedVault),
      }) as unknown as typeof fetch,
    );

    render(<SecretsScreen />);
    const remediationEl = await screen.findByTestId("vault-access-denied-remediation");
    // Never renders blank; the fallback still points at the runbook.
    expect(remediationEl.textContent ?? "").not.toBe("");
    expect((remediationEl.textContent ?? "").toLowerCase()).toContain("runbook");
  });
});

// ── SEAM-D: Catalog "Open Secrets" deep-link (#/secrets?key=<key>) ──────────
// The Catalog readiness gate links each unset optional secret to
// #/secrets?key=<key>. SecretsScreen reads route.query and auto-opens the
// Add-secret modal pre-filled with that key.
describe("SecretsScreen — ?key= deep-link prefill (epic area 2)", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });
  afterEach(() => {
    cleanup();
  });

  const okEmptyVault: SecretsEnvelope = {
    vault_state: "ok",
    secrets: [],
    manifest_errors: [],
  };

  function route(query: string) {
    return { screen: "secrets", query };
  }

  it("auto-opens AddSecretModal pre-filled with the ?key= value (empty vault)", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/secrets": () => jsonResponse(200, okEmptyVault),
        // InitEmptyView fetches /api/status (running-daemon counts) on some paths;
        // tolerate it so the router guard never throws "unexpected fetch".
        "/api/status": () => jsonResponse(200, []),
      }) as unknown as typeof fetch,
    );

    render(<SecretsScreen route={route("key=WOLFRAM_APP_ID")} />);

    // The deep-link toggles the modal open with the key seeded as the name. The
    // modal remounts (keyed on the prefill) so the prefilled name lands
    // asynchronously — wait for the OPEN modal carrying the prefilled value.
    await waitFor(() => {
      const m = screen.getByTestId("add-secret-modal") as HTMLDialogElement;
      expect(m.open).toBe(true);
      const nameInput = m.querySelector('input[type="text"]') as HTMLInputElement;
      expect(nameInput.value).toBe("WOLFRAM_APP_ID");
      // The prefilled name is locked (the deep-link names the exact key).
      expect(nameInput.disabled).toBe(true);
    });
  });

  it("does NOT auto-open the modal when there is no ?key=", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/secrets": () => jsonResponse(200, okEmptyVault),
        "/api/status": () => jsonResponse(200, []),
      }) as unknown as typeof fetch,
    );

    render(<SecretsScreen route={route("")} />);
    // Wait for the empty-state to settle, then assert the modal is not open.
    await screen.findByText("No secrets yet.");
    const modal = screen.queryByTestId("add-secret-modal");
    // happy-dom: a closed <dialog> has no `open` attribute.
    expect(modal && (modal as HTMLDialogElement).open).toBeFalsy();
  });
});
