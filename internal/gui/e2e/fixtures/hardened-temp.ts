import { spawnSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import { chmodSync, mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { resolve } from "node:path";

export function hardenedTempHome(prefix: string): string {
  if (process.platform === "win32") {
    return createHardenedWindowsTempHome(prefix);
  }
  const home = mkdtempSync(resolve(tmpdir(), prefix));
  chmodSync(home, 0o700);
  return home;
}

function createHardenedWindowsTempHome(prefix: string): string {
  const base = process.env.MCPHUB_E2E_TMP_ROOT ?? tmpdir();
  // Some broadened temp roots allow child creation but deny WRITE_DAC, so the
  // owner-only DACL has to be attached at directory creation time.
  const script = `
& {
  $ErrorActionPreference = "Stop"
  $Path = $env:MCPHUB_E2E_HARDEN_DIR
  $acl = New-Object System.Security.AccessControl.DirectorySecurity
  $acl.SetAccessRuleProtection($true, $false)
  $current = [System.Security.Principal.WindowsIdentity]::GetCurrent().User
  $system = New-Object System.Security.Principal.SecurityIdentifier "S-1-5-18"
  $admins = New-Object System.Security.Principal.SecurityIdentifier "S-1-5-32-544"
  $rights = [System.Security.AccessControl.FileSystemRights]::FullControl
  $inheritance = [System.Security.AccessControl.InheritanceFlags]"ContainerInherit, ObjectInherit"
  $propagation = [System.Security.AccessControl.PropagationFlags]::None
  $type = [System.Security.AccessControl.AccessControlType]::Allow
  foreach ($sid in @($current, $system, $admins)) {
    $rule = New-Object System.Security.AccessControl.FileSystemAccessRule($sid, $rights, $inheritance, $propagation, $type)
    $acl.AddAccessRule($rule)
  }
  [System.IO.Directory]::CreateDirectory($Path, $acl) | Out-Null
}
`;

  for (let attempt = 0; attempt < 10; attempt++) {
    const home = resolve(base, `${prefix}${randomBytes(6).toString("hex")}`);
    const result = spawnSync(
      "powershell.exe",
      ["-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", script],
      {
        encoding: "utf8",
        env: { ...process.env, MCPHUB_E2E_HARDEN_DIR: home },
      },
    );
    if (result.status === 0) {
      return home;
    }
    const output = result.stderr || result.stdout;
    if (!/already exists/i.test(output)) {
      throw new Error(`failed to create hardened e2e temp home ${home}: ${output}`);
    }
  }
  throw new Error("failed to create unique hardened e2e temp home");
}
