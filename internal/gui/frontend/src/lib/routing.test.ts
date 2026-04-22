import { describe, expect, it } from "vitest";
import { isHubLoopback } from "./routing";

describe("isHubLoopback", () => {
  it("accepts 127.0.0.1 URLs", () => {
    expect(isHubLoopback("http://127.0.0.1:9123/mcp")).toBe(true);
  });
  it("accepts localhost URLs", () => {
    expect(isHubLoopback("http://localhost:9123/mcp")).toBe(true);
  });
  it("accepts [::1] URLs", () => {
    expect(isHubLoopback("http://[::1]:9123/mcp")).toBe(true);
  });
  it("rejects subdomain-as-path spoofs like 127.0.0.1.evil.com", () => {
    expect(isHubLoopback("https://127.0.0.1.evil.com/foo")).toBe(false);
  });
  it("rejects query-param spoofs like ?host=127.0.0.1", () => {
    expect(isHubLoopback("https://example.com/?host=127.0.0.1")).toBe(false);
  });
  it("rejects non-http schemes like stdio://", () => {
    expect(isHubLoopback("stdio:///memory")).toBe(false);
  });
  it("rejects empty string", () => {
    expect(isHubLoopback("")).toBe(false);
  });
});
