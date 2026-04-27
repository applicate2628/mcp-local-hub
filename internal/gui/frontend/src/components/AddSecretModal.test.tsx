import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/preact";
import userEvent from "@testing-library/user-event";
import { AddSecretModal } from "./AddSecretModal";

const mockFetch = vi.fn();
beforeEach(() => {
  cleanup();
  mockFetch.mockReset();
  globalThis.fetch = mockFetch as unknown as typeof fetch;
  // jsdom's <dialog> support requires this stub before tests run
  if (!HTMLDialogElement.prototype.showModal) {
    HTMLDialogElement.prototype.showModal = function () {
      this.setAttribute("open", "");
    };
    HTMLDialogElement.prototype.close = function () {
      this.removeAttribute("open");
      this.dispatchEvent(new Event("close"));
    };
  }
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
