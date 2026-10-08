# ADR 005: Duplicate-key handling, key scope and key retention

**Status:** Proposed  
**Date:** 2026-10-06  
**Spec sections:** 6 (T1, T2), 9 (idempotency), 4 (idempotency invariants)  
**Depends on:** ADR 001 (Postgres)  

## Context

Clients retry. A request can time out after the job was already saved, or the connection can drop after the commit but before the response arrives. If the client sends the same request again, the engine must not create a second job.

The engine needs a way to tell "the same request again" from "a new request that happens to look the same". It also has to decide what happens in three cases:

- the same key arrives with the same payload (a plain retry)
- the same key arrives with a different payload (a client bug)
- the same key arrives while the first request is still being saved

And two storage questions: who a key belongs to (its scope), and how long it is remembered (its retention).

## Options for how to detect a duplicate

### A. A key chosen by the client (Idempotency-Key header), enforced by a unique constraint in Postgres

**For**

- The client says which requests are the same, so two deliberate, identical jobs are not merged by accident
- The database enforces it, so two requests arriving at the same moment can't both succeed
- The key is saved in the same transaction as the job, so there is no moment where the job exists without its key

**Against**

- Clients have to generate and reuse keys correctly

### B. The server decides, by hashing the payload

**For**

- Clients send nothing extra

**Against**

- Can't tell a retry from a second, intentional job with the same payload (two identical emails to send)

### C. A separate key table with an expiry (keys forgotten after about a day, a common choice in payment APIs)

**For**

- Small rows and bounded size, and finished jobs can be archived or deleted

**Against**

- After the expiry, the same key creates a new job, so "at most one job per key" only holds inside the window

### D. A cache (for example Redis) with a time limit

**For**

- Fast

**Against**

- A cache can evict entries or lose them in a restart, and then a duplicate slips through. The key has to live as long as the job does, in durable storage

## Options for the same key with a different payload

- Update the job with the new payload: dangerous, it can change a job that is already running or finished
- Ignore the new payload and return the original job: hides a client bug
- Reject with 422 and change nothing: the client learns it reused a key. Needs a stored hash of the payload (payload_hash) to compare

## Decision

Use A, with these rules:

- A new job returns 201
- Same key, same payload returns 200 with the original job
- Same key, different payload returns 422 and changes nothing (compared by payload_hash, computed over a canonical form of the JSON so that key order and spacing don't matter)
- Same key while the first request is still being saved returns 409. Postgres makes the second insert wait for the first transaction to finish. If the first has not finished after a short wait (a lock timeout of 2 s), the second request returns 409 and the client retries
- Scope: per client. The key is unique per (client_id, key), so two clients can use the same key text without clashing
- Retention: kept forever, in the jobs table

## Consequences

- "At most one job per client and idempotency key" holds with no exceptions, and it can be checked with one SQL query.
- The jobs table never shrinks, because deleting a finished job would delete its key. At 50 jobs per second that is about 4 million rows a day (ADR 001 describes the vacuum plan and this growth). This is fine for the project's tests and not for long-term running.
- client_id is trusted, not verified, because authentication is a non-goal. One client could send another's client_id, and in this version nothing would stop it.
- The submit request does one INSERT ... ON CONFLICT DO NOTHING; if nothing was inserted, it reads the existing row, compares payload_hash, and returns 200 or 422.
- A job that is waiting for approval also holds its key, so a retried submit returns 200 with that waiting job.

## Upgrade path

- Move keys into their own small table, kept forever (not a cache, no expiry). Jobs can then be archived or deleted while the keys stay, so the invariant still holds and the jobs table stays bounded. This changes where the key lives, not what clients see.

## Revisit this decision if

- The jobs table grows too large for the soak or load tests
- Clients can't be trusted to send their own client_id (needs authentication, a non-goal today)

## To verify before accepting

- Confirm in the Postgres docs for INSERT (ON CONFLICT) that a second insert waits for an uncommitted conflicting insert to finish, and that lock_timeout applies to that wait. The 409 rule depends on both.
