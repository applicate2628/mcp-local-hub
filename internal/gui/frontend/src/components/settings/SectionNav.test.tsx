import { describe, expect, it } from "vitest";
import { render } from "@testing-library/preact";
import { SectionNav } from "./SectionNav";

describe("SectionNav", () => {
  it("renders 5 section links in order", () => {
    const { container } = render(<SectionNav active={null} />);
    const links = container.querySelectorAll("a");
    expect(links).toHaveLength(5);
    expect(links[0].textContent).toBe("Appearance");
    expect(links[4].textContent).toBe("Advanced");
  });

  it("uses query-string deep-link syntax (Codex r1 P1.1)", () => {
    const { container } = render(<SectionNav active={null} />);
    const links = Array.from(container.querySelectorAll("a"));
    for (const a of links) {
      expect(a.getAttribute("href")).toMatch(/^#\/settings\?section=[a-z_]+$/);
    }
  });

  it("highlights active section with class + aria-current", () => {
    const { container } = render(<SectionNav active="backups" />);
    const link = container.querySelector('a[href="#/settings?section=backups"]') as HTMLAnchorElement;
    expect(link.className).toContain("active");
    expect(link.getAttribute("aria-current")).toBe("true");
  });
});
