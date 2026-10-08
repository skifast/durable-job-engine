-- invariants.sql: the correctness rules from spec section 4, as queries.
--
-- Run it after schema.sql. It creates a small helper table (test_acknowledged_jobs)
-- and two views:
--
--   invariant_violations             check at any time, even while the system is running
--   invariant_violations_after_drain same checks plus the ones that only make sense
--                                    once every job has finished and every approval is decided
--
-- Each one must return ZERO ROWS. Any row is a broken rule. The row says which
-- rule (invariant), which job (job_id), and what was wrong (detail).
-- After every load, soak and chaos run:   SELECT * FROM invariant_violations_after_drain;
--
-- Many of these rules are also enforced by CHECK constraints and triggers in
-- schema.sql, so the database refuses the bad row in the first place. The queries
-- stay anyway: they prove the constraints are still in place, and they are the
-- only check for the rules that span two tables.
--
-- WHERE EACH SPEC INVARIANT IS CHECKED
--
--   Spec invariant (section 4)                        Where
--   -------------------------------------------------------------------------------
--   No acknowledged job is lost                       view: acknowledged_job_missing (needs the test client's list)
--   Every acknowledged submit ends in one terminal    view (after drain): acknowledged_job_not_finished
--   state after the run drains
--   Only the listed transitions happen                trigger jobs_check_update; chaos tests
--   finished is set exactly when state is terminal    CHECK + view: finished_mismatch
--   attempt never exceeds max_attempts                CHECK + view: attempt_exceeds_max, queued_without_attempts_left
--   attempt never decreases                           trigger jobs_check_update (tested in test_schema.sql)
--   A succeeded job has a result and no lease         CHECK + view: result_mismatch, lease_mismatch
--   A dead job has at least one error entry           view: attempt_ended_without_error
--   Dead only after a permanent failure or the        view: dead_without_cause
--   attempts are used up
--   A succeeded job is never claimed again            trigger (nothing leaves succeeded)
--   A waiting_approval job is never claimed           CHECK + view: approval_gate_skipped, waiting_job_was_run
--   No two workers hold a valid lease on one job      a job has one lease_owner column; the test workers also
--                                                     log every (job_id, attempt) they receive, and the same
--                                                     pair must never appear twice (done in the test client)
--   A running job has lease_owner and lease_expires   CHECK + view: lease_mismatch
--   A stale heartbeat or report changes nothing       the fenced UPDATE touches 0 rows (tested in test_schema.sql)
--   Reaper twice, or two reapers, requeues once       primary key (job_id, attempt) on job_errors; view:
--                                                     error_attempt_out_of_range, error_on_unfinished_attempt
--   At most one job per client and key                UNIQUE constraint + view: duplicate_idempotency_key
--   Same key, different payload is rejected           API test (the 422 path); payload cannot change (trigger)
--   An approval decision applies at most once         trigger jobs_check_update + view: decision_mismatch
--   Escalation never changes state, happens once      trigger jobs_check_update + view: escalation_mismatch

-- The test client (load, soak and chaos runs) writes one row here for every job the
-- API acknowledged with 201 or 200. This is how "no acknowledged job is lost" is
-- checked: the table lives in the same database as the jobs, so it is kept even
-- when Postgres is killed with kill -9. (A client that keeps its list in memory
-- or a file works too: load it into this table before checking.)
-- The table is empty outside of tests, so those checks then return nothing.
CREATE TABLE IF NOT EXISTS test_acknowledged_jobs (
    job_id          uuid        PRIMARY KEY,
    acknowledged_at timestamptz NOT NULL DEFAULT now()
);

CREATE OR REPLACE VIEW invariant_violations AS

-- ---- State and counters ----

SELECT 'attempt_exceeds_max'::text AS invariant, id AS job_id,
       format('attempt %s, max_attempts %s', attempt, max_attempts) AS detail
FROM jobs
WHERE attempt > max_attempts

UNION ALL
-- A queued job must have an attempt left, or it could be claimed past its limit.
SELECT 'queued_without_attempts_left', id,
       format('attempt %s, max_attempts %s', attempt, max_attempts)
FROM jobs
WHERE state = 'queued' AND attempt >= max_attempts

UNION ALL
-- finished_at is set exactly when the state is succeeded, dead or rejected.
SELECT 'finished_mismatch', id,
       format('state %s, finished_at %s', state, finished_at)
FROM jobs
WHERE (finished_at IS NOT NULL) <> (state IN ('succeeded', 'dead', 'rejected'))

UNION ALL
-- A succeeded job has a result; no other job has one.
SELECT 'result_mismatch', id,
       format('state %s, result is %s', state, CASE WHEN result IS NULL THEN 'null' ELSE 'set' END)
FROM jobs
WHERE (state = 'succeeded') <> (result IS NOT NULL)

UNION ALL
-- The result stays under 64 KB.
SELECT 'result_too_large', id,
       format('%s bytes', octet_length(result::text))
FROM jobs
WHERE octet_length(result::text) > 65536

UNION ALL
-- The attempt count fits the state: a job that ran has been claimed, a job that
-- never passed the gate has not.
SELECT 'attempt_state_mismatch', id,
       format('state %s, attempt %s', state, attempt)
FROM jobs
WHERE NOT (
    (state IN ('running', 'succeeded', 'dead') AND attempt >= 1)
    OR (state IN ('waiting_approval', 'rejected') AND attempt = 0)
    OR state = 'queued'
)

-- ---- Leases ----

UNION ALL
-- A running job has lease_owner and lease_expires_at. A job in any other state has neither.
SELECT 'lease_mismatch', id,
       format('state %s, lease_owner %s, lease_expires_at %s', state, lease_owner, lease_expires_at)
FROM jobs
WHERE NOT (
    (state = 'running' AND lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)
    OR (state <> 'running' AND lease_owner IS NULL AND lease_expires_at IS NULL)
)

-- ---- Error history ----

UNION ALL
-- Every attempt that ended without succeeding has an error entry. A job is queued
-- (after a failure or a lease expiry) or dead only once its latest attempt has failed,
-- so the entry for the current attempt must exist. For a dead job this also means
-- "a dead job has at least one error entry".
SELECT 'attempt_ended_without_error', j.id,
       format('state %s, attempt %s has no error entry', j.state, j.attempt)
FROM jobs j
WHERE j.state IN ('queued', 'dead')
  AND j.attempt >= 1
  AND NOT EXISTS (SELECT 1 FROM job_errors e WHERE e.job_id = j.id AND e.attempt = j.attempt)

UNION ALL
-- An error entry for an attempt the job never reached.
SELECT 'error_attempt_out_of_range', e.job_id,
       format('error for attempt %s, but the job is at attempt %s', e.attempt, j.attempt)
FROM job_errors e
JOIN jobs j ON j.id = e.job_id
WHERE e.attempt > j.attempt

UNION ALL
-- An attempt that is still running, or that succeeded, has no error entry.
-- (An attempt ends once: it either fails and is requeued or marked dead, or it succeeds.)
SELECT 'error_on_unfinished_attempt', e.job_id,
       format('error entry for attempt %s, but the job is %s at that attempt', e.attempt, j.state)
FROM job_errors e
JOIN jobs j ON j.id = e.job_id
WHERE e.attempt = j.attempt AND j.state IN ('running', 'succeeded')

UNION ALL
-- A job becomes dead only after a permanent failure, or when its attempts are used up.
-- So a dead job with attempts left must have a permanent failure as its last error.
SELECT 'dead_without_cause', j.id,
       format('dead at attempt %s of %s, last error kind is %s',
              j.attempt, j.max_attempts, coalesce(e.kind, 'missing'))
FROM jobs j
LEFT JOIN job_errors e ON e.job_id = j.id AND e.attempt = j.attempt
WHERE j.state = 'dead'
  AND j.attempt < j.max_attempts
  AND e.kind IS DISTINCT FROM 'permanent'

-- ---- Approval ----

UNION ALL
-- The approval gate: a job with an approval deadline is waiting or has a decision.
-- It can never be queued, running or finished without one.
SELECT 'approval_gate_skipped', id,
       format('state %s, approval_deadline set, no decision', state)
FROM jobs
WHERE approval_deadline IS NOT NULL AND state <> 'waiting_approval' AND decided_at IS NULL

UNION ALL
-- A job that is waiting for approval has never run and has no decision.
SELECT 'waiting_job_was_run', id,
       format('attempt %s, decided_at %s', attempt, decided_at)
FROM jobs
WHERE state = 'waiting_approval' AND (attempt <> 0 OR decided_at IS NOT NULL OR approval_deadline IS NULL)

UNION ALL
-- A decision has who and when together. A rejected job has a decision and a reason.
SELECT 'decision_mismatch', id,
       format('state %s, decided_by %s, decided_at %s, reason %s', state, decided_by, decided_at, decision_reason)
FROM jobs
WHERE (decided_by IS NULL) <> (decided_at IS NULL)
   OR (state = 'rejected' AND (decided_at IS NULL OR coalesce(length(decision_reason), 0) = 0))

UNION ALL
-- Escalation only applies to a job with an approval deadline, and only after the
-- deadline has passed. (That it happens once, and never changes the state, is
-- enforced by the trigger: escalated_at cannot be set twice or changed.)
SELECT 'escalation_mismatch', id,
       format('escalated_at %s, approval_deadline %s', escalated_at, approval_deadline)
FROM jobs
WHERE escalated_at IS NOT NULL
  AND (approval_deadline IS NULL OR escalated_at < approval_deadline)

-- ---- Idempotency ----

UNION ALL
-- At most one job per client and key.
SELECT 'duplicate_idempotency_key', (array_agg(id))[1],
       format('client %s, key %s used by %s jobs', client_id, idempotency_key, count(*))
FROM jobs
GROUP BY client_id, idempotency_key
HAVING count(*) > 1

-- ---- Acknowledged jobs are never lost (needs the test client's list) ----

UNION ALL
-- Every job the API acknowledged (returned 201 or 200 for) is still in the table.
SELECT 'acknowledged_job_missing', a.job_id,
       format('acknowledged at %s, not found in jobs', a.acknowledged_at)
FROM test_acknowledged_jobs a
LEFT JOIN jobs j ON j.id = a.job_id
WHERE j.id IS NULL;

-- The same checks, plus the ones that only hold once a run has drained
-- (no job is still running, and every approval has been decided).
CREATE OR REPLACE VIEW invariant_violations_after_drain AS
SELECT * FROM invariant_violations

UNION ALL
-- Every acknowledged submit ends in exactly one terminal state.
-- (A job has one state column, so "exactly one" holds by construction; this checks "terminal".)
SELECT 'acknowledged_job_not_finished', a.job_id,
       format('state %s after the run drained', j.state)
FROM test_acknowledged_jobs a
JOIN jobs j ON j.id = a.job_id
WHERE j.state NOT IN ('succeeded', 'dead', 'rejected');
