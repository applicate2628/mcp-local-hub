// lib/project-toggle.test.ts — pure-fn tests for the P3b toggle helpers.
import { describe, expect, it } from "vitest";
import { scopeForToggle, toggleErrorCopy } from "./project-toggle";

describe("scopeForToggle (single-owner scope rule)", () => {
  it("maps each mechanism to its stable scope — never a client-name branch", () => {
    expect(scopeForToggle("workspace")).toBe("workspace-lsp");
    expect(scopeForToggle("object-member")).toBe("project-object-member");
    expect(scopeForToggle("claude-local")).toBe("claude-local-membership");
    expect(scopeForToggle("group")).toBe("group-servers");
  });
});

describe("toggleErrorCopy (§3.1 code→plain-copy map)", () => {
  it("INVALID → retry, no reload, toggle visible", () => {
    const c = toggleErrorCopy("PROJECT_TOGGLE_INVALID");
    expect(c.message).toContain("required field was missing");
    expect(c.retry).toBe(true);
    expect(c.reload).toBe(false);
    expect(c.hideToggle).toBe(false);
  });

  it("UNSUPPORTED → hide the toggle, no retry", () => {
    const c = toggleErrorCopy("PROJECT_TOGGLE_UNSUPPORTED");
    expect(c.message).toContain("manage it in the client");
    expect(c.hideToggle).toBe(true);
    expect(c.retry).toBe(false);
  });

  it("ROOT_INVALID → retry + reload", () => {
    const c = toggleErrorCopy("PROJECT_ROOT_INVALID");
    expect(c.message).toContain("moved or been deleted");
    expect(c.retry).toBe(true);
    expect(c.reload).toBe(true);
  });

  it("UNKNOWN_SERVER → no retry (a wrong name won't get righter on retry)", () => {
    const c = toggleErrorCopy("PROJECT_TOGGLE_UNKNOWN_SERVER");
    expect(c.message).toContain("known routable server");
    expect(c.retry).toBe(false);
  });

  it("GROUP_NOT_FOUND → reload the section, no retry", () => {
    const c = toggleErrorCopy("PROJECT_TOGGLE_GROUP_NOT_FOUND");
    expect(c.message).toContain("no longer exists");
    expect(c.reload).toBe(true);
    expect(c.retry).toBe(false);
  });

  it("FAILED and an UNKNOWN code → the generic save-failed + retry default", () => {
    const failed = toggleErrorCopy("PROJECT_TOGGLE_FAILED");
    expect(failed.message).toContain("couldn't be saved");
    expect(failed.retry).toBe(true);
    // An unmodelled code falls back to the same safe default.
    const unknown = toggleErrorCopy("SOME_FUTURE_CODE");
    expect(unknown).toEqual(failed);
  });
});
