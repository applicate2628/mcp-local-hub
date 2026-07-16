# Vision: clean-install / end-user UX overhaul (FUTURE — after dev-variant is debugged)

Date filed: 2026-06-18 (user vision dump)
Status: PARKED — "это все будем делать, когда отладим наш текущий dev-вариант". Do NOT
implement now; capture so it is not lost. This is the productization / onboarding track.

## The thesis
The current surface is a power-user / agent / dev surface (rich CLI subcommands, headless
flags, manual workspace registration). For a real END USER a clean install is full of
user-friendliness problems. The end-user product should be: **run the exe → the GUI opens
immediately + a tray icon appears + it is obvious what to do → click Install → it just
installs what is needed.** The console surface should be largely hidden from that user.

## Captured problems / requirements

1. **Hide the console functions from end users.** The CLI subcommands are for agents/devs.
   Exposing them to end users invites mistakes. Agents in particular love to reach for
   `--headless` / detached modes. The end-user path should be GUI+tray only; the CLI stays
   available for agents/devs but is not the surface a normal user is steered to.

2. **Name-collision hazard (REAL incident).** An agent once mistakenly installed a DIFFERENT
   vendor's `mcphub` (a name collision — some other package/binary also named `mcphub`). The
   product must disambiguate: a distinct install identity / verified-publisher check / a
   collision guard so "install mcphub" can't grab the wrong `mcphub`. (Our npm meta is
   `mcp-local-hub`; the command is `mcphub` — the collision is on the bare `mcphub` name.)

3. **exe → auto-GUI + tray + onboarding.** Double-clicking the exe with no args should open
   the GUI, show the tray, and present a clear first-run onboarding ("here's what mcphub does,
   click Install to set up your clients"). One Install button that does the right thing.

4. **language-server install is broken for an end user.** `install` on the language-server
   does NOT install it because it REQUIRES a workspace, and WHERE/HOW the user registers a
   workspace is unclear. The GUI must make workspace registration obvious (or auto-derive a
   sensible default) so language-server / serena-style workspace-scoped servers install
   cleanly from the GUI. (Related: the workspace-scoped-daemon UX is currently CLI-driven.)

5. **Vendor Initialize not available for not-yet-installed clients** — see
   `work-items/2026-06-18-vendor-init-uninstalled-clients.md` (G17). Part of the same
   clean-install friction.

6. **General clean-install friction** — many small user-friendly problems surface only on a
   truly empty machine (no client configs, no parent dirs, nothing installed). The whole
   first-run path needs a dedicated pass on a CLEAN VM/container.

## Relationship to the roadmap
This is the **productization / §D-adjacent** track (the GUI-store / onboarding / commercial
direction). It is GATED behind "the current dev variant works + is debugged" per the user.
The current GUI-polish initiative (G1-G17) is the dev-variant debugging; THIS vision is the
next macro-phase after it. Do NOT start it until the user says the dev variant is solid.

## When we DO start it (checklist seed)
- First-run onboarding screen + "Install" one-click flow.
- Auto-GUI-on-bare-exe (already partly: cmd/mcphub/main.go defaults no-args to gui) + tray-first UX.
- Hide/deprioritize CLI in the end-user packaging; keep it for agents/devs.
- Name-collision / publisher-verification guard.
- Workspace registration made obvious in the GUI (language-server / serena).
- A clean-VM first-run test pass.
