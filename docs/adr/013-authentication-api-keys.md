# ADR 013: Authentication with API keys

**Status:** Accepted (2026-10-09)  
**Date:** 2026-10-09  
**Spec sections:** 2 (goals and non-goals), 7 (components), 9 (idempotency)  
**Depends on:** ADR 002 (everything goes through the API), ADR 005 (client_id), ADR 009 (lease_owner), ADR 010 (operator page)  
**Changes:** the "no login" and "typed name" parts of ADR 010, and the security limits in ADR 005  

## Context

The spec started with authentication as a non-goal, so three fields are labels that anyone can set:

- `client_id` on a submit is a header. ADR 005 lists what that allows: reading another client's job by guessing its key, taking a key before its owner uses it, and learning that a key exists
- `lease_owner` is the worker's name, sent by the worker. Anyone who can reach the API can claim jobs or report results
- `decided_by` on an approval is a name typed into the operator page. The approval gate stops a job from running before a decision, but nothing shows who made it

The read endpoints also show every job's payload and result to anyone who asks.

The goal now is to know who is calling and to limit what each kind of caller can do. Not goals: user accounts, passwords, single sign-on, and separate organizations.

An operator is a person who runs the system and uses the operator page.

## Options

### A. Keep authentication a non-goal

**For**

- Nothing to build

**Against**

- The approval step is only a label, and the three attacks in ADR 005 stay open. A reader of the project would reasonably ask who is allowed to approve

### B. One shared secret for the whole API

**For**

- Very small: one setting and one check

**Against**

- Every caller can do everything. A worker could approve a job, and a client could read every other client's jobs
- It cannot tell callers apart, so `client_id`, `lease_owner` and `decided_by` would still be labels

### C. One API key per caller, each with a role (chosen)

**For**

- Every request is tied to a named caller, and a role limits what that caller can do
- `client_id`, `lease_owner` and `decided_by` come from the key, so they are verified
- Small to build: one table, one check on each request
- A key can be revoked on its own without touching the others

**Against**

- Keys have to be created, handed out and replaced by someone
- A stolen key works until it is revoked
- Humans share a key unless each gets their own, and then `decided_by` cannot tell them apart

### D. User accounts with passwords and sessions, or sign-in through an identity provider (SSO)

**For**

- The normal way to identify people, with per-person identity and lockouts

**Against**

- A large piece (user storage, password handling, sessions, recovery, SSO setup) that is not the distributed-systems part of this project

### E. Client certificates (mutual TLS)

**For**

- Strong identity checked at the connection

**Against**

- Certificates have to be issued, renewed and revoked, and a browser page makes that harder

## Decision

Use C. The rules:

- **Keys.** A key is a random value of at least 32 bytes. It is shown once, when it is created. Postgres stores only its SHA-256 hash, because a long random key cannot be guessed and does not need the slow hashing used for passwords
- **Table.** `api_keys` holds an id, a unique name, a role, the key hash, a created time and a revoked time. For a client key the name is the `client_id`. The table is in `db/schema.sql`. A trigger refuses any change to a key's name, role or hash, and makes a revoke final
- **Sending.** The caller sends the key as `Authorization: Bearer <key>` on every request, except a health check. The API hashes it and looks it up. No key, an unknown key or a revoked key returns 401. A valid key that is not allowed to do the thing returns 403
- **Roles.**

  | Role | May do | Identity taken from the key |
  |---|---|---|
  | client | submit jobs; read its own jobs | name becomes `client_id` |
  | worker | `/claim`, `/heartbeat`, `/report` | name becomes `lease_owner` |
  | operator | list and read all jobs; approve, reject, redrive | name becomes `decided_by` and the redrive record |

- **One key per worker and per person.** Each worker has its own key (`worker-1`, `worker-2`, ...), so `lease_owner` says which worker it was. Each operator has their own key, so `decided_by` says who acted
- **Clients see only their own jobs.** A client asking for another client's job gets 404, the same answer as for a job that does not exist, so it cannot learn which ids exist
- **The `X-Client-Id` header is removed.** The idempotency rules in ADR 005 do not change. The key is still unique per (client_id, key), and client_id now comes from the caller's key
- **Issuing and revoking.** Keys are created and revoked with a command-line tool run by someone with database access. There is no API endpoint for keys, so there is no administrator role to attack
- **Revoking takes effect on the next request.** The API looks the key up on each request and does not cache it
- **The reaper** runs inside the engine and talks to Postgres directly, so it needs no key
- **The operator page** asks the operator for their key and keeps it in the page's memory only. Nothing is stored in the browser, and a reload asks again. The page sends the key like any other client
- **Logs** record a key's name and never the key

## Consequences

- ADR 005's three attacks no longer work across clients: a client can only use its own `client_id`. That ADR's security limits no longer apply to separate clients, and a note there points to this ADR
- `decided_by`, the redrive record and `lease_owner` are verified names. ADR 010's "typed name" and "no login" become "operator key" and "no user accounts"
- Fencing (ADR 009) is unchanged. It still protects the row from a stale worker. Authentication adds that an outsider cannot pose as a worker
- Still not protected:
  - Anyone with a valid key can do what its role allows. A stolen key works until it is revoked, and keys do not expire or rotate by themselves
  - The engine does not provide HTTPS. Without it a key is visible on the network, so the API has to run behind a TLS connection (a proxy, for example). Local tests use plain HTTP
  - Anyone with database access can read and change everything and can create keys
  - Two people who share an operator key cannot be told apart in `decided_by`
  - No limit on failed attempts. Guessing a 32-byte random key is not practical, so this is a low risk
- Each request costs one indexed lookup in `api_keys`. The load test will show whether it matters
- New tests: every endpoint returns 401 without a key and with a revoked key; each role gets 403 on the other roles' endpoints; a client gets 404 for another client's job; a worker cannot report as another worker; `decided_by` equals the operator key's name, whatever the request body says
- The spec changes: the non-goal about authentication is replaced by a goal, sections 7 and 9 describe the key check, and the API contract will list the header and the two error codes
- A role change, a new role or per-job permissions would need this ADR revisited. There is no administrator role on purpose

## Revisit this decision if

- Operators need their own accounts, password reset, or sign-in through the company's identity provider (option D, which ends a non-goal)
- The engine serves separate organizations that must be kept apart (multi-tenancy)
- Rotating or expiring keys becomes a chore (add expiry times and a rotation command)
- A person has to approve jobs from a place where pasting a key is not acceptable
