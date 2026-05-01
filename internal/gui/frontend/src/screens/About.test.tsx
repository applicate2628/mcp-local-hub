import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, waitFor, cleanup } from "@testing-library/preact";
import { AboutScreen } from "./About";

const fakeVersion = {
  version: "0.99.test",
  commit: "abcdef0",
  build_date: "2026-04-28T12:00:00Z",
  go_version: "go1.26.2",
  platform: "windows/amd64",
  homepage: "https://github.com/applicate2628/mcp-local-hub",
  issues: "https://github.com/applicate2628/mcp-local-hub/issues",
  license: "Apache-2.0",
  author: "Dmitry Denisenko (@applicate2628)",
};

describe("AboutScreen", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });
  afterEach(() => cleanup());

  it("renders loading state initially", () => {
    // Pending fetch — never resolves during this assertion window.
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockImplementation(
      () => new Promise(() => {}),
    );
    const { findByTestId } = render(<AboutScreen />);
    expect(fetchSpy).toHaveBeenCalled();
    expect((fetchSpy.mock.calls[0]?.[0] as string)).toBe("/api/version");
    return findByTestId("about-loading");
  });

  it("renders version info on success", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify(fakeVersion), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
    const { findByTestId } = render(<AboutScreen />);
    const version = await findByTestId("about-version");
    expect(version.textContent).toBe("0.99.test");
    const commit = await findByTestId("about-commit");
    expect(commit.textContent).toBe("abcdef0");
    const date = await findByTestId("about-build-date");
    expect(date.textContent).toBe("2026-04-28T12:00:00Z");
  });

  it("renders error state when /api/version fails", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ error: "boom" }), {
        status: 500,
        headers: { "Content-Type": "application/json" },
      }),
    );
    const { findByTestId } = render(<AboutScreen />);
    const errBox = await findByTestId("about-error");
    expect(errBox.textContent).toMatch(/boom/);
  });

  it("renders documentation links derived from the homepage URL (Round 1 #7)", async () => {
    // Audit gap: spec calls for README / INSTALL / verification doc
    // links on the About screen so users can find canonical docs without
    // leaving the GUI. The links must be derived from the homepage URL
    // (not hardcoded) so a future repo rename automatically follows.
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify(fakeVersion), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
    const { findByTestId } = render(<AboutScreen />);
    const readme = await findByTestId("about-readme-link");
    expect(readme.getAttribute("href")).toBe(
      "https://github.com/applicate2628/mcp-local-hub/blob/master/README.md",
    );
    const install = await findByTestId("about-install-link");
    expect(install.getAttribute("href")).toBe(
      "https://github.com/applicate2628/mcp-local-hub/blob/master/INSTALL.md",
    );
    const verification = await findByTestId("about-verification-link");
    expect(verification.getAttribute("href")).toBe(
      "https://github.com/applicate2628/mcp-local-hub/blob/master/docs/phase-3b-ii-verification.md",
    );
  });

  it("links open in new tab with rel=noopener noreferrer", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify(fakeVersion), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
    const { findAllByRole } = render(<AboutScreen />);
    await waitFor(async () => {
      const links = await findAllByRole("link");
      expect(links.length).toBeGreaterThanOrEqual(2);
    });
    const links = await findAllByRole("link");
    for (const link of links) {
      expect(link.getAttribute("target")).toBe("_blank");
      expect(link.getAttribute("rel")).toBe("noopener noreferrer");
    }
  });
});
