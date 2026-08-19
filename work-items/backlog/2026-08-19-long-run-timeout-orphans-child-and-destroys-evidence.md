# Backlog — a timed-out call orphans the child and destroys its output

- Filed: 2026-08-19 from the VFEM Fortran wave-port validation work.
- Server: `oneapi-run` (and any server that runs a long child through a pipe).
- Status: candidate
- Priority: P1 — it does not merely fail a call, it **destroys work that already succeeded**.
- No-epic rationale: one contract on one tool surface.

## What happens

A solve was launched through the tool with `timeout_sec=3600`. The call returned
`The operation timed out.` and **no log at all**.

The child was **not killed** — it kept running to completion, holding ~12 GB, and finished
its computation. But its stdout was bound to the **caller's pipe**, and the caller was gone.
So the numbers were computed and then **discarded**.

That is the worst of the three possible outcomes:

| outcome | cost |
|---|---|
| child killed at timeout | the compute is lost — bad, but honest |
| child survives, output preserved | ideal |
| **child survives, output destroyed** | **the compute is spent AND unrecoverable** |

## Why it bit hard

These runs are **hours** long. A 20-point sweep took 4 h 24 min; a single-point diagnostic
takes 16–26 min. On work of that length a timeout is not an edge case — it is the normal
outcome of any wrapper whose ceiling is shorter than the job.

The failure is also **silent in a specific way**: an empty log is indistinguishable from a
run that has not flushed yet, so a caller cannot tell "destroyed" from "still working"
without probing the process independently. Two sessions each spent a cycle on that
ambiguity before it was understood.

## What is asked

**Do not bind a long child's output to the call's lifetime.** Concretely, in order of
preference:

1. **Write the child's output to a file the caller names**, so a timeout at the caller costs
   nothing. The run keeps writing; the caller reads when it can.
2. If a pipe must be used, **stream it to disk incrementally** rather than buffering to
   deliver at return.
3. **Say which happened.** A timeout should report whether the child was killed or left
   running, and where its output went. Right now the caller learns neither.

**Not asked: killing the child on timeout.** That would be honest but wasteful — and for a
four-hour solve, expensive. Preserving the output is strictly better than tidying up.

## What was NOT verified

Whether the tool's effective ceiling is the requested `timeout_sec` or a lower internal cap
— `ASSUMPTION (UNVERIFIED)`. The observed run exceeded 600 s while 3600 was requested, but
the exact ceiling was never probed. *Resolving step:* launch a child that prints a timestamp
each minute and observe where the call returns.

Whether other servers that spawn children share the shape is also unchecked; this entry is
filed against the one where it was observed.

## Terms and Abbreviations

- **orphaned child** — a spawned process that outlives the call that started it.
- **pipe-bound output** — stdout delivered through the caller's channel rather than a file;
  it exists only while a reader exists.
- **flush** — the point at which buffered output actually reaches disk.
