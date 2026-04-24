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

// postDismiss sends the Migration screen's Unknown-group Dismiss action
// to the hub. Backend persistence lives in Task 2; GET /api/dismissed
// in Task 3. This
// is a thin wrapper so the screen code does not repeat fetch plumbing.
// Throws on non-204 responses with a descriptive message including the
// backend-provided error field when present.
export async function postDismiss(server: string): Promise<void> {
  const resp = await fetch("/api/dismiss", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ server }),
  });
  if (resp.status === 204) return;
  let body: { error?: string } | null = null;
  try {
    body = (await resp.json()) as { error?: string };
  } catch {
    // Non-JSON error body; fall through.
  }
  throw new Error(`/api/dismiss: ${body?.error ?? resp.statusText}`);
}

// postManifestCreate writes a new manifest via the A2a GUI pipeline. On
// success the backend returns 204; any non-2xx is surfaced as a thrown
// Error carrying the backend's {error} envelope text when present. Callers
// handle the "already exists" case by inspecting the error message — the
// backend currently returns "manifest \"<name>\" already exists at ..."
// verbatim, which is user-friendly enough to show in a banner.
export async function postManifestCreate(name: string, yaml: string): Promise<void> {
  const resp = await fetch("/api/manifest/create", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name, yaml }),
  });
  if (resp.status === 204) return;
  let body: { error?: string } | null = null;
  try {
    body = (await resp.json()) as { error?: string };
  } catch {
    // Non-JSON error body; fall through.
  }
  throw new Error(`/api/manifest/create: ${body?.error ?? resp.statusText}`);
}

// postManifestValidate returns the list of structural warnings produced by
// api.ManifestValidate. Empty array == valid. Throws on transport/HTTP error
// (not on validation warnings — those are normal return values).
export async function postManifestValidate(yaml: string): Promise<string[]> {
  const resp = await fetch("/api/manifest/validate", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ yaml }),
  });
  if (!resp.ok) {
    let body: { error?: string } | null = null;
    try {
      body = (await resp.json()) as { error?: string };
    } catch {
      // Non-JSON error body; fall through.
    }
    throw new Error(`/api/manifest/validate: ${body?.error ?? resp.statusText}`);
  }
  const payload = (await resp.json()) as { warnings?: string[] };
  return payload.warnings ?? [];
}

// getExtractManifest fetches the prefill YAML that populates AddServer's
// form when the user arrives via the A1 Migration Create-manifest button.
// Returns the raw YAML string. Throws on non-2xx with the backend error.
export async function getExtractManifest(client: string, server: string): Promise<string> {
  const url = `/api/extract-manifest?client=${encodeURIComponent(client)}&server=${encodeURIComponent(server)}`;
  const resp = await fetch(url);
  if (!resp.ok) {
    let body: { error?: string } | null = null;
    try {
      body = (await resp.json()) as { error?: string };
    } catch {
      // Non-JSON error body; fall through.
    }
    throw new Error(`/api/extract-manifest: ${body?.error ?? resp.statusText}`);
  }
  const payload = (await resp.json()) as { yaml?: string };
  return payload.yaml ?? "";
}
