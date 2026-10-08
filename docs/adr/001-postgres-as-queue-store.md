# ADR 001: Postgres as the queue store

**Status:** Accepted (2026-10-07)  
**Date:** 2026-10-06  
**Spec sections:** 3 (scale and durability), 6 (states and transitions)  

## Context

The engine has to store jobs, hand each one to exactly one worker at a time, track leases and attempts, and survive crashes without losing a job. It also has a human approval step, where a job waits for a person, possibly for days. The target is 50 jobs per second with 10 workers (plus a separate test of the maximum speed, using a handler that does no work), a 100k-row backlog, and no job lost once the API has acknowledged it (spec section 3).

Two requirements drive the choice:

- A job's state, attempt count and lease must change together, atomically (all or nothing). If they were separate writes, a crash between them would leave the data contradicting itself. For example: a worker claims a job, the engine sets state = running, and then crashes before writing lease_owner and lease_expires_at. The job now looks claimed (state says running), but no worker owns it and no lease will ever expire, so the reaper never finds it and it is stuck forever. The reverse order fails too: if the lease is written first and the state still says queued, a second worker can claim the same job, and two workers run it at once.
- A person has to be able to look at any job and see its state and error history, and act on it (approve, reject, redrive), from the operator page, which works through the API (ADR 010).

## Options

### A. Postgres (a jobs table, claimed with SELECT ... FOR UPDATE SKIP LOCKED)

**For**

- One transaction can change state, attempt, lease and error history together
- SKIP LOCKED lets many workers claim at once without waiting on each other
- Every job can be inspected and counted with SQL, and every invariant in section 4 can be written as a SQL query
- Long waits (approval) are just rows in a table; nothing expires
- Only Postgres to run, back up and reason about

**Against**

- Lower throughput ceiling than the others (fine for hundreds per second; not for hundreds of thousands)
- Workers polling adds load even when the queue is empty. Workers have no way of being told a job has arrived, so each one keeps asking (every 100 ms), and every ask is a request to the API and a query on Postgres, even when the answer is always "nothing yet"
- Heavy updates create dead rows, so vacuum and indexes need attention under load
- The row lock ends when the transaction ends, so the claim has to be written to columns (state, lease_owner, lease_expires_at, attempt). This is extra work, and it is the part the project demonstrates

### B. Redis (lists or Streams)

**For**

- Very fast, in memory, built-in blocking reads so workers don't have to poll
- Streams (Redis's own append-only log type, unrelated to anything in Postgres) have consumer groups and a way to reclaim messages that a dead consumer left pending

**Against**

- Durability depends on how it is configured (append-only file settings), and a crash can lose recent writes
- Job state would live in Redis while the approval and operator view want relational queries, so state could end up in two places
- No transaction across "change job state" and "write error history" the way a relational table has
- Retries with a delay, and the approval wait, have to be built on top

### C. SQS

**For**

- Managed, durable, scales far past this target
- Already has the features this project builds by hand: a visibility timeout (30 seconds by default, which is where the 30 s lease comes from), retries, and a dead-letter queue

**Against**

- Not a table: there is no way to ask "show me all jobs waiting for approval" or run the invariant queries
- Delivery is at least once, with no fencing token, and a message can't be held for days with a human-driven state
- Using it would remove the part of the work this project is meant to demonstrate

### D. Kafka

**For**

- Built for very high throughput and many independent consumers of the same events; keeps history

**Against**

- A log, not a work queue. A log is append-only: messages are added at the end and stay in order, and each consumer remembers just one position (an offset) per partition. There is no per-message state, so there is nowhere to record "this job is leased to worker X until 12:00:30" and no per-message acknowledgement. Ordering is per partition, so one slow job can hold up the others behind it
- Delayed retries and a dead-letter topic have to be built with extra topics
- Much heavier to run than this scale needs

### E. Skip all of them: an in-process queue

Not an option for this project. It loses every job when the process dies.

## Decision

Use Postgres. At 50 jobs per second it is well within range of our goals, and a test with an empty handler (one that does no work) will show how much faster it could go. It also gives atomic state changes, inspection with SQL, and long approval waits, with only Postgres to run. Redis, SQS and Kafka each fit the throughput better in some way, but each removes one of the properties above or removes the part of the work the project is meant to show.

## Consequences

- The claim has to be written to columns, not just held as a row lock. SKIP LOCKED only locks the row while the claiming transaction is open. If the worker kept that transaction open for the whole run (up to 60 s), it would tie up a database connection, hold back vacuum, and lose the claim if the connection dropped, with no record of who had the job. So the claim transaction commits right away, after writing state = running, lease_owner, lease_expires_at and attempt into the row. Other workers skip the row because its state is no longer queued, and the lease expiry time lets the reaper notice a dead worker. This leads to the lease and fencing design (ADR 003, ADR 009).
- Workers poll the API, which polls Postgres, so the poll interval (100 ms) trades start latency against idle load.
- The jobs table needs an index that matches the claim query (state and run_at), and dead-row cleanup (vacuum) needs to be watched in the load tests.
- Throughput stops at what one Postgres node can do. That is why scale above a few thousand jobs per second and Postgres failover are non-goals.
- Dead rows and vacuum. Every state change (claim, heartbeat, report) updates a row, and Postgres keeps the old version of the row until vacuum removes it. The plan:
  - Keep every transaction short. A transaction that stays open holds back vacuum. The claim writes the lease to columns instead of holding a row lock, so nothing stays open while a handler runs
  - Tune autovacuum on the jobs table only, so it runs after a small fraction of rows change instead of the default 20%
  - Leave free space in each page of the table (a lower fillfactor), so updates can stay in the same page
  - Measure it: the soak run in the test plan (30 minutes at the target rate) records dead rows (n_dead_tup), table size and claim p95 over time. If claim latency climbs, the plan failed and this ADR is revisited
- Table growth. Idempotency keys are kept forever and live in the jobs table (spec section 9), so finished jobs can never be deleted and the table only grows. At 50 jobs per second that is about 4 million rows a day. This is acceptable for the project's tests, but not for long-term running. The fix would be to move keys into their own small table and archive finished jobs, which is a change to ADR 005, not this one.
- Moving to another store later means rewriting the claim, lease and reaper logic. The API, the job model and the handler contract would not change.

## Revisit this decision if

- The load tests can't sustain the scale target on one node
- The product needs many independent consumers of the same job events (Kafka's strength)
- The team would rather run a managed queue than own the queue logic

## References

*The comparison points for Redis and Kafka come from general knowledge, so confirm them against the official docs.*

- Postgres SELECT ... FOR UPDATE ... SKIP LOCKED: postgresql.org/docs/current/sql-select.html (locking clause)
- SQS visibility timeout and dead-letter queues: docs.aws.amazon.com/AWSSimpleQueueService
- Redis Streams consumer groups and pending entries: redis.io/docs (Streams)
- Kafka consumer groups and partition ordering: kafka.apache.org/documentation
