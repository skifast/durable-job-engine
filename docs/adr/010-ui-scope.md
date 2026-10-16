# ADR 010: UI scope

**Status:** Accepted (2026-10-09)  
**Date:** 2026-10-06  
**Spec sections:** 7 (operator page), 6 (T3, T4, T14), 2 (non-goals)  
**Depends on:** ADR 002 (everything goes through the API), ADR 006 (approval), ADR 013 (operator API keys)  

## Context

Humans have to act on jobs in three places (these people are operators: people who run the system): approving or rejecting a job that waits, looking at a job that failed, and redriving a dead job. The API endpoints for all of these exist. The question is whether to build a screen on top of them, and how much of one.

## Options

### A. No UI: use the API directly (curl or a script)

**For**

- Least to build
- The approval gate is still real, since the endpoints do the work

**Against**

- A person has to find waiting jobs and read error history through raw JSON. Hard to demo and slow to use

### B. A read-only page: list jobs by state and show one job

**For**

- Shows the state of the system at a glance and is safe, since it changes nothing

**Against**

- Acting on a job still means going to the API

### C. An operator page: list, detail, approve, reject, redrive

**For**

- Covers everything a human has to do in one place
- Makes the approval step visible end to end, which is the point of having it
- Needs no new engine features, since it uses only the public API

**Against**

- More to build, and one more thing that can be wrong
- Anyone who holds an operator key can approve, so the page is only as safe as its keys (ADR 013)

## Decision

Use C. The operator page:

- lists jobs by state, with escalated approvals easy to find, and pages through them
- shows one job: state, attempts, lease, payload, result and the error history
- approves or rejects a waiting job (reject asks for a reason)
- lists dead jobs and redrives one (asks for the new max_attempts)
- shows the API's refusal message when an action is refused (already decided, wrong state, stale page), then reloads the job
- asks the operator for their API key, keeps it in the page's memory only, and sends it with each call. The API records the key's name as decided_by
- refreshes only when asked (a button, no live updates)
It talks only to the public API, never to Postgres. Out of scope: submitting jobs from the page, live updates and login with user accounts.

## Consequences

- The approval step is only as strong as the operator keys (ADR 013). decided_by is the name of the key that made the call, so it shows who acted when each person has their own key. Two people who share a key cannot be told apart. Keys are not stored in the browser, so a page reload asks for the key again.
- The page can show stale state. The API refuses a second or out-of-order action, so a stale click is harmless: the page shows the refusal and reloads the job.
- GET /jobs is paginated, so a 100k-row backlog never loads at once.
- Recovering from a long outage means redriving dead jobs one at a time. A bulk redrive is not included.
- The page needs the same tests as any client: approve, reject and redrive call the right endpoints, a refused action shows its message, and two operators acting on one job end with one success and one refusal.
- Choosing a framework and how the page is served is left to the build phase, since neither affects the engine.

## Revisit this decision if

- Operators need their own accounts, password reset, or sign-in through the company's identity provider (user accounts, which end a non-goal)
- Operators need to be told about escalations (add notification, ADR 006)
- Recovering from outages needs a bulk redrive
