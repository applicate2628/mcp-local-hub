// Coarse-theme display taxonomy for the Catalog "Marketplace" section.
//
// The marketplace catalog rows already carry a GRANULAR `categories: string[]`
// field (54 distinct tags across 22 entries, mostly 1× — e.g. `daw`, `pcb`,
// `eda`, `lsp`, `spreadsheet`). Rendering one section per raw category would
// produce ~54 mostly-single-entry sections — useless. This module is a
// FRONTEND-ONLY display taxonomy that folds those granular tags into ~6 coarse
// themes ("дев отдельно, музыка отдельно, кады отдельно") so the Catalog groups
// into a handful of human-meaningful sections.
//
// It is a DISPLAY layer over the existing `categories` field — NO backend /
// schema change and NO re-tagging of entries. The category strings stay exactly
// as the registry ships them; only their GROUPING for render is decided here.

// CatalogTheme is the coarse display bucket a granular category folds into. The
// literal strings double as the rendered section-header text.
export type CatalogTheme =
  | "Engineering & CAD"
  | "Development & Code"
  | "Data & Office"
  | "Research & Docs"
  | "Music & Audio"
  | "Creative & Media"
  | "Utilities"
  | "Other";

// THEME_ORDER is the STABLE display order of the section headers. A theme with
// zero entries is dropped at render time, but the relative order of the themes
// that DO appear always follows this list. "Other" is last by design — it is
// the catch-all for any category that maps to no known theme (none today, but
// the helper must keep a future uncategorized/unknown-tag row visible rather
// than silently dropping it).
export const THEME_ORDER: readonly CatalogTheme[] = [
  "Engineering & CAD",
  "Development & Code",
  "Data & Office",
  "Research & Docs",
  "Music & Audio",
  "Creative & Media",
  "Utilities",
  "Other",
];

// CATEGORY_TO_THEME maps each granular category the registry ships onto one
// coarse theme. Keys are matched case-insensitively (see normalizeCategory).
// The current v2 catalog's categories are all covered; any category absent from
// this map folds into "Other" via themeForCategories so an unknown future tag
// still renders.
const CATEGORY_TO_THEME: Readonly<Record<string, CatalogTheme>> = {
  // Engineering & CAD — CAD/CAE/EDA/FEA and scientific-engineering tooling.
  engineering: "Engineering & CAD",
  cad: "Engineering & CAD",
  cae: "Engineering & CAD",
  eda: "Engineering & CAD",
  fea: "Engineering & CAD",
  mechanical: "Engineering & CAD",
  pcb: "Engineering & CAD",
  electronics: "Engineering & CAD",
  drawing: "Engineering & CAD",
  com: "Engineering & CAD",
  ansys: "Engineering & CAD",
  matlab: "Engineering & CAD",
  simulation: "Engineering & CAD",
  "scientific-computing": "Engineering & CAD",
  // science/statistics share the engineering-scientific bucket (the rmcp R row's
  // primary category is `science`; statistics rides alongside math/sci-computing).
  // `plotting` is the origin-pro scientific-graphing tag (rides with science/data).
  science: "Engineering & CAD",
  statistics: "Engineering & CAD",
  plotting: "Engineering & CAD",

  // Music & Audio.
  music: "Music & Audio",
  daw: "Music & Audio",
  audio: "Music & Audio",
  ableton: "Music & Audio",

  // Creative & Media — image/graphics editing and creative-design desktop tools
  // (kept distinct from Music & Audio, which is audio-specific; the photoshop row's
  // primary category is `image-editing`). The wave-2b docs-only pointers add
  // creative-3D (blender's `3d`/`modeling`) and video-editing (davinci's `video`/
  // `editing`) tags here. `3d`/`modeling` map to Creative & Media so the
  // creative-3D blender row buckets here, while the CAD-3D rhino row (primary
  // category `engineering`) stays in Engineering & CAD by the primary-category rule.
  "image-editing": "Creative & Media",
  design: "Creative & Media",
  creative: "Creative & Media",
  "3d": "Creative & Media",
  modeling: "Creative & Media",
  video: "Creative & Media",
  editing: "Creative & Media",

  // Development & Code — code intelligence, agents, VCS, debugging.
  "code-intelligence": "Development & Code",
  lsp: "Development & Code",
  refactoring: "Development & Code",
  "coding-agent": "Development & Code",
  codex: "Development & Code",
  git: "Development & Code",
  vcs: "Development & Code",
  qt: "Development & Code",
  debug: "Development & Code",
  agent: "Development & Code",
  reasoning: "Development & Code",
  ai: "Development & Code",
  dev: "Development & Code",

  // Data & Office — databases, spreadsheets, filesystem/IO, BI/analytics, and
  // data-science notebooks (grafana/tableau/metabase/jupyter land here by their
  // primary `data` category; observability/bi/analytics/notebook/data-analysis are
  // the data-platform tags those rows carry).
  database: "Data & Office",
  sql: "Data & Office",
  spreadsheet: "Data & Office",
  excel: "Data & Office",
  office: "Data & Office",
  io: "Data & Office",
  filesystem: "Data & Office",
  data: "Data & Office",
  observability: "Data & Office",
  bi: "Data & Office",
  analytics: "Data & Office",
  notebook: "Data & Office",
  "data-analysis": "Data & Office",
  // Office-document docs-only pointers (word/powerpoint) — their primary `office`
  // tag already buckets here; `documents`/`presentations` ride alongside it.
  documents: "Data & Office",
  presentations: "Data & Office",

  // Research & Docs — documentation, papers, knowledge, diagrams, math, and
  // personal-knowledge/notes tooling (the wave-2b obsidian/logseq PKM rows lead
  // with `pkm`; `notes`/`productivity` ride alongside it). The anki flashcards
  // pointer (primary `productivity`) and its `education`/`flashcards` tags bucket
  // here as study/learning tooling.
  docs: "Research & Docs",
  papers: "Research & Docs",
  research: "Research & Docs",
  reference: "Research & Docs",
  "knowledge-graph": "Research & Docs",
  diagrams: "Research & Docs",
  math: "Research & Docs",
  visualization: "Research & Docs",
  pkm: "Research & Docs",
  notes: "Research & Docs",
  productivity: "Research & Docs",
  education: "Research & Docs",
  flashcards: "Research & Docs",

  // Utilities — general-purpose helpers and everything not above.
  utilities: "Utilities",
  memory: "Utilities",
  time: "Utilities",
  browser: "Utilities",
  http: "Utilities",
  remote: "Utilities",
  automation: "Utilities",
  generation: "Utilities",
};

// normalizeCategory folds a raw category string to its lookup key (trimmed +
// lower-cased) so registry casing/whitespace variation can't miss the map.
function normalizeCategory(category: string): string {
  return category.trim().toLowerCase();
}

// OTHER_THEME is the fallback bucket when no category maps to a known theme.
const OTHER_THEME: CatalogTheme = "Other";

// themeForCategories resolves ONE entry's coarse theme by the PRIMARY-category
// rule: the theme of the FIRST category (in the entry's own order) that maps to
// a known theme. A category that maps to no theme is skipped, so a leading
// unknown tag does not force the whole entry into "Other" if a later category
// is recognized. If NONE of the categories map (including an empty list), the
// entry folds into "Other".
export function themeForCategories(categories: readonly string[]): CatalogTheme {
  for (const raw of categories) {
    const theme = CATEGORY_TO_THEME[normalizeCategory(raw)];
    if (theme !== undefined) return theme;
  }
  return OTHER_THEME;
}

// ThemeSection is one rendered group: the theme header + the entries that fold
// into it, in the order groupByTheme produced (caller-sorted).
export interface ThemeSection<T> {
  theme: CatalogTheme;
  entries: T[];
}

// groupByTheme folds a flat entry list into ordered theme sections.
//
//   • getCategories(entry) yields the entry's raw categories; themeForCategories
//     picks the coarse bucket by the primary-category rule.
//   • Sections appear in THEME_ORDER; a theme with zero entries is DROPPED (no
//     empty section headers).
//   • Within each section, entries are sorted by getSortKey (stable display
//     order) — the caller supplies the key (e.g. the entry name) so the helper
//     stays agnostic of the entry shape.
//   • An empty input yields zero sections (the caller renders its empty-state
//     card instead).
export function groupByTheme<T>(
  entries: readonly T[],
  getCategories: (entry: T) => readonly string[],
  getSortKey: (entry: T) => string,
): ThemeSection<T>[] {
  const buckets = new Map<CatalogTheme, T[]>();
  for (const entry of entries) {
    const theme = themeForCategories(getCategories(entry));
    const bucket = buckets.get(theme);
    if (bucket) bucket.push(entry);
    else buckets.set(theme, [entry]);
  }

  const sections: ThemeSection<T>[] = [];
  for (const theme of THEME_ORDER) {
    const bucket = buckets.get(theme);
    if (!bucket || bucket.length === 0) continue;
    bucket.sort((a, b) =>
      getSortKey(a).localeCompare(getSortKey(b), undefined, { sensitivity: "base" }),
    );
    sections.push({ theme, entries: bucket });
  }
  return sections;
}
