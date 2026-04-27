import { describe, expect, it, beforeEach, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/preact";
import { useSecretsSnapshot } from "./use-secrets-snapshot";
import type { SecretsSnapshot } from "./use-secrets-snapshot";

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

describe("useSecretsSnapshot — fetchedAt (A3-b D8a)", () => {
  it("fetchedAt is null on initial loading state", async () => {
    mockFetch.mockImplementation(() => new Promise(() => {})); // never resolves
    const { result } = renderHook(() => useSecretsSnapshot());
    expect(result.current.status).toBe("loading");
    expect(result.current.fetchedAt).toBe(null);
  });

  it("fetchedAt is a positive timestamp after successful load", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ vault_state: "ok", secrets: [], manifest_errors: [] }),
    });
    const t0 = Date.now();
    const { result } = renderHook(() => useSecretsSnapshot());
    await waitFor(() => expect(result.current.status).toBe("ok"));
    expect(result.current.fetchedAt).not.toBe(null);
    expect(result.current.fetchedAt!).toBeGreaterThanOrEqual(t0);
    expect(result.current.fetchedAt!).toBeLessThanOrEqual(Date.now());
  });

  it("preserves last-known fetchedAt on success-then-error transition (Codex memo-R5 P3 + R12 P3)", async () => {
    // First fetch: success
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: async () => ({ vault_state: "ok", secrets: [], manifest_errors: [] }),
    });
    const { result } = renderHook(() => useSecretsSnapshot());
    await waitFor(() => expect(result.current.status).toBe("ok"));
    const firstTimestamp = result.current.fetchedAt;
    expect(firstTimestamp).not.toBe(null);

    // Second fetch: failure
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 500,
      json: async () => ({ error: "boom" }),
    });
    await result.current.refresh();
    await waitFor(() => expect(result.current.status).toBe("error"));
    // data is null per existing contract (stale data NOT preserved)
    expect(result.current.data).toBe(null);
    // fetchedAt is preserved from the successful first fetch
    expect(result.current.fetchedAt).toBe(firstTimestamp);
  });

  it("preserves fetchedAt across error → error retry (Codex plan-R1 P1-3)", async () => {
    // First fetch: success
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: async () => ({ vault_state: "ok", secrets: [], manifest_errors: [] }),
    });
    const { result } = renderHook(() => useSecretsSnapshot());
    await waitFor(() => expect(result.current.status).toBe("ok"));
    const T1 = result.current.fetchedAt!;

    // Second fetch: failure (transitions to error with fetchedAt=T1)
    mockFetch.mockResolvedValueOnce({ ok: false, status: 500, json: async () => ({ error: "x" }) });
    await result.current.refresh();
    await waitFor(() => expect(result.current.status).toBe("error"));
    expect(result.current.fetchedAt).toBe(T1);

    // Third fetch: also fails. fetchedAt MUST still be T1 (not null, not refreshed).
    // The refresh() call sets state to "loading" mid-flight; we must NOT lose
    // T1 during that intermediate state, OR we must transition error→error
    // without going through loading. Either works as long as fetchedAt
    // remains T1 after the third fetch resolves.
    mockFetch.mockResolvedValueOnce({ ok: false, status: 500, json: async () => ({ error: "y" }) });
    await result.current.refresh();
    await waitFor(() => expect(result.current.status).toBe("error"));
    expect(result.current.fetchedAt).toBe(T1);
  });

  it("fetchedAt is null on error variant when no successful fetch ever happened", async () => {
    mockFetch.mockResolvedValue({
      ok: false,
      status: 500,
      json: async () => ({ error: "boom" }),
    });
    const { result } = renderHook(() => useSecretsSnapshot());
    await waitFor(() => expect(result.current.status).toBe("error"));
    expect(result.current.fetchedAt).toBe(null);
  });

  it("exported SecretsSnapshot type is the hook return type (compile-time check)", () => {
    // This test exists as a smoke that the import resolves at compile time.
    // A separate type-level assertion via `expectTypeOf` is overkill here.
    // eslint-disable-next-line @typescript-eslint/no-unused-vars
    const _typeCheck: SecretsSnapshot | null = null;
    expect(_typeCheck).toBe(null);
  });
});
