# Database

| File | What it is |
|---|---|
| [`schema.sql`](schema.sql) | The `jobs` and `job_errors` tables, indexes, and the rules the database enforces on its own (CHECK constraints and two triggers) |
| [`invariants.sql`](invariants.sql) | The correctness rules from [spec section 4](../docs/spec.md#4-invariants), as two views that must return zero rows |
| [`tests/test_schema.sql`](tests/test_schema.sql) | Tests that drive the real state changes, try to break every rule, and plant a violation for each invariant |

The design decisions behind these files are in [ADR 011](../docs/adr/011-schema-error-history-and-enforced-transitions.md).

## Running it

You need Postgres 13 or newer (tested on 16) and a scratch database. With Docker:

```
docker run --rm -d --name jobengine-pg -e POSTGRES_PASSWORD=dev -p 5432:5432 postgres:16
export PGPASSWORD=dev
createdb -h localhost -U postgres jobengine_dev      # or: docker exec jobengine-pg createdb -U postgres jobengine_dev
psql -h localhost -U postgres -d jobengine_dev -v ON_ERROR_STOP=1 -f db/schema.sql
psql -h localhost -U postgres -d jobengine_dev -v ON_ERROR_STOP=1 -f db/invariants.sql
psql -h localhost -U postgres -d jobengine_dev -v ON_ERROR_STOP=1 -f db/tests/test_schema.sql
```

The last command should end with `ALL TESTS PASSED`. The test script empties the `jobs` table and rolls back when it finishes, so use a scratch database only.

After any load, soak or chaos run, check the rules with:

```
SELECT * FROM invariant_violations_after_drain;
```

Zero rows means every invariant held.
