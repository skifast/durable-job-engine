#!/bin/bash
# Fresh database, engine and seed data for browser-check.mjs.
#
#   ./setup.sh WORKDIR        WORKDIR gets the engine binary, the keys and the job ids
#
# Needs Postgres (psql, createdb, dropdb) on the PATH and a server that your user may create
# databases on. It makes a database called jobengine_ui and starts an engine on port 18090.
# Stop the engine afterwards with:  lsof -ti tcp:18090 | xargs kill
set -e
W=$(cd "$1" 2>/dev/null && pwd || { mkdir -p "$1" && cd "$1" && pwd; })
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../../.." && pwd)
cd "$ROOT"
# Stop any engine already on the port (it holds the database open).
lsof -ti tcp:18090 | xargs kill 2>/dev/null || true; sleep 1
dropdb --if-exists jobengine_ui && createdb jobengine_ui
psql -q -d jobengine_ui -v ON_ERROR_STOP=1 -f db/schema.sql >/dev/null
export DATABASE_URL="postgres://$(whoami)@localhost:5432/jobengine_ui"
go build -o "$W/engine" ./cmd/engine
go build -o "$W/jobkeys" ./cmd/jobkeys
key(){ "$W/jobkeys" create -name "$1" -role "$2" | grep -o '[A-Za-z0-9_-]\{40,\}' | head -1; }
CK=$(key ui-client client); WK=$(key ui-worker worker); OA=$(key ops-alice operator); OB=$(key ops-bob operator)
printf '%s\n' "$CK" "$WK" "$OA" "$OB" > "$W/keys"
(JOB_TYPES_FILE="$HERE/types.json" LISTEN_ADDR=127.0.0.1:18090 nohup "$W/engine" > "$W/engine.log" 2>&1 &)
sleep 1.5
U=http://127.0.0.1:18090
sub(){ curl -s -X POST $U/jobs -H "Authorization: Bearer $CK" -H "Idempotency-Key: $(uuidgen)" -d "{\"type\":\"$1\",\"payload\":$2}" | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])'; }
# Payloads and messages that would run as code if the page ever treated text as markup.
sub wire_payment '{"amount_cents":125000,"to":"ACME Ltd","note":"<img src=x onerror=alert(1)>"}' > "$W/id_wire"
sub gated '{"task":"rotate keys"}' > "$W/id_g1"
sub gated '{"task":"purge cache"}' > "$W/id_g2"
sub gated '{"task":"overdue approval"}' > "$W/id_esc"
sub gated '{"task":"approved from two windows"}' > "$W/id_race"
sub one_try '{"n":1}' > "$W/id_dead"
sub plain '{"n":2}' > "$W/id_ok"
sub plain '{"n":3}' > "$W/id_q"
claim(){ curl -s -X POST $U/claim -H "Authorization: Bearer $WK"; }
A1=$(claim | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')
curl -s -X POST $U/jobs/$A1/report -H "Authorization: Bearer $WK" -d '{"attempt":1,"outcome":"retryable","message":"downstream <script>alert(1)</script> timed out"}' >/dev/null
A2=$(claim | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')
curl -s -X POST $U/jobs/$A2/report -H "Authorization: Bearer $WK" -d '{"attempt":1,"outcome":"success","result":{"sent":true}}' >/dev/null
# An approval whose deadline has passed: the engine's reaper escalates it within 5 seconds.
psql -q -d jobengine_ui -c "BEGIN; SET LOCAL session_replication_role = replica; UPDATE jobs SET approval_deadline = now() - interval '1 minute' WHERE id = '$(cat "$W/id_esc")'; COMMIT;"
sleep 7
psql -d jobengine_ui -tA -c "select type||' '||state||' escalated='||(escalated_at is not null) from jobs order by created_at"
