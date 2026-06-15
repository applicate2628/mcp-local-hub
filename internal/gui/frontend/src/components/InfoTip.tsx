import { useState, useId, useRef, useEffect } from "preact/hooks";

export type InfoTipProps = {
  /** The help text to reveal. */
  text: string;
  /** Accessible label for the trigger button. */
  label?: string;
  /**
   * Optional test id forwarded to the trigger button. Lets callers that
   * moved an inline prose block into this popover keep their coverage
   * anchored on a stable selector (the prose is reachable by opening the
   * popover or by reading the trigger's title attribute).
   */
  "data-testid"?: string;
};

// InfoTip — a small "ⓘ" affordance that reveals long help text in a floating
// popover ON DEMAND instead of dumping a wall of muted prose inline under
// every field. Native Preact + ARIA — deliberately NOT Radix/@material/shadcn:
// those are React + Tailwind component libraries that would need a
// preact/compat shim with real portal/ref/context risk; a tooltip is simple
// enough to own. Styling rides the app's design tokens (var(--…)) re-exported
// to Tailwind via @theme, so it follows the live light/dark palette.
//
// Two open modes, ONE shared component (the "минимум дублирования" the user
// asked for — fixing it here fixes every ⓘ across Catalog cards,
// MatrixColumnsMenu, Migration, Settings, …):
//
//   - HOVER PREVIEW (mouse only): pointer over the trigger OR the popover
//     opens it transiently; moving away closes it. A keyboard focus also
//     previews. This is the cheap, ephemeral path.
//   - CLICK PIN (the explicit ask): clicking the trigger TOGGLES a pinned
//     state. closed → open-and-pinned; open → close. While pinned the
//     popover stays up regardless of mouseleave/blur until: another trigger
//     click, a click outside, or Escape.
//
// `pinned` is the authoritative latch; `hovering` is the transient preview.
// The popover renders when EITHER is set, but only `pinned` survives a
// mouseleave/blur — so a click both OPENS and CLOSES (the explicit
// requirement), and a hover can't accidentally dismiss a pinned popover.
export function InfoTip({
  text,
  label = "More info",
  "data-testid": dataTestid,
}: InfoTipProps): preact.JSX.Element {
  const [pinned, setPinned] = useState(false);
  const [hovering, setHovering] = useState(false);
  const open = pinned || hovering;
  const rawId = useId();
  const id = `infotip-${rawId.replace(/[:]/g, "")}`;
  const wrapRef = useRef<HTMLSpanElement>(null);

  // Escape + click-outside DISMISS the pinned popover (and clear any hover
  // preview). Only wired while pinned so we never leak global listeners for a
  // mere hover. mousedown (not click) so the outside-dismiss fires before a
  // re-open click handler on another trigger can re-toggle.
  useEffect(() => {
    if (!pinned) return;
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") {
        setPinned(false);
        setHovering(false);
      }
    }
    function onDoc(e: MouseEvent) {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) {
        setPinned(false);
        setHovering(false);
      }
    }
    document.addEventListener("keydown", onKey);
    document.addEventListener("mousedown", onDoc);
    return () => {
      document.removeEventListener("keydown", onKey);
      document.removeEventListener("mousedown", onDoc);
    };
  }, [pinned]);

  return (
    <span
      ref={wrapRef}
      class="infotip relative inline-flex items-center align-middle"
      // Hover PREVIEW (mouse): open while the pointer is over trigger OR
      // popover (both live inside this wrapper), so moving onto the popover
      // keeps it up. This only flips the transient `hovering` flag — a pinned
      // popover is unaffected by the pointer leaving.
      onMouseEnter={() => setHovering(true)}
      onMouseLeave={() => setHovering(false)}
    >
      <button
        type="button"
        class="infotip-trigger inline-flex h-[15px] w-[15px] items-center justify-center rounded-full border border-app-border text-[10px] font-semibold leading-none text-app-muted transition-colors hover:border-app-accent hover:text-app-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-app-accent/50"
        aria-label={label}
        // aria-expanded reflects the COMBINED open state (pinned or hovering)
        // so assistive tech always sees whether the popover is currently
        // visible, matching what a sighted user perceives.
        aria-expanded={open}
        aria-describedby={open ? id : undefined}
        // Native tooltip mirror of the popover prose: keeps the help text
        // reachable on a long-hover even before the popover paints, and gives
        // moved-inline-description callers a stable text surface to assert.
        title={text}
        data-testid={dataTestid}
        // CLICK toggles the PINNED latch — open→close, close→open. Clearing
        // `hovering` on the close branch guarantees a click can actually close
        // even while the pointer is still over the trigger (otherwise the
        // hover preview would immediately re-open it).
        onClick={() =>
          setPinned((p) => {
            const next = !p;
            if (!next) setHovering(false);
            return next;
          })
        }
        // Keyboard PREVIEW: focus opens transiently; blur clears the preview
        // but never the pin (Tab away from a pinned popover keeps it up until
        // Escape / outside-click / re-click — matches mouse pin semantics).
        onFocus={() => setHovering(true)}
        onBlur={() => setHovering(false)}
      >
        i
      </button>
      {open ? (
        // Two nodes on purpose: the ANCHOR owns the horizontal centering
        // (-translate-x-1/2, a STATIC transform never touched by the
        // animation), the inner POP owns the fade+slide (opacity + translateY
        // only). Decoupling them means the X-centering can never animate, so
        // the popover cannot "drift in from the side". The keyframe also runs
        // with fill-mode:both (see .infotip-pop) so frame 0 is opacity:0 —
        // any first-paint width-settle of w-max happens while invisible.
        <span class="infotip-anchor absolute left-1/2 top-[calc(100%+8px)] z-50 -translate-x-1/2">
          <span
            id={id}
            role="tooltip"
            class="infotip-pop block w-max max-w-[18rem] rounded-lg border border-app-border bg-app-card px-3 py-2 text-left text-xs font-normal leading-relaxed text-app-text shadow-xl"
          >
            {text}
          </span>
        </span>
      ) : null}
    </span>
  );
}
