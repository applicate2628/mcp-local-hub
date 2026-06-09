import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor } from "@testing-library/preact";
import { DaemonEnvSettings } from "./DaemonEnvSettings";
import {
  listDaemonEnv,
  applyDaemonEnv,
  respawnDaemon,
  type DaemonEnvListResponse,
} from "../../api";

vi.mock("../../api", () => ({
  listDaemonEnv: vi.fn(),
  applyDaemonEnv: vi.fn(),
  respawnDaemon: vi.fn(),
}));

const firstResponse: DaemonEnvListResponse = {
  daemons: [
    {
      task_name: "\\mcp-local-hub-memory-default",
      server: "memory",
      daemon: "default",
      env: { MEMORY_FILE_PATH: "old.jsonl" },
    },
  ],
};

const refreshedResponse: DaemonEnvListResponse = {
  daemons: [
    {
      task_name: "\\mcp-local-hub-memory-default",
      server: "memory",
      daemon: "default",
      env: { MEMORY_FILE_PATH: "new.jsonl" },
    },
  ],
};

const emptyEnvResponse: DaemonEnvListResponse = {
  daemons: [
    {
      task_name: "\\mcp-local-hub-memory-default",
      server: "memory",
      daemon: "default",
      env: {},
    },
  ],
};

const twoDaemonResponse: DaemonEnvListResponse = {
  daemons: [
    {
      task_name: "\\mcp-local-hub-memory-default",
      server: "memory",
      daemon: "default",
      env: { MEMORY_FILE_PATH: "old.jsonl" },
    },
    {
      task_name: "\\mcp-local-hub-time-default",
      server: "time",
      daemon: "default",
      env: { TZ: "UTC" },
    },
  ],
};

// happy-dom does NOT implement <dialog>.showModal()/close() natively
// (they noop and never flip `open`), so the ConfirmModal's confirm/cancel
// buttons would never become visible to a `dialog[open]` query. Mirror the
// shim used by SectionMaintenance.test.tsx / SectionBackups.test.tsx so the
// `open` content-attribute is reflected and the active modal is selectable.
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

describe("DaemonEnvSettings", () => {
  beforeEach(() => {
    installDialogShim();
    vi.mocked(applyDaemonEnv).mockResolvedValue({
      task_name: "\\mcp-local-hub-memory-default",
      changed_keys: [],
    });
    vi.mocked(respawnDaemon).mockResolvedValue({
      task_name: "\\mcp-local-hub-memory-default",
      force: false,
      state: "queued",
    });
  });

  afterEach(() => {
    cleanup();
    vi.resetAllMocks();
  });

  it("refreshes the editor value when the selected daemon env changes", async () => {
    vi.mocked(listDaemonEnv)
      .mockResolvedValueOnce(firstResponse)
      .mockResolvedValueOnce(refreshedResponse);

    const { findByTestId } = render(<DaemonEnvSettings />);

    const value = (await findByTestId("daemon-env-value")) as HTMLInputElement;
    await waitFor(() => expect(value.value).toBe("old.jsonl"));

    fireEvent.click(await findByTestId("daemon-env-refresh"));

    await waitFor(() => expect(value.value).toBe("new.jsonl"));
  });

  it("keeps an unsaved value edit when refresh returns the same selected env", async () => {
    vi.mocked(listDaemonEnv)
      .mockResolvedValueOnce(firstResponse)
      .mockResolvedValueOnce({
        daemons: [
          {
            task_name: "\\mcp-local-hub-memory-default",
            server: "memory",
            daemon: "default",
            env: { MEMORY_FILE_PATH: "old.jsonl" },
          },
        ],
      });

    const { findByTestId } = render(<DaemonEnvSettings />);

    const value = (await findByTestId("daemon-env-value")) as HTMLInputElement;
    await waitFor(() => expect(value.value).toBe("old.jsonl"));

    fireEvent.input(value, { target: { value: "draft.jsonl" } });
    expect(value.value).toBe("draft.jsonl");

    fireEvent.click(await findByTestId("daemon-env-refresh"));

    await waitFor(() => expect(listDaemonEnv).toHaveBeenCalledTimes(2));
    expect(value.value).toBe("draft.jsonl");
  });

  it("does not mark the initial empty env draft dirty until the user edits it", async () => {
    vi.mocked(listDaemonEnv).mockResolvedValueOnce(emptyEnvResponse);
    const onDirty = vi.fn();

    const { findByTestId } = render(<DaemonEnvSettings onDirtyChange={onDirty} />);

    const task = (await findByTestId("daemon-env-task")) as HTMLSelectElement;
    await waitFor(() => expect(task.value).toBe("\\mcp-local-hub-memory-default"));
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(onDirty).not.toHaveBeenCalledWith(true);
    expect(onDirty).toHaveBeenLastCalledWith(false);

    const value = (await findByTestId("daemon-env-value")) as HTMLInputElement;
    fireEvent.input(value, { target: { value: "draft.jsonl" } });

    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(true));
  });

  it("disables Apply on the initial empty draft and does not write a placeholder override", async () => {
    vi.mocked(listDaemonEnv).mockResolvedValueOnce(emptyEnvResponse);

    const { findByTestId } = render(<DaemonEnvSettings />);

    const task = (await findByTestId("daemon-env-task")) as HTMLSelectElement;
    await waitFor(() => expect(task.value).toBe("\\mcp-local-hub-memory-default"));
    await new Promise((resolve) => setTimeout(resolve, 20));

    const applyBtn = (await findByTestId("daemon-env-apply")) as HTMLButtonElement;
    expect(applyBtn.disabled).toBe(true);
    fireEvent.click(applyBtn);
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(applyDaemonEnv).not.toHaveBeenCalled();

    const value = (await findByTestId("daemon-env-value")) as HTMLInputElement;
    fireEvent.input(value, { target: { value: "D:\\memory\\memory.jsonl" } });
    await waitFor(() => expect(applyBtn.disabled).toBe(false));
  });

  it("gates a daemon switch with a ConfirmModal while the env edit is dirty (#268 r11 P2)", async () => {
    vi.mocked(listDaemonEnv).mockResolvedValue(twoDaemonResponse);
    const onDirty = vi.fn();

    const { findByTestId, container } = render(
      <DaemonEnvSettings onDirtyChange={onDirty} />,
    );

    const task = (await findByTestId("daemon-env-task")) as HTMLSelectElement;
    await waitFor(() => expect(task.value).toBe("\\mcp-local-hub-memory-default"));

    const value = (await findByTestId("daemon-env-value")) as HTMLInputElement;
    await waitFor(() => expect(value.value).toBe("old.jsonl"));

    // Edit the value so the row is dirty.
    fireEvent.input(value, { target: { value: "draft.jsonl" } });
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(true));

    // Attempt to switch to the second daemon → the switch must be intercepted
    // by a ConfirmModal; selection stays on the current daemon and the parent
    // dirty guard is NOT cleared.
    fireEvent.change(task, { target: { value: "\\mcp-local-hub-time-default" } });

    const modal = container.ownerDocument!.querySelector(
      'dialog[data-testid="confirm-modal"][open]',
    );
    expect(modal).not.toBeNull();
    expect(task.value).toBe("\\mcp-local-hub-memory-default");
    expect(value.value).toBe("draft.jsonl");
    expect(onDirty).not.toHaveBeenLastCalledWith(false);

    // Cancel keeps the current daemon + edit intact.
    fireEvent.click(
      modal!.querySelector(
        '[data-testid="confirm-modal-cancel"]',
      ) as HTMLButtonElement,
    );
    await waitFor(() =>
      expect(
        container.ownerDocument!.querySelector(
          'dialog[data-testid="confirm-modal"][open]',
        ),
      ).toBeNull(),
    );
    expect(task.value).toBe("\\mcp-local-hub-memory-default");
    expect(value.value).toBe("draft.jsonl");
    expect(onDirty).not.toHaveBeenLastCalledWith(false);

    // Re-attempt the switch and confirm → the switch performs, loading the
    // second daemon's env, and the dirty guard clears.
    fireEvent.change(task, { target: { value: "\\mcp-local-hub-time-default" } });
    const modal2 = container.ownerDocument!.querySelector(
      'dialog[data-testid="confirm-modal"][open]',
    );
    expect(modal2).not.toBeNull();
    fireEvent.click(
      modal2!.querySelector(
        '[data-testid="confirm-modal-confirm"]',
      ) as HTMLButtonElement,
    );

    await waitFor(() => expect(task.value).toBe("\\mcp-local-hub-time-default"));
    await waitFor(() => expect(value.value).toBe("UTC"));
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(false));
  });

  it("surfaces a post-apply refresh failure instead of a bare Saved (#268 r11 P3)", async () => {
    // First mount load succeeds; the post-apply refresh rejects.
    vi.mocked(listDaemonEnv)
      .mockResolvedValueOnce(firstResponse)
      .mockRejectedValueOnce(new Error("backend down"));
    vi.mocked(applyDaemonEnv).mockResolvedValue({
      task_name: "\\mcp-local-hub-memory-default",
      changed_keys: ["MEMORY_FILE_PATH"],
    });
    const onDirty = vi.fn();

    const { findByTestId } = render(<DaemonEnvSettings onDirtyChange={onDirty} />);

    const value = (await findByTestId("daemon-env-value")) as HTMLInputElement;
    await waitFor(() => expect(value.value).toBe("old.jsonl"));

    // Edit so Apply is enabled, then apply.
    fireEvent.input(value, { target: { value: "draft.jsonl" } });
    const applyBtn = (await findByTestId("daemon-env-apply")) as HTMLButtonElement;
    await waitFor(() => expect(applyBtn.disabled).toBe(false));
    fireEvent.click(applyBtn);

    // The POST resolved but the refresh rejected → banner reflects the
    // refresh failure, not a bare success.
    const banner = (await findByTestId("daemon-env-banner")) as HTMLElement;
    await waitFor(() =>
      expect(banner.textContent).toContain("could not refresh the list"),
    );
    expect(banner.textContent).toContain("backend down");
    expect(banner.className).toContain("error");
    expect(banner.textContent).not.toBe(
      "Saved. Restart the daemon for the change to take effect.",
    );

    // The draft is still in place (no successful reload), so the panel is
    // still dirty — it did not falsely clear.
    expect(value.value).toBe("draft.jsonl");
    expect(onDirty).toHaveBeenLastCalledWith(true);
  });
});
