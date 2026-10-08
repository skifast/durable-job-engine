-- schema.sql: the jobs table, the error history table, and the rules the
-- database enforces on its own.
--
-- Written against docs/spec.md (sections 4, 5, 6, 9) and ADR 001, 005, 006, 009, 011.
-- Needs Postgres 13 or newer (gen_random_uuid() is built in from 13).
-- Tested on Postgres 16.
--
-- Three layers of protection, from strongest to weakest:
--   1. CHECK constraints: rules about one row at rest ("a running job has a lease").
--   2. Triggers: rules about a change from one row version to the next
--      ("only these state changes are allowed", "attempt only goes up at a claim").
--   3. invariants.sql: rules that need two tables or outside knowledge, checked by
--      queries after every test run.
--
-- The API server and reaper still do their own checks (for example the fenced
-- UPDATE: WHERE lease_owner = $1 AND attempt = $2 AND state = 'running').
-- The database rules are the last line of defense if that code has a bug.

BEGIN;

-- ---------------------------------------------------------------------------
-- jobs
-- ---------------------------------------------------------------------------
CREATE TABLE jobs (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Identity and input. Never change after the insert (a trigger enforces this).
    type              text        NOT NULL CHECK (length(type) > 0),
    client_id         text        NOT NULL CHECK (length(client_id) > 0),
    -- Spec section 9: an empty key, or one longer than 255 characters, is refused with 400.
    idempotency_key   text        NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 255),
    -- SHA-256 of the canonical JSON of {type, payload}, as 64 lowercase hex characters.
    -- Used to tell "same key, same request" (200) from "same key, different request" (422).
    payload_hash      text        NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    payload           jsonb       NOT NULL,

    -- State and counters.
    state             text        NOT NULL CHECK (state IN
                          ('queued', 'waiting_approval', 'running', 'succeeded', 'dead', 'rejected')),
    -- attempt counts claims. It goes up by 1 at each claim and never goes down.
    -- It is also the fencing token (ADR 009).
    attempt           integer     NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    max_attempts      integer     NOT NULL DEFAULT 8 CHECK (max_attempts >= 1),
    -- A queued job can be claimed once run_at has passed. Retries set it in the future.
    run_at            timestamptz NOT NULL DEFAULT now(),

    -- Lease. Set only while state = 'running'.
    lease_owner       text,
    lease_expires_at  timestamptz,

    -- Approval (T2 to T5).
    approval_deadline timestamptz,          -- set at insert for jobs that need approval
    escalated_at      timestamptz,          -- set once by the reaper when the deadline passes
    decided_by        text,                 -- typed operator name, not verified (ADR 010)
    decided_at        timestamptz,
    decision_reason   text,                 -- required for a reject, optional for an approve

    -- Redrive (T14). Only the most recent redrive is kept here.
    redriven_by       text,
    redriven_at       timestamptz,

    -- Outcome. The error history is in job_errors.
    -- Spec section 5: at most 64 KB. A success with no result is stored as '{}'.
    result            jsonb       CHECK (octet_length(result::text) <= 65536),

    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    finished_at       timestamptz,          -- set exactly when state is succeeded, dead or rejected

    -- Spec section 9: at most one job per client and key.
    CONSTRAINT jobs_idempotency_unique UNIQUE (client_id, idempotency_key),

    -- attempt never exceeds max_attempts.
    CONSTRAINT jobs_attempt_within_max CHECK (attempt <= max_attempts),

    -- A queued job always has an attempt left. This is why a redrive has to raise
    -- max_attempts above attempt (T14): otherwise the job could be claimed again
    -- with nothing left to count.
    CONSTRAINT jobs_queued_has_attempts_left CHECK (state <> 'queued' OR attempt < max_attempts),

    -- A job that has run (running, succeeded, dead) has been claimed at least once.
    -- A job that is waiting for approval or was rejected has never been claimed.
    CONSTRAINT jobs_attempt_matches_state CHECK (
        (state IN ('running', 'succeeded', 'dead') AND attempt >= 1)
        OR (state IN ('waiting_approval', 'rejected') AND attempt = 0)
        OR state = 'queued'
    ),

    -- A running job has an owner and an expiry; any other job has neither.
    CONSTRAINT jobs_lease_matches_state CHECK (
        (state = 'running'  AND lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR (state <> 'running' AND lease_owner IS NULL AND lease_expires_at IS NULL)
    ),

    -- finished_at is set exactly when the state is succeeded, dead or rejected.
    CONSTRAINT jobs_finished_matches_state CHECK (
        (finished_at IS NOT NULL) = (state IN ('succeeded', 'dead', 'rejected'))
    ),

    -- A succeeded job has a result. No other job has one.
    CONSTRAINT jobs_result_matches_state CHECK ((state = 'succeeded') = (result IS NOT NULL)),

    -- Approval rules.
    -- A waiting job has a deadline and no decision yet.
    CONSTRAINT jobs_waiting_has_deadline CHECK (
        state <> 'waiting_approval' OR (approval_deadline IS NOT NULL AND decided_at IS NULL)
    ),
    -- A job that went through the approval gate has a decision once it leaves waiting_approval.
    -- This is the gate: no job with a deadline can be queued, running or finished without one.
    CONSTRAINT jobs_gate_not_skipped CHECK (
        approval_deadline IS NULL OR state = 'waiting_approval' OR decided_at IS NOT NULL
    ),
    CONSTRAINT jobs_decision_complete CHECK ((decided_by IS NULL) = (decided_at IS NULL)),
    CONSTRAINT jobs_decision_needs_gate CHECK (decided_at IS NULL OR approval_deadline IS NOT NULL),
    CONSTRAINT jobs_reason_needs_decision CHECK (decision_reason IS NULL OR decided_at IS NOT NULL),
    -- coalesce matters here: length(NULL) is NULL, and a CHECK passes when it evaluates to NULL.
    CONSTRAINT jobs_rejected_has_reason CHECK (
        state <> 'rejected' OR (decided_at IS NOT NULL AND coalesce(length(decision_reason), 0) > 0)
    ),
    CONSTRAINT jobs_escalation_needs_deadline CHECK (escalated_at IS NULL OR approval_deadline IS NOT NULL),

    -- Redrive: who and when are recorded together.
    CONSTRAINT jobs_redrive_complete CHECK ((redriven_by IS NULL) = (redriven_at IS NULL))
)
-- Starting values for the storage settings (ADR 001, ADR 011). The 30-minute soak
-- test decides whether they hold; change them here and in metrics-and-settings.md.
--   fillfactor 70: leave 30% of each page free so an updated row can stay in the same page.
--   autovacuum after 2% of rows change (default is 20%), so dead rows are cleaned early.
WITH (
    fillfactor = 70,
    autovacuum_vacuum_scale_factor = 0.02,
    autovacuum_analyze_scale_factor = 0.02
);

-- ---------------------------------------------------------------------------
-- job_errors: the error history (ADR 011)
-- ---------------------------------------------------------------------------
-- One row per attempt that ended in a failure or a lease expiry.
-- The primary key (job_id, attempt) means an attempt can end only once. So if two
-- reapers both try to requeue the same expired attempt, the second insert fails
-- instead of adding a second entry (spec invariant: "running the reaper twice
-- requeues a job only once").
CREATE TABLE job_errors (
    job_id      uuid        NOT NULL REFERENCES jobs (id),
    attempt     integer     NOT NULL CHECK (attempt >= 1),
    occurred_at timestamptz NOT NULL DEFAULT now(),
    -- retryable: the worker reported a failure that may work next time (includes a handler timeout)
    -- permanent: the worker reported that no retry can help
    -- lease_expired: the reaper took the job back
    kind        text        NOT NULL CHECK (kind IN ('retryable', 'permanent', 'lease_expired')),
    message     text        NOT NULL CHECK (length(message) <= 1000),
    PRIMARY KEY (job_id, attempt)
);

-- ---------------------------------------------------------------------------
-- Indexes
-- ---------------------------------------------------------------------------
-- Each one covers a small slice of the table, so it stays small even when the
-- table holds millions of finished jobs.

-- Claim (T6): oldest queued job whose run_at has passed.
CREATE INDEX jobs_claim_idx ON jobs (run_at, id) WHERE state = 'queued';

-- Reaper, expired leases (T12, T13).
CREATE INDEX jobs_lease_idx ON jobs (lease_expires_at) WHERE state = 'running';

-- Reaper, overdue approvals that are not escalated yet (T5).
CREATE INDEX jobs_approval_deadline_idx ON jobs (approval_deadline)
    WHERE state = 'waiting_approval' AND escalated_at IS NULL;

-- Operator page: list waiting and dead jobs, newest first, with paging.
-- Other lists (succeeded, queued) are not indexed for paging; see ADR 011.
CREATE INDEX jobs_operator_idx ON jobs (state, created_at DESC, id DESC)
    WHERE state IN ('waiting_approval', 'dead');

-- ---------------------------------------------------------------------------
-- Rules for a new row (T1, T2)
-- ---------------------------------------------------------------------------
CREATE FUNCTION jobs_check_insert() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    -- A job starts queued (T1, no approval needed) or waiting_approval (T2).
    IF NOT (
        (NEW.state = 'queued' AND NEW.approval_deadline IS NULL)
        OR NEW.state = 'waiting_approval'
    ) THEN
        RAISE EXCEPTION 'a new job must start as queued (no approval) or waiting_approval, not %', NEW.state
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    -- It starts clean: never claimed, nothing decided, escalated, redriven or finished.
    IF NEW.attempt <> 0 OR NEW.escalated_at IS NOT NULL OR NEW.decided_at IS NOT NULL
       OR NEW.redriven_at IS NOT NULL OR NEW.finished_at IS NOT NULL OR NEW.result IS NOT NULL THEN
        RAISE EXCEPTION 'a new job must start with attempt 0 and no decision, escalation, redrive or result'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER jobs_check_insert
    BEFORE INSERT ON jobs
    FOR EACH ROW EXECUTE FUNCTION jobs_check_insert();

-- ---------------------------------------------------------------------------
-- Rules for every change to an existing row (T3 to T14)
-- ---------------------------------------------------------------------------
-- Any change not listed in spec section 6 is refused.
-- The checks on timing (is the lease really expired? has the deadline passed?)
-- stay in the reaper's queries, because the same state change is legal for
-- different reasons (running -> queued happens on a failure report and on a
-- lease expiry).
CREATE FUNCTION jobs_check_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    -- 1. Which state changes are allowed.
    --    Same-state rows are the heartbeat (T7) and the escalation (T5); the
    --    other same-state updates are refused.
    IF NOT ((OLD.state, NEW.state) IN (
            ('queued',           'running'),            -- T6  claim
            ('running',          'running'),            -- T7  heartbeat
            ('running',          'succeeded'),          -- T8  success report
            ('running',          'queued'),             -- T9, T12  retry after a failure or lease expiry
            ('running',          'dead'),               -- T10, T11, T13  permanent failure, attempts used up
            ('waiting_approval', 'queued'),             -- T3  approve
            ('waiting_approval', 'rejected'),           -- T4  reject
            ('waiting_approval', 'waiting_approval'),   -- T5  escalate
            ('dead',             'queued')              -- T14 redrive
        )) THEN
        RAISE EXCEPTION 'state change % -> % is not allowed', OLD.state, NEW.state
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    -- 2. Fields that never change after the insert.
    IF (NEW.id, NEW.type, NEW.client_id, NEW.idempotency_key, NEW.payload_hash,
        NEW.payload, NEW.created_at, NEW.approval_deadline)
       IS DISTINCT FROM
       (OLD.id, OLD.type, OLD.client_id, OLD.idempotency_key, OLD.payload_hash,
        OLD.payload, OLD.created_at, OLD.approval_deadline) THEN
        RAISE EXCEPTION 'identity, payload and approval deadline cannot change'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    -- 3. attempt changes only at a claim, and by exactly 1 (T6).
    --    A claim must change it. This also means attempt never goes down.
    IF OLD.state = 'queued' AND NEW.state = 'running' THEN
        IF NEW.attempt <> OLD.attempt + 1 THEN
            RAISE EXCEPTION 'a claim must raise attempt by exactly 1 (was %, now %)', OLD.attempt, NEW.attempt
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    ELSIF NEW.attempt <> OLD.attempt THEN
        RAISE EXCEPTION 'attempt can change only at a claim (was %, now %)', OLD.attempt, NEW.attempt
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    -- 4. A heartbeat renews the lease. It cannot hand the lease to another worker.
    IF OLD.state = 'running' AND NEW.state = 'running'
       AND NEW.lease_owner IS DISTINCT FROM OLD.lease_owner THEN
        RAISE EXCEPTION 'a heartbeat cannot change the lease owner'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    -- 5. Escalation (T5) sets escalated_at once and changes nothing else about the state.
    IF OLD.state = 'waiting_approval' AND NEW.state = 'waiting_approval'
       AND NOT (OLD.escalated_at IS NULL AND NEW.escalated_at IS NOT NULL) THEN
        RAISE EXCEPTION 'a waiting job can only be updated to set escalated_at, and only once'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.escalated_at IS NOT NULL AND NEW.escalated_at IS DISTINCT FROM OLD.escalated_at THEN
        RAISE EXCEPTION 'escalated_at is set once and cannot change'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.escalated_at IS NULL AND NEW.escalated_at IS NOT NULL
       AND NOT (OLD.state = 'waiting_approval' AND NEW.state = 'waiting_approval') THEN
        RAISE EXCEPTION 'only the escalation (T5) can set escalated_at'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    -- 6. A decision is recorded once, when the job leaves waiting_approval (T3, T4).
    IF OLD.decided_at IS NOT NULL
       AND (NEW.decided_by, NEW.decided_at, NEW.decision_reason)
           IS DISTINCT FROM (OLD.decided_by, OLD.decided_at, OLD.decision_reason) THEN
        RAISE EXCEPTION 'this job already has a decision; a second one is refused'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.decided_at IS NULL AND NEW.decided_at IS NOT NULL AND OLD.state <> 'waiting_approval' THEN
        RAISE EXCEPTION 'only a waiting_approval job can be decided'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    -- 7. Redrive (T14): the only time max_attempts changes. It must be raised above
    --    attempt, and the operator is recorded. Nothing else can touch these fields.
    IF OLD.state = 'dead' AND NEW.state = 'queued' THEN
        IF NEW.max_attempts <= OLD.attempt THEN
            RAISE EXCEPTION 'a redrive needs max_attempts greater than attempt (attempt %, max_attempts %)',
                OLD.attempt, NEW.max_attempts
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF NEW.redriven_by IS NULL OR NEW.redriven_at IS NULL THEN
            RAISE EXCEPTION 'a redrive must record who did it'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    ELSIF NEW.max_attempts <> OLD.max_attempts
          OR (NEW.redriven_by, NEW.redriven_at) IS DISTINCT FROM (OLD.redriven_by, OLD.redriven_at) THEN
        RAISE EXCEPTION 'max_attempts and the redrive fields change only at a redrive'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    NEW.updated_at := now();
    RETURN NEW;
END;
$$;

CREATE TRIGGER jobs_check_update
    BEFORE UPDATE ON jobs
    FOR EACH ROW EXECUTE FUNCTION jobs_check_update();

COMMIT;
