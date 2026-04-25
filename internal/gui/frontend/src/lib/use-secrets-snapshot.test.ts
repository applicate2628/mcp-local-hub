import { describe, expect, it, beforeEach, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/preact";
import { useSecretsSnapshot } from "./use-secrets-snapshot";

const mockFetch = vi.fn();
beforeEach(() => {
  mockFetch.mockReset();
  globalThis.fetch = mockFetch as unknown as typeof fetch;
});

describe("useSecretsSnapshot", () => {
  it("transitions loading → ok on success", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ vault_state: "ok", secrets: [], manifest_errors: [] }),
    });

    const { result } = renderHook(() => useSecretsSnapshot());
    expect(result.current.status).toBe("loading");

    await waitFor(() => expect(result.current.status).toBe("ok"));
    expect(result.current.data?.vault_state).toBe("ok");
  });

  it("transitions to error on fetch failure", async () => {
    mockFetch.mockResolvedValue({
      ok: false,
      status: 500,
      json: async () => ({ error: "boom", code: "SECRETS_LIST_FAILED" }),
    });

    const { result } = renderHook(() => useSecretsSnapshot());
    await waitFor(() => expect(result.current.status).toBe("error"));
  });
});
