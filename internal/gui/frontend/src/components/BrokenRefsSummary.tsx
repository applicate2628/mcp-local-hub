import type { VaultState } from "../lib/secrets-api";

export interface BrokenRefsSummaryProps {
  vaultState: VaultState;
  brokenRefs: string[]; // vault key names referenced but not present (only meaningful when vaultState === "ok")
}

export function BrokenRefsSummary(props: BrokenRefsSummaryProps) {
  const { vaultState, brokenRefs } = props;

  // Memo §5.3 / D3: render nothing when vault is ok and at most 1 broken ref.
  if (vaultState === "ok" && brokenRefs.length <= 1) return null;

  let message: string;
  let icon: string;
  if (vaultState !== "ok") {
    icon = "🔒";
    if (vaultState === "decrypt_failed") {
      message = "Vault not readable (decrypt_failed). Cannot verify any secret: references. Fix vault on Secrets screen first.";
    } else if (vaultState === "missing") {
      message = "Vault not initialized. Open Secrets screen to create one.";
    } else {
      message = "Vault file corrupted. Open Secrets screen to recover.";
    }
  } else {
    icon = "⚠";
    const list = brokenRefs.join(", ");
    message = `${brokenRefs.length} secrets referenced but not in vault: ${list}. Daemons will fail to start.`;
  }

  return (
    <div class="secret-broken-summary" role="status" aria-live="polite">
      <span class="secret-broken-summary-icon" aria-hidden="true">{icon}</span>
      <span class="secret-broken-summary-text">{message}</span>
    </div>
  );
}
