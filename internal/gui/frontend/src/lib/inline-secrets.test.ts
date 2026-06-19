import { describe, expect, it } from "vitest";
import { inlineSecretsToWrite } from "./inline-secrets";

describe("inlineSecretsToWrite", () => {
  it("writes only values for keys still referenced as a secret: ref", () => {
    const out = inlineSecretsToWrite(
      { api_key: "sk-1", orphan: "x" },
      [{ value: "secret:api_key" }, { value: "literal" }],
    );
    expect(out).toEqual([["api_key", "sk-1"]]);
  });

  it("drops a value whose ref was removed or renamed", () => {
    expect(inlineSecretsToWrite({ gone: "v" }, [{ value: "secret:other" }])).toEqual([]);
  });

  it("ignores empty / whitespace-only values", () => {
    expect(inlineSecretsToWrite({ k: "   " }, [{ value: "secret:k" }])).toEqual([]);
  });

  it("ignores a bare secret: ref with no key", () => {
    expect(inlineSecretsToWrite({ "": "v" }, [{ value: "secret:" }])).toEqual([]);
  });

  it("drops a non-conforming vault key name (hyphen / leading digit)", () => {
    expect(
      inlineSecretsToWrite(
        { "foo-bar": "v", "123": "x" },
        [{ value: "secret:foo-bar" }, { value: "secret:123" }],
      ),
    ).toEqual([]);
  });

  it("keeps a valid letter/underscore/digit key", () => {
    expect(inlineSecretsToWrite({ API_KEY2: "v" }, [{ value: "secret:API_KEY2" }])).toEqual([
      ["API_KEY2", "v"],
    ]);
  });
});
