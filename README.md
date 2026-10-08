# Durable Job Engine

A background job engine built on Postgres, with worker leases, retries, fencing and a human approval step. The goal is to build the hard parts by hand (the parts a managed queue like SQS gives you for free) and to understand every decision behind them.

## Status

**Design phase. No implementation yet.**

- The spec is written ([docs/spec.md](docs/spec.md)).
- 10 architecture decision records are written ([docs/adr](docs/adr/README.md)): 6 accepted, 4 proposed and under review.
- Every number (lease, retries, targets, limits) is listed with its reasoning in [docs/metrics-and-settings.md](docs/metrics-and-settings.md).
- Implementation starts with the database schema and the invariant checks (see the roadmap below).

## What it is designed to do

- Accepts jobs through an API, with an idempotency key so a client retry never creates a second job.
- Stores every job in Postgres and hands each one to exactly one worker at a time.
- Gives the worker a time-limited **lease**, renewed by **heartbeats**. If the worker dies, the lease runs out and a **reaper** puts the job back in the queue.
- Retries failed jobs with exponential backoff and jitter, and moves jobs that keep failing to a **dead** state with their error history.
- Uses a **fencing token** so a slow or paused worker can't overwrite the result of the worker that took over.
- Supports a **human approval** step: a job can wait for a person to approve or reject it before any worker runs it. An **operator page** handles approvals, job inspection and redriving dead jobs.

Delivery is at least once, so handlers must be safe to run more than once. Exactly-once side effects are a non-goal.

## How the pieces fit

```
Clients        - POST /jobs -------------------------┐
Workers        - /claim, /heartbeat, /report ---------┤
Operator page  - list, approve, reject, redrive ------┤
                                                      ▼
                                 Engine = API server + reaper
                                                      │
                                                      ▼
                                       Postgres (the jobs table)
```

Workers never talk to the database directly (ADR 002). A job moves through six states: `queued`, `waiting_approval`, `running`, `succeeded`, `dead` and `rejected`. All 14 allowed transitions are listed in [spec section 6](docs/spec.md#6-states-and-transitions), and each one will be covered by a test.

## Key design decisions

| ADR | Decision | Status |
|---|---|---|
| [001](docs/adr/001-postgres-as-queue-store.md) | Postgres as the queue store | Accepted |
| [002](docs/adr/002-workers-call-api.md) | Workers call the API, not Postgres | Accepted |
| [003](docs/adr/003-lease-and-heartbeat.md) | 30 s lease, 7.5 s heartbeat | Accepted |
| [004](docs/adr/004-retry-backoff-and-jitter.md) | 8 attempts, exponential backoff with a 60 s cap and jitter | Proposed |
| [005](docs/adr/005-duplicate-key-handling.md) | Per-client idempotency keys, kept forever | Proposed |
| [006](docs/adr/006-approval-placement-and-timeout.md) | Approval is a gate before the handler; timeouts escalate, never reject | Accepted |
| [007](docs/adr/007-lease-expiry-requeue.md) | Lease expiry is retried with backoff, like a failure | Proposed |
| [008](docs/adr/008-durability.md) | Single node, `synchronous_commit` on | Accepted |
| [009](docs/adr/009-fencing-token-and-redrive.md) | The attempt number is the fencing token | Accepted |
| [010](docs/adr/010-ui-scope.md) | Operator page built on the public API | Proposed |

## Targets

| Target | Value |
|---|---|
| Throughput | 50 jobs per second with 10 workers on one Postgres |
| Start latency | 250 ms or less (p95, idle system) |
| Backlog | 100,000 queued jobs without slowing claims (p95 within 2x of an empty queue) |
| Durability | An acknowledged job survives crashes of the API, workers, reaper and Postgres. It does not survive losing the disk |

The correctness rules are written as invariants in [spec section 4](docs/spec.md#4-invariants). Each one will become a SQL query that must return zero rows after every test run, including runs where workers, the API and Postgres are killed under load.

## Roadmap

- [x] Spec
- [x] Architecture decision records, drafted (6 of 10 accepted)
- [ ] Review and accept the remaining ADRs
- [ ] `schema.sql` (jobs table with constraints) and `invariants.sql` (the zero-row queries)
- [ ] API server: submit, claim, heartbeat, report
- [ ] Worker and handler contract
- [ ] Reaper
- [ ] Operator page
- [ ] Load, soak and chaos tests, with results written up

## How this is being built

I make the design decisions and review everything. I use AI tools (Claude) to help draft documents and, later, to help write code, with each decision recorded in an ADR first so I can explain it. The spec and ADRs come before the code, and the invariants come before the implementation.

## Repository layout

```
docs/
  spec.md                    the full specification
  metrics-and-settings.md    every number, with reasoning
  adr/                       architecture decision records
```
