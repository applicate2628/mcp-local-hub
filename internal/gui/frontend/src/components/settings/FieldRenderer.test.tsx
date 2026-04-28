import { describe, expect, it, vi } from "vitest";
import { render, fireEvent } from "@testing-library/preact";
import { FieldRenderer } from "./FieldRenderer";
import type { ConfigSettingDTO } from "../../lib/settings-types";

const enumDef: ConfigSettingDTO = {
  key: "appearance.theme", section: "appearance", type: "enum",
  default: "system", value: "system", enum: ["light", "dark", "system"],
  deferred: false, help: "Color theme.",
};
const boolDef: ConfigSettingDTO = {
  key: "gui_server.browser_on_launch", section: "gui_server", type: "bool",
  default: "true", value: "true", deferred: false, help: "",
};
const intDef: ConfigSettingDTO = {
  key: "gui_server.port", section: "gui_server", type: "int",
  default: "9125", value: "9125", min: 1024, max: 65535, deferred: false, help: "",
};
const pathDef: ConfigSettingDTO = {
  key: "appearance.default_home", section: "appearance", type: "path",
  default: "", value: "", optional: true, deferred: false, help: "",
};

describe("FieldRenderer", () => {
  it("renders <select> for enum with all options", () => {
    const onChange = vi.fn();
    const { container } = render(<FieldRenderer def={enumDef} value="system" onChange={onChange} />);
    const select = container.querySelector("select")!;
    expect(select).toBeTruthy();
    expect(select.querySelectorAll("option")).toHaveLength(3);
  });

  it("enum onChange fires with selected option value", () => {
    const onChange = vi.fn();
    const { container } = render(<FieldRenderer def={enumDef} value="system" onChange={onChange} />);
    const select = container.querySelector("select")! as HTMLSelectElement;
    fireEvent.change(select, { target: { value: "dark" } });
    expect(onChange).toHaveBeenCalledWith("dark");
  });

  it("bool checkbox checked iff value === 'true'", () => {
    const { container } = render(<FieldRenderer def={boolDef} value="true" onChange={() => {}} />);
    expect((container.querySelector("input[type=checkbox]") as HTMLInputElement).checked).toBe(true);
    const { container: c2 } = render(<FieldRenderer def={boolDef} value="false" onChange={() => {}} />);
    expect((c2.querySelector("input[type=checkbox]") as HTMLInputElement).checked).toBe(false);
  });

  it("bool onChange emits 'true' / 'false' (string)", () => {
    const onChange = vi.fn();
    const { container } = render(<FieldRenderer def={boolDef} value="false" onChange={onChange} />);
    const cb = container.querySelector("input[type=checkbox]")! as HTMLInputElement;
    fireEvent.click(cb);
    expect(onChange).toHaveBeenLastCalledWith("true");
  });

  it("int control respects min/max attributes from def", () => {
    const { container } = render(<FieldRenderer def={intDef} value="9125" onChange={() => {}} />);
    const input = container.querySelector("input[type=number]")! as HTMLInputElement;
    expect(input.min).toBe("1024");
    expect(input.max).toBe("65535");
  });

  it("path renders text input", () => {
    const { container } = render(<FieldRenderer def={pathDef} value="" onChange={() => {}} />);
    expect(container.querySelector("input[type=text]")).toBeTruthy();
  });

  it("disabled propagates to control + shows '(coming in A4-b)' for deferred", () => {
    const def = { ...enumDef, deferred: true };
    const { container, getByText } = render(<FieldRenderer def={def} value="system" onChange={() => {}} disabled />);
    expect((container.querySelector("select") as HTMLSelectElement).disabled).toBe(true);
    expect(getByText(/coming in A4-b/)).toBeTruthy();
  });

  it("inline error renders with role=alert and aria-describedby", () => {
    const { container } = render(<FieldRenderer def={enumDef} value="system" onChange={() => {}} error="bad value" />);
    const select = container.querySelector("select") as HTMLSelectElement;
    expect(select.getAttribute("aria-invalid")).toBe("true");
    expect(select.getAttribute("aria-describedby")).toBe("appearance.theme-error");
    const err = container.querySelector("[role=alert]");
    expect(err?.textContent).toBe("bad value");
  });
});
