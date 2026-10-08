# ADR 006: Approval placement and timeout

**Status:** Accepted (2026-10-07)  
**Date:** 2026-10-06  
**Spec sections:** 6 (T2, T3, T4, T5), 7 (reaper, operator page), 4 (approval invariants)  
**Depends on:** ADR 001 (Postgres), ADR 010 (operator page)  

## Context

Some job types need a person to say yes before they run. Two questions follow: where in the job's life the approval sits, and what happens when nobody answers in time.

The engine is deliberately not a workflow engine (spec non-goals: no multi-step workflows), so whatever approval design is chosen has to stay small. Waits may last hours or days, so a restart must not lose them.

## Options for where approval sits

### A. A gate before the handler

The job is saved as waiting_approval and can't be claimed. An approve call moves it to queued (T3). A reject call ends it as rejected (T4). Once running, it behaves like any other job.

**For**

- Small: one extra state and three transitions
- A waiting job holds no worker, no lease and no heartbeat, so a wait of days costs nothing
- Nothing has run yet, so a rejection has nothing to undo

**Against**

- Only handles "approve before it starts", not an approval partway through a handler

### B. Approval inside the handler (the handler pauses and waits)

**For**

- Allows an approval at any point in the work

**Against**

- The worker would have to hold a lease and send heartbeats for days, or the engine would have to save and resume the handler's progress. Saving and resuming is what a workflow engine does, and it is out of scope

### C. Approval as a separate job (job 1 asks for approval, job 2 does the work)

**For**

- No new state in the engine

**Against**

- The client has to connect the two jobs, which is a multi-step workflow pushed onto every client. A failed hand-off between them can lose the work

## Options for what happens when nobody answers by the deadline

- Reject automatically: the job ends as rejected. Safe, but the engine makes a human decision for them, and a slow approver silently kills work
- Approve automatically: defeats the purpose of an approval step
- Wait forever with no signal: nothing is lost, but nobody notices the job is stuck
- Escalate: the job stays waiting, and a flag (escalated_at) is set once so the job shows up in a list of escalated approvals. A person can still approve or reject afterwards
- Escalate and send a notification: the same, plus a webhook or message. Needs a notification system the project doesn't have

## Decision

Use A, and on timeout escalate without notifying. "Escalate" means only this: when approval_deadline passes, the reaper sets escalated_at once (T5), and the job is listed under "escalated approvals" at the top of the operator page (GET /jobs?state=waiting_approval&escalated=true). So the job is escalated to whoever is using the operator page. Nobody is told, and there are no roles, so any operator can approve or reject it. The job stays waiting_approval, and an approve or reject call still works afterwards. Nothing is ever rejected by a timeout; only an explicit reject call produces rejected.

## Consequences

- A job that nobody ever decides stays waiting forever. The "no job is lost" invariant says so: a waiting job only reaches a terminal state after a decision. A test run that checks every job ended must decide all approvals first.
- A late approval is allowed, which differs from a design where a decision after the deadline is refused. The only refused decision is a second one on the same job.
- Escalation is only as useful as someone looking at the list. There is no alert. This is a known limit, and notification is the natural next step.
- The approval deadline defaults to 24 hours after submit, and can be set differently for each job type.
- decided_by is a name typed on the operator page and not verified (ADR 010).
- The reaper escalates each job at most once, and running two reapers at once is safe because the update only applies when escalated_at is still empty.

## Revisit this decision if

- Approvals are needed in the middle of a job (B or a workflow engine)
- Escalated jobs sit unnoticed in practice (add notification)
- Waiting jobs pile up with no owner (add a hard stop that ends them, chosen by the business, not the engine)
