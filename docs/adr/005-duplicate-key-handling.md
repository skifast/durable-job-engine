# ADR 005: Duplicate-key handling, key scope and key retention

**Status:** Accepted (2026-10-09)  
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

- A missing or empty `Idempotency-Key`, or a key longer than 255 characters, returns 400 and stores nothing. (A missing or invalid API key returns 401, ADR 013.) Clients should use random keys (for example UUIDs)
- `payload_hash` covers the job type as well as the payload, so the same key with a different job type also returns 422
- A submit rejected for an invalid payload stores nothing, so the key is still free for a corrected request
- A request that fails before the commit stores nothing, so the retry is treated as new and returns 201
- The same key and payload for a job that is waiting, queued or running returns 200 with the job in its current state
- The same key and payload for a job that has already ended (succeeded, dead or rejected) also returns 200 with that job. A new job is not created. To run the work again, the client uses a new key, or an operator redrives a dead job

## Consequences

- "At most one job per client and idempotency key" holds with no exceptions, and it can be checked with one SQL query.
- The jobs table never shrinks, because deleting a finished job would delete its key. At 50 jobs per second that is about 4 million rows a day (ADR 001 describes the vacuum plan and this growth). This is fine for the project's tests and not for long-term running.
- client_id was designed as a trusted header, because authentication was a non-goal. ADR 013 (2026-10-09) adds API keys: client_id now comes from the caller's key, so one client cannot use another's client_id. The security limits below describe the design before that change and no longer apply across clients.
- The submit request does one INSERT ... ON CONFLICT DO NOTHING; if nothing was inserted, it reads the existing row, compares payload_hash, and returns 200 or 422.
- A job that is waiting for approval also holds its key, so a retried submit returns 200 with that waiting job.
- The API maps the lock-timeout error (55P03) to 409. It should set the timeout with SET LOCAL inside the submit transaction, so the setting does not carry over to other work on a pooled connection.
- Security limits before ADR 013 (authentication was a non-goal). `client_id` was only a header, so anyone who could reach the API could send any `client_id`. That allowed three things:
  - reading another client's job: send their `client_id`, their key and the same payload, and the answer is 200 with their job
  - key squatting: submit under a victim's `client_id` with a key the victim will use later, so the victim's real request gets 200 with the attacker's job (or 422 for a different payload)
  - learning that a key exists, from a 422

  All three need the attacker to know the key. Random keys (UUIDs) make that impractical unless a key leaks, and the API is treated as a trusted-network service. This is a known limit of the non-goal, not a claim that the design is safe on an open network. The real fix is authentication, with `client_id` taken from verified credentials instead of a header. The per-client scope then becomes a real boundary. ADR 013 does this.

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
- Clients need more than one key each, or keys that expire (the key table in the upgrade path and ADR 013 would change together)

## Verification

- PostgreSQL docs (checked 2026-10-07): the page on index uniqueness checks says that when a conflicting row comes from an uncommitted transaction, the would-be inserter waits for that transaction to end and then checks again. The page for `lock_timeout` says it applies to waits for locks on tables, indexes, rows or other database objects. Neither page says directly that `lock_timeout` cuts short this particular wait, or what `ON CONFLICT DO NOTHING` returns after the wait.
- Two-session test (2026-10-09, PostgreSQL 16, `db/tests/test_duplicate_key_wait.sh`). Session A inserts a key and keeps its transaction open. Session B runs the submit's insert with `lock_timeout` set to 2 s:
  - A commits after 5 s: B fails after 2.0 s with error 55P03 ("canceling statement due to lock timeout"). The API turns this into a 409.
  - A commits within the timeout: B waits for A, then `ON CONFLICT DO NOTHING` inserts nothing and returns no row. The API then reads the existing row and returns 200 or 422.
  - A rolls back: B waits, then inserts the job. The first request left nothing behind, so the retry is a new job (201).
  This settles both points the docs left open. The script is part of the project's tests.
