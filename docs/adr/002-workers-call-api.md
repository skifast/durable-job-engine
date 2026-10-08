# ADR 002: Workers talk to the API server, not to Postgres directly

**Status:** Accepted (2026-10-07)  
**Date:** 2026-10-06  
**Spec sections:** 6 (transitions T6-T13), 7 (components), 8 (retry policy)  
**Depends on:** ADR 001 (Postgres as the queue store)  

## Context

Workers need to claim a job, renew its lease, and report the outcome. Each of those changes a row in the jobs table, and each has rules:

- a claim picks one due job, sets the lease and increments attempt (T6)
- a heartbeat and a report are accepted only if lease_owner and attempt match (T7-T11)
- a failed report either requeues the job with a backoff delay or moves it to dead, depending on the attempt count (T9-T11)

The question is where those rules live: in the workers (which connect to Postgres and run the SQL themselves), or in one service (the API server) that the workers call.

The retry-or-dead rule is what the engine does when an attempt ends in failure. In order:
1.  A permanent failure sends the job to dead (T10).
2.  A retryable failure, or a lease that expired, on the last attempt (attempt >= max_attempts) sends the job to dead (T11, T13).
3.  Any other retryable failure, or lease expiry, puts the job back in the queue with a backoff delay (T9, T12).

## Options

### A. Workers call the API server (/claim, /heartbeat, /report)

**For**

- The rules exist in one place. The fencing check, the retry-or-dead rule and the backoff calculation are written once, so a worker can't get them wrong, and the reaper can share the same rule (spec section 7)
- Workers hold no database credentials and don't know the schema. A worker runs handler code, which is the part most likely to be buggy
- The schema can change, or the store can be replaced, without touching workers (ADR 001, Consequences)
- The backoff delay and the injectable random source live in one process, which keeps retry tests simple
- Workers can be written in any language, since they only speak HTTP

**Against**

- One more network hop on every claim, heartbeat and report. This counts against the 250 ms start-latency target
- The API server becomes a dependency of the worker path. If it is down, workers can't claim or renew
- One more piece to build, run and test

### B. Workers connect to Postgres directly and run the SQL

**For**

- Fewer moving parts and no extra hop, so lower latency
- Workers that stay connected could use LISTEN/NOTIFY to be told about new jobs, instead of polling

**Against**

- Every worker needs database credentials and a copy of the schema, and a bug in a worker can write any state
- The retry-or-dead rule and the fencing check would be copied into every worker (and every language used)
- Each worker holds database connections, so connection count grows with the number of workers
- Changing the schema means updating and redeploying all workers

### C. Workers call stored functions in Postgres (claim(), heartbeat(), report())

**For**

- The rules live in one place, inside the database, and run in one transaction
- Fewer round trips than separate statements

**Against**

- Workers still need database credentials and a direct connection
- Rules written in SQL are harder to test and version than service code, and the injectable random source for backoff is awkward
- The project is meant to show backend service design, and this moves the logic out of the service

## Decision

Use A. Workers call /claim, /heartbeat and /report on the API server, and only the API server talks to Postgres. This keeps the fencing check and the retry-or-dead rule in one place and keeps handler code away from the database. (The retry-or-dead rule is defined in Context.) The cost is one extra hop, which is acceptable at a few hundred requests per second, and which the load tests will measure.

## Consequences

- The start-latency target of 250 ms (p95, idle) now includes the poll wait (up to 100 ms) plus an HTTP request and one transaction. Measure this in the load tests.
- The API server must stay stateless: all state is in Postgres, so more than one API instance can run and a restart loses nothing.
- If the API server is unreachable, a worker can't claim or renew, and it cancels its handler once it can't renew for longer than the lease. This is already a listed failure case (spec section 11).
- Authentication is a non-goal, so the worker endpoints are not authenticated in this version. Workers hold no database credentials, but anyone who can reach the API can call /claim. Treat the API as a trusted-network service.
- Poll traffic is HTTP requests rather than database connections: with 10 workers polling every 100 ms that is about 100 /claim requests per second, plus one heartbeat every 7.5 s per running job.
- The API server applies fencing, so heartbeats and reports with the wrong lease_owner or attempt are refused there (invariant in spec section 4).

## Revisit this decision if

- The load tests show the extra hop or the polling traffic stops the system meeting the latency or throughput targets
- Workers need push notification of new jobs instead of polling (the case where direct LISTEN/NOTIFY, or long polling in the API, would help)
- The API server becomes the thing that fails most often, and the worker path needs to be simpler
