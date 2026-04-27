import { describe, expect, it } from "vitest";
import { RESERVED_SECRET_NAMES, isReservedName, SECRET_NAME_RE, isValidSecretName } from "./reserved-names";

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

describe("SECRET_NAME_RE / isValidSecretName (Codex PR-r1 P1)", () => {
  it("accepts standard identifier names", () => {
    expect(isValidSecretName("foo")).toBe(true);
    expect(isValidSecretName("OPENAI_API_KEY")).toBe(true);
    expect(isValidSecretName("a")).toBe(true);
    expect(isValidSecretName("Foo123_bar")).toBe(true);
  });
  it("rejects names starting with digit", () => {
    expect(isValidSecretName("1foo")).toBe(false);
  });
  it("rejects names starting with underscore", () => {
    expect(isValidSecretName("_foo")).toBe(false);
  });
  it("rejects names with hyphen", () => {
    expect(isValidSecretName("foo-bar")).toBe(false);
    expect(isValidSecretName("api-key")).toBe(false);
  });
  it("rejects names with other special characters", () => {
    expect(isValidSecretName("foo.bar")).toBe(false);
    expect(isValidSecretName("foo bar")).toBe(false);
    expect(isValidSecretName("foo@bar")).toBe(false);
  });
  it("rejects empty string", () => {
    expect(isValidSecretName("")).toBe(false);
  });
  it("SECRET_NAME_RE matches the backend regex literally", () => {
    expect(SECRET_NAME_RE.source).toBe("^[A-Za-z][A-Za-z0-9_]*$");
  });
});
