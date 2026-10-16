#!/usr/bin/env bash
# test_duplicate_key_wait.sh: checks what happens when a second request arrives with the
# same idempotency key while the first request's transaction is still open (ADR 005).
#
# Needs two database sessions at the same time, so it cannot be a plain .sql file.
# Run it against a scratch database that already has db/schema.sql loaded:
#   PGHOST=localhost PGUSER=postgres PGDATABASE=jobengine_dev bash db/tests/test_duplicate_key_wait.sh
# It uses the standard Postgres environment variables (PGHOST, PGPORT, PGUSER, PGDATABASE,
# PGPASSWORD). It needs psql and python3 (python3 is used only for timing).
# It adds rows for a test client and removes them when it finishes.
#
# Session A is the first request: it inserts the key and keeps its transaction open.
# Session B is the duplicate: lock_timeout is 2 s, and it does the submit's insert.
#   Case 1: A stays open longer than 2 s, then commits. B must fail with a lock timeout after about 2 s.
#   Case 2: A commits within 2 s. B must wait, then insert nothing (the duplicate is detected).
#   Case 3: A rolls back. B must wait, then insert the job (the first request left nothing behind).

set -u
PSQL="psql -X -q -t -A -v VERBOSITY=verbose"
CLIENT="dupwait-test"
FAILED=0

now() { python3 -c 'import time; print(time.time())'; }

cleanup() { $PSQL -c "DELETE FROM jobs WHERE client_id = '$CLIENT'" >/dev/null 2>&1; }
trap cleanup EXIT
cleanup

# $1 = case label, $2 = seconds A holds its transaction open, $3 = how A ends (COMMIT or ROLLBACK)
run_case() {
    local label="$1" hold="$2" ending="$3"
    local key="dup-$RANDOM-$RANDOM"
    local insert="INSERT INTO jobs (type, client_id, idempotency_key, payload_hash, payload, state)
                  VALUES ('test', '$CLIENT', '$key', repeat('a', 64), '{}', 'queued')"

    # Session A: insert, hold the transaction open, then end it.
    $PSQL >/dev/null 2>&1 <<EOF &
BEGIN;
$insert;
SELECT pg_sleep($hold);
$ending;
EOF
    local a_pid=$!
    sleep 0.7                                   # give A time to insert and start holding

    # Session B: the duplicate, with the 2 s lock timeout.
    local start end out
    start=$(now)
    out=$($PSQL 2>&1 <<EOF
SET lock_timeout = '2s';
$insert ON CONFLICT (client_id, idempotency_key) DO NOTHING RETURNING id;
EOF
)
    end=$(now)
    wait "$a_pid"

    local elapsed rows
    elapsed=$(python3 -c "print(round($end - $start, 1))")
    rows=$($PSQL -c "SELECT count(*) FROM jobs WHERE client_id = '$CLIENT' AND idempotency_key = '$key'")
    echo "--- $label"
    echo "    B returned after ${elapsed}s; rows with this key afterwards: $rows"
    echo "    B output: ${out:-<nothing>}"
    RESULT_OUT="$out"; RESULT_ELAPSED="$elapsed"; RESULT_ROWS="$rows"
}

check() { # $1 = description, $2 = result (0 = pass)
    if [ "$2" -eq 0 ]; then echo "    PASS: $1"; else echo "    FAIL: $1"; FAILED=1; fi
}

run_case "Case 1: A holds 5 s then commits" 5 COMMIT
check "B fails with SQLSTATE 55P03 (lock_not_available)" "$(echo "$RESULT_OUT" | grep -q '55P03' && echo 0 || echo 1)"
check "B gave up after about 2 s (1.5 to 3 s)" "$(python3 -c "print(0 if 1.5 <= $RESULT_ELAPSED <= 3.0 else 1)")"
check "exactly one row exists (A's)" "$([ "$RESULT_ROWS" = "1" ] && echo 0 || echo 1)"

run_case "Case 2: A holds 1.5 s then commits" 1.5 COMMIT
check "B inserted nothing and returned no error" "$([ -z "$RESULT_OUT" ] && echo 0 || echo 1)"
check "B waited for A (more than 0.3 s) but less than the timeout" "$(python3 -c "print(0 if 0.3 < $RESULT_ELAPSED < 2.0 else 1)")"
check "exactly one row exists (A's)" "$([ "$RESULT_ROWS" = "1" ] && echo 0 || echo 1)"

run_case "Case 3: A holds 1.5 s then rolls back" 1.5 ROLLBACK
check "B inserted the job and got its id back" "$(echo "$RESULT_OUT" | grep -Eq '^[0-9a-f-]{36}$' && echo 0 || echo 1)"
check "exactly one row exists (B's)" "$([ "$RESULT_ROWS" = "1" ] && echo 0 || echo 1)"

echo
if [ "$FAILED" -eq 0 ]; then echo "ALL TESTS PASSED"; else echo "SOME TESTS FAILED"; exit 1; fi
