// Lock the verbatim Codex copy from memo §9.4. If a future implementer
// rewords any of these constants the test fails immediately, regardless
// of whether component tests still pass against the (paraphrased) constant.
import { describe, expect, it } from "vitest";
import { BACKUPS_COPY } from "./backups-copy";

describe("BACKUPS_COPY (memo §9.4 verbatim Codex copy)", () => {
  it("sliderLabel matches memo exactly", () => {
    expect(BACKUPS_COPY.sliderLabel).toBe("Keep timestamped backups per client");
  });
  it("helperText matches memo exactly", () => {
    expect(BACKUPS_COPY.helperText).toBe("Preview only. No files are deleted from this screen.");
  });
  it("rowBadge matches memo exactly", () => {
    expect(BACKUPS_COPY.rowBadge).toBe("Would be eligible for cleanup");
  });
  it("cleanTooltip matches memo exactly", () => {
    expect(BACKUPS_COPY.cleanTooltip).toBe(
      "Cleanup arrives in A4-b. This view only previews which timestamped backups cleanup would target.",
    );
  });
  it("groupNote matches memo exactly", () => {
    expect(BACKUPS_COPY.groupNote).toBe(
      "Original backups are never cleaned. Retention is calculated separately for each client.",
    );
  });
  it("previewFailureInline matches memo exactly", () => {
    expect(BACKUPS_COPY.previewFailureInline).toBe("Preview unavailable");
  });
});
