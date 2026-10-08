# ADR 009: Fencing token and redrive

**Status:** Accepted (2026-10-07)  
**Date:** 2026-10-06  
**Spec sections:** 4 (claims and leases), 6 (T6-T11, T14), 10 (handler contract)  
**Depends on:** ADR 001 (the claim is written to columns), ADR 003 (lease), ADR 007 (lease expiry)  

## Context

A worker can lose its lease and not know it yet: it was paused, the network dropped, or the lease expired while it was still running. Meanwhile the reaper gives the job to another worker. Now two workers think they own the job, and the first one will eventually report. If the engine accepts that report, the old worker overwrites the new one's work, or marks the job finished when it isn't.

A fencing token fixes this. Each claim hands out a token, and every later heartbeat and report must show the token for the current claim. An old token is refused, even if it comes from a worker that is still running.

Redrive raises a second question: when an operator brings a dead job back, can its token numbers start again?

## Options for the fencing token

### A. Check only the worker ID (lease_owner)

**For**

- Nothing new to store

**Against**

- The same worker can claim the same job again after a requeue. An old, still-running thread of that worker then looks like the current owner, and its stale report is accepted

### B. The attempt counter, which goes up by one on every claim (chosen)

**For**

- Already exists: it counts runs, limits retries and numbers the error history
- Always increases, so a larger token always means a newer claim
- A heartbeat or report is accepted only if lease_owner and attempt both match and the state is running

**Against**

- The token is guessable (attempt 3 follows attempt 2). Authentication is a non-goal, so the token is a safety check against mistakes, not a secret

### C. A random token (UUID) per claim, stored in its own column

**For**

- Not guessable, and no counter semantics to get wrong

**Against**

- An extra column, and it doesn't increase, so "newer" can't be read from it. Attempt would still be needed for retries

### D. No fencing (the last write wins)

**For**

- Simplest

**Against**

- A slow worker can overwrite a newer one's result. This is the exact problem the engine is meant to solve

## Options for redrive (dead job brought back by an operator)

- Reset attempt to 0 and give a fresh set of attempts: simple to explain, but the token numbers 1, 2, 3 would be handed out a second time. A stale worker still holding the old attempt 3 could match a new attempt 3 (especially if the worker ID is the same)
- Keep attempt and raise max_attempts (chosen): attempt never goes down, so every token is used only once for the life of the job
- Create a new job: loses the history, and the idempotency key already belongs to the old job

## Decision

Use B for the token, and keep attempt when redriving. Redriving means an operator bringing a dead job back to the queue (from the operator page) so it runs again, for example after a downstream outage ends or a bug is fixed. The operator picks a new max_attempts, which must be greater than the job's current attempt, and the job goes back to queued (T14). The token is per job, not global: it is the attempt counter stored on that job's row, so job A's attempt 3 and job B's attempt 3 are unrelated, and a token is only ever compared with the one on the same job. /claim returns the attempt number as the fencing token. /heartbeat and /report are accepted only if lease_owner and attempt match and the state is running; otherwise nothing changes and the worker is told it lost the lease. Redrive (T14) only works on a dead job, sets the new max_attempts, clears finished, and never changes attempt.

## Consequences

- A report that arrives after the lease expired is still accepted, as long as the reaper has not taken the job back yet (the owner and attempt still match, and the state is still running). Once the reaper requeues the job, the state is queued and the late report is refused, even if no other worker has claimed it yet. This saves work that was finished just too late, and no later.
- Fencing protects the job row only. If a stale worker already called a downstream service, nothing in the engine can take that back. Handlers pass idempotency keys derived from the job ID to downstream calls, the same on every attempt, so a repeat is harmless (spec section 10).
- After a redrive, attempt is already high. A job that died after 8 attempts and is redriven with max_attempts 12 starts at attempt 9. Because the retry delay is based on the attempt number, a redriven job's retries wait the capped 60 s from the first failure on, which is reasonable for a job that has already failed 8 times.
- The invariants "attempt never decreases" and "attempt never exceeds max_attempts" both hold through a redrive, and they can be checked with SQL.
- Redrive needs a new max_attempts, so the operator page asks for it.
- The test for this ADR: pause a worker past its lease, let another worker finish the job, wake the first, and check that its report changes nothing. Also: redrive a dead job and check that the new claim's token is higher than every earlier one.

## Revisit this decision if

- Tokens must be unguessable (authentication added, use C)
- Attempt gets used for something that needs to reset, such as the retry delay after a redrive (add a separate column for retries since the last redrive)
