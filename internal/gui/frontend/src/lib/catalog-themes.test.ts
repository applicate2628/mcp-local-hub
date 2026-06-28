import { describe, it, expect } from "vitest";
import {
  THEME_ORDER,
  themeForCategories,
  groupByTheme,
  type CatalogTheme,
} from "./catalog-themes";

describe("themeForCategories — granular category → coarse theme", () => {
  // Each granular category the v2 catalog ships maps to its intended coarse
  // theme. One representative per theme bucket plus the engineering/CAD spread.
  const cases: Array<[string, CatalogTheme]> = [
    // Engineering & CAD
    ["engineering", "Engineering & CAD"],
    ["cad", "Engineering & CAD"],
    ["cae", "Engineering & CAD"],
    ["eda", "Engineering & CAD"],
    ["fea", "Engineering & CAD"],
    ["mechanical", "Engineering & CAD"],
    ["pcb", "Engineering & CAD"],
    ["electronics", "Engineering & CAD"],
    ["ansys", "Engineering & CAD"],
    ["matlab", "Engineering & CAD"],
    ["simulation", "Engineering & CAD"],
    ["scientific-computing", "Engineering & CAD"],
    // Music & Audio
    ["music", "Music & Audio"],
    ["daw", "Music & Audio"],
    ["audio", "Music & Audio"],
    ["ableton", "Music & Audio"],
    // Development & Code
    ["code-intelligence", "Development & Code"],
    ["lsp", "Development & Code"],
    ["git", "Development & Code"],
    ["vcs", "Development & Code"],
    ["agent", "Development & Code"],
    ["reasoning", "Development & Code"],
    ["debug", "Development & Code"],
    // Data & Office
    ["database", "Data & Office"],
    ["sql", "Data & Office"],
    ["spreadsheet", "Data & Office"],
    ["filesystem", "Data & Office"],
    ["io", "Data & Office"],
    // Research & Docs
    ["docs", "Research & Docs"],
    ["papers", "Research & Docs"],
    ["research", "Research & Docs"],
    ["diagrams", "Research & Docs"],
    ["math", "Research & Docs"],
    // Utilities
    ["utilities", "Utilities"],
    ["memory", "Utilities"],
    ["time", "Utilities"],
    ["http", "Utilities"],
    ["browser", "Utilities"],
    ["automation", "Utilities"],
  ];

  it.each(cases)("category %s → %s", (category, theme) => {
    expect(themeForCategories([category])).toBe(theme);
  });

  it("folds an unknown tag into Other", () => {
    expect(themeForCategories(["totally-made-up-tag"])).toBe("Other");
  });

  it("folds an empty category list into Other", () => {
    expect(themeForCategories([])).toBe("Other");
  });

  it("uses the PRIMARY (first mapping) category for a multi-category entry", () => {
    // excel ships ['engineering', 'office', 'spreadsheet', 'com', 'excel'] — the
    // first category 'engineering' decides the theme even though later tags
    // (office/spreadsheet/excel) would map to Data & Office.
    expect(
      themeForCategories(["engineering", "office", "spreadsheet", "com", "excel"]),
    ).toBe("Engineering & CAD");
    // serena ships ['code-intelligence', 'lsp', 'refactoring'] — primary decides.
    expect(themeForCategories(["code-intelligence", "lsp", "refactoring"])).toBe(
      "Development & Code",
    );
  });

  it("skips a leading UNKNOWN tag and uses the first KNOWN category", () => {
    // A leading unmapped tag must not force the entry into Other when a later
    // category is recognized.
    expect(themeForCategories(["mystery-tag", "music", "daw"])).toBe("Music & Audio");
  });

  it("is case- and whitespace-insensitive on the category key", () => {
    expect(themeForCategories(["  Music  "])).toBe("Music & Audio");
    expect(themeForCategories(["CODE-INTELLIGENCE"])).toBe("Development & Code");
  });
});

describe("groupByTheme — flat list → ordered theme sections", () => {
  interface Row {
    name: string;
    categories: string[];
  }
  const cats = (r: Row) => r.categories;
  const key = (r: Row) => r.name;

  it("yields zero sections for an empty input", () => {
    expect(groupByTheme<Row>([], cats, key)).toEqual([]);
  });

  it("drops themes with zero entries (only non-empty sections render)", () => {
    const rows: Row[] = [
      { name: "ableton", categories: ["music", "daw"] },
      { name: "git", categories: ["git", "vcs"] },
    ];
    const sections = groupByTheme(rows, cats, key);
    const themes = sections.map((s) => s.theme);
    // Only the two themes with members appear — no empty Engineering/Data/etc.
    expect(themes).toEqual(["Development & Code", "Music & Audio"]);
  });

  it("orders sections by the stable THEME_ORDER regardless of input order", () => {
    // Input deliberately out of THEME_ORDER (Music first, Engineering last).
    const rows: Row[] = [
      { name: "suno", categories: ["music", "ai"] },
      { name: "memory", categories: ["memory"] }, // Utilities
      { name: "serena", categories: ["code-intelligence"] }, // Dev & Code
      { name: "onshape", categories: ["engineering", "cad"] }, // Engineering & CAD
    ];
    const themes = groupByTheme(rows, cats, key).map((s) => s.theme);
    expect(themes).toEqual([
      "Engineering & CAD",
      "Development & Code",
      "Music & Audio",
      "Utilities",
    ]);
    // The rendered order is a subsequence of THEME_ORDER.
    const orderIndex = themes.map((t) => THEME_ORDER.indexOf(t));
    expect(orderIndex).toEqual([...orderIndex].sort((a, b) => a - b));
  });

  it("sorts entries alphabetically within a section", () => {
    const rows: Row[] = [
      { name: "matlab", categories: ["engineering"] },
      { name: "ansys", categories: ["engineering"] },
      { name: "kicad", categories: ["engineering"] },
      { name: "excel", categories: ["engineering"] },
    ];
    const sections = groupByTheme(rows, cats, key);
    expect(sections).toHaveLength(1);
    expect(sections[0].theme).toBe("Engineering & CAD");
    expect(sections[0].entries.map((e) => e.name)).toEqual([
      "ansys",
      "excel",
      "kicad",
      "matlab",
    ]);
  });

  it("places an all-unknown-category entry into a trailing Other section", () => {
    const rows: Row[] = [
      { name: "git", categories: ["git"] },
      { name: "mystery", categories: ["unmapped-future-tag"] },
    ];
    const sections = groupByTheme(rows, cats, key);
    expect(sections.map((s) => s.theme)).toEqual(["Development & Code", "Other"]);
    expect(sections[1].entries.map((e) => e.name)).toEqual(["mystery"]);
  });

  it("groups the full v2 catalog into the six expected non-empty themes", () => {
    // The real 23-entry v2 catalog primary categories. Confirms every entry
    // lands and the distribution matches the design (no surprise Other).
    const rows: Row[] = [
      { name: "filesystem", categories: ["filesystem", "io"] },
      { name: "git", categories: ["git", "vcs"] },
      { name: "fetch", categories: ["http", "io"] },
      { name: "sqlite", categories: ["database", "sql"] },
      { name: "playwright", categories: ["browser", "automation"] },
      { name: "context7", categories: ["docs", "remote"] },
      { name: "qt-docs", categories: ["docs", "remote", "qt"] },
      { name: "excalidraw", categories: ["diagrams", "drawing", "visualization"] },
      { name: "everything", categories: ["debug", "reference"] },
      { name: "memory", categories: ["memory", "knowledge-graph"] },
      { name: "time", categories: ["time", "utilities"] },
      { name: "sequential-thinking", categories: ["reasoning", "agent"] },
      { name: "serena", categories: ["code-intelligence", "lsp", "refactoring"] },
      { name: "paper-search-mcp", categories: ["research", "papers"] },
      { name: "excel", categories: ["engineering", "office", "spreadsheet", "com", "excel"] },
      { name: "ableton", categories: ["music", "daw", "ableton"] },
      { name: "codex-mcp-server", categories: ["agent", "codex", "coding-agent"] },
      { name: "matlab", categories: ["engineering", "matlab", "math", "scientific-computing"] },
      { name: "ansys", categories: ["engineering", "ansys", "fea", "cae", "simulation"] },
      { name: "kicad", categories: ["engineering", "eda", "pcb", "electronics"] },
      { name: "suno", categories: ["music", "ai", "audio", "generation"] },
      { name: "onshape", categories: ["engineering", "cad", "mechanical", "cloud"] },
      { name: "reaper", categories: ["music", "daw", "audio"] },
    ];
    const sections = groupByTheme(rows, cats, key);
    // No Other section for the real catalog (every entry maps).
    expect(sections.map((s) => s.theme)).toEqual([
      "Engineering & CAD",
      "Development & Code",
      "Data & Office",
      "Research & Docs",
      "Music & Audio",
      "Utilities",
    ]);
    const counts = Object.fromEntries(sections.map((s) => [s.theme, s.entries.length]));
    expect(counts).toEqual({
      "Engineering & CAD": 5, // excel, matlab, ansys, kicad, onshape
      "Development & Code": 5, // git, everything, sequential-thinking, serena, codex-mcp-server
      "Data & Office": 2, // filesystem, sqlite
      "Research & Docs": 4, // context7, qt-docs, excalidraw, paper-search-mcp
      "Music & Audio": 3, // ableton, suno, reaper
      "Utilities": 4, // fetch, playwright, memory, time
    });
    // Every input entry is accounted for exactly once.
    const total = sections.reduce((n, s) => n + s.entries.length, 0);
    expect(total).toBe(rows.length);
  });
});
