// toast-store — a tiny module-level pub/sub store for transient toast
// notifications, decoupled from any single screen. Any screen calls
// pushToast(...) to enqueue a toast; the ToastContainer (mounted once in
// app.tsx) subscribes and renders the live list with Flowbite Toast markup.
//
// Why a module store rather than React context: toasts are fired from
// deeply-nested handlers (Servers Apply, AddServer Save/Install) that have
// no shared ancestor besides App. A module-level store lets any code emit a
// toast with a single function call — no prop drilling, no provider — and
// keeps the emit-site free of presentational concerns. This mirrors the
// existing "backend is canonical, UI is a projection" pattern used by the
// Dashboard SSE state: pushToast is the event, the container is the view.
//
// The store is intentionally framework-agnostic (no Preact import) so it is
// unit-testable in isolation and so the emit-sites don't pull in render code.

export type ToastVariant = "success" | "danger" | "warning" | "info";

export interface Toast {
  // Monotonic id used as the render key and the dismiss handle. Stable for
  // the lifetime of the toast so a re-render never re-keys an existing one.
  id: number;
  variant: ToastVariant;
  message: string;
  // Auto-dismiss delay in ms. 0 (or negative) disables auto-dismiss so the
  // toast stays until the operator clicks the close button — used for
  // error/danger toasts the operator should acknowledge. Defaults applied by
  // pushToast per variant.
  timeoutMs: number;
}

type Listener = (toasts: readonly Toast[]) => void;

// Module-singleton state. `toasts` is the live ordered list (oldest first);
// `listeners` are the subscribed views (normally just the one container).
let toasts: Toast[] = [];
const listeners = new Set<Listener>();
let nextId = 1;
// Tracks the per-toast auto-dismiss timers so dismiss()/clearAllToasts() can
// cancel a pending fire and avoid a setState-after-removal no-op.
const timers = new Map<number, ReturnType<typeof setTimeout>>();

// Default auto-dismiss windows per variant. Success/info are transient
// (4 s); warnings linger a touch longer (6 s); danger never auto-dismisses
// (the operator must acknowledge an error explicitly) — same "fail-loud,
// operator-visible" posture the Dashboard degraded banner uses.
const DEFAULT_TIMEOUTS: Record<ToastVariant, number> = {
  success: 4000,
  info: 4000,
  warning: 6000,
  danger: 0,
};

function emit(): void {
  // Hand listeners a fresh array reference so a Preact useState setter sees a
  // new value and re-renders (Object.is bail-out otherwise).
  const snapshot = toasts.slice();
  for (const l of listeners) l(snapshot);
}

// pushToast enqueues a toast and returns its id. `options.timeoutMs`
// overrides the per-variant default (pass 0 to make it sticky). The auto-
// dismiss timer is armed here, in the store, so emit-sites never manage
// timers and every toast (regardless of source) ages out consistently.
export function pushToast(
  variant: ToastVariant,
  message: string,
  options?: { timeoutMs?: number },
): number {
  const id = nextId++;
  const timeoutMs = options?.timeoutMs ?? DEFAULT_TIMEOUTS[variant];
  toasts = [...toasts, { id, variant, message, timeoutMs }];
  emit();
  if (timeoutMs > 0) {
    const t = setTimeout(() => dismissToast(id), timeoutMs);
    timers.set(id, t);
  }
  return id;
}

// dismissToast removes one toast by id (idempotent — dismissing an
// already-gone id is a no-op) and cancels its pending auto-dismiss timer.
export function dismissToast(id: number): void {
  const t = timers.get(id);
  if (t) {
    clearTimeout(t);
    timers.delete(id);
  }
  const before = toasts.length;
  toasts = toasts.filter((x) => x.id !== id);
  if (toasts.length !== before) emit();
}

// subscribe registers a listener and immediately pushes the current
// snapshot so a freshly-mounted container paints existing toasts. Returns an
// unsubscribe function for useEffect cleanup.
export function subscribeToasts(listener: Listener): () => void {
  listeners.add(listener);
  listener(toasts.slice());
  return () => {
    listeners.delete(listener);
  };
}

// clearAllToasts drops every toast and cancels every timer. Used by tests
// for isolation; safe to call anytime.
export function clearAllToasts(): void {
  for (const t of timers.values()) clearTimeout(t);
  timers.clear();
  toasts = [];
  emit();
}
