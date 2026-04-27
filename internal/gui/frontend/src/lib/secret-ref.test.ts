import { describe, expect, it } from "vitest";
import { parseSecretRef, isSecretRef, hasSecretKey } from "./secret-ref";

describe("parseSecretRef", () => {
  it("parses secret: prefix into kind=secret with the key", () => {
    expect(parseSecretRef("secret:foo")).toEqual({ kind: "secret", key: "foo" });
  });
  it("parses file: prefix into kind=file with the key", () => {
    expect(parseSecretRef("file:bar")).toEqual({ kind: "file", key: "bar" });
  });
  it("parses $ prefix into kind=env with the var name", () => {
    expect(parseSecretRef("$HOME")).toEqual({ kind: "env", key: "HOME" });
  });
  it("falls back to literal for plain strings", () => {
    expect(parseSecretRef("plain literal")).toEqual({ kind: "literal", key: null });
  });
  it("treats empty string as literal", () => {
    expect(parseSecretRef("")).toEqual({ kind: "literal", key: null });
  });
  it("treats secret: with empty key as kind=secret with empty string", () => {
    expect(parseSecretRef("secret:")).toEqual({ kind: "secret", key: "" });
  });
});

describe("isSecretRef", () => {
  it("returns true for secret: prefix", () => {
    expect(isSecretRef("secret:x")).toBe(true);
  });
  it("returns true even for empty secret: (editing prefix)", () => {
    expect(isSecretRef("secret:")).toBe(true);
  });
  it("returns false for file: prefix", () => {
    expect(isSecretRef("file:x")).toBe(false);
  });
  it("returns false for $ prefix", () => {
    expect(isSecretRef("$HOME")).toBe(false);
  });
  it("returns false for literal", () => {
    expect(isSecretRef("plain")).toBe(false);
  });
  it("returns false for empty string", () => {
    expect(isSecretRef("")).toBe(false);
  });
});

describe("hasSecretKey", () => {
  it("returns false for editing prefix only (secret: with empty key)", () => {
    expect(hasSecretKey("secret:")).toBe(false);
  });
  it("returns true once a key follows the prefix", () => {
    expect(hasSecretKey("secret:foo")).toBe(true);
  });
  it("returns false for non-secret values", () => {
    expect(hasSecretKey("plain")).toBe(false);
    expect(hasSecretKey("$HOME")).toBe(false);
    expect(hasSecretKey("file:x")).toBe(false);
  });
});
