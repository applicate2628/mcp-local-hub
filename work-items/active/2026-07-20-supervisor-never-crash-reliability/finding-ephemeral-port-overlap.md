# Finding — Windows ephemeral port range fully overlaps the mcphub daemon pools

Discovered 2026-07-20 (surfaced by `mcphub setup`'s own warning on the operator's
host, then confirmed directly). **Host configuration, not a code defect** — but it
is the largest single historical source of daemon crash-looping on this machine.

## Measured on the operator's host

```
> netsh int ipv4 show dynamicport tcp
Start Port      : 1024
Number of Ports : 13977          → dynamic range 1024-15000
```

mcphub daemon pools:

| Pool | Range | Can a daemon move off a taken port? |
|---|---|---|
| Global daemons | 9121-9149 | **NO — fixed port** |
| Dynamic pool | 9150-9199 | yes (realloc) |
| LSP / workspace pool | 9400-9599 | yes (realloc) |

**All three lie entirely inside 1024-15000.** Windows may therefore hand any pool
port to an unrelated process for an outbound connection.

## Mechanism

A foreign process gets an ephemeral port that happens to be, say, 9123 (memory).
When the supervisor spawns that daemon, the bind is refused
(`WSAEACCES` 10013 / `WSAEADDRINUSE` 10048). A **dynamic-pool** daemon self-heals
by moving to a fresh port. A **fixed-port global daemon (9121-9149) cannot move**
and crash-loops until the squatter releases the port — which is outside anyone's
control.

`mcphub setup`'s own warning states this exactly:

> the OS can hand a pool port to a foreign process, so a daemon's bind is refused
> (WSAEACCES/WSAEADDRINUSE). mcphub self-heals dynamic-pool daemons by moving them
> to a fresh port, but a FIXED-port global daemon (9121-9149) cannot move and will
> crash-loop until the port frees.

## Why this is the big one

L2's lifetime failure census over all 2,969 recorded launch failures:

| Mechanism | Total | Last seen |
|---|---|---|
| **PORT-IN-USE (bind collision on respawn)** | **2072 (70%)** | 2026-07-10 |
| STDIO CHILD DIED | 267 | 2026-07-19 |
| UPSTREAM-NOT-READY-30s | 203 | 2026-07-19 |
| PORT WSAEACCES | 55 | 2026-07-12 |

Port-in-use was **70% of every launch failure ever recorded** and was
self-amplifying (respawn → port still held → fail → respawn). It collapsed after
2026-07-10, consistent with the `exitBindRefused`(3) + dynamic-pool realloc
self-heal (`internal/cli/daemon_bind_refused.go:9-39`) — but that self-heal
**cannot help the fixed 9121-9149 range**, which is where the always-on global
daemons (memory, time, fetch, sequential-thinking, …) live.

## Remedy — requires an ELEVATED shell (operator action; the agent cannot do it)

```powershell
# in a shell started with "Run as administrator"
mcphub setup --fix-ephemeral-range
```

Moves the Windows dynamic range above the pools.

**Do NOT** use `netsh add excludedportrange` on the pools — mcphub's allocator
would then treat the pool as unusable. `setup`'s warning says this explicitly.

Manual equivalent (admin): `netsh int ipv4 set dynamicport tcp start=<above-pools> num=<n>`.

## Applies to the operator's laptop too — untested but high prior

1024-15000 is the Windows default, so the same overlap is expected on any
unconfigured host. This is one of the two independent, plausible explanations for
the laptop incident ("ran ~15 min, daemon quarantined, restart does not help") —
the other being the serena 30s/120s readiness-timeout inversion, which a slower
laptop would hit harder. Both are consistent with the report; neither is confirmed
without the laptop's own `supervisor-events.log`.

**Discriminator to run on the laptop:** if `daemon-quarantined` is preceded by
`daemon-exited` with a bind-refused signature or `WSAEACCES`/`WSAEADDRINUSE` in the
per-daemon log, it is this finding. If preceded by `upstream not ready after 30s`,
it is the serena timeout inversion.

## Product follow-up (separate from the host fix)

The remedy exists but is **discoverable only as a warning printed during `setup`**
— an operator who ran setup once months ago will never see it again, and nothing
surfaces the overlap in the GUI or in `mcphub status`. Given that the condition
caused 70% of all historical launch failures, a persistent surface is warranted:
a startup check that emits a `warn` event and a GUI Dashboard badge while the
ranges overlap, cleared automatically once they do not. Candidate for the same
observability batch as L4's N1-N3.
