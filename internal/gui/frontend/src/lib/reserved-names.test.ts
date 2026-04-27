import { describe, expect, it } from "vitest";
import { RESERVED_SECRET_NAMES, isReservedName } from "./reserved-names";

describe("RESERVED_SECRET_NAMES", () => {
  it("contains 'init' (HTTP routing reservation)", () => {
    expect(RESERVED_SECRET_NAMES.has("init")).toBe(true);
  });
  it("does not contain arbitrary other names", () => {
    expect(RESERVED_SECRET_NAMES.has("foo")).toBe(false);
    expect(RESERVED_SECRET_NAMES.has("openai_api_key")).toBe(false);
  });
});

describe("isReservedName", () => {
  it("returns true for 'init' exactly", () => {
    expect(isReservedName("init")).toBe(true);
  });
  it("returns false for case variants (Set is case-sensitive — backend names are lowercase by convention)", () => {
    // Documenting the conservative behavior: backend AddSecret name validator
    // matches /^[A-Za-z][A-Za-z0-9_]*$/ which allows uppercase. The reserved
    // check is exact-match because the route conflict is also exact.
    expect(isReservedName("INIT")).toBe(false);
    expect(isReservedName("Init")).toBe(false);
  });
  it("returns false for unrelated names", () => {
    expect(isReservedName("foo")).toBe(false);
  });
  it("returns false for empty string", () => {
    expect(isReservedName("")).toBe(false);
  });
});
