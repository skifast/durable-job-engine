# ADR 008: Durability level

**Status:** Accepted (2026-10-07)  
**Date:** 2026-10-06  
**Spec sections:** 3 (durability), 4 (no job is lost), 12 (chaos tests)  
**Depends on:** ADR 001 (Postgres)  

## Context

The engine tells a client "201, your job is saved". After that, the job must not be lost. How strong that promise is depends on when Postgres considers a commit finished, and on how many copies of the data exist. Stronger promises cost speed and extra machines.

## Options

### A. One Postgres node, default settings (synchronous_commit on)

A commit returns only after Postgres has written the change to its write-ahead log on disk.

**For**

- Survives a crash or kill of the Postgres process, the API, the workers and the reaper
- Nothing extra to run

**Against**

- Does not survive losing the disk or the machine
- Every commit waits for a disk flush (Postgres groups concurrent commits, so this is cheap at 50 jobs per second)

### B. One node with synchronous_commit off

**For**

- Faster commits, because they don't wait for the disk

**Against**

- A crash can lose the last few commits, up to about three times wal_writer_delay (default 200 ms, so about 0.6 s). The database stays consistent, but a client was told "201" for a job that is gone. That breaks "no acknowledged job is lost"

### C. A standby with synchronous replication

**For**

- A commit is also on a second machine, so losing the first loses nothing

**Against**

- A second machine, plus failover, which is a non-goal. Every commit also waits for the standby, which adds latency

### D. A standby with asynchronous replication

**For**

- A second copy exists

**Against**

- The standby can be behind, so a failover can lose the latest commits. It adds machines and failover work for a guarantee that is still not complete

## Decision

Use A: a single Postgres node with the default settings (synchronous_commit on, fsync on). A 201 or a 200 is only returned after the commit has finished.

## Consequences

- The promise is exactly what the spec says: an acknowledged job survives crashes of the API, workers, reaper and the Postgres process. It does not survive losing the disk.
- Backups are not part of this project. Losing the disk loses everything since the last backup, and no backup plan exists in the spec.
- fsync stays on. Turning it off for speed would make a crash corrupt the database, not just lose recent commits.
- The reply to the client is sent after the commit returns, never before. A crash between the commit and the reply is the "connection drops after the commit" case, and it is handled by the retry with the same key (ADR 005).
- Each state change (claim, heartbeat, report) is a commit, so a job costs a few commits. At 50 jobs per second that is on the order of 150 commits per second, which one disk handles easily.
- The chaos test that matters here: while a load client records every job the API acknowledged, kill -9 the Postgres process and restart it, then check that every acknowledged job is in the table.

## Revisit this decision if

- Losing a disk or machine becomes unacceptable (add C, which also needs failover)
- Commit latency, not the claim query, turns out to limit throughput (consider B only for job types where losing a job is acceptable, and say so in the spec)

## References

- Checked against the PostgreSQL docs on 2026-10-07 (Write-Ahead Log settings). With `synchronous_commit` set to off, the docs give the maximum delay before a commit is safe against a crash as three times `wal_writer_delay`, which is 200 ms by default (about 0.6 s). They also say that, unlike turning off `fsync`, this cannot make the database inconsistent: a crash can lose recent commits, and the state is as if those transactions had been cleanly aborted.
