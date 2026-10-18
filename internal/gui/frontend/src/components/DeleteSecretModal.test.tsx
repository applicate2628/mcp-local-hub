import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/preact";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { DeleteSecretModal } from "./DeleteSecretModal";

beforeEach(() => {
  cleanup();
  vi.restoreAllMocks();
  HTMLDialogElement.prototype.showModal = function () { this.open = true; };
  HTMLDialogElement.prototype.close = function () { this.open = false; };
});

afterEach(() => cleanup());

describe("DeleteSecretModal — initial unreferenced-secret guard", () => {
  it("opens confirmation first and Cancel sends no DELETE", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(null, { status: 204 }),
    );
    const onClose = vi.fn();
    const onDeleted = vi.fn();

    render(
      <DeleteSecretModal
        name="UNREFERENCED_KEY"
        onClose={onClose}
        onDeleted={onDeleted}
      />,
    );

    const modal = await screen.findByTestId("delete-secret-initial-confirm");
    expect((modal as HTMLDialogElement).open).toBe(true);
    expect(fetchSpy).not.toHaveBeenCalled();

    fireEvent.click(screen.getByTestId("delete-secret-initial-confirm-cancel"));

    expect(fetchSpy).not.toHaveBeenCalled();
    expect(onDeleted).not.toHaveBeenCalled();
    expect(onClose).toHaveBeenCalledOnce();
  });

  it("Confirm sends exactly one unconfirmed classification DELETE", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(null, { status: 204 }),
    );
    const onClose = vi.fn();
    const onDeleted = vi.fn();

    render(
      <DeleteSecretModal
        name="UNREFERENCED_KEY"
        onClose={onClose}
        onDeleted={onDeleted}
      />,
    );

    fireEvent.click(await screen.findByTestId("delete-secret-initial-confirm-confirm"));

    await waitFor(() => expect(fetchSpy).toHaveBeenCalledOnce());
    expect(fetchSpy).toHaveBeenCalledWith(
      "/api/secrets/UNREFERENCED_KEY",
      { method: "DELETE" },
    );
    expect(fetchSpy.mock.calls.some(([url]) => String(url).includes("confirm=true"))).toBe(false);
    await waitFor(() => {
      expect(onDeleted).toHaveBeenCalledOnce();
      expect(onClose).toHaveBeenCalledOnce();
    });
  });
});
