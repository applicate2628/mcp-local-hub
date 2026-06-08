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

describe("DaemonEnvSettings", () => {
  beforeEach(() => {
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
});
