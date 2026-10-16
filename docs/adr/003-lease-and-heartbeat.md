# ADR 003: Lease length and heartbeat interval

**Status:** Accepted (2026-10-07)  
**Date:** 2026-10-06  
**Spec sections:** 6 (T6, T7, T12, T13), 7 (worker, reaper), 11 (failure modes)  
**Depends on:** ADR 001 (Postgres), ADR 002 (workers call the API)  

## Context

When a worker claims a job it gets a lease: lease_expires_at = now() + lease length. While the handler runs, the worker sends a heartbeat that pushes the expiry forward. If heartbeats stop (the worker crashed, froze or lost the network), the lease runs out, and the reaper puts the job back in the queue for another worker (T12) or marks it dead if attempts are used up (T13).

Two numbers have to be chosen: how long the lease is, and how often the worker sends a heartbeat. They trade against each other:

- A short lease recovers from a crash quickly, but a healthy worker that is only briefly slow (a pause, a network blip, a busy database) can lose its job. The job then runs twice.
- A long lease almost never expires by mistake, but a crashed worker's job sits there until the lease runs out.
- Heartbeats need to be frequent enough that a few lost ones don't expire a healthy lease, but each one is a request to the API and a write to Postgres.

### How the two numbers relate
A heartbeat sent every H seconds renews a lease of L seconds. To survive m heartbeats in a row failing, the next one must still arrive before the lease runs out: (m + 1) x H < L.

| Heartbeat interval | Beats per 30 s lease | Missed beats in a row that are survived |
|---|---|---|
| L / 2 = 15 s | 2 beats per lease | 0 (one miss expires the lease) |
| L / 3 = 10 s | 3 beats per lease | 1 |
| L / 4 = 7.5 s | 4 beats per lease | 2 |
| L / 5 = 6 s | 5 beats per lease | 3 |

The lease is renewed for as long as the worker lives, so the lease length does NOT limit how long a handler can run. That is a separate limit (60 s default per job type, spec section 10), enforced by the worker.

## Options for the lease length

### A. Short: about 10 s (heartbeat 2.5 s)

**For**

- A crashed worker's job is picked up within about 10 s plus the reaper interval

**Against**

- More false expiries: any pause longer than the survivable misses (2 beats = 5 s here) loses the job
- A heartbeat every 2.5 s instead of every 7.5 s, so 3x the heartbeat traffic of the 30 s lease

### B. Medium: 30 s (heartbeat 7.5 s)

**For**

- The default visibility timeout in SQS, so it is a known, reasonable middle ground
- Survives two lost heartbeats in a row, and pauses of up to about 15 s
- Recovery in about 30 s plus the reaper interval is fine for jobs that run seconds to a minute

**Against**

- A crashed worker's job waits up to 30 s before anyone can take it. Not suitable if jobs must restart within a few seconds

### C. Long: 2 to 5 minutes (heartbeat 30-75 s)

**For**

- Very few false expiries, and very light heartbeat traffic

**Against**

- A crash costs minutes of delay, which is longer than the whole retry schedule (182 s of waiting across all 7 retries, spec section 8). Poor fit for 60 s handlers

## Options for the heartbeat interval (with a 30 s lease)

- 10 s (L/3): survives one miss. Cheaper, but a single dropped request plus a slow second one can expire the lease
- 7.5 s (L/4): survives two misses. Four heartbeats per lease
- 5 s or less: survives more misses, but doubles the traffic for little added safety, because several lost heartbeats in a row usually mean the worker really can't reach the API

## Decision

Lease 30 s, adjustable by configuration. Heartbeat every 7.5 s, which is L / 4: a worker can miss two heartbeats in a row and still keep its job. 30 s matches SQS's default visibility timeout and balances a dead worker not waiting too long against not losing healthy workers by mistake. If the lease setting is changed, the heartbeat interval should change with it so the ratio holds.

## Consequences

- Time to recover from a worker crash, at worst: lease (30 s) + reaper interval (5 s) + the retry delay (2 s on the first expiry, because T12 uses the same backoff as T9) + poll interval (100 ms) + the run time on the next worker. That is about 37 s plus run time, and it is the bound written in the spec's goals.
- The worker cancels its handler if it can't renew for longer than the lease. It should measure that time on its own monotonic clock from when it sent the last successful heartbeat request, so it stops no later than the database's expiry. The database clock decides expiry; the worker's clock only measures how long it has been out of touch.
- A false expiry can still happen (a long pause, a slow database). It is safe, because the old worker's heartbeat and report are refused by fencing (ADR 009), and the handler is repeat-safe (spec section 10). It costs wasted work, not wrong results.
- Heartbeat load is small: one request per running job every 7.5 s. Each worker runs one job at a time, so at most 10 jobs run at once, which is about 1.3 requests per second.
- The load and chaos tests should check the numbers above: kill a worker and measure the time until the job runs elsewhere; pause a worker for longer than the lease and confirm the stale report is refused.

## Revisit this decision if

- Jobs need to restart within a few seconds of a crash (shorter lease)
- Load tests show many false expiries on a healthy system (longer lease, or more tolerance)
- Heartbeat traffic becomes a measurable share of API load

## Settings this ADR depends on

- Reaper run interval: 5 s (spec section 7). It adds directly to recovery time.
- Each worker runs one job at a time (spec section 7), so heartbeats are per worker, one running job each.
