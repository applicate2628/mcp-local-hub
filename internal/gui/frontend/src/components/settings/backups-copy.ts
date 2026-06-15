// Codex-locked copy strings (memo §9.4). DO NOT paraphrase. The Vitest
// test in backups-copy.test.ts asserts exact equality against the memo
// literals — paraphrasing breaks those tests independent of any
// component test that only checks rendering.
export const BACKUPS_COPY = {
  sliderLabel: "Keep timestamped backups per client",
  helperText:
    "Drag to set how many timestamped backups to keep per client. The Clean buttons below delete the older eligible ones; originals are never touched.",
  rowBadge:    "Would be eligible for cleanup",
  groupNote:
    "Original backups are never cleaned. Retention is calculated separately for each client.",
  previewFailureInline: "Preview unavailable",
} as const;
