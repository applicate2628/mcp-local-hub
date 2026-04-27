import { describe, expect, it } from "vitest";
import { normalizeForMatch, matchTier } from "./name-match";

describe("normalizeForMatch", () => {
  it("lowercases the input", () => {
    expect(normalizeForMatch("OPENAI_API_KEY")).toBe("openai_api_key");
  });
  it("converts hyphens to underscores", () => {
    expect(normalizeForMatch("X-API-KEY")).toBe("x_api_key");
  });
  it("preserves already-lowercase inputs", () => {
    expect(normalizeForMatch("foo")).toBe("foo");
  });
  it("handles mixed case + hyphen together", () => {
    expect(normalizeForMatch("My-API-Key")).toBe("my_api_key");
  });
});

describe("matchTier", () => {
  it("returns 0 for exact match after normalization (case + hyphen insensitive)", () => {
    expect(matchTier("openai_api_key", "OPENAI_API_KEY")).toBe(0);
  });
  it("returns 0 even when vault key is uppercase", () => {
    expect(matchTier("OPENAI_API_KEY", "openai_api_key")).toBe(0);
  });
  it("returns 1 when vault key extends env key (vault is more specific)", () => {
    expect(matchTier("openai_api_key_v2", "OPENAI_API_KEY")).toBe(1);
  });
  it("returns 1 when env key extends vault key (env is more specific)", () => {
    expect(matchTier("openai", "OPENAI_API_KEY")).toBe(1);
  });
  it("returns 2 for unrelated keys", () => {
    expect(matchTier("wolfram", "OPENAI_API_KEY")).toBe(2);
  });
  it("treats hyphens and underscores as equivalent", () => {
    expect(matchTier("x-api-key", "X_API_KEY")).toBe(0);
  });
});
