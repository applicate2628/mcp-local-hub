// Codex-locked copy strings (memo §9.4). DO NOT paraphrase. The Vitest
// test in backups-copy.test.ts asserts exact equality against the memo
// literals — paraphrasing breaks those tests independent of any
// component test that only checks rendering.
export const BACKUPS_COPY = {
  sliderLabel: "Keep timestamped backups per client",
  helperText:  "Preview only. No files are deleted from this screen.",
  rowBadge:    "Would be eligible for cleanup",
  cleanTooltip:
    "Cleanup arrives in A4-b. This view only previews which timestamped backups cleanup would target.",
  groupNote:
    "Original backups are never cleaned. Retention is calculated separately for each client.",
  previewFailureInline: "Preview unavailable",
} as const;
