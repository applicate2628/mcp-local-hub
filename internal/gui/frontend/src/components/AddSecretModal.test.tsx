import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, cleanup, waitFor } from "@testing-library/preact";
import userEvent from "@testing-library/user-event";
import { AddSecretModal } from "./AddSecretModal";

const mockFetch = vi.fn();
beforeEach(() => {
  cleanup();
  mockFetch.mockReset();
  globalThis.fetch = mockFetch as unknown as typeof fetch;
});

describe("AddSecretModal — reserved-name client validation", () => {
  it("disables Save and shows inline error when user types reserved name 'init'", async () => {
    const user = userEvent.setup();
    const onClose = vi.fn();
    const onSaved = vi.fn();
    render(<AddSecretModal open={true} onClose={onClose} onSaved={onSaved} />);

    const nameInput = screen.getByLabelText(/Name/i);
    const valueInput = screen.getByLabelText(/Value/i);
    await user.type(nameInput, "init");
    await user.type(valueInput, "any-value");

    expect(screen.getByText(/'init' is a reserved name/i)).toBeTruthy();
    const saveButton = screen.getByRole("button", { name: /Save/i });
    expect((saveButton as HTMLButtonElement).disabled).toBe(true);
  });

  it("does not show reserved-name error for arbitrary names", async () => {
    const user = userEvent.setup();
    render(<AddSecretModal open={true} onClose={vi.fn()} onSaved={vi.fn()} />);

    const nameInput = screen.getByLabelText(/Name/i);
    await user.type(nameInput, "openai_api_key");
    expect(screen.queryByText(/'init' is a reserved name/i)).toBeNull();
    expect(screen.queryByText(/reserved name/i)).toBeNull();
  });

  it("does not allow reserved name even when modal is opened with prefillName='init'", async () => {
    render(<AddSecretModal open={true} prefillName="init" onClose={vi.fn()} onSaved={vi.fn()} />);
    expect(screen.getByText(/'init' is a reserved name/i)).toBeTruthy();
    const saveButton = screen.getByRole("button", { name: /Save/i });
    expect((saveButton as HTMLButtonElement).disabled).toBe(true);
  });
});

describe("AddSecretModal — async onSaved + single close path (A3-b §2.2)", () => {
  it("awaits onSaved before closing", async () => {
    const user = userEvent.setup();
    mockFetch.mockResolvedValue({ ok: true, status: 201 });
    let savedResolved = false;
    const onSaved = vi.fn(async () => {
      // Simulate a slow snapshot.refresh()
      await new Promise((r) => setTimeout(r, 50));
      savedResolved = true;
    });
    const onClose = vi.fn();
    render(<AddSecretModal open={true} onClose={onClose} onSaved={onSaved} />);

    await user.type(screen.getByLabelText(/Name/i), "x");
    await user.type(screen.getByLabelText(/Value/i), "y");
    await user.click(screen.getByRole("button", { name: /Save/i }));

    // Wait for onClose to fire (which only happens AFTER onSaved resolves
    // AND the dialog's native close event propagates).
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    // savedResolved must be true by the time onClose was called.
    expect(savedResolved).toBe(true);
  });

  it("calls onClose exactly once per save-success lifecycle (no double-firing)", async () => {
    const user = userEvent.setup();
    mockFetch.mockResolvedValue({ ok: true, status: 201 });
    const onClose = vi.fn();
    const onSaved = vi.fn();
    render(<AddSecretModal open={true} onClose={onClose} onSaved={onSaved} />);

    await user.type(screen.getByLabelText(/Name/i), "x");
    await user.type(screen.getByLabelText(/Value/i), "y");
    await user.click(screen.getByRole("button", { name: /Save/i }));

    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(onSaved).toHaveBeenCalledTimes(1);
  });

  it("calls onClose exactly once on Cancel click", async () => {
    const user = userEvent.setup();
    const onClose = vi.fn();
    render(<AddSecretModal open={true} onClose={onClose} onSaved={vi.fn()} />);

    await user.click(screen.getByRole("button", { name: /Cancel/i }));
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("renders 'Manage all secrets' footer link that opens #/secrets in a new tab (Codex memo D4 + plan-R1 P1-2)", async () => {
    const user = userEvent.setup();
    const openSpy = vi.spyOn(window, "open").mockImplementation(() => null);
    render(<AddSecretModal open={true} onClose={vi.fn()} onSaved={vi.fn()} />);

    const link = screen.getByRole("link", { name: /Manage all secrets/i }) as HTMLAnchorElement;
    expect(link).toBeTruthy();
    await user.click(link);
    expect(openSpy).toHaveBeenCalledWith("#/secrets", "_blank");
    openSpy.mockRestore();
  });

  it("does NOT close the modal when backend POST fails (server error)", async () => {
    const user = userEvent.setup();
    mockFetch.mockResolvedValue({
      ok: false,
      status: 500,
      json: async () => ({ error: "boom", code: "SECRETS_ADD_FAILED" }),
    });
    const onClose = vi.fn();
    const onSaved = vi.fn();
    render(<AddSecretModal open={true} onClose={onClose} onSaved={onSaved} />);

    await user.type(screen.getByLabelText(/Name/i), "x");
    await user.type(screen.getByLabelText(/Value/i), "y");
    await user.click(screen.getByRole("button", { name: /Save/i }));

    // Inline error visible
    await waitFor(() => expect(screen.queryByText(/boom/i)).toBeTruthy());
    // Modal remained open: onClose not yet called, onSaved not called
    expect(onClose).not.toHaveBeenCalled();
    expect(onSaved).not.toHaveBeenCalled();
  });
});
