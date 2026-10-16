# Durable Job Engine

A background job engine built on Postgres, with worker leases, retries, fencing and a human approval step. The goal is to build the hard parts by hand (the parts a managed queue like SQS gives you for free) and to understand every decision behind them.

## Status

**The design is done and the database layer is written and tested. The API server is partly written (submit, read one job, claim, heartbeat, report) and so is the reaper; the worker, the operator calls and the operator page are not started.**

- The spec is written ([docs/spec.md](docs/spec.md)).
- 13 architecture decision records are written and accepted ([docs/adr](docs/adr/README.md)).
- Every number (lease, retries, targets, limits) is listed with its reasoning in [docs/metrics-and-settings.md](docs/metrics-and-settings.md).
- The database schema, the invariant checks and their tests are written ([db](db/README.md)), and the API contract is approved ([docs/api.md](docs/api.md)). The tests drive every state change, try to break each rule, and plant a violation for each of the 20 invariant checks to confirm it is caught.
- The API server is written in Go ([ADR 012](docs/adr/012-implementation-language.md)). It has authentication, submit, read one job, claim, heartbeat and report, tested end to end against a real Postgres (see [Running it](#running-it)). See the roadmap below for what is left.

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
| [004](docs/adr/004-retry-backoff-and-jitter.md) | 8 attempts, exponential backoff with a 60 s cap and jitter | Accepted |
| [005](docs/adr/005-duplicate-key-handling.md) | Per-client idempotency keys, kept forever | Accepted |
| [006](docs/adr/006-approval-placement-and-timeout.md) | Approval is a gate before the handler; timeouts escalate, never reject | Accepted |
| [007](docs/adr/007-lease-expiry-requeue.md) | Lease expiry is retried with backoff, like a failure | Accepted |
| [008](docs/adr/008-durability.md) | Single node, `synchronous_commit` on | Accepted |
| [009](docs/adr/009-fencing-token-and-redrive.md) | The attempt number is the fencing token | Accepted |
| [010](docs/adr/010-ui-scope.md) | Operator page built on the public API, with an operator API key | Accepted |
| [011](docs/adr/011-schema-error-history-and-enforced-transitions.md) | Error history in its own table; the database refuses illegal state changes | Accepted |
| [012](docs/adr/012-implementation-language.md) | Go for the API server, worker and reaper | Accepted |
| [013](docs/adr/013-authentication-api-keys.md) | API keys for every caller, with roles (client, worker, operator) | Accepted |

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
- [x] Architecture decision records (13, all accepted)
- [x] `schema.sql` (jobs table with constraints and triggers), `invariants.sql` (the zero-row queries) and their tests
- [x] API contract ([docs/api.md](docs/api.md))
- [x] API server, first part: authentication, submit, read one job, claim, heartbeat, report, health check, and the `jobkeys` tool for API keys
- [ ] API server, second part: list jobs, approve, reject, redrive
- [ ] Worker and handler contract
- [x] Reaper: takes back expired leases (T12, T13) and escalates overdue approvals (T5), every 5 s, up to 100 jobs of each kind per pass, one transaction per job
- [ ] Operator page
- [ ] Load, soak and chaos tests, with results written up

## How this is being built

I make the design decisions and review everything. I use AI tools (Claude) to help draft documents and, later, to help write code, with each decision recorded in an ADR first so I can explain it. The spec and ADRs come before the code, and the invariants come before the implementation.

## Running it

You need Go 1.24 or newer and a Postgres 13 or newer. With Docker:

```
docker run --rm -d --name jobengine-pg -e POSTGRES_PASSWORD=dev -p 5432:5432 postgres:16
export TEST_DATABASE_URL='postgres://postgres:dev@localhost:5432/postgres'

go mod tidy      # first time only: downloads the dependencies and writes go.sum
go test ./...    # each test builds its own empty database from db/schema.sql
```

The tests need a real Postgres on purpose: the database enforces the rules (ADR 011), so a mock would hide the thing being tested. After each API test the invariant views (`db/invariants.sql`) are checked and must return zero rows.

To run the engine by hand:

```
createdb -h localhost -U postgres jobengine_dev
psql -h localhost -U postgres -d jobengine_dev -v ON_ERROR_STOP=1 -f db/schema.sql
export DATABASE_URL='postgres://postgres:dev@localhost:5432/jobengine_dev'
export JOB_TYPES_FILE=examples/job-types.json

go run ./cmd/jobkeys create -name my-client -role client     # prints the key once
go run ./cmd/jobkeys create -name worker-1 -role worker
go run ./cmd/engine                                          # listens on :8080

curl -s -X POST localhost:8080/jobs \
  -H "Authorization: Bearer <the client key>" -H "Idempotency-Key: $(uuidgen)" \
  -d '{"type": "send_email", "payload": {"to": "a@example.com"}}'
```

The engine speaks plain HTTP. Put it behind a TLS proxy for anything beyond local use ([ADR 013](docs/adr/013-authentication-api-keys.md)).

## Repository layout

```
cmd/
  engine/                    the API server, with the reaper running inside it
  jobkeys/                   create, list and revoke API keys (needs database access)
internal/
  api/                       HTTP handlers, authentication, the error shape from docs/api.md
  reaper/                    the 5-second loop that calls the store's reaper functions
  store/                     all the SQL; the only package that talks to Postgres
  retry/                     the backoff delay from ADR 004
  canonical/                 RFC 8785 canonical JSON, used for the idempotency hash
  config/                    reads the job-type file
testutil/                    gives tests a fresh real database
examples/job-types.json      an example job-type file
db/
  schema.sql                 tables, indexes, constraints and triggers
  invariants.sql             the correctness rules as queries that must return zero rows
  tests/test_schema.sql      tests for the schema and the invariant queries
docs/
  spec.md                    the full specification
  api.md                     the API contract: every endpoint, request, response and error code
  metrics-and-settings.md    every number, with reasoning
  adr/                       architecture decision records
```
