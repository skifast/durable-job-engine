# ADR 004: Retry curve and jitter

**Status:** Proposed  
**Date:** 2026-10-06  
**Spec sections:** 6 (T9, T11, T12, T13), 8 (retry policy), 10 (handler contract)  
**Depends on:** ADR 002 (the API server computes the delay), ADR 003 (lease expiry uses the same delay)  

## Context

A retryable failure (a timeout, a network error, a 5xx from a downstream service, a handler that ran past its time limit) means trying again might work. A lease that expires counts the same way. The engine has to decide how long to wait before the next attempt, how many attempts to allow, and how to keep many failing jobs from all retrying at the same moment.

Waiting too little hammers a service that is already struggling, and burns attempts before it recovers. Waiting too long delays jobs that would have worked. Permanent failures (invalid payload, a 4xx) are never retried, so they are outside this decision.

The delay is worked out by the API server when it handles /report, and by the reaper when a lease expires. It is stored as run_at = now() + delay, using the database clock. The random part is injectable so tests can use a fixed seed.

## Options for the curve

### A. Fixed delay (for example 10 s every time)

**For**

- Simplest to explain and test

**Against**

- Keeps hitting a struggling service at the same rate, and a fixed delay is either too short for a long outage or too long for a blip

### B. Linear (10 s, 20 s, 30 s, ...)

**For**

- Backs off, and grows predictably

**Against**

- Backs off slowly at first and does not adapt to how long the outage lasts

### C. Exponential doubling with a cap (2 s, 4 s, 8 s, ... capped at 60 s)

**For**

- Retries quickly after a blip (first retry at 2 s) and backs off fast if the failure persists
- The cap bounds the wait, so late retries don't drift into minutes

**Against**

- More parameters to choose (base, cap, attempts)

### D. Exponential doubling with no cap

**For**

- One fewer parameter

**Against**

- With 8 attempts the last wait is 128 s, and any later increase in attempts makes waits grow quickly. The cap only matters when it is below 128 s, so with 8 attempts this option differs from C only in the last two waits (64 and 128 s instead of 60 and 60)

## Options for the jitter

Jitter spreads retries so that jobs that failed together don't all retry together.

- None: all jobs that failed at the same time retry at the same time
- Small proportional jitter: add 0-10% of the delay. Keeps the schedule predictable, but the spread is narrow (a 60 s delay spreads over 60-66 s)
- Full jitter: wait a random time between 0 and the delay. The widest spread and the lowest load on the downstream service in published comparisons, but a job can retry almost immediately and the schedule is much less predictable
- Equal jitter: wait half the delay plus a random time up to the other half. A middle ground
- Decorrelated jitter: each wait is random, based on the previous wait. Spreads well, but is harder to reason about and to test

## Options for the number of attempts (with the 60 s cap)

| Attempts | Waits | Total waiting | Covers a downstream outage of about |
|---|---|---|---|
| 5 | 4 | 30 s | half a minute |
| 8 | 7 | 182 s (3 min) | about 3 minutes |
| 12 | 11 | 422 s (7 min) | about 7 minutes |

## Decision

Use C with 8 attempts. The delay before the next attempt is min(2 x 2^(attempt - 1), 60 s) + jitter, where attempt is the number of the attempt that just failed. That gives waits of 2, 4, 8, 16, 32, 60 and 60 s, 182 s in all (about 3.3 minutes with jitter). Jitter is a random 0-10% of the delay, added after the cap.

## Consequences

- Because the jitter is added after the cap, the final wait can be up to 66 s. The cap limits the base delay, not the final delay.
- A downstream outage longer than about 3 minutes uses up every attempt, and the jobs go to dead with their error history. After the outage, an operator redrives them from the operator page. That is the intended path for long outages, not a bug.
- The operator page redrives one job at a time, so recovering from a long outage with many dead jobs is slow. A bulk redrive is not in the spec.
- The 0-10% jitter is narrow. If many jobs fail at the same moment, their retries will bunch up. In this system the workers limit the damage: 10 workers running one job at a time can start at most about 50-100 jobs per second, so the load on a downstream service is bounded by the workers, not by the retry schedule. Wider jitter would matter more with more workers.
- A lease that expires uses the same delay as a failure (T12), computed from the attempt that expired, so a crash costs the 2 s first-retry wait on top of the lease and reaper interval (ADR 003).
- Permanent failures skip retries entirely and go straight to dead (T10). Which failures count as retryable is a handler contract question (spec section 10), and getting that split wrong costs more than any number here.
- The delay function takes the attempt number and a random source and returns a number of seconds. That makes it easy to test with a fixed seed: the delays for a given attempt must match the table, and the jitter must stay within 0-10%.

## Revisit this decision if

- Load or chaos tests show retries bunching up and overloading a downstream service (widen the jitter, for example to full jitter)
- Typical outages last longer than about 3 minutes (more attempts, or a larger cap)
- Operators spend real time redriving after outages (add a bulk redrive)

## To verify before accepting

- The jitter variants (full, equal, decorrelated) come from the AWS Architecture Blog post "Exponential Backoff And Jitter": aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/. Read it and confirm how it compares them.
