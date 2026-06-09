// SectionTrustedRoots component tests — render list / empty-state, add,
// remove (via the ConfirmModal gate), and inline relative-path
// validation. Modeled on SectionSecrets/SectionMaintenance test style:
// the api.ts helpers are module-mocked, and the ConfirmModal needs the
// happy-dom <dialog> shim because happy-dom does not implement
// showModal()/close() natively.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor } from "@testing-library/preact";
import { SectionTrustedRoots } from "./SectionTrustedRoots";
import {
  getTrustedRoots,
  addTrustedRoot,
  removeTrustedRoot,
  type TrustedRootsResponse,
} from "../../api";

vi.mock("../../api", () => ({
  getTrustedRoots: vi.fn(),
  addTrustedRoot: vi.fn(),
  removeTrustedRoot: vi.fn(),
}));

// happy-dom does NOT implement <dialog>.showModal()/close() — they noop
// and never flip `open`. ConfirmModal drives its visibility through them,
// so without this shim the confirm/cancel buttons never become visible.
// Mirror SectionMaintenance.test.tsx.
function installDialogShim() {
  HTMLDialogElement.prototype.showModal = function () {
    this.open = true;
    this.setAttribute("open", "");
  };
  HTMLDialogElement.prototype.close = function () {
    this.open = false;
    this.removeAttribute("open");
  };
}

const emptyResp: TrustedRootsResponse = { roots: [], path: "C:\\state\\lsp-trusted-roots.json" };
const oneRootResp: TrustedRootsResponse = {
  roots: ["C:\\dev\\proj"],
  path: "C:\\state\\lsp-trusted-roots.json",
};
const twoRootResp: TrustedRootsResponse = {
  roots: ["C:\\dev\\proj", "C:\\dev\\other"],
  path: "C:\\state\\lsp-trusted-roots.json",
};

describe("SectionTrustedRoots", () => {
  beforeEach(() => {
    cleanup();
    document.body.innerHTML = "";
    // clearAllMocks resets call history on the module-level vi.mock fns
    // (restoreAllMocks only undoes vi.spyOn spies, so call counts would
    // otherwise leak across tests in this file).
    vi.clearAllMocks();
    installDialogShim();
  });
  afterEach(() => cleanup());

  it("renders the security note and the empty-state on a clean store", async () => {
    vi.mocked(getTrustedRoots).mockResolvedValue(emptyResp);
    const { findByTestId, getByText } = render(<SectionTrustedRoots />);
    expect(await findByTestId("trusted-roots-empty")).toBeTruthy();
    // The security note copy from the prompt.
    expect(getByText(/Add only roots you control/)).toBeTruthy();
    // The store path is surfaced for hand-inspection.
    expect(getByText(/lsp-trusted-roots\.json/)).toBeTruthy();
  });

  it("renders the list of trusted roots with a Remove button each", async () => {
    vi.mocked(getTrustedRoots).mockResolvedValue(twoRootResp);
    const { findByTestId, container } = render(<SectionTrustedRoots />);
    await findByTestId("trusted-roots-list");
    const removeButtons = container.querySelectorAll('[data-testid="trusted-roots-remove"]');
    expect(removeButtons).toHaveLength(2);
    expect(container.textContent).toContain("C:\\dev\\proj");
    expect(container.textContent).toContain("C:\\dev\\other");
  });

  it("adds a root and re-renders from the returned list", async () => {
    vi.mocked(getTrustedRoots).mockResolvedValue(emptyResp);
    vi.mocked(addTrustedRoot).mockResolvedValue(oneRootResp);
    const { findByTestId, getByTestId } = render(<SectionTrustedRoots />);
    await findByTestId("trusted-roots-empty");

    const input = getByTestId("trusted-roots-input") as HTMLInputElement;
    fireEvent.input(input, { target: { value: "C:\\dev\\proj" } });
    fireEvent.click(getByTestId("trusted-roots-add-button"));

    await waitFor(() => expect(addTrustedRoot).toHaveBeenCalledWith("C:\\dev\\proj"));
    // The list now reflects the response from addTrustedRoot.
    await findByTestId("trusted-roots-list");
  });

  it("disables Add and shows an inline error for a relative path", async () => {
    vi.mocked(getTrustedRoots).mockResolvedValue(emptyResp);
    const { findByTestId, getByTestId } = render(<SectionTrustedRoots />);
    await findByTestId("trusted-roots-empty");

    const input = getByTestId("trusted-roots-input") as HTMLInputElement;
    fireEvent.input(input, { target: { value: "relative/path" } });

    // Inline validation error appears; Add stays disabled; no network call.
    expect(await findByTestId("trusted-roots-relative-error")).toBeTruthy();
    const addBtn = getByTestId("trusted-roots-add-button") as HTMLButtonElement;
    expect(addBtn.disabled).toBe(true);
    expect(addTrustedRoot).not.toHaveBeenCalled();
  });

  it("does not call addTrustedRoot for an empty input", async () => {
    vi.mocked(getTrustedRoots).mockResolvedValue(emptyResp);
    const { findByTestId, getByTestId } = render(<SectionTrustedRoots />);
    await findByTestId("trusted-roots-empty");

    const addBtn = getByTestId("trusted-roots-add-button") as HTMLButtonElement;
    expect(addBtn.disabled).toBe(true);
    fireEvent.click(addBtn);
    expect(addTrustedRoot).not.toHaveBeenCalled();
  });

  it("removes a root only after confirming in the ConfirmModal", async () => {
    vi.mocked(getTrustedRoots).mockResolvedValue(oneRootResp);
    vi.mocked(removeTrustedRoot).mockResolvedValue(emptyResp);
    const { findByTestId, getByTestId } = render(<SectionTrustedRoots />);
    await findByTestId("trusted-roots-list");

    // Click Remove → opens the confirm modal; no removal yet.
    fireEvent.click(getByTestId("trusted-roots-remove"));
    expect(removeTrustedRoot).not.toHaveBeenCalled();

    // Confirm inside the modal → removeTrustedRoot fires with the root.
    const confirmBtn = await findByTestId("confirm-modal-confirm");
    fireEvent.click(confirmBtn);
    await waitFor(() => expect(removeTrustedRoot).toHaveBeenCalledWith("C:\\dev\\proj"));
    // After removal the list reflects the empty response.
    await findByTestId("trusted-roots-empty");
  });

  it("cancelling the remove modal does not remove the root", async () => {
    vi.mocked(getTrustedRoots).mockResolvedValue(oneRootResp);
    const { findByTestId, getByTestId } = render(<SectionTrustedRoots />);
    await findByTestId("trusted-roots-list");

    fireEvent.click(getByTestId("trusted-roots-remove"));
    const cancelBtn = await findByTestId("confirm-modal-cancel");
    fireEvent.click(cancelBtn);
    expect(removeTrustedRoot).not.toHaveBeenCalled();
    // The root is still listed.
    expect(getByTestId("trusted-roots-list")).toBeTruthy();
  });

  it("renders a load-error with Retry when getTrustedRoots rejects", async () => {
    vi.mocked(getTrustedRoots).mockRejectedValueOnce(new Error("boom"));
    const { findByTestId, getByText } = render(<SectionTrustedRoots />);
    expect(await findByTestId("trusted-roots-load-error")).toBeTruthy();

    // Retry re-fetches; second call succeeds and the empty-state renders.
    vi.mocked(getTrustedRoots).mockResolvedValueOnce(emptyResp);
    fireEvent.click(getByText("Retry"));
    await findByTestId("trusted-roots-empty");
  });

  it("surfaces an action error when addTrustedRoot rejects", async () => {
    vi.mocked(getTrustedRoots).mockResolvedValue(emptyResp);
    vi.mocked(addTrustedRoot).mockRejectedValueOnce(
      new Error("/api/lsp/trusted-roots [LSP_TRUSTED_ROOTS_NOT_ABSOLUTE]: nope"),
    );
    const { findByTestId, getByTestId } = render(<SectionTrustedRoots />);
    await findByTestId("trusted-roots-empty");

    const input = getByTestId("trusted-roots-input") as HTMLInputElement;
    fireEvent.input(input, { target: { value: "C:\\dev\\proj" } });
    fireEvent.click(getByTestId("trusted-roots-add-button"));

    expect(await findByTestId("trusted-roots-action-error")).toBeTruthy();
  });
});
