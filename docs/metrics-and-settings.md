# Metrics and settings

Every number in the spec and the ADRs, in one place: what it is, what value it has, why, and where it is decided. If a number changes, change it here, in the spec, and in the ADR that owns it.

## Terms used below

| Term | Meaning |
|---|---|
| Throughput | How many jobs the system finishes per second. |
| Latency | How long one thing takes (for example, how long a job waits before a worker starts it). |
| p95 | The 95th percentile: 95 out of 100 measurements are at or below this value. It ignores the slowest 5%, so one freak delay doesn't hide the typical behavior. |
| Backlog | Jobs that are queued and waiting for a worker. |
| Lease | A time limit on a worker's claim of a job. If it runs out, the job can go to another worker. |
| Heartbeat | A message from a worker saying "I'm alive, extend my lease." |
| Jitter | A small random amount added to a wait so that many jobs don't retry at the same instant. |
| Empty handler | A handler that does no work and reports success immediately. Used to measure the engine's own speed. |
| Attempt | One run of a job by a worker. It counts up on every claim. |

## 1. Performance targets

Defined in spec section 3. Tested in the load and soak runs (spec section 12).

| Setting | Value | Why | Owner |
|---|---|---|---|
| Throughput | 50 jobs per second, sustained, with 10 workers on one Postgres | Each worker runs one job at a time. With a 100 ms handler plus about 100 ms of request time, one job takes about 200 ms, so a worker does about 5 jobs per second and 10 workers do about 50. | Spec section 3 |
| Test length | 5 minutes at the target rate | Long enough to see steady behavior rather than a warm-up burst. | Spec section 3 |
| Empty-handler run | Reports the maximum throughput; not pass/fail | Shows how fast the engine itself can go, and how much room is left above the target. | Spec section 3 |
| Start latency | 250 ms or less at p95, on an idle system | The wait for a worker's next poll (up to 100 ms) plus the claim request and its commit. 250 ms leaves room for a slow request or two. | Spec section 3, ADR 002 |
| Backlog | 100,000 queued rows; claim p95 within 2x of the claim time with an empty queue | A deep queue must not make claiming slow. The claim query uses an index on state and run_at. | Spec section 3, ADR 001 |
| Soak run | 30 minutes at the target rate | Records dead rows, table size and claim p95 over time, to check that vacuum keeps up. | ADR 001 |

## 2. Leases, heartbeats and polling

| Setting | Value | Why | Owner |
|---|---|---|---|
| Lease length | 30 seconds (adjustable by configuration) | The default visibility timeout in SQS. A crashed worker's job waits up to 30 s, but a worker that is only briefly slow doesn't lose its job. | ADR 003 |
| Heartbeat interval | 7.5 seconds (lease / 4) | Gives four heartbeats per lease, so two in a row can be lost without losing the job. If the lease setting changes, the heartbeat should change with it. | ADR 003 |
| Missed heartbeats survived | 2 in a row | A heartbeat every H seconds on a lease of L seconds survives m misses when (m + 1) x H < L. Here 3 x 7.5 = 22.5 s, which is below 30 s. A third miss would put the next beat at 30 s, too late. | ADR 003 |
| Poll interval | 100 ms | An idle worker asks for a job ten times a second. The longest a job waits for the next poll is 100 ms, which fits inside the 250 ms start-latency target. | ADR 002 |
| Reaper interval | 5 seconds | How often the reaper looks for expired leases and overdue approvals. It adds directly to crash-recovery time. | Spec section 7, ADR 003 |
| Worker concurrency | 1 job at a time per worker | Keeps heartbeats and cancellation simple. Throughput comes from running more workers. | Spec section 7 |

### Worked example: a worker crashes mid-job

The worst-case time before the job is running again on another worker:

| Step | Time |
|---|---|
| Lease runs out | up to 30 s |
| Reaper notices | up to 5 s |
| Retry delay after the first failure | 2 s (plus 0-10% jitter) |
| Another worker's next poll | up to 0.1 s |
| **Total, before the job's own run time** | **about 37 s** |

## 3. Retries

Defined in spec section 8 and ADR 004. A lease that expires uses the same delays (ADR 007).

| Setting | Value | Why |
|---|---|---|
| max_attempts | 8 total runs, which means 7 waits | Covers a downstream outage of about 3 minutes. Longer outages end in dead, and an operator redrives. |
| Base delay | 2 seconds, doubling each time | A quick first retry for a brief problem, and a fast backoff if it persists. |
| Cap | 60 seconds | Without it the last two waits would be 64 s and 128 s. |
| Jitter | 0-10% of the delay, added after the cap | Spreads retries a little. The random source is injectable so tests are repeatable. |
| Total waiting | 182 seconds (about 3 minutes); about 3.3 minutes with 10% jitter | The sum of the seven waits below. |

The formula: delay = min(2 x 2^(attempt - 1), 60 s) + jitter, where `attempt` is the number of the attempt that just failed.

| Attempt that failed | Delay before next attempt | Total waited so far | With the most jitter (+10%) |
|---|---|---|---|
| 1 | 2 s | 2 s | 2.2 s |
| 2 | 4 s | 6 s | 6.6 s |
| 3 | 8 s | 14 s | 15.4 s |
| 4 | 16 s | 30 s | 33.0 s |
| 5 | 32 s | 62 s | 68.2 s |
| 6 | 60 s (would be 64 s uncapped) | 122 s | 134.2 s |
| 7 | 60 s (would be 128 s uncapped) | 182 s | 200.2 s |
| 8 | none: the job goes to dead | | |

Because the jitter is added after the cap, the last waits can reach 66 s. The total time from the first failure to dead also includes the run time of each attempt (up to 60 s each, so up to about 11 minutes in the worst case).

## 4. Handlers and data limits

| Setting | Value | Why | Owner |
|---|---|---|---|
| Handler maximum run time | 60 seconds by default, set per job type | Stops a stuck handler. A handler that passes it is cancelled and counts as a retryable failure. The lease is renewed by heartbeats, so the lease length does not limit run time. | Spec section 10 |
| Result size | 64 KB of JSON; larger output is stored elsewhere and referenced | Keeps rows small, so updates and vacuum stay cheap. | Spec section 5 |
| Error message length | 1,000 characters | Keeps the error history readable and the rows small. | Spec section 5 |
| Table growth | About 4.3 million rows a day at 50 jobs per second (50 x 86,400) | Idempotency keys are kept forever in the jobs table, so rows are never deleted. Fine for tests, not for long-term running. | ADR 001, ADR 005 |

## 5. Approvals

| Setting | Value | Why | Owner |
|---|---|---|---|
| Approval deadline | 24 hours after submit, set per job type | After this the reaper flags the job as escalated (once). It does not reject it. | ADR 006 |
| Escalation | Sets `escalated_at` once; the job stays waiting | The job shows in the "escalated approvals" list on the operator page. No notification is sent. | ADR 006 |

## 6. Idempotency

| Setting | Value | Why | Owner |
|---|---|---|---|
| Key scope | Per client: unique on (client_id, key) | Two clients can use the same key text without clashing. `client_id` is trusted, not verified. | ADR 005 |
| Key retention | Forever | Keeps "at most one job per client and key" true with no exceptions. | ADR 005 |
| Lock timeout for a duplicate in progress | 2 seconds | If the first request's transaction hasn't finished in 2 s, the duplicate gets 409 and retries. | ADR 005 |
| Response codes | 201 new job, 200 same key and payload, 422 same key with a different payload, 409 first request still in progress | Lets a client tell a retry from a mistake. | ADR 005 |

## 7. Durability

| Setting | Value | Why | Owner |
|---|---|---|---|
| synchronous_commit | on (the default) | A 201 or 200 is returned only after the commit has reached disk. | ADR 008 |
| fsync | on | Turning it off could corrupt the database in a crash. | ADR 008 |
| Commit rate at the target | About 150 commits per second (50 jobs x about 3 commits each) | One disk handles this easily, because Postgres groups concurrent commits. | ADR 008 |
| Replicas | None: one node | Losing the disk loses everything since the last backup. Failover is a non-goal. | ADR 008 |

## 8. How each number gets checked

| What is checked | How |
|---|---|
| Throughput and start latency | Load test, 5 minutes, with the 100 ms handler. Record p95 start latency. |
| Maximum speed | Same test with an empty handler. Report the number; no pass/fail. |
| Backlog behavior | Fill the queue to 100,000 rows, then compare claim p95 with an empty queue. |
| Vacuum keeps up | 30-minute soak run. Watch dead rows (`n_dead_tup`), table size and claim p95. |
| Lease and heartbeat | Pause a worker past its lease and confirm its late report is refused. Kill a worker and time how long until the job runs elsewhere (about 37 s expected). |
| Retry schedule | Unit test of the delay function with a fixed random seed: delays must match the table above and jitter must stay within 0-10%. |
| Durability | Record every job the API acknowledged, kill -9 Postgres under load, restart, and confirm every acknowledged job is in the table. |
| Invariants | After every run, each invariant in spec section 4 is checked by a SQL query that must return zero rows. |
