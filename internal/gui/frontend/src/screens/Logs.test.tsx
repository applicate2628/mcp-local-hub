import { describe, expect, it } from "vitest";
import { classifyLine } from "./Logs";

describe("Logs.classifyLine", () => {
  it("tags ERROR/error/Error as error", () => {
    expect(classifyLine("ERROR: something blew up")).toBe("error");
    expect(classifyLine("[error] foo")).toBe("error");
    expect(classifyLine("Error 500 from upstream")).toBe("error");
  });

  it("tags FATAL/PANIC as error", () => {
    expect(classifyLine("FATAL: cannot recover")).toBe("error");
    expect(classifyLine("panic: nil pointer dereference")).toBe("error");
  });

  it("tags WARN/warning as warn", () => {
    expect(classifyLine("WARN: retrying once")).toBe("warn");
    expect(classifyLine("warning: deprecated flag")).toBe("warn");
  });

  it("returns null for plain info lines", () => {
    expect(classifyLine("INFO: started on :9120")).toBeNull();
    expect(classifyLine("client connected")).toBeNull();
    expect(classifyLine("")).toBeNull();
  });

  it("matches plural and stem variants on a leading word boundary", () => {
    // "errors" / "warnings" are real plurals with the stem at a word
    // boundary — true positive.
    expect(classifyLine("3 errors during startup")).toBe("error");
    expect(classifyLine("multiple warnings issued")).toBe("warn");
    // Mid-word embeddings ("terror", "swarm") never start at a word
    // boundary, so they don't false-match.
    expect(classifyLine("dispatching event terrorist-watchlist")).toBeNull();
    expect(classifyLine("swarm coordinator online")).toBeNull();
  });
});
