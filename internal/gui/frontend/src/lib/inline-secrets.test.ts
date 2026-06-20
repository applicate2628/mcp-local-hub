import { describe, expect, it } from "vitest";
import { inlineSecretsToWrite } from "./inline-secrets";
import { BLANK_FORM } from "./manifest-yaml";
import type { ManifestFormState } from "../types";

function form(overrides: Partial<ManifestFormState>): ManifestFormState {
  return {
    ...BLANK_FORM,
    ...overrides,
    _preservedRaw: {
      ...BLANK_FORM._preservedRaw,
      ...overrides._preservedRaw,
    },
  };
}

describe("inlineSecretsToWrite", () => {
  it("writes only values for keys still referenced as a secret: ref", () => {
    const out = inlineSecretsToWrite(
      { api_key: "sk-1", orphan: "x" },
      form({ env: [{ key: "API_KEY", value: "secret:api_key" }, { key: "PLAIN", value: "literal" }] }),
    );
    expect(out).toEqual([["api_key", "sk-1"]]);
  });

  it("drops a value whose ref was removed or renamed", () => {
    expect(inlineSecretsToWrite({ gone: "v" }, form({ env: [{ key: "OTHER", value: "secret:other" }] }))).toEqual([]);
  });

  it("ignores empty / whitespace-only values", () => {
    expect(inlineSecretsToWrite({ k: "   " }, form({ env: [{ key: "K", value: "secret:k" }] }))).toEqual([]);
  });

  it("ignores a bare secret: ref with no key", () => {
    expect(inlineSecretsToWrite({ "": "v" }, form({ env: [{ key: "EMPTY", value: "secret:" }] }))).toEqual([]);
  });

  it("drops a non-conforming vault key name (hyphen / leading digit)", () => {
    expect(
      inlineSecretsToWrite(
        { "foo-bar": "v", "123": "x" },
        form({
          env: [
            { key: "BAD_ONE", value: "secret:foo-bar" },
            { key: "BAD_TWO", value: "secret:123" },
          ],
        }),
      ),
    ).toEqual([]);
  });

  it("keeps a valid letter/underscore/digit key", () => {
    expect(inlineSecretsToWrite({ API_KEY2: "v" }, form({ env: [{ key: "API_KEY2", value: "secret:API_KEY2" }] }))).toEqual([
      ["API_KEY2", "v"],
    ]);
  });

  it("drops a reserved vault key name (init)", () => {
    expect(inlineSecretsToWrite({ init: "v" }, form({ env: [{ key: "INIT", value: "secret:init" }] }))).toEqual([]);
  });

  it("writes a value for a remote url ${secret:KEY} placeholder", () => {
    const out = inlineSecretsToWrite(
      { TOKEN: "remote-token", unused: "drop" },
      form({ _preservedRaw: { url: "https://api.example.test/mcp?token=${secret:TOKEN}" } }),
    );
    expect(out).toEqual([["TOKEN", "remote-token"]]);
  });

  it("writes values for remote header ${secret:KEY} placeholders", () => {
    const out = inlineSecretsToWrite(
      { TOKEN: "remote-token", OTHER: "other" },
      form({
        _preservedRaw: {
          headers: {
            Authorization: "Bearer ${secret:TOKEN}",
            "X-Other": "${secret:OTHER}",
          },
        },
      }),
    );
    expect(out).toEqual([
      ["TOKEN", "remote-token"],
      ["OTHER", "other"],
    ]);
  });
});
