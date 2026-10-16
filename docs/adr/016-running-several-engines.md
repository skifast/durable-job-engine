# ADR 016: Running several engines

**Status:** Accepted (2026-10-16)  
**Date:** 2026-10-16  
**Spec sections:** 3 (scale), 7 (components), 11 (failure modes)  
**Depends on:** ADR 002 (workers call the API, which stays stateless), ADR 003 (the reaper), ADR 009 (the fence), ADR 011 (the database enforces the rules)  
**Changes:** the engine gains a `DB_MAX_CONNS` setting, and `/healthz` reads from the jobs table  

## Context

Until now the engine ran as one process, and every test used one engine. ADR 002 said the API server must stay stateless so that more than one could run, and the reaper was written to be safe when two run at once, but neither was ever tried with several engines under load. Running several engines removes the engine as a single point of failure and lets one be restarted while the others serve.

Terms. A **load balancer** (LB) is a front door that spreads requests over several engines and stops sending to one that fails its health check. A **connection pool** is the set of database connections an engine keeps open.

This ADR is about the stateless tier only: several engines, and workers on other hosts, all talking to one Postgres. Postgres itself stays a single node (ADR 008), so this does not remove the database as a single point of failure.

## Decisions

### 1. `DB_MAX_CONNS`, default 10

Each engine opens at most `DB_MAX_CONNS` connections to Postgres. The default is 10. Until now the limit was the pgx library's default, the larger of 4 and the CPU count: 16 on the test laptop, 64 on a big server, so a bigger machine quietly opened more connections per engine and several engines would run into Postgres's limit.

The rule for sizing: **engines × `DB_MAX_CONNS` plus a margin must stay under Postgres's `max_connections`**. The default `max_connections` is 100, but Postgres reserves 3 of them for superusers, so about 97 are usable. The margin covers anything else that connects, such as a psql session, a backup or a monitoring tool. With the defaults, about 9 engines fit before this needs tuning.

Considered and not chosen: a setting that keeps the old default. Nothing would change by default, but a large machine would keep silently opening dozens of connections per engine, which is the failure that prompted this.

The load results in `docs/load-test-results.md` before this ADR used a pool of 16. Whether a pool of 10 lowers an engine's throughput ceiling is **not settled**: the rerun of the maximum-throughput ramp could not be completed on the test laptop because it overheated and throttled (see the results document, "What could not be measured"). At 50 jobs/s, and in a test at 400 jobs/s with only 12 connections in all, the cap made no difference. The default should be revisited once a clean measurement exists.

### 2. Every engine runs a reaper

The reaper (T5, T12, T13) runs inside each engine. Several reapers at once is safe: each re-checks a job under a row lock and skips one that is locked or already handled, the error entry's primary key (job_id, attempt) refuses a second requeue, and the escalation is one `UPDATE` that only applies while `escalated_at` is empty. Tests release three reapers at the same instant, round after round, over 1,000 expired leases and over 500 overdue approvals. Every job is requeued once and escalated once.

If an engine dies, the others carry on, with no election to go wrong and no delay while a new leader is chosen.

The cost is redundant queries every five seconds from every engine. A second cost, measured: reapers fired at the same instant do not add up. Each lists the oldest 100 expired leases, so reapers that fire together spend their pass on the same 100 jobs, and only one requeues each. Three reapers released together recovered 1,000 leases in 10 passes, the same as one reaper. Reapers on different timers, which is what separate engines have, do add up: with 1,000 expired leases, one engine took 49.1 and 49.3 seconds and three engines took 19.0 and 19.0 seconds (about 2.6 times faster), because by the time a second reaper looks, the first has usually finished its 100.

Considered and not chosen: one elected reaper using a Postgres advisory lock. It is tidier, but it adds a failover delay when the leader dies, and the case of a leader that hangs without dying has to be handled. That is a lot of machinery for little gain at this scale.

### 3. A load balancer in front, and workers see one address

Workers, clients and the operator page use one address. Nothing in the worker changes. The LB checks each engine with `GET /healthz`.

Considered and not chosen: the worker taking a list of engine addresses and failing over itself. It needs no LB, but it adds code to the worker, and clients and the operator page would still need their own answer.

Be straight about what this does and does not give:

- **The load balancer in the tests is a stand-in.** It is a small round-robin proxy in the test harness (`cmd/loadtest/lb.go`), not something to run in production. Use a real one (nginx, HAProxy, a cloud load balancer).
- **The claim is "no single engine is a point of failure".** The load balancer itself still is one unless it is run redundantly, and that is left to whoever deploys it. Postgres is also still a single point of failure (ADR 008).
- **The tests ran every engine on one laptop.** They show that the mechanisms work. They do not show a speed-up, because the engines, the workers, the proxy and Postgres all share the same CPU. The engine-kill and rolling-restart results should be rerun on separate hosts before being quoted as a deployment result.

### 4. What `/healthz` checks, and what it does not

An engine answers 200 only if it can run `SELECT EXISTS (SELECT 1 FROM jobs LIMIT 1)` within 2 seconds, and 503 `database_unavailable` otherwise. It originally ran `SELECT 1`, which does not touch a table. A probe found the gap: with the jobs table locked (as a migration would lock it), `/healthz` answered 200 in 21 ms while a real query waited. A load balancer would have kept sending traffic to an engine that could not serve it. The check now reads from `jobs`, so a lock on it makes the engine report unhealthy, and it recovers the moment the lock is released.

Tested cases: the database unreachable, a locked `jobs` table, every pool connection busy (answers 503 after the 2-second limit, so the probe is never left hanging), and connections killed by the server (the pool replaces them and the check recovers).

What `/healthz` does not check, so the next reader knows:

- **Whether this engine's reaper is running.** A hung reaper does not make an engine unhealthy. With a reaper in every engine, the others cover for it, and the failure would not stop jobs from being recovered.
- **Whether the schema is the right version.** It only needs the table to be readable.
- A load balancer should probe with a timeout of at least 2.5 seconds, longer than the engine's own 2 seconds, or it will report timeouts the engine would have answered with a clean 503.

## Defaults set with these decisions

| Setting | Value | Reason |
|---|---|---|
| `DB_MAX_CONNS` | 10 per engine | Fixed, not the CPU count (decision 1) |
| Harness proxy health check | every 250 ms, 3 s timeout, one failure takes an engine out, one success puts it back | Quick to react, so a killed engine stops receiving traffic within a quarter of a second |
| Harness proxy on a refused connection | try the next engine | Nothing was sent, so the request is safe to retry |
| A killed engine's in-flight requests | fail, and the client retries with the same idempotency key | At-least-once submit (ADR 005) is already designed for this |

## Consequences

- An engine can be restarted, upgraded or killed while the others serve. The tests below measure what clients saw when that happened.
- Total database connections are bounded: engines × `DB_MAX_CONNS`. A run's sampler records the peak, and the harness checks it.
- A request is not tied to an engine, since no state lives in one. The operator page can be loaded from any engine.
- Throughput is still limited by one Postgres. More engines do not raise that ceiling.
- A rolling restart means stopping an engine with SIGTERM: it finishes the requests it is serving (up to 10 seconds, the engine's shutdown limit) and exits. A load balancer should take it out of rotation first, or it will see refused connections for a moment. The harness proxy does this within a quarter of a second.

## How it was tested

See `docs/load-test-results.md`, section "Several engines". In short: three engines at 50 jobs/s for five minutes; one engine killed at a time under load; a rolling restart under load; the same idempotency key sent to different engines at the same instant; three reapers released together over 1,000 expired leases and 500 overdue approvals; and the engines' connection count against their limit.

## Revisit this decision if

- Postgres itself must survive a machine loss (ADR 008): that needs replication and failover, which is a separate piece of work
- Reapers become a measurable load, or the redundant passes slow recovery (consider an elected reaper or a candidate list that different reapers spread over)
- Workers cannot be put behind a load balancer (then the worker takes a list of engines)
