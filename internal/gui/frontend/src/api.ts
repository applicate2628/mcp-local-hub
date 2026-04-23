// fetchOrThrow is the shared API wrapper mirroring the legacy fetchOrThrow
// from servers.js. Backend handlers surface errors via the {error, code}
// JSON envelope (writeAPIError in the Go side) — not the success shape the
// UI expects. Without the response-shape guard, callers that iterate the
// parsed body would treat the truthy envelope object as iterable and throw
// inside render logic, leaving the screen blank. Require resp.ok AND the
// declared top-level shape before trusting the payload.
export async function fetchOrThrow<T>(
  path: string,
  expect: "array" | "object",
  init?: RequestInit,
): Promise<T> {
  const resp = await fetch(path, init);
  let data: unknown = null;
  try {
    data = await resp.json();
  } catch {
    // Non-JSON body left as null; handled below.
  }
  if (!resp.ok) {
    const msg = (data as { error?: string } | null)?.error ?? resp.statusText ?? "unknown";
    throw new Error(`${path}: ${msg}`);
  }
  if (expect === "array" && !Array.isArray(data)) {
    throw new Error(`${path}: expected array, got ${typeof data}`);
  }
  if (
    expect === "object" &&
    (data === null || typeof data !== "object" || Array.isArray(data))
  ) {
    throw new Error(
      `${path}: expected object, got ${Array.isArray(data) ? "array" : typeof data}`,
    );
  }
  return data as T;
}
