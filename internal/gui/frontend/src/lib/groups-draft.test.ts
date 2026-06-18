import { describe, expect, it } from "vitest";
import type { GroupDTO } from "../api";
import {
  validateGroupName,
  parseHiddenTools,
  selectedServers,
  projectToolsHidden,
  draftToBody,
  emptyDraft,
  draftFromGroup,
  isDirty,
  fieldForCode,
  type GroupDraft,
} from "./groups-draft";

const AVAILABLE = ["memory", "time", "serena"];

describe("validateGroupName", () => {
  it("rejects empty / whitespace-only names (mirrors the Go non-empty rule)", () => {
    expect(validateGroupName("")).not.toBeNull();
    expect(validateGroupName("   ")).not.toBeNull();
  });
  it("rejects a name containing the reserved ':' separator (scope-key forge guard)", () => {
    // The Go validateGroupName forbids ':' so a name cannot forge the "g:"
    // kind prefix. The client mirrors that verdict before the POST.
    expect(validateGroupName("g:frontend")).not.toBeNull();
    expect(validateGroupName("a:b")).not.toBeNull();
  });
  it("accepts an ordinary non-empty name", () => {
    expect(validateGroupName("frontend")).toBeNull();
    expect(validateGroupName("infra-tools")).toBeNull();
  });
  it("trims before validating so a padded but otherwise-valid name passes", () => {
    expect(validateGroupName("  frontend  ")).toBeNull();
  });
  it("rejects route-unsafe characters the old denylist leaked (#, %, slash, whitespace)", () => {
    // The allowlist (^[A-Za-z0-9._-]+$) closes the whole class the denylist
    // missed: '#' (URL fragment — server would see only "/g/ops"), '%'
    // (percent-encoding), slash + whitespace, mirroring the Go groupNameAllowed.
    expect(validateGroupName("ops#prod")).not.toBeNull();
    expect(validateGroupName("ops%20prod")).not.toBeNull();
    expect(validateGroupName("front/end")).not.toBeNull();
    expect(validateGroupName("front end")).not.toBeNull();
  });
  it("rejects '.' and '..' (path-traversal segments the route mux rewrites)", () => {
    expect(validateGroupName(".")).not.toBeNull();
    expect(validateGroupName("..")).not.toBeNull();
    // A dot is still allowed mid-name (e.g. a versioned group).
    expect(validateGroupName("v1.2")).toBeNull();
  });
  it("caps the name length at 64 (mirrors the Go maxGroupNameLen — C5)", () => {
    // 64 chars is at the cap → accepted; 65 → rejected.
    expect(validateGroupName("a".repeat(64))).toBeNull();
    expect(validateGroupName("a".repeat(65))).not.toBeNull();
  });
});

describe("parseHiddenTools", () => {
  it("splits on commas and whitespace, trims, and dedupes preserving order", () => {
    expect(parseHiddenTools("definition, references  references , hover")).toEqual([
      "definition",
      "references",
      "hover",
    ]);
  });
  it("returns an empty list for blank input", () => {
    expect(parseHiddenTools("")).toEqual([]);
    expect(parseHiddenTools("   ,  , ")).toEqual([]);
  });
});

describe("selectedServers", () => {
  it("returns only checked servers, sorted (order-independent dirty checks)", () => {
    const draft: GroupDraft = {
      name: "g",
      description: "",
      selected: { time: true, memory: false, serena: true },
      hiddenText: {},
    };
    expect(selectedServers(draft)).toEqual(["serena", "time"]);
  });
});

describe("projectToolsHidden", () => {
  it("emits hidden tools only for SELECTED servers with non-empty parsed lists", () => {
    const draft: GroupDraft = {
      name: "g",
      description: "",
      selected: { memory: true, time: true, serena: false },
      hiddenText: {
        memory: "delete_entities, create_entities",
        // time selected but no hidden tools → omitted (compact wire shape).
        time: "  ",
        // serena UNSELECTED → its hidden text is ignored even if present.
        serena: "find_symbol",
      },
    };
    expect(projectToolsHidden(draft)).toEqual({
      memory: ["delete_entities", "create_entities"],
    });
  });
});

describe("draftToBody", () => {
  it("trims name + description, sorts servers, and omits tools_hidden when empty", () => {
    const draft: GroupDraft = {
      name: "  frontend  ",
      description: "  JS tools  ",
      selected: { time: true, memory: true },
      hiddenText: {},
    };
    const body = draftToBody(draft);
    expect(body.name).toBe("frontend");
    expect(body.description).toBe("JS tools");
    expect(body.servers).toEqual(["memory", "time"]);
    expect(body.tools_hidden).toBeUndefined();
  });
  it("includes tools_hidden when a selected server hides tools", () => {
    const draft: GroupDraft = {
      name: "g",
      description: "",
      selected: { memory: true },
      hiddenText: { memory: "x, y" },
    };
    expect(draftToBody(draft).tools_hidden).toEqual({ memory: ["x", "y"] });
  });
});

describe("emptyDraft", () => {
  it("creates a blank draft with a hidden-text slot per available server", () => {
    const d = emptyDraft(AVAILABLE);
    expect(d.name).toBe("");
    expect(selectedServers(d)).toEqual([]);
    expect(Object.keys(d.hiddenText).sort()).toEqual(["memory", "serena", "time"]);
  });
});

describe("draftFromGroup", () => {
  it("hydrates members pre-checked + joins hidden tools back into tag text", () => {
    const group: GroupDTO = {
      name: "frontend",
      description: "JS tools",
      servers: ["memory", "time"],
      tools_hidden: { memory: ["delete_entities"] },
    };
    const d = draftFromGroup(group, AVAILABLE);
    expect(d.name).toBe("frontend");
    expect(d.description).toBe("JS tools");
    expect(selectedServers(d)).toEqual(["memory", "time"]);
    expect(d.hiddenText.memory).toBe("delete_entities");
    // round-trips losslessly: hydrate → project equals the original.
    expect(draftToBody(d).tools_hidden).toEqual({ memory: ["delete_entities"] });
  });
});

describe("isDirty", () => {
  it("a fresh new-group draft is NOT dirty until it has content", () => {
    expect(isDirty(emptyDraft(AVAILABLE), null)).toBe(false);
  });
  it("a new-group draft becomes dirty once a name OR a server is added", () => {
    const named = { ...emptyDraft(AVAILABLE), name: "x" };
    expect(isDirty(named, null)).toBe(true);
    const withServer: GroupDraft = {
      ...emptyDraft(AVAILABLE),
      selected: { memory: true },
    };
    expect(isDirty(withServer, null)).toBe(true);
  });
  it("an edit draft equal to the persisted group is NOT dirty (order-independent)", () => {
    const group: GroupDTO = {
      name: "g",
      description: "d",
      servers: ["time", "memory"],
      tools_hidden: { memory: ["a", "b"] },
    };
    const d = draftFromGroup(group, AVAILABLE);
    expect(isDirty(d, group)).toBe(false);
    // Reordering the hidden-tool tags is NOT a real change (set equality).
    const reordered: GroupDraft = { ...d, hiddenText: { ...d.hiddenText, memory: "b, a" } };
    expect(isDirty(reordered, group)).toBe(false);
  });
  it("an edit draft becomes dirty when membership, description, or hidden tools change", () => {
    const group: GroupDTO = {
      name: "g",
      description: "d",
      servers: ["memory"],
      tools_hidden: {},
    };
    const base = draftFromGroup(group, AVAILABLE);
    expect(isDirty({ ...base, selected: { memory: true, time: true } }, group)).toBe(true);
    expect(isDirty({ ...base, description: "changed" }, group)).toBe(true);
    expect(
      isDirty({ ...base, hiddenText: { ...base.hiddenText, memory: "x" } }, group),
    ).toBe(true);
  });
});

describe("fieldForCode", () => {
  it("maps each backend validation code to its offending field", () => {
    expect(fieldForCode("GROUPS_INVALID_NAME")).toBe("name");
    expect(fieldForCode("GROUPS_NAME_REQUIRED")).toBe("name");
    expect(fieldForCode("GROUPS_UNKNOWN_SERVER")).toBe("servers");
    expect(fieldForCode("GROUPS_HIDDEN_NONMEMBER")).toBe("tools");
  });
  it("maps unknown / transport codes to a form-level banner", () => {
    expect(fieldForCode("GROUPS_WRITE_FAILED")).toBe("form");
    expect(fieldForCode("HTTP_500")).toBe("form");
  });
});
