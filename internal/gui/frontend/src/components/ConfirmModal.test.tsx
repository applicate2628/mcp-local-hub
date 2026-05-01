import { render, screen, fireEvent, waitFor, cleanup } from "@testing-library/preact";
import { describe, expect, it, vi, beforeEach } from "vitest";
import { ConfirmModal } from "./ConfirmModal";

beforeEach(() => {
  cleanup();
  HTMLDialogElement.prototype.showModal = function () { this.open = true; };
  HTMLDialogElement.prototype.close = function () { this.open = false; };
});

describe("ConfirmModal", () => {
  it("renders title + body + confirm label", () => {
    render(
      <ConfirmModal
        open
        title="Delete eligible backups?"
        body={<>Delete <b>3</b> backups across <b>2</b> client(s).</>}
        confirmLabel="Delete"
        onConfirm={vi.fn()}
        onCancel={vi.fn()}
      />,
    );
    expect(screen.getByText("Delete eligible backups?")).toBeTruthy();
    expect(screen.getByText("Delete")).toBeTruthy();
    expect(screen.getByText("3")).toBeTruthy();
  });

  it("calls onConfirm when confirm button clicked", async () => {
    const onConfirm = vi.fn();
    render(
      <ConfirmModal
        open
        title="X"
        body={<>Y</>}
        confirmLabel="Yes"
        onConfirm={onConfirm}
        onCancel={vi.fn()}
      />,
    );
    fireEvent.click(screen.getByText("Yes"));
    await waitFor(() => expect(onConfirm).toHaveBeenCalledOnce());
  });

  it("calls onCancel when cancel button clicked", () => {
    const onCancel = vi.fn();
    render(
      <ConfirmModal
        open
        title="X"
        body={<>Y</>}
        confirmLabel="OK"
        onConfirm={vi.fn()}
        onCancel={onCancel}
      />,
    );
    fireEvent.click(screen.getByText("Cancel"));
    expect(onCancel).toHaveBeenCalledOnce();
  });

  it("applies danger class when danger=true", () => {
    const { container } = render(
      <ConfirmModal
        open
        title="X"
        body={<>Y</>}
        confirmLabel="Delete"
        danger
        onConfirm={vi.fn()}
        onCancel={vi.fn()}
      />,
    );
    const confirm = container.querySelector('button[data-testid="confirm-modal-confirm"]');
    expect(confirm?.className).toContain("danger");
  });

  it("disables confirm button while busy", async () => {
    const slow = vi.fn().mockImplementation(() => new Promise<void>((r) => setTimeout(r, 100)));
    const { container } = render(
      <ConfirmModal
        open
        title="X"
        body={<>Y</>}
        confirmLabel="Go"
        onConfirm={slow}
        onCancel={vi.fn()}
      />,
    );
    fireEvent.click(screen.getByText("Go"));
    await waitFor(() => {
      const confirm = container.querySelector('button[data-testid="confirm-modal-confirm"]');
      expect((confirm as HTMLButtonElement).disabled).toBe(true);
    });
  });

  it("does not render the dialog when open=false", () => {
    render(
      <ConfirmModal
        open={false}
        title="X"
        body={<>Y</>}
        confirmLabel="OK"
        onConfirm={vi.fn()}
        onCancel={vi.fn()}
      />,
    );
    const dialog = document.querySelector("dialog");
    expect(dialog?.open).toBeFalsy();
  });
});
