# ADR 007: What happens when a lease expires

**Status:** Proposed  
**Date:** 2026-10-06  
**Spec sections:** 6 (T12, T13), 7 (reaper), 8 (retry policy)  
**Depends on:** ADR 003 (lease), ADR 004 (backoff)  

## Context

When a lease runs out, the reaper takes the job back (T12), or marks it dead if attempts are used up (T13). The question is when the job becomes available again: right away, or after a delay like a failed job gets.

A lease can expire for two very different reasons:

- The worker is healthy but was slow for a moment (a pause, a network blip). Nothing is wrong with the job.
- The job itself kills the worker, for example by running it out of memory. Every worker that picks it up dies.

The engine can't tell these apart at the time of expiry.

## Options

### A. Requeue immediately (run_at = now())

**For**

- Fastest recovery: nothing waits beyond the lease and the reaper interval

**Against**

- A job that kills its worker is picked up again in a tight loop, killing a worker each time until its attempts run out

### B. Requeue with the same backoff as a failure (chosen)

**For**

- A job that keeps killing workers is slowed down the same way a failing job is, and the attempt limit ends the loop in dead
- One rule for all retries, so one function to write and test

**Against**

- A healthy job that lost its lease for a moment waits an extra 2 s on the first expiry (more on later ones)

### C. Immediate on the first expiry, backoff after that

**For**

- Fast for the common single blip

**Against**

- Two rules to explain and test, for a saving of 2 s

### D. A separate, shorter curve for lease expiry

**For**

- Tunable on its own

**Against**

- A second set of numbers with no data yet to justify it

## Decision

Use B. A lease expiry is handled like a retryable failure: the job goes back to queued with run_at = now() + delay, using the same backoff and jitter as T9 (ADR 004). The delay is based on the attempt that expired. If that was the last attempt, the job goes to dead (T13). In both cases the reaper adds a "lease expired" entry to the error history.

## Consequences

- The attempt counter goes up at claim time, not at failure time. A job whose worker dies on every attempt therefore reaches dead after 8 claims, and can't loop forever.
- A crash costs the lease (30 s) plus the reaper interval (5 s) plus the first retry delay (2 s). This is the recovery bound in ADR 003.
- Lease expiries and failures share one rule, so the reaper and /report must use the same function (spec section 7), or the two paths could disagree about when a job is dead.
- The error history shows lease expiries as their own entries, so an operator can see a job that kept killing workers.
- A worker that was only slow may still finish and report after its lease expired. If the reaper has not yet taken the job back (it is still running with that worker's lease fields), that report is accepted (ADR 009). Once the reaper has requeued it, the report is refused, even if no other worker has claimed it yet.

## Revisit this decision if

- Recovery time after a crash matters more than protection from jobs that kill workers (switch to C)
- Chaos tests show most lease expiries are healthy workers, not crashes
