// isHubLoopback reports whether an http endpoint URL targets the local hub.
// MUST parse the URL and compare hostname — a substring test like
// endpoint.includes("127.0.0.1") misclassifies URLs that merely contain the
// loopback string as a DNS label or path/query component. Such a
// misclassification would let Apply rewrite a binding based on the wrong
// routing assumption. Unparseable endpoints (stdio:, relative paths, empty)
// fall to not-loopback.
export function isHubLoopback(endpoint: string): boolean {
  if (!endpoint) return false;
  try {
    const u = new URL(endpoint);
    return u.hostname === "127.0.0.1" || u.hostname === "localhost" || u.hostname === "[::1]";
  } catch {
    return false;
  }
}
