# ADR 011: Schema: error history in its own table, and state changes enforced by the database

**Status:** Proposed  
**Date:** 2026-10-07  
**Spec sections:** 4 (invariants), 5 (job model), 6 (states and transitions)  
**Depends on:** ADR 001 (Postgres, vacuum plan), ADR 007 (lease expiry), ADR 009 (fencing token)  

## Context

Writing `db/schema.sql` meant making two choices the spec left open.

1. **Where the error history lives.** Spec section 5 says a job has an error history (attempt, time, type, message). It does not say whether that is a column on the job or a table of its own.
2. **Who enforces the rules for state changes.** Spec section 6 lists 14 allowed transitions, and section 4 lists rules like "attempt never decreases" and "a second approval decision is refused". These can be enforced only by the API server and reaper code, or also by the database itself.

## Decision 1: where the error history lives

### A. A JSON list in a column on the jobs row

**For**

- One row has everything about the job, so a read is one query
- A failure report is a single UPDATE

**Against**

- The row grows with every failure (up to 8 entries of up to 1,000 characters), and every state change rewrites the row. ADR 001 relies on small rows and cheap updates
- Appending to a JSON list is a read-modify-write, so two writers can lose an entry
- Nothing stops two entries for the same attempt, so the two-reapers case needs a separate guard

### B. A separate table, `job_errors`, with the primary key (job_id, attempt) (chosen)

**For**

- The jobs table stays small, which is what the vacuum and fillfactor plan in ADR 001 assumes
- An attempt can end only once, so the primary key refuses a second error entry for the same attempt. That is exactly what happens if two reapers try to requeue the same expired lease. The rule "running the reaper twice requeues once" is enforced by the database
- The invariant checks are plain joins ("a dead job has an error entry for its last attempt")

**Against**

- A failure report now writes two rows (the job and its error entry). They go in one statement and one transaction, so they succeed or fail together
- Reading a job's full history is a second query

## Decision 2: how the database enforces the rules

### A. Only application code

**For**

- No logic in the database

**Against**

- One bug in the API server or reaper can write an impossible row, and nothing notices until an invariant query runs

### B. CHECK constraints only

**For**

- Simple and fast. Good for rules about one row at a time (a running job has a lease; finished_at is set exactly when the state is terminal)

**Against**

- A CHECK sees only the new row, not the old one. It cannot say "attempt may only go up by 1, at a claim" or "this state change is not on the list"

### C. CHECK constraints plus triggers (chosen)

A trigger on UPDATE sees both the old and the new row. It refuses any state change not in spec section 6, any change to attempt except +1 at a claim, a second decision, a second escalation, and changes to the payload or key. A trigger on INSERT makes sure a job starts as queued or waiting_approval with attempt 0.

**For**

- The strongest form of the spec's invariants: the database refuses the change, so the bad row never exists
- Each rule has a test that tries to break it (`db/tests/test_schema.sql`)

**Against**

- Rules now live in two places (the API code and the trigger), and a change to the spec's transition list means a change to the trigger
- The trigger runs on every update. Measured on a laptop-class Postgres 16 with 10 concurrent clients claiming from a 60,000-job queue, claims ran at about 3,200 per second with the trigger and 3,200 to 3,600 without it, against a target of 50 jobs per second. This is not an official test result (that is the load test)
- It does not protect against someone who owns the table: they can disable a trigger. It guards against bugs, not against a malicious operator
- Two rules stay out of the trigger on purpose. "Is the lease really expired?" and "has the approval deadline passed?" depend on the clock, and the same state change (running to queued) is legal for different reasons. The reaper's query keeps those checks

## Consequences

- Three layers, from strongest to weakest: CHECK constraints (one row at rest), triggers (one change), and `db/invariants.sql` (anything across two tables, and the checks that use the test client's records). Each spec invariant is mapped to its layer in the header of `invariants.sql`
- The job-ending rules that need both tables are checked by invariants only: "a dead job has an error entry for its last attempt" and "dead only after a permanent failure or when attempts are used up". Putting them in a trigger would depend on the order of statements inside one transaction
- A queued job must always have an attempt left (`attempt < max_attempts`). This is what forces a redrive to raise `max_attempts` above `attempt`, as ADR 009 requires
- The approval gate is a constraint: a job with an approval deadline must have a recorded decision unless it is waiting_approval. A job cannot be queued, running or finished without passing the gate
- `queued` to `queued` is not an allowed change, so a queued job's `run_at` cannot be edited in place. Nothing in the spec needs that. The tests move time forward by briefly disabling the trigger
- Only the latest redrive is kept (`redriven_by`, `redriven_at`). A job redriven twice shows only the second. A full history would need a log table, which this version does not have
- A success with no result is stored as `{}`, because the spec says a succeeded job always has a result
- Starting values for table storage: fillfactor 70, and autovacuum after 2% of rows change (the default is 20%). ADR 001 asks for both but gives no numbers. The 30-minute soak test decides whether they hold
- Indexes are partial, so each stays small however many finished jobs the table holds: one for the claim query, one for expired leases, one for overdue approvals, and one for the operator page's lists of waiting and dead jobs. Lists of succeeded or queued jobs are not indexed for paging. The operator page is meant for waiting and dead jobs, and a "show me queued jobs" list on a large backlog would be slow. That is a known limit
- `state` is text with a CHECK, not a Postgres enum. A new state is then a one-line constraint change instead of a type migration

## Revisit this decision if

- The soak or load test shows the trigger or the second write costs too much (move the trigger rules into CHECKs and the API, and keep the invariant queries)
- The transition list in spec section 6 changes often (the trigger becomes a maintenance cost)
- The operator page needs the full history of redrives, or other event history (add an event log table; this would be the first step toward the event-sourced design that ADR 001 chose not to build)
- An operator needs to list queued or succeeded jobs on a large table (add an index, and accept the extra write cost)
