# ADR 004: Retry curve and jitter

**Status:** Accepted (2026-10-09)  
**Date:** 2026-10-06  
**Spec sections:** 6 (T9, T11, T12, T13), 8 (retry policy), 10 (handler contract)  
**Depends on:** ADR 002 (the API server computes the delay), ADR 003 (lease expiry uses the same delay)  

## Context

A retryable failure is one where trying again might work, because the cause is outside the job and is likely to go away. Examples: a timeout, a network error, a 5xx response from a downstream service, a 429 (too many requests) or 408 (request timeout) response, or a handler that ran past its time limit. The list is not complete. A lease that expires counts the same way. The engine has to decide how long to wait before the next attempt, how many attempts to allow, and how to keep many failing jobs from all retrying at the same moment.

Waiting too little hammers a service that is already struggling, and burns attempts before it recovers. Waiting too long delays jobs that would have worked. Permanent failures are never retried, so they are outside this decision. They are failures where a later attempt would give the same result. Examples: an invalid payload, a missing resource (404), a permission error (403), or most other 4xx responses. This list is also not complete. The handler decides which kind a failure is (spec section 10).

The delay is worked out by the API server when it handles /report, and by the reaper when a lease expires. It is stored as run_at = now() + delay, using the database clock. The random part is injectable so tests can use a fixed seed.

## Options for the curve

### A. Fixed delay (for example 10 s every time)

**For**

- Simplest to explain and test

**Against**

- Keeps hitting a struggling service at the same rate, and a fixed delay is either too short for a long outage or too long for a short outage that fixes itself

### B. Linear (10 s, 20 s, 30 s, ...)

**For**

- Backs off, and grows predictably

**Against**

- Backs off slowly at first and does not adapt to how long the outage lasts

### C. Exponential doubling with a cap (2 s, 4 s, 8 s, ... capped at 60 s)

**For**

- Retries quickly after a short outage that fixes itself (first retry at 2 s) and backs off fast if the failure persists
- The cap bounds the wait, so late retries don't drift into minutes
- The cap limits the wait between attempts. It is separate from the handler's time limit, which limits how long one attempt may run

**Against**

- More parameters to choose (base, cap, attempts)

### D. Exponential doubling with no cap

**For**

- One fewer setting to choose, document and test

**Against**

- The waits keep doubling. With 8 attempts the last wait is 128 s, so a job that fails just before the downstream service recovers sits idle for more than two minutes. With 12 attempts the last wait would be 2,048 s (about 34 minutes)
- With 8 attempts this option differs from C only in the last two waits (64 and 128 s instead of 60 and 60), so the saving is small

## Options for the jitter

Jitter spreads retries so that jobs that failed together don't all retry together.

- None: all jobs that failed at the same time retry at the same time
- Small proportional jitter: add 0-10% of the delay. Keeps the schedule predictable, but the spread is narrow (a 60 s delay spreads over 60-66 s)
- Full jitter: wait a random time between 0 and the delay. The widest spread, and the least total work in the AWS post's simulation (see References), but a job can retry almost immediately and the schedule is much less predictable
- Equal jitter: wait half the delay plus a random time up to the other half. Avoids very short waits, but ranked last among the jittered options in the AWS post's simulation
- Decorrelated jitter: each wait is random, based on the previous wait. Finished slightly faster than full jitter in the AWS post's simulation, with more total work, but is harder to reason about and to test

## Options for the number of attempts (with the 60 s cap)

| Attempts | Waits | Total waiting | Covers a downstream outage of about |
|---|---|---|---|
| 5 | 4 | 30 s | half a minute |
| 8 | 7 | 182 s (3 min) | about 3 minutes |
| 12 | 11 | 422 s (7 min) | about 7 minutes |

## Decision

Use C with 8 attempts. The delay before the next attempt is min(2 x 2^(attempt - 1), 60 s) + jitter, where attempt is the number of the attempt that just failed. That gives waits of 2, 4, 8, 16, 32, 60 and 60 s, 182 s in all (about 3.3 minutes with jitter). Jitter is a random 0-10% of the delay, added after the cap. It is small and proportional on purpose: at this stage a predictable schedule is worth more than the widest spread. The AWS post's simulation favors full or decorrelated jitter for reducing load, so this choice accepts more bunching in exchange for predictability (see Consequences for why the workers limit the damage).

## Consequences

- Because the jitter is added after the cap, the final wait can be up to 66 s. The cap limits the base delay, not the final delay.
- A downstream outage longer than about 3 minutes uses up every attempt, and the jobs go to dead with their error history. After the outage, an operator (a person who runs the system and uses the operator page) redrives them, which puts each dead job back in the queue for more attempts. That is the intended path for long outages, not a bug.
- The operator page redrives one job at a time, so recovering from a long outage with many dead jobs is slow. A bulk redrive is not in the spec.
- The 0-10% jitter is narrow. If many jobs fail at the same moment, their retries will bunch up. In this system the workers limit the damage: 10 workers running one job at a time can start at most about 50-100 jobs per second, so the load on a downstream service is bounded by the workers, not by the retry schedule. Wider jitter would matter more with more workers.
- A lease that expires uses the same delay as a failure (T12), computed from the attempt that expired, so a crash costs the 2 s first-retry wait on top of the lease and reaper interval (ADR 003).
- Unclassified failures (added 2026-10-09): when a handler raises an error it did not classify, the worker reports it under its own kind, `unclassified`. The engine retries it with the same schedule as a retryable failure, because it cannot tell whether a retry would help and a transient fault often shows up this way. The cost is that a handler with a bug can run all 8 attempts before the job goes to dead. The separate kind makes those jobs easy to find in the error history (spec section 10).
- Permanent failures skip retries entirely and go straight to dead (T10). Which failures count as retryable is a handler contract question (spec section 10), and getting that split wrong costs more than any number here.
- The delay function takes the attempt number and a random source and returns a number of seconds. That makes it easy to test with a fixed seed: the delays for a given attempt must match the table, and the jitter must stay within 0-10%.

## Revisit this decision if

- Load or chaos tests show retries bunching up and overloading a downstream service (widen the jitter, for example to full jitter)
- Typical outages last longer than about 3 minutes (more attempts, or a larger cap)
- Operators spend real time redriving after outages (add a bulk redrive)

## References

- AWS Architecture Blog, "Exponential Backoff And Jitter": https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/ (checked 2026-10-09). It compares no jitter, full, equal and decorrelated jitter in a simulation. Its conclusions: no jitter is "the clear loser"; of the jittered options, equal jitter is the loser; full jitter uses less work than decorrelated but takes slightly more time. Two limits on using it here. The simulation is 100 clients contending for one resource, not a downstream outage. And the formulas are shown as figures in the post, which this ADR describes in words; check the figures if the exact formulas matter.
