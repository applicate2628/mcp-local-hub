import { useEffect, useRef, useState } from "preact/hooks";
import {
  postAdopt,
  postAdoptPlan,
  type APIError,
  type AdoptPlan,
  type AdoptRequest,
} from "../api";
import { pushToast } from "../lib/toast-store";

export interface AdoptionTarget {
  entry: string;
  client: string;
  projectSourceRoot?: string;
}

export interface AdoptionFlowError {
  key: string;
  code: string;
  message: string;
}

interface ActiveAdoption {
  key: string;
  target: AdoptionTarget;
  plan: AdoptPlan;
}

export interface AdoptionFlowController {
  busyKey: string | null;
  actionError: AdoptionFlowError | null;
  active: ActiveAdoption | null;
  consent: boolean;
  confirmBusy: boolean;
  replanning: boolean;
  modalError: AdoptionFlowError | null;
  start: (target: AdoptionTarget) => Promise<void>;
  retry: () => Promise<void>;
  setConsent: (next: boolean) => void;
  cancel: () => void;
  confirm: () => Promise<void>;
  retryModal: () => Promise<void>;
}

interface UseAdoptionFlowOptions {
  onSuccess: () => void;
  onPlanError?: (error: AdoptionFlowError) => void;
  mountedRef?: { current: boolean };
}

export function adoptionActionKey(entry: string, projectSourceRoot?: string): string {
  if (!projectSourceRoot) return `adopt:${entry}`;
  return `adopt:${JSON.stringify([projectSourceRoot, entry])}`;
}

function requestForTarget(target: AdoptionTarget): AdoptRequest {
  return {
    entry: target.entry,
    client: target.client,
    ...(target.projectSourceRoot
      ? { project_source_root: target.projectSourceRoot }
      : {}),
  };
}

function adoptionError(error: unknown, key: string, prefix = ""): AdoptionFlowError {
  const apiError = error as APIError;
  const message = error instanceof Error ? error.message : String(error);
  return {
    key,
    code: apiError.code || "ADOPT_FAILED",
    message: `${prefix}${message}`,
  };
}

export function useAdoptionFlow({
  onSuccess,
  onPlanError,
  mountedRef,
}: UseAdoptionFlowOptions): AdoptionFlowController {
  const [busyKey, setBusyKey] = useState<string | null>(null);
  const [actionError, setActionError] = useState<AdoptionFlowError | null>(null);
  const [active, setActive] = useState<ActiveAdoption | null>(null);
  const [consent, setConsent] = useState(false);
  const [confirmBusy, setConfirmBusy] = useState(false);
  const [replanning, setReplanning] = useState(false);
  const [modalError, setModalError] = useState<AdoptionFlowError | null>(null);
  const retryTargetRef = useRef<AdoptionTarget | null>(null);
  const ownMountedRef = useRef(true);
  const requestRevisionRef = useRef(0);
  const planningRef = useRef(false);
  const confirmingRef = useRef(false);

  useEffect(() => {
    ownMountedRef.current = true;
    return () => {
      ownMountedRef.current = false;
      requestRevisionRef.current += 1;
    };
  }, []);

  const isMounted = () => ownMountedRef.current && (mountedRef?.current ?? true);

  async function start(target: AdoptionTarget): Promise<void> {
    if (planningRef.current || confirmingRef.current) return;
    planningRef.current = true;
    retryTargetRef.current = target;
    const key = adoptionActionKey(target.entry, target.projectSourceRoot);
    const revision = requestRevisionRef.current + 1;
    requestRevisionRef.current = revision;
    setBusyKey(key);
    setActionError(null);
    setModalError(null);
    try {
      const plan = await postAdoptPlan(requestForTarget(target));
      if (!isMounted() || requestRevisionRef.current !== revision) return;
      setActive({ key, target, plan });
      setConsent(false);
    } catch (error) {
      if (!isMounted() || requestRevisionRef.current !== revision) return;
      const nextError = adoptionError(error, key, `Adopt ${target.entry}: `);
      setActionError(nextError);
      onPlanError?.(nextError);
    } finally {
      planningRef.current = false;
      if (isMounted() && requestRevisionRef.current === revision) setBusyKey(null);
    }
  }

  async function retry(): Promise<void> {
    const target = retryTargetRef.current;
    if (target) await start(target);
  }

  function cancel(): void {
    if (confirmingRef.current) return;
    requestRevisionRef.current += 1;
    setActive(null);
    setConsent(false);
    setModalError(null);
  }

  async function confirm(): Promise<void> {
    if (!active || confirmingRef.current || planningRef.current) return;
    if (
      active.target.projectSourceRoot &&
      !active.plan.project_source_preimage_token?.trim()
    ) {
      setModalError({
        key: active.key,
        code: "CODEX_PROJECT_SOURCE_PREVIEW_REQUIRED",
        message: "The project source preview is missing. Retry to refresh it before adopting.",
      });
      return;
    }
    confirmingRef.current = true;
    setConfirmBusy(true);
    setModalError(null);
    const { key, target, plan } = active;
    const revision = requestRevisionRef.current + 1;
    requestRevisionRef.current = revision;
    const request: AdoptRequest = {
      entry: plan.EntryName,
      client: plan.SourceClient,
      clients: plan.AdoptClients,
      name: plan.ManifestName,
      port: plan.Port,
      ...(target.projectSourceRoot
        ? {
            project_source_root: target.projectSourceRoot,
            project_source_preimage_token: plan.project_source_preimage_token,
          }
        : {}),
    };
    try {
      await postAdopt({
        ...request,
        symlink_consent: consent ? plan.symlink_targets : [],
      });
      if (!isMounted() || requestRevisionRef.current !== revision) return;
      pushToast("success", `Adopted ${plan.ManifestName} into hub.`);
      setActive(null);
      setConsent(false);
      setActionError(null);
      onSuccess();
    } catch (error) {
      if (!isMounted() || requestRevisionRef.current !== revision) return;
      const apiError = error as APIError;
      if (apiError.code === "SYMLINK_CONSENT_REQUIRED") {
        setModalError({
          key,
          code: apiError.code,
          message: "Symlink consent is required before adopting this entry.",
        });
        try {
          const refreshed = await postAdoptPlan(
            target.projectSourceRoot ? requestForTarget(target) : request,
          );
          if (!isMounted() || requestRevisionRef.current !== revision) return;
          setActive({ key, target, plan: refreshed });
          setConsent(false);
        } catch (refreshError) {
          if (!isMounted() || requestRevisionRef.current !== revision) return;
          setModalError(adoptionError(refreshError, key));
        }
        return;
      }
      setModalError(adoptionError(error, key));
    } finally {
      confirmingRef.current = false;
      if (isMounted() && requestRevisionRef.current === revision) setConfirmBusy(false);
    }
  }

  async function retryModal(): Promise<void> {
    if (!active || planningRef.current || confirmingRef.current) return;
    planningRef.current = true;
    setReplanning(true);
    setModalError(null);
    const { key, target } = active;
    const revision = requestRevisionRef.current + 1;
    requestRevisionRef.current = revision;
    try {
      const plan = await postAdoptPlan(requestForTarget(target));
      if (!isMounted() || requestRevisionRef.current !== revision) return;
      setActive({ key, target, plan });
      setConsent(false);
    } catch (error) {
      if (!isMounted() || requestRevisionRef.current !== revision) return;
      setModalError(adoptionError(error, key));
    } finally {
      planningRef.current = false;
      if (isMounted() && requestRevisionRef.current === revision) setReplanning(false);
    }
  }

  return {
    busyKey,
    actionError,
    active,
    consent,
    confirmBusy,
    replanning,
    modalError,
    start,
    retry,
    setConsent,
    cancel,
    confirm,
    retryModal,
  };
}

export function AdoptionActionError({ flow }: { flow: AdoptionFlowController }) {
  if (!flow.actionError) return null;
  return (
    <p class="settings-error" role="alert">
      <code>{flow.actionError.code}</code>: {flow.actionError.message}{" "}
      <button type="button" class="btn text-xs" onClick={() => void flow.retry()}>
        Retry
      </button>
    </p>
  );
}

export function AdoptionFlowModal({ flow }: { flow: AdoptionFlowController }) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const { active } = flow;
  useEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog) return;
    if (active && !dialog.open) {
      dialog.showModal();
    } else if (!active && dialog.open) {
      dialog.close();
    }
  }, [active]);
  if (!active) return null;

  const { plan } = active;
  const symlinkTargets = plan.symlink_targets ?? [];
  const needsSymlinkConsent = symlinkTargets.length > 0;
  const projectRetry = Boolean(active.target.projectSourceRoot && flow.modalError);
  return (
    <dialog
      key={active.key}
      ref={dialogRef}
      data-testid="adopt-confirm-modal"
      onCancel={(event) => {
        event.preventDefault();
        flow.cancel();
      }}
    >
      <h2>Adopt into hub</h2>
      <div class="adopt-plan-summary">
        <p>Manifest: {plan.ManifestName}</p>
        <p>Port: {plan.Port}</p>
        <section>
          <h3>Clients to repoint</h3>
          <ul>
            {plan.AdoptClients.map((client) => (
              <li key={client}>{client}</li>
            ))}
          </ul>
        </section>
        {(plan.AlsoPresent.length > 0 ||
          plan.SignatureMismatches.length > 0 ||
          plan.DisabledSameName.length > 0) && (
          <section>
            <h3>Excluded same-name clients</h3>
            <ul>
              {plan.AlsoPresent.map((client) => (
                <li key={`also-${client}`}>{client}: also present, not selected</li>
              ))}
              {plan.SignatureMismatches.map((mismatch) => (
                <li key={`mismatch-${mismatch.Client}`}>
                  {mismatch.Client}: {mismatch.Reason}
                </li>
              ))}
              {plan.DisabledSameName.map((disabled) => (
                <li key={`disabled-${disabled.Client}`}>{disabled.Client}: disabled</li>
              ))}
            </ul>
          </section>
        )}
        {plan.SecretRoutedKeys.length > 0 && (
          <section>
            <h3>Secret-routed keys</h3>
            <ul>
              {plan.SecretRoutedKeys.map((key) => (
                <li key={key}>{key}</li>
              ))}
            </ul>
          </section>
        )}
        {needsSymlinkConsent && (
          <section>
            <h3>Symlink write consent</h3>
            <ul>
              {symlinkTargets.map((target) => (
                <li key={`${target.client}:${target.resolved_path}`}>
                  {target.client} config is a symlink -&gt; {target.resolved_path}; adopting writes
                  through the resolved target.
                </li>
              ))}
            </ul>
            <label class="adopt-symlink-consent">
              <input
                type="checkbox"
                checked={flow.consent}
                onChange={(event) =>
                  flow.setConsent((event.currentTarget as HTMLInputElement).checked)
                }
              />
              <span>I understand and consent to writing through the resolved target.</span>
            </label>
          </section>
        )}
      </div>
      {flow.modalError &&
        (active.target.projectSourceRoot ? (
          <p class="error" role="alert">
            <code>{flow.modalError.code}</code>: {flow.modalError.message}
          </p>
        ) : (
          <p class="error">{flow.modalError.message}</p>
        ))}
      <menu>
        <button type="button" onClick={flow.cancel} disabled={flow.confirmBusy || flow.replanning}>
          Cancel
        </button>
        <button
          type="button"
          class="primary"
          data-action="confirm-adopt"
          disabled={
            flow.confirmBusy ||
            flow.replanning ||
            (needsSymlinkConsent && !flow.consent)
          }
          onClick={() => void (projectRetry ? flow.retryModal() : flow.confirm())}
        >
          {flow.replanning
            ? "Planning..."
            : flow.confirmBusy
              ? "Adopting..."
              : projectRetry
                ? "Retry"
                : "Adopt into hub"}
        </button>
      </menu>
    </dialog>
  );
}
