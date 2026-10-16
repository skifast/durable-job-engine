# Database

| File | What it is |
|---|---|
| [`schema.sql`](schema.sql) | The `jobs`, `job_errors` and `api_keys` tables, indexes, and the rules the database enforces on its own (CHECK constraints and triggers) |
| [`invariants.sql`](invariants.sql) | The correctness rules from [spec section 4](../docs/spec.md#4-invariants), as two views that must return zero rows |
| [`tests/test_schema.sql`](tests/test_schema.sql) | Tests that drive the real state changes, try to break every rule, and plant a violation for each invariant |
| [`tests/test_duplicate_key_wait.sh`](tests/test_duplicate_key_wait.sh) | Two-session test of what happens when a duplicate key arrives while the first request is still open (ADR 005). Needs `psql` and `python3` |

The design decisions behind these files are in [ADR 011](../docs/adr/011-schema-error-history-and-enforced-transitions.md) (jobs and error history) and [ADR 013](../docs/adr/013-authentication-api-keys.md) (API keys).

## Running it

You need Postgres 13 or newer (tested on 16) and a scratch database. The database must use the UTF8 encoding (`schema.sql` refuses to load otherwise, because `length()` counts bytes in SQL_ASCII); Docker's `postgres` image does by default. With Docker:

```
docker run --rm -d --name jobengine-pg -e POSTGRES_PASSWORD=dev -p 5432:5432 postgres:16
export PGPASSWORD=dev
createdb -h localhost -U postgres jobengine_dev      # or: docker exec jobengine-pg createdb -U postgres jobengine_dev
psql -h localhost -U postgres -d jobengine_dev -v ON_ERROR_STOP=1 -f db/schema.sql
psql -h localhost -U postgres -d jobengine_dev -v ON_ERROR_STOP=1 -f db/invariants.sql
psql -h localhost -U postgres -d jobengine_dev -v ON_ERROR_STOP=1 -f db/tests/test_schema.sql
```

The last command should end with `ALL TESTS PASSED`. Then run the two-session test the same way:

```
PGHOST=localhost PGUSER=postgres PGDATABASE=jobengine_dev bash db/tests/test_duplicate_key_wait.sh
```

It should also end with `ALL TESTS PASSED`. `test_schema.sql` empties the `jobs` table and rolls back when it finishes, so use a scratch database only.

After any load, soak or chaos run, check the rules with:

```
SELECT * FROM invariant_violations_after_drain;
```

Zero rows means every invariant held.
