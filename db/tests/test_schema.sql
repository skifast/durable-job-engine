-- test_schema.sql: checks that schema.sql and invariants.sql behave as designed.
--
-- Run it on a scratch database that already has schema.sql and invariants.sql loaded:
--   psql -v ON_ERROR_STOP=1 -f db/tests/test_schema.sql <database>
--
-- It runs inside one transaction and ends with ROLLBACK, so it leaves nothing behind
-- (it does empty the jobs table first, so never run it on a database with real jobs).
-- If a check fails, psql stops and prints the failing line. If you see
-- "ALL TESTS PASSED" at the end, everything held.
--
-- Parts:
--   1  submit and duplicate keys
--   2  claim, heartbeat and success report, with the fencing check
--   3  retry, then dead when attempts run out
--   4  lease expiry and the reaper
--   5  approval: the gate, escalation, approve, reject
--   6  redrive
--   7  changes the database must refuse
--   8  each CHECK constraint on its own
--   9  invariants.sql: clean data gives zero rows, and each invariant catches a planted violation

BEGIN;

-- ---------------------------------------------------------------------------
-- Helpers (temporary: they disappear when the session ends)
-- ---------------------------------------------------------------------------

-- Runs a statement and passes only if it fails with the expected error code.
-- 23000 = refused by a trigger, 23514 = refused by a CHECK constraint, 23505 = unique violation.
CREATE FUNCTION pg_temp.must_fail(label text, expected text, stmt text) RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
    BEGIN
        EXECUTE stmt;
    EXCEPTION WHEN OTHERS THEN
        IF SQLSTATE = expected THEN
            RETURN;
        END IF;
        RAISE EXCEPTION 'TEST FAILED (%): expected error %, got % (%)', label, expected, SQLSTATE, SQLERRM;
    END;
    RAISE EXCEPTION 'TEST FAILED (%): the statement was accepted but should have been refused', label;
END;
$$;

CREATE FUNCTION pg_temp.reset() RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
    TRUNCATE job_errors, jobs;
    TRUNCATE test_acknowledged_jobs;
END;
$$;

-- Submit (T1 or T2). Returns the new job's id.
CREATE FUNCTION pg_temp.new_job(p_key text, p_approval boolean DEFAULT false, p_max integer DEFAULT 8,
                                p_deadline interval DEFAULT interval '24 hours') RETURNS uuid
LANGUAGE sql AS $$
    INSERT INTO jobs (type, client_id, idempotency_key, payload_hash, payload, state, approval_deadline, max_attempts)
    VALUES ('test', 'client-1', p_key,
            encode(sha256(convert_to(p_key, 'UTF8')), 'hex'),
            jsonb_build_object('key', p_key),
            CASE WHEN p_approval THEN 'waiting_approval' ELSE 'queued' END,
            CASE WHEN p_approval THEN now() + p_deadline END,
            p_max)
    RETURNING id
$$;

-- Claim (T6): the oldest queued job that is due. Returns nothing if there is none.
CREATE FUNCTION pg_temp.claim(p_worker text) RETURNS TABLE (job_id uuid, att integer)
LANGUAGE sql AS $$
    WITH next AS (
        SELECT id FROM jobs
        WHERE state = 'queued' AND run_at <= now()
        ORDER BY run_at, id
        LIMIT 1
        FOR UPDATE SKIP LOCKED
    )
    UPDATE jobs j
    SET state = 'running', lease_owner = p_worker,
        lease_expires_at = now() + interval '30 seconds', attempt = j.attempt + 1
    FROM next
    WHERE j.id = next.id
    RETURNING j.id, j.attempt
$$;

-- Claim one specific job (the same change as claim(), for setting up a test).
CREATE FUNCTION pg_temp.claim_job(p_job uuid, p_worker text) RETURNS void
LANGUAGE sql AS $$
    UPDATE jobs
    SET state = 'running', lease_owner = p_worker,
        lease_expires_at = now() + interval '30 seconds', attempt = attempt + 1
    WHERE id = p_job AND state = 'queued'
$$;

-- Failure report (T9, T10, T11), fenced on worker and attempt.
-- Applies the retry-or-dead rule from the spec. Returns the number of jobs changed (0 or 1).
CREATE FUNCTION pg_temp.report_failure(p_job uuid, p_worker text, p_attempt integer,
                                       p_kind text, p_message text) RETURNS integer
LANGUAGE plpgsql AS $$
DECLARE
    n integer;
BEGIN
    WITH failed AS (
        UPDATE jobs
        SET state = CASE WHEN p_kind = 'permanent' OR attempt >= max_attempts THEN 'dead' ELSE 'queued' END,
            finished_at = CASE WHEN p_kind = 'permanent' OR attempt >= max_attempts THEN now() END,
            run_at = now() + interval '2 seconds',
            lease_owner = NULL, lease_expires_at = NULL
        WHERE id = p_job AND lease_owner = p_worker AND attempt = p_attempt AND state = 'running'
        RETURNING id, attempt
    )
    INSERT INTO job_errors (job_id, attempt, kind, message)
    SELECT id, attempt, p_kind, p_message FROM failed;
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END;
$$;

-- Reaper pass (T12, T13): handles every running job whose lease has expired.
CREATE FUNCTION pg_temp.reap() RETURNS integer
LANGUAGE plpgsql AS $$
DECLARE
    n integer;
BEGIN
    WITH expired AS (
        UPDATE jobs
        SET state = CASE WHEN attempt >= max_attempts THEN 'dead' ELSE 'queued' END,
            finished_at = CASE WHEN attempt >= max_attempts THEN now() END,
            run_at = now() + interval '2 seconds',
            lease_owner = NULL, lease_expires_at = NULL
        WHERE state = 'running' AND lease_expires_at < now()
        RETURNING id, attempt
    )
    INSERT INTO job_errors (job_id, attempt, kind, message)
    SELECT id, attempt, 'lease_expired', 'lease expired' FROM expired;
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END;
$$;

-- Moves a job's run_at into the past, standing in for "time passed". now() does not
-- move inside a transaction, so the trigger is switched off for this one update.
CREATE FUNCTION pg_temp.fast_forward(p_job uuid) RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
    ALTER TABLE jobs DISABLE TRIGGER jobs_check_update;
    UPDATE jobs SET run_at = now() - interval '1 second' WHERE id = p_job;
    ALTER TABLE jobs ENABLE TRIGGER jobs_check_update;
END;
$$;

CREATE FUNCTION pg_temp.violation_count() RETURNS bigint
LANGUAGE sql AS $$ SELECT count(*) FROM invariant_violations_after_drain $$;

-- ---------------------------------------------------------------------------
-- 1. Submit and duplicate keys
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    j uuid;
    n integer;
BEGIN
    PERFORM pg_temp.reset();
    j := pg_temp.new_job('key-1');
    ASSERT (SELECT state FROM jobs WHERE id = j) = 'queued', 'a new job is queued';
    ASSERT (SELECT attempt FROM jobs WHERE id = j) = 0, 'a new job has attempt 0';

    -- The same client and key again: the unique constraint refuses it.
    PERFORM pg_temp.must_fail('duplicate client and key', '23505',
        format($q$INSERT INTO jobs (type, client_id, idempotency_key, payload_hash, payload, state)
                  VALUES ('test', 'client-1', 'key-1', repeat('a', 64), '{}', 'queued')$q$));

    -- The API's pattern: INSERT ... ON CONFLICT DO NOTHING inserts nothing, then reads the existing row.
    INSERT INTO jobs (type, client_id, idempotency_key, payload_hash, payload, state)
    VALUES ('test', 'client-1', 'key-1', repeat('a', 64), '{}', 'queued')
    ON CONFLICT (client_id, idempotency_key) DO NOTHING;
    GET DIAGNOSTICS n = ROW_COUNT;
    ASSERT n = 0, 'ON CONFLICT DO NOTHING inserts nothing for a duplicate';

    -- A different client can use the same key text.
    INSERT INTO jobs (type, client_id, idempotency_key, payload_hash, payload, state)
    VALUES ('test', 'client-2', 'key-1', repeat('a', 64), '{}', 'queued');

    -- Keys and client ids that are empty, or keys over 255 characters, are refused.
    PERFORM pg_temp.must_fail('empty key', '23514',
        $q$INSERT INTO jobs (type, client_id, idempotency_key, payload_hash, payload, state)
           VALUES ('test', 'client-1', '', repeat('a', 64), '{}', 'queued')$q$);
    PERFORM pg_temp.must_fail('256-character key', '23514',
        $q$INSERT INTO jobs (type, client_id, idempotency_key, payload_hash, payload, state)
           VALUES ('test', 'client-1', repeat('k', 256), repeat('a', 64), '{}', 'queued')$q$);
    INSERT INTO jobs (type, client_id, idempotency_key, payload_hash, payload, state)
    VALUES ('test', 'client-1', repeat('k', 255), repeat('a', 64), '{}', 'queued');   -- 255 is allowed
    PERFORM pg_temp.must_fail('empty client_id', '23514',
        $q$INSERT INTO jobs (type, client_id, idempotency_key, payload_hash, payload, state)
           VALUES ('test', '', 'key-x', repeat('a', 64), '{}', 'queued')$q$);

    ASSERT pg_temp.violation_count() = 0, 'part 1 leaves no invariant violations';
    RAISE NOTICE 'part 1 passed: submit and duplicate keys';
END;
$$;

-- ---------------------------------------------------------------------------
-- 2. Claim, heartbeat and success report, with the fencing check
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    j uuid;
    got record;
    n integer;
BEGIN
    PERFORM pg_temp.reset();
    j := pg_temp.new_job('claim-1');

    SELECT * INTO got FROM pg_temp.claim('worker-a');
    ASSERT got.job_id = j AND got.att = 1, 'the first claim returns the job at attempt 1';
    ASSERT (SELECT lease_owner FROM jobs WHERE id = j) = 'worker-a', 'the claim writes the lease owner';
    ASSERT NOT EXISTS (SELECT 1 FROM pg_temp.claim('worker-b')), 'a running job is not claimed again';

    -- Heartbeat from the wrong worker, or with the wrong attempt, changes nothing.
    UPDATE jobs SET lease_expires_at = now() + interval '30 seconds'
    WHERE id = j AND lease_owner = 'worker-b' AND attempt = 1 AND state = 'running';
    GET DIAGNOSTICS n = ROW_COUNT;
    ASSERT n = 0, 'a heartbeat from the wrong worker changes nothing';
    UPDATE jobs SET lease_expires_at = now() + interval '30 seconds'
    WHERE id = j AND lease_owner = 'worker-a' AND attempt = 2 AND state = 'running';
    GET DIAGNOSTICS n = ROW_COUNT;
    ASSERT n = 0, 'a heartbeat with the wrong attempt changes nothing';
    UPDATE jobs SET lease_expires_at = now() + interval '60 seconds'
    WHERE id = j AND lease_owner = 'worker-a' AND attempt = 1 AND state = 'running';
    GET DIAGNOSTICS n = ROW_COUNT;
    ASSERT n = 1, 'a heartbeat from the lease holder is accepted';

    -- Success report: a stale attempt is refused, the right one is accepted.
    UPDATE jobs SET state = 'succeeded', result = '{"ok": true}', finished_at = now(),
                    lease_owner = NULL, lease_expires_at = NULL
    WHERE id = j AND lease_owner = 'worker-a' AND attempt = 2 AND state = 'running';
    GET DIAGNOSTICS n = ROW_COUNT;
    ASSERT n = 0, 'a report with the wrong attempt changes nothing';
    UPDATE jobs SET state = 'succeeded', result = '{"ok": true}', finished_at = now(),
                    lease_owner = NULL, lease_expires_at = NULL
    WHERE id = j AND lease_owner = 'worker-a' AND attempt = 1 AND state = 'running';
    GET DIAGNOSTICS n = ROW_COUNT;
    ASSERT n = 1, 'the lease holder reports success';
    ASSERT (SELECT state FROM jobs WHERE id = j) = 'succeeded', 'the job is succeeded';

    -- The same report again changes nothing (the job is no longer running).
    UPDATE jobs SET state = 'succeeded', result = '{"ok": false}', finished_at = now()
    WHERE id = j AND lease_owner = 'worker-a' AND attempt = 1 AND state = 'running';
    GET DIAGNOSTICS n = ROW_COUNT;
    ASSERT n = 0, 'a second report changes nothing';
    ASSERT NOT EXISTS (SELECT 1 FROM pg_temp.claim('worker-b')), 'a succeeded job is never claimed again';

    -- A job whose run_at is in the future is not claimed.
    PERFORM pg_temp.new_job('claim-future');
    ALTER TABLE jobs DISABLE TRIGGER jobs_check_update;
    UPDATE jobs SET run_at = now() + interval '1 hour' WHERE idempotency_key = 'claim-future';
    ALTER TABLE jobs ENABLE TRIGGER jobs_check_update;
    ASSERT NOT EXISTS (SELECT 1 FROM pg_temp.claim('worker-a')), 'a job that is not due yet is not claimed';

    ASSERT pg_temp.violation_count() = 0, 'part 2 leaves no invariant violations';
    RAISE NOTICE 'part 2 passed: claim, heartbeat, success, fencing';
END;
$$;

-- ---------------------------------------------------------------------------
-- 3. Retry, then dead when attempts run out
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    j uuid;
    got record;
BEGIN
    PERFORM pg_temp.reset();
    j := pg_temp.new_job('retry-1', false, 2);       -- max_attempts = 2

    SELECT * INTO got FROM pg_temp.claim('worker-a');
    ASSERT got.att = 1, 'attempt 1';

    -- A stale report (wrong attempt) changes nothing.
    ASSERT pg_temp.report_failure(j, 'worker-a', 2, 'retryable', 'stale') = 0, 'a stale failure report changes nothing';
    ASSERT (SELECT count(*) FROM job_errors) = 0, 'a stale report adds no error entry';

    -- Retryable failure on attempt 1: back to queued, run_at in the future.
    ASSERT pg_temp.report_failure(j, 'worker-a', 1, 'retryable', 'downstream timeout') = 1, 'failure accepted';
    ASSERT (SELECT state FROM jobs WHERE id = j) = 'queued', 'a retryable failure requeues the job';
    ASSERT (SELECT run_at FROM jobs WHERE id = j) > now(), 'the retry is scheduled in the future';
    ASSERT (SELECT attempt FROM jobs WHERE id = j) = 1, 'attempt is not reset';
    ASSERT NOT EXISTS (SELECT 1 FROM pg_temp.claim('worker-b')), 'the retry is not claimed before run_at';
    ASSERT pg_temp.violation_count() = 0, 'a requeued job has an error entry for its attempt';

    -- Time passes. Attempt 2 is the last one, so a retryable failure now means dead.
    PERFORM pg_temp.fast_forward(j);
    SELECT * INTO got FROM pg_temp.claim('worker-b');
    ASSERT got.job_id = j AND got.att = 2, 'the retry is claimed at attempt 2';
    ASSERT pg_temp.report_failure(j, 'worker-b', 2, 'retryable', 'still failing') = 1, 'failure accepted';
    ASSERT (SELECT state FROM jobs WHERE id = j) = 'dead', 'attempts used up: the job is dead';
    ASSERT (SELECT finished_at FROM jobs WHERE id = j) IS NOT NULL, 'dead sets finished_at';
    ASSERT (SELECT count(*) FROM job_errors WHERE job_id = j) = 2, 'both failures are in the error history';
    ASSERT pg_temp.violation_count() = 0, 'a dead job at max_attempts is valid';

    -- A permanent failure goes straight to dead, even with attempts left.
    j := pg_temp.new_job('permanent-1', false, 8);
    PERFORM pg_temp.claim('worker-a');
    ASSERT pg_temp.report_failure(j, 'worker-a', 1, 'permanent', 'invalid payload') = 1, 'permanent failure accepted';
    ASSERT (SELECT state FROM jobs WHERE id = j) = 'dead', 'a permanent failure goes straight to dead';
    ASSERT pg_temp.violation_count() = 0, 'dead after a permanent failure is valid';

    RAISE NOTICE 'part 3 passed: retry and dead';
END;
$$;

-- ---------------------------------------------------------------------------
-- 4. Lease expiry and the reaper
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    j uuid;
    late uuid;
    n integer;
BEGIN
    PERFORM pg_temp.reset();

    -- An expired lease is requeued once.
    j := pg_temp.new_job('lease-1');
    PERFORM pg_temp.claim('worker-a');
    ASSERT pg_temp.reap() = 0, 'the reaper leaves a live lease alone';
    UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE id = j;      -- the lease runs out
    ASSERT pg_temp.reap() = 1, 'the reaper takes back an expired lease';
    ASSERT (SELECT state FROM jobs WHERE id = j) = 'queued', 'the job is requeued';
    ASSERT (SELECT kind FROM job_errors WHERE job_id = j AND attempt = 1) = 'lease_expired', 'the expiry is in the error history';
    ASSERT pg_temp.reap() = 0, 'running the reaper twice requeues the job only once';

    -- A second reaper that read the row before the first one finished would try to write
    -- the same (job, attempt) error entry. The primary key refuses it.
    PERFORM pg_temp.must_fail('second error entry for the same attempt', '23505',
        format($q$INSERT INTO job_errors (job_id, attempt, kind, message) VALUES (%L, 1, 'lease_expired', 'again')$q$, j));

    -- An attempt ends only once, whatever the kind: a failure entry for the same attempt is refused too.
    PERFORM pg_temp.must_fail('error entry of another kind for the same attempt', '23505',
        format($q$INSERT INTO job_errors (job_id, attempt, kind, message) VALUES (%L, 1, 'retryable', 'also failed')$q$, j));

    -- The slow original worker reports after the reaper took the job back: refused.
    ASSERT pg_temp.report_failure(j, 'worker-a', 1, 'retryable', 'too late') = 0, 'a late report after the requeue changes nothing';
    UPDATE jobs SET state = 'succeeded', result = '{}', finished_at = now()
    WHERE id = j AND lease_owner = 'worker-a' AND attempt = 1 AND state = 'running';
    GET DIAGNOSTICS n = ROW_COUNT;
    ASSERT n = 0, 'a late success report after the requeue changes nothing';

    -- A slow worker that reports after its lease expired but BEFORE the reaper runs is accepted (ADR 009).
    late := pg_temp.new_job('lease-late');
    PERFORM pg_temp.claim_job(late, 'worker-d');
    UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE id = late;   -- expired, reaper has not run
    UPDATE jobs SET state = 'succeeded', result = '{"slow": true}', finished_at = now(),
                    lease_owner = NULL, lease_expires_at = NULL
    WHERE id = late AND lease_owner = 'worker-d' AND attempt = 1 AND state = 'running';
    GET DIAGNOSTICS n = ROW_COUNT;
    ASSERT n = 1, 'a report after expiry but before the reaper runs is accepted';

    ASSERT pg_temp.violation_count() = 0, 'part 4 leaves no invariant violations';
    RAISE NOTICE 'part 4 passed: lease expiry and reaper';
END;
$$;

-- A cleaner version of the last-attempt case, on its own.
DO $$
DECLARE
    j uuid;
BEGIN
    PERFORM pg_temp.reset();
    j := pg_temp.new_job('lease-last-only', false, 1);                                -- max_attempts = 1
    PERFORM pg_temp.claim('worker-a');
    UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE id = j;
    ASSERT pg_temp.reap() = 1, 'the reaper handles the expired lease';
    ASSERT (SELECT state FROM jobs WHERE id = j) = 'dead', 'a lease that expires on the last attempt goes to dead';
    ASSERT (SELECT finished_at FROM jobs WHERE id = j) IS NOT NULL, 'dead sets finished_at';
    ASSERT pg_temp.violation_count() = 0, 'dead after a lease expiry on the last attempt is valid';
    RAISE NOTICE 'part 4b passed: lease expiry on the last attempt';
END;
$$;

-- ---------------------------------------------------------------------------
-- 5. Approval: the gate, escalation, approve, reject
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    a uuid;
    b uuid;
    got record;
    n integer;
BEGIN
    PERFORM pg_temp.reset();
    a := pg_temp.new_job('approval-a', true, 8, interval '-1 hour');                  -- deadline already passed
    ASSERT (SELECT state FROM jobs WHERE id = a) = 'waiting_approval', 'a job that needs approval waits';
    ASSERT NOT EXISTS (SELECT 1 FROM pg_temp.claim('worker-a')), 'a waiting_approval job is never claimed';

    -- The gate: waiting_approval cannot jump to running.
    PERFORM pg_temp.must_fail('waiting_approval -> running', '23000',
        format($q$UPDATE jobs SET state = 'running', attempt = 1, lease_owner = 'w', lease_expires_at = now() WHERE id = %L$q$, a));

    -- Escalation (T5): sets escalated_at once, the job stays waiting.
    UPDATE jobs SET escalated_at = now()
    WHERE id = a AND state = 'waiting_approval' AND escalated_at IS NULL AND approval_deadline <= now();
    GET DIAGNOSTICS n = ROW_COUNT;
    ASSERT n = 1, 'the reaper escalates an overdue approval';
    ASSERT (SELECT state FROM jobs WHERE id = a) = 'waiting_approval', 'escalation does not change the state';
    UPDATE jobs SET escalated_at = now()
    WHERE id = a AND state = 'waiting_approval' AND escalated_at IS NULL AND approval_deadline <= now();
    GET DIAGNOSTICS n = ROW_COUNT;
    ASSERT n = 0, 'a second escalation changes nothing (guard)';
    PERFORM pg_temp.must_fail('second escalation forced', '23000',
        format($q$UPDATE jobs SET escalated_at = now() + interval '1 minute' WHERE id = %L$q$, a));

    -- escalated_at cannot be erased or moved by another change, such as an approve.
    PERFORM pg_temp.must_fail('approve that erases escalated_at', '23000',
        format($q$UPDATE jobs SET state = 'queued', decided_by = 'alice', decided_at = now(), run_at = now(),
                                  escalated_at = NULL WHERE id = %L$q$, a));

    -- An escalated job can still be approved (T3).
    UPDATE jobs SET state = 'queued', decided_by = 'alice', decided_at = now(), run_at = now()
    WHERE id = a AND state = 'waiting_approval';
    GET DIAGNOSTICS n = ROW_COUNT;
    ASSERT n = 1, 'an escalated job can be approved';
    UPDATE jobs SET state = 'queued', decided_by = 'bob', decided_at = now()
    WHERE id = a AND state = 'waiting_approval';
    GET DIAGNOSTICS n = ROW_COUNT;
    ASSERT n = 0, 'a second decision is refused (guard)';
    PERFORM pg_temp.must_fail('second decision forced', '23000',
        format($q$UPDATE jobs SET decided_by = 'bob' WHERE id = %L$q$, a));
    SELECT * INTO got FROM pg_temp.claim('worker-a');
    ASSERT got.job_id = a, 'an approved job is claimed like any other';
    -- The decision cannot be rewritten later, for example by an update that looks like a heartbeat.
    PERFORM pg_temp.must_fail('decision rewritten on a running job', '23000',
        format($q$UPDATE jobs SET decided_by = 'mallory' WHERE id = %L$q$, a));

    -- Reject (T4) needs a reason.
    b := pg_temp.new_job('approval-b', true);
    PERFORM pg_temp.must_fail('reject without a reason', '23514',
        format($q$UPDATE jobs SET state = 'rejected', decided_by = 'alice', decided_at = now(), finished_at = now() WHERE id = %L$q$, b));
    UPDATE jobs SET state = 'rejected', decided_by = 'alice', decided_at = now(),
                    decision_reason = 'not needed', finished_at = now()
    WHERE id = b AND state = 'waiting_approval';
    GET DIAGNOSTICS n = ROW_COUNT;
    ASSERT n = 1, 'reject with a reason works';
    PERFORM pg_temp.must_fail('rejected -> queued', '23000',
        format($q$UPDATE jobs SET state = 'queued', run_at = now() WHERE id = %L$q$, b));

    ASSERT pg_temp.violation_count() = 0, 'part 5 leaves no invariant violations';
    RAISE NOTICE 'part 5 passed: approval';
END;
$$;

-- ---------------------------------------------------------------------------
-- 6. Redrive
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    j uuid;
    r uuid;
    got record;
    n integer;
BEGIN
    PERFORM pg_temp.reset();
    j := pg_temp.new_job('redrive-1', false, 1);
    PERFORM pg_temp.claim('worker-a');
    PERFORM pg_temp.report_failure(j, 'worker-a', 1, 'retryable', 'boom');
    ASSERT (SELECT state FROM jobs WHERE id = j) = 'dead', 'the job is dead (1 attempt, used up)';

    -- max_attempts must go above attempt.
    PERFORM pg_temp.must_fail('redrive with max_attempts = attempt', '23000',
        format($q$UPDATE jobs SET state = 'queued', max_attempts = 1, finished_at = NULL, run_at = now(),
                                  redriven_by = 'alice', redriven_at = now() WHERE id = %L$q$, j));
    -- The operator must be recorded.
    PERFORM pg_temp.must_fail('redrive without a name', '23000',
        format($q$UPDATE jobs SET state = 'queued', max_attempts = 3, finished_at = NULL, run_at = now() WHERE id = %L$q$, j));
    -- Attempt is never reset.
    PERFORM pg_temp.must_fail('redrive that resets attempt', '23000',
        format($q$UPDATE jobs SET state = 'queued', max_attempts = 3, attempt = 0, finished_at = NULL, run_at = now(),
                                  redriven_by = 'alice', redriven_at = now() WHERE id = %L$q$, j));

    UPDATE jobs SET state = 'queued', max_attempts = 3, finished_at = NULL, run_at = now(),
                    redriven_by = 'alice', redriven_at = now()
    WHERE id = j AND state = 'dead';
    GET DIAGNOSTICS n = ROW_COUNT;
    ASSERT n = 1, 'redrive from dead works';
    ASSERT (SELECT attempt FROM jobs WHERE id = j) = 1, 'redrive keeps attempt';
    SELECT * INTO got FROM pg_temp.claim('worker-b');
    ASSERT got.att = 2, 'the next claim gets a new fencing token (attempt 2)';

    -- Redrive only applies to a dead job (the API's guard).
    UPDATE jobs SET state = 'queued', max_attempts = 5, redriven_by = 'alice', redriven_at = now()
    WHERE id = j AND state = 'dead';
    GET DIAGNOSTICS n = ROW_COUNT;
    ASSERT n = 0, 'redrive of a job that is not dead changes nothing (guard)';
    -- Even without the guard, max_attempts cannot change outside a redrive.
    PERFORM pg_temp.must_fail('running -> queued with a new max_attempts', '23000',
        format($q$UPDATE jobs SET state = 'queued', max_attempts = 9, lease_owner = NULL, lease_expires_at = NULL,
                                  run_at = now() WHERE id = %L$q$, j));

    ASSERT pg_temp.violation_count() = 0, 'part 6 leaves no invariant violations';
    RAISE NOTICE 'part 6 passed: redrive';
END;
$$;

-- ---------------------------------------------------------------------------
-- 7. Changes the database must refuse (trigger rules, error code 23000)
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    q uuid;         -- queued
    r uuid;         -- running
    s uuid;         -- succeeded
BEGIN
    PERFORM pg_temp.reset();
    q := pg_temp.new_job('refuse-q');            -- stays queued
    r := pg_temp.new_job('refuse-r');            -- will be running
    s := pg_temp.new_job('refuse-s');            -- will be succeeded
    PERFORM pg_temp.claim_job(r, 'w1');
    PERFORM pg_temp.claim_job(s, 'w1');
    UPDATE jobs SET state = 'succeeded', result = '{}', finished_at = now(), lease_owner = NULL, lease_expires_at = NULL
    WHERE id = s;

    -- Transitions that are not in the spec.
    PERFORM pg_temp.must_fail('queued -> succeeded', '23000',
        format($q$UPDATE jobs SET state = 'succeeded', result = '{}', finished_at = now() WHERE id = %L$q$, q));
    PERFORM pg_temp.must_fail('succeeded -> queued', '23000',
        format($q$UPDATE jobs SET state = 'queued', run_at = now() WHERE id = %L$q$, s));
    PERFORM pg_temp.must_fail('succeeded -> running', '23000',
        format($q$UPDATE jobs SET state = 'running', attempt = 2, lease_owner = 'w', lease_expires_at = now(), finished_at = NULL, result = NULL WHERE id = %L$q$, s));
    PERFORM pg_temp.must_fail('running -> waiting_approval', '23000',
        format($q$UPDATE jobs SET state = 'waiting_approval', lease_owner = NULL, lease_expires_at = NULL WHERE id = %L$q$, r));
    PERFORM pg_temp.must_fail('queued -> queued', '23000',
        format($q$UPDATE jobs SET run_at = now() WHERE id = %L$q$, q));

    -- attempt rules.
    PERFORM pg_temp.must_fail('attempt goes down', '23000',
        format($q$UPDATE jobs SET attempt = 0 WHERE id = %L$q$, r));
    PERFORM pg_temp.must_fail('attempt goes up outside a claim', '23000',
        format($q$UPDATE jobs SET attempt = attempt + 1 WHERE id = %L$q$, r));
    PERFORM pg_temp.must_fail('claim that does not raise attempt', '23000',
        format($q$UPDATE jobs SET state = 'running', lease_owner = 'w', lease_expires_at = now() WHERE id = %L$q$, q));
    PERFORM pg_temp.must_fail('claim that raises attempt by 2', '23000',
        format($q$UPDATE jobs SET state = 'running', attempt = attempt + 2, lease_owner = 'w', lease_expires_at = now() WHERE id = %L$q$, q));

    -- Other fields that must not change.
    PERFORM pg_temp.must_fail('payload changes', '23000',
        format($q$UPDATE jobs SET payload = '{"other": 1}' WHERE id = %L$q$, r));
    PERFORM pg_temp.must_fail('idempotency key changes', '23000',
        format($q$UPDATE jobs SET idempotency_key = 'new-key' WHERE id = %L$q$, r));
    PERFORM pg_temp.must_fail('heartbeat changes the lease owner', '23000',
        format($q$UPDATE jobs SET lease_owner = 'someone-else' WHERE id = %L$q$, r));
    PERFORM pg_temp.must_fail('max_attempts changes outside a redrive', '23000',
        format($q$UPDATE jobs SET max_attempts = 20 WHERE id = %L$q$, r));

    -- New rows.
    PERFORM pg_temp.must_fail('insert as running', '23000',
        $q$INSERT INTO jobs (type, client_id, idempotency_key, payload_hash, payload, state, attempt, lease_owner, lease_expires_at)
           VALUES ('test', 'c', 'bad-1', repeat('a', 64), '{}', 'running', 1, 'w', now())$q$);
    PERFORM pg_temp.must_fail('insert with attempt 1', '23000',
        $q$INSERT INTO jobs (type, client_id, idempotency_key, payload_hash, payload, state, attempt)
           VALUES ('test', 'c', 'bad-2', repeat('a', 64), '{}', 'queued', 1)$q$);

    ASSERT pg_temp.violation_count() = 0, 'part 7 leaves no invariant violations';
    RAISE NOTICE 'part 7 passed: illegal changes are refused';
END;
$$;

-- ---------------------------------------------------------------------------
-- 8. Each CHECK constraint on its own (triggers off, so the CHECK is what answers)
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    q uuid;
    r uuid;
    w uuid;
BEGIN
    PERFORM pg_temp.reset();
    q := pg_temp.new_job('check-q');
    w := pg_temp.new_job('check-w', true);
    r := pg_temp.new_job('check-r');
    UPDATE jobs SET state = 'running', attempt = 1, lease_owner = 'w1', lease_expires_at = now() + interval '30 seconds'
    WHERE id = r;
    ALTER TABLE jobs DISABLE TRIGGER USER;

    PERFORM pg_temp.must_fail('queued job with a lease owner', '23514',
        format($q$UPDATE jobs SET lease_owner = 'w' WHERE id = %L$q$, q));
    PERFORM pg_temp.must_fail('running job without a lease', '23514',
        format($q$UPDATE jobs SET lease_owner = NULL, lease_expires_at = NULL WHERE id = %L$q$, r));
    PERFORM pg_temp.must_fail('queued job with finished_at', '23514',
        format($q$UPDATE jobs SET finished_at = now() WHERE id = %L$q$, q));
    PERFORM pg_temp.must_fail('succeeded job without a result', '23514',
        format($q$UPDATE jobs SET state = 'succeeded', finished_at = now(), lease_owner = NULL, lease_expires_at = NULL WHERE id = %L$q$, r));
    PERFORM pg_temp.must_fail('queued job with a result', '23514',
        format($q$UPDATE jobs SET result = '{}' WHERE id = %L$q$, q));
    PERFORM pg_temp.must_fail('result over 64 KB', '23514',
        format($q$UPDATE jobs SET state = 'succeeded', result = to_jsonb(repeat('x', 70000)), finished_at = now(),
                                  lease_owner = NULL, lease_expires_at = NULL WHERE id = %L$q$, r));
    PERFORM pg_temp.must_fail('queued job with no attempts left', '23514',
        format($q$UPDATE jobs SET attempt = 8 WHERE id = %L$q$, q));
    PERFORM pg_temp.must_fail('attempt above max_attempts', '23514',
        format($q$UPDATE jobs SET attempt = 9 WHERE id = %L$q$, r));
    PERFORM pg_temp.must_fail('max_attempts of 0', '23514',
        format($q$UPDATE jobs SET max_attempts = 0 WHERE id = %L$q$, q));
    PERFORM pg_temp.must_fail('waiting job already decided', '23514',
        format($q$UPDATE jobs SET decided_by = 'a', decided_at = now() WHERE id = %L$q$, w));
    PERFORM pg_temp.must_fail('waiting job queued without a decision (gate)', '23514',
        format($q$UPDATE jobs SET state = 'queued' WHERE id = %L$q$, w));
    PERFORM pg_temp.must_fail('waiting job with an attempt', '23514',
        format($q$UPDATE jobs SET attempt = 1 WHERE id = %L$q$, w));
    PERFORM pg_temp.must_fail('rejected without a reason', '23514',
        format($q$UPDATE jobs SET state = 'rejected', decided_by = 'a', decided_at = now(), finished_at = now() WHERE id = %L$q$, w));
    PERFORM pg_temp.must_fail('escalated without an approval deadline', '23514',
        format($q$UPDATE jobs SET escalated_at = now() WHERE id = %L$q$, q));
    PERFORM pg_temp.must_fail('decision with who but no when', '23514',
        format($q$UPDATE jobs SET decided_by = 'a' WHERE id = %L$q$, q));
    PERFORM pg_temp.must_fail('redrive with who but no when', '23514',
        format($q$UPDATE jobs SET redriven_by = 'a' WHERE id = %L$q$, q));
    PERFORM pg_temp.must_fail('payload_hash that is not a SHA-256 hex string', '23514',
        format($q$UPDATE jobs SET payload_hash = 'xyz' WHERE id = %L$q$, q));
    PERFORM pg_temp.must_fail('unknown state', '23514',
        format($q$UPDATE jobs SET state = 'paused' WHERE id = %L$q$, q));
    PERFORM pg_temp.must_fail('error message over 1,000 characters', '23514',
        format($q$INSERT INTO job_errors (job_id, attempt, kind, message) VALUES (%L, 1, 'retryable', repeat('m', 1001))$q$, r));
    PERFORM pg_temp.must_fail('unknown error kind', '23514',
        format($q$INSERT INTO job_errors (job_id, attempt, kind, message) VALUES (%L, 1, 'weird', 'm')$q$, r));
    PERFORM pg_temp.must_fail('error entry for attempt 0', '23514',
        format($q$INSERT INTO job_errors (job_id, attempt, kind, message) VALUES (%L, 0, 'retryable', 'm')$q$, r));
    PERFORM pg_temp.must_fail('error entry for a job that does not exist', '23503',
        $q$INSERT INTO job_errors (job_id, attempt, kind, message) VALUES (gen_random_uuid(), 1, 'retryable', 'm')$q$);

    ALTER TABLE jobs ENABLE TRIGGER USER;
    RAISE NOTICE 'part 8 passed: CHECK constraints';
END;
$$;

-- ---------------------------------------------------------------------------
-- 9. invariants.sql: zero rows on good data, and every invariant catches a planted violation
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    expected text[] := ARRAY[
        'attempt_exceeds_max', 'queued_without_attempts_left', 'finished_mismatch', 'result_mismatch',
        'result_too_large', 'attempt_state_mismatch', 'lease_mismatch', 'attempt_ended_without_error',
        'error_attempt_out_of_range', 'error_on_unfinished_attempt', 'dead_without_cause',
        'approval_gate_skipped', 'waiting_job_was_run', 'decision_mismatch', 'escalation_mismatch',
        'duplicate_idempotency_key', 'acknowledged_job_missing', 'acknowledged_job_not_finished'];
    in_view text[];
    j uuid;
    ghost uuid := gen_random_uuid();
    name text;
    constraint_name text;
BEGIN
    -- Every invariant named in the view must be in the list above, so a new invariant
    -- cannot be added without a planted-violation test for it.
    SELECT array_agg(DISTINCT m[1]) INTO in_view
    FROM regexp_matches(pg_get_viewdef('invariant_violations_after_drain'::regclass) ||
                        pg_get_viewdef('invariant_violations'::regclass),
                        '''([a-z_]+)''::text AS invariant', 'g') AS m;
    RAISE NOTICE 'invariants found in the views: %', in_view;

    -- Good data first: one job in every state, built through the real transitions.
    PERFORM pg_temp.reset();
    PERFORM pg_temp.new_job('good-queued');
    PERFORM pg_temp.new_job('good-waiting', true);
    ASSERT pg_temp.violation_count() = 0, 'good data gives zero rows';

    -- Now take the protection away: triggers off, every CHECK and UNIQUE constraint dropped.
    -- (All of this is rolled back at the end.)
    ALTER TABLE jobs DISABLE TRIGGER USER;
    FOR constraint_name IN
        SELECT conname FROM pg_constraint WHERE conrelid = 'jobs'::regclass AND contype IN ('c', 'u')
    LOOP
        EXECUTE format('ALTER TABLE jobs DROP CONSTRAINT %I', constraint_name);
    END LOOP;

    CREATE TEMP TABLE planted (invariant text, job_id uuid) ON COMMIT DROP;

    -- attempt_exceeds_max
    j := pg_temp.new_job('p1'); UPDATE jobs SET state = 'running', attempt = 9, lease_owner = 'w', lease_expires_at = now() WHERE id = j;
    INSERT INTO planted VALUES ('attempt_exceeds_max', j);
    -- queued_without_attempts_left
    j := pg_temp.new_job('p2'); UPDATE jobs SET attempt = max_attempts WHERE id = j;
    INSERT INTO planted VALUES ('queued_without_attempts_left', j);
    -- finished_mismatch
    j := pg_temp.new_job('p3'); UPDATE jobs SET finished_at = now() WHERE id = j;
    INSERT INTO planted VALUES ('finished_mismatch', j);
    -- result_mismatch
    j := pg_temp.new_job('p4'); UPDATE jobs SET result = '{}' WHERE id = j;
    INSERT INTO planted VALUES ('result_mismatch', j);
    -- result_too_large
    j := pg_temp.new_job('p5'); UPDATE jobs SET result = to_jsonb(repeat('x', 70000)) WHERE id = j;
    INSERT INTO planted VALUES ('result_too_large', j);
    -- attempt_state_mismatch (a waiting job that has an attempt)
    j := pg_temp.new_job('p6', true); UPDATE jobs SET attempt = 1 WHERE id = j;
    INSERT INTO planted VALUES ('attempt_state_mismatch', j);
    -- lease_mismatch
    j := pg_temp.new_job('p7'); UPDATE jobs SET lease_owner = 'w' WHERE id = j;
    INSERT INTO planted VALUES ('lease_mismatch', j);
    -- attempt_ended_without_error (queued after attempt 1, but no error entry)
    j := pg_temp.new_job('p8'); UPDATE jobs SET attempt = 1 WHERE id = j;
    INSERT INTO planted VALUES ('attempt_ended_without_error', j);
    -- error_attempt_out_of_range
    j := pg_temp.new_job('p9'); INSERT INTO job_errors (job_id, attempt, kind, message) VALUES (j, 5, 'retryable', 'x');
    INSERT INTO planted VALUES ('error_attempt_out_of_range', j);
    -- error_on_unfinished_attempt (running at attempt 1 with an error entry for attempt 1)
    j := pg_temp.new_job('p10'); UPDATE jobs SET state = 'running', attempt = 1, lease_owner = 'w', lease_expires_at = now() WHERE id = j;
    INSERT INTO job_errors (job_id, attempt, kind, message) VALUES (j, 1, 'retryable', 'x');
    INSERT INTO planted VALUES ('error_on_unfinished_attempt', j);
    -- dead_without_cause (dead with attempts left, and the last error was only retryable)
    j := pg_temp.new_job('p11'); UPDATE jobs SET state = 'dead', attempt = 1, finished_at = now() WHERE id = j;
    INSERT INTO job_errors (job_id, attempt, kind, message) VALUES (j, 1, 'retryable', 'x');
    INSERT INTO planted VALUES ('dead_without_cause', j);
    -- approval_gate_skipped
    j := pg_temp.new_job('p12', true); UPDATE jobs SET state = 'queued' WHERE id = j;
    INSERT INTO planted VALUES ('approval_gate_skipped', j);
    -- waiting_job_was_run
    j := pg_temp.new_job('p13', true); UPDATE jobs SET attempt = 2 WHERE id = j;
    INSERT INTO planted VALUES ('waiting_job_was_run', j);
    -- decision_mismatch
    j := pg_temp.new_job('p14'); UPDATE jobs SET decided_by = 'x' WHERE id = j;
    INSERT INTO planted VALUES ('decision_mismatch', j);
    -- escalation_mismatch (escalated before the deadline)
    j := pg_temp.new_job('p15', true); UPDATE jobs SET escalated_at = now() WHERE id = j;
    INSERT INTO planted VALUES ('escalation_mismatch', j);
    -- duplicate_idempotency_key (the unique constraint is dropped, so a second row gets in)
    j := pg_temp.new_job('p16');
    INSERT INTO jobs (type, client_id, idempotency_key, payload_hash, payload, state)
    VALUES ('test', 'client-1', 'p16', repeat('a', 64), '{}', 'queued');
    INSERT INTO planted VALUES ('duplicate_idempotency_key', NULL);
    -- acknowledged_job_missing
    INSERT INTO test_acknowledged_jobs (job_id) VALUES (ghost);
    INSERT INTO planted VALUES ('acknowledged_job_missing', ghost);
    -- acknowledged_job_not_finished (only in the after-drain view)
    j := pg_temp.new_job('p18');
    INSERT INTO test_acknowledged_jobs (job_id) VALUES (j);
    INSERT INTO planted VALUES ('acknowledged_job_not_finished', j);

    -- Every planted violation must show up under its own name and (where there is one) its own job id.
    FOR name, j IN SELECT invariant, job_id FROM planted LOOP
        ASSERT EXISTS (
            SELECT 1 FROM invariant_violations_after_drain v
            WHERE v.invariant = name AND (j IS NULL OR v.job_id = j)
        ), format('TEST FAILED: the planted violation for %s was not caught', name);
    END LOOP;

    -- The "anytime" view must not include the after-drain-only check.
    ASSERT NOT EXISTS (SELECT 1 FROM invariant_violations WHERE invariant = 'acknowledged_job_not_finished'),
        'acknowledged_job_not_finished belongs only in the after-drain view';

    -- And the list of tested invariants matches the list in the views.
    ASSERT in_view @> expected AND expected @> in_view,
        format('TEST FAILED: invariants in the views %s do not match the tested list %s', in_view, expected);

    RAISE NOTICE 'part 9 passed: % invariants each caught a planted violation', array_length(expected, 1);
END;
$$;

DO $$ BEGIN RAISE NOTICE 'ALL TESTS PASSED'; END $$;

ROLLBACK;
