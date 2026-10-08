# ADR 005: Duplicate-key handling, key scope and key retention

**Status:** Proposed  
**Date:** 2026-10-06  
**Spec sections:** 6 (T1, T2), 9 (idempotency), 4 (idempotency invariants)  
**Depends on:** ADR 001 (Postgres)  

## Context

Clients retry. A request can time out after the job was already saved, or the connection can drop after the commit but before the response arrives. If the client sends the same request again, the engine must not create a second job.

The engine needs a way to tell "the same request again" from "a new request that happens to look the same". It also has to decide what happens in three main cases (a few edge cases are in the Decision):

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

Start with A, and plan to move toward C later (see Upgrade path). The rules:

- A new job returns 201
- Same key, same payload returns 200 with the original job
- Same key, different payload returns 422 and changes nothing (compared by payload_hash, computed over a canonical form of the JSON so that key order and spacing don't matter)
- Same key while the first request is still being saved returns 409. Postgres makes the second insert wait for the first transaction to finish. If the first has not finished after a short wait (a lock timeout of 2 s), the second request returns 409 and the client retries
- Scope: per client. The key is unique per (client_id, key), so two clients can use the same key text without clashing
- Retention, for now: kept forever, in the jobs table

Edge cases:

- A missing or empty `Idempotency-Key` or `client_id`, or a key longer than 255 characters, returns 400 and stores nothing. Clients should use random keys (for example UUIDs)
- `payload_hash` covers the job type as well as the payload, so the same key with a different job type also returns 422
- A submit rejected for an invalid payload stores nothing, so the key is still free for a corrected request
- A request that fails before the commit stores nothing, so the retry is treated as new and returns 201
- The same key and payload for a job that is waiting, queued or running returns 200 with the job in its current state
- The same key and payload for a job that has already ended (succeeded, dead or rejected) also returns 200 with that job. A new job is not created. To run the work again, the client uses a new key, or an operator redrives a dead job

## Consequences

- "At most one job per client and idempotency key" holds with no exceptions, and it can be checked with one SQL query.
- The jobs table never shrinks, because deleting a finished job would delete its key. At 50 jobs per second that is about 4 million rows a day (ADR 001 describes the vacuum plan and this growth). This is fine for the project's tests and not for long-term running.
- client_id is trusted, not verified, because authentication is a non-goal. One client could send another's client_id, and in this version nothing would stop it.
- The submit request does one INSERT ... ON CONFLICT DO NOTHING; if nothing was inserted, it reads the existing row, compares payload_hash, and returns 200 or 422.
- A job that is waiting for approval also holds its key, so a retried submit returns 200 with that waiting job.
- Security limits, because authentication is a non-goal. `client_id` is only a header, so anyone who can reach the API can send any `client_id`. That allows three things:
  - reading another client's job: send their `client_id`, their key and the same payload, and the answer is 200 with their job
  - key squatting: submit under a victim's `client_id` with a key the victim will use later, so the victim's real request gets 200 with the attacker's job (or 422 for a different payload)
  - learning that a key exists, from a 422

  All three need the attacker to know the key. Random keys (UUIDs) make that impractical unless a key leaks, and the API is treated as a trusted-network service. This is a known limit of the non-goal, not a claim that the design is safe on an open network. The real fix is authentication, with `client_id` taken from verified credentials instead of a header. The per-client scope then becomes a real boundary.

## Upgrade path

Move to C: keys in their own small table with an expiry, so the system can run beyond tests.

- The table holds the client, the key, the job, the `payload_hash` and an expiry time, with the unique constraint on (client_id, key). The submit inserts the key and the job in one transaction, as now
- Finished jobs can then be archived or deleted, so the jobs table stays bounded
- The expiry should count from when the job reaches a final state (for example 24 hours after), not from submit. A job that is queued, running or waiting for approval (which can take days) then never loses its key
- The window must be longer than the longest time a client keeps retrying
- The invariant weakens to "at most one job per client and key while the key exists". A request that reuses a key after it expires creates a new job and returns 201
- Expired keys have to be deleted by something; the reaper is the natural place
- The spec's invariant (section 4) and retention rule (section 9) would be rewritten when this happens

## Revisit this decision if

- The jobs table grows too large for the soak or load tests (move to C)
- Clients can't be trusted to send their own client_id (needs authentication, a non-goal today)

## To verify before accepting

- Checked in the PostgreSQL docs (2026-10-07): the page on index uniqueness checks says that when a conflicting row comes from an uncommitted transaction, the would-be inserter waits for that transaction to end and then checks again. The page for `lock_timeout` says it applies to waits for locks on tables, indexes, rows or other database objects.
- Not stated directly in the docs: that `lock_timeout` cuts short this particular wait, and the exact result of `ON CONFLICT DO NOTHING` after the wait. Confirm both with a two-session test (session A inserts and holds the transaction open; session B inserts the same key with `lock_timeout` set to 2 s). That test should become an automated test in the project.
