// Discriminated union per memo §8.2 (Codex r1 P2.2 + r2 P1.2). Action
// entries omit `value` and `default` to match the wire shape from
// internal/gui/settings.go::settingDTO.MarshalJSON.

export type Section = "appearance" | "gui_server" | "daemons" | "backups" | "advanced";

type BaseSettingDTO = {
  key: string;
  section: Section;
  deferred: boolean;
  help: string;
};

export type ConfigSettingDTO = BaseSettingDTO & {
  type: "enum" | "bool" | "int" | "string" | "path";
  default: string;
  value: string;
  enum?: string[];
  min?: number;
  max?: number;
  pattern?: string;
  optional?: boolean;
};

export type ActionSettingDTO = BaseSettingDTO & {
  type: "action";
};

export type SettingDTO = ConfigSettingDTO | ActionSettingDTO;

export type SettingsEnvelope = {
  settings: SettingDTO[];
  actual_port: number;
};

export type APIError = { code?: string; message: string };

export type SettingsSnapshotState =
  | { status: "loading"; data: null; error: null }
  | { status: "ok"; data: SettingsEnvelope; error: null }
  | { status: "error"; data: null; error: APIError | Error };

export type SettingsSnapshot = SettingsSnapshotState & {
  refresh: () => Promise<void>;
};

export function isAction(s: SettingDTO): s is ActionSettingDTO {
  return s.type === "action";
}

export function isConfig(s: SettingDTO): s is ConfigSettingDTO {
  return s.type !== "action";
}

export type BackupInfo = {
  client: string;
  path: string;
  kind: "original" | "timestamped";
  mod_time: string;
  size_byte: number;
};
