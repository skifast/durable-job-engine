# API contract

**Status:** Approved 2026-10-09. Implemented so far: health check, submit, read one job, claim, heartbeat and report. Not yet: list jobs, approve, reject, redrive.

Every request and response between a client, a worker or the operator page and the engine. It follows [the spec](spec.md) (sections 6, 7, 9 and 10), [ADR 002](adr/002-workers-call-api.md) (everything goes through the API) and [ADR 013](adr/013-authentication-api-keys.md) (API keys). Where this contract adds something the spec does not say, it is listed under [Changes this contract makes to the spec](#changes-this-contract-makes-to-the-spec), and the choices made are under [Decisions](#decisions).

## Conventions

- JSON in and out, UTF-8. Send `Content-Type: application/json` on requests with a body.
- Timestamps are RFC 3339 in UTC with milliseconds, for example `2026-10-09T20:15:30.123Z`.
- Job ids are UUIDs.
- A field with no value is `null`, not left out.
- No version prefix in the path for now (decision 5).
- **Authentication.** Every endpoint except `GET /healthz` needs `Authorization: Bearer <api key>`. The key's name and role come from the `api_keys` table.
  - No key, an unknown key or a revoked key: 401 `unauthorized`
  - A valid key whose role may not make this call: 403 `forbidden`
- **Errors** all have the same shape, and `code` is the part a program should check:

  ```json
  { "error": { "code": "lease_lost", "message": "This worker no longer holds the lease for attempt 2.", "details": { "state": "queued" } } }
  ```

  `details` is optional and depends on the code.
- **The order of checks** is: key (401), role (403), request format (400), then the work. A job that exists but belongs to another client looks like a job that does not exist (404).

## Endpoints at a glance

| Call | Role | What it does | Spec |
|---|---|---|---|
| `POST /jobs` | client | Submit a job | T1, T2 |
| `GET /jobs/{id}` | client (own jobs), operator | One job, with its error history | |
| `GET /jobs` | client (own jobs), operator | List jobs, filtered and paged | |
| `POST /claim` | worker | Take the next due job | T6 |
| `POST /jobs/{id}/heartbeat` | worker | Renew the lease | T7 |
| `POST /jobs/{id}/report` | worker | Report success or failure | T8, T9, T10, T11 |
| `POST /jobs/{id}/approve` | operator | Approve a waiting job | T3 |
| `POST /jobs/{id}/reject` | operator | Reject a waiting job | T4 |
| `POST /jobs/{id}/redrive` | operator | Put a dead job back in the queue | T14 |
| `GET /healthz` | none | Is the engine up and connected to Postgres | |

The reaper has no endpoint. It runs inside the engine and does T5 (escalate), T12 and T13 (lease expiry) directly in Postgres.

## Job representations

**Full job** (returned by submit, `GET /jobs/{id}`, approve, reject and redrive):

```json
{
  "id": "6f1c0e5a-6a53-4d6e-9d57-0a2b7f2f6b11",
  "type": "send_invoice",
  "client_id": "billing-service",
  "idempotency_key": "0b6d6e0e-4c34-4c3c-a2f4-9e0d8d4a1f10",
  "state": "queued",
  "attempt": 0,
  "max_attempts": 8,
  "run_at": "2026-10-09T20:15:30.123Z",
  "lease_owner": null,
  "lease_expires_at": null,
  "approval_deadline": null,
  "escalated_at": null,
  "decided_by": null,
  "decided_at": null,
  "decision_reason": null,
  "redriven_by": null,
  "redriven_at": null,
  "payload": { "invoice_id": 1234 },
  "result": null,
  "errors": [
    { "attempt": 1, "kind": "retryable", "message": "downstream timeout", "occurred_at": "2026-10-09T20:15:35.001Z" }
  ],
  "created_at": "2026-10-09T20:15:30.123Z",
  "updated_at": "2026-10-09T20:15:30.123Z",
  "finished_at": null
}
```

`errors` is in attempt order. `kind` is `retryable`, `permanent`, `unclassified` or `lease_expired`.

**Job summary** (items in a list). It leaves out the payload, result and full error history so a page of jobs stays small:

```json
{
  "id": "6f1c0e5a-6a53-4d6e-9d57-0a2b7f2f6b11",
  "type": "send_invoice",
  "client_id": "billing-service",
  "state": "dead",
  "attempt": 8,
  "max_attempts": 8,
  "run_at": "2026-10-09T20:18:51.000Z",
  "approval_deadline": null,
  "escalated_at": null,
  "created_at": "2026-10-09T20:15:30.123Z",
  "finished_at": "2026-10-09T20:19:40.512Z",
  "last_error": { "attempt": 8, "kind": "retryable", "message": "downstream timeout" }
}
```

`last_error` is `null` when the job has no errors.

**Work item** (what a worker gets from `/claim`). It has only what a handler needs:

```json
{
  "id": "6f1c0e5a-6a53-4d6e-9d57-0a2b7f2f6b11",
  "type": "send_invoice",
  "payload": { "invoice_id": 1234 },
  "attempt": 3,
  "lease_expires_at": "2026-10-09T20:16:00.123Z",
  "handler_time_limit_ms": 60000
}
```

`attempt` is the fencing token. The worker sends it back on every heartbeat and report. `handler_time_limit_ms` is the job type's handler time limit from the engine's job-types file; the worker cancels the handler when it passes (ADR 014). If the type is no longer in the file, it is the 60 s default.

## Submit a job

`POST /jobs` (role: client)

**Headers:** `Idempotency-Key` is required, 1 to 255 characters. Use a random value such as a UUID.

**Body:**

```json
{ "type": "send_invoice", "payload": { "invoice_id": 1234 } }
```

`type` must be a job type the engine is configured for. `payload` can be any JSON value. The job's `client_id` is the name of the calling key.

**What the engine does:** it takes the SHA-256 of the canonical JSON (RFC 8785) of `{"type": ..., "payload": ...}` as `payload_hash`, then does one `INSERT ... ON CONFLICT (client_id, idempotency_key) DO NOTHING` with a 2 s lock timeout (ADR 005). If a job type needs approval, the job starts as `waiting_approval` with a deadline; otherwise it starts as `queued`.

| Status | Code | When |
|---|---|---|
| 201 | | New job. Body: the full job. `Location: /jobs/{id}` |
| 200 | | Same key and same payload as an earlier submit. Body: the original job in its current state, including when it has already ended |
| 400 | `invalid_request` | Missing or empty `Idempotency-Key`, a key over 255 characters, bad JSON, no `type` or `payload`, or an unknown job type. Nothing is stored, so the key is still free |
| 409 | `request_in_progress` | The same key arrived while the first request was still being saved and did not finish within 2 s. The `Retry-After: 1` header says when to retry |
| 413 | `payload_too_large` | The payload is over 64 KB of compact JSON (decision 3) |
| 422 | `idempotency_key_reused` | Same key, different payload or type. Nothing changes |

## Read jobs

### One job

`GET /jobs/{id}` (role: client for its own jobs, operator for any)

- 200: the full job
- 404 `not_found`: no such job, or it belongs to another client

### List jobs

`GET /jobs` (role: client for its own jobs, operator for all)

| Parameter | Meaning |
|---|---|
| `state` | One or more states, comma separated: `queued`, `waiting_approval`, `running`, `succeeded`, `dead`, `rejected` |
| `escalated` | `true` returns waiting jobs whose approval deadline has passed and that the reaper has escalated. It cannot be combined with another `state` |
| `type` | One job type |
| `client_id` | Operator only. A client always gets its own jobs |
| `limit` | 1 to 200. Default 50 |
| `cursor` | The `next_cursor` from the previous page |

Newest first (by `created_at`, then `id`). The cursor marks a position, not a page number, so jobs submitted while someone is paging do not shift the pages.

```json
{ "jobs": [ /* job summaries */ ], "next_cursor": "opaque-string-or-null" }
```

`next_cursor` is `null` on the last page. A bad `state`, `limit` or `cursor` returns 400 `invalid_request`.

Lists of `waiting_approval` and `dead` jobs are indexed (ADR 011). A list of `succeeded` or `queued` jobs on a large table can be slow; the operator page does not need it.

## Worker calls

### Claim

`POST /claim` (role: worker). No body.

The engine picks the oldest queued job whose `run_at` has passed (`FOR UPDATE SKIP LOCKED`), sets the lease owner to the key's name, the lease to now plus the lease length, and raises `attempt` by 1, all in one commit (ADR 001).

- 200: a work item
- 204: nothing is due. The worker waits 100 ms and asks again

### Heartbeat

`POST /jobs/{id}/heartbeat` (role: worker)

```json
{ "attempt": 3 }
```

Accepted only if the job is `running`, its `lease_owner` is the calling key's name, and its `attempt` matches (the fence).

- 200: `{ "lease_expires_at": "..." }`. The lease is renewed
- 409 `lease_lost`: the fence did not match. Nothing changed. The worker must cancel its handler. `details` has the job's current `state`
- 404 `not_found`: no such job

### Report

`POST /jobs/{id}/report` (role: worker)

```json
{ "attempt": 3, "outcome": "success", "result": { "sent": true } }
```

```json
{ "attempt": 3, "outcome": "retryable", "message": "downstream timeout" }
```

| Field | Meaning |
|---|---|
| `attempt` | The fencing token from the work item |
| `outcome` | `success`, `retryable`, `permanent` or `unclassified` (the worker sends this when a handler raised an error it did not classify, spec section 10) |
| `result` | With `success` only. Optional. Stored as `{}` if left out. At most 64 KB of compact JSON |
| `message` | With the three failure outcomes. Required. The engine keeps the first 1,000 characters |

Accepted only if the job is `running`, its `lease_owner` is the calling key's name, and its `attempt` matches. Then, in one transaction:

| Outcome | Result | Spec |
|---|---|---|
| `success` | state `succeeded`, result stored, lease cleared | T8 |
| `permanent` | state `dead`, error recorded | T10 |
| `retryable` or `unclassified`, attempts left | state `queued`, `run_at` set to now plus the backoff delay (ADR 004), error recorded | T9 |
| `retryable` or `unclassified`, no attempts left | state `dead`, error recorded | T11 |

- 200: `{ "state": "queued", "attempt": 3, "run_at": "...", "replayed": false }`. `state` is where the job went. `run_at` is set only when it was requeued
- 200 with `"replayed": true`: this exact outcome was already recorded for this attempt (see below). Nothing changed this time
- 409 `lease_lost`: the fence did not match and the outcome was not already recorded. Nothing changed. The current lease holder decides the outcome
- 413 `result_too_large`: the result is over 64 KB. Nothing changed. The worker should report a `permanent` failure instead (decision 2)
- 400 `invalid_request`: unknown outcome, or no message on a failure
- 404 `not_found`

**Repeated reports.** A worker retries a report if the answer is lost (spec section 11). The first try may already have been applied. So a report is treated as a repeat, and answered with 200 and `"replayed": true`, when the attempt has already ended with the same outcome: for `success`, the job is `succeeded` at that attempt; for a failure, the error history has an entry for that attempt with the same kind. A lease that expired and was requeued by the reaper has an entry of kind `lease_expired`, so a late report after that is still refused with 409.

## Operator calls

All three return the full job, in its new state.

### Approve

`POST /jobs/{id}/approve` (role: operator). The body is optional: `{ "reason": "looks fine" }`.

Allowed only while the job is `waiting_approval`, including after it was escalated. Sets `decided_by` to the key's name, `decided_at`, `run_at` to now, and the state to `queued` (T3).

- 200: the job
- 404 `not_found`
- 409 `not_waiting_approval`: the job is not waiting. This is also the answer to a second decision. `details` has the current `state` and, if there is one, `decided_by`

### Reject

`POST /jobs/{id}/reject` (role: operator)

```json
{ "reason": "duplicate of an earlier request" }
```

`reason` is required, not empty, at most 1,000 characters. Same rules as approve. State becomes `rejected` and `finished_at` is set (T4).

- 200: the job
- 400 `invalid_request`: no reason
- 404 `not_found`
- 409 `not_waiting_approval`

### Redrive

`POST /jobs/{id}/redrive` (role: operator)

```json
{ "max_attempts": 12 }
```

Allowed only from `dead`. Sets the new `max_attempts`, clears `finished_at`, records `redriven_by` and `redriven_at`, sets `run_at` to now, and the state to `queued`. `attempt` is not reset (T14, ADR 009).

- 200: the job
- 400 `max_attempts_too_low`: `max_attempts` is not greater than the job's `attempt`. `details` has the current `attempt`
- 400 `max_attempts_too_high`: `max_attempts` is over 100
- 400 `invalid_request`: `max_attempts` is missing or not a whole number
- 404 `not_found`
- 409 `not_dead`

## Who am I

`GET /whoami` (any valid key: client, worker or operator)

- 200: `{ "name": "ops-alice", "role": "operator" }`. `name` is the key's name, the value recorded as `client_id`, `lease_owner` or `decided_by`
- 401 `unauthorized`

The operator page uses it at sign-in (ADR 015).

## Operator page

`GET /ui/` and the files under it (`/ui/app.mjs`, `/ui/style.css` and so on) are the operator page (ADR 015). They need no key. Every response carries a Content-Security-Policy header and `X-Frame-Options: DENY`. `GET /` redirects to `/ui/`. The page itself calls only the endpoints in this document.

## Health

`GET /healthz` (no key). 200 `{ "status": "ok" }` when the engine can read from the jobs table within 2 seconds, otherwise 503 `{ "status": "database_unavailable" }`.

It reads from the jobs table, and not just "SELECT 1", so a lock on that table (a migration, for example) makes the engine report unhealthy, as does a database that cannot be reached, a hung database, and a pool with every connection busy. Killed connections are replaced and the check recovers. It does not check that the reaper is running or the schema's version (ADR 016). A load balancer should probe with a timeout of at least 2.5 seconds, longer than the engine's own 2 seconds.

## Errors that can come from any call

| Status | Code | Meaning |
|---|---|---|
| 404 | `not_found` | No such endpoint (a path the engine does not have). Checked before the key |
| 405 | `method_not_allowed` | The path exists but not with this method. The `Allow` header and `details.allow` list the methods that work. Checked before the key |
| 401 | `unauthorized` | Missing, unknown or revoked key |
| 403 | `forbidden` | The key's role may not make this call |
| 500 | `internal_error` | A bug in the engine. Nothing is promised about the state of the request |
| 503 | `database_unavailable` | Postgres could not be reached or the request timed out waiting for it. Nothing was committed, so the caller retries (spec section 11) |

## Tests this contract implies

- Every endpoint returns 401 without a key and with a revoked key, and 403 for the other roles
- Submit returns 201, then 200 for the same key and payload, 422 for the same key with a different payload or type, and 409 while the first request is open
- A client gets 404 for another client's job from `GET /jobs/{id}`, and 403 on the approve, reject and redrive calls
- A heartbeat or report with the wrong worker or the wrong attempt returns 409 and changes nothing
- A repeated report returns 200 with `replayed: true`; a late report after a `lease_expired` entry returns 409
- Two operators acting on one job: one 200, one 409 `not_waiting_approval`
- `decided_by`, `lease_owner` and `client_id` always equal the calling key's name, whatever the request body says
- Each state change follows the spec: the database's own rules (ADR 011) must never fire during these tests, so a trigger error means the API tried an illegal change

## Changes this contract makes to the spec

These edits were made to the spec when the contract was approved:

1. **Heartbeat and report paths include the job id** (`/jobs/{id}/heartbeat`, `/jobs/{id}/report`). The spec lists `/heartbeat` and `/report` without ids.
2. **Repeated reports are accepted** (the `replayed` rule). The spec says the worker retries a report (section 11) but not what the engine answers to a repeat.
3. **Error messages are cut to 1,000 characters** by the engine instead of being refused.
4. **A client reading another client's job gets 404** (from ADR 013).
5. **`/claim` takes no body.** There is no way for a worker to ask for only certain job types in this version.
6. **Size and range limits** (decisions 2, 3 and 7), and **job types come from a configuration file** (decision 6).

## Decisions

Taken on 2026-10-09 when this contract was approved.

1. **Repeated reports are accepted.** A repeat of a report that was already applied is answered with 200 and `replayed: true`. Otherwise every worker would have to work out from the job whether its report landed.
2. **A result over 64 KB is refused with 413 and nothing changes.** The worker then reports a `permanent` failure, because a handler that returns too much is a bug and a retry will not fix it.
3. **Payloads and results are limited to 64 KB of compact JSON** (the JSON text with no extra whitespace). The database also refuses anything over 128 KB, as a backstop that catches an engine bug. It is higher because Postgres prints JSON with extra spaces, so a payload the API accepted could print as more than 64 KB.
4. **`payload_hash` uses RFC 8785** (the JSON Canonicalization Scheme), so two clients that send the same data with different key order or spacing get the same hash.
5. **No `/v1` in the path.** A version can be added when a breaking change is needed.
6. **Job types are set in a configuration file** that the engine reads when it starts. For each type: whether it needs approval, the approval window (default 24 h), the handler time limit (default 60 s) and `max_attempts` (default 8, between 1 and 100). A new job type needs a restart.
7. **`max_attempts` is between 1 and 100.** A redrive sets a number greater than the job's `attempt` and at most 100.
