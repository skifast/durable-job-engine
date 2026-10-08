# Durable Job Engine on Postgres (queue-style) with a human approval step

## Contents

1. Context and overview
2. Goals and non-goals
3. Scale and durability
4. Invariants
5. Job model
6. States and transitions
7. Components
8. Retry policy
9. Idempotency
10. Handler contract
11. Failure modes
12. Test plan
13. ADR index
14. Open decisions

## 1. Context and overview

### Context

It is important in distributed systems that work be run reliably. Unexpected things can happen such as workers crashing, clients retrying, or networks dropping. Something needs to track each job so work doesn't get lost or run multiple times. This engine accepts jobs through an API, stores them in Postgres, runs them on worker processes, and recovers when a worker dies.

### Overview

Several jobs can run at once and any of them can die at any moment. If a worker dies, the heartbeats stop and the lease expires. A background reaper then puts the job back in the queue for another worker, or marks it dead if its attempts are used up.

Build the leases, retries, and fencing manually with Postgres. SQS provides all of this out of the box. Building it by hand is what I want to demonstrate.

## 2. Goals and non-goals

### Goals

- No acknowledged job is lost
- A job whose worker crashes finishes on another worker, within lease + reaper interval + poll interval + run time
- Duplicate submits never create duplicate jobs (idempotency key)
- Jobs that keep failing end in the dead state (the dead-letter queue) after max_attempts, with their error history
- A job waiting for approval stays waiting across restarts, and is escalated if nobody decides by the deadline

### Non-goals

- User-initiated cancellation of a running job
- Authentication and multi-tenancy (client_id is a trusted label, not a verified identity)
- Multi-step workflows or fan-out
- Exactly-once side effects
- Ordering guarantees
- Postgres failover or multi-region (durability stops at a single node)
- Large results (only small JSON or a reference)
- Several handlers at once inside one worker (throughput comes from running more workers)
- Submitting jobs from the operator page, live push updates, and login (the operator types a name that is recorded, not verified)

## 3. Scale and durability

### Scale target

- 50 jobs per second sustained with 10 workers on one Postgres, each worker running one job at a time (100 ms handler; this needs about 200 ms per job in each worker, so it leaves room for the HTTP round trips). Test conditions: 5 minutes sustained, once with the 100 ms handler and once with an empty handler (one that does no work and returns success at once). The empty-handler run is not pass/fail: it reports the maximum throughput, and the hardware is recorded
- Start latency <= 250 ms (p95, idle system)
- Backlog 100k rows (claim p95 stays within 2x of the empty-queue claim latency)

### Durability

- Once the API returns 201 (or 200 for a duplicate), the job is committed to Postgres with the default synchronous_commit setting (on), so it is on disk. It survives crashes of the API, workers, reaper and the Postgres process. It does not survive losing the disk.

## 4. Invariants

Each one becomes a SQL query that returns zero rows when it holds (see Test plan).

### Jobs are never lost

- No job is ever lost: once the API returns 201 or 200, the job stays in the table until it reaches succeeded, dead or rejected. A waiting_approval job stays waiting until someone decides, so it only reaches a terminal state after a decision
- After a test run drains (all approvals decided), every acknowledged submit is in exactly one terminal state

### State and counters

- Only the transitions listed in section 6 ever happen; succeeded and rejected are final
- finished is set exactly when the state is succeeded, dead or rejected
- attempt never exceeds max_attempts
- attempt never decreases
- A succeeded job has a result and no lease
- A dead job has at least one error history entry
- A job becomes dead only after a permanent failure or when its attempts are used up

### Claims and leases

- A job in succeeded is never claimed again
- A waiting_approval job is never claimed by a worker
- Concurrent claims never give the same job to two workers, so no two workers hold a valid lease on the same job
- A running job always has lease_owner and lease_expires_at; a job in any other state has neither
- A heartbeat or report whose lease_owner and attempt do not match the job changes nothing
- Running the reaper twice, or two reapers at once, requeues a job only once

### Idempotency

- There is at most one job per client and idempotency key
- Same key and same payload returns the same job; same key with a different payload is rejected and changes nothing

### Approval

- An approval decision applies at most once; a second decision on the same job is refused
- Escalation never changes a job's state, and a job is escalated at most once

## 5. Job model

A job is a single handler call.

### What the engine knows about the job

- identity: id, type, client_id, idempotency key, payload_hash
- input: payload, as JSON
- state: state, attempt, max_attempts, run_at
- lease: lease_owner, lease_expires_at
- approval: approval_deadline, escalated_at, decided_by, decided_at
- outcome: result, error history
- timestamps: created, updated, finished

### Result detail

- Store small JSON with a size cap of 64 KB, or a reference for big output
- Errors: attempt number, time, type, and a message under a length cap of 1,000 characters
- No secrets in payloads, results or error messages
- The worker sends the result through /report; the result and the state change are written in one UPDATE so nothing ever succeeds without a result

## 6. States and transitions

**States:** queued, waiting_approval, running, succeeded, dead, rejected

succeeded and rejected are final. dead is final except for an operator redrive (T14). A waiting_approval job that passes its deadline is escalated (T5) but stays waiting_approval.

### Assumptions

- Approval is a gate before the handler. Job types that need approval are inserted directly as waiting_approval.
- rejected is the end state for an explicit rejection only. When the approval deadline passes, the job is escalated: it stays waiting_approval, escalated_at is set once, and an approve or reject call still works afterwards. Escalation only sets the flag; the job then shows up when listing escalated approvals. There is no notification hook.
- A handler that passes its time limit counts as a retryable failure.
- Lease-expiry requeues use the same backoff as failures (T9).
- Any transition not listed here is refused.

### T1: (new) → queued

- **Trigger:** valid submit, job type needs no approval
- **Who:** API server
- **Guard and effects:** inserted in one transaction with the idempotency key; run_at = now(); attempt = 0; returns 201

### T2: (new) → waiting_approval

- **Trigger:** valid submit, job type needs approval
- **Who:** API server
- **Guard and effects:** same insert; sets approval_deadline = now() + the job type's approval window (default 24 hours); returns 201

### T3: waiting_approval → queued

- **Trigger:** approve call, any time while the job is waiting_approval (including after escalation)
- **Who:** API server
- **Guard and effects:** only if still waiting_approval; records decided_by and decided_at; run_at = now(); a second decision is refused

### T4: waiting_approval → rejected

- **Trigger:** reject call
- **Who:** API server
- **Guard and effects:** same guard as T3; records who and why; sets finished

### T5: waiting_approval → waiting_approval (escalate)

- **Trigger:** approval_deadline passed and escalated_at is empty
- **Who:** reaper
- **Guard and effects:** sets escalated_at = now() once; state does not change; finished stays empty; no notification is sent

### T6: queued → running

- **Trigger:** /claim
- **Who:** worker, via the API server
- **Guard and effects:** picks a job with state = queued and run_at <= now() (row-locked with SKIP LOCKED); sets lease_owner and lease_expires_at = now() + lease; attempt = attempt + 1; returns the job and its attempt (the fencing token)

### T7: running → running

- **Trigger:** /heartbeat
- **Who:** worker, via the API server
- **Guard and effects:** only if lease_owner and attempt match; lease_expires_at = now() + lease; no match means the worker lost the lease and cancels its handler

### T8: running → succeeded

- **Trigger:** /report with success
- **Who:** worker, via the API server
- **Guard and effects:** accepted only if lease_owner and attempt match and state = running; sets result and finished and clears the lease in one UPDATE

### T9: running → queued

- **Trigger:** /report with a retryable failure (including a handler timeout), attempt < max_attempts
- **Who:** worker, via the API server
- **Guard and effects:** fenced like T8; appends to error history; run_at = now() + delay (delay computed with an injectable random source, applied with the database clock); clears the lease

### T10: running → dead

- **Trigger:** /report with a permanent failure
- **Who:** worker, via the API server
- **Guard and effects:** fenced like T8; appends to error history; sets finished; clears the lease

### T11: running → dead

- **Trigger:** /report with a retryable failure, attempt >= max_attempts
- **Who:** worker, via the API server
- **Guard and effects:** same as T10

### T12: running → queued

- **Trigger:** lease_expires_at < now(), attempt < max_attempts
- **Who:** reaper
- **Guard and effects:** appends "lease expired" to error history; run_at = now() + delay, same backoff as T9 (delay from the attempt that expired); clears the lease

### T13: running → dead

- **Trigger:** lease_expires_at < now(), attempt >= max_attempts
- **Who:** reaper
- **Guard and effects:** appends "lease expired" to error history; sets finished; clears the lease

### T14: dead → queued

- **Trigger:** operator redrive
- **Who:** API server
- **Guard and effects:** only from dead; sets a new max_attempts (greater than attempt, normally higher than before) and does not reset attempt (so fencing tokens stay unique); clears finished (so the finished invariant holds); records who

### Never happens (write a test for each)

- queued → succeeded (skipping running)
- waiting_approval → running (skipping the approval gate)
- anything out of succeeded or rejected
- running → waiting_approval
- waiting_approval → rejected by a timeout (only an explicit reject call does this)
- escalating a job a second time, or a job that is not waiting_approval
- any change made by a worker whose lease_owner and attempt do not match

## 7. Components

### Clients should

- submit jobs through the API with an idempotency key and a client_id

### API server should

- accept jobs (POST /jobs with an Idempotency-Key header), store each one in Postgres (the current state of each job in a table, changing with transactions), and return 201
- serve the worker endpoints: /claim, /heartbeat, /report
- serve the approval and operator endpoints: POST /jobs/{id}/approve, POST /jobs/{id}/reject (takes a reason), POST /jobs/{id}/redrive (takes the new max_attempts, which must be greater than the current attempt)
- serve read endpoints so a human can find jobs: GET /jobs?state=...&escalated=true (paginated, so a 100k-row backlog never loads at once), and GET /jobs/{id} (state, attempt, error history, result)
- record results, and apply the retry-or-dead rule when a worker reports

### Reaper should

- run every 5 s
- find jobs whose lease has expired
- put them back in the queue, or mark them dead if attempts are used up (max_attempts), using the same rule as the failure path in /report so the two cannot disagree
- escalate waiting_approval jobs past their approval_deadline whose escalated_at is empty (T5)
- be safe to run twice or at the same time as another reaper

### Worker should

- poll /claim every 100 ms (needed for the 250 ms start-latency target). The claim is transition T6: it writes the lease (30 seconds, adjustable via config, plus the worker's ID) and returns the attempt number, which is the fencing token
- run one handler at a time
- call /heartbeat every 7.5 seconds while it runs (with a 30 second lease it can miss two in a row before losing the job)
- report the outcome through /report. The report is accepted only if this worker still holds the lease for that attempt (same lease_owner and attempt) at the time of the report; otherwise nothing changes
- cancel its handler if it can't renew the lease for longer than the lease length, or if the handler passes its time limit

### Operator page should

*A screen on top of the API server; it uses only the endpoints above, never Postgres.*

- list jobs, filtered by state, with escalated approvals easy to find, and page through them
- show one job: state, attempt and max_attempts, lease, payload, result, and the error history
- approve or reject a waiting_approval job (reject asks for a reason)
- list dead jobs with their error history, and redrive one (asks for the new max_attempts)
- show the API's refusal message when an action is refused (already decided, wrong state, stale page), then reload the job
- take the operator's name from a typed-in field and send it as decided_by
- refresh on demand (a refresh button; no live updates)

## 8. Retry policy

- max_attempts = 8 (total runs), which gives 7 waits
- delay before the next attempt = min(2 x 2^(attempt - 1), cap) + jitter, where attempt is the number of the attempt that just failed
- cap: 60 s. Without the cap the delays would be 2, 4, 8, 16, 32, 64, 128 s; with it they are 2, 4, 8, 16, 32, 60, 60 s
- total waiting: 182 s (about 3 minutes) before jitter, about 3.3 minutes with 10% jitter
- jitter: a small proportional random amount (0-10% of the delay) added after the cap to avoid collisions. The random source is injectable so tests can use a fixed seed
- run_at = now() + delay, using the database clock. The API server computes the delay when it handles /report
- on the 8th failed attempt, or on any permanent failure, the API server moves the job to dead. If a lease expires on the last attempt, the reaper moves it to dead

## 9. Idempotency

- Clients send an Idempotency-Key header with each job submission
- A duplicate submit (same key, same payload) returns 200 with the original job
- The same key with a different payload is rejected with 422; nothing changes. A hash of the payload (payload_hash) is stored with the key to detect this
- A duplicate submitted while the first request is still being processed returns 409. Mechanically: the second insert waits for the first transaction to finish; if the first hasn't finished within a lock timeout of 2 s, the second request returns 409 (ADR 005)
- Key scope: per client. A key is unique per (client_id, key). The client sends a client_id with each request (for example an X-Client-Id header). It is trusted, not authenticated, because authentication is a non-goal
- Key retention: keys are kept forever (simplest), so the "at most one job per client and idempotency key" invariant holds as written. The keys live in the jobs table

## 10. Handler contract

### Handlers should

- Receive the payload plus the job ID and attempt number
- Return one of three outcomes: success (with an optional result), retryable failure (with an error), or permanent failure (with an error)
- Pass idempotency keys (derived from the job ID, the same on every attempt) to any downstream services
- Be safe to run more than once for the same job
- Have a maximum run time per job type, with a default of 60 s. A handler that passes it is cancelled and counts as a retryable failure

### Failure types (what a handler can report)

- Retryable failure: trying again might work (timeout, network error, 5xx from a downstream service, handler passed its time limit). The job is retried with backoff until max_attempts.
- Permanent failure: no retry can help (invalid payload, 4xx from a downstream service). The job goes straight to dead.

## 11. Failure modes (case and required behavior)

### Submit and API

- API server connection drops: if it drops before the commit, nothing exists and the client retries with the same key. If it drops after the commit but before the response, the retry returns the original job (200).
- Duplicate key submit: return 200 with the original job.
- Same key, different payload: reject with 422; nothing changes.
- Duplicate submit while the first is still being processed: return 409.

### Workers and leases

- Lease expires mid-run: the reaper requeues the job, or marks it dead if attempts are used up. The original worker's later heartbeat or report is rejected, and it cancels the handler.
- Worker crashes: heartbeats stop, the lease expires, and the job is handled as in "Lease expires mid-run".
- Worker does not hold the lease when it reports (job finishes but is not reported): the report changes nothing, and the current lease holder decides the outcome.
- Worker can't reach the API server: it can't claim or renew. It keeps polling, and cancels its handler if it can't renew for longer than the lease.
- A job that always fails: retried with backoff until max_attempts, then dead with its error history.
- Result write fails: the worker retries the report a few times while it holds the lease. If it still fails, it stops, the lease expires, and the job runs again.

### Database and infrastructure

- DB connection drops:
  - Submit: nothing was committed, so the client retries with the same key.
  - Claim: nothing ran, so the worker polls again.
  - Heartbeat: the worker cancels the handler if it can't renew for longer than the lease.
  - Ultimately nothing is lost; no job makes progress while the DB is down.
- Reaper crashes or restarts: expired jobs wait for the next run; nothing is lost. Running two reapers is safe.
- Clocks differ between machines: lease and run_at times use the database clock only.

### Approval and operators

- Approval decided twice: the second decision is refused and logged.
- Two operators act on the same job, or the page shows stale state: the API refuses the second action (state or decision already changed). The page shows the refusal and reloads the job.
- Approval not decided by the deadline: the reaper escalates the job (sets escalated_at). The job keeps waiting and can still be approved or rejected. Nothing is rejected automatically.

## 12. Test plan (draft)

- Chaos runs: kill workers, the API server, the reaper, and the Postgres process during load
- Load tests: empty handler and 100 ms handler, at the scale targets above
- Soak run: 30 minutes at the target rate, recording dead rows (n_dead_tup), table size, and claim p95 over time, to check that vacuum keeps up (ADR 001)
- After every run, check each invariant with a query that returns zero rows when it holds
- One test for each "never happens" case in section 6
- Operator page: check that approve, reject and redrive call the right endpoints, that a refused action shows the message, and that a second operator's action on the same job is refused
- Fake handlers for tests: sleep, flaky, always_fail, crash, needs_approval

## 13. ADR index

One page each: context, options with tradeoffs, decision, consequences. See [the ADR index](adr/README.md) for the full list and statuses.

1. [Postgres as the queue store](adr/001-postgres-as-queue-store.md) (Accepted): Postgres, with jobs claimed using `SELECT ... FOR UPDATE SKIP LOCKED`.
2. [Workers talk to the API server, not to Postgres directly](adr/002-workers-call-api.md) (Proposed): Workers call `/claim`, `/heartbeat` and `/report` on the API server.
3. [Lease length and heartbeat interval](adr/003-lease-and-heartbeat.md) (Proposed): 30 s lease, 7.5 s heartbeat.
4. [Retry curve and jitter](adr/004-retry-backoff-and-jitter.md) (Proposed): 8 attempts, 2 s base doubling, 60 s cap (182 s total waiting), 0-10% jitter.
5. [Duplicate-key handling, key scope and retention](adr/005-duplicate-key-handling.md) (Proposed): Per-client scope, keys kept forever; 200 for a duplicate, 422 for a different payload, 409 while in progress.
6. [Approval placement and timeout](adr/006-approval-placement-and-timeout.md) (Accepted): Gate before the handler; on timeout, escalate (flag) instead of reject.
7. [What happens when a lease expires](adr/007-lease-expiry-requeue.md) (Proposed): Backoff, same as failures.
8. [Durability level](adr/008-durability.md) (Proposed): Single node, default `synchronous_commit`.
9. [Fencing token and redrive](adr/009-fencing-token-and-redrive.md) (Accepted): The attempt number is the fencing token; redrive sets a higher `max_attempts` and never resets `attempt`.
10. [UI scope](adr/010-ui-scope.md) (Proposed): Operator page (list, detail, approve, reject, redrive) built only on the public API, no login.

Other settings (recorded in the ADR that uses them): see [metrics and settings](metrics-and-settings.md).

## 14. Open decisions

None right now.
