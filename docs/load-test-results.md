# Load, soak and chaos test results

Run on 2026-10-15 with the harness in [cmd/loadtest](../cmd/loadtest) (the working tree at commit `86fb784` plus the harness, committed together with this file). The raw reports, with every number and the time series, are in [load-results/](load-results/).

These tests check the targets in spec section 3 and the checks in spec sections 8 and 12. Where the spec gives a number, the result is judged against it. Where it does not, the criterion is the harness's own and is stated next to the result.

## Summary

| Test | Spec target | Result | Verdict |
|---|---|---|---|
| Throughput, 100 ms handler, 5 minutes | 50 jobs/s sustained, 10 workers | 50.0 jobs/s completed (worst 10-second stretch: 49.5). 15,000 of 15,000 jobs succeeded | Pass |
| Maximum throughput, empty handler | Report the number (not pass/fail) | 1,600 jobs/s sustained; fell behind at 2,400 | Reported |
| Start latency, idle system | p95 of 250 ms or less | p95 100 ms (p50 53 ms, p99 104 ms, max 483 ms), 300 jobs | Pass |
| Backlog of 100,000 queued rows | Claim p95 within 2x of the claim p95 with an empty queue | Claim p95 0.79 ms with 100,000 queued, against 0.86 ms with one due job | Pass |
| Soak, 30 minutes at 50 jobs/s | Vacuum keeps up (no number given) | 29 autovacuum runs; dead rows stayed near 4,000; no bloat; latency did not drift | Pass (my criteria) |
| Crash recovery of a killed worker | About 37 s | 36.6 to 37.0 s over 5 kills | Pass |
| Worker paused past its lease | Late report refused | Refused with 409; the old attempt's work never landed | Pass |
| Chaos: kill workers under load | No job lost | 10 kills, 9,000 of 9,000 jobs succeeded | Pass |
| Chaos: kill the engine (API server and reaper) under load | No job lost | 5 kills, 9,000 of 9,000 jobs succeeded | Pass |
| Chaos: kill Postgres under load | Every acknowledged job survives | 2 kills, 9,000 of 9,000 acknowledged jobs present and finished | Pass |

After every run the invariant views in [db/invariants.sql](../db/invariants.sql) returned zero rows, including the check that every acknowledged job exists and is finished.

## How to read the numbers

- **Everything ran on one laptop.** The test harness, the engine, 10 worker processes and Postgres shared one Intel Core i9-9980HK (16 logical CPUs, 16 GB of memory, macOS 26.6.2). A real deployment would put Postgres on its own machine. These numbers are what this system does on this hardware, not a promise about other hardware.
- **Postgres had stock settings**: PostgreSQL 16.15, `shared_buffers` 128 MB, `synchronous_commit` on, `fsync` on. Each run builds a fresh cluster, so a result can be repeated.
- **p50, p95, p99** are percentiles. A p95 of 100 ms means 95 out of every 100 jobs were faster than 100 ms.
- **Start latency** is the time from the job being saved (`created_at`) to its handler beginning. The handler writes its own start time into its result, and the harness subtracts. The harness and the workers run on one machine, so the clocks agree.
- **Open-loop load** means the harness submits at a fixed rate whatever the system's speed, so a slow system builds a queue instead of slowing the clients down. That is how real clients behave.
- Clients retry a failed submit with the same idempotency key for up to 60 s, as spec section 11 asks.

## Throughput: 50 jobs/s for 5 minutes

15,000 jobs at 50 per second, 10 workers, handler sleeping 100 ms.

| | |
|---|---|
| Completed per second (steady window) | 50.0, worst 10-second stretch 49.5 |
| Queue | never more than 4 jobs |
| Time to finish after submitting stopped | 0.5 s |
| Submit call (the API's 201) | p50 2.3 ms, p95 3.9 ms, p99 4.1 ms |
| Start latency under load | p50 51 ms, p95 98 ms, p99 103 ms |
| Whole job, saved to finished | p50 152 ms, p95 199 ms |

The start latency is about 50 ms at the median because a worker polls every 100 ms, so a job waits on average half a poll interval. The system had a lot of room at this rate: the 10 workers were busy about half the time.

## Maximum throughput: empty handler

The harness raised the submit rate in 30-second steps and checked at each step that the jobs finished kept up with the jobs submitted, and that the queue did not grow by more than two seconds' worth.

| Target rate | Completed | Queue at start → end | Start latency p95 | Kept up? |
|---|---|---|---|---|
| 200/s | 200/s | 0 → 4 | 97 ms | yes |
| 400/s | 400/s | 4 → 12 | 93 ms | yes |
| 800/s | 800/s | 0 → 12 | 19 ms | yes |
| 1,200/s | 1,200/s | 0 → 2 | 37 ms | yes |
| 1,600/s | 1,600/s | 0 → 0 | 387 ms | yes |
| 2,400/s | 2,184/s | 0 → 6,473 | 2,581 ms | no |

**The highest rate sustained was 1,600 jobs/s**, which is 32 times the target. The ramp stopped at the first step that fell behind. All 198,002 jobs were still finished, with none lost, once the harness stopped submitting. Because the harness shares the machine, the number on separate machines would differ.

## Start latency on an idle system

300 jobs, one at a time with random 100 to 300 ms gaps between them, so each job lands at a random point in the workers' 100 ms polling cycle and the system is idle in between.

p50 53 ms, p95 **100 ms**, p99 104 ms, max 483 ms. The target is 250 ms at p95. The p99 is 104 ms, so at most 3 of the 300 jobs were slower than that; the max is an outlier.

## Backlog: 100,000 queued rows

The queue was filled to 100,000 due jobs, then the harness made claim calls and timed them.

| Case | Claim p95 |
|---|---|
| Empty queue (nothing to claim, 204) | 0.55 ms |
| Exactly one due job | 0.86 ms |
| **100,000 queued, one client** | **0.79 ms** |
| 100,000 queued, ten clients at once | 1.77 ms |

The claim does not slow down with a deep queue, because it reads the index on (`run_at`, `id`) and stops at the first row.

Two notes on method. First, the spec says "within 2x of the claim time with an empty queue". An empty-queue claim finds nothing, so it does less work than one that returns a job. I compared against the claim of one due job, which is like for like. Against the 204 case instead, the limit would be 1.1 ms and 0.79 ms still passes. Second, the 100,000 rows were inserted with SQL, because 100,000 API submits would take minutes and the claim query cannot tell how a row got there. The rows are ordinary: valid hash, state `queued`, due now. A table statistics refresh (`ANALYZE`) followed the load, as autovacuum would do.

## Soak: 30 minutes at 50 jobs/s

90,000 jobs. All succeeded, none lost.

| Minute | Rows | Dead rows (`n_dead_tup`) | Autovacuum runs | Table size |
|---|---|---|---|---|
| 3 | 9,012 | 4,096 | 2 | 4.0 MB |
| 15 | 44,995 | 4,038 | 14 | 18.4 MB |
| 30 | 89,990 | 4,044 | 29 | 36.3 MB |

A **dead row** is an old version of a row, left behind each time a row is updated. Each job is updated twice (the claim, then the finish), so dead rows pile up until autovacuum (Postgres's cleanup process) removes them. The count stayed near 4,000 for the whole run (peak 5,937) instead of climbing, and autovacuum ran about once a minute.

The criteria below are mine, since the spec says only that vacuum must keep up:

- Autovacuum ran during the soak: **yes, 29 runs**.
- No bloat, meaning bytes per live row at the end are at most 1.5 times the early value: **438 early, 404 at the end**.
- Latency does not drift. Start-latency p95 for each 5-minute window: **97.6, 98.5, 98.4, 98.7, 98.5, 98.5 ms**. The completion-latency p95 stayed at 199 to 201 ms.

## Crash recovery of a killed worker

A worker was killed with `kill -9` while it held a job, five times. The harness timed from the kill to the job running on another worker.

36.6, 36.8, 36.9, 36.9 and 37.0 seconds. The design predicted about 37 s as the worst case: a 30-second lease, up to 5 seconds until the reaper runs, and a 2-second first retry delay (ADR 003 and ADR 007). The measured time is at the top of that range because the harness kills the worker moments after it claims the job, so nearly the whole 30-second lease is still to run.

## Worker paused past its lease

A worker holding a job was frozen with `SIGSTOP`, which stops a process completely, as a long pause or a stopped virtual machine would. After 36.6 s another worker took the job over (attempt 2).

- A late `success` report for attempt 1, sent with the frozen worker's own key, was refused with **409 `lease_lost`**.
- When the worker was resumed, its handler timer had already run out, so it finished at once and sent its own late report. The engine refused that too (the worker's log reads `report refused: the job is no longer ours`).
- The job finished once, on attempt 2, with one `lease_expired` entry. The stored result is attempt 2's, not the late one.

This is the fencing token (ADR 009) doing its job.

## Chaos: killing things under load

Each run: 50 jobs/s for 3 minutes with 10 workers, `kill -9` on a schedule, a calm stretch at the end, then wait for every job to finish. The checks: every acknowledged job is in the table, none given up on, none dead, no duplicates, and the invariant views are clean.

| Killed | How | Jobs | Client retries | Jobs that needed a second attempt | Slowest job |
|---|---|---|---|---|---|
| A random worker | 10 times, down 1 s each | 9,000 | 0 | 3 (lease expired) | 37.2 s (the lease recovery above) |
| The engine (API server and reaper) | 5 times, down 2 s each | 9,000 | 2,329 | 2 (lease expired) | 34.4 s |
| Postgres, all processes | 2 times, down 5 s each | 9,000 | 3,041 | 0 | 7.2 s |

Findings:

- **No job was lost, duplicated, or left unfinished in any run.** A submit whose answer was lost when the engine died was sent again with the same key; the engine answered 200 with the same job, and the table has one row per key (9,000 rows for 9,000 keys).
- **The engine recovered by itself after Postgres came back.** Its connection pool reconnected with no restart. Postgres was accepting connections again 0.3 s after it was started, after replaying its write-ahead log.
- **Workers rode out engine downtime.** Their reports are retried for up to 30 s (ADR 014), so a 2-second outage cost almost nothing.
- Only a few jobs were interrupted in the worker run (3 of 10 kills landed on a job in progress), because at 50 jobs/s the workers are busy about half the time.

## What the harness got wrong first

Two of my first versions were wrong, and the results above are from the fixed ones.

1. **Maximum throughput.** The first version submitted as fast as 64 connections could, which was 5,491 jobs/s. The queue reached 606,000 jobs, the 3-minute drain limit ran out with 182,000 jobs unfinished, and three checks failed. The failures were the test's, not the engine's. It also reported "431 jobs/s, the workers were the limit", which was wrong: the submitters and workers were fighting for one Postgres. The workers finished about 2,300 jobs/s once submitting stopped. The ramp above replaced it.
2. **The pause check.** It looked only for a cancelled handler and treated the text `outcome=success` in the refusal log line as an accepted report. The worker had behaved correctly. The check now accepts either legitimate outcome and looks for an accepted report specifically.

## Limits of these tests

- **One machine.** Network latency between the engine and Postgres is not tested. The harness takes CPU from the system under test.
- **`kill -9` of Postgres is a process crash, not power loss.** Every committed row was on disk (`fsync` and `synchronous_commit` on), but the operating system survived, so these runs do not show behaviour when a machine loses power. ADR 008 already says the design does not survive losing the disk.
- **The soak table is small.** It reached 90,000 rows (47 MB with indexes). Behaviour with millions of finished rows, which ADR 011 mentions, is not tested. The backlog test measured claim speed with 100,000 queued rows, not a large table of finished ones.
- **The reaper was killed together with the engine.** It runs inside the engine, so there is no run that kills the reaper alone. Its restart behaviour is the same code path.
- **One handler speed.** The 100 ms handler was tested at the 50 jobs/s target only, not pushed to its own ceiling (about 95 jobs/s with 10 workers, from the arithmetic in the spec).
- **Short chaos runs.** Three minutes each, with 2 to 10 kills. This shows the mechanisms work, not that nothing rare can go wrong.
- **Start latency under chaos** is not judged. The target is for an idle system only.

## Reproducing it

The harness needs the Postgres programs (`initdb`, `postgres`) on the machine. It builds the engine and worker, creates a fresh cluster in the directory you give it (wiping that directory first), and uses ports 55432 and 58080.

```
export PATH="$(pg_config --bindir):$PATH"
go run ./cmd/loadtest start-latency  -dir /tmp/lt
go run ./cmd/loadtest throughput     -dir /tmp/lt                 # 50/s for 5 minutes
go run ./cmd/loadtest max-throughput -dir /tmp/lt -handler noop
go run ./cmd/loadtest backlog        -dir /tmp/lt
go run ./cmd/loadtest soak           -dir /tmp/lt                 # 30 minutes
go run ./cmd/loadtest lease-recovery -dir /tmp/lt
go run ./cmd/loadtest pause-worker   -dir /tmp/lt
go run ./cmd/loadtest chaos-worker   -dir /tmp/lt                 # also chaos-engine, chaos-postgres
```

Add `-out file.json` to keep the report. The exit status is 1 if any check fails.
