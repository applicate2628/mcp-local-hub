import { execFileSync } from "node:child_process";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import {
  AUDIT_HANDOFFS,
  AUDIT_LOCK_AUTHORIZATIONS,
  AUDIT_LOCK_RECEIPT_STATUSES,
  AUDIT_LOCK_TERMINATION_STATES,
  DAEMON_RECOVER_ERROR_CODES,
  PORT_OWNER_CHECKS,
  PORT_WAIT_OUTCOMES,
} from "./api";

type RecoveryWireContract = Record<keyof typeof frontendFamilies, string[]>;

const EXPORTER_TIMEOUT_MS = 10_000;
const CONTRACT_TEST_TIMEOUT_MS = 15_000;

const frontendFamilies = {
  recovery_error_codes: DAEMON_RECOVER_ERROR_CODES,
  audit_lock_receipt_statuses: AUDIT_LOCK_RECEIPT_STATUSES,
  audit_lock_authorizations: AUDIT_LOCK_AUTHORIZATIONS,
  audit_lock_termination_states: AUDIT_LOCK_TERMINATION_STATES,
  port_owner_checks: PORT_OWNER_CHECKS,
  port_wait_outcomes: PORT_WAIT_OUTCOMES,
  audit_handoffs: AUDIT_HANDOFFS,
} as const;

function assertUnique(family: string, values: readonly string[], source: string): void {
  const duplicate = values.find((value, index) => values.indexOf(value) !== index);
  if (duplicate !== undefined) {
    throw new Error(`recovery-wire-contract duplicate value: ${family}; ${source}=${duplicate}`);
  }
}

export function assertRecoveryWireContractEqual(contract: RecoveryWireContract): void {
  const backendKeys = Object.keys(contract).sort();
  const frontendKeys = Object.keys(frontendFamilies).sort();
  if (backendKeys.join("\u0000") !== frontendKeys.join("\u0000")) {
    throw new Error(
      `recovery-wire-contract family mismatch; backend-only=[${backendKeys.filter((key) => !frontendKeys.includes(key)).join(",")}]; frontend-only=[${frontendKeys.filter((key) => !backendKeys.includes(key)).join(",")}]`,
    );
  }

  for (const family of frontendKeys as Array<keyof typeof frontendFamilies>) {
    const backend = contract[family];
    const frontend = frontendFamilies[family];
    assertUnique(family, backend, "backend");
    assertUnique(family, frontend, "frontend");
    const backendOnly = backend.filter((value) => !frontend.includes(value as never));
    const frontendOnly = frontend.filter((value) => !backend.includes(value));
    if (backendOnly.length > 0 || frontendOnly.length > 0) {
      throw new Error(
        `recovery-wire-contract drift: ${family}; backend-only=[${backendOnly.join(",")}]; frontend-only=[${frontendOnly.join(",")}]`,
      );
    }
  }
}

function readBackendContract(): RecoveryWireContract {
  const root = resolve(process.cwd(), "../../..");
  const raw = execFileSync("go", ["run", "./internal/gui/frontend/testdata/recoverywireexport"], {
    cwd: root,
    encoding: "utf8",
    timeout: EXPORTER_TIMEOUT_MS,
  });
  return JSON.parse(raw) as RecoveryWireContract;
}

describe("recovery wire contract", () => {
  it("matches all frontend boundary arrays exactly", () => {
    assertRecoveryWireContractEqual(readBackendContract());
  }, CONTRACT_TEST_TIMEOUT_MS);

  it("rejects a deterministic backend-only mutation", () => {
    const contract = readBackendContract();
    const mutated = {
      ...contract,
      recovery_error_codes: [...contract.recovery_error_codes, "__synthetic_backend_only__"],
    };
    expect(() => assertRecoveryWireContractEqual(mutated)).toThrow(
      "recovery-wire-contract drift: recovery_error_codes; backend-only=[__synthetic_backend_only__]; frontend-only=[]",
    );
  }, CONTRACT_TEST_TIMEOUT_MS);
});
